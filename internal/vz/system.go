//go:build darwin
// +build darwin

package vz

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/diskstat"
	"github.com/charliek/shed/internal/systemprune"
	"github.com/charliek/shed/internal/vmimage"
)

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
		Snapshots:   []config.SnapshotDiskEntry{},
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
		// and is intentionally omitted from ListImages(). Resolve via the
		// content-addressed blob store and stat the underlying rootfs.
		if basePath := vmimage.Resolve(imagesDir, "_base", ""); basePath != "" {
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
		}

		orphans, err := systemprune.FindOrphans(imagesDir)
		if err != nil {
			return du, fmt.Errorf("scanning orphans: %w", err)
		}
		du.Orphans = orphans
	}

	if c.cfg.SnapshotsDir != "" {
		snapOrphans, err := systemprune.FindSnapshotOrphans(c.cfg.SnapshotsDir)
		if err != nil {
			return du, fmt.Errorf("scanning snapshot orphans: %w", err)
		}
		du.Orphans = append(du.Orphans, snapOrphans...)
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

	// Snapshots: enumerate {snapshotsDir}/{name}/ directories and stat their rootfs.
	if c.cfg.SnapshotsDir != "" {
		names, err := listSnapshotNames(c.cfg.SnapshotsDir)
		if err != nil {
			return du, fmt.Errorf("listing snapshots: %w", err)
		}
		for _, name := range names {
			snap, err := loadSnapshot(c.cfg.SnapshotsDir, name)
			if err != nil {
				continue
			}
			rootfs := SnapshotRootfsPath(c.cfg.SnapshotsDir, name)
			logical, physical, _ := diskstat.Stat(rootfs)
			entry := config.SnapshotDiskEntry{
				Name:       snap.Name,
				SourceShed: snap.SourceShed,
				Rootfs: config.FileEntry{
					Path: rootfs,
					Size: config.DiskSize{LogicalBytes: logical, PhysicalBytes: physical},
					Kind: "rootfs",
				},
				Total: config.DiskSize{LogicalBytes: logical, PhysicalBytes: physical},
			}
			meta := SnapshotMetadataPath(c.cfg.SnapshotsDir, name)
			if metaLogical, metaPhysical, err := diskstat.Stat(meta); err == nil {
				entry.OtherFiles = append(entry.OtherFiles, config.FileEntry{
					Path: meta,
					Size: config.DiskSize{LogicalBytes: metaLogical, PhysicalBytes: metaPhysical},
					Kind: "metadata",
				})
				entry.Total.LogicalBytes += metaLogical
				entry.Total.PhysicalBytes += metaPhysical
			}
			du.Snapshots = append(du.Snapshots, entry)
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
	for _, snap := range du.Snapshots {
		du.Totals.Snapshots.LogicalBytes += snap.Total.LogicalBytes
		du.Totals.Snapshots.PhysicalBytes += snap.Total.PhysicalBytes
	}
	for _, orph := range du.Orphans {
		du.Totals.Orphans.LogicalBytes += orph.Size.LogicalBytes
		du.Totals.Orphans.PhysicalBytes += orph.Size.PhysicalBytes
	}
	du.Totals.All.LogicalBytes = du.Totals.Images.LogicalBytes + du.Totals.Sheds.LogicalBytes + du.Totals.Snapshots.LogicalBytes + du.Totals.Orphans.LogicalBytes
	du.Totals.All.PhysicalBytes = du.Totals.Images.PhysicalBytes + du.Totals.Sheds.PhysicalBytes + du.Totals.Snapshots.PhysicalBytes + du.Totals.Orphans.PhysicalBytes

	du.Notes = append(du.Notes,
		"physical bytes may overcount shared extents on APFS (clonefile) or hardlinks",
	)
	if len(du.Snapshots) > 0 {
		du.Notes = append(du.Notes,
			"rootfs extents are shared via reflink between a snapshot and sheds spawned from it — physical bytes count those extents under both; metadata files are unique per snapshot",
		)
	}

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
	opts = systemprune.NormalizePruneOptions(opts)

	report := config.PruneReport{
		DryRun:     opts.DryRun,
		ServerName: c.serverCfg.Name,
		Scope:      systemprune.ScopeFlags(opts),
		Until:      opts.Until.String(),
		Items:      []config.PrunedItem{},
		Skipped:    []config.SkippedItem{},
	}

	// Step 1: snapshot mtimes BEFORE ListSheds. Plan-mandated: ListSheds
	// can refresh mtime via its stale-running→stopped re-check, which would
	// otherwise reset the age clock and confuse the age filter.
	instanceNames, _ := ListInstances(c.cfg.InstanceDir)
	mtimes := systemprune.SnapshotMetadataMtimes(c.cfg.InstanceDir, instanceNames)

	// Step 2a: instance candidates.
	var instanceCandidates []systemprune.InstanceCandidate
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
	var orphanCandidates []systemprune.OrphanCandidate
	if opts.Orphans && c.cfg.ImagesDir != "" {
		cands, skipped := systemprune.CollectOrphanCandidates(c.cfg.ImagesDir)
		orphanCandidates = cands
		report.Skipped = append(report.Skipped, skipped...)
	}

	// Step 2b': snapshot orphan candidates (partial snapshot dirs).
	var snapshotOrphanCandidates []systemprune.SnapshotOrphanCandidate
	if opts.Orphans && c.cfg.SnapshotsDir != "" {
		cands, skipped := systemprune.CollectSnapshotOrphanCandidates(c.cfg.SnapshotsDir)
		snapshotOrphanCandidates = cands
		report.Skipped = append(report.Skipped, skipped...)
	}

	// Step 2c: image candidates (dry-run, respects candidate deletions).
	// Same empty-ImagesDir guard for safety.
	var imageCandidates []vmimage.ImageInfo
	if opts.Images && c.cfg.ImagesDir != "" {
		skipSet := make(map[string]bool, len(instanceCandidates))
		for _, ic := range instanceCandidates {
			skipSet[ic.Name] = true
		}
		mgr := vmimage.NewManager(c.cfg, c.refScannerExcept(skipSet))
		cands, err := mgr.PruneImages(true)
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
		report.Items = append(report.Items, ic.ToPrunedItem(opts.DryRun))
	}
	for _, oc := range orphanCandidates {
		report.Items = append(report.Items, oc.ToPrunedItem(opts.DryRun))
	}
	for _, sc := range snapshotOrphanCandidates {
		report.Items = append(report.Items, sc.ToPrunedItems(opts.DryRun)...)
	}
	for _, img := range imageCandidates {
		report.Items = append(report.Items, systemprune.ImageToPrunedItem(img, opts.DryRun))
	}
	for _, lc := range logCandidates {
		report.Items = append(report.Items, lc.toPrunedItem(opts.DryRun))
	}

	if opts.DryRun {
		systemprune.FinalizeReport(&report)
		return report, nil
	}

	// Step 3: execute. Reset items so we report only what ACTUALLY ran.
	report.Items = report.Items[:0]

	// 3a: delete candidate instances via DeleteShed (handles TAP/cred
	// cleanup correctly and re-checks running state under its own lock).
	for _, ic := range instanceCandidates {
		if err := c.DeleteShed(ctx, ic.Name, false); err != nil {
			report.Skipped = append(report.Skipped, config.SkippedItem{
				Kind: "instance", Name: ic.Name,
				Reason: fmt.Sprintf("delete failed: %v", err),
			})
			continue
		}
		report.Items = append(report.Items, ic.ToPrunedItem(false))
	}

	// 3b: real image prune. ListInstances no longer sees deleted sheds,
	// so the unmodified inUseImageNames closure returns the correct set.
	// On error we still return the report (with Items populated from 3a)
	// so the client sees partial progress rather than a bare 500.
	if opts.Images && c.cfg.ImagesDir != "" {
		mgr := vmimage.NewManager(c.cfg, c.refScanner())
		deleted, err := mgr.PruneImages(false)
		if err != nil {
			systemprune.FinalizeReport(&report)
			return report, fmt.Errorf("image prune: %w", err)
		}
		for _, img := range deleted {
			report.Items = append(report.Items, systemprune.ImageToPrunedItem(img, false))
		}
	}

	// 3c: sweep orphans — re-acquire flock for safety.
	if opts.Orphans {
		for _, oc := range orphanCandidates {
			if ok := systemprune.SweepOrphan(c.cfg.ImagesDir, oc); !ok {
				report.Skipped = append(report.Skipped, config.SkippedItem{
					Kind: oc.Kind, Path: oc.Path,
					Reason: "lock now held or removal failed",
				})
				continue
			}
			report.Items = append(report.Items, oc.ToPrunedItem(false))
		}
		for _, sc := range snapshotOrphanCandidates {
			if ok := systemprune.SweepSnapshotOrphan(sc); !ok {
				report.Skipped = append(report.Skipped, config.SkippedItem{
					Kind: "snapshot_orphan", Path: sc.Dir,
					Reason: "removal failed",
				})
				continue
			}
			report.Items = append(report.Items, sc.ToPrunedItems(false)...)
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

	systemprune.FinalizeReport(&report)
	return report, nil
}

// collectInstanceCandidates returns stopped sheds whose mtime(metadata.json)
// is older than until. Running sheds and too-recent stopped sheds land in
// the skipped list. Malformed metadata is silently skipped (matches
// ListSheds behavior).
func (c *Client) collectInstanceCandidates(ctx context.Context, mtimes map[string]time.Time, until time.Duration) ([]systemprune.InstanceCandidate, []config.SkippedItem, error) {
	sheds, err := c.ListSheds(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("listing sheds: %w", err)
	}
	var cands []systemprune.InstanceCandidate
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
				Reason: fmt.Sprintf("too recent (%s < %s)", systemprune.HumanDuration(age), systemprune.HumanDuration(until)),
			})
			continue
		}
		cands = append(cands, systemprune.InstanceCandidate{
			Name:  shed.Name,
			Image: shed.Image,
			Age:   age,
			Size:  systemprune.InstanceSize(c.cfg.InstanceDir, shed.Name),
		})
	}
	return cands, skipped, nil
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

// humanBytes is a tiny helper for log reasons. Log-truncation messages
// report MiB/KiB — a different format than systemprune.HumanDuration.
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
