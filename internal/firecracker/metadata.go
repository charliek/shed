package firecracker

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// ErrInstanceNotFound is returned when a requested instance does not exist.
var ErrInstanceNotFound = errors.New("instance not found")

// Metadata represents the persistent state of a VM instance.
type Metadata struct {
	// Name is the shed name
	Name string `json:"name"`

	// Status is the VM status (running, stopped, error)
	Status string `json:"status"`

	// CreatedAt is when the instance was created
	CreatedAt time.Time `json:"created_at"`

	// Backend is always "firecracker"
	Backend string `json:"backend"`

	// CID is the vsock context ID
	CID uint32 `json:"cid"`

	// PID is the Firecracker process ID (when running)
	PID int `json:"pid,omitempty"`

	// IPAddress is the assigned IP address
	IPAddress string `json:"ip_address"`

	// TAPDevice is the TAP device name
	TAPDevice string `json:"tap_device"`

	// CPUs is the number of vCPUs
	CPUs int `json:"cpus"`

	// MemoryMB is the memory in MB
	MemoryMB int `json:"memory_mb"`

	// RootfsPath is the path to the instance's rootfs image
	RootfsPath string `json:"rootfs_path"`

	// Repo is the optional git repository URL
	Repo string `json:"repo,omitempty"`
}

const metadataFilename = "metadata.json"

// MetadataPath returns the path to the metadata file for an instance.
func MetadataPath(instanceDir, name string) string {
	return filepath.Join(instanceDir, name, metadataFilename)
}

// InstanceDir returns the directory for an instance.
func InstanceDir(baseDir, name string) string {
	return filepath.Join(baseDir, name)
}

// LoadMetadata loads metadata from the instance directory.
func LoadMetadata(instanceDir, name string) (*Metadata, error) {
	path := MetadataPath(instanceDir, name)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: %s", ErrInstanceNotFound, name)
		}
		return nil, fmt.Errorf("failed to read metadata: %w", err)
	}

	var meta Metadata
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, fmt.Errorf("failed to parse metadata: %w", err)
	}

	return &meta, nil
}

// Save writes the metadata to the instance directory.
func (m *Metadata) Save(instanceDir string) error {
	dir := InstanceDir(instanceDir, m.Name)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create instance directory: %w", err)
	}

	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal metadata: %w", err)
	}

	path := MetadataPath(instanceDir, m.Name)
	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("failed to write metadata: %w", err)
	}

	return nil
}

// Delete removes the instance directory and all its contents.
func (m *Metadata) Delete(instanceDir string) error {
	dir := InstanceDir(instanceDir, m.Name)
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("failed to remove instance directory: %w", err)
	}
	return nil
}

// ListInstances returns the names of all instances in the instance directory.
func ListInstances(instanceDir string) ([]string, error) {
	entries, err := os.ReadDir(instanceDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to read instance directory: %w", err)
	}

	var names []string
	for _, entry := range entries {
		if entry.IsDir() {
			// Check if metadata exists
			metaPath := MetadataPath(instanceDir, entry.Name())
			if _, err := os.Stat(metaPath); err == nil {
				names = append(names, entry.Name())
			}
		}
	}

	return names, nil
}
