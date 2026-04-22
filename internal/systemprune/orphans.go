package systemprune

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/diskstat"
	"github.com/charliek/shed/internal/vmimage"
)

// TmpOrphanMinAge skips very fresh `.tmp` files: conversions write to a
// `.tmp` before rename, so treating them as orphans would race.
const TmpOrphanMinAge = time.Hour

// ClassifySidecar returns the parent rootfs filename and sidecar kind for
// a filename, or ("", "") if the filename isn't a known sidecar.
//
// Recognized patterns (all relative to `{name}-rootfs.ext4`):
//   - {name}-rootfs.ext4.lock
//   - {name}-rootfs.ext4.source
//   - {name}-rootfs.ext4.tmp   or   {name}-rootfs.ext4.tmp.{pid}
func ClassifySidecar(filename string) (baseRootfs, kind string) {
	const suffix = "-rootfs.ext4"
	idx := strings.Index(filename, suffix)
	if idx < 0 {
		return "", ""
	}
	after := filename[idx+len(suffix):]
	if after == "" {
		return "", "" // the rootfs itself
	}
	if after == ".lock" {
		return filename[:idx+len(suffix)], "lock"
	}
	if after == ".source" {
		return filename[:idx+len(suffix)], "source"
	}
	if after == ".tmp" || strings.HasPrefix(after, ".tmp.") {
		return filename[:idx+len(suffix)], "tmp"
	}
	return "", ""
}

// FindOrphans scans imagesDir for sidecar files (`.lock`, `.tmp*`,
// `.source`) whose corresponding `{name}-rootfs.ext4` does not exist.
// Pure observation — used by DiskUsage. Prune uses CollectOrphanCandidates
// which adds flock + age checks.
func FindOrphans(imagesDir string) ([]config.FileEntry, error) {
	entries, err := os.ReadDir(imagesDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	presentRootfs := make(map[string]bool)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.HasSuffix(e.Name(), "-rootfs.ext4") {
			presentRootfs[e.Name()] = true
		}
	}

	var orphans []config.FileEntry
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		base, kind := ClassifySidecar(name)
		if base == "" {
			continue
		}
		if presentRootfs[base] {
			continue
		}
		path := filepath.Join(imagesDir, name)
		logical, physical, err := diskstat.Stat(path)
		if err != nil {
			continue
		}
		orphans = append(orphans, config.FileEntry{
			Path: path,
			Size: config.DiskSize{LogicalBytes: logical, PhysicalBytes: physical},
			Kind: kind,
		})
	}
	return orphans, nil
}

// OrphanCandidate is a sidecar that's lost its parent rootfs AND has
// passed flock + age checks.
type OrphanCandidate struct {
	Path string
	Kind string // "tmp" | "source" (lock orphans are skipped, not swept)
	Size config.DiskSize
}

// ToPrunedItem renders an OrphanCandidate as a PrunedItem.
func (oc OrphanCandidate) ToPrunedItem(dry bool) config.PrunedItem {
	reason := ""
	if dry {
		reason = "orphan (no matching rootfs)"
	}
	return config.PrunedItem{
		Kind: oc.Kind, Path: oc.Path, Action: "deleted",
		Freed: oc.Size, Reason: reason,
	}
}

// CollectOrphanCandidates scans imagesDir for sidecars whose parent
// rootfs is absent, after verifying the canonical `.lock` is NOT held by
// a live conversion and the file is not a very-fresh `.tmp`.
//
// `.lock` orphans are NEVER candidates — SweepOrphan refuses to remove
// them to avoid the inode-reuse race (see Manager.PruneImages). Instead
// they're reported as SkippedItem with reason "lock file retained" so the
// report doesn't lie about what will/did happen.
func CollectOrphanCandidates(imagesDir string) ([]OrphanCandidate, []config.SkippedItem) {
	var candidates []OrphanCandidate
	var skipped []config.SkippedItem

	entries, err := os.ReadDir(imagesDir)
	if err != nil {
		return nil, nil
	}

	presentRootfs := make(map[string]bool)
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), "-rootfs.ext4") {
			presentRootfs[e.Name()] = true
		}
	}

	now := time.Now()
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		base, kind := ClassifySidecar(name)
		if base == "" || presentRootfs[base] {
			continue
		}
		path := filepath.Join(imagesDir, name)
		fi, err := e.Info()
		if err != nil {
			continue
		}

		if kind == "lock" {
			skipped = append(skipped, config.SkippedItem{
				Kind: kind, Path: path,
				Reason: "lock file retained (inode-reuse race safety)",
			})
			continue
		}

		if kind == "tmp" && now.Sub(fi.ModTime()) < TmpOrphanMinAge {
			skipped = append(skipped, config.SkippedItem{
				Kind: kind, Path: path,
				Reason: fmt.Sprintf("too recent (%s < %s)", HumanDuration(now.Sub(fi.ModTime())), HumanDuration(TmpOrphanMinAge)),
			})
			continue
		}

		lockPath := filepath.Join(imagesDir, base+".lock")
		release, held, err := vmimage.TryAcquireFileLock(lockPath)
		if err != nil {
			skipped = append(skipped, config.SkippedItem{
				Kind: kind, Path: path,
				Reason: fmt.Sprintf("lock probe failed: %v", err),
			})
			continue
		}
		if !held {
			skipped = append(skipped, config.SkippedItem{
				Kind: kind, Path: path,
				Reason: "lock held (conversion in progress)",
			})
			continue
		}
		release()

		logical, physical, _ := diskstat.Stat(path)
		candidates = append(candidates, OrphanCandidate{
			Path: path, Kind: kind,
			Size: config.DiskSize{LogicalBytes: logical, PhysicalBytes: physical},
		})
	}
	return candidates, skipped
}

// SweepOrphan re-acquires the canonical flock and deletes the sidecar.
// Returns false on any failure — the caller should record a SkippedItem.
// `.lock` orphans are filtered out upstream in CollectOrphanCandidates so
// by the time SweepOrphan runs, oc.Kind is "tmp" or "source".
func SweepOrphan(imagesDir string, oc OrphanCandidate) bool {
	base := filepath.Base(oc.Path)
	rootfsBase, _ := ClassifySidecar(base)
	if rootfsBase == "" {
		return false
	}
	lockPath := filepath.Join(imagesDir, rootfsBase+".lock")
	release, held, err := vmimage.TryAcquireFileLock(lockPath)
	if err != nil || !held {
		return false
	}
	defer release()

	if err := os.Remove(oc.Path); err != nil {
		return errors.Is(err, os.ErrNotExist)
	}
	return true
}
