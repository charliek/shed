//go:build linux
// +build linux

package firecracker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/systemprune"
	"github.com/charliek/shed/internal/vmimage"
	"github.com/charliek/shed/internal/vmimage/clone"
)

const (
	snapshotMetadataFilename = "snapshot.json"
	snapshotRootfsFilename   = "rootfs.ext4"
	snapshotRootfsMode       = 0o444
)

// SnapshotDir returns the directory for a snapshot.
func SnapshotDir(snapshotsDir, name string) string {
	return filepath.Join(snapshotsDir, name)
}

// SnapshotMetadataPath returns the path to the snapshot metadata file.
func SnapshotMetadataPath(snapshotsDir, name string) string {
	return filepath.Join(snapshotsDir, name, snapshotMetadataFilename)
}

// SnapshotRootfsPath returns the path to the snapshot rootfs image.
func SnapshotRootfsPath(snapshotsDir, name string) string {
	return filepath.Join(snapshotsDir, name, snapshotRootfsFilename)
}

func loadSnapshot(snapshotsDir, name string) (*config.Snapshot, error) {
	if err := config.ValidateSnapshotName(name); err != nil {
		return nil, err
	}

	path := SnapshotMetadataPath(snapshotsDir, name)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: %s", config.ErrSnapshotNotFoundSentinel, name)
		}
		return nil, fmt.Errorf("failed to read snapshot metadata: %w", err)
	}

	var snap config.Snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return nil, fmt.Errorf("failed to parse snapshot metadata: %w", err)
	}
	if snap.Version == 0 {
		// Older builds wrote no version; treat as v1.
		snap.Version = 1
	}
	return &snap, nil
}

func saveSnapshot(snapshotsDir string, snap *config.Snapshot) error {
	snap.Version = config.SnapshotSchemaVersion
	return writeSnapshot(snapshotsDir, snap)
}

// writeSnapshot writes snap's snapshot.json as it is, its schema version
// included (saveSnapshot sets the current one first).
func writeSnapshot(snapshotsDir string, snap *config.Snapshot) error {
	return writeSnapshotFile(snapshotsDir, snap, false)
}

// writeSnapshotDurable is writeSnapshot that returns only once the new
// snapshot.json survives a crash or a power loss: the file is synced before
// the rename and the snapshot's directory after it. It is for a write that
// must never be lost once acknowledged (stamping the snapshot with a user).
// An error after the rename (the directory sync) leaves the new file in
// place, perhaps not durable; the caller treats it as not written.
func writeSnapshotDurable(snapshotsDir string, snap *config.Snapshot) error {
	return writeSnapshotFile(snapshotsDir, snap, true)
}

func writeSnapshotFile(snapshotsDir string, snap *config.Snapshot, durable bool) error {
	if err := config.ValidateSnapshotName(snap.Name); err != nil {
		return err
	}

	dir := SnapshotDir(snapshotsDir, snap.Name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("failed to create snapshot directory: %w", err)
	}

	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal snapshot metadata: %w", err)
	}

	path := SnapshotMetadataPath(snapshotsDir, snap.Name)
	tmpFile, err := os.CreateTemp(dir, ".snapshot-*.json.tmp")
	if err != nil {
		return fmt.Errorf("failed to create temp snapshot file: %w", err)
	}
	tmpPath := tmpFile.Name()

	if _, err := tmpFile.Write(data); err != nil {
		tmpFile.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("failed to write snapshot metadata: %w", err)
	}
	if durable {
		if err := metadataFileSync(tmpFile); err != nil {
			tmpFile.Close()
			os.Remove(tmpPath)
			return fmt.Errorf("failed to sync snapshot metadata: %w", err)
		}
	}
	if err := tmpFile.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("failed to close temp snapshot file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("failed to rename snapshot metadata: %w", err)
	}
	if durable {
		if err := metadataDirSync(dir); err != nil {
			return fmt.Errorf("failed to sync the snapshot directory: %w", err)
		}
	}
	return nil
}

func listSnapshotNames(snapshotsDir string) ([]string, error) {
	entries, err := os.ReadDir(snapshotsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to read snapshots directory: %w", err)
	}

	var names []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		metaPath := SnapshotMetadataPath(snapshotsDir, entry.Name())
		if _, err := os.Stat(metaPath); err == nil {
			names = append(names, entry.Name())
		}
	}
	return names, nil
}

// acquireSnapshotLock returns an unlock closure after taking the per-name
// snapshot mutex. Mirrors acquireCreateLock in the snapshot keyspace.
//
// Lock-order rule: when both locks are needed (snapshot create or
// from-snapshot spawn), acquire snapshotLock BEFORE the source shed's
// createLock to avoid AB-BA deadlock.
func (c *Client) acquireSnapshotLock(name string) func() {
	return c.snapshotLocks.Acquire(name)
}

// CreateSnapshot captures a stopped shed's rootfs as a named, immutable artifact.
func (c *Client) CreateSnapshot(ctx context.Context, req config.SnapshotCreateRequest) (*config.Snapshot, error) {
	if err := config.ValidateSnapshotName(req.Name); err != nil {
		return nil, err
	}
	if err := config.ValidateShedName(req.SourceShed); err != nil {
		return nil, fmt.Errorf("invalid source shed name: %w", err)
	}

	defer c.acquireSnapshotLock(req.Name)()
	defer c.acquireCreateLock(req.SourceShed)()

	// Only a definitive "no such snapshot" frees the name. A snapshot.json
	// that cannot be read or parsed is still someone's snapshot (whose, it
	// does not say): it is never treated as absent and overwritten.
	if _, err := loadSnapshot(c.cfg.SnapshotsDir, req.Name); err == nil {
		return nil, fmt.Errorf("%w: %s", config.ErrSnapshotAlreadyExistsSentinel, req.Name)
	} else if !errors.Is(err, config.ErrSnapshotNotFoundSentinel) {
		log.Printf("snapshot %s: refusing to create over an existing snapshot whose metadata cannot be read: %v", req.Name, err)
		return nil, fmt.Errorf("%w: %s (its metadata cannot be read, so it is left alone)", config.ErrSnapshotAlreadyExistsSentinel, req.Name)
	}

	srcMeta, err := LoadMetadata(c.cfg.InstanceDir, req.SourceShed)
	if err != nil {
		if errors.Is(err, ErrInstanceNotFound) {
			return nil, fmt.Errorf("%w: %s", config.ErrShedNotFoundSentinel, req.SourceShed)
		}
		return nil, err
	}
	// Checked under the source's lock, against the shed being snapshotted:
	// the incarnation the caller was authorized to enter.
	if err := checkIdentity(ctx, req.SourceShed, srcMeta); err != nil {
		return nil, err
	}

	// The snapshot is its source's user's: the user the source is
	// reported with, never one the request names. A source whose record
	// names both a service client and a user is reported unresolved; it is
	// refused rather than snapshotted, so no snapshot record ever carries
	// config.UserUnresolved or an ownership the source does not have.
	_, srcUser := config.ShedOwnership(srcMeta.Owner, srcMeta.User)
	if srcUser == config.UserUnresolved {
		return nil, fmt.Errorf("%w: source shed %s names both a service client and a user; its ownership must be resolved before it can be snapshotted",
			config.ErrInvalidShedRequestSentinel, req.SourceShed)
	}

	if srcMeta.Status == config.StatusRunning {
		return nil, fmt.Errorf("%w: %s", config.ErrSnapshotSourceRunningSentinel, req.SourceShed)
	}

	if len(srcMeta.ProjectMounts) != 0 {
		backend.Phase(ctx, "snapshot")
		backend.StatusWarning(ctx, fmt.Sprintf("source shed mounts host directories (%s); their contents are NOT included in the snapshot", strings.Join(config.ProjectMountSources(srcMeta.ProjectMounts), ", ")))
	}

	dir := SnapshotDir(c.cfg.SnapshotsDir, req.Name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("failed to create snapshot directory: %w", err)
	}

	// Drop a `.creating` marker so `shed system prune --orphans` won't sweep
	// this dir if the host crashes mid-create. Fail-closed: if we can't make
	// the marker durable before the long-running clone, abort rather than
	// proceed with no crash protection. fsync the parent dir so the marker
	// dentry is on stable storage before returning from the write.
	markerPath := filepath.Join(dir, systemprune.SnapshotCreatingMarker)
	if err := os.WriteFile(markerPath, nil, 0o600); err != nil {
		os.RemoveAll(dir)
		return nil, fmt.Errorf("failed to create snapshot marker: %w", err)
	}
	if err := syncDir(dir); err != nil {
		_ = os.Remove(markerPath)
		os.RemoveAll(dir)
		return nil, fmt.Errorf("failed to sync snapshot marker: %w", err)
	}
	defer os.Remove(markerPath)

	dstRootfs := SnapshotRootfsPath(c.cfg.SnapshotsDir, req.Name)
	if err := os.Remove(dstRootfs); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("failed to clean stale snapshot rootfs: %w", err)
	}

	// Snapshot the per-shed upper only. The lower image is shared via
	// LowerDigest, so capturing it here would balloon the snapshot to a
	// full image clone for no gain.
	src := srcMeta.UpperPath
	if src == "" {
		src = srcMeta.RootfsPath
	}
	backend.Phase(ctx, "snapshot")
	backend.Status(ctx, "Copying upper layer to snapshot...")
	strategy, err := clone.CloneFile(src, dstRootfs)
	if err != nil {
		os.RemoveAll(dir)
		return nil, fmt.Errorf("failed to copy upper: %w", err)
	}

	// fsync ladder mirroring CopyRootfs in rootfs.go: FICLONE/copy_file_range
	// don't guarantee metadata durability until the next FS commit, and
	// delayed-writeback errors (ENOSPC, EIO) only surface on fsync. Run
	// fsync BEFORE the chmod-to-0444 since fsync requires write fd.
	f, err := os.OpenFile(dstRootfs, os.O_RDWR, 0)
	if err != nil {
		os.RemoveAll(dir)
		return nil, fmt.Errorf("failed to reopen snapshot rootfs for sync: %w", err)
	}
	if syncErr := f.Sync(); syncErr != nil {
		f.Close()
		os.RemoveAll(dir)
		return nil, fmt.Errorf("failed to sync snapshot rootfs: %w", syncErr)
	}
	if closeErr := f.Close(); closeErr != nil {
		os.RemoveAll(dir)
		return nil, fmt.Errorf("failed to close snapshot rootfs after sync: %w", closeErr)
	}

	if err := os.Chmod(dstRootfs, snapshotRootfsMode); err != nil {
		os.RemoveAll(dir)
		return nil, fmt.Errorf("failed to set snapshot rootfs mode: %w", err)
	}

	var sizeBytes int64
	if fi, statErr := os.Stat(dstRootfs); statErr == nil {
		sizeBytes = fi.Size()
	}
	log.Printf("snapshot strategy=%s src=%s dst=%s logical_bytes=%d", strategy, src, dstRootfs, sizeBytes)

	snap := &config.Snapshot{
		Version:         config.SnapshotSchemaVersion,
		Name:            req.Name,
		Backend:         config.BackendFirecracker,
		SourceShed:      req.SourceShed,
		SourceImage:     snapshotSourceImage(srcMeta),
		SourceLocalDirs: config.ProjectMountSources(srcMeta.ProjectMounts),
		Comment:         req.Comment,
		CreatedAt:       time.Now(),
		SizeBytes:       sizeBytes,
		LowerDigest:     srcMeta.LowerDigest,
		User:            srcUser,
	}

	backend.Phase(ctx, "snapshot")
	backend.Status(ctx, "Writing snapshot metadata...")
	if err := saveSnapshot(c.cfg.SnapshotsDir, snap); err != nil {
		os.RemoveAll(dir)
		return nil, fmt.Errorf("failed to save snapshot metadata: %w", err)
	}

	// Final dir fsync so the rename of snapshot.json and the rootfs link
	// are durable before we report success — same crash-safety bar as
	// CopyRootfs's syncDir at the end of its happy path.
	if err := syncDir(dir); err != nil {
		os.RemoveAll(dir)
		return nil, fmt.Errorf("failed to sync snapshot directory: %w", err)
	}

	return snap, nil
}

// ListSnapshots returns all snapshots stored on this server.
func (c *Client) ListSnapshots(_ context.Context) ([]config.Snapshot, error) {
	names, err := listSnapshotNames(c.cfg.SnapshotsDir)
	if err != nil {
		return nil, err
	}
	out := make([]config.Snapshot, 0, len(names))
	for _, name := range names {
		snap, err := loadSnapshot(c.cfg.SnapshotsDir, name)
		if err != nil {
			log.Printf("Warning: skipping invalid snapshot %q: %v", name, err)
			continue
		}
		c.augmentSnapshot(snap)
		out = append(out, *snap)
	}
	return out, nil
}

// GetSnapshot returns a single snapshot by name.
func (c *Client) GetSnapshot(_ context.Context, name string) (*config.Snapshot, error) {
	snap, err := loadSnapshot(c.cfg.SnapshotsDir, name)
	if err != nil {
		return nil, err
	}
	c.augmentSnapshot(snap)
	return snap, nil
}

// augmentSnapshot fills in transient fields (LowerCached) that are
// recomputed on every read rather than persisted.
func (c *Client) augmentSnapshot(snap *config.Snapshot) {
	if snap.LowerDigest == "" || c.cfg.ImagesDir == "" {
		return
	}
	snap.LowerCached = vmimage.BlobExists(c.cfg.ImagesDir, snap.LowerDigest)
}

// DeleteSnapshot removes a snapshot from disk.
func (c *Client) DeleteSnapshot(ctx context.Context, name string) error {
	if err := config.ValidateSnapshotName(name); err != nil {
		return err
	}

	defer c.acquireSnapshotLock(name)()

	snap, err := loadSnapshot(c.cfg.SnapshotsDir, name)
	if err != nil {
		return err
	}
	// Checked under the snapshot lock, against the snapshot being deleted:
	// the one the caller was authorized for (backend.WithRequiredSnapshot).
	if err := backend.CheckSnapshot(ctx, name, snap); err != nil {
		return err
	}

	dir := SnapshotDir(c.cfg.SnapshotsDir, name)
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("failed to remove snapshot directory: %w", err)
	}
	return nil
}

// snapshotSourceImage picks the most actionable display value for the
// snapshot's SourceImage field. LowerImageTag is the tag the source
// shed pinned its lower digest to (always set for non-snapshot-derived
// sheds). Image is the variant requested by the operator and may be
// empty (default base) or stale after image retag. We prefer the
// pinned tag because that's the value `shed snapshot info` and the
// "lower missing" recovery message refer to.
func snapshotSourceImage(meta *Metadata) string {
	if meta.LowerImageTag != "" {
		return meta.LowerImageTag
	}
	return meta.Image
}
