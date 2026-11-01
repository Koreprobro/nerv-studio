package mcp

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	_ "image/jpeg"
	png "image/png"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"nerv-core/internal/config"
)

func artifactTestServer(t *testing.T) (*Server, *httptest.Server, string) {
	t.Helper()
	afDir := t.TempDir()
	cfg := &config.Config{ToolsDir: t.TempDir(), ArtifactoryDir: afDir}
	srv := New(cfg, nil, nil, log.New(io.Discard, "", 0))
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return srv, ts, afDir
}

func write(t *testing.T, afDir, rel string, data []byte) {
	t.Helper()
	full := filepath.Join(afDir, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeBigPNG(t *testing.T, afDir, rel string, w, h int) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{uint8(x % 256), uint8(y % 256), 128, 255})
		}
	}
	full := filepath.Join(afDir, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(full)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		t.Fatal(err)
	}
}

type contentBlock struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	Data     string `json:"data,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
}

func callMCP(t *testing.T, ts *httptest.Server, name string, args map[string]any) map[string]any {
	t.Helper()
	body := map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": name, "arguments": args},
	}
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/mcp", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", testProtocolVersion)
	req.Header.Set("Mcp-Method", "tools/call")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func resultBlocks(t *testing.T, out map[string]any) []contentBlock {
	t.Helper()
	if err := out["error"]; err != nil {
		t.Fatalf("tool error: %v", err)
	}
	res, _ := out["result"].(map[string]any)
	raw, _ := res["content"].([]any)
	blocks := make([]contentBlock, 0, len(raw))
	for _, c := range raw {
		cm, ok := c.(map[string]any)
		if !ok {
			continue
		}
		blocks = append(blocks, contentBlock{
			Type: cm["type"].(string), Text: stringOr(cm, "text"),
			Data: stringOr(cm, "data"), MimeType: stringOr(cm, "mimeType"),
		})
	}
	return blocks
}

func stringOr(m map[string]any, k string) string {
	if v, ok := m[k].(string); ok {
		return v
	}
	return ""
}

func TestArtifactHTTPGet(t *testing.T) {
	srv, ts, afDir := artifactTestServer(t)
	const rel = "artifacts/pic.png"
	writeBigPNG(t, afDir, rel, 1200, 1200)

	resp, err := http.Get(ts.URL + "/artifacts/" + rel)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	if got := resp.Header.Get("Cache-Control"); !strings.Contains(got, "immutable") {
		t.Fatalf("missing immutable cache header: %q", got)
	}
	body, _ := io.ReadAll(resp.Body)
	if _, _, err := image.Decode(bytes.NewReader(body)); err != nil {
		t.Fatalf("GET body not a decodable image: %v", err)
	}

	miss, _ := http.Get(ts.URL + "/artifacts/does-not-exist.png")
	miss.Body.Close()
	if miss.StatusCode != http.StatusNotFound {
		t.Fatalf("missing artifact: want 404 got %d", miss.StatusCode)
	}

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/artifacts/../../etc/passwd", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("traversal: want 400 got %d", rec.Code)
	}
}

func TestArtifactProbeAndGetImage(t *testing.T) {
	_, ts, afDir := artifactTestServer(t)
	const rel = "artifacts/big.png"
	writeBigPNG(t, afDir, rel, 2400, 2400)

	pb := resultBlocks(t, callMCP(t, ts, "browse", map[string]any{"root": "artifactory", "command": "probe", "artifact": rel}))
	if len(pb) != 1 || pb[0].Type != "text" {
		t.Fatalf("probe content = %+v", pb)
	}
	var meta map[string]any
	if err := json.Unmarshal([]byte(pb[0].Text), &meta); err != nil {
		t.Fatal(err)
	}
	if int(meta["width"].(float64)) != 2400 || int(meta["height"].(float64)) != 2400 {
		t.Fatalf("probe dims = %vx%v", meta["width"], meta["height"])
	}
	if meta["format"] != "png" {
		t.Fatalf("probe format = %v", meta["format"])
	}

	gb := resultBlocks(t, callMCP(t, ts, "browse", map[string]any{"root": "artifactory", "command": "get", "artifact": rel}))
	if len(gb) != 1 || gb[0].Type != "image" || gb[0].MimeType != "image/jpeg" {
		t.Fatalf("get content = %+v", gb)
	}
	raw, err := base64.StdEncoding.DecodeString(gb[0].Data)
	if err != nil {
		t.Fatal(err)
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("preview not decodable: %v", err)
	}
	if d := max(cfg.Width, cfg.Height); d > imgDefaultMaxDim {
		t.Fatalf("preview dim %d > max_dim %d", d, imgDefaultMaxDim)
	}
}

// TestArtifactProbeReturnsURL verifies that probe returns metadata WITHOUT
// bytes AND the full HTTP link (artifactURL) pointing at the file, so the
// client can fetch the raw bytes directly (SPECS.md §4.3).
func TestArtifactProbeReturnsURL(t *testing.T) {
	_, ts, afDir := artifactTestServer(t)
	const rel = "artifacts/withurl.png"
	writeBigPNG(t, afDir, rel, 64, 64)

	pb := resultBlocks(t, callMCP(t, ts, "browse", map[string]any{"root": "artifactory", "command": "probe", "artifact": rel}))
	if len(pb) != 1 || pb[0].Type != "text" {
		t.Fatalf("probe content = %+v", pb)
	}
	var meta map[string]any
	if err := json.Unmarshal([]byte(pb[0].Text), &meta); err != nil {
		t.Fatal(err)
	}
	url, _ := meta["artifactURL"].(string)
	if !strings.HasPrefix(url, "http://") || !strings.HasSuffix(url, "/artifacts/"+rel) {
		t.Fatalf("probe artifactURL = %q (want http link to /artifacts/%s)", url, rel)
	}
	// probe must still report the image metadata (dims) alongside the link.
	if _, ok := meta["width"]; !ok {
		t.Fatalf("probe should still report image dims, got %v", meta)
	}
}

func TestArtifactTextCap(t *testing.T) {
	_, ts, afDir := artifactTestServer(t)
	const rel = "artifacts/log.txt"
	write(t, afDir, rel, bytes.Repeat([]byte("x\n"), 30000))

	pb := resultBlocks(t, callMCP(t, ts, "browse", map[string]any{"root": "artifactory", "command": "probe", "artifact": rel}))
	if int(mustFloat(t, pb[0].Text, "lines")) != 30000 {
		t.Fatalf("probe lines = %v", pb[0].Text)
	}

	gb := resultBlocks(t, callMCP(t, ts, "browse", map[string]any{"root": "artifactory", "command": "get", "artifact": rel}))
	if gb[0].Type != "text" || !strings.Contains(gb[0].Text, "truncated") {
		t.Fatalf("get text not truncated: %.40q", gb[0].Text)
	}
	if len(gb[0].Text) > txtDefaultMax+64 {
		t.Fatalf("text preview too long: %d", len(gb[0].Text))
	}
}

func TestArtifactBinaryPassthrough(t *testing.T) {
	_, ts, afDir := artifactTestServer(t)
	const rel = "artifacts/blob.bin"
	write(t, afDir, rel, []byte{0, 1, 2, 3, 4, 5})

	pb := resultBlocks(t, callMCP(t, ts, "browse", map[string]any{"root": "artifactory", "command": "probe", "artifact": rel}))
	if int(mustFloat(t, pb[0].Text, "sizeBytes")) != 6 {
		t.Fatalf("probe sizeBytes = %v", pb[0].Text)
	}
	gb := resultBlocks(t, callMCP(t, ts, "browse", map[string]any{"root": "artifactory", "command": "get", "artifact": rel}))
	if gb[0].Type != "text" || !strings.Contains(gb[0].Text, "note") {
		t.Fatalf("binary get should be a metadata note: %+v", gb[0])
	}
}

// TestArtifactGetDownscalesContent guards audit M1: encodeImage must truly
// resample. The 2400x2400 source is black except a red band on its right
// quarter (x >= 1800). A real downscale to max_dim=1600 keeps ~25% red pixels;
// the old stdlib draw.Draw "resize" cropped the top-left 1600x1600, losing the
// band entirely (0% red) while still reporting correct dimensions.
func TestArtifactGetDownscalesContent(t *testing.T) {
	_, ts, afDir := artifactTestServer(t)
	const rel = "artifacts/band.png"
	img := image.NewRGBA(image.Rect(0, 0, 2400, 2400))
	for y := 0; y < 2400; y++ {
		for x := 1800; x < 2400; x++ {
			img.Set(x, y, color.RGBA{255, 0, 0, 255})
		}
	}
	full := filepath.Join(afDir, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(full)
	if err != nil {
		t.Fatal(err)
	}
	if err := png.Encode(f, img); err != nil {
		t.Fatal(err)
	}
	f.Close()

	blocks := resultBlocks(t, callMCP(t, ts, "browse", map[string]any{"root": "artifactory", "command": "get", "artifact": rel}))
	if blocks[0].Type != "image" {
		t.Fatalf("get = %+v", blocks[0])
	}
	raw, err := base64.StdEncoding.DecodeString(blocks[0].Data)
	if err != nil {
		t.Fatalf("preview not decodable: %v", err)
	}
	prev, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("decode preview: %v", err)
	}
	b := prev.Bounds()
	if d := max(b.Dx(), b.Dy()); d != imgDefaultMaxDim {
		t.Fatalf("preview dim %d != max_dim %d", d, imgDefaultMaxDim)
	}
	red, total := 0, b.Dx()*b.Dy()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			r, g, bb, _ := prev.At(x, y).RGBA()
			if r > 50000 && g < 20000 && bb < 20000 {
				red++
			}
		}
	}
	frac := float64(red) / float64(total)
	if frac < 0.15 || frac > 0.40 {
		t.Fatalf("red fraction %.3f — preview is a crop, not a downscale (want ~0.25)", frac)
	}
}

func TestArtifactGetRejectsTraversal(t *testing.T) {
	_, ts, _ := artifactTestServer(t)
	out := callMCP(t, ts, "browse", map[string]any{"root": "artifactory", "command": "get", "artifact": "../../etc/passwd"})
	txt := toolErrorText(t, out)
	if !strings.Contains(txt, "escapes") {
		t.Fatalf("expected traversal error, got %q", txt)
	}
}

// TestArtifactGetTextCapped guards audit M6: text get must read and return
// only the byte-capped window.
func TestArtifactGetTextCapped(t *testing.T) {
	_, ts, afDir := artifactTestServer(t)
	const rel = "artifacts/big.txt"
	write(t, afDir, rel, []byte(strings.Repeat("x", 5000)))
	blocks := resultBlocks(t, callMCP(t, ts, "browse",
		map[string]any{"root": "artifactory", "command": "get", "artifact": rel, "max_bytes": 100}))
	if !strings.Contains(blocks[0].Text, "truncated") {
		t.Fatalf("expected truncation notice, got %q", blocks[0].Text)
	}
	if len(blocks[0].Text) > 300 {
		t.Fatalf("text window too large: %d bytes", len(blocks[0].Text))
	}
}

func TestArtifactVideoPreview(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not available")
	}
	_, ts, afDir := artifactTestServer(t)
	const rel = "artifacts/clip.mp4"
	clip := filepath.Join(afDir, rel)
	if err := os.MkdirAll(filepath.Dir(clip), 0o755); err != nil {
		t.Fatal(err)
	}
	gen := exec.Command("ffmpeg", "-y", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=duration=3:size=320x240:rate=10", clip)
	if out, err := gen.CombinedOutput(); err != nil {
		t.Skipf("cannot generate test clip: %v\n%s", err, out)
	}

	pb := resultBlocks(t, callMCP(t, ts, "browse", map[string]any{"root": "artifactory", "command": "probe", "artifact": rel}))
	var meta map[string]any
	_ = json.Unmarshal([]byte(pb[0].Text), &meta)
	if meta["width"].(string) != "320" || meta["height"].(string) != "240" {
		t.Fatalf("video probe dims = %vx%v", meta["width"], meta["height"])
	}

	gb := resultBlocks(t, callMCP(t, ts, "browse", map[string]any{"root": "artifactory", "command": "get", "artifact": rel}))
	if gb[0].Type != "image" || gb[0].MimeType != "image/jpeg" {
		t.Fatalf("video get should be a jpeg thumbnail: %+v", gb[0])
	}
	raw, _ := base64.StdEncoding.DecodeString(gb[0].Data)
	cfg, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("thumbnail not decodable: %v", err)
	}
	if d := max(cfg.Width, cfg.Height); d > vidDefaultMaxDim {
		t.Fatalf("thumbnail %d > max_dim %d", d, vidDefaultMaxDim)
	}
}

func TestArtifactAudioPreview(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not available")
	}
	_, ts, afDir := artifactTestServer(t)
	const rel = "artifacts/tone.mp3"
	aud := filepath.Join(afDir, rel)
	if err := os.MkdirAll(filepath.Dir(aud), 0o755); err != nil {
		t.Fatal(err)
	}
	gen := exec.Command("ffmpeg", "-y", "-loglevel", "error",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=1", aud)
	if out, err := gen.CombinedOutput(); err != nil {
		t.Skipf("cannot generate test tone: %v\n%s", err, out)
	}

	gb := resultBlocks(t, callMCP(t, ts, "browse", map[string]any{"root": "artifactory", "command": "get", "artifact": rel}))
	if gb[0].Type != "audio" || gb[0].MimeType != "audio/mpeg" {
		t.Fatalf("audio get = %+v", gb[0])
	}
}

// toolErrorText asserts that a tools/call answered with an MCP tool error
// (isError content, HTTP 200 — M7.2) rather than a JSON-RPC protocol error,
// and returns the error text.
func toolErrorText(t *testing.T, out map[string]any) string {
	t.Helper()
	if out["error"] != nil {
		t.Fatalf("domain failure must be isError content, not a JSON-RPC error: %v", out["error"])
	}
	res, _ := out["result"].(map[string]any)
	if res == nil || res["isError"] != true {
		t.Fatalf("expected isError result, got %v", out["result"])
	}
	raw, _ := res["content"].([]any)
	if len(raw) == 0 {
		t.Fatalf("isError result without content: %v", out["result"])
	}
	cm, _ := raw[0].(map[string]any)
	txt, _ := cm["text"].(string)
	return txt
}

func mustFloat(t *testing.T, text, k string) float64 {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(text), &m); err != nil {
		t.Fatal(err)
	}
	v, ok := m[k].(float64)
	if !ok {
		t.Fatalf("missing %s in %v", k, m)
	}
	return v
}
