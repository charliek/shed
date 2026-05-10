package vmimage

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
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

func (s *fakeScanner) ScanRefs() ([]Reference, error) { return s.refs, s.err }

// installFakeBlob installs a deterministic fake blob with the given
// SourceRef and tag. Returns the digest.
func installFakeBlob(t *testing.T, imagesDir, tag, sourceRef string, body []byte) string {
	t.Helper()
	src := filepath.Join(t.TempDir(), "rootfs.ext4")
	if err := os.WriteFile(src, body, 0o644); err != nil {
		t.Fatal(err)
	}
	digest := DigestPrefix + hex.EncodeToString(sumSha(body))
	if _, _, err := InstallBlob(imagesDir, BlobInstallSpec{
		Files: map[string]string{BlobRootfsFilename: src},
		Manifest: Manifest{
			SchemaVersion:     ManifestSchemaVersion,
			Digest:            digest,
			SourceRef:         sourceRef,
			RootfsLogicalSize: int64(len(body)),
		},
	}); err != nil {
		t.Fatalf("InstallBlob: %v", err)
	}
	if tag != "" {
		if err := SetTag(imagesDir, tag, digest); err != nil {
			t.Fatalf("SetTag: %v", err)
		}
	}
	return digest
}

func sumSha(b []byte) []byte {
	s := sha256.Sum256(b)
	return s[:]
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
	// Install one dangling blob.
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
	if manifest.SourceRef != "ghcr.io/test:v1" {
		t.Errorf("manifest source ref = %q", manifest.SourceRef)
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

	// Dry run: dangling is a candidate; pinned is not.
	cands, err := mgr.PruneImages(true)
	if err != nil {
		t.Fatalf("PruneImages(dryRun): %v", err)
	}
	if len(cands) != 1 {
		t.Fatalf("expected 1 prune candidate, got %d (%#v)", len(cands), cands)
	}
	if cands[0].Digest != danglingDigest {
		t.Errorf("candidate digest = %s, want %s", cands[0].Digest, danglingDigest)
	}

	// Real prune: dangling is removed, pinned remains.
	deleted, err := mgr.PruneImages(false)
	if err != nil {
		t.Fatalf("PruneImages: %v", err)
	}
	if len(deleted) != 1 || deleted[0].Digest != danglingDigest {
		t.Fatalf("unexpected deletions: %#v", deleted)
	}
	if !BlobExists(imagesDir, pinnedDigest) {
		t.Fatalf("pinned blob removed by prune")
	}
	if BlobExists(imagesDir, danglingDigest) {
		t.Fatalf("dangling blob not removed")
	}
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
