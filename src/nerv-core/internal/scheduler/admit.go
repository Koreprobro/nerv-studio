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
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"nerv-core/internal/gpu"
	"nerv-core/internal/job"
)

// ResolveTool guards the tool name (traversal-safe, relative to tools/) and
// applies the ".sh may be omitted" leniency of the browse listing.
func ResolveTool(toolsDir, name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", fmt.Errorf("'tool' is required")
	}
	name = filepath.ToSlash(name)
	if err := job.WithinRel(name); err != nil {
		return "", fmt.Errorf("tool %q: %v", name, err)
	}
	if !strings.HasSuffix(name, ".sh") {
		if st, err := os.Stat(filepath.Join(toolsDir, filepath.FromSlash(name+".sh"))); err == nil && !st.IsDir() {
			name += ".sh"
		}
	}
	st, err := os.Stat(filepath.Join(toolsDir, filepath.FromSlash(name)))
	if err != nil || st.IsDir() {
		return "", fmt.Errorf("tool %q not found under tools/", name)
	}
	return name, nil
}

// ProbeTool reads the tool's self-declared interface. The probe runs in a
// scratch dir with a minimal env: an unguarded script must not write into
// the daemon's working directory.
func ProbeTool(toolsDir, name string) (*job.ToolInfo, error) {
	cmd := exec.Command("sh", filepath.Join(toolsDir, filepath.FromSlash(name)), "--tool-info")
	cmd.Dir = os.TempDir()
	cmd.Env = []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("--tool-info failed: %v: %s", err, tail(strings.TrimSpace(string(out)), 200))
	}
	return job.ParseToolInfo(out)
}

// admit resolves a job's GPU admission set — backend ∩ pin — fail-fast:
// a nail that missed the wall never enters the wave loop (§2.5).
func admit(pool *gpu.Pool, backend, pin string) ([]gpu.Slot, error) {
	if pool == nil {
		return nil, fmt.Errorf("gpu-require > 0 but no GPU pool is configured")
	}
	base, err := gpu.Filter(pool.Slots(), backend)
	if err != nil {
		return nil, fmt.Errorf("gpu-backend: %w", err)
	}
	if pin != "" {
		pins, err := pool.PinSet(pin)
		if err != nil {
			return nil, err
		}
		in := make(map[gpu.Slot]bool, len(pins))
		for _, sl := range pins {
			in[sl] = true
		}
		var inter []gpu.Slot
		for _, sl := range base {
			if in[sl] {
				inter = append(inter, sl)
			}
		}
		base = inter
		if len(base) == 0 {
			return nil, fmt.Errorf("gpu-pin %q matches no slot of backend %q", pin, backend)
		}
	}
	if len(base) == 0 {
		return nil, fmt.Errorf("no pool slot matches gpu-backend %q (pool backends: %s)",
			backend, strings.Join(pool.Backends(), ","))
	}
	return base, nil
}

// takeFree picks n not-yet-busy eligible slots (exclusive capture); nil when
// the wave cannot seat the job yet.
func takeFree(eligible []gpu.Slot, busy map[gpu.Slot]bool, n int) []gpu.Slot {
	out := make([]gpu.Slot, 0, n)
	for _, sl := range eligible {
		if busy[sl] {
			continue
		}
		out = append(out, sl)
		if len(out) == n {
			return out
		}
	}
	return nil
}

func readyEntries(s *Scheduler, entries []*Admitted) []*Admitted {
	var out []*Admitted
	for _, e := range entries {
		if e.status != job.Pending {
			continue
		}
		if e.missing(s) == nil {
			out = append(out, e)
		}
	}
	return out
}

func (e *Admitted) missing(s *Scheduler) []string {
	var miss []string
	for _, rel := range e.inputs {
		if !s.Deps.Artifactory.Exists(rel) {
			miss = append(miss, rel)
		}
	}
	return miss
}
