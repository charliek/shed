#!/usr/bin/env bash
# shed-agent-install: replace the agent binary and restart the agent.
#
# Installed as /usr/local/sbin/shed-agent-install (root:root 0755). The
# server runs it as shed-link through the one sudo rule in
# /etc/sudoers.d/shed-link (guest.InstallCommand):
#
#   sudo -n /usr/local/sbin/shed-agent-install <sha256>
#
# with the new binary on stdin (at most 256 MiB). It writes the binary to a
# temporary file beside the agent, refuses it unless its sha256 equals the
# argument, sets root:root 0755, renames it onto the agent path, restarts
# shed-agent.service, prints "installed <sha256>" and exits 0. Any failure
# exits non-zero with the reason on stderr; a failure before the rename
# leaves the old binary in place.
set -euo pipefail
umask 022
PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin

agent=/usr/local/bin/shed-agent
unit=shed-agent.service
max_bytes=$((256 * 1024 * 1024))

die() { echo "shed-agent-install: $*" >&2; exit 1; }

[ "$#" -eq 1 ] || die "usage: shed-agent-install <sha256>"
want="$1"
printf '%s' "${want}" | grep -Eq '^[0-9a-f]{64}$' || die "the argument must be 64 lowercase hex digits"
[ "$(id -u)" -eq 0 ] || die "must run as root"

tmp="$(mktemp "$(dirname "${agent}")/.shed-agent.XXXXXX")"
trap 'rm -f "${tmp}"' EXIT

# Read one byte past the cap so an oversized upload is refused, not cut.
head -c "$((max_bytes + 1))" > "${tmp}"
size="$(stat -c %s "${tmp}")"
[ "${size}" -gt 0 ] || die "no binary on stdin"
[ "${size}" -le "${max_bytes}" ] || die "binary is larger than ${max_bytes} bytes"

got="$(sha256sum "${tmp}" | cut -d' ' -f1)"
[ "${got}" = "${want}" ] || die "sha256 mismatch: got ${got}, want ${want}"

chown root:root "${tmp}"
chmod 0755 "${tmp}"
mv -f "${tmp}" "${agent}"
trap - EXIT

systemctl restart "${unit}" || die "installed ${want} but could not restart ${unit}"
echo "installed ${want}"
