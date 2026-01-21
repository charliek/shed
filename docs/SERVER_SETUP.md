# Shed Server Setup Guide

This guide covers installing and configuring `shed-server` on a Linux server.

## Prerequisites

- Linux server (Ubuntu 20.04+, Debian 11+, or RHEL/Fedora)
- Docker installed and running
- Tailscale (or other private network) configured
- Go 1.24+ (for building from source)

## Installation

### Option 1: Build from Source

```bash
# Clone the repository
git clone https://github.com/charliek/shed.git
cd shed

# Build the server binary
make build

# Copy to system location
sudo cp bin/shed-server /usr/local/bin/
```

### Option 2: Download Release Binary

```bash
# Download the latest release (adjust version as needed)
curl -L https://github.com/charliek/shed/releases/download/v1.0.0/shed-server-linux-amd64 -o shed-server
chmod +x shed-server
sudo mv shed-server /usr/local/bin/
```

## Initial Setup

### 1. Create Required Directories

```bash
# Create configuration and data directories
sudo mkdir -p /etc/shed
sudo chown $USER:$USER /etc/shed

# Create config directory for user
mkdir -p ~/.config/shed
```

### 2. Build the Base Docker Image

The shed containers require a base image with development tools pre-installed:

```bash
# From the shed repository
./scripts/build-image.sh
```

This creates the `shed-base:latest` image with:
- Ubuntu 22.04 base
- Git, curl, wget, vim, tmux
- Go 1.24
- Node.js 20
- Python 3 with pip
- Claude Code and OpenCode CLI tools

### 3. Create Server Configuration

Create `/etc/shed/server.yaml` or `~/.config/shed/server.yaml`:

```yaml
# Server identification
name: my-server

# Network ports
http_port: 8080
ssh_port: 2222

# Default container image
default_image: shed-base:latest

# Credential mounts (bind mounts into containers)
credentials:
  git-ssh:
    source: /home/youruser/.ssh
    target: /root/.ssh
    readonly: true
  git-config:
    source: /home/youruser/.gitconfig
    target: /root/.gitconfig
    readonly: true

# Environment file for API keys
env_file: /home/youruser/.shed/env

# Logging level (debug, info, warn, error)
log_level: info
```

### 4. Create Environment File

Create the environment file specified in `env_file` (e.g., `~/.shed/env`):

```bash
# Claude API key for Claude Code
ANTHROPIC_API_KEY=sk-ant-...

# OpenAI key for OpenCode (optional)
OPENAI_API_KEY=sk-...

# GitHub token for private repos (optional)
GITHUB_TOKEN=ghp_...
```

**Important**: Ensure this file has restricted permissions:

```bash
chmod 600 ~/.shed/env
```

### 5. Start the Server

#### Manual Start (for testing)

```bash
shed-server serve
```

#### Systemd Service (recommended)

Install as a systemd service:

```bash
shed-server install
```

This creates and enables `/etc/systemd/system/shed-server.service`.

To uninstall:

```bash
shed-server uninstall
```

Manual systemd commands:

```bash
# Start the service
sudo systemctl start shed-server

# Enable on boot
sudo systemctl enable shed-server

# Check status
sudo systemctl status shed-server

# View logs
journalctl -u shed-server -f
```

## Configuration Reference

### Server Configuration Fields

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `name` | string | `shed-server` | Server identifier shown in client |
| `http_port` | int | `8080` | HTTP API port |
| `ssh_port` | int | `2222` | SSH server port |
| `default_image` | string | `shed-base:latest` | Default Docker image for sheds |
| `credentials` | map | `{}` | Bind mounts for credentials |
| `env_file` | string | - | Path to environment variables file |
| `log_level` | string | `info` | Logging verbosity |

### Credential Mounts

Each credential entry creates a bind mount in all shed containers:

```yaml
credentials:
  name:
    source: /host/path      # Path on the host
    target: /container/path  # Path in container
    readonly: true           # Optional, default false
```

Common credential mounts:

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

## Firewall Configuration

### With Tailscale (recommended)

If using Tailscale, the shed ports are only accessible within your Tailscale network. No additional firewall configuration is needed.

### Without Tailscale

If exposing to a broader network, configure firewall rules:

```bash
# UFW (Ubuntu)
sudo ufw allow from 192.168.0.0/16 to any port 8080
sudo ufw allow from 192.168.0.0/16 to any port 2222

# firewalld (RHEL/Fedora)
sudo firewall-cmd --permanent --add-rich-rule='rule family="ipv4" source address="192.168.0.0/16" port port="8080" protocol="tcp" accept'
sudo firewall-cmd --permanent --add-rich-rule='rule family="ipv4" source address="192.168.0.0/16" port port="2222" protocol="tcp" accept'
sudo firewall-cmd --reload
```

## Troubleshooting

### Server Won't Start

1. **Check Docker is running:**
   ```bash
   docker info
   ```

2. **Check /etc/shed directory exists and is writable:**
   ```bash
   ls -la /etc/shed
   ```

3. **Check configuration file syntax:**
   ```bash
   shed-server serve -c /path/to/config.yaml
   ```

4. **Check port availability:**
   ```bash
   ss -tlnp | grep -E '8080|2222'
   ```

### SSH Connection Issues

1. **Verify SSH server is running:**
   ```bash
   nc -vz localhost 2222
   ```

2. **Check host key was generated:**
   ```bash
   ls -la /etc/shed/host_key
   ```

3. **Test SSH connection:**
   ```bash
   ssh -p 2222 shed-name@localhost -v
   ```

### Container Creation Fails

1. **Check Docker image exists:**
   ```bash
   docker images | grep shed-base
   ```

2. **Check Docker permissions:**
   ```bash
   docker run --rm hello-world
   ```

3. **Check server logs:**
   ```bash
   journalctl -u shed-server -n 50
   ```

## Updating

To update shed-server:

```bash
# Stop the service
sudo systemctl stop shed-server

# Build/download new binary
make build
sudo cp bin/shed-server /usr/local/bin/

# Start the service
sudo systemctl start shed-server
```

## Uninstalling

```bash
# Stop and disable service
sudo systemctl stop shed-server
sudo systemctl disable shed-server

# Uninstall service file
shed-server uninstall

# Remove binary
sudo rm /usr/local/bin/shed-server

# Remove configuration (optional)
sudo rm -rf /etc/shed

# Remove Docker image (optional)
docker rmi shed-base:latest
```
