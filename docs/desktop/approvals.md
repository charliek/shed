# Credential approvals

The headline feature: `shed-host-agent` delegates credential-approval decisions to
shed-desktop over a local Unix-domain socket, and streams an all-namespace audit feed
the app surfaces in **Activity**. The app holds no credentials — only request metadata
crosses the socket, and the agent stays the sole credential holder.

## Configuring for shed-desktop

In the host agent's `~/.config/shed/extensions.yaml`, set the `approval.policy` of each
extension you want the app to decide to `shed-desktop`:

```yaml
ssh:
  approval:
    policy: shed-desktop      # SSH approvals decided in the app (interactive)
aws:
  approval:
    policy: shed-desktop      # optional — a live Allow/Deny toggle in the app
docker:
  approval:
    policy: shed-desktop      # optional — a live Allow/Deny toggle in the app

# Optional — how long the agent waits for the app to decide before failing
# closed (deny). Go duration, default 25s.
# approval_timeout: 25s
```

The agent always serves the approval socket at a fixed path, so there is nothing
else to enable. (shed-host-agent v0.4.0 removed the old `desktop.enabled` /
`socket_path` / `timeout_ms` keys; a config that still sets them loads with a
deprecation warning.)

Restart the host agent, then launch shed-desktop. The **Approvals** pane header shows
`gate: shed-desktop` once connected; if the agent is down it shows
`host agent not connected` (also observable over IPC as `ui.state.host_agent_connected`).

It is **default-off**: an extension whose policy isn't `shed-desktop` is handled by the
agent itself (`deny-all`, `approve-all`, or native Touch ID for SSH) and is **audit-only**
to the app — its events still stream to Activity. The agent advertises which extensions it
delegates in `hello_ack.gate_namespaces`; Preferences shows an approval section for exactly
those.

## Policy (per provider)

Each request is decided by the [`PolicyEngine`](architecture.md), most-specific match first:

```
session grant  >  per-(server,shed) rule  >  per-provider rule
```

Configured in **Preferences**, per delegated provider:

- **SSH** — a **Method** (Touch ID or password / Touch ID only / Prompt) plus a default
  **decision** (pre-fills the card) and **Duration**. An incoming SSH sign shows a card with
  one **decision dropdown**, ordered most → least permissive:
    - **Always Allow** — persistent auto-approve rule for the shed (survives restart).
    - **Per Shed Allow** — auto-approve the shed until the app restarts (so each shed prompts once, until you restart).
    - **Time Based Allow** — auto-approve for the duration (default 2h).
    - **Always Ask** — approve this request and prompt again next time.
    - **Always Deny** — persistent auto-deny rule.

  Changing any SSH setting clears the live in-memory grants, so the new policy takes effect on
  the next request. The fingerprint icon appears only for the biometric methods (not "Prompt").
- **AWS / Docker** — a live **Allow / Deny** toggle (no prompt). Changing it takes effect
  immediately; no restart.
- **Per-shed rules** — *Always allow* / *Always deny* on a card persists a per-`(server,
  shed)` rule (managed under Preferences → Per-shed overrides). Identical shed names on
  different servers don't collide — rules are keyed by `(server, shed)`.
- **Session grants** — an in-memory grant for the chosen duration (not persisted).

When SSH prompts, an actionable **Approve / Deny** notification is posted so the decision is
reachable without the dashboard.

## Fail-closed

The agent denies a request when **no app is connected**, when the app **doesn't answer
within `approval_timeout`**, or on a **disconnect mid-request** — the same outcome as an
unanswered local prompt today. The app likewise auto-denies a queued request when its
countdown expires.

## Verifying it live

Everything above is exercised hermetically by the pytest harness against a Python fake
agent (`make -C desktop e2e-ci`). The one path a fake can't prove — the **real Rust
host-agent ↔ real Swift app** wire protocol — is covered by `scripts/live-verify.sh`:

```bash
# Non-disruptive: builds a real host-agent, starts it on a private socket with
# no plugin-bus servers, launches the real app pointed at it, and asserts the
# hello/hello_ack handshake (ui.state.host_agent_connected). Touches no brew
# service and no shed server.
./desktop/scripts/live-verify.sh --handshake

# Full SSH-sign drive: stops the brew host-agent (restored on exit), watches
# every server, and waits for you to trigger a real sign inside a shed
# (ssh in, then a git op). Approves it via shedctl, asserts the response, then
# checks fail-closed (quit the app → the next sign is denied).
./desktop/scripts/live-verify.sh --full
```

The script builds the agent from the in-repo `crates/shed-host-agent` (the credential
broker lives in this monorepo). It is not part of CI — it needs a real VM in the loop.
