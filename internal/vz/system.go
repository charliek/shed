//go:build darwin
// +build darwin

package vz

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/diskstat"
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
