# Development setup

The desktop app lives under `desktop/` in the [shed monorepo](https://github.com/charliek/shed);
its shared Rust core is the sibling top-level `crates/` workspace. Targets run from the
monorepo root via the `desktop-` passthrough (`make desktop-<target>`) or directly with
`make -C desktop <target>`.

## Prerequisites

- Rust stable ≥1.97 (both cargo workspaces pin the `stable` channel in `rust-toolchain.toml`)
- Node 20+ for the frontend in `desktop/tauri/ui`
- [uv](https://docs.astral.sh/uv/) for the docs + test harness (Python)
- Docker, for the Linux legs (`tauri-build-linux`, `tauri-test-linux`, `deb`, `core-linux`)

Platform extras:

| Platform | Needs |
|---|---|
| macOS 14+ (Apple Silicon) | Xcode Command Line Tools. Full Xcode is needed only for `actool`, which compiles the macOS 26 (Tahoe) glass app icon; without it `tauri-bundle-mac` falls back to the flat `.icns`. |
| Linux | The WebKitGTK 4.1 build dependencies. On Ubuntu 24.04: `build-essential pkg-config libwebkit2gtk-4.1-dev libgtk-3-dev libssl-dev libxdo-dev libayatana-appindicator3-dev librsvg2-dev libsoup-3.0-dev` (the list `desktop/Dockerfile.tauri-linux` installs), plus `xvfb` and `scrot` to run the harness headless. |

Install the frontend dependencies once:

```bash
npm --prefix desktop/tauri/ui ci
```

## Common tasks

```bash
make -C desktop build       # frontend bundle + cargo build of the Tauri app
make -C desktop run         # build the frontend + launch the app (cargo run)
make -C desktop test        # cargo test: the Rust core + the Tauri crate
make -C desktop lint        # cargo clippy -D warnings: the Rust core + the Tauri crate
make -C desktop fmt         # cargo fmt: the Rust core + the Tauri crate
make -C desktop e2e         # the hermetic pytest harness (needs a display; Xvfb on Linux)
make -C desktop clean       # remove build artifacts
```

`build`, `run`, `test`, `lint` and `e2e` are aliases for `tauri-build`, `tauri-run`,
`core-test` + `tauri-test`, `core-lint` + `tauri-lint` and `e2e-tauri`; `make -C desktop help`
lists every target. On macOS the Tauri targets first stage the pinned `Sparkle.framework`
(`sparkle-framework`), which the app links.

Packaging:

| Target | Output |
|---|---|
| `tauri-bundle-mac` | `desktop/build/ShedDesktop.app` (ad-hoc signed unless `SHED_DESKTOP_DEVELOPER_ID_IDENTITY` is set) |
| `tauri-dmg-mac` | the bundle plus a drag-install `desktop/build/ShedDesktop-<version>.dmg` |
| `deb` | the Linux `.deb`, built in Docker → `desktop/out/` |
| `deb-validate` | `deb`, then an install check in a clean `ubuntu:24.04` container |

The Linux render gate (`tauri-build-linux`) and the Linux-only crate tests
(`tauri-test-linux`) are covered in [Test automation](test-automation.md). Cutting an actual
release (which signs the DMG and publishes the Sparkle appcast) is
[RELEASING.md](https://github.com/charliek/shed/blob/main/desktop/RELEASING.md).

## Layout

```
crates/                   # shared Rust core workspace (sibling of desktop/)
  shed-core/              # pure protocol client: HTTP/SSE, decoders, TLS, token FSM, config, approval + lane models
  shed-app/               # UI-free app logic: Backend, approval Coordinator, host-agent client, roost
  shed-broker/            # the credential broker (embedded in the app, or run by shed-host-agent)
  shed-host-agent/        # the standalone broker daemon
  shed-craze/             # craze agent-lane adapter
  shed-opencode/          # opencode agent-lane adapter
  shedctl/                # headless IPC client (shipped in the Linux .deb)
desktop/
  tauri/
    src-tauri/            # the Tauri app: its own cargo workspace (bin shed-desktop-tauri)
    ui/                   # React + Vite + TypeScript frontend
  Resources/bin/          # bundled terminal openers (shed-open-ghostty, shed-open-roost.py)
  scripts/                # macOS bundle, DMG, notarize, Sparkle fetch + appcast
  linux/scripts/          # .deb build + validation
  packaging/              # nfpm config, desktop entry, polkit action, icons
  tools/shedtest/         # pytest functional harness + in-process mock shed-server
  tools/fake-host-agent/  # the fake host agent the harness approves against
  Dockerfile.tauri-linux  # the Linux render-gate / .deb build image
  Dockerfile.linux        # the shed-core Linux test image
```

## Style

- Default to no comments; add one only when the *why* is non-obvious (a hidden constraint,
  a workaround, a tricky invariant). Don't comment what well-named code already says.
- Errors are returned, not logged-and-swallowed.
- Keep `shed-core` and `shed-app` free of UI and Tauri dependencies; Tauri/WebKitGTK crates
  belong only in `desktop/tauri/src-tauri`.
- Run `make -C desktop fmt` before committing Rust changes.
