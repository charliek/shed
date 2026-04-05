# Quick Start

Get up and running with Shed in a few minutes.

## Prerequisites

- Go 1.24+ (for building from source)
- A server running `shed-server` with one of:
    - **macOS Apple Silicon** — VZ backend (Virtualization.framework via vfkit)
    - **Linux with KVM** — Firecracker backend (microVMs)
- Tailscale (or other private network) connecting your machines

The `detect` backend (the default) automatically selects VZ on macOS and Firecracker on Linux, so you typically don't need to specify a backend.

## Install the CLI

```bash
# Build from source
git clone https://github.com/charliek/shed.git
cd shed
make build

# Or install directly
go install github.com/charliek/shed/cmd/shed@latest
```

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
