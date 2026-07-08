# Configuration

## Host-Side Configuration

The host agent reads configuration from `~/.config/shed/extensions.yaml`.

```yaml
# shed-server URL
server: http://localhost:8080

# How long to wait for a shed-desktop approval before failing closed (default 25s)
# approval_timeout: 25s

ssh:
  # SSH backend: "agent-forward", "local-keys", or "" (auto-detect)
  # mode: ""
  approval:
    policy: biometrics-or-password   # see "Approval model" below
    scope: per-session               # biometric policies only
    session_ttl: 1h

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
    mini2:
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

### Approval model

Every extension (`ssh`, `aws`, `docker`) has a required `approval.policy`. An
omitted/empty policy means **`deny-all`** (fail closed). Values:

| Policy | Behavior |
|--------|----------|
| `deny-all` | Reject every request. The safe default / kill-switch. |
| `approve-all` | Allow every request. Any allowlist/role below still applies — this only removes the approval prompt. |
| `shed-desktop` | Decide each request in the [shed-desktop](desktop-approval.md) app. The app connects to the agent's always-on [approval socket](host-agent-ipc.md#approval-channel); its Preferences own method/scope/TTL. If the app isn't running, requests **fail closed**. |
| `biometrics` | **SSH only.** Native macOS Touch ID, biometrics only (fails with no sensor). |
| `biometrics-or-password` | **SSH only.** Native Touch ID, Apple Watch, **or** account password — works in clamshell mode and on Macs without a sensor. |

`scope` and `session_ttl` apply **only** to the native biometric policies (they
cache an approval so you aren't prompted on every operation). Under
`shed-desktop` the app owns scope/TTL; under `deny-all`/`approve-all` they are unused.

The top-level **`approval_timeout`** (Go duration, default `25s`) bounds how long
the agent waits for a `shed-desktop` decision before failing closed.

### SSH Settings

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `ssh.mode` | string | `""` (auto) | SSH backend mode. Auto-detect selects agent-forward if `SSH_AUTH_SOCK` exists, falls back to local-keys. |
| `ssh.approval.policy` | string | `deny-all` | One of `deny-all`, `approve-all`, `biometrics`, `biometrics-or-password`, `shed-desktop` (see Approval model) |
| `ssh.approval.scope` | string | `per-session` | Biometric policies only: `per-request`, `per-session`, `per-shed` |
| `ssh.approval.session_ttl` | string | `4h` | Biometric policies only: how long a cached approval remains valid |

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
| `logging.path` | string | `~/.local/share/shed/extensions-audit.log` | Path to audit log file |

### Server Settings

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `server` | string | `http://localhost:8080` | shed-server URL. Used only in single-server mode (when `discovery:` is omitted) |

## Multi-Server Discovery

A single `shed-host-agent` process can broker credentials for **many** shed
servers at once, instead of running one process per server. Add a `discovery:`
block — its presence switches the agent into multi-server mode (and `server:`
above is then ignored). Servers are discovered from shed's own CLI config
(`~/.shed/config.yaml`, the same `servers:` you manage with `shed server add`).
A server's broker URL is its pinned `api_url` (`https://…`) when present, else
`http://<host>:<http_port>`.

The agent **self-mints its `credentials` token only for secure servers** — those
reached over an `https` `api_url` (which `shed server add` writes for a
secure-mode server; tokens ⟺ TLS ⟺ secure). For an **open-mode** server (plain
`http`) no token is needed: the agent connects to the credential bus
unauthenticated and does not attempt a mint. (The presence of an `ssh_port`
alone is *not* the signal — every shed server has an SSH endpoint.)

```yaml
discovery:
  # "all" (default) watches every server in ~/.shed/config.yaml.
  # A list watches only those by name:
  #   servers: [mini2, mini3]
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

When a server is added to (or removed from) `~/.shed/config.yaml`, the agent
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
    mini2:
      default_role: arn:aws:iam::111:role/Mini2  # per-server default
      sheds:
        web:
          role: arn:aws:iam::111:role/Web        # per-shed (wins)
        sso-app:
          mode: passthrough                      # per-shed mode override

docker:
  registries: [ghcr.io]                          # default allowlist
  allow_all: false
  servers:
    mini2:
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
- `shed-ext-ssh-agent` and `shed-ext-aws-credentials` start via systemd at boot
- `docker-credential-shed` available on PATH; `~/.docker/config.json` configured with `"credsStore": "shed"`

## Diagnostics

### `shed-host-agent status`

Queries the **running** agent over its read-only [status socket](host-agent-ipc.md#status-socket)
and prints the daemon's own self-report. It does not read a config file — the daemon is the source
of truth for what it loaded — so `status` can never disagree with the running service (the confusion
that motivated this design: a bare invocation reading a different config than the brew service).

```bash
shed-host-agent status              # human-readable
shed-host-agent status --json       # machine-readable (schema 1)
```

It reports:

- which **config file** the running agent loaded (the absolute `config_path`);
- the **effective approval policy** per provider, and which are delegated to shed-desktop;
- the **approval channel**: its socket path and whether a consumer (e.g. shed-desktop) is connected;
- each **watched server**: per-namespace connection state — `connected`, `reconnecting` (with the
  failure reason), or `stopped`.

If the agent isn't running, `status` says so and exits non-zero (nothing is listening on the
socket) rather than guessing from a file. Start it with `brew services start shed-host-agent`.

(`status` is the host-side counterpart to the in-VM [`shed-ext status`](status-cli.md), which
checks connectivity from inside a shed.)

## CLI Flags

### shed-host-agent

| Flag / Subcommand | Default | Description |
|------|---------|-------------|
| `--config` | `~/.config/shed/extensions.yaml` | Path to config file |
| `--log-file` | `""` (stderr) | Write the operational log to this file, size-capped + rotated (the brew service sets it; empty logs to stderr) |
| `version` | — | Print version and exit |
| `status [--json]` | — | Query the running daemon's self-report (config, policies, connection state) and exit (see [Diagnostics](#diagnostics)) |

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

