// SPDX-License-Identifier: AGPL-3.0-or-later
//
// nerv-core — omni-channel AI studio core: a thin MCP orchestrator of
// system-installed generation tools, a GPU scheduler and an artifact server.
// Copyright (C) 2026 Koreprobro
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program. If not, see <https://www.gnu.org/licenses/>.

package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"

	"nerv-core/internal/browse"
	"nerv-core/internal/config"
	"nerv-core/internal/gpu"
	"nerv-core/internal/llm"
	"nerv-core/internal/scheduler"
	"nerv-core/internal/store"
)

// ProtocolVersion is the highest MCP revision the daemon speaks. initialize
// negotiates: real clients (e.g. Cline) only know published MCP dates and
// reject a future one, so the server answers with the client's version when
// it is at or below this one, and with this one otherwise.
const ProtocolVersion = "2026-07-28"

// Server is the MCP surface over the studio wiring. Storage is config
// driven (SPECS §3.7): one browse.Domain per configured directory, the
// unique "mount" domain is the playground (Store, /artifacts, lineage),
// every "execute"-granted domain owns a scheduler over its toolbox root.
type Server struct {
	Cfg        *config.Config
	Pool       *gpu.Pool
	Scheds     map[string]*scheduler.Scheduler // toolbox domain -> its scheduler
	Store      store.Store                     // the mount playground store
	Mount      string                          // playground domain name
	LLM        *llm.Client                     // nil when llm_offload is false
	domains    map[string]*browse.Domain
	redactions []rootTag // server roots -> <name> tags, longest root first
	// Wave pipeline (M15): the runner goroutine executes waves FIFO —
	// daemon-wide one wave at a time (the serialization dispatchMu used
	// to provide). The ledger remembers recent waves for {wave: ...}
	// lookups; waveCtx owns everything a client may not kill.
	submits    chan *wave
	waveCtx    context.Context
	waveCancel context.CancelFunc
	closed     chan struct{} // closed on Shutdown: no new submits
	runnerDone chan struct{} // closed when the runner goroutine exits
	ledgerMu   sync.Mutex
	ledger     []*wave
	waveSeq    int // monotonic id counter (never trimmed, unlike the ring)
}

// New wires the surface: one browse.Domain per configured storage root,
// budgets from the config media section, hints and allow-lists from it too.
// The schedulers map is keyed by toolbox domain name (cfg.Toolboxes()).
func New(cfg *config.Config, pool *gpu.Pool, scheds map[string]*scheduler.Scheduler) *Server {
	b := browse.Budget{
		MaxBytes: cfg.Media.MaxBytes, MaxBlockBytes: cfg.Media.MaxBlockBytes,
		MaxDim: cfg.Media.MaxDim, VideoMaxDim: cfg.Media.VideoMaxDim,
		Quality: cfg.Media.Quality, Format: cfg.Media.Format,
		Rate: cfg.Media.Rate, Channels: cfg.Media.Channels,
		Bitrate: cfg.Media.Bitrate, MaxDuration: cfg.Media.MaxDuration,
	}
	s := &Server{Cfg: cfg, Pool: pool, Scheds: scheds, domains: map[string]*browse.Domain{}}
	if cfg.LLMOffload {
		s.LLM = llm.New(cfg.AIURL)
	}
	if name, d := cfg.Mount(); name != "" {
		s.Mount = name
		s.Store = store.Store{Root: d.Path}
	}
	for _, name := range cfg.DomainNames() {
		s.domains[name] = &browse.Domain{Name: name, Root: cfg.Domains[name].Path, Budget: b}
	}
	s.redactions = buildRedactions(cfg)
	s.waveCtx, s.waveCancel = context.WithCancel(context.Background())
	s.submits = make(chan *wave, submitQueue)
	s.closed = make(chan struct{})
	s.runnerDone = make(chan struct{})
	go s.runWaves()
	return s
}

// Handler is the mounted surface: POST /mcp (streamable JSON-RPC) and the
// /artifacts/ read mount backing artifactURL links (the playground root).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", s.handleRPC)
	mount := "/artifacts/"
	mux.Handle(mount, http.StripPrefix(mount,
		http.FileServer(http.Dir(s.Store.Root))))
	return mux
}

func (s *Server) handleRPC(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, nil, codeInvalid, "POST only")
		return
	}
	var req rpcRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, nil, codeParse, "parse error: "+err.Error())
		return
	}
	if req.isNotification() {
		switch req.Method {
		case "notifications/initialized", "initialized", "notifications/cancelled":
			w.WriteHeader(http.StatusAccepted)
		default:
			writeError(w, nil, codeNoMethod, "unknown notification: "+req.Method)
		}
		return
	}
	switch req.Method {
	case "initialize":
		writeResult(w, req.ID, map[string]any{
			"protocolVersion": s.negotiateVersion(r, &req),
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": "nerv-core", "version": "0.2.0"},
		})
	case "ping":
		writeResult(w, req.ID, map[string]any{})
	case "tools/list":
		writeResult(w, req.ID, map[string]any{"tools": s.toolsList()})
	case "tools/call":
		s.handleCall(w, r, req)
	default:
		writeError(w, req.ID, codeNoMethod, "method not found: "+req.Method)
	}
}

// effectiveProtocolVersion resolves the protocol version a request speaks, from
// the body (request metadata / initialize params) or the OPTIONAL
// MCP-Protocol-Version header — whichever is present. A client may convey the
// version in only one of the two (e.g. Cline sends it only in the header).
func effectiveProtocolVersion(r *http.Request, req *rpcRequest) string {
	if bv := bodyProtocolVersion(req); bv != "" {
		return bv
	}
	return r.Header.Get("MCP-Protocol-Version")
}

// negotiateVersion applies MCP protocol-version negotiation. The server answers
// with a version both support: it echoes the client's requested version (read
// from the body or, if absent, the header) when that is at or below the server's
// own ProtocolVersion, otherwise it falls back to its own. A missing client
// version defaults to the server's ProtocolVersion.
//
// This is what lets real MCP clients (e.g. Cline), which only know the published
// MCP dates (2024-11-05, 2025-06-18, ...) and not the project's 2026-07-28,
// connect: the server answers with the version the client actually speaks.
func (s *Server) negotiateVersion(r *http.Request, req *rpcRequest) string {
	if v := effectiveProtocolVersion(r, req); v != "" && v <= ProtocolVersion {
		return v
	}
	return ProtocolVersion
}

// bodyProtocolVersion extracts the MCP protocol version from request metadata
// (_meta) or from initialize params, whichever is present.
func bodyProtocolVersion(req *rpcRequest) string {
	if req.Meta != nil {
		if v, ok := req.Meta["io.modelcontextprotocol/protocolVersion"].(string); ok {
			return v
		}
	}
	if len(req.Params) > 0 {
		var p map[string]any
		if err := json.Unmarshal(req.Params, &p); err == nil {
			if v, ok := p["protocolVersion"].(string); ok {
				return v
			}
		}
	}
	return ""
}

// toolHandler answers one tools/call. It returns the result plus whether
// the response was already streamed to w (SSE execute), in which case the
// caller must not write a JSON body.
type toolHandler func(w http.ResponseWriter, r *http.Request, req rpcRequest, args, meta map[string]any) (ToolResult, bool)

func (s *Server) handleCall(w http.ResponseWriter, r *http.Request, req rpcRequest) {
	var p struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
		Meta      map[string]any `json:"_meta"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil {
		writeError(w, req.ID, codeNoParams, "bad tools/call params")
		return
	}
	if p.Arguments == nil {
		p.Arguments = map[string]any{}
	}
	tool, err := s.lookup(p.Name)
	if err != nil {
		writeError(w, req.ID, codeNoParams, s.redact(err.Error()))
		return
	}
	res, streamed := tool(w, r, req, p.Arguments, p.Meta)
	if streamed {
		return
	}
	res = s.guardVolume(res, p.Arguments)
	writeResult(w, req.ID, res)
}

// lookup resolves the tool name: every configured domain is a tool whose
// allow-list defines its commands (SPECS §3.7); the utilities are the fixed
// remainder. The hidden gpu.list is reachable but never listed (§2.2).
func (s *Server) lookup(name string) (toolHandler, error) {
	if _, ok := s.domains[name]; ok {
		return s.domainHandler(name), nil
	}
	switch name {
	case "dice.roll":
		return func(w http.ResponseWriter, _ *http.Request, _ rpcRequest, a, _ map[string]any) (ToolResult, bool) {
			return s.callDice(a), false
		}, nil
	case "calc.resolution":
		return func(w http.ResponseWriter, _ *http.Request, _ rpcRequest, a, _ map[string]any) (ToolResult, bool) {
			return s.callCalcResolution(a), false
		}, nil
	case "gpu.list":
		return func(w http.ResponseWriter, _ *http.Request, _ rpcRequest, a, _ map[string]any) (ToolResult, bool) {
			return s.callGPUList(a), false
		}, nil
	}
	return nil, &unknownTool{name}
}

// domainHandler routes one domain tool call: the execute grant turns
// {command:"execute"} into the wave launch over this domain's toolbox,
// everything else runs over the shared browse mechanism.
func (s *Server) domainHandler(name string) toolHandler {
	return func(w http.ResponseWriter, r *http.Request, req rpcRequest, args, meta map[string]any) (ToolResult, bool) {
		if hasAny(s.Cfg.Domains[name].Commands, "execute") && asString(args, "command") == "execute" {
			return s.callExecute(name, w, r, req, args, meta)
		}
		return s.callDomain(name, args), false
	}
}

type unknownTool struct{ name string }

func (e *unknownTool) Error() string { return "unknown tool: " + e.name }
