//go:build darwin

package vz

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/charliek/shed/internal/config"
)

func newSystemTestClient(t *testing.T) (*Client, string, string) {
	t.Helper()
	imagesDir := t.TempDir()
	instanceDir := t.TempDir()

	cfg := &config.VZConfig{
		ImagesDir:   imagesDir,
		InstanceDir: instanceDir,
		Images:      map[string]string{},
		BaseRootfs:  "ghcr.io/example/base:v1",
	}
	serverCfg := &config.ServerConfig{Name: "test-server"}

	client := &Client{cfg: cfg, serverCfg: serverCfg}
	return client, imagesDir, instanceDir
}

func TestDiskUsage_Empty(t *testing.T) {
	client, _, _ := newSystemTestClient(t)
	du, err := client.DiskUsage(context.Background())
	if err != nil {
		t.Fatalf("DiskUsage: %v", err)
	}
	if du.Backend != "vz" {
		t.Errorf("Backend = %q, want vz", du.Backend)
	}
	if du.ServerName != "test-server" {
		t.Errorf("ServerName = %q, want test-server", du.ServerName)
	}
	if len(du.Images) != 0 || len(du.Sheds) != 0 || len(du.Orphans) != 0 {
		t.Errorf("expected empty slices, got images=%d sheds=%d orphans=%d",
			len(du.Images), len(du.Sheds), len(du.Orphans))
	}
	if du.Totals.All.LogicalBytes != 0 {
		t.Errorf("expected zero total, got %d", du.Totals.All.LogicalBytes)
	}
}

func TestDiskUsage_Populated(t *testing.T) {
	client, imagesDir, instanceDir := newSystemTestClient(t)

	// _base (discovered via tag indirection) + one variant (auto-discovered via ListImages).
	createFakeImage(t, imagesDir, "_base")
	createFakeImage(t, imagesDir, "experimental")

	// One stopped shed with a rootfs copy and a console.log.
	dir := filepath.Join(instanceDir, "api-dev")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	meta := &Metadata{
		Version: MetadataVersion,
		Name:    "api-dev",
		Status:  config.StatusStopped,
		Backend: "vz",
		Image:   "experimental",
	}
	if err := meta.Save(instanceDir); err != nil {
		t.Fatalf("save metadata: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "rootfs.ext4"), make([]byte, 4096), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "console.log"), make([]byte, 256), 0644); err != nil {
		t.Fatal(err)
	}

	// A dead orphan sidecar.
	if err := os.WriteFile(filepath.Join(imagesDir, "dead-rootfs.ext4.lock"), []byte{}, 0644); err != nil {
		t.Fatal(err)
	}

	du, err := client.DiskUsage(context.Background())
	if err != nil {
		t.Fatalf("DiskUsage: %v", err)
	}

	// Must include both _base and experimental.
	names := map[string]bool{}
	for _, img := range du.Images {
		names[img.Name] = true
	}
	if !names["_base"] || !names["experimental"] {
		t.Errorf("expected both _base and experimental; got images %+v", names)
	}

	if len(du.Sheds) != 1 || du.Sheds[0].Name != "api-dev" {
		t.Fatalf("expected 1 shed named api-dev, got %+v", du.Sheds)
	}
	shed := du.Sheds[0]
	if shed.ConsoleLog == nil {
		t.Errorf("expected ConsoleLog to be set on VZ shed")
	}
	if shed.Rootfs.Size.LogicalBytes != 4096 {
		t.Errorf("rootfs logical = %d, want 4096", shed.Rootfs.Size.LogicalBytes)
	}

	if len(du.Orphans) != 1 {
		t.Errorf("expected 1 orphan, got %d", len(du.Orphans))
	}

	if du.Totals.All.LogicalBytes <= 0 {
		t.Errorf("expected non-zero total, got %d", du.Totals.All.LogicalBytes)
	}
	// Images total should be non-zero. createFakeImage writes ~20-byte
	// fake rootfs bodies; the old 12288-byte threshold predated that
	// helper (when tests wrote 4 KiB + 8 KiB raw files). Use a lower
	// bound that just verifies the bytes flow through — the exact size
	// is asserted on per-image entries elsewhere.
	if du.Totals.Images.LogicalBytes == 0 {
		t.Errorf("images total = %d, want > 0", du.Totals.Images.LogicalBytes)
	}
	// Sheds total should include rootfs (4096) + console.log (256) at minimum.
	if du.Totals.Sheds.LogicalBytes < 4096+256 {
		t.Errorf("sheds total = %d, want >= 4352", du.Totals.Sheds.LogicalBytes)
	}
}

func TestDiskUsage_MalformedMetadataSkipped(t *testing.T) {
	client, _, instanceDir := newSystemTestClient(t)

	// Create an instance directory with an invalid metadata file.
	dir := filepath.Join(instanceDir, "broken")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "metadata.json"), []byte("{not-json"), 0644); err != nil {
		t.Fatal(err)
	}

	// DiskUsage should skip the broken instance rather than failing.
	du, err := client.DiskUsage(context.Background())
	if err != nil {
		t.Fatalf("DiskUsage should not fail on malformed metadata: %v", err)
	}
	if len(du.Sheds) != 0 {
		t.Errorf("expected 0 sheds after skipping broken, got %d", len(du.Sheds))
	}
}
