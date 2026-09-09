# Agent lanes

An **agent lane** is a live transcript for one coding-agent session, opened
directly from a machine row: activity, the message history, pending
approvals, and a prompt box, without leaving the dashboard. This page covers
the contract, the current adapter (opencode), the roost handshake that turns
a card into a lane, and what this cut does not do.

## What a lane is

`shed_core::lane` (plan 015) defines the contract every agent adapter
implements: a set of DTOs (session, transcript row, approval, event) plus one
async trait, `AgentLane` — "a coding agent with sessions, a transcript and
approvals," normalized so the desktop app (and, later, mobile) drives every
agent the same way. The contract carries no I/O of its own; each agent gets
its own adapter crate that supplies the transport, the reconnect loop, and
the translation from that agent's wire format into the contract's DTOs.

Today there is exactly one adapter, `crates/shed-opencode`, for
[opencode](https://opencode.ai). A `gx` adapter is planned as a follow-up;
nothing in the Tauri app assumes opencode is the only agent — the capability
signal described below is what turns the affordance on or off per row.

## opencode's lane

`shed-opencode` talks to **the same local HTTP server opencode's own TUI is
already running** — never a sidecar, never a second process it launches
itself. That server lists sessions, replays and streams a session's
transcript, accepts prompts, and answers permission/question approvals over
opencode's own HTTP API (the legacy `GET /event` stream; opencode's newer
`v2` event stream was still missing `session.idle` at the version this
adapter was built against, so the crate stays on the proven feed and
documents why in its module comments).

Every reconnect — the first subscribe, or a recovery after the stream drops —
is bracketed by a `Reset` event and a `Ready` event: everything the client
receives in between is staged and swapped in atomically once `Ready` arrives,
so the transcript panel never shows a half-seeded view mid-reconnect. A
`Down` event means the subscription ended; the panel keeps the last good
transcript on screen with a banner explaining why, rather than clearing it.

`shed-opencode`'s capabilities, as reported by `AgentLane::capabilities()`:

| Field | Value | Meaning |
|---|---|---|
| `kind` | `"opencode"` | The adapter identity. |
| `create` | `true` | New sessions can be opened through the contract. |
| `cancel` | `true` | A turn in flight can be aborted. |
| `approvals` | `true` | Permissions and questions surface and can be answered. |
| `interject` | `false` | See [Limits](#limits) below. |
| `history_cursor` | `false` | See [Limits](#limits) below. |

## The roost `server_url` handshake

The desktop app never scans for opencode servers and never launches one
itself. It learns a session's server address entirely from **roost**, the
per-host tab manager the app already reads machine rows from:

1. When a roost tab is an opencode session, roost's opencode plugin exposes
   that TUI's own HTTP server on a loopback port — starting one on the
   plugin's own port-0 listener if the TUI wasn't started with an explicit
   one, or simply reporting the address the TUI is already listening on if
   it was.
2. The plugin reports that address as `server_url` in the tab's ownership
   metadata (roost R10). It is always a loopback URL — roost validates this
   and drops anything else — because the server it names is only ever
   reachable from the host that ran it.
3. The Tauri app stamps a row's session DTO with an `agent_lane` field
   whenever a tab's ownership has `source == "opencode"` **and** a
   non-empty `server_url` **and** a non-empty `session_id`:

   ```json
   {
     "kind": "opencode",
     "session_id": "ses_...",
     "server_url": "http://127.0.0.1:41234"
   }
   ```

`agent_lane`'s **presence is the entire capability signal**. A card with it
gets a Transcript affordance; a card without it does not, and every
`lane.*` op on that session answers the `no_lane` error. A tab reports no
`server_url` in two cases worth knowing:

- **The session isn't opencode**, or is opencode but the plugin isn't
  installed — an ordinary status-only row, same as before this feature
  existed.
- **The plugin loaded mid-session** — `opencode attach`, or the plugin was
  installed while a session was already running. roost has no create-time
  event to hang the address on until the *next* session starts; until then
  the row is status-only. This is a known limitation, not a bug to chase.
- **The operator opted out** — roost's plugin honours
  `ROOST_OPENCODE_NO_SERVER=1`, which suppresses the loopback listener
  entirely. A tab started under it reports no address, so it is status-only
  by choice. Check this before treating a missing lane as a fault; see
  roost's `docs/guides/agents.md` for the opt-out's exact scope.

### Reaching the server: local vs. SSH

`server_url` names a port on the machine that reported it, not on the
machine running the desktop app, so how the app reaches it depends on where
that machine is:

- **Local** (the machine the app itself is running on, or a machine mapped
  by a test harness's socket table) — the app dials `server_url` directly.
- **Everything else** — the app opens `ssh -N -L <local>:127.0.0.1:<port>`
  to the machine and points the adapter at `http://127.0.0.1:<local>`
  instead. This forward is shared: two sessions on the same agent server on
  the same machine ride one `ssh -N` child. A forward is torn down when its
  last lane closes, when the tab disappears from roost's own snapshot (the
  tab's process died), or when a restarted tab reports a new `server_url`
  (a new ephemeral port is a different server, even for the same session
  id).

### The `--port 0` alias, for hosts without roost's plugin

The roost handshake above is how the **desktop app** discovers a lane
automatically. The adapter itself has no roost dependency at all — it
targets whatever base URL it is given. If a host doesn't have roost's
opencode plugin installed, or you want to drive a session by hand without
opening the app, start opencode directly with an explicit loopback port and
point the crate's own CLI at it:

```bash
opencode --port 0 --hostname 127.0.0.1
# note the printed port, then:
cargo run -p shed-opencode --example lane -- \
  http://127.0.0.1:<port> <session_id> watch
```

`examples/lane.rs` (`shed-opencode-lane`) supports `sessions`, `history`,
`watch`, `send <text>`, `cancel`, `approvals`, and
`answer <id> allow-once|allow-always|reject` — the same verbs the desktop
app's `lane.*` ops drive. `OPENCODE_SERVER_PASSWORD` is honored if opencode's
own password gate is set.

## Driving a lane

Once a row carries `agent_lane`, opening its Transcript affordance calls
`lane.open {machine, session_id}` and mounts the panel:

| Op | Does |
|---|---|
| `lane.open` | Ensures a subscription (idempotent — a second call for an already-open lane re-answers from the existing entry). |
| `lane.messages` | The staged transcript: up to the last 500 rows, current activity, generation, and a `stale` reason when the lane is `Down`. |
| `lane.approvals` | Pending permissions and questions, root session plus its children. |
| `lane.send` | Queues a prompt. |
| `lane.cancel` | Aborts the turn in flight. |
| `lane.answer` | Answers one approval — a permission decision (`allow-once` / `allow-always` / `reject`) or a question's options plus optional free text. |
| `lane.close` | Ends the subscription; the last close on a shared SSH forward tears it down. |

A failure comes back as `{code, message}` with the contract's own snake_case
codes (`unauthorized`, `unknown_session`, `unknown_approval`,
`already_submitted`, `already_resolved`, `not_accepting`, `unavailable`,
`failed`), plus `no_lane` for a row with no `agent_lane` stamp at all.

## The password / status-only rule

There is **no credential source in this cut**. If opencode's server has
`OPENCODE_SERVER_PASSWORD` set, every request the adapter makes without
credentials answers `401`, which the adapter maps to `LaneError::Unauthorized`
and the panel renders as an inline error — the transcript never loads. This
is deliberate and documented, not a bug: a password-protected opencode
session is **status-only** in this build. The client type
(`shed_opencode::BasicAuth`) already exists for the follow-up that adds a
config field to supply one; nothing wires it up yet, and no test claims
otherwise.

## Limits

- **No interject.** `capabilities().interject` is `false`. Every send goes
  through opencode's `prompt_async`, which is accepted and ordered after
  whatever the session's runner is already doing — it does not preempt a
  turn in flight. There is no "type over the agent" affordance.
- **No resume-from-cursor.** `capabilities().history_cursor` is `false`.
  Every reconnect refolds the full transcript from the top rather than
  resuming from a client-held position; this is what the `Reset` … `Ready`
  bracket exists to make invisible to the panel. A durable, resumable stream
  (opencode's `session.next.*`) is future work.
- **Child sessions.** roost attributes a subagent's own events to the
  parent tab, so a session can sit blocked on a **child's** approval that
  never appears anywhere else. The adapter tracks a root session's
  descendants and surfaces their pending approvals — answering one routes
  to the child's own request id — but the **transcript stays root-only**:
  a child session's messages are never rendered, only the fact that it is
  waiting on you.

## See also

- [Machines and RC sessions](rc-sessions.md) for how a machine row and its
  sessions are discovered in the first place.
- [IPC](ipc.md) for the full `rc.*` / machine op surface a lane's row lives
  inside.
