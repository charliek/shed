# CLI Reference

Complete reference for the `shed` command-line interface.

## Global Flags

| Flag | Short | Description |
|------|-------|-------------|
| `--server` | `-s` | Target server (overrides default) |
| `--verbose` | `-v` | Increase output verbosity (`-v` for expanded, `-vv` for full detail) |
| `--config` | `-c` | Config file path (default: `~/.shed/config.yaml`) |
| `--json` | | Emit structured JSON to stdout (suppresses verbose output) |

## Server Management

### shed server add

Adds a new server to the client configuration.

```bash
shed server add <host> [flags]
```

| Flag | Short | Default | Description |
|------|-------|---------|-------------|
| `--name` | `-n` | Derived from host | Friendly name for server |
| `--port` | `-p` | `8080` | HTTP API port |

The SSH port is automatically discovered from the server's `/api/info` endpoint.

**Example:**

```bash
shed server add mini-desktop.tailnet.ts.net --name mini
```

### shed server list

Lists configured servers.

```bash
shed server list
```

**Output:**

```
NAME            HOST                              HTTP    SSH     STATUS     DEFAULT
mini-desktop    mini-desktop.tailnet.ts.net       8080    2222    online     *
cloud-vps       vps.tailnet.ts.net                8080    2222    offline
```

### shed server remove

Removes a server from configuration.

```bash
shed server remove <name>
```

### shed server set-default

Sets the default server.

```bash
shed server set-default <name>
```

## Shed Lifecycle

### shed create

Creates a new shed.

```bash
shed create <name> [flags]
```

| Flag | Short | Default | Description |
|------|-------|---------|-------------|
| `--repo` | `-r` | None | Repository to clone (`owner/repo` shorthand or full URL) |
| `--server` | `-s` | Default server | Target server |
| `--image` | `-i` | Server default | Image [variant name](images.md) to use |
| `--backend` | | Server default | Backend to use: `firecracker` or `vz` |
| `--cpus` | | Server default | Number of vCPUs |
| `--memory` | | Server default | Memory in MB |
| `--local-dir` | | None | Mount a local host directory as the workspace (mutually exclusive with `--repo`) |
| `--no-provision` | | `false` | Skip provisioning hooks |
| `--sync-profile` | | `default` | Profile to sync after creation |
| `--no-sync` | | `false` | Skip syncing default profile |
| `--timeout` | | `10m` | Timeout for create operation (e.g., `30s`, `5m`, `2h`) |

**Examples:**

```bash
shed create scratch
shed create codelens --repo charliek/codelens
shed create stbot --repo charliek/stbot --server cloud-vps
shed create myproj --sync-profile full
shed create myproj --no-sync
shed create bigproj --repo org/large-repo --timeout 30m

# Mount a local directory as the workspace
shed create myproj --local-dir ~/projects/myproj
```

!!! note "Local directory mounts"
    When using `--local-dir`, the specified host directory is mounted directly as the workspace. No volume is created and `--repo` cannot be used. VZ uses VirtioFS; Firecracker uses 9P over TCP.

### shed list

Lists sheds.

```bash
shed list [flags]
```

| Flag | Short | Default | Description |
|------|-------|---------|-------------|
| `--server` | `-s` | Default | List from specific server |
| `--all` | `-a` | false | List from all servers |

**Output (default):**

```text
NAME          BACKEND    STATUS     SSH                              CREATED
codelens      vz         running    codelens@mini-desktop:2222       2026-01-20 10:30
mcp-test      vz         stopped    -                                2026-01-17 14:00
```

**Output with `-v` (expanded):**

```text
NAME          BACKEND    STATUS     SSH                              IP             RESOURCES    SOURCE              UPTIME
codelens      vz         running    codelens@mini-desktop:2222       192.168.64.2   2c/4096MB    charliek/codelens    2h30m
mcp-test      vz         stopped    -                                -              -            -                    -
```

With `-vv`, each shed is displayed as a grouped key-value detail view with network, resources, and runtime sections.

### shed start

Starts a stopped shed.

```bash
shed start <name> [flags]
```

| Flag | Short | Default | Description |
|------|-------|---------|-------------|
| `--timeout` | | `10m` | Timeout for start operation (e.g., `30s`, `5m`, `2h`) |

### shed stop

Stops a running shed.

```bash
shed stop <name>
```

### shed delete

Deletes a shed.

```bash
shed delete <name> [flags]
```

| Flag | Short | Default | Description |
|------|-------|---------|-------------|
| `--keep-volume` | | `false` | Keep the data volume |
| `--force` | `-f` | `false` | Skip confirmation |

**Note:** When using `--json`, the `--force` flag is required (interactive confirmation is not supported in JSON mode).

## Snapshots

A snapshot captures a stopped shed's rootfs as a named, immutable artifact.
New sheds spawned from a snapshot get a fresh identity (machine-id, SSH host
keys, hostname) and their own runtime mounts. See [Snapshots](snapshots.md)
for the full overview.

### shed snapshot create

```bash
shed snapshot create <shed-name> <snapshot-name> [--comment "..."]
```

Captures the source shed's rootfs into a new immutable snapshot. The source
shed must be stopped.

### shed snapshot list

```bash
shed snapshot list
```

Lists snapshots on the current server.

### shed snapshot info

```bash
shed snapshot info <snapshot-name>
```

Shows snapshot metadata, including provenance and size.

### shed snapshot delete

```bash
shed snapshot delete <snapshot-name> [-f]
```

Removes a snapshot. Spawned sheds remain independent (each has its own
rootfs copy).

### shed create --from-snapshot

```bash
shed create <new-name> --from-snapshot <snapshot-name> [--local-dir ...]
```

Spawns a new shed from a snapshot. `--from-snapshot` is mutually exclusive
with `--image` and `--repo` (the snapshot rootfs is the source of truth).
`--local-dir`, `--cpus`, and `--memory` remain valid since they describe
runtime configuration, not rootfs source.

## Image Management

### shed image build

Builds a rootfs image from a Dockerfile or Docker registry image. The target platform is auto-detected from the host OS (linux/amd64 for Firecracker, linux/arm64 for VZ).

```bash
shed image build [flags] [context]
```

| Flag | Short | Default | Description |
|------|-------|---------|-------------|
| `--file` | `-f` | `./Dockerfile.shed` or `./Dockerfile` | Dockerfile path |
| `--from` | | | Docker image reference to convert directly (skips build) |
| `--name` | `-n` | *required* | Image variant name |
| `--target` | | | Docker build target stage (Dockerfile mode only) |
| `--size` | | `20G` | Rootfs image size |
| `--output-dir` | | auto-detected | Output directory |
| `--force` | | `false` | Skip base image validation warning |

**Dockerfile mode:**

```bash
shed image build -f Dockerfile.shed -n myimage .
```

**Registry mode:**

```bash
shed image build --from ghcr.io/charliek/shed-fc-base:{version} -n myimage
```

Replace `{version}` with the version matching your `shed` binary — run `shed version` to check.

### shed image list

Lists available image variants from server config and auto-discovered images.

```bash
shed image list
```

**Output:**

```
NAME         SOURCE       SIZE      CACHED   REF
base         config       2.1 GB    yes      -
default      config       -         no       ghcr.io/charliek/shed-vz-default:{version}
experimental discovered   3.8 GB    yes      -
```

### shed image delete

Deletes a cached rootfs image from the images directory.

```bash
shed image delete <name> [flags]
```

| Flag | Short | Default | Description |
|------|-------|---------|-------------|
| `--force` | | `false` | Skip confirmation prompt |

Removes the cached ext4 rootfs and source sidecar files. Does not affect running sheds (they use copies of the image). Config-managed images cannot be deleted — remove them from the server config first.

**Note:** When using `--json`, the `--force` flag is required.

**Examples:**

```bash
shed image delete myimage
shed image delete myimage --force
```

### shed image prune

Removes cached images that are not referenced by config or any existing shed.

```bash
shed image prune [flags]
```

| Flag | Short | Default | Description |
|------|-------|---------|-------------|
| `--force` | | `false` | Skip confirmation prompt |
| `--dry-run` | | `false` | List candidates without deleting |

A cached `.ext4` is preserved only if it is referenced by the current server config **and** its `.source` sidecar matches the current Docker ref (or the entry is a local path), or if it is referenced by an existing shed's metadata. Stale caches left behind after a config ref bump are reclaimed; see the [upgrade-and-reclaim cookbook](images.md#cookbook-upgrading-image-versions-and-reclaiming-disk) for the full flow.

**Note:** When using `--json`, the `--force` flag is required.

**Examples:**

```bash
shed image prune --dry-run       # Preview what would be removed
shed image prune                 # Interactive confirmation
shed image prune --force         # No confirmation
```

## System (Disk Usage)

### shed system df

Shows disk usage for shed servers: image cache, per-instance rootfs copies, kernel/initrd, and orphan sidecar files.

```bash
shed system df [flags]
```

| Flag | Short | Default | Description |
|------|-------|---------|-------------|
| `--all` | | `false` | Query every configured server (client-side fan-out) |
| `--server` | `-s` | (default server) | Target a specific server (global flag) |
| `--verbose` | `-v` | | Show per-image, per-shed, kernel, and orphan rows (global flag) |
| `--json` | | `false` | Emit machine-readable JSON (global flag) |

The rollup view groups files into four categories — images (including kernel and initrd), sheds (per-instance rootfs + console logs), orphans (stale `.lock`/`.tmp`/`.source` files), and totals. `-v` breaks each category into per-file rows.

Both **logical** (apparent) and **physical** (allocated) bytes are reported. Physical bytes come from `stat.Blocks * 512`. On filesystems that support extent sharing (APFS clonefile, ext4/xfs reflink, hardlinks), the same bytes may be attributed to multiple files — so summed physical totals may overcount actual on-disk usage. A note on every report calls this out.

**Multi-server (`--all`):** runs concurrently across every configured server. Offline or older servers (returning errors or 404) are reported inline without aborting; the command exits 0. In `--json` mode the response is `{servers: [{server_name, usage?, error?}, ...]}`.

**Examples:**

```bash
shed system df                 # Rollup for the active server
shed system df -v              # Per-image and per-shed detail
shed system df --json | jq     # Pipe the raw wire type to jq
shed system df --all           # Fan out across all configured servers
shed system df -s mini2 --json # Query one specific server in JSON
```

### shed system prune

Reclaims disk space by deleting unused images, stopped instances past an age threshold, and orphaned sidecar files. Optionally truncates VZ `console.log` files.

```bash
shed system prune [flags]
```

| Flag | Default | Description |
|------|---------|-------------|
| `--images` | `false` | Prune unreferenced cached images |
| `--instances` | `false` | Prune stopped instances older than `--until` |
| `--logs` | `false` | Truncate VZ `console.log` files (opt-in; no-op on Firecracker) |
| `--orphans` | `false` | Remove `.tmp`/`.source` sidecars whose parent rootfs is gone |
| `--dry-run` | `false` | Show candidates without mutating |
| `--force` | `false` | Skip confirmation prompt; required with `--json` for the execute path |
| `--until` | `72h` | Stopped-instance age threshold via `mtime(metadata.json)`. `0s` = any age |
| `--log-tail-bytes` | `0` | Console log truncation target (`0` = server default, 5 MiB) |
| `--all` | `false` | Prune on every configured server (client-side fan-out) |
| `--server` (`-s`) | (default server) | Target a specific server (global flag) |
| `--json` | `false` | Emit machine-readable JSON (global flag) |

**Scope semantics:** flags are additive. If none of `--images/--instances/--logs/--orphans` is set, the server applies its default scope: images + instances + orphans (NOT logs, which is always opt-in).

**Dry-run-first UX:** the command always previews candidates before mutating. Without `--force` the CLI prints the candidate table and prompts for confirmation. With `--force` it executes immediately. With `--dry-run` it previews and exits.

**Age filtering:** stopped instances are pruned only when `mtime(metadata.json) < now - --until`. The mtime snapshot is captured before the staleness re-check that `shed list` would otherwise trigger, so transient running→stopped transitions don't reset the clock. `--until 0s` disables the age gate.

**Orphan safety:** a sidecar (`.tmp`, `.source`, or `.lock`) is treated as an orphan only when its parent `{name}-rootfs.ext4` is absent **and** a non-blocking `flock()` on `{name}-rootfs.ext4.lock` succeeds (i.e., no conversion is in flight). `.tmp` files younger than 1 hour are skipped unconditionally. The canonical `.lock` file is never removed even when abandoned, to avoid the inode-reuse race documented in `shed image prune`.

**Console log truncation (VZ only):** when `--logs` is set, each surviving VZ shed's `console.log` is truncated to its last `--log-tail-bytes` in place (preserves inode so `vfkit`'s `O_APPEND` file descriptor keeps writing past the new EOF). Firecracker does not produce a per-instance console log, so `--logs` is a silent no-op for FC sheds.

**Freed bytes caveat:** reported `physical_bytes` come from `stat.Blocks * 512` and are attributed to each file — they reflect how much the filesystem said each file occupied, not how much disk is actually reclaimed. Files that share extents via clonefile/FICLONE clones or hardlinks may report bytes that won't be reclaimed until the last reference is removed. Compare `shed system df` before and after for true reclamation.

**JSON + destructive:** `--json` without `--force` blocks the execute path. `--dry-run --json` is always allowed.

**Examples:**

```bash
shed system prune --dry-run            # Preview default scope
shed system prune                      # Preview + interactive confirm
shed system prune --force              # Execute without prompt
shed system prune --instances --until 1h --force
shed system prune --logs --log-tail-bytes 1048576 --force
shed system prune --all --force        # Prune every configured server
shed system prune --json --dry-run     # Machine-readable preview
```

## Interactive Access

### shed console

Opens an interactive shell in a shed.

```bash
shed console <name>
```

Opens `/bin/bash` in the shed. If the shed is stopped, it will be started automatically.

### shed exec

Executes a command in a shed.

```bash
shed exec <name> <command...>
```

| Flag | Short | Default | Description |
|------|-------|---------|-------------|
| `--session` | `-S` | None | Run in tmux session context |

**Examples:**

```bash
shed exec codelens git status
shed exec codelens "cd /workspace && npm test"
shed exec codelens --session default git status
```

### shed attach

Attaches to a tmux session. Creates the session if it doesn't exist.

```bash
shed attach <name> [flags]
```

| Flag | Short | Default | Description |
|------|-------|---------|-------------|
| `--session` | `-S` | `default` | Session name |
| `--new` | | `false` | Force create new session |

**Examples:**

```bash
shed attach codelens
shed attach codelens --session debug
shed attach codelens --new --session experiment
```

Detach with `Ctrl-B D`.

## Session Management

### shed sessions

Lists tmux sessions.

```bash
shed sessions [shed-name] [flags]
```

| Flag | Short | Default | Description |
|------|-------|---------|-------------|
| `--all` | `-a` | `false` | List from all servers |

**Examples:**

```bash
shed sessions                  # All sessions on default server
shed sessions myproj           # Sessions in specific shed
shed sessions --all            # Across all servers
```

**Output:**

```
SHED        SESSION      STATUS      CREATED      WINDOWS
codelens    default      attached    2h ago       1
codelens    debug        detached    30m ago      2
```

### shed sessions kill

Terminates a tmux session.

```bash
shed sessions kill <shed-name> <session-name>
```

## Port Forwarding

### shed tunnels start

Starts SSH tunnels for port forwarding.

```bash
shed tunnels start <shed> [flags]
```

| Flag | Short | Default | Description |
|------|-------|---------|-------------|
| `--profile` | `-p` | None | Use tunnel profile |
| `--tunnel` | `-t` | None | Port mapping (local:remote) |
| `--background` | `-d` | `false` | Run in background |
| `--replace` | | `false` | Replace existing tunnel without prompting |

**Examples:**

```bash
shed tunnels start myproj -t 3000:3000
shed tunnels start myproj -t 3000:3000 -t 5432:5432
shed tunnels start myproj -p webdev -d
shed tunnels start myproj -p webdev --replace
```

### shed tunnels stop

Stops tunnels.

```bash
shed tunnels stop [shed] [flags]
```

| Flag | Default | Description |
|------|---------|-------------|
| `--all` | | `false` | Stop all tunnels |

### shed tunnels list

Lists active tunnels.

```bash
shed tunnels list [flags]
```

| Flag | Short | Default | Description |
|------|-------|---------|-------------|
| `--verbose` | `-v` | `false` | Show detailed info |

### shed tunnels config

Previews tunnel configuration.

```bash
shed tunnels config <shed> [flags]
```

| Flag | Short | Default | Description |
|------|-------|---------|-------------|
| `--profile` | `-p` | None | Profile to preview |
| `--tunnel` | `-t` | None | Additional tunnels to include |

## File Sync

### shed sync

Syncs local files to a shed.

```bash
shed sync <name> [flags]
```

| Flag | Short | Default | Description |
|------|-------|---------|-------------|
| `--profile` | `-p` | `default` | Sync profile to use |
| `--feature` | `-f` | None | Sync single feature |
| `--dry-run` | | `false` | Preview without syncing |

**Examples:**

```bash
shed sync myproj
shed sync myproj -p full
shed sync myproj -f devproxy
shed sync myproj --dry-run
```

## IDE Integration

### shed ssh-config

Generates or installs SSH config for IDE integration.

```bash
shed ssh-config [name] [flags]
```

| Flag | Short | Default | Description |
|------|-------|---------|-------------|
| `--all` | | `false` | Generate for all known sheds |
| `--install` | | `false` | Write to `~/.ssh/config` |
| `--dry-run` | | `false` | Show changes without applying |
| `--uninstall` | | `false` | Remove entries |

**Examples:**

```bash
shed ssh-config codelens              # Print config for one shed
shed ssh-config --all --install --dry-run  # Preview changes
shed ssh-config --all --install       # Apply changes
shed ssh-config --uninstall           # Remove managed block
```

## Server Commands

These commands are part of `shed-server`, the server daemon binary.

### shed-server setup

Sets up Firecracker infrastructure on a Linux host. Idempotent and safe to re-run.

```bash
sudo shed-server setup
```

| Step | Description |
|------|-------------|
| KVM check | Verifies `/dev/kvm` is available |
| Docker check | Verifies Docker CE is installed |
| Firecracker | Downloads and installs Firecracker and jailer binaries |
| Kernel | Downloads fallback CI kernel if none exists |
| Directories | Creates `/var/lib/shed/firecracker/` and `/var/run/shed/firecracker/` |
| Bridge | Creates `shed-br0` bridge network |
| NAT | Enables IP forwarding and iptables masquerade rules |
| Capabilities | Sets `CAP_NET_ADMIN` on `shed-server` and `firecracker` binaries |

Linux only. Not available on macOS.

### shed-server pull-images

Pre-caches VM images by pulling Docker refs and converting to ext4.

```bash
shed-server pull-images [flags]
```

| Flag | Default | Description |
|------|---------|-------------|
| `--variant` | All | Pull a specific image variant only |
| `--config` | Auto-detect | Path to server config file |

**Examples:**

```bash
sudo shed-server pull-images                  # Pull all configured variants
sudo shed-server pull-images --variant base   # Pull only the base variant
```

Works on both macOS (VZ) and Linux (Firecracker). Uses the images configured in `server.yaml` for the active backend. When `base_rootfs` shares a Docker ref with any `images:` entry, the `_base` cache is hardlinked to the matching variant rather than being re-pulled — `shed create` without `--image` is then immediate. See the [on-disk layout](images.md#on-disk-layout) and [upgrade cookbook](images.md#cookbook-upgrading-image-versions-and-reclaiming-disk) in the images reference.

### shed-server install

Installs shed-server as a systemd service. Alternative to the deb package for manual binary installs.

```bash
sudo shed-server install
```

Creates a systemd unit file at `/etc/systemd/system/shed-server.service` and enables it. Does not start the service.

### shed-server uninstall

Removes the shed-server systemd service.

```bash
sudo shed-server uninstall
```

Stops the service, disables it, and removes the unit file.

## Utility

### shed version

Shows version information.

```bash
shed version [flags]
```

| Flag | Short | Default | Description |
|------|-------|---------|-------------|
| `--verbose` | `-v` | `false` | Show full version info including dependencies |
