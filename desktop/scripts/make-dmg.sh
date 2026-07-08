#!/usr/bin/env bash
# Package build/ShedDesktop.app into a drag-install DMG (M8).
#
# Output: build/ShedDesktop-<version>.dmg, containing ShedDesktop.app + an
# /Applications symlink (drag-to-install).
#
# Defaults to `hdiutil` — headless-safe, never hangs on Finder AppleScript,
# which matters on GitHub's GUI-less macOS runners. Set SHED_DESKTOP_DMG_FANCY=1
# (local/manual builds) to use `create-dmg` for a styled window.
#
# Run scripts/bundle.sh first (this packages whatever is in build/).
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

FILE_VERSION="$(tr -d '[:space:]' < "${REPO_ROOT}/VERSION" 2>/dev/null || echo "")"
VERSION="${SHED_DESKTOP_VERSION:-${FILE_VERSION:-0.0.0}}"
APP_DIR="${REPO_ROOT}/build/ShedDesktop.app"
DMG_OUT="${REPO_ROOT}/build/ShedDesktop-${VERSION}.dmg"

[ -d "${APP_DIR}" ] || { echo "error: ${APP_DIR} missing — run scripts/bundle.sh first" >&2; exit 1; }

STAGING="$(mktemp -d -t shed-desktop-dmg)"
trap 'rm -rf "${STAGING}"' EXIT
cp -R "${APP_DIR}" "${STAGING}/"

# First-launch note for the ad-hoc / non-notarized interim. It sits beside the
# app in the mounted DMG so the Gatekeeper-bypass step is visible before the
# user hits the wall. Gated on SHED_DESKTOP_DEVELOPER_ID_IDENTITY (the same
# signal bundle.sh uses): once a real identity is present the build is on the
# notarization path and the note is omitted.
if [ -z "${SHED_DESKTOP_DEVELOPER_ID_IDENTITY:-}" ]; then
  cat > "${STAGING}/FIRST-LAUNCH.txt" <<'EOF'
shed desktop — first launch on macOS

shed desktop is ad-hoc-signed but not yet notarized, so macOS Gatekeeper blocks
the first launch. You only need to do this once.

Easiest (works on every supported macOS): after dragging shed desktop into the
Applications folder, run this once in Terminal, then open it normally:

    xattr -dr com.apple.quarantine "/Applications/ShedDesktop.app"

Or via the GUI (macOS 15+): double-click ShedDesktop, dismiss the "Apple could
not verify…" warning, then open System Settings -> Privacy & Security, scroll to
the message about ShedDesktop, and click "Open Anyway".

Once a notarized build ships, this goes away and it opens with a normal
double-click.
EOF
fi

rm -f "${DMG_OUT}"

if [ "${SHED_DESKTOP_DMG_FANCY:-0}" = "1" ] && command -v create-dmg >/dev/null 2>&1; then
  echo "==> create-dmg (styled) -> ${DMG_OUT}"
  create-dmg \
    --volname "shed desktop ${VERSION}" \
    --app-drop-link 480 170 \
    --icon "ShedDesktop.app" 160 170 \
    --window-size 640 360 \
    "${DMG_OUT}" "${STAGING}" >/dev/null
else
  echo "==> hdiutil -> ${DMG_OUT}"
  ln -s /Applications "${STAGING}/Applications"
  # hdiutil intermittently fails with "Resource busy" on CI runners (transient
  # device/Spotlight contention, often right after codesign touches the bundle —
  # not a real error). Retry a few times before giving up.
  for attempt in 1 2 3 4 5; do
    if hdiutil create \
         -volname "shed desktop ${VERSION}" \
         -srcfolder "${STAGING}" \
         -ov -format UDZO \
         "${DMG_OUT}" >/dev/null; then
      break
    fi
    if [ "${attempt}" -eq 5 ]; then
      echo "error: hdiutil create failed after ${attempt} attempts" >&2
      exit 1
    fi
    echo "    hdiutil create failed (attempt ${attempt}); retrying in 3s…" >&2
    rm -f "${DMG_OUT}"
    sleep 3
  done
fi

echo "    Built: ${DMG_OUT} ($(du -h "${DMG_OUT}" | cut -f1))"
