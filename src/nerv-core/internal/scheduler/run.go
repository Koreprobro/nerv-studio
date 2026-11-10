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

package scheduler

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"nerv-core/internal/job"
)

// runWave runs the wave concurrently, each job on its exclusive slots. A
// job that exits 0 without having written its declared output fails: the
// exit code alone is not trusted to mean a file landed (§2.4).
func (s *Scheduler) runWave(ctx context.Context, wave []*Admitted, onProgress Progress, wl *waveLog) {
	var wg sync.WaitGroup
	for _, e := range wave {
		wg.Add(1)
		go func(e *Admitted) {
			defer wg.Done()
			s.setStatus(e, job.Running, "", onProgress)
			wl.event(map[string]any{"event": "start", "index": e.idx, "tool": e.j.Tool,
				"slots": slotNames(e.slots), "t": stamp()})
			t0 := time.Now()
			err := s.runJob(ctx, e, wl)
			e.dur = time.Since(t0)
			e.j.Dur = job.Duration{Duration: e.dur}
			if err == nil {
				for _, out := range e.outputs {
					if !s.Deps.Artifactory.Exists(out) {
						err = fmt.Errorf("tool wrote no output at %s", out)
						break
					}
				}
			}
			if err != nil {
				s.setStatus(e, job.Failed, err.Error(), onProgress)
			} else {
				s.setStatus(e, job.Done, "", onProgress)
			}
			wl.event(map[string]any{"event": string(e.status), "index": e.idx, "tool": e.j.Tool,
				"dur": job.Duration{Duration: e.dur}.String(), "t": stamp()})
		}(e)
	}
	wg.Wait()
}

// setStatus mutates the entry and its job snapshot, then reports. Entries
// are touched by exactly one goroutine at a time and the wave loop reads
// statuses only after wg.Wait().
func (s *Scheduler) setStatus(e *Admitted, st job.Status, errMsg string, onProgress Progress) {
	e.status, e.err = st, errMsg
	e.j.Status, e.j.Error = st, errMsg
	if onProgress != nil {
		onProgress(e.j, st)
	}
}

// runJob executes the script with the contract env. Exit 0 == ok; the
// script runs in its own process group and ctx cancellation SIGKILLs the
// whole group (the wrapper sh spawns generator children — killing only the
// direct child would orphan the GPU process). Output streams into a capped
// in-memory buffer (the failed-tail source, last ~64 KB, §2.7) and, with a
// debug wave log, additionally into <index>-<tool-stem>.log.
func (s *Scheduler) runJob(ctx context.Context, e *Admitted, wl *waveLog) error {
	if err := s.archiveOld(e); err != nil {
		return err
	}
	for _, out := range e.outputs {
		if err := s.Deps.Artifactory.EnsureOutputDir(out); err != nil {
			return fmt.Errorf("create output dir: %w", err)
		}
	}
	cmd := exec.CommandContext(ctx, "sh", filepath.Join(s.Deps.ToolsDir, filepath.FromSlash(e.j.Tool)))
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 10 * time.Second
	env, err := s.buildEnv(e)
	if err != nil {
		return err
	}
	cmd.Env = env
	buf := &cappedWriter{capBytes: 64 << 10}
	var w io.Writer = buf
	if jw := wl.jobWriter(e.idx, e.j.Tool); jw != nil {
		w = io.MultiWriter(buf, jw)
	}
	cmd.Stdout, cmd.Stderr = w, w
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s: %w", e.j.Tool, err)
	}
	if err := cmd.Wait(); err != nil {
		if msg := strings.TrimSpace(buf.String()); msg != "" {
			return fmt.Errorf("%w: %s", err, tail(msg, 500))
		}
		return err
	}
	return nil
}

// cappedWriter keeps only the last capBytes of the stream — an endless
// generator log must not grow the daemon's memory without bound (§2.7).
type cappedWriter struct {
	capBytes int
	buf      []byte
}

func (c *cappedWriter) Write(p []byte) (int, error) {
	c.buf = append(c.buf, p...)
	if len(c.buf) > c.capBytes*2 { // trim early: keep the amortization cheap
		c.buf = append([]byte(nil), c.buf[len(c.buf)-c.capBytes:]...)
	}
	return len(p), nil
}

func (c *cappedWriter) String() string { return string(c.buf) }

// archiveOld is the core's same-name overwrite protection (SPECS §6): an
// existing file the job is about to produce is moved aside to
// "name-<i>.ext" before the script starts, so the consumer-AI always gets
// exactly the output name it declared and conflicts resolve in favor of
// the new file. In-place jobs (an output that is also an input of the same
// job) keep the tool's own overwrite — the script must still find its input.
func (s *Scheduler) archiveOld(e *Admitted) error {
	for _, out := range e.outputs {
		inPlace := false
		for _, in := range e.inputs {
			if in == out {
				inPlace = true
				break
			}
		}
		if inPlace {
			continue
		}
		moved, err := s.Deps.Artifactory.ArchiveIndex(out)
		if err != nil {
			return fmt.Errorf("archive old output: %w", err)
		}
		if moved != "" {
			e.archived = append(e.archived, moved)
		}
	}
	return nil
}

// abort lands every not-yet-started job as failed (cancel / no-slots).
func (s *Scheduler) abort(entries []*Admitted, msg string, onProgress Progress) {
	for _, e := range entries {
		if e.status == job.Pending {
			s.setStatus(e, job.Failed, msg, onProgress)
		}
	}
}

// failStuck is the deadlock trap: pending jobs waiting on files that will
// never appear (their producer failed or was never in the queue). A
// circular queue lands here too — no cycle detection needed.
func (s *Scheduler) failStuck(entries []*Admitted, onProgress Progress) {
	for _, e := range entries {
		if e.status != job.Pending {
			continue
		}
		s.setStatus(e, job.Stuck, "input file(s) never appeared: "+strings.Join(e.missing(s), ", "), onProgress)
	}
}

func results(entries []*Admitted) []JobResult {
	out := make([]JobResult, 0, len(entries))
	for _, e := range entries {
		slots := make([]string, 0, len(e.slots))
		for _, sl := range e.slots {
			slots = append(slots, sl.String())
		}
		out = append(out, JobResult{
			Index: e.idx, Tool: e.j.Tool, Outputs: e.outputs, Archived: e.archived,
			Status: string(e.status), Error: e.err,
			Dur: job.Duration{Duration: e.dur}.String(), Slots: slots,
		})
	}
	return out
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}
