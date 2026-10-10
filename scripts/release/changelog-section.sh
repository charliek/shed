#!/usr/bin/env bash
# Print the body of CHANGELOG.md's `## vX.Y.Z — <date>` section: everything
# after that heading up to the next section heading (`## Unreleased` or
# `## v<digit>`), the heading itself excluded
# (plan 026 §3.11). It is the curated release body for a stable tag: the
# publish workflow writes it to a file and hands it to the release (goreleaser's
# `--release-notes`, or `gh release create --notes-file` on a desktop-only tag).
#
# release-plan.sh reads the section through this script too, for its
# `**Ships:**` cross-check, so the section that passed that check is exactly the
# one published.
#
# Usage: changelog-section.sh <tag>   (vX.Y.Z; a leading `v` is optional)
#
# The match is an exact prefix on `## v<version> — ` (an em dash, as
# release-plan.sh requires), so `## Unreleased` and every other heading are
# never read. Only the FIRST matching heading opens the section.
#
# The section ends ONLY at the next real section heading, a line starting
# `## Unreleased` or `## v<digit>` (CHANGELOG.md has no other level-2
# headings). Every other line is body text, a `## ` line included, fenced or
# not, so an upgrade example's `## Upgrade commands` cannot cut the body short.
# The authoring rule this implies (RELEASING.md): a curated section must not
# contain a line starting `## Unreleased` or `## v<digit>`. A second heading
# for the same version therefore ends the section rather than merging.
#
# Exit codes: 0 printed; 1 no such section, or it is empty, or CHANGELOG.md is
# missing; 2 usage (no tag, or not vX.Y.Z[-prerelease]).

set -euo pipefail

TAG="${1:-}"
if [ -z "${TAG}" ]; then
  echo "usage: $0 <tag>   (vX.Y.Z)" >&2
  exit 2
fi
# The same grammar as release-plan.sh. A prerelease is well-formed but has no
# CHANGELOG section, so it fails below as a missing section.
SEMVER_RE='(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[A-Za-z0-9.-]+)?'
if [[ ! "${TAG}" =~ ^v?${SEMVER_RE}$ ]]; then
  echo "usage: $0 <tag>   tag '${TAG}' is not vX.Y.Z[-prerelease]" >&2
  exit 2
fi
V="${TAG#v}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHANGELOG="$(cd "${SCRIPT_DIR}/../.." && pwd)/CHANGELOG.md"
if [ ! -f "${CHANGELOG}" ]; then
  echo "::error::${CHANGELOG} is missing" >&2
  exit 1
fi

# index()==1 anchors an exact prefix match, with no regex-metachar surprises
# from the version's dots; `!seen` opens only the FIRST matching heading.
awk_rc=0
section="$(awk -v head="## v${V} — " '
  index($0, head) == 1 && !seen { seen = 1; insec = 1; next }
  insec && /^## (Unreleased|v[0-9])/ { exit }
  insec { print }
' "${CHANGELOG}")" || awk_rc=$?

if [ "${awk_rc}" -ne 0 ]; then
  echo "::error::reading CHANGELOG.md failed (awk exit ${awk_rc})" >&2
  exit 1
fi
if [ -z "${section}" ]; then
  echo "::error::CHANGELOG.md has no '## v${V} — <date>' section (or it is empty)" >&2
  exit 1
fi

printf '%s\n' "${section}"
