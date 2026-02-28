//go:build darwin
// +build darwin

package vz

import (
	"os"
	"testing"
)

func TestCopyRootfs(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "vz-rootfs-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Create a fake source rootfs
	srcFile, err := os.CreateTemp(tmpDir, "base-rootfs-*.ext4")
	if err != nil {
		t.Fatalf("Failed to create source file: %v", err)
	}
	srcContent := []byte("fake rootfs content for testing")
	if _, err := srcFile.Write(srcContent); err != nil {
		t.Fatalf("Failed to write source file: %v", err)
	}
	if err := srcFile.Close(); err != nil {
		t.Fatalf("Failed to close source file: %v", err)
	}

	// Copy rootfs
	instanceDir := tmpDir + "/instances"
	dstPath, err := CopyRootfs(srcFile.Name(), instanceDir, "test-vm")
	if err != nil {
		t.Fatalf("CopyRootfs() failed: %v", err)
	}

	// Verify contents match
	dstContent, err := os.ReadFile(dstPath)
	if err != nil {
		t.Fatalf("Failed to read copied rootfs: %v", err)
	}
	if string(dstContent) != string(srcContent) {
		t.Errorf("Copied content = %q, want %q", dstContent, srcContent)
	}

	// Verify RootfsExists
	if !RootfsExists(instanceDir, "test-vm") {
		t.Error("RootfsExists() returned false after CopyRootfs")
	}

	// Delete and verify
	if err := DeleteRootfs(instanceDir, "test-vm"); err != nil {
		t.Fatalf("DeleteRootfs() failed: %v", err)
	}
	if RootfsExists(instanceDir, "test-vm") {
		t.Error("RootfsExists() returned true after DeleteRootfs")
	}
}

func TestCopyRootfsMissingSource(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "vz-rootfs-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	_, err = CopyRootfs("/nonexistent/rootfs.ext4", tmpDir, "test-vm")
	if err == nil {
		t.Error("CopyRootfs() should fail with missing source")
	}
}

func TestRootfsPath(t *testing.T) {
	path := RootfsPath("/var/lib/shed/vz/instances", "my-vm")
	want := "/var/lib/shed/vz/instances/my-vm/rootfs.ext4"
	if path != want {
		t.Errorf("RootfsPath() = %q, want %q", path, want)
	}
}
