package vmimage

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestInstallBlobRejectsTraversalKeys confirms a caller cannot smuggle
// a path-traversal payload into spec.Files. Each key flows straight
// into filepath.Join(tmpDir, ...) inside InstallBlob; a key like
// "../tags/x.json" would otherwise be able to overwrite sibling state.
func TestInstallBlobRejectsTraversalKeys(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "rootfs.ext4")
	if err := os.WriteFile(src, []byte("real-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	digest, err := HashFile(src)
	if err != nil {
		t.Fatalf("HashFile: %v", err)
	}

	for _, badKey := range []string{
		"../etc/passwd",
		"sub/dir/file",
		"./rootfs.ext4",
		"",
	} {
		t.Run("rejects:"+badKey, func(t *testing.T) {
			_, _, err := InstallBlob(dir, BlobInstallSpec{
				Files: map[string]string{
					BlobRootfsFilename: src,
					badKey:             src,
				},
				Manifest: Manifest{
					SchemaVersion: ManifestSchemaVersion,
					Digest:        digest,
				},
			})
			if err == nil {
				t.Fatalf("InstallBlob accepted bad key %q", badKey)
			}
			if !strings.Contains(err.Error(), "file key") {
				t.Errorf("error %q does not mention the key-validation failure", err.Error())
			}
		})
	}

	// Unknown bare-filename keys are also rejected.
	t.Run("rejects:unknown-bare-filename", func(t *testing.T) {
		_, _, err := InstallBlob(dir, BlobInstallSpec{
			Files: map[string]string{
				BlobRootfsFilename: src,
				"extra.dat":        src,
			},
			Manifest: Manifest{
				SchemaVersion: ManifestSchemaVersion,
				Digest:        digest,
			},
		})
		if err == nil {
			t.Fatalf("InstallBlob accepted unsupported filename")
		}
		if !strings.Contains(err.Error(), "unsupported file") {
			t.Errorf("error %q does not mention unsupported file", err.Error())
		}
	})
}
