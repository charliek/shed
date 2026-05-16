package systemprune

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/diskstat"
)

// instanceMetadataFilename is the bookkeeping file a completed shed
// owns. Duplicated here (rather than imported from internal/firecracker
// or internal/vz) to keep this package dep-free.
const instanceMetadataFilename = "metadata.json"

// UpperOrphanCandidate is an `uppers/<name>/` directory whose
// corresponding `instances/<name>/metadata.json` is missing AND
// whose `.creating` marker (if any) is stale. The whole upper dir
// is reclaimable.
//
// Files holds each enumerated entry inside so Prune can remove
// them individually before rmdir-ing the parent. Mirrors the shape
// of SnapshotOrphanCandidate.
type UpperOrphanCandidate struct {
	Dir   string
	Files []config.FileEntry
	Total config.DiskSize
}

// ToPrunedItems renders the candidate as one PrunedItem per
// enumerated file plus a final entry for the empty-dir rmdir. The
// `upper_orphan` kind is informational and matches `snapshot_orphan`'s
// shape so the CLI table can treat them uniformly.
func (c UpperOrphanCandidate) ToPrunedItems(dry bool) []config.PrunedItem {
	reason := ""
	if dry {
		reason = "upper directory left behind by a crashed `shed create`"
	}
	items := make([]config.PrunedItem, 0, len(c.Files)+1)
	for _, f := range c.Files {
		items = append(items, config.PrunedItem{
			Kind:   "upper_orphan",
			Path:   f.Path,
			Action: "removed",
			Freed:  f.Size,
			Reason: reason,
		})
	}
	items = append(items, config.PrunedItem{
		Kind:   "upper_orphan",
		Path:   c.Dir,
		Action: "removed",
		Freed:  config.DiskSize{},
		Reason: reason,
	})
	return items
}

// FindUpperOrphans walks uppersDir and reports each orphaned
// per-shed upper as a FileEntry suitable for `shed system df`. An
// upper is orphan when:
//
//   - uppersDir/<name>/ exists and contains files
//   - instanceDir/<name>/metadata.json does NOT exist
//   - either no `.creating` marker exists at instanceDir/<name>/.creating,
//     or the marker is stale (mtime older than InstanceCreatingMaxAge)
//
// In other words: there's an upper file but no live shed bookkeeping
// and no fresh in-flight create that would protect it. Permission or
// transient I/O errors are reported via the candidate-collector
// (CollectUpperOrphanCandidates), NOT here — df is read-only and
// shouldn't surface skip reasons.
func FindUpperOrphans(uppersDir, instanceDir string) ([]config.FileEntry, error) {
	if uppersDir == "" {
		return nil, nil
	}
	entries, err := os.ReadDir(uppersDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading uppers dir: %w", err)
	}
	var orphans []config.FileEntry
	now := time.Now()
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if !isUpperOrphan(uppersDir, instanceDir, e.Name(), now) {
			continue
		}
		dir := filepath.Join(uppersDir, e.Name())
		files, err := walkUpperDir(dir)
		if err != nil {
			continue
		}
		orphans = append(orphans, files...)
	}
	return orphans, nil
}

// CollectUpperOrphanCandidates returns upper dirs ready to delete,
// plus SkippedItems for dirs that are protected by a fresh `.creating`
// marker (an in-flight CreateShed). Mirrors CollectSnapshotOrphanCandidates.
func CollectUpperOrphanCandidates(uppersDir, instanceDir string) ([]UpperOrphanCandidate, []config.SkippedItem) {
	if uppersDir == "" {
		return nil, nil
	}
	entries, err := os.ReadDir(uppersDir)
	if err != nil {
		return nil, nil
	}

	var candidates []UpperOrphanCandidate
	var skipped []config.SkippedItem
	now := time.Now()

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(uppersDir, e.Name())

		// Skip when the metadata.json is present (live shed). We
		// fail-closed on stat errors other than NotExist — a
		// permission flake should NOT cause us to enqueue someone's
		// live shed for deletion.
		metaPath := filepath.Join(instanceDir, e.Name(), instanceMetadataFilename)
		_, statErr := os.Stat(metaPath)
		if statErr == nil {
			continue // live shed
		}
		if !os.IsNotExist(statErr) {
			skipped = append(skipped, config.SkippedItem{
				Kind:   "upper_orphan",
				Path:   dir,
				Reason: fmt.Sprintf("stat instance metadata.json failed: %v", statErr),
			})
			continue
		}

		// Skip when there's a fresh `.creating` marker for this name
		// — an in-flight CreateShed will save metadata shortly.
		markerPath := filepath.Join(instanceDir, e.Name(), InstanceCreatingMarker)
		fi, markerErr := os.Stat(markerPath)
		if markerErr == nil {
			age := now.Sub(fi.ModTime())
			if age < InstanceCreatingMaxAge {
				skipped = append(skipped, config.SkippedItem{
					Kind:   "upper_orphan",
					Path:   dir,
					Reason: fmt.Sprintf("create in flight (.creating marker, age %s)", HumanDuration(age)),
				})
				continue
			}
		} else if !os.IsNotExist(markerErr) {
			// Permission/transient I/O on the marker: fail closed.
			skipped = append(skipped, config.SkippedItem{
				Kind:   "upper_orphan",
				Path:   dir,
				Reason: fmt.Sprintf("stat %s failed: %v", InstanceCreatingMarker, markerErr),
			})
			continue
		}

		files, err := walkUpperDir(dir)
		if err != nil {
			skipped = append(skipped, config.SkippedItem{
				Kind:   "upper_orphan",
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
		candidates = append(candidates, UpperOrphanCandidate{Dir: dir, Files: files, Total: total})
	}
	return candidates, skipped
}

// SweepUpperOrphan removes every file in the candidate directory
// and then rmdirs the directory itself. Returns false if any step
// fails — caller records a SkippedItem and surfaces the failure.
func SweepUpperOrphan(c UpperOrphanCandidate) bool {
	for _, f := range c.Files {
		if err := os.Remove(f.Path); err != nil && !os.IsNotExist(err) {
			return false
		}
	}
	if err := os.Remove(c.Dir); err != nil && !os.IsNotExist(err) {
		return false
	}
	return true
}

// isUpperOrphan reports whether the upper dir for `name` lacks a
// live metadata.json AND lacks a fresh in-flight marker. Used by
// FindUpperOrphans for the df view (no skip reasons needed there).
func isUpperOrphan(uppersDir, instanceDir, name string, now time.Time) bool {
	if _, err := os.Stat(filepath.Join(instanceDir, name, instanceMetadataFilename)); err == nil {
		return false
	}
	markerPath := filepath.Join(instanceDir, name, InstanceCreatingMarker)
	if fi, err := os.Stat(markerPath); err == nil {
		if now.Sub(fi.ModTime()) < InstanceCreatingMaxAge {
			return false
		}
	}
	return true
}

// walkUpperDir returns each regular file inside dir as a FileEntry
// with Kind="upper_orphan" and diskstat sizes. Uses filepath.WalkDir
// to enumerate any depth (uppers are flat in practice but the
// contract is "every file under dir").
func walkUpperDir(dir string) ([]config.FileEntry, error) {
	var out []config.FileEntry
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		logical, physical, _ := diskstat.Stat(path)
		out = append(out, config.FileEntry{
			Path: path,
			Size: config.DiskSize{LogicalBytes: logical, PhysicalBytes: physical},
			Kind: "upper_orphan",
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
