# Credential broker: wire-visible behaviour catalog

**Authority.** The Rust implementation — `crates/shed-broker` plus shed-app's embedded
bridge (`crates/shed-app/src/broker_bridge.rs`, the desktop app's in-process broker) —
is authoritative for the credential broker's wire-visible behaviour. Its executable
contract coverage is the goldens + fixture vectors under `tests/broker-wire/` (see
that directory's README), which run the app's broker in the test-only
`crates/shed-broker-harness` process — those, not this document, are what a behaviour
change must keep green.

The retired standalone daemon's own surfaces — its desktop approval socket, its
status socket and `status` command, and its CLI and socket lifecycle — went with the
daemon (plan 037), and so did their sections here. "The agent" below is the broker.

This document predates the Rust port and was written against the Go daemon, which was
retired in plan 006. Every `file.go:line` citation below is **traceability only**: it
points at where the described behavior was proven against the Go implementation, in the
Go daemon's directory under `cmd/` at `bbd73e2^`, the last commit that carried it
(`git show --stat bbd73e2` names the directory; `git show bbd73e2^:<that directory>/<file>`
shows a file). The citations are **not** re-pointed at the Rust source line-by-line —
treat the prose as the contract and the Rust crates + the golden fixtures as its current,
executable form. All citations are `file.go:line` in that directory at that commit unless
another package is named.

Protocol/const source of truth: `internal/ext/protocol/{ssh,aws,docker}.go`
(namespaces, ops, codes, payload field names) and `sdk/envelope.go` (envelope).

---

## 0. Observable output channels (what the harness must capture)

| # | Channel | Direction | Transport / location | Format |
|---|---------|-----------|----------------------|--------|
| 1 | **Bus responses** | agent → shed-server | SDK `HostClient.Respond` HTTP POST to the server bus (subscribed via SSE) | `sdk.Envelope` (JSON) with a per-namespace payload |
| 2 | **Durable audit log** | agent → disk | file at `cfg.logging.path` (default `~/.local/share/shed/extensions-audit.log`) | one `AuditEntry` JSON object per line (JSONL), `json.Encoder` `\n`-terminated (`audit.go:80,113`) |
| 3 | **Approval requests and audit events to the app** | broker → app | in-process: the bridge's event stream to the app's Coordinator (the harness's control socket stands in for it, `{"op":"subscribe"}`) | `approval_request` / `event` frames |
| 4 | **Status** | broker → app | in-process: `LiveStatus` (§4.1; the harness serves it as `{"op":"status"}`; the app reports the same `servers`, `machine` and `ssh` in `broker.status`) | one JSON object |
| 6 | **Operational log** | broker → the app's log | the app's log file (`--log-file` in the harness) | text lines (loose; not a good diff target) |
| 7 | **Egress SSE consumer** | agent → shed-server | HTTP `GET /api/egress/stream` | observable *request*: method, path, `Accept: text/event-stream`, `Authorization: Bearer` |
| 8 | **SSH mint subprocess** | agent → `ssh` | `exec` of system `ssh` (via `sdk/bootstrap`) | observable *argv* + stdin/stdout contract |
| 9 | **Docker helper subprocess** | agent → `docker-credential-<h>` | `exec ... get`, serverURL on stdin | observable argv + stdin + parsed stdout |
| 10 | **Shutdown** | app | the app's exit (the harness: SIGTERM) | every server group drained before exit (§10.3) |

---

## 1. Volatile-field normalization mask (redact/normalize before diffing)

These vary run-to-run and MUST be masked. Grouped by channel.

**Audit JSONL (channel 2) / the app's `event` frame (channel 3):**
- `ts` — RFC3339 `time.Now().UTC()` stamped in `LogEntry` if unset (`audit.go:106`). VOLATILE. (Exception: egress entries carry the server-supplied `ts` — still wall-clock.)
- `detail` — VOLATILE **only for AWS ok** (`awsExpiryDetail` → `expires:15:04` / `expires:none`, `aws_handler.go:168`) and **SSH list** (`"N keys"`). Docker/SSH-sign details are stable (registry / key type).
- `ttl` — config-derived (`4h`), stable per config, but treat as env-derived.

**Approval requests to the app (channel 3):**
- `id` — a fresh UUID per request; `ts` — RFC3339 UTC send time; `expires_at` — `now + approval_timeout`. VOLATILE.

**Status (channel 4):**
- `pid`, `started_at`, `written_at` (snapshot time), `version`, per-namespace `since` (RFC3339). All VOLATILE.
- `config_path` — absolute path, env-dependent. VOLATILE.
- `machine.id` (a random 32-hex id per state dir) and `machine.name` (from the host's hostname). VOLATILE; the harness asserts their shape (§4.1) before masking them to `<machine_id>` / `<machine_name>`.
- `schema` — constant `1` (stable; a mismatch is a hard reject).

**Bus response payloads (channel 1):**
- Envelope `id`, `in_reply_to`, `timestamp` (`sdk/envelope.go:27-32`). VOLATILE.
- AWS `access_key_id`/`secret_access_key`/`session_token` — secret + non-deterministic. REDACT.
- AWS `expiration`, `cached_until` — STS/clock-derived (`aws_handler.go:112,138`). VOLATILE.
- SSH `blob` (signature) — non-deterministic per sign (nonce). REDACT/normalize to "present".
- Docker `secret`/`username` — from helper. REDACT.

**Mint / STS internals (indirect):**
- STS `RoleSessionName` = `shed-<server>-<shed>-<unix>` (`aws_backend.go:142`) — embeds `time.Now().Unix()`.
- Token strings + `expires_at` from the mint bundle. VOLATILE.

**Non-volatile (safe to diff directly):** all `namespace`/`ns`, `operation`/`op`,
`result`, `code`, `reason`, `approval`, `decided_by`, `scope`, `server`, `shed`,
`type`, `accepted`, `mode`, `key_count`, `allow_all`, `registry_count`,
`connected`, `role` (assume-role ARN or `passthrough:<profile>`), `status:"ok"`,
and every error `{error,code}` string.

---

## 2. External seams the harness must fake

| Seam | How the agent reaches it (prod) | Fake mechanism (used by tests) |
|------|--------------------------------|--------------------------------|
| **shed-server bus** | `sdk.HostClient` SSE `Subscribe` + POST `Respond`; `sdk.WithServerURL(t.URL)` | `httptest.NewServer` loopback that emits one request envelope over SSE and captures the POST reply |
| **SSH mint (`ssh`)** | `sdk/bootstrap.Run` → `exec.CommandContext(ssh, sshArgs(p)...)` (`bootstrap.go:193`) | inject `CredentialMinter.bootstrapRun func(ctx, Params)(sdk.Bundle,error)` (`credmint.go:33`); OR the `minter` interface / `fakeMinter` (`credmint_test.go:193`) at the `credentialSource` layer |
| **ssh-agent (agent-forward)** | `net.Dial("unix", $SSH_AUTH_SOCK)` per op (`ssh_backend_agent.go:21,59`) | set `SSH_AUTH_SOCK` to a fake agent socket; OR construct `localKeysBackend` directly; OR inject a stub `SSHBackend` into the handler (`ssh_handler_test.go:26`) |
| **local SSH keys** | reads `~/.ssh/{id_ed25519,id_rsa,id_ecdsa}` (`ssh_backend_localkeys.go:16,44`) | construct `&localKeysBackend{keys: [...]}` in-memory (`ssh_backend_localkeys_test.go:50`) |
| **AWS STS (assume-role)** | `config.LoadDefaultConfig(WithSharedConfigProfile(source_profile))` once + `sts.AssumeRole` (`aws_backend.go:97,144`) | **no in-process seam** — tests either pre-seed `stsBackend.cache` (`aws_backend_test.go:196`) or inject a `mockAWSBackend` at the handler layer |
| **AWS passthrough** | `config.LoadSharedConfigProfile` with explicit files resolved by `sharedCredentialsPath`/`sharedConfigPath` (`aws_backend.go:191,259,267`) | `t.Setenv("AWS_SHARED_CREDENTIALS_FILE", tmp)` + `t.Setenv("AWS_CONFIG_FILE", tmp)` → temp ini (`aws_backend_test.go:277`) |
| **Docker helper** | `exec docker-credential-<helper> get`, serverURL on stdin (`docker_backend.go:238,256`) | inject `helperExecutor` via `dockerHelperBackend.executor` field (`docker_backend.go:42,111`); tests set `executor:&mockExecutor{}` (`docker_backend_test.go:203`) |
| **Docker config.json** | `$DOCKER_CONFIG/config.json` or `~/.docker/config.json` or `cfg.config_path` (`docker_backend.go:356`) | `t.Setenv("DOCKER_CONFIG", tmp)` or write temp `config.json` + `DockerConfig.ConfigPath` |
| **egress SSE** | HTTP `GET server/api/egress/stream` (`egress_handler.go:133`) | `httptest` loopback + `fakeTokenSource` implementing `tokenSource` (`egress_handler_test.go:90`) |
| **known_hosts pin** | reads `~/.shed/known_hosts` (`credmint.go:88`) | write a temp known_hosts with `knownhosts.Line` (`credmint_test.go:93`) |
| **discovery source** | reads `~/.shed/config.yaml` (shed CLI config) (`discovery.go:62`) | write a temp YAML, point `discovery.source` / `SHED_*` at it |

---

## 3. Audit records (`audit.go`, `audit_test.go`)

### 3.1 Durable record shape (`AuditEntry`, `audit.go:13-43`)

JSONL, one object per line. Field order in the struct is the emission order
(Go `json.Encoder` preserves struct order):

| JSON key | Go field | omitempty | Notes |
|----------|----------|-----------|-------|
| `ts` | Timestamp | no | RFC3339 UTC, stamped in `LogEntry` if unset. **VOLATILE** |
| `server` | Server | **yes** | discovery name; empty in single-server mode (omitted) |
| `shed` | Shed | no | shed instance name (may be `""`) |
| `ns` | Namespace | no | `ssh-agent`/`aws-credentials`/`docker-credentials`/`egress` |
| `op` | Operation | no | e.g. `sign`, `get_credentials`, `get`, `list`; egress uses the **protocol** (`https`/`tcp`) |
| `result` | Result | no | `ok`/`error`/`denied`/`anonymous`/`undelivered`/`refused` (a sign request the signing check declined, plan 035 §6.1); egress uses the **verdict** (`allow`/`deny`) |
| `detail` | Detail | **yes** | free text (registry, key type, `N keys`, `expires:HH:MM`, `host:port (ip)`) |
| `code` | Code | **yes** | protocol error code or audit-only `APPROVAL_DENIED` / `SIGN_REFUSED`; empty on ok |
| `reason` | Reason | **yes** | host-side human explanation; never raw helper stderr. On a `refused` sign: the refusal class (§6.1), or `audit_rate_limited` on a summary line |
| `approval` | Approval | no | the policy method: `deny-all`/`approve-all`/`shed-desktop`/biometrics/`none` |
| `decided_by` | DecidedBy | **yes** | gated ops: `user`/`touchid`/`policy`/`timeout` |
| `scope` | Scope | **yes** | approval scope applied |
| `ttl` | TTL | **yes** | approval TTL applied |

Two writers: `Log(server,shed,ns,op,result,detail,approval)` (basic, no
approval-detail; `audit.go:87`) and `LogEntry(entry)` (full, used by gated ops).

### 3.2 File location / perms / rotation

- Path = `cfg.logging.path`; dir created `0700`, file opened `O_APPEND|O_CREATE|O_WRONLY, 0600` (`audit.go:66,71`). Verified `audit_test.go:110-120` (dir 0700, file 0600).
- **No rotation** for the audit file (append-only forever). The *operational* log (`-log-file`) rotates via lumberjack (10 MB, 5 backups, 30 days, compress; `logging.go:20`) — separate file.
- Disabled logging (`enabled:false`) or open failure → **no-op logger**, no file, no panic (`audit.go:60,72`; `audit_test.go:54`).

### 3.3 Audit → the app's activity stream

Every entry the durable sink writes is also fanned out, after the file line
(`JsonlAuditSink`, the `AuditFanout` seam; even when file logging is disabled). In the
app the bridge maps each entry to an `event` frame on its event stream
(`audit_entry_to_event_frame`: `server`, `shed`, `ns`, `op`, `result`, `detail`,
`code`, `reason`, `approval`, `ts` — the entry's own timestamp, not re-stamped — with
empty fields left out; `decided_by`, `scope` and `ttl` stay in the durable line). The
retired daemon's desktop-socket `event` frame, and its replay ring, went with it.

---

## 4. Status (`status.go`, `status_server.go`, `status_test.go`)

### 4.1 `LiveStatus` JSON (`status_server.go:25-60`) — channel 4

`shed_broker::status::build_live_status` builds it; the harness serves it for
`{"op":"status"}` (`version` = `shed-broker-harness`).

```jsonc
{
  "schema": 1,                       // const statusSchemaVersion; mismatch → CLI exit 1
  "version": "<build>",              // VOLATILE
  "pid": 4242,                       // VOLATILE
  "started_at": "<RFC3339>",         // VOLATILE (daemon start)
  "written_at": "<RFC3339>",         // VOLATILE (snapshot time)
  "config_path": "/abs/....yaml",    // VOLATILE (abs path)
  "policies": {                      // namespace -> EffectivePolicy() (deny-all default)
    "ssh-agent": "deny-all",
    "aws-credentials": "deny-all",
    "docker-credentials": "approve-all"
  },
  "gate_namespaces": ["ssh-agent"],  // omitempty; only ns whose policy == shed-desktop
  "servers": [                       // sorted by name (Supervisor.Health)
    { "name": "mini2", "url": "https://...",
      "namespaces": [
        { "namespace": "ssh-agent", "state": "connected",
          "last_error": "",          // omitempty
          "since": "<RFC3339>",      // VOLATILE
          "active_source": {         // omitempty; only in state "not-source" (plan 035)
            "machine_id": "...", "machine_name": "desk" } }
      ],
      "active_source": { ... } }     // omitempty; set iff any namespace is "not-source"
  ],
  "machine": {                       // omitted when absent (older daemon, or no identity)
    "id": "0123456789abcdef0123456789abcdef",  // VOLATILE: 32 lowercase hex, no hyphens
    "name": "laptop"                 // VOLATILE: 1-63 bytes UTF-8, no control characters
  },
  "machine_error": "...",            // omitempty; why there is no `machine`
  "ssh": {                           // omitted by an older daemon (plan 035 §3.5)
    "mode": "local-keys",            // the resolved SSH backend mode
    "allow_users": ["git"],          // ssh.allow_users: the usernames a login is signed for
    "key_filter": false,             // whether ssh.offer_keys is set
    "keys_offered": 3,               // omitempty: absent until a listing has happened
    "keys_available": 3              // omitempty: the backend's keys at that listing
  }
}
```

- `policies` keys are always the 3 credential namespaces (`status_server.go:81`).
- `state` ∈ `connected|reconnecting|stopped` (also `rejected` in the SDK), from `sdk/hostclient.go:28-34` `ConnConnected/ConnReconnecting/ConnStopped/ConnRejected` → the health snapshot maps `client.Status()` (`supervisor.go:243`). Plan 035 adds two non-terminal states, Rust only: `not-source` and `duplicate` (§9b; the daemon's third, `yielding`, went with it in plan 037).
- `active_source` (plan 035, additive under schema 1, omitted otherwise so every pre-pin golden is unchanged): on a namespace, the machine a `403 SOURCE_NOT_BOUND` named as the active credential source, exactly as the server sent it (control and Unicode format characters dropped, at most 64 bytes each); on a server, the first such namespace's, so the server row reads as not the source when any of its namespaces is.
- `ssh` (plan 035, Rust only, additive under schema 1): what this machine signs for a shed — the policy built once at start from `ssh.allow_users` and `ssh.offer_keys` (§5.2), the backend mode, and the key filter's counts from the LAST `list` (the app's broker lists no keys at start-up, so the counts are absent until a shed first lists keys; the retired daemon listed local keys at start-up). The desktop app's embedded broker reports the same object as `broker.status.ssh` (null outside embedded mode or before the broker started).
- `buildLiveStatus` (`status_server.go:65`) composes it: policies from `cfg.*.Approval.EffectivePolicy()`, gate list from `desktopGateNamespaces(cfg)`, approval-channel from `desktop.ConsumerInfo()`, servers from `sup.Health()`.
- `machine` / `machine_error` (plan 035, Rust only — added after the Go daemon's retirement, additive under schema 1): this machine's identity from `<state dir>/broker/machine.json` (`shed_broker::machine`). Loaded once at start, before any socket binds. The id and the schema are read on their own first, so an id that can be read is never thrown away. Missing → created; a valid id with any other field missing, `null`, of the wrong type or malformed → the id kept and the rest repaired (a WARN line); a newer schema → never rewritten, moved or replaced: its id is used if it can be read, otherwise the daemon runs without one and `machine_error` names the schema; no readable id at all → copied to `machine.json.corrupt` and replaced in one atomic rename (a WARN line: servers will see a new machine); unreadable, impossible to create, or a write whose fsync (file, directory, or a directory it created) fails → the daemon runs without one and `machine_error` says why, and `machine.json` is never left missing by a failed replacement. Nothing here stops the daemon from starting, matching the credential store. Every bus request carries it (§9b).

The daemon's status socket and its `status` command (exit codes, the text render) went
with the daemon (plan 037); the app shows the same snapshot in Preferences and
`broker.status`.

---

## 5. Config (`config.go` + tests)

Loaded by `LoadConfig(path)` (`config.go:452`): tilde-expand path → `os.ReadFile`
→ `yaml.Unmarshal` over `DefaultConfig()` → expand `logging.path` → warn
deprecated desktop keys → `Validate()` → apply discovery defaults. **Any error →
`main.go:85` os.Exit(1)** with `failed to load config`.

### 5.1 Top-level keys (`config.go:38-56`)

| YAML key | Default | Meaning |
|----------|---------|---------|
| `server` | `http://localhost:8080` | legacy single-server URL (ignored if `discovery:` set) |
| `discovery` | absent → single-server | multi-server block (§5.6) |
| `approval_timeout` | `25s` | delegated-approval budget; must parse as positive Go duration |
| `ssh` | — | §5.2 |
| `aws` | — | §5.3 |
| `docker` | — | §5.4 |
| `logging` | `{enabled:true, path:~/.local/share/shed/extensions-audit.log}` | §3.2 |
| `desktop` | deprecated/ignored | presence of `enabled`/`socket_path`/`timeout_ms` → WARN (`config.go:530`) |

### 5.2 SSH (`config.go:385`)
```yaml
ssh:
  mode: ""            # "" (auto) | agent-forward | local-keys
  approval:
    policy: ""        # ""(=deny-all) | deny-all | approve-all | biometrics | biometrics-or-password | shed-desktop
    scope: per-session   # biometric only; default per-session
    session_ttl: 4h      # biometric only; default 4h
```
Auto mode (`ssh_backend.go:35`): `SSH_AUTH_SOCK` set + dialable → agent-forward, else local-keys. Unknown mode → **fatal** `unknown ssh mode: %q` (backend init, `main.go:118` exit 1).

Plan 035 (Rust only) adds two machine-wide keys under `ssh:`, read once at start:

```yaml
ssh:
  allow_users: [git]   # the usernames an SSH login is signed for; default [git]
  offer_keys:          # optional: the only keys listed to sheds or used to sign
    - "SHA256:..."     # a fingerprint as `ssh-add -l` prints it, or a public key line
```

A name beginning with `_` in `allow_users`, a list holding a name other than `git` with no `offer_keys`, or an `offer_keys` entry that does not parse or names a key the broker cannot sign with fails config load (exit 1). With `offer_keys` set, `list` returns only those keys and a sign with any other key is declined (§6.1).

### 5.3 AWS (`config.go:271`)
```yaml
aws:
  source_profile: default      # default "default"; process-global (not per-server)
  default_role: ""             # role ARN
  mode: ""                     # ""(=assume-role) | assume-role | passthrough
  session_duration: 1h         # default 1h
  cache_refresh_before: 5m     # default 5m; process-global
  approval: { policy: "" }     # deny-all | approve-all | shed-desktop  (NO biometrics)
  servers:                     # per-server overrides, each with .sheds.<shed>
    mini2: { default_role: "...", mode: "...", session_duration: "...",
             sheds: { web: { role: "...", mode: "...", session_duration: "..." } } }
```
- `Enabled()` (`config.go:356`): true iff any path is passthrough OR sets a non-empty role. An assume-role with no role anywhere = **AWS off** → `NewSTSBackend` returns an error and the handler never starts (`aws_backend.go:59`; observably: no `aws-credentials` subscription).
- `Resolve(server,shed)` (`config.go:317`): layer defaults → server → shed; empty `mode` normalizes to `assume-role` at the end (child role under passthrough parent stays passthrough).

### 5.4 Docker (`config.go:198`)
```yaml
docker:
  registries: [index.docker.io, ghcr.io]   # allowlist (default tier)
  allow_all: false                          # bypass allowlist
  config_path: ""                           # override ~/.docker/config.json
  approval: { policy: "" }                  # deny-all | approve-all | shed-desktop
  servers:
    mini2: { registries: [...], allow_all: true, sheds: { web: { registries: [...], allow_all: false } } }
```
`Resolve` (`config.go:233`): most-specific wins; a non-nil `registries` list **replaces** (not merges). "helper" mode isn't a config toggle — helper vs inline-auth is decided at read time from the host `config.json` (§6.4 backend).

### 5.5 Approval policies + validation (`config.go:376,487`)
- Constants: `deny-all`, `approve-all`, `biometrics`, `biometrics-or-password`, `shed-desktop` (`config.go:377-382`).
- SSH accepts all five; **AWS/Docker reject the two biometric policies** → error `<p>.approval.policy %q is not one of deny-all, approve-all, shed-desktop` (`config.go:489,553`; `config_test.go` "rejects biometric for AWS").
- Empty policy → `deny-all` (fail-closed, `EffectivePolicy`).
- Observable validation errors (all → exit 1, message prefixed `invalid config:`):
  - `aws.sheds was removed; move entries under aws.servers.<server>.sheds.<shed>` (`config.go:562`)
  - `aws.mode %q is not one of assume-role, passthrough` (and `aws.servers.<s>.mode`, `aws.servers.<s>.sheds.<sh>.mode`, `config.go:586`)
  - `approval_timeout %q is not a valid duration` / `must be positive` (`config.go:518,521`)
  - unknown policy (above)

### 5.6 Discovery (`config.go:60`, `discovery.go`)
```yaml
discovery:
  servers: all          # scalar "all"(=every) | single name | YAML list; []=watch none, omitted=all
  source: ~/.shed/config.yaml   # default DefaultDiscoverySource
  watch: fsnotify       # fsnotify(default) | poll | off
  poll_interval: 10s    # poll mode; default 10s
  debounce: 500ms       # fsnotify mode; default 500ms
```
- `ServerSelector.UnmarshalYAML` (`config.go:85`): scalar `""`/`all`→All; other scalar→single name; sequence→list (empty list kept non-nil = "watch none"). Bad kind → `discovery.servers must be "all" or a list of server names`.
- The **discovery source** (`~/.shed/config.yaml`, a shed CLI config) is parsed by `LoadDiscoveredServers` (`discovery.go`) into `ServerTarget`s: prefers `api_url` (https) over `host:http_port` (default port 8080); carries `credentials_token`→Token, `tls_cert_fingerprint`→TLSFingerprint, `ssh_port`→SSHPort, `auth_mode`→AuthMode; **skips empty-host entries**; **sorts by name**. Missing file → empty (not an error); malformed YAML → error. Pinned by the golden fixture `tests/broker-wire/fixtures/load_discovered_servers.json`.
- `IsSecure()` = URL has `https://` prefix (case-insensitive) — the sole open-vs-secure signal, and still the sole driver of self-minting.
- `AuthMode` is carried **VERBATIM**: not defaulted, not normalized, not validated. `""` (never recorded, a legacy entry) and `"token"` (recorded) stay distinguishable, because only the former means "this client has never been told". `IsMTLS()` is a case-insensitive equality against `"mtls"`.
- It is **knowledge, not policy**: `shouldMint` deliberately ignores it. Both secure modes are reached over https and mint over the same channel, and the mint itself is CSR-first and mode-agnostic, so the SERVER's answer decides the credential shape. Keying the mint decision on a cached string would let a stale entry disable brokering entirely — precisely the failure a mode flip must not cause. What `auth_mode` buys is (a) whether a persisted certificate is worth loading before the first mint and (b) answering the desktop's legacy `token.get` with an explicit "upgrade the app" instead of a doomed round-trip.

### 5.7 Minimal launch configs per mode (a harness can write these verbatim)

```yaml
# OPEN single server, ssh auto, no aws/docker, egress implicit, discovery off
server: http://localhost:8080
ssh: { approval: { policy: approve-all } }
logging: { enabled: true, path: /tmp/h/audit.log }
```
```yaml
# SECURE single server (self-mints token) — set server to an https URL and rely on
# ~/.shed/known_hosts + ssh; the http `server:` field only holds one URL, so secure
# single-server is normally exercised via discovery (below) where api_url is https.
```
```yaml
# SSH local-keys, biometric-or-password approval (SSH only)
server: http://localhost:8080
ssh: { mode: local-keys, approval: { policy: biometrics-or-password, session_ttl: 1h } }
```
```yaml
# AWS assume-role
aws: { source_profile: dev, default_role: "arn:aws:iam::123:role/x", approval: { policy: approve-all } }
```
```yaml
# AWS passthrough
aws: { source_profile: sso, mode: passthrough, approval: { policy: approve-all } }
```
```yaml
# Docker allowlist / allow_all / (helper is host-side, not config)
docker: { registries: [ghcr.io], approval: { policy: approve-all } }      # allowlist
docker: { allow_all: true,        approval: { policy: approve-all } }      # allow_all
```
```yaml
# Egress is not a config toggle — the subscriber always runs; the SERVER decides
# (501/404 → treated as disabled). No key to set on the agent side.
```
```yaml
# Discovery variants
discovery: { watch: off }                        # reconcile once
discovery: { watch: poll, poll_interval: 20ms }  # ticker
discovery: { watch: fsnotify, debounce: 500ms }  # dir watch
discovery: { servers: [mini2], source: /tmp/shed.yaml }   # subset + custom source
```
```yaml
# Approval policy per mode (any namespace)
ssh:    { approval: { policy: deny-all } }      # every request denied
aws:    { approval: { policy: approve-all } }   # allowed (role/allowlist still apply)
docker: { approval: { policy: shed-desktop } }  # delegated to the app; fail-closed if none connected
```

---

## 6. Credential handlers (dispatch → response/error)

Common contract for all three (`ssh_handler.go`, `aws_handler.go`,
`docker_handler.go`): `Run` subscribes to the namespace; each inbound
`sdk.Envelope` is peeked for `{"operation": "..."}`. **Errors are NOT bus-level
failures** — they ride back as a normal **response** envelope
(`sdk.NewResponse(req.ID, ns, payload)`, `Shed` echoed) whose payload is the
namespace's `*ErrorResponse{error, code}`. A payload that won't parse as
`{operation}` → `error:"invalid payload", code:INTERNAL_ERROR`.

### 6.1 SSH (`ssh-agent`, `ssh_handler.go:68`)

| op | success payload | error paths (`{error, code}`) | audit |
|----|-----------------|-------------------------------|-------|
| `list` | `SSHListResponse{keys:[{format,blob(b64 marshaled pubkey),comment}]}` | backend err → `{"key listing failed", INTERNAL_ERROR}` | ok `"N keys"` / error; approval `none` |
| `sign` | `SSHSignResponse{format, blob(b64 sig), rest:""}` | undecodable request → `{"invalid sign request", INTERNAL_ERROR}`; bad pubkey b64 → `{"invalid public key encoding", INTERNAL_ERROR}`; bad data b64 → `{"invalid challenge data encoding", INTERNAL_ERROR}`; unparsable pubkey → `{"invalid public key", KEY_NOT_FOUND}`; any other refusal by the signing check → `{"sign operation failed", SIGN_FAILED}`; gate deny → `{"approval denied", SIGN_FAILED}`; backend sign err → `{"sign operation failed", SIGN_FAILED}` | refused / denied / ok / error; ok and error detail = the key type; approval = method + decided_by/scope/ttl (refused: `none`) |
| `ping` | `SSHPingResponse{status:"ok"}` | — | none |
| `status` | `SSHStatusResponse{connected:true, mode, key_count}` (mode = backend.Mode()) | — | none |
| unknown | — | `{"unknown operation: <op>", INTERNAL_ERROR}` | none |

**`sign` order (plan 035 §3.5, Rust only; the Go daemon ran the gate first):** decode the request → decode the key, then the data → **the signing check** (`shed_broker::sign_check::SignPolicy::check`, with the request's flags) → approval gate → sign through the check's `Approved`, which hands the backend exactly the decoded key and data buffers and the flags that were checked (the only path to the backend's `sign`). The check accepts only an exact SSH login request (RFC 4252 §7, or `publickey-hostbound-v00@openssh.com`) for a username on `ssh.allow_users`, never one beginning with `_`, naming the requested key, or an exact SSHSIG blob with namespace `git`; an RSA request's flags must name the hash its payload does. A request that fails to decode or that the check declines never reaches the gate — no approval prompt, no approval-cache entry read or written, no backend call — and is answered with the reply in the table (the shed cannot tell which rule declined it; a key that is not offered gets what an unknown key gets) and audited:

| field | refused sign audit |
|---|---|
| `result` | `refused` |
| `code` | `SIGN_REFUSED` (audit only, never sent to the shed) |
| `reason` | the class: `malformed_request` (the request, the key's base64 or the data's base64 does not decode), `malformed_key`, `key_not_offered`, `username_not_allowed`, `reserved_username`, `wrong_namespace`, `unparseable_data` (neither shape, or RSA flags that disagree with the payload) |
| `detail` | the check's bounded detail (at most 320 printable ASCII bytes; a shed-supplied value escaped and cut to 64 bytes): e.g. `login user=deploy`, `sshsig namespace=file`, `key SHA256:…`, `login: trailing bytes`; never the request |
| `approval` | `none`; no `decided_by`/`scope`/`ttl` |

Refusal lines are limited per shed, in fixed 60 s windows: the first 20 refusals get a line each, and after that the first refusal of each class not yet logged in the window still gets its own line (so every kind of refusal, with its username or namespace, is on record once per window). The rest are counted per class and written as ONE summary line (`result: refused`, `code: SIGN_REFUSED`, `reason: audit_rate_limited`, `detail: N more refused sign requests were not logged one by one (…): <class>=<count> …`, the classes counted in the §6.1 order) when the window ends, so every refused request is either logged or counted under its own class. The worst case per shed per window is 20 + 7 + 1 lines. At most 1024 windows are kept; a new shed past that writes out and drops ended windows, then the oldest open one. On shutdown every pending summary is written before the server group stops; a hard kill loses the counts of windows still open. Nothing else bounds them (the server does not limit a shed's bus requests and the audit file is never rotated). The ok, denied and error lines are unchanged and unlimited. A malformed key under a deny-all gate therefore answers the key error, not `approval denied`. The approval prompt's reason is still the fixed `SSH sign request`.

Backend seam (`ssh_backend.go:14`): `List() []*agent.Key`, `Sign(pubkey,data,flags)`, `Mode()`.
- **agent-forward** (`ssh_backend_agent.go`): fresh `net.Dial("unix", $SSH_AUTH_SOCK)` per op; `flags!=0` → `SignWithFlags`.
- **local-keys** (`ssh_backend_localkeys.go`): loads `~/.ssh/{id_ed25519,id_rsa,id_ecdsa}`, skips encrypted, comment = filename; `Sign` picks `rsa-sha2-256/512` from flags. Unknown key → `key not found`.

### 6.2 AWS (`aws-credentials`, `aws_handler.go:64`)

| op | success payload | error paths | audit |
|----|-----------------|-------------|-------|
| `get_credentials` | `AWSCredentialsResponse{access_key_id, secret_access_key, session_token, expiration(omitempty, "2006-01-02T15:04:05Z" only if non-zero)}` | gate deny → `{"approval denied", ASSUME_ROLE_FAILED}`; backend err → `{"credential request failed", ASSUME_ROLE_FAILED}` | denied / ok(`awsExpiryDetail`) / error(err.Error()) |
| `ping` | `AWSPingResponse{status:"ok"}` | — | none |
| `status` | `AWSStatusResponse{connected:true, role, cached_until(omitempty)}` | — | none |
| unknown | — | `{"unknown operation:...", INTERNAL_ERROR}` | none |

Note: `ASSUME_ROLE_FAILED` covers **all** failure causes (approval + backend). The
protocol constant `ROLE_NOT_FOUND` (`aws.go:13`) is **defined but never emitted**
by this handler. `role` in status = ARN (assume-role) or `passthrough:<profile>`.

Backend seam (`aws_backend.go:17`): `GetCredentials(ctx,server,shed)`, `Status(server,shed)`.
- **assume-role**: cache keyed `server/shed`, hit if `expiry-now > refreshBefore`; else `AssumeRole(RoleArn, RoleSessionName="shed-<server>-<shed>-<unix>", DurationSeconds)`. No role → error `no role configured for shed %q on server %q` (`aws_backend.go:116`).
- **passthrough**: re-reads shared creds file every call; requires access key + secret + **session token** (errors: `...no static credentials...`, `...no aws_session_token...`); expiry scanned from ini keys `aws_session_expiration`/`x_security_token_expires` (`aws_backend.go:307`).

### 6.3 Docker (`docker-credentials`, `docker_handler.go:84`)

| op | success payload | error paths | audit |
|----|-----------------|-------------|-------|
| `get` | `DockerGetResponse{server_url, username, secret}` | gate deny → `{"approval denied", REGISTRY_NOT_ALLOWED}` (audit code `APPROVAL_DENIED`); backend err → `{"credential request failed", <dockerError.code or INTERNAL_ERROR>}` | denied / ok / `anonymous`(when code `CREDENTIALS_NOT_FOUND`) / error; detail = `server_url`, reason = err.Error() |
| `list` | `DockerListResponse{registries: map[reg]->username}` | backend err → `{"list failed", INTERNAL_ERROR}` | ok `count:N`, approval `none` |
| `ping` | `DockerPingResponse{status:"ok"}` | — | none |
| `status` | `DockerStatusResponse{connected:true, allow_all, registry_count}` | — | none |
| unknown | — | `{"unknown operation:...", INTERNAL_ERROR}` | none |

Key subtlety (`docker_handler.go:135`): the **guest always gets the raw code**
(so `CREDENTIALS_NOT_FOUND` triggers its anonymous-pull fallback), but the
**audit result** for `CREDENTIALS_NOT_FOUND` is `anonymous` (not `error`);
`REGISTRY_NOT_ALLOWED`/`HELPER_FAILED`/etc stay `error`. Approval-deny returns
`REGISTRY_NOT_ALLOWED` to the guest but audits `code:APPROVAL_DENIED`.

Backend seam (`docker_backend.go:22`): `GetCredentials(ctx,server,shed,serverURL)`,
`ListCredentials`, `Status`. Resolution order per request:
1. allowlist check (`Resolve(server,shed)`; `!allow_all && !inSet` → `dockerError{REGISTRY_NOT_ALLOWED}`);
2. read host `config.json`;
3. `credHelpers[reg]` → `exec docker-credential-<h> get` (serverURL stdin) → parse `{ServerURL,Username,Secret}`;
4. `credsStore` fallback (helper failure → try auths);
5. inline `auths[reg].auth` = base64(user:pass);
6. none → `dockerError{CREDENTIALS_NOT_FOUND}`.
Helper not found on PATH → `dockerError{HELPER_FAILED, "<bin> not found: ..."}`;
helper exits non-zero → `HELPER_FAILED`; bad helper JSON → `HELPER_FAILED`.
`normalizeRegistry` strips scheme + trailing `/`,`/v1`,`/v2`. Helper PATH is
augmented with `/usr/local/bin`,`/opt/homebrew/bin` (`docker_backend.go:83`).

---

## 7. Minter (`credmint.go`, `controltoken.go` + `sdk/bootstrap`)

### 7.1 SSH-bootstrap mint flow

`CredentialMinter.Mint(t, scope)` (`credmint.go:57`):
1. **Pre-check pin** `knownHostsPinned(~/.shed/known_hosts, host, port)` (`credmint.go:88`): reads the file, computes `knownhosts.Normalize("[host]:port")` (bare host for port 22), scans entries, **skips `@revoked`/`@cert-authority` markers**, returns nil iff a plain entry matches. Missing/garbled/absent-entry → `no host key pinned for %s in %s (run 'shed server add' first)`. This is a fast-fail convenience; ssh re-verifies authoritatively.
2. `bootstrapRun(Params{Host, Port, KnownHostsPath, Scope, ClientKind})` → `sdk.Bundle`, with the broker's client kind (`broker` since plan 037). Returns `(bundle.Token, bundle.ExpiresAt, err)`.

**Exact `ssh` argv** (`sdk/bootstrap/bootstrap.go:109` `sshArgs`, then
`exec.CommandContext(ssh, args...)` at :193):
```
ssh -T -p <port>
  -o BatchMode=yes -o StrictHostKeyChecking=yes
  -o UserKnownHostsFile=<KnownHostsPath> -o GlobalKnownHostsFile=/dev/null
  -o VerifyHostKeyDNS=no -o KnownHostsCommand=none -o UpdateHostKeys=no -o CheckHostIP=no
  -o PreferredAuthentications=publickey -o PubkeyAuthentication=yes
  -o PasswordAuthentication=no -o KbdInteractiveAuthentication=no
  -o ChallengeResponseAuthentication=no -o NumberOfPasswordPrompts=0
  -o ForwardAgent=no -o ClearAllForwardings=yes -o PermitLocalCommand=no
  -l _bootstrap <host> <scope> [<clientKind>]
```
ssh binary resolved via `exec.LookPath("ssh")` with a macOS fallback. `~/.ssh/config` is intentionally NOT disabled (allows IdentityAgent). The remote command is `<scope> [<clientKind>]` — server-side `_bootstrap` user.

**stdout parse** (`decodeBundle`, `bootstrap.go:252`): exactly one JSON object
(`sdk.Bundle`, `bundle.go:16`), then EOF (trailing data → reject); require
non-empty `token`; require a usable port (`https_port` or `http_port`); an
`https_port` needs a `tls_cert_fingerprint`; `scope` (if echoed) must match.
`Bundle` JSON: `http_port`, `https_port`, `tls_cert_fingerprint`, `token`,
`scope`, `token_id`, `expires_at`. **stdout is never logged** (carries the token).

**Fail-closed classification** (`classify`, `bootstrap.go:231`): exit 255 + host-key
CHANGED banner → **terminal `ErrHostKeyMismatch`**; `Host key verification failed`
→ `ErrHostKeyVerificationFailed` (retryable); `Permission denied (publickey`/`No
more authentication methods` → `ErrNoSSHIdentities` (retryable). ctx cancel/timeout
→ retryable abort.

### 7.2 CONTROL vs CREDENTIALS

- `scopeCredentials` — the agent's own bus brokering credential (secure servers). Attached via **both** `sdk.WithTokenProvider` and `sdk.WithClientCertificates` in `busClientOptions` when `shouldMint` (`supervisor.go`): one object serving both interfaces, because a bearer token and a client certificate are two shapes of the same credential and only the server knows which is current.
- `scopeControl` — used for (a) the egress SSE stream (control-scoped route) and (b) the app's own control credential, minted on its behalf (`controltoken.go`).
- Difference is only the `Scope` param in the argv + a distinct `credentialSource`. **One certificate carries one scope**, so in mtls mode these are two genuinely different certificates, not one credential used twice.

### 7.2b The two mint entry points

| entry point | keypair | used by | result |
|---|---|---|---|
| `CredentialMinter.Mint` → `sdkbootstrap.RunCredential` | generated HERE, per mint | the agent's own bus + egress credentials | `sdk.Credential` — token, or certificate **+ the matching key** |
| `CredentialMinter.MintRelayed` → `sdkbootstrap.RunWithCSR` | generated by the CALLER (the desktop app) | the app's control credential (in the app, through the embedded bridge; the retired daemon served it as `credential.get`, and with an empty CSR `token.get`) | `sdk.Bundle` — token, or certificate and **no key** |

The relay exists because the desktop's key must not leave the desktop. `RunCredential` on
that path would return a certificate for a key the agent holds — useless to the app that
has to present it.

**`ssh` argv, mtls addition:** the remote command becomes `<scope> [<clientKind>] [csr=<b64>]`.
The `csr=` element goes through the same argv validation as every other element (single
token, no whitespace, no NUL). A token-mode server accepts and deliberately IGNORES it, so
the CSR is sent unconditionally by `RunCredential` — which is what makes a server-side mode
flip a non-event for the client.

### 7.3 Caching / refresh / expiry (`credentialSource`, `credmint.go`)

Backed by `internal/clienttoken.Source` — the same two-state credential machine the CLI
uses, not a second implementation.

- `Token()` mints when there is nothing usable or the credential is within
  `clienttoken.RefreshWindow` (**2h**) of expiry; otherwise serves the cache. In **mtls
  state it returns `("", nil)`** — the credential is real, it simply is not a bearer
  token, and the certificate path carries it instead. A non-nil error means there is no
  credential of any shape.
- `ClientCertificate()` returns the certificate to present, or nil in token state. It
  **never mints**: it runs inside the TLS handshake, where an SSH round-trip would stall a
  dial behind an operation the handshake's deadline knows nothing about. `Token()` — which
  every request path calls first — is where the mint happens.
- **Single-flight + generation-aware**: N concurrent callers share one mint; a caller whose
  credential was already replaced does not trigger a second.
- `Invalidate()` (called by the SDK after a 401 **or an auth-shaped TLS alert**, classified
  by `sdk/authfail`) re-mints. A refused certificate has no 401 to carry it — the server
  rejects it in (TLS 1.2) or right after (TLS 1.3) the handshake — so the transport error
  is classified too, on both the bus and the egress stream.
- **Host-key mismatch is TERMINAL**: latched, never retried — fail closed forever for that server.
- `refreshLoop` proactively re-mints at ~50% of remaining TTL, jittered ±25%, clamped [1m, 12h]; `defaultRefreshDelay=1h` before first mint.
- **Persistence** (credentials scope only): the issued certificate + key are written to the
  agent's OWN store — `<state>/broker/creds/credentials/<escaped-server>/{client.pem,client.key}`,
  0700 dirs / 0600 files, atomic temp+rename, key first, whole write under a per-server
  flock (`sdk/creds`). Deliberately NOT `~/.shed/creds`, which is the CLI's control-scope
  material: two processes rotating into one directory would overwrite each other in place.
  A stored certificate is loaded at start only when the entry records `auth_mode: mtls`,
  and is REMOVED when a mint comes back in token mode (a flip back leaves inert material
  for a mode the server no longer serves). The control scope persists nothing.
- `controlTokenProvider.Token(server)` **always** mints fresh (never serves a cache) because
  a restarted server silently invalidates control tokens; concurrent asks for one server
  coalesce. `Credential(server, csr)` does NOT coalesce — two requests carry two CSRs, and
  sharing one answer would hand an app a certificate for a key it does not hold. Both
  reject, before any mint: unknown server, no SSH endpoint, or open (non-https) server.

### 7.4 Test seam for deterministic minting

`CredentialMinter.bootstrapRun` (default `sdkbootstrap.RunCredential`) and
`relayRun` (default `sdkbootstrap.RunWithCSR`) are fields. Tests set them → no ssh,
no server. At the `credentialSource` layer the `minter` interface is faked by
`fakeMinter` (canned `[]mintResult`, counts calls, records the relayed CSRs) and
`minterFunc`. A harness drives minting entirely through these — no live ssh needed.

---

## 8. The app's approval gate (`crates/shed-app/src/broker_bridge.rs`)

The daemon's desktop UDS server (its handshake, single-consumer supersede, replay ring,
`token.get` and `credential.get`) went with the daemon (plan 037). In the app, a
`shed-desktop` policy selects the bridge's in-process gate (`AppGate`): it posts an
`approval_request` (`id`, `ts`, `server` — empty in single-server mode — `namespace`,
`op`, `shed`, `detail`, `expires_at` = now + `approval_timeout`) to the app's
Coordinator, which is always attached (the app is its own consumer), and waits for the
person's decision; `decided_by` defaults to `user`, `scope` and `ttl` pass through to
the audit line. No decision within `approval_timeout` (default 25 s) → deny with no
`decided_by`; a late decision finds nothing and is a no-op. The harness's control socket
stands in for the Coordinator (`{"op":"subscribe"}`, `{"op":"decide"}`).

---

## 9. Egress (`egress_handler.go`, `egress_handler_test.go`)

- Consumes shed-server SSE `GET /api/egress/stream` (`egress_handler.go:25,133`), `Accept: text/event-stream`, `Authorization: Bearer <token>` when a token is available.
- **Read-only fan-out**: each `data:` line is JSON-decoded into `egressDecision{ts,shed,host,port,resolved_ip,protocol,verdict,reason}` and mapped to an `AuditEntry` (`egressAuditEntry`, `:174`): `ns:"egress"`, `op:d.Protocol`, `result:d.Verdict`, `detail:"host:port"` (+ ` (ip)` if resolved), `reason:d.Reason`, `ts` = server ts. Malformed frames skipped. → audit log + desktop feed. **Never gates/modifies egress.**
- Token: secure server uses a **control**-scoped `credentialSource` (`bearer()`, `:87`); open server sends its static (usually empty) `ServerTarget.Token`; a mint error sends none (→ 401 → retry).
- **401** → `tokens.Invalidate()` (re-mint on reconnect) then retry (`:147`).
- **501 or 404** → `errEgressUnavailable` → **hard backoff 5 min** (not the 30s exp backoff), while still re-checking (`:112,150`). Other non-200 → `unexpected status %d`, normal exp backoff (1s→30s, `×2`).
- **Gating**: no config toggle; the subscriber always runs per server. Enabled/disabled is decided **by the server** (501/404 = disabled).
- TLS: pinned transport for https+fingerprint; **pin set on a non-https URL → fail-closed transport** that errors every request (`egressHTTPClient`, `:200`). Pin verify = sha256 of leaf cert vs `sha256:<hex>` (`:225`).

---

## 9b. The machine pin (plan 035 §3.4; Rust only)

A pinning shed-server answers one machine per user. The broker's side of that contract:

- **Request headers.** Every bus subscribe (`GET …/messages`) and every respond (`POST …/respond`) carries `X-Shed-Machine-Id: <32 lowercase hex>` and `X-Shed-Machine-Name: <name, percent-encoded UTF-8>` (every byte outside RFC 3986 unreserved `A-Z a-z 0-9 - . _ ~` as `%XX`, so a space is `%20` and a `+` is `%2B`). The identity is `machine.json`'s (§4.1). With no identity (it could not be loaded) neither header is sent and the wire is exactly the pre-pin one; an identity whose id or name this build would not write is never sent. The egress stream and the SSH bootstrap carry no identity. An older server ignores both headers.
- **Refusal bodies** are read within 1024 bytes and 3 s (a respond's also within its deadline). A body cut short by either is never classified as a pin refusal or as the server's own; what is shown in `last_error` or written to the log is cleaned (control and Unicode format characters dropped) and capped.
- **Redirects** are followed only to the configured server's own HTTPS origin (scheme, host, port), at most 3 hops; any other redirect is the answer. The identity headers and the client certificate never reach another host.
- **Subscribe answers** (`classify_subscribe_refusal`, on at most 1024 bytes of body, read whole):

  | Status | `error.code` (in `{"error":{"code","message"}}`) | State | Loop |
  |---|---|---|---|
  | 403 | `SOURCE_NOT_BOUND` (+ top-level `source:{machine_id,machine_name}`) | `not-source`, `active_source` set, `last_error` = `not the credential source for this server; the active source is <name> (<id>)` | backoff 1–30 s and retry; **no re-mint**; one INFO line on entering (or when the named machine changes), DEBUG after |
  | 409 | `SOURCE_DUPLICATE_STREAM` | `duplicate`, `last_error` names it | backoff and retry; no re-mint; one WARN on entering, DEBUG after |
  | 429 | `SOURCE_RATE_LIMITED` | `reconnecting`, `last_error` = `unexpected status 429: <body>` | retry after the server's `Retry-After` (whole seconds, capped at 60 s; anything else is ignored), never sooner than the current backoff; a `retry_now` does not cut that wait short |
  | 409 | anything else, or no parseable or complete body | `rejected` | **terminal**, as before (`namespace already has an active subscriber: <body>`) |
  | 401 | any | `reconnecting` | re-mint, backoff, retry (as before) |
  | any other (a 403 with another code — `SOURCE_MACHINE_ID_REQUIRED`, a scope failure, a script key, a reserved namespace — or a 403 whose body does not parse or was cut short) | — | `reconnecting`, `last_error` = `unexpected status <n>: <body>` | backoff and retry (as before) |

  `not-source` is entered if and only if the status is 403 **and** the body was read whole **and** parses with `error.code == SOURCE_NOT_BOUND`; a truncated, slow or unparseable body never is, even when the part read parses. Any other state clears `active_source`.
- **The daemon and the desktop's embedded broker on one machine.** The presence lock that let the two share one machine identity, and the daemon's yielding to the app, went with the daemon (plan 037).
- **Respond answers.** A 503 or 429 **with `Retry-After`** means the server wrote nothing: the same body is re-sent at most 3 times. Each POST waits at most `RESPOND_ATTEMPT_TIMEOUT`, 8 s: the server's own delivery deadline (`responseTimeout`, 5 s, `internal/plugin/bridge.go`), after which it answers `503 RESPONSE_NOT_DELIVERED` with `Retry-After: 1`, plus 3 s of margin, so that 503 arrives and is retried instead of this client giving up first. One deadline, `RESPOND_RETRY_BUDGET` = 8 + 1 + 8 = 17 s from the first send (room for one more whole attempt after that 503), covers every refusal body read, every wait, every credential fetch and every send after it; when it passes the respond stops and is audited. `Retry-After` must be a non-negative whole number of seconds (waited at least 100 ms) after which one whole attempt (8 s) still fits what is left of the deadline — otherwise the respond ends on that refusal, the server's own word (`undelivered`), rather than on an attempt the deadline would cut short; an HTTP date, a negative, a fraction, garbage or a number too large means no retry. A 503/429 without `Retry-After`, a 403, a 413 and every other non-204 are final, one send. Immediately before every send but the first (a 503/429 retry, a 401's re-send, the one re-send after a lost pooled connection), after its credential fetch, at least one whole attempt (8 s) must be left of the deadline; otherwise nothing is sent and the respond ends (credential work, a re-mint or a wait may have used the room up). The 17 s bounds the respond phase from its first send; it does not cover the time before it (queueing behind earlier requests of the namespace, the approval, the backend, the first credential fetch). One invalidation (401 / TLS alert) and one credential fetch that could mint, per respond, across the retries: after a failed re-mint the surviving credential is presented without minting again.
- **Audit.** When the shed server itself refused a respond (after any retries) — a non-204 whose body, read whole, is the API's error JSON with an `error.code` — the request's audit entry (if the op audits) is written with `result: "undelivered"` and `reason: "result <what the op came to>, but the server refused the response (status <n> <error.code>)"` appended to any existing reason. Any other non-204 (a proxy's 502, an empty or cut-short body) keeps the op's result and appends `the response was answered with status <n>; delivery unknown`. A POST that got no answer within its 8 s keeps the op's result and appends `the response got no answer within 8s; delivery unknown`. Any other transport failure leaves the entry as before (the answer may have been delivered). A send that was not made because less than a whole attempt was left (or the deadline passed during its credential fetch) is recorded by the last thing known about the answer: the server's last refusal of it in this respond (a 401, whose body is read for its code, or a 503/429 before a retry), as that refusal would be (`undelivered` with its code, or delivery unknown for a non-API body); with none, the op's result is kept and `the response could not be sent again within its deadline; delivery unknown` is appended. An answer not sent because the desktop app's embedded broker took over (it runs, or started since the handler did) is recorded as above (`undelivered`, `left for the desktop app`) when no POST of it was made or every one made was refused by the shed server itself; when an earlier POST of it may have reached the shed (no answer in its 8 s, a transport failure after the request may have been written, a non-API refusal body), the op's result is kept and `an earlier send of the response was answered with status <n>; delivery unknown; the retry was not sent: left for the desktop app (...)` is appended (`got no definite answer` in place of the status when the last POST got none).
- **Waking.** `Supervisor::retry_now` (and the embedded broker's `EmbeddedBroker::retry_now`) cut every waiting subscribe backoff short and reset it to the floor, so a machine just made the source connects at once.
- **The desktop's embedded broker** loads the same `machine.json` through the same function at spawn and sends the same headers. Like the daemon, an identity it cannot load does not stop it: it brokers without one, and `broker.status` carries `machine` / `machine_error`.
- **Harness.** `tests/broker-wire/synthetic_bus.py` and `desktop/tools/shedtest/mockserver.py` answer each refusal with these exact shapes (`refuse=` / `accept`, `respond_script`) and record the identity headers; the broker cells are `test_source_pin.py`. Both match responds as the server does: a request is pending from the write of its frame, an answer must name a pending request of its namespace and shed (`in_reply_to`), else `403 FORBIDDEN`, a final answer consumes it, and a move away drops the requests pending on the old machine (so after a move A → B → A, A's answer to a request delivered before the first move is `403 FORBIDDEN`). Their codes are the server's: a respond body over the limit is `413 REQUEST_TOO_LARGE`, an unconfirmed move `500 INTERNAL_ERROR`, a malformed machine id `400 INVALID_REQUEST`. A move away also ends the old machine's live streams. `SyntheticBus.restart()` is a server restart (every stream ends, every pending request is gone); `drop_streams()` is a lost connection with the server up (every stream ends, pending requests kept). The synthetic bus re-delivers, as the server does: a new stream is first sent every request pending on its namespace that was delivered to the same machine id at the same binding generation (bumped by each move away and back), in first-delivery order; and a stream that has ended never delivers a frame it takes afterwards (the frame goes back on the queue); a re-delivered frame whose request was answered finally after the new stream copied it (the old stream's answer) is not written and does not become pending again, as `Listener.Take` checks the message is still live. The desktop mock does NOT model re-delivery (stated in `mockserver.py`): no desktop cell sees the handoff of an in-flight request; the synthetic bus's own cells (`test_synthetic_bus.py`) pin it. The desktop mock binds each stream to the binding generation it was admitted at: a stream that stopped being live cannot take a request (it goes back for the next stream), as the server's `Listener.Take` refuses an unregistered listener; it checks a respond's machine headers, then its size, then the binding, in the server's order; and `pause()` (a restart) forgets pending requests. `SyntheticBus(single_holder=True)` adds the duplicate rule for one bound machine (one live stream per namespace, `holders()`, `attempts()`); their own contract is pinned by `test_synthetic_bus.py` and `desktop/tools/shedtest/test_mockserver_sources.py`.

---

## 10. Supervision / lifecycle (`supervisor.go`, `watcher.go`, `main.go`)

### 10.1 Reconcile semantics (`Supervisor.Reconcile`, `supervisor.go:154`)
- Diffs desired `[]ServerTarget` against running groups by **name**. Stops a group when its name is gone **or the whole target struct changed** (URL, Token, **TLSFingerprint** — a rotated token or newly-added pin restarts the watcher). Unchanged (same name+URL+token+pin) → left running (no churn). Starts groups for new names. Idempotent; post-`Shutdown` → no-op.
- Cancel under lock, drain (`<-g.done`) after releasing it. Warns once when the desired set first becomes empty.

### 10.2 Per-server group (`startWatcherGroup`, `supervisor.go:54`)
- Builds an `sdk.HostClient` with `WithServerURL/WithLogger/WithTLSPin`; if `shouldMint(deps,t)` (`:46`: minter present + SSHHost + SSHPort>0 + **https URL**) attaches `WithTokenProvider(credentialSource)` + a proactive `refreshLoop`, else `WithToken(t.Token)`.
- Runs SSH handler always; AWS/Docker handlers only when their backend is non-nil; egress subscriber always (control-token source only for secure servers).
- `shouldMint` matrix is a clean golden: true **only** for https (case-insensitive) + non-nil minter + non-empty SSHHost + SSHPort>0.

### 10.3 Shutdown (`EmbeddedBroker`)
- The shutdown flip ends the watch loop, which then runs `sup.shutdown()`: every group is cancelled and drained. The app flips it on exit and does not wait; `EmbeddedBroker::shutdown_and_wait(bound)` flips it and waits for the loop task, returning `true` only once every group has drained (`false` when `bound` passed first).
- The harness (`shed-broker-harness`): SIGTERM/SIGINT → `shutdown_and_wait(5 s)` → `harness drained` in its log and **exit 0**, or `harness drain timed out` and **exit 3**. **Exit 1** when the config exists but does not load or the broker does not spawn (an unknown `ssh.mode`, or an explicit `agent-forward` with `$SSH_AUTH_SOCK` unset); a missing config is the app's synthesized default, not an error. **Exit 2** on a usage error. (The daemon's CLI exit codes went with it.)

### 10.4 Watch loop (`watcher.go:18`)
- `off` → reconcile once, idle until ctx done. `poll` → ticker (`poll_interval`, default 10s). `fsnotify`/`""` → watches the source's **parent dir** (survives atomic rename), debounced (`debounce`, default 500ms), matches basename; falls back to poll if fsnotify can't start. Unknown mode → poll with a warning. Single-server mode = `{Watch:"off"}` reconciled once (`main.go:234`).

---

## 11. Test corpus inventory (golden-fixture sources)

`P` = PURE (deterministic in→out, no clock/net/exec — best goldens);
`S` = SEEDED-EXTERNAL (deterministic given one faked seam: temp file, env var,
injected fake); `L` = LIVE (real socket/exec/timing — skip for language-neutral goldens).

| File | LoC | Key tests / vectors | Class |
|------|-----|---------------------|-------|
| `approval_test.go` | 52 | gate `Method()`/`Approve` truth table; `gateFor(policy)` → gate type (``→deny-all, approve-all, shed-desktop, nil-channel-denies) | **P** |
| `config_test.go` | 353 | `Validate()` error strings (`aws.sheds was removed`, `aws.mode`, `aws.servers.mini2.sheds.web.mode`); `resolveAllowPassword`/`EffectivePolicy` defaults; `ApprovalTimeoutDuration` (25s/40s/reject `nonsense`,`0s`,`-5s`); **`TestExampleConfigIsValid`** parses the committed `configs/extensions.example.yaml` (ssh `biometrics-or-password`, docker `[index.docker.io, ghcr.io]`) | **P** + **S** (temp/committed YAML) |
| `config_discovery_test.go` | 194 | `ServerSelector` YAML unmarshal (`all`/omitted/`""`/`[]`/list/scalar); `ResolveTargets`; `DockerConfig.Resolve` inheritance; discovery defaults (fsnotify/10s/500ms) | **P** (unmarshal/resolve) + **S** (defaults) |
| `discovery_test.go` | 104 | `LoadDiscoveredServers` → sorted `[]ServerTarget`, port-default 8080, api_url override → `https://tlshost:8443` + Token + `sha256:abc123`, empty-host skip, missing-file→empty, malformed→err | **S** (temp YAML) |
| `credmint_test.go` | 324 | `knownHostsPinned` (pinned→nil, missing/absent→err, `@revoked`→err); `Mint` param assertion + `ErrHostKeyMismatch` terminal + unpinned-never-runs-ssh; `credentialSource` cache/invalidate/near-expiry/terminal/proactive | **S** (bootstrapRun/fakeMinter) except single-flight = **L** |
| `controltoken_test.go` | 162 | always-mints-fresh, restart re-mint, errors (unknown/no-ssh/open-http) before mint (calls==0) | **S**; overlap test **L** |
| `aws_backend_test.go` | 556 | `Resolve` (10 cases) + `Enabled` (8) matrices; `parseSessionExpiry`/`parseExpiryValue` layouts; `NoRoleConfigured` exact err; passthrough creds-ini → creds + expiry; cache-hit isolation; `TestPassthroughStatus` | **P** (Resolve/Enabled/parse) + **S** (env+temp ini, cache) |
| `aws_handler_test.go` | 391 | `awsExpiryDetail` (`expires:none`/`expires:19:05`); creds→payload (`expiration` present/omitted); denied-by-gate → `ASSUME_ROLE_FAILED` + backend not called; status role | **P** (awsExpiryDetail) + **S** (loopback SSE + mock backend) |
| `docker_backend_test.go` | 610 | `normalizeRegistry`, `decodeInlineAuth` (colon-in-pw, invalid), `augmentPATH`, `dockerError.Error`, `Status`; allowlist/allow_all/credHelper/credsStore/priority/not-found via temp config + mockExecutor | **P** (pure fns) + **S** (temp config/executor); `ExecHelperResolvesViaExtraDir` **L** (real `/bin/sh`) |
| `docker_handler_test.go` | 503 | get→payload; audit-result mapping (`CREDENTIALS_NOT_FOUND`→`anonymous`, others→`error`); approval-denied → guest gets `REGISTRY_NOT_ALLOWED`, audit code `APPROVAL_DENIED`; list/ping/status | **S** (loopback SSE + mock backend + gates) |
| `ssh_handler_test.go` | 319 | list→comment; sign→`ssh-ed25519` format + non-empty blob; ping `ok`; status mode/key_count | **S** (loopback SSE + stub backend); sign key is random |
| `ssh_backend_localkeys_test.go` | 103 | load+sign round-trip (`Verify` accepts), `Mode()=="local-keys"`, unknown-key err; format `ssh-ed25519` | **S** (in-mem key struct; random material) |
| `egress_handler_test.go` | 154 | `egressAuditEntry` → ns `egress`, op=protocol, result=verdict, detail `evil.com:443 (1.2.3.4)`; SSE stream requires Bearer; sends **control** token; 401→Invalidate; 501→`errEgressUnavailable`; pin-on-plain-URL fail-closed | **P** (egressAuditEntry, pin) + **S** (loopback HTTP + fakeTokenSource) |
| `desktop_server_test.go` | 434 | `prepareSocketPath` (missing/non-socket/stale/live); approve/deny/timeout/no-consumer; audit fanout all-ns; code/reason forwarding; last-writer-wins supersede; hello_ack gate_namespaces; decision-detail audited; token.get→token.response | mostly **L** (real UDS) except `ApprovalTimeoutDefault`/`gateFor` = **P** |
| `supervisor_test.go` | 227 | **`shouldMint` matrix** (7 cases); reconcile add/remove/no-churn/url-change/cred-change/dedup; shutdown; health sorted | **P** (shouldMint) + **S** (injected `newGroup` factory) |
| `watcher_test.go` | 90 | off/poll/fsnotify loops with real timers + file events | **L** |
| `audit_test.go` | 122 | `Log(...)`→JSONL fields; disabled→no file; perms 0700/0600; 20-goroutine → 20 lines | **S** (temp file); concurrency = **L** |
| `status_test.go` | 168 | `socketDir`/path composition (env override); `renderStatus` text landmarks; full round-trip over socket (pid/version/decided-in-shed-desktop); schema-skew→exit 1 | **P** (paths, render) + **L** (socket) |
| `logging_test.go` | 33 | `newLogWriter("")==stderr`; path → rotating file write | **P** + **S** |
| `touchid_darwin_test.go` | 38 | `newApprovalGate` resolves `allowPassword`/`Method()` for biometrics vs -or-password (does NOT call Approve → no real prompt); darwin-gated | **P** (darwin only) |

**Best pure golden sources** (language-neutral, no seam): `approval_test.go`,
`config_discovery_test.go` (unmarshal/resolve), the pure halves of
`aws_backend_test.go` (`Resolve`/`Enabled`/`parseSessionExpiry`/`parseExpiryValue`),
`docker_backend_test.go` (`normalizeRegistry`/`decodeInlineAuth`/`augmentPATH`),
`aws_handler_test.go:awsExpiryDetail`, `egress_handler_test.go:egressAuditEntry`,
`supervisor_test.go:shouldMint`, `status_test.go` (path composition + `renderStatus`).

**Strong one-seam SEEDED goldens:** `LoadConfig`/`LoadDiscoveredServers`
(temp YAML → struct/`[]ServerTarget`), AWS passthrough ini parsing (env → creds),
Docker resolution (temp `config.json` + injected executor), the three handler
request→response payload maps (mock backend + `noopGate`/`denyAllGate`), the
minter (`bootstrapRun`/`fakeMinter`), and supervisor reconcile (fake factory).

**Skip (LIVE):** all of `watcher_test.go`; single-flight/overlap concurrency
(`credmint_test.go:275`, `controltoken_test.go:86`); most of
`desktop_server_test.go` (UDS IPC); the socket-serving half of `status_test.go`;
`TestExecHelperResolvesViaExtraDir`; `TestAuditLoggerConcurrent`.
