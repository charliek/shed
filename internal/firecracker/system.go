//go:build linux
// +build linux

package firecracker

import (
	"context"
	"fmt"
	"time"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/diskstat"
	"github.com/charliek/shed/internal/systemprune"
	"github.com/charliek/shed/internal/vmimage"
)

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
		Snapshots:   []config.SnapshotDiskEntry{},
		Orphans:     []config.FileEntry{},
	}

	imagesDir := c.cfg.ImagesDir
	if imagesDir != "" {
		imgs, err := c.ListImages()
		if err != nil {
			return du, fmt.Errorf("listing images: %w", err)
		}
		seenDigest := make(map[string]bool)
		for _, img := range imgs {
			if !img.Cached {
				continue
			}
			entry := config.ImageDiskEntry{
				Name:      img.Name,
				Path:      img.Path,
				DockerRef: img.DockerRef,
			}
			// Count on-disk bytes once per content digest: multiple refs can
			// point at the same cached manifest, but the blob exists once.
			// Subsequent rows for the same digest stay visible with zero size.
			if !seenDigest[img.Digest] {
				if img.Path != "" {
					entry.Size.LogicalBytes, entry.Size.PhysicalBytes, _ = diskstat.Stat(img.Path)
				}
				// Fall back to the manifest-computed footprint (layers + lower)
				// when there's no single statable lower file.
				if entry.Size.LogicalBytes == 0 {
					entry.Size.LogicalBytes = img.SizeBytes
					entry.Size.PhysicalBytes = img.SizeBytes
				}
				seenDigest[img.Digest] = true
			}
			du.Images = append(du.Images, entry)
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

	// Uppers left behind by crashed creates that the operator never
	// retried (the CreateShed-time sweep handles the retry case, but
	// not "operator gave up").
	if c.cfg.UppersDir != "" {
		upperOrphans, err := systemprune.FindUpperOrphans(c.cfg.UppersDir, c.cfg.InstanceDir)
		if err != nil {
			return du, fmt.Errorf("scanning upper orphans: %w", err)
		}
		du.Orphans = append(du.Orphans, upperOrphans...)
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

	// Snapshots
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

	if len(du.Snapshots) > 0 {
		du.Notes = append(du.Notes,
			"snapshot upper extents are shared via reflink with sheds spawned from the snapshot; physical bytes count those extents under both",
		)
	}

	return du, nil
}

// shedDiskEntryForFC builds a ShedDiskEntry for a single Firecracker instance.
// No ConsoleLog (FC writes to stderr, not a per-instance file).
//
// Per-shed accounting reflects only the writable upper layer. The
// (much larger) read-only lower image is shared across every shed
// pinning the same digest and is reported once in du.Images, so
// summing per-shed bytes against image bytes gives an honest picture
// of total disk consumed.
func shedDiskEntryForFC(instanceDir string, meta *Metadata) config.ShedDiskEntry {
	entry := config.ShedDiskEntry{
		Name:   meta.Name,
		Status: meta.Status,
		Image:  meta.Image,
	}

	upperPath := meta.UpperPath
	if upperPath == "" {
		upperPath = meta.RootfsPath
	}
	if upperPath == "" {
		upperPath = RootfsPath(instanceDir, meta.Name)
	}
	rootfsLogical, rootfsPhysical, _ := diskstat.Stat(upperPath)
	entry.Rootfs = config.FileEntry{
		Path: upperPath,
		Size: config.DiskSize{LogicalBytes: rootfsLogical, PhysicalBytes: rootfsPhysical},
		Kind: "upper",
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

// Prune removes items selected by opts. See backend.PruneOptions for flag
// semantics. Mirrors the VZ implementation except:
//   - No console.log handling (FC has no per-instance console log).
//   - DeleteShed also frees TAP devices and unregisters CID/IP.
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

	// Step 1: snapshot mtimes BEFORE ListSheds.
	instanceNames, _ := ListInstances(c.cfg.InstanceDir)
	mtimes := systemprune.SnapshotMetadataMtimes(c.cfg.InstanceDir, instanceNames)

	var instanceCandidates []systemprune.InstanceCandidate
	if opts.Instances {
		cands, skipped, err := c.collectInstanceCandidates(ctx, mtimes, opts.Until)
		if err != nil {
			return report, err
		}
		instanceCandidates = cands
		report.Skipped = append(report.Skipped, skipped...)
	}

	// Empty ImagesDir guard (Codex #7) — os.ReadDir("") reads CWD.
	var orphanCandidates []systemprune.OrphanCandidate
	if opts.Orphans && c.cfg.ImagesDir != "" {
		cands, skipped := systemprune.CollectOrphanCandidates(c.cfg.ImagesDir)
		orphanCandidates = cands
		report.Skipped = append(report.Skipped, skipped...)
	}

	var snapshotOrphanCandidates []systemprune.SnapshotOrphanCandidate
	if opts.Orphans && c.cfg.SnapshotsDir != "" {
		cands, skipped := systemprune.CollectSnapshotOrphanCandidates(c.cfg.SnapshotsDir)
		snapshotOrphanCandidates = cands
		report.Skipped = append(report.Skipped, skipped...)
	}

	var upperOrphanCandidates []systemprune.UpperOrphanCandidate
	if opts.Orphans && c.cfg.UppersDir != "" {
		cands, skipped := systemprune.CollectUpperOrphanCandidates(c.cfg.UppersDir, c.cfg.InstanceDir)
		upperOrphanCandidates = cands
		report.Skipped = append(report.Skipped, skipped...)
	}

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
		report.Items = append(report.Items, ic.ToPrunedItem(opts.DryRun))
	}
	for _, oc := range orphanCandidates {
		report.Items = append(report.Items, oc.ToPrunedItem(opts.DryRun))
	}
	for _, sc := range snapshotOrphanCandidates {
		report.Items = append(report.Items, sc.ToPrunedItems(opts.DryRun)...)
	}
	for _, uc := range upperOrphanCandidates {
		report.Items = append(report.Items, uc.ToPrunedItems(opts.DryRun)...)
	}
	for _, img := range imageCandidates {
		report.Items = append(report.Items, systemprune.ImageToPrunedItem(img, opts.DryRun))
	}
	report.Skipped = append(report.Skipped, logSkips...)

	if opts.DryRun {
		systemprune.FinalizeReport(&report)
		return report, nil
	}

	report.Items = report.Items[:0]

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
		for _, uc := range upperOrphanCandidates {
			if ok := systemprune.SweepUpperOrphan(uc); !ok {
				report.Skipped = append(report.Skipped, config.SkippedItem{
					Kind: "upper_orphan", Path: uc.Dir,
					Reason: "removal failed",
				})
				continue
			}
			report.Items = append(report.Items, uc.ToPrunedItems(false)...)
		}
	}

	systemprune.FinalizeReport(&report)
	return report, nil
}

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
