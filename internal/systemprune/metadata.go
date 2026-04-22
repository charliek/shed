// Package systemprune holds backend-independent helpers shared by the VZ
// and Firecracker `DiskUsage` and `Prune` implementations. It does not
// import any backend package — callers pass in the paths and metadata they
// already have.
package systemprune

import (
	"os"
	"path/filepath"
	"time"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/diskstat"
)

// metadataFilename is the per-instance metadata file both backends write.
// Kept as a shared constant so backend code and systemprune agree on the
// layout without an import cycle.
const metadataFilename = "metadata.json"

// MetadataPath returns the canonical path to metadata.json for an instance.
// Both VZ and Firecracker use this exact layout; systemprune exports it so
// shared code (and any future consumer) can stay backend-agnostic.
func MetadataPath(instanceDir, name string) string {
	return filepath.Join(instanceDir, name, metadataFilename)
}

// InstanceDir returns the per-instance directory under instanceDir.
func InstanceDir(instanceDir, name string) string {
	return filepath.Join(instanceDir, name)
}

// InstanceCandidate is a stopped shed eligible for prune.
type InstanceCandidate struct {
	Name  string
	Image string
	Age   time.Duration
	Size  config.DiskSize
}

// ToPrunedItem renders an InstanceCandidate as a PrunedItem. When dry is
// false, the reason reflects that the item was actually deleted.
func (ic InstanceCandidate) ToPrunedItem(dry bool) config.PrunedItem {
	reason := "stopped " + HumanDuration(ic.Age)
	if !dry {
		reason = "deleted (stopped " + HumanDuration(ic.Age) + ")"
	}
	return config.PrunedItem{
		Kind: "instance", Name: ic.Name, Action: "deleted",
		Freed: ic.Size, Reason: reason,
	}
}

// SnapshotMetadataMtimes reads mtime(metadata.json) for every instance
// under instanceDir. Must run BEFORE any call that might touch metadata
// (ListSheds, GetShed) since those paths save on stale-running→stopped
// detection, which would otherwise reset the age clock.
//
// names is the list of candidate instance names supplied by the backend
// (typically via its ListInstances helper). Missing metadata files are
// silently skipped.
func SnapshotMetadataMtimes(instanceDir string, names []string) map[string]time.Time {
	mtimes := make(map[string]time.Time, len(names))
	for _, name := range names {
		fi, err := os.Stat(MetadataPath(instanceDir, name))
		if err != nil {
			continue
		}
		mtimes[name] = fi.ModTime()
	}
	return mtimes
}

// InstanceSize returns the attributed bytes for an instance directory
// (rootfs + console.log + metadata + any other files). Measured before
// deletion so the report shows what was reclaimed.
func InstanceSize(instanceDir, name string) config.DiskSize {
	var size config.DiskSize
	dir := InstanceDir(instanceDir, name)
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		logical, physical, err := diskstat.Stat(path)
		if err == nil {
			size.LogicalBytes += logical
			size.PhysicalBytes += physical
		}
		return nil
	})
	return size
}
