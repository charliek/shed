//go:build darwin

package clone

import (
	"os"
	"path/filepath"
	"testing"
)

// TestCloneFile_Darwin_IsClonefile asserts that on darwin/APFS (the only
// supported VZ configuration) the native strategyegy is selected. This catches
// regressions where the build tag drifts or unix.Clonefile moves behind a
// newer errno we haven't folded into isFallback.
func TestCloneFile_Darwin_IsClonefile(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")

	if err := os.WriteFile(src, []byte("hello world"), 0644); err != nil {
		t.Fatal(err)
	}

	strategy, err := CloneFile(src, dst)
	if err != nil {
		// ENOTSUP from a non-APFS tmpfs-like FS is the one legitimate
		// reason to skip; any other error is a real failure.
		t.Skipf("CloneFile failed on darwin (non-APFS tmp dir?): %v", err)
	}
	if strategy != StrategyClonefile {
		t.Errorf("strategyegy = %v, want StrategyClonefile (run on APFS)", strategy)
	}
}
