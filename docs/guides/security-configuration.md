# Security Configuration

shed is **local-development-first**: out of the box (the brew/apt install) it
runs localhost-only. Networked access is opt-in. Its network security is two
independent choices:

1. **Posture** — `auth.mode: open | token | mtls`. *Open* trusts the network
   (plain HTTP, no credential, no TLS). *Token* trusts nothing (pinned TLS +
   bearer tokens + an SSH key allowlist), and is **TLS-only** — it serves no
   plain-HTTP listener at all. *mtls* is the same hardened bundle with the
   bearer token replaced by a client certificate bound to a key that never
   leaves the client's device — the **recommended posture for anything
   internet-facing**. (`secure` is accepted as a deprecated alias for `token`.)
2. **Where listeners bind** — `bind_address` governs every listener (HTTP,
   HTTPS, SSH). It **defaults to loopback (`127.0.0.1`)** in every mode, so the
   server is reachable only on this machine until you set it to a routable
   address.

Separately from both, you can limit what each **shed** reaches: no server
mounts, and on Firecracker an internet-only network. That works in every
posture, a localhost-only laptop included; see [Hardening the sheds
themselves](#hardening-the-sheds-themselves).

Think of it as two deployment shapes:

- **Local development** (the default) — localhost-only, open (the installed
  default), token, or mtls. Cases 1 and 2 below.
- **Remote-facing** — reachable across the network. mtls or token is the
  preferred posture (TLS + a credential); open is allowed only with an
  explicit `bind_address` *and* `allow_insecure_exposure: true`. Cases 3, 3b,
  and 4 below.

This guide walks the common shapes those two choices produce. For the underlying
model — the invariants, the credential bus, and the exact connection flow — see
the [Security reference](../reference/security.md). For a full internet-facing
walkthrough, see [Public VPS Deployment](vps-deployment.md). Upgrading an
existing token-mode server to mtls, or upgrading from a pre-v0.7.4 server (the
loopback default is new)? See [Upgrading to mTLS](../upgrades/token-to-mtls.md)
and [Upgrading v0.7.3 → v0.7.4](../upgrades/v0.7.3-to-v0.7.4.md).

Server settings live in `server.yaml` (`/etc/shed/server.yaml` on Linux,
`~/.config/shed/server.yaml` or the Homebrew `…/etc/shed/server.yaml` on macOS).
Client settings are written per server into `~/.shed/config.yaml` by
`shed server add`.

## Choosing a posture

| Question | If yes |
|----------|--------|
| Is everything on **one machine**, and do you want zero auth ceremony? | **Open + loopback** (Case 1) — the installed default |
| One machine, but you want TLS + key checking anyway (shared box, or mirror prod locally)? | **Token + loopback** (Case 2) |
| Reachable **over the network** / internet-facing, and `curl`/scripts/CI need **admin** access to the API with a bearer header? | **Token, all interfaces** (Case 3) |
| Reachable **over the network** / internet-facing, and every caller is part of the shed client fleet (CLI, desktop, host-agent, mobile)? | **mtls, all interfaces** (Case 3b) — the **recommended** networked posture |
| Reachable over the network, and an orchestrator (CI, an agent runner) needs to create and drive **only its own sheds**? | **mtls** (Case 3b) with a [service client](orchestrated-sheds.md) |
| Reachable over the network, but a **trusted private network only** and you accept plaintext? | **Open on a LAN** (Case 4) — needs `allow_insecure_exposure: true` |

Two invariants hold across every token- and mtls-mode deployment, and explain
the startup rejections you may hit (see
[Troubleshooting](#troubleshooting-startup-rejections)):

- **credential ⟺ TLS ⟺ enforced mode** — HTTP credential enforcement (a bearer
  token in `token` mode, a client certificate in `mtls` mode) only exists in an
  enforced mode, and an enforced mode is always TLS.
- **https ⟺ enforced mode** — `https_port` is valid only in `token`/`mtls`
  mode. So a client (or the host-agent) can treat an `https://` `api_url` as
  proof the server enforces a credential.

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

**When to prefer Case 3b instead:** token mode is the right call whenever
`curl`, a CI runner, or any third-party integration needs **admin** access to
the API with a bearer header — an mtls server has no bearer credential. An
orchestrator that needs only its own sheds does not need token mode: on an
mtls server it can be a [service client](orchestrated-sheds.md), whose
certificate bundle works with `curl --cert`.

---

## Case 3b — Remote, internet-facing, mtls (recommended)

**For:** the same VPS/remote-host shape as Case 3, but every caller is part of
the shed client fleet (CLI, desktop, host-agent, mobile) and you want the
credential itself to be a key-bound certificate rather than a bearer
instrument. This is the **recommended posture for anything internet-facing**.

`server.yaml`:

```yaml
auth:
  mode: mtls
  ssh:
    github_users: [charliek]   # only these GitHub keys may SSH in (and enroll certificates)
tls_names:
  - shed.example.com           # your public hostname / IP (extra cert SANs)
bind_address: 0.0.0.0          # mtls mode defaults to loopback too — set this to face the network
# https_port defaults to 8443
```

Every invariant from Case 3 holds identically (loopback-by-default, no
`allow_insecure_exposure` ack needed, `github_users`/`authorized_keys`
required). What's different is what a client presents: instead of a bearer
token, `shed server add` generates a private key locally, sends a certificate
signing request over the `_bootstrap` SSH channel, and the server's small
internal CA signs it. The private key never leaves the client:

```bash
shed server add shed.example.com --ssh-port 2222 --trust-on-first-use
```

**You get:** the same TLS-only, SSH-allowlisted surface as Case 3, but an
unauthenticated peer cannot even complete the HTTPS handshake — the server
demands a client certificate as part of `RequireAndVerifyClientCert`
(`curl -k` with no certificate gets no HTTP response at all, not even a 401).
**Trade-off:** revoking access means removing the SSH key (coarser than
token mode's per-token revoke — it also cuts SSH/SFTP), and `curl`/scripts
cannot reach the server as an admin. A script or orchestrator that needs only
its own sheds can be a [service client](orchestrated-sheds.md) instead. See
[mTLS mode](../reference/security.md#mtls-mode) for the full guarantee, the
per-request re-validation, and the accepted limitations, and
[Upgrading to mTLS](../upgrades/token-to-mtls.md) for flipping an existing
token-mode server over.

---

## Case 4 — Remote, open on a trusted LAN (open + `allow_insecure_exposure`)

**For:** a server reachable across a **trusted private network only** (a
Tailscale tailnet or a closed LAN), where you accept plaintext and want no
credential / TLS ceremony. **Prefer mtls (Case 3b) or token (Case 3) mode for
anything networked** — this case has no transport security, so use it only
when the network itself is the trust boundary.

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
API — the private network *is* the trust boundary. Move to mtls (Case 3b) or
token mode (Case 3) the moment the server faces anything wider.

---

## Hardening the sheds themselves

The posture decides who reaches the server. What a shed itself can reach is
set per shed, in any posture:

```bash
# No server mounts, and (Firecracker) the internet and nothing else
shed create untrusted --repo someone/tool --mounts none --network isolated
shed list -v      # the MOUNTS column shows none, NETWORK shows isolated
```

- **`--mounts none`** (or `--mounts claude,gh`) picks which of the server's
  `mounts:` the shed gets. Without the flag it gets all of them, as before. The
  choice is stored and applied on every start. See [Security › Mount
  selection](../reference/security.md#mount-selection).
- **`--network isolated`** (Firecracker only) lets the shed reach the internet
  and nothing else: not the host on any of its addresses, other sheds, the LAN
  (RFC 1918), the tailnet (`100.64.0.0/10`), link-local addresses and the cloud
  metadata service, and no IPv6. shed-server enforces it with its own nftables
  tables, so a process in the guest cannot opt out. See [Security › Network
  profiles](../reference/security.md#network-profiles).
- **Use both.** On Firecracker, an `open` shed with no mounts can still reach
  another running shed's 9P port on the bridge, so leaving out mounts is not a
  boundary by itself.

To make `isolated` the default for every create on a Firecracker server:

```yaml
network:
  default_profile: isolated    # a create without --network gets this; --network open opts out
  allow_cidrs:
    - 192.168.1.20/32          # optional: private addresses the sheds still need (a LAN registry)
```

`allow_cidrs` opens only routed destinations. It never opens the host itself
or other sheds.

**Requirements.** An isolated shed needs the `nft` binary on the server's
`PATH` and shed-server running as root, which the deb package's systemd unit
does. Without them the create or start is refused with
`503 NETWORK_ENFORCEMENT_UNAVAILABLE`; a server with only open sheds needs
neither. See [Firecracker setup › Prerequisites](../getting-started/fc-setup.md#prerequisites).

**VZ (macOS) supports only `open`.** `--network isolated` is refused there
(`422 NETWORK_PROFILE_UNSUPPORTED`), and a VZ server refuses
`network.default_profile: isolated` at load. `--mounts` works on both backends.

**Egress profiles add domain filtering on top.** The isolated profile decides
what any process in the shed can reach; an [egress profile](../reference/egress.md)
decides which internet hosts a cooperating client (one that honours
`HTTP_PROXY`) may use, and audits it. See [Egress Control › With the isolated
network profile](../reference/egress.md#with-the-isolated-network-profile).

---

## Staging the SSH allowlist with `warn` (avoid locking yourself out)

`auth.mode: token`/`mtls` force the SSH allowlist to `enforce` — and a wrong or
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
`auth.mode: token` or `auth.mode: mtls` (either forces `enforce`). This is the
same pattern as SELinux `permissive` or CSP report-only — a dry run that
proves the policy before it blocks. `warn` is valid only in open mode; token
and mtls mode always enforce.

## Is anything plaintext in token or mtls mode?

No — nothing, anywhere. Both modes serve **no** plain-HTTP listener at all (not
even on loopback): a single pinned-TLS listener faces clients, and a client
that holds a pin but is handed a non-`https` URL fails closed rather than send
plaintext. The SSH channel (shed sessions and the credential issuance channel)
is encrypted with the host key pinned in `known_hosts`. The only
trust-on-first-use moment is `shed server add` showing you the cert
fingerprint(s) to confirm (or pass `--tls-fingerprint` / `--fingerprint` to
verify out-of-band). See the
[connection-flow table](../reference/security.md#connection-flow-whats-encrypted-where).

The credential bus (`credentials` scope) and Connect tunnel (`control` or
`credentials`) ride that same TLS listener — so a co-located host-agent reaches
them over `https://127.0.0.1:8443` with the pinned cert (in mtls mode, its own
client certificate). There is no plaintext channel, implicit or opt-in.

## Troubleshooting startup rejections

Token and mtls mode refuse to start half-configured, and the simplification
removed the footgun states — so these configs are **rejected at startup** (the
server names the gap and exits):

| Message names… | Cause | Fix |
|----------------|-------|-----|
| `auth.mode: token requires … an SSH key source` (or `auth.mode: mtls requires …`) | token/mtls mode with no `github_users` / `authorized_keys` / `authorized_keys_file` | Add a key source (Cases 2–4). |
| `https_port requires auth.mode: token or mtls` | `https_port` set under `open` | Use `token`/`mtls` (either defaults `https_port` to 8443), or drop `https_port`. |
| `auth.ssh.mode: enforce requires auth.mode: token or mtls` | enforcing the allowlist without an enforced mode | Use `token`/`mtls`, or `warn` to stage (above). |
| `auth.mode: token forces auth.ssh.mode: enforce` (or `auth.mode: mtls forces …`) | an explicit `off`/`warn` under an enforced mode | Remove `auth.ssh.mode` — token/mtls mode derives `enforce`. |
| `config key "auth.http" was removed` | a leftover `auth.http` block | Delete it — token enforcement derives from `auth.mode: token`. |
| `config key "http_bind"/"ssh_bind" was removed` | a leftover `http_bind`/`ssh_bind` | Replace both with a single `bind_address`. |
| `config key "internal_http_port" was removed` | a leftover `internal_http_port` | Delete it — the bus + Connect tunnel ride the single listener (Case 3/3b). |
| `bind_address … requires allow_insecure_exposure` | a non-loopback `bind_address` in **open** mode | Add `allow_insecure_exposure: true` (Case 4), or switch to `token`/`mtls` mode (Case 3/3b). |
| `invalid auth.mode: … (must be open, token, or mtls)` | a typo or unsupported value in `auth.mode` | Use `open`, `token`, or `mtls` (`secure` is also accepted, as a deprecated alias for `token`). |
| `auth.clients requires auth.mode: mtls` | [service clients](orchestrated-sheds.md) declared in `open` or `token` mode | Switch to `auth.mode: mtls` (Case 3b), or remove `auth.clients`. |
| `invalid auth.clients name …`, or `auth.clients.<name>.policy.name_prefix is required` (likewise `max_sheds is required and must be at least 1`, `max_age is required and must be positive`) | a client name that is not lowercase letters, digits and hyphens, or a required policy field missing or zero | Fix the name, or set the field (see [Configuration › Service clients](../reference/configuration.md#service-clients)). |
| `auth.clients.<name>.policy.server_mounts names "…", which is not in mounts` (or `policy.images names "…", which is not an image alias of the … backend`) | a policy that names a mount or an image alias the server does not define | Add it to `mounts:` or the backend's `image_aliases`, or remove it from the policy. |
| `auth.clients: … field … not found` (or `network: … field … not found`) | an unknown key, usually a typo, under `auth.clients` or `network:` | Correct the key the message names. |
| `network.default_profile: isolated is not supported by the vz backend` | an isolated default on a VZ (macOS) server | Use `default_profile: open` (or leave it out); VZ supports only `open`. |
| `auth.clients.<name>.policy.network is isolated (the default when unset), which the vz backend cannot enforce; set network: open` | a service client on a VZ server with no `policy.network` | Set `network: open` in that client's policy. |
| `invalid network.default_profile: …` or `invalid network.allow_cidrs entry …` | a profile other than `open`/`isolated`, or an `allow_cidrs` entry that is not an IPv4 CIDR | Use `open` or `isolated`, and CIDRs such as `192.168.1.0/24`. |
| `state_dir must be an absolute path or start with ~/` | a relative `state_dir` | Use an absolute path (`/var/lib/shed/state`) or one under `~/`. |
| `invalid audit.exec_argv: …` | a value other than `""` or `full` | Leave it empty (argv[0] and the count) or set `full`. |
| `audit: open …/audit.jsonl: …` | the audit log is needed (`auth.clients` is set, or `audit.enabled: true`) and the server cannot create or open it | Make the parent of the backend's `instance_dir` writable by the user that runs `serve` (see [Configuration › Audit log](../reference/configuration.md#audit-log)). |

## Quick reference

| Case | `auth.mode` | `bind_address` | TLS | `shed server add` |
|------|-------------|----------------|-----|-------------------|
| 1 — local simple | `open` | `127.0.0.1` (default) | none | `shed server add localhost` |
| 2 — local hardened | `token` | `127.0.0.1` (default) | loopback `:8443` | `shed server add localhost --https-port 8443` |
| 3 — remote, bearer-token admin scripts | `token` | `0.0.0.0` | all ifaces `:8443` | `shed server add <host> --https-port 8443` |
| 3b — remote (recommended) | `mtls` | `0.0.0.0` | all ifaces `:8443`, client cert required | `shed server add <host> --ssh-port <port>` |
| 4 — remote open on LAN | `open` + `allow_insecure_exposure` | `0.0.0.0` | none | `shed server add <host>` |
