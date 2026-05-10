package vmimage

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIsDockerRef(t *testing.T) {
	tests := []struct {
		name string
		s    string
		want bool
	}{
		// Valid Docker refs
		{name: "registry with tag", s: "ghcr.io/charliek/shed-vz-base:v1.0.0", want: true},
		{name: "registry with latest", s: "ghcr.io/charliek/shed-vz-base:latest", want: true},
		{name: "simple image with tag", s: "ubuntu:24.04", want: true},
		{name: "simple image latest", s: "ubuntu:latest", want: true},
		{name: "bare image name", s: "ubuntu", want: true},
		{name: "localhost registry", s: "localhost:5000/myimage:v1", want: true},
		{name: "company registry", s: "registry.company.com/shed-custom:latest", want: true},
		{name: "digest ref", s: "ghcr.io/charliek/shed-vz-base@sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", want: true},
		{name: "nested path", s: "registry.co/org/team/image:v1", want: true},

		// Filesystem paths (not Docker refs)
		{name: "absolute path", s: "/var/lib/shed/rootfs.ext4", want: false},
		{name: "home dir path", s: "~/Library/Application Support/shed/vz/default-rootfs.ext4", want: false},
		{name: "relative path", s: "./my-image.ext4", want: false},
		{name: "empty string", s: "", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsDockerRef(tt.s)
			if got != tt.want {
				t.Errorf("IsDockerRef(%q) = %v, want %v", tt.s, got, tt.want)
			}
		})
	}
}

// TestHashFileDeterminism verifies that the digest of a fixture file is
// stable across calls — the foundation of the content-addressed store.
func TestHashFileDeterminism(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rootfs.ext4")
	body := []byte("the-quick-brown-fox-jumps-over-the-lazy-dog")
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}

	d1, err := HashFile(path)
	if err != nil {
		t.Fatalf("HashFile: %v", err)
	}
	d2, err := HashFile(path)
	if err != nil {
		t.Fatalf("HashFile: %v", err)
	}
	if d1 != d2 {
		t.Fatalf("HashFile not deterministic: %s vs %s", d1, d2)
	}

	want := DigestPrefix + hex.EncodeToString(sumBytes(body))
	if d1 != want {
		t.Fatalf("HashFile = %s, want %s", d1, want)
	}
}

func TestResolveTagAndBlob(t *testing.T) {
	dir := t.TempDir()

	// Install a blob.
	body := []byte("rootfs-bytes")
	src := filepath.Join(dir, "src-rootfs.ext4")
	if err := os.WriteFile(src, body, 0o644); err != nil {
		t.Fatal(err)
	}
	digest := DigestPrefix + hex.EncodeToString(sumBytes(body))
	if _, _, err := InstallBlob(dir, BlobInstallSpec{
		Files: map[string]string{BlobRootfsFilename: src},
		Manifest: Manifest{
			SchemaVersion:     ManifestSchemaVersion,
			Digest:            digest,
			SourceRef:         "ghcr.io/test:v1",
			RootfsLogicalSize: int64(len(body)),
		},
	}); err != nil {
		t.Fatal(err)
	}

	// Tag it.
	if err := SetTag(dir, "default", digest); err != nil {
		t.Fatalf("SetTag: %v", err)
	}

	// Resolve hits when expectedRef matches.
	if got := Resolve(dir, "default", "ghcr.io/test:v1"); got == "" {
		t.Fatalf("Resolve: cache miss after install")
	}

	// Resolve misses on stale ref.
	if got := Resolve(dir, "default", "ghcr.io/test:v2"); got != "" {
		t.Fatalf("Resolve: stale ref should miss, got %q", got)
	}

	// Resolve hits when expectedRef is empty (skip check).
	if got := Resolve(dir, "default", ""); got == "" {
		t.Fatalf("Resolve(empty ref): expected hit")
	}

	gotDigest, gotPath, err := ResolveTag(dir, "default")
	if err != nil {
		t.Fatalf("ResolveTag: %v", err)
	}
	if gotDigest != digest {
		t.Fatalf("ResolveTag digest = %s, want %s", gotDigest, digest)
	}
	if gotPath == "" {
		t.Fatalf("ResolveTag path empty")
	}
}

func sumBytes(b []byte) []byte {
	s := sha256.Sum256(b)
	return s[:]
}

// TestInstallBlobRejectsDigestMismatch confirms the install-time guard:
// if the manifest's claimed digest doesn't match sha256(rootfs.ext4),
// install fails before anything is written into the blob store. This
// preserves the "blobs/sha256/<digest>/rootfs.ext4 always hashes to
// <digest>" invariant the rest of the system depends on.
func TestInstallBlobRejectsDigestMismatch(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.ext4")
	if err := os.WriteFile(src, []byte("real-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Wrong digest: pretend this is a 64-zero-hex blob. InstallBlob
	// must hash the actual file and reject the mismatch.
	wrongDigest := DigestPrefix + "0000000000000000000000000000000000000000000000000000000000000000"
	_, _, err := InstallBlob(dir, BlobInstallSpec{
		Files: map[string]string{BlobRootfsFilename: src},
		Manifest: Manifest{
			SchemaVersion: ManifestSchemaVersion,
			Digest:        wrongDigest,
			SourceRef:     "ghcr.io/test:wrong",
		},
	})
	if err == nil {
		t.Fatalf("InstallBlob accepted mismatched digest; want error")
	}
	if !strings.Contains(err.Error(), "manifest says") {
		t.Errorf("error %q does not mention digest mismatch", err.Error())
	}
}
