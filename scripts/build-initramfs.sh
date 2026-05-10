#!/bin/bash
# Build the shed initramfs by invoking the `shed-initramfs` stage of
# initramfs/Dockerfile. The output is a single gzipped cpio archive
# extracted to the path passed in --output.
#
# Usage:
#   ./scripts/build-initramfs.sh \
#       --platform <linux/arm64|linux/amd64> \
#       --output <path>
#
# `--backend` is accepted for symmetry with caller scripts but is
# informational only — the same initramfs serves both backends.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(dirname "$SCRIPT_DIR")"

BACKEND=""
PLATFORM=""
OUTPUT=""

require_value() {
    local flag="$1"
    local val="${2:-}"
    if [ -z "$val" ] || [[ "$val" == --* ]]; then
        echo "ERROR: $flag requires a value" >&2
        exit 2
    fi
}

while [[ $# -gt 0 ]]; do
    case "$1" in
        --backend)
            require_value "--backend" "${2:-}"
            BACKEND="$2"; shift 2 ;;
        --platform)
            require_value "--platform" "${2:-}"
            PLATFORM="$2"; shift 2 ;;
        --output)
            require_value "--output" "${2:-}"
            OUTPUT="$2"; shift 2 ;;
        --help|-h)
            sed -n '2,15p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
            exit 0 ;;
        *)
            echo "ERROR: unknown argument: $1" >&2
            exit 2 ;;
    esac
done

if [ -z "$PLATFORM" ] || [ -z "$OUTPUT" ]; then
    echo "ERROR: --platform and --output are required" >&2
    exit 2
fi

DOCKERFILE="$PROJECT_ROOT/initramfs/Dockerfile"
if [ ! -f "$DOCKERFILE" ]; then
    echo "ERROR: $DOCKERFILE not found" >&2
    exit 2
fi

OUTPUT_DIR="$(dirname "$OUTPUT")"
mkdir -p "$OUTPUT_DIR"

# Build the shed-initramfs target into a tmp dir, then move /initrd.img
# to the requested output. `--output type=local` writes one file per
# top-level path in the scratch stage, so we get exactly initrd.img.
TMP_OUT="$(mktemp -d)"
trap 'rm -rf "$TMP_OUT"' EXIT

echo "shed-initramfs: building from $DOCKERFILE (backend=${BACKEND:-any} platform=$PLATFORM)" >&2
docker buildx build \
    --platform "$PLATFORM" \
    --file "$DOCKERFILE" \
    --target shed-initramfs \
    --output "type=local,dest=$TMP_OUT" \
    "$PROJECT_ROOT" >&2

if [ ! -f "$TMP_OUT/initrd.img" ]; then
    echo "ERROR: shed-initramfs build did not produce initrd.img" >&2
    exit 1
fi

cp "$TMP_OUT/initrd.img" "$OUTPUT"
size_bytes="$(wc -c < "$OUTPUT" | tr -d ' ')"
echo "shed-initramfs: wrote $OUTPUT (${size_bytes} bytes)" >&2
