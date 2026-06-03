package vmimage

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// testConfig implements ImageConfig for testing.
type testConfig struct {
	defaultImage  string
	imageAliases  map[string]string
	pullPolicy    string
	imagesDir     string
	platform      string
	extractKernel bool
	needsInitrd   bool
}

func (c *testConfig) GetDefaultImage() string            { return c.defaultImage }
func (c *testConfig) GetImageAliases() map[string]string { return c.imageAliases }
func (c *testConfig) GetPullPolicy() string              { return c.pullPolicy }
func (c *testConfig) GetImagesDir() string               { return c.imagesDir }
func (c *testConfig) GetPlatform() string                { return c.platform }
func (c *testConfig) GetExtractKernel() bool             { return c.extractKernel }
func (c *testConfig) GetNeedsInitrd() bool               { return c.needsInitrd }

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
		defaultImage: "ghcr.io/example/default:v1",
		imagesDir:    imagesDir,
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

// TestListImagesDisplaysConfiguredRefForDivergentTag guards the ref-keyed
// display fix: an image pulled by a mutable tag (whose manifest source-ref is
// the publish version) must be listed by the CONFIGURED ref and classified
// "config" — not by the publish source-ref, which would drop it from the
// config bucket.
func TestListImagesDisplaysConfiguredRefForDivergentTag(t *testing.T) {
	imagesDir := t.TempDir()
	// Manifest published as :v0.6.0, but the operator configured/pulled :latest.
	digest := installFakeBlob(t, imagesDir, "full", "ghcr.io/x/y:v0.6.0", []byte("body"))
	if err := RefIndexPut(imagesDir, "ghcr.io/x/y:latest", digest); err != nil {
		t.Fatalf("RefIndexPut: %v", err)
	}
	cfg := &testConfig{defaultImage: "ghcr.io/x/y:latest", imagesDir: imagesDir}
	mgr := NewManager(cfg, nil)

	got, err := mgr.ListImages()
	if err != nil {
		t.Fatalf("ListImages: %v", err)
	}
	var found *ImageInfo
	for i := range got {
		if got[i].Digest == digest {
			found = &got[i]
		}
	}
	if found == nil {
		t.Fatalf("manifest %s not listed: %#v", digest, got)
	}
	if found.DockerRef != "ghcr.io/x/y:latest" {
		t.Errorf("DockerRef = %q, want the configured ref ghcr.io/x/y:latest (not the publish source-ref)", found.DockerRef)
	}
	if found.Source != "config" {
		t.Errorf("Source = %q, want config", found.Source)
	}
}

// TestListImagesAliasAndDefault pins the picker-enabling metadata: a
// config image carries its friendly image_aliases key, exactly the
// default_image entry reports IsDefault, and a user-pulled image (neither
// default nor aliased) carries neither.
func TestListImagesAliasAndDefault(t *testing.T) {
	imagesDir := t.TempDir()
	cfg := &testConfig{
		defaultImage: "ghcr.io/example/full:v1",
		imageAliases: map[string]string{
			"full": "ghcr.io/example/full:v1", // also the default
			"base": "ghcr.io/example/base:v1",
		},
		imagesDir: imagesDir,
	}
	mgr := NewManager(cfg, nil)

	installFakeBlob(t, imagesDir, "full", "ghcr.io/example/full:v1", []byte("full-body"))
	installFakeBlob(t, imagesDir, "base", "ghcr.io/example/base:v1", []byte("base-body"))
	// A user-pulled image: addressable by its ref-index entry, not configured.
	pulled := installFakeBlob(t, imagesDir, "scratch", "ghcr.io/example/scratch:v1", []byte("scratch-body"))
	if err := RefIndexPut(imagesDir, "ghcr.io/example/scratch:v1", pulled); err != nil {
		t.Fatalf("RefIndexPut: %v", err)
	}

	got, err := mgr.ListImages()
	if err != nil {
		t.Fatalf("ListImages: %v", err)
	}
	byRef := map[string]ImageInfo{}
	for _, img := range got {
		byRef[img.DockerRef] = img
	}

	if d := byRef["ghcr.io/example/full:v1"]; d.Alias != "full" || !d.IsDefault {
		t.Errorf("default image: Alias=%q IsDefault=%v, want full/true (%#v)", d.Alias, d.IsDefault, d)
	}
	if a := byRef["ghcr.io/example/base:v1"]; a.Alias != "base" || a.IsDefault {
		t.Errorf("alias image: Alias=%q IsDefault=%v, want base/false (%#v)", a.Alias, a.IsDefault, a)
	}
	if u := byRef["ghcr.io/example/scratch:v1"]; u.Alias != "" || u.IsDefault || u.Source != "user" {
		t.Errorf("user image: Alias=%q IsDefault=%v Source=%q, want \"\"/false/user (%#v)", u.Alias, u.IsDefault, u.Source, u)
	}

	// InspectImage must agree with ListImages on the alias/default metadata.
	di, _, err := mgr.InspectImage("full")
	if err != nil {
		t.Fatalf("InspectImage(full): %v", err)
	}
	if di.Alias != "full" || !di.IsDefault {
		t.Errorf("inspect default: Alias=%q IsDefault=%v, want full/true (%#v)", di.Alias, di.IsDefault, di)
	}
	bi, _, err := mgr.InspectImage("base")
	if err != nil {
		t.Fatalf("InspectImage(base): %v", err)
	}
	if bi.Alias != "base" || bi.IsDefault {
		t.Errorf("inspect alias: Alias=%q IsDefault=%v, want base/false (%#v)", bi.Alias, bi.IsDefault, bi)
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
		defaultImage: "ghcr.io/example/managed:v1",
		imagesDir:    imagesDir,
	}

	managedDigest := installFakeBlob(t, imagesDir, "managed", "ghcr.io/example/managed:v1", []byte("a"))
	installFakeBlob(t, imagesDir, "removable", "ghcr.io/example/removable:v1", []byte("b"))

	// A manifest pinned by a live shed/snapshot is hard-blocked.
	pinnedMgr := NewManager(cfg, &fakeScanner{refs: []Reference{{Digest: managedDigest, Kind: RefKindShed, Name: "s1"}}})
	if err := pinnedMgr.DeleteImage("managed"); !errors.Is(err, ErrImageInUse) {
		t.Errorf("DeleteImage(pinned) = %v, want ErrImageInUse", err)
	}

	// Without a live ref, the configured default_image is NOT hard-blocked
	// (warn-and-confirm is a CLI concern); delete untags it (blob remains).
	mgr := NewManager(cfg, nil)
	if err := mgr.DeleteImage("managed"); err != nil {
		t.Errorf("DeleteImage(managed, unpinned): %v", err)
	}
	if _, err := GetTag(imagesDir, "managed"); !errors.Is(err, ErrTagNotFound) {
		t.Errorf("expected managed tag gone, got %v", err)
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

// TestPruneProtectsTaggedManifest covers the v0.5.7 → v0.5.8 prune
// fix: a tag pointing at a manifest now keeps that manifest (and
// every blob the manifest references — config, layers, kernel,
// initrd, and the rootfs erofs blob) alive across `shed image
// prune`. Before the fix, `shed image pull X && shed image prune`
// would delete the manifest just pulled. See refs.go RefKindTag and
// docs/upgrades/v0.5.7-to-v0.5.8.md.
func TestPruneProtectsTaggedManifest(t *testing.T) {
	imagesDir := t.TempDir()
	cfg := &testConfig{imagesDir: imagesDir}
	mgr := NewManager(cfg, nil)

	digest, err := InstallSyntheticImage(
		imagesDir,
		"protected",
		"ghcr.io/example/protected:v1",
		[]byte("rootfs"),
		[]byte("kernel"),
		[]byte("initrd"),
	)
	if err != nil {
		t.Fatalf("InstallSyntheticImage: %v", err)
	}

	manifest, err := LoadManifestByDigest(imagesDir, digest)
	if err != nil {
		t.Fatalf("LoadManifestByDigest: %v", err)
	}

	// Dry run should leave the tagged manifest out of the candidate set.
	cands, err := mgr.PruneImages(true)
	if err != nil {
		t.Fatalf("PruneImages(dryRun): %v", err)
	}
	if hasCandidate(cands, digest) {
		t.Fatalf("tagged manifest %s appears in prune candidates: %#v", digest, cands)
	}

	// Real prune should leave the manifest, its config, every layer,
	// kernel, initrd, and the erofs rootfs blob on disk.
	if _, err := mgr.PruneImages(false); err != nil {
		t.Fatalf("PruneImages: %v", err)
	}
	if !BlobExists(imagesDir, digest) {
		t.Fatalf("tagged manifest blob removed by prune: %s", digest)
	}
	if !BlobExists(imagesDir, manifest.Config.Digest) {
		t.Fatalf("config blob for tagged manifest removed: %s", manifest.Config.Digest)
	}
	for _, layer := range manifest.Layers {
		if !BlobExists(imagesDir, layer.Digest) {
			t.Fatalf("layer blob for tagged manifest removed: %s", layer.Digest)
		}
	}
	if d := manifest.ShedKernelDigest(); d != "" && !BlobExists(imagesDir, d) {
		t.Fatalf("kernel blob for tagged manifest removed: %s", d)
	}
	if d := manifest.ShedInitrdDigest(); d != "" && !BlobExists(imagesDir, d) {
		t.Fatalf("initrd blob for tagged manifest removed: %s", d)
	}
	if d := manifest.ShedRootfsErofsDigest(); d != "" && !BlobExists(imagesDir, d) {
		t.Fatalf("rootfs erofs blob for tagged manifest removed: %s", d)
	}

	// Tag file itself must remain.
	if _, err := GetTag(imagesDir, "protected"); err != nil {
		t.Fatalf("tag should still exist post-prune: %v", err)
	}
}

// TestPruneAfterUntagDeletesOrphan covers the documented cleanup
// workflow: `shed image rm <name>` to drop the tag, then `shed
// image prune` to GC the now-orphaned manifest and its transitive
// blobs. Verifies the tag is what's protective; removing the tag
// hands the underlying blobs back to prune.
func TestPruneAfterUntagDeletesOrphan(t *testing.T) {
	imagesDir := t.TempDir()
	cfg := &testConfig{imagesDir: imagesDir}
	mgr := NewManager(cfg, nil)

	digest, err := InstallSyntheticImage(
		imagesDir,
		"orphan-soon",
		"ghcr.io/example/orphan-soon:v1",
		[]byte("rootfs"),
		[]byte("kernel"),
		nil,
	)
	if err != nil {
		t.Fatalf("InstallSyntheticImage: %v", err)
	}

	manifest, err := LoadManifestByDigest(imagesDir, digest)
	if err != nil {
		t.Fatalf("LoadManifestByDigest: %v", err)
	}

	// Untag and prune.
	if err := mgr.DeleteImage("orphan-soon"); err != nil {
		t.Fatalf("DeleteImage: %v", err)
	}
	deleted, err := mgr.PruneImages(false)
	if err != nil {
		t.Fatalf("PruneImages: %v", err)
	}
	if !hasDeleted(deleted, digest) {
		t.Fatalf("orphaned manifest %s not in deleted set: %#v", digest, deleted)
	}
	if BlobExists(imagesDir, digest) {
		t.Fatalf("orphaned manifest blob still present after prune: %s", digest)
	}
	for _, layer := range manifest.Layers {
		if BlobExists(imagesDir, layer.Digest) {
			t.Fatalf("orphaned layer blob still present after prune: %s", layer.Digest)
		}
	}
	if d := manifest.ShedKernelDigest(); d != "" && BlobExists(imagesDir, d) {
		t.Fatalf("orphaned kernel blob still present after prune: %s", d)
	}
	if d := manifest.ShedRootfsErofsDigest(); d != "" && BlobExists(imagesDir, d) {
		t.Fatalf("orphaned rootfs erofs blob still present after prune: %s", d)
	}
}

// TestPruneHandlesStaleTag covers the failure mode where the
// underlying manifest blob is missing (host disk corruption,
// partial restore from backup, etc.). Prune must log a warning and
// proceed instead of crashing or treating the missing digest as
// protective.
func TestPruneHandlesStaleTag(t *testing.T) {
	imagesDir := t.TempDir()
	cfg := &testConfig{imagesDir: imagesDir}
	mgr := NewManager(cfg, nil)

	// Install a real image to give prune some blobs to walk past.
	realDigest := installFakeBlob(t, imagesDir, "real", "ghcr.io/example/real:v1", []byte("real-body"))

	// Write a tag file directly pointing at a fabricated digest whose
	// blob has never been written to disk. SetTag's digest validator
	// accepts well-formed sha256:<hex> regardless of whether the blob
	// exists, so this is the production stale-tag shape.
	staleDigest := SyntheticDigestFromString("stale-manifest-never-on-disk")
	if err := SetTag(imagesDir, "stale", staleDigest); err != nil {
		t.Fatalf("SetTag(stale): %v", err)
	}
	if BlobExists(imagesDir, staleDigest) {
		t.Fatalf("test setup invalid: stale digest blob unexpectedly present: %s", staleDigest)
	}

	// Prune must not panic, must not error, and must leave the real
	// manifest's blobs alone.
	if _, err := mgr.PruneImages(false); err != nil {
		t.Fatalf("PruneImages with stale tag returned err: %v", err)
	}
	if !BlobExists(imagesDir, realDigest) {
		t.Fatalf("real manifest %s deleted while a stale tag was present", realDigest)
	}
}

// TestPruneStillProtectsShedPinnedManifest is a regression check that
// scanner-supplied RefKindShed refs continue to be protective after
// the v0.5.8 tag-protection change. The fixture matches the
// production shape: a manifest with kernel + initrd + erofs blobs,
// pinned by a shed but with no tag.
func TestPruneStillProtectsShedPinnedManifest(t *testing.T) {
	imagesDir := t.TempDir()
	cfg := &testConfig{imagesDir: imagesDir}

	digest, err := InstallSyntheticImage(
		imagesDir,
		"",
		"ghcr.io/example/shed-pinned:v1",
		[]byte("shed-pinned-rootfs"),
		[]byte("shed-pinned-kernel"),
		[]byte("shed-pinned-initrd"),
	)
	if err != nil {
		t.Fatalf("InstallSyntheticImage: %v", err)
	}
	manifest, err := LoadManifestByDigest(imagesDir, digest)
	if err != nil {
		t.Fatalf("LoadManifestByDigest: %v", err)
	}

	scanner := &fakeScanner{refs: []Reference{
		{Digest: digest, Kind: RefKindShed, Name: "live-shed"},
	}}
	mgr := NewManager(cfg, scanner)

	if _, err := mgr.PruneImages(false); err != nil {
		t.Fatalf("PruneImages: %v", err)
	}
	if !BlobExists(imagesDir, digest) {
		t.Fatalf("shed-pinned manifest deleted by prune: %s", digest)
	}
	if !BlobExists(imagesDir, manifest.Config.Digest) {
		t.Fatalf("shed-pinned config deleted: %s", manifest.Config.Digest)
	}
	if d := manifest.ShedRootfsErofsDigest(); d != "" && !BlobExists(imagesDir, d) {
		t.Fatalf("shed-pinned erofs deleted: %s", d)
	}
}

// TestEnsureImageSurfacesRegistryError confirms that a registry pull
// failure surfaces directly to the caller — v0.5.3 dropped the
// docker-daemon fallback (it produced single-layer flattens with
// Ubuntu's stock initramfs that couldn't boot via shed-overlay), so
// the only sensible behavior on a registry miss is to fail fast.
// Previously this test verified the "tried registry first, then
// docker" ordering (#98); after the fallback removal the ordering
// is degenerate and the error message we assert on changed.
func TestEnsureImageSurfacesRegistryError(t *testing.T) {
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

	_, err := mgr.EnsureImage(context.Background(), ResolvedRef{
		Name:      "test-registry-only",
		DockerRef: "ghcr.io/charliek/this-image-deliberately-does-not-exist:v0.0.0",
	}, nil)
	if err == nil {
		t.Fatal("expected error from EnsureImage against a bogus ref, got nil")
	}
	if !strings.Contains(err.Error(), "pulling ghcr.io/charliek/this-image-deliberately-does-not-exist:v0.0.0 from registry") {
		t.Fatalf("error should surface the registry pull verbatim — no silent fallback.\n  got: %v", err)
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
