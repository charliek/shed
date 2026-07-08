#!/usr/bin/env bash
# Non-test-mode launch smoke — guards the REAL startup path (issue #2).
#
# The hermetic E2E harness always runs with SHED_DESKTOP_TEST_MODE=1, which
# swaps in FakeNotificationPresenter and never calls requestAuthorization(), so
# a crash on the real launch path is invisible to it. v0.0.2 shipped exactly
# such a crash: a @MainActor completion closure handed to UserNotifications ran
# on its background XPC queue and SIGTRAP'd on first launch on macOS 26.
#
# This launches the actual app NON-test (real SystemNotificationPresenter +
# requestAuthorization) and asserts it survives long enough to bind its IPC
# socket and answer `identify`. On a fresh machine / CI runner (notification
# auth still "undetermined") this drives the exact path that crashed in #2.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
APP="$REPO_ROOT/build/ShedDesktop.app"
SHEDCTL="$APP/Contents/Resources/bin/shedctl"

# Bounded identify: shedctl's read() has no timeout, so a wedged app (socket
# bound but unresponsive) could otherwise hang the step. perl's alarm timer
# survives exec and defaults to terminating the process, so this self-kills
# after 5s. Returns shedctl's exit status (non-zero on timeout/failure).
ident() { perl -e 'alarm 5; exec @ARGV' "$SHEDCTL" identify; }

[ -d "$APP" ] || "$REPO_ROOT/scripts/bundle.sh" debug

reports_dir="$HOME/Library/Logs/DiagnosticReports"
crash_count() { ls "$reports_dir" 2>/dev/null | grep -c -i ShedDesktop || true; }

# Clean slate + throwaway config/state/defaults so the smoke touches nothing
# real (missing config => empty host list; missing agent socket => degrades).
pkill -x ShedDesktop 2>/dev/null || true
TMP="$(mktemp -d)"
trap 'pkill -x ShedDesktop 2>/dev/null || true; rm -rf "$TMP"' EXIT
before="$(crash_count)"

# NB: NO SHED_DESKTOP_TEST_MODE — this is the whole point. `open` (LaunchServices)
# is required: a bare-binary launch never wires up the UserNotifications XPC
# connection, so it would not reproduce the crash.
open -n \
  --env "SHED_DESKTOP_SHED_CONFIG=$TMP/none.yaml" \
  --env "SHED_DESKTOP_STATE_DIR=$TMP/state" \
  --env "SHED_DESKTOP_HOST_AGENT_SOCKET=$TMP/no-agent.sock" \
  --env "SHED_DESKTOP_DEFAULTS_SUITE=ai.stridelabs.ShedDesktop.smoke" \
  "$APP"

# Reach IPC within the budget (a #2-style crash lands in well under a second).
ok=""
for _ in $(seq 1 20); do
  if ident >/dev/null 2>&1; then ok=1; break; fi
  sleep 0.5
done
[ -n "$ok" ] || { echo "FAIL: app never answered identify — crashed on launch?" >&2; exit 1; }

# Re-check after a grace period: the requestAuthorization callback fires shortly
# after launch, so a regression would kill the app a beat *after* it came up.
sleep 2
if ! ident >/dev/null 2>&1; then
  echo "FAIL: app died shortly after launch — real notification path regressed?" >&2
  exit 1
fi
if [ "$(crash_count)" -gt "$before" ]; then
  echo "FAIL: a ShedDesktop crash report appeared during the launch" >&2
  exit 1
fi

echo "OK: non-test launch survived — $(ident | tr -d '\n ')"
