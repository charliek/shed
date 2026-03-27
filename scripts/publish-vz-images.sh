#!/bin/bash
# Publish VZ Docker images to a container registry.
#
# Builds all VZ image variants and pushes them to the configured registry.
# Requires Docker with buildx support and registry authentication
# (e.g., `docker login ghcr.io` or `echo $TOKEN | docker login ghcr.io -u USERNAME --password-stdin`).
#
# Usage:
#   ./scripts/publish-vz-images.sh --version v1.0.0                # publish all variants
#   ./scripts/publish-vz-images.sh --version v1.0.0 --variant base # publish one variant
#   ./scripts/publish-vz-images.sh --version v1.0.0 --dry-run      # build only, don't push
#   ./scripts/publish-vz-images.sh --version v1.0.0 --registry ghcr.io/myorg  # custom registry

set -e
set -o pipefail

# Configuration
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(dirname "$SCRIPT_DIR")"
VZ_DIR="$PROJECT_ROOT/vz"
REGISTRY="${REGISTRY:-ghcr.io/charliek}"
KNOWN_VARIANTS="base devtools default typescript"

# Defaults
VERSION=""
VARIANT=""
BUILD_ALL=true
DRY_RUN=false

# Parse arguments
while [[ $# -gt 0 ]]; do
    case "$1" in
        --version)
            if [[ $# -lt 2 || "$2" == --* ]]; then
                echo "ERROR: --version requires a value (e.g., v1.0.0)"
                exit 1
            fi
            VERSION="$2"
            shift 2
            ;;
        --variant)
            if [[ $# -lt 2 || "$2" == --* ]]; then
                echo "ERROR: --variant requires a value"
                exit 1
            fi
            VARIANT="$2"
            BUILD_ALL=false
            shift 2
            ;;
        --registry)
            if [[ $# -lt 2 || "$2" == --* ]]; then
                echo "ERROR: --registry requires a value"
                exit 1
            fi
            REGISTRY="$2"
            shift 2
            ;;
        --dry-run)
            DRY_RUN=true
            shift
            ;;
        --help|-h)
            echo "Usage: $0 --version <version> [OPTIONS]"
            echo ""
            echo "Build and publish VZ Docker images to a container registry."
            echo ""
            echo "Options:"
            echo "  --version <version>  Version tag (default: derived from git describe)"
            echo ""
            echo "Options:"
            echo "  --variant <name>     Publish a specific variant only"
            echo "                       Available: $KNOWN_VARIANTS"
            echo "  --registry <prefix>  Registry prefix (default: ghcr.io/charliek)"
            echo "  --dry-run            Build images locally without pushing"
            echo "  --help, -h           Show this help message"
            echo ""
            echo "Environment variables:"
            echo "  REGISTRY             Registry prefix (same as --registry flag)"
            echo ""
            echo "Prerequisites:"
            echo "  - Docker with buildx support"
            echo "  - Registry authentication (docker login)"
            echo "  - Go toolchain (for cross-compiling shed-agent)"
            exit 0
            ;;
        *)
            echo "ERROR: Unknown argument: $1"
            echo "Run '$0 --help' for usage."
            exit 1
            ;;
    esac
done

if [ -z "$VERSION" ]; then
    VERSION=$(git describe --tags --always 2>/dev/null)
    if [ -z "$VERSION" ]; then
        echo "ERROR: --version is required (no git tags found to derive from)"
        echo "Run '$0 --help' for usage."
        exit 1
    fi
    echo "Derived version from git: $VERSION"
fi

# Build shed-agent binary for linux/arm64
build_agent() {
    echo ""
    echo "=== Building shed-agent binary (linux/arm64) ==="
    cd "$PROJECT_ROOT"
    GOOS=linux GOARCH=arm64 go build -o "$VZ_DIR/shed-agent" ./cmd/shed-agent
    echo "Built shed-agent binary"
}

# Publish a single variant
publish_variant() {
    local variant="$1"
    local docker_target="shed-vz-${variant}"
    local image_name="${REGISTRY}/shed-vz-${variant}"

    echo ""
    echo "========================================"
    echo "  Variant:  $variant"
    echo "  Target:   $docker_target"
    echo "  Image:    $image_name:$VERSION"
    echo "  Dry run:  $DRY_RUN"
    echo "========================================"

    cd "$VZ_DIR"

    if [ "$DRY_RUN" = true ]; then
        echo ""
        echo "=== Building (dry run — load locally, no push) ==="
        docker buildx build \
            --platform linux/arm64 \
            --target "$docker_target" \
            -t "$image_name:$VERSION" \
            -t "$image_name:latest" \
            --load \
            .
        echo "Built $image_name:$VERSION (local only)"
    else
        echo ""
        echo "=== Building and pushing ==="
        docker buildx build \
            --platform linux/arm64 \
            --target "$docker_target" \
            -t "$image_name:$VERSION" \
            -t "$image_name:latest" \
            --push \
            .
        echo "Pushed $image_name:$VERSION and $image_name:latest"
    fi
}

# Main execution
echo "=== Publish VZ Docker Images ==="
echo "Registry: $REGISTRY"
echo "Version:  $VERSION"
echo "Dry run:  $DRY_RUN"

# Build the agent binary first (shared across all variants)
build_agent

if [ "$BUILD_ALL" = true ]; then
    echo ""
    echo "Publishing all variants: $KNOWN_VARIANTS"
    for v in $KNOWN_VARIANTS; do
        publish_variant "$v"
    done
else
    publish_variant "$VARIANT"
fi

# Clean up the shed-agent binary from the build directory
rm -f "$VZ_DIR/shed-agent"

echo ""
echo "=== Publish Complete ==="
if [ "$BUILD_ALL" = true ]; then
    for v in $KNOWN_VARIANTS; do
        if [ "$DRY_RUN" = true ]; then
            echo "  ${REGISTRY}/shed-vz-${v}:${VERSION} (local only)"
        else
            echo "  ${REGISTRY}/shed-vz-${v}:${VERSION}"
        fi
    done
else
    if [ "$DRY_RUN" = true ]; then
        echo "  ${REGISTRY}/shed-vz-${VARIANT}:${VERSION} (local only)"
    else
        echo "  ${REGISTRY}/shed-vz-${VARIANT}:${VERSION}"
    fi
fi
