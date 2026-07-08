# Auth & Transport Security: Optional, Iterative Hardening

Discovery document for adding authentication and encryption to shed-server — SSH client auth, HTTP auth, transport encryption, and server identity — as **optional layers** that default off, scale from today's single-user LAN/Tailscale deployment up to a multi-user org and (eventually) a cloud-proxied fleet, and never break the ecosystem (raw `ssh`, IDE remoting, shed-desktop, shed-extensions).

> **Status.** Discovery only — nothing here is decided or built. The current
> trust model (network perimeter = auth boundary) remains the supported
> deployment and the default after any of this ships.
>
> **Revised by the implementation plan.** After an adversarial design review,
> the §6 transport "leaning" (HTTP-over-SSH as primary) is **superseded**: the
> chosen primary is native pinned self-signed TLS + a bearer token, with
> HTTP-over-SSH demoted to an optional Go-client convenience, and public-internet
> deployment promoted from "directional only" to a first-class target. See
> [Auth & Transport Plan](auth-transport-plan.md) for the decisions of record and
> the buildable, cross-repo phasing. This doc will be reconciled (slimmed to
> forward-looking rationale, or deleted) as the final plan step.

## 1. Goals and Constraints

**Goals, in priority order:**

1. **Optional and incremental.** Every layer is off by default and independently adoptable. A LAN/Tailscale user who configures nothing sees zero change.
2. **SSH client authentication** — server verifies keys against a known set, GitHub-style (identity comes from the key, not the username). Key set seedable from GitHub.
3. **HTTP authentication and encryption** — without requiring users to provision real TLS certificates.
4. **Server identity** — clients can verify they're talking to the right shed-server.
5. **An architecture that grows** into multi-user (org installs users, sheds get owners) and eventually a control plane proxying sheds onto elastically provisioned cloud hosts (self-managed or a provider like Daytona) — *designed for, not built now*.

**Hard constraints:**

- **Standard SSH must keep working.** Zed Remote-SSH, VS Code Remote-SSH, JetBrains Gateway, raw `ssh`, `rsync`, `sftp` all speak vanilla public-key auth. Whatever we do on the SSH side must be expressible as ordinary `authorized_keys`-style verification — no custom auth methods, no forced certificates in the near term.
- **shed-extensions keeps working** — both the guest binaries (which never touch the server's network ports; they publish to shed-agent on loopback inside the VM) and `shed-host-agent` (which is a first-class HTTP API client and must be able to adopt each new layer).
- **shed-desktop keeps working** — Swift/URLSession HTTP client plus `ssh(1)` subprocesses. Both codebases can change over time, but each phase must leave them functional.
- **Tailscale stays the easy path.** Tailscale already encrypts everything on the wire (WireGuard), so for tailnet users the *encryption* layers below are defense-in-depth, not a prerequisite. The design must not force cert management on them.

## 2. Current State (verified against source)

### 2.1 SSH

| Aspect | Today | Reference |
|---|---|---|
| Client auth | `PublicKeyHandler` exists but **accepts every key**, logging the SHA256 fingerprint (`// For MVP, accept all keys`) | `internal/sshd/server.go:173-183` |
| Identity | SSH username **is the shed name**; no user concept | `internal/sshd/session.go:46` |
| Reserved username | `_api` is reserved "for API access" and rejected for sessions — an unused placeholder | `internal/sshd/session.go:19-20,38` |
| Host key | ED25519, generated on first start, persisted (`/etc/shed/host_key` or `~/.shed/host_key`), fingerprint logged | `cmd/shed-server/serve.go:32-37`, `internal/sshd/server.go:124-129` |
| Port forwarding | `direct-tcpip` allowed, restricted to localhost destinations, dialed **inside the shed VM** via `DialService` | `internal/sshd/server.go:195-231` |
| Library | `gliderlabs/ssh` over `golang.org/x/crypto/ssh` | `internal/sshd/server.go:17-18` |

### 2.2 HTTP

| Aspect | Today | Reference |
|---|---|---|
| Auth | **None.** Chi middleware stack is RequestID/RealIP/Logger/Recoverer only | `internal/api/server.go:37-41` |
| TLS | **None.** Plain `ListenAndServe`, binds all interfaces | `cmd/shed-server/serve.go:116` |
| Surface | Full control plane: shed create/delete/start/stop, image management, system prune, sessions | `internal/api/server.go:44-120` |
| Connect API | `GET /api/sheds/{name}/connect/{port}` hijacks to a raw TCP tunnel into the VM (`Upgrade: shed-tcp`). `shed tunnels` rides this, **not** SSH | `internal/api/connect.go` |
| Plugin bus | SSE subscribe + respond endpoints for credential brokering (ssh-agent, AWS, Docker namespaces) — unauthenticated | `internal/api/plugin_handlers.go` |
| Host key distribution | `GET /api/ssh-host-key` serves the SSH host public key over plain HTTP | `internal/api/server.go` (route), used by `cmd/shed/server.go:86-89` |

### 2.3 Clients

| Client | Transport | Auth posture today |
|---|---|---|
| `shed` CLI | HTTP to `host:8080`; SSH by exec-ing system `ssh(1)` | Pins host keys in `~/.shed/known_hosts` with `StrictHostKeyChecking=yes` — but the pin is **bootstrapped over plain HTTP** at `shed server add` (`cmd/shed/server.go:86-114`) |
| `shed tunnels` | Connect API (plain HTTP hijack) | None |
| shed-desktop | URLSession with **hardcoded `http://`** (`AppModel.swift:321`); `ssh(1)` subprocess with `StrictHostKeyChecking=accept-new` and agent-default keys (`RemoteControl.swift:205-216`) | None; weaker host-key posture than the CLI (accept-new, default known_hosts) |
| shed-host-agent | SDK `HostClient`, **hardcoded `http://`** (`sdk/hostclient.go:20`, `shed-extensions cmd/shed-host-agent/discovery.go:75`); discovers servers from `~/.shed/config.yaml` | None |
| Guest binaries (shed-extensions) | Loopback HTTP to shed-agent (`127.0.0.1:498`) → vsock | **Unaffected by everything in this doc** — never crosses the host network |
| IDEs / raw `ssh` / `rsync` | Vanilla SSH | Whatever keys the user's agent offers (all currently accepted) |

### 2.4 What this means concretely

On a hostile network, today, anyone who can reach the two ports can: SSH into any shed with any key, create/destroy sheds and images over HTTP, open raw TCP tunnels into VMs via the Connect API, and interact with the **credential-brokering plugin bus** (subscribe to a namespace's SSE stream or post responses) — the last being the sharpest edge, since shed-host-agent answers ssh-agent signing and AWS/Docker credential requests over that bus. The mitigations are exactly the intended ones: LAN you trust, or a tailnet. This doc is about making that boundary optional rather than load-bearing.

Worth noting what's already in good shape: the server has a persistent host key, the CLI already pins it and enforces `StrictHostKeyChecking=yes`, the `PublicKeyHandler` callback is exactly where an allowlist check goes, and `_api` is already reserved. The skeleton anticipated this work.

## 3. Threat Model by Deployment Tier

| Tier | Deployment | Adversary | What's needed |
|---|---|---|---|
| 0 | Single user, home LAN or tailnet (today) | Nobody on the network is hostile | Nothing — current model is fine. Tailnet traffic is already encrypted by WireGuard |
| 1 | Single user, shared/office/untrusted LAN | Passive sniffing; active port scanning | SSH key allowlist; HTTP auth; transport encryption; verified server identity |
| 2 | Small org, multiple users, shared servers | Honest-but-curious colleagues; compromised laptop | Everything in tier 1 + per-user identity, shed ownership, authz on API routes |
| 3 | Org with elastic cloud hosts behind a control plane | Internet-exposed endpoints | Tier 2 + centralized identity (OAuth/IdP), short-lived credentials, host attestation, audit |

The phasing in §9 maps onto these tiers. Nothing for tier 3 gets built now, but §4.4 and §8 explain which tier-1/2 choices keep it reachable.

## 4. SSH Client Authentication

### 4.1 The GitHub model: identity from the key, not the username

The SSH username is already spoken for — it's the shed name. That's the same shape as GitHub, where the username is always `git` and *the offered key* identifies the account. We keep that: the username selects the shed, the key authenticates (and later, identifies) the user. No client-side change of any kind — every existing tool already offers the user's keys.

Enforcement is a single change at the existing callback (`internal/sshd/server.go:handlePublicKey`): instead of `return true`, check the offered key against the configured allowlist. The gliderlabs callback is invoked once per offered key, so multi-key agents work as usual.

### 4.2 Key sources

```yaml
# shed-server config — sketch
auth:
  ssh:
    mode: enforce            # off (default) | warn | enforce
    authorized_keys:         # inline, OpenSSH format
      - ssh-ed25519 AAAA... charlie@laptop
    authorized_keys_file: /etc/shed/authorized_keys   # optional, same format
    github_users:            # seed from GitHub
      - charliek
    github_refresh: 1h       # re-fetch cadence; 0 = fetch once at startup
```

- **Inline keys / file** — the baseline. OpenSSH `authorized_keys` format so users can copy lines from anywhere.
- **GitHub seeding** — `https://github.com/<user>.keys` returns the account's public keys as plain text, no token or API quota concerns (the REST endpoint `GET /users/{u}/keys` is an alternative if we later want key IDs/titles). The server fetches at startup and on a refresh interval. This is the "the server only allows keys GitHub knows" property with ~30 lines of code.
- **Failure mode: fail closed to last-known-good.** Cache fetched keys on disk (`{state_dir}/github_keys/<user>`); if GitHub is unreachable, keep enforcing the cached set rather than locking everyone out or (worse) failing open. Log loudly when the cache is stale.
- **`warn` mode** is the migration path: log `would-deny` with the fingerprint, accept anyway. Run it for a few days, confirm the only fingerprints in the log are yours, flip to `enforce`. This matters because the operator's first enforcement mistake otherwise locks them out of their own sheds.

A break-glass note for docs: enforcement never affects local console access to the server host itself; recovering from a bad key config is "edit the config file on the server, restart."

### 4.3 Compatibility check

| Client | Impact of `enforce` |
|---|---|
| `shed` CLI / `shed exec` / `shed console` | None, if the user's key is listed — execs `ssh(1)` which offers agent + default keys |
| shed-desktop | None — `BatchMode=yes` + agent keys (`RemoteControl.swift:205`); works iff a listed key is in the agent. Needs a good error surface when auth fails (BatchMode means no prompt, just failure) |
| VS Code / Zed / JetBrains / rsync / sftp | None — vanilla pubkey auth |
| shed-host-agent | None — it doesn't speak SSH to the server |

### 4.4 Later: SSH user certificates

For tier 2/3, key lists stop scaling (every key on every server) and the standard answer is an **SSH CA**: the server trusts one CA public key; users present short-lived certificates signed by it (minted by `shed login`, see §7). `x/crypto/ssh` supports certificate checking natively (`ssh.CertChecker`), and cert auth degrades gracefully — a server can trust *both* a CA and a static allowlist. Not built now; mentioned because it's the reason §4.2's config block should be a list of *trust sources* rather than hardwired to literal keys.

## 5. Server Identity (host key verification)

Mostly already solved — the gaps are bootstrap and consistency:

1. **Bootstrap over plain HTTP.** `shed server add` fetches the host key from `/api/ssh-host-key` over unauthenticated, unencrypted HTTP and pins it. An active MITM at add-time poisons the pin forever after. Fix is cheap and worthwhile regardless of everything else in this doc: **print the SHA256 fingerprint at `shed server add` and ask for confirmation** (mirroring first-connect `ssh`), plus a `--fingerprint SHA256:...` flag so the expected value can be supplied out-of-band (read from the server's startup log, which already prints it). Non-interactive contexts get `--fingerprint` or `--trust-on-first-use`.
2. **Desktop divergence.** shed-desktop uses `accept-new` against the user's default known_hosts — it should point at `~/.shed/known_hosts` (which `shed server add` already populates) and use `StrictHostKeyChecking=yes`, matching the CLI. Small change in `RemoteControl.sshArgv()` and `TerminalLauncher`.
3. **HTTP server identity** is a different problem and is owned by whichever transport option wins in §6 — pinned TLS gives it directly; HTTP-over-SSH inherits it from the host key pin.
4. **Later (tier 3):** host certificates signed by a fleet CA remove TOFU entirely — clients trust the CA once and any newly provisioned worker host verifies immediately. Same CA infrastructure as §4.4. (DNS-based SSHFP is not realistic — requires DNSSEC.)

## 6. HTTP: Authentication and Encryption

This is the genuinely open design question. SSH already has a credential story; HTTP has none, and the HTTP surface is large (full control plane + Connect API tunnels + the credential bus). Four candidate approaches, not all mutually exclusive.

### Option A — Bearer tokens over plain HTTP (auth only)

Server config defines one or more static tokens (later: server-issued, per-user); clients send `Authorization: Bearer <token>`. One chi middleware; clients each add one header.

```yaml
auth:
  http:
    mode: token              # off (default) | token
    tokens:
      - name: charlie
        token: shed_2f7c...  # generated by `shed-server token new`
```

- **Pro:** smallest possible change in every codebase. CLI: one header on its HTTP client. SDK `HostClient`: a `WithToken()` option (host-agent picks it up via its config + the `~/.shed/config.yaml` server entry growing an optional `token:` field, which desktop also reads). Desktop: one line in `ShedServerClient.requestData()` (`ShedServerClient.swift:159-185`). SSE and the Connect API hijack both pass headers before upgrade, so they're covered for free.
- **Pro:** on a tailnet this is arguably *complete* — WireGuard already encrypts the wire, so the missing piece there is authorization, not confidentiality.
- **Con:** on a bare LAN the token is sniffable plaintext. A is honest about being an authorization layer, not an encryption layer.
- **Con:** a second secret to manage alongside SSH keys (mitigated later by §7, where tokens become server-issued and derived from a real login).

### Option B — HTTP over SSH (one identity system, encryption for free)

The idea you sketched: keep the API as HTTP, but carry it over the already-authenticated, already-encrypted, already-pinned SSH connection. The `_api` reserved username is the natural anchor, and Go makes the server side almost free:

- **Server:** accept SSH connections for user `_api` (subject to the same §4 key allowlist). Instead of a shell session, the client requests either (a) `direct-tcpip` to a virtual address that the sshd routes to the API in-process, or (b) a named subsystem (`shed-api`). Either way the sshd wraps the SSH channels in a `net.Listener` and hands it to the existing chi router via `http.Server.Serve()` — no loopback TCP hop, and `ConnContext` can stamp each request with the authenticated key/user, which is exactly the identity hook §7 needs.
- **Lockdown mode:** with this in place, a config knob flips the plain-HTTP listener to loopback-only (`http_bind: 127.0.0.1`). The server then exposes a single authenticated port (2222) to the network. This is the cleanest end-state story of any option.
- **Clients:**
    - `shed` CLI and `shed-host-agent` are Go — `x/crypto/ssh` + a custom `http.Transport.DialContext` that opens a channel per connection. Moderate but well-trodden code; connection caching/reconnect is the real work.
    - shed-desktop has no Swift SSH library and shells out today. Workable pattern: spawn a long-lived `ssh -N -L <localport>:_api:80 _api@host` subprocess and point URLSession at `127.0.0.1:<localport>`. Functional, but now the desktop owns subprocess lifecycle, port collision, and reconnect logic for its primary data path — meaningfully more fragile than adding a header.
    - `curl`-ability and quick debugging degrade unless the loopback plain listener stays (it should).
- **Pro:** no second credential, no certificates ever, encryption + server identity + client identity all inherited from SSH machinery we already run and pin. The per-request identity stamp falls out naturally.
- **Con:** every non-Go client pays an SSH-plumbing tax; SSE and hijacked tunnel streams over SSH channels need real soak testing (they should work — it's all just bytes — but long-lived streams over forwarded channels is where reconnect bugs live).

### Option C — TLS with a pinned, server-generated certificate

Apply the known_hosts model to HTTPS: shed-server generates a long-lived self-signed cert at first start (same lifecycle as the SSH host key); `shed server add` fetches it, shows its fingerprint alongside the SSH one, and pins it in `~/.shed/config.yaml` (`tls_cert_fingerprint:` or the PEM itself). Clients verify by pin, not by CA — Go via `tls.Config.VerifyPeerCertificate`, Swift via a `URLSessionDelegate` challenge handler, `curl` via an exported `--cacert` file.

- **Pro:** clients stay plain HTTP clients — every language speaks HTTPS, nobody manages SSH plumbing. Auth still comes from Option A tokens riding inside the encrypted channel. This is the Docker-daemon-TLS shape, minus the client-cert ceremony, minus any ACME/Let's Encrypt involvement — satisfying the "no real certificates" requirement.
- **Pro:** a future web UI or third-party tooling gets transport security with zero exotic code.
- **Con:** two pinned identities per server (SSH host key + TLS cert) unless we cross-sign or derive one from the other; pin distribution/rotation is ours to design (re-running `shed server add` is the rotation story, which is fine at tier 1 but clunky at tier 3 without a CA).
- **Con:** Swift pinning code and the SDK's TLS config are real (if modest) work in both ecosystem repos, and `https://` plumbing must reach every hardcoded `http://` site found in §2.3.

### Option D — Tailscale-native identity (opportunistic, complementary)

When the server is reached over the tailnet, Tailscale's LocalAPI `whois` can map an incoming connection's source address to a tailnet user — free per-request identity with zero keys, zero tokens, zero certs, and the wire is already encrypted. A `tailscale_identity: true` knob could satisfy tiers 0–1 for tailnet users entirely on its own. It does nothing for bare-LAN or non-Tailscale deployments, so it's a complement to A–C, not a substitute. Listed because it's very high leverage for the project's actual primary userbase.

### Comparison

| | A: tokens | B: over SSH | C: pinned TLS | D: tailscale whois |
|---|---|---|---|---|
| Client auth | ✓ | ✓ (SSH key) | needs A | ✓ (tailnet user) |
| Encryption on bare LAN | ✗ | ✓ | ✓ | n/a |
| Server identity | ✗ | ✓ (host key pin) | ✓ (cert pin) | ✓ (tailnet) |
| New secret type | token | none | cert pin | none |
| CLI / host-agent (Go) effort | trivial | moderate | small | trivial |
| Desktop (Swift) effort | trivial | high (subprocess mgmt) | moderate (pinning delegate) | none |
| curl / future web UI | ✓ | ✗ (needs forward) | ✓ | ✓ |
| Single-identity story (SSH keys for everything) | ✗ | ✓ | ✗ | ✗ |

### Leaning

**Ship A first** — it's prerequisite-free, every client adopts it in a line or two, and combined with Tailscale (D's deployment, even without D's code) it closes the realistic tier-0/1 gap immediately.

**For encryption, B and C are both viable and the decision can wait** until A is digested. The honest trade: B is architecturally the most elegant (one identity system, one pinned key, one exposed port, per-request identity for free — and `_api` was clearly reserved for exactly this) but taxes the Swift client hardest; C is boring in the best way for clients but adds a second pinned identity and pin-rotation surface. A reasonable sequencing is to build B's server side (it's small in Go, and the `_api` + in-process listener design is clean) and let the CLI and host-agent use it, while desktop continues on token-over-plain-HTTP until either it grows the forward-subprocess plumbing or C lands for it. Encryption layers compose — nothing about B precludes adding C later if a web UI demands it.

## 7. Identity and the User Model (tier 2)

Today there is implicitly one user. The evolution, kept deliberately boring:

1. **Users in config.** `auth.users: [{name, github, keys, tokens}]` — the §4.2 key sources and §6 tokens move under a user. The `PublicKeyHandler` resolves key→user; the HTTP middleware resolves token→user. Single-user installs just have one entry (or keep the flat config, which is sugar for one user).
2. **Sheds get an owner.** New metadata field, stamped at create from the authenticated identity. Authorization is owner-or-admin on shed-scoped routes and SSH sessions (`handlePublicKey` knows the user *and* the target shed name — deny cross-user shed SSH at the same callback). List endpoints filter by owner.
3. **`shed login` with GitHub device flow.** Rather than running an OAuth provider, the server *consumes* GitHub identity: `shed login` runs the device flow, the server verifies the resulting identity, matches it against a configured user's `github:` field, and mints a **server-local token** (the same token type as §6 Option A, now issued instead of static). This collapses the "second secret" objection to tokens — the durable secret becomes your GitHub login, exactly like `gh auth login`. Short-lived SSH certificates (§4.4) can be minted from the same flow when that tier arrives.
4. **Shed naming** stays globally unique per server initially (the SSH username = shed name contract depends on it); per-owner namespacing is an open question (§10).

Each step is additive and optional; a server with `auth.mode: off` never sees any of it.

## 8. Fleet / Cloud Tier (directional only)

The long-term picture — a control plane that provisions sheds onto elastically scaled hosts (self-managed, or a provider like Daytona) — is explicitly not being designed here. What matters now is that nothing above forecloses it, and the choices that age best:

- **Control plane / worker split.** Identity, users, tokens, and policy live in a control plane; worker hosts run shed-server in an agent mode that trusts the control plane (mTLS or a join token). Today's shed-server is simply both halves in one binary — the §7 user model is what makes them separable later.
- **SSH CA over key sync.** With many ephemeral workers, copying authorized_keys around loses; the control plane becomes the §4.4 CA, signs short-lived user certs at `shed login`, and workers trust the CA key baked in at provision time. Host certs from the same CA kill TOFU for clients (§5.4).
- **SSH reachability** to ephemeral workers is either `ProxyJump` through the control plane or the control plane proxying SSH in-process. Both are standard; the client-visible contract (`ssh <shed>@<entry-point>`) doesn't change, which is the property to protect.
- **Provider abstraction.** Whether workers are static minis, autoscaled cloud VMs, or a Daytona-style API is invisible above the Backend interface boundary, provided identity was never coupled to host-local state — which is the real architectural requirement this doc's choices need to respect.

## 9. Proposed Phasing

Each phase ships independently, defaults off, and is useful on its own.

| Phase | Scope | Touches | Size |
|---|---|---|---|
| 0 | Document the trust model in the deployment docs; add `http_bind` / `ssh_bind` address config so perimeter-minded users can already restrict interfaces (e.g. bind to the tailnet IP) | shed | XS |
| 1 | **SSH key allowlist**: `auth.ssh` config (inline keys, file, `github_users` + cached refresh), `warn`→`enforce` modes, wired into `handlePublicKey` | shed only — zero client changes | S |
| 2 | **Server-identity hardening**: fingerprint confirmation + `--fingerprint` on `shed server add`; desktop moves to `~/.shed/known_hosts` + `StrictHostKeyChecking=yes` | shed, shed-desktop | S |
| 3 | **HTTP bearer tokens** (Option A): middleware, `shed-server token new`, `token:` field in `~/.shed/config.yaml` server entries; header support in CLI, SDK (`HostClient` option → host-agent), desktop (`requestData`) | shed, sdk, shed-extensions, shed-desktop | M |
| 4 | **Encryption decision + build** (Option B vs C, §6 “Leaning”): spike B’s `_api` in-process listener server-side; decide desktop’s path (forward subprocess vs pinned TLS) with real code in hand | shed first; clients follow | M–L |
| 5 | **Users + ownership**: `auth.users`, key→user and token→user resolution, `owner` on shed metadata, owner-or-admin authz on HTTP routes and SSH sessions | shed (+ clients only for display) | M |
| 6 | **`shed login`** via GitHub device flow; server-issued tokens replace static ones (static remain supported) | shed, CLI; others keep using tokens | M |
| 7 | Fleet tier (control plane, SSH CA, providers) | everything | future |

Phases 1–3 are each small, independently shippable, and collectively take a hostile-LAN deployment from "fully open" to "authenticated control plane + authenticated SSH + verified server identity" with the only remaining gap being bare-LAN eavesdropping — which phase 4 closes and Tailscale users never had.

## 10. Open Questions

1. **Connect API + bus authorization granularity.** Once tokens exist, is bearer-token-per-server enough for the credential bus, or do the plugin namespaces (ssh-agent signing!) deserve a separate, stronger gate (e.g. only specific tokens may subscribe/respond on credential namespaces)? The host-agent approval UX in shed-desktop already gates the *operations*; this question is about who may even join the bus.
2. **Option B stream soak.** SSE and hijacked `shed-tcp` tunnels over SSH channels — any real-world issues with long-lived streams, keepalives, and reconnects that don't show up in a quick spike?
3. **Token storage on clients.** `~/.shed/config.yaml` is the obvious home (and desktop/host-agent already read it), but it's plaintext on disk. Acceptable at this tier (same as `~/.kube/config`, `~/.docker/config.json`), or do we want macOS Keychain integration in desktop from day one?
4. **GitHub key refresh semantics.** On key *removal* from GitHub: how fast must revocation propagate (refresh interval vs a `shed-server auth refresh` admin command), and do cached keys need a max-staleness after which the server warns persistently?
5. **Per-owner shed namespacing** (tier 2): keep global shed names per server, or scope by owner — and if scoped, what does the SSH username become (`owner--shed`? cert principal?)? Affects the username=shed-name contract that IDE configs depend on.
6. **Should phase 1 enforce on `_api` too?** (Yes, presumably — same allowlist — but it means B's API access is gated by SSH keys, so the CLI needs a key even for pure-HTTP-over-SSH workflows. Fine for humans; worth a thought for headless automation.)
7. **Multi-server token UX.** `shed-host-agent` discovers *all* servers from `~/.shed/config.yaml` — with per-server tokens, its config grows per-server credentials. Does the SDK grow a shared credential-resolution helper so CLI/desktop/host-agent stay consistent?
