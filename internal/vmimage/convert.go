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
//
// Convert leaves all output files in OutputDir. Callers that want to
// install the result into the content-addressed blob store should pass
// the result to InstallBlob.
type ConvertResult struct {
	// RootfsPath is the path to the created ext4 image.
	RootfsPath string

	// KernelPath is the path to the extracted kernel (empty if not extracted).
	KernelPath string

	// InitrdPath is the path to the extracted initrd (empty if not extracted).
	InitrdPath string

	// Digest is the sha256 digest of the rootfs.ext4 file produced by
	// the conversion, formatted as "sha256:<hex>".
	Digest string
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

// Resolve looks up a tag and returns the path to its blob's rootfs.ext4
// if the tag exists, the blob is installed, and (when expectedRef is
// non-empty) the blob's manifest.SourceRef matches expectedRef. Returns
// "" if any of these conditions fail (i.e. cache miss).
//
// expectedRef may be left empty to skip the source-ref check (callers
// that don't track cache freshness by Docker ref).
func Resolve(imagesDir, tag, expectedRef string) string {
	t, err := GetTag(imagesDir, tag)
	if err != nil {
		return ""
	}
	if !BlobExists(imagesDir, t.Digest) {
		return ""
	}
	if expectedRef != "" {
		manifest, err := LoadManifest(imagesDir, t.Digest)
		if err != nil {
			return ""
		}
		if manifest.SourceRef != expectedRef {
			return ""
		}
	}
	rootfs, err := BlobRootfsPath(imagesDir, t.Digest)
	if err != nil {
		return ""
	}
	return rootfs
}

// ResolveTag looks up a tag and returns its digest plus the path to the
// blob's rootfs.ext4. Returns ErrTagNotFound or ErrBlobNotFound if the
// tag/blob is missing.
func ResolveTag(imagesDir, tag string) (digest, rootfsPath string, err error) {
	t, err := GetTag(imagesDir, tag)
	if err != nil {
		return "", "", err
	}
	if !BlobExists(imagesDir, t.Digest) {
		return t.Digest, "", fmt.Errorf("%w: %s (tag %q)", ErrBlobNotFound, t.Digest, tag)
	}
	rootfs, err := BlobRootfsPath(imagesDir, t.Digest)
	if err != nil {
		return t.Digest, "", err
	}
	return t.Digest, rootfs, nil
}

// Convert pulls a Docker image and converts it to an ext4 rootfs in a
// staging directory under OutputDir. The conversion uses a privileged
// Docker container for ext4 creation (loop mount).
//
// The caller is responsible for installing the result into the
// content-addressed blob store (see InstallBlob). On success, all files
// referenced by the returned ConvertResult live in a per-call staging
// dir; if the caller does not InstallBlob the files, it should remove
// the staging dir to avoid leaks.
func Convert(ctx context.Context, opts ConvertOptions) (*ConvertResult, error) {
	if opts.RootfsSize == "" {
		opts.RootfsSize = "20G"
	}
	if opts.Platform == "" {
		opts.Platform = DefaultPlatform
	}

	if err := os.MkdirAll(opts.OutputDir, 0o755); err != nil {
		return nil, fmt.Errorf("failed to create output directory: %w", err)
	}

	stagingDir, err := os.MkdirTemp(opts.OutputDir, ".convert-*")
	if err != nil {
		return nil, fmt.Errorf("failed to create staging directory: %w", err)
	}
	cleanupStaging := true
	defer func() {
		if cleanupStaging {
			os.RemoveAll(stagingDir)
		}
	}()

	rootfsFile := BlobRootfsFilename
	rootfsPath := filepath.Join(stagingDir, rootfsFile)

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

	if err := createExt4(ctx, opts.Platform, tarPath, rootfsFile, stagingDir, opts.RootfsSize); err != nil {
		return nil, fmt.Errorf("failed to create ext4 image: %w", err)
	}

	digest, err := HashFile(rootfsPath)
	if err != nil {
		return nil, fmt.Errorf("hashing rootfs: %w", err)
	}

	result := &ConvertResult{
		RootfsPath: rootfsPath,
		Digest:     digest,
	}

	if opts.ExtractKernel {
		if err := extractKernel(ctx, opts.Platform, opts.DockerRef, stagingDir); err != nil {
			return nil, fmt.Errorf("failed to extract kernel: %w", err)
		}
		// extractKernel writes to stagingDir/vmlinux; rename to the
		// blob-store filename so InstallBlob can pick it up by name.
		oldKernel := filepath.Join(stagingDir, "vmlinux")
		newKernel := filepath.Join(stagingDir, BlobKernelFilename)
		if err := os.Rename(oldKernel, newKernel); err != nil {
			return nil, fmt.Errorf("renaming kernel into staging: %w", err)
		}
		result.KernelPath = newKernel

		if opts.NeedsInitrd {
			if err := extractInitrd(ctx, opts.Platform, opts.DockerRef, stagingDir); err != nil {
				return nil, fmt.Errorf("failed to extract initrd: %w", err)
			}
			oldInitrd := filepath.Join(stagingDir, "initrd.img")
			newInitrd := filepath.Join(stagingDir, BlobInitrdFilename)
			if err := os.Rename(oldInitrd, newInitrd); err != nil {
				return nil, fmt.Errorf("renaming initrd into staging: %w", err)
			}
			result.InitrdPath = newInitrd
		}
	}

	cleanupStaging = false
	return result, nil
}

// CleanupConvert removes the staging directory associated with a
// ConvertResult. Safe to call after a successful InstallBlob to clear
// the now-empty staging dir.
func CleanupConvert(r *ConvertResult) {
	if r == nil || r.RootfsPath == "" {
		return
	}
	os.RemoveAll(filepath.Dir(r.RootfsPath))
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
