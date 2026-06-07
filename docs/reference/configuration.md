# Configuration

Shed uses YAML configuration files for both client and server settings.

## Client Configuration

**Location:** `~/.shed/config.yaml`

The client configuration stores server connections and cached shed locations.

```yaml
servers:
  mini-desktop:
    host: mini-desktop.tailnet.ts.net
    http_port: 8080
    ssh_port: 2222
    added_at: "2026-01-20T10:00:00Z"

  cloud-vps:
    host: vps.tailnet.ts.net
    http_port: 8080
    ssh_port: 2222
    added_at: "2026-01-19T14:00:00Z"

default_server: mini-desktop

# Timeout for shed create and start operations
create_timeout: 30m

sheds:
  codelens:
    server: mini-desktop
    status: running
    updated_at: "2026-01-20T10:30:00Z"
```

### Client Fields

| Field | Type | Description |
|-------|------|-------------|
| `servers` | map | Configured server connections |
| `servers.<name>.host` | string | Server hostname or IP |
| `servers.<name>.http_port` | int | HTTP API port |
| `servers.<name>.ssh_port` | int | SSH server port |
| `default_server` | string | Default server for commands |
| `sheds` | map | Cached shed locations |
| `create_timeout` | duration | Timeout for create/start operations (default: `10m`) |

## Server Configuration

**Locations (checked in order):**

1. `./server.yaml`
2. `~/.config/shed/server.yaml`
3. `/etc/shed/server.yaml`

```yaml
name: mini-desktop
http_port: 8080
ssh_port: 2222

mounts:
  claude:
    source: ~/.claude
    target: /home/shed/.claude
    readonly: false

env_file: ~/.shed/env
log_level: info
```

### Server Fields

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `name` | string | `shed-server` | Server identifier |
| `http_port` | int | `8080` | HTTP API port |
| `ssh_port` | int | `2222` | SSH server port |
| `default_backend` | string | `detect` | Backend to use when none is specified (`detect`, `firecracker`, `vz`). `detect` auto-selects based on platform: `vz` on macOS, `firecracker` on Linux. |
| `mounts` | map | `{}` | Host directories to mount into sheds (formerly `credentials`) |
| `env_file` | string | - | Path to environment variables file |
| `log_level` | string | `info` | Logging level (debug, info, warn, error) |
| `extensions` | object | `{}` | Extensions to activate in VMs (see [Extensions](extensions.md)) |
| `git` | object | - | Git behaviour for in-VM clones (see [Git](#git)) |
| `firecracker` | object | - | Firecracker-specific configuration (see below) |
| `vz` | object | - | VZ-specific configuration (see below) |

**Note:** Only VM backends are supported. Firecracker is available on Linux. VZ is available on macOS Apple Silicon (arm64). The `detect` backend auto-selects based on platform.

### Mounts

Mounts are directories from the host that are shared with sheds. The method depends on the backend:

- **Firecracker**: Mounted via 9P over the TAP bridge network.
- **VZ**: Mounted via VirtioFS.

Both mechanisms provide live filesystem sharing -- changes on either side are immediately visible to the other.

!!! note "Renamed from `credentials`"
    This section was previously named `credentials`. The `mounts` key has the identical shape. The deprecated `credentials` key still works as a fallback when `mounts` is absent, but new configs should use `mounts`.

```yaml
mounts:
  name:
    source: /host/path      # Path on the host (~ supported, must be a directory)
    target: /container/path  # Path inside shed
    readonly: true           # Optional, default false
```

**Mount sources must be directories.** Single-file mounts are not supported. For individual config files like `.gitconfig`, use [`shed sync`](sync.md) to push them as dotfiles. For SSH-based git authentication, use the shed-extensions SSH agent forwarding instead of mounting `~/.ssh`.

**Missing sources:** If a mount's source path does not exist on the host, it is skipped with a log warning. Create the source directory on the host before starting the shed.

**Common mounts:**

```yaml
mounts:
  # Claude Code config (needs write for token refresh)
  claude:
    source: ~/.claude
    target: /home/shed/.claude
    readonly: false

  # GitHub CLI
  gh:
    source: ~/.config/gh
    target: /home/shed/.config/gh
    readonly: true

  # AWS credentials
  aws:
    source: ~/.aws
    target: /home/shed/.aws
    readonly: true

  # GCP credentials
  gcloud:
    source: ~/.config/gcloud
    target: /home/shed/.config/gcloud
    readonly: true
```

### Exclude Patterns

The mount config accepts an `exclude` field with glob patterns. This field is currently accepted but has no effect on VM backends -- VirtioFS and 9P mount entire directories. Exclude patterns are used by [`shed sync`](sync.md) path mappings. The field is retained for forward compatibility.

```yaml
mounts:
  claude:
    source: ~/.claude
    target: /home/shed/.claude
    readonly: false
    exclude:
      - "*.db"
      - "*.db-shm"
      - "*.db-wal"
      - "log/*"
      - "storage/*"
```

## Extensions

Extensions are activated per-VM by listing their namespace names. The agent reads manifests from `/etc/shed-extensions.d/` in the VM image and enables the matching systemd units at startup. When `extensions` is omitted, no extensions are activated.

```yaml
extensions:
  enabled:
    - ssh-agent
    - aws-credentials
    - docker-credentials
```

See [Extensions](extensions.md) for the full guide on the message bus, manifests, SDK, and health reporting.

## Git

When a shed is created with a `repo` whose URL uses SSH (e.g., `git@github.com:org/repo.git` or `ssh://git@host/path`), the server seeds the in-VM `~/.ssh/known_hosts` before invoking `git clone`. Without this, OpenSSH defaults to `StrictHostKeyChecking=yes` and rejects the connection with `Host key verification failed`. The `owner/repo` shorthand is expanded to `git@github.com:owner/repo.git`, so it goes through the same SSH path.

GitHub's published host keys (ED25519, ECDSA, RSA) are baked into the server binary and are always included. To trust additional hosts (GitLab, GitHub Enterprise, self-hosted Gitea, etc.), add their `known_hosts` lines to `git.extra_known_hosts`:

```yaml
git:
  extra_known_hosts:
    - "gitlab.com ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAfuCHKVTjquxvt6CM6tdG4SLp1Btn/nOeHHE5UOzRdf"
    - "my-gitea.internal ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAI..."
```

Each entry must be a syntactically valid `known_hosts` line: `<host> <key-type> <base64-key>`. The server validates this at startup and refuses to start if any entry is malformed.

**Generating entries:** Run `ssh-keyscan <host>` on a trusted machine and paste the output. For example:

```bash
ssh-keyscan gitlab.com 2>/dev/null
```

The `git.extra_known_hosts` list is *additive* — it extends the built-in defaults, never replaces them. Only SSH-form clone URLs (`git@host:path`, `ssh://...`) consult `known_hosts`; HTTPS clones skip this step entirely.

**Key rotation:** If GitHub or another host rotates its keys, you can extend the trust list via `extra_known_hosts` immediately without waiting for a shed release. The default GitHub keys ship with the server binary and are updated in releases.

## Firecracker Configuration

When enabling Firecracker, configure the Firecracker-specific settings:

```yaml
default_backend: firecracker

firecracker:
  default_image: ghcr.io/charliek/shed-fc-full:v{version}
  image_aliases:
    base: ghcr.io/charliek/shed-fc-base:v{version}
    extensions: ghcr.io/charliek/shed-fc-extensions:v{version}
    full: ghcr.io/charliek/shed-fc-full:v{version}
  pull_policy: missing
  images_dir: /var/lib/shed/firecracker/images
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

Replace `{version}` with the version matching your `shed` binary — run `shed version` to check.

### Firecracker Fields

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `kernel_path` | string | `""` | Optional. Path to a Linux kernel image (auto-populated by published-image pulls). The Phase B initramfs prefers the kernel embedded in the image blob (`{images_dir}/blobs/sha256/<digest>/kernel`); this path is the fallback for legacy blobs that lack an embedded kernel. |
| `default_image` | string | `""` | Path or Docker ref used for new sheds when `shed create` runs without `--image`. Empty is fine if every create passes `--image`; otherwise `shed create` errors with `INVALID_REQUEST: no --image specified and no default_image configured`. |
| `image_aliases` | map | - | Optional short alias → Docker ref (or path) map for `shed create --image <alias>`. Listings always show the resolved ref, not the alias. |
| `pull_policy` | string | `missing` | `missing` (use cache, pull if absent), `always` (always pull), or `never` (error if not cached). Ignored for local-path images. |
| `images_dir` | string | `/var/lib/shed/firecracker/images` | Directory for the content-addressed blob store and the ref→digest index |
| `upper_size_default` | string | `5G` | Default logical size of the per-shed writable overlay upper layer when `shed create --upper-size` is omitted. Validated to the range 1–100 GiB. |
| `instance_dir` | string | - | Directory for VM instances |
| `socket_dir` | string | - | Directory for API/vsock sockets |
| `default_cpus` | int | `2` | Default vCPUs per VM |
| `default_memory_mb` | int | `4096` | Default memory per VM (MB) |
| `default_disk_gb` | int | `20` | Default disk size per VM (GB) |
| `vsock_base_cid` | int | `100` | Starting CID for vsock guest addressing |
| `console_port` | int | `1024` | Vsock port for VM console I/O |
| `notify_port` | int | `1026` | Vsock port for the message channel (health checks, plugins) |
| `start_timeout` | duration | `30s` | VM startup timeout |
| `stop_timeout` | duration | `10s` | Graceful shutdown timeout |
| `bridge_name` | string | `shed-br0` | Linux bridge name |
| `bridge_cidr` | string | `172.30.0.1/24` | Bridge network CIDR |
| `tap_prefix` | string | `shed-tap` | TAP device name prefix |

Path-existence validation only fires when **all** configured image sources
are local paths. When `default_image` (or any `image_aliases` entry) is a
Docker ref, the path-existence check is skipped because the file is created
on first pull. `kernel_path` is still required to point at a real file when
set non-empty.

See [Firecracker Setup](../getting-started/fc-setup.md) for setup details.

## VZ Configuration

When enabling the VZ backend on macOS Apple Silicon, configure the VZ-specific settings:

Image values in `default_image` and `image_aliases` can be either ext4 file paths or Docker image references. Docker refs are auto-pulled and converted on first use.

```yaml
default_backend: vz

vz:
  vfkit_path: vfkit
  kernel_path: ~/Library/Application Support/shed/vz/vmlinux
  initrd_path: ~/Library/Application Support/shed/vz/initrd.img
  default_image: ghcr.io/charliek/shed-vz-full:v{version}
  image_aliases:
    base: ghcr.io/charliek/shed-vz-base:v{version}
    extensions: ghcr.io/charliek/shed-vz-extensions:v{version}
    full: ghcr.io/charliek/shed-vz-full:v{version}
  pull_policy: missing
  images_dir: ~/Library/Application Support/shed/vz/
  instance_dir: ~/Library/Application Support/shed/vz/instances
  socket_dir: ~/.shed/vz/sockets
  default_cpus: 2
  default_memory_mb: 4096
  default_disk_gb: 20
  console_port: 1024
  notify_port: 1026
  tcp_proxy_port: 1028
  start_timeout: 60s
  stop_timeout: 10s
```

### VZ Fields

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `vfkit_path` | string | `vfkit` | Path to vfkit binary |
| `kernel_path` | string | `""` | Optional. Path to a decompressed Linux kernel (auto-populated by published-image pulls). Phase B prefers the kernel embedded in the image blob; this is the fallback for legacy blobs that lack an embedded kernel. |
| `initrd_path` | string | `""` | Optional. Path to an initial RAM disk image. The shed-overlay initramfs lives inside the image blob, so this field is only consulted for legacy blobs. |
| `default_image` | string | `""` | Path or Docker ref used for new sheds when `shed create` runs without `--image`. Empty is fine if every create passes `--image`; otherwise `shed create` errors with `INVALID_REQUEST: no --image specified and no default_image configured`. |
| `image_aliases` | map | - | Optional short alias → Docker ref (or path) map for `shed create --image <alias>` (see [Images](images.md)). Listings always show the resolved ref. |
| `pull_policy` | string | `missing` | `missing` (use cache, pull if absent), `always` (always pull), or `never` (error if not cached). Ignored for local-path images. |
| `images_dir` | string | `~/Library/Application Support/shed/vz/` | Directory for the content-addressed blob store and the ref→digest index |
| `upper_size_default` | string | `5G` | Default logical size of the per-shed writable overlay upper layer when `shed create --upper-size` is omitted. Validated to the range 1–100 GiB. |
| `instance_dir` | string | - | Directory for VM instances |
| `socket_dir` | string | - | Directory for vsock Unix sockets (must not contain spaces) |
| `default_cpus` | int | `2` | Default vCPUs per VM |
| `default_memory_mb` | int | `4096` | Default memory per VM (MB) |
| `default_disk_gb` | int | `20` | Default disk size per VM (GB) |
| `console_port` | int | `1024` | Vsock port for VM console I/O |
| `notify_port` | int | `1026` | Vsock port for the message channel (health checks, plugins) |
| `tcp_proxy_port` | int | `1028` | Vsock port for TCP proxy (used by DialService for tunnels and Connect API) |
| `start_timeout` | duration | `60s` | VM startup timeout |
| `stop_timeout` | duration | `10s` | Graceful shutdown timeout |

See [VZ Setup](../getting-started/vz-setup.md) for setup details.

## Environment File

**Location:** As configured in `env_file` (typically `~/.shed/env`)

Environment variables injected into all containers:

```bash
ANTHROPIC_API_KEY=sk-ant-...
OPENAI_API_KEY=sk-...
GITHUB_TOKEN=ghp_...
```

Set restricted permissions:

```bash
chmod 600 ~/.shed/env
```

## SSH Known Hosts

**Location:** `~/.shed/known_hosts`

Stores SSH host keys for shed servers. Populated automatically when running `shed server add`.

## Sync Configuration

See [File Sync](sync.md) for sync configuration.

## Tunnel Configuration

See [Tunnels](tunnels.md) for tunnel configuration.
