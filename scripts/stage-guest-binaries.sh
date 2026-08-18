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
# linux/<goarch> (CGO disabled; version-stamped via the same -X ldflags the
# Makefile uses, see below), then mirrors guest/extensions/etc/ into
# <context-dir>/ext-etc/.
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

# Version stamping, mirroring the Makefile's LDFLAGS exactly (same three -X
# flags, same git-describe derivation) so the guest binaries report a real
# version instead of internal/version's "dev" default. Works unchanged in
# Version resolution, in caller-priority order:
#   1. $VERSION / $GIT_COMMIT / $BUILD_DATE from the environment — what
#      publish-images.yaml passes, so the guest binaries carry EXACTLY the same
#      stamps as shed-agent/shed-firstboot in the same image (bare "0.8.2", not
#      "v0.8.2" — the two conventions coexisting in one rootfs was a
#      patch-cluster review finding).
#   2. git describe on a dev checkout: "vX.Y.Z-N-g<sha>" (-dirty with edits).
# BUILD_DATE defaults to the COMMIT date, not `date -u`: the staged bytes feed a
# BuildKit bind-mount whose layer cache is keyed on them, so a wall-clock stamp
# would bust the rootfs cache on every local rebuild for zero information.
VERSION="${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo "dev")}"
GIT_COMMIT="${GIT_COMMIT:-$(git rev-parse --short HEAD 2>/dev/null || echo "unknown")}"
BUILD_DATE="${BUILD_DATE:-$(git show -s --format=%cI HEAD 2>/dev/null || echo "unknown")}"
LDFLAGS="-X github.com/charliek/shed/internal/version.Version=$VERSION -X github.com/charliek/shed/internal/version.GitCommit=$GIT_COMMIT -X github.com/charliek/shed/internal/version.BuildDate=$BUILD_DATE"

# Guest extension binaries. CGO disabled so the linux/<goarch> cross-compile
# is static and needs no host toolchain.
for cmd in shed-ext-ssh-agent shed-ext-aws-credentials docker-credential-shed shed-ext-rc; do
    echo "=== Building $cmd (linux/$goarch) ==="
    CGO_ENABLED=0 GOOS=linux GOARCH="$goarch" \
        go build -ldflags "$LDFLAGS" -o "$ctx_dir/$cmd" "./cmd/$cmd"
done

# Guest /etc overlay (systemd units, environment.d, shed-extensions.d configs).
# Delete-and-copy so a file removed upstream doesn't linger in a previously
# staged ext-etc/.
echo "=== Staging guest extensions /etc overlay -> $ctx_dir/ext-etc ==="
rm -rf "$ctx_dir/ext-etc"
mkdir -p "$ctx_dir/ext-etc"
cp -R "$PROJECT_ROOT/guest/extensions/etc/." "$ctx_dir/ext-etc/"
