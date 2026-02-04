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
default_image: shed-base:latest

credentials:
  git-ssh:
    source: ~/.ssh
    target: /root/.ssh
    readonly: true

  git-config:
    source: ~/.gitconfig
    target: /root/.gitconfig
    readonly: true

  claude:
    source: ~/.claude
    target: /root/.claude
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
| `backend` | string | `docker` | Backend type: `docker` or `firecracker` |
| `default_image` | string | `shed-base:latest` | Default Docker image for sheds |
| `credentials` | map | `{}` | Credentials to mount/copy into sheds |
| `env_file` | string | - | Path to environment variables file |
| `log_level` | string | `info` | Logging level (debug, info, warn, error) |
| `firecracker` | object | - | Firecracker-specific configuration (see below) |

### Credentials

Credentials are made available to sheds. The method depends on the backend:

- **Docker**: Credentials are bind-mounted (live sync with host)
- **Firecracker**: Credentials are copied at create/start time (no live sync)

```yaml
credentials:
  name:
    source: /host/path      # Path on the host (~ supported)
    target: /container/path  # Path inside shed
    readonly: true           # Optional, default false
```

**Note:** For Firecracker, changes to credentials on the host after shed start won't be reflected in the VM until the next restart.

**Common credential mounts:**

```yaml
credentials:
  # SSH keys for git
  git-ssh:
    source: ~/.ssh
    target: /root/.ssh
    readonly: true

  # Git configuration
  git-config:
    source: ~/.gitconfig
    target: /root/.gitconfig
    readonly: true

  # Claude Code config (needs write for token refresh)
  claude:
    source: ~/.claude
    target: /root/.claude
    readonly: false

  # GitHub CLI
  gh:
    source: ~/.config/gh
    target: /root/.config/gh
    readonly: true

  # AWS credentials
  aws:
    source: ~/.aws
    target: /root/.aws
    readonly: true

  # GCP credentials
  gcloud:
    source: ~/.config/gcloud
    target: /root/.config/gcloud
    readonly: true
```

## Firecracker Configuration

When using `backend: firecracker`, configure the Firecracker-specific settings:

```yaml
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
  start_timeout: 120s
  stop_timeout: 10s
  bridge_name: shed-br0
  bridge_cidr: 172.30.0.1/24
  tap_prefix: shed-tap
```

### Firecracker Fields

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `kernel_path` | string | - | Path to Linux kernel image |
| `base_rootfs` | string | - | Path to base rootfs ext4 image |
| `instance_dir` | string | - | Directory for VM instances |
| `socket_dir` | string | - | Directory for API/vsock sockets |
| `default_cpus` | int | `2` | Default vCPUs per VM |
| `default_memory_mb` | int | `4096` | Default memory per VM (MB) |
| `start_timeout` | duration | `30s` | VM startup timeout |
| `stop_timeout` | duration | `10s` | Graceful shutdown timeout |
| `bridge_name` | string | `shed-br0` | Linux bridge name |
| `bridge_cidr` | string | `172.30.0.1/24` | Bridge network CIDR |
| `tap_prefix` | string | `shed-tap` | TAP device name prefix |

See [Firecracker Installation](../firecracker_install.md) for setup details.

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
