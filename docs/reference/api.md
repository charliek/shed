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
| GET | `/api/images` | List available image variants |
| DELETE | `/api/images/{name}` | Delete a cached image |
| POST | `/api/images/prune` | Prune unused cached images |
| GET | `/api/sheds/{name}/connect/{port}` | TCP tunnel via HTTP upgrade |
| GET | `/api/plugins/listeners` | List active extension listeners |
| GET | `/api/plugins/listeners/{ns}/messages` | Subscribe to namespace (SSE) |
| POST | `/api/plugins/listeners/{ns}/respond` | Respond to a plugin message |
| GET | `/api/plugins/sheds` | List sheds with active message channels |

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
  "backend": "vz"
}
```

The `backend` field reflects the resolved backend for this server (`vz` or `firecracker`). When the server is configured with `default_backend: detect`, this field shows the auto-detected value.

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
      "backend": "vz",
      "ip_address": "192.168.64.2",
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
| `image` | No | Server config | Image variant name (see [Image Variants](images.md)) |
| `backend` | No | Server default | Backend to use: `firecracker`, `vz`, or `detect` |
| `local_dir` | No | null | Absolute path to host directory to mount as workspace (mutually exclusive with `repo`) |
| `cpus` | No | Backend default | Number of vCPUs |
| `memory_mb` | No | Backend default | Memory in MB |

**Response (201 Created):**

```json
{
  "name": "codelens",
  "status": "running",
  "created_at": "2026-01-20T10:30:00Z",
  "repo": "charliek/codelens",
  "backend": "vz",
  "ip_address": "192.168.64.2",
  "cpus": 2,
  "memory_mb": 4096
}
```

#### SSE Progress Streaming

The create endpoint supports real-time progress streaming via Server-Sent Events. To opt in, set the `Accept` header:

```http
Accept: text/event-stream
```

The server streams events as the shed is created:

```text
event: progress
data: {"message":"Pulling image shed-base:latest..."}

event: progress
data: {"message":"Creating VM..."}

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
| 500 | Backend or clone failure |

### GET /api/sheds/{name}

Gets details for a specific shed.

**Response (200 OK):**

```json
{
  "name": "codelens",
  "status": "running",
  "created_at": "2026-01-20T10:30:00Z",
  "repo": "charliek/codelens",
  "backend": "vz"
}
```

**Errors:**

| Code | Description |
|------|-------------|
| 404 | Shed not found |

### DELETE /api/sheds/{name}

Deletes a shed and its data.

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

## Images

### GET /api/images

Returns available image variants across all backends.

**Response:**

```json
{
  "images": [
    {
      "name": "base",
      "path": "/Users/user/Library/Application Support/shed/vz/base-rootfs.ext4",
      "docker_ref": "ghcr.io/charliek/shed-vz-base:{version}",
      "size_bytes": 2147483648,
      "source": "config",
      "cached": true
    },
    {
      "name": "custom",
      "path": "/Users/user/Library/Application Support/shed/vz/custom-rootfs.ext4",
      "size_bytes": 3221225472,
      "source": "discovered",
      "cached": true
    }
  ]
}
```

| Field | Type | Description |
|-------|------|-------------|
| `name` | string | Image variant name |
| `path` | string | Local ext4 file path (empty if not cached) |
| `docker_ref` | string | Docker image reference (empty for local-only images) |
| `size_bytes` | int | File size in bytes (0 if not cached) |
| `source` | string | `config` (from server config) or `discovered` (found in images_dir) |
| `cached` | bool | Whether the ext4 file exists locally |

### DELETE /api/images/{name}

Deletes a cached image by name. Removes the ext4 rootfs and source sidecar files but preserves the lock file.

**Response (204 No Content)**

**Errors:**

| Code | Description |
|------|-------------|
| 404 | Image not found |
| 409 | Image is referenced by config |

### POST /api/images/prune

Removes cached images not referenced by config or any existing shed.

**Query Parameters:**

| Param | Default | Description |
|-------|---------|-------------|
| `dry_run` | `false` | Return candidates without deleting |

**Response (200 OK):**

```json
{
  "deleted": [
    {
      "name": "old-variant",
      "path": "/Users/user/Library/Application Support/shed/vz/old-variant-rootfs.ext4",
      "docker_ref": "ghcr.io/example/old:v1",
      "size_bytes": 2147483648,
      "cached": true
    }
  ]
}
```

The `deleted` array contains the images that were removed (or would be removed if `dry_run=true`).

## System

### GET /api/system/df

Returns disk usage information for the server: image cache, per-instance rootfs copies and console logs, kernel/initrd, and orphan sidecar files.

**Response (200 OK):**

```json
{
  "server_name": "prod-mac",
  "backend": "vz",
  "generated_at": "2026-04-20T11:23:45Z",
  "images": [
    {
      "name": "default",
      "path": "/var/lib/shed/vz/default-rootfs.ext4",
      "docker_ref": "ghcr.io/example/default:v1",
      "size": {"logical_bytes": 5368709120, "physical_bytes": 4831838208}
    },
    {
      "name": "_base",
      "path": "/var/lib/shed/vz/_base-rootfs.ext4",
      "size": {"logical_bytes": 5368709120, "physical_bytes": 0},
      "is_base": true
    }
  ],
  "kernel": {
    "path": "/var/lib/shed/vz/vmlinux",
    "size": {"logical_bytes": 8388608, "physical_bytes": 8388608},
    "kind": "kernel"
  },
  "initrd": {
    "path": "/var/lib/shed/vz/initrd",
    "size": {"logical_bytes": 102400, "physical_bytes": 102400},
    "kind": "initrd"
  },
  "sheds": [
    {
      "name": "api-dev",
      "status": "running",
      "image": "default",
      "rootfs": {
        "path": "/var/lib/shed/vz/instances/api-dev/rootfs.ext4",
        "size": {"logical_bytes": 2147483648, "physical_bytes": 2147483648},
        "kind": "rootfs"
      },
      "console_log": {
        "path": "/var/lib/shed/vz/instances/api-dev/console.log",
        "size": {"logical_bytes": 819200, "physical_bytes": 819200},
        "kind": "console_log"
      },
      "other_files": [],
      "total": {"logical_bytes": 2148302848, "physical_bytes": 2148302848}
    }
  ],
  "orphans": [
    {
      "path": "/var/lib/shed/vz/stale-rootfs.ext4.lock",
      "size": {"logical_bytes": 0, "physical_bytes": 0},
      "kind": "lock"
    }
  ],
  "totals": {
    "images":  {"logical_bytes": 10737418240, "physical_bytes": 4840226816},
    "sheds":   {"logical_bytes": 2148302848,  "physical_bytes": 2148302848},
    "orphans": {"logical_bytes": 0,           "physical_bytes": 0},
    "all":     {"logical_bytes": 12885721088, "physical_bytes": 6988529664}
  },
  "notes": [
    "physical bytes may overcount shared extents on APFS (clonefile) or hardlinks"
  ]
}
```

**Field notes:**

- `backend` is `"vz"`, `"firecracker"`, or `"none"` (the last when the native backend isn't available on this platform).
- `console_log` is always absent on Firecracker (the FC SDK writes to stderr, not a per-instance file).
- `initrd` is VZ-only — Firecracker has no initrd.
- `physical_bytes` comes from `stat.Blocks * 512`. Files that share extents via clonefile/FICLONE or hardlinks may have those bytes counted against each referencing file, inflating sums.
- `is_base` marks the runtime-managed `_base-rootfs.ext4` cache.

**Multi-server aggregation:** the server never fans out; `shed system df --all` issues one request per configured server from the client and assembles the per-server results.

### POST /api/system/prune

Runs a disk cleanup pass. Returns a report of what was (or would be) removed or truncated. Dry-run returns the same shape without mutating.

**Query parameters:**

| Param | Default | Description |
|-------|---------|-------------|
| `scope` | (default scope) | Repeatable. One of `images`, `instances`, `logs`, `orphans`. When omitted, the backend applies the default: `images + instances + orphans`. Unknown values return 400. |
| `dry_run` | `false` | If true, return candidates without mutating. |
| `until` | `72h` | Go `time.Duration` string. Stopped instances whose `mtime(metadata.json)` is younger than `now - until` are skipped. `0s` = any age. Negative values return 400. |
| `log_tail_bytes` | `0` | Size console logs are truncated to when the `logs` scope is active. `0` uses the server default (5 MiB). |

**Scope rules:**

- `images` — runs `vmimage.Manager.PruneImages`; refuses images referenced by config or any surviving instance.
- `instances` — deletes stopped sheds older than `until`. Uses `Client.DeleteShed` internally so TAP devices (Firecracker) and credential state are cleaned up correctly. Running sheds are never pruned.
- `orphans` — removes `.tmp`/`.source` sidecars whose parent rootfs is absent, gated by a non-blocking `flock()` probe on the canonical `.lock` and a 1-hour minimum age for `.tmp` files. The canonical `.lock` file itself is always preserved.
- `logs` — Firecracker: no-op (FC has no per-instance console log; each shed gets a `SkippedItem`). VZ: truncates `console.log` in place to `log_tail_bytes`.

**Internal ordering (within a single Prune call):**

1. Snapshot `mtime(metadata.json)` for every instance **before** calling `ListSheds` (which can refresh mtime via its staleness re-check).
2. Collect candidates for instances, orphans, and images (image dry-run uses `inUseImageNamesExcept(<candidate sheds>)` to simulate the post-instance-delete state).
3. If `dry_run`, return. Otherwise: delete instances first so their image references drop from `inUseImageNames`, then prune images, then sweep orphans, then truncate logs.

**Response (200 OK):**

```json
{
  "dry_run": false,
  "server_name": "prod-mac",
  "scope": ["images", "instances", "orphans"],
  "until": "72h0m0s",
  "items": [
    {
      "kind": "instance",
      "name": "api-old",
      "action": "deleted",
      "freed": {"logical_bytes": 2147483648, "physical_bytes": 2147483648},
      "reason": "deleted (stopped 5d)"
    },
    {
      "kind": "image",
      "name": "old-variant",
      "path": "/var/lib/shed/vz/old-variant-rootfs.ext4",
      "action": "deleted",
      "freed": {"logical_bytes": 5368709120, "physical_bytes": 5368709120}
    },
    {
      "kind": "tmp",
      "path": "/var/lib/shed/vz/stale-rootfs.ext4.tmp",
      "action": "deleted",
      "freed": {"logical_bytes": 4096, "physical_bytes": 4096}
    }
  ],
  "skipped": [
    {"kind": "instance", "name": "api-dev", "reason": "cannot prune running shed"},
    {"kind": "instance", "name": "api-test", "reason": "too recent (3h < 72h)"},
    {"kind": "tmp", "path": "/var/lib/shed/vz/live-rootfs.ext4.tmp", "reason": "lock held (conversion in progress)"}
  ],
  "notes": [
    "physical bytes are attributed (stat.Blocks*512); clonefile/FICLONE clones and hardlinks may report bytes that won't actually be reclaimed"
  ],
  "totals": {
    "freed": {"logical_bytes": 7516196864, "physical_bytes": 7516196864},
    "items": 3
  }
}
```

**Error responses:**

| Status | When |
|--------|------|
| 400 | Invalid `dry_run`, `until`, `log_tail_bytes`, or an unknown `scope` value |
| 500 | Backend error during prune |
| 501 | Backend does not support prune (e.g., the VZ stub on non-darwin hosts) |

**Timeouts:** the CLI uses a 10-minute client timeout for this endpoint since large fleets can exceed the default 30-second window used by list/get endpoints.

## Connect API

The Connect API provides TCP tunnels into shed VMs via HTTP upgrade. This is the foundation for port forwarding (tunnels) and the proxy extension.

### GET /api/sheds/{name}/connect/{port}

Opens a raw TCP tunnel to a port inside a running shed VM.

**Request headers:**

```
Connection: Upgrade
Upgrade: shed-tcp
```

**Success response:** `101 Switching Protocols`

After the 101 response, the connection becomes a bidirectional byte stream to the target port inside the VM. The server uses `DialService` internally — for VZ this goes through the vsock TCP proxy (port 1028), for Firecracker it connects directly to the bridge IP.

**Error responses:**

| Status | Code | Description |
|--------|------|-------------|
| 400 | `INVALID_PORT` | Port must be 1-65535 |
| 404 | `SHED_NOT_FOUND` | Shed does not exist |
| 502 | `CONNECT_FAILED` | Could not connect to the port (service not running) |
| 503 | `SHED_NOT_RUNNING` | Shed is not running |

**Security:** The Connect API has the same access control as other API endpoints — network-level only (Tailscale, firewall). It connects to the VM's loopback interface and does not support arbitrary destinations.

**Consumers:**

- `shed tunnels` CLI uses Connect API for port forwarding
- The proxy extension (`shed-ext-proxy`) uses Connect API for reverse proxying
- SSH port forwarding (`ssh -L`) uses `DialService` internally via `handleDirectTCPIP`

## Extensions

Extension endpoints enable plugin communication between VMs and host processes. See [Extensions](extensions.md) for the full guide.

### GET /api/plugins/listeners

Lists all active extension listeners.

**Response (200 OK):**

```json
{
  "listeners": [
    {"namespace": "op", "created_at": "2026-03-29T12:00:00Z"}
  ]
}
```

### GET /api/plugins/listeners/{namespace}/messages

Subscribes to a namespace via SSE. One listener per namespace.

**Headers:** `Accept: text/event-stream`

**Errors:**

| Code | Status | Description |
|------|--------|-------------|
| `NAMESPACE_RESERVED` | 403 | `system:*` namespaces cannot be registered |
| `NAMESPACE_ALREADY_REGISTERED` | 409 | Namespace already has a listener |

### POST /api/plugins/listeners/{namespace}/respond

Sends a response back to a shed. The envelope must include `shed.name`.

**Errors:**

| Code | Status | Description |
|------|--------|-------------|
| `MISSING_SHED` | 400 | Envelope missing `shed.name` |
| `SHED_NOT_CONNECTED` | 404 | Target shed not connected |

### GET /api/plugins/sheds

Lists sheds with active message channels.

**Response (200 OK):**

```json
{
  "sheds": [
    {"name": "my-dev", "backend": "vz", "server": "mini"}
  ]
}
```
