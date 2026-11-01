package job

import (
	"encoding/json"
	"testing"
)

// TestSubmitRoundTrip verifies a submitted queue survives a JSON round-trip,
// preserving params (output + file-field lists).
func TestSubmitRoundTrip(t *testing.T) {
	sub := Submit{Queue: []Job{
		{Tool: "image/anima/anima-base.sh", Params: map[string]any{
			"output": "scene/portrait.png",
			"prompt": "a portrait",
			"refs":   []any{"a.png", "b.png"},
		}},
	}}
	b, err := json.Marshal(sub)
	if err != nil {
		t.Fatal(err)
	}
	var got Submit
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Queue) != 1 {
		t.Fatalf("jobs = %d", len(got.Queue))
	}
	j := got.Queue[0]
	if j.Tool != "image/anima/anima-base.sh" {
		t.Errorf("tool = %q", j.Tool)
	}
	if j.Output() != "scene/portrait.png" {
		t.Errorf("output = %q", j.Output())
	}
	refs, ok := j.Params["refs"].([]any)
	if !ok || len(refs) != 2 || refs[0] != "a.png" {
		t.Errorf("refs = %v", j.Params["refs"])
	}
}

// TestNormalizePath pins the legacy "artifacts/" mount-name leniency.
func TestNormalizePath(t *testing.T) {
	cases := map[string]string{
		"artifacts/scene/x.png": "scene/x.png",
		"scene/x.png":           "scene/x.png",
		"  artifacts/x.png  ":   "x.png",
		"artifacts/":            "artifacts/", // trimming would empty the path
		"artifactsX/y":          "artifactsX/y",
	}
	for in, want := range cases {
		if got := NormalizePath(in); got != want {
			t.Errorf("NormalizePath(%q) = %q want %q", in, got, want)
		}
	}
}

// TestOutput covers the params["output"] accessor.
func TestOutput(t *testing.T) {
	j := Job{Params: map[string]any{"output": "artifacts/a/b.png"}}
	if got := j.Output(); got != "a/b.png" {
		t.Errorf("Output = %q", got)
	}
	if got := (&Job{}).Output(); got != "" {
		t.Errorf("empty Output = %q", got)
	}
	bad := Job{Params: map[string]any{"output": 42}}
	if got := bad.Output(); got != "" {
		t.Errorf("non-string Output = %q", got)
	}
}
