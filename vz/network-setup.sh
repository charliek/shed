#!/bin/bash
# Network setup script for VZ VMs
# VZ uses NAT networking with DHCP, so this script is simpler than Firecracker's.
# systemd-networkd handles DHCP automatically; this script handles cleanup
# and ensures services are ready.

set -e

# Wait for a network interface to come up with an IPv4 address (DHCP via
# systemd-networkd). The interface name is RE-RESOLVED every iteration: the
# kernel renames the NIC shortly after it first appears (e.g. eth0 -> enp0s1),
# so latching the name once can leave us polling a device that no longer
# exists for the full timeout. This matters whenever network-setup runs early
# in boot (it raced the rename and hung ~30s). See
# docs/discovery/platform-runtime-optimization.md §12.
INTERFACE=""
IP_ADDR=""
for i in $(seq 1 30); do
    # VZ presents the NIC as enp0sN or similar; re-resolve each pass.
    INTERFACE=$(ip -o link show | grep -v 'lo:' | awk -F': ' '{print $2}' | head -1)
    if [ -n "$INTERFACE" ]; then
        IP_ADDR=$(ip -4 addr show "$INTERFACE" | grep -oP 'inet \K[0-9.]+' || true)
        if [ -n "$IP_ADDR" ]; then
            echo "Network ready: interface=$INTERFACE ip=$IP_ADDR"
            break
        fi
    fi
    echo "Waiting for network (interface=${INTERFACE:-none})..."
    sleep 1
done

if [ -z "$INTERFACE" ]; then
    echo "WARNING: No network interface found, continuing without network"
elif [ -z "$IP_ADDR" ]; then
    echo "WARNING: No IPv4 address assigned via DHCP on $INTERFACE"
fi

# Ensure /etc/resolv.conf points to systemd-resolved stub.
# Docker overwrites resolv.conf during image build, so we restore the symlink.
if [ ! -L /etc/resolv.conf ] || [ "$(readlink /etc/resolv.conf)" != "../run/systemd/resolve/stub-resolv.conf" ]; then
    rm -f /etc/resolv.conf
    ln -s ../run/systemd/resolve/stub-resolv.conf /etc/resolv.conf
    echo "Restored resolv.conf symlink to systemd-resolved stub"
fi

# Configure hosts file for localhost resolution
cat > /etc/hosts << HOSTS_EOF
127.0.0.1 localhost $(hostname)
::1 localhost ip6-localhost ip6-loopback
HOSTS_EOF

# Clean stale runtime state from unclean VM shutdown.
# The rootfs persists across stop/start, but services expect
# clean state on boot.

# Ensure sshd run directory exists
mkdir -p /var/run/sshd

# Shared memory cleanup (e.g., stale POSIX shared memory segments)
rm -rf /dev/shm/* 2>/dev/null || true

# Lock file cleanup
rm -f /var/lock/*.lock /run/lock/*.lock 2>/dev/null || true

# Temp file cleanup (stale sockets, lock files from previous boot)
find /tmp -mindepth 1 -maxdepth 1 -exec rm -rf {} + 2>/dev/null || true

echo "Network setup complete"
