// Package vmimage provides Docker-to-ext4 image conversion for VM backends.
// This package is cross-platform (no build tags) so it can be tested on Linux CI.
// Both VZ and Firecracker backends use ext4 rootfs images and can share this pipeline.
package vmimage

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/distribution/reference"
)

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

	// ExtractKernel controls whether kernel and initrd should be extracted.
	// If true and kernel/initrd don't already exist in OutputDir, they are extracted.
	ExtractKernel bool
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

// Convert pulls a Docker image and converts it to an ext4 rootfs.
// The conversion uses a privileged Docker container for ext4 creation (loop mount).
func Convert(ctx context.Context, opts ConvertOptions) (*ConvertResult, error) {
	if opts.RootfsSize == "" {
		opts.RootfsSize = "20G"
	}

	rootfsFile := RootfsFilename(opts.Name)
	rootfsPath := filepath.Join(opts.OutputDir, rootfsFile)

	// Ensure output directory exists
	if err := os.MkdirAll(opts.OutputDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create output directory: %w", err)
	}

	// Create container and export filesystem to tar
	containerID, err := dockerCreate(ctx, opts.DockerRef)
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

	// Create ext4 image via privileged Docker container
	if err := createExt4(ctx, tarPath, rootfsFile, opts.OutputDir, opts.RootfsSize); err != nil {
		// Clean up partial rootfs on failure
		os.Remove(rootfsPath)
		return nil, fmt.Errorf("failed to create ext4 image: %w", err)
	}

	result := &ConvertResult{RootfsPath: rootfsPath}

	// Extract kernel and initrd if requested
	if opts.ExtractKernel {
		kernelPath := filepath.Join(opts.OutputDir, "vmlinux")
		initrdPath := filepath.Join(opts.OutputDir, "initrd.img")

		if _, err := os.Stat(kernelPath); os.IsNotExist(err) {
			if err := extractKernel(ctx, opts.DockerRef, opts.OutputDir); err != nil {
				return nil, fmt.Errorf("failed to extract kernel: %w", err)
			}
		}
		if _, err := os.Stat(initrdPath); os.IsNotExist(err) {
			if err := extractInitrd(ctx, opts.DockerRef, opts.OutputDir); err != nil {
				return nil, fmt.Errorf("failed to extract initrd: %w", err)
			}
		}

		result.KernelPath = kernelPath
		result.InitrdPath = initrdPath
	}

	return result, nil
}

// dockerCreate creates a container from the given image without starting it.
func dockerCreate(ctx context.Context, imageRef string) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", "create", "--platform", "linux/arm64", imageRef)
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

// createExt4 creates an ext4 filesystem from a tar using a privileged Docker container.
// This matches the approach in build-vz-rootfs.sh.
func createExt4(ctx context.Context, tarPath, rootfsFile, outputDir, size string) error {
	cmd := exec.CommandContext(ctx, "docker", "run", "--rm", "--privileged",
		"-v", tarPath+":/tmp/rootfs.tar",
		"-v", outputDir+":/output",
		"--platform", "linux/arm64",
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

// extractKernel extracts the decompressed kernel from a Docker image.
func extractKernel(ctx context.Context, imageRef, outputDir string) error {
	cmd := exec.CommandContext(ctx, "docker", "run", "--rm", "--platform", "linux/arm64",
		"--entrypoint", "/bin/bash",
		"-v", outputDir+":/output",
		imageRef, "-c", `set -euo pipefail
VMLINUZ=$(ls /boot/vmlinuz-* 2>/dev/null | head -1)
if [ -z "$VMLINUZ" ]; then
    echo 'ERROR: No kernel found in /boot/'
    exit 1
fi
if zcat "$VMLINUZ" > /output/vmlinux 2>/dev/null; then
    echo 'Decompressed gzip kernel'
else
    cp "$VMLINUZ" /output/vmlinux
fi`)

	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w", strings.TrimSpace(stderr.String()), err)
	}
	return nil
}

// extractInitrd extracts the initrd from a Docker image.
func extractInitrd(ctx context.Context, imageRef, outputDir string) error {
	cmd := exec.CommandContext(ctx, "docker", "run", "--rm", "--platform", "linux/arm64",
		"--entrypoint", "/bin/bash",
		"-v", outputDir+":/output",
		imageRef, "-c", `set -euo pipefail
INITRD=$(ls /boot/initrd.img-* 2>/dev/null | head -1)
if [ -z "$INITRD" ]; then
    echo 'ERROR: No initrd found in /boot/'
    exit 1
fi
cp "$INITRD" /output/initrd.img`)

	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w", strings.TrimSpace(stderr.String()), err)
	}
	return nil
}
