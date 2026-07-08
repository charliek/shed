# AWS Credentials

The `aws-credentials` namespace brokers AWS credential access between shed microVMs and the host machine. Long-lived AWS credentials never enter the VM — only short-lived STS session tokens cross the bus.

## How It Works

`shed-ext-aws-credentials` runs inside the VM as a systemd service, listening on `http://127.0.0.1:499`. The `AWS_CONTAINER_CREDENTIALS_FULL_URI` environment variable points all AWS SDKs to this endpoint.

When an AWS SDK requests credentials, `shed-ext-aws-credentials` translates the request into a message bus call to the host agent. The host agent performs `sts:AssumeRole` using the developer's local AWS profile and returns temporary credentials.

```mermaid
sequenceDiagram
    participant SDK as AWS SDK (any language)
    participant Proxy as shed-ext-aws-credentials (VM)
    participant Bus as Message Bus
    participant Host as shed-host-agent (Mac)
    participant STS as AWS STS

    SDK->>Proxy: GET /credentials
    Proxy->>Bus: publish(aws-credentials, get_credentials)
    Bus->>Host: SSE event
    Host->>Host: Check credential cache
    alt Cache valid
        Host->>Bus: cached credentials
    else Cache stale
        Host->>STS: sts:AssumeRole
        STS->>Host: temporary credentials
        Host->>Host: Update cache
        Host->>Bus: fresh credentials
    end
    Bus->>Proxy: response
    Proxy->>SDK: {AccessKeyId, SecretAccessKey, Token, Expiration}
```

## AWS SDK Integration

The AWS SDK credential chain checks `AWS_CONTAINER_CREDENTIALS_FULL_URI` automatically across all languages (Java, Python, Go, Node, Kotlin). No application code changes needed.

The proxy serves credentials in the exact format the SDK expects:

```
GET http://127.0.0.1:499/credentials
```

```json
{
  "AccessKeyId": "ASIA...",
  "SecretAccessKey": "...",
  "Token": "...",
  "Expiration": "2026-03-31T19:00:00Z"
}
```

The SDK handles refresh automatically — when credentials approach expiration, it re-requests from the endpoint.

## Role Configuration

In the default **assume-role mode**, each shed maps to a specific IAM role via host-side configuration. The VM doesn't get to choose which role it receives. (For SSO/SAML setups where no assumable role exists, see [Passthrough mode](#passthrough-mode-ssosaml), which needs no role.)

```yaml
aws:
  source_profile: default
  default_role: arn:aws:iam::123456789012:role/acmeco-dev

  servers:
    mini2:
      sheds:
        my-service:
          role: arn:aws:iam::123456789012:role/acmeco-dev
        integration-tests:
          role: arn:aws:iam::123456789012:role/acmeco-staging-readonly
```

## Passthrough mode (SSO/SAML)

AssumeRole requires a role the host's source session is allowed to assume. In **SSO/SAML environments there is often no such role** — the application's real role is an instance/service role (trusted only by a service principal), or the developer's own credentials came from `sts:AssumeRoleWithSAML`, and neither can be re-assumed with plain `sts:AssumeRole`.

Passthrough mode skips `AssumeRole` and vends the `source_profile`'s **existing session credentials** directly. SSO/SAML CLI helpers conventionally write those temporary credentials to the standard `~/.aws/credentials` file, so nothing new is needed to obtain them.

```yaml
aws:
  source_profile: my-sso     # the profile whose session creds are vended
  mode: passthrough          # assume-role (default) | passthrough
  approval:
    policy: approve-all
```

`mode` layers exactly like `role` — set it at the top level, per server, or per shed (most specific wins). This lets one shed use passthrough while another keeps AssumeRole:

```yaml
aws:
  source_profile: default
  default_role: arn:aws:iam::111111111111:role/dev   # assume-role default
  servers:
    mini2:
      sheds:
        sso-app:    { mode: passthrough }
        scoped-app: { role: arn:aws:iam::111111111111:role/scoped }
```

> **`mode` is inherited, and `role` is ignored under passthrough.** When `mode: passthrough` is set at a parent level it applies to every child shed unless a child overrides it with `mode: assume-role`; a `role` configured on a passthrough shed has no effect.

**Refresh is your responsibility.** The host agent cannot perform your SSO/SAML login, so it cannot refresh these credentials — it only re-reads the shared file. Re-running your login (e.g. `aws sso login`) rewrites `~/.aws/credentials`, and the agent picks up the new credentials on the next request, no restart needed. Passthrough is **not cached** for this reason.

**Expiry.** The standard shared-config loader treats profile credentials as static, so passthrough parses the helper-written `aws_session_expiration` / `x_security_token_expires` hint to report accurate expiry. If neither key is present, the `Expiration` field is omitted and the guest discovers expiry on a `403`. The omit-expiry behavior is verified for the Go AWS SDK; if your guest app uses a non-Go SDK (boto3, JS v3, Java) that requires `Expiration`, make sure your helper writes `aws_session_expiration`. Only the standard `aws_session_token` key is read — the legacy `aws_security_token` written by some SAML helpers is not supported.

### Tradeoffs vs AssumeRole

|                  | AssumeRole (default)            | Passthrough                     |
|------------------|---------------------------------|---------------------------------|
| IAM prerequisite | a role the source can assume    | none                            |
| Identity vended  | scoped assumed role             | the source identity itself      |
| Lifetime in VM   | short (e.g. 1h), auto-refreshed | full remaining source session   |
| Least privilege  | tight                           | broad                           |

Passthrough is less least-privilege, but it still delivers the extension's core wins over copying the credentials file into the VM: **host-brokered, per-request audited, and no credential written to the VM's disk** — and it's revocable by stopping the host agent.

## Message Format

### Request

```json
{
  "id": "0192b3a5-...",
  "namespace": "aws-credentials",
  "type": "request",
  "payload": {
    "operation": "get_credentials"
  }
}
```

### Response

```json
{
  "id": "0192b3a5-...",
  "namespace": "aws-credentials",
  "type": "response",
  "payload": {
    "access_key_id": "ASIAIOSFODNN7EXAMPLE",
    "secret_access_key": "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
    "session_token": "FwoGZXIvYXdzE...",
    "expiration": "2026-03-31T19:00:00Z"
  }
}
```

### Error

```json
{
  "id": "0192b3a5-...",
  "namespace": "aws-credentials",
  "type": "response",
  "payload": {
    "error": "sts:AssumeRole failed",
    "code": "ASSUME_ROLE_FAILED"
  }
}
```

## Credential Caching

The guest-side proxy does not cache credentials — every SDK request passes through to the host. In **assume-role mode** the host handler maintains a per-shed credential cache:

- Credentials are cached until they have less than 5 minutes remaining
- When the cache is stale, a fresh `sts:AssumeRole` call is made
- The AWS SDK's own refresh timing (~once per hour) drives re-fetching

**Passthrough mode is never cached** — the shared credentials file is re-read on every request so a fresh SSO/SAML login is picked up immediately.

Cache parameters are configurable:

```yaml
aws:
  session_duration: 1h         # STS token lifetime
  cache_refresh_before: 5m     # refresh when < 5 min remaining
```

## STS Session Details

These apply to **assume-role mode**; passthrough mode performs no `AssumeRole` and creates no STS session.

- **Session name format**: `shed-{server}-{shed}-{timestamp}` for CloudTrail traceability
- **Default duration**: 1 hour (configurable)
- **Source credentials**: Loaded from `~/.aws/credentials` using the configured profile

## Timeouts

Credential requests use a 3-second timeout. On timeout, the proxy returns:

```json
{
  "error": "credential request timed out",
  "message": "shed-host-agent not reachable. Is it running on the host machine?",
  "hint": "Ensure shed-host-agent is running on the host machine."
}
```

## Startup Health Check

On startup, `shed-ext-aws-credentials` pings the `aws-credentials` namespace. If no response arrives within 2 seconds, it logs a warning but continues starting.
