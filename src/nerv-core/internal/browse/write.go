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

package browse

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// textExts is the allowlist of the `write` command. Deliberately
// conservative: executable / binary formats (.sh, .py, .bin, .exe, .so)
// are rejected — the artifactory stores artifacts, not programs.
var textExts = map[string]struct{}{
	".md": {}, ".markdown": {}, ".txt": {}, ".text": {}, ".textproto": {},
	".json": {}, ".jsonl": {}, ".ndjson": {},
	".yaml": {}, ".yml": {}, ".toml": {}, ".ini": {}, ".cfg": {}, ".conf": {},
	".csv": {}, ".tsv": {}, ".tab": {},
	".xml": {}, ".html": {}, ".htm": {}, ".css": {}, ".scss": {}, ".less": {},
	".log": {}, ".env": {}, ".gitattributes": {}, ".gitignore": {},
	".rst": {}, ".adoc": {}, ".org": {}, ".nerv": {},
}

// Mkdir creates a directory under the root, trailing-slash tolerant
// ("proj/" and "proj" name the same directory). Repeating an existing
// directory is not an error; landing on an existing FILE is. Returns the
// directory path relative to the root, with a trailing slash.
func (d *Domain) Mkdir(path string) (string, error) {
	trimmed := strings.TrimSuffix(strings.TrimSpace(path), "/")
	if trimmed == "" {
		return "", fmt.Errorf("%s: path must name a directory: %q", d.Name, path)
	}
	abs, err := d.Resolve(trimmed)
	if err != nil {
		return "", err
	}
	if info, err := os.Stat(abs); err == nil && !info.IsDir() {
		return "", fmt.Errorf("%q is a file, not a directory", trimmed)
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return "", err
	}
	return filepath.ToSlash(filepath.Clean(trimmed)) + "/", nil
}

// WriteText creates, overwrites, or appends a text file under the root.
// Only allowlisted text extensions are accepted (an empty extension is
// rejected: name the type). Returns the path relative to the root.
func (d *Domain) WriteText(path, content string, appendMode bool) (string, error) {
	if path == "" {
		return "", fmt.Errorf("%s: path is required", d.Name)
	}
	rel := filepath.ToSlash(filepath.Clean(path))
	if rel == "." || rel == "/" || strings.HasSuffix(path, "/") {
		return "", fmt.Errorf("%s: path must be a file (directories end with /): %q", d.Name, path)
	}
	ext := filepath.Ext(path)
	if _, ok := textExts[strings.ToLower(ext)]; !ok {
		return "", fmt.Errorf("disallowed file type (write text files only): %q", ext)
	}
	abs, err := d.Resolve(path)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return "", err
	}
	flag := os.O_CREATE | os.O_TRUNC | os.O_WRONLY
	if appendMode {
		flag = os.O_CREATE | os.O_APPEND | os.O_WRONLY
	}
	f, err := os.OpenFile(abs, flag, 0o644)
	if err != nil {
		return "", err
	}
	if _, err := f.WriteString(content); err != nil {
		f.Close()
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	return rel, nil
}
