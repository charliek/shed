# Test automation

The app is built to be driven and observed by an automated agent, so changes are verified
by running the real app — not by a human clicking. Three layers cover it.

## Unit tests (cargo)

Pure logic lives in the Rust crates and is unit-tested there: config parsing, model decoding
against real API shapes (including `{"sheds": null}` and mixed timestamp formats), the SSE
parser, the approval policy engine and coordinator, the lane adapters, and the Tauri crate's
own IPC doors and seams. No running UI required.

```bash
make -C desktop test        # core-test + tauri-test
make -C desktop lint        # core-lint + tauri-lint (clippy, -D warnings)
make -C desktop core-linux  # shed-core on ubuntu:24.04, in Docker
```

The frontend's pure helpers have their own tests, run by `npm run build` in
`desktop/tauri/ui` (and so by every target that builds the bundle).

## Functional harness (`tools/shedtest`, pytest)

Drives the **real** app over the [IPC socket](ipc.md) and asserts on what it rendered via the
UI-truth ops (`dashboard.dump`, `agents.dump`, `prefs.dump`, …) and on backend state
(`sheds.list`, `approvals.list`, …). It is fully **hermetic**:

1. A stdlib `ThreadingHTTPServer` (`mockserver.py`) stands in for `shed-server` on an
   ephemeral `127.0.0.1` port, serving fixture JSON the test can mutate directly, and a fake
   host agent (`tools/fake-host-agent`) backs the approval gate.
2. The harness launches the app in test mode (`SHED_TAURI_TEST_MODE=1`,
   `SHED_TAURI_MOCK_BASE_URL=…`, `SHED_TAURI_SHED_CONFIG` pointing at a fixture config) with a
   throwaway `HOME` and `XDG_RUNTIME_DIR`, so every HTTP client is redirected to the mock, the
   socket (`$XDG_RUNTIME_DIR/shed-tauri.sock`) and single-instance lock are private to the
   run, and nothing leaves the box.
3. `identify` is checked up front to confirm the run is actually hermetic.
4. Tests use condition-waits (`wait_until`), never sleeps; `SHED_TAURI_TEST_TIMEOUT_SCALE`
   multiplies the timeout budgets for slower CI runners.

```bash
make -C desktop e2e-tauri   # alias: make -C desktop e2e
```

`e2e-tauri` builds the app, then runs `uv run --group test pytest tools/shedtest -q` from
`desktop/`. It needs a display: native on macOS (WKWebView), Xvfb on Linux (wrap the run in
`xvfb-run -a`, or use the Docker render gate below). `SHED_TAURI_BIN` points the harness at a
prebuilt binary.

Preferences is a dedicated native window, not a dashboard modal — `ui.show_preferences`
(alias `ui.open_preferences`) opens or focuses the lazily created window without raising the
dashboard, so `ui.modal` only ever reports `create`, `launch`, or `null`. The window's UI truth
is `prefs.dump` → `{visible, title, prefs}`: `visible`/`title` are the native window state,
and `prefs` is the window's own reported `{sections, values, mode}` snapshot — the surface a
test asserts (gated section visibility, persisted values, theme) instead of pixels, the same
UI-truth pattern as `dashboard.dump`.

### Simulating a down (unreachable) host

The shared session redirects every configured server to the one in-process mock, so a
per-host error row (an unreachable host in the Egress / System panes) can't appear there. To
exercise it, point a **dedicated** fixture config at an extra server and name it in
`SHED_TAURI_MOCK_UNREACHABLE_HOSTS` (comma-separated server names, parsed only in test mode):
the backend points those hosts at a closed port (`http://127.0.0.1:1`, a deterministic
connection refusal) while the rest still hit the mock. `test_tauri_downhost.py` is the
pattern — it launches its own throwaway app instance (distinct `HOME`/`XDG_RUNTIME_DIR` →
distinct socket and single-instance lock, so it coexists with the session app) against
`fixtures/config-downhost.yaml`. Keep the down host out of the shared `fixtures/config.yaml`
so the healthy-path tests and screenshots are undisturbed.

## The Linux render gate + `.deb`

The app's shipped Linux WebView is WebKitGTK, so a Linux-only render gate runs the harness on
`ubuntu:24.04` / WebKitGTK 2.44 under Xvfb, in Docker:

```bash
make -C desktop tauri-build-linux   # render gate (WebKitGTK 2.44, Xvfb) — run for any shared/Linux change
make -C desktop tauri-test-linux    # the Tauri crate's Linux-only approval-seam tests (polkit gate)
make -C desktop deb-validate        # build the .deb + install-validate in a clean container
```

## CI

| Job | Runs |
|---|---|
| `core-mac` | `core-test` + `core-lint` on macOS |
| `core-linux` | the Rust core's tests and clippy on Linux |
| `tauri-mac` | `tauri-lint`, `tauri-test`, the hermetic `e2e-tauri` on WKWebView (`SHED_TAURI_TEST_TIMEOUT_SCALE=4`), and a bundle smoke of `bundle-tauri-mac.sh` (Sparkle staging, signing and `Info.plist` assertions) |
| `tauri-linux` | `tauri-test-linux` and the `tauri-build-linux` render gate |

## Screenshots

The app has no in-process WebKitGTK capture, so `app.screenshot` shells out to a platform
tool (`grim`, `scrot` or ImageMagick `import` on Linux, `screencapture` on macOS) and captures
the screen. On macOS that path is gated by the Screen Recording permission and fails in an
agent or headless session, so the screenshot assertions **skip on macOS by design** — the
Linux render gate under Xvfb is the visual gate. An agent can read the PNG directly to eyeball
a change:

```bash
shedctl raw ui.show_window
shedctl screenshot --out /tmp/shot.png
```

`ui.set_appearance {mode: light|dark}` drives the dashboard's light/dark mode
deterministically (the reported `ui.computed_style` `mode` echoes it), so a dark-mode capture
doesn't depend on toggling the header.
