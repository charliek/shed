# Credential approvals

The headline feature: the credential broker delegates credential-approval decisions to
shed-desktop, and streams an all-namespace audit feed the app surfaces in **Activity**. The
broker is either the app's embedded one (the default) or a separate `shed-host-agent` daemon
reached over a local Unix-domain socket — see [Installation → Credential
broker](installation.md#credential-broker). Either way only request metadata reaches the
approval gate; the broker stays the sole credential holder.

## Configuring for shed-desktop

In `~/.config/shed/extensions.yaml` (read by the embedded broker and by the daemon alike),
set the `approval.policy` of each extension you want the app to decide to `shed-desktop`:

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

The daemon always serves the approval socket at a fixed path, so there is nothing else to
enable. (shed-host-agent v0.4.0 removed the old `desktop.enabled` / `socket_path` /
`timeout_ms` keys; a config that still sets them loads with a deprecation warning.) With no
`extensions.yaml` at all, the embedded broker synthesizes a default that routes SSH approvals
to the app — see [Architecture → The embedded credential
broker](architecture.md#the-embedded-credential-broker).

Restart the daemon (if you run one), then launch shed-desktop. Pending requests appear in the
**Approvals** pane; `broker.status` over IPC reports the broker mode in effect and, for the
embedded broker, the delegated namespaces and per-server connection state.

It is **default-off**: an extension whose policy isn't `shed-desktop` is handled by the
broker itself (`deny-all`, `approve-all`, or native Touch ID for SSH) and is **audit-only**
to the app — its events still stream to Activity. The broker advertises which extensions it
delegates (`hello_ack.gate_namespaces` from the daemon); Preferences shows an approval section
for exactly those.

## Policy (per provider)

Each request is decided by the policy engine (`shed-core`'s `PolicyEngine`), most-specific
match first:

```
session grant  >  per-(server,shed) rule  >  per-provider rule
```

Configured in **Preferences**, per delegated provider:

- **SSH** — an **Approval policy**, ordered most → least permissive:
    - **Always Allow** — approve every sign, no prompt.
    - **Per Shed Allow** — prompt once per shed, then approve that shed until the app
      restarts.
    - **Time Based Allow** (the default) — prompt, then approve the session for the
      **Duration** (default 2h).
    - **Always Ask** — prompt for every sign.
    - **Always Deny** — deny every sign, no prompt.

  For the policies that prompt, a **Method** picks the confirmation: **Authenticate**
  (`biometrics-or-password` — Touch ID or the login password on macOS, a polkit password
  prompt on Linux) or **Prompt only** (`prompt`, a plain Approve button). An incoming sign that
  prompts shows a card in the Approvals pane with **Approve** and **Deny** and a line naming
  what approving grants; the fingerprint icon appears only for the authenticating method.
  Changing any SSH setting clears the live in-memory grants, so the new policy takes effect on
  the next request.
- **AWS / Docker** — a live **Allow / Deny** toggle (no prompt). Changing it takes effect
  immediately; no restart.
- **Per-shed rules** — an *always-allow* or *always-deny* rule for one `(server, shed)`,
  created by `approval.decide` with `persist: true` over [IPC](ipc.md#approval-ops) and listed
  (and removable) under Preferences → Per-shed overrides. Identical shed names on different
  servers don't collide — rules are keyed by `(server, shed)`.
- **Session grants** — an in-memory grant for the chosen duration (not persisted).

When SSH prompts, an actionable **Approve / Deny** notification is posted so the decision is
reachable without the dashboard.

## Fail-closed

The broker denies a request when **no app is connected**, when the app **doesn't answer
within `approval_timeout`**, or on a **disconnect mid-request** — the same outcome as an
unanswered local prompt. The app likewise auto-denies a queued request when its countdown
expires. Quitting the app with the embedded broker stops brokering altogether, and the
server side fails closed.

## Testing

The approval flow is exercised hermetically by the desktop harness, which drives the real
app against a Python fake host agent (`desktop/tools/fake-host-agent`) and a mock
shed-server:

```bash
make -C desktop e2e-tauri    # includes test_approvals.py and test_tauri_broker.py
```

The `shed-host-agent` daemon's own wire output is pinned separately, against committed
goldens, by the host-agent wire harness (no app, no VM):

```bash
make test-host-agent-diff
```

See [Test automation](test-automation.md).
