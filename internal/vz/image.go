//go:build darwin

package vz

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/vmimage"
)

// EnsureImage ensures a resolved image is available as a local ext4 file.
// If the image is already a local path, it returns that path directly.
// If it's a Docker reference, it pulls and converts to ext4, caching the result.
func EnsureImage(ctx context.Context, resolved config.ResolvedImage, cfg *config.VZConfig) (string, error) {
	if resolved.Path != "" {
		return resolved.Path, nil
	}

	if resolved.DockerRef == "" {
		return "", fmt.Errorf("resolved image has neither path nor docker ref")
	}

	outputDir := cfg.ImagesDir
	if outputDir == "" {
		outputDir = config.ExpandPath(config.DefaultVZImagesDir)
	}

	rootfsPath := filepath.Join(outputDir, vmimage.RootfsFilename(resolved.Name))

	// Acquire a flock-based lock to prevent concurrent conversions.
	// Unlike O_EXCL, flock is automatically released if the process crashes.
	lockPath := rootfsPath + ".lock"
	unlock, err := acquireFileLock(lockPath)
	if err != nil {
		return "", fmt.Errorf("failed to acquire lock for image conversion: %w", err)
	}
	defer unlock()

	// Re-check cache after acquiring lock (another process may have completed)
	if cached := vmimage.CheckCache(outputDir, resolved.Name, resolved.DockerRef); cached != "" {
		return cached, nil
	}

	// Stale cache — remove for re-conversion
	os.Remove(rootfsPath)

	backend.Progress(ctx, "image", fmt.Sprintf("Pulling %s...", resolved.DockerRef))
	log.Printf("Converting Docker image %s to ext4 for variant %q", resolved.DockerRef, resolved.Name)

	result, err := vmimage.Convert(ctx, vmimage.ConvertOptions{
		DockerRef:     resolved.DockerRef,
		Name:          resolved.Name,
		OutputDir:     outputDir,
		ExtractKernel: true,
	})
	if err != nil {
		return "", fmt.Errorf("failed to convert image %s: %w", resolved.DockerRef, err)
	}

	if err := vmimage.WriteSource(outputDir, resolved.Name, resolved.DockerRef); err != nil {
		log.Printf("Warning: failed to write source sidecar: %v", err)
	}

	return result.RootfsPath, nil
}

// ListImages returns available image variants from config and auto-discovery in ImagesDir.
func (c *Client) ListImages() ([]config.ImageInfo, error) {
	seen := make(map[string]bool)
	var images []config.ImageInfo

	for name, val := range c.cfg.Images {
		seen[name] = true
		info := config.ImageInfo{
			Name:   name,
			Source: "config",
		}
		if vmimage.IsDockerRef(val) {
			info.DockerRef = val
			if cached := vmimage.CheckCache(c.cfg.ImagesDir, name, val); cached != "" {
				info.Path = cached
				if fi, err := os.Stat(cached); err == nil {
					info.SizeBytes = fi.Size()
				}
				info.Cached = true
			}
		} else {
			info.Path = val
			info.Cached = true
			if fi, err := os.Stat(val); err == nil {
				info.SizeBytes = fi.Size()
			}
		}
		images = append(images, info)
	}

	if c.cfg.ImagesDir != "" {
		entries, err := os.ReadDir(c.cfg.ImagesDir)
		if err == nil {
			for _, e := range entries {
				if strings.HasSuffix(e.Name(), "-rootfs.ext4") && !e.IsDir() {
					name := strings.TrimSuffix(e.Name(), "-rootfs.ext4")
					if name == "" || name == "_base" || seen[name] {
						continue
					}
					info := config.ImageInfo{
						Name:   name,
						Path:   filepath.Join(c.cfg.ImagesDir, e.Name()),
						Source: "discovered",
						Cached: true,
					}
					if fi, err := e.Info(); err == nil {
						info.SizeBytes = fi.Size()
					}
					images = append(images, info)
				}
			}
		}
	}

	return images, nil
}

// resolveImagesDir returns the images directory, falling back to the default.
func (c *Client) resolveImagesDir() string {
	if c.cfg.ImagesDir != "" {
		return c.cfg.ImagesDir
	}
	return config.ExpandPath(config.DefaultVZImagesDir)
}

// validateImageName validates that an image name is safe for filesystem operations.
func validateImageName(name string) error {
	if name == "" {
		return fmt.Errorf("image name cannot be empty")
	}
	if strings.Contains(name, "..") {
		return fmt.Errorf("invalid image name: %q", name)
	}
	if strings.ContainsRune(name, filepath.Separator) || strings.ContainsRune(name, '/') {
		return fmt.Errorf("invalid image name: %q", name)
	}
	return nil
}

// DeleteImage removes a cached image by name.
// It deletes the ext4 rootfs and source sidecar but NOT the lock file.
func (c *Client) DeleteImage(name string) error {
	if err := validateImageName(name); err != nil {
		return err
	}

	// Refuse if this image is in the config Images map
	if _, ok := c.cfg.Images[name]; ok {
		return config.ErrImageInUseSentinel
	}

	// Refuse if this is _base and BaseRootfs is a Docker ref (it produced this cached image)
	if name == "_base" && vmimage.IsDockerRef(c.cfg.BaseRootfs) {
		return config.ErrImageInUseSentinel
	}

	// Refuse if any existing shed references this image
	instances, err := ListInstances(c.cfg.InstanceDir)
	if err == nil {
		for _, inst := range instances {
			meta, err := LoadMetadata(c.cfg.InstanceDir, inst)
			if err != nil {
				continue
			}
			if meta.Image == name {
				return config.ErrImageInUseSentinel
			}
		}
	}

	imagesDir := c.resolveImagesDir()

	rootfsPath := filepath.Join(imagesDir, vmimage.RootfsFilename(name))
	if err := os.Remove(rootfsPath); err != nil {
		if os.IsNotExist(err) {
			return config.ErrImageNotFoundSentinel
		}
		return fmt.Errorf("removing rootfs: %w", err)
	}

	// Best-effort removal of source sidecar (NOT the lock file)
	os.Remove(filepath.Join(imagesDir, vmimage.SourceFilename(name)))

	return nil
}

// PruneImages removes cached images not referenced by config or existing sheds.
// If dryRun is true, returns candidates without deleting.
func (c *Client) PruneImages(dryRun bool) ([]config.ImageInfo, error) {
	imagesDir := c.resolveImagesDir()

	entries, err := os.ReadDir(imagesDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to read images directory: %w", err)
	}

	// Build exclusion set
	exclude := make(map[string]bool)

	// Config-managed images
	for name := range c.cfg.Images {
		exclude[name] = true
	}

	// _base if BaseRootfs is a Docker ref
	if vmimage.IsDockerRef(c.cfg.BaseRootfs) {
		exclude["_base"] = true
	}

	// Images referenced by existing sheds — fail closed if we can't read metadata
	instances, err := ListInstances(c.cfg.InstanceDir)
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("listing instances: %w", err)
	}
	for _, inst := range instances {
		meta, err := LoadMetadata(c.cfg.InstanceDir, inst)
		if err != nil {
			return nil, fmt.Errorf("reading metadata for %s: %w", inst, err)
		}
		if meta.Image != "" {
			exclude[meta.Image] = true
		}
	}

	// Find candidates
	var candidates []config.ImageInfo
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), "-rootfs.ext4") || e.IsDir() {
			continue
		}
		name := strings.TrimSuffix(e.Name(), "-rootfs.ext4")
		if name == "" || exclude[name] {
			continue
		}

		info := config.ImageInfo{
			Name:   name,
			Path:   filepath.Join(imagesDir, e.Name()),
			Cached: true,
		}
		if fi, err := e.Info(); err == nil {
			info.SizeBytes = fi.Size()
		}
		// Read source sidecar for docker ref
		sourceFile := filepath.Join(imagesDir, vmimage.SourceFilename(name))
		if data, err := os.ReadFile(sourceFile); err == nil {
			info.DockerRef = strings.TrimSpace(string(data))
		}
		candidates = append(candidates, info)
	}

	// Sort for deterministic output
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].Name < candidates[j].Name
	})

	if dryRun {
		return candidates, nil
	}

	// Delete candidates — only report images whose rootfs was actually removed
	var deleted []config.ImageInfo
	for _, img := range candidates {
		if err := os.Remove(img.Path); err != nil && !os.IsNotExist(err) {
			log.Printf("warning: failed to remove %s: %v", img.Path, err)
			continue
		}
		// Best-effort removal of source sidecar
		os.Remove(filepath.Join(imagesDir, vmimage.SourceFilename(img.Name)))
		deleted = append(deleted, img)
	}

	return deleted, nil
}

// acquireFileLock acquires an exclusive flock on the given path.
// The lock is automatically released if the process exits or crashes.
func acquireFileLock(path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, err
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return nil, err
	}

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, fmt.Errorf("failed to acquire lock: %w", err)
	}

	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN) //nolint:errcheck
		f.Close()
		// Lock file is intentionally not removed — deleting it creates a race
		// where concurrent processes can hold locks on different inodes.
	}, nil
}
