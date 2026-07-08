#!/usr/bin/env bash
# live-verify.sh — exercise the shed-desktop approval gate against the REAL
# shed-host-agent (Go) over the Unix-domain socket, end to end.
#
# The hermetic pytest harness covers everything against a Python fake agent;
# this is the one path that puts the real Go agent and the real .app in the
# loop, so it validates the cross-implementation wire protocol (hello /
# hello_ack / approval_request / approval_response / event, incl. the #21
# `server` field) that no fake can prove.
#
# Modes:
#   --handshake  (default) Build a real host-agent, start it with the desktop
#                socket enabled and NO plugin-bus servers, launch the real app
#                pointed at that socket, and assert the app completes the
#                hello/hello_ack handshake (ui.state.host_agent_connected). This
#                is non-disruptive: a private socket, a throwaway state dir, and
#                it does NOT touch the brew service or any shed server.
#
#   --full       Also drive a real SSH-sign approval. Stops the brew host-agent
#                (restored on exit), watches every server in ~/.shed/config.yaml,
#                and waits for you to trigger a sign inside a shed (e.g.
#                `git -C <repo> pull` over SSH). Approves it via shedctl, asserts
#                the response, then checks fail-closed: quit the app and confirm
#                the next sign is denied. Restarts the brew service on exit.
#
# Nothing here runs in CI (it needs a real VM); it's a one-command local check.
set -euo pipefail

MODE="handshake"
[ "${1:-}" = "--full" ] && MODE="full"
[ "${1:-}" = "--handshake" ] && MODE="handshake"

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# The shed-host-agent (Go) now lives in this monorepo at cmd/shed-host-agent
# (one level above the desktop package root).
MONO_ROOT="$(cd "${REPO_ROOT}/.." && pwd)"
APP="$REPO_ROOT/build/ShedDesktop.app"
CTL="$APP/Contents/Resources/bin/shedctl"
APP_SOCK="$HOME/Library/Caches/ShedDesktop/shed-desktop.sock"
GO="${GO:-$(command -v go || echo "$HOME/.local/share/mise/shims/go")}"

TMP="$(mktemp -d -t shed-live-verify)"
CFG="$TMP/extensions.yaml"
AGENT_SOCK="$TMP/host-agent.sock"
AGENT_BIN="$TMP/shed-host-agent"
STATE_DIR="$TMP/state"
DEFAULTS_SUITE="ai.stridelabs.ShedDesktop.live"
AGENT_PID=""
BREW_STOPPED=""

say() { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
ok()  { printf '\033[1;32m  ✓\033[0m %s\n' "$*"; }
die() { printf '\033[1;31merror:\033[0m %s\n' "$*" >&2; exit 1; }

cleanup() {
  say "Cleanup"
  osascript -e 'tell application "ShedDesktop" to quit' >/dev/null 2>&1 || true
  pkill -x ShedDesktop >/dev/null 2>&1 || true
  rm -f "$APP_SOCK" "${APP_SOCK%.sock}.lock" 2>/dev/null || true
  defaults delete "$DEFAULTS_SUITE" >/dev/null 2>&1 || true
  [ -n "$AGENT_PID" ] && kill "$AGENT_PID" >/dev/null 2>&1 || true
  if [ -n "$BREW_STOPPED" ]; then
    say "Restarting brew shed-host-agent"
    brew services start shed-host-agent >/dev/null 2>&1 || true
  fi
  rm -rf "$TMP" 2>/dev/null || true
}
trap cleanup EXIT

[ -d "$APP" ] || die "no app bundle at $APP (run: make bundle)"
[ -d "$MONO_ROOT/cmd/shed-host-agent" ] || die "cmd/shed-host-agent not found under $MONO_ROOT"
[ -x "$GO" ] || die "go toolchain not found (set GO=/path/to/go)"

say "Building shed-host-agent from $MONO_ROOT ($(cd "$MONO_ROOT" && git rev-parse --short HEAD 2>/dev/null || echo '?'))"
( cd "$MONO_ROOT" && "$GO" build -o "$AGENT_BIN" ./cmd/shed-host-agent ) || die "host-agent build failed"
ok "built $AGENT_BIN"

if [ "$MODE" = "full" ]; then
  SERVERS="all"
  SHED_CFG="$HOME/.shed/config.yaml"        # show real sheds; needed for real signs
  if brew services list 2>/dev/null | grep -qE '^shed-host-agent[[:space:]]+started'; then
    say "Stopping brew shed-host-agent (restored on exit)"
    brew services stop shed-host-agent >/dev/null 2>&1 && BREW_STOPPED=1
  fi
else
  SERVERS="[]"
  SHED_CFG="$TMP/shed-config.yaml"          # handshake: reach no real shed server
  printf '# handshake mode: no shed servers (UDS handshake only)\n' > "$SHED_CFG"
fi

cat > "$CFG" <<YAML
discovery:
  servers: ${SERVERS}
  watch: fsnotify
ssh:
  approval:
    policy: shed-desktop
aws:
  source_profile: default
  approval:
    policy: shed-desktop
docker:
  registries: []
  approval:
    policy: shed-desktop
desktop:
  enabled: true
  socket_path: ${AGENT_SOCK}
  timeout_ms: 25000
logging:
  enabled: false
YAML

say "Starting real host-agent (socket: $AGENT_SOCK)"
"$AGENT_BIN" -config "$CFG" >"$TMP/agent.log" 2>&1 &
AGENT_PID=$!
for _ in $(seq 1 50); do [ -S "$AGENT_SOCK" ] && break; sleep 0.1; done
[ -S "$AGENT_SOCK" ] || { cat "$TMP/agent.log"; die "host-agent socket never appeared"; }
ok "host-agent up (pid $AGENT_PID)"

say "Launching real ShedDesktop.app pointed at the host-agent socket"
osascript -e 'tell application "ShedDesktop" to quit' >/dev/null 2>&1 || true
pkill -x ShedDesktop >/dev/null 2>&1 || true; sleep 0.5
rm -f "$APP_SOCK" "${APP_SOCK%.sock}.lock" 2>/dev/null || true
mkdir -p "$STATE_DIR"
open --env "SHED_DESKTOP_HOST_AGENT_SOCKET=$AGENT_SOCK" \
     --env "SHED_DESKTOP_STATE_DIR=$STATE_DIR" \
     --env "SHED_DESKTOP_DEFAULTS_SUITE=$DEFAULTS_SUITE" \
     --env "SHED_DESKTOP_SHED_CONFIG=$SHED_CFG" \
     "$APP"

for _ in $(seq 1 60); do "$CTL" identify >/dev/null 2>&1 && break; sleep 0.25; done
"$CTL" identify >/dev/null 2>&1 || die "app never answered its IPC socket"
ok "app up"

say "Waiting for the hello/hello_ack handshake (real Go agent ↔ real Swift app)"
connected=""
for _ in $(seq 1 40); do
  if "$CTL" call ui.state 2>/dev/null | grep -q '"host_agent_connected"[[:space:]]*:[[:space:]]*true'; then
    connected=1; break
  fi
  sleep 0.25
done
[ -n "$connected" ] || { cat "$TMP/agent.log"; die "app did not connect to the host-agent"; }
ok "HANDSHAKE VERIFIED — app connected to the real host-agent over the UDS"

if [ "$MODE" != "full" ]; then
  say "Handshake mode complete. For the full SSH-sign drive, re-run with: $0 --full"
  exit 0
fi

# ---- full mode: drive a real SSH-sign approval ----
cat <<MSG

$(printf '\033[1;33m')ACTION NEEDED$(printf '\033[0m'): trigger an SSH signature inside a shed now, e.g.
  ssh -p <sshPort> <shed>@<host>   # then run a git op that signs, e.g. \`git pull\`
Waiting up to 120s for an approval request to arrive…
MSG

req_id=""
for _ in $(seq 1 480); do
  req_id="$("$CTL" call approvals.list 2>/dev/null | grep -oE '"id"[[:space:]]*:[[:space:]]*"[^"]+"' | head -1 | grep -oE '"[^"]+"$' | tr -d '"' || true)"
  [ -n "$req_id" ] && break
  sleep 0.25
done
[ -n "$req_id" ] || die "no approval request arrived (did the sign trigger reach the host-agent?)"
ok "approval request received: $req_id"
"$CTL" call approvals.list

say "Approving $req_id"
"$CTL" call approval.decide id="$req_id" decision=approve >/dev/null
sleep 0.5
ok "approved (the git op should have succeeded)"

say "Fail-closed check: quitting the app — the next sign must be DENIED by the agent"
osascript -e 'tell application "ShedDesktop" to quit' >/dev/null 2>&1 || true
pkill -x ShedDesktop >/dev/null 2>&1 || true
cat <<MSG
Trigger another SSH signature now. With no app connected, the host-agent must
fail closed (deny) — your git op should fail. Press Enter when confirmed.
MSG
read -r _ || true
ok "fail-closed confirmed by operator"
say "LIVE VERIFY COMPLETE"
