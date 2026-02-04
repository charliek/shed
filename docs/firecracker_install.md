# Firecracker Backend Installation

This guide covers the installation and setup of the Firecracker backend for shed.

## Prerequisites

- Linux host with KVM support
- Root access (for network setup)
- Docker (for building rootfs and downloading kernel)
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
- `/var/lib/shed/firecracker/vmlinux.bin` - Linux kernel (from Weave Ignite, with Docker support)
- `/var/lib/shed/firecracker/vmlinux-minimal.bin` - Minimal kernel (fallback, no Docker support)

**Note:** The script requires Docker to extract the kernel from the `weaveworks/ignite-kernel` image.

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

## 7. Set Capabilities (Alternative to Running as Root)

To run shed-server without sudo, grant capabilities to BOTH binaries:

```bash
# Both binaries need CAP_NET_ADMIN for TAP device creation
sudo setcap cap_net_admin+ep ./bin/shed-server
sudo setcap cap_net_admin+ep /usr/local/bin/firecracker

# Verify capabilities are set
getcap ./bin/shed-server /usr/local/bin/firecracker
```

**Note:** Firecracker is spawned as a child process, and Linux capabilities don't inherit to child processes. That's why both binaries need the capability set directly.

## 8. Start the Server

```bash
# With capabilities set (from step 7)
shed-server serve

# Or run as root
sudo shed-server serve
```

## 9. Create a Firecracker Shed

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

or

```
Could not create the network device: Open tap device failed: ... Resource busy
```

Solution: Run shed-server as root or with CAP_NET_ADMIN capability on BOTH binaries:
```bash
sudo shed-server serve

# Or with capabilities (must set on BOTH binaries)
sudo setcap cap_net_admin+ep $(which shed-server)
sudo setcap cap_net_admin+ep $(which firecracker)
```

If you see "Resource busy", clean up stale TAP devices:
```bash
sudo ip link delete shed-tap-0  # or whichever TAP is stale
```

### Vsock Address In Use

```
Cannot create backend for vsock device: UnixBind(Os { code: 98, kind: AddrInUse, message: "Address in use" })
```

Solution: Remove stale vsock socket files:
```bash
sudo rm -f /var/run/shed/firecracker/*.vsock
```

### VM Timeout During Start

```
agent health check failed: context deadline exceeded
```

Possible causes:
1. Rootfs image is corrupted - rebuild with `build-firecracker-rootfs.sh`
2. Kernel is incompatible - try a different kernel version
3. shed-agent is not starting - check VM console output
4. Stale socket files - remove `/var/run/shed/firecracker/*.sock` and `*.vsock`

### No Network Connectivity in VM

**How network is configured:** The VM's IP address is passed via kernel command line arguments using the kernel IP autoconfig format: `ip=<client>::<gateway>:<netmask>::<device>:off`. For example: `ip=172.30.0.2::172.30.0.1:255.255.255.0::eth0:off`. The kernel configures the network interface automatically during boot.

Verify:
1. Bridge is up: `ip link show shed-br0`
2. IP forwarding is enabled: `cat /proc/sys/net/ipv4/ip_forward`
3. NAT rules are in place: `sudo iptables -t nat -L -n`
4. TAP device is attached to bridge: `ip link show`
5. Check kernel args inside VM: `shed exec myproject -- cat /proc/cmdline`
6. Check network-setup ran: `shed exec myproject -- systemctl status network-setup`
7. Check interface is up: `shed exec myproject -- ip addr show eth0`

### Docker Fails to Start in VM

Docker is configured with the `vfs` storage driver and `cgroupfs` driver for compatibility with the Firecracker kernel. If Docker fails to start:

```bash
# Check Docker daemon status
shed exec myproject -- systemctl status docker

# View Docker daemon logs
shed exec myproject -- journalctl -u docker

# Verify daemon.json exists
shed exec myproject -- cat /etc/docker/daemon.json

# Check cgroup support (kernel args should include cgroup_enable=memory)
shed exec myproject -- cat /proc/cmdline | grep cgroup
```

If you see cgroup errors, ensure the kernel args include `cgroup_enable=memory cgroup_memory=1`. This is set automatically by shed-server.

### Docker Containers Fail to Start (BPF Error)

If you see errors like `bpf_prog_query(BPF_CGROUP_DEVICE) failed`:

```
runc create failed: unable to start container process: error during container init:
error setting cgroup config for procHooks process: bpf_prog_query(BPF_CGROUP_DEVICE)
failed: invalid argument
```

This means the kernel lacks BPF cgroup support. The default kernel downloaded by `download-firecracker.sh` (from Weave Ignite) has BPF support. If you're using the minimal kernel (`vmlinux-minimal.bin`), switch to the default kernel which has:
- `CONFIG_BPF=y`
- `CONFIG_BPF_SYSCALL=y`
- `CONFIG_CGROUP_BPF=y`

To switch kernels, update `kernel_path` in your server.yaml:
```yaml
firecracker:
  kernel_path: /var/lib/shed/firecracker/vmlinux.bin  # Full kernel with Docker support
  # kernel_path: /var/lib/shed/firecracker/vmlinux-minimal.bin  # Minimal kernel (no Docker)
```

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
