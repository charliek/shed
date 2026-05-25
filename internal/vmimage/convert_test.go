package vmimage

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
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

// TestResolveTagAndBlob exercises the tag → manifest → cached-ext4
// resolution path used by VM start. Installs a synthetic OCI image
// (manifest + config + layer + cached ext4) under a tag, then verifies
// Resolve / ResolveTag honor the source-ref check and surface the path
// to the first layer's cached ext4.
func TestResolveTagAndBlob(t *testing.T) {
	dir := t.TempDir()

	rootfsContent := []byte("rootfs-bytes-for-resolve-test")
	digest, err := InstallSyntheticImage(dir, "default", "ghcr.io/test:v1", rootfsContent, nil, nil)
	if err != nil {
		t.Fatalf("InstallSyntheticImage: %v", err)
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

// TestBuildShedAnnotationsSourceRef pins down the contract that the
// io.shed.source-ref annotation reads back EXACTLY the DockerRef the
// caller passed in. The server's resolveImage cache lookup compares
// `manifest.ShedSourceRef() == cfg.ref`, so any transformation here
// (prefixing, lowercasing, dropping the registry) would silently
// break post-publish cache hits and force a re-pull on every
// `shed create`. Locking the round-trip here catches that class of
// regression before it reaches a live shed.
func TestBuildShedAnnotationsSourceRef(t *testing.T) {
	tests := []struct {
		name      string
		sourceRef string
	}{
		{name: "registry ref with version tag", sourceRef: "ghcr.io/foo/bar:v1"},
		{name: "registry ref with full version", sourceRef: "ghcr.io/charliek/shed-vz-full:v0.5.0"},
		{name: "local buildx tag", sourceRef: "shed-vz-full:latest"},
		{name: "digest ref", sourceRef: "ghcr.io/foo/bar@sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ann := buildShedAnnotations("full", tt.sourceRef, "", "", "", "")
			if got := ann[AnnotationSourceRef]; got != tt.sourceRef {
				t.Errorf("AnnotationSourceRef = %q, want %q", got, tt.sourceRef)
			}
			// Round-trip through the parser to mirror the runtime path
			// (Convert writes the manifest blob; the server later reads
			// it back via ParseManifest before calling ShedSourceRef).
			m := &OCIManifest{
				SchemaVersion: 2,
				MediaType:     MediaTypeOCIManifest,
				Layers:        []Descriptor{{MediaType: MediaTypeOCILayer, Digest: "sha256:deadbeef", Size: 1}},
				Annotations:   ann,
			}
			data, err := m.MarshalIndent()
			if err != nil {
				t.Fatalf("MarshalIndent: %v", err)
			}
			parsed, err := ParseManifest(data)
			if err != nil {
				t.Fatalf("ParseManifest: %v", err)
			}
			if got := parsed.ShedSourceRef(); got != tt.sourceRef {
				t.Errorf("parsed ShedSourceRef() = %q, want %q", got, tt.sourceRef)
			}
		})
	}
}

// TestBuildShedAnnotationsOptionalFields verifies that empty optional
// values are omitted from the annotation map (so the on-disk manifest
// JSON doesn't carry confusing empty-string entries).
func TestBuildShedAnnotationsOptionalFields(t *testing.T) {
	ann := buildShedAnnotations("base", "ghcr.io/x/y:v1", "", "", "", "")
	if _, ok := ann[AnnotationKernelDigest]; ok {
		t.Errorf("empty kernel digest should be omitted, got %q", ann[AnnotationKernelDigest])
	}
	if _, ok := ann[AnnotationInitrdDigest]; ok {
		t.Errorf("empty initrd digest should be omitted, got %q", ann[AnnotationInitrdDigest])
	}
	if _, ok := ann[AnnotationRootfsErofsDigest]; ok {
		t.Errorf("empty rootfs erofs digest should be omitted, got %q", ann[AnnotationRootfsErofsDigest])
	}
	if _, ok := ann[AnnotationRootfsLogicalSize]; ok {
		t.Errorf("empty rootfs logical size should be omitted, got %q", ann[AnnotationRootfsLogicalSize])
	}

	ann2 := buildShedAnnotations("base", "ghcr.io/x/y:v1", "sha256:k", "sha256:i", "sha256:e", "12345")
	if ann2[AnnotationKernelDigest] != "sha256:k" {
		t.Errorf("kernel digest = %q, want sha256:k", ann2[AnnotationKernelDigest])
	}
	if ann2[AnnotationInitrdDigest] != "sha256:i" {
		t.Errorf("initrd digest = %q, want sha256:i", ann2[AnnotationInitrdDigest])
	}
	if ann2[AnnotationRootfsErofsDigest] != "sha256:e" {
		t.Errorf("rootfs erofs digest = %q, want sha256:e", ann2[AnnotationRootfsErofsDigest])
	}
	if ann2[AnnotationRootfsLogicalSize] != "12345" {
		t.Errorf("rootfs logical size = %q, want 12345", ann2[AnnotationRootfsLogicalSize])
	}
}
