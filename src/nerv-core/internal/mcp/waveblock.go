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
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"nerv-core/internal/scheduler"
)

// waveDebugCap bounds the debug tail folded into the queue file's wave
// block (SPECS §6.6): the playground copy stays readable; the full log is
// transient anyway (system temp, deleted after the fold).
const waveDebugCap = 256 << 10

// writeWaveBlocks stamps the finished wave back into every queue file of
// the set (SPECS §6.1): a "wave" object with the wave id, the verdict and
// the file's own sub-ledger (indexes local to that file) — the queue file
// becomes a self-describing ticket: recipe in, result out. Bare arrays are
// promoted to the {"jobs": [...], "wave": {...}} envelope; the daemon
// written block is ignored on read, so a finished queue resubmits as is
// (reshoot). The debug tail rides in the anchor (first) file only.
// Best-effort: a write failure is reported on stderr, the wave answer stands.
func (s *Server) writeWaveBlocks(w *wave, debug, debugNote string) {
	if s.Mount == "" || len(w.Files) == 0 {
		return
	}
	w.mu.Lock()
	res := w.res
	w.mu.Unlock()
	if res == nil {
		return
	}
	ledger := w.mapAnswer(*res).Jobs
	offset := 0
	for i, f := range w.Files {
		abs := s.Store.Abs(f.File)
		raw, err := os.ReadFile(abs)
		if err == nil {
			obj := map[string]any{}
			if firstByte(raw) == "[" { // promote the bare array to the envelope
				var lines []json.RawMessage
				err = json.Unmarshal(raw, &lines)
				obj["jobs"] = lines
			} else {
				err = json.Unmarshal(raw, &obj)
			}
			if err == nil {
				err = stampWaveFile(abs, obj, w, res, ledger, offset, f.Jobs, i == 0, debug, debugNote)
			}
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "wave %s: queue write-back %s: %v\n", w.ID, f.File, err)
		}
		offset += f.Jobs
	}
}

// stampWaveFile writes the wave block into the parsed queue object and
// swaps it in atomically (temp file + rename).
func stampWaveFile(abs string, obj map[string]any, w *wave, res *scheduler.Result,
	ledger []answeredJob, offset, jobs int, anchor bool, debug, debugNote string) error {
	sub := make([]answeredJob, 0, jobs)
	for _, aj := range ledger {
		if aj.Index >= offset && aj.Index < offset+jobs {
			aj.Index -= offset // the line's place in this file, not in the wave
			sub = append(sub, aj)
		}
	}
	block := map[string]any{"wave": w.ID, "mode": w.Mode, "ok": res.OK(),
		"started": w.Started.Format(time.RFC3339), "finished": w.Ended.Format(time.RFC3339),
		"jobs": sub}
	if anchor {
		if w.Skipped != nil {
			block["skipped"] = w.Skipped
		}
		if debug != "" {
			block["debug"] = debug
			if debugNote != "" {
				block["debug_note"] = debugNote
			}
		}
	}
	obj["wave"] = block
	out, err := json.MarshalIndent(obj, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(filepath.Dir(abs), "."+filepath.Base(abs)+".wave.tmp")
	if err := os.WriteFile(tmp, append(out, '\n'), 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, abs); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// tailFile reads the last maxBytes of a file and reports them with the
// file's full size.
func tailFile(p string, maxBytes int64) (string, int64, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", 0, err
	}
	size := st.Size()
	if size > maxBytes {
		if _, err := f.Seek(size-maxBytes, io.SeekStart); err != nil {
			return "", size, err
		}
	}
	b, err := io.ReadAll(f)
	return string(b), size, err
}
