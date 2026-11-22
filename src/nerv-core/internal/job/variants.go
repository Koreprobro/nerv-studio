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

package job

import (
	"math/rand/v2"
	"strconv"
	"strings"
)

// ResolveVariants expands dynamic-prompt variant lists in the job's string
// params (SPECS §6.1): {{cat|dog}} picks one option, {{0.7::cat|0.3::dog}}
// picks weighted (a missing weight is 1). The sigil is the double brace plus
// a required top-level pipe — "{{plain text}}", single braces (a layout-JSON
// prompt) and the escaped \{{a|b}} stay literal. Picked options expand in
// turn, so lists nest (capped at maxVariantDepth). The picks are seeded
// from the job's declared "seed" param when present (a reshoot of the
// stamped file reproduces them
// byte for byte), from process entropy otherwise. Expanded params are
// recorded in Resolved: the queue line keeps the template, the stamp tells
// what fired.
func (j *Job) ResolveVariants() {
	var r *roller
	for k, v := range j.Params {
		s, ok := v.(string)
		if !ok || !strings.Contains(s, "{{") {
			continue
		}
		if r == nil {
			r = newRoller(j.Params["seed"])
		}
		out, changed := expandVariants(s, r)
		if !changed {
			continue
		}
		j.Params[k] = out
		if j.Resolved == nil {
			j.Resolved = map[string]string{}
		}
		j.Resolved[k] = out
	}
}

// expandVariants resolves every {{a|b}} list in s, picking one option each.
// Groups match brace-balanced and a picked option is expanded in turn, so
// lists nest; the group's own top-level pipe is the guard — a group without
// one stays literal whole, inner lists included.
func expandVariants(s string, r *roller) (string, bool) {
	return expandDepth(s, r, 0)
}

// maxVariantDepth caps the recursion into picked options: pathological
// templates stop expanding instead of eating the launch.
const maxVariantDepth = 8

func expandDepth(s string, r *roller, depth int) (string, bool) {
	if depth > maxVariantDepth || !strings.Contains(s, "{{") {
		return s, false
	}
	var b strings.Builder
	changed := false
	for i := 0; i < len(s); {
		if s[i] == '\\' && strings.HasPrefix(s[i+1:], "{{") {
			b.WriteString("{{") // escaped opener: the braces stay, literal
			changed = true      // the backslash is gone — the string changed
			i += 3
			continue
		}
		if s[i] != '{' || !strings.HasPrefix(s[i:], "{{") {
			b.WriteByte(s[i])
			i++
			continue
		}
		closer, ok := findCloser(s, i)
		if !ok {
			b.WriteString(s[i:]) // unclosed: the rest rides verbatim
			return b.String(), changed
		}
		segments := splitTopPipes(s[i+2 : closer])
		if len(segments) == 1 {
			b.WriteString(s[i : closer+2]) // pipe-guard: no pipe of its own, literal whole
			i = closer + 2
			continue
		}
		opts, weights := parseOptions(segments)
		picked, _ := expandDepth(opts[r.pick(weights)], r, depth+1) // nesting: the pick may hold lists too
		b.WriteString(picked)
		changed = true
		i = closer + 2
	}
	return b.String(), changed
}

// findCloser returns the index of the "}}" closing the group that opens at
// i, brace-balanced and escape-aware; ok=false when the group never closes.
func findCloser(s string, i int) (int, bool) {
	depth := 1
	for j := i + 2; j < len(s); {
		switch {
		case s[j] == '\\' && strings.HasPrefix(s[j+1:], "{{"):
			j += 3
		case strings.HasPrefix(s[j:], "{{"):
			depth++
			j += 2
		case strings.HasPrefix(s[j:], "}}"):
			if depth--; depth == 0 {
				return j, true
			}
			j += 2
		default:
			j++
		}
	}
	return 0, false
}

// splitTopPipes cuts the group content on the "|" that sit at brace depth 0
// (pipes inside a nested list belong to that list).
func splitTopPipes(content string) []string {
	var segs []string
	depth, start := 0, 0
	for j := 0; j < len(content); {
		switch {
		case content[j] == '\\' && strings.HasPrefix(content[j+1:], "{{"):
			j += 3
		case strings.HasPrefix(content[j:], "{{"):
			depth++
			j += 2
		case strings.HasPrefix(content[j:], "}}"):
			if depth > 0 {
				depth--
			}
			j += 2
		case content[j] == '|' && depth == 0:
			segs = append(segs, content[start:j])
			start = j + 1
			j++
		default:
			j++
		}
	}
	return append(segs, content[start:])
}

// parseOptions reads the optional "w::" weight prefixes off the segments
// (default 1; unparsable or negative prefixes stay option text).
func parseOptions(segments []string) ([]string, []float64) {
	opts := make([]string, len(segments))
	weights := make([]float64, len(segments))
	for i, p := range segments {
		opts[i], weights[i] = p, 1
		if j := strings.Index(p, "::"); j >= 0 {
			if f, err := strconv.ParseFloat(strings.TrimSpace(p[:j]), 64); err == nil && f >= 0 {
				opts[i], weights[i] = p[j+2:], f
			}
		}
	}
	return opts, weights
}

// roller picks variant options: the same job seed replays the same picks on
// every launch, xorshift64 keeps the seeded path out of the rand globals.
type roller struct {
	s uint64
}

func newRoller(seed any) *roller {
	s, ok := seedBits(seed)
	if !ok {
		s = rand.Uint64() // process entropy: every launch picks afresh
	}
	return &roller{s: s*0x9E3779B97F4A7C15 ^ 0xA5A5A5A5A5A5A5A5 | 1}
}

// seedBits reads a declared seed param (JSON numbers arrive as float64,
// strings ride the raw-string transports like comfyui's).
func seedBits(seed any) (uint64, bool) {
	switch v := seed.(type) {
	case float64:
		return uint64(int64(v)), true
	case int:
		return uint64(v), true
	case int64:
		return uint64(v), true
	case string:
		if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
			return uint64(int64(f)), true
		}
	}
	return 0, false
}

func (r *roller) next() uint64 {
	r.s ^= r.s << 13
	r.s ^= r.s >> 7
	r.s ^= r.s << 17
	return r.s
}

// pick returns the chosen option index on the weight line.
func (r *roller) pick(weights []float64) int {
	total := 0.0
	for _, w := range weights {
		total += w
	}
	if total <= 0 {
		return 0
	}
	t := float64(r.next()>>11) / float64(1<<53) * total
	for i, w := range weights {
		t -= w
		if t < 0 {
			return i
		}
	}
	return len(weights) - 1
}
