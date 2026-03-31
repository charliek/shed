#!/bin/bash
# Download Firecracker binary and kernel
# This script downloads the latest Firecracker release and a compatible kernel

set -e
set -o pipefail

# Configuration
FIRECRACKER_VERSION="${FIRECRACKER_VERSION:-v1.14.1}"
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

# Find checksums file (SHA256SUMS in archive, or download checksums.txt as fallback)
CHECKSUMS_FILE=$(find "$TMP_DIR" -name "SHA256SUMS" -type f | head -1)
if [ -z "$CHECKSUMS_FILE" ]; then
    CHECKSUM_URL="https://github.com/firecracker-microvm/firecracker/releases/download/${FIRECRACKER_VERSION}/checksums.txt"
    if curl -fL -o "$TMP_DIR/checksums.txt" "$CHECKSUM_URL" 2>/dev/null; then
        CHECKSUMS_FILE="$TMP_DIR/checksums.txt"
    fi
fi

# Find and copy binaries
FC_BIN=$(find "$TMP_DIR" -name "firecracker-${FIRECRACKER_VERSION}-*" -not -name "*.debug" -not -name "*.yaml" -type f | head -1)
JAILER_BIN=$(find "$TMP_DIR" -name "jailer-${FIRECRACKER_VERSION}-*" -not -name "*.debug" -type f | head -1)

if [ -z "$FC_BIN" ]; then
    echo "ERROR: firecracker binary not found in archive"
    exit 1
fi
if [ -z "$JAILER_BIN" ]; then
    echo "ERROR: jailer binary not found in archive"
    exit 1
fi

# verify_checksum extracts the expected checksum for a binary from the checksums file.
# Handles both "hash  ./filename" (SHA256SUMS) and "hash  filename" formats.
verify_checksum() {
    local bin_path="$1"
    local checksums_file="$2"
    local basename
    basename=$(basename "$bin_path")

    if [ -z "$checksums_file" ]; then
        return
    fi

    # Match filename at end of line, with optional ./ prefix
    local expected_sum
    expected_sum=$(grep -E "[[:space:]]\\.?/?${basename}$" "$checksums_file" | awk '{print $1}' || true)

    if [ -n "$expected_sum" ]; then
        local actual_sum
        actual_sum=$(sha256sum "$bin_path" | awk '{print $1}')
        if [ "$expected_sum" != "$actual_sum" ]; then
            echo "ERROR: checksum mismatch for $basename"
            echo "  expected: $expected_sum"
            echo "  actual:   $actual_sum"
            exit 1
        fi
        echo "Checksum verified for $basename"
    else
        echo "WARNING: no checksum found for $basename, skipping verification"
    fi
}

if [ -n "$FC_BIN" ]; then
    verify_checksum "$FC_BIN" "$CHECKSUMS_FILE"
    sudo cp "$FC_BIN" /usr/local/bin/firecracker
    sudo chmod +x /usr/local/bin/firecracker
    echo "Installed firecracker to /usr/local/bin/firecracker"
fi

if [ -n "$JAILER_BIN" ]; then
    verify_checksum "$JAILER_BIN" "$CHECKSUMS_FILE"
    sudo cp "$JAILER_BIN" /usr/local/bin/jailer
    sudo chmod +x /usr/local/bin/jailer
    echo "Installed jailer to /usr/local/bin/jailer"
fi

# Kernel
echo ""
echo "=== Kernel ==="

if [ -f "$OUTPUT_DIR/vmlinux.bin" ]; then
    echo "Kernel already exists at $OUTPUT_DIR/vmlinux.bin"
    echo "To rebuild, run: ./scripts/build-firecracker-kernel.sh"
else
    echo "No kernel found at $OUTPUT_DIR/vmlinux.bin"
    echo ""
    echo "Downloading Firecracker CI 6.1 kernel as quick-start fallback..."
    echo "(For full Docker support, build a custom kernel: ./scripts/build-firecracker-kernel.sh)"
    # The v1.9 in the URL is the CI artifact path, not the Firecracker version.
    # This 6.1 kernel is compatible with any recent Firecracker release.
    # It is a quick-start fallback; for full Docker support, use build-firecracker-kernel.sh.
    CI_KERNEL_URL="https://s3.amazonaws.com/spec.ccfc.min/firecracker-ci/v1.9/${FC_ARCH}/vmlinux-6.1.102"
    sudo curl -fL -o "$OUTPUT_DIR/vmlinux.bin" "$CI_KERNEL_URL"
    echo "Downloaded CI kernel to $OUTPUT_DIR/vmlinux.bin"
fi

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
echo "1. (Optional) Build Docker-capable kernel: ./scripts/build-firecracker-kernel.sh"
echo "2. Build rootfs: ./scripts/build-firecracker-rootfs.sh"
echo "3. Set up bridge network (see docs/getting-started/fc-setup.md)"
