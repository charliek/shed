#!/usr/bin/env bash
# Real (non-test) launch/reopen window behavior — guards issue #4.
#
# The hermetic E2E harness runs with SHED_DESKTOP_TEST_MODE=1, which gates the
# launch auto-open OFF so the suite keeps its hidden-start / accessory policy.
# So the genuine "a user launch opens the dashboard, and reopening the running
# app reaches it" behavior is invisible to it. This drives the REAL launch path
# and asserts both over IPC:
#
#   1. Foreground launch (`open`, like Finder/Spotlight) -> dashboard visible,
#      app raised to a regular (Dock) app.
#   2. Close the window (-> menu-bar-only accessory), then reopen the running
#      instance (`open`, no -n) -> applicationShouldHandleReopen brings the
#      dashboard back. This is the escape hatch when the status icon is hidden
#      under the notch and unclickable.
#
# The quiet-on-login path can't be faithfully reproduced from a shell (every
# shell launch carries a kAEOpenApplication event; only a real launchd login
# launch is flagged keyAELaunchedAsLogInItem) — it is covered by the
# LaunchClassifier unit test and must be eyeballed once on a real reboot.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
APP="$REPO_ROOT/build/ShedDesktop.app"
SHEDCTL="$APP/Contents/Resources/bin/shedctl"

# Bounded IPC calls: shedctl's read() has no timeout, so a wedged app could
# otherwise hang the step. perl's alarm survives exec and terminates on fire.
ident()  { perl -e 'alarm 5; exec @ARGV' "$SHEDCTL" identify; }
wstate() { perl -e 'alarm 5; exec @ARGV' "$SHEDCTL" ui window-state; }
hide()   { perl -e 'alarm 5; exec @ARGV' "$SHEDCTL" ui hide-window; }
is_visible() { wstate 2>/dev/null | grep -Eq '"visible"[[:space:]]*:[[:space:]]*true'; }
is_regular() { wstate 2>/dev/null | grep -Eq '"activation_policy"[[:space:]]*:[[:space:]]*"regular"'; }

[ -d "$APP" ] || "$REPO_ROOT/scripts/bundle.sh" debug

TMP="$(mktemp -d)"
trap 'pkill -x ShedDesktop 2>/dev/null || true; rm -rf "$TMP"' EXIT

# Throwaway config/state/defaults so the smoke touches nothing real. NO
# SHED_DESKTOP_TEST_MODE — exercising the real launch path is the whole point.
ENV_ARGS=(
  --env "SHED_DESKTOP_SHED_CONFIG=$TMP/none.yaml"
  --env "SHED_DESKTOP_STATE_DIR=$TMP/state"
  --env "SHED_DESKTOP_HOST_AGENT_SOCKET=$TMP/no-agent.sock"
  --env "SHED_DESKTOP_DEFAULTS_SUITE=ai.stridelabs.ShedDesktop.smoke-window"
)

wait_identify() {
  for _ in $(seq 1 20); do
    if ident >/dev/null 2>&1; then return 0; fi
    sleep 0.5
  done
  echo "FAIL: app never answered identify — crashed on launch?" >&2
  exit 1
}

wait_until() {  # wait_until <pred-fn> <label>
  for _ in $(seq 1 20); do
    if "$1"; then return 0; fi
    sleep 0.25
  done
  echo "FAIL: $2" >&2
  exit 1
}

not_visible() { ! is_visible; }

pkill -x ShedDesktop 2>/dev/null || true
for _ in $(seq 1 20); do pgrep -x ShedDesktop >/dev/null || break; sleep 0.2; done

# 1. Foreground launch opens the dashboard + raises to a regular app ----------
open -n "${ENV_ARGS[@]}" "$APP"
wait_identify
wait_until is_visible "foreground launch did not open the dashboard"
is_regular || { echo "FAIL: foreground launch did not become a regular (Dock) app" >&2; exit 1; }
echo "OK: foreground launch -> dashboard visible + regular"

# 2. Close -> reopen reaches the dashboard (the notch escape hatch) -----------
hide >/dev/null
wait_until not_visible "window did not close on hide"
is_regular && { echo "FAIL: closing the window did not revert to accessory" >&2; exit 1; }
echo "OK: window closed -> menu-bar-only accessory"

open "$APP"   # no -n: reopen the existing instance -> applicationShouldHandleReopen
wait_until is_visible "reopen did not bring up the dashboard (escape hatch broken)"
is_regular || { echo "FAIL: reopen did not become a regular (Dock) app" >&2; exit 1; }
echo "OK: reopen -> dashboard visible + regular"

echo "PASS: launch/reopen window behavior (issue #4)"
