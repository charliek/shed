//go:build darwin

package vz

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/vmimage"
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

func TestClassifySidecar(t *testing.T) {
	tests := []struct {
		in       string
		wantBase string
		wantKind string
	}{
		{"default-rootfs.ext4.lock", "default-rootfs.ext4", "lock"},
		{"default-rootfs.ext4.source", "default-rootfs.ext4", "source"},
		{"default-rootfs.ext4.tmp", "default-rootfs.ext4", "tmp"},
		{"default-rootfs.ext4.tmp.12345", "default-rootfs.ext4", "tmp"},
		{"default-rootfs.ext4", "", ""},       // the rootfs itself
		{"random.txt", "", ""},                // unrelated
		{"default-rootfs.ext4.weird", "", ""}, // unknown sidecar suffix
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			base, kind := classifySidecar(tt.in)
			if base != tt.wantBase || kind != tt.wantKind {
				t.Errorf("classifySidecar(%q) = (%q, %q), want (%q, %q)",
					tt.in, base, kind, tt.wantBase, tt.wantKind)
			}
		})
	}
}

func TestFindOrphans(t *testing.T) {
	dir := t.TempDir()

	// Live: rootfs present → lock/source/tmp are NOT orphans.
	live := "live-rootfs.ext4"
	if err := os.WriteFile(filepath.Join(dir, live), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, live+".lock"), []byte{}, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, live+".source"), []byte("ref"), 0644); err != nil {
		t.Fatal(err)
	}

	// Dead: no rootfs → lock/tmp/source ARE orphans.
	dead := "dead-rootfs.ext4"
	if err := os.WriteFile(filepath.Join(dir, dead+".lock"), []byte{}, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, dead+".tmp"), []byte("junk"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, dead+".tmp.99999"), []byte("junk"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, dead+".source"), []byte("ref"), 0644); err != nil {
		t.Fatal(err)
	}

	// Noise: files that aren't sidecars must be ignored.
	if err := os.WriteFile(filepath.Join(dir, "readme.txt"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}

	got, err := findOrphans(dir)
	if err != nil {
		t.Fatalf("findOrphans: %v", err)
	}
	if len(got) != 4 {
		names := make([]string, len(got))
		for i, f := range got {
			names[i] = filepath.Base(f.Path)
		}
		t.Fatalf("got %d orphans %v, want 4 (dead .lock, .tmp, .tmp.99999, .source)", len(got), names)
	}
	// Classification spot-check.
	for _, f := range got {
		if filepath.Base(f.Path) == dead+".lock" && f.Kind != "lock" {
			t.Errorf("dead.lock kind = %q, want lock", f.Kind)
		}
	}
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

	// _base (discovered directly) + one variant (auto-discovered via ListImages).
	basePath := filepath.Join(imagesDir, vmimage.RootfsFilename("_base"))
	if err := os.WriteFile(basePath, make([]byte, 4096), 0644); err != nil {
		t.Fatal(err)
	}
	variantPath := filepath.Join(imagesDir, vmimage.RootfsFilename("experimental"))
	if err := os.WriteFile(variantPath, make([]byte, 8192), 0644); err != nil {
		t.Fatal(err)
	}

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
	// Images total should include at least _base + experimental (12288 bytes).
	if du.Totals.Images.LogicalBytes < 12288 {
		t.Errorf("images total = %d, want >= 12288", du.Totals.Images.LogicalBytes)
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
