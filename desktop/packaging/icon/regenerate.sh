#!/usr/bin/env bash
# Regenerate Resources/AppIcon.icns from the SF-symbol glyph (see
# generate-icon.swift). Pass through flags, e.g. --bg 2E5C8A --fg FFFFFF.
set -euo pipefail

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "${DIR}/../.." && pwd)"
MASTER="${DIR}/master.png"

swift "${DIR}/generate-icon.swift" --out "${MASTER}" "$@"

ICONSET="$(mktemp -d)/AppIcon.iconset"
mkdir -p "${ICONSET}"
emit() { sips -z "$1" "$1" "${MASTER}" --out "${ICONSET}/$2" >/dev/null; }
emit 16   icon_16x16.png
emit 32   icon_16x16@2x.png
emit 32   icon_32x32.png
emit 64   icon_32x32@2x.png
emit 128  icon_128x128.png
emit 256  icon_128x128@2x.png
emit 256  icon_256x256.png
emit 512  icon_256x256@2x.png
emit 512  icon_512x512.png
cp "${MASTER}" "${ICONSET}/icon_512x512@2x.png"

iconutil -c icns "${ICONSET}" -o "${ROOT}/Resources/AppIcon.icns"
echo "wrote ${ROOT}/Resources/AppIcon.icns"

# --- Tauri client icons (same master → the cross-platform app's tray + app/dock
# icon, so it matches the Swift app instead of the default green placeholder).
# icon.ico is left as-is: sips can't emit .ico and Windows isn't a shipped target.
TAURI="${ROOT}/tauri/src-tauri/icons"
sips -z 32 32   "${MASTER}" --out "${TAURI}/32x32.png"           >/dev/null
sips -z 128 128 "${MASTER}" --out "${TAURI}/128x128.png"         >/dev/null
sips -z 256 256 "${MASTER}" --out "${TAURI}/128x128@2x.png"      >/dev/null
sips -z 512 512 "${MASTER}" --out "${TAURI}/icon.png"            >/dev/null
sips -z 30 30   "${MASTER}" --out "${TAURI}/Square30x30Logo.png" >/dev/null
sips -z 50 50   "${MASTER}" --out "${TAURI}/StoreLogo.png"       >/dev/null
iconutil -c icns "${ICONSET}" -o "${TAURI}/icon.icns"
echo "wrote ${TAURI}/{32x32,128x128,128x128@2x,icon}.png + icon.icns"

# --- mac menu-bar TEMPLATE glyph (black-on-transparent silhouette). Rendered
# separately from the colored master (--template skips the rounded-square body) so
# the Tauri tray can `icon_as_template(true)` and get a real menu-bar glyph that
# adapts to light/dark — matching the Swift NSStatusItem. macOS-only at runtime
# (Linux keeps the colored icon; a silhouette would render as a black blob there).
TPL_MASTER="$(mktemp -d)/tray-template-master.png"
swift "${DIR}/generate-icon.swift" --template --out "${TPL_MASTER}"
sips -z 18 18 "${TPL_MASTER}" --out "${TAURI}/tray-template.png"    >/dev/null
sips -z 36 36 "${TPL_MASTER}" --out "${TAURI}/tray-template@2x.png" >/dev/null
echo "wrote ${TAURI}/tray-template{,@2x}.png"
