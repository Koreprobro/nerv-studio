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
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"nerv-core/internal/config"
)

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
// deeper tag (SPECS §3.6). The system temp dir gets the <tmp> tag: the
// daemon's transient wave logs live there and can surface in answers.
func buildRedactions(cfg *config.Config) []rootTag {
	out := make([]rootTag, 0, len(cfg.Domains)+2)
	if cfg.ProjectRoot != "" {
		out = append(out, rootTag{filepath.Clean(cfg.ProjectRoot), "<project>"})
	}
	if tmp := filepath.Clean(os.TempDir()); tmp != "" && tmp != "/" {
		out = append(out, rootTag{tmp, "<tmp>"})
	}
	for name, d := range cfg.Domains {
		out = append(out, rootTag{filepath.Clean(d.Path), "<" + name + ">"})
	}
	sort.Slice(out, func(i, j int) bool { return len(out[i].root) > len(out[j].root) })
	return out
}

// strayAbsPath matches a standalone absolute path token of two or more
// segments: the leading delimiter excludes URLs ("http://host:9696/art"
// — ':' and '/' are not start characters), relative "a/b/c" paths (a
// letter never starts the token) and the tails of tagged paths
// ("<kb>/a/b.md" — '>' is not a start character either). One-segment
// tokens ("/etc") stay: too ambiguous to be a path in prose.
var strayAbsPath = regexp.MustCompile(`(^|[^0-9A-Za-z_./:\\~>-])((?:/[A-Za-z0-9._+~-]+){2,}/?)`)

// scrubStrayPaths is the jail rule of the redactor (SPECS §3.6): with the
// mounted roots already tagged, any remaining absolute token collapses to
// <path>/basename — the consumer-AI world knows relative paths and root
// tags only; full server paths survive exclusively in debug artifacts.
// /nest/ URLs are the studio's own space and pass through untouched.
func scrubStrayPaths(msg string) string {
	return strayAbsPath.ReplaceAllStringFunc(msg, func(m string) string {
		head := 0
		if m[0] != '/' {
			head = 1 // the match carries its leading delimiter
		}
		token := m[head:]
		if strings.HasPrefix(token, "/nest/") {
			return m
		}
		trailing := strings.HasSuffix(token, "/")
		_, base := path.Split(strings.TrimSuffix(token, "/"))
		if base == "" {
			return m[:head] + "<path>"
		}
		if trailing {
			return m[:head] + "<path>/" + base + "/" // keep the dir-ness visible
		}
		return m[:head] + "<path>/" + base
	})
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
	return scrubStrayPaths(msg)
}

// redactResult maps server-absolute paths inside the text blocks of a
// tool result to their root tags. Redaction is enforced at the response
// boundary (SPECS §3.6): every answer reaching the client — error text,
// file content (a stamped queue file carries wave-debug tails quoting
// absolute paths), wave blocks, progress lines — passes through here, so
// no per-site error formatter can smuggle a raw root in. Media blocks
// are base64 and pass through untouched.
func (s *Server) redactResult(res ToolResult) ToolResult {
	for i := range res.Content {
		if res.Content[i].Type == "text" {
			res.Content[i].Text = s.redact(res.Content[i].Text)
		}
	}
	return res
}
