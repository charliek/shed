# Security Posture: Credential Isolation

## Executive Summary

The credential extensions keep long-lived credential material and private keys out of
development VMs by brokering signing and secret resolution through the user's own machine.
SSH private keys and long-lived AWS credentials never enter the VM: only signatures and
short-lived STS session credentials cross the bus. Docker registry credentials are brokered
on demand from the machine's credential helpers, limited to an allowlist of registries.

Three rules bound what a shed can obtain:

- **One machine answers per user.** A shed's requests go only to the
  [credential source](source-binding.md) of the shed's own user on that server, and only that
  machine's answers are accepted.
- **The machine signs only two kinds of SSH data**: a login as an allowed username (by
  default `git`) with a key it offers to sheds, and a `git` commit or tag signature.
  Everything else is refused before any approval prompt.
- **Every request is audited** on the machine, refusals included.

## Before / After Comparison

| Aspect | Before (traditional) | After (shed extensions) |
|--------|---------------------|------------------------|
| SSH private keys | Copied into VM or mounted | Never enter the VM: only signatures cross the bus |
| What a key can be used for from the VM | Anything: any host, any username | SSH logins as `ssh.allow_users` (default `git`) and `git` commit signatures, with the keys in `ssh.offer_keys` (default all) |
| AWS credentials | `~/.aws/credentials` mounted or env vars set | Never enter the VM: only short-lived STS credentials |
| Key rotation | Must update every VM or shared mount | Rotate on the machine only |
| Credential scope | VM has full access to all keys/profiles | Per-shed IAM role via the machine's config |
| Audit trail | None | JSON audit log on the machine: shed, operation, result, approval |
| Compromised VM impact | Attacker gets the key material | Attacker can request the signatures and credentials listed below, cannot extract keys; STS credentials expire |

## What Enters the VM

| Data | Enters VM? | Risk level | Notes |
|------|-----------|------------|-------|
| SSH private keys | No | — | Only signatures cross the bus |
| SSH public keys | Yes | None | Only the keys in `ssh.offer_keys`, when it is set |
| AWS long-lived credentials | No | — | Never leave the machine |
| AWS STS session credentials | Yes | Low | Short-lived (1h by default), role-scoped, cannot be refreshed without the broker |
| AWS role ARN | No | — | Set in the machine's config, not selectable from the VM |
| Docker registry credentials | Yes | Medium | Tokens or passwords from the machine's credential helpers; lifetime depends on the registry |
| Docker config.json | No | — | The machine's config never enters the VM |
| Touch ID biometric data | No | — | Evaluated entirely on the machine's hardware |

## Threat Model

### Compromised VM

Code running as root in a shed can send any request the in-VM helpers can. With whatever
approval the machine's policy requires, it **can** obtain:

- an SSH login signature for a username in `ssh.allow_users` (default `git`), made with a key
  the machine offers to sheds, for **any** SSH server;
- an SSH signature in the namespace `git` over any content (a commit or tag signature in the
  user's name);
- AWS STS credentials for the role configured for that shed (in `mode: passthrough`, the
  source profile's own session credentials);
- credentials for the Docker registries in the shed's allowlist.

It **cannot**:

- extract an SSH private key (they never enter the VM);
- get a login signature for any other username. A username beginning with `_` is refused
  whatever the configuration says, so the user's key can never be used to enrol a credential
  on a shed server's `_bootstrap` channel;
- with the default `ssh.allow_users`, log in to a shed server with the signature it can get:
  shed-server refuses the SSH username `git` and refuses `git` as a shed name;
- get a signature in any other SSHSIG namespace (such as `file`), or over bytes that are not
  exactly one of the two shapes (an arbitrary challenge, data with trailing bytes, an RSA
  request whose hash flags disagree with its payload);
- sign with a key the machine does not offer, or with a certificate, a security-key (`sk-`)
  key or a DSA key. A key that is not offered is answered exactly as a key that is not
  present, so a shed cannot tell the two apart;
- reach another user's machine: requests are routed by the shed's user, as the server
  records it, never as the guest says;
- escalate to an AWS role not configured for that shed, refresh expired STS credentials
  without the broker, reach a registry outside the allowlist, or store or erase credentials
  on the machine (the Docker broker is read-only).

The signing check runs **before** the approval gate. A refused request never shows a prompt,
never reads or writes an approval cache entry, never reaches the SSH agent or key file, and
is written to the audit log with its reason. What the shed is told is no more than it could
learn before: `invalid public key` (`KEY_NOT_FOUND`) for a key that does not parse, and
`sign operation failed` (`SIGN_FAILED`) for every other refusal. See
[SSH Agent](ssh-agent.md#what-the-machine-signs) for the exact rules.

A shed owned by a [service client](../reference/security.md#service-clients) gets nothing:
the credential bus is always off for it, and its messages are dropped before they reach any
machine.

The bus is only one side. What a compromised guest can reach on the **network** is set by its
network profile, not by the extensions. See
[Security › Network profiles](../reference/security.md#network-profiles).

### What the signing check does not cover

- **Which host a login is for.** A login signature is not bound to a server the machine
  trusts. In the host-bound form the request names the server's host key, which is checked
  for structure only; the CA signature on a host certificate is **not** verified. Once
  approved, a `git` login signature works for any server that accepts that key for the user
  `git` (GitHub, GitLab, a self-hosted git server, or any SSH server with such an account).
- **Allowed usernames on any host.** Adding a username to `ssh.allow_users` lets a shed log
  in as that user, with an offered key, to any host that accepts the key for that name. That
  is why any name other than `git` requires `ssh.offer_keys`: name only the keys that should
  be usable that way. On a shed server the SSH username is the shed's name, so an allowed
  username that is also the name of one of your sheds lets a compromised shed open that shed
  with a key the server accepts for you.
- **What is signed with `git`.** A commit or tag signature covers whatever content the shed
  hands over.
- **AWS and Docker.** Credential requests in those namespaces are not narrowed by this
  check; they are bounded by the role, the allowlist and the approval policy.
- **Older shed servers.** A server older than 0.10.0 does not reserve `git`; there a shed
  named `git` could be opened with a `git` login signature. Upgrade every server a machine
  answers for.
- **The approval prompt.** The prompt for an SSH sign request names the server and shed, not
  the username or host the signature is for.

### Compromised machine

The broker has access to the user's SSH keys (through `SSH_AUTH_SOCK` or `~/.ssh/`) and AWS
credentials. A compromised machine is the same threat it was without shed.

### Bus interception and routing

Inside the VM the helpers talk to the shed agent on `127.0.0.1:498`, and the agent talks to
the shed server over vsock, a host-to-VM channel with no network path. From the server to the
machine the bus runs over HTTPS with a client certificate in `mtls` mode; an `open` server
listens on loopback only.

The server routes each request to the listener of the shed's user for that namespace, and
admits a listener only from that user's bound machine. Another user's machine never receives
the request, and its answer is refused.

### Replay

The guest chooses its own request ids, so the server does not trust them: it mints its own
random id for every request it accepts, sends that to the machine, and accepts an answer only
for an id it still holds, from the user it was routed to, for the same shed and namespace. A
final answer is consumed once it has been written into the shed. An answer to an id that was
already answered, dropped or never issued is refused.

## Audit Trail

Every credential operation is appended as one JSON line to the machine's audit log
(`logging.path`, default `~/.local/share/shed/extensions-audit.log`), whether or not the
desktop app is connected. The file is never rotated by the broker.

```json
{"ts":"2026-11-16T15:04:05Z","server":"devbox","shed":"my-service","ns":"ssh-agent","op":"sign","result":"ok","detail":"ssh-ed25519","approval":"shed-desktop","decided_by":"user","scope":"per-session","ttl":"2h"}
{"ts":"2026-11-16T15:04:06Z","server":"devbox","shed":"my-service","ns":"ssh-agent","op":"sign","result":"refused","detail":"login user=deploy","code":"SIGN_REFUSED","reason":"username_not_allowed","approval":"none"}
{"ts":"2026-11-16T15:04:07Z","server":"devbox","shed":"my-service","ns":"aws-credentials","op":"get_credentials","result":"ok","detail":"expires:16:04","approval":"approve-all"}
```

| Field | Description |
|-------|-------------|
| `ts` | UTC timestamp |
| `server` | The server's name in multi-server mode; omitted in single-server mode |
| `shed` | Shed name |
| `ns` | `ssh-agent`, `aws-credentials`, `docker-credentials`, or `egress` |
| `op` | Operation (`sign`, `list`, `get_credentials`, `get`, …); for egress, the protocol |
| `result` | `ok`, `denied`, `error`, `anonymous` (a Docker `get` with no credential to serve; the guest pulls anonymously), `refused` (a sign request the signing check declined), `undelivered` (the server refused the answer, for example after a move of the credential source); for egress, the verdict (`allow` / `deny`) |
| `detail` | Key type, `N keys`, registry, `expires:HH:MM`; for a refused sign, a short description such as `login user=deploy` or `sshsig namespace=file` (never the request itself) |
| `code` | Error code; `SIGN_REFUSED` and `APPROVAL_DENIED` appear only in the audit log |
| `reason` | Why a request was not ok; for a refused sign, its class (below) |
| `approval` | The approval method in force: `deny-all`, `approve-all`, `shed-desktop`, `biometrics`, `biometrics-or-password`, or `none` (an operation that is not gated, or a request refused before the gate) |
| `decided_by`, `scope`, `ttl` | How an approval was decided (`user`, `touchid`, `policy`, `timeout`), and the scope and lifetime it applied |

A refused sign request's `reason` is one of `malformed_request`, `malformed_key`,
`key_not_offered`, `username_not_allowed`, `reserved_username`, `wrong_namespace` or
`unparseable_data`. Refusal lines are limited per shed in fixed 60-second windows: the first
20 refusals each get a line, then the first refusal of each class not yet logged in the window
still gets one, and the rest are counted and written as one summary line
(`reason: audit_rate_limited`, with a per-class count in `detail`) when the window ends or the
broker shuts down. A broker killed outright loses the counts of windows still open. `ok`,
`denied` and `error` lines are not limited.

## Compliance Benefits

- **Credential rotation**: rotate keys on the machine without touching any VM
- **Least privilege**: each shed gets one IAM role from the machine's config (except opt-in
  passthrough mode; see Residual Risks); SSH keys are usable from a shed only for the
  usernames and keys the machine allows
- **Audit**: one machine-readable log of every credential operation, refusals included
- **Separation of concerns**: the dev environment runs apart from credential storage
- **Key management flexibility**: keep existing tools (Secretive, 1Password, yubikey-agent)

## Residual Risks

- **Machine compromise**: a compromised machine has the same access to credentials it has
  without shed.
- **STS window**: STS credentials a shed already holds stay valid until they expire (1 hour
  by default) after the broker stops; AWS cannot revoke them early.
- **Passthrough mode breadth**: in `mode: passthrough` the broker vends the source profile's
  own session credentials and full scope, not a narrower per-shed role. `status` reports
  `passthrough:<profile>` in place of a role ARN. Prefer AssumeRole wherever an assumable role
  exists.
- **Signature use within the rules**: while the broker runs, a compromised shed can ask for
  the signatures listed above as often as it likes; the broker does not rate-limit sign
  requests. An approval policy that prompts (`shed-desktop`, `biometrics`,
  `biometrics-or-password`) limits each to what the user approves; `approve-all` signs every
  request that passes the check.
- **No TLS inside the VM hop**: the guest-to-agent and agent-to-server hops use plain HTTP on
  loopback and vsock. Neither has a network path.
