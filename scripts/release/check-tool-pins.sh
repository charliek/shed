#!/usr/bin/env bash
# check-tool-pins.sh: the linter and goreleaser versions must be the same
# everywhere they are pinned: .mise.toml (local runs), ci.yml (every pull
# request) and publish-images.yaml (the release). The release job lints and
# builds with its own pins, on a tag, where a failure costs a version: v0.10.0
# was tagged with CI green and stopped at the release job's lint step, whose
# golangci-lint was older than CI's and could not read the Go version.
#
# Offline; awk and sed only (it runs on macOS too). Exits 1 on any drift.
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/../.."

fail=0
note() { echo "check-tool-pins: $*" >&2; fail=1; }

# mise_pin TOOL: the version .mise.toml pins, without quotes.
mise_pin() {
	awk -F'"' -v tool="$1" '$0 ~ "^" tool " *= *\"" { print $2; exit }' .mise.toml
}

# action_pins FILE ACTION: each `version:` that follows a `uses: ACTION@...`
# step in FILE, one a line (the first `version:` before the next `uses:`).
action_pins() {
	awk -v action="$2" '
		/uses:/ { want = (index($0, "uses: " action "@") > 0) }
		want && /^[[:space:]]*version:/ { sub(/^[[:space:]]*version:[[:space:]]*/, ""); print; want = 0 }
	' "$1"
}

check() { # TOOL ACTION
	local tool="$1" action="$2" want file pins pin
	want="$(mise_pin "$tool")"
	if [ -z "$want" ]; then
		note ".mise.toml pins no $tool"
		return
	fi
	for file in .github/workflows/ci.yml .github/workflows/publish-images.yaml; do
		pins="$(action_pins "$file" "$action")"
		if [ -z "$pins" ]; then
			note "$file has no pinned $action step"
			continue
		fi
		while IFS= read -r pin; do
			if [ "$pin" != "v$want" ]; then
				note "$file pins $action at $pin, but .mise.toml pins $tool $want"
			fi
		done <<<"$pins"
	done
}

check golangci-lint golangci/golangci-lint-action
check goreleaser goreleaser/goreleaser-action

if [ "$fail" -ne 0 ]; then
	echo "check-tool-pins: move every pin together (.mise.toml, ci.yml, publish-images.yaml)" >&2
	exit 1
fi
echo "tool pins OK: golangci-lint $(mise_pin golangci-lint), goreleaser $(mise_pin goreleaser)"
