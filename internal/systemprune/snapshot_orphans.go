package systemprune

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/diskstat"
)

// SnapshotCreatingMarker is the filename written into a snapshot directory
// while CreateSnapshot is in flight. Its presence (and a recent mtime) tells
// the orphan scanner to leave the directory alone.
const SnapshotCreatingMarker = ".creating"

// SnapshotCreatingMaxAge bounds how long a `.creating` marker is honored
// before the directory is treated as crash residue. A correct CreateSnapshot
// completes well under this threshold; anything older is taken to mean the
// process was killed mid-create and the partial dir should be reclaimable.
const SnapshotCreatingMaxAge = 24 * time.Hour

// snapshotMetadataFilename is the JSON sidecar each completed snapshot dir holds.
// Duplicated here (rather than imported) to keep internal/systemprune dep-free.
const snapshotMetadataFilename = "snapshot.json"

// SnapshotOrphanCandidate is a snapshot directory whose snapshot.json is
// missing AND whose `.creating` marker is absent or stale. The whole dir is
// orphan; Files holds each enumerated entry inside so Prune can remove them
// individually before rmdir-ing the parent.
type SnapshotOrphanCandidate struct {
	Dir   string
	Files []config.FileEntry
	// Total is the sum of Files sizes; convenient for the PrunedItem.Freed
	// emitted by ToPrunedItem.
	Total config.DiskSize
}

// ToPrunedItems renders the candidate as one PrunedItem per enumerated file
// plus a final entry for the empty-dir rmdir. Reason text differs from the
// image-orphan path because the cause is "snapshot.json absent" rather than
// "no matching rootfs."
func (c SnapshotOrphanCandidate) ToPrunedItems(dry bool) []config.PrunedItem {
	reason := ""
	if dry {
		reason = "snapshot directory missing snapshot.json"
	}
	items := make([]config.PrunedItem, 0, len(c.Files)+1)
	for _, f := range c.Files {
		items = append(items, config.PrunedItem{
			Kind:   "snapshot_orphan",
			Path:   f.Path,
			Action: "deleted",
			Freed:  f.Size,
			Reason: reason,
		})
	}
	items = append(items, config.PrunedItem{
		Kind:   "snapshot_orphan",
		Path:   c.Dir,
		Action: "deleted",
		Freed:  config.DiskSize{},
		Reason: reason,
	})
	return items
}

// FindSnapshotOrphans scans snapshotsDir for subdirectories missing their
// snapshot.json sidecar. Pure observation — used by DiskUsage to surface
// orphans in `shed system df`. CollectSnapshotOrphanCandidates adds the
// `.creating`-marker check used by Prune.
func FindSnapshotOrphans(snapshotsDir string) ([]config.FileEntry, error) {
	if snapshotsDir == "" {
		return nil, nil
	}
	entries, err := os.ReadDir(snapshotsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var orphans []config.FileEntry
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(snapshotsDir, e.Name())
		_, statErr := os.Stat(filepath.Join(dir, snapshotMetadataFilename))
		if statErr == nil {
			continue
		}
		// Only IsNotExist means "metadata is genuinely missing." A permission
		// or transient I/O error must NOT misclassify a healthy snapshot as
		// orphan — skip and let the next scan retry.
		if !os.IsNotExist(statErr) {
			continue
		}
		files, err := walkSnapshotDir(dir)
		if err != nil {
			continue
		}
		orphans = append(orphans, files...)
	}
	return orphans, nil
}

// CollectSnapshotOrphanCandidates returns snapshot dirs ready to delete, plus
// SkippedItems for partial dirs that are protected by a recent `.creating`
// marker (an in-flight CreateSnapshot). Mirrors CollectOrphanCandidates' shape.
func CollectSnapshotOrphanCandidates(snapshotsDir string) ([]SnapshotOrphanCandidate, []config.SkippedItem) {
	if snapshotsDir == "" {
		return nil, nil
	}
	entries, err := os.ReadDir(snapshotsDir)
	if err != nil {
		return nil, nil
	}

	var candidates []SnapshotOrphanCandidate
	var skipped []config.SkippedItem
	now := time.Now()

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(snapshotsDir, e.Name())
		_, statErr := os.Stat(filepath.Join(dir, snapshotMetadataFilename))
		if statErr == nil {
			continue
		}
		// Permission or transient I/O failure must NOT be treated as "missing
		// metadata" — that path leads to deletion of healthy snapshots. Surface
		// it as a SkippedItem instead.
		if !os.IsNotExist(statErr) {
			skipped = append(skipped, config.SkippedItem{
				Kind:   "snapshot_orphan",
				Path:   dir,
				Reason: fmt.Sprintf("stat snapshot.json failed: %v", statErr),
			})
			continue
		}

		markerPath := filepath.Join(dir, SnapshotCreatingMarker)
		fi, markerErr := os.Stat(markerPath)
		if markerErr == nil {
			age := now.Sub(fi.ModTime())
			if age < SnapshotCreatingMaxAge {
				skipped = append(skipped, config.SkippedItem{
					Kind:   "snapshot_orphan",
					Path:   dir,
					Reason: fmt.Sprintf("create in flight (.creating marker, age %s)", HumanDuration(age)),
				})
				continue
			}
		} else if !os.IsNotExist(markerErr) {
			// Permission/transient I/O error stat'ing the marker: fail closed.
			// Treating "stat failed" as "marker absent" would let an in-flight
			// create be enqueued for deletion.
			skipped = append(skipped, config.SkippedItem{
				Kind:   "snapshot_orphan",
				Path:   dir,
				Reason: fmt.Sprintf("stat %s failed: %v", SnapshotCreatingMarker, markerErr),
			})
			continue
		}

		files, err := walkSnapshotDir(dir)
		if err != nil {
			skipped = append(skipped, config.SkippedItem{
				Kind:   "snapshot_orphan",
				Path:   dir,
				Reason: fmt.Sprintf("walk failed: %v", err),
			})
			continue
		}
		var total config.DiskSize
		for _, f := range files {
			total.LogicalBytes += f.Size.LogicalBytes
			total.PhysicalBytes += f.Size.PhysicalBytes
		}
		candidates = append(candidates, SnapshotOrphanCandidate{Dir: dir, Files: files, Total: total})
	}
	return candidates, skipped
}

// SweepSnapshotOrphan removes every file in the candidate directory and then
// rmdirs the directory itself. Returns false if any step fails — caller is
// expected to record a SkippedItem and surface the failure.
func SweepSnapshotOrphan(c SnapshotOrphanCandidate) bool {
	for _, f := range c.Files {
		if err := os.Remove(f.Path); err != nil && !os.IsNotExist(err) {
			return false
		}
	}
	if err := os.Remove(c.Dir); err != nil && !os.IsNotExist(err) {
		// Some leftover (e.g., new file appeared mid-sweep) — bail rather than
		// recursively delete and risk eating an in-flight create.
		return false
	}
	return true
}

// walkSnapshotDir returns each regular file inside dir as a FileEntry with
// Kind="snapshot_orphan" and a diskstat.Stat-derived size. Uses
// filepath.WalkDir to enumerate any depth (snapshots are typically flat but
// the contract is "every file under dir").
func walkSnapshotDir(dir string) ([]config.FileEntry, error) {
	var out []config.FileEntry
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		logical, physical, err := diskstat.Stat(path)
		if err != nil {
			return nil // skip unreadable files; the dir delete will fail loudly
		}
		out = append(out, config.FileEntry{
			Path: path,
			Size: config.DiskSize{LogicalBytes: logical, PhysicalBytes: physical},
			Kind: "snapshot_orphan",
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
