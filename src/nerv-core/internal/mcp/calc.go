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
	"strings"

	"nerv-core/internal/resolution"
)

// callCalcResolution answers the calc.resolution utility: an aspect string
// in, generator-friendly WxH out. Pure arithmetic — no volume budget, no GPU.
// The studio defaults (multiple 32, 2 MP target, sides 512..1536) are what
// the image tools expect on their required width/height fields.
func (s *Server) callCalcResolution(args map[string]any) ToolResult {
	aspect := strings.TrimSpace(asString(args, "aspect"))
	if aspect == "" {
		return toolError(`calc.resolution: aspect is required (e.g. "16:9", "1024x768", "1.5")`)
	}
	spec := resolution.Studio()
	if v, ok := jsonInt64(args["multiple"]); ok && v > 0 {
		spec.Multiple = int(v)
	}
	if v, ok, err := jsonFloat(args["targetMP"]); ok && err == nil && v > 0 {
		spec.Target = int(v * 1_000_000)
	}
	if v, ok := jsonInt64(args["minSide"]); ok && v > 0 {
		spec.MinSide = int(v)
	}
	if v, ok := jsonInt64(args["maxSide"]); ok && v > 0 {
		spec.MaxSide = int(v)
	}
	w, h, err := resolution.Compute(aspect, spec)
	if err != nil {
		return toolError("%v", err)
	}
	return jsonResult(map[string]any{
		"aspect":   aspect,
		"width":    w,
		"height":   h,
		"pixels":   w * h,
		"multiple": spec.Multiple,
	})
}

func jsonFloat(v any) (float64, bool, error) {
	switch n := v.(type) {
	case float64:
		return n, true, nil
	case int:
		return float64(n), true, nil
	case int64:
		return float64(n), true, nil
	case nil:
		return 0, false, nil
	}
	return 0, false, &badArg{"targetMP"}
}

type badArg struct{ name string }

func (e *badArg) Error() string { return "calc.resolution: " + e.name + " must be a number" }
