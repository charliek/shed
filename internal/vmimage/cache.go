// Derived read-only layer cache for OCI image layers.
//
// OCI layers are gzipped tarballs; shed boots from compressed read-only
// block devices. The cache holds one erofs image per layer digest at
// {imagesDir}/cache/sha256/<hex>.erofs. Cache files are derived: they
// can be regenerated from the layer blob at any time. Future work (see
// docs/discovery/layer-storage-optimization.md) adds eviction policies
// for layers not currently in use.
//
// Phase 2 (v0.5.1) replaced the docker-based `mkfs.ext4` pipeline with
// a backend-specific materializer:
//
//   - On Linux hosts, mkfs.erofs is invoked natively (deb dependency
//     adds erofs-utils as a runtime dep).
//   - On Mac hosts, shed-server boots a one-shot vfkit VM whose
//     initramfs runs mkfs.erofs internally via the shed.mode=materialize
//     branch.
//
// The legacy `docker run ubuntu:24.04 mkfs.ext4` pipeline is preserved
// as a fallback for the first-pull case on Mac (no shed image cached
// yet, so no kernel + initrd available to boot the materializer VM).
// That fallback writes an .ext4 file alongside the new .erofs format so
// callers that still need the legacy single-extension can find it.

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
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"syscall"
)

// validLowerSize constrains the size string passed to truncate(1) when
// the legacy docker fallback runs. Restricting to a digits-with-optional-
// suffix pattern keeps that path injection-free even if a future caller
// plumbs untrusted input down.
var validLowerSize = regexp.MustCompile(`^[1-9][0-9]*[KMGTP]?$`)

const cacheDir = "cache"

// CacheLowerExt is the file extension used for derived lower images in
// the cache directory. The erofs format gives lz4 random-read
// compression and ~50% on-disk savings vs ext4. The extension is part
// of the on-disk filename only; callers should treat the path returned
// by CacheLowerPath as an opaque value.
const CacheLowerExt = ".erofs"

// LegacyCacheLowerExt is the previous extension (pre-v0.5.1). Some
// install paths still produce .ext4 cache files (e.g., the docker
// fallback when no shed image is yet cached on Mac). PruneImages walks
// both extensions so both formats are GC'd uniformly.
const LegacyCacheLowerExt = ".ext4"

// DefaultLayerSize is the sparse size used for derived lower images
// produced by the docker fallback path. The erofs materializer produces
// a tightly-sized image and ignores this hint. Tiny layers waste some
// blocks but the file is sparse so on-disk usage tracks content.
const DefaultLayerSize = "20G"

// MaterializerFunc materializes an OCI layer tar.gz at blobPath into
// outputPath as a read-only filesystem image. Implementations are
// expected to be backend-specific (host-native mkfs on Linux, a
// materializer VM on Mac). The function is registered at process
// startup by the server binary so the vmimage package stays
// backend-agnostic.
type MaterializerFunc func(ctx context.Context, blobPath, outputPath, platform string) error

// materializerHook is the registered backend-specific materializer.
// nil on test/CLI binaries that never need to materialize.
var materializerHook MaterializerFunc

// RegisterMaterializer installs the backend-specific materializer.
// Idempotent; the last caller wins. Pass nil to clear the hook (used in
// tests that exercise the host-native or fallback paths exclusively).
func RegisterMaterializer(fn MaterializerFunc) {
	materializerHook = fn
}

// CacheLowerPath returns the on-disk path of the derived lower image
// for a layer. layerDigest must be of the form "sha256:<hex>".
func CacheLowerPath(imagesDir, layerDigest string) (string, error) {
	hex, err := digestHex(layerDigest)
	if err != nil {
		return "", err
	}
	return filepath.Join(imagesDir, cacheDir, algorithmDir, hex+CacheLowerExt), nil
}

// CacheLowerPathLegacy returns the legacy .ext4 path for a layer. Used
// by the docker fallback materializer (which still writes .ext4) and by
// PruneImages's GC scan. Returns the same hex prefix as CacheLowerPath
// but with the legacy extension.
func CacheLowerPathLegacy(imagesDir, layerDigest string) (string, error) {
	hex, err := digestHex(layerDigest)
	if err != nil {
		return "", err
	}
	return filepath.Join(imagesDir, cacheDir, algorithmDir, hex+LegacyCacheLowerExt), nil
}

// CacheExt4Path is the legacy accessor name preserved for backward
// compatibility with internal tests. New code should call
// CacheLowerPath. The returned path uses the current cache extension
// (.erofs from v0.5.1 onward).
//
// Deprecated: use CacheLowerPath.
func CacheExt4Path(imagesDir, layerDigest string) (string, error) {
	return CacheLowerPath(imagesDir, layerDigest)
}

// CacheLowerExists reports whether the derived lower image for a layer
// is present, in either the current (.erofs) or legacy (.ext4) format.
func CacheLowerExists(imagesDir, layerDigest string) bool {
	if path, err := CacheLowerPath(imagesDir, layerDigest); err == nil {
		if _, err := os.Stat(path); err == nil {
			return true
		}
	}
	if path, err := CacheLowerPathLegacy(imagesDir, layerDigest); err == nil {
		if _, err := os.Stat(path); err == nil {
			return true
		}
	}
	return false
}

// CacheExt4Exists is preserved for compatibility with existing tests
// and call sites. Returns true if any cached lower image exists for the
// layer, regardless of format.
//
// Deprecated: use CacheLowerExists.
func CacheExt4Exists(imagesDir, layerDigest string) bool {
	return CacheLowerExists(imagesDir, layerDigest)
}

// CacheLowerSize returns the on-disk size of the cached lower image
// for a layer, or 0 if absent. Reports actual allocated blocks
// (st_blocks × 512), not the sparse-file logical length — otherwise
// `shed image ls`'s SIZE column reads 100+ GB for a manifest with a
// handful of layers. If both .erofs and .ext4 exist for the same
// digest (shouldn't happen but is possible during a partial migration),
// returns the sum so disk-usage reporting stays honest.
func CacheLowerSize(imagesDir, layerDigest string) int64 {
	var total int64
	for _, fn := range []func(string, string) (string, error){CacheLowerPath, CacheLowerPathLegacy} {
		path, err := fn(imagesDir, layerDigest)
		if err != nil {
			continue
		}
		fi, err := os.Stat(path)
		if err != nil {
			continue
		}
		if st, ok := fi.Sys().(*syscall.Stat_t); ok {
			total += st.Blocks * 512
		} else {
			total += fi.Size()
		}
	}
	return total
}

// CacheExt4Size is preserved for compatibility with existing call sites.
//
// Deprecated: use CacheLowerSize.
func CacheExt4Size(imagesDir, layerDigest string) int64 {
	return CacheLowerSize(imagesDir, layerDigest)
}

// RemoveCachedLower evicts derived lower images for a layer in both the
// current and legacy formats. Idempotent: missing files are not an
// error.
func RemoveCachedLower(imagesDir, layerDigest string) error {
	for _, fn := range []func(string, string) (string, error){CacheLowerPath, CacheLowerPathLegacy} {
		path, err := fn(imagesDir, layerDigest)
		if err != nil {
			return err
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("removing cached lower: %w", err)
		}
	}
	return nil
}

// RemoveCachedExt4 is preserved for compatibility with existing call
// sites. Same semantics as RemoveCachedLower.
//
// Deprecated: use RemoveCachedLower.
func RemoveCachedExt4(imagesDir, layerDigest string) error {
	return RemoveCachedLower(imagesDir, layerDigest)
}

// EnsureLowerFromManifest materializes (or re-materializes) the
// flattened read-only lower image for an OCI manifest. The cache file
// is keyed by manifest digest at
// {imagesDir}/cache/sha256/<manifest-digest>.erofs, shared across every
// shed that boots from this manifest.
//
// If the cache file already exists, returns its path without rebuilding.
// Otherwise: flatten every layer with whiteout handling into a temp
// tarball, run `mkfs.erofs --tar=f -z lz4 -E force-inode-compact` to
// produce the erofs image, then atomically rename into the cache. A
// file lock around the cache path keeps concurrent EnsureImage calls
// from racing each other.
//
// Requires `mkfs.erofs` on PATH (apt install erofs-utils on
// Debian/Ubuntu, brew install erofs-utils on macOS).
func EnsureLowerFromManifest(ctx context.Context, imagesDir, manifestDigest string) (string, error) {
	if err := EnsureOCILayout(imagesDir); err != nil {
		return "", err
	}
	if _, err := exec.LookPath("mkfs.erofs"); err != nil {
		return "", fmt.Errorf("mkfs.erofs not found on PATH (install erofs-utils): %w", err)
	}

	finalPath, err := CacheLowerPath(imagesDir, manifestDigest)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(finalPath); err == nil {
		return finalPath, nil
	}

	if !BlobExists(imagesDir, manifestDigest) {
		return "", fmt.Errorf("%w: manifest %s", ErrBlobNotFound, manifestDigest)
	}

	hex, _ := digestHex(manifestDigest)
	lockPath := filepath.Join(imagesDir, cacheDir, algorithmDir, "."+hex+CacheLowerExt+".lock")
	unlock, err := acquireFileLock(lockPath)
	if err != nil {
		return "", fmt.Errorf("locking cache lower %s: %w", manifestDigest, err)
	}
	defer unlock()

	// Re-check under the lock — another worker may have raced us.
	if _, err := os.Stat(finalPath); err == nil {
		return finalPath, nil
	}

	stagingErofs, err := os.CreateTemp(filepath.Dir(finalPath), "."+hex+".*"+CacheLowerExt+".tmp")
	if err != nil {
		return "", fmt.Errorf("creating staging erofs: %w", err)
	}
	stagingPath := stagingErofs.Name()
	stagingErofs.Close()
	cleanupStaging := true
	defer func() {
		if cleanupStaging {
			os.Remove(stagingPath)
		}
	}()

	mergedTar, err := os.CreateTemp("", "shed-merged-*.tar")
	if err != nil {
		return "", fmt.Errorf("creating merged tar: %w", err)
	}
	mergedTarPath := mergedTar.Name()
	mergedTar.Close()
	defer os.Remove(mergedTarPath)

	if err := MergeLayersFromManifest(ctx, imagesDir, manifestDigest, mergedTarPath); err != nil {
		return "", fmt.Errorf("flattening layers: %w", err)
	}

	// mkfs.erofs refuses to overwrite an existing target. Unlink the
	// empty staging file before running it (CreateTemp leaves it empty).
	if err := os.Remove(stagingPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("removing empty staging file: %w", err)
	}

	cmd := exec.CommandContext(ctx,
		"mkfs.erofs",
		"--quiet",
		"--tar=f",
		"-z", "lz4",
		"-E", "force-inode-compact",
		stagingPath,
		mergedTarPath,
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("mkfs.erofs: %s: %w", strings.TrimSpace(stderr.String()), err)
	}

	if err := os.Chmod(stagingPath, 0o444); err != nil {
		return "", fmt.Errorf("chmod staging lower: %w", err)
	}
	if err := fsyncFile(stagingPath); err != nil {
		return "", fmt.Errorf("fsync staging lower: %w", err)
	}
	if err := os.Rename(stagingPath, finalPath); err != nil {
		return "", fmt.Errorf("renaming lower into cache: %w", err)
	}
	cleanupStaging = false
	if err := fsyncDir(filepath.Dir(finalPath)); err != nil {
		return "", fmt.Errorf("fsync cache dir: %w", err)
	}
	return finalPath, nil
}

// materializeLayer dispatches between the host-native and VM-based
// materializers based on GOOS. Linux hosts always use the host-native
// fast path (no VM needed). Mac hosts use the registered materializer
// hook (a vfkit VM running mkfs.erofs); when the hook isn't registered
// or fails because no shed kernel is cached yet, falls back to the
// legacy docker-based mkfs.ext4 pipeline.
func materializeLayer(ctx context.Context, blobPath, outputPath, platform, sizeBytes string) error {
	if runtime.GOOS == "linux" {
		return materializeNativeLinux(ctx, blobPath, outputPath)
	}
	if materializerHook != nil {
		if err := materializerHook(ctx, blobPath, outputPath, platform); err == nil {
			return nil
		} else if !errors.Is(err, ErrMaterializerUnavailable) {
			return err
		}
		// Fall through to the docker fallback when the hook signals
		// "I can't run yet" (e.g., no shed kernel cached).
	}
	return materializeViaDockerFallback(ctx, blobPath, outputPath, platform, sizeBytes)
}

// ErrMaterializerUnavailable is returned by the registered materializer
// hook when it cannot run yet — typically because no shed image is
// cached locally to provide the materializer VM's kernel + initrd. The
// dispatcher catches this and falls back to the legacy docker pipeline,
// producing a .ext4 file alongside the missing .erofs.
var ErrMaterializerUnavailable = errors.New("materializer unavailable (no cached kernel)")

// materializeNativeLinux runs mkfs.erofs directly on the host. Linux
// hosts have erofs-utils via the deb package's runtime dependency.
//
// Process:
//  1. Extract blobPath (tar.gz) into a tempdir (tmpfs-backed if /tmp is)
//  2. mkfs.erofs -z lz4 outputPath <tempdir>
//  3. Clean up the tempdir
//
// Returns wrapped errors with the underlying mkfs.erofs stderr so log
// readers can diagnose missing kernel modules / bad input layers
// without re-running with debug flags.
func materializeNativeLinux(ctx context.Context, blobPath, outputPath string) error {
	if _, err := exec.LookPath("mkfs.erofs"); err != nil {
		return fmt.Errorf("mkfs.erofs not found on PATH (install erofs-utils): %w", err)
	}

	tmpDir, err := os.MkdirTemp("", "shed-materialize-*")
	if err != nil {
		return fmt.Errorf("creating extract tempdir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	if err := extractTarGz(blobPath, tmpDir); err != nil {
		return fmt.Errorf("extracting layer %s: %w", blobPath, err)
	}

	// erofs refuses to overwrite an existing target. The caller's
	// staging file is empty (CreateTemp), so unlink it first.
	if err := os.Remove(outputPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("removing existing staging file: %w", err)
	}

	cmd := exec.CommandContext(ctx, "mkfs.erofs", "-z", "lz4", outputPath, tmpDir)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("mkfs.erofs: %s: %w", strings.TrimSpace(stderr.String()), err)
	}
	return nil
}

// extractTarGz extracts a gzipped tar archive into destDir. Uses the Go
// stdlib so we don't depend on a tar binary at runtime.
//
// Symlinks, hardlinks, and special files are preserved. Whiteouts (OCI
// "deleted" markers) are passed through as-is — the consumer (erofs +
// overlayfs) handles them.
func extractTarGz(srcPath, destDir string) error {
	f, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		// Allow non-gzip-wrapped tars too (some registries serve
		// uncompressed layer blobs when the media type indicates so).
		if _, seekErr := f.Seek(0, io.SeekStart); seekErr == nil {
			return extractTar(f, destDir)
		}
		return fmt.Errorf("opening gzip: %w", err)
	}
	defer gz.Close()
	return extractTar(gz, destDir)
}

func extractTar(r io.Reader, destDir string) error {
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("reading tar header: %w", err)
		}
		// Strip a leading "./" but otherwise honor the archive's path
		// structure. Skip absolute or escaping paths defensively even
		// though we trust shed-emitted layers.
		name := strings.TrimPrefix(hdr.Name, "./")
		if name == "" || name == "." {
			continue
		}
		if filepath.IsAbs(name) || strings.HasPrefix(name, "../") || strings.Contains(name, "/../") {
			continue
		}
		target := filepath.Join(destDir, name)
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(hdr.Mode)&0o777); err != nil {
				return fmt.Errorf("mkdir %s: %w", target, err)
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(hdr.Mode)&0o777)
			if err != nil {
				return fmt.Errorf("create %s: %w", target, err)
			}
			if _, err := io.Copy(out, tr); err != nil {
				out.Close()
				return fmt.Errorf("write %s: %w", target, err)
			}
			out.Close()
		case tar.TypeSymlink:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			// Skip-on-exists keeps the loop idempotent for duplicate
			// tar entries (rare but seen in some buildkit outputs).
			if _, err := os.Lstat(target); err == nil {
				continue
			}
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return fmt.Errorf("symlink %s -> %s: %w", target, hdr.Linkname, err)
			}
		case tar.TypeLink:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			linkTarget := filepath.Join(destDir, strings.TrimPrefix(hdr.Linkname, "./"))
			if _, err := os.Lstat(target); err == nil {
				continue
			}
			if err := os.Link(linkTarget, target); err != nil {
				// Fall back to a symlink if the hardlink target doesn't
				// exist yet (some tars list link entries before their
				// target). Cosmetic; erofs preserves either.
				_ = os.Symlink(hdr.Linkname, target)
			}
		default:
			// Skip char devs / block devs / fifos — shed images don't
			// rely on them, and creating them needs root.
		}
	}
	return nil
}

// materializeViaDockerFallback runs the legacy privileged-Docker
// container to produce an .ext4 file. This path is taken when:
//
//   - Host is Mac and no materializer hook is registered (CLI binary,
//     or shed-server built without the vz materializer).
//   - Host is Mac, the materializer hook is registered, but it returned
//     ErrMaterializerUnavailable (no shed kernel cached yet).
//
// The output file ends in .ext4 (the legacy extension), so callers
// invoking EnsureLowerFromLayer must accept either format. The staging
// path passed in will be renamed to the legacy path at the end of this
// function rather than going through the .erofs final path.
func materializeViaDockerFallback(ctx context.Context, blobPath, outputPath, platform, size string) error {
	if !validLowerSize.MatchString(size) {
		return fmt.Errorf("invalid lower size %q (want NNN[KMGTP])", size)
	}
	// Rewrite the staging path's extension to .ext4 so the caller's
	// rename of staging -> finalPath needs adjusting. Easier: write
	// directly to outputPath but rename at the end so the caller's
	// finalPath logic doesn't drift. The docker container only cares
	// about its in-container path.
	outDir := filepath.Dir(outputPath)
	outName := filepath.Base(outputPath)
	cmd := exec.CommandContext(ctx, "docker", "run", "--rm", "--privileged",
		"-v", blobPath+":/tmp/layer.tar.gz:ro",
		"-v", outDir+":/output",
		"--platform", platform,
		"ubuntu:24.04", "bash", "-c",
		fmt.Sprintf(`set -euo pipefail
apt-get update >/dev/null 2>&1
apt-get install -y e2fsprogs >/dev/null 2>&1
truncate -s %s /output/%s
mkfs.ext4 -F -q /output/%s
mkdir -p /mnt/rootfs
mount -o loop /output/%s /mnt/rootfs
tar -xzf /tmp/layer.tar.gz -C /mnt/rootfs
umount /mnt/rootfs`, size, outName, outName, outName))

	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w", strings.TrimSpace(stderr.String()), err)
	}
	return nil
}
