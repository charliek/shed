#!/usr/bin/env bash
# Installs the pinned zig on a macOS CI runner, for `cargo zigbuild`
# (goreleaser's `builder: rust` in .goreleaser.host-agent.yaml). Shared by
# ci.yml's release-snapshot job and publish-images.yaml's release job so the
# two can never drift apart.
#
# Why pinned: both jobs used to run a bare `brew install zig`. When Homebrew
# moved zig from 0.16.0 to 0.17.0, the darwin shed-host-agent it linked loaded
# CoreFoundation twice, and the macOS 26 runner's dyld refused to start it
# ("duplicate linked dylib", exit 134). A zig bump is now a reviewed change
# here, tested by release-snapshot before it can reach a tag.
#
# The tarball comes from ziglang.org and is checked against the sha256 that
# https://ziglang.org/download/index.json lists for it. zig's bin directory is
# appended to $GITHUB_PATH, so it is on PATH from the next step on.
#
# Usage: scripts/release/install-zig.sh
set -euo pipefail

ZIG_VERSION=0.16.0

case "$(uname -s)-$(uname -m)" in
  Darwin-arm64)
    target=aarch64-macos
    sha256=b23d70deaa879b5c2d486ed3316f7eaa53e84acf6fc9cc747de152450d401489
    ;;
  *)
    echo "install-zig.sh: no pinned zig for $(uname -s)-$(uname -m) (the release jobs run on macOS arm64)" >&2
    exit 1
    ;;
esac

name="zig-${target}-${ZIG_VERSION}"
dest="${RUNNER_TEMP:?install-zig.sh runs in GitHub Actions}/zig"
mkdir -p "$dest"
curl -fsSL --retry 3 -o "$dest/$name.tar.xz" "https://ziglang.org/download/${ZIG_VERSION}/${name}.tar.xz"
echo "${sha256}  $dest/$name.tar.xz" | shasum -a 256 -c -
tar -xJf "$dest/$name.tar.xz" -C "$dest"
"$dest/$name/zig" version
echo "$dest/$name" >> "$GITHUB_PATH"
