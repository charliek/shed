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
	"strings"
	"syscall"
)

const cacheDir = "cache"

// CacheLowerExt is the file extension used for derived lower images in
// the cache directory. The erofs format gives lz4 random-read
// compression and ~50% on-disk savings vs ext4. The extension is part
// of the on-disk filename only; callers should treat the path returned
// by CacheLowerPath as an opaque value.
const CacheLowerExt = ".erofs"

// CacheLowerPath returns the on-disk path of the derived lower image
// for a manifest. manifestDigest must be of the form "sha256:<hex>".
func CacheLowerPath(imagesDir, manifestDigest string) (string, error) {
	hex, err := digestHex(manifestDigest)
	if err != nil {
		return "", err
	}
	return filepath.Join(imagesDir, cacheDir, algorithmDir, hex+CacheLowerExt), nil
}

// CacheLowerExists reports whether the derived lower image is present.
func CacheLowerExists(imagesDir, manifestDigest string) bool {
	path, err := CacheLowerPath(imagesDir, manifestDigest)
	if err != nil {
		return false
	}
	_, err = os.Stat(path)
	return err == nil
}

// CacheLowerSize returns the on-disk size of the cached lower image
// for a manifest, or 0 if absent. Reports actual allocated blocks
// (st_blocks × 512), not the sparse-file logical length, so the
// `shed image ls` SIZE column reads true on-disk usage.
func CacheLowerSize(imagesDir, manifestDigest string) int64 {
	path, err := CacheLowerPath(imagesDir, manifestDigest)
	if err != nil {
		return 0
	}
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return st.Blocks * 512
	}
	return fi.Size()
}

// RemoveCachedLower evicts the derived lower image for a manifest.
// Idempotent: a missing file is not an error.
func RemoveCachedLower(imagesDir, manifestDigest string) error {
	path, err := CacheLowerPath(imagesDir, manifestDigest)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("removing cached lower: %w", err)
	}
	return nil
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
		// -b 4096 pins the erofs block size to 4 KiB regardless of the
		// host's page size. mkfs.erofs defaults to host page size, which
		// on Apple Silicon Macs is 16 KiB; mounting that image inside the
		// Linux guest (4-KiB page size) panics with
		// "erofs_read_superblock: blkszbits 14 isn't supported". 4 KiB
		// matches what the guest kernel expects on both Linux and macOS.
		"-b", "4096",
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

