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

package resolution

import "testing"

// Goldens are the values the sh reference (the awk block in anima/kroma/...
// scripts) produced before the calculator moved into the core.
func TestComputeGoldens(t *testing.T) {
	for _, tc := range []struct {
		aspect     string
		w, h       int
		wantErrMin bool // side below the 512 floor
	}{
		{"16:9", 1888, 1056, false},
		{"9:16", 1056, 1888, false},
		{"1:1", 1408, 1408, false},
		{"4:3", 1632, 1216, false},
		{"21:9", 2176, 928, false},
		{"1.5", 1728, 1152, false},
		{"1024x768", 1632, 1216, false},
		{"30:1", 7680, 256, true},
		{"0:0", 1408, 1408, false},
	} {
		w, h, err := Compute(tc.aspect, Studio())
		if w != tc.w || h != tc.h {
			t.Errorf("Compute(%q) = %dx%d, want %dx%d", tc.aspect, w, h, tc.w, tc.h)
		}
		if tc.wantErrMin && err == nil {
			t.Errorf("Compute(%q): side below floor must report an error", tc.aspect)
		}
		if !tc.wantErrMin && err != nil {
			t.Errorf("Compute(%q): unexpected error %v", tc.aspect, err)
		}
	}
}

func TestComputeCustomSpec(t *testing.T) {
	spec := Spec{Multiple: 16, Target: 1_000_000, MinSide: 256, MaxSide: 1024}
	w, h, err := Compute("16:9", spec)
	if err != nil || w != 1344 || h != 752 {
		t.Errorf("custom spec 16:9 = %dx%d err %v, want 1344x752", w, h, err)
	}
	// Zero fields fall back to the studio defaults.
	w, h, err = Compute("16:9", Spec{})
	if err != nil || w != 1888 || h != 1056 {
		t.Errorf("zero spec 16:9 = %dx%d err %v, want 1888x1056", w, h, err)
	}
}

// The studio default carries no area ceiling: a big target is a big canvas, and the
// generator's own limits are the tool's business. An explicit MaxSide still clamps, and a
// ceiling below the floor collapses to the floor instead of a resurrected default.
func TestComputeNoCeiling(t *testing.T) {
	w, h, err := Compute("1:1", Spec{Target: 4_000_000})
	if err != nil || w != 2016 || h != 2016 {
		t.Errorf("4 MP 1:1 = %dx%d err %v, want 2016x2016", w, h, err)
	}
	w, h, err = Compute("16:9", Spec{Target: 8_000_000})
	if err != nil || w != 3744 || h != 2112 {
		t.Errorf("8 MP 16:9 = %dx%d err %v, want 3744x2112", w, h, err)
	}
	w, h, err = Compute("1:1", Spec{Target: 8_000_000, MaxSide: 1536})
	if err != nil || w != 1536 || h != 1536 {
		t.Errorf("8 MP 1:1 under a 1536 ceiling = %dx%d err %v, want 1536x1536", w, h, err)
	}
	w, h, _ = Compute("1:1", Spec{MinSide: 1024, MaxSide: 512})
	if w != 1024 || h != 1024 {
		t.Errorf("ceiling under the floor = %dx%d, want 1024x1024", w, h)
	}
}

func TestRatioBadInput(t *testing.T) {
	for _, bad := range []string{"", "  ", "abc", "a:9"} {
		if _, err := Ratio(bad); err == nil {
			t.Errorf("Ratio(%q) must fail", bad)
		}
	}
	if r, err := Ratio("16 × 9"); err != nil || r != 16.0/9.0 {
		t.Errorf(`Ratio("16 × 9") = %v %v`, r, err)
	}
}
