#!/usr/bin/env bash
# Drive a running ShedDesktop.app and capture labeled screenshots of each
# pane + the menu popover. Useful for eyeballing UI changes.
#
# Usage: tools/screenshot/smoke.sh [OUT_DIR]   (default: /tmp/shed-desktop-smoke)
#
# Assumes the app is already running (e.g. `make run`). Uses the bundled
# shedctl so it drives whatever instance is live.

set -euo pipefail

OUT="${1:-/tmp/shed-desktop-smoke}"
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
CTL="${REPO_ROOT}/build/ShedDesktop.app/Contents/Resources/bin/shedctl"
if [ ! -x "$CTL" ]; then CTL="shedctl"; fi

mkdir -p "$OUT"
echo "==> screenshots -> $OUT"

"$CTL" ui show-window >/dev/null
"$CTL" sheds refresh >/dev/null || true

for pane in sheds approvals agents activity egress system; do
  "$CTL" ui navigate "$pane" >/dev/null
  "$CTL" screenshot --surface window --scale 2 --out "$OUT/pane-$pane.png"
done

"$CTL" ui open-menu true >/dev/null
"$CTL" screenshot --surface menu --scale 2 --out "$OUT/menu.png"
"$CTL" ui open-menu false >/dev/null

{
  echo "# shed-desktop smoke screenshots"
  echo
  for f in pane-sheds pane-approvals pane-agents pane-activity pane-egress pane-system menu; do
    echo "- $f.png"
  done
} > "$OUT/manifest.md"

echo "==> done. See $OUT/manifest.md"
