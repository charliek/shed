//go:build linux
// +build linux

package firecracker

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// MetadataVersion is the current metadata schema version. Bumped from
// 1 to 2 with the introduction of content-addressed image lower digests
// in the storage rewrite. Pre-v2 metadata is rejected on load — operators
// must `shed delete <name>` and recreate.
const MetadataVersion = 2

// ErrInstanceNotFound is returned when a requested instance does not exist.
var ErrInstanceNotFound = errors.New("instance not found")

// ErrInvalidInstanceName is returned when a requested instance name is unsafe.
var ErrInvalidInstanceName = errors.New("invalid instance name")

// ErrLegacyMetadata is returned when loading metadata written by a
// pre-v2 build. Surfaced as a clear operator-facing error in handlers.
var ErrLegacyMetadata = errors.New("metadata is from a pre-overlay version of shed; please run `shed delete <name>` and recreate")

// Metadata represents the persistent state of a VM instance.
type Metadata struct {
	// Version is the metadata schema version
	Version int `json:"version"`

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

	// LocalDir is the host directory mounted via 9P as the workspace (if set)
	LocalDir string `json:"local_dir,omitempty"`

	// Image is the image variant name (tag) used to create this instance.
	// Display-only; identity lives in LowerDigest.
	Image string `json:"image,omitempty"`

	// LowerDigest is the digest of the lower (base) image this shed was
	// cloned from, in the form "sha256:...". This pins the underlying
	// blob against prune for as long as the shed exists.
	LowerDigest string `json:"lower_digest,omitempty"`

	// LowerImageTag is the image variant name at create time (mirrors
	// Image; kept for naming symmetry with the future overlay model).
	LowerImageTag string `json:"lower_image_tag,omitempty"`

	// FromSnapshot records the snapshot this instance was spawned from (if any).
	FromSnapshot string `json:"from_snapshot,omitempty"`
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

	// Refuse pre-v2 metadata — the storage layout changed in an
	// incompatible way (introduction of content-addressed lowers and
	// LowerDigest tracking). Operators must wipe and recreate.
	if meta.Version < MetadataVersion {
		return nil, fmt.Errorf("%w (shed=%q, version=%d)", ErrLegacyMetadata, name, meta.Version)
	}

	return &meta, nil
}

// Save writes the metadata to the instance directory atomically.
// It writes to a temporary file and renames, which is atomic on Linux ext4
// when source and dest are in the same directory.
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
			// Check if metadata exists
			metaPath := MetadataPath(instanceDir, entry.Name())
			if _, err := os.Stat(metaPath); err == nil {
				names = append(names, entry.Name())
			}
		}
	}

	return names, nil
}
