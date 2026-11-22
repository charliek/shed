# IPC

shed-desktop exposes a control socket so the app can be driven and observed
programmatically — by `shedctl`, by the functional test harness, or by hand. This is a
first-class feature: it is how changes are verified without a human clicking.

## Transport

- A Unix-domain socket at `$XDG_RUNTIME_DIR/shed-tauri.sock`, falling back to
  `/tmp/shed-tauri-<uid>/shed-tauri.sock` when `XDG_RUNTIME_DIR` is unset (the usual case on
  macOS). `SHED_TAURI_SOCKET` overrides the path. The socket is mode `0600` and its directory
  `0700`.
- A sibling `shed-tauri.lock` keeps a single instance: a second launch sends `app.activate`
  to the running app and exits.
- Newline-delimited JSON, one object per line, 1 MiB frame cap (a longer line is answered
  `frame_too_large` and the connection closes).
- Request: `{"id": "<string>", "op": "...", "params": {...}}`
- Response: `{"id": "...", "ok": true, "result": {...}}` or
  `{"id": "...", "ok": false, "error": {"code": "...", "message": "..."}}`

Error codes are stable snake_case strings. The common ones are `unknown_op`, `bad_request`
(a missing or malformed param), `not_enabled` (a test-mode-only op outside test mode),
`not_supported`, `not_found`, `action_failed` (the op was tried and failed),
`frontend_not_ready` (the UI has not reported yet; retry) and `internal`. Op families add their
own, documented with them below.

## Core ops

| op | params | result |
|----|--------|--------|
| `identify` | — | `socket_path`, `pid`, `core` (`rust`), `platform` (`tauri`), `test_mode`, `mock_base_url` (`null` outside test mode), `log_path` (the app's log file, `null` when file logging is off) |
| `sheds.list` | — | `{sheds, host_errors}` — a fresh query of every configured host |
| `sheds.refresh` | — | `{sheds, host_errors}` — asks the dashboard to re-fetch, waits for it to render, and answers with the listing it committed |
| `system.df` | — | `usage[]` (per-host `GET /api/system/df`: totals + image/shed/orphan disk entries) |
| `hosts.auth` | — | `hosts[]`, each `{name, auth_mode, learned}`: the credential mode per server (`mtls`, or `token` for any other entry: a server older than 0.10.0 still in token mode (removed in 0.10.0), or an entry that records no mode) and whether this session learned it or read it from the config |
| `app.screenshot` | — | `png` (base64), `width`, `height` |
| `app.activate` | — | `{}` — raises the dashboard window (what a second launch sends) |
| `app.quit` | — | `{}`, then the app exits (test mode only): the tray's Quit, drivable — the orderly shutdown runs, then the process ends (see [Architecture](architecture.md#shutting-down)) |
| `app.terminate` | — | `{}`, then the app exits (macOS, test mode only): the macOS system quit, drivable — `[NSApplication terminate:]`, the path a Dock Quit, a logout and an AppleScript `quit` take, so the orderly shutdown runs from `RunEvent::Exit` alone and the process ends with status 0. Off macOS it answers `not_supported` (use `app.quit`) |

`app.screenshot` shells out to a platform capture tool — `grim` (Wayland/wlroots), `scrot` or
ImageMagick `import` (X11) on Linux, `screencapture` on macOS — and returns the first non-empty
PNG, or `screenshot_failed` naming each tool's problem. It captures the screen, not one window,
so treat it as a visual aid; the `*.dump` ops below are the deterministic truth. On macOS
`screencapture` needs the Screen Recording permission, so in an agent or headless session it
fails there.

**Per-host failures.** A host whose sheds can't be listed is reported rather than dropped:
`host_errors[]` carries `{server, kind, summary, detail}` per failed host, where `kind` is
`other`, `summary` is the one-line text naming the host, and `detail` is the hover/log body. It is always
present — `[]` when every host is healthy. The UI-truth op `dashboard.dump` returns
`{rows, host_errors, empty}`: the rendered shed rows (each with the `status_text` and
`controls` its card shows, [below](#ui-truth-ops)), the failures the shell holds, and the
rendered empty state (`{title, body}`, `null` when the list rendered).

The Sheds pane renders no error rows of its own. An unreachable server is a *status*, and
status lives in the sidebar's **SHED SERVERS** section (the row's dot, with the reason on
hover) and the System pane. The pane's only duty when it cannot list is to not claim "No
sheds yet" — its empty state names the unreachable servers and points at the sidebar,
carrying no transport error text.

## Window, panes + preferences

| op | params | result |
|----|--------|--------|
| `ui.navigate` | `pane` (sheds\|approvals\|agents\|activity\|egress\|system) | `pane` |
| `ui.current_pane` | — | `pane` (as the dashboard last reported it) |
| `ui.modal` | — | `modal`: `create` \| `craze` \| `null` |
| `ui.badges` | — | `badges`: the sidebar nav badge counts `{sheds, agents, hosts, pending}`, `null` before the first report |
| `ui.computed_style` | — | `style`: the dashboard's reported computed style (its `mode` echoes `ui.set_appearance`) |
| `ui.set_appearance` | `mode` (light\|dark) | `{}` |
| `ui.show_window` | — | `{}` (raises the dashboard; same as `app.activate`) |
| `ui.show_create` | — | `{}` (raises the dashboard and opens the Create dialog) |
| `ui.show_preferences` (alias `ui.open_preferences`) | — | `{}` — opens or focuses the Preferences window without raising the dashboard |
| `egress.show` | `tab?` (activity\|profiles), `profile?`, `host?` | `{}` — drives the Egress pane (it must be the current pane) |
| `prefs.dump` | — | `{visible, title, prefs}` — the Preferences window's native state plus its reported `{sections, values, mode}` snapshot (`prefs` is `null` before the first open) |
| `prefs.close` | — | `{}` (hides the Preferences window) |
| `prefs.get` | — | `{terminal_preset, terminal_template}` |
| `prefs.set_terminal` | `preset` (ghostty\|roost\|custom), `template?` | `{}` (persists the terminal preference) |
| `terminal.presets` | — | `presets[]`, each `{id, label, detail, available}` |
| `loginitem.status` | — | `enabled` |
| `loginitem.set` | `enabled` (bool) | `{}` |
| `tray.show` / `tray.hide` / `tray.toggle` | — | `{}` — runs the same path a tray-icon left-click runs (the macOS popover) |
| `tray.dump` | — | `{present, items, popover, popover_visible, popover_height}` — whether the tray installed (a headless or no-StatusNotifier Linux session has nowhere to show it), its menu item ids, and on macOS the popover's reported rows, visibility and height (`popover` is `null` on Linux, which has no popover) |
| `updater.status` | — | `{os, enabled, reason, instantiated}` — the Sparkle updater's state (macOS; disabled in test mode) |
| `updater.check` | — | `{}` — fronts the app and presents Sparkle's check; a disabled updater answers `updater_disabled` with the reason |

## Lifecycle, create + terminal

| op | params | result |
|----|--------|--------|
| `shed.start` / `shed.stop` / `shed.reset` / `shed.delete` | `host?`, `name` | `{}`; a failure is `action_failed` |
| `create.start` | `host?`, `name`, `repo?`, `local_dir?`, `image?`, `backend?`, `cpus?`, `memory_mb?`, `no_provision?` | `create_id` |
| `create.status` | `create_id` | the create's progress snapshot (poll until `complete`/`error`); `{state: "unknown"}` once it is cancelled or gone |
| `create.cancel` | `create_id` | `{}` — aborts the create's stream and drops its state (idempotent) |
| `terminal.preview` | `host?`, `shed`, `session?`, `preset?`, `template?` | the resolved ssh command and terminal invocation (spawns nothing) |
| `terminal.open` | `host?`, `shed`, `session?`, `preset?`, `template?` | launches the terminal (`not_enabled` in test mode; use `terminal.preview`) |

`preset` is a `terminal.presets` id (`ghostty`, `roost` or `custom`) and `template` the custom
command template; both default to the saved preference.

## Agent sessions

Every session here is on a **shed**. A shed's roost tabs are what its own
`roost-session` reports, shown read-only. A craze session on that shed is the one
lane the desktop drives. (Before 0.9.0 a shed's rows came from an in-guest RC hub.
The guest binary and the hub are gone. Plan 038 removed `machines:` hosts, the
implicit `localhost` host, `LaunchModal`, and the opencode lane.)

`ui.navigate` with `"machines"` is `bad_request`. These ops answer `unknown_op`:
`machines.list`, `machines.dump`, `machine.add`, `machine.kill`, `machine.capabilities`,
`machine.launch`, `roost.launch`, `roost.run`, `rc.kill`, `ui.show_launch`,
`ui.close_launch`, `ui.fill_launch`, `ui.submit_launch`, `launch.dump`. (`rc.launch`
was already `unknown_op` in 0.10.0.) Opening an interactive roost tab, killing one,
or driving a host that is not a shed belongs in the `roost` app.

| op | params | result |
|----|--------|--------|
| `rc.list` | `host?`, `shed?` | `{sessions, capabilities, hosts, shed_craze}` — `hosts` is the shed roster (the key was `machines`). Each session row is a shed (`target: "roost:<server>/<shed>"`, `host`, `shed`, `source: "roost"` or `"craze"`). `shed_craze` lists the unlisted sheds craze is offered on |
| `rc.inject_test` | `target` (`roost:<server>/<shed>`), `slug`, `kind?`, `display_name?`, `workdir?`, `lifecycle?`, `attention?` | `{}` — **test mode only**; puts a row into that shed's roost snapshot. A machine name is rejected |
| `roost.probe` | `target` (`roost:<server>/<shed>`, a server-listed shed) | `target`, `probe` (its `fingerprint` nested inside) and `plan` — a read-only look at that shed's `roost-session`. The plan matrix row comes back from here too |
| `roost.preview` | `target` | the plan (Install/Update/Start/Report/nothing to do) plus the sentence naming where the bytes would come from |
| `roost.bootstrap` | `target`, `fingerprint`, `consent: true` | installs/updates/starts `roost-session` on that shed and wires its agent hooks; refuses without consent or against a stale fingerprint |

`roost.probe`, `roost.preview` and `roost.bootstrap` are how the desktop
drives [putting `roost-session` on a shed](../extensions/roost-session-hosts.md).
Kicking off a `claude` or `opencode` tab from roost's own command palette is
[the `shed` roost provider](../extensions/roost-provider.md), which lists running
sheds only.

`capabilities` is keyed by a row's **`target`** — `roost:<server>/<shed>` — so a
card can read the contract behind the row it is drawing. An opencode tab is
passive: `feed: "activity"`, `watch: false`, label and activity only, no transcript.

### Sessions from roost

A shed's roost rows come from that shed's `roost-session` daemon over the shed's
own SSH. Plan 014 replaced the 2 s poll with roost's **observer event stream**.
At session protocol 6 there is no lease to contest. Only **agent-owned tabs** are
sessions; a plain shell tab in roost is not one. A sticky **attention dot** on the
card mirrors roost's own notification bit. There is no kill and no launch from
this app: a roost row is status. A shed with no `roost-session` lists no roost
sessions, and the Sheds pane offers the bootstrap.

### Craze sessions

Each running shed has a **craze
source**: its craze hub's live session list (craze is the provider abstraction for cursor,
grok, claude and native sessions). Its rows ride `rc.list` beside roost's, stamped
`source: "craze"`, `kind: "craze"`, with `slug` = the session's **hostId**, the hub's own
facts (`provider`, `model`, `doing`, `head_ask_summary`, `last_reply`, `attached`,
`start_error`, `pending_approvals` — a count here — `permission_mode`,
`provider_session_id`), and `agent_lane: {kind: "craze", session_id: <hostId>}`.

**For a craze session the hub row IS the row.** A roost tab owned by craze (a craze TUI)
folds into the hub row it names — the row gains that tab's `tab_id`, and the tab is not
listed as a roost row — but only while the host's craze feed is **live**; with it dormant,
offline or absent, roost's row stands alone as before. A craze row a source still holds while
it is not live is the last known one: `stale: true`, `approximate: true`. The fold is computed
per shed.

A shed's source is **attach-only**: a find-only probe
(`craze providers --hub --json`, which never starts a hub) runs every 30 s while there is no
hub, and the roster attaches only once one is running — the app never starts a hub in the
background, and it never dials a local craze. `rc.list`'s `hosts` carries
the source's state:

| field | meaning |
|-------|---------|
| `craze.state` | `live` (attached), `dormant` (craze there, no hub running), `offline` (it dropped, or could not be asked), or `absent` (never reached — and not installed, which renders as absent) |
| `craze.cause` | an offline (or not-installed) state's class: `unreachable`, `too_old`, `failed`, `not_installed` |
| `craze.create`, `craze.create_options`, `craze.create_title` | the LIVE hub's capabilities; `false` otherwise. `create_title` (craze 0.1.1's `createTitle`) is what the create sheet's "Name (optional)" waits for: a dormant machine's sheet shows the field once the hub it starts is live |
| `craze.source_gen` | the generation of the host's craze source: its **incarnation**, as `craze.create_options`, `craze.saved` and `craze.reopen` report it; `null` while the host has no craze source. A host removed and added again under the same address is a new incarnation, and generations only grow, so the new one's is always larger. The app resets the create sheet's per-machine Resume state when a status names a newer one, whether the sheet is open or not, and without reading the create options (a read that can fail) |

A shed whose craze is too old says so on its Sheds-pane row (`shed_roost.dump`'s
`craze_note`). A shed without craze says nothing.

**A running shed is offered craze before it is listed.** A shed has a status row only once
something answers there — its roost-session, or its craze hub (the roster's first `Ready`, or a
session created there). Before that, a running shed whose probe proved craze there is an entry
of `rc.list`'s `shed_craze`, read under the same lock as the status rows, so a host is never
both:

| field | meaning |
|-------|---------|
| `name`, `label`, `origin`, `kind` | the status row's identity fields (`name` is the address, `roost:<server>/<shed>`) |
| `craze` | the same object a status row carries: `state` is `dormant` (offered New craze session on the shed's Sheds-pane row; opening the sheet starts its hub) or `offline` with `cause: too_old` (its row shows the too-old note) |

A shed whose craze is not installed, that the probe cannot reach, or that has not been probed
yet has no entry. The probe stays find-only: nothing dials `bridge --hub` there until the sheet
opens. `shed_craze` honours `rc.list`'s `{host, shed}` filter as the status rows do, and a
change of the entry (dormant ↔ too old ↔ none, the shed stopping or being deleted) pushes the
UI's re-read; a repeated probe that says the same thing does not.

#### Creating a craze session, and Open in terminal

A shed offers **New craze session** when its craze is **live** and its hub can list providers
and create, or when it is **dormant** — opening the sheet on that shed's Sheds-pane row is
then the explicit action that starts its hub. A live hub that cannot create shows "update
craze on this shed to create sessions here"; listing still works.

| op | params | result |
|----|--------|--------|
| `craze.create_options` | `target` | `{target, options: {providers, default_provider?, recent_dirs, reopen}, source_gen}` — every provider in craze's order with its `state` (`ready` \| `needs_setup` \| `unavailable`) and, when not ready, craze's `reason` and `fix`; the default provider as craze states it; the recent directories, newest first; `reopen`, whether the hub can list and reopen saved sessions (craze 0.2.0+, below). `target` is `roost:<server>/<shed>`. `source_gen` is the generation of that shed's craze source the call went through: its **incarnation**, which the address does not name (a shed removed and added again under the same address is a new one, with a new, larger generation); the sheet binds the state it keeps to it, as it does to every status row's `craze.source_gen`. Read afresh on every call. **On a dormant shed this starts its hub** (an explicit action), and the shed's source attaches to it at once |
| `craze.create` | `target`, `cwd`, `provider?`, `prompt?`, `title?`, `request_id?`, `held?` | `{session, host_id, ended, prompt, prompt_error?, request_id}` — a new session (provider, directory, an optional name and first prompt only: no model, effort or permission mode). `cwd` must be absolute; a blank `provider` (craze's default) or `prompt` (an idle session) is absent. `title` names the session from its first frame — trimmed, and a blank one is absent, never sent empty; craze normalizes it, and refuses one that is blank once normalized (`bad_request`, `params.title is blank once normalized: send none instead`). A hub without `createTitle` refuses a fresh titled create before anything is sent; a retry or a held replay keeps the title exactly and stays `outcome_unknown`. `session` is its row, **already in the machine's listing**, so `lane.open {target, session_id: host_id}` works at once — unless `ended` is `true`: craze replayed an earlier create's answer (it does, for ten minutes, under the same `request_id`) for a session that has since ended, and nothing is listed. `prompt` is `none` \| `accepted` \| `unknown` \| `refused`. `request_id` is the caller's — a present one must be a string in craze's form (1–64 of `[A-Za-z0-9._-]`), else `bad_request`, and is never replaced — or minted when absent (or `null`), and answered back. `held: true` says the call **replays** a `request_id` kept from an unknown outcome (below); it needs the `request_id`, else `bad_request`. Every field is typed: a non-string (a non-bool `held`) is `bad_request`, never read as absent |
| `craze.open_terminal` | `target`, `session_id` (the row's hostId) | `{origin, target, session_id, tab_id, cwd, argv}` — a roost tab running `craze attach --session <hostId>` (craze's ladder, the same `sh -c` composition every craze command uses) in the session's workspace. `target` is `roost:<server>/<shed>` (and `origin` is the same address). The hub row shows that tab (`tab_id`) from then on, across roost snapshots — and while the shed's craze feed is down, on the retained (stale) row — for as long as roost lists the tab: a snapshot taken after the open — or by a restarted roost — that no longer lists it ends that. Closing the tab in roost only **detaches**; `/exit` inside it **stops** the session. The desktop card has no close-tab action. The hostId must be twelve lowercase hex digits (`bad_request`) and a session the shed lists (`unknown_session`) |

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

#### Resuming a saved craze session

The create sheet's Resume half (plan 032 §3.4) lists a host's **saved** sessions — ones its
craze remembers that nothing runs now — and runs one again. Both ops need craze 0.2.0's hub
capability `reopen`, which `craze.create_options` reports as `options.reopen` (read from the
same hub `hello`, so a dormant machine's sheet learns it from the hub the sheet started). Like
the create ops, each is an explicit action on its own connection, through the ungated
`bridge --hub`, so on a dormant machine either may start the hub, and the host's source is
woken.

| op | params | result |
|----|--------|--------|
| `craze.saved` | `target` | `{target, saved: {sessions, more}, home, source_gen}` — craze's saved sessions, newest first, at most 50 (`more` is `true` when craze left some out), each `{id, provider, workspace, title?, updated_unix_ms, created_unix_ms?}`. `id` is opaque: craze's session id, or `<provider>:<providerSessionId>` for a session recorded before craze ids. `workspace` is absolute and not checked to exist. `title` is absent when the session has none; `created_unix_ms` is absent when craze recorded no time. `home` is `/home/shed`, the shed user's home, which the sheet uses to shorten a workspace under it to `~`. `source_gen` is the incarnation the list came through, as `craze.create_options` reports it. Read afresh on every call |
| `craze.reopen` | `target`, `id` | `{session, host_id, ended, superseded, joined, id, source_gen}` — the saved session `id` running again: craze **joins** a host already serving it (`joined: true`) or starts one that loads it (`joined: false`). `session` is its row, **already in the machine's listing** (the source keeps it as a created row, even for a hostId whose earlier session ended), so `lane.open {target, session_id: host_id}` works at once, and a lane on that hostId that was departing is live again (a joined host that outlived its hub, say); `lane.open` on a FINISHED lane there opens a fresh one — unless `ended` is `true`: the session was gone again by the time its row was folded, and nothing is listed; or `superseded` is `true`: the machine was removed, or removed and added again, while the reopen ran, so its answer came through a craze source the machine no longer has (`source_gen`, the incarnation the reopen went through). Nothing is listed then, `session` is the last known row (`stale`), and there is nothing to open on it: a `lane.open` goes through the machine's current source. The row's own `stale` is not that test — a reopen on a dormant machine answers `stale: true` too (its hub just started, its source not yet attached) and opens at once. `id` must be a non-blank string (`bad_request`). It can take minutes: the app allows craze 200 s per answer and sends a lost answer again once; a caller that stops waiting abandons the wait, never the reopen |

A reopen's refusal keeps its kind as the code, so a caller branches on the code and never on
the words:

| code | meaning | the sheet |
|------|---------|-----------|
| `gone` | the session cannot run here any more: no such saved session (`unknown_session`), or one craze will not resume (`not_accepting`/`not_resumable`: its directory is gone, say) | drops the row, and says why where it was |
| `start_failed` | it was found and its start failed: `could not start: <craze's cause>` | shows the cause, keeps the row |
| `unavailable` | transient: craze busy or closing, the dial failed, or the answer was lost twice ("…the session may already be running — Retry is safe": a reopen of a running session joins it); also a hub that cannot reopen at all — "this craze hub (craze <version>) does not offer reopening saved sessions" (`version unknown` when the app has not read the hub's `hello`; a hub that says a version below 0.2.0 and its pid adds "end it with kill <pid>") | shows it, with Retry |
| any other | the lane contract's own code (`bad_request`, `failed`, …), or the create ops' `not_installed`, `too_old`, `no_craze`, `shutting_down` | shows it, keeps the row |

`craze.saved` refuses with the create ops' codes: `unavailable` for a hub without `reopen` (the
same words), the lane contract's otherwise.

### Agent lanes

A craze row opens a live transcript through these ops. `target` is
`roost:<server>/<shed>` and `session_id` is the hub's hostId, not the roost tab's slug.
There is no `kind` and no `machine` parameter: craze is the only lane. An opencode roost
tab is status only (no transcript). The `lane-event` envelope is `{target, session_id, event}`
with no `machine` and no `kind`. The full contract — staging, reconnects, the answer forms,
the failure codes — is [Agent lanes](agent-lanes.md).

| op | params | result |
|----|--------|--------|
| `lane.open` | `target`, `session_id` | `{session}` — the session row alone. Idempotent: a second call re-answers from the open lane — also a craze lane whose row has left (departing or finished, below); a finished one whose row is listed again is replaced by a fresh lane once that open succeeds (a failed one leaves it readable) |
| `lane.messages` | `target`, `session_id` | `{messages, activity, session, generation, stale, ended, finished, capabilities, settings, slash_menu, effort_option}` — the staged view, never a half-seeded one; `finished` is `true` once the lane is over for good (below) — not `ended`, which a retryable `Down` sets too; `session` is the live session row (`null` until the first seed); `slash_menu` is the session's `/` menu RESOLVED by the shared core — `{rows: [{name, qualified?, description?, source, arg, arg_hint?, while_working, action}], hidden: [{name, reason}]}`, each row's `action` one of `prompt`, `set_title`, `set_mode` (`{change: {kind: "mode", id}}`), `stop`, `model_picker`, `effort_picker` (`{option, for_model, levels: [{id, name}], default_level?}` — craze 0.2.0's `/effort`, shown only while the settings' current model has an effort option; else hidden, "the current model has no effort setting", and re-resolved on every settings change) — or `null` when the capabilities do not say `slash` or no catalog has arrived (no cap on the rows); `effort_option` is the id of `settings`' effort option by craze's `EffortOption` rule (what the chip shows as the effort and `/effort` sets), `null` when the settings shown are or name none |
| `lane.approvals` | `target`, `session_id` | `{approvals}` — pending only, oldest first |
| `lane.send` | `target`, `session_id`, `text`, `mode?` (`queue` \| `interject`) | `{}` |
| `lane.cancel` | `target`, `session_id` | `{}` |
| `lane.answer` | `target`, `session_id`, `approval_id`, `answer` | `{}` |
| `lane.stop` | `target`, `session_id` | `{}` — ends the **session** (craze's `session.stop`), answered on craze's receipt; the row leaves, and the lane ends — `ended` and `finished`, stale `session_closed` — when the session closes (usually just after the row, within the departure grace below). Refused by a session whose capabilities say `stop: false` |
| `lane.settings` | `target`, `session_id` | `{settings}` — the session's settings, read now: `model` and `models` (craze's order: the current model, the remembered ones by rank, the rest in catalog order; on a native session each model may carry `effort` — `{efforts, default_effort?, switch_effort?}`, craze 0.2.0's effort facts: its levels, its default, and the level a switch to it lands on — absent on every other session's), `mode` and `modes` (none when its capabilities offer no switchable modes), `options` (the current model's own, the model and mode rows excluded, `thought_level` first, then `model_config`, each in the provider's order; values in the provider's order) and `usage` (`{context_tokens, context_window}`, a native session's). The empty default on a session whose capabilities say `settings: false`. `lane.messages`' `settings` is the stream's copy, which the sheet renders. A finished lane answers its last-stated settings from the view, with no call to the agent |
| `lane.set` | `target`, `session_id`, `change` | `{}` — change one setting (craze's `session.set`). `change` is `{kind: "model", id}`, `{kind: "mode", id}` or `{kind: "config", id, value, for_model?}` — ids as `settings` offers them, verbatim; anything else (an unknown kind, a field beside the form, an empty id or `for_model`) is `bad_request`. A config change is bound to `for_model`, the model the caller DISPLAYED the option for (the settings sheet always sends it), or — absent — to the model the lane has folded, waiting for the lane's first settings when none has arrived (refused `unavailable`, unsent, if they do not come within the verb deadline); craze refuses it `stale_model` — `not_accepting` — once the session has left that model, and a retry is a new command. `{}` is craze's confirmation; the new value arrives on the stream, ahead of it — or, when craze could learn no revision (`rev: 0`, no event will follow), from the confirmed value, which the lane applies itself. An answer lost to a drop is `outcome_unknown`: the change may have run, it is never resent, and the next `settings` says what the session is at |
| `lane.set_title` | `target`, `session_id`, `title` | `{}` — rename the **session** (craze's `session.setTitle`; the composer's `/rename <title>`). The title is trimmed; a blank one is `bad_request`, never sent. The new title arrives on the stream (`lane.messages`' `session.title`, from craze's delta), not from this answer. `{warning}` is a rename that TOOK and craze could not record (`index_write`): `"renamed, but craze could not record it: <cause>"` — a success, shown as a warning, never rolled back or retried. An answer lost to a drop is `outcome_unknown` (never resent); a session whose capabilities say `set_title: false` (every opencode one) refuses it |
| `lane.close` | `target`, `session_id` | `{}` — idempotent; removes the lane whether it is live, departing or finished |

Every frame reaches the frontend as the `lane-event` Tauri event, `{target, session_id, event}`.
A roost snapshot never evicts a craze lane; its row's craze source governs it. When the
row leaves (removed, a reseed that no longer lists it, or the hub gone, which takes a session a
`craze.create` or `craze.reopen` answered with along too, before any roster listed it) the lane
**departs**: it keeps its own connection for a 10 s grace, in which the session's final `Down`
(`session_closed` after a Stop) ends it as usual and a row listed again keeps it live, whether
the roster lists it or a `craze.create` or `craze.reopen` answers with it; at the grace's end the
desktop ends it itself — `Down{session_gone}` for a removed row, `Down{hub_gone}` for a hub that
went. The desktop has no close-tab action, and `machine.kill` answers `unknown_op`. The host's removal ends every lane of its source at once, `Down{host_removed}`. All three are
final reasons, like `unknown_session`, `session_closed` and `start_failed:<cause>` (see
[Agent lanes](agent-lanes.md#a-lane-outlives-its-row)).

**A finished lane** — ended by any final reason — stays registered and readable
(`lane.messages`, `lane.approvals`, `lane.settings` from its view) until `lane.close`, with its
connection released however it finished. Every verb on it (`lane.send`, `lane.cancel`,
`lane.answer`, `lane.set`, `lane.set_title`, `lane.stop`) is refused at once with `lane_ended`,
"the session has ended" — never sent, and never the 10 s wait and `unavailable` a reconnecting
lane gives; a verb already under way when the lane finished runs its course.

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
with none to show. `stale` is the banner — the reason a lane is not live
— and `ended` is a different fact: `true` only once the subscription is over, where a `stale`
lane alone may be reconnecting on its own.

**So is the session row, once there is one.** `lane.open`'s `session` is the row as the
source listed it at the open, and a second `lane.open` re-answers that same row for as long as
the lane stays open. `lane.messages`' `session` is the stream's latest — `null` until the first
seed has swapped one in — and it carries what the session states after the open: for a craze
session opened the moment its create answered, the open's row is the create's own, and only the
stream's carries the attach's `permission_mode`. The panel's header (title, directory,
permission line) reads the live row, and `lane.open`'s only until the first seed.

### UI-truth ops

These report what the frontend RENDERED, so a test can assert the window rather than the
backend's view — the two can disagree, and have.

| op | answers | result |
|----|---------|--------|
| `dashboard.dump` | on the Sheds pane | `{rows, host_errors, empty}` — each row is the shed as shed-core decoded it (`status` is `running`, `stopped`, `starting`, `stopping`, `error`, or `unknown` for a status the app does not know; `status_reason` the server's reason code or `null`), plus what its card rendered, read off the row: `status_text` (the status, and the reason in parentheses as `shed list` shows it: `stopped (capacity)`) and `controls` (`{spinner, buttons}`: whether the busy spinner shows, and the titles of the action buttons present — a `starting` or `stopping` shed shows the spinner and none). Both are `null` when the card is not mounted. Rows of one server that share a name take their cards in order, and are `null` when the number of rows and cards differs, so a row never reports another row's card |
| `agents.dump` | on the Agents pane | `{sessions, empty}` — `empty` is the rendered empty state (`{state, title, body, action}`), `null` when rows rendered |
| `egress.profiles` | on the Egress pane | `{egress}` |
| `sidebar.dump` | **always** | `{servers}` — the sidebar's status foot; each server is `{name, reachable, detail}` |
| `roost_consent.dump` | while the roost bootstrap consent dialog is open | `{consent}` — the dialog's rendered copy, `null` when none is mounted |
| `toast.dump` | **always** | `{toast}` — the toast on screen, `null` when none is |
| `shed_roost.dump` | on the Sheds pane | `{sheds}` — each RUNNING shed's row by its `roost:<server>/<shed>` target: its roost line (`status`, `button`, `busy`, `error`, `rendered`), and its craze: `craze_create` (whether the row shows New craze session) and `craze_note` (the too-old note it shows, else `null`) — read off the row |
| `craze_create.dump` | while the craze create sheet is open | `{craze_create}` — what the sheet rendered: `target` (`roost:<server>/<shed>`); `state` (`loading` \| `failed` (the options) \| `idle` \| `submitting` \| `refused` \| `unknown` \| `created`); `providers[]` — `{id, label, state, dimmed, selected, reason, fix}`, a non-ready one dimmed and never selectable — except the provider of a kept request id being replayed, which stays `selected` whatever its state (one the re-read no longer lists is a row of its own, `state: "absent"`); `preselected` (the default provider when listed and ready, else the first ready one) and `default_provider`; `recent_dirs`; `values` (the typed "Directory", "Name" and "First prompt"); `title_field` (whether "Name (optional)" is shown: the shed's live hub takes a title at create, or one is typed) and `title_problem` (a refusal about the title, shown under it — craze's `params.title …` `bad_request`); the `request_id` it holds (only while a submission is in flight or its outcome is unknown); its `note` (offline / too old / not installed; else, while a kept request id is replayed, the unknown outcome and "Changing the form starts a new request instead."; else "no provider is ready on this machine"); the `error` as shown (`{code, message, where, text}`), a start failure's `cause` verbatim, the directory's own `cwd_problem`; `create_enabled` and the `primary` button's label (Create, Try again, Creating…; empty in the Resume half); `modes` — `{offered, enabled}`: whether the `New \| Resume` control is shown (the options say `reopen`) and usable (not while a create submits or a reopen runs); `mode` (`new` \| `resume`, the half on screen); and `saved` — the Resume half as rendered, `null` while New shows: `{state, error, more, gone, rows[]}`, `state` the list's read (`loading` \| `failed` \| `ready`), `error` a failed read's words, `more` "Showing the 50 most recent." when craze left some out, `gone` the note a dropped row left (its title and craze's words), and each row `{id, title, provider, workspace, updated, state, error, button, enabled}` — `workspace` as shown (the home shortened to `~` where known), `updated` its age as shown, `state` `idle` \| `resuming` \| `refused` \| `unavailable`, `error` its refusal as shown, `button` Resume / "Resuming… (this can take a few minutes)" / Retry — read off the mounted DOM; `null` when none is mounted |
| `lane.dump` | while the transcript panel is open | `{lane}` — what the panel rendered: its rows, approval cards, `target`, `session_id`, and its `kind` badge (the view's `capabilities.kind`), the `permission` line (`bypass` reads "runs tools without asking"), Interject toggle, `can_cancel` and `stop` (`{confirming}`, or `null` with no Stop button) — all read off `lane.messages`' `capabilities`; Cancel, Interject and Stop exist only when the capabilities offer them, and Cancel/Interject are live only while the session is working and the lane is not finished — `finished` (`lane.messages`' own) and `composer_disabled` (the composer's text box, Send, Interject and Cancel disabled because the lane is finished; the banner reads `ended · <reason>`), `settings_chip` (the header's `<model name> · <effort value> · fast`, from the current values, the effort being the option `lane.messages`' `effort_option` names; `null` — no chip — when the capabilities do not say `settings`), `stale`, `ended`, and its error; the composer's `draft`; and `slash_menu` — `{open, token, rows: [{name, dimmed, highlighted}], draft, refusal, warning}`, the `/` menu as rendered: `open` is the menu ON SCREEN (the shared filter's, less an Esc dismissal, which lasts until the token changes), `rows` what it lists in order (`dimmed` while a turn runs and the row would wait for it — "after this turn"; `highlighted` what Enter and Tab accept), `refusal` the line a refused command left (the shared core's reason, craze's words, or "may have applied — check the session"), `warning` a rename that took with a caveat — or `null` on a session with no menu (no `slash` capability, or no catalog yet); `null` when none is mounted |
| `lane_settings.dump` | while the settings sheet is open | `{lane_settings}` — what the sheet rendered: its `target`, `session_id` and `chip`; `rows[]` in render order (the model, the current model's options, the mode) — `{id, kind, name, category, control, current, current_name, values: [{id, name, selected, secondary}], state, text, enabled}`, where a value's `secondary` is the dim line drawn under it — on the model list, `effort → <level>` for a model other than the current one whose effort facts name the level a switch lands on (its `switch_effort`, else its `default_effort`; a native session's), else `null` — `control` is `list` (the model, and any row of more than four values) or `segmented`, `state` is `pending` (craze has not answered; the row takes no press), `refused` (`text` the refusal shown inline — "the model changed; try again" on an option whose model moved under it) or `not_confirmed` (the answer was lost; until the session's next settings), else `null`; and `usage` (`{tokens, window, percent, text}`, the context meter, when the session reports both) — `null` when none is open |

**In test mode only**, `ui.report_history {clear?}` answers what every one of the dashboard's
full reports carried, oldest first: `{reports: [{hosts, rows, servers}], dropped}` — the Hosts
badge, the dashboard's row count and the sidebar's server count, per report — from a ring of
the last 256. `dropped` counts the reports the ring evicted since it was last cleared, and
`clear: true` empties it after the read. A sampled `ui.badges` can fall between two reports and
miss one; the history cannot. Outside test mode it answers `not_enabled`.

The transcript panel is mounted by `ui.show_lane {target, session_id}` (the card's
Transcript affordance, which is a click) and unmounted by `ui.close_lane`.

Its settings sheet is opened by `ui.show_lane_settings {target, session_id}` (the header's
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
`/<name> `; a mode, `/exit`, `/model` or `/effort` runs at once — `/effort` with a level typed
after it sets that level; while a turn runs, a row that waits for it is refused instead, "runs
after this turn", but for an `/effort` level that names nothing, refused for that first) —
Tab completes, Esc closes the menu (and
stops there: an open dialog's Esc handler ignores an Esc already handled) until the token
changes, and ↑/↓ move the highlight. With no menu up — zero matches included — Enter sends.
Shift+Enter is a newline, and every key is ignored while an IME composes. Sending goes through
the same shared dispatch as Enter: a builtin the menu hides (`/help` in phase 1) is refused
inline, never sent as text; a prompt row is sent as `/<its spelling> <argument>`.

The roost bootstrap consent dialog opens from a card's plan button, a click, so it has doors
too — **test mode only**, because showing the card and confirming it are the two halves of a
user's consent: `ui.show_roost_consent {target}` opens it for a shed or machine target,
`ui.confirm_roost_consent` presses Confirm, and `ui.close_roost_consent` closes it. The
production door is `roost.bootstrap`, which takes `consent: true` and the probe's fingerprint.

The craze create sheet is opened by `ui.show_craze_create {target}` (the New craze session
action, a click; opening it reads `craze.create_options`, so on a dormant machine it starts the
hub) and closed by `ui.close_craze_create` — closing it while a create runs keeps the create
running, and its session simply appears as a row. **In test mode only**,
`ui.fill_craze_create {provider?, cwd?, title?, prompt?, recent?}` fills it the way a person would
(`provider` clicks that provider's row — a dimmed one refuses the click; `recent: <n>` taps the
n-th recent directory) and `ui.submit_craze_create` presses its primary button (Create, or Try
again) through the button's own gate; outside test mode both answer `not_enabled`, and the
production door is `craze.create`. After a create the sheet closes and opens the session's
transcript at once; a first prompt craze did not take is said in a toast (`toast.dump`).
Its Resume half has two more doors, **test mode only** on the same rule:
`ui.choose_craze_mode {mode: "new" | "resume"}` presses that half of the `New | Resume`
control (refused, as a pointer is, while the control is not shown or is disabled; choosing
Resume the first time after the sheet opens reads `craze.saved`), and `ui.resume_saved {id}`
presses that saved row's button — Resume, or Retry after `unavailable` — through its own gate
(one reopen at a time: while one runs every other button is disabled, and a press is
refused). Production doors: `craze.saved` and `craze.reopen`. After a resume the sheet
closes and opens the session's transcript at once, as after a create.

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
| `notifications.list` | — | `notifications[]` (what the gate posted) |
| `notification.invoke` | `id`, `action` (approve\|deny) | `{}` — drives a posted notification's action; `not_found` for an unknown id |
| `notification.open` | — | `{}` — a banner-body tap: raises the window on the Approvals pane |
| `ui.set_ssh_approval` | `method?` (biometrics-or-password\|biometrics\|prompt), `policy?` (always-allow\|per-shed-allow\|time-based-allow\|always-ask\|always-deny), `ttl?` | `{}` — applies and persists the SSH approval prefs and resets live SSH grants |
| `ui.ssh_prefs` | — | `{method, policy, ttl}` — the SSH approval prefs in effect |
| `prefs.provider_modes` | — | `{namespace: "approve"\|"deny"}` for `aws-credentials` and `docker-credentials` |
| `prefs.set_provider` | `namespace`, `decision` (approve\|deny) | `{}` — sets and persists an AWS/Docker mode |
| `prefs.remove_shed_rule` | `server`, `shed` | `{}` — removes one per-shed override (`server` verbatim; `""` is the single unnamed server) |
| `broker.status` | — | `{config, resolved_ssh_mode, gate_namespaces, servers, machine, machine_error, ssh, broker_error}` (`machine` is the broker's `{id, name}`, `machine_error` why there is none; `ssh` is what it signs for a shed, `{mode, allow_users, key_filter, keys_offered?, keys_available?}`; all three `null` before the broker started or when its config failed; a `servers[]` namespace's `state` is one of `connected`, `reconnecting`, `not-source`, `duplicate`, `rejected`, `stopped`), and a row and namespace carry `active_source` when the server names another machine as the credential source) — see [Architecture → The credential broker](architecture.md#the-credential-broker) |
| `source.status` | — | `{rows, this_machine, this_machine_error}` — one row per configured server, `{host, supported, source, this_machine, connected, busy, unreachable, error}` (`source` is the machine that answers credential requests for you there, `{id, name}` or `null`; a server older than credential sources — a plain `404` — is `supported: false` with no error; `busy: true` means the server answered `503 SOURCE_BUSY` on every one of a few retries, so the source is unknown for now rather than absent — read again later; `unreachable: true` (with `supported: false` and the reason in `error`) means the server could not be reached or its answer read — a transport failure, an undecodable body, a 5xx with no API error body — so whether it supports sources is unknown, not "no": keep what was last known and read again later); `this_machine` is this machine's `{id, name}`, or `null` with `this_machine_error` saying why |
| `source.use` | `host` | `{row, woke_broker}` — makes this machine the credential source on `host` (one request, never retried) and wakes the broker so it connects at once. Refusals: `bad_request` (unknown server), `machine_identity_unavailable`, `source_machine_not_found` (the server has not seen this machine's broker yet), `forbidden`, `unsupported`, `action_failed` |
| `ui.press_source_use` | `host` | `{}` — test mode only: presses the open Preferences window's own "Use this machine" button for `host` (a disabled or missing button takes no press) |

`approval.decide` with `persist:true` saves a per-`(server,shed)` rule (always-allow
when `decision:approve`, always-deny when `decision:deny`). For an approve, `scope`
controls the grant: `per-request` (once), or `per-session`/`per-shed` add an in-memory
grant lasting `ttl` (e.g. `1h`). `scope`/`ttl`/`persist` are reported to the broker
so its durable audit records how the decision was made.

## Test mode

When launched with `SHED_TAURI_TEST_MODE=1`, `identify` reports `test_mode: true` and the
`mock_base_url` the app's HTTP clients were redirected to (`SHED_TAURI_MOCK_BASE_URL`), so the
harness can confirm a run is hermetic before asserting anything. Test mode also unlocks the
fault-injection and driver ops marked "test mode only" above (`policy.set`, `rc.inject_test`,
`app.quit`, the `ui.fill_*`/`ui.submit_*` doors, and so on), swaps the notification presenter and
the Touch ID / polkit gate for fakes (so `notification.invoke` drives what `notifications.list`
reports, and every gate approves), disables
`terminal.open` and Sparkle, and never reads the developer's own `~/.shed/config.yaml`: an
unset `SHED_TAURI_SHED_CONFIG` loads an empty config.

`SHED_TAURI_MOCK_UNREACHABLE_HOSTS` (comma-separated server names) points those hosts at a
closed port instead of the mock, a deterministic per-host failure. `SHED_TAURI_ROOST_SOCKETS`
(`roost:<server>/<shed>=<socket path>,…`, test mode only) dials that shed's `roost-session`
on a local socket instead of over ssh. The map is per shed, not one socket for the
whole app and not a `localhost` key. A shed named in the map is dialled on that socket.
A shed that is not named still uses `SHED_TAURI_SSH_BIN` when that fake `ssh` is
configured, and is unreachable when it is not.

Craze is reached only on a listed shed, through the fake-`ssh` seam
(`SHED_TAURI_SSH_BIN`, debug builds only). There is no local craze path.
