#!/usr/bin/env bash
# Self-test for the release-model scripts:
#
#   scripts/release/update-version.sh        (component-selecting version bumper)
#   scripts/release/release-plan.sh          (tag → per-component ship selector)
#   scripts/release/recommend-components.sh  (next-tag component recommender)
#
# Runs every synthetic combination the release model depends on — per-component
# (server / host-agent / machine-rc / desktop) and combined plans / neither→
# exit 1 / lockstep-broken→exit 1 / malformed-manifest→exit 1 / prerelease×
# goreleaser-component→exit 1 / desktop-only-prerelease→pass / the `**Ships:**`
# CHANGELOG cross-check (legacy alias, unknown token, duplicate, mismatch,
# missing entry, Unreleased-skipping) — plus the update-version.sh contract
# asserts (unknown component errors + valid-set listing; `--components server`
# leaves desktop manifests untouched; `--components desktop` bumps all four
# desktop surfaces + both locks; per-component host-agent / machine-rc bumps;
# the 4-way combined bump; `go` alias warns on stderr; prerelease rejected for
# goreleaser components but allowed desktop-only; idempotent re-run).
#
# recommend-components.sh is exercised against a SEPARATE throwaway git repo
# (built below) with synthetic tagged history — proving the last-shipped walk,
# the pre-migration VERSION-absence fallback, desktop-bump Cargo churn NOT
# flagging host-agent, sdk/plugin changes flagging server, the bump-level
# derivation, and the shallow / target≤max / prerelease preconditions.
#
# The whole thing runs in a TEMP COPY of the relevant files (scripts +
# manifests + both cargo workspaces, minus target/) so it never dirties the
# repo. Needs: bash, jq, cargo (the lock regens run `cargo update --offline`),
# git (the recommender fixture). The recommender's `gh` probe is forced off in
# the fixture (RECOMMEND_NO_GH=1) — synthetic tags have no GitHub releases.
#
# Wired into CI as a step of ci.yml's `plugin` job (gated go||ci; the `ci`
# path filter includes scripts/release/**, so editing these scripts always
# runs this test).

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

if ! command -v cargo >/dev/null 2>&1 && [ -f "${HOME}/.cargo/env" ]; then
  # shellcheck disable=SC1091
  . "${HOME}/.cargo/env"
fi
command -v cargo >/dev/null 2>&1 || { echo "error: cargo not found — the desktop bump regenerates Cargo locks" >&2; exit 1; }
command -v jq >/dev/null 2>&1 || { echo "error: jq not found" >&2; exit 1; }
command -v git >/dev/null 2>&1 || { echo "error: git not found — the recommender fixture builds a temp repo" >&2; exit 1; }

SCRATCH="$(mktemp -d -t shed-release-scripts-test.XXXXXX)"
trap 'rm -rf "${SCRATCH}"' EXIT

# Copy the exact file set the two scripts operate on, preserving relative
# layout (both scripts locate the repo root from their own path, so the
# scratch copies operate purely inside $SCRATCH). target/ dirs are excluded
# — cargo only needs manifests + sources to resolve the workspaces.
tar -C "${REPO_ROOT}" -cf - \
  --exclude 'crates/target' \
  --exclude 'desktop/tauri/src-tauri/target' \
  scripts/set-version.sh \
  scripts/release/update-version.sh \
  scripts/release/release-plan.sh \
  scripts/release/recommend-components.sh \
  .claude-plugin/plugin.json \
  desktop/VERSION \
  crates/shed-host-agent/VERSION \
  cmd/shed-machine-rc/VERSION \
  CHANGELOG.md \
  crates \
  desktop/tauri/src-tauri \
  | tar -C "${SCRATCH}" -xf -

UV="${SCRATCH}/scripts/release/update-version.sh"
RP="${SCRATCH}/scripts/release/release-plan.sh"
RECO="${SCRATCH}/scripts/release/recommend-components.sh"

# Prime the cargo registry index for both scratch workspaces. update-version.sh
# runs `cargo update --offline` (the script's no-network contract), and offline
# resolution needs the crates.io index entries for the locked deps in the local
# cargo cache — a fresh CI runner has none. A `--dry-run` update (no --offline)
# fetches just the sparse-index metadata (~seconds, no crate downloads) and
# leaves both locks untouched. On a warm dev machine this is a near-no-op.
(cd "${SCRATCH}/crates" && cargo update --workspace --dry-run -q)
(cd "${SCRATCH}/desktop/tauri/src-tauri" && cargo update --workspace --dry-run -q)

# Hermetic baseline. The tar copy above imports the REAL component manifests at
# whatever version the repo currently sits at, so a shipped version can leak into
# a synthetic case: once v0.8.0 shipped, crates/shed-host-agent/VERSION and
# cmd/shed-machine-rc/VERSION both read 0.8.0, and the desktop-only `v0.8.0` case
# below then matched THREE components instead of one (issue #280). Reset all four
# selectors to a sentinel equal to NONE of the tags this test uses, so every
# case's residual (unbumped) manifest cannot collide with that case's tag — the
# test fully owns version state regardless of the repo's current release. (Uses
# update-version.sh's desktop arm, so it needs the primed cargo index above.)
BASELINE=0.0.0
"${UV}" "${BASELINE}" --components server,host-agent,machine-rc,desktop >/dev/null

PASS=0
step() { echo "--- $*"; }
ok()   { PASS=$((PASS + 1)); echo "    ok: $*"; }
fail() { echo "FAIL: $*" >&2; exit 1; }

# sha256 over the desktop version surfaces, to prove (non-)modification.
desktop_state() {
  cat "${SCRATCH}/desktop/VERSION" \
      "${SCRATCH}/crates/Cargo.toml" "${SCRATCH}/crates/Cargo.lock" \
      "${SCRATCH}/desktop/tauri/src-tauri/Cargo.toml" \
      "${SCRATCH}/desktop/tauri/src-tauri/tauri.conf.json" \
      "${SCRATCH}/desktop/tauri/src-tauri/Cargo.lock" \
    | shasum -a 256 | awk '{print $1}'
}

# run_plan <tag> → stdout captured to $PLAN_OUT, exit status to $PLAN_RC.
PLAN_OUT=""
PLAN_RC=0
run_plan() {
  PLAN_RC=0
  PLAN_OUT="$("${RP}" "$@" 2>/dev/null)" || PLAN_RC=$?
}

# The exact 5-line stdout release-plan.sh emits, in emission order. Building the
# expectation from named booleans keeps the assertions readable as the ship set
# changes case-to-case.
expect_plan() {
  # $1 server, $2 host-agent, $3 machine-rc, $4 desktop, $5 goreleaser
  printf 'ship_server=%s\nship_host_agent=%s\nship_machine_rc=%s\nship_desktop=%s\nship_goreleaser=%s' \
    "$1" "$2" "$3" "$4" "$5"
}

# Write a synthetic scratch CHANGELOG.md for the version under test. The tag's
# `## v<ver> — <date>` section carries the given `**Ships:**` tokens; a decoy
# `## Unreleased` section carries a full-set Ships line that would FAIL the
# cross-check if release-plan.sh ever read it (proving Unreleased is skipped).
# release-plan.sh only ever looks up the tag's own section, so one entry suffices.
write_changelog() {
  # $1 = version (no `v`); $2 = Ships tokens (verbatim, after "**Ships:** ")
  cat > "${SCRATCH}/CHANGELOG.md" <<EOF
# Changelog

<!-- synthetic fixture — see scripts/release/release-scripts-test.sh -->

## Unreleased

**Ships:** server, host-agent, machine-rc, desktop

### Added
- decoy entry (must never be read for a released tag)

## v$1 — 2026-01-01

**Ships:** $2

### Added
- synthetic entry for tests
EOF
}

# ---------------------------------------------------------------------------
step "update-version.sh: unknown component hard-errors and names the valid set"
if out="$("${UV}" 1.0.0 --components gopher 2>&1)"; then
  fail "unknown component 'gopher' was accepted"
fi
echo "${out}" | grep -q "unknown component 'gopher'" || fail "error message missing the offending component: ${out}"
echo "${out}" | grep -q "server, host-agent, machine-rc, desktop" || fail "error message doesn't list valid components: ${out}"
ok "rejected with valid-name listing"

# ---------------------------------------------------------------------------
step "update-version.sh --components go bumps plugin.json ONLY (desktop untouched)"
before="$(desktop_state)"
"${UV}" 9.9.9 --components go >/dev/null
[ "$(jq -r '.version' "${SCRATCH}/.claude-plugin/plugin.json")" = "9.9.9" ] || fail "plugin.json didn't bump to 9.9.9"
[ "$(desktop_state)" = "${before}" ] || fail "--components go modified a desktop version surface"
ok "plugin.json=9.9.9, desktop surfaces byte-identical"

# ---------------------------------------------------------------------------
step "release-plan.sh: server-only tag (only plugin.json matches)"
write_changelog 9.9.9 "server"
run_plan v9.9.9
[ "${PLAN_RC}" -eq 0 ] || fail "server-only plan exited ${PLAN_RC}"
[ "${PLAN_OUT}" = "$(expect_plan true false false false true)" ] || fail "server-only plan stdout: ${PLAN_OUT}"
ok "ship_server=true, others false, ship_goreleaser=true"

# ---------------------------------------------------------------------------
step "update-version.sh --components desktop lands all four surfaces + both locks (0.0.x -> 0.8.0 jump)"
"${UV}" 0.8.0 --components desktop >/dev/null
[ "$(tr -d '[:space:]' < "${SCRATCH}/desktop/VERSION")" = "0.8.0" ] || fail "desktop/VERSION didn't bump"
grep -q '^version = "0.8.0"' "${SCRATCH}/crates/Cargo.toml" || fail "crates/Cargo.toml didn't bump"
grep -q '^version = "0.8.0"' "${SCRATCH}/desktop/tauri/src-tauri/Cargo.toml" || fail "tauri Cargo.toml didn't bump"
[ "$(jq -r '.version' "${SCRATCH}/desktop/tauri/src-tauri/tauri.conf.json")" = "0.8.0" ] || fail "tauri.conf.json didn't bump"
for dep in shed-core shed-app; do
  grep -A1 "^name = \"${dep}\"$" "${SCRATCH}/crates/Cargo.lock" | grep -q '^version = "0.8.0"' \
    || fail "crates/Cargo.lock ${dep} entry didn't refresh"
  grep -A1 "^name = \"${dep}\"$" "${SCRATCH}/desktop/tauri/src-tauri/Cargo.lock" | grep -q '^version = "0.8.0"' \
    || fail "tauri Cargo.lock ${dep} entry didn't refresh"
done
[ "$(jq -r '.version' "${SCRATCH}/.claude-plugin/plugin.json")" = "9.9.9" ] || fail "--components desktop touched plugin.json"
ok "VERSION + crates Cargo.toml/lock + tauri Cargo.toml/conf/lock all at 0.8.0; plugin.json untouched"

# ---------------------------------------------------------------------------
step "release-plan.sh: desktop-only tag (no goreleaser manifest matches)"
write_changelog 0.8.0 "desktop"
run_plan v0.8.0
[ "${PLAN_RC}" -eq 0 ] || fail "desktop-only plan exited ${PLAN_RC}"
[ "${PLAN_OUT}" = "$(expect_plan false false false true false)" ] || fail "desktop-only plan stdout: ${PLAN_OUT}"
ok "ship_desktop=true only, ship_goreleaser=false"

# ---------------------------------------------------------------------------
step "update-version.sh --components go,desktop; release-plan sees a combined tag"
"${UV}" 0.8.0 --components go,desktop >/dev/null
[ "$(jq -r '.version' "${SCRATCH}/.claude-plugin/plugin.json")" = "0.8.0" ] || fail "plugin.json didn't bump to 0.8.0"
write_changelog 0.8.0 "server, desktop"
run_plan v0.8.0
[ "${PLAN_RC}" -eq 0 ] || fail "combined plan exited ${PLAN_RC}"
[ "${PLAN_OUT}" = "$(expect_plan true false false true true)" ] || fail "combined plan stdout: ${PLAN_OUT}"
ok "ship_server=true ship_desktop=true ship_goreleaser=true"

# ---------------------------------------------------------------------------
step "release-plan.sh: GITHUB_REF_NAME fallback (no positional arg)"
run_plan_env_rc=0
plan_env_out="$(GITHUB_REF_NAME=v0.8.0 "${RP}" 2>/dev/null)" || run_plan_env_rc=$?
[ "${run_plan_env_rc}" -eq 0 ] || fail "GITHUB_REF_NAME fallback exited ${run_plan_env_rc}"
[ "${plan_env_out}" = "$(expect_plan true false false true true)" ] || fail "GITHUB_REF_NAME fallback stdout: ${plan_env_out}"
ok "reads the tag from GITHUB_REF_NAME"

# ---------------------------------------------------------------------------
step "update-version.sh: idempotent re-run (byte-identical tree)"
before="$(desktop_state)$(jq -r '.version' "${SCRATCH}/.claude-plugin/plugin.json")"
"${UV}" 0.8.0 --components go,desktop >/dev/null
after="$(desktop_state)$(jq -r '.version' "${SCRATCH}/.claude-plugin/plugin.json")"
[ "${before}" = "${after}" ] || fail "re-running the same bump changed bytes"
ok "same-version re-run is a no-op"

# ---------------------------------------------------------------------------
step "release-plan.sh: tag matching NEITHER manifest exits 1"
run_plan v7.7.7
[ "${PLAN_RC}" -eq 1 ] || fail "no-match plan exited ${PLAN_RC} (want 1)"
out="$("${RP}" v7.7.7 2>&1 || true)"
echo "${out}" | grep -q "matches NO component manifest" || fail "no-match error message missing: ${out}"
ok "loud exit 1 on a silent no-op tag"

# ---------------------------------------------------------------------------
step "release-plan.sh: broken lockstep (tauri.conf.json drifts) exits 1 naming the offender"
sed -i.bak -E 's/"version": "0.8.0"/"version": "0.7.9"/' "${SCRATCH}/desktop/tauri/src-tauri/tauri.conf.json"
run_plan v0.8.0
[ "${PLAN_RC}" -eq 1 ] || fail "lockstep-broken (tauri.conf.json) plan exited ${PLAN_RC} (want 1)"
out="$("${RP}" v0.8.0 2>&1 || true)"
echo "${out}" | grep -q "tauri.conf.json" || fail "lockstep error doesn't name tauri.conf.json: ${out}"
mv "${SCRATCH}/desktop/tauri/src-tauri/tauri.conf.json.bak" "${SCRATCH}/desktop/tauri/src-tauri/tauri.conf.json"
ok "tauri.conf.json drift caught"

# ---------------------------------------------------------------------------
step "release-plan.sh: broken lockstep (stale shed-core entry in the tauri lock) exits 1"
lock="${SCRATCH}/desktop/tauri/src-tauri/Cargo.lock"
awk '
  $0 == "name = \"shed-core\"" { print; getline; sub(/"0\.8\.0"/, "\"0.0.1\""); print; next }
  { print }
' "${lock}" > "${lock}.tmp" && mv "${lock}.tmp" "${lock}"
grep -A1 '^name = "shed-core"$' "${lock}" | grep -q '^version = "0.0.1"' || fail "test setup: lock entry not staled"
run_plan v0.8.0
[ "${PLAN_RC}" -eq 1 ] || fail "lockstep-broken (lock entry) plan exited ${PLAN_RC} (want 1)"
out="$("${RP}" v0.8.0 2>&1 || true)"
echo "${out}" | grep -q "Cargo.lock entry for shed-core" || fail "lockstep error doesn't name the shed-core lock entry: ${out}"
# Repair via the bumper itself (also proves it fixes exactly this state).
"${UV}" 0.8.0 --components desktop >/dev/null
run_plan v0.8.0
[ "${PLAN_RC}" -eq 0 ] || fail "lockstep not repaired by re-running the bumper"
ok "stale lock entry caught; bumper repairs it"

# ===========================================================================
# Per-component + Ships-grammar cases (new VERSION manifests + CHANGELOG cross-
# check). These MUTATE the shared scratch state; each case leaves the tree in a
# known shape for the next. C1 has no update-version.sh arm for the new VERSION
# files (that's C2), so they're written directly here.
#
# Entering state: plugin.json=0.8.0, host-agent=0.0.0, machine-rc=0.0.0 (the
# hermetic baseline — never bumped above), desktop surfaces=0.8.0 (lockstep
# intact).
# ===========================================================================

HOST_AGENT_VER="${SCRATCH}/crates/shed-host-agent/VERSION"
MACHINE_RC_VER="${SCRATCH}/cmd/shed-machine-rc/VERSION"

# ---------------------------------------------------------------------------
step "release-plan.sh: host-agent-only tag (only crates/shed-host-agent/VERSION matches)"
printf '1.2.3\n' > "${HOST_AGENT_VER}"
write_changelog 1.2.3 "host-agent"
run_plan v1.2.3
[ "${PLAN_RC}" -eq 0 ] || fail "host-agent-only plan exited ${PLAN_RC}"
[ "${PLAN_OUT}" = "$(expect_plan false true false false true)" ] || fail "host-agent-only plan stdout: ${PLAN_OUT}"
ok "ship_host_agent=true only, ship_goreleaser=true"

# ---------------------------------------------------------------------------
step "release-plan.sh: machine-rc-only tag (only cmd/shed-machine-rc/VERSION matches)"
printf '3.4.5\n' > "${MACHINE_RC_VER}"
write_changelog 3.4.5 "machine-rc"
run_plan v3.4.5
[ "${PLAN_RC}" -eq 0 ] || fail "machine-rc-only plan exited ${PLAN_RC}"
[ "${PLAN_OUT}" = "$(expect_plan false false true false true)" ] || fail "machine-rc-only plan stdout: ${PLAN_OUT}"
ok "ship_machine_rc=true only, ship_goreleaser=true"

# ---------------------------------------------------------------------------
step "release-plan.sh: combined server + host-agent (both manifests at 4.0.0)"
"${UV}" 4.0.0 --components go >/dev/null
printf '4.0.0\n' > "${HOST_AGENT_VER}"
write_changelog 4.0.0 "server, host-agent"
run_plan v4.0.0
[ "${PLAN_RC}" -eq 0 ] || fail "server+host-agent plan exited ${PLAN_RC}"
[ "${PLAN_OUT}" = "$(expect_plan true true false false true)" ] || fail "server+host-agent plan stdout: ${PLAN_OUT}"
ok "ship_server=true ship_host_agent=true ship_goreleaser=true"

# ---------------------------------------------------------------------------
step "release-plan.sh: legacy 'server/CLI' alias accepted (server + desktop tag)"
"${UV}" 0.8.0 --components go >/dev/null
write_changelog 0.8.0 "server/CLI, desktop"
run_plan v0.8.0
[ "${PLAN_RC}" -eq 0 ] || fail "legacy-alias plan exited ${PLAN_RC}"
[ "${PLAN_OUT}" = "$(expect_plan true false false true true)" ] || fail "legacy-alias plan stdout: ${PLAN_OUT}"
ok "server/CLI alias maps to server; ship_server=true ship_desktop=true"

# ---------------------------------------------------------------------------
step "release-plan.sh: unknown Ships token exits 1 naming it"
write_changelog 0.8.0 "server, gopher"
run_plan v0.8.0
[ "${PLAN_RC}" -eq 1 ] || fail "unknown-token plan exited ${PLAN_RC} (want 1)"
out="$("${RP}" v0.8.0 2>&1 || true)"
echo "${out}" | grep -q "unknown token 'gopher'" || fail "unknown-token error doesn't name gopher: ${out}"
ok "unknown token 'gopher' rejected"

# ---------------------------------------------------------------------------
step "release-plan.sh: duplicate Ships token (post-aliasing) exits 1"
write_changelog 0.8.0 "server, server/CLI"
run_plan v0.8.0
[ "${PLAN_RC}" -eq 1 ] || fail "duplicate-token plan exited ${PLAN_RC} (want 1)"
out="$("${RP}" v0.8.0 2>&1 || true)"
echo "${out}" | grep -q "duplicate token 'server'" || fail "duplicate-token error missing: ${out}"
ok "duplicate 'server' (via server/CLI alias) rejected"

# ---------------------------------------------------------------------------
step "release-plan.sh: Ships set disagreeing with manifests exits 1 showing both"
"${UV}" 5.0.0 --components go >/dev/null   # plugin.json=5.0.0 → server-only tag
write_changelog 5.0.0 "server, desktop"
run_plan v5.0.0
[ "${PLAN_RC}" -eq 1 ] || fail "ships-mismatch plan exited ${PLAN_RC} (want 1)"
out="$("${RP}" v5.0.0 2>&1 || true)"
echo "${out}" | grep -q "disagrees with the manifest-computed ship set" || fail "ships-mismatch error missing phrase: ${out}"
echo "${out}" | grep -q "desktop,server" || fail "ships-mismatch error missing actual set: ${out}"
ok "Ships (desktop,server) vs manifests (server) mismatch caught"

# ---------------------------------------------------------------------------
step "release-plan.sh: stable tag with no CHANGELOG entry exits 1"
write_changelog 9.9.9 "server"   # no ## v5.0.0 section present
run_plan v5.0.0
[ "${PLAN_RC}" -eq 1 ] || fail "missing-entry plan exited ${PLAN_RC} (want 1)"
out="$("${RP}" v5.0.0 2>&1 || true)"
echo "${out}" | grep -q "no '## v5.0.0 — <date>' section" || fail "missing-entry error missing: ${out}"
ok "missing CHANGELOG entry for a stable tag caught"

# ---------------------------------------------------------------------------
step "release-plan.sh: prerelease tag shipping a goreleaser component exits 1"
printf '2.0.0-rc.1\n' > "${HOST_AGENT_VER}"
run_plan v2.0.0-rc.1
[ "${PLAN_RC}" -eq 1 ] || fail "prerelease-goreleaser plan exited ${PLAN_RC} (want 1)"
out="$("${RP}" v2.0.0-rc.1 2>&1 || true)"
echo "${out}" | grep -q "ships a goreleaser component" || fail "prerelease-goreleaser error missing: ${out}"
ok "prerelease host-agent tag rejected (goreleaser components are stable-only)"

# ---------------------------------------------------------------------------
step "release-plan.sh: desktop-only prerelease plans cleanly (no Ships check)"
"${UV}" 2.1.0-rc.1 --components desktop >/dev/null   # desktop surfaces at the rc, lockstep held
run_plan v2.1.0-rc.1
[ "${PLAN_RC}" -eq 0 ] || fail "desktop-only prerelease plan exited ${PLAN_RC}"
[ "${PLAN_OUT}" = "$(expect_plan false false false true false)" ] || fail "desktop-only prerelease stdout: ${PLAN_OUT}"
ok "ship_desktop=true only; prerelease skips the Ships cross-check"

# ---------------------------------------------------------------------------
step "release-plan.sh: empty VERSION manifest exits 1 naming the file"
: > "${HOST_AGENT_VER}"
run_plan v0.8.0
[ "${PLAN_RC}" -eq 1 ] || fail "empty-manifest plan exited ${PLAN_RC} (want 1)"
out="$("${RP}" v0.8.0 2>&1 || true)"
echo "${out}" | grep -q "crates/shed-host-agent/VERSION is empty" || fail "empty-manifest error doesn't name the file: ${out}"
printf '%s\n' "${BASELINE}" > "${HOST_AGENT_VER}"   # restore to the hermetic baseline (defensive)
ok "empty crates/shed-host-agent/VERSION rejected, naming the file"

# ---------------------------------------------------------------------------
step "release-plan.sh: desktop/VERSION with internal whitespace exits 1 naming the file"
# `tr -d '[:space:]'` used to normalize `0 . 8 . 0` into a false `0.8.0` match;
# routing desktop/VERSION through read_version_file rejects it as non-semver.
cp "${SCRATCH}/desktop/VERSION" "${SCRATCH}/desktop/VERSION.bak"
printf '0 . 8 . 0\n' > "${SCRATCH}/desktop/VERSION"
run_plan v0.8.0
[ "${PLAN_RC}" -eq 1 ] || fail "whitespace desktop/VERSION plan exited ${PLAN_RC} (want 1)"
out="$("${RP}" v0.8.0 2>&1 || true)"
echo "${out}" | grep -q "desktop/VERSION" || fail "whitespace-VERSION error doesn't name the file: ${out}"
mv "${SCRATCH}/desktop/VERSION.bak" "${SCRATCH}/desktop/VERSION"
ok "internal-whitespace desktop/VERSION rejected (no false match), naming the file"

# ---------------------------------------------------------------------------
step "release-plan.sh: missing crates/shed-host-agent/VERSION exits 1 naming the file"
mv "${HOST_AGENT_VER}" "${HOST_AGENT_VER}.bak"
run_plan v0.8.0
[ "${PLAN_RC}" -eq 1 ] || fail "missing-manifest plan exited ${PLAN_RC} (want 1)"
out="$("${RP}" v0.8.0 2>&1 || true)"
echo "${out}" | grep -q "crates/shed-host-agent/VERSION is missing" || fail "missing-manifest error doesn't name the file: ${out}"
mv "${HOST_AGENT_VER}.bak" "${HOST_AGENT_VER}"
ok "missing crates/shed-host-agent/VERSION rejected, naming the file"

# ---------------------------------------------------------------------------
step "release-plan.sh: a leading-zero tag (v0.07.11) is rejected as malformed (exit 2)"
# SEMVER_RE bars leading zeros in each field, matching update-version.sh /
# recommend-components.sh, so a zero-padded tag is a malformed-tag usage error.
run_plan v0.07.11
[ "${PLAN_RC}" -eq 2 ] || fail "leading-zero tag v0.07.11 exited ${PLAN_RC} (want 2)"
out="$("${RP}" v0.07.11 2>&1 || true)"
echo "${out}" | grep -q "is not vX.Y.Z" || fail "leading-zero-tag error missing: ${out}"
ok "v0.07.11 rejected as a malformed tag (leading zeros barred)"

# ---------------------------------------------------------------------------
step "release-plan.sh: a zero-padded VERSION manifest (1.02.3) exits 1 naming the file"
# read_version_file validates every VERSION manifest up front, so a zero-padded
# field is caught as non-semver regardless of the (well-formed) tag argument.
printf '1.02.3\n' > "${HOST_AGENT_VER}"
run_plan v9.9.9
[ "${PLAN_RC}" -eq 1 ] || fail "zero-padded-manifest plan exited ${PLAN_RC} (want 1)"
out="$("${RP}" v9.9.9 2>&1 || true)"
echo "${out}" | grep -q "crates/shed-host-agent/VERSION" || fail "zero-padded-manifest error doesn't name the file: ${out}"
echo "${out}" | grep -q "not semver" || fail "zero-padded-manifest error missing 'not semver': ${out}"
printf '%s\n' "${BASELINE}" > "${HOST_AGENT_VER}"   # restore to the hermetic baseline
ok "zero-padded 1.02.3 in crates/shed-host-agent/VERSION rejected, naming the file"

# ---------------------------------------------------------------------------
step "release-plan.sh: Ships line with a trailing comma exits 1"
# `read -a` drops a trailing empty field, so `**Ships:** server,` used to slip
# past the per-token empty check; the explicit leading/trailing-comma guard
# closes that gap. plugin.json=5.0.0 here → server-only tag reaches the check.
write_changelog 5.0.0 "server,"
run_plan v5.0.0
[ "${PLAN_RC}" -eq 1 ] || fail "trailing-comma Ships plan exited ${PLAN_RC} (want 1)"
out="$("${RP}" v5.0.0 2>&1 || true)"
echo "${out}" | grep -q "empty token" || fail "trailing-comma error missing 'empty token': ${out}"
ok "trailing comma in Ships line rejected (read -a drop-trailing gap closed)"

# ---------------------------------------------------------------------------
step "release-plan.sh: duplicate '## v<V>' headings — only the first (Ships-less) section is parsed → exits 1"
# Two headings for the same version: the FIRST has no Ships line, the SECOND
# does. The awk extraction must capture ONLY the first section (never re-enter),
# so this errors "missing Ships line" instead of borrowing the second's Ships.
cat > "${SCRATCH}/CHANGELOG.md" <<'EOF'
# Changelog

## v5.0.0 — 2026-01-01

### Added
- first section for this version, deliberately missing a Ships line

## v5.0.0 — 2026-01-02

**Ships:** server

### Added
- second, later section that DOES carry a Ships line
EOF
run_plan v5.0.0
[ "${PLAN_RC}" -eq 1 ] || fail "duplicate-heading plan exited ${PLAN_RC} (want 1)"
out="$("${RP}" v5.0.0 2>&1 || true)"
echo "${out}" | grep -qF "has no **Ships:** line" || fail "duplicate-heading error doesn't report a missing Ships line: ${out}"
ok "only the first duplicate section is parsed; its missing Ships line is caught (no borrow from the second)"

# ===========================================================================
# C2: update-version.sh new arms (host-agent / machine-rc / 4-way), the `go`
# deprecation alias, and the prerelease matrix. These run last (they mutate the
# shared scratch manifests; nothing after them depends on the resulting state).
# ===========================================================================

hostagent_ver() { tr -d '[:space:]' < "${HOST_AGENT_VER}"; }
machinerc_ver() { tr -d '[:space:]' < "${MACHINE_RC_VER}"; }

# ---------------------------------------------------------------------------
step "update-version.sh --components host-agent bumps ONLY crates/shed-host-agent/VERSION"
before_desktop="$(desktop_state)"
before_plugin="$(jq -r '.version' "${SCRATCH}/.claude-plugin/plugin.json")"
before_mrc="$(machinerc_ver)"
"${UV}" 6.1.0 --components host-agent >/dev/null
[ "$(hostagent_ver)" = "6.1.0" ] || fail "host-agent VERSION didn't bump to 6.1.0"
[ "$(machinerc_ver)" = "${before_mrc}" ] || fail "--components host-agent touched machine-rc VERSION"
[ "$(jq -r '.version' "${SCRATCH}/.claude-plugin/plugin.json")" = "${before_plugin}" ] || fail "--components host-agent touched plugin.json"
[ "$(desktop_state)" = "${before_desktop}" ] || fail "--components host-agent touched a desktop surface"
ok "host-agent VERSION=6.1.0; plugin/machine-rc/desktop untouched"

# ---------------------------------------------------------------------------
step "update-version.sh --components machine-rc bumps ONLY cmd/shed-machine-rc/VERSION"
before_desktop="$(desktop_state)"
before_plugin="$(jq -r '.version' "${SCRATCH}/.claude-plugin/plugin.json")"
before_ha="$(hostagent_ver)"
"${UV}" 6.2.0 --components machine-rc >/dev/null
[ "$(machinerc_ver)" = "6.2.0" ] || fail "machine-rc VERSION didn't bump to 6.2.0"
[ "$(hostagent_ver)" = "${before_ha}" ] || fail "--components machine-rc touched host-agent VERSION"
[ "$(jq -r '.version' "${SCRATCH}/.claude-plugin/plugin.json")" = "${before_plugin}" ] || fail "--components machine-rc touched plugin.json"
[ "$(desktop_state)" = "${before_desktop}" ] || fail "--components machine-rc touched a desktop surface"
ok "machine-rc VERSION=6.2.0; plugin/host-agent/desktop untouched"

# ---------------------------------------------------------------------------
step "update-version.sh --components server,host-agent,machine-rc,desktop bumps all four"
"${UV}" 6.3.0 --components server,host-agent,machine-rc,desktop >/dev/null
[ "$(jq -r '.version' "${SCRATCH}/.claude-plugin/plugin.json")" = "6.3.0" ] || fail "4-way: plugin.json didn't bump"
[ "$(hostagent_ver)" = "6.3.0" ] || fail "4-way: host-agent VERSION didn't bump"
[ "$(machinerc_ver)" = "6.3.0" ] || fail "4-way: machine-rc VERSION didn't bump"
[ "$(tr -d '[:space:]' < "${SCRATCH}/desktop/VERSION")" = "6.3.0" ] || fail "4-way: desktop/VERSION didn't bump"
grep -q '^version = "6.3.0"' "${SCRATCH}/crates/Cargo.toml" || fail "4-way: crates/Cargo.toml didn't bump"
ok "server + host-agent + machine-rc + desktop all at 6.3.0"

# ---------------------------------------------------------------------------
step "update-version.sh: the 4-way bump is idempotent (byte-identical re-run)"
before="$(desktop_state)$(jq -r '.version' "${SCRATCH}/.claude-plugin/plugin.json")$(hostagent_ver)$(machinerc_ver)"
"${UV}" 6.3.0 --components server,host-agent,machine-rc,desktop >/dev/null
after="$(desktop_state)$(jq -r '.version' "${SCRATCH}/.claude-plugin/plugin.json")$(hostagent_ver)$(machinerc_ver)"
[ "${before}" = "${after}" ] || fail "re-running the 4-way bump changed bytes"
ok "4-way re-run is a no-op"

# ---------------------------------------------------------------------------
step "update-version.sh: 'go' is a deprecated alias for 'server' AND warns on stderr"
err="$("${UV}" 6.4.0 --components go 2>&1 >/dev/null)"
[ "$(jq -r '.version' "${SCRATCH}/.claude-plugin/plugin.json")" = "6.4.0" ] || fail "'go' alias didn't bump plugin.json"
echo "${err}" | grep -q "deprecated alias for 'server'" || fail "'go' alias didn't warn on stderr: ${err}"
ok "'go' bumps plugin.json (== server) and prints the deprecation warning to stderr"

# ---------------------------------------------------------------------------
step "update-version.sh: prerelease + host-agent is rejected (goreleaser components stable-only)"
if out="$("${UV}" 7.0.0-rc.1 --components host-agent 2>&1)"; then
  fail "prerelease host-agent bump was accepted"
fi
echo "${out}" | grep -q "stable-only" || fail "prerelease-host-agent error missing 'stable-only': ${out}"
ok "7.0.0-rc.1 --components host-agent rejected"

# ---------------------------------------------------------------------------
step "update-version.sh: prerelease + server,machine-rc rejected; prerelease desktop-only allowed"
if "${UV}" 7.0.0-rc.1 --components server,machine-rc >/dev/null 2>&1; then
  fail "prerelease server,machine-rc bump was accepted"
fi
"${UV}" 7.0.0-rc.1 --components desktop >/dev/null || fail "desktop-only prerelease bump was rejected"
[ "$(tr -d '[:space:]' < "${SCRATCH}/desktop/VERSION")" = "7.0.0-rc.1" ] || fail "desktop-only prerelease didn't bump desktop/VERSION"
ok "prerelease rejected for goreleaser components, allowed for desktop-only"

# ---------------------------------------------------------------------------
step "update-version.sh: a leading-zero version (0.07.11) is rejected as non-semver"
if out="$("${UV}" 0.07.11 --components server 2>&1)"; then
  fail "leading-zero version 0.07.11 was accepted"
fi
echo "${out}" | grep -q "not semver" || fail "leading-zero error missing 'not semver': ${out}"
ok "0.07.11 rejected (leading zeros barred)"

# ---------------------------------------------------------------------------
step "update-version.sh: --components go,go warns exactly once (not per occurrence)"
err="$("${UV}" 6.5.0 --components go,go 2>&1 >/dev/null)"
[ "$(jq -r '.version' "${SCRATCH}/.claude-plugin/plugin.json")" = "6.5.0" ] || fail "go,go didn't bump plugin.json"
n_warn="$(printf '%s\n' "${err}" | grep -c "deprecated alias for 'server'")"
[ "${n_warn}" -eq 1 ] || fail "expected exactly 1 deprecation warning, got ${n_warn}: ${err}"
ok "go,go bumps plugin.json and warns once"

# ===========================================================================
# C2: recommend-components.sh against a throwaway git repo with synthetic tagged
# history. Built here (not tar-copied) because the recommender walks tag history
# and diffs <lastship>..HEAD. It resolves its repo from `git rev-parse
# --show-toplevel` of the CWD, so pointing it at the fixture is just a `cd`. gh
# is forced off (RECOMMEND_NO_GH=1) — the fixture has no GitHub releases; the
# failed-tag caveat path must stay non-fatal.
#
# Synthetic history:
#   v0.9.0  plugin.json 0.9.0, desktop/VERSION 0.9.0; NO host-agent/machine-rc
#           VERSION files (pre-migration) → their walk FALLS BACK to this tag.
#   v0.9.1  desktop-only: desktop/VERSION 0.9.1 (plugin still 0.9.0) + a commit
#           touching ONLY crates/Cargo.toml + Cargo.lock (desktop lockfile churn).
#   HEAD    post-v0.9.1: sdk/x.go, .claude-plugin/plugin-meta.txt, desktop/ui.txt,
#           then (LAST, added mid-suite) crates/shed-broker/src/lib.rs.
# ===========================================================================

FIX="${SCRATCH}/reco-fixture"
mkdir -p "${FIX}"
git -C "${FIX}" init -q
gitf() { git -C "${FIX}" -c user.email=t@t -c user.name=t "$@"; }

# --- base tree + v0.9.0 (pre-migration; no host-agent/machine-rc VERSION) ---
mkdir -p "${FIX}/.claude-plugin" "${FIX}/sdk" "${FIX}/crates/shed-broker/src" "${FIX}/desktop"
printf '{"version": "0.9.0"}\n' > "${FIX}/.claude-plugin/plugin.json"
printf '0.9.0\n' > "${FIX}/desktop/VERSION"
printf 'package sdk\n' > "${FIX}/sdk/x.go"
printf 'meta v0\n' > "${FIX}/.claude-plugin/plugin-meta.txt"
printf 'pub fn broker() {}\n' > "${FIX}/crates/shed-broker/src/lib.rs"
printf 'ui v0\n' > "${FIX}/desktop/ui.txt"
printf 'version = "0.9.0"\n' > "${FIX}/crates/Cargo.toml"
printf 'lock v0\n' > "${FIX}/crates/Cargo.lock"
gitf add -A
gitf commit -q -m 'v0.9.0 base'
gitf tag v0.9.0

# --- v0.9.1: desktop-only bump, then a Cargo-only churn commit ---
printf '0.9.1\n' > "${FIX}/desktop/VERSION"
gitf add -A; gitf commit -q -m 'desktop -> 0.9.1'
printf 'version = "0.9.1"\n' > "${FIX}/crates/Cargo.toml"
printf 'lock v1\n' > "${FIX}/crates/Cargo.lock"
gitf add -A; gitf commit -q -m 'desktop bump: crates/Cargo.{toml,lock} churn only'
gitf tag v0.9.1

# --- post-v0.9.1 commits; the shed-broker change is added LATER so we can run
#     the recommender at the pre-broker rev (Cargo churn present, broker absent). ---
printf 'package sdk // changed\n' > "${FIX}/sdk/x.go"
gitf add -A; gitf commit -q -m 'server: sdk change'
printf 'meta v1\n' > "${FIX}/.claude-plugin/plugin-meta.txt"
gitf add -A; gitf commit -q -m 'server: .claude-plugin change'
printf 'ui v1\n' > "${FIX}/desktop/ui.txt"
gitf add -A; gitf commit -q -m 'desktop: ui change'

# run_reco <args...> → RECO_OUT (stdout), RECO_ERR (stderr), RECO_RC (status).
RECO_OUT=""; RECO_ERR=""; RECO_RC=0
run_reco() {
  RECO_RC=0
  RECO_OUT="$(cd "${FIX}" && RECOMMEND_NO_GH=1 "${RECO}" "$@" 2>"${SCRATCH}/reco.err")" || RECO_RC=$?
  RECO_ERR="$(cat "${SCRATCH}/reco.err")"
}

# ---------------------------------------------------------------------------
step "recommend-components.sh: desktop Cargo churn ALONE does not flag host-agent (patch, pre-broker rev)"
run_reco 0.9.2
[ "${RECO_RC}" -eq 0 ] || fail "pre-broker recommend exited ${RECO_RC}: ${RECO_ERR}"
echo "${RECO_OUT}" | grep -qx "level=patch" || fail "pre-broker level wrong: ${RECO_OUT}"
echo "${RECO_OUT}" | grep -qx "recommended_components=server,desktop" || fail "pre-broker recommendation wrong: ${RECO_OUT} // ${RECO_ERR}"
ok "server,desktop only — the crates/Cargo.{toml,lock} churn did NOT flag host-agent"

# --- now add the shed-broker change (LAST commit) ---
printf 'pub fn broker() { /* changed */ }\n' > "${FIX}/crates/shed-broker/src/lib.rs"
gitf add -A; gitf commit -q -m 'host-agent: shed-broker change'

# ---------------------------------------------------------------------------
step "recommend-components.sh: patch 0.9.2 → server (sdk+plugin), host-agent (broker), desktop (ui); NOT machine-rc"
run_reco 0.9.2
[ "${RECO_RC}" -eq 0 ] || fail "patch recommend exited ${RECO_RC}: ${RECO_ERR}"
echo "${RECO_OUT}" | grep -qx "level=patch" || fail "patch level wrong: ${RECO_OUT}"
echo "${RECO_OUT}" | grep -qx "recommended_components=server,host-agent,desktop" || fail "patch recommendation wrong: ${RECO_OUT} // ${RECO_ERR}"
echo "${RECO_ERR}" | grep -q "CAVEAT (failed-tag)" || fail "gh-skip failed-tag caveat not printed (should stay non-fatal): ${RECO_ERR}"
ok "server,host-agent,desktop (machine-rc correctly unflagged; gh-skip caveat non-fatal)"

# ---------------------------------------------------------------------------
step "recommend-components.sh: last-shipped fallback (host-agent/machine-rc → v0.9.0 via plugin match; desktop → v0.9.1)"
echo "${RECO_ERR}" | grep -Eq '^[[:space:]]*server[[:space:]]+v0\.9\.0[[:space:]]' || fail "server last-shipped not v0.9.0: ${RECO_ERR}"
echo "${RECO_ERR}" | grep -Eq '^[[:space:]]*host-agent[[:space:]]+v0\.9\.0[[:space:]]' || fail "host-agent last-shipped not v0.9.0 (fallback): ${RECO_ERR}"
echo "${RECO_ERR}" | grep -Eq '^[[:space:]]*machine-rc[[:space:]]+v0\.9\.0[[:space:]]' || fail "machine-rc last-shipped not v0.9.0 (fallback): ${RECO_ERR}"
echo "${RECO_ERR}" | grep -Eq '^[[:space:]]*desktop[[:space:]]+v0\.9\.1[[:space:]]' || fail "desktop last-shipped not v0.9.1: ${RECO_ERR}"
ok "host-agent/machine-rc fell back to v0.9.0; desktop resolved to v0.9.1"

# ---------------------------------------------------------------------------
step "recommend-components.sh: minor 0.10.0 → ALL components regardless of diffs"
run_reco 0.10.0
[ "${RECO_RC}" -eq 0 ] || fail "minor recommend exited ${RECO_RC}: ${RECO_ERR}"
echo "${RECO_OUT}" | grep -qx "level=minor" || fail "minor level wrong: ${RECO_OUT}"
echo "${RECO_OUT}" | grep -qx "recommended_components=server,host-agent,machine-rc,desktop" || fail "minor recommendation wrong: ${RECO_OUT}"
ok "all four components (a minor bump ships the fleet)"

# ---------------------------------------------------------------------------
step "recommend-components.sh: target == max (0.9.1) and target < max (0.8.0) both exit 1 (monotonicity)"
run_reco 0.9.1
[ "${RECO_RC}" -eq 1 ] || fail "target==max didn't exit 1 (got ${RECO_RC})"
echo "${RECO_ERR}" | grep -q "is not greater than the max current manifest" || fail "target==max error missing: ${RECO_ERR}"
run_reco 0.8.0
[ "${RECO_RC}" -eq 1 ] || fail "target<max didn't exit 1 (got ${RECO_RC})"
ok "the monotonic-tag-family guard rejects target ≤ max"

# ---------------------------------------------------------------------------
step "recommend-components.sh: prerelease target 1.0.0-rc.1 exits 1"
run_reco 1.0.0-rc.1
[ "${RECO_RC}" -eq 1 ] || fail "prerelease target didn't exit 1 (got ${RECO_RC})"
echo "${RECO_ERR}" | grep -q "prerelease" || fail "prerelease error missing: ${RECO_ERR}"
ok "prerelease target rejected (recommendation is stable-only)"

# ---------------------------------------------------------------------------
step "recommend-components.sh: shallow clone is rejected with an --unshallow hint"
SHALLOW="${SCRATCH}/reco-shallow"
if git clone -q --depth 1 "file://${FIX}" "${SHALLOW}" 2>/dev/null \
   && [ "$(git -C "${SHALLOW}" rev-parse --is-shallow-repository 2>/dev/null)" = "true" ]; then
  reco_shallow_rc=0
  reco_shallow_err="$(cd "${SHALLOW}" && RECOMMEND_NO_GH=1 "${RECO}" 0.9.2 2>&1 1>/dev/null)" || reco_shallow_rc=$?
  [ "${reco_shallow_rc}" -eq 1 ] || fail "shallow-clone recommend didn't exit 1 (got ${reco_shallow_rc})"
  echo "${reco_shallow_err}" | grep -q "SHALLOW" || fail "shallow error missing: ${reco_shallow_err}"
  ok "shallow clone rejected with an --unshallow hint"
else
  ok "shallow clone rejected (skipped — this git did not produce a shallow clone over file://)"
fi

# ---------------------------------------------------------------------------
step "recommend-components.sh: a leading-zero target (0.07.11) is rejected (exit 2)"
run_reco 0.07.11
[ "${RECO_RC}" -eq 2 ] || fail "leading-zero target didn't exit 2 (got ${RECO_RC})"
echo "${RECO_ERR}" | grep -q "not a stable semver" || fail "leading-zero error missing: ${RECO_ERR}"
ok "0.07.11 rejected (leading zeros barred)"

# ---------------------------------------------------------------------------
# H1: the pre-migration fallback must require the VERSION file to be ABSENT at a
# tag — a post-migration tag that carries the file but shipped server-only
# (VERSION stale, plugin==tag) did NOT ship host-agent/machine-rc and must NOT be
# borrowed as the basis. A SEPARATE fixture (so it can't perturb the state the
# tests above thread through the shared reco-fixture).
#
#   v1.0.0  plugin=1.0.0, desktop=1.0.0, NO host-agent/machine-rc VERSION
#           (pre-migration → the legitimate fallback basis).
#   v1.1.0  server-only: plugin=1.1.0, and the host-agent/machine-rc VERSION
#           files NOW EXIST but at a STALE 1.0.0 (migration happened; the tag
#           shipped server only). first-loop finds no VERSION==tag; the FIXED
#           fallback skips v1.1.0 (file present) and lands on v1.0.0. A fallback
#           lacking the absent-file guard would wrongly pick v1.1.0.
# ---------------------------------------------------------------------------
step "recommend-components.sh: fallback skips a post-migration server-only tag (stale VERSION present) → basis v1.0.0"
FIX2="${SCRATCH}/reco-fixture-h1"
mkdir -p "${FIX2}"
git -C "${FIX2}" init -q
gitf2() { git -C "${FIX2}" -c user.email=t@t -c user.name=t "$@"; }
mkdir -p "${FIX2}/.claude-plugin" "${FIX2}/crates/shed-host-agent" "${FIX2}/cmd/shed-machine-rc" "${FIX2}/crates/shed-broker/src" "${FIX2}/desktop"
printf '{"version": "1.0.0"}\n' > "${FIX2}/.claude-plugin/plugin.json"
printf '1.0.0\n' > "${FIX2}/desktop/VERSION"
printf 'pub fn broker() {}\n' > "${FIX2}/crates/shed-broker/src/lib.rs"
gitf2 add -A; gitf2 commit -q -m 'v1.0.0 base (pre-migration: no VERSION files)'
gitf2 tag v1.0.0
# migration + server-only 1.1.0: VERSION files appear but stale at 1.0.0.
printf '{"version": "1.1.0"}\n' > "${FIX2}/.claude-plugin/plugin.json"
printf '1.0.0\n' > "${FIX2}/crates/shed-host-agent/VERSION"
printf '1.0.0\n' > "${FIX2}/cmd/shed-machine-rc/VERSION"
gitf2 add -A; gitf2 commit -q -m 'v1.1.0 server-only (VERSION files present, stale 1.0.0)'
gitf2 tag v1.1.0
# a post-tag change so the walk has a diff to compute.
printf 'pub fn broker() { /* changed */ }\n' > "${FIX2}/crates/shed-broker/src/lib.rs"
gitf2 add -A; gitf2 commit -q -m 'post-v1.1.0 broker change'
h1_rc=0
h1_err="$(cd "${FIX2}" && RECOMMEND_NO_GH=1 "${RECO}" 1.1.1 2>&1 1>/dev/null)" || h1_rc=$?
[ "${h1_rc}" -eq 0 ] || fail "H1 recommend exited ${h1_rc}: ${h1_err}"
echo "${h1_err}" | grep -Eq '^[[:space:]]*host-agent[[:space:]]+v1\.0\.0[[:space:]]' || fail "H1: host-agent basis not v1.0.0 (fallback picked the stale-VERSION server tag?): ${h1_err}"
echo "${h1_err}" | grep -Eq '^[[:space:]]*machine-rc[[:space:]]+v1\.0\.0[[:space:]]' || fail "H1: machine-rc basis not v1.0.0: ${h1_err}"
echo "${h1_err}" | grep -Eq '^[[:space:]]*host-agent[[:space:]]+v1\.1\.0[[:space:]]' && fail "H1: host-agent wrongly used v1.1.0 as basis: ${h1_err}"
ok "host-agent/machine-rc fell back to v1.0.0; the stale-VERSION v1.1.0 tag was NOT borrowed"

# ---------------------------------------------------------------------------
# H2: a stable target must dominate a same-base prerelease held in a manifest.
# desktop/VERSION legitimately holds an rc after a Tauri rc rehearsal; promoting
# to the stable base must pass the monotonicity guard (SemVer: 1.0.0 > 1.0.0-rc.1).
# Write the rc directly into the shared reco-fixture working tree (LAST reco
# case — nothing after depends on this manifest state).
# ---------------------------------------------------------------------------
step "recommend-components.sh: stable target 1.0.0 clears the monotonicity guard vs a desktop rc manifest (1.0.0-rc.1)"
printf '1.0.0-rc.1\n' > "${FIX}/desktop/VERSION"
run_reco 1.0.0
[ "${RECO_RC}" -eq 0 ] || fail "H2 stable-vs-rc recommend exited ${RECO_RC}: ${RECO_ERR}"
echo "${RECO_ERR}" | grep -q "is not greater than the max current manifest" && fail "H2: monotonicity guard wrongly rejected 1.0.0 vs 1.0.0-rc.1: ${RECO_ERR}"
echo "${RECO_ERR}" | grep -q "max current manifest 1.0.0-rc.1" || fail "H2: rc manifest not recognized as the max: ${RECO_ERR}"
ok "1.0.0 accepted over 1.0.0-rc.1 (stable dominates same-base prerelease)"

echo
echo "release-scripts-test: all ${PASS} checks passed"
