#!/usr/bin/env bash
# Remove everything that must not be shared by every shed made from the
# image: host keys, authorized keys, the machine ID, the bake login, cloud-init
# instance state, the builder's identity, logs and package lists.
#
# Runs as root on the builder after verify.sh --phase live, then
# verify.sh --phase sealed checks the result. The VM is stopped right after;
# nothing here may run again before then.
#
# Usage: seal.sh [--bake-user NAME]
#   --bake-user  the login the bake used; deleted with its home and sudo rule
set -euo pipefail

bake_user=""
while [ $# -gt 0 ]; do
  case "$1" in
    --bake-user) bake_user="${2:?}"; shift 2 ;;
    *) echo "seal: unknown argument: $1" >&2; exit 2 ;;
  esac
done
[ "$(id -u)" -eq 0 ] || { echo "seal: must run as root" >&2; exit 2; }
log() { echo "==> seal: $*"; }

log "stopping the agent and clearing its runtime state"
systemctl stop shed-agent.service || true
systemctl stop shed-identity.service || true
rm -rf /run/shed /run/shed-agent

log "clearing Docker's state from verification"
if systemctl is-active --quiet docker.service; then
  docker system prune --all --force --volumes >/dev/null || true
fi
systemctl stop docker.service docker.socket containerd.service || true
# Per-install identity; Docker makes a new one on start.
rm -f /var/lib/docker/engine-id

if [ -n "${bake_user}" ] && id "${bake_user}" >/dev/null 2>&1; then
  log "deleting the bake user ${bake_user}"
  # Its SSH session is still open (it is running this script); -f deletes
  # the account anyway, and the session ends when the bake does.
  userdel --force --remove "${bake_user}" 2>/dev/null || userdel --force "${bake_user}"
  rm -rf "/home/${bake_user:?}"
fi
rm -f /etc/sudoers.d/90-cloud-init-users

log "removing keys"
rm -f /etc/ssh/ssh_host_*
rm -f /etc/ssh/shed-link/authorized_keys
find /root /home -name 'authorized_keys*' -type f -delete 2>/dev/null || true

log "resetting cloud-init and guest agent instance state"
if command -v cloud-init >/dev/null 2>&1; then
  cloud-init clean --logs
fi
# The guest agent's record of the instance it last set up.
rm -f /etc/instance_id

log "clearing the builder's identity"
: > /etc/machine-id
if [ -f /var/lib/dbus/machine-id ] && [ ! -L /var/lib/dbus/machine-id ]; then
  rm -f /var/lib/dbus/machine-id
fi
echo localhost > /etc/hostname
if [ -f /etc/hosts ]; then
  # Rewritten in place, not renamed over (it may be a mount point).
  hosts="$(grep -Ev '# shed-identity$|# Added by Google$' /etc/hosts || true)"
  printf '%s\n' "${hosts}" > /etc/hosts
fi
rm -f /var/lib/systemd/random-seed

log "clearing package lists, logs and history"
apt-get clean
rm -rf /var/lib/apt/lists/*
# journald writes to /run until shutdown, then the next boot starts a fresh
# journal under the new machine ID.
journalctl --relinquish-var >/dev/null 2>&1 || true
rm -rf /var/log/journal/*
find /var/log -path /var/log/journal -prune -o -type f \( -name '*.gz' -o -name '*.[0-9]' -o -name '*.old' \) -print0 | xargs -0r rm -f
find /var/log -path /var/log/journal -prune -o -type f -print0 | xargs -0r truncate -s 0
rm -f /root/.bash_history /home/shed/.bash_history
rm -rf /root/.ssh /var/tmp/* 2>/dev/null || true

sync
log "done"
