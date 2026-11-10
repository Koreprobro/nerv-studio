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

package mcp

import (
	"sort"
	"strings"
)

// toolDef is one entry of tools/list.
type toolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

// fallbackHints: built-in one-liners for the fixed utilities. Domain tool
// descriptions arrive from the config "hint" field (§3.7).
var fallbackHints = map[string]string{
	"dice.roll":       "dice roller — notation + optional replayable seed",
	"calc.resolution": "resolution calculator — aspect in, generator WxH out",
	"gpu.list":        "gpu pool diagnostics (hidden tool)",
}

// toolsList is the config-driven surface (SPECS §3.7): every mounted
// directory is a tool, sorted by name, then the utilities; gpu.list stays
// hidden.
func (s *Server) toolsList() []toolDef {
	names := s.Cfg.DomainNames()
	out := make([]toolDef, 0, len(names)+2)
	for _, name := range names {
		out = append(out, toolDef{Name: name, Description: s.hint(name), InputSchema: s.domainSchema(name)})
	}
	out = append(out,
		toolDef{Name: "dice.roll", Description: s.hint("dice.roll"), InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"notation": map[string]any{"type": "string", "description": "dice expression: 2d6, d100, 4d6kh3+2, 2d8+1d4-1"},
				"seed":     map[string]any{"type": "integer", "description": "optional; makes the roll deterministic and replayable"},
			},
			"required": []string{"notation"},
		}},
		toolDef{Name: "calc.resolution", Description: s.hint("calc.resolution"), InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"aspect":   map[string]any{"type": "string", "description": `aspect: "16:9", "1024x768" or a decimal factor like "1.5"`},
				"multiple": map[string]any{"type": "integer", "description": "side granularity in px (default 32)"},
				"targetMP": map[string]any{"type": "number", "description": "pixel target in megapixels (default 2)"},
				"minSide":  map[string]any{"type": "integer", "description": "smallest usable side; bounds the area to minSide^2 (default 512)"},
				"maxSide":  map[string]any{"type": "integer", "description": "largest area as maxSide^2 pixels (default 1536)"},
			},
			"required": []string{"aspect"},
		}})
	return out
}

// hint is the tool description: the domain's config hint, a built-in
// one-liner for the utilities, or a generic command summary.
func (s *Server) hint(name string) string {
	if d, ok := s.Cfg.Domains[name]; ok {
		if d.Hint != "" {
			return d.Hint
		}
		return name + " — mounted directory: " + strings.Join(allowedVisible(d.Commands), ", ")
	}
	return fallbackHints[name]
}

// domainSchema: navigation params only — budgets default from the config
// media section, the bag of knobs stays out of the schema. An
// "execute"-granted domain additionally speaks the queue/wave pair.
func (s *Server) domainSchema(name string) map[string]any {
	d := s.Cfg.Domains[name]
	props := map[string]any{
		"command": map[string]any{"type": "string", "enum": allowedVisible(d.Commands),
			"description": "domain command (allow-list is config-driven)"},
		"path":      map[string]any{"type": "string", "description": name + "-relative path; omit for the root"},
		"recursive": map[string]any{"type": "boolean", "description": "browse: bounded tree instead of one level"},
	}
	if hasAny(d.Commands, "read") {
		props["lines"] = map[string]any{"type": "string", "description": `read: line range "A-B" (1-based inclusive, sides optional)`}
		props["bytes"] = map[string]any{"type": "string", "description": `read: byte range "A-B" (mutually exclusive with lines)`}
		props["max_bytes"] = map[string]any{"type": "integer", "description": "read: preview cap in bytes (config media.max_bytes)"}
	}
	if hasAny(d.Commands, "write") {
		props["content"] = map[string]any{"type": "string", "description": "write: file text (directories end with /)"}
		props["append"] = map[string]any{"type": "boolean", "description": "write: append instead of truncate"}
	}
	if hasAny(d.Commands, "execute") {
		props["queue"] = map[string]any{"type": "string", "description": `playground path to the queue file — a bare JSON array [{"tool": "...", "params": {...}}]; the launch input of {command: "execute"}`}
		props["wave"] = map[string]any{"type": "string", "description": `result lookup: "latest" or a wave id`}
	}
	return map[string]any{"type": "object", "properties": props}
}

func hasAny(commands []string, want ...string) bool {
	for _, c := range commands {
		for _, w := range want {
			if c == w {
				return true
			}
		}
	}
	return false
}

// allowedVisible lists the allow-list for the schema and error messages:
// sorted, minus "mount" — the structural grant is a property, not a command.
func allowedVisible(commands []string) []string {
	out := make([]string, 0, len(commands))
	for _, c := range commands {
		if c != "mount" {
			out = append(out, c)
		}
	}
	sort.Strings(out)
	return out
}

func artifactURL(rel string) string { return "/artifacts/" + strings.TrimPrefix(rel, "/") }
