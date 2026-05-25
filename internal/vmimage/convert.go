// OCI-archive ingestion pipeline.
//
// Convert reads an OCI image-layout tar (produced by `docker buildx
// build --output type=oci,dest=...`), copies its layer + config blobs
// into the local OCI layout under {ImagesDir}, extracts the kernel
// and initrd blobs from the image's annotations or fallback layer
// scan, mints the rootfs erofs via shed-build-tools (when
// BuildToolsRef is set), and writes a shed-annotated manifest blob.
// Returns the manifest digest plus the resolved blob digests.
//
// The legacy "DockerRef-without-OCIArchivePath" path (docker create
// + docker export → single-layer flatten) was removed in v0.5.2 —
// it produced single-layer images with Ubuntu's stock initrd that
// couldn't boot through shed-overlay. Callers must hand Convert an
// OCI archive; `shed image build` produces one via buildx, and the
// registry-pull path (`PullToOCILayout` in registry.go) is the
// other supported way to get content into the local store.

package vmimage

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/distribution/reference"
)

// DefaultPlatform is the Docker platform used for VZ images (Apple Silicon).
const DefaultPlatform = "linux/arm64"

// FirecrackerPlatform is the Docker platform used for Firecracker images (x86_64 Linux).
const FirecrackerPlatform = "linux/amd64"

// ConvertOptions configures an OCI-archive ingestion.
//
// OCIArchivePath is the only input mode — Convert reads the OCI
// image-layout tar, copies its layer + config blobs into the local
// OCI image-layout, extracts kernel/initrd, mints the rootfs erofs
// when BuildToolsRef is set, and writes a shed-annotated manifest.
// The docker image's layer structure is preserved across variants
// (extensions and full share base's layers in the on-disk store).
type ConvertOptions struct {
	// OCIArchivePath is the path to an OCI image-layout tar produced
	// by `docker buildx build --output type=oci,dest=...`. Required.
	OCIArchivePath string

	// DockerRef is the image reference being converted (e.g.
	// ghcr.io/.../shed-vz-full:v1.0.0). Recorded verbatim in the
	// io.shed.source-ref manifest annotation so the server's
	// resolveImage cache-hit check (compares the annotation to the
	// configured ref) matches on subsequent pulls. Not used to fetch
	// content — that's the OCI archive's job.
	DockerRef string

	// Name is the variant name (e.g. "full"); recorded as io.shed.variant.
	Name string

	// ImagesDir is the OCI image-layout root; blobs are written under
	// {ImagesDir}/blobs/sha256/.
	ImagesDir string

	// Platform is the Docker platform (e.g., "linux/arm64"). Defaults to DefaultPlatform.
	Platform string

	// ExtractKernel controls whether the kernel should be extracted and
	// written as a shed-typed blob referenced by the manifest annotation.
	ExtractKernel bool

	// NeedsInitrd controls whether an initrd should be extracted alongside
	// the kernel. Only consulted when ExtractKernel is true. When set,
	// either InitrdSourcePath must be provided OR the Ubuntu-style
	// /boot/initrd.img-* will be extracted from the image (a fallback
	// useful for ad-hoc Dockerfiles; not appropriate for shed images
	// that need the shed-overlay initramfs to assemble overlayfs at boot).
	NeedsInitrd bool

	// InitrdSourcePath, when set, points at a pre-built initrd file (on
	// the host) that shed should install as the image's boot initrd
	// instead of extracting one from the rootfs. The shed build flow
	// builds the shed-overlay initramfs separately via build-initramfs.sh
	// and passes the path here so the resulting image boots through
	// shed's overlay assembly path rather than Ubuntu's regular initrd.
	InitrdSourcePath string

	// BuildToolsRef, when set, triggers MintRootfsErofs after the
	// layer blobs are installed: a docker container running mkfs.erofs
	// from this image flattens the layers and produces the
	// io.shed.rootfs.erofs.digest blob. The shed publish workflow pins
	// this to ghcr.io/charliek/shed-build-tools:<current shed tag>;
	// local `shed image build` runs default to the same pin via
	// cmd/shed/image.go's BuildToolsRef flag (see --build-tools-version).
	// When empty, no erofs is minted — the resulting image will be
	// rejected by v0.5.2+ servers at EnsureImage. Empty is only
	// appropriate for tests or for images that will be re-processed
	// by a later mint pass.
	BuildToolsRef string
}

// ConvertResult holds the digests produced by a successful conversion.
type ConvertResult struct {
	// ManifestDigest is the OCI image manifest digest. Used as the
	// image identity in the tag store.
	ManifestDigest string

	// ConfigDigest is the OCI image config digest.
	ConfigDigest string

	// LayerDigests lists the OCI layer descriptor digests (one per
	// layer). Phase 1 always produces exactly one entry; layered
	// conversions land in later phases.
	LayerDigests []string

	// KernelDigest names the shed-typed kernel blob (if extracted).
	KernelDigest string

	// InitrdDigest names the shed-typed initrd blob (if extracted).
	InitrdDigest string

	// RootfsErofsDigest names the prebuilt rootfs erofs blob (only
	// populated when ConvertOptions.BuildToolsRef was non-empty).
	RootfsErofsDigest string

	// RootfsLogicalSize records the uncompressed tar size in bytes.
	RootfsLogicalSize int64
}

// buildShedAnnotations returns the manifest annotation map shed
// writes to every image it ingests. Empty digest / size strings are
// omitted so the emitted JSON doesn't carry confusing empty-string
// entries.
//
// sourceRef is recorded verbatim in io.shed.source-ref; the server's
// resolveImage cache-hit check compares this against the configured
// `ref:`, so publish flows MUST pass the final registry ref here.
func buildShedAnnotations(variant, sourceRef, kernelDigest, initrdDigest, rootfsErofsDigest, rootfsLogicalSize string) map[string]string {
	ann := map[string]string{
		AnnotationSchemaVersion: ShedSchemaVersion,
		AnnotationVariant:       variant,
		AnnotationSourceRef:     sourceRef,
	}
	if kernelDigest != "" {
		ann[AnnotationKernelDigest] = kernelDigest
	}
	if initrdDigest != "" {
		ann[AnnotationInitrdDigest] = initrdDigest
	}
	if rootfsErofsDigest != "" {
		ann[AnnotationRootfsErofsDigest] = rootfsErofsDigest
	}
	if rootfsLogicalSize != "" {
		ann[AnnotationRootfsLogicalSize] = rootfsLogicalSize
	}
	return ann
}

// IsDockerRef returns true if s is a Docker image reference rather than a filesystem path.
func IsDockerRef(s string) bool {
	if s == "" {
		return false
	}
	if strings.HasPrefix(s, "/") || strings.HasPrefix(s, "~") || strings.HasPrefix(s, ".") {
		return false
	}
	_, err := reference.ParseNormalizedNamed(s)
	return err == nil
}

// Resolve looks up a tag and returns the path to the prebuilt
// rootfs erofs blob referenced by the manifest's
// io.shed.rootfs.erofs.digest annotation, when the tag exists, the
// manifest is installed, and (when expectedRef is non-empty) the
// source-ref annotation matches expectedRef. Returns "" otherwise
// (cache miss, manifest missing annotation, or expectedRef
// mismatch).
func Resolve(imagesDir, tag, expectedRef string) string {
	t, err := GetTag(imagesDir, tag)
	if err != nil {
		return ""
	}
	if !BlobExists(imagesDir, t.Digest) {
		return ""
	}
	manifest, err := LoadManifestByDigest(imagesDir, t.Digest)
	if err != nil {
		return ""
	}
	if expectedRef != "" && manifest.ShedSourceRef() != expectedRef {
		return ""
	}
	if len(manifest.Layers) == 0 {
		return ""
	}
	// v0.5.2+: the lower is the prebuilt rootfs erofs blob, not a
	// locally-materialized cache file. Pre-v0.5.2 images have no
	// annotation and are treated as cache misses so callers re-pull
	// against the new tooling (manager.resolveManifestLower returns
	// the precise "rebuild" error when EnsureImage actually tries to
	// boot one).
	erofsDigest := manifest.ShedRootfsErofsDigest()
	if erofsDigest == "" {
		return ""
	}
	if !BlobExists(imagesDir, erofsDigest) {
		return ""
	}
	blobPath, err := BlobPath(imagesDir, erofsDigest)
	if err != nil {
		return ""
	}
	return blobPath
}

// ResolveTag looks up a tag and returns its manifest digest plus the
// path to the manifest's prebuilt rootfs erofs blob. Returns
// ErrTagNotFound or ErrBlobNotFound on miss; returns a clear error
// when the manifest lacks the v0.5.2+ io.shed.rootfs.erofs.digest
// annotation.
func ResolveTag(imagesDir, tag string) (digest, rootfsPath string, err error) {
	t, err := GetTag(imagesDir, tag)
	if err != nil {
		return "", "", err
	}
	if !BlobExists(imagesDir, t.Digest) {
		return t.Digest, "", fmt.Errorf("%w: %s (tag %q)", ErrBlobNotFound, t.Digest, tag)
	}
	manifest, err := LoadManifestByDigest(imagesDir, t.Digest)
	if err != nil {
		return t.Digest, "", err
	}
	if len(manifest.Layers) == 0 {
		return t.Digest, "", fmt.Errorf("manifest %s has no layers", t.Digest)
	}
	erofsDigest := manifest.ShedRootfsErofsDigest()
	if erofsDigest == "" {
		return t.Digest, "", fmt.Errorf(
			"manifest %s (tag %q) lacks %s annotation — image was built with pre-v0.5.2 tooling; re-pull against current images",
			ShortDigest(t.Digest), tag, AnnotationRootfsErofsDigest,
		)
	}
	if !BlobExists(imagesDir, erofsDigest) {
		return t.Digest, "", fmt.Errorf(
			"%w: rootfs erofs %s referenced by manifest %s (tag %q)",
			ErrBlobNotFound, erofsDigest, t.Digest, tag,
		)
	}
	blobPath, err := BlobPath(imagesDir, erofsDigest)
	if err != nil {
		return t.Digest, "", err
	}
	return t.Digest, blobPath, nil
}

// LoadManifestByDigest reads + parses an OCI manifest blob.
func LoadManifestByDigest(imagesDir, manifestDigest string) (*OCIManifest, error) {
	data, err := ReadBlob(imagesDir, manifestDigest)
	if err != nil {
		return nil, err
	}
	return ParseManifest(data)
}

// LoadConfigByDigest reads + parses an OCI image config blob.
func LoadConfigByDigest(imagesDir, configDigest string) (*OCIConfig, error) {
	data, err := ReadBlob(imagesDir, configDigest)
	if err != nil {
		return nil, err
	}
	return ParseConfig(data)
}

// Convert dispatches between two input modes:
//
//   - OCIArchivePath: ingest a buildx OCI tar, preserving its layer
//     structure. Shared layers (extensions FROM base) end up referenced
//     by both manifests with identical digests in the store, so disk
//     usage scales with unique-deltas rather than full-rootfs-per-tag.
//   - DockerRef: flatten the named local docker daemon image to a
//     single layer via docker create + docker export. Kept for the
//     pull-fallback case where a ref is in the daemon but not on
//     a registry.
//
// The caller advances the tag (SetTag) after Convert returns; this
// keeps Convert pure with respect to tag indirection.
func Convert(ctx context.Context, opts ConvertOptions) (*ConvertResult, error) {
	if opts.Platform == "" {
		opts.Platform = DefaultPlatform
	}
	if opts.ImagesDir == "" {
		return nil, errors.New("ConvertOptions.ImagesDir is required")
	}
	if err := EnsureOCILayout(opts.ImagesDir); err != nil {
		return nil, err
	}
	if opts.OCIArchivePath == "" {
		// v0.5.2 dropped the docker-create + docker-export flatten
		// path: it always produced a single-layer manifest with
		// Ubuntu's stock initrd (instead of shed-overlay's), so the
		// resulting shed couldn't boot. Producers must hand us an
		// OCI archive — `shed image build` does this via
		// `docker buildx --output type=oci,dest=...`.
		return nil, errors.New("ConvertOptions.OCIArchivePath is required (docker-export flatten path was removed in v0.5.2)")
	}
	return convertFromOCIArchive(ctx, opts)
}

// convertFromOCIArchive ingests a docker buildx-produced OCI image-layout
// tar, preserving the Dockerfile's stage-to-stage layer structure. Each
// layer blob is content-addressed; layers shared with previously-built
// variants (extensions FROM base, full FROM extensions) collide on the
// existing blobs in our store and dedupe automatically.
func convertFromOCIArchive(ctx context.Context, opts ConvertOptions) (*ConvertResult, error) {
	stagingDir, err := os.MkdirTemp("", "shed-buildx-ingest-*")
	if err != nil {
		return nil, fmt.Errorf("creating ingest staging dir: %w", err)
	}
	defer os.RemoveAll(stagingDir)

	// Untar the buildx output into stagingDir. Result is an OCI image
	// layout (oci-layout + index.json + blobs/sha256/*).
	if err := untarOCIArchive(opts.OCIArchivePath, stagingDir); err != nil {
		return nil, fmt.Errorf("extracting OCI archive: %w", err)
	}

	// Resolve to the platform-specific image manifest. buildx may emit
	// an index with both the image and an attestation manifest;
	// resolveBuildxImage filters attestations out.
	srcManifestDigest, srcManifestBytes, err := resolveBuildxImage(stagingDir, opts.Platform)
	if err != nil {
		return nil, fmt.Errorf("resolving buildx image manifest: %w", err)
	}
	srcManifest, err := ParseManifest(srcManifestBytes)
	if err != nil {
		return nil, fmt.Errorf("parsing buildx manifest: %w", err)
	}
	if len(srcManifest.Layers) == 0 {
		return nil, errors.New("buildx manifest has no layers")
	}
	if len(srcManifest.Layers) > MaxLayers {
		return nil, fmt.Errorf("buildx manifest has %d layers (max %d)", len(srcManifest.Layers), MaxLayers)
	}

	// Stream each layer blob from the staging layout into our local
	// store. Existing blobs (shared base layers between variants) are
	// no-ops thanks to content addressing.
	layerDigests := make([]string, 0, len(srcManifest.Layers))
	for _, layer := range srcManifest.Layers {
		layerDigests = append(layerDigests, layer.Digest)
		stagingBlobPath := filepath.Join(stagingDir, "blobs", "sha256", strings.TrimPrefix(layer.Digest, DigestPrefix))
		if BlobExists(opts.ImagesDir, layer.Digest) {
			continue
		}
		data, err := os.ReadFile(stagingBlobPath)
		if err != nil {
			return nil, fmt.Errorf("reading staged layer %s: %w", layer.Digest, err)
		}
		if _, err := WriteBlob(opts.ImagesDir, layer.Digest, data); err != nil {
			return nil, fmt.Errorf("installing layer %s: %w", layer.Digest, err)
		}
	}

	// Install the OCI config blob verbatim from the buildx layout.
	srcConfigPath := filepath.Join(stagingDir, "blobs", "sha256", strings.TrimPrefix(srcManifest.Config.Digest, DigestPrefix))
	cfgBytes, err := os.ReadFile(srcConfigPath)
	if err != nil {
		return nil, fmt.Errorf("reading staged config: %w", err)
	}
	configDigest := srcManifest.Config.Digest
	if _, err := WriteBlob(opts.ImagesDir, configDigest, cfgBytes); err != nil {
		return nil, fmt.Errorf("installing config blob: %w", err)
	}
	srcConfig, _ := ParseConfig(cfgBytes) // best-effort; we only use rootfs.diff_ids for logging

	// Extract kernel from the freshly-installed layers (top layer first
	// since later RUNs override earlier ones).
	var kernelDigest string
	if opts.ExtractKernel {
		kd, err := extractKernelFromLayers(opts.ImagesDir, layerDigests)
		if err != nil {
			return nil, fmt.Errorf("extracting kernel from buildx layers: %w", err)
		}
		kernelDigest = kd
	}

	// Initrd: prefer caller-supplied path (the shed-overlay initramfs);
	// fall back to extracting from layers for non-shed Dockerfiles.
	var initrdDigest string
	if opts.InitrdSourcePath != "" {
		if _, err := os.Stat(opts.InitrdSourcePath); err != nil {
			return nil, fmt.Errorf("initrd source %q: %w", opts.InitrdSourcePath, err)
		}
		d, _, err := WriteBlobFromFile(opts.ImagesDir, opts.InitrdSourcePath, false)
		if err != nil {
			return nil, fmt.Errorf("installing initrd blob: %w", err)
		}
		initrdDigest = d
	} else if opts.NeedsInitrd {
		d, err := extractInitrdFromLayers(opts.ImagesDir, layerDigests)
		if err != nil {
			return nil, fmt.Errorf("extracting initrd from buildx layers: %w", err)
		}
		initrdDigest = d
	}

	// Mint the read-only rootfs erofs blob (v0.5.2+). The publishing
	// flow pins a known-good mkfs.erofs via the shed-build-tools OCI
	// image — see internal/vmimage/erofs.go for the rationale. When
	// BuildToolsRef is empty (test fixtures, special-purpose builds),
	// skip minting and emit a manifest without the annotation; the
	// resulting image will fail at EnsureImage on any v0.5.2+ server
	// with a clear "rebuild with v0.5.2+ tooling" error.
	var rootfsErofsDigest string
	if opts.BuildToolsRef != "" {
		d, err := MintRootfsErofs(ctx, MintErofsOptions{
			ImagesDir:     opts.ImagesDir,
			LayerDigests:  layerDigests,
			BuildToolsRef: opts.BuildToolsRef,
		})
		if err != nil {
			return nil, fmt.Errorf("minting rootfs erofs: %w", err)
		}
		rootfsErofsDigest = d
	}

	// Build our shed-annotated manifest referencing the same layer +
	// config digests as buildx emitted. Adding annotations changes the
	// manifest digest (it's our digest now, not the buildx digest) —
	// that's fine because our annotations are the source of truth for
	// shed runtime behavior.
	manifest := &OCIManifest{
		SchemaVersion: 2,
		MediaType:     MediaTypeOCIManifest,
		Config: Descriptor{
			MediaType: MediaTypeOCIConfig,
			Digest:    configDigest,
			Size:      int64(len(cfgBytes)),
		},
		Layers:      append([]Descriptor{}, srcManifest.Layers...),
		Annotations: buildShedAnnotations(opts.Name, opts.DockerRef, kernelDigest, initrdDigest, rootfsErofsDigest, ""),
	}
	manData, err := manifest.MarshalIndent()
	if err != nil {
		return nil, fmt.Errorf("marshalling manifest: %w", err)
	}
	manifestDigest := DigestBytes(manData)
	if _, err := WriteBlob(opts.ImagesDir, manifestDigest, manData); err != nil {
		return nil, fmt.Errorf("installing manifest blob: %w", err)
	}

	if err := indexUpsert(opts.ImagesDir, Descriptor{
		MediaType: MediaTypeOCIManifest,
		Digest:    manifestDigest,
		Size:      int64(len(manData)),
		Annotations: map[string]string{
			"org.opencontainers.image.ref.name": opts.Name,
		},
	}); err != nil {
		return nil, fmt.Errorf("updating index.json: %w", err)
	}

	rootfsLogical := int64(0)
	if srcConfig != nil {
		// Sum uncompressed layer sizes as an approximation. We don't
		// have the per-diff_id sizes from the OCI config, so this is
		// best-effort display data only.
		_ = srcConfig // kept for future use
	}
	_ = srcManifestDigest // unused; we mint our own manifest digest with annotations

	return &ConvertResult{
		ManifestDigest:    manifestDigest,
		ConfigDigest:      configDigest,
		LayerDigests:      layerDigests,
		KernelDigest:      kernelDigest,
		InitrdDigest:      initrdDigest,
		RootfsErofsDigest: rootfsErofsDigest,
		RootfsLogicalSize: rootfsLogical,
	}, nil
}

// resolveBuildxImage walks the OCI index in stagingDir and returns the
// digest + raw bytes of the image manifest for the given platform.
// Filters out attestation manifests (which buildx >= 0.11 emits as a
// sibling descriptor).
func resolveBuildxImage(stagingDir, platform string) (digest string, manifestBytes []byte, err error) {
	idxBytes, err := os.ReadFile(filepath.Join(stagingDir, "index.json"))
	if err != nil {
		return "", nil, fmt.Errorf("reading staged index.json: %w", err)
	}
	var idx OCIIndex
	if err := jsonUnmarshal(idxBytes, &idx); err != nil {
		return "", nil, fmt.Errorf("parsing staged index: %w", err)
	}

	// Strategy: prefer descriptors whose Platform fields match. Skip
	// descriptors annotated as attestation manifests. For single-
	// platform buildx output we usually find one non-attestation
	// descriptor.
	wantArch := platformArch(platform)
	for _, m := range idx.Manifests {
		if m.Annotations != nil {
			if t := m.Annotations["vnd.docker.reference.type"]; t == "attestation-manifest" {
				continue
			}
		}
		// Read the manifest blob bytes from staging.
		hex := strings.TrimPrefix(m.Digest, DigestPrefix)
		path := filepath.Join(stagingDir, "blobs", "sha256", hex)
		data, err := os.ReadFile(path)
		if err != nil {
			return "", nil, fmt.Errorf("reading staged manifest %s: %w", m.Digest, err)
		}
		// If the manifest is itself an index, recurse to its platform-matching child.
		if m.MediaType == MediaTypeOCIIndex || strings.HasSuffix(m.MediaType, "manifest.list.v2+json") {
			var inner OCIIndex
			if err := jsonUnmarshal(data, &inner); err != nil {
				return "", nil, fmt.Errorf("parsing nested index %s: %w", m.Digest, err)
			}
			for _, im := range inner.Manifests {
				if im.Annotations != nil && im.Annotations["vnd.docker.reference.type"] == "attestation-manifest" {
					continue
				}
				innerHex := strings.TrimPrefix(im.Digest, DigestPrefix)
				innerPath := filepath.Join(stagingDir, "blobs", "sha256", innerHex)
				innerBytes, err := os.ReadFile(innerPath)
				if err != nil {
					return "", nil, fmt.Errorf("reading nested manifest %s: %w", im.Digest, err)
				}
				return im.Digest, innerBytes, nil
			}
		}
		_ = wantArch // Phase 1: single-platform buildx output — first non-attestation wins.
		return m.Digest, data, nil
	}
	return "", nil, errors.New("no image manifest in OCI archive")
}

// untarOCIArchive extracts a tarball (typically docker buildx --output
// type=oci output) into dst. Safe-by-construction path validation
// rejects any entry that would escape dst.
func untarOCIArchive(archivePath, dst string) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer f.Close()
	tr := tar.NewReader(f)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("tar next: %w", err)
		}
		// Clean and validate the entry path.
		cleaned := filepath.Clean(hdr.Name)
		if strings.HasPrefix(cleaned, "..") || strings.HasPrefix(cleaned, "/") {
			return fmt.Errorf("unsafe tar entry %q", hdr.Name)
		}
		out := filepath.Join(dst, cleaned)
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(out, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
				return err
			}
			w, err := os.OpenFile(out, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
			if err != nil {
				return err
			}
			if _, err := io.Copy(w, tr); err != nil {
				w.Close()
				return err
			}
			if err := w.Close(); err != nil {
				return err
			}
		default:
			// Skip symlinks, char devices, etc. — buildx OCI output
			// doesn't include them at the top level.
		}
	}
	return nil
}

// jsonUnmarshal is a tiny indirection so we can wrap parse errors with
// uniform context strings. encoding/json is the only impl.
func jsonUnmarshal(data []byte, v interface{}) error {
	return json.Unmarshal(data, v)
}

// indexUpsert adds or replaces a descriptor in index.json by ref-name
// annotation. Descriptors without ref names are deduplicated by digest.
//
// Serialized via a per-images-dir flock so two concurrent pulls/builds
// can't both read the same index, modify in memory, and clobber each
// other's writes.
func indexUpsert(imagesDir string, d Descriptor) error {
	lockPath := filepath.Join(imagesDir, ".index.lock")
	unlock, err := acquireFileLock(lockPath)
	if err != nil {
		return fmt.Errorf("locking index: %w", err)
	}
	defer unlock()

	idx, err := ReadIndex(imagesDir)
	if err != nil {
		return err
	}
	refName := ""
	if d.Annotations != nil {
		refName = d.Annotations["org.opencontainers.image.ref.name"]
	}
	out := idx.Manifests[:0]
	for _, m := range idx.Manifests {
		if refName != "" && m.Annotations != nil && m.Annotations["org.opencontainers.image.ref.name"] == refName {
			continue
		}
		if refName == "" && m.Digest == d.Digest {
			continue
		}
		out = append(out, m)
	}
	idx.Manifests = append(out, d)
	return WriteIndex(imagesDir, idx)
}

// platformArch extracts the architecture component of a Docker platform
// string. "linux/arm64" → "arm64", "linux/amd64" → "amd64".
func platformArch(platform string) string {
	parts := strings.SplitN(platform, "/", 2)
	if len(parts) == 2 {
		return parts[1]
	}
	return "arm64"
}
