//go:build darwin

package vz

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

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
		outputDir = config.ExpandPath("~/Library/Application Support/shed/vz")
	}

	rootfsFile := vmimage.RootfsFilename(resolved.Name)
	rootfsPath := filepath.Join(outputDir, rootfsFile)
	sourceFile := filepath.Join(outputDir, vmimage.SourceFilename(resolved.Name))

	// Acquire a file lock to prevent concurrent conversions
	lockPath := rootfsPath + ".lock"
	unlock, err := acquireFileLock(lockPath)
	if err != nil {
		return "", fmt.Errorf("failed to acquire lock for image conversion: %w", err)
	}
	defer unlock()

	// Re-check cache after acquiring lock (another process may have completed)
	if _, err := os.Stat(rootfsPath); err == nil {
		if source, err := os.ReadFile(sourceFile); err == nil && strings.TrimSpace(string(source)) == resolved.DockerRef {
			return rootfsPath, nil
		}
		// Source mismatch — remove stale cache for re-conversion
		log.Printf("Image %q source changed, re-converting from %s", resolved.Name, resolved.DockerRef)
		os.Remove(rootfsPath)
		os.Remove(sourceFile)
	}

	// Pull and convert
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

	// Write source sidecar for cache invalidation
	if err := os.WriteFile(sourceFile, []byte(resolved.DockerRef+"\n"), 0644); err != nil {
		log.Printf("Warning: failed to write source sidecar %s: %v", sourceFile, err)
	}

	return result.RootfsPath, nil
}

// ListImages returns available image variants from config and auto-discovery in ImagesDir.
func (c *Client) ListImages() ([]config.ImageInfo, error) {
	seen := make(map[string]bool)
	var images []config.ImageInfo

	// Images from config
	for name, val := range c.cfg.Images {
		seen[name] = true
		info := config.ImageInfo{
			Name:   name,
			Source: "config",
		}
		if vmimage.IsDockerRef(val) {
			info.DockerRef = val
			// Check if cached
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

	// Auto-discovered images in ImagesDir
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

// acquireFileLock creates a simple file-based lock using O_CREATE|O_EXCL.
// Returns an unlock function to release the lock.
func acquireFileLock(path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, err
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		if os.IsExist(err) {
			return nil, fmt.Errorf("another conversion is in progress (lock file: %s)", path)
		}
		return nil, err
	}
	f.Close()

	return func() {
		os.Remove(path)
	}, nil
}
