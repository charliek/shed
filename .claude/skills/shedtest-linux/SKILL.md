---
name: shedtest-linux
description: Validate the shed desktop app's Tauri Linux client from a Mac (or Linux) dev machine — the WebKitGTK render gate, the Linux-only approval-seam crate tests, the nfpm .deb build + clean-container install validation, and driving the app headlessly over its IPC socket. Use when asked to test the Tauri/Linux client, verify a shared/Linux desktop change, debug a WebKitGTK render or .deb failure, or run the Docker legs. The app lives under `desktop/`; the shared Rust core is the sibling `crates/` workspace. The macOS app loop is the shedtest-mac skill.
---

# Tauri Linux client end-to-end (from a Mac dev box)

The shipped **Linux** client is the **Tauri** app (`desktop/tauri/`, React/Vite/Tailwind on
WebKitGTK), a thin shell over the shared Rust core in `crates/`. Its WebKitGTK toolchain isn't
on macOS, so the Linux legs run **in Docker** — the same north star as the mac loop applies:
verify by driving the real app over its IPC socket, not by clicking. Run targets with
`make -C desktop <target>` (or the root `make desktop-<target>` passthrough). Docker must be
running.

## The fast path

```bash
make -C desktop tauri-test-linux    # Tauri crate's Linux-only approval-seam tests (polkit gate; Docker, no display)
make -C desktop tauri-build-linux   # the render gate: --target tauri on ubuntu:24.04 / WebKitGTK 2.44 under Xvfb (Docker)
make -C desktop core-linux          # shed-core cargo test + clippy on Linux (Docker)
```

- **`tauri-build-linux` is the render gate and the one to trust for any shared/Linux change.**
  It builds the Tauri Rust app and runs the `--target tauri` pytest suite on the **real shipped
  WebView** (WebKitGTK 2.44) under Xvfb — the render smoke *is* the CSS gate (2.44 supports
  oklch/color-mix/`:has()`/`@container`, so a static denylist would be miscalibrated). The mac
  WKWebView e2e alone can miss Linux-only breaks.
- **`tauri-test-linux`** compiles the Tauri crate on Linux (where the polkit `AuthGate` +
  libnotify `Notifier` compile) and asserts the gate is fail-closed. No display needed.
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
  uv run --group test pytest tools/shedtest --target tauri -q -p no:cacheprovider
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
`shed-desktop-tauri` → `/usr/bin/shed-desktop`), with a headless `shedctl` and the polkit
action bundled; `deb-validate` installs it in a fresh container and runs
`linux/scripts/validate-deb.sh` against the newest `out/*.deb`.

## Driving the Tauri app over IPC

The Tauri client speaks the same `{id,op,params}` JSON IPC as the mac app, over
`$XDG_RUNTIME_DIR/shed-tauri.sock` (override `SHED_TAURI_SOCKET`; `/tmp/shed-tauri-<uid>/`
fallback). Hermeticity hooks are `SHED_TAURI_TEST_MODE` / `SHED_TAURI_MOCK_BASE_URL` /
`SHED_TAURI_SHED_CONFIG`; timeouts scale with `SHED_TAURI_TEST_TIMEOUT_SCALE`.

```bash
make -C desktop tauri-run       # build the UI bundle + launch natively (Mac Homebrew WebKitGTK / Linux)
make -C desktop e2e-tauri       # shared suite + test_tauri at --target tauri (needs a display; Xvfb on Linux)
```

- The harness picks a prebuilt binary via **`SHED_TAURI_BIN`** — the render gate points it at
  `/target/debug/shed-desktop-tauri` inside the container so the pytest run drives the binary
  built in the same step. Set `SHED_TAURI_BIN` when driving a binary you built out-of-band.
- `e2e-tauri` runs the ONE `tools/shedtest` harness with `--target tauri`; mac-only ops stay
  gated off. On Linux the tray is a native menu (Tauri emits no Linux tray-click events → no
  popover; expected).
- **Driving the Preferences window.** Preferences is a dedicated native window (mac parity,
  no longer a dashboard modal), so it has its own IPC surface rather than `ui.modal`:
  `ui.show_preferences` (open/focus the lazy-created singleton; **it does NOT raise the
  dashboard**) with `ui.open_preferences` as the mac-named alias op; `prefs.dump` →
  `{visible, title, prefs}` (the window's UI truth — `visible`/`title` are Rust-side native
  window state, `prefs` is the React-reported `{sections, values, mode}` snapshot keyed under
  the **`preferences`** window label); `prefs.close` (hide, close-hides contract);
  `prefs.provider_modes` / `prefs.set_provider` (AWS/Docker Allow|Deny, ungated); and
  `prefs.remove_shed_rule {server, shed}` (the per-shed override row's remove button). Note
  `ui.modal` now only ever reports `create` | `launch` | `null` — Preferences is never a
  modal value.
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

## Driving against a roost-session (machine rows)

Since plan 013 the Tauri app's **machine** rows come from a `roost-session`, not from the RC
hub — one `RoostWatcher` per `machines:` entry, plus an implicit `localhost` host for the
machine the app is running on. Since plan 014 that watcher **does not poll**: it subscribes to
roost's leaseless observer event stream (session protocol 4) and emits a snapshot only when a
row actually changed, so there is **no poll knob any more** — the env var that used to turn the
cadence down was deleted from the app, from `shed_app::roost` and from `ui.py`, and a recipe
that still exports it is exporting nothing. Two env vars steer it:

| Variable | Where it is read | Effect |
|---|---|---|
| `SHED_TAURI_ROOST_SOCKETS` | `src-tauri/src/env.rs`, **test mode only** | Comma-separated `<machine>=<socket path>`. A named machine (including `localhost`) is dialled on that Unix socket instead of through roost's SSH client-bridge. Non-empty ⇒ **no** machine ever spawns ssh; an unmapped entry is a permanently-unreachable row. This is the app-level var — `ui.py`'s `subproc_env` sets or CLEARS it on every hermetic launch (never inherited from the parent shell), so nothing downstream of the harness can leak a stray value in. |
| `SHEDTEST_ROOST_SOCKETS` | `conftest.py`'s `_env_roost_sockets`, harness-level | Same `<machine>=<socket path>` shape, read once and passed explicitly as `roost_sockets` to the SESSION app fixture (`_app_session`) — the harness-level opt-in for pointing the pytest session app at a real daemon instead of `fake_roost.py`. A custom driver script (not going through `conftest.py`) must do the equivalent itself: read this var, parse it into a `{name: path}` map, and pass it as `ui.launch(..., roost_sockets=...)` — since the launch no longer inherits, nothing shows up unless the caller supplies the map explicitly. |

`localhost` is listed only once its socket has answered at least once (connect-if-present in
both directions), so an app with no session running shows no `localhost` row at all — that is
correct, not a bug. A configured machine named `localhost` wins over the implicit one, and
`machine.add {"name":"localhost"}` is refused.

### Agent lanes ride the same seam (plan 015)

`test_tauri_lane.py` drives the `lane.*` ops — an opencode transcript on a machine row — and
needs **no extra setup**: `fake_roost` serves a tab whose `ownership.metadata` carries
`server_url`, and `fake_opencode.py` (a port of the rc-parity fake, pin guard and all) answers
on a loopback port in the pytest process. The mapping in `SHED_TAURI_ROOST_SOCKETS` is what
makes the machine count as LOCAL, so the lane dials that URL directly and the suite spawns no
ssh. Against a real remote machine the same code takes the other branch — an
`ssh -N -L <local>:127.0.0.1:<reported>` child per session-port — which nothing hermetic can
exercise.

**Driving the transcript PANEL.** The panel opens from a card's Transcript affordance — a
click, which the harness does not have — so it has drivable ops on the `ui.show_create` /
`ui.show_launch` pattern: `ui.show_lane {machine, session_id}` mounts it (and raises the
window), `ui.close_lane` unmounts it, and `lane.dump` answers what it RENDERED (`null` once
no panel is mounted, which is deliberately not pane-gated — the panel can be open over any
pane). The panel itself calls `lane.open` on mount and `lane.close` on unmount, so mounting
one opens a lane and unmounting one closes it: a cell that leaves a panel up leaves a
subscription up. `lane.messages` (what the backend staged) and `lane.dump` (what is on
screen) are different questions — assert the one you mean. `SHED_LANE_SHOTS=<dir>` makes the
panel cells keep their `app.screenshot` PNGs there; unset, they still capture and assert one.

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
  -e SHEDTEST_ROOST_SOCKETS=localhost=/roost/roost.sock \
  --cap-add SYS_ADMIN --security-opt seccomp=unconfined --shm-size=1g \
  shed-tauri-linux:latest bash -c '…build + driver, as in the screenshot recipe below…'
```

- Export `SHEDTEST_ROOST_SOCKETS`, not the app-level `SHED_TAURI_ROOST_SOCKETS` — a hermetic
  launch always sets-or-clears the app-level var itself (`ui.py`'s `subproc_env`), so an
  inherited value from the container's own env never reaches the app; it must be supplied
  explicitly instead. Driving through pytest, `conftest.py`'s `_app_session` fixture reads
  `SHEDTEST_ROOST_SOCKETS` and passes it to `ui.launch(roost_sockets=...)`. A bespoke driver
  script (the screenshot recipe below) must do the same itself.
- Then `rc.list` carries the roost rows (`origin: "machine:localhost"`, `tab_id`, `attention`)
  and `capabilities["machine:localhost"]` — synthesized, `attach: "native-remote"`, so the card
  offers **no** terminal action and `terminal.preview {machine: …}` answers
  `not_enabled: terminal unavailable: attach is native-remote`.
- The container's uid is root (0) while the socket is owned by uid 1000; roost's socket is
  mode 0600, so either run with `--user 1000` or loosen the socket's mode for the run. If
  `rc.list` shows `localhost` absent, that is the first thing to check — an unconnectable
  socket is indistinguishable from "no session" by design.

### `tauri-test-linux` does not lint — CI does, on a Mac

`make tauri-test-linux` runs the Tauri crate's `cargo test`; it does **not** run
`cargo clippy`. CI's clippy for that crate lives in the **`tauri-mac`** job
(`make tauri-lint`), so a platform-independent lint — `type_complexity` on a test
helper, say — passes every Linux gate here and fails CI on macOS. Plan 014 lost a
round trip to exactly that.

Linux clippy on this crate cannot stand in for it: `src/tray.rs` and the
`zbus::proxy` macro in `src/approval.rs` carry three pre-existing Linux-only
findings, so `-D warnings` is red there before you change anything. Use it as a
*filter* rather than a gate — run it and check your own file is absent from the
output:

```bash
docker run --rm -v "$PWD:/repo:ro" \
  -v shed-tauri-linux-cargo:/usr/local/cargo/registry \
  -v shed-tauri-linux-cargo-git:/usr/local/cargo/git \
  -v shed-tauri-linux-target:/target -w /repo -e CARGO_TARGET_DIR=/target \
  shed-tauri-linux:latest bash -lc 'set -e; mkdir -p /work; \
    tar -C /repo --exclude=.git --exclude=target --exclude=node_modules \
      -cf - crates desktop/tauri desktop/tools desktop/Resources \
      desktop/pyproject.toml desktop/uv.lock | tar -C /work -xf -; \
    cd /work/desktop/tauri/src-tauri && cargo clippy --locked --all-targets 2>&1 \
      | grep -E "^ *--> " '
```

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

**Unlike Linux, this baseline is CLEAN at `origin/main`** — there is no `tray.rs`/`zbus::proxy`
pre-existing-findings exemption on macOS (those are Linux-only code paths). So on a Mac, run
clippy with `-D warnings` as a real gate, not a filter: if it is red, the finding is yours.

### The daemon must speak session protocol 4

Since plan 014 shed pins `roost-ipc` at `c67ac27b6a85dbee0871f32d49c1566cc068d1c8` and
`Conn::session_identify` **refuses a mismatch by name** rather than limping — a protocol-2
daemon (anything before roost's R1) reads as an unreachable machine row whose detail is
`ProtocolMismatch { theirs: 2, ours: 4 }`. That is correct, not a bug: the lease semantics
changed in both directions (`events.subscribe` stopped taking a lease, `tab.write` started
requiring one), so limping would mean guessing.

Build one from roost's tree at the pinned sha into a scratch checkout of its own — a **release**
build, so the daemon's socket lands under the non-`-dev` `roost-session` directory (the `-dev`
trap in `crates/CLAUDE.md`), and outside roost's own working tree so it survives whatever branch
that is on:

```bash
git -C ~/projects/roost archive c67ac27b6a85dbee0871f32d49c1566cc068d1c8 \
  | (mkdir -p ~/.cache/shed-plan014/roost-c67ac27 && tar -x -C ~/.cache/shed-plan014/roost-c67ac27)
cd ~/.cache/shed-plan014/roost-c67ac27
cargo build --release -p roost-session -p roost-cli   # roost-cli's binary is `roostctl`
# → target/release/{roost-session,roostctl}
```

(roost's build needs libghostty-vt; follow its own README if the link step complains.) Both
`SHED_TAURI_ROOST_SESSION_BIN` (the `real_roost` smoke below) and the mount-the-host-socket
recipe above want that binary. If a machine row comes up unreachable with a protocol mismatch,
the daemon is the old one — rebuild rather than un-pinning shed.

### The `real_roost` pytest smoke (no host socket to mount)

`test_tauri_machines.py::test_a_real_roost_session_answers_the_client` is a simpler
alternative to the mount-the-host-socket recipe above: it spawns its OWN jailed
`roost-session` daemon inside the container and discovers its socket by globbing the
daemon's runtime dir, so nothing needs mounting except the binary itself. Mount a built
`roost-session`'s directory read-only and point `SHED_TAURI_ROOST_SESSION_BIN` at it, then
run pytest with `-m real_roost` (the test is skipped, not failed, when the var is unset —
that's how CI stays roost-binary-free):

```bash
-v /path/to/roost/target/debug:/roost-bin:ro \
-e SHED_TAURI_ROOST_SESSION_BIN=/roost-bin/roost-session \
… uv run --group test pytest tools/shedtest/test_tauri_machines.py -m real_roost
```

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

- adds `/work/desktop/tools/shedtest` + `/work/desktop/tools/fake-host-agent` to
  `sys.path` and imports `ui`, `client`, `mockserver`, `fake_host_agent`;
- launches the mock + fake host-agent + the app hermetically via the harness's own
  `ui.launch` (throwaway HOME/XDG under `/work` or `/tmp`);
- seeds fixtures — `rc.inject_test` sessions for the Agents pane, `fake.emit_event`
  audit frames for Activity/Egress (mixed-ns: an `ssh-agent` + an `egress` event is
  the ns-filter fixture) — `navigate`s to the pane, drives sub-state
  (`egress.show`), calls `ui.set_appearance("dark")` for the dark shot, and captures
  via the `app.screenshot` op, writing each PNG under `/out`.

`app.screenshot` on the Xvfb (X11) leg shells out to `scrot`, falling back to
ImageMagick's `import -window root` if `scrot` is absent or fails (the tool order in
`src-tauri/src/screenshot.rs::capture` is `grim` → `scrot` → `import`; `grim` is
skipped without a `WAYLAND_DISPLAY`, so X11/Xvfb tries `scrot` then `import`). Either
way the PNG is the full display; the reported truth ops (`dashboard.dump` / `agents.dump` /
`egress.profiles` / `ui.badges` / `ui.computed_style`) stay the deterministic
assertions, the pixels are the eyeball.

## Gremlins

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
  twenty-six hunks across `approval.rs`, `broker.rs`, `lib.rs`, `live_activity.rs`,
  `screenshot.rs`, `termctl.rs`, `tray.rs`, `updater.rs`), so a whole-crate `cargo fmt --check`
  is red before you touch anything and a whole-crate `cargo fmt -- --emit files` would bury your
  diff in unrelated reflows. Check only the files you edited, against their own baseline:
  `git show HEAD:desktop/tauri/src-tauri/src/<f>.rs > /tmp/base.rs && rustfmt --check --edition
  2021 /tmp/base.rs` — if that is silent the file was clean, so format just yours with
  `rustfmt --edition 2021 src/<f>.rs`.
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
- **A module fixture named `fake` breaks the whole pytest session** → `conftest.py` owns a
  SESSION-scoped `fake` (the fake host-agent) that the autouse `_app_session` requests by name.
  A module-level `fake` shadows it and every test in the run dies with
  `ScopeMismatch: You tried to access the function scoped fixture fake with a session scoped
  request object` — but ONLY when that module is collected before the session fixture is first
  resolved, so it can pass in a full run and fail when the file is run alone. Name a per-module
  double something else (`fake_roost`, `roost`, …).
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
  stays dark-on-dark. In the Preferences window every dropdown (terminal preset, SSH policy)
  uses the dialog kit's `Select` (`components/dialog`, `appearance-none` + its own caret), NOT
  a bare `<select>`. Reach for `Select` when adding any picker to the Tauri UI, or the dark
  screenshot is unreadable (`PreferencesWindow.tsx`).
- **`policy.set` resets the ENGINE but not the coordinator's `extra_rules`** → the harness's
  per-test `policy.set` reset replaces the policy *engine's* rules, but the coordinator's
  persisted per-shed `extra_rules` survive it and get recomposed into the engine on the next
  `rebuild_policy` (a persisted approval decision from an earlier test resurrects there). So
  the absolute shed-rule count is NOT yours to pin — write per-shed-rule assertions
  **engine-relative** (`prefs.dump`'s `shed_rules_count` == `len(policy.list scope=="shed")`,
  and filter `policy.list` to YOUR shed), never against a fixed number. `test_preferences_shed_rules`
  is the pattern.
- **No per-window screenshot capture exists** → `app.screenshot` grabs the whole X display
  (`screenshot.rs::capture`, `grim`→`scrot`→`import`), not a chosen window, and the Preferences
  window is fixed-size (520×640, not resizable). Its below-the-fold content can't be
  photographed, so don't try to prove a lower section by pixels — trim the fixture so the
  section you care about sits above the fold (e.g. an `always-deny` SSH policy hides both the
  Duration and Method rows — `usesDuration`/`prompts` are false — collapsing the SSH card), and
  rely on `prefs.dump`'s reported `sections`/`values` for the logical assertions. Pixels are the
  eyeball; `prefs.dump` is the truth.

## Native run on a Mac (quick UI-comparison loop)

`make -C desktop tauri-run` builds + launches the Tauri client natively via Homebrew WebKitGTK
— useful to eyeball the Linux UI against the Swift app without Docker. It is **not** the render
gate; still run `tauri-build-linux` before trusting a shared/Linux change.

## When you hit a NEW rough edge

Update this skill whenever you hit a new rough edge it doesn't cover.
