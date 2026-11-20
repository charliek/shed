# Configuration

## Host-Side Configuration

The credential broker, which runs inside the [shed desktop app](../desktop/index.md), reads
its configuration from `~/.config/shed/extensions.yaml`. Restart the app after you change it.

```yaml
# shed-server URL. Leave this unset: the app then brokers for every server in
# ~/.shed/config.yaml; setting it limits the app to that one server.
# server: http://localhost:8080

# How long to wait for a shed-desktop approval before failing closed (default 25s)
# approval_timeout: 25s

ssh:
  # SSH backend: "agent-forward", "local-keys", or "" (auto-detect)
  # mode: ""
  approval:
    policy: biometrics-or-password   # see "Approval model" below
    scope: per-session               # biometric policies only
    session_ttl: 1h
  # What this machine signs for a shed (see SSH Agent → What the machine signs):
  # allow_users: [git]               # usernames a login is signed for (default [git])
  # offer_keys:                      # keys offered to sheds (default: every key)
  #   - SHA256:QSbP+oRmKHQkEp2CsJn72x/5Qxyq5lsK6sib0fK1mr0

aws:
  source_profile: default
  default_role: arn:aws:iam::123456789012:role/acmeco-dev
  mode: assume-role                  # assume-role (default) | passthrough (SSO/SAML, see AWS Credentials)
  session_duration: 1h
  cache_refresh_before: 5m
  approval:
    policy: deny-all                 # off until configured; then approve-all | shed-desktop
  # Per-server / per-shed role and mode overrides
  servers:
    devbox:
      sheds:
        my-service:
          role: arn:aws:iam::123456789012:role/acmeco-dev
        sso-app:
          mode: passthrough          # vend source_profile's session creds directly

docker:
  registries:
    - index.docker.io                # Docker Hub — list as index.docker.io, not docker.io
    - ghcr.io
    - registry.acmeco.com
  # allow_all: true         # bypass allowlist
  approval:
    policy: approve-all              # deny-all | approve-all | shed-desktop

# Audit logging
logging:
  enabled: true
  path: ~/.local/share/shed/extensions-audit.log
```

!!! note "`rc_hub:` is retired"
    A config file may still carry a `rc_hub:` block from before v0.9.0 (the agent used
    to host the machine RC activity hub on `127.0.0.1:1029`). Plan 022 (S6,
    [`charliek/shed#328`](https://github.com/charliek/shed/issues/328)) removed the hub —
    machine session activity now comes from [roost](roost-session-hosts.md).
    The key is **ignored, not rejected**: a config that still carries it loads unchanged,
    so there is nothing to do beyond deleting the stanza whenever it suits you.

### Approval model

Every extension (`ssh`, `aws`, `docker`) has a required `approval.policy`. An
omitted/empty policy means **`deny-all`** (fail closed). Values:

| Policy | Behavior |
|--------|----------|
| `deny-all` | Reject every request. The safe default / kill-switch. |
| `approve-all` | Allow every request. Any allowlist/role below still applies — this only removes the approval prompt. |
| `shed-desktop` | Decide each request in the [shed-desktop](../desktop/approvals.md) app: SSH shows an interactive card (scope, TTL, always-allow, always-deny); AWS and Docker show a live Allow/Deny toggle. Its Preferences own method/scope/TTL. If no one answers within `approval_timeout`, the request **fails closed**. An extension whose policy is anything else is handled by the broker itself and only streams its events to the app's Activity feed. |
| `biometrics` | **SSH only, macOS.** Native Touch ID, biometrics only (fails with no sensor). Off macOS it denies every request. |
| `biometrics-or-password` | **SSH only, macOS.** Native Touch ID, Apple Watch, **or** account password — works in clamshell mode and on Macs without a sensor. Off macOS it denies every request. |

`scope` and `session_ttl` apply **only** to the native biometric policies (they
cache an approval so you aren't prompted on every operation). Under
`shed-desktop` the app owns scope/TTL; under `deny-all`/`approve-all` they are unused.

The top-level **`approval_timeout`** (Go duration, default `25s`) bounds how long
the broker waits for a `shed-desktop` decision before failing closed.

A top-level `desktop:` block (`enabled`, `socket_path`, `timeout_ms`, from the retired
credential daemon) fails the load: ``the `desktop:` block was removed; delete it (a
`desktop.timeout_ms` becomes `approval_timeout` with a unit: 30000 ms is `approval_timeout:
30s`)``. The app then starts no broker and shows the
message in Preferences › Credential broker. Delete the block.

For SSH, the policy decides only requests that have already passed the signing check: a
request the check refuses never reaches the policy and shows no prompt (see
[SSH Agent → What the machine signs](ssh-agent.md#what-the-machine-signs)).

### SSH Settings

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `ssh.mode` | string | `""` (auto) | SSH backend mode. Auto-detect selects agent-forward if `SSH_AUTH_SOCK` is set and its socket answers, else local-keys. |
| `ssh.approval.policy` | string | `deny-all` | One of `deny-all`, `approve-all`, `biometrics`, `biometrics-or-password`, `shed-desktop` (see Approval model) |
| `ssh.approval.scope` | string | `per-session` | Biometric policies only: `per-request`, `per-session`, `per-shed` |
| `ssh.approval.session_ttl` | string | `4h` | Biometric policies only: how long a cached approval remains valid |
| `ssh.allow_users` | []string | `[git]` | The usernames an SSH login signature may be for. A present list replaces the default; `[]` signs no login. A name beginning with `_` fails config load; any name other than `git` requires `ssh.offer_keys`. See [What the machine signs](ssh-agent.md#what-the-machine-signs) |
| `ssh.offer_keys` | []string | absent (every key) | The keys sheds may list and sign with, each a SHA256 fingerprint or an OpenSSH public key line. `[]` offers none. A certificate, `sk-` key, DSA key or invalid key fails config load |

### AWS Settings

`approval.policy` is one of `deny-all`, `approve-all`, `shed-desktop`. The role
fields below are authorization (which role to assume) and must be set here —
there is no shed-desktop Preferences UI for them.

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `aws.approval.policy` | string | `deny-all` | `deny-all`, `approve-all`, or `shed-desktop` |
| `aws.source_profile` | string | `default` | AWS profile vended: the AssumeRole source, or (passthrough) the session creds served directly |
| `aws.default_role` | string | | IAM role ARN to assume (required for assume-role mode) |
| `aws.mode` | string | `assume-role` | `assume-role` (default) or `passthrough` (vend `source_profile` session creds directly, for SSO/SAML) |
| `aws.session_duration` | string | `1h` | STS session token lifetime (assume-role mode) |
| `aws.cache_refresh_before` | string | `5m` | Refresh cached credentials when less than this time remains (assume-role mode) |
| `aws.servers.<server>.default_role` | string | | Per-server role default (multi-server mode) |
| `aws.servers.<server>.mode` | string | | Per-server mode override |
| `aws.servers.<server>.session_duration` | string | | Per-server session token lifetime (assume-role mode) |
| `aws.servers.<server>.sheds.<shed>.role` | string | | Per-server, per-shed role override (most specific) |
| `aws.servers.<server>.sheds.<shed>.mode` | string | | Per-server, per-shed mode override (most specific) |
| `aws.servers.<server>.sheds.<shed>.session_duration` | string | | Per-server, per-shed session token lifetime (most specific) |

### Docker Settings

`approval.policy` is one of `deny-all`, `approve-all`, `shed-desktop`. The
`registries` allowlist is authorization and must be set here — there is no
shed-desktop Preferences UI for it.

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `docker.approval.policy` | string | `deny-all` | `deny-all`, `approve-all`, or `shed-desktop` |
| `docker.registries` | []string | `[]` | Registry hostnames to allow credential brokering for |
| `docker.allow_all` | bool | `false` | Allow credentials for any registry (bypasses allowlist) |
| `docker.config_path` | string | (auto-detect) | Override Docker config.json path. If unset, checks `$DOCKER_CONFIG/config.json` first, then `~/.docker/config.json` |
| `docker.servers.<server>.registries` / `.allow_all` | []string / bool | | Per-server allowlist override (multi-server mode) |
| `docker.servers.<server>.sheds.<shed>.registries` / `.allow_all` | []string / bool | | Per-server, per-shed allowlist override (most specific) |

### Logging Settings

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `logging.enabled` | bool | `true` | Enable audit logging |
| `logging.path` | string | `~/.local/share/shed/extensions-audit.log` | Path to audit log file. Appended to, never rotated |

### Server Settings

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `server` | string | unset | shed-server URL. When set, limits the broker to that one server (single-server mode, which turns discovery off); leave it unset to broker for every server in `~/.shed/config.yaml` |

### Machine identity

Not configured here. The broker keeps this machine's id and name in `machine.json` under its
state directory (`~/Library/Application Support/shed/broker/` on macOS,
`$XDG_DATA_HOME/shed/broker/`, else `~/.local/share/shed/broker/`, on Linux; `SHED_BROKER_STATE_DIR`
replaces the `shed` part), creates it on first start, and sends it on every
bus request so a server can tell the user's machines apart. The name comes from the hostname
once and can be edited in the file. See
[Credential Source → Machine identity](source-binding.md#machine-identity).

## Multi-Server Discovery

One broker can broker credentials for **many** shed
servers at once. Add a `discovery:`
block — its presence switches the broker into multi-server mode (and `server:`
above is then ignored). With neither `server:` nor `discovery:` the broker discovers every
server in `~/.shed/config.yaml`. Servers are discovered from shed's own CLI config
(`~/.shed/config.yaml`, the same `servers:` you manage with `shed server add`).
A server's broker URL is its pinned `api_url` (`https://…`) when present, else
`http://<host>:<http_port>`.

The broker **enrols its own `credentials` certificate only for servers reached over
`https`**: an `api_url` in `~/.shed/config.yaml`, which `shed server add` writes for an
`mtls` server. It enrols over the server's `_bootstrap` SSH channel with your system `ssh`
client (see [SSH Agent → Enrolling the broker's certificate](ssh-agent.md#enrolling-the-brokers-certificate)).
For an **open** server (plain `http`, loopback only) no credential is needed: the broker
connects to the credential bus unauthenticated and enrols nothing. (The presence of an
`ssh_port` alone is *not* the signal — every shed server has an SSH endpoint.)

```yaml
discovery:
  # "all" (default) watches every server in ~/.shed/config.yaml.
  # A list watches only those by name:
  #   servers: [devbox, buildbox]
  servers: all

  # Where to discover servers (default ~/.shed/config.yaml)
  # source: ~/.shed/config.yaml

  # How changes are picked up without a restart:
  #   fsnotify (default) — react instantly to ~/.shed/config.yaml changes
  #   poll               — re-read on an interval
  #   off                — read once at startup
  watch: fsnotify
  # poll_interval: 10s    # used when watch: poll
  # debounce: 500ms       # used when watch: fsnotify
```

When a server is added to (or removed from) `~/.shed/config.yaml`, the broker
starts (or stops) brokering for it automatically — no restart needed. Offline
servers are retried in the background and cost nothing until they come up.

### Discovery Settings

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `discovery.servers` | `all` \| []string | `all` | Which discovered servers to watch — every server, or a named subset |
| `discovery.source` | string | `~/.shed/config.yaml` | shed CLI config to discover servers from |
| `discovery.watch` | string | `fsnotify` | Live-reload mechanism: `fsnotify`, `poll`, or `off`. Falls back to polling if fsnotify can't start |
| `discovery.poll_interval` | string | `10s` | Reconcile cadence when `watch: poll` |
| `discovery.debounce` | string | `500ms` | Event coalescing window when `watch: fsnotify` |

### Per-server and per-shed overrides

In multi-server mode the `aws:` and `docker:` blocks are **hierarchical**: the
top-level fields are the defaults, and a nested `servers:` tree overrides them
per server and per shed (most specific wins). This also keeps identical shed
names on different servers isolated.

```yaml
aws:
  source_profile: default                       # process-global
  default_role: arn:aws:iam::111:role/Dev        # default for every server/shed
  session_duration: 1h
  servers:
    devbox:
      default_role: arn:aws:iam::111:role/Devbox  # per-server default
      sheds:
        web:
          role: arn:aws:iam::111:role/Web        # per-shed (wins)
        sso-app:
          mode: passthrough                      # per-shed mode override

docker:
  registries: [ghcr.io]                          # default allowlist
  allow_all: false
  servers:
    devbox:
      allow_all: true                            # per-server
      sheds:
        web:
          registries: [ghcr.io, us-central1-docker.pkg.dev]   # per-shed
```

Resolution order (later overrides earlier): top-level defaults →
`…servers.<server>` → `…servers.<server>.sheds.<shed>`. For AWS, `role`, `mode`,
and `session_duration` layer independently (a `mode: passthrough` inherited from
a parent makes a shed's `role` irrelevant). For Docker, a non-`null` `registries` list
**replaces** (does not merge) the inherited list, and `allow_all` is only
overridden when explicitly set at a more specific level.

All audit log lines and shed-desktop events carry a `server` field so activity
from different servers stays distinguishable in the combined log.

## Guest-Side Configuration

None. The opinionated base image configures everything:

- `SSH_AUTH_SOCK=/run/shed-extensions/ssh-agent.sock` via `/etc/environment.d/shed-extensions.conf`
- `AWS_CONTAINER_CREDENTIALS_FULL_URI=http://127.0.0.1:499/credentials` via `/etc/environment.d/shed-extensions.conf`
- `shed-ext-ssh-agent.service` and `shed-ext-aws-credentials.service` ship **disabled**. The
  in-VM shed agent enables and starts (`systemctl enable --now`) the units of the namespaces
  listed in the server's `extensions.enabled` (see
  [Extension Management](../reference/extensions.md#extension-management)); a namespace not
  listed stays off
- `docker-credential-shed` is on PATH at `/usr/local/bin/`, and `~/.docker/config.json` is
  configured with `"credsStore": "shed"`; Docker runs it on demand, so it has no unit

## Diagnostics

### Checking the broker

The broker is the desktop app, so the app is where you check it:

- **Preferences → Credential broker** shows whether the broker started (a start-up error is
  shown there), this machine's name, and one row per server with its
  [credential source](source-binding.md).
- `shedctl raw broker.status` prints the broker's own self-report as JSON (see
  [shedctl](../desktop/cli.md) and the [IPC catalog](../desktop/ipc.md)).

`broker.status` reports:

- where the **config** came from (`loaded`, `synthesized` when there is no file, or `error`
  with the message);
- this **machine's identity** (its name and id, from `machine.json`), or why there is none
  (see [Credential Source → Machine identity](source-binding.md#machine-identity));
- the namespaces **delegated to shed-desktop** (`gate_namespaces`);
- what the machine **signs for a shed**: the SSH backend mode in force, the usernames in
  `ssh.allow_users`, and the keys offered;
- each **watched server**: per-namespace connection state.

| State | Meaning |
|---|---|
| `connected` | Subscribed; requests are served |
| `reconnecting` | Not subscribed; retrying on a 1–30 s backoff. `last_error` says why, such as `unexpected status 403: …` with the server's message when this machine has no identity and the server requires one |
| `not-source` | Another of your machines is the [credential source](source-binding.md) on this server; retrying. The row names the source; `shed source use` moves it to this machine |
| `duplicate` | Another process with this machine's id already holds the namespace on this server; retrying |
| `rejected` | The server refused a second listener (a `409` from a server older than credential sources); not retried |
| `stopped` | The subscription ended (shutdown) |

Credential requests are answered only while the app is running. With the app quit there is no
broker: a machine without the app (a server, or a box with no desktop session) has none.

## CLI Flags

The broker has no command line of its own. The in-VM extension binaries take these flags:

### shed-ext-ssh-agent

| Flag | Default | Description |
|------|---------|-------------|
| `--sock` | `/run/shed-extensions/ssh-agent.sock` | Unix socket path |
| `--publish-url` | `http://127.0.0.1:498/v1/publish` | shed-agent publish endpoint |

### shed-ext-aws-credentials

| Flag | Default | Description |
|------|---------|-------------|
| `--port` | `499` | HTTP listen port |
| `--publish-url` | `http://127.0.0.1:498/v1/publish` | shed-agent publish endpoint |

### docker-credential-shed

| Flag / Env | Default | Description |
|------------|---------|-------------|
| `SHED_PUBLISH_URL` | `http://127.0.0.1:498/v1/publish` | shed-agent publish endpoint (env var) |

Commands: `get` (resolve credentials), `list` (list registries), `store` (rejected), `erase` (rejected), `version`.

