// Derived ext4 cache for OCI layer blobs.
//
// OCI layers are tar.gz; shed boots from ext4 block devices. The cache
// holds one ext4 per layer digest at {imagesDir}/cache/sha256/<hex>.ext4.
// Cache files are derived: they can be regenerated from the layer blob
// at any time. Future work (see docs/discovery/layer-storage-optimization.md)
// adds eviction policies for layers not currently in use.

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
)

const cacheDir = "cache"

// DefaultLayerExt4Size is the sparse size used for derived ext4 files.
// Identical to the legacy single-image size; tiny layers waste some
// blocks but the file is sparse so on-disk usage tracks content.
const DefaultLayerExt4Size = "20G"

// CacheExt4Path returns the on-disk path of the derived ext4 for a layer.
// layerDigest must be of the form "sha256:<hex>".
func CacheExt4Path(imagesDir, layerDigest string) (string, error) {
	hex, err := digestHex(layerDigest)
	if err != nil {
		return "", err
	}
	return filepath.Join(imagesDir, cacheDir, algorithmDir, hex+".ext4"), nil
}

// CacheExt4Exists reports whether the derived ext4 for a layer is present.
func CacheExt4Exists(imagesDir, layerDigest string) bool {
	path, err := CacheExt4Path(imagesDir, layerDigest)
	if err != nil {
		return false
	}
	if _, err := os.Stat(path); err != nil {
		return false
	}
	return true
}

// CacheExt4Size returns the on-disk size of a cached ext4 file, or 0
// if absent.
func CacheExt4Size(imagesDir, layerDigest string) int64 {
	path, err := CacheExt4Path(imagesDir, layerDigest)
	if err != nil {
		return 0
	}
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return fi.Size()
}

// RemoveCachedExt4 evicts a derived ext4 file. Idempotent: a missing
// file is not an error.
func RemoveCachedExt4(imagesDir, layerDigest string) error {
	path, err := CacheExt4Path(imagesDir, layerDigest)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("removing cached ext4: %w", err)
	}
	return nil
}

// EnsureExt4FromLayer materializes (or re-materializes) the ext4 for a
// layer blob. If the cache file already exists, returns its path without
// rebuilding. Otherwise reads the layer blob (a tar.gz), runs the
// existing privileged-Docker mkfs.ext4 pipeline, and installs the
// result atomically into the cache.
//
// platform should be the Docker platform string ("linux/arm64" or
// "linux/amd64") matching the layer; sizeBytes controls the sparse ext4
// size, defaulting to DefaultLayerExt4Size when empty.
func EnsureExt4FromLayer(ctx context.Context, imagesDir, layerDigest, platform, sizeBytes string) (string, error) {
	if err := EnsureOCILayout(imagesDir); err != nil {
		return "", err
	}
	if sizeBytes == "" {
		sizeBytes = DefaultLayerExt4Size
	}

	finalPath, err := CacheExt4Path(imagesDir, layerDigest)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(finalPath); err == nil {
		return finalPath, nil
	}
	if !BlobExists(imagesDir, layerDigest) {
		return "", fmt.Errorf("%w: layer %s", ErrBlobNotFound, layerDigest)
	}

	hex, _ := digestHex(layerDigest)
	lockPath := filepath.Join(imagesDir, cacheDir, algorithmDir, "."+hex+".ext4.lock")
	unlock, err := acquireFileLock(lockPath)
	if err != nil {
		return "", fmt.Errorf("locking cache ext4 %s: %w", layerDigest, err)
	}
	defer unlock()

	if _, err := os.Stat(finalPath); err == nil {
		return finalPath, nil
	}

	blobPath, err := BlobPath(imagesDir, layerDigest)
	if err != nil {
		return "", err
	}

	stagingFile, err := os.CreateTemp(filepath.Dir(finalPath), "."+hex+".*.ext4.tmp")
	if err != nil {
		return "", fmt.Errorf("creating staging ext4: %w", err)
	}
	stagingPath := stagingFile.Name()
	stagingFile.Close()
	cleanupStaging := true
	defer func() {
		if cleanupStaging {
			os.Remove(stagingPath)
		}
	}()

	if err := createExt4FromTarGz(ctx, platform, blobPath, stagingPath, sizeBytes); err != nil {
		return "", fmt.Errorf("creating ext4 from layer: %w", err)
	}
	if err := os.Chmod(stagingPath, 0o444); err != nil {
		return "", fmt.Errorf("chmod staging ext4: %w", err)
	}
	if err := fsyncFile(stagingPath); err != nil {
		return "", fmt.Errorf("fsync staging ext4: %w", err)
	}
	if err := os.Rename(stagingPath, finalPath); err != nil {
		return "", fmt.Errorf("renaming ext4 into cache: %w", err)
	}
	cleanupStaging = false
	if err := fsyncDir(filepath.Dir(finalPath)); err != nil {
		return "", fmt.Errorf("fsync cache dir: %w", err)
	}
	return finalPath, nil
}

// createExt4FromTarGz runs a privileged Docker container to:
//  1. Create a sparse ext4 file
//  2. Mount it via loop
//  3. Extract the layer tar.gz into it
//  4. Unmount
//
// blobPath must be a gzipped tar produced by `Convert`'s layer writer.
// outputPath is where the resulting ext4 file should be written.
func createExt4FromTarGz(ctx context.Context, platform, blobPath, outputPath, size string) error {
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
