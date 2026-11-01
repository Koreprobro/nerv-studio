// Package config parses the nerv-core CLI flags into a runtime Config.
//
// The core is a thin orchestrator of system tools: every path is configurable
// and defaults to a project-local location. See PLAN-nerv.md §4.2 and
// SPECS.md §1 for the flag list.
//
// Path anchoring: relative config paths (tools/, kb/, artifactory/,
// lora-model-dir/) are resolved against the project root, never the process
// cwd (see ProjectRoot). This keeps the daemon identical regardless of where it
// is launched from.
package config

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
)

// Config holds the resolved runtime configuration.
type Config struct {
	ToolsDir          string // consumer sh tools tree (tool id = path relative to this)
	ArtifactoryDir    string // art-project repository
	KBDir             string // knowledge base
	LoraModelDir      string // LoRA models directory
	AIURL             string // OpenAI-compatible provider URL (LLM transport)
	LLMOffload        bool   // XOR VRAM: offload LLM via provider API for GPU waves
	GPUVisibleDevices string // raw --gpu-visible-devices value; "" => all physical GPUs
	Port              int    // MCP streamable HTTP port
	MaxOutputChars    int    // volume guard: max chars of text a single MCP tool result may return
	HumanToken        string // human-mode secret (X-Nerv-Human header); "" disables human mode
}

// ProjectRoot returns the project root: the parent of the directory that holds
// the running binary. nerv-core is built to <root>/bin/nerv-core, so the root
// is dirname(dirname(binary)). If the executable cannot be resolved, it falls
// back to the current working directory.
func ProjectRoot() string {
	exe, err := os.Executable()
	if err != nil {
		wd, werr := os.Getwd()
		if werr != nil {
			return "."
		}
		return wd
	}
	return filepath.Dir(filepath.Dir(exe))
}

// resolve makes a path absolute, anchoring relative paths at the project root.
// Absolute paths are returned unchanged.
func resolve(p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(ProjectRoot(), p)
}

// Parse parses args (excluding the program name) into a Config.
//
// When --gpu-visible-devices is set, it is applied to CUDA_VISIBLE_DEVICES
// here, BEFORE the GPU adapter enumerates the pool, so the scheduler never sees
// a GPU outside the requested subset.
func Parse(args []string) (*Config, error) {
	c := &Config{}
	fs := flag.NewFlagSet("nerv-core", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "nerv-core — omni-channel AI studio core (thin MCP orchestrator)\n\n")
		fs.PrintDefaults()
	}
	fs.StringVar(&c.ToolsDir, "tools", "tools", "directory tree of consumer sh tools")
	fs.StringVar(&c.ArtifactoryDir, "artifactory", "artifactory", "art-project repository")
	fs.StringVar(&c.KBDir, "kb", "kb", "knowledge base")
	fs.StringVar(&c.LoraModelDir, "lora-model-dir", "lora-model-dir", "directory of LoRA models")
	fs.StringVar(&c.AIURL, "ai-url", "http://127.0.0.1:5000/v1", "OpenAI-compatible provider URL (LLM transport)")
	fs.BoolVar(&c.LLMOffload, "llm-offload", false, "force LLM unloading to free VRAM for diffusion models (XOR VRAM)")
	fs.StringVar(&c.GPUVisibleDevices, "gpu-visible-devices", "", "limit the scheduler GPU pool via CUDA_VISIBLE_DEVICES (ROCm equivalent)")
	fs.IntVar(&c.Port, "port", 9696, "MCP streamable HTTP port")
	fs.IntVar(&c.MaxOutputChars, "max-output-chars", 32768, "volume guard: refuse (with a warning) tool results whose text exceeds this many characters")
	fs.StringVar(&c.HumanToken, "human-token", "", "human-mode secret for the MCP HTTP surface (X-Nerv-Human header); empty disables human mode (env NERV_HUMAN_TOKEN is the fallback)")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if c.HumanToken == "" {
		c.HumanToken = os.Getenv("NERV_HUMAN_TOKEN")
	}

	// Anchor all relative project paths at the project root (parent of the bin/
	// dir holding this binary), never at the process cwd. Absolute paths are
	// kept as-is.
	c.ToolsDir = resolve(c.ToolsDir)
	c.ArtifactoryDir = resolve(c.ArtifactoryDir)
	c.KBDir = resolve(c.KBDir)
	c.LoraModelDir = resolve(c.LoraModelDir)

	if c.GPUVisibleDevices != "" {
		os.Setenv("CUDA_VISIBLE_DEVICES", c.GPUVisibleDevices)
	}
	return c, nil
}
