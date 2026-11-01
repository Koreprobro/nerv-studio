package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"

	"nerv-core/internal/artifactory"
	"nerv-core/internal/config"
	"nerv-core/internal/gpu"
	"nerv-core/internal/job"
	"nerv-core/internal/llm"
	"nerv-core/internal/scheduler"
)

// ProtocolVersion is the MCP transport version served (2026-07-28).
const ProtocolVersion = "2026-07-28"

// Tool is a registered MCP tool. Streaming tools implement Stream; others Call.
type Tool struct {
	Name        string
	Description string
	InputSchema map[string]any
	// Hidden marks an internal tool that must not be advertised in tools/list,
	// so the consumer-AI discovers only the tools it should orchestrate. The
	// tool stays registered and callable by name (GUI / CLI / debugging).
	Hidden bool
	// Call carries the request context so a tool can see the human-mode marker
	// (human.go) — visibility decisions are per request, never per tool.
	Call func(ctx context.Context, args map[string]any) (any, error)
	// Stream is a long-running tool that answers with an SSE stream (queue.submit).
	// progressToken is the client's tools/call _meta.progressToken (echoed in
	// progress notifications, M7.6); it is nil when the client sent none.
	Stream func(ctx context.Context, w io.Writer, id *jsonRPCID, args map[string]any, progressToken any) error
}

// wave is one submitted DAG plus its live job state. Async waves are
// server-owned and run to completion even if the submitting client disconnects;
// blocking waves are connection-scoped and are canceled when the client drops
// (D1). The wave's cancel func + done channel let the server cancel all
// unfinished waves at shutdown.
type wave struct {
	id     int
	sched  *scheduler.Scheduler
	cancel context.CancelFunc // cancels the wave's dispatch (set on submit)
	done   chan struct{}      // closed when the dispatch goroutine returns
	// human marks a wave submitted in human-mode (GUI). Human waves are
	// invisible to the consumer-AI's job.list/job.status (SPECS.md §4.4) —
	// the creative sandbox never sees operator runs.
	human bool
}

// Server is the MCP streamable-HTTP surface.
type Server struct {
	config   *config.Config
	artifact *artifactory.Artifactory
	gpus     []gpu.GPU
	llm      *llm.Client
	logger   *log.Logger
	tools    map[string]*Tool

	mu    sync.Mutex
	waves []*wave
	seq   int

	// dispatchMu serializes wave execution daemon-wide (M3): GPU exclusivity in
	// the scheduler is per-wave, so without it two concurrent queue.submit calls
	// could run generators on the same physical GPU. Held for the entire
	// Dispatch of a wave, so waves run one after another (FIFO).
	dispatchMu sync.Mutex
}

// New builds the server and registers the core scheduler/LLM tools.
func New(cfg *config.Config, gpus []gpu.GPU, ll *llm.Client, logger *log.Logger) *Server {
	s := &Server{
		config:   cfg,
		artifact: artifactory.New(cfg.ArtifactoryDir),
		gpus:     gpus,
		llm:      ll,
		logger:   logger,
		tools:    map[string]*Tool{},
	}
	s.registerCoreTools()
	registerBrowseTool(s)
	registerDiceTool(s)
	return s
}

func (s *Server) schedulerFor() *scheduler.Scheduler {
	return scheduler.New(s.config.ToolsDir, s.config.LoraModelDir, s.config.KBDir, artifactory.New(s.config.ArtifactoryDir), s.gpus)
}

func (s *Server) addWave(sched *scheduler.Scheduler, cancel context.CancelFunc, human bool) *wave {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	w := &wave{id: s.seq, sched: sched, cancel: cancel, done: make(chan struct{}), human: human}
	s.waves = append(s.waves, w)
	// Bound wave retention: drop the oldest FINISHED waves beyond the cap
	// (unbounded history leaks schedulers + job state forever — audit M6).
	// Running waves are never dropped: their status stays queryable.
	const maxRetainedWaves = 64
	if over := len(s.waves) - maxRetainedWaves; over > 0 {
		kept := make([]*wave, 0, len(s.waves))
		for _, old := range s.waves {
			if over > 0 && waveDone(old) {
				over--
				continue
			}
			kept = append(kept, old)
		}
		s.waves = kept
	}
	return w
}

// waveDone reports whether a wave's dispatch goroutine has returned.
func waveDone(w *wave) bool {
	select {
	case <-w.done:
		return true
	default:
		return false
	}
}

// Shutdown cancels every unfinished wave (the scheduler's cancel hook SIGKILLs
// running tool process groups, D1) and waits for the dispatch goroutines to
// return, bounded by ctx. Called from main on daemon shutdown (audit M2).
func (s *Server) Shutdown(ctx context.Context) {
	s.mu.Lock()
	var pending []*wave
	for _, w := range s.waves {
		if w.cancel != nil {
			w.cancel()
		}
		select {
		case <-w.done:
		default:
			pending = append(pending, w)
		}
	}
	s.mu.Unlock()
	for _, w := range pending {
		select {
		case <-w.done:
		case <-ctx.Done():
			return
		}
	}
}

// latestWave returns the newest wave visible to the caller. Human-mode waves
// are invisible to non-human callers (SPECS.md §4.4): the consumer-AI's
// "latest" is the latest wave it is allowed to see.
func (s *Server) latestWave(human bool) *wave {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.waves) - 1; i >= 0; i-- {
		if w := s.waves[i]; human || !w.human {
			return w
		}
	}
	return nil
}

// ServeHTTP handles both surfaces on the same port: GET /artifacts/<rel> serves
// immutable artifact bytes for the GUI, while POST carries the streamable MCP
// JSON-RPC traffic. The two never overlap (method + path).
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Human-mode marker (human.go): the token is transport-only; the MCP body
	// is untouched, so the consumer-AI's view of the protocol never changes.
	r = r.WithContext(withHuman(r.Context(), s.isHumanRequest(r)))
	if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/artifacts/") {
		s.serveArtifact(w, r)
		return
	}
	// The RPC surface is served on POST /mcp only; no path answers the JSON-RPC
	// dispatch (M7.6 — the old handler answered RPC on any path).
	if r.URL.Path != "/mcp" {
		s.writeError(w, nil, InvalidRequest, "not found: "+r.URL.Path+" (RPC is served on POST /mcp)")
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 16<<20))
	if err != nil {
		s.writeError(w, nil, ParseError, "read body: "+err.Error())
		return
	}
	// JSON-RPC batching is explicitly unsupported (M7.6) — answer a clear
	// Invalid Request instead of a confusing parse error or silent first-item.
	if trimmed := strings.TrimLeft(string(body), " \t\r\n"); strings.HasPrefix(trimmed, "[") {
		s.writeError(w, nil, InvalidRequest, "batch requests are not supported (send one JSON-RPC object per request)")
		return
	}
	var req Request
	if err := json.Unmarshal(body, &req); err != nil {
		s.writeError(w, nil, ParseError, "parse error: "+err.Error())
		return
	}
	if err := s.validateHeaders(r, &req); err != nil {
		// A JSON-RPC error object (Streamable HTTP header-mismatch code -32020)
		// instead of a plain-text 400 — clients parse the body as JSON-RPC
		// (audit M6). The 400 status is kept.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(Result{
			JSONRPC: jsonRPCVersion, ID: req.ID,
			Error: &RPCError{Code: HeaderMismatch, Message: err.Error()},
		})
		return
	}
	if req.IsNotification() {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	s.dispatch(w, r, &req)
}

func (s *Server) dispatch(w http.ResponseWriter, r *http.Request, req *Request) {
	switch req.Method {
	case "initialize":
		s.writeResult(w, req.ID, map[string]any{
			"protocolVersion": s.negotiateVersion(r, req),
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "nerv-core", "version": "0.1.0"},
		})
	case "tools/list":
		s.handleToolsList(w, req)
	case "tools/call":
		s.handleToolsCall(w, r, req)
	case "ping":
		s.writeResult(w, req.ID, map[string]any{})
	case "subscriptions/listen":
		s.handleSubscriptionsListen(w, req)
	default:
		s.writeError(w, req.ID, MethodNotFound, "method not found: "+req.Method)
	}
}

// effectiveProtocolVersion resolves the protocol version a request speaks, from
// the body (request metadata / initialize params) or the OPTIONAL
// MCP-Protocol-Version header — whichever is present. A client may convey the
// version in only one of the two (e.g. Cline sends it only in the header).
func effectiveProtocolVersion(r *http.Request, req *Request) string {
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
func (s *Server) negotiateVersion(r *http.Request, req *Request) string {
	if v := effectiveProtocolVersion(r, req); v != "" && v <= ProtocolVersion {
		if s.logger != nil {
			s.logger.Printf("initialize: negotiated protocol version %q (client requested)", v)
		}
		return v
	}
	return ProtocolVersion
}

// bodyProtocolVersion extracts the MCP protocol version from request metadata
// (_meta) or from initialize params, whichever is present.
func bodyProtocolVersion(req *Request) string {
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

// validateHeaders enforces the Streamable HTTP request-metadata contract
// (2026-07-28). All three request-metadata headers are OPTIONAL: clients may
// omit any of them and convey the values in the body. When a header IS present
// it MUST mirror the corresponding body field, else 400 + HeaderMismatch.
func (s *Server) validateHeaders(r *http.Request, req *Request) error {
	hv := r.Header.Get("MCP-Protocol-Version")
	bv := bodyProtocolVersion(req)
	// The protocol version may be conveyed in EITHER the header or the body
	// (request metadata); a mismatch is reported only when BOTH are present and
	// differ. A client that sends the version in only one of the two is valid
	// (e.g. Cline sends it only in the header, body="").
	if hv != "" && bv != "" && hv != bv {
		return fmt.Errorf("HeaderMismatch: MCP-Protocol-Version header=%q body=%q", hv, bv)
	}
	hm := r.Header.Get("Mcp-Method")
	if hm != "" && hm != req.Method {
		return fmt.Errorf("HeaderMismatch: Mcp-Method header=%q body=%q", hm, req.Method)
	}
	if req.Method == "tools/call" {
		var p map[string]any
		_ = json.Unmarshal(req.Params, &p)
		hn := r.Header.Get("Mcp-Name")
		name, _ := p["name"].(string)
		if hn != "" && hn != name {
			return fmt.Errorf("HeaderMismatch: Mcp-Name header=%q body=%q", hn, name)
		}
	}
	return nil
}

func (s *Server) handleToolsList(w http.ResponseWriter, req *Request) {
	// Deterministic order (M7.5): sorted by name, so tools/list is diffable
	// and cache-stable across daemon restarts.
	names := make([]string, 0, len(s.tools))
	for n := range s.tools {
		names = append(names, n)
	}
	sort.Strings(names)
	tools := make([]map[string]any, 0, len(s.tools))
	for _, n := range names {
		t := s.tools[n]
		if t.Hidden {
			continue // internal tool: not advertised to the consumer-AI
		}
		tools = append(tools, map[string]any{
			"name":        t.Name,
			"description": t.Description,
			"inputSchema": t.InputSchema,
		})
	}
	s.writeResult(w, req.ID, map[string]any{"tools": tools})
}

func (s *Server) handleSubscriptionsListen(w http.ResponseWriter, req *Request) {
	var params struct {
		Notifications struct {
			ToolsListChanged bool `json:"toolsListChanged"`
		} `json:"notifications"`
	}
	_ = json.Unmarshal(req.Params, &params)
	// Acknowledge with the subscription id == the request id.
	s.writeResult(w, req.ID, map[string]any{
		"_meta": map[string]any{
			"io.modelcontextprotocol/subscriptionId": req.ID,
		},
		"notifications": map[string]any{
			"toolsListChanged": params.Notifications.ToolsListChanged,
		},
	})
}

func (s *Server) writeResult(w http.ResponseWriter, id *jsonRPCID, result any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(Result{JSONRPC: jsonRPCVersion, ID: id, Result: result})
}

func (s *Server) writeError(w http.ResponseWriter, id *jsonRPCID, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(Result{JSONRPC: jsonRPCVersion, ID: id, Error: &RPCError{Code: code, Message: msg}})
}

// anyToString marshals a tool result value into a text content string.
func anyToString(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}

// hasGPUJob reports whether the queue has at least one gpu-count > 0 job.
func hasGPUJob(submit job.Submit) bool {
	for _, j := range submit.Queue {
		if j.GPUCount > 0 {
			return true
		}
	}
	return false
}

// handleToolsCall dispatches a tools/call. Streaming tools answer with an SSE
// stream; others with a single JSON response. A tool's own (domain) failure is
// returned as an MCP tool error — isError content, HTTP 200 — not a JSON-RPC
// protocol error (M7.2): the protocol exchange succeeded, the task failed. The
// message is path-redacted for the client; the full detail goes to the log.
func (s *Server) handleToolsCall(w http.ResponseWriter, r *http.Request, req *Request) {
	var params struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		s.writeError(w, req.ID, InvalidParams, "invalid params: "+err.Error())
		return
	}
	t, ok := s.tools[params.Name]
	if !ok {
		s.writeToolError(w, req.ID, "unknown tool: "+params.Name)
		return
	}
	if t.Stream != nil {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)
		// FlushWriter pushes every event through http.Flusher so the client (and
		// its disconnect) is seen in real time, not when the handler returns.
		sw := FlushWriter{w}
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush() // send headers immediately
		}
		if err := t.Stream(r.Context(), sw, req.ID, params.Arguments, progressTokenOf(req)); err != nil {
			_ = NewSSEWriter(sw).Event(errorResult(req.ID, s.redact(err.Error())))
			_ = NewSSEWriter(sw).Flush()
		}
		return
	}
	result, err := t.Call(r.Context(), params.Arguments)
	if err != nil {
		if s.logger != nil {
			s.logger.Printf("tools/call %s: %v", params.Name, err)
		}
		s.writeToolError(w, req.ID, err.Error())
		return
	}
	// A tool may return a ready-made ToolResult (multi-block content, e.g. an
	// image/video/audio block from the get command); honor it verbatim. Otherwise
	// wrap the plain value as a single text block. The volume guard (D4) caps
	// the TEXT surface; media blocks carry their own byte budget.
	tr, over := s.guardVolume(toolResultOf(result), params.Arguments)
	if over {
		tr.IsError = true
	}
	s.writeResult(w, req.ID, tr)
}

// writeToolError answers a tools/call with an MCP tool error (isError content
// block), never a JSON-RPC protocol error (M7.2). The message is redacted of
// server-absolute paths before it reaches the client (M7.3).
func (s *Server) writeToolError(w http.ResponseWriter, id *jsonRPCID, msg string) {
	s.writeResult(w, id, ToolResult{
		Content: []ToolContent{textContent(s.redact(msg))},
		IsError: true,
	})
}

// redact removes server-absolute filesystem paths from client-visible strings
// (M7.3): the configured roots are replaced by their root names, so a blunder
// like path=tools under the tools root leaks a root-relative path, not the
// host layout. Full detail stays in the daemon log.
func (s *Server) redact(msg string) string {
	for _, r := range [][2]string{
		{s.config.ArtifactoryDir, "artifactory/"},
		{s.config.ToolsDir, "tools/"},
		{s.config.KBDir, "kb/"},
		{s.config.LoraModelDir, "loras/"},
	} {
		if r[0] == "" {
			continue
		}
		msg = strings.ReplaceAll(msg, r[0]+string(os.PathSeparator), r[1])
		msg = strings.ReplaceAll(msg, r[0], r[1])
	}
	return msg
}

// guardVolume enforces the volume guard (D4): a result whose TEXT exceeds
// MaxOutputChars is refused with an actionable warning that reveals the
// escape hatch (volume_ack=true) — a full dump must be a deliberate two-step
// act, never an accident. Media blocks are EXEMPT: get of an image never
// trips the guard; their bound is the byte budget (max_block_bytes).
func (s *Server) guardVolume(res ToolResult, args map[string]any) (ToolResult, bool) {
	if ack, _ := args["volume_ack"].(bool); ack {
		return res, false
	}
	limit := s.config.MaxOutputChars
	if limit <= 0 {
		return res, false
	}
	n := 0
	for _, c := range res.Content {
		if c.Type == "text" {
			n += len(c.Text)
		}
	}
	if n <= limit {
		return res, false
	}
	warn := fmt.Sprintf(
		"volume guard: output is %d characters, over the %d limit. Narrow the request: ls a deeper path, probe instead of get, page text with lines=\"A-B\" or bytes=\"A-B\", or lower max_dim/max_block_bytes. If you truly need the whole output, resend the same request with volume_ack=true.",
		n, limit)
	return ToolResult{Content: []ToolContent{textContent(warn)}, IsError: true}, true
}

// toolResultOf normalizes a tool result into a ToolResult content envelope.
func toolResultOf(v any) ToolResult {
	if tr, ok := v.(ToolResult); ok {
		return tr
	}
	return ToolResult{Content: []ToolContent{textContent(anyToString(v))}}
}

// progressTokenOf extracts the client's progress token from a tools/call:
// params._meta.progressToken (MCP spec), falling back to the request-level
// _meta. Returned to callers as-is (string or number; nil when absent).
func progressTokenOf(req *Request) any {
	var p struct {
		Meta struct {
			ProgressToken any `json:"progressToken"`
		} `json:"_meta"`
	}
	if len(req.Params) > 0 && json.Unmarshal(req.Params, &p) == nil && p.Meta.ProgressToken != nil {
		return p.Meta.ProgressToken
	}
	if req.Meta != nil {
		return req.Meta["progressToken"]
	}
	return nil
}
