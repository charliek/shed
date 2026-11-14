//go:build darwin

package vz

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charliek/shed/internal/config"
)

func newSystemTestClient(t *testing.T) (*Client, string, string) {
	t.Helper()
	imagesDir := t.TempDir()
	instanceDir := t.TempDir()

	cfg := &config.VZConfig{
		ImagesDir:    imagesDir,
		InstanceDir:  instanceDir,
		ImageAliases: map[string]string{},
		DefaultImage: "ghcr.io/example/base:v1",
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

	// Two installed images, listed by their Docker ref identity.
	createFakeImage(t, imagesDir, "base")
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

	// Must include both images, keyed by their Docker ref.
	names := map[string]bool{}
	for _, img := range du.Images {
		names[img.Name] = true
	}
	if !names["ghcr.io/example/base:v1"] || !names["ghcr.io/example/experimental:v1"] {
		t.Errorf("expected both base and experimental refs; got images %+v", names)
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

// TestRefScanner_StrictModeFailsOnMalformedMetadata locks in the policy
// split Codex flagged: image prune (which scans refs in strict mode)
// must refuse to run when an instance has malformed metadata, because
// returning a partial protective-ref set from a destructive caller
// risks deleting a blob the broken-but-recoverable shed pinned. The
// lenient (read-only) path still warn-and-skips.
func TestRefScanner_StrictModeFailsOnMalformedMetadata(t *testing.T) {
	_, _, instanceDir := newSystemTestClient(t)

	dir := filepath.Join(instanceDir, "broken")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "metadata.json"), []byte("{not-json"), 0644); err != nil {
		t.Fatal(err)
	}

	scanner := &vzRefScanner{cfg: &config.VZConfig{InstanceDir: instanceDir}}

	if _, err := scanner.ScanRefs(false); err != nil {
		t.Fatalf("lenient ScanRefs should not fail on malformed metadata: %v", err)
	}

	_, err := scanner.ScanRefs(true)
	if err == nil {
		t.Fatal("strict ScanRefs should fail on malformed metadata")
	}
	if !strings.Contains(err.Error(), "broken") {
		t.Errorf("error should name the broken instance, got: %v", err)
	}
	if !strings.Contains(err.Error(), "remove the directory") {
		t.Errorf("error should hint at fix, got: %v", err)
	}
}

// TestDiskUsage_ReplacedBetweenLoadAndStat: DiskUsage reads a shed's or a
// snapshot's record and then stats its files (the upper, the console log, the
// metadata) without the lock. When the shed or snapshot is deleted and made
// again under its name by someone else in between, the sizes it read are the
// new one's: the entry is dropped rather than shown with the old record's
// owner. One left alone is still reported.
func TestDiskUsage_ReplacedBetweenLoadAndStat(t *testing.T) {
	client, _, instanceDir := newSystemTestClient(t)
	snapshotsDir := t.TempDir()
	client.cfg.SnapshotsDir = snapshotsDir
	t0 := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

	putShed := func(name, user string, at time.Time, size int) {
		t.Helper()
		meta := &Metadata{Version: MetadataVersion, Name: name, Status: config.StatusStopped, Backend: "vz", User: user, CreatedAt: at}
		if err := meta.Save(instanceDir); err != nil {
			t.Fatal(err)
		}
		dir := InstanceDir(instanceDir, name)
		if err := os.WriteFile(RootfsPath(instanceDir, name), make([]byte, size), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, consoleLogFilename), make([]byte, size), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	putSnap := func(name, user string, at time.Time, size int) {
		t.Helper()
		if err := saveSnapshot(snapshotsDir, &config.Snapshot{Name: name, SourceShed: "src", User: user, CreatedAt: at}); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(SnapshotRootfsPath(snapshotsDir, name), make([]byte, size), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	putShed("box", "alice", t0, 4096)
	putShed("keep", "alice", t0, 4096)
	putSnap("snap", "alice", t0, 4096)
	putSnap("kept", "alice", t0, 4096)

	replaced := map[string]bool{}
	diskUsageLoaded = func(kind, name string) {
		switch {
		case kind == "shed" && name == "box":
			if err := os.RemoveAll(InstanceDir(instanceDir, name)); err != nil {
				t.Fatal(err)
			}
			putShed(name, "bob", t0.Add(time.Minute), 1<<20)
			replaced[kind] = true
		case kind == "snapshot" && name == "snap":
			if err := os.RemoveAll(SnapshotDir(snapshotsDir, name)); err != nil {
				t.Fatal(err)
			}
			putSnap(name, "bob", t0.Add(time.Minute), 1<<20)
			replaced[kind] = true
		}
	}
	t.Cleanup(func() { diskUsageLoaded = nil })

	du, err := client.DiskUsage(context.Background())
	if err != nil {
		t.Fatalf("DiskUsage: %v", err)
	}
	if !replaced["shed"] || !replaced["snapshot"] {
		t.Fatalf("the replacement never ran: %v", replaced)
	}
	var sheds, snaps []string
	for _, e := range du.Sheds {
		sheds = append(sheds, e.Name)
		if e.Name == "box" {
			t.Errorf("shed box reported as %s's with %d bytes: the sizes are the replacement's", e.User, e.Total.LogicalBytes)
		}
	}
	for _, e := range du.Snapshots {
		snaps = append(snaps, e.Name)
		if e.Name == "snap" {
			t.Errorf("snapshot snap reported as %s's with %d bytes: the sizes are the replacement's", e.User, e.Rootfs.Size.LogicalBytes)
		}
	}
	if len(sheds) != 1 || sheds[0] != "keep" {
		t.Errorf("sheds %v, want only keep", sheds)
	}
	if len(snaps) != 1 || snaps[0] != "kept" {
		t.Errorf("snapshots %v, want only kept", snaps)
	}
}
