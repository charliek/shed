package firecracker

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/charliek/shed/internal/config"
)

func TestMetadataPath(t *testing.T) {
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
			want:        "/var/lib/shed/instances/test-vm/metadata.json",
		},
		{
			name:        "empty shed name",
			instanceDir: "/var/lib/shed",
			shedName:    "",
			want:        "/var/lib/shed/metadata.json",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MetadataPath(tt.instanceDir, tt.shedName)
			if got != tt.want {
				t.Errorf("MetadataPath() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestInstanceDir(t *testing.T) {
	tests := []struct {
		name     string
		baseDir  string
		shedName string
		want     string
	}{
		{
			name:     "simple path",
			baseDir:  "/var/lib/shed/instances",
			shedName: "my-vm",
			want:     "/var/lib/shed/instances/my-vm",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := InstanceDir(tt.baseDir, tt.shedName)
			if got != tt.want {
				t.Errorf("InstanceDir() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestLoadMetadata_Valid(t *testing.T) {
	dir := mustTempDir(t, "metadata-test")

	// Create a valid metadata file
	meta := &Metadata{
		Name:       "test-vm",
		Status:     config.StatusRunning,
		CreatedAt:  time.Now().Truncate(time.Second),
		Backend:    config.BackendFirecracker,
		CID:        123,
		PID:        4567,
		IPAddress:  "172.30.0.5",
		TAPDevice:  "fc-tap-3",
		CPUs:       4,
		MemoryMB:   1024,
		RootfsPath: "/path/to/rootfs.ext4",
		Repo:       "https://github.com/example/repo",
	}

	if err := meta.Save(dir); err != nil {
		t.Fatalf("failed to save metadata: %v", err)
	}

	// Load and verify
	loaded, err := LoadMetadata(dir, "test-vm")
	if err != nil {
		t.Fatalf("LoadMetadata() error = %v", err)
	}

	if loaded.Name != meta.Name {
		t.Errorf("Name = %v, want %v", loaded.Name, meta.Name)
	}
	if loaded.Status != meta.Status {
		t.Errorf("Status = %v, want %v", loaded.Status, meta.Status)
	}
	if loaded.CID != meta.CID {
		t.Errorf("CID = %v, want %v", loaded.CID, meta.CID)
	}
	if loaded.PID != meta.PID {
		t.Errorf("PID = %v, want %v", loaded.PID, meta.PID)
	}
	if loaded.IPAddress != meta.IPAddress {
		t.Errorf("IPAddress = %v, want %v", loaded.IPAddress, meta.IPAddress)
	}
	if loaded.CPUs != meta.CPUs {
		t.Errorf("CPUs = %v, want %v", loaded.CPUs, meta.CPUs)
	}
	if loaded.MemoryMB != meta.MemoryMB {
		t.Errorf("MemoryMB = %v, want %v", loaded.MemoryMB, meta.MemoryMB)
	}
	if loaded.Repo != meta.Repo {
		t.Errorf("Repo = %v, want %v", loaded.Repo, meta.Repo)
	}
}

func TestLoadMetadata_NotFound(t *testing.T) {
	dir := mustTempDir(t, "metadata-test")

	_, err := LoadMetadata(dir, "nonexistent")
	if err == nil {
		t.Fatal("LoadMetadata() expected error for nonexistent instance")
	}

	if !errors.Is(err, ErrInstanceNotFound) {
		t.Errorf("error should wrap ErrInstanceNotFound, got: %v", err)
	}
}

func TestLoadMetadata_Corrupt(t *testing.T) {
	dir := mustTempDir(t, "metadata-test")

	// Create instance directory
	instanceDir := filepath.Join(dir, "corrupt-vm")
	if err := os.MkdirAll(instanceDir, 0755); err != nil {
		t.Fatalf("failed to create instance dir: %v", err)
	}

	// Write corrupt JSON
	metaPath := filepath.Join(instanceDir, "metadata.json")
	if err := os.WriteFile(metaPath, []byte("not valid json"), 0644); err != nil {
		t.Fatalf("failed to write corrupt metadata: %v", err)
	}

	_, err := LoadMetadata(dir, "corrupt-vm")
	if err == nil {
		t.Fatal("LoadMetadata() expected error for corrupt metadata")
	}
}

func TestMetadataSave(t *testing.T) {
	dir := mustTempDir(t, "metadata-test")

	meta := testMetadata("save-test")
	if err := meta.Save(dir); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	// Verify file exists
	metaPath := MetadataPath(dir, "save-test")
	if _, err := os.Stat(metaPath); os.IsNotExist(err) {
		t.Error("metadata file was not created")
	}

	// Verify content is valid JSON
	data, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatalf("failed to read metadata file: %v", err)
	}

	var loaded Metadata
	if err := json.Unmarshal(data, &loaded); err != nil {
		t.Errorf("saved metadata is not valid JSON: %v", err)
	}
}

func TestMetadataDelete(t *testing.T) {
	dir := mustTempDir(t, "metadata-test")

	// Create an instance
	meta := createTestInstance(t, dir, "delete-test")

	// Verify it exists
	instanceDir := InstanceDir(dir, "delete-test")
	if _, err := os.Stat(instanceDir); os.IsNotExist(err) {
		t.Fatal("instance directory should exist before delete")
	}

	// Delete it
	if err := meta.Delete(dir); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}

	// Verify it's gone
	if _, err := os.Stat(instanceDir); !os.IsNotExist(err) {
		t.Error("instance directory should be deleted")
	}
}

func TestListInstances_Empty(t *testing.T) {
	dir := mustTempDir(t, "metadata-test")

	names, err := ListInstances(dir)
	if err != nil {
		t.Fatalf("ListInstances() error = %v", err)
	}

	if len(names) != 0 {
		t.Errorf("ListInstances() = %v, want empty", names)
	}
}

func TestListInstances_Multiple(t *testing.T) {
	dir := mustTempDir(t, "metadata-test")

	// Create multiple instances
	createTestInstance(t, dir, "vm-1")
	createTestInstance(t, dir, "vm-2")
	createTestInstance(t, dir, "vm-3")

	names, err := ListInstances(dir)
	if err != nil {
		t.Fatalf("ListInstances() error = %v", err)
	}

	if len(names) != 3 {
		t.Errorf("ListInstances() returned %d instances, want 3", len(names))
	}

	// Verify all names are present
	nameSet := make(map[string]bool)
	for _, name := range names {
		nameSet[name] = true
	}

	for _, expected := range []string{"vm-1", "vm-2", "vm-3"} {
		if !nameSet[expected] {
			t.Errorf("ListInstances() missing %q", expected)
		}
	}
}

func TestListInstances_InvalidDir(t *testing.T) {
	dir := mustTempDir(t, "metadata-test")

	// Create a directory without metadata
	invalidDir := filepath.Join(dir, "not-a-vm")
	if err := os.MkdirAll(invalidDir, 0755); err != nil {
		t.Fatalf("failed to create invalid dir: %v", err)
	}

	// Create a valid instance
	createTestInstance(t, dir, "valid-vm")

	names, err := ListInstances(dir)
	if err != nil {
		t.Fatalf("ListInstances() error = %v", err)
	}

	// Should only return the valid instance
	if len(names) != 1 {
		t.Errorf("ListInstances() returned %d instances, want 1", len(names))
	}

	if names[0] != "valid-vm" {
		t.Errorf("ListInstances() = %v, want [valid-vm]", names)
	}
}

func TestListInstances_NonexistentDir(t *testing.T) {
	names, err := ListInstances("/nonexistent/directory")
	if err != nil {
		t.Fatalf("ListInstances() error = %v", err)
	}

	if names != nil {
		t.Errorf("ListInstances() = %v, want nil for nonexistent dir", names)
	}
}
