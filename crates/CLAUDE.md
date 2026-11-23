# CLAUDE.md — working in `crates/`

The shared **Rust client core** — one Cargo workspace (committed `Cargo.lock`,
`rust-toolchain.toml` pins the channel) whose logic backs every shed client so nothing is
re-implemented per language. The root `CLAUDE.md` owns the monorepo layout + release model;
`desktop/CLAUDE.md` owns the app that consumes this core.

## The crates

Plan 038 split the old `shed-core` / `shed-app` mix into a zero-cycle DAG and
retired the opencode adapter crate. Default members are every crate below except
`shed-broker-harness`. WebKitGTK/Tauri deps stay out of this workspace.

```
shed-lane (pure leaf: no shed-core, reqwest, roost-ipc, or chrono)
   ├── shed-core ── shed-source ── shed-broker
   │       └── shed-app ── shed-desktop-core
   ├── shed-craze ────────────┘
   └── shed-roost (sole direct roost-ipc consumer) ──┘
shed-provision (no workspace crate) ── shed-provision-providers
       └──────────────────────────────────┴── shed-desktop-core
shedctl (no internal deps)
```

- **`shed-lane`** — pure agent-lane and RC DTO leaf (`lane` including
  `conformance` under `test-support`, `rc` with `RcSession` / `RcKind` /
  `strip_format_chars`, `text`, `time` including `rfc3339_z`, the craze
  command ladder in `craze`, `shell_quote` (`shell_quote_always`,
  `display_line`), `lane_view`, `slash_menu`, `craze_rows`, `backoff`).
  `tests/machine_transport_contract.rs` and `tests/craze_ladder.rs` live here.
  Android-clean. `RcKind::Opencode` stays so a passive roost tab can still
  name the tool; there is no opencode adapter.
- **`shed-core`** — pure shed-server protocol (`http`, `models`, `sse`,
  `create`, `tls`, `token`, `csr`, `certuser`, `authfail`, `identityfile`,
  `terminal`, `approval`, `config` for `servers:` + `default_server` +
  `known_hosts_path()`). Depends on `shed-lane` for `RcSession` and the text
  helpers `OverviewShed` and credential-source display use. **No `roost-ipc`,
  no `machines:` config, no machine-insert editor.** Keeps `regex` (`token.rs`) and `libc`
  (`identityfile.rs`). Android-clean.
- **`shed-roost`** — the **sole** direct `roost-ipc` crate in `crates/`:
  `conn`, `error`, `fence`, `model`, `paths`, `bootstrap` (including
  `bootstrap/machines.rs`, the install state machines — not a `machines:`
  config), `testing::FakeRoost` under `test-support`, `spawn_gate`,
  `ssh_endpoint` (`SshEndpoint`, `check_identity`, `user_at_host`, `ssh_argv`,
  `FixedPort`, `ForwardError`), and `reach` (`RoostReach`, `RoostWatcher`,
  `tab_open` / `tab_open_fenced`, `launch_argv`, `ReachKind`).
  `lane_adapter_exists` is false, so `roost_capabilities()` advertises
  `feed: "activity", watch: false` for an opencode tab. `RELEASE_PIN` lives in
  `src/bootstrap/source.rs`. Android-clean.
- **`shed-app`** — toolkit-free shed client core: `Backend`, `AuthModeRegistry`,
  `Coordinator`, `AuditStore`, `BrokerEvent`, the embedded broker bridge
  (`broker_bridge.rs`, non-default `broker` feature — its only non-default
  feature), `traits`, `fakes`, `timefmt`. **No `roost-ipc`, no `shed-roost`,
  no `shed-lane`, no `saphyr-parser` under default features.** A bare
  `cargo test -p shed-app` skips `broker_bridge.rs`; cover it with
  `--features broker`.
- **`shed-desktop-core`** — toolkit-free **shed-only** runtime the Tauri shell
  (and a future iced UI) hosts: `ShedId` (`roost:<server>/<shed>`),
  `ShedHosts`, `ShedCraze` (remote attach-only), `ShedLanes`, `slash`, and
  `shutdown_children`. Depends on `shed-app` with **default features only —
  never `broker`**. No `tauri`, no `iced`, no `shed-broker`, no direct
  `roost-ipc`.
- **`shedctl`** — a headless UDS/IPC client with no dependency on the other
  crates here (`serde_json`, `base64`, `libc`), shipped in the Linux `.deb`.
  In `default-members`.
- **`shed-source`** (plan 039 §3.7) — what a machine needs to be a credential source,
  extracted unchanged from `shed-broker`: `machine` (`<state>/broker/machine.json`),
  `paths` (`state_dir`: `SHED_BROKER_STATE_DIR`, else the per-user data dir; plus
  `user_home_dir`/`expand_tilde`), `credstore` (`<state>/broker/creds`), `minter` (the
  SSH-bootstrap minter and credential source; `bootstrap` is internal), `controltoken`
  (the control-token provider, which resolves servers through an injected
  `ServerResolver`: this crate has no YAML parser, so the broker passes its saphyr-backed
  `discovery::load_discovered_servers`), `target` (`ServerTarget`) and `time` (the RFC3339
  helpers). `shed-broker` re-exports every module under its old path (`status`,
  `discovery` and `config` re-export the helpers), so no broker caller or embedder moved.
  Reaches only `shed-core`/`shed-lane`, never `saphyr-parser`. Its `test-support` feature
  (the minter's test seams and `minter::tests` helpers) is enabled only by
  `shed-broker`'s `[dev-dependencies]`, never in a shipped build. The control-token
  provider's tests stay in the broker (`controltoken_tests.rs`), since they resolve
  through the broker's reader. Default member.
- **`shed-broker`** — the embeddable credential broker core: the shed-server plugin bus,
  the multi-server supervisor + discovery watcher, the SSH/AWS/Docker/egress credential
  backends, the SSH-bootstrap minter + control-token provider (from `shed-source`), the approval/audit seams
  (incl. the always-compiled `AuditFanout` fan-out and the native `touchid` gate), the
  `config` reader, socket path-resolution + liveness probes, and the health types
  (`ServerHealth`, `SshSigningStatus`). Embedded in-process by the desktop app (through
  `shed-app`'s `broker` feature) and hosted for the wire harness by the test-only
  **`shed-broker-harness`**. The standalone daemon that used to drive it was removed in
  plan 037. Carries no WebKitGTK concern.
  `sign_check` (plan 035 §3.5) is the pure parser and policy for what the machine will
  sign for a shed: an exact SSH login request or an exact `git` SSHSIG blob, nothing
  else, returning an `Approved` that is the only way to sign the bytes it checked. Its
  vectors in `shed-broker/fixtures/sign_check/` are REAL OpenSSH captures: re-record
  them only with that directory's `capture.sh` (Docker), never by hand. Its two
  machine-wide settings are `ssh.allow_users` and `ssh.offer_keys` (`config.rs`,
  `ExtensionsYaml::ssh_sign_policy`); the key filter itself is
  `ssh_backend::FilteredSshBackend`, wrapped around whichever SSH backend resolved in
  the embedded broker. It asks the backend nothing of its own (the
  agent-forward transcripts depend on that), and a key it does not offer fails with
  the backend's own unknown-key error, so a shed cannot tell "not offered" from "not
  present".
- **`shed-broker-harness`** — a TEST-ONLY binary (plan 037) that hosts the desktop app's
  embedded broker (`shed-app`'s `broker` feature, minus the Coordinator) for the wire
  harness `tests/broker-wire/`: a config in, the bus and the audit file out, and a
  control socket for its status snapshot (`src/status.rs`, the JSON shape the goldens
  record), approval frames and decisions. A workspace member but **not** a default
  member: as one, a plain `cargo build`/`cargo test` would turn `broker` on for the whole
  workspace by feature unification and hide dead code in the default build shed-mobile
  links. For the same reason `make -C desktop core-lint`'s workspace leg excludes it and
  a leg of its own lints it. It ships in no release (`tests/not_shipped.rs`).
  `crates/shed-core/tests/crate_isolation.rs` (cargo metadata) holds the DAG above:
  no `roost-ipc` outside `shed-roost`, no `reqwest`/`chrono`/`shed-core` in
  `shed-lane` or `shed-craze`, no `shed-broker`/`saphyr-parser` in default
  `shed-app`, no `tauri`/`iced`/direct `roost-ipc` in `shed-desktop-core`.
  The same test holds plan 039's provision crates: `shed-provision` reaches no
  workspace crate, `shed-provision-providers` only `shed-provision`, neither
  `saphyr-parser`/`tokio`/`reqwest`, and `shed-core`, `shed-lane`,
  `shed-roost`, `shed-craze` and default `shed-app` reach neither (shed-mobile
  links those). `shed-source` reaches only `shed-core`/`shed-lane` and no
  `saphyr-parser`, and the same mobile crates do not reach it.
- **`shed-provision`** (plan 039) — the source side of the provision contract
  `tests/provision-contract/v1/`: the documents and every rule that judges
  them, matching `internal/delivery` rule for rule and in order (requests,
  answers, records; printable = Go's `unicode.IsPrint`, from a table
  `gen/printable.go` generates with Go), the stream's SSE framing, the
  `Provider` and `Reader` traits, the `only` rules, the answer builder (git
  config merge, conflicting and nested targets, limits), keyed fingerprints and
  the redacted `Preview`. No file or process access of its own: providers read
  through a `Reader`. Its `contract_tests` module reads every fixture and the
  manifest (so `make -C desktop core-linux` mounts `tests/provision-contract`).
  Default member.
- **`shed-provision-providers`** (plan 039) — the five providers (`ssh-key`,
  `github`, `api-keys`, `envsecrets-login`, `files`) and `delivery.toml`
  (`~/.config/shed/delivery.toml`, walked against its schema as `toml`'s
  spanned document; schema in `src/settings.rs`). A settings error is fixed
  text with a line, column and schema key path, never the file's text. No YAML
  parser: the envsecrets config's two flat keys are read with a strict line
  pattern. A `.pub` is exactly one key line, re-written, and must match the
  private key (OpenSSH, or derived from PEM/PKCS#8 RSA, ECDSA, DSA). Problem
  messages are fixed text, never a value or a command's output. The `Reader`
  follows links on purpose; its docs say what a disk reader must do (open
  non-blocking, check the opened descriptor is a regular file, read at most
  the limit). Default member. `shed-desktop-core` depends on
  both (re-exported as `shed_desktop_core::delivery`); the runtime that answers
  a server's delivery stream is plan 039 A6's.
- **The opencode adapter crate was removed in plan 038.** Craze is the only agent lane.
  An opencode roost tab still renders as a passive status row (`RcKind::Opencode`);
  interactive opencode sessions belong to the `roost` app.

- **`shed-craze`** — the **craze adapter** for `shed_lane` (plan 025 C7+, shed#392):
  one machine's craze **hub** — craze's per-machine process that lists every cursor, grok,
  claude and native session, says what a create can start, starts one, and splices a client
  through to a session's host — as an `AgentSource`, `CrazeSource`. **The transport is the
  client's** (P8): every connection is a fresh duplex to `craze bridge --hub` from a
  client-supplied `CrazeDial` — `ProcessDial` (a local `/bin/sh -c '<ladder>'`, run by
  absolute path; the ladder is `shed_lane::craze`'s), `TcpDial` (the phone's loopback
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
  plan 025 §3.3.2's precedence as plan 032 §3.5 (PC12) extends it — pinned row by row by a
  table cell (exit 127 → `NotInstalled`; `unknown flag: --hub` on stderr — v0.0.1 — or a hub
  `hello` short of the rule → `TooOld`; exit 1 with craze 0.1.x's `craze bridge: unknown or
  malformed flag` → `NoStartUnsupported`, which only a `--no-start` dial can draw; exit 3
  with `no hub is running` or an empty stderr → `NoHub`, `SourceOffline::NoHub`, a lane's
  `Unavailable("no craze hub is running")`; anything else before `hello` → `Unreachable`
  with the stderr tail; a refused `hello` → `Failed`, `protocol_version` → `TooOld`) and
  classifies the find-only probe (`providers --hub --json`, which never starts a hub) by
  its stderr TEXT, never its exit code. **The `NoHub` row is the documented exception** to
  that rule (PC14e): it reads exit code 3, because craze documents exit 3 as `--no-start`'s
  contract (`cli.md`, "craze bridge") and every other bridge failure exits 1 — and it still
  requires craze's words or none, so an unrelated exit 3 stays `Unreachable`. **The
  fallback:** `CrazeDial::fall_back(&err)` (default `false`; a wrapping dial MUST forward
  it) is asked on a `NoStartUnsupported` once `connect_hub` has closed the failed
  connection, and on `true` `connect_hub` dials ONCE more; one it does not take reads as
  `Unreachable` (the desktop's `SshCrazeDial` takes it once per source, swapping to its
  probe-then-`bridge --hub` legacy). `errors.rs` is craze's published
  code → `LaneError` table **verbatim, keyed on `data.code` only**, with P14's one
  deviation (a create refused `not_accepting/start_failed` → `Failed(data.cause)`) — and a
  reopen's own `LaneReopenError` map, whose reason-keyed `not_accepting` row is a documented
  exception (below).
  `CrazeSource::create` retries ONCE under the same `requestId` on an unknown outcome (a
  dropped connection, the 120 s deadline) and never on a definite answer;
  `is_outcome_unknown` is the one test a caller keeps its id on. A create keeps the row it
  answered with APART from the roster's (`Held`): until a roster lists that hostId or removes
  it — a seed that does not list it yet keeps it — or a lane opened through the source sees
  the session end, or 10 min pass (craze's replay window); never for a hostId a roster (or
  such a lane) let go within craze's 10-minute replay window (timestamped tombstones, a 4096
  count only as a memory backstop, so a replayed create cannot resurrect it); at most 64 at
  once. **The lane's end is load-bearing** (live leg 1's ghost row): craze's roster removes
  only a host it SENT, so a session created and stopped before its next flush is never
  removed by it — the lane, holding the source's rows weakly, lets the created row go and
  tombstones it on a terminal `Down` of `session_closed`/`unknown_session`/`start_failed`,
  or on ANY end of the lane's LAST running watcher once the host took its `session.stop`
  (the receipt is craze's word the close follows) — by the receipt itself when no
  subscription is left to see the close — said before the `Down` is published for every end
  the watcher sees (a receipt whose wake loses the race to a lane fault's `Down` says it just
  after it: the row still goes), once per lane; `on_created_gone(f)` is the
  client's hook for that one change no roster frame says (the desktop re-reads its listing).
  `created_rows()` is that set, evaluated at the call — the one
  authority a client lists created rows from (the desktop keeps no copy). So `open(hostId)` on a just-created session binds a
  lane that knows its row at once; `dialling(dial)` is the same source — its rows — on
  another dial (the desktop runs a user's explicit `create_options`/`create` through an
  ungated one). A row's id is its **hostId** (P11).
  **Saved sessions and reopen** (plan 032 §3.4, craze 0.2.0's hub capability `reopen`,
  which `SourceCapabilities.reopen` and — read from the SAME `hello` — `LaneCreateOptions.reopen`
  carry): `saved()` is `create_options`-shaped (`sessions.saved{limit: 50}`, never `subAgents`)
  and `reopen(id)` is `session.reopen{id}` on a connection of its own, each gated on its
  connection's `hello` — no `reopen` there is `Unavailable` in `errors::reopen_unsupported`'s
  words (the one wording, which names `kill <pid>` only for a version that PARSES below 0.2.0
  and a known pid — never an inferred one), nothing sent. A reopen allows `REOPEN_DEADLINE`
  (200 s, craze's "at least") per answer; a LOST answer (connection ended, deadline) is sent
  again ONCE on a fresh connection; a second loss, a resend that cannot reach a hub that
  reopens, or a success shed cannot read is `LaneReopenError::Unavailable` worded
  `errors::REOPEN_LOST` ("Retry is safe" — a reopen is idempotent, so never "outcome unknown";
  `is_reopen_lost` tells it). Its answer's row is a **created row** that is FRESH: it lifts a
  tombstone for its hostId instead of being refused by one. A reopen that may have started a
  session (a success, a lost answer) **wakes** every roster pump of the source (clones
  included) that is waiting out its reconnect backoff. `LaneReopenError` is
  `Gone`/`StartFailed`/`Unavailable`/`Lane(LaneError)`, mapped by `errors::reopen_error` —
  and its `not_accepting` row is **the documented exception to "keyed on `data.code` ONLY"**:
  reason `not_resumable` is `Gone` (drop the saved row), `start_failed` is `StartFailed` (show
  the cause, keep the row), because craze's own reopen table gives the two opposite client
  actions and `NotAccepting` is a unit variant; only `session.reopen` reads that reason
  (`shed_lane`'s error table and `errors.rs` say the same). `tests/recipe_source.rs`'s
  reopen cells are craze's reopen recipe (the index seeded through
  `Recipe::write_saved_sessions`, the agent the fake agent's `grok-load`). **`CrazeLane`** (plan 025 C8, `open` binds one with no I/O) is one
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
  it alone clears it), else read on a connection of their own — `approvals()` the
  engine's ask registry (`asks.list` + `asks.get`, Amendment A11), `settings()` a
  snapshot. `fold.rs` is craze's events/snapshots → append-only rows (craze's own wordings ported: `noteTodos`,
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
  off; each model's craze 0.2.0 effort facts — `efforts`/`defaultEffort`/`switchEffort`,
  a native session's — as `LaneChoice::effort`, plan 032 §3.3) and `set` (C11) is
  `session.set` — `setting_for` binds a config change to the model
  the CLIENT displayed (`LaneSettingChange::Config`'s `for_model`, Amendment A13), else the
  lane's folded one, waiting for the lane's first settings rather than going out unbound
  (`forModel`), so craze refuses it `stale_model` (`NotAccepting`) once the session has left
  that model; the new value comes back on the stream, from the change's own `meta` delta —
  or, answered `rev: 0` (no delta will follow), from the confirmed value, which every
  running watcher applies (`LaneShared::confirmed`). `errors::craze_says` keeps a craze
  message that happens to begin "outcome unknown: " from reading as shed's own. `tests/recipe_settings.rs` drives it against the real
  hub with `craze-fake-agent -script permodel` as `cursor` (cursor's per-model catalogs;
  `Recipe::set_agents`, and `script_agent_with` for the agent's own
  `CRAZE_FAKE_DUMP_CALLS` record).
  Plan 029 C2: `slash.rs` reads craze's `slash` section PRESENCE-AWARE and row by row
  (craze's own `wireSlashIn` rule — a malformed row or a non-token name drops that row
  alone, a `slash`/`items` of the wrong type is a present, EMPTY catalog, never an
  absent one; unknown values kept as `Other`; no cap — 512 is craze's snapshot bound),
  not through `SettingsSections::from_value`. The fold holds the catalog (a snapshot's
  `settings.slash`, else none — never across a restore; a `meta` delta's replaces it
  whole) and a title overlay (snapshot `settings.title`, then `meta` `state.title`; empty
  says nothing; cleared at every fresh `sessions.list` row, as the activity is). The
  watcher stages `Slash` in a seed only when the snapshot carried one, publishes each
  change after `Ready`, restates it before a silent resume's `Ready`, and re-emits
  `Session` on a title change. `set_title` is `session.setTitle` on the live connection,
  like `stop` (never resent); its `index_write` is `errors::set_title_error`'s "renamed,
  but craze could not record it: <cause>" (`index_write_cause`). A create's `title` goes only to a
  hub whose `hello` says `createTitle`, judged on EACH attempt: fresh → `Failed`, nothing
  sent; a retry or held replay → outcome unknown, id and title kept. `testing`'s
  `reopen_hub_capabilities` is 0.2.0's (`reopen`, plan 032),
  `full_hub_capabilities` is 0.1.1's (`createTitle`), `hub_capabilities_0_1_0` and
  `session_caps` (no `slashCatalog`) the older shapes, `slash_session_caps` the newer.
  The 0.1.0+gx golden's projection drops the two plan-029 capability bits (pinned by
  their own cell), so the golden stays byte-identical. Pure lib — serde, tokio, no `reqwest`, no `chrono` — builds for `aarch64-linux-android` (mobile links it). In
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
  `shed_lane::conformance`.

`fixtures/` holds the real-shaped JSON/YAML samples (server info, `shed list`, `system df`,
egress profiles, enriched image, config) that the Rust decoders assert against — keep them
byte-real, not hand-trimmed.
`fixtures/roost-vectors/` is a **vendored copy** of roost's own golden IPC vectors at the
pinned rev (see below); its README carries the source sha and roost's never-semantically-edit
rule.

## The one git dependency: `roost-ipc`

`shed-roost` is the **sole** direct consumer of `roost-ipc` (the Roost Pivot, plan 013) — the only git dependency in
this workspace, and it is **rev-pinned in `crates/Cargo.toml`'s `[workspace.dependencies]`, not just in `Cargo.lock`**:

```toml
roost-ipc = { git = "https://github.com/charliek/roost", rev = "<sha>" }
```

shed-mobile consumes `shed-core` **as a git dependency**, and cargo does not inherit a
dependency's lockfile — a `branch = "main"` pin here would let mobile resolve whatever `main`
was on the day its own lock was regenerated, against a crate that publishes **no Rust-API
stability promise** (the pin is what absorbs that). With the rev in the manifest, mobile's
lock cannot disagree, so `shed-mobile/scripts/check-lock-rev.sh` needs no change.

**Bump recipe:** edit the `rev` in `crates/Cargo.toml`'s `[workspace.dependencies]`
(the desktop workspace no longer pins `roost-ipc` itself; it reaches it only through
`shed-roost`) and in shed-mobile's `rust/Cargo.toml`, then run `cargo update -p roost-ipc`
in `crates/`, in `desktop/tauri/src-tauri/` (Tauri locks `roost-ipc` transitively through
`shed-roost`, so `desktop/tauri/src-tauri/Cargo.lock` moves with the pin), and in
shed-mobile's `rust/` so the lockfiles move together. Re-copy
`fixtures/roost-vectors/` from the new rev's `tests/ipc-vectors/` (updating that README's sha),
commit all of it. Re-read roost's
`docs/reference/ipc-compatibility.md` on any bump crossing a `SESSION_PROTOCOL_VERSION` change —
`shed_roost::Conn::session_identify` refuses a mismatch by name rather than limping. roost
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
`TabOpenParams.cwd_from_tab`. **The library rev and the binary pin are different releases of
the same generation:** `RELEASE_PIN` (the `roost-session` the bootstrap installs and the images
bake) is roost **v0.0.26**, the last release speaking 6, while `roost-ipc` stays at the v0.0.21
rev; 0.0.22–0.0.26 only added fields and events shed's decoders already ignore (`dev`/`git_sha`
in `identify`, `exe_path` in `session.identify`, `Tab.password_input`/`dropped_reports`,
`tab.password_input`). roost **v0.0.27** moved the session protocol to **7** and v0.1.0 kept it
(`LATEST_KNOWN_RELEASE` says so); moving shed there is a migration, not a pin move: the
`roost-ipc` bootstrap API changed (`RemotePlatform` for `RemoteArch`, `map_arch`/`check_os`
gone), `Tab.state` became a projection that includes program status, the Linux session socket
moved to `~/.local/state/roost-session/run/roost.sock` (`roost::paths`' table), and the Go
`roostprovider` (`SpokenProtocol`) and the phone speak 6 too. Protocol 4's lease (roost R1, plan 014) stayed
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
  `shed_roost::Fence::apply_event`'s catch-all absorbs it unread — a watcher views no tab,
  so an effect frame is inert here.
- **A fresh subscribe gets back the daemon's incarnation.** `EventsSubscribeResult.session_id`
  is always present now; `shed_roost::RoostWatcher` compares it against the `session_id`
  `session.identify` returned on the other connection and treats a mismatch as an immediate
  resync, rather than waiting for the next cycle's EOF to fix it by accident.
- **The watcher still does not poll.** Unchanged from protocol 4: one *observe cycle* — identify
  on conn A, `subscribe()` on conn B, `tab.list` on conn A, then fold batches through
  `shed_roost::Fence` — and a `Snapshot` only when a row actually changed. A bare EOF and a
  revision gap are **resyncs** (a new cycle at once, no `Down`), bounded by
  `MAX_CONSECUTIVE_RESYNCS`. **`SHED_ROOST_POLL_MS` is gone**, with `POLL_INTERVAL` and the rest
  of the cadence: latency is the push, and the only sleep left is the failure backoff.

**What 6 changed, on top of that:**

- **`session.set_agent_hooks` became a pure raise.** `{mode, skip, client}` is gone;
  `SessionSetAgentHooksParams` is now `{agents, client}`. `mode`/`skip` retired with **nothing
  replacing them** — there is no narrowing direction left on this op, and removal is a
  deliberate local act on the host (`roostctl agent uninstall`), not something a client can ask
  for. See `crates/shed-roost/src/bootstrap/hooks.rs` for shed's `ROOST_WIRED_AGENTS` and
  the doc-extensions page for the user-facing consequence (re-widening).
- **`EventFrame::Ended` / the wire's `stream.ended` exists, and it is a resync, not a Down.**
  Its one defined reason is `backend-switch` — the daemon is alive and this stream's workspace
  is being replaced — so `shed_roost` treats it exactly like a bare EOF: a fresh cycle at
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
  today — see `crates/shed-roost/src/model.rs` if that changes. (`features` is unrelated
  and already gone: it retired off this same struct with the lease, at protocol 5, not here.)

**What it drags in.** `roost-ipc`'s own leaf deps — **`anyhow`**, the **`tracing` facade** (a
facade only: no subscriber, no `tracing-subscriber`) and **`libc`** — sit on `shed-roost`,
not on `shed-core`. The workspace asks for tokio `rt-multi-thread, macros, sync, time`;
`shed-roost` adds `net, io-util, process` (the roost connection and its loopback pump).
`cargo tree -e features -p shed-roost` prints the resulting set. The workspace
`rust-version` is **1.97**, which is roost-ipc's MSRV.

`shed-roost` takes the **`tracing` facade** directly (plan 014): the roost observer loop has
two things worth saying that are neither an error nor an update — a resync, and a session
the watcher did not expect. It adds no crate to the lock (roost-ipc already brings the same
facade into that graph), there is still **no subscriber** anywhere in this workspace (the
Tauri app installs one in its OWN workspace — `desktop/tauri/src-tauri/src/applog.rs`, #414),
and a client that installs none pays nothing.

**Android.** `cargo check --target aarch64-linux-android -p shed-core -p shed-lane -p shed-roost -p shed-craze` is the early gate for
mobile (`roost-ipc` compiles for it cleanly — its `peer.rs` has a fail-closed non-linux/macOS
fallback). It needs the NDK's clang on the env for `ring`'s build script
(`CC_aarch64_linux_android`, `AR_aarch64_linux_android`,
`CARGO_TARGET_AARCH64_LINUX_ANDROID_LINKER`), the way `cargo-ndk` sets it in shed-mobile's
CI — that requirement predates roost-ipc.

**The `-dev` trap.** `roost_ipc::paths::BundleProfile::session()` (and `ssh::classify`) append
`-dev` when the **consuming** crate is a debug build, so a debug shed would look for a dev
roost and find nothing — which is why `shed_roost::paths` resolves the session socket
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

**The scoped exception:** `shed-broker` (the broker core, home of the extensions `config`
reader) depends on **`saphyr-parser`** (pure-Rust, no-serde, no-C, no encoding_rs,
`default-features = false`) to back ITS `yaml_lite::parse`. Justification: the shipped
`configs/extensions.example.yaml` uses block-style `docker.registries:` sequences the line/colon
reader silently dropped, and Go's `LoadConfig` rejects malformed YAML the hand-rolled reader could
not detect — a real Go-vs-Rust divergence on the product's own default config. `saphyr-parser`
sits behind the `Node` interface (swap-insulation for its pre-1.0 API). It — and `shed-broker`'s
other leaf deps (`notify`, the `aws-sdk-*` stack) — are **deps of `shed-broker`, reaching only its
embedders** (`shed-app` under its non-default `broker` feature and, transitively, the Tauri
client; and the test-only `shed-broker-harness`). They must **never** reach `shed-core`
or **default-features `shed-app`** (proven by `cargo tree -i saphyr-parser` /
`notify` / `aws-sdk-sts` — the §7 reverse-dep AC mechanically enforces this). **shed-core's own
`yaml_lite` stays hand-rolled**; converging the two readers
onto `saphyr-parser` would be a separate shed-core slice, not assumed here.

## Build / test

```bash
cd crates && cargo test                              # workspace tests (default members)
cargo test -p shed-app --features broker             # the embedded broker bridge
cargo test -p shed-lane --features test-support      # exports lane::conformance
cargo test -p shed-roost --features test-support     # exports testing::FakeRoost
cargo clippy --workspace --exclude shed-broker-harness --all-targets -- -D warnings
cargo test -p shed-broker -p shed-broker-harness --all-targets   # the broker + its wire-harness host
cargo test -p shed-source                            # the extracted identity/credstore/minter
cargo clippy -p shed-broker-harness --all-targets -- -D warnings
cargo clippy -p shed-app --features broker --all-targets -- -D warnings
cargo test -p shed-craze                             # the craze adapter (recipe cells skip)
# the craze recipe against the real hub: build the pinned binaries, then require them
make -C .. craze-binaries                            # prints SHED_CRAZE_BIN_DIR=…
SHED_CRAZE_BIN_DIR=… SHED_CRAZE_REQUIRE=1 cargo test -p shed-craze --all-targets --features test-support
cargo test -p shed-lane --test machine_transport_contract --test craze_ladder --locked
```

Note: `broker` is the one non-default feature left in this workspace. A bare
`cargo test`/`clippy --workspace` does not compile `broker_bridge.rs` at all, so the
explicit `-p shed-app --features broker` legs above are its ONLY coverage (and what CI
runs). `shed-app`'s other non-default feature, `rc`, went with `shed-rc-engine` and the
RC hub in plan 022 (S6, #328); `sx`, which used to enable `rc` workspace-wide by feature
unification, was sunset in plan 016 (S7, #329).

The shared crates also build/test on Linux — `make -C desktop core-linux` runs them in Docker.

## Version lockstep

At **release**, this workspace's `Cargo.toml` version is bumped in **lockstep** with
`desktop/VERSION` (and the Tauri manifests) by `scripts/release/update-version.sh X.Y.Z
--components desktop`; `scripts/release/release-plan.sh` hard-verifies the lockstep before the
desktop leg ships. Don't hand-edit the version out of step.
