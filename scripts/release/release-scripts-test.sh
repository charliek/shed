#!/usr/bin/env bash
# Self-test for the release-model scripts:
#
#   scripts/release/update-version.sh        (component-selecting version bumper)
#   scripts/release/release-plan.sh          (tag → per-component ship selector)
#   scripts/release/recommend-components.sh  (next-tag component recommender)
#   scripts/release/desktop-signing-gate.sh  (a stable desktop tag fails closed
#                                             without the six Apple secrets)
#   scripts/release/changelog-section.sh     (the curated release body)
#
# Runs every synthetic combination the release model depends on — per-component
# (server / host-agent / desktop) and combined plans / neither→exit 1 /
# lockstep-broken→exit 1 / malformed-manifest→exit 1 / prerelease×
# goreleaser-component→exit 1 / desktop-only-prerelease→pass / the `**Ships:**`
# CHANGELOG cross-check (legacy alias, unknown token, duplicate, mismatch,
# missing entry, Unreleased-skipping) — plus the update-version.sh contract
# asserts (unknown component errors + valid-set listing; `--components server`
# leaves desktop manifests untouched; `--components desktop` bumps all four
# desktop surfaces + both locks; the per-component host-agent bump;
# the 3-way combined bump; `go` alias warns on stderr; prerelease rejected for
# goreleaser components but allowed desktop-only; idempotent re-run; the
# RETIRED `machine-rc` component rejected by both the bumper and the Ships
# grammar — plan 010; the SUNSET `sx` component rejected by both — plan 016).
#
# recommend-components.sh is exercised against a SEPARATE throwaway git repo
# (built below) with synthetic tagged history — proving the last-shipped walk,
# the pre-migration VERSION-absence fallback, the NEVER_SHIPPED first-ship
# bootstrap (and that an UNLISTED basis-less component still hard-errors),
# desktop-bump Cargo churn NOT flagging host-agent, sdk/plugin changes flagging server, the bump-level
# derivation, and the shallow / target≤max / prerelease preconditions.
#
# NEVER_SHIPPED is EMPTY since plan 016 (its only entry, `sx`, was sunset), so
# no real component exercises the first-ship bootstrap any more. The mechanism
# is kept honest by a SYNTHETIC POSITIVE: the H3 case copies
# recommend-components.sh into the scratch dir, sed-patches that copy's
# `NEVER_SHIPPED=()` to `NEVER_SHIPPED=(desktop)`, and runs it against the
# basis-less H3 fixture; the UNMODIFIED script must still hard-error on the same
# fixture. Nothing test-only is added to the production script.
#
# The whole thing runs in a TEMP COPY of the relevant files (scripts +
# manifests + both cargo workspaces, minus target/) so it never dirties the
# repo. Needs: bash, jq, cargo (the lock regens run `cargo update --offline`),
# git (the recommender fixture). The recommender's `gh` probe is forced off in
# the fixture (RECOMMEND_NO_GH=1) — synthetic tags have no GitHub releases.
#
# Phase 5 (plan 026 §3.11) runs desktop-signing-gate.sh and
# changelog-section.sh as deployed, and parses publish-images.yaml to assert the
# workflow really calls the gate: in the `release-plan` job right after
# release-plan.sh, conditioned on ship_desktop, and in desktop-macos right
# after its checkout, each with the six `--has NAME=${{ secrets.NAME != '' }}`
# flags CAN_NOTARIZE reads, as a top-level command no exit or open block can
# skip. It then EXECUTES both steps' run scripts (secrets rendered true/false)
# in scratch checkouts with and without the gate, which pins the missing-script
# fallback to a dispatch of a pre-0.9.0 tag. The parse uses python3 with PyYAML when it imports,
# else `yq -o=json` (mikefarah v4; the ubuntu runners carry it);
# RELEASE_SELFTEST_YAML_LOADER=pyyaml|yq forces one, to exercise the other path.
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
command -v python3 >/dev/null 2>&1 || { echo "error: python3 not found — the signing-gate wiring guard parses publish-images.yaml" >&2; exit 1; }

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
  scripts/release/check-craze-pin.sh \
  scripts/release/read-craze-pin.sh \
  scripts/release/desktop-signing-gate.sh \
  scripts/release/changelog-section.sh \
  .github/workflows/publish-images.yaml \
  craze-pin.env \
  .claude-plugin/plugin.json \
  desktop/VERSION \
  crates/shed-host-agent/VERSION \
  CHANGELOG.md \
  crates \
  desktop/tauri/src-tauri \
  | tar -C "${SCRATCH}" -xf -

UV="${SCRATCH}/scripts/release/update-version.sh"
RP="${SCRATCH}/scripts/release/release-plan.sh"
RECO="${SCRATCH}/scripts/release/recommend-components.sh"
CRAZE_PIN_FILE="${SCRATCH}/craze-pin.env"

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
# a synthetic case: once v0.8.0 shipped, crates/shed-host-agent/VERSION reads
# 0.8.0, and the desktop-only `v0.8.0` case below then matched extra components
# instead of one (issue #280). Reset all THREE selectors to a sentinel equal to
# NONE of the tags this test uses, so every
# case's residual (unbumped) manifest cannot collide with that case's tag — the
# test fully owns version state regardless of the repo's current release. (Uses
# update-version.sh's desktop arm, so it needs the primed cargo index above.)
BASELINE=0.0.0
"${UV}" "${BASELINE}" --components server,host-agent,desktop >/dev/null

# ---------------------------------------------------------------------------
# release-plan.sh now calls scripts/release/check-craze-pin.sh for every
# stable tag that ships `server` (plan 025 O4). That script's `git
# ls-remote` against charliek/craze is network, which this self-test must
# never need — SHED_RELEASE_SELFTEST=1 + CRAZE_PIN_LS_REMOTE substitute a
# fake that always resolves to CRAZE_FAKE_RESOLVED_SHA (see
# check-craze-pin.sh's seam). Exported ONCE here, matching
# CRAZE_FAKE_RESOLVED_SHA to the scratch craze-pin.env's CRAZE_TEST_SHA, so
# EVERY existing ship_server=true case above (and below) gets a default
# MATCHING fake with no per-case wiring — Phase 4 at the end of this file is
# the only place that deliberately mismatches it.
#
# FAKE_CRAZE_SHA / OTHER_FAKE_SHA are built via `printf` repetition rather
# than typed out, so their exactly-40-lowercase-hex shape (what
# check-craze-pin.sh itself validates) can't be thrown off by a manual
# miscount.
FAKE_CRAZE_SHA="$(printf 'a%.0s' $(seq 1 40))"
OTHER_FAKE_SHA="$(printf 'b%.0s' $(seq 1 40))"
cat > "${CRAZE_PIN_FILE}" <<EOF
CRAZE_TEST_SHA=${FAKE_CRAZE_SHA}
CRAZE_RELEASE=1.2.3
EOF
# CRAZE_FAKE_CALL_LOG: every fake invocation appends its args here, so a
# "the checker must NOT run at all" case (a desktop-only / host-agent-only
# stable tag, Phase 4) can assert the fake was never called — the matching
# fake alone can't prove that, since it would ALSO silently "pass" a scope
# regression that calls the checker when it must not.
CRAZE_FAKE_CALL_LOG="${SCRATCH}/craze-fake-calls.log"
: > "${CRAZE_FAKE_CALL_LOG}"
FAKE_LS_REMOTE="${SCRATCH}/fake-ls-remote.sh"
cat > "${FAKE_LS_REMOTE}" <<'EOF'
#!/usr/bin/env bash
# Self-test seam for check-craze-pin.sh's `git ls-remote` call (honoured
# ONLY under SHED_RELEASE_SELFTEST=1 — see release-scripts-test.sh and
# check-craze-pin.sh). Emits one ls-remote-style row per ref argument, all
# resolving to $CRAZE_FAKE_RESOLVED_SHA, and (if set) logs its invocation to
# $CRAZE_FAKE_CALL_LOG so a caller can assert it was or wasn't called.
set -euo pipefail
sha="${CRAZE_FAKE_RESOLVED_SHA:?CRAZE_FAKE_RESOLVED_SHA not set}"
if [ -n "${CRAZE_FAKE_CALL_LOG:-}" ]; then
  printf '%s\n' "$*" >> "${CRAZE_FAKE_CALL_LOG}"
fi
for ref in "$@"; do
  printf '%s\t%s\n' "${sha}" "${ref}"
done
EOF
chmod +x "${FAKE_LS_REMOTE}"
export SHED_RELEASE_SELFTEST=1
export CRAZE_PIN_LS_REMOTE="${FAKE_LS_REMOTE}"
export CRAZE_FAKE_RESOLVED_SHA="${FAKE_CRAZE_SHA}"
export CRAZE_FAKE_CALL_LOG

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

# The exact 4-line stdout release-plan.sh emits, in emission order. Building the
# expectation from named booleans keeps the assertions readable as the ship set
# changes case-to-case.
expect_plan() {
  # $1 server, $2 host-agent, $3 desktop, $4 goreleaser
  printf 'ship_server=%s\nship_host_agent=%s\nship_desktop=%s\nship_goreleaser=%s' \
    "$1" "$2" "$3" "$4"
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

**Ships:** server, host-agent, desktop

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
echo "${out}" | grep -q "server, host-agent, desktop" || fail "error message doesn't list valid components: ${out}"
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
[ "${PLAN_OUT}" = "$(expect_plan true false false true)" ] || fail "server-only plan stdout: ${PLAN_OUT}"
ok "ship_server=true, others false, ship_goreleaser=true"

# ---------------------------------------------------------------------------
step "update-version.sh --components desktop lands all four surfaces + both locks (0.0.x -> 0.8.0 jump)"
"${UV}" 0.8.0 --components desktop >/dev/null
[ "$(tr -d '[:space:]' < "${SCRATCH}/desktop/VERSION")" = "0.8.0" ] || fail "desktop/VERSION didn't bump"
grep -q '^version = "0.8.0"' "${SCRATCH}/crates/Cargo.toml" || fail "crates/Cargo.toml didn't bump"
grep -q '^version = "0.8.0"' "${SCRATCH}/desktop/tauri/src-tauri/Cargo.toml" || fail "tauri Cargo.toml didn't bump"
[ "$(jq -r '.version' "${SCRATCH}/desktop/tauri/src-tauri/tauri.conf.json")" = "0.8.0" ] || fail "tauri.conf.json didn't bump"
for dep in shed-core shed-app shed-broker shed-opencode shed-craze; do
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
[ "${PLAN_OUT}" = "$(expect_plan false false true false)" ] || fail "desktop-only plan stdout: ${PLAN_OUT}"
ok "ship_desktop=true only, ship_goreleaser=false"

# ---------------------------------------------------------------------------
step "update-version.sh --components go,desktop; release-plan sees a combined tag"
"${UV}" 0.8.0 --components go,desktop >/dev/null
[ "$(jq -r '.version' "${SCRATCH}/.claude-plugin/plugin.json")" = "0.8.0" ] || fail "plugin.json didn't bump to 0.8.0"
write_changelog 0.8.0 "server, desktop"
run_plan v0.8.0
[ "${PLAN_RC}" -eq 0 ] || fail "combined plan exited ${PLAN_RC}"
[ "${PLAN_OUT}" = "$(expect_plan true false true true)" ] || fail "combined plan stdout: ${PLAN_OUT}"
ok "ship_server=true ship_desktop=true ship_goreleaser=true"

# ---------------------------------------------------------------------------
step "release-plan.sh: GITHUB_REF_NAME fallback (no positional arg)"
run_plan_env_rc=0
plan_env_out="$(GITHUB_REF_NAME=v0.8.0 "${RP}" 2>/dev/null)" || run_plan_env_rc=$?
[ "${run_plan_env_rc}" -eq 0 ] || fail "GITHUB_REF_NAME fallback exited ${run_plan_env_rc}"
[ "${plan_env_out}" = "$(expect_plan true false true true)" ] || fail "GITHUB_REF_NAME fallback stdout: ${plan_env_out}"
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
# Per-component + Ships-grammar cases (the host-agent VERSION manifest +
# CHANGELOG cross-check). These MUTATE the shared scratch state; each case
# leaves the tree in a known shape for the next. Nothing here goes through
# update-version.sh's host-agent arm (that's Phase 2 below), so the VERSION file
# is written directly.
#
# Entering state: plugin.json=0.8.0, host-agent=0.0.0 (the hermetic baseline —
# never bumped above), desktop surfaces=0.8.0 (lockstep intact).
# ===========================================================================

HOST_AGENT_VER="${SCRATCH}/crates/shed-host-agent/VERSION"

# ---------------------------------------------------------------------------
step "release-plan.sh: host-agent-only tag (only crates/shed-host-agent/VERSION matches)"
printf '1.2.3\n' > "${HOST_AGENT_VER}"
write_changelog 1.2.3 "host-agent"
run_plan v1.2.3
[ "${PLAN_RC}" -eq 0 ] || fail "host-agent-only plan exited ${PLAN_RC}"
[ "${PLAN_OUT}" = "$(expect_plan false true false true)" ] || fail "host-agent-only plan stdout: ${PLAN_OUT}"
ok "ship_host_agent=true only, ship_goreleaser=true"

# The two token-rejection cases below both need a manifest matching v1.2.3 (the
# Ships grammar check only runs once a component manifest matches the tag);
# host-agent is left at 1.2.3 by the case above. Re-assert it defensively.
printf '1.2.3\n' > "${HOST_AGENT_VER}"

# ---------------------------------------------------------------------------
step "release-plan.sh: retired 'machine-rc' Ships token exits 1 naming the retirement"
write_changelog 1.2.3 "host-agent, machine-rc"
run_plan v1.2.3
[ "${PLAN_RC}" -eq 1 ] || fail "retired machine-rc token plan exited ${PLAN_RC} (want 1)"
out="$("${RP}" v1.2.3 2>&1 || true)"
echo "${out}" | grep -q "retired in plan 010" || fail "retired-token error missing the retirement message: ${out}"
ok "'machine-rc' in a Ships line rejected with the retirement message"

# ---------------------------------------------------------------------------
step "release-plan.sh: sunset 'sx' Ships token exits 1 naming the sunset (plan 016)"
# A stale muscle-memory Ships line must fail with the SUNSET reason, not as a
# generic unknown token. The entry pairs `sx` with a SURVIVING component that
# actually matches the tag — an `**Ships:** sx`-only entry on a tag no manifest
# matches would fail earlier ("matches NO component manifest") and never reach
# the token parser.
write_changelog 1.2.3 "host-agent, sx"
run_plan v1.2.3
[ "${PLAN_RC}" -eq 1 ] || fail "sunset sx token plan exited ${PLAN_RC} (want 1)"
out="$("${RP}" v1.2.3 2>&1 || true)"
echo "${out}" | grep -q "sunset in plan 016" || fail "sunset-token error missing the sunset message: ${out}"
echo "${out}" | grep -q "names 'sx'" || fail "sunset-token error doesn't name the offending token: ${out}"
echo "${out}" | grep -q "unknown token" && fail "'sx' fell through to the generic unknown-token arm: ${out}"
ok "'sx' in a Ships line rejected with the plan-016 sunset message"

# ---------------------------------------------------------------------------
step "release-plan.sh: combined server + host-agent (both manifests at 4.0.0)"
"${UV}" 4.0.0 --components go >/dev/null
printf '4.0.0\n' > "${HOST_AGENT_VER}"
write_changelog 4.0.0 "server, host-agent"
run_plan v4.0.0
[ "${PLAN_RC}" -eq 0 ] || fail "server+host-agent plan exited ${PLAN_RC}"
[ "${PLAN_OUT}" = "$(expect_plan true true false true)" ] || fail "server+host-agent plan stdout: ${PLAN_OUT}"
ok "ship_server=true ship_host_agent=true ship_goreleaser=true"

# ---------------------------------------------------------------------------
step "release-plan.sh: legacy 'server/CLI' alias accepted (server + desktop tag)"
"${UV}" 0.8.0 --components go >/dev/null
write_changelog 0.8.0 "server/CLI, desktop"
run_plan v0.8.0
[ "${PLAN_RC}" -eq 0 ] || fail "legacy-alias plan exited ${PLAN_RC}"
[ "${PLAN_OUT}" = "$(expect_plan true false true true)" ] || fail "legacy-alias plan stdout: ${PLAN_OUT}"
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
[ "${PLAN_OUT}" = "$(expect_plan false false true false)" ] || fail "desktop-only prerelease stdout: ${PLAN_OUT}"
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
# Phase 2: update-version.sh arms (host-agent / the retired machine-rc / the
# sunset sx / 3-way), the `go` deprecation alias, and the prerelease matrix.
# These run last (they mutate the shared scratch manifests; nothing after them
# depends on the state left).
# ===========================================================================

hostagent_ver() { tr -d '[:space:]' < "${HOST_AGENT_VER}"; }

# ---------------------------------------------------------------------------
step "update-version.sh --components host-agent bumps ONLY crates/shed-host-agent/VERSION"
before_desktop="$(desktop_state)"
before_plugin="$(jq -r '.version' "${SCRATCH}/.claude-plugin/plugin.json")"
"${UV}" 6.1.0 --components host-agent >/dev/null
[ "$(hostagent_ver)" = "6.1.0" ] || fail "host-agent VERSION didn't bump to 6.1.0"
[ "$(jq -r '.version' "${SCRATCH}/.claude-plugin/plugin.json")" = "${before_plugin}" ] || fail "--components host-agent touched plugin.json"
[ "$(desktop_state)" = "${before_desktop}" ] || fail "--components host-agent touched a desktop surface"
ok "host-agent VERSION=6.1.0; plugin/desktop untouched"

# ---------------------------------------------------------------------------
step "update-version.sh: sunset 'sx' component hard-errors with the plan-016 sunset message"
# The `sx` case arm rejects BEFORE any stable-only / bump logic runs, so the
# error must name plan 016 and the sunset (not read as a generic typo), exit 2,
# and leave every surviving selector byte-identical.
before_desktop="$(desktop_state)"
before_plugin="$(jq -r '.version' "${SCRATCH}/.claude-plugin/plugin.json")"
before_ha="$(hostagent_ver)"
sx_rc=0
out="$("${UV}" 6.1.5 --components sx 2>&1)" || sx_rc=$?
[ "${sx_rc}" -eq 2 ] || fail "sunset component 'sx' exited ${sx_rc} (want 2): ${out}"
echo "${out}" | grep -q "plan 016" || fail "sunset-component error doesn't name plan 016: ${out}"
echo "${out}" | grep -q "sunset" || fail "sunset-component error doesn't say 'sunset': ${out}"
echo "${out}" | grep -q "unknown component" && fail "'sx' fell through to the generic unknown-component arm: ${out}"
[ "$(hostagent_ver)" = "${before_ha}" ] || fail "rejected sx bump touched host-agent VERSION"
[ "$(jq -r '.version' "${SCRATCH}/.claude-plugin/plugin.json")" = "${before_plugin}" ] || fail "rejected sx bump touched plugin.json"
[ "$(desktop_state)" = "${before_desktop}" ] || fail "rejected sx bump touched a desktop surface"
ok "'sx' rejected (exit 2) naming plan 016 + the sunset; all three selectors byte-identical"

# ---------------------------------------------------------------------------
step "update-version.sh: retired 'machine-rc' component hard-errors with the retirement message"
before_desktop="$(desktop_state)"
before_plugin="$(jq -r '.version' "${SCRATCH}/.claude-plugin/plugin.json")"
before_ha="$(hostagent_ver)"
if out="$("${UV}" 6.2.0 --components machine-rc 2>&1)"; then
  fail "retired component 'machine-rc' was accepted"
fi
echo "${out}" | grep -q "retired in plan 010" || fail "retired-component error missing the retirement message: ${out}"
[ "$(hostagent_ver)" = "${before_ha}" ] || fail "rejected machine-rc bump touched host-agent VERSION"
[ "$(jq -r '.version' "${SCRATCH}/.claude-plugin/plugin.json")" = "${before_plugin}" ] || fail "rejected machine-rc bump touched plugin.json"
[ "$(desktop_state)" = "${before_desktop}" ] || fail "rejected machine-rc bump touched a desktop surface"
ok "machine-rc rejected with the retirement message; nothing touched"

# ---------------------------------------------------------------------------
step "update-version.sh --components server,host-agent,desktop bumps all three"
"${UV}" 6.3.0 --components server,host-agent,desktop >/dev/null
[ "$(jq -r '.version' "${SCRATCH}/.claude-plugin/plugin.json")" = "6.3.0" ] || fail "3-way: plugin.json didn't bump"
[ "$(hostagent_ver)" = "6.3.0" ] || fail "3-way: host-agent VERSION didn't bump"
[ "$(tr -d '[:space:]' < "${SCRATCH}/desktop/VERSION")" = "6.3.0" ] || fail "3-way: desktop/VERSION didn't bump"
grep -q '^version = "6.3.0"' "${SCRATCH}/crates/Cargo.toml" || fail "3-way: crates/Cargo.toml didn't bump"
ok "server + host-agent + desktop all at 6.3.0"

# ---------------------------------------------------------------------------
step "release-plan.sh: a 3-way tag ships every component"
write_changelog 6.3.0 "server, host-agent, desktop"
run_plan v6.3.0
[ "${PLAN_RC}" -eq 0 ] || fail "3-way plan exited ${PLAN_RC}"
[ "${PLAN_OUT}" = "$(expect_plan true true true true)" ] || fail "3-way plan stdout: ${PLAN_OUT}"
ok "all three ship_* true, ship_goreleaser=true"

# ---------------------------------------------------------------------------
step "update-version.sh: the 3-way bump is idempotent (byte-identical re-run)"
before="$(desktop_state)$(jq -r '.version' "${SCRATCH}/.claude-plugin/plugin.json")$(hostagent_ver)"
"${UV}" 6.3.0 --components server,host-agent,desktop >/dev/null
after="$(desktop_state)$(jq -r '.version' "${SCRATCH}/.claude-plugin/plugin.json")$(hostagent_ver)"
[ "${before}" = "${after}" ] || fail "re-running the 3-way bump changed bytes"
ok "3-way re-run is a no-op"

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
step "update-version.sh: prerelease + server,host-agent rejected; prerelease desktop-only allowed"
if "${UV}" 7.0.0-rc.1 --components server,host-agent >/dev/null 2>&1; then
  fail "prerelease server,host-agent bump was accepted"
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
# Phase 3: recommend-components.sh against a throwaway git repo with synthetic
# tagged history. Built here (not tar-copied) because the recommender walks tag history
# and diffs <lastship>..HEAD. It resolves its repo from `git rev-parse
# --show-toplevel` of the CWD, so pointing it at the fixture is just a `cd`. gh
# is forced off (RECOMMEND_NO_GH=1) — the fixture has no GitHub releases; the
# failed-tag caveat path must stay non-fatal.
#
# Synthetic history:
#   v0.9.0  plugin.json 0.9.0, desktop/VERSION 0.9.0; NO host-agent VERSION
#           file (pre-migration) → its walk FALLS BACK to this tag.
#   v0.9.1  desktop-only: desktop/VERSION 0.9.1 (plugin still 0.9.0) + a commit
#           touching ONLY crates/Cargo.toml + Cargo.lock (desktop lockfile churn).
#   HEAD    post-v0.9.1: sdk/x.go, .claude-plugin/plugin-meta.txt, desktop/ui.txt,
#           then (LAST, added mid-suite) crates/shed-broker/src/lib.rs.
#
# Every one of the three surviving components resolves a basis in this fixture,
# so nothing here touches NEVER_SHIPPED (empty since plan 016) — but every run
# still exercises the empty-array `never_shipped()` scan under `set -u`. The
# bootstrap's positive path is covered synthetically in the H3 block below.
# ===========================================================================

FIX="${SCRATCH}/reco-fixture"
mkdir -p "${FIX}"
git -C "${FIX}" init -q
gitf() { git -C "${FIX}" -c user.email=t@t -c user.name=t "$@"; }

# --- base tree + v0.9.0 (pre-migration; no host-agent VERSION) ---
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
step "recommend-components.sh: patch 0.9.2 → server (sdk+plugin), host-agent (broker), desktop (ui)"
run_reco 0.9.2
[ "${RECO_RC}" -eq 0 ] || fail "patch recommend exited ${RECO_RC}: ${RECO_ERR}"
echo "${RECO_OUT}" | grep -qx "level=patch" || fail "patch level wrong: ${RECO_OUT}"
echo "${RECO_OUT}" | grep -qx "recommended_components=server,host-agent,desktop" || fail "patch recommendation wrong: ${RECO_OUT} // ${RECO_ERR}"
echo "${RECO_ERR}" | grep -q "CAVEAT (failed-tag)" || fail "gh-skip failed-tag caveat not printed (should stay non-fatal): ${RECO_ERR}"
ok "server,host-agent,desktop (gh-skip caveat non-fatal)"

# ---------------------------------------------------------------------------
step "recommend-components.sh: last-shipped fallback (host-agent → v0.9.0 via plugin match; desktop → v0.9.1)"
echo "${RECO_ERR}" | grep -Eq '^[[:space:]]*server[[:space:]]+v0\.9\.0[[:space:]]' || fail "server last-shipped not v0.9.0: ${RECO_ERR}"
echo "${RECO_ERR}" | grep -Eq '^[[:space:]]*host-agent[[:space:]]+v0\.9\.0[[:space:]]' || fail "host-agent last-shipped not v0.9.0 (fallback): ${RECO_ERR}"
echo "${RECO_ERR}" | grep -Eq '^[[:space:]]*desktop[[:space:]]+v0\.9\.1[[:space:]]' || fail "desktop last-shipped not v0.9.1: ${RECO_ERR}"
ok "host-agent fell back to v0.9.0; desktop resolved to v0.9.1"

# ---------------------------------------------------------------------------
step "recommend-components.sh: minor 0.10.0 → ALL components regardless of diffs"
run_reco 0.10.0
[ "${RECO_RC}" -eq 0 ] || fail "minor recommend exited ${RECO_RC}: ${RECO_ERR}"
echo "${RECO_OUT}" | grep -qx "level=minor" || fail "minor level wrong: ${RECO_OUT}"
echo "${RECO_OUT}" | grep -qx "recommended_components=server,host-agent,desktop" || fail "minor recommendation wrong: ${RECO_OUT}"
ok "all three components (a minor bump ships the fleet)"

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
# (VERSION stale, plugin==tag) did NOT ship host-agent and must NOT be borrowed
# as the basis. A SEPARATE fixture (so it can't perturb the state the
# tests above thread through the shared reco-fixture).
#
#   v1.0.0  plugin=1.0.0, desktop=1.0.0, NO host-agent VERSION
#           (pre-migration → the legitimate fallback basis).
#   v1.1.0  server-only: plugin=1.1.0, and the host-agent VERSION file NOW
#           EXISTS but at a STALE 1.0.0 (migration happened; the tag
#           shipped server only). first-loop finds no VERSION==tag; the FIXED
#           fallback skips v1.1.0 (file present) and lands on v1.0.0. A fallback
#           lacking the absent-file guard would wrongly pick v1.1.0.
# ---------------------------------------------------------------------------
step "recommend-components.sh: fallback skips a post-migration server-only tag (stale VERSION present) → basis v1.0.0"
FIX2="${SCRATCH}/reco-fixture-h1"
mkdir -p "${FIX2}"
git -C "${FIX2}" init -q
gitf2() { git -C "${FIX2}" -c user.email=t@t -c user.name=t "$@"; }
mkdir -p "${FIX2}/.claude-plugin" "${FIX2}/crates/shed-host-agent" "${FIX2}/crates/shed-broker/src" "${FIX2}/desktop"
printf '{"version": "1.0.0"}\n' > "${FIX2}/.claude-plugin/plugin.json"
printf '1.0.0\n' > "${FIX2}/desktop/VERSION"
printf 'pub fn broker() {}\n' > "${FIX2}/crates/shed-broker/src/lib.rs"
gitf2 add -A; gitf2 commit -q -m 'v1.0.0 base (pre-migration: no VERSION files)'
gitf2 tag v1.0.0
# migration + server-only 1.1.0: VERSION files appear but stale at 1.0.0.
printf '{"version": "1.1.0"}\n' > "${FIX2}/.claude-plugin/plugin.json"
printf '1.0.0\n' > "${FIX2}/crates/shed-host-agent/VERSION"
gitf2 add -A; gitf2 commit -q -m 'v1.1.0 server-only (VERSION file present, stale 1.0.0)'
gitf2 tag v1.1.0
# a post-tag change so the walk has a diff to compute.
printf 'pub fn broker() { /* changed */ }\n' > "${FIX2}/crates/shed-broker/src/lib.rs"
gitf2 add -A; gitf2 commit -q -m 'post-v1.1.0 broker change'
h1_rc=0
h1_err="$(cd "${FIX2}" && RECOMMEND_NO_GH=1 "${RECO}" 1.1.1 2>&1 1>/dev/null)" || h1_rc=$?
[ "${h1_rc}" -eq 0 ] || fail "H1 recommend exited ${h1_rc}: ${h1_err}"
echo "${h1_err}" | grep -Eq '^[[:space:]]*host-agent[[:space:]]+v1\.0\.0[[:space:]]' || fail "H1: host-agent basis not v1.0.0 (fallback picked the stale-VERSION server tag?): ${h1_err}"
echo "${h1_err}" | grep -Eq '^[[:space:]]*host-agent[[:space:]]+v1\.1\.0[[:space:]]' && fail "H1: host-agent wrongly used v1.1.0 as basis: ${h1_err}"
ok "host-agent fell back to v1.0.0; the stale-VERSION v1.1.0 tag was NOT borrowed"

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

# ---------------------------------------------------------------------------
# H3 (plan 011, recast by plan 016): the NEVER_SHIPPED bootstrap has two halves,
# and ONE basis-less fixture proves both.
#
#   * POSITIVE — a component ON the list reports the basis "(never shipped)",
#     counts as changed unconditionally and is recommended, with no hard error.
#   * NEGATIVE — a component NOT on the list with no historical basis keeps
#     hard-erroring, because for an established component that state means the
#     repo/tag history is broken, not that the component is new.
#
# Since plan 016 the production list is EMPTY (its only entry, `sx`, was sunset
# before it ever shipped), so the positive half has no real component to ride.
# It is exercised synthetically instead: a COPY of recommend-components.sh with
# `NEVER_SHIPPED=()` sed-patched to `NEVER_SHIPPED=(desktop)`. The production
# script grows no test-only seam, and the two runs differ in exactly that one
# line — so the behavioral delta is attributable to the list and nothing else.
#
# The fixture: server resolves at v2.0.0, host-agent takes the pre-migration
# fallback, and desktop/VERSION (1.4.7) never equals a tag → desktop is the
# basis-less component both halves hinge on.
# ---------------------------------------------------------------------------
FIX3="${SCRATCH}/reco-fixture-h3"
mkdir -p "${FIX3}"
git -C "${FIX3}" init -q
gitf3() { git -C "${FIX3}" -c user.email=t@t -c user.name=t "$@"; }
mkdir -p "${FIX3}/.claude-plugin" "${FIX3}/desktop" "${FIX3}/crates/shed-broker/src"
printf '{"version": "2.0.0"}\n' > "${FIX3}/.claude-plugin/plugin.json"
# desktop/VERSION deliberately never equals a tag → no basis.
printf '1.4.7\n' > "${FIX3}/desktop/VERSION"
printf 'pub fn broker() {}\n' > "${FIX3}/crates/shed-broker/src/lib.rs"
gitf3 add -A; gitf3 commit -q -m 'v2.0.0 base'
gitf3 tag v2.0.0
printf 'pub fn broker() { /* changed */ }\n' > "${FIX3}/crates/shed-broker/src/lib.rs"
gitf3 add -A; gitf3 commit -q -m 'post-v2.0.0 change'

# ---------------------------------------------------------------------------
step "recommend-components.sh: a LISTED component with no basis bootstraps (synthetic NEVER_SHIPPED=(desktop))"
RECO_PATCHED="${SCRATCH}/recommend-components-never-shipped-desktop.sh"
sed 's/^NEVER_SHIPPED=()$/NEVER_SHIPPED=(desktop)/' "${RECO}" > "${RECO_PATCHED}"
chmod +x "${RECO_PATCHED}"
# Prove the patch landed — a renamed/reflowed assignment would silently turn
# this into a duplicate of the negative case below.
grep -qx 'NEVER_SHIPPED=(desktop)' "${RECO_PATCHED}" \
  || fail "H3: the NEVER_SHIPPED=() line was not sed-patched — did the assignment's shape change?"
h3p_rc=0
( cd "${FIX3}" && RECOMMEND_NO_GH=1 "${RECO_PATCHED}" 2.0.1 >"${SCRATCH}/h3p.out" 2>"${SCRATCH}/h3p.err" ) || h3p_rc=$?
h3p_err="$(cat "${SCRATCH}/h3p.err")"
h3p_out="$(cat "${SCRATCH}/h3p.out")"
[ "${h3p_rc}" -eq 0 ] || fail "H3 positive: listed basis-less 'desktop' exited ${h3p_rc}: ${h3p_err}"
echo "${h3p_err}" | grep -Eq "^[[:space:]]*desktop[[:space:]]+\\(never shipped\\)" || fail "H3 positive: desktop row doesn't show the never-shipped basis: ${h3p_err}"
echo "${h3p_err}" | grep -q "first ship — no diff basis" || fail "H3 positive: desktop row missing the first-ship sample text: ${h3p_err}"
echo "${h3p_err}" | grep -q "NOTE (first ship)" || fail "H3 positive: the first-ship prune note was not printed: ${h3p_err}"
echo "${h3p_err}" | grep -q "no historical basis" && fail "H3 positive: the listed component hard-errored instead of bootstrapping: ${h3p_err}"
echo "${h3p_out}" | grep -Eq '^recommended_components=.*desktop' || fail "H3 positive: desktop was not recommended: ${h3p_out}"
ok "listed 'desktop' bootstraps: basis '(never shipped)', changed=yes, recommended, prune note printed"

# ---------------------------------------------------------------------------
step "recommend-components.sh: an UNLISTED component with no basis still hard-errors (the carve-out is narrow)"
h3_rc=0
h3_err="$(cd "${FIX3}" && RECOMMEND_NO_GH=1 "${RECO}" 2.0.1 2>&1 1>/dev/null)" || h3_rc=$?
[ "${h3_rc}" -eq 1 ] || fail "H3: basis-less 'desktop' didn't hard-error (got ${h3_rc}): ${h3_err}"
echo "${h3_err}" | grep -q "no historical basis for component 'desktop'" || fail "H3: error doesn't name desktop: ${h3_err}"
ok "desktop (absent from the EMPTY production NEVER_SHIPPED) still exits 1 with 'no historical basis'"

# ---------------------------------------------------------------------------
# H4: the desktop path list must cover EVERY crate the Tauri app links.
#
# `desktop/tauri/src-tauri/Cargo.toml` takes `shed-opencode` as a plain
# path-dep, so its source IS in the shipped DMG/.deb — but PATHS_DESKTOP used
# to list neither it nor `shed-gx` (plan 017's second adapter, retired in plan
# 025 C1 — shed#390), so a correctness fix confined to an adapter recommended
# NO component at all on a patch tag and the desktop shipped stale. A
# SEPARATE fixture (the shared one is spent: H2 wrote an rc into its
# desktop/VERSION).
#
#   v3.0.0  plugin=3.0.0, desktop=3.0.0, NO host-agent VERSION (pre-migration →
#           its walk falls back to this tag, so all three resolve a basis and a
#           mis-attributed diff would show up as a WRONG component, not a crash).
#   HEAD    exactly one commit, touching ONLY one adapter's crate (each in turn).
#
# The loop is over both agent-lane adapters the Tauri app links — shed-opencode
# and, since plan 025 C9, shed-craze (gx's own commit was removed with it) —
# so each re-proves the same genericity by being ADDED to this list, not by a
# second copy of the fixture. The assertion is desktop AND ONLY desktop: no
# adapter reaches the host-agent binary (shed-broker has never linked one), so
# a lazy "add it everywhere" fix is red here too.
# ---------------------------------------------------------------------------
FIX4="${SCRATCH}/reco-fixture-h4"
mkdir -p "${FIX4}"
git -C "${FIX4}" init -q
gitf4() { git -C "${FIX4}" -c user.email=t@t -c user.name=t "$@"; }
mkdir -p "${FIX4}/.claude-plugin" "${FIX4}/desktop" "${FIX4}/crates/shed-opencode/src" \
  "${FIX4}/crates/shed-craze/src"
printf '{"version": "3.0.0"}\n' > "${FIX4}/.claude-plugin/plugin.json"
printf '3.0.0\n' > "${FIX4}/desktop/VERSION"
printf 'pub fn fold() {}\n' > "${FIX4}/crates/shed-opencode/src/fold.rs"
printf 'pub fn fold() {}\n' > "${FIX4}/crates/shed-craze/src/fold.rs"
gitf4 add -A; gitf4 commit -q -m 'v3.0.0 base'
gitf4 tag v3.0.0

run_reco4() {
  RECO_RC=0
  RECO_OUT="$(cd "${FIX4}" && RECOMMEND_NO_GH=1 "${RECO}" 3.0.1 2>"${SCRATCH}/reco4.err")" || RECO_RC=$?
  RECO_ERR="$(cat "${SCRATCH}/reco4.err")"
}

for adapter in shed-opencode shed-craze; do
  step "recommend-components.sh: a ${adapter}-only change flags desktop (the Tauri app links it) and nothing else"
  printf 'pub fn fold() { /* changed */ }\n' > "${FIX4}/crates/${adapter}/src/fold.rs"
  gitf4 add -A; gitf4 commit -q -m "desktop: ${adapter} change"
  run_reco4
  [ "${RECO_RC}" -eq 0 ] || fail "H4 ${adapter}: recommend exited ${RECO_RC}: ${RECO_ERR}"
  echo "${RECO_OUT}" | grep -qx "level=patch" || fail "H4 ${adapter}: level wrong: ${RECO_OUT}"
  echo "${RECO_OUT}" | grep -qx "recommended_components=desktop" \
    || fail "H4 ${adapter}: expected exactly 'desktop' — is crates/${adapter} missing from PATHS_DESKTOP (or wrongly in another list)? got: ${RECO_OUT} // ${RECO_ERR}"
  echo "${RECO_ERR}" | grep -q "NO component changed" \
    && fail "H4 ${adapter}: the recommender saw no change at all — crates/${adapter} is in NO path list: ${RECO_ERR}"
  ok "${adapter}-only change → recommended_components=desktop"
  # Restore so the NEXT adapter's commit is the only diff since v3.0.0.
  printf 'pub fn fold() {}\n' > "${FIX4}/crates/${adapter}/src/fold.rs"
  gitf4 add -A; gitf4 commit -q -m "revert ${adapter} change"
done

# ===========================================================================
# Phase 4: scripts/release/check-craze-pin.sh's release-time check (plan 025
# §3.5/O4), exercised through release-plan.sh's ship_server call-out. Every
# PRECEDING ship_server=true case above ran against the DEFAULT matching
# fake wired in just after SCRATCH was built. This phase deliberately
# mutates that state in its own FRESH ship_server=true tag (8.8.8, unused
# above) so it can't perturb any earlier case, and nothing below depends on
# what it leaves behind.
# ===========================================================================

calls_count() { wc -l < "${CRAZE_FAKE_CALL_LOG}" | tr -d ' '; }

READ_PIN="${SCRATCH}/scripts/release/read-craze-pin.sh"

# ---------------------------------------------------------------------------
# read-craze-pin.sh's own literal-value contract: a CRLF line ending or
# trailing space/tab in a value must fail validation, never be silently
# trimmed/normalized — the exact regression a sourced `. craze-pin.env`
# used to hide (and an earlier version of this parser reintroduced by
# trimming before matching). Written directly as files rather than via
# write_changelog/run_plan — this is read-craze-pin.sh's own unit contract,
# independent of release-plan.sh's integration.
# ---------------------------------------------------------------------------

step "read-craze-pin.sh: a CRLF-terminated CRAZE_TEST_SHA line is rejected, not silently normalized"
CRLF_PIN="${SCRATCH}/crlf-pin.env"
printf 'CRAZE_TEST_SHA=%s\r\nCRAZE_RELEASE=\r\n' "${FAKE_CRAZE_SHA}" > "${CRLF_PIN}"
crlf_rc=0
crlf_out="$("${READ_PIN}" "${CRLF_PIN}" 2>&1 1>/dev/null)" || crlf_rc=$?
[ "${crlf_rc}" -eq 1 ] || fail "CRLF pin file accepted (rc=${crlf_rc}, want 1): ${crlf_out}"
echo "${crlf_out}" | grep -q "not EXACTLY 40 lowercase hex" || fail "CRLF pin error doesn't name the literal-shape violation: ${crlf_out}"
echo "${crlf_out}" | grep -q "^::error::${CRLF_PIN}:1:" || fail "CRLF pin error doesn't cite line 1: ${crlf_out}"
ok "a CRLF-terminated value fails validation, citing the offending line"

step "read-craze-pin.sh: a trailing space on CRAZE_TEST_SHA and a trailing tab on CRAZE_RELEASE are both rejected"
TRAIL_PIN="${SCRATCH}/trailing-ws-pin.env"
printf 'CRAZE_TEST_SHA=%s   \nCRAZE_RELEASE=1.2.3\t\n' "${FAKE_CRAZE_SHA}" > "${TRAIL_PIN}"
trail_rc=0
trail_out="$("${READ_PIN}" "${TRAIL_PIN}" 2>&1 1>/dev/null)" || trail_rc=$?
[ "${trail_rc}" -eq 1 ] || fail "trailing-whitespace pin file accepted (rc=${trail_rc}, want 1): ${trail_out}"
echo "${trail_out}" | grep -q "^::error::${TRAIL_PIN}:1:" || fail "trailing-space error doesn't cite line 1 (CRAZE_TEST_SHA): ${trail_out}"
echo "${trail_out}" | grep -q "not EXACTLY 40 lowercase hex" || fail "trailing-space error doesn't name the literal-shape violation: ${trail_out}"
ok "trailing space/tab on either value is rejected (CRAZE_TEST_SHA checked first, per line order)"

step "read-craze-pin.sh: an indented assignment line is an unexpected line, not a lenient match"
INDENT_PIN="${SCRATCH}/indented-pin.env"
printf '  CRAZE_TEST_SHA=%s\nCRAZE_RELEASE=\n' "${FAKE_CRAZE_SHA}" > "${INDENT_PIN}"
indent_rc=0
indent_out="$("${READ_PIN}" "${INDENT_PIN}" 2>&1 1>/dev/null)" || indent_rc=$?
[ "${indent_rc}" -eq 1 ] || fail "indented-assignment pin file accepted (rc=${indent_rc}, want 1): ${indent_out}"
echo "${indent_out}" | grep -q "unexpected line" || fail "indented-assignment error doesn't say 'unexpected line': ${indent_out}"
ok "an indented CRAZE_TEST_SHA= line is refused as an unexpected line (must start at column 1)"

step "read-craze-pin.sh: comment and blank lines (including a CRLF blank line) still skip cleanly around a valid pin"
GOOD_PIN="${SCRATCH}/good-with-comments-pin.env"
printf '# a comment\r\n   \r\n\nCRAZE_TEST_SHA=%s\n  # an indented comment\nCRAZE_RELEASE=1.2.3\n' "${FAKE_CRAZE_SHA}" > "${GOOD_PIN}"
good_rc=0
good_out="$("${READ_PIN}" "${GOOD_PIN}")" || good_rc=$?
[ "${good_rc}" -eq 0 ] || fail "a well-formed pin with comments/blank lines (one CRLF-blank) was rejected: ${good_out}"
[ "${good_out}" = "$(printf 'CRAZE_TEST_SHA=%s\nCRAZE_RELEASE=1.2.3' "${FAKE_CRAZE_SHA}")" ] || fail "well-formed pin output mismatch: ${good_out}"
ok "comments (plain and indented) and blank/CRLF-blank lines are skipped; the two real values parse cleanly"

step "release-plan.sh: craze release check PASSES when the resolved commit matches CRAZE_TEST_SHA (non-vacuous: the checker actually ran)"
"${UV}" 8.8.8 --components go >/dev/null
write_changelog 8.8.8 "server"
cat > "${CRAZE_PIN_FILE}" <<EOF
CRAZE_TEST_SHA=${FAKE_CRAZE_SHA}
CRAZE_RELEASE=1.2.3
EOF
calls_before="$(calls_count)"
match_rc=0
match_out="$("${RP}" v8.8.8 2>"${SCRATCH}/craze-match.err")" || match_rc=$?
match_err="$(cat "${SCRATCH}/craze-match.err")"
calls_after="$(calls_count)"
[ "${match_rc}" -eq 0 ] || fail "craze-match plan exited ${match_rc} (want 0): ${match_err}"
[ "${match_out}" = "$(expect_plan true false false true)" ] || fail "craze-match plan stdout: ${match_out}"
# Non-vacuous: a no-op integration (the call removed entirely) would ALSO
# exit 0 with the same stdout, so assert the checker's own success line
# reached stderr AND that the ls-remote fake was actually invoked (the call
# log grew) — proof the checker really ran, not just that nothing failed.
echo "${match_err}" | grep -q "craze release check OK" || fail "craze-match: checker's OK line missing from stderr (did it actually run?): ${match_err}"
[ "${calls_after}" -gt "${calls_before}" ] || fail "craze-match: the ls-remote fake was never invoked (checker didn't run) — before=${calls_before} after=${calls_after}"
ok "a resolved commit matching CRAZE_TEST_SHA plans cleanly, AND the checker provably ran (OK line + fake invocation observed)"

step "release-plan.sh: craze release check FAILS when the resolved commit mismatches CRAZE_TEST_SHA"
craze_mismatch_rc=0
craze_mismatch_out="$(CRAZE_FAKE_RESOLVED_SHA="${OTHER_FAKE_SHA}" "${RP}" v8.8.8 2>&1 1>/dev/null)" || craze_mismatch_rc=$?
[ "${craze_mismatch_rc}" -eq 1 ] || fail "craze-mismatch plan exited ${craze_mismatch_rc} (want 1)"
echo "${craze_mismatch_out}" | grep -q "drifted apart" || fail "craze-mismatch error missing 'drifted apart': ${craze_mismatch_out}"
ok "a resolved commit disagreeing with CRAZE_TEST_SHA fails the stable server-shipping plan"

step "release-plan.sh: craze release check FAILS when CRAZE_RELEASE is empty"
cat > "${CRAZE_PIN_FILE}" <<EOF
CRAZE_TEST_SHA=${FAKE_CRAZE_SHA}
CRAZE_RELEASE=
EOF
craze_empty_rc=0
craze_empty_out="$("${RP}" v8.8.8 2>&1 1>/dev/null)" || craze_empty_rc=$?
[ "${craze_empty_rc}" -eq 1 ] || fail "craze-empty plan exited ${craze_empty_rc} (want 1)"
echo "${craze_empty_out}" | grep -q "craze release is empty" || fail "craze-empty error missing 'craze release is empty': ${craze_empty_out}"
ok "an empty CRAZE_RELEASE fails a stable server-shipping plan (RELEASING.md's stated consequence)"

# Restore a matching pin AND re-write the v8.8.8 CHANGELOG section — the
# scope cases below call write_changelog for OTHER tags (8.8.9, 8.8.10),
# and write_changelog REPLACES the whole scratch CHANGELOG.md file each
# time, so v8.8.8's section (and thus release-plan.sh's reason to reach the
# craze check at all) would otherwise be gone by the time the seam tests
# below run. The seam tests run BEFORE the scope tests for the same reason:
# keeping v8.8.8 the LAST tag written to the scratch CHANGELOG before they
# fire means a seam-ignored case's exit 1 can only come from the craze
# check itself, never from an unrelated "no CHANGELOG entry" failure
# upstream of it (that exact vacuous-pass trap was caught live while
# writing this test — see live-c6-controls.txt).
cat > "${CRAZE_PIN_FILE}" <<EOF
CRAZE_TEST_SHA=${FAKE_CRAZE_SHA}
CRAZE_RELEASE=1.2.3
EOF
write_changelog 8.8.8 "server"

# ---------------------------------------------------------------------------
# A hermetic stand-in for the real charliek/craze remote, for the two
# seam-negative controls below. Those must prove the REAL `git ls-remote`
# code path ran (not the fake) WITHOUT touching the network and WITHOUT
# modifying check-craze-pin.sh's hardcoded GitHub URL (nor assuming that URL
# will never grow a real v1.2.3 tag). git's own URL-rewrite config
# (`url.<base>.insteadOf`, set via GIT_CONFIG_COUNT/KEY_0/VALUE_0, git
# >=2.31) transparently redirects that EXACT URL string to a local repo —
# scoped to just these two invocations' environment, so nothing else in
# this file is affected.
# ---------------------------------------------------------------------------
LOCAL_CRAZE_REMOTE="${SCRATCH}/local-craze-remote"
mkdir -p "${LOCAL_CRAZE_REMOTE}"
git -C "${LOCAL_CRAZE_REMOTE}" init -q
git -C "${LOCAL_CRAZE_REMOTE}" -c user.email=t@t -c user.name=t commit -q --allow-empty -m "hermetic stand-in for charliek/craze v1.2.3"
LOCAL_CRAZE_COMMIT="$(git -C "${LOCAL_CRAZE_REMOTE}" rev-parse HEAD)"
git -C "${LOCAL_CRAZE_REMOTE}" tag v1.2.3

step "release-plan.sh: the CRAZE_PIN_LS_REMOTE seam is IGNORED when SHED_RELEASE_SELFTEST is unset/empty (never a bypass)"
# CRAZE_PIN_LS_REMOTE still points at the matching fake, but
# SHED_RELEASE_SELFTEST is cleared for this one call — check-craze-pin.sh
# must fall through to the REAL `git ls-remote` code path, which (via the
# insteadOf redirect) resolves v1.2.3 against the LOCAL stand-in repo, to
# LOCAL_CRAZE_COMMIT — a real git sha that is NOT CRAZE_TEST_SHA (the fake
# sha), so this is reported as a mismatch (reusing the same "drifted apart"
# wording the mismatch case above already asserts). If the
# SHED_RELEASE_SELFTEST=1 gate is ever dropped, this case goes green instead
# (the fake always matches CRAZE_TEST_SHA) — that is the negative control.
# Asserting the SPECIFIC local commit (not just rc==1, and not just "drifted
# apart") is load-bearing: an unrelated upstream failure also exits 1 with
# some message, and must not be mistaken for the seam actually being
# rejected — only the real resolver can have produced THIS sha.
craze_seam_rc=0
craze_seam_out="$(SHED_RELEASE_SELFTEST= GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0="url.${LOCAL_CRAZE_REMOTE}.insteadOf" GIT_CONFIG_VALUE_0="https://github.com/charliek/craze" "${RP}" v8.8.8 2>&1 1>/dev/null)" || craze_seam_rc=$?
[ "${craze_seam_rc}" -eq 1 ] || fail "craze-seam-ignored plan exited ${craze_seam_rc} (want 1 — the fake must not be honoured): ${craze_seam_out}"
echo "${craze_seam_out}" | grep -q "resolves to ${LOCAL_CRAZE_COMMIT}" || fail "craze-seam-ignored: expected the REAL (locally-redirected) ls-remote to report the stand-in's commit ${LOCAL_CRAZE_COMMIT}, got: ${craze_seam_out}"
echo "${craze_seam_out}" | grep -q "drifted apart" || fail "craze-seam-ignored: expected the mismatch wording, got: ${craze_seam_out}"
ok "CRAZE_PIN_LS_REMOTE is ignored when SHED_RELEASE_SELFTEST is unset/empty; the real (locally-redirected) resolver ran, offline, and reported the stand-in's commit"

step "release-plan.sh: the CRAZE_PIN_LS_REMOTE seam is IGNORED when SHED_RELEASE_SELFTEST=0 (strict '=1', not merely non-empty)"
# Distinct from the unset/empty case above: an implementation that checks
# `[ -n "${SHED_RELEASE_SELFTEST:-}" ]` instead of the strict `= "1"` would
# treat "0" as enabled too and wrongly honour the fake here. Same local
# stand-in redirect, same specific-commit assertion.
craze_seam0_rc=0
craze_seam0_out="$(SHED_RELEASE_SELFTEST=0 GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0="url.${LOCAL_CRAZE_REMOTE}.insteadOf" GIT_CONFIG_VALUE_0="https://github.com/charliek/craze" "${RP}" v8.8.8 2>&1 1>/dev/null)" || craze_seam0_rc=$?
[ "${craze_seam0_rc}" -eq 1 ] || fail "craze-seam-zero plan exited ${craze_seam0_rc} (want 1 — SHED_RELEASE_SELFTEST=0 must not enable the fake): ${craze_seam0_out}"
echo "${craze_seam0_out}" | grep -q "resolves to ${LOCAL_CRAZE_COMMIT}" || fail "craze-seam-zero: expected the REAL (locally-redirected) ls-remote to report the stand-in's commit ${LOCAL_CRAZE_COMMIT}, got: ${craze_seam0_out}"
echo "${craze_seam0_out}" | grep -q "drifted apart" || fail "craze-seam-zero: expected the mismatch wording, got: ${craze_seam0_out}"
ok "CRAZE_PIN_LS_REMOTE is ignored when SHED_RELEASE_SELFTEST=0 (strict equality, not a truthy/non-empty check); offline via the local redirect"

# ---------------------------------------------------------------------------
# Scope: the always-matching fake above proves the checker behaves correctly
# WHEN CALLED, but it would also silently paper over a regression that calls
# it when it must NOT be called (e.g. the `[ "${SHIP_SERVER}" = "true" ]`
# guard in release-plan.sh accidentally dropped) — a desktop-only or
# host-agent-only tag would then get a free matching result instead of
# failing loudly. These two cases assert the ls-remote fake's call log is
# UNCHANGED, i.e. the checker was never invoked at all, for stable tags that
# do NOT ship `server`. Fresh tags (8.8.9, 8.8.10) unused elsewhere in this
# file, so they can't collide with any other case's manifest state. They run
# LAST because each write_changelog call replaces the scratch CHANGELOG.md
# wholesale — nothing after this point still needs v8.8.8's section.
# ---------------------------------------------------------------------------

step "release-plan.sh: a desktop-only stable tag NEVER invokes the craze checker (SHIP_SERVER=false)"
"${UV}" 8.8.9 --components desktop >/dev/null
write_changelog 8.8.9 "desktop"
calls_before="$(calls_count)"
run_plan v8.8.9
calls_after="$(calls_count)"
[ "${PLAN_RC}" -eq 0 ] || fail "desktop-only scope-case plan exited ${PLAN_RC} (want 0)"
[ "${PLAN_OUT}" = "$(expect_plan false false true false)" ] || fail "desktop-only scope-case plan stdout: ${PLAN_OUT}"
[ "${calls_before}" -eq "${calls_after}" ] || fail "desktop-only tag invoked the craze ls-remote fake (the checker ran when SHIP_SERVER=false) — calls before=${calls_before} after=${calls_after}"
ok "a desktop-only stable tag plans cleanly and never touches the craze checker (call log unchanged)"

step "release-plan.sh: a host-agent-only stable tag NEVER invokes the craze checker (SHIP_SERVER=false)"
printf '8.8.10\n' > "${HOST_AGENT_VER}"
write_changelog 8.8.10 "host-agent"
calls_before="$(calls_count)"
run_plan v8.8.10
calls_after="$(calls_count)"
[ "${PLAN_RC}" -eq 0 ] || fail "host-agent-only scope-case plan exited ${PLAN_RC} (want 0)"
[ "${PLAN_OUT}" = "$(expect_plan false true false true)" ] || fail "host-agent-only scope-case plan stdout: ${PLAN_OUT}"
[ "${calls_before}" -eq "${calls_after}" ] || fail "host-agent-only tag invoked the craze ls-remote fake (the checker ran when SHIP_SERVER=false) — calls before=${calls_before} after=${calls_after}"
ok "a host-agent-only stable tag plans cleanly and never touches the craze checker (call log unchanged)"

# ===========================================================================
# Phase 5 (plan 026 §3.11): the desktop signing gate and the curated release
# body. desktop-signing-gate.sh and changelog-section.sh run as deployed (the
# scratch copies of the production scripts); the wiring guard parses the
# scratch copy of the REAL publish-images.yaml. Nothing here depends on the
# manifest state the phases above leave; the changelog cells write their own
# scratch CHANGELOG.md.
# ===========================================================================

GATE="${SCRATCH}/scripts/release/desktop-signing-gate.sh"
# The six presence flags, in the order the gate names them (CAN_NOTARIZE's).
GATE_SECRETS="MACOS_CERTIFICATE_P12_BASE64 MACOS_CERTIFICATE_PASSWORD SHED_DESKTOP_DEVELOPER_ID_IDENTITY APPLE_ID APPLE_TEAM_ID APPLE_APP_SPECIFIC_PASSWORD"
GATE_REFUSAL="::error::a stable desktop tag requires Developer ID signing and notarization; missing:"

# gate_flags "<names to mark false>" → GATE_FLAGS, all six `--has NAME=…` pairs.
GATE_FLAGS=()
gate_flags() {
  GATE_FLAGS=()
  local n m v
  for n in ${GATE_SECRETS}; do
    v=true
    for m in $1; do
      if [ "${m}" = "${n}" ]; then v=false; fi
    done
    GATE_FLAGS+=(--has "${n}=${v}")
  done
}
# run_gate <args...> → GATE_OUT (stdout+stderr), GATE_RC.
GATE_OUT=""; GATE_RC=0
run_gate() {
  GATE_RC=0
  GATE_OUT="$("${GATE}" "$@" 2>&1)" || GATE_RC=$?
}
# "A, B, C" from "A B C".
comma_list() { printf '%s' "$1" | sed -E 's/ +/, /g'; }

for n in ${GATE_SECRETS}; do
  step "desktop-signing-gate.sh: a stable tag missing only ${n} exits 1, naming exactly it"
  gate_flags "${n}"
  run_gate v9.1.0 "${GATE_FLAGS[@]}"
  [ "${GATE_RC}" -eq 1 ] || fail "stable tag missing ${n} exited ${GATE_RC} (want 1): ${GATE_OUT}"
  printf '%s\n' "${GATE_OUT}" | grep -qxF "${GATE_REFUSAL} ${n}" \
    || fail "stable tag missing ${n}: the refusal does not name exactly ${n}: ${GATE_OUT}"
  ok "missing ${n} → exit 1, \"missing: ${n}\" and nothing else"
done

step "desktop-signing-gate.sh: a stable tag with all six present exits 0"
gate_flags ""
run_gate v9.1.0 "${GATE_FLAGS[@]}"
[ "${GATE_RC}" -eq 0 ] || fail "stable tag with all six exited ${GATE_RC} (want 0): ${GATE_OUT}"
echo "${GATE_OUT}" | grep -q "is stable and all six Apple secrets are present" || fail "all-present pass line missing: ${GATE_OUT}"
echo "${GATE_OUT}" | grep -q "missing:" && fail "all-present run still reports something missing: ${GATE_OUT}"
ok "all six present → exit 0"

step "desktop-signing-gate.sh: a stable tag missing all six exits 1, naming all six"
gate_flags "${GATE_SECRETS}"
run_gate v9.1.0 "${GATE_FLAGS[@]}"
[ "${GATE_RC}" -eq 1 ] || fail "stable tag missing all six exited ${GATE_RC} (want 1): ${GATE_OUT}"
printf '%s\n' "${GATE_OUT}" | grep -qxF "${GATE_REFUSAL} $(comma_list "${GATE_SECRETS}")" \
  || fail "the all-missing refusal does not name all six in order: ${GATE_OUT}"
ok "all six missing → exit 1, all six named"

step "desktop-signing-gate.sh: a prerelease missing all six exits 0 with the ad-hoc note"
gate_flags "${GATE_SECRETS}"
run_gate v9.1.0-rc.1 "${GATE_FLAGS[@]}"
[ "${GATE_RC}" -eq 0 ] || fail "prerelease missing all six exited ${GATE_RC} (want 0): ${GATE_OUT}"
echo "${GATE_OUT}" | grep -qF "is a prerelease, so an ad-hoc-signed, un-notarized Mac build is allowed (missing: $(comma_list "${GATE_SECRETS}"))" \
  || fail "prerelease ad-hoc note missing or incomplete: ${GATE_OUT}"
echo "${GATE_OUT}" | grep -qF "::error::" && fail "a prerelease printed an error annotation: ${GATE_OUT}"
ok "prerelease (v9.1.0-rc.1), all six missing → exit 0 with the ad-hoc note"

step "desktop-signing-gate.sh: malformed flags and tags exit 2"
# Each case: a label, then the whole argv (the tag first, where there is one),
# built from the six valid flags with one change.
gate_flags ""
malformed_case() {
  # $1 label, rest = full argv
  local label="$1"; shift
  run_gate "$@"
  [ "${GATE_RC}" -eq 2 ] || fail "malformed (${label}) exited ${GATE_RC} (want 2): ${GATE_OUT}"
  ok "malformed: ${label} → exit 2"
}
malformed_case "a value that is not true|false" v9.1.0 "${GATE_FLAGS[@]:0:10}" --has APPLE_APP_SPECIFIC_PASSWORD=yes
malformed_case "a flag with no =" v9.1.0 "${GATE_FLAGS[@]:0:10}" --has APPLE_APP_SPECIFIC_PASSWORD
malformed_case "an unknown secret name" v9.1.0 "${GATE_FLAGS[@]}" --has SPARKLE_ED_PRIVATE_KEY=true
malformed_case "a name given twice" v9.1.0 "${GATE_FLAGS[@]}" --has APPLE_ID=true
malformed_case "a dangling --has" v9.1.0 "${GATE_FLAGS[@]}" --has
malformed_case "a bare argument" v9.1.0 "${GATE_FLAGS[@]}" APPLE_ID=true
malformed_case "a tag that is not vX.Y.Z" 9.1 "${GATE_FLAGS[@]}"
malformed_case "no arguments at all"
run_gate v9.1.0 "${GATE_FLAGS[@]:0:10}"
[ "${GATE_RC}" -eq 2 ] || fail "malformed (only five flags) exited ${GATE_RC} (want 2): ${GATE_OUT}"
echo "${GATE_OUT}" | grep -q "no --has flag for: APPLE_APP_SPECIFIC_PASSWORD" || fail "five-flag refusal doesn't name the absent flag: ${GATE_OUT}"
ok "malformed: only five of the six flags → exit 2, naming the absent one"

step "desktop-signing-gate.sh: a secret-shaped value is never echoed, wherever it is wired"
# A distinctive sentinel stands in for a secret value wired in by mistake
# (e.g. `--has APPLE_ID=${{ secrets.APPLE_ID }}`). Built at runtime.
SENTINEL="sentinel-$(printf 'q%.0s' $(seq 1 12))-$$"
sentinel_case() {
  # $1 label, $2 expected rc, rest = argv
  local label="$1" want="$2"; shift 2
  run_gate "$@"
  [ "${GATE_RC}" -eq "${want}" ] || fail "sentinel (${label}) exited ${GATE_RC} (want ${want}): ${GATE_OUT}"
  case "${GATE_OUT}" in
    *"${SENTINEL}"*) fail "sentinel (${label}): the output echoed the value: ${GATE_OUT}" ;;
  esac
  ok "sentinel ${label}: exit ${want}, value never echoed"
}
sentinel_case "as a flag value" 2 v9.1.0 "${GATE_FLAGS[@]:0:6}" --has "APPLE_ID=${SENTINEL}" "${GATE_FLAGS[@]:8:4}"
sentinel_case "as a flag name" 2 v9.1.0 "${GATE_FLAGS[@]}" --has "${SENTINEL}=true"
sentinel_case "as a bare argument" 2 v9.1.0 "${GATE_FLAGS[@]}" "${SENTINEL}"
sentinel_case "as the tag" 2 "${SENTINEL}" "${GATE_FLAGS[@]}"
# The gate reads no environment: the six names exported with the sentinel
# change nothing (all six flags false → the normal refusal, no echo).
gate_flags "${GATE_SECRETS}"
GATE_RC=0
GATE_OUT="$(MACOS_CERTIFICATE_P12_BASE64="${SENTINEL}" MACOS_CERTIFICATE_PASSWORD="${SENTINEL}" \
  SHED_DESKTOP_DEVELOPER_ID_IDENTITY="${SENTINEL}" APPLE_ID="${SENTINEL}" APPLE_TEAM_ID="${SENTINEL}" \
  APPLE_APP_SPECIFIC_PASSWORD="${SENTINEL}" "${GATE}" v9.1.0 "${GATE_FLAGS[@]}" 2>&1)" || GATE_RC=$?
[ "${GATE_RC}" -eq 1 ] || fail "sentinel (in the environment) exited ${GATE_RC} (want 1): ${GATE_OUT}"
case "${GATE_OUT}" in *"${SENTINEL}"*) fail "sentinel (in the environment): echoed: ${GATE_OUT}" ;; esac
ok "sentinel in the six env vars: ignored (exit 1 from the flags alone), never echoed"

# ---------------------------------------------------------------------------
# The wiring guard: publish-images.yaml must CALL the gate (testing a copied
# snippet would not prove that). One python3 helper, written to its own dir:
#   check <workflow> <gate-script>  → exit 0, or 1 listing every violation
#   mutate <in> <out> <mutation>    → a structurally mutated copy (as JSON,
#                                     which both loaders read) for the
#                                     negative cells below
# ---------------------------------------------------------------------------
WIRING_DIR="${SCRATCH}/wiring"
mkdir -p "${WIRING_DIR}"
WIRING="${WIRING_DIR}/wiring.py"
cat > "${WIRING}" <<'PY'
import json
import os
import re
import shutil
import subprocess
import sys

GATE = "scripts/release/desktop-signing-gate.sh"
TAG_EXPR = "github.event.inputs.version || github.ref_name"
RP_IF = "steps.plan.outputs.ship_desktop == 'true'"
# One `--has NAME=${{ secrets.NAME != '' }}` flag: FLAG captures both names,
# FLAG_NC is the same pattern without groups, for the whole-line match.
FLAG = r"--has ([A-Z0-9_]+)=\$\{\{ secrets\.([A-Z0-9_]+) != '' \}\}"
FLAG_NC = r"--has [A-Z0-9_]+=\$\{\{ secrets\.[A-Z0-9_]+ != '' \}\}"
LINE_RE = re.compile(re.escape(GATE) + r' "\$\{TAG\}"((?: ' + FLAG_NC + r")+)")


def load(path):
    want = os.environ.get("RELEASE_SELFTEST_YAML_LOADER", "auto")
    if want not in ("auto", "pyyaml", "yq"):
        sys.exit("wiring: RELEASE_SELFTEST_YAML_LOADER must be auto, pyyaml or yq, not %r" % want)
    if want in ("auto", "pyyaml"):
        try:
            import yaml
        except ImportError:
            if want == "pyyaml":
                sys.exit("wiring: RELEASE_SELFTEST_YAML_LOADER=pyyaml, but PyYAML does not import")
        else:
            with open(path) as f:
                return yaml.safe_load(f), "PyYAML"
    yq = shutil.which("yq")
    if not yq:
        sys.exit("wiring: parsing the workflow needs python3 with PyYAML, or yq (mikefarah v4)")
    out = subprocess.run([yq, "-o=json", ".", path], check=True, capture_output=True, text=True).stdout
    return json.loads(out), "yq"


def expr(value):
    """An `if:`/env value with any ${{ }} wrapper removed and whitespace collapsed."""
    e = " ".join(str(value).split())
    m = re.fullmatch(r"\$\{\{ ?(.*?) ?\}\}", e)
    return m.group(1) if m else e


def gate_lines(step):
    """Lines of the step's run that execute the gate, continuations joined."""
    run = re.sub(r"[ \t]*\\\n[ \t]*", " ", step.get("run") or "")
    return [l.strip() for l in run.splitlines() if l.strip().startswith(GATE + " ")]


def gate_indices(steps):
    return [i for i, s in enumerate(steps) if gate_lines(s)]


OPENERS = ("if", "case", "for", "while", "until", "select", "{", "(")
CLOSERS = ("fi", "esac", "done", "}", ")")
# The gate called behind a control operator on the same line (`true || gate`,
# `false && gate`, `x; gate`, `x | gate`): a call that may never run, or whose
# status is lost. The `[ ! -f <gate> ]` test and the messages naming the gate
# never put it right after one of these.
BEHIND_OPERATOR = re.compile(r"(\|\||&&|;|\|)\s*\"?" + re.escape(GATE) + r"\b")
SET_PLUS_E = re.compile(r"(?m)^\s*set\s+(?:-\w*\s+)*\+\w*e|^\s*set\s+\+o\s+errexit\b")


def unreachable(step):
    """Why the gate line may not run as a top-level command, or None.

    The rule fits these steps' shape: the invocation line BEGINS with the gate
    (column 0, no prefix), the previous non-blank line does not end with an
    operator or a continuation that would make it the tail of another command
    (`||`, `&&`, `|`, `\\`), every if/case/loop/block opened in column 0 before
    it is closed again, no column-0 exit/return/exec comes first, and nothing
    in the step turns errexit off (`set +e`). Indented lines are bodies of such
    blocks, and continuation lines belong to the line they continue.

    It targets ACCIDENTAL restructuring. Deliberate obfuscation (the gate inside
    a function that is never called, or inside a here-doc) is out of scope: the
    execution cells, which run each step's real script, catch any change that
    stops the gate from running at all.
    """
    run = step.get("run") or ""
    if SET_PLUS_E.search(run):
        return "the step turns errexit off (`set +e`), so a refusal could be ignored"
    if BEHIND_OPERATOR.search(run):
        return "the gate is called behind `||`/`&&`/`;`/`|` on its line, not at the start of a command"
    depth, cont, prev = 0, False, ""
    for raw in run.splitlines():
        line = raw.rstrip()
        text = line.strip()
        if text.startswith(GATE + " "):
            if cont or prev.endswith(("||", "&&", "|", "\\")):
                return "the line before the gate line ends with %r, so the gate is the tail of another command" % prev.split()[-1]
            if raw[:1] in (" ", "\t"):
                return "the gate line is indented (the body of a branch or block), not a top-level command"
            if depth != 0:
                return "the gate line sits inside an if/case/loop/block left open in column 0"
            return None
        continued, cont = cont, line.endswith("\\")
        if text and not text.startswith("#"):
            prev = text
        if continued or not text or text.startswith("#") or raw[:1] in (" ", "\t"):
            continue
        words = re.split(r"[\s;]+", text)
        if words[0] in ("exit", "return", "exec"):
            return "a top-level `%s` comes before the gate line" % words[0]
        depth += sum(w in OPENERS for w in words) - sum(w in CLOSERS for w in words)
    return "no gate line"


# The only steps desktop-macos may run before its checkout, by name: anything
# else there (a toolchain install, `go install`, a build) would run before the
# backstop gate. Add a step here only if it builds and uploads nothing.
PRE_CHECKOUT_ALLOWED = (
    "Guard — republishing a pre-0.9.0 tag's mac assets is not idempotent",
)


def check_invocation(errors, where, step, job, six):
    lines = gate_lines(step)
    if len(lines) != 1:
        errors.append("%s: %d lines run the gate (want exactly 1)" % (where, len(lines)))
        return
    m = LINE_RE.fullmatch(lines[0])
    if not m:
        errors.append("%s: the gate line is not exactly %s \"${TAG}\" followed by --has NAME=${{ secrets.NAME != '' }} flags, with nothing after them: %s" % (where, GATE, lines[0]))
        return
    pairs = re.findall(FLAG, m.group(1))
    for name, secret in pairs:
        if name != secret:
            errors.append("%s: --has %s is built from secrets.%s" % (where, name, secret))
    names = [n for n, _ in pairs]
    if sorted(names) != sorted(six) or len(set(names)) != len(names):
        errors.append("%s: --has flags %s are not CAN_NOTARIZE's six %s" % (where, names, six))
    tag = (step.get("env") or {}).get("TAG", (job.get("env") or {}).get("TAG"))
    if tag is None or expr(tag) != TAG_EXPR:
        errors.append("%s: TAG is %r, not ${{ %s }}" % (where, tag, TAG_EXPR))
    if step.get("continue-on-error") not in (None, False):
        errors.append("%s: continue-on-error would let a refusal pass" % where)
    why = unreachable(step)
    if why:
        errors.append("%s: the gate may never run: %s" % (where, why))


def behind_operator(errors, where, steps):
    """Every step, gate step or not: the invocation line must BEGIN with the gate."""
    for j, s in enumerate(steps):
        if BEHIND_OPERATOR.search(s.get("run") or ""):
            errors.append("%s: step %d calls the gate behind `||`/`&&`/`;`/`|`; the invocation line must BEGIN with the gate" % (where, j))


def check(path, gate_script):
    wf, loader = load(path)
    errors = []
    jobs = wf.get("jobs") or {}
    dm = jobs.get("desktop-macos") or {}
    six = re.findall(r"secrets\.([A-Z0-9_]+) != ''", str((dm.get("env") or {}).get("CAN_NOTARIZE", "")))
    if len(six) != 6 or len(set(six)) != 6:
        errors.append("desktop-macos: CAN_NOTARIZE does not read six distinct secrets: %s" % six)
    with open(gate_script) as f:
        m = re.search(r'^NAMES="([^"]*)"', f.read(), re.M)
    if not m or m.group(1).split() != six:
        errors.append("%s's NAMES (%s) are not CAN_NOTARIZE's six, in order (%s)" % (GATE, m.group(1).split() if m else None, six))

    rp = jobs.get("release-plan") or {}
    steps = rp.get("steps") or []
    plan = [i for i, s in enumerate(steps) if s.get("id") == "plan" and "scripts/release/release-plan.sh" in (s.get("run") or "")]
    found = gate_indices(steps)
    behind_operator(errors, "release-plan", steps)
    if len(plan) != 1:
        errors.append("release-plan: no single step with id `plan` runs release-plan.sh")
    if len(found) != 1:
        errors.append("release-plan: %d steps run the gate (want exactly 1)" % len(found))
    elif plan:
        i = found[0]
        if i != plan[0] + 1:
            errors.append("release-plan: the gate is step %d, not right after release-plan.sh (step %d)" % (i, plan[0]))
        if expr(steps[i].get("if", "")) != RP_IF:
            errors.append("release-plan: the gate step's if is %r, not %r" % (steps[i].get("if"), RP_IF))
        check_invocation(errors, "release-plan", steps[i], rp, six)

    steps = dm.get("steps") or []
    checkout = [i for i, s in enumerate(steps) if str(s.get("uses", "")).startswith("actions/checkout@")]
    found = gate_indices(steps)
    behind_operator(errors, "desktop-macos", steps)
    if len(checkout) != 1:
        errors.append("desktop-macos: %d checkout steps (want exactly 1)" % len(checkout))
    if len(found) != 1:
        errors.append("desktop-macos: %d steps run the gate (want exactly 1)" % len(found))
    elif checkout:
        i = found[0]
        if i != checkout[0] + 1:
            errors.append("desktop-macos: the gate is step %d, not right after the checkout (step %d), before any build" % (i, checkout[0]))
        if "if" in steps[i]:
            errors.append("desktop-macos: the gate step has an if (%r); the backstop must always run" % steps[i]["if"])
        for j, other in enumerate(steps[:checkout[0]]):
            label = other.get("name") or other.get("id")
            if label not in PRE_CHECKOUT_ALLOWED:
                errors.append("desktop-macos: step %d (%s) runs before the checkout and the gate, and is not on PRE_CHECKOUT_ALLOWED" % (j, label or other.get("uses") or "unnamed"))
        check_invocation(errors, "desktop-macos", steps[i], dm, six)

    for e in errors:
        print("wiring: " + e)
    if errors:
        return 1
    print("wiring OK (parsed with %s): release-plan runs the gate right after release-plan.sh when ship_desktop, desktop-macos right after its checkout, each with the six flags" % loader)
    return 0


def mutate(src, dst, name):
    wf, _ = load(src)
    jobs = wf["jobs"]
    rp, dm = jobs["release-plan"]["steps"], jobs["desktop-macos"]["steps"]

    def one(steps):
        found = gate_indices(steps)
        if len(found) != 1:
            sys.exit("mutate %s: expected one gate step, found %d" % (name, len(found)))
        return found[0]

    if name == "none":
        pass
    elif name == "rp-delete":
        del rp[one(rp)]
    elif name == "rp-before-plan":
        rp.insert(0, rp.pop(one(rp)))
    elif name == "rp-unconditioned":
        rp[one(rp)].pop("if")
    elif name == "rp-drop-flag":
        s = rp[one(rp)]
        new = re.sub(r"[ \t]*\\\n[ \t]*--has APPLE_APP_SPECIFIC_PASSWORD=\$\{\{[^}]*\}\}", "", s["run"])
        if new == s["run"]:
            sys.exit("mutate rp-drop-flag: the flag was not found")
        s["run"] = new
    elif name == "rp-or-true":
        s = rp[one(rp)]
        s["run"] = s["run"].rstrip("\n") + " || true\n"
    elif name == "dm-delete":
        del dm[one(dm)]
    elif name == "dm-after-build":
        step = dm.pop(one(dm))
        build = [i for i, s in enumerate(dm) if "tauri-dmg-mac" in (s.get("run") or "")]
        if len(build) != 1:
            sys.exit("mutate dm-after-build: no single build step")
        dm.insert(build[0] + 1, step)
    elif name == "dm-conditioned":
        dm[one(dm)]["if"] = "env.CAN_NOTARIZE == 'true'"
    elif name == "dm-build-before-checkout":
        rust = [s for s in dm if s.get("name") == "Rust toolchain"]
        if len(rust) != 1:
            sys.exit("mutate dm-build-before-checkout: no single Rust toolchain step")
        dm.insert(0, dict(rust[0]))
    elif name == "dm-go-install-before-checkout":
        dm.insert(0, {"name": "Install a tool", "run": "go install example.com/tool@v1.2.3"})
    elif name in ("rp-unreachable", "rp-after-exit", "rp-in-block", "rp-true-or", "rp-true-or-inline", "rp-set-plus-e"):
        s = rp[one(rp)]
        lines = s["run"].split("\n")
        start = next(k for k, l in enumerate(lines) if l.startswith(GATE + " "))
        end = start
        while lines[end].rstrip().endswith("\\"):
            end += 1
        block = lines[start:end + 1]
        if name == "rp-after-exit":
            lines[start:start] = ["exit 0"]
        elif name == "rp-true-or":
            lines[start:start] = ["true ||"]
        elif name == "rp-true-or-inline":
            lines[start] = "true || " + lines[start]
        elif name == "rp-set-plus-e":
            lines[start:start] = ["set +e"]
        elif name == "rp-in-block":
            lines[start:end + 1] = ["if false; then"] + block + ["fi"]
        else:
            # Into the missing-script branch, after its `exit 1`: the text,
            # flags, env and step index all survive, but the gate never runs.
            del lines[start:end + 1]
            fi = next(k for k, l in enumerate(lines) if l == "fi")
            lines[fi:fi] = ["  " + l for l in block]
        s["run"] = "\n".join(lines)
    else:
        sys.exit("mutate: unknown mutation %r" % name)
    with open(dst, "w") as f:
        json.dump(wf, f, indent=1, default=str)
    return 0


def extract(src, job, dst, missing):
    """Write the job's gate step's run script with each ${{ secrets.X != '' }}
    replaced by false (X listed in `missing`) or true, as GitHub renders it."""
    wf, _ = load(src)
    steps = wf["jobs"][job]["steps"]
    found = gate_indices(steps)
    if len(found) != 1:
        sys.exit("extract %s: expected one gate step, found %d" % (job, len(found)))
    run = re.sub(r"\$\{\{ secrets\.([A-Z0-9_]+) != '' \}\}",
                 lambda m: "false" if m.group(1) in missing else "true", steps[found[0]]["run"])
    if "${{" in run:
        sys.exit("extract %s: an expression is left unrendered in the step" % job)
    with open(dst, "w") as f:
        f.write(run)
    return 0


if __name__ == "__main__":
    if len(sys.argv) == 4 and sys.argv[1] == "check":
        sys.exit(check(sys.argv[2], sys.argv[3]))
    if len(sys.argv) == 5 and sys.argv[1] == "mutate":
        sys.exit(mutate(sys.argv[2], sys.argv[3], sys.argv[4]))
    if len(sys.argv) >= 5 and sys.argv[1] == "extract":
        sys.exit(extract(sys.argv[2], sys.argv[3], sys.argv[4], set(sys.argv[5:])))
    sys.exit("usage: wiring.py check <workflow> <gate-script> | mutate <in> <out> <mutation> | extract <workflow> <job> <out> [missing secret...]")
PY

WF="${SCRATCH}/.github/workflows/publish-images.yaml"
WIRING_OUT=""; WIRING_RC=0
run_wiring() {
  WIRING_RC=0
  WIRING_OUT="$(python3 "${WIRING}" check "$1" "${GATE}" 2>&1)" || WIRING_RC=$?
}

step "publish-images.yaml: release-plan and desktop-macos both call the signing gate, wired as required"
run_wiring "${WF}"
[ "${WIRING_RC}" -eq 0 ] || fail "the wiring guard rejects the real workflow (rc=${WIRING_RC}): ${WIRING_OUT}"
ok "${WIRING_OUT#wiring OK }"

step "the wiring guard: an unmutated JSON round-trip still passes (the mutation cells' baseline)"
python3 "${WIRING}" mutate "${WF}" "${WIRING_DIR}/none.json" none || fail "mutate none failed"
run_wiring "${WIRING_DIR}/none.json"
[ "${WIRING_RC}" -eq 0 ] || fail "the unmutated round-trip is rejected (rc=${WIRING_RC}): ${WIRING_OUT}"
ok "round-trip baseline passes"

# mutation_case <mutation> <expected substring of the guard's complaint>
mutation_case() {
  python3 "${WIRING}" mutate "${WF}" "${WIRING_DIR}/$1.json" "$1" || fail "mutate $1 failed"
  run_wiring "${WIRING_DIR}/$1.json"
  [ "${WIRING_RC}" -eq 1 ] || fail "the wiring guard passed a workflow mutated by $1 (rc=${WIRING_RC}): ${WIRING_OUT}"
  echo "${WIRING_OUT}" | grep -qF "$2" || fail "mutation $1 was refused for the wrong reason (want '$2'): ${WIRING_OUT}"
  ok "mutation $1 → refused: $2"
}
step "the wiring guard: deleting, moving or weakening either invocation turns it red"
mutation_case rp-delete "release-plan: 0 steps run the gate"
mutation_case rp-before-plan "release-plan: the gate is step 0, not right after release-plan.sh"
mutation_case rp-unconditioned "release-plan: the gate step's if is None"
mutation_case rp-drop-flag "release-plan: --has flags"
mutation_case rp-or-true "release-plan: the gate line is not exactly"
mutation_case dm-delete "desktop-macos: 0 steps run the gate"
mutation_case dm-after-build "desktop-macos: the gate is step"
mutation_case dm-conditioned "desktop-macos: the gate step has an if"
mutation_case rp-unreachable "release-plan: the gate may never run: the gate line is indented"
mutation_case rp-after-exit "release-plan: the gate may never run: a top-level \`exit\` comes before the gate line"
mutation_case rp-in-block "release-plan: the gate may never run: the gate line sits inside an if/case/loop/block"
mutation_case rp-true-or "release-plan: the gate may never run: the gate is called behind"
mutation_case rp-true-or-inline "release-plan: step 2 calls the gate behind"
mutation_case rp-set-plus-e "release-plan: the gate may never run: the step turns errexit off"
mutation_case dm-build-before-checkout "desktop-macos: step 0 (Rust toolchain) runs before the checkout and the gate, and is not on PRE_CHECKOUT_ALLOWED"
mutation_case dm-go-install-before-checkout "desktop-macos: step 0 (Install a tool) runs before the checkout and the gate, and is not on PRE_CHECKOUT_ALLOWED"

# ---------------------------------------------------------------------------
# The two gate steps, EXECUTED: the workflow's own run scripts, with each
# ${{ secrets.X != '' }} rendered true/false as GitHub would, run by
# `bash -e` (the runner's default shell) in a scratch "checkout" with or
# without scripts/release/desktop-signing-gate.sh (the real gate). This proves
# what the text checks cannot: the gate is reached, the flags map to the
# right names, and the missing-script fallback admits exactly a dispatch of a
# pre-0.9.0 tag (the tags that predate the gate).
# ---------------------------------------------------------------------------
STEP_DIR="${SCRATCH}/step-exec"
mkdir -p "${STEP_DIR}/no-gate" "${STEP_DIR}/with-gate/scripts/release"
cp "${GATE}" "${STEP_DIR}/with-gate/scripts/release/"
STEP_OUT=""; STEP_RC=0
# run_step <workflow> <job> <checkout: no-gate|with-gate> <event> <tag> [missing secret...]
run_step() {
  local wf="$1" job="$2" co="${STEP_DIR}/$3" ev="$4" tag="$5"
  shift 5
  python3 "${WIRING}" extract "${wf}" "${job}" "${STEP_DIR}/step.sh" "$@" || fail "extract ${job} from ${wf} failed"
  STEP_RC=0
  STEP_OUT="$(cd "${co}" && env -i PATH="${PATH}" HOME="${HOME}" GITHUB_EVENT_NAME="${ev}" TAG="${tag}" \
    bash -e "${STEP_DIR}/step.sh" 2>&1)" || STEP_RC=$?
}
# step_case <label> <want rc> <expected output substring> <run_step args...>
step_case() {
  local label="$1" want="$2" expect="$3"
  shift 3
  run_step "$@"
  [ "${STEP_RC}" -eq "${want}" ] || fail "step (${label}) exited ${STEP_RC} (want ${want}): ${STEP_OUT}"
  case "${STEP_OUT}" in
    *"${expect}"*) ;;
    *) fail "step (${label}) output lacks '${expect}': ${STEP_OUT}" ;;
  esac
  ok "step ${label} → exit ${want}"
}
SKIPPED="predates scripts/release/desktop-signing-gate.sh (a pre-0.9.0 tag); skipping the gate"
NO_GATE="is missing from"

step "release-plan's gate step, executed: the missing-script fallback admits only a dispatch of a pre-0.9.0 tag"
step_case "no gate, dispatch v0.8.1" 0 "${SKIPPED}" "${WF}" release-plan no-gate workflow_dispatch v0.8.1
step_case "no gate, dispatch v0.8.0" 0 "${SKIPPED}" "${WF}" release-plan no-gate workflow_dispatch v0.8.0
step_case "no gate, dispatch v0.9.0" 1 "${NO_GATE}" "${WF}" release-plan no-gate workflow_dispatch v0.9.0
step_case "no gate, dispatch v0.9.0-rc.1 (a 0.9.0 prerelease does not qualify)" 1 "${NO_GATE}" "${WF}" release-plan no-gate workflow_dispatch v0.9.0-rc.1
step_case "no gate, dispatch v0.8.1-.. (a malformed suffix)" 1 "${NO_GATE}" "${WF}" release-plan no-gate workflow_dispatch v0.8.1-..
step_case "no gate, dispatch v0.8.1-rc.1 (no suffix of any kind)" 1 "${NO_GATE}" "${WF}" release-plan no-gate workflow_dispatch v0.8.1-rc.1
step_case "no gate, dispatch v0.8.1+build (no suffix of any kind)" 1 "${NO_GATE}" "${WF}" release-plan no-gate workflow_dispatch v0.8.1+build
step_case "no gate, dispatch v0.10.0 (numeric, not lexical)" 1 "${NO_GATE}" "${WF}" release-plan no-gate workflow_dispatch v0.10.0
step_case "no gate, dispatch v1.0.0" 1 "${NO_GATE}" "${WF}" release-plan no-gate workflow_dispatch v1.0.0
step_case "no gate, dispatch of a malformed tag (v0.8)" 1 "${NO_GATE}" "${WF}" release-plan no-gate workflow_dispatch v0.8
step_case "no gate, dispatch of a leading-zero tag (v0.08.1)" 1 "${NO_GATE}" "${WF}" release-plan no-gate workflow_dispatch v0.08.1
step_case "no gate, a tag push of v0.8.1" 1 "${NO_GATE}" "${WF}" release-plan no-gate push v0.8.1

step "release-plan's gate step, executed: with the script present, the gate always runs"
step_case "gate present, dispatch v0.8.1, all six missing (no fallback when the script exists)" 1 "${GATE_REFUSAL} $(comma_list "${GATE_SECRETS}")" \
  "${WF}" release-plan with-gate workflow_dispatch v0.8.1 ${GATE_SECRETS}
step_case "gate present, push v9.1.0, all six present" 0 "is stable and all six Apple secrets are present" \
  "${WF}" release-plan with-gate push v9.1.0
step_case "gate present, push v9.1.0, secrets.APPLE_ID missing (the flag reaches the gate under its own name)" 1 "${GATE_REFUSAL} APPLE_ID" \
  "${WF}" release-plan with-gate push v9.1.0 APPLE_ID
step_case "gate present, push v9.1.0-rc.1, all six missing" 0 "ad-hoc-signed" \
  "${WF}" release-plan with-gate push v9.1.0-rc.1 ${GATE_SECRETS}

step "desktop-macos's backstop step, executed: the gate runs, and a missing script fails closed"
step_case "backstop, push v9.1.0, all six missing" 1 "${GATE_REFUSAL} $(comma_list "${GATE_SECRETS}")" \
  "${WF}" desktop-macos with-gate push v9.1.0 ${GATE_SECRETS}
step_case "backstop, push v9.1.0, all six present" 0 "is stable and all six Apple secrets are present" \
  "${WF}" desktop-macos with-gate push v9.1.0
step_case "backstop, no gate script, dispatch v0.9.0" 127 "desktop-signing-gate.sh" \
  "${WF}" desktop-macos no-gate workflow_dispatch v0.9.0

step "the rp-unreachable mutation is a real bypass when executed, which the guard refuses (above)"
run_step "${WIRING_DIR}/rp-unreachable.json" release-plan with-gate push v9.1.0 ${GATE_SECRETS}
[ "${STEP_RC}" -eq 0 ] || fail "the rp-unreachable step was expected to pass a stable all-missing push (the gate never runs), got ${STEP_RC}: ${STEP_OUT}"
case "${STEP_OUT}" in *"desktop signing gate"*|*"::error::"*) fail "the rp-unreachable step still ran the gate: ${STEP_OUT}" ;; esac
ok "moved after the branch's exit, the gate never runs (a stable all-missing push exits 0), so the text checks alone were not enough"

# ---------------------------------------------------------------------------
# changelog-section.sh: the release body. Its own scratch CHANGELOG, with a
# decoy Unreleased section above, a `## v3.2.10 — ` heading to prove the match
# is exact (v3.2.1 must not read it), and a later section that must not leak.
# ---------------------------------------------------------------------------
SECTION="${SCRATCH}/scripts/release/changelog-section.sh"
cat > "${SCRATCH}/CHANGELOG.md" <<'EOF'
# Changelog

## Unreleased

**Ships:** server, host-agent, desktop

- unreleased decoy (must never be printed)

## v3.2.10 — 2026-03-03

**Ships:** desktop

- the 3.2.10 entry (v3.2.1 must not read this heading)

## v3.2.1 — 2026-02-02

**Ships:** server

- the 3.2.1 entry
- its second line

### Fixed
- a subsection stays in the body

## v3.2.0 — 2026-01-01

**Ships:** desktop

- the 3.2.0 entry (must not leak into 3.2.1's body)
EOF
SEC_OUT=""; SEC_RC=0
run_section() { SEC_RC=0; SEC_OUT="$("${SECTION}" "$@" 2>&1)" || SEC_RC=$?; }

step "changelog-section.sh: prints exactly the tag's section body, stopping at the next ## heading"
run_section v3.2.1
[ "${SEC_RC}" -eq 0 ] || fail "v3.2.1 exited ${SEC_RC}: ${SEC_OUT}"
expected="$(printf '\n**Ships:** server\n\n- the 3.2.1 entry\n- its second line\n\n### Fixed\n- a subsection stays in the body')"
[ "${SEC_OUT}" = "${expected}" ] || fail "v3.2.1 body mismatch: [${SEC_OUT}]"
ok "v3.2.1 → its own body (a ### subsection included), heading excluded, nothing from v3.2.0 or v3.2.10"

step "changelog-section.sh: never reads ## Unreleased, and the last section runs to EOF"
run_section v3.2.0
[ "${SEC_RC}" -eq 0 ] || fail "v3.2.0 exited ${SEC_RC}: ${SEC_OUT}"
[ "${SEC_OUT}" = "$(printf '\n**Ships:** desktop\n\n- the 3.2.0 entry (must not leak into 3.2.1'"'"'s body)')" ] || fail "v3.2.0 body mismatch: [${SEC_OUT}]"
for t in v3.2.1 v3.2.0 v3.2.10; do
  run_section "${t}"
  case "${SEC_OUT}" in *"unreleased decoy"*) fail "${t}'s body includes the Unreleased decoy" ;; esac
done
ok "the Unreleased decoy is never printed; the final section ends at EOF"

step "changelog-section.sh: a missing section (or a prerelease, which has none) exits 1"
run_section v9.9.9
[ "${SEC_RC}" -eq 1 ] || fail "missing section exited ${SEC_RC} (want 1): ${SEC_OUT}"
echo "${SEC_OUT}" | grep -qF "no '## v9.9.9 — <date>' section" || fail "missing-section error unclear: ${SEC_OUT}"
run_section v3.2.1-rc.1
[ "${SEC_RC}" -eq 1 ] || fail "prerelease v3.2.1-rc.1 exited ${SEC_RC} (want 1): ${SEC_OUT}"
ok "v9.9.9 and v3.2.1-rc.1 → exit 1"

step "changelog-section.sh: a malformed tag exits 2"
run_section 3.2
[ "${SEC_RC}" -eq 2 ] || fail "malformed tag exited ${SEC_RC} (want 2): ${SEC_OUT}"
run_section
[ "${SEC_RC}" -eq 2 ] || fail "no tag exited ${SEC_RC} (want 2): ${SEC_OUT}"
ok "'3.2' and no tag → exit 2"

# ---------------------------------------------------------------------------
# A `## ` line that is not a section heading is body text, fenced or not: the
# section ends only at the next `## Unreleased` / `## v<digit>` line. The
# fixture is assembled from three pieces so the expected v4.0.0 body is a file
# compared byte for byte: a bash fence holding `## Upgrade commands`, a bare
# `## Notes for upgraders` and `## version notes` (not `v<digit>`), a ~~~ fence
# never closed, then the warning; then a real `## v3.9.0 — ` heading that
# must still end it.
# ---------------------------------------------------------------------------
FENCE_DIR="${SCRATCH}/fence"
mkdir -p "${FENCE_DIR}"
cat > "${FENCE_DIR}/head.md" <<'MD'
# Changelog

## Unreleased

**Ships:** server, host-agent, desktop

- unreleased work

## v4.0.0 — 2026-04-04
MD
cat > "${FENCE_DIR}/body.md" <<'MD'

**Ships:** server

- the upgrade, step by step:

  ```bash
  ## Upgrade commands (a comment in a shell example, not a heading)
  sudo apt-get install shed-server
  ```

## Notes for upgraders

## version notes (a `## v` that is not `## v<digit>`)

~~~
## inside a tilde fence that is never closed
```

- the warning after the examples (it must reach the release page)
MD
cat > "${FENCE_DIR}/tail.md" <<'MD'

## v3.9.0 — 2026-03-03

**Ships:** desktop

- an older entry (must not leak into v4.0.0's body)
MD
cat "${FENCE_DIR}/head.md" "${FENCE_DIR}/body.md" "${FENCE_DIR}/tail.md" > "${SCRATCH}/CHANGELOG.md"

step "changelog-section.sh: any ## line other than a section heading is body text; a real ## v<digit> heading still ends it"
SEC_RC=0
"${SECTION}" v4.0.0 > "${FENCE_DIR}/out.md" 2>"${FENCE_DIR}/out.err" || SEC_RC=$?
[ "${SEC_RC}" -eq 0 ] || fail "v4.0.0 exited ${SEC_RC}: $(cat "${FENCE_DIR}/out.err")"
cmp -s "${FENCE_DIR}/out.md" "${FENCE_DIR}/body.md" \
  || fail "v4.0.0 body differs from the expected one: $(diff "${FENCE_DIR}/body.md" "${FENCE_DIR}/out.md" || true)"
grep -q "an older entry" "${FENCE_DIR}/out.md" && fail "v4.0.0's body ran past the real ## v3.9.0 heading"
ok "v4.0.0 → the whole body, byte for byte: the fenced ## Upgrade commands, the bare ## lines and an unclosed fence kept, the warning kept, stopped at ## v3.9.0"
run_section v3.9.0
[ "${SEC_RC}" -eq 0 ] || fail "v3.9.0 exited ${SEC_RC}: ${SEC_OUT}"
[ "${SEC_OUT}" = "$(printf '\n**Ships:** desktop\n\n- an older entry (must not leak into v4.0.0'"'"'s body)')" ] || fail "v3.9.0 body mismatch: [${SEC_OUT}]"
ok "the section after it extracts cleanly too"

step "changelog-section.sh: a ## Unreleased line below a section ends it too (the other real heading form)"
printf '# Changelog\n\n## v4.1.0 — 2026-06-06\n\n**Ships:** server\n\n- the entry\n\n## Unreleased\n\n- misplaced, must not leak\n' > "${SCRATCH}/CHANGELOG.md"
run_section v4.1.0
[ "${SEC_RC}" -eq 0 ] || fail "v4.1.0 exited ${SEC_RC}: ${SEC_OUT}"
[ "${SEC_OUT}" = "$(printf '\n**Ships:** server\n\n- the entry')" ] || fail "v4.1.0 body mismatch: [${SEC_OUT}]"
ok "## Unreleased is a boundary as well"

step "release-plan.sh reads the same body: a Ships line after a fenced ## line is found; a ## v<digit> line in the body ends it"
# The phases above leave host-agent at 8.8.10 alone (server 8.8.8, desktop
# 8.8.9), so v8.8.10 is a stable host-agent-only tag with no craze check. The
# Ships line sits AFTER a fenced `## step one` on purpose: an extractor that
# stopped at it would hide the Ships line and fail the plan.
printf '# Changelog\n\n## v8.8.10 — 2026-05-05\n\n```bash\n## step one\n```\n\n**Ships:** host-agent\n\n## v8.8.9 — 2026-04-04\n\n**Ships:** desktop\n' > "${SCRATCH}/CHANGELOG.md"
run_plan v8.8.10
[ "${PLAN_RC}" -eq 0 ] || fail "release-plan with a Ships line after a fenced ## exited ${PLAN_RC} (want 0)"
[ "${PLAN_OUT}" = "$(expect_plan false true false true)" ] || fail "release-plan (Ships after a fenced ##) stdout: ${PLAN_OUT}"
# The documented authoring rule, enforced loudly: a body line starting
# `## v<digit>` ends the section, so a Ships line after it is not seen.
printf '# Changelog\n\n## v8.8.10 — 2026-05-05\n\n- an entry\n\n## v1.0.0 was the first release (a misplaced line)\n\n**Ships:** host-agent\n' > "${SCRATCH}/CHANGELOG.md"
rp_rule_rc=0
rp_rule_out="$("${RP}" v8.8.10 2>&1 1>/dev/null)" || rp_rule_rc=$?
[ "${rp_rule_rc}" -eq 1 ] || fail "release-plan with a ## v<digit> body line exited ${rp_rule_rc} (want 1): ${rp_rule_out}"
echo "${rp_rule_out}" | grep -qF "has no **Ships:** line" || fail "release-plan does not report the hidden Ships line: ${rp_rule_out}"
ok "Ships after a fenced ## → the host-agent-only plan; a ## v<digit> body line ends the section, and the plan fails loudly"

echo
echo "release-scripts-test: all ${PASS} checks passed"
