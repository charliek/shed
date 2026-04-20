//go:build linux

package clone

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// TestCloneFile_Linux_Strategy confirms CloneFile picks FICLONE or
// copy_file_range when available on this FS. On CI filesystems like
// tmpfs/overlay that don't support FICLONE, copy_file_range is the
// expected answer.
func TestCloneFile_Linux_Strategy(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")

	data := []byte("hello world, from linux clone")
	if err := os.WriteFile(src, data, 0644); err != nil {
		t.Fatal(err)
	}

	strategy, err := CloneFile(src, dst)
	if err != nil {
		t.Fatalf("CloneFile: %v", err)
	}
	// Accept any of FICLONE / copy_file_range / io_copy — filesystems
	// vary widely. The fallback chain is the guarantee, not the exact
	// winner. We only fail if we got Unknown.
	if strategy == StrategyUnknown {
		t.Errorf("strategy = Unknown; want a concrete strategy")
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Errorf("dst content mismatch")
	}
}

// TestCloneFile_Linux_CopyFileRange_ShortCopyLoop exercises
// tryCopyFileRange with a large file so the kernel's internal chunking
// guarantees multiple iterations of the copy loop.
func TestCloneFile_Linux_CopyFileRange_ShortCopyLoop(t *testing.T) {
	// Force FICLONE to fall through so we land on copy_file_range directly.
	// An easy way is to use a filesystem that almost certainly doesn't
	// support reflink (tmpfs via t.TempDir) — but we can't guarantee
	// that, so the assertion tolerates io_copy too. The important check
	// is content integrity across a file large enough to span multiple
	// kernel calls (a few MiB is enough on most kernels).
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")

	const size = 4 * 1024 * 1024
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i)
	}
	if err := os.WriteFile(src, data, 0644); err != nil {
		t.Fatal(err)
	}

	strategy, err := CloneFile(src, dst)
	if err != nil {
		t.Fatalf("CloneFile: %v", err)
	}
	if strategy == StrategyUnknown {
		t.Fatalf("strategy = Unknown")
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Errorf("4 MiB clone dst mismatch (strategy=%s)", strategy)
	}
}
