package llm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestNewDefaultURL verifies New falls back to the local provider URL.
func TestNewDefaultURL(t *testing.T) {
	c := New("", "mymodel")
	if c.BaseURL != "http://127.0.0.1:5000/v1" {
		t.Errorf("BaseURL = %q, want default local URL", c.BaseURL)
	}
	if c.Model != "mymodel" {
		t.Errorf("Model = %q, want mymodel", c.Model)
	}
	if c.HTTP == nil {
		t.Fatal("HTTP client is nil")
	}
}

// TestNewExplicitURL verifies an explicit baseURL is honoured.
func TestNewExplicitURL(t *testing.T) {
	c := New("http://example.test/v1", "")
	if c.BaseURL != "http://example.test/v1" {
		t.Errorf("BaseURL = %q", c.BaseURL)
	}
}

// TestUnloadSuccess verifies Unload discovers the loaded model via /v1/models and
// unloads it through /models/unload, returning nil on a 2xx response.
func TestUnloadSuccess(t *testing.T) {
	var modelsCalled, unloadCalled bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			modelsCalled = true
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"test-model","status":{"value":"loaded"}}]}`))
		case "/models/unload":
			unloadCalled = true
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"success":true}`))
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	}))
	defer srv.Close()

	c := New(srv.URL+"/v1", "test-model")
	if err := c.Unload(context.Background()); err != nil {
		t.Errorf("Unload = %v, want nil", err)
	}
	if !modelsCalled || !unloadCalled {
		t.Errorf("expected both /v1/models and /models/unload to be called")
	}
}

// TestUnloadProviderError verifies a non-2xx /models/unload surfaces an error.
func TestUnloadProviderError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"test-model","status":{"value":"loaded"}}]}`))
		case "/models/unload":
			http.Error(w, "nope", http.StatusBadGateway)
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	}))
	defer srv.Close()

	c := New(srv.URL+"/v1", "test-model")
	if err := c.Unload(context.Background()); err == nil {
		t.Fatal("expected error for 502 unload, got nil")
	}
}

// TestUnloadNothingLoaded verifies Unload is a no-op (nil) when no model is loaded.
func TestUnloadNothingLoaded(t *testing.T) {
	var unloadCalled bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"test-model","status":{"value":"unloaded"}}]}`))
		case "/models/unload":
			unloadCalled = true
		}
	}))
	defer srv.Close()

	c := New(srv.URL+"/v1", "test-model")
	if err := c.Unload(context.Background()); err != nil {
		t.Errorf("Unload = %v, want nil when nothing loaded", err)
	}
	if unloadCalled {
		t.Errorf("/models/unload should not be called when nothing is loaded")
	}
}
