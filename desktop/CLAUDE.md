# CLAUDE.md — working in `desktop/`

The shed desktop app: a Tauri client (React/Vite/Tailwind UI + a Rust shell), shipping as both
the **macOS** client and the **Linux** client, a thin shell over the shared Rust core in the
sibling `crates/` workspace. (Before 0.9.0 the macOS client was a native Swift app; it has been
removed — the Tauri mac app keeps its bundle id, Sparkle feed and EdDSA key so those installs
update in place.) This file orients an AI assistant working in this subtree. The root
`CLAUDE.md` owns the monorepo layout + release model; `crates/CLAUDE.md` owns the core.

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

- **The shared Rust core lives in `../crates/`** (`shed-core` + `shed-app` + the Rust
  `shedctl`), NOT here.
- `tauri/` — the **Tauri client**: the React/Vite/Tailwind frontend in `tauri/ui`, the Rust
  shell in `tauri/src-tauri` (IPC server `ipc.rs`, tray, approval gate, Sparkle updater,
  broker wiring, …). `tauri/src-tauri` is its **own standalone Cargo workspace** so its
  WebKitGTK/Tauri deps never bleed into `../crates/`; it takes `shed-core` + `shed-app` as
  cross-workspace path-deps (`../../../crates/…`). Runs on macOS (WKWebView) and Linux
  (WebKitGTK). The Linux `.deb` (`shed-desktop`, bin `shed-desktop-tauri`) is built from it
  via nfpm (`linux/scripts/build-deb.sh`); the macOS `.app`/DMG via
  `scripts/bundle-tauri-mac.sh` + `scripts/make-dmg.sh`.
- `tools/shedtest/` — the pytest harness + in-process mock shed-server that drives the app.
- `Resources/bin/` — the terminal-opener scripts bundled into the app (`tauri.conf.json`
  `bundle.resources`; `build.rs` validates them).

The dedicated GTK client that used to ship the `.deb` has been **retired** — Tauri replaced
it. There is no `shed-gtk` crate.

**macOS packaging:** `make tauri-bundle-mac` / `make tauri-dmg-mac` build a signed
`ShedDesktop.app`/DMG with an embedded real Sparkle updater. On **Darwin** every Tauri build
(`tauri-build`/`-lint`/`-test`/`-run`) needs `Sparkle.framework` staged first
(`make sparkle-framework` runs `scripts/fetch-sparkle.sh`, pinned + gitignored) — the
sparkle-updater crate's `build.rs` panics without it. The mac bundle uses the identity
`ai.stridelabs.ShedDesktop` (mac-only overlay `tauri.macos.conf.json`), so the mac dev config
dir is `~/Library/Application Support/ai.stridelabs.ShedDesktop`; the **Linux** identity
`ai.stridelabs.shed-desktop` (polkit/D-Bus/nfpm) is different. **Never change the mac bundle
id, `SUFeedURL` or `SUPublicEDKey`** (`tauri/src-tauri/Info.plist`) — every installed copy
updates against them, and CI's bundle smoke pins the key. See `desktop/RELEASING.md` for the
release flow.

## The change loop

```bash
npm --prefix tauri/ui ci     # once: the frontend's deps
make build && make test      # tauri-build; core-test + tauri-test (cargo unit tests)
make lint                    # clippy (core + tauri), -D warnings
make e2e                     # the hermetic pytest harness (Xvfb on Linux: wrap in xvfb-run -a)
make run                     # launch the app against your real ~/.shed/config.yaml
```

The harness is hermetic: it launches the binary with `SHED_TAURI_TEST_MODE=1` +
`SHED_TAURI_MOCK_BASE_URL` and a throwaway HOME/XDG_RUNTIME_DIR, so all HTTP hits the
in-process mock and no real shed-server or your own config is touched. `identify` is checked up
front to confirm hermeticity. Use condition-waits (`wait_until`), **never sleeps**. Fixtures
live in `tools/shedtest/fixtures/`. To eyeball a change, drive a running app with the Rust
`shedctl` (`cargo run -p shedctl --manifest-path ../crates/Cargo.toml -- …`) or the harness's
`TauriClient`, and read `app.screenshot` (Linux; on macOS the capture is Screen-Recording-TCC-
gated, so the harness skips it there). The `shedtest-mac` / `shedtest-linux` skills have the
full loops.

## Build-order gotchas (read these)

- **The frontend bundle must exist before any `cargo` build of the Tauri crate.**
  `generate_context!` fails closed if `tauri.conf.json`'s `frontendDist` (`../ui/dist`) is
  missing; every `tauri-*` make target builds it first (`tauri-ui-build`, which also runs the
  frontend's type-check + unit tests).
- **On macOS stage Sparkle first** (`make sparkle-framework`; the make targets do it for you).
- **If the mise `cargo` shim errors** ("No version is set for shim: cargo") put
  `~/.cargo/bin` first on `PATH` — the workspaces pin `stable` via `rust-toolchain.toml`.

## Linux: the render gate + the `.deb`

```bash
make tauri-run           # build + launch the Tauri client natively
make e2e-tauri           # the hermetic pytest harness (needs a display, Xvfb on Linux)
make tauri-build-linux   # the WebKitGTK render gate: the e2e on ubuntu:24.04 / WebKitGTK 2.44 (Docker, Xvfb)
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
the macOS WKWebView e2e alone can miss Linux-only breaks. On Linux the tray is a native menu
(Tauri emits no Linux tray-click events → no popover; expected).

The craze cells (`tools/shedtest/test_tauri_craze.py`, plan 025) drive the REAL craze hub and
need the pinned binaries: `make craze-binaries` (natively; it prints `SHED_CRAZE_BIN_DIR`), or
`make craze-binaries OUT=desktop/tools/shedtest/.craze-bin` plus
`SHED_CRAZE_BIN_DIR=/work/desktop/tools/shedtest/.craze-bin` for the Docker legs. Without them
the module skips; `SHED_CRAZE_REQUIRE=1` (CI) makes that a failure. The shedtest-linux skill has
the seams (`SHED_TAURI_CRAZE_PATH`, the jailed ladder) and the traps.

## The credential broker

The app **is** the credential broker: it brokers credentials in-process, with no separate
daemon. It embeds `crates/shed-broker` via `shed-app`'s non-default `broker` feature
(`crates/shed-app/src/broker_bridge.rs`), wired into the setup hook by
`desktop/tauri/src-tauri/src/broker.rs`. There is no mode to choose: a malformed
`extensions.yaml` fails the broker closed (approvals dark, reported in `broker.status`) and
the app keeps running. This machine's identity is `<state>/broker/machine.json`
(`shed_broker::machine`, `<state>` = `shed_broker::paths::state_dir`); it is the only id
`source.use` ever sends. Full behavior is documented user-facing in
[docs/desktop/architecture.md § The embedded credential
broker](../docs/desktop/architecture.md#the-embedded-credential-broker).

The broker answers only while the app runs, so **launch at login is on by default**: at
the first launch that can record it (`Prefs.login_item_defaulted` in `prefs.json`, saved
BEFORE the login item is set; `prefs::apply_login_default`) the app turns it on once, and
never again. A debug build outside the harness never applies it (`login_default_applies`),
so `make run` / `tauri dev` never register a `target/debug` binary. Under the harness the
write is contained: Linux writes `$HOME/.config/autostart/*.desktop` under the throwaway
HOME; macOS test mode keeps it in an in-memory cell (`LoginItemCell`).

Test pointers:

- `cargo test -p shed-app --features broker` — the bridge's unit tests
  (`load_or_synthesize`, outcome mapping, timeout/dismiss). `broker` is `shed-app`'s ONLY
  non-default feature.
- `desktop/tools/shedtest/test_tauri_broker.py` — the hermetic e2e cells (the app always
  brokers in process, the bus → AppGate → Coordinator → respond round-trip via the mock
  server's plugin-bus endpoints, split-namespace `409`, malformed-config fail-closed, the
  synthesized fresh-install namespace set, the plan 035 machine pin: `not-source` then
  accepted, `duplicate`, other 403s, the respond `503` retry; the credential-source rows:
  `source.status`/`source.use`, the Preferences "Use this machine" button via the test-mode
  `ui.press_source_use` door, against the mock's `arm_source` binding; and the
  launch-at-login default, once, and never with unwritable prefs).
- The mock server's plugin-bus routes live in `desktop/tools/shedtest/mockserver.py`
  (`GET /api/plugins/listeners/{ns}/messages` SSE + `POST
  /api/plugins/listeners/{ns}/respond`), shape-pinned against
  `tests/broker-wire`'s synthetic bus and `docs/development/broker-wire-catalog.md`
  so the fakes can't drift independently.
- The `tests/broker-wire` wire harness (`make test-broker-wire`) pins the broker's
  wire output: it runs the app's embedded broker in the test-only
  `crates/shed-broker-harness` process against committed goldens.

## mTLS credentials

The app can drive a `shed-server` running `auth.mode: mtls` (client
certificates instead of bearer tokens — see the root `docs/reference/
security.md#mtls-mode`) through the Rust core's credential provider.

The private key is generated inside the Rust core's credential provider; only
the CSR goes to the embedded broker's minter, and the signed public certificate
comes back. The Tauri shell and frontend never hold, see, or serialize key
material. The app's control-scope certificate lives in memory only, and a cold
launch re-mints it over SSH through the broker. The broker persists its own
credentials-scope certificate in its state dir — a different scope and a
different tradeoff (see the credential-ownership table in
`~/.claude/plans/shed/001-mtls-mode.md` §3 D6 if working in this area).

## Socket paths

- **App IPC:** `$XDG_RUNTIME_DIR/shed-tauri.sock` (override `SHED_TAURI_SOCKET`; `/tmp`
  fallback; same on macOS). Hermeticity: `SHED_TAURI_TEST_MODE` / `SHED_TAURI_MOCK_BASE_URL` /
  `SHED_TAURI_SHED_CONFIG`. In containers, point the harness at a prebuilt binary with
  `SHED_TAURI_BIN`.

## Conventions

- New UI or behavior gets an IPC op (`tauri/src-tauri/src/ipc.rs`) + harness coverage — the
  north star above.
- Default to no comments; add one only when the *why* is non-obvious.
- Decode defensively against real shed-server shapes (`{"sheds": null}`, omitted fields,
  mixed timestamp formats) — there are unit tests pinning these.
- Don't expose any shed-server further; a reachable server is trusted by the network, not the
  app.

## Skills + docs

The end-to-end validation loops are documented as skills (update them when you hit a new
rough edge): `.claude/skills/shedtest-mac` (the macOS loop: WKWebView e2e, the signed
bundle + DMG) and `.claude/skills/shedtest-linux` (the Linux loop: the WebKitGTK render gate,
the `.deb`, Docker legs). The user-facing docs live under
`docs/desktop/` — the [Test automation](../docs/desktop/test-automation.md) page mirrors the
harness. Recurring release steps: `desktop/RELEASING.md`.
