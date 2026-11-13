#!/bin/sh
# Plays the part of the guest's boot unit, then runs the agent and sshd.
# SHED_LINK_AUTHORIZED_KEYS holds the server's public key (never a secret).
set -eu

if [ -z "${SHED_LINK_AUTHORIZED_KEYS:-}" ]; then
    echo "SHED_LINK_AUTHORIZED_KEYS is not set" >&2
    exit 1
fi

# The image bakes no host key; the first boot makes one and later boots keep it.
if [ ! -f /etc/ssh/ssh_host_ed25519_key ]; then
    ssh-keygen -q -t ed25519 -N '' -C '' -f /etc/ssh/ssh_host_ed25519_key
fi

tmp=$(mktemp /etc/ssh/shed-link/authorized_keys.XXXXXX)
printf '%s\n' "$SHED_LINK_AUTHORIZED_KEYS" > "$tmp"
chmod 0644 "$tmp"
mv "$tmp" /etc/ssh/shed-link/authorized_keys

# Extra sshd settings for one test. sshd keeps the first value it reads, so
# a file that sorts before 10-shed.conf overrides it.
if [ -n "${SHED_SSHD_EXTRA_CONFIG:-}" ]; then
    printf '%s\n' "$SHED_SSHD_EXTRA_CONFIG" > /etc/ssh/sshd_config.d/05-test.conf
fi

mkdir -p /run/sshd
/usr/local/bin/shed-agent --listen unix:/run/shed-agent --socket-group shed-link \
    --shed-name "${SHED_NAME:-sshd-test}" &

# -e: log to stderr, so "docker logs" shows why a login was refused.
exec /usr/sbin/sshd -D -e
