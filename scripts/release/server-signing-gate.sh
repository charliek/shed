#!/usr/bin/env bash
# The server signing gate: a server tag must ship a Developer ID signed
# ShedServer.app (plan 028 D4, #304), so it refuses to proceed unless all four
# signing secrets exist. The server shares the desktop's Developer ID
# certificate; it needs no notarization, so the three notarytool-only secrets
# are not asked for.
#
# Usage:
#   server-signing-gate.sh <tag> \
#     --has MACOS_CERTIFICATE_P12_BASE64=true|false \
#     --has MACOS_CERTIFICATE_PASSWORD=true|false \
#     --has SHED_DESKTOP_DEVELOPER_ID_IDENTITY=true|false \
#     --has APPLE_TEAM_ID=true|false
#
# The four names are exactly the secrets publish-images.yaml's
# server-signing-preflight job and the release job's server signing read. Each
# flag is a PRESENCE flag the workflow builds as `${{ secrets.NAME != '' }}`:
# this script never receives a secret value, and it never prints any argument
# it did not validate (a value wired in by mistake is refused without being
# echoed).
#
# Exit codes:
#   0  all four present
#   1  any of the four missing; the message names exactly the missing ones.
#      This holds for a prerelease (`-suffix`) tag too, unlike
#      desktop-signing-gate.sh: the release job always signs the server with
#      SHED_SERVER_SIGNING=required (there is no ad hoc server release to fall
#      back to), and server tags are stable-only anyway (release-plan.sh
#      refuses a prerelease that ships server, before this gate runs). If that
#      rule is ever relaxed, the signing requirement does not relax with it.
#   2  usage: no tag, a malformed tag, or a flag that is not exactly one
#      `--has NAME=true|false` for each of the four names
#
# publish-images.yaml runs this in the `release-plan` job (when ship_server, on
# a tag push), which every other job needs, so a server tag without the signing
# secrets fails before anything publishes. The server-signing-preflight job
# then proves the certificate, password, identity and timestamping actually
# work, before any image job starts. release-scripts-test.sh tests this script
# as deployed and asserts its wiring in the workflow.
#
# Kept portable to stock macOS bash 3.2 (no associative arrays), like the other
# release scripts.

set -euo pipefail

NAMES="MACOS_CERTIFICATE_P12_BASE64 MACOS_CERTIFICATE_PASSWORD SHED_DESKTOP_DEVELOPER_ID_IDENTITY APPLE_TEAM_ID"

usage_fail() {
  echo "::error::server-signing-gate: $1" >&2
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
p12="" password="" identity="" team_id=""

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
    MACOS_CERTIFICATE_P12_BASE64|MACOS_CERTIFICATE_PASSWORD|SHED_DESKTOP_DEVELOPER_ID_IDENTITY|APPLE_TEAM_ID) ;;
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
    APPLE_TEAM_ID) current="${team_id}" ;;
  esac
  [ -z "${current}" ] || usage_fail "--has ${name} is given more than once"
  case "${name}" in
    MACOS_CERTIFICATE_P12_BASE64) p12="${value}" ;;
    MACOS_CERTIFICATE_PASSWORD) password="${value}" ;;
    SHED_DESKTOP_DEVELOPER_ID_IDENTITY) identity="${value}" ;;
    APPLE_TEAM_ID) team_id="${value}" ;;
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
note APPLE_TEAM_ID "${team_id}"

[ -z "${not_given}" ] || usage_fail "no --has flag for: ${not_given}"

kind=stable
case "${TAG}" in
  *-*) kind="a prerelease (server tags are stable-only; this gate still requires all four)" ;;
esac

if [ -n "${missing}" ]; then
  echo "::error::a server tag requires a Developer ID signed ShedServer.app; missing: ${missing}" >&2
  echo "server signing gate: ${TAG} (${kind}) is refused before anything publishes. Add the missing repository secrets (RELEASING.md, Secrets); the server has no ad hoc release path." >&2
  exit 1
fi

echo "server signing gate: ${TAG} (${kind}) has all four signing secrets; server-signing-preflight checks the certificate before any image publishes."
