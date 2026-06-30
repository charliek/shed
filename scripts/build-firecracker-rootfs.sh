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

# Built-in variants surfaced by --all and --help. Explicit --variant values
# are forwarded to Docker so custom shed-fc-<name> stages can be built too.
KNOWN_VARIANTS="base extensions full"

# Defaults
VARIANT="full"
BUILD_ALL=false
SHED_EXT_VERSION=""
BUILD_TOOLS_VERSION=""

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
        --build-tools-version)
            # Forwarded to `shed image build`. Pins the
            # shed-build-tools image used to mint the rootfs erofs.
            # Default is derived from the shed CLI's version (see
            # buildToolsRefDefault in cmd/shed/image.go); pass `dev`
            # when iterating against a `make build-tools`-built image.
            if [[ $# -lt 2 || "$2" == --* ]]; then
                echo "ERROR: --build-tools-version requires a value"
                echo "Run '$0 --help' for usage."
                exit 1
            fi
            BUILD_TOOLS_VERSION="$2"
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
            echo "  --build-tools-version  Override the shed-build-tools image tag used to mint the rootfs erofs"
            echo "                         (e.g., 'dev' to use a locally-built shed-build-tools:dev image)"
            echo "  --help, -h         Show this help message"
            echo ""
            echo "Environment variables:"
            echo "  OUTPUT_DIR         Output directory (default: /var/lib/shed/firecracker/images)"
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

    local extra_args=()
    if [ -n "$BUILD_TOOLS_VERSION" ]; then
        extra_args+=(--build-tools-version "$BUILD_TOOLS_VERSION")
    fi

    # The ref-index resolves `shed create --image <ref>` to a manifest, so the
    # built image MUST be recorded under the ref the server is configured for.
    # Honor SHED_SOURCE_REF (parallel-dev points it at image_aliases.base);
    # otherwise derive the release ref from `shed version`. Without this the FC
    # build records `shed-fc-<variant>:latest`, which `--image base` can't
    # resolve (mirrors build-vz-rootfs.sh).
    local source_ref
    if [ -n "${SHED_SOURCE_REF:-}" ]; then
        source_ref="$SHED_SOURCE_REF"
    else
        local version
        version="$("$PROJECT_ROOT/bin/shed" version 2>/dev/null | awk '{print $2}')"
        version="${version:-dev}"
        source_ref="ghcr.io/charliek/shed-fc-${variant}:${version}"
    fi

    "$PROJECT_ROOT/bin/shed" image build \
        --target "$docker_target" \
        -n "$variant" \
        --initramfs "$SHED_INITRD" \
        --output-dir "$OUTPUT_DIR" \
        --source-ref "$source_ref" \
        -f "$FIRECRACKER_DIR/Dockerfile" \
        "${extra_args[@]}" \
        "$FIRECRACKER_DIR" || return $?

    # Helpful pointer so server config can be aligned. Important: if
    # server.yaml's images.<variant> doesn't match this source-ref,
    # `shed create --image <variant>` will fall through to a registry
    # pull and OVERWRITE this manifest. (Mirrors build-vz-rootfs.sh.)
    echo "Tip: ensure ~/.config/shed/server.yaml has 'images.${variant}: $source_ref'"
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
