package vmimage

import (
	"testing"
)

// TestRefIndexGetDecoupledFromSourceRef is the regression guard for the
// divergent-ref bug: an image pulled by a mutable tag / digest pin / mirror
// host carries a publish-time io.shed.source-ref that differs from the ref the
// operator configured. RefIndexGet must hit on the ref it was PUT under,
// regardless of the manifest's source-ref annotation, or every create
// re-pulls (missing) or errors (never) despite the image being local.
func TestRefIndexGetDecoupledFromSourceRef(t *testing.T) {
	imagesDir := t.TempDir()
	// Manifest published as :v0.6.0 (its source-ref), but pulled by :latest.
	digest := installFakeBlob(t, imagesDir, "full", "ghcr.io/x/y:v0.6.0", []byte("body"))

	pulledRef := "ghcr.io/x/y:latest"
	if err := RefIndexPut(imagesDir, pulledRef, digest); err != nil {
		t.Fatalf("RefIndexPut: %v", err)
	}

	got, ok := RefIndexGet(imagesDir, pulledRef)
	if !ok {
		t.Fatal("RefIndexGet missed for the ref it was PUT under (source-ref mismatch must not delete the entry)")
	}
	if got != digest {
		t.Errorf("RefIndexGet = %q, want %q", got, digest)
	}
}

// TestRefIndexGetDropsStaleDigest confirms a missing blob (GC'd digest) is
// treated as a miss and the stale entry removed.
func TestRefIndexGetDropsStaleDigest(t *testing.T) {
	imagesDir := t.TempDir()
	if err := RefIndexPut(imagesDir, "ghcr.io/x/y:v1", "sha256:"+stale64); err != nil {
		t.Fatalf("RefIndexPut: %v", err)
	}
	if _, ok := RefIndexGet(imagesDir, "ghcr.io/x/y:v1"); ok {
		t.Error("RefIndexGet hit on a digest with no blob present")
	}
}

const stale64 = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"

// TestRefIndexDeleteByDigest removes every entry pointing at a digest.
func TestRefIndexDeleteByDigest(t *testing.T) {
	imagesDir := t.TempDir()
	d := installFakeBlob(t, imagesDir, "full", "ghcr.io/x/y:v1", []byte("b"))
	// Two refs converge on the same digest.
	_ = RefIndexPut(imagesDir, "ghcr.io/x/y:v1", d)
	_ = RefIndexPut(imagesDir, "ghcr.io/x/y:latest", d)

	RefIndexDeleteByDigest(imagesDir, d)

	if _, ok := RefIndexGet(imagesDir, "ghcr.io/x/y:v1"); ok {
		t.Error("entry for :v1 survived DeleteByDigest")
	}
	if _, ok := RefIndexGet(imagesDir, "ghcr.io/x/y:latest"); ok {
		t.Error("entry for :latest survived DeleteByDigest")
	}
}

// TestPruneProtectsConfiguredRefViaPullIndex is the regression guard for the
// prune use-after-free: a configured default_image installed via PullImage
// (not a create-triggered EnsureImage) and stripped of its cosmetic tag must
// still be protected from prune, because `shed create` would resolve it via
// the ref-index.
func TestPruneProtectsConfiguredRefViaPullIndex(t *testing.T) {
	imagesDir := t.TempDir()
	cfg := &testConfig{
		defaultImage: "ghcr.io/x/configured:v1",
		imagesDir:    imagesDir,
	}
	mgr := NewManager(cfg, &fakeScanner{}) // no shed/snapshot refs

	// Install the configured image and record it in the ref-index the way
	// PullImage would, then drop the cosmetic tag so only ref-index
	// protection remains.
	digest := installFakeBlob(t, imagesDir, "configured", "ghcr.io/x/configured:v1", []byte("body"))
	if err := RefIndexPut(imagesDir, "ghcr.io/x/configured:v1", digest); err != nil {
		t.Fatalf("RefIndexPut: %v", err)
	}
	if err := DeleteTag(imagesDir, "configured"); err != nil {
		t.Fatalf("DeleteTag: %v", err)
	}

	cands, err := mgr.PruneImages(true)
	if err != nil {
		t.Fatalf("PruneImages(dryRun): %v", err)
	}
	for _, c := range cands {
		if c.Digest == digest {
			t.Fatalf("configured default_image %s is a prune candidate (use-after-free): %#v", digest, cands)
		}
	}
}
