#!/bin/bash
# Network setup script for Firecracker VMs
# This script configures the network interface with a static IP
# The IP address is passed via kernel command line or derived from the VM's CID

set -e

# Default values
GATEWAY="172.30.0.1"
DNS="8.8.8.8"

# Try to get IP from kernel command line (ip=X.X.X.X)
IP_ADDR=$(cat /proc/cmdline | grep -oP 'ip=\K[0-9.]+' || true)

if [ -z "$IP_ADDR" ]; then
    # If no IP on command line, try to derive from vsock CID
    # CID 100 -> 172.30.0.2, CID 101 -> 172.30.0.3, etc.
    CID=$(cat /sys/class/vsock/cid 2>/dev/null || echo "")
    if [ -n "$CID" ]; then
        if [[ "$CID" =~ ^[0-9]+$ ]]; then
            LAST_OCTET=$((CID - 100 + 2))
            if [ "$LAST_OCTET" -ge 2 ] && [ "$LAST_OCTET" -le 254 ]; then
                IP_ADDR="172.30.0.${LAST_OCTET}"
            else
                echo "Derived IP octet out of range from CID ${CID}: ${LAST_OCTET}"
            fi
        else
            echo "Invalid CID value: $CID"
        fi
    fi
fi

if [ -z "$IP_ADDR" ]; then
    echo "No IP address configured, skipping network setup"
    exit 0
fi

echo "Configuring network: IP=$IP_ADDR, Gateway=$GATEWAY"

# Resolve the primary non-loopback interface, re-resolving each pass until it
# appears. Previously hardcoded to eth0; the kernel can rename the NIC
# (e.g. eth0 -> enp0s1), so resolve dynamically with eth0 as a last-resort
# fallback. See docs/discovery/runtime-optimization-backlog.md §12.
INTERFACE=""
for i in $(seq 1 10); do
    INTERFACE=$(ip -o link show | grep -v 'lo:' | awk -F': ' '{print $2}' | head -1)
    if [ -n "$INTERFACE" ]; then
        break
    fi
    echo "Waiting for network interface..."
    sleep 1
done
if [ -z "$INTERFACE" ]; then
    INTERFACE="eth0"
    echo "WARNING: no interface detected, falling back to $INTERFACE"
fi
echo "Using interface: $INTERFACE"

# Configure interface
ip addr add "${IP_ADDR}/24" dev "$INTERFACE" 2>/dev/null || true
ip link set "$INTERFACE" up
ip route add default via "$GATEWAY" 2>/dev/null || true

# Lower the guest MTU when shed-server detected a reduced host egress path
# (e.g. a VPN/overlay on the FC host routing through a <1500 link). shed.mtu=
# is emitted on the kernel cmdline ONLY in that case (see
# vmutil.ResolveGuestMTU), so a normal 1500 path leaves the guest unchanged.
# FC uses a static IP (no systemd-networkd to re-assert the link), so an
# imperative set sticks. No MSS clamp here: the FC custom kernel lacks the
# xt_TCPMSS target (tracked as a fast-follow); the MTU-lowering alone fixes
# dockerd's own registry pulls. Bounds mirror config.MinGuestMTU/MaxGuestMTU.
SHED_MTU=$(grep -oP 'shed\.mtu=\K[0-9]+' /proc/cmdline 2>/dev/null || true)
if [ -n "$SHED_MTU" ] && [ "$SHED_MTU" -ge 1280 ] && [ "$SHED_MTU" -le 1500 ] && [ -n "$INTERFACE" ]; then
    echo "Applying guest MTU $SHED_MTU to $INTERFACE (reduced host egress path)"
    ip link set "$INTERFACE" mtu "$SHED_MTU" || true
fi

# Configure DNS
echo "nameserver $DNS" > /etc/resolv.conf

# Configure hosts file for localhost resolution
cat > /etc/hosts << HOSTS_EOF
127.0.0.1 localhost
::1 localhost ip6-localhost ip6-loopback
HOSTS_EOF

# Clean stale runtime state from unclean VM shutdown.
# The rootfs overlay persists across stop/start, but services expect
# clean state on boot. Generic system-level cleanup only —
# service-specific cleanup belongs in the startup hook.

# Ensure sshd run directory exists (rootfs-level service)
mkdir -p /var/run/sshd

# Shared memory cleanup (e.g., stale POSIX shared memory segments)
rm -rf /dev/shm/* 2>/dev/null || true

# Lock file cleanup
rm -f /var/lock/*.lock /run/lock/*.lock 2>/dev/null || true

# Temp file cleanup (stale sockets, lock files from previous boot)
find /tmp -mindepth 1 -maxdepth 1 -exec rm -rf {} + 2>/dev/null || true

echo "Network configuration complete"
