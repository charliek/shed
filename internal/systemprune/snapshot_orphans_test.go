package systemprune

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

// snapshotFixture builds a snapshotsDir under t.TempDir() with the requested
// per-snapshot contents and returns the dir path.
func snapshotFixture(t *testing.T, snaps map[string]map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, files := range snaps {
		dir := filepath.Join(root, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
		for fname, content := range files {
			if err := os.WriteFile(filepath.Join(dir, fname), []byte(content), 0o644); err != nil {
				t.Fatalf("write %s: %v", fname, err)
			}
		}
	}
	return root
}

func TestFindSnapshotOrphans(t *testing.T) {
	tests := []struct {
		name      string
		setup     func(t *testing.T) string
		wantPaths []string // file paths (relative to snapshotsDir) expected in result
	}{
		{
			name: "complete snapshot is not flagged",
			setup: func(t *testing.T) string {
				return snapshotFixture(t, map[string]map[string]string{
					"complete": {"snapshot.json": "{}", "rootfs.ext4": "data"},
				})
			},
			wantPaths: nil,
		},
		{
			name: "rootfs without metadata is flagged",
			setup: func(t *testing.T) string {
				return snapshotFixture(t, map[string]map[string]string{
					"partial": {"rootfs.ext4": "data"},
				})
			},
			wantPaths: []string{"partial/rootfs.ext4"},
		},
		{
			name: "rootfs plus tmp metadata is flagged on both files",
			setup: func(t *testing.T) string {
				return snapshotFixture(t, map[string]map[string]string{
					"partial": {
						"rootfs.ext4":            "data",
						".snapshot-XYZ.json.tmp": "{}",
					},
				})
			},
			wantPaths: []string{"partial/.snapshot-XYZ.json.tmp", "partial/rootfs.ext4"},
		},
		{
			name: "fresh .creating marker still observed (FindSnapshotOrphans is pure)",
			setup: func(t *testing.T) string {
				root := snapshotFixture(t, map[string]map[string]string{
					"inflight": {
						"rootfs.ext4": "data",
						".creating":   "",
					},
				})
				return root
			},
			// FindSnapshotOrphans is the df-side observer; it reports
			// every partial dir (including the marker file). The
			// .creating-marker check belongs to the prune-side
			// CollectSnapshotOrphanCandidates path.
			wantPaths: []string{"inflight/.creating", "inflight/rootfs.ext4"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := tt.setup(t)
			got, err := FindSnapshotOrphans(root)
			if err != nil {
				t.Fatalf("FindSnapshotOrphans: %v", err)
			}

			gotPaths := make([]string, 0, len(got))
			for _, f := range got {
				rel, _ := filepath.Rel(root, f.Path)
				gotPaths = append(gotPaths, filepath.ToSlash(rel))
				if f.Kind != "snapshot_orphan" {
					t.Errorf("Kind = %q; want snapshot_orphan", f.Kind)
				}
			}
			sort.Strings(gotPaths)
			sort.Strings(tt.wantPaths)
			if !equalStringSlice(gotPaths, tt.wantPaths) {
				t.Errorf("paths = %v; want %v", gotPaths, tt.wantPaths)
			}
		})
	}
}

func TestCollectSnapshotOrphanCandidates_CreatingMarker(t *testing.T) {
	tests := []struct {
		name           string
		markerAge      time.Duration // how old to make the .creating marker
		wantCandidates int           // expected candidate count for the inflight dir
		wantSkipped    int           // expected skipped count
	}{
		{name: "fresh marker protects the dir", markerAge: 30 * time.Second, wantCandidates: 0, wantSkipped: 1},
		{name: "stale marker (>24h) is treated as orphan", markerAge: 25 * time.Hour, wantCandidates: 1, wantSkipped: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := snapshotFixture(t, map[string]map[string]string{
				"inflight": {
					"rootfs.ext4": "data",
					".creating":   "",
				},
			})
			markerPath := filepath.Join(root, "inflight", ".creating")
			ts := time.Now().Add(-tt.markerAge)
			if err := os.Chtimes(markerPath, ts, ts); err != nil {
				t.Fatalf("chtimes: %v", err)
			}

			cands, skipped := CollectSnapshotOrphanCandidates(root)
			if len(cands) != tt.wantCandidates {
				t.Errorf("candidates = %d; want %d (cands=%+v)", len(cands), tt.wantCandidates, cands)
			}
			if len(skipped) != tt.wantSkipped {
				t.Errorf("skipped = %d; want %d (skipped=%+v)", len(skipped), tt.wantSkipped, skipped)
			}
		})
	}
}

func TestSweepSnapshotOrphan_RemovesFilesAndDir(t *testing.T) {
	root := snapshotFixture(t, map[string]map[string]string{
		"partial": {"rootfs.ext4": "data", ".snapshot-XYZ.json.tmp": "{}"},
	})
	cands, _ := CollectSnapshotOrphanCandidates(root)
	if len(cands) != 1 {
		t.Fatalf("candidates = %d; want 1", len(cands))
	}
	if !SweepSnapshotOrphan(cands[0]) {
		t.Fatal("SweepSnapshotOrphan returned false")
	}
	if _, err := os.Stat(filepath.Join(root, "partial")); !os.IsNotExist(err) {
		t.Errorf("partial dir still present (err=%v)", err)
	}
}

func equalStringSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
