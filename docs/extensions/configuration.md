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
| `shed-desktop` | Decide each request in the [shed-desktop](desktop-approval.md) app. The app connects to the agent's always-on [approval socket](host-agent-ipc.md#approval-channel); its Preferences own method/scope/TTL. If the app isn't running, requests **fail closed**. |
| `biometrics` | **SSH only, macOS.** Native Touch ID, biometrics only (fails with no sensor). Off macOS it denies every request. |
| `biometrics-or-password` | **SSH only, macOS.** Native Touch ID, Apple Watch, **or** account password — works in clamshell mode and on Macs without a sensor. Off macOS it denies every request. |

`scope` and `session_ttl` apply **only** to the native biometric policies (they
cache an approval so you aren't prompted on every operation). Under
`shed-desktop` the app owns scope/TTL; under `deny-all`/`approve-all` they are unused.

The top-level **`approval_timeout`** (Go duration, default `25s`) bounds how long
the agent waits for a `shed-desktop` decision before failing closed.

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
| `server` | string | `http://localhost:8080` | shed-server URL. Used only in single-server mode (when `discovery:` is omitted) |

### Machine identity

Not configured here. The broker keeps this machine's id and name in `machine.json` under its
state directory (`~/Library/Application Support/shed/host-agent/` on macOS,
`$XDG_DATA_HOME/shed/host-agent/`, else `~/.local/share/shed/host-agent/`, on Linux), creates it on first start, and sends it on every
bus request so a server can tell the user's machines apart. The name comes from the hostname
once and can be edited in the file. See
[Credential Source → Machine identity](source-binding.md#machine-identity).

## Multi-Server Discovery

A single `shed-host-agent` process can broker credentials for **many** shed
servers at once, instead of running one process per server. Add a `discovery:`
block — its presence switches the agent into multi-server mode (and `server:`
above is then ignored). Servers are discovered from shed's own CLI config
(`~/.shed/config.yaml`, the same `servers:` you manage with `shed server add`).
A server's broker URL is its pinned `api_url` (`https://…`) when present, else
`http://<host>:<http_port>`.

The agent **enrols its own `credentials` certificate only for servers reached over
`https`**: an `api_url` in `~/.shed/config.yaml`, which `shed server add` writes for an
`mtls` server. It enrols over the server's `_bootstrap` SSH channel with your system `ssh`
client (see [SSH Agent → Enrolling the broker's certificate](ssh-agent.md#enrolling-the-brokers-certificate)).
For an **open** server (plain `http`, loopback only) no credential is needed: the agent
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
- this **machine's identity** (its name and id, from `machine.json`), or why there is none
  (see [Credential Source → Machine identity](source-binding.md#machine-identity));
- the **effective approval policy** per provider, and which are delegated to shed-desktop;
- what the machine **signs for a shed**: the SSH backend mode, the usernames in
  `ssh.allow_users`, and how many keys are offered (`N offered` with no filter,
  `N of M offered` with `ssh.offer_keys`, `not listed yet` until the first listing, since a
  forwarded agent is not asked until a shed asks);
- the **approval channel**: its socket path and whether a consumer (e.g. shed-desktop) is
  connected;
- each **watched server**: per-namespace connection state, marked `ok`, `-` or `x`.

```text
shed-host-agent status (pid 4242, 0.10.0)
config:   /Users/me/.config/shed/extensions.yaml
started:  2026-11-16T09:12:03Z
machine:  laptop (0f3c9a2e7b1d4c6a8e5f2b9d1c7a3e60)

Approval policies:
  ssh-agent           shed-desktop  (decided in shed-desktop)
  aws-credentials     deny-all
  docker-credentials  approve-all

SSH signing:
  mode   agent-forward
  users  git
  keys   2 of 5 offered (ssh.offer_keys)

Approval channel:
  socket    /Users/me/Library/Application Support/shed/host-agent.sock
  consumer  connected (shed-desktop 0.10.0)

Servers (1):
  devbox  (https://devbox.example.com:18443)
    ok  ssh-agent           connected
    ok  docker-credentials  connected
```

| State | Mark | Meaning |
|---|---|---|
| `connected` | `ok` | Subscribed; requests are served |
| `reconnecting` | `x` | Not subscribed; retrying on a 1–30 s backoff. `last_error` says why, such as `unexpected status 403: …` with the server's message when this machine has no identity and the server requires one |
| `not-source` | `-` | Another of your machines is the [credential source](source-binding.md) on this server; retrying. Two more lines name the source and print the `shed source use` command that moves it here |
| `duplicate` | `x` | Another process with this machine's id already holds the namespace on this server; retrying |
| `rejected` | `x` | The server refused a second listener (a `409` from a server older than credential sources); not retried |
| `stopped` | `-` | The subscription ended (shutdown) |

If the agent isn't running, `status` says so and exits non-zero (nothing is listening on the
socket) rather than guessing from a file. Start it with `brew services start shed-host-agent`.

## CLI Flags

### shed-host-agent

| Flag / Subcommand | Default | Description |
|------|---------|-------------|
| `--config` | `~/.config/shed/extensions.yaml` | Path to config file |
| `--log-file` | `""` (stderr) | Append the operational log to this file (the brew service sets it to `$(brew --prefix)/var/log/shed-host-agent.log`; empty logs to stderr). The file is not rotated or size-capped |
| `version` | — | Print version and exit |
| `status [--json]` | — | Query the running daemon's self-report (config, machine, policies, SSH signing, connection state) and exit (see [Diagnostics](#diagnostics)) |
| `rc-hub` | — | **Retired** (plan 022, S6, [`charliek/shed#328`](https://github.com/charliek/shed/issues/328)). Exits 2 naming the retirement rather than starting anything — kept as a tombstone so stale docs or launchd/systemd entries fail loudly instead of silently doing something else |

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

