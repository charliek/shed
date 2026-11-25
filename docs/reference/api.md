# HTTP API

The `shed-server` exposes a REST API for managing sheds.

**Base URL:** `http://{host}:8080/api` (or `https://{host}:8443/api` with TLS)

!!! note "Authentication"
    In `auth.mode: open` (the default, loopback only) the API has no
    authentication: every request is the implicit user, an admin. In
    `auth.mode: mtls` every request needs a client certificate of the required
    scope, presented at the TLS handshake (`RequireAndVerifyClientCert`): an
    unauthenticated peer never reaches the router, so no route is exempt,
    `GET /api/info` included. The credential bus (`/api/plugins/*`) requires
    the `credentials` scope; the Connect tunnel
    (`/api/sheds/{name}/connect/{port}`) and the egress audit stream
    (`/api/egress/stream`) accept **either** `control` or `credentials`;
    everything else needs `control`. A certificate is never issued over HTTP:
    it is enrolled over the SSH `_bootstrap` channel (see
    [Authentication](#authentication)). The `Authorization` header is never
    read. See [Security](security.md) for the full model and pinned TLS. In
    mtls mode a server may also accept **service clients** (orchestrators with
    a certificate from `shed-server client issue`), which reach a fixed subset
    of the API for their own sheds only; see [Service
    clients](#service-clients).

Errors share one JSON shape, whatever the endpoint:

```json
{"error": {"code": "SHED_NOT_FOUND", "message": "shed not found: codelens"}}
```

## Authentication

In `mtls` mode the HTTP API is certificate-gated, and certificates are **not**
issued over HTTP. A client enrolls over a reserved **`_bootstrap` SSH
channel**:

1. Connect to the SSH port as user `_bootstrap` over the pinned host key.
2. Send the request line `"<scope> [<kind>] csr=<base64(std, DER)>"`. The
   server re-checks the connecting key against the live key set for this
   channel and signs the CSR with its internal CA, composing the subject
   entirely from authenticated knowledge (the SSH key fingerprint, the
   requested scope and kind). Any subject fields, SANs or extensions in the
   CSR itself are ignored.
3. The server returns a JSON bundle and closes the channel:

   ```json
   {
     "auth_mode": "mtls",
     "https_port": 8443,
     "tls_cert_fingerprint": "sha256:…",
     "client_cert": "-----BEGIN CERTIFICATE-----\n…",
     "scope": "control",
     "cert_serial": "…",
     "expires_at": "2026-06-15T00:00:00Z"
   }
   ```

`shed server add` performs this exchange automatically; the desktop app enrolls
its own `credentials` certificate the same way. The certificate lives
`auth.token_ttl` (default 24h) and is **re-validated on every request**: its
expiry, its key against the live key set (with a user list, also the key's
`expires` and its user's current admin flag), and its scope. A refused
certificate gets `401 UNAUTHORIZED`; a scope the route does not accept gets
`403 FORBIDDEN`. Clients re-enroll near expiry and on an auth failure.

A request acts as the user the certificate's key belongs to: on a server with
no user list that is the implicit user, an admin; on a server with
[`auth.users`](configuration.md#users) it is a named user with that user's
admin flag. What each user may reach is in [Ownership](#ownership).

A `_bootstrap` request with no `csr=` argument gets `this server requires
auth.mode: mtls; upgrade shed (client certificate support)`. An open server
answers every `_bootstrap` request with `bootstrap requires auth.ssh.mode:
enforce`. A script key cannot take the `credentials` scope (`bootstrap: a
script key cannot take the credentials scope`). See [Security › mTLS
mode](security.md#mtls-mode) for the full guarantee and the accepted
limitations.

A [service client](#service-clients)'s certificate is different: it is issued
on the shed host by `shed-server client issue`, never over `_bootstrap`, and
is checked against `auth.clients` and a fixed route list instead of the key
set and scope. Its bundle carries `client` (the client name) in place of
`scope`, and also `client_key`, because the server generates the key. See
[Service clients](#service-clients).

The bearer-token mode (`auth.mode: token`) was removed in v0.10.0; a server
never issues or reads a bearer token.

## Ownership

On a server with [`auth.users`](configuration.md#users) every shed and
snapshot has a `user`. The rules, which [Security ›
Ownership](security.md#ownership) gives in full:

| Caller | Their own shed | Another user's shed, or one with no user | A service client's shed |
|---|---|---|---|
| A user | every route | `404 SHED_NOT_FOUND`, the body of a shed that does not exist | `404 SHED_NOT_FOUND` |
| An admin | every route | `GET`, list, `stop`, `DELETE`; every other route answers `403 FORBIDDEN` | every route |

`GET /api/sheds` holds only what the caller may view. A record an admin views
but may not enter carries no `repo` and no `landing_dir`. Creating a shed under
a name someone else holds is the usual `409 SHED_ALREADY_EXISTS`. A user who
is not an admin is refused host directories, server mounts, images that are
not the default or an alias, the image store, `system prune` and egress
profile changes with `403 FORBIDDEN` (`admin only: <what>`). On a server with
no user list every caller is the implicit user and reaches everything.

A route under `/api/sheds/{name}` or `/api/egress/{name}` whose `{name}` is not
a valid shed name (lowercase letters, digits and hyphens, starting with a
letter) answers `404 SHED_NOT_FOUND` before any other check, so for such a name
the route's own `403`, `400` or `501` never appears.

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
| POST | `/api/sheds/{name}/assign` | Give a shed with no user to a user (admin) |
| POST | `/api/sheds/{name}/reset` | Wipe and recreate the shed's writable upper layer |
| POST | `/api/sheds/{name}/exec` | Run one command in a running shed, streamed as NDJSON |
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
| POST | `/api/users/refresh` | Fetch the GitHub logins again, or pull the directory's list (admin) |
| GET | `/api/users/status` | Where the server's users come from, and the directory's state (admin) |
| GET | `/api/sources` | A user's credential source and machines |
| POST | `/api/sources/use` | Move a user's credential source |
| POST | `/api/sources/unbind` | Clear a user's credential source |
| GET | `/api/plugins/listeners` | List active extension listeners |
| GET | `/api/plugins/listeners/{ns}/messages` | Subscribe to namespace (SSE) |
| POST | `/api/plugins/listeners/{ns}/respond` | Respond to a plugin message |
| GET | `/api/plugins/sheds` | List sheds with active message channels |

## Server Info

### GET /api/info

Returns server metadata and capabilities.

**Response (open mode):**

```json
{
  "name": "my-server",
  "version": "1.0.0",
  "ssh_port": 2222,
  "http_port": 8080,
  "backend": "vz",
  "auth_mode": "open",
  "features": ["overview", "http_exec", "exec_limits", "mount_selection", "uniform_shed_record", "source_binding"],
  "guest_arch": "arm64",
  "exec": {
    "max_stdin_bytes": 1048576,
    "max_timeout_seconds": 3600,
    "default_timeout_seconds": 60,
    "max_output_bytes": 268435456,
    "default_output_bytes": 4194304,
    "max_concurrent_per_shed": 8,
    "max_argv_elements": 1024,
    "max_env_keys": 256,
    "max_argv_env_bytes": 1048576
  }
}
```

A server whose backend has a `default_image` (configured, or set by a release
build) also reports it as `default_image`.

**Response (mtls mode), reachable only by a client that already presents a
valid certificate, since the listener runs `RequireAndVerifyClientCert`:**

```json
{
  "name": "build-box",
  "version": "1.0.0",
  "ssh_port": 2222,
  "backend": "firecracker",
  "auth_mode": "mtls",
  "https_port": 8443,
  "ca_fingerprint": "sha256:…",
  "ca_not_after": "2036-06-15T00:00:00Z",
  "features": ["overview", "http_exec", "exec_limits", "mount_selection", "uniform_shed_record", "service_clients", "network_profiles", "source_binding"],
  "guest_arch": "amd64",
  "exec": {
    "max_stdin_bytes": 1048576,
    "max_timeout_seconds": 3600,
    "default_timeout_seconds": 60,
    "max_output_bytes": 268435456,
    "default_output_bytes": 4194304,
    "max_concurrent_per_shed": 8,
    "max_argv_elements": 1024,
    "max_env_keys": 256,
    "max_argv_env_bytes": 1048576
  }
}
```

`auth_mode` is `open` or `mtls`. An mtls server serves no plain HTTP, so
`http_port` is always omitted there. A server older
than v0.10.0 may report `secure` (its token mode); current clients treat
`secure` and `token` as the same value.

`features` lists what this server supports beyond the base API, so a client can
refuse to send a field an older server would silently ignore. Tokens are added,
never renamed; treat an unknown token as absent.

| Token | Advertised | Meaning |
|-------|------------|---------|
| `overview` | always | `GET /api/overview`, the single-call host snapshot |
| `http_exec` | always | [`POST /api/sheds/{name}/exec`](#post-apishedsnameexec) |
| `exec_limits` | always (with `http_exec`) | This response carries `exec`: the limits an exec request is refused above, and the defaults a request that sets none gets (below) |
| `mount_selection` | always | The create request's `mounts` field |
| `uniform_shed_record` | always | Every [shed record](#get-apisheds), on every backend and every route that returns one, carries `mounts`, `network_profile` and `isolation`, and `arch` where the server knows it; this response carries `guest_arch` when every shed this server can create has one architecture (below) |
| `service_clients` | mtls mode with `auth.clients` configured | This server accepts [service clients](#service-clients) |
| `network_profiles` | Firecracker; remote unless its `firewall.mode` is `any` | The create request's `network` field (`open` or `isolated`) |
| `async_lifecycle` | remote backend | Create, start, stop and delete return before the cloud work is done; see [Remote backend](remote-backend.md) |
| `delivery` | always | The shed record carries `delivery` (see [Delivery in the shed document](#delivery-in-the-shed-document)); [`POST /api/sheds/{name}/provision`](#post-apishedsnameprovision) and the [delivery routes](#delivery-routes) exist |
| `activity` | while the audit log is open | [`GET /api/activity`](#get-apiactivity) |
| `source_binding` | a server with a credential bus | One machine per user answers credential requests; every subscribe and respond must carry the machine headers. See [Credential sources](#credential-sources) |

The `shed` CLI refuses `--mounts none|<names>` against a server without
`mount_selection`, and `--network isolated` against one without
`network_profiles`, before it sends anything.

`exec` is present exactly when `features` lists `exec_limits` (an older server
sends neither). It holds the limits of
[`POST /api/sheds/{name}/exec`](#post-apishedsnameexec), read from the same
values the route enforces, so a client can size a request instead of learning
a limit from a refusal. They are the server's own, from its `exec:` config and
fixed values: every caller, a person or a [service client](#service-clients),
reads the same ones and is held to them.

| Member | Kind | Meaning |
|--------|------|---------|
| `max_stdin_bytes` | limit | `stdin`, once decoded, is refused above this (`exec.max_stdin_bytes`, default 1 MiB) |
| `max_timeout_seconds` | limit | `timeout_seconds` is refused above this (`exec.max_timeout`, default 1h) |
| `default_timeout_seconds` | default | The timeout of a request with no `timeout_seconds`: 60, or `max_timeout_seconds` when that is lower |
| `max_output_bytes` | limit | `max_output_bytes` is refused above this (256 MiB) |
| `default_output_bytes` | default | The output budget of a request with no `max_output_bytes` (4 MiB) |
| `max_concurrent_per_shed` | limit | One more exec in a shed already running this many is refused with `429 EXEC_LIMIT` (`exec.max_concurrent_per_shed`, default 8) |
| `max_argv_elements` | limit | `argv` is refused above this many elements (1024) |
| `max_env_keys` | limit | `env` is refused above this many keys (256) |
| `max_argv_env_bytes` | limit | `argv` and `env` together are refused above this many bytes (1 MiB): the bytes of every `argv` element plus, for each `env` entry, its key, one `=` byte and its value |

Times are whole seconds, rounded down; an `exec.max_timeout` that is not a
whole number of seconds is reported as the largest `timeout_seconds` the route
accepts. A `max_*` member is the point past which a request is refused, not a
promise that a request at every limit at once fits: the request body as a
whole is also capped (base64 `stdin` plus 1 MiB), and a body over that is
refused with `413 REQUEST_TOO_LARGE`.

The `backend` field reflects the resolved backend for this server (`vz`,
`firecracker` or `remote`). When the server is configured with
`default_backend: detect`, this field shows the auto-detected value.

`guest_arch` is the CPU architecture (`amd64` or `arm64`) every shed this
server can create runs, so a client can pick a binary before it sends one. On
Firecracker and VZ it is the host's. On the remote backend it is the `arch`
every configured size shares, and it is omitted when the sizes mix
architectures: each shed record's `arch` then says. An older server omits it.

`isolation` is present on the remote backend only (`topology`): how that
backend provides the isolated profile. It names the backend's kind whether or
not the current `firewall` config offers `isolated` (`network_profiles` says
whether it does). What a given shed has is its record's
[`isolation`](#get-apisheds).

`ca_fingerprint` / `ca_not_after` are present **only in mtls mode**: the
internal CA's fingerprint and expiry (see [Security › CA
rotation](security.md#accepted-limitations)).

In `open` mode this endpoint needs no credential (nothing does). In `mtls`
mode no client can reach it before it holds a certificate; `shed server add`
learns the mode from the `_bootstrap` SSH bundle's `auth_mode` field instead
(see [Authentication](#authentication)).

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
      "memory_mb": 4096,
      "mounts": {"mode": "all"},
      "network_profile": "open",
      "isolation": "none",
      "arch": "arm64"
    }
  ]
}
```

Fields like `ip_address`, `cpus`, `memory_mb`, `pid`, `rootfs_path`, `project_mounts`, `landing_dir`, `owner`, `user` and `arch` are omitted when empty or zero.

| Field | Type | Description |
|-------|------|-------------|
| `owner` | string | The [service client](#service-clients) that created the shed. Omitted for a shed a person created. |
| `user` | string | The [user](configuration.md#users) the shed belongs to. Omitted on a server with no user list, and for a shed with no user (one made before the server had a user list, until [assigned](#post-apishedsnameassign)). A shed has an `owner` or a `user`, never both. `!unresolved` marks a shed whose records disagree about whose it is; only an admin reaches it. |
| `mounts` | object | The server mounts (top-level `mounts:` in the server config) the shed gets on every start: `{"mode": "all"}` (every server mount, the default) or `{"mode": "selected", "names": [...]}` (only those; no `names` means none). A shed created before mount selection existed reports `{"mode": "all"}`. A remote shed, which mounts nothing, always reports `{"mode": "selected"}`. |
| `network_profile` | string | `open` or `isolated`, fixed at create. A shed created before network profiles existed reports `open`. See [Security › Network profiles](security.md#network-profiles). |
| `isolation` | string | The isolation in force for this shed: `none` (its profile is `open`), `firewall` (`isolated`, held by firewall rules the server installs on its own host: Firecracker) or `topology` (`isolated`, held by the network giving the shed no route to another shed or to the server's private network: [remote](remote-backend.md)). It fails closed: an `isolated` shed on a backend that cannot hold isolation now reports `none` (a remote server whose `firewall` config admits more than the server), never the profile's name. It names the kind of isolation the server's configuration provides; it is not a live check that the network isolates the shed at this moment. A client that requires isolation reads this member, not `network_profile`, and treats a record without it (an older server) as not isolated. |
| `arch` | string | The guest's CPU architecture, `amd64` or `arm64`. Firecracker and VZ: the host's. Remote: the shed's size's, stored at create; when a lost record is rebuilt from the cloud, the architecture the cloud reports for the VM's boot disk (when the cloud reports none, the configured sizes' for its machine type if they all agree; none when they disagree or no configured size has that machine type); a remote shed created by a server older than this member reports none. |

Every route that returns a shed record (this list, `GET /api/sheds/{name}`,
create (JSON and the stream's `complete` event), start, stop, assign, reset,
the egress `POST` and `DELETE`, and `GET /api/overview`) returns it with these
members; `isolation` and the local `arch` are computed when the record is
reported, never stored. Advertised by `uniform_shed_record`.

The list holds only the sheds the caller may view: a service client's, its
own; a user's, their own; an admin's, every shed (see [Ownership](#ownership)).

**Status values:** `running`, `stopped`, `starting`, `stopping`, `error`

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
| `mounts` | No | every server mount | Which server mounts (top-level `mounts:`) the shed gets. Omitted: every one, as before. `[]`: none. `["claude", "gh"]`: exactly those; each must be a configured mount name (else `400 INVALID_REQUEST`). Stored with the shed and honoured on every later start, including an SSH auto-start. Needs the `mount_selection` feature. |
| `network` | No | `network.default_profile` (`open`) | Network profile: `open` (today's network) or `isolated` (the internet only; Firecracker). Fixed for the shed's life. Needs the `network_profiles` feature. See [Security › Network profiles](security.md#network-profiles). |

A [service client](#service-clients)'s create is checked against its policy
first: `mounts` defaults to its `server_mounts`, `network` to its pinned
profile, and the shed is recorded with the client as its `owner`. A person's
create records them as the shed's `user` (on a server with users). A user who
is not an admin may not set `local_dir`, `add_dirs`, a non-empty `mounts`, or
an `image` other than the default or an `image_aliases` name (`403 FORBIDDEN`,
`admin only: <field>`); omitting `mounts` gives them none, and their `repo`
clone never gets the server's `env_file` variables.

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
| 400 | `INVALID_SHED_NAME` | Invalid name format, or the reserved name `git` (`shed name "git" is reserved`) |
| 400 | `INVALID_REPO_URL` | Invalid repository URL |
| 400 | `INVALID_LOCAL_DIR` | Invalid local directory path |
| 400 | `INVALID_REQUEST` | No `image` specified and `default_image` is not configured on the chosen backend, or `upper_size_bytes` is outside the 1–100 GiB range, or `from_snapshot` was passed together with `image`/`repo`, or `mounts` names a mount the server does not have. |
| 400 | `INVALID_NETWORK_PROFILE` | `network` is not `open` or `isolated` |
| 403 | `FORBIDDEN` | A user who is not an admin asked for an admin-only field (`admin only: local_dir`, and the like), or `from_snapshot` names another user's snapshot as an admin |
| 403 | `POLICY_VIOLATION` | Service clients only: the request breaks the client's policy; the message names the client and the rule |
| 404 | `SNAPSHOT_NOT_FOUND` | `from_snapshot` names a snapshot that does not exist or that the caller may not see |
| 409 | `SHED_ALREADY_EXISTS` | A shed with that name exists, whoever it belongs to |
| 422 | `NETWORK_PROFILE_UNSUPPORTED` | A non-`open` profile on a backend that cannot enforce it (VZ) |
| 500 | `CLONE_FAILED` / `BACKEND_ERROR` | Backend or clone failure |
| 503 | `NETWORK_ENFORCEMENT_UNAVAILABLE` | The server could not install the firewall rules an `isolated` shed needs, so it refused to create it |

With SSE streaming, a refusal that happens before streaming starts
(validation, policy, network profile) is the ordinary JSON error above; a
failure after that arrives as an `error` event with the same `code`.

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
  ],
  "mounts": {"mode": "selected", "names": ["claude"]},
  "network_profile": "open",
  "isolation": "none",
  "arch": "arm64"
}
```

The fields are those of [GET /api/sheds](#get-apisheds), including `owner`
and `user` (both omitted here), `mounts`, `network_profile`, `isolation` and
`arch`. An admin
viewing another user's shed gets it without `repo` and `landing_dir`.

| Field | Type | Description |
|-------|------|-------------|
| `landing_dir` | string | Directory an interactive login lands in: `/home/shed` by default, the cloned repo path for `repo`, or the primary mount target for `local_dir`. Omitted when it is the default home. |
| `project_mounts` | array | Host directories mounted into the guest via `local_dir`/`add_dirs`. Each entry is `{source, target, readonly}`. Omitted when the shed has no project mounts. |

**Errors:**

| Code | Description |
|------|-------------|
| 404 | `SHED_NOT_FOUND`: no such shed, or one the caller may not see (the same body) |

### DELETE /api/sheds/{name}

Deletes a shed and its data.

**Response (204 No Content)**

**Errors:**

| Code | Description |
|------|-------------|
| 404 | `SHED_NOT_FOUND`: no such shed, or one the caller may not see |

An admin may delete any shed.

### Delivery in the shed document

Every shed record carries `delivery`, the record of the last delivery of credentials into the shed. It holds no value. A shed created before delivery existed reports `{"state": "none"}`.

```json
{
  "delivery": {
    "state": "partial",
    "steps": {
      "stage1": {"state": "done"},
      "clone": {"state": "done"},
      "stage2": {"state": "skipped", "reason": "no_items"},
      "install": {"state": "owed"}
    },
    "last": {"at": "2026-11-23T10:00:00Z", "reason": "create", "outcome": "accepted",
             "source": {"machine_id": "5f0c6e2a9b8d4c1e7a3f2b6d8e9c0a1b", "machine_name": "ada-laptop"}},
    "items": [{"name": "github", "provider": "github", "stage": 1, "kind": "file", "base": "home",
               "target": ".config/gh/hosts.yml", "fingerprint": "k3y-fp_01",
               "written_at": "2026-11-23T10:00:02Z", "result": "ok"}],
    "owed": ["install"]
  }
}
```

The members `state`, `steps`, `last` and `items` are the contract's record ([section 4](provision-contract.md)). The shed document adds `expired` (names of items past their `expires_at`), `owed` (the owed steps in the order they run) and `withheld` (`true` when the caller is an admin looking at another user's shed: only `state` is then present). See [Credential delivery](delivery.md) for the states and reasons.

### POST /api/sheds/{name}/provision

Delivers the shed's credentials again and finishes whatever its create left owed: the clone and the install hook. The shed must be running and the caller must be its own user. Script keys and service clients are refused with `403 FORBIDDEN`. The response is an SSE progress stream like [create](#sse-progress-streaming) and ends with the shed document.

| Status | Code | When |
|---|---|---|
| 403 | `FORBIDDEN` | not the shed's user, a script key, or a service client |
| 404 | `SHED_NOT_FOUND` | no such shed, or one the caller may not see |
| 409 | `SHED_NOT_RUNNING` | the shed is not running |
| 409 | `PROVISION_IN_PROGRESS` | a provision, or a remote job, is already running for the shed |
| 409 | `SHED_ORIGIN_CHANGED` | the shed's user's name has moved between the server's local admins and the account service since the shed was made. Fixed message; nothing is requested. See [Credential delivery](delivery.md#where-a-sheds-user-came-from) |
| 409 | `SHED_ORIGIN_UNKNOWN` | the shed has no recorded origin: no credentials are delivered to it. Recreate it (a shed made before v0.12 is recorded at the server's first start on v0.12; one made after that by an older shed-server stays unknown). Fixed message |

A failed delivery is a progress warning, `Credentials were not delivered (<reason>). Run: shed provision <name>`, not an error status.

### Delivery routes

The server's channel to a person's credential source; the wire format is the [provision contract](provision-contract.md). Both routes are the machine source's. They need the `credentials` scope and the machine headers, and are refused for service clients and script keys, and with `409 SOURCE_IS_SERVICE` for a user the account service answers for. A hosted source has no route on the server: the server calls the service.

| Route | Purpose |
|---|---|
| `GET /api/delivery/requests` | The source's SSE stream of delivery requests; the `200` carries `X-Shed-Delivery-Stream` |
| `POST /api/delivery/requests/{id}` | The answer to one request; `204` when accepted |

### GET /api/activity

The shed lifecycle and delivery events from the server's audit log, newest first. Control scope, any caller; service clients may read their own. Advertised as the `activity` feature while the audit log is open.

| Query | Meaning |
|---|---|
| `since` | an opaque cursor: pass the previous response's `next` to get the following (older) page. Omitted: the newest events. |
| `shed` | only events about this shed, by name or id |
| `limit` | a whole number from 1; larger than 500 is taken as 500 (default 100) |

The response is `{events, next}`; pass `next` back as `since` for the following page (`next` is empty when there is nothing older). Each event has `id`, `at`, `kind` (`shed.create`, `shed.start`, `shed.stop`, `shed.delete`, `shed.reset`, `shed.assign`, `shed.provision.start`, `shed.provision.end`, `delivery`; an admin also sees older kinds), `shed` (name, id, user), `actor`, and `result` or `delivery`. A delivery event holds states, reasons, the source machine, item names, results and fingerprints, never a value or a target. Visibility is applied before paging: a person sees events of sheds that were theirs at the time, a service client its own, an admin all.

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
| 403 | `FORBIDDEN`: an admin and another user's shed, or a shed with no user (starting is entering); a user who is not an admin and a shed that carries host directories or server mounts |
| 404 | `SHED_NOT_FOUND`: no such shed, or one the caller may not see |
| 409 | Shed already running |
| 503 | `NETWORK_ENFORCEMENT_UNAVAILABLE`: an `isolated` shed cannot start while its firewall rules cannot be installed |

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
| 404 | `SHED_NOT_FOUND`: no such shed, or one the caller may not see |
| 409 | Shed already stopped |

An admin may stop any shed.

### POST /api/sheds/{name}/assign

Gives a shed that has **no user and no service client** to a configured user:
a shed made before the server had a user list. Admin only. A shed is never
moved between users, and snapshots cannot be assigned.

**Request:**

```json
{"user": "alice"}
```

**Response (200 OK):** the shed, with its new `user`.

**Errors:**

| Code | Error | Description |
|------|-------|-------------|
| 400 | `INVALID_REQUEST` | The body is not `{"user":"<name>"}`, or no such user is configured |
| 403 | `FORBIDDEN` | The caller is not an admin (checked first, so a refused caller learns nothing of the user list or the shed) |
| 404 | `SHED_NOT_FOUND` | No such shed, or it was deleted and created again since it was looked up |
| 409 | `SHED_ALREADY_ASSIGNED` | The shed has a user or a service client |
| 409 | `SHED_HOST_RESOURCES` | The shed carries host directories or a server-mount selection that is not explicitly empty: assigning it would hand those to its new user. Recreate it without them (`--mounts none`). On Firecracker and VZ a shed stored before mount selection existed counts as carrying server mounts. |

On Firecracker and VZ the new user is written to disk before the answer, and a
running shed's credential requests go to the new user's machine from then on.
On the remote backend the user is also written to the instance's `shed_user`
label; see [Remote backend › Users](remote-backend.md#users).

### POST /api/sheds/{name}/reset

Wipes and recreates the shed's per-shed writable overlay upper (the shed
user's home directory, including any repo cloned via `repo`). The shared lower
image is untouched, and host-mounted `local_dir`/`add_dirs` directories
(outside the overlay) are unaffected. The shed must be stopped first. Reset clears the delivered items and marks every [delivery step](delivery.md) owed; it asks no source, and the CLI's output names `shed provision`.

**Response (200 OK):** the (still-stopped) Shed object, same shape as
`GET /api/sheds/{name}`.

**Errors:**

| Code | Error | Description |
|------|-------|-------------|
| 403 | `FORBIDDEN` | The caller may view the shed but not enter it (an admin and another user's shed) |
| 404 | `SHED_NOT_FOUND` | No such shed, or one the caller may not see |
| 409 | `SHED_NOT_STOPPED` | Shed must be stopped before reset |

### POST /api/sheds/{name}/exec

Runs one command in a running shed and streams its output, with no SSH client.
Available to a caller who may enter the shed (the `control` scope in mtls
mode; see [Ownership](#ownership)) and to a [service client](#service-clients)
on a shed it owns. Advertised by the
`http_exec` feature.

**Request:**

```json
{
  "argv": ["bash", "-lc", "craze --version"],
  "cwd": "/home/shed/project",
  "env": {"CI": "1"},
  "stdin": "aGVsbG8K",
  "timeout_seconds": 60,
  "max_output_bytes": 4194304
}
```

| Field | Required | Default | Description |
|-------|----------|---------|-------------|
| `argv` | Yes | - | The command, run exactly as given with no shell wrap, as the `shed` user. 1 to 1024 elements, no NUL bytes. A caller who wants a login shell (and its `PATH`) passes `["bash", "-lc", "..."]`; root is `sudo`. |
| `cwd` | No | the shed's landing dir | Working directory; an absolute path. |
| `env` | No | `{}` | Extra environment. At most 256 keys matching `^[A-Za-z_][A-Za-z0-9_]*$`, no NUL in values. `argv` and `env` together are at most 1 MiB. |
| `stdin` | No | none (EOF at once) | Standard base64. At most `exec.max_stdin_bytes` once decoded (default 1 MiB). |
| `timeout_seconds` | No | 60 (or `exec.max_timeout` if lower) | At most `exec.max_timeout` (default 1h). A larger or negative value is refused, never clamped. |
| `max_output_bytes` | No | 4194304 (4 MiB) | Output budget for stdout and stderr together, at most 268435456 (256 MiB). |

Unknown fields and trailing data after the JSON object are refused (`400`).

A server that advertises `exec_limits` reports this server's values for each of
these limits and defaults in [`GET /api/info`](#get-apiinfo) `exec`.

**Response (200 OK, `Content-Type: application/x-ndjson`):** one JSON object
per line, flushed as output arrives:

```text
{"t":"stdout","d":"Y3JhemUgMC4yLjAK"}
{"t":"stderr","d":"d2FybmluZzogLi4uCg=="}
{"t":"exit","code":0,"timed_out":false,"truncated":false,"duration_ms":412}
```

| Frame | Fields | Meaning |
|-------|--------|---------|
| `stdout`, `stderr` | `d` | One chunk of output, standard base64 |
| `exit` | `code`, `timed_out`, `truncated`, `duration_ms` | Always the last line when the result is known. `duration_ms` is measured from the request's arrival. |

How a stream ends:

- **The command exits:** an `exit` frame with its exit code.
- **Output past `max_output_bytes`:** the part that fits is sent, the rest is
  discarded while the command keeps running, and the `exit` frame carries the
  real exit code with `"truncated": true`.
- **Timeout:** the server cancels the command and sends
  `{"t":"exit","code":124,"timed_out":true,...}`.
- **Client disconnect:** the command is cancelled; nothing more is written.
- **No `exit` frame:** the result was lost (for example, the guest agent
  connection failed after the command started). The command may or may not
  have finished; do not read this as success or failure.

Cancellation (timeout or disconnect) closes the agent connection, and the guest
agent then sends `SIGHUP` to the command's process group. A process that
ignores `SIGHUP` survives. Both this and the separate `stderr` frames come from
the guest agent baked into the shed's image: an agent older than v0.7.7 sends
stderr inside the `stdout` frames, and one older than v0.7.6 does not signal the
process group, so the command outlives the cancellation. The 200 is sent once the request has been
dispatched to the guest agent, so every refusal before that is an ordinary JSON
error. A 200 does not prove the command launched: if the guest cannot start it
(a missing executable, a bad `cwd`), the stream carries no output and an `exit`
frame with code 1.

**Errors:**

| Code | Error | Description |
|------|-------|-------------|
| 400 | `INVALID_REQUEST` | Validation failed (empty `argv`, a NUL byte, a bad `env` key, a relative `cwd`, invalid base64, an unknown field, `timeout_seconds` over `exec.max_timeout`, `max_output_bytes` over 256 MiB) |
| 403 | `FORBIDDEN` | The caller may view the shed but not enter it |
| 404 | `SHED_NOT_FOUND` | No such shed, or one the caller may not see |
| 409 | `SHED_NOT_RUNNING` | The shed is not running. Exec never starts a shed; start it first. |
| 413 | `REQUEST_TOO_LARGE` | The body, or its decoded `stdin`, is over the limit |
| 429 | `EXEC_LIMIT` | The shed already runs `exec.max_concurrent_per_shed` HTTPS execs (default 8) |
| 504 | `BACKEND_ERROR` | The timeout expired before the request reached the guest agent |

!!! note "Read only `SHED_NOT_FOUND` as \"shed gone\""
    A 404 means the shed is missing only when its code is `SHED_NOT_FOUND`.
    A proxy or an unknown route can also answer 404; check the code, not the
    status.

The `shed exec` CLI command is unchanged: it still runs over SSH (see
[CLI › shed exec](cli.md#shed-exec)), where the server wraps the command in
`bash -lc`. The HTTPS endpoint does not.

## Service clients

A **service client** is an orchestrator (a CI system, an agent runner) that
drives sheds on a server it does not administer. Clients exist only in
`auth.mode: mtls`, are declared under [`auth.clients`](configuration.md#service-clients),
and authenticate with a certificate signed by `shed-server client issue` (see
the [Orchestrated sheds guide](../guides/orchestrated-sheds.md)). A request
whose certificate is a service certificate is a *client request*; every other
request is a user's request (the implicit user's on a server with no user
list) and follows [Ownership](#ownership). Nothing about clients changes with
a user list.

Client requests are deny by default. The whole surface a client can reach:

| Method | Endpoint | Rule |
|--------|----------|------|
| GET | `/api/info` | |
| GET | `/api/images` | |
| GET | `/api/sheds` | Only the sheds it owns |
| POST | `/api/sheds` | Checked against its policy; the shed's `owner` is the client |
| GET | `/api/sheds/{name}` | Owned sheds only |
| DELETE | `/api/sheds/{name}` | Owned sheds only |
| POST | `/api/sheds/{name}/start` | Owned sheds only |
| POST | `/api/sheds/{name}/stop` | Owned sheds only |
| POST | `/api/sheds/{name}/exec` | Owned sheds only |

| Status | Code | When |
|--------|------|------|
| 401 | `UNAUTHORIZED` | The certificate is service-shaped but malformed, names a client that is not configured, or is expired. The connection is closed. (An unknown client is normally refused at the TLS handshake already.) |
| 403 | `FORBIDDEN` | Any route not in the table above, including every unknown path and wrong method |
| 403 | `POLICY_VIOLATION` | A create that breaks the client's policy (the message is `client <name>: <rule>`) |
| 404 | `SHED_NOT_FOUND` | A shed it does not own, a person's shed, or one that does not exist: the same body in every case, so a client cannot learn which sheds exist |

The create policy refuses `local_dir`, `add_dirs`, `from_snapshot`, an `image`
outside the policy's `images` (empty: only the server default), a name without
the policy's `name_prefix`, CPUs or memory over the policy's `caps` (compared
after defaults), a create that would take the client past `max_sheds` (owned
sheds plus creates in flight), a `network` other than the pinned profile, and a
`mounts` entry outside `server_mounts`. It runs before any other validation
touches the host, so a refused client learns nothing about the server's
filesystem. `repo`, `egress`, `no_provision`, `cpus`, `memory_mb` and
`upper_size_bytes` are allowed within those limits.

An admin may list, stop, delete and enter a client's shed through the full
API; a user who is not an admin cannot see it.

## Snapshots

### GET /api/snapshots

Lists the snapshots the caller may see: a user's own, or, for an admin, every
snapshot. Each snapshot's `lower_cached` field is recomputed at read time from
the local blob store; it is never persisted to `snapshot.json`. On a server
with users each snapshot carries `user`, the user of the shed it was taken
from (omitted when none).

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
| 403 | `FORBIDDEN` | The caller may view the source shed but not enter it (taking a snapshot is entering) |
| 404 | `SHED_NOT_FOUND` | Source shed does not exist, or the caller may not see it |
| 409 | `SNAPSHOT_ALREADY_EXISTS` | A snapshot with that name already exists |
| 409 | `SNAPSHOT_SOURCE_RUNNING` | Source shed is running; stop it before snapshotting |

### GET /api/snapshots/{name}

Returns a single snapshot. Shape matches the list entries above (with
`lower_cached` recomputed at read time).

**Errors:**

| Code | Error | Description |
|------|-------|-------------|
| 404 | `SNAPSHOT_NOT_FOUND` | Snapshot does not exist, or it is another user's |

### DELETE /api/snapshots/{name}

Removes a snapshot. Sheds spawned from this snapshot remain independent —
each has its own writable upper and metadata. Deleting a snapshot whose
`lower_digest` is no longer referenced by any other shed or snapshot makes
that digest a candidate for `shed image prune`.

**Response (204 No Content)**

**Errors:**

| Code | Error | Description |
|------|-------|-------------|
| 404 | `SNAPSHOT_NOT_FOUND` | Snapshot does not exist, or it is another user's (an admin may delete any snapshot) |

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

On a server with [users](configuration.md#users), every route here but the list and inspect is admin-only (`403 FORBIDDEN`, `admin only: image ...`).

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

On a server with users, `GET /api/system/df` shows a caller who is not an admin only their own sheds and snapshots and no orphans (the totals are the server's), and `POST /api/system/prune` is admin-only.

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

After the 101 response, the connection becomes a bidirectional byte stream to the target port inside the VM. The server uses `DialService` internally — on both VZ and Firecracker this goes through the guest agent's vsock TCP proxy (port 1028), which dials the target on the guest's loopback. Loopback-bound guest services (e.g. the rc hub) are reachable on both backends, and the peer address the guest service sees is `127.0.0.1`.

**Error responses:**

| Status | Code | Description |
|--------|------|-------------|
| 400 | `INVALID_PORT` | Port must be 1-65535 |
| 404 | `SHED_NOT_FOUND` | Shed does not exist |
| 409 | `SHED_NOT_RUNNING` | Shed is not running, on the [remote backend](remote-backend.md#calls-and-statuses) (a shed there may be starting or stopping, a state conflict as for exec) |
| 502 | `CONNECT_FAILED` | Could not connect to the port (service not running) |
| 503 | `SHED_NOT_RUNNING` | Shed is not running, on Firecracker and VZ |

**Security:** In `open` mode (loopback only) the Connect API, like the rest of the API, has no credential; in `mtls` mode it requires a client certificate of the `control` **or** `credentials` scope over pinned TLS. The caller must be allowed to enter the shed (see [Ownership](#ownership)). It connects to the VM's loopback interface and does not support arbitrary destinations.

**Consumers:**

- `shed tunnels` CLI uses Connect API for port forwarding
- A proxy extension built on the bus can use the Connect API for reverse proxying
- SSH port forwarding (`ssh -L`) uses `DialService` internally via `handleDirectTCPIP`

## Users

### POST /api/users/refresh

Makes the server fetch the SSH keys of every GitHub login it follows now,
instead of at its next scheduled refresh (`auth.ssh.github_refresh`), each
checked against the account id the login is pinned to (see [Security ›
GitHub logins are pinned](security.md#github-logins-are-pinned-to-their-account-id)).
Admin only (the implicit user is one); `shed users refresh` calls it. It waits
for the fetch for up to 60 s.

**Response (200 OK):** one row per GitHub login.

```json
{
  "logins": [
    {"user": "alice", "login": "alice-gh", "status": "ok", "github_id": 1234567, "keys": 2},
    {"user": "bob", "login": "bob-gh", "status": "mismatch", "github_id": 7654321, "seen_id": 1111111, "keys": 0,
     "removed": ["SHA256:..."]}
  ]
}
```

| Field | Description |
|-------|-------------|
| `user` | The user the login belongs to; `""` for the implicit user of a server with no user list |
| `login` | The GitHub login as configured |
| `status` | `ok` (the login names its pinned account; its keys are current), `pinned` (it had no pin and is now pinned to the account it names), `unchanged` (a check made for an unknown key found its keys as they were), `unreachable` (GitHub could not be asked or answered inconsistently: the last known keys stay and nothing new is accepted), `mismatch` (it names another account than its pin, or none: its keys are dropped until it names the pinned account again), `gone` (a login with no pin names no account), `pin-write-failed` (the pin could not be written, so nothing new is accepted), or `invalid-login` (never fetched) |
| `github_id` | The account id the login is pinned to (omitted: none yet) |
| `seen_id` | On a `mismatch`, the account the login names instead |
| `keys` | How many of the login's keys are accepted now |
| `added`, `removed` | Key fingerprints this fetch added and removed |
| `error` | Why the login could not be fetched, when it could not |

**Errors:**

| Code | Error | Description |
|------|-------|-------------|
| 403 | `FORBIDDEN` | The caller is not an admin |
| 504 | `INTERNAL_ERROR` | The fetch was still running after 60 s; its result applies when it ends |

On a server whose users come from an account service
([`auth.directory`](configuration.md#users-from-an-account-service)) the route
pulls the service's list now, ignoring a pending `Retry-After`, and the answer
gains a `directory` member, the pull's row. A failed pull is a row, not an
error: the status is still `200`. Only the request's own deadline is an error
(`504`, `the directory pull is still running; its result applies when it
ends`).

```json
{
  "logins": [],
  "directory": {"status": "failed", "list": "in-force", "version": "0f1e…", "users": 2, "skipped": 0,
                "error": "directory: unavailable (HTTP 503) \"UNAVAILABLE\": \"try later\""}
}
```

| Field | Description |
|-------|-------------|
| `status` | `good` or `failed` |
| `list` | Whether a list is in force after the pull: `in-force`, `trust-ended` (its trust time ended), `withdrawn` (the pull found the server not enrolled, or its enrolment unusable, and withdrew the list) or `none`. Unless `in-force`, only the local admins are admitted |
| `version` | The version of the list in force (omitted: none) |
| `users`, `skipped` | How many of that list's users the server admits, and how many entries it skips; local admins are not counted |
| `error` | Why the pull failed. Text from the account service in it is quoted |

### GET /api/users/status

Where the server's users come from, who they are, and on a directory server
the state of its list. Admin only, and refused to a script key even when its
user is an admin; `shed users status` calls it. It never holds a key, label or
fingerprint.

```json
{
  "source": "directory",
  "directory": {
    "url": "https://accounts.example.test",
    "organization": "example",
    "client_id": "srv_0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a",
    "state": "trusted",
    "last_good_pull": "2026-11-17T09:00:00Z",
    "version": "0f1e…",
    "trusted_until": "2026-11-20T09:00:00Z",
    "last_attempt": "2026-11-17T09:00:00Z",
    "next_attempt": "2026-11-18T09:00:00Z",
    "copy": "kept",
    "skipped": [{"name": "ops", "reason": "local-admin-name",
                 "description": "its name is a local admin's (the local admin is a separate, local-only identity)"}]
  },
  "users": [
    {"name": "ada", "admin": true, "source": "directory"},
    {"name": "ops", "admin": true, "source": "local"}
  ]
}
```

| Field | Description |
|-------|-------------|
| `source` | `directory`, `config` (`auth.users`) or `none` (no user list; `users` is empty) |
| `users[]` | `name`, `admin` and `source`: `config`, `local` (a local admin) or `directory`. A name from the account service is its text: print it quoted |
| `directory` | Present only when `source` is `directory` |
| `directory.state` | `trusted`, `expired`, `no-copy`, `not-enrolled` or `enrolment-unusable`, with `reason` beside every state but `trusted` |
| `directory.last_good_pull`, `version`, `trusted_until` | The last list held: when its answer arrived, its version, and the end of its trust time |
| `directory.last_attempt`, `last_error`, `next_attempt` | The last pull, why it failed (omitted when it was good), and when the next is due (omitted: at once) |
| `directory.copy`, `copy_error` | The kept copy: `none`, `kept`, `unused`, `not-written` or `stale-on-disk`, and why it is not kept |
| `directory.skipped[]` | Entries the server does not answer for: `name` (the service's text), `reason` (a code: `duplicate-member`, `malformed`, `invalid-name`, `duplicate-name`, `duplicate-key`, `no-usable-key`, `local-admin-name`, `local-admin-key`, `service-client-name`) and `description` |
| `directory.expired_local_admins` | Present when every key of every local admin has expired: their names |

**Errors:**

| Code | Error | Description |
|------|-------|-------------|
| 403 | `FORBIDDEN` | The caller is not an admin, or uses a script key |

## Credential sources

Each user has one **credential source** per server: the machine whose
credential-bus subscriptions the server takes and whose answers it accepts
(see [Security › One machine answers for each
user](security.md#one-machine-answers-for-each-user)). These two routes read
and move it. They need the `control` scope, so a machine's `credentials`
certificate can neither read nor move a binding, and they are not on the
service-client route list. A script key is refused both (`403 FORBIDDEN`, the
read included).

A user reads and moves their own. An admin may name another configured user
(`?user=` on the read, `"user"` in the body of the move) and can then choose
only among that user's own machines; an admin naming a user who is not
configured gets `400 INVALID_REQUEST`. A user who is not an admin naming anyone
else gets one fixed `403 FORBIDDEN`, whether or not that user exists.

### GET /api/sources

The caller's binding and the machines that have presented themselves for them,
read as one moment's state.

**Response (200 OK):**

```json
{
  "user": "alice",
  "binding": {
    "delivery": [{"kind": "machine", "id": "0123456789abcdef0123456789abcdef"}],
    "signing": [{"kind": "machine", "id": "0123456789abcdef0123456789abcdef"}],
    "bound_at": "2026-11-16T09:00:00Z",
    "bound_by": "first-connect",
    "generation": 1
  },
  "machines": [
    {"id": "0123456789abcdef0123456789abcdef", "name": "laptop", "key_fingerprint": "SHA256:...",
     "first_seen": "2026-11-16T09:00:00Z", "connected": true, "namespaces": ["ssh-agent"]},
    {"id": "fedcba9876543210fedcba9876543210", "name": "desktop", "key_fingerprint": "SHA256:...",
     "first_seen": "2026-11-16T10:00:00Z", "connected": false, "namespaces": []}
  ]
}
```

| Field | Description |
|-------|-------------|
| `user` | The user the answer is for; `""` for the implicit user |
| `binding` | `null` when no machine is the source yet (the first to connect becomes it). `delivery` (where requests are sent) and `signing` (whose answers are accepted) each name exactly one machine, the same one. |
| `binding.bound_by` | `first-connect`, or who moved it last: `user <name> with key "<label>" (<fingerprint>)` for a key listed with a label, `user <name> with key <fingerprint>` otherwise, `admin <name> ...` when an admin moved another user's, `the implicit user ...` on a server with no user list |
| `binding.generation` | Increases on every bind and move |
| `machines` | Up to 32 machines remembered for the user, oldest first. A machine is known only once its app has tried to connect. |
| `machines[].key_fingerprint` | The key the machine's certificate was enrolled with (empty in open mode) |
| `machines[].connected`, `namespaces` | Whether it holds a subscription now, and for which request types |

For a user the account service answers for (a directory user on a server with `auth.directory`), the document has no machine and no pin: `machines` is `[]`, and the delivery slot holds one service entry. The user is the target user, so an admin's `?user=` is judged by that user's name.

```json
{
  "user": "ada",
  "binding": {
    "delivery": [{"kind": "service", "url": "https://accounts.example.test"}],
    "signing": []
  },
  "machines": []
}
```

| Field | Description |
|-------|-------------|
| `binding.delivery[].kind` | `service` here; `machine` for a pinned machine (then `id` is present) |
| `binding.delivery[].url` | The account service's URL, as the server's `auth.directory.url` has it |
| `binding.delivery[].state` | Absent normally. `not_delivering` while the server remembers, for up to five minutes, that the service does not offer delivery or does not let this server ask |

The members are additive: a client that reads only `machine` entries ignores a `service` one.

**Errors:** `400 INVALID_REQUEST` (`?user` twice, or an unknown user named by
an admin), `403 FORBIDDEN`, `503 SOURCE_BUSY` with `Retry-After` (the user's
records were being changed or saved for more than 2 s; retry), `503
PLUGINS_DISABLED` (the server has no credential bus).

### The service refusal: `409 SOURCE_IS_SERVICE`

For a user the account service answers for, `POST /api/sources/use`, `POST /api/sources/unbind`, `GET /api/delivery/requests`, `POST /api/delivery/requests/{id}` and the listener routes under `/api/plugins/listeners` answer `409` with one fixed body, judged by the target user:

```json
{
  "error": {
    "code": "SOURCE_IS_SERVICE",
    "message": "the account service is this user's credential source on this server: no machine answers here"
  }
}
```

### POST /api/sources/use

Makes a machine that has presented itself for the user their credential
source. The body is exactly one JSON object with `machine_id` (32 lowercase
hex digits) and, for an admin, optionally `user`; any other field, a field
given twice, or trailing data is refused.

```json
{"machine_id": "fedcba9876543210fedcba9876543210"}
```

**Response (200 OK):** the same document as `GET /api/sources`, as the move
left it.

The move takes effect at once: the old machine's streams end, the requests
sent to it are dropped, and its answers are refused. The new machine is not
called; it starts answering when its app next connects. Moving to the
machine already bound changes nothing (the `generation` is unchanged).

**Errors:**

| Code | Error | Description |
|------|-------|-------------|
| 400 | `INVALID_REQUEST` | The body, a malformed `machine_id`, or an admin naming a user who is not configured |
| 403 | `FORBIDDEN` | As for `GET /api/sources` |
| 404 | `SOURCE_MACHINE_NOT_FOUND` | No machine with that id has presented itself for the user |
| 500 | `INTERNAL_ERROR` | The move was not confirmed: the store could not save it. The current source stays in use for now, but the move may take effect when the server restarts; check with `GET /api/sources` before retrying |
| 503 | `PLUGINS_DISABLED` | The server has no credential bus |

### POST /api/sources/unbind

Clears the user's credential source. The body is `{}` (or empty) and, for an
admin, optionally `{"user": "<name>"}`; any other field, a field given twice,
or trailing data is refused.

The body may also name `"machine_id": "<32 lowercase hex digits>"`, which
makes the unbind a compare-and-clear: the pin is cleared only while it still
names that machine. When the pin names another machine the answer is `409
SOURCE_MOVED` and nothing changes. When the user has no pin the answer is
`200` with `"unbound": false`, as without the field. The desktop app sends
its own machine id on every unbind, so it never clears a pin that another
machine took over in the meantime. Without `machine_id` (`shed source unbind`)
the pin is cleared whichever machine it names. A server older than v0.12
refuses the field with `400 INVALID_REQUEST` and changes nothing.

**Response (200 OK):**

```json
{
  "user": "alice",
  "unbound": true,
  "previous": {"id": "fedcba9876543210fedcba9876543210", "name": "desktop"},
  "forgotten_machines": 2
}
```

| Field | Description |
|-------|-------------|
| `unbound` | `false` when the user had no source: nothing changed (`previous` is then `null`) |
| `previous` | The machine that was the source |
| `forgotten_machines` | How many remembered machines were forgotten with the pin |

The server forgets the pin and the machines it remembers for the user, closes
the user's streams and drops the requests sent on them. The first of the
user's machines to connect next is bound (`first-connect`), so a machine whose
app is still running reconnects and becomes the source again. Until then a
create asks no source (delivery is `not_applicable`).

**Errors:**

| Code | Error | Description |
|------|-------|-------------|
| 400 | `INVALID_REQUEST` | The body, a malformed `machine_id`, or an admin naming a user who is not configured |
| 403 | `FORBIDDEN` | As for `GET /api/sources` |
| 409 | `SOURCE_MOVED` | `machine_id` was given and the pin names another machine: nothing changed |
| 500 | `INTERNAL_ERROR` | The change was not confirmed: the store could not save it. The current source stays in use for now, but the change may take effect when the server restarts; check with `GET /api/sources` before retrying |
| 503 | `PLUGINS_DISABLED` | The server has no credential bus |

## Extensions

Extension endpoints carry the message bus between sheds and the machines
that answer for their users. Shed ships no extension on it since v0.11; credentials travel by [delivery](delivery.md). See [Extensions](extensions.md) for the full guide
and [Security › Credential bus](security.md#credential-bus) for the routing,
the pin and its limits. Every route here needs the `credentials` scope in mtls
mode, and refuses a service client (`403 FORBIDDEN`, `the credential bus is for
users`). Each acts as the caller's user.

### GET /api/plugins/listeners

Lists the caller's active listeners; an admin (and the implicit user) sees
every user's, sorted by user and then namespace.

**Response (200 OK):**

```json
{
  "listeners": [
    {"namespace": "ssh-agent", "user": "alice", "created_at": "2026-03-29T12:00:00Z"}
  ]
}
```

`user` is omitted for the implicit user.

### GET /api/plugins/listeners/{namespace}/messages

Subscribes the caller's machine to the user's requests of one namespace, as
an SSE stream. Another user's subscription to the same namespace is
independent.

**Headers:** `Accept: text/event-stream`, and the machine headers
`X-Shed-Machine-Id` (32 lowercase hex digits) and `X-Shed-Machine-Name`
(percent-encoded UTF-8, 1 to 63 bytes decoded, no control characters).

The stream sends an SSE comment every 25 s when idle. A write that is not
taken within 15 s closes it.

**Errors:**

| Code | Status | Description |
|------|--------|-------------|
| `FORBIDDEN` | 403 | A service client, or a script key |
| `NAMESPACE_RESERVED` | 403 | `system:*` namespaces cannot be registered |
| `INVALID_NAMESPACE` | 400 | The namespace is malformed |
| `SOURCE_MACHINE_ID_REQUIRED` | 403 | The machine headers are missing or invalid (a client released before v0.10.0); the message names the minimum versions |
| `SOURCE_RATE_LIMITED` | 429 | Too many new or changed machines for this user lately; with `Retry-After` in seconds |
| `SOURCE_NOT_BOUND` | 403 | Another machine is the user's source; the body's `source` names it (`machine_id`, `machine_name`) |
| `SOURCE_DUPLICATE_STREAM` | 409 | The source machine already holds this namespace. Retryable: nothing is replaced, and a dead stream is closed by its write deadline |
| `INTERNAL_ERROR` | 500 | The bindings store could not be saved; nothing was registered |
| `PLUGINS_DISABLED` | 503 | The server has no credential bus, or is shutting down |

The first machine to connect for a user becomes the source. A request the
source has not answered when it disconnects is kept for that machine's
reconnect only; no other machine receives it.

### POST /api/plugins/listeners/{namespace}/respond

Sends an answer back to a shed. The envelope must include `shed.name`, and
`in_reply_to` must be the id the server sent with the request. The answer is
accepted only from the user's source machine (the same machine headers as the
subscribe), only for a request that machine was sent, and is written only into
the shed connection the request arrived on.

**Response (204 No Content)** once the answer is written into the shed.

**Errors:**

| Code | Status | Description |
|------|--------|-------------|
| `INVALID_BODY` | 400 | The body is not valid JSON |
| `MISSING_SHED` | 400 | Envelope missing `shed.name` |
| `FORBIDDEN` | 403 | A service client or a script key; or the answer matches no pending request of this user (forged, replayed, already answered, another user's, or for a connection that is gone) |
| `SOURCE_MACHINE_ID_REQUIRED` | 403 | As for subscribe |
| `SOURCE_NOT_BOUND` | 403 | The machine is not the user's source, or no longer is (the binding moved since the request was sent) |
| `SHED_NOT_CONNECTED` | 404 | The write into the shed failed part-way; not retryable |
| `REQUEST_TOO_LARGE` | 413 | The body is over the guest framing limit |
| `TOO_MANY_RESPONSES` | 429 | More than 8 answers in flight to this shed connection; with `Retry-After: 1` |
| `RESPONSE_NOT_DELIVERED` | 503 | The shed did not take the answer within 5 s; nothing was written and the request is still answerable; with `Retry-After: 1` |

### GET /api/plugins/sheds

Lists the caller's sheds with active message channels; an admin (and the
implicit user) sees every user's. A service client's sheds never appear.

**Response (200 OK):**

```json
{
  "sheds": [
    {"name": "my-dev", "backend": "vz", "server": "my-server", "user": "alice"}
  ]
}
```

`user` is omitted for the implicit user and for a shed routed to nobody.
