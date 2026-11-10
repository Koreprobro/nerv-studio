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

// Package scheduler runs a submitted queue across the typed GPU pool with
// file-existence dataflow (REBUILD-nerv-core.md §2.4, §2.5).
//
// There is no dependency graph: a job declares its output files through
// io-marked fields and its input files the same way. The scheduler
// repeatedly scans the queue: pending jobs whose input files all exist are
// packed into free slots (typed admission: backend ∩ pin) and run as a
// wave. When nothing runs and jobs still wait on files that never appear,
// they fail as stuck — a submit-side cycle lands in exactly that trap, so
// no cycle detection is needed.
package scheduler

import (
	"context"
	"fmt"
	"sort"
	"time"

	"nerv-core/internal/gpu"
	"nerv-core/internal/job"
	"nerv-core/internal/store"
)

// Progress receives every job status change. It may be called concurrently
// from several job goroutines — implementations must be goroutine-safe
// (the mcp layer fans events out to SSE).
type Progress func(*job.Job, job.Status)

// Deps wires the scheduler to the studio directories, the frozen pool and
// the operator env (config "env": passed to scripts as-is). The scheduler
// needs no other roots: io paths are resolved against the artifactory, the
// scripts own everything else (SPECS §5).
type Deps struct {
	ToolsDir    string
	Artifactory store.Store
	Pool        *gpu.Pool
	UserEnv     map[string]string
}

// Admitted is one queue line after admission: validated against the tool's
// interface, with io paths and the eligible slot set resolved. Its fields
// stay package-private — callers treat it as an opaque handle for
// RunAdmitted/WantsGPU.
type Admitted struct {
	idx       int
	j         *job.Job
	outputs   []string
	inputs    []string
	outFields map[string]bool // param names declared io:"out" — their env values go absolute (§5)
	inFields  map[string]bool // param names declared io:"in" — absolute in env too (§5.1)
	calc      bool            // tool declares the aspect-ratio + target-mpx pair (§7.3)
	archived  []string        // outputs whose old same-name file was moved aside (§SPECS 6)
	count     int
	backend   string
	pin       string
	eligible  []gpu.Slot // backend ∩ pin; empty only for CPU jobs
	status    job.Status
	err       string
	slots     []gpu.Slot
	dur       time.Duration
}

// Skipped is a queue line that never entered the wave loop: the entry-level
// home of every soft validation failure (§2.3 skipped[{index, tool, reason}]).
type Skipped struct {
	Index  int    `json:"index"`
	Tool   string `json:"tool"`
	Reason string `json:"reason"`
}

// JobResult is a job's final line of the wave answer.
type JobResult struct {
	Index    int      `json:"index"`
	Tool     string   `json:"tool"`
	Outputs  []string `json:"outputs,omitempty"`
	Archived []string `json:"archived,omitempty"` // old same-name files moved to name-<i>.ext
	Status   string   `json:"status"`
	Error    string   `json:"error,omitempty"`
	Dur      string   `json:"dur,omitempty"`
	Slots    []string `json:"slots,omitempty"`
}

// Result is the outcome of one Run (the execute wave answer; the mcp layer
// adds ids and artifactURLs on top).
type Result struct {
	Jobs    []JobResult `json:"jobs"`
	Skipped []Skipped   `json:"skipped,omitempty"`
}

// OK reports every executed job finished done (skipped lines are reported
// separately and do not poison the flag).
func (r Result) OK() bool {
	for _, j := range r.Jobs {
		if j.Status != string(job.Done) {
			return false
		}
	}
	return true
}

// Scheduler runs queues over a frozen pool. It holds no per-run state
// (caches are queue-local), so concurrent Runs are safe.
type Scheduler struct {
	Deps Deps
}

// New returns a Scheduler over the given dependencies.
func New(d Deps) *Scheduler { return &Scheduler{Deps: d} }

// Admit validates queue lines without running anything: tool resolution,
// the --tool-info interface, the field check and GPU admission (§2.3). Soft
// failures leave the queue in skipped[{index, tool, reason}]; the caller
// decides what to do with the accepted remainder — run it now or own it as
// an async wave (the mcp layer is the only scheduling client).
func (s *Scheduler) Admit(queue []job.Job) ([]*Admitted, []Skipped) {
	var skipped []Skipped
	infos := map[string]*job.ToolInfo{} // --tool-info cache, per queue
	entries := make([]*Admitted, 0, len(queue))
	for i := range queue {
		q := &queue[i]
		tool, err := ResolveTool(s.Deps.ToolsDir, q.Tool)
		if err != nil {
			skipped = append(skipped, Skipped{i, q.Tool, err.Error()})
			continue
		}
		q.Tool = tool
		ti, ok := infos[tool]
		if !ok {
			ti, err = ProbeTool(s.Deps.ToolsDir, tool)
			if err != nil {
				skipped = append(skipped, Skipped{i, tool, err.Error()})
				continue
			}
			infos[tool] = ti
		}
		outs, ins, reason := q.Check(ti)
		if reason != "" {
			skipped = append(skipped, Skipped{i, tool, reason})
			continue
		}
		e := &Admitted{idx: i, j: q, outputs: outs, inputs: ins, status: job.Pending}
		for k := range ti.Fields {
			switch ti.Fields[k].IO {
			case "out":
				if e.outFields == nil {
					e.outFields = map[string]bool{}
				}
				e.outFields[ti.Fields[k].Name] = true
			case "in":
				if e.inFields == nil {
					e.inFields = map[string]bool{}
				}
				e.inFields[ti.Fields[k].Name] = true
			}
		}
		e.calc = declaresCalcPair(ti)
		e.count, e.backend, e.pin = q.Require(ti), q.Backend(ti), q.Pin(ti)
		if e.count > 0 {
			elig, err := admit(s.Deps.Pool, e.backend, e.pin)
			if err != nil {
				skipped = append(skipped, Skipped{i, tool, err.Error()})
				continue
			}
			if e.count > len(elig) {
				skipped = append(skipped, Skipped{i, tool,
					fmt.Sprintf("gpu-require %d exceeds %d matching pool slot(s)", e.count, len(elig))})
				continue
			}
			e.eligible = elig
		}
		entries = append(entries, e)
	}
	return entries, skipped
}

// WantsGPU reports whether any admitted line asks for GPU slots — the
// XOR-offload and blocking-mode trigger for the mcp layer (§2.3).
func WantsGPU(adm []*Admitted) bool {
	for _, e := range adm {
		if e.count > 0 {
			return true
		}
	}
	return false
}

// Index, Tool and Job expose an admitted line to the mcp wave ledger
// (snapshot keys and SSE messages) without opening the struct itself.
func (a *Admitted) Index() int    { return a.idx }
func (a *Admitted) Tool() string  { return a.j.Tool }
func (a *Admitted) Job() *job.Job { return a.j }

// RunOpts tunes one admitted wave. LogPath is the wave log file beside the
// queue file (<queue-stem>.log, §2.7): when set, the scheduler writes the
// event lines and every job's streamed stdout+stderr into this one file.
// Empty means no files at all — job output then lives only in the capped
// in-memory buffer feeding the failed-tail (M12 default).
type RunOpts struct {
	WaveID  string // ledger id carried in dump events (optional)
	LogPath string // wave log file beside the queue; "" = no log
}

// RunAdmitted dispatches accepted entries in waves until everything is
// done, stuck, canceled or (defensively) slot-starved. It never returns an
// error: every outcome has a per-line home in the Result — the wave answer
// is always deliverable.
func (s *Scheduler) RunAdmitted(ctx context.Context, adm []*Admitted, opts RunOpts, onProgress Progress) Result {
	wl := openWaveLog(opts)
	defer wl.close()
	wl.event(map[string]any{"event": "begin", "wave": opts.WaveID, "jobs": len(adm), "t": stamp()})
waves:
	for {
		if err := ctx.Err(); err != nil {
			s.abort(adm, "wave canceled: "+err.Error(), onProgress)
			break
		}
		ready := readyEntries(s, adm)
		if len(ready) == 0 {
			break
		}
		// Biggest-first packing reduces slot fragmentation.
		sort.SliceStable(ready, func(i, k int) bool { return ready[i].count > ready[k].count })
		busy := map[gpu.Slot]bool{}
		var wave []*Admitted
		for _, e := range ready {
			if e.count == 0 {
				wave = append(wave, e) // CPU job: unlimited, no slots
				continue
			}
			picked := takeFree(e.eligible, busy, e.count)
			if picked == nil {
				continue // not enough free matching slots right now; next wave
			}
			for _, sl := range picked {
				busy[sl] = true
			}
			e.slots = picked
			e.j.Slots = picked
			wave = append(wave, e)
		}
		if len(wave) == 0 {
			// Admission guaranteed every admitted job fits an idle pool, so
			// this fires only defensively (a shrinking pool, a future bug).
			s.abort(adm, "no free GPU slots for any ready job", onProgress)
			break waves
		}
		s.runWave(ctx, wave, onProgress, wl)
	}
	s.failStuck(adm, onProgress)
	res := Result{Jobs: results(adm)}
	wl.event(map[string]any{"event": "end", "ok": res.OK(), "jobs": jobSummary(adm), "t": stamp()})
	return res
}

// Run admits and runs in one step — the simple call site (tests, callers
// that never need the admission split).
func (s *Scheduler) Run(ctx context.Context, queue []job.Job, onProgress Progress) Result {
	adm, skipped := s.Admit(queue)
	res := s.RunAdmitted(ctx, adm, RunOpts{}, onProgress)
	res.Skipped = skipped
	return res
}
