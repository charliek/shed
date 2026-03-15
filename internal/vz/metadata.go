//go:build darwin
// +build darwin

package vz

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// MetadataVersion is the current metadata schema version.
const MetadataVersion = 1

// ErrInstanceNotFound is returned when a requested instance does not exist.
var ErrInstanceNotFound = errors.New("instance not found")

// ErrInvalidInstanceName is returned when a requested instance name is unsafe.
var ErrInvalidInstanceName = errors.New("invalid instance name")

// Metadata represents the persistent state of a VZ VM instance.
type Metadata struct {
	// Version is the metadata schema version
	Version int `json:"version"`

	// Name is the shed name
	Name string `json:"name"`

	// Status is the VM status (running, stopped, error)
	Status string `json:"status"`

	// CreatedAt is when the instance was created
	CreatedAt time.Time `json:"created_at"`

	// Backend is always "vz"
	Backend string `json:"backend"`

	// PID is the vfkit process ID (when running)
	PID int `json:"pid,omitempty"`

	// CPUs is the number of vCPUs
	CPUs int `json:"cpus"`

	// MemoryMB is the memory in MB
	MemoryMB int `json:"memory_mb"`

	// RootfsPath is the path to the instance's rootfs image
	RootfsPath string `json:"rootfs_path"`

	// Repo is the optional git repository URL
	Repo string `json:"repo,omitempty"`

	// LocalDir is the host directory mounted via VirtioFS (if set)
	LocalDir string `json:"local_dir,omitempty"`

	// Image is the image variant name used to create this instance
	Image string `json:"image,omitempty"`
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
	if err := validateInstanceName(name); err != nil {
		return nil, err
	}

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

	if meta.Version == 0 {
		meta.Version = 1
	}

	return &meta, nil
}

// Save writes the metadata to the instance directory atomically.
func (m *Metadata) Save(instanceDir string) error {
	if err := validateInstanceName(m.Name); err != nil {
		return err
	}

	dir := InstanceDir(instanceDir, m.Name)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create instance directory: %w", err)
	}

	m.Version = MetadataVersion

	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal metadata: %w", err)
	}

	path := MetadataPath(instanceDir, m.Name)
	tmpFile, err := os.CreateTemp(dir, ".metadata-*.json.tmp")
	if err != nil {
		return fmt.Errorf("failed to create temp metadata file: %w", err)
	}
	tmpPath := tmpFile.Name()

	if _, err := tmpFile.Write(data); err != nil {
		tmpFile.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("failed to write metadata: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("failed to close temp metadata file: %w", err)
	}

	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("failed to rename metadata: %w", err)
	}

	return nil
}

// Delete removes the instance directory and all its contents.
func (m *Metadata) Delete(instanceDir string) error {
	if err := validateInstanceName(m.Name); err != nil {
		return err
	}

	dir := InstanceDir(instanceDir, m.Name)
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("failed to remove instance directory: %w", err)
	}
	return nil
}

func validateInstanceName(name string) error {
	if name == "" {
		return ErrInvalidInstanceName
	}
	if filepath.IsAbs(name) {
		return ErrInvalidInstanceName
	}
	if strings.Contains(name, "..") {
		return ErrInvalidInstanceName
	}
	if strings.ContainsRune(name, filepath.Separator) {
		return ErrInvalidInstanceName
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
			metaPath := MetadataPath(instanceDir, entry.Name())
			if _, err := os.Stat(metaPath); err == nil {
				names = append(names, entry.Name())
			}
		}
	}

	return names, nil
}
