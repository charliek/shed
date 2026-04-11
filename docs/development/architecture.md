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

    subgraph server1["Server A (macOS)"]
        SERVER1["shed-server"]
        subgraph vz1["VZ"]
            VM1["shed-codelens"]
            VM2["shed-mcp-test"]
        end
        SERVER1 -->|"manage"| vz1
    end

    subgraph server2["Server B (Linux)"]
        SERVER2["shed-server"]
        subgraph fc1["Firecracker"]
            VM3["shed-stbot"]
        end
        SERVER2 -->|"manage"| fc1
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

## Communication Protocols

| Protocol | Port | Purpose |
|----------|------|---------|
| HTTP | 8080 | REST API for CRUD operations, Connect API, server discovery |
| SSH | 2222 | Terminal access (exec, attach), IDE remote connections |
| vsock | 1024 | VM console I/O (Firecracker, VZ) |
| vsock | 1026 | Message bus — health checks, plugins (Firecracker, VZ) |
| vsock | 1028 | TCP proxy — DialService tunnels into VM services (Firecracker, VZ) |

## Naming Conventions

| Resource | Format | Example |
|----------|--------|---------|
| VM | `shed-{name}` | `shed-codelens` |
| SSH Host | `shed-{name}` | `shed-codelens` (in SSH config) |

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

For the user-facing lifecycle documentation (what happens at each step across all backends), see [Provisioning: Shed Lifecycle](../reference/provisioning.md#shed-lifecycle).

The diagrams below show the internal implementation flow for each backend.

=== "Firecracker"

    ```mermaid
    sequenceDiagram
        participant CLI
        participant Server
        participant VM as Firecracker VM
        participant Agent as shed-agent

        CLI->>Server: POST /api/sheds {name, repo, local_dir}
        Server->>Server: Copy base rootfs to instance dir
        Server->>Server: Allocate CID, TAP device, IP address
        Server->>VM: Spawn Firecracker process
        Server->>Agent: Wait for agent health (poll vsock:1026)
        Agent-->>Server: Healthy
        alt local-dir specified
            Server->>Server: Start 9P TCP server on bridge IP
            Server->>Agent: Mount 9P share at /workspace
        end
        Server->>Agent: Mount 9P credential directories
        alt repo specified
            Server->>Agent: git clone via vsock exec
        end
        Server->>Agent: Run install hook via vsock exec
        Server->>Agent: Capture PATH → /etc/profile.d/
        Server->>Agent: Run startup hook via vsock exec
        Server-->>CLI: {name, status, ...}
        CLI->>CLI: Auto-sync default profile via SSH+tar
    ```

=== "VZ"

    ```mermaid
    sequenceDiagram
        participant CLI
        participant Server
        participant vfkit
        participant Agent as shed-agent

        CLI->>Server: POST /api/sheds {name, repo, local_dir}
        Server->>Server: Copy base rootfs to instance dir
        Server->>vfkit: Spawn vfkit with VirtioFS devices
        Note right of vfkit: Devices: rootfs, local-dir share,<br/>credential directory shares
        Server->>Agent: Wait for agent health (poll vsock:1026)
        Agent-->>Server: Healthy
        alt local-dir specified
            Server->>Agent: Mount VirtioFS share at /workspace
        end
        Server->>Agent: Mount VirtioFS credential directories
        alt repo specified
            Server->>Agent: git clone via vsock exec
        end
        Server->>Agent: Run install hook via vsock exec
        Server->>Agent: Capture PATH → /etc/profile.d/
        Server->>Agent: Run startup hook via vsock exec
        Server-->>CLI: {name, status, ...}
        CLI->>CLI: Auto-sync default profile via SSH+tar
    ```

### Credential Mechanisms

Each backend handles credentials differently based on its isolation model. All credentials must be directories.

**Firecracker** — Credentials are mounted via 9P over the TAP bridge network. Each credential directory gets a TCP-based 9P server on the bridge IP. Changes are immediately visible on both sides.

**VZ** — Credentials are mounted via VirtioFS. Each credential directory gets a VirtioFS share added as a vfkit device argument at VM launch, then mounted inside the guest. Changes are immediately visible on both sides.

### SSH Connection

```mermaid
sequenceDiagram
    participant User
    participant CLI
    participant SSHServer
    participant Agent as shed-agent

    User->>CLI: shed console myproj
    CLI->>SSHServer: SSH as "myproj" user
    SSHServer->>Agent: exec via vsock
    Agent-->>User: Interactive shell
```

## DialService and Connect API

`DialService` is the foundational primitive for opening TCP connections into VMs. It abstracts the per-backend connectivity:

- **VZ:** Dials the vsock TCP proxy port (1028) via a Unix socket, performs a CONNECT handshake (`CONNECT <port>\n` / `OK\n`), and returns a raw TCP connection to the target port inside the VM.
- **Firecracker:** Dials the VM's bridge IP directly over TCP (no proxy needed since VMs have routable IPs).

The **Connect API** (`GET /api/sheds/{name}/connect/{port}`) exposes `DialService` to external processes via HTTP upgrade (101 Switching Protocols). After the upgrade, the connection is a raw bidirectional byte stream.

**Consumers:**

- `shed tunnels` CLI — opens local ports, bridges connections through Connect API
- SSH port forwarding (`handleDirectTCPIP`) — uses `DialService` directly (same process)
- Proxy extension (`shed-ext-proxy`) — uses Connect API for reverse proxying

**Two primitives for two jobs:**

- TCP tunneling (ports, services, proxy): Connect API / `DialService`
- Interactive sessions (exec, attach): SSH / vsock binary framed protocol

## Internal Packages

### `internal/api`

HTTP API handlers and routing. Uses standard `net/http` with Chi router. Includes the Connect API endpoint for TCP tunneling.

### `internal/config`

Configuration types and loading for both client and server configs.

### `internal/sshd`

SSH server implementation using `gliderlabs/ssh`. Routes connections to VMs based on username.

### `internal/sshconfig`

Parses and generates SSH config files. Manages the shed-specific block in `~/.ssh/config`.

### `internal/vmutil`

Shared VM agent communication code used by both Firecracker and VZ backends. Contains the `Dialer` interface (the core abstraction differing between backends), `AgentClient` (exec, health checks), `NotifyConn` (persistent auto-reconnecting connections), and provisioning. No build tags -- all platform-specificity lives in the `Dialer` implementations.

### `internal/firecracker`

Firecracker backend (Linux only): VM lifecycle via Firecracker SDK, TAP networking, rootfs management, metadata persistence. Implements `vmutil.Dialer` with the CONNECT/OK vsock handshake over a single Unix socket.

### `internal/vz`

VZ backend (macOS Apple Silicon only): VM lifecycle via vfkit subprocess, NAT networking, rootfs management, metadata persistence. Implements `vmutil.Dialer` with direct per-port Unix socket connections (no handshake needed).

### `internal/plugin`

Extension/plugin message bus. Defines the message envelope, namespace registry, and bridge (connects API to per-shed vsock connections). See [Extensions](../reference/extensions.md).

### `internal/agentproto`

Binary protocol for framed messages over vsock between shed-server and shed-agent. Message types cover exec, file transfer, health checks, and plugin messages.

### `internal/backend`

Backend interface that Firecracker and VZ backends implement.

### `internal/provision`

Handles in-repo provisioning hooks (`.shed/provision.yaml`).

### `internal/sync`

Client-side file synchronization to VMs.

### `internal/tunnels`

Connect API-based tunnel management for port forwarding.

## Security Model

Shed relies on network-level trust:

- Assumes all machines are on a private network (Tailscale)
- No authentication on HTTP API
- SSH accepts all keys (network access = trust)
- Workloads run as a non-root `shed` user (UID 1000) with passwordless sudo
- Not suitable for multi-tenant or public deployments

## Shed Lifecycle

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
| `~/.shed/env` | Environment variables for VMs |
