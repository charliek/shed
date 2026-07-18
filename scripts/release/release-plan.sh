#!/usr/bin/env bash
# Decide which release components a tag ships (the manifest-selected release
# model — see RELEASING.md "Component selection").
#
# The monorepo carries multiple components on ONE vX.Y.Z tag family. A
# component ships in a release iff its version manifest equals the tag:
#
#   server      .claude-plugin/plugin.json  .version
#   host-agent  crates/shed-host-agent/VERSION
#   machine-rc  cmd/shed-machine-rc/VERSION
#   desktop     desktop/VERSION
#
# `server`, `host-agent` and `machine-rc` are the three goreleaser-published
# components; `ship_goreleaser` is their OR (true iff the tag ships at least
# one goreleaser component). Only `desktop` has a beta channel — the three
# goreleaser components are stable-only.
#
# Input:  the tag, as $1 or $GITHUB_REF_NAME (leading `v` tolerated).
# Output: exactly these `key=value` lines on stdout, suitable for
#         `>> "$GITHUB_OUTPUT"`:
#
#   ship_server=true|false
#   ship_host_agent=true|false
#   ship_machine_rc=true|false
#   ship_desktop=true|false
#   ship_goreleaser=true|false     # OR of the three goreleaser components
#   ship_go=true|false             # LEGACY — see below
#
# The legacy `ship_go` line (== ship_server) is emitted so this commit stays
# self-consistent while publish-images.yaml still reads `ship_go`; it is
# removed when the workflow rewrite (C4) drops its last consumer.
#
# All diagnostics go to stderr so stdout stays machine-parseable.
#
# Hard failures:
#   * exit 2 — malformed tag (usage-style).
#   * exit 1 — a manifest file is missing / empty / multiline / not semver;
#     NO manifest matches the tag (a forgotten
#     scripts/release/update-version.sh run — failing loudly here prevents a
#     silent no-op release); ship_desktop but the desktop version surfaces are
#     out of lockstep; a prerelease tag ships a goreleaser component
#     (stable-only); or (stable tags only) the CHANGELOG `**Ships:**` line
#     disagrees with the manifest-computed ship set.

set -euo pipefail

TAG="${1:-${GITHUB_REF_NAME:-}}"
if [ -z "${TAG}" ]; then
  echo "usage: $0 <tag>   (or set GITHUB_REF_NAME)" >&2
  exit 2
fi

# Shared semver grammar (X.Y.Z with an optional -prerelease suffix), reused by
# the tag check and every manifest validation so the definition can't drift.
SEMVER_RE='[0-9]+\.[0-9]+\.[0-9]+(-[A-Za-z0-9.-]+)?'

# The tag must be a plain vX.Y.Z or a vX.Y.Z-prerelease. Reject anything else
# before touching manifests so a typo can't be misread as a no-match.
if [[ ! "${TAG}" =~ ^v?${SEMVER_RE}$ ]]; then
  echo "usage: $0 <tag>   tag '${TAG}' is not vX.Y.Z[-prerelease]" >&2
  exit 2
fi
V="${TAG#v}"

# A prerelease tag carries a `-suffix` (e.g. v2.1.0-rc.1).
IS_PRERELEASE=false
case "${V}" in
  *-*) IS_PRERELEASE=true ;;
esac

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
CHANGELOG="${REPO_ROOT}/CHANGELOG.md"

# The [package]/[workspace.package] version from a Cargo.toml: the only
# line-anchored `version = "..."` (deps are inline `{ version = "..." }`).
toml_version() { grep -m1 '^version[[:space:]]*=' "$1" | sed -E 's/.*"([^"]+)".*/\1/'; }

# A plain VERSION manifest must be a single non-empty semver line — a missing,
# empty, multiline or malformed file is a source-tree bug, not a no-match.
# Echoes the version on success; exits 1 (naming the file) otherwise.
read_version_file() {
  local file="$1" raw
  if [ ! -f "${file}" ]; then
    echo "::error::manifest ${file} is missing" >&2
    exit 1
  fi
  # Command substitution strips the trailing newline; an all-whitespace file
  # collapses to empty. Any embedded newline (a genuine second line) survives
  # and trips the multiline check below.
  raw="$(cat "${file}")"
  if [ -z "${raw}" ]; then
    echo "::error::manifest ${file} is empty" >&2
    exit 1
  fi
  if [[ "${raw}" == *$'\n'* ]]; then
    echo "::error::manifest ${file} must be a single version line" >&2
    exit 1
  fi
  if [[ ! "${raw}" =~ ^${SEMVER_RE}$ ]]; then
    echo "::error::manifest ${file} = '${raw}' is not semver (X.Y.Z[-prerelease])" >&2
    exit 1
  fi
  printf '%s' "${raw}"
}

# server's manifest is JSON; a missing/malformed file makes jq die under
# `set -euo pipefail` with a raw jq error, so capture its failure and emit the
# script's own `::error::` naming the file. Then validate the extracted
# .version is non-empty and semver-shaped (jq prints `null` for an absent key).
SERVER_V="$(jq -r '.version' "${REPO_ROOT}/.claude-plugin/plugin.json")" || {
  echo "::error::manifest .claude-plugin/plugin.json is missing or not valid JSON" >&2
  exit 1
}
if [ -z "${SERVER_V}" ] || [ "${SERVER_V}" = "null" ] || [[ ! "${SERVER_V}" =~ ^${SEMVER_RE}$ ]]; then
  echo "::error::manifest .claude-plugin/plugin.json .version = '${SERVER_V}' is not semver (X.Y.Z[-prerelease])" >&2
  exit 1
fi

HOST_AGENT_V="$(read_version_file "${REPO_ROOT}/crates/shed-host-agent/VERSION")"
MACHINE_RC_V="$(read_version_file "${REPO_ROOT}/cmd/shed-machine-rc/VERSION")"
DESKTOP_V="$(read_version_file "${REPO_ROOT}/desktop/VERSION")"

SHIP_SERVER=false
[ "${SERVER_V}" = "${V}" ] && SHIP_SERVER=true
SHIP_HOST_AGENT=false
[ "${HOST_AGENT_V}" = "${V}" ] && SHIP_HOST_AGENT=true
SHIP_MACHINE_RC=false
[ "${MACHINE_RC_V}" = "${V}" ] && SHIP_MACHINE_RC=true
SHIP_DESKTOP=false
[ "${DESKTOP_V}" = "${V}" ] && SHIP_DESKTOP=true

# ship_goreleaser is the OR of the three goreleaser components, computed here
# (not inferred in YAML) so the "any goreleaser component" decision is unit-tested.
SHIP_GORELEASER=false
if [ "${SHIP_SERVER}" = "true" ] || [ "${SHIP_HOST_AGENT}" = "true" ] || [ "${SHIP_MACHINE_RC}" = "true" ]; then
  SHIP_GORELEASER=true
fi

# LEGACY: publish-images.yaml still reads ship_go; keep it == ship_server until
# C4 removes the last consumer (see the header note).
SHIP_GO="${SHIP_SERVER}"

if [ "${SHIP_SERVER}" = "false" ] && [ "${SHIP_HOST_AGENT}" = "false" ] \
   && [ "${SHIP_MACHINE_RC}" = "false" ] && [ "${SHIP_DESKTOP}" = "false" ]; then
  echo "::error::tag ${TAG} matches NO component manifest (server .claude-plugin/plugin.json=${SERVER_V}, host-agent crates/shed-host-agent/VERSION=${HOST_AGENT_V}, machine-rc cmd/shed-machine-rc/VERSION=${MACHINE_RC_V}, desktop desktop/VERSION=${DESKTOP_V}). Run scripts/release/update-version.sh ${V} --components ... and re-tag — refusing a silent no-op release." >&2
  exit 1
fi

if [ "${SHIP_DESKTOP}" = "true" ]; then
  # Hard-verify desktop lockstep. Each surface below must equal
  # desktop/VERSION (== the tag); name the offender on mismatch.
  fail_lockstep() {
    echo "::error::desktop version lockstep broken: $1 is at '$2' but desktop/VERSION (== tag ${TAG}) is '${DESKTOP_V}'. Run scripts/release/update-version.sh ${V} --components desktop and re-tag." >&2
    exit 1
  }

  CRATES_V="$(toml_version "${REPO_ROOT}/crates/Cargo.toml")"
  [ "${CRATES_V}" = "${DESKTOP_V}" ] || fail_lockstep "crates/Cargo.toml [workspace.package].version" "${CRATES_V}"

  TAURI_CARGO_V="$(toml_version "${REPO_ROOT}/desktop/tauri/src-tauri/Cargo.toml")"
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

# Stable-only guard: the three goreleaser components have no beta channel, so a
# prerelease (`-suffix`) tag must not select any of them. A desktop-only
# prerelease (the Tauri rc rehearsal) is fine and falls through cleanly.
if [ "${IS_PRERELEASE}" = "true" ] && [ "${SHIP_GORELEASER}" = "true" ]; then
  echo "::error::prerelease tag ${TAG} ships a goreleaser component (server=${SHIP_SERVER}, host-agent=${SHIP_HOST_AGENT}, machine-rc=${SHIP_MACHINE_RC}) — those components are stable-only (only desktop has a beta channel). Cut a stable tag, or bump only the desktop surfaces for an rc." >&2
  exit 1
fi

# Ships cross-check (stable tags only): the CHANGELOG entry's `**Ships:**` line
# must agree with the manifest-computed ship set. Prerelease tags have no
# CHANGELOG entry and skip the whole check.
if [ "${IS_PRERELEASE}" = "false" ]; then
  # Body of the `## v<V> — <date>` section (heading excluded), up to the next
  # `## ` heading. index()==1 anchors an exact prefix match — no regex-metachar
  # surprises from the version's dots — and naturally skips `## Unreleased`
  # (its own `**Ships:**` line must never be read).
  # `!seen` gates the section-open rule to the FIRST matching heading, so a
  # pathological second `## v<V> — ` heading is treated as a section boundary
  # (the exit rule below fires) rather than re-opening the capture and merging
  # the two bodies — which would let a Ships-less first section borrow the
  # second's Ships line.
  section="$(awk -v head="## v${V} — " '
    index($0, head) == 1 && !seen { seen = 1; insec = 1; next }
    insec && /^## / { exit }
    insec { print }
  ' "${CHANGELOG}")"

  if [ -z "${section}" ]; then
    echo "::error::CHANGELOG.md has no '## v${V} — <date>' section — a stable tag (${TAG}) requires a changelog entry with a **Ships:** line." >&2
    exit 1
  fi

  ships_line="$(printf '%s\n' "${section}" | grep -m1 '^\*\*Ships:\*\*' || true)"
  if [ -z "${ships_line}" ]; then
    echo "::error::CHANGELOG.md '## v${V}' section has no **Ships:** line." >&2
    exit 1
  fi

  # Grammar: `**Ships:** token(, token)*`, split on `\s*,\s*`. Tokens are
  # case-sensitive canonical names; `server/CLI` is the sole legacy alias.
  tokens_raw="${ships_line#\*\*Ships:\*\*}"
  # `read -a` silently DROPS a trailing empty field, so a trailing comma
  # (`**Ships:** server,`) would slip past the per-token empty check below.
  # Trim surrounding whitespace and reject a leading or trailing comma up front;
  # internal double commas still surface as an empty token in the loop.
  tokens_trimmed="$(printf '%s' "${tokens_raw}" | sed -E 's/^[[:space:]]+//; s/[[:space:]]+$//')"
  if [[ "${tokens_trimmed}" == ,* || "${tokens_trimmed}" == *, ]]; then
    echo "::error::CHANGELOG.md '## v${V}' **Ships:** line '${ships_line}' has an empty token (leading/trailing comma; grammar: token(, token)*)." >&2
    exit 1
  fi
  declare -A ship_seen=()
  actual_ships=()
  IFS=',' read -r -a ship_parts <<< "${tokens_raw}"
  for part in "${ship_parts[@]}"; do
    tok="$(printf '%s' "${part}" | sed -E 's/^[[:space:]]+//; s/[[:space:]]+$//')"
    if [ -z "${tok}" ]; then
      echo "::error::CHANGELOG.md '## v${V}' **Ships:** line has an empty token (grammar: token(, token)*)." >&2
      exit 1
    fi
    # Legacy alias, applied BEFORE canonical + duplicate checks.
    [ "${tok}" = "server/CLI" ] && tok="server"
    case "${tok}" in
      server|host-agent|machine-rc|desktop) ;;
      *)
        echo "::error::CHANGELOG.md '## v${V}' **Ships:** line has unknown token '${tok}' (valid: server, host-agent, machine-rc, desktop; alias: server/CLI)." >&2
        exit 1
        ;;
    esac
    if [ -n "${ship_seen[${tok}]:-}" ]; then
      echo "::error::CHANGELOG.md '## v${V}' **Ships:** line has duplicate token '${tok}' (post-aliasing)." >&2
      exit 1
    fi
    ship_seen[${tok}]=1
    actual_ships+=("${tok}")
  done

  # Manifest-computed ship set.
  expected_ships=()
  [ "${SHIP_SERVER}" = "true" ] && expected_ships+=(server)
  [ "${SHIP_HOST_AGENT}" = "true" ] && expected_ships+=(host-agent)
  [ "${SHIP_MACHINE_RC}" = "true" ] && expected_ships+=(machine-rc)
  [ "${SHIP_DESKTOP}" = "true" ] && expected_ships+=(desktop)

  actual_sorted="$(printf '%s\n' "${actual_ships[@]}" | sort | paste -sd, -)"
  expected_sorted="$(printf '%s\n' "${expected_ships[@]}" | sort | paste -sd, -)"
  if [ "${actual_sorted}" != "${expected_sorted}" ]; then
    echo "::error::CHANGELOG.md '## v${V}' **Ships:** set (${actual_sorted}) disagrees with the manifest-computed ship set (${expected_sorted}). Fix the CHANGELOG Ships line or re-run update-version.sh for the intended components." >&2
    exit 1
  fi
fi

echo "release plan for ${TAG}: server=${SHIP_SERVER} host-agent=${SHIP_HOST_AGENT} machine-rc=${SHIP_MACHINE_RC} desktop=${SHIP_DESKTOP} goreleaser=${SHIP_GORELEASER} (manifests: server=${SERVER_V}, host-agent=${HOST_AGENT_V}, machine-rc=${MACHINE_RC_V}, desktop=${DESKTOP_V})" >&2

echo "ship_server=${SHIP_SERVER}"
echo "ship_host_agent=${SHIP_HOST_AGENT}"
echo "ship_machine_rc=${SHIP_MACHINE_RC}"
echo "ship_desktop=${SHIP_DESKTOP}"
echo "ship_goreleaser=${SHIP_GORELEASER}"
# LEGACY (removed in C4): kept == ship_server for publish-images.yaml.
echo "ship_go=${SHIP_GO}"
