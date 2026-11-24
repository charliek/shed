# The Agents pane

The **Agents** pane shows agent sessions on your sheds: read-only roost rows,
and craze sessions you can open, watch, and stop, without leaving the dashboard.

A roost row is what that shed's own
[`roost-session`](../extensions/roost-session-hosts.md) reports. A shed with no
`roost-session` running has no roost rows. Before v0.9.0 a shed's rows were the
*union* of a guest RC hub's sessions and roost's; the guest binary
(the guest RC helper) and the hub are gone (S6,
[`charliek/shed#328`](https://github.com/charliek/shed/issues/328)). A craze
session comes from the craze hub on that shed — see
[Craze sessions](#craze-sessions) below.

The desktop does not list `machines:` hosts and does not open, kill, or launch
a roost tab. An interactive session on a host that is not a shed, or a tab you
want to drive yourself, belongs in the `roost` app. A `machines:` block left in
`~/.shed/config.yaml` is preserved by the CLI and ignored here.

## What a row is

A roost row is one roost tab with an owner — a plain shell tab in roost is not a
session and does not appear here. Every roost row is **status only**: its kind
string (`claude`, `opencode`, `codex`, and the rest roost's hooks cover), roost's
activity, and the attention dot. There is no Transcript affordance and no End
session on a roost row.

Each roost row shows:

- **Name** — the tab's title.
- **State and activity** — roost's own liveness state.
- **A sticky attention dot** mirroring roost's own notification bit — shed never
  clears it itself.
- **Open in Claude** — the `claude.ai/code` remote-control URL, Claude kinds only.

### Craze sessions

Every running shed has a **craze source**: its craze hub's live session list. A
craze session's row is the hub's: its provider and model, what it is doing (or
its last reply, dimmed, when idle), how many asks wait on you with the first
one's summary, the attached-client count, and a start error when it failed to
start. A roost tab running a craze TUI folds into that row while the hub feed
is live, and the hub row gains that tab's id; with the feed down, roost's own row for
the tab stands alone and the hub's rows show as last known. A craze row offers
**Transcript**, and **Open in terminal** when it has no tab. See
[Agent lanes § craze](agent-lanes.md#craze) for the source, the fold and the transcript.

## Empty states

The pane distinguishes four blanks, because they call for different reactions:

| State | Meaning |
|---|---|
| Loading | The session list hasn't answered yet. No claim is made about what's running. |
| Failed | The list could not be read; the failure reason is shown in full. |
| Unreachable | Every watched shed is down, and no unwatched running shed is available to set up, so the pane cannot see what is running. `agents.dump`'s `empty.state` is `unreachable`. |
| Empty | The list came back and there is genuinely nothing to show. |

Only **Empty** offers the bootstrap: a shed without a `roost-session` has no
roost rows to list; install and start it from the Sheds pane. The setup itself
happens on the shed's own card — see
[Putting `roost-session` on a shed](../extensions/roost-session-hosts.md) for the
source ladder, the consent step, and the rollback promise. (The mobile app
still installs on a `machines:` host as well; this desktop does not.)

## Starting a session

A craze session is started from the craze create sheet: **New craze session**
on a shed's card. See
[Agent lanes § Creating a session](agent-lanes.md#creating-a-session).

Opening a `claude` or `opencode` tab is
[the `shed` roost provider](../extensions/roost-provider.md), from roost's own
palette, or the `roost` app itself. This desktop has no launch dialog.

### Run a command in a tab

The desktop does not open a roost tab from a command line. `roost.run` answers
`unknown_op`. Start that tab in the `roost` app.

## See also

- [Agent lanes](agent-lanes.md) — the Transcript panel for a craze session.
- [IPC § Agent sessions](ipc.md#agent-sessions) — the `rc.*` / `roost.*` / `craze.*` ops.
- [Putting `roost-session` on a shed](../extensions/roost-session-hosts.md).
- [The `shed` roost provider](../extensions/roost-provider.md) — starting a
  `claude` or `opencode` tab from roost's palette.
