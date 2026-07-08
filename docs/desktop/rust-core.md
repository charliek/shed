# Rust core (`shed-core`)

The shed-server protocol client — HTTP + SSE, control-token auth, TLS pinning,
and the wire DTOs — lives in a shared Rust core in the monorepo's top-level
`crates/` workspace, so the same logic backs both the macOS Swift app and the
**Tauri cross-platform client** (the shipped Linux client) without being
re-implemented per language.

## Layout

A cargo workspace under `crates/` (its members `shed-core`, `shed-core-ffi`,
`shed-app`, `shedctl` are all default-members):

- **`shed-core`** — a *pure* Rust lib (no UniFFI): wire DTOs + serde decoders
  (`models.rs`), the reqwest(rustls) client (`http.rs`), the SSE parser
  (`sse.rs`), leaf-cert pinning (`tls.rs`), and the control-token FSM
  (`token.rs`), plus a `config` parser and a pull-based `create` orchestration
  store — and **`rc.rs`** (pure Remote-Control: the pane classifier, the
  `shed-ext-rc` + non-interactive SSH argv builders, and the wire DTOs). The Linux
  clients (the Tauri app, `shedctl`) link this crate directly (no UniFFI).
- **`shed-core-ffi`** — a thin UniFFI wrapper (`crate-type = ["staticlib"]`)
  exposing a `ShedCore` object + records to Swift. `desktop/scripts/build-core.sh`
  builds it, runs `uniffi-bindgen`, and assembles a **static**
  `ShedCoreFFI.xcframework` linked into the app's Mach-O — no new dylib, so the
  release signing/notarization path is unaffected.
- **`shed-app`** — the UI-free app-logic layer (`Backend`), a workspace
  default-member; consumed by the Tauri client (as a cross-workspace path dep).
  Holds the **`RcRunner` portability seam** (`rc.rs`, behind the
  non-default `rc = ["tokio/process"]` feature) — the trait where a future mobile
  in-process-SSH runner replaces the desktop subprocess runner, so one
  `RcService` serves every frontend.
- **`shedctl`** — a headless UDS/IPC client shipped in the `.deb` alongside the Tauri
  `shed-desktop` binary, mirroring the macOS Swift `shedctl`. It *is* in `default-members`, so
  `make -C desktop core-test`/`core-lint` cover it on the Mac.
- **Tauri client** — *not* a `crates/` workspace member, but a consumer and the
  **shipped Linux client**: `desktop/tauri/src-tauri` is its own standalone cargo
  workspace that takes `shed-core` + `shed-app` as cross-workspace **path deps**.
  Its `[[bin]]` is **`shed-desktop-tauri`**, installed to `/usr/bin/shed-desktop`
  in the `.deb` (built via nfpm, `linux/scripts/build-deb.sh`). Runs on macOS
  (WKWebView, a UI-comparison loop vs the Swift app) and Linux (WebKitGTK, the
  shipped target).

`make -C desktop core` builds it; `make -C desktop build` / `bundle` / CI build it before any
SwiftPM step (the `.binaryTarget` path must exist first). The generated artifacts
under `desktop/artifacts/` are gitignored.

## How the Swift app uses it

`ShedServerClient` (ShedKit) delegates to the core when `SHED_DESKTOP_RUST_CORE=1`
and otherwise keeps its existing `URLSession` path. `RustShedCoreAdapter` maps the
Rust records to the app's Swift `Models` (which double as the IPC wire shapes),
so every IPC op stays byte-unchanged. `identify` reports `core: rust|swift`.

The Rust core is the macOS **default**; `SHED_DESKTOP_RUST_CORE=0`
forces the Swift path (a rollback escape hatch kept ≥2 releases). A per-host
adapter-construction failure fails **loudly** rather than silently downgrading. CI
runs the e2e suite on the default (rust) plus a `=0` Swift-fallback leg, and a
golden-JSON cross-backend byte-diff guards parity; `identify.core` reports which
backend is active.

## Invariants preserved (parity with the Swift client)

- **Defensive decoding**: `{"sheds": null}` → `[]`, omitted optionals, lenient
  `ShedStatus`, `"?"` name sentinels, and timestamps carried verbatim (parsing
  stays in Swift).
- **Auth, fail-closed**: a provider mint failure sends **no** token (never the
  static one — no secure-by-default downgrade); a 401 invalidates + retries once
  (provider-backed only); create mints once with no retry.
- **TLS**: leaf SHA-256 pin (`sha256:<lowercase-hex>`), fail-closed on a
  non-`https://` URL.
- **SSE create**: cross-chunk framing, and the `error` / stream-ended-without-
  `complete` terminal semantics.

## Building & testing

All targets run from the monorepo root (or `make -C desktop …`):

```bash
make -C desktop core            # build the staticlib + regenerate the xcframework
make -C desktop core-test       # cargo test the workspace
make -C desktop core-lint       # cargo clippy -D warnings
make -C desktop test            # swift tests (builds the core first)
SHED_DESKTOP_RUST_CORE=1 make -C desktop e2e-ci   # the hermetic e2e with the Rust backend
```

You can also drive the workspace directly from `crates/` (`cargo test`, `cargo clippy`). Rust
changes need `cargo`/`rustup` (the workspace pins the channel in `crates/rust-toolchain.toml`).

## Status & scope

The core covers the read client, lifecycle, SSE create, TLS pinning, the
control-token FSM, a `config` parser, and the create orchestration store; it is
the macOS **default** and builds/tests on **Linux**. The **Tauri cross-platform
client** (`desktop/tauri/`), built on `shed-core` + `shed-app`, is the **shipped
Linux client** — an earlier GTK MVP has been retired and the `.deb` is now built
from `tauri/src-tauri` (WebKitGTK). It carries lifecycle + create, the approval
spine (polkit gate + zbus notifier on Linux, Touch-ID on macOS), the Agents/RC
pane, tray/native-menu, and launch-at-login, driven by the
`tools/shedtest --target mac|tauri` harness. Still deferred: retiring the Swift
`URLSession` path + unifying config via the FFI, and absorbing/rewriting the
credential broker in Rust (the final consolidation).
