#!/usr/bin/env bash
# Bump shed's release version for the selected components.
#
# The monorepo carries FOUR release components on ONE vX.Y.Z tag family. A
# component ships in a release iff its version manifest equals the tag
# (scripts/release/release-plan.sh is the CI-side selector):
#
#   server      — the Go binaries + Claude Code plugin (brew `shed`, apt
#                 `shed-server` deb, rootfs vz/fc images + build-tools).
#                 * Go binaries (shed, shed-server, shed-agent, ...) get their
#                   version from a build-time ldflag injected by GoReleaser at
#                   tag time — NOT bumped in the source tree.
#                 * .claude-plugin/plugin.json IS a source-tree manifest and is
#                   the server component's ship selector; bumped here by
#                   delegating to scripts/set-version.sh (the existing, tested
#                   bumper), then jq-verified.
#                 (Historically named `go`; `go` is still accepted as a
#                 deprecated alias — it prints a one-line stderr warning and
#                 behaves identically.)
#
#   host-agent  — the host-side credential broker (brew `shed-host-agent` + a
#                 GH linux tarball). Its shipped version is the tag ldflag
#                 (crates/shed-host-agent/src/version.rs), NOT CARGO_PKG_VERSION
#                 — so its ship selector is a standalone file,
#                 crates/shed-host-agent/VERSION, written here and grep-verified.
#                 (Intentionally divergent from crates/Cargo.toml's workspace
#                 version, which the desktop component owns.)
#
#   machine-rc  — the host-side RC-session helper (brew + apt `shed-machine-rc`).
#                 Ship selector: cmd/shed-machine-rc/VERSION (standalone file,
#                 same rationale as host-agent), written here and grep-verified.
#
#   desktop     — the shed-desktop app (absorbed from the old shed-desktop
#                 repo's scripts/release/update-version.sh). Bumps, in lockstep:
#                 * desktop/VERSION            the macOS app's marketing version
#                                              (bundle.sh + shedctl identify read
#                                              it); drives the DMG + Sparkle
#                                              appcast. The desktop ship selector.
#                 * crates/Cargo.toml          the Rust workspace
#                                              ([workspace.package].version; every
#                                              member inherits) + Cargo.lock regen.
#                 * desktop/tauri/src-tauri    the Tauri client is its OWN cargo
#                                              workspace — Cargo.toml
#                                              [package].version + tauri.conf.json
#                                              + Cargo.lock regen (the lock pins
#                                              shed-core/shed-app by version, so a
#                                              stale lock breaks the .deb's
#                                              `cargo build --locked`).
#
# Contract (cc-plugins:release-workflows references/update-version/README.md):
#   - first arg: semver string, no `v` prefix
#   - optional `--components server,host-agent,machine-rc,desktop`
#     (default: server — preserves the historical one-arg behavior; the release
#     skill computes the set from recommend-components.sh and passes it
#     explicitly). `go` accepted as a deprecated alias for `server`.
#   - unknown component → hard error listing valid names
#   - PRERELEASE versions (X.Y.Z-suffix) are rejected for the three
#     goreleaser components (server / host-agent / machine-rc) — they are
#     stable-only. A desktop-only prerelease is allowed (the Tauri rc-rehearsal
#     path).
#   - idempotent (a same-version re-run leaves the tree unchanged)
#   - no network (cargo runs --offline)
#   - verifies its own work (jq/grep-back after every bump)
#   - doesn't `git add` (release skill stages + commits)
#
# Usage:
#   scripts/release/update-version.sh 0.8.0                              # server only
#   scripts/release/update-version.sh 0.8.0 --components host-agent
#   scripts/release/update-version.sh 0.8.0 --components machine-rc
#   scripts/release/update-version.sh 0.8.0 --components desktop
#   scripts/release/update-version.sh 0.8.0 --components server,host-agent,machine-rc,desktop

set -euo pipefail

usage() {
  echo "usage: $0 <X.Y.Z[-suffix]> [--components server,host-agent,machine-rc,desktop]" >&2
  echo "  e.g. $0 0.8.0 --components server,desktop   (default components: server)" >&2
  exit 2
}

V=""
COMPONENTS="server"
while [ $# -gt 0 ]; do
  case "$1" in
    --components)
      [ $# -ge 2 ] || usage
      COMPONENTS="$2"
      shift 2
      ;;
    --components=*)
      COMPONENTS="${1#--components=}"
      shift
      ;;
    -*)
      echo "error: unknown flag '$1'" >&2
      usage
      ;;
    *)
      [ -z "$V" ] || usage
      V="$1"
      shift
      ;;
  esac
done
[ -n "$V" ] || usage

# Each numeric field is (0|[1-9][0-9]*): a leading zero (e.g. 0.07.11) is
# rejected as malformed rather than silently accepted/misparsed.
if [[ ! "$V" =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[a-zA-Z0-9.-]+)?$ ]]; then
  echo "error: '$V' is not semver (X.Y.Z or X.Y.Z-suffix; no leading zeros)" >&2
  exit 2
fi

# A prerelease version carries a `-suffix` (e.g. 2.1.0-rc.1). Only the desktop
# component has a beta channel — the three goreleaser components are stable-only.
IS_PRERELEASE=false
case "$V" in
  *-*) IS_PRERELEASE=true ;;
esac

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"

VALID_COMPONENTS="server, host-agent, machine-rc, desktop"

DO_SERVER=false
DO_HOST_AGENT=false
DO_MACHINE_RC=false
DO_DESKTOP=false
IFS=',' read -r -a comps <<< "${COMPONENTS}"
[ "${#comps[@]}" -gt 0 ] || { echo "error: --components is empty (valid: ${VALID_COMPONENTS})" >&2; exit 2; }
GO_ALIAS_WARNED=false
for c in "${comps[@]}"; do
  case "$c" in
    go)
      # Deprecated alias for `server` (the component was renamed from `go`).
      # Warn once per invocation even if `go` is repeated (e.g. --components go,go).
      if [ "${GO_ALIAS_WARNED}" = "false" ]; then
        echo "warning: component 'go' is a deprecated alias for 'server' — use --components server" >&2
        GO_ALIAS_WARNED=true
      fi
      DO_SERVER=true
      ;;
    server) DO_SERVER=true ;;
    host-agent) DO_HOST_AGENT=true ;;
    machine-rc) DO_MACHINE_RC=true ;;
    desktop) DO_DESKTOP=true ;;
    *)
      echo "error: unknown component '${c}' (valid: ${VALID_COMPONENTS})" >&2
      exit 2
      ;;
  esac
done

# Stable-only guard for the three goreleaser components. A prerelease version is
# only meaningful for a desktop-only rc rehearsal; selecting server/host-agent/
# machine-rc with a `-suffix` version is a mistake (goreleaser components have no
# beta channel — release-plan.sh would reject the resulting tag anyway).
if [ "${IS_PRERELEASE}" = "true" ]; then
  if $DO_SERVER || $DO_HOST_AGENT || $DO_MACHINE_RC; then
    echo "error: prerelease version '${V}' selects a goreleaser component (server/host-agent/machine-rc), which are stable-only." >&2
    echo "       Only --components desktop may take a prerelease (the Tauri rc-rehearsal path)." >&2
    exit 1
  fi
fi

# Write a standalone VERSION ship-selector (host-agent / machine-rc) and
# grep-verify the write landed. Both files are ship-selectors ONLY — the shipped
# binary's version is the tag ldflag (e.g. crates/shed-host-agent/src/version.rs),
# NOT this file — and are intentionally independent of crates/Cargo.toml's
# workspace version (owned by the desktop component). A single trailing newline;
# argument is the repo-relative path.
bump_selector_file() {
  local rel="$1" abs="${REPO_ROOT}/$1"
  printf '%s\n' "${V}" > "${abs}"
  if ! grep -qx "${V}" "${abs}"; then
    echo "error: ${rel} did not bump to ${V}" >&2
    exit 1
  fi
  echo "${rel} -> ${V}"
}

# ------------------------------------------------------------ component: server
if $DO_SERVER; then
  PLUGIN_JSON="${REPO_ROOT}/.claude-plugin/plugin.json"

  # Delegate to the existing plugin.json bumper.
  "${REPO_ROOT}/scripts/set-version.sh" "$V"

  # Verify the bump actually landed. set-version.sh's sed silently no-ops
  # if the `"version"` field is missing or formatted differently than
  # expected, then exits 0 — which would let the release skill commit a
  # stale plugin.json and tag it. Use jq to anchor to the TOP-LEVEL
  # .version field; a regex would also (incorrectly) match a future
  # nested "version" field (e.g. in a future "dependencies" block).
  if [ "$(jq -r '.version' "${PLUGIN_JSON}")" != "${V}" ]; then
    echo "error: plugin.json top-level .version did not bump to ${V}" >&2
    exit 1
  fi
fi

# -------------------------------------------------------- component: host-agent
if $DO_HOST_AGENT; then
  bump_selector_file "crates/shed-host-agent/VERSION"
fi

# -------------------------------------------------------- component: machine-rc
if $DO_MACHINE_RC; then
  bump_selector_file "cmd/shed-machine-rc/VERSION"
fi

# ----------------------------------------------------------- component: desktop
if $DO_DESKTOP; then
  # Resolve cargo even from a non-login shell (make / a release subprocess)
  # via ~/.cargo/env.
  if ! command -v cargo >/dev/null 2>&1 && [ -f "${HOME}/.cargo/env" ]; then
    # shellcheck disable=SC1091
    . "${HOME}/.cargo/env"
  fi
  if ! command -v cargo >/dev/null 2>&1; then
    echo "error: cargo not found (install Rust via rustup, or put it on PATH)." >&2
    exit 1
  fi

  # 1. The macOS marketing version (the desktop component's ship selector).
  printf '%s\n' "${V}" > "${REPO_ROOT}/desktop/VERSION"
  echo "desktop/VERSION -> ${V}"

  # 2. The Rust workspace version. Variable whitespace on the LHS (tolerant of
  #    a reflow) → a single-space replacement, matching crates/Cargo.toml's
  #    layout. `^version = "..."` is the only line-anchored version in the file
  #    (deps are inline `{ version = "..." }`), so the anchored replace is safe.
  cd "${REPO_ROOT}/crates"
  sed -i.bak -E 's/^version[[:space:]]*=[[:space:]]*"[^"]+"/version = "'"$V"'"/' Cargo.toml
  rm -f Cargo.toml.bak
  if ! grep -q "^version = \"$V\"" Cargo.toml; then
    echo "error: crates/Cargo.toml's [workspace.package].version did not update to $V." >&2
    echo "       Inspect by hand — the sed pattern may not match the current layout." >&2
    exit 1
  fi

  # 3. Regenerate crates/Cargo.lock so the workspace-member entries match.
  #    --offline is safe: only internal version strings change, not the dep
  #    tree.
  cargo update --workspace --offline >/dev/null
  if ! grep -q "^version = \"$V\"" Cargo.lock; then
    echo "error: crates/Cargo.lock did not update to $V — a member may override the version." >&2
    exit 1
  fi
  echo "crates/Cargo.toml + crates/Cargo.lock -> ${V}"

  # 4. The Tauri client (the shipped Linux .deb's source) is its OWN cargo
  #    workspace (a standalone [workspace] table), so it does NOT inherit the
  #    crates workspace's [workspace.package].version, and its committed
  #    Cargo.lock pins the shed-core / shed-app path deps by version. If we
  #    bump crates/ but leave this workspace at the old version,
  #    desktop/linux/scripts/build-deb.sh's `cargo build --locked` fails (the
  #    lock is stale). Bump the crate + tauri.conf.json and regenerate the lock
  #    so the .deb build stays green. `^version = "..."` is the only
  #    line-anchored version (deps are inline); tauri.conf.json's top-level
  #    "version" feeds the Tauri bundler.
  cd "${REPO_ROOT}/desktop/tauri/src-tauri"
  sed -i.bak -E 's/^version[[:space:]]*=[[:space:]]*"[^"]+"/version = "'"$V"'"/' Cargo.toml
  rm -f Cargo.toml.bak
  if ! grep -q "^version = \"$V\"" Cargo.toml; then
    echo "error: desktop/tauri/src-tauri/Cargo.toml's [package].version did not update to $V." >&2
    exit 1
  fi
  sed -i.bak -E 's/^([[:space:]]*)"version"[[:space:]]*:[[:space:]]*"[^"]+"/\1"version": "'"$V"'"/' tauri.conf.json
  rm -f tauri.conf.json.bak
  if ! grep -q "\"version\": \"$V\"" tauri.conf.json; then
    echo "error: desktop/tauri/src-tauri/tauri.conf.json's version did not update to $V." >&2
    exit 1
  fi
  # Regenerate the Tauri lock: `cargo update --workspace` rewrites the lock,
  # refreshing both the workspace member (shed-desktop-tauri) AND the
  # shed-core/shed-app path-dep entries (re-read from crates/Cargo.toml, now
  # $V) — like the crates step above.
  cargo update --workspace --offline >/dev/null
  if ! grep -q "^version = \"$V\"" Cargo.lock; then
    echo "error: desktop/tauri/src-tauri/Cargo.lock did not update to $V." >&2
    exit 1
  fi
  # Guard the exact failure this step prevents: the shed-core/shed-app
  # path-dep lock entries (which build-deb.sh's `cargo build --locked` pins)
  # must have refreshed to $V. The generic check above only proves the member
  # (shed-desktop-tauri) is $V; a future cargo change that stopped refreshing
  # path deps would slip past it.
  for dep in shed-core shed-app; do
    if ! grep -A1 "^name = \"${dep}\"$" Cargo.lock | grep -q "^version = \"$V\""; then
      echo "error: desktop/tauri/src-tauri/Cargo.lock still pins ${dep} at the old version (expected $V)." >&2
      exit 1
    fi
  done
  echo "desktop/tauri/src-tauri (Cargo.toml + tauri.conf.json + Cargo.lock) -> ${V}"
fi
