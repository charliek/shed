#!/usr/bin/env bash
# dev-server-probe.sh — one readiness check of a parallel dev shed-server,
# for the Makefile's dev-server-up and dev-server-up-remote (run here) and
# dev-server-up-fc (piped to the FC host over ssh and run there with
# `bash -s`), and for the integration suite's dev_config() restarts. The
# caller polls it: the Makefile through scripts/dev-server-wait.sh, the suite
# in fixtures/devcontrol.py. Both treat any status but 0, 1 and 2 as a
# failure, so a probe that refuses its arguments (64) never reads as "not yet".
#
# Usage:
#   dev-server-probe.sh <log> <pid> [settle-seconds] [first-line]
#
# <log> is the server's log; <pid> is the launched server's process id. Only
# the log from line <first-line> on (default 1, the whole file) is read: a
# caller that empties the log before the launch passes nothing, and one that
# appends to it (dev-server-up-remote) passes the line count before the
# launch, so nothing from an earlier run is read.
#
# Exit status, with one line on stdout saying why:
#   0  ready   the server is up and its listeners accept connections
#   1  not yet keep polling
#   2  failed  the process is gone or the server reported a listener error
#
# Why not just the "Shed server is ready" line: serve.go starts its HTTP(S)
# and SSH listeners in goroutines, logs that line, and only THEN reads the
# listeners' errors. A server whose port is taken logs "ready", then
# "Server error: ...", then exits, and the line stays in the log. So "ready"
# here also needs: the process still running, a "listening on" line for an
# HTTP(S) listener and for the SSH listener, a TCP connection accepted on
# every logged address, and, after <settle-seconds> (default 1), the process
# still running with no "Server error:" in the log. The settle covers the
# instant between the readiness line and a bind error being logged, when a
# port held by some other process would still accept the connection.
#
# The probe needs no credential, so it works before any `shed server add`:
# it only opens a TCP connection and closes it. Under mtls the server logs a
# TLS handshake error for it; that line is expected noise.
set -u

usage() {
  echo "usage: $0 <log> <pid> [settle-seconds] [first-line]" >&2
  exit 64
}

[ "$#" -ge 2 ] || usage
log="$1"
pid="$2"
settle="${3:-1}"
first="${4:-1}"
case "$first" in '' | *[!0-9]*) usage ;; esac
[ "$first" -ge 1 ] || first=1

# This run's part of the log.
logtext() { tail -n "+$first" "$log" 2>/dev/null; }
not_yet() { echo "not yet: $1"; exit 1; }
failed() { echo "failed: $1"; exit 2; }

# ps rather than kill -0: the FC dev server runs as root and this probe runs
# as the SSH user, who cannot signal it. A zombie counts as gone.
alive() {
  case "$pid" in '' | *[!0-9]*) return 1 ;; esac
  local stat
  stat="$(ps -p "$pid" -o stat= 2>/dev/null)" || return 1
  case "$stat" in '' | *Z*) return 1 ;; esac
  return 0
}

check_not_failed() {
  alive || failed "the server process (pid '${pid}') is not running"
  local err
  err="$(logtext | grep -m1 'Server error:')"
  [ -z "$err" ] || failed "$err"
}

# One TCP connection, opened and closed. bash's /dev/tcp under timeout where
# there is a timeout (Linux), else nc (macOS ships nc but no timeout).
connects() {
  if command -v timeout >/dev/null 2>&1; then
    timeout 2 bash -c 'exec 3<>"/dev/tcp/$0/$1"' "$1" "$2" 2>/dev/null
  elif command -v nc >/dev/null 2>&1; then
    nc -z -w 2 "$1" "$2" >/dev/null 2>&1
  else
    bash -c 'exec 3<>"/dev/tcp/$0/$1"' "$1" "$2" 2>/dev/null
  fi
}

check_not_failed
logtext | grep -q 'Shed server is ready' || not_yet "no readiness line in $log yet"
logtext | grep -Eq 'HTTPS? server listening on ' || not_yet "no HTTP(S) listener logged yet"
logtext | grep -q 'SSH server listening on ' || not_yet "no SSH listener logged yet"

addrs="$(logtext | sed -n 's/.* server listening on \([^ ]*\)$/\1/p')"
for addr in $addrs; do
  port="${addr##*:}"
  host="${addr%:*}"
  host="${host#\[}"
  host="${host%\]}"
  case "$host" in '' | 0.0.0.0 | ::) host=127.0.0.1 ;; esac
  connects "$host" "$port" || not_yet "nothing accepts a connection on $addr yet"
done

sleep "$settle"
check_not_failed
# shellcheck disable=SC2086 # one line, addresses split on purpose
echo "ready: listening on" $addrs
exit 0
