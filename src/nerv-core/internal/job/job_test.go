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
	"encoding/json"
	"strings"
	"testing"
)

const genInfo = `{
  "summary": "test tool",
  "fields": [
    {"name": "prompt", "required": true, "description": "text"},
    {"name": "output", "io": "out", "description": "written file"},
    {"name": "refs", "io": "in", "description": "input files"},
    {"name": "seed", "default": 42},
    {"name": "steps", "default": 20},
    {"name": "gpu-require", "default": 1},
    {"name": "gpu-backend", "default": "cuda"},
    {"name": "gpu-pin", "default": "0", "hidden": true},
    {"name": "cfg", "default": "x", "hidden": true}
  ]
}`

func parse(t *testing.T, s string) *ToolInfo {
	t.Helper()
	ti, err := ParseToolInfo([]byte(s))
	if err != nil {
		t.Fatalf("ParseToolInfo: %v", err)
	}
	return ti
}

func TestParseGrammar(t *testing.T) {
	ti := parse(t, genInfo)
	if got := len(ti.PublicFields()); got != 7 {
		t.Errorf("public fields: want 7 (two hidden dropped), got %d", got)
	}
	if ti.Require() != 1 || ti.Backend() != "cuda" || ti.Pin() != "0" {
		t.Errorf("scheduler fields: %d %q %q", ti.Require(), ti.Backend(), ti.Pin())
	}
	if !ti.Declared("cfg") {
		t.Error("hidden field must still be declared (values pass through)")
	}
	for _, bad := range []string{
		`{"fields":[{"description":"no name"}]}`,
		`{"fields":[{"name":"a"},{"name":"a"}]}`,
		`{"fields":[{"name":"a","io":"InOut"}]}`,
		`{"fields":[{"name":"a","hidden":true}]}`,
	} {
		if _, err := ParseToolInfo([]byte(bad)); err == nil {
			t.Errorf("want error for %s", bad)
		}
	}
	// Legacy shape (m13-echo): tolerated — unknown attributes ignored, the
	// top-level "gpu-count" key is simply not a field (reads as 0).
	legacy := parse(t, `{"summary":"echo","fields":[{"name":"text","required":true},{"name":"cfg","required":false,"private":true,"default":7.0}],"gpu-count":0}`)
	if legacy.Require() != 0 || !legacy.Declared("cfg") {
		t.Error("legacy tool-info must parse under the lenient grammar")
	}
}

// The core injects NERV_CALCULATED_WIDTH/HEIGHT for calculator-declared
// tools; a field shadowing the pair (hyphen or underscore spelling) is a
// reserved-name conflict, rejected at parse time.
func TestReservedFieldNames(t *testing.T) {
	for _, bad := range []string{
		`{"fields":[{"name":"calculated-width"}]}`,
		`{"fields":[{"name":"CALCULATED_HEIGHT"}]}`,
	} {
		if _, err := ParseToolInfo([]byte(bad)); err == nil {
			t.Errorf("want reserved-name error for %s", bad)
		}
	}
	if _, err := ParseToolInfo([]byte(`{"fields":[{"name":"width-calc"},{"name":"calc-width"}]}`)); err != nil {
		t.Errorf("lookalike field names must pass: %v", err)
	}
}

func TestCheckResolvesIO(t *testing.T) {
	ti := parse(t, genInfo)
	j := &Job{Tool: "t.sh", Params: map[string]any{
		"prompt": "hi",
		"output": "artifacts/scene/shot.png",
		"refs":   []any{"a.png", "artifacts/b.png", ""},
		"cfg":    "operator value", // hidden param passes validation
	}}
	outs, ins, reason := j.Check(ti)
	if reason != "" {
		t.Fatalf("reason: %q", reason)
	}
	if strings.Join(outs, ",") != "scene/shot.png" {
		t.Errorf("outputs: %v", outs)
	}
	if strings.Join(ins, ",") != "a.png,b.png" {
		t.Errorf("inputs: %v", ins)
	}
	if j.Params["output"] != "scene/shot.png" {
		t.Errorf("params must hold the normalized form (env flows from them): %v", j.Params["output"])
	}
	if j.Params["seed"].(float64) != 42 {
		t.Errorf("missing param with default must materialize: %v", j.Params["seed"])
	}
	var qs []Job
	if err := json.Unmarshal([]byte(`[{"tool":"a.sh","params":{"x":1}},{"tool":"b"}]`), &qs); err != nil {
		t.Fatal(err)
	}
	if len(qs) != 2 || qs[1].Tool != "b" {
		t.Fatalf("bare-array queue: %+v", qs)
	}
}

func TestCheckSkipReasons(t *testing.T) {
	ti := parse(t, genInfo)
	cases := []struct {
		name   string
		params map[string]any
		want   string
	}{
		{"undeclared key", map[string]any{"prompt": "x", "output": "o.png", "promt": "typo"}, "promt"},
		{"missing output", map[string]any{"prompt": "x"}, "output"},
		{"empty output", map[string]any{"prompt": "x", "output": ""}, "empty"},
		{"absolute output", map[string]any{"prompt": "x", "output": "/etc/passwd"}, "absolute"},
		{"array output", map[string]any{"prompt": "x", "output": []any{"a.png", "b.png"}}, "single path"},
		{"escaping ref", map[string]any{"prompt": "x", "output": "o.png", "refs": []any{"../x"}}, "escapes"},
	}
	for _, c := range cases {
		j := &Job{Tool: "t.sh", Params: c.params}
		_, _, reason := j.Check(ti)
		if reason == "" || !strings.Contains(reason, c.want) {
			t.Errorf("%s: want reason containing %q, got %q", c.name, c.want, reason)
		}
	}
	// Optional input absent is fine.
	if _, _, reason := (&Job{Tool: "t.sh", Params: map[string]any{"prompt": "x", "output": "o.png"}}).Check(ti); reason != "" {
		t.Errorf("absent optional input: %q", reason)
	}
}

func TestPerJobOverrides(t *testing.T) {
	ti := parse(t, genInfo)
	j := &Job{Tool: "t.sh", Params: map[string]any{
		"gpu-require": float64(2), "gpu-backend": "rocm", "gpu-pin": "@farm:1",
	}}
	if j.Require(ti) != 2 || j.Backend(ti) != "rocm" || j.Pin(ti) != "@farm:1" {
		t.Errorf("params must override declared defaults: %d %q %q", j.Require(ti), j.Backend(ti), j.Pin(ti))
	}
	cpu := &Job{Tool: "t.sh", Params: map[string]any{"gpu-require": float64(0)}}
	if cpu.Require(ti) != 0 {
		t.Error("explicit gpu-require 0 (CPU run) must override the tool default")
	}
}

func TestPaths(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"artifacts/a/b.png", "a/b.png"},
		{"a/b.png", "a/b.png"},
		{"artifacts/artifacts/x", "artifacts/x"}, // one-way fold only
		{"dir//x.png", "dir/x.png"},
		{" ./x ", "x"},
	} {
		if got := NormalizePath(c.in); got != c.want {
			t.Errorf("NormalizePath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	for _, bad := range []string{"", "/abs/x", "../up", "a/../../up"} {
		if err := WithinRel(NormalizePath(bad)); err == nil {
			t.Errorf("WithinRel(%q) must reject", bad)
		}
	}
}
