# CLAUDE.md — working in `crates/`

The shared **Rust client core** — one Cargo workspace (committed `Cargo.lock`,
`rust-toolchain.toml` pins the channel) whose logic backs every shed client so nothing is
re-implemented per language. The root `CLAUDE.md` owns the monorepo layout + release model;
`desktop/CLAUDE.md` owns the app that consumes this core.

## The crates

- **`shed-core`** — a *pure* Rust lib (no UI, no UniFFI): the reqwest(rustls) HTTP client, the
  SSE parser, defensive wire decoders, leaf-cert TLS pinning, the control-token FSM, a `config`
  parser, the pull-based `create` orchestration store, and `rc.rs` (the pure Remote-Control
  classifier + argv builders). The Linux clients link it directly.
- **`shed-app`** — the UI-free app-logic layer (`Backend`) the clients share; holds the
  `RcRunner` portability seam (`rc.rs`, behind the non-default `rc = ["tokio/process"]`
  feature). A bare `cargo test`/`clippy` skips the `rc` module — cover it with
  `-p shed-app --features rc`.
- **`shed-core-ffi`** — a thin UniFFI wrapper (`crate-type = ["staticlib", "lib"]`)
  exposing a `ShedCore` object to Swift. The `.a` is what the app links (signing/notarization
  unchanged); `lib` is required so `cargo run -p shed-core-ffi --bin uniffi-bindgen` works
  in `desktop/scripts/build-core.sh`.
- **`shedctl`** — a headless UDS/IPC client on `shed-core` (no GUI-toolkit dep), shipped in the
  Linux `.deb` and drives the Tauri app's socket. In `default-members`.

`fixtures/` holds the real-shaped JSON/YAML samples (server info, `shed list`, `system df`,
egress profiles, enriched image, config) that both the Rust decoders and the Swift
`ConfigParityTests` assert against — keep them byte-real, not hand-trimmed.

## The workspace-boundary rule (load-bearing)

`desktop/tauri/src-tauri` is a **separate Cargo workspace ON PURPOSE**. WebKitGTK/Tauri deps
must **never** enter `crates/` — this workspace stays dependency-clean so `shed-core`/`shed-app`
compile everywhere (macOS, Linux, and eventually mobile) without dragging a desktop web stack.
The Tauri crate consumes `shed-core` + `shed-app` as cross-workspace **path-deps**; it is not a
member here. Do not add it as one.

## Build / test

```bash
cd crates && cargo test                              # workspace tests
cargo test -p shed-app --features rc                 # the non-default rc module
cargo clippy --workspace --all-targets -- -D warnings
cargo clippy -p shed-app --features rc --all-targets -- -D warnings
```

`shed-core` also builds/tests on Linux — `make -C desktop core-linux` runs it in Docker.

## FFI regeneration + version lockstep

The Swift-facing `ShedCoreFFI.xcframework` is **not** regenerated here — it is built by
`desktop/scripts/build-core.sh` (run `make -C desktop core`), which outputs into
`desktop/artifacts/` (SwiftPM requires target paths inside the package root). Editing
`shed-core-ffi`'s exported surface means re-running that from `desktop/`.

At **release**, this workspace's `Cargo.toml` version is bumped in **lockstep** with
`desktop/VERSION` (and the Tauri manifests) by `scripts/release/update-version.sh X.Y.Z
--components desktop`; `scripts/release/release-plan.sh` hard-verifies the lockstep before the
desktop leg ships. Don't hand-edit the version out of step.
