//go:build linux

package firecracker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
)

// newPruneTestClient builds a Client backed by a temp config so the
// snapshot-orphan path can be exercised without spinning up a real VM.
func newPruneTestClient(t *testing.T) (*Client, string) {
	t.Helper()
	tmp := t.TempDir()
	cfg := testFirecrackerConfig(tmp)
	cfg.ImagesDir = filepath.Join(tmp, "images")
	cfg.SnapshotsDir = filepath.Join(tmp, "snapshots")
	if err := os.MkdirAll(cfg.SnapshotsDir, 0o755); err != nil {
		t.Fatalf("mkdir snapshots: %v", err)
	}
	c := &Client{cfg: cfg, serverCfg: &config.ServerConfig{Name: "test-prune"}}
	return c, cfg.SnapshotsDir
}

func TestPrune_SnapshotOrphans_PartialDirRemoved(t *testing.T) {
	c, snapshotsDir := newPruneTestClient(t)

	dir := filepath.Join(snapshotsDir, "halfwritten")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	rootfs := filepath.Join(dir, "rootfs.ext4")
	if err := os.WriteFile(rootfs, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}

	report, err := c.Prune(context.Background(), backend.PruneOptions{Orphans: true})
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}

	hasRootfs, hasDir := false, false
	for _, it := range report.Items {
		if it.Path == rootfs && it.Kind == "snapshot_orphan" {
			hasRootfs = true
		}
		if it.Path == dir && it.Kind == "snapshot_orphan" {
			hasDir = true
		}
	}
	if !hasRootfs {
		t.Errorf("expected snapshot rootfs reported as snapshot_orphan, got %+v", report.Items)
	}
	if !hasDir {
		t.Errorf("expected snapshot dir reported as snapshot_orphan, got %+v", report.Items)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("snapshot orphan dir still exists after prune (err=%v)", err)
	}
}

func TestPrune_SnapshotOrphans_FreshCreatingMarkerSkipped(t *testing.T) {
	c, snapshotsDir := newPruneTestClient(t)

	dir := filepath.Join(snapshotsDir, "inflight")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "rootfs.ext4"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".creating"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	report, err := c.Prune(context.Background(), backend.PruneOptions{Orphans: true})
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}

	for _, it := range report.Items {
		if strings.HasPrefix(it.Path, dir) {
			t.Fatalf("inflight snapshot dir was touched: %+v", it)
		}
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("inflight dir was deleted: %v", err)
	}

	hasSkip := false
	for _, sk := range report.Skipped {
		if sk.Path == dir && sk.Kind == "snapshot_orphan" {
			hasSkip = true
		}
	}
	if !hasSkip {
		t.Errorf("expected SkippedItem for inflight snapshot, got %+v", report.Skipped)
	}
}

func TestPrune_SnapshotOrphans_DryRunNoMutation(t *testing.T) {
	c, snapshotsDir := newPruneTestClient(t)

	dir := filepath.Join(snapshotsDir, "halfwritten")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	rootfs := filepath.Join(dir, "rootfs.ext4")
	if err := os.WriteFile(rootfs, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}

	report, err := c.Prune(context.Background(), backend.PruneOptions{Orphans: true, DryRun: true})
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if !report.DryRun {
		t.Errorf("expected DryRun=true")
	}
	if _, err := os.Stat(rootfs); err != nil {
		t.Errorf("dry run deleted rootfs: %v", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("dry run deleted dir: %v", err)
	}
}
