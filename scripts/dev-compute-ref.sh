#!/usr/bin/env bash
# dev-compute-ref.sh — run the in-tree reference account service (the
# directory and compute contracts, internal/directory/refserver) on loopback
# for the remote dev server: `remote.provider: service` with nothing private
# and no cloud. Its machines are records in memory; run the dev server with
# SHED_REMOTE_TRANSPORT=local, so each shed's agent runs on this host.
#
# Usage: dev-compute-ref.sh up|grant|down|status
#
#   up      start it (bin/test/shed-directory-ref, from `make directory-ref`),
#           with an empty list of people and a fresh enrolment code for the
#           server name, and print the URL and the code; it rewrites
#           <dir>/inventory.json every second with the machines it holds (the
#           integration suite confirms its machines are gone by their ids)
#   grant   let the enrolled server manage compute and register the client
#           nocturn for it (run after `shed-server directory enroll`)
#   down    stop it (every machine it held is forgotten)
#   status  say whether it runs
#
# Settings (environment): SHED_COMPUTE_REF_DIR (default
# ~/.shed/dev-remote/ref), SHED_COMPUTE_REF_PORT (default 18700),
# SHED_COMPUTE_REF_SERVER (default remote-dev, the name the dev server
# enrols under). People go in <dir>/control/users.json, as the reference
# service reads it (see refserver.ControlDir).
set -euo pipefail

DIR="${SHED_COMPUTE_REF_DIR:-$HOME/.shed/dev-remote/ref}"
PORT="${SHED_COMPUTE_REF_PORT:-18700}"
SERVER="${SHED_COMPUTE_REF_SERVER:-remote-dev}"
BIN="${SHED_COMPUTE_REF_BIN:-bin/test/shed-directory-ref}"
CONTROL="$DIR/control"
PIDFILE="$DIR/ref.pid"
LOG="$DIR/ref.log"
URL="http://127.0.0.1:$PORT"

running() { [ -f "$PIDFILE" ] && kill -0 "$(cat "$PIDFILE")" 2>/dev/null; }

umask 077
case "${1:-}" in
  up)
    if running; then
      echo "The reference service already runs (pid $(cat "$PIDFILE")) at $URL."
      exit 1
    fi
    [ -x "$BIN" ] || { echo "ERROR: no $BIN; run 'make directory-ref' first."; exit 1; }
    mkdir -p "$CONTROL"
    chmod 700 "$DIR" "$CONTROL"
    [ -f "$CONTROL/users.json" ] || printf '[]\n' > "$CONTROL/users.json"
    code="$(od -An -N8 -tx1 /dev/urandom | tr -d ' \n')"
    printf '%s %s\n' "$SERVER" "$code" > "$CONTROL/codes"
    printf '{"steps": [], "auto_settle_s": 3}\n' > "$CONTROL/compute.json"
    : > "$LOG"
    nohup "$BIN" --control "$CONTROL" --listen "127.0.0.1:$PORT" --inventory "$DIR/inventory.json" >> "$LOG" 2>&1 &
    echo $! > "$PIDFILE"
    for _ in $(seq 1 50); do
      grep -q '^listening on ' "$LOG" 2>/dev/null && break
      running || { echo "ERROR: the reference service did not start:"; cat "$LOG"; rm -f "$PIDFILE"; exit 1; }
      sleep 0.1
    done
    echo "Reference service: $URL (pid $(cat "$PIDFILE"), log $LOG)"
    echo "  Enrolment code for $SERVER: $code"
    echo "  In the dev env file: SHED_REMOTE_DIRECTORY_URL=$URL and SHED_REMOTE_TRANSPORT=local"
    echo "  Then: make dev-server-up-remote; awk '{print \$2}' $CONTROL/codes | bin/shed-server directory enroll --config ~/.shed/dev-remote/server.yaml --name $SERVER; make dev-compute-ref-grant"
    echo "  (shed-server directory enroll reads the code from standard input; there is no --code flag)"
    ;;
  grant)
    running || { echo "ERROR: the reference service does not run; 'make dev-compute-ref-up' first."; exit 1; }
    id="grant-$(date +%s)"
    printf '{"steps": [{"id": "%s-allow", "step": {"compute.allow": "%s"}}, {"id": "%s-client", "step": {"client.register": ["%s", "nocturn"]}}], "auto_settle_s": 3}\n' \
      "$id" "$SERVER" "$id" "$SERVER" > "$CONTROL/compute.json"
    echo "Granted compute to $SERVER and registered the client nocturn (applied at the service's next tick)."
    ;;
  down)
    if running; then
      kill -TERM "$(cat "$PIDFILE")"
      echo "Reference service stopped; the machines it held are forgotten."
    else
      echo "The reference service does not run."
    fi
    rm -f "$PIDFILE"
    ;;
  status)
    if running; then echo "Reference service: running (pid $(cat "$PIDFILE")) at $URL"; else echo "Reference service: not running"; fi
    ;;
  *)
    echo "usage: $0 up|grant|down|status" >&2
    exit 2
    ;;
esac
