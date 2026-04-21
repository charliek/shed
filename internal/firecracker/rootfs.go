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
func RootfsPath(instanceDir, name string) string {
	return filepath.Join(instanceDir, name, "rootfs.ext4")
}

// CopyRootfs copies the base rootfs image to the instance directory,
// preferring FICLONE (reflink) then copy_file_range and falling back to
// io.Copy on older kernels or non-reflink filesystems. See the clone
// package for strategy precedence.
//
// Both FICLONE and copy_file_range require dst to not already exist;
// pre-clean any stale dst before invoking the chain.
//
// CONCURRENCY: single-writer-per-shed-name is assumed. See the matching
// comment in internal/vz/rootfs.go for details on the TOCTOU window.
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
