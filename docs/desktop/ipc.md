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
| `ui.state` | — | `pane`, `hosts[]`, `sheds[]`, `host_agent_connected`, `last_error?` |
| `ui.navigate` | `pane` (sheds\|approvals\|agents\|activity\|system) | `pane` |
| `ui.set_ssh_approval` | `method?`, `scope?`, `ttl?` | `{}` (applies SSH approval prefs + resets live SSH grants) |
| `ui.show_window` | — | `{}` |
| `ui.hide_window` | — | `{}` (closes the dashboard → menu-bar-only accessory) |
| `ui.window_state` | — | `visible` (bool), `activation_policy` (regular\|accessory) |
| `ui.open_preferences` | — | `{}` |
| `ui.open_menu` | `open` (bool) | `open` |
| `host.list` | — | `hosts[]` |
| `sheds.list` | `host?` | `sheds[]` |
| `sheds.refresh` | — | `{}` (forces an immediate poll) |
| `system.df` | — | `usage[]` (per-host `GET /api/system/df`: totals + image/shed/orphan disk entries) |
| `app.window_metrics` | — | `window_width`, `window_height`, `sidebar_width`, `visible_pane` |
| `app.screenshot` | `surface` (window\|menu), `scale` (1\|2) | `png` (base64), `width`, `height`, `scale`, `surface` |

The screenshot renders the target window's content view to a PNG in-process — no screen
capture permission, works even when the window is occluded or off-screen. Capturing the
menu requires it to be open first (`ui.open_menu {open:true}`).

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
