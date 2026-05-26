#!/usr/bin/env bash
# Set the plugin version in .claude-plugin/plugin.json (read by Claude Code at
# install/update time). The Go binaries' version comes from the git tag via
# GoReleaser ldflags, so this is the only committed file that hardcodes a version.
# Usage: scripts/set-version.sh 0.4.4
set -euo pipefail

version="${1:?usage: set-version.sh <X.Y.Z>}"
root="$(cd "$(dirname "$0")/.." && pwd)"
plugin="$root/.claude-plugin/plugin.json"

tmp="$(mktemp)"
sed -E 's/("version"[[:space:]]*:[[:space:]]*")[^"]*"/\1'"$version"'"/' "$plugin" > "$tmp"
mv "$tmp" "$plugin"

echo "plugin version set to $version"
