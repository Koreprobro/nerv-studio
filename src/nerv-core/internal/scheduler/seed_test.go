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

import "testing"

func TestResolveSeed(t *testing.T) {
	// No seed field → no-op.
	params := map[string]any{"prompt": "cat"}
	resolveSeed(params)
	if _, ok := params["seed"]; ok {
		t.Fatal("must not inject seed when absent")
	}

	// seed = -1 → replaced by a concrete non-negative int64.
	params = map[string]any{"seed": float64(-1)}
	resolveSeed(params)
	v, ok := params["seed"]
	if !ok {
		t.Fatal("seed must be present")
	}
	n, isInt64 := v.(int64)
	if !isInt64 {
		t.Fatalf("seed must be int64, got %T", v)
	}
	if n < 0 {
		t.Fatalf("seed must be non-negative, got %d", n)
	}
	if n > 0x7FFF_FFFF_FFFF_FFFF {
		t.Fatalf("seed must fit int64 positive, got %d", n)
	}

	// seed = 42 (explicit) → untouched.
	params = map[string]any{"seed": float64(42)}
	resolveSeed(params)
	f, _ := params["seed"].(float64)
	if f != 42 {
		t.Fatalf("explicit seed must remain, got %v", params["seed"])
	}

	// seed = 0 → not -1, untouched.
	params = map[string]any{"seed": float64(0)}
	resolveSeed(params)
	f, _ = params["seed"].(float64)
	if f != 0 {
		t.Fatalf("seed 0 must remain, got %v", params["seed"])
	}

	// Two calls produce different values (hardware randomness).
	params1 := map[string]any{"seed": float64(-1)}
	params2 := map[string]any{"seed": float64(-1)}
	resolveSeed(params1)
	resolveSeed(params2)
	s1, _ := params1["seed"].(int64)
	s2, _ := params2["seed"].(int64)
	if s1 == s2 {
		t.Fatalf("two random seeds must differ (extremely unlikely collision): %d", s1)
	}
}
