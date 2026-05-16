// Package vmimage provides Docker-to-ext4 image conversion and image lifecycle
// management for VM backends. This package is cross-platform (no build tags)
// so it can be tested on Linux CI. Both VZ and Firecracker backends use ext4
// rootfs images and share this pipeline.
//
// Import constraint: config imports vmimage, so vmimage must NOT import config
// or backend. All external dependencies are injected via interfaces and closures.
//
// Storage model: images are content-addressed under {imagesDir}/blobs/sha256/
// with tag indirection under {imagesDir}/tags/. See blobstore.go for the
// disk layout details.
package vmimage

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"
)

// Sentinel errors for image operations. Backend wrappers map these to
// config sentinel errors (e.g., config.ErrImageNotFoundSentinel).
var (
	// ErrImageNotFound is returned when a tag does not exist.
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

// ImageInfo describes an image known to the blob store, addressed by tag.
// Keep in sync with config.ImageInfo (field-by-field copy in backend wrappers).
type ImageInfo struct {
	Name      string // tag name (or "<dangling>" for unreferenced blobs)
	Digest    string // "sha256:..." digest of the underlying blob
	Tag       string // tag name (same as Name for tagged images, empty for dangling)
	Path      string // path to blob's rootfs.ext4
	DockerRef string // manifest.SourceRef
	SizeBytes int64  // logical size of rootfs.ext4
	Source    string // "config", "discovered", or "dangling"
	Cached    bool   // blob is installed
	InUse     bool   // protected by a shed or snapshot reference
}

// Manager handles image lifecycle: ensure, list, delete, prune.
// All file locking happens inside the Manager.
type Manager struct {
	cfg     ImageConfig
	scanner RefScanner // optional; nil disables shed/snapshot ref protection
}

// NewManager creates a new image Manager with the given configuration.
//
// scanner may be nil for code paths that only operate on tags + blobs
// (e.g. `shed image build` running locally on a developer machine);
// callers that perform Delete/Prune from a server with live sheds MUST
// supply a scanner so refcount protection applies.
func NewManager(cfg ImageConfig, scanner RefScanner) *Manager {
	return &Manager{cfg: cfg, scanner: scanner}
}

// ProgressFunc is called to report progress during long-running operations.
type ProgressFunc func(stage, msg string)

// ResolvedRef describes an image to ensure: either a local path or a Docker ref to pull.
// Callers unpack config.ResolvedImage fields into this struct to avoid an import cycle.
type ResolvedRef struct {
	Path      string // set when the ext4 image already exists on disk
	DockerRef string // set when the image needs to be pulled and converted
	Name      string // variant name, used as a tag in the blob store
	Digest    string // set when Path came from a tag in the blob store; preserved through to EnsureResult.Digest
}

// EnsureResult is what EnsureImage returns: the path to the cached
// rootfs.ext4 (always inside the blob store, except when ref.Path was
// passed) and the digest pinning that blob.
type EnsureResult struct {
	Path   string // path to rootfs.ext4 on disk
	Digest string // "sha256:..." (empty when ref.Path was a local file)
}

// EnsureImage ensures an image is available as a local ext4 file.
//
//   - If ref.Path is set, returns it directly. ref.Digest carries the
//     blob-store digest forward when the path was discovered via tag
//     resolution; a path without a digest is the legacy local-path
//     escape hatch and returns digest-less.
//   - Else: looks up the tag named ref.Name. If cached and the blob's
//     manifest.SourceRef matches ref.DockerRef, returns the cached blob.
//     Otherwise pulls + converts ref.DockerRef, installs into the blob
//     store, and advances the tag.
func (m *Manager) EnsureImage(ctx context.Context, ref ResolvedRef, progress ProgressFunc) (EnsureResult, error) {
	if ref.Path != "" {
		return EnsureResult{Path: ref.Path, Digest: ref.Digest}, nil
	}

	if ref.DockerRef == "" {
		return EnsureResult{}, fmt.Errorf("resolved image has neither path nor docker ref")
	}
	if err := ValidateImageName(ref.Name); err != nil {
		return EnsureResult{}, err
	}

	imagesDir := m.cfg.GetImagesDir()
	if imagesDir == "" {
		return EnsureResult{}, fmt.Errorf("images_dir is not configured")
	}

	// Cache hit fast path: tag points at a blob whose manifest matches
	// the requested DockerRef.
	if cached := Resolve(imagesDir, ref.Name, ref.DockerRef); cached != "" {
		t, err := GetTag(imagesDir, ref.Name)
		if err == nil {
			return EnsureResult{Path: cached, Digest: t.Digest}, nil
		}
	}

	// Serialize concurrent EnsureImage calls for the same tag — two
	// callers should pull/convert once, not twice. Lock by tag name
	// since digest isn't known yet.
	tagLockPath := filepath.Join(imagesDir, tagsDir, ref.Name+".lock")
	unlock, err := acquireFileLock(tagLockPath)
	if err != nil {
		return EnsureResult{}, fmt.Errorf("acquiring tag lock: %w", err)
	}
	defer unlock()

	// Re-check cache after acquiring the lock.
	if cached := Resolve(imagesDir, ref.Name, ref.DockerRef); cached != "" {
		t, err := GetTag(imagesDir, ref.Name)
		if err == nil {
			return EnsureResult{Path: cached, Digest: t.Digest}, nil
		}
	}

	if progress != nil {
		progress("image", fmt.Sprintf("Pulling %s...", ref.DockerRef))
	}
	log.Printf("Converting Docker image %s to blob (tag %q)", ref.DockerRef, ref.Name)

	digest, err := m.convertAndInstall(ctx, ref.DockerRef, ref.Name, progress)
	if err != nil {
		return EnsureResult{}, err
	}

	if err := SetTag(imagesDir, ref.Name, digest); err != nil {
		return EnsureResult{}, fmt.Errorf("advancing tag %q: %w", ref.Name, err)
	}

	rootfs, err := BlobRootfsPath(imagesDir, digest)
	if err != nil {
		return EnsureResult{}, err
	}
	return EnsureResult{Path: rootfs, Digest: digest}, nil
}

// PullImage pulls a Docker reference, converts it, installs into the blob
// store, and advances the named tag to the new digest. Returns the digest.
//
// Used by `shed image pull` and `shed image build --from` (Dockerfile-less
// path). Identical to EnsureImage except it does not check the cache —
// always re-converts.
func (m *Manager) PullImage(ctx context.Context, dockerRef, tag string, progress ProgressFunc) (string, error) {
	if err := ValidateImageName(tag); err != nil {
		return "", err
	}
	imagesDir := m.cfg.GetImagesDir()
	if imagesDir == "" {
		return "", fmt.Errorf("images_dir is not configured")
	}

	tagLockPath := filepath.Join(imagesDir, tagsDir, tag+".lock")
	unlock, err := acquireFileLock(tagLockPath)
	if err != nil {
		return "", fmt.Errorf("acquiring tag lock: %w", err)
	}
	defer unlock()

	digest, err := m.convertAndInstall(ctx, dockerRef, tag, progress)
	if err != nil {
		return "", err
	}
	if err := SetTag(imagesDir, tag, digest); err != nil {
		return "", fmt.Errorf("advancing tag %q: %w", tag, err)
	}
	return digest, nil
}

// convertAndInstall runs a Docker-to-ext4 conversion and installs the
// result into the blob store. Returns the digest. The tag is NOT
// advanced — caller is responsible.
func (m *Manager) convertAndInstall(ctx context.Context, dockerRef, name string, progress ProgressFunc) (string, error) {
	imagesDir := m.cfg.GetImagesDir()
	if err := os.MkdirAll(imagesDir, 0o755); err != nil {
		return "", fmt.Errorf("creating images dir: %w", err)
	}

	result, err := Convert(ctx, ConvertOptions{
		DockerRef:     dockerRef,
		Name:          name,
		OutputDir:     imagesDir,
		Platform:      m.cfg.GetPlatform(),
		ExtractKernel: m.cfg.GetExtractKernel(),
		NeedsInitrd:   m.cfg.GetNeedsInitrd(),
	})
	if err != nil {
		return "", fmt.Errorf("converting %s: %w", dockerRef, err)
	}
	defer CleanupConvert(result)

	rootfsLogical, _ := fileSize(result.RootfsPath)
	manifest := Manifest{
		SchemaVersion:     ManifestSchemaVersion,
		Digest:            result.Digest,
		SourceRef:         dockerRef,
		RootfsLogicalSize: rootfsLogical,
		CreatedAt:         time.Now().UTC(),
	}

	files := map[string]string{BlobRootfsFilename: result.RootfsPath}
	if result.KernelPath != "" {
		files[BlobKernelFilename] = result.KernelPath
		if sz, _ := fileSize(result.KernelPath); sz > 0 {
			manifest.KernelSize = sz
		}
	}
	if result.InitrdPath != "" {
		files[BlobInitrdFilename] = result.InitrdPath
		if sz, _ := fileSize(result.InitrdPath); sz > 0 {
			manifest.InitrdSize = sz
		}
	}

	if progress != nil {
		progress("image", fmt.Sprintf("Installing blob %s...", ShortDigest(result.Digest)))
	}

	if _, _, err := InstallBlob(imagesDir, BlobInstallSpec{Files: files, Manifest: manifest}); err != nil {
		return "", fmt.Errorf("installing blob: %w", err)
	}
	return result.Digest, nil
}

// ListImages returns ImageInfo entries for every known tag plus dangling
// blobs (installed but not referenced by any tag). Config-managed tags
// take precedence in the Source column.
func (m *Manager) ListImages() ([]ImageInfo, error) {
	imagesDir := m.cfg.GetImagesDir()
	if imagesDir == "" {
		return nil, nil
	}

	configMap := m.cfg.GetImages()
	if configMap == nil {
		configMap = map[string]string{}
	}

	tags, err := ListTags(imagesDir)
	if err != nil {
		return nil, err
	}

	// Collect protective refs once for the InUse column.
	var refs []Reference
	if m.scanner != nil {
		refs, err = m.scanner.ScanRefs()
		if err != nil {
			return nil, fmt.Errorf("scanning refs: %w", err)
		}
	}

	taggedDigests := make(map[string]bool)
	var out []ImageInfo
	for _, tag := range tags {
		t, err := GetTag(imagesDir, tag)
		if err != nil {
			log.Printf("Warning: skipping tag %q: %v", tag, err)
			continue
		}
		taggedDigests[t.Digest] = true
		info := ImageInfo{
			Name:   tag,
			Tag:    tag,
			Digest: t.Digest,
			Cached: BlobExists(imagesDir, t.Digest),
		}
		if dockerRef, ok := configMap[tag]; ok && IsDockerRef(dockerRef) {
			info.Source = "config"
			info.DockerRef = dockerRef
		} else {
			info.Source = "discovered"
		}
		if info.Cached {
			if path, _ := BlobRootfsPath(imagesDir, t.Digest); path != "" {
				info.Path = path
				info.SizeBytes, _ = fileSize(path)
			}
			if manifest, err := LoadManifest(imagesDir, t.Digest); err == nil {
				if info.DockerRef == "" {
					info.DockerRef = manifest.SourceRef
				}
			}
			if len(ProtectiveRefs(refs, t.Digest)) > 0 {
				info.InUse = true
			}
		}
		out = append(out, info)
	}

	// Dangling blobs: installed but no tag points at them.
	digests, err := ListBlobs(imagesDir)
	if err != nil {
		return nil, err
	}
	for _, d := range digests {
		if taggedDigests[d] {
			continue
		}
		info := ImageInfo{
			Name:   ShortDigest(d),
			Digest: d,
			Source: "dangling",
			Cached: true,
		}
		if path, _ := BlobRootfsPath(imagesDir, d); path != "" {
			info.Path = path
			info.SizeBytes, _ = fileSize(path)
		}
		if manifest, err := LoadManifest(imagesDir, d); err == nil {
			info.DockerRef = manifest.SourceRef
		}
		if len(ProtectiveRefs(refs, d)) > 0 {
			info.InUse = true
		}
		out = append(out, info)
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Source != out[j].Source {
			return out[i].Source < out[j].Source
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// InspectImage returns full details for a tag or digest. The argument may
// be either a tag name (e.g. "experimental"), a full digest ("sha256:..."),
// or a digest prefix accepted by ResolveDigestPrefix.
func (m *Manager) InspectImage(tagOrDigest string) (*ImageInfo, *Manifest, error) {
	imagesDir := m.cfg.GetImagesDir()
	digest, tagName, err := m.resolveTagOrDigest(tagOrDigest)
	if err != nil {
		return nil, nil, err
	}

	manifest, err := LoadManifest(imagesDir, digest)
	if err != nil {
		return nil, nil, err
	}

	info := &ImageInfo{
		Digest:    digest,
		Tag:       tagName,
		Cached:    BlobExists(imagesDir, digest),
		DockerRef: manifest.SourceRef,
		Source:    "discovered",
	}
	if tagName != "" {
		info.Name = tagName
		if dockerRef, ok := m.cfg.GetImages()[tagName]; ok && IsDockerRef(dockerRef) {
			info.Source = "config"
		}
	} else {
		info.Name = ShortDigest(digest)
		info.Source = "dangling"
	}
	if path, _ := BlobRootfsPath(imagesDir, digest); path != "" {
		info.Path = path
		info.SizeBytes, _ = fileSize(path)
	}
	if m.scanner != nil {
		refs, err := m.scanner.ScanRefs()
		if err == nil && len(ProtectiveRefs(refs, digest)) > 0 {
			info.InUse = true
		}
	}
	return info, manifest, nil
}

// TagImage points a new tag at the digest currently held by srcTagOrDigest.
// Equivalent to `docker tag`. Overwrites newTag if it already exists.
func (m *Manager) TagImage(srcTagOrDigest, newTag string) error {
	imagesDir := m.cfg.GetImagesDir()
	if err := ValidateImageName(newTag); err != nil {
		return err
	}
	digest, _, err := m.resolveTagOrDigest(srcTagOrDigest)
	if err != nil {
		return err
	}
	if !BlobExists(imagesDir, digest) {
		return fmt.Errorf("%w: %s", ErrBlobNotFound, digest)
	}
	return SetTag(imagesDir, newTag, digest)
}

// resolveTagOrDigest accepts either a tag name or a digest (full or short
// prefix) and returns (digest, tagName). tagName is "" for digest inputs.
func (m *Manager) resolveTagOrDigest(s string) (digest, tagName string, err error) {
	imagesDir := m.cfg.GetImagesDir()
	// Looks like a digest? Try prefix match against installed blobs.
	if len(s) >= len(DigestPrefix) && s[:len(DigestPrefix)] == DigestPrefix {
		full, err := resolveDigestPrefix(imagesDir, s)
		if err != nil {
			return "", "", err
		}
		return full, "", nil
	}
	t, err := GetTag(imagesDir, s)
	if err != nil {
		if errors.Is(err, ErrTagNotFound) {
			return "", "", fmt.Errorf("%w: %s", ErrImageNotFound, s)
		}
		return "", "", err
	}
	return t.Digest, s, nil
}

// resolveDigestPrefix matches a "sha256:<hex...>" string (full or
// truncated) against installed blobs. Returns ErrBlobNotFound if no
// match, or an error if more than one blob matches.
func resolveDigestPrefix(imagesDir, prefix string) (string, error) {
	if len(prefix) <= len(DigestPrefix) {
		return "", fmt.Errorf("%w: empty digest", ErrInvalidDigest)
	}
	digests, err := ListBlobs(imagesDir)
	if err != nil {
		return "", err
	}
	var matches []string
	for _, d := range digests {
		if len(d) >= len(prefix) && d[:len(prefix)] == prefix {
			matches = append(matches, d)
		}
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("%w: %s", ErrBlobNotFound, prefix)
	case 1:
		return matches[0], nil
	default:
		return "", fmt.Errorf("ambiguous digest prefix %q matches %d blobs", prefix, len(matches))
	}
}

// DeleteImage removes a tag. Following the Docker model, the underlying
// blob is NOT removed — call PruneImages to garbage-collect blobs that
// are no longer protected by any shed/snapshot/tag.
//
// Refuses to remove tags listed in the config.Images map (those are the
// system-managed tag set and would break ResolveImage). Returns
// ErrImageNotFound if the tag does not exist.
func (m *Manager) DeleteImage(name string) error {
	if err := ValidateImageName(name); err != nil {
		return err
	}
	imagesDir := m.cfg.GetImagesDir()

	// Refuse if this image is in the config Images map.
	if _, ok := m.cfg.GetImages()[name]; ok {
		return ErrImageInUse
	}

	// Refuse if this is _base and BaseRootfs is a Docker ref.
	if name == "_base" && IsDockerRef(m.cfg.GetBaseRootfs()) {
		return ErrImageInUse
	}

	if err := DeleteTag(imagesDir, name); err != nil {
		if errors.Is(err, ErrTagNotFound) {
			return ErrImageNotFound
		}
		return err
	}
	return nil
}

// PruneImages removes blobs that have no protective references (no shed,
// no snapshot pinning the digest). If dryRun is true, returns candidates
// without deleting.
//
// Following the Docker model, untagged blobs that are still referenced
// by a shed or snapshot are kept; tags do NOT protect.
//
// In addition to blob GC, prune drops dangling tag files whose digest is
// no longer present in the store (a previous prune removed the blob).
func (m *Manager) PruneImages(dryRun bool) ([]ImageInfo, error) {
	imagesDir := m.cfg.GetImagesDir()
	if imagesDir == "" {
		return nil, nil
	}

	digests, err := ListBlobs(imagesDir)
	if err != nil {
		return nil, err
	}

	var refs []Reference
	if m.scanner != nil {
		refs, err = m.scanner.ScanRefs()
		if err != nil {
			return nil, fmt.Errorf("scanning refs: %w", err)
		}
	}

	tagMap, err := tagDigestMap(imagesDir)
	if err != nil {
		return nil, err
	}

	var candidates []ImageInfo
	for _, d := range digests {
		if len(ProtectiveRefs(refs, d)) > 0 {
			continue
		}
		info := ImageInfo{
			Digest: d,
			Source: "dangling",
			Cached: true,
		}
		if tagName, ok := tagMap[d]; ok {
			info.Tag = tagName
			info.Name = tagName
			info.Source = "discovered"
		} else {
			info.Name = ShortDigest(d)
		}
		if path, _ := BlobRootfsPath(imagesDir, d); path != "" {
			info.Path = path
			info.SizeBytes, _ = fileSize(path)
		}
		if manifest, err := LoadManifest(imagesDir, d); err == nil {
			info.DockerRef = manifest.SourceRef
		}
		candidates = append(candidates, info)
	}

	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].Name < candidates[j].Name
	})

	if dryRun {
		return candidates, nil
	}

	var deleted []ImageInfo
	for _, c := range candidates {
		if err := DeleteBlob(imagesDir, c.Digest); err != nil {
			log.Printf("warning: failed to remove blob %s: %v", c.Digest, err)
			continue
		}
		// Also remove any tag pointing at the now-deleted digest.
		if c.Tag != "" {
			_ = DeleteTag(imagesDir, c.Tag)
		}
		deleted = append(deleted, c)
	}
	return deleted, nil
}

// tagDigestMap returns digest -> tag name for the first tag found
// pointing at each digest. Used by PruneImages to label deleted blobs.
func tagDigestMap(imagesDir string) (map[string]string, error) {
	tags, err := ListTags(imagesDir)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(tags))
	for _, name := range tags {
		t, err := GetTag(imagesDir, name)
		if err != nil {
			continue
		}
		if _, ok := out[t.Digest]; !ok {
			out[t.Digest] = name
		}
	}
	return out, nil
}

func fileSize(path string) (int64, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return fi.Size(), nil
}

// ValidateImageName validates that an image name is safe for filesystem operations.
func ValidateImageName(name string) error {
	if name == "" {
		return fmt.Errorf("image name cannot be empty")
	}
	if name == "." || name == ".." {
		return fmt.Errorf("invalid image name: %q", name)
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '-' || r == '_' || r == '.':
		default:
			return fmt.Errorf("invalid image name: %q (only alphanumerics, '-', '_', '.' allowed)", name)
		}
	}
	return nil
}

// acquireFileLock acquires an exclusive flock on the given path.
// The lock is automatically released if the process exits or crashes.
func acquireFileLock(path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
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

// TryAcquireFileLockBlocking takes an exclusive flock on path and blocks
// until it's available. Use this from tests that need to simulate a
// live conversion holding the lock.
func TryAcquireFileLockBlocking(path string) (func(), error) {
	return acquireFileLock(path)
}

// TryAcquireFileLock attempts a non-blocking exclusive flock on path.
// See TryAcquireFileLock comment in earlier revisions for the contract.
// Returns held=true with no-op release if the lock file does not exist.
func TryAcquireFileLock(path string) (release func(), held bool, err error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return func() {}, true, nil
		}
		return nil, false, err
	}

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("failed to acquire lock: %w", err)
	}

	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN) //nolint:errcheck
		f.Close()
	}, true, nil
}
