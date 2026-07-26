# Security

shed-server has three postures, selected by a single switch:

```yaml
auth:
  mode: open      # open (default) | token | mtls
```

!!! note "`secure` is a deprecated alias"
    The mode value `secure` is still accepted and normalized to `token` at load
    time, with a one-time stderr deprecation warning. New configs should write
    `token`.

- **`open`** (the default) serves plain HTTP only — no HTTP token, no TLS — and
  binds loopback by default, so out of the box it is **local-development-only**.
  It is also fine on a private network (a Tailscale tailnet or trusted LAN) where
  the network is the security boundary, but exposing it there is explicit: set a
  routable `bind_address` *and* `allow_insecure_exposure: true`. The SSH allowlist
  is the one independently-tunable layer here (`auth.ssh.mode: off|warn`, where
  `warn` stages an allowlist before you commit to enforcing it).
- **`token`** is for an internet-facing deployment. It derives the whole
  hardening bundle (SSH allowlist enforced + HTTP tokens enforced + TLS-only,
  with no plain-HTTP listener) and **refuses to start** without an SSH key
  source, so a server is never half-hardened on a public address. Two invariants
  hold: **tokens ⟺ TLS ⟺ token** and **https ⟺ token**.
- **`mtls`** replaces the bearer token with a client certificate bound to a
  private key that never leaves the client's device. It inherits every `token`
  invariant (SSH enforce, key source at preflight, TLS-only, `https_port`
  default) and is the **recommended posture for anything internet-facing** — see
  [mTLS mode](#mtls-mode) below.

| `auth.mode` | SSH allowlist | HTTP credential | Plain-HTTP listener | TLS |
|-------------|---------------|-------------|---------------------|-----|
| `open` | as configured (`auth.ssh`: `off`/`warn`) | none | served (bound to `bind_address`) | none (`https_port` requires `token`/`mtls`) |
| `token` | **forced `enforce`** (needs a key source) | bearer token, minted over SSH | **none — TLS-only** | **on** (`https_port` defaults to 8443) |
| `mtls` | **forced `enforce`** (needs a key source) | client certificate, issued over SSH | **none — TLS-only** | **on, `RequireAndVerifyClientCert`** (`https_port` defaults to 8443) |

Every listener (plain HTTP, HTTPS, SSH) binds the single `bind_address`, which
**defaults to loopback (`127.0.0.1`) in every mode** — shed is local-first.
Facing the network is opt-in: set `bind_address` to a routable address, and in
**open** mode also set `allow_insecure_exposure: true` (open has no transport
security). Token and mtls modes need no acknowledgment to bind the network.

All server settings live in the server config (`/etc/shed/server.yaml` on
Linux, `~/.config/shed/server.yaml` on macOS). Client settings live per server
entry in `~/.shed/config.yaml`.

## SSH key allowlist

By default any public key is accepted (the username still selects the shed).
`auth.ssh` restricts which keys may connect; `auth.mode: token` forces
`enforce`.

```yaml
auth:
  ssh:
    mode: enforce            # off | warn | enforce
    github_users: [charliek] # seed from https://github.com/<user>.keys
    authorized_keys:         # inline OpenSSH authorized_keys lines
      - ssh-ed25519 AAAA... laptop
    authorized_keys_file: ~/.shed/authorized_keys
    github_refresh: 1h       # re-fetch GitHub keys on this interval
    max_auth_tries: 10       # public-key attempts per connection (raise for many-key agents)
```

| Mode | Behavior |
|------|----------|
| `off` | Accept every key (the `open`-mode default). |
| `warn` | Log would-deny attempts but still accept — useful while building the list. |
| `enforce` | Reject keys not in the allowlist (forced in `token` mode). |

GitHub-seeded keys are fetched at startup and on `github_refresh`, cached to
`{state_dir}/github_keys/<user>`, and kept as last-known-good if GitHub is
unreachable. **`enforce` with no resolvable keys fails startup** — the server
never starts with an empty allowlist (which would lock everyone out) and never
silently falls back to accept-all.

`github_users` is the recommended identity source: it ties shed access to your
GitHub keys, which you already rotate — and, as below, the same allowlist is
what mints and revokes HTTP tokens.

## HTTP tokens are minted over SSH

This section describes **token** mode. In **mtls** mode there are no bearer
tokens at all — skip ahead to [mTLS mode](#mtls-mode).

There is no `shed-server token new` and no static `auth.http.tokens` list.
Instead, a client that already holds an allowlisted SSH key mints a token over a
reserved **`_bootstrap`** SSH channel, and the server tracks it:

```bash
shed server add shed.example.com --https-port 8443
#  → pins the SSH host key + TLS cert, then SSHes as _bootstrap@host with your
#    key and writes the minted control token into ~/.shed/config.yaml.
```

Under the hood `shed server add` connects as `_bootstrap@<host>` over the pinned
host key. The server **re-verifies the allowlist** for that key (the bootstrap
channel requires `auth.ssh.mode: enforce`), mints a scoped token, and returns a
bundle the CLI persists:

```json
{ "http_port": 8080, "https_port": 8443, "tls_cert_fingerprint": "sha256:…",
  "token": "shed_control_…", "scope": "control",
  "token_id": "…", "expires_at": "2026-06-15T00:00:00Z" }
```

Tokens have the shape `shed_<scope>_<base64url-random>`. They are **opaque and
server-tracked**: the server stores only a SHA-256 hash (the plaintext never
touches disk), alongside a non-secret `token_id`, the subject key fingerprint,
the scope, and an expiry.

| Scope | Grants |
|-------|--------|
| `control` | The control plane (lifecycle, images, sessions, snapshots), the Connect tunnel for `shed forward`, and the egress audit stream. |
| `credentials` | The credential bus (`/api/plugins/*`) — live SSH signatures and cloud credentials — plus the Connect tunnel and the egress audit stream (used by the host-agent's reverse proxy / egress subscriber). |

(The pre-v0.7.1 `admin` scope is removed.) Under `token` mode every request needs
a matching `Authorization: Bearer` token of the required scope. The bus requires
`credentials`, so a leaked `control` token cannot reach the live-secret bus; the
Connect tunnel and the egress audit stream accept **either** scope (the CLI uses
`control`, the host-agent's proxy / egress subscriber uses `credentials`).
`GET /api/info` stays reachable without a
token so `shed server add` can read the auth mode and ports before the operator
holds one.

### Short TTL, transparent refresh

Minted tokens are short-lived — `auth.token_ttl`, default **24h**:

```yaml
auth:
  mode: token
  token_ttl: 24h
```

Every client refreshes transparently, so the TTL is invisible in normal use:

- **CLI** — re-bootstraps when the stored token is near expiry, and on a `401`
  it refreshes and retries the request once. The expiry is tracked in the
  client config as `control_token_expires_at`.
- **host-agent** — mints its **own** `credentials` token over the same SSH
  bootstrap channel using its identity key, and refreshes on a jittered ~50% of
  the TTL. It no longer reads a static `credentials_token` from config.
- **shed-desktop** — requests a `control` token from the local host-agent (over
  the host-agent's Unix socket) and refreshes near expiry / on `401`.
- **background tunnels** (`shed tunnels start -d`) — the detached daemon re-mints
  its `control` token the same way (proactively near expiry, reactively on a
  `401`); once running those re-mints are **in memory only**, so a multi-day daemon
  never rewrites `~/.shed/config.yaml` and can't clobber a concurrent foreground
  edit. It re-mints non-interactively (SSH `BatchMode`), so it needs SSH access
  without a prompt — an agent that outlives the launching terminal, or a
  passphrase-less / agent-loaded key.

### Revocation follows the allowlist

You do not revoke tokens by hand. When a key leaves the SSH allowlist — removed
from `authorized_keys`, or dropped from a `github_users` account on the next
`github_refresh` — the server purges every token minted for that subject. The
revocation latency is therefore the allowlist's own refresh latency
(`github_refresh` + TTL for GitHub-sourced keys); it is not instantaneous, and
it never fires on a transient GitHub fetch failure (the last-known-good
allowlist is retained, so a network blip cannot mass-revoke valid tokens).

Clients carry their (auto-managed) token in `~/.shed/config.yaml`:

```yaml
servers:
  my-server:
    api_url: https://shed.example.com:8443
    tls_cert_fingerprint: sha256:<hex>
    control_token:            shed_control_xxxxx   # written by `server add`
    control_token_expires_at: 2026-06-15T00:00:00Z # refresh hint
```

There is no client `credentials_token`: the host-agent mints its own.

## Native pinned TLS

shed uses **pinned self-signed TLS** for the *server's* identity — no public
CA, no domain, no ACME. The server generates a self-signed certificate on
first start (the same lifecycle as the SSH host key) and clients pin it by
the SHA-256 fingerprint of its DER encoding, exactly the trust model SSH host
keys use. `auth.mode: token` and `auth.mode: mtls` both turn this on by
default (`https_port` defaults to `8443`).

This is a **separate mechanism from the small internal CA** mtls mode uses to
sign *client* certificates (see [mTLS mode](#mtls-mode)) — the server's own
leaf and how clients pin it are unchanged by mtls; only the client-credential
side changes from a bearer token to a certificate.

```yaml
https_port: 8443                 # the pinned-TLS listener (token/mtls mode serves HTTPS only)
tls_names:                       # extra SANs so hostname verification passes
  - shed.example.com
  - 203.0.113.10
# localhost, 127.0.0.1, ::1 are always included.
```

`shed server add` pins the fingerprint automatically (it is in the bootstrap
bundle). All clients verify by pin: the Go CLI and sdk via
`VerifyPeerCertificate`, the desktop via `URLSessionDelegate`, and `curl` via
the cert handed in with `--cacert`. A client that configures a pin but a
non-`https` URL **fails closed** rather than sending plaintext.

**Rotation.** Changing `tls_names` regenerates the certificate (new
fingerprint). Re-pin clients with:

```bash
shed server update my-server --tls-fingerprint sha256:<new>   # pin out-of-band
shed server update my-server --refetch                        # fetch + re-pin (TOFU)
```

Rotating an existing pin in a non-interactive session requires
`--trust-on-first-use`, so a re-pin is never silently accepted.

## Network surface

There is **one HTTP listener per mode** (SSH always listens separately on
`ssh_port`): a single plain-HTTP listener in open mode, or a single pinned-TLS
(`https_port`) listener in `token`/`mtls` mode — in either enforced mode there
is **no plain-HTTP listener at all**. The credential bus (`/api/plugins/*`)
and the Connect tunnel (`/api/sheds/*/connect/*`) ride that same listener; in
an enforced mode the bus is gated by the `credentials` scope (so a leaked
`control` credential can't reach it) while the Connect tunnel accepts control
or credentials, and both travel over TLS. A co-located host-agent reaches the
bus over `https://127.0.0.1:8443` with the pinned cert (in mtls mode, its own
credentials-scope client certificate). There is no separate internal/loopback
listener. These knobs shape what is reachable where:

| Field | Effect |
|-------|--------|
| `bind_address` | Interface every listener (plain HTTP, HTTPS, SSH) binds to. **Defaults to loopback (`127.0.0.1`) in every mode.** Set a specific IP, `0.0.0.0`/`*` (all IPv4), or `::` (all interfaces) to face the network. |
| `allow_insecure_exposure` | Required to bind a **non-loopback** `bind_address` in **open** mode (no transport security). Ignored in token/mtls mode and for loopback binds. |
| `https_port` | The HTTPS listener (bound to `bind_address`) serving the control plane, credential bus, and Connect tunnel over pinned TLS. **Requires `token` or `mtls` mode**; defaults to `8443` there. |
| `trusted_proxy` | Trust `X-Forwarded-For` (only safe behind a proxy that overwrites it). Default false uses the real TCP peer, so a source IP can't be forged to evade per-IP controls. |

## Connection flow — what's encrypted where

| Hop | open mode | token mode | mtls mode |
|-----|-----------|-------------|-----------|
| Control-plane / bus HTTP | plain `http://` (the network is the trust boundary) | pinned `https://` only — a client that holds a pin but is given a non-`https` URL **fails closed** rather than sending plaintext | pinned `https://`, client presents a certificate at the handshake; a peer with no cert never completes the handshake |
| SSH (shed sessions + the `_bootstrap` issuance channel) | encrypted; host key pinned in `known_hosts` | same | same |
| Credential mint/issuance | n/a (no credential) | bearer token, over the pinned-host-key SSH `_bootstrap` channel | client certificate, over the same `_bootstrap` channel (CSR in, signed cert out) |

In token and mtls modes **nothing plaintext faces the network**: there is no
plain-HTTP listener at all (see [Network surface](#network-surface)), and the
only trust-on-first-use moment is `shed server add` fetching the SSH host key
and TLS/CA fingerprints to show you for confirmation (or pass
`--tls-fingerprint` / `--fingerprint` to verify out-of-band). After that, every
byte travels over pinned TLS or pinned-host-key SSH — the flow never depends on
an unverified or plaintext response.

## Credential bus

The credential bus brokers live secrets (SSH signatures, AWS/Docker
credentials) between a shed VM and a host-side agent. Beyond the
`credentials`-scope gate, the server defends against **response injection** at
the registry, below the HTTP handler.

When a request is dispatched to the registered listener, the registry records it
as pending, keyed by `(namespace, shed, requestID)` — and the requestID (a
UUIDv7) is delivered **only** to that listener over its SSE stream. A
`POST /respond` is honored only if it matches an outstanding pending request:

- A forged response (a made-up requestID) has no pending entry → **dropped**.
- A response is consumed on its final reply → a **replay** is dropped.
- When the listener disconnects, its un-acked pending requests are **retained**
  and **re-delivered** when a listener re-subscribes, so a host-agent that
  reconnects across a blip doesn't strand an in-flight credential request.
  Re-subscribing requires the `credentials` token and the namespace allows only
  one listener at a time (a second subscriber is rejected `409`), so only the
  same credential holder can ever receive the re-delivery; stale pending is
  swept after a retention TTL.

This ownership tracking is enforced whenever HTTP auth is on (i.e. `token`
mode); with auth off (the `open` default) the bus behaves exactly as before, and
without re-delivery.

## Token mode

`auth.mode: token` is the internet-facing posture. It derives the full bundle
and **refuses to start** if a piece is missing, naming the first gap — so you
can't half-deploy to a public address.

```yaml
auth:
  mode: token
  ssh:
    github_users: [charliek]   # an SSH key source is required
```

What `token` derives:

| Derived | Why |
|---------|-----|
| `auth.ssh.mode: enforce` | Only allowlisted keys may SSH in — and that allowlist is what mints/revokes HTTP tokens. **Requires** a key source (`github_users`, `authorized_keys`, or `authorized_keys_file`), else startup fails. |
| HTTP auth enforced | Every HTTP request needs a bearer token; the credential bus is gated by the `credentials` scope. |
| `https_port: 8443` | The network-facing API is pinned TLS. |
| no plain-HTTP listener | Token mode serves **no** plain-HTTP listener at all (not even on loopback) — only the pinned-TLS listener faces clients. On-box tooling (a co-located host-agent) reaches the control plane and credential bus over `https://127.0.0.1:8443` with the pinned cert; there is no plaintext channel. |

The whole flow is hands-off: enable `token`, list your `github_users`, and a
client runs one `shed server add` to pin TLS and mint its token. See the
[Public VPS Deployment](../guides/vps-deployment.md) guide for a complete
walkthrough.

### Removed in v0.7.1

These pre-v0.7.1 keys are **rejected at startup** (the server names them and
exits) so an old config can't silently weaken a deployment:

| Removed | Replacement |
|---------|-------------|
| `public_exposure: true` | `auth.mode: token` (derives the same bundle). |
| `auth.http.tokens` (static list) + `shed-server token new` | Tokens minted over the `_bootstrap` SSH channel by `shed server add`. |
| `admin` scope | `control` + `credentials` only. |
| client `credentials_token` | The host-agent mints its own `credentials` token. |

See [Upgrading v0.7.0 → v0.7.1](../upgrades/v0.7.0-to-v0.7.1.md) for the migration.

### Removed/changed in v0.7.2

v0.7.2 collapses the intermediate states beneath `auth.mode` so that
`tokens ⟺ TLS ⟺ token` and `https ⟺ token` always hold. These are **rejected
at startup**:

| Removed / invalid | Replacement |
|-------------------|-------------|
| `auth.http.mode` (the whole `auth.http` block) | HTTP enforcement derives from `auth.mode: token`. |
| `https_port` under `auth.mode: open` | Use `token` (defaults `https_port` to 8443), or drop it. |
| `auth.ssh.mode: enforce` under `open` | Use `token`, or `warn` to stage. |
| `auth.ssh.mode: off`/`warn` under `token` | Remove it — token forces `enforce`. |

Plus a behavior change: **token mode no longer serves a loopback plain-HTTP
listener** (TLS-only). See [Upgrading v0.7.1 → v0.7.2](../upgrades/v0.7.1-to-v0.7.2.md).

### Removed/changed in v0.7.4

v0.7.4 collapses to a single listener per mode and makes shed local-first. These
are **rejected at startup**:

| Removed / renamed | Replacement |
|-------------------|-------------|
| `internal_http_port` | Removed — the credential bus (credentials scope) and Connect tunnel (control or credentials) ride the single listener in token mode; a co-located host-agent reaches them over `https://127.0.0.1:8443`. |
| `http_bind` / `ssh_bind` | A single `bind_address` governs every listener (HTTP, HTTPS, SSH). |

Plus a behavior change: **`bind_address` defaults to loopback (`127.0.0.1`) in
both modes** (previously unset = all interfaces), and `http_port` is optional in
token mode. A non-loopback bind in open mode now requires
`allow_insecure_exposure: true`; token mode binds the network without an ack.
See [Upgrading v0.7.3 → v0.7.4](../upgrades/v0.7.3-to-v0.7.4.md).

## mTLS mode

`auth.mode: mtls` is the **recommended posture for anything internet-facing**
(and the intended eventual default). It inherits every `token`-mode invariant
— SSH allowlist forced to `enforce`, an SSH key source required at preflight,
TLS-only serving, `https_port` defaulting to `8443`, no acknowledgment needed
to bind the network — and replaces the bearer token with a **client
certificate bound to a private key that never leaves the client's device**,
issued over the same already-authenticated `_bootstrap` SSH channel that mints
tokens in `token` mode.

```yaml
auth:
  mode: mtls
  ssh:
    github_users: [charliek]   # an SSH key source is required, same as token mode
```

### What it guarantees — precisely

The server's HTTPS listener runs `RequireAndVerifyClientCert` against a small
internal CA (`ca_cert.pem` / `ca_key.pem`, generated on first mtls startup and
persisted next to the SSH host key / TLS cert). The precise claim, stated
carefully rather than oversold:

- **An unauthenticated peer can never send an HTTP byte or reach the
  router.** The TLS handshake itself enforces the certificate — Go's
  `net/http` never invokes a handler until `RequireAndVerifyClientCert`
  passes, so there is no code path from "no cert" to a parsed request.
- **The listener and the TLS `CertificateRequest` remain observable to a
  scanner.** A port scan still finds the port open, and `openssl s_client`
  still completes a TCP connection and sees the server ask for a client
  certificate. Nothing about mtls mode makes the port *invisible* — the
  guarantee is about what happens *after* the handshake starts, not whether
  the port exists. Docs and error messages avoid the word "invisible" for
  exactly this reason.

Live-verified behavior (see the branch's validation transcripts): `curl -k`
with no client certificate against an mtls server gets `http_code=000` and
curl exit 56 — **no HTTP status is ever obtained**. `openssl s_client` shows
the server send `Acceptable client certificate CA names` (the
`CertificateRequest`) and then terminate with `tlsv13 alert certificate
required` — the connection dies at the TLS layer, before any HTTP parsing.
With the enrolled certificate, the same request returns `200`.

### The certificate IS the credential

There are no bearer tokens in mtls mode: nothing mints them, the
`Authorization` header is ignored (it cannot augment or substitute for a
certificate's scope), and the token store is not wired. The certificate's
subject carries the principal, composed entirely server-side from
authenticated knowledge — a client cannot request an identity it doesn't
hold:

- **CN** = the SSH key fingerprint (`SHA256:...`), the same subject string
  tokens use today.
- **OU** = scope (`control` or `credentials`), one scope per certificate.
- **O** = client kind (`cli`, `host-agent`, `desktop`, `mobile`).
- TTL = `auth.token_ttl` (default 24h), extended-key-usage
  `ClientAuth` only.

Enrollment rides the `_bootstrap` channel exactly like a token mint, with a
CSR appended to the request line: the client generates a P-256 keypair
locally, sends the CSR, and the server signs it — ignoring every requested
subject field, SAN, or extension in the CSR itself.

### Per-request re-validation

A TLS handshake happens once per connection, but a pooled keep-alive
connection can carry many requests — so the mtls auth middleware
**re-validates on every request**, not just at the handshake: certificate
`NotBefore`/`NotAfter` against the current time, the CN against the *live*
SSH allowlist, and the OU against the route's required scope. This gives mtls
the same "revocation/expiry lands on the next request" property token mode
has always had, rather than a weaker "only checked once per TCP connection"
guarantee.

### Revocation

Revoking a client certificate means **removing its SSH key from the
allowlist** — there is no separate `RevokeBySubject` for certificates (that
mechanism still exists for tokens). The next request on any connection
presenting that certificate is rejected by the per-request re-validation
above; a new connection is rejected at the handshake itself.

This is a **deliberate, coarser tradeoff than token mode**: removing a key in
mtls mode also cuts off shell/SFTP access (the SSH allowlist is now the one
lever for both), where token mode can revoke an HTTP credential independently
via `RevokeBySubject`. Documented and accepted — an mtls deployment that wants
per-purpose revocation without cutting SSH access should stay on `token` mode.

### Accepted limitations

- **Already-established streams survive revocation until they close.** An SSE
  subscription or a `shed forward` tunnel opened before a certificate is
  revoked or expires keeps running until it ends — identical to token mode's
  behavior (tokens are also only checked at request time, not mid-stream).
  Revocation and expiry bind on the *next* request/dial, not on
  already-hijacked byte-copy loops.
- **`curl` and ad-hoc scripts cannot reach an mtls server.** There is no way
  to hand a script a bearer header and have it work — it needs a client
  certificate and the private key that signed its CSR. **This is exactly why
  `token` mode is retained and not deprecated**: anything that needs
  programmatic HTTP access without the shed client fleet (CI runners,
  third-party integrations, `curl` debugging) stays on `token`.
- **CA rotation is manual.** Deleting `ca_cert.pem` + `ca_key.pem` and
  restarting the server invalidates every previously-issued client
  certificate at once — a fleet-wide re-enrollment. Every well-behaved client
  recovers automatically (one silent SSH round-trip on its next command, via
  the same adaptive-transport mechanism that handles ordinary renewal and
  mode flips), so the operator-facing cost is "every client pays one extra
  SSH round-trip," not an outage requiring per-client intervention. There is
  no `shed server ca rotate` / `ca status` command yet — a full CLI story is
  future work. The server logs the CA fingerprint and expiry at startup (like
  the TLS cert fingerprint) and **warns when the CA is within 90 days of
  expiry**; `/api/info` also carries `ca_fingerprint` + `ca_not_after` in mtls
  mode (see [API › GET /api/info](api.md#get-apiinfo)).

See [Upgrading to mTLS](../upgrades/token-to-mtls.md) for the client-then-server
rollout order and the desktop component-upgrade ordering.

## Deferred

An **enrollment secret** (a transport-layer HMAC over the bootstrap handshake,
to gate issuance even tighter than the SSH allowlist) remains out of scope.
Mutual TLS itself is no longer deferred — see [mTLS mode](#mtls-mode) above.
