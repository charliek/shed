# VZ Operations (macOS Apple Silicon)

This guide covers day-to-day operations with the VZ backend.

VZ support is currently Apple Silicon-only.

## Basic Operations

### Create a shed

```bash
# Basic creation
shed create myproject --backend=vz

# With a git repository
shed create myproject --backend=vz --repo=git@github.com:user/repo.git

# With a local directory (mounted via VirtioFS)
shed create myproject --backend=vz --local-dir=/path/to/project

# With custom resources
shed create myproject --backend=vz --cpus=4 --memory=8192
```

### Start and stop

```bash
shed start myproject
shed stop myproject
```

### Delete

```bash
shed delete myproject
```

### List sheds

```bash
shed list
```

## Local Directory Mounting

When using `--local-dir`, the host directory is shared with the VM via VirtioFS and mounted at `/workspace` inside the guest. Changes on either side are immediately visible to the other.

```bash
shed create myproject --backend=vz --local-dir=~/projects/myapp
shed console myproject
# Inside the VM: ls /workspace shows the contents of ~/projects/myapp
```

`--local-dir` is mutually exclusive with `--repo`. If the VirtioFS mount fails (e.g., the guest kernel lacks `CONFIG_VIRTIO_FS`), the create or start operation will fail with an error.

## Credentials

All credentials configured in `server.yaml` are mounted into VZ VMs via VirtioFS. Changes are immediately visible on both sides, similar to Docker bind mounts.

Read-only credentials (`readonly: true`) are enforced as read-only at the mount level. Writable credentials (`readonly: false`) reflect changes immediately in both directions.

Configure credentials in `server.yaml`:

```yaml
credentials:
  claude:
    source: ~/.claude
    target: /home/shed/.claude
    readonly: false
```

## Provisioning

Provisioning hooks execute in the VM via vsock, identically to Firecracker. Credentials are mounted via VirtioFS before hooks run.

For the full sequence of operations during create, start, stop, and delete (including how VZ differs from other backends), see [Shed Lifecycle](provisioning.md#shed-lifecycle). For hook configuration, see [Provisioning](provisioning.md).

## Inspecting VMs

### Metadata

Each instance stores metadata at:

```
~/Library/Application Support/shed/vz/instances/<name>/metadata.json
```

```bash
cat ~/Library/Application\ Support/shed/vz/instances/myproject/metadata.json | jq .
```

### vfkit process

```bash
ps aux | grep vfkit
```

### Socket files

Each VM creates per-port Unix sockets:

```bash
ls ~/.shed/vz/sockets/
# myproject-1024.sock  (console)
# myproject-1026.sock  (message bus: health, plugins)
```

## Networking

VZ uses NAT networking provided by Apple's Virtualization.framework. The guest obtains an IP address via DHCP through `systemd-networkd`.

From the host, `shed` commands communicate with the VM over vsock (Unix sockets), not TCP. The `GetNetworkEndpoint` API returns `127.0.0.1`.

## Docker Inside VZ

Docker is pre-installed in the VZ rootfs image. It starts automatically via systemd:

```bash
shed console myproject
docker ps
docker run hello-world
```

## Debugging

### Manual health check

```bash
# Connect to the message bus socket (health checks use system:health namespace)
nc -U ~/.shed/vz/sockets/myproject-1026.sock
```

### View console log

Each VM writes boot and console output to a log file:

```bash
cat ~/Library/Application\ Support/shed/vz/instances/myproject/console.log
```

### Check vfkit process

```bash
ps aux | grep vfkit
```

### View instance metadata

```bash
cat ~/Library/Application\ Support/shed/vz/instances/myproject/metadata.json
```

## macOS-Specific Notes

- **Code signing**: The shed-server binary must be signed with the `com.apple.security.virtualization` entitlement.
- **No `/proc`**: Process identification uses `ps -p <pid> -o comm=` instead of reading `/proc/<pid>/cmdline`.
- **Console device**: VZ uses `hvc0` (virtio console) instead of Firecracker's `ttyS0`.
- **vfkit subprocess model**: Each VM runs as a separate vfkit process. Stopping a VM sends SIGTERM, then SIGKILL after timeout.
- **Socket naming**: Per-port sockets follow the pattern `<name>-<port>.sock` in the socket directory.
