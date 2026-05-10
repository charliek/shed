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
#   ./scripts/build-firecracker-rootfs.sh --variant experimental  # Build experimental variant
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
KNOWN_VARIANTS="base default experimental"

# Defaults
VARIANT="default"
BUILD_ALL=false
SHED_EXT_VERSION=""

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
        --shed-ext-version)
            if [[ $# -lt 2 || "$2" == --* ]]; then
                echo "ERROR: --shed-ext-version requires a value"
                echo "Run '$0 --help' for usage."
                exit 1
            fi
            SHED_EXT_VERSION="$2"
            shift 2
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
            echo "  --shed-ext-version Override shed-extensions image version for experimental variant"
            echo "                     (e.g., 'dev' to use a locally-built image)"
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

    echo "=== Building shed-firstboot binary (linux/amd64) ==="
    GOOS=linux GOARCH=amd64 go build -o "$FIRECRACKER_DIR/shed-firstboot" ./cmd/shed-firstboot
    echo "Built shed-firstboot binary"
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

    # Build Docker image. Context is the firecracker/ directory so the
    # Dockerfile's relative COPY paths resolve correctly. The shed
    # initramfs is built separately by build-initramfs.sh.
    echo ""
    echo "=== Building Docker image ($docker_tag) ==="
    cd "$FIRECRACKER_DIR"
    local build_args=()
    if [ -n "$SHED_EXT_VERSION" ]; then
        build_args+=(--build-arg "SHED_EXT_VERSION=$SHED_EXT_VERSION")
    fi
    if ! docker buildx build --platform linux/amd64 --target "$docker_target" -t "$docker_tag" "${build_args[@]}" --load .; then
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

    # Build the shed-overlay initramfs into a tempfile rather than
    # OUTPUT_DIR. OUTPUT_DIR is root-owned by default (`sudo mkdir -p`
    # at script start), so writing the intermediate as the current
    # user would fail with EPERM before install-blob.sh ever runs.
    # install-blob.sh moves the file into the blob layout, so no
    # explicit cleanup is needed on the happy path.
    local shed_initrd
    shed_initrd="$(mktemp "${TMPDIR:-/tmp}/shed-initrd-fc.XXXXXX.img")"
    echo ""
    echo "=== Building shed-overlay initramfs ==="
    "$SCRIPT_DIR/build-initramfs.sh" \
        --backend firecracker \
        --platform linux/amd64 \
        --output "$shed_initrd"

    # Firecracker's compiled kernel lives at ${OUTPUT_DIR}/vmlinux after
    # download-firecracker.sh runs. Use it as the blob's kernel when
    # present so the runtime never has to look outside the blob dir.
    local kernel_arg=()
    if [ -f "$OUTPUT_DIR/vmlinux" ]; then
        kernel_arg=(--kernel "$OUTPUT_DIR/vmlinux")
    fi

    echo ""
    echo "=== Installing blob ==="
    "$SCRIPT_DIR/install-blob.sh" \
        --images-dir "$OUTPUT_DIR" \
        --rootfs "$rootfs_path" \
        "${kernel_arg[@]}" \
        --initrd "$shed_initrd" \
        --tag "$variant" \
        --backend firecracker \
        --arch amd64
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

# Clean up the shed-agent and shed-firstboot binaries from the build directory
rm -f "$FIRECRACKER_DIR/shed-agent" "$FIRECRACKER_DIR/shed-firstboot"

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
