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
	"nerv-core/internal/gpu"
	"nerv-core/internal/job"
	"nerv-core/internal/scheduler"
)

// probeTool answers `<toolbox> probe <name>`: summary + fields (hidden
// entries drop silently unless fullRawAccess) + the computed
// minimal-capacity (§2.5 — the planning number for the consumer, mechanics
// hidden). The toolbox root comes from the domain the call arrived on.
func (s *Server) probeTool(domain string, args map[string]any) ToolResult {
	name := asString(args, "path")
	if name == "" {
		name = asString(args, "tool")
	}
	if name == "" {
		return toolError("%s probe needs a tool name in path", domain)
	}
	root := s.domains[domain].Root
	tool, err := scheduler.ResolveTool(root, name)
	if err != nil {
		return toolError("%v", err)
	}
	ti, err := scheduler.ProbeTool(root, tool)
	if err != nil {
		return toolError("%v", err)
	}
	fields := ti.Fields
	if !optBool(args, "fullRawAccess") {
		fields = ti.PublicFields()
	}
	view := struct {
		Tool            string      `json:"tool"`
		Summary         string      `json:"summary"`
		Fields          []job.Field `json:"fields"`
		MinimalCapacity any         `json:"minimal_capacity"`
	}{Tool: tool, Summary: ti.Summary, Fields: fields, MinimalCapacity: s.minimalCapacity(ti)}
	return jsonResult(view)
}

// minimalCapacity = eligible slots (pin set, or backend filter, or the
// minimum across pool backends for gpu-backend "any") / gpu-require, floor 1;
// CPU tools are unlimited; a poolless daemon cannot launch GPU tools.
func (s *Server) minimalCapacity(ti *job.ToolInfo) any {
	n := ti.Require()
	if n == 0 {
		return "unlimited"
	}
	if s.Pool == nil {
		return 0
	}
	eligible := s.Pool.Slots()
	if pin := ti.Pin(); pin != "" {
		set, err := s.Pool.PinSet(pin)
		if err != nil {
			return 0 // a nail through the wall: nothing to seat
		}
		return max(1, len(set)/n)
	}
	if be := ti.Backend(); be != "" && be != "any" {
		set, err := gpu.Filter(eligible, be)
		if err != nil {
			return 0
		}
		return max(1, len(set)/n)
	}
	return max(1, s.Pool.MinCount()/n)
}
