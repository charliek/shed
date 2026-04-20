//go:build darwin
// +build darwin

package vz

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

// Default tail size for console.log truncation when opts.LogTailBytes is 0.
const defaultLogTailBytes = 5 * 1024 * 1024

// Default skip threshold for very fresh .tmp files: conversions write to a
// .tmp before rename, so treating them as orphans would race.
const tmpOrphanMinAge = time.Hour

// consoleLogFilename is written by vm.go when launching vfkit. Kept here so
// DiskUsage/Prune don't depend on the non-exported VM internals.
const consoleLogFilename = "console.log"

// DiskUsage returns disk-usage information for everything the VZ backend
// manages on the local server: image cache, per-instance rootfs copies and
// console logs, kernel/initrd, and orphan sidecar files.
func (c *Client) DiskUsage(ctx context.Context) (config.DiskUsage, error) {
	du := config.DiskUsage{
		ServerName:  c.serverCfg.Name,
		Backend:     "vz",
		GeneratedAt: time.Now().UTC(),
		Images:      []config.ImageDiskEntry{},
		Sheds:       []config.ShedDiskEntry{},
		Orphans:     []config.FileEntry{},
	}

	imagesDir := c.cfg.ImagesDir
	if imagesDir != "" {
		// Discovered variants and config-referenced images. `ListImages`
		// intentionally skips `_base`, so we stat it directly below.
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

		// _base is produced by the runtime when base_rootfs is a Docker ref,
		// and is intentionally omitted from ListImages(). Stat it here.
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

		// Orphan sweep: sidecars (`.lock`, `.tmp*`, `.source`) whose parent
		// `-rootfs.ext4` is missing. Phase 2 adds the flock + age-threshold
		// checks before acting on these; Phase 1 only reports.
		orphans, err := findOrphans(imagesDir)
		if err != nil {
			return du, fmt.Errorf("scanning orphans: %w", err)
		}
		du.Orphans = orphans
	}

	// Kernel + initrd
	if c.cfg.KernelPath != "" {
		if logical, physical, err := diskstat.Stat(c.cfg.KernelPath); err == nil {
			du.Kernel = &config.FileEntry{
				Path: c.cfg.KernelPath,
				Size: config.DiskSize{LogicalBytes: logical, PhysicalBytes: physical},
				Kind: "kernel",
			}
		}
	}
	if c.cfg.InitrdPath != "" {
		if logical, physical, err := diskstat.Stat(c.cfg.InitrdPath); err == nil {
			du.Initrd = &config.FileEntry{
				Path: c.cfg.InitrdPath,
				Size: config.DiskSize{LogicalBytes: logical, PhysicalBytes: physical},
				Kind: "initrd",
			}
		}
	}

	// Per-instance disk usage. Use ListInstances (no staleness re-check) for
	// reporting — we only want what's on disk now, regardless of agent state.
	instanceDir := c.cfg.InstanceDir
	if instanceDir != "" {
		names, err := ListInstances(instanceDir)
		if err != nil {
			return du, fmt.Errorf("listing instances: %w", err)
		}
		for _, name := range names {
			meta, err := LoadMetadata(instanceDir, name)
			if err != nil {
				// Skip malformed metadata; don't fail the whole df.
				continue
			}
			du.Sheds = append(du.Sheds, shedDiskEntryForVZ(instanceDir, meta))
		}
	}

	// Totals
	for _, img := range du.Images {
		du.Totals.Images.LogicalBytes += img.Size.LogicalBytes
		du.Totals.Images.PhysicalBytes += img.Size.PhysicalBytes
	}
	if du.Kernel != nil {
		du.Totals.Images.LogicalBytes += du.Kernel.Size.LogicalBytes
		du.Totals.Images.PhysicalBytes += du.Kernel.Size.PhysicalBytes
	}
	if du.Initrd != nil {
		du.Totals.Images.LogicalBytes += du.Initrd.Size.LogicalBytes
		du.Totals.Images.PhysicalBytes += du.Initrd.Size.PhysicalBytes
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
		"physical bytes may overcount shared extents on APFS (clonefile) or hardlinks",
	)

	return du, nil
}

// shedDiskEntryForVZ builds a ShedDiskEntry for a single VZ instance.
func shedDiskEntryForVZ(instanceDir string, meta *Metadata) config.ShedDiskEntry {
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

	consolePath := filepath.Join(InstanceDir(instanceDir, meta.Name), consoleLogFilename)
	if logical, physical, err := diskstat.Stat(consolePath); err == nil {
		entry.ConsoleLog = &config.FileEntry{
			Path: consolePath,
			Size: config.DiskSize{LogicalBytes: logical, PhysicalBytes: physical},
			Kind: "console_log",
		}
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
	if entry.ConsoleLog != nil {
		entry.Total.LogicalBytes += entry.ConsoleLog.Size.LogicalBytes
		entry.Total.PhysicalBytes += entry.ConsoleLog.Size.PhysicalBytes
	}
	for _, f := range entry.OtherFiles {
		entry.Total.LogicalBytes += f.Size.LogicalBytes
		entry.Total.PhysicalBytes += f.Size.PhysicalBytes
	}

	return entry
}

// findOrphans scans imagesDir for sidecar files (`.lock`, `.tmp*`, `.source`)
// whose corresponding `{name}-rootfs.ext4` does not exist. Phase 1 is
// observational — Phase 2 adds flock / age checks before acting.
func findOrphans(imagesDir string) ([]config.FileEntry, error) {
	entries, err := os.ReadDir(imagesDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	// Build the set of base rootfs names present on disk.
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

// classifySidecar returns the parent rootfs filename and the sidecar kind
// for a filename. Returns ("", "") for anything that isn't a known sidecar.
//
// Recognized patterns (all relative to `{name}-rootfs.ext4`):
//   - {name}-rootfs.ext4.lock
//   - {name}-rootfs.ext4.source
//   - {name}-rootfs.ext4.tmp   or   {name}-rootfs.ext4.tmp.{pid}
func classifySidecar(filename string) (baseRootfs, kind string) {
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

// Prune removes items selected by opts. See backend.PruneOptions for flag
// semantics and backend.Backend.Prune for contract.
//
// Internal ordering (plan-mandated):
//  1. Snapshot mtimes of metadata.json files BEFORE calling ListSheds (since
//     ListSheds's staleness re-check can refresh mtime and break age filters).
//  2. Collect candidate instances, orphans, and images; image dry-run uses
//     inUseImageNamesExcept(<stopped candidates>) to simulate post-delete.
//  3. DryRun → return report with no mutation.
//  4. Execute: delete instances first (so inUseImageNames excludes them
//     naturally), then prune images, then sweep orphans (re-acquiring
//     flock per file), then optionally truncate console.log on survivors.
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

	// Step 1: snapshot mtimes BEFORE ListSheds. Plan-mandated: ListSheds
	// can refresh mtime via its stale-running→stopped re-check, which would
	// otherwise reset the age clock and confuse the age filter.
	mtimes := snapshotMetadataMtimes(c.cfg.InstanceDir)

	// Step 2a: instance candidates.
	var instanceCandidates []instanceCandidate
	if opts.Instances {
		cands, skipped, err := c.collectInstanceCandidates(ctx, mtimes, opts.Until)
		if err != nil {
			return report, err
		}
		instanceCandidates = cands
		report.Skipped = append(report.Skipped, skipped...)
	}

	// Step 2b: orphan candidates. Skipped: empty ImagesDir means there's
	// no configured cache, and passing "" to os.ReadDir would scan the
	// daemon's working directory (Codex review).
	var orphanCandidates []orphanCandidate
	if opts.Orphans && c.cfg.ImagesDir != "" {
		cands, skipped := collectOrphanCandidates(c.cfg.ImagesDir)
		orphanCandidates = cands
		report.Skipped = append(report.Skipped, skipped...)
	}

	// Step 2c: image candidates (dry-run, respects candidate deletions).
	// Same empty-ImagesDir guard for safety.
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

	// Step 2d: log-truncation candidates. Critical: these must be
	// collected during the candidate phase — NOT the execute phase — or
	// the dry-run-first CLI flow sees zero candidates and exits before
	// anything can be truncated. (Codex review #1.)
	var logCandidates []logCandidate
	if opts.Logs {
		logCandidates = c.collectLogCandidates(opts.LogTailBytes)
	}

	// Populate candidate Items for dry-run display. On execute we'll
	// rebuild with post-action Freed numbers — but for instances/orphans
	// the pre-delete attributed bytes are exactly what we want to show.
	for _, ic := range instanceCandidates {
		report.Items = append(report.Items, ic.toPrunedItem(opts.DryRun))
	}
	for _, oc := range orphanCandidates {
		report.Items = append(report.Items, oc.toPrunedItem(opts.DryRun))
	}
	for _, img := range imageCandidates {
		report.Items = append(report.Items, imageToPrunedItem(img, opts.DryRun))
	}
	for _, lc := range logCandidates {
		report.Items = append(report.Items, lc.toPrunedItem(opts.DryRun))
	}

	if opts.DryRun {
		finalizeReport(&report)
		return report, nil
	}

	// Step 3: execute. Reset items so we report only what ACTUALLY ran.
	report.Items = report.Items[:0]

	// 3a: delete candidate instances via DeleteShed (handles TAP/cred
	// cleanup correctly and re-checks running state under its own lock).
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

	// 3b: real image prune. ListInstances no longer sees deleted sheds,
	// so the unmodified inUseImageNames closure returns the correct set.
	// On error we still return the report (with Items populated from 3a)
	// so the client sees partial progress rather than a bare 500.
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

	// 3c: sweep orphans — re-acquire flock for safety.
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

	// 3d: truncate console.log on surviving VZ sheds.
	if opts.Logs {
		for _, lc := range logCandidates {
			if err := truncateConsoleLogInPlace(lc.path, lc.origSize, opts.LogTailBytes); err != nil {
				report.Skipped = append(report.Skipped, config.SkippedItem{
					Kind: "console_log", Name: lc.shedName, Path: lc.path,
					Reason: fmt.Sprintf("truncate failed: %v", err),
				})
				continue
			}
			report.Items = append(report.Items, lc.toPrunedItem(false))
		}
	}

	finalizeReport(&report)
	return report, nil
}

// normalizePruneOptions applies defaults: empty scope → images+instances+
// orphans; zero LogTailBytes → 5 MiB.
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

// scopeFlags returns the list of active scope names for the report.
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

// snapshotMetadataMtimes reads mtime(metadata.json) for every instance under
// instanceDir. Must run BEFORE any call that might touch metadata (ListSheds,
// GetShed) since those paths save on stale-running→stopped detection.
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

// instanceCandidate is a stopped shed that is eligible for prune.
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

// collectInstanceCandidates returns stopped sheds whose mtime(metadata.json)
// is older than until. Running sheds and too-recent stopped sheds land in
// the skipped list. Malformed metadata is silently skipped (matches
// ListSheds behavior).
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
			// Metadata disappeared between snapshot and ListSheds; skip.
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

// instanceSize returns the attributed bytes for an instance directory
// (rootfs + console.log + metadata + any other files). Measured before
// deletion so the report shows what was reclaimed.
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

// orphanCandidate is a sidecar that's lost its parent rootfs AND has passed
// flock + age checks.
type orphanCandidate struct {
	path string
	kind string // "lock" | "tmp" | "source"
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

// collectOrphanCandidates scans imagesDir for sidecars whose parent rootfs
// is absent, after verifying the canonical `.lock` is NOT held by a live
// conversion and the file is not a very-fresh `.tmp`.
//
// `.lock` orphans are NEVER candidates — sweepOrphan refuses to remove them
// to avoid the inode-reuse race (see Manager.PruneImages). Instead they're
// reported as SkippedItem with reason "lock file retained" so the report
// doesn't lie about what will/did happen. (Codex review #4.)
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

		// .lock orphans: preserved, not deleted. Recorded as SkippedItem
		// so the report accurately reflects the no-op.
		if kind == "lock" {
			skipped = append(skipped, config.SkippedItem{
				Kind: kind, Path: path,
				Reason: "lock file retained (inode-reuse race safety)",
			})
			continue
		}

		// .tmp files younger than tmpOrphanMinAge are almost certainly an
		// in-flight EnsureImage. Skip with reason.
		if kind == "tmp" && now.Sub(fi.ModTime()) < tmpOrphanMinAge {
			skipped = append(skipped, config.SkippedItem{
				Kind: kind, Path: path,
				Reason: fmt.Sprintf("too recent (%s < %s)", humanDuration(now.Sub(fi.ModTime())), humanDuration(tmpOrphanMinAge)),
			})
			continue
		}

		// Confirm the canonical lock is NOT held. A conversion can write
		// a .tmp before the ext4 exists, holding the .lock the whole time.
		// TryAcquireFileLock treats a missing .lock as "nothing to contend
		// with" so we don't pollute the cache dir with new lock files
		// during dry-run probes.
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

// sweepOrphan re-acquires the canonical flock and deletes the sidecar.
// Returns false on any failure — the caller should record a SkippedItem.
// `.lock` orphans are filtered out upstream in collectOrphanCandidates so
// by the time sweepOrphan runs, oc.kind is "tmp" or "source".
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

	if err := os.Remove(oc.path); err != nil {
		// Treat "already gone" as success — another sweep beat us to it.
		return errors.Is(err, os.ErrNotExist)
	}
	return true
}

// imageToPrunedItem wraps a vmimage.ImageInfo as a PrunedItem. Physical
// bytes are pulled via diskstat so the report reflects the actual on-disk
// footprint (ImageInfo only carries logical SizeBytes).
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

// logCandidate is a console.log file eligible for truncation. Captured
// during the candidate phase so dry-run reports honor --logs, matching
// how instance/orphan/image candidates flow.
type logCandidate struct {
	shedName  string
	path      string
	origSize  int64
	tailBytes int64
}

func (lc logCandidate) toPrunedItem(dry bool) config.PrunedItem {
	freed := lc.origSize - lc.tailBytes
	reason := fmt.Sprintf("would keep last %s", humanBytes(lc.tailBytes))
	if !dry {
		reason = fmt.Sprintf("kept last %s", humanBytes(lc.tailBytes))
	}
	return config.PrunedItem{
		Kind: "console_log", Name: lc.shedName, Path: lc.path,
		Action: "truncated",
		Freed:  config.DiskSize{LogicalBytes: freed, PhysicalBytes: freed},
		Reason: reason,
	}
}

// collectLogCandidates returns one logCandidate per VZ shed whose
// console.log is larger than tailBytes. Pure read — no mutation — so
// it's safe to run unconditionally during the candidate phase.
func (c *Client) collectLogCandidates(tailBytes int64) []logCandidate {
	var cands []logCandidate

	names, err := ListInstances(c.cfg.InstanceDir)
	if err != nil {
		return nil
	}
	for _, name := range names {
		consolePath := filepath.Join(InstanceDir(c.cfg.InstanceDir, name), consoleLogFilename)
		fi, err := os.Stat(consolePath)
		if err != nil {
			continue // no log — nothing to report
		}
		if fi.Size() <= tailBytes {
			continue // already small enough; skip silently (don't clutter)
		}
		cands = append(cands, logCandidate{
			shedName:  name,
			path:      consolePath,
			origSize:  fi.Size(),
			tailBytes: tailBytes,
		})
	}
	return cands
}

// humanBytes is a tiny helper for log reasons. Avoids pulling in the
// CLI's formatSize (different package).
func humanBytes(n int64) string {
	const mb = 1024 * 1024
	const kb = 1024
	switch {
	case n >= mb:
		return fmt.Sprintf("%.1f MiB", float64(n)/float64(mb))
	case n >= kb:
		return fmt.Sprintf("%.1f KiB", float64(n)/float64(kb))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// truncateConsoleLogInPlace truncates path to 0 and rewrites the last
// tailBytes of its original contents to the head. Preserves inode so
// vfkit's O_APPEND fd continues appending past the rewritten tail.
func truncateConsoleLogInPlace(path string, origSize, tailBytes int64) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer f.Close()

	tail := make([]byte, tailBytes)
	if _, err := f.ReadAt(tail, origSize-tailBytes); err != nil {
		return fmt.Errorf("read tail: %w", err)
	}
	if err := f.Truncate(0); err != nil {
		return fmt.Errorf("ftruncate: %w", err)
	}
	if _, err := f.Seek(0, 0); err != nil {
		return fmt.Errorf("seek: %w", err)
	}
	if _, err := f.Write(tail); err != nil {
		return fmt.Errorf("write tail: %w", err)
	}
	return nil
}

// finalizeReport sums Freed across all items into Totals.
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

// humanDuration formats a duration for SkippedItem.Reason / PrunedItem.Reason.
// Rounds to the nearest sensible unit: seconds, minutes, hours, or days.
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
