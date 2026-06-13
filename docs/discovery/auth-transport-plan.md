# Auth & Transport: Implementation Plan

The buildable, cross-repo plan that turns the [Auth & Transport Security discovery](auth-and-transport-security.md) into shipped work. It supersedes the discovery doc's §6 transport "leaning": after an adversarial design review, the chosen primary is **native pinned self-signed TLS + a bearer token**, not HTTP-over-SSH. The discovery doc remains the rationale/threat-model reference; this doc is the work plan.

> **Decision of record (locked).** For the encrypted posture (office LAN + public VPS), the HTTP transport is **native pinned self-signed TLS** (server self-signs a cert with the same lifecycle as the SSH host key; clients pin its fingerprint at `shed server add`) plus a **deny-by-default bearer token**. No ACME, no domains, no reverse proxy required. HTTP-over-SSH stays in the toolbox as an *optional* Go-client convenience, never the load-bearing primary.

## 1. Goal & acceptance criteria

shed today treats the network as the auth boundary (LAN/Tailscale). This plan makes that boundary *optional* rather than load-bearing, so the same server can run safely in three postures:

| Posture | Wire | What it needs | Default |
|---|---|---|---|
| **Tailscale / LAN (primary today)** | WireGuard-encrypted already | Authorization only (token); nothing else changes | hardening **off** |
| **Office shared LAN, no Tailscale** | plaintext, semi-trusted insiders | Encryption + auth | opt-in now → default later |
| **Public internet-facing VPS** | hostile | Encryption + auth + SSH allowlist + bus lockdown + DoS hardening | opt-in |

**Acceptance test (the definition of done for the core feature):**

> Deploy `shed-server` to an internet-facing VPS where **only the operator's GitHub keys** (`github_users: [charliek]` → `https://github.com/charliek.keys`) can SSH in, **all HTTP is encrypted and token-authenticated**, and the **credential bus is unreachable** by any unauthenticated party — with the Tailscale path unchanged when the hardening is off.

**Secure-by-default trajectory.** Every layer ships default-off so the tailnet path is untouched and the work proves out incrementally. Once soaked (a release or two), TLS + token flip to **on-by-default with auto-provisioning at `shed server add`** — there is no UX drawback to keeping them on, which was the whole point of choosing native TLS over HTTP-over-SSH. The SSH key allowlist stays explicit-opt-in permanently (turning it on without keys configured would lock the operator out).

**Mandatory bundle.** For *genuine public exposure* these stop being independent toggles and become an ordered, all-or-nothing bundle — each one alone leaves a full-compromise path (an allowlisted SSH port still sits next to an unauthenticated credential bus, a token still travels in plaintext without TLS). The server must refuse to bind a non-loopback interface unless the whole bundle is satisfied (a startup preflight check, see Phase 6).

## 2. Target architecture (locked decisions)

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
   • shed CLU / host-agent (Go)  → HTTPS + pinned cert + token header
   • shed-desktop (Swift)        → URLSession HTTPS + pinned cert + token header
   • terminals                   → ssh(1) to <shed>@host, pinned host key
   • approval UX                 → local UNIX socket to host-agent (never network)
```

Decisions baked in (see discovery doc §6 and the design-review brief for rationale):

1. **Transport:** native pinned self-signed TLS + bearer token. *(Locked by user.)*
2. **Credential bus + Connect API:** a credential-scoped second gate beyond the general token, **plus** loopback-binding where the consuming `shed-host-agent` is co-located with the server. For a remote host-agent (the FC/VPS case, where the agent runs on the operator's laptop), the bus rides the TLS+scoped-token channel — it cannot be pure-loopback because its legitimate consumer is across the network. This is the one place the discovery doc's "always loopback" needs refining.
3. **Token format:** flat token now, but **designed with scopes** (`control`, `credentials`, `admin`) so the credential grant and per-user/admin splits land later without re-issuing tokens.
4. **SSH auth:** allowlist in `handlePublicKey`, seeded by `github_users`, `warn` → `enforce` rollout.
5. **Desktop:** stays Swift + `URLSession` (native TLS means no Rust/FFI/subprocess needed); CLI-as-backend remains a *future* consolidation, not required here.

## 3. Cross-repo map

| Repo | Language | Owns |
|---|---|---|
| **shed** (`/Users/charliek/projects/shed`) | Go | server (bind config, SSH allowlist, HTTP auth middleware, TLS, bus gating, preflight), CLI client (token+pin in config, `shed server add` fingerprint, `shed-server token new`) |
| **shed/sdk** (`/Users/charliek/projects/shed/sdk`) | Go | `HostClient`/`BusClient` — TLS pin verification + token header on **both** subscribe (GET SSE) and respond (POST) |
| **shed-extensions** (`/Users/charliek/projects/shed-extensions`) | Go | `shed-host-agent` — per-server token+pin threading through `discovery.go`/`supervisor.go`/`ServerTarget`; guest binaries unchanged (loopback inside VM) |
| **shed-desktop** (`/Users/charliek/projects/shed-desktop`) | Swift | `ShedServerClient` (HTTPS scheme + token header + `URLSessionDelegate` pin), both SSH paths → `~/.shed/known_hosts` + `StrictHostKeyChecking=yes` |

The token-and-pin threading is a **4-repo change that must land in lockstep**: if the server enforces before the host-agent presents a token, credential brokering breaks for every shed. Phases 4–6 call out the rollout ordering (server enforces *last*, after all clients can present credentials, or behind a per-server opt-in).

## 4. Test infrastructure setup (prerequisite — P0)

The pytest integration suite (`tests/integration/`, parameterized over `["vz", "fc"]`) is how every server-side phase is validated on **both** backends. It has not been run on this machine before. Verified current state of this host:

- ✅ `uv`, Go 1.25.9 (mise), `jq` present.
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

Each phase is an independently shippable PR (or small PR stack), default-off, validated on VZ **and** FC. Phases 1–6 constitute the public-VPS acceptance bundle; 7+ are beyond the single-user goal.

### Phase 1 — Network surface config + RealIP fix `[shed]`

**Scope.** Add `http_bind` / `ssh_bind` to `ServerConfig` (none exist today — both listeners hardcode `:port`, `cmd/shed-server/serve.go:106`, `internal/sshd/server.go:153`). Add an optional **internal listener** bound to loopback that carries the bus (`/api/plugins/*`) and Connect (`/api/sheds/*/connect/*`) routes, so co-located deployments keep them off the public interface; the public router omits them. Drop/guard `middleware.RealIP` (`internal/api/server.go:38`) — it trusts client `X-Forwarded-For`, poisoning any future IP-based rate-limit or audit log. Add the missing `http.Server` timeouts (`ReadHeaderTimeout`/`ReadTimeout`/`WriteTimeout`/`IdleTimeout`) and a `MaxBytesReader` on JSON bodies.

**Default.** Binds all-interfaces exactly as today when unset; RealIP behavior gated behind an explicit `trusted_proxy` config. No behavior change for existing deployments.

- **Unit tests:** config parse + listener-address selection; route-to-listener assignment (bus/Connect only on internal listener); `RemoteAddr` is the real TCP peer when no trusted proxy.
- **Integration (VZ+FC), new `test_network_surface.py`:** with the internal listener on loopback, assert `/api/plugins/listeners/...` and `/api/sheds/{n}/connect/{p}` are **unreachable** on the public bind and reachable on loopback; existing lifecycle tests still pass unchanged.
- **Docs:** `reference/configuration.md` (`http_bind`/`ssh_bind`/`trusted_proxy`/timeouts); note in `reference/api.md` that the bus/Connect routes may be internal-only.

### Phase 2 — SSH key allowlist + GitHub seeding `[shed]` *(acceptance-critical)*

**Scope.** Replace `return true` in `handlePublicKey` (`internal/sshd/server.go:174-183`) with an allowlist check. Config `auth.ssh`: inline keys, `authorized_keys_file`, and `github_users` (fetch `https://github.com/<u>.keys` at startup + on a refresh interval, cache to `{state_dir}/github_keys/<u>`, **fail closed to last-known-good** if GitHub is unreachable). Modes `off | warn | enforce`. Add per-IP auth throttling / `MaxAuthTries` via the gliderlabs `ServerConfigCallback` (the SSH port becomes a brute-force *target* only once accept-all is gone).

**Default.** `off` (accept-all as today). `warn` logs would-deny with fingerprints; `enforce` denies. The acceptance config is `mode: enforce`, `github_users: [charliek]`.

- **Unit tests:** allowlist match across multi-key agents; GitHub-seed parse + cache + fail-closed-to-cache; `warn` vs `enforce` decisions; `_api` gated by the same list (once Phase 7 exists).
- **Integration (VZ+FC), new `test_ssh_auth.py`:** generate an off-list keypair, drive `ssh -i` → assert `enforce` denies and `warn` allows+logs; an on-list key still connects. Must run against the **dev server** (server-side change).
- **Docs:** new `reference/security.md` SSH section; `reference/configuration.md` `auth.ssh` block; `getting-started/*-quickstart.md` "locking down SSH" note.

### Phase 3 — Server identity hardening `[shed, shed-desktop]`

**Scope.** Close the host-key-over-plain-HTTP bootstrap: `shed server add` prints the SHA256 fingerprint and confirms (TOFU like first-connect ssh), plus a `--fingerprint SHA256:…` flag for out-of-band verification (read from the server's startup log). Desktop: point **both** SSH paths at `~/.shed/known_hosts` with `StrictHostKeyChecking=yes` — `RemoteControl.swift` uses `accept-new` and `TerminalLauncher` sets *no* host-key option at all (wider gap than the discovery doc states).

**Default.** Always-on improvement (no toggle); `--fingerprint`/`--trust-on-first-use` for non-interactive use.

- **Unit tests:** Swift argv builders assert the host-key options on both paths; CLI fingerprint formatting + `--fingerprint` mismatch rejection.
- **Integration:** manual MITM-at-add-time rejected (documented check; not in the pytest suite).
- **Docs:** `reference/cli.md` (`shed server add --fingerprint`); `reference/security.md` server-identity section.

### Phase 4 — HTTP bearer token auth `[shed, sdk, shed-extensions, shed-desktop]`

**Scope.** Deny-by-default auth middleware over the entire `/api` subtree (`internal/api/server.go:37-41`), covering SSE subscribe, the `/respond` POST, and list endpoints (recon leak). `shed-server token new` mints tokens; **scoped format** (`control`/`credentials`/`admin`) per Decision 3. `token:` field on `~/.shed/config.yaml` server entries. Thread the header through: CLI HTTP client; SDK `HostClient` on **both** subscribe and respond; `shed-host-agent` (`discovery.go` reads `token`, carried on `ServerTarget`/supervisor — it has no per-server-secret mechanism today); desktop `requestData` + `createShed` (two call sites).

**Default.** `off` (no token required). Rollout: ship client token-*support* first, enable server enforcement last (or per-server opt-in) so no partial-rollout breaks brokering.

- **Unit tests:** middleware 401 on missing/bad token incl. the plugin subtree; token scope checks; both SDK bus call sites attach the header.
- **Integration (VZ+FC), new `test_http_auth.py`:** with a token set, assert 401 without header / 200 with; partial-rollout guard (server enforces, host-agent presents token, brokering still works).
- **Docs:** `reference/security.md` token section; `reference/cli.md` (`shed-server token new`); `reference/configuration.md` (`token:`); `reference/api.md` (Authorization header). shed-extensions `CLAUDE.md`/README host-agent token config.

### Phase 5 — Native pinned TLS `[shed, sdk, shed-extensions, shed-desktop]`

**Scope.** Server generates a self-signed cert on first start (same lifecycle as the ED25519 host key, `cmd/shed-server/serve.go:32-37`); serves HTTPS; `serve.go:116`'s plain `ListenAndServe` becomes `ListenAndServeTLS` (or an `https_port` alongside a redirect). `shed server add` fetches the cert, shows its fingerprint alongside the SSH host-key fingerprint, and pins it (`tls_cert_fingerprint:` in the config entry). Clients verify by pin: Go via `tls.Config.VerifyPeerCertificate`, Swift via `URLSessionDelegate`, `curl` via an exported `--cacert` file. Replace every hardcoded `http://` (sdk `hostclient.go:20`, `busclient.go:15`; shed-extensions `discovery.go:75`; desktop `AppModel.swift:321`).

**Default.** `off` (plain HTTP). Designed so a later flip to on-by-default + auto-provision-at-`server add` is a one-line default change.

- **Unit tests:** cert generation/persistence/rotation; pin verification per client (good pin, wrong pin, rotated pin).
- **Integration (VZ+FC), new `test_tls.py`:** TLS handshake + pin succeeds; plain-HTTP refused when TLS required; `curl --cacert` works; **soak** the host-agent bus SSE over TLS through an idle/NAT firewall (keepalive correctness).
- **Docs:** `reference/security.md` TLS section; `reference/cli.md` (`server add` fingerprint output, `--cacert` export); the new `guides/vps-deployment.md`.

### Phase 6 — Credential-bus hardening + public-exposure preflight `[shed, sdk, shed-extensions]`

**Scope.** The sharpest edge. Enforce the **`credentials` scope** on bus subscribe/respond so the general server token cannot join `ssh-agent`/`aws-credentials`/`docker-credentials`. Fix response-injection (`/respond` authenticates no responder — matched only on `InReplyTo`/`Shed.Name`, `plugin_handlers.go:109`) and listener-squat (one-listener-per-namespace first-come, `registry.go:43`) by tying namespace registration to the credential-scoped identity. Implement the **co-located-loopback vs remote-TLS** reachability model (Decision 2): host-agent on the same host reaches the loopback bus; remote host-agent reaches it over TLS+scope. Add a **startup preflight**: refuse to bind a non-loopback interface unless the mandatory bundle (allowlist enforce + token + TLS + bus-gated) is satisfied — the guardrail that makes "you can't half-deploy to the internet" real.

**Default.** Bus scope enforced only when tokens are enabled (Phase 4 off → bus behaves as today). Preflight only triggers on non-loopback binds.

- **Unit tests:** bus subscribe/respond rejected without `credentials` scope; response-injection from a non-owning identity rejected; preflight refuses public bind with an incomplete bundle and names the missing piece.
- **Integration (VZ+FC), new `test_cred_bus.py`:** a `control`-only token is refused on a credential namespace; a forged `/respond` is dropped; end-to-end brokering still works for the legitimate host-agent over the gated channel. Soak the long-lived bus stream (the #1 residual risk from the design review).
- **Docs:** new `reference/security.md` "Credential bus" section (the loopback/remote model, the second gate); the preflight behavior in `guides/vps-deployment.md`.

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
| Swift unit | shed-desktop test target | argv builders (host-key opts), pin delegate, token header |
| Integration VZ+FC | `tests/integration/` new files (P1–P6) | live auth/bind/TLS/bus behavior on real VMs, both backends, against the dev build |
| Soak | new harness under `tests/integration/` | long-lived bus SSE over TLS through NAT/idle-firewall (keepalive, half-dead detection) |
| Perf gate | `test_create_agent_p50` + dev-vs-release compare | Phase 1's bind-selection is the only boot-path-adjacent change; confirm no regression on both backends |

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

## 8. Final step — reconcile the discovery doc

Once Phases 1–6 are shipped and documented in the reference pages above, the [discovery doc](auth-and-transport-security.md) has served its purpose for everything that became real. The closing task:

1. **Fold the decided/built material out** — the current-state audit, the A/B/C/D options comparison, and §9 phasing are now captured (and corrected) here and in `reference/security.md`.
2. **Keep only the still-forward-looking rationale** — the tiered threat model and the future tiers (multi-user/ownership, `shed login`, fleet/control-plane, HTTP-over-SSH) — as a slimmed "future work & rationale" doc, **or delete it entirely** if nothing non-obvious remains beyond what `reference/security.md` and the project ROADMAP carry.
3. Decide update-vs-delete at that point based on how much genuinely forward-looking design is left; default to **slim-and-keep the rationale, delete if empty**.

Until then, the discovery doc carries a banner pointing here for the revised transport decision.
