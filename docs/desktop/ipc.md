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
| `sheds.list` | — | `{sheds, host_errors}` — a fresh query of every configured host; a cloud shed carries its server's `cloud` and `cloud_row` (what its row shows and offers: `badge`, `size_words`, `image`, `lease_words`, `lease_state` `ok`/`soon`/`ended`/`unknown`, `run_words`, `actions` `{start, stop, delete, extend}`, `extend_presets_h`, `extend_max_h`, `expires_words`, `cap_words`, `created_words`, and `extend_unresolved_h` with `extend_unresolved_words` while an extend of that shed incarnation has an unknown outcome, else `null`; Extend is disabled meanwhile), any other shed `cloud_row: null` |
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
| `ui.navigate` | `pane` (sheds\|agents\|credentials\|activity\|system) | `pane` — the five sidebar entries; any other pane (`approvals`, `egress`, `machines` included) is `bad_request` |
| `ui.current_pane` | — | `pane` (as the dashboard last reported it) |
| `ui.modal` | — | `modal`: `create` \| `craze` \| `null` |
| `ui.badges` | — | `badges`: the sidebar nav badge counts `{sheds, agents, hosts}`, `null` before the first report |
| `ui.computed_style` | — | `style`: the dashboard's reported computed style (its `mode` echoes `ui.set_appearance`) |
| `ui.set_appearance` | `mode` (light\|dark) | `{}` |
| `ui.show_window` | — | `{}` (raises the dashboard; same as `app.activate`) |
| `ui.show_create` | — | `{}` (raises the dashboard and opens the Create dialog) |
| `ui.fill_create` | `name`, `host?` | `{}` — test mode only: types into the open New shed sheet's Name (and picks its Host); a sheet with a create under way takes nothing |
| `ui.press_create` | — | `{}` — test mode only: presses the sheet's **Create shed** (disabled, it takes no press) |
| `ui.close_create` | — | `{}` — test mode only: closes the sheet; a create under way runs on (`create.cancel` stops it) |
| `create_sheet.dump` | — | `{sheet}`: what the New shed sheet rendered, `{phase, name, host, state, error, doors}` (`phase` is `form` or `progress`; `doors` is true once its doors' listeners are attached; `error` is the line it shows, an account-service refusal in its fixed words), or `null` while it is closed |
| `ui.show_preferences` (alias `ui.open_preferences`) | — | `{}` — opens or focuses the Preferences window without raising the dashboard |
| `prefs.dump` | — | `{visible, title, prefs}` — the Preferences window's native state plus its reported `{sections, values, mode}` snapshot (`prefs` is `null` before the first open); the sections are General and Terminal |
| `sidebar.dump` | — | the sidebar as rendered: its nav `entries` (`{pane, label}`, in order) and the status foot's `servers` (`null` before the first report) |
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
| `shed.extend` | `host?`, `name`, `hours` (1 to 720) | `{shed, cloud_row}`: a cloud shed with its lease moved `hours` later; a refusal is its API code (`LEASE_AT_CAP`, `EXTEND_NOT_ALLOWED`, …) with fixed words, `action_failed` when the failure has no code. `extend_unknown` when every send ended with no answer or a `5xx`: until a listing settles it (the lease moved by the hours; or unmoved 150 s on; or the shed gone or created again; or 24 h on) every `shed.extend` of that shed is `extend_pending`, unsent, as is one while another is in flight. The shed is read first (`GET /api/sheds/{name}`); a read that fails sends nothing |
| `cloud.usage` | `host` | `{host, usage, words}`: the server's cloud machines against the account service's limits (`usage` is `{person, org}`, `person` null for a caller who is not a person of the service); a server without cloud sheds answers `FEATURE_UNSUPPORTED` |
| `ui.press_shed_details` | `server`, `shed` | `{}` — test mode only: presses the row's own details toggle (its `details` chip, or its delivery badge), opening or closing its detail; a cloud row's detail is the cloud one (`dashboard.dump`'s `cloud.detail`), with no egress section. `ui.show_shed_delivery` / `ui.close_shed_delivery` open and close the same detail |
| `ui.press_shed_delete` | `server`, `shed` | `{}` — test mode only: presses the row's own **Delete** (a missing button takes no press). A cloud row asks first (`cloud.delete_confirm`); any other row deletes at once, as before |
| `ui.press_delete_confirm` | `button` (`delete` \| `cancel`) | `{}` — test mode only: presses that button of the open delete confirmation |
| `ui.press_extend` | `server`, `shed`, `hours` | `{}` — test mode only: presses the open cloud detail's preset for `hours` (a preset it does not offer, or a disabled one, takes no press); the extend runs `extend_shed` and the rows are listed again after it succeeds |
| `ui.fill_extend_hours` | `server`, `shed`, `value` (a string) | `{}` — test mode only: types `value` into the open cloud detail's custom hours field |
| `ui.press_extend_custom` | `server`, `shed` | `{}` — test mode only: presses the custom field's **Extend**, which is disabled unless the field holds whole hours from 1 to the row's `extend_max_h` |
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
grok, claude, antigravity and native sessions). Its rows ride `rc.list` beside roost's, stamped
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
| `dashboard.dump` | on the Sheds pane | `{rows, host_errors, empty}` — each row is the shed as shed-core decoded it (`status` is `running`, `stopped`, `starting`, `stopping`, `error`, or `unknown` for a status the app does not know; `status_reason` the server's reason code or `null`), plus what its card rendered, read off the row: `status_text` (the status, and the reason in parentheses as `shed list` shows it: `stopped (capacity)`) and `controls` (`{spinner, buttons}`: whether the busy spinner shows, and the titles of the action buttons present — a `starting` or `stopping` shed shows the spinner and none). Both are `null` when the card is not mounted. Rows of one server that share a name take their cards in order, and are `null` when the number of rows and cards differs, so a row never reports another row's card. Each row also carries `tag` (the tag's text: the runtime, or `Cloud`) and `tag_tint` (`vz`, `fc` or `cloud`), `meta` (the meta line's whole text: `2 vCPU · 8 GB · running · deleted in 5h 10m` on a cloud row) and `lease` (a cloud row's lease words as drawn, `{text, state, amber}`, `amber` true under its warning time; `null` on any other row). `cloud` is what the pane's cloud parts rendered (plan 041), `null` off the pane: `detail` (the open cloud detail: `{server, shed, facts: {image, created, run, lease_end, cap}, extend, result, unresolved, rendered, doors}` (`unresolved` the row's line while an extend's outcome is not known, else `null`; `doors` true once its press listeners are attached), `extend` `null` when the row offers none, else `{presets: [{hours, label, disabled}], custom: {value, max, disabled}, busy}`, `result` the last extend's `{ok, words}`: a refusal's fixed words (an extend, its busy state and its result are held by the app per shed incarnation, so a reopened detail shows one still running and takes no second press); `null` when no cloud detail is open), `usage` (the usage line under each cloud server's heading, `[{server, words}]`; a server whose usage cannot be read has none) and `delete_confirm` (`{server, shed, title, words, buttons}` while Delete asks, else `null`; it closes, and its Delete sends nothing, once the shed's current row no longer offers Delete or the shed is no longer listed). A cloud row offers no Restart |
| `agents.dump` | on the Agents pane | `{sessions, empty}` — `empty` is the rendered empty state (`{state, title, body, action}`), `null` when rows rendered |
| `egress.profiles` | on the System pane | `{egress}` — every server's egress profiles as rendered, read off the pane's nodes: `{profiles, errors}`, each profile `{host, name, source, mode, allow, deny, rule}` (`rule` is the CEL expression as shown, `null` when the profile has none) and each server that could not list them `{host, error}` |
| `activity.dump` | on the Activity pane; **test mode only** (`not_enabled` otherwise) | `{activity}` — what the pane rendered: `state` (`loading` \| `ready` \| `failed`), `events` in order, each `{id ("<server>/<event id>"), at, server, kind, shed, actor, outcome}` as shown (`outcome` is the result, or a delivery's state in words), `errors` (each server that could not be read, as shown), `failure`, `older` (the Load older button `{disabled, text}`, `null` when nothing older is offered — it is offered whenever a server has a continuation, an empty page included) and `empty`. Pages are merged into one newest-first list, so a server that recovers on a later page has its newer events placed above older rows |
| `shed_egress.dump` | while a shed's detail is open; **test mode only** | `{detail}` — its egress section as rendered: `{server, shed, created_at, profiles, blocks, note, error, rendered}`, `blocks` newest first, each `{target (host:port), reason, at}`; `null` when no detail is open |
| `sidebar.dump` | **always** | `{servers}` — the sidebar's status foot; each server is `{name, reachable, detail}` |
| `roost_consent.dump` | while the roost bootstrap consent dialog is open | `{consent}` — the dialog's rendered copy, `null` when none is mounted |
| `toast.dump` | **always** | `{toast}` — the toast on screen, `null` when none is |
| `shed_roost.dump` | on the Sheds pane | `{sheds}` — each RUNNING shed's row by its `roost:<server>/<shed>` target: its roost line (`status`, `button`, `busy`, `error`, `rendered`), and its craze: `craze_create` (whether the row shows New craze session) and `craze_note` (the too-old note it shows, else `null`) — read off the row |
| `craze_create.dump` | while the craze create sheet is open | `{craze_create}` — what the sheet rendered: `target` (`roost:<server>/<shed>`); `state` (`loading` \| `failed` (the options) \| `idle` \| `submitting` \| `refused` \| `unknown` \| `created`); `providers[]` — `{id, label, state, dimmed, selected, reason, fix}`, a non-ready one dimmed and never selectable — except the provider of a kept request id being replayed, which stays `selected` whatever its state (one the re-read no longer lists is a row of its own, `state: "absent"`); `preselected` (the default provider when listed and ready, else the first ready one) and `default_provider`; `recent_dirs`; `values` (the typed "Directory", "Name" and "First prompt"); `title_field` (whether "Name (optional)" is shown: the shed's live hub takes a title at create, or one is typed) and `title_problem` (a refusal about the title, shown under it — craze's `params.title …` `bad_request`); the `request_id` it holds (only while a submission is in flight or its outcome is unknown); its `note` (offline / too old / not installed; else, while a kept request id is replayed, the unknown outcome and "Changing the form starts a new request instead."; else "no provider is ready on this machine"); the `error` as shown (`{code, message, where, text}`), a start failure's `cause` verbatim, the directory's own `cwd_problem`; `create_enabled` and the `primary` button's label (Create, Try again, Creating…; empty in the Resume half); `modes` — `{offered, enabled}`: whether the `New \| Resume` control is shown (the options say `reopen`) and usable (not while a create submits or a reopen runs); `mode` (`new` \| `resume`, the half on screen); and `saved` — the Resume half as rendered, `null` while New shows: `{state, error, more, gone, rows[]}`, `state` the list's read (`loading` \| `failed` \| `ready`), `error` a failed read's words, `more` "Showing the 50 most recent." when craze left some out, `gone` the note a dropped row left (its title and craze's words), and each row `{id, title, provider, workspace, updated, state, error, button, enabled}` — `workspace` as shown (the home shortened to `~` where known), `updated` its age as shown, `state` `idle` \| `resuming` \| `refused` \| `unavailable`, `error` its refusal as shown, `button` Resume / "Resuming… (this can take a few minutes)" / Retry — read off the mounted DOM; `null` when none is mounted, and until the sheet's driver doors (`ui.fill_craze_create`, `ui.submit_craze_create`, `ui.choose_craze_mode`, `ui.resume_saved`) are listening — so a sheet that has reported hears a door sent next |
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
failure's `summary` (empty for a healthy host).

## Source, delivery and egress ops

These drive this machine's part as a credential source (the delivery stream, plan 039), the
provisioning settings, and the egress decision feed.

| op | params | result |
|----|--------|--------|
| `source.status` | — | `{rows, this_machine, this_machine_error}` — one row per server the shed config lists now (re-read on each call, so a server added while the app runs is there), `{host, supported, source, this_machine, connected, busy, unreachable, error}` (`source` is the machine that answers credential requests for you there, `{id, name}` or `null`; a server older than credential sources — a plain `404` — is `supported: false` with no error; `busy: true` means the server answered `503 SOURCE_BUSY` on every one of a few retries, so the source is unknown for now rather than absent — read again later; `unreachable: true` (with `supported: false` and `error` set) means the server could not be reached or its answer read — a transport failure, an undecodable body, a 5xx with no API error body — so whether it supports sources is unknown, not "no": keep what was last known and read again later; `error` is the fixed words of the error's class, the same as `sources.list`'s `error_message`, never the server's text or a decode error's); `this_machine` is this machine's `{id, name}`, or `null` with `this_machine_error` saying why |
| `source.use` | `host` | `{row, woke_source}` — resolves `host` again once it holds that server's lock (a server removed while it waited is `bad_request`), reloads the source runtime's servers from the config as it reads now (so the resumed stream and the move go to the same, current endpoint), then makes this machine the credential source on `host` (one request, never retried) and reopens this machine's delivery stream to `host` so it connects at once (`woke_source`: a stream was reopened). Refusals: `bad_request` (unknown server), `config_unreadable` (the shed config cannot be read), `machine_identity_unavailable`, `source_machine_not_found` (the server has not seen this machine's delivery stream yet), `forbidden`, `unsupported`, `action_failed` |
| `ui.press_source_use` | `host` | `{}` — test mode only: presses the Credentials view's own **Answer from here** button for `host` (a disabled or missing button takes no press). It is offered where another machine or nobody answers, and where this machine is bound and yet stopped (a stop whose outcome was uncertain), where it undoes the pause |
| `sources.list` | — | `{servers, this_machine, this_machine_error, paused_error}` — one row per server the shed config lists now (re-read on each call: a server added while the app runs is listed, and a changed endpoint is asked at its new address; `source.use` and `source.unbind` take the same servers), `{server, kind, machine, connected, url, state, stream, paused, busy, unreachable, error, error_message}`. `kind` says who answers: `this_machine`, `other_machine` (`machine` is its `{id, name}`, `null` otherwise), `service` (the account service: `url`, and `state` is `not_delivering` when it does not deliver here), `nobody`, `unsupported` (a server older than credential sources) or `unknown` (the server could not say: `unreachable`, `busy` or a refusal). `paused`: this machine stopped answering there and holds no stream. `error` is a class (`unreachable`, `busy`, `unauthorized`, `forbidden`, `source_is_service`, `refused`, ...) and `error_message` its fixed words, never the server's. `paused_error` says what is wrong with `paused-servers.json`, or `null`: `unreadable`, `unparseable` or `unknown_version` (the file cannot be used, so every server is paused until a write), or `write_failed` (a change could not be saved: the set in use still holds, and this machine keeps answering wherever it has not stopped). `config_error` is `unreadable` while the shed config cannot be read (refused whole, or unreadable): the rows are then the servers it last listed, never dropped, and `source.use` and `source.unbind` are refused `config_unreadable` with nothing sent; `null` otherwise (a missing or empty config is no servers, not an error). A server's client is kept only while its whole config entry is unchanged (endpoint, ports, TLS pin, auth mode, certificate, user, identity file). Every name and URL is cleaned for display |
| `source.unbind` | `host` | `{server, unbound, previous, previous_was_this_machine, forgotten_machines, paused, verified}` — "Stop answering from here": resolves `host` again once it holds that server's lock (a server removed while it waited is `bad_request`), pauses this machine on `host` (written to `paused-servers.json` first), waits for that server's stream to end, sends the unbind, reads the binding once more and unbinds again if a late admission re-bound this machine. The pause stays on any uncertain outcome, and lasts until `source.use` lifts it, even if the server leaves the config and comes back. Where the account service answers it is refused `source_is_service` before anything changes. After the pause it reads the binding again and sends the unbind only when that read names this machine. Where another of your machines answers, this machine is paused there and no unbind is sent (that machine's binding is never cleared): the answer is `unbound: false`, `previous` that machine, `another_machine_answers: true`. Where nobody is bound it is `unbound: false`, `verified: true`; where the read said nothing (it failed, or the server was busy) the pause stays, nothing is sent, and the answer is `unbound: false`, `unverified: true`. Every unbind it sends (the first and the re-check's) names this machine's id, so a server with that check (v0.12 or newer) clears the binding only while it still names this machine: another machine that took over between the last read and the unbind is refused by the server, and the answer is `source_moved` (this machine stays paused there, no further unbind is sent, the other machine stays the source), or `moved_after_unbind` when it happened at the re-check's unbind (the first unbind did clear this machine). A server older than that check (v0.11) refuses the field before changing anything and is sent the plain unbind, which clears whichever machine is bound by then. Unbinds and moves on one server run one at a time. Refusals are codes from a closed list with fixed words (`bad_request` for an unknown server, `stream_not_stopped`, `rebound`, `source_moved`, `moved_after_unbind`, ...) |
| `ui.press_source_unbind` | `host` | `{}` — test mode only: presses the Credentials view's own **Stop answering from here** for `host` (a missing or disabled button takes no press) |
| `delivery.describe` | — | `{providers}` — each provider's descriptor, in the order they are asked: title, summary, where its items land and its fields (what the Credentials view draws its forms from) |
| `delivery.recent` | — | `{recent}` — the last 32 answers this machine gave, newest first, redacted as in `delivery.status`'s `recent` |
| `credentials.dump` | — | `{credentials}` — test mode only: what the Credentials view rendered (Where, What, Check, Recent deliveries), read off its nodes; `null` off the view. A passphrase command never appears |
| `ui.fill_check` | `field` (`by`, `shed`, `server`, `backend`, `repo`), `value` | `{}` — test mode only: fills the Check form's own input |
| `ui.press_check` | `button` (`run`, `details`) | `{}` — test mode only: presses Check's own button; `run` re-reads the settings first and runs nothing if the commands it would run (`gh`, the passphrase command) differ from the warning shown |
| `delivery.status` | — | `{machine, machine_error, key_error, config_error, settings_path, servers, recent}` — the source runtime: this machine's `{id, name}` (or why there is none), each configured server's delivery stream `{server, state, since, last_error, streams, requests, unreadable, answered, refused, dropped}` (`state` is `connecting`, `connected`, `not_bound`, `duplicate`, `service` (the account service answers there: no stream is held, and the server is asked again on a timer), `offline` or `stopped`; `last_error` is a class such as `transport`, `unauthorized`, `not_bound`, `rate_limited`, `unsupported`, `config`, never a server's words), and the last answers, newest first, redacted: `{server, request, shed, reason, at, items, problems, outcome}` (`items` hold names, targets, sizes and fingerprints, never a value) |
| `delivery.settings` | — | `{path, exists, error, providers}` — `delivery.toml`: each of `ssh-key`, `github`, `api-keys`, `envsecrets-login`, `files` as `{name, configured, enabled, only, config}`, where `config` is the provider's table as flat JSON (the shape `delivery.set` takes); `error` says where an unreadable file is wrong, never quoting it. A stored `passphrase_command` reads `"set"`, never its arguments (they may hold the passphrase) |
| `delivery.set` | `provider`, `config` (object, or `null` to remove the provider), optional `base` | the new `delivery.settings` — replaces one provider's table and writes the whole file back (mode `0600`, atomically). With `base` (the provider's `config` as `delivery.settings` showed it when the edit began, `null` for a provider not set up) the write is a compare-and-swap: inside the same lock the provider's table as it is now, in that shape, must equal `base`, or it is refused `conflict` and nothing is written (other providers' tables are not compared; a stored `passphrase_command` compares as `"set"`). Without `base` the table is replaced whatever it holds. The Credentials view's Save sends `base`, and on `conflict` discards the form and shows the settings as they are now, with the discard notice. A change the schema refuses is `bad_request` with the key path and line, never the value, and the file is left as it was; a file that cannot be read now is `settings_unreadable`. `passphrase_command: "set"` keeps the stored command exactly (refused when none is stored); an array replaces it; leaving the key out removes it |
| `delivery.preview` | `server`, `shed?`, `backend?`, `repo?`, `runs?` | `{server, shed, request?, preview}` — what `shed` on `server` would be delivered, from its document (`request` is the backend, repo, local and project directories the server would send); with a `backend` (and optionally a `repo`) instead of a shed, the request a new shed of that kind would draw (what Check sends for a shed not yet created); with neither, what the providers have at all. A shed together with a backend, or a repo without a backend, is `bad_request`. `runs` is the list of commands the person was told would run: a preview whose settings would run others runs nothing and answers `settings_changed`; `preview` is `{items, problems, providers}` with each item's name, provider, kind, stage, base, target, mode, size and fingerprint — never a value. `bad_request` for an unknown server, a bad shed name or a bad target |
| `shed.delivery` | `server`, `shed` | `{server, shed, delivery, egress_profiles, egress, egress_error}` — the shed document's delivery record (`null` from a server older than delivery), its explicit egress selection (`null` when it names none and takes the server's default), and its effective egress from `GET /api/egress/{shed}`: `{enabled, restricted, profiles, inherited, mode}`, where `profiles` are the profiles in force, `inherited` says they are the server's `egress.default`, and `mode` is `audit`, `enforce` or `null` (unknown); `egress` is `null` with `egress_error` set when that read failed. `not_found` for no such shed |
| `shed.provision` | `server`, `shed` | `{server, shed, ok, progress, delivery, error}` — runs `POST /api/sheds/{shed}/provision` (one request) and reports its progress lines `{message, warning}`, the record it ended with, or the server's error `{status, code, message}` |
| `activity.server_list` | `limit?` (1 to 500, default 100), `cursor?`, `shed?` | `{events, next, errors}` — `GET /api/activity` of every configured server, merged newest first, each event with its `server`; pass `next` back as `cursor` for the page after (`null`: nothing older anywhere); a server that could not be read is in `errors` and keeps its place for the next page |
| `egress.recent` | `server`, `shed`, `created_at`, `blocks?` | `{server, shed, created_at, events}` — that shed **incarnation**'s last egress decisions (at most 50, kept in memory since the app connected), newest first, each `{server, ts, shed, shed_created_at, host, port, resolved_ip, protocol, verdict, reason}` (`verdict` is `allow` or `deny`; every string cleaned for display). `created_at` is required: the shed's own creation time, from its row (`sheds.list`'s `created_at`), compared as an instant; without one it is `bad_request`. Decisions are kept per incarnation (server, name, creation time, which the server stamps on each decision as `shed_created_at`), so a shed deleted and created again under its name starts empty; a decision that names no incarnation is dropped (counted in `egress.status`'s `unattributed`). `blocks: true` keeps only the `deny` decisions, a shed's recent blocks. A listing (`sheds.list`, `sheds.refresh`, the dashboard's own refresh) drops the incarnations it no longer has. A kept decision emits the `egress-changed` event `{server}` |
| `egress.status` | — | `{servers}` — each configured server's `GET /api/egress/stream` consumer, `{server, state, error, connects, decisions, unattributed}`; `state` is `connecting`, `connected`, `waiting` (reconnecting: 1 s doubling to 30 s, reset once a stream held for 60 s), `unavailable` (`501` or `404`: egress control is not enabled there; asked again after 5 minutes) or `stopped` |
| `egress.server_profiles` | — | `{hosts}` — each configured server's egress profiles (`GET /api/egress/profiles`), or the error it returned: the rows the `egress_profiles` command gives the System pane |
| `ui.press_delivery_button` | `provider`, `button` | `{}` — test mode only: presses a control of `provider`'s row in the Credentials view: its actions `setup`, `edit`, `turn_on`, `turn_off`, `remove` (a provider that is not set up offers only `setup`), and in its open form `save`, `cancel`, the scope's `scope:every`, `scope:only`, `server:<name>`, `backend:<name>` (a file entry's own scope prefixed `file.<i>.`, as `file.0.scope:only` or `file.0.server:mock`), `add:<list>`, `remove:<list>:<i>` (lists are `env_files`, `passphrase_command`, `only.repos`, a file entry's `file.<i>.only.repos`, and `file`, the file entries themselves: `add:file`, `remove:file:<i>`), `toggle:<switch>` (`sign_commits`, `craze_providers`), and for a stored passphrase command `replace:passphrase_command`, `clear:passphrase_command`, `keep:passphrase_command`. Save writes the provider's table with `delivery.set`; a provider cannot be saved on while a required field is empty, and a refusal is shown under the row in fixed words |
| `ui.type_delivery_field` | `provider`, `field`, `index?`, `value` | `{}` — test mode only: types `value` into the open editor's real input (its value set, then its own input event): `key_file`, `gh`, `passphrase_file`, an entry of a list (`env_files`, `passphrase_command`, `only.<key>`) by `index`, a file entry's `file.from` / `file.to` / `file.base` / `file.mode` by `index` (`file.mode` is `""` for the default `0600`, `0600` or `0400`), or a file entry's repo pattern, `file.<i>.only.repos`, by `index`. Every string is kept exactly; nothing is split or trimmed |
| `ui.show_shed_delivery` | `server`, `shed` | `{}` — raises the dashboard, switches to Sheds and opens that row's detail (what pressing its delivery badge, or its details toggle when it has no record, does): its delivery when it has a record, and its egress |
| `egress.show` | `server`, `shed` | `{}` — the same as `ui.show_shed_delivery`: a shed's egress lives on its detail (the profiles assigned to it and its recent blocks, read back with `shed_egress.dump`) |
| `ui.close_shed_delivery` | — | `{}` — closes the open shed detail |
| `ui.scroll_to` | `anchor` | `{}` — test mode only: scrolls the dashboard to the element whose `data-scroll-anchor` is `anchor` (`shed:<server>/<shed>` for a shed's row, `host:<server>` for a server's group of rows on the Sheds pane, `system-egress` for the System pane's profiles, `activity` for the Activity pane), so a screenshot can show what is below the fold; answers `{token}`, which `ui.scrolled` (also test mode only) reports back as `{anchor, token, found, animations}` once the dashboard has scrolled (`animations`: how many finite CSS animations were still running, a busy row's endless spinner not counted, so a screenshot can wait for zero) |
| `ui.press_activity_older` | — | `{}` — test mode only: presses the Activity pane's own **Load older** (a missing or disabled button takes no press); read the page back from `activity.dump` |
| `ui.press_deliver_again` | `server`, `shed` | `{}` — test mode only: presses the open detail's own **Deliver again** (refused, as a pointer is, while the shed is not running or a delivery runs), which runs `shed.provision` |
| `shed_delivery.dump` | — | `{detail}` — what the open delivery detail rendered, read off its nodes: `{server, shed, state, withheld, summary, steps, last, failure, problems, items, deliver_again, result, warnings, progress, rendered, doors}` (`doors` is true once its press listener is attached: a `ui.press_deliver_again` sent before then reaches no one; `items` are `{name, provider, stage, target, result, written, expires}` as shown), or `null` when none is open. `withheld` is the server's own mark on a record it stripped for a caller who may not enter the shed (an admin looking at another user's): the detail then shows the state alone and no Deliver again. A Deliver again run is held by the app per shed until it ends, so closing the detail or leaving the pane neither loses it nor lets a second one start. Each `dashboard.dump` row also carries its `delivery` record and `delivery_badge`, the badge's text (`null` without one) |

## Test mode

When launched with `SHED_TAURI_TEST_MODE=1`, `identify` reports `test_mode: true` and the
`mock_base_url` the app's HTTP clients were redirected to (`SHED_TAURI_MOCK_BASE_URL`), so the
harness can confirm a run is hermetic before asserting anything. The URL is one mock for every
server, or a default followed by `,<server>=<url>` overrides
(`http://127.0.0.1:4001,mock2=http://127.0.0.1:4002`), so a harness can drive two servers that
answer differently. Test mode also unlocks the
fault-injection and driver ops marked "test mode only" above (`rc.inject_test`, `app.quit`,
the `ui.fill_*`/`ui.submit_*` doors, and so on), disables `terminal.open` and Sparkle, and never reads the developer's own `~/.shed/config.yaml`: an
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
