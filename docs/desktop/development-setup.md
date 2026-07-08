# Development setup

The desktop app lives under `desktop/` in the [shed monorepo](https://github.com/charliek/shed);
its shared Rust core is the sibling top-level `crates/` workspace. Targets run from the
monorepo root via the `desktop-` passthrough (`make desktop-<target>`) or directly with
`make -C desktop <target>`.

## Prerequisites

- macOS 14+ on Apple Silicon
- Xcode 16+ (Swift 6 toolchain)
- Rust stable ≥1.85 (the `crates/` workspace pins the channel in `rust-toolchain.toml`)
- [uv](https://docs.astral.sh/uv/) for the docs + test harness (Python)
- Docker (for the Linux Tauri/`.deb` legs)

## Common tasks

```bash
make -C desktop build     # Rust core (xcframework) + swift build
make -C desktop test      # swift test (ShedKit unit tests + Rust FFI canary)
make -C desktop bundle    # assemble desktop/build/ShedDesktop.app (ad-hoc signed, Sparkle embedded)
make -C desktop dmg       # release bundle + a drag-install disk image
make -C desktop run       # bundle + open the app
make -C desktop e2e       # functional harness against the running/auto-launched app
make -C desktop e2e-ci    # hermetic: fresh app, test mode, in-process mock shed-server
make -C desktop smoke     # drive the app + capture labeled screenshots
make -C desktop fmt / lint # swift-format
make -C desktop clean     # remove build artifacts
```

The Linux Tauri client + `.deb` have their own targets — `tauri-run`, `e2e-tauri`,
`tauri-build-linux` (WebKitGTK render gate), `tauri-test-linux`, `deb`, `deb-validate` — see
[Test automation](test-automation.md).

`make -C desktop dmg` is the local packaging path; cutting an actual release (which signs the
DMG and publishes the Sparkle appcast) is
[RELEASING.md](https://github.com/charliek/shed/blob/main/desktop/RELEASING.md).

## Layout

```
crates/                 # shared Rust core workspace (sibling of desktop/)
  shed-core/            # pure protocol client: HTTP/SSE, decoders, TLS, token FSM, rc
  shed-app/             # UI-free app-logic layer (Backend), the RcRunner seam
  shed-core-ffi/        # UniFFI staticlib linked into the Swift app
  shedctl/              # headless Rust IPC client (shipped in the Linux .deb)
desktop/
  Sources/
    ShedKit/            # core, no SwiftUI: HTTP/SSE clients, models, config, IPC, screenshot
    ShedDesktopUI/      # SwiftUI views + AppState
    ShedDesktopApp/     # @main app: AppModel, IPC handler impl
    shedctl/            # Swift CLI driver
  Tests/ShedKitTests/   # unit tests
  tauri/                # Tauri Linux client (its own standalone cargo workspace) + React UI
  tools/shedtest/       # pytest functional harness + in-process mock server
  artifacts/            # gitignored Rust-core build outputs (xcframework)
```

## Style

- Default to no comments; add one only when the *why* is non-obvious (a hidden constraint,
  a workaround, a tricky invariant). Don't comment what well-named code already says.
- Errors are returned/thrown, not logged-and-swallowed.
- Keep `ShedKit` free of SwiftUI so it stays unit-testable.
- Do **not** run `swift format -i` reflexively on the whole tree — match the existing
  4-space style; formatting churn muddies review.
