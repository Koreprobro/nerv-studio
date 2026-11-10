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

package scheduler

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"nerv-core/internal/gpu"
)

// waveLog is the per-wave debug log (§2.7): ONE file beside the queue file,
// holding jsonl event lines plus every job's streamed stdout+stderr
// (line-prefixed so parallel jobs stay attributable). It exists only when
// RunOpts.LogPath is set — the default daemon writes no files at all.
// Every method is nil-safe and goroutine-safe (job goroutines report
// concurrently).
type waveLog struct {
	mu  sync.Mutex
	f   *os.File
	enc *json.Encoder
}

// openWaveLog creates the log file (its directory already holds the queue);
// a failure degrades to "no log" — debugging must never sink the wave.
func openWaveLog(opts RunOpts) *waveLog {
	if opts.LogPath == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(opts.LogPath), 0o755); err != nil {
		return nil
	}
	f, err := os.Create(opts.LogPath)
	if err != nil {
		return nil
	}
	return &waveLog{f: f, enc: json.NewEncoder(f)}
}

func (w *waveLog) close() {
	if w != nil {
		_ = w.f.Close()
	}
}

func (w *waveLog) event(ev map[string]any) {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	_ = w.enc.Encode(ev)
}

// jobWriter returns the sink for one job's combined output: every line is
// prefixed "<index> <tool> | " so interleaved parallel jobs stay readable.
// nil-safe: nil when the wave log is off.
func (w *waveLog) jobWriter(idx int, tool string) io.Writer {
	if w == nil {
		return nil
	}
	stem := strings.TrimSuffix(path.Base(tool), ".sh")
	return &prefixWriter{w: w, prefix: fmt.Sprintf("%d %s | ", idx, stem)}
}

// prefixWriter stamps each complete or partial line it receives.
type prefixWriter struct {
	w       *waveLog
	prefix  string
	atStart bool // true once the previous write ended mid-line
}

func (p *prefixWriter) Write(b []byte) (int, error) {
	p.w.mu.Lock()
	defer p.w.mu.Unlock()
	written := 0
	for len(b) > 0 {
		if !p.atStart {
			if _, err := p.w.f.WriteString(p.prefix); err != nil {
				return written, err
			}
			p.atStart = true
		}
		chunk := b
		if nl := bytes.IndexByte(b, '\n'); nl >= 0 {
			chunk = b[:nl+1]
			p.atStart = false
		}
		n, err := p.w.f.Write(chunk)
		written += n
		if err != nil {
			return written, err
		}
		b = b[n:]
	}
	return written, nil
}

// stamp is the RFC3339 wall clock for dump events.
func stamp() string { return time.Now().Format(time.RFC3339) }

// slotNames renders captured slots for dump events.
func slotNames(slots []gpu.Slot) []string {
	if len(slots) == 0 {
		return nil
	}
	out := make([]string, 0, len(slots))
	for _, sl := range slots {
		out = append(out, sl.String())
	}
	return out
}

// jobSummary is the end-event snapshot: final status + duration per line.
func jobSummary(adm []*Admitted) []map[string]any {
	out := make([]map[string]any, 0, len(adm))
	for _, e := range adm {
		out = append(out, map[string]any{
			"index": e.idx, "tool": e.j.Tool, "status": string(e.status),
			"dur": job0Duration(e.dur), "slots": slotNames(e.slots),
		})
	}
	return out
}

func job0Duration(d time.Duration) string {
	if d == 0 {
		return ""
	}
	return d.Round(time.Second).String()
}
