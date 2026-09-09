# shed-opencode

The **opencode adapter** for `shed_core::lane` — the agent-lane contract. One
adapter per agent; this is opencode's, speaking its server's local HTTP API over
loopback (the server the TUI already runs, whose URL the roost plugin reports).

```text
OpencodeClient  — the transport (two reqwest clients), the verbs, AgentLane
  ├─ fold.rs    — the pure fold: envelopes → activity + rows + approvals
  ├─ ring.rs    — the bounded ring that OWNS `seq`
  └─ watcher.rs — the reconnecting pump: generations, Reset … Ready
```

## The fold is a port, and it is pinned as one

`OpencodeFold` is a port of the rc hub's fold
(`crates/shed-broker/src/rc_hub/watch_opencode.rs`), which is itself a port of
the Go guest's `internal/ext/rc/watch_opencode.go`. Three things keep the port
honest, and they carry **different claims** — `fixtures/README.md` is the one
place that spells the difference out:

- **`fixtures/opencode_turn.golden.json` — FIDELITY.** A recording of what the
  HUB's fold produces on `crates/fixtures/jsonl/opencode_turn.jsonl` (opencode
  1.17.15), reduced to the projection the two folds share.
  `tests/fold_fixtures.rs` replays the fixture through THIS fold and asserts it
  still equals that. It is **not** regenerable from this crate — regenerating a
  hub recording from the port would assert the port against itself.
- **`fixtures/1.18.29/fold.golden.json` — REGRESSION DETECTION.** What THIS fold
  produces from the committed 155-frame opencode 1.18.29 recording: rows,
  verdicts and the full `LaneApproval` DTOs, so the approval and question paths
  are pinned against real wire rather than only hand-authored inputs. Re-derive
  it offline with `SHED_OPENCODE_REGOLD=1 cargo test -p shed-opencode --test
  fold_fixtures`. Its transcript subset was separately proven byte-identical to
  the hub's fold (plan 015 C3b, throwaway harness, deleted with its dev-dep) —
  that subset is fidelity; its approval DTOs and its `session.error` row are the
  port's alone.
- **Hand-authored unit inputs** (`src/fold.rs`'s test module) for the rules no
  recording exercises deterministically — `note_gap`, `reset`, `seed_approvals`,
  a re-ask reopening a resolved entry.

The helpers in `src/helpers.rs` are **copied** from `shed-broker::rc_hub`, not
linked: `rc_hub::watch` imports `shed_rc_engine::tmux::Tmux`, so linking would
drag the RC engine and the hub into a crate that only speaks HTTP. The
duplication is deliberate and temporary — the hub's copy goes away with the rest
of its opencode watcher.

Three differences from the hub are deliberate (plan 015 §3.2), and documented at
each site in `src/fold.rs`:

1. the hub's synthesized `shed.approval.seed` envelope is the method
   `seed_approvals(permissions, questions)`;
2. questions materialize as `LaneApproval { kind: Question }` carrying
   opencode's `QuestionInfo`, where the hub could only count them;
3. `session.error` — ignored by the hub — emits a display-only `status` row.

The hub's **correlation** machinery (`not_before`, `created_late_enough`,
`dir_match_canon`, `ClaimHolder`, `rest_find_candidate`, `root_pin_from_created`)
is deliberately absent. It existed because the hub had to FIND its session in a
shared store; in the roost model the session id arrives by construction, so the
lane never searches — it scopes.

## Two clients, and why the stream one has no timeout

`OpencodeClient` holds two `reqwest::Client`s, both `.no_proxy()`: a `rest` one
with a 5 s **total** timeout (the hub's `restTimeout`/`ocVerbTimeout`), and a
`stream` one with a 3 s **connect** timeout and **no request timeout at all**. A
request timeout on the streaming client would silently cut the long-lived
`GET /event` — the transcript would reset every N seconds forever and nothing
would look broken. Liveness on that stream is the watcher's 30 s stall window
instead, and `tests/watcher.rs` holds a stream open past 5 s on comment pings
alone to prove the timeout is not there.

"No request timeout" is about the **body**. Two waits on that client are not the
body and are bounded explicitly (`STREAM_HEAD_TIMEOUT`, the hub's
`headerTimeout`): the response **head**, and the **error body** of a non-2xx
`/event`. A peer that completes the TCP handshake and then never sends a status
line satisfies `connect_timeout` and would otherwise park the watcher forever —
after its `Reset`, before there is any body for the stall timer to watch — so no
stall, no retry, no `Down`. `tests/watcher.rs` drives both wedges against a fake
that accepts and goes quiet.

Every REST body is read under `MAX_REST_BYTES` (8 MiB, the hub's
`maxRESTBytes`), enforced while the body is consumed: a declared `Content-Length`
past the cap is refused before a body byte is read, and a close-delimited one is
refused the moment the accumulated total would cross it. `.text()` collects the
whole response before anything can look at it, which the 5 s timeout does not
bound over loopback. The SSE body is exempt — it streams.

## Directory routing

opencode resolves an *instance* from `?directory=` (its
`WorkspaceRoutingMiddleware`), falling back to the server process's cwd. Four
routes are instance-scoped and MUST carry the subscribed session's directory:
`GET /event`, `GET /session/status`, `GET /permission`, `GET /question`. `POST
/session` (create) carries the new session's cwd. Every id-addressed route sends
**no** `directory` — the id is the address. Two directories on one server, and
two sessions in one directory, each have a test.

## The generation bracket

Every `/event` connection is a **generation**, and the order inside one is
pinned (`src/watcher.rs` carries the full version):

1. `Reset { reason, generation }` + `fold.reset()`;
2. open `GET /event?directory=` **first**, wait for `server.connected`, buffer
   later frames into a bounded inbox (1024 items / 4 MiB);
3. run the REST seed **while the stream keeps reading**;
4. drain the buffered frames (a live status seen during the seed therefore wins
   over the REST fallback) — folded, but their session/approval effects are
   **deferred** to step 5, because emitting them here would put an `Approval`
   ahead of the first `Session` and emission is deduplicated, so step 5 could
   not repair it;
5. `Session`, then the approvals, then `Ready { generation }`.

Seeding before subscribing is the bug that order exists to prevent, and
`tests/watcher.rs` injects an event *during* the seed and asserts it is neither
lost nor duplicated.

## The approval scope is the root's descendants, transitively

A sub-agent can spawn a sub-agent, and the live `session.created` handler already
grows the scope that way ("a `session.created` whose `parentID` is in the set").
The seed therefore walks `/session/{id}/children` **breadth-first**, not one
level: a reseed that restored only the immediate children would silently drop a
grandchild's pending approvals — and every later approval frame for it, since the
scope is also the live filter. The walk is bounded (`MAX_DESCENDANT_DEPTH` = 8,
`MAX_DESCENDANT_SESSIONS` = 256, the latter also capping the request count) and
cycle-safe via its visited set, because `parentID` is untrusted input. A failed
`/children` read degrades to "nothing below that node" rather than failing the
seed.

## `testing::FakeOpencode`

Behind the non-default **`test-support`** feature — the
`shed_core::roost::testing::FakeRoost` pattern — so integration tests reach the
double through the feature and never through `cfg(test)`. Its **pin guard**
takes its grammar from **`tests/rc-parity/fake_opencode.py`** (that fake's
`_SCOPED_RE`, itself the Go double's `ocScopedMutationRe`), extended with the two
request-addressed answer routes this crate uses: a session-scoped mutation must
name the pinned session, and a `/permission/{id}` or `/question/{id}` answer must
name a request the fake issued for that session or one of its descendants.
Anything else is a recorded violation and answers 500. The suite records **zero**
violations; the one test that deliberately trips the guard drives it with raw
HTTP rather than through the lane, so the lane's own record stays clean.

There are now three fakes of one server — this one, `tests/rc-parity/`'s and
`desktop/tools/shedtest/`'s — and the never-cross-wire rule applies: they share a
grammar, not code.

## Build / test

```bash
cd crates
cargo test -p shed-opencode                    # the fake comes in via a self dev-dep
cargo test -p shed-opencode --features test-support
cargo clippy -p shed-opencode --all-targets -- -D warnings
cargo run --example shed-opencode-lane -- http://127.0.0.1:4096 ses_… watch
```

The **live smoke** (`tests/live.rs`) talks to a real `opencode serve`. It is
gated by `SHED_OPENCODE_LIVE=1`, skips with a message otherwise, and is never run
in CI:

```bash
SHED_OPENCODE_LIVE=1 cargo test -p shed-opencode --test live -- --nocapture
SHED_OPENCODE_LIVE=1 SHED_OPENCODE_RECORD=1 \
  cargo test -p shed-opencode --test live -- --nocapture   # → fixtures/1.18.29/
```

Re-recording the wire and re-deriving the goldens are **two independent steps**:
the command above needs a live opencode, network and money; re-deriving is
offline, free and deterministic:

```bash
SHED_OPENCODE_REGOLD=1 cargo test -p shed-opencode --test fold_fixtures
```

An opencode version bump is: re-record → re-derive → **review the golden diff as
the substantive change**. `fixtures/README.md` has the full recipe.
