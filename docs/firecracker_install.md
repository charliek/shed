# Firecracker Backend Installation

This guide covers the installation and setup of the Firecracker backend for shed.

## Prerequisites

- Linux host with KVM support
- Root access (for network setup)
- Docker (for building rootfs)
- Go 1.21+ (for building shed-agent)

## 1. Check KVM Support

Firecracker requires hardware virtualization (KVM). Verify it's available:

```bash
# Check if KVM is available
ls -la /dev/kvm

# If not accessible, add your user to the kvm group
sudo usermod -aG kvm $USER
# Log out and back in for changes to take effect
```

## 2. Download Firecracker

Run the download script to get Firecracker and a compatible kernel:

```bash
./scripts/download-firecracker.sh
```

This installs:
- `/usr/local/bin/firecracker` - Firecracker binary
- `/var/lib/shed/firecracker/vmlinux.bin` - Linux kernel

## 3. Build the Rootfs Image

Build the base rootfs image that VMs will use:

```bash
./scripts/build-firecracker-rootfs.sh
```

This creates:
- `/var/lib/shed/firecracker/base-rootfs.ext4` - 20GB ext4 image with Ubuntu 24.04, Docker, and shed-agent

## 4. Set Up Bridge Network

Firecracker VMs need a bridge network for connectivity. This is a one-time setup.

### Create the Bridge

```bash
# Create bridge
sudo ip link add shed-br0 type bridge
sudo ip addr add 172.30.0.1/24 dev shed-br0
sudo ip link set shed-br0 up
```

### Enable IP Forwarding

```bash
# Enable IP forwarding (temporary)
echo 1 | sudo tee /proc/sys/net/ipv4/ip_forward

# Make permanent
echo "net.ipv4.ip_forward = 1" | sudo tee /etc/sysctl.d/99-ip-forward.conf
sudo sysctl -p /etc/sysctl.d/99-ip-forward.conf
```

### Configure NAT for Internet Access

```bash
# Add NAT rule for outbound traffic
sudo iptables -t nat -A POSTROUTING -s 172.30.0.0/24 -j MASQUERADE

# Allow forwarding
sudo iptables -A FORWARD -i shed-br0 -j ACCEPT
sudo iptables -A FORWARD -o shed-br0 -j ACCEPT
```

### Make Network Persistent (Optional)

To persist the bridge across reboots, create a systemd-networkd configuration:

```bash
# /etc/systemd/network/shed-br0.netdev
cat << 'EOF' | sudo tee /etc/systemd/network/shed-br0.netdev
[NetDev]
Name=shed-br0
Kind=bridge
EOF

# /etc/systemd/network/shed-br0.network
cat << 'EOF' | sudo tee /etc/systemd/network/shed-br0.network
[Match]
Name=shed-br0

[Network]
Address=172.30.0.1/24
ConfigureWithoutCarrier=yes
EOF

# Enable systemd-networkd
sudo systemctl enable systemd-networkd
sudo systemctl restart systemd-networkd
```

For iptables persistence, install `iptables-persistent`:

```bash
sudo apt install iptables-persistent
sudo netfilter-persistent save
```

## 5. Configure shed-server

Update your `server.yaml` to use the Firecracker backend:

```yaml
name: shed-server
http_port: 8080
ssh_port: 2222
backend: firecracker

firecracker:
  kernel_path: /var/lib/shed/firecracker/vmlinux.bin
  base_rootfs: /var/lib/shed/firecracker/base-rootfs.ext4
  instance_dir: /var/lib/shed/firecracker/instances
  socket_dir: /var/run/shed/firecracker
  default_cpus: 2
  default_memory_mb: 4096
  default_disk_gb: 20
  vsock_base_cid: 100
  console_port: 1024
  health_port: 1025
  start_timeout: 30s
  stop_timeout: 10s
  bridge_name: shed-br0
  bridge_cidr: 172.30.0.1/24
  tap_prefix: shed-tap
```

## 6. Create Required Directories

```bash
sudo mkdir -p /var/lib/shed/firecracker/instances
sudo mkdir -p /var/run/shed/firecracker
sudo chown -R $USER:$USER /var/lib/shed/firecracker
sudo chown -R $USER:$USER /var/run/shed/firecracker
```

## 7. Start the Server

```bash
shed-server serve
```

## 8. Create a Firecracker Shed

```bash
# Create a shed with the Firecracker backend
shed create myproject --backend=firecracker

# Or with custom resources
shed create myproject --backend=firecracker --cpus=4 --memory=8192
```

## Troubleshooting

### KVM Permission Denied

```
failed to create firecracker machine: permission denied
```

Solution: Add your user to the kvm group:
```bash
sudo usermod -aG kvm $USER
# Log out and back in
```

### Bridge Not Found

```
failed to find bridge shed-br0
```

Solution: Create the bridge network (see step 4).

### TAP Device Creation Failed

```
failed to create TAP device: operation not permitted
```

Solution: Run shed-server as root or with CAP_NET_ADMIN capability:
```bash
sudo shed-server serve
# Or with capabilities
sudo setcap cap_net_admin+ep $(which shed-server)
```

### VM Timeout During Start

```
agent health check failed: context deadline exceeded
```

Possible causes:
1. Rootfs image is corrupted - rebuild with `build-firecracker-rootfs.sh`
2. Kernel is incompatible - try a different kernel version
3. shed-agent is not starting - check VM console output

### No Network Connectivity in VM

Verify:
1. Bridge is up: `ip link show shed-br0`
2. IP forwarding is enabled: `cat /proc/sys/net/ipv4/ip_forward`
3. NAT rules are in place: `sudo iptables -t nat -L -n`
4. TAP device is attached to bridge: `ip link show`

## Network Architecture

```
                    ┌─────────────┐
                    │   Host      │
                    │  eth0/wlan  │
                    └──────┬──────┘
                           │ NAT (iptables MASQUERADE)
                    ┌──────┴──────┐
                    │  shed-br0   │  172.30.0.1/24
                    │   (bridge)  │
                    └──────┬──────┘
              ┌────────────┼────────────┐
              │            │            │
        ┌─────┴─────┐┌─────┴─────┐┌─────┴─────┐
        │shed-tap-0 ││shed-tap-1 ││shed-tap-2 │
        └─────┬─────┘└─────┬─────┘└─────┬─────┘
              │            │            │
        ┌─────┴─────┐┌─────┴─────┐┌─────┴─────┐
        │   VM 0    ││   VM 1    ││   VM 2    │
        │172.30.0.2 ││172.30.0.3 ││172.30.0.4 │
        └───────────┘└───────────┘└───────────┘
```

Each VM gets:
- A dedicated TAP device attached to the bridge
- A static IP in the 172.30.0.0/24 network
- Internet access via NAT
- vsock for communication with the host
