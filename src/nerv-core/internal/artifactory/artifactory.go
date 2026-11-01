// Package artifactory persists what the consumer-AI writes (SPECS.md §5).
// Path naming is the consumer's: the core no longer indexes or reserves names
// (M10) — the sh layer archives a colliding old file before a tool overwrites.
package artifactory

import (
	"os"
	"path/filepath"
)

// Artifactory is the art-project repository root.
type Artifactory struct {
	Root string
}

// New returns an Artifactory rooted at root (cleaned).
func New(root string) *Artifactory {
	return &Artifactory{Root: filepath.Clean(root)}
}

// Abs joins a declared artifactory-relative path onto the root.
func (a *Artifactory) Abs(rel string) string {
	return filepath.Join(a.Root, rel)
}

// EnsureOutputDir creates the parent directory of a declared output path
// (artifactory-relative) so the tool script can write straight into it.
func (a *Artifactory) EnsureOutputDir(rel string) error {
	return os.MkdirAll(filepath.Dir(filepath.Join(a.Root, rel)), 0o755)
}
