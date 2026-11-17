#!/usr/bin/env bash
# dev-server-wait.sh — poll scripts/dev-server-probe.sh (or a command that
# runs it somewhere else) until it says ready, says failed, or the tries run
# out. The Makefile's dev-server-up, dev-server-up-fc and dev-server-up-remote
# all wait through this, so they read the probe's exit status the same way.
#
# Usage:
#   dev-server-wait.sh [--stdin FILE] [--retry STATUS]... <tries> -- <command> [args...]
#
# <command> is run once a second, up to <tries> times, with stdin from FILE
# when --stdin is given (dev-server-up-fc pipes the probe to `bash -s` over
# ssh). Its stdout and stderr are its verdict. The probe's statuses:
#   0  ready    -> print the verdict, exit 0
#   1  not yet  -> try again
#   2  failed   -> print the verdict, exit 2
# A status named by --retry is treated as "not yet" too (ssh's 255 for the
# FC target: the host did not answer this second). ANY other status is a
# failure, exit 2: the probe refusing its arguments (64) or not running at
# all (126, 127) is a broken check, and a broken check must not read as "the
# server is still starting".
#
# Exit status, with one line on stdout:
#   0  ready     "<verdict> (try N of T)"
#   1  timed out the last "not yet" verdict
#   2  failed    the probe's verdict, or "probe exited with unexpected status N: ..."
#  64  usage
set -u

usage() {
  echo "usage: $0 [--stdin FILE] [--retry STATUS]... <tries> -- <command> [args...]" >&2
  exit 64
}

stdin_file=/dev/null
retry=" 1 "
while [ "$#" -gt 0 ]; do
  case "$1" in
    --stdin) [ "$#" -ge 2 ] || usage; stdin_file="$2"; shift 2 ;;
    --retry)
      [ "$#" -ge 2 ] || usage
      case "$2" in '' | *[!0-9]*) usage ;; esac
      retry="$retry$2 "; shift 2 ;;
    *) break ;;
  esac
done
[ "$#" -ge 3 ] || usage
tries="$1"
case "$tries" in '' | *[!0-9]*) usage ;; esac
[ "$tries" -ge 1 ] || usage
[ "$2" = "--" ] || usage
shift 2

# One line: the command's output with newlines folded to spaces.
oneline() { printf '%s' "$1" | tr '\n' ' ' | sed 's/ *$//'; }

out=""
i=1
while :; do
  out="$("$@" < "$stdin_file" 2>&1)"
  rc=$?
  case "$rc" in
    0) echo "$(oneline "$out") (try $i of $tries)"; exit 0 ;;
    2) oneline "$out"; echo; exit 2 ;;
  esac
  case "$retry" in
    *" $rc "*) ;;
    *) echo "probe exited with unexpected status $rc: $(oneline "$out")"; exit 2 ;;
  esac
  [ "$i" -lt "$tries" ] || break
  i=$((i + 1))
  sleep 1
done
oneline "$out"
echo
exit 1
