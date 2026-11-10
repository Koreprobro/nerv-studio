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

// Package llm is a thin OpenAI-protocol client for the VRAM XOR
// (REBUILD §2.5): before a wave with gpu-require > 0 the daemon washes out
// the chat model through the provider (list loaded via /v1/models, then
// POST /models/unload). Best-effort by contract: the caller swallows
// errors, the wave still runs. Reload is the chat frontend's business,
// not the scheduler's.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// DefaultBaseURL is the local llama-server convention, used when the
// config leaves ai_url empty.
const DefaultBaseURL = "http://127.0.0.1:5000/v1"

// Client talks to an OpenAI-compatible provider (default: llama-server).
type Client struct {
	BaseURL string // e.g. http://127.0.0.1:5000/v1
	HTTP    *http.Client
}

// New returns a Client; an empty aiURL falls back to DefaultBaseURL.
func New(aiURL string) *Client {
	if aiURL == "" {
		aiURL = DefaultBaseURL
	}
	return &Client{BaseURL: strings.TrimSuffix(aiURL, "/"), HTTP: &http.Client{Timeout: 30 * time.Second}}
}

// Loaded lists the ids of models currently loaded in the provider.
func (c *Client) Loaded(ctx context.Context) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/models", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var doc struct {
		Data []struct {
			ID     string `json:"id"`
			Status struct {
				Value string `json:"value"`
			} `json:"status"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return nil, fmt.Errorf("parse /models: %w", err)
	}
	var out []string
	for _, m := range doc.Data {
		if m.Status.Value == "loaded" {
			out = append(out, m.ID)
		}
	}
	return out, nil
}

// Unload washes every loaded model out of VRAM. llama.cpp exposes model
// management at the server root (/models/unload), not under the /v1
// OpenAI prefix — the suffix is stripped to reach it. Errors are the
// caller's concern (best-effort XOR).
func (c *Client) Unload(ctx context.Context) ([]string, error) {
	loaded, err := c.Loaded(ctx)
	if err != nil || len(loaded) == 0 {
		return nil, err
	}
	root := strings.TrimSuffix(c.BaseURL, "/v1")
	var unloaded []string
	for _, id := range loaded {
		body, _ := json.Marshal(map[string]string{"model_name": id, "model": id})
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, root+"/models/unload", bytes.NewReader(body))
		if err != nil {
			return unloaded, err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := c.HTTP.Do(req)
		if err != nil {
			return unloaded, err
		}
		resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return unloaded, fmt.Errorf("unload %q: provider status %d", id, resp.StatusCode)
		}
		unloaded = append(unloaded, id)
	}
	return unloaded, nil
}
