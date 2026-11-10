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

package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func fakeProvider(t *testing.T, modelsJSON string, unloadStatus int, unloaded *[]string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(modelsJSON))
	})
	mux.HandleFunc("/models/unload", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		*unloaded = append(*unloaded, body["model_name"])
		w.WriteHeader(unloadStatus)
	})
	return httptest.NewServer(mux)
}

func TestUnloadWashesLoaded(t *testing.T) {
	var got []string
	srv := fakeProvider(t, `{"data":[{"id":"llama-a","status":{"value":"loaded"}},{"id":"idle-b","status":{"value":"stopped"}}]}`, 200, &got)
	defer srv.Close()
	c := New(srv.URL + "/v1")
	unloaded, err := c.Unload(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(unloaded) != 1 || unloaded[0] != "llama-a" {
		t.Errorf("unloaded: %v", unloaded)
	}
	if len(got) != 1 || got[0] != "llama-a" {
		t.Errorf("provider saw: %v", got)
	}
}

func TestUnloadNothingLoaded(t *testing.T) {
	var got []string
	srv := fakeProvider(t, `{"data":[{"id":"idle-b","status":{"value":"stopped"}}]}`, 200, &got)
	defer srv.Close()
	c := New(srv.URL + "/v1")
	unloaded, err := c.Unload(context.Background())
	if err != nil || unloaded != nil {
		t.Errorf("nothing-loaded: %v %v", unloaded, err)
	}
	if len(got) != 0 {
		t.Error("unload must not be called when nothing is loaded")
	}
}

func TestUnloadProviderError(t *testing.T) {
	var got []string
	srv := fakeProvider(t, `{"data":[{"id":"llama-a","status":{"value":"loaded"}}]}`, 500, &got)
	defer srv.Close()
	c := New(srv.URL + "/v1")
	if _, err := c.Unload(context.Background()); err == nil {
		t.Error("500 status must surface as error (caller swallows it)")
	}
}

func TestLoadedErrorPath(t *testing.T) {
	c := New("http://127.0.0.1:1/v1") // nothing listens there
	if _, err := c.Loaded(context.Background()); err == nil {
		t.Error("dead provider must error")
	}
}

func TestNewDefaults(t *testing.T) {
	c := New("")
	if c.BaseURL != DefaultBaseURL {
		t.Errorf("default base: %q", c.BaseURL)
	}
	c = New("http://host:9/v1/")
	if c.BaseURL != "http://host:9/v1" {
		t.Errorf("trailing slash: %q", c.BaseURL)
	}
	if !strings.HasSuffix(c.BaseURL, "/v1") {
		t.Error("base must keep the /v1 prefix for /models")
	}
}
