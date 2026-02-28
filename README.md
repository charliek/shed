# Shed

Shed is a lightweight tool for managing persistent, containerized development environments across multiple servers. It enables developers to spin up isolated coding sessions with AI tools (Claude Code, OpenCode) pre-installed, disconnect, and reconnect later to continue work.

## Features

- **Simple CLI** - Create and manage dev environments with minimal commands
- **Session Persistence** - Containers keep running after disconnect
- **Multi-Server** - Manage sheds across home servers and cloud VPS instances
- **IDE Integration** - Native Cursor/VS Code support via SSH Remote
- **AI-Ready** - Pre-configured for Claude Code and OpenCode workflows
- **Multiple Backends** - Docker containers, Firecracker microVMs (Linux), or Apple VZ virtual machines (macOS Apple Silicon)

## Quick Start

### 1. Install the CLI

```bash
# Build from source
make build

# Or install directly
go install github.com/charliek/shed/cmd/shed@latest
```

### 2. Add a Server

```bash
shed server add my-server.local --name my-server
```

### 3. Create a Shed

```bash
# Create an empty shed
shed create my-project

# Or clone a repository
shed create my-project --repo git@github.com:user/repo.git
```

### 4. Connect

```bash
# Open a terminal session
shed console my-project

# Or use VS Code/Cursor with SSH Remote
# The shed ssh-config command generates SSH config entries
shed ssh-config >> ~/.ssh/config
```

## Backends

### Docker (Default)

Uses Docker containers with bind mounts for credential passthrough. Best for:
- Quick setup (just needs Docker installed)
- Shared filesystem access with host
- Lower resource overhead

### Firecracker

Uses Firecracker microVMs for stronger isolation. Best for:
- Security-sensitive workloads
- Full VM-level isolation
- Running Docker inside the shed

```bash
# Create with Firecracker backend
shed create myproject --backend=firecracker

# With custom resources
shed create myproject --backend=firecracker --cpus=4 --memory=8192

# Clone a private repo (requires SSH credentials configured)
shed create myproject --backend=firecracker --repo=git@github.com:user/repo.git
```

See [Firecracker Installation](docs/firecracker_install.md) and [Firecracker Operations](docs/firecracker_howto.md) for setup and usage details.

### VZ (macOS)

Uses Apple's Virtualization.framework for native Linux VMs on macOS. Best for:
- Local development on macOS with full VM isolation
- Running Docker inside the shed (no Docker Desktop needed)
- Same vsock + agent architecture as Firecracker

```bash
# Create with VZ backend
shed create myproject --backend=vz

# With custom resources
shed create myproject --backend=vz --cpus=4 --memory=8192
```

See [VZ Setup](docs/getting-started/vz-setup.md) and [VZ Operations](docs/reference/vz-operations.md) for setup and usage details.

## Requirements

- **Client**: macOS or Linux with Go 1.24+
- **Server (Docker)**: Linux with Docker installed
- **Server (Firecracker)**: Linux with KVM support
- **Server (VZ)**: macOS 13+ (Ventura) on Apple Silicon (arm64)
- **Network**: Tailscale (or any private network) connecting all machines

## Architecture

Shed consists of two binaries:

- **`shed`** - CLI for developer machines
- **`shed-server`** - Server daemon exposing HTTP API (port 8080) and SSH server (port 2222)

The server supports three backends:
- **Docker** (default) - Uses Docker containers with bind mounts for credentials
- **Firecracker** - Uses microVMs with vsock communication for better isolation (Linux only)
- **VZ** - Uses Apple Virtualization.framework VMs via vfkit (macOS Apple Silicon only)

```
Developer Machine                    Remote Server / Local Mac
┌─────────────────┐                 ┌──────────────────────────────────────┐
│    shed CLI     │ ──HTTP/SSH───▶ │  shed-server                         │
└─────────────────┘                 │  ├── HTTP API (CRUD operations)      │
                                    │  └── SSH Server (terminal/IDE)       │
                                    │              │                        │
                                    │      ┌───────┼───────┐               │
                                    │      ▼       ▼       ▼               │
                                    │  ┌──────┐ ┌──────┐ ┌────┐           │
                                    │  │Docker│ │  FC  │ │ VZ │           │
                                    │  │┌────┐│ │┌────┐│ │┌──┐│           │
                                    │  ││shed││ ││ VM ││ ││VM││           │
                                    │  │└────┘│ │└────┘│ │└──┘│           │
                                    │  └──────┘ └──────┘ └────┘           │
                                    └──────────────────────────────────────┘
```

## CLI Commands

```bash
# Shed Management
shed create <name> [--repo URL]  # Create a new shed
shed list                        # List all sheds on the current server
shed start <name>                # Start a stopped shed
shed stop <name>                 # Stop a running shed
shed delete <name> [--force]     # Delete a shed

# Connection & Execution
shed console <name>              # Open direct terminal session
shed attach <name>               # Attach to tmux session (persistent)
shed attach <name> -S <session>  # Attach to named session
shed exec <name> <cmd>           # Run command in shed

# Session Management
shed sessions                    # List all sessions on default server
shed sessions <name>             # List sessions in a specific shed
shed sessions --all              # List sessions across all servers
shed sessions kill <shed> <session>  # Kill a session

# Server & IDE
shed server add <name>           # Add a server to client config
shed server list                 # List configured servers
shed server remove <name>        # Remove a server from client config
shed ssh-config                  # Generate SSH config for IDE integration
```

## Session Persistence

Shed supports persistent sessions via tmux. This allows you to:

1. Start a long-running agent (Claude Code, OpenCode)
2. Detach from the session (Ctrl-B D)
3. Reconnect later to check on progress
4. Run multiple named sessions per shed

### Quick Example

```bash
# Create a shed and attach to a persistent session
shed create myproj --repo user/repo
shed attach myproj

# Inside the session, start an agent
claude

# Detach with Ctrl-B D (tmux default)
# The agent keeps running!

# Later, reattach to see progress
shed attach myproj

# List all active sessions
shed sessions --all
```

### console vs attach

- `shed console` - Direct shell, no persistence (exits when you disconnect)
- `shed attach` - tmux session, persists after disconnect

## Server Setup

See [docs/SERVER_SETUP.md](docs/SERVER_SETUP.md) for detailed server installation and configuration instructions.

## Development

See [docs/DEVELOPMENT.md](docs/DEVELOPMENT.md) for building from source and contributing.

## Configuration

### Client Configuration (`~/.shed/config.yaml`)

```yaml
default_server: my-server
servers:
  my-server:
    host: my-server.local
    http_port: 8080
    ssh_port: 2222
```

### Server Configuration (`/etc/shed/server.yaml` or `~/.config/shed/server.yaml`)

```yaml
name: my-server
http_port: 8080
ssh_port: 2222
default_image: shed-base:latest

# enabled_backends:
#   - docker
#   - firecracker  # Linux only
#   - vz           # macOS Apple Silicon only
# default_backend: docker

credentials:
  ssh:
    source: ~/.ssh
    target: /home/shed/.ssh
    readonly: true
  gitconfig:
    source: ~/.gitconfig
    target: /home/shed/.gitconfig
    readonly: true
env_file: ~/.shed/env

# Firecracker-specific config (only needed if firecracker is enabled)
# firecracker:
#   kernel_path: /var/lib/shed/firecracker/vmlinux.bin
#   base_rootfs: /var/lib/shed/firecracker/base-rootfs.ext4
#   instance_dir: /var/lib/shed/firecracker/instances
#   socket_dir: /var/run/shed/firecracker
#   default_cpus: 2
#   default_memory_mb: 4096

# VZ-specific config (only needed if vz is enabled, macOS Apple Silicon only)
# vz:
#   vfkit_path: vfkit
#   kernel_path: ~/Library/Application Support/shed/vz/vmlinux
#   base_rootfs: ~/Library/Application Support/shed/vz/base-rootfs.ext4
#   instance_dir: ~/Library/Application Support/shed/vz/instances
#   socket_dir: ~/Library/Application Support/shed/vz/sockets
#   default_cpus: 2
#   default_memory_mb: 4096
```

## Security Model

Shed is designed for single-user scenarios where:
- All machines are connected via Tailscale (or similar private network)
- The developer owns/controls all machines
- Network access implies trust
- Workloads run as a non-root `shed` user (UID 1000) with passwordless sudo

**Not suitable for:**
- Multi-tenant environments
- Public internet exposure
- Untrusted network access

## License

MIT
