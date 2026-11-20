# Security Configuration

shed is **local-development-first**: out of the box (the brew/apt install) it
runs localhost-only. Networked access is opt-in. Its network security is three
choices:

1. **Posture**: `auth.mode: open | mtls`. *Open* trusts the machine it runs on
   (plain HTTP, no credential, no TLS) and **binds loopback only**. *mtls*
   trusts nothing: pinned TLS, an SSH key allowlist, and a client certificate
   bound to a key that never leaves the client's device. It is **TLS-only**
   and the one posture that may face the network.
2. **Where listeners bind**: `bind_address` governs every listener (HTTP,
   HTTPS, SSH). It **defaults to loopback (`127.0.0.1`)** in both modes; in
   mtls mode you set it to a routable address to face the network.
3. **Who the callers are**: with no user list every accepted key is one
   implicit user, an admin. With [`auth.users`](../reference/configuration.md#users)
   (mtls only) each key belongs to a named user, who reaches only their own
   sheds. See the [Multi-user server guide](multi-user-server.md).

Separately from all three, you can limit what each **shed** reaches: no server
mounts, and on Firecracker an internet-only network. That works in every
posture, a localhost-only laptop included; see [Hardening the sheds
themselves](#hardening-the-sheds-themselves).

This guide walks the common shapes these choices produce. For the underlying
model (the invariants, ownership, the credential bus and the exact connection
flow) see the [Security reference](../reference/security.md). For a full
internet-facing walkthrough, see [Public VPS Deployment](vps-deployment.md).

Server settings live in `server.yaml` (`/etc/shed/server.yaml` on Linux,
`~/.config/shed/server.yaml` or the Homebrew `…/etc/shed/server.yaml` on macOS).
Client settings are written per server into `~/.shed/config.yaml` by
`shed server add`.

!!! note "Removed in v0.10.0"
    The bearer-token mode (`auth.mode: token`, also spelled `secure`) and
    `allow_insecure_exposure` (an open server off loopback) were removed in
    v0.10.0. A config that names either fails to load with a message naming
    `mtls`, rather than starting open. Switch such a server to
    `auth.mode: mtls` (Case 2 or 3): every client since v0.8.1 re-enrolls with
    a certificate on its next request. A caller that used a bearer token
    directly (`curl`, a script, CI) needs a client certificate, or can become a
    [service client](orchestrated-sheds.md).

## Choosing a posture

| Question | If yes |
|----------|--------|
| Is everything on **one machine**, and do you want zero auth ceremony? | **Open** (Case 1), the installed default |
| One machine, but you want TLS and key checking anyway (a shared box, or to mirror a networked server locally)? | **mtls on loopback** (Case 2) |
| Reachable **over the network**, a LAN, a tailnet or the internet? | **mtls, facing the network** (Case 3) |
| Several **people** use the server, each with their own sheds? | **mtls with users** (Case 4) |
| An orchestrator (CI, an agent runner) needs to create and drive **only its own sheds**? | **mtls** (Case 3) with a [service client](orchestrated-sheds.md) |

Two invariants hold, and explain the startup rejections you may hit (see
[Troubleshooting](#troubleshooting-startup-rejections)):

- **credential ⟺ TLS ⟺ mtls**: HTTP credential enforcement (a client
  certificate) exists only in mtls mode, and mtls is always TLS.
- **https ⟺ mtls**: `https_port` is valid only in mtls mode. So a client (or
  the desktop app) can treat an `https://` `api_url` as proof the server
  requires a credential.

---

## Case 1: Local only, simplest (open)

**For:** a developer running shed-server and the CLI on the same machine, who
wants nothing exposed and no SSH allowlist, certificate or TLS ceremony.

`server.yaml`:

```yaml
auth:
  mode: open             # the default; may be omitted
bind_address: 127.0.0.1  # the default; an open server may bind nothing else
```

This is also what you get with an empty `server.yaml`. Add the server (plain
HTTP, no flags):

```bash
shed server add localhost
```

**You get:** plain HTTP + SSH bound to loopback. Nothing reaches the LAN; no
credential, no TLS, no key allowlist. **Trade-off:** any local user on the box
can reach the API, and every caller is the implicit user, an admin: loopback
*is* the trust boundary. Right for a single-user laptop. An open server with a
non-loopback `bind_address` fails to load.

---

## Case 2: Local only, hardened (mtls on loopback)

**For:** one machine, but you want full TLS and an SSH key allowlist even
locally: a shared host where loopback isn't a sufficient boundary, or to
mirror a networked config on your laptop.

`server.yaml`:

```yaml
auth:
  mode: mtls
  ssh:
    authorized_keys:
      - ssh-ed25519 AAAA…your-key… you@laptop   # the key you SSH and enroll with
bind_address: 127.0.0.1   # the default: HTTPS + SSH bind loopback only
# https_port defaults to 8443 in mtls mode
```

Add the server. First contact is over SSH: it pins the host key, enrolls a
client certificate over the `_bootstrap` channel and learns the HTTPS port:

```bash
shed server add localhost --name local
```

**You get:** TLS-only on `127.0.0.1:8443`, the SSH allowlist enforced, a
client certificate required on every request: nothing plaintext, nothing on
the LAN. **Trade-off:** mtls **requires** an SSH key source, so you must list
your key (`authorized_keys` here, or `github_users: [you]`).

---

## Case 3: Networked (mtls facing the network)

**For:** a VPS, a LAN box or a tailnet host reachable from other machines.
Only your keys may connect, and all traffic is encrypted.

`server.yaml`:

```yaml
auth:
  mode: mtls
  ssh:
    github_users: [alice-gh]   # only these GitHub keys may SSH in (and enroll certificates)
tls_names:
  - shed.example.com           # your public hostname / IP (extra cert SANs)
bind_address: 0.0.0.0          # mtls defaults to loopback too: set this to face the network
# https_port defaults to 8443
```

mtls defaults to loopback like open mode, so a networked server **must set
`bind_address`** explicitly (`0.0.0.0`/`*` for all IPv4, `::` for all
interfaces, or a specific public or tailnet IP); otherwise it is unreachable.
mtls needs no acknowledgment to bind the network: TLS plus a required
certificate make it safe.

From your laptop:

```bash
shed server add shed.example.com --name work
```

This pins the SSH host key (showing its fingerprint for confirmation),
enrolls a client certificate over the `_bootstrap` SSH channel (the private
key never leaves the client), and pins the TLS cert from the bundle; there is
no credential to paste. Add `--ssh-port` when the server's SSH port is not
`2222`.

**You get:** HTTPS on the chosen interfaces (`8443`), the SSH allowlist, and a
certificate checked on every request. An unauthenticated peer cannot complete
the HTTPS handshake (`curl -k` with no certificate gets no HTTP response at
all, not even a `401`). The credential bus is gated by the
`credentials` scope on the same pinned-TLS listener. **Trade-off:** revoking access means removing the SSH key
(it also cuts SSH and SFTP), and `curl` or a script needs a client
certificate. A script or orchestrator that needs only its own sheds can be a
[service client](orchestrated-sheds.md). See [Public VPS
Deployment](vps-deployment.md) for the full flow, out-of-band fingerprint
verification and rotation, and [mTLS
mode](../reference/security.md#mtls-mode) for the guarantee and its limits.

---

## Case 4: Several people (mtls with users)

**For:** a server shared by several people, each of whom should see and enter
only their own sheds.

`server.yaml`:

```yaml
auth:
  mode: mtls                 # required with auth.users
  users:
    alice:
      github: alice-gh
      admin: true            # lists, stops and deletes anyone's sheds; uses host directories
    bob:
      github: bob-gh
tls_names:
  - shed.example.com
bind_address: 0.0.0.0
```

`auth.users` replaces `auth.ssh`'s key sources: a key is listed under its user
(a GitHub login, keys by hand, or both), and config load refuses
`github_users` or `authorized_keys` beside it. Each person adds the server
with their own key, as in Case 3.

**You get:** each user lists and enters only their own sheds; another user's
shed answers as if it did not exist. An admin may also list, stop and delete
anyone's shed, but not enter it. Host directories, server mounts, images that
are not the default or an alias, and the image store are for admins. Each
user's credential requests go to one machine of theirs. **Trade-off:** a
server with users is not isolation between hostile tenants: the admins control
the host, and Firecracker runs without its jailer. Write the full user list
before the first start with it: on that start, a server with exactly one user
gives every existing shed to that user. The [Multi-user server
guide](multi-user-server.md) covers the whole setup, including sheds made
before the list existed.

---

## Hardening the sheds themselves

The posture decides who reaches the server. What a shed itself can reach is
set per shed, in any posture:

```bash
# No server mounts, and (Firecracker) the internet and nothing else
shed create untrusted --repo https://github.com/someone/tool.git --mounts none --network isolated
shed list -v      # the MOUNTS column shows none, NETWORK shows isolated
```

- **`--mounts none`** (or `--mounts claude,gh`) picks which of the server's
  `mounts:` the shed gets. Without the flag it gets all of them, as before
  (none for a user who is not an admin, on a server with users). The choice
  is stored and applied on every start. See [Security › Mount
  selection](../reference/security.md#mount-selection).
- **`--network isolated`** (Firecracker only) lets the shed reach the internet
  and nothing else: not the host on any of its addresses (except the shed's own
  9P and egress-proxy ports on the gateway), other sheds, the LAN (RFC 1918),
  the tailnet (`100.64.0.0/10`), link-local addresses and the cloud metadata
  service, and no IPv6. shed-server enforces it with its own nftables tables,
  so a process in the guest cannot opt out. Something on the host can remove
  them (`nft flush ruleset`, a firewall reload), and the shed is then open for
  up to 30 s, until shed-server's check puts them back. See [Security › Network
  profiles](../reference/security.md#network-profiles).
- **Use both.** On Firecracker, an `open` shed with no mounts can still reach
  another running shed's 9P port on the bridge, so leaving out mounts is not a
  boundary by itself.
- **Neither flag turns off the credential bus.** While a machine of the shed's
  user answers for this server, code in the shed can request SSH signatures
  and the cloud and registry credentials configured for it, like any shed (see
  [Credential isolation](../extensions/security-posture.md)). For untrusted
  code, quit the desktop app on your machine while the shed runs, or have a [service
  client](../reference/security.md#service-clients) own the shed: the bus is
  always off for an owned shed.
- **Neither flag stops the create's sync.** `shed create` still syncs your
  `default` [sync profile](../reference/sync.md#auto-sync-on-create) into the
  shed; add `--no-sync` when it copies anything the code should not see.

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

`auth.mode: mtls` forces the SSH allowlist to `enforce`, and a wrong or
incomplete allowlist on a remote host locks you out (recovery means editing
`server.yaml` from the provider's console). The safe rollout uses `warn` as a
**pre-flight**, in open mode, on the box itself, before you commit:

```yaml
auth:
  mode: open
  ssh:
    mode: warn                 # consult the allowlist, log would-deny, but still accept
    github_users: [alice-gh]
```

Restart, then watch the log for `SSH auth WOULD DENY (warn mode)` lines while
you (and the desktop app) connect. Once nothing legitimate is denied, switch to
`auth.mode: mtls`, which forces `enforce`. `warn` is valid only in open mode,
and a server with `auth.users` (mtls only) always enforces.

## Is anything plaintext in mtls mode?

No. mtls serves **no** plain-HTTP listener at all (not even on loopback): a
single pinned-TLS listener faces clients, and a client that holds a pin but
is handed a non-`https` URL fails closed rather than send plaintext. The SSH
channel (shed sessions and the certificate issuance channel) is encrypted
with the host key pinned in `~/.shed/known_hosts`. The only trust-on-first-use
moment is `shed server add` showing you the fingerprints to confirm (or pass
`--fingerprint` / `--tls-fingerprint` to verify out-of-band). See the
[connection-flow table](../reference/security.md#connection-flow-whats-encrypted-where).

The credential bus (`credentials` scope) and Connect tunnel (`control` or
`credentials`) ride that same TLS listener, so a desktop app's broker reaches
them over `https://HOST:8443` with the pinned cert and its own client
certificate. There is no plaintext channel, implicit or opt-in.

## Troubleshooting startup rejections

mtls refuses to start half-configured, and removed settings are refused by
name, so these configs are **rejected at startup** (the server names the gap
and exits):

| Message names… | Cause | Fix |
|----------------|-------|-----|
| `auth.mode: "token" was removed in v0.10.0; use auth.mode: mtls …` (or `"secure"`) | the removed bearer-token mode | Set `auth.mode: mtls` (Case 2 or 3), or remove `auth.mode` for an open server on loopback. |
| `config key "allow_insecure_exposure" was removed in v0.10.0 …` | the removed open-off-loopback acknowledgment, with any value | Delete it, and use `auth.mode: mtls` (Case 3) to reach the server from other hosts. |
| `bind_address "…" exposes an open-mode server beyond loopback …` | a non-loopback `bind_address` in open mode | Switch to `auth.mode: mtls` (Case 3), or bind loopback (Case 1). |
| `auth.mode: mtls requires at least one SSH key source (auth.users, or auth.ssh.github_users, authorized_keys, or authorized_keys_file)` | mtls with no key source | Add a key source (Cases 2 to 4). |
| `https_port requires auth.mode: mtls …` | `https_port` set under `open` | Use `mtls` (it defaults `https_port` to 8443), or drop `https_port`. |
| `auth.ssh.mode: enforce requires auth.mode: mtls …` | enforcing the allowlist in open mode | Use `mtls`, or `warn` to stage (above). |
| `auth.mode: mtls forces auth.ssh.mode: enforce …` | an explicit `off`/`warn` under mtls | Remove `auth.ssh.mode`: mtls derives `enforce`. |
| `invalid auth.mode: … (must be open or mtls)` | a typo or unsupported value in `auth.mode` | Use `open` or `mtls`. |
| `auth: … (keys are case-sensitive)`, or `config key "…" is not recognised; keys are case-sensitive, did you mean "…"?` | an unknown or differently cased key under `auth` (`Mode:`), or a top-level key that differs from a known one only in case (`Auth:`) | Correct the key the message names. |
| `auth.users requires auth.mode: mtls` | a user list in open mode | Set `auth.mode: mtls` (Case 4). |
| `auth.users cannot be combined with auth.ssh.github_users …` (or `authorized_keys`, `authorized_keys_file`) | key sources under both `auth.ssh` and `auth.users` | List each key or login under its user, and remove the `auth.ssh` source. |
| `auth.users.<name> needs a github login or at least one key`, `… label is required`, `key … is listed under … and …`, `… name the same GitHub login …`, `… is an SSH certificate …` | a user with no credential, a key without a `label`, a key or login listed twice, or a certificate as a key | Fix the entry the message names (see [Configuration › Users](../reference/configuration.md#users)). |
| `auth.users needs at least one admin (admin: true) with a github login or a key that is not marked script` | no admin who can act as a person | Mark a user `admin: true` who has a GitHub login or a key without `script: true`. |
| `"…" is not a boolean; write true or false, unquoted` | `admin` or `script` set to `yes`, `on` or a quoted string | Write `true` or `false`. |
| `github pin store …/github_pins.json is unreadable …` | a damaged GitHub pin file | Restore it, or move it aside to pin every login again at its next fetch. |
| `config key "auth.http" was removed` | a leftover `auth.http` block | Delete it: enforcement derives from `auth.mode: mtls`. |
| `config key "http_bind"/"ssh_bind" was unified into "bind_address" …` | a leftover `http_bind`/`ssh_bind` | Replace both with a single `bind_address`. |
| `config key "internal_http_port" was removed` | a leftover `internal_http_port` | Delete it: the bus and Connect tunnel ride the single listener. |
| `auth.clients requires auth.mode: mtls` | [service clients](orchestrated-sheds.md) declared in open mode | Switch to `auth.mode: mtls` (Case 3), or remove `auth.clients`. |
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
| 1: local, simple | `open` | `127.0.0.1` (the only choice) | none | `shed server add localhost` |
| 2: local, hardened | `mtls` | `127.0.0.1` (default) | loopback `:8443`, client cert required | `shed server add localhost --name local` |
| 3: networked | `mtls` | `0.0.0.0` or an address | `:8443`, client cert required | `shed server add <host> [--ssh-port <port>]` |
| 4: several people | `mtls` + `auth.users` | `0.0.0.0` or an address | `:8443`, client cert required | each person: `shed server add <host>` |
