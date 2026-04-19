// Package vmimage provides Docker-to-ext4 image conversion for VM backends.
// This package is cross-platform (no build tags) so it can be tested on Linux CI.
// Both VZ and Firecracker backends use ext4 rootfs images and can share this pipeline.
package vmimage

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/distribution/reference"
)

// DefaultPlatform is the Docker platform used for VZ images (Apple Silicon).
const DefaultPlatform = "linux/arm64"

// FirecrackerPlatform is the Docker platform used for Firecracker images (x86_64 Linux).
const FirecrackerPlatform = "linux/amd64"

// ConvertOptions configures a Docker-to-ext4 conversion.
type ConvertOptions struct {
	// DockerRef is the Docker image reference to convert (e.g., "ghcr.io/charliek/shed-vz-default:v1.0.0").
	DockerRef string

	// Name is the variant name used for output file naming (e.g., "default" → "default-rootfs.ext4").
	Name string

	// OutputDir is the directory where the ext4 file will be written.
	OutputDir string

	// RootfsSize is the sparse ext4 image size (e.g., "20G"). Defaults to "20G".
	RootfsSize string

	// Platform is the Docker platform (e.g., "linux/arm64"). Defaults to DefaultPlatform.
	Platform string

	// ExtractKernel controls whether the kernel should be extracted from the Docker image.
	// If true, the kernel is always extracted to OutputDir, overwriting any existing file.
	ExtractKernel bool

	// NeedsInitrd controls whether an initrd should be extracted alongside the kernel.
	// Only consulted when ExtractKernel is true.
	// True for VZ (requires initrd for LinuxBootloader), false for Firecracker (boots directly).
	NeedsInitrd bool
}

// ConvertResult holds the output paths from a successful conversion.
type ConvertResult struct {
	// RootfsPath is the path to the created ext4 image.
	RootfsPath string

	// KernelPath is the path to the extracted kernel (empty if not extracted).
	KernelPath string

	// InitrdPath is the path to the extracted initrd (empty if not extracted).
	InitrdPath string
}

// IsDockerRef returns true if s is a Docker image reference rather than a filesystem path.
// It uses the OCI distribution reference parser for accurate detection.
func IsDockerRef(s string) bool {
	if s == "" {
		return false
	}
	// Filesystem paths are not Docker refs
	if strings.HasPrefix(s, "/") || strings.HasPrefix(s, "~") || strings.HasPrefix(s, ".") {
		return false
	}
	// Try parsing as a Docker reference
	_, err := reference.ParseNormalizedNamed(s)
	return err == nil
}

// RootfsFilename returns the ext4 filename for a given variant name.
func RootfsFilename(name string) string {
	return name + "-rootfs.ext4"
}

// SourceFilename returns the source sidecar filename for cache invalidation.
func SourceFilename(name string) string {
	return name + "-rootfs.ext4.source"
}

// CheckCache returns the cached rootfs path if it exists and its source sidecar
// matches expectedRef. Returns "" if not cached or stale.
func CheckCache(imagesDir, name, expectedRef string) string {
	rootfsPath := filepath.Join(imagesDir, RootfsFilename(name))
	if _, err := os.Stat(rootfsPath); err != nil {
		return ""
	}
	sourceFile := filepath.Join(imagesDir, SourceFilename(name))
	source, err := os.ReadFile(sourceFile)
	if err != nil || strings.TrimSpace(string(source)) != expectedRef {
		return ""
	}
	return rootfsPath
}

// WriteSource writes the Docker ref to a sidecar file for cache invalidation tracking.
func WriteSource(imagesDir, name, ref string) error {
	sourceFile := filepath.Join(imagesDir, SourceFilename(name))
	return os.WriteFile(sourceFile, []byte(ref+"\n"), 0644)
}

// sweepStaleTmp removes orphan tmp files for a single target produced by
// a prior crashed LinkCachedImage call. It matches only:
//   - {base}.tmp              — the current fixed tmp name.
//   - {base}.tmp.<digits>     — the legacy PID-suffixed tmp name from
//     pre-v0.3.6 builds (fmt.Sprintf("%s.tmp.%d", ...)).
//
// Any other suffix (e.g. .keep, .bak, .tmp.tmp) is left alone so operator
// scratch files in the images directory are never collected.
//
// Caller MUST hold {targetPath}.lock — the sweep and the subsequent
// os.Link must run under the same flock, otherwise a concurrent writer
// could have an in-flight {base}.tmp that this sweep would destroy.
//
// os.Remove failures other than os.IsNotExist are logged but non-fatal:
// if the stale file is the fixed {base}.tmp, the subsequent os.Link
// will surface the problem via EEXIST; legacy PID-suffixed orphans that
// resist removal become visible to the operator via the log line rather
// than turning into silent long-lived leaks.
func sweepStaleTmp(targetPath string) error {
	dir := filepath.Dir(targetPath)
	base := filepath.Base(targetPath)
	prefix := base + ".tmp"

	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("reading %s: %w", dir, err)
	}

	for _, e := range entries {
		n := e.Name()
		if !strings.HasPrefix(n, prefix) {
			continue
		}
		suffix := n[len(prefix):]
		if suffix != "" && !isLegacyPIDSuffix(suffix) {
			continue
		}
		p := filepath.Join(dir, n)
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			log.Printf("warning: failed to remove stale tmp %s: %v", p, err)
		}
	}
	return nil
}

// isLegacyPIDSuffix reports whether s is "." followed by one or more
// decimal digits, matching the pre-v0.3.6 tmp-file naming convention
// fmt.Sprintf("%s.tmp.%d", targetPath, os.Getpid()).
func isLegacyPIDSuffix(s string) bool {
	if len(s) < 2 || s[0] != '.' {
		return false
	}
	for _, r := range s[1:] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// LinkCachedImage hardlinks an existing cached ext4 under sourceName to
// targetName in the same imagesDir and writes targetName's .source sidecar
// so callers see a matching cache entry.
//
// It acquires targetName's .lock flock for the duration of the rename and
// sidecar write, serializing against EnsureImage / DeleteImage / PruneImages.
// The replace uses link-to-tmp + rename to stay atomic and preserve any open
// FDs an in-flight CopyRootfs holds against the old target inode.
//
// Returns an error if os.Link fails (e.g. cross-device, missing source, or
// permission issue). Callers should treat this as a signal to fall back to
// a full pull. No partial state is left on error.
func LinkCachedImage(imagesDir, sourceName, targetName, ref string) error {
	sourcePath := filepath.Join(imagesDir, RootfsFilename(sourceName))
	targetPath := filepath.Join(imagesDir, RootfsFilename(targetName))

	if _, err := os.Stat(sourcePath); err != nil {
		return fmt.Errorf("source image %q: %w", sourceName, err)
	}

	unlock, err := acquireFileLock(targetPath + ".lock")
	if err != nil {
		return fmt.Errorf("acquiring lock for %q: %w", targetName, err)
	}
	defer unlock()

	// Sweep stale tmp orphans for this exact target only — must not
	// match siblings (other variants) or operator scratch files.
	// Matches "{base}.tmp" (the fixed tmp name used below) and the
	// legacy "{base}.tmp.<digits>" name produced by pre-v0.3.6 builds
	// that crashed between os.Link and os.Rename.
	if err := sweepStaleTmp(targetPath); err != nil {
		return fmt.Errorf("scanning for stale tmp files: %w", err)
	}

	// Fixed tmp name: the target's .lock flock already serializes all
	// writers, so a PID suffix adds nothing. A stable name is what lets
	// the sweep above self-heal across crashes.
	tmpPath := targetPath + ".tmp"
	if err := os.Link(sourcePath, tmpPath); err != nil {
		return fmt.Errorf("linking %s -> %s: %w", sourcePath, tmpPath, err)
	}

	if err := os.Rename(tmpPath, targetPath); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("renaming %s -> %s: %w", tmpPath, targetPath, err)
	}

	if err := WriteSource(imagesDir, targetName, ref); err != nil {
		// Keep the "no partial state on error" contract: a successful
		// rename already replaced targetPath, so tearing it down here
		// avoids leaving a rootfs with a missing/stale sidecar behind.
		_ = os.Remove(targetPath)
		_ = os.Remove(filepath.Join(imagesDir, SourceFilename(targetName)))
		return fmt.Errorf("writing source sidecar for %q: %w", targetName, err)
	}

	return nil
}

// Convert pulls a Docker image and converts it to an ext4 rootfs.
// The conversion uses a privileged Docker container for ext4 creation (loop mount).
func Convert(ctx context.Context, opts ConvertOptions) (*ConvertResult, error) {
	if opts.RootfsSize == "" {
		opts.RootfsSize = "20G"
	}
	if opts.Platform == "" {
		opts.Platform = DefaultPlatform
	}

	rootfsFile := RootfsFilename(opts.Name)
	rootfsPath := filepath.Join(opts.OutputDir, rootfsFile)

	if err := os.MkdirAll(opts.OutputDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create output directory: %w", err)
	}

	containerID, err := dockerCreate(ctx, opts.Platform, opts.DockerRef)
	if err != nil {
		return nil, fmt.Errorf("failed to create container from %s: %w", opts.DockerRef, err)
	}
	defer dockerRemove(ctx, containerID)

	exportTar, err := os.CreateTemp("", "shed-rootfs-*.tar")
	if err != nil {
		return nil, fmt.Errorf("failed to create temp file: %w", err)
	}
	tarPath := exportTar.Name()
	exportTar.Close()
	defer os.Remove(tarPath)

	if err := dockerExport(ctx, containerID, tarPath); err != nil {
		return nil, fmt.Errorf("failed to export container: %w", err)
	}

	if err := createExt4(ctx, opts.Platform, tarPath, rootfsFile, opts.OutputDir, opts.RootfsSize); err != nil {
		// Clean up partial rootfs on failure
		os.Remove(rootfsPath)
		return nil, fmt.Errorf("failed to create ext4 image: %w", err)
	}

	result := &ConvertResult{RootfsPath: rootfsPath}

	if opts.ExtractKernel {
		kernelPath := filepath.Join(opts.OutputDir, "vmlinux")

		if err := extractKernel(ctx, opts.Platform, opts.DockerRef, opts.OutputDir); err != nil {
			return nil, fmt.Errorf("failed to extract kernel: %w", err)
		}
		result.KernelPath = kernelPath

		if opts.NeedsInitrd {
			initrdPath := filepath.Join(opts.OutputDir, "initrd.img")
			if err := extractInitrd(ctx, opts.Platform, opts.DockerRef, opts.OutputDir); err != nil {
				return nil, fmt.Errorf("failed to extract initrd: %w", err)
			}
			result.InitrdPath = initrdPath
		}
	}

	return result, nil
}

func dockerCreate(ctx context.Context, platform, imageRef string) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", "create", "--platform", platform, imageRef)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%s: %w", strings.TrimSpace(stderr.String()), err)
	}
	return strings.TrimSpace(stdout.String()), nil
}

// dockerExport exports a container's filesystem to a tar file.
func dockerExport(ctx context.Context, containerID, tarPath string) error {
	outFile, err := os.Create(tarPath)
	if err != nil {
		return err
	}
	defer outFile.Close()

	cmd := exec.CommandContext(ctx, "docker", "export", containerID)
	cmd.Stdout = outFile
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w", strings.TrimSpace(stderr.String()), err)
	}
	return nil
}

// dockerRemove removes a container, ignoring errors.
func dockerRemove(ctx context.Context, containerID string) {
	exec.CommandContext(ctx, "docker", "rm", containerID).Run() //nolint:errcheck
}

// createExt4 creates an ext4 filesystem from a tar using a privileged Docker container
// (required for loop mounting).
func createExt4(ctx context.Context, platform, tarPath, rootfsFile, outputDir, size string) error {
	cmd := exec.CommandContext(ctx, "docker", "run", "--rm", "--privileged",
		"-v", tarPath+":/tmp/rootfs.tar",
		"-v", outputDir+":/output",
		"--platform", platform,
		"ubuntu:24.04", "bash", "-c",
		fmt.Sprintf(`set -euo pipefail
apt-get update && apt-get install -y e2fsprogs >/dev/null 2>&1
truncate -s %s /output/%s
mkfs.ext4 -F /output/%s
mkdir -p /mnt/rootfs
mount -o loop /output/%s /mnt/rootfs
tar -xf /tmp/rootfs.tar -C /mnt/rootfs
umount /mnt/rootfs
echo 'ext4 image created successfully'`, size, rootfsFile, rootfsFile, rootfsFile))

	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w", strings.TrimSpace(stderr.String()), err)
	}
	return nil
}

// dockerRunScript runs a bash script inside the Docker image with outputDir mounted at /output.
func dockerRunScript(ctx context.Context, platform, imageRef, outputDir, script string) error {
	cmd := exec.CommandContext(ctx, "docker", "run", "--rm", "--platform", platform,
		"--entrypoint", "/bin/bash",
		"-v", outputDir+":/output",
		imageRef, "-c", script)

	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w", strings.TrimSpace(stderr.String()), err)
	}
	return nil
}

func extractKernel(ctx context.Context, platform, imageRef, outputDir string) error {
	return dockerRunScript(ctx, platform, imageRef, outputDir, `set -euo pipefail
# Try VZ-style compressed kernel first (linux-image-generic)
VMLINUZ=$(ls -v /boot/vmlinuz-* 2>/dev/null | tail -1 || true)
if [ -n "$VMLINUZ" ]; then
    if zcat "$VMLINUZ" > /output/vmlinux 2>/dev/null; then
        echo 'Decompressed gzip kernel'
    else
        cp "$VMLINUZ" /output/vmlinux
    fi
    exit 0
fi
# Try FC-style uncompressed kernel (custom Firecracker build)
if [ -f /boot/vmlinux ]; then
    cp /boot/vmlinux /output/vmlinux
    echo 'Copied uncompressed kernel'
    exit 0
fi
echo 'ERROR: No kernel found in /boot/'
exit 1`)
}

func extractInitrd(ctx context.Context, platform, imageRef, outputDir string) error {
	return dockerRunScript(ctx, platform, imageRef, outputDir, `set -euo pipefail
INITRD=$(ls -v /boot/initrd.img-* 2>/dev/null | tail -1 || true)
if [ -z "$INITRD" ]; then
    echo 'ERROR: No initrd found in /boot/'
    exit 1
fi
cp "$INITRD" /output/initrd.img`)
}
