# Public VPS Deployment

This guide deploys shed-server on an internet-facing VPS, locked down so that
**only your GitHub keys** can SSH in, **all HTTP is encrypted**, and the
**credential bus is reachable only with the required scope over TLS**.

The default shed posture is local-only (open, bound to loopback). This guide is
the opposite end: a hardened, internet-facing server. mtls derives the
hardening bundle (SSH allowlist enforced + client certificate enforced +
TLS-only, with no plain-HTTP listener) and **refuses to start** if any piece is
missing; one `bind_address` faces the network:

- **[`auth.mode: mtls`](#1-server-config)**, the one networked posture. The
  HTTP credential is a client certificate bound to a private key that never
  leaves the client's device, issued over SSH. An unauthenticated peer can
  never complete the TLS handshake, let alone reach the router.
- A CI runner or agent runner that needs only its own sheds can be a
  [service client](orchestrated-sheds.md) of the same server.
- Several people can share it with a user list; see the [Multi-user server
  guide](multi-user-server.md). This guide uses one person's keys
  (`auth.ssh.github_users`).

There are no credentials to mint or paste by hand: clients get
them automatically over SSH.

## 1. Server config

Write `/etc/shed/server.yaml`. mtls requires an SSH key source, and —
since v0.7.4 every posture defaults to loopback — a `bind_address` so the VPS
is reachable from off-box:

```yaml
auth:
  mode: mtls                     # derives the full bundle; refuses to start without a key source
  ssh:
    github_users: [alice-gh]     # only these GitHub keys may SSH in (and enroll certificates)
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
    loopback only and is unreachable from your laptop. mtls needs no
    acknowledgment for it — TLS plus a required credential make the network
    bind safe — and an open server may not bind beyond loopback at all.

Start (or restart) the server. If a required piece is missing it exits
immediately, naming the gap:

```text
auth.mode: mtls requires at least one SSH key source (auth.users, or auth.ssh.github_users, authorized_keys, or authorized_keys_file)
```

!!! note "Removed in v0.10.0: token mode"
    `auth.mode: token` (and its older spelling `secure`) was removed in
    v0.10.0: a config that names it fails to load with a message naming
    `mtls`, rather than starting open. Switch it to `auth.mode: mtls`; every
    client since v0.8.1 re-enrolls with a certificate on its next request.
    Callers that used a bearer token directly (`curl`, scripts, CI) must
    present a client certificate, or become a [service
    client](orchestrated-sheds.md).

## 2. Add the server from your client

From your laptop, one command enrolls a certificate over SSH — `shed server
add` is SSH-first, so it needs the SSH port (default
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
plain-TOFU adds: `shed server add` against an mtls server now
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

## 3. Credential brokering over TLS

The shed desktop app on your laptop is the credential broker. Its broker **enrolls its
own `credentials`-scope certificate** over the same SSH bootstrap channel —
there is no credential to paste — and subscribes to the credential bus over
the pinned TLS connection, brokering SSH signatures / cloud credentials to
your remote shed. It picks up `api_url` and `tls_cert_fingerprint` from the
same `~/.shed/config.yaml` entry, and persists its own certificate/key in its
state dir (never the same key material the CLI holds — one certificate per
process, per scope). The app also needs launch at login on, which it turns on at its first
launch, since credentials are answered only while it runs.

The bus stream is long-lived and often idle; shed-server sends an SSE
keepalive comment every 25 s so an idle NAT or proxy does not evict the
connection. If the broker reconnects across a blip, a credential request it had
not answered is delivered to it again; no other machine ever receives it.

The first machine whose desktop app connects becomes your
**credential source** on this server, and only it answers your sheds'
requests. A second machine of yours is refused, names the first, and keeps
retrying until you move the source with `shed source use` (see [Security ›
One machine answers for each
user](../reference/security.md#one-machine-answers-for-each-user)). Run
the desktop app at 0.10.0 or newer: an older one is refused with a message to
upgrade.

## 4. Rotation and expiry

Client certificates are short-lived (`auth.token_ttl`, default 24h) and
renew themselves: near expiry, or on an auth-shaped failure, the client
generates a fresh keypair + CSR and re-enrolls over SSH — the same
`reqwest::Client`/`http.Transport` instance keeps running throughout (no
client rebuild). You never rotate a certificate by hand. To **revoke**
access, remove the key from the allowlist (drop it from `github_users`, or
from the GitHub account); the next request on any connection presenting that
certificate is rejected. Removing the SSH key also cuts shell and SFTP
access: the key set is the one lever for both. See [Security › mTLS
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

## Hardening the sheds (Firecracker)

The steps above protect the server's API and SSH. They do not limit what a
shed can reach: an `open` shed, the default, reaches whatever the host's NAT
reaches. On a cloud VPS that includes the provider's metadata service at
`169.254.169.254`, the host's own ports, other sheds and the provider's
private network. The `isolated` network profile closes all of those, leaving
the internet. Make it the default for every create:

```yaml
network:
  default_profile: isolated    # every shed is internet-only unless created with --network open
  allow_cidrs: []              # add private ranges the sheds must still reach (a registry on the VPC)
```

Create the sheds that need more with `shed create <name> --network open`, and
the ones that should not see the host's directories with `--mounts none`
(`shed list -v` shows both). `allow_cidrs` opens only routed destinations,
never the host or other sheds.

The host needs the `nft` binary (`command -v nft`; `sudo apt install
nftables` if it is missing), and shed-server must run as root, which the deb's
systemd unit does. With `default_profile: isolated` and no working `nft`,
every create without `--network open` is refused with
`503 NETWORK_ENFORCEMENT_UNAVAILABLE`. What the profile blocks, and its limits,
are in [Security › Network profiles](../reference/security.md#network-profiles).

## Hardening the add-time trust

`shed server add` closes the add-time MITM window by prompting you to confirm
the SSH host key (and, for mtls, the TLS cert fingerprint) before enrolling.
Tighten it further by verifying out-of-band (`--fingerprint` /
`--tls-fingerprint`, read from the server's startup log), or bring your own
TLS certificate instead of the self-signed one with `tls_cert_file` /
`tls_key_file` in the server config (this affects only the server's own
leaf, not the internal client-certificate CA).

## Hardening the SSH surface

SSH stays the internet-exposed root of trust: it's the shell
access channel *and* the channel that issues every HTTP credential, so it's
worth the same operational care as any other internet-facing SSH daemon:

- **`auth.ssh.max_auth_tries`** (default `10`) bounds public-key attempts per
  connection; raise it only if a legitimate client's agent (1Password,
  Secretive) holds enough keys that the allowlisted one gets tried late.
- **Built-in limits on connections that have not authenticated**, in every
  auth mode: a connection is closed if it has not authenticated within 120
  seconds; at most 100 may be unauthenticated at once, and at most 10 from one
  address (an IPv4 address or an IPv6 `/64`). They are fixed values. What each
  does, the lines it logs, and what to do when the limit refuses everyone are
  in [Security › Facing the
  internet](../reference/security.md#facing-the-internet).
- **fail2ban** (or an equivalent rate-limiter) is standard, optional
  hardening for a publicly-reachable `ssh_port`: shed rejects unauthorized
  keys itself, but banning an address that keeps failing reduces log noise and
  the (already bounded) cost of scanner traffic. Match these lines, each of
  which names the peer's address:
    - `SSH auth failed: remote=<host:port> ...`, written once per connection
      that offered a key and closed without proving one;
    - `SSH: closed a connection from <host:port> that did not authenticate ...`
      (the login grace);
    - `SSH: refusing connections from <address>: ...` (the per-address limit).

    Never match `SSH auth DENIED`: it is written once per refused key, so a
    person whose agent offers several keys draws several `DENIED` lines on a
    login that then succeeds, and would be banned for it. This is
    operator-managed; shed does not ship a rate-limiter itself.
- Keeping `ssh_port` off the well-known `22` is a minor, optional reduction
  in automated-scanner noise, not a security boundary on its own.

An example for a deb install (the unit `shed-server.service` logs to the
journal), banning an address after 5 failures in 10 minutes. Each pattern is
anchored at the start of the message, so a username a client chooses (which
comes after the address) cannot put another address in front of it:

```ini
# /etc/fail2ban/filter.d/shed-ssh.conf
[INCLUDES]
before = common.conf

[Definition]
_daemon = shed-server
_go_time = (?:\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2} )?
failregex = ^%(__prefix_line)s%(_go_time)sSSH auth failed: remote=\[?<HOST>\]?:\d+ user=
            ^%(__prefix_line)s%(_go_time)sSSH: closed a connection from \[?<HOST>\]?:\d+ that did not authenticate
            ^%(__prefix_line)s%(_go_time)sSSH: refusing connections from <HOST>(?:/64)?: 
ignoreregex =

[Init]
journalmatch = _SYSTEMD_UNIT=shed-server.service
```

```ini
# /etc/fail2ban/jail.d/shed-ssh.conf
[shed-ssh]
enabled  = true
filter   = shed-ssh
backend  = systemd
port     = 2222
maxretry = 5
findtime = 10m
bantime  = 1h
```

Set `port` to your `ssh_port`, and check the filter against your own log with
`fail2ban-regex systemd-journal shed-ssh` before you enable the jail. Some
limits of this example:

- `maxretry` counts matching lines, not connections: a connection that
  offered a key and then stalled past the login grace writes both its
  `SSH auth failed` line and the grace line, so it counts twice.
- An IPv6 peer's refusal line names its `/64`; the pattern bans the `/64`'s
  first address only, so ban IPv6 ranges at the firewall instead if you see
  them.
- A link-local IPv6 peer whose address carries a zone
  (`remote=[fe80::1%eth0]:4000`) is not matched: fail2ban's `<HOST>` does not
  take the `%eth0`. Such a peer is on your own network segment.
- The line for the limit of 100 (`SSH: refusing new connections: ...`) is not
  matched on purpose: it names the address holding the most connections, not
  the one refused.

## No broker on the server

A server has no credential broker unless a desktop app runs there. Credential requests are
answered only while the shed desktop app is running on your machine; a machine without the
app (a server, or a box with no desktop session) has no broker. Run the app on your own
machine and make it your credential source (see [Credential brokering over
TLS](#3-credential-brokering-over-tls)). The credential bus (`credentials` scope) and Connect
tunnel (`control` or `credentials`) ride the single pinned-TLS listener, so the app reaches
them over `https://` with the pinned cert and its own credentials-scope client certificate.
Remote `shed forward` keeps working at the same time, because there is no separate
loopback-only listener. See the [network
surface](../reference/security.md#network-surface).
