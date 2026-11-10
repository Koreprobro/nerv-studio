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

package config

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// osExecutable is the runtime seam used by ProjectRoot.
func osExecutable() (string, error) { return os.Executable() }

// envFlag collects repeated --env KEY=VALUE flags.
type envFlag struct{ pairs []string }

func (e *envFlag) String() string { return strings.Join(e.pairs, ",") }
func (e *envFlag) Set(v string) error {
	e.pairs = append(e.pairs, v)
	return nil
}

// normalizeArgs rewrites hyphenated CLI flag names to their canonical
// underscore form (--ai-url => --ai_url, --llm-offload=true =>
// --llm_offload=true). The deployed unit and operator tooling pass hyphens,
// while the config file (nerv.json) uses underscores; this lets both survive.
// Values are never rewritten: a --flag=value token is split at '=', and a
// value passed as the next token never starts with '--', so it is left alone.
func normalizeArgs(args []string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		dashes := 0
		for dashes < 2 && dashes < len(a) && a[dashes] == '-' {
			dashes++
		}
		if dashes > 0 {
			name := a[dashes:]
			eq := strings.IndexByte(name, '=')
			namePart := name
			if eq >= 0 {
				namePart = name[:eq]
			}
			if namePart != "" && strings.Contains(namePart, "-") {
				suffix := ""
				if eq >= 0 {
					suffix = name[eq:]
				}
				out[i] = strings.Repeat("-", dashes) + strings.ReplaceAll(namePart, "-", "_") + suffix
				continue
			}
		}
		out[i] = a
	}
	return out
}

// Parse resolves args (without the program name) into a Config:
// built-in defaults < config file < explicitly passed flags (§2.1).
// A malformed/unreadable config file is fatal; a missing default-path file
// is fine. Unknown config keys only warn (typo trap for AI-authored files).
func Parse(args []string) (*Config, error) {
	return ParseWithRoot(args, ProjectRoot())
}

// ParseWithRoot is Parse with an explicit project root (test seam).
func ParseWithRoot(args []string, root string) (*Config, error) {
	cfg := defaults(root)

	fs := flag.NewFlagSet("nerv-core", flag.ContinueOnError)
	configPath := fs.String("config", "", "config file (default <project-root>/nerv.json when present)")
	debug := fs.Bool("debug", false, "operator debug mode: wave logs written beside the queue file")
	aiURL := fs.String("ai_url", "", "OpenAI-compatible provider URL (LLM transport)")
	llmOffload := fs.Bool("llm_offload", false, "XOR VRAM: offload LLM before GPU waves (blocking SSE)")
	gpuBackend := fs.String("gpu_backend", "", "GPU pool selector, e.g. cuda,rocm:0-1 (empty => greedy scoop)")
	externalGPU := fs.String("external_gpu", "", "remote GPU slots, e.g. cuda@host1:2,rocm@host2")
	port := fs.Int("port", 0, "MCP streamable HTTP port")
	maxOut := fs.Int("max_output_chars", 0, "volume guard: max chars of tool-result text")
	var env envFlag
	fs.Var(&env, "env", "extra env var KEY=VALUE for every tool launch (repeatable)")
	if err := fs.Parse(normalizeArgs(args)); err != nil {
		return nil, err
	}

	// Config file: the explicit path must exist; the default path is optional.
	path := *configPath
	if path == "" {
		p := filepath.Join(root, "nerv.json")
		if _, err := os.Stat(p); err == nil {
			path = p
		}
	} else if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("config file: %w", err)
	}
	if path != "" {
		if err := cfg.applyFile(path); err != nil {
			return nil, err
		}
	}

	// Explicit flags override the file (fs.Visit lists only set flags).
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "debug":
			cfg.Debug = *debug
		case "ai_url":
			cfg.AIURL = *aiURL
		case "llm_offload":
			cfg.LLMOffload = *llmOffload
		case "gpu_backend":
			cfg.GPUBackend = *gpuBackend
		case "external_gpu":
			cfg.ExternalGPU = *externalGPU
		case "port":
			cfg.Port = *port
		case "max_output_chars":
			cfg.MaxOutputChars = *maxOut
		}
	})
	for _, pair := range env.pairs {
		k, v, ok := strings.Cut(pair, "=")
		if !ok || k == "" {
			cfg.warnf("--env %q ignored (want KEY=VALUE)", pair)
			continue
		}
		cfg.Env[k] = v
	}

	// The NERV_ prefix belongs to the scheduler<->sh contract (§2.4):
	// user env keys carrying it are rejected at startup, never silently.
	var rejected []string
	for k := range cfg.Env {
		if strings.HasPrefix(k, "NERV_") {
			delete(cfg.Env, k)
			rejected = append(rejected, k)
		}
	}
	slices.Sort(rejected)
	for _, k := range rejected {
		cfg.warnf("env key %q rejected: the NERV_ prefix belongs to the scheduler<->sh contract", k)
	}

	if cfg.Port < 1 || cfg.Port > 65535 {
		return nil, fmt.Errorf("port %d out of range", cfg.Port)
	}
	if err := cfg.validateDomains(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// utilityToolNames are the non-domain surface names a domain must not take.
var utilityToolNames = map[string]bool{"dice.roll": true, "calc.resolution": true, "gpu.list": true}

// validateDomains enforces the storage contract (SPECS §3.7): exactly one
// domain carries the structural "mount" grant — the playground every
// scheduler file operation is anchored at; names must be legal MCP tool
// names and must not collide with the utilities; paths must be non-empty;
// unknown commands only warn (typo trap, the surface ignores them).
func (c *Config) validateDomains() error {
	if len(c.Domains) == 0 {
		return fmt.Errorf("config: no domains configured (the \"domains\" map is the whole storage surface)")
	}
	mounts := 0
	for _, name := range c.DomainNames() {
		d := c.Domains[name]
		if !validToolName(name) {
			return fmt.Errorf("domain %q: name must match [A-Za-z0-9_.-]+ (1-64 chars)", name)
		}
		if utilityToolNames[name] {
			return fmt.Errorf("domain %q: name collides with a utility tool", name)
		}
		if d.Path == "" {
			return fmt.Errorf("domain %q: \"path\" is required", name)
		}
		if d.Has("mount") {
			mounts++
		}
		for _, cmd := range d.Commands {
			if cmd == "mount" || cmd == "execute" {
				continue // structural grants
			}
			if !slices.Contains(CallableCommands, cmd) {
				c.warnf("domain %q: unknown command %q ignored", name, cmd)
			}
		}
	}
	if mounts == 0 {
		return fmt.Errorf(`config: no domain has the "mount" property — exactly one playground directory is required (SPECS §3.7)`)
	}
	if mounts > 1 {
		return fmt.Errorf(`config: %d domains claim the "mount" property — exactly one playground directory is allowed`, mounts)
	}
	return nil
}

// validToolName is the MCP tool-name grammar the surface requires.
func validToolName(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, r := range s {
		ok := r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' ||
			r == '_' || r == '-' || r == '.'
		if !ok {
			return false
		}
	}
	return true
}
