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

package gpu

import (
	"errors"
	"strings"
	"testing"
)

func fakeEnums() map[string]Enumerator {
	return map[string]Enumerator{
		"cuda": func() ([]Slot, error) {
			return []Slot{
				{Backend: "cuda", Index: 0, Name: "RTX 4090", MemMB: 24564},
				{Backend: "cuda", Index: 1, Name: "RTX 3050", MemMB: 8192},
			}, nil
		},
		"rocm": func() ([]Slot, error) {
			return []Slot{{Backend: "rocm", Index: 0, Name: "RX 7900"}}, nil
		},
		"intel": func() ([]Slot, error) {
			return nil, errors.New("sycl-ls: not found") // absent => drops out
		},
	}
}

func TestBuildGreedyScoop(t *testing.T) {
	pool, warns, err := Build("", "", fakeEnums())
	if err != nil {
		t.Fatal(err)
	}
	if len(pool.Slots()) != 3 {
		t.Errorf("greedy scoop = %v, want cuda:0, cuda:1, rocm:0", pool.describe())
	}
	if pool.Count("cuda") != 2 || pool.Count("rocm") != 1 || pool.Count("intel") != 0 {
		t.Errorf("counts wrong: cuda=%d rocm=%d", pool.Count("cuda"), pool.Count("rocm"))
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "intel") {
		t.Errorf("absent backend must warn once: %v", warns)
	}
	if pool.MinCount() != 1 {
		t.Errorf("MinCount = %d, want 1 (min across backends)", pool.MinCount())
	}
}

func TestBuildSelectorFilter(t *testing.T) {
	pool, warns, err := Build("cuda:1", "", fakeEnums())
	if err != nil {
		t.Fatal(err)
	}
	if len(pool.Slots()) != 1 || pool.Slots()[0].String() != "cuda:1" {
		t.Errorf("selector must keep only cuda:1, got %s (%v)", pool.describe(), warns)
	}
	// a selected backend absent on the host: warn, not fatal
	if _, warns, err := Build("tpu", "", fakeEnums()); err != nil || len(warns) == 0 {
		t.Errorf("unknown backend selector: err=%v warns=%v", err, warns)
	}
	// range grammar
	pool, _, err = Build("cuda:0-1,rocm", "", fakeEnums())
	if err != nil || len(pool.Slots()) != 3 {
		t.Errorf("range selector: %s err=%v", pool.describe(), err)
	}
}

func TestBuildSelectorMatchesNothingWarns(t *testing.T) {
	_, warns, err := Build("cuda:5", "", fakeEnums())
	if err != nil {
		t.Fatal(err)
	}
	if len(warns) == 0 || !strings.Contains(warns[0], "matches no detected") {
		t.Errorf("cuda:5 must warn, got %v", warns)
	}
}

func TestExternalSlots(t *testing.T) {
	ext, err := ParseExternal("cuda@host1:2,rocm@host2")
	if err != nil {
		t.Fatal(err)
	}
	if len(ext) != 3 {
		t.Fatalf("want 3 slots (2+1 default), got %v", ext)
	}
	if ext[0].String() != "cuda@host1:0" || ext[2].String() != "rocm@host2:0" {
		t.Errorf("slot naming wrong: %v", ext)
	}
	if ext[0].Local() {
		t.Error("external slot must not be local")
	}
	for _, bad := range []string{"cuda", "cuda@host1:0", "cuda@host1:x"} {
		if _, err := ParseExternal(bad); err == nil {
			t.Errorf("external entry %q must be rejected", bad)
		}
	}
}

func TestBuildWithExternal(t *testing.T) {
	pool, _, err := Build("", "cuda@nowhere:2", fakeEnums())
	if err != nil {
		t.Fatal(err)
	}
	if pool.Count("cuda") != 4 {
		t.Errorf("external cuda slots must add to the pool: %s", pool.describe())
	}
	// CPU-only host + external slots (checkpoint 6 scenario)
	cpuOnly := map[string]Enumerator{"cuda": func() ([]Slot, error) { return nil, errors.New("no nvidia-smi") }}
	pool, _, err = Build("", "cuda@nowhere:2", cpuOnly)
	if err != nil || len(pool.Slots()) != 2 {
		t.Errorf("remote-pool mode on a GPU-less host: %s err=%v", pool.describe(), err)
	}
}
