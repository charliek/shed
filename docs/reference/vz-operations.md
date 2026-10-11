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

## Restarting the server

From v0.9.1, VZ VMs outlive `shed-server`: `brew services restart shed` and `brew services stop shed` leave running sheds running, and the next server start re-adopts them. To stop sheds, run `shed stop <name>` first.

```bash
shed stop <name>              # for each shed you want stopped
brew services restart shed
```

A restart can take up to about 30 s while `shed-server` shuts down gracefully. If the server cannot come back, the VMs keep running unmanaged until it does. A foreground `shed-server` stopped with Ctrl-C no longer stops its VMs either. Upgrading from 0.9.0 stops running sheds once; see the [v0.9.0 to v0.9.1 guide](../upgrades/v0.9.0-to-v0.9.1.md).

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

## Image store on an external drive

macOS asks for permission before `shed-server` can read a state directory on an external drive (`/Volumes/...`). The same applies to protected folders such as `~/Documents`, `~/Desktop` and `~/Downloads`. The prompt asks to allow access to the volume or folder, and names the process macOS holds responsible: the server under the brew service, or your terminal app when you run `shed-server` from a terminal.

### What the log shows

Before it starts the backend, the server reads each state directory once and names them:

```text
Checking state directories: /Volumes/ext/shed/vz, /Volumes/ext/shed/vz/instances, /Volumes/ext/shed/vz/snapshots, /Volumes/ext/shed/vz/uppers, /Users/me/.shed/vz/sockets
```

If nobody answers the prompt, for example on a headless Mac or a service started at login, startup waits at that directory. After 10 seconds, and then every minute, it logs:

```text
shed-server: startup is still waiting on /Volumes/ext/shed/vz (checking state directory) after 10s. macOS may be waiting for permission to access it (external drives, and folders like Documents, need it). Answer the "Shed Server" prompt on this Mac's screen, or grant Full Disk Access to <path> in System Settings → Privacy & Security → Full Disk Access. It continues on its own once access is granted.
```

`<path>` is what to grant access to, with symlinks resolved. Answer the prompt or grant Full Disk Access, and startup continues without a restart. The same line with `resuming running sheds` in place of `startup` can appear later, from the background walk that reconnects running sheds.

If the prompt was answered with **Don't Allow**, the read fails instead and the server logs a warning:

```text
Warning: cannot read /Volumes/ext/shed/vz: open /Volumes/ext/shed/vz: operation not permitted. macOS denied access; grant Full Disk Access to <path> in System Settings → Privacy & Security → Full Disk Access, then restart shed-server.
```

The check itself is not fatal, but a later step that needs the directory can still fail. With egress enabled, for example, opening the audit log next to `instance_dir` stops startup.

Read the log with:

```bash
tail -f "$(brew --prefix)/var/log/shed-server.log"
```

### Grant Full Disk Access

1. Open **System Settings → Privacy & Security → Full Disk Access**.
2. Click **+**, press **Cmd-Shift-G**, paste the `<path>` from the log line, press **Return**, and click **Open**.
3. Make sure its switch is on.

After a **Don't Allow**, restart the server once access is granted. Running sheds keep running across the restart.

```bash
brew services restart shed
```

### Across upgrades

Whether a grant survives `brew upgrade shed` depends on what macOS records it against:

| Server installed as | Grant recorded against | After an upgrade |
|---|---|---|
| A bare executable | Its absolute path, which includes the Homebrew version directory | macOS asks again |
| An app bundle signed with the same identity in every release | The bundle identifier and its signing requirement | The grant carries over |

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
- **vfkit subprocess model**: Each VM runs as a separate vfkit process. Stopping a VM sends SIGTERM, then SIGKILL after timeout. vfkit runs in its own process group so a launchd restart of `shed-server` does not kill it (v0.9.1+).
- **Socket naming**: Per-port sockets follow the pattern `<name>-<port>.sock` in the socket directory.
