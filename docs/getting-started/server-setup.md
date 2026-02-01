# Server Setup

This guide covers installing and configuring `shed-server` on a Linux server.

## Prerequisites

- Linux server (Ubuntu 20.04+, Debian 11+, or RHEL/Fedora)
- Docker installed and running
- Tailscale (or other private network) configured
- Go 1.24+ (for building from source)

## Installation

### Build from Source

```bash
git clone https://github.com/charliek/shed.git
cd shed
make build
sudo cp bin/shed-server /usr/local/bin/
```

### Download Release Binary

Pre-built binaries will be available in future releases. For now, build from source using the instructions above.

## Initial Setup

### 1. Create Directories

```bash
sudo mkdir -p /etc/shed
sudo chown $USER:$USER /etc/shed
mkdir -p ~/.config/shed ~/.shed
```

### 2. Build the Base Docker Image

The shed containers require a base image with development tools:

```bash
./scripts/build-image.sh
```

This creates `shed-base:latest` with Ubuntu 22.04, Git, Go, Node.js, Python, and AI tools pre-installed.

### 3. Create Server Configuration

Create `/etc/shed/server.yaml` or `~/.config/shed/server.yaml`:

```yaml
name: my-server
http_port: 8080
ssh_port: 2222
default_image: shed-base:latest

credentials:
  # Git credentials
  git_ssh:
    source: ~/.ssh
    target: /mnt/ssh-host
    readonly: true
  git_config:
    source: ~/.gitconfig
    target: /root/.gitconfig
    readonly: true

  # Claude Code - container-specific credentials
  claude:
    source: ~/.shed/mounts/claude
    target: /root/.claude
    readonly: false

  # OpenCode - data, state, and cache directories
  opencode_data:
    source: ~/.shed/mounts/opencode/share
    target: /root/.local/share/opencode
    readonly: false
  opencode_state:
    source: ~/.shed/mounts/opencode/state
    target: /root/.local/state/opencode
    readonly: false
  opencode_cache:
    source: ~/.shed/mounts/opencode/cache
    target: /root/.cache/opencode
    readonly: false

  # GitHub CLI
  gh:
    source: ~/.shed/mounts/gh
    target: /root/.config/gh
    readonly: false

env_file: ~/.shed/env
log_level: info
```

### 4. Create Environment File

Create the environment file for API keys (`~/.shed/env`):

```bash
ANTHROPIC_API_KEY=sk-ant-...
OPENAI_API_KEY=sk-...
GITHUB_TOKEN=ghp_...
```

Set restricted permissions:

```bash
chmod 600 ~/.shed/env
```

### 5. Start the Server

**Manual start (for testing):**

```bash
shed-server serve
```

**Systemd service (recommended):**

```bash
shed-server install
```

This creates and enables `/etc/systemd/system/shed-server.service`.

## Configuration Reference

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `name` | string | `shed-server` | Server identifier |
| `http_port` | int | `8080` | HTTP API port |
| `ssh_port` | int | `2222` | SSH server port |
| `default_image` | string | `shed-base:latest` | Default Docker image |
| `credentials` | map | `{}` | Bind mounts for credentials |
| `env_file` | string | - | Path to environment variables file |
| `log_level` | string | `info` | Logging verbosity |

## Credential Mounts

Credentials are bind-mounted into all shed containers. See [Configuration - Credential Mounts](../reference/configuration.md#credential-mounts) for detailed examples.

## Firewall Configuration

### With Tailscale

No additional firewall configuration needed - ports are only accessible within your Tailscale network.

### Without Tailscale

Restrict access to trusted networks:

```bash
# UFW (Ubuntu)
sudo ufw allow from 192.168.0.0/16 to any port 8080
sudo ufw allow from 192.168.0.0/16 to any port 2222
```

## Systemd Commands

```bash
sudo systemctl start shed-server
sudo systemctl enable shed-server
sudo systemctl status shed-server
journalctl -u shed-server -f
```

## Updating

```bash
sudo systemctl stop shed-server
make build
sudo cp bin/shed-server /usr/local/bin/
sudo systemctl start shed-server
```

## Uninstalling

```bash
sudo systemctl stop shed-server
sudo systemctl disable shed-server
shed-server uninstall
sudo rm /usr/local/bin/shed-server
```
