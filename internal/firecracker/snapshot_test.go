//go:build linux
// +build linux

package firecracker

import (
	"context"
	"errors"
	"os"
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

// TestResetShedRefusesRunning confirms ResetShed wraps the "must be
// stopped" sentinel so the API layer maps it to HTTP 409 with the
// dedicated SHED_NOT_STOPPED error code. Without that check, an
// operator could wipe a live VM's writable upper out from under it.
func TestResetShedRefusesRunning(t *testing.T) {
	instanceDir := t.TempDir()
	uppersDir := t.TempDir()

	meta := testMetadata("running-shed")
	meta.Status = config.StatusRunning
	if err := meta.Save(instanceDir); err != nil {
		t.Fatalf("save metadata: %v", err)
	}

	c := &Client{
		cfg: &config.FirecrackerConfig{
			InstanceDir:      instanceDir,
			UppersDir:        uppersDir,
			UpperSizeDefault: "5G",
		},
		serverCfg: &config.ServerConfig{Name: "test"},
	}

	_, err := c.ResetShed(context.Background(), "running-shed")
	if err == nil {
		t.Fatalf("ResetShed of running shed succeeded; want error")
	}
	if !errors.Is(err, config.ErrShedNotStoppedSentinel) {
		t.Errorf("ResetShed err = %v, want wrapped ErrShedNotStoppedSentinel", err)
	}
}

// TestResetShedRecreatesUpper confirms a stopped shed's upper is wiped
// and recreated at the requested size, and the metadata is updated.
func TestResetShedRecreatesUpper(t *testing.T) {
	instanceDir := t.TempDir()
	uppersDir := t.TempDir()

	// Set up a stopped shed with an existing upper.
	meta := testMetadata("rstshed")
	meta.Status = config.StatusStopped
	// Use the minimum allowed size (1 GiB) to keep the sparse-file
	// create + reset round-trip cheap in CI.
	meta.UpperSizeBytes = 1 * 1024 * 1024 * 1024
	upperPath, err := EnsureUpper(uppersDir, "rstshed", meta.UpperSizeBytes)
	if err != nil {
		t.Fatalf("seed EnsureUpper: %v", err)
	}
	meta.UpperPath = upperPath
	meta.RootfsPath = upperPath
	if err := meta.Save(instanceDir); err != nil {
		t.Fatalf("save metadata: %v", err)
	}

	// Stamp a marker into the existing upper so we can detect a recreate.
	if err := os.WriteFile(upperPath, []byte("STALE"), 0o644); err != nil {
		t.Fatalf("stamp: %v", err)
	}

	c := &Client{
		cfg: &config.FirecrackerConfig{
			InstanceDir:      instanceDir,
			UppersDir:        uppersDir,
			UpperSizeDefault: "5G",
		},
		serverCfg: &config.ServerConfig{Name: "test"},
	}

	shed, err := c.ResetShed(context.Background(), "rstshed")
	if err != nil {
		t.Fatalf("ResetShed: %v", err)
	}
	if shed == nil {
		t.Fatalf("ResetShed returned nil shed")
	}

	// The recreated upper exists, was truncated to the requested size,
	// and no longer contains the stale marker.
	fi, statErr := os.Stat(upperPath)
	if statErr != nil {
		t.Fatalf("upper missing after reset: %v", statErr)
	}
	if fi.Size() != meta.UpperSizeBytes {
		t.Errorf("upper size after reset = %d, want %d", fi.Size(), meta.UpperSizeBytes)
	}
	first5, _ := os.ReadFile(upperPath)
	if len(first5) >= 5 && string(first5[:5]) == "STALE" {
		t.Errorf("reset did not wipe stale upper contents")
	}

	// Metadata was rewritten.
	got, err := LoadMetadata(instanceDir, "rstshed")
	if err != nil {
		t.Fatalf("LoadMetadata: %v", err)
	}
	if got.UpperSizeBytes != meta.UpperSizeBytes {
		t.Errorf("meta.UpperSizeBytes = %d, want %d", got.UpperSizeBytes, meta.UpperSizeBytes)
	}
	if got.RootfsPath != upperPath {
		t.Errorf("meta.RootfsPath = %q, want %q", got.RootfsPath, upperPath)
	}
}
