#!/usr/bin/env bash
# Bump shed's release version.
#
# shed has TWO version surfaces:
#
#   * Go binaries (shed, shed-server, shed-agent) — version comes from
#     a build-time ldflag injected by GoReleaser at tag time:
#         -X github.com/charliek/shed/internal/version.Version={{.Version}}
#         -X github.com/charliek/shed/internal/version.GitCommit={{.ShortCommit}}
#         -X github.com/charliek/shed/internal/version.BuildDate={{.Date}}
#     NOT bumped in the source tree.
#
#   * Claude Code plugin (.claude-plugin/plugin.json) — lives in source.
#     Read by Claude Code at install/update time. IS a source-tree
#     manifest that must be bumped to match the released tag.
#
# This script bumps plugin.json by delegating to scripts/set-version.sh
# (the existing, tested in-repo bumper) and then jq-verifies the bump
# landed. The convention requires the script to live at
# scripts/release/update-version.sh; this thin wrapper preserves the
# existing scripts/set-version.sh entry point.
#
# Pre-migration shape: plugin.json was bumped by CI's `sync-version`
# job AFTER the tag was pushed (the "inverse pattern"). post-migration
# shape: plugin.json is bumped HERE, before tagging, by the
# /release-workflows:release skill. The sync-version job is removed in
# the same commit that adds this script.
#
# Contract (see cc-plugins:release-workflows references/update-version/README.md):
#   - one arg: semver string, no `v` prefix
#   - idempotent
#   - no network
#   - verifies its own work — explicit jq-back on plugin.json after
#     delegating, because set-version.sh's sed substitution silently
#     no-ops if the version field is missing or malformed
#   - doesn't `git add` (release skill stages + commits)

set -euo pipefail

if [[ $# -ne 1 ]]; then
  echo "usage: $0 <X.Y.Z>   e.g. $0 0.5.9" >&2
  exit 2
fi
V="$1"

if [[ ! "$V" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[a-zA-Z0-9.-]+)?$ ]]; then
  echo "error: '$V' is not semver (X.Y.Z or X.Y.Z-suffix)" >&2
  exit 2
fi

PLUGIN_JSON="$(dirname "$0")/../../.claude-plugin/plugin.json"

# Delegate to the existing plugin.json bumper.
"$(dirname "$0")/../set-version.sh" "$V"

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
