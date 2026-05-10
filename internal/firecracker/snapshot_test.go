//go:build linux
// +build linux

package firecracker

import (
	"context"
	"testing"

	"github.com/charliek/shed/internal/config"
)

// TestGetSnapshotPopulatesLowerCached confirms augmentSnapshot fills
// the transient LowerCached field based on whether the lower-digest
// blob is currently present. This is what `shed snapshot info` keys
// off when it warns about a missing lower image, so silent regressions
// here would mute the warning.
func TestGetSnapshotPopulatesLowerCached(t *testing.T) {
	imagesDir := t.TempDir()
	snapshotsDir := t.TempDir()

	// Install a real blob so the digest exists. installTestBlob lives
	// in testutil_test.go and produces a content-addressed entry.
	digest := installTestBlob(t, imagesDir, "default", []byte("snapshot-test-rootfs"))

	c := &Client{
		cfg: &config.FirecrackerConfig{
			ImagesDir:    imagesDir,
			SnapshotsDir: snapshotsDir,
		},
		serverCfg: &config.ServerConfig{Name: "test"},
	}

	// Snapshot pinned to the cached digest -> LowerCached=true.
	pinned := &config.Snapshot{
		Name:        "pinned",
		Backend:     config.BackendFirecracker,
		LowerDigest: digest,
	}
	if err := saveSnapshot(snapshotsDir, pinned); err != nil {
		t.Fatalf("saveSnapshot: %v", err)
	}

	got, err := c.GetSnapshot(context.Background(), "pinned")
	if err != nil {
		t.Fatalf("GetSnapshot: %v", err)
	}
	if !got.LowerCached {
		t.Errorf("LowerCached = false for snapshot pinning a cached blob; want true")
	}

	// Snapshot pinned to a fake (non-existent) digest -> LowerCached=false.
	fake := &config.Snapshot{
		Name:        "ghost",
		Backend:     config.BackendFirecracker,
		LowerDigest: "sha256:ff00000000000000000000000000000000000000000000000000000000000000",
	}
	if err := saveSnapshot(snapshotsDir, fake); err != nil {
		t.Fatalf("saveSnapshot: %v", err)
	}
	got, err = c.GetSnapshot(context.Background(), "ghost")
	if err != nil {
		t.Fatalf("GetSnapshot: %v", err)
	}
	if got.LowerCached {
		t.Errorf("LowerCached = true for snapshot pinning a missing blob; want false")
	}

	// ListSnapshots augments every entry the same way.
	list, err := c.ListSnapshots(context.Background())
	if err != nil {
		t.Fatalf("ListSnapshots: %v", err)
	}
	cached := map[string]bool{}
	for _, s := range list {
		cached[s.Name] = s.LowerCached
	}
	if !cached["pinned"] {
		t.Errorf("ListSnapshots: pinned LowerCached=false; want true")
	}
	if cached["ghost"] {
		t.Errorf("ListSnapshots: ghost LowerCached=true; want false")
	}
}
