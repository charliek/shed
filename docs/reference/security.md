# Security

shed-server ships **open by default**: no SSH key allowlist, no HTTP token, no
TLS. That is the right posture on a private network — a Tailscale tailnet or a
trusted LAN — where the network is the security boundary and adding auth would
only get in the way.

Every hardening layer below is **opt-in and independent**. You can enable just
an SSH allowlist, or just HTTP tokens, or the full bundle. For a genuinely
internet-facing deployment, [`public_exposure`](#public-exposure-preflight)
turns the individual toggles into an all-or-nothing bundle the server refuses to
start without.

| Layer | Config | Default |
|-------|--------|---------|
| SSH key allowlist | `auth.ssh` | off (accept all keys) |
| HTTP bearer tokens | `auth.http` | off (no token required) |
| Native pinned TLS | `https_port` + `tls_names` | off (plain HTTP) |
| Network surface / bind | `http_bind`, `ssh_bind`, `internal_http_port` | all interfaces, single listener |
| Public-exposure preflight | `public_exposure` | off (inert) |

All server settings live in the server config (`/etc/shed/server.yaml` on
Linux, `~/.config/shed/server.yaml` on macOS). Client settings live per server
entry in `~/.shed/config.yaml`.

## SSH key allowlist

By default any public key is accepted (the username still selects the shed).
`auth.ssh` restricts which keys may connect.

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
| `off` | Accept every key (legacy default). |
| `warn` | Log would-deny attempts but still accept — useful while building the list. |
| `enforce` | Reject keys not in the allowlist. |

GitHub-seeded keys are fetched at startup and on `github_refresh`, cached to
`{state_dir}/github_keys/<user>`, and kept as last-known-good if GitHub is
unreachable. **`enforce` with no resolvable keys fails startup** — the server
never starts with an empty allowlist (which would lock everyone out) and never
silently falls back to accept-all.

`github_users` is the recommended identity source: it ties shed access to your
GitHub keys, which you already rotate.

## HTTP bearer tokens

By default the HTTP API requires no token. `auth.http` turns on deny-by-default
bearer-token auth.

```yaml
auth:
  http:
    mode: enforce            # off | enforce
    tokens:
      - { name: cli,        scope: control,     token: shed_control_xxxxx }
      - { name: host-agent, scope: credentials, token: shed_credentials_xxxxx }
```

Mint tokens with:

```bash
shed-server token new --scope control       # CLI / desktop
shed-server token new --scope credentials   # host-agent / credential bus
```

Tokens have the shape `shed_<scope>_<base64url-random>` and carry one of three
scopes:

| Scope | Grants |
|-------|--------|
| `control` | The control plane: lifecycle, images, sessions, snapshots, the Connect tunnel for `shed forward`. |
| `credentials` | The credential bus (`/api/plugins/*`) and the Connect tunnel — vends live SSH signatures and cloud credentials. |
| `admin` | Everything. |

Under `mode: enforce`, every request needs a matching `Authorization: Bearer`
token of the required scope; the bus and Connect tunnel specifically require the
`credentials` scope, so a leaked `control` token cannot reach them. Two
**bootstrap** endpoints stay open without a token so `shed server add` can fetch
server info and the SSH host key before the operator holds one: `GET /api/info`
and `GET /api/ssh-host-key`.

Clients carry their tokens in `~/.shed/config.yaml`:

```yaml
servers:
  my-server:
    control_token:     shed_control_xxxxx       # CLI/desktop send this
    credentials_token: shed_credentials_xxxxx   # the host-agent sends this
```

## Native pinned TLS

shed uses **pinned self-signed TLS** — no CA, no domain, no ACME. The server
generates a self-signed certificate on first start (the same lifecycle as the
SSH host key) and clients pin it by the SHA-256 fingerprint of its DER encoding,
exactly the trust model SSH host keys use.

Enable it on the server:

```yaml
https_port: 8443                 # serve HTTPS here (in addition to http_port)
tls_names:                       # extra SANs so hostname verification passes
  - shed.example.com
  - 203.0.113.10
# localhost, 127.0.0.1, ::1 are always included.
```

Pin it on the client with `shed server add`:

```bash
shed server add shed.example.com --https-port 8443
#  → shows the TLS cert fingerprint (and the SSH host-key fingerprint),
#    prompts to trust, and pins it into the server entry.
```

```yaml
servers:
  my-server:
    api_url: https://shed.example.com:8443   # control plane over TLS
    tls_cert_fingerprint: sha256:<hex>       # the pin
```

All clients verify by pin: the Go CLI and sdk via `VerifyPeerCertificate`, the
desktop via `URLSessionDelegate`, and `curl` via the cert handed in with
`--cacert` (the `tls_names` SANs make hostname verification pass). A client that
configures a pin but a non-`https` URL **fails closed** rather than sending
plaintext.

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
knobs shape what is reachable where.

| Field | Effect |
|-------|--------|
| `http_bind` | Interface for the plain-HTTP listener (e.g. `127.0.0.1`, a tailnet IP). Empty = all interfaces. |
| `ssh_bind` | Interface for the SSH listener. |
| `https_port` | When set, an HTTPS listener (shares `http_bind`) serving the same control plane over pinned TLS. |
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
- When a listener disconnects, its pending requests are swept → a reconnecting
  listener cannot answer the previous listener's in-flight requests
  (**listener-squat** defense).

This ownership check is enforced whenever HTTP auth is on; with auth off (the
default) the bus behaves exactly as before.

## Public-exposure preflight

For a genuinely internet-facing deployment, set `public_exposure: true`. The
server then **refuses to start** unless the full hardening bundle is present,
naming the first missing piece — so you can't half-deploy to the internet.

```yaml
public_exposure: true
```

The required bundle:

| Requirement | Why |
|-------------|-----|
| `auth.ssh.mode: enforce` | Only allowlisted keys may SSH in. |
| `auth.http.mode: enforce` + a strong token | Every HTTP request needs a bearer token; this also gates the credential bus by the `credentials` scope. |
| `https_port` set | The network-facing API is TLS. |

When `public_exposure` is set, the plain-HTTP listener is forced to **loopback**
regardless of `http_bind`, so only the TLS listener (and the loopback internal
bus, if configured) face the network — there is no public plaintext API path.
Tokens must be at least 24 characters (generated tokens are well above this), so
an obviously weak hand-set token is rejected.

When `public_exposure` is **unset** (the default), the preflight is inert: bind
behavior — including a non-loopback bind or a routine production restart — is
exactly as it is today.

See the [Public VPS Deployment](../guides/vps-deployment.md) guide for a
complete walkthrough.
