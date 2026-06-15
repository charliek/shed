# Public VPS Deployment

This guide deploys shed-server on an internet-facing VPS, locked down so that
**only your GitHub keys** can SSH in, **all HTTP is encrypted and
token-authenticated**, and the **credential bus is reachable only with a
credentials token over TLS**.

The default shed posture is open-on-a-trusted-network (Tailscale/LAN). This
guide is the opposite end: a hardened, internet-facing server. One switch —
[`auth.mode: secure`](../reference/security.md#secure-mode) — derives the whole
hardening bundle (SSH allowlist enforced + HTTP tokens enforced + TLS + loopback
plain-HTTP) and **refuses to start** if any piece is missing. There are no tokens
to mint or paste: clients get them automatically over SSH.

## 1. Server config

Write `/etc/shed/server.yaml`. The only thing `secure` requires from you is an
SSH key source:

```yaml
auth:
  mode: secure                   # derives the full bundle; refuses to start without a key source
  ssh:
    github_users: [charliek]     # only these GitHub keys may SSH in (and mint tokens)
tls_names:
  - shed.example.com             # your VPS hostname / public IP (extra cert SANs)
```

`secure` mode forces `auth.ssh.mode: enforce`, enforces HTTP bearer tokens, turns
on pinned TLS (`https_port` defaults to `8443`), and binds the plain-HTTP
listener to loopback — so the only network-facing API is HTTPS on `8443`, with
the credential bus and Connect tunnel gated by the `credentials` scope.

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

## Co-located alternative

If you instead run the host-agent **on the VPS itself**, set
`internal_http_port: 8081` to move the credential bus to a loopback-only
listener. Note this also moves the Connect tunnel to loopback, so remote
`shed forward` is no longer available — use the co-located split only when the
host-agent is on the same box. See the
[route matrix](../reference/security.md#route-matrix).
