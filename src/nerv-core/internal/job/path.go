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

// URLPrefix is the HTTP file mount name (GET /nest/<domain>/<rel>, SPECS §3.1).
// It is not a directory of any domain — NormalizePath folds it away together
// with the active mount domain's name segment.
const URLPrefix = "nest"

// NormalizePath folds a copied artifactURL ("nest/<mount>/<rel>") back to the
// canonical playground-relative form and cleans the path. The leniency is
// decided and kept (SPECS §3.1): consumers copy URL text out of answers and
// hand it back; the canonical stored/served form carries neither the prefix
// nor the domain segment, so dual names never coexist silently. A URL of a
// foreign domain ("nest/<other>/…") does not fold — it reads as an ordinary
// playground path and fails on existence (files of other domains enter queues
// only via copy, SPECS §3.7).
func NormalizePath(p, mount string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	if mount != "" {
		if rest, ok := strings.CutPrefix(p, URLPrefix+"/"+mount+"/"); ok && rest != "" {
			p = rest
		}
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
