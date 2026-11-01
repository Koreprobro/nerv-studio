package gpu

import (
	"errors"
	"strings"
	"testing"
)

// TestListPhysicalIndex: the adapter re-indexes visible GPUs 0..N-1 while the
// PhysicalIndex keeps the nvidia-smi id (audit M5).
func TestListPhysicalIndex(t *testing.T) {
	t.Setenv("CUDA_VISIBLE_DEVICES", "")
	orig := nvidiaSMI
	defer func() { nvidiaSMI = orig }()
	nvidiaSMI = func() ([]phy, error) {
		return []phy{{id: 5, mem: 24000, name: "A"}, {id: 7, mem: 24000, name: "B"}}, nil
	}
	gpus, err := List()
	if err != nil {
		t.Fatal(err)
	}
	if len(gpus) != 2 || gpus[0].Index != 0 || gpus[0].PhysicalIndex != 5 ||
		gpus[1].Index != 1 || gpus[1].PhysicalIndex != 7 {
		t.Fatalf("bad re-index: %+v", gpus)
	}
}

// TestDiscoverNoBackend emulates a host without nvidia-smi: Discover surfaces a
// combined error (CUDA failure + ROCm stub), which the daemon turns into a warn
// + empty pool instead of a fatal exit (audit M5 / D4).
func TestDiscoverNoBackend(t *testing.T) {
	orig := nvidiaSMI
	defer func() { nvidiaSMI = orig }()
	nvidiaSMI = func() ([]phy, error) {
		return nil, errors.New("exec: \"nvidia-smi\": executable file not found")
	}
	gpus, err := Discover()
	if err == nil {
		t.Fatalf("expected error without any GPU backend, got %+v", gpus)
	}
	if !strings.Contains(err.Error(), "nvidia-smi") || !strings.Contains(err.Error(), "rocm") {
		t.Fatalf("error should mention both backends: %v", err)
	}
}
