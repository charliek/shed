//go:build darwin
// +build darwin

package vz

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
// preferring reflink/clonefile when available so `shed create` is near-
// instant and near-zero physical cost. Falls back to io.Copy on older
// kernels or non-reflink filesystems.
//
// Pre-clean a stale dst: the kernel primitives (Clonefile / FICLONE)
// both require dst to not exist. A prior failed create may have left
// rootfs.ext4 behind; remove it first (ignoring "already gone").
func CopyRootfs(baseRootfs, instanceDir, name string) (string, error) {
	dst := RootfsPath(instanceDir, name)

	dir := InstanceDir(instanceDir, name)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", fmt.Errorf("failed to create instance directory: %w", err)
	}

	if err := os.Remove(dst); err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("failed to clean stale rootfs: %w", err)
	}

	strategy, err := clone.CloneFile(baseRootfs, dst)
	if err != nil {
		_ = os.Remove(dst)
		return "", fmt.Errorf("failed to copy rootfs: %w", err)
	}

	// Stable log line for operators: "rootfs strategy=clonefile src=... dst=... logical_bytes=..."
	var logical int64
	if fi, statErr := os.Stat(dst); statErr == nil {
		logical = fi.Size()
	}
	log.Printf("rootfs strategy=%s src=%s dst=%s logical_bytes=%d", strategy, baseRootfs, dst, logical)

	// Sync on every path. Clonefile/FICLONE don't guarantee metadata
	// durability until the next FS commit, and the negligible cost on a
	// cloned inode isn't worth the "did the next create-then-crash lose
	// the instance?" hazard.
	f, err := os.OpenFile(dst, os.O_RDWR, 0)
	if err == nil {
		_ = f.Sync()
		f.Close()
	}

	return dst, nil
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
