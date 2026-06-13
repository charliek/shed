# HTTP API

The `shed-server` exposes a REST API for managing sheds.

**Base URL:** `http://{host}:8080/api` (or `https://{host}:8443/api` with TLS)

!!! note "Authentication is optional"
    By default the API has no authentication — security relies on a trusted
    network (Tailscale, firewall rules). When `auth.http.mode: enforce` is set,
    every request requires an `Authorization: Bearer <token>` header of the
    required scope, except the bootstrap endpoints `GET /api/info` and
    `GET /api/ssh-host-key`. The credential bus (`/api/plugins/*`) and the
    Connect tunnel (`/api/sheds/{name}/connect/{port}`) require the
    `credentials` scope. See [Security](security.md) for the full model,
    scoped tokens, and pinned TLS.

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
| POST | `/api/sheds/{name}/reset` | Wipe and recreate the shed's writable upper layer |
| GET | `/api/sheds/{name}/sessions` | List tmux sessions in shed |
| DELETE | `/api/sheds/{name}/sessions/{session}` | Kill a tmux session |
| GET | `/api/sessions` | List all sessions across sheds |
| GET | `/api/snapshots` | List snapshots |
| POST | `/api/snapshots` | Create a snapshot from a stopped shed |
| GET | `/api/snapshots/{name}` | Get snapshot details |
| DELETE | `/api/snapshots/{name}` | Delete a snapshot |
| GET | `/api/images` | List installed images (by Docker ref) |
| GET | `/api/images/inspect/{name}` | Inspect a tag or digest (manifest + info) |
| POST | `/api/images/tag` | Point a new tag at an existing digest |
| POST | `/api/images/pull` | Pull a Docker reference into the blob store |
| DELETE | `/api/images/{name}` | Delete a tag (blob preserved for `prune`) |
| POST | `/api/images/prune` | Reclaim unreferenced blobs |
| GET | `/api/system/df` | Disk usage report (image cache, sheds, orphans) |
| POST | `/api/system/prune` | Scoped disk cleanup (dry-run, images/instances/logs/orphans) |
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

Fields like `ip_address`, `cpus`, `memory_mb`, `pid`, `rootfs_path`, `project_mounts`, and `landing_dir` are omitted when empty or zero.

**Status values:** `running`, `stopped`, `starting`, `error`

### POST /api/sheds

Creates a new shed.

**Request:**

```json
{
  "name": "codelens",
  "repo": "charliek/codelens",
  "image": "full"
}
```

| Field | Required | Default | Description |
|-------|----------|---------|-------------|
| `name` | Yes | - | Shed name (alphanumeric + hyphens) |
| `repo` | No | null | Repository to clone (`owner/repo` shorthand or full URL) |
| `image` | No | `default_image` | Docker ref, `image_aliases` name, or local label (see [Images](images.md)) |
| `backend` | No | Server default | Backend to use: `firecracker`, `vz`, or `detect` |
| `local_dir` | No | null | Absolute path to a host directory to mount at `/home/shed/<basename>`; the shed's landing directory. Mutually exclusive with `repo`. |
| `add_dirs` | No | `[]` | Array of absolute host directory paths, each mounted at `/home/shed/<basename>` as a reference sibling. Requires `local_dir`. Mutually exclusive with `repo`. No two mounted directories may share a basename; dotfile-style basenames (leading `.`) are rejected. |
| `cpus` | No | Backend default | Number of vCPUs |
| `memory_mb` | No | Backend default | Memory in MB |
| `from_snapshot` | No | null | Snapshot name to spawn from (mutually exclusive with `image` and `repo`). Provisioning is skipped because the snapshot is already provisioned. |
| `upper_size_bytes` | No | Server default | Per-shed overlay upper size in bytes. Range-validated 1 GiB – 100 GiB. When omitted, the per-backend `upper_size_default` config value applies. |
| `no_provision` | No | `false` | Skip provisioning hooks (repo clone, install hook, first-time auto-sync). |

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

| Code | Error | Description |
|------|-------|-------------|
| 400 | `INVALID_SHED_NAME` | Invalid name format |
| 400 | `INVALID_REPO_URL` | Invalid repository URL |
| 400 | `INVALID_LOCAL_DIR` | Invalid local directory path |
| 400 | `INVALID_REQUEST` | No `image` specified and `default_image` is not configured on the chosen backend, or `upper_size_bytes` is outside the 1–100 GiB range, or `from_snapshot` was passed together with `image`/`repo`. |
| 409 | `SHED_ALREADY_EXISTS` | Shed already exists |
| 500 | `CLONE_FAILED` / `BACKEND_ERROR` | Backend or clone failure |

### GET /api/sheds/{name}

Gets details for a specific shed.

**Response (200 OK):**

```json
{
  "name": "codelens",
  "status": "running",
  "created_at": "2026-01-20T10:30:00Z",
  "repo": "charliek/codelens",
  "backend": "vz",
  "landing_dir": "/home/shed/codelens",
  "project_mounts": [
    {
      "source": "/Users/alice/projects/codelens",
      "target": "/home/shed/codelens",
      "readonly": false
    }
  ]
}
```

| Field | Type | Description |
|-------|------|-------------|
| `landing_dir` | string | Directory an interactive login lands in: `/home/shed` by default, the cloned repo path for `repo`, or the primary mount target for `local_dir`. Omitted when it is the default home. |
| `project_mounts` | array | Host directories mounted into the guest via `local_dir`/`add_dirs`. Each entry is `{source, target, readonly}`. Omitted when the shed has no project mounts. |

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

### POST /api/sheds/{name}/reset

Wipes and recreates the shed's per-shed writable overlay upper (the shed
user's home directory, including any repo cloned via `repo`). The shared lower
image is untouched, and host-mounted `local_dir`/`add_dirs` directories
(outside the overlay) are unaffected. The shed must be stopped first.

**Response (200 OK):** the (still-stopped) Shed object, same shape as
`GET /api/sheds/{name}`.

**Errors:**

| Code | Error | Description |
|------|-------|-------------|
| 404 | `SHED_NOT_FOUND` | Shed does not exist |
| 409 | `SHED_NOT_STOPPED` | Shed must be stopped before reset |

## Snapshots

### GET /api/snapshots

Lists all snapshots managed by this server. Each snapshot's `lower_cached`
field is recomputed at read time from the local blob store; it is never
persisted to `snapshot.json`.

**Response (200 OK):**

```json
{
  "snapshots": [
    {
      "version": 2,
      "name": "post-migration",
      "backend": "vz",
      "source_shed": "api-dev",
      "source_image": "full",
      "size_bytes": 5368709120,
      "created_at": "2026-05-10T12:00:00Z",
      "lower_digest": "sha256:abc123...",
      "lower_cached": true
    }
  ]
}
```

### POST /api/snapshots

Creates a snapshot from a stopped shed. The snapshot captures the writable
upper (the home directory, including any cloned repo) but not host-mounted
`local_dir`/`add_dirs` contents. Backend-emitted warnings (e.g., the source
used `local_dir`, so those mounted directories are not captured) are returned
alongside the snapshot rather than failing the request.

**Request:**

```json
{
  "name": "post-migration",
  "source_shed": "api-dev",
  "comment": "after the schema bump"
}
```

| Field | Required | Description |
|-------|----------|-------------|
| `name` | Yes | Snapshot name (alphanumeric + hyphens) |
| `source_shed` | Yes | Source shed name. Must be stopped. |
| `comment` | No | Free-form note attached to the snapshot |

**Response (201 Created):**

```json
{
  "snapshot": {
    "version": 2,
    "name": "post-migration",
    "backend": "vz",
    "source_shed": "api-dev",
    "lower_digest": "sha256:abc123...",
    "lower_cached": true,
    "size_bytes": 5368709120,
    "created_at": "2026-05-10T12:00:00Z"
  },
  "warnings": [
    "source shed used --local-dir; host-mounted directories are not captured in the snapshot"
  ]
}
```

**Errors:**

| Code | Error | Description |
|------|-------|-------------|
| 400 | `INVALID_SNAPSHOT_NAME` / `INVALID_SHED_NAME` | Name validation failed |
| 404 | `SHED_NOT_FOUND` | Source shed does not exist |
| 409 | `SNAPSHOT_ALREADY_EXISTS` | A snapshot with that name already exists |
| 409 | `SNAPSHOT_SOURCE_RUNNING` | Source shed is running; stop it before snapshotting |

### GET /api/snapshots/{name}

Returns a single snapshot. Shape matches the list entries above (with
`lower_cached` recomputed at read time).

**Errors:**

| Code | Error | Description |
|------|-------|-------------|
| 404 | `SNAPSHOT_NOT_FOUND` | Snapshot does not exist |

### DELETE /api/snapshots/{name}

Removes a snapshot. Sheds spawned from this snapshot remain independent —
each has its own writable upper and metadata. Deleting a snapshot whose
`lower_digest` is no longer referenced by any other shed or snapshot makes
that digest a candidate for `shed image prune`.

**Response (204 No Content)**

**Errors:**

| Code | Error | Description |
|------|-------|-------------|
| 404 | `SNAPSHOT_NOT_FOUND` | Snapshot does not exist |

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

Returns installed images across all backends, keyed by Docker ref. Each entry is an
`ImageInfo`: tag-or-dangling-digest plus content-addressed metadata.

**Response:**

```json
{
  "images": [
    {
      "name": "base",
      "path": "/var/lib/shed/vz/blobs/sha256/abc123.../rootfs.ext4",
      "docker_ref": "ghcr.io/charliek/shed-vz-base:v0.5.1",
      "size_bytes": 2147483648,
      "source": "config",
      "cached": true,
      "digest": "sha256:abc123...",
      "tag": "base",
      "in_use": true,
      "alias": "base",
      "is_default": true
    },
    {
      "name": "sha256:ff8800...",
      "path": "/var/lib/shed/vz/blobs/sha256/ff8800.../rootfs.ext4",
      "size_bytes": 2147483648,
      "source": "dangling",
      "cached": true,
      "digest": "sha256:ff8800..."
    }
  ]
}
```

| Field | Type | Description |
|-------|------|-------------|
| `name` | string | Tag name, or `sha256:...` for dangling blobs |
| `path` | string | Blob rootfs path (empty if not cached) |
| `docker_ref` | string | Docker image reference (empty for local-only images) |
| `size_bytes` | int | File size in bytes (0 if not cached) |
| `source` | string | `config` (from server config), `user` (pulled or tagged ad-hoc), or `dangling` (blob with no tag) |
| `cached` | bool | Whether the underlying blob exists locally |
| `digest` | string | Content digest (`sha256:...`) of the blob; empty when the image is uncached |
| `tag` | string | Tag name pointing at this blob; empty for dangling entries |
| `in_use` | bool | True when any existing shed or snapshot pins this digest |
| `alias` | string | Friendly `image_aliases` key (e.g. `base`); set only on `config` images, omitted otherwise |
| `is_default` | bool | True for the `config` image whose ref is the backend's `default_image`; omitted otherwise |

### GET /api/images/inspect/{name}

Returns the full manifest plus `ImageInfo` for a tag or digest. `{name}`
accepts either a tag (`full`) or a `sha256:...` digest (full or
truncated).

**Response (200 OK):**

```json
{
  "image": {
    "name": "full",
    "path": "/var/lib/shed/vz/blobs/sha256/abc123.../rootfs.ext4",
    "docker_ref": "ghcr.io/charliek/shed-vz-full:v0.5.1",
    "size_bytes": 3700000000,
    "source": "config",
    "cached": true,
    "digest": "sha256:abc123...",
    "tag": "full",
    "in_use": false,
    "alias": "full",
    "is_default": true
  },
  "manifest": {
    "schema_version": 1,
    "digest": "sha256:abc123...",
    "backend": "vz",
    "arch": "arm64",
    "source_ref": "ghcr.io/charliek/shed-vz-full:v0.5.1",
    "source_ref_digest": "sha256:def456...",
    "shed_ext_version": "v0.3.1",
    "kernel_size": 8388608,
    "initrd_size": 102400,
    "rootfs_logical_size": 21474836480,
    "rootfs_physical_size": 3700000000,
    "created_at": "2026-05-01T08:00:00Z"
  }
}
```

**Errors:**

| Code | Error | Description |
|------|-------|-------------|
| 404 | `IMAGE_NOT_FOUND` | Tag/digest does not resolve to a cached blob |

### POST /api/images/tag

Points a new tag at the digest held by another tag (or a digest passed
directly). Equivalent to `docker tag`.

**Request:**

```json
{
  "source": "full",
  "target": "stable"
}
```

| Field | Required | Description |
|-------|----------|-------------|
| `source` | Yes | Tag name or `sha256:...` digest of an existing blob |
| `target` | Yes | New tag name (must be a valid image name) |

**Response (204 No Content)**

**Errors:**

| Code | Error | Description |
|------|-------|-------------|
| 400 | `INVALID_REQUEST` | Missing fields or invalid target name |
| 404 | `IMAGE_NOT_FOUND` | Source tag/digest not found |

### POST /api/images/pull

Pulls a Docker reference, converts it to ext4, installs it under the blob
store, and advances a tag. Returns the resulting digest.

**Request:**

```json
{
  "docker_ref": "ghcr.io/charliek/shed-vz-full:v0.5.1",
  "tag": "full"
}
```

| Field | Required | Description |
|-------|----------|-------------|
| `docker_ref` | Yes | Docker registry reference to pull |
| `tag` | Yes | Tag to advance to the resulting digest |

**Response (200 OK):**

```json
{
  "tag": "full",
  "digest": "sha256:abc123..."
}
```

**Errors:**

| Code | Error | Description |
|------|-------|-------------|
| 400 | `INVALID_REQUEST` | Missing `docker_ref`/`tag` or invalid tag name |
| 500 | `BACKEND_ERROR` | Pull or ext4 conversion failed |

### DELETE /api/images/{name}

Removes the tag (Docker model). The underlying blob is preserved and is
reclaimed only by `POST /api/images/prune` once it has zero protective
references. Config-managed tags cannot be removed via this endpoint —
adjust server config instead.

**Response (204 No Content)**

**Errors:**

| Code | Error | Description |
|------|-------|-------------|
| 404 | `IMAGE_NOT_FOUND` | Tag does not exist |
| 409 | `IMAGE_IN_USE` | Tag is config-managed |

### POST /api/images/prune

Reclaims blobs that no shed or snapshot pins by digest. Tags do **not**
protect blobs — only `lower_digest` references on instance `metadata.json`
and snapshot `snapshot.json` do.

**Query Parameters:**

| Param | Default | Description |
|-------|---------|-------------|
| `dry_run` | `false` | Return candidates without deleting |

**Response (200 OK):**

```json
{
  "deleted": [
    {
      "name": "sha256:ff8800...",
      "path": "/var/lib/shed/vz/blobs/sha256/ff8800.../rootfs.ext4",
      "size_bytes": 2147483648,
      "source": "dangling",
      "cached": true,
      "digest": "sha256:ff8800...",
      "in_use": false
    }
  ]
}
```

The `deleted` array contains the blobs that were removed (or would be removed
if `dry_run=true`).

**Fail-closed on malformed metadata:** prune aborts when any instance's
`metadata.json` cannot be parsed for `lower_digest`. The error names the
broken shed and its directory — fix or remove the broken instance before
retrying. Lenient read paths (`GET /api/sheds`, `GET /api/images`,
`GET /api/system/df`) warn-and-skip on the same fault.

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
      "name": "ghcr.io/charliek/shed-vz-full:v0.6.0",
      "path": "/Users/alice/Library/Application Support/shed/vz/blobs/sha256/abc.../rootfs.erofs",
      "docker_ref": "ghcr.io/charliek/shed-vz-full:v0.6.0",
      "size": {"logical_bytes": 5368709120, "physical_bytes": 4831838208}
    },
    {
      "name": "ghcr.io/charliek/shed-vz-base:v0.6.0",
      "path": "/Users/alice/Library/Application Support/shed/vz/blobs/sha256/def.../rootfs.erofs",
      "docker_ref": "ghcr.io/charliek/shed-vz-base:v0.6.0",
      "size": {"logical_bytes": 5368709120, "physical_bytes": 0}
    }
  ],
  "kernel": {
    "path": "/Users/alice/Library/Application Support/shed/vz/vmlinux",
    "size": {"logical_bytes": 8388608, "physical_bytes": 8388608},
    "kind": "kernel"
  },
  "initrd": {
    "path": "/Users/alice/Library/Application Support/shed/vz/initrd.img",
    "size": {"logical_bytes": 102400, "physical_bytes": 102400},
    "kind": "initrd"
  },
  "sheds": [
    {
      "name": "api-dev",
      "status": "running",
      "image": "default",
      "rootfs": {
        "path": "/Users/alice/Library/Application Support/shed/vz/instances/api-dev/rootfs.ext4",
        "size": {"logical_bytes": 2147483648, "physical_bytes": 2147483648},
        "kind": "rootfs"
      },
      "console_log": {
        "path": "/Users/alice/Library/Application Support/shed/vz/instances/api-dev/console.log",
        "size": {"logical_bytes": 819200, "physical_bytes": 819200},
        "kind": "console_log"
      },
      "other_files": [],
      "total": {"logical_bytes": 2148302848, "physical_bytes": 2148302848}
    }
  ],
  "orphans": [
    {
      "path": "/Users/alice/Library/Application Support/shed/vz/stale-rootfs.ext4.lock",
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
- Images are keyed by their Docker ref (`name` == `docker_ref`); on-disk bytes are counted once per content digest even when several refs share it.

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
