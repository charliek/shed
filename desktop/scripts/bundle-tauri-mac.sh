#!/usr/bin/env bash
# Tauri ShedDesktop.app bundling (macOS).
#
# The Tauri sibling of scripts/bundle.sh: builds the Tauri client with the
# tauri CLI (which applies the tauri.macos.conf.json overlay — productName
# ShedDesktop, identifier ai.stridelabs.ShedDesktop, embedded
# Sparkle.framework), copies the .app to build/ShedDesktop.app, and re-signs
# it in Sparkle's required order. scripts/make-dmg.sh packages the result
# unchanged (same output paths as the Swift bundle — they clobber each other).
#
# Signing is deliberately STRICTER than bundle.sh (which --deep-signs the
# framework): Sparkle 2 documents an ordered inner->outer sign of its nested
# helpers, and a wrong order (or --deep on the outer app) signs + notarizes
# clean but BREAKS AT UPDATE TIME. Do not "align" this down to bundle.sh.
#
# Env (same conventions as bundle.sh):
#   SHED_DESKTOP_DEVELOPER_ID_IDENTITY  codesign identity (default: ad-hoc "-")
#   SHED_DESKTOP_ALLOW_UNSIGNED=1       continue past codesign failures
#   SHED_TAURI_CONFIG_OVERRIDE          inline `tauri build --config <json>` merge
#                                       (CI smoke only — e.g. a prerelease version
#                                       override to prove the suffix survives plist
#                                       stamping verbatim; NOT used by releases)
#
# Usage:
#   ./scripts/bundle-tauri-mac.sh            # release (default)
#   ./scripts/bundle-tauri-mac.sh --debug    # debug bundle (CI smoke)

set -euo pipefail

if [ "$(uname -s)" != "Darwin" ]; then
  echo "error: bundle-tauri-mac.sh is macOS-only" >&2
  exit 1
fi

CONFIG="release"
if [ "${1:-}" = "--debug" ]; then
  CONFIG="debug"
elif [ -n "${1:-}" ]; then
  echo "error: unknown argument '${1}' (only --debug is supported)" >&2
  exit 1
fi

# Tauri's bundler auto-signs when it sees Apple signing env vars — with a real
# cert it would sign the nested Sparkle helpers in the wrong order (and not
# re-signable cleanly). This script owns signing; the bundler must only ever
# apply its overwritable ad-hoc pass.
for v in APPLE_SIGNING_IDENTITY APPLE_CERTIFICATE; do
  if [ -n "${!v:-}" ]; then
    echo "error: ${v} is set — unset it; the Tauri bundler must not sign." >&2
    echo "       This script signs (identity from SHED_DESKTOP_DEVELOPER_ID_IDENTITY)." >&2
    exit 1
  fi
done

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

APP_NAME="ShedDesktop"
BUNDLE_ID="ai.stridelabs.ShedDesktop"
ENT_FILE="${REPO_ROOT}/tauri/src-tauri/${APP_NAME}.entitlements"
OUT_DIR="${REPO_ROOT}/build"
APP_DIR="${OUT_DIR}/${APP_NAME}.app"

# App-icon sources (packaging/icon/generate_icons.py emits both):
#   AppIcon.icon — Icon Composer catalog, compiled with actool into Assets.car
#     for the macOS 26 (Tahoe) glass icon.
#   icons/icon.icns — flat .icns, the pre-Tahoe / no-actool fallback.
ICON_COMPOSER_SRC="${REPO_ROOT}/tauri/src-tauri/AppIcon.icon"
ICON_SRC="${REPO_ROOT}/tauri/src-tauri/icons/icon.icns"

echo "==> Staging Sparkle.framework"
"${SCRIPT_DIR}/fetch-sparkle.sh"

if [ ! -d "${REPO_ROOT}/tauri/ui/node_modules" ]; then
  echo "error: tauri/ui/node_modules missing — run 'npm --prefix tauri/ui ci' first" >&2
  exit 1
fi

# The tauri CLI walks up from cwd to find src-tauri/tauri.conf.json, so run it
# from tauri/ (npm exec resolves the binary from ui/node_modules but keeps the
# cwd). Trailing args after the standalone `--` go to cargo: --locked so the
# build cannot drift from (or dirty) the committed Cargo.lock the Linux .deb
# and the release lockstep guard pin.
echo "==> tauri build (${CONFIG})"
TAURI_FLAGS=(--bundles app)
if [ "${CONFIG}" = "debug" ]; then
  TAURI_FLAGS+=(--debug)
fi
# An inline config merge (CI smoke uses it to override the version) — passed to
# the tauri CLI, before the standalone `--` that forwards the rest to cargo.
if [ -n "${SHED_TAURI_CONFIG_OVERRIDE:-}" ]; then
  TAURI_FLAGS+=(--config "${SHED_TAURI_CONFIG_OVERRIDE}")
fi
pushd "${REPO_ROOT}/tauri" >/dev/null
npm --prefix ui exec tauri -- build "${TAURI_FLAGS[@]}" -- --locked
popd >/dev/null

BUNDLED_APP="${REPO_ROOT}/tauri/src-tauri/target/${CONFIG}/bundle/macos/${APP_NAME}.app"
if [ ! -d "${BUNDLED_APP}" ]; then
  echo "error: tauri build did not produce ${BUNDLED_APP}" >&2
  exit 1
fi

echo "==> Copying to ${APP_DIR}"
# cp -R preserves read-only directory bits (Sparkle's dist has some), which can
# make a bare rm -rf of a previous copy fail with "Directory not empty".
if [ -d "${APP_DIR}" ]; then
  chmod -R u+w "${APP_DIR}"
fi
rm -rf "${APP_DIR}"
mkdir -p "${OUT_DIR}"
cp -R "${BUNDLED_APP}" "${APP_DIR}"

# Tauri renames the binary to productName (ShedDesktop); verify the plist and
# the file agree before signing seals them.
EXEC_NAME="$(plutil -extract CFBundleExecutable raw "${APP_DIR}/Contents/Info.plist")"
if [ ! -f "${APP_DIR}/Contents/MacOS/${EXEC_NAME}" ]; then
  echo "error: CFBundleExecutable '${EXEC_NAME}' missing under Contents/MacOS/" >&2
  ls "${APP_DIR}/Contents/MacOS/" >&2
  exit 1
fi

FRAMEWORK="${APP_DIR}/Contents/Frameworks/Sparkle.framework"
if [ ! -d "${FRAMEWORK}" ]; then
  echo "error: ${FRAMEWORK} missing — tauri.macos.conf.json frameworks not applied?" >&2
  exit 1
fi

# App icon. On macOS 26 (Tahoe) a loose .icns is treated as legacy and inset on
# the system glass tile (a gray frame + muted color around the art). The fix is
# a compiled Icon Composer catalog (tauri/src-tauri/AppIcon.icon, generated by
# packaging/icon/generate_icons.py) — `actool` renders it into Assets.car + a
# flattened AppIcon.icns, and Tahoe then fills the tile edge-to-edge with the
# native glass treatment. `actool` ships with full Xcode, not the bare Command
# Line Tools, so we fall back to the committed flat .icns when it's unavailable —
# that still builds a launchable bundle, just with the framed legacy icon on
# Tahoe. CFBundleIconName=AppIcon (set in tauri/src-tauri/Info.plist, merged into
# the bundle plist by the Tauri bundler) routes the OS to the catalog; the .icns
# covers pre-Tahoe and the no-actool path. This runs BEFORE codesign so the
# added Assets.car + Info.plist changes are sealed by the signature.
ICON_DONE=0
if [ -d "${ICON_COMPOSER_SRC}" ] && command -v xcrun >/dev/null 2>&1 \
   && xcrun --find actool >/dev/null 2>&1; then
  # Match the bundle's own minimum OS so the catalog targets what the app ships.
  MIN_OS="$(/usr/libexec/PlistBuddy -c 'Print :LSMinimumSystemVersion' \
    "${APP_DIR}/Contents/Info.plist" 2>/dev/null || echo 26.0)"
  ACTOOL_TMP="$(mktemp -d)"
  echo "==> Compiling AppIcon.icon with actool (Tahoe glass icon, min ${MIN_OS})"
  if xcrun actool "${ICON_COMPOSER_SRC}" \
       --compile "${ACTOOL_TMP}" \
       --platform macosx \
       --minimum-deployment-target "${MIN_OS}" \
       --app-icon AppIcon \
       --output-partial-info-plist "${ACTOOL_TMP}/partial.plist" \
       --errors --warnings >/dev/null 2>&1 \
     && [ -f "${ACTOOL_TMP}/Assets.car" ]; then
    cp "${ACTOOL_TMP}/Assets.car" "${APP_DIR}/Contents/Resources/Assets.car"
    # actool also emits a flattened .icns — keep it as the pre-Tahoe fallback.
    [ -f "${ACTOOL_TMP}/AppIcon.icns" ] \
      && cp "${ACTOOL_TMP}/AppIcon.icns" "${APP_DIR}/Contents/Resources/AppIcon.icns"
    echo "    Compiled: ${APP_DIR}/Contents/Resources/Assets.car"
    ICON_DONE=1
  else
    echo "    warn: actool failed; falling back to flat AppIcon.icns" >&2
  fi
  rm -rf "${ACTOOL_TMP}"
fi
if [ "${ICON_DONE}" -eq 0 ]; then
  if [ -f "${ICON_SRC}" ]; then
    echo "==> Including flat AppIcon.icns (no actool — Tahoe will show the framed legacy icon)"
    cp "${ICON_SRC}" "${APP_DIR}/Contents/Resources/AppIcon.icns"
  else
    echo "==> No app icon found; bundle ships without a custom catalog icon"
  fi
fi

# Sign in Sparkle's REQUIRED order, innermost first, never --deep. The
# scripts under Contents/Resources/bin/ are NOT signed individually — they
# are shell/Python text, not Mach-O; the outer app signature seals them
# (same convention as bundle.sh).
SIGN_IDENTITY="${SHED_DESKTOP_DEVELOPER_ID_IDENTITY:--}"
TS_FLAG=""
if [ "${SIGN_IDENTITY}" != "-" ]; then
  TS_FLAG="--timestamp"
fi
if ! command -v codesign >/dev/null 2>&1; then
  if [ "${SHED_DESKTOP_ALLOW_UNSIGNED:-0}" = "1" ]; then
    echo "==> warn: codesign not found; SHED_DESKTOP_ALLOW_UNSIGNED=1, shipping unsigned"
  else
    echo "error: codesign not found (set SHED_DESKTOP_ALLOW_UNSIGNED=1 to bypass)" >&2
    exit 1
  fi
else
  if [ "${SIGN_IDENTITY}" = "-" ]; then
    echo "==> Ad-hoc codesign (set SHED_DESKTOP_DEVELOPER_ID_IDENTITY for a notarizable build)"
  else
    echo "==> Developer ID codesign (identity: ${SIGN_IDENTITY})"
  fi
  codesign_or_die() {
    local target="$1"
    shift
    # shellcheck disable=SC2086  # TS_FLAG must word-split (empty => no flag)
    if codesign --force --sign "${SIGN_IDENTITY}" \
         --options runtime \
         ${TS_FLAG} \
         "$@" \
         "${target}"
    then
      return 0
    fi
    if [ "${SHED_DESKTOP_ALLOW_UNSIGNED:-0}" = "1" ]; then
      echo "    warn: codesign(${target}) failed; SHED_DESKTOP_ALLOW_UNSIGNED=1, continuing"
      return 0
    fi
    echo "    error: codesign(${target}) failed (set SHED_DESKTOP_ALLOW_UNSIGNED=1 to bypass)" >&2
    exit 1
  }
  # Downloader.xpc: preserve the dist's entitlements verbatim (an EMPTY dict —
  # Sparkle >= 2.6 removed its sandbox, sparkle-project/Sparkle#2511) rather
  # than injecting the app's.
  codesign_or_die "${FRAMEWORK}/Versions/B/XPCServices/Installer.xpc"
  codesign_or_die "${FRAMEWORK}/Versions/B/XPCServices/Downloader.xpc" \
    --preserve-metadata=entitlements
  codesign_or_die "${FRAMEWORK}/Versions/B/Autoupdate"
  codesign_or_die "${FRAMEWORK}/Versions/B/Updater.app"
  codesign_or_die "${FRAMEWORK}"
  codesign_or_die "${APP_DIR}" --entitlements "${ENT_FILE}"

  echo "==> codesign --verify --strict"
  if ! codesign --verify --strict "${APP_DIR}"; then
    if [ "${SHED_DESKTOP_ALLOW_UNSIGNED:-0}" = "1" ]; then
      echo "    warn: strict verify failed; SHED_DESKTOP_ALLOW_UNSIGNED=1, continuing"
    else
      echo "    error: strict verify failed" >&2
      exit 1
    fi
  fi
fi

VERSION="$(plutil -extract CFBundleVersion raw "${APP_DIR}/Contents/Info.plist")"
echo "==> Bundled: ${APP_DIR}"
echo "    Bundle ID:  ${BUNDLE_ID}"
echo "    Version:    ${VERSION}"
echo "    Executable: ${APP_DIR}/Contents/MacOS/${EXEC_NAME}"
echo "    Framework:  ${FRAMEWORK}"
echo
echo "Launch with: open '${APP_DIR}'"
