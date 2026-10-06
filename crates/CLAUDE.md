# CLAUDE.md — working in `crates/`

The shared **Rust client core** — one Cargo workspace (committed `Cargo.lock`,
`rust-toolchain.toml` pins the channel) whose logic backs every shed client so nothing is
re-implemented per language. The root `CLAUDE.md` owns the monorepo layout + release model;
`desktop/CLAUDE.md` owns the app that consumes this core.

## The crates

- **`shed-core`** — a *pure* Rust lib (no UI, no UniFFI): the reqwest(rustls) HTTP client, the
  SSE parser, defensive wire decoders, leaf-cert TLS pinning, the control-token FSM, a `config`
  parser, the pull-based `create` orchestration store, and `rc.rs` — which since plan 022
  (S6, charliek/shed#328) is the Remote-Control **model and nothing else**: `RcKind` /
  `RcState` / `RcActivity`, `RcCapabilities` / `RcKindFeatures`, the `RcSessionDto` wire
  row, the `RcFeed*` transcript rows `lane.rs` reuses, and `RcError` (shed-mobile's error
  mapping names it). The argv builders, the create/prompt invocations, the permission-mode
  table, the stdout decoders, the non-interactive ssh argv and the last of the claude.ai
  pane classifier all went with the hub; `hub_client.rs`, `rc_events.rs` and `rc_agents.rs`
  went with it entirely (`shell_quote_always` moved from `rc_agents` into `machine.rs`,
  where `display_line` — the one composer every machine transport shares — is its only
  caller). The Linux clients link it directly.
  `lane.rs` (plan 015; split in plan 025 C4, shed#391) is the **agent-lane contract** —
  the DTOs plus TWO async traits that normalize "a coding agent with sessions, a
  transcript and approvals": a machine-level `AgentSource` (`subscribe` → the live session
  list as `SourceEvent`s, `create_options`, `create`, `open(id)`) and the session-scoped
  `AgentLane` it opens (no verb takes an id; `settings`/`set`/`stop` have no default
  bodies). One adapter per agent implements both (opencode over its local HTTP server;
  craze over its per-machine hub, `shed-craze` — its source since plan 025 C7, its lane
  since C8 — in the slot plan 017's `gx` adapter held until plan 025 C1 retired it, shed#390).
  **Capabilities are per session and ride
  the stream** (`LaneEvent::Capabilities`, and `Settings` when they say so) — there is no
  capabilities getter, so a client reads them from its view, never caches them at open.
  `LaneEvent::Stale` is a non-terminal transport loss (a silent resume ends it with a lone
  `Ready` of the same generation); `Down` alone ends a lane; a source's outage is the
  non-terminal `SourceEvent::Offline`. Pure types, **no I/O** — the transport, fold, ring
  and reconnect loop belong to whatever crate implements them. It lives here, not in
  `shed-app`, because shed-mobile links the DTOs through FRB. `lane::conformance` (behind
  `test-support`) is the shared kit every adapter's tests run their streams through (the
  bracket, capabilities before `Ready`, seq, silent resume, `Down` last, no source-set
  `tab_id`). `time.rs` is the pure RFC 3339 → unix-ms parser (no `chrono`; correction 10).
  It also owns the one shared overflow policy (plan 018, module doc correction 13), for
  BOTH levels: the frame channel (`Subscription<T>`, aliased `LaneSubscription` /
  `SourceSubscription`) is bounded at `LANE_CHANNEL_CAPACITY` (1024 frames), and the
  generic `Publisher<T>` (`LanePublisher` / `SourcePublisher`) — deliberately **not**
  `Clone`, one per subscription — is the only way onto it: `publish` (`try_send`; a full
  channel answers `Publish::Lagged` and drops the frame), `publish_final` (consumes self
  and awaits; the terminal `Down` is the one frame that can never be the dropped one),
  `publish_waiting` (awaits room like `publish_final` without consuming — ONLY for the
  rows a terminal path flushes just before its `Down`, never on a live path), and
  `wait_drained` (resolves only once every slot is free). Every adapter propagates a
  `Lagged` out of every emitting helper and reseeds rather than silently resumes — the
  dropped frames may already be behind the client's cursor (plan 017's gx adapter, which
  also offered a bounded silent resume on its OWN cursor-honoured reconnect, proved the
  two are independent; it was retired in plan 025 C1, shed#390).
  **The FRB-mirror rule (load-bearing):** mobile HAND-mirrors every lane DTO into Dart, so
  every field is an owned `String`/`Option`/`Vec`/scalar — **no `serde_json::Value`, no
  `HashMap`, no borrowed lifetimes**; free-form payloads travel as a `String` of raw JSON
  (`LaneApproval::request_json`, `LaneAnswer::Raw`). A fielded enum becomes a Dart sealed
  class, a plain one a plain Dart enum. Same rule as `rc.rs`'s feed types, which `lane`
  reuses (`RcFeedMessage` IS the transcript row) — which is why those gained `Serialize`
  plus a tolerant `Deserialize` delegating to their existing `from_map` reader.
  `craze.rs` (plan 025 C5) is the **craze remote command**, pure like `machine.rs`: the
  `sh -c '<ladder>'` argv every client runs to reach `craze` on a host —
  `bridge_hub_argv`/`bridge_hub_command` (every hub connection), `providers_hub_argv` (the
  find-only probe, which never starts a hub) and `attach_argv(host_id)` (Open in terminal;
  the id must be twelve lowercase hex digits). The script is craze's published
  binary-finding ladder verbatim plus one change, an enhanced PATH applied only at the
  `exec`, composed over two tables (`Ladder { rungs, exec_path }`) so tests can re-root
  them; the `*_argv_jailed()` variants keep rungs 1–2 and no PATH change, for test mode
  only. `tests/machine-transport`'s `craze-bridge-hub`/`craze-providers-hub` scenarios
  EQUAL the composers' output (the Rust leg asserts it), and `tests/craze_ladder.rs` runs
  the ladder for real under every local `sh`.
- **`shed-app`** — the UI-free app-logic layer (`Backend`) the clients share; holds the
  embedded broker bridge (`broker_bridge.rs`, behind the non-default
  `broker = ["dep:shed-broker"]` feature — leg 3a.2), which since plan 022 is its ONLY
  non-default feature (`rc`, the `RcRunner` seam plus the re-exported `shed-rc-engine`,
  went with the hub). `roost.rs` (plan 013, ungated like `machine.rs` so mobile's
  default-features build links it) is the reach + watcher layer over `shed_core::roost` —
  `RoostReach`/`RoostWatcher`/`RoostPeek` — that both clients read a machine's or the local
  `roost-session` through. A bare `cargo test`/`clippy` run against `shed-app` **alone**
  (`-p shed-app`, no `--features`) skips `broker_bridge.rs` — cover it with
  `-p shed-app --features broker`, which is its ONLY coverage and what CI runs.
  `lane_view.rs` (plan 018 §3.5, ungated for the same reason `machine.rs` and
  `roost.rs` are) is the **staged agent-lane view** — `LaneView`/`LaneViewSnapshot`,
  moved down out of the Tauri crate — that folds a `shed_core::lane` subscription
  (messages, activity, generation, approvals, and — staged and swapped with the seed —
  the session's capabilities and settings) into what `lane.messages`/`lane.approvals`
  return, behind the same `Reset … Ready` staging the contract promises. Generations are
  MATCHED (only a `Ready` equal to the staged `Reset`'s swaps; only one equal to the live
  generation clears `stale`), and `stale` (the banner, set by `Stale` or `Down`) is kept
  apart from `ended` (set only by `Down` — the one thing a client reopens a lane on);
  `LaneView::snapshot(since_seq)` is the typed projection both a full read and a delta
  poll go through. It is ungated because mobile links `shed-app` with default features
  and needs the identical fold — the phone showing the same view the desktop shows is
  a property of one implementation, not two that have to agree.
  `craze_rows.rs` (plan 025 §3.6.3, ungated for the same reason) is **the craze row
  merge, D4 literally**: `fold_plan(roost_tabs, hub_rows)` for ONE machine — with the
  hub feed live (`Some`) every craze-owned roost tab is absorbed (attached to the hub row
  whose `provider_session_id` it names, or hidden when none does), with it down (`None`)
  nothing is; only craze ownership folds, the newest tab of a session attaches, two rows
  claiming one session resolve to the newer `since`/`startedAt` then the greater hostId.
  The desktop applies it in `roost_hosts.rs`; the phone (CM3) links the same function.
  `SshExec::spawn_duplex(command)` (C9) is the long-lived sibling of `SshExec::run` — the
  same pinned config and private ControlMaster, all three bands piped, killed on drop —
  that the desktop's remote craze dial runs `craze bridge --hub` through.
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
  implementation of both contract traits that talks to an opencode server's local HTTP
  API — the same server the TUI is already running, never a sidecar it launches itself.
  `OpencodeSource` (plan 025 P7) is the `AgentSource` (a 5 s poll of the session list,
  create with an optional first prompt, `open` binding an id with no I/O) and
  `OpencodeLane` the session-scoped `AgentLane` it opens (the pre-split verbs with the id
  bound; every seed carries its fixed `Capabilities` row; `settings`/`set`/`stop`
  answer "unsupported"). The Tauri client consumes it as a plain path-dep, opening each
  lane as `OpencodeSource::new(url, None).open(session_id)` (a machine row's
  `agent_lane` stamp, fed by roost's `server_url` report on the tab; see
  `docs/desktop/agent-lanes.md` for the end-to-end contract and its current limits).
  `fold.rs` is a **port** of the rc hub's `OpencodeFold`, and it is pinned as one —
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
  `rc_hub::watch` imported `shed_rc_engine::tmux::Tmux` — linking would have dragged the
  RC engine into an HTTP adapter. S6 (plan 022) deleted the hub, so `helpers.rs` is now
  the only copy and the golden is what still pins it. In `default-members`.
  (`shed-gx` — the **gx adapter**, plan 017's second implementation of
  `AgentLane` against gx's remote lane — lived here the same shape, with the
  twelve contract corrections it forced recorded in `shed_core::lane`'s own
  module doc; it was retired in plan 025 C1, shed#390, when gx and the other
  direct-agent kinds left shed for the craze lane. `shed-craze`, below, holds
  this slot now.)
- **`shed-craze`** — the **craze adapter** for `shed_core::lane` (plan 025 C7+, shed#392):
  one machine's craze **hub** — craze's per-machine process that lists every cursor, grok,
  gx and native session, says what a create can start, starts one, and splices a client
  through to a session's host — as an `AgentSource`, `CrazeSource`. **The transport is the
  client's** (P8): every connection is a fresh duplex to `craze bridge --hub` from a
  client-supplied `CrazeDial` — `ProcessDial` (a local `/bin/sh -c '<ladder>'`, run by
  absolute path; the ladder is `shed_core::craze`'s), `TcpDial` (the phone's loopback
  port), and the desktop's `SshExec` duplex (C9) — one for the roster, one per
  `createOptions`, one per create, because the hub answers one request at a time, in
  order. `conn.rs` is one NDJSON connection: craze's line limits **from the client's
  side** (never WRITE a line over 4 MiB; READ up to 16 MiB), id demux (host replies come
  out of order; a reply's id must be the request's JSON value verbatim, and every message
  `"jsonrpc":"2.0"`), notifications on a bounded channel the reader **never waits on** (a
  full queue ends the connection as `ConnEnd::Backlog` rather than stall the replies
  behind it), deadlines that cover the write as well as the reply, the **bounded
  preamble** (up to 16 non-JSON lines / 4 KiB before the first reply — a shed's `bash
  -lc` login profile — kept for the error; after it, any non-JSON line, a blank one
  included, is a fault), and the hub `hello` (protocol 1, **codecs event 1 and snapshot
  1**, `rosterSubscribe` + `connect`). `dial.rs` classifies a dial that never reached a hub, in
  plan 025 §3.3.2's precedence (exit 127 → `NotInstalled`; `unknown flag: --hub` on stderr
  — v0.0.1 — or a hub `hello` short of the rule → `TooOld`; anything else before `hello` →
  `Unreachable` with the stderr tail; a refused `hello` → `Failed`, `protocol_version` →
  `TooOld`) and classifies the find-only probe (`providers --hub --json`, which never
  starts a hub) by its stderr TEXT, never its exit code. `errors.rs` is craze's published
  code → `LaneError` table **verbatim, keyed on `data.code` only**, with P14's one
  deviation (a create refused `not_accepting/start_failed` → `Failed(data.cause)`).
  `CrazeSource::create` retries ONCE under the same `requestId` on an unknown outcome (a
  dropped connection, the 120 s deadline) and never on a definite answer;
  `is_outcome_unknown` is the one test a caller keeps its id on. A create keeps the row it
  answered with APART from the roster's (`Held`): until a roster lists that hostId or removes
  it — a seed that does not list it yet keeps it — or 10 min pass (craze's replay window);
  never for a hostId a roster let go within craze's 10-minute replay window (timestamped
  tombstones, a 4096 count only as a memory backstop, so a replayed create cannot resurrect
  it); at most 64 at once. `created_rows()` is that set, evaluated at the call — the one
  authority a client lists created rows from (the desktop keeps no copy). So `open(hostId)` on a just-created session binds a
  lane that knows its row at once; `dialling(dial)` is the same source — its rows — on
  another dial (the desktop runs a user's explicit `create_options`/`create` through an
  ungated one). A row's id is its **hostId** (P11). **`CrazeLane`** (plan 025 C8, `open` binds one with no I/O) is one
  session: its watcher (`watcher.rs` — the state machine table is its module doc) dials
  one connection per lane, splices to the host (`session.connect{hostId}` with the host
  `hello` pipelined), re-reads the host's own `sessions.list` row (the craze `sessionId`
  every session call carries — learned, and re-learned after `session_replaced`), and
  attaches: a snapshot seeds `Reset … Ready` (every `Ready` waits for its attachment's
  `synchronized` at the last seq held); after `Ready`, a lost connection is `Stale` and a
  redial that offers the cursor — the host decides: honoured is a SILENT resume (a lone
  `Ready`, same generation), refused is `Reset{cursor_lost:…}`. Bounds: 8 attaches per
  episode (reset at `synchronized`), dials give up 10 min into an outage (an outage ends
  only at a `synchronized`, so a host that answers attaches and never synchronizes is ended
  too); every dial is under a 30 s deadline (`dial::DIAL_DEADLINE` — for a source an
  `Offline`, for a create an unknown outcome retried under the same id), and an attachment
  that goes 60 s without a word before its `synchronized` is a dead connection; a
  client-channel lag (or `ConnEnd::Backlog`) drops the connection, waits for the drain and
  reseeds; terminal `Down` only for `session_closed`, `unknown_session`, `start_failed:
  <cause>`, a splice to another host (`protocol: …`, which every read refuses too) and
  the bounds — a `reset{omitted}` whose re-attach gets no answer is confirmed by a redial,
  never assumed closed; the rows flushed ahead of a `Down` wait for room like the `Down`;
  a seed's rows are capped at what the ring keeps so it always fits the channel. The
  verbs share the lane's connection (`send`/`cancel`/`answer`/`set`/`stop`, fresh
  commandIds per lane): issued while disconnected they wait 10 s then fail `Unavailable`;
  in flight at a drop, or past their 30 s deadline (which also closes the connection),
  they are "outcome unknown" and never resent — and a silent resume restates the
  session's `Settings` before its lone `Ready`, so a client showing a lost change "not
  confirmed" has the real value to replace it with. `session()` never dials; `approvals()`/
  `settings()` answer from a RUNNING watcher's fold once it seeded (the watcher that set
  it alone clears it), else read a snapshot on a connection of their own. `fold.rs` is
  craze's events/snapshots → append-only rows (craze's own wordings ported: `noteTodos`,
  `compactionNote`, the foreign-turn notes, the shell-context/attachment strip) with an
  approval book (`answer_body` maps the contract's answers onto `asks.answer`). **Rows
  follow craze's transcript, approvals its ENGINE ask registry** (Amendment A11): a
  sub-agent's ask (craze's fold child-ignores all four ask kinds) draws no row, yet is an
  approval — the registry has no agent field, and `pendingAsks` counts it — gated exactly
  as craze's `HiddenBy` (a question needs `askCards`, a plan `planCards`; a permission is
  never hidden); every seed and silent resume runs a FENCED read
  (attach, `asks.list` + `asks.get`, `session.sync`) and its `Ready` waits for every event
  through the sync's seq;
  `segment.rs` is shed-gx's segmenter, ported (8 KiB lossless splits, the 2 s flush
  clock); `settings.rs` is the settings data (plan 025 §3.10: craze's model order and the
  options' order, computed once; modes hidden when the session's `modes` capability is
  off) and `set` (C11) is `session.set` — `setting_for` binds a config change to the model
  the CLIENT displayed (`LaneSettingChange::Config`'s `for_model`, Amendment A13), else the
  lane's folded one, waiting for the lane's first settings rather than going out unbound
  (`forModel`), so craze refuses it `stale_model` (`NotAccepting`) once the session has left
  that model; the new value comes back on the stream, from the change's own `meta` delta —
  or, answered `rev: 0` (no delta will follow), from the confirmed value, which every
  running watcher applies (`LaneShared::confirmed`). `errors::craze_says` keeps a craze
  message that happens to begin "outcome unknown: " from reading as shed's own. `tests/recipe_settings.rs` drives it against the real
  hub with `craze-fake-agent -script permodel` as `cursor` (cursor's per-model catalogs;
  `Recipe::set_agents`, and `script_agent_with` for the agent's own
  `CRAZE_FAKE_DUMP_CALLS` record). Pure lib — serde, tokio, no `reqwest`, no `chrono` — not
  FFI-exported, builds for `aarch64-linux-android` (mobile links it). In
  `default-members`. **Tests:** `tests/wire.rs` runs every vendored WIRE fixture
  (`fixtures/wire/`, craze's `internal/fakehost/testdata/wire` at the sha
  `fixtures/wire.PIN` names, its `README.md` included — `make check-craze-pin` requires
  `wire.PIN` == `CRAZE_TEST_SHA`, and CI's `craze-binaries` action diffs the two trees);
  `tests/recipe_source.rs` is craze's own **hermetic recipe** (`testing::Recipe`, behind
  `test-support`): the REAL hub over `craze-fake-host` entries, creates spawning
  `craze-fake-agent` as grok, every craze process under the recipe's six variables with a
  short 0700 `CRAZE_RUNTIME_DIR` under `/tmp` (never `~/.cache`, which craze refuses),
  binaries copied into a private `PATH`, the hub's pid read from its record and checked
  before any signal, and craze's own `cleanup` as the teardown. Its cells **skip** without
  `SHED_CRAZE_BIN_DIR` (`make craze-binaries` prints the line) and **fail** instead under
  `SHED_CRAZE_REQUIRE=1`, which CI's `core-linux` sets. `tests/recipe_lane.rs` drives lanes
  the same way (fake-host ops for the asks, a hub-created `craze serve` for `stop`, a
  `HookDial` that kills a lane's bridge for the silent resume); `tests/lane.rs` and
  `tests/overflow.rs` pin the watcher's edges against a SCRIPTED host (the bounds on tokio's
  paused clock). `fixtures/0.1.0+gx/` is one LIVE recording of a gx session through a real
  hub, replayed offline into `fold.golden.json` — **regression detection only**;
  `fixtures/README.md` says what each artifact claims and how to re-record
  (`SHED_CRAZE_LIVE=1 SHED_CRAZE_RECORD=1 … --test live`) and re-derive
  (`SHED_CRAZE_REGOLD=1`). Every source and lane frame goes through
  `shed_core::lane::conformance`.

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
`2bc71fa…` (roost **v0.0.21**, roost's bug-fix release after v0.0.20; v0.0.20 was the first
release that speaks 6, and its `roost-ipc` crate was byte-identical to `ee71e44…`, where 6
landed) and shed speaks generation **6**. 0.0.21's only `roost-ipc` API change shed sees is
`TabOpenParams.cwd_from_tab`. Protocol 4's lease (roost R1, plan 014) stayed
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
cargo test -p shed-app --features broker             # the embedded broker bridge (3a.2)
cargo test -p shed-core --features test-support      # exports `roost::testing::FakeRoost` + `lane::conformance`
cargo clippy --workspace --all-targets -- -D warnings
cargo clippy -p shed-app --features broker --all-targets -- -D warnings
cargo test -p shed-opencode                          # the opencode agent-lane adapter
cargo test -p shed-opencode --features test-support  # exports `testing::FakeOpencode`
cargo test -p shed-craze                             # the craze adapter (recipe cells skip)
# the craze recipe against the real hub: build the pinned binaries, then require them
make -C .. craze-binaries                            # prints SHED_CRAZE_BIN_DIR=…
SHED_CRAZE_BIN_DIR=… SHED_CRAZE_REQUIRE=1 cargo test -p shed-craze --all-targets --features test-support
```

Note: `broker` is the one non-default feature left in this workspace. A bare
`cargo test`/`clippy --workspace` does not compile `broker_bridge.rs` at all, so the
explicit `-p shed-app --features broker` legs above are its ONLY coverage (and what CI
runs). `shed-app`'s other non-default feature, `rc`, went with `shed-rc-engine` and the
RC hub in plan 022 (S6, #328); `sx`, which used to enable `rc` workspace-wide by feature
unification, was sunset in plan 016 (S7, #329).

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
