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
curl -L "$FC_URL" | tar -xz -C "$TMP_DIR"

# Find and copy binaries
FC_BIN=$(find "$TMP_DIR" -name "firecracker-*" -type f | head -1)
JAILER_BIN=$(find "$TMP_DIR" -name "jailer-*" -type f | head -1)

if [ -n "$FC_BIN" ]; then
    sudo cp "$FC_BIN" /usr/local/bin/firecracker
    sudo chmod +x /usr/local/bin/firecracker
    echo "Installed firecracker to /usr/local/bin/firecracker"
fi

if [ -n "$JAILER_BIN" ]; then
    sudo cp "$JAILER_BIN" /usr/local/bin/jailer
    sudo chmod +x /usr/local/bin/jailer
    echo "Installed jailer to /usr/local/bin/jailer"
fi

rm -rf "$TMP_DIR"

# Download kernel
echo ""
echo "=== Downloading kernel ==="

# Use the Firecracker-provided kernel
KERNEL_URL="https://s3.amazonaws.com/spec.ccfc.min/ci-artifacts/kernels/${FC_ARCH}/vmlinux-5.10.217.bin"
echo "URL: $KERNEL_URL"

sudo curl -L -o "$OUTPUT_DIR/vmlinux.bin" "$KERNEL_URL"
echo "Downloaded kernel to $OUTPUT_DIR/vmlinux.bin"

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
