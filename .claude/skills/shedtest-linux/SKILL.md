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

## How the Docker legs are wired (so failures make sense)

The `deb`, `tauri-build-linux`, and `tauri-test-linux` targets `tar` a **repo-root-relative**
layout (`crates desktop/tauri desktop/tools desktop/Resources …`) into `/work` inside the
container, so the Tauri crate's `../../../crates` path-deps resolve in the recreated layout
exactly as in the repo. The source is copied into a writable `/work` (not a read-only mount)
because Tauri's `build.rs` writes `gen/` next to `Cargo.toml`. Rust builds to a `/target`
volume so it never clobbers the mac target dir.

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
