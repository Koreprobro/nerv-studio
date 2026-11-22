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
	"fmt"
	"sort"
	"strings"
	"time"

	"nerv-core/internal/gpu"
)

// Status is a job's lifecycle state.
type Status string

const (
	Pending Status = "pending"
	Running Status = "running"
	Done    Status = "done"
	Failed  Status = "failed"
	// Stuck is the deadlock landing: pending jobs whose inputs can no longer
	// appear fail as stuck (a circular queue lands here too — no cycle
	// detection, SPECS §6.3).
	Stuck Status = "stuck"
)

// Job is one entry of a submitted queue. The queue file is a bare array of
// {tool, params} — no top-level envelope (the legacy top-level "gpu-count"
// key died with the inline-queue era, SPECS §4.4): scheduler-relevant values are
// ordinary params overriding the tool's declared defaults.
type Job struct {
	// Tool (required) is the script name relative to tools/ (the ".sh"
	// suffix may be omitted — the scheduler resolves it).
	Tool string `json:"tool"`
	// Params are the tool's parameters; every key must be a declared field
	// (public or hidden — Check enforces this, SPECS §6.2). Values flow to the
	// script untouched as NERV_<FIELD> env vars.
	Params map[string]any `json:"params,omitempty"`

	// Resolved records the params whose variant lists fired at launch
	// (SPECS §6.1): the queue line keeps the template, the wave stamp
	// carries what was picked. Set by ResolveVariants, omitted when empty.
	Resolved map[string]string `json:"resolved,omitempty"`

	// --- scheduler runtime state ---
	Status Status     `json:"status,omitempty"`
	Error  string     `json:"error,omitempty"`
	Slots  []gpu.Slot `json:"slots,omitempty"` // captured slots, exclusive
	Dur    Duration   `json:"dur,omitempty"`
}

// Duration renders compact wave answers ("2m27s").
type Duration struct{ time.Duration }

func (d Duration) String() string {
	if d.Duration == 0 {
		return ""
	}
	return d.Duration.Round(time.Second).String()
}

// MergeDefaults fills a queue line from a template line (the queue file's
// "defaults" block, SPECS §6.1): top-level fields apply only where the line
// is silent; params merge key-wise, the line's values winning whole (no
// splice of maps or arrays).
func (j *Job) MergeDefaults(d Job) {
	if j.Tool == "" {
		j.Tool = d.Tool
	}
	if len(d.Params) == 0 {
		return
	}
	if j.Params == nil {
		j.Params = map[string]any{}
	}
	for k, v := range d.Params {
		if _, ok := j.Params[k]; !ok {
			j.Params[k] = v
		}
	}
}

// Check validates a queue entry against the tool's declared interface and
// resolves the io-marked paths in place (params keep the normalized form —
// what flows to env must match what was checked). It returns the collected
// output and input paths, plus a skip reason ("" = valid). A skip reason is
// soft: the entry lands in skipped[{index, tool, reason}], the wave goes on
// (lenient validation, SPECS §6.2). mount names the active playground domain:
// a copied "nest/<mount>/" URL prefix folds off io paths.
func (j *Job) Check(ti *ToolInfo, mount string) (outputs, inputs []string, reason string) {
	if strings.TrimSpace(j.Tool) == "" {
		return nil, nil, "'tool' is required"
	}
	if j.Params == nil {
		j.Params = map[string]any{}
	}
	// Every param key must be a declared field — a typo cannot leak through
	// into the script as an invented env var (SPECS §6.2). Sorted for a
	// deterministic reason on multiple offenders.
	keys := make([]string, 0, len(j.Params))
	for k := range j.Params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !ti.Declared(k) {
			return nil, nil, fmt.Sprintf("undeclared field %q (see the tool interface)", k)
		}
	}
	// Materialize declared defaults for absent params: what flows to env is
	// the complete param set (defaults are part of the interface, SPECS §6.2).
	for i := range ti.Fields {
		f := &ti.Fields[i]
		if _, ok := j.Params[f.Name]; !ok && f.HasDefault {
			j.Params[f.Name] = f.Default
		}
	}
	// Io resolution: outputs must end up present, inputs may be absent
	// (optional). Values are normalized in place — params hold the canonical
	// form that flows to env.
	for i := range ti.Fields {
		f := &ti.Fields[i]
		if f.IO == "" {
			continue
		}
		v, present := j.Params[f.Name]
		if !present {
			if f.IO == "out" {
				return nil, nil, fmt.Sprintf("output field %q: no value and no default", f.Name)
			}
			continue
		}
		var collected []string
		norm, r := normalizeIO(f.Name, v, f.IO == "out", mount, &collected)
		if r != "" {
			return nil, nil, r
		}
		j.Params[f.Name] = norm
		if f.IO == "out" {
			if len(collected) == 0 {
				return nil, nil, fmt.Sprintf("output field %q: empty", f.Name)
			}
			outputs = append(outputs, collected...)
		} else {
			inputs = append(inputs, collected...)
		}
	}
	return outputs, inputs, ""
}

// normalizeIO reads one io field value and returns the normalized form
// (written back into params) plus a skip reason on shape violations. Inputs
// accept a path or an array of paths (the studio's multi-file convention,
// e.g. refs); outputs are single-valued — a tool producing several artifacts
// declares several io:"out" fields, one per file (arrays as outputs were
// cut as over-engineering).
func normalizeIO(name string, v any, isOut bool, mount string, collected *[]string) (any, string) {
	switch tv := v.(type) {
	case string:
		if tv == "" {
			return tv, ""
		}
		p := NormalizePath(tv, mount)
		if err := WithinRel(p); err != nil {
			return v, fmt.Sprintf("field %q: %v", name, err)
		}
		*collected = append(*collected, p)
		return p, ""
	case []any:
		if isOut {
			return v, fmt.Sprintf("output field %q: want a single path (declare one field per artifact)", name)
		}
		for i, e := range tv {
			sv, ok := e.(string)
			if !ok || sv == "" {
				continue
			}
			p := NormalizePath(sv, mount)
			if err := WithinRel(p); err != nil {
				return v, fmt.Sprintf("field %q: %v", name, err)
			}
			tv[i] = p // normalized element flows to env
			*collected = append(*collected, p)
		}
		return tv, ""
	}
	if isOut {
		return v, fmt.Sprintf("output field %q: want a single path", name)
	}
	return v, fmt.Sprintf("field %q: want a path or an array of paths", name)
}

// Require resolves the job's appetite: the param overrides the tool's
// declared default (the ordinary-field convention, SPECS §4.4).
func (j *Job) Require(ti *ToolInfo) int {
	if v, ok := j.Params[FieldGPURequire]; ok {
		if n, ok := asInt(v); ok && n >= 0 {
			return n
		}
	}
	return ti.Require()
}

// Backend resolves the job's backend requirement (param overrides default).
func (j *Job) Backend(ti *ToolInfo) string {
	if s, ok := j.Params[FieldGPUBackend].(string); ok && s != "" {
		return s
	}
	return ti.Backend()
}

// Pin resolves the job's slot pin selector (param overrides default;
// "" means no pin).
func (j *Job) Pin(ti *ToolInfo) string {
	if s, ok := j.Params[FieldGPUPin].(string); ok {
		return s
	}
	return ti.Pin()
}

func asInt(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	}
	return 0, false
}
