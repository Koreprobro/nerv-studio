package gpu

import (
	"os"
	"testing"
)

// TestListAll verifies the default pool enumerates all physical GPUs,
// re-indexed 0..N-1 by visibility.
func TestListAll(t *testing.T) {
	os.Unsetenv("CUDA_VISIBLE_DEVICES")
	gpus, err := List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(gpus) != 4 {
		t.Fatalf("expected 4 GPUs, got %d", len(gpus))
	}
	for i, g := range gpus {
		if g.Index != i {
			t.Errorf("visible index mismatch: got %d want %d", g.Index, i)
		}
	}
}

// TestListVisibleSubset verifies --gpu-visible-devices (CUDA_VISIBLE_DEVICES)
// trims the pool and re-indexes visible GPUs 0..N-1 while preserving physical ids.
func TestListVisibleSubset(t *testing.T) {
	os.Setenv("CUDA_VISIBLE_DEVICES", "1,3")
	defer os.Unsetenv("CUDA_VISIBLE_DEVICES")
	gpus, err := List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(gpus) != 2 {
		t.Fatalf("expected 2 visible GPUs, got %d", len(gpus))
	}
	if gpus[0].PhysicalIndex != 1 || gpus[1].PhysicalIndex != 3 {
		t.Errorf("physical index mapping wrong: %+v", gpus)
	}
	if gpus[0].Index != 0 || gpus[1].Index != 1 {
		t.Errorf("visible index mapping wrong: %+v", gpus)
	}
}
