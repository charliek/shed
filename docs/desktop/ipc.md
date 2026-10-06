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
| `rc.list` | `host?`, `shed?` | `{sessions, capabilities, machines}` |
| `rc.launch` | `host?`, `shed`, `kind?`, `display_name?`, `workdir?`, `initial_prompt?` | the opened row — an **alias** for `roost.launch` on `roost:<host>/<shed>`, kept for 0.9.x |
| `rc.kill` | `host?`, `shed`, `slug` | `{}` — a roost `tab.close`; the slug IS the tab id, and must be one THIS host lists (ids are per-host, so one copied from elsewhere would close an unrelated tab) |
| `machines.list` | — | `machines[]` — every configured machine's health, name-ordered, each with its `craze` source's state (below) |
| `machine.kill` | `machine`, `slug` | `{}` (addressed by machine + slug, not host/shed). For a **craze row**, `slug` is the row's `tab_id` — its End tab closes the roost tab the session runs in; a craze row's own slug is its hostId, not a tab id |
| `rc.inject_test` | `shed`, `slug`, `kind?`, `display_name?`, `workdir?`, `lifecycle?`, `attention?` | `{}` — **test mode only**; puts a row into that shed's roost snapshot. `slug` must parse as a roost tab id, and `kind` must be one roost has an adapter for (anything else would be an unowned tab, which a real snapshot never lists) |
| `roost.probe` | `target` | `target`, `probe` (its `fingerprint` nested inside) and `plan` — a read-only look at a shed or machine's `roost-session` state. The plan matrix row comes back from here too, so a caller that needs only the row does not also have to call `roost.preview` |
| `roost.preview` | `target` | the plan (Install/Update/Start/Report/nothing to do) plus the sentence naming where the bytes would come from |
| `roost.bootstrap` | `target`, `fingerprint`, `consent: true` | installs/updates/starts `roost-session` on that target and wires its agent hooks; refuses without consent or against a stale fingerprint |
| `roost.launch` | `target`, `kind?`, `workdir?`, … | generalizes `machine.launch` to any roost host (a shed or a machine); `machine.launch` stays as an alias |
| `roost.run` | `target` (or `machine`), `command`, `workdir?` | `{origin, machine, slug, cwd, argv}` — the tab it opened. **Run a command in a tab**: `command` split on ASCII whitespace into the argv, with **no shell and no quoting**; gated by no kind. `command` is required — absent, empty or whitespace-only is `bad_request` |

`roost.probe`, `roost.preview`, `roost.bootstrap` and `roost.launch` are how the desktop
drives [putting `roost-session` on a shed or
machine](../extensions/roost-session-hosts.md) — the source ladder, the plan matrix, the
consent card, and the rollback promise are documented there, not here. Kicking off an agent
from roost's own command palette instead of the desktop app is [the `shed` roost
provider](../extensions/roost-provider.md).

`display_name`, `permission_mode` and `initial_prompt` are **accepted and not used** on
either launch door: roost's `tab.open` is the agent binary plus a working directory, roost
owns the tab's title, and typed prompts arrive with [the `shed` roost
provider](../extensions/roost-provider.md). They stay in the wire so a caller written against
the pre-0.9.0 op is not silently refused for sending what the hub accepted — but **the app's
own launch dialog no longer offers a prompt field**, because a box whose contents go nowhere
is worse than no box (`charliek/shed#366` tracks delivering one).

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
nowhere to put it. Each entry carries `{name, origin, reachable, connected_once, sessions,
detail?}`, where `detail` is why it is unreachable — verbatim, because "no route to host"
and "nothing is listening" need different fixes.

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
directory; typed prompts and permission modes arrive later with [the `shed` roost
provider](../extensions/roost-provider.md).

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

This machine's source is **eager** (one roster connection from launch; its hub is born in the
app's own session). A **remote** machine's or a shed's is **attach-only**: a find-only probe
(`craze providers --hub --json`, which never starts a hub) runs every 30 s while there is no
hub, and the roster attaches only once one is running — the app never starts a hub on another
machine in the background. Every status row (`machines.list`, `rc.list`'s `machines`) carries
the source's state:

| field | meaning |
|-------|---------|
| `craze.state` | `live` (attached), `dormant` (craze there, no hub running), `offline` (it dropped, or could not be asked), or `absent` (never reached — and not installed, which renders as absent) |
| `craze.cause` | an offline (or not-installed) state's class: `unreachable`, `too_old`, `failed`, `not_installed` |
| `craze.create`, `craze.create_options` | the LIVE hub's capabilities; `false` otherwise |

A machine whose craze is too old says so on its Machines-pane card ("craze on this machine is
too old for shed; update it", `machines.dump`'s `craze_note`); a machine without craze says
nothing.

#### Creating a craze session, and Open in terminal

A machine offers **New craze session** (its Machines-pane card, its group in the Agents pane,
and a "— new craze session" entry in the New-session dialog's "Where") when its craze is
**live** and its hub can list providers and create, or when it is **dormant** — opening the
sheet is then the explicit action that starts its hub. A live hub that cannot create shows
"update craze on this machine to create sessions here"; listing still works.

| op | params | result |
|----|--------|--------|
| `craze.create_options` | `machine` | `{machine, options: {providers, default_provider?, recent_dirs}}` — every provider in craze's order with its `state` (`ready` \| `needs_setup` \| `unavailable`) and, when not ready, craze's `reason` and `fix`; the default provider as craze states it; the recent directories, newest first. Read afresh on every call. **On a dormant machine this starts its hub** (an explicit action), and the machine's source attaches to it at once |
| `craze.create` | `machine`, `cwd`, `provider?`, `prompt?`, `request_id?` | `{session, host_id, ended, prompt, prompt_error?, request_id}` — a new session (provider, directory and first prompt only: no model, effort or permission mode). `cwd` must be absolute; a blank `provider` (craze's default) or `prompt` (an idle session) is absent. `session` is its row, **already in the machine's listing**, so `lane.open {kind: "craze", session_id: host_id}` works at once — unless `ended` is `true`: craze replayed an earlier create's answer (it does, for ten minutes, under the same `request_id`) for a session that has since ended, and nothing is listed. `prompt` is `none` \| `accepted` \| `unknown` \| `refused`. `request_id` is the caller's — a present one must be a string in craze's form (1–64 of `[A-Za-z0-9._-]`), else `bad_request`, and is never replaced — or minted when absent (or `null`), and answered back. Every field is typed: a non-string is `bad_request`, never read as absent |
| `craze.open_terminal` | `machine`, `session_id` (the row's hostId) | `{origin, machine, session_id, tab_id, cwd, argv}` — a roost tab running `craze attach --session <hostId>` (craze's ladder, the same `sh -c` composition every craze command uses) in the session's workspace. The hub row shows that tab (`tab_id`, End tab and all) from then on, across roost snapshots — and while the machine's craze feed is down, on the retained (stale) row — for as long as roost lists the tab: a snapshot taken after the open — or by a restarted roost — that no longer lists it ends that. Closing the tab only **detaches**; `/exit` inside it **stops** the session. The hostId must be twelve lowercase hex digits (`bad_request`) and a session the machine lists (`unknown_session`) |

**The request id** makes a create idempotent: craze answers a repeat of one with the first
create's answer — its failure too — for ten minutes. So a caller keeps its id **only while the
outcome is unknown** (`outcome_unknown`: craze's answer was lost twice) and retries under it;
after any definite answer — a session, or any refusal — the next create needs a new one.
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
| `lane.messages` | `machine`, `kind`, `session_id` | `{messages, activity, generation, stale, ended, capabilities, settings}` — the staged view, never a half-seeded one |
| `lane.approvals` | `machine`, `kind`, `session_id` | `{approvals}` — pending only, oldest first |
| `lane.send` | `machine`, `kind`, `session_id`, `text`, `mode?` (`queue` \| `interject`) | `{}` |
| `lane.cancel` | `machine`, `kind`, `session_id` | `{}` |
| `lane.answer` | `machine`, `kind`, `session_id`, `approval_id`, `answer` | `{}` |
| `lane.stop` | `machine`, `kind`, `session_id` | `{}` — ends the **session** (craze's `session.stop`), answered on craze's receipt; the lane ends (`ended`) and the row leaves when the session closes. Refused by a session whose capabilities say `stop: false` |
| `lane.close` | `machine`, `kind`, `session_id` | `{}` — idempotent |

Every frame reaches the frontend as the `lane-event` Tauri event, `{machine, kind, session_id,
event}`. A craze lane is evicted when its row leaves its machine's craze source (removed, a
reseed that no longer lists it, the hub gone, the host removed, its tab ended); a roost
snapshot never evicts one.

**What the session can do is read from `lane.messages`, not `lane.open`.** Capabilities are
per session and ride the lane's stream (a craze session's change with its incarnation), so
`capabilities` and `settings` are the live generation's, staged and swapped in with its rows —
each `null` until a seed carrying it has completed, and `settings` stays `null` on a session
with none to show (every opencode lane). `stale` is the banner — the reason a lane is not live
— and `ended` is a different fact: `true` only once the subscription is over, where a `stale`
lane alone may be reconnecting on its own.

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
| `sidebar.dump` | **always** | `{servers, machines}` — the sidebar's status foot |
| `craze_create.dump` | while the craze create sheet is open | `{craze_create}` — what the sheet rendered: `machine`; `state` (`loading` \| `failed` (the options) \| `idle` \| `submitting` \| `refused` \| `unknown` \| `created`); `providers[]` — `{id, label, state, dimmed, selected, reason, fix}`, a non-ready one dimmed and never selectable; `preselected` (the default provider when listed and ready, else the first ready one) and `default_provider`; `recent_dirs`; `values` (the typed "Directory" and "First prompt"); the `request_id` it holds (only while a submission is in flight or its outcome is unknown); its `note` (offline / too old / not installed, or "no provider is ready on this machine"); the `error` as shown (`{code, message, where, text}`), a start failure's `cause` verbatim, the directory's own `cwd_problem`; `create_enabled` and the `primary` button's label (Create, Try again, Creating…) — read off the mounted DOM; `null` when none is mounted |
| `lane.dump` | while the transcript panel is open | `{lane}` — what the panel rendered: its rows, approval cards, `kind` badge (and `lane_kind`, the kind it was opened with), the `permission` line (`bypass` reads "runs tools without asking"), Interject toggle, `can_cancel` and `stop` (`{confirming}`, or `null` with no Stop button) — all read off `lane.messages`' `capabilities`; Cancel, Interject and Stop exist only when the capabilities offer them, and Cancel/Interject are live only while the session is working — `stale`, `ended`, and its error; `null` when none is mounted |

The transcript panel is mounted by `ui.show_lane {machine, kind, session_id}` (the card's
Transcript affordance, which is a click) and unmounted by `ui.close_lane`.

The New-session dialog is driven the same way: `ui.show_launch` opens it, and — **in test
mode only** — `ui.fill_launch {mode?, target?, command?, workdir?}` types into it (`mode` is
`agent` or `command`; `target` is a "Where" value, `machine:<name>` or `shed:<host>/<name>`; an
absent key is left as it is) and `ui.submit_launch` presses Create, through the button's own
gate. They exist because the "Run a command" mode is reachable only by typing and clicking;
outside test mode both answer `not_enabled`, and the production door onto the same backend is
`roost.run`. A `craze:<machine>` target (a machine that offers New craze session) leads on to
the craze create sheet: its button reads Continue.

The craze create sheet is opened by `ui.show_craze_create {machine}` (the New craze session
action, a click; opening it reads `craze.create_options`, so on a dormant machine it starts the
hub) and closed by `ui.close_craze_create` — closing it while a create runs keeps the create
running, and its session simply appears as a row. **In test mode only**,
`ui.fill_craze_create {provider?, cwd?, prompt?, recent?}` fills it the way a person would
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
