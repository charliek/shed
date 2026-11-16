# Host Agent IPC

`shed-host-agent` exposes two local Unix-domain sockets. They are the agent's
**public interface**: the [shed-desktop](desktop-approval.md) app, the
[`status`](configuration.md#shed-host-agent-status) command, and any future
tooling rendezvous here. Because clients must be able to find the agent without
reading its private config, the socket paths are **fixed and not configurable**
in `extensions.yaml`.

## Socket locations

Both sockets live in one directory, resolved in this order:

1. `$SHED_HOST_AGENT_SOCKET_DIR` if set (escape hatch for tests and running
   several agents side by side);
2. macOS: `~/Library/Application Support/shed/`;
3. otherwise: `$XDG_RUNTIME_DIR/shed/`, else `~/.local/share/shed/`.

| Socket | File | Purpose |
|--------|------|---------|
| Status | `host-agent-status.sock` | Read-only. Any client; dumps a JSON self-report and closes. |
| Approval channel | `host-agent.sock` | Stateful. A single consumer (normally shed-desktop) streams audit events and answers approvals. |

Both are created mode `0600` inside a `0700` directory (owner-only). A new agent
**refuses to bind** over a socket another live agent is already serving, and
removes only a genuinely stale file — so a second agent never silently steals
the first's clients.

## Status socket

Connect, read one JSON object, and the agent closes the connection. The object
is the daemon's authoritative self-report — the single source of truth for what
the *running* agent is doing (it never reflects a config file the agent didn't
load).

```json
{
  "schema": 1,
  "version": "0.10.0",
  "pid": 4242,
  "started_at": "2026-11-16T09:12:03Z",
  "written_at": "2026-11-16T09:40:02Z",
  "config_path": "/opt/homebrew/etc/shed/extensions.yaml",
  "policies": {
    "aws-credentials": "deny-all",
    "docker-credentials": "approve-all",
    "ssh-agent": "shed-desktop"
  },
  "gate_namespaces": ["ssh-agent"],
  "approval_channel": {
    "socket_path": ".../host-agent.sock",
    "consumer_connected": true,
    "client_name": "shed-desktop",
    "client_version": "0.10.0"
  },
  "servers": [
    {
      "name": "devbox",
      "url": "https://devbox.example.com:18443",
      "namespaces": [
        { "namespace": "ssh-agent", "state": "not-source",
          "last_error": "not the credential source for this server; the active source is desk (7d2e0c41a9b85f3e6c1d0a2b4f8e9c57)",
          "since": "...",
          "active_source": { "machine_id": "7d2e0c41a9b85f3e6c1d0a2b4f8e9c57", "machine_name": "desk" } }
      ],
      "active_source": { "machine_id": "7d2e0c41a9b85f3e6c1d0a2b4f8e9c57", "machine_name": "desk" }
    }
  ],
  "machine": { "id": "0f3c9a2e7b1d4c6a8e5f2b9d1c7a3e60", "name": "laptop" },
  "ssh": {
    "mode": "agent-forward",
    "allow_users": ["git"],
    "key_filter": true,
    "keys_offered": 2,
    "keys_available": 5
  }
}
```

| Field | Meaning |
|-------|---------|
| `schema` | Contract version (currently `1`). Bumped only on a breaking change. |
| `config_path` | Absolute path of the config the daemon actually loaded. |
| `policies` | Effective approval policy per namespace (after defaulting empty → `deny-all`). |
| `gate_namespaces` | Namespaces whose policy is `shed-desktop` (decided in the app). Omitted when empty. |
| `approval_channel` | The approval socket path and its current consumer, if any. |
| `servers[].namespaces[].state` | `connected`, `reconnecting` (with `last_error`), `not-source`, `duplicate`, `yielding` (the desktop app's embedded broker runs on this machine and answers; the daemon subscribes nothing until it quits; see [Credential source](source-binding.md#the-daemon-and-the-desktop-app-on-one-machine)), `rejected` (terminal), or `stopped`. See [Configuration → `shed-host-agent status`](configuration.md#shed-host-agent-status). |
| `servers[].namespaces[].active_source` | Only in `not-source`: the machine the server named as the user's [credential source](source-binding.md), `{machine_id, machine_name}` as the server sent them (control and format characters removed, at most 64 bytes each). |
| `servers[].active_source` | Set when any namespace of the server is `not-source`: the first such namespace's `active_source`. |
| `machine` | This machine's identity, `{id, name}`, from `machine.json`. Omitted when the broker has none. |
| `machine_error` | Why there is no `machine`. Omitted when there is one. |
| `ssh` | What the machine signs for a shed: `mode` (the resolved SSH backend), `allow_users`, `key_filter` (whether `ssh.offer_keys` is set), and `keys_offered` / `keys_available` from the last key listing (omitted until one has happened; local keys are listed at start-up, a forwarded agent only when a shed asks). |

**Compatibility:** new fields may be added without bumping `schema`; clients
should ignore unknown fields. A field's removal or a meaning change bumps
`schema`. `machine`, `machine_error`, `ssh`, `active_source` and the `yielding` state were added this way; an older
daemon's report has none of them.

## Approval channel

Newline-delimited JSON, one typed envelope per line, max 1 MiB per frame.
Protocol version (`v`) is **2**. A single consumer is active at a time
(last writer wins — a new `hello` supersedes the previous connection).

```text
app → agent:  hello, approval_response, pong, credential.get, token.get
agent → app:  hello_ack, approval_request, event, ping, credential.response, token.response
```

**Handshake.** The client sends `hello` within 2 seconds of connecting:

```json
{ "type": "hello",
  "client": { "name": "shed-desktop", "version": "0.10.0", "pid": 5678 },
  "capabilities": [],
  "replay_events": 50 }
```

The agent replies `hello_ack` — `accepted: true` advertises the namespaces it
serves, the `gate_namespaces` to show approval UI for, the `request_timeout_ms`
budget (from `approval_timeout`), and `agent_capabilities`: the optional messages it
answers (today `["credential.get"]`). An agent that predates capabilities omits the key,
and the app then knows not to send those messages. A superseded connection
gets `accepted: false, reason: "superseded"` and is closed. `replay_events`
asks the agent to re-send up to N of the most recent buffered events (it keeps the last 100)
on connect.

```json
{ "v": 2, "type": "hello_ack", "id": "...", "ts": "...",
  "agent": { "version": "0.10.0", "approval_method": "shed-desktop" },
  "namespaces": ["ssh-agent", "aws-credentials", "docker-credentials", "egress"],
  "gate_namespaces": ["ssh-agent"],
  "agent_capabilities": ["credential.get"],
  "request_timeout_ms": 25000,
  "accepted": true }
```

**Approvals.** For each `shed-desktop`-policy request the agent sends an
`approval_request` carrying `namespace`, `op`, `server` (omitted in single-server mode),
`shed`, `detail`, and an `expires_at`. `detail` is a fixed description: `SSH sign request`,
`AWS credentials request`, or `Docker credentials for <registry URL>`. An SSH sign request
reaches this point only after it has passed the
[signing check](ssh-agent.md#what-the-machine-signs); a refused one is never sent. The
client answers with `approval_response` (`request_id`, `decision: "approve"` or anything
else for deny, plus `decided_by`/`scope`/`ttl` for the audit log). Only the connection the
request was sent to can answer it. **Fail-closed:** if no consumer is connected, the decision
times out, or the connection drops mid-flight, the request is denied.

**Events.** Every audit entry is sent as an `event` frame (`kind: "audit"`) with the audit
fields (`server`, `shed`, `ns`, `op`, `result`, `detail`, `code`, `reason`, `approval`,
`decided_by`, `scope`, `ttl`; empty ones omitted). Refused sign requests arrive this way with
`result: "refused"`; see [Security Posture → Audit Trail](security-posture.md#audit-trail).

**Credentials for the app.** The app asks the agent to obtain a control-scope credential for
a server on its behalf:

| Frame | Fields | Notes |
|---|---|---|
| `credential.get` | `id`, `server`, `csr?` | `csr` is a base64 PKCS#10 request the app generated; its private key never leaves the app. Sent only when `hello_ack.agent_capabilities` lists `credential.get` |
| `credential.response` | `in_reply_to`, `server`, `auth_mode?`, `client_cert?`, `cert_serial?`, `token?`, `expires_at?`, `error?` | `auth_mode` names which credential is filled in: `mtls` (`client_cert`) or `token`, from a server older than 0.10.0 still in token mode (removed in 0.10.0). On an error neither is set |
| `token.get` | `id`, `server` | The older message, kept for apps that predate `credential.get`. It can only carry a bearer token; for an `mtls` server the agent answers with an error asking for the app to be upgraded |
| `token.response` | `in_reply_to`, `server`, `token?`, `expires_at?`, `error?` | |

The agent obtains either over the server's `_bootstrap` SSH channel with the system `ssh`
client, and refuses, before any SSH, an unknown server, a server with no SSH endpoint, or an
`open` (non-https) server.

**Frame limits.** Every outbound frame carries `v: 2`, a fresh `id` and a `ts`. A frame may be
at most 1 MiB; each write has a 5-second deadline.

**Liveness.** The agent sends `ping` every 10s; the client replies `pong`.

**The first client is shed-desktop**, but the protocol is deliberately
client-agnostic — the `status` socket already demonstrates a second, read-only
consumer of the agent, and the `capabilities` field is reserved for negotiating
future modes (e.g. an observe-only audit stream).
