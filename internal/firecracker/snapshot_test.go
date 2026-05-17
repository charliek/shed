//go:build linux
// +build linux

package firecracker

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/systemprune"
	"github.com/charliek/shed/internal/vmimage"
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
	// Read only the first 5 bytes; the upper is a 1 GiB sparse file
	// and os.ReadFile would spike memory in CI.
	upperFile, openErr := os.Open(upperPath)
	if openErr != nil {
		t.Fatalf("open upper: %v", openErr)
	}
	prefix := make([]byte, 5)
	n, _ := io.ReadFull(upperFile, prefix)
	_ = upperFile.Close()
	if n >= 5 && string(prefix[:5]) == "STALE" {
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

// TestPruneImagesProtectsSnapshotPin (FC) confirms a snapshot's
// LowerDigest keeps its source blob alive even when no shed pins it.
// Without this guarantee, `shed image prune` could delete the only
// blob a `shed create --from-snapshot` would spawn from. Mirrors the
// VZ test of the same name.
func TestPruneImagesProtectsSnapshotPin(t *testing.T) {
	imagesDir := t.TempDir()
	instanceDir := t.TempDir()
	snapshotsDir := t.TempDir()

	c := &Client{
		cfg: &config.FirecrackerConfig{
			ImagesDir:    imagesDir,
			InstanceDir:  instanceDir,
			SnapshotsDir: snapshotsDir,
		},
		serverCfg: &config.ServerConfig{Name: "test"},
	}

	// Install two blobs; only the first is referenced by a snapshot.
	snapshotPinned := installTestBlob(t, imagesDir, "", []byte("snap-pinned-rootfs"))
	dangling := installTestBlob(t, imagesDir, "", []byte("dangling-rootfs"))

	snap := &config.Snapshot{
		Name:        "preserved",
		Backend:     config.BackendFirecracker,
		LowerDigest: snapshotPinned,
	}
	if err := saveSnapshot(snapshotsDir, snap); err != nil {
		t.Fatalf("saveSnapshot: %v", err)
	}

	deleted, err := c.PruneImages(false)
	if err != nil {
		t.Fatalf("PruneImages: %v", err)
	}
	if len(deleted) != 1 || deleted[0].Digest != dangling {
		t.Fatalf("unexpected deletions: %#v (want only dangling=%s)", deleted, dangling)
	}
	if vmimage.BlobExists(imagesDir, dangling) {
		t.Fatalf("dangling blob still exists after prune")
	}
	if !vmimage.BlobExists(imagesDir, snapshotPinned) {
		t.Fatalf("snapshot-pinned blob removed by prune; shed create --from-snapshot would now fail")
	}
}

// TestStartShedMissingUpperFailsClearly confirms vm.Start surfaces
// a clean recovery hint when the upper file is gone (e.g. after a
// ResetShed interrupted between DeleteUpper and EnsureUpper).
// Without this guard, the operator sees an opaque firecracker SDK
// "drive open failed" error and has to dig through logs.
func TestStartShedMissingUpperFailsClearly(t *testing.T) {
	vm := &VM{
		meta: &Metadata{
			Name:       "missing-upper",
			RootfsPath: "/nonexistent/uppers/missing-upper/upper.ext4",
		},
		cfg: &config.FirecrackerConfig{
			SocketDir: t.TempDir(),
		},
	}
	err := vm.Start(context.Background())
	if err == nil {
		t.Fatalf("vm.Start with missing upper succeeded; want error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "shed reset missing-upper") {
		t.Errorf("error %q does not point operator at `shed reset`", msg)
	}
}

// TestPruneRespectsCreatingMarker confirms the prune-vs-create race
// window flagged in the second deep review is closed: a `.creating`
// marker recording the lower digest keeps an in-flight create's
// blob from being swept, even when no shed/snapshot ref exists yet.
//
// Mirrors the snapshot-pin test but for the in-flight path.
func TestPruneRespectsCreatingMarker(t *testing.T) {
	imagesDir := t.TempDir()
	instanceDir := t.TempDir()
	snapshotsDir := t.TempDir()

	c := &Client{
		cfg: &config.FirecrackerConfig{
			ImagesDir:    imagesDir,
			InstanceDir:  instanceDir,
			SnapshotsDir: snapshotsDir,
		},
		serverCfg: &config.ServerConfig{Name: "test"},
	}

	// Install two blobs. Only the first is "claimed" by an
	// in-flight create marker — no shed pins either.
	inFlight := installTestBlob(t, imagesDir, "", []byte("in-flight-rootfs"))
	dangling := installTestBlob(t, imagesDir, "", []byte("dangling-rootfs"))

	shedDir := filepath.Join(instanceDir, "creating-now")
	if err := os.MkdirAll(shedDir, 0o755); err != nil {
		t.Fatalf("mkdir shedDir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(shedDir, systemprune.InstanceCreatingMarker), []byte(inFlight), 0o600); err != nil {
		t.Fatalf("write marker: %v", err)
	}

	// Fresh marker -> dangling is the only prune candidate.
	deleted, err := c.PruneImages(false)
	if err != nil {
		t.Fatalf("PruneImages: %v", err)
	}
	if len(deleted) != 1 || deleted[0].Digest != dangling {
		t.Fatalf("unexpected deletions: %#v (want only dangling=%s)", deleted, dangling)
	}
	if !vmimage.BlobExists(imagesDir, inFlight) {
		t.Fatalf("in-flight blob was pruned despite fresh .creating marker")
	}

	// Re-install the dangling blob and expire the marker by
	// rewinding its mtime past InstanceCreatingMaxAge.
	dangling = installTestBlob(t, imagesDir, "", []byte("dangling-rootfs-v2"))
	stale := time.Now().Add(-2 * systemprune.InstanceCreatingMaxAge)
	if err := os.Chtimes(filepath.Join(shedDir, systemprune.InstanceCreatingMarker), stale, stale); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	// Stale marker -> in-flight blob is no longer protected and the
	// fresh dangling-v2 is also sweepable.
	deleted, err = c.PruneImages(false)
	if err != nil {
		t.Fatalf("PruneImages (2nd): %v", err)
	}
	gotDigests := map[string]bool{}
	for _, d := range deleted {
		gotDigests[d.Digest] = true
	}
	if !gotDigests[inFlight] {
		t.Errorf("stale marker should have stopped protecting; in-flight blob still present")
	}
	if !gotDigests[dangling] {
		t.Errorf("expected dangling-v2 blob to be swept too")
	}
}
