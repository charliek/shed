package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/charliek/shed/internal/vmimage"
)

// TestRunImageInstall covers the install subcommand end-to-end:
// hashing, blob install, tag advancement, and --consume.
func TestRunImageInstall(t *testing.T) {
	tmp := t.TempDir()
	rootfsBody := []byte("test-rootfs-bytes")
	rootfsPath := filepath.Join(tmp, "rootfs.ext4")
	if err := os.WriteFile(rootfsPath, rootfsBody, 0o644); err != nil {
		t.Fatalf("write rootfs: %v", err)
	}
	kernelPath := filepath.Join(tmp, "kernel")
	if err := os.WriteFile(kernelPath, []byte("test-kernel"), 0o644); err != nil {
		t.Fatalf("write kernel: %v", err)
	}
	initrdPath := filepath.Join(tmp, "initrd")
	if err := os.WriteFile(initrdPath, []byte("test-initrd"), 0o644); err != nil {
		t.Fatalf("write initrd: %v", err)
	}
	imagesDir := filepath.Join(tmp, "imagesdir")

	// Populate the package-scope flag vars (cobra would otherwise do
	// this from the command line).
	imageInstallRootfs = rootfsPath
	imageInstallKernel = kernelPath
	imageInstallInitrd = initrdPath
	imageInstallTag = "default"
	imageInstallBackend = "vz"
	imageInstallArch = "arm64"
	imageInstallSourceRef = "ghcr.io/example/test:v1"
	imageInstallImagesDir = imagesDir
	imageInstallConsume = false // don't eat the source files
	t.Cleanup(func() {
		imageInstallRootfs = ""
		imageInstallKernel = ""
		imageInstallInitrd = ""
		imageInstallTag = ""
		imageInstallBackend = ""
		imageInstallArch = ""
		imageInstallSourceRef = ""
		imageInstallImagesDir = ""
		imageInstallConsume = false
	})

	if err := runImageInstall(nil, nil); err != nil {
		t.Fatalf("runImageInstall: %v", err)
	}

	// Verify the tag resolves and the blob layout is what we expect.
	expectedDigest, err := vmimage.HashFile(rootfsPath)
	if err != nil {
		t.Fatalf("HashFile: %v", err)
	}
	tag, err := vmimage.GetTag(imagesDir, "default")
	if err != nil {
		t.Fatalf("GetTag: %v", err)
	}
	if tag.Digest != expectedDigest {
		t.Errorf("tag.Digest = %q, want %q", tag.Digest, expectedDigest)
	}

	if !vmimage.BlobExists(imagesDir, expectedDigest) {
		t.Errorf("blob not installed at %s", expectedDigest)
	}

	manifest, err := vmimage.LoadManifest(imagesDir, expectedDigest)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if manifest.Backend != "vz" || manifest.Arch != "arm64" {
		t.Errorf("manifest backend/arch = %q/%q, want vz/arm64", manifest.Backend, manifest.Arch)
	}
	if manifest.SourceRef != "ghcr.io/example/test:v1" {
		t.Errorf("manifest.SourceRef = %q, want ghcr.io/example/test:v1", manifest.SourceRef)
	}
	if manifest.RootfsLogicalSize != int64(len(rootfsBody)) {
		t.Errorf("manifest.RootfsLogicalSize = %d, want %d", manifest.RootfsLogicalSize, len(rootfsBody))
	}

	// --consume=false: source files must still exist after install.
	for _, p := range []string{rootfsPath, kernelPath, initrdPath} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("source file %s missing post-install (consume was false): %v", p, err)
		}
	}
}

// TestRunImageInstallConsumesSources confirms --consume moves source
// files into the blob layout so the build pipeline doesn't leak them.
func TestRunImageInstallConsumesSources(t *testing.T) {
	tmp := t.TempDir()
	rootfsPath := filepath.Join(tmp, "rootfs.ext4")
	if err := os.WriteFile(rootfsPath, []byte("consume-me"), 0o644); err != nil {
		t.Fatalf("write rootfs: %v", err)
	}

	imageInstallRootfs = rootfsPath
	imageInstallImagesDir = filepath.Join(tmp, "imagesdir")
	imageInstallTag = "default"
	imageInstallBackend = "vz"
	imageInstallArch = "arm64"
	imageInstallConsume = true
	t.Cleanup(func() {
		imageInstallRootfs = ""
		imageInstallImagesDir = ""
		imageInstallTag = ""
		imageInstallBackend = ""
		imageInstallArch = ""
		imageInstallConsume = false
	})

	if err := runImageInstall(nil, nil); err != nil {
		t.Fatalf("runImageInstall: %v", err)
	}

	if _, err := os.Stat(rootfsPath); err == nil {
		t.Errorf("rootfs source still present with --consume; expected it to be moved into the blob")
	}
}

// TestRunImageInstallRejectsBadTag locks down the early validation
// of --tag: an invalid tag fails before any work happens (no blob is
// installed, no source file is consumed).
func TestRunImageInstallRejectsBadTag(t *testing.T) {
	tmp := t.TempDir()
	rootfsPath := filepath.Join(tmp, "rootfs.ext4")
	if err := os.WriteFile(rootfsPath, []byte("body"), 0o644); err != nil {
		t.Fatalf("write rootfs: %v", err)
	}

	imageInstallRootfs = rootfsPath
	imageInstallImagesDir = filepath.Join(tmp, "imagesdir")
	imageInstallTag = "../escape"
	imageInstallBackend = "vz"
	imageInstallArch = "arm64"
	imageInstallConsume = true
	t.Cleanup(func() {
		imageInstallRootfs = ""
		imageInstallImagesDir = ""
		imageInstallTag = ""
		imageInstallBackend = ""
		imageInstallArch = ""
		imageInstallConsume = false
	})

	err := runImageInstall(nil, nil)
	if err == nil {
		t.Fatalf("runImageInstall accepted unsafe tag; expected error")
	}

	// Validation happens BEFORE InstallBlob, so the source file
	// should still exist (no work was done).
	if _, statErr := os.Stat(rootfsPath); statErr != nil {
		t.Errorf("source file consumed before tag validation: %v", statErr)
	}
}
