#!/usr/bin/env bash
#
# install-input-sha.sh <context-dir>
#
# Prints a stable content hash of a docker build context, for injection as
# `--build-arg SHED_INSTALL_SHA=...`. BuildKit's `RUN --mount=type=bind` and
# `COPY` cache keys use a per-file stat cache keyed on (path, size, mtime) — NOT
# content — so a rebuilt binary that collides on size+mtime is silently reused
# from cache, baking a STALE file (#227). Referencing this hash inside the
# install RUN makes a real content change bust that layer's cache key.
#
# It hashes the WHOLE context (every file except the Dockerfile itself, whose
# text BuildKit already folds into each layer's key) rather than a hand-listed
# subset. That is deliberate: the context is exactly the surface those
# cache-vulnerable bind/COPY instructions expose, so the hash can never drift
# out of sync with what they consume — adding a staged file to the Dockerfile
# needs no change here. The output is order-independent (digests are sorted) and
# content-only, so it is stable across checkouts and reruns.
#
# Single source of truth: the rootfs build scripts, the publish CI workflow, and
# publish-images-local.sh all call this rather than re-deriving the hash.
set -euo pipefail

dir="${1:?usage: install-input-sha.sh <context-dir>}"
[ -d "$dir" ] || { echo "install-input-sha.sh: not a directory: $dir" >&2; exit 1; }

# Portable SHA-256: GNU coreutils sha256sum (Linux) or BSD/macOS shasum -a 256.
# Word-split intentionally so "shasum -a 256" expands to argv (SC2086).
if command -v sha256sum >/dev/null 2>&1; then
    SHA_CMD="sha256sum"
else
    SHA_CMD="shasum -a 256"
fi

# Run from inside the context so the paths in the hash are relative — stable
# across checkout locations, and so the hash binds path→content (a digest list
# alone would miss two files swapping contents).
cd "$dir" || exit 1

# Guard against an empty context: with no input and no -r, xargs would hand the
# sha tool an empty arg list and hang reading stdin.
if [ "$(find . -type f ! -name Dockerfile | wc -l)" -eq 0 ]; then
    echo "install-input-sha.sh: no files in context $dir" >&2
    exit 1
fi

# Hash each file ($SHA_CMD emits "<digest>  <relpath>", binding path to
# content), sort the lines for order-independence, then reduce to one digest.
# shellcheck disable=SC2086
find . -type f ! -name Dockerfile -print0 \
    | xargs -0 $SHA_CMD \
    | LC_ALL=C sort \
    | $SHA_CMD \
    | awk '{print $1}'
