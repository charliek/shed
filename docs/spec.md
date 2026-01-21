# Shed: Technical Specification

**Version:** 1.0.0-draft  
**Date:** January 2026  
**Status:** MVP Specification

---

## Table of Contents

1. [Overview](#1-overview)
2. [Architecture](#2-architecture)
3. [Server Specification](#3-server-specification)
4. [CLI Specification](#4-cli-specification)
5. [Configuration Schemas](#5-configuration-schemas)
6. [Data Flows](#6-data-flows)
7. [Base Image](#7-base-image)
8. [File & Directory Layouts](#8-file--directory-layouts)
9. [Error Catalog](#9-error-catalog)
10. [Build & Release](#10-build--release)
11. [Testing Strategy](#11-testing-strategy)
12. [Post-MVP Roadmap](#12-post-mvp-roadmap)

---

## 1. Overview

### 1.1 Purpose

Shed is a lightweight tool for managing persistent, containerized development environments across multiple servers. It enables developers to spin up isolated coding sessions with AI tools (Claude Code, OpenCode) pre-installed, disconnect, and reconnect later to continue work.

### 1.2 Goals

- **Simple CLI:** Create and manage dev environments with minimal commands
- **Session Persistence:** Containers keep running after disconnect
- **Multi-Server:** Manage sheds across home servers and cloud VPS instances
- **IDE Integration:** Native Cursor/VS Code support via SSH
- **AI-Ready:** Pre-configured for Claude Code and OpenCode workflows

### 1.3 Non-Goals (MVP)

- Multi-user support
- Authentication beyond Tailscale network trust
- Container resource limits
- Automatic provisioning hooks
- TLS on HTTP API

### 1.4 Assumptions

- All servers connected via Tailscale
- Single user (developer) owns all machines
- Docker installed on all servers
- Credentials (Git SSH, Claude auth, API keys) configured per-server

---

## 2. Architecture

### 2.1 System Overview

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                              CLIENT (Developer Machine)                      │
│                                                                             │
│  ┌─────────────┐         ┌──────────────────────────────────────────────┐  │
│  │  shed CLI   │         │  ~/.shed/                                    │  │
│  │             │────────▶│    config.yaml     (servers, defaults)       │  │
│  │  Commands:  │         │    known_hosts     (SSH host keys)           │  │
│  │  - create   │         └──────────────────────────────────────────────┘  │
│  │  - list     │                                                           │
│  │  - console  │                                                           │
│  │  - exec     │                                                           │
│  │  - delete   │                                                           │
│  └──────┬──────┘                                                           │
└─────────┼──────────────────────────────────────────────────────────────────┘
          │
          │  Tailscale Network
          │
          ├─────────────────────────────────────────┐
          ▼                                         ▼
┌──────────────────────────────┐      ┌──────────────────────────────┐
│  Server A (mini-desktop)     │      │  Server B (cloud-vps)        │
│  ┌────────────────────────┐  │      │  ┌────────────────────────┐  │
│  │     shed-server        │  │      │  │     shed-server        │  │
│  │                        │  │      │  │                        │  │
│  │  HTTP API    :8080     │  │      │  │  HTTP API    :8080     │  │
│  │  SSH Server  :2222     │  │      │  │  SSH Server  :2222     │  │
│  └───────────┬────────────┘  │      │  └───────────┬────────────┘  │
│              │               │      │              │               │
│              ▼               │      │              ▼               │
│  ┌────────────────────────┐  │      │  ┌────────────────────────┐  │
│  │       Docker           │  │      │  │       Docker           │  │
│  │  ┌──────────────────┐  │  │      │  │  ┌──────────────────┐  │  │
│  │  │  shed-codelens   │  │  │      │  │  │  shed-stbot      │  │  │
│  │  ├──────────────────┤  │  │      │  │  └──────────────────┘  │  │
│  │  │  shed-mcp-test   │  │  │      │  └────────────────────────┘  │
│  │  └──────────────────┘  │  │      └──────────────────────────────┘
│  └────────────────────────┘  │
└──────────────────────────────┘
```

### 2.2 Components

| Component | Description |
|-----------|-------------|
| `shed` | CLI binary for developer machines (macOS, Linux) |
| `shed-server` | Server binary exposing HTTP + SSH APIs (Linux) |
| `shed-base` | Docker image with pre-installed dev tools |

### 2.3 Communication Protocols

| Protocol | Port | Purpose |
|----------|------|---------|
| HTTP | 8080 | REST API for CRUD operations, server discovery |
| SSH | 2222 | Terminal access, IDE remote connections |

### 2.4 Naming Conventions

| Resource | Format | Example |
|----------|--------|---------|
| Container | `shed-{name}` | `shed-codelens` |
| Volume | `shed-{name}-workspace` | `shed-codelens-workspace` |
| SSH Host | `shed-{name}` | `shed-codelens` (in ~/.ssh/config) |

### 2.5 Docker Labels

All shed-managed containers are tagged with:

```
shed=true
shed.name={name}
shed.created={ISO8601 timestamp}
shed.repo={owner/repo}  # if created with --repo
```

---

## 3. Server Specification

### 3.1 Overview

The `shed-server` binary runs on each server machine, exposing both HTTP and SSH interfaces. It manages Docker containers and provides terminal access.

### 3.2 HTTP API

**Base URL:** `http://{host}:8080/api`

#### 3.2.1 GET /api/info

Returns server metadata and capabilities.

**Response:**
```json
{
  "name": "mini-desktop",
  "version": "1.0.0",
  "ssh_port": 2222,
  "http_port": 8080
}
```

#### 3.2.2 GET /api/ssh-host-key

Returns the server's SSH host public key for client known_hosts.

**Response:**
```json
{
  "host_key": "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAA..."
}
```

#### 3.2.3 GET /api/sheds

Lists all sheds on this server.

**Response:**
```json
{
  "sheds": [
    {
      "name": "codelens",
      "status": "running",
      "created_at": "2026-01-20T10:30:00Z",
      "repo": "charliek/codelens",
      "container_id": "abc123..."
    },
    {
      "name": "mcp-test",
      "status": "stopped",
      "created_at": "2026-01-18T14:00:00Z",
      "repo": null,
      "container_id": "def456..."
    }
  ]
}
```

**Status values:** `running`, `stopped`, `starting`, `error`

#### 3.2.4 POST /api/sheds

Creates a new shed.

**Request:**
```json
{
  "name": "codelens",
  "repo": "charliek/codelens",
  "image": "shed-base:latest"
}
```

| Field | Required | Default | Description |
|-------|----------|---------|-------------|
| name | Yes | - | Shed name (alphanumeric + hyphens) |
| repo | No | null | GitHub repo to clone (owner/repo format) |
| image | No | From server config | Base Docker image |

**Response (201 Created):**
```json
{
  "name": "codelens",
  "status": "running",
  "created_at": "2026-01-20T10:30:00Z",
  "repo": "charliek/codelens",
  "container_id": "abc123..."
}
```

**Errors:**
- `409 Conflict` - Shed with this name already exists
- `400 Bad Request` - Invalid name format
- `500 Internal Server Error` - Docker or clone failure

#### 3.2.5 GET /api/sheds/{name}

Gets details for a specific shed.

**Response (200 OK):**
```json
{
  "name": "codelens",
  "status": "running",
  "created_at": "2026-01-20T10:30:00Z",
  "repo": "charliek/codelens",
  "container_id": "abc123..."
}
```

**Errors:**
- `404 Not Found` - Shed does not exist

#### 3.2.6 DELETE /api/sheds/{name}

Deletes a shed and its data.

**Query Parameters:**
| Param | Default | Description |
|-------|---------|-------------|
| keep_volume | false | If true, preserves the workspace volume |

**Response (204 No Content)**

**Behavior:**
1. Stop container if running
2. Remove container
3. Remove volume (unless keep_volume=true)

**Errors:**
- `404 Not Found` - Shed does not exist

#### 3.2.7 POST /api/sheds/{name}/start

Starts a stopped shed.

**Response (200 OK):**
```json
{
  "name": "codelens",
  "status": "running",
  ...
}
```

**Errors:**
- `404 Not Found` - Shed does not exist
- `409 Conflict` - Shed is already running

#### 3.2.8 POST /api/sheds/{name}/stop

Stops a running shed.

**Response (200 OK):**
```json
{
  "name": "codelens",
  "status": "stopped",
  ...
}
```

**Errors:**
- `404 Not Found` - Shed does not exist
- `409 Conflict` - Shed is already stopped

### 3.3 SSH Server

#### 3.3.1 Connection Routing

The SSH username determines which container to connect to:

```
ssh codelens@mini-desktop.tailnet.ts.net -p 2222
     └──┬───┘
        └── Container name (shed-codelens)
```

**Special usernames:**
- `_api` - Reserved for future CLI-over-SSH operations

#### 3.3.2 Session Types

**Interactive Shell (no command):**
```bash
ssh codelens@server -p 2222
# Opens /bin/bash in shed-codelens container
```

**Command Execution:**
```bash
ssh codelens@server -p 2222 "git status"
# Executes command, returns output, exits
```

**SFTP:**
```bash
sftp -P 2222 codelens@server
# File access to /workspace in container
```

#### 3.3.3 PTY Handling

- Terminal type passed via `TERM` environment variable
- Window resize events (`SIGWINCH`) forwarded to container
- Raw mode preserved for full terminal compatibility

#### 3.3.4 Auto-Start Behavior

If a connection targets a stopped container:
1. Start the container
2. Wait for container to be ready (up to 10 seconds)
3. Attach to shell

#### 3.3.5 Authentication

**MVP:** Accept all SSH keys (Tailscale network is trust boundary)

The server should still collect and log the connecting key fingerprint for audit purposes.

### 3.4 Container Management

#### 3.4.1 Container Creation

When creating a shed, the server:

1. Creates Docker volume `shed-{name}-workspace`
2. Creates container `shed-{name}` with:
   - Image from request or server config default
   - Workspace volume mounted at `/workspace`
   - Credential mounts from server config
   - Environment variables from server config
   - Labels for shed identification
3. Starts container
4. If `repo` specified:
   - Executes `git clone git@github.com:{repo}.git /workspace/{repo-name}`
   - Sets working directory context

#### 3.4.2 Mount Configuration

Mounts are defined in server config and applied to all containers:

```yaml
credentials:
  git_ssh:
    source: ~/.ssh
    target: /root/.ssh
    readonly: true
```

#### 3.4.3 Environment Variables

Environment variables are loaded from the configured env_file and injected into containers.

### 3.5 Server Installation

The server binary supports self-installation:

```bash
# Install as systemd service
sudo shed-server install

# Uninstall
sudo shed-server uninstall

# Run in foreground (for debugging)
shed-server serve
```

**Systemd unit location:** `/etc/systemd/system/shed-server.service`

---

## 4. CLI Specification

### 4.1 Global Flags

| Flag | Short | Description |
|------|-------|-------------|
| `--server` | `-s` | Target server (overrides default) |
| `--verbose` | `-v` | Enable debug output |
| `--config` | `-c` | Config file path (default: ~/.shed/config.yaml) |

### 4.2 Server Management Commands

#### 4.2.1 shed server add

Adds a new server to the client configuration.

```bash
shed server add <host> [flags]
```

**Arguments:**
- `host` - Server hostname or IP (e.g., `mini-desktop.tailnet.ts.net`)

**Flags:**
| Flag | Default | Description |
|------|---------|-------------|
| `--name` | Derived from host | Friendly name for server |
| `--http-port` | 8080 | HTTP API port |
| `--ssh-port` | 2222 | SSH port |

**Behavior:**
1. Connect to `http://{host}:{http-port}/api/info`
2. Retrieve server metadata
3. Fetch SSH host key from `/api/ssh-host-key`
4. Add to `~/.shed/config.yaml`
5. Add host key to `~/.shed/known_hosts`
6. If first server, set as default

**Output:**
```
✓ Connected to mini-desktop.tailnet.ts.net
✓ Server version: 1.0.0
✓ Retrieved SSH host key
✓ Server "mini-desktop" added and set as default
```

#### 4.2.2 shed server list

Lists configured servers.

```bash
shed server list
```

**Output:**
```
NAME            HOST                              STATUS     DEFAULT
mini-desktop    mini-desktop.tailnet.ts.net       online     *
cloud-vps       vps.tailnet.ts.net                offline
```

#### 4.2.3 shed server remove

Removes a server from configuration.

```bash
shed server remove <name>
```

**Behavior:**
1. Remove from config.yaml
2. Optionally remove associated entries from known_hosts
3. Clear cached shed list for this server

#### 4.2.4 shed server set-default

Sets the default server.

```bash
shed server set-default <name>
```

### 4.3 Shed Management Commands

#### 4.3.1 shed create

Creates a new shed.

```bash
shed create <name> [flags]
```

**Arguments:**
- `name` - Shed name (alphanumeric, hyphens allowed)

**Flags:**
| Flag | Default | Description |
|------|---------|-------------|
| `--repo`, `-r` | None | GitHub repo to clone (owner/repo) |
| `--server`, `-s` | Default server | Target server |
| `--image` | Server default | Base Docker image |

**Examples:**
```bash
# Empty shed
shed create scratch

# Shed with repo
shed create codelens --repo charliek/codelens

# On specific server
shed create stbot --repo charliek/stbot --server cloud-vps
```

**Output:**
```
✓ Creating shed "codelens" on mini-desktop...
✓ Cloning charliek/codelens...
✓ Shed ready

Connect with: shed console codelens
```

#### 4.3.2 shed list

Lists sheds.

```bash
shed list [flags]
```

**Flags:**
| Flag | Default | Description |
|------|---------|-------------|
| `--server`, `-s` | Default | List from specific server |
| `--all`, `-a` | false | List from all servers |

**Output:**
```
SERVER          NAME          STATUS     CREATED          REPO
mini-desktop    codelens      running    2 hours ago      charliek/codelens
mini-desktop    mcp-test      stopped    3 days ago       -
cloud-vps       stbot         running    1 day ago        charliek/stbot
```

#### 4.3.3 shed delete

Deletes a shed.

```bash
shed delete <name> [flags]
```

**Flags:**
| Flag | Default | Description |
|------|---------|-------------|
| `--keep-volume` | false | Preserve workspace data |
| `--force`, `-f` | false | Skip confirmation |

**Behavior:**
1. Prompt for confirmation (unless --force)
2. Call DELETE /api/sheds/{name}
3. Update local cache

**Output:**
```
Delete shed "codelens" and all its data? [y/N]: y
✓ Deleted shed "codelens"
```

#### 4.3.4 shed stop

Stops a running shed.

```bash
shed stop <name>
```

**Output:**
```
✓ Stopped shed "codelens"
```

#### 4.3.5 shed start

Starts a stopped shed.

```bash
shed start <name>
```

**Output:**
```
✓ Started shed "codelens"
```

### 4.4 Interactive Commands

#### 4.4.1 shed console

Opens an interactive shell in a shed.

```bash
shed console <name>
```

**Behavior:**
1. Look up shed's server from cache
2. If shed is stopped, start it
3. SSH to `{name}@{server}:{ssh_port}`
4. Pass through terminal to container

**Implementation:**
Executes system SSH with appropriate arguments:
```bash
ssh -t -p 2222 \
    -o UserKnownHostsFile=~/.shed/known_hosts \
    -o StrictHostKeyChecking=yes \
    codelens@mini-desktop.tailnet.ts.net
```

#### 4.4.2 shed exec

Executes a command in a shed.

```bash
shed exec <name> <command...>
```

**Examples:**
```bash
shed exec codelens git status
shed exec codelens "cd /workspace/codelens && npm test"
```

**Output:**
Command stdout/stderr streamed to terminal, exits with command's exit code.

### 4.5 IDE Integration Commands

#### 4.5.1 shed ssh-config

Generates or installs SSH config for IDE integration.

```bash
shed ssh-config [name] [flags]
```

**Arguments:**
- `name` - Specific shed (optional)

**Flags:**
| Flag | Default | Description |
|------|---------|-------------|
| `--all` | false | Generate for all known sheds |
| `--install` | false | Write to ~/.ssh/config |
| `--dry-run` | false | Show changes without applying |
| `--uninstall` | false | Remove entries from ~/.ssh/config |

**Examples:**

Print config for one shed:
```bash
shed ssh-config codelens
```
```
# Add to ~/.ssh/config:

Host shed-codelens
    HostName mini-desktop.tailnet.ts.net
    Port 2222
    User codelens
    UserKnownHostsFile ~/.shed/known_hosts
```

Dry run install:
```bash
shed ssh-config --all --install --dry-run
```
```
Would modify: ~/.ssh/config

--- ADDITIONS ---
+ Host shed-codelens
+     HostName mini-desktop.tailnet.ts.net
+     Port 2222
+     User codelens
+     UserKnownHostsFile ~/.shed/known_hosts
+
+ Host shed-stbot
+     HostName cloud-vps.tailnet.ts.net
+     Port 2222
+     User stbot
+     UserKnownHostsFile ~/.shed/known_hosts

--- REMOVALS ---
- Host shed-old-project    (shed no longer exists)

--- UNCHANGED ---
  Host shed-mcp-test       (already correct)

Run without --dry-run to apply changes.
```

Install:
```bash
shed ssh-config --all --install
```
```
✓ Updated ~/.ssh/config (added 2, removed 1, unchanged 1)
```

**SSH Config Block Format:**

The CLI manages a clearly delimited block:

```
# --- BEGIN SHED MANAGED BLOCK ---
# Do not edit manually - managed by shed CLI
# Last updated: 2026-01-20T10:30:00Z

Host shed-codelens
    HostName mini-desktop.tailnet.ts.net
    Port 2222
    User codelens
    UserKnownHostsFile ~/.shed/known_hosts

Host shed-stbot
    HostName cloud-vps.tailnet.ts.net
    Port 2222
    User stbot
    UserKnownHostsFile ~/.shed/known_hosts

# --- END SHED MANAGED BLOCK ---
```

**Behavior:**
- `--install` replaces entire managed block
- Content outside the block is preserved
- If block doesn't exist, appends to end of file
- Creates `~/.ssh/config` if it doesn't exist (with mode 0600)

### 4.6 Shed Resolution

When a command references a shed by name:

1. Check local cache (`~/.shed/config.yaml` sheds section)
2. If not found and `--all` not specified, query default server
3. If not found and `--all` specified, query all servers
4. Cache result for future lookups

This allows `shed console codelens` to work without specifying `--server` after the first `shed list --all`.

---

## 5. Configuration Schemas

### 5.1 Client Configuration

**Location:** `~/.shed/config.yaml`

```yaml
# Client configuration schema

# Configured servers
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

# Default server for commands
default_server: mini-desktop

# Cached shed locations (updated on list)
sheds:
  codelens:
    server: mini-desktop
    status: running
    updated_at: "2026-01-20T10:30:00Z"
  stbot:
    server: cloud-vps
    status: running
    updated_at: "2026-01-20T09:00:00Z"
```

### 5.2 Server Configuration

**Locations (checked in order):**
1. `./server.yaml`
2. `~/.config/shed/server.yaml`
3. `/etc/shed/server.yaml`

```yaml
# Server configuration schema

# Server identity
name: mini-desktop

# Network configuration
http_port: 8080
ssh_port: 2222

# Docker settings
default_image: shed-base:latest

# Credentials to mount into containers
# Paths support ~ expansion
credentials:
  git_ssh:
    source: ~/.ssh
    target: /root/.ssh
    readonly: true
    
  git_config:
    source: ~/.gitconfig
    target: /root/.gitconfig
    readonly: true
    
  claude:
    source: ~/.claude
    target: /root/.claude
    readonly: false  # Needs write for token refresh
    
  opencode:
    source: ~/.config/opencode
    target: /root/.config/opencode
    readonly: true
    
  gh:
    source: ~/.config/gh
    target: /root/.config/gh
    readonly: true

# Environment file to source
# Each line: KEY=value
env_file: ~/.shed/env

# Logging
log_level: info  # debug, info, warn, error
```

### 5.3 Server Environment File

**Location:** `~/.shed/env` (or as configured)

```bash
# Environment variables injected into all containers
ANTHROPIC_API_KEY=sk-ant-...
GITHUB_TOKEN=ghp_...
OPENAI_API_KEY=sk-...
```

---

## 6. Data Flows

### 6.1 Server Discovery Flow

```
shed server add mini-desktop.tailnet.ts.net
                        │
                        ▼
    ┌─────────────────────────────────────────────┐
    │  GET http://mini-desktop...:8080/api/info   │
    └─────────────────────────────────────────────┘
                        │
                        ▼
              ┌─────────────────┐
              │  200 OK         │
              │  {              │
              │    "name": ..., │
              │    "version":.. │
              │  }              │
              └─────────────────┘
                        │
                        ▼
    ┌──────────────────────────────────────────────────┐
    │  GET http://mini-desktop...:8080/api/ssh-host-key│
    └──────────────────────────────────────────────────┘
                        │
                        ▼
              ┌────────────────────┐
              │  200 OK            │
              │  { "host_key": ... }│
              └────────────────────┘
                        │
                        ▼
    ┌─────────────────────────────────────────────┐
    │  Update ~/.shed/config.yaml                 │
    │  Append to ~/.shed/known_hosts              │
    └─────────────────────────────────────────────┘
                        │
                        ▼
              ┌─────────────────┐
              │  Success output │
              └─────────────────┘
```

### 6.2 Shed Creation Flow

```
shed create codelens --repo charliek/codelens
                        │
                        ▼
    ┌─────────────────────────────────────────────┐
    │  Resolve server (default: mini-desktop)     │
    └─────────────────────────────────────────────┘
                        │
                        ▼
    ┌─────────────────────────────────────────────┐
    │  POST /api/sheds                            │
    │  {                                          │
    │    "name": "codelens",                      │
    │    "repo": "charliek/codelens"              │
    │  }                                          │
    └─────────────────────────────────────────────┘
                        │
                        ▼ (on server)
    ┌─────────────────────────────────────────────┐
    │  1. docker volume create shed-codelens-workspace │
    └─────────────────────────────────────────────┘
                        │
                        ▼
    ┌─────────────────────────────────────────────┐
    │  2. docker create shed-codelens             │
    │     - Mount volume                          │
    │     - Mount credentials                     │
    │     - Set labels                            │
    │     - Inject env vars                       │
    └─────────────────────────────────────────────┘
                        │
                        ▼
    ┌─────────────────────────────────────────────┐
    │  3. docker start shed-codelens              │
    └─────────────────────────────────────────────┘
                        │
                        ▼
    ┌─────────────────────────────────────────────┐
    │  4. docker exec: git clone ...              │
    └─────────────────────────────────────────────┘
                        │
                        ▼
    ┌─────────────────────────────────────────────┐
    │  201 Created                                │
    │  { "name": "codelens", "status": "running" }│
    └─────────────────────────────────────────────┘
                        │
                        ▼ (on client)
    ┌─────────────────────────────────────────────┐
    │  Update local shed cache                    │
    │  Print success message                      │
    └─────────────────────────────────────────────┘
```

### 6.3 Console Connection Flow

```
shed console codelens
         │
         ▼
┌─────────────────────────────────────────────┐
│  1. Resolve shed server from cache          │
│     → mini-desktop                          │
└─────────────────────────────────────────────┘
         │
         ▼
┌─────────────────────────────────────────────┐
│  2. Check shed status (optional, cached)    │
│     → running (or start if stopped)         │
└─────────────────────────────────────────────┘
         │
         ▼
┌─────────────────────────────────────────────┐
│  3. Execute SSH command                     │
│     ssh -t -p 2222 \                        │
│       -o UserKnownHostsFile=~/.shed/known_hosts \
│       codelens@mini-desktop.tailnet.ts.net  │
└─────────────────────────────────────────────┘
         │
         ▼ (on server, SSH handler)
┌─────────────────────────────────────────────┐
│  4. Parse username → "codelens"             │
│     Map to container → "shed-codelens"      │
└─────────────────────────────────────────────┘
         │
         ▼
┌─────────────────────────────────────────────┐
│  5. docker exec -it shed-codelens /bin/bash │
│     Attach PTY, forward resize events       │
└─────────────────────────────────────────────┘
         │
         ▼
┌─────────────────────────────────────────────┐
│  6. Bidirectional stream:                   │
│     SSH client ←→ shed-server ←→ container  │
└─────────────────────────────────────────────┘
```

### 6.4 SSH Config Installation Flow

```
shed ssh-config --all --install --dry-run
         │
         ▼
┌─────────────────────────────────────────────┐
│  1. Load current ~/.ssh/config              │
└─────────────────────────────────────────────┘
         │
         ▼
┌─────────────────────────────────────────────┐
│  2. Parse existing SHED MANAGED BLOCK       │
│     → Extract current entries               │
└─────────────────────────────────────────────┘
         │
         ▼
┌─────────────────────────────────────────────┐
│  3. Load shed cache from ~/.shed/config.yaml│
│     → All known sheds across servers        │
└─────────────────────────────────────────────┘
         │
         ▼
┌─────────────────────────────────────────────┐
│  4. Compute diff:                           │
│     - Additions (new sheds)                 │
│     - Removals (deleted sheds)              │
│     - Unchanged (existing, correct)         │
└─────────────────────────────────────────────┘
         │
         ▼
┌─────────────────────────────────────────────┐
│  5. If --dry-run: print diff and exit       │
│     Otherwise: write updated config         │
└─────────────────────────────────────────────┘
```

---

## 7. Base Image

### 7.1 Dockerfile

**Location:** `images/shed-base/Dockerfile`

```dockerfile
FROM ubuntu:24.04

# System packages
RUN apt-get update && apt-get install -y \
    curl \
    git \
    tmux \
    vim \
    wget \
    jq \
    openssh-client \
    build-essential \
    ca-certificates \
    && rm -rf /var/lib/apt/lists/*

# Node.js 22.x (for Claude Code)
RUN curl -fsSL https://deb.nodesource.com/setup_22.x | bash - \
    && apt-get install -y nodejs \
    && rm -rf /var/lib/apt/lists/*

# Claude Code
RUN npm install -g @anthropic-ai/claude-code

# OpenCode
RUN curl -fsSL https://opencode.ai/install | bash

# GitHub CLI
RUN curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg \
      -o /usr/share/keyrings/githubcli-archive-keyring.gpg \
    && echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" \
      > /etc/apt/sources.list.d/github-cli.list \
    && apt-get update && apt-get install -y gh \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /workspace

CMD ["sleep", "infinity"]
```

### 7.2 Build Script

**Location:** `scripts/build-image.sh`

```bash
#!/bin/bash
set -e

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(dirname "$SCRIPT_DIR")"
TAG="${TAG:-shed-base:latest}"

echo "Building shed-base image..."
docker build -t "$TAG" "$REPO_ROOT/images/shed-base"
echo "✓ Built $TAG"
```

---

## 8. File & Directory Layouts

### 8.1 Repository Structure

```
shed/
├── cmd/
│   ├── shed/                     # CLI entrypoint
│   │   └── main.go
│   └── shed-server/              # Server entrypoint
│       └── main.go
├── internal/
│   ├── api/                      # HTTP handlers
│   │   ├── handlers.go
│   │   ├── middleware.go
│   │   └── routes.go
│   ├── sshd/                     # SSH server
│   │   ├── server.go
│   │   └── session.go
│   ├── docker/                   # Docker client wrapper
│   │   ├── client.go
│   │   ├── containers.go
│   │   └── volumes.go
│   ├── config/                   # Configuration
│   │   ├── client.go
│   │   ├── server.go
│   │   └── types.go
│   ├── sshconfig/                # SSH config management
│   │   ├── parser.go
│   │   └── writer.go
│   └── version/                  # Version info
│       └── version.go
├── images/
│   └── shed-base/
│       └── Dockerfile
├── configs/
│   └── server.example.yaml
├── scripts/
│   ├── install-cli.sh            # CLI installer
│   ├── build-image.sh            # Build base image
│   └── release.sh                # Build release binaries
├── Makefile
├── go.mod
├── go.sum
└── README.md
```

### 8.2 Client Directories

```
~/.shed/
├── config.yaml          # Client configuration
├── known_hosts          # SSH host keys for shed servers
└── cache/               # Optional: response caching
```

### 8.3 Server Directories

```
~/.config/shed/
└── server.yaml          # Server configuration (alternative location)

~/.shed/
└── env                  # Environment variables file

/etc/shed/
├── server.yaml          # Server configuration (system location)
└── host_key             # SSH host private key
```

### 8.4 Systemd Unit

**Location:** `/etc/systemd/system/shed-server.service`

```ini
[Unit]
Description=Shed Development Environment Server
After=network.target docker.service
Requires=docker.service

[Service]
Type=simple
User=charlie
Group=charlie
ExecStart=/usr/local/bin/shed-server serve
Restart=on-failure
RestartSec=5

# Environment
Environment=HOME=/home/charlie

[Install]
WantedBy=multi-user.target
```

---

## 9. Error Catalog

### 9.1 HTTP API Errors

All errors return JSON:

```json
{
  "error": {
    "code": "SHED_NOT_FOUND",
    "message": "Shed 'codelens' not found"
  }
}
```

| Code | HTTP Status | Description |
|------|-------------|-------------|
| `SHED_NOT_FOUND` | 404 | Shed does not exist |
| `SHED_ALREADY_EXISTS` | 409 | Shed with this name exists |
| `SHED_ALREADY_RUNNING` | 409 | Shed is already running |
| `SHED_ALREADY_STOPPED` | 409 | Shed is already stopped |
| `INVALID_SHED_NAME` | 400 | Name contains invalid characters |
| `CLONE_FAILED` | 500 | Git clone failed |
| `DOCKER_ERROR` | 500 | Docker operation failed |
| `INTERNAL_ERROR` | 500 | Unexpected server error |

### 9.2 CLI Error Messages

Errors should be actionable with suggested next steps:

```
Error: shed "codelens" not found on mini-desktop

Try:
  shed list --all       # Find which server has it
  shed create codelens  # Create a new shed
```

```
Error: shed "codelens" already exists on mini-desktop

Try:
  shed console codelens  # Connect to existing shed
  shed delete codelens   # Delete and recreate
```

```
Error: cannot connect to server "mini-desktop"

Try:
  ping mini-desktop.tailnet.ts.net  # Check network
  shed server list                  # Verify configuration
```

```
Error: git clone failed - repository not found or access denied

Check:
  - Repository exists: github.com/charliek/codelens
  - SSH key added to GitHub
  - ssh -T git@github.com works on server
```

### 9.3 SSH Connection Errors

| Scenario | Behavior |
|----------|----------|
| Container not found | Print "Shed 'x' not found", exit 1 |
| Container won't start | Print "Failed to start shed 'x': <reason>", exit 1 |
| Docker exec fails | Print error, exit with docker's exit code |

---

## 10. Build & Release

### 10.1 Go Module

**go.mod:**
```
module github.com/charliek/shed

go 1.24

require (
    github.com/docker/docker v...
    github.com/gliderlabs/ssh v...
    github.com/go-chi/chi/v5 v...
    github.com/spf13/cobra v...
    gopkg.in/yaml.v3 v...
)
```

### 10.2 Build Targets

**Makefile:**

```makefile
.PHONY: build build-cli build-server test release clean

VERSION := $(shell git describe --tags --always --dirty)
LDFLAGS := -ldflags "-X github.com/charliek/shed/internal/version.Version=$(VERSION)"

build: build-cli build-server

build-cli:
	go build $(LDFLAGS) -o bin/shed ./cmd/shed

build-server:
	go build $(LDFLAGS) -o bin/shed-server ./cmd/shed-server

test:
	go test ./...

release:
	GOOS=darwin GOARCH=amd64 go build $(LDFLAGS) -o dist/shed-darwin-amd64 ./cmd/shed
	GOOS=darwin GOARCH=arm64 go build $(LDFLAGS) -o dist/shed-darwin-arm64 ./cmd/shed
	GOOS=linux GOARCH=amd64 go build $(LDFLAGS) -o dist/shed-linux-amd64 ./cmd/shed
	GOOS=linux GOARCH=arm64 go build $(LDFLAGS) -o dist/shed-linux-arm64 ./cmd/shed
	GOOS=linux GOARCH=amd64 go build $(LDFLAGS) -o dist/shed-server-linux-amd64 ./cmd/shed-server
	GOOS=linux GOARCH=arm64 go build $(LDFLAGS) -o dist/shed-server-linux-arm64 ./cmd/shed-server

clean:
	rm -rf bin/ dist/
```

### 10.3 Release Artifacts

Each release includes:

```
shed-v1.0.0-darwin-amd64.tar.gz
shed-v1.0.0-darwin-arm64.tar.gz
shed-v1.0.0-linux-amd64.tar.gz
shed-v1.0.0-linux-arm64.tar.gz
shed-server-v1.0.0-linux-amd64.tar.gz
shed-server-v1.0.0-linux-arm64.tar.gz
checksums.txt
```

### 10.4 Install Script

**scripts/install-cli.sh:**

```bash
#!/bin/bash
set -e

REPO="charliek/shed"
INSTALL_DIR="${INSTALL_DIR:-/usr/local/bin}"

# Detect OS and architecture
OS=$(uname -s | tr '[:upper:]' '[:lower:]')
ARCH=$(uname -m)
case $ARCH in
    x86_64) ARCH="amd64" ;;
    aarch64|arm64) ARCH="arm64" ;;
    *) echo "Unsupported architecture: $ARCH"; exit 1 ;;
esac

# Get latest version
VERSION=$(curl -s "https://api.github.com/repos/${REPO}/releases/latest" | grep '"tag_name"' | cut -d'"' -f4)

# Download and install
URL="https://github.com/${REPO}/releases/download/${VERSION}/shed-${VERSION}-${OS}-${ARCH}.tar.gz"
echo "Downloading shed ${VERSION} for ${OS}/${ARCH}..."
curl -sL "$URL" | tar xz -C "$INSTALL_DIR"
echo "✓ Installed shed to ${INSTALL_DIR}/shed"
```

---

## 11. Testing Strategy

### 11.1 Test Coverage Expectations

All packages should have meaningful test coverage. Target 70%+ coverage for core logic.

| Package | Unit Test Focus |
|---------|-----------------|
| `internal/config` | YAML parsing, path expansion, defaults |
| `internal/sshconfig` | SSH config parsing, block detection, diff generation |
| `internal/docker` | Mock Docker client, container/volume naming |
| `internal/api` | HTTP handlers with mock Docker backend |
| `internal/sshd` | Session routing, username parsing |

### 11.2 Unit Tests

**Config Parsing:**
```go
func TestServerConfigDefaults(t *testing.T)
func TestServerConfigPathExpansion(t *testing.T)
func TestClientConfigLoadSave(t *testing.T)
func TestClientConfigShedCache(t *testing.T)
```

**SSH Config Management:**
```go
func TestSSHConfigParseExisting(t *testing.T)
func TestSSHConfigDetectManagedBlock(t *testing.T)
func TestSSHConfigGenerateEntry(t *testing.T)
func TestSSHConfigComputeDiff(t *testing.T)
func TestSSHConfigWritePreservesUserContent(t *testing.T)
```

**Docker Operations:**
```go
func TestContainerNaming(t *testing.T)
func TestVolumeNaming(t *testing.T)
func TestLabelGeneration(t *testing.T)
func TestMountConfigBuilding(t *testing.T)
```

**API Handlers:**
```go
func TestCreateShedValidation(t *testing.T)
func TestCreateShedAlreadyExists(t *testing.T)
func TestListShedsFiltering(t *testing.T)
func TestDeleteShedNotFound(t *testing.T)
```

### 11.3 Integration Tests

Integration tests run against a real shed-server with a real Docker daemon. They verify end-to-end behavior.

**Test Structure:**
```go
// integration/integration_test.go

func TestMain(m *testing.M) {
    // Start shed-server on test ports
    // Run tests
    // Cleanup all shed-* containers and volumes
}

func TestCreateAndDeleteShed(t *testing.T)
func TestCreateShedWithRepo(t *testing.T)
func TestConsoleConnection(t *testing.T)
func TestExecCommand(t *testing.T)
func TestStopAndStartShed(t *testing.T)
func TestMultipleConcurrentSessions(t *testing.T)
func TestAutoStartOnConsole(t *testing.T)
```

**Test Isolation:**
- Each test uses unique shed names (e.g., `test-{testname}-{timestamp}`)
- Tests clean up their own containers/volumes
- `TestMain` does final cleanup of any orphaned `shed-test-*` resources

**Running Integration Tests:**
```bash
# Requires Docker running locally
make test-integration

# Or with verbose output
go test -v -tags=integration ./integration/...
```

### 11.4 Single-Machine Development Setup

Developers can run and test everything on a single machine using localhost.

**Local Development Workflow:**

```bash
# Terminal 1: Run the server
cd shed
go run ./cmd/shed-server serve --config ./configs/server.dev.yaml

# Terminal 2: Use the CLI against localhost
go run ./cmd/shed -- server add localhost
go run ./cmd/shed -- create test-shed
go run ./cmd/shed -- console test-shed
```

**Development Server Config:**

```yaml
# configs/server.dev.yaml

name: localhost-dev

http_port: 8080
ssh_port: 2222

default_image: shed-base:latest

# For local dev, credentials can point to your actual home dir
# or use test fixtures
credentials:
  git_ssh:
    source: ~/.ssh
    target: /root/.ssh
    readonly: true
  git_config:
    source: ~/.gitconfig
    target: /root/.gitconfig
    readonly: true

# Optional: skip claude/opencode mounts for basic testing
# credentials: {}

env_file: ""  # Empty for dev, no env vars injected

log_level: debug
```

**Development Client Config:**

After running `shed server add localhost`:

```yaml
# ~/.shed/config.yaml (auto-generated)

servers:
  localhost-dev:
    host: localhost
    http_port: 8080
    ssh_port: 2222

default_server: localhost-dev
```

**Makefile Targets for Development:**

```makefile
# Run server in development mode
dev-server:
	go run ./cmd/shed-server serve --config ./configs/server.dev.yaml

# Run CLI commands during development
dev-cli:
	go run ./cmd/shed $(ARGS)

# Example: make dev-cli ARGS="list"

# Run all tests
test:
	go test ./...

# Run integration tests (requires Docker)
test-integration:
	go test -v -tags=integration ./integration/...

# Quick smoke test: create, console, delete
test-smoke: build
	./scripts/smoke-test.sh
```

**Smoke Test Script:**

```bash
#!/bin/bash
# scripts/smoke-test.sh
set -e

SHED_NAME="smoke-test-$$"

echo "=== Smoke Test ==="

# Ensure server is running (or start it)
if ! curl -s http://localhost:8080/api/info > /dev/null; then
    echo "Starting server..."
    ./bin/shed-server serve --config ./configs/server.dev.yaml &
    SERVER_PID=$!
    sleep 2
    trap "kill $SERVER_PID 2>/dev/null" EXIT
fi

echo "Creating shed..."
./bin/shed create $SHED_NAME

echo "Listing sheds..."
./bin/shed list | grep $SHED_NAME

echo "Executing command..."
./bin/shed exec $SHED_NAME "echo hello from shed"

echo "Stopping shed..."
./bin/shed stop $SHED_NAME

echo "Starting shed..."
./bin/shed start $SHED_NAME

echo "Deleting shed..."
./bin/shed delete $SHED_NAME --force

echo "=== Smoke Test Passed ==="
```

### 11.5 Test Environment Variables

```bash
# Force specific ports for testing (avoid conflicts)
SHED_HTTP_PORT=18080
SHED_SSH_PORT=12222

# Use test config
SHED_CONFIG=./configs/server.test.yaml

# Enable debug logging in tests
SHED_LOG_LEVEL=debug
```

### 11.6 CI Pipeline Considerations

For CI (GitHub Actions, etc.):

```yaml
# .github/workflows/test.yml
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      
      - uses: actions/setup-go@v5
        with:
          go-version: '1.24'
      
      - name: Unit Tests
        run: make test
      
      - name: Build
        run: make build
      
      - name: Build Test Image
        run: docker build -t shed-base:latest ./images/shed-base
      
      - name: Integration Tests
        run: make test-integration
```

### 11.7 Manual Testing Checklist

Before release, manually verify:

- [ ] `shed server add` discovers and configures new server
- [ ] `shed create` with and without `--repo` flag
- [ ] `shed console` attaches correctly, terminal resize works
- [ ] `shed console` auto-starts stopped containers
- [ ] `shed exec` runs commands and returns output
- [ ] `shed list` shows correct status across servers
- [ ] `shed list --all` queries multiple servers
- [ ] `shed stop` and `shed start` work correctly
- [ ] `shed delete` removes container and volume
- [ ] `shed delete --keep-volume` preserves data
- [ ] `shed ssh-config --install --dry-run` shows correct diff
- [ ] `shed ssh-config --install` updates ~/.ssh/config correctly
- [ ] Cursor connects via generated SSH config
- [ ] Multiple simultaneous `shed console` sessions to same shed
- [ ] Server survives restart, reconnects to existing containers

---

## 12. Post-MVP Roadmap

### 12.1 Provisioning Hooks

Support `.shed/config.yaml` or `.devcontainer/devcontainer.json` in repos:

```yaml
# .shed/config.yaml
image: shed-base:latest

onCreateCommand: |
  npm install
  
postStartCommand: |
  ./scripts/start-services.sh
  
env:
  DATABASE_URL: postgres://localhost/dev
```

### 12.2 Auto Feature Branches

Automatically create Git branches on shed creation:

```yaml
# ~/.shed/config.yaml
defaults:
  auto_branch: true
  branch_pattern: "shed/{name}-{date}"
  push_branch: false
```

### 12.3 Container Resource Limits

Configure CPU/memory limits:

```yaml
# server.yaml
resources:
  cpu_limit: "4"
  memory_limit: "8g"
```

### 12.4 Checkpoint/Restore

Save and restore shed state:

```bash
shed checkpoint codelens --name "before-refactor"
shed restore codelens --checkpoint "before-refactor"
shed checkpoints codelens  # list checkpoints
```

Implementation: `docker commit` + tagged images

### 12.5 Web Dashboard

Simple web UI for monitoring:
- List all sheds across servers
- Start/stop controls
- Basic logs view
- Mobile-friendly

### 12.6 Multi-User Support

- Per-user authentication
- User-scoped sheds
- Credential injection per user

---

## Appendix A: Example Session

Complete workflow example:

```bash
# Initial setup (once)
curl -fsSL https://raw.githubusercontent.com/charliek/shed/main/scripts/install-cli.sh | bash
shed server add mini-desktop.tailnet.ts.net

# On the server (once)
git clone https://github.com/charliek/shed.git
cd shed && ./scripts/build-image.sh
sudo shed-server install

# Create a development environment
shed create codelens --repo charliek/codelens
# ✓ Creating shed "codelens" on mini-desktop...
# ✓ Cloning charliek/codelens...
# ✓ Shed ready

# Connect and start working
shed console codelens
# Now inside the container:
$ cd /workspace/codelens
$ claude --dangerously-skip-permissions
# > Implement the user authentication module...
# [Claude Code working...]
# Press Ctrl-D to disconnect, work continues

# Check on it later
shed list
# SERVER          NAME          STATUS     CREATED
# mini-desktop    codelens      running    2 hours ago

# Reconnect
shed console codelens
# Back to your session

# Set up Cursor
shed ssh-config --all --install
# ✓ Updated ~/.ssh/config

# In Cursor: Remote-SSH → Connect → shed-codelens

# Clean up when done
shed delete codelens
```

---

## Appendix B: Cursor Integration Guide

### B.1 Setup

1. Install and configure shed CLI
2. Create a shed and run `shed ssh-config --all --install`
3. In Cursor: Cmd+Shift+P → "Remote-SSH: Connect to Host"
4. Select `shed-{name}` from the list

### B.2 How It Works

Cursor uses the SSH config entry to connect:

```
Host shed-codelens
    HostName mini-desktop.tailnet.ts.net
    Port 2222
    User codelens
    UserKnownHostsFile ~/.shed/known_hosts
```

The shed-server's SSH server receives the connection, maps `User: codelens` to container `shed-codelens`, and establishes a shell session.

### B.3 Features That Work

- File explorer (full /workspace access)
- Integrated terminal
- Git integration
- Extensions (installed in remote)
- Search across files
- Debugging (with appropriate extensions)

### B.4 Troubleshooting

**Connection timeout:**
- Ensure shed is running: `shed list`
- Check Tailscale connection: `ping mini-desktop.tailnet.ts.net`
- Verify SSH port: `ssh -p 2222 codelens@mini-desktop.tailnet.ts.net`

**Host key verification failed:**
- Re-run `shed server add <host>` to refresh keys
- Or manually update `~/.shed/known_hosts`

---

*End of Specification*
