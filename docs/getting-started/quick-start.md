# Quick Start

Get up and running with Shed in a few minutes.

## Prerequisites

- **macOS Apple Silicon** — Homebrew (recommended) or Go 1.24+ for source builds
- **Linux with KVM** — deb package (recommended) or Go 1.24+ for source builds
- Docker (for VM image management)
- Tailscale (or other private network) if connecting to remote servers

## Install

### Homebrew (macOS, Recommended)

```bash
brew install charliek/tap/shed
```

This installs both `shed` (CLI) and `shed-server`, generates a default server config with the VZ backend, codesigns the server binary, and sets up a launchd service.

For credential brokering (SSH agent forwarding, AWS credentials, Docker registry auth), also install the host agent:

```bash
brew install charliek/tap/shed-host-agent
```

Edit the server config at `$(brew --prefix)/etc/shed/server.yaml` to configure credentials and extensions, then start the services:

```bash
brew services start shed
brew services start shed-host-agent  # if installed
```

See [VZ Setup](vz-setup.md) for the full macOS setup guide.

### deb Package (Linux, Recommended)

Download and install the `.deb` from the [latest release](https://github.com/charliek/shed/releases):

```bash
wget https://github.com/charliek/shed/releases/download/v{version}/shed_{version}_amd64.deb
sudo dpkg -i shed_{version}_amd64.deb
```

Replace `{version}` with the version you want to install (e.g., `0.3.2`).

This installs `shed` (CLI) and `shed-server`, generates a default Firecracker server config, and sets up a systemd service.

Complete the Firecracker infrastructure setup:

```bash
sudo shed-server setup
sudo shed-server pull-images
sudo systemctl start shed-server
```

See [Firecracker Setup](fc-setup.md) for the full Linux setup guide.

### Build from Source

```bash
git clone https://github.com/charliek/shed.git
cd shed
make build

# Or install the CLI only
go install github.com/charliek/shed/cmd/shed@latest
```

See [VZ Setup](vz-setup.md) or [Firecracker Setup](fc-setup.md) for server setup instructions when building from source.

## Add a Server

Register a server that has `shed-server` running:

```bash
shed server add my-server.tailnet.ts.net --name my-server
```

This connects to the server, retrieves its SSH host key, and saves the configuration.

## Create a Shed

```bash
# Create an empty shed
shed create my-project

# Or clone a repository
shed create my-project --repo git@github.com:user/repo.git

# Or mount a local directory as the workspace
shed create my-project --local-dir ~/projects/my-project
```

## Connect

### Direct Shell

```bash
shed console my-project
```

Opens a bash shell in the VM. Exits when you disconnect.

### Persistent Session

```bash
shed attach my-project
```

Opens a tmux session that persists after you disconnect. Detach with `Ctrl-B D` and reconnect later with the same command.

## IDE Integration

Generate SSH config entries for VS Code or Cursor:

```bash
# Preview the config
shed ssh-config my-project

# Install to ~/.ssh/config
shed ssh-config --all --install
```

Then connect using VS Code Remote-SSH to `shed-my-project`.

## Common Workflows

### Run a coding agent

```bash
# Create a shed and attach to a persistent session
shed create myproj --repo user/repo
shed attach myproj

# Inside the session, start Claude Code
claude

# Detach with Ctrl-B D - the agent keeps running
# Later, reattach to see progress
shed attach myproj
```

### Multiple sessions

```bash
# Attach to a named session
shed attach myproj --session debug

# List all sessions
shed sessions --all
```

### Port forwarding

```bash
# Start tunnels for web development
shed tunnels start myproj -t 3000:3000

# Run in background
shed tunnels start myproj -t 3000:3000 -d
```

## Next Steps

- [VZ Setup (macOS)](vz-setup.md) - Set up the VZ backend on Apple Silicon
- [Firecracker Setup (Linux)](fc-setup.md) - Set up the Firecracker backend
- [CLI Reference](../reference/cli.md) - All available commands
- [Configuration](../reference/configuration.md) - Client and server config options
- [Extensions](../reference/extensions.md) - Credential brokering with the experimental image variant
- [Tunnels](../reference/tunnels.md) - Port forwarding configuration
