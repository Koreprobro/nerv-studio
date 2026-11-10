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

// Package gpu builds the scheduler's typed slot pool (REBUILD-nerv-core.md
// §2.5): greedy scoop of everything detected, one operator selector
// (gpu_backend), anonymous remote slots (external_gpu) and the tool pin
// resolution (gpu-pin). VRAM is intentionally not accounted — Name/MemMB are
// informational; the sh author owns memory. Backend names are opaque
// strings: built-in tables exist only for detection and pin-env.
package gpu

import (
	"strconv"
	"strings"
)

// Slot is one schedulable GPU slot: a local card (Host "") or an anonymous
// remote capability declared by external_gpu. Index is the detected device
// index — never re-indexed; external slots are numbered 0..N-1.
type Slot struct {
	Backend string
	Host    string // "" => local
	Index   int
	Name    string // informational (model), may be empty
	MemMB   uint64 // informational (never used for accounting)
}

// String renders the slot selector form: "cuda:0", "cuda@host1:2".
func (s Slot) String() string {
	h := ""
	if s.Host != "" {
		h = "@" + s.Host
	}
	return s.Backend + h + ":" + strconv.Itoa(s.Index)
}

// Local reports a same-host slot.
func (s Slot) Local() bool { return s.Host == "" }

// Pool is the frozen typed slot set (backend => slots), built once at
// startup and never mutated afterwards.
type Pool struct {
	slots     []Slot
	byBackend map[string][]Slot
}

func newPool(slots []Slot) *Pool {
	p := &Pool{slots: slots, byBackend: make(map[string][]Slot)}
	for _, s := range slots {
		p.byBackend[s.Backend] = append(p.byBackend[s.Backend], s)
	}
	return p
}

// Slots returns all pool slots in build order.
func (p *Pool) Slots() []Slot { return p.slots }

// Backends returns the distinct backends present in the pool, sorted.
func (p *Pool) Backends() []string {
	names := make([]string, 0, len(p.byBackend))
	for b := range p.byBackend {
		names = append(names, b)
	}
	sortStrings(names)
	return names
}

// Count returns the slots of one backend (local + external).
func (p *Pool) Count(backend string) int { return len(p.byBackend[backend]) }

// MinCount returns the smallest per-backend slot count — the safe capacity
// basis for a tool declaring backend "any" (§2.5). Empty pool => 0.
func (p *Pool) MinCount() int {
	min := -1
	for _, n := range p.byBackend {
		if min < 0 || len(n) < min {
			min = len(n)
		}
	}
	if min < 0 {
		return 0
	}
	return min
}

// Find returns the pool slot matching the exact selector triple.
func (p *Pool) Find(backend, host string, index int) (Slot, bool) {
	for _, s := range p.slots {
		if s.Backend == backend && s.Host == host && s.Index == index {
			return s, true
		}
	}
	return Slot{}, false
}

func (p *Pool) describe() string {
	parts := make([]string, 0, len(p.slots))
	for _, s := range p.slots {
		parts = append(parts, s.String())
	}
	return strings.Join(parts, ", ")
}
