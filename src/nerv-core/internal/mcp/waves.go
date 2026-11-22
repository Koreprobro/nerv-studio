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
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"nerv-core/internal/gpu"
	"nerv-core/internal/job"
	"nerv-core/internal/scheduler"
)

const (
	ledgerKeep    = 32 // waves remembered for {wave: "..."} lookups
	submitQueue   = 16 // pending waves are cheap: admitted lines only
	waveTimeFmt   = "20060102-150405"
	waveIDPattern = "%s-%02d"

	// VRAM drain (the offload race of SPECS §8): child processes are reaped
	// when a wave ends, but drivers tear their contexts down asynchronously —
	// a chat reload fired right away can cudaMalloc into still-held VRAM and
	// OOM. After a GPU wave the runner waits for pool load to go quiet,
	// floored (teardown may not have started yet), polled and capped; a cap
	// miss proceeds best-effort, like every other XOR courtesy.
	vramFloor   = 2 * time.Second
	vramPoll    = time.Second
	vramCeil    = 45 * time.Second
	vramQuietMB = 512 // total MiB across queryable slots that counts as quiet
)

// jobLine is the live ledger snapshot of one queue line — enough for
// "is it done yet?" answers while the wave still runs.
type jobLine struct {
	Index  int    `json:"index"`
	Tool   string `json:"tool"`
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
	Dur    string `json:"dur,omitempty"`
}

// wave is one submitted queue: the admitted lines plus everything the
// ledger needs to answer about it later. The runner goroutine executes
// waves FIFO; every wave belongs to the daemon — a gone connection never
// kills it, cancellation is explicit (shutdown or POST /wave/stop,
// SPECS §6.6) and the result lands in the queue files' wave block (SPECS §6.1).
type wave struct {
	ID       string
	Toolbox  string      // execute-granted domain the wave was launched through
	Mount    string      // active playground domain at launch: the nest URL and fold target
	Mode     string      // "blocking" | "async"
	Tag      string      // "" = consumer wave; "raw" = fullRawAccess launch (SPECS §3.3)
	QueueRel string      // anchor queue file (playground-relative): the debug tail rides into its block
	Files    []queueFile // the whole queue set — each file gets its own wave block (sub-ledger)
	Started  time.Time
	Ended    time.Time

	adm        []*scheduler.Admitted
	resolved   map[int]map[string]string // variant picks per queue index, harvested at launch (SPECS §6.1)
	Skipped    []scheduler.Skipped
	byJob      map[*job.Job]int
	ctx        context.Context
	cancel     context.CancelFunc         // release the derived ctx when the run ends
	done       chan struct{}              // closed when the runner finishes the wave
	onProgress func(*job.Job, job.Status) // optional SSE fan-out (blocking)

	mu       sync.Mutex
	running  bool
	logRel   string // live debug log path (system temp), set while the log is open
	statuses map[int]jobLine
	res      *scheduler.Result
}

// newWave registers a submitted wave (status "queued" until the runner
// picks it up). The wave id is a wall-clock stamp plus a per-daemon counter:
// unique across restarts without a ledger on disk.
func (s *Server) newWave(mode, tag, toolbox string, files []queueFile, adm []*scheduler.Admitted, skipped []scheduler.Skipped,
	ctx context.Context, cancel context.CancelFunc) *wave {
	s.ledgerMu.Lock()
	defer s.ledgerMu.Unlock()
	s.waveSeq++
	queueRel := ""
	if len(files) > 0 {
		queueRel = files[0].File
	}
	w := &wave{
		ID:       fmt.Sprintf(waveIDPattern, time.Now().Format(waveTimeFmt), s.waveSeq),
		Mode:     mode,
		Tag:      tag,
		Toolbox:  toolbox,
		Mount:    s.Mount,
		QueueRel: queueRel,
		Files:    files,
		Started:  time.Now(),
		adm:      adm,
		Skipped:  skipped,
		byJob:    map[*job.Job]int{},
		statuses: map[int]jobLine{},
		ctx:      ctx,
		cancel:   cancel,
		done:     make(chan struct{}),
	}
	for _, a := range adm {
		w.byJob[a.Job()] = a.Index()
		w.statuses[a.Index()] = jobLine{Index: a.Index(), Tool: a.Tool(), Status: "queued"}
	}
	return w
}

// progress is the scheduler-facing callback: the ledger snapshot is always
// updated, the SSE fan-out (blocking waves) rides along. The onProgress hook
// is guarded by mu — the handler attaches it while the runner fires events.
func (w *wave) progress(j *job.Job, st job.Status) {
	idx := w.byJob[j]
	line := jobLine{Index: idx, Tool: j.Tool, Status: string(st), Error: j.Error, Dur: j.Dur.String()}
	w.mu.Lock()
	w.statuses[idx] = line
	hook := w.onProgress
	w.mu.Unlock()
	if hook != nil {
		hook(j, st)
	}
}

// setProgress attaches the SSE fan-out hook (blocking waves only).
func (w *wave) setProgress(hook func(*job.Job, job.Status)) {
	w.mu.Lock()
	w.onProgress = hook
	w.mu.Unlock()
}

// progressParams renders one notifications/progress payload (SPECS §6.6):
// machine-readable counters (done/total/wave) plus a human line that doubles
// as markdown for the GUI console — running lines name the job and its slot,
// finished lines link the artifact, failures carry the error. The hook runs
// on the runner goroutine after the ledger is updated, so the counters
// include the event that fired.
func (w *wave) progressParams(token any, j *job.Job, st job.Status) map[string]any {
	idx := w.byJob[j]
	total := len(w.adm)
	name := jobDisplay(j)
	w.mu.Lock()
	done := 0
	for _, ln := range w.statuses {
		switch job.Status(ln.Status) {
		case job.Done, job.Failed, job.Stuck:
			done++
		}
	}
	w.mu.Unlock()
	var line string
	switch st {
	case job.Running:
		slot := ""
		if len(j.Slots) > 0 {
			slot = " (" + j.Slots[0].String() + ")"
		}
		line = fmt.Sprintf("▶ %s%s %d/%d", name, slot, idx+1, total)
	case job.Done:
		if out, ok := j.Params["output"].(string); ok && out != "" {
			line = fmt.Sprintf("[%s](%s) ✓ %s", name, nestURL(w.Mount, out), j.Dur.String())
		} else {
			line = strings.TrimSpace(fmt.Sprintf("%s ✓ %s", name, j.Dur.String()))
		}
	case job.Pending:
		line = fmt.Sprintf("◦ %s %d/%d", name, idx+1, total)
	default: // failed, stuck
		line = strings.TrimSpace(fmt.Sprintf("✖ %s — %s %s", name, j.Error, j.Dur.String()))
	}
	return map[string]any{"progressToken": token, "message": line, "markdown": line,
		"done": done, "total": total, "wave": w.ID}
}

// view renders the ledger answer: live snapshots while running, the full
// wave result (with artifactURLs) once finished. With --debug a running
// wave also carries the `log` pointer — its transient temp-dir log
// (SPECS §6.6; the finished tail rides in the queue file's wave block).
func (w *wave) view(debug bool) map[string]any {
	w.mu.Lock()
	defer w.mu.Unlock()
	status := "queued"
	switch {
	case !w.Ended.IsZero():
		status = "finished"
	case w.running:
		status = "running"
	}
	v := map[string]any{"wave": w.ID, "mode": w.Mode, "status": status,
		"started": w.Started.Format(time.RFC3339)}
	if w.Tag != "" {
		v["tag"] = w.Tag
	}
	if !w.Ended.IsZero() {
		v["finished"] = w.Ended.Format(time.RFC3339)
		v["ok"] = w.res.OK()
		v["jobs"] = w.mapAnswer(*w.res).Jobs
	} else {
		lines := make([]jobLine, 0, len(w.adm))
		for _, a := range w.adm {
			lines = append(lines, w.statuses[a.Index()])
		}
		v["jobs"] = lines
	}
	if w.Skipped != nil {
		v["skipped"] = w.Skipped
	}
	if debug && w.logRel != "" {
		v["log"] = w.logRel
	}
	return v
}

// result returns the finished wave result (zero value while running).
func (w *wave) result() scheduler.Result {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.res == nil {
		return scheduler.Result{}
	}
	return *w.res
}

// submitWave enqueues a wave for the runner and registers it in the ledger.
func (s *Server) submitWave(w *wave) error {
	s.ledgerMu.Lock()
	defer s.ledgerMu.Unlock()
	select {
	case <-s.closed:
		return fmt.Errorf("daemon is shutting down: no new waves")
	default:
	}
	select {
	case s.submits <- w:
	default:
		return fmt.Errorf("wave queue full (%d pending): retry after a wave finishes", submitQueue)
	}
	s.ledger = append(s.ledger, w)
	if len(s.ledger) > ledgerKeep {
		s.ledger = s.ledger[len(s.ledger)-ledgerKeep:]
	}
	return nil
}

// runWaves is the daemon-wide wave pipeline: one wave at a time, FIFO —
// the serialization dispatchMu used to provide, now by construction.
func (s *Server) runWaves() {
	defer close(s.runnerDone)
	for w := range s.submits {
		s.runWaveJob(w)
	}
}

// runWaveJob executes one wave to completion (runner goroutine only).
func (s *Server) runWaveJob(w *wave) {
	defer w.cancel()
	w.mu.Lock()
	w.running = true
	w.mu.Unlock()
	// VRAM XOR: wash the chat model out before any wave with GPU jobs
	// (best-effort — a dead provider never blocks the wave, SPECS §8).
	if s.LLM != nil && scheduler.WantsGPU(w.adm) {
		_, _ = s.LLM.Unload(w.ctx)
	}
	// The debug wave log runs in the system temp dir, out of the playground
	// (SPECS §6.6): the playground only ever sees the folded tail inside the
	// queue file's wave block — no stray log files to archive or clean up.
	var opts scheduler.RunOpts
	if s.Cfg.Debug {
		opts.WaveID = w.ID
		opts.LogPath = filepath.Join(os.TempDir(), "nerv-wave-"+w.ID+".log")
		w.mu.Lock()
		w.logRel = opts.LogPath
		w.mu.Unlock()
	}
	res := s.Scheds[w.Toolbox].RunAdmitted(w.ctx, w.adm, opts, w.progress)
	// VRAM XOR close: processes are reaped but their contexts die async —
	// drain the pool load before close(w.done) releases the chat to reload
	// (same gate as the pre-wave wash; best-effort, bounded). A canceled
	// wave (red button) drains on a fresh context: whoever stopped the wave
	// still deserves a quiet pool to reload into; daemon shutdown does not.
	if s.LLM != nil && scheduler.WantsGPU(w.adm) {
		dctx := w.ctx
		if w.ctx.Err() != nil {
			select {
			case <-s.closed: // daemon going down: nobody waits for the courtesy
			default:
				dctx = context.Background()
			}
		}
		vramDrain(dctx, s.Pool, gpu.QueryLoad, vramFloor, vramPoll, vramCeil, vramQuietMB)
	}
	w.mu.Lock()
	w.res = &res
	w.Ended = time.Now()
	w.running = false
	w.mu.Unlock()
	// Debug fold (SPECS §6.6): the log's tail rides into the queue file's
	// wave block and the temp file disappears. Then the result stamp itself
	// (SPECS §6.1): every queue file of the set learns its outcome before
	// the wave is announced done.
	var debug, debugNote string
	if s.Cfg.Debug && opts.LogPath != "" {
		if tail, total, err := tailFile(opts.LogPath, waveDebugCap); err == nil {
			debug = tail
			if total > waveDebugCap {
				debugNote = fmt.Sprintf("tail %d KB of %d KB", waveDebugCap>>10, total>>10)
			}
		}
		_ = os.Remove(opts.LogPath)
		w.mu.Lock()
		w.logRel = ""
		w.mu.Unlock()
	}
	s.writeWaveBlocks(w, debug, debugNote)
	close(w.done)
}

// vramDrain waits until the summed load of all queryable pool slots falls to
// quietMB or the ceiling passes — the driver's teardown race answered with
// patience instead of prayer (SPECS §8). Slots the loader cannot read
// (remote, other backends) are not our concern; if none are queryable the
// floor alone applies. A canceled ctx (killed wave, gone caller) leaves
// immediately: nobody waits on this wave's courtesy anymore.
func vramDrain(ctx context.Context, pool *gpu.Pool, load func(gpu.Slot) (uint64, error),
	floor, poll, ceil time.Duration, quietMB uint64) {
	if !sleepCtx(ctx, floor) {
		return
	}
	if pool == nil {
		return
	}
	deadline := time.Now().Add(ceil)
	for {
		var used, slots uint64
		for _, sl := range pool.Slots() {
			mb, err := load(sl)
			if err != nil {
				continue
			}
			used += mb
			slots++
		}
		if slots == 0 || used <= quietMB || !time.Now().Before(deadline) {
			return
		}
		if !sleepCtx(ctx, poll) {
			return
		}
	}
}

// sleepCtx sleeps d unless ctx ends first; it reports whether the full
// duration elapsed (false = ctx is done, callers should unwind).
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// findWave resolves a {wave: ...} lookup: an exact id, or "latest" — the
// most recent wave. Raw-tagged waves hide from the consumer's default
// "latest" (SPECS §3.3); fullRawAccess sees them.
func (s *Server) findWave(arg string, raw bool) *wave {
	s.ledgerMu.Lock()
	defer s.ledgerMu.Unlock()
	for i := len(s.ledger) - 1; i >= 0; i-- {
		w := s.ledger[i]
		if arg == "latest" {
			if raw || w.Tag == "" {
				return w
			}
			continue
		}
		if w.ID == arg {
			return w
		}
	}
	return nil
}

// Shutdown drains the wave pipeline within grace (SPECS §6.6): no
// new submits, queued and running waves finish on their own; stragglers get
// the wave ctx canceled — the scheduler SIGKILLs their process groups.
func (s *Server) Shutdown(grace time.Duration) {
	s.ledgerMu.Lock()
	select {
	case <-s.closed:
	default:
		close(s.closed)
		close(s.submits)
	}
	s.ledgerMu.Unlock()
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-s.runnerDone:
	case <-timer.C:
		s.waveCancel()
		<-s.runnerDone
	}
}
