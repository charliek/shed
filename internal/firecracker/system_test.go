//go:build linux

package firecracker

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/vmimage"
)

func newSystemTestClient(t *testing.T) (*Client, string, string) {
	t.Helper()
	imagesDir := t.TempDir()
	instanceDir := t.TempDir()

	cfg := &config.FirecrackerConfig{
		ImagesDir:   imagesDir,
		InstanceDir: instanceDir,
		Images:      map[string]string{},
		BaseRootfs:  "ghcr.io/example/base:v1",
	}
	serverCfg := &config.ServerConfig{Name: "test-fc"}

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
		{"default-rootfs.ext4", "", ""},
		{"random.txt", "", ""},
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

func TestDiskUsage_Empty_FC(t *testing.T) {
	client, _, _ := newSystemTestClient(t)
	du, err := client.DiskUsage(context.Background())
	if err != nil {
		t.Fatalf("DiskUsage: %v", err)
	}
	if du.Backend != "firecracker" {
		t.Errorf("Backend = %q, want firecracker", du.Backend)
	}
	if du.Initrd != nil {
		t.Errorf("Initrd must be nil on Firecracker, got %v", du.Initrd)
	}
}

func TestPrune_FC_LogsSkippedWithReason(t *testing.T) {
	client, _, instanceDir := newSystemTestClient(t)

	// A stopped shed — exists just so `--logs` has something to iterate.
	meta := testMetadata("some-shed")
	meta.Status = config.StatusStopped
	if err := meta.Save(instanceDir); err != nil {
		t.Fatal(err)
	}

	report, err := client.Prune(context.Background(), backend.PruneOptions{Logs: true})
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	// FC should have NO truncated console logs in Items...
	for _, it := range report.Items {
		if it.Kind == "console_log" {
			t.Errorf("FC prune produced console_log Item %+v; FC has no console.log", it)
		}
	}
	// ...and should have a skipped entry explaining why.
	found := false
	for _, s := range report.Skipped {
		if s.Kind == "console_log" && s.Name == "some-shed" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected skipped console_log entry for some-shed, got %+v", report.Skipped)
	}
}

func TestDiskUsage_Populated_FC(t *testing.T) {
	client, imagesDir, instanceDir := newSystemTestClient(t)

	// _base and a variant.
	if err := os.WriteFile(filepath.Join(imagesDir, vmimage.RootfsFilename("_base")), make([]byte, 4096), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(imagesDir, vmimage.RootfsFilename("default")), make([]byte, 8192), 0644); err != nil {
		t.Fatal(err)
	}

	// One stopped shed with rootfs — but NO console.log (FC has no console log file).
	meta := testMetadata("api-dev")
	meta.Image = "default"
	meta.Status = config.StatusStopped
	if err := meta.Save(instanceDir); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(instanceDir, "api-dev")
	if err := os.WriteFile(filepath.Join(dir, "rootfs.ext4"), make([]byte, 4096), 0644); err != nil {
		t.Fatal(err)
	}

	du, err := client.DiskUsage(context.Background())
	if err != nil {
		t.Fatalf("DiskUsage: %v", err)
	}

	names := map[string]bool{}
	for _, img := range du.Images {
		names[img.Name] = true
	}
	if !names["_base"] || !names["default"] {
		t.Errorf("expected both _base and default; got %+v", names)
	}

	if len(du.Sheds) != 1 {
		t.Fatalf("expected 1 shed, got %d", len(du.Sheds))
	}
	if du.Sheds[0].ConsoleLog != nil {
		t.Errorf("FC shed must NOT have ConsoleLog, got %v", du.Sheds[0].ConsoleLog)
	}

	if du.Initrd != nil {
		t.Errorf("FC DiskUsage must not populate Initrd, got %v", du.Initrd)
	}
}
