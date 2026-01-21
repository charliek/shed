#!/bin/bash
set -e

# Detect OS
OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
case "$OS" in
    darwin)
        OS="darwin"
        ;;
    linux)
        OS="linux"
        ;;
    *)
        echo "Error: Unsupported operating system: $OS"
        exit 1
        ;;
esac

# Detect architecture
ARCH="$(uname -m)"
case "$ARCH" in
    x86_64)
        ARCH="amd64"
        ;;
    aarch64|arm64)
        ARCH="arm64"
        ;;
    *)
        echo "Error: Unsupported architecture: $ARCH"
        exit 1
        ;;
esac

echo "Detected: ${OS}/${ARCH}"

# GitHub repository
REPO="charliek/shed"

# Get latest release version
echo "Fetching latest release..."
LATEST_VERSION=$(curl -s "https://api.github.com/repos/${REPO}/releases/latest" | grep '"tag_name"' | sed -E 's/.*"([^"]+)".*/\1/')

if [ -z "$LATEST_VERSION" ]; then
    echo "Error: Could not determine latest version"
    exit 1
fi

echo "Latest version: ${LATEST_VERSION}"

# Download URL
FILENAME="shed_${LATEST_VERSION#v}_${OS}_${ARCH}.tar.gz"
DOWNLOAD_URL="https://github.com/${REPO}/releases/download/${LATEST_VERSION}/${FILENAME}"

# Create temporary directory
TMP_DIR=$(mktemp -d)
trap "rm -rf $TMP_DIR" EXIT

# Download and extract
echo "Downloading ${FILENAME}..."
curl -sL "$DOWNLOAD_URL" -o "${TMP_DIR}/${FILENAME}"

echo "Extracting..."
tar -xzf "${TMP_DIR}/${FILENAME}" -C "$TMP_DIR"

# Install to /usr/local/bin
INSTALL_DIR="${INSTALL_DIR:-/usr/local/bin}"

if [ -w "$INSTALL_DIR" ]; then
    mv "${TMP_DIR}/shed" "$INSTALL_DIR/"
    mv "${TMP_DIR}/shed-server" "$INSTALL_DIR/" 2>/dev/null || true
else
    echo "Installing to ${INSTALL_DIR} (requires sudo)..."
    sudo mv "${TMP_DIR}/shed" "$INSTALL_DIR/"
    sudo mv "${TMP_DIR}/shed-server" "$INSTALL_DIR/" 2>/dev/null || true
fi

echo ""
echo "✓ Shed ${LATEST_VERSION} installed successfully!"
echo ""
echo "To get started, run:"
echo "  shed --help"
