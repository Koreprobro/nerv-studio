package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"nerv-core/internal/gpu"
	"nerv-core/internal/job"
	"nerv-core/internal/scheduler"
)

// queueSubmitHint is the concise manual injected into the `queue.submit` tool's
// description (the MCP "hint"): it documents the file-dataflow job model so
// the consumer-AI submits a correct queue without reading SPECS.md.
const queueSubmitHint = `Submit a queue of jobs.
Each job:
- tool     tool id: the script path as browse lists it (e.g. image/anima/anima-base.sh; the .sh suffix may be omitted). Discover its fields with: browse root=tools command=probe path=<tool>.
- params   tool parameters (object), two conventions:
  * output (required): the file the job writes, relative to the artifactory root, WITH extension (e.g. scene/portrait.png). You choose the name; the tool's field description states the file type it produces. An existing file with that name is archived by the tool (.1, .2, ... suffixes) and yours is written under the exact requested name.
  * fields marked "file": true in the tool's field list are input files (one path or an array of paths, artifactory-root-relative). A job starts only when all its input files exist — they may be produced by earlier jobs of this same queue: data flows through files, there are no dependency edges to declare.
A job still waiting for input files when nothing is left running fails, naming the paths that never appeared.
GPU placement is automatic (a client-set gpu-count is ignored).
Waves execute strictly one at a time (FIFO): a newly submitted queue starts only after all previously submitted waves have finished.
Every job result carries output (a path usable with browse) and artifactURL (full HTTP link, artifactURL = server + "/artifacts/" + output).
Render a produced artifact in chat as a markdown image: ![<alt>](<artifactURL>).`

// registerCoreTools wires the scheduler/LLM MCP tools (SPECS.md §4).
func (s *Server) registerCoreTools() {
	s.tools["gpu.list"] = &Tool{
		Name:        "gpu.list",
		Description: "List the GPUs visible to the scheduler pool (frozen at startup).",
		Hidden:      true, // internal: not advertised in tools/list (the consumer-AI does not manage GPU placement)
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
		Call: func(ctx context.Context, args map[string]any) (any, error) {
			out := make([]map[string]any, 0, len(s.gpus))
			for _, g := range s.gpus {
				load, _ := gpu.QueryLoad(g.Index)
				out = append(out, map[string]any{
					"index":         g.Index,
					"memoryTotalMB": g.MemTotalMB,
					"memoryUsedMB":  load,
					"name":          g.Name,
				})
			}
			return map[string]any{"gpus": out}, nil
		},
	}
	s.tools["queue.submit"] = &Tool{
		Name:        "queue.submit",
		Description: queueSubmitHint,
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"queue": queueSchema()},
			"required":   []string{"queue"},
		},
		Stream: s.streamQueueSubmit,
	}
	s.tools["job.list"] = &Tool{
		Name:        "job.list",
		Description: "List the jobs of a wave (default: the latest wave).",
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"waveId": intSchema(), "status": statusSchema()},
		},
		Call: s.handleJobList,
	}
	s.tools["job.status"] = &Tool{
		Name:        "job.status",
		Description: "Get the status of a job by output file path or index (default wave: latest).",
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"waveId": intSchema(), "output": stringSchema(), "index": intSchema()},
		},
		Call: s.handleJobStatus,
	}
	// The directory surface (artifactory/tools/kb/loras) is the single `browse`
	// tool; see browse.go and SPECS.md §4.1.
}

func queueSchema() map[string]any {
	return map[string]any{
		"type":        "array",
		"items":       jobSchema(),
		"description": "array of jobs; each has tool and params{output, ...}. Inputs flow as files: fields the tool marks \"file\": true name existing-or-to-be-produced files, and the scheduler starts a job once all of them exist. GPU placement is automatic; a client-set gpu-count is ignored",
	}
}

func jobSchema() map[string]any {
	return map[string]any{
		"type":       "object",
		"properties": jobProps(),
		"required":   []string{"tool", "params"},
	}
}

func jobProps() map[string]any {
	return map[string]any{
		"tool": stringSchema(),
		"params": map[string]any{
			"type":        "object",
			"description": "tool params (see browse root=tools command=probe path=<tool>.sh); params.output = the output file, relative to the artifactory root WITH extension; fields marked \"file\": true are input file paths",
			"required":    []string{"output"},
		},
	}
}

func stringSchema() map[string]any { return map[string]any{"type": "string"} }
func intSchema() map[string]any    { return map[string]any{"type": "integer"} }
func boolSchema() map[string]any   { return map[string]any{"type": "boolean"} }
func arraySchema() map[string]any  { return map[string]any{"type": "array"} }
func statusSchema() map[string]any {
	return map[string]any{"type": "string", "enum": []string{"pending", "running", "done", "failed"}}
}

// artifactGetProps is the get command's input schema. Every media type has a
// default compression bound; these let the caller loosen or tighten it. Text
// supports range paging (lines/bytes, D4); media supports a byte budget
// (max_block_bytes) with automatic degrade.
func artifactGetProps() map[string]any {
	return map[string]any{
		"max_dim":         map[string]any{"type": "integer", "description": "image/video longest side in px (default 1600 img / 800 video)"},
		"quality":         map[string]any{"type": "number", "description": "JPEG quality 1-100 for images (default 82)"},
		"format":          stringSchema(),
		"max_bytes":       map[string]any{"type": "integer", "description": "text preview cap in bytes (default 16384)"},
		"lines":           map[string]any{"type": "string", "description": `text line range "A-B" (1-based inclusive; A or B may be empty, e.g. "80-" from line 80); mutually exclusive with bytes`},
		"bytes":           map[string]any{"type": "string", "description": `text byte range "A-B" (1-based inclusive; A or B may be empty); mutually exclusive with lines`},
		"max_block_bytes": map[string]any{"type": "integer", "description": "media block byte budget after encoding (default 524288); over budget the renderer degrades quality, then dimensions"},
		"seek":            stringSchema(),
		"rate":            map[string]any{"type": "integer", "description": "audio sample rate (default 16000)"},
		"channels":        map[string]any{"type": "integer", "description": "audio channels (default 1)"},
		"bitrate":         stringSchema(),
		"max_duration":    map[string]any{"type": "integer", "description": "audio/video preview length in seconds (default 30)"},
	}
}

// streamQueueSubmit is the blocking/async queue.submit handler (SPECS.md §3.4).
// progressToken (from the client's tools/call _meta) is echoed in every
// progress notification so any MCP client can correlate them (M7.6).
func (s *Server) streamQueueSubmit(ctx context.Context, w io.Writer, id *jsonRPCID, args map[string]any, progressToken any) error {
	var submit job.Submit
	if raw, ok := args["queue"]; ok {
		var queue []job.Job
		b, err := json.Marshal(raw)
		if err != nil {
			return fmt.Errorf("invalid queue: %w", err)
		}
		if err := json.Unmarshal(b, &queue); err != nil {
			return fmt.Errorf("invalid queue: %w", err)
		}
		// gpu-count is scheduler-owned (resolved from --tool-info): a
		// client-supplied value is ignored, not honored (M7.5 schema honesty).
		for i := range queue {
			queue[i].GPUCount = 0
		}
		submit.Queue = queue
	}
	sched := s.schedulerFor()
	if err := sched.Build(submit); err != nil {
		return err
	}
	blocking := s.config.LLMOffload && hasGPUJob(submit)
	// Wave context (D1): a blocking wave is bound to the client connection —
	// disconnect cancels it and the scheduler SIGKILLs the running tool process
	// groups. An async wave runs to completion in the background and is canceled
	// only by server shutdown (the wave stores its cancel func).
	base := context.Background()
	if blocking {
		base = ctx
	}
	waveCtx, cancel := context.WithCancel(base)
	wave := s.addWave(sched, cancel, HumanFrom(ctx))
	if blocking {
		defer cancel()
	}

	// VRAM XOR (SPECS.md §3.3): unload the LLM via the provider API before a
	// GPU wave. Best-effort — a missing endpoint must not stop the wave.
	if blocking && s.llm != nil {
		if err := s.llm.Unload(ctx); err != nil {
			s.logger.Printf("offload: %v", err)
		}
	}

	if blocking {
		sse := NewSSEWriter(w)
		// progressUpdate carries an immutable status snapshot so the select
		// loop never reads the scheduler's mutable job.Status.
		type progressUpdate struct {
			job    *job.Job
			status job.Status
		}
		progressCh := make(chan progressUpdate, 256)
		go func() {
			defer close(wave.done)
			defer close(progressCh)
			s.dispatchMu.Lock() // one wave at a time (M3)
			defer s.dispatchMu.Unlock()
			sched.Dispatch(waveCtx, func(j *job.Job, status job.Status) {
				progressCh <- progressUpdate{j, status}
			})
		}()
		keepalive := time.NewTicker(15 * time.Second)
		defer keepalive.Stop()
	loop:
		for {
			select {
			case upd, ok := <-progressCh:
				if !ok {
					return sse.Event(s.resultEventGuarded(id, s.waveResult(wave, sched), args))
				}
				if err := sse.Event(progressEvent(id, progressToken, upd.job, upd.status)); err != nil {
					// write failure == client gone (D1)
					break loop
				}
			case <-ctx.Done():
				// Client dropped the SSE: waveCtx (child of ctx) is canceled, the
				// scheduler kills the running tool process groups, and abortPending
				// fails the rest. Drain progressCh so the dispatch goroutine is
				// never blocked on a full channel (audit M2 — the old code leaked
				// and stalled the wave here).
				break loop
			case <-keepalive.C:
				if err := sse.Keepalive(); err != nil {
					break loop // failed keepalive == dropped connection
				}
			}
		}
		// shared exit for disconnect paths: ensure cancellation + no blocked sender
		cancel()
		go func() {
			for range progressCh {
			}
		}()
		return nil
	}

	// Async mode (default): run in the background, return the wave handle now.
	go func() {
		defer close(wave.done)
		s.dispatchMu.Lock() // one wave at a time (M3)
		defer s.dispatchMu.Unlock()
		sched.Dispatch(waveCtx, nil)
	}()
	return NewSSEWriter(w).Event(s.resultEventGuarded(id, s.waveResult(wave, sched), args))
}

// waveResult serializes a wave's job statuses. It reads an immutable snapshot
// so it never races with a dispatch in progress (blocking or async mode).
func (s *Server) waveResult(w *wave, sched *scheduler.Scheduler) map[string]any {
	snap := sched.JobsSnapshot()
	jobs := make([]map[string]any, 0, len(snap))
	for _, j := range snap {
		jobs = append(jobs, map[string]any{
			"tool":        j.Tool,
			"output":      j.Output,
			"artifactURL": s.artifactURL(j.Output),
			"status":      string(j.Status),
			"error":       s.redact(j.Error),
		})
	}
	return map[string]any{"waveId": w.id, "jobs": jobs}
}

func (s *Server) handleJobList(ctx context.Context, args map[string]any) (any, error) {
	sched, err := s.resolveWave(ctx, args)
	if err != nil {
		return nil, err
	}
	if sched == nil {
		return map[string]any{"jobs": []any{}}, nil
	}
	filter := ""
	if v, ok := args["status"].(string); ok {
		filter = v
	}
	jobs := make([]map[string]any, 0)
	for _, j := range sched.JobsSnapshot() {
		if filter != "" && string(j.Status) != filter {
			continue
		}
		jobs = append(jobs, s.jobStatusMap(j))
	}
	return map[string]any{"jobs": jobs}, nil
}

func (s *Server) handleJobStatus(ctx context.Context, args map[string]any) (any, error) {
	sched, err := s.resolveWave(ctx, args)
	if err != nil {
		return nil, err
	}
	if sched == nil {
		return map[string]any{"found": false}, nil
	}
	if out, ok := args["output"].(string); ok {
		out = job.NormalizePath(out) // symmetric with the submit-side normalization
		for _, j := range sched.JobsSnapshot() {
			if out == j.Output {
				return s.jobStatusMap(j), nil
			}
		}
	}
	if idx, ok := args["index"].(float64); ok {
		for i, j := range sched.JobsSnapshot() {
			if float64(i) == idx {
				return s.jobStatusMap(j), nil
			}
		}
	}
	return map[string]any{"found": false}, nil
}

func (s *Server) jobStatusMap(j scheduler.JobSnapshot) map[string]any {
	return map[string]any{
		"tool": j.Tool, "output": j.Output,
		"artifactURL": s.artifactURL(j.Output),
		"status":      string(j.Status), "error": s.redact(j.Error),
	}
}

// resolveWave returns the scheduler for the requested wave. An explicit
// waveId that does not exist — or names a HUMAN wave for a non-human caller
// (SPECS.md §4.4; the answer is the identical "unknown" error, so hidden waves
// are not even enumerable) — is an error: it must NOT silently fall back to
// the latest wave, which made the client query one wave and read another
// (M7.4). With no waveId given it returns the latest visible wave, or
// (nil, nil) when no visible wave has run yet.
func (s *Server) resolveWave(ctx context.Context, args map[string]any) (*scheduler.Scheduler, error) {
	human := HumanFrom(ctx)
	if _, given := args["waveId"]; given {
		w := s.pickWave(args, human)
		if w == nil {
			return nil, fmt.Errorf("unknown waveId: %v (no such wave in this session; omit waveId for the latest)", args["waveId"])
		}
		return w.sched, nil
	}
	if w := s.latestWave(human); w != nil {
		return w.sched, nil
	}
	return nil, nil
}

func (s *Server) pickWave(args map[string]any, human bool) *wave {
	wid, ok := args["waveId"].(float64)
	if !ok {
		return nil
	}
	target := int(wid)
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, w := range s.waves {
		if w.id == target && (human || !w.human) {
			return w
		}
	}
	return nil
}

// --- SSE event builders for queue.submit ---

// resultEventGuarded emits the wave's final result event under the volume
// guard (D4): a pathologically large wave report is refused with the volume
// warning instead of dumped into the client.
func (s *Server) resultEventGuarded(id *jsonRPCID, result any, args map[string]any) map[string]any {
	tr, over := s.guardVolume(toolResultOf(result), args)
	if over {
		return errorResult(id, tr.Content[0].Text)
	}
	return resultEvent(id, result)
}

func progressEvent(id *jsonRPCID, progressToken any, j *job.Job, status job.Status) map[string]any {
	params := map[string]any{
		"progressId": waveProgressID(id),
		"status":     string(status),
		"tool":       j.Tool,
		"output":     j.Output(),
	}
	// Echo the client's tools/call _meta.progressToken (MCP progress contract,
	// M7.6) so notifications correlate with the request in any MCP client.
	if progressToken != nil {
		params["progressToken"] = progressToken
	}
	return map[string]any{
		"jsonrpc": "2.0",
		"method":  "notifications/progress",
		"params":  params,
	}
}

func resultEvent(id *jsonRPCID, result any) map[string]any {
	return map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"result":  ToolResult{Content: []ToolContent{{Type: "text", Text: anyToString(result)}}},
	}
}

func errorResult(id *jsonRPCID, msg string) map[string]any {
	return map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"result":  ToolResult{Content: []ToolContent{{Type: "text", Text: msg}}, IsError: true},
	}
}

func waveProgressID(id *jsonRPCID) string {
	b, _ := json.Marshal(id)
	return "wave-" + strings.ReplaceAll(string(b), `"`, "")
}
