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

When using `--local-dir`, the host directory is shared with the VM via VirtioFS and mounted at `/home/shed/<basename>` inside the guest, where `<basename>` is the last path segment of the host directory. Changes on either side are immediately visible to the other, and the shed lands there on login.

```bash
shed create myproject --backend=vz --local-dir=~/projects/myapp
shed console myproject
# Inside the VM: lands in /home/shed/myapp, the contents of ~/projects/myapp
```

To mount additional reference directories alongside the primary one, repeat `--add-dir` (valid only with `--local-dir`). Each is mounted at `/home/shed/<basename>` as a sibling:

```bash
shed create myproject --backend=vz \
  --local-dir=~/projects/myapp \
  --add-dir=~/projects/shared-lib
# /home/shed/myapp and /home/shed/shared-lib are both VirtioFS-backed
```

No two mounted directories may share a basename, and dotfile basenames are rejected. `--local-dir`/`--add-dir` are mutually exclusive with `--repo`. If the VirtioFS mount fails (e.g., the guest kernel lacks `CONFIG_VIRTIO_FS`), the create or start operation will fail with an error.

## Mounts

All mounts configured under the `mounts:` section of `server.yaml` are bound into VZ VMs via VirtioFS. Changes are immediately visible on both sides, similar to Docker bind mounts.

Read-only mounts (`readonly: true`) are enforced as read-only at the mount level. Writable mounts (`readonly: false`) reflect changes immediately in both directions.

Configure mounts in `server.yaml`:

```yaml
mounts:
  claude:
    source: ~/.claude
    target: /home/shed/.claude
    readonly: false
```

The legacy `credentials:` key is still honored as a fallback when `mounts:` is absent.

## Provisioning

Provisioning hooks execute in the VM via vsock, identically to Firecracker. Mounts are bound via VirtioFS before hooks run.

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

From the host, `shed` commands communicate with the VM over vsock (Unix sockets), not TCP. `shed list` and the API still report a `127.0.0.1` IP for running VZ sheds (so the field has a sensible value in JSON output), but service traffic itself routes through `DialService`'s vsock TCP-proxy hop rather than that IP.

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

## Time synchronization

VZ guests keep their clock correct two ways:

- **`systemd-timesyncd`** provides standard network time discipline (SNTP).
- **`shed-agent`** periodically steps the system clock forward to the host-backed RTC. This covers host **sleep/suspend**: while the host sleeps, the paused guest's wall clock (`CLOCK_REALTIME`, derived from a frozen counter) falls behind real time and gets no resume event to correct it. The RTC keeps real time across the pause, so the agent re-syncs from it within ~30 s of a large drift — before `timesyncd`'s next network poll would. Without this, a guest resumed after a long sleep can be hours behind, which breaks AWS SigV4/STS signing, TLS validity windows, and token expiry.

Verify inside a shed:

```bash
timedatectl                                  # "NTP service: active"
sudo journalctl -u shed-agent | grep clock-sync
# clock-sync: started (checking the host RTC every 30s, stepping when drift > 30s)
# ...and after a host sleep, the correction:
# clock-sync: stepped clock forward 2h3m0s (system ... -> RTC ...)
```

On an **older image** built before this shipped (no `systemd-timesyncd`, so `timedatectl` shows `NTP service: n/a`), step the clock from the correct RTC manually, then re-pull a current image:

```bash
sudo timedatectl set-time "$(cat /sys/class/rtc/rtc0/date) $(cat /sys/class/rtc/rtc0/time)"
```

## macOS-Specific Notes

- **Code signing**: The shed-server binary must be signed with the `com.apple.security.virtualization` entitlement.
- **No `/proc`**: Process identification uses `ps -p <pid> -o comm=` instead of reading `/proc/<pid>/cmdline`.
- **Console device**: VZ uses `hvc0` (virtio console) instead of Firecracker's `ttyS0`.
- **vfkit subprocess model**: Each VM runs as a separate vfkit process. Stopping a VM sends SIGTERM, then SIGKILL after timeout.
- **Socket naming**: Per-port sockets follow the pattern `<name>-<port>.sock` in the socket directory.
