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
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"nerv-core/internal/browse"
)

// callDomain runs one domain command over the shared browse mechanism.
// The config allow-list gates the command; fullRawAccess opens everything
// that the mechanism knows (§2.2). On an "execute"-granted domain the
// probe command (and a bare call) speaks the tool-info dialect (§3.7).
func (s *Server) callDomain(name string, args map[string]any) ToolResult {
	raw := optBool(args, "fullRawAccess")
	d := s.domains[name]
	toolbox := hasAny(s.Cfg.Domains[name].Commands, "execute")
	cmd := asString(args, "command")
	if cmd == "" || cmd == "ls" {
		if toolbox {
			cmd = "probe" // the toolbox door is tool-info, never a listing
		} else {
			cmd = "browse"
		}
	}
	if !raw && !hasAny(s.Cfg.Domains[name].Commands, cmd) {
		return toolError("command %q is not enabled for %s; available: %s",
			cmd, name, strings.Join(allowedVisible(s.Cfg.Domains[name].Commands), ", "))
	}
	if cmd == "probe" && toolbox {
		return s.probeTool(name, args)
	}
	path := asString(args, "path")
	switch cmd {
	case "browse":
		node, err := d.Ls(path, optBool(args, "recursive"))
		if err != nil {
			return toolError("%v", err)
		}
		return jsonResult(node)
	case "probe":
		if path == "" {
			return toolError("probe needs a path")
		}
		meta, err := d.Probe(path)
		if err != nil {
			return toolError("%v", err)
		}
		return jsonResult(meta)
	case "read":
		if path == "" {
			return toolError("read needs a path")
		}
		tx, err := d.Read(path, asString(args, "lines"), asString(args, "bytes"), readMaxBytes(args))
		if err != nil {
			return toolError("%v", err)
		}
		out := tx.Content
		if tx.Note != "" {
			out += "\n[" + tx.Note + "]"
		}
		return textResult(out)
	case "fetch":
		if path == "" {
			return toolError("fetch needs a path")
		}
		return s.fetchMedia(name, d, path, args)
	case "write":
		if path == "" {
			return toolError("write needs a path")
		}
		if strings.HasSuffix(path, "/") {
			dir, err := d.Mkdir(path)
			if err != nil {
				return toolError("%v", err)
			}
			return textResult("created " + dir)
		}
		rel, err := d.WriteText(path, asString(args, "content"), optBool(args, "append"))
		if err != nil {
			return toolError("%v", err)
		}
		out := "wrote " + rel
		if name == s.Mount {
			out += " (artifactURL: " + artifactURL(rel) + ")"
		}
		return textResult(out)
	}
	return toolError("unknown %s command: %q", name, cmd)
}

func (s *Server) fetchMedia(name string, d *browse.Domain, path string, args map[string]any) ToolResult {
	med, err := d.Fetch(path, fetchOpts(args))
	if err != nil {
		return toolError("%v", err)
	}
	if len(med.Data) == 0 {
		return textResult(med.Note)
	}
	block := imageBlock
	if strings.HasPrefix(med.Mime, "audio/") {
		block = audioBlock
	}
	res := ToolResult{Content: []ToolContent{block(base64.StdEncoding.EncodeToString(med.Data), med.Mime)}}
	note := ""
	if name == s.Mount {
		note = "artifactURL: " + artifactURL(path)
	}
	if med.Degraded {
		if note != "" {
			note += " "
		}
		note += "[degraded into budget]"
	}
	if note != "" {
		res.Content = append(res.Content, textBlock(note))
	}
	return res
}

func fetchOpts(args map[string]any) browse.FetchOpts {
	var o browse.FetchOpts
	o.Format = asString(args, "format")
	o.Seek = asString(args, "seek")
	o.Bitrate = asString(args, "bitrate")
	o.MaxDim, _ = optInt(args, "max_dim")
	o.Quality, _ = optInt(args, "quality")
	o.MaxBlockBytes, _ = optInt(args, "max_block_bytes")
	o.MaxDuration, _ = optInt(args, "max_duration")
	o.Rate, _ = optInt(args, "rate")
	o.Channels, _ = optInt(args, "channels")
	return o
}

func readMaxBytes(args map[string]any) int {
	if v, ok := optInt(args, "max_bytes"); ok {
		return v
	}
	return 0 // budget default
}

func jsonResult(v any) ToolResult {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return toolError("%v", err)
	}
	return textResult(string(b))
}

func textResult(s string) ToolResult {
	return ToolResult{Content: []ToolContent{textBlock(s)}}
}

func toolError(format string, a ...any) ToolResult {
	return ToolResult{Content: []ToolContent{textBlock(fmt.Sprintf(format, a...))}, IsError: true}
}
