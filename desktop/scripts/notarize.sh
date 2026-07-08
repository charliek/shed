#!/usr/bin/env bash
# Notarize + staple a ShedDesktop DMG. (notarytool needs an archive — a bare
# .app must be zipped first; the release pipeline always passes the DMG.)
#
# No-op (exit 0) when no credentials are configured, so the release pipeline
# still ships an ad-hoc / un-notarized DMG until an Apple Developer account is
# wired up. Adding the secrets below activates it with no other changes.
#
# Credentials (either form):
#   * SHED_DESKTOP_NOTARY_PROFILE — a stored notarytool keychain profile
#       (local: `xcrun notarytool store-credentials <name> --apple-id … --team-id … --password …`)
#   * APPLE_ID + APPLE_TEAM_ID + APPLE_APP_SPECIFIC_PASSWORD — CI secrets
#
# Usage:
#   ./scripts/notarize.sh build/ShedDesktop-0.0.13.dmg
set -euo pipefail

TARGET="${1:-}"
if [ -z "${TARGET}" ] || [ ! -e "${TARGET}" ]; then
  echo "usage: $0 <path-to-dmg-or-archive>" >&2
  exit 1
fi

if [ -n "${SHED_DESKTOP_NOTARY_PROFILE:-}" ]; then
  AUTH=(--keychain-profile "${SHED_DESKTOP_NOTARY_PROFILE}")
elif [ -n "${APPLE_ID:-}" ] && [ -n "${APPLE_TEAM_ID:-}" ] && [ -n "${APPLE_APP_SPECIFIC_PASSWORD:-}" ]; then
  AUTH=(--apple-id "${APPLE_ID}" --team-id "${APPLE_TEAM_ID}" --password "${APPLE_APP_SPECIFIC_PASSWORD}")
else
  echo "==> notarize: no credentials set — skipping (DMG ships un-notarized)."
  echo "    To enable: set SHED_DESKTOP_NOTARY_PROFILE, or APPLE_ID + APPLE_TEAM_ID +"
  echo "    APPLE_APP_SPECIFIC_PASSWORD, then re-run."
  echo "    Until then, users clear Gatekeeper once after install with:"
  echo "      xattr -dr com.apple.quarantine /Applications/ShedDesktop.app"
  echo "    (or System Settings > Privacy & Security > Open Anyway)."
  exit 0
fi

echo "==> notarytool submit (waits for Apple; usually a few minutes)…"
xcrun notarytool submit "${TARGET}" "${AUTH[@]}" --wait

echo "==> stapler staple"
xcrun stapler staple "${TARGET}"
xcrun stapler validate "${TARGET}"
echo "==> Notarized + stapled: ${TARGET}"
