//go:build darwin

package clone

import (
	"os"
	"path/filepath"
	"testing"
)

// TestCloneFile_Darwin_IsClonefile asserts that on darwin/APFS (the only
// supported VZ configuration) the native strategy is selected. This catches
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
		// Only skip when the filesystem doesn't support clonefile —
		// any other error is a real regression that should fail the
		// test rather than be hidden by t.Skip.
		if isFallback(err) {
			t.Skipf("CloneFile fallback on darwin (non-APFS tmp dir?): %v", err)
		}
		t.Fatalf("CloneFile failed on darwin: %v", err)
	}
	if strategy != StrategyClonefile {
		t.Errorf("strategy = %v, want StrategyClonefile (run on APFS)", strategy)
	}
}
