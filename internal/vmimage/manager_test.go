package vmimage

import (
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

func newTestManager(t *testing.T) (*Manager, string) {
	t.Helper()
	imagesDir := t.TempDir()

	cfg := &testConfig{
		images: map[string]string{
			"managed": "ghcr.io/example/managed:v1",
		},
		imagesDir:  imagesDir,
		baseRootfs: "ghcr.io/example/base:v1",
		platform:   "linux/amd64",
	}

	mgr := NewManager(cfg)
	return mgr, imagesDir
}

func createFakeImage(t *testing.T, imagesDir, name string) {
	t.Helper()
	rootfs := filepath.Join(imagesDir, RootfsFilename(name))
	if err := os.WriteFile(rootfs, []byte("fake-rootfs"), 0644); err != nil {
		t.Fatalf("failed to create fake image: %v", err)
	}
	source := filepath.Join(imagesDir, SourceFilename(name))
	if err := os.WriteFile(source, []byte("ghcr.io/example/"+name+":v1\n"), 0644); err != nil {
		t.Fatalf("failed to create fake source: %v", err)
	}
}

func noInUseNames() ([]string, error) {
	return nil, nil
}

func inUseNames(names ...string) func() ([]string, error) {
	return func() ([]string, error) {
		return names, nil
	}
}

func TestDeleteImage(t *testing.T) {
	tests := []struct {
		name       string
		imageName  string
		setup      func(t *testing.T, mgr *Manager, imagesDir string)
		inUseNames func() ([]string, error)
		wantErr    error
		wantAnyErr bool
	}{
		{
			name:       "successful delete",
			imageName:  "deleteme",
			inUseNames: noInUseNames,
			setup: func(t *testing.T, _ *Manager, imagesDir string) {
				createFakeImage(t, imagesDir, "deleteme")
			},
		},
		{
			name:       "config-managed image refused",
			imageName:  "managed",
			inUseNames: noInUseNames,
			wantErr:    ErrImageInUse,
		},
		{
			name:       "_base with docker ref refused",
			imageName:  "_base",
			inUseNames: noInUseNames,
			setup: func(t *testing.T, _ *Manager, imagesDir string) {
				createFakeImage(t, imagesDir, "_base")
			},
			wantErr: ErrImageInUse,
		},
		{
			name:       "shed-referenced image refused",
			imageName:  "inuse",
			inUseNames: inUseNames("inuse"),
			setup: func(t *testing.T, _ *Manager, imagesDir string) {
				createFakeImage(t, imagesDir, "inuse")
			},
			wantErr: ErrImageInUse,
		},
		{
			name:       "nonexistent image",
			imageName:  "nonexistent",
			inUseNames: noInUseNames,
			wantErr:    ErrImageNotFound,
		},
		{
			name:       "empty name",
			imageName:  "",
			inUseNames: noInUseNames,
			wantAnyErr: true,
		},
		{
			name:       "path traversal rejected",
			imageName:  "../etc/passwd",
			inUseNames: noInUseNames,
			wantAnyErr: true,
		},
		{
			name:       "path separator rejected",
			imageName:  "foo/bar",
			inUseNames: noInUseNames,
			wantAnyErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mgr, imagesDir := newTestManager(t)
			if tt.setup != nil {
				tt.setup(t, mgr, imagesDir)
			}

			err := mgr.DeleteImage(tt.imageName, tt.inUseNames)

			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Errorf("DeleteImage(%q) error = %v, want %v", tt.imageName, err, tt.wantErr)
				}
				return
			}

			if tt.wantAnyErr {
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
			rootfs := filepath.Join(imagesDir, RootfsFilename(tt.imageName))
			if _, err := os.Stat(rootfs); !os.IsNotExist(err) {
				t.Errorf("rootfs file should be deleted: %s", rootfs)
			}
			source := filepath.Join(imagesDir, SourceFilename(tt.imageName))
			if _, err := os.Stat(source); !os.IsNotExist(err) {
				t.Errorf("source file should be deleted: %s", source)
			}
		})
	}
}

func TestDeleteImage_PreservesLockFile(t *testing.T) {
	mgr, imagesDir := newTestManager(t)
	createFakeImage(t, imagesDir, "testimg")

	// Create a lock file
	lockPath := filepath.Join(imagesDir, RootfsFilename("testimg")+".lock")
	if err := os.WriteFile(lockPath, []byte{}, 0644); err != nil {
		t.Fatalf("failed to create lock file: %v", err)
	}

	if err := mgr.DeleteImage("testimg", noInUseNames); err != nil {
		t.Fatalf("DeleteImage error: %v", err)
	}

	// Lock file must still exist
	if _, err := os.Stat(lockPath); os.IsNotExist(err) {
		t.Error("lock file should NOT be deleted")
	}
}

func TestPruneImages(t *testing.T) {
	t.Run("prunes only unused images", func(t *testing.T) {
		mgr, imagesDir := newTestManager(t)

		// Create images: one managed (should be excluded), one used by shed, one unused
		createFakeImage(t, imagesDir, "managed")
		createFakeImage(t, imagesDir, "shedref")
		createFakeImage(t, imagesDir, "unused1")
		createFakeImage(t, imagesDir, "unused2")
		createFakeImage(t, imagesDir, "_base")

		deleted, err := mgr.PruneImages(false, inUseNames("shedref"))
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
			rootfs := filepath.Join(imagesDir, RootfsFilename(name))
			if _, err := os.Stat(rootfs); !os.IsNotExist(err) {
				t.Errorf("%s rootfs should be deleted", name)
			}
		}

		// Verify protected files still exist
		for _, name := range []string{"managed", "shedref", "_base"} {
			rootfs := filepath.Join(imagesDir, RootfsFilename(name))
			if _, err := os.Stat(rootfs); err != nil {
				t.Errorf("%s rootfs should still exist: %v", name, err)
			}
		}
	})

	t.Run("dry run returns candidates without deleting", func(t *testing.T) {
		mgr, imagesDir := newTestManager(t)
		createFakeImage(t, imagesDir, "candidate")

		candidates, err := mgr.PruneImages(true, noInUseNames)
		if err != nil {
			t.Fatalf("PruneImages(dryRun=true) error: %v", err)
		}

		if len(candidates) != 1 || candidates[0].Name != "candidate" {
			t.Errorf("expected [candidate], got %+v", candidates)
		}

		// File should still exist
		rootfs := filepath.Join(imagesDir, RootfsFilename("candidate"))
		if _, err := os.Stat(rootfs); err != nil {
			t.Errorf("candidate rootfs should still exist after dry run: %v", err)
		}
	})

	t.Run("empty images dir returns nil", func(t *testing.T) {
		mgr, _ := newTestManager(t)
		deleted, err := mgr.PruneImages(false, noInUseNames)
		if err != nil {
			t.Fatalf("PruneImages error: %v", err)
		}
		if len(deleted) != 0 {
			t.Errorf("expected empty result, got %d", len(deleted))
		}
	})

	t.Run("_base with non-docker BaseRootfs is prunable", func(t *testing.T) {
		mgr, imagesDir := newTestManager(t)
		mgr.cfg.(*testConfig).baseRootfs = "/path/to/local.ext4" // not a docker ref
		createFakeImage(t, imagesDir, "_base")

		deleted, err := mgr.PruneImages(false, noInUseNames)
		if err != nil {
			t.Fatalf("PruneImages error: %v", err)
		}
		if len(deleted) != 1 || deleted[0].Name != "_base" {
			t.Errorf("expected [_base], got %+v", deleted)
		}
	})
}

func TestValidateImageName(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{"valid", "myimage", false},
		{"valid with hyphen", "my-image", false},
		{"valid with underscore", "my_image", false},
		{"empty", "", true},
		{"dot-dot", "..", true},
		{"contains dot-dot", "foo..bar", true},
		{"path separator", "foo/bar", true},
		{"leading dot-dot slash", "../etc", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateImageName(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateImageName(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			}
		})
	}
}

func TestListImages(t *testing.T) {
	t.Run("lists config and discovered images", func(t *testing.T) {
		mgr, imagesDir := newTestManager(t)

		// Create a discovered image
		createFakeImage(t, imagesDir, "discovered")

		// Create the managed image with matching cache
		createFakeImage(t, imagesDir, "managed")

		images, err := mgr.ListImages()
		if err != nil {
			t.Fatalf("ListImages error: %v", err)
		}

		// Should have config image (managed) and discovered image
		if len(images) != 2 {
			t.Fatalf("expected 2 images, got %d: %+v", len(images), images)
		}

		// Find each image
		var foundManaged, foundDiscovered bool
		for _, img := range images {
			switch img.Name {
			case "managed":
				foundManaged = true
				if img.Source != "config" {
					t.Errorf("managed image source = %q, want config", img.Source)
				}
				if img.DockerRef != "ghcr.io/example/managed:v1" {
					t.Errorf("managed image DockerRef = %q", img.DockerRef)
				}
			case "discovered":
				foundDiscovered = true
				if img.Source != "discovered" {
					t.Errorf("discovered image source = %q, want discovered", img.Source)
				}
				if !img.Cached {
					t.Error("discovered image should be cached")
				}
			}
		}

		if !foundManaged {
			t.Error("managed image not found")
		}
		if !foundDiscovered {
			t.Error("discovered image not found")
		}
	})

	t.Run("skips _base in discovery", func(t *testing.T) {
		mgr, imagesDir := newTestManager(t)
		createFakeImage(t, imagesDir, "_base")

		images, err := mgr.ListImages()
		if err != nil {
			t.Fatalf("ListImages error: %v", err)
		}

		// Should only have the config image (managed), not _base
		for _, img := range images {
			if img.Name == "_base" {
				t.Error("_base should be skipped in discovery")
			}
		}
	})
}
