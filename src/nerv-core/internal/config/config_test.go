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
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func hasWarning(cfg *Config, substr string) bool {
	for _, w := range cfg.Warnings {
		if strings.Contains(w, substr) {
			return true
		}
	}
	return false
}

// minimal.json is the smallest legal storage config: one playground plus a
// toolbox — used by every test that only exercises the non-storage keys.
const minimalJSON = `{
	"domains": {
		"artifactory": { "path": "art", "commands": ["read", "mount"] },
		"tools":       { "path": "tools", "commands": ["probe", "execute"] }
	}
}`

func TestNoDomainsIsFatal(t *testing.T) {
	if _, err := ParseWithRoot(nil, "/root"); err == nil {
		t.Fatal("a daemon without configured domains must refuse to start")
	}
}

func TestMinimalFile(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "nerv.json"), minimalJSON)
	cfg, err := ParseWithRoot(nil, root)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Port != 9696 || cfg.MaxOutputChars != 32768 {
		t.Errorf("port/max = %d/%d, want 9696/32768", cfg.Port, cfg.MaxOutputChars)
	}
	name, mount := cfg.Mount()
	if name != "artifactory" || mount.Path != filepath.Join(root, "art") {
		t.Errorf("mount = %q %q, want artifactory anchored at root", name, mount.Path)
	}
	if tb := cfg.Toolboxes(); len(tb) != 1 || tb[0] != "tools" {
		t.Errorf("toolboxes = %v, want [tools]", tb)
	}
	if cfg.Debug {
		t.Error("debug must default off (flag-only)")
	}
	if cfg.Media.Quality != 82 || cfg.Media.MaxDim != 1600 || cfg.Media.MaxBlockBytes != 524288 {
		t.Errorf("media defaults wrong: %+v", cfg.Media)
	}
	if len(cfg.Env) != 0 || len(cfg.Warnings) != 0 {
		t.Errorf("clean minimal: env=%v warnings=%v", cfg.Env, cfg.Warnings)
	}
}

func TestDomainPathsAndHints(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "nerv.json"), `{
		"domains": {
			"art":    { "path": "/abs/playground", "commands": ["read", "mount"], "hint": "the playground" },
			"kb":     { "path": "knowledge", "commands": ["read", "probe"] },
			"toolbox":{ "path": "tools", "commands": ["probe", "execute"], "hint": "sh toolbox" }
		}
	}`)
	cfg, err := ParseWithRoot(nil, root)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Domains["art"].Path != "/abs/playground" {
		t.Errorf("absolute path must pass through: %q", cfg.Domains["art"].Path)
	}
	if cfg.Domains["kb"].Path != filepath.Join(root, "knowledge") {
		t.Errorf("relative path must anchor at root: %q", cfg.Domains["kb"].Path)
	}
	if cfg.Domains["toolbox"].Hint != "sh toolbox" || cfg.Domains["art"].Hint != "the playground" {
		t.Errorf("hints lost: %+v", cfg.Domains)
	}
	if name, _ := cfg.Mount(); name != "art" {
		t.Errorf("mount = %q, want art", name)
	}
}

func TestMountExactlyOne(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "nerv.json"), `{
		"domains": { "a": { "path": "a", "commands": ["read"] }, "b": { "path": "b", "commands": ["read"] } }
	}`)
	if _, err := ParseWithRoot(nil, root); err == nil || !strings.Contains(err.Error(), "mount") {
		t.Errorf("no-mount config must fail with a mount error: %v", err)
	}
	mustWrite(t, filepath.Join(root, "nerv.json"), `{
		"domains": { "a": { "path": "a", "commands": ["read", "mount"] }, "b": { "path": "b", "commands": ["mount"] } }
	}`)
	if _, err := ParseWithRoot(nil, root); err == nil || !strings.Contains(err.Error(), "2 domains") {
		t.Errorf("double-mount config must fail: %v", err)
	}
}

func TestDomainNameAndPathRules(t *testing.T) {
	root := t.TempDir()
	cases := []struct{ body, want string }{
		{`{"domains": {"art": {"path": "", "commands": ["mount"]}}}`, "path"},
		{`{"domains": {"bad name": {"path": "x", "commands": ["mount"]}}}`, "name"},
		{`{"domains": {"gpu.list": {"path": "x", "commands": ["mount"]}}}`, "collides"},
	}
	for _, c := range cases {
		mustWrite(t, filepath.Join(root, "nerv.json"), c.body)
		_, err := ParseWithRoot(nil, root)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("config %s: want error about %q, got %v", c.body, c.want, err)
		}
	}
}

func TestUnknownCommandWarns(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "nerv.json"), `{
		"domains": { "art": { "path": "art", "commands": ["mount", "skate"] } }
	}`)
	cfg, err := ParseWithRoot(nil, root)
	if err != nil {
		t.Fatal(err)
	}
	if !hasWarning(cfg, "skate") {
		t.Errorf("unknown command must warn: %v", cfg.Warnings)
	}
}

func TestFileAppliesOverDefaults(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "nerv.json"), `{
		"_comment": "studio box",
		"port": 7000,
		"media": {"quality": 70},
		"env": {"HF_HOME": "/ai/hf", "NERV_X": "drop"},
		"domains": {
			"artifactory": { "path": "art", "commands": ["browse", "mount"] },
			"kb": { "path": "kb", "commands": ["read"] }
		},
		"bogus": 1
	}`)
	cfg, err := ParseWithRoot(nil, root)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Port != 7000 {
		t.Errorf("file value not applied: port=%d", cfg.Port)
	}
	if cfg.Debug {
		t.Error("debug is flag-only: the file key must not enable it")
	}
	if cfg.Media.Quality != 70 || cfg.Media.MaxDim != 1600 {
		t.Errorf("media merge wrong: %+v", cfg.Media)
	}
	if cfg.Env["HF_HOME"] != "/ai/hf" {
		t.Errorf("env not applied: %v", cfg.Env)
	}
	if _, ok := cfg.Env["NERV_X"]; ok {
		t.Error("NERV_ key must be rejected from user env")
	}
	if !hasWarning(cfg, "NERV_X") {
		t.Errorf("NERV_ rejection must warn: %v", cfg.Warnings)
	}
	if !hasWarning(cfg, "bogus") {
		t.Errorf("unknown key must warn: %v", cfg.Warnings)
	}
	if !slices.Equal(cfg.Domains["kb"].Commands, []string{"read"}) {
		t.Errorf("kb allow-list not replaced: %v", cfg.Domains["kb"].Commands)
	}
}

func TestFlagOverridesFile(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "nerv.json"), `{"port": 7000,
		"domains": {"art": {"path": "art", "commands": ["mount"]}}}`)
	cfg, err := ParseWithRoot([]string{"-port", "8123", "-debug"}, root)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Port != 8123 {
		t.Errorf("flag must beat file: port=%d", cfg.Port)
	}
	if !cfg.Debug {
		t.Error("-debug flag must land")
	}
}

func TestEnvFlagRepeatedAndRejected(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "nerv.json"), minimalJSON)
	cfg, err := ParseWithRoot([]string{"-env", "A=1", "-env", "B=2", "-env", "NERV_BAD=9", "-env", "oops"}, root)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Env["A"] != "1" || cfg.Env["B"] != "2" {
		t.Errorf("--env pairs lost: %v", cfg.Env)
	}
	if _, ok := cfg.Env["NERV_BAD"]; ok {
		t.Error("NERV_BAD must be rejected")
	}
	if !hasWarning(cfg, "NERV_BAD") || !hasWarning(cfg, "oops") {
		t.Errorf("expected warnings for NERV_BAD and oops: %v", cfg.Warnings)
	}
}

func TestExplicitConfigMissingIsFatal(t *testing.T) {
	if _, err := ParseWithRoot([]string{"-config", "/nope/nerv.json"}, "/root"); err == nil {
		t.Fatal("explicit missing config file must be an error")
	}
}

func TestBadJSONIsFatal(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "nerv.json"), `{oops`)
	if _, err := ParseWithRoot(nil, root); err == nil {
		t.Fatal("malformed config must be an error")
	}
}

func TestPortRange(t *testing.T) {
	if _, err := ParseWithRoot([]string{"-port", "0"}, "/root"); err == nil {
		t.Fatal("port 0 must be an error")
	}
}
