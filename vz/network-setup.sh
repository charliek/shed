#!/bin/bash
# Network setup script for VZ VMs
# VZ uses NAT networking with DHCP, so this script is simpler than Firecracker's.
# systemd-networkd handles DHCP automatically; this script handles cleanup
# and ensures services are ready.

set -e

# Wait for a network interface to be available
INTERFACE=""
for i in $(seq 1 15); do
    # VZ presents the NIC as enp0sN or similar
    INTERFACE=$(ip -o link show | grep -v 'lo:' | awk -F': ' '{print $2}' | head -1)
    if [ -n "$INTERFACE" ]; then
        break
    fi
    echo "Waiting for network interface..."
    sleep 1
done

if [ -z "$INTERFACE" ]; then
    echo "WARNING: No network interface found, continuing without network"
fi

# Wait for an IP address (DHCP via systemd-networkd)
if [ -n "$INTERFACE" ]; then
    for i in $(seq 1 30); do
        IP_ADDR=$(ip -4 addr show "$INTERFACE" | grep -oP 'inet \K[0-9.]+' || true)
        if [ -n "$IP_ADDR" ]; then
            echo "Network ready: interface=$INTERFACE ip=$IP_ADDR"
            break
        fi
        echo "Waiting for DHCP on $INTERFACE..."
        sleep 1
    done

    if [ -z "$IP_ADDR" ]; then
        echo "WARNING: No IP address assigned via DHCP"
    fi
fi

# Configure hosts file for localhost resolution
cat > /etc/hosts << HOSTS_EOF
127.0.0.1 localhost
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
