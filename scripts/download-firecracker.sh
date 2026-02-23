#!/bin/bash
# Download Firecracker binary and kernel
# This script downloads the latest Firecracker release and a compatible kernel

set -e
set -o pipefail

# Configuration
FIRECRACKER_VERSION="${FIRECRACKER_VERSION:-v1.6.0}"
OUTPUT_DIR="${OUTPUT_DIR:-/var/lib/shed/firecracker}"

echo "=== Downloading Firecracker ==="
echo "Version: $FIRECRACKER_VERSION"
echo "Output directory: $OUTPUT_DIR"

# Create output directory
sudo mkdir -p "$OUTPUT_DIR"

# Detect architecture
ARCH=$(uname -m)
case "$ARCH" in
    x86_64)
        FC_ARCH="x86_64"
        ;;
    aarch64)
        FC_ARCH="aarch64"
        ;;
    *)
        echo "Unsupported architecture: $ARCH"
        exit 1
        ;;
esac

echo "Architecture: $FC_ARCH"

# Download Firecracker binary
echo ""
echo "=== Downloading Firecracker binary ==="
FC_URL="https://github.com/firecracker-microvm/firecracker/releases/download/${FIRECRACKER_VERSION}/firecracker-${FIRECRACKER_VERSION}-${FC_ARCH}.tgz"
echo "URL: $FC_URL"

TMP_DIR=$(mktemp -d)
trap 'rm -rf "$TMP_DIR"' EXIT
curl -fL "$FC_URL" | tar -xz -C "$TMP_DIR"

# Download checksums
CHECKSUM_URL="https://github.com/firecracker-microvm/firecracker/releases/download/${FIRECRACKER_VERSION}/checksums.txt"
curl -fL -o "$TMP_DIR/checksums.txt" "$CHECKSUM_URL"

# Find and copy binaries
FC_BIN=$(find "$TMP_DIR" -name "firecracker-*" -type f | head -1)
JAILER_BIN=$(find "$TMP_DIR" -name "jailer-*" -type f | head -1)

if [ -z "$FC_BIN" ]; then
    echo "ERROR: firecracker binary not found in archive"
    exit 1
fi
if [ -z "$JAILER_BIN" ]; then
    echo "ERROR: jailer binary not found in archive"
    exit 1
fi

if [ -n "$FC_BIN" ]; then
    FC_BASENAME=$(basename "$FC_BIN")
    EXPECTED_SUM=$(grep " ${FC_BASENAME}$" "$TMP_DIR/checksums.txt" | awk '{print $1}')
    if [ -z "$EXPECTED_SUM" ]; then
        echo "ERROR: missing checksum for $FC_BASENAME"
        exit 1
    fi
    ACTUAL_SUM=$(sha256sum "$FC_BIN" | awk '{print $1}')
    if [ "$EXPECTED_SUM" != "$ACTUAL_SUM" ]; then
        echo "ERROR: checksum mismatch for $FC_BASENAME"
        exit 1
    fi
    sudo cp "$FC_BIN" /usr/local/bin/firecracker
    sudo chmod +x /usr/local/bin/firecracker
    echo "Installed firecracker to /usr/local/bin/firecracker"
fi

if [ -n "$JAILER_BIN" ]; then
    JAILER_BASENAME=$(basename "$JAILER_BIN")
    EXPECTED_SUM=$(grep " ${JAILER_BASENAME}$" "$TMP_DIR/checksums.txt" | awk '{print $1}')
    if [ -z "$EXPECTED_SUM" ]; then
        echo "ERROR: missing checksum for $JAILER_BASENAME"
        exit 1
    fi
    ACTUAL_SUM=$(sha256sum "$JAILER_BIN" | awk '{print $1}')
    if [ "$EXPECTED_SUM" != "$ACTUAL_SUM" ]; then
        echo "ERROR: checksum mismatch for $JAILER_BASENAME"
        exit 1
    fi
    sudo cp "$JAILER_BIN" /usr/local/bin/jailer
    sudo chmod +x /usr/local/bin/jailer
    echo "Installed jailer to /usr/local/bin/jailer"
fi

# Download kernel
echo ""
echo "=== Downloading kernel ==="

# Use the Ignite kernel which has BPF/cgroup support for Docker containers
# This kernel is extracted from the weaveworks/ignite-kernel Docker image
IGNITE_KERNEL_VERSION="5.10.51"
IGNITE_IMAGE="weaveworks/ignite-kernel:${IGNITE_KERNEL_VERSION}"

echo "Extracting kernel from Docker image: $IGNITE_IMAGE"
echo "(This kernel has BPF and cgroup support for Docker containers)"

# Pull the image
docker pull "$IGNITE_IMAGE"

# Create a temporary container and extract the kernel
CONTAINER_ID=$(docker create "$IGNITE_IMAGE" /bin/true)
docker cp "$CONTAINER_ID:/boot/vmlinux-${IGNITE_KERNEL_VERSION}" "$OUTPUT_DIR/vmlinux.bin"
docker rm "$CONTAINER_ID"

echo "Extracted kernel to $OUTPUT_DIR/vmlinux.bin"

# Also download the minimal Firecracker kernel as a fallback
echo ""
echo "=== Downloading minimal kernel (fallback) ==="
KERNEL_URL="https://s3.amazonaws.com/spec.ccfc.min/ci-artifacts/kernels/${FC_ARCH}/vmlinux-5.10.217.bin"
echo "URL: $KERNEL_URL"

sudo curl -fL -o "$OUTPUT_DIR/vmlinux-minimal.bin" "$KERNEL_URL"
KERNEL_SHA_URL="${KERNEL_URL}.sha256"
if curl -fL -o "$TMP_DIR/vmlinux-minimal.bin.sha256" "$KERNEL_SHA_URL"; then
    EXPECTED_KERNEL_SUM=$(awk '{print $1}' "$TMP_DIR/vmlinux-minimal.bin.sha256")
    ACTUAL_KERNEL_SUM=$(sha256sum "$OUTPUT_DIR/vmlinux-minimal.bin" | awk '{print $1}')
    if [ "$EXPECTED_KERNEL_SUM" != "$ACTUAL_KERNEL_SUM" ]; then
        echo "ERROR: checksum mismatch for vmlinux-minimal.bin"
        exit 1
    fi
else
    echo "WARNING: no checksum available for vmlinux-minimal.bin"
fi
echo "Downloaded minimal kernel to $OUTPUT_DIR/vmlinux-minimal.bin"
echo "(Use this if you don't need Docker container support)"

# Verify installation
echo ""
echo "=== Verifying installation ==="
if command -v firecracker &> /dev/null; then
    echo "Firecracker version:"
    firecracker --version
else
    echo "WARNING: firecracker not found in PATH"
fi

# Check KVM access
echo ""
if [ -c /dev/kvm ]; then
    if [ -r /dev/kvm ] && [ -w /dev/kvm ]; then
        echo "KVM: Available and accessible"
    else
        echo "KVM: Available but not accessible (check permissions)"
        echo "  Run: sudo usermod -aG kvm $USER"
    fi
else
    echo "KVM: Not available"
    echo "  Firecracker requires KVM. Enable it in your system/VM settings."
fi

echo ""
echo "=== Download Complete ==="
echo ""
echo "Files:"
echo "  Firecracker: /usr/local/bin/firecracker"
echo "  Kernel: $OUTPUT_DIR/vmlinux.bin"
echo ""
echo "Next steps:"
echo "1. Build rootfs: ./scripts/build-firecracker-rootfs.sh"
echo "2. Set up bridge network (see docs/firecracker_install.md)"
