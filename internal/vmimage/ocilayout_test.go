package vmimage

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
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

// TestReadBlobDetectsLegacyBundledDirectory exercises the v0.4.x → v0.5.0
// upgrade path: a blob slot is a directory (containing manifest.json /
// kernel / initrd / rootfs.ext4) instead of the flat OCI blob file. The
// reader must return a typed sentinel error so the CLI can point at the
// migration docs instead of the misleading "unreadable manifest …: is a
// directory" + "unknown image" fallback.
func TestReadBlobDetectsLegacyBundledDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := EnsureOCILayout(dir); err != nil {
		t.Fatalf("EnsureOCILayout: %v", err)
	}
	hex := strings.Repeat("a", 64)
	digest := DigestPrefix + hex
	bundle := filepath.Join(dir, blobsDir, algorithmDir, hex)
	if err := os.MkdirAll(bundle, 0o755); err != nil {
		t.Fatalf("mkdir bundle: %v", err)
	}
	for _, name := range []string{"manifest.json", "kernel", "initrd", "rootfs.ext4"} {
		if err := os.WriteFile(filepath.Join(bundle, name), []byte("legacy"), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	_, err := ReadBlob(dir, digest)
	if err == nil {
		t.Fatalf("expected error from ReadBlob on bundled directory, got nil")
	}
	if !errors.Is(err, ErrLegacyBundledBlob) {
		t.Fatalf("expected ErrLegacyBundledBlob, got %v", err)
	}
	for _, want := range []string{
		"v0.4.x bundled directory layout",
		"docs/upgrades/v0.4-to-v0.5.md",
		"wipe legacy store",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q missing expected substring %q", err.Error(), want)
		}
	}

	// LoadManifestByDigest goes through ReadBlob and should surface the
	// same sentinel so manager / config callers can branch on it.
	if _, mErr := LoadManifestByDigest(dir, digest); !errors.Is(mErr, ErrLegacyBundledBlob) {
		t.Fatalf("LoadManifestByDigest: expected ErrLegacyBundledBlob, got %v", mErr)
	}

	// OpenBlob takes a different code path; verify it too.
	if _, oErr := OpenBlob(dir, digest); !errors.Is(oErr, ErrLegacyBundledBlob) {
		t.Fatalf("OpenBlob: expected ErrLegacyBundledBlob, got %v", oErr)
	}
}

// TestDetectLegacyBundledBlobNonDirectory confirms we don't accidentally
// flag a real blob file or a missing path as legacy.
func TestDetectLegacyBundledBlobNonDirectory(t *testing.T) {
	dir := t.TempDir()
	// Missing path: not legacy.
	if err := detectLegacyBundledBlob(filepath.Join(dir, "absent")); err != nil {
		t.Fatalf("missing path flagged as legacy: %v", err)
	}
	// Regular file: not legacy.
	regular := filepath.Join(dir, "blob")
	if err := os.WriteFile(regular, []byte("x"), 0o644); err != nil {
		t.Fatalf("write regular: %v", err)
	}
	if err := detectLegacyBundledBlob(regular); err != nil {
		t.Fatalf("regular file flagged as legacy: %v", err)
	}
	// Directory without manifest.json: not legacy.
	emptyDir := filepath.Join(dir, "empty-dir")
	if err := os.MkdirAll(emptyDir, 0o755); err != nil {
		t.Fatalf("mkdir empty: %v", err)
	}
	if err := detectLegacyBundledBlob(emptyDir); err != nil {
		t.Fatalf("empty dir flagged as legacy: %v", err)
	}
}
