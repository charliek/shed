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

	cfg := &config.VZConfig{
		ImagesDir:   imagesDir,
		InstanceDir: instanceDir,
		Images: map[string]string{
			"managed": "ghcr.io/example/managed:v1",
		},
		BaseRootfs: "ghcr.io/example/base:v1",
	}

	client := &Client{cfg: cfg}
	return client, imagesDir
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
	if !vmimage.BlobExists(imagesDir, pinnedDigest) {
		t.Fatalf("pinned blob removed by prune")
	}
}
