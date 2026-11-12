---
name: shedtest-mac
description: Run and debug the shed desktop app (the Tauri client) on macOS — the hermetic pytest harness that drives the real app over its IPC socket on WKWebView, the Rust core + Tauri crate unit tests and clippy, the signed ShedDesktop.app/DMG bundle with its embedded Sparkle updater, and the bundle smoke CI runs. Use when asked to run the Mac desktop tests, verify a UI/behavior change on macOS by driving the app, debug a macOS e2e or bundle/Sparkle/signing failure, add a harness test, or reproduce the tauri-mac / core-mac CI jobs. The app lives under `desktop/`; its shared Rust core is the sibling `crates/` workspace. The Linux legs (WebKitGTK render gate, .deb, Docker) are the shedtest-linux skill.
---

# The desktop app on macOS: e2e, unit tests, bundle

The shed desktop app is the **Tauri** client (`desktop/tauri/`: a React/Vite/Tailwind UI in
`tauri/ui`, a Rust shell in `tauri/src-tauri`) over the shared Rust core in `crates/`. On
macOS it renders on **WKWebView**. Its north star is that the app is **drivable and observable
by an agent**: every change is verified by launching the real app and driving it over its IPC
socket, not by asking a human to click. This skill is that loop on a Mac. Run targets with
`make -C desktop <target>` (or the root `make desktop-<target>` passthrough).

## The fast path

```bash
npm --prefix desktop/tauri/ui ci   # once (and after package-lock.json changes)
make -C desktop build              # tauri-build: frontend bundle + the Tauri crate (Sparkle staged first)
make -C desktop test               # core-test + tauri-test: cargo unit tests, Rust core + Tauri crate
make -C desktop lint               # core-lint + tauri-lint: clippy -D warnings
make -C desktop e2e                # e2e-tauri: the hermetic pytest harness (no Xvfb needed on macOS)
```

That is what CI runs on `macos-15`: the **`tauri-mac`** job (`tauri-lint`, `tauri-test`, the
craze binaries, `SHED_TAURI_TEST_TIMEOUT_SCALE=4 make -C desktop e2e-tauri`, then the bundle
smoke below) and the **`core-mac`** job (`make -C desktop core-test core-lint` — the
`cfg(target_os = "macos")` paths in `crates/`, e.g. `shed-host-agent`'s Touch ID gate, only
compile there).

- **Craze cells.** `test_tauri_craze.py` drives the real craze hub and skips without the
  pinned binaries. `make craze-binaries` (repo root) builds them and prints
  `SHED_CRAZE_BIN_DIR=…`; export that before `make -C desktop e2e`. CI sets
  `SHED_CRAZE_REQUIRE=1`, which turns the skip into a failure.
- **One module / one test:** from `desktop/`,
  `uv run --group test pytest tools/shedtest/test_tauri.py -k preferences -q`. The harness
  builds nothing itself — run `make -C desktop tauri-build` after every change, Rust **or**
  UI (the frontend is embedded in the binary at compile time).
- **Timeouts** scale with `SHED_TAURI_TEST_TIMEOUT_SCALE` (CI uses 4). Use it for a loaded
  machine instead of editing a `wait_until`.

## How the harness launches the app

`desktop/tools/shedtest/conftest.py` starts an in-process mock shed-server
(`mockserver.py`) and a fake host-agent (`tools/fake-host-agent/`), then runs the debug binary
(`tauri/src-tauri/target/debug/shed-desktop-tauri`, override `SHED_TAURI_BIN`) with:

- `SHED_TAURI_TEST_MODE=1`, `SHED_TAURI_MOCK_BASE_URL=<mock>`,
  `SHED_TAURI_SHED_CONFIG=fixtures/config.yaml`;
- a throwaway dir as `HOME`, `XDG_RUNTIME_DIR`, `XDG_CONFIG_HOME` and `XDG_STATE_HOME`, so it
  never reads your `~/.shed`, your prefs or your real socket;
- `SHED_TAURI_HOST_AGENT_SOCKET` pointed at the fake.

`identify` is checked before any test runs (`test_mode`, the mock URL, `core: "rust"`,
`platform: "tauri"`), so a run that isn't hermetic fails fast instead of touching a real
server. Every app-level env knob is **set-or-cleared** (`ui.subproc_env`), so a value exported
in your shell never leaks into a run.

- **The app log** is `$TMPDIR/shed-tauri-ui.log` (stdout+stderr of the harness-launched
  binary; `SHED_TAURI_E2E_LOG_DIR` moves it) — it survives teardown. The app's own log file
  lands under the throwaway HOME at `Library/Logs/ShedDesktop/shed-desktop.log` and is
  deleted with it.
- **A module that needs a different config or env** launches its own instance with a
  separate runtime dir rather than relaunching the session app — `test_tauri_downhost.py`
  (`ui.subproc_env` + `ui.await_hermetic` + `ui.terminate`) is the worked example. Two
  instances coexist because each has its own socket under its own runtime dir.
- **Socket paths must stay short.** macOS `$TMPDIR` is long (`/var/folders/…`) and a Unix
  socket path has to fit `SUN_LEN` (104 bytes). Keep `mkdtemp` prefixes short; a "path too
  long" bind error at launch is this.

## Driving the app by hand

```bash
make -C desktop run    # launch against your REAL ~/.shed/config.yaml (not hermetic)
```

The socket is `$SHED_TAURI_SOCKET`, else `$XDG_RUNTIME_DIR/shed-tauri.sock`, else
`/tmp/shed-tauri-<uid>/shed-tauri.sock` — the last one on a stock Mac, where
`XDG_RUNTIME_DIR` is unset. Drive it with the Rust `shedctl` (run with no arguments it prints its usage; `raw <op>
--param k=v` reaches any op):

```bash
cargo run -q --manifest-path crates/Cargo.toml -p shedctl -- identify
cargo run -q --manifest-path crates/Cargo.toml -p shedctl -- raw ui.navigate --param pane=system
cargo run -q --manifest-path crates/Cargo.toml -p shedctl -- dashboard dump
```

or from Python (`desktop/`): `uv run --group test python -c "import sys; sys.path.insert(0,
'tools/shedtest'); import ui; c = ui.make_client(); print(c.identify())"`. The op catalog is
the `match` in `desktop/tauri/src-tauri/src/ipc.rs`; `docs/desktop/ipc.md` documents it.

**Read state, don't screenshot.** On macOS `app.screenshot` shells out to `screencapture`,
which needs a Screen-Recording (TCC) grant that a harness-launched or agent-run binary doesn't
have, so it exits 1. Every screenshot assertion **skips on Darwin by design**; the Linux/Xvfb
render gate (`shedtest-linux`) is the pixel gate. On a Mac, assert on the UI-truth dump ops
instead: `dashboard.dump`, `ui.current_pane`, `ui.modal`, `ui.badges`, `ui.computed_style`,
`prefs.dump`, `tray.dump`, `agents.dump`, `launch.dump`, `sidebar.dump`, `machines.dump`,
`lane.dump`, `toast.dump`. They report what the frontend rendered, not just what the backend
holds.

## macOS-only behavior the e2e covers

- **The tray popover** (`test_tray_popover_drivable`): on macOS the tray icon opens a second,
  transparent webview. OS tray clicks aren't hermetic, so `tray.show` / `tray.toggle` /
  `tray.hide` / `tray.dump` are the drivability surface. Linux has a native menu and no
  popover, so that test skips there.
- **The updater** (`test_updater_status_and_check_disabled`): `updater.status` →
  `{os, enabled, reason, instantiated}`. Under the harness the Sparkle plugin is **never
  registered**, so on macOS it reports `enabled=false, instantiated=false,
  reason="test_mode"`, and `updater.check` returns `updater_disabled:test_mode`.
- **Launch at login** (`test_loginitem_probe`): test mode round-trips the toggle without
  writing a real LaunchAgent (a real write would hit TCC).
- **Approvals** (`test_approvals.py`, `test_tauri_broker.py`): the Touch ID gate is never
  shown under the harness — the gate is exercised through the fake host-agent and the
  in-process broker. The native `LAContext` error mapping is covered by the Tauri crate's unit tests
  (`approval.rs`, macOS-only).

## The bundle + DMG (the shipped artifact)

```bash
make -C desktop tauri-bundle-mac   # desktop/build/ShedDesktop.app, signed (ad-hoc by default)
make -C desktop tauri-dmg-mac      # + desktop/build/ShedDesktop-<ver>.dmg
desktop/scripts/bundle-tauri-mac.sh --debug   # the debug bundle CI's smoke builds
```

`SHED_DESKTOP_DEVELOPER_ID_IDENTITY` picks a real signing identity (else ad-hoc `-`);
`SHED_DESKTOP_ALLOW_UNSIGNED=1` continues past codesign failures. **Unset
`APPLE_SIGNING_IDENTITY` / `APPLE_CERTIFICATE`** — the script refuses to run with them set,
because the Tauri bundler would sign Sparkle's helpers in the wrong order.

To reproduce CI's **bundle smoke** locally, run `bundle-tauri-mac.sh --debug` and check what
the `tauri-mac` job's "Bundle smoke" step asserts (`.github/workflows/ci.yml`): the bundle id
is `ai.stridelabs.ShedDesktop`; `CFBundleExecutable` names a real Mach-O; `CFBundleVersion`
matches `tauri.conf.json`; `Sparkle.framework` is embedded; `Downloader.xpc`'s entitlements
are byte-identical to the pristine dist copy; `SUFeedURL` / `SUPublicEDKey` /
`SUEnableAutomaticChecks` are present and `SUPublicEDKey` equals the pinned key; and
`codesign --verify --strict` passes. A second build with
`SHED_TAURI_CONFIG_OVERRIDE='{"version":"0.0.0-rc.1"}'` proves a prerelease suffix survives
plist stamping.

**Never change the bundle id, `SUFeedURL` or `SUPublicEDKey`** (`tauri/src-tauri/Info.plist`,
`tauri.macos.conf.json`). Every installed copy — including installs from before 0.9.0, when
the mac client was a Swift app — updates against them.

## Sparkle + signing rough edges

- **Every mac Tauri build needs `Sparkle.framework` staged first.** `tauri.macos.conf.json`
  embeds it and the `tauri-plugin-sparkle-updater` crate's `build.rs` **panics if it is
  absent**. `make -C desktop sparkle-framework` stages it (`scripts/fetch-sparkle.sh`, pinned,
  checksum-verified, idempotent); on Darwin it is an automatic prerequisite of `tauri-build` /
  `tauri-lint` / `tauri-test` / `tauri-run`. A bare `cargo build` in `tauri/src-tauri` skips
  that — stage it first. The framework and `.sparkle-dist/` are gitignored.
- **Unbundled binaries load Sparkle via a debug-only rpath** injected by `build.rs`, so
  `cargo test`, `make tauri-run` and the harness binary link the staged framework. Release
  bundles use the embedded `@executable_path/../Frameworks` copy.
- **Sign inner → outer, never `--deep`.** `bundle-tauri-mac.sh` signs `Installer.xpc`,
  `Downloader.xpc` (`--preserve-metadata=entitlements`), `Autoupdate`, `Updater.app`, the
  framework, then the app. A wrong order notarizes clean and **only fails when Sparkle applies
  an update** — PR CI cannot catch that; the rc rehearsal in `desktop/RELEASING.md` can.
- **Sparkle ≥ 2.6's `Downloader.xpc` has EMPTY entitlements** (its sandbox was removed,
  Sparkle #2511). Never assert `app-sandbox` on it, and never inject the app's.
- **Verifying a real Sparkle check is TCC-bound** for screenshots; use the log instead:
  `log show --predicate 'subsystem == "org.sparkle-project.Sparkle"' --last 5m` (look for the
  `.sessionInProgress` line).
- **`npm --prefix ui exec tauri` keeps the shell's cwd** — the Tauri CLI walks up from cwd to
  find `src-tauri/tauri.conf.json`, so `bundle-tauri-mac.sh` runs it from `desktop/tauri`.
- **The Tahoe glass icon needs full Xcode** (`actool`). With only the Command Line Tools the
  script falls back to the flat `icon.icns` and logs it; the bundle still launches.

## Gotchas

- **`mise ERROR No version is set for shim: cargo`** — put `~/.cargo/bin` first on `PATH`;
  both workspaces pin `stable` through `rust-toolchain.toml`.
- **`tsc: not found` from `tauri-ui-build`** — the frontend deps aren't installed; run
  `npm --prefix desktop/tauri/ui ci`.
- **The mac dev config dir** for a hand-run app is
  `~/Library/Application Support/ai.stridelabs.ShedDesktop`; the Linux identity
  (`ai.stridelabs.shed-desktop`) is different. The harness is unaffected (throwaway HOME).
- **`hdiutil detach` can report a different disk than `attach` printed** — check
  `hdiutil info` when scripting DMG mount/unmount.
- **`cp -R` preserves read-only directory bits** — `chmod -R u+w` a copied signed `.app`
  before `rm -rf`, or the delete fails.

## When you hit a NEW rough edge

Update this skill in the same PR whenever you hit a rough edge it doesn't cover.
