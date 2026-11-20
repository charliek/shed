# Credential approvals

The headline feature: the credential broker delegates credential-approval decisions to
shed-desktop, and streams an all-namespace audit feed the app surfaces in **Activity**. The
broker runs inside the app — see [Installation → Credential
broker](installation.md#credential-broker). Only request metadata reaches the approval gate;
the broker stays the sole credential holder.

## Configuring for shed-desktop

In `~/.config/shed/extensions.yaml` (read by the app's broker),
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

# Optional — how long the broker waits for an approval before failing
# closed (deny). Go duration, default 25s.
# approval_timeout: 25s
```

There is nothing else to enable. With no `extensions.yaml` at all, the broker synthesizes a default that routes SSH approvals
to the app — see [Architecture → The embedded credential
broker](architecture.md#the-credential-broker).

Restart shed-desktop to apply the change. Pending requests appear in the
**Approvals** pane; `broker.status` over IPC reports the delegated namespaces, per-server
connection state, this machine's identity and the SSH signing settings in force.

It is **default-off**: an extension whose policy isn't `shed-desktop` is handled by the
broker itself (`deny-all`, `approve-all`, or native Touch ID for SSH) and is **audit-only**
to the app — its events still stream to Activity. Preferences shows an approval section
for exactly the extensions the broker delegates.

## What reaches the Approvals pane

An SSH sign request is checked before any policy decides it. The broker signs only a login
as a username in `ssh.allow_users` (default `git`) with a key it offers, or a `git` commit or
tag signature; anything else is refused at once, with no card, no notification and no change
to any grant, and appears in **Activity** with the result `refused` and the reason (for
example `username_not_allowed`). See
[SSH Agent → What the machine signs](../extensions/ssh-agent.md#what-the-machine-signs).

A request also reaches this machine only if it is your
[credential source](../extensions/source-binding.md) on that server. On any other machine
nothing arrives, and Preferences offers **Use this machine** (see
[Configuration → Credential sources](configuration.md#credential-sources)). When the source
moves away, the server drops the requests it had sent here: a card still waiting can be
answered, but the server refuses the answer, and Activity records it as `undelivered`.

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

The broker denies a request when the approval **isn't answered within
`approval_timeout`** — the same outcome as an unanswered local prompt. The app likewise
auto-denies a queued request when its countdown expires. Credentials are answered only while
the app is running: with the app quit there is no broker, and the server side fails closed.

## Testing

The approval flow is exercised hermetically by the desktop harness, which drives the real
app's own broker against mock shed-servers: a request is pushed on a mock's plugin bus
(`bus_stimulus.py`), and the decision is read from the bus answer and the broker's audit
file. The broker runs with a throwaway HOME, keys and server list (`broker_env.py`, checked
before every launch):

```bash
make -C desktop e2e-tauri    # includes test_approvals.py and test_tauri_broker.py
```

The broker's own wire output is pinned separately, against committed goldens, by the
broker wire harness (the app's broker in a test-only process; no app window, no VM):

```bash
make test-broker-wire
```

See [Test automation](test-automation.md).
