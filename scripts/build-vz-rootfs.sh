#!/bin/bash
# Build script for VZ (Apple Virtualization.framework) rootfs image
# This script:
# 1. Builds the shed-agent binary for linux/arm64 (Apple Silicon)
# 2. Builds a Docker image with the rootfs contents
# 3. Exports the filesystem to an ext4 image
# 4. Extracts the kernel for LinuxBootloader
#
# Prerequisites: Docker, Go, truncate (from coreutils: brew install coreutils)
# Note: VZ support is currently Apple Silicon-only.

set -e
set -o pipefail

# Configuration
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(dirname "$SCRIPT_DIR")"
VZ_DIR="$PROJECT_ROOT/vz"
OUTPUT_DIR="${OUTPUT_DIR:-$HOME/Library/Application Support/shed/vz}"
ROOTFS_SIZE="${ROOTFS_SIZE:-20G}"  # 20GB default

# Variables for cleanup
MOUNT_POINT=""
EXPORT_TAR=""
CONTAINER_ID=""

# Cleanup function for trap
cleanup() {
    if [ -n "$MOUNT_POINT" ] && [ -d "$MOUNT_POINT" ]; then
        # Try to detach the disk image on macOS
        if command -v hdiutil &>/dev/null; then
            hdiutil detach "$MOUNT_POINT" 2>/dev/null || true
        fi
        rmdir "$MOUNT_POINT" 2>/dev/null || true
    fi
    if [ -n "$EXPORT_TAR" ] && [ -f "$EXPORT_TAR" ]; then
        rm -f "$EXPORT_TAR"
    fi
    if [ -n "$CONTAINER_ID" ]; then
        docker rm "$CONTAINER_ID" 2>/dev/null || true
    fi
}

trap cleanup EXIT

echo "=== Building VZ Rootfs ==="
echo "Project root: $PROJECT_ROOT"
echo "Output directory: $OUTPUT_DIR"

HOST_ARCH="$(uname -m)"
if [ "$HOST_ARCH" != "arm64" ] && [ "$HOST_ARCH" != "aarch64" ]; then
    echo "ERROR: VZ rootfs build currently supports Apple Silicon hosts only (found: $HOST_ARCH)"
    echo "Intel macOS support is planned but not yet implemented."
    exit 1
fi

# Create output directory
mkdir -p "$OUTPUT_DIR"

# Step 1: Build shed-agent binary for linux/arm64
echo ""
echo "=== Step 1: Building shed-agent binary (linux/arm64) ==="
cd "$PROJECT_ROOT"
GOOS=linux GOARCH=arm64 go build -o "$VZ_DIR/shed-agent" ./cmd/shed-agent
echo "Built shed-agent binary"

# Step 2: Build Docker image (using buildx for cross-platform)
echo ""
echo "=== Step 2: Building Docker image ==="
cd "$VZ_DIR"
if ! docker buildx build --platform linux/arm64 -t shed-vz-rootfs:latest --load .; then
    echo "ERROR: Docker build failed"
    echo "Hint: Ensure Docker Desktop has buildx enabled for linux/arm64"
    exit 1
fi
echo "Built Docker image"

# Step 3: Create container and export filesystem
echo ""
echo "=== Step 3: Exporting filesystem ==="
CONTAINER_ID=$(docker create --platform linux/arm64 shed-vz-rootfs:latest)
echo "Created container: $CONTAINER_ID"

# Export filesystem to tar
EXPORT_TAR=$(mktemp)
docker export "$CONTAINER_ID" > "$EXPORT_TAR"
docker rm "$CONTAINER_ID"
CONTAINER_ID=""  # Clear to prevent double cleanup
echo "Exported filesystem to tar"

# Step 4: Create ext4 image
echo ""
echo "=== Step 4: Creating ext4 image ==="
ROOTFS_PATH="$OUTPUT_DIR/base-rootfs.ext4"

# On macOS, use a Linux container to create the ext4 image since macOS
# doesn't natively support ext4.
echo "Creating ext4 image via Docker..."
docker run --rm --privileged \
    -v "$EXPORT_TAR:/tmp/rootfs.tar" \
    -v "$OUTPUT_DIR:/output" \
    --platform linux/arm64 \
    ubuntu:24.04 bash -c "
        set -euo pipefail
        apt-get update && apt-get install -y e2fsprogs >/dev/null 2>&1
        truncate -s $ROOTFS_SIZE /output/base-rootfs.ext4
        mkfs.ext4 -F /output/base-rootfs.ext4
        mkdir -p /mnt/rootfs
        mount -o loop /output/base-rootfs.ext4 /mnt/rootfs
        tar -xf /tmp/rootfs.tar -C /mnt/rootfs
        umount /mnt/rootfs
        echo 'ext4 image created successfully'
    "

echo "Created rootfs image: $ROOTFS_PATH"

# Step 5: Extract kernel
echo ""
echo "=== Step 5: Extracting kernel ==="
KERNEL_PATH="$OUTPUT_DIR/vmlinux"

# Extract the kernel from the Docker image
# The kernel is at /boot/vmlinuz-* in Ubuntu images; we need the decompressed version
docker run --rm --platform linux/arm64 \
    -v "$OUTPUT_DIR:/output" \
    shed-vz-rootfs:latest bash -c "
        set -euo pipefail
        VMLINUZ=\$(ls /boot/vmlinuz-* 2>/dev/null | head -1)
        if [ -z \"\$VMLINUZ\" ]; then
            echo 'ERROR: No kernel found in /boot/'
            exit 1
        fi
        echo \"Found kernel: \$VMLINUZ\"

        # Try to extract decompressed kernel
        # ARM64 kernels are often gzip-compressed
        if file \"\$VMLINUZ\" | grep -q 'gzip'; then
            echo 'Decompressing gzip kernel...'
            zcat \"\$VMLINUZ\" > /output/vmlinux
        elif file \"\$VMLINUZ\" | grep -q 'PE32+'; then
            # ARM64 EFI stub kernels - copy as-is, vfkit can handle them
            echo 'ARM64 EFI stub kernel, copying directly...'
            cp \"\$VMLINUZ\" /output/vmlinux
        else
            echo 'Copying kernel as-is...'
            cp \"\$VMLINUZ\" /output/vmlinux
        fi
        echo 'Kernel extracted successfully'
    "

echo "Extracted kernel: $KERNEL_PATH"

# Clean up the shed-agent binary from the build directory
rm -f "$VZ_DIR/shed-agent"

echo ""
echo "=== Build Complete ==="
echo "Rootfs image: $ROOTFS_PATH"
echo "Kernel: $KERNEL_PATH"
echo ""
echo "Next steps:"
echo "1. Install vfkit: brew install vfkit"
echo "2. Configure server.yaml with backend: vz"
echo "3. Code-sign shed-server: codesign --entitlements internal/vz/entitlements.plist -s - ./shed-server"
echo "4. Start the server: ./shed-server serve"
