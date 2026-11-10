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
	"sort"
	"strconv"
	"strings"
)

func sortStrings(s []string) { sort.Strings(s) }

// Enumerator lists the local slots of one backend (detection is per vendor;
// the map is injectable for tests).
type Enumerator func() ([]Slot, error)

// idxSet is an index filter: all indices, or an explicit set built as the
// union of singletons and ranges.
type idxSet struct {
	all bool
	ids map[int]bool
}

func (s *idxSet) match(i int) bool { return s.all || s.ids[i] }

// term is one comma-separated selector term: backend[@host][:index|a-b].
// Without @host the term addresses local slots only.
type term struct {
	backend string
	host    string
	ids     *idxSet
}

func (t term) match(s Slot) bool {
	return t.backend == s.Backend && t.host == s.Host && t.ids.match(s.Index)
}

// Selector is the gpu_backend / gpu-pin value grammar.
type Selector struct{ terms []term }

// ParseSelector parses "cuda,rocm:0-1,cuda:3" style values; an empty value
// yields a nil selector (greedy scoop / no pin).
func ParseSelector(value string) (*Selector, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}
	sel := &Selector{}
	for _, t := range strings.Split(value, ",") {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		tm, err := parseTerm(t)
		if err != nil {
			return nil, err
		}
		sel.terms = append(sel.terms, tm)
	}
	if len(sel.terms) == 0 {
		return nil, nil
	}
	return sel, nil
}

func parseTerm(t string) (term, error) {
	var tm term
	left, spec := t, ""
	if i := strings.Index(t, ":"); i >= 0 {
		left, spec = t[:i], t[i+1:]
	}
	if b, h, ok := strings.Cut(left, "@"); ok {
		tm.backend, tm.host = b, h
	} else {
		tm.backend = left
	}
	if tm.backend == "" {
		return term{}, fmt.Errorf("bad selector term %q", t)
	}
	switch {
	case spec == "":
		tm.ids = &idxSet{all: true}
	default:
		tm.ids = &idxSet{ids: map[int]bool{}}
		lo, hiStr, ranged := strings.Cut(spec, "-")
		loN, err := strconv.Atoi(lo)
		if err != nil || loN < 0 {
			return term{}, fmt.Errorf("bad selector term %q", t)
		}
		if !ranged {
			tm.ids.ids[loN] = true
			break
		}
		hiN, err := strconv.Atoi(hiStr)
		if err != nil || hiN < loN || hiN-loN > 63 {
			return term{}, fmt.Errorf("bad selector term %q", t)
		}
		for i := loN; i <= hiN; i++ {
			tm.ids.ids[i] = true
		}
	}
	return tm, nil
}

func (sel *Selector) matches(s Slot) bool {
	for _, t := range sel.terms {
		if t.match(s) {
			return true
		}
	}
	return false
}

// backends lists the distinct local backends mentioned by the selector,
// in first-mention order (host terms are excluded: their slots are
// declared by external_gpu, not detected).
func (sel *Selector) backends() []string {
	var out []string
	seen := map[string]bool{}
	for _, t := range sel.terms {
		if t.host == "" && !seen[t.backend] {
			seen[t.backend] = true
			out = append(out, t.backend)
		}
	}
	return out
}

// Filter returns the slots matching a selector value; "" and "any" mean all
// slots (the scheduler's backend-agnostic admission, §2.5). Unlike the
// operator's gpu_backend selector, a job's gpu-backend requirement without
// @host is host-agnostic: transport to a remote slot is the script's
// concern, the scheduler only counts it.
func Filter(slots []Slot, selector string) ([]Slot, error) {
	v := strings.TrimSpace(selector)
	if v == "" || v == "any" {
		return append([]Slot(nil), slots...), nil
	}
	sel, err := ParseSelector(v)
	if err != nil {
		return nil, err
	}
	var out []Slot
	for _, s := range slots {
		for _, t := range sel.terms {
			if t.backend == s.Backend && (t.host == "" || t.host == s.Host) && t.ids.match(s.Index) {
				out = append(out, s)
				break
			}
		}
	}
	return out, nil
}
