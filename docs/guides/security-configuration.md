# Security Configuration

shed's network security is two independent choices:

1. **Posture** — `auth.mode: open | secure`. *Open* trusts the network (plain
   HTTP, no tokens, no TLS). *Secure* trusts nothing (pinned TLS + bearer tokens
   + an SSH key allowlist), and is **TLS-only** — it serves no plain-HTTP
   listener at all.
2. **Where listeners bind** — `http_bind` / `ssh_bind` (and the optional
   `internal_http_port`). This decides whether the server is reachable only on
   `127.0.0.1` (this machine) or across the network.

This guide walks the common shapes those two choices produce. For the underlying
model — the invariants, the credential bus, and the exact connection flow — see
the [Security reference](../reference/security.md). For a full internet-facing
walkthrough, see [Public VPS Deployment](vps-deployment.md).

Server settings live in `server.yaml` (`/etc/shed/server.yaml` on Linux,
`~/.config/shed/server.yaml` or the Homebrew `…/etc/shed/server.yaml` on macOS).
Client settings are written per server into `~/.shed/config.yaml` by
`shed server add`.

## Choosing a posture

| Question | If yes |
|----------|--------|
| Is everything on **one machine**, and do you want zero auth ceremony? | **Open + loopback** (Case 1) |
| One machine, but you want TLS + key checking anyway (shared box, or mirror prod locally)? | **Secure + loopback** (Case 2) |
| Reachable **over the network** / internet-facing? | **Secure, all interfaces** (Case 3) |
| Network-facing **and** the credential host-agent runs on the same box? | **Secure + `internal_http_port`** (Case 4) |

Two invariants hold across every secure deployment, and explain the startup
rejections you may hit (see [Troubleshooting](#troubleshooting-startup-rejections)):

- **tokens ⟺ TLS ⟺ secure** — HTTP token enforcement only exists in secure mode,
  and secure mode is always TLS.
- **https ⟺ secure** — `https_port` is valid only in secure mode. So a client (or
  the host-agent) can treat an `https://` `api_url` as proof the server enforces
  tokens.

---

## Case 1 — Local only, simplest (open + loopback)

**For:** a developer running shed-server and the CLI on the same machine, who
wants nothing exposed to the LAN and no SSH allowlist / token / TLS ceremony.

`server.yaml`:

```yaml
auth:
  mode: open        # the default — may be omitted
http_bind: 127.0.0.1   # plain-HTTP API reachable only on this machine
ssh_bind: 127.0.0.1    # SSH (shed sessions) reachable only on this machine
```

Add the server (plain HTTP — no flags):

```bash
shed server add localhost
```

**You get:** plain HTTP + SSH bound to loopback. Nothing reaches the LAN; no
tokens, no TLS, no key allowlist. **Trade-off:** any local user on the box can
reach the API — loopback *is* the trust boundary. Right for a single-user laptop.

---

## Case 2 — Local only, hardened (secure + loopback)

**For:** one machine, but you want full TLS and an SSH key allowlist even locally
— a shared/multi-user host where loopback isn't a sufficient boundary, or to
mirror a production secure config on your laptop.

`server.yaml`:

```yaml
auth:
  mode: secure
  ssh:
    authorized_keys:
      - ssh-ed25519 AAAA…your-key… you@laptop   # the key you SSH/​bootstrap with
http_bind: 127.0.0.1   # the HTTPS listener binds here → loopback-only TLS
ssh_bind: 127.0.0.1
# https_port defaults to 8443 in secure mode
```

Add the server over TLS (pins the self-signed cert, mints a token over SSH):

```bash
shed server add localhost --https-port 8443
```

**You get:** TLS-only on `127.0.0.1:8443`, SSH allowlist enforced, bearer tokens
enforced — nothing plaintext, nothing on the LAN. **Trade-off:** secure mode
**requires** an SSH key source, so you must list your key (`authorized_keys`
here, or `github_users: [you]`); clients pin the cert. More setup than Case 1,
in exchange for defense-in-depth on the box itself.

---

## Case 3 — Remote, internet-facing (secure, all interfaces)

**For:** a VPS or remote host reachable over the network — only your keys may
connect, and all traffic is encrypted.

`server.yaml`:

```yaml
auth:
  mode: secure
  ssh:
    github_users: [charliek]   # only these GitHub keys may SSH in (and mint tokens)
tls_names:
  - shed.example.com           # your public hostname / IP (extra cert SANs)
# http_bind empty → HTTPS binds all interfaces; https_port defaults to 8443
```

From your laptop:

```bash
shed server add shed.example.com --https-port 8443
```

This fetches the cert + SSH host key, shows both fingerprints for confirmation,
pins them, then mints a token over the `_bootstrap` SSH channel — no token to
paste. **You get:** HTTPS on all interfaces (`8443`), SSH allowlist, tokens; no
plaintext anywhere. See [Public VPS Deployment](vps-deployment.md) for the full
flow, out-of-band fingerprint verification, and rotation.

---

## Case 4 — Remote + co-located host-agent (secure + `internal_http_port`)

**For:** a remote secure server where the credential **host-agent runs on the
same box**. The credential bus should stay on loopback rather than ride the
public TLS listener.

`server.yaml`:

```yaml
auth:
  mode: secure
  ssh:
    github_users: [charliek]
tls_names: [shed.example.com]
internal_http_port: 8081   # bus + Connect tunnel move to a loopback-only listener
```

Clients add the server exactly as in Case 3 (the control plane is unchanged):

```bash
shed server add shed.example.com --https-port 8443
```

The co-located host-agent reaches the bus over `http://127.0.0.1:8081` (loopback,
on-box). **You get:** a public pinned-TLS control plane *plus* an on-box loopback
bus. **Trade-off (important):** `internal_http_port` also moves the Connect
tunnel to loopback, so **remote `shed forward` stops working** — use this only
when the host-agent is on the same machine as shed-server. With it unset (Case 3),
the bus + tunnel ride the public TLS listener, gated by the `credentials` scope,
which is what a **remote** host-agent / `shed forward` needs.

---

## Staging the SSH allowlist with `warn` (avoid locking yourself out)

`auth.mode: secure` forces the SSH allowlist to `enforce` — and a wrong or
incomplete allowlist on a remote host locks you out (recovery means editing
`server.yaml` from the provider's console). The safe rollout uses `warn` as a
**pre-flight**, in open mode, before you commit:

```yaml
auth:
  mode: open
  ssh:
    mode: warn                 # consult the allowlist, LOG would-deny, but still accept
    github_users: [charliek]
```

Restart, then watch the log for `would-deny` lines while you (and your CI, and
the host-agent) connect. Once nothing legitimate is denied, switch to
`auth.mode: secure` (which forces `enforce`). This is the same pattern as
SELinux `permissive` or CSP report-only — a dry run that proves the policy before
it blocks. `warn` is valid only in open mode; secure always enforces.

## Is anything plaintext in secure mode?

No. Secure mode serves **no** plain-HTTP listener — only the pinned-TLS listener
faces clients, and a client that holds a pin but is handed a non-`https` URL
fails closed rather than send plaintext. The SSH channel (shed sessions and the
token mint) is encrypted with the host key pinned in `known_hosts`. The only
trust-on-first-use moment is `shed server add` showing you the cert fingerprint
to confirm (or pass `--tls-fingerprint` / `--fingerprint` to verify out-of-band).
See the [connection-flow table](../reference/security.md#connection-flow-whats-encrypted-where).

The one exception is deliberate and opt-in: `internal_http_port` (Case 4) is a
loopback-only plaintext listener for a co-located bus. There is no implicit
plaintext channel.

## Troubleshooting startup rejections

Secure mode refuses to start half-configured, and the simplification removed the
footgun states — so these configs are **rejected at startup** (the server names
the gap and exits):

| Message names… | Cause | Fix |
|----------------|-------|-----|
| `auth.mode: secure requires … an SSH key source` | secure with no `github_users` / `authorized_keys` / `authorized_keys_file` | Add a key source (Cases 2–4). |
| `https_port requires auth.mode: secure` | `https_port` set under `open` | Use `secure` (it defaults `https_port` to 8443), or drop `https_port`. |
| `auth.ssh.mode: enforce requires auth.mode: secure` | enforcing the allowlist without secure | Use `secure`, or `warn` to stage (above). |
| `auth.mode: secure forces auth.ssh.mode: enforce` | an explicit `off`/`warn` under secure | Remove `auth.ssh.mode` — secure derives `enforce`. |
| `config key "auth.http" was removed` | a leftover `auth.http` block | Delete it — token enforcement derives from `auth.mode: secure`. |

## Quick reference

| Case | `auth.mode` | `http_bind` | TLS | `shed server add` |
|------|-------------|-------------|-----|-------------------|
| 1 — local simple | `open` | `127.0.0.1` | none | `shed server add localhost` |
| 2 — local hardened | `secure` | `127.0.0.1` | loopback `:8443` | `shed server add localhost --https-port 8443` |
| 3 — remote | `secure` | (all) | all ifaces `:8443` | `shed server add <host> --https-port 8443` |
| 4 — remote + co-located agent | `secure` + `internal_http_port` | (all) | all ifaces `:8443` | `shed server add <host> --https-port 8443` |
