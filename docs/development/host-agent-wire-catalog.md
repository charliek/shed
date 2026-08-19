# shed-host-agent — Wire-Visible Behavior Catalog

**Authority.** The Rust implementation (`crates/shed-host-agent`, `crates/shed-broker`)
is authoritative for shed-host-agent's wire-visible behavior. Its executable contract
coverage is the goldens + fixture vectors under `tests/host-agent-diff/` (see that
directory's README) — those, not this document, are what a behavior change must keep
green.

This document predates the Rust port and was written against the Go daemon
(`cmd/shed-host-agent`), which was retired in plan 006. Every `file.go:line` citation
below is **traceability only**: it points at where the described behavior was proven
against the Go implementation, resolvable with `git show bbd73e2^:cmd/shed-host-agent/<path>`
(`bbd73e2^` is the last commit that carried `cmd/shed-host-agent` before its deletion;
see `git log -- cmd/shed-host-agent`). The citations are **not** re-pointed at the Rust
source line-by-line — treat the prose as the contract and the Rust crates + the golden
fixtures as its current, executable form. All citations are `file.go:line` in
`cmd/shed-host-agent/` at that commit unless another package is named.

Protocol/const source of truth: `internal/ext/protocol/{ssh,aws,docker}.go`
(namespaces, ops, codes, payload field names) and `sdk/envelope.go` (envelope).

---

## 0. Observable output channels (what the harness must capture)

| # | Channel | Direction | Transport / location | Format |
|---|---------|-----------|----------------------|--------|
| 1 | **Bus responses** | agent → shed-server | SDK `HostClient.Respond` HTTP POST to the server bus (subscribed via SSE) | `sdk.Envelope` (JSON) with a per-namespace payload |
| 2 | **Durable audit log** | agent → disk | file at `cfg.logging.path` (default `~/.local/share/shed/extensions-audit.log`) | one `AuditEntry` JSON object per line (JSONL), `json.Encoder` `\n`-terminated (`audit.go:80,113`) |
| 3 | **Desktop approval channel** | agent ↔ app | UDS `<socketDir>/host-agent.sock`, perms `0600` | newline-delimited typed JSON frames (`desktop_protocol.go`) |
| 4 | **Status socket** | agent → any client | UDS `<socketDir>/host-agent-status.sock`, perms `0600` | one `LiveStatus` JSON blob per connection, then close (`status_server.go:97`) |
| 5 | **`status` subcommand** | CLI → stdout | reads channel 4 | `LiveStatus` pretty-JSON (`--json`) or text report; **process exit code** 0/1/2 |
| 6 | **Operational log** | agent → stderr or file | `-log-file` → lumberjack-rotated file, else stderr | `slog` text lines (loose; not a good diff target) |
| 7 | **Egress SSE consumer** | agent → shed-server | HTTP `GET /api/egress/stream` | observable *request*: method, path, `Accept: text/event-stream`, `Authorization: Bearer` |
| 8 | **SSH mint subprocess** | agent → `ssh` | `exec` of system `ssh` (via `sdk/bootstrap`) | observable *argv* + stdin/stdout contract |
| 9 | **Docker helper subprocess** | agent → `docker-credential-<h>` | `exec ... get`, serverURL on stdin | observable argv + stdin + parsed stdout |
| 10 | **Process lifecycle** | OS | signals / exit | exit codes 0/1/2; socket file teardown |

`socketDir()` (`sockets.go:21`): `$SHED_HOST_AGENT_SOCKET_DIR` if set; else darwin
`~/Library/Application Support/shed`; else `$XDG_RUNTIME_DIR/shed`; else
`~/.local/share/shed`. **The two socket paths are NOT configurable** — fixed
public interface; only the dir env override exists.

---

## 1. Volatile-field normalization mask (redact/normalize before diffing)

These vary run-to-run and MUST be masked. Grouped by channel.

**Audit JSONL (channel 2) / desktop `event` frame (channel 3):**
- `ts` — RFC3339 `time.Now().UTC()` stamped in `LogEntry` if unset (`audit.go:106`). VOLATILE. (Exception: egress entries carry the server-supplied `ts` — still wall-clock.)
- `detail` — VOLATILE **only for AWS ok** (`awsExpiryDetail` → `expires:15:04` / `expires:none`, `aws_handler.go:168`) and **SSH list** (`"N keys"`). Docker/SSH-sign details are stable (registry / key type).
- `ttl` — config-derived (`4h`), stable per config, but treat as env-derived.

**Desktop frames (channel 3, `desktop_protocol.go`):**
- `id` — UUIDv7 `newID()` on every outbound frame (`hello_ack`, `approval_request`, `event`, `ping`, `token.response`). VOLATILE.
- `ts` — RFC3339 UTC `nowRFC3339()`. VOLATILE (except `event.ts`, which echoes the audit entry's `ts`).
- `expires_at` — `approval_request` = `now + timeout`; `token.response` = mint expiry. VOLATILE.
- `agent.version` / `hello_ack` version — build-version string. VOLATILE.
- `request_timeout_ms` — stable per config (`int(timeout/ms)`).

**Status (channels 4/5, `status.go`/`status_server.go`):**
- `pid` (`os.Getpid()`), `started_at`, `written_at` (snapshot time), `version`, per-namespace `since` (RFC3339). All VOLATILE.
- `config_path` — absolute path, env-dependent. VOLATILE.
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
| `result` | Result | no | `ok`/`error`/`denied`/`anonymous`; egress uses the **verdict** (`allow`/`deny`) |
| `detail` | Detail | **yes** | free text (registry, key type, `N keys`, `expires:HH:MM`, `host:port (ip)`) |
| `code` | Code | **yes** | protocol error code or audit-only `APPROVAL_DENIED`; empty on ok |
| `reason` | Reason | **yes** | host-side human explanation; never raw helper stderr |
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

### 3.3 Audit → desktop event fan-out (`desktop_server.go:421-448`)

Every `LogEntry`/`Log` publishes to in-process subscribers (`audit.go:117,145`,
non-blocking, drops on a full 256-buffer channel). The desktop server maps each
`AuditEntry` → an `event` frame **1:1**:

```
eventMsg{ v:2, type:"event", id:<newID>, ts:entry.Timestamp, kind:"audit",
  server, shed, ns:entry.Namespace, op:entry.Operation,
  result, detail, code, reason, approval, decided_by, scope, ttl }
```

Field-for-field copy; only `id` (fresh UUIDv7) and the fixed `v/type/kind` are
added. `ts` is the audit entry's own timestamp (NOT re-stamped). Ring buffer of
last **100** events for replay (`ringMax=100`, `desktop_server.go:171,438`).
Fan-out happens even when file logging is disabled (`audit.go:104` comment).

---

## 4. Status (`status.go`, `status_server.go`, `sockets.go`, `status_test.go`)

### 4.1 `LiveStatus` JSON (`status_server.go:25-60`) — emitted by channel 4/5

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
  "approval_channel": {
    "socket_path": "<...>/host-agent.sock",
    "consumer_connected": true,
    "client_name": "ShedDesktop",    // omitempty; from consumer hello
    "client_version": "1.2.0"        // omitempty
  },
  "rc_hub": {                        // the machine rc-hub role (plan 010 §2.6)
    "state": "listening",            // listening | deferred | disabled
    "addr": "127.0.0.1:1029"         // omitempty; the bind/dial address
  },
  "servers": [                       // sorted by name (Supervisor.Health)
    { "name": "mini2", "url": "https://...",
      "namespaces": [
        { "namespace": "ssh-agent", "state": "connected",
          "last_error": "",          // omitempty
          "since": "<RFC3339>" }     // VOLATILE
      ] }
  ]
}
```

- `policies` keys are always the 3 credential namespaces (`status_server.go:81`).
- `state` ∈ `connected|reconnecting|stopped` (also `rejected` in the SDK), from `sdk/hostclient.go:28-34` `ConnConnected/ConnReconnecting/ConnStopped/ConnRejected` → the health snapshot maps `client.Status()` (`supervisor.go:243`).
- `buildLiveStatus` (`status_server.go:65`) composes it: policies from `cfg.*.Approval.EffectivePolicy()`, gate list from `desktopGateNamespaces(cfg)`, approval-channel from `desktop.ConsumerInfo()`, servers from `sup.Health()`.

### 4.2 Status socket behavior (`status_server.go:97`, `sockets.go`)

- Read-only: per connection, write `LiveStatus` JSON then close. 5s write deadline. Separate from the approval socket (a status query never becomes the approval consumer).
- `bindUnixSocket` ceremony (shared, `desktop_server.go:65`): dir `0700`, `prepareSocketPath` refuses to clobber a **live** socket or a non-socket file, removes a **stale** one, binds, chmods `0600`. Stale detection = `net.DialTimeout` 500 ms probe (`desktop_server.go:23,27`).

### 4.3 `status` subcommand (`status.go:28`, `main.go:54`)

- Dials the status socket (2s). Not running → stderr message + **exit 1** (`status.go:32`).
- Decodes `LiveStatus`; `schema != 1` → stderr skew message + **exit 1** (`status.go:48`).
- `--json`/`-json` → indented `LiveStatus` (exit 0). Else `renderStatus` text (exit 0).
- Unknown arg or removed `--live` → stderr + **exit 2** (`main.go:61-66`).
- `renderStatus` text landmarks (good for text golden, `status_test.go:108-122`): `shed-host-agent status (pid N, <ver>)`, `Approval policies:`, `(decided in shed-desktop)` for gated ns, `none connected (shed-desktop-policy requests fail closed)`, `Servers (N):`, `(none being watched)`, per-ns marks `ok`/`-`/`x` (`connMark`, `status.go:141`).

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
- The **discovery source** (`~/.shed/config.yaml`, a shed CLI config) is parsed by `LoadDiscoveredServers` (`discovery.go`) into `ServerTarget`s: prefers `api_url` (https) over `host:http_port` (default port 8080); carries `credentials_token`→Token, `tls_cert_fingerprint`→TLSFingerprint, `ssh_port`→SSHPort, `auth_mode`→AuthMode; **skips empty-host entries**; **sorts by name**. Missing file → empty (not an error); malformed YAML → error. Pinned by the golden fixture `tests/host-agent-diff/fixtures/load_discovered_servers.json`.
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
| `sign` | `SSHSignResponse{format, blob(b64 sig), rest:""}` | gate deny → `{"approval denied", SIGN_FAILED}`; bad pubkey b64 → `{"invalid public key encoding", INTERNAL_ERROR}`; unparsable pubkey → `{"invalid public key", KEY_NOT_FOUND}`; bad data b64 → `{"invalid challenge data encoding", INTERNAL_ERROR}`; backend sign err → `{"sign operation failed", SIGN_FAILED}` | denied / ok / error; detail = `pubKey.Type()`; approval = method + decided_by/scope/ttl |
| `ping` | `SSHPingResponse{status:"ok"}` | — | none |
| `status` | `SSHStatusResponse{connected:true, mode, key_count}` (mode = backend.Mode()) | — | none |
| unknown | — | `{"unknown operation: <op>", INTERNAL_ERROR}` | none |

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
2. `bootstrapRun(Params{Host, Port, KnownHostsPath, Scope, ClientKind:"host-agent"})` → `sdk.Bundle`. Returns `(bundle.Token, bundle.ExpiresAt, err)`.

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
- `scopeControl` — used for (a) the egress SSE stream (control-scoped route) and (b) the desktop's `credential.get`/`token.get` on the app's behalf (`controltoken.go`).
- Difference is only the `Scope` param in the argv + a distinct `credentialSource`. **One certificate carries one scope**, so in mtls mode these are two genuinely different certificates, not one credential used twice.

### 7.2b The two mint entry points

| entry point | keypair | used by | result |
|---|---|---|---|
| `CredentialMinter.Mint` → `sdkbootstrap.RunCredential` | generated HERE, per mint | the agent's own bus + egress credentials | `sdk.Credential` — token, or certificate **+ the matching key** |
| `CredentialMinter.MintRelayed` → `sdkbootstrap.RunWithCSR` | generated by the CALLER (the desktop app) | `credential.get` (§8.6b); and, with an empty CSR, `token.get` | `sdk.Bundle` — token, or certificate and **no key** |

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
  agent's OWN store — `<state>/host-agent/creds/credentials/<escaped-server>/{client.pem,client.key}`,
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

## 8. Desktop UDS server (`desktop_server.go`, `desktop_protocol.go`, `desktop_gate.go`, `approval*.go`)

Protocol v2, newline-delimited JSON, one typed envelope per line
(`desktop_protocol.go:15`). Directions: app→agent `hello`, `approval_response`,
`pong`, `token.get`, `credential.get`; agent→app `hello_ack`, `approval_request`,
`event`, `ping`, `token.response`, `credential.response`.

**Version skew is handled by capability, not by `v`.** `v` is stamped on every frame
and never checked on receive, and bumping it would break the very pairing it is meant
to describe — shed-desktop and shed-host-agent ship separately, so every combination
of versions runs in the field. The agent instead names what it answers, in
`hello_ack.agent_capabilities` (§8.1), and the app checks before sending an optional
message. An agent too old to know a message does not reject it, it drops it, so
without the advertisement a mismatch would surface as a request timeout rather than a
sentence naming what to upgrade.

### 8.1 Handshake (`desktop_server.go:208`)
- New conn must send a `hello` within **2s** (read deadline), else the conn is dropped (`desktop_server.go:216`). A first line whose `type != "hello"` → dropped.
- Agent replies `hello_ack` **before promoting** the connection:
```jsonc
{ "v":2, "type":"hello_ack", "id":<uuid>, "ts":<rfc3339>,
  "agent": { "version":<build>, "approval_method":"shed-desktop" },
  "namespaces": ["ssh-agent","aws-credentials","docker-credentials","egress"],
  "gate_namespaces": [...],           // == desktopGateNamespaces(cfg)
  "agent_capabilities": ["credential.get"],  // omitempty — see below
  "request_timeout_ms": <timeout ms>,
  "accepted": true }
```
- `agent_capabilities` is **omitempty**, and its ABSENCE is load-bearing: that is exactly
  what an agent predating capability advertisement emits, and what a new app turns into
  "upgrade shed-host-agent". It is absent, never `null` and never `[]` — a third state
  nobody reads. (Contrast `namespaces`/`gate_namespaces`, which are non-omitempty and so
  marshal as `null` when nil.)
- The **superseded** ack (`accepted:false`) carries no capabilities either: it is a bare
  `helloAckMsg{}` (§8.2), so the key is absent there too.
- `hello.client{name,version,pid}` is stored and surfaced in status (`client_name`/`client_version`).
- `hello.replay_events` (int) drives replay (§8.4).

### 8.2 Single-consumer / last-writer-wins (`promote`, `desktop_server.go:349`)
- A 2nd connection **supersedes** the 1st: the old consumer receives
  `hello_ack{accepted:false, reason:"superseded"}` and is closed. The new one becomes active. (Not "second rejected" — newest wins.)
- On disconnect (`demote`, `:363`) the active consumer is cleared and **all its in-flight approvals fail closed** (`approved:false`).
- `ConsumerInfo()` (`:393`) reports connected + client identity for status.

### 8.3 Approval request/response correlation + timeout (`RequestApproval`, `:318`)
- No consumer → `errNoConsumer` (→ deny). Else send:
```jsonc
{ "v":2, "type":"approval_request", "id":<uuid>, "ts":<rfc3339>,
  "namespace", "op", "server"(omitempty), "shed", "detail",
  "expires_at": <now+timeout rfc3339> }
```
- Reply `approval_response{request_id, decision:"approve"|<other=deny>, decided_by, scope(omitempty), ttl(omitempty)}` correlated by `id`==`request_id`.
- **Ownership guard** (`resolve`, `:402`): only the consumer the request was sent to may resolve it; a superseded conn that merely saw the id can't.
- Timeout = `s.timeout` (config `approval_timeout`, default 25s; non-positive → 25s, `desktop_server.go:163`) → `errTimeout` → deny. ctx cancel → deny.
- `desktopGate.Approve` (`desktop_gate.go:20`): maps reply → `ApprovalOutcome{decided_by(default "user" if empty), scope, ttl}`, returns the outcome on **both** approve and deny (so denies are audited with detail).

### 8.4 Event fan-out + replay (`forwardAudit`/`replay`, `:421,450`)
- Every audit entry → `event` frame (§3.3) appended to a **100-entry ring**, sent to the active consumer.
- On connect, `replay(c, hello.replay_events)`: replays the **last min(n, ring-len)** buffered events (n≤0 → none). Order preserved.

### 8.5 Peer-UID trust
- **No SO_PEERCRED / peer-UID check** in this code. Trust is the socket's `0700` parent dir + `0600` socket file (`bindUnixSocket`, `desktop_server.go:65-86`) — owner-only filesystem perms are the gate. (Any file-level fake works; there is no UID assertion to satisfy.)

### 8.6 token.get → token.response (`handleTokenGet`, `:296`)
- Inbound `token.get{type,id,server}` handled in its own goroutine (must not stall the read loop). Reply:
```jsonc
{ "v":2, "type":"token.response", "id":<uuid>, "ts":<rfc3339>,
  "in_reply_to": <req.id>, "server": <req.server>,
  "token":<...>(omitempty), "expires_at":<...>(omitempty), "error":<...>(omitempty) }
```
- `nil controlTokens` → `error:"control-token minting is not available"`. Mint error → `error:<msg>`, token/expires_at empty (**fail closed, never a partial token**). Success → token + (non-zero) expiry. Backed by `controlTokenProvider` (§7.3).
- **FROZEN at bearer tokens, and the request carries NO `csr=`.** This is the message every
  shipped app knows, and a certificate cannot be delivered through a `token.response`.
  Against a server whose entry records `auth_mode: mtls` the agent therefore answers with
  an explicit error naming the component to upgrade — `server %q issues client
  certificates (auth.mode: mtls); this shed-desktop is too old to use one — upgrade the
  app` (`controltoken.go:errDesktopTooOldForMTLS`) — **before** any SSH round-trip, and
  never a certificate the app cannot present.

### 8.6b credential.get → credential.response (`handleCredentialGet`)

The mode-agnostic successor to `token.get`, and a SEPARATE message rather than a widened
one. Silently extending `token.get` would leave a new app unable to tell "the server is in
token mode" from "the agent is old", and would let a new agent answer an old app with a
`token.response` carrying no token — which decodes fine and fails later.

- Inbound `credential.get{type,id,server,csr?}`, handled in its own goroutine.
- `csr` is standard-base64 PKCS#10 DER **generated by the APP**. Only the request crosses
  the socket; the private key that will pair with the issued certificate never leaves the
  app process (plan 001 §D6). The agent passes it through **verbatim** as the bootstrap's
  `csr=` argument — it does not parse, re-encode, or substitute one. `csr` is optional
  (`omitempty`); absent means the bootstrap sends no `csr=` argument at all (**not** an
  empty one, which the server rejects).
- Reply:
```jsonc
{ "v":2, "type":"credential.response", "id":<uuid>, "ts":<rfc3339>,
  "in_reply_to": <req.id>, "server": <req.server>,
  "auth_mode":"token"|"mtls"(omitempty),
  "token":<...>(omitempty),        // token mode only
  "client_cert":<PEM>(omitempty),  // mtls mode only
  "cert_serial":<hex>(omitempty), "expires_at":<...>(omitempty),
  "error":<...>(omitempty) }
```
- `auth_mode` names which shape is populated, so the app never infers the mode from which
  field happens to be non-empty. **Never both**; on error, **neither** (fail closed).
- `nil` credential provider → `error:"control-credential minting is not available"`.
- Go: `MintRelayed` → `sdkbootstrap.RunWithCSR` (the relay entry point — it validates the
  bundle's SHAPE but not against a private key, because this side does not have one).

### 8.7 Frame safety
- 1 MiB per-line cap (`maxFrameBytes`, `:19`); 5s write deadline per frame (`consumerWriteTimeout`, `:107`); 10s server→app `ping` keepalive; `pong` is liveness-only.

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

## 10. Supervision / lifecycle (`supervisor.go`, `watcher.go`, `main.go`)

### 10.1 Reconcile semantics (`Supervisor.Reconcile`, `supervisor.go:154`)
- Diffs desired `[]ServerTarget` against running groups by **name**. Stops a group when its name is gone **or the whole target struct changed** (URL, Token, **TLSFingerprint** — a rotated token or newly-added pin restarts the watcher). Unchanged (same name+URL+token+pin) → left running (no churn). Starts groups for new names. Idempotent; post-`Shutdown` → no-op.
- Cancel under lock, drain (`<-g.done`) after releasing it. Warns once when the desired set first becomes empty.

### 10.2 Per-server group (`startWatcherGroup`, `supervisor.go:54`)
- Builds an `sdk.HostClient` with `WithServerURL/WithLogger/WithTLSPin`; if `shouldMint(deps,t)` (`:46`: minter present + SSHHost + SSHPort>0 + **https URL**) attaches `WithTokenProvider(credentialSource)` + a proactive `refreshLoop`, else `WithToken(t.Token)`.
- Runs SSH handler always; AWS/Docker handlers only when their backend is non-nil; egress subscriber always (control-token source only for secure servers).
- `shouldMint` matrix is a clean golden: true **only** for https (case-insensitive) + non-nil minter + non-empty SSHHost + SSHPort>0.

### 10.3 Signal shutdown ordering (`main.go`)
- `signal.Notify(SIGTERM, SIGINT)` (`main.go:106`); on signal → `cancel()` the root ctx (`:111`).
- Shutdown order after the watch loop returns (`:241-247`): (a) `sup.Shutdown()` cancels every group and drains; (b) ctx-cancel closes the desktop + status listeners (each unlinks its own socket via `ln.Close()`); (c) `wg.Wait()`; audit file closed via defer. **Exit 0** on clean shutdown.
- Startup exit codes a harness can watch: **exit 1** on config load failure (`:85`) or SSH-backend init failure (`:118`); **exit 0** for `version`; **exit 0/1** for `status`; **exit 2** for a bad `status` arg / removed `--live`. Sockets appear at bind (logged `socket listening`) and disappear on `ln.Close()`.

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
