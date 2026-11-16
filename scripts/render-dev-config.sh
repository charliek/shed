#!/usr/bin/env bash
# render-dev-config.sh — render a throwaway parallel-dev shed-server config
# from a committed base, applying the per-developer overrides the Makefile's
# dev-server targets support (see: dev-server-up, dev-server-restart,
# dev-server-up-fc, dev-server-restart-fc).
#
# Never edits the committed base config (configs/server.dev-parallel.*.yaml).
# Writes to <output>, regenerated on every invocation, so it is safe to run
# repeatedly and safe to leave behind (the caller picks a path under
# ~/.shed/dev/, which is already scratch space). With NO overrides the
# Makefile skips this script entirely and runs the committed config verbatim —
# so a developer who sets nothing gets byte-identical behavior to before this
# script existed.
#
# Usage:
#   render-dev-config.sh <base.yaml> <out.yaml> [--state-dir DIR] [--users DIR]
#
# There is no auth-mode override: both committed dev-parallel configs run
# auth.mode: mtls, the only enforced mode shed-server loads. (The --token and
# --mtls flags of earlier versions are gone and are refused as unknown.)
#
# --state-dir   SHED_DEV_STATE_DIR. Relocates the dev server's BLOB-HEAVY
#               state — images_dir, instance_dir, snapshots_dir, uppers_dir —
#               under DIR, for a developer whose boot volume cannot host
#               several GB of dev images (this repo's dev store runs ~5 GB+).
#               The committed default stays `~/Library/Application Support/
#               shed-dev/vz`, so this is opt-in per machine and no other
#               developer is affected.
#
#               socket_dir is deliberately NOT relocated. Two reasons: a unix
#               socket path has a hard length limit (~104 bytes on macOS) that
#               a deep external-volume path can breach, and an unmounted
#               volume should fail as "no images" at startup rather than as a
#               confusing mid-run socket error. The brew shed-server on this
#               developer's machine is configured the same way.
#
# --users       SHED_DEV_USERS=itest. Gives the server the integration suite's
#               itest user list (auth.users): itest-admin, itest-alice and
#               itest-bob, with keys generated once under DIR (the suite's
#               SHED_ITEST_USERS_DIR, default ~/.shed/dev/itest-users), and
#               moves the base's auth.ssh key sources (github_users,
#               authorized_keys) under a user named `dev`, an admin, because
#               config load refuses auth.ssh key sources beside a user list.
#               The developer's own entry keeps working, as user `dev`. Done
#               by tests/integration/fixtures/identity.py through `uv run`, so
#               it needs uv; the output is re-serialised YAML (comments go).
#               Applied last, after --state-dir.
set -euo pipefail

usage() {
  echo "usage: $0 <base-config.yaml> <output-config.yaml> [--state-dir DIR] [--users DIR]" >&2
  exit 1
}

[ "$#" -ge 2 ] || usage
base="$1"
out="$2"
shift 2

state_dir=""
users_dir=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    --state-dir) [ "$#" -ge 2 ] || usage; state_dir="$2"; shift 2 ;;
    --users) [ "$#" -ge 2 ] && [ -n "$2" ] || usage; users_dir="$2"; shift 2 ;;
    *) echo "render-dev-config: unknown argument: $1" >&2; usage ;;
  esac
done

if [ ! -f "$base" ]; then
  echo "render-dev-config: base config not found: $base" >&2
  exit 1
fi

# `sed -i` takes an empty backup-suffix argument on BSD (macOS) and rejects one
# on GNU. Pick the form once rather than at every call site.
sed_inplace() {
  if sed --version >/dev/null 2>&1; then
    sed -E -i "$@"          # GNU
  else
    sed -E -i '' "$@"       # BSD / macOS
  fi
}

mkdir -p "$(dirname "$out")"
cp "$base" "$out"
applied=()

# ---- --state-dir ------------------------------------------------------------
if [ -n "$state_dir" ]; then
  state_dir="${state_dir%/}"   # strip a trailing slash so paths never double up

  # Rewrite only the blob-heavy dirs, and only where the base actually declares
  # them — a base config that inherits a default for one of these keeps
  # inheriting it rather than silently gaining an override this script
  # invented. Each substitution is anchored to the key at line start (with its
  # own indent preserved), so a VALUE containing the key name cannot be hit.
  rewrote=0
  for key in images_dir instance_dir snapshots_dir uppers_dir; do
    grep -qE "^[[:space:]]*${key}:" "$out" || continue
    case "$key" in
      images_dir)    sub="$state_dir" ;;
      instance_dir)  sub="$state_dir/instances" ;;
      snapshots_dir) sub="$state_dir/snapshots" ;;
      uppers_dir)    sub="$state_dir/uppers" ;;
    esac
    sed_inplace "s|^([[:space:]]*)${key}:[[:space:]]*.*$|\\1${key}:    ${sub}|" "$out"
    rewrote=$((rewrote + 1))
  done

  if [ "$rewrote" -eq 0 ]; then
    echo "render-dev-config: --state-dir was given but $base declares none of" \
         "images_dir/instance_dir/snapshots_dir/uppers_dir; refusing to" \
         "silently do nothing" >&2
    exit 1
  fi

  cat >> "$out" <<EOF

# --- Generated by scripts/render-dev-config.sh for SHED_DEV_STATE_DIR ---
# The $rewrote blob-heavy state dir(s) above were rewritten under
#   $state_dir
# socket_dir is deliberately left where the base config put it (unix socket
# path length limits; see the script header).
EOF
  applied+=("state dir: $state_dir")
fi

# ---- --users ----------------------------------------------------------------
if [ -n "$users_dir" ]; then
  if ! command -v uv >/dev/null 2>&1; then
    echo "render-dev-config: --users needs uv (https://docs.astral.sh/uv/)" >&2
    exit 1
  fi
  repo="$(cd "$(dirname "$0")/.." && pwd)"
  abs_out="$(cd "$(dirname "$out")" && pwd)/$(basename "$out")"
  mkdir -p "$users_dir"
  abs_users="$(cd "$users_dir" && pwd)"
  # Rendered next to the output, then moved over it, so a failure leaves no
  # half-written config behind.
  if ! (cd "$repo/tests/integration" && uv run --quiet python -m fixtures.identity --dir "$abs_users" \
          render-users "$abs_out" "$abs_out.users") >&2; then
    rm -f "$abs_out.users"
    echo "render-dev-config: --users failed" >&2
    exit 1
  fi
  mv "$abs_out.users" "$abs_out"
  applied+=("itest user list: keys in $abs_users")
fi

if [ "${#applied[@]}" -eq 0 ]; then
  echo "render-dev-config: no overrides requested — the caller should have run" \
       "the committed base config directly" >&2
  exit 1
fi

joined=""
for a in "${applied[@]}"; do
  [ -z "$joined" ] && joined="$a" || joined="$joined, $a"
done
echo "render-dev-config: wrote $out ($joined)"
