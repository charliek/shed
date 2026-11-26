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

- **The shared Rust core lives in `../crates/`** (`shed-lane`, `shed-core`, `shed-roost`,
  `shed-craze`, `shed-app`, `shed-desktop-core`, `shedctl`, plus `shed-source` and the
  provision crates), NOT here.
  The desktop is shed-only: no `machines:` hosts, no implicit localhost, no opencode lane.
  `shed-desktop-core` is the toolkit-free runtime (`ShedHosts`, `ShedCraze`, `ShedLanes`).
- `tauri/` — the **Tauri client**: the React/Vite/Tailwind frontend in `tauri/ui`, the Rust
  shell in `tauri/src-tauri` (IPC server `ipc.rs`, tray, Sparkle updater, the source
  runtime and egress feed wiring, …). `tauri/src-tauri` is its **own standalone Cargo
  workspace** so its WebKitGTK/Tauri deps never bleed into `../crates/`; it takes
  `shed-core`, `shed-app`, `shed-desktop-core`, `shed-lane`, `shed-roost` and `shed-craze` as
  cross-workspace path-deps (`../../../crates/…`). It does not depend on `roost-ipc`
  directly. Runs on macOS (WKWebView) and Linux
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
`ai.stridelabs.shed-desktop` (D-Bus/nfpm) is different. **Never change the mac bundle
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
make tauri-test-linux    # the Tauri crate's clippy + cargo test on Linux (its Linux cfg; Docker)
make core-linux          # shared-crate cargo test/clippy on Linux (Docker)
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
the module skips; `SHED_CRAZE_REQUIRE=1` (CI) makes that a failure. Every craze cell
targets a listed shed over the fake-`ssh` seam (`SHED_TAURI_SSH_BIN`); there is no
local craze path and no `machines:` fixture. The shedtest-linux skill has the traps.

## The source runtime (no broker)

The credential broker is gone (plan 039 PR B): no approvals, no plugin-bus client, no
Touch ID / polkit gate. What the app keeps of it is this machine's part as a **source**:
`shed-desktop-core`'s `DeliveryRuntime` (`desktop/tauri/src-tauri/src/delivery.rs`) holds one
delivery stream per configured server and answers provision requests from `delivery.toml`.
This machine's identity is `<state>/broker/machine.json` and the credentials-scope store
`<state>/broker/creds` (`shed_source::machine` / `credstore`, `<state>` =
`shed_source::paths::state_dir`, `SHED_BROKER_STATE_DIR` overrides it; the paths are the
retired broker's, kept so no pin or certificate is lost); that id is the only one
`source.use` sends (`src/source.rs`), and a move reopens that server's delivery stream.
The sidebar's **Credentials** view (plan 040 §3.9; Preferences keeps General and Terminal)
is plain components over `ui/src/lib/credentials.ts`, which holds every decision (row
states, scope wording, what a form requires, what Check sends; its tests are in the
explicit list in `ui/package.json`), so a port is a port of that module and a redraw.
Its forms come from `delivery.describe`'s descriptors. "Stop answering from here" sticks
through `paused-servers.json` beside `machine.json` (`src/source.rs`; unbind and use are
serialised per server). A server where the account service answers is a settled state,
`kind: service`, with no action. IPC for it: `sources.list`, `source.unbind`,
`delivery.describe`, `delivery.recent`, `credentials.dump`, `sidebar.dump`, `ui.fill_check`,
`ui.press_check`, `ui.press_source_unbind` (the Preferences provisioning doors and the `source_status` command are gone).
The egress decision feed (`src/egress.rs`, `shed_desktop_core::egress`) is the other
long-lived consumer: `egress.recent` / `egress.status` / `egress.server_profiles`. Both
drain first at shutdown (`Shutdown::set_source_drain`).

The source answers only while the app runs, so **launch at login is on by default**: at
the first launch that can record it (`Prefs.login_item_defaulted` in `prefs.json`, saved
BEFORE the login item is set; `prefs::apply_login_default`) the app turns it on once, and
never again. A debug build outside the harness never applies it (`login_default_applies`),
so `make run` / `tauri dev` never register a `target/debug` binary. Under the harness the
write is contained: Linux writes `$HOME/.config/autostart/*.desktop` under the throwaway
HOME; macOS test mode keeps it in an in-memory cell (`LoginItemCell`).

Test pointers: `cargo test -p shed-desktop-core` (the cold-launch mint, the egress feed, the
delivery runtime) and `-p shed-source` (identity, store, minter, delivery stream client);
the harness cells for the source rows and delivery live in `desktop/tools/shedtest/`.

## mTLS credentials

The app can drive a `shed-server` running `auth.mode: mtls` (client
certificates instead of bearer tokens — see the root `docs/reference/
security.md#mtls-mode`) through the Rust core's credential provider.

The private key is generated inside the Rust core's credential provider; only
the CSR goes to the minter (`shed_desktop_core::control_mint`: `shed-source`'s
SSH-bootstrap minter, built at start-up and handed to `Backend` before its first
request), and the signed public certificate comes back. The Tauri shell and
frontend never hold, see, or serialize key material. The app's control-scope
certificate lives in memory only, and a cold launch re-mints it over SSH. The
source runtime persists its credentials-scope certificate in the state dir — a
different scope and a different tradeoff (see the credential-ownership table in
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

## Cloud rows

A shed of a server whose machines come from the account service carries `cloud` (plan 041). Every decision about it (the words, the lease state `ok`/`soon`/`ended`/`unknown`, which actions it offers, the extend presets clipped to the cap, the fixed refusal words, the usage line) is made once in `crates/shed-app/src/cloud.rs`; `sheds.list` attaches the result as `cloud_row`, and the UI (`tauri/ui/src/components/ShedCloud.tsx`, `lib/cloud.ts`) only draws it, so keep new decisions out of the frontend. The IPC doors are `shed.extend`, `cloud.usage` and the test-mode `ui.press_*` / `ui.fill_extend_hours` ones (`docs/desktop/ipc.md`); the cells are `tools/shedtest/test_tauri_cloud.py` and `test_tauri_cloud_view.py`. User docs: `docs/desktop/cloud-sheds.md`.

## Skills + docs

The end-to-end validation loops are documented as skills (update them when you hit a new
rough edge): `.claude/skills/shedtest-mac` (the macOS loop: WKWebView e2e, the signed
bundle + DMG) and `.claude/skills/shedtest-linux` (the Linux loop: the WebKitGTK render gate,
the `.deb`, Docker legs). The user-facing docs live under
`docs/desktop/` — the [Test automation](../docs/desktop/test-automation.md) page mirrors the
harness. Recurring release steps: `desktop/RELEASING.md`.
