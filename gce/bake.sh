#!/usr/bin/env bash
# The whole in-VM part of a bake, in one root process so it survives the bake
# user being deleted part way: provision, start the boot unit and the agent
# from the builder's own metadata, verify, seal, verify the seal.
#
# scripts/build-gce-image.sh copies this directory to the builder as
# /tmp/shed-bake together with:
#   shed-agent    the linux agent binary
#   pins.env      from scripts/read-guest-pins.sh
#   link_key      private half of the builder's shed-link-authorized-keys
# and runs: sudo /tmp/shed-bake/bake.sh --variant V --arch A --bake-user U
#
# The script prints "BAKE OK" as its last line on success. The driver stops
# the VM afterwards; it does not reboot or log in again.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
variant=extensions
arch=""
bake_user=""
while [ $# -gt 0 ]; do
  case "$1" in
    --variant) variant="${2:?}"; shift 2 ;;
    --arch) arch="${2:?}"; shift 2 ;;
    --bake-user) bake_user="${2:?}"; shift 2 ;;
    *) echo "bake: unknown argument: $1" >&2; exit 2 ;;
  esac
done
[ "$(id -u)" -eq 0 ] || { echo "bake: must run as root" >&2; exit 2; }
[ -n "${arch}" ] || arch="$(dpkg --print-architecture)"

"${here}/provision.sh" --variant "${variant}" --arch "${arch}" \
  --agent "${here}/shed-agent" --pins "${here}/pins.env"

echo "==> bake: starting shed-identity and shed-agent from the builder's metadata"
systemctl start shed-identity.service
systemctl start docker.service shed-agent.service
# The agent creates its sockets after it starts; give it a moment.
for _ in $(seq 1 30); do
  [ -S /run/shed-agent/1028.sock ] && break
  sleep 1
done

link_args=()
[ -f "${here}/link_key" ] && link_args=(--link-key "${here}/link_key")
"${here}/verify.sh" --phase live --variant "${variant}" --pins "${here}/pins.env" "${link_args[@]}"

seal_args=()
[ -n "${bake_user}" ] && seal_args=(--bake-user "${bake_user}")
"${here}/seal.sh" "${seal_args[@]}"
"${here}/verify.sh" --phase sealed "${seal_args[@]}"

rm -rf "${here}"
echo "BAKE OK"
