// Package vmimage provides Docker-to-ext4 image conversion and image lifecycle
// management for VM backends. This package is cross-platform (no build tags)
// so it can be tested on Linux CI. Both VZ and Firecracker backends use ext4
// rootfs images and share this pipeline.
//
// Import constraint: config imports vmimage, so vmimage must NOT import config
// or backend. All external dependencies are injected via interfaces and closures.
package vmimage

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

// Sentinel errors for image operations. Backend wrappers map these to
// config sentinel errors (e.g., config.ErrImageNotFoundSentinel).
var (
	// ErrImageNotFound is returned when a cached image does not exist.
	ErrImageNotFound = errors.New("image not found")

	// ErrImageInUse is returned when trying to delete an image referenced
	// by config or an existing shed.
	ErrImageInUse = errors.New("image is in use")
)

// ImageConfig provides the configuration data needed by image management operations.
// Both VZConfig and FirecrackerConfig implement this interface.
type ImageConfig interface {
	GetImages() map[string]string
	GetImagesDir() string
	GetBaseRootfs() string
	GetPlatform() string    // "linux/arm64" or "linux/amd64"
	GetExtractKernel() bool // true for both VZ and Firecracker
	GetNeedsInitrd() bool   // true for VZ, false for Firecracker
}

// ImageInfo describes an available image variant.
// Keep in sync with config.ImageInfo (field-by-field copy in backend wrappers).
type ImageInfo struct {
	Name      string
	Path      string
	DockerRef string
	SizeBytes int64
	Source    string // "config" or "discovered"
	Cached    bool
}

// Manager handles image lifecycle: ensure, list, delete, prune.
// All file locking happens inside the Manager.
type Manager struct {
	cfg ImageConfig
}

// NewManager creates a new image Manager with the given configuration.
func NewManager(cfg ImageConfig) *Manager {
	return &Manager{cfg: cfg}
}

// ProgressFunc is called to report progress during long-running operations.
type ProgressFunc func(stage, msg string)

// ResolvedRef describes an image to ensure: either a local path or a Docker ref to pull.
// Callers unpack config.ResolvedImage fields into this struct to avoid an import cycle.
type ResolvedRef struct {
	Path      string // set when the ext4 image already exists on disk
	DockerRef string // set when the image needs to be pulled and converted
	Name      string // variant name, used for caching
}

// EnsureImage ensures an image is available as a local ext4 file.
// If ref.Path is set, it returns that path directly.
// If ref.DockerRef is set, it pulls and converts to ext4, caching the result.
func (m *Manager) EnsureImage(ctx context.Context, ref ResolvedRef, progress ProgressFunc) (string, error) {
	if ref.Path != "" {
		return ref.Path, nil
	}

	if ref.DockerRef == "" {
		return "", fmt.Errorf("resolved image has neither path nor docker ref")
	}

	outputDir := m.cfg.GetImagesDir()
	if outputDir == "" {
		return "", fmt.Errorf("images_dir is not configured")
	}

	rootfsPath := filepath.Join(outputDir, RootfsFilename(ref.Name))

	// Acquire a flock-based lock to prevent concurrent conversions.
	// Unlike O_EXCL, flock is automatically released if the process crashes.
	lockPath := rootfsPath + ".lock"
	unlock, err := acquireFileLock(lockPath)
	if err != nil {
		return "", fmt.Errorf("failed to acquire lock for image conversion: %w", err)
	}
	defer unlock()

	// Re-check cache after acquiring lock (another process may have completed)
	if cached := CheckCache(outputDir, ref.Name, ref.DockerRef); cached != "" {
		return cached, nil
	}

	// Stale cache — remove for re-conversion
	os.Remove(rootfsPath)

	if progress != nil {
		progress("image", fmt.Sprintf("Pulling %s...", ref.DockerRef))
	}
	log.Printf("Converting Docker image %s to ext4 for variant %q", ref.DockerRef, ref.Name)

	result, err := Convert(ctx, ConvertOptions{
		DockerRef:     ref.DockerRef,
		Name:          ref.Name,
		OutputDir:     outputDir,
		ExtractKernel: m.cfg.GetExtractKernel(),
		NeedsInitrd:   m.cfg.GetNeedsInitrd(),
		Platform:      m.cfg.GetPlatform(),
	})
	if err != nil {
		return "", fmt.Errorf("failed to convert image %s: %w", ref.DockerRef, err)
	}

	if err := WriteSource(outputDir, ref.Name, ref.DockerRef); err != nil {
		log.Printf("Warning: failed to write source sidecar: %v", err)
	}

	return result.RootfsPath, nil
}

// ListImages returns available image variants from config and auto-discovery in ImagesDir.
func (m *Manager) ListImages() ([]ImageInfo, error) {
	seen := make(map[string]bool)
	var images []ImageInfo

	for name, val := range m.cfg.GetImages() {
		seen[name] = true
		info := ImageInfo{
			Name:   name,
			Source: "config",
		}
		if IsDockerRef(val) {
			info.DockerRef = val
			if cached := CheckCache(m.cfg.GetImagesDir(), name, val); cached != "" {
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

	imagesDir := m.cfg.GetImagesDir()
	if imagesDir != "" {
		entries, err := os.ReadDir(imagesDir)
		if err == nil {
			for _, e := range entries {
				if strings.HasSuffix(e.Name(), "-rootfs.ext4") && !e.IsDir() {
					name := strings.TrimSuffix(e.Name(), "-rootfs.ext4")
					if name == "" || name == "_base" || seen[name] {
						continue
					}
					info := ImageInfo{
						Name:   name,
						Path:   filepath.Join(imagesDir, e.Name()),
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

// DeleteImage removes a cached image by name.
// It deletes the ext4 rootfs and source sidecar but NOT the lock file.
// The inUseNames function returns image names currently referenced by existing sheds.
func (m *Manager) DeleteImage(name string, inUseNames func() ([]string, error)) error {
	if err := ValidateImageName(name); err != nil {
		return err
	}

	// Refuse if this image is in the config Images map
	if _, ok := m.cfg.GetImages()[name]; ok {
		return ErrImageInUse
	}

	// Refuse if this is _base and BaseRootfs is a Docker ref (it produced this cached image)
	if name == "_base" && IsDockerRef(m.cfg.GetBaseRootfs()) {
		return ErrImageInUse
	}

	// Refuse if any existing shed references this image — fail closed on errors
	if inUseNames != nil {
		names, err := inUseNames()
		if err != nil {
			return fmt.Errorf("listing in-use images: %w", err)
		}
		for _, n := range names {
			if n == name {
				return ErrImageInUse
			}
		}
	}

	imagesDir := m.cfg.GetImagesDir()
	if imagesDir == "" {
		return fmt.Errorf("images_dir is not configured")
	}

	rootfsPath := filepath.Join(imagesDir, RootfsFilename(name))

	// Acquire the same flock used by EnsureImage to prevent races with
	// concurrent image conversions.
	unlock, err := acquireFileLock(rootfsPath + ".lock")
	if err != nil {
		return fmt.Errorf("acquiring image lock: %w", err)
	}
	defer unlock()

	if err := os.Remove(rootfsPath); err != nil {
		if os.IsNotExist(err) {
			return ErrImageNotFound
		}
		return fmt.Errorf("removing rootfs: %w", err)
	}

	// Best-effort removal of source sidecar (NOT the lock file)
	os.Remove(filepath.Join(imagesDir, SourceFilename(name)))

	return nil
}

// PruneImages removes cached images not referenced by config or existing sheds.
// If dryRun is true, returns candidates without deleting.
// The inUseNames function returns image names currently referenced by existing sheds.
func (m *Manager) PruneImages(dryRun bool, inUseNames func() ([]string, error)) ([]ImageInfo, error) {
	imagesDir := m.cfg.GetImagesDir()
	if imagesDir == "" {
		return nil, nil
	}

	entries, err := os.ReadDir(imagesDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to read images directory: %w", err)
	}

	// Build exclusion set. For Docker-ref entries, exclusion is source-aware:
	// a cached variant or _base is protected only if its .source sidecar
	// matches the current config ref. After a config bump (e.g. v0.3.3 →
	// v0.3.4), the stale cache file no longer matches and becomes a prune
	// candidate. Local-path entries are unconditionally excluded — we never
	// delete a file the config explicitly points at.
	exclude := make(map[string]bool)

	// excludeLocalPath protects the on-disk file a local-path config entry
	// points at. If the path lives inside imagesDir and follows the
	// {name}-rootfs.ext4 convention, exclude that derived name — otherwise
	// the directory scan could match a candidate with a different name
	// than the config map key and delete a file the config depends on.
	excludeLocalPath := func(ref string) {
		if ref == "" {
			return
		}
		if filepath.Dir(ref) != imagesDir {
			return
		}
		base := filepath.Base(ref)
		if !strings.HasSuffix(base, "-rootfs.ext4") {
			return
		}
		derivedName := strings.TrimSuffix(base, "-rootfs.ext4")
		if derivedName != "" {
			exclude[derivedName] = true
		}
	}

	for name, ref := range m.cfg.GetImages() {
		if !IsDockerRef(ref) {
			// Legacy: protect the config map key. Also protect the
			// on-disk file the path actually points at (they may differ).
			exclude[name] = true
			excludeLocalPath(ref)
			continue
		}
		if cached := CheckCache(imagesDir, name, ref); cached != "" {
			exclude[name] = true
		}
	}

	if base := m.cfg.GetBaseRootfs(); base != "" {
		if IsDockerRef(base) {
			if cached := CheckCache(imagesDir, "_base", base); cached != "" {
				exclude["_base"] = true
			}
		} else {
			// Local-path base_rootfs: protect its on-disk file if it
			// lives in imagesDir (mirrors the images: branch above).
			excludeLocalPath(base)
		}
	}

	// Images referenced by existing sheds — fail closed if we can't read metadata
	if inUseNames != nil {
		names, err := inUseNames()
		if err != nil {
			return nil, fmt.Errorf("listing in-use images: %w", err)
		}
		for _, n := range names {
			if n != "" {
				exclude[n] = true
			}
		}
	}

	// Find candidates
	var candidates []ImageInfo
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), "-rootfs.ext4") || e.IsDir() {
			continue
		}
		name := strings.TrimSuffix(e.Name(), "-rootfs.ext4")
		if name == "" || exclude[name] {
			continue
		}

		info := ImageInfo{
			Name:   name,
			Path:   filepath.Join(imagesDir, e.Name()),
			Cached: true,
		}
		if fi, err := e.Info(); err == nil {
			info.SizeBytes = fi.Size()
		}
		// Read source sidecar for docker ref
		sourceFile := filepath.Join(imagesDir, SourceFilename(name))
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

	// Delete candidates — acquire flock to prevent races with EnsureImage
	var deleted []ImageInfo
	for _, img := range candidates {
		unlock, err := acquireFileLock(img.Path + ".lock")
		if err != nil {
			log.Printf("warning: failed to lock %s: %v", img.Path, err)
			continue
		}
		if err := os.Remove(img.Path); err != nil && !os.IsNotExist(err) {
			unlock()
			log.Printf("warning: failed to remove %s: %v", img.Path, err)
			continue
		}
		// Best-effort removal of source sidecar
		os.Remove(filepath.Join(imagesDir, SourceFilename(img.Name)))
		unlock()
		deleted = append(deleted, img)
	}

	return deleted, nil
}

// ValidateImageName validates that an image name is safe for filesystem operations.
func ValidateImageName(name string) error {
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
