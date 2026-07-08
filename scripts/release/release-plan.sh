#!/usr/bin/env bash
# Decide which release components a tag ships (the manifest-selected release
# model — see RELEASING.md "Component selection").
#
# The monorepo carries multiple components on ONE vX.Y.Z tag family. A
# component ships in a release iff its version manifest equals the tag:
#
#   go       .claude-plugin/plugin.json  .version
#   desktop  desktop/VERSION
#
# Input:  the tag, as $1 or $GITHUB_REF_NAME (leading `v` tolerated).
# Output: exactly two `key=value` lines on stdout, suitable for
#         `>> "$GITHUB_OUTPUT"`:
#
#   ship_go=true|false
#   ship_desktop=true|false
#
# All diagnostics go to stderr so stdout stays machine-parseable.
#
# Hard failures (exit 1):
#   * NEITHER manifest matches the tag — a forgotten
#     scripts/release/update-version.sh run; failing loudly here prevents a
#     silent no-op release.
#   * ship_desktop, but the desktop version surfaces are out of lockstep:
#     desktop/VERSION must equal crates/Cargo.toml's [workspace.package]
#     version, desktop/tauri/src-tauri/Cargo.toml's [package] version,
#     tauri.conf.json's version, and the Tauri Cargo.lock's shed-core AND
#     shed-app entries (the .deb's `cargo build --locked` pins those).

set -euo pipefail

TAG="${1:-${GITHUB_REF_NAME:-}}"
if [ -z "${TAG}" ]; then
  echo "usage: $0 <tag>   (or set GITHUB_REF_NAME)" >&2
  exit 2
fi
V="${TAG#v}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"

GO_V="$(jq -r '.version' "${REPO_ROOT}/.claude-plugin/plugin.json")"
DESKTOP_V="$(tr -d '[:space:]' < "${REPO_ROOT}/desktop/VERSION")"

SHIP_GO=false
[ "${GO_V}" = "${V}" ] && SHIP_GO=true
SHIP_DESKTOP=false
[ "${DESKTOP_V}" = "${V}" ] && SHIP_DESKTOP=true

if [ "${SHIP_GO}" = "false" ] && [ "${SHIP_DESKTOP}" = "false" ]; then
  echo "::error::tag ${TAG} matches NO component manifest (go .claude-plugin/plugin.json=${GO_V}, desktop desktop/VERSION=${DESKTOP_V}). Run scripts/release/update-version.sh ${V} --components ... and re-tag — refusing a silent no-op release." >&2
  exit 1
fi

if [ "${SHIP_DESKTOP}" = "true" ]; then
  # Hard-verify desktop lockstep. Each surface below must equal
  # desktop/VERSION (== the tag); name the offender on mismatch.
  fail_lockstep() {
    echo "::error::desktop version lockstep broken: $1 is at '$2' but desktop/VERSION (== tag ${TAG}) is '${DESKTOP_V}'. Run scripts/release/update-version.sh ${V} --components desktop and re-tag." >&2
    exit 1
  }

  CRATES_V="$(grep -m1 '^version[[:space:]]*=' "${REPO_ROOT}/crates/Cargo.toml" | sed -E 's/.*"([^"]+)".*/\1/')"
  [ "${CRATES_V}" = "${DESKTOP_V}" ] || fail_lockstep "crates/Cargo.toml [workspace.package].version" "${CRATES_V}"

  TAURI_CARGO_V="$(grep -m1 '^version[[:space:]]*=' "${REPO_ROOT}/desktop/tauri/src-tauri/Cargo.toml" | sed -E 's/.*"([^"]+)".*/\1/')"
  [ "${TAURI_CARGO_V}" = "${DESKTOP_V}" ] || fail_lockstep "desktop/tauri/src-tauri/Cargo.toml [package].version" "${TAURI_CARGO_V}"

  TAURI_CONF_V="$(jq -r '.version' "${REPO_ROOT}/desktop/tauri/src-tauri/tauri.conf.json")"
  [ "${TAURI_CONF_V}" = "${DESKTOP_V}" ] || fail_lockstep "desktop/tauri/src-tauri/tauri.conf.json .version" "${TAURI_CONF_V}"

  # The Tauri lock's path-dep entries: build-deb.sh's `cargo build --locked`
  # pins shed-core/shed-app by version — a stale entry fails the .deb build
  # mid-release. `[[package]]` blocks put `version` on the line after `name`.
  for dep in shed-core shed-app; do
    LOCK_V="$(grep -A1 "^name = \"${dep}\"$" "${REPO_ROOT}/desktop/tauri/src-tauri/Cargo.lock" | grep -m1 '^version = ' | sed -E 's/.*"([^"]+)".*/\1/' || true)"
    [ "${LOCK_V}" = "${DESKTOP_V}" ] || fail_lockstep "desktop/tauri/src-tauri/Cargo.lock entry for ${dep}" "${LOCK_V:-<missing>}"
  done
fi

echo "release plan for ${TAG}: ship_go=${SHIP_GO} ship_desktop=${SHIP_DESKTOP} (go manifest=${GO_V}, desktop manifest=${DESKTOP_V})" >&2

echo "ship_go=${SHIP_GO}"
echo "ship_desktop=${SHIP_DESKTOP}"
