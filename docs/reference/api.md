# HTTP API

The `shed-server` exposes a REST API for managing sheds.

**Base URL:** `http://{host}:8080/api`

!!! warning "No Authentication"
    The API has no built-in authentication. Security relies on network-level access control (e.g., Tailscale, firewall rules). Only expose the API on trusted networks.

## Endpoints

| Method | Endpoint | Description |
|--------|----------|-------------|
| GET | `/api/info` | Server metadata |
| GET | `/api/ssh-host-key` | SSH host public key |
| GET | `/api/sheds` | List all sheds |
| POST | `/api/sheds` | Create a shed |
| GET | `/api/sheds/{name}` | Get shed details |
| DELETE | `/api/sheds/{name}` | Delete a shed |
| POST | `/api/sheds/{name}/start` | Start a shed |
| POST | `/api/sheds/{name}/stop` | Stop a shed |
| GET | `/api/sheds/{name}/sessions` | List tmux sessions in shed |
| DELETE | `/api/sheds/{name}/sessions/{session}` | Kill a tmux session |
| GET | `/api/sessions` | List all sessions across sheds |

## Server Info

### GET /api/info

Returns server metadata and capabilities.

**Response:**

```json
{
  "name": "mini-desktop",
  "version": "1.0.0",
  "ssh_port": 2222,
  "http_port": 8080,
  "default_backend": "docker",
  "enabled_backends": ["docker"]
}
```

### GET /api/ssh-host-key

Returns the server's SSH host public key.

**Response:**

```json
{
  "host_key": "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAA..."
}
```

## Shed Management

### GET /api/sheds

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
      "backend": "docker",
      "container_id": "abc123...",
      "ip_address": "172.17.0.2",
      "cpus": 2,
      "memory_mb": 4096
    }
  ]
}
```

Fields like `ip_address`, `cpus`, `memory_mb`, `pid`, `rootfs_path`, and `local_dir` are omitted when empty or zero.

**Status values:** `running`, `stopped`, `starting`, `error`

### POST /api/sheds

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
| `name` | Yes | - | Shed name (alphanumeric + hyphens) |
| `repo` | No | null | Repository to clone (`owner/repo` shorthand or full URL) |
| `image` | No | Server config | Base Docker image |
| `backend` | No | Server default | Backend to use: `docker`, `firecracker`, or `vz` |
| `local_dir` | No | null | Absolute path to host directory to mount as workspace (mutually exclusive with `repo`) |
| `cpus` | No | Backend default | Number of vCPUs (firecracker/vz only) |
| `memory_mb` | No | Backend default | Memory in MB (firecracker/vz only) |

**Response (201 Created):**

```json
{
  "name": "codelens",
  "status": "running",
  "created_at": "2026-01-20T10:30:00Z",
  "repo": "charliek/codelens",
  "backend": "docker",
  "container_id": "abc123...",
  "ip_address": "172.17.0.2",
  "cpus": 2,
  "memory_mb": 4096
}
```

#### SSE Progress Streaming

The create endpoint supports real-time progress streaming via Server-Sent Events. To opt in, set the `Accept` header:

```
Accept: text/event-stream
```

The server streams events as the shed is created:

```
event: progress
data: {"message":"Pulling image shed-base:latest..."}

event: progress
data: {"message":"Creating container..."}

event: complete
data: {"name":"codelens","status":"running",...}
```

| Event | Description |
|-------|-------------|
| `progress` | Status update with a `message` field |
| `complete` | Final shed object (same shape as the synchronous response) |
| `error` | Error object with `code` and `message` fields |

Without the `Accept: text/event-stream` header, the endpoint behaves synchronously and returns the shed object directly.

**Errors:**

| Code | Description |
|------|-------------|
| 400 | Invalid name format |
| 400 | Invalid repository URL |
| 400 | Invalid local directory path |
| 409 | Shed already exists |
| 500 | Docker or clone failure |

### GET /api/sheds/{name}

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

| Code | Description |
|------|-------------|
| 404 | Shed not found |

### DELETE /api/sheds/{name}

Deletes a shed and its data.

**Query Parameters:**

| Param | Default | Description |
|-------|---------|-------------|
| `keep_volume` | false | Preserve workspace volume |

**Response (204 No Content)**

**Errors:**

| Code | Description |
|------|-------------|
| 404 | Shed not found |

### POST /api/sheds/{name}/start

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

| Code | Description |
|------|-------------|
| 404 | Shed not found |
| 409 | Shed already running |

### POST /api/sheds/{name}/stop

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

| Code | Description |
|------|-------------|
| 404 | Shed not found |
| 409 | Shed already stopped |

## Session Management

### GET /api/sheds/{name}/sessions

Lists tmux sessions in a shed.

**Response (200 OK):**

```json
{
  "sessions": [
    {
      "name": "default",
      "shed_name": "codelens",
      "created_at": "2026-01-20T10:30:00Z",
      "attached": true,
      "window_count": 1
    }
  ]
}
```

**Errors:**

| Code | Description |
|------|-------------|
| 404 | Shed not found |
| 409 | Shed not running |

### DELETE /api/sheds/{name}/sessions/{session}

Terminates a tmux session.

**Response (204 No Content)**

**Errors:**

| Code | Description |
|------|-------------|
| 404 | Shed or session not found |
| 409 | Shed not running |

### GET /api/sessions

Lists all tmux sessions across all running sheds.

**Response (200 OK):**

```json
{
  "sessions": [
    {
      "name": "default",
      "shed_name": "codelens",
      "created_at": "2026-01-20T10:30:00Z",
      "attached": true,
      "window_count": 1
    },
    {
      "name": "default",
      "shed_name": "stbot",
      "created_at": "2026-01-19T14:00:00Z",
      "attached": false,
      "window_count": 1
    }
  ]
}
```
