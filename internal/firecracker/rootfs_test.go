package firecracker

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRootfsPath(t *testing.T) {
	tests := []struct {
		name        string
		instanceDir string
		shedName    string
		want        string
	}{
		{
			name:        "simple path",
			instanceDir: "/var/lib/shed/instances",
			shedName:    "test-vm",
			want:        "/var/lib/shed/instances/test-vm/rootfs.ext4",
		},
		{
			name:        "different base",
			instanceDir: "/home/user/vms",
			shedName:    "my-shed",
			want:        "/home/user/vms/my-shed/rootfs.ext4",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := RootfsPath(tt.instanceDir, tt.shedName)
			if got != tt.want {
				t.Errorf("RootfsPath() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCopyRootfs_Success(t *testing.T) {
	dir := mustTempDir(t, "rootfs-test")

	// Create a source rootfs file
	srcContent := []byte("test rootfs content for testing")
	srcPath := filepath.Join(dir, "base-rootfs.ext4")
	if err := os.WriteFile(srcPath, srcContent, 0644); err != nil {
		t.Fatalf("failed to create source rootfs: %v", err)
	}

	// Copy rootfs
	instanceDir := filepath.Join(dir, "instances")
	dstPath, err := CopyRootfs(srcPath, instanceDir, "test-vm")
	if err != nil {
		t.Fatalf("CopyRootfs() error = %v", err)
	}

	// Verify destination path
	expectedPath := filepath.Join(instanceDir, "test-vm", "rootfs.ext4")
	if dstPath != expectedPath {
		t.Errorf("CopyRootfs() returned %v, want %v", dstPath, expectedPath)
	}

	// Verify content was copied
	dstContent, err := os.ReadFile(dstPath)
	if err != nil {
		t.Fatalf("failed to read destination rootfs: %v", err)
	}

	if string(dstContent) != string(srcContent) {
		t.Error("destination content does not match source")
	}
}

func TestCopyRootfs_MissingSource(t *testing.T) {
	dir := mustTempDir(t, "rootfs-test")

	_, err := CopyRootfs("/nonexistent/rootfs.ext4", dir, "test-vm")
	if err == nil {
		t.Error("CopyRootfs() expected error for missing source")
	}
}

func TestDeleteRootfs_Exists(t *testing.T) {
	dir := mustTempDir(t, "rootfs-test")

	// Create the rootfs file
	instanceDir := filepath.Join(dir, "test-vm")
	if err := os.MkdirAll(instanceDir, 0755); err != nil {
		t.Fatalf("failed to create instance dir: %v", err)
	}

	rootfsPath := filepath.Join(instanceDir, "rootfs.ext4")
	if err := os.WriteFile(rootfsPath, []byte("content"), 0644); err != nil {
		t.Fatalf("failed to create rootfs file: %v", err)
	}

	// Delete it
	if err := DeleteRootfs(dir, "test-vm"); err != nil {
		t.Fatalf("DeleteRootfs() error = %v", err)
	}

	// Verify it's gone
	if _, err := os.Stat(rootfsPath); !os.IsNotExist(err) {
		t.Error("rootfs file should be deleted")
	}
}

func TestDeleteRootfs_Nonexistent(t *testing.T) {
	dir := mustTempDir(t, "rootfs-test")

	// Should not error for nonexistent file
	if err := DeleteRootfs(dir, "nonexistent-vm"); err != nil {
		t.Errorf("DeleteRootfs() error = %v, want nil for nonexistent", err)
	}
}

func TestRootfsExists(t *testing.T) {
	dir := mustTempDir(t, "rootfs-test")

	// Create the rootfs file
	instanceDir := filepath.Join(dir, "test-vm")
	if err := os.MkdirAll(instanceDir, 0755); err != nil {
		t.Fatalf("failed to create instance dir: %v", err)
	}

	rootfsPath := filepath.Join(instanceDir, "rootfs.ext4")
	if err := os.WriteFile(rootfsPath, []byte("content"), 0644); err != nil {
		t.Fatalf("failed to create rootfs file: %v", err)
	}

	// Test exists
	if !RootfsExists(dir, "test-vm") {
		t.Error("RootfsExists() = false, want true")
	}

	// Test not exists
	if RootfsExists(dir, "nonexistent-vm") {
		t.Error("RootfsExists() = true for nonexistent, want false")
	}
}
