# IPC

shed-desktop exposes a control socket so the app can be driven and observed
programmatically — by `shedctl`, by the functional test harness, or by hand. This is a
first-class feature: it is how changes are verified without a human clicking.

## Transport

- A Unix-domain socket at `~/Library/Caches/ShedDesktop/shed-desktop.sock` (mode `0600`).
- Newline-delimited JSON, one object per line, 16 MiB frame cap.
- Request: `{"id": "<int64-as-string>", "op": "...", "params": {...}}`
- Response: `{"id": "...", "ok": true, "result": {...}}` or
  `{"id": "...", "ok": false, "error": {"code": "...", "message": "..."}}`

Request structs reject unknown fields. Errors use stable codes: `unknown-op`,
`invalid-param`, `unknown-field`, `not-found`, `internal`, `not-enabled`.

## Core ops

| op | params | result |
|----|--------|--------|
| `identify` | — | `socket_path`, `pid`, `app_label`, `app_id`, `ui_version`, `protocol_version`, `test_mode`, `mock_base_url?` |
| `ui.state` | — | `pane`, `hosts[]`, `sheds[]`, `host_agent_connected`, `last_error?`, `sheds_empty_state` |
| `ui.navigate` | `pane` (sheds\|machines\|approvals\|agents\|activity\|egress\|system) | `pane` |
| `ui.set_ssh_approval` | `method?`, `scope?`, `ttl?` | `{}` (applies SSH approval prefs + resets live SSH grants) |
| `ui.show_window` | — | `{}` |
| `ui.hide_window` | — | `{}` (closes the dashboard → menu-bar-only accessory) |
| `ui.window_state` | — | `visible` (bool), `activation_policy` (regular\|accessory) |
| `ui.open_preferences` | — | `{}` |
| `ui.open_menu` | `open` (bool) | `open` |
| `host.list` | — | `hosts[]` |
| `sheds.list` | `host?` | `sheds[]` (Tauri also returns `host_errors[]`) |
| `sheds.refresh` | — | `{}` (forces an immediate poll); Tauri returns the `sheds.list` payload the UI committed |
| `system.df` | — | `usage[]` (per-host `GET /api/system/df`: totals + image/shed/orphan disk entries) |
| `app.window_metrics` | — | `window_width`, `window_height`, `sidebar_width`, `visible_pane` |
| `app.screenshot` | `surface` (window\|menu), `scale` (1\|2) | `png` (base64), `width`, `height`, `scale`, `surface` |
| `app.quit` | — | `{}`, then the app exits (Tauri, test mode only): the tray's Quit, drivable — the orderly shutdown runs, then the process ends (see [Architecture](architecture.md#shutting-down-tauri)) |
| `app.terminate` | — | `{}`, then the app exits (Tauri on macOS, test mode only): the macOS system quit, drivable — `[NSApplication terminate:]`, the path a Dock Quit, a logout and an AppleScript `quit` take, so the orderly shutdown runs from `RunEvent::Exit` alone and AppKit ends the process with status 0. Off macOS it answers `not_supported` (use `app.quit`) |

The screenshot renders the target window's content view to a PNG in-process — no screen
capture permission, works even when the window is occluded or off-screen. Capturing the
menu requires it to be open first (`ui.open_menu {open:true}`).

**Per-host failures (Tauri).** A host whose sheds can't be listed is reported rather than
dropped: `host_errors[]` carries `{server, kind, summary, detail}` per failed host, where
`kind` is `agent_upgrade_required` or `other`, `summary` is the one-line remedy-first text,
and `detail` is the hover/log body. It is always present — `[]` when every host is healthy.
The Tauri UI-truth op `dashboard.dump` returns `{rows, host_errors, empty}`: the rendered
shed rows, the failures the shell holds, and the rendered empty state (`{title, body}`,
`null` when the list rendered).

The Sheds pane renders no error rows of its own. An unreachable server is a *status*, and
status lives in the sidebar's **SHED SERVERS** section (the row's dot, with the reason on
hover) and the System pane. The pane's only duty when it cannot list is to not claim "No
sheds yet" — its empty state names the unreachable servers and points at the sidebar,
carrying no transport error text.

**Per-host failures (mac).** The same shape rides on the host itself. Each entry in
`hosts[]` carries `name`, `host`, `http_port`, `ssh_port`, `reachable`, `backend?`,
`version?`, `last_error?` and — when the probe failed — a typed `failure`:

| field | meaning |
|-------|---------|
| `server` | the configured server name the failure belongs to |
| `kind` | `agent_upgrade_required` (shed-host-agent is too old to obtain a certificate) or `other` |
| `summary` | the one-line banner text, remedy first (also mirrored into `last_error`) |
| `detail` | the full cause — the sidebar tooltip and the diagnostic log body |

`sheds_empty_state` is the sentence the Sheds pane's empty state renders: a known
`failure.kind` speaks (naming the remedy) instead of the generic "check
~/.shed/config.yaml" advice, which remains for a failure with no recognized cause.

## Lifecycle, create + terminal

| op | params | result |
|----|--------|--------|
| `shed.start` / `shed.stop` / `shed.reset` / `shed.delete` | `host?`, `name` | `{}` (refreshes first) |
| `create.start` | `host?`, `name`, `repo?`, `local_dir?`, `image?`, `backend?`, `cpus?`, `memory_mb?`, `no_provision?` | `create_id` |
| `create.status` | `create_id` | `CreateProgress` (poll until `complete`/`error`) |
| `terminal.preview` | `host?`, `shed`, `session?` | the ssh `TerminalCommand` (spawns nothing) |
| `terminal.open` | `host?`, `shed`, `session?` | launches the terminal (**disabled** in test mode) |

## Agent sessions

Every session here is a **roost tab**. A shed's agent sessions are what its own
`roost-session` reports, exactly as a machine's are — a shed with no session running has
none. (Before 0.9.0 a shed's rows came from an in-guest RC hub, reached by ssh'ing
`shed-ext-rc` into the shed, and a shed's answer was the *union* of that and roost's. The
guest binary and the hub are gone.)

| op | params | result |
|----|--------|--------|
| `rc.list` | `host?`, `shed?` | `{sessions, capabilities, machines, shed_craze}` — `shed_craze` lists the unlisted sheds craze is offered on (below) |
| `rc.launch` | `host?`, `shed`, `kind`, `display_name?`, `workdir?`, `permission_mode?`, `initial_prompt?` | the opened row — an **alias** for `roost.launch` on `roost:<host>/<shed>`, kept for 0.9.x, under the same launch rules (below) |
| `rc.kill` | `host?`, `shed`, `slug` | `{}` — a roost `tab.close`; the slug IS the tab id, and must be one THIS host lists (ids are per-host, so one copied from elsewhere would close an unrelated tab) |
| `machines.list` | — | `machines[]` — every configured machine's health, name-ordered, each with its `craze` source's state (below) |
| `machine.kill` | `machine`, `slug` | `{}` (addressed by machine + slug, not host/shed). For a **craze row**, `slug` is the row's `tab_id` — its End tab closes the roost tab the session runs in; a craze row's own slug is its hostId, not a tab id |
| `rc.inject_test` | `shed`, `slug`, `kind?`, `display_name?`, `workdir?`, `lifecycle?`, `attention?` | `{}` — **test mode only**; puts a row into that shed's roost snapshot. `slug` must parse as a roost tab id, and `kind` must be one roost has an adapter for (anything else would be an unowned tab, which a real snapshot never lists) |
| `roost.probe` | `target` | `target`, `probe` (its `fingerprint` nested inside) and `plan` — a read-only look at a shed or machine's `roost-session` state. The plan matrix row comes back from here too, so a caller that needs only the row does not also have to call `roost.preview` |
| `roost.preview` | `target` | the plan (Install/Update/Start/Report/nothing to do) plus the sentence naming where the bytes would come from |
| `roost.bootstrap` | `target`, `fingerprint`, `consent: true` | installs/updates/starts `roost-session` on that target and wires its agent hooks; refuses without consent or against a stale fingerprint |
| `roost.launch` | `target`, `kind`, `display_name?`, `workdir?`, `permission_mode?`, `initial_prompt?` | the opened row, plus `title` when a name was given (below). Generalizes `machine.launch` to any roost host (a shed or a machine); `machine.launch` (addressed by `machine` or `target`) stays as an alias |
| `roost.run` | `target` (or `machine`), `command`, `workdir?` | `{origin, machine, slug, cwd, argv}` — the tab it opened. **Run a command in a tab**: `command` split on ASCII whitespace into the argv, with **no shell and no quoting**; gated by no kind. `command` is required — absent, empty or whitespace-only is `bad_request` |

`roost.probe`, `roost.preview`, `roost.bootstrap` and `roost.launch` are how the desktop
drives [putting `roost-session` on a shed or
machine](../extensions/roost-session-hosts.md) — the source ladder, the plan matrix, the
consent card, and the rollback promise are documented there, not here. Kicking off an agent
from roost's own command palette instead of the desktop app is [the `shed` roost
provider](../extensions/roost-provider.md).

**The launch rules** (`rc.launch`, `roost.launch`, `machine.launch`, and the app's own launch
dialog). roost's `tab.open` is the agent binary plus a working directory, so:

- **`display_name` is the tab's title.** Once the tab is open, a non-blank name (trimmed) is
  set with roost's `tab.set_title`, which sticks: the agent's own terminal title no longer
  replaces it. The answered row gains `title`: `"applied"`, or `"failed: <cause>"` when roost
  refused it. The tab is open either way, so a failed title is still a successful launch (and
  is logged). Without a name there is no `title` key and no `tab.set_title`.
- **`initial_prompt` and `permission_mode` are refused** with `bad_request`, before any tab
  opens and before the host is contacted: "initial_prompt is not supported by a roost launch;
  type it into the tab, or start a craze session, whose create takes a first prompt", and
  "permission_mode is not supported by a roost launch; set the mode in the agent once its tab
  is open". A launch has no channel to deliver either, and accepting one to drop it would say
  the launch did what it did not. A craze session's create takes a first prompt
  ([`craze.create`](#craze-sessions-tauri)).
- **Blank is absent** for every field: an empty or whitespace-only `display_name`,
  `workdir`, `initial_prompt` or `permission_mode` is treated as not sent.
- **Error codes:** `bad_request` for a request a launch refuses, and `action_failed` for a
  launch that was tried and opened no tab. `bad_request` covers the two fields above (in any
  JSON shape: a non-string one is refused too, never read as absent), a missing or unknown
  `kind`, and an unresolvable `host`. `action_failed` covers an unknown or unreachable host, a
  kind roost has no recipe for, a refused `tab.open`, and the app quitting. The
  `rc_launch`/`machine_launch` commands the UI calls answer the same two codes, as
  `"<code>: <message>"`.

The app's own launch dialog offers a **Session name** but no prompt or permission-mode
field, because neither could be delivered.

**`roost.run` — run a command in a tab.** The launch dialog's second mode, beside the agent
picker, and how an agent's own TUI is started now that the per-agent kinds are gone: a roost
`tab.open` of the command line as typed, on a shed or a machine. It is **not gated by kinds**
— there is no `kind`, nothing is checked against the host's capabilities, and the first word
can name any program. The line is split on ASCII whitespace into the argv and handed to roost
verbatim; **there is no shell and no quoting**, so `codex --model x` runs `codex` with two
arguments, while `say 'a b'` passes `'a` and `b'` (quotes and all) and `$HOME` or `|` is
literal text. A pipeline belongs in a shell tab, which roost's own UI opens. `command` is
required: absent, empty or whitespace-only is `bad_request`, answered before the host is
touched — a blank command is not a plain shell. The answer is the **tab**, not a row: a tab
nobody owns is not a session, so the row that results is whatever roost reports — a plain row
once roost's own agent hooks claim the tab (a `codex`, say), and none for a tab no hook claims.

`capabilities` is keyed by a row's **`origin`** — `machine:<name>` or
`roost:<server>/<shed>` — so a card can read the contract behind the row it is drawing. It is
roost's synthesized contract, not a probe: there is nothing to ask, and it answers for a host
that is currently asleep.

### Machines (Tauri)

A **machine** is a native host you reach over SSH that runs a `roost-session` — no shed
server in the path, no TLS pin, no control token. Machines come
from the `machines:` section of `~/.shed/config.yaml` and are
read ONCE at startup, so there is no in-app add/edit; an editor that silently needed a
relaunch would be worse than the file.

Machine sessions appear in `rc.list` beside shed sessions, each stamped with
`origin_kind: "machine"` and `origin: "machine:<name>"`. **Row keys and grouping must use
`origin`, never `shed`** — a machine row carries an EMPTY shed, so two machines sharing a
slug would collide into one row. `machines.list` is separate from
`rc.list` because a machine is worth showing even when it has no sessions and cannot be
reached: that row IS the information ("mini3 is asleep"), and a sessions-only payload has
nowhere to put it. Each entry carries `{name, origin, reachable, connected_once, roost_watched,
sessions, detail?}`, where `detail` is why it is unreachable — verbatim, because "no route to host"
and "nothing is listening" need different fixes — and `roost_watched` says a roost watcher runs
for the host: a configured machine's and the implicit `localhost`'s from the start, a shed's once
its roost answered `session.identify`, which is before its first inventory sets
`connected_once`.

Unreachable is a first-class state, not an error: a machine that is asleep, off-network, or
simply not running a `roost-session` is the everyday case, and it must never fail the
sessions view.

### Sessions from roost

Plan 013 (the Roost Pivot) re-points the section above: a machine row's sessions come
from that machine's `roost-session` daemon. Plan 014 replaced the
2 s poll with roost's **observer event stream**: the watcher holds one subscription per cycle
and pushes a snapshot only when a row actually changes, so there is no polling cadence left
(`SHED_ROOST_POLL_MS` no longer exists). At session protocol 5 (plan 020) there is no
classification left to reclassify — subscribing takes no lease and never did, and every
subscriber now sees every event the same way, so watching a machine's tabs never contests
anything and there is no `session.driver_changed` event left to receive. A configured machine
reaches it over roost's SSH
bridge; an implicit **`localhost`** host is also listed once a local `roost-session`
socket has existed in this process (release path
`$XDG_RUNTIME_DIR/roost-session/roost.sock`, macOS
`~/Library/Caches/RoostSession/roost.sock`, override with `SHED_ROOST_SOCKET`) — a machine
with no `roost-session` lists nothing until one is running.

Only **agent-owned tabs** are sessions; a plain shell tab in roost is not one. A sticky
**attention dot** on the card mirrors roost's own notification bit (shed never clears it
itself). There is **no terminal action** on a roost row yet — roost's `vt` attach hasn't
landed (`kind_features.attach = "native-remote"` gates it off) — so `kill` maps to
`tab.close` and `launch` to `tab.open` with the chosen agent binary in the chosen working
directory, named by the launch's `display_name`; a first prompt or a permission mode is refused
(see the launch rules above).

A **shed**, not just a `machines:` entry, is a roost host the same way: `roost.probe` /
`roost.bootstrap` install and start a `roost-session` on it over the shed's
own SSH — see [putting `roost-session` on a shed or machine](../extensions/roost-session-hosts.md)
for the mechanism, the source ladder, and consent. A shed's rows are then its roost rows
(`source: "roost"`, `origin: "roost:<server>/<shed>"`), and a filtered `rc.list {host, shed}`
returns them. **A shed with no `roost-session` lists no sessions**, which is what the Agents
pane's empty state says — and it offers the setup, which happens on the shed's own card.
See the roost project's own docs for the daemon and its IPC contract.

### Craze sessions (Tauri)

Each host — this machine, every configured machine, every running shed — has a **craze
source**: its craze hub's live session list (craze is the provider abstraction for cursor,
grok, gx and native sessions). Its rows ride `rc.list` beside roost's, stamped
`source: "craze"`, `kind: "craze"`, with `slug` = the session's **hostId**, the hub's own
facts (`provider`, `model`, `doing`, `head_ask_summary`, `last_reply`, `attached`,
`start_error`, `pending_approvals` — a count here — `permission_mode`,
`provider_session_id`), and `agent_lane: {kind: "craze", session_id: <hostId>}`.

**For a craze session the hub row IS the row.** A roost tab owned by craze (a craze TUI)
folds into the hub row it names — the row gains that tab's `tab_id`, and the tab is not
listed as a roost row — but only while the host's craze feed is **live**; with it dormant,
offline or absent, roost's row stands alone as before. A craze row a source still holds while
it is not live is the last known one: `stale: true`, `approximate: true`. The fold is computed
per host and never crosses machines.

This machine's source is **eager** (one roster connection from launch, joining the hub already
running here or, when none is running yet, starting one in the app's own session). A **remote** machine's or a shed's is **attach-only**: a find-only probe
(`craze providers --hub --json`, which never starts a hub) runs every 30 s while there is no
hub, and the roster attaches only once one is running — the app never starts a hub on another
machine in the background. Every status row (`machines.list`, `rc.list`'s `machines`) carries
the source's state:

| field | meaning |
|-------|---------|
| `craze.state` | `live` (attached), `dormant` (craze there, no hub running), `offline` (it dropped, or could not be asked), or `absent` (never reached — and not installed, which renders as absent) |
| `craze.cause` | an offline (or not-installed) state's class: `unreachable`, `too_old`, `failed`, `not_installed` |
| `craze.create`, `craze.create_options`, `craze.create_title` | the LIVE hub's capabilities; `false` otherwise. `create_title` (craze 0.1.1's `createTitle`) is what the create sheet's "Name (optional)" waits for: a dormant machine's sheet shows the field once the hub it starts is live |

A machine whose craze is too old says so on its Machines-pane card ("craze on this machine is
too old for shed; update it", `machines.dump`'s `craze_note`); a machine without craze says
nothing.

**A running shed is offered craze before it is listed.** A shed has a status row only once
something answers there — its roost-session, or its craze hub (the roster's first `Ready`, or a
session created there). Before that, a running shed whose probe proved craze there is an entry
of `rc.list`'s `shed_craze`, read under the same lock as the status rows, so a host is never
both:

| field | meaning |
|-------|---------|
| `name`, `label`, `origin`, `kind` | the status row's identity fields (`name` is the address, `roost:<server>/<shed>`) |
| `craze` | the same object a status row carries: `state` is `dormant` (offered New craze session on the shed's Sheds-pane row and in "Where"; opening the sheet starts its hub) or `offline` with `cause: too_old` (its row shows the too-old note) |

A shed whose craze is not installed, that the probe cannot reach, or that has not been probed
yet has no entry. The probe stays find-only: nothing dials `bridge --hub` there until the sheet
opens. `shed_craze` honours `rc.list`'s `{host, shed}` filter as the status rows do, and a
change of the entry (dormant ↔ too old ↔ none, the shed stopping or being deleted) pushes the
UI's re-read; a repeated probe that says the same thing does not. A host listed by craze alone
— a listed status row with `roost_watched: false`, `connected_once: false` and a craze source —
reads `craze only` (`machines.dump`'s `status`, and the sidebar's); a shed whose roost answered
but whose first inventory has not arrived is watched, and reads as before.

#### Creating a craze session, and Open in terminal

A host offers **New craze session** when its craze is **live** and its hub can list providers
and create, or when it is **dormant** — opening the sheet is then the explicit action that
starts its hub. A dormant **machine** offers it on its Machines-pane card (and its group in the
Agents pane); a dormant **shed** offers it on its Sheds-pane row (`shed_craze`, above); each is
a "— new craze session" entry in the New-session dialog's "Where". A shed shows in the Machines
pane once its roost answers or it has a hub. A live hub that cannot create shows "update craze
on this machine to create sessions here"; listing still works.

| op | params | result |
|----|--------|--------|
| `craze.create_options` | `machine` | `{machine, options: {providers, default_provider?, recent_dirs}}` — every provider in craze's order with its `state` (`ready` \| `needs_setup` \| `unavailable`) and, when not ready, craze's `reason` and `fix`; the default provider as craze states it; the recent directories, newest first. Read afresh on every call. **On a dormant machine this starts its hub** (an explicit action), and the machine's source attaches to it at once |
| `craze.create` | `machine`, `cwd`, `provider?`, `prompt?`, `title?`, `request_id?`, `held?` | `{session, host_id, ended, prompt, prompt_error?, request_id}` — a new session (provider, directory, an optional name and first prompt only: no model, effort or permission mode). `cwd` must be absolute; a blank `provider` (craze's default) or `prompt` (an idle session) is absent. `title` names the session from its first frame — trimmed, and a blank one is absent, never sent empty; craze normalizes it, and refuses one that is blank once normalized (`bad_request`, `params.title is blank once normalized: send none instead`). A hub without `createTitle` refuses a fresh titled create before anything is sent; a retry or a held replay keeps the title exactly and stays `outcome_unknown`. `session` is its row, **already in the machine's listing**, so `lane.open {kind: "craze", session_id: host_id}` works at once — unless `ended` is `true`: craze replayed an earlier create's answer (it does, for ten minutes, under the same `request_id`) for a session that has since ended, and nothing is listed. `prompt` is `none` \| `accepted` \| `unknown` \| `refused`. `request_id` is the caller's — a present one must be a string in craze's form (1–64 of `[A-Za-z0-9._-]`), else `bad_request`, and is never replaced — or minted when absent (or `null`), and answered back. `held: true` says the call **replays** a `request_id` kept from an unknown outcome (below); it needs the `request_id`, else `bad_request`. Every field is typed: a non-string (a non-bool `held`) is `bad_request`, never read as absent |
| `craze.open_terminal` | `machine`, `session_id` (the row's hostId) | `{origin, machine, session_id, tab_id, cwd, argv}` — a roost tab running `craze attach --session <hostId>` (craze's ladder, the same `sh -c` composition every craze command uses) in the session's workspace. The hub row shows that tab (`tab_id`, End tab and all) from then on, across roost snapshots — and while the machine's craze feed is down, on the retained (stale) row — for as long as roost lists the tab: a snapshot taken after the open — or by a restarted roost — that no longer lists it ends that. Closing the tab only **detaches**; `/exit` inside it **stops** the session. The hostId must be twelve lowercase hex digits (`bad_request`) and a session the machine lists (`unknown_session`) |

**The request id** makes a create idempotent: craze answers a repeat of one with the first
create's answer — its failure too — for ten minutes. So a caller keeps its id **only while the
outcome is unknown** (`outcome_unknown`: craze's answer was lost twice) and retries under it;
after any definite answer — a session, or any refusal — the next create needs a new one.
**A retry under a kept id is sent `held: true`**, and then only craze's own answer about that
request ends the id: a retry that never reaches craze, a craze refusal that does not say
whether the first create started a session (`unavailable` with reason `host_unreachable`, `busy`
or `closing`, and the like), and the app's own preflight refusals (the host no longer
registered, craze not installed or too old) all answer `outcome_unknown` again, so the caller
keeps the id. A session, `bad_request` (an id reused with other params included) and a replayed
start failure end it. A success the app cannot read, or one craze could not send (its `failed`
with reason `failed` or `response_too_large`), is `outcome_unknown` on any create, held or not. craze keeps the guarantee while the first session
runs. Within ten minutes of the first answer a retry gets that answer back, even after the
session has ended; after that, if the session has ended and craze finds no live session for
the request, a retry starts another.
Refusal codes: the lane contract's (`bad_request` — craze's own, an unknown host, a relative
`cwd`, an id not in craze's form; `unavailable`; `failed` — a session that failed to start,
its message craze's own cause, verbatim), plus `outcome_unknown`, `too_old` ("update craze
on this machine"), `not_installed`, `no_craze` (the host has no craze source), and
`unknown_session`/`action_failed` for Open in terminal.

### Agent lanes (Tauri)

A row that carries an `agent_lane` stamp — an opencode tab that reported its server, or a
craze row — opens a live transcript through these ops. `session_id` is the **agent's**
session id from that stamp (a craze row's hostId), not the tab's slug, and **`kind` is
required on every op**: it is the stamp's `kind`, and a craze hostId and an opencode session id
are separate namespaces. The full contract — staging, reconnects, the answer forms, the
failure codes — is [Agent lanes](agent-lanes.md).

| op | params | result |
|----|--------|--------|
| `lane.open` | `machine`, `kind`, `session_id` | `{session}` — the session row alone. Idempotent: a second call re-answers from the open lane |
| `lane.messages` | `machine`, `kind`, `session_id` | `{messages, activity, session, generation, stale, ended, capabilities, settings, slash_menu}` — the staged view, never a half-seeded one; `session` is the live session row (`null` until the first seed); `slash_menu` is the session's `/` menu RESOLVED by the shared core — `{rows: [{name, qualified?, description?, source, arg, arg_hint?, while_working, action}], hidden: [{name, reason}]}`, each row's `action` one of `prompt`, `set_title`, `set_mode` (`{change: {kind: "mode", id}}`), `stop`, `model_picker` — or `null` when the capabilities do not say `slash` or no catalog has arrived (no cap on the rows) |
| `lane.approvals` | `machine`, `kind`, `session_id` | `{approvals}` — pending only, oldest first |
| `lane.send` | `machine`, `kind`, `session_id`, `text`, `mode?` (`queue` \| `interject`) | `{}` |
| `lane.cancel` | `machine`, `kind`, `session_id` | `{}` |
| `lane.answer` | `machine`, `kind`, `session_id`, `approval_id`, `answer` | `{}` |
| `lane.stop` | `machine`, `kind`, `session_id` | `{}` — ends the **session** (craze's `session.stop`), answered on craze's receipt; the lane ends (`ended`) and the row leaves when the session closes. Refused by a session whose capabilities say `stop: false` |
| `lane.settings` | `machine`, `kind`, `session_id` | `{settings}` — the session's settings, read now: `model` and `models` (craze's order: the current model, the remembered ones by rank, the rest in catalog order), `mode` and `modes` (none when its capabilities offer no switchable modes), `options` (the current model's own, the model and mode rows excluded, `thought_level` first, then `model_config`, each in the provider's order; values in the provider's order) and `usage` (`{context_tokens, context_window}`, a native session's). The empty default on a session whose capabilities say `settings: false`. `lane.messages`' `settings` is the stream's copy, which the sheet renders |
| `lane.set` | `machine`, `kind`, `session_id`, `change` | `{}` — change one setting (craze's `session.set`). `change` is `{kind: "model", id}`, `{kind: "mode", id}` or `{kind: "config", id, value, for_model?}` — ids as `settings` offers them, verbatim; anything else (an unknown kind, a field beside the form, an empty id or `for_model`) is `bad_request`. A config change is bound to `for_model`, the model the caller DISPLAYED the option for (the settings sheet always sends it), or — absent — to the model the lane has folded, waiting for the lane's first settings when none has arrived (refused `unavailable`, unsent, if they do not come within the verb deadline); craze refuses it `stale_model` — `not_accepting` — once the session has left that model, and a retry is a new command. `{}` is craze's confirmation; the new value arrives on the stream, ahead of it — or, when craze could learn no revision (`rev: 0`, no event will follow), from the confirmed value, which the lane applies itself. An answer lost to a drop is `outcome_unknown`: the change may have run, it is never resent, and the next `settings` says what the session is at |
| `lane.set_title` | `machine`, `kind`, `session_id`, `title` | `{}` — rename the **session** (craze's `session.setTitle`; the composer's `/rename <title>`). The title is trimmed; a blank one is `bad_request`, never sent. The new title arrives on the stream (`lane.messages`' `session.title`, from craze's delta), not from this answer. `{warning}` is a rename that TOOK and craze could not record (`index_write`): `"renamed, but craze could not record it: <cause>"` — a success, shown as a warning, never rolled back or retried. An answer lost to a drop is `outcome_unknown` (never resent); a session whose capabilities say `set_title: false` (every opencode one) refuses it |
| `lane.close` | `machine`, `kind`, `session_id` | `{}` — idempotent |

Every frame reaches the frontend as the `lane-event` Tauri event, `{machine, kind, session_id,
event}`. A craze lane is evicted when its row leaves its machine's craze source (removed, a
reseed that no longer lists it, the hub gone, the host removed, its tab ended); a roost
snapshot never evicts one.

A craze verb whose answer was lost with its connection — in flight at a drop, or unanswered
within its 30 s deadline — fails `outcome_unknown` (it may have run; it is never resent; a
definite craze failure is never given that code, whatever its words); a verb issued while the
lane is reconnecting waits up to 10 s, then fails `unavailable` (never sent, so a retry is
safe). The panel keeps a Send's text after `outcome_unknown` and says to check the transcript
before sending again.

**What the session can do is read from `lane.messages`, not `lane.open`.** Capabilities are
per session and ride the lane's stream (a craze session's change with its incarnation), so
`capabilities` and `settings` are the live generation's, staged and swapped in with its rows —
each `null` until a seed carrying it has completed, and `settings` stays `null` on a session
with none to show (every opencode lane). `stale` is the banner — the reason a lane is not live
— and `ended` is a different fact: `true` only once the subscription is over, where a `stale`
lane alone may be reconnecting on its own.

**So is the session row, once there is one.** `lane.open`'s `session` is the row as the
source listed it at the open, and a second `lane.open` re-answers that same row for as long as
the lane stays open. `lane.messages`' `session` is the stream's latest — `null` until the first
seed has swapped one in — and it carries what the session states after the open: for a craze
session opened the moment its create answered, the open's row is the create's own, and only the
stream's carries the attach's `permission_mode`. The panel's header (title, directory,
permission line) reads the live row, and `lane.open`'s only until the first seed.

### UI-truth ops (Tauri)

These report what the frontend RENDERED, so a test can assert the window rather than the
backend's view — the two can disagree, and have.

| op | answers | result |
|----|---------|--------|
| `dashboard.dump` | on the Sheds pane | `{rows, host_errors, empty}` |
| `agents.dump` | on the Agents pane | `{sessions, empty}` — `empty` is the rendered empty state (`{state, title, body, action}`), `null` when rows rendered. `state` is `loading` \| `failed` \| `unreachable` \| `empty`: four blanks wearing one screen, and only `empty` offers the bootstrap |
| `launch.dump` | while the New-session dialog is open | `{launch}` — `{rendered, values, create_enabled}`, `null` when none is mounted: the dialog's own rendered text, each labelled control's current value keyed by its label (what was typed is not text content), and whether its Create button is enabled — all three read off the mounted DOM |
| `egress.profiles` | on the Egress pane | `{egress}` |
| `machines.dump` | on the Machines pane | `{machines}` — a row per machine with its `status` word, `detail` line, and grouped session slugs, plus its `craze_note`: the note the card renders about the machine's craze ("craze on this machine is too old for shed; update it", or for a live hub that cannot create "update craze on this machine to create sessions here"), `null` when it says nothing — a machine without craze is not a problem to report — and `craze_create`: whether the card offers New craze session |
| `sidebar.dump` | **always** | `{servers, machines}` — the sidebar's status foot; each machine row is `{name, origin, status, note, tone}`, `tone` its dot: `ok`, `pending` (still connecting), `off`, or `neutral` (listed by craze alone, `craze only`) |
| `shed_roost.dump` | on the Sheds pane | `{sheds}` — each RUNNING shed's row by its `roost:<server>/<shed>` target: its roost line (`status`, `button`, `busy`, `error`, `rendered`), and its craze: `craze_create` (whether the row shows New craze session) and `craze_note` (the too-old note it shows, else `null`) — read off the row |
| `craze_create.dump` | while the craze create sheet is open | `{craze_create}` — what the sheet rendered: `machine`; `state` (`loading` \| `failed` (the options) \| `idle` \| `submitting` \| `refused` \| `unknown` \| `created`); `providers[]` — `{id, label, state, dimmed, selected, reason, fix}`, a non-ready one dimmed and never selectable — except the provider of a kept request id being replayed, which stays `selected` whatever its state (one the re-read no longer lists is a row of its own, `state: "absent"`); `preselected` (the default provider when listed and ready, else the first ready one) and `default_provider`; `recent_dirs`; `values` (the typed "Directory", "Name" and "First prompt"); `title_field` (whether "Name (optional)" is shown: the machine's live hub takes a title at create, or one is typed) and `title_problem` (a refusal about the title, shown under it — craze's `params.title …` `bad_request`); the `request_id` it holds (only while a submission is in flight or its outcome is unknown); its `note` (offline / too old / not installed; else, while a kept request id is replayed, the unknown outcome and "Changing the form starts a new request instead."; else "no provider is ready on this machine"); the `error` as shown (`{code, message, where, text}`), a start failure's `cause` verbatim, the directory's own `cwd_problem`; `create_enabled` and the `primary` button's label (Create, Try again, Creating…) — read off the mounted DOM; `null` when none is mounted |
| `lane.dump` | while the transcript panel is open | `{lane}` — what the panel rendered: its rows, approval cards, `kind` badge (and `lane_kind`, the kind it was opened with), the `permission` line (`bypass` reads "runs tools without asking"), Interject toggle, `can_cancel` and `stop` (`{confirming}`, or `null` with no Stop button) — all read off `lane.messages`' `capabilities`; Cancel, Interject and Stop exist only when the capabilities offer them, and Cancel/Interject are live only while the session is working — `settings_chip` (the header's `<model name> · <effort value> · fast`, from the current values; `null` — no chip — when the capabilities do not say `settings`), `stale`, `ended`, and its error; the composer's `draft`; and `slash_menu` — `{open, token, rows: [{name, dimmed, highlighted}], draft, refusal, warning}`, the `/` menu as rendered: `open` is the menu ON SCREEN (the shared filter's, less an Esc dismissal, which lasts until the token changes), `rows` what it lists in order (`dimmed` while a turn runs and the row would wait for it — "after this turn"; `highlighted` what Enter and Tab accept), `refusal` the line a refused command left (the shared core's reason, craze's words, or "may have applied — check the session"), `warning` a rename that took with a caveat — or `null` on a session with no menu (no `slash` capability, or no catalog yet); `null` when none is mounted |
| `lane_settings.dump` | while the settings sheet is open | `{lane_settings}` — what the sheet rendered: its `machine`, `session_id`, `lane_kind` and `chip`; `rows[]` in render order (the model, the current model's options, the mode) — `{id, kind, name, category, control, current, current_name, values: [{id, name, selected}], state, text, enabled}`, where `control` is `list` (the model, and any row of more than four values) or `segmented`, `state` is `pending` (craze has not answered; the row takes no press), `refused` (`text` the refusal shown inline — "the model changed; try again" on an option whose model moved under it) or `not_confirmed` (the answer was lost; until the session's next settings), else `null`; and `usage` (`{tokens, window, percent, text}`, the context meter, when the session reports both) — `null` when none is open |

**In test mode only**, `ui.report_history {clear?}` answers what every one of the dashboard's
full reports carried, oldest first: `{reports: [{hosts, rows, servers}], dropped}` — the Hosts
badge, the dashboard's row count and the sidebar's server count, per report — from a ring of
the last 256. `dropped` counts the reports the ring evicted since it was last cleared, and
`clear: true` empties it after the read. A sampled `ui.badges` can fall between two reports and
miss one; the history cannot. Outside test mode it answers `not_enabled`.

The transcript panel is mounted by `ui.show_lane {machine, kind, session_id}` (the card's
Transcript affordance, which is a click) and unmounted by `ui.close_lane`.

Its settings sheet is opened by `ui.show_lane_settings {machine, kind, session_id}` (the header's
settings chip, a click; it mounts that session's transcript panel too) and closed by
`ui.close_lane_settings`. It opens only on a session whose capabilities say `settings` — on any
other there is no chip, and no sheet opens (`lane_settings.dump` stays `null`). **In test mode
only**, `ui.pick_lane_setting {row, value}` presses a value on an open sheet the way a person
would — `row` is `model`, `mode` or an option's id, `value` the value's id, both verbatim —
through the row's own gate (a row still pending, a value it does not render, and the value it
already shows take no press); outside test mode it answers `not_enabled`, and the production
door is `lane.set`. Also test mode only, `ui.hold_lane_view {hold: true|false}` holds the open
transcript panel's view: it goes on reading its lane and renders nothing new until released —
the moment between the lane folding a change and the panel showing it, held open (a settings
option pressed then is still bound to the model the sheet displays).

The composer's `/` menu (a session whose `lane.messages` carries a `slash_menu`) is reached only
by typing and pressing keys, so it has its own doors — **test mode only**, `not_enabled`
otherwise, the production doors being the lane verbs. Each acts on the open panel:

| op | does |
|----|------|
| `ui.type_lane_composer {text}` | the composer's draft becomes `text`, the caret at its end, as if typed (`""` clears it). Its listener is the panel's last: re-send it until `lane.dump`'s `draft` shows it, and the doors below are live too |
| `ui.key_lane_composer {key, shift?, composing?}` | a REAL `keydown` dispatched on the composer's textarea — `key` one of `Enter`, `Tab`, `Escape`, `ArrowUp`, `ArrowDown` (anything else is `bad_request`), `shift` its Shift, `composing` an IME composition in progress (`isComposing`). The composer's own handler decides; the browser's default for an Enter nobody took (a newline) is applied by the door, since a synthetic event carries none |
| `ui.pick_slash_row {name}` | a click on the menu row `name` — only a row the menu on screen lists takes it |
| `ui.answer_lane_stop {stop}` | the open Stop confirm's buttons: `true` Stop session, `false` Keep (nothing while no confirm is open) |

What the keys do: with the menu up, Enter accepts the highlighted row — it SENDS when the token
already spells the row, and otherwise selects it (a row that takes an argument completes to
`/<name> `; a mode, `/exit` or `/model` runs at once; while a turn runs, a row that waits for it
is refused instead, "runs after this turn") — Tab completes, Esc closes the menu (and
stops there: an open dialog's Esc handler ignores an Esc already handled) until the token
changes, and ↑/↓ move the highlight. With no menu up — zero matches included — Enter sends.
Shift+Enter is a newline, and every key is ignored while an IME composes. Sending goes through
the same shared dispatch as Enter: a builtin the menu hides (`/help` in phase 1) is refused
inline, never sent as text; a prompt row is sent as `/<its spelling> <argument>`.

The New-session dialog is driven the same way: `ui.show_launch` opens it, and — **in test
mode only** — `ui.fill_launch {mode?, target?, command?, workdir?, name?}` types into it
(`mode` is `agent` or `command`; `target` is a "Where" value, `machine:<name>` or
`shed:<host>/<name>`; `name` is the agent mode's Session name; an absent key is left as it is) and `ui.submit_launch` presses Create, through the button's own
gate. They exist because the "Run a command" mode is reachable only by typing and clicking;
outside test mode both answer `not_enabled`, and the production door onto the same backend is
`roost.run`. A `craze:<machine>` target (a host that offers New craze session — a machine, or
a dormant shed, by its address) leads on to the craze create sheet: its button reads Continue.
`ui.close_launch` closes the dialog (its Escape or Cancel).

The craze create sheet is opened by `ui.show_craze_create {machine}` (the New craze session
action, a click; opening it reads `craze.create_options`, so on a dormant machine it starts the
hub) and closed by `ui.close_craze_create` — closing it while a create runs keeps the create
running, and its session simply appears as a row. **In test mode only**,
`ui.fill_craze_create {provider?, cwd?, title?, prompt?, recent?}` fills it the way a person would
(`provider` clicks that provider's row — a dimmed one refuses the click; `recent: <n>` taps the
n-th recent directory) and `ui.submit_craze_create` presses its primary button (Create, or Try
again) through the button's own gate; outside test mode both answer `not_enabled`, and the
production door is `craze.create`. After a create the sheet closes and opens the session's
transcript at once; a first prompt craze did not take is said in a toast (`toast.dump`).

A pane dump answers `null` off its pane; reporting copy nobody is reading would let a test
assert a surface that isn't on screen. `sidebar.dump` is the exception because the sidebar
is always mounted — which is precisely why it is where an unreachable server's reason
lives now that the Sheds pane carries no error strip. Its `servers[].detail` is the host
failure's `summary` (empty for a healthy host); its `machines[].note` is the clean word a
person reads in the list (`offline`, `connecting`, `N sessions`), never the raw transport
error, which stays on hover.

## Approval ops

These drive the credential-approval gate (see [Credential approvals](approvals.md)).

| op | params | result |
|----|--------|--------|
| `approvals.list` | — | `approvals[]` (each carries `server?`, `namespace`, `op`, `shed`, `detail`, `expires_at`, `gate`, `default_scope`, `default_ttl`) |
| `approval.decide` | `id`, `decision` (approve\|deny), `scope?` (per-request\|per-session\|per-shed), `ttl?` (e.g. `1h`), `persist?` | `{}` |
| `activity.list` | `limit?` (default 200) | `entries[]` (audit feed) |
| `activity.log_path` | — | `path` (the append-only audit log) |
| `policy.list` | — | `rules[]` (effective: default + per-namespace + per-shed) |
| `policy.set` | `rules[]` | `{}` (test mode only) |
| `notifications.list` | — | `notifications[]` (test mode: what the gate posted) |
| `notification.invoke` | `id`, `action` (approve\|deny) | `{}` (test mode: drive a notification action) |
| `notification.open` | — | `{}` (test mode: drive a banner-body tap → opens the Approvals pane) |

`approval.decide` with `persist:true` saves a per-`(server,shed)` rule (always-allow
when `decision:approve`, always-deny when `decision:deny`). For an approve, `scope`
controls the grant: `per-request` (once), or `per-session`/`per-shed` add an in-memory
grant lasting `ttl` (e.g. `1h`). `scope`/`ttl`/`persist` are reported to the host agent
so its durable audit records how the decision was made.

## Test mode

When launched with `SHED_DESKTOP_TEST_MODE=1`, `identify` reports `test_mode: true` and the
`mock_base_url` the app's HTTP clients were redirected to, so the harness can confirm a run
is hermetic before asserting anything. Fault-injection ops (like `policy.set`) are gated
behind this flag.

`SHED_TAURI_CRAZE_PATH` (Tauri, test mode **and** a debug build) names the directory this
machine's craze source finds `craze` in: the local dial then runs craze's ladder JAILED to its
first two rungs, with `PATH` exactly that directory and nothing of the app's environment but
`HOME`, `CRAZE_HOME` and `CRAZE_RUNTIME_DIR`. Unset in test mode, the app has no local craze
source; a remote one exists in test mode only through the fake-`ssh` seam
(`SHED_TAURI_SSH_BIN`), jailed the same way.

Two launch-time overrides exist only in test mode, both taking comma-separated server
names: `SHED_DESKTOP_MOCK_UNREACHABLE_HOSTS` points a host at a closed port (a
deterministic per-host failure), and `SHED_DESKTOP_MOCK_CREDENTIAL_HOSTS` keeps a host's
REAL control-credential wiring — the host agent plus the config's `auth_mode` — against
the mock, so the mint → refusal → banner chain is drivable end to end.
