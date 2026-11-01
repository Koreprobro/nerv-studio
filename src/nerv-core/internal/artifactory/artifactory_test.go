package artifactory

import (
	"os"
	"path/filepath"
	"testing"
)

// TestAbs verifies root composition.
func TestAbs(t *testing.T) {
	af := New("/root/art")
	if got, want := af.Abs("scene/x.png"), filepath.Join("/root/art", "scene/x.png"); got != want {
		t.Fatalf("Abs: got %q want %q", got, want)
	}
}

// TestEnsureOutputDir verifies the parent directory of a declared output path
// is created so the tool script can write straight into it.
func TestEnsureOutputDir(t *testing.T) {
	dir := t.TempDir()
	af := New(dir)
	if err := af.EnsureOutputDir("scene/deep/x.png"); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(filepath.Join(dir, "scene", "deep"))
	if err != nil || !st.IsDir() {
		t.Fatalf("output dir not created: %v", err)
	}
}
