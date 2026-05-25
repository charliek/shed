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
	Name      string // variant name, used as a tag in the blob store
	Digest    string // set when Path came from a tag in the blob store; preserved through to EnsureResult.Digest
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

	// Cache hit fast path: tag points at a manifest whose source-ref
	// annotation matches the requested DockerRef. Confirm every layer's
	// ext4 is materialized, since a previous cache eviction might have
	// removed them.
	if res, ok := m.resolveCachedTag(ctx, imagesDir, ref.Name, ref.DockerRef); ok {
		return res, nil
	}

	// Serialize concurrent EnsureImage calls for the same tag.
	tagLockPath := filepath.Join(imagesDir, tagsDir, ref.Name+".lock")
	unlock, err := acquireFileLock(tagLockPath)
	if err != nil {
		return EnsureResult{}, fmt.Errorf("acquiring tag lock: %w", err)
	}
	defer unlock()

	if res, ok := m.resolveCachedTag(ctx, imagesDir, ref.Name, ref.DockerRef); ok {
		return res, nil
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
	return m.resolveManifestLower(ctx, imagesDir, pullResult.ManifestDigest)
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

// resolveCachedTag returns an EnsureResult derived from a cached tag
// when one is available and every layer is materialized.
//
// Local tags always win over the configured registry ref: if a tag
// named `name` exists locally and its layers are fully materialized,
// we use it regardless of whether the manifest's io.shed.source-ref
// matches the configured Docker ref. The previous strict equality
// check forced a registry re-pull every time the server config or a
// local build set a different source-ref — that overwrote
// locally-built manifests with whatever was published, which is
// exactly backwards for development workflows. To force a refresh
// from the registry, run `shed image pull <ref> -t <name>` (or
// `shed image rm <name>` first).
//
// `expectedRef` is still accepted for the signature compat but only
// logged for debugging when there's a mismatch.
func (m *Manager) resolveCachedTag(ctx context.Context, imagesDir, name, expectedRef string) (EnsureResult, bool) {
	t, err := GetTag(imagesDir, name)
	if err != nil {
		return EnsureResult{}, false
	}
	if !BlobExists(imagesDir, t.Digest) {
		return EnsureResult{}, false
	}
	manifest, err := LoadManifestByDigest(imagesDir, t.Digest)
	if err != nil {
		return EnsureResult{}, false
	}
	if expectedRef != "" && manifest.ShedSourceRef() != expectedRef {
		log.Printf("vmimage: tag %q points at manifest with source-ref %q which differs from configured ref %q — using local tag anyway (run `shed image rm %s` then re-pull to refresh from registry)",
			name, manifest.ShedSourceRef(), expectedRef, name)
	}
	res, err := m.resolveManifestLower(ctx, imagesDir, t.Digest)
	if err != nil {
		return EnsureResult{}, false
	}
	return res, true
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

	configMap := m.cfg.GetImages()
	if configMap == nil {
		configMap = map[string]string{}
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
		if mi.tag != "" {
			info.Name = mi.tag
			if dockerRef, ok := configMap[mi.tag]; ok && IsDockerRef(dockerRef) {
				info.Source = "config"
				info.DockerRef = dockerRef
			} else {
				info.Source = "discovered"
			}
		} else {
			info.Name = ShortDigest(mi.digest)
			info.Source = "dangling"
		}
		if mi.manifest != nil {
			if info.DockerRef == "" {
				info.DockerRef = mi.manifest.ShedSourceRef()
			}
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

// DeleteImage removes a tag. Following the Docker model, the underlying
// manifest blob is NOT removed — call PruneImages to garbage-collect.
func (m *Manager) DeleteImage(name string) error {
	if err := ValidateImageName(name); err != nil {
		return err
	}
	imagesDir := m.cfg.GetImagesDir()

	if _, ok := m.cfg.GetImages()[name]; ok {
		return ErrImageInUse
	}
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
	// snapshot, pending-create). Tags do NOT keep manifests alive.
	liveManifests := make(map[string]bool)
	for _, r := range refs {
		if r.Kind == RefKindTag {
			continue
		}
		liveManifests[r.Digest] = true
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
			info.Source = "discovered"
		} else {
			info.Name = ShortDigest(b)
		}
		if m, ok := manifestCandidates[b]; ok {
			info.DockerRef = m.ShedSourceRef()
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
