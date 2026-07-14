#!/usr/bin/env bash
# Stage Sparkle.framework + its bin/ tools for the Tauri mac app.
#
# Downloads the official Sparkle release tarball (pinned version + SHA256 —
# same pinning discipline as tauri-plugin-sparkle-updater's own
# scripts/download-sparkle.sh, whose 2.8.1 SHA this matches) and stages:
#
#   Sparkle.framework  -> tauri/src-tauri/Sparkle.framework   (gitignored)
#   bin/ (sign_update, generate_keys, BinaryDelta, ...)
#                      -> tauri/src-tauri/.sparkle-dist/bin/  (gitignored)
#
# The framework location is where the updater crate's build.rs looks (the dir
# containing tauri.conf.json) and what tauri.macos.conf.json's
# bundle.macOS.frameworks references. The .sparkle-dist tools are what the
# release workflow uses to EdDSA-sign the DMG (the Swift job takes sign_update
# from SwiftPM artifacts; the Tauri job has none, so it comes from here).
#
# Idempotent: skips the download when the framework is present and the stamp
# file matches the pinned version + checksum.
#
# Usage:
#   ./scripts/fetch-sparkle.sh          # or: make sparkle-framework

set -euo pipefail

if [ "$(uname -s)" != "Darwin" ]; then
  echo "==> non-macOS: skipping Sparkle staging (mac-only artifact)"
  exit 0
fi

SPARKLE_VERSION="2.8.1"
SPARKLE_SHA256="5cddb7695674ef7704268f38eccaee80e3accbf19e61c1689efff5b6116d85be"
SPARKLE_URL="https://github.com/sparkle-project/Sparkle/releases/download/${SPARKLE_VERSION}/Sparkle-${SPARKLE_VERSION}.tar.xz"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

DEST_DIR="${REPO_ROOT}/tauri/src-tauri"
FRAMEWORK_DIR="${DEST_DIR}/Sparkle.framework"
DIST_DIR="${DEST_DIR}/.sparkle-dist"
STAMP_FILE="${DIST_DIR}/.staged"
STAMP="${SPARKLE_VERSION} ${SPARKLE_SHA256}"

if [ -d "${FRAMEWORK_DIR}" ] && [ -x "${DIST_DIR}/bin/sign_update" ] \
  && [ -f "${STAMP_FILE}" ] \
  && [ "$(cat "${STAMP_FILE}")" = "${STAMP}" ]; then
  echo "==> Sparkle ${SPARKLE_VERSION} already staged: ${FRAMEWORK_DIR}"
  exit 0
fi

TMP_DIR="$(mktemp -d -t shed-sparkle)"
# Staged next to the destination so the final install is two same-volume mv's
# (near-atomic), not a slow cp into a half-deleted target a concurrent build
# could observe.
STAGE_DIR="$(mktemp -d "${DEST_DIR}/.sparkle-stage.XXXXXX")"
trap 'rm -rf "${TMP_DIR}" "${STAGE_DIR}"' EXIT

echo "==> Downloading Sparkle ${SPARKLE_VERSION}"
curl -fsSL -o "${TMP_DIR}/sparkle.tar.xz" "${SPARKLE_URL}"

echo "==> Verifying checksum"
echo "${SPARKLE_SHA256}  ${TMP_DIR}/sparkle.tar.xz" | shasum -a 256 -c - >/dev/null

echo "==> Extracting"
mkdir -p "${TMP_DIR}/extract"
tar -xf "${TMP_DIR}/sparkle.tar.xz" -C "${TMP_DIR}/extract"

for expected in "${TMP_DIR}/extract/Sparkle.framework" "${TMP_DIR}/extract/bin"; do
  if [ ! -e "${expected}" ]; then
    echo "error: Sparkle distribution is missing ${expected##*/} — layout changed?" >&2
    exit 1
  fi
done

# cp -R preserves the Versions/Current symlink farm codesign requires.
cp -R "${TMP_DIR}/extract/Sparkle.framework" "${STAGE_DIR}/Sparkle.framework"
mkdir -p "${STAGE_DIR}/.sparkle-dist"
cp -R "${TMP_DIR}/extract/bin" "${STAGE_DIR}/.sparkle-dist/bin"
printf '%s' "${STAMP}" > "${STAGE_DIR}/.sparkle-dist/.staged"

rm -rf "${FRAMEWORK_DIR}" "${DIST_DIR}"
mv "${STAGE_DIR}/Sparkle.framework" "${FRAMEWORK_DIR}"
mv "${STAGE_DIR}/.sparkle-dist" "${DIST_DIR}"

echo "==> Staged: ${FRAMEWORK_DIR}"
echo "==> Staged: ${DIST_DIR}/bin"
