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

The Tauri client also has a **macOS packaging path** (Swift→Tauri transition): `make
tauri-bundle-mac` / `make tauri-dmg-mac` build a signed `ShedDesktop.app`/DMG with an
embedded real Sparkle updater. On **Darwin** every Tauri build (`tauri-build`/`-lint`/`-test`/
`-run`) needs `Sparkle.framework` staged first (`make sparkle-framework` runs
`scripts/fetch-sparkle.sh`, pinned + gitignored) — the sparkle-updater crate's `build.rs`
panics without it. The mac Tauri bundle aligns its identity to the Swift app
(`ai.stridelabs.ShedDesktop`, mac-only overlay `tauri.macos.conf.json`), so the mac dev
config dir is `~/Library/Application Support/ai.stridelabs.ShedDesktop`; the **Linux** identity
`ai.stridelabs.shed-desktop` (polkit/D-Bus/nfpm) is unchanged. See `desktop/RELEASING.md` for
the beta-rollout release flow.

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

## The embedded credential broker (leg 3a.2)

The Tauri app can broker credentials **in-process**, no separate `shed-host-agent`
daemon required. It embeds `crates/shed-broker` (the same lib crate the standalone
daemon's `main.rs` wraps) via `shed-app`'s non-default `broker` feature
(`crates/shed-app/src/broker_bridge.rs`), wired into the setup hook by
`desktop/tauri/src-tauri/src/broker.rs`. Full behavior is documented user-facing in
[docs/desktop/architecture.md § The embedded credential
broker](../docs/desktop/architecture.md#the-embedded-credential-broker-tauri).

Three modes, resolved once at startup from a persisted `broker_mode` pref (`auto`
default) overlaid on a probe of both daemon sockets — `external` (a full daemon is
running, dial it as before), `headless-coexist` (only a headless daemon's status
socket is live — mint-only, no in-app approvals), `embedded` (neither socket live —
start the in-process broker). The Swift app has no embedded path; it is always
`external`.

Test pointers:

- `cargo test -p shed-app --features broker` / `--features broker,rc` — the bridge's
  unit tests (mode resolution, `load_or_synthesize`, outcome mapping, timeout/dismiss).
- `desktop/tools/shedtest/test_tauri_broker.py` — the `--target tauri`-only hermetic
  e2e cells (three-way auto-detect, the bus → AppGate → Coordinator → respond
  round-trip via the mock server's plugin-bus endpoints, split-namespace `409`,
  malformed-config fail-closed, the synthesized fresh-install namespace set).
- The mock server's plugin-bus routes live in `desktop/tools/shedtest/mockserver.py`
  (`GET /api/plugins/listeners/{ns}/messages` SSE + `POST
  /api/plugins/listeners/{ns}/respond`), shape-pinned against
  `tests/host-agent-diff`'s synthetic bus and `docs/development/host-agent-wire-catalog.md`
  so the fakes can't drift independently.
- The Go-vs-Rust differential harness (`make test-host-agent-diff`, 108 cells) still
  protects the extracted `shed-broker` core itself — it doesn't know about the
  embedded path, only that the standalone daemon built from the same lib is
  wire-identical.

## mTLS credentials

Both UIs can drive a `shed-server` running `auth.mode: mtls` (client
certificates instead of bearer tokens — see the root `docs/reference/
security.md#mtls-mode`), but **only through the Rust-core path**. The
legacy Swift `URLSession` transport (`SHED_DESKTOP_RUST_CORE=0`) has no
certificate machinery at all and fails fast with an explicit error the
moment it's asked to talk to an mtls server, rather than attempting a
handshake it can't complete — see `ShedServerClient.swift`'s zero-network
guard. There is no equivalent escape hatch on Tauri; it's Rust-core-only by
construction.

The private key is generated inside the Rust core's credential provider and
**never crosses the FFI/UDS boundary** — only the CSR goes out (to the
host-agent, over the UDS `credential.get` message) and the signed public
certificate comes back. Swift/Tauri code never holds, sees, or serializes
key material. The app process itself **persists no credential**: the
control-scope certificate lives in memory only, and a cold launch re-mints
it from scratch over SSH via the host-agent relay (matches the existing
no-persistence posture of the Rust core's control-scope token, just extended
to certificates). The embedded broker (Tauri) and the standalone
`shed-host-agent` both persist their own credentials-scope certificate in
their state dir, same as they do for tokens — that's a different scope/
process and a different tradeoff (see the credential-ownership table in
`~/.claude/plans/shed/001-mtls-mode.md` §3 D6 if working in this area).

A desktop app newer than its `shed-host-agent` install can't use mTLS at all
until the agent is upgraded — see the [Upgrading to
mTLS](../docs/upgrades/token-to-mtls.md) guide's "desktop users" section for
the exact failure mode and the required upgrade order (agent before/with the
app; they're separate release selectors).

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
