#!/usr/bin/env bash
# Signs the darwin shed-server and assembles ShedServer.app (plan 028 D4, #304).
#
# macOS keys a bare binary's privacy grant (an external drive, Full Disk
# Access) on its absolute path, which brew changes on every upgrade. An app
# bundle's grant is keyed on its bundle ID plus its designated requirement,
# which a Developer ID signature keeps stable across builds. So the brew
# formula installs this bundle and links bin/shed-server into it.
#
# Usage:
#   macos-server-app.sh <binary> <os> <arch> <version>
#       The shed-server build's goreleaser `hooks.post`, run once per target
#       from the repo root:
#       1. a target <os> other than darwin exits 0 before anything else;
#       2. picks the signing mode (below);
#       3. signs <binary> in place with the bundle identifier and
#          internal/vz/entitlements.plist (plus --timestamp unless ad hoc);
#       4. assembles dist/server/macos-app/darwin_<arch>/ShedServer.app
#          (staging isolated per arch) with Info.plist and explicit modes;
#       5. signs the bundle with the same identity, entitlements and timestamp;
#       6. verifies both <binary> and the bundle (the --verify checks).
#
#   macos-server-app.sh --verify <path>
#       Step 6 alone, for a Mach-O <path> or a ShedServer.app <path>. The
#       release workflows call it on a scratch signature and on extracted
#       tarballs, so the checks live only here.
#
# Signing mode (environment):
#   SHED_SERVER_SIGNING=required
#       Developer ID. Needs SHED_SERVER_SIGN_IDENTITY (the codesign identity),
#       SHED_SERVER_TEAM_ID and a macOS host; anything missing exits 1. The
#       checks then also require the first Authority to be "Developer ID
#       Application: …", TeamIdentifier to equal SHED_SERVER_TEAM_ID, a secure
#       Timestamp, and a designated requirement naming the identifier and the
#       Team ID. --verify needs only SHED_SERVER_TEAM_ID.
#   unset or empty
#       Ad hoc (`-`), for snapshots and CI. On a host that isn't macOS it
#       warns and builds no bundle (a local Linux snapshot).
#   Any other value exits 1, so a typo can't fall back to ad hoc.
#
# Every check failure exits 1; a usage error exits 2. codesign, plutil and
# uname are resolved from PATH, so release-scripts-test.sh runs this script on
# Linux with stubs ahead of the real tools (a stub `uname` stands in for the
# host OS). Nothing test-only lives here.
#
# Kept portable to stock macOS bash 3.2, like the other release scripts.

set -euo pipefail

BUNDLE_ID="ai.stridelabs.shed-server"
APP_NAME="ShedServer.app"
EXE_NAME="shed-server"
# The only entitlement shed-server carries, as `plutil -convert json` prints it.
WANT_ENTITLEMENTS='{"com.apple.security.virtualization":true}'

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
ENTITLEMENTS="${REPO_ROOT}/internal/vz/entitlements.plist"

die() {
  echo "macos-server-app: error: $*" >&2
  exit 1
}

usage_fail() {
  echo "macos-server-app: $1" >&2
  echo "usage: $0 <binary> <os> <arch> <version>" >&2
  echo "       $0 --verify <Mach-O or ${APP_NAME} path>" >&2
  exit 2
}

need_tools() {
  local t
  for t in codesign plutil; do
    command -v "${t}" >/dev/null 2>&1 || die "${t} not found on PATH (it needs a macOS host)"
  done
}

# Sets MODE to adhoc or required from SHED_SERVER_SIGNING.
signing_mode() {
  case "${SHED_SERVER_SIGNING:-}" in
    "") MODE=adhoc ;;
    required) MODE=required ;;
    *) die "SHED_SERVER_SIGNING must be 'required' or unset" ;;
  esac
}

SCRATCH=""
cleanup() {
  if [ -n "${SCRATCH}" ]; then rm -rf "${SCRATCH}"; fi
}
trap cleanup EXIT

# verify_path <path>: step 6 for one Mach-O or one bundle, under MODE.
verify_path() {
  local p="$1" out details ident plist plist_id ents authority team dr
  if [ -d "${p}" ]; then
    plist="${p}/Contents/Info.plist"
    [ -f "${plist}" ] || die "${p}: no Contents/Info.plist, so it is not an app bundle"
  elif [ -f "${p}" ]; then
    plist=""
  else
    die "${p}: no such file or bundle"
  fi

  out="$(codesign --verify --strict --verbose=2 "${p}" 2>&1)" \
    || die "${p}: codesign --verify --strict failed: ${out}"

  details="$(codesign -d --verbose=4 "${p}" 2>&1)" \
    || die "${p}: codesign -d failed: ${details}"
  ident="$(printf '%s\n' "${details}" | sed -n 's/^Identifier=//p')"
  [ "${ident}" = "${BUNDLE_ID}" ] \
    || die "${p}: the signing identifier is '${ident}', want '${BUNDLE_ID}'"

  if [ -n "${plist}" ]; then
    out="$(plutil -lint "${plist}" 2>&1)" || die "${plist}: plutil -lint failed: ${out}"
    plist_id="$(plutil -extract CFBundleIdentifier raw -o - "${plist}" 2>&1)" \
      || die "${plist}: cannot read CFBundleIdentifier: ${plist_id}"
    [ "${plist_id}" = "${BUNDLE_ID}" ] \
      || die "${plist}: CFBundleIdentifier is '${plist_id}', want '${BUNDLE_ID}'"
  fi

  if [ -z "${SCRATCH}" ]; then
    SCRATCH="$(mktemp -d "${TMPDIR:-/tmp}/macos-server-app.XXXXXX")"
  fi
  codesign -d --entitlements - --xml "${p}" >"${SCRATCH}/entitlements.plist" 2>/dev/null \
    || die "${p}: codesign cannot read its entitlements"
  ents="$(plutil -convert json -o - "${SCRATCH}/entitlements.plist" 2>/dev/null | tr -d '[:space:]')" \
    || die "${p}: its entitlements are not a valid plist (is it signed without them?)"
  [ "${ents}" = "${WANT_ENTITLEMENTS}" ] \
    || die "${p}: the entitlements are ${ents:-<none>}, want exactly ${WANT_ENTITLEMENTS}"

  if [ "${MODE}" = required ]; then
    authority="$(printf '%s\n' "${details}" | awk '/^Authority=/ { sub(/^Authority=/, ""); print; exit }')"
    case "${authority}" in
      "Developer ID Application: "*) ;;
      "") die "${p}: no signing Authority (an ad hoc signature?), want Developer ID Application" ;;
      *) die "${p}: the first Authority is '${authority}', want 'Developer ID Application: …'" ;;
    esac
    team="$(printf '%s\n' "${details}" | sed -n 's/^TeamIdentifier=//p')"
    [ "${team}" = "${SHED_SERVER_TEAM_ID}" ] \
      || die "${p}: TeamIdentifier is '${team}', which is not SHED_SERVER_TEAM_ID"
    printf '%s\n' "${details}" | grep -q '^Timestamp=' \
      || die "${p}: no secure Timestamp (was it signed without --timestamp?)"
    dr="$(codesign -d -r- "${p}" 2>&1)" || die "${p}: codesign cannot read its requirements: ${dr}"
    dr="$(printf '%s\n' "${dr}" | sed -n 's/^designated => //p')"
    case "${dr}" in
      *"identifier \"${BUNDLE_ID}\""*) ;;
      *) die "${p}: the designated requirement does not name identifier \"${BUNDLE_ID}\": ${dr:-<none>}" ;;
    esac
    case "${dr}" in
      *"leaf[subject.OU] = ${SHED_SERVER_TEAM_ID}"* | *"leaf[subject.OU] = \"${SHED_SERVER_TEAM_ID}\""*) ;;
      *) die "${p}: the designated requirement does not pin SHED_SERVER_TEAM_ID: ${dr:-<none>}" ;;
    esac
    echo "macos-server-app: verified ${p} (${BUNDLE_ID}, Developer ID, team ${team}, timestamped)"
  else
    echo "macos-server-app: verified ${p} (${BUNDLE_ID}, ad hoc)"
  fi
}

# --- --verify <path> -------------------------------------------------------
if [ "${1:-}" = "--verify" ]; then
  [ "$#" -eq 2 ] || usage_fail "--verify takes exactly one path"
  signing_mode
  if [ "${MODE}" = required ] && [ -z "${SHED_SERVER_TEAM_ID:-}" ]; then
    die "SHED_SERVER_SIGNING=required needs SHED_SERVER_TEAM_ID"
  fi
  need_tools
  verify_path "$2"
  exit 0
fi

# --- the build hook ----------------------------------------------------------
[ "$#" -eq 4 ] || usage_fail "expected 4 arguments, got $#"
BIN="$1"
TARGET_OS="$2"
ARCH="$3"
VERSION="$4"

# 1. Only a darwin target gets a bundle, and nothing else is checked first.
[ "${TARGET_OS}" = darwin ] || exit 0

# 2. The signing mode, and what it needs.
signing_mode
HOST_OS="$(uname -s)"
if [ "${MODE}" = required ]; then
  missing=""
  [ -n "${SHED_SERVER_SIGN_IDENTITY:-}" ] || missing="${missing:+${missing}, }SHED_SERVER_SIGN_IDENTITY"
  [ -n "${SHED_SERVER_TEAM_ID:-}" ] || missing="${missing:+${missing}, }SHED_SERVER_TEAM_ID"
  [ "${HOST_OS}" = Darwin ] || missing="${missing:+${missing}, }a macOS host (this host is ${HOST_OS})"
  [ -z "${missing}" ] || die "SHED_SERVER_SIGNING=required needs: ${missing}"
  IDENTITY="${SHED_SERVER_SIGN_IDENTITY}"
else
  if [ "${HOST_OS}" != Darwin ]; then
    echo "macos-server-app: warning: ${HOST_OS} host, so no ${APP_NAME} for darwin/${ARCH} (codesign needs macOS); the darwin shed-homebrew archive will lack it" >&2
    exit 0
  fi
  IDENTITY="-"
fi
need_tools

case "${ARCH}" in
  amd64 | arm64) ;;
  *) usage_fail "arch must be amd64 or arm64" ;;
esac
[ -f "${BIN}" ] || die "${BIN}: no such binary"
[ -f "${ENTITLEMENTS}" ] || die "${ENTITLEMENTS} is missing"
# CFBundleVersion and CFBundleShortVersionString take the numeric X.Y.Z only,
# so a snapshot's 0.9.1-SNAPSHOT-abc1234 becomes 0.9.1.
[[ "${VERSION}" =~ ^v?([0-9]+\.[0-9]+\.[0-9]+) ]] || usage_fail "version '${VERSION}' does not start with X.Y.Z"
BUNDLE_VERSION="${BASH_REMATCH[1]}"

sign() {
  if [ "${MODE}" = required ]; then
    codesign --force --sign "${IDENTITY}" --timestamp "$@"
  else
    codesign --force --sign "${IDENTITY}" "$@"
  fi
}

# 3. The raw binary, in place (it also ships in the shed-server archive).
sign --identifier "${BUNDLE_ID}" --entitlements "${ENTITLEMENTS}" "${BIN}" \
  || die "${BIN}: codesign failed"

# 4. The bundle, in this arch's own staging directory.
STAGE="${REPO_ROOT}/dist/server/macos-app/darwin_${ARCH}"
APP="${STAGE}/${APP_NAME}"
rm -rf "${STAGE}"
mkdir -p "${APP}/Contents/MacOS"
cp "${BIN}" "${APP}/Contents/MacOS/${EXE_NAME}"
cat >"${APP}/Contents/Info.plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>CFBundleIdentifier</key>
	<string>${BUNDLE_ID}</string>
	<key>CFBundleName</key>
	<string>Shed Server</string>
	<key>CFBundleDisplayName</key>
	<string>Shed Server</string>
	<key>CFBundleExecutable</key>
	<string>${EXE_NAME}</string>
	<key>CFBundlePackageType</key>
	<string>APPL</string>
	<key>CFBundleShortVersionString</key>
	<string>${BUNDLE_VERSION}</string>
	<key>CFBundleVersion</key>
	<string>${BUNDLE_VERSION}</string>
	<key>LSUIElement</key>
	<true/>
</dict>
</plist>
PLIST
chmod 0755 "${APP}" "${APP}/Contents" "${APP}/Contents/MacOS" "${APP}/Contents/MacOS/${EXE_NAME}"
chmod 0644 "${APP}/Contents/Info.plist"

# 5. The bundle signature (it re-signs the executable inside it, and seals
# Info.plist into _CodeSignature/CodeResources).
sign --entitlements "${ENTITLEMENTS}" "${APP}" || die "${APP}: codesign failed"

# 6. Both artifacts the release ships.
verify_path "${BIN}"
verify_path "${APP}"
