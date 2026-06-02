// Package vmimage provides Docker-to-OCI image conversion and image lifecycle
// management for VM backends. The on-disk store is OCI image-layout-v1
// compliant (see ocilayout.go); shed-specific tag indirection lives in
// {imagesDir}/tags/ as a sibling of the OCI blobs.
//
// Import constraint: config imports vmimage, so vmimage must NOT import
// config or backend. All external dependencies are injected via interfaces
// and closures.

package vmimage

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
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
	GetDefaultImage() string            // ref (or path) for new sheds when no --image
	GetImageAliases() map[string]string // alias -> ref convenience map
	GetPullPolicy() string              // "missing" (default) | "always" | "never"
	GetImagesDir() string
	GetPlatform() string    // "linux/arm64" or "linux/amd64"
	GetExtractKernel() bool // true for both VZ and Firecracker
	GetNeedsInitrd() bool   // true for VZ, false for Firecracker
}

// ImageInfo describes an image known to the blob store, addressed by tag.
// Keep in sync with config.ImageInfo (field-by-field copy in backend wrappers).
type ImageInfo struct {
	Name        string // tag name (or "<dangling>" for unreferenced blobs)
	Digest      string // "sha256:..." digest of the underlying OCI manifest
	Tag         string // tag name (same as Name for tagged images, empty for dangling)
	Path        string // path to the first layer's cached ext4 (single-layer compat)
	DockerRef   string // OCI manifest annotation io.shed.source-ref
	SizeBytes   int64  // sum of layer descriptor sizes + cached ext4 bytes
	UniqueBytes int64  // bytes attributable to layers only this manifest references
	SharedBytes int64  // bytes attributable to layers also referenced by other manifests
	Source      string // "config", "discovered", or "dangling"
	Cached      bool   // manifest blob is installed
	InUse       bool   // protected by a shed or snapshot reference
}

// Manager handles image lifecycle: ensure, list, delete, prune.
// All file locking happens inside the Manager.
type Manager struct {
	cfg     ImageConfig
	scanner RefScanner // optional; nil disables shed/snapshot ref protection
}

// NewManager creates a new image Manager with the given configuration.
func NewManager(cfg ImageConfig, scanner RefScanner) *Manager {
	return &Manager{cfg: cfg, scanner: scanner}
}

// ProgressFunc is called to report progress during long-running operations.
type ProgressFunc func(stage, msg string)

// ResolvedRef describes an image to ensure: either a local path or a Docker ref to pull.
type ResolvedRef struct {
	Path      string // set when the ext4 image already exists on disk
	DockerRef string // set when the image needs to be pulled and converted
	Name      string // cosmetic label derived from the ref; not an identity key
	Digest    string // set when Path came from a tag in the blob store; preserved through to EnsureResult.Digest

	// Policy governs cache-vs-pull for a DockerRef. Empty means PullMissing.
	// Ignored for local-path refs.
	Policy PullPolicy
}

// EnsureResult is what EnsureImage returns: the path to the
// content-addressed lower image for the resolved manifest, and the
// manifest digest itself.
type EnsureResult struct {
	Path   string // path to the cached lower image (manifest-digest-keyed erofs)
	Digest string // OCI manifest digest
}

// EnsureImage ensures an image is available locally. For Docker refs,
// pulls + converts + writes OCI blobs + materializes ext4 cache, then
// advances the tag. For local-path refs, returns the path directly
// (legacy escape hatch).
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
	if err := EnsureOCILayout(imagesDir); err != nil {
		return EnsureResult{}, err
	}

	policy := ref.Policy
	if policy == "" {
		policy = PullMissing
	}

	// Cache lookup by ref identity (the io.shed.source-ref annotation),
	// resolved O(1) through the ref-index. PullAlways bypasses the cache
	// entirely so a stale tag/index can't short-circuit the pull (F1).
	if policy != PullAlways {
		if res, ok := m.resolveCachedRef(ctx, imagesDir, ref.DockerRef); ok {
			return res, nil
		}
	}

	// Serialize concurrent EnsureImage calls for the SAME ref (keyed by a
	// hash of the full ref, so distinct refs never block each other).
	refLockPath := filepath.Join(imagesDir, tagsDir, refIndexKey(ref.DockerRef)+".lock")
	unlock, err := acquireFileLock(refLockPath)
	if err != nil {
		return EnsureResult{}, fmt.Errorf("acquiring image lock: %w", err)
	}
	defer unlock()

	// Re-check under lock (another caller may have just pulled this ref).
	if policy != PullAlways {
		if res, ok := m.resolveCachedRef(ctx, imagesDir, ref.DockerRef); ok {
			return res, nil
		}
	}

	// Cache miss. PullNever must not contact the registry (F2).
	if policy == PullNever {
		return EnsureResult{}, fmt.Errorf("%w: %s", ErrPullDisabled, ref.DockerRef)
	}

	if progress != nil {
		progress("image", fmt.Sprintf("Pulling %s...", ref.DockerRef))
	}

	// Registry-direct pull only — v0.5.2 dropped the legacy
	// docker-daemon fallback. The fallback flattened to a single
	// layer and extracted Ubuntu's stock /boot/initrd.img (which
	// doesn't understand shed.upper / shed.lowers), so on the rare
	// occasions it was actually exercised the resulting shed
	// panicked at boot. The remaining "registry-only" failure path
	// surfaces the underlying pull error verbatim — no silent
	// fallback that turns a network blip into a confusing boot
	// failure ten minutes later.
	platform := m.cfg.GetPlatform()
	pullResult, err := PullToOCILayout(ctx, PullOptions{
		Ref:           ref.DockerRef,
		ImagesDir:     imagesDir,
		TagName:       ref.Name,
		Platform:      platform,
		Insecure:      isLoopbackRef(ref.DockerRef),
		ExtractKernel: m.cfg.GetExtractKernel(),
		NeedsInitrd:   m.cfg.GetNeedsInitrd(),
		Progress:      progress,
	})
	if err != nil {
		return EnsureResult{}, fmt.Errorf("pulling %s from registry: %w", ref.DockerRef, err)
	}
	res, err := m.resolveManifestLower(ctx, imagesDir, pullResult.ManifestDigest)
	if err != nil {
		return EnsureResult{}, err
	}
	// Final commit: record ref -> digest only after the manifest + all
	// blobs (verified by resolveManifestLower) and the OCI index are
	// durable, so the ref-index can never point at an incomplete pull.
	if err := RefIndexPut(imagesDir, ref.DockerRef, pullResult.ManifestDigest); err != nil {
		log.Printf("vmimage: failed to record ref-index entry for %s: %v", ref.DockerRef, err)
	}
	return res, nil
}

// resolveCachedRef is the by-ref validated cache lookup used on the create
// hot path. It consults the O(1) ref-index first and falls back to a
// one-time index rebuild (scan) when the entry is missing — e.g. images
// pulled before the ref-index existed — so a cold index self-heals.
func (m *Manager) resolveCachedRef(ctx context.Context, imagesDir, ref string) (EnsureResult, bool) {
	digest, ok := ResolveRefDigest(imagesDir, ref)
	if !ok {
		return EnsureResult{}, false
	}
	res, err := m.resolveManifestLower(ctx, imagesDir, digest)
	if err != nil {
		// Manifest present but a required blob (erofs) is gone. Drop the
		// stale entry so policy (pull/never) governs the repair.
		RefIndexDeleteByDigest(imagesDir, digest)
		return EnsureResult{}, false
	}
	return res, true
}

// PullImage pulls a registry reference straight to the OCI layout (no
// Docker daemon required) and advances the named tag. Defaults to the
// backend's native platform when platform is empty.
func (m *Manager) PullImage(ctx context.Context, dockerRef, tag, platform string, progress ProgressFunc) (string, error) {
	if err := ValidateImageName(tag); err != nil {
		return "", err
	}
	imagesDir := m.cfg.GetImagesDir()
	if imagesDir == "" {
		return "", fmt.Errorf("images_dir is not configured")
	}
	if err := EnsureOCILayout(imagesDir); err != nil {
		return "", err
	}

	tagLockPath := filepath.Join(imagesDir, tagsDir, tag+".lock")
	unlock, err := acquireFileLock(tagLockPath)
	if err != nil {
		return "", fmt.Errorf("acquiring tag lock: %w", err)
	}
	defer unlock()

	if platform == "" {
		platform = m.cfg.GetPlatform()
	}

	result, err := PullToOCILayout(ctx, PullOptions{
		Ref:           dockerRef,
		ImagesDir:     imagesDir,
		TagName:       tag,
		Platform:      platform,
		Insecure:      isLoopbackRef(dockerRef),
		ExtractKernel: m.cfg.GetExtractKernel(),
		NeedsInitrd:   m.cfg.GetNeedsInitrd(),
		Progress:      progress,
	})
	if err != nil {
		return "", fmt.Errorf("pulling %s from registry: %w", dockerRef, err)
	}
	// Record the ref->digest mapping so `shed create` (and prune protection)
	// can resolve this ref O(1) without a manifest scan, and so a configured
	// default_image pulled via `shed image pull` is protected from prune.
	if err := RefIndexPut(imagesDir, dockerRef, result.ManifestDigest); err != nil {
		log.Printf("vmimage: failed to record ref-index entry for %s: %v", dockerRef, err)
	}
	return result.ManifestDigest, nil
}

// PushImage uploads the manifest currently held by tagOrDigest to a
// destination registry ref. Byte-perfect: the on-disk tar.gz layer
// blobs are streamed straight from the OCI store.
func (m *Manager) PushImage(ctx context.Context, tagOrDigest, dstRef string, progress ProgressFunc) error {
	imagesDir := m.cfg.GetImagesDir()
	if imagesDir == "" {
		return fmt.Errorf("images_dir is not configured")
	}
	digest, _, err := m.resolveTagOrDigest(tagOrDigest)
	if err != nil {
		return err
	}
	return PushFromOCILayout(ctx, PushOptions{
		Ref:            dstRef,
		ImagesDir:      imagesDir,
		ManifestDigest: digest,
		Progress:       progress,
	})
}

// isLoopbackRef returns true for registry refs whose registry host is
// loopback. Parses the host out of the ref first so refs like
// `localhost.example.com/repo` are NOT misclassified — only the actual
// localhost / 127.0.0.0/8 / ::1 hosts get the Insecure name.Option.
func isLoopbackRef(ref string) bool {
	host, _, _ := strings.Cut(ref, "/")
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if host == "localhost" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// ResolveManifestLower returns the path of the single content-addressed
// lower-image (flattened, all-layers-merged erofs) for the manifest,
// materializing it from the layer blobs if it isn't already cached.
// Used by VM start to attach one read-only block device.
func (m *Manager) ResolveManifestLower(ctx context.Context, manifestDigest string) (string, error) {
	imagesDir := m.cfg.GetImagesDir()
	if imagesDir == "" {
		return "", fmt.Errorf("images_dir is not configured")
	}
	res, err := m.resolveManifestLower(ctx, imagesDir, manifestDigest)
	if err != nil {
		return "", err
	}
	return res.Path, nil
}

// ResolveImageBlobs returns the manifest and config for an installed
// image, plus its kernel/initrd blob paths if the manifest advertises
// them via shed annotations.
func (m *Manager) ResolveImageBlobs(manifestDigest string) (manifest *OCIManifest, kernelPath, initrdPath string, err error) {
	imagesDir := m.cfg.GetImagesDir()
	manifest, err = LoadManifestByDigest(imagesDir, manifestDigest)
	if err != nil {
		return nil, "", "", err
	}
	if d := manifest.ShedKernelDigest(); d != "" {
		p, perr := BlobPath(imagesDir, d)
		if perr != nil {
			return nil, "", "", perr
		}
		kernelPath = p
	}
	if d := manifest.ShedInitrdDigest(); d != "" {
		p, perr := BlobPath(imagesDir, d)
		if perr != nil {
			return nil, "", "", perr
		}
		initrdPath = p
	}
	return manifest, kernelPath, initrdPath, nil
}

// resolveManifestLower resolves the single read-only lower image for a
// manifest. v0.5.2+ images carry a prebuilt erofs blob referenced by
// the io.shed.rootfs.erofs.digest annotation — we return that blob's
// path directly, no local mkfs.erofs invocation, no cache file. The
// blob path IS the lower the VM mounts.
//
// Images built with v0.5.1 or earlier tooling lack the annotation;
// since shed-build-tools published a known-good mkfs.erofs starting
// with v0.5.2, the upgrade path is to re-pull rather than fall back
// to the host's mkfs.erofs (which has writer-bug exposure on
// erofs-utils 1.7.x). The error surfaces the precise command needed.
func (m *Manager) resolveManifestLower(_ context.Context, imagesDir, manifestDigest string) (EnsureResult, error) {
	manifest, err := LoadManifestByDigest(imagesDir, manifestDigest)
	if err != nil {
		return EnsureResult{}, fmt.Errorf("loading manifest %s: %w", ShortDigest(manifestDigest), err)
	}
	erofsDigest := manifest.ShedRootfsErofsDigest()
	if erofsDigest == "" {
		return EnsureResult{}, fmt.Errorf(
			"image manifest %s lacks %s annotation (built with pre-v0.5.2 tooling); "+
				"re-pull against current images: shed image rm %s && shed-server pull-images "+
				"(see docs/upgrades/v0.5.1-to-v0.5.2.md for the full upgrade path)",
			ShortDigest(manifestDigest), AnnotationRootfsErofsDigest, ShortDigest(manifestDigest),
		)
	}
	if !BlobExists(imagesDir, erofsDigest) {
		return EnsureResult{}, fmt.Errorf(
			"rootfs erofs blob %s referenced by manifest %s is missing from the local store; "+
				"re-pull the image (shed image pull <ref> -t <name>) to recover",
			ShortDigest(erofsDigest), ShortDigest(manifestDigest),
		)
	}
	path, err := BlobPath(imagesDir, erofsDigest)
	if err != nil {
		return EnsureResult{}, fmt.Errorf("resolving rootfs erofs blob path: %w", err)
	}
	return EnsureResult{
		Path:   path,
		Digest: manifestDigest,
	}, nil
}

// ListImages returns ImageInfo entries for every known tag plus dangling
// blobs (installed manifests not referenced by any tag). UniqueBytes and
// SharedBytes are computed by attributing each layer's on-disk cost to
// its referencing manifests.
func (m *Manager) ListImages() ([]ImageInfo, error) {
	imagesDir := m.cfg.GetImagesDir()
	if imagesDir == "" {
		return nil, nil
	}

	// Refs the server config points at (default_image + alias values).
	// A manifest whose source-ref is in this set is sourced from config;
	// anything else with a source-ref was pulled ad-hoc by the user.
	configuredRefs := map[string]bool{}
	if dr := m.cfg.GetDefaultImage(); IsDockerRef(dr) {
		configuredRefs[dr] = true
	}
	for _, v := range m.cfg.GetImageAliases() {
		if IsDockerRef(v) {
			configuredRefs[v] = true
		}
	}

	tags, err := ListTags(imagesDir)
	if err != nil {
		return nil, err
	}

	var refs []Reference
	if m.scanner != nil {
		refs, err = m.scanner.ScanRefs(false)
		if err != nil {
			return nil, fmt.Errorf("scanning refs: %w", err)
		}
	}

	// Build the manifest set: tag-mapped + dangling.
	type manifestInfo struct {
		digest   string
		tag      string
		manifest *OCIManifest
	}
	var manifests []manifestInfo
	taggedDigests := make(map[string]bool)
	for _, tag := range tags {
		t, err := GetTag(imagesDir, tag)
		if err != nil {
			log.Printf("Warning: skipping tag %q: %v", tag, err)
			continue
		}
		taggedDigests[t.Digest] = true
		if !BlobExists(imagesDir, t.Digest) {
			manifests = append(manifests, manifestInfo{digest: t.Digest, tag: tag})
			continue
		}
		m, err := LoadManifestByDigest(imagesDir, t.Digest)
		if err != nil {
			if errors.Is(err, ErrLegacyBundledBlob) {
				log.Printf("Warning: tag %q references legacy bundled blob: %v", tag, err)
			} else {
				log.Printf("Warning: tag %q points at unreadable manifest %s: %v", tag, t.Digest, err)
			}
			manifests = append(manifests, manifestInfo{digest: t.Digest, tag: tag})
			continue
		}
		manifests = append(manifests, manifestInfo{digest: t.Digest, tag: tag, manifest: m})
	}

	// Find dangling manifests by consulting index.json — which lists
	// every installed manifest by digest. This avoids the older O(N)
	// probe-read of every blob (a layer tar.gz can be GBs and would
	// otherwise be loaded into memory just to fail ParseManifest).
	indexed, err := IndexManifestDigests(imagesDir)
	if err != nil {
		return nil, fmt.Errorf("reading OCI index: %w", err)
	}
	for digest := range indexed {
		if taggedDigests[digest] {
			continue
		}
		if !BlobExists(imagesDir, digest) {
			continue
		}
		mf, err := LoadManifestByDigest(imagesDir, digest)
		if err != nil {
			if errors.Is(err, ErrLegacyBundledBlob) {
				log.Printf("Warning: index entry %s references legacy bundled blob: %v", digest, err)
			} else {
				log.Printf("Warning: index entry %s unreadable: %v", digest, err)
			}
			continue
		}
		manifests = append(manifests, manifestInfo{digest: digest, manifest: mf})
	}

	// Compute per-layer reference counts across DISTINCT manifests
	// (tagged + dangling). Multiple tags pointing at the same manifest
	// would otherwise count its layers twice and flip every entry from
	// unique to shared.
	layerRefs := make(map[string]int)
	seenManifests := make(map[string]bool)
	for _, mi := range manifests {
		if mi.manifest == nil {
			continue
		}
		if seenManifests[mi.digest] {
			continue
		}
		seenManifests[mi.digest] = true
		for _, layer := range mi.manifest.Layers {
			layerRefs[layer.Digest]++
		}
	}

	var out []ImageInfo
	for _, mi := range manifests {
		info := ImageInfo{
			Digest: mi.digest,
			Tag:    mi.tag,
			Cached: mi.manifest != nil,
		}
		if mi.manifest != nil {
			info.DockerRef = mi.manifest.ShedSourceRef()
		}
		// Identity is the Docker ref (io.shed.source-ref) when present.
		switch {
		case info.DockerRef != "":
			info.Name = info.DockerRef
		case mi.tag != "":
			info.Name = mi.tag
		default:
			info.Name = ShortDigest(mi.digest)
		}
		// Source: config when the ref is what the server is configured to
		// use; user when the operator labeled it with a cosmetic tag;
		// dangling when it's an untagged, unconfigured leftover.
		switch {
		case configuredRefs[info.DockerRef]:
			info.Source = "config"
		case mi.tag != "":
			info.Source = "user"
		default:
			info.Source = "dangling"
		}
		if mi.manifest != nil {
			for _, layer := range mi.manifest.Layers {
				info.SizeBytes += layer.Size
				if layerRefs[layer.Digest] <= 1 {
					info.UniqueBytes += layer.Size
				} else {
					info.SharedBytes += layer.Size
				}
			}
			// The flattened lower is keyed by manifest digest and
			// unique-per-manifest (no cross-manifest sharing for the
			// erofs file, only for layer blobs above).
			info.SizeBytes += CacheLowerSize(imagesDir, mi.digest)
			info.UniqueBytes += CacheLowerSize(imagesDir, mi.digest)
			if path, err := CacheLowerPath(imagesDir, mi.digest); err == nil {
				info.Path = path
			}
		}
		if len(ProtectiveRefs(refs, mi.digest)) > 0 {
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

// InspectImage returns full details for a tag or digest.
func (m *Manager) InspectImage(tagOrDigest string) (*ImageInfo, *OCIManifest, error) {
	imagesDir := m.cfg.GetImagesDir()
	digest, tagName, err := m.resolveTagOrDigest(tagOrDigest)
	if err != nil {
		return nil, nil, err
	}

	manifest, err := LoadManifestByDigest(imagesDir, digest)
	if err != nil {
		return nil, nil, err
	}

	info := &ImageInfo{
		Digest:    digest,
		Tag:       tagName,
		Cached:    BlobExists(imagesDir, digest),
		DockerRef: manifest.ShedSourceRef(),
		Source:    "user",
	}
	if info.DockerRef != "" {
		info.Name = info.DockerRef
		if m.isConfiguredRef(info.DockerRef) {
			info.Source = "config"
		}
	} else if tagName != "" {
		info.Name = tagName
	} else {
		info.Name = ShortDigest(digest)
		info.Source = "dangling"
	}
	for _, layer := range manifest.Layers {
		info.SizeBytes += layer.Size
	}
	info.SizeBytes += CacheLowerSize(imagesDir, digest)
	if path, err := CacheLowerPath(imagesDir, digest); err == nil {
		info.Path = path
	}
	if m.scanner != nil {
		refs, err := m.scanner.ScanRefs(false)
		if err == nil && len(ProtectiveRefs(refs, digest)) > 0 {
			info.InUse = true
		}
	}
	return info, manifest, nil
}

// TagImage points a new tag at the manifest digest currently held by
// srcTagOrDigest. Overwrites newTag if it already exists.
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
// prefix) and returns (digest, tagName).
func (m *Manager) resolveTagOrDigest(s string) (digest, tagName string, err error) {
	imagesDir := m.cfg.GetImagesDir()
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
// truncated) against installed blobs.
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

// configuredRefs returns the Docker refs the server config currently points
// at: the default_image plus every image_aliases value.
func (m *Manager) configuredRefs() []string {
	var out []string
	if dr := m.cfg.GetDefaultImage(); IsDockerRef(dr) {
		out = append(out, dr)
	}
	for _, v := range m.cfg.GetImageAliases() {
		if IsDockerRef(v) {
			out = append(out, v)
		}
	}
	return out
}

// isConfiguredRef reports whether ref is the configured default_image or one
// of the configured image_aliases values.
func (m *Manager) isConfiguredRef(ref string) bool {
	if ref == "" {
		return false
	}
	if m.cfg.GetDefaultImage() == ref {
		return true
	}
	for _, v := range m.cfg.GetImageAliases() {
		if v == ref {
			return true
		}
	}
	return false
}

// resolveDeleteTarget maps a user-supplied identifier (a cosmetic tag label,
// a digest, or a Docker ref) to a manifest digest. A bare label like "full"
// parses as a Docker ref (docker.io/library/full), so tag/digest resolution
// is tried first; the ref-index is the fallback for full registry refs.
func (m *Manager) resolveDeleteTarget(imagesDir, ident string) (string, error) {
	if digest, _, err := m.resolveTagOrDigest(ident); err == nil {
		return digest, nil
	}
	if IsDockerRef(ident) {
		if digest, ok := ResolveRefDigest(imagesDir, ident); ok {
			return digest, nil
		}
	}
	return "", ErrImageNotFound
}

// DeleteImage removes an image's addressability — every cosmetic tag and the
// ref-index entry pointing at the resolved manifest. Following the Docker
// model, the underlying manifest blob is NOT removed; call PruneImages to
// garbage-collect once nothing references it. ident may be a Docker ref, a
// digest, or a cosmetic tag label.
//
// Hard-blocks only when the manifest is pinned by a live shed or snapshot.
// (Warning when the target is the configured default_image is a CLI concern.)
func (m *Manager) DeleteImage(ident string) error {
	imagesDir := m.cfg.GetImagesDir()
	digest, err := m.resolveDeleteTarget(imagesDir, ident)
	if err != nil {
		return err
	}

	if m.scanner != nil {
		refs, serr := m.scanner.ScanRefs(false)
		if serr != nil {
			return fmt.Errorf("scanning refs: %w", serr)
		}
		if len(ProtectiveRefs(refs, digest)) > 0 {
			return ErrImageInUse
		}
	}

	// Untag every cosmetic tag pointing at this manifest and drop its
	// ref-index entry so the image is no longer resolvable.
	tags, err := ListTags(imagesDir)
	if err != nil {
		return err
	}
	for _, tag := range tags {
		if t, gerr := GetTag(imagesDir, tag); gerr == nil && t.Digest == digest {
			_ = DeleteTag(imagesDir, tag)
		}
	}
	RefIndexDeleteByDigest(imagesDir, digest)
	return nil
}

// PruneImages removes blobs unreferenced by any shed/snapshot.
//
// Reachability: a blob is "live" iff it is a manifest pinned by a shed
// or snapshot (via metadata.LowerDigest, which is the OCI manifest
// digest), or it is reachable from a live manifest (its config,
// layers, kernel, initrd, and any cached ext4 for those layers).
//
// All other blobs are candidates for prune. Cached ext4 files for
// orphaned layers are also evicted. Dangling tag files (pointing at a
// no-longer-present manifest) are dropped.
func (m *Manager) PruneImages(dryRun bool) ([]ImageInfo, error) {
	imagesDir := m.cfg.GetImagesDir()
	if imagesDir == "" {
		return nil, nil
	}

	// PruneImages is destructive: a partial ref set risks deleting a
	// blob the broken-but-recoverable shed pinned. Strict scan fails
	// closed with a clear "remove the broken dir first" error.
	var refs []Reference
	if m.scanner != nil {
		var err error
		refs, err = m.scanner.ScanRefs(true)
		if err != nil {
			return nil, fmt.Errorf("scanning refs: %w", err)
		}
	}

	// Live manifest digests come from: every protective ref (shed,
	// snapshot, pending-create) PLUS every tag. RefScanner
	// implementations don't emit tag refs (tags live in the central
	// store the Manager owns, not in per-backend metadata), so we
	// walk them here and merge. Pre-v0.5.8 tags were treated as
	// informational and prune deleted blobs they pointed at; the
	// `shed image pull X && shed image prune` workflow then deleted
	// the manifest just pulled. See refs.go RefKindTag and
	// docs/upgrades/v0.5.7-to-v0.5.8.md.
	liveManifests := make(map[string]bool)
	for _, r := range refs {
		liveManifests[r.Digest] = true
	}
	tagNames, err := ListTags(imagesDir)
	if err != nil {
		return nil, fmt.Errorf("listing tags for prune protection: %w", err)
	}
	for _, name := range tagNames {
		t, err := GetTag(imagesDir, name)
		if err != nil {
			log.Printf("Warning: skipping tag %q for prune protection: %v", name, err)
			continue
		}
		if !BlobExists(imagesDir, t.Digest) {
			// Stale tag — manifest blob is already gone. Nothing to
			// protect; leave it out of liveManifests so the prune
			// reachability walk doesn't trip on the missing blob.
			log.Printf("Warning: tag %q points at missing manifest %s; tag is stale", name, t.Digest)
			continue
		}
		liveManifests[t.Digest] = true
	}

	// Protect the digests the server config currently resolves to
	// (default_image + image_aliases), independent of cosmetic tags.
	// Tags are now cosmetic labels a user can `rm`; without this, removing
	// the derived tag and then pruning could GC the blob a fresh `shed
	// create` still resolves to via the ref-index — a use-after-free.
	for _, ref := range m.configuredRefs() {
		if digest, ok := ResolveRefDigest(imagesDir, ref); ok && BlobExists(imagesDir, digest) {
			liveManifests[digest] = true
		}
	}

	// Expand to a full reachable-set: configs, layers, kernel, initrd
	// of every live manifest.
	reachable := make(map[string]bool)
	for digest := range liveManifests {
		reachable[digest] = true
		manifest, err := LoadManifestByDigest(imagesDir, digest)
		if err != nil {
			// Broken live manifest blocks deletion of unrelated blobs.
			// Log and continue; the broken blob itself will not be removed
			// because liveManifests already marks it reachable.
			log.Printf("Warning: live manifest %s unreadable: %v", digest, err)
			continue
		}
		reachable[manifest.Config.Digest] = true
		for _, layer := range manifest.Layers {
			reachable[layer.Digest] = true
		}
		if d := manifest.ShedKernelDigest(); d != "" {
			reachable[d] = true
		}
		if d := manifest.ShedInitrdDigest(); d != "" {
			reachable[d] = true
		}
		if d := manifest.ShedRootfsErofsDigest(); d != "" {
			reachable[d] = true
		}
	}

	allBlobs, err := ListBlobs(imagesDir)
	if err != nil {
		return nil, err
	}

	tagMap, err := tagDigestMap(imagesDir)
	if err != nil {
		return nil, err
	}

	// index.json is the cheap source of truth for "which blobs are
	// manifests". The old prune walker read every blob to probe-parse
	// it as a manifest; that's O(total store bytes) for what amounts
	// to a directory listing.
	indexed, err := IndexManifestDigests(imagesDir)
	if err != nil {
		return nil, fmt.Errorf("reading OCI index: %w", err)
	}

	// Manifest candidates are unreachable blobs the index identifies
	// as manifests; non-manifest candidates (configs/layers/kernels
	// that nobody references anymore) come from the remaining
	// unreachable blobs.
	manifestCandidates := make(map[string]*OCIManifest)
	var candidateBlobs []string
	for _, b := range allBlobs {
		if reachable[b] {
			continue
		}
		candidateBlobs = append(candidateBlobs, b)
		if !indexed[b] {
			continue
		}
		if mf, err := LoadManifestByDigest(imagesDir, b); err == nil {
			manifestCandidates[b] = mf
		}
	}

	var candidates []ImageInfo
	for _, b := range candidateBlobs {
		info := ImageInfo{
			Digest: b,
			Source: "dangling",
			Cached: true,
		}
		if tagName, ok := tagMap[b]; ok {
			info.Tag = tagName
			info.Name = tagName
		} else {
			info.Name = ShortDigest(b)
		}
		if m, ok := manifestCandidates[b]; ok {
			info.DockerRef = m.ShedSourceRef()
			if info.DockerRef != "" {
				info.Name = info.DockerRef
			}
			for _, layer := range m.Layers {
				info.SizeBytes += layer.Size
			}
			info.SizeBytes += CacheLowerSize(imagesDir, b)
		}
		candidates = append(candidates, info)
	}

	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].Name < candidates[j].Name
	})

	if dryRun {
		return candidates, nil
	}

	// Cached lower-image files are keyed by manifest digest. Evict any
	// cache file whose digest is not in `reachable`.
	cacheDirPath := filepath.Join(imagesDir, cacheDir, algorithmDir)
	if entries, err := os.ReadDir(cacheDirPath); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			name := e.Name()
			if filepath.Ext(name) != CacheLowerExt {
				continue
			}
			hex := strings.TrimSuffix(name, CacheLowerExt)
			if len(hex) != 64 {
				continue
			}
			digest := DigestPrefix + hex
			if reachable[digest] {
				continue
			}
			_ = os.Remove(filepath.Join(cacheDirPath, name))
		}
	}

	var deleted []ImageInfo
	for _, c := range candidates {
		if err := DeleteBlob(imagesDir, c.Digest); err != nil {
			log.Printf("warning: failed to remove blob %s: %v", c.Digest, err)
			continue
		}
		if c.Tag != "" {
			_ = DeleteTag(imagesDir, c.Tag)
		}
		// Drop the index.json entry too so foreign OCI tools don't
		// see a descriptor pointing at a now-missing blob.
		if indexed[c.Digest] {
			_ = IndexRemoveByDigest(imagesDir, c.Digest)
			// Drop any ref-index entry pointing at this manifest so a
			// later resolve can't hit a dangling sidecar entry (F5).
			RefIndexDeleteByDigest(imagesDir, c.Digest)
		}
		deleted = append(deleted, c)
	}
	return deleted, nil
}

// tagDigestMap returns digest -> tag name for the first tag found
// pointing at each digest.
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
