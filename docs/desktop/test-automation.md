# Test automation

The app is built to be driven and observed by an automated agent, so changes are verified
by running the real app — not by a human clicking. Three layers cover the two clients.

## Unit tests (`swift test`)

Pure logic in `ShedKit`: config parsing, model decoding against real API shapes (including
`{"sheds": null}` and mixed timestamp formats), the SSE parser, the IPC envelope, the
remote-control classifier, and the approval policy engine. No running UI required.

```bash
make -C desktop test        # builds the Rust core first, then swift test
```

The Rust core has its own workspace tests (`make -C desktop core-test` /
`core-lint`), and `core-linux` runs `shed-core` on Linux in Docker.

## Functional harness (`tools/shedtest`, pytest)

Drives a **real** client over the [IPC socket](ipc.md) and asserts on state via
`ui.state` / `sheds.list`, plus screenshot checks. One harness drives both clients through
`--target mac|tauri` (default `mac`). It is fully **hermetic**:

1. A stdlib `ThreadingHTTPServer` (`mockserver.py`) stands in for `shed-server` on an
   ephemeral `127.0.0.1` port, serving fixture JSON the test can mutate directly.
2. The harness launches the app with test mode on (`SHED_DESKTOP_TEST_MODE=1` +
   `SHED_DESKTOP_MOCK_BASE_URL=…`, or the `SHED_TAURI_*` equivalents), so every HTTP client
   is redirected to the mock — no real shed-server is touched and nothing leaves the box.
3. `identify` is checked up front to confirm the run is actually hermetic (and, for the mac
   client, that `identify.core` is the expected `rust`/`swift` backend).
4. Tests use condition-waits (`wait_until`), never sleeps; the timeout budget scales from
   `SHED_DESKTOP_TEST_TIMEOUT_SCALE` (mac) / `SHED_TAURI_TEST_TIMEOUT_SCALE` (tauri) for
   slower CI runners.

```bash
make -C desktop e2e-ci      # mac: fresh, hermetic, mock-backed (Rust core, default)
make -C desktop e2e-swift   # mac: same, Rust core forced off (SHED_DESKTOP_RUST_CORE=0 leg)
make -C desktop e2e-tauri   # tauri: shared suite + test_tauri (needs a display; Xvfb on Linux)
```

## The Linux render gate + `.deb`

The Tauri client's real shipped WebView is WebKitGTK, so a Linux-only render gate runs the
`--target tauri` suite on `ubuntu:24.04` / WebKitGTK 2.44 under Xvfb, in Docker:

```bash
make -C desktop tauri-build-linux   # render gate (WebKitGTK 2.44, Xvfb) — run for any shared/Linux change
make -C desktop tauri-test-linux    # the Tauri crate's Linux-only approval-seam tests (polkit gate)
make -C desktop deb-validate        # build the .deb + install-validate in a clean container
```

## Screenshots

On the **mac** app `app.screenshot` renders a window's content view to a PNG in-process (no
screen-capture permission, works occluded/off-screen). The harness asserts the PNG decodes
and matches `app.window_metrics`; an agent can also read the PNG directly to eyeball a change.

```bash
shedctl ui show-window
shedctl screenshot --surface window --scale 2 --out /tmp/shot.png
```

The **Tauri** client has no in-process WebKitGTK capture, so its `app.screenshot` shells out
to a platform tool (`grim`/`scrot` on Linux, `screencapture` on macOS). On macOS that path is
Screen-Recording-TCC-gated and exits non-zero in an agent/headless session, so the Tauri
screenshot assertions **skip on Darwin by design** — the Linux render gate under Xvfb is the
visual gate. `ui.set_appearance {mode: light|dark}` drives the dashboard's light/dark mode
deterministically (the reported `computed_style.mode` echoes it), so a dark-mode capture
doesn't depend on toggling the header.
