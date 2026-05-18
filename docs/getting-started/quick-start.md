# Quick Start

Get up and running with Shed in a few minutes.

## Prerequisites

- **macOS Apple Silicon** — Homebrew (recommended) or Go 1.24+ for source builds
- **Linux with KVM** — deb package (recommended) or Go 1.24+ for source builds
- Docker (for VM image management)
- Tailscale (or other private network) if connecting to remote servers

## Install

Shed has two install paths:

- **Published packages (Homebrew on macOS, deb on Linux)** are the
  easiest path. They install pre-built binaries and pull pre-built VM
  images from `ghcr.io`. Pick this if you want shed running quickly and
  don't need to modify the rootfs / kernel / initramfs.
- **Build from source** is for contributors and bleeding-edge use. It
  builds binaries locally from the repo and (optionally) builds the VM
  rootfs images from the local `vz/` and `firecracker/` Dockerfiles.
  Pick this if you're hacking on shed itself or want to run an unreleased
  commit.

Upgrading from v0.4.x? See the [Upgrade Guide](../UPGRADE.md) — the
v0.5.0 image store is not backwards-compatible.

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
# Find the latest version at https://github.com/charliek/shed/releases
# or, with the gh CLI:
VERSION=$(gh release view --repo charliek/shed --json tagName -q .tagName | sed 's/^v//')

wget https://github.com/charliek/shed/releases/download/v${VERSION}/shed-server_${VERSION}_amd64.deb
sudo dpkg -i shed-server_${VERSION}_amd64.deb
```

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

`make build` produces `bin/shed`, `bin/shed-server`, and `bin/shed-agent`.
That's only step one of a source install — `shed-server` still needs a
server config, VM images, and (on Linux) bridge networking before it can
launch a VM.

Continue with the backend-specific setup guide:

- macOS Apple Silicon: [VZ Setup — Build from Source](vz-setup.md#build-from-source-alternative)
- Linux with KVM: [Firecracker Setup — Build from Source](fc-setup.md#build-from-source-alternative)

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

Once you have a few sheds, `shed system df` shows what's on disk and `shed system prune` reclaims unused space. See [Disk Management](../reference/disk-management.md).

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
