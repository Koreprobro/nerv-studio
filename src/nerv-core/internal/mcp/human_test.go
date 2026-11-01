package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"nerv-core/internal/config"
	"nerv-core/internal/gpu"
)

// humanToolFixture: public prompt + private cfg/sampler (with defaults).
const humanToolFixture = `#!/usr/bin/env sh
case "${1:-}" in
  --tool-info) echo '{"summary":"priv demo","fields":[{"name":"prompt","required":true,"description":"public prompt"},{"name":"cfg","required":false,"private":true,"default":7.0,"description":"guidance scale"},{"name":"sampler","required":false,"private":true,"default":"euler","description":"sampler name"}],"gpu-count":0}'; exit 0;;
esac
echo "p=${NERV_PROMPT} cfg=${NERV_CFG:-7.0}" > "$NERV_ARTIFACTORY_DIR/$NERV_OUTPUT"
`

const humanTestToken = "sekrit-token-0123456789abcdef"

// newHumanServer builds an async server (cpu gen tool, no offload) with the
// human token set and the private-field fixture tool installed.
func newHumanServer(t *testing.T) *httptest.Server {
	t.Helper()
	toolsDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(toolsDir, "priv.sh"), []byte(humanToolFixture), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		ToolsDir:       toolsDir,
		ArtifactoryDir: t.TempDir(),
		HumanToken:     humanTestToken,
	}
	srv := New(cfg, []gpu.GPU{{Index: 0, MemTotalMB: 16000, Name: "test-gpu"}}, nil, log.New(io.Discard, "", 0))
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return ts
}

// postHuman posts a JSON-RPC request, optionally carrying the human header.
func postHuman(t *testing.T, client *http.Client, url string, body map[string]any, token string) *http.Response {
	t.Helper()
	if body["_meta"] == nil {
		body["_meta"] = map[string]any{"io.modelcontextprotocol/protocolVersion": testProtocolVersion}
	}
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url+"/mcp", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", testProtocolVersion)
	req.Header.Set("Mcp-Method", body["method"].(string))
	if token != "" {
		req.Header.Set(humanHeader, token)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// callHumanText calls a tool with (token != "") or without the human header.
func callHumanText(t *testing.T, client *http.Client, url, token, name string, args map[string]any) string {
	t.Helper()
	resp := postHuman(t, client, url, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": name, "arguments": args},
	}, token)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var r struct {
		Result struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
		Error *json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		t.Fatalf("parse result for %s: %v (%s)", name, err, body)
	}
	if r.Error != nil || r.Result.IsError || len(r.Result.Content) == 0 {
		t.Fatalf("expected ok text result for %s, got %s", name, body)
	}
	return r.Result.Content[0].Text
}

// submitHumanWave submits a one-job async wave and returns its waveId.
func submitHumanWave(t *testing.T, client *http.Client, url, token, artifact string) int {
	t.Helper()
	resp := postHuman(t, client, url, map[string]any{
		"jsonrpc": "2.0", "id": 10, "method": "tools/call",
		"params": map[string]any{"name": "queue.submit", "arguments": map[string]any{
			"queue": []map[string]any{{"tool": "priv.sh", "params": map[string]any{"output": artifact + ".txt", "prompt": "x", "cfg": 9.5}}},
		}},
	}, token)
	defer resp.Body.Close()
	for _, ev := range readSSE(t, resp) {
		res, ok := ev["result"].(map[string]any)
		if !ok {
			continue
		}
		content, _ := res["content"].([]any)
		for _, c := range content {
			cm, _ := c.(map[string]any)
			var wm map[string]any
			if json.Unmarshal([]byte(cm["text"].(string)), &wm) == nil {
				if id, ok := wm["waveId"].(float64); ok {
					return int(id)
				}
			}
		}
	}
	t.Fatal("no wave result event")
	return 0
}

// TestPublicToolViewUnit covers the boundary filter (C1): private field
// entries vanish (name, description, default), the legacy top-level private
// map is dropped, public fields pass through untouched.
func TestPublicToolViewUnit(t *testing.T) {
	raw := map[string]any{
		"summary":   "s",
		"gpu-count": float64(1),
		"private":   map[string]any{"gpu-count": float64(1)}, // legacy wrapper
		"fields": []any{
			map[string]any{"name": "prompt", "required": true},
			map[string]any{"name": "cfg", "required": false, "private": true, "default": 7.0},
			"not-an-object",
		},
	}
	view := publicToolView(raw)
	if _, ok := view["private"]; ok {
		t.Fatal("legacy private map must be dropped")
	}
	fields := view["fields"].([]any)
	if len(fields) != 2 {
		t.Fatalf("want 2 kept entries (public + non-object), got %d", len(fields))
	}
	b, _ := json.Marshal(view)
	for _, leak := range []string{"cfg", "\"default\"", "\"private\""} {
		if strings.Contains(string(b), leak) {
			t.Fatalf("view leaks %q: %s", leak, b)
		}
	}
	if len(raw["fields"].([]any)) != 3 {
		t.Fatal("publicToolView must not mutate the raw interface")
	}
}

// TestPrivateFieldsVisibility: the consumer-AI never sees private fields in
// probe or in the tools ls view; the human (token) sees everything; a wrong
// token behaves like no token.
func TestPrivateFieldsVisibility(t *testing.T) {
	ts := newHumanServer(t)
	client := &http.Client{}
	probeArgs := map[string]any{"root": "tools", "command": "probe", "path": "priv.sh"}

	consumer := callHumanText(t, client, ts.URL, "", "browse", probeArgs)
	human := callHumanText(t, client, ts.URL, humanTestToken, "browse", probeArgs)
	wrong := callHumanText(t, client, ts.URL, "not-the-token", "browse", probeArgs)

	for _, s := range []string{consumer, wrong} {
		if !strings.Contains(s, "prompt") {
			t.Fatalf("public field must be visible: %s", s)
		}
		for _, leak := range []string{"cfg", "sampler", "private", "euler", "default"} {
			if strings.Contains(strings.ToLower(s), strings.ToLower(leak)) {
				t.Fatalf("consumer view leaks private field data %q: %s", leak, s)
			}
		}
	}
	for _, want := range []string{"cfg", "sampler", "private", "euler"} {
		if !strings.Contains(human, want) {
			t.Fatalf("human view missing %q: %s", want, human)
		}
	}

	dirProbeArgs := map[string]any{"root": "tools", "command": "probe", "path": "."}
	lsConsumer := callHumanText(t, client, ts.URL, "", "browse", dirProbeArgs)
	if strings.Contains(lsConsumer, "cfg") || strings.Contains(lsConsumer, "sampler") {
		t.Fatalf("tools dir probe leaks private fields: %s", lsConsumer)
	}
	lsHuman := callHumanText(t, client, ts.URL, humanTestToken, "browse", dirProbeArgs)
	if !strings.Contains(lsHuman, "sampler") {
		t.Fatalf("human tools dir probe must include private fields: %s", lsHuman)
	}
}

// TestHumanWavesHidden: waves submitted in human-mode are invisible to the
// consumer-AI — the default "latest" skips them and an explicit waveId answers
// the identical "unknown waveId" error, so hidden waves are not enumerable.
func TestHumanWavesHidden(t *testing.T) {
	ts := newHumanServer(t)
	client := &http.Client{}

	pubWave := submitHumanWave(t, client, ts.URL, "", "pub")
	humWave := submitHumanWave(t, client, ts.URL, humanTestToken, "hum")
	if pubWave == humWave {
		t.Fatal("human submission must get its own wave id")
	}

	// consumer default view: the human wave never happened.
	listText := callHumanText(t, client, ts.URL, "", "job.list", map[string]any{})
	if !strings.Contains(listText, "pub") || strings.Contains(listText, "hum") {
		t.Fatalf("consumer job.list must show only the consumer wave: %s", listText)
	}
	// explicit query for the hidden wave: indistinguishable from nonexistent.
	if msg := callToolErr(t, client, ts.URL, "job.list", map[string]any{"waveId": float64(humWave)}); !strings.Contains(msg, "unknown waveId") {
		t.Fatalf("hidden waveId must answer the standard unknown error, got %q", msg)
	}
	// human sees the full history in both directions.
	humList := callHumanText(t, client, ts.URL, humanTestToken, "job.list", map[string]any{})
	if !strings.Contains(humList, "hum") {
		t.Fatalf("human default job.list must show the latest (human) wave: %s", humList)
	}
	pubList := callHumanText(t, client, ts.URL, humanTestToken, "job.list", map[string]any{"waveId": float64(pubWave)})
	if !strings.Contains(pubList, "pub") {
		t.Fatalf("human must still see consumer waves: %s", pubList)
	}
}
