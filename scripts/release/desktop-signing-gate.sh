#!/usr/bin/env bash
# The desktop signing gate: a STABLE desktop tag must ship a Developer ID
# signed, notarized Mac build, so it refuses to proceed unless all six Apple
# secrets exist (plan 026 §3.11, shed#402).
#
# Usage:
#   desktop-signing-gate.sh <tag> \
#     --has MACOS_CERTIFICATE_P12_BASE64=true|false \
#     --has MACOS_CERTIFICATE_PASSWORD=true|false \
#     --has SHED_DESKTOP_DEVELOPER_ID_IDENTITY=true|false \
#     --has APPLE_ID=true|false \
#     --has APPLE_TEAM_ID=true|false \
#     --has APPLE_APP_SPECIFIC_PASSWORD=true|false
#
# The six names are exactly the secrets `CAN_NOTARIZE` reads in
# publish-images.yaml's desktop-macos job. Each flag is a PRESENCE flag the
# workflow builds as `${{ secrets.NAME != '' }}`: this script never receives a
# secret value, and it never prints any argument it did not validate (a value
# wired in by mistake is refused without being echoed).
#
# Exit codes:
#   0  the tag may proceed: a stable tag with all six present, or any
#      prerelease (`-suffix`) tag, which may build ad-hoc (the rc rehearsal)
#   1  a stable tag with any of the six missing; the message names exactly the
#      missing ones
#   2  usage: no tag, a malformed tag, or a flag that is not exactly one
#      `--has NAME=true|false` for each of the six names
#
# publish-images.yaml runs this in the `release-plan` job (when ship_desktop),
# which every other job needs, so a stable tag without the signing secrets
# fails before anything publishes; desktop-macos runs it again right after its
# checkout as a backstop. release-scripts-test.sh tests this script as deployed
# and asserts both invocations exist in the workflow.
#
# Kept portable to stock macOS bash 3.2 (no associative arrays), like the other
# release scripts.

set -euo pipefail

NAMES="MACOS_CERTIFICATE_P12_BASE64 MACOS_CERTIFICATE_PASSWORD SHED_DESKTOP_DEVELOPER_ID_IDENTITY APPLE_ID APPLE_TEAM_ID APPLE_APP_SPECIFIC_PASSWORD"

usage_fail() {
  echo "::error::desktop-signing-gate: $1" >&2
  echo "usage: $0 <tag> --has NAME=true|false (once for each of: ${NAMES})" >&2
  exit 2
}

# The same tag grammar as release-plan.sh (vX.Y.Z[-prerelease], no leading
# zeros). An invalid tag is refused without being echoed.
SEMVER_RE='(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[A-Za-z0-9.-]+)?'

[ "$#" -ge 1 ] || usage_fail "no tag given"
TAG="$1"
shift
[[ "${TAG}" =~ ^v?${SEMVER_RE}$ ]] || usage_fail "the tag argument is not vX.Y.Z[-prerelease] (its content is not shown)"

# One slot per name, in NAMES order: "" (not given yet), "true" or "false".
p12="" password="" identity="" apple_id="" team_id="" app_password=""

arg_no=1
while [ "$#" -gt 0 ]; do
  arg_no=$((arg_no + 1))
  [ "$1" = "--has" ] || usage_fail "argument ${arg_no} is not --has (its content is not shown)"
  [ "$#" -ge 2 ] || usage_fail "--has at argument ${arg_no} has no NAME=true|false after it"
  shift
  arg_no=$((arg_no + 1))
  pair="$1"
  shift
  case "${pair}" in
    *=*) ;;
    *) usage_fail "argument ${arg_no} is not NAME=true|false (its content is not shown)" ;;
  esac
  name="${pair%%=*}"
  value="${pair#*=}"
  case "${name}" in
    MACOS_CERTIFICATE_P12_BASE64|MACOS_CERTIFICATE_PASSWORD|SHED_DESKTOP_DEVELOPER_ID_IDENTITY|APPLE_ID|APPLE_TEAM_ID|APPLE_APP_SPECIFIC_PASSWORD) ;;
    *) usage_fail "argument ${arg_no} names an unknown secret (its content is not shown)" ;;
  esac
  case "${value}" in
    true|false) ;;
    *) usage_fail "--has ${name} is not =true or =false (the value is not shown: it may be a secret wired in by mistake)" ;;
  esac
  case "${name}" in
    MACOS_CERTIFICATE_P12_BASE64) current="${p12}" ;;
    MACOS_CERTIFICATE_PASSWORD) current="${password}" ;;
    SHED_DESKTOP_DEVELOPER_ID_IDENTITY) current="${identity}" ;;
    APPLE_ID) current="${apple_id}" ;;
    APPLE_TEAM_ID) current="${team_id}" ;;
    APPLE_APP_SPECIFIC_PASSWORD) current="${app_password}" ;;
  esac
  [ -z "${current}" ] || usage_fail "--has ${name} is given more than once"
  case "${name}" in
    MACOS_CERTIFICATE_P12_BASE64) p12="${value}" ;;
    MACOS_CERTIFICATE_PASSWORD) password="${value}" ;;
    SHED_DESKTOP_DEVELOPER_ID_IDENTITY) identity="${value}" ;;
    APPLE_ID) apple_id="${value}" ;;
    APPLE_TEAM_ID) team_id="${value}" ;;
    APPLE_APP_SPECIFIC_PASSWORD) app_password="${value}" ;;
  esac
done

missing=""
not_given=""
note() {
  # $1 = name, $2 = its slot
  case "$2" in
    true) ;;
    false) missing="${missing:+${missing}, }$1" ;;
    *) not_given="${not_given:+${not_given}, }$1" ;;
  esac
}
note MACOS_CERTIFICATE_P12_BASE64 "${p12}"
note MACOS_CERTIFICATE_PASSWORD "${password}"
note SHED_DESKTOP_DEVELOPER_ID_IDENTITY "${identity}"
note APPLE_ID "${apple_id}"
note APPLE_TEAM_ID "${team_id}"
note APPLE_APP_SPECIFIC_PASSWORD "${app_password}"

[ -z "${not_given}" ] || usage_fail "no --has flag for: ${not_given}"

case "${TAG}" in
  *-*)
    if [ -n "${missing}" ]; then
      echo "::notice::desktop signing gate: ${TAG} is a prerelease, so an ad-hoc-signed, un-notarized Mac build is allowed (missing: ${missing}). A stable tag fails closed without all six."
    else
      echo "desktop signing gate: ${TAG} is a prerelease and all six Apple secrets are present; the Mac build is Developer ID signed and notarized."
    fi
    exit 0
    ;;
esac

if [ -n "${missing}" ]; then
  echo "::error::a stable desktop tag requires Developer ID signing and notarization; missing: ${missing}" >&2
  echo "desktop signing gate: ${TAG} is refused before anything publishes. Add the missing repository secrets (desktop/RELEASING.md, Secrets), or rehearse on a prerelease (-rc.N) tag, which may build ad-hoc." >&2
  exit 1
fi

echo "desktop signing gate: ${TAG} is stable and all six Apple secrets are present; the Mac build is Developer ID signed and notarized."
