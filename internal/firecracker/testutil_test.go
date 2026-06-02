//go:build linux
// +build linux

package firecracker

import (
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
		DefaultImage:    "/tmp/rootfs.ext4",
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

// installTestBlob installs a synthetic OCI image into imagesDir with
// the given body bytes as the layer content, tagged at `tag` (or no
// tag if empty). Returns the OCI manifest digest, which is what
// metadata records under LowerDigest.
func installTestBlob(t *testing.T, imagesDir, tag string, body []byte) string {
	t.Helper()
	srcRef := ""
	if tag != "" {
		srcRef = "ghcr.io/test/" + tag + ":v1"
	}
	digest, err := vmimage.InstallSyntheticImage(imagesDir, tag, srcRef, body, nil, nil)
	if err != nil {
		t.Fatalf("InstallSyntheticImage: %v", err)
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
