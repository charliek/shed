//go:build darwin

package vz

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/vmimage"
)

func newTestClient(t *testing.T) (*Client, string) {
	t.Helper()
	imagesDir := t.TempDir()
	instanceDir := t.TempDir()
	snapshotsDir := t.TempDir()

	cfg := &config.VZConfig{
		ImagesDir:    imagesDir,
		InstanceDir:  instanceDir,
		SnapshotsDir: snapshotsDir,
		Images: map[string]string{
			"managed": "ghcr.io/example/managed:v1",
		},
		BaseRootfs: "ghcr.io/example/base:v1",
	}

	client := &Client{cfg: cfg}
	return client, imagesDir
}

// createFakeSnapshot writes a minimal snapshot.json with the given
// LowerDigest so the refScanner sees a snapshot-kind protective ref.
// Used by TestPruneImagesProtectsSnapshotPin to confirm snapshots can
// keep a blob alive after every shed that referenced it has been
// deleted — the exact "lower digest stays cached for snapshot restore"
// guarantee the storage rewrite committed to.
func createFakeSnapshot(t *testing.T, snapshotsDir, name, lowerDigest string) {
	t.Helper()
	dir := filepath.Join(snapshotsDir, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("failed to create snapshot dir: %v", err)
	}
	snap := config.Snapshot{
		Version:     config.SnapshotSchemaVersion,
		Name:        name,
		Backend:     config.BackendVZ,
		LowerDigest: lowerDigest,
	}
	data, _ := json.MarshalIndent(snap, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "snapshot.json"), data, 0o644); err != nil {
		t.Fatalf("failed to write snapshot.json: %v", err)
	}
}

// createFakeImage installs a fake blob into imagesDir tagged as `name`.
// Returns the digest.
func createFakeImage(t *testing.T, imagesDir, name string) string {
	t.Helper()
	stagingDir := t.TempDir()
	src := filepath.Join(stagingDir, "rootfs.ext4")
	body := []byte("fake-rootfs-" + name)
	if err := os.WriteFile(src, body, 0o644); err != nil {
		t.Fatalf("failed to write staging rootfs: %v", err)
	}
	sum := sha256.Sum256(body)
	digest := vmimage.DigestPrefix + hex.EncodeToString(sum[:])
	if _, _, err := vmimage.InstallBlob(imagesDir, vmimage.BlobInstallSpec{
		Files: map[string]string{vmimage.BlobRootfsFilename: src},
		Manifest: vmimage.Manifest{
			SchemaVersion:     vmimage.ManifestSchemaVersion,
			Digest:            digest,
			SourceRef:         "ghcr.io/example/" + name + ":v1",
			RootfsLogicalSize: int64(len(body)),
		},
	}); err != nil {
		t.Fatalf("InstallBlob: %v", err)
	}
	if err := vmimage.SetTag(imagesDir, name, digest); err != nil {
		t.Fatalf("SetTag: %v", err)
	}
	return digest
}

// createFakeInstance writes a minimal v2 metadata.json with the given
// LowerDigest so the refScanner sees a protective ref.
func createFakeInstance(t *testing.T, instanceDir, name, image, lowerDigest string) {
	t.Helper()
	dir := filepath.Join(instanceDir, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("failed to create instance dir: %v", err)
	}
	meta := Metadata{
		Version:     MetadataVersion,
		Name:        name,
		Status:      config.StatusRunning,
		Backend:     "vz",
		Image:       image,
		LowerDigest: lowerDigest,
	}
	data, _ := json.MarshalIndent(meta, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, metadataFilename), data, 0o644); err != nil {
		t.Fatalf("failed to write metadata: %v", err)
	}
}

// TestDeleteImage covers tag-level removal under the Docker model:
// `image rm` drops the tag but leaves the blob; refusal of config-managed
// tags works as before.
func TestDeleteImage(t *testing.T) {
	tests := []struct {
		name      string
		imageName string
		setup     func(t *testing.T, client *Client, imagesDir string)
		wantErr   error
	}{
		{
			name:      "successful delete",
			imageName: "deleteme",
			setup: func(t *testing.T, _ *Client, imagesDir string) {
				createFakeImage(t, imagesDir, "deleteme")
			},
		},
		{
			name:      "config-managed image refused",
			imageName: "managed",
			wantErr:   config.ErrImageInUseSentinel,
		},
		{
			name:      "_base with docker ref refused",
			imageName: "_base",
			setup: func(t *testing.T, _ *Client, imagesDir string) {
				createFakeImage(t, imagesDir, "_base")
			},
			wantErr: config.ErrImageInUseSentinel,
		},
		{
			name:      "nonexistent image",
			imageName: "nonexistent",
			wantErr:   config.ErrImageNotFoundSentinel,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client, imagesDir := newTestClient(t)
			if tt.setup != nil {
				tt.setup(t, client, imagesDir)
			}

			err := client.DeleteImage(tt.imageName)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Errorf("DeleteImage(%q) error = %v, want %v", tt.imageName, err, tt.wantErr)
				}
				return
			}

			if err != nil {
				t.Fatalf("DeleteImage(%q) unexpected error: %v", tt.imageName, err)
			}
			if _, err := vmimage.GetTag(imagesDir, tt.imageName); !errors.Is(err, vmimage.ErrTagNotFound) {
				t.Errorf("expected tag %q removed, got %v", tt.imageName, err)
			}
		})
	}
}

// TestPruneImagesRespectsRefs verifies blob GC: the refScanner builds
// protective references from existing sheds, and prune skips digests
// covered by any ref.
func TestPruneImagesRespectsRefs(t *testing.T) {
	client, imagesDir := newTestClient(t)

	// One blob is pinned by an existing shed, one is dangling.
	pinnedDigest := createFakeImage(t, imagesDir, "pinned")
	danglingDigest := createFakeImage(t, imagesDir, "dangling")
	createFakeInstance(t, client.cfg.InstanceDir, "live-shed", "pinned", pinnedDigest)

	// Dry-run reports the dangling blob only.
	cands, err := client.PruneImages(true)
	if err != nil {
		t.Fatalf("PruneImages(dryRun): %v", err)
	}
	if len(cands) != 1 || cands[0].Digest != danglingDigest {
		t.Fatalf("expected only dangling blob as candidate, got %#v", cands)
	}

	// Real prune removes the dangling blob; pinned remains.
	deleted, err := client.PruneImages(false)
	if err != nil {
		t.Fatalf("PruneImages: %v", err)
	}
	if len(deleted) != 1 || deleted[0].Digest != danglingDigest {
		t.Fatalf("unexpected deletions: %#v", deleted)
	}
	// PruneImages reports what it intended to delete; assert the blob
	// is actually gone from disk so a regression that drops the
	// DeleteBlob call is caught.
	if vmimage.BlobExists(imagesDir, danglingDigest) {
		t.Fatalf("dangling blob still exists after prune")
	}
	if !vmimage.BlobExists(imagesDir, pinnedDigest) {
		t.Fatalf("pinned blob removed by prune")
	}
}

// TestPruneImagesProtectsSnapshotPin confirms that a snapshot's
// LowerDigest counts as a protective reference even when no shed
// pins it — the snapshot is supposed to keep its source blob
// reclaimable for `shed create --from-snapshot` later. Without
// this guarantee, the snapshot rewrite (Phase C) is meaningless:
// pruning would delete the only blob a snapshot could spawn from.
func TestPruneImagesProtectsSnapshotPin(t *testing.T) {
	client, imagesDir := newTestClient(t)

	// Set up two blobs: one referenced only by a snapshot, one truly
	// dangling. NO shed pins either one — this is the
	// "all shed refs removed, snapshot remains" scenario.
	snapshotPinned := createFakeImage(t, imagesDir, "snap-pinned")
	dangling := createFakeImage(t, imagesDir, "dangling")
	createFakeSnapshot(t, client.cfg.SnapshotsDir, "preserved-snap", snapshotPinned)

	// Drop the tags so the only protection on snapshotPinned is the
	// snapshot reference (tags don't protect in Docker model).
	if err := vmimage.DeleteTag(imagesDir, "snap-pinned"); err != nil {
		t.Fatalf("DeleteTag: %v", err)
	}
	if err := vmimage.DeleteTag(imagesDir, "dangling"); err != nil {
		t.Fatalf("DeleteTag: %v", err)
	}

	// Dry-run: snapshotPinned is NOT a candidate; dangling is.
	cands, err := client.PruneImages(true)
	if err != nil {
		t.Fatalf("PruneImages(dryRun): %v", err)
	}
	if len(cands) != 1 || cands[0].Digest != dangling {
		t.Fatalf("expected only dangling blob as candidate; got %#v (snapshot-pinned digest %s should not appear)", cands, snapshotPinned)
	}

	// Real prune: dangling removed, snapshot-pinned blob still present.
	if _, err := client.PruneImages(false); err != nil {
		t.Fatalf("PruneImages: %v", err)
	}
	if vmimage.BlobExists(imagesDir, dangling) {
		t.Fatalf("dangling blob still exists after prune")
	}
	if !vmimage.BlobExists(imagesDir, snapshotPinned) {
		t.Fatalf("snapshot-pinned blob removed by prune; shed create --from-snapshot would now fail")
	}
}
