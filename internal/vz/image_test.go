//go:build darwin

package vz

import (
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

	client := &Client{
		cfg: cfg,
	}
	return client, imagesDir
}

func createFakeImage(t *testing.T, imagesDir, name string) {
	t.Helper()
	rootfs := filepath.Join(imagesDir, vmimage.RootfsFilename(name))
	if err := os.WriteFile(rootfs, []byte("fake-rootfs"), 0644); err != nil {
		t.Fatalf("failed to create fake image: %v", err)
	}
	source := filepath.Join(imagesDir, vmimage.SourceFilename(name))
	if err := os.WriteFile(source, []byte("ghcr.io/example/"+name+":v1\n"), 0644); err != nil {
		t.Fatalf("failed to create fake source: %v", err)
	}
}

func createFakeInstance(t *testing.T, instanceDir, name, image string) {
	t.Helper()
	dir := filepath.Join(instanceDir, name)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("failed to create instance dir: %v", err)
	}
	meta := Metadata{
		Version: MetadataVersion,
		Name:    name,
		Status:  config.StatusRunning,
		Backend: "vz",
		Image:   image,
	}
	data, _ := json.MarshalIndent(meta, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, metadataFilename), data, 0644); err != nil {
		t.Fatalf("failed to write metadata: %v", err)
	}
}

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
			name:      "shed-referenced image refused",
			imageName: "inuse",
			setup: func(t *testing.T, client *Client, imagesDir string) {
				createFakeImage(t, imagesDir, "inuse")
				createFakeInstance(t, client.cfg.InstanceDir, "myshed", "inuse")
			},
			wantErr: config.ErrImageInUseSentinel,
		},
		{
			name:      "nonexistent image",
			imageName: "nonexistent",
			wantErr:   config.ErrImageNotFoundSentinel,
		},
		{
			name:      "empty name",
			imageName: "",
			wantErr:   nil, // generic error, not sentinel
		},
		{
			name:      "path traversal rejected",
			imageName: "../etc/passwd",
			wantErr:   nil, // generic error
		},
		{
			name:      "path separator rejected",
			imageName: "foo/bar",
			wantErr:   nil, // generic error
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

			// For validation errors (empty name, path traversal), just check error is non-nil
			if tt.imageName == "" || tt.imageName == "../etc/passwd" || tt.imageName == "foo/bar" {
				if err == nil {
					t.Errorf("DeleteImage(%q) should have returned an error", tt.imageName)
				}
				return
			}

			// Successful delete
			if err != nil {
				t.Fatalf("DeleteImage(%q) unexpected error: %v", tt.imageName, err)
			}

			// Verify files are gone
			rootfs := filepath.Join(imagesDir, vmimage.RootfsFilename(tt.imageName))
			if _, err := os.Stat(rootfs); !os.IsNotExist(err) {
				t.Errorf("rootfs file should be deleted: %s", rootfs)
			}
			source := filepath.Join(imagesDir, vmimage.SourceFilename(tt.imageName))
			if _, err := os.Stat(source); !os.IsNotExist(err) {
				t.Errorf("source file should be deleted: %s", source)
			}

			// Verify lock file is NOT touched (if it existed)
			// Lock file wouldn't exist in this test, but verify we don't create artifacts
		})
	}
}

func TestDeleteImage_PreservesLockFile(t *testing.T) {
	client, imagesDir := newTestClient(t)
	createFakeImage(t, imagesDir, "testimg")

	// Create a lock file
	lockPath := filepath.Join(imagesDir, vmimage.RootfsFilename("testimg")+".lock")
	if err := os.WriteFile(lockPath, []byte{}, 0644); err != nil {
		t.Fatalf("failed to create lock file: %v", err)
	}

	if err := client.DeleteImage("testimg"); err != nil {
		t.Fatalf("DeleteImage error: %v", err)
	}

	// Lock file must still exist
	if _, err := os.Stat(lockPath); os.IsNotExist(err) {
		t.Error("lock file should NOT be deleted")
	}
}

func TestPruneImages(t *testing.T) {
	t.Run("prunes only unused images", func(t *testing.T) {
		client, imagesDir := newTestClient(t)

		// Create images: one managed (should be excluded), one used by shed, one unused
		createFakeImage(t, imagesDir, "managed")
		createFakeImage(t, imagesDir, "shedref")
		createFakeImage(t, imagesDir, "unused1")
		createFakeImage(t, imagesDir, "unused2")
		createFakeImage(t, imagesDir, "_base")

		// Create a shed referencing "shedref"
		createFakeInstance(t, client.cfg.InstanceDir, "myshed", "shedref")

		deleted, err := client.PruneImages(false)
		if err != nil {
			t.Fatalf("PruneImages error: %v", err)
		}

		// Should only delete unused1 and unused2
		if len(deleted) != 2 {
			t.Fatalf("expected 2 deleted, got %d: %+v", len(deleted), deleted)
		}

		// Should be sorted
		if deleted[0].Name != "unused1" || deleted[1].Name != "unused2" {
			t.Errorf("expected [unused1, unused2], got [%s, %s]", deleted[0].Name, deleted[1].Name)
		}

		// Verify unused files are gone
		for _, name := range []string{"unused1", "unused2"} {
			rootfs := filepath.Join(imagesDir, vmimage.RootfsFilename(name))
			if _, err := os.Stat(rootfs); !os.IsNotExist(err) {
				t.Errorf("%s rootfs should be deleted", name)
			}
		}

		// Verify protected files still exist
		for _, name := range []string{"managed", "shedref", "_base"} {
			rootfs := filepath.Join(imagesDir, vmimage.RootfsFilename(name))
			if _, err := os.Stat(rootfs); err != nil {
				t.Errorf("%s rootfs should still exist: %v", name, err)
			}
		}
	})

	t.Run("dry run returns candidates without deleting", func(t *testing.T) {
		client, imagesDir := newTestClient(t)
		createFakeImage(t, imagesDir, "candidate")

		candidates, err := client.PruneImages(true)
		if err != nil {
			t.Fatalf("PruneImages(dryRun=true) error: %v", err)
		}

		if len(candidates) != 1 || candidates[0].Name != "candidate" {
			t.Errorf("expected [candidate], got %+v", candidates)
		}

		// File should still exist
		rootfs := filepath.Join(imagesDir, vmimage.RootfsFilename("candidate"))
		if _, err := os.Stat(rootfs); err != nil {
			t.Errorf("candidate rootfs should still exist after dry run: %v", err)
		}
	})

	t.Run("empty images dir returns nil", func(t *testing.T) {
		client, _ := newTestClient(t)
		deleted, err := client.PruneImages(false)
		if err != nil {
			t.Fatalf("PruneImages error: %v", err)
		}
		if len(deleted) != 0 {
			t.Errorf("expected empty result, got %d", len(deleted))
		}
	})

	t.Run("_base with non-docker BaseRootfs is prunable", func(t *testing.T) {
		client, imagesDir := newTestClient(t)
		client.cfg.BaseRootfs = "/path/to/local.ext4" // not a docker ref
		createFakeImage(t, imagesDir, "_base")

		deleted, err := client.PruneImages(false)
		if err != nil {
			t.Fatalf("PruneImages error: %v", err)
		}
		if len(deleted) != 1 || deleted[0].Name != "_base" {
			t.Errorf("expected [_base], got %+v", deleted)
		}
	})
}
