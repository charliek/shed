# shed-desktop Approval Channel

`shed-host-agent` can delegate approval decisions to the
[shed-desktop](../desktop/index.md) app over a local Unix-domain socket, instead of
approving inline. (The desktop app can also run the same broker in-process; then no
socket is involved. See [Desktop architecture](../desktop/architecture.md#the-embedded-credential-broker).) The same socket also
carries an all-namespace audit/event stream that the app renders as a live
activity feed.

The approval socket is **always on** — there is nothing to enable. It lives at a
fixed path (see [Host Agent IPC](host-agent-ipc.md)), so the app can always find
the agent. To route a given extension's decisions to the app, set its
`approval.policy` to `shed-desktop`:

```yaml
ssh:
  approval:
    policy: shed-desktop            # decide SSH sign approvals in the app
aws:
  approval:
    policy: shed-desktop            # optional: toggle Allow/Deny in the app
docker:
  approval:
    policy: shed-desktop            # optional: toggle Allow/Deny in the app

# Optional: how long to wait for the app to decide before failing closed (default 25s)
# approval_timeout: 25s
```

A `shed-desktop` policy selects the delegating gate for that extension. If a
policy is `shed-desktop` but no app is connected, the agent **fails closed** —
it denies rather than silently falling back.

## Scope

- **Gated:** any extension whose policy is `shed-desktop`. The agent advertises
  these in `hello_ack.gate_namespaces` so the app shows the matching approval
  UI. SSH presents the full interactive card (scope / TTL / always-allow /
  always-deny); AWS and Docker present a live Allow/Deny toggle.
- **Stream-only:** every other namespace's audit entries are still streamed to
  the app so the activity feed is complete, even when that extension is gated
  natively (Touch ID) or set to approve-all/deny-all.

## Transport

- A Unix-domain socket at a fixed path (mode `0600`; owner-only). See
  [Host Agent IPC](host-agent-ipc.md#socket-locations) for the path resolution.
- Newline-delimited JSON, one typed envelope per line.
- Single active consumer, **last-writer-wins**: a new `hello` supersedes the
  previous app connection (the old one receives `hello_ack {accepted:false}`).

## Wire protocol

The frame catalog and handshake are specified in
[Host Agent IPC → Approval channel](host-agent-ipc.md#approval-channel) (the
canonical reference). In short:

| Direction | type | Fields |
|-----------|------|--------|
| app → agent | `hello` | `client{name,version,pid}`, `capabilities[]`, `replay_events` |
| agent → app | `hello_ack` | `agent{version,approval_method}`, `namespaces[]`, `gate_namespaces[]`, `agent_capabilities[]` (omitted by an older agent), `request_timeout_ms`, `accepted` |
| agent → app | `approval_request` | `id`, `namespace`, `op`, `server` (omitted in single-server mode), `shed`, `detail`, `expires_at` |
| app → agent | `approval_response` | `request_id`, `decision` (approve\|deny), `decided_by`, `scope`, `ttl` |
| agent → app | `event` | `kind`, `server`, `shed`, `ns`, `op`, `result`, `detail`, `code`, `reason`, `approval`, `decided_by`, `scope`, `ttl` (one per audit entry, all namespaces) |
| app → agent | `credential.get` / `token.get` | `id`, `server` (and `csr` for `credential.get`): a control-scope credential for the app |
| agent → app | `credential.response` / `token.response` | `in_reply_to`, `server`, the credential or an `error` |
| agent → app | `ping` / app → agent `pong` | liveness |

`approval_request.detail` is a fixed description (`SSH sign request`, `AWS credentials
request`, `Docker credentials for <registry URL>`). It does not name the username or host an
SSH signature is for. The app never sees key material or secrets.

An SSH sign request reaches the app only after it has passed the
[signing check](ssh-agent.md#what-the-machine-signs). A request the check refuses never
produces an `approval_request`; it appears only as an `event` with `result: "refused"`.

When the app approves, it returns `decided_by` (`user`/`touchid`/`policy`),
`scope`, and `ttl`; the agent records these in its **durable audit log**, so the
host-side record is complete regardless of where the decision was made.

## Fail-closed semantics

The delegated gate degrades to the same outcome as an unanswered Touch ID
prompt today — a **deny**:

| Condition | Outcome |
|-----------|---------|
| No app connected | deny immediately |
| App connected, no response within `approval_timeout` | deny |
| App disconnects mid-request | deny |
| Duplicate / unknown `request_id` response | ignored |
| `decision: approve` within budget | sign proceeds |

## Security model

The agent remains the sole holder of credentials; the app only ever handles
request metadata, exactly as in the existing
[security posture](security-posture.md). The socket is owner-only and there is
no network listener — the trust boundary is the local user account.
