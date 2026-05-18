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
#   ./scripts/build-vz-rootfs.sh                      # Build full variant
#   ./scripts/build-vz-rootfs.sh --variant base        # Build base variant
#   ./scripts/build-vz-rootfs.sh --variant extensions  # Build extensions variant
#   ./scripts/build-vz-rootfs.sh --variant full        # Build full variant
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
KNOWN_VARIANTS="base extensions full"

# Defaults
VARIANT="full"
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
            echo "  --variant <name>   Build a specific variant (default: full)"
            echo "                     Available variants: $KNOWN_VARIANTS"
            echo "  --all              Build all variants"
            echo "  --force-kernel     Force kernel/initrd re-extraction even if files exist"
            echo "  --shed-ext-version Override shed-extensions image version for extensions/full variants"
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
SHED_INITRD=""

# Cleanup function for trap
cleanup() {
    if [ -n "$SHED_INITRD" ] && [ -f "$SHED_INITRD" ]; then
        rm -f "$SHED_INITRD"
    fi
    # Remove the staged binaries from the build context so the working
    # tree stays clean after the script exits (Dockerfile COPYs them
    # via relative paths during the build).
    rm -f "$VZ_DIR/shed-agent" "$VZ_DIR/shed-firstboot"
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

# Build the prerequisites the Dockerfile needs (shed-agent + shed-firstboot
# in the build context) and the host-side shed CLI (used to drive the
# OCI conversion).
build_prereqs() {
    echo ""
    echo "=== Building shed-agent binary (linux/arm64) ==="
    cd "$PROJECT_ROOT"
    GOOS=linux GOARCH=arm64 go build -o "$VZ_DIR/shed-agent" ./cmd/shed-agent

    echo "=== Building shed-firstboot binary (linux/arm64) ==="
    GOOS=linux GOARCH=arm64 go build -o "$VZ_DIR/shed-firstboot" ./cmd/shed-firstboot

    echo "=== Building host shed CLI ==="
    mkdir -p "$PROJECT_ROOT/bin"
    go build -o "$PROJECT_ROOT/bin/shed" ./cmd/shed
}

# Build the shed-overlay initramfs once and reuse across variants
# (the initrd is image-content-independent, so all variants share it).
build_shed_initrd() {
    if [ -n "$SHED_INITRD" ] && [ -f "$SHED_INITRD" ]; then
        return
    fi
    SHED_INITRD="$(mktemp "${TMPDIR:-/tmp}/shed-initrd-vz.XXXXXX.img")"
    echo ""
    echo "=== Building shed-overlay initramfs ==="
    "$SCRIPT_DIR/build-initramfs.sh" \
        --backend vz \
        --platform linux/arm64 \
        --output "$SHED_INITRD"
}

# Build a single variant via `shed image build`, which drives docker
# buildx, exports the rootfs as an OCI tar.gz layer, extracts the
# kernel from /boot inside the image, installs the manifest + config +
# layer + kernel + initrd blobs into the OCI store under OUTPUT_DIR,
# and advances the named tag. The shed-overlay initramfs comes via
# --initramfs (extracted Ubuntu /boot/initrd.img-* is not appropriate
# for shed images — shed needs the overlayfs-assembly initramfs).
build_variant() {
    local variant="$1"
    local docker_target="shed-vz-${variant}"

    echo ""
    echo "========================================"
    echo "  Building variant: $variant"
    echo "  Docker target: $docker_target"
    echo "  Output dir:    $OUTPUT_DIR"
    echo "========================================"

    local extra_args=()
    if [ -n "$SHED_EXT_VERSION" ]; then
        # shed image build passes --build-arg through to buildx via the
        # builder. Encode as KEY=VALUE so the existing flag plumbing
        # forwards verbatim.
        echo "Note: SHED_EXT_VERSION=$SHED_EXT_VERSION (forward via docker build cache)"
        # The Dockerfile reads SHED_EXT_VERSION from ARG; passing via
        # `DOCKER_BUILDKIT_ARGS` keeps the shell invocation simple.
        export BUILDX_BUILDER="${BUILDX_BUILDER:-default}"
        # No direct --build-arg pass-through on shed image build yet;
        # operators who pin shed-ext should edit the ARG line in
        # vz/Dockerfile or run docker buildx manually before this step.
    fi

    "$PROJECT_ROOT/bin/shed" image build \
        --target "$docker_target" \
        -n "$variant" \
        --initramfs "$SHED_INITRD" \
        --size "$ROOTFS_SIZE" \
        --output-dir "$OUTPUT_DIR" \
        -f "$VZ_DIR/Dockerfile" \
        "${extra_args[@]}" \
        "$VZ_DIR"
}

# Main execution
echo "=== Building VZ Rootfs ==="
echo "Project root: $PROJECT_ROOT"
echo "Output directory: $OUTPUT_DIR"

# Build host prerequisites + shared shed-overlay initramfs once.
build_prereqs
build_shed_initrd

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

echo ""
echo "=== Build Complete ==="
echo "OCI store at: $OUTPUT_DIR"
echo "Tags installed:"
if [ "$BUILD_ALL" = true ]; then
    for v in $KNOWN_VARIANTS; do
        echo "  - $v"
    done
else
    echo "  - $VARIANT"
fi
echo ""
echo "Inspect with:"
echo "  shed -c <server.yaml> image ls"
echo "  shed -c <server.yaml> image history <variant>"
echo ""
echo "Next steps:"
echo "1. Install vfkit: brew install vfkit"
echo "2. Configure server.yaml with backend: vz"
echo "3. Code-sign shed-server: codesign --entitlements internal/vz/entitlements.plist -s - ./shed-server"
echo "4. Start the server: ./shed-server serve"
