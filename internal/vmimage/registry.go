// Registry-direct image pull, using go-containerregistry. Replaces the
// `docker create` / `docker export` shell-out path for any reference
// reachable over HTTPS. The Docker daemon is no longer required for
// `shed image pull` — we still shell out to a privileged container for
// the mkfs.ext4 step that materializes derived ext4s in the cache.

package vmimage

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/partial"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/singleflight"
)

// PullOptions configures a registry-direct image pull.
type PullOptions struct {
	// Ref is the registry reference (e.g. "ghcr.io/charliek/shed-vz-full:v1").
	Ref string

	// ImagesDir is the OCI image-layout root that receives the blobs.
	ImagesDir string

	// TagName is the shed tag to advance to the new manifest digest.
	// If empty, no tag is set — useful for previewing pulls.
	TagName string

	// Platform selects the per-platform manifest from a multi-arch
	// image index. Defaults to the backend's native platform via
	// the caller; pass an empty string to use go-containerregistry's
	// default-platform resolver.
	Platform string

	// Insecure permits plain-HTTP transport (typical for local test
	// registries like `registry:2` on localhost:5000).
	Insecure bool

	// AuthKeychain is the credential helper chain. nil falls back to
	// authn.DefaultKeychain which reads ~/.docker/config.json.
	AuthKeychain authn.Keychain

	// ExtractKernel, when true, asks the puller to extract a kernel
	// from the layer rootfs if the manifest has no
	// io.shed.kernel.digest annotation. ExtractKernel + a manifest
	// without the annotation triggers an in-Go tar walk that fishes
	// out /boot/vmlinuz-* or /boot/vmlinux.
	ExtractKernel bool

	// NeedsInitrd is the initrd analog of ExtractKernel.
	NeedsInitrd bool

	// Progress receives stage messages during the pull.
	Progress ProgressFunc

	// Concurrency caps how many blobs download in parallel. <=1 means
	// serial. Callers should pass cfg.GetPullConcurrency().
	Concurrency int
}

// PullResult mirrors ConvertResult so callers can use either flow.
type PullResult struct {
	ManifestDigest string
	ConfigDigest   string
	LayerDigests   []string
	KernelDigest   string
	InitrdDigest   string
}

// PushOptions configures a registry-direct image push.
type PushOptions struct {
	// Ref is the destination registry reference.
	Ref string

	// ImagesDir is the source OCI layout.
	ImagesDir string

	// ManifestDigest is the source manifest digest to push (typically
	// resolved from a tag by the caller).
	ManifestDigest string

	// Insecure permits plain-HTTP transport. Auto-detected for
	// loopback hosts when left at its zero value (caller can also
	// set explicitly).
	Insecure bool

	// AuthKeychain controls credential lookup. nil → DefaultKeychain.
	AuthKeychain authn.Keychain

	// Progress is invoked with stage updates.
	Progress ProgressFunc
}

// PushFromOCILayout uploads the on-disk manifest + config + layers to
// the destination registry. Layer bytes are streamed straight from the
// OCI store so the registry digests are preserved end-to-end (the
// byte-perfect push guarantee). Shed-specific kernel/initrd blobs are
// also uploaded when the manifest carries the corresponding
// annotations, so other shed instances can pull them back without
// re-extracting from rootfs.
func PushFromOCILayout(ctx context.Context, opts PushOptions) error {
	if opts.Ref == "" {
		return errors.New("PushOptions.Ref is required")
	}
	if opts.ImagesDir == "" {
		return errors.New("PushOptions.ImagesDir is required")
	}
	if opts.ManifestDigest == "" {
		return errors.New("PushOptions.ManifestDigest is required")
	}

	manifestBytes, err := ReadBlob(opts.ImagesDir, opts.ManifestDigest)
	if err != nil {
		return fmt.Errorf("reading manifest blob: %w", err)
	}
	manifest, err := ParseManifest(manifestBytes)
	if err != nil {
		return fmt.Errorf("parsing manifest: %w", err)
	}

	keychain := opts.AuthKeychain
	if keychain == nil {
		keychain = authn.DefaultKeychain
	}

	insecure := opts.Insecure || isLoopbackHost(opts.Ref)
	var nameOpts []name.Option
	if insecure {
		nameOpts = append(nameOpts, name.Insecure)
	}
	dst, err := name.ParseReference(opts.Ref, nameOpts...)
	if err != nil {
		return fmt.Errorf("parsing destination ref %q: %w", opts.Ref, err)
	}

	remoteOpts := []remote.Option{
		remote.WithAuthFromKeychain(keychain),
		remote.WithContext(ctx),
	}

	// Upload kernel / initrd / rootfs-erofs loose blobs first so
	// the manifest's annotation references resolve when other clients
	// fetch.
	for _, ann := range []string{AnnotationKernelDigest, AnnotationInitrdDigest, AnnotationRootfsErofsDigest} {
		d := manifest.Annotations[ann]
		if d == "" {
			continue
		}
		emitStatus(opts.Progress, "image", fmt.Sprintf("Pushing %s %s", ann, ShortDigest(d)))
		if err := pushLooseBlob(ctx, dst, d, opts.ImagesDir, remoteOpts, nameOpts); err != nil {
			return fmt.Errorf("pushing %s blob: %w", ann, err)
		}
	}

	// Open the OCI image-layout we already wrote on disk and let
	// go-containerregistry resolve the v1.Image for our manifest
	// digest. remote.Write then streams the layer + config blobs
	// straight from disk byte-for-byte.
	lp, err := layout.FromPath(opts.ImagesDir)
	if err != nil {
		return fmt.Errorf("opening OCI layout: %w", err)
	}
	hash, err := v1.NewHash(opts.ManifestDigest)
	if err != nil {
		return fmt.Errorf("parsing manifest digest %q: %w", opts.ManifestDigest, err)
	}
	img, err := lp.Image(hash)
	if err != nil {
		return fmt.Errorf("loading image from layout: %w", err)
	}
	emitStatus(opts.Progress, "image", fmt.Sprintf("Pushing manifest %s → %s", ShortDigest(opts.ManifestDigest), opts.Ref))
	if err := remote.Write(dst, img, remoteOpts...); err != nil {
		return fmt.Errorf("uploading image: %w", err)
	}
	return nil
}

// pushLooseBlob uploads a single shed-specific blob (kernel/initrd)
// to the registry by digest. Used because go-containerregistry's
// remote.Write only handles layers + config; sibling blobs need a
// direct upload. The blob's bytes flow straight from the on-disk OCI
// store.
func pushLooseBlob(ctx context.Context, ref name.Reference, digest, imagesDir string, remoteOpts []remote.Option, nameOpts []name.Option) error {
	blobRef, err := name.NewDigest(ref.Context().Name()+"@"+digest, nameOpts...)
	if err != nil {
		return fmt.Errorf("forming digest ref: %w", err)
	}
	if _, err := remote.Head(blobRef, remoteOpts...); err == nil {
		return nil // already uploaded
	}
	layer, err := newLooseLayer(imagesDir, digest)
	if err != nil {
		return err
	}
	return remote.WriteLayer(ref.Context(), layer, remoteOpts...)
}

// looseLayer adapts a blob in the OCI store to v1.Layer for upload.
// We mark it as the shed-specific kernel/initrd media type; foreign
// tools see an unknown blob and skip it, while shed re-pulling the
// image fetches the same bytes back by digest.
type looseLayer struct {
	imagesDir string
	digest    v1.Hash
	size      int64
}

func newLooseLayer(imagesDir, digest string) (*looseLayer, error) {
	h, err := v1.NewHash(digest)
	if err != nil {
		return nil, fmt.Errorf("parsing digest %q: %w", digest, err)
	}
	path, err := BlobPath(imagesDir, digest)
	if err != nil {
		return nil, err
	}
	fi, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("stat loose blob: %w", err)
	}
	return &looseLayer{imagesDir: imagesDir, digest: h, size: fi.Size()}, nil
}

func (l *looseLayer) Digest() (v1.Hash, error) { return l.digest, nil }
func (l *looseLayer) DiffID() (v1.Hash, error) { return l.digest, nil }
func (l *looseLayer) Size() (int64, error)     { return l.size, nil }
func (l *looseLayer) MediaType() (types.MediaType, error) {
	return types.MediaType("application/vnd.shed.blob"), nil
}
func (l *looseLayer) Compressed() (io.ReadCloser, error) {
	return OpenBlob(l.imagesDir, l.digest.String())
}
func (l *looseLayer) Uncompressed() (io.ReadCloser, error) {
	return l.Compressed()
}

// Compile-time check: looseLayer satisfies partial.UncompressedLayer.
var _ partial.UncompressedLayer = (*looseLayer)(nil)

// isLoopbackHost is the no-scheme analog of isLoopbackRef (which
// inspects the full ref). True if the ref's host segment is loopback.
func isLoopbackHost(ref string) bool {
	host := ref
	if i := strings.Index(host, "/"); i > 0 {
		host = host[:i]
	}
	for _, p := range []string{"localhost", "127.0.0.1", "[::1]"} {
		if strings.HasPrefix(host, p) {
			return true
		}
	}
	return false
}

// PullToOCILayout fetches an image from a registry into the OCI image
// layout under opts.ImagesDir. The pulled manifest's digest is recorded;
// each layer + config blob is written under blobs/sha256/<digest>.
// If the manifest carries shed annotations for kernel/initrd, those
// blobs are also fetched. Otherwise — and when opts.ExtractKernel is
// set — the kernel/initrd are pulled out of the rootfs layer in Go.
//
// After all blobs are written, the named tag (if any) is advanced and
// each layer is materialized into the derived ext4 cache.
func PullToOCILayout(ctx context.Context, opts PullOptions) (*PullResult, error) {
	if opts.Ref == "" {
		return nil, errors.New("PullOptions.Ref is required")
	}
	if opts.ImagesDir == "" {
		return nil, errors.New("PullOptions.ImagesDir is required")
	}
	if err := EnsureOCILayout(opts.ImagesDir); err != nil {
		return nil, err
	}

	// The progress callback is invoked from parallel download goroutines
	// below, so serialize it: direct callers (tests, the bulk pull-images
	// command) may pass a non-thread-safe sink. The server's SSE sink is
	// already channel-safe; the mutex is cheap because progress is throttled.
	if opts.Progress != nil {
		orig := opts.Progress
		var progMu sync.Mutex
		opts.Progress = func(ev ProgressEvent) {
			progMu.Lock()
			defer progMu.Unlock()
			orig(ev)
		}
	}

	keychain := opts.AuthKeychain
	if keychain == nil {
		keychain = authn.DefaultKeychain
	}

	var nameOpts []name.Option
	if opts.Insecure {
		nameOpts = append(nameOpts, name.Insecure)
	}
	ref, err := name.ParseReference(opts.Ref, nameOpts...)
	if err != nil {
		return nil, fmt.Errorf("parsing ref %q: %w", opts.Ref, err)
	}

	remoteOpts := []remote.Option{
		remote.WithAuthFromKeychain(keychain),
		remote.WithContext(ctx),
	}
	if opts.Platform != "" {
		p, err := PlatformOCI(opts.Platform)
		if err != nil {
			return nil, err
		}
		remoteOpts = append(remoteOpts, remote.WithPlatform(*p))
	}

	emitStatus(opts.Progress, "image", fmt.Sprintf("Fetching manifest %s...", ref.String()))

	var desc *remote.Descriptor
	err = withRetry(ctx, "fetching manifest "+ref.String(), func() error {
		var rerr error
		desc, rerr = remote.Get(ref, remoteOpts...)
		return rerr
	})
	if err != nil {
		return nil, fmt.Errorf("fetching descriptor: %w", err)
	}

	// If the descriptor is an index, resolve to the platform manifest.
	img, err := desc.Image()
	if err != nil {
		return nil, fmt.Errorf("resolving platform image from %s: %w", opts.Ref, err)
	}

	// Manifest descriptor + raw bytes.
	manifestDesc, err := img.Manifest()
	if err != nil {
		return nil, fmt.Errorf("loading manifest: %w", err)
	}
	manifestBytes, err := img.RawManifest()
	if err != nil {
		return nil, fmt.Errorf("raw manifest: %w", err)
	}
	manifestDigest, err := img.Digest()
	if err != nil {
		return nil, fmt.Errorf("manifest digest: %w", err)
	}

	// Config bytes.
	configBytes, err := img.RawConfigFile()
	if err != nil {
		return nil, fmt.Errorf("raw config: %w", err)
	}
	configDigest := DigestBytes(configBytes)

	// Persist config first so layer-cache materialization can read it
	// later if needed.
	if _, err := WriteBlob(opts.ImagesDir, configDigest, configBytes); err != nil {
		return nil, fmt.Errorf("writing config blob: %w", err)
	}

	// Pull each layer to a blob file, content-verified.
	layers, err := img.Layers()
	if err != nil {
		return nil, fmt.Errorf("listing layers: %w", err)
	}
	if len(layers) > MaxLayers {
		return nil, fmt.Errorf("image has %d layers (max %d). This image was probably built with the pre-v0.5.0 build pipeline. Pull a v0.5.0 or later tag from ghcr.io, or build locally with the consolidated Dockerfile under {vz,firecracker}/Dockerfile", len(layers), MaxLayers)
	}

	// Resolve every layer digest up front (cheap, from the manifest) so
	// layerDigests keeps manifest order and the parallel workers below
	// address it by index without racing on append.
	layerDigests := make([]string, len(layers))
	for i := range layers {
		ld, err := layers[i].Digest()
		if err != nil {
			return nil, fmt.Errorf("layer %d digest: %w", i, err)
		}
		layerDigests[i] = ld.String()
	}

	// Download layers + annotated loose blobs concurrently, bounded by
	// opts.Concurrency. Per-blob writes are already atomic (file lock +
	// content-verified rename); a singleflight keyed by digest dedups
	// concurrent fetches of the SAME digest, so a duplicate layer never
	// opens two network streams (and distinct digests never contend on the
	// same blob lock). The first error cancels the group, and the manifest /
	// index / tag are written only after a clean Wait, so a partial pull can
	// never advance a tag.
	//
	// Note on cancellation: gctx cancels *scheduling* (queued goroutines
	// don't start once a sibling fails), but the HTTP reads themselves are
	// bound to the original ctx via remoteOpts, so an already-streaming blob
	// finishes rather than being killed mid-read. That's acceptable — the
	// reads are wrapped in withRetry which bails on ctx cancellation, and a
	// few extra in-flight bytes on the error path don't matter.
	concurrency := opts.Concurrency
	if concurrency < 1 {
		concurrency = 1
	}
	var sf singleflight.Group
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(concurrency)

	for i := range layers {
		i, layer, digest := i, layers[i], layerDigests[i]
		g.Go(func() error {
			return pullLayerBlob(gctx, &sf, opts, i, len(layers), digest, layer)
		})
	}

	// Annotated loose blobs (kernel/initrd/erofs) are independent of the
	// layers, so they download in the same group. The extract-from-layers
	// fallbacks (no annotation) need the layers present and so run after
	// Wait below — they must not race a still-downloading layer.
	kernelAnn := annotationFromManifest(manifestDesc, AnnotationKernelDigest)
	initrdAnn := annotationFromManifest(manifestDesc, AnnotationInitrdDigest)
	erofsAnn := annotationFromManifest(manifestDesc, AnnotationRootfsErofsDigest)
	for _, lb := range []struct{ digest, label, kind string }{
		{kernelAnn, "kernel " + ShortDigest(kernelAnn), "kernel"},
		{initrdAnn, "initrd " + ShortDigest(initrdAnn), "initrd"},
		{erofsAnn, "rootfs " + ShortDigest(erofsAnn), "rootfs erofs"},
	} {
		if lb.digest == "" {
			continue
		}
		lb := lb
		g.Go(func() error {
			_, err, _ := sf.Do(lb.digest, func() (any, error) {
				return nil, pullLooseBlob(gctx, ref, lb.digest, opts.ImagesDir, opts.Insecure, remoteOpts, opts.Progress, lb.label)
			})
			if err != nil {
				return fmt.Errorf("pulling %s blob %s: %w", lb.kind, lb.digest, err)
			}
			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return nil, err
	}

	// Write the manifest verbatim (preserves the registry digest).
	if _, err := WriteBlob(opts.ImagesDir, manifestDigest.String(), manifestBytes); err != nil {
		return nil, fmt.Errorf("writing manifest blob: %w", err)
	}

	// Resolve kernel / initrd digests: an annotation means the blob was
	// already pulled above; otherwise extract it from the now-present layers
	// (the legacy pre-v0.5.2 fallback). This MUST stay after g.Wait() — the
	// extraction reads layer blobs and must not race a still-downloading one.
	var kernelDigest, initrdDigest string
	if kernelAnn != "" {
		kernelDigest = kernelAnn
	} else if opts.ExtractKernel {
		d, err := extractKernelFromLayers(opts.ImagesDir, layerDigests)
		if err != nil {
			return nil, fmt.Errorf("extracting kernel from layer tar: %w", err)
		}
		kernelDigest = d
	}
	if initrdAnn != "" {
		initrdDigest = initrdAnn
	} else if opts.NeedsInitrd && opts.ExtractKernel {
		d, err := extractInitrdFromLayers(opts.ImagesDir, layerDigests)
		if err != nil {
			return nil, fmt.Errorf("extracting initrd from layer tar: %w", err)
		}
		initrdDigest = d
	}

	// Record the manifest in the top-level OCI index.
	if err := indexUpsert(opts.ImagesDir, Descriptor{
		MediaType: string(manifestDesc.MediaType),
		Digest:    manifestDigest.String(),
		Size:      int64(len(manifestBytes)),
		Annotations: map[string]string{
			"org.opencontainers.image.ref.name": opts.TagName,
		},
	}); err != nil {
		return nil, fmt.Errorf("updating index.json: %w", err)
	}

	// The flattened manifest lower is materialized lazily on the next
	// EnsureImage call so a registry pull stays fast on its own; the
	// erofs gets built when the next shed actually needs to boot.

	if opts.TagName != "" {
		if err := SetTag(opts.ImagesDir, opts.TagName, manifestDigest.String()); err != nil {
			return nil, fmt.Errorf("setting tag %q: %w", opts.TagName, err)
		}
	}

	return &PullResult{
		ManifestDigest: manifestDigest.String(),
		ConfigDigest:   configDigest,
		LayerDigests:   layerDigests,
		KernelDigest:   kernelDigest,
		InitrdDigest:   initrdDigest,
	}, nil
}

// pullLayerBlob downloads (or reports cached) a single layer. It emits the
// blob event before the verbose line so a renderer client suppresses the
// redundant per-layer line, then runs the fetch under a singleflight keyed by
// digest so a duplicate layer is fetched exactly once.
func pullLayerBlob(ctx context.Context, sf *singleflight.Group, opts PullOptions, i, n int, digest string, layer v1.Layer) error {
	if err := ctx.Err(); err != nil {
		return err // the group already failed; don't start another download
	}
	label := fmt.Sprintf("layer %d/%d %s", i+1, n, ShortDigest(digest))
	if BlobExists(opts.ImagesDir, digest) {
		size, _ := layer.Size()
		emitBlob(opts.Progress, digest, label, BlobStatusExists, size, size)
		emitStatus(opts.Progress, "image", fmt.Sprintf("Layer %d/%d %s already present", i+1, n, ShortDigest(digest)))
		return nil
	}
	size, _ := layer.Size()
	emitBlob(opts.Progress, digest, label, BlobStatusDownloading, 0, size)
	emitStatus(opts.Progress, "image", fmt.Sprintf("Pulling layer %d/%d %s", i+1, n, ShortDigest(digest)))
	_, err, _ := sf.Do(digest, func() (any, error) {
		// Re-check under the singleflight: a duplicate layer may have just
		// written this blob.
		if BlobExists(opts.ImagesDir, digest) {
			return nil, nil
		}
		rc, err := layer.Compressed()
		if err != nil {
			return nil, fmt.Errorf("opening layer %s: %w", digest, err)
		}
		defer rc.Close()
		return nil, streamBlobWithProgress(digest, label, size, opts.Progress, rc, func(r io.Reader) error {
			return writeBlobFromReader(opts.ImagesDir, digest, r)
		})
	})
	if err != nil {
		return fmt.Errorf("streaming layer %s: %w", digest, err)
	}
	return nil
}

// writeBlobFromReader streams a reader through sha256 verification into
// a blob file. Fails if the streamed digest doesn't match `digest`.
func writeBlobFromReader(imagesDir, digest string, r io.Reader) error {
	hex, err := digestHex(digest)
	if err != nil {
		return err
	}
	final := filepath.Join(imagesDir, blobsDir, algorithmDir, hex)
	if _, err := os.Stat(final); err == nil {
		_, _ = io.Copy(io.Discard, r)
		return nil
	}
	lockPath := filepath.Join(imagesDir, blobsDir, algorithmDir, "."+hex+".lock")
	unlock, err := acquireFileLock(lockPath)
	if err != nil {
		return fmt.Errorf("locking blob %s: %w", digest, err)
	}
	defer unlock()
	if _, err := os.Stat(final); err == nil {
		_, _ = io.Copy(io.Discard, r)
		return nil
	}
	tmpFile, err := os.CreateTemp(filepath.Dir(final), "."+hex+".*.tmp")
	if err != nil {
		return fmt.Errorf("creating tmp blob: %w", err)
	}
	tmpPath := tmpFile.Name()

	verified, _, err := streamHashCopy(tmpFile, r)
	if err != nil {
		tmpFile.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("streaming blob: %w", err)
	}
	if err := tmpFile.Sync(); err != nil {
		tmpFile.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := tmpFile.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}
	if verified != digest {
		os.Remove(tmpPath)
		return fmt.Errorf("blob digest mismatch: wanted %s got %s", digest, verified)
	}
	if err := os.Chmod(tmpPath, 0o444); err != nil {
		os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, final); err != nil {
		os.Remove(tmpPath)
		return err
	}
	return nil
}

// streamHashCopy writes src into dst while hashing the same bytes,
// returning the digest of what was copied.
func streamHashCopy(dst io.Writer, src io.Reader) (string, int64, error) {
	pr, pw := io.Pipe()
	defer pr.Close()
	go func() {
		_, err := io.Copy(pw, src)
		pw.CloseWithError(err)
	}()
	digest, size, err := DigestReader(io.TeeReader(pr, dst))
	if err != nil {
		return "", 0, err
	}
	return digest, size, nil
}

// pullLooseBlob fetches a sibling blob (kernel, initrd, etc.) by digest
// from the same repo as `ref`, writing it under blobs/sha256/<hex>.
// The caller passes the same insecure flag they set on the originating
// pull so the sibling-blob request honors the same TLS policy.
// progress + human carry structured per-blob byte progress for the loose
// blob (kernel / initrd / rootfs erofs). Unlike layers these emit NO plain
// status line — they were silent before, so line-mode output is unchanged;
// only the structured (renderer-only) blob events are added.
func pullLooseBlob(ctx context.Context, ref name.Reference, digest, imagesDir string, insecure bool, remoteOpts []remote.Option, progress ProgressFunc, human string) error {
	if BlobExists(imagesDir, digest) {
		// Report the on-disk size (like cached layers do) so an opted-in
		// renderer shows a full bar and can tell a real zero-byte blob from
		// "size unknown".
		size := BlobSize(imagesDir, digest)
		emitBlob(progress, digest, human, BlobStatusExists, size, size)
		return nil
	}
	blobRef, err := name.NewDigest(ref.Context().Name()+"@"+digest, nameOptsForInsecure(insecure)...)
	if err != nil {
		return fmt.Errorf("forming digest ref: %w", err)
	}
	// Whole-blob retry: a mid-stream TCP cut leaves a partial
	// blobs/sha256/<hex>.tmp on disk that writeBlobFromReader's
	// rename never commits, so each retry attempt re-starts from a
	// clean slate. Suitable for kernel + initrd + rootfs erofs
	// blobs (all sub-100 MB); for the multi-hundred-MB rootfs
	// layers a more granular range-resume would pay off — file a
	// follow-up if that ever becomes the bottleneck.
	return withRetry(ctx, "pulling loose blob "+ShortDigest(digest), func() error {
		layer, lerr := remote.Layer(blobRef, remoteOpts...)
		if lerr != nil {
			return fmt.Errorf("requesting layer %s: %w", digest, lerr)
		}
		size, _ := layer.Size()
		rc, oerr := layer.Compressed()
		if oerr != nil {
			return fmt.Errorf("opening loose blob: %w", oerr)
		}
		defer rc.Close()
		// New progressReader per attempt: a retry restarts the byte count
		// from 0 (the renderer shows the restart), which is correct.
		emitBlob(progress, digest, human, BlobStatusDownloading, 0, size)
		return streamBlobWithProgress(digest, human, size, progress, rc, func(r io.Reader) error {
			return writeBlobFromReader(imagesDir, digest, r)
		})
	})
}

// nameOptsForInsecure returns the name parsing options needed for a
// sibling-blob fetch on the same registry. Callers pass the insecure
// flag through from the originating PullOptions rather than guessing
// from the registry string — that way a private HTTP registry that
// the caller explicitly marked insecure stays insecure.
func nameOptsForInsecure(insecure bool) []name.Option {
	if insecure {
		return []name.Option{name.Insecure}
	}
	return nil
}

// annotationFromManifest looks up an annotation value on the manifest.
func annotationFromManifest(m *v1.Manifest, key string) string {
	if m == nil || m.Annotations == nil {
		return ""
	}
	return m.Annotations[key]
}

// extractKernelFromLayers walks layer tar.gzs (newest last) hunting for
// /boot/vmlinuz-* or /boot/vmlinux, writes the kernel as a blob, and
// returns its digest. Used when the manifest has no kernel annotation.
func extractKernelFromLayers(imagesDir string, layerDigests []string) (string, error) {
	for i := len(layerDigests) - 1; i >= 0; i-- {
		data, err := extractBootFile(imagesDir, layerDigests[i], "vmlinuz|vmlinux")
		if err != nil {
			return "", err
		}
		if data == nil {
			continue
		}
		digest := DigestBytes(data)
		if _, err := WriteBlob(imagesDir, digest, data); err != nil {
			return "", err
		}
		return digest, nil
	}
	return "", errors.New("no kernel found in layer rootfs")
}

// extractInitrdFromLayers is the initrd analog of extractKernelFromLayers.
func extractInitrdFromLayers(imagesDir string, layerDigests []string) (string, error) {
	for i := len(layerDigests) - 1; i >= 0; i-- {
		data, err := extractBootFile(imagesDir, layerDigests[i], "initrd.img")
		if err != nil {
			return "", err
		}
		if data == nil {
			continue
		}
		digest := DigestBytes(data)
		if _, err := WriteBlob(imagesDir, digest, data); err != nil {
			return "", err
		}
		return digest, nil
	}
	return "", errors.New("no initrd found in layer rootfs")
}

// maxBootFileSize caps the in-memory buffer used to extract kernel /
// initrd entries from a layer tar. Defense-in-depth: hdr.Size is
// attacker-controlled if a malicious or corrupted registry serves a
// crafted tar; the cap keeps a hostile blob from triggering OOM.
const maxBootFileSize = 256 << 20 // 256 MiB — comfortably above any real kernel/initrd

// extractBootFile streams the layer tar.gz at layerDigest and returns
// the bytes of the highest-versioned /boot/<basenamePattern>* entry.
// basenamePattern is a "|"-separated set of prefixes (e.g. "vmlinuz|vmlinux").
func extractBootFile(imagesDir, layerDigest, basenamePattern string) ([]byte, error) {
	blobPath, err := BlobPath(imagesDir, layerDigest)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(blobPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("gzip reader: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	type candidate struct {
		name string
		buf  *bytes.Buffer
	}
	prefixes := strings.Split(basenamePattern, "|")
	var found []candidate
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("tar next: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		name := filepath.Base(hdr.Name)
		dir := filepath.Dir(hdr.Name)
		if filepath.Base(dir) != "boot" {
			continue
		}
		matches := false
		for _, p := range prefixes {
			if strings.HasPrefix(name, p) {
				matches = true
				break
			}
		}
		if !matches {
			continue
		}
		if hdr.Size > maxBootFileSize {
			return nil, fmt.Errorf("boot file %s exceeds %d-byte limit", hdr.Name, maxBootFileSize)
		}
		buf := bytes.NewBuffer(make([]byte, 0, hdr.Size))
		if _, err := io.Copy(buf, io.LimitReader(tr, maxBootFileSize)); err != nil {
			return nil, fmt.Errorf("reading %s: %w", hdr.Name, err)
		}
		found = append(found, candidate{name: name, buf: buf})
	}
	if len(found) == 0 {
		return nil, nil
	}
	// Highest-versioned wins (string sort approximates Debian's
	// vmlinuz-X.Y.Z ordering well enough for the common cases).
	sort.Slice(found, func(i, j int) bool { return found[i].name > found[j].name })

	// vmlinuz-* may be gzip-compressed; transparently decompress so the
	// caller gets a flat ELF kernel suitable for vfkit/firecracker.
	data := found[0].buf.Bytes()
	if isGzip(data) {
		gr, err := gzip.NewReader(bytes.NewReader(data))
		if err == nil {
			defer gr.Close()
			var out bytes.Buffer
			if _, err := io.Copy(&out, gr); err == nil {
				return out.Bytes(), nil
			}
		}
	}
	return data, nil
}

func isGzip(b []byte) bool {
	return len(b) >= 2 && b[0] == 0x1f && b[1] == 0x8b
}
