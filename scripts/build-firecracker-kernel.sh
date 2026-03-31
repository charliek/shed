#!/bin/bash
# Build a custom Linux kernel for Firecracker VMs with Docker support.
#
# Uses the Firecracker CI kernel config for 6.1.x as the base, which already
# includes BPF, cgroups, overlayfs, namespaces, bridge, veth, and netfilter
# support needed for Docker containers inside VMs.
#
# Prerequisites:
#   - Build tools: gcc, make, flex, bison, libelf-dev, bc, libssl-dev, git, curl, file
#   - On Debian/Ubuntu: apt install build-essential flex bison libelf-dev bc libssl-dev git curl file
#   - ~2GB disk space for kernel source, ~10 min build time on 8 cores

set -e
set -o pipefail

# Configuration
# Set KERNEL_TAG to pin a specific version (e.g., KERNEL_TAG=v6.1.102)
KERNEL_MAJOR="${KERNEL_MAJOR:-6.1}"
KERNEL_TAG="${KERNEL_TAG:-}"
OUTPUT_DIR="${OUTPUT_DIR:-/var/lib/shed/firecracker}"
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
DOCKER_FRAGMENT="${SCRIPT_DIR}/../firecracker/kernel-config-docker.fragment"

echo "=== Building Firecracker Kernel ==="
echo "Kernel series: ${KERNEL_MAJOR}.x"
echo "Output directory: ${OUTPUT_DIR}"

# Create output directory
sudo mkdir -p "$OUTPUT_DIR"

# Check prerequisites
for cmd in gcc make flex bison bc git curl file; do
    if ! command -v "$cmd" &> /dev/null; then
        echo "ERROR: $cmd is required but not found"
        echo "  Install: sudo apt install build-essential flex bison libelf-dev bc libssl-dev git curl file"
        exit 1
    fi
done

# Check for required development headers
for header in elf.h openssl/ssl.h; do
    if ! echo "#include <$header>" | gcc -E - &>/dev/null; then
        echo "ERROR: Development headers for $header not found"
        echo "  Install: sudo apt install libelf-dev libssl-dev"
        exit 1
    fi
done

# Determine latest 6.1.x tag via git ls-remote
echo ""
echo "=== Finding latest ${KERNEL_MAJOR}.x release ==="
if [ -n "$KERNEL_TAG" ]; then
    LATEST_TAG="$KERNEL_TAG"
    echo "Using pinned kernel tag: ${LATEST_TAG}"
else
    # Use a temp file to avoid SIGPIPE with pipefail when head -1 closes early
    _tags_tmp=$(mktemp)
    git ls-remote --tags --sort=-v:refname \
        https://git.kernel.org/pub/scm/linux/kernel/git/stable/linux.git \
        "v${KERNEL_MAJOR}.*" > "$_tags_tmp" 2>/dev/null || true
    LATEST_TAG=$(grep -v '\^{}' "$_tags_tmp" | head -1 | sed 's|.*refs/tags/||')
    rm -f "$_tags_tmp"
fi

if [ -z "$LATEST_TAG" ]; then
    echo "ERROR: could not find a ${KERNEL_MAJOR}.x kernel tag"
    exit 1
fi
echo "Latest tag: ${LATEST_TAG}"

# Shallow clone kernel source
BUILD_DIR=$(mktemp -d)
trap 'rm -rf "$BUILD_DIR"' EXIT

echo ""
echo "=== Cloning kernel source (shallow) ==="
git clone --depth 1 --branch "$LATEST_TAG" \
    https://git.kernel.org/pub/scm/linux/kernel/git/stable/linux.git \
    "$BUILD_DIR/linux"

cd "$BUILD_DIR/linux"

# Detect architecture
ARCH=$(uname -m)
case "$ARCH" in
    x86_64)  FC_ARCH="x86_64" ;;
    aarch64) FC_ARCH="aarch64" ;;
    *)
        echo "ERROR: Unsupported architecture: $ARCH"
        exit 1
        ;;
esac

# Download Firecracker CI config for 6.1
echo ""
echo "=== Downloading Firecracker CI kernel config (${FC_ARCH}) ==="
FC_VERSION="${FC_VERSION:-v1.14.1}"
FC_CONFIG_URL="https://raw.githubusercontent.com/firecracker-microvm/firecracker/${FC_VERSION}/resources/guest_configs/microvm-kernel-ci-${FC_ARCH}-6.1.config"
curl -fL -o .config "$FC_CONFIG_URL"
echo "Downloaded: microvm-kernel-ci-${FC_ARCH}-6.1.config"

# Merge Docker config fragment if it exists and is non-empty
if [ -f "$DOCKER_FRAGMENT" ]; then
    # Filter out comments and blank lines
    FRAGMENT_CONFIGS=$(grep -v '^#' "$DOCKER_FRAGMENT" | grep -v '^$' || true)
    if [ -n "$FRAGMENT_CONFIGS" ]; then
        echo ""
        echo "=== Merging Docker config fragment ==="
        # Use scripts/kconfig/merge_config.sh if available, otherwise append + olddefconfig
        if [ -x scripts/kconfig/merge_config.sh ]; then
            scripts/kconfig/merge_config.sh -m .config "$DOCKER_FRAGMENT"
        else
            echo "$FRAGMENT_CONFIGS" >> .config
        fi
    fi
fi

# Resolve any config conflicts
echo ""
echo "=== Resolving config (olddefconfig) ==="
make olddefconfig

# Build vmlinux
echo ""
echo "=== Building vmlinux ($(nproc) cores) ==="
make -j"$(nproc)" vmlinux

# Copy to output
echo ""
echo "=== Installing kernel ==="
sudo cp vmlinux "$OUTPUT_DIR/vmlinux.bin"
echo "Installed kernel to $OUTPUT_DIR/vmlinux.bin"

# Verify
echo ""
echo "=== Verification ==="
file "$OUTPUT_DIR/vmlinux.bin"
echo ""
echo "Kernel version: ${LATEST_TAG}"
echo ""
echo "=== Build Complete ==="
echo ""
echo "Next steps:"
echo "1. Update kernel_path in server.yaml to: $OUTPUT_DIR/vmlinux.bin"
echo "2. Restart shed-server"
echo "3. New VMs will use the updated kernel"
