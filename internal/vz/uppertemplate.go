//go:build darwin
// +build darwin

package vz

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

	"github.com/charliek/shed/internal/version"
	"github.com/charliek/shed/internal/vmimage/clone"
)

// Upper-template provisioning.
//
// A freshly-created shed's writable upper (/dev/vda) is an empty ext4
// filesystem. Historically the initramfs formatted it with mkfs.ext4 on
// first boot — but on the raw virtio-blk device under VZ that costs
// ~4 s (journal + superblock writes), dominating an otherwise ~2 s boot.
//
// Instead we keep a single pre-formatted ext4 image per upper size on the
// host and copy-on-write clone it into each new shed's upper. The clone
// already carries the ext4 superblock, so the initramfs detects the magic
// and mounts it directly, skipping mkfs entirely. clone.CloneFile uses
// APFS clonefile / Linux reflink, so the clone is near-instant and shares
// extents (the template is sparse — a few MB on disk).
//
// macOS has no native mkfs.ext4, so the template is minted by mkfs.ext4
// inside the shed-build-tools container (the same image that already runs
// mkfs.erofs at publish time). Everything here is best-effort: any failure
// returns an error and the caller falls back to the in-guest mkfs path, so
// shed creation never regresses.

const upperTemplateLabel = "shed-upper"

// templatesDir is where pre-formatted upper templates are cached, a
// sibling of the per-shed uppers directory (e.g. .../vz/templates).
func (c *Client) templatesDir() string {
	return filepath.Join(filepath.Dir(c.cfg.UppersDir), "templates")
}

// resolveBuildToolsRef returns the shed-build-tools image ref used to run
// mkfs.ext4 for upper templates. SHED_BUILD_TOOLS_REF overrides (e.g.
// "shed-build-tools:dev" for local iteration). Otherwise it derives from
// the server version, matching the image-build pipeline's pinning model.
// Dev/dirty builds have no matching published tag, so they return "" —
// the caller then falls back to in-guest mkfs.
func resolveBuildToolsRef() string {
	if ref := os.Getenv("SHED_BUILD_TOOLS_REF"); ref != "" {
		return ref
	}
	v := version.Version
	if v == "" || v == "dev" || strings.Contains(v, "-g") || strings.Contains(v, "-dirty") {
		return ""
	}
	return "ghcr.io/charliek/shed-build-tools:" + v
}

// upperTemplatePath is the cached template image for a given upper size.
// Templates are keyed by exact byte size since a clone inherits the
// template's size verbatim.
func upperTemplatePath(templatesDir string, sizeBytes int64) string {
	return filepath.Join(templatesDir, fmt.Sprintf("ext4-%d.img", sizeBytes))
}

// EnsureUpperTemplate returns the path to a cached, pre-formatted ext4
// image of exactly sizeBytes, minting it on first use via mkfs.ext4 in the
// build-tools container. Returns ("", err) when no template can be
// produced (no build-tools ref, docker missing, mkfs failure); callers
// fall back to allocating an empty upper and formatting it in the guest.
func EnsureUpperTemplate(ctx context.Context, templatesDir, buildToolsRef string, sizeBytes int64, docker string) (string, error) {
	if buildToolsRef == "" {
		return "", errors.New("no shed-build-tools ref configured (dev build or unset SHED_BUILD_TOOLS_REF)")
	}
	if sizeBytes <= 0 {
		return "", fmt.Errorf("invalid template size %d", sizeBytes)
	}
	if docker == "" {
		docker = "docker"
	}
	if _, err := exec.LookPath(docker); err != nil {
		return "", fmt.Errorf("%s not on PATH: %w", docker, err)
	}
	if err := os.MkdirAll(templatesDir, 0o755); err != nil {
		return "", fmt.Errorf("creating templates dir: %w", err)
	}

	tmplPath := upperTemplatePath(templatesDir, sizeBytes)
	if validTemplate(tmplPath, sizeBytes) {
		return tmplPath, nil
	}

	// Serialize concurrent minting across create calls of the same size.
	unlock, err := lockTemplate(templatesDir, sizeBytes)
	if err != nil {
		return "", err
	}
	defer unlock()
	if validTemplate(tmplPath, sizeBytes) { // re-check under lock
		return tmplPath, nil
	}

	staging, err := os.CreateTemp(templatesDir, ".ext4-*.tmp")
	if err != nil {
		return "", fmt.Errorf("creating staging file: %w", err)
	}
	stagingPath := staging.Name()
	_ = staging.Close()
	defer os.Remove(stagingPath) // no-op once renamed into place

	if err := os.Truncate(stagingPath, sizeBytes); err != nil {
		return "", fmt.Errorf("sizing staging file: %w", err)
	}

	// mkfs.ext4 in the build-tools container. lazy_itable_init +
	// lazy_journal_init + nodiscard keep generation fast and the template
	// sparse (a few MB); the inode tables and journal initialize lazily
	// inside the guest, off the boot critical path.
	dir := filepath.Dir(stagingPath)
	base := filepath.Base(stagingPath)
	args := []string{
		"run", "--rm",
		"-u", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()),
		"-v", dir + ":/shed/work",
		"-w", "/shed/work",
		"--entrypoint", "mkfs.ext4",
		buildToolsRef,
		"-F", "-q",
		"-L", upperTemplateLabel,
		"-E", "lazy_itable_init=1,lazy_journal_init=1,nodiscard",
		"/shed/work/" + base,
	}
	cmd := exec.CommandContext(ctx, docker, args...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(out.String())
		if msg == "" {
			msg = "(no output)"
		}
		return "", fmt.Errorf("mkfs.ext4 via %s %s: %w\n%s", docker, buildToolsRef, err, msg)
	}
	if !hasExt4Magic(stagingPath) {
		return "", errors.New("minted template lacks ext4 magic")
	}
	if err := os.Chmod(stagingPath, 0o444); err != nil {
		return "", fmt.Errorf("chmod template: %w", err)
	}
	if err := os.Rename(stagingPath, tmplPath); err != nil {
		return "", fmt.Errorf("installing template: %w", err)
	}
	return tmplPath, nil
}

// provisionUpperFromTemplate replaces the freshly-allocated signature
// upper at upperPath with a copy-on-write clone of templatePath. The
// cloned upper carries the ext4 superblock, so the initramfs mounts it
// directly instead of running mkfs.ext4. The clone goes to a temp sibling
// first so a failure leaves the original signature upper intact for the
// in-guest-mkfs fallback.
func provisionUpperFromTemplate(upperPath, templatePath string) error {
	tmp := upperPath + ".tmpl"
	_ = os.Remove(tmp) // clone.CloneFile requires dst to be absent
	if _, err := clone.CloneFile(templatePath, tmp); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("cloning template: %w", err)
	}
	if err := os.Chmod(tmp, 0o644); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("chmod cloned upper: %w", err)
	}
	if err := os.Rename(tmp, upperPath); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("installing cloned upper: %w", err)
	}
	return nil
}

// validTemplate reports whether path is a usable template: present, the
// expected size, and carrying the ext4 magic.
func validTemplate(path string, sizeBytes int64) bool {
	fi, err := os.Stat(path)
	if err != nil || fi.Size() != sizeBytes {
		return false
	}
	return hasExt4Magic(path)
}

// hasExt4Magic reports whether path carries the ext4 superblock magic
// (0x53 0xEF, little-endian s_magic) at offset 1080 — the same marker the
// initramfs checks to decide whether the upper needs formatting.
func hasExt4Magic(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	b := make([]byte, 2)
	if _, err := f.ReadAt(b, 1080); err != nil {
		return false
	}
	return b[0] == 0x53 && b[1] == 0xEF
}

// lockTemplate takes an exclusive flock on a per-size lock file so two
// concurrent creates don't mint the same template at once. Returns an
// unlock func.
func lockTemplate(templatesDir string, sizeBytes int64) (func(), error) {
	lockPath := filepath.Join(templatesDir, fmt.Sprintf(".ext4-%d.lock", sizeBytes))
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("opening template lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("locking template: %w", err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}
