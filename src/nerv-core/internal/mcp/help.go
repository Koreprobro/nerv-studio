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
	"fmt"
	"strings"

	"nerv-core/internal/config"
)

// help is the always-available self-documentation door of every domain
// (SPECS §3.3): no path lists the commands this domain answers to, a method
// name as path explains that one method. The texts tell the consumer-AI
// WHAT a method does and how to address it; HOW the daemon implements it is
// noise in a creative context and stays out.

type cmdDoc struct {
	summary string // one line for the domain listing
	full    string // the method's full info
}

// helpDocs is the WHAT-only registry; the code stays the source of truth,
// the listing is generated through the domain's actual grants.
var helpDocs = map[string]cmdDoc{
	"browse": {
		summary: "see what a folder holds — names and sizes",
		full: `browse — see what a folder holds.
  path — the folder to look at; omit it for the root of the domain. A file path answers with that file's size.
  recursive=true — the whole subtree (bounded) instead of one level.`,
	},
	"probe": {
		summary: "learn what a file is, without opening its content",
		full: `probe — what a file is, without its content.
  path — the file (required). The answer carries its size and type, and whatever the type reveals: dimensions for pictures, duration and shape for sound and video.`,
	},
	"read": {
		summary: "read a text file — whole or in a window",
		full: `read — the text door.
  path — the file (required).
  lines="A-B" — a window of lines (counted from 1, either side optional); bytes="A-B" — a window of bytes. The two windows exclude each other.
  max_bytes — cap on the returned text.
  Media is not text: a picture or a sound is sent away to fetch.`,
	},
	"fetch": {
		summary: "perceive a media file — picture, listening clip, video frame — within the context budget",
		full: `fetch — the media door: perceive a media file without flooding the context.
  path — the media file (required). What comes back: a picture, a short listening clip, or a video frame.
  seek — where the clip begins, in seconds ("12") or clock form ("00:00:12"); max_duration — how long the clip may run.
  format, max_dim, quality — how the picture looks; rate, channels, bitrate — how the sound arrives; max_block_bytes — cap on the returned block.
  Plain text is not media: it is sent away to read.`,
	},
	"write": {
		summary: "put a text file down — or make a folder",
		full: `write — put a text file down.
  path — the file (required); a trailing "/" makes a folder instead.
  content — the text; append=true extends the file instead of replacing it.`,
	},
	"execute": {
		summary: "run a wave of jobs from a queue file and wait for its answer",
		full: `execute — run a wave of jobs through the toolbox.
  queue — a queue file in the playground: a JSON array of jobs [{"tool": ..., "params": {...}}] or the {"defaults": ..., "jobs": [...]} envelope. A line with a top-level "defaults" key is a scoped directive: it merges into the lines that follow, within this file only; a later declaration wins per key, the job line wins over every defaults. "_comment" rides along at any level — ignored by the scheduler, kept by the stamp. Several files separated by "," merge into ONE wave. A {{a|b}} list inside a string param picks one option at launch (weighted: {{0.7::a|0.3::b}}); the job's seed replays the pick, the stamp records what fired; backslash+{{ escapes.
  wave="latest" or a wave id — look up a finished wave instead of launching a new one.
  The wave's answer is stamped back into the queue file; relaunching a stamped file reshoots it.`,
	},
	"check": {
		summary: "rehearse a wave — the same queue, validated but not launched",
		full: `check — rehearse a wave before firing it.
  queue — the same queue file execute takes (required). Nothing launches; you get the verdict on the plan.`,
	},
	"help": {
		summary: "this manual — a method name as path explains one method",
		full: `help — the domain explains its own methods.
  No path — the commands this domain answers to. A method name as path — everything about that one method.`,
	},
}

// helpProbeToolbox replaces the probe entry on an execute-granted domain:
// there probe speaks the tool-info dialect, not the file sniff.
var helpProbeToolbox = cmdDoc{
	summary: "read a tool's card — what the tool does and what it takes",
	full: `probe — a tool's card (this domain is a toolbox).
  path — the tool's name, with or without ".sh"; omit it to see the whole toolbox. The answer carries the tool's summary and the parameters it accepts, with their defaults.`,
}

// helpCommands lists what `help` shows and what `path="<method>"` accepts:
// the visible allow-list, the callable companion of the execute grant, and
// help itself (deduped when the operator listed it).
func helpCommands(d config.Domain) []string {
	cmds := allowedVisible(d.Commands)
	if hasAny(d.Commands, "execute") {
		cmds = append(cmds, "check")
	}
	if !hasAny(d.Commands, "help") {
		cmds = append(cmds, "help")
	}
	return cmds
}

func helpDocFor(cmd string, toolbox bool) (cmdDoc, bool) {
	if cmd == "probe" && toolbox {
		return helpProbeToolbox, true
	}
	doc, ok := helpDocs[cmd]
	return doc, ok
}

// domainHelp answers the always-available help command.
func (s *Server) domainHelp(name string, method string) ToolResult {
	d := s.Cfg.Domains[name]
	toolbox := hasAny(d.Commands, "execute")
	cmds := helpCommands(d)
	if method == "" {
		var b strings.Builder
		fmt.Fprintf(&b, "%s — commands:\n", name)
		for _, c := range cmds {
			if doc, ok := helpDocFor(c, toolbox); ok {
				fmt.Fprintf(&b, "  %s — %s\n", c, doc.summary)
			}
		}
		return textResult(strings.TrimRight(b.String(), "\n"))
	}
	for _, c := range cmds {
		if c == method {
			doc, _ := helpDocFor(c, toolbox)
			return textResult(doc.full)
		}
	}
	return toolError("help: %s has no method %q; available: %s",
		name, method, strings.Join(cmds, ", "))
}
