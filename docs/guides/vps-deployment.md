# Public VPS Deployment

This guide deploys shed-server on an internet-facing VPS, locked down so that
**only your GitHub keys** can SSH in, **all HTTP is encrypted and
token-authenticated**, and the **credential bus is reachable only with a
credentials token over TLS**.

The default shed posture is local-only (open, bound to loopback). This guide is
the opposite end: a hardened, internet-facing server. One switch —
[`auth.mode: secure`](../reference/security.md#secure-mode) — derives the whole
hardening bundle (SSH allowlist enforced + HTTP tokens enforced + TLS-only, with
no plain-HTTP listener) and **refuses to start** if any piece is missing; one
`bind_address` faces it at the network. There are no tokens to mint or paste:
clients get them automatically over SSH.

## 1. Server config

Write `/etc/shed/server.yaml`. `secure` requires an SSH key source, and — since
v0.7.4 every posture defaults to loopback — a `bind_address` so the VPS is
reachable from off-box:

```yaml
auth:
  mode: secure                   # derives the full bundle; refuses to start without a key source
  ssh:
    github_users: [charliek]     # only these GitHub keys may SSH in (and mint tokens)
tls_names:
  - shed.example.com             # your VPS hostname / public IP (extra cert SANs)
bind_address: 0.0.0.0            # face the network (loopback is the default) — or a specific public IP
```

`secure` mode forces `auth.ssh.mode: enforce`, enforces HTTP bearer tokens, turns
on pinned TLS (`https_port` defaults to `8443`), and serves **no plain-HTTP
listener** (TLS-only) — so the only API is HTTPS on `8443`, with the credential
bus gated by the `credentials` scope and the Connect tunnel accepting `control`
or `credentials`. (`shed server add` against a secure server therefore needs
`--https-port`, as below.)

!!! warning "`bind_address` is required for a remote server"
    Since v0.7.4 `bind_address` **defaults to loopback (`127.0.0.1`) in secure
    mode too**, so without the `bind_address: 0.0.0.0` line above the VPS binds
    loopback only and is unreachable from your laptop. Secure mode needs no
    `allow_insecure_exposure` ack — TLS + tokens make the network bind safe.

Start (or restart) the server. If a required piece is missing it exits
immediately, naming the gap:

```text
auth.mode: secure requires at least one SSH key source (github_users, authorized_keys, or authorized_keys_file)
```

## 2. Add the server from your client

From your laptop, one command pins the server and mints your token:

```bash
shed server add shed.example.com --https-port 8443
```

This fetches the TLS certificate and SSH host key, shows both fingerprints,
prompts you to trust them, then connects over the reserved `_bootstrap` SSH
channel (using one of your allowlisted keys) and mints a `control` token. The
pinned entry — `api_url`, `tls_cert_fingerprint`, `control_token`, and
`control_token_expires_at` — is written to `~/.shed/config.yaml`:

```yaml
servers:
  shed.example.com:
    api_url: https://shed.example.com:8443
    tls_cert_fingerprint: sha256:<pinned at add time>
    control_token:            shed_control_xxxxx     # minted over SSH, never printed
    control_token_expires_at: 2026-06-15T00:00:00Z   # refreshed automatically
```

Your SSH key must be on the server's allowlist for the mint to succeed (it is —
you listed it in `github_users`). Verify the control plane over the pinned TLS
connection:

```bash
shed -s shed.example.com list
```

To verify the fingerprints out-of-band (e.g. from a CI runner with a key in the
allowlist), pass them explicitly:

```bash
shed server add shed.example.com --https-port 8443 \
  --tls-fingerprint sha256:<hex> --fingerprint SHA256:<ssh>
```

## 3. Credential brokering over TLS

Point the host-agent (running on your laptop) at the server. It **mints its own
`credentials` token** over the same SSH bootstrap channel — there is no token to
paste — and subscribes to the credential bus over the pinned TLS connection,
brokering SSH signatures / cloud credentials to your remote shed. It picks up
`api_url` and `tls_cert_fingerprint` from the same `~/.shed/config.yaml` entry.

The bus stream is long-lived and often idle; shed-server sends a periodic SSE
keepalive comment so an idle NAT or proxy does not evict the connection. If the
agent reconnects across a blip, any un-acked credential request is re-delivered.

## 4. Rotation and expiry

Tokens are short-lived (`auth.token_ttl`, default 24h) and refresh themselves:
the CLI re-bootstraps near expiry and on a `401`, and the host-agent refreshes on
a timer. You never rotate a token by hand. To **revoke** access, remove the key
from the allowlist (drop it from `github_users`, or from the GitHub account); the
server purges that key's tokens on the next allowlist refresh.

Rotate the TLS cert (e.g. after changing `tls_names`) and re-pin clients:

```bash
shed server update shed.example.com --refetch   # fetch the new cert + re-pin
```

## Hardening the add-time trust

`shed server add` closes the add-time MITM window by prompting you to confirm the
SSH and TLS fingerprints. Tighten it further by verifying out-of-band
(`--fingerprint` / `--tls-fingerprint`, read from the server's startup log), or
bring your own TLS certificate instead of the self-signed one with
`tls_cert_file` / `tls_key_file` in the server config.

## Co-located host-agent

If you instead run the host-agent **on the VPS itself**, no extra config is
needed: the credential bus (`credentials` scope) and Connect tunnel (`control` or
`credentials`) ride the single pinned-TLS listener, and the on-box host-agent reaches them over
`https://127.0.0.1:8443` with the pinned cert. Remote `shed forward` keeps
working at the same time, because there is no longer a separate loopback-only
listener. See the [network surface](../reference/security.md#network-surface).
