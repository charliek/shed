#!/usr/bin/env bash
# Build the shed-desktop Linux .deb: cargo build --release the Tauri client +
# shedctl, stage them + packaging assets, and run nfpm to emit
# out/shed-desktop_<ver>_<arch>.deb.
#
# The shipped Linux client is the Tauri (React/WebKitGTK) app on the shared
# shed-core; its release binary is renamed shed-desktop-tauri -> /usr/bin/shed-desktop.
#
# Run on the target architecture (no cross-compile): an amd64 deb is built on
# amd64, arm64 on arm64. Prereqs (Ubuntu/Debian): the Tauri build deps
# (libwebkit2gtk-4.1-dev libgtk-3-dev libsoup-3.0-dev librsvg2-dev
# libayatana-appindicator3-dev pkg-config build-essential), a Rust toolchain,
# nfpm on PATH (https://nfpm.goreleaser.com), AND a prebuilt frontend bundle at
# tauri/ui/dist (run `npm --prefix tauri/ui ci && npm --prefix tauri/ui run build`
# first — a plain `cargo build` does NOT run tauri's beforeBuildCommand, and
# generate_context! fails closed without the bundle). `make deb` runs this inside
# the Linux Docker with the bundle built on the host and mounted in.
#
# Usage: ./linux/scripts/build-deb.sh 0.0.1
set -euo pipefail

VERSION="${1:-}"
if [ -z "${VERSION}" ]; then
  echo "usage: $0 <version>   (e.g. 0.0.1 or 0.0.1-dev)" >&2
  exit 1
fi

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
cd "${REPO_ROOT}"

# The shared Rust core lives in the top-level crates/ workspace (one level above
# the desktop package root); the Tauri client is a standalone workspace under
# tauri/src-tauri.
CRATES_DIR="$(cd "${REPO_ROOT}/../crates" && pwd)"

# Cargo resolves a relative CARGO_TARGET_DIR relative to each workspace's own dir
# (crates/, tauri/src-tauri/), but the binary-path probes below run from the repo
# root — make it absolute so both agree. (Callers today pass an absolute /target
# or leave it unset; this just closes the relative-path foot-gun.)
if [ -n "${CARGO_TARGET_DIR:-}" ] && [ "${CARGO_TARGET_DIR#/}" = "${CARGO_TARGET_DIR}" ]; then
  CARGO_TARGET_DIR="${REPO_ROOT}/${CARGO_TARGET_DIR}"
  export CARGO_TARGET_DIR
fi

# Debian arch name: prefer dpkg (the real target); fall back to uname on a
# non-Debian dev host (a layout smoke — the deb won't be functional there).
if command -v dpkg >/dev/null 2>&1; then
  SHED_DEB_ARCH="$(dpkg --print-architecture)"
else
  case "$(uname -m)" in
    x86_64|amd64) SHED_DEB_ARCH="amd64" ;;
    aarch64|arm64) SHED_DEB_ARCH="arm64" ;;
    *) echo "error: unsupported arch $(uname -m)" >&2; exit 1 ;;
  esac
fi
export SHED_DEB_ARCH
export SHED_DEB_VERSION="${VERSION}"

# The frontend bundle must be prebuilt (see the header): generate_context! reads
# tauri.conf.json's frontendDist (../ui/dist) at compile time and fails closed if
# it's absent. Fail fast with a clear pointer rather than a cryptic cargo error.
if [ ! -f "${REPO_ROOT}/tauri/ui/dist/index.html" ]; then
  echo "error: tauri/ui/dist is missing — build the frontend first:" >&2
  echo "       npm --prefix tauri/ui ci && npm --prefix tauri/ui run build" >&2
  exit 1
fi

# Two cargo workspaces with distinct target dirs. When CARGO_TARGET_DIR is set
# (the Docker path shares one volume), give each workspace its own subdir so they
# never clobber each other; when unset (local dev / native release runner), use
# each workspace's default target dir.
if [ -n "${CARGO_TARGET_DIR:-}" ]; then
  TAURI_TARGET="${CARGO_TARGET_DIR}/tauri"
  CORE_TARGET="${CARGO_TARGET_DIR}/core"
else
  TAURI_TARGET="${REPO_ROOT}/tauri/src-tauri/target"
  CORE_TARGET="${CRATES_DIR}/target"
fi

echo "==> cargo build --release -p shedctl (crates workspace)"
( cd "${CRATES_DIR}" && CARGO_TARGET_DIR="${CORE_TARGET}" cargo build --release --locked -p shedctl )

echo "==> cargo build --release (Tauri client, standalone workspace)"
( cd tauri/src-tauri && CARGO_TARGET_DIR="${TAURI_TARGET}" cargo build --release --locked )

BIN="${TAURI_TARGET}/release/shed-desktop-tauri"
CTL="${CORE_TARGET}/release/shedctl"
for b in "${BIN}" "${CTL}"; do
  if [ ! -x "${b}" ]; then
    echo "error: expected binary not found: ${b}" >&2
    exit 1
  fi
done

echo "==> Staging dist/"
rm -rf "${REPO_ROOT}/dist"
mkdir -p "${REPO_ROOT}/dist"
install -m 0755 "${BIN}" "${REPO_ROOT}/dist/shed-desktop"
install -m 0755 "${CTL}" "${REPO_ROOT}/dist/shedctl"

echo "==> nfpm pkg (version=${SHED_DEB_VERSION}, arch=${SHED_DEB_ARCH})"
mkdir -p "${REPO_ROOT}/out"
nfpm pkg --packager deb --config "${REPO_ROOT}/packaging/nfpm.yaml" --target "${REPO_ROOT}/out/"

echo "==> Built:"
ls -1 "${REPO_ROOT}"/out/*.deb
