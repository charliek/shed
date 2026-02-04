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
├── rootfs.ext4      # VM's root filesystem (copy of base)
└── firecracker.sock # API socket (when running)
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
# Remove old API sockets
rm -f /var/run/shed/firecracker/*.sock
```

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

Docker is pre-installed in the rootfs and works out of the box:

```bash
# Run Docker commands inside the VM
shed exec myproject -- docker run hello-world

# Check Docker status
shed exec myproject -- systemctl status docker

# View Docker images
shed exec myproject -- docker images
```

## Debugging

### View VM Console Output

The VM console output goes to the Firecracker process. To see it:

```bash
# Run shed-server in foreground with debug logging
shed-server serve
```

### Check Agent Status

```bash
# Inside the VM
shed exec myproject -- systemctl status shed-agent

# View agent logs
shed exec myproject -- journalctl -u shed-agent
```

### Manually Test vsock Connection

From the host, you can use `socat` to test vsock:

```bash
# Install socat if needed
sudo apt install socat

# Connect to agent health port (port 1025, CID from metadata)
CID=$(cat /var/lib/shed/firecracker/instances/myproject/metadata.json | jq -r '.cid')
socat - SOCKET-CONNECT:40:0:x0000x$(printf '%04x' 1025)x$(printf '%08x' $CID)x0000000000000000
```

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

# Or grant capabilities
sudo setcap cap_net_admin+ep $(which shed-server)
```
