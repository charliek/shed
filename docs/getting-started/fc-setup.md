# Firecracker Setup

This guide covers the installation and setup of the Firecracker backend for shed. Firecracker is Linux-only and requires KVM.

## Prerequisites

- Linux host with KVM support
- Root access (for network setup)
- Docker (for pulling and converting images, or building from source)

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

Run the download script to get the Firecracker binary:

```bash
./scripts/download-firecracker.sh
```

This installs:
- `/usr/local/bin/firecracker` - Firecracker binary (v1.14.1)

When using published images (Option A below), the kernel is included in the image and extracted automatically. For custom kernel builds, see `scripts/build-firecracker-kernel.sh`.

## 3. Set Up Firecracker Images

### Option A: Use published images (recommended)

Configure your server to use published Docker image references. Shed auto-pulls and converts them to ext4 on first use. Published images include a custom Firecracker kernel with Docker, 9P, and BPF support — no separate kernel build needed.

```yaml
firecracker:
  base_rootfs: ghcr.io/charliek/shed-fc-base:{version}
  images:
    base: ghcr.io/charliek/shed-fc-base:{version}
  images_dir: /var/lib/shed/firecracker/images
```

Replace `{version}` with the version matching your `shed` binary — run `shed version` to check.

The first `shed create` will pull the image, convert it to ext4, extract the kernel, and cache everything automatically. See [Image Variants](../reference/images.md) for available images and configuration details.

### Option B: Build from source

Build rootfs images locally. Requires Go 1.24+ for compiling `shed-agent`.

```bash
# Build the default variant
./scripts/build-firecracker-rootfs.sh

# Build a specific variant
./scripts/build-firecracker-rootfs.sh --variant base

# Build all variants (base, default, experimental)
./scripts/build-firecracker-rootfs.sh --all
```

This creates ext4 images in `/var/lib/shed/firecracker/images/`:
- `default-rootfs.ext4` - Full development environment with all coding agents
- `base-rootfs.ext4` - Minimal shed infrastructure + basic dev tools
- `experimental-rootfs.ext4` - Default + shed-extensions credential brokering

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

Update your `server.yaml` to enable the Firecracker backend:

```yaml
name: shed-server
http_port: 8080
ssh_port: 2222
enabled_backends:
  - docker
  - firecracker
default_backend: firecracker

# Credentials are copied into VMs at create/start time
# (Firecracker doesn't support bind mounts like Docker)
credentials:
  ssh:
    source: ~/.ssh
    target: /home/shed/.ssh
    readonly: true
  gitconfig:
    source: ~/.gitconfig
    target: /home/shed/.gitconfig
    readonly: true

# Environment variables passed to git clone and provisioning hooks
env_file: ~/.shed/env

firecracker:
  base_rootfs: ghcr.io/charliek/shed-fc-base:{version}
  instance_dir: /var/lib/shed/firecracker/instances
  socket_dir: /var/run/shed/firecracker
  default_cpus: 2
  default_memory_mb: 4096
  default_disk_gb: 20
  vsock_base_cid: 100
  console_port: 1024
  notify_port: 1026
  start_timeout: 120s
  stop_timeout: 10s
  bridge_name: shed-br0
  bridge_cidr: 172.30.0.1/24
  tap_prefix: shed-tap
```

### Configure SSH for Private Repos

To clone private repositories via SSH, create an env file with the `GIT_SSH_COMMAND`:

```bash
mkdir -p ~/.shed
cat > ~/.shed/env << 'EOF'
GIT_SSH_COMMAND=ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -i /home/shed/.ssh/id_ed25519
EOF
```

**Note:** Adjust the SSH key path if you use a different key type (e.g., `id_rsa`).

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

## 9P Kernel Configuration

The `--local-dir` flag and directory credential mounts use the 9P filesystem protocol over the TAP bridge network. This requires the following kernel configuration options to be built into the guest kernel:

```
CONFIG_NET_9P=y
CONFIG_NET_9P_FD=y
CONFIG_9P_FS=y
CONFIG_9P_FS_POSIX_ACL=y
```

The custom kernel built by `build-firecracker-kernel.sh` already includes these options. If you are building your own kernel, add these options to your kernel config fragment.

To verify 9P support inside a running VM:

```bash
shed exec myproject -- grep 9p /proc/filesystems
```

If the output includes `nodev	9p`, the kernel has 9P support.

## Known Limitations

### Server restart does not recover 9P mounts

If `shed-server` restarts while VMs with `--local-dir` or directory credential mounts are running, the 9P servers are not automatically restarted. Running VMs will have stale mounts that return I/O errors. Recovery requires stopping and starting the affected sheds:

```bash
shed stop myproject
shed start myproject
```

### UID mapping

9P maps UIDs directly (1:1). Host UID 1000 corresponds to guest UID 1000. If the host user running `shed-server` has a different UID than the `shed` user inside the VM (UID 1000), file ownership may appear incorrect. This matches the behavior of VirtioFS on Linux. Apple VirtioFS has transparent UID mapping, but that is not available on Linux.

### Firewall and iptables

The 9P TCP servers bind to the bridge IP on dynamically assigned ports. If the host has restrictive `iptables` INPUT rules, these ports may be blocked. Ensure that traffic on the bridge network (default `172.30.0.0/24`) is allowed:

```bash
# Allow all traffic on the shed bridge interface
sudo iptables -A INPUT -i shed-br0 -j ACCEPT
```

This rule is typically not needed if the default INPUT policy is ACCEPT, which is common on desktop Linux systems.

## Troubleshooting

### KVM Permission Denied

```text
failed to create firecracker machine: permission denied
```

Solution: Add your user to the kvm group:
```bash
sudo usermod -aG kvm $USER
# Log out and back in
```

### Bridge Not Found

```text
failed to find bridge shed-br0
```

Solution: Create the bridge network (see step 4).

### TAP Device Creation Failed

```text
failed to create TAP device: operation not permitted
```

or

```text
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

```text
Cannot create backend for vsock device: UnixBind(Os { code: 98, kind: AddrInUse, message: "Address in use" })
```

Solution: Remove stale vsock socket files:
```bash
sudo rm -f /var/run/shed/firecracker/*.vsock
```

### VM Timeout During Start

```text
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

```text
runc create failed: unable to start container process: error during container init:
error setting cgroup config for procHooks process: bpf_prog_query(BPF_CGROUP_DEVICE)
failed: invalid argument
```

This means the kernel lacks BPF cgroup support. The custom 6.1 kernel built by `build-firecracker-kernel.sh` has full BPF support. If you're using the CI fallback kernel, build a custom kernel which includes:
- `CONFIG_BPF=y`
- `CONFIG_BPF_SYSCALL=y`
- `CONFIG_CGROUP_BPF=y`

To build and use the Docker-capable kernel:
```bash
./scripts/build-firecracker-kernel.sh
```

Then set `kernel_path` in your server.yaml to point to the built kernel:
```yaml
firecracker:
  kernel_path: /var/lib/shed/firecracker/vmlinux
```

## Network Architecture

```text
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
