# Auth & Transport: Implementation Plan & Acceptance Criteria

> **✅ Implemented (2026-06).** All phases below shipped on the `feat/auth-transport`
> branches — shed [#198](https://github.com/charliek/shed/pull/198), shed-desktop
> [#15](https://github.com/charliek/shed-desktop/pull/15), shed-extensions
> [#35](https://github.com/charliek/shed-extensions/pull/35). The acceptance suite
> passes 11/11 on the VZ dev server. The user-facing reference is now
> [reference/security.md](../reference/security.md) + the
> [Public VPS Deployment](../guides/vps-deployment.md) guide; this doc is retained
> as the design/acceptance record. Two corrections vs. the plan as written:
> (a) the **SSE credential-bus keepalive moved from Phase 1 to Phase 5**, where
> TLS-over-NAT makes it matter (a `WriteTimeout: 0` already keeps SSE alive on a
> LAN); (b) the **Phase-6 `public_exposure` preflight does not require
> `internal_http_port`** — the bus + Connect tunnel are credentials-scope-gated on
> the public listener under HTTP-enforce (the plan's route matrix), so the loopback
> internal listener is an optional co-located optimization, not a hard requirement
> (requiring it would break the remote host-agent / remote `shed forward` model).

The buildable, cross-repo plan that turns the [Auth & Transport Security discovery](auth-and-transport-security.md) into shipped work, **and** the acceptance-criteria document for an autonomous, phase-by-phase build. It supersedes the discovery doc's §6 transport "leaning": after an adversarial design review, the chosen primary is **native pinned self-signed TLS + a bearer token**, not HTTP-over-SSH. The discovery doc remains the rationale/threat-model reference; this doc is the work plan and the definition of done.

> **Decision of record (locked).** For the encrypted posture (office LAN + public VPS), the HTTP transport is **native pinned self-signed TLS** (server self-signs a cert with the same lifecycle as the SSH host key; clients pin its fingerprint at `shed server add`) plus a **deny-by-default bearer token**. No ACME, no domains, no reverse proxy required. HTTP-over-SSH stays in the toolbox as an *optional* Go-client convenience, never the load-bearing primary.

> **Panel-reviewed (v2).** This plan was reviewed by Codex, Kimi K2.6, and CodeRabbit before execution. Their findings are incorporated throughout; the load-bearing changes from v1 are: (a) the public-exposure preflight is now **opt-in via an explicit `public_exposure: true` flag** so it can never break the tailnet path or a routine production restart; (b) **separate `control_token` / `credentials_token`** scopes replace a single flat token; (c) a new **Phase 0.5** builds the config-mutate+restart and expected-startup-failure test primitives the existing fixtures lack; (d) the SSE credential-bus **keepalive is now explicit build scope** (it does not exist today) and streaming routes are exempt from the new HTTP timeouts; (e) autonomous test runs are **hard-scoped to the `*-dev` servers** with a target-name guard, because `make test-integration[-dev]` otherwise still touches production; (f) a **shared wire contract** pins cross-repo config keys and the sdk-module dependency choreography. Per-finding disposition is in §12.

## 1. Goals & acceptance test

**Goals (in priority order):**

1. **Make the network perimeter optional, not load-bearing** — the same server runs safely on a tailnet, a shared office LAN, or a public VPS.
2. **Every layer default-off** — the Tailscale/LAN path is byte-for-byte unchanged until an operator opts in. *No phase may change the behavior of an existing default-config server, including on restart/upgrade.*
3. **One canonical lock-down mechanism** — SSH keys seeded from GitHub (`github_users`) are the operator's identity; the HTTP token + TLS pin are auto-provisioned.
4. **Never break the ecosystem** — raw `ssh`, IDE remoting (Zed/VS Code/JetBrains), `rsync`, shed-desktop, and shed-extensions all keep working.
5. **Build toward multi-user/fleet** without building it now — token scopes and key→identity resolution are shaped for it.

shed today treats the network as the auth boundary. The three target postures:

| Posture | Wire | What it needs | Default |
|---|---|---|---|
| **Tailscale / LAN (primary today)** | WireGuard-encrypted already | Authorization only (token); nothing else changes | hardening **off** |
| **Office shared LAN, no Tailscale** | plaintext, semi-trusted insiders | Encryption + auth | opt-in now → default later |
| **Public internet-facing VPS** | hostile | `public_exposure: true` → full bundle enforced | opt-in |

**Acceptance test (definition of done for the core feature):**

> Deploy `shed-server` to an internet-facing VPS with `public_exposure: true`, where **only the operator's GitHub keys** (`github_users: [charliek]` → `https://github.com/charliek.keys`) can SSH in, **all HTTP is encrypted and token-authenticated**, and the **credential bus is unreachable** by any unauthenticated party — with the Tailscale/default-off path unchanged when `public_exposure` is unset.

**Secure-by-default trajectory.** Every layer ships default-off so the tailnet path is untouched and the work proves out incrementally. Once soaked (a release or two), TLS + token flip to **on-by-default with auto-provisioning at `shed server add`** — there is no UX drawback to keeping them on, which was the whole point of choosing native TLS over HTTP-over-SSH. The SSH key allowlist stays explicit-opt-in permanently (turning it on without keys configured would lock the operator out).

**Mandatory bundle.** For *genuine public exposure* (`public_exposure: true`) these stop being independent toggles and become an ordered, all-or-nothing bundle — each one alone leaves a full-compromise path (an allowlisted SSH port still sits next to an unauthenticated credential bus, a token still travels in plaintext without TLS). The startup preflight (Phase 6) refuses to start when `public_exposure: true` but the bundle is incomplete, naming the missing piece. When `public_exposure` is unset (the default), the preflight is inert and bind behavior is exactly as today.

## 2. Target architecture & design decisions

```
                       PUBLIC INTERNET / OFFICE LAN
                                 │
                 ┌───────────────┴────────────────┐
                 │  shed-server (single binary)    │
                 │  native pinned TLS on :8443     │  ← self-signed (SANs cover
                 │  + bearer token (deny-default)  │     advertised names/IPs),
                 └───────┬─────────────────┬───────┘     fingerprint pinned at add
        EXPOSED          │                 │  EXPOSED
        ▼                ▼                 ▼
  ┌─────────────┐  ┌──────────────┐   SSH :2222
  │ control     │  │ credential   │   key allowlist enforced in
  │ plane       │  │ bus          │   handlePublicKey (github_users)
  │ (lifecycle, │  │ /api/plugins │   shells / sftp / -L forwards
  │ images,     │  │ + Connect    │
  │ sessions)   │  │ /api/.../    │   bus + Connect: credentials-scoped
  │ control-tok │  │ connect/*    │   gate; reached on the INTERNAL bind
  │ + bootstrap │  │ creds-scoped │   (loopback) when host-agent is
  │ endpoints   │  │ internal-    │   co-located, or over TLS+creds-token
  │ token-exempt│  │ bind/remote  │   when the host-agent is remote
  └─────────────┘  └──────────────┘
═══════════════════════ host trust boundary ═══════════════════════
  guest VM → 127.0.0.1:498 → vsock (UNAUTHENTICATED by design — VM is the boundary)

  Clients:
   • shed CLI            → HTTPS + pinned cert + CONTROL token (all HTTP paths)
   • shed-host-agent     → HTTPS + pinned cert + CREDENTIALS token (or loopback bus)
   • shed-desktop (Swift)→ URLSession HTTPS + pinned cert + CONTROL token
   • terminals           → ssh(1) to <shed>@host, pinned host key
   • approval UX         → local UNIX socket to host-agent (never network)
   • bootstrap (server add) → GET /api/info + /api/ssh-host-key + cert fetch: TOKEN-EXEMPT
```

**Design decisions of record** (the rationale a reviewer/acceptance-checker should hold the build to):

| # | Decision | Why | Rejected / superseded |
|---|---|---|---|
| **D1** | Native pinned self-signed TLS + bearer token as the primary transport | Every client speaks HTTPS natively (curl, Go, Swift `URLSession`); no cert/domain/ACME management; *becomes the default with zero UX drawback* | HTTP-over-SSH (loses curl, needs a deadline-aware channel adapter, single-port blast radius); reverse proxy (extra moving part + cert wrangling) |
| **D2** | Credential bus: credentials-scoped gate; reached on the **internal bind** (loopback) when host-agent is co-located, or over TLS+creds-token when remote. Explicit `bus_url` in client config — never inferred from `host==localhost` | The bus vends live SSH signatures + AWS creds (highest-value target), but a remote host-agent must still reach it across the network | Always-loopback (breaks the remote host-agent on FC/VPS); inferring reachability from hostname |
| **D3** | **Per-scope tokens: `control` and `credentials`** (a scoped token map, format `shed_<scope>_<random>`), not one flat token. Response-injection fixed at the registry (listener identity + pending `(namespace, shed, requestID)` tracking), not at the HTTP handler | CLI/desktop need control without credential-bus power; the host-agent needs credentials; a single shared token cannot express that and cannot stop a forged `/respond` | Single flat `token:` (couldn't separate scopes or stop injection — raised by Codex + CodeRabbit) |
| **D4** | SSH allowlist seeded from GitHub keys, `warn` → `enforce`; **`enforce` + empty key cache = fail startup** (never accept-all); GitHub usernames validated before URL construction | Identity from the key (the GitHub model); username stays the shed name so IDEs/rsync are unaffected; `warn` prevents self-lockout; fail-closed prevents a silent open door | Static per-server lists only (don't scale); SSH CA now (premature); accept-all-on-fetch-failure (a lockout-avoidance footgun that opens the server) |
| **D5** | Desktop stays Swift + `URLSession`; **desktop is a client, decoupled from the server acceptance bundle** | Native TLS removes the one thing Swift couldn't do cleanly; a Swift blocker must not gate the server-side VPS deliverable | Rust core (second client impl); CLI-as-backend (deferred) |
| **D6** | Every layer default-off; secure flips on later | Tailnet path untouched; proves out incrementally; SSH allowlist stays opt-in forever (lockout risk) | Secure-by-default immediately (changes tailnet behavior + risks lockout before soak) |
| **D7** | **Public exposure is opt-in via an explicit `public_exposure: true` flag.** Only then does the startup preflight refuse to bind without the full bundle. Default-unset → preflight inert, bind behavior identical to today (incl. existing `0.0.0.0` fleet on restart) | Makes "you can't half-deploy to the internet" a hard guarantee **without** tripping on tailnet IPs (non-loopback) or breaking a routine production restart — the failure mode all three reviewers flagged | Inferring "public" from a non-loopback bind class (breaks tailnet + upgrade-safety) |
| **D8** | New config: server `internal_bind`/`internal_http_port` (known address for the loopback bus); `http_bind`/`ssh_bind`; `trusted_proxy`; `tls_names` (cert SANs). Client: `api_url`/`bus_url`, `control_token`/`credentials_token`, `tls_cert_fingerprint` | The bus needs a discoverable internal address; curl/TLS needs SAN coverage; the route-surface matrix needs explicit knobs | Dynamic/ephemeral internal port (untestable, undiscoverable) |

## 3. Cross-repo map

**Three git repositories** (the `sdk` is a Go *module inside the shed repo*, not a separate repo — verified):

| Repo (git) | Language | Owns | Build env |
|---|---|---|---|
| **shed** (`/Users/charliek/projects/shed`) | Go | server (bind config, SSH allowlist, HTTP auth middleware, TLS, bus gating + registry identity, preflight, SSE keepalive), CLI client, **and the `sdk/` module** (`HostClient`/`BusClient` — TLS pin + token header on **both** subscribe and respond) | Go (per `go.mod`) ✅ |
| **shed-extensions** (`/Users/charliek/projects/shed-extensions`) | Go | `shed-host-agent` — per-server token+pin threading through `discovery.go` (`shedServerEntry`) / `supervisor.go` / `ServerTarget`; guest binaries unchanged (loopback inside VM) | Go ✅ |
| **shed-desktop** (`/Users/charliek/projects/shed-desktop`) | Swift | `ShedServerClient` (HTTPS scheme + control-token header + `URLSessionDelegate` pin), `ShedConfig` parser fields, both SSH paths → `~/.shed/known_hosts` + `StrictHostKeyChecking=yes` | Swift 6.3.2 + xcodebuild ✅ |

### 3.1 Shared wire contract (pin this before any cross-repo code)

The host-agent reads a **separate, narrower** config struct (`shedServerEntry`: host + http_port only, hardcodes `http://`, `shed-extensions/cmd/shed-host-agent/discovery.go:20-75`) than shed's own `ServerEntry` (`internal/config/client.go:37-42`). The desktop has its own hand-rolled YAML parser (`shed-desktop/Sources/ShedKit/Models/ShedConfig.swift`). All three must add the **same `~/.shed/config.yaml` keys, byte-identical**, or a client silently reads an empty value and fails closed to 401 once enforcement is on. The frozen contract:

```yaml
# ~/.shed/config.yaml — per server entry (all new fields OPTIONAL, default empty/off)
servers:
  my-server:
    host: host.example
    http_port: 8080            # legacy plain-HTTP port (unchanged)
    ssh_port: 2222
    api_url: https://host.example:8443   # when set, overrides scheme+host+port for the control plane
    bus_url:  https://host.example:8443   # credential-bus endpoint; loopback URL when co-located
    control_token:     shed_control_xxxxx
    credentials_token: shed_creds_xxxxx
    tls_cert_fingerprint: sha256:....      # pinned at `shed server add`
```

- **Token format:** `shed_<scope>_<base64url-random>`; scope ∈ {`control`, `credentials`, `admin`}. Minted by `shed-server token new --scope <scope>`.
- **SDK option composition:** `WithTLSPin(fp)` installs a `*http.Client` with `TLSClientConfig.VerifyPeerCertificate`; it must **compose with, not be clobbered by**, `WithHTTPClient` (define last-wins precedence and document it). A rotated cert is not picked up by a live host-agent until restart — documented, acceptable for now.
- **Credential namespaces** (require `credentials` scope): `ssh-agent`, `aws-credentials`, `docker-credentials`. Any other namespace uses the `control` scope.
- **0600 posture:** `~/.shed/config.yaml` is already written `0600` (`client.go:139`); the token fields inherit it. The host-agent and desktop read the same file; add a load-time warning if it is group/world-readable.

### 3.2 sdk-module dependency choreography

`shed-extensions` depends on the `sdk` module that lives **inside** the shed repo. During the cross-repo branch work, shed-extensions uses a local `replace github.com/charliek/shed/sdk => ../shed/sdk` directive; **before opening/merging the shed-extensions PR**, swap the `replace` for a pinned pseudo-version/tag of the merged sdk change. The plan's rollout ordering (client support before server enforcement) depends on this: the sdk change lands first, host-agent pins it, then server enforcement flips.

## 4. Test infrastructure setup (prerequisite — P0)

The pytest integration suite (`tests/integration/`, parameterized over `["vz", "fc"]`) is how every server-side phase is validated on **both** backends. Verified current state of this host:

- ✅ `uv`, Go (mise 1.25.x), `jq`, Swift 6.3.2 + xcodebuild present; `uv`/pytest confirmed working (`test_framework_meta.py` 8 passed / 1 skipped).
- ✅ brew `shed-server` running locally (VZ) — config entry `localmac-dev` (host `localhost`, 8080/2222). Brew log at `/opt/homebrew/var/log/shed-server.log`.
- ✅ `mini2` / `mini3` reachable over SSH, both running deb `shed-server` 0.6.5 (FC).

> **⚠️ Production-safety correction (Codex).** `make test-integration` creates/deletes sheds on the **installed brew/deb (production) servers**. `make test-integration-dev` retargets **only VZ** to the dev server — its FC parameter still hits **prod mini3**; `make test-integration-dev-fc` is the inverse. So neither blanket target is prod-safe for an autonomous run. **Autonomous phase validation therefore runs only the new per-phase test files against the `shed_server_dev` fixture, both backends pointed at dev, guarded by a target-name assertion that the server is `*-dev` (see Phase 0.5).** The full-suite `make test-integration` prod baseline is a **manual, operator-run** step, not an autonomous one.

**P0 steps (idempotent — re-derive host state, never trust a stale snapshot):**

```sh
# Every mutation is idempotent: probe first, add only if absent.
shed -s my-server list >/dev/null 2>&1 || shed server add localhost --port 8080 --name my-server
make dev-server-up                                                   # VZ dev :18080/:12222
shed -s my-server-dev list >/dev/null 2>&1 || shed server add localhost --port 18080 --name my-server-dev
make dev-server-up-fc                                                # FC dev mini3:18080/:12222
shed -s mini3-dev list >/dev/null 2>&1 || shed server add mini3 --port 18080 --name mini3-dev
make test-integration-dev && make test-integration-dev-fc           # smoke the dev path (see caveat above)
```

The autonomous runner must **re-derive** host facts at P0 (run `shed -s <x> info`, `ssh mini3 'shed-server --version'`) and branch on the result, rather than assume the embedded snapshot. Any P0 command that would mutate a non-`*-dev` target is a hard stop (§8).

## 5. Phased implementation

Each phase is an independently shippable PR (or small PR stack), default-off, validated on VZ **and** FC against the **dev** servers only. Phases 0.5–6 constitute the public-VPS acceptance bundle; 7+ are beyond the single-user goal. Each phase lists explicit **Done when** acceptance criteria.

### Phase 0.5 — Test-harness primitives `[shed/tests]` *(new; unblocks every later phase)*

**Why.** The existing fixtures (`tests/integration/fixtures/server.py`) only drive `shed -s <name>` against an already-running server with a **static** config. Every server-side phase below needs "set config → restart → assert new behavior → restore," and Phase 6's preflight needs "launch with bad config → assert non-zero exit + log line → never binds." Neither exists today (CodeRabbit C1, Codex, Kimi).

**Scope.**
- A `LocalServer.with_config(overrides)` capability: write a **temporary** config + isolated state dir under `~/.shed/dev/` (never edit the committed `configs/server.dev-parallel.*.yaml`), `make dev-server-restart`, yield, restore. VZ first.
- FC remote variant (`RemoteServer.with_config`): remote temp-config write + `sudo` process bounce on `/tmp/shed-server-dev*`; if too costly, scope config-mutation integration tests to **VZ-only** and cover FC by unit tests + one manual pass (documented, not silent).
- A `subprocess` harness for **expected-startup-failure** (preflight): launch `bin/shed-server serve --config <temp>`, assert non-zero exit + expected stderr line, assert nothing bound.
- A **target-name guard** fixture used by every new test: assert the server under test resolves to a `*-dev` entry on `:18080/:12222`; refuse to run (error, not skip) against `my-server`/`mini2`/`mini3`. This is the enforced version of the §8 safety discipline.

- **Done when:** a VZ test can set `http_bind`, restart the dev server, assert, and restore with no residue; the subprocess harness can assert a refused startup; the target guard hard-fails if pointed at any non-`*-dev` server; committed dev-parallel configs are never modified.

### Phase 1 — Network surface + SSE keepalive + RealIP fix `[shed]`

**Scope.** Add `http_bind` / `ssh_bind` + `internal_bind` / `internal_http_port` to `ServerConfig` (none exist today — both listeners hardcode `:port`, `cmd/shed-server/serve.go:106`, `internal/sshd/server.go:153`; `errChan` capacity grows to 3 for the third listener). Serve the bus (`/api/plugins/*`) and Connect (`/api/sheds/*/connect/*`) routes on the **internal listener** at a *known* address (`internal_http_port`), per the **route-surface matrix** below; the public router exposes the control plane (incl. the token-exempt bootstrap endpoints). **Add a server-side SSE keepalive** (periodic `:\n\n` comment ping on the bus stream — it does **not** exist today, `internal/api/plugin_handlers.go`/`handlers.go`) and client dead-stream detection. Drop/guard `middleware.RealIP` (`internal/api/server.go:38`) behind `trusted_proxy`. Add `http.Server` timeouts (`ReadHeaderTimeout`/`ReadTimeout`/`IdleTimeout`) **but exempt SSE + hijacked Connect streams from `WriteTimeout`** (per-handler `http.ResponseController.SetWriteDeadline`, or no `WriteTimeout` on the streaming server) and a shared `MaxBytesReader` JSON body cap (default 1 MB, `max_request_body_bytes`) applied to JSON handlers only — never SSE/hijack.

**Route-surface matrix** (explicit; do not globally omit bus/Connect from the public router):

| Mode | Control plane | Bus / Connect |
|---|---|---|
| **Default (legacy)** | public bind | public bind (as today) |
| **Co-located host-agent** | public bind | internal bind (loopback) only |
| **Remote host-agent (VPS)** | public bind, TLS+control | public bind, TLS + **credentials** scope |

**Default.** Binds all-interfaces exactly as today when unset; RealIP gated behind `trusted_proxy`; route-surface = legacy. No behavior change for existing deployments.

- **Unit tests:** config parse + listener-address selection; route→listener assignment per matrix; `RemoteAddr` is the real peer unless `trusted_proxy`; **SSE keepalive emits pings under idle**; `WriteTimeout` not applied to streaming handlers; body cap excludes SSE/hijack.
- **Integration (VZ+FC, dev only), new `test_network_surface.py`:** with co-located mode, bus + Connect reachable on the internal bind and **refused** on the public bind; control plane + bootstrap endpoints reachable on the public bind; existing lifecycle unaffected; an idle SSE subscription survives past `IdleTimeout` thanks to keepalive.
- **Docs:** `reference/configuration.md` (the new fields + matrix); `reference/api.md` (bus/Connect placement, keepalive).
- **Done when:** unset config = no behavior change (existing suite green); the route-surface matrix holds on both backends; an idle bus SSE stream stays alive across the configured `IdleTimeout` window; `WriteTimeout` never kills a stream; `RemoteAddr` correct unless `trusted_proxy`; no perf regression vs release baseline on both backends.

### Phase 2 — SSH key allowlist + GitHub seeding `[shed]` *(acceptance-critical)*

**Scope.** Replace `return true` in `handlePublicKey` (`internal/sshd/server.go:174-183`) with an allowlist check. Config `auth.ssh`: inline keys, `authorized_keys_file`, and `github_users` (**validate the username** — non-empty, no `/` or path tricks — then fetch `https://github.com/<u>.keys` at startup + on a refresh interval; **atomic cache write**, keep last-known-good on partial failure; cache to `{state_dir}/github_keys/<u>`). Modes `off | warn | enforce`. **`enforce` with zero resolvable keys (empty cache + unreachable GitHub, or no inline keys) fails startup** with a clear error — never accept-all, never start with an empty allowlist. Per-connection `MaxAuthTries` via the gliderlabs `ServerConfigCallback` (gliderlabs v0.3.8 supports it; true per-IP throttling is a Phase 7+ item — do not over-claim). Decide `_api` explicitly: the allowlist **applies** to `_api` (it is gated like any user; the reservation enforcement at `session.go:38` is separate and unchanged).

**Default.** `off` (accept-all as today). The acceptance config is `mode: enforce`, `github_users: [charliek]`.

- **Unit tests:** allowlist match across multi-key agents; **injected `*http.Client`** so a 500/timeout simulates a GitHub outage and asserts last-known-good cache use; `enforce` + empty cache → startup error; username validation rejects `../foo`, ``, `a/b`; `warn` vs `enforce` decisions.
- **Integration (VZ+FC, dev only), new `test_ssh_auth.py`:** **hermetic** — generate an off-list keypair *and* an on-list keypair, add the on-list public key via `auth.ssh.authorized_keys`, drive `ssh -i`; assert `enforce` denies the off-list key and admits the on-list key, `warn` admits+logs. (Do **not** depend on the runner holding charliek's private key.) A separate, manual GitHub-seeded smoke validates the `github_users` path.
- **Docs:** new `reference/security.md` SSH section; `reference/configuration.md` `auth.ssh`; quickstarts "locking down SSH" note.
- **Done when:** `off` unchanged; `warn` logs+admits; `enforce` denies off-list and admits on-list (hermetic, both backends); `enforce`+empty = startup error; GitHub fetch caches + fails closed to last-known-good (unit); usernames validated; `_api` behavior documented and tested.

### Phase 3 — Server identity hardening `[shed, shed-desktop]`

**Scope.** `shed server add` prints the SHA256 host-key fingerprint; **prompts only when stdin is a TTY** (default `N`/fail-closed), and on non-TTY does TOFU auto-accept unless `--fingerprint SHA256:…` is supplied (then fail-closed on mismatch). Add `--trust-on-first-use` for explicit non-interactive accept. (These bootstrap flags are prerequisites for Phases 4–5; see also `--token`/`--tls-fingerprint` there.) Desktop: point **both** SSH paths at `~/.shed/known_hosts` with `StrictHostKeyChecking=yes` — `RemoteControl.swift` uses `accept-new` and `TerminalLauncher` sets *no* host-key option at all.

**Default.** Always-on improvement; non-interactive-safe by construction.

- **Unit tests:** Swift argv builders assert host-key options on both paths; CLI fingerprint formatting; `--fingerprint` mismatch rejection; **TTY vs non-TTY branch** (no hang on piped stdin).
- **Integration:** manual MITM-at-add-time rejected (documented check).
- **Docs:** `reference/cli.md` (`shed server add` flags); `reference/security.md` server-identity.
- **Done when:** `shed server add` never hangs on non-TTY stdin; `--fingerprint` rejects a mismatch; both desktop SSH paths emit `~/.shed/known_hosts` + `StrictHostKeyChecking=yes`; `swift build` + desktop tests green.

### Phase 4 — HTTP token auth: client support, then enforcement `[shed, sdk, shed-extensions, shed-desktop]`

Split into **4a (client support)** and **4b (server enforcement)** so no partial rollout breaks brokering (Codex).

**4a — client support + token plumbing (no enforcement yet).**
- `shed-server token new --scope {control|credentials|admin}` mints scoped tokens (`cmd/shed-server/token.go`, sharing `serve`'s config/state loading).
- Add `control_token`/`credentials_token`/`tls_cert_fingerprint` per §3.1 to: shed `ServerEntry`, the sdk `HostClient` options (`WithControlToken`/`WithCredentialsToken`), shed-extensions `shedServerEntry`→`ServerTarget`→`supervisor`, desktop `ShedConfig` + `ShedServerClient`.
- Thread the header through **every** client HTTP path, not one: CLI common + long-timeout + SSE create/pull + `Ping` + raw Connect tunnels (`internal/tunnels/connect.go`); sdk `HostClient` on **both** subscribe (GET SSE) and respond (POST); desktop `requestData` + `createShed`.
- `shed server add` gains `--token`/`--tls-fingerprint` and the bootstrap fetch uses the **token-exempt** endpoints (below).

**4b — server enforcement.**
- Deny-by-default auth middleware over `/api`, **except the bootstrap endpoints `GET /api/info` and `GET /api/ssh-host-key`** (needed by `shed server add` before the operator holds a token — Kimi/Codex/CodeRabbit). `control` scope for the control plane; `credentials` scope required for the bus namespaces.

**Default.** `off` (no token required). Rollout: 4a (all repos) lands and is verified with enforcement **off**; then 4b flips enforcement; host-agent must already present its `credentials` token.

- **Unit tests:** middleware 401 on missing/bad token across `/api` incl. plugin subtree; **bootstrap endpoints return 200 without a token even when enforcement is on**; scope checks (`control` cannot hit a credential namespace); both SDK bus call sites attach the header; **host-agent + desktop config-parser tests** for token/scheme/pin/bus-url.
- **Integration (VZ+FC, dev only), new `test_http_auth.py`:** 401 without header / 200 with; `test_api_info_unauthenticated` (bootstrap exempt); partial-rollout guard (4a on + enforcement off → brokering works; then enforcement on with tokens present → still works).
- **Docs:** `reference/security.md` token+scopes; `reference/cli.md` (`token new`, `server add --token`); `reference/configuration.md`; `reference/api.md`; shed-extensions README host-agent token config.
- **Done when:** every `/api` route except the two bootstrap endpoints requires a valid scoped token; `control` is refused on credential namespaces; all client HTTP paths in all three repos attach the right token; brokering survives the off→on enforcement flip on both backends.

### Phase 5 — Native pinned TLS `[shed, sdk, shed-extensions, shed-desktop]`

**Scope.** Server generates a self-signed cert on first start (same lifecycle as the ED25519 host key) with **SANs covering `tls_names`** (configured advertised hostnames/IPs) so `curl --cacert` passes hostname verification; serves HTTPS (`ListenAndServeTLS` / `https_port`). `shed server add` fetches the cert, shows its fingerprint alongside the SSH host-key fingerprint, and pins it (`tls_cert_fingerprint:`). Clients verify by pin: Go `VerifyPeerCertificate`, Swift `URLSessionDelegate`, `curl` via exported `--cacert` **or** documented pin-by-fingerprint if SANs can't cover the address. Replace every hardcoded `http://` (sdk `hostclient.go:20`, `busclient.go:15`; shed-extensions `discovery.go:75`; desktop `AppModel.swift:321`). `shed server update <name> --fingerprint <new>` for rotation.

**Default.** `off` (plain HTTP). Designed for a one-line later flip to on-by-default + auto-provision at `server add`.

- **Unit tests:** cert generation/persistence/rotation; SANs include `tls_names`; pin verification per client (good/wrong/rotated); `WithTLSPin` composes with `WithHTTPClient`.
- **Integration (VZ+FC, dev only), new `test_tls.py`:** handshake + pin succeeds; plain-HTTP refused when TLS required; `curl --cacert` works against a `tls_names` SAN.
- **Soak (separate, `@pytest.mark.soak`, not CI-gating):** `tests/integration/soak_tls_bus.py --server my-server-dev --duration 15m` — hold a `HostClient` bus SSE over TLS idle past the `IdleTimeout`+NAT-evict margin, then assert a credential round-trip still works (this is the safety net for the Phase-1 keepalive).
- **Docs:** `reference/security.md` TLS; `reference/cli.md` (`server add`/`update` fingerprint, `--cacert` export, `tls_names`); new `guides/vps-deployment.md`.
- **Done when:** HTTPS with a persisted, SAN-correct cert; pin verified (good/wrong/rotated); `curl --cacert` works and plain HTTP refused when required; every hardcoded `http://` replaced; host-agent + desktop reach the server over HTTPS; the 15-min bus soak survives the idle window and brokers afterward.

### Phase 6 — Credential-bus hardening + public-exposure preflight `[shed, sdk, shed-extensions]`

**Scope.** Enforce the **`credentials` scope** on bus subscribe/respond. Fix response-injection and listener-squat **at the registry, below the HTTP handler** (`internal/plugin/registry.go`, `internal/api/plugin_handlers.go:83-115`): the registry records the **authenticated listener identity** at `Register`, tracks pending `(namespace, shed, requestID) → listener identity`, and `/respond` is validated against that mapping (a `credentials`-token holder cannot forge a response for a namespace/request it does not own). Handle the legitimate-reconnect case (the host-agent's `subscribeLoop` re-`Register`s on reconnect — re-registration by the same identity must succeed without a squat window). Wire the **route-surface matrix** end-to-end (co-located loopback bus vs remote TLS+creds). Add the **startup preflight**, gated on `public_exposure: true` (D7): refuse to start when `public_exposure` is set but the bundle (allowlist `enforce` + tokens + TLS + bus gated/internal) is incomplete, naming the missing piece; **inert when `public_exposure` is unset** (no effect on the existing fleet).

**Default.** Bus scope enforced only when tokens are enabled (Phase 4 off → bus as today). Preflight inert unless `public_exposure: true`.

- **Unit tests:** bus subscribe/respond rejected without `credentials` scope; `/respond` rejected when the caller doesn't own the pending request; reconnect by the same identity re-registers cleanly; **preflight truth table** (`public_exposure` × bundle-completeness → start / hard-exit) incl. the inert default and a tailnet-IP bind under `public_exposure:false` not tripping.
- **Integration (VZ+FC, dev only), new `test_cred_bus.py`:** a `control`-only token is refused on a credential namespace; a forged `/respond` is dropped; co-located host-agent brokers over the loopback bus and remote host-agent brokers over TLS+creds; **preflight refuse-to-start** via the Phase 0.5 subprocess harness (incomplete bundle + `public_exposure:true` → non-zero exit, nothing bound).
- **Docs:** `reference/security.md` "Credential bus" (scope gate, registry identity, route matrix); `guides/vps-deployment.md` preflight + `public_exposure`.
- **Done when:** credential namespaces require `credentials` scope; a forged `/respond` from a non-owning identity is dropped; legitimate reconnect doesn't flap; the route matrix holds on both backends; preflight refuses an incomplete `public_exposure:true` bind (subprocess-tested) and is provably inert when unset; full acceptance run green on VZ+FC dev servers.

### Phase 7+ (beyond the single-user acceptance — future)

- **7. HTTP-over-SSH (optional, Go clients):** `_api` channel handler intercepted **before** `session.go:38` rejects it → in-process `http.Server.Serve` over a **deadline-capable** `ssh.Channel`→`net.Conn` adapter (the naive wrapper silently disables `http.Server` timeouts/shutdown and leaks goroutines). Keep the loopback plain listener for `curl`.
- **8. Multi-user + ownership:** `auth.users`, key→user / token→user resolution, `owner` on shed metadata, owner-or-admin authz, bus routing scoped to `(namespace, owner)`, per-identity (not just per-scope) response-injection defense.
- **9. `shed login` (GitHub device flow):** server-issued tokens derived from GitHub identity replace static tokens.
- **10. Fleet / control-plane:** control-plane/worker split, SSH CA + host certs, provider abstraction (Daytona-style); true per-IP SSH throttling.

## 6. Cross-cutting testing strategy

**Both backends, every server-side phase — dev servers only.** Each phase's new pytest file runs under the parameterized `shed_server_dev` fixture, both backends pointed at the `*-dev` servers, guarded by the Phase 0.5 target-name assertion. Autonomous phases **never** invoke blanket `make test-integration[-dev]` (those touch prod per §4); they run the specific new test file(s) with the dev fixture + guard.

| Layer | Tooling | Covers |
|---|---|---|
| Go unit | `make test` (CI) | config parse, middleware, allowlist (+injected client), token scopes, cert/SAN/pin, registry identity, preflight truth table, keepalive |
| Swift unit | shed-desktop test target | argv builders (host-key opts), pin delegate, control-token header, config parser fields |
| Integration VZ+FC (dev) | `tests/integration/` new files (P1–P6) + Phase 0.5 primitives | live behavior on real VMs, both backends, `*-dev` only |
| Soak (manual, `@pytest.mark.soak`) | `soak_tls_bus.py` | 15-min idle bus SSE over TLS, then a credential round-trip |
| Preflight (subprocess) | Phase 0.5 harness | expected-startup-failure, nothing bound |
| Perf gate | `test_create_agent_p50` + dev-vs-release | Phase 1 bind/keepalive is boot-path-adjacent; confirm no regression both backends |

**New integration files:** `test_network_surface.py` (P1), `test_ssh_auth.py` (P2), `test_http_auth.py` (P4), `test_tls.py` (P5), `test_cred_bus.py` (P6), plus `soak_tls_bus.py` (manual). Markers in `pyproject.toml`: `network_surface`, `ssh_auth`, `http_auth`, `tls`, `cred_bus`, `soak`, `slow`. Throttle/cache windows configurable so tests run fast.

**Acceptance run (end of Phase 6).** A dev server with `public_exposure: true` + the full bundle: off-list key denied / on-list admitted (hermetic), HTTP requires the right scoped token over TLS, bus rejects a `control`-only token, a forged `/respond` dropped, preflight blocks an incomplete bundle (subprocess) — on **both** VZ and FC dev servers.

## 7. Documentation plan

| Doc | Change | Phase |
|---|---|---|
| **`reference/security.md`** (new page + nav) | SSH allowlist + GitHub seeding, scoped tokens, native pinned TLS + SANs, bind/route matrix, credential-bus model + registry identity, `public_exposure` preflight | P2–P6 |
| **`guides/vps-deployment.md`** (new page + nav) | The acceptance recipe: `public_exposure: true`, `github_users: [charliek]`, token mint, cert pin; verify only your keys get in | P5–P6 |
| `reference/configuration.md` | `http_bind`/`ssh_bind`/`internal_*`/`trusted_proxy`/`tls_names`/timeouts/`max_request_body_bytes`, `auth.ssh`, the §3.1 client fields, `public_exposure` | P1–P6 |
| `reference/cli.md` | `server add --fingerprint/--token/--tls-fingerprint`, `server update --fingerprint`, `shed-server token new --scope` | P3–P5 |
| `reference/api.md` | scoped Authorization, bootstrap-endpoint exemption, TLS, bus/Connect placement + keepalive | P1, P4 |
| `getting-started/*-quickstart.md`, `vz-setup.md`, `fc-setup.md` | "open by default on your LAN; here's how to lock it down" | P2 |
| shed-extensions `README.md`/`CLAUDE.md` | host-agent `credentials_token`/cert-pin/`bus_url` config | P4–P5 |
| shed-desktop docs | control-token + cert-pin + known_hosts hardening | P3–P5 |

## 8. Execution protocol (autonomous run)

**Branches & PRs.** Three git repos (sdk is a module in shed). Implementation on a fresh `feat/auth-transport` branch cut from each repo's `main`. Each phase is phase-scoped commit(s) (`feat(auth): phase N — …`). The plan doc lives on `docs/auth-transport-discovery` (PR #194) and is referenced, not re-committed onto feature branches. shed-extensions uses a local sdk `replace` during the branch, pinned before its PR (§3.2).

**Per-phase cadence:** implement → build + unit + lint green → integration on **both** dev backends (new test file + target guard, not blanket make) → manual functional pass on the dev processes → `/simplify` the diff → `/codex:rescue` review → address → commit → update tracker.

**Safety discipline (non-negotiable — enforced, not just stated):**

- **`*-dev` servers only.** All live testing targets `my-server-dev` (`:18080/:12222`) and `mini3-dev` (`mini3:18080/:12222`), enforced by the Phase 0.5 target-name guard (errors, not skips, on any other target). mini2 is a secondary FC dev fallback.
- **Hard-stop list (any of these = halt + report, never autonomous):** running plain `make test-integration` / `make test-integration-dev[-fc]` blanket targets; restarting/reconfiguring brew or systemd `shed-server`; editing `/etc/shed`, `/opt/homebrew` service state, or `configs/server.dev-parallel.*.yaml`; deleting or mutating any shed on `my-server`, `mini2`, or `mini3`; any FC write outside `/tmp/shed-server-dev*` and `/var/lib/shed-dev/*`.
- **Default-off = rollback.** Every phase ships its feature default-off; the worktree stays shippable.
- **Lockstep (P4–P5):** client support (4a, all repos) verified with enforcement off, then enforcement (4b) on, before committing the enforcing change.
- **Cleanup trap:** on any phase failure, run `make dev-server-down` + `make dev-server-down-fc` before halting, and report the stopping point from the task tracker.

**Hosts (verified reachable):** this Mac (VZ dev + Go/Swift builds + pytest driver), `mini3` (primary FC dev), `mini2` (secondary FC dev).

**End-of-run:** one PR per repo (`shed`, `shed-extensions`, `shed-desktop`) against `main`, cross-linking this plan + acceptance results; then `/watch-pr` each until CI + bot review are green, fixing failures.

## 9. Overall acceptance checklist (definition of done for the run)

- [ ] **Phase 0.5 primitives** built; target-name guard refuses non-`*-dev` servers.
- [ ] **Phases 1–6 each:** committed phase-scoped, `/simplify` + `/codex:rescue` applied, unit + VZ + FC dev integration green, manual functional pass done.
- [ ] **Acceptance test green on VZ and FC dev servers** with `public_exposure: true`: only on-list keys SSH in; HTTP needs the right scoped token over TLS; bus rejects a `control`-only token; forged `/respond` dropped; preflight blocks an incomplete bundle (subprocess); default-off/Tailscale path provably unchanged.
- [ ] **Bus soak** (15-min idle over TLS) brokers afterward.
- [ ] **Docs updated** (security.md, vps-deployment.md, configuration/cli/api, quickstarts, extensions/desktop).
- [ ] **One PR per repo opened;** `/watch-pr` green on each.
- [ ] **Discovery doc reconciled** (slimmed or deleted) per §11.

## 10. Risk register

| Risk | Likelihood | Mitigation |
|---|---|---|
| Autonomous test run touches production (blanket make targets / P0 baseline) | Med→**Low** | Dev-fixture-only per-phase runs + Phase 0.5 target-name guard (errors on non-`*-dev`); §8 hard-stop list; prod baseline is manual-only |
| Preflight breaks tailnet path or a production restart | Med→**Low** | `public_exposure: true` opt-in gate (D7); inert by default; truth-table unit tests incl. the inert + tailnet cases |
| SSE bus dies under new HTTP timeouts / NAT idle | Med | Build the keepalive in Phase 1 (not just test for it); exempt streaming from `WriteTimeout`; 15-min soak |
| Cross-repo token/TLS lockstep breaks brokering | Med→Low | §3.1 frozen wire contract; sdk `replace`→pin (§3.2); 4a-before-4b; verify off→on on dev |
| Flat token can't separate scopes / stop injection | **resolved** | Per-scope `control`/`credentials` tokens (D3) + registry-identity `/respond` validation (Phase 6) |
| No fixture to reconfigure+restart / test preflight | **resolved** | Phase 0.5 builds both primitives before any dependent phase |
| `shed server add` hangs automation / bootstrap catch-22 | **resolved** | TTY-only prompt + non-TTY TOFU (Phase 3); `/api/info`+`/api/ssh-host-key` token-exempt (Phase 4) |
| `curl --cacert` fails hostname verification | Med→Low | `tls_names` SANs in the cert (Phase 5); pin-by-fingerprint fallback documented |
| Swift desktop build friction | Low | Env confirmed; desktop decoupled from the server acceptance bundle (D5) — a Swift blocker doesn't gate the VPS deliverable |
| Autonomous run can't finish all phases overnight | Med | Phases independently shippable + default-off; tracker records the resume point; cleanup trap on failure |

## 11. Final step — reconcile the discovery doc

Once Phases 1–6 ship and are documented in the reference pages, the [discovery doc](auth-and-transport-security.md): (1) fold the decided/built material out (current-state audit, A/B/C/D comparison, §9 phasing — now captured/corrected here and in `reference/security.md`); (2) keep only forward-looking rationale (tiered threat model, future tiers: multi-user/ownership, `shed login`, fleet/control-plane, HTTP-over-SSH) as a slimmed doc, **or delete entirely** if nothing non-obvious remains; (3) default to slim-and-keep, delete if empty. Pull the strike of the stale §6 "Leaning" (B-first) **forward** so an agent re-reading the rationale mid-run can't anchor on it (CodeRabbit m6).

## 12. Panel-review disposition

**Incorporated (multi-reviewer / high-confidence):** preflight → `public_exposure` opt-in (Codex/CodeRabbit/Kimi); SSE keepalive built + streaming exempt from `WriteTimeout` (CodeRabbit/Kimi); per-scope `control`/`credentials` tokens + registry-identity injection fix (Codex/CodeRabbit); Phase 0.5 config-mutate/restart + subprocess preflight harness (CodeRabbit/Codex/Kimi); dev-only test scoping + target guard + hard-stop list (Codex); shared wire contract + sdk `replace`→pin (Codex/CodeRabbit); bootstrap-endpoint token exemption + TTY-safe `shed server add` (Kimi/Codex/CodeRabbit); `enforce`+empty = fail-closed startup (all three); `internal_*` known bus address + `api_url`/`bus_url` (Codex/Kimi/CodeRabbit).

**Incorporated (single-reviewer, strong):** hermetic generated-key SSH tests (Codex); GitHub username validation (Codex); cert SANs / `tls_names` for `curl --cacert` (Codex); body cap as shared middleware excluding streams (Codex); explicit credential-namespace list (Codex); P0 idempotency + re-derive host state (CodeRabbit/Codex); `_api` allowlist decision in Phase 2 (CodeRabbit); soak duration/marker/home (CodeRabbit/Kimi); desktop decoupled from server acceptance (CodeRabbit); cert rotation `server update` (Kimi); cleanup trap on failure (Kimi); `errChan` capacity 3 (Kimi).

**Noted, not separately actioned:** Go version-string drift (use "per `go.mod`"); RealIP log-output change is expected, not a regression (named in Phase 1); `plugin_handlers.go` path confirmed at `internal/api/plugin_handlers.go` (Kimi's "missing" referred to `internal/plugin/`). No blocking contradictions between reviewers.
