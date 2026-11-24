---
name: shedtest-linux
description: Validate the shed desktop app's Tauri Linux client from a Mac (or Linux) dev machine — the WebKitGTK render gate, the Tauri crate's Linux clippy and tests, the nfpm .deb build + clean-container install validation, and driving the app headlessly over its IPC socket. Use when asked to test the Tauri/Linux client, verify a shared/Linux desktop change, debug a WebKitGTK render or .deb failure, or run the Docker legs. The app lives under `desktop/`; the shared Rust core is the sibling `crates/` workspace. The macOS app loop is the shedtest-mac skill.
---

# Tauri Linux client end-to-end (from a Mac dev box)

The shipped **Linux** client is the **Tauri** app (`desktop/tauri/`, React/Vite/Tailwind on
WebKitGTK), a thin shell over the shared Rust core in `crates/`. Its WebKitGTK toolchain isn't
on macOS, so the Linux legs run **in Docker** (or natively on a Linux host) — the same north
star as the macOS loop applies:
verify by driving the real app over its IPC socket, not by clicking. Run targets with
`make -C desktop <target>` (or the root `make desktop-<target>` passthrough). Docker must be
running.

## The fast path

```bash
make -C desktop tauri-test-linux    # the Tauri crate's clippy + cargo test on Linux (its Linux cfg; Docker, no display)
make -C desktop tauri-build-linux   # the render gate: the e2e on ubuntu:24.04 / WebKitGTK 2.44 under Xvfb (Docker)
make -C desktop core-linux          # shared crates: test + clippy on Linux (Docker)
```

- **`tauri-build-linux` is the render gate and the one to trust for any shared/Linux change.**
  It builds the Tauri Rust app and runs the pytest suite on the **real shipped
  WebView** (WebKitGTK 2.44) under Xvfb — the render smoke *is* the CSS gate (2.44 supports
  oklch/color-mix/`:has()`/`@container`, so a static denylist would be miscalibrated). The macOS
  WKWebView e2e alone can miss Linux-only breaks.
- **`tauri-test-linux`** runs the Tauri crate's clippy and unit tests on Linux, so the
  `cfg(target_os = "linux")` code compiles and is linted there. No display needed. (The polkit
  approval gate and its seam tests were removed in plan 039 with the approvals.)
- Both reuse the `shed-tauri-linux` image (built from `desktop/Dockerfile.tauri-linux`).

### The native inner loop (a Linux host with the WebKitGTK dev stack)

Docker is the **gate**; it is a poor iteration loop (image build + tar + cold cargo cache per
run). On a Linux box that can already build the crate natively, drive the suite directly and
keep Docker for the final check:

```bash
cd desktop/tauri/ui && npm ci && npm run build      # ONCE, and after any tauri/ui change
cd ../src-tauri && cargo build                      # AFTER EVERY change, Rust OR UI — see below
cd ../..                                            # desktop/
SHED_TAURI_BIN=$PWD/tauri/src-tauri/target/debug/shed-desktop-tauri \
WEBKIT_DISABLE_DMABUF_RENDERER=1 \
xvfb-run -a --server-args="-screen 0 1400x900x24" \
  uv run --group test pytest tools/shedtest -q -p no:cacheprovider
```

Three traps, all of which cost real time and none of which produces a useful error:

- **The harness runs `SHED_TAURI_BIN`, not `cargo`.** A Rust change you did not `cargo build`
  is simply not under test, and the run passes (or fails) on the OLD binary. `cargo test --lib`
  builds a *different* artifact and does not refresh it. Rebuild before every pytest run.
- **`cargo build` is also how a UI change reaches the app.** `generate_context!` **embeds**
  `tauri/ui/dist` INTO the binary, so `npm run build` (or `make tauri-ui-build`) alone changes
  nothing the harness runs — the app keeps serving whatever bundle was embedded at the last
  `cargo build`. There is no error and no warning; the app renders the old UI perfectly, which
  is exactly what makes it expensive. Symptom to recognise: an assertion about the new UI fails
  against markup you can see is no longer in the source, or a screenshot shows the previous
  layout. **After ANY `tauri/ui/**` edit: `npm run build` THEN `cargo build`, in that order.**
  (The `make` targets get this right — `tauri-build` depends on `tauri-ui-build` — so the trap
  is specific to driving the pytest run against a hand-built binary.)
- **A `tauri/ui/dist` that exists is not a `dist` that works.** `generate_context!` only needs
  the directory, so a placeholder `index.html` (a one-line `probe` stub is a real thing to find
  there) compiles and links fine — and then the WebView mounts nothing, `ui.current_pane()`
  stays `None`, and EVERY tauri cell dies at `timed out … waiting for tauri frontend ready`
  with nothing in the app log. `ls tauri/ui/dist` should show `assets/`, `index.html`,
  `popover.html`, `preferences.html`; if it does not, `npm ci && npm run build`.

## The .deb

```bash
make -C desktop deb                       # build shed-desktop_<ver>_<arch>.deb → desktop/out/ (DEB_VERSION=x)
make -C desktop deb-validate              # build + install-validate the .deb in a clean ubuntu:24.04 container
make -C desktop deb DEB_VERSION=0.8.0     # override the version stamped into the package
```

`deb` builds the Tauri binary via nfpm (`desktop/linux/scripts/build-deb.sh`, bin
`shed-desktop-tauri` → `/usr/bin/shed-desktop`), with a headless `shedctl`
bundled; `deb-validate` installs it in a fresh container and runs
`linux/scripts/validate-deb.sh` against the newest `out/*.deb`.

## Driving the Tauri app over IPC

The Tauri client speaks `{id,op,params}` JSON IPC over
`$XDG_RUNTIME_DIR/shed-tauri.sock` (override `SHED_TAURI_SOCKET`; `/tmp/shed-tauri-<uid>/`
fallback). Hermeticity hooks are `SHED_TAURI_TEST_MODE` / `SHED_TAURI_MOCK_BASE_URL` /
`SHED_TAURI_SHED_CONFIG`; timeouts scale with `SHED_TAURI_TEST_TIMEOUT_SCALE`.

```bash
make -C desktop tauri-run       # build the UI bundle + launch natively
make -C desktop e2e-tauri       # the hermetic pytest harness (needs a display; Xvfb on Linux)
```

- The harness picks a prebuilt binary via **`SHED_TAURI_BIN`** — the render gate points it at
  `/target/debug/shed-desktop-tauri` inside the container so the pytest run drives the binary
  built in the same step. Set `SHED_TAURI_BIN` when driving a binary you built out-of-band.
- `e2e-tauri` runs the ONE `tools/shedtest` harness (there is no `--target` any more; the
  Tauri app is the only client). Natively on a Linux host wrap it in `xvfb-run -a` and pass
  `SHED_CRAZE_BIN_DIR` (from `make craze-binaries`) or the craze cells skip. On Linux the tray is a native menu (Tauri emits no Linux tray-click events → no
  popover; expected).
- **Driving the Preferences window.** Preferences is a dedicated native window (not a
  dashboard modal), so it has its own IPC surface rather than `ui.modal`:
  `ui.show_preferences` (open/focus the lazy-created singleton; **it does NOT raise the
  dashboard**) with `ui.open_preferences` as an alias op; `prefs.dump` →
  `{visible, title, prefs}` (the window's UI truth — `visible`/`title` are Rust-side native
  window state, `prefs` is the React-reported `{sections, values, mode}` snapshot keyed under
  the **`preferences`** window label); `prefs.close` (hide, close-hides contract);
  Note
  `ui.modal` now only ever reports `create` | `launch` | `null` — Preferences is never a
  modal value.
- **Credential sources (plan 035 §3.4, re-hosted on the delivery stream in plan 039 PR B).**
  `source.status` → `{rows, this_machine, this_machine_error}` (one row per configured
  server: `host`, `supported`, `source {id,name}|null`, `this_machine`, `connected`, `error`)
  and `source.use {host}` (makes THIS machine the source; `{row, woke_source}`; refusals come
  back as `bad_request`, `machine_identity_unavailable`, `source_machine_not_found`,
  `forbidden`, `unsupported`, `action_failed`). The Preferences rows live INSIDE the
  Provisioning section (`prefs.dump`'s `values.sources` / `this_machine_name` /
  `this_machine_error` carry them). The button is pressed through `ui.press_source_use
  {host}` (test mode only): the open window clicks its OWN `button[data-source-use="<host>"]`,
  so a disabled button takes no press — the dump's `can_use` and the DOM's `disabled` come
  from one rule. The mock is UNARMED by default (`/api/sources` answers chi's plain 404 = an
  older server); `MockShedServer.arm_source(...)` (or `_source_app(source={...})` in
  `test_tauri_source.py`) makes it pin: another machine's delivery stream gets 403
  SOURCE_NOT_BOUND naming the bound machine, a move needs a machine the mock has SEEN (a
  stream, or `know_machine` for a machine that never connects), and
  `source_use_script([403, 500])` scripts refusals. Like the real server, a move
  (`POST /api/sources/use` or `mock.move_source(id, name)`, another machine taking it) ends
  the old machine's streams, and the old machine's later answers get 403 SOURCE_NOT_BOUND.
  More knobs: `source_busy(n, retry_after)` (next n reads 503 SOURCE_BUSY — the core asks 3
  times per read), `source_reads_late(secs)` (a read that takes its document on arrival and
  answers late — an overtaken read), `delay_source_use(secs)` (two moves in flight at once).
  The mock's own contract is pinned without the app in `test_mockserver_sources.py`. The cells
  are `test_tauri_source.py`'s; the "connected at once" cell waits for four refusals (next
  backoff 8 s, no jitter) and asserts the accepted stream came within `_WAKE_BUDGET_S` of the
  last one (`mock.subscribe_times`): 6 s at scale 1, scaled with
  `SHED_TAURI_TEST_TIMEOUT_SCALE` but capped 0.5 s under the backoff — without the reopen it
  lands at exactly +8 s. Preferences re-reads the rows when a hidden window is shown again
  (the native `prefs-shown` event, plus the DOM `focus`/`visibilitychange`, throttled to
  one read per 5 s); under Xvfb either path alone refreshes a reopen.
- **Provisioning (plan 039 A7).** Preferences → Provisioning holds the five providers'
  switches and editors, the source rows (`data-source-*` nodes, `values.sources`), and a Preview. Read it back from
  `prefs.dump`'s `values.provisioning` (built off the rendered nodes: each provider's
  switch state, `only` line, editor, refusal and notice; `editor`, the open editor's
  committed input values, passphrase command arguments `redacted`; the open draft as its
  `delivery.set` config, redacted the same way; the preview's items and problems;
  `rendered`). Drive it with `ui.prefs_section` (scroll), and in test mode
  `ui.press_delivery_toggle`, `ui.press_delivery_button` (Edit/Save/Cancel, add/remove an
  entry, the command's Replace/Clear/Keep) and `ui.type_delivery_field`, which type into
  and press the REAL controls: wait for `editor.values` to show what was typed before
  pressing Save. A stored passphrase command is never shown: `delivery.settings` says
  `"set"`, and `delivery.set` takes `"set"` back as "unchanged". Each shed row has a delivery badge
  (`dashboard.dump` rows' `delivery_badge`) and a detail opened by `ui.show_shed_delivery`,
  read by `shed_delivery.dump`, pressed by `ui.press_deliver_again` (test mode).
  `test_tauri_provisioning.py` launches its own app over the A6 module's throwaway home;
  `SHED_DELIVERY_SHOTS=<dir>` keeps its screenshots. Two traps: a self-managed launch
  must wait for `ui.current_pane` to be non-null before its first `ui.navigate`
  (`frontend_not_ready` otherwise; `await_hermetic` does not wait for it), and the
  Preferences window re-reads `delivery.toml` only when it is shown again (a `prefs-shown`
  from `ui.show_preferences`), so a cell that rewrites the file shows the window again.
- **Launch at login is on by default** (plan 037 §3.1.4): every hermetic first launch turns
  it on, so the session app reads on (`test_loginitem_probe`). On Linux `auto-launch` writes
  `$HOME/.config/autostart/*.desktop` and ignores `XDG_CONFIG_HOME`, so it lands under the
  throwaway HOME (`ui.linux_autostart_dir`), never yours; `prefs.json` is under
  `XDG_CONFIG_HOME` (`ui.app_config_dir`). `test_login_item_default_once` relaunches over the
  same runtime dir (a self-managed app with `runtime_dir=...`, caller-owned); the unwritable cell makes
  `prefs.json` a DIRECTORY because the Docker gate runs as root, which a read-only directory
  does not stop. A debug build outside the harness never applies the default.
- **The launch environment is the harness's own** (`hermetic.py`). The app is a credential
  source in process, so every launch is made hermetic: HOME, every `XDG_*_HOME`,
  `XDG_RUNTIME_DIR` and `SHED_BROKER_STATE_DIR` inside the runtime dir, no `SSH_AUTH_SOCK`,
  no `AWS_*` file overrides or `DOCKER_CONFIG`, no proxy variables (`NO_PROXY=*`), and every
  mock URL `http://127.0.0.1:<port>`. `hermetic.validate` runs inside `ui.subproc_env`
  before every spawn. There is no broker to configure and no approval to decide: delivery is
  driven by the mock's `/api/delivery/requests` stream (`push_delivery_request`), and a cell
  reads the answer off `delivery.status`. `hermetic.generate_key` makes a throwaway ed25519
  key for a cell that needs one, so the render-gate image installs `openssh-client`.
- **The server views** (`test_tauri_server_views.py`) run over private mocks with
  `hermetic.mock_url(base, per_server)` naming each server's URL, and cover the four sidebar
  entries, Activity merged across servers, a shed's delivery and egress detail and the
  System pane's egress profiles.
- **Updater ops on Linux report `linux_apt`.** `updater.status` → `{os, enabled, reason,
  instantiated}` and `updater.check` exist on both targets (the Sparkle updater is macOS-only;
  Linux ships via apt). On the Linux render gate `updater.status` is
  `{os:"linux", enabled:false, reason:"linux_apt", instantiated:false}` regardless of test
  mode, and `updater.check` returns the deterministic `updater_disabled:linux_apt` error
  without crashing. `test_tauri.py` branches on `platform.system()` and pins this cell.
- **Simulating a down (unreachable) host.** The shared session redirects EVERY configured
  server to the one in-process mock, so a per-host error row can't appear there. To exercise
  it, use a DEDICATED fixture config with an extra server plus the
  `SHED_TAURI_MOCK_UNREACHABLE_HOSTS=<name,...>` override (comma-separated server NAMES, parsed
  only in test mode) — the backend points those at `http://127.0.0.1:1` (deterministic
  ECONNREFUSED) while the rest hit the mock. `test_tauri_downhost.py` is the pattern: it
  launches its OWN throwaway instance (distinct HOME/XDG → distinct socket + single-instance
  lock, so it coexists with the session app) against `fixtures/config-downhost.yaml`. Keep the
  down host OUT of the shared `fixtures/config.yaml` (it would break the m0 golden gate + put an
  error banner in every "healthy" screenshot).

## Driving a shed's roost-session

The desktop lists sheds only. There is no `machines:` host list, no implicit
`localhost` host, and no Machines pane. A shed's roost rows come from that
shed's `roost-session`. The watcher subscribes to roost's observer event stream
and emits a snapshot only when a row actually changed.

| Variable | Where it is read | Effect |
|---|---|---|
| `SHED_TAURI_ROOST_SOCKETS` | `src-tauri/src/env.rs`, **test mode only** | Comma-separated `roost:<server>/<shed>=<socket path>`, one entry per shed (not `localhost`). A mapped shed's `roost-session` is dialled on that Unix socket. A shed that is not in the map still uses `SHED_TAURI_SSH_BIN` when that fake `ssh` is configured, and is unreachable when it is not. `ui.py`'s `subproc_env` sets or CLEARS the var on every hermetic launch. |
| `SHEDTEST_ROOST_SOCKETS` | `conftest.py`'s `_env_roost_sockets`, harness-level | Same `roost:<server>/<shed>=<socket path>` shape, passed as `roost_sockets` to the session app. A custom driver must pass the map to `ui.launch(..., roost_sockets=...)`. |

`rc.inject_test` plants a roost row on a shed (`target` is `roost:<server>/<shed>`).
`machines.list`, `machines.dump`, `machine.*`, `roost.launch`, `roost.run`, `rc.kill`,
`ui.show_launch`, `ui.*_launch` and `launch.dump` answer `unknown_op`. `ui.navigate`
with `"machines"` is `bad_request`.

### Agent lanes

Craze is the only lane, and it runs on a shed over the fake-ssh seam
(`SHED_TAURI_SSH_BIN`). There is no local craze path and no opencode lane.
`test_tauri_craze.py` is the module. `lane.*`, `ui.show_lane` and
`ui.show_lane_settings` take `target` (`roost:<server>/<shed>`) and `session_id`.
There is no `kind` parameter.

**Driving the transcript panel.** `ui.show_lane {target, session_id}` mounts it,
`ui.close_lane` unmounts it, and `lane.dump` answers what it rendered (`null`
once no panel is mounted). The panel calls `lane.open` on mount and `lane.close`
on unmount. `lane.messages` (what the backend staged) and `lane.dump` (what is
on screen) are different questions. `SHED_LANE_SHOTS=<dir>` keeps the panel
cells' screenshots.

### The gx lane (plan 017) — retired

gx and its lane left shed in plan 025 C1 (shed#390); `test_tauri_gx_lane.py` and `fake_gx.py`
are gone with it. Its slot is craze's — next section.

### Craze sessions — the real hub, on a shed

`test_tauri_craze.py` drives the **real** craze hub: the pinned `craze`, `craze-fake-host`,
`craze-fake-agent` and the real `craze-0.0.1`, from `SHED_CRAZE_BIN_DIR`. Without it the module
SKIPS; with **`SHED_CRAZE_REQUIRE=1`** a missing binary FAILS it instead (CI sets both, so CI can
never go green on skipped craze cells).

```bash
# native (a Linux host): build the binaries once, then point the harness at them
make craze-binaries                                  # prints SHED_CRAZE_BIN_DIR=~/.cache/shed/craze-<sha12>
# the Docker legs only see the repo layout, so build INTO it (gitignored) and pass the
# IN-CONTAINER path — desktop/Makefile forwards both variables into `docker run` by name
make craze-binaries OUT=desktop/tools/shedtest/.craze-bin
SHED_CRAZE_REQUIRE=1 SHED_CRAZE_BIN_DIR=/work/desktop/tools/shedtest/.craze-bin make -C desktop tauri-build-linux
```

- **Craze is reached only on a shed, through the fake ssh** (`SHED_TAURI_SSH_BIN`, test mode
  and a debug build). There is no local craze path. `ui.subproc_env` still clears
  `CRAZE_HOME` and `CRAZE_RUNTIME_DIR` so a developer's own values never reach a hermetic
  launch.
- **`CRAZE_RUNTIME_DIR` must be short, 0700 and under `/tmp`** — craze refuses one beneath a
  group-writable ancestor (`~/.cache` is 0775 on some hosts) and a socket path over ~104 bytes.
  The rigs use `tempfile.mkdtemp(prefix="shcz-", dir="/tmp")`.
- **Remote craze in test mode exists only through the fake ssh** (`SHED_TAURI_SSH_BIN`), also
  jailed. The craze fake ssh (`_fake_ssh` in the test file) runs each remote command in the jail
  its destination USER names, records every command (`cmds/<user>.*`), answers roost's
  `client-bridge` with `command not found`, and REFUSES (records `refused`, exit 255) any craze
  command naming an absolute ladder rung — so the jailed composition is asserted without the
  production ladder ever running here. The jail's `bin` holds `sh` beside `craze`: the remote
  command is `sh -c '…'`.
- **Remote is attach-only, so a dormant machine never sees `bridge --hub`.** To test a LIVE
  remote, start its hub BEFORE the app launches (the rig's own `craze bridge --hub`, held open):
  the dormant re-probe is 30 s, so a hub started later is only seen half a minute on. A live
  remote's roster and lanes dial `bridge --hub --no-start` (plan 032 §3.5) with no probe of
  their own; only the sheet's `create_options`/create dial a plain `bridge --hub`. Match the
  two apart with `--no-start` in the text — `*"bridge --hub"*` matches both.
- **The sentinels.** The main instance's seam holds a RECORDING `craze` wrapper (it logs the
  `PATH` and argv it ran with, then execs a private copy) — every dial must have run with `PATH`
  exactly the seam dir. The empty-seam instance and the dormant remote jail each carry a craze
  stub at `$HOME/.nix-profile/bin` (an absolute rung relative to HOME) that must never run.
- **Do not point a hermetic launch at this host's `/usr/local/bin/craze`.** Craze is reached
  only through the fake-ssh jail. A control that swaps the jail's `craze` for the production
  binary belongs on one cell, or in the Docker leg, never on the whole file.
- **Teardown is by program path**: every craze process of a rig — each bridge, the hub, each
  `craze serve` it creates, the fake hosts and agents — runs a private COPY under the rig's
  `real/`, and the module end signals exactly those (re-checking each pid's command line first).
  Never `pkill -f craze`: the owner's own craze and hub may be running.
- The row merge needs a `FakeRoost` tab with `source="craze"` and `session_id` = the hub row's
  `provider_session_id` (the craze rows carry it). To show the roost row coming BACK, the feed
  must stay down: rename the seam's `craze` before SIGTERM, so a redial cannot birth a new hub.
  That cell is LAST in the module for that reason.
- `lane.*` ops take `target` and `session_id`. There is no `kind`. `ui.show_lane` too.

### The craze create sheet and Open in terminal (plan 025 C10)

- **Drive the sheet through its doors**: `ui.show_craze_create {target}` (opening it reads
  `craze.create_options` — on a DORMANT remote that is the explicit action that starts its hub),
  `ui.fill_craze_create {provider?, cwd?, title?, prompt?, recent?}` and `ui.submit_craze_create` (test
  mode only), `ui.close_craze_create`; read it with `craze_create.dump` (providers with
  `dimmed`/`selected`/`reason`/`fix`, `values`, the `request_id` it holds, `note`, `error`,
  `cause`, `create_enabled`, `primary`). **Wait for the dump to SHOW a fill** (its `values`, or
  `create_enabled`) before `ui.submit_craze_create`: the submit door presses the button through
  the gate of the LAST render, and a submit landing before the fill re-rendered is dropped
  (the launch dialog's doors behave the same).
- **The draft is the app shell's, per machine**, not the sheet's: closing and re-opening the
  sheet keeps the typed form (and a held request id). A cell that wants a clean form fills
  every field it relies on.
- **Losing a create's answer on purpose** (the unknown-outcome cell): the recipe rig's
  recording `craze` wrapper has switches in the rig root — `proxy` runs each NEW
  `bridge --hub` through `bridge_proxy.py`, which logs every `session.create`'s requestId to
  `creates.log`; with `drop-creates` too it relays the create, relays NOTHING back from then
  on (or a fast hub's answer slips through in the grace second), and cuts the connection a
  second later. The source's own retry is cut the same way, so the sheet sees
  `outcome_unknown` with the same id twice in the log. `outage` makes every craze run exit 1
  before any hub (Unreachable — a short backoff, unlike not-installed's 30 s); SIGTERM the hub
  with it on to take a machine OFFLINE under an open sheet, then remove it to bring it back.
  `refuse-dials` (with `proxy` on; plan 026) makes each NEW `bridge --hub` refuse the
  connection (logged to `refused.log`, read with `rig.refused()`) while connections already up
  — the roster's — stay, so the machine stays live: a create's dial fails without the machine
  going offline under the sheet, which `outage` cannot do. A HELD retry (Try again after an
  unknown outcome, sent `held: true`) dials twice into it and stays `unknown`; wait on the
  dump's `error.message` naming the refusal, not on `state` (it reads `unknown` before the
  press too).
- **A start failure on demand**: point `[agents].grok` at `craze-fake-agent -script
  exit-two-lines` (`rig.set_grok(...)`); craze reads it at every create. Restore it (in a
  `finally`) before the next create.
- **The fake agent reuses one provider session id**, so every hub-created session's row
  claims the same `provider_session_id`. Open in terminal attaches its tab to ITS row by
  hostId when a provider session is ambiguous (or absent) — a fold that went through the
  shared rule's tie-break would hand the tab to the newest row.
- **An attach tab is kept by a revision FENCE, not a timer**: `craze.open_terminal` reads
  `session.identify` + `tab.list`'s `revision` right after its `tab.open`
  (`shed_app::roost::tab_open_fenced`, retried once on a fresh connection); a snapshot of the
  SAME daemon below it is silent about the fresh (hidden, unowned) tab and is ignored, any later
  one — or one from a RESTARTED roost, whose revisions start again at 1 — that does not list the
  tab drops it. An unfenced entry (both reads failed) is kept 5 s whatever a snapshot says. A fake's `tab.list` must carry `revision`
  (both FakeRoosts do) or every snapshot is taken at its word. While the craze feed is down
  the tab stays on its retained row (the map is the app's own knowledge, not roost ownership).
- **`laneFailure` must not be applied twice**: a wrapper that already threw a coded
  `LaneFailure` has a message WITHOUT the code, and re-splitting it read `outcome_unknown` as
  `failed`. `laneFailure` now returns a `LaneFailure` unchanged.

### A running shed's craze — offer, don't list (plan 026 C2)

- **`app_shed` is the shed instance**: a PRIVATE `MockShedServer` (so the session mock's
  per-test reset never touches it) lists four sheds running under
  `fixtures/config-craze-shed.yaml` (one server, NO machines — a configured machine's
  unreachable backoff would repaint the listing and mask a "no repaint" control). A shed's ssh
  user is its NAME, so each shed is its own jail of the same fake `ssh` (`Remote(bins,
  jails=SHED_JAILS)`). None runs roost (the fake answers `client-bridge` `command not found`),
  so only craze can list one; until then it is `rc.list`'s `shed_craze`, read on screen through
  `shed_roost.dump`'s `craze_create`/`craze_note`.
- **Three switches in a JAIL's root**: `probe-127` makes the find-only probe exit 127 (not
  installed); `hold-roster` (content N) holds the (N+1)-th `bridge --hub` of that jail —
  `--no-start` ones counted — until the switch goes, recording the held record's name in
  `held` — `1` holds the roster's bridge behind the sheet's, so the sheet can be asserted while
  the shed is still unlisted; `0` holds the roster's first dial, which is how the W5 race cell
  ends the hub between the probe and the bridge. A held bridge's `hello` times out at 30 s:
  assert inside that. `craze-0.1` (plan 032) emulates a craze older than 0.2.0: every `bridge
  --hub --no-start` exits 1 with 0.1.x's `craze bridge: unknown or malformed flag` line (the
  real 0.2.0 bridge never runs), so the host's dial falls back to probe + plain `bridge --hub`.
  The W5 cells run in an instance of their own (`app_no_start`, config written by the fixture),
  because both switches must be set BEFORE the app's first dial.
- **The recorded pid is the bridge** (`Remote.records(user)` / `live_bridges(user)`, pids under
  `<root>/pids/`): the fake EXECs a craze command, because dash does NOT exec the last
  command of `sh -c` — without it the pid is an outer `/bin/sh` whose death leaves the craze
  bridge (and its pipes) alive, and the app never sees the connection end. The remote command
  arrives per-argument single-quoted (`'sh' '-c' '…'`), so a `case` on `sh -c *` never
  matches; match `"'sh' '-c' "*`.
- `commands(user)` is sorted by mktemp name (random), not time: use `records(user)` (by mtime)
  for ORDER ("the first `bridge --hub` is the sheet's").
- A dormant probe re-runs every 30 s, and so does a not-installed one: a cell that flips
  `probe-127` waits ~45 s each way. Without a fresh `rc.list` re-read (a `refresh` event) the UI
  keeps its last listing — drive a `refresh` before a control that must see the
  later listing.

### The orderly shutdown cells (plan 026 C3)

The app now handles SIGTERM/SIGINT/SIGHUP (a dedicated signal thread) and runs one bounded,
idempotent shutdown of every child it owns — so **the harness's `ui.terminate` (SIGTERM) runs that
shutdown too**. `test_tauri_craze.py`'s `_stop` times every stop and fails if the 5 s grace's
SIGKILL was ever needed; a typical stop is ~0.03 s. The app logs each phase to its own log
(`shed-desktop-tauri: shutdown: phase N … took M ms`).

- **A signal cell signals an app instance of its OWN** (`SigRig`, a function-scoped rig), never
  the module's shared `app`. Signal by the recorded pid; the test-mode `app.quit` op is the tray's
  path (`app.exit(0)`).
- **`app.terminate` is the macOS system quit** (plan 026 C3b). It drives `[NSApplication
  terminate:]`, the path a Dock Quit, a logout and an AppleScript quit take, which reaches the
  app only as `RunEvent::Exit`. It is test mode only and macOS only: Linux answers
  `not_supported`, so `test_a_macos_system_quit_leaves_no_app_children` SKIPS here and runs in
  CI's `tauri-mac`. Never send `terminate:` inline from a `run_on_main_thread` closure. The
  closure runs inside tao's event handler, holding its lock, and `applicationWillTerminate:`
  takes that lock again on the same thread: a deadlock. Queue it instead
  (`performSelectorOnMainThread:…waitUntilDone:NO`), as `ipc.rs`'s `system_terminate` does.
- **A descendant check alone cannot prove the orderly path in this rig.** Kill the app with no
  handler and every descendant still dies within a second: the craze bridges exit on stdin EOF,
  WebKit's helpers die with their parent. What a hard kill leaves behind is the `SshExec`
  ControlMaster, which ssh daemonizes — it is never in the descendant tree. Its proof is the fake
  ssh's `exits.log` (every `-O exit`, by control path); check it, and the exit status (an orderly
  exit is 0).
- **The craze hub is in the app's tree while the app runs** (the first bridge that Ensures it is
  its parent until that bridge exits), so list it — and everything under it — apart, from its own
  hub record, before asserting what died.
- **The post-exit grace for recorded descendants is 0.5 s, not more** (C3 fix round 1): the
  shutdown kills AND awaits its own children before the app exits, so they are gone already; the
  half second is only for WebKit's helpers noticing their parent went and a killed child's zombie
  being reaped once reparented. Don't widen it to make a cell pass — a bridge that needs longer
  is a bug. Likewise `_launch` releases its own app, log and agent if it fails after the spawn,
  and `SigRig.teardown` runs every step (holder, both namespaces) before re-raising the first
  failure, so a forced kill never leaks a jail.
- **Two signals sent back to back are ONE signal.** The OS and tokio coalesce a SIGTERM that
  arrives before the signal thread consumed the previous one. A "second signal" cell must wait
  for proof the first is being handled (a NEW `-O exit` past `phase 2` in the app's log) — not
  for any `-O exit`: with the fake ssh's `wedge-exit` switch on from launch, roost's own replace
  path logs (and is held by) some during normal operation.
- **Every dir above a `CRAZE_RUNTIME_DIR` must be 0700**: a nested rig dir made with
  `mkdir(parents=True)` is group-writable under a 002 umask, and craze then refuses the runtime
  dir with no error the harness surfaces (the fake host just exits before its ready line).

### The settings sheet (plan 025 C11)

- **Drive it through its doors**: `ui.show_lane_settings {target, session_id}` mounts
  the transcript panel AND asks for its sheet; `ui.pick_lane_setting {row, value}` (test mode
  only) presses a value — `row` is `model`, `mode` or an option's id; `ui.close_lane_settings`.
  Read it with `lane_settings.dump` (rows with `control`, `current`, `values[].selected`,
  `state`/`text`/`enabled`, the `chip`, `usage`) and the header chip with `lane.dump`'s
  `settings_chip`. Wait for the dump to show a press's effect (`state == "pending"`, or the new
  `current`) — never sleep. A press on the value a row already shows sends nothing.
- **A session with per-model settings**: `craze-fake-agent -script permodel` is cursor's wire
  (four models, each with its own options) and speaks cursor's AUTH, so it must run as
  `cursor`, not grok (as grok it fails `no supported auth method`). `rig.set_agents(grok=…,
  cursor=<wrapper>)` and create with `provider: "cursor"`; then `rig.set_grok(rig.grok_echo)` AT
  ONCE — the C10 sheet cell asserts cursor is NOT ready. Every fake-agent script advertises
  models and modes, so no craze session in the rig has `settings: false`. A roost tab is
  status only: there is no opencode transcript and no `machine.kill`.
- **The agent's own knobs go in its wrapper** (the hub's env is fixed at its birth):
  `CRAZE_FAKE_DUMP_CALLS=<file>` records every `set_config_option <id>=<value>` the agent was
  ASKED for (what reached it, not what was sent); `CRAZE_FAKE_SET_GATE=<path>` holds each set
  until a byte is written to that FIFO — and is a no-op while no FIFO exists there, so a cell
  turns it on with `mkfifo` (`SetGate`) and off by unlinking it. Release only after the set is
  at the agent (the dump shows it): the gate's reader opens the FIFO when the set arrives.
- **`stale_model` on demand**: a SECOND client (the rig's own `craze bridge --hub`: `hello`,
  `session.connect{hostId}`, the host `hello`, `sessions.list`) sends a model change while the
  gate holds it; the sheet's option press, bound to the old model, queues behind it in craze's
  one-at-a-time settings queue; release the gate and craze refuses it `stale_model`. Seen on
  the wire in `sets.log` (the proxy logs every `session.set` with its params).
- **"The sheet shows A, the lane has folded B"** (Amendment A13): `ui.hold_lane_view {hold:
  true}` (test mode) stops the panel committing its reads, so it keeps rendering the old model
  while `lane.messages` shows the move a second client made; a press then is bound to the
  displayed model (`for_model`). Release it (`hold: false`) in a `finally`.
- **A lost answer on demand**: with `proxy` on (BEFORE the lane's bridge is dialled — the
  wrapper reads the switch per spawn), `drop-sets` makes the proxy relay a `session.set` and
  cut its connection; with `outage` on too the redial fails, so "not confirmed" holds until
  both are off and the lane resumes. The resume restates the settings before its `Ready`.

### The `/` menu, rename and Name at create (plan 029 C4)

- **Drive the composer through its doors** (test mode only): `ui.type_lane_composer {text}` sets
  the draft (caret at the end), `ui.key_lane_composer {key, shift?, composing?}` dispatches a REAL
  `keydown` on the textarea (`key` ∈ Enter/Tab/Escape/ArrowUp/ArrowDown — a typo is
  `bad_request`, so a "nothing happened" cell cannot pass on a key never pressed),
  `ui.pick_slash_row {name}` clicks a menu row, `ui.answer_lane_stop {stop}` answers the Stop
  confirm `/exit` opens. Read `lane.dump`'s `draft` and `slash_menu` (`{open, token, rows:
  [{name, dimmed, highlighted}], refusal, warning}`); `lane.messages`' `slash_menu` is the
  backend's resolved menu (different question).
- **The type door is the LAST listener the panel registers**: re-send it until the dump's
  `draft` shows it (`_type` in `test_tauri_craze.py` does), and every other door is live then.
  Wait for the menu to have answered the token (`_wait_menu`: `token` and `open`) before a key —
  the filter is an async invoke per keystroke.
- **A synthetic keydown has no default action.** The door applies the browser's one default
  that matters — a newline for an Enter nobody prevented (not while composing) — so Shift+Enter
  is observable as `draft == "/\n"`. Arrows never move the caret through the door.
- **Only HUB-CREATED sessions have a catalog.** The `craze-fake-host` binary has no slash flag
  (its `fakehost.Options.Slash` is library-only), so the fake host's lane says `slash: false`
  and has no menu — which is the "no catalog, no menu" cell. Create through the rig's bridge
  (`_hub_create`); a grok-echo session shows `model, rename, plan, ask, agent, exit, research`.
  `SET-COMMANDS a,b` sent as a prompt makes the fake agent re-advertise its commands live.
- **What the commands put on the wire**: the proxy logs `session.setTitle`, `session.stop` and
  `session.prompt` to `verbs.log` (`rig.verbs(method)`), `session.set` to `sets.log`, and a
  create's `title` in `creates.log`. It also records what came BACK: the hub's reply to every
  `session.create` (`create-replies.log`, `rig.create_replies()`: the `session` row it answered
  with) and the reply to every `session.attach` (`attaches.log`, `rig.attaches()`: `cursor`,
  `snapshot`, the snapshot's `settings.title`), each written before it is relayed — so it is on
  record once the app has read it. Turn `proxy` on BEFORE the session's lane is first dialled
  (the wrapper reads the switch per spawn), and `lane.close` a lane an earlier cell left open on
  an unproxied connection before reusing it.
- **A turn held open**: `craze-fake-agent -script sigint-hold` with `CRAZE_FAKE_GATE` a FIFO that
  exists BEFORE the first held turn (the agent then holds it open and every byte releases one
  turn; with no FIFO there, turns are released at once) and `CRAZE_FAKE_DUMP_PROMPTS` to see what
  the agent READ. It is cursor's dialect, so it runs as `cursor` (restore `set_grok` at once).
  Turn 1 is an echo; every later one — a queued prompt included — waits for its own byte.
- **A rename held pending**: while `hold-titles` exists as a FIFO in the rig root (`with
  SetGate(rig.root / "hold-titles", what="rename")`), the proxy logs a `session.setTitle` to
  `verbs.log` and then HOLDS it until one byte is written (`gate.release()`); removing the FIFO
  lets it through too, so a failing cell never wedges the lane. Wait for the verb in
  `verbs.log` before acting "while it is pending" (the draft-ownership cell types a new draft
  then). The permodel agent's set gate does NOT hold a mode change on its own: it holds only
  `session/set_config_option`, and craze sends a mode as `session/set_mode`.
- **"From the first frame" is asserted on the WIRE, never by polling** (P26): assert on the
  proxy's recorded replies — the create's answer row (`rig.create_replies()`, `session.row.title`)
  and the lane connection's FIRST attach reply for that session (`rig.attaches()`: `cursor`
  false, `snapshot` true, its `title`). No poll proves a first frame, however tight: an untitled
  seed (or row) followed by a title delta before the first read passes any poll, and a delta
  moves `session.title` without moving `generation`. Polls of `rc.list`, `lane.messages` and
  `lane.dump` (`_poll`) are fine as EVENTUAL checks of what the app shows — never call them
  first-frame checks. Negative control: strip the create's `title` in the proxy and rename the
  session right after it — before relaying the create's answer (the cell must fail on the
  recorded reply row) or right after relaying the lane's first attach reply (with the reply
  assertion off, it must fail on the recorded snapshot). Whether a poll ALSO catches either is
  timing (the app's `rc.list` shows the create's own untitled answer row for a moment) — which
  is exactly why a poll is never the first-frame assertion.
- **A craze row's title is `display_name`** in `rc.list`; the lane's is `session.title`.
- **The create sheet's `values` gained "Name"** on a live craze 0.1.1 hub (`createTitle`); it is
  absent while the machine is offline or dormant (and on an older hub) unless a title is typed.

### Against a REAL local daemon

The render-gate container can drive the roost-session running on the **host**. Mount its
socket *directory* — a bind-mounted socket must be **writable**, `:ro` makes it unconnectable
— and map it:

```bash
make -C desktop tauri-ui-build
docker build -t shed-tauri-linux:latest - < desktop/Dockerfile.tauri-linux
ROOT="$PWD"; HOST_OUT=/tmp/shed-shots; mkdir -p "$HOST_OUT"
docker run --rm -v "$ROOT:/repo:ro" -v "$HOST_OUT:/out" \
  -v /run/user/1000/roost-session:/roost \
  -v shed-tauri-linux-cargo:/usr/local/cargo/registry \
  -v shed-tauri-linux-cargo-git:/usr/local/cargo/git \
  -v shed-tauri-linux-target:/target -e CARGO_TARGET_DIR=/target \
  -e UV_PROJECT_ENVIRONMENT=/tmp/uv-venv \
  -e SHED_TAURI_BIN=/target/debug/shed-desktop-tauri \
  -e SHEDTEST_ROOST_SOCKETS=roost:my-server/dev=/roost/roost.sock \
  --cap-add SYS_ADMIN --security-opt seccomp=unconfined --shm-size=1g \
  shed-tauri-linux:latest bash -c '…build + driver, as in the screenshot recipe below…'
```

- Export `SHEDTEST_ROOST_SOCKETS`, not the app-level `SHED_TAURI_ROOST_SOCKETS` — a hermetic
  launch always sets-or-clears the app-level var itself (`ui.py`'s `subproc_env`), so an
  inherited value from the container's own env never reaches the app; it must be supplied
  explicitly instead. Driving through pytest, `conftest.py`'s `_app_session` fixture reads
  `SHEDTEST_ROOST_SOCKETS` and passes it to `ui.launch(roost_sockets=...)`. A bespoke driver
  script (the screenshot recipe below) must do the same itself.
- Then `rc.list` carries that shed's roost rows (`target` `roost:<server>/<shed>`,
  `tab_id`, `attention`) and `capabilities["roost:<server>/<shed>"]` — synthesized,
  `attach: "native-remote"`, so the card offers no terminal attach.
- The container's uid is root (0) while the socket is owned by uid 1000; roost's socket is
  mode 0600, so either run with `--user 1000` or loosen the socket's mode for the run. If
  `rc.list` shows that shed absent, that is the first thing to check — an unconnectable
  socket is indistinguishable from "no session" by design. A shed not named in the map
  still uses `SHED_TAURI_SSH_BIN` when that fake `ssh` is configured.

### `tauri-test-linux` lints the Linux cfg; `tauri-mac` lints the mac one

`make tauri-test-linux` runs `cargo clippy --locked --all-targets -- -D warnings` in its
container before the crate's `cargo test` (plan 030, FU3), and `make -C desktop tauri-lint` is
clean on Linux too — the three Linux-only findings that used to sit in `src/tray.rs` and the
`zbus::proxy` macro in the old approval code were fixed (that code is gone). So a Linux lint is a real failure now: it
fails CI's `tauri-linux` job before a test runs, and there is no baseline to discount.

What Linux clippy still cannot see is code under `#[cfg(target_os = "macos")]`, which only the
**`tauri-mac`** job's `make tauri-lint` compiles. A platform-independent lint in shared code —
`type_complexity` on a test helper, say — is caught on both legs; a mac-only one only on the
Mac. Plan 014 lost a round trip to that, before Linux linted at all.

### The definitive check runs on a Mac, and its baseline is CLEAN

`make -C desktop tauri-lint` is the real gate (`desktop/Makefile` already makes it depend on
both `sparkle-framework` and `tauri-ui-build`, so a bare `make -C desktop tauri-lint` stages
Sparkle and rebuilds the UI bundle for you). If you instead invoke `cargo clippy` directly on
the Tauri crate — e.g. over SSH to a Mac worktree for a faster inner loop while iterating on one
file — both prerequisites are on you:

```bash
ssh mac-mini   # or whatever your macOS box is
cd ~/projects.bak/shed   # a worktree checked out to the branch under review
make -C desktop sparkle-framework          # build.rs panics without it (macOS-only dep)
make -C desktop tauri-ui-build             # generate_context! panics at macro expansion
                                            #   without a non-empty tauri/ui/dist — same
                                            #   trap as the Linux gate above, just fatal
                                            #   here instead of "serves stale content"
cd desktop/tauri/src-tauri
cargo clippy --locked --all-targets -- -D warnings
```

With a `CARGO_TARGET_DIR` outside `src-tauri` (a scratch target dir you can delete afterwards),
the sparkle-updater `build.rs` panics with `Sparkle.framework not found!` even though it is
staged. It looks for the framework by walking `OUT_DIR`'s ancestors for `tauri.conf.json`, so
name it: `export SPARKLE_FRAMEWORK_PATH=<worktree>/desktop/tauri/src-tauri`. And `npm` need not
exist on the Mac: copying a built `desktop/tauri/ui/dist` over from Linux satisfies
`generate_context!` for `cargo check`/`clippy`.

**Unlike Linux, this baseline is CLEAN at `origin/main`** — there is no `tray.rs`/`zbus::proxy`
pre-existing-findings exemption on macOS (those are Linux-only code paths). So on a Mac, run
clippy with `-D warnings` as a real gate, not a filter: if it is red, the finding is yours.

### The daemon must speak the pinned session protocol

Shed pins `roost-ipc` to one rev in `crates/Cargo.toml` — **read the pin from there rather than
from this page**, and `roost_ipc::messages::SESSION_PROTOCOL_VERSION` at that rev is the number
(6 as of plan 021). `Conn::session_identify` **refuses a mismatch by name** rather than limping,
so a daemon at any older generation — protocol 5, which is what plan 020's desktop installed on
every host it bootstrapped; protocol 4 from plan 019 before it; the protocol-2 releases before
that — reads as an unreachable machine row whose detail is `ProtocolMismatch { theirs, ours }`.
That is correct, not a bug: generation 5 retired the lease from the wire entirely
(`events.subscribe` takes no arguments, `session.set_agent_hooks` is open to every client) and
generation 6 reshaped that op into a raise, so limping would mean guessing which side's shape to
speak.

Build one from roost's tree at the pinned sha into a scratch checkout of its own — a **release**
build, so the daemon's socket lands under the non-`-dev` `roost-session` directory (the `-dev`
trap in `crates/CLAUDE.md`), and outside roost's own working tree so it survives whatever branch
that is on:

```bash
# The rev shed pins, read out of the manifest rather than retyped here.
REV=$(sed -n 's/.*roost-ipc = .*rev = "\([0-9a-f]*\)".*/\1/p' ~/projects/shed/crates/Cargo.toml)
DEST=~/.cache/shed-roost/roost-${REV:0:7}
git -C ~/projects/roost archive "$REV" | (mkdir -p "$DEST" && tar -x -C "$DEST")
cd "$DEST"
cargo build --release -p roost-session -p roost-cli   # roost-cli's binary is `roostctl`
# → target/release/{roost-session,roostctl}
```

**No build needed for the pinned release:** `RELEASE_PIN`'s linux asset
(`gh release download v<pin> -R charliek/roost -p 'roost-session-<pin>-linux-amd64*'`, then
`sha256sum -c` the `.sha256` beside it) is a release build of the same protocol and works as
this binary; copy it to a directory of its own named `roost-session`. The render gate
(`tauri-build-linux`) does **not** forward `SHED_TAURI_ROOST_SESSION_BIN`, so run its `docker
run` by hand with `-v <that dir>:/roost-bin:ro -e SHED_TAURI_ROOST_SESSION_BIN=/roost-bin/roost-session`
added; both `real_roost` cells then run inside the full suite.

**`SHED_TAURI_ROOST_SESSION_BIN` is the knob that makes this binary findable.** The shedtest
default path (`test_tauri_bootstrap.py::_real_session_binary`) names the rev of whichever plan
last moved the pin, so after a re-pin the default will not exist and the `real_roost` cells
**skip silently** rather than fail — export the var at the path built above.

(roost's build needs libghostty-vt; follow its own README if the link step complains.) Confirm
the result with `roost-session --version`-style output: it must report the same
`session_protocol` the pinned `roost-ipc` speaks. Both
`SHED_TAURI_ROOST_SESSION_BIN` (the `real_roost` smoke below) and the mount-the-host-socket
recipe above want that binary. If a machine row comes up unreachable with a protocol mismatch,
the daemon is the old one — rebuild rather than un-pinning shed.

### The `real_roost` pytest smoke (no host socket to mount)

`test_tauri_bootstrap.py::test_the_real_binary_is_installed_and_started_behind_the_fake_ssh`
is a simpler alternative to the mount-the-host-socket recipe above: it drives a real
`roost-session` behind the fake ssh. Mount a built `roost-session`'s directory read-only
and point `SHED_TAURI_ROOST_SESSION_BIN` at it, then run pytest with `-m real_roost`
(the test is skipped, not failed, when the binary is absent — that's how CI stays
roost-binary-free):

```bash
-v /path/to/roost/target/debug:/roost-bin:ro \
-e SHED_TAURI_ROOST_SESSION_BIN=/roost-bin/roost-session \
… uv run --group test pytest tools/shedtest/test_tauri_bootstrap.py -m real_roost
```

The rig forces `0700` on the jail directories it creates (`test_tauri_bootstrap.py`),
so a group-writable developer umask does not fail the daemon's runtime-dir check.

## How the Docker legs are wired (so failures make sense)

The `deb`, `tauri-build-linux`, and `tauri-test-linux` targets `tar` a **repo-root-relative**
layout (`crates desktop/tauri desktop/tools desktop/Resources …`) into `/work` inside the
container, so the Tauri crate's `../../../crates` path-deps resolve in the recreated layout
exactly as in the repo. The source is copied into a writable `/work` (not a read-only mount)
because Tauri's `build.rs` writes `gen/` next to `Cargo.toml`. Rust builds to a `/target`
volume so it never clobbers the mac target dir. **Two** cargo caches are volumes — the
registry AND `shed-tauri-linux-cargo-git:/usr/local/cargo/git`, because `shed-core` takes
`roost-ipc` as a **git** dependency and without the second volume every run re-clones roost.

## Capturing deterministic screenshots (the render gate, repurposed)

The render gate proves the app renders; to grab labeled PNGs of a specific
pane/appearance (e.g. the Plex reskin, the Egress pane, a dark-mode shot), run a
**one-off `docker run` that mirrors `tauri-build-linux`** but swaps the pytest
command for a small Python driver. Same wiring as the target (see the Makefile):

- Build the frontend bundle FIRST — `make -C desktop tauri-ui-build` (or `cd
  desktop/tauri/ui && npm run build`). The Makefile legs take `tauri-ui-build` as
  a prereq and the resulting `tauri/ui/dist` is tarred into `/work` (dist is not
  excluded); on a clean checkout it's absent and the in-container `cargo build`
  fails closed at `generate_context!` (see the Gremlins note below).
- `docker build -t shed-tauri-linux:latest - < desktop/Dockerfile.tauri-linux`
  first (these commands run from the repo/worktree root — the same dir the mounts
  below are relative to).
- Mount the **REPO/WORKTREE ROOT** (the dir that holds `crates/` + `desktop/`)
  **read-only** at `/repo` (`-v "$ROOT:/repo:ro"`), and add a **writable** out dir
  for the PNGs (`-v "$HOST_OUT:/out"` — the `deb` target's pattern).
- Reuse the `shed-tauri-linux-{cargo,target}` cache volumes so the Rust build is
  incremental across runs (`-v shed-tauri-linux-cargo:/usr/local/cargo/registry -v
  shed-tauri-linux-target:/target`), with `-e CARGO_TARGET_DIR=/target`.
- **Pass `-e UV_PROJECT_ENVIRONMENT=/tmp/uv-venv`** (copy it verbatim from the
  Makefile's `tauri-build-linux` render-gate block). `uv run` defaults to writing
  its project venv at `.venv` next to `pyproject.toml` — but that's under the
  read-only `/repo`→`/work` tree, so without this override `uv run` **wedges
  silently** in the container (no venv it can write, no error you'll see) and the
  driver never runs. Every Makefile Docker leg that runs `uv` sets it; the ad-hoc
  driver run needs it too.
- `--cap-add SYS_ADMIN --security-opt seccomp=unconfined --shm-size=1g` and
  `-e SHED_TAURI_BIN=/target/debug/shed-desktop-tauri` (point the harness at the
  binary this run builds). **Note:** `--cap-add SYS_ADMIN --security-opt
  seccomp=unconfined` materially weakens the container's isolation (it's needed so
  WebKitGTK's bubblewrap sandbox can create user namespaces) — only run this
  against a **trusted** source tree, and never mount host Docker sockets, SSH
  agents, or secrets into a container running with these flags.

Putting it together — the complete invocation (repo/worktree root; expects your
driver at `$HOST_OUT/driver.py`). **Set `ROOT`/`HOST_OUT` on their OWN line, as
below — NOT as one-line prefix assignments on the `docker run` itself.** A prefix
assignment (`HOST_OUT=/tmp/shed-shots docker run … -v "$HOST_OUT:/out"`) does NOT
apply to that same command's own argument expansion — the shell expands `$HOST_OUT`
before the assignment takes effect, so `-v ":/out"` binds an empty source and the
PNGs vanish. Assign first, then run:

```bash
make -C desktop tauri-ui-build
docker build -t shed-tauri-linux:latest - < desktop/Dockerfile.tauri-linux
ROOT="$PWD"; HOST_OUT=/tmp/shed-shots; mkdir -p "$HOST_OUT"
docker run --rm -v "$ROOT:/repo:ro" -v "$HOST_OUT:/out" \
  -v shed-tauri-linux-cargo:/usr/local/cargo/registry \
  -v shed-tauri-linux-target:/target -e CARGO_TARGET_DIR=/target \
  -e UV_PROJECT_ENVIRONMENT=/tmp/uv-venv \
  -e SHED_TAURI_BIN=/target/debug/shed-desktop-tauri \
  --cap-add SYS_ADMIN --security-opt seccomp=unconfined --shm-size=1g \
  shed-tauri-linux:latest bash -c '
    mkdir -p /work && cd /repo && \
    tar cf - crates desktop/tauri desktop/tools desktop/Resources \
      desktop/pyproject.toml desktop/uv.lock | tar xf - -C /work && \
    cd /work/desktop/tauri/src-tauri && cargo build --locked && \
    cd /work/desktop && xvfb-run -a --server-args="-screen 0 1400x900x24" \
      uv run --group test python /out/driver.py'
```

The driver is a Python script that:

- adds `/work/desktop/tools/shedtest` to `sys.path` and imports `ui`, `client`,
  `mockserver`, `hermetic`;
- launches the mock + the app hermetically via the harness's own `ui.launch`
  (throwaway HOME/XDG under `/work` or `/tmp`), passing
  `hermetic.overrides(runtime_dir)` for the launch environment;
- seeds fixtures — `rc.inject_test` rows for the Agents pane (since S6 it injects into
  the shed's ROOST snapshot, so `slug` must parse as a roost tab id and the shed has to
  have been listed by `sheds.list` first), events for Activity
  (the mock's `/api/activity` rows), and for a shed's egress detail a mock
  built with `MockShedServer(egress_events=[{...}])`, whose egress route streams those
  decisions instead of answering 501 (`test_tauri.py::test_egress_activity_filters_ns`
  is the mixed-ns pattern) — `navigate`s to the pane, drives sub-state
  (`egress.show`), calls `ui.set_appearance("dark")` for the dark shot, and captures
  via the `app.screenshot` op, writing each PNG under `/out`.

`app.screenshot` on the Xvfb (X11) leg shells out to `scrot`, falling back to
ImageMagick's `import -window root` if `scrot` is absent or fails (the tool order in
`src-tauri/src/screenshot.rs::capture` is `grim` → `scrot` → `import`; `grim` is
skipped without a `WAYLAND_DISPLAY`, so X11/Xvfb tries `scrot` then `import`). Either
way the PNG is the full display; the reported truth ops (`dashboard.dump` / `agents.dump` /
`sidebar.dump` / `shed_roost.dump` / `egress.profiles` / `ui.badges` / `ui.computed_style`) stay the deterministic
assertions, the pixels are the eyeball. `agents.dump`'s `empty` is how the ABSENCE of
sessions is asserted. `launch.dump` and `machines.dump` answer `unknown_op`.

## Gremlins

- **On a Linux workstation, `e2e-tauri` must run under Xvfb — never on the inherited desktop.**
  An agent's shell inherits `DISPLAY`/`WAYLAND_DISPLAY` from the owner's Wayland session; a run
  there pops windows onto the owner's screen, fails every screenshot cell (`scrot: no image
  grabbed`) and fails craze lane cells that pass under Xvfb (this cost plan 025's C1
  hours). The exact command:
  `env -u WAYLAND_DISPLAY -u DISPLAY XDG_SESSION_TYPE=x11 xvfb-run -a --server-args="-screen 0 1400x900x24" make -C desktop e2e-tauri`
  — and kill any `shed-desktop-tauri` the run leaves behind BY PID (`ps -o pid,ppid,lstart,args
  -C shed-desktop-tauri`), never one the owner started.
- **A `#[tauri::command]` drops every invoke key it does not declare, silently** → the
  frontend (or a harness) sending `permissionMode` to a command without a `permission_mode`
  parameter gets the command run as if the key were absent — no error anywhere. A command's
  argument shape is testable only above its body: drive it through Tauri's mock runtime
  (`tauri::test::mock_builder` + `get_ipc_response`; the crate's dev-dependency turns on
  tauri's `test` feature, which adds no package). The launch-command tests were removed
  with the launch dialog.
- **Run a subset of tauri modules in collection (alphabetical) order.** A module that
  `ui.quit`s the session app and does not bring it back makes a later module error at
  setup with `FileNotFoundError` from `client.py`'s `connect`. A full run is fine.
- **WebKitGTK web-process dies / JS never runs** → the render gate needs
  `--cap-add SYS_ADMIN --security-opt seccomp=unconfined` (already in the target) so WebKitGTK's
  bubblewrap sandbox can create user namespaces Docker's default seccomp blocks.
- **DMABUF / GPU errors** → `WEBKIT_DISABLE_DMABUF_RENDERER=1` (set in
  `Dockerfile.tauri-linux`; there's no GPU in Docker). If you run the binary by hand in a
  container, export it yourself.
- **Render gate flakes / OOM** → the target passes `--shm-size=1g`; a constrained Docker VM
  (low memory/CPU) starves WebKitGTK — bump Docker Desktop's resource limits.
- **Stale frontend / build errors after a `tauri/ui` dep bump** → the bundle is built on the
  host first (`tauri-ui-build` = `npm run build`); if `node_modules` is stale, refresh it with
  `cd desktop/tauri/ui && npm ci`.
- **`generate_context!` fails closed** → `cargo build`/`test`/`clippy` of the Tauri crate needs
  the frontend bundle (`tauri/ui/dist`) present; the targets run `tauri-ui-build` first for
  this reason. Building the crate by hand? build the UI bundle first.
- **In-container paths** → everything runs under `/work/desktop`; `uv` uses
  `UV_PROJECT_ENVIRONMENT=/tmp/uv-venv` (the repo mount is read-only). Don't assume host paths.
- **A rev bump of `roost-ipc` has TWO manifests** → `crates/Cargo.toml` (the workspace pin) and
  `desktop/tauri/src-tauri/Cargo.toml` (the Tauri crate names `TabOpenParams`/`Tab`/`Ownership`
  directly, so it takes its own git dep). They must stay in lockstep or the tree gets two copies
  of the crate and `shed_app::roost`'s signatures stop accepting the Tauri crate's types. The
  Tauri lock is regenerated **without** WebKitGTK — `cargo metadata --manifest-path
  desktop/tauri/src-tauri/Cargo.toml --offline >/dev/null` resolves and rewrites it in a second;
  a full `cargo build` there only works in Docker.
- **`cargo fmt --check` is not a gate on the Tauri crate, but your own files still are** —
  unlike `crates/`, `desktop/tauri/src-tauri` is NOT rustfmt-clean at `origin/main` (about
  twenty-odd hunks across `lib.rs`,
  `screenshot.rs`, `termctl.rs`, `tray.rs`, `updater.rs`), so a whole-crate `cargo fmt --check`
  is red before you touch anything and a whole-crate `cargo fmt -- --emit files` would bury your
  diff in unrelated reflows. Check only the files you edited, against their own baseline:
  `git show HEAD:desktop/tauri/src-tauri/src/<f>.rs > /tmp/base.rs && rustfmt --check --edition
  2021 /tmp/base.rs` — if that is silent the file was clean, so format just yours with
  `rustfmt --edition 2021 src/<f>.rs`.
- **Bidirectional and zero-width characters in test strings must be ESCAPES.** rustc denies a
  literal U+202A–U+202E / U+2066–U+2069 in a string (`text_direction_codepoint_in_literal`),
  and some editors and tools turn a typed `\u202e` into the literal character on save. Write
  them as `\u{202E}` in Rust and `\N{RIGHT-TO-LEFT OVERRIDE}` in Python, and check with
  `grep -nP '[\x{202a}-\x{202e}\x{200b}\x{2066}-\x{2069}]' <files>` before building
  (the machine-name cleaning cells need such strings).
- **Adding or dropping a `crates/` dependency moves TWO lockfiles** — the same trap as the
  `roost-ipc` rev bump above, for a different reason. The Tauri crate path-depends on
  `crates/*`, so a dep added to (or removed from) any of those crates changes
  `crates/Cargo.lock` **and** `desktop/tauri/src-tauri/Cargo.lock`, and
  `scripts/release/release-plan.sh` verifies the two in lockstep before the desktop leg ships
  (it greps the Tauri lock for a `version` entry per workspace path-dep, so a NEW member crate
  must be added to that loop in all three `scripts/release/*.sh` too). Regenerate both without
  a WebKitGTK toolchain:
  `(cd crates && cargo update -w --offline)` and
  `(cd desktop/tauri/src-tauri && cargo update -w --offline)` — each rewrites its lock in a
  second. Review the diff: a path-dep change should add/remove exactly the `[[package]]` block
  and its `dependencies` lines, and nothing third-party.
- **A module fixture named after a session fixture breaks the whole pytest session** →
  `conftest.py` owns the SESSION-scoped `mock` and `studio` that the autouse `_app_session`
  requests by name. A module-level fixture of either name shadows it and every test in the
  run dies with `ScopeMismatch: You tried to access the function scoped fixture mock with a
  session scoped request object` — but ONLY when that module is collected before the session
  fixture is first resolved, so it can pass in a full run and fail when the file is run alone.
  Name a per-module double something else (`fake_roost`, `roost`, `egress_mock`, …). (`fake`
  is free since the session stopped using a fake daemon.)
- **`FakeRoost.stop()` is roost's `session.stopping`, not the teardown** (plan 014, mirroring the
  Rust fake) → it tells every stream why, hangs up, and latches the fake *unavailable* until
  `restart()`. Tearing the fake down — stop listening, remove the socket file, produce the
  durable "no roost-session at <path>" state — is **`shutdown()`**.
- **`cargo: command not found` in a fresh shell** → the host Rust toolchain is
  mise-managed and isn't on a non-login shell's PATH. `export PATH="$HOME/.cargo/bin:$PATH"`
  before running `cargo`/`make tauri-*` by hand (the Docker legs carry their own in-image
  cargo, so this only bites host-side builds — the native run + the ad-hoc screenshot driver).
- **`rc.inject_test` state silently coerces to `ready`** → the valid `RcState` wire values
  are `starting|ready|reconnecting|needs-trust|needs-auth|dead` — there is no `working`/`idle`.
  An unrecognized `state` fails to deserialize and falls back to `RcState::Ready` with **no
  error** (`ipc.rs::build_inject_session`), so a fixture with a typo'd/invented state renders
  Ready and you chase a phantom. Send the exact wire value.
- **WebKitGTK renders a raw `<select>` illegibly in dark mode** → its native button text
  stays dark-on-dark. In the Preferences window every dropdown (terminal preset)
  uses the dialog kit's `Select` (`components/dialog`, `appearance-none` + its own caret), NOT
  a bare `<select>`. Reach for `Select` when adding any picker to the Tauri UI, or the dark
  screenshot is unreadable (`PreferencesWindow.tsx`).
- **No per-window screenshot capture exists** → `app.screenshot` grabs the whole X display
  (`screenshot.rs::capture`, `grim`→`scrot`→`import`), not a chosen window, and the Preferences
  window is fixed-size (520×640, not resizable). Its below-the-fold content can't be
  photographed, so don't try to prove a lower section by pixels — trim the fixture so the
  section you care about sits above the fold (e.g. leave a provider off so its editor stays
  collapsed), and
  rely on `prefs.dump`'s reported `sections`/`values` for the logical assertions. Pixels are the
  eyeball; `prefs.dump` is the truth.

## Native run on a Mac

On macOS `make -C desktop tauri-run` builds + launches the client on **WKWebView**, not
WebKitGTK, so it is **not** the render gate; still run `tauri-build-linux` before trusting a
shared/Linux change. The macOS loop itself is the `shedtest-mac` skill.

## When you hit a NEW rough edge

Update this skill whenever you hit a new rough edge it doesn't cover.
