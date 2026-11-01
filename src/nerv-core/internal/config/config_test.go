package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestParseDefaults verifies that an empty arg list yields the documented
// project-local defaults, resolved absolutely against the project root.
func TestParseDefaults(t *testing.T) {
	c, err := Parse(nil)
	if err != nil {
		t.Fatal(err)
	}
	root := ProjectRoot()
	want := func(p string) string { return filepath.Join(root, p) }
	if c.ToolsDir != want("tools") {
		t.Errorf("ToolsDir = %q, want %q", c.ToolsDir, want("tools"))
	}
	if c.ArtifactoryDir != want("artifactory") {
		t.Errorf("ArtifactoryDir = %q, want %q", c.ArtifactoryDir, want("artifactory"))
	}
	if c.KBDir != want("kb") {
		t.Errorf("KBDir = %q, want %q", c.KBDir, want("kb"))
	}
	if c.LoraModelDir != want("lora-model-dir") {
		t.Errorf("LoraModelDir = %q, want %q", c.LoraModelDir, want("lora-model-dir"))
	}
	if c.AIURL != "http://127.0.0.1:5000/v1" {
		t.Errorf("AIURL = %q, want %q", c.AIURL, "http://127.0.0.1:5000/v1")
	}
	if c.LLMOffload {
		t.Error("LLMOffload = true, want false")
	}
	if c.GPUVisibleDevices != "" {
		t.Errorf("GPUVisibleDevices = %q, want empty", c.GPUVisibleDevices)
	}
	if c.Port != 9696 {
		t.Errorf("Port = %d, want 9696", c.Port)
	}
}

// TestParseOverrides verifies explicit flag values are honoured.
func TestParseOverrides(t *testing.T) {
	args := []string{
		"--tools", "/srv/tools",
		"--artifactory", "/srv/art",
		"--kb", "/srv/kb",
		"--lora-model-dir", "/srv/lora",
		"--ai-url", "http://example.test/v1",
		"--llm-offload=true",
		"--port", "8080",
	}
	c, err := Parse(args)
	if err != nil {
		t.Fatal(err)
	}
	if c.ToolsDir != "/srv/tools" || c.ArtifactoryDir != "/srv/art" || c.KBDir != "/srv/kb" {
		t.Errorf("dirs not overridden: %+v", c)
	}
	if c.LoraModelDir != "/srv/lora" {
		t.Errorf("LoraModelDir = %q", c.LoraModelDir)
	}
	if c.AIURL != "http://example.test/v1" {
		t.Errorf("AIURL = %q", c.AIURL)
	}
	if !c.LLMOffload {
		t.Error("LLMOffload = false, want true")
	}
	if c.Port != 8080 {
		t.Errorf("Port = %d, want 8080", c.Port)
	}
}

// TestGPUVisibleDevicesEnv verifies that --gpu-visible-devices is applied to
// CUDA_VISIBLE_DEVICES so the scheduler never sees a GPU outside the subset.
func TestGPUVisibleDevicesEnv(t *testing.T) {
	prev, had := os.LookupEnv("CUDA_VISIBLE_DEVICES")
	defer func() {
		if had {
			os.Setenv("CUDA_VISIBLE_DEVICES", prev)
		} else {
			os.Unsetenv("CUDA_VISIBLE_DEVICES")
		}
	}()

	c, err := Parse([]string{"--gpu-visible-devices", "0,1"})
	if err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("CUDA_VISIBLE_DEVICES"); got != "0,1" {
		t.Errorf("CUDA_VISIBLE_DEVICES = %q, want %q", got, "0,1")
	}
	if c.GPUVisibleDevices != "0,1" {
		t.Errorf("GPUVisibleDevices = %q", c.GPUVisibleDevices)
	}
}

// TestGPUVisibleDevicesEmptyLeavesEnv verifies that with no --gpu-visible-devices
// the CUDA_VISIBLE_DEVICES env is untouched.
func TestGPUVisibleDevicesEmptyLeavesEnv(t *testing.T) {
	os.Unsetenv("CUDA_VISIBLE_DEVICES")
	c, err := Parse(nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := os.LookupEnv("CUDA_VISIBLE_DEVICES"); ok {
		t.Errorf("CUDA_VISIBLE_DEVICES should be unset, got %q", os.Getenv("CUDA_VISIBLE_DEVICES"))
	}
	_ = c
}

// TestParseBadFlag verifies an unknown flag returns an error (ContinueOnError).
func TestParseBadFlag(t *testing.T) {
	if _, err := Parse([]string{"--nope"}); err == nil {
		t.Fatal("expected error for unknown flag, got nil")
	}
}
