# Configuration

Shed uses YAML configuration files for both client and server settings.

## Client Configuration

**Location:** `~/.shed/config.yaml`

The client configuration stores server connections and cached shed locations.

```yaml
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

default_server: mini-desktop

# Timeout for shed create and start operations
create_timeout: 30m

sheds:
  codelens:
    server: mini-desktop
    status: running
    updated_at: "2026-01-20T10:30:00Z"
```

### Client Fields

| Field | Type | Description |
|-------|------|-------------|
| `servers` | map | Configured server connections |
| `servers.<name>.host` | string | Server hostname or IP |
| `servers.<name>.http_port` | int | HTTP API port |
| `servers.<name>.ssh_port` | int | SSH server port |
| `servers.<name>.api_url` | string | HTTPS control-plane URL (e.g. `https://host:8443`); overrides scheme+host+port. Set by `shed server add --https-port`. |
| `servers.<name>.tls_cert_fingerprint` | string | Pinned TLS cert fingerprint (`sha256:...`) — the server's own leaf, pinned in every enforced mode. |
| `servers.<name>.control_token` | string | Control-scoped bearer token for the CLI/desktop. Auto-written and refreshed by `shed server add` in token mode. Absent in mtls mode. |
| `servers.<name>.control_token_expires_at` | timestamp | Expiry of `control_token`; the CLI refreshes near this and on a 401. Auto-managed. |
| `servers.<name>.auth_mode` | string | The credential shape the server last issued at bootstrap: `token` or `mtls`. **Absent means `token`** (every entry written before certificate support existed predates this field). Not a setting — it's a cache of what the server last said; a bootstrap that comes back in the other mode rewrites it, which is how an operator can flip a server's `auth.mode` without re-adding every client. |
| `servers.<name>.client_cert_file` / `client_key_file` | string | Paths (under `~/.shed/creds/<server>/`, mode 0700/0600) to this entry's client certificate and private key. Set only in mtls mode. |
| `servers.<name>.client_cert_expires_at` | timestamp | Expiry of the client certificate; the CLI re-enrolls before a request would race expiry. Set only in mtls mode. |
| `default_server` | string | Default server for commands |
| `sheds` | map | Cached shed locations |
| `create_timeout` | duration | Timeout for create/start operations (default: `10m`) |

## Server Configuration

**Locations (checked in order):**

1. `./server.yaml`
2. `~/.config/shed/server.yaml`
3. `/etc/shed/server.yaml`

```yaml
name: mini-desktop
http_port: 8080
ssh_port: 2222

mounts:
  claude:
    source: ~/.claude
    target: /home/shed/.claude
    readonly: false

env_file: ~/.shed/env
log_level: info
```

### Server Fields

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `name` | string | `shed-server` | Server identifier |
| `http_port` | int | `8080` | Plain-HTTP API port. **Required in open mode; optional in token/mtls mode** (an enforced mode serves no plain HTTP, so it is omitted from `/api/info` and the written client config entry). |
| `ssh_port` | int | `2222` | SSH server port |
| `bind_address` | string | `127.0.0.1` | Interface every listener (HTTP, HTTPS, SSH) binds to. **Defaults to loopback in every mode** — shed is local-first. Set a specific IP (LAN/tailnet), `0.0.0.0` or `*` (all IPv4), or `::` (all interfaces) to face the network. A non-loopback bind in **open** mode also requires `allow_insecure_exposure: true`; token/mtls mode needs no acknowledgment. |
| `allow_insecure_exposure` | bool | `false` | Acknowledge binding a **non-loopback** `bind_address` in **open** mode (no transport security). Required there, ignored in token/mtls mode and for loopback binds. |
| `trusted_proxy` | bool | `false` | Trust the client-supplied `X-Forwarded-For` header for the request source IP. Only enable behind a reverse proxy that overwrites that header; the default uses the real TCP peer address. |
| `default_backend` | string | `detect` | Backend to use when none is specified (`detect`, `firecracker`, `vz`). `detect` auto-selects based on platform: `vz` on macOS, `firecracker` on Linux. |
| `mounts` | map | `{}` | Host directories to mount into sheds (formerly `credentials`) |
| `env_file` | string | - | Path to environment variables file |
| `log_level` | string | `info` | Logging level (debug, info, warn, error) |
| `extensions` | object | `{}` | Extensions to activate in VMs (see [Extensions](extensions.md)) |
| `git` | object | - | Git behaviour for in-VM clones (see [Git](#git)) |
| `firecracker` | object | - | Firecracker-specific configuration (see below) |
| `vz` | object | - | VZ-specific configuration (see below) |
| `auth` | object | - | Optional authentication (see [SSH authentication](#ssh-authentication)) |

**Note:** Only VM backends are supported. Firecracker is available on Linux. VZ is available on macOS Apple Silicon (arm64). The `detect` backend auto-selects based on platform.

### Authentication mode

`auth.mode` is the headline switch. The default `open` posture is unchanged —
ideal on a tailnet or trusted LAN (plain HTTP, no credential, no TLS); the SSH
allowlist (`auth.ssh`) is the one independently-tunable layer there. `token`
and `mtls` are the internet-facing postures: both derive the same hardened
bundle (SSH enforce + HTTP credential enforce + TLS-only) and refuse to start
without an SSH key source; they differ only in the shape of the HTTP
credential — a bearer token minted over SSH (`token`) versus a client
certificate, bound to a key that never leaves the client, issued over SSH
(`mtls`). Two invariants hold for both enforced modes: **credential ⟺ TLS ⟺
enforced mode** and **https ⟺ enforced mode**. `mtls` is the **recommended
posture for anything internet-facing**; `token` is retained (not deprecated)
because `curl`/scripts/CI/third-party callers can't present a client
certificate. See [Security](security.md) — in particular [mTLS
mode](security.md#mtls-mode) — and the [Security Configuration
guide](../guides/security-configuration.md).

| `auth.mode` | HTTP credential | SSH allowlist | Notes |
|-------------|------------------|----------------|-------|
| `open` (default) | none | as configured (`auth.ssh.mode: off`/`warn`) | Plain HTTP; `https_port` rejected. |
| `token` | bearer token, minted over the `_bootstrap` SSH channel | forced `enforce` | TLS-only; `curl`/scripts can present the token directly. `secure` is a permanent deprecated alias, normalized at load with a one-time stderr warning. |
| `mtls` | client certificate, issued over the `_bootstrap` SSH channel (CSR in, signed cert out); revalidated on every request | forced `enforce` | TLS-only; recommended for internet-facing deployments. Revocation removes the SSH key (coarser than token's per-token revoke). `curl`/scripts cannot authenticate. |

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `auth.mode` | string | `open` | `open` enforces nothing (plain HTTP only); `token`/`mtls` both derive SSH-allowlist enforce + HTTP-credential enforce + TLS-only (`https_port` defaults to `8443`; **no plain-HTTP listener is served**), and **fail to start without a key source**. `secure` is accepted as a deprecated alias for `token`, normalized at load with a one-time stderr warning. |
| `auth.token_ttl` | duration | `24h` | Lifetime of a bootstrap-minted HTTP token (`token` mode) or issued client certificate (`mtls` mode). Clients refresh/re-enroll transparently near expiry / on an auth failure. |

The pre-v0.7.1 `public_exposure` flag and `auth.http.tokens` list are removed and
**rejected at startup** — set `auth.mode: token` (or `mtls`) and let
`shed server add` obtain a credential over SSH. As of v0.7.2 the whole
`auth.http` block is gone (HTTP enforcement derives from `auth.mode`), and
`https_port` / `auth.ssh.mode: enforce` are valid only in an enforced mode —
see [Upgrading v0.7.1 → v0.7.2](../upgrades/v0.7.1-to-v0.7.2.md). As of v0.7.4
`http_bind`/`ssh_bind`/`internal_http_port` are removed in favour of a single
`bind_address` (loopback by default), and `http_port` is optional in an
enforced mode — see
[Upgrading v0.7.3 → v0.7.4](../upgrades/v0.7.3-to-v0.7.4.md). `mtls` mode was
added afterward — see [Upgrading to mTLS](../upgrades/token-to-mtls.md).

### SSH authentication

`auth.ssh` gates SSH public-key access against an allowlist. The default (omitted, or `mode: off`) accepts all keys — the legacy behavior. Identity comes from the offered key, GitHub-style; the username still selects the shed.

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `auth.ssh.mode` | string | `off` | `off` accepts all keys; `warn` logs would-deny attempts but still accepts (use to roll out safely); `enforce` rejects keys not in the allowlist. |
| `auth.ssh.authorized_keys` | list | `[]` | Inline OpenSSH `authorized_keys` lines. |
| `auth.ssh.authorized_keys_file` | string | - | Path to an `authorized_keys`-format file. |
| `auth.ssh.github_users` | list | `[]` | Seed the allowlist from `https://github.com/<user>.keys`. Keys are cached to disk and fail closed to the last-known-good cache if GitHub is unreachable. |
| `auth.ssh.github_refresh` | duration | `1h` | How often to re-fetch GitHub keys. |
| `auth.ssh.max_auth_tries` | int | `10` | Public-key attempts allowed per connection. Raise it when a client's agent (1Password, Secretive) holds many keys, so the allowlisted key is tried before the server gives up. |

With `mode: enforce`, the server refuses to start if no keys resolve (empty inline/file and a failed GitHub fetch with no cache) — use `warn` for first boot, or provide inline keys.

```yaml
auth:
  mode: token                # open (default) | token | mtls — both enforced modes force ssh enforce
  ssh:
    github_users: [charliek]  # a key source is required in an enforced mode
```

### TLS and network surface

TLS and network-surface fields. HTTP credential enforcement is **not** an
independent field — it is derived from `auth.mode: token`/`mtls` (there is no
`auth.http` block). Either enforced mode turns on TLS for you, and
`https_port` is valid only in an enforced mode. See
[Security](security.md) and the
[Security Configuration guide](../guides/security-configuration.md).

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `https_port` | int | `8443` in an enforced mode | Serve HTTPS with a self-signed, client-pinned cert, bound to `bind_address`. **Requires `auth.mode: token` or `mtls`** (rejected in open mode); defaults to `8443` there. In `token`/`mtls` mode this is the *only* listener — no plain HTTP is served. In `mtls` mode the listener also runs `RequireAndVerifyClientCert` against the internal CA (see [mTLS mode](security.md#mtls-mode)). |
| `tls_names` | list | `[]` | Extra hostnames/IPs added as cert SANs (on the server's own leaf; unrelated to the mtls client-CA). `localhost`, `127.0.0.1`, `::1` are always included. |
| `tls_cert_file` / `tls_key_file` | string | next to host key | Override where the TLS cert + key are persisted. |
| `bind_address` | string | `127.0.0.1` | Interface every listener binds to (loopback by default in every mode). Set a specific IP, `0.0.0.0`/`*` (all IPv4), or `::` (all interfaces) to face the network. A non-loopback bind in **open** mode also requires `allow_insecure_exposure: true`. |
| `allow_insecure_exposure` | bool | `false` | Acknowledge a non-loopback `bind_address` in **open** mode (no transport security). Ignored in token/mtls mode. |
| `trusted_proxy` | bool | `false` | Trust `X-Forwarded-For` (only behind a proxy that overwrites it). |

`mtls` mode additionally persists a small internal CA (`ca_cert.pem` /
`ca_key.pem`, next to the TLS cert/key) the first time the server starts in
that mode — it signs client certificates only; the server's own leaf above is
unaffected. There is no separate config field for it: it is generated lazily,
logged (fingerprint + expiry) at startup, and surfaced on `GET /api/info` as
`ca_fingerprint` / `ca_not_after` (see [API reference](api.md#get-apiinfo)).

### Mounts

Mounts are directories from the host that are shared with sheds. The method depends on the backend:

- **Firecracker**: Mounted via 9P over the TAP bridge network.
- **VZ**: Mounted via VirtioFS.

Both mechanisms provide live filesystem sharing -- changes on either side are immediately visible to the other.

!!! note "Renamed from `credentials`"
    This section was previously named `credentials`. The `mounts` key has the identical shape. The deprecated `credentials` key still works as a fallback when `mounts` is absent, but new configs should use `mounts`.

```yaml
mounts:
  name:
    source: /host/path      # Path on the host (~ supported, must be a directory)
    target: /container/path  # Path inside shed
    readonly: true           # Optional, default false
```

**Mount sources must be directories.** Single-file mounts are not supported. For individual config files like `.gitconfig`, use [`shed sync`](sync.md) to push them as dotfiles. For SSH-based git authentication, use the SSH agent forwarding extension instead of mounting `~/.ssh`.

**Missing sources:** If a mount's source path does not exist on the host, it is skipped with a log warning. Create the source directory on the host before starting the shed.

**Common mounts:**

```yaml
mounts:
  # Claude Code config (needs write for token refresh)
  claude:
    source: ~/.claude
    target: /home/shed/.claude
    readonly: false

  # Codex CLI config/auth
  codex:
    source: ~/.codex
    target: /home/shed/.codex
    readonly: false

  # opencode auth + state (NOT ~/.config/opencode — that is not where opencode
  # keeps its login/state; two mounts, both writable)
  opencode_data:
    source: ~/.local/share/opencode
    target: /home/shed/.local/share/opencode
    readonly: false
  opencode_state:
    source: ~/.local/state/opencode
    target: /home/shed/.local/state/opencode
    readonly: false

  # cursor-agent auth — ~/.config/cursor ONLY. On Linux (the guest OS) the
  # cursor-agent CLI keeps its login tokens here, not under ~/.cursor. Do NOT
  # also mount ~/.cursor: shed's rc hub writes a hook relay into
  # ~/.cursor/hooks.json so cursor-agent sessions report activity/feed to the
  # hub, and it deliberately SKIPS that write whenever ~/.cursor sits on a
  # different filesystem than $HOME (its "this is a host auth mount, not a
  # guest-local dir" guard) — mounting ~/.cursor silently disables the hook
  # feed for every cursor session in the shed. Leave ~/.cursor guest-local.
  cursor:
    source: ~/.config/cursor
    target: /home/shed/.config/cursor
    readonly: false

  # GitHub CLI
  gh:
    source: ~/.config/gh
    target: /home/shed/.config/gh
    readonly: true

  # AWS credentials
  aws:
    source: ~/.aws
    target: /home/shed/.aws
    readonly: true

  # GCP credentials
  gcloud:
    source: ~/.config/gcloud
    target: /home/shed/.config/gcloud
    readonly: true
```

See `configs/server.dev-parallel.mac.yaml` for these mounts in a complete, working
server config (the same shape `make dev-server-up` uses for local agent-auth testing).

### Exclude Patterns

The mount config accepts an `exclude` field with glob patterns. This field is currently accepted but has no effect on VM backends -- VirtioFS and 9P mount entire directories. Exclude patterns are used by [`shed sync`](sync.md) path mappings. The field is retained for forward compatibility.

```yaml
mounts:
  claude:
    source: ~/.claude
    target: /home/shed/.claude
    readonly: false
    exclude:
      - "*.db"
      - "*.db-shm"
      - "*.db-wal"
      - "log/*"
      - "storage/*"
```

## Extensions

Extensions are activated per-VM by listing their namespace names. The agent reads manifests from `/etc/shed-extensions.d/` in the VM image and enables the matching systemd units at startup. When `extensions` is omitted, no extensions are activated.

```yaml
extensions:
  enabled:
    - ssh-agent
    - aws-credentials
    - docker-credentials
```

See [Extensions](extensions.md) for the full guide on the message bus, manifests, SDK, and health reporting.

## Git

When a shed is created with a `repo` whose URL uses SSH (e.g., `git@github.com:org/repo.git` or `ssh://git@host/path`), the server seeds the in-VM `~/.ssh/known_hosts` before invoking `git clone`. Without this, OpenSSH defaults to `StrictHostKeyChecking=yes` and rejects the connection with `Host key verification failed`. The `owner/repo` shorthand is expanded to `git@github.com:owner/repo.git`, so it goes through the same SSH path.

GitHub's published host keys (ED25519, ECDSA, RSA) are baked into the server binary and are always included. To trust additional hosts (GitLab, GitHub Enterprise, self-hosted Gitea, etc.), add their `known_hosts` lines to `git.extra_known_hosts`:

```yaml
git:
  extra_known_hosts:
    - "gitlab.com ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAfuCHKVTjquxvt6CM6tdG4SLp1Btn/nOeHHE5UOzRdf"
    - "my-gitea.internal ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAI..."
```

Each entry must be a syntactically valid `known_hosts` line: `<host> <key-type> <base64-key>`. The server validates this at startup and refuses to start if any entry is malformed.

**Generating entries:** Run `ssh-keyscan <host>` on a trusted machine and paste the output. For example:

```bash
ssh-keyscan gitlab.com 2>/dev/null
```

The `git.extra_known_hosts` list is *additive* — it extends the built-in defaults, never replaces them. Only SSH-form clone URLs (`git@host:path`, `ssh://...`) consult `known_hosts`; HTTPS clones skip this step entirely.

**Key rotation:** If GitHub or another host rotates its keys, you can extend the trust list via `extra_known_hosts` immediately without waiting for a shed release. The default GitHub keys ship with the server binary and are updated in releases.

## Image references and `${shed.version}`

`default_image` and `image_aliases` are **optional**. On a release build of
`shed-server`, when they are unset they are **synthesized from the server's own
version**: `default_image` becomes `ghcr.io/charliek/shed-<backend>-full:vX.Y.Z`
and the `base` / `extensions` / `full` aliases resolve to the matching tags. So
upgrading `shed-server` moves new sheds to the new images with **no config edit**.

Set them explicitly only to pin a custom or mirrored registry. To track the
server version without hand-editing the tag on every upgrade, use the
`${shed.version}` token — it expands to the running server's release tag
(`vX.Y.Z`) at config load:

```yaml
vz:
  default_image: myregistry.example.com/shed-vz-full:${shed.version}
```

**Why this is version-coupled:** the base images bundle the in-VM `shed-agent`
and the guest extension binaries, which are versioned in lockstep with
`shed`. Pinning the image to the server version — via the default synthesis or
the `${shed.version}` token — keeps the guest components compatible with the
server instead of drifting on upgrade.

!!! note "`${shed.version}` is the only accepted token"
    A literal `{version}` or `v{version}` is **not** expanded. On a dev/unreleased
    build the token has nothing to resolve to, so set concrete refs (or leave them
    unset and pass `--image` on each `create`).

## Egress Control

Opt-in, audit-first egress filtering (off unless `enabled: true`). A bad glob or
CEL rule fails server start. See [Egress Control](egress.md) for the full model,
the policy language, and the (important) honest security posture.

```yaml
egress:
  enabled: true
  port_range: "20000-30000"   # per-shed listener allocation (default)
  default: []                 # profiles for sheds created without --egress ([] = none)
  profiles:
    github:   { allow: ["*.github.com", "github.com"] }
    internal: { rule: 'host.endsWith(".corp.internal") && port == 443' }
```

| Field | Default | Description |
|-------|---------|-------------|
| `enabled` | `false` | Master switch. When off, the proxy child is never started and sheds get no egress control. |
| `port_range` | `20000-30000` | Per-shed listener port allocation range. |
| `default` | `[]` | Profiles applied to sheds created without `--egress`. `[]`/absent = no egress for those sheds. |
| `profiles` | `{}` | Named policy fragments (`allow`/`deny` globs, `mode: audit`, or a CEL `rule`). Names `off`/`none`/`default` are reserved. |

## Firecracker Configuration

When enabling Firecracker, configure the Firecracker-specific settings:

```yaml
default_backend: firecracker

firecracker:
  default_image: ghcr.io/charliek/shed-fc-full:${shed.version}
  image_aliases:
    base: ghcr.io/charliek/shed-fc-base:${shed.version}
    extensions: ghcr.io/charliek/shed-fc-extensions:${shed.version}
    full: ghcr.io/charliek/shed-fc-full:${shed.version}
  pull_policy: missing
  images_dir: /var/lib/shed/firecracker/images
  instance_dir: /var/lib/shed/firecracker/instances
  socket_dir: /var/run/shed/firecracker
  default_cpus: 2
  default_memory_mb: 4096
  default_disk_gb: 20
  vsock_base_cid: 100
  console_port: 1024
  notify_port: 1026
  tcp_proxy_port: 1028
  start_timeout: 120s
  stop_timeout: 10s
  bridge_name: shed-br0
  bridge_cidr: 172.30.0.1/24
  tap_prefix: shed-tap
```

The `${shed.version}` token above resolves to the running server's release tag —
see [Image references and `${shed.version}`](#image-references-and-shedversion).
These fields are optional; left unset on a release build they are synthesized
from the server version.

### Firecracker Fields

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `kernel_path` | string | `""` | Optional. Path to a Linux kernel image (auto-populated by published-image pulls). The Phase B initramfs prefers the kernel embedded in the image blob (`{images_dir}/blobs/sha256/<digest>/kernel`); this path is the fallback for legacy blobs that lack an embedded kernel. |
| `default_image` | string | `""` | Path or Docker ref used for new sheds when `shed create` runs without `--image`. Empty is fine if every create passes `--image`; otherwise `shed create` errors with `INVALID_REQUEST: no --image specified and no default_image configured`. |
| `image_aliases` | map | - | Optional short alias → Docker ref (or path) map for `shed create --image <alias>`. Listings always show the resolved ref, not the alias. |
| `pull_policy` | string | `missing` | `missing` (use cache, pull if absent), `always` (always pull), or `never` (error if not cached). Ignored for local-path images. |
| `images_dir` | string | `/var/lib/shed/firecracker/images` | Directory for the content-addressed blob store and the ref→digest index |
| `upper_size_default` | string | `5G` | Default logical size of the per-shed writable overlay upper layer when `shed create --upper-size` is omitted. Validated to the range 1–100 GiB. |
| `instance_dir` | string | - | Directory for VM instances |
| `socket_dir` | string | - | Directory for API/vsock sockets |
| `default_cpus` | int | `2` | Default vCPUs per VM |
| `default_memory_mb` | int | `4096` | Default memory per VM (MB) |
| `default_disk_gb` | int | `20` | Default disk size per VM (GB) |
| `vsock_base_cid` | int | `100` | Starting CID for vsock guest addressing |
| `console_port` | int | `1024` | Vsock port for VM console I/O |
| `notify_port` | int | `1026` | Vsock port for the message channel (health checks, plugins) |
| `tcp_proxy_port` | int | `1028` | Vsock port for the TCP proxy (used by `DialService` for tunnels and the Connect API). Must match the guest agent's flagless default (1028) — the systemd unit runs `shed-agent` without a `--tcp-proxy-port` override. |
| `start_timeout` | duration | `30s` | VM startup timeout |
| `stop_timeout` | duration | `10s` | Graceful shutdown timeout |
| `guest_mtu` | int | `0` | Guest primary-interface MTU. `0` (default) auto-detects the host egress path MTU at VM start and lowers the guest to match a reduced path (e.g. a VPN/overlay), otherwise leaves it at 1500. Set `1280`–`1500` to pin a value when detection misses. See note below. |
| `bridge_name` | string | `shed-br0` | Linux bridge name |
| `bridge_cidr` | string | `172.30.0.1/24` | Bridge network CIDR |
| `tap_prefix` | string | `shed-tap` | TAP device name prefix |

Path-existence validation only fires when **all** configured image sources
are local paths. When `default_image` (or any `image_aliases` entry) is a
Docker ref, the path-existence check is skipped because the file is created
on first pull. `kernel_path` is still required to point at a real file when
set non-empty.

See [Firecracker Setup](../getting-started/fc-setup.md) for setup details.

## VZ Configuration

When enabling the VZ backend on macOS Apple Silicon, configure the VZ-specific settings:

Image values in `default_image` and `image_aliases` can be either ext4 file paths or Docker image references. Docker refs are auto-pulled and converted on first use.

```yaml
default_backend: vz

vz:
  vfkit_path: vfkit
  kernel_path: ~/Library/Application Support/shed/vz/vmlinux
  initrd_path: ~/Library/Application Support/shed/vz/initrd.img
  default_image: ghcr.io/charliek/shed-vz-full:${shed.version}
  image_aliases:
    base: ghcr.io/charliek/shed-vz-base:${shed.version}
    extensions: ghcr.io/charliek/shed-vz-extensions:${shed.version}
    full: ghcr.io/charliek/shed-vz-full:${shed.version}
  pull_policy: missing
  images_dir: ~/Library/Application Support/shed/vz/
  instance_dir: ~/Library/Application Support/shed/vz/instances
  socket_dir: ~/.shed/vz/sockets
  default_cpus: 2
  default_memory_mb: 4096
  default_disk_gb: 20
  console_port: 1024
  notify_port: 1026
  tcp_proxy_port: 1028
  start_timeout: 60s
  stop_timeout: 10s
```

### VZ Fields

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `vfkit_path` | string | `vfkit` | Path to vfkit binary |
| `kernel_path` | string | `""` | Optional. Path to a decompressed Linux kernel (auto-populated by published-image pulls). Phase B prefers the kernel embedded in the image blob; this is the fallback for legacy blobs that lack an embedded kernel. |
| `initrd_path` | string | `""` | Optional. Path to an initial RAM disk image. The shed-overlay initramfs lives inside the image blob, so this field is only consulted for legacy blobs. |
| `default_image` | string | `""` | Path or Docker ref used for new sheds when `shed create` runs without `--image`. Empty is fine if every create passes `--image`; otherwise `shed create` errors with `INVALID_REQUEST: no --image specified and no default_image configured`. |
| `image_aliases` | map | - | Optional short alias → Docker ref (or path) map for `shed create --image <alias>` (see [Images](images.md)). Listings always show the resolved ref. |
| `pull_policy` | string | `missing` | `missing` (use cache, pull if absent), `always` (always pull), or `never` (error if not cached). Ignored for local-path images. |
| `images_dir` | string | `~/Library/Application Support/shed/vz/` | Directory for the content-addressed blob store and the ref→digest index |
| `upper_size_default` | string | `5G` | Default logical size of the per-shed writable overlay upper layer when `shed create --upper-size` is omitted. Validated to the range 1–100 GiB. |
| `instance_dir` | string | - | Directory for VM instances |
| `socket_dir` | string | - | Directory for vsock Unix sockets (must not contain spaces) |
| `default_cpus` | int | `2` | Default vCPUs per VM |
| `default_memory_mb` | int | `4096` | Default memory per VM (MB) |
| `default_disk_gb` | int | `20` | Default disk size per VM (GB) |
| `console_port` | int | `1024` | Vsock port for VM console I/O |
| `notify_port` | int | `1026` | Vsock port for the message channel (health checks, plugins) |
| `tcp_proxy_port` | int | `1028` | Vsock port for TCP proxy (used by DialService for tunnels and Connect API) |
| `start_timeout` | duration | `60s` | VM startup timeout |
| `stop_timeout` | duration | `10s` | Graceful shutdown timeout |
| `guest_mtu` | int | `0` | Guest primary-interface MTU. `0` (default) auto-detects the host egress path MTU at VM start and lowers the guest to match a reduced path (e.g. a VPN), otherwise leaves it at 1500. Set `1280`–`1500` to pin a value when detection misses. See note below. |

### Guest MTU and VPNs

Behind a VPN (or any path whose MTU is below 1500), full-size guest packets can
be silently dropped — most visibly, `docker pull` fails with a TLS handshake
timeout while `curl` to the same registry works. With `guest_mtu: 0` (the
default), shed-server detects the host's egress path MTU when a shed starts and
lowers the guest interface to match; on a normal 1500 path nothing changes.

Detection runs **at VM start**, so if you connect or disconnect the VPN while a
shed is already running, run `shed restart <name>` to re-detect. Set `guest_mtu`
explicitly only to override detection (for example, to pin a value for a VPN
whose tunnel interface still reports 1500).

See [VZ Setup](../getting-started/vz-setup.md) for setup details.

## Environment File

**Location:** As configured in `env_file` (typically `~/.shed/env`)

Environment variables injected into all containers:

```bash
ANTHROPIC_API_KEY=sk-ant-...
OPENAI_API_KEY=sk-...
GITHUB_TOKEN=ghp_...
```

Set restricted permissions:

```bash
chmod 600 ~/.shed/env
```

## SSH Known Hosts

**Location:** `~/.shed/known_hosts`

Stores SSH host keys for shed servers. Populated automatically when running `shed server add`.

## Sync Configuration

See [File Sync](sync.md) for sync configuration.

## Tunnel Configuration

See [Tunnels](tunnels.md) for tunnel configuration.
