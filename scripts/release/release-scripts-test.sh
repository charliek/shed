#!/usr/bin/env bash
# Self-test for the release-model scripts:
#
#   scripts/release/update-version.sh        (component-selecting version bumper)
#   scripts/release/release-plan.sh          (tag → per-component ship selector)
#   scripts/release/recommend-components.sh  (next-tag component recommender)
#   scripts/release/desktop-signing-gate.sh  (a stable desktop tag fails closed
#                                             without the six Apple secrets)
#   scripts/release/changelog-section.sh     (the curated release body)
#   scripts/release/macos-server-app.sh      (the darwin shed-server signing +
#                                             ShedServer.app hook, plan 028 D4)
#   scripts/release/server-signing-gate.sh   (a server tag fails closed
#                                             without the four Developer ID
#                                             secrets, plan 028 D4)
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
# Phase 6 (plan 028 D4, #304) runs macos-server-app.sh, the darwin shed-server
# signing hook, with stub codesign/plutil/uname on PATH: a linux target is a
# no-op; required signing fails closed (no identity or team, a non-macOS host,
# an ad hoc, wrong-team, untimestamped or over-entitled signature); the
# positive ad hoc and Developer ID stagings; and the standalone --verify.
#
# Phase 7 (plan 028 D4, #304) runs server-signing-gate.sh as deployed and
# extends Phase 5's wiring guard to the server's release signing: the
# release-plan gate step (its four flags, its tag-push condition, right after
# the desktop gate), the server-signing-preflight job and the skipped-vs-failed
# conditions every image job and `release` put on it, the release job's
# certificate import / signing env / keychain delete between the server and
# host-agent goreleaser steps, and server-signing-dry-run.yml. Mutated copies
# prove each rule bites; the gate step and the preflight's signing step are
# EXECUTED (the latter against Phase 6's stub codesign/plutil).
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
  scripts/release/macos-server-app.sh \
  scripts/release/server-signing-gate.sh \
  internal/vz/entitlements.plist \
  .github/workflows/publish-images.yaml \
  .github/workflows/server-signing-dry-run.yml \
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

step "check-craze-pin.sh: a not-found tag hints 'craze's version, not the shed tag' ONLY when the argument is the shed manifest version"
# The seam emits NO rows (/bin/true), so the tag is "not found" whatever the
# argument is. The hint must name the mistake (passing the shed tag), and must
# not appear for an ordinary craze version that is merely absent.
CCP="${SCRATCH}/scripts/release/check-craze-pin.sh"
SHED_MANIFEST_V="$(jq -r '.version' "${SCRATCH}/.claude-plugin/plugin.json")"
[[ "${SHED_MANIFEST_V}" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || fail "scratch shed manifest version '${SHED_MANIFEST_V}' is not X.Y.Z"
CCP_HINT="this takes craze's version from craze-pin.env, not the shed tag"
ccp_shed_rc=0
ccp_shed_out="$(CRAZE_PIN_LS_REMOTE=/bin/true "${CCP}" --release "v${SHED_MANIFEST_V}" 2>&1)" || ccp_shed_rc=$?
[ "${ccp_shed_rc}" -eq 1 ] || fail "craze-hint (shed version) exited ${ccp_shed_rc} (want 1): ${ccp_shed_out}"
echo "${ccp_shed_out}" | grep -qF "not found on charliek/craze" || fail "craze-hint (shed version): the not-found error is missing: ${ccp_shed_out}"
echo "${ccp_shed_out}" | grep -qF "${CCP_HINT}" || fail "craze-hint (shed version): the hint is missing: ${ccp_shed_out}"
ccp_other_rc=0
ccp_other_out="$(CRAZE_PIN_LS_REMOTE=/bin/true "${CCP}" --release "v9.9.9" 2>&1)" || ccp_other_rc=$?
[ "${ccp_other_rc}" -eq 1 ] || fail "craze-hint (other version) exited ${ccp_other_rc} (want 1): ${ccp_other_out}"
echo "${ccp_other_out}" | grep -qF "not found on charliek/craze" || fail "craze-hint (other version): the not-found error is missing: ${ccp_other_out}"
echo "${ccp_other_out}" | grep -qF "${CCP_HINT}" && fail "craze-hint (other version): the hint appeared for a version that is not the shed manifest's: ${ccp_other_out}"
ok "the hint appears for the shed manifest version (${SHED_MANIFEST_V}) and not for an unrelated craze version"

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
#   check <workflow> <gate-script> <server-gate-script> <dry-run-workflow>
#                                   → exit 0, or 1 listing every violation
#                                     (the server's signing wiring, Phase 7,
#                                     included)
#   mutate <in> <out> <mutation>    → a structurally mutated copy (as JSON,
#                                     which both loaders read) for the
#                                     negative cells below (mutate-dry: the
#                                     same for the dry-run workflow)
#   extract… <workflow> …           → a step's run script (and env), rendered
#                                     as GitHub would, for the execution cells
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
SERVER_GATE = "scripts/release/server-signing-gate.sh"
TAG_EXPR = "github.event.inputs.version || github.ref_name"
RP_IF = "steps.plan.outputs.ship_desktop == 'true'"
# One `--has NAME=${{ secrets.NAME != '' }}` flag: FLAG captures both names,
# FLAG_NC is the same pattern without groups, for the whole-line match.
FLAG = r"--has ([A-Z0-9_]+)=\$\{\{ secrets\.([A-Z0-9_]+) != '' \}\}"
FLAG_NC = r"--has [A-Z0-9_]+=\$\{\{ secrets\.[A-Z0-9_]+ != '' \}\}"


def line_re(gate):
    return re.compile(re.escape(gate) + r' "\$\{TAG\}"((?: ' + FLAG_NC + r")+)")


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


def gate_lines(step, gate=GATE):
    """Lines of the step's run that execute the gate, continuations joined."""
    run = re.sub(r"[ \t]*\\\n[ \t]*", " ", step.get("run") or "")
    return [l.strip() for l in run.splitlines() if l.strip().startswith(gate + " ")]


def gate_indices(steps, gate=GATE):
    return [i for i, s in enumerate(steps) if gate_lines(s, gate)]


OPENERS = ("if", "case", "for", "while", "until", "select", "{", "(")
CLOSERS = ("fi", "esac", "done", "}", ")")


def behind_re(gate):
    """The gate called behind a control operator on the same line (`true ||
    gate`, `false && gate`, `x; gate`, `x | gate`): a call that may never run,
    or whose status is lost. The `[ ! -f <gate> ]` test and the messages naming
    the gate never put it right after one of these."""
    return re.compile(r"(\|\||&&|;|\|)\s*\"?" + re.escape(gate) + r"\b")


SET_PLUS_E = re.compile(r"(?m)^\s*set\s+(?:-\w*\s+)*\+\w*e|^\s*set\s+\+o\s+errexit\b")


def unreachable(step, gate=GATE):
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
    if behind_re(gate).search(run):
        return "the gate is called behind `||`/`&&`/`;`/`|` on its line, not at the start of a command"
    depth, cont, prev = 0, False, ""
    for raw in run.splitlines():
        line = raw.rstrip()
        text = line.strip()
        if text.startswith(gate + " "):
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


def check_invocation(errors, where, step, job, six, gate=GATE, which="CAN_NOTARIZE's six"):
    lines = gate_lines(step, gate)
    if len(lines) != 1:
        errors.append("%s: %d lines run the gate (want exactly 1)" % (where, len(lines)))
        return
    m = line_re(gate).fullmatch(lines[0])
    if not m:
        errors.append("%s: the gate line is not exactly %s \"${TAG}\" followed by --has NAME=${{ secrets.NAME != '' }} flags, with nothing after them: %s" % (where, gate, lines[0]))
        return
    pairs = re.findall(FLAG, m.group(1))
    for name, secret in pairs:
        if name != secret:
            errors.append("%s: --has %s is built from secrets.%s" % (where, name, secret))
    names = [n for n, _ in pairs]
    if sorted(names) != sorted(six) or len(set(names)) != len(names):
        errors.append("%s: --has flags %s are not %s %s" % (where, names, which, six))
    tag = (step.get("env") or {}).get("TAG", (job.get("env") or {}).get("TAG"))
    if tag is None or expr(tag) != TAG_EXPR:
        errors.append("%s: TAG is %r, not ${{ %s }}" % (where, tag, TAG_EXPR))
    if step.get("continue-on-error") not in (None, False):
        errors.append("%s: continue-on-error would let a refusal pass" % where)
    why = unreachable(step, gate)
    if why:
        errors.append("%s: the gate may never run: %s" % (where, why))


def behind_operator(errors, where, steps, gate=GATE):
    """Every step, gate step or not: the invocation line must BEGIN with the gate."""
    for j, s in enumerate(steps):
        if behind_re(gate).search(s.get("run") or ""):
            errors.append("%s: step %d calls the gate behind `||`/`&&`/`;`/`|`; the invocation line must BEGIN with the gate" % (where, j))


# --- the server's release signing (plan 028 D4, #304) ----------------------
PREFLIGHT = "server-signing-preflight"
IMPORT_STEP = "Import Developer ID certificate"
DELETE_STEP = "Delete signing keychain"
KEYCHAIN = "shed-server-signing.keychain-db"
SERVER_RP_IF = "steps.plan.outputs.ship_server == 'true' && github.event_name == 'push'"
PREFLIGHT_IF = "needs.release-plan.outputs.ship_server == 'true' && github.event_name == 'push'"
# A skipped preflight passes only off a tag push (a workflow_dispatch image
# republish); a failed or cancelled one, or one skipped on a tag push, blocks.
PF_OK = ("(needs.server-signing-preflight.result == 'success' || "
         "(github.event_name != 'push' && needs.server-signing-preflight.result == 'skipped'))")
IMAGE_IFS = {
    "publish-build-tools": "!cancelled() && needs.release-plan.result == 'success' && "
                           "needs.release-plan.outputs.ship_server == 'true' && " + PF_OK,
    "publish-vz": "!cancelled() && needs.publish-build-tools.result == 'success' && " + PF_OK,
    "publish-fc": "!cancelled() && needs.publish-build-tools.result == 'success' && " + PF_OK,
}
RELEASE_IF = ("!cancelled() && github.event_name == 'push' && startsWith(github.ref, 'refs/tags/') && "
              "needs.release-plan.result == 'success' && needs.release-plan.outputs.ship_goreleaser == 'true' && "
              "(needs.release-plan.outputs.ship_server != 'true' || "
              "(needs.server-signing-preflight.result == 'success' && "
              "needs.publish-build-tools.result == 'success' && needs.publish-vz.result == 'success' && "
              "needs.publish-fc.result == 'success' && needs.smoke.result == 'success'))")
SIGN_ENV = {
    "SHED_SERVER_SIGNING": "required",
    "SHED_SERVER_SIGN_IDENTITY": "${{ secrets.SHED_DESKTOP_DEVELOPER_ID_IDENTITY }}",
    "SHED_SERVER_TEAM_ID": "${{ secrets.APPLE_TEAM_ID }}",
}
IMPORT_ENV = {
    "MACOS_CERTIFICATE_P12_BASE64": "${{ secrets.MACOS_CERTIFICATE_P12_BASE64 }}",
    "MACOS_CERTIFICATE_PASSWORD": "${{ secrets.MACOS_CERTIFICATE_PASSWORD }}",
}
DRY_RUN_ARGS = "release --snapshot --clean --skip=publish -f .goreleaser.server.yaml"
# The release job's Rust install: its `cargo install` runs third-party build
# scripts, so it (like every step running cargo, rustup or brew) must come
# after the keychain delete, and only on success() (a failed delete skips it).
RUST_STEP = "Rust toolchain + zig cross tooling"
RUST_IF = "success() && needs.release-plan.outputs.ship_host_agent == 'true'"
INSTALLER = re.compile(r"(?m)^[^#\n]*\b(cargo|rustup|brew)\b")


def as_list(v):
    return [v] if isinstance(v, str) else list(v or [])


def joined(run):
    return re.sub(r"[ \t]*\\\n[ \t]*", " ", run or "")


def secrets_in(obj):
    return set(re.findall(r"secrets\.([A-Z0-9_]+)", json.dumps(obj, default=str)))


def named(steps, name):
    return [i for i, s in enumerate(steps) if s.get("name") == name]


def env_is(errors, where, step, want):
    got = {k: " ".join(str(v).split()) for k, v in (step.get("env") or {}).items()}
    if got != want:
        errors.append("%s: env is %s, want exactly %s" % (where, got, want))


def no_continue(errors, where, what, step):
    if step.get("continue-on-error") not in (None, False):
        errors.append("%s: the %s has continue-on-error, which would let its failure pass" % (where, what))


def keychain_steps(errors, where, steps, before):
    """The import step (exactly one, before index `before`) and the delete step
    (exactly one, right after `before`, `if: always()`), as (import, delete).
    None of the three (import, signing step, delete) may continue on error."""
    imp, dele = named(steps, IMPORT_STEP), named(steps, DELETE_STEP)
    if len(imp) != 1 or len(dele) != 1:
        errors.append("%s: %d %r and %d %r steps (want exactly 1 each)" % (where, len(imp), IMPORT_STEP, len(dele), DELETE_STEP))
        return None
    i, d = imp[0], dele[0]
    for what, k in (("certificate import", i), ("signing step", before), ("keychain delete", d)):
        no_continue(errors, where, what, steps[k])
    if i >= before:
        errors.append("%s: the certificate import (step %d) is not before the signing step (step %d)" % (where, i, before))
    if d != before + 1:
        errors.append("%s: the keychain delete is step %d, not right after the signing step (step %d)" % (where, d, before))
    if expr(steps[d].get("if", "")) != "always()":
        errors.append("%s: the keychain delete's if is %r, not always()" % (where, steps[d].get("if")))
    env_is(errors, where + " import", steps[i], IMPORT_ENV)
    if KEYCHAIN not in (steps[i].get("run") or "") or "security delete-keychain" not in (steps[d].get("run") or ""):
        errors.append("%s: the import does not create %s, or the delete does not run security delete-keychain" % (where, KEYCHAIN))
    return steps[i], steps[d]


def check_server(errors, wf, server_script, dry_path):
    jobs = wf.get("jobs") or {}
    with open(server_script) as f:
        m = re.search(r'^NAMES="([^"]*)"', f.read(), re.M)
    four = m.group(1).split() if m else []
    if len(four) != 4 or len(set(four)) != 4:
        errors.append("%s: NAMES is not four distinct secrets: %s" % (SERVER_GATE, four))

    # The release-plan gate step: once, right after the desktop gate, on a tag
    # push that ships server, with the four flags.
    rp = jobs.get("release-plan") or {}
    steps = rp.get("steps") or []
    where = "release-plan (server gate)"
    behind_operator(errors, where, steps, SERVER_GATE)
    found, desktop = gate_indices(steps, SERVER_GATE), gate_indices(steps)
    if len(found) != 1:
        errors.append("%s: %d steps run the server gate (want exactly 1)" % (where, len(found)))
    else:
        i = found[0]
        if len(desktop) != 1 or i != desktop[0] + 1:
            errors.append("%s: the server gate is step %d, not right after the desktop gate (steps %s)" % (where, i, desktop))
        if expr(steps[i].get("if", "")) != SERVER_RP_IF:
            errors.append("%s: the server gate step's if is %r, not %r" % (where, steps[i].get("if"), SERVER_RP_IF))
        check_invocation(errors, where, steps[i], rp, four, SERVER_GATE, "the server gate's four")

    # The preflight: tag push + ship_server, after release-plan; imports the
    # cert, signs a scratch binary the way the hook does and runs the hook's
    # --verify under required signing, then always deletes the keychain. Its
    # secrets are step-scoped and are exactly the gate's four.
    pf = jobs.get(PREFLIGHT)
    where = PREFLIGHT
    if not pf:
        errors.append("%s: no such job" % where)
    else:
        if as_list(pf.get("needs")) != ["release-plan"]:
            errors.append("%s: needs is %r, not release-plan" % (where, pf.get("needs")))
        if expr(pf.get("if", "")) != PREFLIGHT_IF:
            errors.append("%s: if is %r, not %r" % (where, pf.get("if"), PREFLIGHT_IF))
        if pf.get("runs-on") != "macos-latest":
            errors.append("%s: runs-on is %r, not macos-latest" % (where, pf.get("runs-on")))
        if secrets_in(pf.get("env")):
            errors.append("%s: job-level env reads secrets; keep them step-scoped" % where)
        if secrets_in(pf) != set(four):
            errors.append("%s: reads secrets %s, not exactly the gate's four %s" % (where, sorted(secrets_in(pf)), four))
        no_continue(errors, where, "job", pf)
        steps = pf.get("steps") or []
        sign = [i for i, s in enumerate(steps) if "scripts/release/macos-server-app.sh --verify" in (s.get("run") or "")]
        if len(sign) != 1:
            errors.append("%s: %d steps run macos-server-app.sh --verify (want exactly 1)" % (where, len(sign)))
        else:
            s = steps[sign[0]]
            run = joined(s.get("run"))
            for want in ("codesign --force --sign \"$SHED_SERVER_SIGN_IDENTITY\"", "--identifier ai.stridelabs.shed-server",
                         "--entitlements internal/vz/entitlements.plist", "--timestamp"):
                if want not in run:
                    errors.append("%s: the scratch signature lacks %s" % (where, want))
            env_is(errors, where + " sign step", s, SIGN_ENV)
            # No if at all: it runs exactly when the import succeeded (the
            # implicit success()), so an `if: false` cannot skip the proof.
            if "if" in s:
                errors.append("%s: the sign step has an if (%r); it must run whenever the import succeeded" % (where, s["if"]))
            if keychain_steps(errors, where, steps, sign[0]) and sign[0] + 1 != len(steps) - 1:
                errors.append("%s: the keychain delete is not the last step" % where)

    # Every image job needs the preflight, with the skipped-vs-failed rule.
    for job, want in sorted(IMAGE_IFS.items()):
        j = jobs.get(job) or {}
        if PREFLIGHT not in as_list(j.get("needs")):
            errors.append("%s: needs %r does not list %s" % (job, j.get("needs"), PREFLIGHT))
        if expr(j.get("if", "")) != want:
            errors.append("%s: if is %r, not %r" % (job, expr(j.get("if", "")), want))

    # The release job: needs the preflight; imports the cert right before the
    # server goreleaser step (which gets the required-signing env) and deletes
    # the keychain right after it, before the host-agent step; only those two
    # steps read the signing secrets. No third-party build code runs while the
    # cert is imported: the Rust toolchain step sits between the delete and
    # the host-agent step, on success(), and no step before the delete runs
    # cargo, rustup or brew.
    rel = jobs.get("release") or {}
    where = "release"
    if PREFLIGHT not in as_list(rel.get("needs")):
        errors.append("%s: needs %r does not list %s" % (where, rel.get("needs"), PREFLIGHT))
    if expr(rel.get("if", "")) != RELEASE_IF:
        errors.append("%s: if is %r, not %r" % (where, expr(rel.get("if", "")), RELEASE_IF))
    steps = rel.get("steps") or []
    srv = [i for i, s in enumerate(steps) if ".goreleaser.server.yaml" in str((s.get("with") or {}).get("args", ""))]
    ha = [i for i, s in enumerate(steps) if ".goreleaser.host-agent.yaml" in str((s.get("with") or {}).get("args", ""))]
    server_version = (steps[srv[0]].get("with") or {}).get("version") if len(srv) == 1 else None
    if len(srv) != 1 or len(ha) != 1:
        errors.append("%s: %d server and %d host-agent goreleaser steps (want 1 each)" % (where, len(srv), len(ha)))
    else:
        env = {k: v for k, v in (steps[srv[0]].get("env") or {}).items() if k in SIGN_ENV}
        env_is(errors, where + " server goreleaser (signing)", {"env": env}, SIGN_ENV)
        pair = keychain_steps(errors, where, steps, srv[0])
        if pair:
            imp = named(steps, IMPORT_STEP)[0]
            if imp != srv[0] - 1:
                errors.append("%s: the certificate import is step %d, not right before the server goreleaser step (%d)" % (where, imp, srv[0]))
            if expr(steps[imp].get("if", "")) != "success() && needs.release-plan.outputs.ship_server == 'true'":
                errors.append("%s: the certificate import's if is %r, not gated on ship_server" % (where, steps[imp].get("if")))
            if not srv[0] + 1 < ha[0]:
                errors.append("%s: the keychain delete is not before the host-agent goreleaser step" % where)
            d = named(steps, DELETE_STEP)[0]
            rust = named(steps, RUST_STEP)
            if len(rust) != 1:
                errors.append("%s: %d %r steps (want exactly 1)" % (where, len(rust), RUST_STEP))
            else:
                if not d < rust[0] < ha[0]:
                    errors.append("%s: the Rust toolchain step is step %d, not after the keychain delete (step %d) and before the host-agent goreleaser step (step %d); its cargo install runs third-party build scripts" % (where, rust[0], d, ha[0]))
                if expr(steps[rust[0]].get("if", "")) != RUST_IF:
                    errors.append("%s: the Rust toolchain step's if is %r, not %r, so it could run after a failed keychain delete" % (where, steps[rust[0]].get("if"), RUST_IF))
            early = [k for k, s in enumerate(steps[:d + 1]) if INSTALLER.search(s.get("run") or "")]
            if early:
                errors.append("%s: steps %s run cargo, rustup or brew before the keychain delete (step %d), while the certificate is imported" % (where, early, d))
        if secrets_in(rel.get("env")) & set(four):
            errors.append("%s: job-level env reads a signing secret; keep them step-scoped" % where)
        readers = [i for i, s in enumerate(steps) if secrets_in(s) & set(four)]
        if pair and readers != sorted([named(steps, IMPORT_STEP)[0], srv[0]]):
            errors.append("%s: steps %s read the signing secrets; only the import and the server goreleaser step may" % (where, readers))

    # server-signing-dry-run.yml: dispatch-only, read-only, main only; the
    # release job's goreleaser version, snapshot without publishing, signed
    # as required; the keychain deleted right after; the tarballs uploaded for
    # 3 days.
    dr, _ = load(dry_path)
    where = "server-signing-dry-run.yml"
    on = dr.get("on", dr.get(True))
    if not isinstance(on, dict) or list(on) != ["workflow_dispatch"]:
        errors.append("%s: on is %r, not workflow_dispatch alone" % (where, on))
    if dr.get("permissions") != {"contents": "read"}:
        errors.append("%s: permissions are %r, not contents: read" % (where, dr.get("permissions")))
    djobs = list((dr.get("jobs") or {}).values())
    if len(djobs) != 1:
        errors.append("%s: %d jobs (want 1)" % (where, len(djobs)))
    else:
        dj = djobs[0]
        if expr(dj.get("if", "")) != "github.ref == 'refs/heads/main'":
            errors.append("%s: the job's if is %r, not github.ref == 'refs/heads/main'" % (where, dj.get("if")))
        if "permissions" in dj:
            errors.append("%s: the job sets its own permissions" % where)
        if secrets_in(dj) != set(four):
            errors.append("%s: reads secrets %s, not exactly the gate's four %s" % (where, sorted(secrets_in(dj)), four))
        steps = dj.get("steps") or []
        gr = [i for i, s in enumerate(steps) if str(s.get("uses", "")).startswith("goreleaser/goreleaser-action@")]
        up = [i for i, s in enumerate(steps) if str(s.get("uses", "")).startswith("actions/upload-artifact@")]
        ver = [i for i, s in enumerate(steps) if "scripts/release/macos-server-app.sh --verify" in (s.get("run") or "")]
        if len(gr) != 1 or len(up) != 1 or len(ver) != 1:
            errors.append("%s: %d goreleaser, %d upload and %d verify steps (want 1 each)" % (where, len(gr), len(up), len(ver)))
        else:
            g = steps[gr[0]].get("with") or {}
            if g.get("args") != DRY_RUN_ARGS:
                errors.append("%s: goreleaser args are %r, not %r" % (where, g.get("args"), DRY_RUN_ARGS))
            if g.get("version") != server_version:
                errors.append("%s: goreleaser version %r is not the release job's %r" % (where, g.get("version"), server_version))
            env_is(errors, where + " goreleaser", steps[gr[0]], SIGN_ENV)
            keychain_steps(errors, where, steps, gr[0])
            if not gr[0] < ver[0] < up[0]:
                errors.append("%s: the steps are not goreleaser, then verify, then upload" % where)
            u = steps[up[0]].get("with") or {}
            if str(u.get("retention-days")) != "3" or not re.fullmatch(r"dist/server/shed-homebrew_darwin_\*\.tar\.gz", str(u.get("path", ""))):
                errors.append("%s: the upload is %r, not the darwin shed-homebrew tarballs for 3 days" % (where, u))
        imports = [s for s in steps if s.get("name") == IMPORT_STEP]
        deletes = [s for s in steps if s.get("name") == DELETE_STEP]
        # The three copies of the import and the delete stay identical.
        for job in (PREFLIGHT, "release"):
            jsteps = (jobs.get(job) or {}).get("steps") or []
            for name, mine in ((IMPORT_STEP, imports), (DELETE_STEP, deletes)):
                theirs = [s.get("run") for s in jsteps if s.get("name") == name]
                if mine and theirs and theirs != [mine[0].get("run")]:
                    errors.append("%s: the %r step's run differs from %s's" % (where, name, job))


def check(path, gate_script, server_script, dry_path):
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

    check_server(errors, wf, server_script, dry_path)

    for e in errors:
        print("wiring: " + e)
    if errors:
        return 1
    print("wiring OK (parsed with %s): release-plan runs the gate right after release-plan.sh when ship_desktop, desktop-macos right after its checkout, each with the six flags; the server gate follows the desktop gate on a server tag push, every image job and release need %s, and the signing keychain lives only around the server goreleaser step, with no cargo, rustup or brew before its delete" % (loader, PREFLIGHT))
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
    elif not mutate_server(jobs, name):
        sys.exit("mutate: unknown mutation %r" % name)
    with open(dst, "w") as f:
        json.dump(wf, f, indent=1, default=str)
    return 0


def mutate_server(jobs, name):
    """The server-signing mutations (plan 028 D4); False for an unknown name."""
    rp = jobs["release-plan"]["steps"]
    rel = jobs["release"]["steps"]

    def server_gate():
        found = gate_indices(rp, SERVER_GATE)
        if len(found) != 1:
            sys.exit("mutate %s: expected one server gate step, found %d" % (name, len(found)))
        return found[0]

    def by_name(steps, step_name):
        found = named(steps, step_name)
        if len(found) != 1:
            sys.exit("mutate %s: expected one %r step, found %d" % (name, step_name, len(found)))
        return found[0]

    def goreleaser(config):
        found = [i for i, s in enumerate(rel) if config in str((s.get("with") or {}).get("args", ""))]
        if len(found) != 1:
            sys.exit("mutate %s: expected one %s goreleaser step" % (name, config))
        return found[0]

    def replace_if(job, old, new):
        cond = expr(jobs[job].get("if", ""))
        if old not in cond:
            sys.exit("mutate %s: %r is not in %s's if" % (name, old, job))
        jobs[job]["if"] = cond.replace(old, new)

    if name == "srv-delete":
        del rp[server_gate()]
    elif name == "srv-before-desktop":
        step = rp.pop(server_gate())
        rp.insert(gate_indices(rp)[0], step)
    elif name == "srv-any-event":
        rp[server_gate()]["if"] = "steps.plan.outputs.ship_server == 'true'"
    elif name == "srv-drop-flag":
        s = rp[server_gate()]
        new = re.sub(r"[ \t]*\\\n[ \t]*--has APPLE_TEAM_ID=\$\{\{[^}]*\}\}", "", s["run"])
        if new == s["run"]:
            sys.exit("mutate srv-drop-flag: the flag was not found")
        s["run"] = new
    elif name == "pf-on-dispatch":
        jobs[PREFLIGHT]["if"] = "needs.release-plan.outputs.ship_server == 'true'"
    elif name == "pf-no-timestamp":
        s = jobs[PREFLIGHT]["steps"][[i for i, s in enumerate(jobs[PREFLIGHT]["steps"]) if "--timestamp" in (s.get("run") or "")][0]]
        s["run"] = s["run"].replace(" --timestamp", "")
    elif name == "pf-job-secret":
        jobs[PREFLIGHT]["env"] = {"SHED_SERVER_TEAM_ID": "${{ secrets.APPLE_TEAM_ID }}"}
    elif name == "vz-no-preflight-need":
        jobs["publish-vz"]["needs"] = "publish-build-tools"
    elif name == "fc-implicit-success":
        jobs["publish-fc"].pop("if")
    elif name == "bt-skipped-on-push":
        replace_if("publish-build-tools", PF_OK,
                   "(needs.server-signing-preflight.result == 'success' || needs.server-signing-preflight.result == 'skipped')")
    elif name == "bt-failed-ok":
        replace_if("publish-build-tools", PF_OK, "needs.server-signing-preflight.result != 'cancelled'")
    elif name == "rel-no-preflight":
        replace_if("release", "needs.server-signing-preflight.result == 'success' && ", "")
    elif name == "rel-import-early":
        rel.insert(1, rel.pop(by_name(rel, IMPORT_STEP)))
    elif name == "rel-delete-after-host-agent":
        step = rel.pop(by_name(rel, DELETE_STEP))
        rel.insert(goreleaser(".goreleaser.host-agent.yaml") + 1, step)
    elif name == "rel-delete-on-success":
        rel[by_name(rel, DELETE_STEP)]["if"] = "success()"
    elif name == "rel-no-required":
        rel[goreleaser(".goreleaser.server.yaml")]["env"].pop("SHED_SERVER_SIGNING")
    elif name == "rel-identity-to-host-agent":
        rel[goreleaser(".goreleaser.host-agent.yaml")]["env"]["SHED_SERVER_SIGN_IDENTITY"] = SIGN_ENV["SHED_SERVER_SIGN_IDENTITY"]
    elif name in ("pf-sign-if-false", "pf-sign-continue", "pf-import-continue"):
        pf = jobs[PREFLIGHT]["steps"]
        if name == "pf-import-continue":
            pf[by_name(pf, IMPORT_STEP)]["continue-on-error"] = True
        else:
            sign = [i for i, s in enumerate(pf) if "macos-server-app.sh --verify" in (s.get("run") or "")]
            if len(sign) != 1:
                sys.exit("mutate %s: no single sign step" % name)
            if name == "pf-sign-if-false":
                pf[sign[0]]["if"] = False
            else:
                pf[sign[0]]["continue-on-error"] = True
    elif name == "pf-job-continue":
        jobs[PREFLIGHT]["continue-on-error"] = True
    elif name == "rel-server-continue":
        rel[goreleaser(".goreleaser.server.yaml")]["continue-on-error"] = True
    elif name == "rel-import-continue":
        rel[by_name(rel, IMPORT_STEP)]["continue-on-error"] = True
    elif name == "rel-delete-continue":
        rel[by_name(rel, DELETE_STEP)]["continue-on-error"] = True
    elif name == "rel-rust-before-import":
        # Back to where it sat before plan 028's fix: ahead of Run Tests, long
        # before the certificate import.
        step = rel.pop(by_name(rel, RUST_STEP))
        rel.insert(by_name(rel, "Run Tests"), step)
    elif name == "rel-rust-always":
        rel[by_name(rel, RUST_STEP)]["if"] = "always() && needs.release-plan.outputs.ship_host_agent == 'true'"
    elif name == "rel-cargo-before-import":
        rel.insert(by_name(rel, IMPORT_STEP), {"name": "Install a tool", "run": "cargo install --locked --version 1.2.3 some-tool"})
    else:
        return False
    return True


def mutate_dry(src, dst, name):
    """Mutations of server-signing-dry-run.yml."""
    wf, _ = load(src)
    on = wf.pop(True, None)
    if on is not None:
        wf["on"] = on
    job = list(wf["jobs"].values())[0]
    gr = [s for s in job["steps"] if str(s.get("uses", "")).startswith("goreleaser/goreleaser-action@")][0]
    if name == "none":
        pass
    elif name == "dr-publishes":
        gr["with"]["args"] = "release --clean -f .goreleaser.server.yaml"
    elif name == "dr-write-token":
        wf["permissions"] = {"contents": "write"}
    elif name == "dr-any-branch":
        job.pop("if")
    elif name == "dr-other-version":
        gr["with"]["version"] = "latest"
    else:
        sys.exit("mutate-dry: unknown mutation %r" % name)
    with open(dst, "w") as f:
        json.dump(wf, f, indent=1, default=str)
    return 0


def extract(src, job, dst, missing, gate=GATE):
    """Write the job's gate step's run script with each ${{ secrets.X != '' }}
    replaced by false (X listed in `missing`) or true, as GitHub renders it."""
    wf, _ = load(src)
    steps = wf["jobs"][job]["steps"]
    found = gate_indices(steps, gate)
    if len(found) != 1:
        sys.exit("extract %s: expected one gate step, found %d" % (job, len(found)))
    run = re.sub(r"\$\{\{ secrets\.([A-Z0-9_]+) != '' \}\}",
                 lambda m: "false" if m.group(1) in missing else "true", steps[found[0]]["run"])
    if "${{" in run:
        sys.exit("extract %s: an expression is left unrendered in the step" % job)
    with open(dst, "w") as f:
        f.write(run)
    return 0


def extract_step(src, job, step_name, dst, env_dst):
    """Write the named step's run script, and its env as NAME=value lines with
    each ${{ secrets.X }} rendered from this process's SECRET_X."""
    wf, _ = load(src)
    found = [s for s in wf["jobs"][job]["steps"] if s.get("name") == step_name]
    if len(found) != 1:
        sys.exit("extract-step %s: expected one %r step, found %d" % (job, step_name, len(found)))
    run = found[0]["run"]
    if "${{" in run:
        sys.exit("extract-step %s: an expression is left unrendered in the step" % job)
    lines = []
    for k, v in (found[0].get("env") or {}).items():
        v = re.sub(r"\$\{\{ secrets\.([A-Z0-9_]+) \}\}", lambda m: os.environ["SECRET_" + m.group(1)], str(v))
        if "${{" in v or "\n" in v:
            sys.exit("extract-step %s: env %s is not a plain value or one secret" % (job, k))
        lines.append("%s=%s\n" % (k, v))
    with open(dst, "w") as f:
        f.write(run)
    with open(env_dst, "w") as f:
        f.writelines(lines)
    return 0


if __name__ == "__main__":
    if len(sys.argv) == 6 and sys.argv[1] == "check":
        sys.exit(check(sys.argv[2], sys.argv[3], sys.argv[4], sys.argv[5]))
    if len(sys.argv) == 5 and sys.argv[1] == "mutate":
        sys.exit(mutate(sys.argv[2], sys.argv[3], sys.argv[4]))
    if len(sys.argv) == 5 and sys.argv[1] == "mutate-dry":
        sys.exit(mutate_dry(sys.argv[2], sys.argv[3], sys.argv[4]))
    if len(sys.argv) >= 5 and sys.argv[1] == "extract":
        sys.exit(extract(sys.argv[2], sys.argv[3], sys.argv[4], set(sys.argv[5:])))
    if len(sys.argv) >= 4 and sys.argv[1] == "extract-server-gate":
        sys.exit(extract(sys.argv[2], "release-plan", sys.argv[3], set(sys.argv[4:]), SERVER_GATE))
    if len(sys.argv) == 7 and sys.argv[1] == "extract-step":
        sys.exit(extract_step(*sys.argv[2:7]))
    sys.exit("usage: wiring.py check <workflow> <gate-script> <server-gate-script> <dry-run-workflow>"
             " | mutate <in> <out> <mutation> | mutate-dry <in> <out> <mutation>"
             " | extract <workflow> <job> <out> [missing secret...]"
             " | extract-server-gate <workflow> <out> [missing secret...]"
             " | extract-step <workflow> <job> <step name> <out> <env out>")
PY

WF="${SCRATCH}/.github/workflows/publish-images.yaml"
SERVER_GATE="${SCRATCH}/scripts/release/server-signing-gate.sh"
DRY_RUN_WF="${SCRATCH}/.github/workflows/server-signing-dry-run.yml"
WIRING_OUT=""; WIRING_RC=0
# run_wiring <publish-images workflow> [<dry-run workflow>]
run_wiring() {
  WIRING_RC=0
  WIRING_OUT="$(python3 "${WIRING}" check "$1" "${GATE}" "${SERVER_GATE}" "${2:-${DRY_RUN_WF}}" 2>&1)" || WIRING_RC=$?
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

# ===========================================================================
# Phase 6 (plan 028 D4, #304): macos-server-app.sh, the shed-server build's
# goreleaser hook that signs the darwin binary and assembles ShedServer.app,
# plus its standalone --verify. It runs as deployed (the scratch copy, so its
# staging lands under ${SCRATCH}/dist), with stub codesign, plutil and uname
# ahead of the real tools on PATH: the stub uname plays the host OS, and the
# stub codesign reports the signature each case needs (STUB_SIGNATURE
# adhoc|devid, STUB_TEAM, STUB_TIMESTAMP yes|no, STUB_ENTITLEMENTS virt|extra).
# Every stub logs its calls, so a case can prove nothing was signed.
# ===========================================================================

MSA="${SCRATCH}/scripts/release/macos-server-app.sh"
MSA_STUBS="${SCRATCH}/msa-stubs"
MSA_LOG="${SCRATCH}/msa-calls.log"
MSA_BIN="${SCRATCH}/dist/server/shed-server_darwin_arm64_v8.0/shed-server"
MSA_APP="${SCRATCH}/dist/server/macos-app/darwin_arm64/ShedServer.app"
MSA_ENTITLEMENTS="${SCRATCH}/internal/vz/entitlements.plist"
MSA_TEAM="TEAM123456"
MSA_IDENTITY="Developer ID Application: Example Dev (${MSA_TEAM})"
mkdir -p "${MSA_STUBS}"

cat > "${MSA_STUBS}/codesign" <<'STUB'
#!/bin/sh
echo "codesign $*" >> "${STUB_LOG:-/dev/null}"
for last in "$@"; do :; done
case " $* " in
  *" --verify "*) exit 0 ;;
  *" --sign "*)
    if [ -d "${last}" ]; then
      mkdir -p "${last}/Contents/_CodeSignature"
      echo sealed > "${last}/Contents/_CodeSignature/CodeResources"
    fi
    exit 0 ;;
  *" -r- "*)
    echo "Executable=${last}" >&2
    if [ "${STUB_SIGNATURE:-devid}" = devid ]; then
      echo "designated => identifier \"ai.stridelabs.shed-server\" and anchor apple generic and certificate 1[field.1.2.840.113635.100.6.2.6] /* exists */ and certificate leaf[field.1.2.840.113635.100.6.1.13] /* exists */ and certificate leaf[subject.OU] = ${STUB_TEAM:-TEAM123456}"
    else
      echo 'designated => cdhash H"0123456789abcdef0123456789abcdef01234567"'
    fi
    exit 0 ;;
  *" --entitlements "*)
    echo "Executable=${last}" >&2
    printf '<?xml version="1.0" encoding="UTF-8"?>\n<plist version="1.0"><dict><key>com.apple.security.virtualization</key><true/>'
    if [ "${STUB_ENTITLEMENTS:-virt}" = extra ]; then
      printf '<key>com.apple.security.cs.disable-library-validation</key><true/>'
    fi
    printf '</dict></plist>\n'
    exit 0 ;;
  *" -d "*)
    {
      echo "Executable=${last}"
      echo "Identifier=ai.stridelabs.shed-server"
      if [ "${STUB_SIGNATURE:-devid}" = devid ]; then
        echo "Authority=Developer ID Application: Example Dev (${STUB_TEAM:-TEAM123456})"
        echo "Authority=Developer ID Certification Authority"
        echo "Authority=Apple Root CA"
        if [ "${STUB_TIMESTAMP:-yes}" = yes ]; then
          echo "Timestamp=Oct 11, 2026 at 10:00:00"
        else
          echo "Signed Time=Oct 11, 2026 at 10:00:00"
        fi
        echo "TeamIdentifier=${STUB_TEAM:-TEAM123456}"
      else
        echo "Signature=adhoc"
        echo "TeamIdentifier=not set"
      fi
    } >&2
    exit 0 ;;
esac
echo "stub codesign: unexpected call: $*" >&2
exit 64
STUB

cat > "${MSA_STUBS}/plutil" <<'STUB'
#!/bin/sh
echo "plutil $*" >> "${STUB_LOG:-/dev/null}"
case "$1" in
  -lint) echo "$2: OK"; exit 0 ;;
  -extract)
    # -extract KEY raw -o - FILE: the <string> on the line after <key>KEY</key>.
    awk -v k="<key>$2</key>" 'f { sub(/.*<string>/, ""); sub(/<\/string>.*/, ""); print; exit } index($0, k) { f = 1 }' "$6"
    exit 0 ;;
  -convert)
    # -convert json -o - FILE
    if grep -q 'disable-library-validation' "$5"; then
      printf '{"com.apple.security.cs.disable-library-validation":true,"com.apple.security.virtualization":true}'
    elif grep -q '<key>com.apple.security.virtualization</key>' "$5"; then
      printf '{"com.apple.security.virtualization":true}'
    else
      exit 1
    fi
    exit 0 ;;
esac
echo "stub plutil: unexpected call: $*" >&2
exit 64
STUB

cat > "${MSA_STUBS}/uname" <<'STUB'
#!/bin/sh
echo "uname $*" >> "${STUB_LOG:-/dev/null}"
echo "${STUB_UNAME:-Darwin}"
STUB
chmod 0755 "${MSA_STUBS}/codesign" "${MSA_STUBS}/plutil" "${MSA_STUBS}/uname"

# msa_reset: a fresh dummy darwin binary, no staged bundle.
msa_reset() {
  rm -rf "${SCRATCH}/dist"
  mkdir -p "$(dirname "${MSA_BIN}")"
  printf 'not really a Mach-O\n' > "${MSA_BIN}"
}
# run_msa [NAME=value...] <script> <args...> → MSA_OUT (stdout+stderr), MSA_RC.
# The signing variables are cleared first, so a case sets exactly what it names.
MSA_OUT=""; MSA_RC=0
run_msa() {
  MSA_RC=0
  : > "${MSA_LOG}"
  MSA_OUT="$(env -u SHED_SERVER_SIGNING -u SHED_SERVER_SIGN_IDENTITY -u SHED_SERVER_TEAM_ID \
    PATH="${MSA_STUBS}:${PATH}" STUB_LOG="${MSA_LOG}" "$@" 2>&1)" || MSA_RC=$?
}
msa_signed_nothing() {
  if grep -q -- '--sign' "${MSA_LOG}"; then
    fail "$1: codesign signed something: $(cat "${MSA_LOG}")"
  fi
}
REQUIRED_ENV=(SHED_SERVER_SIGNING=required "SHED_SERVER_SIGN_IDENTITY=${MSA_IDENTITY}" "SHED_SERVER_TEAM_ID=${MSA_TEAM}")

step "macos-server-app.sh: a non-darwin target is a no-op, before any host check"
msa_reset
run_msa SHED_SERVER_SIGNING=required STUB_UNAME=Linux "${MSA}" "${SCRATCH}/dist/server/shed-server_linux_arm64_v8.0/shed-server" linux arm64 0.9.1
[ "${MSA_RC}" -eq 0 ] || fail "linux target exited ${MSA_RC} (want 0): ${MSA_OUT}"
[ -z "${MSA_OUT}" ] || fail "linux target printed output: ${MSA_OUT}"
[ ! -s "${MSA_LOG}" ] || fail "linux target ran a tool (uname, codesign or plutil): $(cat "${MSA_LOG}")"
[ ! -e "${SCRATCH}/dist/server/macos-app" ] || fail "linux target staged a bundle"
ok "linux target with required signing and nothing else set → exit 0, silent, no tool run, no bundle"

step "macos-server-app.sh: required signing without an identity, or without a team, fails before signing"
msa_reset
run_msa SHED_SERVER_SIGNING=required "SHED_SERVER_TEAM_ID=${MSA_TEAM}" "${MSA}" "${MSA_BIN}" darwin arm64 0.9.1
[ "${MSA_RC}" -eq 1 ] || fail "required without an identity exited ${MSA_RC} (want 1): ${MSA_OUT}"
printf '%s\n' "${MSA_OUT}" | grep -qF "SHED_SERVER_SIGNING=required needs: SHED_SERVER_SIGN_IDENTITY" \
  || fail "required without an identity does not name SHED_SERVER_SIGN_IDENTITY: ${MSA_OUT}"
msa_signed_nothing "required without an identity"
run_msa SHED_SERVER_SIGNING=required "SHED_SERVER_SIGN_IDENTITY=${MSA_IDENTITY}" "${MSA}" "${MSA_BIN}" darwin arm64 0.9.1
[ "${MSA_RC}" -eq 1 ] || fail "required without a team exited ${MSA_RC} (want 1): ${MSA_OUT}"
printf '%s\n' "${MSA_OUT}" | grep -qF "SHED_SERVER_SIGNING=required needs: SHED_SERVER_TEAM_ID" \
  || fail "required without a team does not name SHED_SERVER_TEAM_ID: ${MSA_OUT}"
msa_signed_nothing "required without a team"
[ ! -e "${SCRATCH}/dist/server/macos-app" ] || fail "required without an identity or team staged a bundle"
ok "no identity → exit 1 naming SHED_SERVER_SIGN_IDENTITY; no team → exit 1 naming SHED_SERVER_TEAM_ID; nothing signed"

step "macos-server-app.sh: required signing on a non-macOS host fails"
msa_reset
run_msa "${REQUIRED_ENV[@]}" STUB_UNAME=Linux "${MSA}" "${MSA_BIN}" darwin arm64 0.9.1
[ "${MSA_RC}" -eq 1 ] || fail "required on a Linux host exited ${MSA_RC} (want 1): ${MSA_OUT}"
printf '%s\n' "${MSA_OUT}" | grep -qF "needs: a macOS host (this host is Linux)" \
  || fail "required on a Linux host does not say it needs macOS: ${MSA_OUT}"
msa_signed_nothing "required on a Linux host"
ok "required on Linux → exit 1, nothing signed"

# msa_required_refused <label> <expected message> <stub settings...>
msa_required_refused() {
  local label="$1" want="$2"
  shift 2
  msa_reset
  run_msa "${REQUIRED_ENV[@]}" "$@" "${MSA}" "${MSA_BIN}" darwin arm64 0.9.1
  [ "${MSA_RC}" -eq 1 ] || fail "required with ${label} exited ${MSA_RC} (want 1): ${MSA_OUT}"
  printf '%s\n' "${MSA_OUT}" | grep -qF "${want}" || fail "required with ${label}: no '${want}' in: ${MSA_OUT}"
  ok "required with ${label} → exit 1 (${want})"
}
step "macos-server-app.sh: required signing refuses a signature that is not a timestamped Developer ID one from the team"
msa_required_refused "an ad hoc signature" "no signing Authority (an ad hoc signature?)" STUB_SIGNATURE=adhoc
msa_required_refused "the wrong team" "TeamIdentifier is 'WRONGTEAM9', which is not SHED_SERVER_TEAM_ID" STUB_TEAM=WRONGTEAM9
msa_required_refused "no secure timestamp" "no secure Timestamp" STUB_TIMESTAMP=no
msa_required_refused "an extra entitlement" "want exactly {\"com.apple.security.virtualization\":true}" STUB_ENTITLEMENTS=extra

step "macos-server-app.sh: required signing with a matching Developer ID passes and stages the bundle"
msa_reset
run_msa "${REQUIRED_ENV[@]}" "${MSA}" "${MSA_BIN}" darwin arm64 0.9.1-SNAPSHOT-abc1234
[ "${MSA_RC}" -eq 0 ] || fail "required with a matching Developer ID exited ${MSA_RC} (want 0): ${MSA_OUT}"
grep -qF -- "--sign ${MSA_IDENTITY} --timestamp --identifier ai.stridelabs.shed-server --entitlements ${MSA_ENTITLEMENTS} ${MSA_BIN}" "${MSA_LOG}" \
  || fail "the raw binary was not signed in place with the identity, --timestamp, the identifier and the entitlements: $(cat "${MSA_LOG}")"
grep -qF -- "--sign ${MSA_IDENTITY} --timestamp --entitlements ${MSA_ENTITLEMENTS} ${MSA_APP}" "${MSA_LOG}" \
  || fail "the bundle was not signed with the identity, --timestamp and the entitlements: $(cat "${MSA_LOG}")"
[ -f "${MSA_APP}/Contents/Info.plist" ] || fail "no Contents/Info.plist staged"
[ -f "${MSA_APP}/Contents/_CodeSignature/CodeResources" ] || fail "no _CodeSignature/CodeResources staged"
[ -n "$(find "${MSA_APP}/Contents/MacOS/shed-server" -perm 0755)" ] || fail "Contents/MacOS/shed-server is not mode 0755"
[ -n "$(find "${MSA_APP}/Contents/Info.plist" -perm 0644)" ] || fail "Contents/Info.plist is not mode 0644"
cmp -s "${MSA_BIN}" "${MSA_APP}/Contents/MacOS/shed-server" || fail "the bundle executable is not a copy of the binary"
[ "$(grep -c '<string>0\.9\.1</string>' "${MSA_APP}/Contents/Info.plist")" -eq 2 ] \
  || fail "CFBundleVersion and CFBundleShortVersionString are not both the stripped 0.9.1: $(cat "${MSA_APP}/Contents/Info.plist")"
grep -q SNAPSHOT "${MSA_APP}/Contents/Info.plist" && fail "the snapshot suffix leaked into Info.plist"
for kv in "CFBundleIdentifier=ai.stridelabs.shed-server" "CFBundleExecutable=shed-server" "CFBundlePackageType=APPL" "CFBundleName=Shed Server" "CFBundleDisplayName=Shed Server"; do
  got="$(env PATH="${MSA_STUBS}:${PATH}" plutil -extract "${kv%%=*}" raw -o - "${MSA_APP}/Contents/Info.plist")"
  [ "${got}" = "${kv#*=}" ] || fail "Info.plist ${kv%%=*} is '${got}', want '${kv#*=}'"
done
grep -A1 '<key>LSUIElement</key>' "${MSA_APP}/Contents/Info.plist" | grep -q '<true/>' || fail "Info.plist LSUIElement is not true"
[ "$(printf '%s\n' "${MSA_OUT}" | grep -c "(ai.stridelabs.shed-server, Developer ID, team ${MSA_TEAM}, timestamped)")" -eq 2 ] \
  || fail "the raw binary and the bundle were not both verified as Developer ID: ${MSA_OUT}"
ok "matching Developer ID → exit 0; both signed with --timestamp; bundle staged (0755 executable, Info.plist 0.9.1, ID, LSUIElement); both verified"

step "macos-server-app.sh: ad hoc signing on macOS signs with '-' and no --timestamp"
msa_reset
run_msa STUB_SIGNATURE=adhoc "SHED_SERVER_SIGN_IDENTITY=${MSA_IDENTITY}" "${MSA}" "${MSA_BIN}" darwin arm64 0.9.1
[ "${MSA_RC}" -eq 0 ] || fail "ad hoc on macOS exited ${MSA_RC} (want 0): ${MSA_OUT}"
grep -qF -- "--sign - --identifier ai.stridelabs.shed-server --entitlements ${MSA_ENTITLEMENTS} ${MSA_BIN}" "${MSA_LOG}" \
  || fail "the ad hoc raw-binary signature is wrong: $(cat "${MSA_LOG}")"
grep -qF -- "--sign - --entitlements ${MSA_ENTITLEMENTS} ${MSA_APP}" "${MSA_LOG}" || fail "the ad hoc bundle signature is wrong: $(cat "${MSA_LOG}")"
grep -qF -- "--timestamp" "${MSA_LOG}" && fail "ad hoc signing asked for a timestamp: $(cat "${MSA_LOG}")"
[ -f "${MSA_APP}/Contents/MacOS/shed-server" ] || fail "ad hoc on macOS staged no bundle"
ok "ad hoc → '--sign -' without --timestamp (a set identity is ignored), bundle staged"

step "macos-server-app.sh: ad hoc on a non-macOS host warns and builds no bundle"
msa_reset
run_msa STUB_UNAME=Linux "${MSA}" "${MSA_BIN}" darwin arm64 0.9.1
[ "${MSA_RC}" -eq 0 ] || fail "ad hoc on Linux exited ${MSA_RC} (want 0): ${MSA_OUT}"
printf '%s\n' "${MSA_OUT}" | grep -qF "warning: Linux host, so no ShedServer.app for darwin/arm64" || fail "ad hoc on Linux did not warn: ${MSA_OUT}"
msa_signed_nothing "ad hoc on Linux"
[ ! -e "${SCRATCH}/dist/server/macos-app" ] || fail "ad hoc on Linux staged a bundle"
ok "ad hoc on Linux → exit 0 with a warning, nothing signed, no bundle"

step "macos-server-app.sh: an unknown SHED_SERVER_SIGNING value fails closed"
msa_reset
run_msa SHED_SERVER_SIGNING=Required "${MSA}" "${MSA_BIN}" darwin arm64 0.9.1
[ "${MSA_RC}" -eq 1 ] || fail "SHED_SERVER_SIGNING=Required exited ${MSA_RC} (want 1): ${MSA_OUT}"
printf '%s\n' "${MSA_OUT}" | grep -qF "SHED_SERVER_SIGNING must be 'required' or unset" || fail "unknown mode message missing: ${MSA_OUT}"
msa_signed_nothing "an unknown SHED_SERVER_SIGNING"
ok "'Required' (a typo) → exit 1, not a silent ad hoc build"

step "macos-server-app.sh --verify: the step-6 checks alone, on a bundle and on a Mach-O"
msa_reset
run_msa STUB_SIGNATURE=adhoc "${MSA}" "${MSA_BIN}" darwin arm64 0.9.1
[ "${MSA_RC}" -eq 0 ] || fail "staging a bundle for --verify exited ${MSA_RC}: ${MSA_OUT}"
run_msa SHED_SERVER_SIGNING=required "SHED_SERVER_TEAM_ID=${MSA_TEAM}" "${MSA}" --verify "${MSA_APP}"
[ "${MSA_RC}" -eq 0 ] || fail "--verify (required, Developer ID) on the bundle exited ${MSA_RC} (want 0): ${MSA_OUT}"
printf '%s\n' "${MSA_OUT}" | grep -qF "verified ${MSA_APP} (ai.stridelabs.shed-server, Developer ID, team ${MSA_TEAM}, timestamped)" \
  || fail "--verify did not report the Developer ID bundle: ${MSA_OUT}"
grep -q -- '-lint' "${MSA_LOG}" || fail "--verify on a bundle did not lint Info.plist: $(cat "${MSA_LOG}")"
msa_signed_nothing "--verify"
run_msa SHED_SERVER_SIGNING=required "SHED_SERVER_TEAM_ID=${MSA_TEAM}" STUB_SIGNATURE=adhoc "${MSA}" --verify "${MSA_BIN}"
[ "${MSA_RC}" -eq 1 ] || fail "--verify (required) on an ad hoc Mach-O exited ${MSA_RC} (want 1): ${MSA_OUT}"
run_msa SHED_SERVER_SIGNING=required "SHED_SERVER_TEAM_ID=${MSA_TEAM}" STUB_TEAM=WRONGTEAM9 "${MSA}" --verify "${MSA_APP}"
[ "${MSA_RC}" -eq 1 ] || fail "--verify (required) on a wrong-team bundle exited ${MSA_RC} (want 1): ${MSA_OUT}"
run_msa SHED_SERVER_SIGNING=required "${MSA}" --verify "${MSA_APP}"
[ "${MSA_RC}" -eq 1 ] || fail "--verify (required) without SHED_SERVER_TEAM_ID exited ${MSA_RC} (want 1): ${MSA_OUT}"
printf '%s\n' "${MSA_OUT}" | grep -qF "needs SHED_SERVER_TEAM_ID" || fail "--verify without a team does not name it: ${MSA_OUT}"
run_msa STUB_SIGNATURE=adhoc "${MSA}" --verify "${MSA_BIN}"
[ "${MSA_RC}" -eq 0 ] || fail "--verify (ad hoc) on the Mach-O exited ${MSA_RC} (want 0): ${MSA_OUT}"
printf '%s\n' "${MSA_OUT}" | grep -qF "verified ${MSA_BIN} (ai.stridelabs.shed-server, ad hoc)" || fail "--verify (ad hoc) report missing: ${MSA_OUT}"
grep -q -- '-lint' "${MSA_LOG}" && fail "--verify on a Mach-O linted an Info.plist it does not have: $(cat "${MSA_LOG}")"
run_msa "${MSA}" --verify "${SCRATCH}/no-such-thing"
[ "${MSA_RC}" -eq 1 ] || fail "--verify on a missing path exited ${MSA_RC} (want 1): ${MSA_OUT}"
run_msa "${MSA}" --verify
[ "${MSA_RC}" -eq 2 ] || fail "--verify with no path exited ${MSA_RC} (want 2): ${MSA_OUT}"
run_msa "${MSA}" --verify "${MSA_APP}" "${MSA_BIN}"
[ "${MSA_RC}" -eq 2 ] || fail "--verify with two paths exited ${MSA_RC} (want 2): ${MSA_OUT}"
run_msa "${MSA}" "${MSA_BIN}" darwin arm64
[ "${MSA_RC}" -eq 2 ] || fail "the hook with three arguments exited ${MSA_RC} (want 2): ${MSA_OUT}"
ok "--verify: Developer ID bundle → 0; ad hoc or wrong team under required → 1; required without a team → 1; ad hoc Mach-O → 0 (no plist lint); missing path → 1; bad usage → 2"

# ===========================================================================
# Phase 7 (plan 028 D4, #304): the server's release signing fails closed.
# server-signing-gate.sh runs as deployed. Phase 5's wiring guard already
# passed the real workflows, server rules included (check_server); the
# mutation cells here prove each of those rules bites. Then the release-plan
# server gate step and the preflight's signing step are EXECUTED, the latter
# against Phase 6's stub codesign/plutil.
# ===========================================================================

SGATE_SECRETS="MACOS_CERTIFICATE_P12_BASE64 MACOS_CERTIFICATE_PASSWORD SHED_DESKTOP_DEVELOPER_ID_IDENTITY APPLE_TEAM_ID"
SGATE_REFUSAL="::error::a server tag requires a Developer ID signed ShedServer.app; missing:"
SGATE_PASS="has all four signing secrets"
# sgate_flags "<names to mark false>" → SGATE_FLAGS, all four `--has NAME=…` pairs.
SGATE_FLAGS=()
sgate_flags() {
  SGATE_FLAGS=()
  local n m v
  for n in ${SGATE_SECRETS}; do
    v=true
    for m in $1; do
      if [ "${m}" = "${n}" ]; then v=false; fi
    done
    SGATE_FLAGS+=(--has "${n}=${v}")
  done
}
# run_sgate <args...> → SG_OUT (stdout+stderr), SG_RC.
SG_OUT=""; SG_RC=0
run_sgate() {
  SG_RC=0
  SG_OUT="$("${SERVER_GATE}" "$@" 2>&1)" || SG_RC=$?
}

for n in ${SGATE_SECRETS}; do
  step "server-signing-gate.sh: a stable tag missing only ${n} exits 1, naming exactly it"
  sgate_flags "${n}"
  run_sgate v9.1.0 "${SGATE_FLAGS[@]}"
  [ "${SG_RC}" -eq 1 ] || fail "stable tag missing ${n} exited ${SG_RC} (want 1): ${SG_OUT}"
  printf '%s\n' "${SG_OUT}" | grep -qxF "${SGATE_REFUSAL} ${n}" \
    || fail "stable tag missing ${n}: the refusal does not name exactly ${n}: ${SG_OUT}"
  ok "missing ${n} → exit 1, \"missing: ${n}\" and nothing else"
done

step "server-signing-gate.sh: all four present exits 0; all four missing exits 1, naming all four"
sgate_flags ""
run_sgate v9.1.0 "${SGATE_FLAGS[@]}"
[ "${SG_RC}" -eq 0 ] || fail "stable tag with all four exited ${SG_RC} (want 0): ${SG_OUT}"
echo "${SG_OUT}" | grep -qF "${SGATE_PASS}" || fail "all-present pass line missing: ${SG_OUT}"
echo "${SG_OUT}" | grep -q "missing:" && fail "all-present run still reports something missing: ${SG_OUT}"
sgate_flags "${SGATE_SECRETS}"
run_sgate v9.1.0 "${SGATE_FLAGS[@]}"
[ "${SG_RC}" -eq 1 ] || fail "stable tag missing all four exited ${SG_RC} (want 1): ${SG_OUT}"
printf '%s\n' "${SG_OUT}" | grep -qxF "${SGATE_REFUSAL} $(comma_list "${SGATE_SECRETS}")" \
  || fail "the all-missing refusal does not name all four in order: ${SG_OUT}"
ok "all four present → exit 0; all four missing → exit 1, all four named"

step "server-signing-gate.sh: a prerelease fails closed too (unlike the desktop gate: the server has no ad hoc release)"
sgate_flags "${SGATE_SECRETS}"
run_sgate v9.1.0-rc.1 "${SGATE_FLAGS[@]}"
[ "${SG_RC}" -eq 1 ] || fail "prerelease missing all four exited ${SG_RC} (want 1): ${SG_OUT}"
printf '%s\n' "${SG_OUT}" | grep -qxF "${SGATE_REFUSAL} $(comma_list "${SGATE_SECRETS}")" \
  || fail "the prerelease refusal does not name all four: ${SG_OUT}"
echo "${SG_OUT}" | grep -qF "a prerelease (server tags are stable-only" || fail "the prerelease refusal does not say server tags are stable-only: ${SG_OUT}"
sgate_flags ""
run_sgate v9.1.0-rc.1 "${SGATE_FLAGS[@]}"
[ "${SG_RC}" -eq 0 ] || fail "prerelease with all four exited ${SG_RC} (want 0): ${SG_OUT}"
ok "v9.1.0-rc.1: all four missing → exit 1; all four present → exit 0"

step "server-signing-gate.sh: malformed flags and tags exit 2"
sgate_flags ""
smalformed_case() {
  # $1 label, rest = full argv
  local label="$1"; shift
  run_sgate "$@"
  [ "${SG_RC}" -eq 2 ] || fail "malformed (${label}) exited ${SG_RC} (want 2): ${SG_OUT}"
  ok "malformed: ${label} → exit 2"
}
smalformed_case "a value that is not true|false" v9.1.0 "${SGATE_FLAGS[@]:0:6}" --has APPLE_TEAM_ID=yes
smalformed_case "a flag with no =" v9.1.0 "${SGATE_FLAGS[@]:0:6}" --has APPLE_TEAM_ID
smalformed_case "a desktop-only secret (APPLE_ID) the server gate does not take" v9.1.0 "${SGATE_FLAGS[@]}" --has APPLE_ID=true
smalformed_case "a name given twice" v9.1.0 "${SGATE_FLAGS[@]}" --has APPLE_TEAM_ID=true
smalformed_case "a dangling --has" v9.1.0 "${SGATE_FLAGS[@]}" --has
smalformed_case "a bare argument" v9.1.0 "${SGATE_FLAGS[@]}" APPLE_TEAM_ID=true
smalformed_case "a tag that is not vX.Y.Z" 9.1 "${SGATE_FLAGS[@]}"
smalformed_case "no arguments at all"
run_sgate v9.1.0 "${SGATE_FLAGS[@]:0:6}"
[ "${SG_RC}" -eq 2 ] || fail "malformed (only three flags) exited ${SG_RC} (want 2): ${SG_OUT}"
echo "${SG_OUT}" | grep -q "no --has flag for: APPLE_TEAM_ID" || fail "three-flag refusal doesn't name the absent flag: ${SG_OUT}"
ok "malformed: only three of the four flags → exit 2, naming the absent one"

step "server-signing-gate.sh: a secret-shaped value is never echoed, wherever it is wired"
ssentinel_case() {
  # $1 label, rest = argv
  local label="$1"; shift
  run_sgate "$@"
  [ "${SG_RC}" -eq 2 ] || fail "sentinel (${label}) exited ${SG_RC} (want 2): ${SG_OUT}"
  case "${SG_OUT}" in *"${SENTINEL}"*) fail "sentinel (${label}): the output echoed the value: ${SG_OUT}" ;; esac
  ok "sentinel ${label}: exit 2, value never echoed"
}
ssentinel_case "as a flag value" v9.1.0 "${SGATE_FLAGS[@]:0:6}" --has "APPLE_TEAM_ID=${SENTINEL}"
ssentinel_case "as a flag name" v9.1.0 "${SGATE_FLAGS[@]}" --has "${SENTINEL}=true"
ssentinel_case "as a bare argument" v9.1.0 "${SGATE_FLAGS[@]}" "${SENTINEL}"
ssentinel_case "as the tag" "${SENTINEL}" "${SGATE_FLAGS[@]}"
sgate_flags "${SGATE_SECRETS}"
SG_RC=0
SG_OUT="$(MACOS_CERTIFICATE_P12_BASE64="${SENTINEL}" MACOS_CERTIFICATE_PASSWORD="${SENTINEL}" \
  SHED_DESKTOP_DEVELOPER_ID_IDENTITY="${SENTINEL}" APPLE_TEAM_ID="${SENTINEL}" \
  "${SERVER_GATE}" v9.1.0 "${SGATE_FLAGS[@]}" 2>&1)" || SG_RC=$?
[ "${SG_RC}" -eq 1 ] || fail "sentinel (in the environment) exited ${SG_RC} (want 1): ${SG_OUT}"
case "${SG_OUT}" in *"${SENTINEL}"*) fail "sentinel (in the environment): echoed: ${SG_OUT}" ;; esac
ok "sentinel in the four env vars: ignored (exit 1 from the flags alone), never echoed"

step "the wiring guard, server rules: the real publish-images.yaml and server-signing-dry-run.yml pass"
run_wiring "${WF}"
[ "${WIRING_RC}" -eq 0 ] || fail "the wiring guard rejects the real workflows (rc=${WIRING_RC}): ${WIRING_OUT}"
python3 "${WIRING}" mutate-dry "${DRY_RUN_WF}" "${WIRING_DIR}/dr-none.json" none || fail "mutate-dry none failed"
run_wiring "${WF}" "${WIRING_DIR}/dr-none.json"
[ "${WIRING_RC}" -eq 0 ] || fail "the unmutated dry-run round-trip is rejected (rc=${WIRING_RC}): ${WIRING_OUT}"
ok "server gate after the desktop gate, the preflight and its needs/if wiring, the release keychain window (the Rust install after it) and the dry run all pass (and the dry-run round-trip baseline)"

step "the wiring guard, server rules: moving, weakening or dropping any of them turns it red"
mutation_case srv-delete "release-plan (server gate): 0 steps run the server gate"
mutation_case srv-before-desktop "release-plan (server gate): the server gate is step 2, not right after the desktop gate"
mutation_case srv-any-event "release-plan (server gate): the server gate step's if is"
mutation_case srv-drop-flag "release-plan (server gate): --has flags"
mutation_case pf-on-dispatch "server-signing-preflight: if is"
mutation_case pf-no-timestamp "server-signing-preflight: the scratch signature lacks --timestamp"
mutation_case pf-job-secret "server-signing-preflight: job-level env reads secrets"
mutation_case vz-no-preflight-need "publish-vz: needs 'publish-build-tools' does not list server-signing-preflight"
mutation_case fc-implicit-success "publish-fc: if is ''"
mutation_case bt-skipped-on-push "publish-build-tools: if is"
mutation_case bt-failed-ok "publish-build-tools: if is"
mutation_case rel-no-preflight "release: if is"
mutation_case rel-import-early "release: the certificate import is step 1, not right before the server goreleaser step"
mutation_case rel-delete-after-host-agent "release: the keychain delete is step"
mutation_case rel-delete-on-success "release: the keychain delete's if is 'success()', not always()"
mutation_case rel-no-required "release server goreleaser (signing): env is"
mutation_case rel-identity-to-host-agent "only the import and the server goreleaser step may"

step "the wiring guard, server rules: a preflight that can skip or ignore its signing proof turns it red"
mutation_case pf-sign-if-false "server-signing-preflight: the sign step has an if (False); it must run whenever the import succeeded"
mutation_case pf-sign-continue "server-signing-preflight: the signing step has continue-on-error"
mutation_case pf-import-continue "server-signing-preflight: the certificate import has continue-on-error"
mutation_case pf-job-continue "server-signing-preflight: the job has continue-on-error"
mutation_case rel-server-continue "release: the signing step has continue-on-error"
mutation_case rel-import-continue "release: the certificate import has continue-on-error"
mutation_case rel-delete-continue "release: the keychain delete has continue-on-error"

step "the wiring guard, server rules: third-party build code before the keychain delete turns it red"
mutation_case rel-rust-before-import "release: the Rust toolchain step is step 3, not after the keychain delete"
mutation_case rel-rust-always "release: the Rust toolchain step's if is"
mutation_case rel-cargo-before-import "run cargo, rustup or brew before the keychain delete"

# dry_mutation_case <mutation> <expected substring of the guard's complaint>
dry_mutation_case() {
  python3 "${WIRING}" mutate-dry "${DRY_RUN_WF}" "${WIRING_DIR}/$1.json" "$1" || fail "mutate-dry $1 failed"
  run_wiring "${WF}" "${WIRING_DIR}/$1.json"
  [ "${WIRING_RC}" -eq 1 ] || fail "the wiring guard passed a dry run mutated by $1 (rc=${WIRING_RC}): ${WIRING_OUT}"
  echo "${WIRING_OUT}" | grep -qF "$2" || fail "mutation $1 was refused for the wrong reason (want '$2'): ${WIRING_OUT}"
  ok "mutation $1 → refused: $2"
}
dry_mutation_case dr-publishes "server-signing-dry-run.yml: goreleaser args are"
dry_mutation_case dr-write-token "server-signing-dry-run.yml: permissions are"
dry_mutation_case dr-any-branch "server-signing-dry-run.yml: the job's if is"
dry_mutation_case dr-other-version "server-signing-dry-run.yml: goreleaser version 'latest' is not the release job's"

# The release-plan server gate step, executed: its run script with each
# ${{ secrets.X != '' }} rendered, in a scratch checkout with or without the
# gate. A tag push is the only event it runs on, and a missing script fails
# closed (no fallback).
SSTEP_DIR="${SCRATCH}/server-step-exec"
mkdir -p "${SSTEP_DIR}/no-gate" "${SSTEP_DIR}/with-gate/scripts/release"
cp "${SERVER_GATE}" "${SSTEP_DIR}/with-gate/scripts/release/"
# run_server_step <checkout: no-gate|with-gate> <tag> [missing secret...]
run_server_step() {
  local co="${SSTEP_DIR}/$1" tag="$2"
  shift 2
  python3 "${WIRING}" extract-server-gate "${WF}" "${SSTEP_DIR}/step.sh" "$@" || fail "extract the server gate step failed"
  STEP_RC=0
  STEP_OUT="$(cd "${co}" && env -i PATH="${PATH}" HOME="${HOME}" GITHUB_EVENT_NAME=push TAG="${tag}" \
    bash -e "${SSTEP_DIR}/step.sh" 2>&1)" || STEP_RC=$?
}
# server_step_case <label> <want rc> <expected output substring> <run_server_step args...>
server_step_case() {
  local label="$1" want="$2" expect="$3"
  shift 3
  run_server_step "$@"
  [ "${STEP_RC}" -eq "${want}" ] || fail "server gate step (${label}) exited ${STEP_RC} (want ${want}): ${STEP_OUT}"
  case "${STEP_OUT}" in
    *"${expect}"*) ;;
    *) fail "server gate step (${label}) output lacks '${expect}': ${STEP_OUT}" ;;
  esac
  ok "server gate step ${label} → exit ${want}"
}
step "release-plan's server gate step, executed: each flag reaches the gate under its own name"
server_step_case "push v9.1.0, all four present" 0 "${SGATE_PASS}" with-gate v9.1.0
for n in ${SGATE_SECRETS}; do
  server_step_case "push v9.1.0, secrets.${n} missing" 1 "${SGATE_REFUSAL} ${n}" with-gate v9.1.0 "${n}"
done
# shellcheck disable=SC2086  # one argument per secret name, on purpose
server_step_case "push v9.1.0-rc.1, all four missing" 1 "${SGATE_REFUSAL} $(comma_list "${SGATE_SECRETS}")" \
  with-gate v9.1.0-rc.1 ${SGATE_SECRETS}
server_step_case "push v9.1.0, no gate script (fails closed, no fallback)" 127 "server-signing-gate.sh" no-gate v9.1.0

# The preflight's signing step, executed: its run script and its env (each
# ${{ secrets.X }} rendered from a test value), in a scratch checkout holding
# the real macos-server-app.sh and entitlements, with Phase 6's stub
# codesign/plutil and a stub `file` ahead on PATH. It must sign the scratch
# copy as the hook signs shed-server, and the hook's required checks must
# refuse anything but a timestamped Developer ID signature from the team.
PF_DIR="${SCRATCH}/preflight-exec"
PF_CO="${PF_DIR}/checkout"
PF_STUBS="${PF_DIR}/stubs"
mkdir -p "${PF_CO}/scripts/release" "${PF_CO}/internal/vz" "${PF_STUBS}"
cp "${MSA}" "${PF_CO}/scripts/release/"
cp "${MSA_ENTITLEMENTS}" "${PF_CO}/internal/vz/"
cat > "${PF_STUBS}/file" <<'STUB'
#!/bin/sh
echo "file $*" >> "${STUB_LOG:-/dev/null}"
echo "${STUB_FILE:-Mach-O universal binary with 2 architectures: [x86_64:Mach-O 64-bit executable x86_64] [arm64e]}"
STUB
chmod 0755 "${PF_STUBS}/file"
PF_SIGN_STEP="Sign and verify a scratch binary (Developer ID, timestamped)"
SECRET_SHED_DESKTOP_DEVELOPER_ID_IDENTITY="${MSA_IDENTITY}" SECRET_APPLE_TEAM_ID="${MSA_TEAM}" \
  python3 "${WIRING}" extract-step "${WF}" server-signing-preflight "${PF_SIGN_STEP}" "${PF_DIR}/step.sh" "${PF_DIR}/step.env" \
  || fail "extract the preflight's signing step failed"
PF_ENV=()
while IFS= read -r line; do PF_ENV+=("${line}"); done < "${PF_DIR}/step.env"
PF_SCRATCH_BIN="${PF_DIR}/runner-temp/server-signing-preflight/shed-server"
# run_pf [STUB_*=value...] → PF_OUT (stdout+stderr), PF_RC; the stubs log to MSA_LOG.
PF_OUT=""; PF_RC=0
run_pf() {
  PF_RC=0
  : > "${MSA_LOG}"
  rm -rf "${PF_DIR}/runner-temp"
  mkdir -p "${PF_DIR}/runner-temp"
  PF_OUT="$(cd "${PF_CO}" && env -i PATH="${PF_STUBS}:${MSA_STUBS}:${PATH}" HOME="${HOME}" \
    RUNNER_TEMP="${PF_DIR}/runner-temp" STUB_LOG="${MSA_LOG}" "${PF_ENV[@]}" "$@" \
    bash -e "${PF_DIR}/step.sh" 2>&1)" || PF_RC=$?
}

step "server-signing-preflight's signing step, executed: signs like the hook and passes only a timestamped Developer ID from the team"
[ "$(printf '%s\n' "${PF_ENV[@]}" | LC_ALL=C sort)" = "$(printf '%s\n' SHED_SERVER_SIGNING=required "SHED_SERVER_SIGN_IDENTITY=${MSA_IDENTITY}" "SHED_SERVER_TEAM_ID=${MSA_TEAM}" | LC_ALL=C sort)" ] \
  || fail "the signing step's env does not map the identity and team secrets under required signing: ${PF_ENV[*]}"
run_pf
[ "${PF_RC}" -eq 0 ] || fail "the preflight signing step with a matching Developer ID exited ${PF_RC} (want 0): ${PF_OUT}"
grep -qxF -- "codesign --force --sign ${MSA_IDENTITY} --identifier ai.stridelabs.shed-server --entitlements internal/vz/entitlements.plist --timestamp ${PF_SCRATCH_BIN}" "${MSA_LOG}" \
  || fail "the scratch binary was not signed with the identity, the identifier, the entitlements and --timestamp: $(cat "${MSA_LOG}")"
printf '%s\n' "${PF_OUT}" | grep -qF "verified ${PF_SCRATCH_BIN} (ai.stridelabs.shed-server, Developer ID, team ${MSA_TEAM}, timestamped)" \
  || fail "the preflight did not run the hook's required --verify: ${PF_OUT}"
ok "matching Developer ID → exit 0; signed like the hook; verified under required signing"
# pf_refused <label> <expected message> <stub settings...>
pf_refused() {
  local label="$1" want="$2"
  shift 2
  run_pf "$@"
  [ "${PF_RC}" -eq 1 ] || fail "the preflight signing step with ${label} exited ${PF_RC} (want 1): ${PF_OUT}"
  printf '%s\n' "${PF_OUT}" | grep -qF "${want}" || fail "the preflight with ${label}: no '${want}' in: ${PF_OUT}"
  ok "preflight with ${label} → exit 1 (${want})"
}
pf_refused "an ad hoc signature (no identity in the keychain)" "no signing Authority (an ad hoc signature?)" STUB_SIGNATURE=adhoc
pf_refused "another team's certificate" "TeamIdentifier is 'WRONGTEAM9', which is not SHED_SERVER_TEAM_ID" STUB_TEAM=WRONGTEAM9
pf_refused "no secure timestamp (the timestamp server failed)" "no secure Timestamp" STUB_TIMESTAMP=no
pf_refused "an extra entitlement" "want exactly {\"com.apple.security.virtualization\":true}" STUB_ENTITLEMENTS=extra
pf_refused "a scratch copy that is not a Mach-O" "is not a Mach-O on this runner" "STUB_FILE=POSIX shell script, ASCII text executable"
msa_signed_nothing "the preflight with a non-Mach-O scratch copy"

echo
echo "release-scripts-test: all ${PASS} checks passed"
