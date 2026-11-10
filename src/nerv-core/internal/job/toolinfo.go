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

// Package job models a queue entry, the tool's self-declared interface
// (--tool-info) and the validation rules binding the two together
// (REBUILD-nerv-core.md §2.3, §2.4).
package job

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Reserved field names: scheduler-relevant ordinary fields declared by the
// tool with a default (the ordinary-field convention, §2.4). Values in job
// params override the defaults per job. The gpu trio is resolved by the
// scheduler itself and never reaches the script env (SPECS §5.1); the
// calculator pair triggers NERV_CALCULATED_WIDTH/HEIGHT injection (§7.3).
const (
	FieldGPURequire  = "gpu-require"
	FieldGPUBackend  = "gpu-backend"
	FieldGPUPin      = "gpu-pin"
	FieldAspectRatio = "aspect-ratio"
	FieldTargetMPX   = "target-mpx"
	BackendAny       = "any"
)

// SchedulerFields are consumed by the scheduler at admission; their param
// values are the job's answer and the script needs no echo of them (the
// pin env is the device truth, SPECS §5.1).
var SchedulerFields = map[string]bool{
	FieldGPURequire: true, FieldGPUBackend: true, FieldGPUPin: true,
}

// ReservedEnvNames are core-injected service env vars a declared field must
// not shadow (SPECS §4.5). Under the slim contract (§5.1) the directory
// roots and the scheduler fields left the launch env — the calculator pair
// is all that remains, and this check keeps the namespace clean.
var ReservedEnvNames = map[string]bool{
	"CALCULATED_WIDTH":  true,
	"CALCULATED_HEIGHT": true,
}

// EnvName renders the env var a field flows as: NERV_ prefix, uppercased,
// hyphens folded to underscores (env names must be valid shell identifiers).
func EnvName(field string) string { return "NERV_" + NormEnvField(field) }

// NormEnvField folds a field name into its env suffix form: uppercased,
// hyphens to underscores. Reserved-name checks and calculator triggers
// compare in this normalized form (aspect-ratio == aspect_ratio).
func NormEnvField(field string) string {
	return strings.ToUpper(strings.ReplaceAll(field, "-", "_"))
}

// Field is one declared parameter of a tool. IO marks the dataflow role
// (REBUILD §2.4): "in" — input file(s) the scheduler waits for; "out" —
// output file(s) whose existence after exit 0 marks the job done. Hidden
// entries are dropped from the consumer's probe view silently (entry absent
// — no marker, no count) but their values pass through untouched.
type Field struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Required    bool   `json:"required,omitempty"`
	Default     any    `json:"default,omitempty"`
	IO          string `json:"io,omitempty"`
	Hidden      bool   `json:"hidden,omitempty"`

	// HasDefault reports an explicitly declared default (JSON null counts
	// as absent). A hidden field must have one (§2.4).
	HasDefault bool `json:"-"`
}

// ToolInfo is the self-declared interface of a tool, read from
// `sh <tool> --tool-info`. Scheduler-relevant numbers live as ordinary
// fields with defaults (gpu-require / gpu-backend / gpu-pin), not as a
// separate envelope level.
type ToolInfo struct {
	Summary string  `json:"summary"`
	Fields  []Field `json:"fields"`

	byName map[string]*Field
}

// ParseToolInfo decodes and validates the --tool-info grammar: named, unique
// fields; io is "in"/"out" or absent; a hidden field carries a default.
// Unknown attributes are tolerated (forward compatibility for tool authors).
func ParseToolInfo(data []byte) (*ToolInfo, error) {
	var raw struct {
		Summary string `json:"summary"`
		Fields  []struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			Required    bool            `json:"required"`
			Default     json.RawMessage `json:"default"`
			IO          string          `json:"io"`
			Hidden      bool            `json:"hidden"`
		} `json:"fields"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("tool-info: %w", err)
	}
	ti := &ToolInfo{Summary: raw.Summary, byName: make(map[string]*Field, len(raw.Fields))}
	ti.Fields = make([]Field, 0, len(raw.Fields))
	seen := make(map[string]bool, len(raw.Fields))
	for _, rf := range raw.Fields {
		if rf.Name == "" {
			return nil, fmt.Errorf("tool-info: field without name")
		}
		if rf.IO != "" && rf.IO != "in" && rf.IO != "out" {
			return nil, fmt.Errorf("tool-info: field %q: io must be \"in\" or \"out\"", rf.Name)
		}
		if seen[rf.Name] {
			return nil, fmt.Errorf("tool-info: duplicate field %q", rf.Name)
		}
		if ReservedEnvNames[NormEnvField(rf.Name)] {
			return nil, fmt.Errorf("tool-info: field %q collides with a reserved service env name", rf.Name)
		}
		seen[rf.Name] = true
		f := Field{Name: rf.Name, Description: rf.Description, Required: rf.Required, IO: rf.IO, Hidden: rf.Hidden}
		if len(rf.Default) > 0 && string(rf.Default) != "null" {
			if err := json.Unmarshal(rf.Default, &f.Default); err != nil {
				return nil, fmt.Errorf("tool-info: field %q: default: %w", rf.Name, err)
			}
			f.HasDefault = true
		}
		if f.Hidden && !f.HasDefault {
			return nil, fmt.Errorf("tool-info: hidden field %q must declare a default", rf.Name)
		}
		ti.Fields = append(ti.Fields, f)
	}
	for i := range ti.Fields {
		ti.byName[ti.Fields[i].Name] = &ti.Fields[i]
	}
	return ti, nil
}

// Declared reports whether the tool declares the param name (public or
// hidden — hidden is a visibility marker, not an input filter).
func (ti *ToolInfo) Declared(name string) bool {
	_, ok := ti.byName[name]
	return ok
}

// Field returns a declared field by name.
func (ti *ToolInfo) Field(name string) (*Field, bool) {
	f, ok := ti.byName[name]
	return f, ok
}

// PublicFields returns the consumer-facing view: hidden entries dropped
// silently — the consumer never learns anything was hidden (§2.4).
func (ti *ToolInfo) PublicFields() []Field {
	out := make([]Field, 0, len(ti.Fields))
	for _, f := range ti.Fields {
		if !f.Hidden {
			out = append(out, f)
		}
	}
	return out
}

func (ti *ToolInfo) defaultString(name string) (string, bool) {
	f, ok := ti.byName[name]
	if !ok || !f.HasDefault {
		return "", false
	}
	s, ok := f.Default.(string)
	return s, ok
}

func (ti *ToolInfo) defaultInt(name string) (int, bool) {
	f, ok := ti.byName[name]
	if !ok || !f.HasDefault {
		return 0, false
	}
	n, ok := f.Default.(float64) // JSON numbers decode as float64
	if !ok {
		return 0, false
	}
	return int(n), true
}

// Require is the tool's declared appetite for GPU slots — counted in pool
// slots, not physical cards (default 0 = CPU).
func (ti *ToolInfo) Require() int {
	n, _ := ti.defaultInt(FieldGPURequire)
	if n < 0 {
		return 0
	}
	return n
}

// Backend is the declared backend ("cuda"|"rocm"|"intel"|...) — "any" when
// the tool does not declare one (§2.5).
func (ti *ToolInfo) Backend() string {
	if s, ok := ti.defaultString(FieldGPUBackend); ok && s != "" {
		return s
	}
	return BackendAny
}

// Pin is the tool's declared slot pin selector ("" = no pin, §2.5).
func (ti *ToolInfo) Pin() string {
	s, _ := ti.defaultString(FieldGPUPin)
	return s
}
