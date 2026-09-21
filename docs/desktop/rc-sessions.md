# The Agents pane

The **Agents** pane shows agent sessions running anywhere shed can see them — on a
`machines:` entry or inside a shed — and lets you launch, watch, and end them without
leaving the dashboard.

Since v0.9.0 (S6, [`charliek/shed#328`](https://github.com/charliek/shed/issues/328)) every
session here is a **roost tab**. A shed's agent sessions are exactly what its own
[`roost-session`](../extensions/roost-session-hosts.md) reports, the same way a machine's
always have been — a shed with no `roost-session` running has none. Before v0.9.0 a shed's
rows were the *union* of a guest RC hub's sessions and roost's; the guest binary
(`shed-ext-rc`) and the hub are gone, and roost is now the only source.

## What a row is

A row is one roost tab whose owning process is a recognized agent (`claude`, `codex`,
`cursor-agent`/`cursor`, `opencode`, `gx`/`grok`) — a plain shell tab in roost is not a
session and does not appear here. Each row shows:

- **Name** — the tab's title.
- **State and activity** — roost's own liveness state, plus a live activity badge for
  agents with a [structured lane](agent-lanes.md) (opencode, gx) attached.
- **A sticky attention dot** mirroring roost's own notification bit — shed never clears it
  itself.
- **Transcript** — opens the [agent lane](agent-lanes.md) panel, only on a row that carries
  an `agent_lane` stamp (a lane-capable agent whose roost plugin reported a live server
  address). A row without one is status-only.
- **Open** — opens a terminal on the session, addressed by the row's own machine (a shed's
  own SSH endpoint, or the machine's configured address) — the same button whether the row
  is a shed or a `machines:` entry.
- **Open in Claude** — the `claude.ai/code` remote-control URL, Claude kinds only. Other
  agents have no browser URL.
- **End session** — closes the roost tab (`tab.close`). Idempotent; a session already gone
  is treated as already ended.

## Empty states

The pane distinguishes three blanks, because they call for different reactions:

| State | Meaning |
|---|---|
| Loading | The session list hasn't answered yet. No claim is made about what's running. |
| Failed | The list could not be read; the failure reason is shown in full — "the backend is not up" and "that host refused" need different fixes. |
| Unreachable | Every configured machine is unreachable, so the pane cannot see what's running (see the Machines pane for why). |
| Empty | The list came back and there is genuinely nothing to show. |

Only **Empty** offers the bootstrap: "Agent sessions live in a roost-session — on a
machine, or inside a shed. A shed without one has no sessions to list; install and start it
from the Sheds pane, then launch an agent here." The setup itself happens on the shed's own
card in the Sheds pane — see
[Putting `roost-session` on a shed or machine](../extensions/roost-session-hosts.md) for the
source ladder, the consent step, and the rollback promise.

## Launching a session

New sessions are opened through roost, not through this binary directly:
`roost.launch`/`machine.launch` open a tab running the chosen agent binary in a chosen
working directory — see [IPC § Agent sessions](ipc.md#agent-sessions) for the op surface.
The launch dialog does not offer a prompt field: roost owns the tab from the moment it
opens, and a typed kickoff has no wire to travel down yet
([`charliek/shed#366`](https://github.com/charliek/shed/issues/366) tracks delivering one).
Kicking off an agent from roost's own command palette instead of this dialog is
[the `shed` roost provider](../extensions/roost-provider.md).

## Machines

A **machine** is a native host reached over SSH that runs a `roost-session` — no shed
server in the path, no TLS pin, no control token. Machines come from the `machines:`
section of `~/.shed/config.yaml`, read once at startup (no in-app add/edit). A machine
worth showing even when it has no sessions and cannot currently be reached (asleep,
off-network) contributes its own row on the Machines pane, naming why.

## See also

- [Agent lanes](agent-lanes.md) — the Transcript panel: opencode and gx, capabilities,
  reconnects, and the roost `server_url` handshake that turns a row into a lane.
- [IPC § Agent sessions](ipc.md#agent-sessions) — the full `rc.*`/`machine.*`/`roost.*` op
  table this pane is driven by.
- [Putting `roost-session` on a shed or machine](../extensions/roost-session-hosts.md) — the
  bootstrap this pane's empty state offers.
- [The `shed` roost provider](../extensions/roost-provider.md) — starting an agent from
  roost's own palette instead of this pane.
