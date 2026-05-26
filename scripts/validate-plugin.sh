#!/usr/bin/env bash
# Validate the Claude Code plugin: manifest JSON parses and every skill has the
# required YAML frontmatter (name + description). Runner-agnostic (jq + python3).
# Run locally or from CI: scripts/validate-plugin.sh
set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$root"

jq -e . .claude-plugin/plugin.json >/dev/null
jq -e . .claude-plugin/marketplace.json >/dev/null
echo "ok: .claude-plugin JSON parses"

python3 - <<'PY'
import re, pathlib, sys

skills = sorted(pathlib.Path("skills").glob("*/SKILL.md"))
if not skills:
    sys.exit("FAIL: no skills/*/SKILL.md found")

bad = 0
for sk in skills:
    m = re.match(r"^---\n(.*?)\n---\n", sk.read_text(), re.S)
    if not m:
        print(f"FAIL: {sk} missing YAML frontmatter"); bad += 1; continue
    fm = m.group(1)
    missing = [f for f in ("name", "description") if not re.search(rf"^{f}:\s*\S", fm, re.M)]
    if missing:
        print(f"FAIL: {sk} missing {missing}"); bad += 1
    else:
        print(f"ok: {sk}")

sys.exit(1 if bad else 0)
PY
