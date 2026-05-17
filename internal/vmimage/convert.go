// Docker-to-OCI conversion pipeline.
//
// Convert takes a Docker reference, exports its rootfs to a tar, gzips
// the tar as an OCI layer, writes the layer + OCI image config + OCI
// manifest as blobs into the local OCI layout, extracts kernel/initrd
// as additional shed-typed blobs, and materializes an ext4 in the
// cache directory. Returns the manifest digest.
//
// Phase 1 of the OCI refactor still uses docker create/export and
// privileged-Docker mkfs.ext4 for the conversion. Phase 2 replaces the
// pull half with go-containerregistry's remote.Image so a Docker daemon
// is no longer required for `shed image pull`.

package vmimage

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/distribution/reference"
)

// DefaultPlatform is the Docker platform used for VZ images (Apple Silicon).
const DefaultPlatform = "linux/arm64"

// FirecrackerPlatform is the Docker platform used for Firecracker images (x86_64 Linux).
const FirecrackerPlatform = "linux/amd64"

// ConvertOptions configures a Docker-to-OCI conversion.
type ConvertOptions struct {
	// DockerRef is the image reference to convert (e.g. ghcr.io/.../shed-vz-full:v1.0.0).
	DockerRef string

	// Name is the variant name (e.g. "full"); recorded as io.shed.variant.
	Name string

	// ImagesDir is the OCI image-layout root; blobs are written under
	// {ImagesDir}/blobs/sha256/.
	ImagesDir string

	// RootfsSize is the sparse ext4 image size (e.g., "20G"). Defaults to "20G".
	RootfsSize string

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

	// RootfsLogicalSize records the uncompressed tar size in bytes.
	RootfsLogicalSize int64
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

// Resolve looks up a tag and returns the path to the cached ext4 for
// its first layer, when the tag exists, the blob is installed, and
// (when expectedRef is non-empty) the manifest's source-ref annotation
// matches expectedRef. Returns "" otherwise (cache miss).
//
// Phase 1 manifests always have exactly one layer, so the first-layer
// ext4 is the only lower the VM needs. In later phases, callers must
// migrate to ResolveTagLayers and assemble the multi-lower boot.
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
	layer := manifest.Layers[0]
	cachePath, err := CacheExt4Path(imagesDir, layer.Digest)
	if err != nil {
		return ""
	}
	if _, err := os.Stat(cachePath); err != nil {
		return ""
	}
	return cachePath
}

// ResolveTag looks up a tag and returns its manifest digest plus the
// path to the first layer's cached ext4. Returns ErrTagNotFound or
// ErrBlobNotFound on miss.
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
	cachePath, err := CacheExt4Path(imagesDir, manifest.Layers[0].Digest)
	if err != nil {
		return t.Digest, "", err
	}
	return t.Digest, cachePath, nil
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

// Convert pulls a Docker image, gzips its filesystem as an OCI layer,
// writes the OCI manifest+config+layer blobs into the store, extracts
// kernel/initrd as shed-typed blobs, and materializes the derived ext4
// in the cache. Returns the manifest digest.
//
// The caller advances the tag (SetTag) after Convert returns; this
// keeps Convert pure with respect to tag indirection.
func Convert(ctx context.Context, opts ConvertOptions) (*ConvertResult, error) {
	if opts.RootfsSize == "" {
		opts.RootfsSize = DefaultLayerExt4Size
	}
	if opts.Platform == "" {
		opts.Platform = DefaultPlatform
	}
	if opts.ImagesDir == "" {
		return nil, errors.New("ConvertOptions.ImagesDir is required")
	}
	if err := EnsureOCILayout(opts.ImagesDir); err != nil {
		return nil, err
	}

	stagingDir, err := os.MkdirTemp(opts.ImagesDir, ".convert-*")
	if err != nil {
		return nil, fmt.Errorf("creating staging dir: %w", err)
	}
	defer os.RemoveAll(stagingDir)

	containerID, err := dockerCreate(ctx, opts.Platform, opts.DockerRef)
	if err != nil {
		return nil, fmt.Errorf("creating container from %s: %w", opts.DockerRef, err)
	}
	defer dockerRemove(ctx, containerID)

	tarPath := filepath.Join(stagingDir, "rootfs.tar")
	if err := dockerExport(ctx, containerID, tarPath); err != nil {
		return nil, fmt.Errorf("exporting container: %w", err)
	}
	tarStat, err := os.Stat(tarPath)
	if err != nil {
		return nil, fmt.Errorf("stat tar: %w", err)
	}

	// Gzip the tar into a layer blob and capture both digests
	// (compressed = layer descriptor; uncompressed = config diff_id).
	gzPath := filepath.Join(stagingDir, "layer.tar.gz")
	diffID, layerDigest, gzSize, err := gzipFileWithDigests(tarPath, gzPath)
	if err != nil {
		return nil, fmt.Errorf("gzipping layer: %w", err)
	}

	// Install the layer blob.
	layerBlobPath := mustBlobPath(opts.ImagesDir, layerDigest)
	if _, err := os.Stat(layerBlobPath); err != nil {
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("stat layer blob: %w", err)
		}
		if _, _, err := WriteBlobFromFile(opts.ImagesDir, gzPath, true); err != nil {
			return nil, fmt.Errorf("installing layer blob: %w", err)
		}
	}

	// Optional: extract kernel + initrd from the source image and
	// install as shed-typed blobs (annotations on the manifest point
	// at them).
	var kernelDigest, initrdDigest string
	if opts.ExtractKernel {
		kPath := filepath.Join(stagingDir, "vmlinux")
		if err := extractKernel(ctx, opts.Platform, opts.DockerRef, stagingDir); err != nil {
			return nil, fmt.Errorf("extracting kernel: %w", err)
		}
		d, _, err := WriteBlobFromFile(opts.ImagesDir, kPath, true)
		if err != nil {
			return nil, fmt.Errorf("installing kernel blob: %w", err)
		}
		kernelDigest = d

		if opts.NeedsInitrd {
			// Prefer a caller-provided initrd (the shed-overlay
			// initramfs from build-initramfs.sh). Fall back to
			// extracting Ubuntu's stock initrd from /boot for
			// non-shed Dockerfiles.
			var initrdSrc string
			if opts.InitrdSourcePath != "" {
				if _, err := os.Stat(opts.InitrdSourcePath); err != nil {
					return nil, fmt.Errorf("initrd source %q: %w", opts.InitrdSourcePath, err)
				}
				initrdSrc = opts.InitrdSourcePath
			} else {
				initrdSrc = filepath.Join(stagingDir, "initrd.img")
				if err := extractInitrd(ctx, opts.Platform, opts.DockerRef, stagingDir); err != nil {
					return nil, fmt.Errorf("extracting initrd: %w", err)
				}
			}
			d, _, err := WriteBlobFromFile(opts.ImagesDir, initrdSrc, opts.InitrdSourcePath == "")
			if err != nil {
				return nil, fmt.Errorf("installing initrd blob: %w", err)
			}
			initrdDigest = d
		}
	}

	// Build + install the OCI image config.
	arch := platformArch(opts.Platform)
	cfg := &OCIConfig{
		Architecture: arch,
		OS:           "linux",
		Created:      time.Now().UTC().Format(time.RFC3339Nano),
		Author:       "shed",
		RootFS: OCIRootFS{
			Type:    "layers",
			DiffIDs: []string{diffID},
		},
		History: []OCIHistory{{
			Created:   time.Now().UTC().Format(time.RFC3339Nano),
			CreatedBy: fmt.Sprintf("shed image convert %s", opts.DockerRef),
		}},
	}
	cfgData, err := cfg.MarshalIndent()
	if err != nil {
		return nil, fmt.Errorf("marshalling config: %w", err)
	}
	configDigest := DigestBytes(cfgData)
	if _, err := WriteBlob(opts.ImagesDir, configDigest, cfgData); err != nil {
		return nil, fmt.Errorf("installing config blob: %w", err)
	}

	// Build + install the OCI manifest.
	manifest := &OCIManifest{
		SchemaVersion: 2,
		MediaType:     MediaTypeOCIManifest,
		Config: Descriptor{
			MediaType: MediaTypeOCIConfig,
			Digest:    configDigest,
			Size:      int64(len(cfgData)),
		},
		Layers: []Descriptor{{
			MediaType: MediaTypeOCILayer,
			Digest:    layerDigest,
			Size:      gzSize,
		}},
		Annotations: map[string]string{
			AnnotationSchemaVersion:     ShedSchemaVersion,
			AnnotationVariant:           opts.Name,
			AnnotationSourceRef:         opts.DockerRef,
			AnnotationRootfsLogicalSize: fmt.Sprintf("%d", tarStat.Size()),
		},
	}
	if kernelDigest != "" {
		manifest.Annotations[AnnotationKernelDigest] = kernelDigest
	}
	if initrdDigest != "" {
		manifest.Annotations[AnnotationInitrdDigest] = initrdDigest
	}
	manData, err := manifest.MarshalIndent()
	if err != nil {
		return nil, fmt.Errorf("marshalling manifest: %w", err)
	}
	manifestDigest := DigestBytes(manData)
	if _, err := WriteBlob(opts.ImagesDir, manifestDigest, manData); err != nil {
		return nil, fmt.Errorf("installing manifest blob: %w", err)
	}

	// Record the manifest in the top-level OCI index. Foreign tools
	// (crane, oras) enumerate the store via index.json.
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

	// Materialize the derived ext4 in the cache (Phase 1: single layer).
	if _, err := EnsureExt4FromLayer(ctx, opts.ImagesDir, layerDigest, opts.Platform, opts.RootfsSize); err != nil {
		return nil, fmt.Errorf("materializing ext4 cache: %w", err)
	}

	return &ConvertResult{
		ManifestDigest:    manifestDigest,
		ConfigDigest:      configDigest,
		LayerDigests:      []string{layerDigest},
		KernelDigest:      kernelDigest,
		InitrdDigest:      initrdDigest,
		RootfsLogicalSize: tarStat.Size(),
	}, nil
}

// gzipFileWithDigests reads src, computes its sha256 (the OCI diff_id),
// gzips it to dst, and computes the gzipped output's sha256 (the OCI
// layer descriptor digest). Returns both digests plus the gzipped size.
func gzipFileWithDigests(src, dst string) (diffID, layerDigest string, gzSize int64, err error) {
	in, err := os.Open(src)
	if err != nil {
		return "", "", 0, err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return "", "", 0, err
	}
	defer out.Close()

	// Compose: out ← gw ← tee(in, diffH) ; layerH digests gw's bytes.
	// We hash compressed bytes via a TeeReader on the final output file
	// after gzipping; a stream-side MultiWriter on gw would only see
	// uncompressed input. Two-pass keeps the code simple.
	gw := gzip.NewWriter(out)
	diffH := sha256.New()
	tee := io.TeeReader(in, diffH)

	if _, err := io.Copy(gw, tee); err != nil {
		gw.Close()
		return "", "", 0, fmt.Errorf("gzipping: %w", err)
	}
	if err := gw.Close(); err != nil {
		return "", "", 0, fmt.Errorf("closing gzip writer: %w", err)
	}
	if err := out.Sync(); err != nil {
		return "", "", 0, fmt.Errorf("sync gz: %w", err)
	}
	stat, err := out.Stat()
	if err != nil {
		return "", "", 0, fmt.Errorf("stat gz: %w", err)
	}
	gzSize = stat.Size()

	// Hash the compressed output.
	gz, err := os.Open(dst)
	if err != nil {
		return "", "", 0, err
	}
	defer gz.Close()
	layerH := sha256.New()
	if _, err := io.Copy(layerH, gz); err != nil {
		return "", "", 0, fmt.Errorf("hashing gz: %w", err)
	}

	diffID = DigestPrefix + hex.EncodeToString(diffH.Sum(nil))
	layerDigest = DigestPrefix + hex.EncodeToString(layerH.Sum(nil))
	return diffID, layerDigest, gzSize, nil
}

// mustBlobPath panics if BlobPath fails; convenience for cases where
// the digest has already been validated.
func mustBlobPath(imagesDir, digest string) string {
	p, err := BlobPath(imagesDir, digest)
	if err != nil {
		panic(err)
	}
	return p
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

func dockerCreate(ctx context.Context, platform, imageRef string) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", "create", "--platform", platform, imageRef)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%s: %w", strings.TrimSpace(stderr.String()), err)
	}
	return strings.TrimSpace(stdout.String()), nil
}

func dockerExport(ctx context.Context, containerID, tarPath string) error {
	outFile, err := os.Create(tarPath)
	if err != nil {
		return err
	}
	defer outFile.Close()

	cmd := exec.CommandContext(ctx, "docker", "export", containerID)
	cmd.Stdout = outFile
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w", strings.TrimSpace(stderr.String()), err)
	}
	return nil
}

func dockerRemove(ctx context.Context, containerID string) {
	exec.CommandContext(ctx, "docker", "rm", containerID).Run() //nolint:errcheck
}

// dockerRunScript runs a bash script inside the Docker image with
// outputDir mounted at /output.
func dockerRunScript(ctx context.Context, platform, imageRef, outputDir, script string) error {
	cmd := exec.CommandContext(ctx, "docker", "run", "--rm", "--platform", platform,
		"--entrypoint", "/bin/bash",
		"-v", outputDir+":/output",
		imageRef, "-c", script)

	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w", strings.TrimSpace(stderr.String()), err)
	}
	return nil
}

func extractKernel(ctx context.Context, platform, imageRef, outputDir string) error {
	return dockerRunScript(ctx, platform, imageRef, outputDir, `set -euo pipefail
VMLINUZ=$(ls -v /boot/vmlinuz-* 2>/dev/null | tail -1 || true)
if [ -n "$VMLINUZ" ]; then
    if zcat "$VMLINUZ" > /output/vmlinux 2>/dev/null; then
        echo 'Decompressed gzip kernel'
    else
        cp "$VMLINUZ" /output/vmlinux
    fi
    exit 0
fi
if [ -f /boot/vmlinux ]; then
    cp /boot/vmlinux /output/vmlinux
    echo 'Copied uncompressed kernel'
    exit 0
fi
echo 'ERROR: No kernel found in /boot/'
exit 1`)
}

func extractInitrd(ctx context.Context, platform, imageRef, outputDir string) error {
	return dockerRunScript(ctx, platform, imageRef, outputDir, `set -euo pipefail
INITRD=$(ls -v /boot/initrd.img-* 2>/dev/null | tail -1 || true)
if [ -z "$INITRD" ]; then
    echo 'ERROR: No initrd found in /boot/'
    exit 1
fi
cp "$INITRD" /output/initrd.img`)
}
