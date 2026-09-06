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

## Remote control

| op | params | result |
|----|--------|--------|
| `rc.classify` | `kind`, `pane` | `state`, `url?` (pure pane classifier) |
| `rc.list` | `host?`, `shed?` | `sessions[]` |
| `rc.launch` | `host?`, `shed`, `kind?`, `display_name?`, `workdir?`, `initial_prompt?` | the launched `RcSession` |
| `rc.kill` | `host?`, `shed`, `slug` | `{}` |
| `machines.list` | — | `machines[]` — every configured machine's health, name-ordered |
| `machine.kill` | `machine`, `slug` | `{}` (addressed by machine + slug, not host/shed) |
| `rc.inject_test` | `shed`, `slug`, `kind?`, `state?`, `managed?`, `display_name?`, `created_by?`, … | `{}` — **test mode only**; injects a session (e.g. a legacy row) into the table |

`initial_prompt` is an optional one-line kickoff delivered once the session is ready (an
initial prompt for `claude-rc`, an initial command for `shell`). Leading/trailing whitespace
(including newlines) is trimmed, and a blank value sends nothing. After trimming, an embedded
control character, a value over 2000 UTF-8 bytes, or any prompt for a kind that doesn't accept
typed input (`claude-broker`) is rejected with `invalid-param`. (Mirrors shed-remote-agent's
create-request normalization.)

Each `RcSession` carries the [RC Session Convention v2](rc-sessions.md) metadata:
`managed`, and (when managed) `rc_id`, `created_by`, `created_at`, `target_label`.
A legacy/unmanaged `rc-*` session decodes with `managed: false` and no metadata.

### Machines (Tauri)

A **machine** is a native host you reach over SSH that runs the RC activity hub on its
loopback `1029` — no shed server in the path, no TLS pin, no control token. Machines come
from the `machines:` section of `~/.shed/config.yaml` (the same section `sx` reads) and are
read ONCE at startup, so there is no in-app add/edit; an editor that silently needed a
relaunch would be worse than the file.

Machine sessions appear in `rc.list` beside shed sessions, each stamped with
`origin_kind: "machine"` and `origin: "machine:<name>"`. **Row keys and grouping must use
`origin`, never `shed`** — a hub reports an EMPTY shed on every session it serves, so two
machines sharing a slug would collide into one row. `machines.list` is separate from
`rc.list` because a machine is worth showing even when it has no sessions and cannot be
reached: that row IS the information ("mini3 is asleep"), and a sessions-only payload has
nowhere to put it. Each entry carries `{name, origin, reachable, connected_once, sessions,
detail?}`, where `detail` is why it is unreachable — verbatim, because "no route to host"
and "nothing is listening on 1029" need different fixes.

Unreachable is a first-class state, not an error: a machine that is asleep, off-network, or
simply not running a hub is the everyday case, and it must never fail the sessions view.

### UI-truth ops (Tauri)

These report what the frontend RENDERED, so a test can assert the window rather than the
backend's view — the two can disagree, and have.

| op | answers | result |
|----|---------|--------|
| `dashboard.dump` | on the Sheds pane | `{rows, host_errors, empty}` |
| `agents.dump` | on the Agents pane | `{sessions}` |
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
