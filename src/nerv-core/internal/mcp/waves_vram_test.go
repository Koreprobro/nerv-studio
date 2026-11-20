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
	"errors"
	"testing"
	"time"

	"nerv-core/internal/gpu"
)

func oneSlotPool(t *testing.T) *gpu.Pool {
	t.Helper()
	pool, _, err := gpu.Build("", "", map[string]gpu.Enumerator{
		"cuda": func() ([]gpu.Slot, error) {
			return []gpu.Slot{{Backend: "cuda", Index: 0, Name: "fake"}}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return pool
}

func TestVRAMDrainReleasesWhenQuiet(t *testing.T) {
	vals := []uint64{9000, 8000, 10}
	calls := 0
	load := func(gpu.Slot) (uint64, error) {
		v := vals[calls]
		if calls < len(vals)-1 {
			calls++
		}
		return v, nil
	}
	start := time.Now()
	vramDrain(context.Background(), oneSlotPool(t), load, 10*time.Millisecond, 10*time.Millisecond, time.Second, 512)
	if calls != 2 { // polls: 9000 busy, 8000 busy, then quiet 10 — returned
		t.Errorf("polls wrong: calls=%d want 2", calls)
	}
	if d := time.Since(start); d > 900*time.Millisecond {
		t.Errorf("drain lingered: %v", d)
	}
}

func TestVRAMDrainCapsAtCeiling(t *testing.T) {
	load := func(gpu.Slot) (uint64, error) { return 9000, nil }
	start := time.Now()
	vramDrain(context.Background(), oneSlotPool(t), load, 10*time.Millisecond, 10*time.Millisecond, 200*time.Millisecond, 512)
	if d := time.Since(start); d < 200*time.Millisecond || d > 600*time.Millisecond {
		t.Errorf("ceiling miss: waited %v, want ~200ms", d)
	}
}

func TestVRAMDrainCanceledLeavesNow(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	load := func(gpu.Slot) (uint64, error) { t.Error("loader must not run after cancel"); return 0, nil }
	vramDrain(ctx, oneSlotPool(t), load, 50*time.Millisecond, 10*time.Millisecond, time.Second, 512)
}

func TestVRAMDrainNoQueryableSlots(t *testing.T) {
	load := func(gpu.Slot) (uint64, error) { return 0, errors.New("no load query for slot") }
	start := time.Now()
	vramDrain(context.Background(), oneSlotPool(t), load, 10*time.Millisecond, 10*time.Millisecond, 5*time.Second, 512)
	if d := time.Since(start); d > 250*time.Millisecond {
		t.Errorf("unqueryable pool must fall through after the floor, waited %v", d)
	}
}
