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

There are two adapters today: `crates/shed-opencode` for
[opencode](https://opencode.ai), and `crates/shed-gx` for
[gx](https://github.com/charliek/grok-build)'s remote lane (`gx-remote-api`).
Nothing in the Tauri app assumes either is the only agent — the capability
signal described below is what turns the affordance on or off per row, and
`Lanes::open` picks a concrete client by the row's own `kind`, never by
assuming one.

gx is the *second* adapter, and that matters beyond feature count: a contract
validated against one implementation is a design, not yet a contract. Building
`shed-gx` forced real corrections into `shed_core::lane` itself — recorded in
the module's own doc as "what the gx adapter changed" — because gx's remote
lane has things opencode's local server never needed: a bearer token, a
resumable cursor, and permission options whose semantic `kind` is separate
from their id and not always unique. Those corrections are what the [gx's
lane](#gxs-lane) section below is mostly about.

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

## gx's lane

`shed-gx`'s capabilities, as reported by `AgentLane::capabilities()`:

| Field | Value | Meaning |
|---|---|---|
| `kind` | `"gx"` | The adapter identity. |
| `create` | `true` | `POST /v1/sessions` mints a new session. |
| `cancel` | `true` | A turn in flight can be aborted. Refused with `not_accepting` on an idle session — the panel gates the affordance on `Working` so no human sees the refusal. |
| `approvals` | `true` | Permissions, questions, plan approvals and MCP elicitations all surface and can be answered. |
| `interject` | `true` | `lane.send` with `mode: interject` preempts the turn in flight — opencode's lane has no equivalent verb, so its capability stays `false`. |
| `history_cursor` | `true` | gx's `lastEventId` lets a reconnect resume from a cursor instead of refolding history from the top — see [Reconnects](#bounded-silent-resume-reset-ready-is-the-reseed-bracket-only) below. |

### Discovery and the `healthz` / `instanceId` pin

A gx leader writes a discovery record next to its home: `$GROK_HOME/gx-remote.json`
on the default socket, `gx-remote-<16hex>.json` on any other —
`{url, pid, instanceId, socketPath, tokenFile, version, startedAt}`. The token
itself is one file, `$GROK_HOME/gx-remote.token` — **never** suffixed, even
when the record is, because gx keeps **one token per `$GROK_HOME`**, shared by
every leader on it; a leader restart changes `instanceId`, never the token.

Before the adapter sends its first bearer request — and again at the start of
every transport epoch (every reconnect, every failure) — it calls the
token-free `GET /v1/healthz` and compares the `instanceId` it returns against
the discovery record. Only on a match does the token go out. A mismatch
triggers one re-discovery attempt; still mismatched, or no live record at all,
answers `Unavailable` and leaves the epoch unpinned — no bearer request is
ever sent on a lane the adapter cannot prove is the one discovery described.
This gate (`ensure_pinned`) is one serialized async lock per client, so
concurrent first callers share a single pin instead of racing separate ones.

### Two URLs, never conflated

Same invariant as opencode's SSH forward (above), stated explicitly here
because gx's credential pin depends on it: a gx lane has a **reported** URL
(what a discovery record's `url` is matched against, and what roost stamps as
`gx.remote`) and a **dial** URL (where HTTP actually goes — the same address
on a local machine, `http://127.0.0.1:<local>` over a forwarded SSH tunnel).
`GxClient` is constructed with both, and a `GxTransport::dial()` hook is
called before **every** connect attempt — the first verb, every SSE
(re)connect, every verb after a failure — so a forward that moved is noticed
rather than dialled blind. Discovery matches the reported URL; `healthz` and
every bearer request ride the dial URL.

One consequence worth stating plainly, because it is easy to get backwards:
`gx.remote`'s value is **slash-free** (`http://127.0.0.1:2431`), and
`loopback_base_url` — the same validator roost itself uses to decide whether
to publish the key at all — rejects a trailing slash. A trailing slash
anywhere in the reported URL means **no lane at all**, not a degraded one.

### The credential rule

The lane contract carries **no credential type**. roost's job stops at
reporting *where* an agent is; *how* a client is let in is the client's own
problem, deliberately kept out of `shed_core::lane` so a third adapter never
inherits a credential shape gx happens to need. Concretely: an adapter takes
its credentials at construction, from a source the client supplies —
`GxClient::new` takes a `credentials: Arc<dyn GxCredentialSource>`. The Tauri
app's implementation reads a local `$GROK_HOME` directly (never by shelling
out) when the machine is local, or runs one POSIX `sh -c` probe over the
machine's SSH reach otherwise — checking the same things gx's own reader
checks (a regular file, mode `0600`, owned by the caller) before it will hand
a token back, and refusing a symlinked or wrongly-permissioned token exactly
as gx does. `StaticCredentials` is the fixed-value implementation the crate's
tests, its `examples/lane.rs` CLI, and the phone's first cut use.

### Kind promotion is a hint, not liveness

A gx tab reports `source: "grok"` in roost whether or not a lane is up.
`RcKind::Gx` is derived, not reported: `source == "grok"` **and**
`metadata["gx.remote"]` passing `loopback_base_url` promotes the row to `Gx`;
`source == "grok"` with no such key (or one that fails validation) leaves it
`Grok` — creatable, but lane-less by design (see below). Promotion is one-way
per report, and **a dead lane does not un-stamp the row**: a tab stays `gx`
for as long as roost keeps the metadata key on it, even after the leader
behind that key has died. `lane.open` is what surfaces the difference — it
answers `unavailable` rather than the Transcript affordance silently failing
to appear.

### `unsupported_lane`

`LaneEntry.client` is `Arc<dyn AgentLane>`, keyed by the full stamp
`(kind, server_url)`; no concrete adapter type is named anywhere outside
`Lanes::open`'s own match on the stamp's `kind`. A kind that is neither
`"opencode"` nor `"gx"` answers `LaneFailure::UnsupportedLane(kind)` — IPC
code `unsupported_lane` — instead of the app guessing at a client to
construct.

### Segments, not tokens — and lossless

gx's chunks stream token by token; the transcript panel does not render at
that granularity. A chunk streak closes and emits a row when a
transcript-bearing update of a different kind arrives, when the prompt
changes, when `turn_completed` fires, or when the streak passes **8 KiB** —
whichever comes first — and a streak that has gone silent for **2 seconds**
flushes on its own rather than waiting indefinitely for a reason to close.
Segmentation is **lossless**: text is split only on character boundaries,
never truncated to fit a cap, and the chunk after a cut opens a fresh streak
rather than losing the tail of the one before it.

### Bounded silent resume; `Reset … Ready` is the *reseed* bracket only

opencode has no cursor, so every reconnect refolds its whole transcript from
the top, and `Reset … Ready` exists purely to make that invisible to the
panel. gx has `Last-Event-ID`, so a reconnect the server accepts from the
client's cursor resumes **silently** — no `Reset` at all: the ring, the open
streak, the generation and the panel's view all survive untouched. That
silent path is bounded — at most three attempts within thirty seconds of the
first loss — past which, or on an explicit server `reset`, or on a cursor the
server no longer recognizes, the adapter **reseeds**: the open streak is
discarded, history is rebuilt from scratch, and the rebuild is bracketed with
`Reset … Ready` exactly as opencode's reconnect always is. `Reset.reason`
(`connect`, `cursor_lost`, `server_reset:<r>`, `stall`) is free text for a log
line — nothing switches on it.

Either way, **every** reconnect — silent or not — re-fetches what the SSE
stream itself never replays (approvals, the session row), because a resumed
stream is not a complete one; a held-pending approval the re-fetch no longer
lists comes back as a `Resolved` tombstone rather than staying stuck on
screen forever. Repairing the transport (re-establishing an SSH forward) is
not a `LaneEvent` either — it rides the `GxTransport::dial()` hook described
above, called before every connect attempt, so a forward that needed
re-ensuring never forces a visible reseed on its own.

### The `option_for` ambiguity refusal

This is the headline contract change, and it exists because of a real gx
permission recorded live, not a hypothetical one: asked to run `id -un`, a
live gx leader offered **five** options, and **two of them** declared
`kind: "allow_once"` — "Yes, proceed" and "Yes, and don't ask again for
anything (always-approve mode)". `LaneApprovalOption.kind` is the option's
*semantic* kind (`allow_once` / `allow_always` / `reject_once` /
`reject_always`); `id` is opaque and independent of it — an agent can offer
several options of the same kind, so a bare decision
(`AllowOnce` / `AllowAlways` / `Reject`) is not always enough to pick one.
When it is ambiguous, `LaneApproval::option_for` **refuses to guess** — it
returns `None`, and the adapter answers `BadRequest` — rather than resolving
the tie by offered order, which on gx's own five-option set would have
silently selected the option that turns off every future permission prompt.

The panel's answer is capability-driven, not a fixed three-decision form: it
renders every option an approval offers, under the agent's own label, in the
agent's own order, and posts back `LaneAnswer::Choice { option_id }` — the
exact id the human pressed — for every adapter. Clients send
`{choice: "<id>"}` over IPC. opencode's three options carry the same labels
either way, so nothing visibly changes there, and the scripted
`{permission: "allow-once"}` form still works when the kind it names is
unambiguous on the approval being answered.

### Lane-less grok

`RcKind::Grok` — a `gx` session with no bound lane, or one started
`--no-leader` / under `GX_REMOTE_DISABLE=1` — is creatable and renders as an
ordinary status-only row, same as any RC kind before agent lanes existed: no
Transcript affordance, because there is no lane to open.

### Deferred: free-text question answers

gx keys a question's answer set by the question's own text and accepts a list
of chosen labels; a free-text reply is really the label `"Other"` plus an
annotation the label list has nowhere to carry. The contract has no field for
that annotation yet, so free-text answers are not supported against gx in
this cut — `custom` stays `false`. Filed as a follow-up alongside the phone's
DTO mirror.

## Driving a lane

Once a row carries `agent_lane`, opening its Transcript affordance calls
`lane.open {machine, session_id}` and mounts the panel:

| Op | Does |
|---|---|
| `lane.open` | Ensures a subscription (idempotent — a second call for an already-open lane re-answers from the existing entry). |
| `lane.messages` | The staged transcript: up to the last 500 rows, current activity, generation, and a `stale` reason when the lane is `Down`. |
| `lane.approvals` | Pending permissions and questions, root session plus its children. |
| `lane.send` | Queues a prompt (`mode: queue`), or preempts the turn in flight (`mode: interject`) when the lane advertises `interject` — the panel shows the toggle only then, and only enables it while the turn is `Working`. |
| `lane.cancel` | Aborts the turn in flight. |
| `lane.answer` | Answers one approval. The scripted three-decision form (`{permission: "allow-once" \| "allow-always" \| "reject"}`) or a question's options plus optional free text still works when it is unambiguous; the panel itself always sends `{choice: "<id>"}` — the exact option id the approval offered — because an agent can offer several options of the same decision kind (see [gx's `option_for` refusal](#the-option_for-ambiguity-refusal)). |
| `lane.close` | Ends the subscription; the last close on a shared SSH forward tears it down. |

A failure comes back as `{code, message}` with the contract's own snake_case
codes (`unauthorized`, `unknown_session`, `unknown_approval`,
`already_submitted`, `already_resolved`, `not_accepting`, `unavailable`,
`failed`), plus `no_lane` for a row with no `agent_lane` stamp at all and
`unsupported_lane` for a row whose `agent_lane.kind` names no adapter this
build has.

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
