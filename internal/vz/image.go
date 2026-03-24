//go:build darwin

package vz

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
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
			cached := filepath.Join(c.cfg.ImagesDir, vmimage.RootfsFilename(name))
			if fi, err := os.Stat(cached); err == nil {
				info.Path = cached
				info.SizeBytes = fi.Size()
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
		os.Remove(path)
	}, nil
}
