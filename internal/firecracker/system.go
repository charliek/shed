//go:build linux
// +build linux

package firecracker

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
