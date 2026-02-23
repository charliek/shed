#!/bin/bash
# Build script for Firecracker rootfs image
# This script:
# 1. Builds the shed-agent binary for linux/amd64
# 2. Builds a Docker image with the rootfs contents
# 3. Exports the filesystem to an ext4 image

set -e
set -o pipefail

# Configuration
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(dirname "$SCRIPT_DIR")"
FIRECRACKER_DIR="$PROJECT_ROOT/firecracker"
OUTPUT_DIR="${OUTPUT_DIR:-/var/lib/shed/firecracker}"
ROOTFS_SIZE="${ROOTFS_SIZE:-20G}"  # 20GB default

# Variables for cleanup
MOUNT_POINT=""
EXPORT_TAR=""
CONTAINER_ID=""

# Cleanup function for trap
cleanup() {
    if [ -n "$MOUNT_POINT" ] && mountpoint -q "$MOUNT_POINT" 2>/dev/null; then
        echo "Cleaning up: unmounting $MOUNT_POINT"
        sudo umount "$MOUNT_POINT" || true
    fi
    if [ -n "$MOUNT_POINT" ] && [ -d "$MOUNT_POINT" ]; then
        rmdir "$MOUNT_POINT" || true
    fi
    if [ -n "$EXPORT_TAR" ] && [ -f "$EXPORT_TAR" ]; then
        rm -f "$EXPORT_TAR"
    fi
    if [ -n "$CONTAINER_ID" ]; then
        docker rm "$CONTAINER_ID" 2>/dev/null || true
    fi
}

trap cleanup EXIT

echo "=== Building Firecracker Rootfs ==="
echo "Project root: $PROJECT_ROOT"
echo "Output directory: $OUTPUT_DIR"

# Create output directory
sudo mkdir -p "$OUTPUT_DIR"

# Step 1: Build shed-agent binary
echo ""
echo "=== Step 1: Building shed-agent binary ==="
cd "$PROJECT_ROOT"
GOOS=linux GOARCH=amd64 go build -o "$FIRECRACKER_DIR/shed-agent" ./cmd/shed-agent
echo "Built shed-agent binary"

# Step 2: Build Docker image
echo ""
echo "=== Step 2: Building Docker image ==="
cd "$FIRECRACKER_DIR"
if ! docker build -t shed-rootfs:latest .; then
    echo "ERROR: Docker build failed"
    exit 1
fi
echo "Built Docker image"

# Step 3: Create container and export filesystem
echo ""
echo "=== Step 3: Exporting filesystem ==="
CONTAINER_ID=$(docker create shed-rootfs:latest)
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

# Create a sparse file
sudo truncate -s "$ROOTFS_SIZE" "$ROOTFS_PATH"

# Format as ext4
sudo mkfs.ext4 -F "$ROOTFS_PATH"

# Mount and extract
MOUNT_POINT=$(mktemp -d)
sudo mount -o loop "$ROOTFS_PATH" "$MOUNT_POINT"

echo "Extracting filesystem..."
sudo tar -xf "$EXPORT_TAR" -C "$MOUNT_POINT"

# Clean up (clear variables to prevent double cleanup in trap)
sudo umount "$MOUNT_POINT"
rmdir "$MOUNT_POINT"
MOUNT_POINT=""
rm "$EXPORT_TAR"
EXPORT_TAR=""
# CONTAINER_ID already cleaned up after export

echo ""
echo "=== Build Complete ==="
echo "Rootfs image: $ROOTFS_PATH"
echo ""
echo "Next steps:"
echo "1. Download Firecracker and kernel: ./scripts/download-firecracker.sh"
echo "2. Configure server.yaml with backend: firecracker"
echo "3. Set up bridge network (see docs/firecracker_install.md)"
