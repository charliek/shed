#!/bin/bash
# Network setup script for Firecracker VMs
# This script configures the network interface with a static IP
# The IP address is passed via kernel command line or derived from the VM's CID

set -e

# Default values
INTERFACE="eth0"
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

# Wait for interface to be available
for i in $(seq 1 10); do
    if ip link show "$INTERFACE" &>/dev/null; then
        break
    fi
    echo "Waiting for $INTERFACE..."
    sleep 1
done

# Configure interface
ip addr add "${IP_ADDR}/24" dev "$INTERFACE" 2>/dev/null || true
ip link set "$INTERFACE" up
ip route add default via "$GATEWAY" 2>/dev/null || true

# Configure DNS
echo "nameserver $DNS" > /etc/resolv.conf

# Configure hosts file for localhost resolution
cat > /etc/hosts << HOSTS_EOF
127.0.0.1 localhost
::1 localhost ip6-localhost ip6-loopback
HOSTS_EOF

# Clean stale runtime state from unclean VM shutdown.
# The rootfs overlay persists across stop/start, but services expect
# clean state on boot. Remove stale PID/socket files.
rm -rf /var/run/postgresql /var/run/redis /var/run/sshd
mkdir -p /var/run/postgresql && chown postgres:postgres /var/run/postgresql 2>/dev/null || true
mkdir -p /var/run/redis && chown redis:redis /var/run/redis 2>/dev/null || true
mkdir -p /var/run/sshd

echo "Network configuration complete"
