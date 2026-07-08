#!/usr/bin/env bash
# Bump shed's release version for the selected components.
#
# The monorepo carries multiple release components on ONE vX.Y.Z tag
# family. A component ships in a release iff its version manifest equals
# the tag (scripts/release/release-plan.sh is the CI-side selector):
#
#   go       — the Go binaries + Claude Code plugin.
#              * Go binaries (shed, shed-server, shed-agent, ...) get their
#                version from a build-time ldflag injected by GoReleaser at
#                tag time — NOT bumped in the source tree.
#              * .claude-plugin/plugin.json IS a source-tree manifest and is
#                the go component's ship selector; bumped here by delegating
#                to scripts/set-version.sh (the existing, tested bumper),
#                then jq-verified.
#
#   desktop  — the shed-desktop app (absorbed from the old
#              shed-desktop repo's scripts/release/update-version.sh).
#              Bumps, in lockstep:
#              * desktop/VERSION            the macOS app's marketing version
#                                           (bundle.sh + shedctl identify read
#                                           it); drives the DMG + Sparkle
#                                           appcast. The desktop ship selector.
#              * crates/Cargo.toml          the Rust workspace
#                                           ([workspace.package].version; every
#                                           member inherits) + Cargo.lock regen.
#              * desktop/tauri/src-tauri    the Tauri client is its OWN cargo
#                                           workspace — Cargo.toml
#                                           [package].version + tauri.conf.json
#                                           + Cargo.lock regen (the lock pins
#                                           shed-core/shed-app by version, so a
#                                           stale lock breaks the .deb's
#                                           `cargo build --locked`).
#
# Contract (cc-plugins:release-workflows references/update-version/README.md):
#   - first arg: semver string, no `v` prefix
#   - optional `--components go,desktop` (default: go — preserves the
#     historical one-arg behavior; the release skill computes the set and
#     passes it explicitly)
#   - unknown component → hard error listing valid names
#   - idempotent (a same-version re-run leaves the tree unchanged)
#   - no network (cargo runs --offline)
#   - verifies its own work (jq/grep-back after every bump)
#   - doesn't `git add` (release skill stages + commits)
#
# Usage:
#   scripts/release/update-version.sh 0.8.0                       # go only
#   scripts/release/update-version.sh 0.8.0 --components desktop
#   scripts/release/update-version.sh 0.8.0 --components go,desktop

set -euo pipefail

usage() {
  echo "usage: $0 <X.Y.Z[-suffix]> [--components go,desktop]" >&2
  echo "  e.g. $0 0.8.0 --components go,desktop   (default components: go)" >&2
  exit 2
}

V=""
COMPONENTS="go"
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

if [[ ! "$V" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[a-zA-Z0-9.-]+)?$ ]]; then
  echo "error: '$V' is not semver (X.Y.Z or X.Y.Z-suffix)" >&2
  exit 2
fi

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"

DO_GO=false
DO_DESKTOP=false
IFS=',' read -r -a comps <<< "${COMPONENTS}"
[ "${#comps[@]}" -gt 0 ] || { echo "error: --components is empty (valid: go, desktop)" >&2; exit 2; }
for c in "${comps[@]}"; do
  case "$c" in
    go) DO_GO=true ;;
    desktop) DO_DESKTOP=true ;;
    *)
      echo "error: unknown component '${c}' (valid: go, desktop)" >&2
      exit 2
      ;;
  esac
done

# ---------------------------------------------------------------- component: go
if $DO_GO; then
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
