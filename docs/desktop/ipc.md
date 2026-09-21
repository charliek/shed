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
| `machines.list` | — | `machines[]` — every configured machine's health, name-ordered |
| `machine.kill` | `machine`, `slug` | `{}` (addressed by machine + slug, not host/shed) |
| `rc.inject_test` | `shed`, `slug`, `kind?`, `display_name?`, `workdir?`, `lifecycle?`, `attention?` | `{}` — **test mode only**; puts a row into that shed's roost snapshot. `slug` must parse as a roost tab id, and `kind` must be one roost has an adapter for (anything else would be an unowned tab, which a real snapshot never lists) |
| `roost.probe` | `target` | `target`, `probe` (its `fingerprint` nested inside) and `plan` — a read-only look at a shed or machine's `roost-session` state. The plan matrix row comes back from here too, so a caller that needs only the row does not also have to call `roost.preview` |
| `roost.preview` | `target` | the plan (Install/Update/Start/Report/nothing to do) plus the sentence naming where the bytes would come from |
| `roost.bootstrap` | `target`, `fingerprint`, `consent: true` | installs/updates/starts `roost-session` on that target and wires its agent hooks; refuses without consent or against a stale fingerprint |
| `roost.launch` | `target`, `kind?`, `workdir?`, … | generalizes `machine.launch` to any roost host (a shed or a machine); `machine.launch` stays as an alias |

The four ops above are how the desktop drives [putting `roost-session` on a shed or
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

### UI-truth ops (Tauri)

These report what the frontend RENDERED, so a test can assert the window rather than the
backend's view — the two can disagree, and have.

| op | answers | result |
|----|---------|--------|
| `dashboard.dump` | on the Sheds pane | `{rows, host_errors, empty}` |
| `agents.dump` | on the Agents pane | `{sessions, empty}` — `empty` is the rendered empty state (`{state, title, body, action}`), `null` when rows rendered. `state` is `loading` \| `failed` \| `unreachable` \| `empty`: four blanks wearing one screen, and only `empty` offers the bootstrap |
| `launch.dump` | while the New-session dialog is open | `{launch}` — the dialog's own rendered text (`{rendered}`), `null` when none is mounted |
| `egress.profiles` | on the Egress pane | `{egress}` |
| `machines.dump` | on the Machines pane | `{machines}` — a row per machine with its `status` word, `detail` line, and grouped session slugs |
| `sidebar.dump` | **always** | `{servers, machines}` — the sidebar's status foot |

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

Two launch-time overrides exist only in test mode, both taking comma-separated server
names: `SHED_DESKTOP_MOCK_UNREACHABLE_HOSTS` points a host at a closed port (a
deterministic per-host failure), and `SHED_DESKTOP_MOCK_CREDENTIAL_HOSTS` keeps a host's
REAL control-credential wiring — the host agent plus the config's `auth_mode` — against
the mock, so the mint → refusal → banner chain is drivable end to end.
