# SSH Agent

The `ssh-agent` namespace brokers SSH key operations between the shed microVM and the host machine.

## How It Works

`shed-ext-ssh-agent` runs inside the VM as a systemd service, listening on a Unix domain socket at `/run/shed-extensions/ssh-agent.sock`. The `SSH_AUTH_SOCK` environment variable points all SSH clients to this socket.

When an SSH client (git, ssh, scp) requests a key operation, `shed-ext-ssh-agent` translates it into a message bus request and forwards it to the credential broker (the shed desktop app on your machine) for processing.

## Operations

| Operation | Description | Approval |
|-----------|-------------|----------|
| `list` | Return the public keys offered to sheds | Never |
| `sign` | Sign data with the specified key, if [the signing check](#what-the-machine-signs) accepts it | Per `ssh.approval.policy` |

## Message Format

### Request

```json
{
  "id": "0192b3a4-...",
  "namespace": "ssh-agent",
  "type": "request",
  "payload": {
    "operation": "sign",
    "public_key": "<base64-encoded public key blob>",
    "data": "<base64-encoded data to sign>",
    "flags": 0
  }
}
```

### Response

```json
{
  "id": "0192b3a4-...",
  "namespace": "ssh-agent",
  "type": "response",
  "payload": {
    "format": "ssh-ed25519",
    "blob": "<base64-encoded signature>",
    "rest": ""
  }
}
```

### Error

```json
{
  "id": "0192b3a4-...",
  "namespace": "ssh-agent",
  "type": "response",
  "payload": {
    "error": "sign operation failed",
    "code": "SIGN_FAILED"
  }
}
```

## Host-Side Backend

The broker auto-detects the best signing strategy for **request-time signing**
(the per-VM ssh-agent operations above):

1. **Agent-forward** (default): If `SSH_AUTH_SOCK` is set on the host and its socket answers, proxies sign requests to the developer's existing SSH agent (Secretive, 1Password, ssh-agent, etc.)
2. **Local-keys** (fallback): Reads `~/.ssh/id_ed25519`, `~/.ssh/id_rsa` and `~/.ssh/id_ecdsa`. **Only passphrase-less key files work** in this mode — an encrypted key is skipped. Use agent-forward (which handles passphrase/Keychain/hardware keys via your agent) for protected keys.

Override via config:

```yaml
ssh:
  mode: agent-forward  # or "local-keys"
```

## Enrolling the broker's certificate

The broker needs a credential of its own to subscribe to a server's credential bus. For a
server reached over `https` (every `mtls` server), it enrols a client certificate for the
`credentials` scope over the server's reserved `_bootstrap` SSH channel: it generates a key
pair, sends a certificate request, and stores the issued certificate with its key under its
state directory (`broker/creds/credentials/<server>/`, `0600` files in `0700`
directories). It enrols again before the certificate expires, and at once when the server
refuses it. A server reached over plain `http` (an `open` server, which listens on loopback
only) needs no credential, and the broker enrols nothing for it. A server older than 0.10.0
that still runs token mode (removed in 0.10.0) answers the same request with a bearer token,
which the broker still accepts.

The broker runs your **system `ssh` client** for this, so the SSH identity is resolved
exactly the way `shed server add` and `shed attach` resolve it: from your agent, the macOS
Keychain, a 1Password/Secretive `IdentityAgent`, a hardware key, or `~/.ssh/config`. A
**passphrase-protected or agent-only key works**; no unencrypted key file is required, and
the broker never reads private key material itself for this. The key must be one the server
accepts for you, and not one marked as a script key (a script key cannot enrol for the
`credentials` scope).

The server's host key is pinned in `~/.shed/known_hosts` (written by `shed server add`) and
`ssh` verifies it with `StrictHostKeyChecking=yes` against that file as the sole trust root.
A confirmed host-key **change** is treated as a possible MITM and fails closed permanently for
that server; a *missing* pin is a non-terminal "run `shed server add`" error.

### Running at login

The desktop app starts at login, as a login item, not from your shell. macOS exposes the
**Apple Keychain ssh-agent** to that session automatically, so a key added with
`ssh-add --apple-use-keychain` works out of the box. **1Password and Secretive**,
however, usually export `SSH_AUTH_SOCK` only from your shell rc files, which a login item
does not inherit, so the app may not see them. If a mint fails
and the app's log (or `broker.status`) reports *the server refused every key
ssh offered* while your shell's `ssh` gets in, the broker is not reaching your
agent: point `ssh` at the agent explicitly in `~/.ssh/config` (the recommended
fix):

```ssh-config
Host my-server          # the SSH host the broker dials — see `shed server list`
    IdentityAgent ~/.1password/agent.sock   # or your Secretive socket
    # or pin a specific key instead:
    # IdentityFile ~/.ssh/id_ed25519
    # IdentitiesOnly yes
```

An alternative is `launchctl setenv SSH_AUTH_SOCK SOCKET`, with the path of your agent's
socket, followed by a restart of the app.

> The `Host` block must match the **actual** server host the broker dials (as in
> `shed server list`), not an invented alias — the bootstrap connects to that
> host directly.

If your allowlisted identity lives in an agent holding **many** keys, `ssh` can
exhaust the server's per-connection attempt cap before offering it; pin the one
key with `IdentityFile` + `IdentitiesOnly yes`, or raise `auth.ssh.max_auth_tries`
on the server. A **touch-required** (FIDO/hardware) key is a poor fit for the
unattended, periodic mint — the broker can't satisfy the touch — so keep a
non-touch key on the server's allowlist. Note that `~/.ssh/config` is read by the
broker on its own, so a global `ProxyCommand`/`Match` will run on each mint.

## What the machine signs

The broker signs a request only if its data is **exactly** one of two things:

| Request | Signed when |
|---|---|
| An SSH public-key login (RFC 4252 §7, or OpenSSH's host-bound form `publickey-hostbound-v00@openssh.com`) | the username is in `ssh.allow_users` (default `git`), the service is `ssh-connection`, the key in the request is the key being asked to sign, and nothing follows the last field |
| An SSH signature (`ssh-keygen -Y sign`, which git uses to sign commits and tags) | the namespace is `git` |

Everything else is refused, by design:

- `ssh`, `scp` or `rsync` from a shed to a host **as any username not in
  `ssh.allow_users`** (for example `ssh deploy@host`), until that username is added;
- a username beginning with `_`, always, whatever `ssh.allow_users` says;
- SSH signatures in any namespace other than `git` (`file`, for example);
- arbitrary bytes, and data with anything after its last field;
- an RSA request whose hash flags disagree with the hash its data names;
- a signing key that is a certificate, a security-key (`sk-`) key or a DSA key, or that is not
  a valid key;
- a key not in `ssh.offer_keys`, when that is set.

The check runs **before** the approval gate. A refused request shows no prompt, reads or
writes no cached approval, and never reaches the SSH agent or key file. The shed is told
`invalid public key` (`KEY_NOT_FOUND`) for a key that does not parse, and `sign operation
failed` (`SIGN_FAILED`) for anything else, the same answers it got before the check existed.
The machine's audit log records each refusal with `result: refused`, `code: SIGN_REFUSED` and
its class as the `reason` (see [Security Posture](security-posture.md#audit-trail)).

A request that passes the check then goes to the approval policy as before.

### Allowing another username

To let sheds log in somewhere as a user other than `git`, add the name to `ssh.allow_users`
**and** name the keys that may be used with `ssh.offer_keys`:

```yaml
ssh:
  allow_users: [git, deploy]
  offer_keys:
    - SHA256:QSbP+oRmKHQkEp2CsJn72x/5Qxyq5lsK6sib0fK1mr0      # as `ssh-add -l` prints it
```

| Key | Default | Rules |
|---|---|---|
| `ssh.allow_users` | `[git]` (also when the key is absent or `null`) | A list; a present list replaces the default and `[]` signs no login at all (commit signing only). Each entry a non-empty string with no control character, compared byte for byte (`Git` is another name). A name beginning with `_` fails config load. Any name other than `git` requires `ssh.offer_keys`. |
| `ssh.offer_keys` | none: every key the backend holds is offered | A list; `[]` offers no key. Each entry a SHA256 fingerprint as `ssh-keygen -l` and `ssh-add -l` print it, or an OpenSSH public key line (as in a `.pub` file or `ssh-add -L`). A key line for a certificate, an `sk-` key, a DSA key or an invalid key fails config load. |

Both settings are machine-wide: they apply to every server and shed the broker answers for.
An invalid setting makes the config invalid, so the broker fails closed (see
[Desktop architecture](../desktop/architecture.md#the-credential-broker)).

With `ssh.offer_keys` set, a `list` from a shed returns only the offered keys, and a sign
request for any other key fails exactly as one for a key the backend does not hold, so a shed
cannot tell "not offered" from "not present". At start-up the broker logs how many keys are
offered of how many are available, and warns once, by fingerprint, about an entry that
matches no key.

`broker.status` shows the result in its `ssh` block (the backend mode, the
allowed usernames, and the keys offered); see
[Configuration](configuration.md#checking-the-broker).

## Approval

Sign requests that pass the check are then decided by `ssh.approval.policy` (see
[Configuration](configuration.md#approval-model) for every policy). `list` is never gated.

On macOS two native policies prompt with LocalAuthentication:

| `ssh.approval.policy` | LocalAuthentication policy | Behavior |
|----------|----------------------------|----------|
| `biometrics-or-password` | `LAPolicyDeviceOwnerAuthentication` | Touch ID, Apple Watch, or account password. Works in clamshell mode and on Macs without a fingerprint sensor. |
| `biometrics` | `LAPolicyDeviceOwnerAuthenticationWithBiometrics` | Touch ID only. Fails when no biometric is available. |

A prompt left unanswered for 120 seconds is a deny. On Linux there is no LocalAuthentication,
so both policies deny every request; use `shed-desktop` there.

For these two policies, `ssh.approval.scope` sets how often the prompt appears
(`per-request`, `per-session` (default), or `per-shed`) and `ssh.approval.session_ttl` how
long an approval is cached (default `4h`). `LAPolicyDeviceOwnerAuthentication` also has its
own macOS-managed grace period that can briefly skip the prompt; it is independent of
`session_ttl`. Under `shed-desktop` the app's Preferences own the scope and duration.

## Timeouts

The in-VM helper gives up on a request after 3 seconds (`list` and `sign` alike) and the SSH
client sees the agent fail. `shed-ext-ssh-agent` logs nothing for it; the machine's audit log
shows what happened to the request, if it arrived. The 3 seconds include any approval prompt:
a sign that waits longer than that for a decision fails in the shed even if it is approved
afterwards. With a policy that caches the approval (a session or per-shed scope), running the
command again is then signed without a prompt.

A request whose user has no connected credential source is dropped by the server, so it ends
the same way. See [Credential Source](source-binding.md#nothing-fails-over-by-itself).

## Startup Health Check

On startup, `shed-ext-ssh-agent` publishes a ping to the `ssh-agent` namespace. If no response
arrives within 2 seconds, it logs a warning but keeps starting (so it is ready when the broker
comes up), and writes `ssh-agent: not connected` to `/run/shed-extensions/ssh-agent.status`
(`ssh-agent: connected` otherwise).
