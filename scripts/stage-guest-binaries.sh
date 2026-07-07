#!/usr/bin/env bash
#
# stage-guest-binaries.sh <context-dir> <goarch>
#
# Stages the shed guest extension binaries + the guest /etc overlay into a
# rootfs Docker build context, so the vz/ and firecracker/ Dockerfiles can
# install them from the context (`--mount=type=bind,source=.,target=/ctx`)
# instead of `COPY --from=` a published ghcr.io/charliek/shed-extensions image.
#
# It cross-compiles the four guest binaries — shed-ext-ssh-agent,
# shed-ext-aws-credentials, docker-credential-shed, shed-ext-rc — for
# linux/<goarch> (CGO disabled, no ldflags, matching how the rootfs scripts
# build shed-agent; the live "version != v0.4.9" assert is satisfied either
# way), then mirrors guest/extensions/etc/ into <context-dir>/ext-etc/.
#
# Single source of truth: the rootfs build scripts, the publish CI workflow,
# and publish-images-local.sh all call this rather than re-deriving the build
# + copy steps, so every rootfs context producer stages identical bytes.
set -euo pipefail

ctx_dir="${1:?usage: stage-guest-binaries.sh <context-dir> <goarch>}"
goarch="${2:?usage: stage-guest-binaries.sh <context-dir> <goarch>}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(dirname "$SCRIPT_DIR")"

# Resolve the context dir to an absolute path relative to the invocation CWD
# BEFORE we cd into the repo root (the module root the `go build`s run from).
case "$ctx_dir" in
    /*) ;;
    *)  ctx_dir="$(pwd)/$ctx_dir" ;;
esac
[ -d "$ctx_dir" ] || { echo "stage-guest-binaries.sh: not a directory: $ctx_dir" >&2; exit 1; }

cd "$PROJECT_ROOT"

# Guest extension binaries. No ldflags (reports "dev"), matching how the rootfs
# scripts build shed-agent; CGO disabled so the linux/<goarch> cross-compile is
# static and needs no host toolchain.
for cmd in shed-ext-ssh-agent shed-ext-aws-credentials docker-credential-shed shed-ext-rc; do
    echo "=== Building $cmd (linux/$goarch) ==="
    CGO_ENABLED=0 GOOS=linux GOARCH="$goarch" \
        go build -o "$ctx_dir/$cmd" "./cmd/$cmd"
done

# Guest /etc overlay (systemd units, environment.d, shed-extensions.d configs).
# Delete-and-copy so a file removed upstream doesn't linger in a previously
# staged ext-etc/.
echo "=== Staging guest extensions /etc overlay -> $ctx_dir/ext-etc ==="
rm -rf "$ctx_dir/ext-etc"
mkdir -p "$ctx_dir/ext-etc"
cp -R "$PROJECT_ROOT/guest/extensions/etc/." "$ctx_dir/ext-etc/"
