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
#   ./scripts/build-firecracker-rootfs.sh                      # Build full variant
#   ./scripts/build-firecracker-rootfs.sh --variant base        # Build base variant
#   ./scripts/build-firecracker-rootfs.sh --variant extensions  # Build extensions variant
#   ./scripts/build-firecracker-rootfs.sh --variant full        # Build full variant
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
KNOWN_VARIANTS="base extensions full"

# Defaults
VARIANT="full"
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
            echo "  --variant <name>   Build a specific variant (default: full)"
            echo "                     Available variants: $KNOWN_VARIANTS"
            echo "  --all              Build all variants"
            echo "  --shed-ext-version Override shed-extensions image version for extensions/full variants"
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
SHED_INITRD=""

# Cleanup function for trap
cleanup() {
    if [ -n "$SHED_INITRD" ] && [ -f "$SHED_INITRD" ]; then
        rm -f "$SHED_INITRD"
    fi
    rm -f "$FIRECRACKER_DIR/shed-agent" "$FIRECRACKER_DIR/shed-firstboot"
}

trap cleanup EXIT

# Create output directory
sudo mkdir -p "$OUTPUT_DIR"

# Build the prerequisites the Dockerfile needs (shed-agent + shed-firstboot
# staged into the build context) and the host-side shed CLI used to
# drive the OCI conversion.
build_prereqs() {
    echo ""
    echo "=== Building shed-agent binary (linux/amd64) ==="
    cd "$PROJECT_ROOT"
    GOOS=linux GOARCH=amd64 go build -o "$FIRECRACKER_DIR/shed-agent" ./cmd/shed-agent

    echo "=== Building shed-firstboot binary (linux/amd64) ==="
    GOOS=linux GOARCH=amd64 go build -o "$FIRECRACKER_DIR/shed-firstboot" ./cmd/shed-firstboot

    echo "=== Building host shed CLI ==="
    mkdir -p "$PROJECT_ROOT/bin"
    go build -o "$PROJECT_ROOT/bin/shed" ./cmd/shed
}

# Build the shed-overlay initramfs once and reuse across variants
# (it's image-content-independent).
build_shed_initrd() {
    if [ -n "$SHED_INITRD" ] && [ -f "$SHED_INITRD" ]; then
        return
    fi
    SHED_INITRD="$(mktemp "${TMPDIR:-/tmp}/shed-initrd-fc.XXXXXX.img")"
    echo ""
    echo "=== Building shed-overlay initramfs ==="
    "$SCRIPT_DIR/build-initramfs.sh" \
        --backend firecracker \
        --platform linux/amd64 \
        --output "$SHED_INITRD"
}

# Build a single variant via `shed image build`. The Firecracker
# Dockerfile's kernel-builder stage COPYs vmlinux to /boot/vmlinux
# inside the final image, so Convert's kernel extraction (which
# already handles /boot/vmlinux) picks it up automatically — no
# external kernel path needed any more.
build_variant() {
    local variant="$1"
    local docker_target="shed-fc-${variant}"

    echo ""
    echo "========================================"
    echo "  Building variant: $variant"
    echo "  Docker target: $docker_target"
    echo "  Output dir:    $OUTPUT_DIR"
    echo "========================================"

    "$PROJECT_ROOT/bin/shed" image build \
        --target "$docker_target" \
        -n "$variant" \
        --initramfs "$SHED_INITRD" \
        --size "$ROOTFS_SIZE" \
        --output-dir "$OUTPUT_DIR" \
        -f "$FIRECRACKER_DIR/Dockerfile" \
        "$FIRECRACKER_DIR"
}

# Main execution
echo "=== Building Firecracker Rootfs ==="
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
echo "Next steps:"
echo "1. Configure server.yaml with backend: firecracker (point at $OUTPUT_DIR)"
echo "2. Set up bridge network (see docs/getting-started/fc-setup.md)"
