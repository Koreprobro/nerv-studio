package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"nerv-core/internal/config"
	"nerv-core/internal/gpu"
	"nerv-core/internal/job"
)

const testProtocolVersion = "2026-07-28"

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	toolsDir := t.TempDir()
	afDir := t.TempDir()

	tool := filepath.Join(toolsDir, "gen.sh")
	// gen declares a GPU via --tool-info, so a wave through this server takes
	// the blocking SSE path (LLMOffload=true + gpu job), like a real generation.
	if err := os.WriteFile(tool, []byte("#!/usr/bin/env sh\ncase \"${1:-}\" in\n  --tool-info) echo '{\"summary\":\"test gen\",\"fields\":[],\"gpu-count\":1}'; exit 0;;\nesac\necho hi > \"$NERV_ARTIFACTORY_DIR/$NERV_OUTPUT\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		ToolsDir:       toolsDir,
		ArtifactoryDir: afDir,
		LLMOffload:     true,
	}
	srv := New(cfg, []gpu.GPU{{Index: 0, MemTotalMB: 16000, Name: "test-gpu"}}, nil,
		log.New(io.Discard, "", 0))
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return ts
}

// post sends a JSON-RPC request with the mandatory Streamable HTTP metadata
// headers and returns the raw response.
func post(t *testing.T, client *http.Client, url string, body map[string]any) *http.Response {
	t.Helper()
	if body["_meta"] == nil {
		body["_meta"] = map[string]any{"io.modelcontextprotocol/protocolVersion": testProtocolVersion}
	}
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	endpoint := url
	if !strings.HasSuffix(endpoint, "/mcp") {
		endpoint += "/mcp" // the RPC surface is served on POST /mcp (M7.6)
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, endpoint, bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", testProtocolVersion)
	req.Header.Set("Mcp-Method", body["method"].(string))
	if params, ok := body["params"].(map[string]any); ok {
		if name, ok := params["name"].(string); ok {
			req.Header.Set("Mcp-Name", name)
		}
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// readSSE parses an event-stream body into a slice of JSON events.
func readSSE(t *testing.T, resp *http.Response) []map[string]any {
	t.Helper()
	var events []map[string]any
	sc := bufio.NewScanner(resp.Body)
	var data strings.Builder
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "data:") {
			data.WriteString(strings.TrimPrefix(line, "data:"))
			data.WriteString("\n")
		} else if line == "" && data.Len() > 0 {
			var ev map[string]any
			if err := json.Unmarshal(bytes.TrimSpace([]byte(data.String())), &ev); err == nil {
				events = append(events, ev)
			}
			data.Reset()
		}
	}
	return events
}

func TestMCPInitializeAndToolsList(t *testing.T) {
	ts := newTestServer(t)
	client := &http.Client{}

	// initialize
	resp := post(t, client, ts.URL, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize"})
	defer resp.Body.Close()
	var init map[string]any
	json.NewDecoder(resp.Body).Decode(&init)
	res, _ := init["result"].(map[string]any)
	if res["protocolVersion"] != testProtocolVersion {
		t.Fatalf("protocolVersion=%v", res["protocolVersion"])
	}

	// tools/list
	resp = post(t, client, ts.URL, map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/list"})
	defer resp.Body.Close()
	var list map[string]any
	json.NewDecoder(resp.Body).Decode(&list)
	tools, _ := list["result"].(map[string]any)["tools"].([]any)
	// M7 surface: exactly ONE directory tool (browse) + the three scheduler
	// tools + the dice roller. gpu.list is internal (hidden). Total visible = 5.
	var names []string
	for _, t := range tools {
		names = append(names, t.(map[string]any)["name"].(string))
	}
	if len(names) != 5 {
		t.Fatalf("expected 5 tools, got %d: %v", len(names), names)
	}
	want := map[string]bool{
		"browse":       false,
		"queue.submit": false,
		"job.list":     false,
		"job.status":   false,
		"dice.roll":    false,
	}
	for _, n := range names {
		if _, ok := want[n]; ok {
			want[n] = true
		}
	}
	for n, seen := range want {
		if !seen {
			t.Fatalf("expected tool %q in surface, tools=%v", n, names)
		}
	}
	// The legacy per-directory tools are retired (M7.1).
	for _, legacy := range []string{"artifactory", "knowledge-base", "tools-info", "loras-info"} {
		for _, n := range names {
			if n == legacy {
				t.Fatalf("legacy tool %q must not be in the surface, names=%v", legacy, names)
			}
		}
	}
	// tools/list is deterministic: sorted by name (M7.5).
	for i := 1; i < len(names); i++ {
		if names[i-1] > names[i] {
			t.Fatalf("tools/list not sorted: %v", names)
		}
	}
	// gpu.list is an internal scheduler tool: it stays registered and callable
	// by name, but must NOT be advertised in tools/list (the consumer-AI does
	// not manage GPU placement).
	for _, n := range names {
		if n == "gpu.list" {
			t.Fatalf("gpu.list must not be advertised to the consumer-AI, names=%v", names)
		}
	}
	// llm.complete must not be in the surface (removed — the provider is callable
	// directly; the LLM client is used only for VRAM offload).
	for _, n := range names {
		if n == "llm.complete" {
			t.Fatalf("llm.complete must not be in the MCP surface, names=%v", names)
		}
	}
}

// TestMCPInitializeBodyOnlyHeader mirrors real MCP clients (Cline, etc.) that
// do NOT send the OPTIONAL MCP-Protocol-Version header and convey the version
// only in initialize params. The server must accept it (no HeaderMismatch).
func TestMCPInitializeBodyOnlyHeader(t *testing.T) {
	ts := newTestServer(t)
	client := &http.Client{}
	b, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params":  map[string]any{"protocolVersion": testProtocolVersion, "capabilities": map[string]any{}},
	})
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, ts.URL+"/mcp", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Mcp-Method", "initialize")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200 without protocol-version header, got %d: %s", resp.StatusCode, string(body))
	}
}

// TestMCPBodyOnlyNoHeaders sends a request with NONE of the optional
// request-metadata headers (no protocol-version, no Mcp-Method, no Mcp-Name),
// mirroring clients that rely solely on the body. Must succeed.
func TestMCPBodyOnlyNoHeaders(t *testing.T) {
	ts := newTestServer(t)
	client := &http.Client{}
	b, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      3,
		"method":  "ping",
	})
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, ts.URL+"/mcp", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200 with no request-metadata headers, got %d: %s", resp.StatusCode, string(body))
	}
}

// TestMCPResponseHasJSONRPC asserts every JSON-RPC response carries the required
// "jsonrpc": "2.0" member (JSON-RPC 2.0 / MCP 2026-07-28). Real MCP clients such
// as Cline validate responses against a strict schema and reject any response
// missing this member — this test guards against that regression.
func TestMCPResponseHasJSONRPC(t *testing.T) {
	ts := newTestServer(t)
	client := &http.Client{}

	resp := post(t, client, ts.URL, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize"})
	defer resp.Body.Close()
	var got map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got["jsonrpc"] != "2.0" {
		t.Fatalf("response missing required jsonrpc member, got %v", got["jsonrpc"])
	}
}

// TestMCPNegotiateVersion exercises MCP protocol-version negotiation. A client may
// convey its supported version in the OPTIONAL MCP-Protocol-Version header (the
// Cline path: header set, body empty), in initialize.params.protocolVersion, or
// both. The server must echo whichever the client uses when it is at or below
// ProtocolVersion, and fall back to its own when the client sends nothing.
func TestMCPNegotiateVersion(t *testing.T) {
	ts := newTestServer(t)
	client := &http.Client{}

	// (a) version conveyed ONLY in the header, as Cline does.
	b, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "initialize",
		"params": map[string]any{"capabilities": map[string]any{}},
	})
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, ts.URL+"/mcp", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("MCP-Protocol-Version", "2025-06-18")
	req.Header.Set("Mcp-Method", "initialize")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if res, _ := got["result"].(map[string]any); res["protocolVersion"] != "2025-06-18" {
		t.Fatalf("header-only version not echoed, got %v", res["protocolVersion"])
	}

	// (b) version conveyed ONLY in params.
	b, _ = json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 2, "method": "initialize",
		"params": map[string]any{"protocolVersion": "2024-11-05", "capabilities": map[string]any{}},
	})
	req, _ = http.NewRequestWithContext(context.Background(), http.MethodPost, ts.URL+"/mcp", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if res, _ := got["result"].(map[string]any); res["protocolVersion"] != "2024-11-05" {
		t.Fatalf("params-only version not echoed, got %v", res["protocolVersion"])
	}

	// (c) nothing conveyed -> server's own ProtocolVersion.
	b, _ = json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 3, "method": "initialize"})
	req, _ = http.NewRequestWithContext(context.Background(), http.MethodPost, ts.URL+"/mcp", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if res, _ := got["result"].(map[string]any); res["protocolVersion"] != testProtocolVersion {
		t.Fatalf("expected server protocol version %q, got %v", testProtocolVersion, res["protocolVersion"])
	}
}

func TestMCPHeaderMismatch(t *testing.T) {
	ts := newTestServer(t)
	client := &http.Client{}
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "ping"})
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, ts.URL+"/mcp", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("MCP-Protocol-Version", testProtocolVersion)
	req.Header.Set("Mcp-Method", "WRONG")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for header mismatch, got %d", resp.StatusCode)
	}
	// audit M6: the body must be a JSON-RPC error object (-32020), not plain text
	var got struct {
		JSONRPC string `json:"jsonrpc"`
		ID      any    `json:"id"`
		Error   *struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("header-mismatch body must be JSON-RPC: %v", err)
	}
	if got.Error == nil || got.Error.Code != HeaderMismatch {
		t.Fatalf("expected error code %d, got %+v", HeaderMismatch, got)
	}
	if got.ID != float64(1) {
		t.Fatalf("response must echo request id, got %v", got.ID)
	}
}

// TestMCPParseErrorNullID guards audit M6: a JSON-RPC error response must
// always carry the id member — null when the request id is unparseable.
func TestMCPParseErrorNullID(t *testing.T) {
	ts := newTestServer(t)
	resp, err := http.Post(ts.URL+"/mcp", "application/json", strings.NewReader("{not json"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"id":null`) {
		t.Fatalf(`parse-error response must contain "id":null, got %s`, body)
	}
	if !strings.Contains(string(body), "-32700") {
		t.Fatalf("parse-error response must carry code -32700, got %s", body)
	}
}

// TestWaveRetentionCap guards audit M6: finished waves must be trimmed from
// server memory (cap 64); running waves are kept regardless.
func TestWaveRetentionCap(t *testing.T) {
	srv := New(&config.Config{ToolsDir: t.TempDir(), ArtifactoryDir: t.TempDir()},
		nil, nil, log.New(io.Discard, "", 0))
	for i := 0; i < 70; i++ {
		w := srv.addWave(nil, func() {}, false)
		if i == 0 {
			continue // wave 1 stays unfinished — it must survive the trim
		}
		close(w.done)
	}
	srv.mu.Lock()
	n := len(srv.waves)
	ids := make([]int, 0, n)
	for _, w := range srv.waves {
		ids = append(ids, w.id)
	}
	srv.mu.Unlock()
	if n != 64 {
		t.Fatalf("retained %d waves, want cap 64", n)
	}
	if ids[0] != 1 {
		t.Fatalf("unfinished wave 1 must be retained, got ids %v", ids)
	}
	if srv.latestWave(true).id != 70 {
		t.Fatalf("latest wave lost: %d", srv.latestWave(true).id)
	}
}

// TestProbeVideoDimsNoPanic guards audit M6: probeVideoDims used to type-assert
// ffprobe map values, panicking when width/height are missing (nil map for a
// missing/non-media file).
func TestProbeVideoDimsNoPanic(t *testing.T) {
	if w, h, ok := probeVideoDims(filepath.Join(t.TempDir(), "nope.bin")); ok || w != 0 || h != 0 {
		t.Fatalf("expected ok=false for non-media, got %v %v %v", w, h, ok)
	}
}

// TestMCPQueueSubmitStream exercises the blocking queue.submit SSE stream and
// verifies the wave completes, outputs land under the declared names, and job.status
// reports them.
func TestMCPQueueSubmitStream(t *testing.T) {
	ts := newTestServer(t)
	client := &http.Client{}

	queue := map[string]any{
		"queue": []map[string]any{
			{"tool": "gen.sh", "params": map[string]any{"output": "artifacts/a.png"}, "gpu-count": 1},
			{"tool": "gen.sh", "params": map[string]any{"output": "b.png"}, "gpu-count": 1},
		},
	}
	resp := post(t, client, ts.URL, map[string]any{
		"jsonrpc": "2.0", "id": 10, "method": "tools/call",
		"params": map[string]any{"name": "queue.submit", "arguments": queue},
	})
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("expected text/event-stream, got %q", ct)
	}
	events := readSSE(t, resp)

	// last event is the final result
	var final map[string]any
	for _, ev := range events {
		if _, ok := ev["result"]; ok {
			final = ev
		}
	}
	if final == nil {
		t.Fatalf("no final result event in %d events", len(events))
	}
	res, _ := final["result"].(map[string]any)
	content, _ := res["content"].([]any)
	var waveJobs []any
	for _, c := range content {
		cm, ok := c.(map[string]any)
		if !ok || cm["type"] != "text" {
			continue
		}
		t.Logf("finalText=%s", cm["text"])
		var wm map[string]any
		if json.Unmarshal([]byte(cm["text"].(string)), &wm) == nil {
			if j, ok := wm["jobs"].([]any); ok {
				waveJobs = j
			}
		}
	}
	if len(waveJobs) != 2 {
		t.Fatalf("expected 2 jobs in result, got %d", len(waveJobs))
	}

	// job.status via a fresh wave (latest)
	resp = post(t, client, ts.URL, map[string]any{
		"jsonrpc": "2.0", "id": 11, "method": "tools/call",
		"params": map[string]any{"name": "job.status", "arguments": map[string]any{"output": "b.png"}},
	})
	defer resp.Body.Close()
	var statusResp map[string]any
	json.NewDecoder(resp.Body).Decode(&statusResp)
	statusRes, _ := statusResp["result"].(map[string]any)
	content = statusRes["content"].([]any)
	if len(content) == 0 {
		t.Fatalf("empty job.status content")
	}
	var jobStatus map[string]any
	json.Unmarshal([]byte(content[0].(map[string]any)["text"].(string)), &jobStatus)
	if jobStatus["status"] != "done" {
		t.Fatalf("job b status=%v want done", jobStatus["status"])
	}
}

// TestJobStatusRelativeArtifactPath asserts that job.status and queue.submit
// report the artifact as a relative /artifacts/<rel> link (not a server-absolute
// path), matching the GET /artifacts/<rel> byte surface (SPECS.md §6). The wave
// is built synchronously at submit, so resolvedArtifact is populated before the
// job.status query runs.
func TestJobStatusRelativeArtifactPath(t *testing.T) {
	ts := newTestServer(t)
	client := &http.Client{}

	queue := map[string]any{
		"queue": []map[string]any{
			{"tool": "gen.sh", "params": map[string]any{"output": "artifacts/a.png"}, "gpu-count": 1},
			{"tool": "gen.sh", "params": map[string]any{"output": "b.png"}, "gpu-count": 1},
		},
	}
	resp := post(t, client, ts.URL, map[string]any{
		"jsonrpc": "2.0", "id": 10, "method": "tools/call",
		"params": map[string]any{"name": "queue.submit", "arguments": queue},
	})
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("expected text/event-stream, got %q", ct)
	}
	events := readSSE(t, resp)

	// In blocking mode the SSE stream carries progress events first (no "result"
	// key); the final result event holds the wave snapshot with the jobs.
	var waveJobs []any
	for _, ev := range events {
		res, ok := ev["result"].(map[string]any)
		if !ok {
			continue
		}
		content, _ := res["content"].([]any)
		for _, c := range content {
			cm, ok := c.(map[string]any)
			if !ok || cm["type"] != "text" {
				continue
			}
			text, ok := cm["text"].(string)
			if !ok {
				continue
			}
			var wm map[string]any
			if json.Unmarshal([]byte(text), &wm) == nil {
				if j, ok := wm["jobs"].([]any); ok {
					waveJobs = j
				}
			}
		}
	}
	if len(waveJobs) != 2 {
		t.Fatalf("queue.submit wave: %d jobs, want 2", len(waveJobs))
	}
	for _, jj := range waveJobs {
		j, _ := jj.(map[string]any)
		out, _ := j["output"].(string)
		// output is reported as declared (browse path), no leading slash, and
		// the legacy "artifacts/" submit prefix is stripped.
		if out == "" || strings.HasPrefix(out, "/") || strings.HasPrefix(out, "artifacts/") {
			t.Errorf("queue.submit %v: output=%q, want artifactory-relative without artifacts/ prefix", j["tool"], out)
		}
	}

	// job.status on a fresh query returns the same relative link.
	st := callTool(t, client, ts.URL, "job.status", map[string]any{"output": "b.png"})
	jb, _ := st.(map[string]any)
	out, _ := jb["output"].(string)
	if want := "b.png"; out != want {
		t.Errorf("job.status output=%q, want %q", out, want)
	}
}

// TestArtifactHTTPURL asserts that queue.submit and job.status include a full
// HTTP artifact link (artifactURL: http://127.0.0.1:<port>/artifacts/<rel>)
// alongside the relative resolvedArtifact, so the client can fetch the bytes
// directly over GET /artifacts/<rel>. The port is cfg.Port (0 in the test
// server); only the scheme/host prefix and the relative-path suffix are
// asserted, so the test is independent of the actual listen port.
func TestArtifactHTTPURL(t *testing.T) {
	ts := newTestServer(t)
	client := &http.Client{}

	queue := map[string]any{
		"queue": []map[string]any{
			{"tool": "gen.sh", "params": map[string]any{"output": "artifacts/a.png"}, "gpu-count": 1},
			{"tool": "gen.sh", "params": map[string]any{"output": "b.png"}, "gpu-count": 1},
		},
	}
	resp := post(t, client, ts.URL, map[string]any{
		"jsonrpc": "2.0", "id": 11, "method": "tools/call",
		"params": map[string]any{"name": "queue.submit", "arguments": queue},
	})
	defer resp.Body.Close()
	events := readSSE(t, resp)

	// In blocking mode the SSE stream carries progress events first (no "result"
	// key); the final result event holds the wave snapshot with the jobs.
	var waveJobs []any
	for _, ev := range events {
		res, ok := ev["result"].(map[string]any)
		if !ok {
			continue
		}
		content, _ := res["content"].([]any)
		for _, c := range content {
			cm, ok := c.(map[string]any)
			if !ok || cm["type"] != "text" {
				continue
			}
			text, ok := cm["text"].(string)
			if !ok {
				continue
			}
			var wm map[string]any
			if json.Unmarshal([]byte(text), &wm) == nil {
				if j, ok := wm["jobs"].([]any); ok {
					waveJobs = j
				}
			}
		}
	}
	if len(waveJobs) != 2 {
		t.Fatalf("queue.submit wave: %d jobs, want 2", len(waveJobs))
	}
	for _, jj := range waveJobs {
		j, _ := jj.(map[string]any)
		ua, _ := j["artifactURL"].(string)
		out, _ := j["output"].(string)
		if !strings.HasPrefix(ua, "http://127.0.0.1:") {
			t.Errorf("queue.submit %v: artifactURL=%q, want http://127.0.0.1:<port>/artifacts/<rel>", j["tool"], ua)
		}
		if !strings.HasSuffix(ua, out) {
			t.Errorf("queue.submit %v: artifactURL=%q must end with output=%q", j["tool"], ua, out)
		}
	}

	// job.status carries the same full link.
	st := callTool(t, client, ts.URL, "job.status", map[string]any{"output": "b.png"})
	jb, _ := st.(map[string]any)
	ua, _ := jb["artifactURL"].(string)
	out, _ := jb["output"].(string)
	if !strings.HasPrefix(ua, "http://127.0.0.1:") || !strings.HasSuffix(ua, out) {
		t.Errorf("job.status artifactURL=%q, want http://127.0.0.1:<port>/artifacts/%q", ua, out)
	}
}

// callTool posts a non-streaming tools/call and returns the tool's JSON return
// value (unwrapped from the text content block).
func callTool(t *testing.T, client *http.Client, url, name string, args map[string]any) any {
	t.Helper()
	resp := post(t, client, url, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": name, "arguments": args},
	})
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var r struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		t.Fatalf("parse result for %s: %v (%s)", name, err, body)
	}
	if r.Error != nil {
		t.Fatalf("tool %s returned error: %s", name, r.Error.Message)
	}
	if len(r.Result.Content) == 0 {
		t.Fatalf("tool %s: empty content", name)
	}
	var v any
	if err := json.Unmarshal([]byte(r.Result.Content[0].Text), &v); err != nil {
		t.Fatalf("parse tool %s output %q: %v", name, r.Result.Content[0].Text, err)
	}
	return v
}

// writeSafetensors writes a minimal valid safetensors file: the 8-byte
// little-endian header-length prefix, the JSON header (with __metadata__), and a
// few dummy tensor bytes. It lets the test exercise the safetensors metadata
// branch of loras-info detail without loading any tensor data.
func writeSafetensors(t *testing.T, path string, meta map[string]string) {
	t.Helper()
	header := map[string]any{
		"__metadata__": meta,
		"w":            map[string]any{"dtype": "F32", "shape": []int{2, 2}, "data_offsets": []int{0, 16}},
	}
	hb, err := json.Marshal(header)
	if err != nil {
		t.Fatal(err)
	}
	var lenBuf [8]byte
	binary.LittleEndian.PutUint64(lenBuf[:], uint64(len(hb)))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	f.Write(lenBuf[:])
	f.Write(hb)
	f.Write(make([]byte, 16))
}

// contains reports whether s contains v. Used by the surface tests.
func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

const smokeTool = `#!/usr/bin/env sh
case "${1:-}" in
  --tool-info)
    echo '{"summary":"smoke gen a thing","fields":[{"name":"prompt","required":true,"description":"smoke prompt"}],"gpu-count":1}'
    ;;
esac
exit 0
`

// waitGone polls until path stops existing (a /proc/<pid> entry disappearing).
func waitGone(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if _, err := os.Stat(path); err != nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("still present after %s", timeout)
		}
		time.Sleep(30 * time.Millisecond)
	}
}

// TestWaveCancelOnClientDisconnect guards audit M2 / D1: a blocking wave is
// connection-scoped. When the client drops mid-stream the wave is canceled:
// the running tool's whole process group is SIGKILLed (the script spawns a
// `sleep` child to prove the group kill reaches grandchildren), the pending
// job is failed "wave canceled", and no goroutines/progress senders leak
// (the old code stalled the wave once progressCh filled with no reader).
func TestWaveCancelOnClientDisconnect(t *testing.T) {
	toolsDir := t.TempDir()
	sleeper := `#!/usr/bin/env sh
case "${1:-}" in
  --tool-info) echo '{"summary":"sleeper","fields":[],"gpu-count":1}'; exit 0;;
esac
sleep 57 &
echo $! > "$NERV_MARKER"
wait
`
	if err := os.WriteFile(filepath.Join(toolsDir, "sleeper.sh"), []byte(sleeper), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{ToolsDir: toolsDir, ArtifactoryDir: t.TempDir(), LLMOffload: true}
	srv := New(cfg, []gpu.GPU{{Index: 0, MemTotalMB: 1, Name: "test-gpu"}}, nil,
		log.New(io.Discard, "", 0))
	ts := httptest.NewServer(srv)
	defer ts.Close()

	marker := filepath.Join(t.TempDir(), "child.pid")
	queue := map[string]any{"queue": []map[string]any{
		{"tool": "sleeper.sh", "gpu-count": 1,
			"params": map[string]any{"output": "slow1.png", "marker": marker}},
		{"tool": "sleeper.sh", "gpu-count": 1,
			"params": map[string]any{"output": "slow2.png"}},
	}}

	reqCtx, cancelReq := context.WithCancel(context.Background())
	defer cancelReq()
	b, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 42, "method": "tools/call",
		"_meta":  map[string]any{"io.modelcontextprotocol/protocolVersion": testProtocolVersion},
		"params": map[string]any{"name": "queue.submit", "arguments": queue},
	})
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, ts.URL+"/mcp", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// wait for job 1 to report "running" over SSE
	sc := bufio.NewScanner(resp.Body)
	running := false
	for sc.Scan() {
		if strings.Contains(sc.Text(), `"status":"running"`) {
			running = true
			break
		}
	}
	if !running {
		t.Fatal("job never reached running")
	}

	// the script records its `sleep` child pid; wait for it
	childPID := ""
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && childPID == "" {
		if data, err := os.ReadFile(marker); err == nil {
			if s := strings.TrimSpace(string(data)); s != "" {
				childPID = s
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if childPID == "" {
		t.Fatal("sleeper never recorded the child pid")
	}

	baseG := runtime.NumGoroutine()

	// client disconnects mid-stream (D1)
	resp.Body.Close()
	cancelReq()

	// (a) the grandchild generation process dies with its group
	if err := waitGone("/proc/"+childPID, 5*time.Second); err != nil {
		t.Fatalf("generation child pid %s survived wave cancel: %v", childPID, err)
	}

	// (b) both jobs settle failed: running killed, pending canceled — never
	// stuck "running"/"pending"
	w := srv.latestWave(true)
	if w == nil {
		t.Fatal("no wave registered")
	}
	deadline = time.Now().Add(5 * time.Second)
	for {
		snap := w.sched.JobsSnapshot()
		settled := len(snap) == 2 &&
			snap[0].Status == job.Failed &&
			snap[1].Status == job.Failed && strings.Contains(snap[1].Error, "canceled")
		if settled {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("jobs did not settle after cancel: %+v", snap)
		}
		time.Sleep(25 * time.Millisecond)
	}

	// (c) no goroutine pile-up after the disconnect + drain
	deadline = time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > baseG+2 && time.Now().Before(deadline) {
		time.Sleep(25 * time.Millisecond)
	}
	if cur := runtime.NumGoroutine(); cur > baseG+2 {
		t.Fatalf("goroutine leak: baseline %d, now %d", baseG, cur)
	}
}

// TestConcurrentSubmitsSerialize guards audit M3: waves must run strictly one
// at a time daemon-wide (the scheduler only excludes GPUs inside a single
// wave, so two concurrent waves could double-book the same physical GPU). Two
// async waves are submitted at once; their tool scripts record start/end
// wall-clock stamps into one marker file — the intervals must not overlap.
func TestConcurrentSubmitsSerialize(t *testing.T) {
	toolsDir := t.TempDir()
	ticker := `#!/usr/bin/env sh
case "${1:-}" in
  --tool-info) echo '{"summary":"ticker","fields":[],"gpu-count":0}'; exit 0;;
esac
echo "start $NERV_TAG $(date +%s%N)" >> "$NERV_MARK"
sleep 0.4
echo "end $NERV_TAG $(date +%s%N)" >> "$NERV_MARK"
echo tick > "$NERV_ARTIFACTORY_DIR/$NERV_OUTPUT"
`
	if err := os.WriteFile(filepath.Join(toolsDir, "ticker.sh"), []byte(ticker), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{ToolsDir: toolsDir, ArtifactoryDir: t.TempDir(), LLMOffload: false}
	srv := New(cfg, nil, nil, log.New(io.Discard, "", 0))
	ts := httptest.NewServer(srv)
	defer ts.Close()

	mark := filepath.Join(t.TempDir(), "marks")
	client := &http.Client{}
	var wg sync.WaitGroup
	for _, tag := range []string{"A", "B"} {
		wg.Add(1)
		go func(tag string) {
			defer wg.Done()
			resp := post(t, client, ts.URL, map[string]any{
				"jsonrpc": "2.0", "id": 1, "method": "tools/call",
				"params": map[string]any{"name": "queue.submit", "arguments": map[string]any{
					"queue": []map[string]any{{
						"tool":   "ticker.sh",
						"params": map[string]any{"output": "tick-" + tag + ".log", "tag": tag, "mark": mark},
					}},
				}},
			})
			defer resp.Body.Close()
			readSSE(t, resp) // async mode: single result event
		}(tag)
	}
	wg.Wait()

	// wait for both waves' dispatches to settle
	deadline := time.Now().Add(10 * time.Second)
	for {
		srv.mu.Lock()
		settled := len(srv.waves) == 2
		for _, w := range srv.waves {
			select {
			case <-w.done:
			default:
				settled = false
			}
		}
		srv.mu.Unlock()
		if settled {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("waves did not finish")
		}
		time.Sleep(20 * time.Millisecond)
	}

	data, err := os.ReadFile(mark)
	if err != nil {
		t.Fatal(err)
	}
	type span struct {
		tag        string
		start, end int64
	}
	spans := map[string]*span{}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		f := strings.Fields(line)
		if len(f) != 3 {
			t.Fatalf("bad marker line %q", line)
		}
		ns, err := strconv.ParseInt(f[2], 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		s := spans[f[1]]
		if s == nil {
			s = &span{tag: f[1]}
			spans[f[1]] = s
		}
		if f[0] == "start" {
			s.start = ns
		} else {
			s.end = ns
		}
	}
	a, b := spans["A"], spans["B"]
	if a == nil || b == nil || a.start == 0 || a.end == 0 || b.start == 0 || b.end == 0 {
		t.Fatalf("incomplete markers: %q", data)
	}
	if a.start < b.end && b.start < a.end {
		t.Fatalf("waves overlapped: A[%d..%d] B[%d..%d]", a.start, a.end, b.start, b.end)
	}
}
