#!/bin/bash
# Build the shed initramfs by invoking the `shed-initramfs` stage of one
# of the backend Dockerfiles. The output is a single gzipped cpio archive
# (/initrd.img) extracted to the path passed in --output.
#
# Usage:
#   ./scripts/build-initramfs.sh \
#       --backend <vz|firecracker> \
#       --platform <linux/arm64|linux/amd64> \
#       --output <path>
#
# The Dockerfile stage is the canonical source for the initramfs build;
# this script is a thin wrapper around `docker buildx build --output`.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(dirname "$SCRIPT_DIR")"

BACKEND=""
PLATFORM=""
OUTPUT=""

while [[ $# -gt 0 ]]; do
    case "$1" in
        --backend)
            BACKEND="$2"; shift 2 ;;
        --platform)
            PLATFORM="$2"; shift 2 ;;
        --output)
            OUTPUT="$2"; shift 2 ;;
        --help|-h)
            sed -n '2,15p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
            exit 0 ;;
        *)
            echo "ERROR: unknown argument: $1" >&2
            exit 2 ;;
    esac
done

if [ -z "$BACKEND" ] || [ -z "$PLATFORM" ] || [ -z "$OUTPUT" ]; then
    echo "ERROR: --backend, --platform, and --output are required" >&2
    exit 2
fi

case "$BACKEND" in
    vz)          DOCKERFILE="$PROJECT_ROOT/vz/Dockerfile" ;;
    firecracker) DOCKERFILE="$PROJECT_ROOT/firecracker/Dockerfile" ;;
    *) echo "ERROR: unknown backend $BACKEND (want vz|firecracker)" >&2; exit 2 ;;
esac

OUTPUT_DIR="$(dirname "$OUTPUT")"
mkdir -p "$OUTPUT_DIR"

# Build the shed-initramfs target into a tmp dir, then move /initrd.img
# to the requested output. `--output type=local` writes one file per
# top-level path in the scratch stage, so we get exactly initrd.img.
TMP_OUT="$(mktemp -d)"
trap 'rm -rf "$TMP_OUT"' EXIT

echo "shed-initramfs: building from $DOCKERFILE (backend=$BACKEND platform=$PLATFORM)" >&2
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
