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
  `RcRunner` portability seam (`rc.rs`) behind the non-default `rc` feature — which also
  pulls in and re-exports `shed-rc-engine` as `shed_app::rc_engine` — and the embedded
  broker bridge (`broker_bridge.rs`, behind the non-default `broker = ["dep:shed-broker"]`
  feature — leg 3a.2). A bare `cargo test`/`clippy` skips both modules — cover them with
  `-p shed-app --features rc` and `-p shed-app --features broker` (or `broker,rc`
  together).
- **`shed-rc-engine`** — the one-shot Remote-Control engine ported from the Go guest
  binary (plan 009), graduated out of shed-app at its second consumer (plan 010:
  shed-broker's `rc_hub` — a broker→shed-app dep would cycle through shed-app's `broker`
  feature). Synchronous by design, on the pure `shed_core::rc_agents` kernel; carries its
  own minimal `clock` seam (shed-app's `traits::Clock` stays in shed-app). The
  `test-support` feature exports `fake` (the fake tmux runner) for sx's and the hub's
  tests. In `default-members`.
- **`shed-core-ffi`** — a thin UniFFI wrapper (`crate-type = ["staticlib", "lib"]`)
  exposing a `ShedCore` object to Swift. The `.a` is what the app links (signing/notarization
  unchanged); `lib` is required so `cargo run -p shed-core-ffi --bin uniffi-bindgen` works
  in `desktop/scripts/build-core.sh`.
- **`shedctl`** — a headless UDS/IPC client on `shed-core` (no GUI-toolkit dep), shipped in the
  Linux `.deb` and drives the Tauri app's socket. In `default-members`.
- **`sx`** — the RC **porcelain** binary (plan 009), on `shed-core` + `shed-app` (with the
  non-default `rc` feature enabled by its own manifest, so `cargo build -p sx` needs no
  flags). Today it exposes one namespace, `sx rc <verb>` — the ported one-shot engine,
  wire-compatible with the Go `shed-machine-rc <verb>` under the comparison model
  `tests/rc-parity` enforces (`make test-rc-parity` builds BOTH binaries and diffs them) — and
  the **porcelain verbs** on top of it: `sx agent <tool>` / `sx plan <file>` (kickoff) and
  `sx ls` / `sx watch` / `sx attach` / `sx kill` (observe), each taking
  `--on local | machine:<name> | shed:<name>[@<server>]`. `machine:` entries come from the
  `machines:` section of `~/.shed/config.yaml` (Rust-defined, Go-passthrough); a shed's SSH
  endpoint comes from `shed-app`'s `Backend`. Hand-rolled arg parsing, like `shedctl`, with the
  house subject-first grammar (`sx watch <slug> --on …`). In `default-members`. Its
  dev-dependencies enable shed-app's **`test-support`** feature, which exports
  `rc_engine::fake` (the fake tmux runner) across the crate boundary — test-only by
  construction (`#[cfg(any(test, feature = "test-support"))]`).
- **`shed-broker`** — the embeddable host-agent broker core: the shed-server plugin bus,
  the multi-server supervisor + discovery watcher, the SSH/AWS/Docker/egress credential
  backends, the SSH-bootstrap minter + control-token provider, the approval/audit seams
  (incl. the always-compiled `AuditFanout` fan-out and the native `touchid` gate), the
  `config` reader, socket path-resolution + liveness probes, and the LiveStatus snapshot
  builder. Consumed today by the **`shed-host-agent` bin** (the daemon shell — CLI,
  signals, socket bind, the Surface-A desktop UDS server) and, from leg 3a.2, embedded
  in-process by the desktop app. Carries no daemon-only or WebKitGTK concern.

`fixtures/` holds the real-shaped JSON/YAML samples (server info, `shed list`, `system df`,
egress profiles, enriched image, config) that both the Rust decoders and the Swift
`ConfigParityTests` assert against — keep them byte-real, not hand-trimmed.

## The workspace-boundary rule (load-bearing)

`desktop/tauri/src-tauri` is a **separate Cargo workspace ON PURPOSE**. WebKitGTK/Tauri deps
must **never** enter `crates/` — this workspace stays dependency-clean so `shed-core`/`shed-app`
compile everywhere (macOS, Linux, and eventually mobile) without dragging a desktop web stack.
The Tauri crate consumes `shed-core` + `shed-app` as cross-workspace **path-deps**; it is not a
member here. Do not add it as one.

### The no-YAML-dep posture — and its one carve-out

Both clients hand-roll a tiny indentation reader (`shed-core`'s and `shed-broker`'s own
`yaml_lite` mods) rather than take a YAML dependency. That aversion targets the **serde-based**
crates (`serde_yaml` — archived/unmaintained — and `serde_norway`): serde-derive on the config
structs is what's being avoided, not a parser per se.

**The scoped exception:** `shed-broker` (the broker core, home of the host-agent `config`
reader) depends on **`saphyr-parser`** (pure-Rust, no-serde, no-C, no encoding_rs,
`default-features = false`) to back ITS `yaml_lite::parse`. Justification: the shipped
`configs/extensions.example.yaml` uses block-style `docker.registries:` sequences the line/colon
reader silently dropped, and Go's `LoadConfig` rejects malformed YAML the hand-rolled reader could
not detect — a real Go-vs-Rust divergence on the product's own default config. `saphyr-parser`
sits behind the `Node` interface (swap-insulation for its pre-1.0 API). It — and `shed-broker`'s
other leaf deps (`notify`, the `aws-sdk-*` stack) — are **deps of `shed-broker`, reaching only its
embedders** (today the `shed-host-agent` bin; from leg 3a.2 also `shed-app` under its non-default
`broker` feature and, transitively, the Tauri client). They must **never** reach `shed-core`,
`shed-core-ffi`, or **default-features `shed-app`** (proven by `cargo tree -i saphyr-parser` /
`notify` / `aws-sdk-sts` — the §7 reverse-dep AC mechanically enforces this). **shed-core's own
`yaml_lite` stays hand-rolled** (it carries a Swift byte-parity test); converging the two readers
onto `saphyr-parser` would be a separate shed-core slice, not assumed here.

## Build / test

```bash
cd crates && cargo test                              # workspace tests
cargo test -p shed-app --features rc                 # the non-default rc module
cargo test -p shed-app --features broker             # the embedded broker bridge (3a.2)
cargo test -p shed-app --features broker,rc          # both non-default features together
cargo test -p shed-rc-engine --features test-support # the graduated engine + its doubles
cargo clippy --workspace --all-targets -- -D warnings
cargo clippy -p shed-app --features rc --all-targets -- -D warnings
cargo clippy -p shed-app --features broker --all-targets -- -D warnings
cargo clippy -p shed-app --features broker,rc --all-targets -- -D warnings
cargo test -p sx                                     # the RC porcelain CLI
```

Note: because `sx` is a default member that enables shed-app's `rc` feature, a bare
`cargo test`/`clippy --workspace` now also compiles (and runs) the `rc` modules through
feature unification. The explicit `-p shed-app --features rc` legs above stay — they are
what covers the crate when it is built ALONE (and what CI runs).

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
