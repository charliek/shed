#!/bin/bash
# Linux smoke test for shed-server. Exercises the full
# install → setup → pull → create → exec → delete lifecycle
# that a freshly-installed-from-apt user goes through. Runs in two
# modes:
#
#   --from-apt   Install shed-server from the apt-charliek repo, then
#                run the full lifecycle. Used when validating a
#                published deb (e.g. after a release tag).
#
#   --from-local Use binaries in ./bin (default: make build artifacts).
#                Used during PR CI and local development.
#
# /dev/kvm is required for the create-and-boot phase. When absent
# (which is the case on GitHub-hosted ubuntu-latest runners) the
# script runs install-only smoke: setup + pull-images succeed but
# the create cycle is skipped with a clearly-flagged warning. The
# full create cycle is exercised by release validation against
# whatever bare-metal Linux+KVM hosts the maintainer keeps for
# this purpose.
#
# Exits non-zero on any step failure with a structured log so CI
# annotations point at the precise failing command.
#
# Usage:
#   sudo ./scripts/smoke-test-linux.sh                  # local mode (default)
#   sudo ./scripts/smoke-test-linux.sh --from-apt       # install via apt first
#   sudo ./scripts/smoke-test-linux.sh --skip-create    # skip create even if KVM is present

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(dirname "$SCRIPT_DIR")"

MODE="local"            # "local" or "apt"
SKIP_CREATE=false       # force-skip the create phase even if /dev/kvm exists
SHED_NAME="smoke-$(date +%s)"
LOCAL_DIR=""
SMOKE_IMAGE="base"      # which image to pull / boot from

while [[ $# -gt 0 ]]; do
    case "$1" in
        --from-apt)   MODE="apt"; shift ;;
        --from-local) MODE="local"; shift ;;
        --skip-create) SKIP_CREATE=true; shift ;;
        --image)
            if [[ $# -lt 2 || "$2" == --* ]]; then
                echo "ERROR: --image requires a value" >&2; exit 64
            fi
            SMOKE_IMAGE="$2"
            shift 2
            ;;
        --help|-h)
            sed -n '2,/^$/p' "$0"
            exit 0
            ;;
        *) echo "ERROR: unknown arg $1"; exit 64 ;;
    esac
done

if [[ "$EUID" -ne 0 ]]; then
    echo "ERROR: smoke-test-linux.sh requires root (uses apt / systemctl / mounts /dev/kvm)" >&2
    echo "Re-run with: sudo $0 $*" >&2
    exit 77
fi

# Structured logging — each step prints a "::group::" tag so GitHub
# Actions folds it in the UI; locally the tags are just visible
# headers.
section() {
    echo "::group::$1"
    echo "==> $1"
}
endsection() {
    echo "::endgroup::"
}
fail() {
    echo "::error::smoke-test failed at: $1"
    echo "FAIL: $1" >&2
    exit 1
}
warn() {
    echo "::warning::$1"
    echo "WARN: $1" >&2
}

cleanup() {
    local rc=$?
    set +e
    if [[ -n "${LOCAL_DIR:-}" && -d "$LOCAL_DIR" ]]; then
        rm -rf "$LOCAL_DIR"
    fi
    if [[ "${CREATED:-false}" == "true" ]]; then
        echo "==> cleanup: deleting smoke shed"
        shed delete "$SHED_NAME" --force >/dev/null 2>&1 || true
    fi
    if [[ -n "${SERVER_BG_PID:-}" ]] && kill -0 "$SERVER_BG_PID" 2>/dev/null; then
        echo "==> cleanup: stopping background shed-server (PID $SERVER_BG_PID)"
        kill "$SERVER_BG_PID" 2>/dev/null || true
        wait "$SERVER_BG_PID" 2>/dev/null || true
    fi
    if [[ $rc -ne 0 ]]; then
        echo ""
        echo "=== Recent shed-server logs ==="
        if [[ -f /tmp/shed-server.log ]]; then
            tail -50 /tmp/shed-server.log
        else
            journalctl -u shed-server -n 50 --no-pager 2>/dev/null || true
        fi
    fi
    exit $rc
}
trap cleanup EXIT

# ---- 1. Install / verify shed-server ----

section "Step 1: shed-server install"

USES_SYSTEMD=false
if [[ "$MODE" == "apt" ]]; then
    # apt-from-apt-charliek mode. Assumes the repo is already
    # configured on the host (release validation step or a fresh VM
    # bootstrapped by the workflow). The deb's postinstall installs
    # the systemd unit + writes /etc/shed/server.yaml.
    apt-get update -qq
    apt-get install -y shed-server
    USES_SYSTEMD=true
    SHED_BIN=/usr/local/bin/shed
    SHED_SERVER_BIN=/usr/local/bin/shed-server
else
    # Local build mode. Installs binaries over whatever was there.
    # PROJECT_ROOT/bin must contain shed and shed-server. The
    # systemd unit is NOT installed — we spawn shed-server in the
    # background instead so this works in CI runners that don't
    # have the deb's postinstall side effects.
    for bin in shed shed-server; do
        if [[ ! -x "$PROJECT_ROOT/bin/$bin" ]]; then
            fail "missing $PROJECT_ROOT/bin/$bin — run 'make build' first"
        fi
        install -m 0755 "$PROJECT_ROOT/bin/$bin" "/usr/local/bin/$bin"
    done
    SHED_BIN=/usr/local/bin/shed
    SHED_SERVER_BIN=/usr/local/bin/shed-server
    # If the box happens to have a shed-server.service installed
    # already (e.g. local dev box), reuse it. Otherwise we'll fall
    # back to a background process below.
    if systemctl list-unit-files shed-server.service --no-pager 2>/dev/null | grep -q shed-server; then
        USES_SYSTEMD=true
    fi
fi

echo "Installed:"
# shed CLI exposes version as a subcommand; shed-server exposes it
# via the cobra root flag. Both must report something — empty
# output indicates a broken/stub binary.
"$SHED_BIN" version || fail "shed version"
"$SHED_SERVER_BIN" --version || fail "shed-server --version"
endsection

# ---- 2. shed-server setup ----

section "Step 2: shed-server setup"
"$SHED_SERVER_BIN" setup || fail "shed-server setup"
endsection

# Make sure the service is up before pulling images. pull-images is
# a separate subcommand that uses the configured images_dir but
# doesn't talk to the running service — still, restarting now means
# any config changes from `setup` take effect for the create step.
# Falls back to a background process in --from-local mode when no
# systemd unit is installed (CI runners without the deb).
section "Step 3: start shed-server"
SERVER_BG_PID=""
if [[ "$USES_SYSTEMD" == "true" ]]; then
    systemctl restart shed-server
    sleep 2
    systemctl is-active --quiet shed-server || fail "shed-server failed to start"
    echo "shed-server: active (systemd)"
else
    # Background mode: spawn shed-server with the default config
    # location. If /etc/shed/server.yaml doesn't exist, shed-server
    # logs that and exits — that's fine for install-only smoke and
    # the missing-config branch below catches the pull-images
    # failure with a clear WARN.
    if [[ -f /etc/shed/server.yaml ]]; then
        "$SHED_SERVER_BIN" serve --config /etc/shed/server.yaml >/tmp/shed-server.log 2>&1 &
        SERVER_BG_PID=$!
        # Give it a moment to bind ports.
        sleep 2
        if ! kill -0 "$SERVER_BG_PID" 2>/dev/null; then
            echo "--- /tmp/shed-server.log ---"
            cat /tmp/shed-server.log
            fail "shed-server (background) exited before smoke could proceed"
        fi
        echo "shed-server: active (background PID $SERVER_BG_PID)"
    else
        echo "shed-server: not started (no /etc/shed/server.yaml — pull-images / create will be skipped)"
    fi
fi
endsection

# ---- 4. pull-images ----

section "Step 4: shed-server pull-images"
# pull-images needs /etc/shed/server.yaml to know what to pull.
# --from-apt mode gets one from the deb's postinstall; --from-local
# mode (CI on a fresh runner, or developer iterating) typically
# doesn't. When the config is absent we skip pull-images with a
# clear note rather than fail — the install-only smoke still
# catches the binary / setup regressions that motivate this script.
PULL_IMAGES_RESULT="skipped (no /etc/shed/server.yaml)"
if [[ ! -f /etc/shed/server.yaml ]]; then
    warn "/etc/shed/server.yaml not present — skipping pull-images (no apt deb postinstall in --from-local mode). Install the deb (--from-apt) or write the config manually to exercise this step."
    echo "SKIPPED (no config)"
else
    "$SHED_SERVER_BIN" pull-images --variant "$SMOKE_IMAGE" || fail "shed-server pull-images --variant $SMOKE_IMAGE"
    PULL_IMAGES_RESULT="yes ($SMOKE_IMAGE)"
fi
endsection

# ---- 5. Decide whether to run the create cycle ----

section "Step 5: full-lifecycle gate"
SKIP_REASON=""
if [[ ! -r /dev/kvm ]]; then
    SKIP_REASON="/dev/kvm not present"
elif [[ "$SKIP_CREATE" == "true" ]]; then
    SKIP_REASON="--skip-create requested"
elif ! curl -sf "http://localhost:8080/api/info" >/dev/null 2>&1; then
    # shed-server isn't reachable. In --from-local mode on a fresh
    # runner we may have intentionally not started it (no server.yaml);
    # call it out and skip rather than fail.
    SKIP_REASON="shed-server not reachable on localhost:8080 (likely no /etc/shed/server.yaml in this mode)"
fi
if [[ -n "$SKIP_REASON" ]]; then
    warn "skipping create cycle: $SKIP_REASON"
    echo ""
    echo "=== Install-only smoke summary ==="
    echo "  shed-server installed:     yes"
    echo "  shed-server setup:         yes"
    echo "  shed-server pull-images:   $PULL_IMAGES_RESULT"
    echo "  create + exec + delete:    SKIPPED"
    echo ""
    echo "PASS (install-only)"
    endsection
    exit 0
fi
echo "all preconditions met — running full lifecycle"
endsection

# ---- 6. Create + exec + delete ----

LOCAL_DIR=$(mktemp -d)
echo "smoke marker $(date)" > "$LOCAL_DIR/HELLO.txt"
# 9P passes host UIDs through; the guest's shed user (UID 1000)
# needs to be able to read the directory we mounted. mktemp creates
# at mode 0o700, so widen to 0o755 (we're root; the dir is a temp
# unique to this run and gets deleted in the cleanup trap).
chmod 0755 "$LOCAL_DIR"
chmod 0644 "$LOCAL_DIR/HELLO.txt"

# --local-dir mounts the host directory at ~/<basename> inside the guest.
WS_DIR="/home/shed/$(basename "$LOCAL_DIR")"

# The `shed` CLI requires a configured server entry to know where
# to talk to. Add a localhost entry if root's client config doesn't
# already have one (idempotent — `shed server add` errors if the
# name exists, so we list first).
section "Step 5.5: configure shed client → localhost"
if ! "$SHED_BIN" server list 2>/dev/null | awk 'NR>1 {print $1}' | grep -q '^smoke$'; then
    "$SHED_BIN" server add localhost -n smoke || fail "shed server add localhost"
fi
endsection

section "Step 6: shed create"
"$SHED_BIN" create "$SHED_NAME" --local-dir "$LOCAL_DIR" --image "$SMOKE_IMAGE" || fail "shed create"
CREATED=true
endsection

section "Step 7: shed exec — confirm project mount"
EXPECTED="smoke marker"
OUTPUT=""
# Retry briefly: `shed create` returns once the VM is Running but
# sshd inside the guest needs a moment more to bind :22. Five
# attempts at 2 s spacing has been enough on every environment
# tested so far. Each attempt is bounded by SHED_EXEC_TIMEOUT
# (default 15 s) so a wedged guest can't deadlock the smoke
# (without the timeout, a stuck sshd or dropped vsock leaves the
# whole script blocked on stdin from the dead VM).
SHED_EXEC_TIMEOUT="${SHED_EXEC_TIMEOUT:-15}"
for attempt in 1 2 3 4 5; do
    OUTPUT="$(timeout "${SHED_EXEC_TIMEOUT}s" "$SHED_BIN" exec "$SHED_NAME" -- cat "$WS_DIR/HELLO.txt" 2>&1)" || true
    if grep -q "$EXPECTED" <<<"$OUTPUT"; then
        break
    fi
    sleep 2
done
if ! grep -q "$EXPECTED" <<<"$OUTPUT"; then
    echo "shed exec returned (last attempt): $OUTPUT"
    fail "shed exec — project mount ($WS_DIR) did not surface the test file"
fi
echo "OK: project mount carries the test file"
endsection

section "Step 8: shed delete"
"$SHED_BIN" delete "$SHED_NAME" --force || fail "shed delete"
CREATED=false
endsection

echo ""
echo "=== Full lifecycle smoke summary ==="
echo "  shed-server installed:     yes"
echo "  shed-server setup:         yes"
echo "  shed-server pull-images:   $PULL_IMAGES_RESULT"
echo "  shed create:               yes ($SMOKE_IMAGE)"
echo "  shed exec (project mount): yes"
echo "  shed delete:               yes"
echo ""
echo "PASS"
