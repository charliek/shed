# CLAUDE.md — working in `desktop/`

The shed desktop app: a native macOS menu-bar client (SwiftUI + AppKit) and a Tauri Linux
client, both thin shells over the shared Rust core in the sibling `crates/` workspace. This
file orients an AI assistant working in this subtree. The root `CLAUDE.md` owns the monorepo
layout + release model; `crates/CLAUDE.md` owns the core.

Run targets from here with `make <target>`, or from the monorepo root with `make -C desktop
<target>` / the `make desktop-<target>` passthrough.

## North star

The app is **drivable and observable by an automated agent** — a first-class feature, not a
test afterthought. Every change is verified by running the real app and driving it over the
IPC socket (and reading in-process screenshots), not by asking a human to click. When you add
UI or behavior, add the IPC op + harness coverage that lets you verify it. That is the
definition of done here.

## Architecture

Core/UI split (see `docs/desktop/architecture.md`):

- `Sources/ShedKit/` — core, no SwiftUI: HTTP (`ShedServerClient`) + SSE (`SSEParser`)
  clients, models (`Models.swift`, `ShedConfig.swift`), the IPC server (`IPC/`),
  `Screenshot.swift`, the **Approval** subsystem (`Approval/`: `HostAgentClient`,
  `PolicyEngine`, `AuditStore`, `NotificationPresenter`), behind the `UiBridge`/`ShedBackend`
  seam.
- `Sources/ShedDesktopUI/` — SwiftUI views (Sheds/Approvals/Agents/Activity/System/
  Preferences/menu) + `AppState` (the observable view-model).
- `Sources/ShedDesktopApp/` — `@main`, `AppModel` (host poller + windows + IPC handler +
  approval coordinator), `IPCHandlerImpl`, `SystemNotificationPresenter`, the Sparkle updater.
- `Sources/shedctl/` — the Swift CLI driver for the socket (bundled in the `.app`).
- **The shared Rust core lives in `../crates/`** (`shed-core` + `shed-app` + `shed-core-ffi` +
  the Rust `shedctl`), NOT here. The macOS app links a static xcframework generated from it;
  `SHED_DESKTOP_RUST_CORE=0` forces the legacy Swift `URLSession` path (a rollback escape
  hatch).
- `tauri/` — the **Tauri Linux client** (React/Vite/Tailwind on WebKitGTK), its **own
  standalone Cargo workspace** (`tauri/src-tauri`) so its WebKitGTK/Tauri deps never bleed
  into `../crates/`. Takes `shed-core` + `shed-app` as cross-workspace path-deps
  (`../../../crates/…`). The shipped Linux `.deb` (`shed-desktop`, bin `shed-desktop-tauri`)
  is built from it via nfpm (`linux/scripts/build-deb.sh`).
- `tools/shedtest/` — ONE pytest harness + in-process mock shed-server, driving BOTH UIs via
  `--target mac|tauri` (default `mac`).

The dedicated GTK client that used to ship the `.deb` has been **retired** — Tauri replaced
it. There is no `--target gtk` and no `shed-gtk` crate.

## The change loop (macOS)

```bash
make build && make test      # compile (Rust core + Swift) + unit tests
make bundle                  # build/ShedDesktop.app (ad-hoc signed)
make e2e-ci                  # hermetic functional harness (mock shed-server)
# Eyeball a change against a running app:
make run
build/ShedDesktop.app/Contents/Resources/bin/shedctl ui show-window
build/ShedDesktop.app/Contents/Resources/bin/shedctl screenshot --surface window --out /tmp/s.png
```

The harness is hermetic: it launches the app with `SHED_DESKTOP_TEST_MODE=1` +
`SHED_DESKTOP_MOCK_BASE_URL`, so all HTTP hits the in-process mock and no real shed-server is
touched. `identify` is checked up front to confirm hermeticity (and, on mac, that
`identify.core` is the expected `rust`/`swift` backend). Use condition-waits (`wait_until`),
**never sleeps**. Fixtures live in `tools/shedtest/fixtures/`.

## Build-order gotchas (read these)

- **`make core` (or `make build`) MUST run before any bare `swift build`/`swift test`.** The
  Swift package links a static xcframework generated from the Rust core; that `.binaryTarget`
  path must exist first. `bundle.sh` / `make e2e-ci` build it themselves.
- **Generated Rust-core artifacts land under `desktop/artifacts/`** (gitignored), NOT under
  `crates/` — SwiftPM requires target paths inside the package root. `scripts/build-core.sh`
  writes them (plus a staleness stamp) there.
- **NEVER `swift format -i` the whole tree** reflexively — match the existing 4-space style;
  formatting churn muddies review.

## The Linux client (Tauri) + the `.deb`

```bash
make tauri-run           # build + launch the Tauri client natively (Mac Homebrew WebKitGTK / Linux)
make e2e-tauri           # hermetic Tauri pytest (--target tauri; needs a display, Xvfb on Linux)
make tauri-build-linux   # the WebKitGTK render gate: --target tauri on ubuntu:24.04 / WebKitGTK 2.44 (Docker, Xvfb)
make tauri-test-linux    # the Tauri crate's Linux-only approval-seam tests (polkit gate; Docker)
make core-linux          # shed-core cargo test/clippy on Linux (Docker)
make deb / make deb-validate   # build the .deb / build + install-validate in a clean ubuntu:24.04 container
```

The Docker legs (`deb`, `tauri-build-linux`, `tauri-test-linux`) tar a **repo-root-relative**
layout (`crates desktop/tauri desktop/tools …`) into `/work` inside the container so the
`../../../crates` Tauri path-deps resolve in the recreated layout. The render gate needs
`--cap-add SYS_ADMIN --security-opt seccomp=unconfined` (WebKitGTK's web-process bubblewrap
sandbox needs user namespaces). `WEBKIT_DISABLE_DMABUF_RENDERER=1` is set in the image (no GPU
in Docker). **Run `make tauri-build-linux` (the render gate) for any shared/Linux change** —
the mac WKWebView e2e alone can miss Linux-only breaks. On Linux the tray is a native menu
(Tauri emits no Linux tray-click events → no popover; expected).

## Socket paths

- **mac IPC:** `~/Library/Caches/ShedDesktop/shed-desktop.sock` (override `SHED_DESKTOP_SOCKET`).
  Hermeticity: `SHED_DESKTOP_TEST_MODE` / `SHED_DESKTOP_MOCK_BASE_URL` / `SHED_DESKTOP_SHED_CONFIG`.
- **Tauri IPC:** `$XDG_RUNTIME_DIR/shed-tauri.sock` (override `SHED_TAURI_SOCKET`; `/tmp`
  fallback). Hermeticity: `SHED_TAURI_TEST_MODE` / `SHED_TAURI_MOCK_BASE_URL` /
  `SHED_TAURI_SHED_CONFIG`. In containers, point the harness at a prebuilt binary with
  `SHED_TAURI_BIN`.
- **host-agent (mac):** `~/Library/Application Support/shed/host-agent.sock`.

## Conventions

- Swift 6 strict concurrency. Keep `ShedKit` free of SwiftUI. The IPC handler is an actor;
  reach the app via `@MainActor` op methods that return only `Sendable` results.
- Default to no comments; add one only when the *why* is non-obvious.
- Decode defensively against real shed-server shapes (`{"sheds": null}`, omitted fields,
  mixed timestamp formats) — there are unit tests pinning these.
- Don't expose any shed-server further; a reachable server is trusted by the network, not the
  app.

## Skills + docs

The end-to-end validation loops are documented as skills (update them when you hit a new
rough edge): `.claude/skills/shedtest-mac` (the macOS app loop) and
`.claude/skills/shedtest-linux` (the Tauri/Linux loop). The user-facing docs live under
`docs/desktop/` — the [Test automation](../docs/desktop/test-automation.md) page mirrors the
harness. Recurring release steps: `desktop/RELEASING.md`.

# p2-selectivity scratch marker (delete me)
