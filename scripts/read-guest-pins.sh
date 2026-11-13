#!/usr/bin/env bash
# Print the guest tool pins the GCE image bake installs, parsed from
# firecracker/Dockerfile.
#
# The Firecracker Dockerfile is the one place these pins are written down:
# `make check-roost-pin` holds its roost-session ARGs in lockstep with vz's and
# with crates/shed-core, and `make check-craze-pin` holds its craze ARGs in
# lockstep with vz's and with craze-pin.env. Reading them from here (rather
# than keeping a third copy in gce/) means those two checks guard the cloud
# image too, and `make check-gce-pins` refuses a literal pin in gce/.
#
# The file is parsed as DATA (never sourced). Each value is validated before
# it is printed, so a caller can use the output with no further quoting.
#
# Usage:
#   scripts/read-guest-pins.sh [path/to/Dockerfile]
#     Prints seven KEY=value lines in a fixed order:
#       ROOST_SESSION_VERSION=X.Y.Z
#       ROOST_SESSION_PROTOCOL=N
#       ROOST_SESSION_SHA256_AMD64=<64 hex>
#       ROOST_SESSION_SHA256_ARM64=<64 hex>
#       CRAZE_VERSION=X.Y.Z
#       CRAZE_SHA256_AMD64=<64 hex>
#       CRAZE_SHA256_ARM64=<64 hex>
#   Exits 1, naming the key, when any value is missing or malformed.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
dockerfile="${1:-${repo_root}/firecracker/Dockerfile}"

if [ ! -f "${dockerfile}" ]; then
  echo "read-guest-pins: ${dockerfile} not found" >&2
  exit 1
fi

# key|pattern the value must match in full
specs=(
  'ROOST_SESSION_VERSION|^[0-9]+\.[0-9]+\.[0-9]+$'
  'ROOST_SESSION_PROTOCOL|^[0-9]+$'
  'ROOST_SESSION_SHA256_AMD64|^[0-9a-f]{64}$'
  'ROOST_SESSION_SHA256_ARM64|^[0-9a-f]{64}$'
  'CRAZE_VERSION|^[0-9]+\.[0-9]+\.[0-9]+$'
  'CRAZE_SHA256_AMD64|^[0-9a-f]{64}$'
  'CRAZE_SHA256_ARM64|^[0-9a-f]{64}$'
)

status=0
out=""
for spec in "${specs[@]}"; do
  key="${spec%%|*}"
  pattern="${spec#*|}"
  # First non-comment `ARG KEY=value` line; the value is taken literally.
  value="$(grep -Ev '^[[:space:]]*#' "${dockerfile}" \
    | sed -nE "s/^[[:space:]]*ARG[[:space:]]+${key}=(.*)\$/\\1/p" \
    | head -1)"
  if [ -z "${value}" ]; then
    echo "read-guest-pins: ARG ${key} is missing or empty in ${dockerfile}" >&2
    status=1
    continue
  fi
  if ! printf '%s' "${value}" | grep -Eq "${pattern}"; then
    echo "read-guest-pins: ARG ${key}='${value}' in ${dockerfile} does not match ${pattern}" >&2
    status=1
    continue
  fi
  out+="${key}=${value}"$'\n'
done

if [ "${status}" -ne 0 ]; then
  exit "${status}"
fi
printf '%s' "${out}"
