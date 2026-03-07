# Architecture

This document describes Shed's internal architecture and design decisions.

## System Overview

```mermaid
flowchart TB
    subgraph client["Developer Machine"]
        CLI["shed CLI"]
        CONFIG["~/.shed/config.yaml"]
        HOSTS["~/.shed/known_hosts"]
        CLI --> CONFIG
        CLI --> HOSTS
    end

    subgraph server1["Server A"]
        SERVER1["shed-server"]
        subgraph docker1["Docker"]
            C1["shed-codelens"]
            C2["shed-mcp-test"]
        end
        SERVER1 -->|"manage"| docker1
    end

    subgraph server2["Server B"]
        SERVER2["shed-server"]
        subgraph docker2["Docker"]
            C3["shed-stbot"]
        end
        SERVER2 -->|"manage"| docker2
    end

    CLI -->|"HTTP :8080"| SERVER1
    CLI -->|"SSH :2222"| SERVER1
    CLI -->|"HTTP :8080"| SERVER2
    CLI -->|"SSH :2222"| SERVER2
```

## Components

| Component | Description |
|-----------|-------------|
| `shed` | CLI binary for developer machines (macOS, Linux) |
| `shed-server` | Server binary exposing HTTP + SSH APIs (Linux, macOS) |
| `shed-agent` | Agent binary running inside Firecracker and VZ VMs (Linux) |
| `shed-base` | Docker image with pre-installed dev tools |

## Communication Protocols

| Protocol | Port | Purpose |
|----------|------|---------|
| HTTP | 8080 | REST API for CRUD operations, server discovery |
| SSH | 2222 | Terminal access, IDE remote connections |
| vsock | 1024 | VM console I/O (Firecracker, VZ) |
| vsock | 1025 | Agent health checks (Firecracker, VZ) |
| vsock | 1026 | Credential change notifications (Firecracker, VZ) |

## Naming Conventions

| Resource | Format | Example |
|----------|--------|---------|
| Container | `shed-{name}` | `shed-codelens` |
| Volume | `shed-{name}-workspace` | `shed-codelens-workspace` |
| SSH Host | `shed-{name}` | `shed-codelens` (in SSH config) |

## Docker Labels

All shed-managed containers are tagged:

```
shed=true
shed.name={name}
shed.created={ISO8601 timestamp}
shed.repo={owner/repo}
shed.backend={docker|firecracker|vz}
shed.local_dir={host path}
```

## Data Flows

### Server Discovery

```mermaid
sequenceDiagram
    participant CLI
    participant Server
    participant Config

    CLI->>Server: GET /api/info
    Server-->>CLI: {name, version, ports}
    CLI->>Server: GET /api/ssh-host-key
    Server-->>CLI: {host_key}
    CLI->>Config: Update config.yaml
    CLI->>Config: Append to known_hosts
```

### Shed Creation

```mermaid
sequenceDiagram
    participant CLI
    participant Server
    participant Docker

    CLI->>Server: POST /api/sheds {name, repo, local_dir}
    alt local_dir specified
        Server->>Docker: Create container (bind mount)
    else
        Server->>Docker: Create volume
        Server->>Docker: Create container
    end
    Server->>Docker: Start container
    alt repo specified
        Server->>Docker: git clone in container
    end
    Server-->>CLI: {name, status, ...}
```

### SSH Connection

```mermaid
sequenceDiagram
    participant User
    participant CLI
    participant SSHServer
    participant Container

    User->>CLI: shed console myproj
    CLI->>SSHServer: SSH as "myproj" user
    SSHServer->>Container: docker exec -it
    Container-->>User: Interactive shell
```

## Internal Packages

### `internal/api`

HTTP API handlers and routing. Uses standard `net/http` with Chi router.

### `internal/config`

Configuration types and loading for both client and server configs.

### `internal/docker`

Docker client wrapper for container and volume operations.

### `internal/sshd`

SSH server implementation using `gliderlabs/ssh`. Routes connections to containers based on username.

### `internal/sshconfig`

Parses and generates SSH config files. Manages the shed-specific block in `~/.ssh/config`.

### `internal/vmutil`

Shared VM agent communication code used by both Firecracker and VZ backends. Contains the `Dialer` interface (the core abstraction differing between backends), `AgentClient` (exec, health checks), `NotifyConn` (persistent auto-reconnecting connections), provisioning, and credential transfer/sync. No build tags — all platform-specificity lives in the `Dialer` implementations.

### `internal/firecracker`

Firecracker backend (Linux only): VM lifecycle via Firecracker SDK, TAP networking, rootfs management, metadata persistence. Implements `vmutil.Dialer` with the CONNECT/OK vsock handshake over a single Unix socket.

### `internal/vz`

VZ backend (macOS Apple Silicon only): VM lifecycle via vfkit subprocess, NAT networking, rootfs management, metadata persistence. Implements `vmutil.Dialer` with direct per-port Unix socket connections (no handshake needed).

### `internal/agentproto`

Binary protocol for framed messages over vsock between shed-server and shed-agent. Message types cover exec, file transfer, health checks, and credential change notifications.

### `internal/backend`

Backend interface that Docker, Firecracker, and VZ backends all implement.

### `internal/provision`

Handles in-repo provisioning hooks (`.shed/provision.yaml`).

### `internal/sync`

Client-side file synchronization to containers.

### `internal/tunnels`

SSH tunnel management for port forwarding.

## Security Model

Shed relies on network-level trust:

- Assumes all machines are on a private network (Tailscale)
- No authentication on HTTP API
- SSH accepts all keys (network access = trust)
- Workloads run as a non-root `shed` user (UID 1000) with passwordless sudo
- Not suitable for multi-tenant or public deployments

## Container Lifecycle

```mermaid
stateDiagram-v2
    [*] --> Created: shed create
    Created --> Running: automatic
    Running --> Stopped: shed stop
    Stopped --> Running: shed start
    Running --> Running: SSH auto-start
    Stopped --> Running: SSH auto-start
    Running --> [*]: shed delete
    Stopped --> [*]: shed delete
```

## File Locations

### Client

| Path | Purpose |
|------|---------|
| `~/.shed/config.yaml` | Server list, defaults, cached sheds |
| `~/.shed/known_hosts` | SSH host keys |
| `~/.shed/sync.yaml` | File sync configuration |
| `~/.shed/tunnels.yaml` | Tunnel profiles |

### Server

| Path | Purpose |
|------|---------|
| `/etc/shed/server.yaml` | Server configuration |
| `/etc/shed/host_key` or `~/.shed/host_key` | SSH host private key (root vs. non-root) |
| `~/.shed/env` | Environment variables for containers |
