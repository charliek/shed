//go:build darwin
// +build darwin

package vz

import (
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/charliek/shed/internal/systemprune"
)

// RootfsPath returns the path to the rootfs image for an instance.
//
// In the overlay-in-guest model this is a per-shed alias for the upper
// layer file path: per-shed bookkeeping keeps using "rootfs"
// terminology so existing `shed system df` accounting and prune flows
// don't have to grow a parallel "upper" walker.
func RootfsPath(instanceDir, name string) string {
	return filepath.Join(instanceDir, name, "rootfs.ext4")
}

// UpperPath returns the absolute path of the per-shed writable upper
// at {uppersDir}/{name}/upper.ext4.
func UpperPath(uppersDir, name string) string {
	return filepath.Join(uppersDir, name, "upper.ext4")
}

// UpperDir returns the per-shed upper directory.
func UpperDir(uppersDir, name string) string {
	return filepath.Join(uppersDir, name)
}

// EnsureUpper creates the per-shed writable upper as a sparse,
// unformatted file at {uppersDir}/<name>/upper.ext4. The in-guest
// initramfs runs mkfs.ext4 on first boot — it tells "needs mkfs" from
// "corrupted ext4" by reading the 16-byte FreshUpperSignature this
// function stamps at offset FreshUpperSignatureOffset (1024, the
// start of the ext4 superblock region). mkfs.ext4 overwrites that
// region with a real superblock on first boot, so the signature is
// only present on a freshly-allocated upper. See FreshUpperSignature
// for details.
//
// Fails with an explicit error when the upper file already exists rather
// than silently reusing it: a stale upper from a previously crashed
// `shed create` almost always reflects state the next caller doesn't
// intend to inherit. Callers that want fresh-state semantics (e.g.
// `shed reset`) call DeleteUpper first.
func EnsureUpper(uppersDir, name string, sizeBytes int64) (string, error) {
	if sizeBytes <= 0 {
		return "", fmt.Errorf("upper size must be positive (got %d)", sizeBytes)
	}
	dir := UpperDir(uppersDir, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("failed to create upper directory: %w", err)
	}
	path := UpperPath(uppersDir, name)

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
	if _, err := f.WriteAt([]byte(FreshUpperSignature), FreshUpperSignatureOffset); err != nil {
		f.Close()
		os.Remove(path)
		return "", fmt.Errorf("failed to write fresh-upper signature: %w", err)
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

// FreshUpperSignature is written into the upper file's ext4 superblock
// region (offset 1024, length 16) at EnsureUpper time. The initramfs
// uses this to distinguish "freshly allocated, never formatted" from
// "ext4 superblock corrupted, do not reformat — panic instead." Two
// invariants make this signature reliable:
//
//   - mkfs.ext4 writes a real superblock at offset 1024 on first
//     boot, destroying the signature. So a signature present means
//     the upper has *never* been successfully formatted by the
//     in-guest initramfs.
//   - The signature's first two bytes (0x53 0x48 / "SH") are not the
//     ext4 superblock magic (0x53 0xEF at offset 1080). The two byte
//     ranges overlap at byte 1080-1081 of the signature ("SH-UPPER"
//     starts at offset 0 within the signature, byte 56-57 from
//     offset 1024 — but the signature is only 16 bytes long, so this
//     is moot). Magic-byte and signature checks are independent.
//
// A zeroed first MiB (the corruption case Phase 3.6 caught) loses
// both the signature and the ext4 magic, and the initramfs panics
// with the `shed reset` hint instead of silently reformatting.
const (
	FreshUpperSignature       = "SHED-FRESH-UPPER"
	FreshUpperSignatureOffset = 1024
)

// DeleteUpper removes the per-shed upper directory.
func DeleteUpper(uppersDir, name string) error {
	if err := os.RemoveAll(UpperDir(uppersDir, name)); err != nil {
		return fmt.Errorf("failed to remove upper dir: %w", err)
	}
	return nil
}

// syncDir fsyncs a directory so the pending create/unlink metadata is
// actually on stable storage. Cheap on macOS/APFS, cheap on ext4 with
// journal=ordered; never wrong to do on the crash-recovery path.
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

// writeCreatingMarker drops a `.creating` marker into the instance
// directory containing the lower digest the in-flight create is
// about to use. The refscanner (systemprune.ScanInstanceCreatingMarkers)
// reads this body as a protective reference so a racing prune can't
// delete the blob between EnsureImage and meta.Save.
//
// The marker is fsync'd along with its parent dir so a crash right
// after this returns cannot lose the protection. removeCreatingMarker
// is called via defer on every CreateShed exit path.
func writeCreatingMarker(instanceDir, name, lowerDigest string) error {
	dir := InstanceDir(instanceDir, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating instance dir for marker: %w", err)
	}
	path := filepath.Join(dir, systemprune.InstanceCreatingMarker)
	if err := os.WriteFile(path, []byte(lowerDigest), 0o600); err != nil {
		return fmt.Errorf("writing creating marker: %w", err)
	}
	if f, err := os.Open(path); err == nil {
		_ = f.Sync()
		_ = f.Close()
	}
	if err := syncDir(dir); err != nil {
		return fmt.Errorf("syncing instance dir after marker write: %w", err)
	}
	return nil
}

// removeCreatingMarker deletes the `.creating` marker (if present).
// Safe to call when no marker exists — used as a defer in CreateShed.
func removeCreatingMarker(instanceDir, name string) {
	path := filepath.Join(InstanceDir(instanceDir, name), systemprune.InstanceCreatingMarker)
	_ = os.Remove(path)
}
