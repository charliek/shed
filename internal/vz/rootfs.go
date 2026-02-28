//go:build darwin
// +build darwin

package vz

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// RootfsPath returns the path to the rootfs image for an instance.
func RootfsPath(instanceDir, name string) string {
	return filepath.Join(instanceDir, name, "rootfs.ext4")
}

// CopyRootfs copies the base rootfs image to the instance directory.
func CopyRootfs(baseRootfs, instanceDir, name string) (string, error) {
	dst := RootfsPath(instanceDir, name)

	// Ensure instance directory exists
	dir := InstanceDir(instanceDir, name)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", fmt.Errorf("failed to create instance directory: %w", err)
	}

	// Open source file
	src, err := os.Open(baseRootfs)
	if err != nil {
		return "", fmt.Errorf("failed to open base rootfs: %w", err)
	}
	defer src.Close()

	// Create destination file
	dstFile, err := os.Create(dst)
	if err != nil {
		return "", fmt.Errorf("failed to create rootfs copy: %w", err)
	}
	defer dstFile.Close()

	// Copy contents
	if _, err := io.Copy(dstFile, src); err != nil {
		os.Remove(dst) // Clean up on failure
		return "", fmt.Errorf("failed to copy rootfs: %w", err)
	}

	// Sync to ensure data is written
	if err := dstFile.Sync(); err != nil {
		os.Remove(dst)
		return "", fmt.Errorf("failed to sync rootfs: %w", err)
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
