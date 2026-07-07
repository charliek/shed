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
  "version": "0.3.7",
  "pid": 4242,
  "started_at": "2026-06-11T06:34:46Z",
  "written_at": "2026-06-11T06:40:02Z",
  "config_path": "/opt/homebrew/etc/shed/extensions.yaml",
  "policies": {
    "ssh-agent": "shed-desktop",
    "aws-credentials": "deny-all",
    "docker-credentials": "approve-all"
  },
  "gate_namespaces": ["ssh-agent"],
  "approval_channel": {
    "socket_path": ".../host-agent.sock",
    "consumer_connected": true,
    "client_name": "ShedDesktop",
    "client_version": "1.2.0"
  },
  "servers": [
    {
      "name": "localmac-dev",
      "url": "http://localhost:8080",
      "namespaces": [
        { "namespace": "ssh-agent", "state": "connected", "since": "..." }
      ]
    }
  ]
}
```

| Field | Meaning |
|-------|---------|
| `schema` | Contract version (currently `1`). Bumped only on a breaking change. |
| `config_path` | Absolute path of the config the daemon actually loaded. |
| `policies` | Effective approval policy per namespace (after defaulting empty → `deny-all`). |
| `gate_namespaces` | Namespaces whose policy is `shed-desktop` (decided in the app). |
| `approval_channel` | The approval socket path and its current consumer, if any. |
| `servers[].namespaces[].state` | `connected`, `reconnecting` (with `last_error`), or `stopped`. |

**Compatibility:** new fields may be added without bumping `schema`; clients
should ignore unknown fields. A field's removal or a meaning change bumps
`schema`.

## Approval channel

Newline-delimited JSON, one typed envelope per line, max 1 MiB per frame.
Protocol version (`v`) is **2**. A single consumer is active at a time
(last writer wins — a new `hello` supersedes the previous connection).

```text
app → agent:  hello, approval_response, pong
agent → app:  hello_ack, approval_request, event, ping
```

**Handshake.** The client sends `hello` within 2 seconds of connecting:

```json
{ "type": "hello",
  "client": { "name": "ShedDesktop", "version": "1.2.0", "pid": 5678 },
  "capabilities": [],
  "replay_events": 50 }
```

The agent replies `hello_ack` — `accepted: true` advertises the namespaces it
serves, the `gate_namespaces` to show approval UI for, and the
`request_timeout_ms` budget (from `approval_timeout`). A superseded connection
gets `accepted: false, reason: "superseded"` and is closed. `replay_events`
asks the agent to re-send up to N of the most recent buffered events on connect.

**Approvals.** For each `shed-desktop`-policy request the agent sends an
`approval_request` carrying `namespace`, `op`, `server`, `shed`, `detail`, and
an `expires_at`. The client answers with `approval_response`
(`decision: "approve" | "deny"`, plus `decided_by`/`scope`/`ttl` for the audit
log). **Fail-closed:** if no consumer is connected, the decision times out, or
the connection drops mid-flight, the request is denied.

**Liveness.** The agent sends `ping` every 10s; the client replies `pong`.

**The first client is shed-desktop**, but the protocol is deliberately
client-agnostic — the `status` socket already demonstrates a second, read-only
consumer of the agent, and the `capabilities` field is reserved for negotiating
future modes (e.g. an observe-only audit stream).
