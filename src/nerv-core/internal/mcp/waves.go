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
	"path"
	"strings"
	"sync"
	"time"

	"nerv-core/internal/job"
	"nerv-core/internal/scheduler"
)

const (
	ledgerKeep    = 32 // waves remembered for {wave: "..."} lookups
	submitQueue   = 16 // pending waves are cheap: admitted lines only
	waveTimeFmt   = "20060102-150405"
	waveIDPattern = "%s-%02d"
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
// waves FIFO; an async wave belongs to the daemon (a disconnect cannot
// kill it), a blocking one belongs to its connection (§2.3).
type wave struct {
	ID       string
	Toolbox  string // execute-granted domain the wave was launched through
	Mode     string // "blocking" | "async"
	Tag      string // "" = consumer wave; "raw" = fullRawAccess launch (§2.2)
	QueueRel string // playground-relative queue file — the wave log lands beside it
	Started  time.Time
	Ended    time.Time

	adm        []*scheduler.Admitted
	Skipped    []scheduler.Skipped
	byJob      map[*job.Job]int
	ctx        context.Context
	cancel     context.CancelFunc         // release the derived ctx when the run ends
	done       chan struct{}              // closed when the runner finishes the wave
	onProgress func(*job.Job, job.Status) // optional SSE fan-out (blocking)

	mu       sync.Mutex
	running  bool
	logRel   string // final log path (playground-relative), set when the log opens
	statuses map[int]jobLine
	res      *scheduler.Result
}

// newWave registers a submitted wave (status "queued" until the runner
// picks it up). The wave id is a wall-clock stamp plus a per-daemon counter:
// unique across restarts without a ledger on disk.
func (s *Server) newWave(mode, tag, toolbox, queueRel string, adm []*scheduler.Admitted, skipped []scheduler.Skipped,
	ctx context.Context, cancel context.CancelFunc) *wave {
	s.ledgerMu.Lock()
	defer s.ledgerMu.Unlock()
	s.waveSeq++
	w := &wave{
		ID:       fmt.Sprintf(waveIDPattern, time.Now().Format(waveTimeFmt), s.waveSeq),
		Mode:     mode,
		Tag:      tag,
		Toolbox:  toolbox,
		QueueRel: queueRel,
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
	// With --debug the answer points at the wave log beside the queue file
	// (§2.7); the file itself opens when the runner starts the wave.
	if s.Cfg.Debug && queueRel != "" {
		w.logRel = waveLogPath(queueRel)
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

// view renders the ledger answer: live snapshots while running, the full
// wave result (with artifactURLs) once finished. With --debug the answer
// carries the pointer to the wave's dump directory (§2.7).
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
		v["jobs"] = mapAnswer(*w.res).Jobs
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
	// (best-effort — a dead provider never blocks the wave, §2.5).
	if s.LLM != nil && scheduler.WantsGPU(w.adm) {
		_, _ = s.LLM.Unload(w.ctx)
	}
	// The debug wave log lives beside the queue file inside the playground
	// (§2.7): <queue-stem>.log (pointer set at submit), an old same-name log
	// rotates out through the archive mechanism before the wave opens its own.
	var opts scheduler.RunOpts
	if s.Cfg.Debug && w.QueueRel != "" {
		opts.WaveID = w.ID
		w.mu.Lock()
		logRel := w.logRel
		w.mu.Unlock()
		_, _ = s.Store.ArchiveIndex(logRel) // best effort: a failed rotation never sinks the wave
		opts.LogPath = s.Store.Abs(logRel)
	}
	res := s.Scheds[w.Toolbox].RunAdmitted(w.ctx, w.adm, opts, w.progress)
	w.mu.Lock()
	w.res = &res
	w.Ended = time.Now()
	w.running = false
	w.mu.Unlock()
	close(w.done)
}

// findWave resolves a {wave: ...} lookup: an exact id, or "latest" — the
// most recent wave. Raw-tagged waves hide from the consumer's default
// "latest" (§2.2); fullRawAccess sees them.
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

// Shutdown drains the wave pipeline within grace (REBUILD §5 gate 9): no
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

// waveLogPath maps a playground-relative queue path to its wave log beside
// it: <dir>/<stem>.log (§2.7).
func waveLogPath(queueRel string) string {
	dir, base := path.Split(queueRel)
	stem := strings.TrimSuffix(base, path.Ext(base))
	if stem == "" {
		stem = "wave"
	}
	return dir + stem + ".log"
}

// joinCtx cancels when either parent cancels (blocking wave: request OR
// daemon shutdown kills it, §2.3).
func joinCtx(primary, secondary context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(primary)
	stop := context.AfterFunc(secondary, cancel)
	return ctx, func() { stop(); cancel() }
}
