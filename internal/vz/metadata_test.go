//go:build darwin
// +build darwin

package vz

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestMetadataSaveLoad(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "vz-metadata-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	meta := &Metadata{
		Name:       "test-vm",
		Status:     "running",
		CreatedAt:  time.Now().Truncate(time.Second),
		Backend:    "vz",
		PID:        12345,
		CPUs:       4,
		MemoryMB:   8192,
		RootfsPath: "/tmp/rootfs.ext4",
		Repo:       "https://github.com/test/repo",
	}

	if err := meta.Save(tmpDir); err != nil {
		t.Fatalf("Save() failed: %v", err)
	}

	loaded, err := LoadMetadata(tmpDir, "test-vm")
	if err != nil {
		t.Fatalf("LoadMetadata() failed: %v", err)
	}

	if loaded.Name != meta.Name {
		t.Errorf("Name = %q, want %q", loaded.Name, meta.Name)
	}
	if loaded.Status != meta.Status {
		t.Errorf("Status = %q, want %q", loaded.Status, meta.Status)
	}
	if loaded.PID != meta.PID {
		t.Errorf("PID = %d, want %d", loaded.PID, meta.PID)
	}
	if loaded.CPUs != meta.CPUs {
		t.Errorf("CPUs = %d, want %d", loaded.CPUs, meta.CPUs)
	}
	if loaded.MemoryMB != meta.MemoryMB {
		t.Errorf("MemoryMB = %d, want %d", loaded.MemoryMB, meta.MemoryMB)
	}
	if loaded.RootfsPath != meta.RootfsPath {
		t.Errorf("RootfsPath = %q, want %q", loaded.RootfsPath, meta.RootfsPath)
	}
	if loaded.Repo != meta.Repo {
		t.Errorf("Repo = %q, want %q", loaded.Repo, meta.Repo)
	}
	if loaded.Version != MetadataVersion {
		t.Errorf("Version = %d, want %d", loaded.Version, MetadataVersion)
	}
}

func TestMetadataLoadNotFound(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "vz-metadata-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	_, err = LoadMetadata(tmpDir, "nonexistent")
	if err == nil {
		t.Error("LoadMetadata() should fail for nonexistent instance")
	}
}

func TestMetadataDelete(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "vz-metadata-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	meta := &Metadata{
		Name:   "test-vm",
		Status: "stopped",
		CPUs:   2,
	}

	if err := meta.Save(tmpDir); err != nil {
		t.Fatalf("Save() failed: %v", err)
	}

	if err := meta.Delete(tmpDir); err != nil {
		t.Fatalf("Delete() failed: %v", err)
	}

	dir := InstanceDir(tmpDir, "test-vm")
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Error("Instance directory should be removed after Delete()")
	}
}

func TestValidateInstanceName(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{"valid", "my-vm", false},
		{"empty", "", true},
		{"absolute path", "/etc/passwd", true},
		{"path traversal", "../etc/passwd", true},
		{"contains separator", "a/b", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateInstanceName(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateInstanceName(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			}
		})
	}
}

func TestListInstances(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "vz-metadata-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Empty directory
	names, err := ListInstances(tmpDir)
	if err != nil {
		t.Fatalf("ListInstances() failed: %v", err)
	}
	if len(names) != 0 {
		t.Errorf("ListInstances() returned %d names, want 0", len(names))
	}

	// Add some instances
	for _, name := range []string{"vm-a", "vm-b"} {
		meta := &Metadata{Name: name, Status: "stopped", CPUs: 2}
		if err := meta.Save(tmpDir); err != nil {
			t.Fatalf("Save(%s) failed: %v", name, err)
		}
	}

	// Also create a directory without metadata (should be skipped)
	os.MkdirAll(filepath.Join(tmpDir, "no-metadata"), 0755)

	names, err = ListInstances(tmpDir)
	if err != nil {
		t.Fatalf("ListInstances() failed: %v", err)
	}
	if len(names) != 2 {
		t.Errorf("ListInstances() returned %d names, want 2", len(names))
	}
}

func TestListInstancesNonexistentDir(t *testing.T) {
	names, err := ListInstances("/nonexistent/path")
	if err != nil {
		t.Fatalf("ListInstances() should not error for nonexistent dir: %v", err)
	}
	if names != nil {
		t.Errorf("ListInstances() should return nil for nonexistent dir")
	}
}
