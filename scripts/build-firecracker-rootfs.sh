#!/bin/bash
# Build script for Firecracker rootfs image
# This script:
# 1. Builds the shed-agent binary for linux/amd64
# 2. Builds a Docker image with the rootfs contents
# 3. Exports the filesystem to an ext4 image
#
# Prerequisites: Docker, Go
#
# Usage:
#   ./scripts/build-firecracker-rootfs.sh                      # Build default variant
#   ./scripts/build-firecracker-rootfs.sh --variant base        # Build base variant
#   ./scripts/build-firecracker-rootfs.sh --variant typescript   # Build typescript variant
#   ./scripts/build-firecracker-rootfs.sh --all                  # Build all variants

set -e
set -o pipefail

# Configuration
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(dirname "$SCRIPT_DIR")"
FIRECRACKER_DIR="$PROJECT_ROOT/firecracker"
OUTPUT_DIR="${OUTPUT_DIR:-/var/lib/shed/firecracker/images}"
ROOTFS_SIZE="${ROOTFS_SIZE:-20G}"  # 20GB default

# Built-in variants surfaced by --all and --help. Explicit --variant values
# are forwarded to Docker so custom shed-fc-<name> stages can be built too.
KNOWN_VARIANTS="base default typescript"

# Defaults
VARIANT="default"
BUILD_ALL=false

# Parse arguments
while [[ $# -gt 0 ]]; do
    case "$1" in
        --variant)
            if [[ $# -lt 2 || "$2" == --* ]]; then
                echo "ERROR: --variant requires a value"
                echo "Run '$0 --help' for usage."
                exit 1
            fi
            VARIANT="$2"
            shift 2
            ;;
        --all)
            BUILD_ALL=true
            shift
            ;;
        --help|-h)
            echo "Usage: $0 [OPTIONS]"
            echo ""
            echo "Build Firecracker rootfs images for shed."
            echo ""
            echo "Options:"
            echo "  --variant <name>   Build a specific variant (default: default)"
            echo "                     Available variants: $KNOWN_VARIANTS"
            echo "  --all              Build all variants"
            echo "  --help, -h         Show this help message"
            echo ""
            echo "Environment variables:"
            echo "  OUTPUT_DIR         Output directory (default: /var/lib/shed/firecracker/images)"
            echo "  ROOTFS_SIZE        Rootfs image size (default: 20G)"
            exit 0
            ;;
        *)
            echo "ERROR: Unknown argument: $1"
            echo "Run '$0 --help' for usage."
            exit 1
            ;;
    esac
done

# Warn (but don't block) if the variant is not in KNOWN_VARIANTS.
# Custom stages like shed-fc-rust are valid if they exist in the Dockerfile.
warn_unknown_variant() {
    local variant="$1"
    for known in $KNOWN_VARIANTS; do
        if [ "$variant" = "$known" ]; then
            return 0
        fi
    done
    echo "WARNING: '$variant' is not a built-in variant ($KNOWN_VARIANTS)"
    echo "         Attempting to build Docker target 'shed-fc-${variant}'..."
}

# Variables for cleanup
EXPORT_TAR=""
CONTAINER_ID=""

# Cleanup function for trap
cleanup() {
    if [ -n "$EXPORT_TAR" ] && [ -f "$EXPORT_TAR" ]; then
        rm -f "$EXPORT_TAR"
    fi
    if [ -n "$CONTAINER_ID" ]; then
        docker rm "$CONTAINER_ID" 2>/dev/null || true
    fi
}

trap cleanup EXIT

# Create output directory
sudo mkdir -p "$OUTPUT_DIR"

# Build shed-agent binary for linux/amd64 (shared across all variants)
build_agent() {
    echo ""
    echo "=== Building shed-agent binary (linux/amd64) ==="
    cd "$PROJECT_ROOT"
    GOOS=linux GOARCH=amd64 go build -o "$FIRECRACKER_DIR/shed-agent" ./cmd/shed-agent
    echo "Built shed-agent binary"
}

# Build a single variant
build_variant() {
    local variant="$1"
    local docker_target="shed-fc-${variant}"
    local docker_tag="shed-fc-${variant}:latest"
    local rootfs_file="${variant}-rootfs.ext4"
    local rootfs_path="$OUTPUT_DIR/$rootfs_file"

    echo ""
    echo "========================================"
    echo "  Building variant: $variant"
    echo "  Docker target: $docker_target"
    echo "  Output: $rootfs_file"
    echo "========================================"

    # Build Docker image
    echo ""
    echo "=== Building Docker image ($docker_tag) ==="
    cd "$FIRECRACKER_DIR"
    if ! docker buildx build --platform linux/amd64 --target "$docker_target" -t "$docker_tag" --load .; then
        echo "ERROR: Docker build failed for variant '$variant'"
        exit 1
    fi
    echo "Built Docker image: $docker_tag"

    # Create container and export filesystem
    echo ""
    echo "=== Exporting filesystem ==="
    CONTAINER_ID=$(docker create --platform linux/amd64 "$docker_tag")
    echo "Created container: $CONTAINER_ID"

    EXPORT_TAR=$(mktemp)
    docker export "$CONTAINER_ID" > "$EXPORT_TAR"
    docker rm "$CONTAINER_ID"
    CONTAINER_ID=""
    echo "Exported filesystem to tar"

    # Create ext4 image
    echo ""
    echo "=== Creating ext4 image ==="

    # Create a sparse file
    sudo truncate -s "$ROOTFS_SIZE" "$rootfs_path"

    # Format as ext4
    sudo mkfs.ext4 -F "$rootfs_path"

    # Mount and extract
    local mount_point
    mount_point=$(mktemp -d)
    sudo mount -o loop "$rootfs_path" "$mount_point"

    echo "Extracting filesystem..."
    sudo tar -xf "$EXPORT_TAR" -C "$mount_point"

    sudo umount "$mount_point"
    rmdir "$mount_point"

    # Clean up temp tar
    rm -f "$EXPORT_TAR"
    EXPORT_TAR=""

    echo "Created rootfs image: $rootfs_path"
}

# Main execution
echo "=== Building Firecracker Rootfs ==="
echo "Project root: $PROJECT_ROOT"
echo "Output directory: $OUTPUT_DIR"

# Build the agent binary first (shared across all variants)
build_agent

if [ "$BUILD_ALL" = true ]; then
    echo ""
    echo "Building all variants: $KNOWN_VARIANTS"
    for v in $KNOWN_VARIANTS; do
        build_variant "$v"
    done
else
    warn_unknown_variant "$VARIANT"
    build_variant "$VARIANT"
fi

# Clean up the shed-agent binary from the build directory
rm -f "$FIRECRACKER_DIR/shed-agent"

echo ""
echo "=== Build Complete ==="
if [ "$BUILD_ALL" = true ]; then
    for v in $KNOWN_VARIANTS; do
        echo "  ${v}-rootfs.ext4"
    done
else
    echo "  ${VARIANT}-rootfs.ext4"
fi
echo ""
echo "Next steps:"
echo "1. Download Firecracker and kernel: ./scripts/download-firecracker.sh"
echo "2. Configure server.yaml with backend: firecracker"
echo "3. Set up bridge network (see docs/getting-started/fc-setup.md)"
