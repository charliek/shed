package vmimage

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCacheLowerExists(t *testing.T) {
	tmp := t.TempDir()
	if err := EnsureOCILayout(tmp); err != nil {
		t.Fatalf("EnsureOCILayout: %v", err)
	}
	digest := DigestBytes([]byte("test"))

	if CacheLowerExists(tmp, digest) {
		t.Error("CacheLowerExists with no files should return false")
	}

	erofs, _ := CacheLowerPath(tmp, digest)
	_ = os.MkdirAll(filepath.Dir(erofs), 0o755)
	_ = os.WriteFile(erofs, []byte("x"), 0o444)
	if !CacheLowerExists(tmp, digest) {
		t.Error("CacheLowerExists should accept .erofs")
	}
}
