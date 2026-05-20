package vmimage

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// testConfig implements ImageConfig for testing.
type testConfig struct {
	images        map[string]string
	imagesDir     string
	baseRootfs    string
	platform      string
	extractKernel bool
	needsInitrd   bool
}

func (c *testConfig) GetImages() map[string]string { return c.images }
func (c *testConfig) GetImagesDir() string         { return c.imagesDir }
func (c *testConfig) GetBaseRootfs() string        { return c.baseRootfs }
func (c *testConfig) GetPlatform() string          { return c.platform }
func (c *testConfig) GetExtractKernel() bool       { return c.extractKernel }
func (c *testConfig) GetNeedsInitrd() bool         { return c.needsInitrd }

// fakeScanner is a static RefScanner used by tests.
type fakeScanner struct {
	refs []Reference
	err  error
}

func (s *fakeScanner) ScanRefs(strict bool) ([]Reference, error) { return s.refs, s.err }

// installFakeBlob installs a synthetic OCI image with the given source
// ref, tagged at `tag`, into imagesDir. Returns the manifest digest.
func installFakeBlob(t *testing.T, imagesDir, tag, sourceRef string, body []byte) string {
	t.Helper()
	digest, err := InstallSyntheticImage(imagesDir, tag, sourceRef, body, nil, nil)
	if err != nil {
		t.Fatalf("InstallSyntheticImage: %v", err)
	}
	return digest
}

func TestManagerListImages(t *testing.T) {
	imagesDir := t.TempDir()
	cfg := &testConfig{
		images:    map[string]string{"default": "ghcr.io/example/default:v1"},
		imagesDir: imagesDir,
	}
	mgr := NewManager(cfg, nil)

	// No tags or blobs yet.
	got, err := mgr.ListImages()
	if err != nil {
		t.Fatalf("ListImages: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("ListImages with empty store = %v, want empty", got)
	}

	// Install one tagged config-managed image.
	installFakeBlob(t, imagesDir, "default", "ghcr.io/example/default:v1", []byte("default-body"))
	// Install one dangling image (no tag).
	installFakeBlob(t, imagesDir, "", "ghcr.io/example/dangling:v1", []byte("dangling-body"))

	got, err = mgr.ListImages()
	if err != nil {
		t.Fatalf("ListImages: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ListImages = %d entries, want 2 (got %#v)", len(got), got)
	}

	var seenConfig, seenDangling bool
	for _, img := range got {
		switch img.Source {
		case "config":
			seenConfig = true
			if img.Tag != "default" {
				t.Errorf("config-managed image Tag = %q, want default", img.Tag)
			}
			if img.DockerRef != "ghcr.io/example/default:v1" {
				t.Errorf("DockerRef = %q, want ghcr.io/example/default:v1", img.DockerRef)
			}
		case "dangling":
			seenDangling = true
			if img.Tag != "" {
				t.Errorf("dangling image Tag = %q, want empty", img.Tag)
			}
		}
	}
	if !seenConfig || !seenDangling {
		t.Fatalf("expected both config + dangling entries, got %#v", got)
	}
}

func TestManagerInspectAndTag(t *testing.T) {
	imagesDir := t.TempDir()
	cfg := &testConfig{imagesDir: imagesDir}
	mgr := NewManager(cfg, nil)

	digest := installFakeBlob(t, imagesDir, "src", "ghcr.io/test:v1", []byte("body"))

	// Inspect by tag.
	info, manifest, err := mgr.InspectImage("src")
	if err != nil {
		t.Fatalf("InspectImage(tag): %v", err)
	}
	if info.Digest != digest {
		t.Errorf("inspect digest = %q, want %q", info.Digest, digest)
	}
	if manifest.ShedSourceRef() != "ghcr.io/test:v1" {
		t.Errorf("manifest source ref = %q", manifest.ShedSourceRef())
	}

	// Inspect by full digest.
	info2, _, err := mgr.InspectImage(digest)
	if err != nil {
		t.Fatalf("InspectImage(digest): %v", err)
	}
	if info2.Digest != digest {
		t.Errorf("inspect-by-digest digest mismatch")
	}

	// Tag points new tag at same digest.
	if err := mgr.TagImage("src", "alias"); err != nil {
		t.Fatalf("TagImage: %v", err)
	}
	t1, _ := GetTag(imagesDir, "src")
	t2, _ := GetTag(imagesDir, "alias")
	if t1.Digest != t2.Digest {
		t.Errorf("alias tag digest mismatch: %s vs %s", t1.Digest, t2.Digest)
	}
}

func TestManagerDeleteImage(t *testing.T) {
	imagesDir := t.TempDir()
	cfg := &testConfig{
		images:    map[string]string{"managed": "ghcr.io/example/managed:v1"},
		imagesDir: imagesDir,
	}
	mgr := NewManager(cfg, nil)

	installFakeBlob(t, imagesDir, "managed", "ghcr.io/example/managed:v1", []byte("a"))
	installFakeBlob(t, imagesDir, "removable", "ghcr.io/example/removable:v1", []byte("b"))

	// Refuse config-managed.
	if err := mgr.DeleteImage("managed"); !errors.Is(err, ErrImageInUse) {
		t.Errorf("DeleteImage(managed) = %v, want ErrImageInUse", err)
	}

	// Removable tag deletes (Docker model: tag removed, blob remains).
	if err := mgr.DeleteImage("removable"); err != nil {
		t.Errorf("DeleteImage(removable): %v", err)
	}
	if _, err := GetTag(imagesDir, "removable"); !errors.Is(err, ErrTagNotFound) {
		t.Errorf("expected tag gone, got %v", err)
	}

	// Missing → ErrImageNotFound.
	if err := mgr.DeleteImage("nonexistent"); !errors.Is(err, ErrImageNotFound) {
		t.Errorf("DeleteImage(nonexistent) = %v, want ErrImageNotFound", err)
	}
}

func TestManagerPruneRefcount(t *testing.T) {
	imagesDir := t.TempDir()
	cfg := &testConfig{imagesDir: imagesDir}

	pinnedDigest := installFakeBlob(t, imagesDir, "pinned", "ref-a", []byte("a"))
	danglingDigest := installFakeBlob(t, imagesDir, "", "ref-b", []byte("b"))

	scanner := &fakeScanner{refs: []Reference{
		{Digest: pinnedDigest, Kind: RefKindShed, Name: "live-shed"},
	}}
	mgr := NewManager(cfg, scanner)

	// Dry run: every blob reachable from the dangling manifest
	// (manifest + config + layer) is a candidate; nothing reachable
	// from the pinned manifest is.
	cands, err := mgr.PruneImages(true)
	if err != nil {
		t.Fatalf("PruneImages(dryRun): %v", err)
	}
	if !hasCandidate(cands, danglingDigest) {
		t.Errorf("dangling manifest %s missing from prune candidates: %#v", danglingDigest, cands)
	}
	if hasCandidate(cands, pinnedDigest) {
		t.Errorf("pinned manifest %s appears in prune candidates: %#v", pinnedDigest, cands)
	}

	// Real prune: every dangling-reachable blob is removed, every
	// pinned-reachable blob remains.
	deleted, err := mgr.PruneImages(false)
	if err != nil {
		t.Fatalf("PruneImages: %v", err)
	}
	if !hasDeleted(deleted, danglingDigest) {
		t.Fatalf("dangling manifest not deleted: %#v", deleted)
	}
	if !BlobExists(imagesDir, pinnedDigest) {
		t.Fatalf("pinned blob removed by prune")
	}
	if BlobExists(imagesDir, danglingDigest) {
		t.Fatalf("dangling blob not removed")
	}
}

// hasCandidate reports whether digest appears in the list of prune
// candidates returned by PruneImages.
func hasCandidate(cands []ImageInfo, digest string) bool {
	for _, c := range cands {
		if c.Digest == digest {
			return true
		}
	}
	return false
}

// hasDeleted reports whether digest appears in the list of deleted
// entries returned by PruneImages.
func hasDeleted(deleted []ImageInfo, digest string) bool {
	for _, d := range deleted {
		if d.Digest == digest {
			return true
		}
	}
	return false
}

func TestManagerPruneRespectsSnapshotRefs(t *testing.T) {
	imagesDir := t.TempDir()
	cfg := &testConfig{imagesDir: imagesDir}

	digest := installFakeBlob(t, imagesDir, "", "ref", []byte("body"))
	scanner := &fakeScanner{refs: []Reference{
		{Digest: digest, Kind: RefKindSnapshot, Name: "snap-1"},
	}}
	mgr := NewManager(cfg, scanner)

	cands, err := mgr.PruneImages(true)
	if err != nil {
		t.Fatalf("PruneImages: %v", err)
	}
	if len(cands) != 0 {
		t.Fatalf("snapshot-pinned blob should NOT be a prune candidate, got %#v", cands)
	}
}

// TestEnsureImageTriesRegistryBeforeDocker is the regression for #98.
// EnsureImage must call PullToOCILayout (registry-direct) before falling
// back to convertAndInstall (docker-export). Before the fix, only the
// docker path was tried, which produced a single-layer flatten with
// Ubuntu's stock initramfs — boot would fail with "No root device".
//
// Test strategy: with a bogus registry ref + no docker daemon reachable
// in the test env, BOTH paths fail. The combined error message
// "registry pull and docker fallback both failed: registry=... docker=..."
// only appears in the new code path. The old code would have returned
// only the docker error.
func TestEnsureImageTriesRegistryBeforeDocker(t *testing.T) {
	imagesDir := t.TempDir()
	cfg := &testConfig{
		imagesDir:     imagesDir,
		platform:      "linux/arm64",
		extractKernel: true,
		needsInitrd:   true,
	}
	mgr := NewManager(cfg, nil)

	if err := EnsureOCILayout(imagesDir); err != nil {
		t.Fatalf("EnsureOCILayout: %v", err)
	}

	// A ref that doesn't exist anywhere. Both registry pull AND docker
	// daemon pull will fail. We don't care which specific errors fire;
	// only that the combined error message proves both paths were
	// attempted in the right order.
	_, err := mgr.EnsureImage(context.Background(), ResolvedRef{
		Name:      "test-prefers-registry",
		DockerRef: "ghcr.io/charliek/this-image-deliberately-does-not-exist:v0.0.0",
	}, nil)
	if err == nil {
		t.Fatal("expected error from EnsureImage against a bogus ref, got nil")
	}
	if !strings.Contains(err.Error(), "registry pull and docker fallback both failed") {
		t.Fatalf("error should indicate both paths were tried (#98 regression).\n  got: %v", err)
	}
}

func TestValidateImageName(t *testing.T) {
	for _, tt := range []struct {
		name    string
		wantErr bool
	}{
		{"valid", false},
		{"valid-name_1.foo", false},
		{"_base", false},
		{"", true},
		{".", true},
		{"..", true},
		{"with/slash", true},
		{"with space", true},
	} {
		err := ValidateImageName(tt.name)
		if (err != nil) != tt.wantErr {
			t.Errorf("ValidateImageName(%q) err=%v, wantErr=%v", tt.name, err, tt.wantErr)
		}
	}
}
