package clone

import (
	"bytes"
	"crypto/rand"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// randomBytes returns a byte slice of size n filled with cryptographically
// random data. Used so we can prove dst matches src byte-for-byte even
// when the platform path slices the copy through io.Copy.
func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return b
}

func TestCloneFile_NativeStrategy(t *testing.T) {
	// The platform strategyegy is whichever the build-tagged file selects.
	// We only assert: (1) clone succeeds, (2) dst is byte-identical to src.
	// The strategyegy identity is logged for operators, not gated on here.
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")

	data := randomBytes(t, 8192)
	if err := os.WriteFile(src, data, 0644); err != nil {
		t.Fatal(err)
	}

	strategy, err := CloneFile(src, dst)
	if err != nil {
		t.Fatalf("CloneFile: %v", err)
	}
	if strategy == StrategyUnknown {
		t.Errorf("got StrategyUnknown; want a concrete strategyegy")
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Errorf("dst != src (dst len=%d, src len=%d)", len(got), len(data))
	}
}

func TestCloneFile_Fallback_IOCopy(t *testing.T) {
	// ForceFallback disables all platform primitives so we exercise the
	// universal io.Copy path. This keeps the fallback regression-tested
	// even on hosts where every native strategyegy happens to succeed.
	restore := ForceFallback(true)
	defer restore()

	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")

	data := randomBytes(t, 1<<20) // 1 MiB
	if err := os.WriteFile(src, data, 0644); err != nil {
		t.Fatal(err)
	}

	strategy, err := CloneFile(src, dst)
	if err != nil {
		t.Fatalf("CloneFile: %v", err)
	}
	if strategy != StrategyIOCopy {
		t.Errorf("strategyegy = %v, want StrategyIOCopy", strategy)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Errorf("dst content mismatch after io.Copy fallback")
	}
}

func TestCloneFile_EEXIST(t *testing.T) {
	// Clonefile/FICLONE and our io.Copy fallback all use O_EXCL semantics.
	// A pre-existing dst must produce an error, not silently overwrite.
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")

	if err := os.WriteFile(src, []byte("hello"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte("preexisting"), 0644); err != nil {
		t.Fatal(err)
	}

	_, err := CloneFile(src, dst)
	if err == nil {
		t.Fatalf("expected error for pre-existing dst, got nil")
	}

	// dst must still contain the original content (not the src data).
	got, readErr := os.ReadFile(dst)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(got) != "preexisting" {
		t.Errorf("pre-existing dst was overwritten: got %q", got)
	}
}

func TestCloneFile_EmptyPaths(t *testing.T) {
	if _, err := CloneFile("", "/tmp/x"); err == nil {
		t.Errorf("expected error for empty src")
	}
	if _, err := CloneFile("/tmp/x", ""); err == nil {
		t.Errorf("expected error for empty dst")
	}
}

func TestCloneFile_MissingSource(t *testing.T) {
	dir := t.TempDir()
	_, err := CloneFile(filepath.Join(dir, "nope"), filepath.Join(dir, "dst"))
	if err == nil {
		t.Fatal("expected error for missing source")
	}
}

func TestStrategy_String(t *testing.T) {
	cases := map[Strategy]string{
		StrategyClonefile:     "clonefile",
		StrategyFICLONE:       "ficlone",
		StrategyCopyFileRange: "copy_file_range",
		StrategyIOCopy:        "io_copy",
		StrategyUnknown:       "unknown",
		Strategy(99):          "unknown",
	}
	for s, want := range cases {
		if got := s.String(); got != want {
			t.Errorf("%d.String() = %q, want %q", int(s), got, want)
		}
	}
}

// TestCloneFile_ConcurrentSameDst verifies that two concurrent goroutines
// calling CloneFile on the same dst don't both claim success. Exactly
// one should win; the loser must see an EEXIST-class error and leave the
// winner's data intact.
//
// This matters because the plan documents that higher-level shed-create
// serialization is trusted, but the primitive itself must also be race-safe.
func TestCloneFile_ConcurrentSameDst(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")

	data := randomBytes(t, 16384)
	if err := os.WriteFile(src, data, 0644); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	results := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_, err := CloneFile(src, dst)
			results[idx] = err
		}(i)
	}
	wg.Wait()

	successes := 0
	for _, err := range results {
		if err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Errorf("expected exactly one success, got %d (results: %+v)", successes, results)
	}

	// Winner's data must still be present and correct.
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Errorf("dst content corrupted by concurrent CloneFile")
	}
}
