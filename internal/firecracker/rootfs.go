//go:build linux
// +build linux

package firecracker

import (
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/charliek/shed/internal/vmimage/clone"
)

// RootfsPath returns the path to the rootfs image for an instance.
//
// In the overlay-in-guest model this is a symlink-style alias for the
// upper layer file path: per-shed bookkeeping keeps using "rootfs"
// terminology so existing `shed system df` accounting and prune flows
// don't have to grow a parallel "upper" walker.
func RootfsPath(instanceDir, name string) string {
	return filepath.Join(instanceDir, name, "rootfs.ext4")
}

// UpperPath returns the absolute path of the per-shed writable upper
// layer file under {uppersDir}/{name}/upper.ext4.
func UpperPath(uppersDir, name string) string {
	return filepath.Join(uppersDir, name, "upper.ext4")
}

// UpperDir returns the per-shed upper directory.
func UpperDir(uppersDir, name string) string {
	return filepath.Join(uppersDir, name)
}

// EnsureUpper creates the per-shed writable upper layer as a sparse
// ext4-sized file at {uppersDir}/<name>/upper.ext4. The file is left
// unformatted; the in-guest initramfs runs mkfs.ext4 on first boot.
//
// Fails with an explicit error when the upper file already exists
// rather than silently reusing it: a stale upper from a previously
// crashed `shed create` (or from manual operator intervention) almost
// always reflects state the next caller doesn't intend to inherit.
// Callers that want fresh-state semantics (e.g. `shed reset`) call
// DeleteUpper first.
func EnsureUpper(uppersDir, name string, sizeBytes int64) (string, error) {
	if sizeBytes <= 0 {
		return "", fmt.Errorf("upper size must be positive (got %d)", sizeBytes)
	}
	dir := UpperDir(uppersDir, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("failed to create upper directory: %w", err)
	}
	path := UpperPath(uppersDir, name)

	// O_CREATE|O_EXCL guarantees we never silently reuse a stale upper
	// from a previously failed create.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o644)
	if err != nil {
		if os.IsExist(err) {
			return "", fmt.Errorf("upper already exists at %s; remove it (or run `shed reset <name>`) before recreating", path)
		}
		return "", fmt.Errorf("failed to create upper file: %w", err)
	}
	if err := f.Truncate(sizeBytes); err != nil {
		f.Close()
		os.Remove(path)
		return "", fmt.Errorf("failed to size upper file: %w", err)
	}
	// syncDir alone only persists the directory entry; the truncate
	// metadata still needs a file-level sync, or a crash right after
	// EnsureUpper can leave a zero-length upper.ext4 with a tag-good
	// metadata pointer at it.
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(path)
		return "", fmt.Errorf("failed to sync upper file: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return "", fmt.Errorf("failed to close upper file: %w", err)
	}
	if err := syncDir(dir); err != nil {
		return "", fmt.Errorf("failed to sync upper directory: %w", err)
	}
	log.Printf("upper created path=%s size_bytes=%d", path, sizeBytes)
	return path, nil
}

// DeleteUpper removes the per-shed upper directory and its contents.
// Used by `shed delete` and `shed reset`.
func DeleteUpper(uppersDir, name string) error {
	if err := os.RemoveAll(UpperDir(uppersDir, name)); err != nil {
		return fmt.Errorf("failed to remove upper dir: %w", err)
	}
	return nil
}

// CopyRootfs copies the base rootfs image to the instance directory,
// preferring FICLONE (reflink) then copy_file_range and falling back to
// io.Copy on older kernels or non-reflink filesystems. See the clone
// package for strategy precedence.
//
// Both FICLONE and copy_file_range require dst to not already exist;
// pre-clean any stale dst before invoking the chain.
//
// CONCURRENCY: single-writer-per-shed-name is enforced upstream by
// Client.acquireCreateLock, which wraps the whole CreateShed flow. The
// unconditional os.Remove(dst) below is therefore safe against racing
// `shed create` calls for the same name.
func CopyRootfs(baseRootfs, instanceDir, name string) (string, error) {
	dst := RootfsPath(instanceDir, name)

	dir := InstanceDir(instanceDir, name)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", fmt.Errorf("failed to create instance directory: %w", err)
	}

	if err := os.Remove(dst); err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("failed to clean stale rootfs: %w", err)
	}
	if err := syncDir(dir); err != nil {
		return "", fmt.Errorf("failed to sync instance directory after cleanup: %w", err)
	}

	strategy, err := clone.CloneFile(baseRootfs, dst)
	if err != nil {
		_ = os.Remove(dst)
		return "", fmt.Errorf("failed to copy rootfs: %w", err)
	}

	// Force dst to 0o644. FICLONE / copy_file_range / io.Copy on linux
	// already create dst at 0o644, so this is normally a no-op — but a
	// future strategy that preserves source mode would silently leave a
	// 0o444 instance rootfs after spawn-from-snapshot, breaking the VM.
	// Mirrors the same chmod in vz/rootfs.go for cross-backend symmetry.
	if err := os.Chmod(dst, 0o644); err != nil {
		_ = os.Remove(dst)
		return "", fmt.Errorf("failed to chmod rootfs: %w", err)
	}

	var logical int64
	if fi, statErr := os.Stat(dst); statErr == nil {
		logical = fi.Size()
	}
	log.Printf("rootfs strategy=%s src=%s dst=%s logical_bytes=%d", strategy, baseRootfs, dst, logical)

	// Sync on every path so crash recovery semantics stay uniform, and
	// surface delayed-writeback errors (ENOSPC, EIO) rather than silently
	// booting a VM on broken storage.
	f, err := os.OpenFile(dst, os.O_RDWR, 0)
	if err != nil {
		_ = os.Remove(dst)
		return "", fmt.Errorf("failed to reopen rootfs for sync: %w", err)
	}
	if syncErr := f.Sync(); syncErr != nil {
		f.Close()
		_ = os.Remove(dst)
		return "", fmt.Errorf("failed to sync rootfs: %w", syncErr)
	}
	if closeErr := f.Close(); closeErr != nil {
		_ = os.Remove(dst)
		return "", fmt.Errorf("failed to close rootfs after sync: %w", closeErr)
	}
	if err := syncDir(dir); err != nil {
		_ = os.Remove(dst)
		return "", fmt.Errorf("failed to sync instance directory: %w", err)
	}

	return dst, nil
}

// syncDir fsyncs a directory so pending create/unlink metadata is on
// stable storage. See the matching helper in internal/vz/rootfs.go.
func syncDir(path string) error {
	d, err := os.Open(path)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// DeleteRootfs removes the rootfs image for an instance.
func DeleteRootfs(instanceDir, name string) error {
	path := RootfsPath(instanceDir, name)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove rootfs: %w", err)
	}
	return nil
}

// RootfsExists checks if the rootfs image exists for an instance.
func RootfsExists(instanceDir, name string) bool {
	path := RootfsPath(instanceDir, name)
	_, err := os.Stat(path)
	return err == nil
}
