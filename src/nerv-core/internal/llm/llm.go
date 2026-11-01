// Package llm is a thin OpenAI-protocol client used for LLM offload — the
// VRAM XOR (SPECS.md §3.3): it lists the models currently loaded via the
// provider API and unloads them to free VRAM for diffusion models.
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

// Client talks to an OpenAI-compatible provider (default: local llama-server).
type Client struct {
	BaseURL string // e.g. http://127.0.0.1:5000/v1
	Model   string // model name; empty lets the provider use its default
	HTTP    *http.Client
}

// New returns a Client. A nil baseURL defaults to a local provider.
func New(baseURL, model string) *Client {
	if baseURL == "" {
		baseURL = "http://127.0.0.1:5000/v1"
	}
	return &Client{
		BaseURL: baseURL,
		Model:   model,
		HTTP:    &http.Client{Timeout: 10 * time.Minute},
	}
}

// Unload asks the provider to free the LLM from VRAM so diffusion can use it
// (the VRAM XOR, SPECS.md §3.3). It finds the currently-loaded model via
// /v1/models and unloads it through the provider's /models/unload endpoint — this
// llama-server build exposes /models/unload (not /v1/unload, which 404s).
// Best-effort: a missing endpoint or error is logged and swallowed so the wave
// can still run.
func (c *Client) Unload(ctx context.Context) error {
	// Find the currently-loaded model via /v1/models (BaseURL already ends in /v1).
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/models", nil)
	if err != nil {
		return err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	var models struct {
		Data []struct {
			ID     string `json:"id"`
			Status struct {
				Value string `json:"value"`
			} `json:"status"`
		} `json:"data"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&models)
	resp.Body.Close()
	var loaded string
	for _, m := range models.Data {
		if m.Status.Value == "loaded" {
			loaded = m.ID
			break
		}
	}
	if loaded == "" {
		return nil // nothing is loaded, nothing to unload
	}

	// Unload via /models/unload. llama.cpp exposes model management at the server
	// root (/models/unload), NOT under the /v1 OpenAI prefix — so strip the
	// trailing /v1 from the base URL to reach it.
	root := c.BaseURL
	if strings.HasSuffix(root, "/v1") {
		root = strings.TrimSuffix(root, "/v1")
	}
	body, _ := json.Marshal(map[string]any{"model": loaded})
	req, err = http.NewRequestWithContext(ctx, http.MethodPost, root+"/models/unload", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err = c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("unload: provider status %d", resp.StatusCode)
	}
	return nil
}
