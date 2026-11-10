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
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"nerv-core/internal/config"
	"nerv-core/internal/gpu"
)

// slotView is one line of the gpu.list diagnostic.
type slotView struct {
	Slot  string `json:"slot"`
	Local bool   `json:"local"`
	Load  string `json:"load,omitempty"`
}

// callGPUList is the hidden operator diagnostic: pool slots with a live
// load read where the backend supports it.
func (s *Server) callGPUList(_ map[string]any) ToolResult {
	if s.Pool == nil || len(s.Pool.Slots()) == 0 {
		return textResult("gpu pool: empty (no backend detected, no external slots)")
	}
	var out []slotView
	for _, sl := range s.Pool.Slots() {
		v := slotView{Slot: sl.String(), Local: sl.Local()}
		if sl.Local() && sl.Backend == "cuda" {
			if kb, err := gpu.QueryLoad(sl); err == nil {
				v.Load = strconv.FormatUint(kb/1024, 10) + " MiB"
			}
		}
		out = append(out, v)
	}
	return textResult(strings.Join(renderSlots(out), "\n"))
}

func renderSlots(vs []slotView) []string {
	out := make([]string, 0, len(vs))
	for _, v := range vs {
		line := v.Slot
		if !v.Local {
			line += " (external)"
		}
		if v.Load != "" {
			line += " load " + v.Load
		}
		out = append(out, line)
	}
	return out
}

// guardVolume caps the TEXT surface (max_output_chars); media blocks carry
// their own budget. Over the cap the answer is refused with the escape
// hatch revealed: re-send with volume_ack=true — a deliberate two-step
// dump, never an accident.
func (s *Server) guardVolume(res ToolResult, args map[string]any) ToolResult {
	if optBool(args, "volume_ack") {
		return res
	}
	limit := s.Cfg.MaxOutputChars
	if limit <= 0 {
		return res
	}
	n := 0
	for _, c := range res.Content {
		if c.Type == "text" {
			n += len(c.Text)
		}
	}
	if n <= limit {
		return res
	}
	head := resText(res)
	if len(head) > limit {
		head = head[:limit]
	}
	return ToolResult{
		Content: []ToolContent{textBlock(head +
			"\n… [volume guard: " + strconv.Itoa(n) + " text chars > max_output_chars " +
			strconv.Itoa(limit) + "; re-send with volume_ack=true for the full dump]")},
		IsError: true,
	}
}

func resText(res ToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if c.Type == "text" {
			b.WriteString(c.Text)
		}
	}
	return b.String()
}

// rootTag maps one server root to its display tag.
type rootTag struct{ root, tag string }

// buildRedactions maps the project root and every mounted domain root to a
// <name> tag, longest root first — a nested root must redact to its own,
// deeper tag (SPECS §3.7).
func buildRedactions(cfg *config.Config) []rootTag {
	out := make([]rootTag, 0, len(cfg.Domains)+1)
	if cfg.ProjectRoot != "" {
		out = append(out, rootTag{filepath.Clean(cfg.ProjectRoot), "<project>"})
	}
	for name, d := range cfg.Domains {
		out = append(out, rootTag{filepath.Clean(d.Path), "<" + name + ">"})
	}
	sort.Slice(out, func(i, j int) bool { return len(out[i].root) > len(out[j].root) })
	return out
}

// redact strips server-absolute paths from anything the client sees.
func (s *Server) redact(msg string) string {
	for _, r := range s.redactions {
		if r.root == "" {
			continue
		}
		msg = strings.ReplaceAll(msg, r.root+"/", r.tag+"/")
		msg = strings.ReplaceAll(msg, r.root, r.tag)
	}
	return msg
}
