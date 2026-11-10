// SPDX-License-Identifier: AGPL-3.0-or-later
//
// nerv-core — omni-channel AI studio core: a thin MCP orchestrator of
// system-installed generation tools, a GPU scheduler and an artifact server.
// Copyright (C) 2026 Koreprobro
//
// This program is free software: you can redistribute it and/or modify it
// under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful, but
// WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program. If not, see <https://www.gnu.org/licenses/>.

package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sync"

	"nerv-core/internal/job"
	"nerv-core/internal/scheduler"
)

// executeAnswer is the wave reply: scheduler result + artifactURLs + the
// wave identity on top.
type executeAnswer struct {
	Jobs    []answeredJob       `json:"jobs"`
	Skipped []scheduler.Skipped `json:"skipped,omitempty"`
}

type answeredJob struct {
	Index       int      `json:"index"`
	Tool        string   `json:"tool"`
	Outputs     []string `json:"outputs,omitempty"`
	Archived    []string `json:"archived,omitempty"` // old same-name files: name-<i>.ext
	ArtifactURL []string `json:"artifactURL,omitempty"`
	Status      string   `json:"status"`
	Error       string   `json:"error,omitempty"`
	Dur         string   `json:"dur,omitempty"`
	Slots       []string `json:"slots,omitempty"`
}

// callExecute is the wave launch point and the wave ledger lookup (§2.3),
// reached through an "execute"-granted domain ({command:"execute"}, §3.7).
// Launch input is the path to a queue file in the playground — a bare JSON
// array of {tool, params} lines the daemon reads but never mutates. Mode
// follows the config: with llm_offload on, a queue with any gpu-require > 0
// line runs blocking (SSE progress when the client carries a progressToken,
// plain JSON otherwise); everything else runs async — the wave belongs to
// the daemon and the answer returns immediately with the wave id.
// {"wave": "latest"|<id>} looks a wave up in the ledger.
func (s *Server) callExecute(toolbox string, w http.ResponseWriter, r *http.Request, req rpcRequest, args, meta map[string]any) (ToolResult, bool) {
	if id := asString(args, "wave"); id != "" {
		wv := s.findWave(id, optBool(args, "fullRawAccess"))
		if wv == nil {
			return toolError("wave %q not found (the ledger keeps the last %d waves)", id, ledgerKeep), false
		}
		return jsonResult(wv.view(s.Cfg.Debug)), false
	}

	sched := s.Scheds[toolbox]
	if sched == nil {
		return toolError("%s has no scheduler (not an execute-granted domain)", toolbox), false
	}
	raw, path, err := s.readQueueFile(args)
	if err != nil {
		return toolError("%v", err), false
	}
	if first := firstByte(raw); first != "" && first != "[" {
		return toolError(`queue file %s: want a bare JSON array [{"tool": ...}] — the {"queue": ...} envelope is dead (REBUILD §4.9)`, path), false
	}
	var queue []job.Job
	if err := json.Unmarshal(raw, &queue); err != nil {
		return toolError("queue file %s: %v", path, err), false
	}
	if len(queue) == 0 {
		return toolError("queue file %s is empty", path), false
	}

	adm, skipped := sched.Admit(queue)
	if len(adm) == 0 {
		// Nothing survived admission: no wave runs, the skip list IS the answer.
		return ToolResult{
			Content: []ToolContent{textBlock(prettyJSON(executeAnswer{Jobs: []answeredJob{}, Skipped: skipped}))},
			IsError: true,
		}, false
	}

	blocking := s.Cfg.LLMOffload && scheduler.WantsGPU(adm)
	mode := "async"
	if blocking {
		mode = "blocking"
	}
	tag := ""
	if optBool(args, "fullRawAccess") {
		tag = "raw" // raw launches stay out of the consumer default-latest (§2.2)
	}

	// The wave ctx: async waves belong to the daemon (server ctx only),
	// blocking ones also die with their connection (§2.3).
	var (
		wctx   context.Context
		cancel context.CancelFunc
	)
	if blocking {
		wctx, cancel = joinCtx(r.Context(), s.waveCtx)
	} else {
		wctx, cancel = context.WithCancel(s.waveCtx)
	}
	wv := s.newWave(mode, tag, toolbox, path, adm, skipped, wctx, cancel)
	if err := s.submitWave(wv); err != nil {
		cancel()
		return toolError("%v", err), false
	}
	if !blocking {
		return jsonResult(wv.view(s.Cfg.Debug)), false
	}

	// Blocking wave: SSE when the client carries a progressToken (hours of
	// generation on one open stream), plain JSON otherwise. The wave runs
	// on the pipeline goroutine; this handler only waits and writes.
	token, hasToken := meta["progressToken"]
	flusher, canStream := w.(http.Flusher)
	if hasToken && canStream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()
		var writeMu sync.Mutex
		wv.setProgress(func(j *job.Job, st job.Status) {
			note, _ := json.Marshal(map[string]any{
				"jsonrpc": "2.0", "method": "notifications/progress",
				"params": map[string]any{"progressToken": token,
					"message": fmt.Sprintf("%s %s", j.Tool, st)},
			})
			writeMu.Lock()
			defer writeMu.Unlock()
			fmt.Fprintf(w, "event: message\ndata: %s\n\n", note)
			flusher.Flush()
		})
		<-wv.done
		final, _ := json.Marshal(rpcResponse{JSONRPC: "2.0", ID: req.ID,
			Result: s.waveResult(wv)})
		writeMu.Lock()
		defer writeMu.Unlock()
		fmt.Fprintf(w, "event: message\ndata: %s\n\n", final)
		flusher.Flush()
		return ToolResult{}, true // response already streamed
	}
	<-wv.done
	return s.waveResult(wv), false
}

// readQueueFile resolves and reads the queue file: the `queue` argument
// names a JSON file under the playground (mount) root.
func (s *Server) readQueueFile(args map[string]any) (raw []byte, rel string, err error) {
	p := asString(args, "queue")
	if p == "" {
		p = asString(args, "path")
	}
	if p == "" {
		if _, isArray := args["queue"].([]any); isArray {
			return nil, "", fmt.Errorf(`execute wants a queue FILE path (REBUILD §2.3): {"queue": "<project>/queue.json"} — the inline job array is retired`)
		}
		return nil, "", fmt.Errorf(`execute needs {"queue": "<path>"} (queue file in the playground) or {"wave": "latest"|<id>}`)
	}
	rel = job.NormalizePath(p)
	if err := job.WithinRel(rel); err != nil {
		return nil, "", fmt.Errorf("queue file: %v", err)
	}
	raw, err = os.ReadFile(s.Store.Abs(rel))
	if err != nil {
		return nil, "", fmt.Errorf("queue file: %s", s.redact(err.Error()))
	}
	return raw, rel, nil
}

// waveResult builds the blocking answer: the final ledger view, isError
// when any line ended not-done.
func (s *Server) waveResult(wv *wave) ToolResult {
	res := wv.result()
	return ToolResult{Content: []ToolContent{textBlock(prettyJSON(wv.view(s.Cfg.Debug)))},
		IsError: !res.OK()}
}

// firstByte reports the first non-space byte of the queue file as a string
// ("" for empty content) — the envelope detector.
func firstByte(raw []byte) string {
	for _, b := range raw {
		switch b {
		case ' ', '\t', '\r', '\n':
			continue
		}
		return string(b)
	}
	return ""
}

func mapAnswer(res scheduler.Result) executeAnswer {
	out := executeAnswer{}
	for _, j := range res.Jobs {
		a := answeredJob{Index: j.Index, Tool: j.Tool, Outputs: j.Outputs, Archived: j.Archived,
			Status: j.Status, Error: j.Error, Dur: j.Dur, Slots: j.Slots}
		for _, o := range j.Outputs {
			a.ArtifactURL = append(a.ArtifactURL, artifactURL(o))
		}
		out.Jobs = append(out.Jobs, a)
	}
	out.Skipped = res.Skipped
	if out.Jobs == nil {
		out.Jobs = []answeredJob{}
	}
	return out
}

func prettyJSON(v any) string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}
