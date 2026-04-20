package diskstat

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStat_DenseFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dense")

	data := make([]byte, 64*1024) // 64 KB
	for i := range data {
		data[i] = byte(i)
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	logical, physical, err := Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if logical != int64(len(data)) {
		t.Errorf("logical = %d, want %d", logical, len(data))
	}
	// Physical should be >= logical for a dense file, allowing FS overhead.
	if physical < logical {
		t.Errorf("physical (%d) < logical (%d) for dense file", physical, logical)
	}
}

func TestStat_SparseFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sparse")

	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Create a 20 MiB sparse file with no actual data written.
	// Some filesystems (e.g., ZFS-on-Linux, tmpfs) don't preserve sparseness
	// via Truncate; skip the assertion if the kernel materialized it.
	const sparseSize = 20 * 1024 * 1024
	if err := f.Truncate(sparseSize); err != nil {
		f.Close()
		t.Fatalf("truncate: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	logical, physical, err := Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if logical != sparseSize {
		t.Errorf("logical = %d, want %d", logical, sparseSize)
	}
	if physical >= logical {
		t.Skipf("filesystem does not preserve sparseness (physical=%d, logical=%d)", physical, logical)
	}
}

func TestStat_Missing(t *testing.T) {
	_, _, err := Stat(filepath.Join(t.TempDir(), "does-not-exist"))
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestStat_Symlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	link := filepath.Join(dir, "link")

	data := make([]byte, 8192)
	if err := os.WriteFile(target, data, 0644); err != nil {
		t.Fatalf("write target: %v", err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	// Stat follows symlinks — should report the target's size.
	logical, _, err := Stat(link)
	if err != nil {
		t.Fatalf("Stat symlink: %v", err)
	}
	if logical != int64(len(data)) {
		t.Errorf("logical via symlink = %d, want %d", logical, len(data))
	}
}
