# Security

shed-server has two postures, selected by a single switch:

```yaml
auth:
  mode: open      # open (default) | secure
```

- **`open`** (the default) is the right posture on a private network — a
  Tailscale tailnet or a trusted LAN — where the network is the security
  boundary. No SSH key allowlist, no HTTP token, no TLS is required; each
  hardening layer below is still available, opt-in and independent.
- **`secure`** is for an internet-facing deployment. It derives the whole
  hardening bundle (SSH allowlist enforced + HTTP tokens enforced + TLS +
  loopback plain-HTTP) and **refuses to start** without an SSH key source, so a
  server is never half-hardened on a public address.

| `auth.mode` | SSH allowlist | HTTP tokens | Plain-HTTP listener | TLS |
|-------------|---------------|-------------|---------------------|-----|
| `open` | as configured (`auth.ssh`, default off) | off | as configured (`http_bind`) | optional (`https_port`) |
| `secure` | **forced `enforce`** (needs a key source) | **enforced** | **forced loopback** | **on** (`https_port` defaults to 8443) |

All server settings live in the server config (`/etc/shed/server.yaml` on
Linux, `~/.config/shed/server.yaml` on macOS). Client settings live per server
entry in `~/.shed/config.yaml`.

## SSH key allowlist

By default any public key is accepted (the username still selects the shed).
`auth.ssh` restricts which keys may connect; `auth.mode: secure` forces
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
    max_auth_tries: 6        # public-key attempts per connection
```

| Mode | Behavior |
|------|----------|
| `off` | Accept every key (the `open`-mode default). |
| `warn` | Log would-deny attempts but still accept — useful while building the list. |
| `enforce` | Reject keys not in the allowlist (forced in `secure` mode). |

GitHub-seeded keys are fetched at startup and on `github_refresh`, cached to
`{state_dir}/github_keys/<user>`, and kept as last-known-good if GitHub is
unreachable. **`enforce` with no resolvable keys fails startup** — the server
never starts with an empty allowlist (which would lock everyone out) and never
silently falls back to accept-all.

`github_users` is the recommended identity source: it ties shed access to your
GitHub keys, which you already rotate — and, as below, the same allowlist is
what mints and revokes HTTP tokens.

## HTTP tokens are minted over SSH

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
| `control` | The control plane: lifecycle, images, sessions, snapshots, and the Connect tunnel for `shed forward`. |
| `credentials` | The credential bus (`/api/plugins/*`) and the Connect tunnel — vends live SSH signatures and cloud credentials. |

(The pre-v0.8 `admin` scope is removed.) Under `secure` mode every request needs
a matching `Authorization: Bearer` token of the required scope; the bus and
Connect tunnel specifically require `credentials`, so a leaked `control` token
cannot reach them. `GET /api/info` stays reachable without a token so
`shed server add` can read the auth mode and ports before the operator holds
one.

### Short TTL, transparent refresh

Minted tokens are short-lived — `auth.token_ttl`, default **24h**:

```yaml
auth:
  mode: secure
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

shed uses **pinned self-signed TLS** — no CA, no domain, no ACME. The server
generates a self-signed certificate on first start (the same lifecycle as the
SSH host key) and clients pin it by the SHA-256 fingerprint of its DER encoding,
exactly the trust model SSH host keys use. `auth.mode: secure` turns this on by
default (`https_port` defaults to `8443`).

```yaml
https_port: 8443                 # serve HTTPS here (in addition to http_port)
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

By default both listeners bind all interfaces on a single HTTP listener. These
knobs shape what is reachable where; `auth.mode: secure` forces the plain-HTTP
listener to loopback regardless of `http_bind`.

| Field | Effect |
|-------|--------|
| `http_bind` | Interface for the plain-HTTP listener (e.g. `127.0.0.1`, a tailnet IP). Empty = all interfaces. Forced to loopback in `secure` mode. |
| `ssh_bind` | Interface for the SSH listener. |
| `https_port` | When set, an HTTPS listener (shares `http_bind`) serving the same control plane over pinned TLS. On by default in `secure` mode. |
| `internal_http_port` | When > 0, moves the credential bus (`/api/plugins/*`) **and the Connect tunnel** to a loopback-only internal listener; the public listener omits them. |
| `trusted_proxy` | Trust `X-Forwarded-For` (only safe behind a proxy that overwrites it). Default false uses the real TCP peer, so a source IP can't be forged to evade per-IP controls. |

### Route matrix

Where the bus and Connect tunnel are reachable depends on `internal_http_port`:

| `internal_http_port` | Bus + Connect tunnel | Use case |
|----------------------|----------------------|----------|
| unset (0) | On the public listener, **gated by the `credentials` scope** when HTTP auth is enforced (and over TLS when `https_port` is set) | **Remote** host-agent / remote `shed forward` — the host-agent runs on your laptop and brokers to a remote shed. |
| set (> 0) | On a **loopback-only** internal listener; omitted from the public listener | **Co-located** host-agent — runs on the same box as shed-server and reaches the bus over loopback. |

!!! warning "Co-located split disables remote `shed forward`"
    Setting `internal_http_port` moves the Connect tunnel to the loopback
    listener, so `shed forward` and a remote host-agent can no longer reach it
    over the network. Use the internal split only when the host-agent is
    co-located with shed-server. For remote access, leave `internal_http_port`
    unset — HTTP-enforce already gates the bus and tunnel by the `credentials`
    scope.

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

This ownership tracking is enforced whenever HTTP auth is on (i.e. `secure`
mode); with auth off (the `open` default) the bus behaves exactly as before, and
without re-delivery.

## Secure mode

`auth.mode: secure` is the internet-facing posture. It derives the full bundle
and **refuses to start** if a piece is missing, naming the first gap — so you
can't half-deploy to a public address.

```yaml
auth:
  mode: secure
  ssh:
    github_users: [charliek]   # an SSH key source is required
```

What `secure` derives:

| Derived | Why |
|---------|-----|
| `auth.ssh.mode: enforce` | Only allowlisted keys may SSH in — and that allowlist is what mints/revokes HTTP tokens. **Requires** a key source (`github_users`, `authorized_keys`, or `authorized_keys_file`), else startup fails. |
| HTTP auth enforced | Every HTTP request needs a bearer token; the credential bus is gated by the `credentials` scope. |
| `https_port: 8443` | The network-facing API is pinned TLS. |
| plain-HTTP → loopback | The plain-HTTP listener is forced to loopback regardless of `http_bind`, so only the TLS listener (and the loopback internal bus, if configured) face the network — no public plaintext path. |

The whole flow is hands-off: enable `secure`, list your `github_users`, and a
client runs one `shed server add` to pin TLS and mint its token. See the
[Public VPS Deployment](../guides/vps-deployment.md) guide for a complete
walkthrough.

### Removed in v0.8

These pre-v0.8 keys are **rejected at startup** (the server names them and
exits) so an old config can't silently weaken a deployment:

| Removed | Replacement |
|---------|-------------|
| `public_exposure: true` | `auth.mode: secure` (derives the same bundle). |
| `auth.http.tokens` (static list) + `shed-server token new` | Tokens minted over the `_bootstrap` SSH channel by `shed server add`. |
| `admin` scope | `control` + `credentials` only. |
| client `credentials_token` | The host-agent mints its own `credentials` token. |

See [Upgrading v0.7 → v0.8](../upgrades/v0.7-to-v0.8.md) for the migration.

## Deferred

Two hardening layers are intentionally out of scope for v0.8 and tracked
separately: an **enrollment secret** (a transport-layer HMAC over the bootstrap
handshake, to gate token issuance even tighter than the SSH allowlist) and
**mutual TLS** / a **broker handoff** for the credential bus. The current
issuance trust anchor is the SSH allowlist plus the pinned TLS/host keys.
