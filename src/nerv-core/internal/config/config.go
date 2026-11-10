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

// Package config resolves the daemon runtime configuration: a flat JSON
// config file (nerv.json at the project root, or --config) with explicitly
// passed CLI flags overriding it, over built-in defaults
// (REBUILD-nerv-core.md §2.1).
//
// Path anchoring: relative directory paths (from file or flags) are resolved
// against the project root — dirname(dirname(binary)), never the process
// cwd — so the daemon behaves identically from any launch point.
package config

import (
	"path/filepath"
	"slices"
)

// Domain is one mounted directory: its path, the command allow-list that
// defines its purpose, and the WHAT-hint shown in tools/list. Two commands
// are structural grants rather than callable ones:
//
//   - "mount"   — the domain's root is THE playground: the io path jail,
//     the queue-file location, the /artifacts HTTP mount, the wave lineage.
//     Exactly one domain must carry it; startup fails otherwise (SPECS §3.7).
//   - "execute" — the domain's root is a toolbox: queue `tool` names resolve
//     to <root>/<tool>.sh here, and probe reads the tool-info dialect.
//     Several toolboxes are legal; zero turns the studio browse-only.
type Domain struct {
	Path     string   `json:"path"`
	Commands []string `json:"commands"`
	Hint     string   `json:"hint"`
}

// Has reports whether the domain's allow-list carries the command.
func (d Domain) Has(cmd string) bool {
	return slices.Contains(d.Commands, cmd)
}

// Media holds the default media-fetch budgets for bounded content into the
// consumer context (REBUILD-nerv-core.md §2.2; the numbers inherit the
// legacy renderer defaults, SPECS.md §4.2).
type Media struct {
	MaxBlockBytes int    `json:"max_block_bytes"` // per media block budget
	MaxDim        int    `json:"max_dim"`         // image longest side, px
	VideoMaxDim   int    `json:"video_max_dim"`   // video thumbnail longest side, px
	Quality       int    `json:"quality"`         // JPEG quality, 1-100
	Format        string `json:"format"`          // image output encoding
	MaxBytes      int    `json:"max_bytes"`       // text preview cap
	Rate          int    `json:"rate"`            // audio sample rate, Hz
	Channels      int    `json:"channels"`        // audio channels
	Bitrate       string `json:"bitrate"`         // audio codec bitrate
	MaxDuration   int    `json:"max_duration"`    // audio/video preview length, s
}

// Config is the resolved runtime configuration. Domain paths are absolute
// after parsing: relative values are anchored at ProjectRoot (the daemon's
// home derived from the binary's location — never the launch cwd). There
// are no default directories: the domains map is the whole storage surface.
type Config struct {
	ProjectRoot    string
	Debug          bool   // operator debug mode; wave logs are written only with it on
	AIURL          string // OpenAI-compatible provider URL (LLM transport)
	LLMOffload     bool   // XOR VRAM: offload LLM before GPU waves (blocking SSE)
	GPUBackend     string // pool selector "cuda,rocm:0-1"; "" => greedy scoop of all detected (§2.5)
	ExternalGPU    string // remote slots "cuda@host1:2,rocm@host2" (§2.5)
	Port           int
	MaxOutputChars int
	Env            map[string]string // user env mixed into every tool launch (§2.4)
	Domains        map[string]Domain // mounted directories: path + allow-list + hint (§3.7)
	Media          Media

	// Warnings collects startup warnings for stderr (the daemon keeps no
	// logs without --debug, §2.7). Not part of the configuration itself.
	Warnings []string
}

// CallableCommands is the vocabulary of browse-mechanism commands; "mount"
// and "execute" are structural grants, everything else names an action the
// consumer can call. Unknown commands in an allow-list warn at startup.
var CallableCommands = []string{"browse", "probe", "read", "fetch", "write"}

// Mount reports the playground domain (the unique "mount"-granted one):
// its name and absolute root. It exists iff Validate passed.
func (c *Config) Mount() (string, Domain) {
	for _, name := range c.DomainNames() {
		if d := c.Domains[name]; d.Has("mount") {
			return name, d
		}
	}
	return "", Domain{}
}

// Toolboxes lists the "execute"-granted domains, sorted: each is a toolbox
// whose root resolves queue tool names and owns its execute command.
func (c *Config) Toolboxes() []string {
	var out []string
	for _, name := range c.DomainNames() {
		if c.Domains[name].Has("execute") {
			out = append(out, name)
		}
	}
	return out
}

// DomainNames returns all domain names, sorted (deterministic surface).
func (c *Config) DomainNames() []string {
	names := make([]string, 0, len(c.Domains))
	for n := range c.Domains {
		names = append(names, n)
	}
	slices.Sort(names)
	return names
}

// defaults returns the built-in configuration anchored at root. Storage has
// no defaults at all — the domains map arrives from the config file.
func defaults(root string) *Config {
	return &Config{
		ProjectRoot:    root,
		AIURL:          "http://127.0.0.1:5000/v1",
		Port:           9696,
		MaxOutputChars: 32768,
		Env:            map[string]string{},
		Domains:        map[string]Domain{},
		Media: Media{
			MaxBlockBytes: 524288,
			MaxDim:        1600,
			VideoMaxDim:   800,
			Quality:       82,
			Format:        "jpeg",
			MaxBytes:      16384,
			Rate:          16000,
			Channels:      1,
			Bitrate:       "64k",
			MaxDuration:   30,
		},
	}
}

// ProjectRoot returns the project root: the parent of the directory holding
// the running binary (the daemon lives at <root>/bin/nerv-core). If the
// executable cannot be resolved, it falls back to the current working
// directory, then ".".
func ProjectRoot() string {
	exe, err := projectExecutable()
	if err != nil {
		return "."
	}
	return filepath.Dir(filepath.Dir(exe))
}

// projectExecutable is a seam for tests.
var projectExecutable = osExecutable

// resolve anchors a relative path at the project root; absolute paths pass.
func resolve(root, p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(root, p)
}
