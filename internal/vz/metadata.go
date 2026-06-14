//go:build darwin
// +build darwin

package vz

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/charliek/shed/internal/config"
)

// MetadataVersion is the current metadata schema version.
//
//   - v1 → legacy single-rootfs storage (pre-overlay).
//   - v2 → overlay-in-guest with a single content-addressed lower
//     identified by the sha256 of its rootfs.ext4.
//   - v3 → OCI image-layout-v1 storage; LowerDigest now references an
//     OCI manifest digest, and layers are looked up via the manifest.
//
// Pre-v3 metadata is rejected on load — operators must wipe and
// recreate. The on-disk blob store also changed shape in v3, so the
// legacy lower_digest values no longer point at any installed blob.
const MetadataVersion = 3

// ErrInstanceNotFound is returned when a requested instance does not exist.
var ErrInstanceNotFound = errors.New("instance not found")

// ErrInvalidInstanceName is returned when a requested instance name is unsafe.
var ErrInvalidInstanceName = errors.New("invalid instance name")

// ErrLegacyMetadata is returned when loading metadata written by a
// build older than the current MetadataVersion. The on-disk blob and
// metadata layouts changed in an incompatible way; the operator has to
// wipe the instance directory and (in v2→v3) the images_dir because
// `shed delete` itself goes through LoadMetadata and would hit this
// same error.
var ErrLegacyMetadata = errors.New("metadata is from an older shed release; remove the instance directory under {vz.instance_dir}/<name>/, the images_dir blob store, and recreate the shed")

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

	// RootfsPath is the path to the instance's rootfs image. With the
	// overlay-in-guest model this is the per-shed writable upper; the
	// read-only lower is the blob's rootfs.ext4, resolved through
	// LowerDigest at boot time.
	RootfsPath string `json:"rootfs_path"`

	// UpperPath is the absolute path to the per-shed writable upper
	// (an ext4-formatted sparse file mounted as /dev/vda in the guest).
	UpperPath string `json:"upper_path,omitempty"`

	// UpperSizeBytes is the logical size of the upper sparse file.
	UpperSizeBytes int64 `json:"upper_size_bytes,omitempty"`

	// Repo is the optional git repository URL
	Repo string `json:"repo,omitempty"`

	// ProjectMounts are the host directories mounted via VirtioFS under the
	// home directory (--local-dir / --add-dir).
	ProjectMounts []config.MountConfig `json:"project_mounts,omitempty"`

	// LandingDir is the directory interactive logins land in.
	LandingDir string `json:"landing_dir,omitempty"`

	// Image is the image variant name (tag) used to create this instance.
	// Display-only; identity lives in LowerDigest.
	Image string `json:"image,omitempty"`

	// LowerDigest is the OCI image manifest digest of the lower (base)
	// image this shed was cloned from, in the form "sha256:...". The
	// manifest references the OCI layers + config + shed-specific
	// kernel/initrd blobs. Resolved at boot by the image Manager.
	LowerDigest string `json:"lower_digest,omitempty"`

	// LowerImageTag is the image variant name at create time (mirrors
	// Image; the underlying digest in LowerDigest is the source of
	// truth — tags can be retagged later without invalidating sheds).
	LowerImageTag string `json:"lower_image_tag,omitempty"`

	// FromSnapshot records the snapshot this instance was spawned from (if any).
	FromSnapshot string `json:"from_snapshot,omitempty"`

	// Egress* persist this shed's egress-control assignment so restart/stop
	// reuse the same listener port + auth token (allocated at create time by
	// the ConfigureEgressProxy hook). Absent when egress is disabled.
	EgressProfiles []string `json:"egress_profiles,omitempty"`
	EgressPort     int      `json:"egress_port,omitempty"`
	EgressToken    string   `json:"egress_token,omitempty"`
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

	if meta.Version < MetadataVersion {
		return nil, fmt.Errorf("%w (shed=%q, version=%d)", ErrLegacyMetadata, name, meta.Version)
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

// PreserveConsoleLog copies the instance's console.log to destDir before
// the caller tears down the instance directory. The destination filename
// includes the shed name and a timestamp so multiple failed creates of
// the same name don't clobber each other. Returns the absolute path of
// the preserved copy, or empty string if there was nothing to preserve.
// A missing console.log is not an error — the VM may have failed before
// vfkit ever opened the file.
//
// Called from failure cleanup paths so the boot log survives the
// os.RemoveAll(InstanceDir/<name>) that follows a failed CreateShed or
// Start. Without preservation, postmortems on boot regressions reduce to
// "rerun and hope the failure repeats" since vfkit's stderr/stdout are
// only ever written into the about-to-be-deleted instance dir.
func (m *Metadata) PreserveConsoleLog(instanceDir, destDir string) (string, error) {
	if err := validateInstanceName(m.Name); err != nil {
		return "", err
	}

	src := filepath.Join(InstanceDir(instanceDir, m.Name), consoleLogFilename)
	srcFile, err := os.Open(src)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("opening console log: %w", err)
	}
	defer srcFile.Close()

	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return "", fmt.Errorf("creating logs dir: %w", err)
	}
	ts := time.Now().UTC().Format("20060102T150405Z")
	dest := filepath.Join(destDir, fmt.Sprintf("%s-%s.log", m.Name, ts))
	destFile, err := os.Create(dest)
	if err != nil {
		return "", fmt.Errorf("creating preserved log: %w", err)
	}
	defer destFile.Close()
	if _, err := io.Copy(destFile, srcFile); err != nil {
		return "", fmt.Errorf("copying console log: %w", err)
	}
	return dest, nil
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
