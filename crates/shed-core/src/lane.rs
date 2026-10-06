//! The **agent lane contract** — one normalized surface over "a coding agent
//! with sessions, a transcript, and approvals", at two levels: a machine-level
//! **source** that lists sessions, offers what can be created and creates, and a
//! session-scoped **lane** that streams one session's transcript and takes its
//! verbs. One adapter per agent implements both.
//!
//! One adapter today: opencode over its local HTTP server (`shed-opencode`).
//! craze's, over its per-machine hub (`shed-craze`), arrives in plan 025 C7/C8 —
//! it is what this split was shaped for, so where this doc says what "craze"
//! does it describes that adapter as plan 025 designs it from craze's published
//! protocol, not code that exists yet. gx's `/v1` lane
//! held the second slot from plan 017 until plan 025 retired it (shed#390); the
//! corrections it forced are recorded below as history, because the readers of
//! this module — the next adapter, and shed-mobile's hand-written mirror — read
//! that list as the contract. Every client (the desktop app, mobile) renders the
//! SAME DTOs whichever agent is behind them. The contract lives HERE, in
//! shed-core, and not in `shed-app`, because shed-mobile links shed-core's DTOs
//! through flutter_rust_bridge: the traits have to live where the types they
//! speak in live.
//!
//! **No I/O in this module.** These are pure DTOs, two async traits and the one
//! frame channel every adapter publishes through; the transport, the fold, the
//! ring and the reconnect loop belong to whatever crate implements the traits.
//! [`conformance`] (behind `test-support`) is the shared kit that checks an
//! adapter's streams against the rules below.
//!
//! # The FRB-mirror rule (load-bearing)
//!
//! shed-mobile **hand-mirrors** every DTO here into Dart (the way
//! `dto_rc.rs`/`rc_feed.dart` already mirror [`crate::rc`]'s feed types). So
//! every field is an owned `String`/`Option`/`Vec`/scalar: **no
//! `serde_json::Value`, no `HashMap`, no borrowed lifetimes**. A fielded enum
//! becomes a Dart sealed class; a plain enum becomes a plain Dart enum. Free-form
//! payloads travel as a `String` holding raw JSON ([`LaneApproval::request_json`],
//! [`LaneAnswer::Raw`]) rather than as a `Value` — a `Value` is not mirrorable
//! and would silently strand mobile.
//!
//! # Two levels: [`AgentSource`] and [`AgentLane`] (plan 025, D3)
//!
//! An [`AgentSource`] is a machine's (or an agent server's) view of its
//! sessions: [`AgentSource::subscribe`] streams the live session list,
//! [`AgentSource::create_options`] says what a create can start,
//! [`AgentSource::create`] starts one, and [`AgentSource::open`] hands back the
//! session-scoped [`AgentLane`] for one row's id. A lane is bound to that id for
//! its whole life, so none of its verbs takes one.
//!
//! Three things left the lane trait when it split, each for a reason:
//!
//! - **`sessions()` and `create()`** are the source's now. Listing and creating
//!   are machine-level acts — craze's hub does both, for every provider, before
//!   any session exists — and nothing in production called them on a lane.
//! - **`capabilities()` left the trait entirely.** Capabilities are per SESSION
//!   and can change with an incarnation (a craze session re-hosted, a model that
//!   gains a setting), so they are stream state like activity:
//!   [`LaneEvent::Capabilities`]. A synchronous getter invites exactly the
//!   caching both clients did when the trait called them static — and a "best
//!   known" getter is stale by construction before the first attach.
//! - Nothing else moved. **`history` stays** (for craze it is
//!   `session.snapshot`, and a future page view wants it).
//!
//! [`AgentLane::settings`], [`AgentLane::set`] and [`AgentLane::stop`] have **no
//! default bodies**: every adapter states its answer. One that cannot do the
//! thing answers [`LaneError::Failed`] (`"… is not supported by <agent>"`) and
//! keeps the affordance off through its capabilities; a default answering
//! [`LaneError::NotAccepting`] was rejected because "not now" is the wrong
//! posture for "never".
//!
//! # Semantics pinned for every adapter
//!
//! A **reseed** is bracketed by **[`LaneEvent::Reset`] … [`LaneEvent::Ready`]**:
//! between them the adapter replays the full seed (messages, then the session
//! row and its [`LaneEvent::Capabilities`] — and its [`LaneEvent::Settings`] when
//! those capabilities say `settings` — then approvals; an adapter that resumes a
//! cursor's tail, craze's, appends those replayed events after them, still
//! before `Ready`); a client **stages** everything it receives after `Reset` and
//! swaps its view atomically on `Ready` (so a reseed never flickers), and
//! discards anything stamped with an older `generation`.
//!
//! Not every reconnect is a reseed. An adapter advertising
//! [`LaneCapabilities::history_cursor`] may resume from its cursor SILENTLY —
//! no `Reset`, same generation, the client's view untouched — within the bound
//! its own correction-1 entry states. What survives a silent resume is the whole
//! point: a client must not assume a reconnect announces itself with a
//! `Reset`. (It announces itself with a [`LaneEvent::Stale`] instead — see
//! below.) `seq` is assigned by the adapter's bounded ring (not the fold) and
//! is monotonic across `Reset`s within one subscription — a client that sees a
//! `seq` lower than one it holds refetches, the rule
//! [`crate::rc::RcFeedMessage`] already carries. [`LaneEvent::Down`] means the
//! transport is gone and the subscription ENDED (the task exits after it); a
//! client renders `Down` as stale-with-a-reason, not as an error dialog, and it
//! is the ONLY frame that ends a lane subscription.
//!
//! [`LaneEvent`], [`SourceEvent`], [`LaneAnswer`] and [`LaneSettingChange`] are
//! tagged on a `kind` key, snake_case
//! (`#[serde(tag = "kind", rename_all = "snake_case")]`); the enum-like strings
//! ([`LaneApprovalKind`], [`LaneApprovalStatus`], [`LaneDecision`], [`SendMode`],
//! [`SourceOffline`], [`LaneProviderState`], [`LanePromptOutcome`]) serialize as
//! bare snake_case strings. [`LaneError`] is the exception whose wire form is
//! **shape-heterogeneous**: its `BadRequest(String)`-style variants are newtypes
//! over a non-map, which serde REFUSES to tag internally, so the error type uses
//! external tagging with `rename_all = "snake_case"` — a unit variant crosses as
//! a BARE STRING (`"unknown_session"`), a payload variant as a ONE-KEY OBJECT
//! (`{"bad_request":"…"}`). shed-mobile hand-mirrors that, so both shapes are
//! contract, not an implementation detail; a Dart decoder that assumes one of the
//! two breaks on the other half of the enum.
//!
//! # Unknown-value tolerance, applied ASYMMETRICALLY
//!
//! **Stream/inbound types tolerate unknown values; command/outbound types reject
//! them.** A frame arriving from an adapter must never fail a client's decode — a
//! newer adapter emitting a status or event kind this client has never heard of
//! must degrade, not vanish. But a command travelling the other way (a decision, a
//! send mode, an answer, a create, a setting change) must be REJECTED when
//! unrecognized: silently coercing an unknown decision would turn a user's
//! "allow" into a no-op, which is worse than an error.
//!
//! That is the rule a future adapter — and a future variant — follows. The side
//! of every type is chosen by DIRECTION: events and results inbound tolerant,
//! requests and changes strict. Today it makes these tolerant:
//! [`LaneApprovalKind`], [`LaneApprovalStatus`], [`SourceOffline`],
//! [`LaneProviderState`] and [`LanePromptOutcome`], each preserving an
//! unrecognized wire string in an `Other(String)` (hand-written serde, because
//! derived serde would encode `Other("x")` as an object); [`LaneEvent`] and
//! [`SourceEvent`], whose `Unknown` absorbs a `kind` this build has never heard
//! of; and the result DTOs ([`LaneCreateOptions`], [`LaneCreated`],
//! [`LaneSettings`] and their parts), which ignore an unknown field and default
//! an absent list. It leaves [`LaneDecision`], [`SendMode`], [`LaneAnswer`],
//! [`LaneError`], [`LaneCreateRequest`] and [`LaneSettingChange`] strict — the
//! last two `deny_unknown_fields`.
//!
//! **Tolerance is about VALUES, not types.** A known field whose value has the
//! wrong JSON TYPE (an object where a string belongs, a number past its width)
//! is a protocol error at this tier, as it has always been for every field
//! here — because no adapter DESERIALISES these DTOs from its peer: each one
//! CONSTRUCTS them in Rust from the peer's own wire, so tolerating a peer's
//! malformed payload is the job of that adapter's wire layer (opencode's REST
//! and SSE readers; craze's, plan 025 C7), which knows what the peer meant. The
//! three plan-025 tolerant enums ([`SourceOffline`], [`LaneProviderState`],
//! [`LanePromptOutcome`]) go one step further only because it costs nothing:
//! ANY JSON value decodes there, a non-string as `Other(<its compact JSON>)`, so
//! one odd cause or state never takes its whole frame with it.
//!
//! The tolerant half matters most where it is least visible. This module already
//! argues, on [`LaneQuestion`]'s optional booleans, that a strict FIELD fails the
//! WHOLE enclosing [`LaneEvent`] — so an approval would vanish from the panel
//! rather than render conservatively. A strict enum-like VALUE nested in that same
//! approval does exactly the same damage: one `"status":"cancelled"` from a newer
//! producer and every approval frame it appears in is dropped by an older client,
//! with the session still rendering as blocked-on-you and no button to unblock it.
//! Tolerance is not politeness here; it is the difference between a stale row and
//! a stuck one.
//!
//! Because the version skew is real and one-directional (shed-mobile pins
//! shed-core by git rev and therefore LAGS), a tolerant decode is also the only
//! thing that lets a producer add a status or a frame kind without a coordinated
//! release of every client.
//!
//! # Sources
//!
//! A source subscription brackets every (re)seed exactly as a lane does:
//! [`SourceEvent::Reset`] … [`SourceEvent::Session`]* …
//! [`SourceEvent::Capabilities`] … [`SourceEvent::Ready`], and a client stages and
//! swaps exactly as it does for a lane. Between seeds it receives
//! [`SourceEvent::Session`] (an upsert by `session.id`) and
//! [`SourceEvent::Removed`]. A **roster reseed** is a fresh `Reset … Ready` of
//! the whole list, and it is how a client learns of every row that left during
//! an outage — a reseed produces no `Removed` for them, so a client that keys
//! anything off a row (an open lane, a fold) re-checks it at every swap.
//!
//! **A source's seed must FIT the channel.** A seed is published in one burst,
//! so one bigger than [`LANE_CHANNEL_CAPACITY`] lags at the same frame on every
//! attempt and never reaches its `Ready` — correction 13's reseed would loop
//! forever. So a seed carries at most [`MAX_SOURCE_ROWS`] rows (512, craze's own
//! roster cap — with the `Reset`, `Capabilities` and `Ready` around them, half
//! the channel), and a source that drops rows to stay there says so with
//! `Ready { truncated: true }`.
//!
//! [`SourceEvent::Offline`] is **non-terminal**: the source keeps trying, the
//! client keeps the last `Ready` view and marks it stale with the `cause`
//! ([`SourceOffline`]; the UI decides what a cause means — `NotInstalled` is
//! quiet, `TooOld` says "update craze on this machine"), and the next
//! `Reset … Ready` restores it. It is named `Offline`, not `Down`, because
//! [`LaneEvent::Down`] means "this subscription ENDED" and a source's never does:
//! a source subscription ends only when its subscriber drops or stops it.
//!
//! # `Stale` is not `ended`
//!
//! A transport loss that the adapter will retry **without** a reseed emits
//! [`LaneEvent::Stale`]: the client keeps its rows and shows them stale. A silent
//! resume ends with a lone [`LaneEvent::Ready`] of the SAME generation, which
//! clears the stale mark; a refused resume reseeds with `Reset … Ready`; neither
//! is an end. `Down` keeps its meaning — ended, and never dropped (correction
//! 13). opencode never emits `Stale`: it reseeds every reconnect.
//!
//! So a client's view carries two different facts, and must not merge them: a
//! **stale** mark (set by `Stale` or `Down`, cleared by a `Ready`) for the banner,
//! and **ended** (set ONLY by `Down`) for the lifecycle. A client reopens a lane
//! only when it ENDED. Reopening on any stale mark — what both clients did before
//! plan 025 — throws away the cursor a silent resume exists to keep. A lane that
//! ended with `unknown_session`, `session_closed` or `start_failed:<cause>` is
//! never reopened at all: the session is gone, closed, or never started, and no
//! amount of reconnecting changes that.
//!
//! # Capabilities and settings ride the stream
//!
//! **Capabilities are per session.** [`LaneEvent::Capabilities`] rides every
//! seed (before its `Ready`) and is re-emitted whenever they change;
//! [`LaneEvent::Settings`] rides a seed only when those capabilities say
//! `settings` (so opencode never sends one), and is re-emitted on change. A
//! client holds the latest of each in its view — staged with the rest of a seed
//! and swapped in with it — and reads them from there rather than caching them at
//! open. [`SourceCapabilities`] is the source-level half ([`AgentSource::create`]
//! and [`AgentSource::create_options`] are properties of the machine, not of a
//! session), and rides every source seed the same way.
//!
//! # Generations are matched, not assumed
//!
//! A `Ready` swaps a staged seed **only when its generation equals the staged
//! `Reset`'s**, and a lone `Ready` clears a stale mark **only when its generation
//! equals the live one**; any other `Ready` is ignored. Without that, a late
//! `Ready(g1)` could commit an incomplete `Reset(g2)` seed, or clear the stale
//! mark of a newer outage. A **silent resume never changes the generation**; any
//! generation change needs the full `Reset … Ready` bracket. The same rule
//! governs a source's fold.
//!
//! # Before the first seed
//!
//! A lane may emit `Stale` (or end with `Down`) before any `Ready`, and a source
//! may emit `Offline` before any `Reset`. The view is then empty, and the UI shows
//! the cause — there are no rows to mark stale.
//!
//! # Row identity across a hub restart
//!
//! A craze row's id is its host's `hostId` (plan 025 P11), and a host's id
//! survives a hub restart — the hub polls hosts, it does not own them. So a new
//! hub epoch reseeds the roster with the SAME row ids, and a lane open on one of
//! them is untouched. Only a new host (a session re-hosted by `craze -c`) brings a
//! new id. opencode's row id is its own session id and never changes.
//!
//! # The opencode ↔ craze mapping
//!
//! What each contract verb costs on each side, and the reason the contract is
//! shaped the way it is (two levels, session-scoped lanes, per-session
//! capabilities, cursor-optional, approvals as first-class rows). The opencode
//! column is the adapter as built; the craze column is the adapter plan 025
//! C7/C8 builds — no craze adapter exists before then — written from craze's
//! published protocol reference (`docs/reference/protocol.md` at the pinned
//! craze sha, "PM"), not from a guess.
//!
//! | contract | opencode | craze |
//! |---|---|---|
//! | source `subscribe` | `GET /session` (every root on that server, the global store) plus one `GET /session/status?directory=` per distinct directory, polled every 5 s and diffed into `Session`/`Removed`; activity `busy\|retry → Working`, `idle → Idle`; a failed poll is `Offline{unreachable}` alone, and the next good one reseeds | `sessions.subscribe` on its own roster connection, after a hub `hello` that must offer `rosterSubscribe` and `connect`: the reply's rows seed `Reset … Ready`, each `roster` notification's `upserts`/`removes` become `Session`/`Removed` keyed by `hostId`; a roster `reset` resubscribes, EOF or `hub_closing` is `Offline{unreachable}` and a redial |
//! | source `create_options` | one implicit `opencode` provider, `ready`; no default, no recent directories | `sessions.createOptions`, on its own connection, where the hub's `hello` says `createOptions` |
//! | source `create` | `POST /session?directory=<cwd>`, then `prompt_async` when a prompt is given; `request_id` accepted and unused | `session.create{cwd, provider?, prompt?, requestId}`, on its own connection; an unknown outcome retries once under the same `requestId` |
//! | source `open` | binds the session id; no I/O | binds the `hostId` (and the roster row's craze `sessionId` when the source holds it); no I/O |
//! | `session` | `GET /session/{sessionID}` plus its `/session/status` and approval lists | the roster row the lane was opened with, refreshed by the watcher (attach's info document, the fold's activity); never dials |
//! | `history` | `GET /session/{sessionID}/message` through a fresh fold (seq from 1); `cursor` ignored, `truncated` from the page cap | `session.snapshot` folded into rows; `cursor` ignored (craze has no paging); `truncated` when the snapshot was windowed or dropped, or its omitted ledger is non-empty |
//! | `subscribe` | `GET /event?directory=<session dir>` opened **first**, then the REST seed while live frames buffer; `Reset` … `Ready` brackets every (re)connect; transcript frames filtered to the root id, approval frames to root + descendants; empty-id frames count as liveness | `session.connect{sessionId: <hostId>}` through the hub's splice, the host's `hello`, then `session.attach{sessionId, cursor?, when: "ready"}`: a reply with a `snapshot` seeds `Reset … Ready`; one without is the cursor honoured — a silent resume, `Stale` then a lone `Ready`; every `Ready` waits for the attachment's `synchronized` |
//! | `send(Queue)` | `POST /session/{sessionID}/prompt_async` | `session.prompt{mode: "queue"}` |
//! | `send(Interject)` | [`LaneError::NotAccepting`] | `session.prompt{mode: "interject"}`; refused when idle → `NotAccepting` (correction 8's posture) |
//! | `cancel` | `POST /session/{sessionID}/abort` | `session.cancel` with no `turnId` — the current turn |
//! | `approvals` | fold state seeded from `GET /permission` + `GET /question` (`?directory=` of the session) filtered to the root id **and its descendants** (`GET /session/{sessionID}/children`, refreshed on a `session.created` whose `parentID` is in the set) | the attachment's `snapshot.asks` (`asks.get` for a truncated one), kept current by the fold; automatic questions and plans are never approvals |
//! | `answer(Permission)` | `POST /permission/{requestID}/reply {reply}` (the live route; the deprecated session-scoped route is the recorded fallback) | `asks.answer{askId, answer: {optionId}}`, the option chosen by its `kind` through [`LaneApproval::option_for`] (correction 3) |
//! | `answer(Question)` | `POST /question/{requestID}/reply {answers}`; [`LaneAnswer::Reject`] → `…/reject` | `asks.answer` with `{answers: {<question id>: [<option id>…]}}`; [`LaneAnswer::Reject`] → `{skip: true}`; free text refused (craze questions take none) |
//! | `answer(Choice)` | the offered option id, which for opencode IS one of its three | the offered `optionId` verbatim; on a plan approval, the synthesized `accept`/`reject` |
//! | `settings` | [`LaneSettings::default`] — capabilities say `settings: false` | the session info document's catalogs and the snapshot's settings, re-read on a `meta` event |
//! | `set` | [`LaneError::Failed`] — not supported | `session.set{setting: {kind, id?, value, forModel?}}` |
//! | `stop` | [`LaneError::Failed`] — not supported | `session.stop` where the session capability `stop` is true — a receipt; the stream's `Down{"session_closed"}` is its completion |
//! | errors | 401 → [`LaneError::Unauthorized`]; 404 on a session route → [`LaneError::UnknownSession`], on a permission/question route → [`LaneError::UnknownApproval`]; 409/4xx with an opencode error body → `NotAccepting`/`BadRequest(message)` by body; other non-2xx → [`LaneError::Failed`]; dial failure → [`LaneError::Unavailable`] | the table below |
//!
//! craze publishes its own code → [`LaneError`] table (PM "The code → shed
//! `LaneError` table"): twenty-three codes onto nine variants, keyed on
//! `data.code` only — never `reason`, never `message` — so a caller branches on
//! the variant, never on the text. It is reproduced here verbatim, with plan
//! 025's ONE deviation marked:
//!
//! | craze `data.code` | [`LaneError`] |
//! |---|---|
//! | `bad_request`, `stale_version`, `stale_turn` | [`LaneError::BadRequest`] (craze's message, whole) |
//! | `unknown_session` | [`LaneError::UnknownSession`] |
//! | `unknown_ask` | [`LaneError::UnknownApproval`] (craze's "ask" is this contract's "approval") |
//! | `already_submitted` | [`LaneError::AlreadySubmitted`] |
//! | `already_resolved` | [`LaneError::AlreadyResolved`] |
//! | `not_accepting`, `foreign_turn`, `in_progress`, `stale_model` | [`LaneError::NotAccepting`] — **except** a `session.create` refused `not_accepting` with reason `start_failed`, which is [`LaneError::Failed`] carrying `data.cause` (else "the session failed to start"): `NotAccepting` is a unit variant, and the create flow must show the start failure's cause. Only that reason; any other `not_accepting` on create follows the table |
//! | `unavailable` | [`LaneError::Unavailable`] (craze's message) |
//! | `unsupported`, `queue_full`, `text_too_long`, `prompt_in_flight`, `prompt_cancelled`, `unknown_row`, `unknown_command`, `unknown_subagent`, `aborted`, `failed`, `index_write` | [`LaneError::Failed`] (the code's own text) |
//! | *(unused — protocol 1 has no authentication)* | [`LaneError::Unauthorized`] |
//! | a verb in flight when the connection drops, or past its deadline (craze's client-side `resume_lost`/`disconnected`/`no_answer`) | [`LaneError::Failed`] (`"outcome unknown: …"`) — never resent |
//!
//! # The corrections (gx forced, plan 017; gx retired, plan 025)
//!
//! A contract validated against ONE implementation is a design. gx was the
//! second adapter (plan 017), and corrections 1–12 are what it forced; gx itself
//! was retired in plan 025 (shed#390), and the corrections stay because they are
//! the contract, not gx's. (13 came later and is not gx's: it is the CLIENT
//! CHANNEL's bound.) They live in one numbered list because the same readers —
//! the next adapter, and shed-mobile's hand-written mirror — read this list as
//! the contract. Plan 025 changed correction 1's bound to a per-adapter one and
//! nothing else in the list; its own additions are the sections above.
//!
//! 1. **A silent resume is allowed, and [`LaneEvent::Reset`] … [`LaneEvent::Ready`]
//!    is the *reseed* bracket only.** On an adapter advertising
//!    [`LaneCapabilities::history_cursor`], a reconnect the server accepts FROM
//!    THE CURSOR emits no `Reset` at all: the ring, the open streak, the
//!    generation and the client's view all survive. **The bound is the
//!    adapter's own.** gx's was at most 3 attempts within 30 s of the first
//!    loss, because gx's replay was lossy. craze offers the cursor on every
//!    reconnect, unbounded in time, and its host's journal decides whether the
//!    cursor is honoured — that answer is the bound, inside the adapter's own
//!    dial and re-attach caps (plan 025 §3.3.5). Past the bound, on a
//!    server-sent reset, or on a cursor the server will not honor, the adapter
//!    reseeds with the full `Reset` … `Ready` bracket and rebuilds. **Every
//!    reconnect, silent or not, re-fetches what the stream does not replay**
//!    (the approvals, the session row — presence and activity are not
//!    journalled) and emits last-write-wins [`LaneEvent::Approval`]/[`LaneEvent::Session`]
//!    frames — including a [`LaneApprovalStatus::Resolved`] tombstone for an
//!    approval the client is holding as pending that the fetch no longer lists.
//!    `Reset::reason` stays free text (`connect`, `cursor_lost`,
//!    `server_reset:<r>`, `stall`) and nothing switches on it. **Transport repair
//!    is not a `LaneEvent`**: an adapter that reconnects reaches its transport
//!    through something the client supplies — gx called a client hook before
//!    every connect attempt, craze dials a fresh client-supplied duplex per
//!    connection — so a client can re-establish an SSH forward without the
//!    contract growing a frame for it. opencode is unchanged — it advertises
//!    `history_cursor: false` and every reconnect is a reseed.
//! 2. **[`LaneAnswer::Choice`]** — "the option with this id, exactly as offered".
//!    Strict in both directions: serde refuses an unknown `kind`, and an adapter
//!    answers [`LaneError::BadRequest`] for an id the approval did not offer. It
//!    is named `Choice` and not `Option` for the obvious Rust reason (and
//!    [`LaneSettingChange::Config`] is named `Config` for the same one).
//! 3. **[`LaneApprovalOption::kind`]** carries the option's SEMANTIC kind when
//!    the agent states one (`allow_once`, `allow_always`, `reject_once`,
//!    `reject_always` — the ACP vocabulary, snake_case, spelled once in
//!    [`option_kind`]). gx filled it from the request; craze passes the kinds
//!    its permission asks offer, verbatim; opencode fills
//!    `allow_once`/`allow_always`/`reject_once` for its three — and its third
//!    option is the case that proves the split, since its id stays opencode's
//!    own `reject` while its kind is `reject_once`.
//!    **[`LaneAnswer::Permission`] maps by `kind`, never by id**, and
//!    [`LaneApproval::option_for`] IS that mapping — one implementation, so every
//!    adapter and shed-mobile's Dart mirror cannot each re-derive the clauses
//!    differently: [`LaneDecision::AllowOnce`] → the option whose kind is
//!    `allow_once`, [`LaneDecision::AllowAlways`] → `allow_always`,
//!    [`LaneDecision::Reject`] → `reject_once`, falling back (for `Reject`
//!    alone, and only when no `reject_once` is offered at all) to the first
//!    option whose kind starts `reject`; no match is
//!    [`LaneError::BadRequest`], which the ADAPTER raises because only it can
//!    name the agent. **Ids are opaque.** gx's
//!    own fixtures paired `optionId: "allow-once"` with `kind: "allow_once"`, which
//!    is exactly the coincidence that makes id-matching look like it works until
//!    an agent numbers its options.
//!
//!    **The kind must be UNAMBIGUOUS, and a live gx leader is what proved it.**
//!    A real `session/request_permission` offered five options of which TWO
//!    declared `allow_once` — `allow-once` ("Yes, proceed") and
//!    `enable-always-approve` ("Yes, and don't ask again for anything"). Under
//!    the original "first in offered order wins" rule, a human tapping "Allow
//!    once" would have silently turned permission prompts off for the session.
//!    So [`LaneApproval::option_for`] answers `None` whenever MORE THAN ONE
//!    offered option carries the requested kind, rather than picking. An agent
//!    may offer several options of a kind; a three-valued [`LaneDecision`]
//!    cannot say which one the human meant, and guessing is the escalation this
//!    correction exists to stop. It costs a capability-driven client nothing —
//!    the panel already posts [`LaneAnswer::Choice`] with the exact offered id —
//!    and it makes the scripted three-decision form fail loudly instead of
//!    surprisingly.
//! 4. **[`LaneQuestion::id`]** is the key an answer is filed under. gx used the
//!    question's TEXT (what its own TUI keyed by); craze uses each question's
//!    own `id`; opencode has no key and leaves it `None` (positional).
//!    [`LaneAnswer::Question::answers`] stays POSITIONAL — the adapter maps
//!    position → key — so a client never has to know which agent it is talking
//!    to. **Free text is its own positional field**
//!    ([`LaneAnswer::Question::custom_text`]), read against the questions by
//!    [`normalize_question_answer`]: gx turned it into an `"Other"` label plus an
//!    `annotations[key].notes` entry, opencode appends it to that question's
//!    answer list (which is what its own TUI posts), and craze refuses it (its
//!    questions take no free text, so it reports `custom: false`). On opencode
//!    `custom` defaults to TRUE off the wire, which is what opencode's schema
//!    says and what its TUI does with an absent flag.
//! 5. **Credentials are the client's job.** There is no credential type in this
//!    contract and there will not be one: an adapter takes its credentials at
//!    construction, from a source the client supplies; roost carries only WHERE
//!    the agent is, never HOW to be let in. gx needed a bearer token on every
//!    route but its health check, and that token lived on the host that ran gx —
//!    so the adapter's crate defined the source, and the client decided how to
//!    read it. (craze's protocol 1 has no authentication at all; its transport is
//!    the client's ssh or loopback.)
//! 6. **[`crate::rc::RcActivity`] from the agent's own states, with the
//!    approval override.** gx's six mapped `working` → `Working`; `needs_input` →
//!    `NeedsApproval` when the session had a pending approval, else `NeedsInput`;
//!    `idle`, `completed` and `dormant` → `Idle`; `dead` and anything
//!    unrecognized → `Unknown` (craze's mapping lives with its adapter, plan 025
//!    §3.3.3). **A lane-held unanswered approval overrides both `activity` (→
//!    `NeedsApproval`) and [`LaneSession::pending_approvals`]** on every session
//!    row the adapter emits: the adapter's fold knows about an approval the
//!    roster poll does not.
//! 7. **[`LaneSession::approximate`] is per-ROW, not per-producer.** This
//!    correction originally said opencode reports `true` on every row it emits.
//!    That is not what the adapter does, and the flag is better for it:
//!    opencode's ROSTER rows (its source's poll and [`AgentLane::session`],
//!    derived from its `/session/status` poll) report `true`, and its LIVE
//!    WATCHER rows report `false`, because that row's activity is read off the
//!    fold — the event stream itself, not a poll. gx reported `false` from its
//!    roster as well; craze's roster rows carry the hub's own `approximate`. So
//!    the flag describes THE ROW's provenance, which is what it was always for;
//!    a client that expects one producer to answer the same way on every row is
//!    reading it wrong.
//! 8. **Cancel may be refused.** An adapter whose agent rejects a no-op cancel
//!    surfaces [`LaneError::NotAccepting`] rather than swallowing it as `Ok(())`;
//!    clients gate the affordance on [`crate::rc::RcActivity::Working`]. Hiding
//!    the refusal would make a real "the session moved on" indistinguishable
//!    from success.
//! 9. **The feed vocabulary and the sanitizers live in [`feed`]** —
//!    `FEED_ROLE_*`, `FEED_TYPE_*`, [`feed::sanitize_feed_text`],
//!    [`feed::bound_token`], [`feed::truncate_bytes`], the caps and the ANSI
//!    stripper. They were `shed-opencode`'s; a second adapter reaching into the
//!    first one for a sanitizer is a dependency no release schedule survives.
//!    `shed-opencode` re-exports every one of them.
//! 10. **[`ring::MessageRing`] and [`backoff`] moved down here too, WITHOUT
//!     `chrono`.** [`ring::MessageRing::append`] takes `now_unix_ms: i64` and
//!     defaults a missing `ts` through [`crate::roost::rfc3339_z`]; callers that
//!     hold a `DateTime<Utc>` pass `…timestamp_millis()`. The invariant this
//!     protects is mechanical: `cargo tree -p shed-core-ffi` and
//!     `cargo tree -p shed-core --target aarch64-linux-android` must list no crate
//!     they did not list before. `regex` was already a shed-core dependency, which
//!     is the only reason the ANSI stripper could come along. (Plan 025's parse
//!     direction, [`crate::time`], keeps the same invariant: no date crate.)
//! 11. **[`crate::roost::loopback_base_url`]** is roost's own rule, ported
//!     verbatim: `http`, a host of `127.0.0.1`/`localhost`/`[::1]`, an explicit
//!     decimal port in `1..=65535`, and nothing after it. It is what
//!     [`crate::roost::RoostSession::agent_lane`] judges opencode's reported
//!     `server_url` against before stamping a lane, so shed judges an
//!     agent-supplied URL by exactly the rule roost judged it by. (Plan 017's
//!     gx adapter judged its own `gx.remote` metadata the same way, for the
//!     same reason, before plan 025 retired gx and the other direct-agent
//!     kinds from shed entirely — shed#390.)
//! 12. **The mapping table above is written from the adapter as built** (and,
//!     for craze, from its published protocol), and the error table is
//!     explicit rather than "the error table verbatim" — a row-by-row table is a
//!     test, and a prose promise is not.
//!
//! 13. **The frame channel is BOUNDED at [`LANE_CHANNEL_CAPACITY`], and an
//!     overflow is a RESEED.** [`Subscription::rx`] is a bounded
//!     `mpsc::Receiver`; the producing half is a [`Publisher`], which is
//!     deliberately **not `Clone`** — one publisher owns one subscription, which
//!     is what makes [`Publisher::wait_drained`] mean anything. Both levels use
//!     the ONE generic channel ([`LanePublisher`] and [`SourcePublisher`] are
//!     aliases of it), so this is one overflow policy, not two. An adapter
//!     publishes with [`Publisher::publish`], a `try_send`: a full channel
//!     answers [`Publish::Lagged`] and the frame is DROPPED. Every emitting
//!     helper propagates that, so the generation ends at the FIRST dropped
//!     frame — in steady streaming, during the seed, and during a reseed's
//!     own `Reset` … `Ready`, which is why a lagged reseed is abandoned and
//!     retried with a fresh `Reset` rather than left staged with no `Ready`. The
//!     adapter then ends the generation exactly as it ends one on a stream loss
//!     (opencode drops its `/event` stream; gx dropped its SSE stream and
//!     unpinned the epoch), waits for the channel to drain **with no transport
//!     held**, and reseeds as `Reset { reason: "lagged" } … Ready`. Consecutive
//!     lagged generations go through the adapter's ordinary failure backoff, so
//!     a consumer draining one frame at a time cannot turn itself into a
//!     seed-per-frame load on the live agent. A client that honours the bracket
//!     never observes the dropped frames: it holds the last `Ready` view until
//!     the next `Ready` swaps it, and `lagged` is one more free-text
//!     `Reset::reason` that nothing branches on. A lagged generation is never
//!     RESUMED, even on an adapter that can resume: the dropped frames may sit
//!     behind its cursor. The terminal [`LaneEvent::Down`] is the ONE frame
//!     that is never dropped — it goes through [`Publisher::publish_final`],
//!     which awaits, so a client can never hold a stale `Ready` view with no
//!     stale reason.
//!
//!     **`lagged` and `overflow` are different overflows and both names stay.**
//!     `overflow` is an ADAPTER-INTERNAL one — the bounded inbox that buffers
//!     live frames while the REST seed runs — and it says the adapter lost
//!     frames it had not folded yet. `lagged` is the CLIENT channel: the
//!     adapter folded and published them, and the consumer was not reading.
//!     Neither is a substitute for the other, and nothing branches on either.
//!
//!     **What the bound is and is not.** It bounds the COUNT of queued frames,
//!     not their bytes. Frame size is bounded separately by each adapter's own
//!     caps ([`feed::truncate_bytes`] on message text, the 4 MiB SSE frame
//!     limit; [`LaneApproval::request_json`] is the agent's raw request and is
//!     not capped here), so the worst case a stalled consumer retains is
//!     [`LANE_CHANNEL_CAPACITY`] × the largest frame its adapter admits — finite,
//!     which is the whole gain, since the count was previously unbounded. A byte
//!     budget is deliberately not added. Backpressure was rejected because the
//!     publishing task is the one reading the agent's stream, and a
//!     `send().await` mid-generation would stall that read and let the agent's
//!     server close or buffer it — a less controlled reconnect than this one.
//!     Drop-oldest was rejected because the contract promises order and the
//!     exact set within a generation, and a client cannot tell that rows went
//!     missing.
//!
//! One thing these corrections deliberately did NOT do: add
//! `LaneDecision::RejectAlways`, put a `LaneCredentials` type in the contract, or
//! grow an in-place row-update event. Rows stay append-only; an adapter that
//! wants to revise a row emits a new one.

use std::sync::Arc;

use serde::{Deserialize, Serialize};
use tokio::sync::mpsc;

use crate::rc::{RcActivity, RcFeedMessage};

pub mod backoff;
#[cfg(any(test, feature = "test-support"))]
pub mod conformance;
pub mod feed;
pub mod ring;

// ---- sessions ----

/// One agent session as the contract sees it: an id, where it lives, what it is
/// doing, and how much is waiting on the human.
///
/// `activity` reuses [`RcActivity`] rather than minting a parallel vocabulary —
/// every client already renders that badge, and a lane row has to sort into the
/// same sessions view as an RC row.
///
/// The fields after `last_change_unix_ms` are plan 025's: what a craze roster row
/// says about a session beyond its activity (§3.3.3's row mapping). Every one is
/// optional and OMITTED when absent, so a row from an adapter that knows none of
/// them (opencode's) is byte-identical to what this type emitted before they
/// existed, and a lagging mirror decodes it unchanged. They are inbound and
/// tolerant like the rest of the row: absent decodes to `None`.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct LaneSession {
    /// The adapter-scoped session id — the address [`AgentSource::open`] takes,
    /// and what [`AgentLane::session_id`] answers. A craze row's id is its host's
    /// `hostId` (module doc, "Row identity across a hub restart").
    pub id: String,
    pub title: String,
    /// The session's working directory. Also the routing key on adapters that
    /// scope their endpoints by directory (opencode's `?directory=`).
    pub cwd: String,
    /// The session's WORK dimension, and only that — it does NOT subsume
    /// `pending_approvals`. An adapter that derives activity from a cheap status
    /// poll cannot see an approval from there, which is why the pinned opencode
    /// mapping (`busy|retry → Working`, `idle → Idle`) never emits
    /// [`RcActivity::NeedsApproval`] and reports a session sitting on a
    /// permission prompt as `Idle`. Read `activity` for "is it running",
    /// `pending_approvals` for "is it blocked on me"; a client that keys its
    /// blocked badge off `activity` alone will miss every opencode approval.
    pub activity: RcActivity,
    /// How many approvals are open. The authoritative answer to "is this session
    /// blocked on me" — an approval ROW can be evicted from a bounded ring, this
    /// count cannot.
    pub pending_approvals: u32,
    /// `true` when the adapter derived `activity`/`pending_approvals` from a
    /// cheap or stale source (a roster poll rather than a live fold), so a client
    /// can render the row without claiming precision it does not have.
    ///
    /// **It is per-ROW, not per-producer** (module doc, correction 7). opencode
    /// reports `true` on the rows it derives from its `/session/status` poll —
    /// its source's [`SourceEvent::Session`] rows and [`AgentLane::session`] —
    /// and `false` on the [`LaneEvent::Session`] rows its watcher emits, whose
    /// activity comes off the live fold rather than a poll. So a client treats
    /// a `false` as "trust this one more" and reads the flag on each row it
    /// gets; one that expects a given adapter to answer the same way everywhere
    /// is reading it wrong.
    pub approximate: bool,
    /// Set on a child session; `None` on a root. Descendants exist because an
    /// agent can spawn sub-sessions whose approvals still block the parent.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub parent_id: Option<String>,
    /// Unix epoch milliseconds of the last change the adapter observed, when it
    /// has one. Never parsed into a datetime by this type: an adapter whose
    /// agent stamps RFC 3339 converts at its edge ([`crate::time`]).
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub last_change_unix_ms: Option<i64>,
    /// The agent provider driving the session (craze's `host.provider`:
    /// `cursor`, `grok`, `gx`, `native`). `None` where the adapter IS the
    /// provider (opencode).
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub provider: Option<String>,
    /// The model the session runs, as the agent names it.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub model: Option<String>,
    /// What the session is doing right now, one line, as the agent summarized
    /// it (craze's row fact `doing`).
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub doing: Option<String>,
    /// The oldest open approval's one-line summary — what the session is
    /// blocked on, for a row that has no room for the approval itself.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub head_ask_summary: Option<String>,
    /// The head of the agent's last reply, one line.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub last_reply: Option<String>,
    /// Unix epoch milliseconds since the session has been in its current
    /// state, parsed from the agent's RFC 3339 by [`crate::time::rfc3339_unix_ms`].
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub since_unix_ms: Option<i64>,
    /// How many clients are attached to the session right now (craze's
    /// `presence`).
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub attached: Option<u32>,
    /// Why the session failed to start, when it did — the start failure's
    /// first error line, shown as it is.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub start_error: Option<String>,
    /// The PROVIDER's own session id behind this row, when it has one — the key
    /// a roost tab running the same session carries, and so the key a client
    /// folds that tab into this row by (plan 025 D4). Never an address any verb
    /// takes.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub provider_session_id: Option<String>,
    /// The agent's permission posture (craze's `"bypass"` | `"prompt"`), for the
    /// transcript header. An open string: a newer agent's posture renders as
    /// its own word.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub permission_mode: Option<String>,
    /// The roost tab this row was merged with, set ONLY by the client-side merge
    /// (plan 025 §3.6.3) — **never by a source or a lane**. It is on the
    /// contract's row so the merged row a client renders and the one it hands
    /// over a bridge are the same type; an adapter that set it would be
    /// claiming a tab it cannot see, and [`conformance`] refuses one.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub tab_id: Option<i64>,
}

impl Default for LaneSession {
    /// A row with nothing known: empty strings, no approvals, every optional
    /// field absent, and [`RcActivity::Unknown`] — the neutral render. A
    /// convenience for building a row field by field (`..LaneSession::default()`),
    /// never a row an adapter should emit as it stands: its empty `id` addresses
    /// nothing.
    fn default() -> LaneSession {
        LaneSession {
            id: String::new(),
            title: String::new(),
            cwd: String::new(),
            activity: RcActivity::Unknown,
            pending_approvals: 0,
            approximate: false,
            parent_id: None,
            last_change_unix_ms: None,
            provider: None,
            model: None,
            doing: None,
            head_ask_summary: None,
            last_reply: None,
            since_unix_ms: None,
            attached: None,
            start_error: None,
            provider_session_id: None,
            permission_mode: None,
            tab_id: None,
        }
    }
}

/// A page of a session's transcript.
///
/// Messages reuse [`RcFeedMessage`] verbatim — the same rows the RC feed already
/// renders, so a lane transcript and an RC transcript are one widget. `truncated`
/// carries the same meaning it does on `RcMessagesPage`: the client is NOT
/// holding a complete history and must refetch from the earliest retained row
/// rather than splice.
#[derive(Debug, Clone, PartialEq, Eq, Default, Serialize, Deserialize)]
pub struct LaneHistory {
    #[serde(default)]
    pub messages: Vec<RcFeedMessage>,
    pub truncated: bool,
    /// An opaque paging cursor, when an adapter has one to offer. **Advisory**:
    /// both adapters answer `None` and ignore the one handed to
    /// [`AgentLane::history`] (opencode refolds from the top; craze's
    /// `session.snapshot` has no paging), and nothing about it is promised by
    /// [`LaneCapabilities::history_cursor`], which since plan 025 speaks only
    /// for the STREAM.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub cursor: Option<String>,
}

// ---- approvals ----

/// What KIND of thing is blocking on the human.
///
/// [`LaneApprovalKind::Other`] implements the same **unknown-value policy**
/// [`crate::rc::RcKind`] does: an unrecognized wire value is PRESERVED verbatim
/// rather than coerced or dropped, so an approval minted by a newer agent
/// renders neutrally (its raw kind shown, no kind-specific affordance) instead of
/// disappearing. Because of the owned string this enum is not `Copy`.
///
/// **Round-trip identity holds for every value the wire can produce, and not for
/// one it cannot.** A hand-constructed `Other("permission")` serializes to
/// `"permission"` and decodes back as [`LaneApprovalKind::Permission`], so
/// `encode → decode == self` is false for it. That is benign and deliberate:
/// [`LaneApprovalKind::from_wire`] — the ONLY thing that mints an `Other` from
/// input — never puts a known string in it, so no decoded value is ever in that
/// state. It is noted here so a later reader does not "fix" the non-bug by
/// escaping or namespacing the raw string, which WOULD break the wire. Same
/// pre-existing shape as [`crate::rc::RcKind`].
#[derive(Debug, Clone, PartialEq, Eq, Hash)]
pub enum LaneApprovalKind {
    /// "May I run this tool / touch this file?" — answered with a
    /// [`LaneDecision`].
    Permission,
    /// A structured question with options — answered with
    /// [`LaneAnswer::Question`].
    Question,
    /// "Do you accept this plan?"
    PlanApproval,
    /// An MCP server asking the user for input through the agent.
    McpElicitation,
    /// An unrecognized kind, its raw wire string preserved.
    Other(String),
}

impl LaneApprovalKind {
    pub fn as_str(&self) -> &str {
        match self {
            LaneApprovalKind::Permission => "permission",
            LaneApprovalKind::Question => "question",
            LaneApprovalKind::PlanApproval => "plan_approval",
            LaneApprovalKind::McpElicitation => "mcp_elicitation",
            LaneApprovalKind::Other(s) => s,
        }
    }

    /// Parse a wire kind, preserving an unrecognized value as
    /// [`LaneApprovalKind::Other`] rather than failing or defaulting.
    pub fn from_wire(s: &str) -> LaneApprovalKind {
        match s {
            "permission" => LaneApprovalKind::Permission,
            "question" => LaneApprovalKind::Question,
            "plan_approval" => LaneApprovalKind::PlanApproval,
            "mcp_elicitation" => LaneApprovalKind::McpElicitation,
            other => LaneApprovalKind::Other(other.to_string()),
        }
    }

    /// A recognized kind. A `false` here is the neutral-render signal.
    pub fn is_known(&self) -> bool {
        !matches!(self, LaneApprovalKind::Other(_))
    }
}

impl Serialize for LaneApprovalKind {
    fn serialize<S: serde::Serializer>(&self, s: S) -> Result<S::Ok, S::Error> {
        s.serialize_str(self.as_str())
    }
}

impl<'de> Deserialize<'de> for LaneApprovalKind {
    fn deserialize<D: serde::Deserializer<'de>>(d: D) -> Result<Self, D::Error> {
        Ok(LaneApprovalKind::from_wire(&String::deserialize(d)?))
    }
}

/// Where an approval is in its life.
///
/// [`LaneApprovalStatus::Submitted`] is the optimistic middle: the client sent an
/// answer and the adapter has not yet seen the agent acknowledge it. It exists so
/// a double-tap is refusable ([`LaneError::AlreadySubmitted`]) without waiting a
/// round trip.
///
/// **Tolerant**, under the module doc's asymmetric unknown-value rule, and it is
/// the sharpest case of it: this is a STREAM value, riding inbound inside a
/// [`LaneApproval`] and therefore inside a [`LaneEvent::Approval`] frame. A strict
/// enum here means one `"status":"cancelled"` from a producer that later grows a
/// cancelled/expired state fails the decode of the WHOLE enclosing event — the
/// approval does not render conservatively, it VANISHES, exactly the failure
/// [`LaneQuestion`]'s optional booleans are defaulted to avoid. So an unrecognized
/// status is preserved verbatim as [`LaneApprovalStatus::Other`], the
/// [`LaneApprovalKind`] pattern. Because of the owned string this enum is not
/// `Copy`.
///
/// **Ask [`LaneApprovalStatus::is_pending`], never `!= Resolved`.** `Other` is
/// deliberately NOT pending: an unknown status is at least as likely to be
/// terminal (`cancelled`, `expired`) as live, and offering answer buttons for it
/// would post an answer the agent has stopped listening for. The conservative
/// render for an `Other` is the row, its raw status, and no affordance.
#[derive(Debug, Clone, PartialEq, Eq, Hash)]
pub enum LaneApprovalStatus {
    Pending,
    Submitted,
    Resolved,
    /// An unrecognized status, its raw wire string preserved.
    Other(String),
}

impl LaneApprovalStatus {
    pub fn as_str(&self) -> &str {
        match self {
            LaneApprovalStatus::Pending => "pending",
            LaneApprovalStatus::Submitted => "submitted",
            LaneApprovalStatus::Resolved => "resolved",
            LaneApprovalStatus::Other(s) => s,
        }
    }

    /// Parse a wire status, preserving an unrecognized value as
    /// [`LaneApprovalStatus::Other`] rather than failing or defaulting. Never
    /// mints an `Other` holding a known string, so the round-trip caveat on
    /// [`LaneApprovalKind`] applies here identically and is equally unreachable.
    pub fn from_wire(s: &str) -> LaneApprovalStatus {
        match s {
            "pending" => LaneApprovalStatus::Pending,
            "submitted" => LaneApprovalStatus::Submitted,
            "resolved" => LaneApprovalStatus::Resolved,
            other => LaneApprovalStatus::Other(other.to_string()),
        }
    }

    /// A recognized status. A `false` here is the neutral-render signal.
    pub fn is_known(&self) -> bool {
        !matches!(self, LaneApprovalStatus::Other(_))
    }

    /// Whether this approval is still waiting on the human — the ONLY predicate a
    /// client should gate its answer affordance on. See the type doc for why
    /// [`LaneApprovalStatus::Other`] answers `false`.
    pub fn is_pending(&self) -> bool {
        matches!(self, LaneApprovalStatus::Pending)
    }
}

impl Serialize for LaneApprovalStatus {
    fn serialize<S: serde::Serializer>(&self, s: S) -> Result<S::Ok, S::Error> {
        s.serialize_str(self.as_str())
    }
}

impl<'de> Deserialize<'de> for LaneApprovalStatus {
    fn deserialize<D: serde::Deserializer<'de>>(d: D) -> Result<Self, D::Error> {
        Ok(LaneApprovalStatus::from_wire(&String::deserialize(d)?))
    }
}

/// One selectable answer.
///
/// `id` is what goes back to the agent. Some agents' options have no id of their
/// own (opencode's `QuestionOption` is `label` + `description`), in which case
/// the adapter sets `id = label` — the contract keeps an id so a client never has
/// to send display text back as an identifier by accident.
///
/// **`id` is OPAQUE and `kind` is the semantics.** An agent numbers, hashes or
/// names its option ids however it likes; the only safe use of one is to hand it
/// straight back ([`LaneAnswer::Choice`]). Anything that needs to know what an
/// option MEANS — a client styling a destructive button, an adapter translating
/// [`LaneAnswer::Permission`] — reads `kind`. gx's own fixtures (plan 017;
/// gx retired in plan 025) paired `optionId: "allow-once"` with
/// `kind: "allow_once"`, which is exactly the coincidence that makes
/// id-sniffing look correct right up until an agent numbers its options
/// `p-1…p-4`.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct LaneApprovalOption {
    /// Opaque. Round-trip it; never parse it.
    pub id: String,
    pub label: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub description: Option<String>,
    /// The option's semantic kind when the agent states one — the ACP
    /// vocabulary in snake_case, spelled once in [`option_kind`]. `None` when
    /// the agent offers no semantics: a question's answer labels have none
    /// ("yes"/"no" is not a permission posture), and an agent that simply does
    /// not state one leaves it absent rather than having one guessed for it.
    /// Both permission adapters DO state one.
    ///
    /// A `String` rather than an enum on purpose. It is a STREAM value, and the
    /// module doc's asymmetric rule says an inbound value this build has never
    /// heard of must degrade rather than fail the decode of the whole enclosing
    /// [`LaneEvent`] — an approval that vanishes because a newer agent grew a
    /// fifth option kind is the failure mode [`LaneApprovalStatus`] documents at
    /// length. Clients match the four known values and render anything else as a
    /// plain button.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub kind: Option<String>,
}

/// The four ACP option kinds an adapter fills [`LaneApprovalOption::kind`] with,
/// as the wire spells them.
///
/// Constants rather than an enum, for the reason on the field itself: the wire
/// value is tolerated open, and these exist so the three producers and the two
/// clients spell the same four strings.
pub mod option_kind {
    /// Allow this one invocation.
    pub const ALLOW_ONCE: &str = "allow_once";
    /// Allow this and every matching future invocation.
    pub const ALLOW_ALWAYS: &str = "allow_always";
    /// Refuse this one invocation.
    pub const REJECT_ONCE: &str = "reject_once";
    /// Refuse this and every matching future invocation.
    pub const REJECT_ALWAYS: &str = "reject_always";
}

/// A structured question inside an approval: a header, the question itself, and
/// how it may be answered.
///
/// `multiple` allows several options in one answer; `custom` allows free text
/// alongside (or instead of) the options — carried back in
/// [`LaneAnswer::Question::custom_text`] and read by
/// [`normalize_question_answer`], which REFUSES text aimed at a question whose
/// `custom` is false rather than dropping it.
#[derive(Debug, Clone, PartialEq, Eq, Default, Serialize, Deserialize)]
pub struct LaneQuestion {
    /// The key this question's answer is filed under, when the agent files
    /// answers by key rather than by position.
    ///
    /// craze sets it to each question's own `id`; gx (plan 017, retired in plan
    /// 025) set it to the question's own TEXT — that is literally what its TUI
    /// keyed the answer map by — so an adapter that guessed an index would post
    /// an answer the agent never reads. opencode files positionally and leaves
    /// this `None`.
    ///
    /// It is here so the KEY is visible to a client that wants to echo it, not
    /// so a client has to use it: [`LaneAnswer::Question::answers`] stays
    /// positional and the adapter does the mapping.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub id: Option<String>,
    pub header: String,
    pub question: String,
    #[serde(default)]
    pub options: Vec<LaneApprovalOption>,
    /// Absent decodes to `false`: opencode marks both this and `custom` optional
    /// on its own wire (`questions[]{…, multiple?, custom?}`), and a producer one
    /// version behind may omit them. Without the `default` a missing key fails the
    /// WHOLE enclosing [`LaneEvent`], so an approval would vanish rather than
    /// render conservatively.
    #[serde(default)]
    pub multiple: bool,
    /// Whether this question accepts free text beside its options.
    ///
    /// **On THIS DTO an absent value is `false`** — the conservative reading for
    /// a producer one version behind, and the one a panel can act on without
    /// inviting typing that will be refused. That is not the same as the
    /// AGENT's default: opencode's own schema documents `custom` as "Allow
    /// typing a custom answer (default: true)" and its TUI treats an absent
    /// flag as on, so the opencode adapter's wire reader defaults it TRUE
    /// before it ever builds one of these. craze sets it `false` — its
    /// questions take no free text. (gx, until plan 025 retired it, set it
    /// `true` unconditionally: its ask always carried a freeform row.)
    #[serde(default)]
    pub custom: bool,
}

/// One thing waiting on the human.
///
/// A permission and a question are the same row shape on purpose: a client
/// renders ONE approval panel, and `kind` picks which affordance it attaches.
/// **[`LaneApproval::kind`] selects which field a client renders — the two are
/// never both populated.** A [`LaneApprovalKind::Permission`] puts the options
/// THE AGENT OFFERED in `options` — however many, in offered order, each with an
/// opaque id and an optional semantic [`LaneApprovalOption::kind`] — and leaves
/// `questions` empty; a [`LaneApprovalKind::Question`] puts the structured form in
/// `questions` — each [`LaneQuestion`] carrying its OWN `options` plus a `custom`
/// flag for free text — and leaves the top-level `options` empty. Rendering the
/// wrong one yields an approval with no buttons, so branch on `kind`, never on
/// which list happens to be non-empty.
///
/// **This type is authoritative over the transcript row's approval block.** The
/// same approval ALSO reaches a client inside
/// [`crate::rc::RcFeedMessage::approval`] on the `approval_request` row — same
/// id, but the narrower [`crate::rc::RcFeedApproval`] shape, whose status is only
/// `pending`/`resolved` and so cannot express [`LaneApprovalStatus::Submitted`].
/// That block is the TRANSCRIPT's inline rendering; take status from — and answer
/// against — this type and [`LaneEvent::Approval`], or an answered approval keeps
/// reading as pending and the [`LaneError::AlreadySubmitted`] refusal looks like
/// a bug.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct LaneApproval {
    /// The adapter-scoped approval id — the address [`AgentLane::answer`] takes.
    pub id: String,
    /// The session this blocks. May be a DESCENDANT of the session the client
    /// subscribed to: a child session's approval still blocks the parent's agent,
    /// so it surfaces on the parent's panel even though the transcript does not.
    pub session_id: String,
    pub kind: LaneApprovalKind,
    pub status: LaneApprovalStatus,
    pub title: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub detail: Option<String>,
    #[serde(default)]
    pub options: Vec<LaneApprovalOption>,
    #[serde(default)]
    pub questions: Vec<LaneQuestion>,
    /// The agent's own request payload, RAW JSON in a string — not a
    /// `serde_json::Value` (the FRB-mirror rule). It is the escape hatch a client
    /// renders when `kind` is unknown, and the body [`LaneAnswer::Raw`] answers
    /// against.
    pub request_json: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub created_at_unix_ms: Option<i64>,
}

impl LaneApproval {
    /// The offered option a [`LaneDecision`] selects — **by
    /// [`LaneApprovalOption::kind`], never by id**.
    ///
    /// This is the one implementation of the module doc's correction 3. It is
    /// here, and not in each adapter, because the rule has a clause subtle
    /// enough that independent readings would not agree: the `Reject` fallback.
    /// Every adapter that answers a [`LaneAnswer::Permission`] resolves it
    /// through this, and shed-mobile mirrors THIS behaviour rather than
    /// re-deriving it from the prose.
    ///
    /// The rule, in order:
    ///
    /// 1. **Exact kind, and it must be UNAMBIGUOUS.**
    ///    [`LaneDecision::AllowOnce`] → [`option_kind::ALLOW_ONCE`],
    ///    [`LaneDecision::AllowAlways`] → [`option_kind::ALLOW_ALWAYS`],
    ///    [`LaneDecision::Reject`] → [`option_kind::REJECT_ONCE`]. If **exactly
    ///    one** offered option carries that kind, it is the answer. If **more
    ///    than one** does, the answer is `None` — see "Why ambiguity refuses"
    ///    below.
    /// 2. **The `Reject` fallback, and only `Reject`**, and only when NO option
    ///    carries `reject_once` at all: the FIRST option — first in OFFERED
    ///    order — whose kind starts with `reject`. An agent that offers only
    ///    [`option_kind::REJECT_ALWAYS`] must still be refusable, and a refusal
    ///    that is broader than asked for is safe in a way that a broader ALLOW
    ///    would not be. That asymmetry is why there is no matching fallback for
    ///    the two allow decisions: silently upgrading an "allow once" into an
    ///    "allow always" is precisely the bug this contract exists to prevent.
    ///    It is also why the fallback may still break a tie by order — every
    ///    candidate it can pick is a refusal.
    /// 3. Otherwise `None`.
    ///
    /// An option with no `kind` never matches — the contract reads an absent
    /// kind as "this agent states no semantics", and guessing one from the id is
    /// the id-sniffing this method exists to replace.
    ///
    /// # Why ambiguity refuses
    ///
    /// This was found against a **live gx leader** (plan 017; gx was retired in
    /// plan 025, and the rule outlives it), and it overturned the original rule
    /// ("first in offered order wins"). A real `session/request_permission`
    /// offered FIVE options, of which **two declared `allow_once`**:
    ///
    /// | optionId | kind | name |
    /// |---|---|---|
    /// | `enable-always-approve` | `allow_once` | "Yes, and don't ask again for anything (always-approve mode)" |
    /// | `allow-always-command` | `allow_always` | "Always allow: id -un" |
    /// | `allow-once` | `allow_once` | "Yes, proceed" |
    /// | `reject-once` | `reject_once` | "No, and tell Grok what to do differently" |
    /// | `reject-always-command` | `reject_always` | "Never allow: id -un" |
    ///
    /// Under first-in-offered-order, a human tapping **"Allow once"** would have
    /// selected `enable-always-approve` and silently turned off permission
    /// prompts for the whole session. The semantic kind simply does not
    /// disambiguate on a real agent: an agent may offer several options of one
    /// kind, and a three-valued [`LaneDecision`] cannot say which of them the
    /// human meant.
    ///
    /// Refusing costs a capability-driven client nothing — the panel renders the
    /// options the agent offered and posts back
    /// [`LaneAnswer::Choice`] with the exact offered id, which is unambiguous by
    /// construction. All that fails is the scripted three-decision shorthand,
    /// against an agent for which it is genuinely undecidable, and it fails
    /// LOUDLY as [`LaneError::BadRequest`] rather than by picking.
    ///
    /// `None` is not an error here: the caller turns it into
    /// [`LaneError::BadRequest`], because the message that helps ("craze
    /// offered no allow_always option") names the agent, and this module does
    /// not know which agent it is looking at.
    pub fn option_for(&self, decision: LaneDecision) -> Option<&LaneApprovalOption> {
        let wanted = match decision {
            LaneDecision::AllowOnce => option_kind::ALLOW_ONCE,
            LaneDecision::AllowAlways => option_kind::ALLOW_ALWAYS,
            LaneDecision::Reject => option_kind::REJECT_ONCE,
        };
        let mut exact = self
            .options
            .iter()
            .filter(|o| o.kind.as_deref() == Some(wanted));
        match (exact.next(), exact.next()) {
            // Exactly one option carries the kind: unambiguous.
            (Some(only), None) => return Some(only),
            // Several do. The decision cannot say which, so nothing is
            // selected — including for `Reject`, whose two candidates may
            // differ materially ("refuse" vs "refuse and tell the agent why").
            (Some(_), Some(_)) => return None,
            (None, _) => {}
        }
        if matches!(decision, LaneDecision::Reject) {
            // Reached only when NO option carries `reject_once`. Every
            // candidate here is a refusal, so breaking the tie by offered order
            // cannot broaden an allow.
            return self
                .options
                .iter()
                .find(|o| o.kind.as_deref().is_some_and(|k| k.starts_with("reject")));
        }
        None
    }
}

/// The three SEMANTIC decisions a permission approval accepts, independent of
/// what the agent named its options.
///
/// An adapter resolves one of these against [`LaneApproval::options`] **by
/// [`LaneApprovalOption::kind`], never by id** — through
/// [`LaneApproval::option_for`], which is the single implementation of the
/// rule: `AllowOnce` → the option whose kind is `allow_once`, `AllowAlways` →
/// `allow_always`, `Reject` → `reject_once` (falling back to the first option
/// whose kind starts `reject`, so an agent offering only `reject_always` is
/// still refusable). No match is
/// [`LaneError::BadRequest`] — silently picking the nearest option would post a
/// decision the human did not make.
///
/// **Neither is an AMBIGUOUS match.** A real gx permission (plan 017) offered
/// two options declaring `allow_once`, one of which disabled prompting for the
/// whole session; a three-valued decision cannot say which the human meant, so
/// `option_for` answers `None` and the adapter refuses. A client that wants a
/// specific offered option sends [`LaneAnswer::Choice`] instead — which is what
/// a capability-driven panel already does, and why refusing here costs it
/// nothing.
///
/// **Strict** under the module doc's asymmetric rule — this is a client→adapter
/// COMMAND, and an unrecognized decision must be rejected at decode rather than
/// tolerated: a swallowed decision is a user's "allow" silently becoming nothing.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum LaneDecision {
    /// Allow this one invocation.
    AllowOnce,
    /// Allow this and every matching future invocation in the session.
    AllowAlways,
    Reject,
}

/// The answer to an approval.
///
/// [`LaneAnswer::Question::answers`] is a vec-of-vecs: one inner vec per question
/// in [`LaneApproval::questions`], each holding the option ids chosen for it (one
/// entry unless that question is `multiple`). Free text rides BESIDE it in
/// [`LaneAnswer::Question::custom_text`], positionally — never smuggled into
/// `answers` as one more "id", which is what a client used to have to do and
/// what left an adapter unable to tell a chosen label from something typed.
/// [`normalize_question_answer`] is the one reader of the pair.
/// [`LaneAnswer::Raw`] carries a body the contract does not model, as
/// raw JSON in a `String` — the escape hatch for an unknown
/// [`LaneApprovalKind`].
///
/// **Strict** under the module doc's asymmetric rule (no `#[serde(other)]`, unlike
/// its mirror-image [`LaneEvent`]) — an answer travels client→adapter, so an
/// unrecognized `kind` is a command this build cannot honor and must be refused,
/// not degraded into a silent no-op.
///
/// Strictness runs to the FIELDS, not just the variant tag: `deny_unknown_fields`
/// means `{"kind":"permission","decision":"allow_always","option_id":"reject"}`
/// is REFUSED rather than decoding as a bare `AllowAlways` with the `option_id`
/// dropped. That payload reads as "refuse" and would have executed as "allow
/// always" — and it is reachable, because shed-mobile hand-mirrors this enum and
/// a Dart encoder mid-migration between the two answer shapes can emit both keys.
/// Being refused with an error is the only outcome that tells it so.
///
/// **One carve-out, forced by serde:** [`LaneAnswer::Reject`] is a UNIT variant,
/// and an internally tagged unit variant is built from the tag alone — it ignores
/// any other keys, and no attribute changes that. It is left as-is because the
/// direction is safe: extra keys on a `reject` are ignored in favour of refusing,
/// which is the conservative answer. Making it strict would mean turning it into
/// a struct variant, changing the Rust API at every construction site (the Tauri
/// crate included) for no wire change at all.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(tag = "kind", rename_all = "snake_case", deny_unknown_fields)]
pub enum LaneAnswer {
    Permission {
        decision: LaneDecision,
    },
    /// "The option with this id, exactly as it was offered."
    ///
    /// The generic answer, and the one a capability-driven panel sends: it
    /// renders [`LaneApproval::options`] as buttons in offered order and posts
    /// back whichever id the human pressed, without a table mapping this
    /// agent's option vocabulary onto three fixed decisions. That is what lets
    /// an agent offer four options (or two, or six) and a client render them
    /// without a code change.
    ///
    /// **Strict on both ends.** An id the approval did not offer is
    /// [`LaneError::BadRequest`] from the adapter — never a guess at the
    /// closest match, because the guess would be an unintended "allow". Named
    /// `Choice` and not `Option` for the obvious Rust reason.
    ///
    /// [`LaneAnswer::Permission`] survives beside it as the SEMANTIC answer
    /// ("allow this once", whatever the agent calls it) and is what a scripted
    /// caller or a keyboard shortcut sends; the adapter resolves it by
    /// [`LaneApprovalOption::kind`].
    Choice {
        option_id: String,
    },
    Question {
        #[serde(default)]
        answers: Vec<Vec<String>>,
        /// Free text per question, positional like `answers`; `None` where the
        /// human typed nothing. Empty vec = no free text anywhere.
        ///
        /// **Additive, and the compatibility is PRODUCER-side.** A newer
        /// decoder accepts an older client that never sends the field
        /// (`#[serde(default)]`), which is the direction a two-PR lockstep
        /// needs; an older decoder — this enum is `deny_unknown_fields` —
        /// REFUSES a payload carrying it. So the decoding side ships first.
        /// It is skipped when empty so an answer that carries no free text is
        /// byte-identical to what the previous build emitted.
        ///
        /// Named after [`LaneQuestion::custom`], the flag that permits it: a
        /// `Some` at a position whose question is not `custom` is a
        /// [`LaneError::BadRequest`], not a value to drop.
        #[serde(default, skip_serializing_if = "Vec::is_empty")]
        custom_text: Vec<Option<String>>,
    },
    /// Decline the whole request — distinct from a permission's
    /// [`LaneDecision::Reject`], which is one option among three.
    Reject,
    Raw {
        json: String,
    },
}

/// One question's answer after [`normalize_question_answer`] has read the
/// positional pair: the labels chosen for it, and the free text typed beside
/// them.
///
/// One per question in [`LaneApproval::questions`], in question order — a
/// question the client said nothing about is present with an empty `labels` and
/// no `text`, because "unanswered" is a thing each adapter renders its own way
/// (gx, until plan 025, omitted the key; opencode posts an empty list) and a
/// hole in the vec would make position stop meaning question.
#[derive(Debug, Clone, PartialEq, Eq, Default)]
pub struct QuestionReply {
    /// The option ids chosen for this question, in the order the client sent
    /// them. Empty when nothing was picked.
    pub labels: Vec<String>,
    /// The free text typed for this question, TRIMMED — and `None` rather than
    /// `Some("")` when the trim emptied it, so an adapter never has to decide
    /// whether whitespace was an answer.
    pub text: Option<String>,
}

/// Read a [`LaneAnswer::Question`]'s positional pair against the approval's own
/// questions — **the one implementation, shared by every adapter**.
///
/// It is here and not in each adapter for the reason correction 3 put
/// [`LaneApproval::option_for`] here: two adapters reading the same two vectors
/// would not stay agreed about the edges, and every edge below is a place where
/// disagreeing means silently dropping something a human typed.
///
/// The rules, in order:
///
/// 1. `answers` or `custom_text` LONGER than `questions` is
///    [`LaneError::BadRequest`]. Shorter is fine — the tail is unanswered.
/// 2. Both are extended to `questions.len()` with `[]` / `None`, so the result
///    is one reply per question and position keeps meaning question. This is
///    what makes a free-text-only answer to question 2 survive a `custom_text`
///    LONGER than `answers`; a `zip` of the two would drop it.
/// 3. Every `Some(t)` is trimmed, and an empty result becomes `None`. The
///    trimmed text is what is TRANSMITTED — an adapter never sees the padding,
///    so it cannot post one agent a trimmed answer and another a padded one.
/// 4. A `Some` at a position whose [`LaneQuestion::custom`] is `false` is
///    [`LaneError::BadRequest`]. The agent would refuse it (or, worse, file it
///    as a label it never offered), and refusing here happens BEFORE anything
///    reaches the wire.
///
/// The index in the refusal is the POSITION the client sent, 0-based — the same
/// index it addressed `custom_text` by.
pub fn normalize_question_answer(
    questions: &[LaneQuestion],
    answers: &[Vec<String>],
    custom_text: &[Option<String>],
) -> Result<Vec<QuestionReply>, LaneError> {
    if answers.len() > questions.len() {
        return Err(LaneError::BadRequest(format!(
            "the approval has {} questions; {} answers were given",
            questions.len(),
            answers.len()
        )));
    }
    if custom_text.len() > questions.len() {
        return Err(LaneError::BadRequest(format!(
            "the approval has {} questions; free text was given for {}",
            questions.len(),
            custom_text.len()
        )));
    }
    let mut out = Vec::with_capacity(questions.len());
    for (i, q) in questions.iter().enumerate() {
        let text = match custom_text.get(i).and_then(Option::as_deref) {
            None => None,
            Some(t) => match t.trim() {
                "" => None,
                trimmed => {
                    if !q.custom {
                        return Err(LaneError::BadRequest(format!(
                            "question {i} does not accept free text"
                        )));
                    }
                    Some(trimmed.to_string())
                }
            },
        };
        out.push(QuestionReply {
            labels: answers.get(i).cloned().unwrap_or_default(),
            text,
        });
    }
    Ok(out)
}

// ---- the event stream ----

/// One frame of a [`AgentLane::subscribe`] stream.
///
/// The [`LaneEvent::Reset`] … [`LaneEvent::Ready`] bracket is the whole
/// reseed story: see the module doc's "Semantics pinned for every adapter".
/// `generation` increments per reseed within one subscription and is what a
/// client discards stale frames by — and MATCHES a `Ready` against (module doc,
/// "Generations are matched, not assumed").
///
/// **Every payload-carrying variant nests its payload under a named key** rather
/// than flattening a newtype into the tagged object. That is forced, not
/// stylistic: `tag = "kind"` writes the discriminator into the SAME object as the
/// variant's fields, and [`LaneApproval`] (and [`LaneCapabilities`]) have a
/// `kind` field of their own — a flattened `Approval(LaneApproval)` emits
/// `"kind"` twice and decodes back as the approval's kind, so the round trip
/// silently loses the event. Nesting also keeps the Dart sealed-class mirror
/// uniform (one payload field per case).
///
/// **Tolerant** under the module doc's asymmetric unknown-value rule: this is THE
/// inbound type, and [`LaneEvent::Unknown`] is the catch-all that keeps one
/// unrecognized frame from failing the decode of the stream it arrived on.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(tag = "kind", rename_all = "snake_case")]
pub enum LaneEvent {
    /// A transcript row. `cursor` is a per-row resume token, on an adapter that
    /// has one to show; nothing in the contract requires it (craze resumes from
    /// a cursor the adapter keeps for itself).
    Message {
        message: RcFeedMessage,
        #[serde(default, skip_serializing_if = "Option::is_none")]
        cursor: Option<String>,
    },
    /// The session row changed (title, activity, pending count).
    Session { session: LaneSession },
    /// An approval appeared, or an existing one changed status. Id-keyed and
    /// LAST-WRITE-WINS, exactly like [`crate::rc::RcFeedApproval`]: a client must
    /// not require having seen the `pending` frame before the `resolved` one.
    Approval { approval: LaneApproval },
    /// What this SESSION can do, now. In every seed before its `Ready`, and
    /// re-emitted whenever it changes (module doc, "Capabilities and settings
    /// ride the stream"). A client holds the latest one and gates its
    /// affordances on it; it never caches capabilities at open.
    Capabilities { capabilities: LaneCapabilities },
    /// The session's settings — its model, mode and the model's options. Only
    /// on a session whose capabilities say [`LaneCapabilities::settings`]: in
    /// every seed (after its `Capabilities`, before its `Ready`) and on change.
    Settings { settings: LaneSettings },
    /// The transport went away and the adapter is retrying **without** a reseed.
    /// **Non-terminal**: the client keeps its rows and shows them stale with
    /// `reason`. A lone [`LaneEvent::Ready`] of the SAME generation clears it (a
    /// silent resume); a `Reset … Ready` replaces the rows (a refused one); a
    /// `Down` ends the subscription. A client never reopens a lane on a `Stale`
    /// — reopening throws away the cursor the resume needs.
    Stale { reason: String },
    /// A **reseed** started: DISCARD nothing yet, but stage every frame that
    /// follows until the matching [`LaneEvent::Ready`], then swap atomically.
    ///
    /// `reason` is FREE TEXT for a human and a log line — `connect`,
    /// `cursor_lost`, `server_reset:<r>`, `stall` — and nothing branches on it.
    /// A reconnect that resumed from a cursor emits no `Reset` at all (module
    /// doc, correction 1).
    Reset { reason: String, generation: u64 },
    /// The seed for `generation` is complete — swap the staged view in. A
    /// `Ready` with NO seed staged is a silent resume's end: it clears the stale
    /// mark when `generation` is the live one, and changes nothing else.
    Ready { generation: u64 },
    /// The transport is gone and this subscription has ENDED (the adapter's task
    /// exits after emitting it). A normal state, not an error: render the panel
    /// stale-with-a-reason. The only frame that ends a subscription, and the
    /// only thing a client reopens a lane after.
    Down { reason: String },
    /// A frame whose `kind` this build has never heard of.
    ///
    /// **Clients IGNORE an `Unknown` frame.** It is not an error, not a gap, and
    /// not a reason to resubscribe or to refetch — it is one frame this build
    /// cannot name, from a producer that grew a frame kind (a heartbeat, an error
    /// frame, a per-turn cost row) after this client was built. Without it, that
    /// one frame fails `serde`'s decode and takes the whole stream's read loop
    /// with it: every older client loses the subscription the moment a newer
    /// adapter ships. `#[serde(other)]` IS supported on an internally tagged enum,
    /// which this is.
    ///
    /// Nothing from the original object survives, by construction on both sides:
    /// `#[serde(other)]` admits only a UNIT variant, and the FRB-mirror rule bars
    /// the `serde_json::Value` that could otherwise carry the body. That is an
    /// acceptable loss — a client that cannot name the kind has nothing to do with
    /// the payload anyway — but it does mean `Unknown` must never be used to carry
    /// meaning. An adapter NEVER CONSTRUCTS it; it exists only as a decode
    /// outcome. (It does serialize, as `{"kind":"unknown"}`, so a relay that
    /// re-encodes what it decoded degrades the frame rather than corrupting it.)
    #[serde(other)]
    Unknown,
}

/// One frame of an [`AgentSource::subscribe`] stream: the machine's session
/// list.
///
/// The bracket is the lane's: [`SourceEvent::Reset`] …
/// [`SourceEvent::Session`]* … [`SourceEvent::Capabilities`] …
/// [`SourceEvent::Ready`] carries a whole (re)seed of the list, which a client
/// stages and swaps; between seeds, `Session` upserts a row by its `id` and
/// `Removed` drops one. See the module doc's "Sources".
///
/// Nested payloads and the tolerant `Unknown` for the same reasons as
/// [`LaneEvent`]'s.
///
/// `Session` carries its [`LaneSession`] by value, as [`LaneEvent::Session`]
/// does — the shape plan 025 §3.2.2 pins and shed-mobile mirrors — which makes
/// it far the largest variant (clippy's `large_enum_variant`). Boxing it would
/// change no byte on the wire and every construction and match site in every
/// client; the cost it would save is bounded anyway — a subscription queues at
/// most [`LANE_CHANNEL_CAPACITY`] frames — so the lint is allowed here, on this
/// type alone.
#[allow(clippy::large_enum_variant)]
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(tag = "kind", rename_all = "snake_case")]
pub enum SourceEvent {
    /// A roster reseed started: stage until the matching `Ready`. `reason` is
    /// free text nothing branches on.
    Reset { reason: String, generation: u64 },
    /// One row, upserted by `session.id`. A source never sets
    /// [`LaneSession::tab_id`].
    Session { session: LaneSession },
    /// A row left the list.
    Removed { session_id: String },
    /// What this source can do. In every seed before its `Ready`, and on
    /// change.
    Capabilities { capabilities: SourceCapabilities },
    /// The seed for `generation` is complete — swap it in. `truncated` says the
    /// source cut the list to a bound of its own, so a client knows the rows it
    /// holds are not every session there is.
    Ready {
        generation: u64,
        /// Absent decodes to `false` — the reading that claims nothing.
        #[serde(default)]
        truncated: bool,
    },
    /// The source cannot reach its sessions right now. **Non-terminal**: it
    /// keeps trying, the client keeps the last `Ready` view and marks it stale
    /// with `cause`, and the next `Reset … Ready` restores it. A source may
    /// emit one before any `Reset`. Never `Down`: a source subscription ends
    /// only when its subscriber stops it.
    Offline {
        reason: String,
        cause: SourceOffline,
    },
    /// A frame whose `kind` this build has never heard of — ignored, exactly as
    /// [`LaneEvent::Unknown`] is.
    #[serde(other)]
    Unknown,
}

/// What a source can do — the machine-level half of the capabilities, riding
/// every source seed as [`SourceEvent::Capabilities`].
///
/// `kind` is the same agent token [`LaneCapabilities::kind`] carries. `create`
/// and `create_options` say whether [`AgentSource::create`] and
/// [`AgentSource::create_options`] will be honoured on THIS source: a craze
/// hub too old to offer providers answers `create_options: false`, and a
/// client hides the create sheet rather than discovering the refusal on a tap.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct SourceCapabilities {
    pub kind: String,
    pub create: bool,
    pub create_options: bool,
}

/// Why a source is [`SourceEvent::Offline`].
///
/// **Tolerant**, as a bare snake_case string with an `Other` that preserves an
/// unrecognized word — [`LaneApprovalKind`]'s pattern exactly, because derived
/// serde would encode `Other("x")` as an object and a newer producer's cause
/// would fail the whole frame.
///
/// What each means is the UI's to decide (module doc, "Sources"): `NotInstalled`
/// is quiet — the machine simply has no such agent — while `TooOld` asks the
/// person to update it there.
#[derive(Debug, Clone, PartialEq, Eq, Hash)]
pub enum SourceOffline {
    /// The agent is not installed on this machine.
    NotInstalled,
    /// It is installed, and too old to speak this protocol.
    TooOld,
    /// It could not be reached (a dial failed, a connection ended, a poll
    /// failed). The ordinary, transient cause.
    Unreachable,
    /// It answered, and refused.
    Failed,
    /// An unrecognized cause, its raw wire string preserved.
    Other(String),
}

impl SourceOffline {
    pub fn as_str(&self) -> &str {
        match self {
            SourceOffline::NotInstalled => "not_installed",
            SourceOffline::TooOld => "too_old",
            SourceOffline::Unreachable => "unreachable",
            SourceOffline::Failed => "failed",
            SourceOffline::Other(s) => s,
        }
    }

    /// Parse a wire cause, preserving an unrecognized one as
    /// [`SourceOffline::Other`]. Never mints an `Other` holding a known string
    /// (the [`LaneApprovalKind`] round-trip caveat applies identically).
    pub fn from_wire(s: &str) -> SourceOffline {
        match s {
            "not_installed" => SourceOffline::NotInstalled,
            "too_old" => SourceOffline::TooOld,
            "unreachable" => SourceOffline::Unreachable,
            "failed" => SourceOffline::Failed,
            other => SourceOffline::Other(other.to_string()),
        }
    }

    /// A recognized cause. A `false` here is the neutral-render signal.
    pub fn is_known(&self) -> bool {
        !matches!(self, SourceOffline::Other(_))
    }
}

impl Serialize for SourceOffline {
    fn serialize<S: serde::Serializer>(&self, s: S) -> Result<S::Ok, S::Error> {
        s.serialize_str(self.as_str())
    }
}

impl<'de> Deserialize<'de> for SourceOffline {
    fn deserialize<D: serde::Deserializer<'de>>(d: D) -> Result<Self, D::Error> {
        Ok(SourceOffline::from_wire(&tolerant_word(d)?))
    }
}

/// The plan-025 tolerant enums' reader: a JSON string is the word; ANY other
/// JSON value is kept as its compact JSON text, which no known word can equal
/// (a known word is a bare snake_case name; a non-string's text is a number,
/// `true`/`false`/`null`, or something bracketed) — so it always lands in
/// `Other` and the frame around it still decodes (module doc, "Unknown-value
/// tolerance").
fn tolerant_word<'de, D: serde::Deserializer<'de>>(d: D) -> Result<String, D::Error> {
    Ok(match serde_json::Value::deserialize(d)? {
        serde_json::Value::String(word) => word,
        other => other.to_string(),
    })
}

// ---- the frame channel ----

/// How many frames a subscription may have queued for a client before the
/// adapter's next publish is dropped and the generation reseeds (module doc,
/// correction 13). One bound for both levels: a lane's and a source's
/// subscription run on the same [`Publisher`].
///
/// **Frames, not bytes.** A seed of 500 transcript rows plus a session row and
/// its approvals fits inside this with room to spare, so a client that drains
/// promptly never sees the bound at all; what it catches is the client that
/// stops draining entirely, whose queue used to grow without limit.
pub const LANE_CHANNEL_CAPACITY: usize = 1024;

/// The most rows one SOURCE seed may carry (module doc, "Sources").
///
/// craze's hub caps its own roster at the same 512, and the number is chosen
/// against [`LANE_CHANNEL_CAPACITY`]: a seed is one synchronous burst —
/// `Reset`, the rows, `Capabilities`, `Ready` — and one that does not fit the
/// channel lags at the same frame on every reseed, forever. 512 rows leaves the
/// other half of the channel for whatever the client had not yet drained. A
/// source that had more rows than this sends the first 512 and marks the seed
/// [`SourceEvent::Ready`]`{ truncated: true }`.
pub const MAX_SOURCE_ROWS: usize = 512;

/// How often [`Publisher::wait_drained`] re-reads the queue.
///
/// Short enough that a consumer which catches up is not made to wait for its
/// reseed, long enough that a lane parked behind a consumer that never drains
/// costs a timer wakeup rather than a spin.
const DRAIN_POLL: std::time::Duration = std::time::Duration::from_millis(5);

/// What one [`Publisher::publish`] did.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash)]
pub enum Publish {
    /// Queued for the client.
    Sent,
    /// The receiver is gone — the subscription is over. An adapter may keep
    /// going and let its run loop notice; nothing it publishes will be read.
    Closed,
    /// The queue was FULL and this frame was **dropped**. The generation must
    /// end here (module doc, correction 13): a client that saw the frames
    /// either side of a hole has no way to know there was one.
    Lagged,
}

/// The subscriber is gone: [`Publisher::wait_drained`]'s error.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash)]
pub struct Closed;

impl std::fmt::Display for Closed {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str("the lane subscriber is gone")
    }
}

impl std::error::Error for Closed {}

/// The producing half of a [`Subscription`] — and the one place the overflow
/// policy of module-doc correction 13 is written down, for BOTH levels.
///
/// Generic over the frame type so a lane ([`LanePublisher`], over
/// [`LaneEvent`]) and a source ([`SourcePublisher`], over [`SourceEvent`]) share
/// one implementation of that policy. A parallel copy per level was rejected:
/// two copies of the one rule the module doc says there is one of drift the
/// first time somebody fixes one of them.
///
/// **Not `Clone`, on purpose.** One publisher owns one subscription. It is what
/// makes [`Publisher::wait_drained`] meaningful: a second producer on the
/// same channel could refill the slots the first one just waited for, and the
/// "wait until the consumer has caught up" the reseed depends on would be a
/// wait for nothing.
pub struct Publisher<T> {
    tx: mpsc::Sender<T>,
}

/// A lane's publisher: [`LaneEvent`] frames.
pub type LanePublisher = Publisher<LaneEvent>;
/// A source's publisher: [`SourceEvent`] frames.
pub type SourcePublisher = Publisher<SourceEvent>;

impl<T> Publisher<T> {
    /// The channel one subscription runs on: this publisher, and the receiver
    /// that goes into [`Subscription::rx`].
    pub fn channel() -> (Publisher<T>, mpsc::Receiver<T>) {
        let (tx, rx) = mpsc::channel(LANE_CHANNEL_CAPACITY);
        (Publisher { tx }, rx)
    }

    /// Queue one frame, **without ever awaiting**.
    ///
    /// The publishing task is the one reading the agent's own event stream, so
    /// it must not block on a slow consumer: a full queue answers
    /// [`Publish::Lagged`] and the frame is dropped. Every caller propagates
    /// that rather than absorbing it — the generation ends at the first dropped
    /// frame and reseeds.
    pub fn publish(&self, ev: T) -> Publish {
        match self.tx.try_send(ev) {
            Ok(()) => Publish::Sent,
            Err(mpsc::error::TrySendError::Full(_)) => Publish::Lagged,
            Err(mpsc::error::TrySendError::Closed(_)) => Publish::Closed,
        }
    }

    /// Deliver a terminal frame — a lane's [`LaneEvent::Down`] — **waiting for
    /// room if there is none**. Consumes the publisher, because there is
    /// nothing after it.
    ///
    /// This is the one frame that is never dropped. It is sent after the
    /// transport is gone, so the await can stall nothing that matters — and it
    /// is what stops a client holding a `Ready` view forever with no stale
    /// reason, because the one frame that would have told it was the one that
    /// hit a full queue. (A source has no terminal frame — its subscription ends
    /// only when the subscriber stops it — so a source adapter never calls this.)
    pub async fn publish_final(self, ev: T) {
        // `Err` only means the subscriber is already gone, which is the state
        // this was going to tell it about.
        let _ = self.tx.send(ev).await;
    }

    /// Whether the subscriber has dropped its receiver.
    pub fn is_closed(&self) -> bool {
        self.tx.is_closed()
    }

    /// Resolve once the client has drained **every** queued frame.
    ///
    /// Not "there is room again": all of it. A reseed republishes a whole seed,
    /// and starting it against a queue that is merely no longer full would lag
    /// again a few frames in — which is how a slow consumer turns into a
    /// reseed loop against a live agent. [`Closed`] means the subscriber went
    /// away while we waited; the adapter then stops silently, with no `Down`
    /// and no reseed, exactly as it does for the `is_closed` checks it already
    /// makes.
    pub async fn wait_drained(&self) -> Result<(), Closed> {
        loop {
            if self.tx.is_closed() {
                return Err(Closed);
            }
            if self.tx.capacity() == LANE_CHANNEL_CAPACITY {
                return Ok(());
            }
            tokio::time::sleep(DRAIN_POLL).await;
        }
    }
}

/// A live subscription: the frames, and the handle that ends it.
///
/// Deliberately the shape of `shed_app::roost::RoostWatcher` — a spawned task, a
/// channel, [`LaneStop::stop`] aborts, `Drop` stops — so a lane subscription, a
/// source subscription and a roost watcher are torn down the same way in a
/// client that holds all three. Not restartable: subscribe again.
///
/// The channel is **bounded** at [`LANE_CHANNEL_CAPACITY`] frames (module doc,
/// correction 13). `recv().await` is unchanged by that; what changes is what a
/// client that stops reading costs — a reseed on its own subscription, rather
/// than an adapter queueing frames for it forever.
///
/// # Keep BOTH halves alive — the partial move that silently kills the pump
///
/// Both fields are public (the contract pins them that way), so taking just the
/// receiver COMPILES. It is a partial move, and the abandoned `stop` half is
/// dropped — which aborts the pump, because aborting on drop IS the documented
/// teardown. The receiver you kept then yields nothing, forever, with no `Err`,
/// no [`LaneEvent::Down`] and nothing in a log. A client written this way renders
/// an empty transcript and blames the adapter.
///
/// **WRONG — dead on arrival.** When the subscription is a temporary or is owned
/// by the function doing the move, `stop` drops right there:
///
/// ```text
/// Ok(lane.subscribe(None).await?.rx)     // temporary: `stop` dies at the `?`
/// let rx = lane.subscribe(None).await?.rx;
/// fn open(..) -> Receiver<LaneEvent> { let sub = ..; sub.rx }   // dies on return
/// ```
///
/// **ALSO WRONG, and worse to debug** — a partial move out of a LOCAL leaves
/// `stop` in place until the end of that local's scope, so the frames flow for as
/// long as you are looking at them and stop the instant the receiver outlives the
/// scope it was taken in:
///
/// ```text
/// let rx = { let sub = lane.subscribe(None).await?; sub.rx };  // `stop` dies here
/// let LaneSubscription { rx, .. } = sub;                       // same, discarded by `..`
/// self.rx = Some(sub.rx);                                      // and stored, `stop` dropped
/// ```
///
/// Use [`Subscription::into_parts`], and bind BOTH halves for as long as you
/// intend to read frames:
///
/// ```text
/// let (mut rx, stop) = lane.subscribe(None).await?.into_parts();
/// while let Some(ev) = rx.recv().await { render(ev); }
/// drop(stop);                       // or just let it fall out of scope
/// ```
///
/// Storing only `rx` on a long-lived struct is the same bug wearing a field name:
/// keep the [`LaneStop`] next to it (or somewhere that outlives it), which is the
/// reason the two are split at all.
pub struct Subscription<T> {
    pub rx: mpsc::Receiver<T>,
    pub stop: LaneStop,
}

/// A lane's subscription: [`LaneEvent`] frames.
pub type LaneSubscription = Subscription<LaneEvent>;
/// A source's subscription: [`SourceEvent`] frames.
pub type SourceSubscription = Subscription<SourceEvent>;

impl<T> Subscription<T> {
    /// Split into the frames and the lifetime handle — the named, safe idiom.
    ///
    /// Mechanically this does nothing a field move cannot; the point is that the
    /// call site is forced to say what it does with BOTH halves, where `sub.rx`
    /// lets the important one disappear without a word (see the type doc). The
    /// returned [`LaneStop`] still aborts the pump when it drops, so bind it —
    /// `let (rx, _) = ….into_parts();` reintroduces the exact bug this exists to
    /// prevent.
    pub fn into_parts(self) -> (mpsc::Receiver<T>, LaneStop) {
        (self.rx, self.stop)
    }
}

/// The abort handle half of a [`Subscription`] — a lane's or a source's.
///
/// Dropping it stops the adapter's task, which is what closes the transport it
/// holds. Split out from the receiver so a client can move the frames into a
/// render loop and keep the lifetime handle somewhere else.
pub struct LaneStop {
    task: tokio::task::JoinHandle<()>,
}

impl LaneStop {
    /// Wrap the adapter's spawned pump task. The adapter calls this; a client
    /// only ever calls [`LaneStop::stop`] (or drops it).
    pub fn new(task: tokio::task::JoinHandle<()>) -> LaneStop {
        LaneStop { task }
    }

    /// Abort the pump. Dropping the in-flight future closes whatever transport
    /// it held.
    pub fn stop(&self) {
        self.task.abort();
    }
}

impl Drop for LaneStop {
    fn drop(&mut self) {
        self.stop();
    }
}

// ---- capabilities, send mode, errors ----

/// How a [`AgentLane::send`] is meant to land.
///
/// **Strict** under the module doc's asymmetric rule — a send mode is a
/// client→adapter COMMAND, so an unrecognized value is refused rather than
/// tolerated; degrading an unknown mode to a default would deliver the message
/// with semantics the caller did not ask for.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum SendMode {
    /// "Accepted, and ordered after whatever is in flight."
    ///
    /// **Not a queue position.** On opencode this is `prompt_async`, which a
    /// `Working` session accepts (204) and whose runner delivers per its own
    /// rules — it joins the running runner rather than appending to a strict
    /// queue (`prompt.ts:1052`, `runner.ts:115` in the vendored source). The
    /// contract promises ACCEPTANCE and ORDERING, nothing about where in a
    /// backlog the text sits.
    Queue,
    /// Interrupt the turn in flight and deliver now. Adapters that cannot do
    /// this advertise `interject: false` and answer
    /// [`LaneError::NotAccepting`].
    Interject,
}

/// What this SESSION can do — advertised on the stream
/// ([`LaneEvent::Capabilities`]), so a client greys out the affordance instead
/// of discovering the refusal on a tap.
///
/// **Per session, not per adapter** (module doc, "Capabilities and settings ride
/// the stream"): a craze session hosted by its TUI cannot be stopped where one
/// `craze serve` hosts can, and a session gains settings when its model offers
/// options. So these arrive in every seed and again on change, and a client
/// reads the latest from its view — it never caches them at open.
///
/// opencode's row, in every seed:
/// `{ kind: "opencode", interject: false, cancel: true, approvals: true, history_cursor: false, settings: false, stop: false }`.
/// craze's is the session's own (plan 025 §3.3.9): `interject`, `cancel` and
/// `stop` as the session states them, `approvals` from its ask/plan cards,
/// `history_cursor: true`, `settings` when it has a model, mode or option to
/// show.
///
/// `create` left this type in plan 025: creating is a machine-level act, and it
/// is [`SourceCapabilities::create`] now. An older producer's `create` key is
/// an unknown field, which this tolerant type ignores.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct LaneCapabilities {
    /// The adapter's agent token (`"opencode"`, `"craze"`, …) — the same vocabulary
    /// [`crate::rc::RcKind::tool`] speaks.
    pub kind: String,
    /// [`SendMode::Interject`] is honored.
    pub interject: bool,
    /// [`AgentLane::cancel`] works.
    pub cancel: bool,
    /// Approvals surface and can be answered.
    pub approvals: bool,
    /// The STREAM may resume from a cursor silently (module doc, correction 1):
    /// a `true` here means a reconnect may leave no `Reset` in the event stream
    /// — only a [`LaneEvent::Stale`] and, once resumed, a lone `Ready` of the
    /// same generation.
    ///
    /// **Redefined in plan 025, name kept.** It used to say BOTH that and that
    /// [`AgentLane::history`] honours a cursor. It now means only the stream
    /// half: `history`'s cursor is advisory (both adapters ignore it,
    /// [`LaneHistory::cursor`]). Renaming it would have churned every mirror for
    /// no behaviour.
    pub history_cursor: bool,
    /// The session has settings to show and change ([`AgentLane::settings`],
    /// [`AgentLane::set`]), and its seeds carry [`LaneEvent::Settings`]. Absent
    /// decodes to `false`: a producer from before the field existed offers none.
    #[serde(default)]
    pub settings: bool,
    /// [`AgentLane::stop`] works — the session can be ended from this client.
    /// Absent decodes to `false`, the reading that offers no Stop button.
    #[serde(default)]
    pub stop: bool,
}

/// What a lane call can go wrong with.
///
/// Split so a caller never string-matches to decide what to do: the
/// `Unknown*`/`Already*` variants are "your view is stale, refetch", `Unauthorized`
/// and `NotAccepting` are "the affordance should not have been offered",
/// `Unavailable` is quiet (the agent is not running — render stale, keep the row),
/// and `Failed` is the loud residue.
///
/// **Wire shape is heterogeneous** and that is forced, not stylistic:
/// `BadRequest(String)`/`Unavailable(String)`/`Failed(String)` are newtypes over a
/// non-map, which serde REFUSES to tag internally, so this enum is externally
/// tagged — a unit variant is a bare string (`"unknown_session"`), a payload
/// variant a one-key object (`{"bad_request":"…"}`). shed-mobile mirrors both
/// shapes by hand; a decoder that handles only one of them breaks on half the
/// enum. The `rename_all = "snake_case"` codes line up with the plan's error
/// envelope.
///
/// The VARIANTS stay **strict** (no `Other`, no `#[serde(other)]`): an error is
/// something a caller branches on to decide what to do next, and a
/// silently-tolerated unknown code would land in whatever arm it fell into —
/// "refetch, your view is stale" and "the affordance should not have been offered"
/// are not interchangeable. A code this build does not know is `Failed`'s job,
/// carried as text by whoever mapped the wire.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize, thiserror::Error)]
#[serde(rename_all = "snake_case")]
pub enum LaneError {
    /// The agent demanded a credential this build cannot supply.
    #[error("the agent requires authentication")]
    Unauthorized,
    /// The agent rejected the request and said why. The string is ITS message,
    /// kept whole — it is the only thing that explains a schema-level refusal.
    #[error("{0}")]
    BadRequest(String),
    /// No such session — it ended, or was never on this adapter. Refetch the
    /// roster.
    #[error("no such session")]
    UnknownSession,
    /// No such approval — evicted, or answered elsewhere. Refetch the approvals.
    #[error("no such approval")]
    UnknownApproval,
    /// An answer is already in flight for this approval (the optimistic
    /// [`LaneApprovalStatus::Submitted`] state). A double-tap, not an error to
    /// surface loudly.
    #[error("that approval already has an answer in flight")]
    AlreadySubmitted,
    /// The approval was already resolved — by another client, or by the agent's
    /// own TUI.
    #[error("that approval is already resolved")]
    AlreadyResolved,
    /// The adapter or the session will not take this right now: an
    /// [`SendMode::Interject`] on an adapter that cannot, a prompt to a session
    /// that is closing.
    #[error("the session is not accepting that right now")]
    NotAccepting,
    /// Nothing to talk to: the agent is not running, the dial was refused, the
    /// tunnel is down. **Quiet** — the caller renders an unreachable row with
    /// this reason, not an error dialog. The string names what was tried.
    #[error("{0}")]
    Unavailable(String),
    /// Anything else that went wrong on the wire, kept whole.
    #[error("{0}")]
    Failed(String),
}

// ---- creating a session (the source's half) ----

/// What [`AgentSource::create`] can start on this machine, before any session
/// exists: the providers and whether each can start, the default, and the
/// directories sessions last ran in.
///
/// **Inbound, tolerant**: an unknown field is ignored and an absent list
/// decodes empty. Providers come in the agent's own order and a client keeps it
/// (plan 025 D5: a provider that cannot start is dimmed, not hidden).
#[derive(Debug, Clone, PartialEq, Eq, Default, Serialize, Deserialize)]
pub struct LaneCreateOptions {
    #[serde(default)]
    pub providers: Vec<LaneProvider>,
    /// The provider a create that names none starts. May name one that is not
    /// listed or not ready — a client preselects it only when it is listed AND
    /// [`LaneProviderState::Ready`].
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub default_provider: Option<String>,
    /// The directories sessions ran in, newest first.
    #[serde(default)]
    pub recent_dirs: Vec<String>,
}

/// One provider a create can name.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct LaneProvider {
    /// What [`LaneCreateRequest::provider`] takes.
    pub id: String,
    /// What a client shows.
    pub label: String,
    pub state: LaneProviderState,
    /// Why it cannot start — present exactly when `state` is not `Ready`.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub reason: Option<String>,
    /// What to do about it, one line.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub fix: Option<String>,
}

/// Whether a provider can start here.
///
/// **Tolerant**: a bare snake_case string with an `Other` that preserves an
/// unrecognized word ([`LaneApprovalKind`]'s pattern exactly). A client treats
/// `Other` as not ready — it cannot know the state permits a start.
#[derive(Debug, Clone, PartialEq, Eq, Hash)]
pub enum LaneProviderState {
    /// It can start here.
    Ready,
    /// It cannot start until something the agent can lead a person through is
    /// set up (a key, a login).
    NeedsSetup,
    /// It cannot start, and nothing in the agent can make it.
    Unavailable,
    /// An unrecognized state, its raw wire string preserved.
    Other(String),
}

impl LaneProviderState {
    pub fn as_str(&self) -> &str {
        match self {
            LaneProviderState::Ready => "ready",
            LaneProviderState::NeedsSetup => "needs_setup",
            LaneProviderState::Unavailable => "unavailable",
            LaneProviderState::Other(s) => s,
        }
    }

    /// Parse a wire state, preserving an unrecognized one as
    /// [`LaneProviderState::Other`]. Never mints an `Other` holding a known
    /// string.
    pub fn from_wire(s: &str) -> LaneProviderState {
        match s {
            "ready" => LaneProviderState::Ready,
            "needs_setup" => LaneProviderState::NeedsSetup,
            "unavailable" => LaneProviderState::Unavailable,
            other => LaneProviderState::Other(other.to_string()),
        }
    }

    /// A recognized state. A `false` here is the neutral-render signal.
    pub fn is_known(&self) -> bool {
        !matches!(self, LaneProviderState::Other(_))
    }
}

impl Serialize for LaneProviderState {
    fn serialize<S: serde::Serializer>(&self, s: S) -> Result<S::Ok, S::Error> {
        s.serialize_str(self.as_str())
    }
}

impl<'de> Deserialize<'de> for LaneProviderState {
    fn deserialize<D: serde::Deserializer<'de>>(d: D) -> Result<Self, D::Error> {
        Ok(LaneProviderState::from_wire(&tolerant_word(d)?))
    }
}

/// A create, as a client asks for it (plan 025 D6: a provider, a directory and
/// an optional first prompt — nothing else; the model is the provider's
/// default).
///
/// **Outbound, strict** (`deny_unknown_fields`): a create carrying a field this
/// build cannot honour (a model, a permission mode a newer client grew) is
/// refused at decode rather than started without it.
///
/// `request_id` makes the create idempotent where the agent supports it: a
/// client reuses one ONLY while the outcome of the create that carried it is
/// unknown (a lost connection, a deadline), and mints a fresh one after any
/// definite answer, failures included (plan 025 §3.3.3, §3.8). An adapter whose
/// agent has no such idempotency (opencode) accepts it and ignores it.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct LaneCreateRequest {
    /// The new session's working directory — absolute, and existing on the
    /// machine.
    pub cwd: String,
    /// A [`LaneProvider::id`]; `None` for the agent's own default.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub provider: Option<String>,
    /// The first prompt; `None` creates an idle session.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub prompt: Option<String>,
    pub request_id: String,
}

/// What a create answered: the new session's row, and what became of its first
/// prompt.
///
/// **Inbound, tolerant.** The session exists whenever this is returned —
/// `prompt` says only what happened to the prompt, so a refused or lost prompt
/// is not a failed create.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct LaneCreated {
    /// The new row. Its `id` is what [`AgentSource::open`] takes.
    pub session: LaneSession,
    pub prompt: LanePromptOutcome,
    /// Why the prompt was refused, or why its answer was lost.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub prompt_error: Option<String>,
}

/// What became of a create's first prompt.
///
/// **Tolerant**: a bare snake_case string with an `Other` that preserves an
/// unrecognized word ([`LaneApprovalKind`]'s pattern exactly).
#[derive(Debug, Clone, PartialEq, Eq, Hash)]
pub enum LanePromptOutcome {
    /// There was no prompt.
    None,
    /// The session took it.
    Accepted,
    /// It was sent and its answer was lost: the session may be working on it.
    Unknown,
    /// The session refused it, and runs on, idle.
    Refused,
    /// An unrecognized outcome, its raw wire string preserved.
    Other(String),
}

impl LanePromptOutcome {
    pub fn as_str(&self) -> &str {
        match self {
            LanePromptOutcome::None => "none",
            LanePromptOutcome::Accepted => "accepted",
            LanePromptOutcome::Unknown => "unknown",
            LanePromptOutcome::Refused => "refused",
            LanePromptOutcome::Other(s) => s,
        }
    }

    /// Parse a wire outcome, preserving an unrecognized one as
    /// [`LanePromptOutcome::Other`]. Never mints an `Other` holding a known
    /// string.
    pub fn from_wire(s: &str) -> LanePromptOutcome {
        match s {
            "none" => LanePromptOutcome::None,
            "accepted" => LanePromptOutcome::Accepted,
            "unknown" => LanePromptOutcome::Unknown,
            "refused" => LanePromptOutcome::Refused,
            other => LanePromptOutcome::Other(other.to_string()),
        }
    }

    /// A recognized outcome. A `false` here is the neutral-render signal.
    pub fn is_known(&self) -> bool {
        !matches!(self, LanePromptOutcome::Other(_))
    }
}

impl Serialize for LanePromptOutcome {
    fn serialize<S: serde::Serializer>(&self, s: S) -> Result<S::Ok, S::Error> {
        s.serialize_str(self.as_str())
    }
}

impl<'de> Deserialize<'de> for LanePromptOutcome {
    fn deserialize<D: serde::Deserializer<'de>>(d: D) -> Result<Self, D::Error> {
        Ok(LanePromptOutcome::from_wire(&tolerant_word(d)?))
    }
}

// ---- settings (the lane's half) ----

/// A session's settings, rendered generically (plan 025 D7): the model and the
/// models it can move to, the mode and the modes, the model's own options, and
/// how full its context is.
///
/// **Inbound, tolerant**; [`Default`] is "nothing to show", which is what an
/// adapter whose capabilities say `settings: false` answers
/// [`AgentLane::settings`] with.
#[derive(Debug, Clone, PartialEq, Eq, Default, Serialize, Deserialize)]
pub struct LaneSettings {
    /// The current model's id.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub model: Option<String>,
    /// The models a [`LaneSettingChange::Model`] can name, in the order a client
    /// shows them.
    #[serde(default)]
    pub models: Vec<LaneChoice>,
    /// The current mode's id.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub mode: Option<String>,
    #[serde(default)]
    pub modes: Vec<LaneChoice>,
    /// The current model's own options (an effort level, a fast mode, …).
    #[serde(default)]
    pub options: Vec<LaneSetting>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub usage: Option<LaneUsage>,
}

/// One selectable value: a model, a mode, or one option's value.
///
/// craze's `selectValues[{value, name}]` maps as `{id: value, name}`.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct LaneChoice {
    /// What a [`LaneSettingChange`] sends back. Opaque.
    pub id: String,
    /// What a client shows.
    pub name: String,
    /// The agent's own ordering hint, when it gives one (lower first).
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub rank: Option<u32>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub description: Option<String>,
}

/// One of a model's options.
///
/// `current` and each value are STRINGS: craze's config options are selects
/// with string values in all observed data, and a future non-select type maps
/// to its string form. `category` stays an open string for the same reason.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct LaneSetting {
    /// What [`LaneSettingChange::Config::id`] names.
    pub id: String,
    pub name: String,
    pub category: String,
    pub current: String,
    #[serde(default)]
    pub values: Vec<LaneChoice>,
}

/// How full the session's context is, when the agent says.
#[derive(Debug, Clone, PartialEq, Eq, Default, Serialize, Deserialize)]
pub struct LaneUsage {
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub context_tokens: Option<u64>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub context_window: Option<u64>,
}

/// One change to a session's settings — [`AgentLane::set`]'s argument.
///
/// **Outbound, strict**: no `#[serde(other)]`, and `deny_unknown_fields`, so a
/// change this build cannot name — or one carrying a field it would drop — is
/// refused at decode rather than applied as something else. (`deny_unknown_fields`
/// on an internally tagged enum still accepts the `kind` tag itself; the test
/// suite pins both halves, as it does for [`LaneAnswer`].)
///
/// `Config`, not `Option`, for the reason correction 2 gives for
/// [`LaneAnswer::Choice`] — and craze's own wire kind is `config`.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(tag = "kind", rename_all = "snake_case", deny_unknown_fields)]
pub enum LaneSettingChange {
    /// Move to the model with this [`LaneChoice::id`].
    Model { id: String },
    /// Move to the mode with this [`LaneChoice::id`].
    Mode { id: String },
    /// Set the option [`LaneSetting::id`] to the value [`LaneChoice::id`].
    Config { id: String, value: String },
}

// ---- the traits ----

/// A machine's (or an agent server's) sessions: list them, say what can be
/// created, create one, and open a session-scoped [`AgentLane`] on a row.
///
/// Implemented once per agent: opencode's per agent server URL
/// (`shed_opencode::OpencodeSource`) today, and craze's per machine hub
/// (`shed_craze::CrazeSource`) from plan 025 C7. Every method borrows `&self`, so
/// the futures are `Send` under `async_trait`'s default and a source can be held
/// in an `Arc<dyn AgentSource>` and driven from any task.
#[async_trait::async_trait]
pub trait AgentSource: Send + Sync {
    /// The agent token this source speaks for — `"craze"`, `"opencode"` — the
    /// same vocabulary as [`LaneCapabilities::kind`].
    fn kind(&self) -> &str;

    /// The live session list.
    ///
    /// **Never fails after it returns**: an outage is [`SourceEvent::Offline`]
    /// and the source keeps trying; the subscription ends only when its
    /// subscriber drops or stops it. Every (re)seed is a `Reset … Ready`
    /// bracket carrying [`SourceEvent::Capabilities`] (module doc, "Sources").
    ///
    /// Take the result apart with [`Subscription::into_parts`] and keep BOTH
    /// halves — see [`Subscription`].
    async fn subscribe(&self) -> Result<SourceSubscription, LaneError>;

    /// What a create can start here. An adapter whose source cannot answer
    /// (its capabilities say `create_options: false`) answers
    /// [`LaneError::Failed`] naming why.
    async fn create_options(&self) -> Result<LaneCreateOptions, LaneError>;

    /// Start a session. The answer carries the new row — openable at once
    /// through [`AgentSource::open`] — and what became of its first prompt.
    async fn create(&self, request: LaneCreateRequest) -> Result<LaneCreated, LaneError>;

    /// A session-scoped lane for one row's id ([`LaneSession::id`]).
    ///
    /// Opening is BINDING, not dialling: an adapter does no I/O here, and a
    /// session that does not exist surfaces from the lane's own verbs
    /// ([`LaneError::UnknownSession`], or a `Down{"unknown_session"}`).
    async fn open(&self, session_id: &str) -> Result<Arc<dyn AgentLane>, LaneError>;
}

/// One agent session, normalized.
///
/// Session-SCOPED: the id is bound by [`AgentSource::open`] and no verb takes
/// one. Every method borrows `&self`, so the futures are `Send` under
/// `async_trait`'s default and a lane can be held in an `Arc<dyn AgentLane>` and
/// driven from any task.
///
/// Adapters are expected to be cheap to call and to hold their own transport;
/// none of these methods is a place to build a connection per call. There is no
/// capabilities getter: capabilities are per session and ride the stream
/// ([`LaneEvent::Capabilities`]; module doc, "Two levels").
#[async_trait::async_trait]
pub trait AgentLane: Send + Sync {
    /// The row id the lane was opened with — [`LaneSession::id`]; for craze, the
    /// host's `hostId`.
    fn session_id(&self) -> &str;

    /// The session's row. Both clients call this before
    /// [`AgentLane::subscribe`], so an adapter that would have to wait on a
    /// connection to answer it answers from what it was opened with instead.
    async fn session(&self) -> Result<LaneSession, LaneError>;

    /// A page of transcript — the most recent `limit` rows. `cursor` is
    /// **advisory**: both adapters ignore it (module doc;
    /// [`LaneHistory::cursor`]). `limit` is a request, not a promise —
    /// [`LaneHistory::truncated`] is the answer.
    async fn history(&self, cursor: Option<&str>, limit: u32) -> Result<LaneHistory, LaneError>;

    /// Open a live stream for the session.
    ///
    /// The adapter spawns its own pump and hands back the receiver plus the stop
    /// handle. The stream opens with a [`LaneEvent::Reset`] (or, before any seed
    /// could start, a [`LaneEvent::Stale`] or a terminal [`LaneEvent::Down`]),
    /// reaches steady state at the matching [`LaneEvent::Ready`], and ENDS at a
    /// `Down`. LATER reconnects may be silent — on an adapter whose capabilities
    /// say [`LaneCapabilities::history_cursor`], a cursor resume emits `Stale`
    /// and then a lone `Ready` of the same generation, no `Reset` (module doc,
    /// correction 1) — so a client must not count brackets to count connections.
    /// `cursor` is a resume hint for the FIRST connect, honoured under the same
    /// flag. Two subscriptions to one session are two independent streams (and,
    /// on opencode, two connections).
    ///
    /// Take the result apart with [`Subscription::into_parts`] and keep BOTH
    /// halves — `…await?.rx` compiles, drops the stop handle, and hands back a
    /// receiver that will never yield a frame. See [`Subscription`].
    async fn subscribe(&self, cursor: Option<String>) -> Result<LaneSubscription, LaneError>;

    /// Send `text` to the session. See [`SendMode`] for what each mode promises.
    async fn send(&self, text: &str, mode: SendMode) -> Result<(), LaneError>;

    /// Stop the turn in flight.
    ///
    /// An adapter whose agent REFUSES a cancel with nothing to cancel surfaces
    /// that as [`LaneError::NotAccepting`] rather than swallowing it as
    /// `Ok(())` (module doc, correction 8) — a hidden refusal is
    /// indistinguishable from a cancel that worked. Clients gate the affordance
    /// on [`crate::rc::RcActivity::Working`] so the refusal is rare and
    /// explicable when it happens (the turn ended between the render and the
    /// tap).
    async fn cancel(&self) -> Result<(), LaneError>;

    /// Everything open on this session — INCLUDING its descendants' approvals,
    /// which block the same agent even though their transcript rows do not
    /// surface.
    async fn approvals(&self) -> Result<Vec<LaneApproval>, LaneError>;

    /// Answer one approval. A second answer to the same approval is refused
    /// with [`LaneError::AlreadySubmitted`] or [`LaneError::AlreadyResolved`].
    ///
    /// A [`LaneAnswer::Choice`] naming an id the approval did not offer, and a
    /// [`LaneAnswer::Permission`] whose decision matches no offered
    /// [`LaneApprovalOption::kind`] — or matches SEVERAL of them — are all
    /// [`LaneError::BadRequest`] — an adapter never picks the nearest option,
    /// and never breaks a tie the human is the only one who can break.
    async fn answer(&self, approval_id: &str, answer: LaneAnswer) -> Result<(), LaneError>;

    /// The session's settings, now. A session whose capabilities say
    /// `settings: false` answers [`LaneSettings::default`]. (The stream's
    /// [`LaneEvent::Settings`] is how a client stays current; this is the
    /// one-shot read.)
    async fn settings(&self) -> Result<LaneSettings, LaneError>;

    /// Change one setting. **No default body** (module doc, "Two levels"): an
    /// adapter that cannot answers [`LaneError::Failed`] naming itself, and its
    /// capabilities say `settings: false` so no client offers it.
    async fn set(&self, change: LaneSettingChange) -> Result<(), LaneError>;

    /// End the session. **No default body**: an adapter that cannot answers
    /// [`LaneError::Failed`] naming itself, and its capabilities say
    /// `stop: false`. Where it can, the stream's terminal `Down` is the stop's
    /// completion; this call's `Ok` is a receipt.
    async fn stop(&self) -> Result<(), LaneError>;
}

#[cfg(test)]
mod tests;
