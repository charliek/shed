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
#
# Usage:
#   ./scripts/build-vz-rootfs.sh                      # Build default variant
#   ./scripts/build-vz-rootfs.sh --variant base        # Build base variant
#   ./scripts/build-vz-rootfs.sh --variant experimental  # Build experimental variant
#   ./scripts/build-vz-rootfs.sh --all                  # Build all variants
#   ./scripts/build-vz-rootfs.sh --force-kernel         # Force kernel re-extraction

set -e
set -o pipefail

# Configuration
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(dirname "$SCRIPT_DIR")"
VZ_DIR="$PROJECT_ROOT/vz"
OUTPUT_DIR="${OUTPUT_DIR:-$HOME/Library/Application Support/shed/vz}"
ROOTFS_SIZE="${ROOTFS_SIZE:-20G}"  # 20GB default

# Built-in variants surfaced by --all and --help. Explicit --variant values
# are forwarded to Docker so custom shed-vz-<name> stages can be built too.
KNOWN_VARIANTS="base default experimental"

# Defaults
VARIANT="default"
BUILD_ALL=false
FORCE_KERNEL=false
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
        --force-kernel)
            FORCE_KERNEL=true
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
            echo "Build VZ rootfs images for shed."
            echo ""
            echo "Options:"
            echo "  --variant <name>   Build a specific variant (default: default)"
            echo "                     Available variants: $KNOWN_VARIANTS"
            echo "  --all              Build all variants"
            echo "  --force-kernel     Force kernel/initrd re-extraction even if files exist"
            echo "  --shed-ext-version Override shed-extensions image version for experimental variant"
            echo "                     (e.g., 'dev' to use a locally-built image)"
            echo "  --help, -h         Show this help message"
            echo ""
            echo "Environment variables:"
            echo "  OUTPUT_DIR         Output directory (default: ~/Library/Application Support/shed/vz)"
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
# Custom stages like shed-vz-rust are valid if they exist in the Dockerfile.
warn_unknown_variant() {
    local variant="$1"
    for known in $KNOWN_VARIANTS; do
        if [ "$variant" = "$known" ]; then
            return 0
        fi
    done
    echo "WARNING: '$variant' is not a built-in variant ($KNOWN_VARIANTS)"
    echo "         Attempting to build Docker target 'shed-vz-${variant}'..."
}

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

HOST_ARCH="$(uname -m)"
if [ "$HOST_ARCH" != "arm64" ] && [ "$HOST_ARCH" != "aarch64" ]; then
    echo "ERROR: VZ rootfs build currently supports Apple Silicon hosts only (found: $HOST_ARCH)"
    echo "Intel macOS support is planned but not yet implemented."
    exit 1
fi

# Create output directory
mkdir -p "$OUTPUT_DIR"

# Build shed-agent binary for linux/arm64 (shared across all variants)
build_agent() {
    echo ""
    echo "=== Building shed-agent binary (linux/arm64) ==="
    cd "$PROJECT_ROOT"
    GOOS=linux GOARCH=arm64 go build -o "$VZ_DIR/shed-agent" ./cmd/shed-agent
    echo "Built shed-agent binary"

    echo "=== Building shed-firstboot binary (linux/arm64) ==="
    GOOS=linux GOARCH=arm64 go build -o "$VZ_DIR/shed-firstboot" ./cmd/shed-firstboot
    echo "Built shed-firstboot binary"
}

# Extract kernel and initrd from the base image
extract_kernel() {
    local image_tag="$1"

    KERNEL_PATH="$OUTPUT_DIR/vmlinux"
    INITRD_PATH="$OUTPUT_DIR/initrd.img"

    if [ "$FORCE_KERNEL" = false ] && [ -f "$KERNEL_PATH" ] && [ -f "$INITRD_PATH" ]; then
        echo ""
        echo "=== Kernel and initrd already exist, skipping extraction ==="
        echo "  Kernel: $KERNEL_PATH"
        echo "  Initrd: $INITRD_PATH"
        echo "  Use --force-kernel to re-extract"
        return
    fi

    echo ""
    echo "=== Extracting kernel ==="
    docker run --rm --platform linux/arm64 \
        --entrypoint /bin/bash \
        -v "$OUTPUT_DIR:/output" \
        "$image_tag" -c "
            set -euo pipefail
            VMLINUZ=\$(ls /boot/vmlinuz-* 2>/dev/null | head -1)
            if [ -z \"\$VMLINUZ\" ]; then
                echo 'ERROR: No kernel found in /boot/'
                exit 1
            fi
            echo \"Found kernel: \$VMLINUZ\"

            # ARM64 vmlinuz files are gzip-compressed; decompress for VZ LinuxBootloader.
            if zcat \"\$VMLINUZ\" > /output/vmlinux 2>/dev/null; then
                echo 'Decompressed gzip kernel'
            else
                echo 'Kernel not gzip-compressed, copying as-is...'
                cp \"\$VMLINUZ\" /output/vmlinux
            fi
            echo 'Kernel extracted successfully'
        "
    echo "Extracted kernel: $KERNEL_PATH"

    echo ""
    echo "=== Extracting initrd ==="
    docker run --rm --platform linux/arm64 \
        --entrypoint /bin/bash \
        -v "$OUTPUT_DIR:/output" \
        "$image_tag" -c "
            set -euo pipefail
            INITRD=\$(ls /boot/initrd.img-* 2>/dev/null | head -1)
            if [ -z \"\$INITRD\" ]; then
                echo 'ERROR: No initrd found in /boot/'
                exit 1
            fi
            echo \"Found initrd: \$INITRD\"
            cp \"\$INITRD\" /output/initrd.img
            echo 'Initrd extracted successfully'
        "
    echo "Extracted initrd: $INITRD_PATH"
}

# Build a single variant
build_variant() {
    local variant="$1"
    local docker_target="shed-vz-${variant}"
    local docker_tag="shed-vz-${variant}:latest"
    local rootfs_file="${variant}-rootfs.ext4"
    local rootfs_path="$OUTPUT_DIR/$rootfs_file"

    echo ""
    echo "========================================"
    echo "  Building variant: $variant"
    echo "  Docker target: $docker_target"
    echo "  Output: $rootfs_file"
    echo "========================================"

    # Build Docker image. Context is the vz/ directory so the
    # Dockerfile's relative COPY paths (daemon.json, shed-agent, etc.)
    # resolve correctly. The shed initramfs is built separately by
    # build-initramfs.sh from initramfs/Dockerfile.
    echo ""
    echo "=== Building Docker image ($docker_tag) ==="
    cd "$VZ_DIR"
    local build_args=()
    if [ -n "$SHED_EXT_VERSION" ]; then
        build_args+=(--build-arg "SHED_EXT_VERSION=$SHED_EXT_VERSION")
    fi
    if ! docker buildx build --platform linux/arm64 --target "$docker_target" -t "$docker_tag" "${build_args[@]}" --load .; then
        echo "ERROR: Docker build failed for variant '$variant'"
        echo "Hint: Ensure Docker Desktop has buildx enabled for linux/arm64"
        exit 1
    fi
    echo "Built Docker image: $docker_tag"

    # Create container and export filesystem
    echo ""
    echo "=== Exporting filesystem ==="
    CONTAINER_ID=$(docker create --platform linux/arm64 "$docker_tag")
    echo "Created container: $CONTAINER_ID"

    EXPORT_TAR=$(mktemp)
    docker export "$CONTAINER_ID" > "$EXPORT_TAR"
    docker rm "$CONTAINER_ID"
    CONTAINER_ID=""
    echo "Exported filesystem to tar"

    # Create ext4 image
    echo ""
    echo "=== Creating ext4 image ==="
    docker run --rm --privileged \
        -v "$EXPORT_TAR:/tmp/rootfs.tar" \
        -v "$OUTPUT_DIR:/output" \
        --platform linux/arm64 \
        ubuntu:24.04 bash -c "
            set -euo pipefail
            apt-get update && apt-get install -y e2fsprogs >/dev/null 2>&1
            truncate -s $ROOTFS_SIZE /output/$rootfs_file
            mkfs.ext4 -F /output/$rootfs_file
            mkdir -p /mnt/rootfs
            mount -o loop /output/$rootfs_file /mnt/rootfs
            tar -xf /tmp/rootfs.tar -C /mnt/rootfs
            umount /mnt/rootfs
            echo 'ext4 image created successfully'
        "

    # Clean up temp tar
    rm -f "$EXPORT_TAR"
    EXPORT_TAR=""

    echo "Created rootfs image: $rootfs_path"

    # Extract kernel/initrd from the base image (all variants share the same kernel)
    extract_kernel "$docker_tag"

    # Build the shed-overlay initramfs (one initrd is fine across all
    # variants — it's image-content-independent). Stage into a tempfile
    # rather than OUTPUT_DIR for symmetry with the FC script and to
    # avoid leaking intermediates if install-blob.sh is interrupted.
    local shed_initrd
    shed_initrd="$(mktemp "${TMPDIR:-/tmp}/shed-initrd-vz.XXXXXX.img")"
    echo ""
    echo "=== Building shed-overlay initramfs ==="
    "$SCRIPT_DIR/build-initramfs.sh" \
        --backend vz \
        --platform linux/arm64 \
        --output "$shed_initrd"

    # Install rootfs+kernel+initrd as a content-addressed blob and
    # update the variant tag.
    echo ""
    echo "=== Installing blob ==="
    "$SCRIPT_DIR/install-blob.sh" \
        --images-dir "$OUTPUT_DIR" \
        --rootfs "$rootfs_path" \
        --kernel "$KERNEL_PATH" \
        --initrd "$shed_initrd" \
        --tag "$variant" \
        --backend vz \
        --arch arm64
}

# Main execution
echo "=== Building VZ Rootfs ==="
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
rm -f "$VZ_DIR/shed-agent" "$VZ_DIR/shed-firstboot"

echo ""
echo "=== Build Complete ==="
if [ "$BUILD_ALL" = true ]; then
    for v in $KNOWN_VARIANTS; do
        echo "  ${v}-rootfs.ext4"
    done
else
    echo "  ${VARIANT}-rootfs.ext4"
fi
echo "  Kernel: $OUTPUT_DIR/vmlinux"
echo "  Initrd: $OUTPUT_DIR/initrd.img"
echo ""
echo "Next steps:"
echo "1. Install vfkit: brew install vfkit"
echo "2. Configure server.yaml with backend: vz"
echo "3. Code-sign shed-server: codesign --entitlements internal/vz/entitlements.plist -s - ./shed-server"
echo "4. Start the server: ./shed-server serve"
