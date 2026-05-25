// Legacy lower-image cache helpers.
//
// v0.5.1 and earlier materialized the read-only rootfs erofs on the
// host into {imagesDir}/cache/sha256/<manifest-digest>.erofs via
// `mkfs.erofs --tar=f`. v0.5.2 moves that step to the image
// producer; the erofs ships as a content-addressed OCI blob, hosts
// just download it (see internal/vmimage/erofs.go).
//
// What survives in this file:
//
//   - The `cacheDir` / `CacheLowerExt` constants plus
//     `CacheLowerPath` and `CacheLowerSize`. `PruneImages` uses
//     them to sweep stale v0.5.1-era cache files left over from
//     an upgrade. On a fresh v0.5.2+ install they always report
//     "no cache file" and contribute zero to ListImages's sizes.
//
// On v0.5.3+ when no installation in the wild still has legacy
// cache files, these helpers can be removed entirely.

package vmimage

import (
	"os"
	"path/filepath"
	"syscall"
)

const cacheDir = "cache"

// CacheLowerExt is the file extension used by v0.5.1's derived
// lower-image cache. Kept so `PruneImages` can recognize and sweep
// stale entries during the post-upgrade window.
const CacheLowerExt = ".erofs"

// CacheLowerPath returns the on-disk path a v0.5.1 install would
// have used for a manifest's flattened lower. manifestDigest must be
// of the form "sha256:<hex>". v0.5.2+ never writes to this path.
func CacheLowerPath(imagesDir, manifestDigest string) (string, error) {
	hex, err := digestHex(manifestDigest)
	if err != nil {
		return "", err
	}
	return filepath.Join(imagesDir, cacheDir, algorithmDir, hex+CacheLowerExt), nil
}

// CacheLowerSize returns the on-disk size of the legacy cache file
// for a manifest, or 0 if absent (the common case post-v0.5.2).
// Reports actual allocated blocks (st_blocks × 512), not the sparse
// file logical length, so `shed image ls` SIZE reads true on-disk
// usage during the upgrade window.
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
