package vmimage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestEnsureOCILayoutCreatesMarkerAndIndex(t *testing.T) {
	dir := t.TempDir()
	if err := EnsureOCILayout(dir); err != nil {
		t.Fatalf("EnsureOCILayout: %v", err)
	}

	// oci-layout marker
	data, err := os.ReadFile(filepath.Join(dir, "oci-layout"))
	if err != nil {
		t.Fatalf("oci-layout missing: %v", err)
	}
	var marker OCILayoutMarker
	if err := json.Unmarshal(data, &marker); err != nil {
		t.Fatalf("parse oci-layout: %v", err)
	}
	if marker.ImageLayoutVersion != "1.0.0" {
		t.Fatalf("unexpected layout version %q", marker.ImageLayoutVersion)
	}

	// index.json
	idx, err := ReadIndex(dir)
	if err != nil {
		t.Fatalf("ReadIndex: %v", err)
	}
	if idx.SchemaVersion != 2 {
		t.Fatalf("expected index schemaVersion 2, got %d", idx.SchemaVersion)
	}
	if len(idx.Manifests) != 0 {
		t.Fatalf("expected empty index, got %d manifests", len(idx.Manifests))
	}

	// blobs and cache subdirs
	for _, sub := range []string{"blobs/sha256", "cache/sha256", "tags"} {
		if _, err := os.Stat(filepath.Join(dir, sub)); err != nil {
			t.Fatalf("missing %s: %v", sub, err)
		}
	}

	// Re-running should be a no-op.
	if err := EnsureOCILayout(dir); err != nil {
		t.Fatalf("re-running EnsureOCILayout: %v", err)
	}
}

func TestWriteBlobRejectsMismatchedDigest(t *testing.T) {
	dir := t.TempDir()
	if err := EnsureOCILayout(dir); err != nil {
		t.Fatalf("EnsureOCILayout: %v", err)
	}
	bogus := "sha256:" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	_, err := WriteBlob(dir, bogus, []byte("hello"))
	if err == nil {
		t.Fatalf("expected mismatched-digest error, got nil")
	}
}

func TestWriteBlobRoundTrip(t *testing.T) {
	dir := t.TempDir()
	content := []byte(`{"hello":"world"}`)
	digest := DigestBytes(content)
	path, err := WriteBlob(dir, digest, content)
	if err != nil {
		t.Fatalf("WriteBlob: %v", err)
	}
	if !BlobExists(dir, digest) {
		t.Fatalf("BlobExists returned false for %s", digest)
	}
	read, err := ReadBlob(dir, digest)
	if err != nil {
		t.Fatalf("ReadBlob: %v", err)
	}
	if string(read) != string(content) {
		t.Fatalf("content round-trip mismatch")
	}
	if filepath.Base(path) == "" {
		t.Fatalf("BlobPath returned empty filename")
	}
}

func TestSyntheticImageInstall(t *testing.T) {
	dir := t.TempDir()
	rootfs := []byte("rootfs-bytes")
	kernel := []byte("kernel-bytes")
	initrd := []byte("initrd-bytes")

	manifestDigest, err := InstallSyntheticImage(dir, "test", "ghcr.io/example/img:v1", rootfs, kernel, initrd)
	if err != nil {
		t.Fatalf("InstallSyntheticImage: %v", err)
	}
	if !BlobExists(dir, manifestDigest) {
		t.Fatalf("manifest blob missing")
	}
	tag, err := GetTag(dir, "test")
	if err != nil {
		t.Fatalf("GetTag: %v", err)
	}
	if tag.Digest != manifestDigest {
		t.Fatalf("tag digest %s != manifest %s", tag.Digest, manifestDigest)
	}
	manifest, err := LoadManifestByDigest(dir, manifestDigest)
	if err != nil {
		t.Fatalf("LoadManifestByDigest: %v", err)
	}
	if manifest.ShedSourceRef() != "ghcr.io/example/img:v1" {
		t.Fatalf("source ref annotation lost: %q", manifest.ShedSourceRef())
	}
	if manifest.ShedKernelDigest() == "" {
		t.Fatalf("kernel digest annotation missing")
	}
	if manifest.ShedInitrdDigest() == "" {
		t.Fatalf("initrd digest annotation missing")
	}
	if len(manifest.Layers) != 1 {
		t.Fatalf("expected one layer, got %d", len(manifest.Layers))
	}
}
