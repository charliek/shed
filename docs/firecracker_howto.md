# Firecracker Backend Operations

This guide covers common operations and troubleshooting for the Firecracker backend.

## Basic Operations

### Creating a Shed

```bash
# Create with default settings
shed create myproject --backend=firecracker

# Create with custom resources
shed create myproject --backend=firecracker --cpus=4 --memory=8192

# Create from a git repository
shed create myproject --backend=firecracker --repo=https://github.com/user/repo.git
```

### Starting and Stopping

```bash
# Start a stopped shed
shed start myproject

# Stop a running shed
shed stop myproject
```

### Connecting

```bash
# Open a console session
shed console myproject

# Run a command
shed exec myproject -- ls -la

# Attach to a tmux session
shed attach myproject
```

### Deleting

```bash
# Delete a shed (removes all data)
shed delete myproject

# Delete without confirmation
shed delete myproject --force
```

## Credentials

Firecracker VMs don't support bind mounts like Docker. Instead, credentials configured in `server.yaml` are **copied** into the VM at create and start time using tar over vsock.

### How It Works

1. On `shed create` and `shed start`, credentials are:
   - Archived on the host using tar
   - Transferred to the VM via vsock
   - Extracted to the target location
   - Ownership is set to root:root

2. Changes to credentials on the host after VM starts won't be reflected in the VM until restart

### Verifying Credentials

```bash
# Check SSH keys were transferred
shed exec myproject -- ls -la /root/.ssh/

# Check git config
shed exec myproject -- cat /root/.gitconfig

# Test SSH access to GitHub
shed exec myproject -- ssh -T git@github.com
```

### Private Repository Access

For private repos, ensure:
1. SSH credentials are configured in `server.yaml`
2. `GIT_SSH_COMMAND` is set in your env file (`~/.shed/env`)

```bash
# Example env file for SSH git access
cat ~/.shed/env
GIT_SSH_COMMAND=ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -i /root/.ssh/id_ed25519
```

## Provisioning

Shed supports automatic provisioning via `.shed/provision.yaml` in your repository. This works the same as Docker but uses vsock for execution.

### Provisioning Flow

1. **On Create** (with `--repo`):
   - Credentials are transferred
   - Repository is cloned
   - Install hook runs (if configured)
   - Startup hook runs (if configured)

2. **On Start** (restart):
   - Credentials are refreshed
   - Startup hook runs (install hook is skipped)

### Skip Provisioning

```bash
# Create without running hooks
shed create myproject --backend=firecracker --repo=https://github.com/user/repo.git --no-provision
```

### Check Provisioning Logs

```bash
# View install hook output
shed exec myproject -- cat /var/log/shed/install.log

# View startup hook output
shed exec myproject -- cat /var/log/shed/startup.log

# Check provisioning state
shed exec myproject -- cat /var/log/shed/.provision_state
```

### Example provision.yaml

```yaml
hooks:
  install: .shed/scripts/install.sh    # Runs once on create
  startup: .shed/scripts/startup.sh    # Runs on every start

env:
  MY_VAR: "value"

timeout: 30m
```

## Inspecting VMs

### List All Sheds

```bash
shed list
```

### View VM Metadata

VM metadata is stored in JSON files:

```bash
cat /var/lib/shed/firecracker/instances/myproject/metadata.json
```

Example output:
```json
{
  "name": "myproject",
  "status": "running",
  "created_at": "2024-01-15T10:30:00Z",
  "backend": "firecracker",
  "cid": 101,
  "pid": 12345,
  "ip_address": "172.30.0.2",
  "tap_device": "shed-tap-0",
  "cpus": 2,
  "memory_mb": 4096,
  "rootfs_path": "/var/lib/shed/firecracker/instances/myproject/rootfs.ext4"
}
```

### Check VM Process

```bash
# Find the Firecracker process
ps aux | grep firecracker | grep myproject

# Check VM resource usage
top -p $(cat /var/lib/shed/firecracker/instances/myproject/metadata.json | jq -r '.pid')
```

### View Network Interfaces

```bash
# List TAP devices
ip link show | grep shed-tap

# Show bridge status
brctl show shed-br0
# Or with ip command
bridge link show
```

## Networking

### Test VM Connectivity

```bash
# Ping the VM from host
ping 172.30.0.2

# SSH into the VM (if SSH is configured)
ssh shed@172.30.0.2
```

### Port Forwarding

Port forwarding works via the VM's IP address:

```bash
# Forward local port 3000 to VM port 3000
ssh -L 3000:172.30.0.2:3000 shed@172.30.0.2 -N

# Or use shed tunnels (if implemented for firecracker)
shed tunnel myproject 3000:3000
```

### Inside the VM

```bash
# Check network configuration
shed exec myproject -- ip addr show

# Test internet connectivity
shed exec myproject -- curl -I https://google.com
```

## Storage

### VM Disk Layout

Each VM has a copy of the base rootfs:

```
/var/lib/shed/firecracker/instances/myproject/
├── metadata.json    # VM configuration and state
└── rootfs.ext4      # VM's root filesystem (copy of base)

/var/run/shed/firecracker/  # Runtime sockets (when VM is running)
├── myproject.sock   # Firecracker API socket
└── myproject.vsock  # vsock UDS for guest communication
```

### Expanding Disk Space

To resize a VM's rootfs:

```bash
# Stop the VM first
shed stop myproject

# Resize the image
sudo truncate -s 40G /var/lib/shed/firecracker/instances/myproject/rootfs.ext4
sudo e2fsck -f /var/lib/shed/firecracker/instances/myproject/rootfs.ext4
sudo resize2fs /var/lib/shed/firecracker/instances/myproject/rootfs.ext4

# Start the VM
shed start myproject
```

### Backing Up a VM

```bash
# Stop the VM
shed stop myproject

# Copy the instance directory
cp -r /var/lib/shed/firecracker/instances/myproject /backup/myproject-backup

# Restart
shed start myproject
```

## Cleanup

### Remove Stale TAP Devices

If TAP devices are left behind after a crash:

```bash
# List all shed TAP devices
ip link show | grep shed-tap

# Remove a specific TAP device
sudo ip link delete shed-tap-0

# Remove all shed TAP devices
for tap in $(ip link show | grep -o 'shed-tap-[0-9]*'); do
    sudo ip link delete $tap
done
```

### Clean Up Stale Sockets

```bash
# Remove old API and vsock sockets
sudo rm -f /var/run/shed/firecracker/*.sock
sudo rm -f /var/run/shed/firecracker/*.vsock
```

**Note:** Stale vsock sockets cause "Address in use" errors on VM start. The shed-server cleans these up automatically on VM stop, but manual cleanup may be needed after crashes.

### Full Reset

To completely reset the Firecracker backend:

```bash
# Stop all VMs
shed list | tail -n +2 | awk '{print $1}' | xargs -I{} shed stop {}

# Remove all instances
sudo rm -rf /var/lib/shed/firecracker/instances/*

# Remove stale TAP devices
for tap in $(ip link show | grep -o 'shed-tap-[0-9]*'); do
    sudo ip link delete $tap
done

# Remove sockets
rm -f /var/run/shed/firecracker/*.sock
```

## Docker Inside VMs

Docker is pre-installed in the rootfs and configured with the `vfs` storage driver for maximum reliability in VM environments.

```bash
# Run Docker commands inside the VM
shed exec myproject -- docker run hello-world

# Check Docker status
shed exec myproject -- systemctl status docker

# View Docker images
shed exec myproject -- docker images
```

### Docker Troubleshooting

If Docker fails to start:

```bash
# Check daemon logs
shed exec myproject -- journalctl -u docker

# Verify daemon.json config
shed exec myproject -- cat /etc/docker/daemon.json
# Should show: "storage-driver": "vfs"

# Check cgroup kernel args
shed exec myproject -- cat /proc/cmdline | grep cgroup
# Should include: cgroup_enable=memory cgroup_memory=1

# Manually start Docker with debug
shed exec myproject -- dockerd --debug
```

The `vfs` storage driver is slower than overlay2 but always works in VM environments where overlay filesystems may not be supported.

**Note:** Docker bridge networking is disabled (kernel lacks nftables support), so containers use `--network=host` by default. This means containers share the VM's network namespace. The default kernel (from Weave Ignite) has BPF cgroup support, so containers start correctly.

If using the minimal kernel (`vmlinux-minimal.bin`), containers will fail with BPF errors. Switch to the default kernel for Docker support.

## Debugging

### View VM Console Output

The VM console output goes to the Firecracker process. To see it:

```bash
# Run shed-server in foreground with debug logging
shed-server serve
```

### Check Kernel Arguments

Kernel arguments control network configuration and cgroup settings. To verify they were passed correctly:

```bash
# Check kernel args from inside VM
shed exec myproject -- cat /proc/cmdline

# Expected output includes:
# ip=172.30.0.X::172.30.0.1:255.255.255.0::eth0:off cgroup_enable=memory cgroup_memory=1
```

### Verify Network Setup

```bash
# Check if network-setup service ran
shed exec myproject -- systemctl status network-setup

# View network-setup logs
shed exec myproject -- journalctl -u network-setup

# Check interface configuration
shed exec myproject -- ip addr show eth0

# Test connectivity
shed exec myproject -- ping -c 1 172.30.0.1  # gateway
shed exec myproject -- ping -c 1 8.8.8.8      # internet
```

### Check Agent Status

```bash
# Inside the VM
shed exec myproject -- systemctl status shed-agent

# View agent logs
shed exec myproject -- journalctl -u shed-agent
```

### Manually Test vsock Connection

Firecracker exposes vsock via Unix Domain Socket with a special protocol (not kernel vsock):

```bash
# The vsock socket is at:
ls -la /var/run/shed/firecracker/myproject.vsock

# To test manually, connect and send CONNECT command:
# Install socat if needed
sudo apt install socat

# Connect to the UDS and send CONNECT to a port (e.g., health port 1025)
echo -e "CONNECT 1025\n" | socat - UNIX-CONNECT:/var/run/shed/firecracker/myproject.vsock

# A successful connection returns "OK 1025"
```

**Note:** The vsock protocol works as follows:
1. Connect to Firecracker's Unix socket (`myproject.vsock`)
2. Send `CONNECT <port>\n`
3. Receive `OK <port>\n` on success
4. The connection is then bridged to the guest at that port

### Enable Debug Logging

Set log level in server.yaml:

```yaml
log_level: debug
```

## Performance Tuning

### CPU Pinning

By default, VMs share host CPUs. For dedicated CPUs, use CPU pinning (requires jailer):

```bash
# This is an advanced feature not enabled by default
```

### Memory Ballooning

Firecracker supports memory ballooning to reclaim unused memory. This is an advanced feature.

### I/O Performance

For better disk I/O:
1. Use SSD storage for instance directories
2. Consider using tmpfs for socket directory:
   ```bash
   sudo mount -t tmpfs -o size=10M tmpfs /var/run/shed/firecracker
   ```

## Common Issues

### VM Stuck in "starting" State

```bash
# Check if Firecracker process is running
ps aux | grep firecracker

# Check metadata
cat /var/lib/shed/firecracker/instances/myproject/metadata.json

# Force cleanup and try again
shed delete myproject --force
shed create myproject --backend=firecracker
```

### Agent Not Responding

```bash
# Stop and start the VM
shed stop myproject
shed start myproject

# If that doesn't work, delete and recreate
shed delete myproject --force
shed create myproject --backend=firecracker
```

### Permission Errors

Most operations require root or specific capabilities:

```bash
# Run server as root
sudo shed-server serve

# Or grant capabilities to BOTH binaries
# (Firecracker is spawned as a child process and needs its own capabilities)
sudo setcap cap_net_admin+ep $(which shed-server)
sudo setcap cap_net_admin+ep $(which firecracker)
```
