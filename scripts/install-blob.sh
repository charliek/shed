#!/bin/bash
# Install a freshly-built rootfs.ext4 (plus kernel and initrd) into the
# content-addressed blob store under {imagesDir}/blobs/sha256/<digest>/
# and update {imagesDir}/tags/<tag>.json.
#
# This mirrors internal/vmimage/blobstore.go's InstallBlob so the layout
# matches what the runtime expects: the digest is sha256(rootfs.ext4),
# files are 0o444, and install is atomic via a sibling .tmp dir + rename.
#
# Usage:
#   ./scripts/install-blob.sh \
#       --images-dir <dir> \
#       --rootfs    <rootfs.ext4> \
#       --kernel    <vmlinux>            (optional)
#       --initrd    <initrd>             (optional)
#       --tag       <tag-name>           (optional; updates tags/<tag>.json)
#       --backend   <vz|firecracker>     (recorded in manifest)
#       --arch      <arm64|amd64>        (recorded in manifest)
#       --source-ref <docker ref>        (optional, recorded in manifest)
#
# Prints the digest on stdout (form: sha256:<64hex>).

set -euo pipefail

IMAGES_DIR=""
ROOTFS=""
KERNEL=""
INITRD=""
TAG=""
BACKEND=""
ARCH=""
SOURCE_REF=""

while [[ $# -gt 0 ]]; do
    case "$1" in
        --images-dir) IMAGES_DIR="$2"; shift 2 ;;
        --rootfs)     ROOTFS="$2";     shift 2 ;;
        --kernel)     KERNEL="$2";     shift 2 ;;
        --initrd)     INITRD="$2";     shift 2 ;;
        --tag)        TAG="$2";        shift 2 ;;
        --backend)    BACKEND="$2";    shift 2 ;;
        --arch)       ARCH="$2";       shift 2 ;;
        --source-ref) SOURCE_REF="$2"; shift 2 ;;
        *) echo "ERROR: unknown argument: $1" >&2; exit 2 ;;
    esac
done

if [ -z "$IMAGES_DIR" ] || [ -z "$ROOTFS" ]; then
    echo "ERROR: --images-dir and --rootfs are required" >&2
    exit 2
fi
if [ ! -f "$ROOTFS" ]; then
    echo "ERROR: rootfs file not found: $ROOTFS" >&2
    exit 2
fi

# Compute digest using whatever sha256 tool the host has. Linux ships
# sha256sum; macOS ships shasum -a 256. Both produce "<hex>  <path>".
if command -v sha256sum >/dev/null 2>&1; then
    DIGEST_HEX="$(sha256sum "$ROOTFS" | awk '{print $1}')"
elif command -v shasum >/dev/null 2>&1; then
    DIGEST_HEX="$(shasum -a 256 "$ROOTFS" | awk '{print $1}')"
else
    echo "ERROR: need sha256sum or shasum on PATH" >&2
    exit 1
fi

DIGEST="sha256:${DIGEST_HEX}"
BLOB_PARENT="$IMAGES_DIR/blobs/sha256"
BLOB_DIR="$BLOB_PARENT/$DIGEST_HEX"
TMP_DIR="$BLOB_DIR.tmp"

mkdir -p "$BLOB_PARENT"

# move_into copies src to dst when cross-device or when the source must
# be preserved; otherwise rename moves it. We rename to consume source
# files so the caller is not left with stray top-level artifacts (which
# the blob store's prune/df flows would never reclaim).
move_into() {
    local src="$1" dst="$2"
    if mv "$src" "$dst" 2>/dev/null; then
        return 0
    fi
    # Cross-device fallback: copy then unlink the source.
    cp "$src" "$dst" || return 1
    rm -f "$src"
}

# If the blob already exists at this digest, the install is a no-op.
# We still consume the source files so the caller doesn't leak them.
if [ -f "$BLOB_DIR/rootfs.ext4" ]; then
    echo "shed-blob: $DIGEST already installed at $BLOB_DIR" >&2
    rm -f "$ROOTFS"
    [ -n "$KERNEL" ] && rm -f "$KERNEL" || true
    [ -n "$INITRD" ] && rm -f "$INITRD" || true
else
    rm -rf "$TMP_DIR"
    mkdir -p "$TMP_DIR"

    move_into "$ROOTFS" "$TMP_DIR/rootfs.ext4" \
        || { echo "ERROR: failed to stage rootfs" >&2; rm -rf "$TMP_DIR"; exit 1; }
    chmod 0444 "$TMP_DIR/rootfs.ext4"

    if [ -n "$KERNEL" ]; then
        if [ ! -f "$KERNEL" ]; then
            echo "ERROR: kernel file not found: $KERNEL" >&2
            rm -rf "$TMP_DIR"; exit 2
        fi
        move_into "$KERNEL" "$TMP_DIR/kernel" \
            || { echo "ERROR: failed to stage kernel" >&2; rm -rf "$TMP_DIR"; exit 1; }
        chmod 0444 "$TMP_DIR/kernel"
    fi

    if [ -n "$INITRD" ]; then
        if [ ! -f "$INITRD" ]; then
            echo "ERROR: initrd file not found: $INITRD" >&2
            rm -rf "$TMP_DIR"; exit 2
        fi
        move_into "$INITRD" "$TMP_DIR/initrd" \
            || { echo "ERROR: failed to stage initrd" >&2; rm -rf "$TMP_DIR"; exit 1; }
        chmod 0444 "$TMP_DIR/initrd"
    fi

    # Manifest. Read sizes from the staged copies — the source files
    # have already been moved into $TMP_DIR.
    rootfs_logical="$(wc -c < "$TMP_DIR/rootfs.ext4" | tr -d ' ')"
    kernel_size="0"
    [ -n "$KERNEL" ] && kernel_size="$(wc -c < "$TMP_DIR/kernel" | tr -d ' ')"
    initrd_size="0"
    [ -n "$INITRD" ] && initrd_size="$(wc -c < "$TMP_DIR/initrd" | tr -d ' ')"
    created_at="$(date -u +"%Y-%m-%dT%H:%M:%S.000000000Z")"

    cat > "$TMP_DIR/manifest.json" <<EOF
{
  "schema_version": 1,
  "digest": "$DIGEST",
  "backend": "$BACKEND",
  "arch": "$ARCH",
  "source_ref": "$SOURCE_REF",
  "kernel_size": $kernel_size,
  "initrd_size": $initrd_size,
  "rootfs_logical_size": $rootfs_logical,
  "created_at": "$created_at"
}
EOF
    chmod 0444 "$TMP_DIR/manifest.json"

    # Atomic rename — same parent dir guarantees no partial install is
    # ever visible at $BLOB_DIR.
    mv "$TMP_DIR" "$BLOB_DIR"
    echo "shed-blob: installed $DIGEST at $BLOB_DIR" >&2
fi

# Tag advancement (best-effort, atomic via tmp + rename).
if [ -n "$TAG" ]; then
    TAGS_DIR="$IMAGES_DIR/tags"
    mkdir -p "$TAGS_DIR"
    TAG_FILE="$TAGS_DIR/$TAG.json"
    TAG_TMP="$TAG_FILE.tmp"
    updated_at="$(date -u +"%Y-%m-%dT%H:%M:%S.000000000Z")"
    cat > "$TAG_TMP" <<EOF
{
  "digest": "$DIGEST",
  "updated_at": "$updated_at"
}
EOF
    mv "$TAG_TMP" "$TAG_FILE"
    echo "shed-blob: tag $TAG -> $DIGEST" >&2
fi

echo "$DIGEST"
