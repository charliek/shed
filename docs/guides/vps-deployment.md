# Public VPS Deployment

This guide deploys shed-server on an internet-facing VPS, locked down so that
**only your GitHub keys** can SSH in, **all HTTP is encrypted**, and the
**credential bus is reachable only with the required scope over TLS**.

The default shed posture is local-only (open, bound to loopback). This guide is
the opposite end: a hardened, internet-facing server. Two postures both derive
the same hardening bundle (SSH allowlist enforced + HTTP credential enforced +
TLS-only, with no plain-HTTP listener) and **refuse to start** if any piece is
missing; one `bind_address` faces the network:

- **[`auth.mode: mtls`](#1-server-config)** — the **recommended** posture. The
  HTTP credential is a client certificate bound to a private key that never
  leaves the client's device, issued over SSH. An unauthenticated peer can
  never complete the TLS handshake, let alone reach the router.
- **[`auth.mode: token`](#token-mode-alternative)** — a bearer token minted
  over SSH. Use this instead when something outside the shed client fleet
  needs to call the API directly with a bearer header — `curl`, a CI runner, a
  third-party integration. mtls servers cannot be reached that way at all, so
  `token` mode is retained and not deprecated.

Either way, there are no credentials to mint or paste by hand: clients get
them automatically over SSH.

## 1. Server config

Write `/etc/shed/server.yaml`. Both modes require an SSH key source, and —
since v0.7.4 every posture defaults to loopback — a `bind_address` so the VPS
is reachable from off-box:

```yaml
auth:
  mode: mtls                     # derives the full bundle; refuses to start without a key source
  ssh:
    github_users: [charliek]     # only these GitHub keys may SSH in (and enroll certificates)
tls_names:
  - shed.example.com             # your VPS hostname / public IP (extra cert SANs on the server's own leaf)
bind_address: 0.0.0.0            # face the network (loopback is the default) — or a specific public IP
```

`mtls` mode forces `auth.ssh.mode: enforce`, turns on pinned TLS with
`RequireAndVerifyClientCert` (`https_port` defaults to `8443`), and serves
**no plain-HTTP listener** (TLS-only) — so the only API is HTTPS on `8443`,
gated by a client certificate of the required scope rather than a bearer
token. (`shed server add` against an mtls server therefore needs
`--ssh-port`, as below — it cannot probe `/api/info` over plain HTTP first,
because the HTTPS listener itself demands a certificate before answering
anything.)

!!! warning "`bind_address` is required for a remote server"
    Since v0.7.4 `bind_address` **defaults to loopback (`127.0.0.1`) in every
    mode**, so without the `bind_address: 0.0.0.0` line above the VPS binds
    loopback only and is unreachable from your laptop. Neither `token` nor
    `mtls` mode needs an `allow_insecure_exposure` ack — TLS plus a required
    credential make the network bind safe.

Start (or restart) the server. If a required piece is missing it exits
immediately, naming the gap:

```text
auth.mode: mtls requires at least one SSH key source (auth.ssh.github_users, authorized_keys, or authorized_keys_file)
```

### Token mode alternative

If you need `curl`/CI/third-party callers to reach the API directly, use
`auth.mode: token` instead — everything else in this guide (the `bind_address`
requirement, the SSH-first add flow, the rotation story) is the same shape,
just with a bearer token instead of a certificate:

```yaml
auth:
  mode: token
  ssh:
    github_users: [charliek]
tls_names: [shed.example.com]
bind_address: 0.0.0.0
```

See [Security › Token mode](../reference/security.md#token-mode) for the
token-specific details (scopes, TTL, revocation by allowlist removal).

## 2. Add the server from your client

From your laptop, one command enrolls a certificate over SSH — `shed server
add` is SSH-first for every enforced mode, so it needs the SSH port (default
`2222`; override with `--ssh-port` if you changed `ssh_port` in
`server.yaml`):

```bash
shed server add shed.example.com --ssh-port 2222 --trust-on-first-use
```

This performs a bounded SSH key-scan to capture and confirm the host key,
then connects over the reserved `_bootstrap` SSH channel (using one of your
allowlisted keys). The client generates a P-256 keypair locally, sends a CSR,
and the server's internal CA signs it — the private key **never leaves your
machine**. The pinned entry — `api_url`, `tls_cert_fingerprint`, `auth_mode:
mtls`, and the certificate/key paths under `~/.shed/creds/<name>/` (mode
0700/0600) — is written to `~/.shed/config.yaml`:

```yaml
servers:
  shed.example.com:
    api_url: https://shed.example.com:8443
    tls_cert_fingerprint: sha256:<pinned at add time>
    auth_mode: mtls
    client_cert_file: /Users/you/.shed/creds/shed.example.com/client.pem
    client_key_file: /Users/you/.shed/creds/shed.example.com/client.key
    client_cert_expires_at: 2026-06-15T00:00:00Z   # refreshed automatically
```

Your SSH key must be on the server's allowlist for enrollment to succeed (it
is — you listed it in `github_users`). This is a **behavior change** from
plain-TOFU adds: `shed server add` against a `token`/`mtls` server now
requires an allowlisted SSH key **at add time**, not just lazily on first API
call. Verify the control plane over the pinned mTLS connection:

```bash
shed -s shed.example.com list
```

To verify the fingerprints out-of-band (e.g. from a CI runner with a key in
the allowlist), pass them explicitly:

```bash
shed server add shed.example.com --ssh-port 2222 \
  --tls-fingerprint sha256:<hex> --fingerprint SHA256:<ssh>
```

Adding against `token` mode looks identical except the entry gets
`control_token`/`control_token_expires_at` instead of a certificate — see
[Security › HTTP tokens are minted over
SSH](../reference/security.md#http-tokens-are-minted-over-ssh).

## 3. Credential brokering over TLS

Point the host-agent (running on your laptop) at the server. It **enrolls its
own `credentials`-scope certificate** over the same SSH bootstrap channel —
there is no credential to paste — and subscribes to the credential bus over
the pinned TLS connection, brokering SSH signatures / cloud credentials to
your remote shed. It picks up `api_url` and `tls_cert_fingerprint` from the
same `~/.shed/config.yaml` entry, and persists its own certificate/key in its
state dir (never the same key material the CLI holds — one certificate per
process, per scope).

The bus stream is long-lived and often idle; shed-server sends a periodic SSE
keepalive comment so an idle NAT or proxy does not evict the connection. If the
agent reconnects across a blip, any un-acked credential request is re-delivered.

## 4. Rotation and expiry

Client certificates are short-lived (`auth.token_ttl`, default 24h) and
renew themselves: near expiry, or on an auth-shaped failure, the client
generates a fresh keypair + CSR and re-enrolls over SSH — the same
`reqwest::Client`/`http.Transport` instance keeps running throughout (no
client rebuild). You never rotate a certificate by hand. To **revoke**
access, remove the key from the allowlist (drop it from `github_users`, or
from the GitHub account); the next request on any connection presenting that
certificate is rejected. This is coarser than token mode's per-token revoke —
removing the SSH key also cuts shell/SFTP access, since the allowlist is now
the one lever for both. Accepted tradeoff; see [Security › mTLS
mode](../reference/security.md#mtls-mode) for the full revocation model.

**CA rotation** (distinct from per-client certificate renewal above) is
manual: deleting `ca_cert.pem` + `ca_key.pem` on the server and restarting
invalidates every previously-issued client certificate at once — a
fleet-wide re-enrollment. Every well-behaved client recovers on its own (one
silent SSH round-trip on its next command), so the cost is "every client
pays one extra SSH round-trip," not a coordinated outage. There is no `shed
server ca rotate` CLI yet. The server logs the CA fingerprint and expiry at
startup and warns when it's within 90 days of expiring; `/api/info` also
reports `ca_fingerprint` / `ca_not_after` in mtls mode.

Rotate the **server's own** TLS cert (e.g. after changing `tls_names`) and
re-pin clients — this is independent of the client-certificate CA above:

```bash
shed server update shed.example.com --refetch   # fetch the new cert + re-pin
```

## Hardening the add-time trust

`shed server add` closes the add-time MITM window by prompting you to confirm
the SSH host key (and, for mtls, the TLS cert fingerprint) before enrolling.
Tighten it further by verifying out-of-band (`--fingerprint` /
`--tls-fingerprint`, read from the server's startup log), or bring your own
TLS certificate instead of the self-signed one with `tls_cert_file` /
`tls_key_file` in the server config (this affects only the server's own
leaf, not the internal client-certificate CA).

## Hardening the SSH surface

SSH stays the internet-exposed root of trust in both modes — it's the shell
access channel *and* the channel that issues every HTTP credential, so it's
worth the same operational care as any other internet-facing SSH daemon:

- **`auth.ssh.max_auth_tries`** (default `10`) bounds public-key attempts per
  connection; raise it only if a legitimate client's agent (1Password,
  Secretive) holds enough keys that the allowlisted one gets tried late.
- **fail2ban** (or an equivalent rate-limiter) watching shed-server's SSH
  auth-failure log lines is standard, optional hardening for a
  publicly-reachable `ssh_port` — shed's own allowlist rejects unauthorized
  keys, but a connection-rate limiter reduces log noise and the (already
  bounded) cost of processing scanner traffic. This is operator-managed;
  shed does not ship a rate-limiter itself.
- Keeping `ssh_port` off the well-known `22` is a minor, optional reduction
  in automated-scanner noise, not a security boundary on its own.

## Co-located host-agent

If you instead run the host-agent **on the VPS itself**, no extra config is
needed: the credential bus (`credentials` scope) and Connect tunnel (`control`
or `credentials`) ride the single pinned-TLS listener, and the on-box
host-agent reaches them over `https://127.0.0.1:8443` with the pinned cert (in
mtls mode, its own credentials-scope client certificate). Remote `shed
forward` keeps working at the same time, because there is no longer a
separate loopback-only listener. See the [network
surface](../reference/security.md#network-surface).
