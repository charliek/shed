# Security Posture: Credential Isolation

## Executive Summary

shed-extensions eliminates long-lived credential material and private keys from development VMs by brokering all signing and secret resolution through the host machine. SSH private keys and AWS long-lived credentials never enter the VM — only short-lived STS session tokens (1-hour, role-scoped) cross the bus. Docker registry credentials are brokered on demand from the host's credential helpers with a configurable registry allowlist. The developer's existing key management (Secretive, 1Password, ssh-agent) works unchanged, with an added audit trail.

## Before / After Comparison

| Aspect | Before (traditional) | After (shed-extensions) |
|--------|---------------------|------------------------|
| SSH private keys | Copied into VM or mounted via bind | Never enter the VM — only signatures cross the bus |
| AWS credentials | `~/.aws/credentials` mounted or env vars set | Never enter the VM — only short-lived STS tokens |
| Key rotation | Must update every VM or shared mount | Rotate on host only — VMs get new signatures automatically |
| Credential scope | VM has full access to all keys/profiles | Per-shed IAM role scoping via host config |
| Audit trail | None — no visibility into credential usage per VM | JSON audit log on host with shed name, operation, timestamp |
| Accidental exposure | Keys in VM filesystem, env, or process memory | No credential material to expose |
| Compromised VM impact | Attacker gets full key material | Attacker can request signatures but cannot extract keys; STS tokens expire in 1 hour |

## What Enters the VM

| Data | Enters VM? | Risk level | Notes |
|------|-----------|------------|-------|
| SSH private keys | No | — | Only signatures cross the bus |
| SSH public keys | Yes | None | Public by definition |
| AWS long-lived credentials | No | — | Never leave the host |
| AWS STS session tokens | Yes | Low | Short-lived (1h), role-scoped, cannot be refreshed without host agent |
| AWS role ARN | No | — | Determined by host-side config, not VM-selectable |
| Docker registry credentials | Yes | Medium | Tokens or passwords from host credential helpers cross the bus; lifetime depends on registry |
| Docker config.json | No | — | Host-side config never enters the VM |
| Touch ID biometric data | No | — | Evaluated entirely on host hardware |

## Threat Model

### Compromised VM

An attacker with root access inside the VM can:
- Request SSH signatures for challenge data
- Request AWS STS session tokens for the configured role
- Request Docker registry credentials for registries in the allowlist

An attacker **cannot**:
- Extract SSH private keys (they never enter the VM)
- Escalate to AWS roles not configured for that shed
- Refresh expired STS tokens without the host agent running
- Access other sheds' credentials (bus routing is per-shed)
- Access Docker registries not in the allowlist (host rejects)
- Store or erase credentials on the host (Docker broker is read-only)

**Mitigation**: Audit logs on the host capture every credential operation with the shed name and timestamp. STS tokens expire and become useless.

### Compromised Host Agent

The host agent has access to the developer's SSH keys (via SSH_AUTH_SOCK or ~/.ssh/) and AWS credentials (~/.aws/credentials). This is equivalent to the current threat model — a compromised developer machine. No regression.

### Bus Interception

The vsock transport between the VM and host kernel is point-to-point. There is no network path to intercept. Other VMs on the same host cannot access a shed's vsock connection.

### Replay Attacks

shed-extensions does not deduplicate request IDs; each request is processed as it arrives. This adds little exposure in practice. The vsock channel is point-to-point with no network path for an outside party to capture and replay traffic (see Bus Interception). And a compromised VM gains nothing from replaying a request — it is already authorized to issue fresh sign and credential requests directly, which is covered under Compromised VM.

## Audit Trail

All credential operations are logged as JSON lines to `~/.local/share/shed/extensions-audit.log`:

```json
{"ts":"2026-03-31T15:04:05Z","shed":"my-service","ns":"ssh-agent","op":"sign","result":"ok","detail":"ssh-ed25519","approval":"none"}
{"ts":"2026-03-31T15:04:06Z","shed":"my-service","ns":"aws-credentials","op":"get_credentials","result":"ok","detail":"expires:16:04","approval":"none"}
```

| Field | Description |
|-------|-------------|
| `ts` | UTC timestamp |
| `shed` | Shed instance name |
| `ns` | Namespace (`ssh-agent`, `aws-credentials`, or `docker-credentials`) |
| `op` | Operation performed |
| `result` | `ok`, `denied`, `error`, or `anonymous` (docker get for an allowed registry with no credential to serve — the guest pulls anonymously) |
| `detail` | Key type, fingerprint, or role ARN |
| `approval` | Configured approval method: `biometrics-or-password`, `biometrics`, or `none` (gate disabled) |

## Compliance Benefits

- **Credential rotation**: Rotate keys on the host without touching any VM
- **Least privilege**: Each shed gets exactly one IAM role via host config (except opt-in passthrough mode — see Residual Risks)
- **Audit**: Centralized, machine-readable log of all credential operations
- **Separation of concerns**: Dev environment execution is isolated from credential storage
- **Key management flexibility**: Developers keep their existing tools (Secretive, 1Password, yubikey-agent)

## Residual Risks

- **Host machine compromise**: If the developer's Mac is compromised, the attacker has equivalent access to credentials. This is unchanged from the current model.
- **STS token window**: A compromised VM has a 1-hour window to use stolen STS tokens after the host agent stops. Tokens cannot be revoked early (AWS limitation).
- **Passthrough mode breadth**: In `mode: passthrough` (for SSO/SAML setups with no assumable role), the agent vends the source profile's *own* session identity and full scope — not a narrower per-shed role — for the remaining lifetime of that session. The host `status` output reports `passthrough:<profile>` in place of a role ARN. It keeps the core wins (host-brokered, per-request audited, no credential written to the VM's disk, revocable by stopping the host agent), but it is broader than AssumeRole; prefer AssumeRole wherever an assumable role exists.
- **Signature abuse**: A compromised VM can request arbitrary SSH signatures while the host agent is running. The optional Touch ID approval gate (`ssh.approval`) mitigates this by requiring per-request, per-session, or per-shed authorization; shed-extensions does not currently rate-limit sign requests.
- **No mTLS on bus**: The vsock transport does not use TLS. This is acceptable because vsock is a host-kernel-to-VM channel with no network exposure.
