#!/usr/bin/env bash
# Self-test for the release-model scripts:
#
#   scripts/release/update-version.sh   (component-selecting version bumper)
#   scripts/release/release-plan.sh     (tag → per-component ship selector)
#
# Runs every synthetic combination the release model depends on — per-component
# (server / host-agent / machine-rc / desktop) and combined plans / neither→
# exit 1 / lockstep-broken→exit 1 / malformed-manifest→exit 1 / prerelease×
# goreleaser-component→exit 1 / desktop-only-prerelease→pass / the `**Ships:**`
# CHANGELOG cross-check (legacy alias, unknown token, duplicate, mismatch,
# missing entry, Unreleased-skipping) — plus the update-version.sh contract
# asserts (unknown component errors; `--components go` leaves desktop manifests
# untouched; `--components desktop` bumps all four desktop surfaces + both
# locks, covering the real 0.0.x → 0.8.0 jump; idempotent re-run).
#
# The whole thing runs in a TEMP COPY of the relevant files (scripts +
# manifests + both cargo workspaces, minus target/) so it never dirties the
# repo. Needs: bash, jq, cargo (the lock regens run `cargo update --offline`).
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

# Prime the cargo registry index for both scratch workspaces. update-version.sh
# runs `cargo update --offline` (the script's no-network contract), and offline
# resolution needs the crates.io index entries for the locked deps in the local
# cargo cache — a fresh CI runner has none. A `--dry-run` update (no --offline)
# fetches just the sparse-index metadata (~seconds, no crate downloads) and
# leaves both locks untouched. On a warm dev machine this is a near-no-op.
(cd "${SCRATCH}/crates" && cargo update --workspace --dry-run -q)
(cd "${SCRATCH}/desktop/tauri/src-tauri" && cargo update --workspace --dry-run -q)

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

# The exact 6-line stdout release-plan.sh emits, in emission order. Building the
# expectation from named booleans keeps the assertions readable as the ship set
# changes case-to-case.
expect_plan() {
  # $1 server, $2 host-agent, $3 machine-rc, $4 desktop, $5 goreleaser, $6 go
  printf 'ship_server=%s\nship_host_agent=%s\nship_machine_rc=%s\nship_desktop=%s\nship_goreleaser=%s\nship_go=%s' \
    "$1" "$2" "$3" "$4" "$5" "$6"
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
echo "${out}" | grep -q "go, desktop" || fail "error message doesn't list valid components: ${out}"
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
[ "${PLAN_OUT}" = "$(expect_plan true false false false true true)" ] || fail "server-only plan stdout: ${PLAN_OUT}"
ok "ship_server=true, others false, ship_goreleaser=true, ship_go=true"

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
[ "${PLAN_OUT}" = "$(expect_plan false false false true false false)" ] || fail "desktop-only plan stdout: ${PLAN_OUT}"
ok "ship_desktop=true only, ship_goreleaser=false, ship_go=false"

# ---------------------------------------------------------------------------
step "update-version.sh --components go,desktop; release-plan sees a combined tag"
"${UV}" 0.8.0 --components go,desktop >/dev/null
[ "$(jq -r '.version' "${SCRATCH}/.claude-plugin/plugin.json")" = "0.8.0" ] || fail "plugin.json didn't bump to 0.8.0"
write_changelog 0.8.0 "server, desktop"
run_plan v0.8.0
[ "${PLAN_RC}" -eq 0 ] || fail "combined plan exited ${PLAN_RC}"
[ "${PLAN_OUT}" = "$(expect_plan true false false true true true)" ] || fail "combined plan stdout: ${PLAN_OUT}"
ok "ship_server=true ship_desktop=true ship_goreleaser=true ship_go=true"

# ---------------------------------------------------------------------------
step "release-plan.sh: GITHUB_REF_NAME fallback (no positional arg)"
run_plan_env_rc=0
plan_env_out="$(GITHUB_REF_NAME=v0.8.0 "${RP}" 2>/dev/null)" || run_plan_env_rc=$?
[ "${run_plan_env_rc}" -eq 0 ] || fail "GITHUB_REF_NAME fallback exited ${run_plan_env_rc}"
[ "${plan_env_out}" = "$(expect_plan true false false true true true)" ] || fail "GITHUB_REF_NAME fallback stdout: ${plan_env_out}"
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
# Entering state: plugin.json=0.8.0, host-agent=0.7.10, machine-rc=0.7.10,
# desktop surfaces=0.8.0 (lockstep intact).
# ===========================================================================

HOST_AGENT_VER="${SCRATCH}/crates/shed-host-agent/VERSION"
MACHINE_RC_VER="${SCRATCH}/cmd/shed-machine-rc/VERSION"

# ---------------------------------------------------------------------------
step "release-plan.sh: host-agent-only tag (only crates/shed-host-agent/VERSION matches)"
printf '1.2.3\n' > "${HOST_AGENT_VER}"
write_changelog 1.2.3 "host-agent"
run_plan v1.2.3
[ "${PLAN_RC}" -eq 0 ] || fail "host-agent-only plan exited ${PLAN_RC}"
[ "${PLAN_OUT}" = "$(expect_plan false true false false true false)" ] || fail "host-agent-only plan stdout: ${PLAN_OUT}"
ok "ship_host_agent=true only, ship_goreleaser=true, ship_go=false"

# ---------------------------------------------------------------------------
step "release-plan.sh: machine-rc-only tag (only cmd/shed-machine-rc/VERSION matches)"
printf '3.4.5\n' > "${MACHINE_RC_VER}"
write_changelog 3.4.5 "machine-rc"
run_plan v3.4.5
[ "${PLAN_RC}" -eq 0 ] || fail "machine-rc-only plan exited ${PLAN_RC}"
[ "${PLAN_OUT}" = "$(expect_plan false false true false true false)" ] || fail "machine-rc-only plan stdout: ${PLAN_OUT}"
ok "ship_machine_rc=true only, ship_goreleaser=true, ship_go=false"

# ---------------------------------------------------------------------------
step "release-plan.sh: combined server + host-agent (both manifests at 4.0.0)"
"${UV}" 4.0.0 --components go >/dev/null
printf '4.0.0\n' > "${HOST_AGENT_VER}"
write_changelog 4.0.0 "server, host-agent"
run_plan v4.0.0
[ "${PLAN_RC}" -eq 0 ] || fail "server+host-agent plan exited ${PLAN_RC}"
[ "${PLAN_OUT}" = "$(expect_plan true true false false true true)" ] || fail "server+host-agent plan stdout: ${PLAN_OUT}"
ok "ship_server=true ship_host_agent=true ship_goreleaser=true ship_go=true"

# ---------------------------------------------------------------------------
step "release-plan.sh: legacy 'server/CLI' alias accepted (server + desktop tag)"
"${UV}" 0.8.0 --components go >/dev/null
write_changelog 0.8.0 "server/CLI, desktop"
run_plan v0.8.0
[ "${PLAN_RC}" -eq 0 ] || fail "legacy-alias plan exited ${PLAN_RC}"
[ "${PLAN_OUT}" = "$(expect_plan true false false true true true)" ] || fail "legacy-alias plan stdout: ${PLAN_OUT}"
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
[ "${PLAN_OUT}" = "$(expect_plan false false false true false false)" ] || fail "desktop-only prerelease stdout: ${PLAN_OUT}"
ok "ship_desktop=true only; prerelease skips the Ships cross-check"

# ---------------------------------------------------------------------------
step "release-plan.sh: empty VERSION manifest exits 1 naming the file"
: > "${HOST_AGENT_VER}"
run_plan v0.8.0
[ "${PLAN_RC}" -eq 1 ] || fail "empty-manifest plan exited ${PLAN_RC} (want 1)"
out="$("${RP}" v0.8.0 2>&1 || true)"
echo "${out}" | grep -q "crates/shed-host-agent/VERSION is empty" || fail "empty-manifest error doesn't name the file: ${out}"
printf '0.7.10\n' > "${HOST_AGENT_VER}"   # restore (defensive; last case)
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

echo
echo "release-scripts-test: all ${PASS} checks passed"
