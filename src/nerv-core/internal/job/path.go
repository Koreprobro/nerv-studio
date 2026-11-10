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

package job

import (
	"errors"
	"path/filepath"
	"strings"
)

// URLMount is the HTTP mount name of the artifactory (GET /artifacts/<rel>).
// It is not a directory of the artifactory root — NormalizePath folds it away.
const URLMount = "artifacts"

// NormalizePath folds the URL-mount prefix and cleans the path to the
// canonical artifactory-relative form. The leniency is decided and kept
// (REBUILD §4.5): consumers copy "artifacts/..." out of artifactURL and hand
// it back; the canonical stored/served form never carries the prefix, so
// dual names never coexist silently — one-way fold, tested here.
func NormalizePath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	if t := strings.TrimPrefix(p, URLMount+"/"); t != p && t != "" {
		p = t
	}
	return filepath.ToSlash(filepath.Clean(p))
}

// WithinRel rejects paths that escape the root: absolute paths and anything
// reaching above it via "..". Applied to every io path on submit (the
// traversal guard the audit demanded of the old Build, kept verbatim).
func WithinRel(rel string) error {
	if rel == "" {
		return errors.New("empty path")
	}
	if strings.HasPrefix(rel, "/") || filepath.IsAbs(rel) {
		return errors.New("absolute path not allowed (artifactory-relative expected)")
	}
	clean := filepath.ToSlash(filepath.Clean(rel))
	if clean == "." {
		return errors.New("empty path")
	}
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return errors.New("path escapes the artifactory root")
	}
	return nil
}
