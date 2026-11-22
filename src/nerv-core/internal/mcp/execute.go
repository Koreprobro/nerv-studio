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
	"path"
	"strings"
	"sync"
	"time"

	"nerv-core/internal/job"
	"nerv-core/internal/scheduler"
)

// sseKeepalive caps the silence of a blocking SSE wave stream: long
// single-job phases emit no status flips, so a ": keepalive" comment line
// goes out on this interval to keep proxies from reaping the connection.
const sseKeepalive = 30 * time.Second

// executeAnswer is the wave reply: scheduler result + artifactURLs + the
// wave identity on top.
type executeAnswer struct {
	Jobs    []answeredJob       `json:"jobs"`
	Skipped []scheduler.Skipped `json:"skipped,omitempty"`
}

type answeredJob struct {
	Index       int               `json:"index"`
	Tool        string            `json:"tool"`
	Outputs     []string          `json:"outputs,omitempty"`
	Archived    []string          `json:"archived,omitempty"` // old same-name files: name-<i>.ext
	ArtifactURL []string          `json:"artifactURL,omitempty"`
	Status      string            `json:"status"`
	Error       string            `json:"error,omitempty"`
	Dur         string            `json:"dur,omitempty"`
	Slots       []string          `json:"slots,omitempty"`
	Resolved    map[string]string `json:"resolved,omitempty"` // variant picks that fired (SPECS §6.1)
}

// callExecute is the wave launch point and the wave ledger lookup (SPECS §6.1),
// reached through an "execute"-granted domain ({command:"execute"}, SPECS §3.7).
// Launch input is a queue file — or a comma-separated set of queue files
// merged into one wave — in the playground: each a JSON array of {tool,
// params} lines or the {"defaults"?, "jobs", "wave"?} envelope (SPECS §6.1);
// the finished wave is stamped back into its files as a "wave" block, so a
// queue file is a self-describing ticket. Mode follows the config: with
// llm_offload on, a queue with any gpu-require > 0 line runs blocking (one
// SSE stream for the wave's life; notifications/progress ride it when the
// client carries a progressToken). Everything
// else runs async — the answer returns immediately with the wave
// id. Either way the wave belongs to the daemon: a gone client never kills
// it (SPECS §6.6). {"wave": "latest"|<id>} looks a wave up in the ledger.
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
	queue, files, err := s.readQueueSet(args)
	if err != nil {
		return toolError("%v", err), false
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
		tag = "raw" // raw launches stay out of the consumer default-latest (SPECS §3.3)
	}

	// The wave ctx: every wave belongs to the daemon (SPECS §6.6) — a gone
	// connection never kills a running wave. Cancellation is explicit:
	// daemon shutdown or POST /wave/stop.
	wctx, cancel := context.WithCancel(s.waveCtx)
	wv := s.newWave(mode, tag, toolbox, files, adm, skipped, wctx, cancel)
	wv.resolved = resolvedOf(queue) // the fired picks ride the stamp (SPECS §6.1)

	// Blocking wave: the answer rides one SSE stream (SPECS §6.6). Headers
	// and a ": wave" comment flush before the submit, so a client or proxy
	// never sits on a byteless POST — a silent minutes-long wait is exactly
	// what read timeouts reap mid-render. A progressToken additionally gets
	// notifications/progress on the stream; the final response always
	// closes it. Plain JSON remains the fallback for a transport that
	// cannot flush. The progress hook attaches before the submit: the
	// runner may flip the first job to running before this handler is back.
	token, hasToken := meta["progressToken"]
	flusher, canStream := w.(http.Flusher)
	stream := blocking && canStream
	var writeMu sync.Mutex
	send := func(payload any) {
		note, _ := json.Marshal(payload)
		writeMu.Lock()
		defer writeMu.Unlock()
		fmt.Fprintf(w, "event: message\ndata: %s\n\n", note)
		flusher.Flush()
	}
	if stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		writeMu.Lock()
		fmt.Fprintf(w, ": wave %s\n\n", wv.ID)
		flusher.Flush()
		writeMu.Unlock()
		if hasToken {
			wv.setProgress(func(j *job.Job, st job.Status) {
				params := wv.progressParams(token, j, st)
				if line, ok := params["message"].(string); ok {
					line = s.redact(line) // a failed job quotes tool stderr: no raw roots (SPECS §3.6)
					params["message"], params["markdown"] = line, line
				}
				send(map[string]any{"jsonrpc": "2.0", "method": "notifications/progress",
					"params": params})
			})
		}
	}
	if err := s.submitWave(wv); err != nil {
		cancel()
		if stream { // headers are already out: answer the launch error on the stream
			send(rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: s.guardVolume(s.redactResult(toolError("%v", err)), args)})
			return ToolResult{}, true
		}
		return toolError("%v", err), false
	}
	if !blocking {
		return jsonResult(wv.view(s.Cfg.Debug)), false
	}
	if !stream {
		<-wv.done
		return s.waveResult(wv), false
	}
	// Keepalive: long single-job phases keep the stream silent — an SSE
	// comment every sseKeepalive keeps proxies and client read timeouts off
	// a live wave's stream, token or no token.
	kaDone := make(chan struct{})
	defer close(kaDone)
	go func() {
		tick := time.NewTicker(sseKeepalive)
		defer tick.Stop()
		for {
			select {
			case <-tick.C:
				writeMu.Lock()
				fmt.Fprint(w, ": keepalive\n\n")
				flusher.Flush()
				writeMu.Unlock()
			case <-kaDone:
				return
			}
		}
	}()
	<-wv.done
	// The wave outlives this connection by design: writes to a dead
	// client simply fail, the wave stands and its result is stamped
	// into the queue files (SPECS §6.1, §6.6).
	res := s.guardVolume(s.redactResult(s.waveResult(wv)), args)
	if strings.Contains(resText(res), "[volume guard") {
		// The cap tripped on a finished wave: re-sending the execute would
		// reshoot it, so point the full dump at the ledger lookup instead.
		res.Content[0].Text += " [the wave is over: execute wave=\"" + wv.ID +
			"\" re-answers it; volume_ack=true lifts the cap]"
	}
	send(rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: res})
	return ToolResult{}, true // response already streamed
}

// queueFile reports one parsed member of a queue set (the check answer,
// SPECS §6.1).
type queueFile struct {
	File string `json:"file"`
	Jobs int    `json:"jobs"`
}

// checkAnswer is the dry-run report of {command: "check"}: what the queue
// set holds and what admission would make of it — nothing launched.
type checkAnswer struct {
	Files    []queueFile         `json:"files"`
	Jobs     int                 `json:"jobs"`
	Admitted int                 `json:"admitted"`
	Skipped  []scheduler.Skipped `json:"skipped,omitempty"`
}

// callCheck is the dry run of the queue paradigm (SPECS §6.1): the same
// reader and the same admission as execute, but nothing launches — no wave,
// no ledger entry, nothing moved on disk. Parse failures answer isError and
// name the file; otherwise the report counts jobs per file and lists the
// lines admission would skip (unknown tool, missing output, unfit GPU ask).
func (s *Server) callCheck(toolbox string, args map[string]any) ToolResult {
	queue, files, err := s.readQueueSet(args)
	if err != nil {
		return toolError("%v", err)
	}
	sched := s.Scheds[toolbox]
	if sched == nil {
		return toolError("%s has no scheduler (not an execute-granted domain)", toolbox)
	}
	adm, skipped := sched.Admit(queue)
	rep := checkAnswer{Files: files, Jobs: len(queue), Admitted: len(adm), Skipped: skipped}
	res := jsonResult(rep)
	if len(adm) == 0 {
		res.IsError = true // execute would refuse this launch
	}
	return res
}

// readQueueSet resolves and reads the launch input: the `queue` argument
// names one queue file under the playground (mount) root — or a
// comma-separated set of them, merged in list order into one wave
// (SPECS §6.1). Any file that fails to resolve, parse or hold jobs
// rejects the whole launch before the wave starts; the wave and its debug
// fold anchor at the first file.
func (s *Server) readQueueSet(args map[string]any) (queue []job.Job, files []queueFile, err error) {
	p := asString(args, "queue")
	if p == "" {
		p = asString(args, "path")
	}
	if p == "" {
		if _, isArray := args["queue"].([]any); isArray {
			return nil, nil, fmt.Errorf(`execute wants a queue FILE path (SPECS §6.1): {"queue": "<project>/queue.json"} — the inline job array is retired`)
		}
		return nil, nil, fmt.Errorf(`execute needs {"queue": "<path>[, <path>...]"} (queue files in the playground) or {"wave": "latest"|<id>}`)
	}
	for _, part := range strings.Split(p, ",") {
		if part = strings.TrimSpace(part); part == "" {
			continue
		}
		rel := job.NormalizePath(part, s.Mount)
		if err := job.WithinRel(rel); err != nil {
			return nil, nil, fmt.Errorf("queue file %s: %v", part, err)
		}
		raw, err := os.ReadFile(s.Store.Abs(rel))
		if err != nil {
			return nil, nil, fmt.Errorf("queue file %s: %s", rel, s.redact(err.Error()))
		}
		qs, err := parseQueueFile(raw)
		if err != nil {
			return nil, nil, fmt.Errorf("queue file %s: %v", rel, err)
		}
		if len(qs) == 0 {
			return nil, nil, fmt.Errorf("queue file %s is empty", rel)
		}
		files = append(files, queueFile{File: rel, Jobs: len(qs)})
		queue = append(queue, qs...)
	}
	if len(files) == 0 {
		return nil, nil, fmt.Errorf(`execute needs a non-empty {"queue": "<path>[, <path>...]"}`)
	}
	// Dynamic-prompt variant lists roll their picks before admission: the
	// backend never sees the sigil (SPECS §6.1).
	for i := range queue {
		queue[i].ResolveVariants()
	}
	return queue, files, nil
}

// waveResult builds the blocking answer: the final ledger view, isError
// when any line ended not-done.
func (s *Server) waveResult(wv *wave) ToolResult {
	res := wv.result()
	return ToolResult{Content: []ToolContent{textBlock(prettyJSON(wv.view(s.Cfg.Debug)))},
		IsError: !res.OK()}
}

// parseQueueFile reads one queue file (SPECS §6.1): a bare JSON array of
// {tool, params} lines, or the envelope {"defaults": {...}, "jobs": [...],
// "wave": {...}}. A line carrying a top-level "defaults" key is a scoped
// directive: it governs the lines that follow it within this file, later
// declarations layering over earlier ones (expandLines). The daemon-written
// "wave" block is ignored on read — resubmitting a finished queue reshoots
// the same recipe.
func parseQueueFile(raw []byte) ([]job.Job, error) {
	if firstByte(raw) == "[" {
		var lines []json.RawMessage
		if err := json.Unmarshal(raw, &lines); err != nil {
			return nil, err
		}
		return expandLines(lines, job.Job{})
	}
	var env struct {
		Defaults *job.Job          `json:"defaults"`
		Jobs     []json.RawMessage `json:"jobs"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf(`want a JSON array or the {"defaults"?, "jobs", "wave"?} envelope (SPECS §6.1): %v`, err)
	}
	if len(env.Jobs) == 0 {
		return nil, fmt.Errorf(`envelope needs a non-empty "jobs" array`)
	}
	active := job.Job{}
	if env.Defaults != nil {
		active = *env.Defaults
	}
	return expandLines(env.Jobs, active)
}

// expandLines walks the raw lines of one file's jobs array behind an active
// defaults template. A line with a top-level "defaults" key is a scoped
// directive (SPECS §6.1): it layers over the active template — the later
// declaration wins per key, silence keeps the earlier value — and governs
// the lines that follow, within this file only (a multi-file set keeps each
// file's scope to itself). A job line merges under the active template; the
// line wins per key. Directives are not jobs and never reach the ledger.
func expandLines(lines []json.RawMessage, active job.Job) ([]job.Job, error) {
	var jobs []job.Job
	for i, raw := range lines {
		var probe struct {
			Defaults *job.Job `json:"defaults"`
		}
		if err := json.Unmarshal(raw, &probe); err != nil {
			return nil, fmt.Errorf("line %d: %v", i, err)
		}
		if probe.Defaults != nil {
			probe.Defaults.MergeDefaults(active)
			active = *probe.Defaults
			continue
		}
		var j job.Job
		if err := json.Unmarshal(raw, &j); err != nil {
			return nil, fmt.Errorf("line %d: %v", i, err)
		}
		j.MergeDefaults(active)
		jobs = append(jobs, j)
	}
	return jobs, nil
}

// jobDisplay names a job for the human progress lines: the stem of its
// output file (what the client named and will look for), the tool as the
// fallback for tools without an output param.
func jobDisplay(j *job.Job) string {
	if s, ok := j.Params["output"].(string); ok && s != "" {
		b := path.Base(s)
		return strings.TrimSuffix(b, path.Ext(b))
	}
	return j.Tool
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

// mapAnswer builds the wave ledger view of a result: artifactURLs of the
// active playground ride on top of the scheduler's lines (SPECS §3.1).
func (w *wave) mapAnswer(res scheduler.Result) executeAnswer {
	out := executeAnswer{}
	for _, j := range res.Jobs {
		a := answeredJob{Index: j.Index, Tool: j.Tool, Outputs: j.Outputs, Archived: j.Archived,
			Status: j.Status, Error: j.Error, Dur: j.Dur, Slots: j.Slots}
		a.Resolved = w.resolved[j.Index] // the fired pick, if the line held a variant list
		for _, o := range j.Outputs {
			a.ArtifactURL = append(a.ArtifactURL, nestURL(w.Mount, o))
		}
		out.Jobs = append(out.Jobs, a)
	}
	out.Skipped = res.Skipped
	if out.Jobs == nil {
		out.Jobs = []answeredJob{}
	}
	return out
}

// resolvedOf harvests the variant picks of a parsed queue by position:
// the wave's job numbering follows the queue order, skips included
// (SPECS §6.1).
func resolvedOf(queue []job.Job) map[int]map[string]string {
	var m map[int]map[string]string
	for i := range queue {
		if len(queue[i].Resolved) > 0 {
			if m == nil {
				m = map[int]map[string]string{}
			}
			m[i] = queue[i].Resolved
		}
	}
	return m
}

func prettyJSON(v any) string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}
