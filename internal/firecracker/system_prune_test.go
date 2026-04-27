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

// TestPrune_SnapshotOrphans covers all three snapshot-orphan paths in one
// table-driven harness. Setup writes a partial snapshot dir; flags toggle
// the `.creating` marker and DryRun. Assertions cover both the
// PruneReport shape and the on-disk state after Prune returns.
func TestPrune_SnapshotOrphans(t *testing.T) {
	tests := []struct {
		name              string
		writeMarker       bool
		dryRun            bool
		wantItems         bool // expect snapshot_orphan entries in report.Items
		wantSkipped       bool // expect snapshot_orphan SkippedItem
		wantDirRemoved    bool // expect the partial dir to be gone after Prune
		wantRootfsRemoved bool // expect the rootfs file to be gone after Prune
	}{
		{
			name:              "partial dir removed",
			wantItems:         true,
			wantDirRemoved:    true,
			wantRootfsRemoved: true,
		},
		{
			name:        "fresh creating marker is skipped",
			writeMarker: true,
			wantSkipped: true,
		},
		{
			name:      "dry-run does not mutate disk",
			dryRun:    true,
			wantItems: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, snapshotsDir := newPruneTestClient(t)

			dir := filepath.Join(snapshotsDir, "halfwritten")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			rootfs := filepath.Join(dir, "rootfs.ext4")
			if err := os.WriteFile(rootfs, []byte("data"), 0o644); err != nil {
				t.Fatal(err)
			}
			if tt.writeMarker {
				if err := os.WriteFile(filepath.Join(dir, ".creating"), nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}

			report, err := c.Prune(context.Background(), backend.PruneOptions{
				Orphans: true,
				DryRun:  tt.dryRun,
			})
			if err != nil {
				t.Fatalf("Prune: %v", err)
			}
			if report.DryRun != tt.dryRun {
				t.Errorf("report.DryRun = %v; want %v", report.DryRun, tt.dryRun)
			}

			hasItems := false
			for _, it := range report.Items {
				if strings.HasPrefix(it.Path, dir) && it.Kind == "snapshot_orphan" {
					hasItems = true
				}
			}
			if hasItems != tt.wantItems {
				t.Errorf("snapshot_orphan items present = %v; want %v (items=%+v)", hasItems, tt.wantItems, report.Items)
			}

			hasSkipped := false
			for _, sk := range report.Skipped {
				if sk.Path == dir && sk.Kind == "snapshot_orphan" {
					hasSkipped = true
				}
			}
			if hasSkipped != tt.wantSkipped {
				t.Errorf("snapshot_orphan skipped present = %v; want %v (skipped=%+v)", hasSkipped, tt.wantSkipped, report.Skipped)
			}

			_, dirStatErr := os.Stat(dir)
			dirGone := os.IsNotExist(dirStatErr)
			if dirGone != tt.wantDirRemoved {
				t.Errorf("dir removed = %v; want %v (stat err=%v)", dirGone, tt.wantDirRemoved, dirStatErr)
			}

			_, rootfsStatErr := os.Stat(rootfs)
			rootfsGone := os.IsNotExist(rootfsStatErr)
			if rootfsGone != tt.wantRootfsRemoved {
				t.Errorf("rootfs removed = %v; want %v (stat err=%v)", rootfsGone, tt.wantRootfsRemoved, rootfsStatErr)
			}
		})
	}
}
