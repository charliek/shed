# Public VPS Deployment

This guide deploys shed-server on an internet-facing VPS, locked down so that
**only your GitHub keys** can SSH in, **all HTTP is encrypted and
token-authenticated**, and the **credential bus is reachable only with a
credentials token over TLS**.

The default shed posture is open-on-a-trusted-network (Tailscale/LAN). This
guide is the opposite end: a hardened, internet-facing server. The
[`public_exposure`](../reference/security.md#public-exposure-preflight) flag
makes the hardening an all-or-nothing bundle — the server refuses to start if
any piece is missing.

## 1. Mint tokens

On the VPS, generate one token per scope:

```bash
shed-server token new --scope control       # for your CLI / desktop
shed-server token new --scope credentials   # for the host-agent (credential bus)
```

Keep the output — you'll paste it into both the server config and your client
config.

## 2. Server config

Write `/etc/shed/server.yaml` with the full bundle:

```yaml
public_exposure: true            # refuse to start without the bundle below

auth:
  ssh:
    mode: enforce                # only allowlisted keys may SSH in
    github_users: [charliek]     # seeded from https://github.com/charliek.keys
  http:
    mode: enforce                # every HTTP request needs a bearer token
    tokens:
      - { name: cli,        scope: control,     token: shed_control_xxxxx }
      - { name: host-agent, scope: credentials, token: shed_credentials_xxxxx }

https_port: 8443                 # TLS on the public interface
tls_names:
  - shed.example.com             # your VPS hostname / public IP (cert SANs)
```

With `public_exposure: true`, shed-server forces the plain-HTTP listener to
loopback, so the only network-facing API is HTTPS on `8443`. The credential bus
and Connect tunnel stay on that listener, gated by the `credentials` scope.

Start (or restart) the server. If anything in the bundle is missing it exits
immediately, naming the gap:

```text
public_exposure preflight: public_exposure requires https_port (TLS must be on)
```

## 3. Add the server from your client

From your laptop, pin the server's TLS cert and SSH host key:

```bash
shed server add shed.example.com --https-port 8443
```

This fetches the TLS certificate, shows its fingerprint alongside the SSH
host-key fingerprint, prompts you to trust them, and writes the pinned entry to
`~/.shed/config.yaml`. Add your tokens to that entry:

```yaml
servers:
  shed.example.com:
    api_url: https://shed.example.com:8443
    tls_cert_fingerprint: sha256:<pinned at add time>
    control_token:     shed_control_xxxxx
    credentials_token: shed_credentials_xxxxx
```

Verify the control plane works over the pinned TLS connection:

```bash
shed -s shed.example.com list
```

To verify out-of-band (e.g. from a CI runner), pass the expected fingerprints:

```bash
shed server add shed.example.com --https-port 8443 \
  --tls-fingerprint sha256:<hex> --fingerprint SHA256:<ssh>
```

## 4. Credential brokering over TLS

Point the host-agent (running on your laptop) at the server's `api_url` with the
`credentials` token. It subscribes to the credential bus over the pinned TLS
connection and brokers SSH signatures / cloud credentials to your remote shed —
the host-agent config picks up `api_url`, `tls_cert_fingerprint`, and
`credentials_token` from the same `~/.shed/config.yaml` entry.

The bus stream is long-lived and often idle; shed-server sends a periodic
SSE keepalive comment so an idle NAT or proxy does not evict the connection. No
client configuration is needed.

## 5. Rotation

Rotate the TLS cert (e.g. after changing `tls_names`) and re-pin clients:

```bash
shed server update shed.example.com --refetch   # fetch the new cert + re-pin
```

Rotate a token by minting a new one, updating both the server `tokens` list and
the client entry, and restarting the server.

## Co-located alternative

If you instead run the host-agent **on the VPS itself**, set
`internal_http_port: 8081` to move the credential bus to a loopback-only
listener. Note this also moves the Connect tunnel to loopback, so remote
`shed forward` is no longer available — use the co-located split only when the
host-agent is on the same box. See the
[route matrix](../reference/security.md#route-matrix).
