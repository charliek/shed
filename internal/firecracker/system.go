//go:build linux
// +build linux

package firecracker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/diskstat"
	"github.com/charliek/shed/internal/vmimage"
)

// Default tail size for log truncation (FC currently has no console.log,
// so this exists only for future parity).
const defaultLogTailBytes = 5 * 1024 * 1024

// Skip very fresh .tmp files: a conversion writes to .tmp before rename,
// so treating those as orphans would race.
const tmpOrphanMinAge = time.Hour

// DiskUsage returns disk-usage information for everything the Firecracker
// backend manages on the local server: image cache, per-instance rootfs
// copies, kernel, and orphan sidecar files.
//
// Firecracker does not produce a per-instance console log (the SDK writes
// logs to stderr), so ShedDiskEntry.ConsoleLog is always nil for FC sheds.
func (c *Client) DiskUsage(ctx context.Context) (config.DiskUsage, error) {
	du := config.DiskUsage{
		ServerName:  c.serverCfg.Name,
		Backend:     "firecracker",
		GeneratedAt: time.Now().UTC(),
		Images:      []config.ImageDiskEntry{},
		Sheds:       []config.ShedDiskEntry{},
		Orphans:     []config.FileEntry{},
	}

	imagesDir := c.cfg.ImagesDir
	if imagesDir != "" {
		imgs, err := c.ListImages()
		if err != nil {
			return du, fmt.Errorf("listing images: %w", err)
		}
		for _, img := range imgs {
			if !img.Cached || img.Path == "" {
				continue
			}
			entry := config.ImageDiskEntry{
				Name:      img.Name,
				Path:      img.Path,
				DockerRef: img.DockerRef,
			}
			entry.Size.LogicalBytes, entry.Size.PhysicalBytes, _ = diskstat.Stat(img.Path)
			du.Images = append(du.Images, entry)
		}

		// _base is skipped by ListImages by design; stat it directly.
		basePath := filepath.Join(imagesDir, vmimage.RootfsFilename("_base"))
		if logical, physical, err := diskstat.Stat(basePath); err == nil {
			baseRef := ""
			if vmimage.IsDockerRef(c.cfg.BaseRootfs) {
				baseRef = c.cfg.BaseRootfs
			}
			du.Images = append(du.Images, config.ImageDiskEntry{
				Name:      "_base",
				Path:      basePath,
				DockerRef: baseRef,
				Size:      config.DiskSize{LogicalBytes: logical, PhysicalBytes: physical},
				IsBase:    true,
			})
		}

		orphans, err := findOrphans(imagesDir)
		if err != nil {
			return du, fmt.Errorf("scanning orphans: %w", err)
		}
		du.Orphans = orphans
	}

	// Kernel only — Firecracker has no initrd.
	if c.cfg.KernelPath != "" {
		if logical, physical, err := diskstat.Stat(c.cfg.KernelPath); err == nil {
			du.Kernel = &config.FileEntry{
				Path: c.cfg.KernelPath,
				Size: config.DiskSize{LogicalBytes: logical, PhysicalBytes: physical},
				Kind: "kernel",
			}
		}
	}

	instanceDir := c.cfg.InstanceDir
	if instanceDir != "" {
		names, err := ListInstances(instanceDir)
		if err != nil {
			return du, fmt.Errorf("listing instances: %w", err)
		}
		for _, name := range names {
			meta, err := LoadMetadata(instanceDir, name)
			if err != nil {
				continue
			}
			du.Sheds = append(du.Sheds, shedDiskEntryForFC(instanceDir, meta))
		}
	}

	for _, img := range du.Images {
		du.Totals.Images.LogicalBytes += img.Size.LogicalBytes
		du.Totals.Images.PhysicalBytes += img.Size.PhysicalBytes
	}
	if du.Kernel != nil {
		du.Totals.Images.LogicalBytes += du.Kernel.Size.LogicalBytes
		du.Totals.Images.PhysicalBytes += du.Kernel.Size.PhysicalBytes
	}
	for _, shed := range du.Sheds {
		du.Totals.Sheds.LogicalBytes += shed.Total.LogicalBytes
		du.Totals.Sheds.PhysicalBytes += shed.Total.PhysicalBytes
	}
	for _, orph := range du.Orphans {
		du.Totals.Orphans.LogicalBytes += orph.Size.LogicalBytes
		du.Totals.Orphans.PhysicalBytes += orph.Size.PhysicalBytes
	}
	du.Totals.All.LogicalBytes = du.Totals.Images.LogicalBytes + du.Totals.Sheds.LogicalBytes + du.Totals.Orphans.LogicalBytes
	du.Totals.All.PhysicalBytes = du.Totals.Images.PhysicalBytes + du.Totals.Sheds.PhysicalBytes + du.Totals.Orphans.PhysicalBytes

	du.Notes = append(du.Notes,
		"physical bytes may overcount shared extents (reflink clones, hardlinks)",
	)

	return du, nil
}

// shedDiskEntryForFC builds a ShedDiskEntry for a single Firecracker instance.
// No ConsoleLog (FC writes to stderr, not a per-instance file).
func shedDiskEntryForFC(instanceDir string, meta *Metadata) config.ShedDiskEntry {
	entry := config.ShedDiskEntry{
		Name:   meta.Name,
		Status: meta.Status,
		Image:  meta.Image,
	}

	rootfsPath := RootfsPath(instanceDir, meta.Name)
	rootfsLogical, rootfsPhysical, _ := diskstat.Stat(rootfsPath)
	entry.Rootfs = config.FileEntry{
		Path: rootfsPath,
		Size: config.DiskSize{LogicalBytes: rootfsLogical, PhysicalBytes: rootfsPhysical},
		Kind: "rootfs",
	}

	metaPath := MetadataPath(instanceDir, meta.Name)
	if logical, physical, err := diskstat.Stat(metaPath); err == nil {
		entry.OtherFiles = append(entry.OtherFiles, config.FileEntry{
			Path: metaPath,
			Size: config.DiskSize{LogicalBytes: logical, PhysicalBytes: physical},
			Kind: "metadata",
		})
	}

	entry.Total.LogicalBytes = entry.Rootfs.Size.LogicalBytes
	entry.Total.PhysicalBytes = entry.Rootfs.Size.PhysicalBytes
	for _, f := range entry.OtherFiles {
		entry.Total.LogicalBytes += f.Size.LogicalBytes
		entry.Total.PhysicalBytes += f.Size.PhysicalBytes
	}

	return entry
}

// findOrphans scans imagesDir for sidecar files (`.lock`, `.tmp*`, `.source`)
// whose corresponding `{name}-rootfs.ext4` does not exist. Phase 1 reports;
// Phase 2 adds flock + age checks before acting.
func findOrphans(imagesDir string) ([]config.FileEntry, error) {
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
		base, kind := classifySidecar(name)
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

// classifySidecar returns the parent rootfs filename and sidecar kind for a
// filename, or ("", "") if the filename isn't a known sidecar.
func classifySidecar(filename string) (baseRootfs, kind string) {
	const suffix = "-rootfs.ext4"
	idx := strings.Index(filename, suffix)
	if idx < 0 {
		return "", ""
	}
	after := filename[idx+len(suffix):]
	if after == "" {
		return "", ""
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

// Prune removes items selected by opts. See backend.PruneOptions for flag
// semantics. Mirrors the VZ implementation except:
//   - No console.log handling (FC has no per-instance console log).
//   - DeleteShed also frees TAP devices and unregisters CID/IP.
func (c *Client) Prune(ctx context.Context, opts backend.PruneOptions) (config.PruneReport, error) {
	opts = normalizePruneOptions(opts)

	report := config.PruneReport{
		DryRun:     opts.DryRun,
		ServerName: c.serverCfg.Name,
		Scope:      scopeFlags(opts),
		Until:      opts.Until.String(),
		Items:      []config.PrunedItem{},
		Skipped:    []config.SkippedItem{},
	}

	// Step 1: snapshot mtimes BEFORE ListSheds.
	mtimes := snapshotMetadataMtimes(c.cfg.InstanceDir)

	var instanceCandidates []instanceCandidate
	if opts.Instances {
		cands, skipped, err := c.collectInstanceCandidates(ctx, mtimes, opts.Until)
		if err != nil {
			return report, err
		}
		instanceCandidates = cands
		report.Skipped = append(report.Skipped, skipped...)
	}

	// Empty ImagesDir guard (Codex #7) — os.ReadDir("") reads CWD.
	var orphanCandidates []orphanCandidate
	if opts.Orphans && c.cfg.ImagesDir != "" {
		cands, skipped := collectOrphanCandidates(c.cfg.ImagesDir)
		orphanCandidates = cands
		report.Skipped = append(report.Skipped, skipped...)
	}

	var imageCandidates []vmimage.ImageInfo
	if opts.Images && c.cfg.ImagesDir != "" {
		skipSet := make(map[string]bool, len(instanceCandidates))
		for _, ic := range instanceCandidates {
			skipSet[ic.name] = true
		}
		mgr := vmimage.NewManager(c.cfg)
		cands, err := mgr.PruneImages(true, func() ([]string, error) {
			return c.inUseImageNamesExcept(skipSet)
		})
		if err != nil {
			return report, fmt.Errorf("dry-run image prune: %w", err)
		}
		imageCandidates = cands
	}

	// FC has no per-instance console.log; collect the skip reasons as
	// candidates-that-will-be-skipped so dry-run reflects the real result.
	var logSkips []config.SkippedItem
	if opts.Logs {
		names, _ := ListInstances(c.cfg.InstanceDir)
		for _, name := range names {
			logSkips = append(logSkips, config.SkippedItem{
				Kind: "console_log", Name: name,
				Reason: "firecracker does not write a per-instance console log",
			})
		}
	}

	for _, ic := range instanceCandidates {
		report.Items = append(report.Items, ic.toPrunedItem(opts.DryRun))
	}
	for _, oc := range orphanCandidates {
		report.Items = append(report.Items, oc.toPrunedItem(opts.DryRun))
	}
	for _, img := range imageCandidates {
		report.Items = append(report.Items, imageToPrunedItem(img, opts.DryRun))
	}
	report.Skipped = append(report.Skipped, logSkips...)

	if opts.DryRun {
		finalizeReport(&report)
		return report, nil
	}

	report.Items = report.Items[:0]

	for _, ic := range instanceCandidates {
		if err := c.DeleteShed(ctx, ic.name, false); err != nil {
			report.Skipped = append(report.Skipped, config.SkippedItem{
				Kind: "instance", Name: ic.name,
				Reason: fmt.Sprintf("delete failed: %v", err),
			})
			continue
		}
		report.Items = append(report.Items, ic.toPrunedItem(false))
	}

	if opts.Images && c.cfg.ImagesDir != "" {
		mgr := vmimage.NewManager(c.cfg)
		deleted, err := mgr.PruneImages(false, c.inUseImageNames)
		if err != nil {
			finalizeReport(&report)
			return report, fmt.Errorf("image prune: %w", err)
		}
		for _, img := range deleted {
			report.Items = append(report.Items, imageToPrunedItem(img, false))
		}
	}

	if opts.Orphans {
		for _, oc := range orphanCandidates {
			if ok := sweepOrphan(c.cfg.ImagesDir, oc); !ok {
				report.Skipped = append(report.Skipped, config.SkippedItem{
					Kind: oc.kind, Path: oc.path,
					Reason: "lock now held or removal failed",
				})
				continue
			}
			report.Items = append(report.Items, oc.toPrunedItem(false))
		}
	}

	finalizeReport(&report)
	return report, nil
}

func normalizePruneOptions(opts backend.PruneOptions) backend.PruneOptions {
	if !opts.Images && !opts.Instances && !opts.Logs && !opts.Orphans {
		opts.Images = true
		opts.Instances = true
		opts.Orphans = true
	}
	if opts.LogTailBytes == 0 {
		opts.LogTailBytes = defaultLogTailBytes
	}
	return opts
}

func scopeFlags(opts backend.PruneOptions) []string {
	var scope []string
	if opts.Images {
		scope = append(scope, "images")
	}
	if opts.Instances {
		scope = append(scope, "instances")
	}
	if opts.Logs {
		scope = append(scope, "logs")
	}
	if opts.Orphans {
		scope = append(scope, "orphans")
	}
	return scope
}

// snapshotMetadataMtimes must run BEFORE ListSheds (since ListSheds can
// refresh metadata mtime on stale-running→stopped detection).
func snapshotMetadataMtimes(instanceDir string) map[string]time.Time {
	mtimes := make(map[string]time.Time)
	names, err := ListInstances(instanceDir)
	if err != nil {
		return mtimes
	}
	for _, name := range names {
		fi, err := os.Stat(MetadataPath(instanceDir, name))
		if err != nil {
			continue
		}
		mtimes[name] = fi.ModTime()
	}
	return mtimes
}

type instanceCandidate struct {
	name  string
	image string
	age   time.Duration
	size  config.DiskSize
}

func (ic instanceCandidate) toPrunedItem(dry bool) config.PrunedItem {
	reason := fmt.Sprintf("stopped %s", humanDuration(ic.age))
	if !dry {
		reason = fmt.Sprintf("deleted (stopped %s)", humanDuration(ic.age))
	}
	return config.PrunedItem{
		Kind: "instance", Name: ic.name, Action: "deleted",
		Freed: ic.size, Reason: reason,
	}
}

func (c *Client) collectInstanceCandidates(ctx context.Context, mtimes map[string]time.Time, until time.Duration) ([]instanceCandidate, []config.SkippedItem, error) {
	sheds, err := c.ListSheds(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("listing sheds: %w", err)
	}
	var cands []instanceCandidate
	var skipped []config.SkippedItem
	now := time.Now()
	for _, shed := range sheds {
		if shed.Status != config.StatusStopped {
			skipped = append(skipped, config.SkippedItem{
				Kind: "instance", Name: shed.Name,
				Reason: "cannot prune " + shed.Status + " shed",
			})
			continue
		}
		mtime, ok := mtimes[shed.Name]
		if !ok {
			continue
		}
		age := now.Sub(mtime)
		if until > 0 && age < until {
			skipped = append(skipped, config.SkippedItem{
				Kind: "instance", Name: shed.Name,
				Reason: fmt.Sprintf("too recent (%s < %s)", humanDuration(age), humanDuration(until)),
			})
			continue
		}
		cands = append(cands, instanceCandidate{
			name:  shed.Name,
			image: shed.Image,
			age:   age,
			size:  instanceSize(c.cfg.InstanceDir, shed.Name),
		})
	}
	return cands, skipped, nil
}

func instanceSize(instanceDir, name string) config.DiskSize {
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

type orphanCandidate struct {
	path string
	kind string
	size config.DiskSize
}

func (oc orphanCandidate) toPrunedItem(dry bool) config.PrunedItem {
	reason := ""
	if dry {
		reason = "orphan (no matching rootfs)"
	}
	return config.PrunedItem{
		Kind: oc.kind, Path: oc.path, Action: "deleted",
		Freed: oc.size, Reason: reason,
	}
}

func collectOrphanCandidates(imagesDir string) ([]orphanCandidate, []config.SkippedItem) {
	var candidates []orphanCandidate
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
		base, kind := classifySidecar(name)
		if base == "" || presentRootfs[base] {
			continue
		}
		path := filepath.Join(imagesDir, name)
		fi, err := e.Info()
		if err != nil {
			continue
		}

		// .lock orphans: preserved, reported as skipped (Codex #4).
		if kind == "lock" {
			skipped = append(skipped, config.SkippedItem{
				Kind: kind, Path: path,
				Reason: "lock file retained (inode-reuse race safety)",
			})
			continue
		}

		if kind == "tmp" && now.Sub(fi.ModTime()) < tmpOrphanMinAge {
			skipped = append(skipped, config.SkippedItem{
				Kind: kind, Path: path,
				Reason: fmt.Sprintf("too recent (%s < %s)", humanDuration(now.Sub(fi.ModTime())), humanDuration(tmpOrphanMinAge)),
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
		candidates = append(candidates, orphanCandidate{
			path: path, kind: kind,
			size: config.DiskSize{LogicalBytes: logical, PhysicalBytes: physical},
		})
	}
	return candidates, skipped
}

func sweepOrphan(imagesDir string, oc orphanCandidate) bool {
	base := filepath.Base(oc.path)
	rootfsBase, _ := classifySidecar(base)
	if rootfsBase == "" {
		return false
	}
	lockPath := filepath.Join(imagesDir, rootfsBase+".lock")
	release, held, err := vmimage.TryAcquireFileLock(lockPath)
	if err != nil || !held {
		return false
	}
	defer release()
	// `.lock` orphans are filtered upstream; oc.kind is "tmp" or "source".
	if err := os.Remove(oc.path); err != nil {
		return errors.Is(err, os.ErrNotExist)
	}
	return true
}

func imageToPrunedItem(img vmimage.ImageInfo, dry bool) config.PrunedItem {
	size := config.DiskSize{LogicalBytes: img.SizeBytes}
	if img.Path != "" {
		if logical, physical, err := diskstat.Stat(img.Path); err == nil {
			size.LogicalBytes = logical
			size.PhysicalBytes = physical
		}
	}
	reason := ""
	if dry {
		reason = "unreferenced image"
	}
	return config.PrunedItem{
		Kind: "image", Name: img.Name, Path: img.Path, Action: "deleted",
		Freed: size, Reason: reason,
	}
}

func finalizeReport(r *config.PruneReport) {
	for _, item := range r.Items {
		r.Totals.Freed.LogicalBytes += item.Freed.LogicalBytes
		r.Totals.Freed.PhysicalBytes += item.Freed.PhysicalBytes
	}
	r.Totals.Items = len(r.Items)
	r.Notes = append(r.Notes,
		"physical bytes are attributed (stat.Blocks*512); clonefile/FICLONE clones and hardlinks may report bytes that won't actually be reclaimed",
	)
}

func humanDuration(d time.Duration) string {
	if d < 0 {
		d = -d
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int64(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int64(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int64(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int64(d.Hours()/24))
	}
}
