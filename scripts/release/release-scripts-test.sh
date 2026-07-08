#!/usr/bin/env bash
# Self-test for the release-model scripts:
#
#   scripts/release/update-version.sh   (component-selecting version bumper)
#   scripts/release/release-plan.sh     (tag → ship_go/ship_desktop selector)
#
# Runs every synthetic combination the release model depends on — go-only /
# desktop-only / both / neither→exit 1 / lockstep-broken→exit 1 — plus the
# update-version.sh contract asserts (unknown component errors; `--components
# go` leaves desktop manifests untouched; `--components desktop` bumps all four
# desktop surfaces + both locks, covering the real 0.0.x → 0.8.0 jump;
# idempotent re-run).
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
step "release-plan.sh: go-only tag (desktop manifest doesn't match)"
run_plan v9.9.9
[ "${PLAN_RC}" -eq 0 ] || fail "go-only plan exited ${PLAN_RC}"
[ "${PLAN_OUT}" = "$(printf 'ship_go=true\nship_desktop=false')" ] || fail "go-only plan stdout: ${PLAN_OUT}"
ok "ship_go=true ship_desktop=false"

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
step "release-plan.sh: desktop-only tag (go manifest doesn't match)"
run_plan v0.8.0
[ "${PLAN_RC}" -eq 0 ] || fail "desktop-only plan exited ${PLAN_RC}"
[ "${PLAN_OUT}" = "$(printf 'ship_go=false\nship_desktop=true')" ] || fail "desktop-only plan stdout: ${PLAN_OUT}"
ok "ship_go=false ship_desktop=true"

# ---------------------------------------------------------------------------
step "update-version.sh --components go,desktop; release-plan sees a combined tag"
"${UV}" 0.8.0 --components go,desktop >/dev/null
[ "$(jq -r '.version' "${SCRATCH}/.claude-plugin/plugin.json")" = "0.8.0" ] || fail "plugin.json didn't bump to 0.8.0"
run_plan v0.8.0
[ "${PLAN_RC}" -eq 0 ] || fail "combined plan exited ${PLAN_RC}"
[ "${PLAN_OUT}" = "$(printf 'ship_go=true\nship_desktop=true')" ] || fail "combined plan stdout: ${PLAN_OUT}"
ok "ship_go=true ship_desktop=true"

# ---------------------------------------------------------------------------
step "release-plan.sh: GITHUB_REF_NAME fallback (no positional arg)"
run_plan_env_rc=0
plan_env_out="$(GITHUB_REF_NAME=v0.8.0 "${RP}" 2>/dev/null)" || run_plan_env_rc=$?
[ "${run_plan_env_rc}" -eq 0 ] || fail "GITHUB_REF_NAME fallback exited ${run_plan_env_rc}"
[ "${plan_env_out}" = "$(printf 'ship_go=true\nship_desktop=true')" ] || fail "GITHUB_REF_NAME fallback stdout: ${plan_env_out}"
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

echo
echo "release-scripts-test: all ${PASS} checks passed"
