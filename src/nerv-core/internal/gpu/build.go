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
	"fmt"
	"strconv"
	"strings"
)

// Build assembles the frozen pool (REBUILD-nerv-core.md §2.5):
// greedy scoop of every enumerated backend, filtered by the operator
// selector (backendSel, the gpu_backend value), plus the declared external
// slots (externalSpec, the external_gpu value). An absent backend only
// warns — a CPU-only host must still serve the studio (audit M5 / D4).
func Build(backendSel, externalSpec string, enums map[string]Enumerator) (*Pool, []string, error) {
	var warns []string
	sel, err := ParseSelector(backendSel)
	if err != nil {
		return nil, nil, fmt.Errorf("gpu_backend: %w", err)
	}

	order := []string{}
	if sel == nil {
		for b := range enums {
			order = append(order, b)
		}
		sortStrings(order)
	} else {
		order = sel.backends()
	}

	var slots []Slot
	for _, b := range order {
		e, ok := enums[b]
		if !ok {
			warns = append(warns, fmt.Sprintf("gpu_backend: backend %q has no enumerator", b))
			continue
		}
		detected, err := e()
		if err != nil {
			warns = append(warns, fmt.Sprintf("gpu_backend: backend %q not present: %v", b, err))
			continue
		}
		matched := 0
		for _, s := range detected {
			if sel != nil && !sel.matches(s) {
				continue
			}
			slots = append(slots, s)
			matched++
		}
		if sel != nil && matched == 0 {
			warns = append(warns, fmt.Sprintf("gpu_backend: selector %q matches no detected %s slot", backendSel, b))
		}
	}

	ext, err := ParseExternal(externalSpec)
	if err != nil {
		return nil, nil, fmt.Errorf("external_gpu: %w", err)
	}
	for _, s := range ext {
		if sel != nil && !sel.matches(s) {
			continue // the selector may restrict remote slots too
		}
		slots = append(slots, s)
	}
	return newPool(slots), warns, nil
}

// ParseExternal parses the external_gpu value: comma-separated
// backend@host[:N] entries, N default 1. The slots are anonymous: no
// detection, no health-check — a remote host is just a slot for the
// scheduler; the transport is the sh developer's concern (§2.5).
func ParseExternal(value string) ([]Slot, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}
	var out []Slot
	for _, t := range strings.Split(value, ",") {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		left, spec := t, ""
		if i := strings.LastIndex(t, ":"); i >= 0 {
			left, spec = t[:i], t[i+1:]
		}
		b, h, ok := strings.Cut(left, "@")
		if !ok || b == "" || h == "" {
			return nil, fmt.Errorf("bad entry %q (want backend@host[:N])", t)
		}
		n := 1
		if spec != "" {
			v, err := strconv.Atoi(spec)
			if err != nil || v < 1 {
				return nil, fmt.Errorf("bad entry %q (N must be a positive int)", t)
			}
			n = v
		}
		for i := 0; i < n; i++ {
			out = append(out, Slot{Backend: b, Host: h, Index: i, Name: "external"})
		}
	}
	return out, nil
}

// PinSet resolves a gpu-pin value (same grammar as the selector) against
// the pool: the scheduler may take slots only from the returned set
// ("прибить гвоздями"). An empty value means no pin. A pin that matches
// nothing is an error — the nail missed the wall (fail-fast, §2.5).
func (p *Pool) PinSet(value string) ([]Slot, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}
	sel, err := ParseSelector(value)
	if err != nil {
		return nil, fmt.Errorf("gpu-pin: %w", err)
	}
	var out []Slot
	for _, s := range p.slots {
		if sel.matches(s) {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("gpu-pin %q matches no pool slot (%s)", value, p.describe())
	}
	return out, nil
}
