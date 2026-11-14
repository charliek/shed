//go:build linux

package firecracker

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
)

func newSystemTestClient(t *testing.T) (*Client, string, string) {
	t.Helper()
	imagesDir := t.TempDir()
	instanceDir := t.TempDir()

	cfg := &config.FirecrackerConfig{
		ImagesDir:    imagesDir,
		InstanceDir:  instanceDir,
		ImageAliases: map[string]string{},
		DefaultImage: "ghcr.io/example/base:v1",
	}
	serverCfg := &config.ServerConfig{Name: "test-fc"}

	client := &Client{cfg: cfg, serverCfg: serverCfg}
	return client, imagesDir, instanceDir
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

	// Two installed images, listed by their Docker ref identity.
	installTestBlob(t, imagesDir, "base", make([]byte, 4096))
	installTestBlob(t, imagesDir, "default", make([]byte, 8192))

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
	if !names["ghcr.io/test/base:v1"] || !names["ghcr.io/test/default:v1"] {
		t.Errorf("expected both base and default refs; got %+v", names)
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

// TestDiskUsage_ReplacedBetweenLoadAndStat_FC: DiskUsage reads a shed's or a
// snapshot's record and then stats its files without the lock. When the shed
// or snapshot is deleted and made again under its name by someone else in
// between, the sizes it read are the new one's: the entry is dropped rather
// than shown with the old record's owner. One left alone is still reported.
func TestDiskUsage_ReplacedBetweenLoadAndStat_FC(t *testing.T) {
	client, _, instanceDir := newSystemTestClient(t)
	snapshotsDir := t.TempDir()
	client.cfg.SnapshotsDir = snapshotsDir
	t0 := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

	putShed := func(name, user string, at time.Time, size int) {
		t.Helper()
		meta := testMetadata(name)
		meta.User, meta.CreatedAt, meta.RootfsPath, meta.UpperPath = user, at, "", ""
		if err := meta.Save(instanceDir); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(RootfsPath(instanceDir, name), make([]byte, size), 0o644); err != nil {
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
			if err := os.RemoveAll(filepath.Join(instanceDir, name)); err != nil {
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
			t.Errorf("shed box reported as %s's with %d bytes: the sizes are the replacement's", e.User, e.Rootfs.Size.LogicalBytes)
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
