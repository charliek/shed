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
  It also owns the one shared overflow policy (plan 018, module doc correction 13):
  `LaneSubscription`'s frame channel is bounded at `LANE_CHANNEL_CAPACITY` (1024 frames),
  and `LanePublisher` — deliberately **not** `Clone`, one per subscription — is the only
  way onto it: `publish` (`try_send`; a full channel answers `Publish::Lagged` and drops
  the frame), `publish_final` (consumes self and awaits; the terminal `Down` is the one
  frame that can never be the dropped one), and `wait_drained` (resolves only once every
  slot is free). Both adapters propagate a `Lagged` out of every emitting helper and
  reseed rather than silently resume, even on gx — the dropped frames may already be
  behind the client's cursor.
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
  `lane_view.rs` (plan 018 §3.5, ungated for the same reason `machine.rs` and
  `roost.rs` are) is the **staged agent-lane view** — `LaneView`/`LaneViewSnapshot`,
  moved down out of the Tauri crate — that folds a `shed_core::lane` subscription
  (messages, activity, generation, approvals) into what `lane.messages`/`lane.approvals`
  return, behind the same `Reset … Ready` staging the contract promises;
  `LaneView::snapshot(since_seq)` is the typed projection both a full read and a delta
  poll go through. It is ungated because mobile links `shed-app` with default features
  and needs the identical fold — the phone showing the same view the desktop shows is
  a property of one implementation, not two that have to agree.
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
- **`shed-gx`** — the **gx adapter** for `shed_core::lane`: the second
  implementation of `AgentLane`, against gx's remote lane (`gx-remote-api`) —
  a bearer-token HTTP API with a resumable `Last-Event-ID` cursor, unlike
  opencode's unauthenticated, cursor-less local server. Building it forced the
  twelve contract corrections recorded in `shed_core::lane`'s own module doc
  ("what the gx adapter changed"); see `docs/desktop/agent-lanes.md` for the
  end-to-end contract, including the two-URL split (reported vs. dial), the
  `healthz`/`instanceId` credential pin, bounded silent resume vs. reseed, and
  the `option_for` ambiguity refusal a real five-option gx permission forced.
  Shaped like `shed-opencode`: `discovery.rs` (the probe script + parser +
  `GxCredentialSource`), `transport.rs` (`GxTransport::dial`, called before
  every connect attempt so a moved forward is never dialled blind), `fold.rs`
  (pure: envelopes → rows + activity; gx's event-id counters are **not**
  monotonic in transcript order, so the cursor is the maximum counter seen,
  not the last applied one, and history is cut positionally), `watcher.rs`
  (the pump: seed, bounded silent resume, reconcile, reset → reseed, stall,
  `Down`), `testing.rs::FakeGx`, `examples/lane.rs` (the same manual-drive CLI
  shape as opencode's). Ring/backoff/feed are **not** duplicated here — they
  live in `shed_core::lane` (moved there by this same change) and this crate
  re-exports them, same as `shed-opencode` does. Its own dependency set is
  `shed-opencode`'s minus `regex`/`chrono`; not FFI-exported.
  **`fixtures/`** carries one recording from a real gx leader
  (`1.0.16+gx.12/{history.json, event-frames.jsonl, approvals.jsonl}`) and one
  golden derived from it (`fold.golden.json`) — but **unlike** `shed-opencode`'s
  two-golden split, there is no second implementation to port against: nothing
  else folds gx's wire, so the golden is **regression detection only**
  ("this is what the fold does today"), never a fidelity claim against some
  other producer. `crates/shed-gx/fixtures/README.md` is the one place that
  spells out that distinction, the two-step regeneration recipe (re-record
  live with `SHED_GX_LIVE=1 SHED_GX_RECORD=1 …`, re-derive offline with
  `SHED_GX_REGOLD=1 …`), and what the recording deliberately proves that a
  hand-written fixture would not think to (non-monotonic counters, gx's
  double-announced approval — a null-`method`/`request` placeholder followed
  by the real request on the same id — and its own by-counter resume edge,
  filed against gx as a known residual). In `default-members`.

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

**Bump recipe:** edit the `rev` in **all three manifests that pin it** — `crates/Cargo.toml`,
`desktop/tauri/src-tauri/Cargo.toml`, and shed-mobile's `rust/Cargo.toml` — then run
`cargo update -p roost-ipc` in each of the three workspaces so all three lockfiles
(`crates/Cargo.lock`, `desktop/tauri/src-tauri/Cargo.lock`, `shed-mobile/rust/Cargo.lock`) move
together. One manifest moving without its siblings is not a smaller version of the same bump —
it puts two disagreeing copies of `roost-ipc` in one dependency graph the moment shed-mobile's
git dep resolves `shed-core` against the old rev, and the desktop's own comment on this
(`desktop/tauri/src-tauri/Cargo.toml:55-59`) says so. Re-copy `fixtures/roost-vectors/` from the
new rev's `tests/ipc-vectors/` (updating that README's sha), commit all of it. Re-read roost's
`docs/reference/ipc-compatibility.md` on any bump crossing a `SESSION_PROTOCOL_VERSION` change —
`shed_core::roost::Conn::session_identify` refuses a mismatch by name rather than limping. roost
keeps **one `session.identify` vector per generation**
(`session.identify.response.v<N>.json`); shed vendors only the current one, so a generation bump
renames the vendored file and **every** reader of it moves with it. The full list is not
repeated here — `crates/fixtures/roost-vectors/README.md`'s reader-inventory table is the
canonical one (a plan-021 rewrite from prose to a table, because the prose omitted readers):
**three languages**, not just "both fakes" — Rust (`testing.rs`'s `include_str!`,
`fence.rs`'s own separate `include_str!`, and `roost_provider_vectors.rs`), Go (three files
that name the vector by filename), and Python (`desktop/tools/shedtest/fake_roost.py`'s
template, the reader plan 021 found the prose list had dropped). An older shape is a fake
*control* (`set_session_protocol`, `serve_without_features`), never a second vector to keep in
step.

**Two more files move with the protocol *integer* itself, separately from the vector file:**
`cmd/shed/roost_provider_test.go` and `internal/roostprovider/menu_test.go` both build their
expectations off `roostprovider.SpokenProtocol` rather than a literal — check them on a bump
anyway, since a future test could reintroduce a hardcoded number and silently stop moving with
the pin (plan 021 found exactly that: both had hard-coded "this shed speaks 5" before being
switched to the constant).

**What session protocol 6 gave us (plan 021), on top of what 5 already gave us.** The pin is at
`ee71e44…` and shed speaks generation **6**. Protocol 4's lease (roost R1, plan 014) stayed
gone — not narrowed, retired outright, with no replacement — and everything protocol 5 gave
(below) carries forward unchanged; 6's own changes are the second list further down:

- **Every op is open, not owned.** `lease` is dropped from all seven param structs it used to
  sit in (`TabWriteParams`, `EventsSubscribeParams`, `TabAttachParams`, `SessionSetThemeParams`,
  `SessionSetFocusParams`, `SessionSetAgentHooksParams`, `SessionPutFileParams`).
  `events.subscribe` takes zero arguments and `session.set_agent_hooks` is open to every
  same-UID client — there is no `connect-required` / `taken-over` / `already-connected` refusal
  left to hit, because there is nothing left to hold or contest.
- **`tab.effect` now reaches every subscriber.** roost's `event_push.rs` dropped the
  observer/driver filter that used to keep it off a plain `events.subscribe`, so watching a
  session sees the same fan-out an interactive client does.
  `shed_core::roost::Fence::apply_event`'s catch-all absorbs it unread — a watcher views no tab,
  so an effect frame is inert here.
- **A fresh subscribe gets back the daemon's incarnation.** `EventsSubscribeResult.session_id`
  is always present now; `shed_app::roost::RoostWatcher` compares it against the `session_id`
  `session.identify` returned on the other connection and treats a mismatch as an immediate
  resync, rather than waiting for the next cycle's EOF to fix it by accident.
- **The watcher still does not poll.** Unchanged from protocol 4: one *observe cycle* — identify
  on conn A, `subscribe()` on conn B, `tab.list` on conn A, then fold batches through
  `shed_core::roost::Fence` — and a `Snapshot` only when a row actually changed. A bare EOF and a
  revision gap are **resyncs** (a new cycle at once, no `Down`), bounded by
  `MAX_CONSECUTIVE_RESYNCS`. **`SHED_ROOST_POLL_MS` is gone**, with `POLL_INTERVAL` and the rest
  of the cadence: latency is the push, and the only sleep left is the failure backoff.

**What 6 changed, on top of that:**

- **`session.set_agent_hooks` became a pure raise.** `{mode, skip, client}` is gone;
  `SessionSetAgentHooksParams` is now `{agents, client}`. `mode`/`skip` retired with **nothing
  replacing them** — there is no narrowing direction left on this op, and removal is a
  deliberate local act on the host (`roostctl agent uninstall`), not something a client can ask
  for. See `crates/shed-core/src/roost/bootstrap/hooks.rs` for shed's `ROOST_WIRED_AGENTS` and
  the doc-extensions page for the user-facing consequence (re-widening).
- **`EventFrame::Ended` / the wire's `stream.ended` exists, and it is a resync, not a Down.**
  Its one defined reason is `backend-switch` — the daemon is alive and this stream's workspace
  is being replaced — so `shed_core::roost` treats it exactly like a bare EOF: a fresh cycle at
  once, counted against the same `MAX_CONSECUTIVE_RESYNCS` bound as any other resync.
  `session.stopping` is unchanged and still means the daemon itself is going away (a `Down`).
- **`TabOpenParams.activate` is new.** `Some(false)` opens a tab without selecting it or its
  project; absent (the desktop's choice — `roost_hosts.rs`'s one un-defaulted literal) and
  `Some(true)` both select it, matching every prior generation's only behavior.
- **`ServerCode` moved.** Gained `Busy`, `NotEnabled`, `UnknownField`, `MissingParam`,
  `DuplicateId`. `InvalidToken` is gone outright — there is no token concept left at 6 — and
  `TooManyTokens` was renamed to `TooManyAttaches`, a different limit (concurrent attaches, not
  lease tokens) rather than a straight survivor.
- **`session.identify`'s `SessionIdentify` gained `persist_error` and `ops`.** `persist_error`
  names why the last `state.json` write failed; `ops` lists the ops this session would actually
  dispatch right now (absent from an older session). Neither is consumed by shed's roost client
  today — see `crates/shed-core/src/roost/model.rs` if that changes. (`features` is unrelated
  and already gone: it retired off this same struct with the lease, at protocol 5, not here.)

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
cargo test -p shed-gx                                 # the gx agent-lane adapter
cargo test -p shed-gx --features test-support        # exports `testing::FakeGx`; live/regold tests still skip cleanly
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
