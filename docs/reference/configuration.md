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
| `servers.<name>.tls_cert_fingerprint` | string | Pinned TLS cert fingerprint (`sha256:...`) — the server's own leaf, pinned for an mtls server. |
| `servers.<name>.control_token` | string | Control-scoped bearer token for the CLI/desktop, from a server older than v0.10.0 running token mode. Auto-written and refreshed by `shed server add`. Absent for an mtls server. |
| `servers.<name>.control_token_expires_at` | timestamp | Expiry of `control_token`; the CLI refreshes near this and on a 401. Auto-managed. |
| `servers.<name>.auth_mode` | string | The credential shape the server last issued at bootstrap: `mtls`, or `token` from a server older than v0.10.0. **Absent means `token`** (every entry written before certificate support existed predates this field). Not a setting — it's a cache of what the server last said; a bootstrap that comes back in the other mode rewrites it, which is how a server moved from token to mtls is picked up without re-adding every client. |
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
| `name` | string | `shed-server` | Server identifier. Also names this server's nftables tables for the [`isolated` profile](#network-profiles) (`bridge`/`inet shed_<name>`, every character outside `[A-Za-z0-9_]` as `_`), so two servers on one host need names that still differ after that. |
| `http_port` | int | `8080` | Plain-HTTP API port. **Required in open mode; optional in mtls mode** (mtls serves no plain HTTP, so it is omitted from `/api/info` and the written client config entry). |
| `ssh_port` | int | `2222` | SSH server port |
| `bind_address` | string | `127.0.0.1` | Interface every listener (HTTP, HTTPS, SSH) binds to. **Defaults to loopback in every mode** — shed is local-first. In mtls mode set a specific IP (LAN/tailnet), `0.0.0.0` or `*` (all IPv4), or `::` (all interfaces) to face the network. **An open-mode server is loopback only**: a non-loopback bind fails config load (the old `allow_insecure_exposure` acknowledgment was removed in v0.10.0 and is itself refused). |
| `trusted_proxy` | bool | `false` | Trust the client-supplied `X-Forwarded-For` header for the request source IP. Only enable behind a reverse proxy that overwrites that header; the default uses the real TCP peer address. |
| `default_backend` | string | `detect` | Backend to use when none is specified (`detect`, `firecracker`, `vz`, `remote`). `detect` auto-selects based on platform: `vz` on macOS, `firecracker` on Linux; it never picks `remote`. `remote` runs each shed as a cloud VM and needs a `remote:` block (a `remote:` block with any other backend fails config load); see [Remote Backend](remote-backend.md#configuration). |
| `mounts` | map | `{}` | Host directories to mount into sheds (formerly `credentials`) |
| `env_file` | string | - | Path to a `KEY=value` file whose variables are passed to the in-VM `git clone` of `repo` (git settings and credentials such as `GIT_SSH_COMMAND`); see [Environment File](#environment-file) |
| `log_level` | string | `info` | Logging level (debug, info, warn, error) |
| `extensions` | object | `{}` | Extensions to activate in VMs (see [Extensions](extensions.md)) |
| `git` | object | - | Git behaviour for in-VM clones (see [Git](#git)) |
| `firecracker` | object | - | Firecracker-specific configuration (see below) |
| `vz` | object | - | VZ-specific configuration (see below) |
| `auth` | object | - | Optional authentication: the [mode](#authentication-mode), [SSH key sources](#ssh-authentication), and in mtls mode [users](#users) and [service clients](#service-clients) |
| `state_dir` | string | `/etc/shed` as root, `~/.shed` otherwise | Identity material (see [TLS and network surface](#tls-and-network-surface)). A second server on the same host needs its own `state_dir` and a distinct `name`. |
| `exec` | object | - | Limits for the HTTPS exec endpoint (see [HTTPS exec](#https-exec)) |
| `audit` | object | - | The audit log (see [Audit log](#audit-log)) |
| `network` | object | - | Network profiles (see [Network profiles](#network-profiles)) |

**Note:** Only VM backends are supported. Firecracker is available on Linux. VZ is available on macOS Apple Silicon (arm64). The `detect` backend auto-selects based on platform.

### Authentication mode

`auth.mode` is the headline switch, and it has two values. The default `open`
is a local posture: plain HTTP, no credential, no TLS, **loopback only**; the
SSH allowlist (`auth.ssh`) is the one independently-tunable layer there. `mtls`
is the posture for anything reachable from another host: it derives a hardened
bundle (SSH enforce + client-certificate enforce + TLS-only) and refuses to
start without an SSH key source. The client credential is a short-lived
certificate, bound to a key that never leaves the client, issued over SSH. Two
invariants hold: **credential ⟺ TLS ⟺ mtls** and **https ⟺ mtls**. An
orchestrator that needs only its own sheds can be a
[service client](#service-clients) of an mtls server. See
[Security](security.md) — in particular [mTLS mode](security.md#mtls-mode) —
and the [Security Configuration guide](../guides/security-configuration.md).

The bearer-token mode (`auth.mode: token`, and its older spelling `secure`)
was **removed in v0.10.0**: a config that names it fails to load with a message
naming `mtls`, and so does `allow_insecure_exposure` and an open server bound
beyond loopback. Nothing falls back to open. Switch such a server to
`auth.mode: mtls`; clients re-enroll with a certificate on their next request.
Current clients still speak the token protocol to servers older than v0.10.0.

| `auth.mode` | HTTP credential | SSH allowlist | Notes |
|-------------|------------------|----------------|-------|
| `open` (default) | none | as configured (`auth.ssh.mode: off`/`warn`) | Plain HTTP on loopback only; `https_port` and a non-loopback `bind_address` rejected. |
| `mtls` | client certificate, issued over the `_bootstrap` SSH channel (CSR in, signed cert out); revalidated on every request | forced `enforce` | TLS-only. Revocation removes the SSH key. `curl`/scripts cannot authenticate as an admin; a [service client](#service-clients)'s bundle works with `curl --cert`. |

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `auth.mode` | string | `open` | `open` enforces nothing (plain HTTP, loopback only); `mtls` derives SSH-allowlist enforce + client-certificate enforce + TLS-only (`https_port` defaults to `8443`; **no plain-HTTP listener is served**), and **fails to start without a key source**. `token` and `secure` were removed in v0.10.0 and fail config load. |
| `auth.token_ttl` | duration | `24h` | Lifetime of an issued client certificate (the name is kept from the removed token mode). Clients re-enroll transparently near expiry / on an auth failure. |
| `auth.users` | map | `{}` | Named users, keyed by name. **Requires `auth.mode: mtls`**, and replaces `auth.ssh`'s key sources. See [Users](#users). Beside `auth.directory`, the local admins only. |
| `auth.directory` | object | - | Take the users from an account service. **Requires `auth.mode: mtls`** and at least one local admin in `auth.users`. See [Users from an account service](#users-from-an-account-service). |
| `auth.clients` | map | `{}` | Service clients, keyed by name. **Requires `auth.mode: mtls`** (refused at load otherwise). See [Service clients](#service-clients). |

Keys removed in earlier releases (`public_exposure`, `auth.http`,
`internal_http_port`, `http_bind`, `ssh_bind`) are still **rejected at
startup** with a message naming them; see [Security › Removed
earlier](security.md#removed-earlier).

### SSH authentication

`auth.ssh` gates SSH public-key access against an allowlist on a server with
**no user list**, where every accepted key is the implicit user, an admin. The
default (omitted, or `mode: off`) accepts all keys. Identity comes from the
offered key, GitHub-style; the username selects the shed. On a server with
[users](#users), keys come from `auth.users` instead: the three key sources
below are refused beside it, while `github_refresh` and `max_auth_tries` still
apply.

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `auth.ssh.mode` | string | `off` | `off` accepts all keys; `warn` logs `SSH auth WOULD DENY` for a key not on the list but still accepts it (open mode only; use it to roll out safely); `enforce` rejects keys not on the list (forced in mtls mode, and valid only there). |
| `auth.ssh.authorized_keys` | list | `[]` | Inline OpenSSH `authorized_keys` lines. |
| `auth.ssh.authorized_keys_file` | string | - | Path to an `authorized_keys`-format file. |
| `auth.ssh.github_users` | list | `[]` | GitHub logins whose keys are allowed. Each login is pinned to its numeric account id at its first fetch, and its keys are read from the GitHub REST API (`api.github.com`) by that id; see [Security › GitHub logins are pinned](security.md#github-logins-are-pinned-to-their-account-id). The last known keys are kept when GitHub is unreachable. Logins that differ only in case are refused. |
| `auth.ssh.github_refresh` | duration | `1h` | How often to re-fetch GitHub keys (for `auth.users` logins too). |
| `auth.ssh.max_auth_tries` | int | `10` | Public-key attempts allowed per connection. Raise it when a client's agent (1Password, Secretive) holds many keys, so the allowlisted key is tried before the server gives up. |

With `mode: enforce`, the server refuses to start if no keys resolve (empty inline/file and a failed GitHub fetch with no cache) — use `warn` for first boot, or provide inline keys.

mtls needs a key source: `auth.users`, or one of `github_users`,
`authorized_keys` and `authorized_keys_file`. Without one the server exits
with `auth.mode: mtls requires at least one SSH key source (auth.users, or
auth.ssh.github_users, authorized_keys, or authorized_keys_file)`.

```yaml
auth:
  mode: mtls                 # open (default, loopback only) | mtls — mtls forces ssh enforce
  ssh:
    github_users: [alice-gh]  # a key source is required in mtls mode
```

Keys are case-sensitive. An unknown key anywhere under `auth` (`auth.ssh` and
`auth.clients` included) fails config load, so `Mode: mtls` or a misspelt key
cannot leave the server open. A top-level key that matches a known one only
when case is ignored (`Auth:`, `Bind_Address:`) fails load too; other unknown
top-level keys are ignored.

### Users

`auth.users` gives a server **named users**. Without it every accepted key is
one implicit user, an admin, and every caller reaches every shed. With it
every key belongs to exactly one user, each user reaches only their own sheds
and snapshots, and an admin may also list, stop and delete anyone's (never
enter another user's). The rules are in [Security ›
Ownership](security.md#ownership); the [Multi-user server
guide](../guides/multi-user-server.md) walks through adding a list.

```yaml
auth:
  mode: mtls                       # required with auth.users
  users:
    alice:
      github: alice-gh             # her GitHub keys, pinned to the account id
      admin: true
    bob:
      github: bob-gh
      github_id: 1234567           # optional: the account id the login must name
      keys:
        - key: ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAA... bob@laptop
          label: laptop
          expires: 2027-06-30      # accepted through that day, UTC
        - key: ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAA... ci
          label: ci
          script: true             # automation: no credential bus
```

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `auth.users.<name>` | object | - | One user. The name matches `^[a-z][a-z0-9-]{0,62}$` and may not also be an `auth.clients` name. It is recorded on the user's sheds and snapshots. |
| `auth.users.<name>.github` | string | - | A GitHub login whose published keys are this user's, pinned to its numeric account id at the first fetch (see [Security › GitHub logins are pinned](security.md#github-logins-are-pinned-to-their-account-id)). A login belongs to one user (compared without case). |
| `auth.users.<name>.github_id` | int | - | The account id the login must name. Needs `github`. When set it wins over the pin the server stored, which is how to follow an account that was deleted and created again on purpose. |
| `auth.users.<name>.admin` | bool | `false` | An admin may list, stop and delete any shed (and enter a service client's), and use what the host shares: host directories, server mounts, images that are not the default or an alias, the image store, `system prune`, egress profile changes. `true` or `false` only, unquoted. |
| `auth.users.<name>.keys` | list | `[]` | Keys listed by hand (a phone, a CI key, a key not on GitHub). |
| `auth.users.<name>.keys[].key` | string | required | One OpenSSH public key line, with no `authorized_keys` options (`from=`, `command=` and the like are refused) and nothing after it. An SSH certificate is refused: list the public key itself. A key may appear once in the whole list. |
| `auth.users.<name>.keys[].label` | string | required | Names the key in logs and as the mover of a credential source. Unique per user. |
| `auth.users.<name>.keys[].expires` | string | - | When the key stops being accepted: an RFC 3339 time, or a date (`YYYY-MM-DD`), meaning the end of that day in UTC. An expired key is refused at the SSH and TLS handshakes and on every request; open channels stay open. |
| `auth.users.<name>.keys[].script` | bool | `false` | A key used by automation: it reaches the user's sheds, but cannot take a `credentials` certificate, use the credential bus, or see or move the user's credential source. `true` or `false` only. |

Config load refuses, naming the field:

- `auth.users` with any `auth.mode` but `mtls` (`auth.users requires auth.mode: mtls`);
- `auth.users` beside `auth.ssh.authorized_keys`, `authorized_keys_file` or
  `github_users` (list each key or login under its user instead);
- a user with neither `github` nor a key;
- a name that is not valid, or that is also a service client's;
- the same GitHub login for two users, or the same key twice (under one user
  or two);
- a key line that is not one plain public key, carries options, or is an SSH
  certificate; a missing or repeated `label`; an `expires` that is neither an
  RFC 3339 time nor a date;
- `github_id` without `github`, or a negative `github_id`;
- `admin` or `script` set to anything but an unquoted `true` or `false` (`yes`
  and `on` included);
- a list with no admin who has a GitHub login or a key not marked `script`.

`auth.users: {}` is the same as no user list. An expired key is not a load
error: it is simply not accepted, and the server logs it at start.

### Users from an account service

`auth.directory` takes the server's users from an **account service**: a
service that holds people and their SSH login keys and hands an enrolled
server its user list over the [directory contract](directory-contract.md).
Beside it, `auth.users` holds the **local admins** only: admins whose keys are
in this config, who can log in whatever state the service is in. Everyone else
comes from the service's list. The [User Directory
guide](../guides/user-directory.md) walks through enrolling a server; the
security model is in [Security › Users from an account
service](security.md#users-from-an-account-service).

```yaml
auth:
  mode: mtls                       # required
  users:                           # beside a directory: the local admins only
    admin:
      admin: true
      keys:
        - key: ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAA... admin@laptop
          label: laptop
  directory:
    url: https://accounts.example.test   # the service's issuer: an origin, no path
    organization: example                 # optional
    refresh: 24h
    trust: 72h
```

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `auth.directory.url` | string | required | The service's issuer: an origin (scheme, host and optional port) with no path, query, fragment or user info, the host written plainly (ASCII letters, digits, `-` and `.`, an IPv4 address, or an IPv6 address in brackets). One trailing `/` is dropped; the result is compared byte for byte with the issuer the service names, and is the audience of the server's sign-in. `https://`, or `http://` only with `allow_plain_http`. |
| `auth.directory.organization` | string | - | The organization every answer must name (`^[a-z][a-z0-9-]{0,62}$`). Unset, the server accepts the organization its enrolment answer named. |
| `auth.directory.refresh` | duration | `24h` | How long after a good pull the next one is due. From `1m` to `168h` (7 days). |
| `auth.directory.trust` | duration | `72h` | How long after a good pull its list is honoured while no newer one arrives. From `refresh` to `720h` (30 days). After it only the local admins can log in until a good pull. |
| `auth.directory.allow_plain_http` | bool | `false` | Permits an `http://` URL, and only on a loopback host (`localhost`, `127.0.0.0/8`, `::1`). For tests and trials with the [reference server](directory-contract.md#the-reference-server). `true` or `false` only. |

Durations are Go durations (`90m`, `168h`); a day is written in hours.

Config load refuses, naming the field:

- `auth.directory` with any `auth.mode` but `mtls` (`auth.directory requires
  auth.mode: mtls`);
- no local admin (`auth.directory needs at least one local admin in
  auth.users (admin: true, with a key): the local admins are the users who can
  log in whatever state the directory is in`);
- an `auth.users` entry without `admin: true`, with `github` or `github_id`,
  or with a key marked `script` (a local admin has keys only);
- a `url` that is missing, not an origin, not written plainly, or plain HTTP
  without `allow_plain_http`; `allow_plain_http: true` beside a host that is
  not loopback;
- an `organization` that does not match the pattern;
- a `refresh` or `trust` out of its range (a value written as `0` is out of
  range; omit the field for the default);
- an unknown key, a key written twice, a merge key (`<<`) or a key with no
  value under `auth.directory`.

Every rule of [Users](#users) still applies to the local admins: `mtls`, no
`auth.ssh` key source beside them (so no `github_users`), names, labels and
keys. `auth.ssh.max_auth_tries` applies; `github_refresh` has nothing to
fetch.

Fixed, not settings: the retry 15 minutes after a failed pull; the longest
`Retry-After` the server honours, one hour; one pull's bound, 30 seconds; and
the state directory `{state_dir}/directory/` (mode `0700`), which holds
`enrolment.json` (the enrolment and this server's private key) and `users.json`
(the kept copy of the last good pull), each mode `0600`. There is no setting
to ignore the list's admin flag.

`shed-server config-validate` prints the directory's summary, for example
`users: from the directory at https://accounts.example.test (organization
example), refresh 24h0m0s, trust 72h0m0s` and `local admins: admin`.

### TLS and network surface

TLS and network-surface fields. HTTP credential enforcement is **not** an
independent field — it is derived from `auth.mode: mtls` (there is no
`auth.http` block). mtls turns on TLS for you, and `https_port` is valid only
in mtls mode. See
[Security](security.md) and the
[Security Configuration guide](../guides/security-configuration.md).

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `https_port` | int | `8443` in mtls mode | Serve HTTPS with a self-signed, client-pinned cert, bound to `bind_address`. **Requires `auth.mode: mtls`** (rejected in open mode); defaults to `8443` there. It is the *only* listener — no plain HTTP is served — and it runs `RequireAndVerifyClientCert` against the internal CA (see [mTLS mode](security.md#mtls-mode)). |
| `tls_names` | list | `[]` | Extra hostnames/IPs added as cert SANs (on the server's own leaf; unrelated to the mtls client-CA). `localhost`, `127.0.0.1`, `::1` are always included. |
| `tls_cert_file` / `tls_key_file` | string | next to host key | Override where the TLS cert + key are persisted. |
| `state_dir` | string | `/etc/shed` as root, `~/.shed` otherwise | Directory for the server's identity material: the SSH host key, the TLS cert + key (unless overridden above), the mtls client CA, the GitHub key cache (`github_keys/`), the GitHub account pins (`github_pins.json`), the users upgrade marker (`users-upgrade.json`) and, with `auth.directory`, `directory/` (the enrolment and the kept copy of the user list; see [Users from an account service](#users-from-an-account-service)). Absolute or `~/`-relative; created `0700` at start. Set it when a second server runs on the same host, and give that server a `name` that differs from the first's after sanitization (the `name` also names its nftables tables). |
| `bind_address` | string | `127.0.0.1` | Interface every listener binds to (loopback by default in every mode). In mtls mode set a specific IP, `0.0.0.0`/`*` (all IPv4), or `::` (all interfaces) to face the network. An **open** server is loopback only; a non-loopback bind fails config load. |
| `trusted_proxy` | bool | `false` | Trust `X-Forwarded-For` (only behind a proxy that overwrites it). |

`mtls` mode additionally persists a small internal CA (`ca_cert.pem` /
`ca_key.pem`, next to the TLS cert/key) the first time the server starts in
that mode — it signs client certificates only; the server's own leaf above is
unaffected. There is no separate config field for it: it is generated lazily,
logged (fingerprint + expiry) at startup, and surfaced on `GET /api/info` as
`ca_fingerprint` / `ca_not_after` (see [API reference](api.md#get-apiinfo)).

### Service clients

`auth.clients` declares **service clients**: orchestrators that reach the API
with a certificate from `shed-server client issue` and may create, run commands
in and delete only their own sheds, within a policy. They exist only in
`auth.mode: mtls`. The [Orchestrated sheds guide](../guides/orchestrated-sheds.md)
walks through setting one up; [Security › Service
clients](security.md#service-clients) says what a client can and cannot do.

```yaml
auth:
  mode: mtls
  ssh:
    github_users: [alice-gh]
  clients:
    nocturn:
      cert_max_ttl: 2160h            # default 90 days
      policy:
        name_prefix: "nocturn-"      # required
        max_sheds: 4                 # required
        max_age: 30h                 # required
        network: isolated            # default isolated
        server_mounts: []            # default none
        images: []                   # default: the server default image only
        caps: {cpus: 4, memory_mb: 8192}
```

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `auth.clients.<name>` | object | - | One client. The name matches `^[a-z][a-z0-9-]{0,62}$`; it is the certificate's identity and the `owner` of the client's sheds. |
| `auth.clients.<name>.cert_max_ttl` | duration | `2160h` (90 days) | The longest certificate `shed-server client issue` signs for this client, and its default lifetime. |
| `auth.clients.<name>.policy.name_prefix` | string | required | Every shed the client creates must start with it. Lowercase letters, digits and hyphens, starting with a letter. |
| `auth.clients.<name>.policy.max_sheds` | int | required, at least 1 | The most sheds the client may own at once, running or stopped (creates in flight count). |
| `auth.clients.<name>.policy.max_age` | duration | required, positive | How old an owned shed may get. The server stops and deletes an owned shed once it is older than this (checked at start, then every 60 s). |
| `auth.clients.<name>.policy.network` | string | `isolated` | The network profile every shed of this client gets (`open` or `isolated`); a create that asks for another is refused. On a VZ server it must be set to `open` explicitly. |
| `auth.clients.<name>.policy.server_mounts` | list | `[]` (none) | Names from the top-level `mounts:` the client's sheds may get. A create that omits `mounts` gets exactly these. Each name must exist in `mounts`. |
| `auth.clients.<name>.policy.images` | list | `[]` | `image_aliases` keys of the active backend the client may ask for. Empty: only the server's `default_image`. Each must be an alias. |
| `auth.clients.<name>.policy.caps.cpus` / `caps.memory_mb` | int | `0` (no cap) | Per-shed resource caps, compared after the backend's defaults are applied. |

Unknown keys anywhere under `auth.clients` are refused at load, so a typo in a
policy fails loudly instead of being ignored. Clients are read once at start:
**removing a client and restarting `serve` revokes its certificates** (there is
no serial revocation yet). Configuring any client turns the [audit
log](#audit-log) on.

### HTTPS exec

Limits for [`POST /api/sheds/{name}/exec`](api.md#post-apishedsnameexec). Every
field is optional; zero means the default.

```yaml
exec:
  max_stdin_bytes: 1048576
  max_timeout: 1h
  max_concurrent_per_shed: 8
```

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `exec.max_stdin_bytes` | int | `1048576` (1 MiB) | Largest decoded `stdin` a request may carry (`413 REQUEST_TOO_LARGE` above it). |
| `exec.max_timeout` | duration | `1h` | Largest `timeout_seconds` a request may ask for (refused above it, never clamped). At least `1s`. A request with no timeout gets 60 s, or this value if it is lower. |
| `exec.max_concurrent_per_shed` | int | `8` | HTTPS execs one shed may run at once (`429 EXEC_LIMIT` above it). |

Negative values are refused at load.

### Audit log

shed-server can keep a JSON Lines audit log, `audit.jsonl`, next to
`egress-audit.jsonl` in the parent of the backend's `instance_dir`
(`/var/lib/shed/firecracker/audit.jsonl` with the Firecracker defaults,
`~/Library/Application Support/shed/vz/audit.jsonl` with the VZ defaults). It is
created `0600` and rotated at 50 MiB, keeping five old files (`audit.jsonl.1`
is the newest). A server that needs the log and cannot open it does not start.

```yaml
audit:
  enabled: true      # on anyway whenever auth.clients is set
  exec_argv: ""      # "" (argv[0] and the count) or "full"
```

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `audit.enabled` | bool | `false` | Turns the log on for a server without service clients (it then records HTTPS execs). The log is always on when `auth.clients` is set, whatever this says. |
| `audit.exec_argv` | string | `""` | `""` records only `argv0` and `argc` for each exec; `full` records the whole `argv`, which can carry prompts or secrets. Any other value is refused. |

Each line is one JSON object with `ts` first:

| `event` | Written for | Fields |
|---------|-------------|--------|
| `api` | Every service-client request, allowed or refused (401, 403, 404 and policy refusals included). A person's API requests are not logged. | `identity` (`kind: client`, `name`, `serial`), `request_id`, `remote`, `method`, `route` (the matched pattern, `""` when none matched), `path`, `shed`, `status`, `duration_ms` |
| `exec.end` | Every HTTPS exec, a person's or a client's, that passed the request checks and was handed to the backend, however it ended (a timeout before the command started included). An exec refused with `400`, `404`, `409`, `413` or `429` writes none. | `identity` (`kind: client`, or `kind: user` for a person; see below), `request_id`, `shed`, `argv0` and `argc` (or `argv` with `exec_argv: full`), `exit_code` (`-1` when there is none), `timed_out`, `truncated`, `disconnected`, `duration_ms` |
| `reap` | Each shed the `max_age` reaper deleted, and each delete it tried that failed (the shed may still exist; the next pass tries again and writes another line) | `identity` (always `kind: system`), `shed`, `owner`, `age_seconds`, `error` (the backend's error, only when the delete failed) |

The `identity` member says who acted, by `kind`:

| `kind` | Who | Members |
|--------|-----|---------|
| `client` | A service client (`auth.clients`) | `name`, `serial` (the certificate's serial) |
| `user` | A person: a user of the server, or the one user of a server with no user list | `user` (the user's name), `admin` and `script` (always present, `true` or `false`: whether the person is an admin, and whether they used a script key), `subject` (the key's fingerprint), `serial` (the client certificate's serial, when there is one). The one user of a server with no user list carries `implicit: true`, `admin: true` and no `user`; in `open` mode there is no certificate, so no `subject` or `serial` either. |
| `system` | The server acting for itself (the `max_age` reaper) | none |
| `unknown` | A caller the server could not name; never expected, since such a request is refused before it reaches a shed | none |

```json
{"kind":"client","name":"build-1","serial":"3fa2"}
{"kind":"user","user":"pat","admin":false,"script":false,"subject":"SHA256:…","serial":"…"}
{"kind":"user","implicit":true,"admin":true,"script":false}
{"kind":"system"}
```

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

**Per-shed selection:** by default every shed gets every mount, as before. On a
server with [users](#users), server mounts are for admins: a user who is not
an admin gets none and is refused a named one. A
shed can be created with fewer: `shed create --mounts none` or
`--mounts claude,gh` (the API's `mounts` field). The selection is stored with
the shed and applies on every start, including an SSH auto-start; a selected
name that is later removed from `mounts:` is skipped with a log line. A
[service client](#service-clients)'s sheds get only its `server_mounts`
(none by default).

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
  # cursor-agent CLI keeps its login tokens here, not under ~/.cursor. Leave
  # ~/.cursor guest-local: it is cursor's own per-machine configuration, and
  # the shed's roost-session wires its status hooks into an agent's
  # configuration inside the shed (see "Putting roost-session on a shed or
  # machine") — a mount would hand it the host's copy instead.
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

## Network profiles

Each shed has a network profile, fixed at create: `open` (today's network, the
default) or `isolated` (the internet and nothing else: not the host, other
sheds, private, CGNAT/tailnet, link-local or multicast ranges, and no IPv6).
`isolated` is enforced by nftables tables shed-server owns, and is available on
Firecracker only. See [Security › Network profiles](security.md#network-profiles)
for what it guarantees and its limits.

```yaml
network:
  default_profile: open        # open (default) | isolated
  allow_cidrs: []              # IPv4 ranges isolated sheds may still reach
```

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `network.default_profile` | string | `open` | The profile a create that names none gets (`shed create` without `--network`). `isolated` requires the Firecracker backend; a VZ server refuses it at load. |
| `network.allow_cidrs` | list | `[]` | IPv4 CIDRs an isolated shed may reach although they fall in a blocked range (a LAN registry, say). Forwarded traffic only: they never open the host itself or other sheds. |

Unknown keys under `network:` are refused at load. A server with no isolated
sheds and an `open` default installs no rules and needs no `nft` binary; one
that needs them (an isolated shed exists, or the default is `isolated`) needs
`nft` on its `PATH` (tested with nftables 1.0.9), run with the server's own
privileges, so the server must be allowed to change nftables (the deb
package's unit runs it as root). If it cannot install them, an isolated shed
is refused (`503 NETWORK_ENFORCEMENT_UNAVAILABLE`), never run unguarded. A service client's
sheds use its `policy.network` instead of the default.

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

`KEY=value` lines, passed as the environment of the in-VM `git clone` that
`shed create --repo` runs, so a private repository can be cloned with git
settings and credentials from the host (see [Firecracker Operations › Private
Repository Access](fc-operations.md#private-repository-access)). They are not
set in shell sessions, `shed exec`, or provisioning hooks, and a [service
client](#service-clients)'s sheds never get them. On a server with
[users](#users) only an admin's sheds get them; a user who is not an admin is
not refused, the clone simply runs without them.

```bash
GIT_SSH_COMMAND=ssh -i /home/shed/.ssh/id_ed25519
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
