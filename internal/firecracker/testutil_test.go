//go:build linux
// +build linux

package firecracker

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/vmimage"
)

// testMetadata returns a valid test metadata instance.
func testMetadata(name string) *Metadata {
	return &Metadata{
		Name:       name,
		Status:     config.StatusStopped,
		CreatedAt:  time.Now(),
		Backend:    config.BackendFirecracker,
		CID:        100,
		PID:        0,
		IPAddress:  "172.30.0.2",
		TAPDevice:  "fc-tap-0",
		CPUs:       2,
		MemoryMB:   512,
		RootfsPath: "/tmp/test-rootfs.ext4",
		Repo:       "",
	}
}

// testFirecrackerConfig returns a valid test configuration.
func testFirecrackerConfig(tmpDir string) *config.FirecrackerConfig {
	return &config.FirecrackerConfig{
		InstanceDir:     tmpDir,
		KernelPath:      "/tmp/vmlinux",
		BaseRootfs:      "/tmp/rootfs.ext4",
		SocketDir:       filepath.Join(tmpDir, "sockets"),
		BridgeName:      "fc-br0",
		BridgeCIDR:      "172.30.0.1/24",
		TAPPrefix:       "fc-tap",
		VsockBaseCID:    100,
		DefaultCPUs:     2,
		DefaultMemoryMB: 512,
		ConsolePort:     1024,
		NotifyPort:      1026,
		StartTimeout:    config.Duration(60 * time.Second),
		StopTimeout:     config.Duration(30 * time.Second),
	}
}

// createTestInstance creates an instance directory with metadata for testing.
func createTestInstance(t *testing.T, dir, name string) *Metadata {
	t.Helper()

	meta := testMetadata(name)
	if err := meta.Save(dir); err != nil {
		t.Fatalf("failed to create test instance: %v", err)
	}
	return meta
}

// installTestBlob installs a fake blob into imagesDir and tags it with
// `tag`. Returns the digest.
func installTestBlob(t *testing.T, imagesDir, tag string, body []byte) string {
	t.Helper()
	stagingDir := t.TempDir()
	src := filepath.Join(stagingDir, "rootfs.ext4")
	if err := os.WriteFile(src, body, 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	digest := vmimage.DigestPrefix + hex.EncodeToString(sum[:])
	if _, _, err := vmimage.InstallBlob(imagesDir, vmimage.BlobInstallSpec{
		Files: map[string]string{vmimage.BlobRootfsFilename: src},
		Manifest: vmimage.Manifest{
			SchemaVersion:     vmimage.ManifestSchemaVersion,
			Digest:            digest,
			SourceRef:         "ghcr.io/test/" + tag + ":v1",
			RootfsLogicalSize: int64(len(body)),
		},
	}); err != nil {
		t.Fatalf("InstallBlob: %v", err)
	}
	if tag != "" {
		if err := vmimage.SetTag(imagesDir, tag, digest); err != nil {
			t.Fatalf("SetTag: %v", err)
		}
	}
	return digest
}

// mustTempDir creates a temporary directory for testing.
func mustTempDir(t *testing.T, prefix string) string {
	t.Helper()

	dir, err := os.MkdirTemp("", prefix)
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	t.Cleanup(func() {
		os.RemoveAll(dir)
	})
	return dir
}
