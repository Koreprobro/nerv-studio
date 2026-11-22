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
	"strings"
	"testing"
)

func expandPrompt(s string, seed any) *Job {
	j := &Job{Params: map[string]any{"prompt": s}}
	if seed != nil {
		j.Params["seed"] = seed
	}
	j.ResolveVariants()
	return j
}

// The guards keep foreign grammars whole: no "{{" pair, no pipe inside, an
// unclosed opener and the escaped \{{ all stay literal (SPECS §6.1).
func TestVariantsGuards(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"no braces at all", "no braces at all"},
		{"{{plain text}}", "{{plain text}}"},
		{`{"layout": {"style": "noir"}}`, `{"layout": {"style": "noir"}}`},
		{"bbox[54,54]", "bbox[54,54]"},
		{"unclosed {{a|b", "unclosed {{a|b"},
		{`\{{a|b}}`, "{{a|b}}"}, // escaped: the backslash goes, braces stay
	} {
		j := expandPrompt(tc.in, 7)
		if j.Params["prompt"] != tc.want {
			t.Errorf("guard case %q: got %q", tc.in, j.Params["prompt"])
		}
		if tc.in != tc.want {
			// the escape rewrites the string: the literal fire is recorded
			if j.Resolved["prompt"] != tc.want {
				t.Errorf("escaped case %q: resolved=%v", tc.in, j.Resolved)
			}
			continue
		}
		if j.Resolved != nil {
			t.Errorf("guard case %q must stay untouched: resolved=%v", tc.in, j.Resolved)
		}
	}
}

func TestVariantsPick(t *testing.T) {
	j := expandPrompt("a {{cat|dog}} b", 42)
	got := j.Params["prompt"].(string)
	if got != "a cat b" && got != "a dog b" {
		t.Fatalf("pick out of list: %q", got)
	}
	if j.Resolved["prompt"] != got {
		t.Errorf("resolved must echo the fired string: %v", j.Resolved)
	}
	if again := expandPrompt("a {{cat|dog}} b", 42); again.Params["prompt"] != got {
		t.Errorf("seeded reshoot must replay: %v vs %v", again.Params["prompt"], got)
	}
	seen := map[string]bool{}
	for seed := 0; seed < 64; seed++ {
		s := strings.TrimSuffix(strings.TrimPrefix(expandPrompt("a {{cat|dog}} b", seed).Params["prompt"].(string), "a "), " b")
		seen[s] = true
	}
	if len(seen) != 2 {
		t.Errorf("64 seeds must reach both options, saw %v", seen)
	}
	// Weights: a zero-weight option never fires.
	for seed := 0; seed < 64; seed++ {
		if got := expandPrompt("{{0::never|1::always}}", seed).Params["prompt"]; got != "always" {
			t.Fatalf("zero-weight option fired: %v", got)
		}
	}
	// An unparsable weight prefix stays option text.
	if got := expandPrompt("{{x::a|b}}", 3).Params["prompt"].(string); got != "x::a" && got != "b" {
		t.Errorf("unparsable weight must stay text: %q", got)
	}
}

func TestVariantsMultipleLists(t *testing.T) {
	const tmpl = "{{red|green}} on {{day|night}}"
	first := expandPrompt(tmpl, 5).Params["prompt"].(string)
	switch first {
	case "red on day", "red on night", "green on day", "green on night":
	default:
		t.Fatalf("bogus combo: %q", first)
	}
	if second := expandPrompt(tmpl, 5).Params["prompt"].(string); second != first {
		t.Fatalf("multi-list reshoot must replay: %q vs %q", first, second)
	}
	for seed := 0; seed < 64; seed++ {
		got := expandPrompt(tmpl, seed).Params["prompt"].(string)
		switch got {
		case "red on day", "red on night", "green on day", "green on night":
		default:
			t.Fatalf("bogus combo at seed %d: %q", seed, got)
		}
	}
	// Non-string and untouched params leave Resolved empty.
	j := &Job{Params: map[string]any{"steps": float64(30), "prompt": "plain"}}
	j.ResolveVariants()
	if j.Resolved != nil {
		t.Errorf("nothing must fire: %v", j.Resolved)
	}
}

// Seeds arrive as JSON numbers or as raw strings (the comfyui transport) —
// both must drive the same picks.
func TestVariantsSeedTypes(t *testing.T) {
	a := expandPrompt("{{a|b}}", float64(7)).Params["prompt"]
	b := expandPrompt("{{a|b}}", "7").Params["prompt"]
	if a != b {
		t.Errorf("string seed must match the numeric one: %v vs %v", a, b)
	}
}

// Picked options expand in turn, so a list may hide inside an option; the
// pipe-guard reads each group's own top level.
func TestVariantsNesting(t *testing.T) {
	const tmpl = "{{photo of {{cat|dog}}|sketch}}"
	seen := map[string]bool{}
	for seed := 0; seed < 64; seed++ {
		switch got := expandPrompt(tmpl, seed).Params["prompt"].(string); got {
		case "photo of cat", "photo of dog", "sketch":
			seen[got] = true
		default:
			t.Fatalf("bad nested pick: %q", got)
		}
	}
	if len(seen) != 3 {
		t.Errorf("64 seeds must reach all three options, saw %v", seen)
	}
	if a, b := expandPrompt(tmpl, 11), expandPrompt(tmpl, 11); a.Params["prompt"] != b.Params["prompt"] {
		t.Errorf("nested reshoot must replay: %v vs %v", a.Params["prompt"], b.Params["prompt"])
	}
	// A group without a pipe of its own stays literal whole — inner lists
	// ride along unrolled.
	lit := expandPrompt("{{plain {{a|b}} text}}", 3)
	if lit.Params["prompt"] != "{{plain {{a|b}} text}}" || lit.Resolved != nil {
		t.Errorf("no top-level pipe must stay literal: %q resolved=%v", lit.Params["prompt"], lit.Resolved)
	}
}

// Options are raw text: whole phrases roll, not only words; spaces are part
// of the option (nothing is trimmed).
func TestVariantsPhrases(t *testing.T) {
	const tmpl = `{{a cinematic wide shot, golden hour|a close portrait, shallow depth}}`
	for seed := 0; seed < 32; seed++ {
		switch got := expandPrompt(tmpl, seed).Params["prompt"].(string); got {
		case "a cinematic wide shot, golden hour", "a close portrait, shallow depth":
		default:
			t.Fatalf("phrase option mangled: %q", got)
		}
	}
	sp := expandPrompt("{{red | blue}}", 5).Params["prompt"].(string)
	if sp != "red " && sp != " blue" {
		t.Errorf("spaces belong to the option: %q", sp)
	}
}
