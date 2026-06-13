# Auth & Transport: Implementation Plan & Acceptance Criteria

The buildable, cross-repo plan that turns the [Auth & Transport Security discovery](auth-and-transport-security.md) into shipped work, **and** the acceptance-criteria document for an autonomous, phase-by-phase build. It supersedes the discovery doc's §6 transport "leaning": after an adversarial design review, the chosen primary is **native pinned self-signed TLS + a bearer token**, not HTTP-over-SSH. The discovery doc remains the rationale/threat-model reference; this doc is the work plan and the definition of done.

> **Decision of record (locked).** For the encrypted posture (office LAN + public VPS), the HTTP transport is **native pinned self-signed TLS** (server self-signs a cert with the same lifecycle as the SSH host key; clients pin its fingerprint at `shed server add`) plus a **deny-by-default bearer token**. No ACME, no domains, no reverse proxy required. HTTP-over-SSH stays in the toolbox as an *optional* Go-client convenience, never the load-bearing primary.

## 1. Goals & acceptance test

**Goals (in priority order):**

1. **Make the network perimeter optional, not load-bearing** — the same server runs safely on a tailnet, a shared office LAN, or a public VPS.
2. **Every layer default-off** — the Tailscale/LAN path is byte-for-byte unchanged until an operator opts in.
3. **One canonical lock-down mechanism** — SSH keys seeded from GitHub (`github_users`) are the operator's identity; the HTTP token + TLS pin are auto-provisioned.
4. **Never break the ecosystem** — raw `ssh`, IDE remoting (Zed/VS Code/JetBrains), `rsync`, shed-desktop, and shed-extensions all keep working.
5. **Build toward multi-user/fleet** without building it now — token scopes and key→identity resolution are shaped for it.

shed today treats the network as the auth boundary. The three target postures:

| Posture | Wire | What it needs | Default |
|---|---|---|---|
| **Tailscale / LAN (primary today)** | WireGuard-encrypted already | Authorization only (token); nothing else changes | hardening **off** |
| **Office shared LAN, no Tailscale** | plaintext, semi-trusted insiders | Encryption + auth | opt-in now → default later |
| **Public internet-facing VPS** | hostile | Encryption + auth + SSH allowlist + bus lockdown + DoS hardening | opt-in |

**Acceptance test (definition of done for the core feature):**

> Deploy `shed-server` to an internet-facing VPS where **only the operator's GitHub keys** (`github_users: [charliek]` → `https://github.com/charliek.keys`) can SSH in, **all HTTP is encrypted and token-authenticated**, and the **credential bus is unreachable** by any unauthenticated party — with the Tailscale path unchanged when the hardening is off.

**Secure-by-default trajectory.** Every layer ships default-off so the tailnet path is untouched and the work proves out incrementally. Once soaked (a release or two), TLS + token flip to **on-by-default with auto-provisioning at `shed server add`** — there is no UX drawback to keeping them on, which was the whole point of choosing native TLS over HTTP-over-SSH. The SSH key allowlist stays explicit-opt-in permanently (turning it on without keys configured would lock the operator out).

**Mandatory bundle.** For *genuine public exposure* these stop being independent toggles and become an ordered, all-or-nothing bundle — each one alone leaves a full-compromise path (an allowlisted SSH port still sits next to an unauthenticated credential bus, a token still travels in plaintext without TLS). The server must refuse to bind a non-loopback interface unless the whole bundle is satisfied (a startup preflight check, see Phase 6).

## 2. Target architecture & design decisions

```
                       PUBLIC INTERNET / OFFICE LAN
                                 │
                 ┌───────────────┴────────────────┐
                 │  shed-server (single binary)    │
                 │  native pinned TLS on :8443     │  ← self-signed, fingerprint
                 │  + bearer token (deny-default)  │     pinned at `shed server add`
                 └───────┬─────────────────┬───────┘
        EXPOSED          │                 │  EXPOSED
        ▼                ▼                 ▼
  ┌─────────────┐  ┌──────────────┐   SSH :2222
  │ control     │  │ credential   │   key allowlist enforced in
  │ plane       │  │ bus          │   handlePublicKey (github_users)
  │ (lifecycle, │  │ /api/plugins │   shells / sftp / -L forwards
  │ images,     │  │ + Connect    │
  │ sessions)   │  │ /api/.../    │   credential bus + Connect carry a
  │ token-gated │  │ connect/*    │   SECOND, credential-scoped gate
  └─────────────┘  │ scoped-gated │   beyond the general server token;
                   │ + loopback   │   loopback-bindable when the
                   │  when local  │   host-agent is co-located
                   └──────────────┘
═══════════════════════ host trust boundary ═══════════════════════
  guest VM → 127.0.0.1:498 → vsock (UNAUTHENTICATED by design — VM is the boundary)

  Clients:
   • shed CLI / host-agent (Go)  → HTTPS + pinned cert + token header
   • shed-desktop (Swift)        → URLSession HTTPS + pinned cert + token header
   • terminals                   → ssh(1) to <shed>@host, pinned host key
   • approval UX                 → local UNIX socket to host-agent (never network)
```

**Design decisions of record** (the rationale a reviewer/acceptance-checker should hold the build to):

| # | Decision | Why | Rejected alternative |
|---|---|---|---|
| **D1** | Native pinned self-signed TLS + bearer token as the primary transport | Every client speaks HTTPS natively (curl, Go, Swift `URLSession`); no cert/domain/ACME management; *becomes the default with zero UX drawback* | HTTP-over-SSH (loses curl, needs a deadline-aware channel adapter, single-port blast radius); reverse proxy (extra moving part + cert wrangling the user explicitly wants to avoid) |
| **D2** | Credential bus: credential-scoped second gate, loopback-bound when host-agent is co-located, TLS+scope when remote | The bus vends live SSH signatures + AWS creds (highest-value target), but a remote host-agent must still reach it across the network | Always-loopback (breaks the remote host-agent on FC/VPS); one flat token for the bus too (a single leak becomes a credential master key) |
| **D3** | Scoped token format from day one (`control` / `credentials` / `admin`) | Multi-user and the credential split structurally need per-scope authz; shaping the format now avoids a painful token re-issue later | Flat token (forces a migration at tier 2) |
| **D4** | SSH allowlist seeded from GitHub keys, `warn` → `enforce` | Identity from the key (the GitHub model); username stays the shed name so IDEs/rsync are unaffected; `warn` prevents self-lockout | Static per-server key lists only (don't scale); SSH CA now (premature for single-user) |
| **D5** | Desktop stays Swift + `URLSession` | Native TLS removes the one thing Swift couldn't do cleanly; no Rust/FFI/subprocess needed | Rust core (second client impl); CLI-as-backend (needs a CLI daemon; deferred to a future consolidation) |
| **D6** | Every layer default-off; secure flips on later | Tailnet path untouched; proves out incrementally; SSH allowlist stays opt-in forever (lockout risk) | Secure-by-default immediately (changes tailnet behavior + risks lockout before soak) |
| **D7** | Public (non-loopback) bind refused unless the full bundle is present (startup preflight) | Makes "you can't half-deploy to the internet" a hard guarantee, not a doc warning | Rely on operator discipline / documentation only |

## 3. Cross-repo map

**Three git repositories** (the `sdk` is a Go *module inside the shed repo*, not a separate repo — verified):

| Repo (git) | Language | Owns | Build env |
|---|---|---|---|
| **shed** (`/Users/charliek/projects/shed`) | Go | server (bind config, SSH allowlist, HTTP auth middleware, TLS, bus gating, preflight), CLI client, **and the `sdk/` module** (`HostClient`/`BusClient` — TLS pin + token header on **both** subscribe and respond) | Go 1.25.9 ✅ |
| **shed-extensions** (`/Users/charliek/projects/shed-extensions`) | Go | `shed-host-agent` — per-server token+pin threading through `discovery.go`/`supervisor.go`/`ServerTarget`; guest binaries unchanged (loopback inside VM) | Go ✅ |
| **shed-desktop** (`/Users/charliek/projects/shed-desktop`) | Swift | `ShedServerClient` (HTTPS scheme + token header + `URLSessionDelegate` pin), both SSH paths → `~/.shed/known_hosts` + `StrictHostKeyChecking=yes` | Swift 6.3.2 + xcodebuild ✅ |

The token-and-pin threading is a **cross-repo change that must land in lockstep**: if the server enforces before the host-agent presents a token, credential brokering breaks for every shed. Phases 4–6 call out the rollout ordering (client *support* first, server *enforcement* last, or behind a per-server opt-in).

## 4. Test infrastructure setup (prerequisite — P0)

The pytest integration suite (`tests/integration/`, parameterized over `["vz", "fc"]`) is how every server-side phase is validated on **both** backends. It has not been run on this machine before. Verified current state of this host:

- ✅ `uv`, Go 1.25.9 (mise), `jq`, Swift 6.3.2 + xcodebuild present.
- ✅ brew `shed-server` 0.6.6 running locally (VZ) — config entry **`localmac-dev`** (host `localhost`, 8080/2222). Brew log at the default `/opt/homebrew/var/log/shed-server.log`.
- ✅ `mini2` / `mini3` reachable over SSH, both running deb `shed-server` 0.6.5 (FC) — new enough (≥ v0.5.4) for the PhaseTimer tests.
- ⚠️ The suite's VZ default entry name is `my-server`; here it's `localmac-dev`. Most fixtures honor `SHED_VZ_SERVER`, but the framework backstop (`test_meta_full_suite_still_passes_against_brew`) hardcodes `my-server` — so register a `my-server` **alias** to the brew server for a zero-env-var run that also covers the backstop.
- ⚠️ Parallel-dev entries (`my-server-dev`, `mini3-dev`) are not registered yet.
- ⚠️ FC PhaseTimer tests need passwordless `sudo -n journalctl -u shed-server` on the remote; the two timing tests skip cleanly if absent.

**Harness already validated on this machine:** `uv` builds the venv and pytest runs — `test_framework_meta.py` reports 8 passed / 1 skipped (the skip is the brew backstop, which wanted `my-server`). So Python/`uv`/pytest are confirmed working here; the remaining setup is config-entry registration plus the dev servers.

**P0 steps (one-time on this machine):**

```sh
# 1. Register a my-server alias so all suite defaults (incl. the backstop) work
#    with no env vars — points at the same brew VZ server as localmac-dev.
shed server add localhost --port 8080 --name my-server

# 2. Baseline VZ + FC against the INSTALLED servers (proves the live path).
#    Runs both backends: VZ against localmac brew, FC against mini3 over SSH.
make test-integration                 # first run does `uv sync` into tests/integration/.venv

# 3. Register the parallel dev-server entries (for validating server-side source changes).
make dev-server-up                    # VZ dev server on 18080/12222 (nohup, PID file)
shed server add localhost --port 18080 --name my-server-dev
make dev-server-up-fc                  # FC dev server on mini3:18080/12222 (sudo nohup)
shed server add mini3 --port 18080 --name mini3-dev

# 4. Run the suite against the dev servers (what every server-side phase below targets).
make test-integration-dev             # VZ dev
make test-integration-dev-fc          # FC dev (auto-sets SHED_FC_LOG_PATH)
```

Per `CLAUDE.md`, **every server-side phase in this plan must pass `make test-integration-dev` (VZ) and `make test-integration-dev-fc` (FC) against the dev build of the source branch**, not just the brew/deb binary — and any boot-path-adjacent change (Phase 1's bind selection touches startup) gets a release-baseline perf comparison via the parallel-dev pair. PR descriptions state `N/N pass against dev-build at <sha>` for both backends.

## 5. Phased implementation

Each phase is an independently shippable PR (or small PR stack), default-off, validated on VZ **and** FC. Phases 1–6 constitute the public-VPS acceptance bundle; 7+ are beyond the single-user goal. Each phase lists explicit **Done when** acceptance criteria.

### Phase 1 — Network surface config + RealIP fix `[shed]`

**Scope.** Add `http_bind` / `ssh_bind` to `ServerConfig` (none exist today — both listeners hardcode `:port`, `cmd/shed-server/serve.go:106`, `internal/sshd/server.go:153`). Add an optional **internal listener** bound to loopback that carries the bus (`/api/plugins/*`) and Connect (`/api/sheds/*/connect/*`) routes, so co-located deployments keep them off the public interface; the public router omits them. Drop/guard `middleware.RealIP` (`internal/api/server.go:38`) — it trusts client `X-Forwarded-For`, poisoning any future IP-based rate-limit or audit log. Add the missing `http.Server` timeouts (`ReadHeaderTimeout`/`ReadTimeout`/`WriteTimeout`/`IdleTimeout`) and a `MaxBytesReader` on JSON bodies.

**Default.** Binds all-interfaces exactly as today when unset; RealIP behavior gated behind an explicit `trusted_proxy` config. No behavior change for existing deployments.

- **Unit tests:** config parse + listener-address selection; route-to-listener assignment (bus/Connect only on internal listener); `RemoteAddr` is the real TCP peer when no trusted proxy.
- **Integration (VZ+FC), new `test_network_surface.py`:** with the internal listener on loopback, assert `/api/plugins/listeners/...` and `/api/sheds/{n}/connect/{p}` are **unreachable** on the public bind and reachable on loopback; existing lifecycle tests still pass unchanged.
- **Docs:** `reference/configuration.md` (`http_bind`/`ssh_bind`/`trusted_proxy`/timeouts); note in `reference/api.md` that the bus/Connect routes may be internal-only.
- **Done when:** unset config binds all interfaces (no behavior change, existing suite green); a set `http_bind`/`ssh_bind` binds only that interface (verified live); bus + Connect reachable on loopback and **refused** on the public bind on both VZ+FC; `RemoteAddr` reflects the real peer unless `trusted_proxy` set; all four HTTP timeouts + body cap present; no perf regression vs the release baseline on either backend.

### Phase 2 — SSH key allowlist + GitHub seeding `[shed]` *(acceptance-critical)*

**Scope.** Replace `return true` in `handlePublicKey` (`internal/sshd/server.go:174-183`) with an allowlist check. Config `auth.ssh`: inline keys, `authorized_keys_file`, and `github_users` (fetch `https://github.com/<u>.keys` at startup + on a refresh interval, cache to `{state_dir}/github_keys/<u>`, **fail closed to last-known-good** if GitHub is unreachable). Modes `off | warn | enforce`. Add per-IP auth throttling / `MaxAuthTries` via the gliderlabs `ServerConfigCallback` (the SSH port becomes a brute-force *target* only once accept-all is gone).

**Default.** `off` (accept-all as today). `warn` logs would-deny with fingerprints; `enforce` denies. The acceptance config is `mode: enforce`, `github_users: [charliek]`.

- **Unit tests:** allowlist match across multi-key agents; GitHub-seed parse + cache + fail-closed-to-cache; `warn` vs `enforce` decisions; `_api` gated by the same list (once Phase 7 exists).
- **Integration (VZ+FC), new `test_ssh_auth.py`:** generate an off-list keypair, drive `ssh -i` → assert `enforce` denies and `warn` allows+logs; an on-list key still connects. Must run against the **dev server** (server-side change).
- **Docs:** new `reference/security.md` SSH section; `reference/configuration.md` `auth.ssh` block; `getting-started/*-quickstart.md` "locking down SSH" note.
- **Done when:** `off` = accept-all unchanged; `warn` = logs would-deny + admits; `enforce` = denies an off-list key and admits a `github.com/charliek.keys` key on both VZ+FC; GitHub fetch caches and fails closed to last-known-good when unreachable (unit); per-IP throttle active; the acceptance sub-test (off-list `ssh -i` denied under enforce) green on both backends.

### Phase 3 — Server identity hardening `[shed, shed-desktop]`

**Scope.** Close the host-key-over-plain-HTTP bootstrap: `shed server add` prints the SHA256 fingerprint and confirms (TOFU like first-connect ssh), plus a `--fingerprint SHA256:…` flag for out-of-band verification (read from the server's startup log). Desktop: point **both** SSH paths at `~/.shed/known_hosts` with `StrictHostKeyChecking=yes` — `RemoteControl.swift` uses `accept-new` and `TerminalLauncher` sets *no* host-key option at all (wider gap than the discovery doc states).

**Default.** Always-on improvement (no toggle); `--fingerprint`/`--trust-on-first-use` for non-interactive use.

- **Unit tests:** Swift argv builders assert the host-key options on both paths; CLI fingerprint formatting + `--fingerprint` mismatch rejection.
- **Integration:** manual MITM-at-add-time rejected (documented check; not in the pytest suite).
- **Docs:** `reference/cli.md` (`shed server add --fingerprint`); `reference/security.md` server-identity section.
- **Done when:** `shed server add` prints + confirms the fingerprint and `--fingerprint` rejects a mismatch; both desktop SSH paths emit `~/.shed/known_hosts` + `StrictHostKeyChecking=yes` (Swift unit asserts argv); `swift build` + desktop tests green.

### Phase 4 — HTTP bearer token auth `[shed, sdk, shed-extensions, shed-desktop]`

**Scope.** Deny-by-default auth middleware over the entire `/api` subtree (`internal/api/server.go:37-41`), covering SSE subscribe, the `/respond` POST, and list endpoints (recon leak). `shed-server token new` mints tokens; **scoped format** (`control`/`credentials`/`admin`) per D3. `token:` field on `~/.shed/config.yaml` server entries. Thread the header through: CLI HTTP client; SDK `HostClient` on **both** subscribe and respond; `shed-host-agent` (`discovery.go` reads `token`, carried on `ServerTarget`/supervisor — it has no per-server-secret mechanism today); desktop `requestData` + `createShed` (two call sites).

**Default.** `off` (no token required). Rollout: ship client token-*support* first, enable server enforcement last (or per-server opt-in) so no partial-rollout breaks brokering.

- **Unit tests:** middleware 401 on missing/bad token incl. the plugin subtree; token scope checks; both SDK bus call sites attach the header.
- **Integration (VZ+FC), new `test_http_auth.py`:** with a token set, assert 401 without header / 200 with; partial-rollout guard (server enforces, host-agent presents token, brokering still works).
- **Docs:** `reference/security.md` token section; `reference/cli.md` (`shed-server token new`); `reference/configuration.md` (`token:`); `reference/api.md` (Authorization header). shed-extensions `CLAUDE.md`/README host-agent token config.
- **Done when:** every `/api` route (incl. plugin subtree) returns 401 without a valid token and 200 with; `shed-server token new` mints a scoped token and scopes are enforced; CLI + SDK (both bus call sites) + host-agent + desktop all attach the header; brokering works end-to-end with enforcement on; the rollout-safety check (clients token-aware, enforcement off → nothing breaks; then enforcement on → still works) green on VZ+FC.

### Phase 5 — Native pinned TLS `[shed, sdk, shed-extensions, shed-desktop]`

**Scope.** Server generates a self-signed cert on first start (same lifecycle as the ED25519 host key, `cmd/shed-server/serve.go:32-37`); serves HTTPS; `serve.go:116`'s plain `ListenAndServe` becomes `ListenAndServeTLS` (or an `https_port` alongside a redirect). `shed server add` fetches the cert, shows its fingerprint alongside the SSH host-key fingerprint, and pins it (`tls_cert_fingerprint:` in the config entry). Clients verify by pin: Go via `tls.Config.VerifyPeerCertificate`, Swift via `URLSessionDelegate`, `curl` via an exported `--cacert` file. Replace every hardcoded `http://` (sdk `hostclient.go:20`, `busclient.go:15`; shed-extensions `discovery.go:75`; desktop `AppModel.swift:321`).

**Default.** `off` (plain HTTP). Designed so a later flip to on-by-default + auto-provision-at-`server add` is a one-line default change.

- **Unit tests:** cert generation/persistence/rotation; pin verification per client (good pin, wrong pin, rotated pin).
- **Integration (VZ+FC), new `test_tls.py`:** TLS handshake + pin succeeds; plain-HTTP refused when TLS required; `curl --cacert` works; **soak** the host-agent bus SSE over TLS through an idle/NAT firewall (keepalive correctness).
- **Docs:** `reference/security.md` TLS section; `reference/cli.md` (`server add` fingerprint output, `--cacert` export); the new `guides/vps-deployment.md`.
- **Done when:** server serves HTTPS with a cert persisted across restarts; `shed server add` pins the fingerprint and clients verify by pin (good/wrong/rotated covered); `curl --cacert` works and plain HTTP is refused when TLS required; every hardcoded `http://` replaced; host-agent + desktop reach the server over HTTPS; the bus SSE soak over TLS stays alive through an idle firewall; integration green on VZ+FC.

### Phase 6 — Credential-bus hardening + public-exposure preflight `[shed, sdk, shed-extensions]`

**Scope.** The sharpest edge. Enforce the **`credentials` scope** on bus subscribe/respond so the general server token cannot join `ssh-agent`/`aws-credentials`/`docker-credentials`. Fix response-injection (`/respond` authenticates no responder — matched only on `InReplyTo`/`Shed.Name`, `plugin_handlers.go:109`) and listener-squat (one-listener-per-namespace first-come, `registry.go:43`) by tying namespace registration to the credential-scoped identity. Implement the **co-located-loopback vs remote-TLS** reachability model (D2): host-agent on the same host reaches the loopback bus; remote host-agent reaches it over TLS+scope. Add a **startup preflight**: refuse to bind a non-loopback interface unless the mandatory bundle (allowlist enforce + token + TLS + bus-gated) is satisfied — the guardrail that makes "you can't half-deploy to the internet" real.

**Default.** Bus scope enforced only when tokens are enabled (Phase 4 off → bus behaves as today). Preflight only triggers on non-loopback binds.

- **Unit tests:** bus subscribe/respond rejected without `credentials` scope; response-injection from a non-owning identity rejected; preflight refuses public bind with an incomplete bundle and names the missing piece.
- **Integration (VZ+FC), new `test_cred_bus.py`:** a `control`-only token is refused on a credential namespace; a forged `/respond` is dropped; end-to-end brokering still works for the legitimate host-agent over the gated channel. Soak the long-lived bus stream (the #1 residual risk from the design review).
- **Docs:** new `reference/security.md` "Credential bus" section (the loopback/remote model, the second gate); the preflight behavior in `guides/vps-deployment.md`.
- **Done when:** bus subscribe/respond require `credentials` scope and a `control`-only token is refused; a forged `/respond` from a non-owning identity is dropped; a co-located host-agent brokers over the loopback bus and a remote host-agent brokers over TLS+scope; preflight refuses a non-loopback bind unless allowlist=enforce + token + TLS + bus-gate are all present, naming the missing piece; the full acceptance run is green on VZ+FC dev servers.

### Phase 7+ (beyond the single-user acceptance — future)

- **7. HTTP-over-SSH (optional, Go clients):** `_api` channel handler → in-process `http.Server.Serve` over a **deadline-capable** `ssh.Channel`→`net.Conn` adapter (the design review proved the naive wrapper silently disables `http.Server` timeouts/shutdown and leaks goroutines). Single-port convenience for CLI/host-agent only; keep the loopback plain listener for `curl`.
- **8. Multi-user + ownership:** `auth.users`, key→user / token→user resolution, `owner` on shed metadata, owner-or-admin authz, bus routing scoped to `(namespace, owner)`.
- **9. `shed login` (GitHub device flow):** server-issued tokens derived from GitHub identity replace static tokens (collapsing the "second secret" entirely).
- **10. Fleet / control-plane:** control-plane/worker split, SSH CA + host certs, provider abstraction (Daytona-style).

## 6. Cross-cutting testing strategy

**Both backends, every server-side phase.** VZ (Apple Silicon vfkit) and FC (Linux KVM) have different boot/network paths; an auth/bind/TLS change can pass on one and regress the other. Each phase's new pytest file runs under the parameterized `shed_server_dev` fixture against both `make test-integration-dev` (VZ, localmac) and `make test-integration-dev-fc` (FC, mini3).

| Layer | Tooling | Covers |
|---|---|---|
| Go unit | `make test` (CI on every PR) | config parse, middleware, allowlist logic, token scopes, cert/pin, preflight |
| Swift unit | shed-desktop test target (`swift test` / xcodebuild) | argv builders (host-key opts), pin delegate, token header |
| Integration VZ+FC | `tests/integration/` new files (P1–P6) | live auth/bind/TLS/bus behavior on real VMs, both backends, against the dev build |
| Soak | new harness under `tests/integration/` | long-lived bus SSE over TLS through NAT/idle-firewall (keepalive, half-dead detection) |
| Perf gate | `test_create_agent_p50` + dev-vs-release compare | Phase 1's bind-selection is the only boot-path-adjacent change; confirm no regression on both backends |
| Manual | running dev processes on this host + mini3 | exercise the real wire (curl, ssh, broker an ssh-agent sign) per Safety discipline |

**New integration test files:** `test_network_surface.py` (P1), `test_ssh_auth.py` (P2), `test_http_auth.py` (P4), `test_tls.py` (P5), `test_cred_bus.py` (P6). All parameterized `["vz","fc"]`, all skip cleanly when a backend is unreachable, all target the dev fixture. Markers added to `pyproject.toml`.

**Acceptance run (end of Phase 6).** Stand up a dev server with the full bundle enabled (`enforce` + token + TLS + bus-gated) and assert the acceptance test end-to-end on both VZ and FC: off-list key denied, on-list (GitHub-seeded) key admitted, HTTP requires token over TLS, bus rejects a `control`-only token, preflight blocks a public bind with any piece missing.

## 7. Documentation plan

Per-phase doc updates are listed above; the consolidated end-user surface:

| Doc | Change | Phase |
|---|---|---|
| **`reference/security.md`** (new page, add to `mkdocs.yml` nav) | The reference: SSH allowlist + GitHub seeding, bearer token + scopes, native pinned TLS, bind config, credential-bus model. One topic, tables for config fields. | P2–P6 |
| **`guides/vps-deployment.md`** (new page + nav) | The acceptance recipe end-to-end: provision a VPS, enable the bundle, `github_users: [charliek]`, verify only your keys get in. Copy-pasteable. | P5–P6 |
| `reference/configuration.md` | `http_bind`/`ssh_bind`/`trusted_proxy`/timeouts, `auth.ssh`, `token:`, `tls_*`. | P1–P5 |
| `reference/cli.md` | `shed server add --fingerprint`/cert pin output, `shed-server token new`. | P3–P5 |
| `reference/api.md` | Authorization header, TLS, bus/Connect internal-only note. | P1, P4 |
| `getting-started/macos-quickstart.md`, `vz-setup.md`, `fc-setup.md` | "It's open by default on your LAN; here's how to lock it down" note. | P2 |
| shed-extensions `README.md` / `CLAUDE.md` | host-agent per-server `token`/cert-pin config. | P4–P5 |
| shed-desktop docs (if present) | cert-pin + token handling, known_hosts hardening. | P3–P5 |

## 8. Execution protocol (autonomous run)

This plan is executed autonomously, phase by phase. The cadence, branch/PR strategy, and safety rules below are themselves acceptance criteria for *how* the run is conducted.

**Branches & PRs.** Three git repos (sdk is a module in shed). Implementation goes on a fresh `feat/auth-transport` branch cut from each repo's `main`:

- `shed` → `feat/auth-transport` (server + CLI + sdk module + integration tests; docs may ride here or on the existing `docs/auth-transport-discovery` branch)
- `shed-extensions` → `feat/auth-transport` (host-agent)
- `shed-desktop` → `feat/auth-transport` (Swift client)

Each phase is one (or a few tightly-scoped) commit(s) with the phase number in the subject (`feat(auth): phase 1 — network surface config`). The plan doc already lives on `docs/auth-transport-discovery` (PR #194) and is referenced, not re-committed onto the feature branches.

**Per-phase cadence (every phase):**

1. Implement the phase (server / CLI / sdk / host-agent / desktop as scoped).
2. Build + unit + lint green: `make build && make test && make lint` (shed); `go build ./... && go test ./...` (extensions); `swift build` + tests (desktop).
3. Integration on **both** backends against the **dev** servers: `make test-integration-dev` (VZ) + `make test-integration-dev-fc` (FC). The new per-phase pytest file green.
4. **Manual functional testing** on the running dev processes (curl the API, ssh with on/off-list keys, broker a real credential request) — exercise the wire, not just unit assertions.
5. `/simplify` the diff — apply the reuse/clarity cleanups.
6. `/codex:rescue` for an independent review — address findings (or record why deferred).
7. Commit the phase (descriptive, phase-scoped subject).
8. Update the task tracker; proceed to the next phase.

**Safety discipline (non-negotiable — protects the live environment):**

- **Never disrupt production.** All live testing uses the **parallel dev servers** (localmac VZ dev on `:18080/:12222`, mini3 FC dev on `:18080/:12222`). The brew/deb production `shed-server` (`:8080/:2222`) and the operator's existing sheds (`hello-claude`, `t1`, `t2`) are never restarted, deleted, or reconfigured.
- **mini2/mini3 are the production FC fleet.** Use **mini3** for the FC *dev* server only; fall back to **mini2** as a secondary FC dev target if mini3 is busy. Never touch the deb production server or its sheds beyond read-only status checks.
- **Default-off guarantees rollback.** Every phase ships its feature default-off, so a bad phase is inert until explicitly enabled — the worktree stays shippable.
- **Lockstep for the token/TLS cross-repo change (P4–P5):** land client *support* before server *enforcement*; verify brokering on the dev server with enforcement off, then on, before committing the enforcing change.

**Hosts available (verified reachable now):** this Mac (VZ dev + all Go/Swift builds + the pytest driver), `mini3` (primary FC dev), `mini2` (secondary FC dev).

**End-of-run:** open one PR per repo (`shed`, `shed-extensions`, `shed-desktop`) against `main`, cross-linking this plan and the acceptance results; then `/watch-pr` each until CI + bot review are green, fixing failures along the way.

## 9. Overall acceptance checklist (definition of done for the run)

- [ ] **P0 harness:** `make test-integration` + `make test-integration-dev` + `make test-integration-dev-fc` all green on this host.
- [ ] **Phases 1–6 each:** committed phase-scoped on the repo feature branch, `/simplify` + `/codex:rescue` applied, unit + VZ + FC integration green, manual functional pass done.
- [ ] **Acceptance test green on VZ and FC dev servers:** only `charliek` GitHub keys SSH in (enforce); HTTP requires a token over TLS; the bus rejects a `control`-only token; preflight blocks a public bind missing any bundle piece; the default-off/Tailscale path is unchanged.
- [ ] **Full end-to-end manual pass** at the end across the dev servers.
- [ ] **Docs updated:** `reference/security.md`, `guides/vps-deployment.md`, configuration/cli/api, quickstarts, extensions/desktop docs.
- [ ] **One PR per repo opened;** `/watch-pr` green on each (CI + bot review).
- [ ] **Discovery doc reconciled** (slimmed or deleted) per §11.

## 10. Risk register

| Risk | Likelihood | Mitigation |
|---|---|---|
| Cross-repo token/TLS lockstep breaks credential brokering | Med | Client support before server enforcement; dev-server verify off→on before commit; default-off |
| Swift build/test friction for shed-desktop | Low | Env confirmed (Swift 6.3.2 + xcodebuild); Phase 3 is the first desktop touch and is small — surfaces any friction early; if blocked, ship Go/server phases and flag desktop as a follow-up rather than block the run |
| FC dev-server contention on mini3 disrupts the operator | Low | Dev server only, separate ports/state; mini2 as fallback; deb prod never touched |
| TLS pin + SSE keepalive interaction over real NAT | Med | Explicit soak test in Phase 5; SSH-keepalive-equivalent on the bus stream |
| Autonomous run can't finish all 6 phases overnight | Med | Phases are independently shippable + default-off; partial progress is mergeable; task tracker records the stopping point for clean resume |
| `/codex:rescue` or `/simplify` surfaces a redesign mid-phase | Low-Med | Treat as a phase blocker: record, adjust the plan, continue with unaffected phases |

## 11. Final step — reconcile the discovery doc

Once Phases 1–6 are shipped and documented in the reference pages above, the [discovery doc](auth-and-transport-security.md) has served its purpose for everything that became real. The closing task:

1. **Fold the decided/built material out** — the current-state audit, the A/B/C/D options comparison, and §9 phasing are now captured (and corrected) here and in `reference/security.md`.
2. **Keep only the still-forward-looking rationale** — the tiered threat model and the future tiers (multi-user/ownership, `shed login`, fleet/control-plane, HTTP-over-SSH) — as a slimmed "future work & rationale" doc, **or delete it entirely** if nothing non-obvious remains beyond what `reference/security.md` and the project ROADMAP carry.
3. Decide update-vs-delete at that point based on how much genuinely forward-looking design is left; default to **slim-and-keep the rationale, delete if empty**.

Until then, the discovery doc carries a banner pointing here for the revised transport decision.
