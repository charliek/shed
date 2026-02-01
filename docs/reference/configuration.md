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
| `default_image` | string | `shed-base:latest` | Default Docker image for sheds |
| `credentials` | map | `{}` | Bind mounts for credentials |
| `env_file` | string | - | Path to environment variables file |
| `log_level` | string | `info` | Logging level (debug, info, warn, error) |

### Credential Mounts

Credentials are bind-mounted into all shed containers:

```yaml
credentials:
  name:
    source: /host/path      # Path on the host (~ supported)
    target: /container/path  # Path inside container
    readonly: true           # Optional, default false
```

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
