# SSH Agent

The `ssh-agent` namespace brokers SSH key operations between the shed microVM and the host machine.

## How It Works

`shed-ext-ssh-agent` runs inside the VM as a systemd service, listening on a Unix domain socket at `/run/shed-extensions/ssh-agent.sock`. The `SSH_AUTH_SOCK` environment variable points all SSH clients to this socket.

When an SSH client (git, ssh, scp) requests a key operation, `shed-ext-ssh-agent` translates it into a message bus request and forwards it to the host agent for processing.

## Operations

| Operation | Description | Touch ID |
|-----------|-------------|----------|
| `list` | Return available public keys | No |
| `sign` | Sign challenge data with specified key | Configurable |

## Message Format

### Request

```json
{
  "id": "0192b3a4-...",
  "namespace": "ssh-agent",
  "type": "request",
  "payload": {
    "operation": "sign",
    "public_key": "ssh-ed25519 AAAA... user@host",
    "data": "<base64-encoded challenge>",
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
    "error": "key not found",
    "code": "KEY_NOT_FOUND"
  }
}
```

## Host-Side Backend

The host agent auto-detects the best signing strategy for **request-time signing**
(the per-VM ssh-agent operations above):

1. **Agent-forward** (default): If `SSH_AUTH_SOCK` exists on the host, proxies sign requests to the developer's existing SSH agent (Secretive, 1Password, ssh-agent, etc.)
2. **Local-keys** (fallback): Reads keys directly from `~/.ssh/`. **Only passphrase-less key files work** in this mode — an encrypted key is skipped. Use agent-forward (which handles passphrase/Keychain/hardware keys via your agent) for protected keys.

Override via config:

```yaml
ssh:
  mode: agent-forward  # or "local-keys"
```

## Secure-mode token bootstrap

When the shed server runs in `auth.mode: secure`, the host agent mints its own
short-lived API token over the server's reserved `_bootstrap` SSH channel (rather
than reading a pasted `credentials_token`). It does this by invoking your
**system `ssh` client**, so the SSH identity is resolved exactly the way
`shed server add` and `shed attach` resolve it — from your agent, the macOS
Keychain, a 1Password/Secretive `IdentityAgent`, a hardware key, or
`~/.ssh/config`. A **passphrase-protected or agent-only key works**; no
unencrypted key file is required, and the host agent never reads private key
material itself.

The server's host key is pinned in `~/.shed/known_hosts` (written by
`shed server add`) and `ssh` verifies it with `StrictHostKeyChecking=yes` against
that file as the sole trust root. A confirmed host-key **change** is treated as a
possible MITM and fails closed permanently for that server; a *missing* pin is a
non-terminal "run `shed server add`" error.

### Running under `brew services` (launchd)

The agent runs as a per-user LaunchAgent. macOS exposes the **Apple Keychain
ssh-agent** to that launchd session automatically, so a key added with
`ssh-add --apple-use-keychain` works out of the box. **1Password and Secretive**,
however, usually export `SSH_AUTH_SOCK` only from your shell rc files, which the
launchd agent does not inherit — so the daemon may not see them. If a mint fails
and `shed-host-agent status` (or the log) reports *no SSH identity available*,
point `ssh` at the agent explicitly in `~/.ssh/config` (the recommended fix):

```ssh-config
Host my-server          # the SSH host the agent dials — see `shed server list`
    IdentityAgent ~/.1password/agent.sock   # or your Secretive socket
    # or pin a specific key instead:
    # IdentityFile ~/.ssh/id_ed25519
    # IdentitiesOnly yes
```

Alternatives: `launchctl setenv SSH_AUTH_SOCK <socket>`, or add `SSH_AUTH_SOCK`
to the service's launchd plist.

> The `Host` block must match the **actual** server host the agent dials (as in
> `shed server list`), not an invented alias — the bootstrap connects to that
> host directly.

If your allowlisted identity lives in an agent holding **many** keys, `ssh` can
exhaust the server's per-connection attempt cap before offering it; pin the one
key with `IdentityFile` + `IdentitiesOnly yes`, or raise `auth.ssh.max_auth_tries`
on the server. A **touch-required** (FIDO/hardware) key is a poor fit for the
unattended, periodic mint — the daemon can't satisfy the touch — so keep a
non-touch key on the server's allowlist. Note that `~/.ssh/config` is read by an
unattended daemon, so a global `ProxyCommand`/`Match` will run on each mint.

## Touch ID Approval

Sign operations can require interactive approval on the host. Set `ssh.approval.enabled: true` to gate every sign request behind a macOS authentication prompt (see [Configuration](configuration.md#ssh-settings) for all fields).

When enabled, `ssh.approval.method` selects the authentication factor:

| `method` | LocalAuthentication policy | Behavior |
|----------|----------------------------|----------|
| `biometrics-or-password` (default) | `LAPolicyDeviceOwnerAuthentication` | Touch ID, Apple Watch, or account password. Works in clamshell mode (lid closed) and on Macs without a fingerprint sensor. |
| `biometrics` | `LAPolicyDeviceOwnerAuthenticationWithBiometrics` | Touch ID only. Fails when no biometric is available. |

`biometrics-or-password` is the default so approval works out of the box on a closed-lid MacBook with an external display, on a Mac mini/Studio, and on any setup without a Touch ID Magic Keyboard.

The `ssh.approval.policy` field (`per-request`, `per-session`, `per-shed`) controls how often the prompt appears, and `session_ttl` sets how long an approval is cached. `LAPolicyDeviceOwnerAuthentication` also has its own macOS-managed grace period that can briefly skip the prompt; this is independent of `session_ttl`, and the two compose without conflict.

## Timeouts

Credential requests use a 3-second timeout. On timeout, `shed-ext-ssh-agent` logs an actionable error:

```text
ERROR: ssh-agent sign request timed out — shed-host-agent may not be running.
```

## Startup Health Check

On startup, `shed-ext-ssh-agent` publishes a ping to the `ssh-agent` namespace. If no response arrives within 2 seconds, it logs a warning but continues starting (so it's ready when the host agent comes up).
