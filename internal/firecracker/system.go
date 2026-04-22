//go:build linux
// +build linux

package firecracker

import (
	"context"
	"fmt"
	"path/filepath"
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

		orphans, err := systemprune.FindOrphans(imagesDir)
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

	var imageCandidates []vmimage.ImageInfo
	if opts.Images && c.cfg.ImagesDir != "" {
		skipSet := make(map[string]bool, len(instanceCandidates))
		for _, ic := range instanceCandidates {
			skipSet[ic.Name] = true
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
		report.Items = append(report.Items, ic.ToPrunedItem(opts.DryRun))
	}
	for _, oc := range orphanCandidates {
		report.Items = append(report.Items, oc.ToPrunedItem(opts.DryRun))
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
		mgr := vmimage.NewManager(c.cfg)
		deleted, err := mgr.PruneImages(false, c.inUseImageNames)
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
