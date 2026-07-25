# Security Configuration

shed is **local-development-first**: out of the box (the brew/apt install) it
runs localhost-only. Networked access is opt-in. Its network security is two
independent choices:

1. **Posture** — `auth.mode: open | token`. *Open* trusts the network (plain
   HTTP, no tokens, no TLS). *Token* trusts nothing (pinned TLS + bearer tokens
   + an SSH key allowlist), and is **TLS-only** — it serves no plain-HTTP
   listener at all. (`secure` is accepted as a deprecated alias for `token`.)
2. **Where listeners bind** — `bind_address` governs every listener (HTTP,
   HTTPS, SSH). It **defaults to loopback (`127.0.0.1`)** in both modes, so the
   server is reachable only on this machine until you set it to a routable
   address.

Think of it as two deployment shapes:

- **Local development** (the default) — localhost-only, either open (the
  installed default) or token. Cases 1 and 2 below.
- **Remote-facing** — reachable across the network. Token is the preferred
  posture (TLS + tokens); open is allowed only with an explicit `bind_address`
  *and* `allow_insecure_exposure: true`. Cases 3 and 4 below.

This guide walks the common shapes those two choices produce. For the underlying
model — the invariants, the credential bus, and the exact connection flow — see
the [Security reference](../reference/security.md). For a full internet-facing
walkthrough, see [Public VPS Deployment](vps-deployment.md). Upgrading from a
pre-v0.7.4 server (the loopback default is new)? See
[Upgrading v0.7.3 → v0.7.4](../upgrades/v0.7.3-to-v0.7.4.md).

Server settings live in `server.yaml` (`/etc/shed/server.yaml` on Linux,
`~/.config/shed/server.yaml` or the Homebrew `…/etc/shed/server.yaml` on macOS).
Client settings are written per server into `~/.shed/config.yaml` by
`shed server add`.

## Choosing a posture

| Question | If yes |
|----------|--------|
| Is everything on **one machine**, and do you want zero auth ceremony? | **Open + loopback** (Case 1) — the installed default |
| One machine, but you want TLS + key checking anyway (shared box, or mirror prod locally)? | **Token + loopback** (Case 2) |
| Reachable **over the network** / internet-facing? | **Token, all interfaces** (Case 3) — the preferred networked posture |
| Reachable over the network, but a **trusted private network only** and you accept plaintext? | **Open on a LAN** (Case 4) — needs `allow_insecure_exposure: true` |

Two invariants hold across every token-mode deployment, and explain the startup
rejections you may hit (see [Troubleshooting](#troubleshooting-startup-rejections)):

- **tokens ⟺ TLS ⟺ token** — HTTP token enforcement only exists in token mode,
  and token mode is always TLS.
- **https ⟺ token** — `https_port` is valid only in token mode. So a client (or
  the host-agent) can treat an `https://` `api_url` as proof the server enforces
  tokens.

---

## Case 1 — Local only, simplest (open + loopback)

**For:** a developer running shed-server and the CLI on the same machine, who
wants nothing exposed to the LAN and no SSH allowlist / token / TLS ceremony.

`server.yaml`:

```yaml
auth:
  mode: open             # the default — may be omitted
bind_address: 127.0.0.1  # the default — every listener (HTTP + SSH) binds loopback only
```

`bind_address: 127.0.0.1` is the default, so this is also what you get with an
empty `server.yaml`. Add the server (plain HTTP — no flags):

```bash
shed server add localhost
```

**You get:** plain HTTP + SSH bound to loopback. Nothing reaches the LAN; no
tokens, no TLS, no key allowlist. **Trade-off:** any local user on the box can
reach the API — loopback *is* the trust boundary. Right for a single-user laptop.

---

## Case 2 — Local only, hardened (token + loopback)

**For:** one machine, but you want full TLS and an SSH key allowlist even locally
— a shared/multi-user host where loopback isn't a sufficient boundary, or to
mirror a production token-mode config on your laptop.

`server.yaml`:

```yaml
auth:
  mode: token
  ssh:
    authorized_keys:
      - ssh-ed25519 AAAA…your-key… you@laptop   # the key you SSH/​bootstrap with
bind_address: 127.0.0.1   # the default — HTTPS + SSH bind loopback only
# https_port defaults to 8443 in token mode
```

Add the server over TLS (pins the self-signed cert, mints a token over SSH):

```bash
shed server add localhost --https-port 8443
```

**You get:** TLS-only on `127.0.0.1:8443`, SSH allowlist enforced, bearer tokens
enforced — nothing plaintext, nothing on the LAN. **Trade-off:** token mode
**requires** an SSH key source, so you must list your key (`authorized_keys`
here, or `github_users: [you]`); clients pin the cert. More setup than Case 1,
in exchange for defense-in-depth on the box itself.

---

## Case 3 — Remote, internet-facing (token, all interfaces)

**For:** a VPS or remote host reachable over the network — only your keys may
connect, and all traffic is encrypted.

`server.yaml`:

```yaml
auth:
  mode: token
  ssh:
    github_users: [charliek]   # only these GitHub keys may SSH in (and mint tokens)
tls_names:
  - shed.example.com           # your public hostname / IP (extra cert SANs)
bind_address: 0.0.0.0          # token mode defaults to loopback too — set this to face the network
# https_port defaults to 8443
```

Token mode defaults to loopback like every other posture, so a remote server
**must set `bind_address`** explicitly (`0.0.0.0`/`*` for all IPv4, `::` for all
interfaces, or a specific public/tailnet IP) — otherwise it is unreachable.
Token mode needs **no** `allow_insecure_exposure` ack to bind the network: TLS +
tokens already make it safe.

From your laptop:

```bash
shed server add shed.example.com --https-port 8443
```

This fetches the cert + SSH host key, shows both fingerprints for confirmation,
pins them, then mints a token over the `_bootstrap` SSH channel — no token to
paste. **You get:** HTTPS on all interfaces (`8443`), SSH allowlist, tokens; no
plaintext anywhere. A co-located host-agent (one running on the same box) reaches
the credential bus over the same pinned-TLS listener at
`https://127.0.0.1:8443`, gated by the `credentials` scope. See
[Public VPS Deployment](vps-deployment.md) for the full flow, out-of-band
fingerprint verification, and rotation.

---

## Case 4 — Remote, open on a trusted LAN (open + `allow_insecure_exposure`)

**For:** a server reachable across a **trusted private network only** (a
Tailscale tailnet or a closed LAN), where you accept plaintext and want no token
/ TLS ceremony. **Prefer token mode (Case 3) for anything networked** — this case
has no transport security, so use it only when the network itself is the trust
boundary.

`server.yaml`:

```yaml
auth:
  mode: open               # plain HTTP, no tokens, no TLS
bind_address: 0.0.0.0      # face the network (or a specific tailnet/LAN IP)
allow_insecure_exposure: true   # required: acknowledge a non-loopback bind with no transport security
```

Binding a non-loopback interface in **open** mode requires
`allow_insecure_exposure: true` — without it the server refuses to start, because
open mode would otherwise put plaintext on the network silently. (Token mode
needs no such ack.) Add the server (plain HTTP — no flags):

```bash
shed server add my-host.tailnet.ts.net --name my-host
```

**You get:** plain HTTP + SSH on the LAN, no tokens, no TLS. **Trade-off:**
everything is plaintext and any host that can reach the address can reach the
API — the private network *is* the trust boundary. Move to token mode (Case 3)
the moment the server faces anything wider.

---

## Staging the SSH allowlist with `warn` (avoid locking yourself out)

`auth.mode: token` forces the SSH allowlist to `enforce` — and a wrong or
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
`auth.mode: token` (which forces `enforce`). This is the same pattern as
SELinux `permissive` or CSP report-only — a dry run that proves the policy before
it blocks. `warn` is valid only in open mode; token mode always enforces.

## Is anything plaintext in token mode?

No — nothing, anywhere. Token mode serves **no** plain-HTTP listener at all (not
even on loopback): a single pinned-TLS listener faces clients, and a client that
holds a pin but is handed a non-`https` URL fails closed rather than send
plaintext. The SSH channel (shed sessions and the token mint) is encrypted with
the host key pinned in `known_hosts`. The only trust-on-first-use moment is
`shed server add` showing you the cert fingerprint to confirm (or pass
`--tls-fingerprint` / `--fingerprint` to verify out-of-band). See the
[connection-flow table](../reference/security.md#connection-flow-whats-encrypted-where).

The credential bus (`credentials` scope) and Connect tunnel (`control` or
`credentials`) ride that same TLS listener — so a co-located host-agent reaches
them over `https://127.0.0.1:8443` with the pinned cert. There is no plaintext
channel, implicit or opt-in.

## Troubleshooting startup rejections

Token mode refuses to start half-configured, and the simplification removed the
footgun states — so these configs are **rejected at startup** (the server names
the gap and exits):

| Message names… | Cause | Fix |
|----------------|-------|-----|
| `auth.mode: token requires … an SSH key source` | token mode with no `github_users` / `authorized_keys` / `authorized_keys_file` | Add a key source (Cases 2–4). |
| `https_port requires auth.mode: token` | `https_port` set under `open` | Use `token` (it defaults `https_port` to 8443), or drop `https_port`. |
| `auth.ssh.mode: enforce requires auth.mode: token` | enforcing the allowlist without token mode | Use `token`, or `warn` to stage (above). |
| `auth.mode: token forces auth.ssh.mode: enforce` | an explicit `off`/`warn` under token mode | Remove `auth.ssh.mode` — token mode derives `enforce`. |
| `config key "auth.http" was removed` | a leftover `auth.http` block | Delete it — token enforcement derives from `auth.mode: token`. |
| `config key "http_bind"/"ssh_bind" was removed` | a leftover `http_bind`/`ssh_bind` | Replace both with a single `bind_address`. |
| `config key "internal_http_port" was removed` | a leftover `internal_http_port` | Delete it — the bus + Connect tunnel ride the single listener (Case 3). |
| `bind_address … requires allow_insecure_exposure` | a non-loopback `bind_address` in **open** mode | Add `allow_insecure_exposure: true` (Case 4), or switch to `token` mode (Case 3). |

## Quick reference

| Case | `auth.mode` | `bind_address` | TLS | `shed server add` |
|------|-------------|----------------|-----|-------------------|
| 1 — local simple | `open` | `127.0.0.1` (default) | none | `shed server add localhost` |
| 2 — local hardened | `token` | `127.0.0.1` (default) | loopback `:8443` | `shed server add localhost --https-port 8443` |
| 3 — remote (preferred) | `token` | `0.0.0.0` | all ifaces `:8443` | `shed server add <host> --https-port 8443` |
| 4 — remote open on LAN | `open` + `allow_insecure_exposure` | `0.0.0.0` | none | `shed server add <host>` |
