#!/usr/bin/env bash
# Recommend which release components a NEXT stable tag should ship.
#
# The monorepo carries four release components on ONE vX.Y.Z tag family
# (see RELEASING.md "Component selection"): server, host-agent, sx, desktop.
# (machine-rc was retired in plan 010 — the shed-host-agent daemon hosts the
# machine RC hub and `sx` carries the one-shot verbs.)
#
# This script inspects committed history and prints a RECOMMENDATION; it never
# bumps a manifest, never writes a file, never mutates git. The human
# reviews the recommendation, edits it if needed, authors the CHANGELOG
# `**Ships:**` line, then runs scripts/release/update-version.sh with the
# confirmed set. This is deliberately advisory:
#
#   * PATCH tags default to "only what changed since each component's last
#     shipped tag".
#   * MINOR / MAJOR tags default to "everything" (a version-line bump ships the
#     whole fleet).
#
# Requirements / caveats:
#   * FULL CLONE — the walk reads component manifests at historical tags
#     (`git show <tag>:<manifest>`) and diffs `<lastship>..HEAD`. A shallow
#     clone (CI `actions/checkout` default) can't see the history; the script
#     hard-errors with an `--unshallow` hint.
#   * FAILED-TAG CAVEAT — "last shipped" is the newest tag whose manifest equals
#     the tag. A tag whose release job FAILED still counts as the basis. When
#     `gh` is available and authed the script verifies the chosen tag's release
#     actually has assets and warns if not; otherwise it prints a caveat so the
#     human can double-check. Set RECOMMEND_NO_GH=1 to skip the gh probe.
#   * The recommendation is a STARTING POINT — the human confirms/edits it. The
#     path sets are deliberately coarse (over-recommending is free under human
#     confirm); the one known under-flag is called out at the end.
#
# Input:  the target version, `X.Y.Z` (no `v`, stable only), as $1.
# Output:
#   * STDERR — a human-readable table (component | last-shipped | changed? |
#     sample dirtying paths), the derived bump level, and any caveats.
#   * STDOUT — exactly two machine-parseable lines:
#         level=patch|minor|major
#         recommended_components=<comma-list>   (empty if a patch changed nothing)
#
# Hard failures (exit 1): shallow clone; target not strictly greater than the
# max current manifest version (the tag family is monotonic — "never reuse a
# version" — which makes the bump-level compare well-defined); no historical
# basis for a component that is NOT in NEVER_SHIPPED (see below). exit 2: a
# malformed / prerelease target argument.

set -euo pipefail

# ===========================================================================
# PATH SETS — component → the pathspecs whose changes imply that component
# should re-ship. Passed verbatim to `git diff --name-only <lastship>..HEAD --`.
# `:(exclude)…` entries are git pathspec magic (subtract a subtree). These are
# intentionally coarse: over-recommending is harmless under the recommend+
# confirm flow; a false NEGATIVE (an unflagged real change) is the costly one.
# Kept in lockstep with plan 002 §3 D4.
#
# (Each array is consumed indirectly through the get_paths() case dispatch keyed
# on the component name — bash 3.2 has no namerefs, and this script runs LOCALLY
# on macOS stock bash 3.2 — so shellcheck can't see the use; SC2034 disabled.)
# ===========================================================================
# shellcheck disable=SC2034  # PATHS_* are read via get_paths()

# server: the Go binaries + plugin + rootfs image inputs. `sdk/**` is a
# path-replaced local module baked into shipped binaries with no go.sum trace;
# `.claude-plugin/**` is the ship selector; `skills/**` ships in the plugin.
# The two helper cmd/ subtrees (their own components) are subtracted.
PATHS_SERVER=(
  cmd
  # cmd/shed-machine-rc was deleted from the tree (plan 010 H15 — the
  # machine-rc retirement), but the exclusion stays: it's needed so diffs
  # against PRE-DELETION tags still classify correctly. Prune once every
  # plausible basis tag is post-deletion.
  ':(exclude)cmd/shed-machine-rc'
  # cmd/shed-host-agent was deleted from the tree (plan 006 C3 — the Go
  # host-agent sunset), but the exclusion stays: it's needed so diffs against
  # PRE-DELETION tags still classify correctly. Prune after the next
  # host-agent release makes every future basis tag post-deletion.
  ':(exclude)cmd/shed-host-agent'
  internal
  sdk
  guest
  vz
  firecracker
  build-tools
  packaging
  .claude-plugin
  skills
  go.mod
  go.sum
  'scripts/build-*-rootfs.sh'
  scripts/build-initramfs.sh
  scripts/stage-guest-binaries.sh
  scripts/install-input-sha.sh
  .goreleaser.server.yaml
)

# host-agent: the Rust broker + the Go rollback binaries.
# DELIBERATELY EXCLUDES crates/Cargo.toml + crates/Cargo.lock: every desktop
# bump rewrites those (workspace version + lock regen), which would flag
# host-agent after every desktop release. Accepted gap: a
# [workspace.dependencies]-only bump to crates/Cargo.toml won't auto-flag
# host-agent (rare; the human confirm is the backstop; noted in the output).
# shellcheck disable=SC2034  # read via get_paths()
PATHS_HOST_AGENT=(
  crates/shed-host-agent
  crates/shed-broker
  # shed-broker depends on shed-rc-engine from plan 010 H4 (the rc_hub port),
  # so an engine-only change now reaches the shipped host-agent binary.
  crates/shed-rc-engine
  crates/shed-core
  crates/rust-toolchain.toml
  configs/extensions.example.yaml
  # cmd/shed-host-agent was deleted from the tree (plan 006 C3 — the Go
  # host-agent sunset), but the entry stays: it's needed so diffs against
  # PRE-DELETION tags still classify correctly. Prune after the next
  # host-agent release makes every future basis tag post-deletion.
  cmd/shed-host-agent
  .goreleaser.host-agent.yaml
)

# sx: the RC porcelain binary + every crate it links. Same exclusion rationale
# as host-agent: crates/Cargo.toml + crates/Cargo.lock are DELIBERATELY absent
# (every desktop bump rewrites them, which would flag sx after every desktop
# release). Accepted gap: a [workspace.dependencies]-only bump won't auto-flag
# sx either — the human confirm is the backstop.
# shellcheck disable=SC2034  # read via get_paths()
PATHS_SX=(
  crates/sx
  crates/shed-app
  crates/shed-rc-engine
  crates/shed-core
  crates/rust-toolchain.toml
  .goreleaser.sx.yaml
)

# desktop: the app + every crate it links + the shared cargo manifests/locks
# (which the desktop bump owns).
# shellcheck disable=SC2034  # read via get_paths()
PATHS_DESKTOP=(
  desktop
  crates/shed-core
  crates/shed-app
  # The Tauri client compiles the engine via shed-app's `rc` feature — an
  # engine-only source edit must flag desktop or the .deb silently ships stale.
  crates/shed-rc-engine
  crates/shed-core-ffi
  crates/shedctl
  crates/shed-broker
  crates/Cargo.toml
  crates/Cargo.lock
  crates/rust-toolchain.toml
)

# Canonical component order (drives every output list).
COMPONENTS=(server host-agent sx desktop)

# Components that have NEVER shipped in any tag — the first-ship bootstrap.
#
# find_lastship() walks tags for one whose manifest equals the tag; failing
# that, the caller hard-errors "no historical basis". That error is the right
# behavior for an ESTABLISHED component (it means the repo/tag state is broken)
# but it is the WRONG behavior for a brand-new one, which by definition has no
# basis and no pre-migration era to fall back to either.
#
# A component listed here is instead reported with the basis "(never shipped)",
# treated as changed=yes unconditionally (there is no <lastship>..HEAD range to
# diff), and therefore recommended at every level including patch — which is
# exactly right: a component that has never shipped always wants shipping on the
# next tag. Anything NOT listed keeps the hard error, so the sentinel can never
# silently mask a genuine repo-state bug.
#
# Self-healing: once a tag ships the component, loop (a) of find_lastship finds
# it and this list is never consulted again. PRUNE the entry then.
#
# sx (plan 011) is listed because it ships for the first time on whatever tag
# next carries crates/sx/VERSION; the file is seeded at 0.0.0, a version no tag
# will ever carry, so the walk correctly finds nothing.
NEVER_SHIPPED=(sx)

# True (exit 0) iff $1 is in NEVER_SHIPPED. Index-loop the scan (rather than
# `for c in "${NEVER_SHIPPED[@]}"`) so that pruning the LAST entry — which the
# comment above tells the next person to do — leaves an empty array that can't
# trip `set -u` on the `[@]` expansion under stock macOS bash 3.2. Same guard
# release-plan.sh applies to its actual_ships scan.
never_shipped() {
  local i=0
  while [ "${i}" -lt "${#NEVER_SHIPPED[@]}" ]; do
    [ "${NEVER_SHIPPED[${i}]}" = "$1" ] && return 0
    i=$((i + 1))
  done
  return 1
}

# ---------------------------------------------------------------------------
# Argument + repo preconditions.
# ---------------------------------------------------------------------------
TARGET="${1:-}"
if [ -z "${TARGET}" ]; then
  echo "usage: $0 <X.Y.Z>   (stable target version, no 'v' prefix)" >&2
  exit 2
fi
# Semver fields reject leading zeros ((0|[1-9][0-9]*)) so a value like 0.07.11 is
# a malformed argument, not a version bash arithmetic could later misparse as octal.
if [[ "${TARGET}" =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)-[A-Za-z0-9.-]+$ ]]; then
  # A prerelease is a well-formed version that fails a precondition (the
  # recommendation is for stable releases), so it's a hard failure (exit 1),
  # not a usage error (exit 2).
  echo "error: '${TARGET}' is a prerelease — recommend-components is for STABLE releases only." >&2
  echo "       (Only the desktop component has a beta channel; bump its surfaces directly for an rc.)" >&2
  exit 1
fi
if [[ ! "${TARGET}" =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]]; then
  echo "usage: $0 <X.Y.Z>   '${TARGET}' is not a stable semver version (no leading zeros)" >&2
  exit 2
fi

# Resolve the repo from the CURRENT working directory (not the script's own
# path) so the script operates on whatever repo it is invoked inside — the real
# repo when run as scripts/release/recommend-components.sh from the root, and a
# throwaway fixture repo when driven by release-scripts-test.sh.
REPO_ROOT="$(git rev-parse --show-toplevel 2>/dev/null)" || {
  echo "error: not inside a git repository (recommend-components reads tag history)." >&2
  exit 1
}

if [ "$(git -C "${REPO_ROOT}" rev-parse --is-shallow-repository 2>/dev/null)" = "true" ]; then
  echo "error: this is a SHALLOW clone — recommend-components needs full tag history." >&2
  echo "       Run: git fetch --unshallow --tags   (or clone with fetch-depth: 0)" >&2
  exit 1
fi

if [ -n "$(git -C "${REPO_ROOT}" status --porcelain 2>/dev/null)" ]; then
  echo "WARNING: working tree is dirty — the diff basis is committed HEAD, so" >&2
  echo "         uncommitted changes are NOT reflected in the recommendation." >&2
fi

# ---------------------------------------------------------------------------
# Helpers.
# ---------------------------------------------------------------------------

# semver_cmp A B → echoes -1 (A<B), 0 (A==B), 1 (A>B). Compares the X.Y.Z core
# numerically (each field forced base-10 so a leading zero can't be read as
# octal); on an equal core a STABLE version dominates any prerelease of the same
# base (SemVer: 1.0.0 > 1.0.0-rc.1). This matters for the monotonicity guard: a
# desktop/VERSION legitimately holds an rc after a Tauri rc rehearsal, and the
# stable promotion targets the same base — it must be seen as strictly greater.
semver_cmp() {
  local a="${1%%-*}" b="${2%%-*}" a1 a2 a3 b1 b2 b3
  IFS=. read -r a1 a2 a3 <<< "${a}"
  IFS=. read -r b1 b2 b3 <<< "${b}"
  local x l r
  for x in "${a1}:${b1}" "${a2}:${b2}" "${a3}:${b3}"; do
    l="${x%%:*}"; r="${x##*:}"
    if (( 10#${l:-0} > 10#${r:-0} )); then echo 1; return; fi
    if (( 10#${l:-0} < 10#${r:-0} )); then echo -1; return; fi
  done
  # Cores equal — a stable version (no `-suffix`) outranks a prerelease of the
  # same base; two stables or two prereleases of the same base compare equal.
  local a_pre=0 b_pre=0
  case "$1" in *-*) a_pre=1 ;; esac
  case "$2" in *-*) b_pre=1 ;; esac
  if (( a_pre == 0 && b_pre == 1 )); then echo 1; return; fi
  if (( a_pre == 1 && b_pre == 0 )); then echo -1; return; fi
  echo 0
}

# The manifest path for a component (relative to REPO_ROOT).
manifest_path() {
  case "$1" in
    server) echo ".claude-plugin/plugin.json" ;;
    host-agent) echo "crates/shed-host-agent/VERSION" ;;
    sx) echo "crates/sx/VERSION" ;;
    desktop) echo "desktop/VERSION" ;;
  esac
}

# Populate the global `paths` indexed array with a component's pathspecs. Bash
# 3.2 has no namerefs / associative arrays, so this is a plain case dispatch
# (fully reassigning `paths` each call — no stale accumulation).
get_paths() {
  case "$1" in
    server)     paths=("${PATHS_SERVER[@]}") ;;
    host-agent) paths=("${PATHS_HOST_AGENT[@]}") ;;
    sx)         paths=("${PATHS_SX[@]}") ;;
    desktop)    paths=("${PATHS_DESKTOP[@]}") ;;
  esac
}

# The 0-based index of a component in COMPONENTS (its slot in the parallel
# LASTSHIP / CHANGED / SAMPLES indexed arrays — bash-3.2's stand-in for the
# associative arrays this walk would otherwise key on the component name).
#
# DERIVED from COMPONENTS rather than a hand-maintained parallel case map: a
# hardcoded map means adding a component is two edits (append to COMPONENTS,
# and renumber every later entry here), and getting the second one wrong
# misindexes silently. Deriving it makes COMPONENTS the single source of truth.
#
# The trailing hard error is load-bearing, not defensive noise: falling off the
# end would echo the EMPTY STRING, and bash 3.2 evaluates an empty array
# subscript as 0 rather than erroring — so `LASTSHIP[${ci}]=…` would silently
# write into the `server` slot and the unknown component would vanish from the
# report with no diagnostic. (That is the same silent-misindex class the
# hardcoded map had; deriving the index alone only moves it.) A future
# component added to manifest_path()/get_paths() but forgotten in COMPONENTS
# now fails loudly here instead.
comp_index() {
  local i=0
  while [ "${i}" -lt "${#COMPONENTS[@]}" ]; do
    [ "${COMPONENTS[${i}]}" = "$1" ] && { echo "${i}"; return; }
    i=$((i + 1))
  done
  echo "::error::comp_index: '$1' is not in COMPONENTS (${COMPONENTS[*]}) — add it there, or the parallel LASTSHIP/CHANGED/SAMPLES arrays would silently misindex." >&2
  exit 1
}

# Current manifest version from the working tree; empty if the file is absent
# (the host-agent VERSION file doesn't exist pre-migration).
current_manifest() {
  local comp="$1" path
  path="${REPO_ROOT}/$(manifest_path "${comp}")"
  [ -f "${path}" ] || { echo ""; return; }
  if [ "${comp}" = "server" ]; then
    jq -r '.version' "${path}" 2>/dev/null || echo ""
  else
    tr -d '[:space:]' < "${path}"
  fi
}

# The component's manifest version AT tag T; empty if the file didn't exist at T
# (a legitimate pre-migration state). server = jq .version of plugin.json;
# others = VERSION file. Existence is probed with `git cat-file -e` so an ABSENT
# path (→ "") is cleanly distinguished from a `git show` that FAILS on a path
# that DOES exist (a corrupt-object / repo bug) — the latter is a hard error
# rather than a silent empty that would be misread as pre-migration.
manifest_at_tag() {
  local comp="$1" t="$2" path blob
  path="$(manifest_path "${comp}")"
  if ! git -C "${REPO_ROOT}" cat-file -e "${t}:${path}" 2>/dev/null; then
    echo ""; return 0    # path absent at T — pre-migration; legitimately empty
  fi
  blob="$(git -C "${REPO_ROOT}" show "${t}:${path}" 2>/dev/null)" || {
    echo "::error::git show ${t}:${path} failed though the path exists at ${t} (corrupt object?)." >&2
    exit 1
  }
  if [ "${comp}" = "server" ]; then
    printf '%s' "${blob}" | jq -r '.version' 2>/dev/null || echo ""
  else
    printf '%s' "${blob}" | tr -d '[:space:]'
  fi
}

# True (exit 0) iff a component's VERSION manifest exists at tag T. Used to gate
# the pre-migration fallback: the fallback only applies where the component had
# NO standalone selector yet (VERSION absent). server has no VERSION file.
manifest_file_exists_at_tag() {
  local comp="$1" t="$2" path
  path="$(manifest_path "${comp}")"
  git -C "${REPO_ROOT}" cat-file -e "${t}:${path}" 2>/dev/null
}

# ---------------------------------------------------------------------------
# The candidate tags: `git tag -l 'v*'` (this glob EXCLUDES the `sdk/v*` module
# tags), stable only (skip anything with a `-`), sorted semver-descending.
# ---------------------------------------------------------------------------
TAGS=()
while IFS= read -r t; do
  [ -n "${t}" ] || continue
  case "${t}" in *-*) continue ;; esac   # skip prereleases
  TAGS+=("${t}")
done < <(git -C "${REPO_ROOT}" tag -l 'v*' | sort -V -r)

# find_lastship <comp> → echoes the newest tag T that SHIPPED the component.
# A tag T ships host-agent iff EITHER:
#   (a) its VERSION file exists at T and equals T's version (post-migration, its
#       own selector), OR
#   (b) its VERSION file does NOT exist at T (pre-migration era) AND plugin.json
#       at T equals T's version (it rode the old `go`/server component then).
# The (b) fallback MUST require the VERSION file to be absent: a post-migration
# tag that carries the file but shipped server-only (VERSION stale, plugin==tag)
# did NOT ship the component, so it must not be borrowed as the basis — doing so
# would narrow the diff and hide real changes. Returns 1 if there is no basis.
find_lastship() {
  local comp="$1" t vt got
  for t in "${TAGS[@]}"; do
    vt="${t#v}"
    got="$(manifest_at_tag "${comp}" "${t}")"
    if [ -n "${got}" ] && [ "${got}" = "${vt}" ]; then
      echo "${t}"; return 0
    fi
  done
  if [ "${comp}" = "host-agent" ]; then
    for t in "${TAGS[@]}"; do
      # Fallback is pre-migration ONLY: skip any tag where the component already
      # had its own VERSION selector (its ship there is decided by loop (a)).
      if manifest_file_exists_at_tag "${comp}" "${t}"; then
        continue
      fi
      vt="${t#v}"
      got="$(manifest_at_tag server "${t}")"
      if [ -n "${got}" ] && [ "${got}" = "${vt}" ]; then
        echo "${t}"; return 0
      fi
    done
  fi
  return 1
}

# ---------------------------------------------------------------------------
# gh-based failed-tag check, cached per tag. Never a hard failure: gh missing /
# unauthed / api-error (e.g. a fixture repo with no remote) → "skip" (caveat).
# ---------------------------------------------------------------------------
# Bash-3.2 cache: parallel indexed arrays + linear scan (no associative arrays).
# Only the handful of chosen basis tags are ever probed, so the scan is trivial.
GH_NOTE_KEYS=()
GH_NOTE_VALS=()
gh_note_for_tag() {
  local t="$1" i=0 result="skip" n
  while [ "${i}" -lt "${#GH_NOTE_KEYS[@]}" ]; do
    if [ "${GH_NOTE_KEYS[${i}]}" = "${t}" ]; then echo "${GH_NOTE_VALS[${i}]}"; return; fi
    i=$((i + 1))
  done
  if [ -z "${RECOMMEND_NO_GH:-}" ] && command -v gh >/dev/null 2>&1 && gh auth status >/dev/null 2>&1; then
    n="$(gh release view "${t}" --json assets --jq '.assets | length' 2>/dev/null || true)"
    if [ -z "${n}" ]; then
      result="skip"          # no such release / api error
    elif [ "${n}" = "0" ]; then
      result="warn"          # release exists but shipped no assets (failed tag)
    else
      result="ok"
    fi
  fi
  GH_NOTE_KEYS+=("${t}")
  GH_NOTE_VALS+=("${result}")
  echo "${result}"
}

# ---------------------------------------------------------------------------
# Max current manifest version (over the manifests that EXIST) → bump level.
# ---------------------------------------------------------------------------
MAX_VER=""
for comp in "${COMPONENTS[@]}"; do
  v="$(current_manifest "${comp}")"
  [ -n "${v}" ] || continue
  if [ -z "${MAX_VER}" ] || [ "$(semver_cmp "${v}" "${MAX_VER}")" = "1" ]; then
    MAX_VER="${v}"
  fi
done
if [ -z "${MAX_VER}" ]; then
  echo "error: no component manifest found — cannot determine the current version." >&2
  exit 1
fi

if [ "$(semver_cmp "${TARGET}" "${MAX_VER}")" != "1" ]; then
  echo "error: target ${TARGET} is not greater than the max current manifest version ${MAX_VER}." >&2
  echo "       The vX.Y.Z tag family is monotonic (never reuse a version); pick a higher target." >&2
  exit 1
fi

IFS=. read -r tmaj tmin _ <<< "${TARGET}"
IFS=. read -r mmaj mmin _ <<< "${MAX_VER%%-*}"
# Force base-10: MAX_VER comes from a manifest that isn't leading-zero-validated,
# so a stray 0-prefixed field must not be read as octal.
if (( 10#${tmaj:-0} > 10#${mmaj:-0} )); then
  LEVEL="major"
elif (( 10#${tmin:-0} > 10#${mmin:-0} )); then
  LEVEL="minor"
else
  LEVEL="patch"
fi

# ---------------------------------------------------------------------------
# Per-component walk: last-shipped tag, changed?, samples. Bash 3.2 has no
# associative arrays, so LASTSHIP / CHANGED / SAMPLES are plain indexed arrays
# parallel to COMPONENTS (indexed by comp_index).
# ---------------------------------------------------------------------------
LASTSHIP=()
CHANGED=()
SAMPLES=()
GH_CAVEAT=false

FIRST_SHIP=false

for comp in "${COMPONENTS[@]}"; do
  ci="$(comp_index "${comp}")"
  if ! lastship="$(find_lastship "${comp}")"; then
    # No basis. For a NEVER_SHIPPED component that is expected (see the list's
    # comment): report it as a first ship and always recommend it. For anything
    # else it is a repo/tag-state bug and must stay loud.
    if ! never_shipped "${comp}"; then
      echo "error: no historical basis for component '${comp}' (no tag matches its manifest, no fallback)." >&2
      exit 1
    fi
    LASTSHIP[${ci}]="(never shipped)"
    CHANGED[${ci}]=yes
    SAMPLES[${ci}]="(first ship — no diff basis)"
    FIRST_SHIP=true
    continue
  fi
  LASTSHIP[${ci}]="${lastship}"

  # Failed-tag / caveat probe for the chosen basis tag.
  note="$(gh_note_for_tag "${lastship}")"
  case "${note}" in
    warn) echo "WARNING: last-shipped tag ${lastship} for ${comp} has a release with NO assets — it may be a FAILED tag; verify before treating it as the diff basis." >&2 ;;
    skip) GH_CAVEAT=true ;;
  esac

  get_paths "${comp}"
  files="$(git -C "${REPO_ROOT}" diff --name-only "${lastship}..HEAD" -- "${paths[@]}")"
  if [ -n "${files}" ]; then
    CHANGED[${ci}]=yes
    # awk (not `head`) collects the first 5 samples: it reads all of printf's
    # output rather than closing the pipe early, so it can't raise SIGPIPE →
    # exit 141 under `set -o pipefail` on a very large change set.
    SAMPLES[${ci}]="$(printf '%s\n' "${files}" | awk 'NR<=5{printf "%s%s", (NR>1?",":""), $0}')"
  else
    CHANGED[${ci}]=no
    SAMPLES[${ci}]="-"
  fi
done

# ---------------------------------------------------------------------------
# Derive the recommendation.
#   minor/major → all components.
#   patch       → the changed components (canonical order).
# ---------------------------------------------------------------------------
recommended=()
for comp in "${COMPONENTS[@]}"; do
  if [ "${LEVEL}" = "patch" ]; then
    [ "${CHANGED[$(comp_index "${comp}")]}" = "yes" ] && recommended+=("${comp}")
  else
    recommended+=("${comp}")
  fi
done

# ---------------------------------------------------------------------------
# Human-readable report → STDERR.
# ---------------------------------------------------------------------------
{
  echo
  echo "recommend-components: target ${TARGET}  (max current manifest ${MAX_VER} → level: ${LEVEL})"
  printf '  %-11s  %-15s  %-8s  %s\n' "component" "last-shipped" "changed?" "sample paths"
  printf '  %-11s  %-15s  %-8s  %s\n' "---------" "------------" "--------" "------------"
  for comp in "${COMPONENTS[@]}"; do
    ci="$(comp_index "${comp}")"
    printf '  %-11s  %-15s  %-8s  %s\n' "${comp}" "${LASTSHIP[${ci}]}" "${CHANGED[${ci}]}" "${SAMPLES[${ci}]}"
  done
  echo
  if [ "${LEVEL}" = "patch" ]; then
    echo "  patch → recommending only the CHANGED components."
  else
    echo "  ${LEVEL} → recommending ALL components (a version-line bump ships the fleet)."
  fi
  echo "  CAVEAT (host-agent, sx): a [workspace.dependencies]-only bump in crates/Cargo.toml is"
  echo "         NOT auto-detected (that file is excluded to avoid desktop-bump false positives)."
  if [ "${FIRST_SHIP}" = "true" ]; then
    echo "  NOTE (first ship): a component shown as '(never shipped)' has no diff basis — it is"
    echo "         recommended unconditionally. Prune it from NEVER_SHIPPED once it has shipped once."
  fi
  if [ "${GH_CAVEAT}" = "true" ]; then
    echo "  CAVEAT (failed-tag): gh unavailable/unauthed — could not verify the chosen basis"
    echo "         tags actually shipped assets. A failed tag would still count as 'last shipped'."
  fi
  echo
} >&2

# ---------------------------------------------------------------------------
# Machine-parseable conclusion → STDOUT (exactly two lines).
# ---------------------------------------------------------------------------
if [ "${LEVEL}" = "patch" ] && [ "${#recommended[@]}" -eq 0 ]; then
  echo "WARNING: patch target ${TARGET} but NO component changed since its last shipped tag." >&2
  echo "         Nothing to release — re-check the target or the pending work." >&2
fi

echo "level=${LEVEL}"
if [ "${#recommended[@]}" -gt 0 ]; then
  printf 'recommended_components=%s\n' "$(IFS=,; echo "${recommended[*]}")"
else
  echo "recommended_components="
fi
