#!/usr/bin/env bash
# Release-time check (plan 025 §3.5, O4): the craze release the shed images
# bake must be a REAL craze tag, and that tag's resolved commit must equal
# the CI test pin — "at a shed release that ships `server`, CRAZE_TEST_SHA
# must equal CRAZE_RELEASE's tag commit" (craze-pin.env's own comment).
#
# Needs network (a real `git ls-remote` against github.com/charliek/craze)
# unless SHED_RELEASE_SELFTEST=1 substitutes CRAZE_PIN_LS_REMOTE for it (see
# the seam below) — release-scripts-test.sh's self-test is otherwise
# hermetic and must never touch the network.
#
# Usage:
#   scripts/release/check-craze-pin.sh --release vX.Y.Z
#
#   vX.Y.Z   the craze release to verify. Normally == craze-pin.env's
#            CRAZE_RELEASE, v-prefixed (release-plan.sh passes exactly that).
#            Taken as an explicit argument, rather than this script reading
#            CRAZE_RELEASE straight out of craze-pin.env itself, so a caller
#            controls precisely what is being checked — including the
#            empty-CRAZE_RELEASE case ("--release v", i.e. the bare version
#            after stripping the leading `v` is empty).
#
# CRAZE_TEST_SHA is always read from craze-pin.env at the repo root (this
# script's own sibling, same as release-plan.sh's).
#
# Exit 2: usage (missing/malformed --release).
# Exit 1: an empty CRAZE_RELEASE (no craze baked yet — see RELEASING.md's
#   "craze pin" section for the consequence and the emergency override); a
#   malformed craze-pin.env; a craze tag that resolves to no commit, or an
#   ambiguous one; or a resolved commit that disagrees with CRAZE_TEST_SHA.
#
# SHED_RELEASE_ALLOW_NO_CRAZE is release-plan.sh's emergency escape (never
# set by CI) — it skips THIS script's invocation entirely, from the caller
# side; this script has no knowledge of it.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
PIN_FILE="${REPO_ROOT}/craze-pin.env"

RELEASE_ARG=""
while [ $# -gt 0 ]; do
  case "$1" in
    --release)
      if [ $# -ge 2 ]; then
        RELEASE_ARG="$2"
        shift 2
      else
        RELEASE_ARG=""
        shift 1
      fi
      ;;
    --release=*)
      RELEASE_ARG="${1#--release=}"
      shift
      ;;
    *)
      echo "usage: $0 --release vX.Y.Z" >&2
      exit 2
      ;;
  esac
done

if [ -z "${RELEASE_ARG}" ]; then
  echo "usage: $0 --release vX.Y.Z" >&2
  exit 2
fi

# Strip exactly one leading 'v'. An empty remainder means an empty
# CRAZE_RELEASE — the normal state before plan 025's bake lands.
RELEASE_V="${RELEASE_ARG#v}"
if [ -z "${RELEASE_V}" ]; then
  echo "::error::craze release is empty (CRAZE_RELEASE not set in craze-pin.env) — no craze has been baked into the images yet. See RELEASING.md's craze-pin section; set SHED_RELEASE_ALLOW_NO_CRAZE=<reason> for an emergency hotfix." >&2
  exit 1
fi
if [[ ! "${RELEASE_V}" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  echo "::error::craze release '${RELEASE_ARG}' is not v<X.Y.Z>" >&2
  exit 1
fi

# craze-pin.env is NEVER sourced as shell (`. "${PIN_FILE}"`) — a pin file
# can carry attacker/mistake-controlled content, and sourcing it would let a
# line like `CRAZE_TEST_SHA=$(cmd)` execute, or let a bare
# `SHED_RELEASE_SELFTEST=1` / `CRAZE_PIN_LS_REMOTE=...` line silently flip
# the seam below. read-craze-pin.sh parses it as DATA and validates before
# printing — see its own header for the full contract.
CRAZE_TEST_SHA="$("${SCRIPT_DIR}/read-craze-pin.sh" --field CRAZE_TEST_SHA "${PIN_FILE}")"

TAG_REF="refs/tags/v${RELEASE_V}"
PEELED_REF="refs/tags/v${RELEASE_V}^{}"

# The seam: SHED_RELEASE_SELFTEST=1 substitutes CRAZE_PIN_LS_REMOTE (a
# command printed exactly as `git ls-remote <url> <refs...>` would, given the
# two refspecs as its arguments) for the real network call, so
# release-scripts-test.sh can exercise match/mismatch/empty without network.
# A stray CRAZE_PIN_LS_REMOTE with no SHED_RELEASE_SELFTEST=1 is IGNORED —
# it must never let a real release run be quietly faked.
ls_remote_output() {
  if [ "${SHED_RELEASE_SELFTEST:-}" = "1" ] && [ -n "${CRAZE_PIN_LS_REMOTE:-}" ]; then
    "${CRAZE_PIN_LS_REMOTE}" "${TAG_REF}" "${PEELED_REF}"
  else
    git ls-remote https://github.com/charliek/craze "${TAG_REF}" "${PEELED_REF}"
  fi
}

OUT="$(ls_remote_output)" || {
  echo "::error::git ls-remote against charliek/craze failed for ${TAG_REF}" >&2
  exit 1
}

# Exact-ref rows only. Prefer the peeled row (^{}, an annotated tag's target
# commit); else the direct row (a lightweight tag already names the commit).
# More than one row for the SAME ref is ambiguous and fails loudly rather
# than guessing; none at all means the tag doesn't exist on the remote.
peeled_count="$(printf '%s\n' "${OUT}" | awk -v r="${PEELED_REF}" '$2==r' | grep -c . || true)"
direct_count="$(printf '%s\n' "${OUT}" | awk -v r="${TAG_REF}" '$2==r' | grep -c . || true)"

if [ "${peeled_count}" -gt 1 ] || [ "${direct_count}" -gt 1 ]; then
  echo "::error::ambiguous ls-remote result for v${RELEASE_V} (multiple rows matched a single ref)" >&2
  exit 1
fi

if [ "${peeled_count}" -eq 1 ]; then
  RESOLVED="$(printf '%s\n' "${OUT}" | awk -v r="${PEELED_REF}" '$2==r {print $1}')"
elif [ "${direct_count}" -eq 1 ]; then
  RESOLVED="$(printf '%s\n' "${OUT}" | awk -v r="${TAG_REF}" '$2==r {print $1}')"
else
  echo "::error::craze tag v${RELEASE_V} not found on charliek/craze (no ${TAG_REF} and no ${PEELED_REF})" >&2
  exit 1
fi

if [ "${RESOLVED}" != "${CRAZE_TEST_SHA}" ]; then
  echo "::error::craze release v${RELEASE_V} resolves to ${RESOLVED}, but CRAZE_TEST_SHA (${PIN_FILE}) is ${CRAZE_TEST_SHA} — the test pin and the baked release have drifted apart." >&2
  exit 1
fi

echo "craze release check OK: v${RELEASE_V} == CRAZE_TEST_SHA (${RESOLVED})"
