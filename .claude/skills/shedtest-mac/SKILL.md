---
name: shedtest-mac
description: Run and debug the shed desktop app's macOS end-to-end tests — the hermetic pytest harness that drives the real app over its IPC socket and captures in-process screenshots, plus the Swift unit tests and the Rust-core parity leg. Use when asked to run the Mac desktop tests, verify a UI/behavior change by driving the app, debug an e2e failure, add a harness test, or check screenshot/IPC ops. The app lives under `desktop/`; its shared Rust core is the sibling `crates/` workspace. The Linux client is the Tauri app — see the shedtest-linux skill.
---

# macOS desktop app end-to-end + unit tests

The shed desktop app's north star is that the app is **drivable and observable by an
agent**: every change is verified by launching the real app and driving it over the
IPC socket (and reading in-process screenshots), not by asking a human to click. This
skill is that loop. The app lives under `desktop/`; run its targets with
`make -C desktop <target>` (or the root `make desktop-<target>` passthrough).

## The fast path

```bash
make -C desktop build      # Rust core (xcframework) + swift build — run before a bare swift build/test
make -C desktop test       # Swift unit tests (ShedKit) + the Rust FFI canary
make -C desktop e2e-ci     # hermetic e2e: bundles the app, TEST_MODE + in-process mock, fresh
```

- **`make -C desktop e2e-ci`** is CI parity and the one to trust: it sets
  `SHED_DESKTOP_TEST_MODE=1` and points every HTTP client at an **in-process mock
  shed-server** (`desktop/tools/shedtest/mockserver.py`), so no real shed-server is
  touched. `identify` is checked up front to confirm hermeticity.
- **`make -C desktop e2e`** drives a running/auto-launched app for quick local iteration.
- The harness lives in `desktop/tools/shedtest/` (pytest); it speaks the same JSON IPC
  protocol as `shedctl`.

## Driving the app by hand (shedctl)

The bundle ships the CLI driver at
`desktop/build/ShedDesktop.app/Contents/Resources/bin/shedctl`:

```bash
make -C desktop run                                                # build + launch the bundle
desktop/build/ShedDesktop.app/Contents/Resources/bin/shedctl ui show-window
desktop/build/ShedDesktop.app/Contents/Resources/bin/shedctl screenshot --surface window --out /tmp/s.png
desktop/build/ShedDesktop.app/Contents/Resources/bin/shedctl sheds list
```

The screenshot op renders in-process (no screen-recording TCC grant needed), so
it works headless in CI.

## Conventions that keep the harness reliable

- **Condition-waits, never sleeps.** Use `wait_until` / `wait_alive` (readiness is
  gated on the app answering, not on a timer).
- **Hermetic.** The harness launches the app with `SHED_DESKTOP_TEST_MODE=1` +
  `SHED_DESKTOP_MOCK_BASE_URL` so all HTTP hits the in-process mock. Never point a
  test at a real server. Fixtures live in `desktop/tools/shedtest/fixtures/`.
- **New UI ⇒ new IPC op + harness coverage.** When you add UI or behavior, add the
  IPC op that lets an agent observe/drive it, and a pytest that exercises it. That
  is the definition of done here, not a manual click-through.

## Rust-core parity leg

The shed-server protocol path can run through the shared Rust core in `crates/`
(`SHED_DESKTOP_RUST_CORE`, the macOS **default**). `identify.core` reports
`rust|swift`, and `wait_alive` asserts it, so a silent fallback fails the run rather
than passing falsely:

```bash
# the same hermetic suite through the Rust core (the default):
SHED_DESKTOP_RUST_CORE=1 make -C desktop e2e-ci
# force the legacy Swift URLSession path (rollback escape hatch):
make -C desktop e2e-swift        # sets SHED_DESKTOP_RUST_CORE=0
```

If a run reports `identify.core=rust` but a host silently fell back to Swift,
that's a bug to fix (adapter construction must fail loudly, not `try?` away).

## Real-launch smokes (the paths the hermetic suite gates off)

```bash
make -C desktop smoke-real-launch    # non-test launch survival (real notification path)
make -C desktop smoke-launch-window  # user launch opens the dashboard; reopen reaches it
make -C desktop smoke                # drive the app + capture labeled screenshots
```

## Gotchas

- Run `make -C desktop build` (or `make -C desktop core`) before a bare
  `swift build`/`swift test` — the Swift package links a static xcframework
  generated from the Rust core in `crates/`, output to `desktop/artifacts/`, so that
  path must exist first. `bundle.sh` / `make -C desktop e2e-ci` build it themselves.
- Do **not** run `swift format -i` on the whole tree reflexively — match the
  existing 4-space style; formatting churn muddies review.
- The control socket + lock live under `~/Library/Caches/ShedDesktop/` and are NOT
  moved by `SHED_DESKTOP_STATE_DIR`, so the harness + a dev session agree on them.
- **Tauri screenshots on macOS are Screen-Recording-TCC-gated.** The mac app's
  `app.screenshot` renders the content view in-process (no permission needed), but
  the **Tauri** client (`--target tauri` on a Mac) has no in-process WebKitGTK
  capture — it shells out to `screencapture`, which exits 1 without a Screen-
  Recording grant in an agent/headless session. Those Tauri screenshot assertions
  **skip on Darwin by design** (see `test_tauri.py`); for real visual capture of the
  Tauri client run the `shedtest-linux` render gate under Xvfb (`ui.set_appearance`
  makes the dark shot deterministic there).

## Tauri mac packaging + Sparkle updater (rough edges)

The Tauri client now ships a **macOS** DMG with an embedded real Sparkle updater
(`make -C desktop tauri-dmg-mac`; the updater is the tray popover's "Check for Updates…"
row, drivable over IPC). Mac-specific gotchas:

- **Every mac Tauri build needs `Sparkle.framework` staged first.** The overlay
  `tauri.macos.conf.json` embeds it and the `tauri-plugin-sparkle-updater` crate's
  `build.rs` **panics at build time if it is absent**. `make -C desktop sparkle-framework`
  stages it (`scripts/fetch-sparkle.sh`, pinned Sparkle 2.8.1, checksum-verified,
  idempotent); on Darwin it is an **automatic prerequisite** of `tauri-build` /
  `tauri-lint` / `tauri-test` / `tauri-run`. The framework + `.sparkle-dist/` bin tools
  are **gitignored** and `--exclude`'d from the Docker tar legs.
- **Unbundled binaries load Sparkle via a debug-only rpath** injected by `build.rs`, so
  `cargo test` / `make tauri-run` / the raw harness binary can link the staged framework.
  **Release DMGs stay clean** — they use the bundled `@executable_path/../Frameworks` copy.
- **Verifying the Sparkle dialog on a dev Mac is TCC-bound** (no in-process WebKitGTK
  capture, and `screencapture` needs a Screen-Recording grant). The reliable signal is the
  log, not a screenshot: `log show --predicate 'subsystem == "org.sparkle-project.Sparkle"'`
  (look for the `.sessionInProgress` line) confirms the check ran and fronted a session.
- **`npm --prefix ui exec tauri` keeps the shell's cwd** (load-bearing — the Tauri CLI
  walks up from cwd to find `src-tauri/tauri.conf.json`; `bundle-tauri-mac.sh` runs it from
  `desktop/tauri`). A wrong cwd fails to locate the config.
- **Sparkle ≥ 2.6's `Downloader.xpc` has EMPTY entitlements** (its sandbox was removed,
  Sparkle #2511). Never assert `app-sandbox` on it; the re-sign preserves the dist's empty
  entitlements and must not inject the app's.
- **The updater is drivable over IPC:** `updater.status` → `{os, enabled, reason,
  instantiated}` and `updater.check`. Under the harness the plugin is **never registered**
  (Swift parity), so status is `enabled=false, instantiated=false`, `reason="test_mode"` on
  mac (`"linux_apt"` on the Linux render gate); `updater.check` returns the deterministic
  `updater_disabled:<reason>` error and never crashes. `test_tauri.py` pins both.
- **The mac dev config dir moved** to `~/Library/Application Support/ai.stridelabs.ShedDesktop`
  (identity alignment for the Swift→Tauri hop). The harness is unaffected — it runs under a
  throwaway HOME — but a hand-run dev bundle reads/writes there now.
- **`hdiutil detach` can report a different disk number than `attach` printed** — verify the
  actual device with `hdiutil info` rather than trusting the attach output when scripting
  DMG mount/unmount.
- **`cp -R` preserves read-only directory bits** — after copying a signed/notarized `.app`
  around, `chmod -R u+w` it before `rm -rf`, or the delete fails on the read-only dirs.

## When you hit a NEW rough edge

Update this skill whenever you hit a new rough edge it doesn't cover.
