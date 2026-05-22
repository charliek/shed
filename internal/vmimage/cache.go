// Derived read-only lower-image cache, keyed by OCI manifest digest.
//
// `EnsureLowerFromManifest` flattens an OCI manifest's layer blobs into
// a single content-addressed erofs file at
// {imagesDir}/cache/sha256/<manifest-digest>.erofs, shared across every
// shed that boots from this manifest. Cache files are derived — they
// can be regenerated from the layer blobs at any time.
//
// The materialize path is host-native on both Linux and macOS via
// `mkfs.erofs --tar=f`. erofs-utils is a runtime dependency
// (`apt install erofs-utils` on Debian/Ubuntu, `brew install
// erofs-utils` on macOS).

package vmimage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
)

// validLowerSize is retained for input validation against the size
// hint accepted by legacy callers. The new materialize pipeline ignores
// size (erofs is tightly-packed), but config validation still uses this
// regex to catch typos in user-supplied size strings.
var validLowerSize = regexp.MustCompile(`^[1-9][0-9]*[KMGTP]?$`)

const cacheDir = "cache"

// CacheLowerExt is the file extension used for derived lower images in
// the cache directory. The erofs format gives lz4 random-read
// compression and ~50% on-disk savings vs ext4. The extension is part
// of the on-disk filename only; callers should treat the path returned
// by CacheLowerPath as an opaque value.
const CacheLowerExt = ".erofs"

// LegacyCacheLowerExt is the previous extension (pre-v0.5.1). Kept as
// a constant so PruneImages's GC scan can recognize and evict legacy
// .ext4 files left over from a v0.5.0 cache (B.5 deletes this entirely).
const LegacyCacheLowerExt = ".ext4"

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

