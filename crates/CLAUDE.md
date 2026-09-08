# CLAUDE.md — working in `crates/`

The shared **Rust client core** — one Cargo workspace (committed `Cargo.lock`,
`rust-toolchain.toml` pins the channel) whose logic backs every shed client so nothing is
re-implemented per language. The root `CLAUDE.md` owns the monorepo layout + release model;
`desktop/CLAUDE.md` owns the app that consumes this core.

## The crates

- **`shed-core`** — a *pure* Rust lib (no UI, no UniFFI): the reqwest(rustls) HTTP client, the
  SSE parser, defensive wire decoders, leaf-cert TLS pinning, the control-token FSM, a `config`
  parser, the pull-based `create` orchestration store, and `rc.rs` (the Remote-Control
  wire types + argv builders; its pure pane classifier went with S2, charliek/shed#324 —
  a shed row's `state` is liveness off the wire, a machine row's status comes from
  roost). The Linux clients link it directly.
  `lane.rs` (plan 015) is the **agent-lane contract** — the DTOs plus the `AgentLane`
  async trait that normalizes "a coding agent with sessions, a transcript and approvals",
  one adapter per agent (opencode over its local HTTP server; `gx` next). Pure types, **no
  I/O** — the transport, fold, ring and reconnect loop belong to whatever crate implements
  it. It lives here, not in `shed-app`, because shed-mobile links the DTOs through FRB.
  **The FRB-mirror rule (load-bearing):** mobile HAND-mirrors every lane DTO into Dart, so
  every field is an owned `String`/`Option`/`Vec`/scalar — **no `serde_json::Value`, no
  `HashMap`, no borrowed lifetimes**; free-form payloads travel as a `String` of raw JSON
  (`LaneApproval::request_json`, `LaneAnswer::Raw`). A fielded enum becomes a Dart sealed
  class, a plain one a plain Dart enum. Same rule as `rc.rs`'s feed types, which `lane`
  reuses (`RcFeedMessage` IS the transcript row) — which is why those gained `Serialize`
  plus a tolerant `Deserialize` delegating to their existing `from_map` reader.
- **`shed-app`** — the UI-free app-logic layer (`Backend`) the clients share; holds the
  `RcRunner` portability seam (`rc.rs`) behind the non-default `rc` feature — which also
  pulls in and re-exports `shed-rc-engine` as `shed_app::rc_engine` — and the embedded
  broker bridge (`broker_bridge.rs`, behind the non-default `broker = ["dep:shed-broker"]`
  feature — leg 3a.2). `roost.rs` (plan 013, ungated like `machine.rs` so mobile's
  default-features build links it) is the reach + watcher layer over `shed_core::roost` —
  `RoostReach`/`RoostWatcher`/`RoostPeek` — that both clients read a machine's or the local
  `roost-session` through. A bare `cargo test`/`clippy` run against `shed-app` **alone**
  (`-p shed-app`, no `--features`) skips the `rc`/`broker` modules — cover them with
  `-p shed-app --features rc` and `-p shed-app --features broker` (or `broker,rc`
  together). Since `sx` was sunset in plan 016 (S7, #329), nothing in this workspace
  enables `rc` by default any more — the explicit `-p shed-app --features rc` leg
  (see the note below) is the ONLY coverage for it, same as `broker`.
- **`shed-rc-engine`** — the one-shot Remote-Control engine ported from the Go guest
  binary (plan 009), graduated out of shed-app at its second consumer (plan 010:
  shed-broker's `rc_hub` — a broker→shed-app dep would cycle through shed-app's `broker`
  feature). Synchronous by design, on the pure `shed_core::rc_agents` kernel; carries its
  own minimal `clock` seam (shed-app's `traits::Clock` stays in shed-app). The
  `test-support` feature exports `fake` (the fake tmux runner) for the hub's
  tests. In `default-members`.
- **`shed-core-ffi`** — a thin UniFFI wrapper (`crate-type = ["staticlib", "lib"]`)
  exposing a `ShedCore` object to Swift. The `.a` is what the app links (signing/notarization
  unchanged); `lib` is required so `cargo run -p shed-core-ffi --bin uniffi-bindgen` works
  in `desktop/scripts/build-core.sh`.
- **`shedctl`** — a headless UDS/IPC client on `shed-core` (no GUI-toolkit dep), shipped in the
  Linux `.deb` and drives the Tauri app's socket. In `default-members`.
- **`shed-broker`** — the embeddable host-agent broker core: the shed-server plugin bus,
  the multi-server supervisor + discovery watcher, the SSH/AWS/Docker/egress credential
  backends, the SSH-bootstrap minter + control-token provider, the approval/audit seams
  (incl. the always-compiled `AuditFanout` fan-out and the native `touchid` gate), the
  `config` reader, socket path-resolution + liveness probes, and the LiveStatus snapshot
  builder. Consumed today by the **`shed-host-agent` bin** (the daemon shell — CLI,
  signals, socket bind, the Surface-A desktop UDS server) and, from leg 3a.2, embedded
  in-process by the desktop app. Carries no daemon-only or WebKitGTK concern.
- **`shed-opencode`** — the **opencode adapter** for `shed_core::lane` (plan 015): the
  one implementation of `AgentLane` that talks to an opencode server's local HTTP API —
  the same server the TUI is already running, never a sidecar it launches itself. The
  Tauri client consumes it as a plain path-dep (a machine row's `agent_lane` stamp,
  fed by roost's `server_url` report on the tab; see `docs/desktop/agent-lanes.md` for
  the end-to-end contract and its current limits). `fold.rs` is a **port** of the rc
  hub's `OpencodeFold`, and it is pinned as one —
  `fixtures/opencode_turn.golden.json` records what the HUB's fold produced on
  `fixtures/jsonl/opencode_turn.jsonl`, and the test replays the port against it (that
  test must NEVER take a `shed-broker` dep; the golden file is the pin). A **second**
  golden, `fixtures/1.18.29/fold.golden.json`, pins the port's OWN behavior (rows,
  verdicts and `LaneApproval` DTOs) over a committed 155-frame opencode 1.18.29
  recording — regression detection, not fidelity, except for the transcript subset that
  was separately proven identical to the hub's fold. **The two claims must never be
  blurred**; `shed-opencode/fixtures/README.md` is the one place that spells them out,
  and it carries the two-step regeneration recipe (re-record the wire live with
  `SHED_OPENCODE_LIVE=1 SHED_OPENCODE_RECORD=1 … --test live`; re-derive the golden
  OFFLINE with `SHED_OPENCODE_REGOLD=1 … --test fold_fixtures`). The helpers the
  fold needs are **copied** into `helpers.rs` rather than linked, because
  `rc_hub::watch` imports `shed_rc_engine::tmux::Tmux` — linking would drag the RC
  engine into an HTTP adapter. The duplication ends when S6 deletes the hub's watcher.
  In `default-members`.

`fixtures/` holds the real-shaped JSON/YAML samples (server info, `shed list`, `system df`,
egress profiles, enriched image, config) that both the Rust decoders and the Swift
`ConfigParityTests` assert against — keep them byte-real, not hand-trimmed.
`fixtures/roost-vectors/` is a **vendored copy** of roost's own golden IPC vectors at the
pinned rev (see below); its README carries the source sha and roost's never-semantically-edit
rule.

## The one git dependency: `roost-ipc`

`shed-core` depends on `roost-ipc` (the Roost Pivot, plan 013) — the only git dependency in
this workspace, and it is **rev-pinned in `Cargo.toml`, not just in `Cargo.lock`**:

```toml
roost-ipc = { git = "https://github.com/charliek/roost", rev = "<sha>" }
```

shed-mobile consumes `shed-core` **as a git dependency**, and cargo does not inherit a
dependency's lockfile — a `branch = "main"` pin here would let mobile resolve whatever `main`
was on the day its own lock was regenerated, against a crate that publishes **no Rust-API
stability promise** (the pin is what absorbs that). With the rev in the manifest, mobile's
lock cannot disagree, so `shed-mobile/scripts/check-lock-rev.sh` needs no change.

**Bump recipe:** edit the `rev` in `crates/Cargo.toml`, run `cargo update -p roost-ipc`, re-copy
`fixtures/roost-vectors/` from the new rev's `tests/ipc-vectors/` (updating that README's sha),
commit all of it. Re-read roost's `docs/reference/ipc-compatibility.md` on any bump crossing a
`SESSION_PROTOCOL_VERSION` change — `shed_core::roost::Conn::session_identify` refuses a
mismatch by name rather than limping. roost keeps **one `session.identify` vector per
generation** (`session.identify.response.v<N>.json`); shed vendors only the current one, so a
generation bump renames the vendored file and both fakes' `include_str!`/`_vector()` paths move
with it. An older shape is a fake *control* (`set_session_protocol`, `serve_without_features`),
never a second vector to keep in step.

**What session protocol 4 gave us (roost R1, plan 014).** The pin is at `c67ac27…` and shed
speaks generation **4**:

- **Reads are free.** `events.subscribe` takes no lease; it *classifies*. An empty lease is an
  **observer** stream — every workspace batch plus `notification.fired`, never `tab.effect` —
  which is what `shed_app::roost::RoostWatcher` subscribes as, so watching somebody's machine
  never takes the interactive lease from the roost UI they are looking at.
- **Writes are owned.** `Conn::tab_write` takes `lease: Option<&str>` — required on a session
  socket (`connect-required` without one, `taken-over` on a displaced one), accepted and ignored
  on a UI socket. `Conn::session_connect(takeover, client_label)` is what mints one; nothing in
  shed calls it outside tests yet.
- **A takeover no longer ends a stream.** It reclassifies in place and says so once with a
  non-terminal `session.driver_changed`. The only terminal envelope an event stream sees is
  `session.stopping`.
- **The watcher does not poll.** `RoostWatcher` runs one *observe cycle* — identify on conn A,
  `subscribe("")` on conn B, `tab.list` on conn A, then fold batches through
  `shed_core::roost::Fence` — and emits a `Snapshot` only when a row actually changed. A bare
  EOF and a revision gap are **resyncs** (a new cycle at once, no `Down`), bounded by
  `MAX_CONSECUTIVE_RESYNCS`. **`SHED_ROOST_POLL_MS` is gone**, with `POLL_INTERVAL` and the rest
  of the cadence: latency is the push, and the only sleep left is the failure backoff.

**What it drags in.** `roost-ipc`'s own leaf deps — **`anyhow`**, the **`tracing` facade** (a
facade only: no subscriber, no `tracing-subscriber`) and **`libc`** — are new to `shed-core`'s
dependency set but were *already* in this workspace's lock and already reached `shed-core-ffi`
through `uniffi`, `reqwest`/`hyper-util` and `tokio` respectively. So the Swift staticlib's
crate graph gains exactly one crate: `roost-ipc` itself. What it does gain is tokio features:
the workspace asks for `rt-multi-thread, macros, sync, time`, `shed-core` adds `net, io-util`
(the roost connection and its loopback pump — build-time now, not just dev-time), and roost-ipc
unifies in `fs, process, signal, rt`. Measured at `shed-core-ffi`, the delta from before this
dep is **+`process`, +`signal`, +`signal-hook-registry`** (`fs`/`net`/`io-util` already arrived
via reqwest). Nothing FFI-exported changes; `cargo tree -e features -p shed-core-ffi` prints the
resulting set. The workspace `rust-version` is **1.97**, which is roost-ipc's MSRV.

`shed-app` also takes the **`tracing` facade** directly (plan 014): the roost observer loop has
two things worth saying that are neither an error nor an update — a resync, and somebody else
taking the interactive lease. It adds no crate to the lock (roost-ipc already brings the same
facade into that graph), there is still **no subscriber** anywhere in this workspace, and a
client that installs none pays nothing.

**Android.** `cargo check -p shed-core --target aarch64-linux-android` is the early gate for
mobile (`roost-ipc` compiles for it cleanly — its `peer.rs` has a fail-closed non-linux/macOS
fallback). It needs the NDK's clang on the env for `ring`'s build script
(`CC_aarch64_linux_android`, `AR_aarch64_linux_android`,
`CARGO_TARGET_AARCH64_LINUX_ANDROID_LINKER`), the way `cargo-ndk` sets it in shed-mobile's
CI — that requirement predates roost-ipc.

**The `-dev` trap.** `roost_ipc::paths::BundleProfile::session()` (and `ssh::classify`) append
`-dev` when the **consuming** crate is a debug build, so a debug shed would look for a dev
roost and find nothing — which is why `shed_core::roost::paths` resolves the session socket
itself (release path first, `-dev` sibling only as a fallback) and shed never calls roost's
resolver. Related: `roost_ipc::ssh` reads `ROOST_SSH_BIN` / `TMPDIR` / `ROOST_TEST_MODE` from
the shed process env, so build `SshTunnelOptions` explicitly rather than with `from_env()`
outside tests.

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
cargo test -p shed-core --features test-support      # exports `roost::testing::FakeRoost`
cargo clippy --workspace --all-targets -- -D warnings
cargo clippy -p shed-app --features rc --all-targets -- -D warnings
cargo clippy -p shed-app --features broker --all-targets -- -D warnings
cargo clippy -p shed-app --features broker,rc --all-targets -- -D warnings
cargo test -p shed-opencode                          # the opencode agent-lane adapter
cargo test -p shed-opencode --features test-support  # exports `testing::FakeOpencode`
```

Note: `sx` (the crate that used to be a default member enabling shed-app's `rc` feature
via workspace-wide feature unification) was sunset in plan 016 (S7, #329). A bare
`cargo test`/`clippy --workspace` no longer compiles the `rc` modules at all — the
explicit `-p shed-app --features rc` legs above are now the ONLY coverage for them
(and what CI runs).

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

**One crate here ships on its own selector, deliberately OUT of that lockstep:**
`crates/shed-host-agent/VERSION` (the `host-agent` component). That file is a ship-
**selector** only — the shipped binary's version is the tag, injected at build time by
the component's goreleaser config (`SHED_HOST_AGENT_VERSION`) and read via
`option_env!` in `version.rs`. So `shed-host-agent version` on a released build reports
the release tag, NOT `CARGO_PKG_VERSION` (which follows the desktop selector and is not
bumped on a host-agent-only tag). Expect the two versions to differ in normal operation;
that is the design, not drift. See the root `RELEASING.md` "Component selection".
