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
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// applyFile applies a flat JSON config file over the current values.
// The "_comment" key is allowed (strict JSON carries comments this way);
// unknown keys warn. Malformed JSON is an error.
func (c *Config) applyFile(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("config %s: %w", filepath.Base(path), err)
	}
	base := filepath.Base(path)
	keys := make([]string, 0, len(raw))
	for k := range raw {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := raw[k]
		var e error
		switch k {
		case "_comment":
		case "ai_url":
			e = json.Unmarshal(v, &c.AIURL)
		case "llm_offload":
			e = json.Unmarshal(v, &c.LLMOffload)
		case "gpu_backend":
			e = json.Unmarshal(v, &c.GPUBackend)
		case "external_gpu":
			e = json.Unmarshal(v, &c.ExternalGPU)
		case "port":
			e = json.Unmarshal(v, &c.Port)
		case "max_output_chars":
			e = json.Unmarshal(v, &c.MaxOutputChars)
		case "env":
			e = json.Unmarshal(v, &c.Env) // merges into the existing map
		case "domains":
			e = c.applyDomains(v)
		case "media":
			m := c.Media
			e = json.Unmarshal(v, &m) // absent fields keep defaults
			c.Media = m
		default:
			c.warnf("config %s: unknown key %q ignored", base, k)
		}
		if e != nil {
			return fmt.Errorf("config %s: key %q: %w", base, k, e)
		}
	}
	return nil
}

// applyDomains merges the mounted directories: a named domain REPLACES the
// whole entry (path, commands, hint); relative paths anchor at the project
// root — the daemon's home derived from the binary, never the launch cwd.
func (c *Config) applyDomains(v json.RawMessage) error {
	var d map[string]Domain
	if err := json.Unmarshal(v, &d); err != nil {
		return err
	}
	names := make([]string, 0, len(d))
	for name := range d {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		ent := d[name]
		if ent.Path != "" {
			ent.Path = resolve(c.ProjectRoot, ent.Path)
		}
		c.Domains[name] = ent
	}
	return nil
}

func (c *Config) warnf(format string, a ...any) {
	c.Warnings = append(c.Warnings, fmt.Sprintf(format, a...))
}
