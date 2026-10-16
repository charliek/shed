# Agent lanes

An **agent lane** is a live transcript for one coding-agent session, opened
directly from its row in the Agents pane: activity, the message history,
pending approvals, a prompt box and, where the agent offers them, Stop and the
session's settings, without leaving the dashboard. This page covers the
contract, the two adapters behind it (craze and opencode), how the desktop
reaches each one, and what the current build does not do.

## Lanes and sources

`shed_core::lane` is the contract every agent
adapter implements, at two levels:

- A **source** (`AgentSource`) is a machine's view of its sessions: it streams
  the live session list, says what a create can start, creates a session, and
  opens a lane for one row's id.
- A **lane** (`AgentLane`) is one session: its transcript and activity as a
  stream, and its verbs — send, cancel, answer an approval, read and change
  its settings, stop it. A lane is bound to its session id for its whole life,
  so no verb takes one.

The contract is pure DTOs and two async traits; each agent's adapter crate
supplies the transport, the reconnect loop and the translation from that
agent's wire into the contract. Two adapters implement both levels:

| Adapter | Crate | What it talks to | Which rows |
|---|---|---|---|
| **craze** | `crates/shed-craze` | One machine's [craze](https://github.com/charliek/craze) **hub**, which fronts every cursor, grok, gx and native session on that machine | Every craze session the hub lists; the desktop runs one craze source per host |
| **opencode** | `crates/shed-opencode` | The local HTTP server the opencode TUI is already running | An opencode roost tab that reported its server address |

Claude sessions are roost tabs with no lane. Any other agent run directly in a
roost tab (`codex`, say) is a plain row: roost's status, no transcript.

### What a session can do rides the stream

**Capabilities are per session.** Every seed of a lane's stream carries a
`Capabilities` frame (and a `Settings` frame when those capabilities say
`settings`), and either is sent again when it changes — a craze session's
capabilities can change with its incarnation. There is no capabilities getter;
the desktop reads them from the lane's staged view, so `lane.open` answers the
session row alone and `lane.messages` carries `capabilities` and `settings`.

| Field | craze | opencode | Meaning |
|---|---|---|---|
| `kind` | `"craze"` | `"opencode"` | The adapter identity. |
| `cancel` | craze's `cancel` | `true` | A turn in flight can be aborted. |
| `approvals` | craze's `approvals` | `true` | Asks surface and can be answered. |
| `interject` | craze's `interject` | `false` | `lane.send` with `mode: interject` preempts the turn in flight. |
| `history_cursor` | `true` | `false` | A reconnect can resume from a cursor silently, with no reseed. |
| `settings` | the session has a model, mode or option to show | `false` | The settings chip and sheet are offered. |
| `stop` | craze's `stop` | `false` | The contract can end the session. A session hosted by a craze TUI cannot be stopped this way; hub-created and `craze serve` sessions can. |
| `set_title` | `true` | `false` | The session can be renamed (`lane.set_title`, the composer's `/rename`). |
| `slash` | craze's `slashCatalog` (craze 0.1.1) | `false` | The session publishes a slash catalog, and the composer offers a `/` menu once it has arrived. |

The panel hides an affordance the session's capabilities do not offer, rather
than showing it disabled.

### The bracket, and stale versus ended

Every reseed of a lane is bracketed by `Reset` … `Ready`: everything the
client receives in between is staged and swapped in atomically when `Ready`
arrives, so the transcript never shows a half-seeded view. A `Ready` swaps a
staged seed only when its generation matches the staged `Reset`'s.

Not every reconnect is a reseed. A lane that can resume from its cursor
(craze) announces a transport loss with a non-terminal **`Stale`** frame,
keeps every row, and ends the outage with a lone `Ready` of the same
generation. **`Down`** is the only frame that ends a lane. The view keeps the
two facts apart:

- **stale** — set by `Stale` or `Down`, cleared by a `Ready`. It drives the
  banner: "not live · reconnecting — showing the last known transcript".
- **ended** — set only by `Down`. It is the only thing the desktop reopens a
  lane on. Reopening on a stale mark would throw away the cursor a silent
  resume needs.

A lane that ended with `unknown_session`, `session_closed` or
`start_failed:<cause>` is never reopened: the session is gone, closed or never
started. Neither is one the desktop ended itself — `session_gone`, `hub_gone`
or `host_removed` (see [A lane outlives its row](#a-lane-outlives-its-row)).
Any other `Down` (the agent restarted, a reconnect bound ran out) is reopened
with a short backoff.

The desktop keeps a third fact apart from both, **finished**: the lane is
over for good — one of those final reasons arrived, and nothing will reopen
it. `lane.messages` answers it as `finished`. A finished lane stays on screen
and readable (its transcript, approvals and last settings) but holds no
connection, however it finished — the session's own final event included: its
subscription, its adapter and its share of any SSH forward are released (the
last share out tears the forward down). Every verb on it is refused
`lane_ended` ("the session has ended") at once — a verb already under way when
the lane finished runs its course — and the panel disables its composer, Send,
Interject and Cancel. `ended` alone is not
enough to key that on: a retryable `Down` sets it too, and the desktop is
already resubscribing behind it.

A source streams the same way: `Reset` … `Session`* … `Capabilities` …
`Ready`, then upserts and removals. Its outage is the non-terminal `Offline`
frame: the desktop keeps the last listed rows and marks them stale.

## craze

craze is the provider abstraction for every agent shed runs other than Claude
and opencode. One per-machine **hub** lists the machine's craze sessions (its
roster), says which providers a create can start, starts sessions, and splices
a client through to any one session's **host**. `CrazeSource` is one hub;
`CrazeLane` is one session, reached through the hub's splice.

shed needs craze **0.1.0 or later** on a host. An older craze is reported as
too old; a host without craze lists no craze sessions and says nothing about
it.

### A source per machine: this machine eager, every other attach-only

The desktop runs one craze source per host: this machine, every `machines:`
entry, and every running shed (a shed gets one when it is first seen running,
whether or not roost is there).

A shed shows in the Machines pane once something answers there: its
roost-session, or its craze hub (the roster's first answer, or a session
created there). Until then a running shed whose craze is **dormant** is
offered **New craze session** on its Sheds-pane row and in the New-session
dialog's "Where", without being listed, and one whose craze is **too old**
says so on its row. A shed without craze says nothing.

- **This machine is eager.** Its source holds one roster connection from
  launch. `bridge --hub` joins the hub already running there and starts one
  only when none is running yet — then in the app's own session, on a Mac the
  GUI login session, which is what lets cursor run there. A hub something else
  started first keeps its own session and environment.
- **Every other host is attach-only.** The desktop never starts a hub on
  another machine in the background. While a host has no hub, its source runs
  only a find-only probe, `craze providers --hub --json`, which never starts
  one; the roster connection opens once the probe finds a hub, and it too is
  find-only (`craze bridge --hub --no-start`, below).

A remote host's source cycles through these states (this machine's is live,
offline or absent, and retries its roster connection rather than probing —
every 30 s while craze is not installed or too old):

| State | Meaning | What runs |
|---|---|---|
| `live` | Attached to the hub's roster | One roster connection: `craze bridge --hub --no-start` (craze 0.2.0 and later), or the probe and then `craze bridge --hub` for an older craze |
| `dormant` | craze is there, no hub is running — the probe said so, or the roster's own `--no-start` connection found none; nothing is listed from craze (a dormant shed is offered New craze session without being listed) | The probe again every 30 s |
| `offline` | The roster dropped, the probe could not ask (`cause: unreachable`), or craze is too old (`cause: too_old`) | The probe: about a second after a live roster ends, on a backoff (1 s → 30 s) while the host is unreachable, every 30 s while craze is too old |
| `absent` | Never reached, or craze is not installed (`cause: not_installed`) | The probe; every 30 s while craze is not installed |

**No background connection can start a remote hub.** Every remote connection
the desktop makes on its own — the roster's, its redials, and every lane's
background reconnects — runs craze 0.2.0's `craze bridge --hub --no-start`,
which attaches to a hub that is already running and otherwise exits 3
(`craze bridge: no hub is running`) without starting one. When the roster's
connection gets that answer, the source goes `dormant` (its rows leave with
the hub) and probes again 30 s later; a lane stays stale and retries within
its own bound. There is no probe in front of each connection: the connection
itself cannot start a hub. Only the create sheet's explicit actions start one
on a remote host, and only they run plain `craze bridge --hub`: opening it
(its provider list), creating a session, and its Resume half (listing saved
sessions, resuming one). After any of them, the host's source probes at once
and attaches to the new hub. (Open in terminal runs `craze
attach`, which joins the session over the session's own control socket and
starts nothing.)

**An older craze falls back.** A craze older than 0.2.0 refuses the flag
(`craze bridge: unknown or malformed flag`). The host's connections then fall
back together, once, for as long as the desktop keeps that host's source
(until the host is removed and added again, or the app restarts): each runs
the probe on the same SSH ControlMaster first and starts `craze bridge --hub`
only when a hub is running. On that path one race remains, accepted rather
than closed: the probe sees a hub that exits before the bridge behind it
joins, and the bridge starts a new one. craze 0.2.0's `--no-start` is what
closes it — update craze on that host.

### Reaching craze: the remote command

Every connection to a hub runs `craze bridge --hub` — or, for a remote
host's background connections, `craze bridge --hub --no-start` (above) —
through one composed `sh -c` line (`shed_core::craze`): craze's published
binary-finding ladder, verbatim, which tries, in order:

1. `$HOME/.local/bin/craze` (only when `HOME` is absolute)
2. `craze` on the PATH (`command -v`, absolute results only)
3. `/opt/homebrew/bin/craze`
4. `/usr/local/bin/craze`
5. `/home/linuxbrew/.linuxbrew/bin/craze`
6. `/usr/bin/craze`
7. `$HOME/.nix-profile/bin/craze`
8. `/etc/profiles/per-user/$USER/bin/craze`
9. `/run/current-system/sw/bin/craze`

None found prints `craze: command not found` and exits 127, which the desktop
reads as not installed. `~/.local/bin` is first, so a craze placed there wins
over a packaged one. `~/go/bin` is not on the ladder: a `go install` build
must be copied or linked into `~/.local/bin`.

**The exec PATH.** The ladder resolves craze against the PATH it was given,
unchanged. What changes is the PATH craze itself runs with: the ladder's own
directories, in its own order, then the original PATH. The bridge's
environment becomes the hub's, and the hub hands it to every session it
starts, so without this an SSH exec's PATH (`/usr/bin:/bin`) or a macOS GUI
app's would hide every agent installed in `~/.local/bin` or under Homebrew, and
craze would report those providers unavailable. No login profile is sourced.

Two caveats follow from the hub inheriting its environment:

- **A hub's environment is fixed at birth.** A hub that something else started
  — a craze TUI, a `craze new` over SSH — keeps that process's PATH and
  variables until it exits, 60 seconds after its last session and its last
  client have gone. While the desktop holds a roster connection, the hub stays.
  The exec PATH is all the desktop adds: shell exports such as a provider's
  API key or `SSH_AUTH_SOCK` are not part of a hub the desktop started, and the
  user's own `craze new` on that machine inherits that hub too. Use craze's own
  key store (`craze auth login`) for keys.
- **An agent installed anywhere else needs `[agents]`.** An agent binary in a
  directory the ladder does not list is found only through an absolute path in
  `~/.craze/config.toml`:

  ```toml
  [agents]
  cursor = "/opt/cursor/bin/cursor-agent"
  ```

A shed's SSH server runs every command through `bash -lc`, so a noisy login
profile can print before craze does. The client skips up to 16 non-JSON lines
(4 KiB) before the first reply and keeps them for the error message; after
that, any non-JSON line is a protocol fault.

### Rows, and the fold with roost

A craze session's row is the hub's: its provider and model, what it is doing
(or its last reply, dimmed, when idle), how many asks wait on you with the
first one's summary, how many clients are attached, and a start error when it
failed to start. Its row id is the session's **hostId**, which survives a hub
restart. While the source is not live, the rows it still holds show as last
known (`stale`, `approximate`).

A craze session can also be a roost tab: a craze TUI claims its tab, and roost
reports it as owned by craze. For craze sessions **the hub row is the row**,
computed per machine by `shed_app::craze_rows::fold_plan`:

- **While the hub feed is live,** every craze-owned roost tab is folded away. A
  tab that names a hub row's provider session id attaches to that row, which
  gains the tab's `tab_id` (and with it End tab); a tab that names none is
  hidden.
- **While the feed is dormant, offline or absent,** nothing is folded: roost's
  own craze rows show as they would with no craze source at all.
- Only craze ownership folds, the newest tab of a session is the one that
  attaches, a fold never crosses machines, and two hub rows that claim one
  provider session resolve to the newer one (`since`, else `startedAt`, then
  the greater hostId).

A row's actions follow from that: **Transcript** opens its lane; a row with a
tab offers **End tab**, which closes that roost tab by its `tab_id` (never by
the row's hostId); a row without one offers **Open in terminal**.

### Creating a session

**New craze session** is offered where a host's craze can create a session:
live with a hub that can create, or dormant, in which case opening the sheet
is the explicit action that starts its hub.

- A dormant **machine** is offered it on its Machines-pane card (and, once it
  lists sessions, on its group in the Agents pane).
- A dormant **shed** is offered it on its Sheds-pane row. Until its roost
  answers or it has a hub, a shed is not listed, so its row is where it is
  offered; once listed, its card and its Agents-pane group offer it too.
- Both are a "Where" choice in the New-session dialog ("— new craze session").

A hub too old to create says "update craze on this machine to create sessions
here". A craze too old for shed says "craze on this machine is too old for
shed; update it" on the machine's card or the shed's row, and offers nothing.

A shed listed by craze alone — a session created there, or its hub's roster,
listed it, and no roost-session has answered there (so nothing watches one) —
reads **craze only**
on its Machines-pane card and in the sidebar, with "no roost session here yet
— craze sessions only" beneath its name, rather than "connecting". It ranks
with the reachable hosts, its sidebar dot is neutral, and the Agents pane does
not count it as unreachable. Its card still offers to set up roost.

The sheet reads craze's create options afresh on every open; shed stores no
defaults or history of its own.

- **Provider.** craze's providers, in craze's order. Only a `ready` provider
  can be picked; one that `needs_setup` or is `unavailable` is shown dimmed,
  with craze's reason and fix, and cannot be selected. craze's default
  provider is preselected only when it is listed and ready, else the first
  ready one. With none ready, the sheet says "no provider is ready on this
  machine" and Create is disabled.
- **Directory.** craze's recent directories, newest first, as one-tap choices,
  or a typed path. It must be absolute; craze checks that it exists.
- **Name.** Optional, shown when the machine's live hub can name a session at
  create (craze 0.1.1's `createTitle`; a dormant machine's sheet shows it once
  the hub it starts is live). The session is titled from its first frame: the
  create's own answer and the transcript's first snapshot already carry the
  name, so it is never shown untitled and renamed after. The
  name is sent trimmed, and not at all when blank; craze normalizes it and
  refuses one that is blank once normalized, which the sheet shows under the
  field. A retry of an unknown outcome sends exactly the name the first send
  did; changing the name, like any edit, starts a new request.
- **First prompt.** Optional, multi-line.
- Not in the sheet: model, effort, fast and permission mode. A session starts
  on its provider's default model, and a sheet-created session runs in craze's
  default permission mode, `bypass` — its transcript header says "runs tools
  without asking" once the session's attach has stated it (the lane's live
  session row, not the create's own, which states no permission mode).

| State | What the sheet shows |
|---|---|
| Loading | The provider list is disabled until craze answers. |
| Options failed | The error, and Retry. |
| Offline, too old, not installed | The note; Create disabled. |
| Submitting | Create disabled. Closing the sheet keeps the create running, and its session appears as a row. |
| Refused | craze's error inline: a path problem beside the directory, a start failure's cause verbatim, anything else as craze said it (`unavailable` adds "try again"). |
| Outcome unknown | "craze did not answer, so the session may have been created: check the session list. Try again resumes the same request." |
| Created | The sheet closes and the session's transcript opens. A first prompt craze did not take shows as a toast. |

The typed form is never cleared by any of these states, and is kept per
machine across closing and reopening the sheet until a create succeeds.

**The request id.** A create carries a request id, and craze answers a repeat
of that id with the first create's answer — its failure too — for ten
minutes. So the id lives only while the outcome is unknown: the automatic
retry (once, on a dropped connection or the 120 s deadline), a Try again after
an unknown outcome, and reopening the sheet mid-submit all reuse it, so a lost
answer never makes a second session. Any definite answer — a session or any
refusal — and any edit of the form mint a new one.

The boundary of that guarantee: Try again can never start a **second session
while the first one is still running**. Within ten minutes of craze's original
answer, a retry gets that same answer back, even if the session has since
ended (shed does not resurrect the ended row). Only after that, and only if the
session has ended and craze finds no live session for the request, does a retry
start a new one.

**A created session is listed at once.** The roster can list a new session a
moment after the create answers, so `CrazeSource` keeps the created row apart
until a roster lists or removes that hostId, a lane opened on it sees the
session end, or ten minutes pass (at most 64 rows). `CrazeSource::created_rows()`
is the one authority the desktop lists them from; it keeps no copy. A hostId a
roster removed, or whose session a lane saw end, is not listed again within
craze's ten-minute replay window, so a replayed create of a session that has
since ended opens nothing.

The lane is what ends a session the roster never listed. craze's roster sends
a removal only for a session it has already sent, so a session created and
stopped before the roster's next flush is never removed by it. Its lane's end
lets the created row go instead: a `Down` of `session_closed`,
`unknown_session` or `start_failed`, or, once the host has taken a stop, any
end of the lane, including a transcript closed before the close arrived. A
transcript closed while the stop was still in flight leaves no lane to see the
close, so the stop's receipt says it instead. The source then tells the
desktop to re-read the listing, since no roster frame will.

### Resuming a saved session

A craze machine remembers the sessions it ran. A **saved** session is one it
remembers that nothing runs now, and craze 0.2.0 can run one again: the hub's
`sessions.saved` lists them and `session.reopen` starts a host that loads one
(or joins the host already serving it).

The create sheet shows a **New | Resume** control when the create options it
just read say the hub can reopen (`reopen`, from the same hub `hello`). That
covers a dormant machine too: opening the sheet starts its hub, and the
control appears once that hub answers. A hub from before craze 0.2.0 shows no
control. A hub from before 0.2.0 can keep running after craze is upgraded,
until it is idle. Asked anyway (the `craze.saved` or `craze.reopen` op), it is
refused `unavailable` in one wording, "this craze hub (craze <version>) does
not offer reopening saved sessions". The version reads `version unknown` when
the app has not read it, and a hub that says a version below 0.2.0 and its
pid gets the remedy too: end it with `kill <pid>`.

The sheet opens on New. **Resume** reads the saved list the first time it is
chosen after the sheet opens, as the create options are read: nothing is
cached. The list is craze's, newest first, at most 50 ("Showing the 50 most
recent." when craze left some out). Each row shows:

- the session's title, or "Untitled";
- its provider;
- its workspace, with the home directory shortened to `~` where the app knows
  it: this machine's own home, and a shed's `/home/shed`. A machine reached
  over SSH shows the full path, because the app never learns its home;
- how long ago it was last active;
- a **Resume** button.

Resuming runs one reopen at a time. While it runs, its row reads "Resuming…
(this can take a few minutes)" and every other button, the New | Resume
control included, is disabled. It can take minutes: a provider's load replays
the session, and the app allows craze 200 s per answer and sends a lost answer
again once. Closing the sheet stops waiting, never the reopen; its session
appears as a row, and the sheet opened again shows the reopen still running.

That state is bound to the machine's **incarnation**, not just its name. A
machine removed and added again under the same name (a shed restarted out
from under the app, say) is a new one. Every machine status names its
incarnation (`craze.source_gen`), and the app resets the state as soon as a
status names a newer one, whether or not the sheet is open. It does not wait
for the sheet's create options, which can fail to load. So the new
incarnation's sheet opens fresh, never locked by a reopen the old one left
running, and that reopen's late answer is dropped without opening anything.

When the create options fail to load, the sheet says so with a **Retry** in
whichever half shows, Resume included. The Retry works while a reopen runs.

| Outcome | What the sheet does |
|---|---|
| Resumed | The sheet closes and the session's transcript opens at once. The row is listed before any roster frame names it, the same way a created session's is. |
| Resumed, but the machine was removed (or removed and added again) meanwhile | Nothing opens: the answer came through the machine's old connection, which the transcript cannot open through. The row stays and says so ("Resume again to open it"); resuming again joins the running session. A sheet not on screen says it in a toast. If a status has already named the new incarnation, the state has started afresh and the answer is dropped instead, as described above. |
| `gone` | The session cannot run here any more (no such session, or its directory is gone). The row is dropped, and the sheet says why where it was. |
| `start_failed` | The session was found and failed to start. craze's cause shows inline, verbatim, and the row stays. |
| `unavailable` | Transient: craze is busy or closing, or the answer was lost twice ("Retry is safe": a reopen of a running session joins it). The reason shows inline, and the row's button reads Retry. |
| Anything else | The error shows inline, and the row stays. |

### Open in terminal

Open in terminal opens a roost tab on the session's host running
`craze attach --session <hostId>` through the same ladder and exec PATH, in the
session's workspace. Closing that tab only **detaches**; typing `/exit` in it
**stops** the session — the button's tooltip says so.

`craze attach` claims no tab, so roost never reports the tab as craze-owned.
The desktop keeps its own map of the tabs it opened this way and feeds it to
the fold: the tab attaches to the session's row (by provider session when
exactly one row matches, else by hostId), and stays attached while the hub
feed is down, on the retained row. The map is in memory: after a restart such
a tab is an unowned tab, and the row offers Open in terminal again.

An entry leaves when roost stops listing its tab. Each open is **fenced**:
the desktop records roost's daemon incarnation and a revision past the open,
so a roost snapshot taken before the tab existed cannot remove it, while a
later one — or one from a restarted roost — that omits the tab does. An
unfenced open (roost published no revision, or both reads after the open
failed) is kept for 5 seconds whatever a snapshot says, then presence
decides.

### The transcript

The lane opens one connection of its own: the hub's splice to the session's
host (`session.connect` with the hostId), the host's own row (which carries
the craze session id every later call uses), then `session.attach`.

- **Seed.** An attach without a cursor answers with a snapshot, folded into
  rows inside `Reset` … `Ready`. `Ready` waits for craze's `synchronized` for
  that attachment, so a seed is never published half-replayed.
- **Silent resume.** When the connection drops after `Ready`, the lane emits
  `Stale`, redials with a backoff (200 ms → 30 s), and attaches with its
  cursor. If the host honours it, the missed events fold onto the rows
  already on screen and a lone `Ready` clears the banner — no reseed, the same
  generation. If the host refuses it (a new incarnation, a journal gap), the
  lane reseeds. A loss before the seed's `Ready` always reseeds.
- **Bounds.** Each dial must hand back a connection within 30 s, or it counts
  as a failed dial. Dials give up 10 minutes into an outage, an outage allows
  8 attaches, and five consecutive refused `session.connect`s end the lane
  (`unknown_session` ends it at once, for good); past any of these the lane
  ends and the desktop reopens it. An attachment that goes 60 s without a word
  before its `synchronized` is treated as a dead connection and redialled.
- **Verbs** share the lane's connection, each under a 30 s deadline. A verb
  issued while the lane is reconnecting waits up to 10 s, then fails
  `unavailable` — never sent, so a retry is safe. A verb in flight when the
  connection drops, or unanswered by its deadline, fails `outcome_unknown`: it
  may have run, it is never resent, and the transcript shows whether it did.
  The panel keeps a Send's text after `outcome_unknown` and says to check the
  transcript before sending again.
- **Stop** ends the session, not just the transcript, behind an inline
  confirm. craze answers with a receipt; the row leaves, and the lane ends
  with `session_closed` — usually just after the row, see below.
- **End tab and Stop are different on a row hosted in a TUI.** **End tab**
  closes only that roost tab; the session keeps running in craze's hub, and its
  row stays (without the tab). **Stop** is what ends the session.
- **A stopped session can reappear for about a second.** Right after Stop, a
  craze roster update can list the session once more before the hub's next
  update removes it. It is craze's roster behaviour
  ([craze#93](https://github.com/charliek/craze/issues/93)), cosmetic, and
  shed does not hide it, because the roster is the listing authority.

**What the transcript draws.** craze's own fold updates rows in place; shed's
rows are append-only, so the adapter segments: a streaming text or reasoning
run becomes a row at a change of kind, after 2 s without growth, or at 8 KiB
(split on a character boundary, never truncated). A tool draws one row at
first sight and one at its first terminal status. A turn's prompt,
interjections, todo progress, compaction, a foreign turn (the agent continuing
on its own), a cancelled turn, errors and "restored" after a replay draw rows
in craze's own wording. A snapshot whose window was cut opens with one "earlier transcript
omitted" row. Only the main agent draws rows: a sub-agent's events and
transcript are not shown.

### A lane outlives its row

craze 0.2.0 marks a stopping session's host `stopping` before its engine
closes, and the hub drops the row on its next roster round (once a second) —
usually **before** the session's last events and its `session_closed` reach
the lane on its own connection. craze promises that connection runs on to the
session's end, and nothing orders the two connections. So the desktop does
not end a craze lane when its row leaves; the lane **departs**:

- **The grace.** The lane keeps its own connection for **10 seconds**. A
  final `Down` inside it — the normal Stop: the closing rows, then
  `session_closed` — ends the lane exactly as a normal end does, with every
  last row on screen.
- **A return.** A row listed again inside the grace (a reseed that briefly
  dropped it, a hub that came back) cancels the departure: the lane never let
  its connection go. A row that leaves again later gets a fresh grace; a
  repeated removal during one absence never extends it. Departures and
  returns apply in the order they happened, however late one reaches the
  lane: a return reported before a departure never revives the lane that
  departure departed, and a departure from before a return never departs the
  lane again. **End tab** departs no lane: the session runs on in its host
  after its tab closes, and a session that does end leaves the roster on the
  hub's next round.
- **The end.** Otherwise the grace's end ends the lane itself, with a final
  reason the banner shows (`ended · <reason> — showing the last known
  transcript`): **`session_gone`** when the roster removed the row (a
  `Removed`, or a reseed without it), or
  **`hub_gone`** when the row left with the hub (the source went dormant).
  10 s only bounds the worst case: a host that died without a final event
  answers the lane's redial with `unknown_session` well before it.
- **Host removed.** A machine removed from the config, or a shed that
  stopped, ends every lane of its craze source at once with
  **`host_removed`** — no grace: the source that could list the row again is
  gone.

An ended lane releases its connection (and on a remote host its ssh exec),
whatever ended it, but stays readable until its panel closes. `lane.open` on it answers the ended
lane while its row stays gone, and opens a fresh lane once the row is listed
again (the old one is replaced only when the new open succeeds). Closing the
panel removes it. The app's orderly shutdown counts these finished lanes too
(its lanes phase line reads `lanes=<n> (finished=<n>) … graces=<n>`, the
grace timers joined alongside the subscriptions).

### The `/` menu

A craze 0.1.1 session publishes its **slash catalog** — every `/` command it
offers, and how each runs — and the transcript's composer turns it into a menu
(plan 029). Which rows show, what a typed token matches, what a key or a click
does and when a command is refused are one implementation in the shared core
(`shed_app::slash_menu`), so the desktop and the phone agree by construction;
the panel only renders and handles keys.

- **Typing `/`** as the first character of a one-line draft opens the menu
  above the composer while the caret is in the first token; it lists the rows
  this client can run, matched as craze's TUI does (name prefix, then the
  plugin spelling, then substrings). It scrolls; a catalog has no row cap.
- **Keys.** Enter accepts the highlighted row — it runs when the token already
  spells the row, and otherwise selects it; Tab completes; ↑/↓ move the
  highlight, which starts over whenever the typed filter or the catalog
  changes — on the first row, or, while a turn runs, on the first row that is
  not dimmed (a turn starting or ending alone leaves it where it is); Esc
  closes the menu until the token changes (a catalog update does not reopen
  it). With no menu up, Enter sends. Shift+Enter is a newline, and no key
  acts while an input method is composing.
- **What a row does.** A prompt row (the agent's own commands) is sent as
  `/<name> <argument>` and queues during a turn like any prompt. `/rename
  <title>` renames the session (`lane.set_title`); the header changes when
  craze's update arrives. A mode row (`/plan`, `/ask`, `/agent`) changes the
  mode at once, even to the mode it is in. `/exit` opens Stop's confirm.
  `/model` opens the settings sheet's model list (no inline model yet).
- **`/effort [<level>]`** (craze 0.2.0) is listed only while the current
  model has an effort setting — a select whose id or name says effort or
  reasoning (glm's `reasoning` counts), the same option the chip shows. A
  model switch alone moves it: on a model with no effort setting the row
  leaves the menu, and typing it says "the current model has no effort
  setting". Bare, `/effort` opens the settings sheet (its effort row). With
  a level it sets the effort at once, bound to the model you saw (a model
  that changed meanwhile is refused like a stale sheet change). The level
  matches by id exactly, then by id in any case, then by name; one that
  names nothing — or more than one — is refused with craze's own words,
  `no effort <typed>: one of <levels>`, and the draft is kept. `default` is
  the model's default level, known only from a native session's effort
  facts; elsewhere it is refused the same way. A level is sent even when it
  is the current one (craze's rule: a dropped no-op could lose a concurrent
  change) — unlike a press on the sheet's current value, which sends
  nothing.
- **While a turn runs**, a row that cannot run until it ends (a mode,
  `/model`, `/effort`) is shown dimmed, "after this turn", and choosing it
  says so — rather than hidden, because the limit is temporary. A
  `/effort <level>` is checked first: a level that names nothing is refused
  for that, a valid one for the turn.
- **Refused here, never sent.** A builtin this client does not run yet
  (`/help`, `/clear`, `/btw` and craze's other local rows) is refused with a
  reason when typed and sent — not passed to the agent as text. So is an
  argument to a row that takes none, and `/rename` with no title.
- **The draft.** It clears when a command succeeds and when `/model` or a
  bare `/effort` opens the sheet; it stays — with craze's words, or the
  reason — when a command is
  refused, when its answer is lost ("may have applied — check the session"),
  and when Stop's confirm is answered Keep. A rename craze made but could not
  record shows as a warning, the draft cleared. A command only ever clears the
  draft it was sent from: text typed while it is pending stays when it
  answers, and an Enter or Send on a draft that changed before the command
  went out does nothing.
- A session without a catalog — an older craze, the fake host, every opencode
  lane — has no menu, and a `/` draft is sent as text.

### Approvals

A craze session raises three kinds of ask: **permissions**, **questions** and
**plans**. Each is a card in the panel; the main agent's asks are also a pair
of rows in the transcript (when one opens and when it is resolved).

- **Membership comes from craze's ask registry.** At every seed and every
  silent resume the lane reads the session's registry (`asks.list`, then
  `asks.get` for each open ask), then `session.sync`, and publishes `Ready` only
  once every event through that sync is folded. So the cards are the same live,
  after a reseed and after a resume, a sub-agent's ask included.
- **A sub-agent's ask is answerable but draws no transcript row** — the same as
  craze's own transcript. Its card's detail comes from the ask itself; craze
  does not say which sub-agent raised it.
- A question is a card only when the session exposes question cards, a plan
  only when it exposes plan cards; a permission always is. An automatic
  question or plan is never an approval.

**Answering.** A permission card offers craze's options under craze's labels,
and the panel answers with the exact option id pressed. A question card
carries every question of the ask, answered together; craze questions take no
free text (`custom: false`), so a lone single-choice question answers with one
click. A plan card offers "Accept plan" and "Reject plan". Rejecting a
permission cancels it, rejecting a question skips it. An answer craze refuses
as malformed leaves the ask open (`bad_request`).

### Settings

On a session whose capabilities say `settings`, the transcript header carries
a chip — `<model name> · <effort value> · fast`, from the current values, each
part only when the session has it ("fast" only when it is on) — and the chip
(or `/model`, or a bare `/effort`) opens one sheet. Which option is the
effort is craze's own rule (a select whose id or name says effort or
reasoning, the id `effort` first), read once by the shared core — the same
option `/effort` sets.

- **Model** — a list, in craze's order: the current model, then the models
  you used recently, then the rest of the catalog. On a native session each
  model other than the current one carries a dim second line, `effort →
  <level>`: the level a switch to it lands on — that model's last effort in
  this session, else the one craze remembers for it, else its default — for
  a native model switch moves the effort with the model. A model that says
  neither shows no line, nor does the current model (its effort row shows
  its effort). Only a native session states these effort facts, so on any
  other session no model shows a line.
- **Options** — the current model's own, `thought_level` first, then
  `model_config`, each in the provider's order. Four values or fewer are a
  segmented control; more are a list.
- **Mode** — when the session has switchable modes.
- **Context meter** — when craze reports both the tokens used and the window.

A change applies immediately, with `session.set`. The row is pending until
craze answers. craze normally sends the new value on the stream (a `meta`
delta) ahead of its answer; when its answer says it could learn no revision
(`rev: 0`), no delta follows, and the adapter applies the confirmed value and
sends `Settings` itself. Either way the sheet re-renders from the session's
next `Settings`: a model change redraws the options, and a change made from an
attached craze TUI appears live.

- **An option is bound to the model you saw.** The change carries the model
  the sheet displayed when you pressed it; if the session has moved to another
  model, craze refuses it, and the row says "the model changed; try again".
- **A refusal shows inline on its row.** Pressing again sends a new command.
- **A lost answer is never resent.** If a `Settings` frame has already put the
  requested value on screen, the row just shows it. Otherwise it says "not
  confirmed" until the session's next `Settings` states the real value; a
  silent resume always restates the settings, so a reconnect clears it.

- **A cursor model change is the machine's default.** Changing a cursor
  session's model from the sheet changes cursor's own global default on that
  machine (cursor keeps it in `~/.cursor/cli-config.json`), so sessions you
  start there later begin on it too, not only this one.

A session with nothing to set has no chip and no sheet.

### The macOS cursor limit

On a Mac, cursor needs the login keychain, which only the GUI login session
can reach, and every session a craze hub starts runs in that hub's login
session. A hub first started over SSH cannot start a cursor session (craze
SF-126).

- **The desktop on the Mac itself joins whatever hub is running there.** When
  none is running yet, the hub it starts is in the GUI session and cursor
  works. An SSH-born hub that is already running stays SSH-born — the desktop
  joins it, and while it holds it the hub does not idle out — so cursor stays
  unavailable until that hub is stopped (the remedy below).
- **A remote Mac is attach-only,** so configuring one starts nothing. Opening
  the create sheet for it from another machine starts an SSH-born hub there if
  none runs, and cursor is then dimmed with craze's own reason and fix, which
  names the hub's pid. While the desktop's roster connection holds that hub,
  cursor stays unavailable on that Mac — for `craze new` there as well.
- **The remedy is the one craze's fix names:** `kill <hub pid>` ends only the
  SSH-born hub (every session runs on), then `craze ps` in a terminal on the
  Mac starts one in the GUI session.

A GUI-session hub reachable over SSH (craze SF-130) is an open craze
follow-up. The find-only `bridge --hub --no-start` (SF-152) shipped in craze
0.2.0, and every background remote connection runs it.

## opencode

`shed-opencode` talks to **the same local HTTP server opencode's own TUI is
already running** — never a sidecar, never a second process it launches
itself. That server lists sessions, replays and streams a session's
transcript, accepts prompts, and answers permission/question approvals over
opencode's own HTTP API (the legacy `GET /event` stream; opencode's newer
`v2` event stream was still missing `session.idle` at the version this
adapter was built against, so the crate stays on the proven feed and
documents why in its module comments).

opencode has no cursor, so every reconnect — the first subscribe, or a
recovery after the stream drops — is a reseed, bracketed by `Reset` …
`Ready`. opencode never emits `Stale`.

`shed_opencode::OpencodeSource` is its machine-level source: it lists the
server's sessions (a 5 s poll, diffed into upserts and removals), offers its
one provider, creates (the first prompt optional), and opens the
session-scoped lane the app talks to. The desktop uses only the lane: an
opencode row is roost's, and a new opencode session is a roost tab.

### Answering: scoped, and not free

opencode has no by-id GET for a permission or a question — the only lookup it
offers is two directory-wide lists, and an instance is per **directory**, so
those lists carry every root session's approvals, not just the one the panel
is looking at. `answer` resolves the addressed approval inside **its own
session's scope** first — the root session plus its transitive descendants,
the same scope `approvals` already computes — before touching either list, so
one panel can never answer a sibling session's request. An id outside that
scope is `unknown_approval`; an id open in both lists at once is refused as
ambiguous rather than routed by a guess. A failed list read propagates rather
than reading as "not found," because a half-read directory cannot say an id
is absent. Answering an approval whose session has since been deleted is
`unknown_session` rather than reaching the wire.

This costs **four GETs per answer** on a childless session (the root session,
its `/children` read, and the two lists), where an agent with a by-id route
pays one. What it buys is that the answer is translated against the options
the agent actually offered, on an approval this session owns.

### The roost `server_url` handshake

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

`agent_lane`'s **presence is the entire capability signal** for an opencode
row. A card with it gets a Transcript affordance; a card without it does not,
and every `lane.*` op on that session answers the `no_lane` error. A tab
reports no `server_url` in these cases:

- **The session isn't opencode**, or is opencode but the plugin isn't
  installed — an ordinary status-only row.
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

### The password / status-only rule

There is **no credential source in this cut**. If opencode's server has
`OPENCODE_SERVER_PASSWORD` set, every request the adapter makes without
credentials answers `401`, which the adapter maps to `LaneError::Unauthorized`
and the panel renders as an inline error — the transcript never loads. This
is deliberate and documented, not a bug: a password-protected opencode
session is **status-only** in this build. The client type
(`shed_opencode::BasicAuth`) already exists for the follow-up that adds a
config field to supply one; nothing wires it up yet, and no test claims
otherwise.

## Answers

### An ambiguous decision is refused, never guessed

`LaneApprovalOption.kind` is an option's *semantic* kind (`allow_once` /
`allow_always` / `reject_once` / `reject_always`); `id` is opaque and
independent of it, and an agent can offer several options of the same kind. A
live gx leader (an earlier adapter, since retired) proved it: asked to
run `id -un`, it offered five options, two of them `allow_once` — "Yes,
proceed" and "Yes, and don't ask again for anything (always-approve mode)". So
a bare decision (`AllowOnce` / `AllowAlways` / `Reject`) is not always enough
to pick one. When it is ambiguous, `LaneApproval::option_for` **refuses to
guess** — it returns `None`, and the adapter answers `BadRequest` — rather
than resolving the tie by offered order, which on that five-option set would
have silently selected the option that turns off every future permission
prompt. craze passes its permission options through verbatim, so the same
rule applies to every craze provider.

The panel's answer is capability-driven, not a fixed three-decision form: it
renders every option an approval offers, under the agent's own label, in the
agent's own order, and posts back `LaneAnswer::Choice { option_id }` — the
exact id the human pressed — for every adapter. Clients send
`{choice: "<id>"}` over IPC. The scripted `{permission: "allow-once"}` form
still works when the kind it names is unambiguous on the approval being
answered.

### Free-text answers

A question can accept typed prose beside its options. The contract carries it
as its own **positional** field on the answer — `custom_text`, one entry per
question, `null` where nothing was typed — never appended to the list of chosen
option ids, because an adapter that cannot tell a chosen label from something a
human wrote cannot map it onto the agent's own shape.

Over IPC it rides beside `question` and nowhere else:

```json
{"question": [["release"], []], "custom_text": [null, "ship it on Friday"]}
```

`custom_text` beside `choice`, `permission` or `reject` is a `bad_request` — a
client that typed something and named the wrong form is told the text did not
travel. The text is trimmed once, in `shed_core::lane::normalize_question_answer`
(the one reader both adapters share), and text aimed at a question whose `custom`
is `false` is refused **before** anything reaches the wire.

| Adapter | How a typed answer reaches the agent |
|---|---|
| **craze** | It does not: craze questions take no free text, so every craze question says `custom: false`, the panel offers no text field, and text sent anyway is refused. |
| **opencode** | `QuestionReply.answers` is "an array of selected labels", and a custom answer is a label the ask did not offer — so the text is appended as one more entry on that question's list, which is exactly what opencode's own TUI posts. A question answered with neither stays `[]` (opencode reads that as unanswered). |

On opencode the flag's documented default is `true` ("Allow typing a custom
answer (default: true)"), so an ask that omits it — the ordinary wire shape —
accepts free text; only an explicit `custom: false` does not. A question that
accepts free text goes through **Send answer**; a lone single-choice question
that does not answers with one click.

## Driving a lane

The view these ops answer from is `shed_app::lane_view::LaneView` — one per
open subscription, **client-shared Rust, not Tauri-specific**. It folds a
`shed_core::lane` subscription's frames in arrival order behind the same
`Reset … Ready` staging the contract promises, and exposes them through a
typed `LaneView::snapshot(since_seq)` (`LaneViewSnapshot { messages, full,
activity, session, generation, stale, ended, capabilities, settings,
slash_menu, approvals }`, `slash_menu` being the session's slash catalog
resolved against its capabilities and cached until either changes);
`None` returns everything, `Some` a delta honored only when the cursor still
lands inside the live generation's `seq` window. It lives in `shed-app`,
ungated, for the same reason `machine.rs` and `roost.rs` are — mobile links
`shed-app` with default features and needs the identical fold, so "the phone
shows the same view the desktop shows" is a property of one implementation
rather than two that have to agree. The Tauri crate's own `lane.rs` is the
IPC layer: it serialises the snapshot's fields into the
`lane.messages`/`lane.approvals` payload and folds nothing itself.

A row that carries `agent_lane` — a craze row, or an opencode row whose tab
reported its server — opens its Transcript affordance with
`lane.open {machine, kind, session_id}`. Every `lane.*` op takes the stamp's
`kind` as well as its `session_id`: a craze row's `session_id` is its hostId
and an opencode row's is opencode's own id, and the two are separate
namespaces.

| Op | Does |
|---|---|
| `lane.open` | Ensures a subscription (idempotent — a second call for an already-open lane re-answers from the existing entry) and answers the session row as the source listed it at the open. A craze lane whose row has left (departing, or finished) is answered as it is; a finished one whose row is listed again is replaced by a fresh lane once that open succeeds. |
| `lane.messages` | The staged transcript: up to the last 500 rows, current activity, the live session row (`session`, `null` until the first seed), generation, a `stale` reason when the lane is not live, `ended` once its subscription is over, `finished` once the lane is over for good (every verb then answers `lane_ended`), and the session's `capabilities` and `settings` — the only place the panel reads what the session can do. The panel's header reads the live row too: `lane.open`'s is re-answered unchanged while the lane stays open, and for a session opened the moment its create answered it is the create's own, with no permission mode. |
| `lane.approvals` | Pending approvals, oldest first (`created_at`, then `id`): for opencode the root session's and its children's, for craze every open ask in the session's registry, a sub-agent's included. |
| `lane.send` | Queues a prompt (`mode: queue`), or preempts the turn in flight (`mode: interject`) when the lane advertises `interject` — the panel shows the toggle only then, and only enables it while the turn is `Working`. |
| `lane.cancel` | Aborts the turn in flight. The panel offers Cancel only when the session's streamed `capabilities.cancel` is true, and enables it only while the session is `Working`. |
| `lane.answer` | Answers one approval. Four forms, exactly one per answer and nothing beside it (any other key — say an `option_id` next to `permission` — is `bad_request`, so an answer can never execute as something other than what it reads as): `{choice: "<id>"}` — the exact option id the approval offered, which is what the panel always sends (see [the ambiguity refusal](#an-ambiguous-decision-is-refused-never-guessed)); the scripted `{permission: "allow-once" \| "allow-always" \| "reject"}`, which resolves by semantic kind and refuses an ambiguous one; `{question: [[…]]}`, optionally with `custom_text` beside it (see [Free-text answers](#free-text-answers)); and `{reject: true}`. |
| `lane.stop` | Ends the SESSION (craze's `session.stop`), not just the transcript: answered on craze's receipt, after which the lane ends and the row leaves. The panel offers Stop only when the session's `capabilities.stop` is true, behind an inline confirm. |
| `lane.settings` | The session's settings, read now — the model and models in craze's order, the mode and modes, the current model's own options and the usage. The panel renders the stream's copy, `lane.messages`' `settings`. A finished lane answers the last settings it stated, with no call to the agent. |
| `lane.set` | Changes one setting: `{kind: "model"\|"mode", id}` or `{kind: "config", id, value, for_model?}` (craze's `session.set`; `for_model` is the model the client displayed, else the lane's folded one). The new value arrives on the stream; craze's `stale_model` is `not_accepting`, and an answer lost to a drop is `outcome_unknown` — never resent. |
| `lane.set_title` | Renames the SESSION (craze's `session.setTitle`; the composer's `/rename`): the title trimmed, a blank one refused unsent. The new title arrives on the stream; `{warning}` is a rename craze made but could not record. Refused by a session whose `capabilities.set_title` is false. |
| `lane.close` | Ends the subscription and removes the lane, whether it is live, departing or finished; the last close on a shared SSH forward tears it down. |

A failure comes back as `{code, message}` with the contract's own snake_case
codes (`unauthorized`, `unknown_session`, `unknown_approval`,
`already_submitted`, `already_resolved`, `not_accepting`, `unavailable`,
`failed`), plus `no_lane` for a row with no `agent_lane` stamp at all,
`unsupported_lane` for a row whose `agent_lane.kind` names no adapter this
build has, `outcome_unknown` for a craze verb whose answer was lost with its
connection (it may have run, and it is never resent), and `lane_ended` for a
verb (`lane.send`, `lane.cancel`, `lane.answer`, `lane.set`, `lane.set_title`,
`lane.stop`) on a finished lane — refused at once, never sent.

The craze ops — `craze.create_options`, `craze.create`, `craze.saved`,
`craze.reopen`, `craze.open_terminal` — the UI-truth dumps (`lane.dump`, `lane_settings.dump`, `craze_create.dump`)
and the test-mode doors are listed in [IPC](ipc.md#craze-sessions-tauri).

## A stalled client and the channel bound

The channel each subscription streams over is bounded — `LANE_CHANNEL_CAPACITY`
(1024 frames) — so a client that stops draining (a backgrounded phone
mid-session is the canonical case) cannot grow a watcher's backlog without
limit. `shed_core::lane::Publisher<T>` — a lane's `LanePublisher`, a source's
`SourcePublisher` — is the one place the overflow policy lives, shared by
every adapter at both levels: `publish` is a `try_send`, and a full channel
answers `Publish::Lagged` rather than blocking or dropping the frame
unnoticed. Every emitting helper in both adapters propagates that, so a
generation ends at the **first** dropped frame — mid-stream, mid-seed, or
mid-reseed alike — and is retried rather than left half-staged. A source's
seed must fit the channel too: it carries at most 512 rows and says
`truncated` when it dropped any.

A lagged generation is never resumed silently, even on craze, which can: the
adapter drops its connection first, waits for the channel to drain completely
with no transport held, takes the ordinary failure backoff, and reseeds with a
fresh `Reset { reason: "lagged" } … Ready` — because the dropped frames may
already have been folded at or before the client's cursor, and resuming from
it would leave the hole permanent. The one frame that is never dropped is the
terminal `Down`: it goes through `Publisher::publish_final`, which awaits room
rather than trying and giving up, and the rows flushed just before it wait the
same way, so a client can never be left holding a stale `Ready` view with no
`stale` reason to explain it.

`lagged` and `overflow` name different things and neither substitutes for the
other: `overflow` is the adapter's own inbox falling behind its source before
folding (opencode's live frames buffered while its REST seed runs); `lagged`
is this client channel falling behind the fold. craze has one more bound of
its own: a connection's notification queue is capped at the same 1024, and a
full queue ends that connection rather than stall the replies behind it — the
lane reseeds.

## Limits

**craze:**

- **No model, effort or permission mode at create.** A session starts on its
  provider's defaults; change the model and options from the settings sheet
  afterwards.
- **Saved sessions: the 50 most recent, no filter.** The sheet's Resume half
  lists the 50 most recent saved sessions, with no workspace filter and no
  sub-agent sessions; a reopen in flight cannot be cancelled (closing the
  sheet stops waiting, not the reopen). Resume needs craze 0.2.0 or later.
- **No sub-agent transcripts.** A sub-agent's asks are answerable; its own
  messages are not shown.
- **No queue editing,** no send-now.
- **The `/` menu is phase 1.** craze's `local` rows (`/help`, `/clear`, …),
  side questions (`/btw`) and an inline `/model <id>` are not offered yet;
  typed, the builtins are refused.
- **No resend.** A verb lost with its connection is reported `outcome_unknown`
  and never resent, even where craze could resume the command.
- **A craze TUI run with its control socket off** has no host in the roster,
  so while the hub feed is live its roost tab is folded away and nothing shows
  it.
- **One craze directory.** shed never sets `CRAZE_HOME`, so its hubs use the
  default one. A hub's roster lists sessions from every craze directory under
  the same `HOME`, while a create lands in the hub's own.
- **Too-old detection** for craze 0.0.1 reads its `unknown flag: --hub` error;
  every later craze is judged by what its hub says it can do.
- **The macOS cursor limit** above.

**opencode:**

- **No interject.** Its capabilities' `interject` is `false`. Every send goes
  through opencode's `prompt_async`, which is accepted and ordered after
  whatever the session's runner is already doing — it does not preempt a
  turn in flight. There is no "type over the agent" affordance.
- **No resume-from-cursor.** Its capabilities' `history_cursor` is `false`.
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
- **Password-protected servers** are status-only (above).

## See also

- [The Agents pane](rc-sessions.md) for how a machine row and its sessions are
  discovered in the first place.
- [IPC](ipc.md) for the full op surface: the lane ops, the craze ops, and the
  status rows a machine's craze state rides on.
- [craze](https://github.com/charliek/craze) — its own documentation covers
  the hub, `craze attach`, `[agents]` and `craze auth`.
