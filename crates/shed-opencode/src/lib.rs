//! **shed-opencode** — the opencode adapter for the agent-lane contract.
//!
//! [`shed_core::lane::AgentLane`] normalizes "a coding agent with sessions, a
//! transcript and approvals"; this crate is its opencode implementation, talking
//! to an opencode server's local HTTP API (the one the TUI already runs and,
//! under the Roost Pivot, reports the URL of).
//!
//! ```text
//! OpencodeClient  — the transport (two reqwest clients), the verbs, AgentLane
//!   ├─ fold.rs    — the pure fold: envelopes → activity + rows + approvals
//!   ├─ ring.rs    — the bounded ring that OWNS `seq`
//!   └─ watcher.rs — the reconnecting pump: generations, Reset … Ready
//! ```
//!
//! # Why the LEGACY `/event` feed
//!
//! The live feed is opencode's v1 `GET /event`, not its v2 per-session stream,
//! and that is a decision rather than an oversight:
//!
//! - `/event`'s vocabulary is what the proven fold consumes. [`fold`] is a port
//!   of the rc hub's `OpencodeFold`, pinned against it by
//!   `fixtures/opencode_turn.golden.json`; re-teaching it a second vocabulary
//!   would have thrown that pin away.
//! - The v2 runtime stream at 1.18.29 was observed serving ~59 event types and
//!   **omitting `session.idle`**, while the OpenAPI's `V2Event` union lists it.
//!   Spec and runtime disagree; the fold's activity verdict turns on that exact
//!   event.
//! - v2's durable per-session routes (`GET /api/session/{id}/event?after=`,
//!   `…/history?after=`) emit `SessionDurableEvent` — the `session.next.*`
//!   family, a different vocabulary again. There is no v1 per-session `?after=`
//!   route, which is also why this adapter advertises `history_cursor: false`
//!   and refolds from the top instead of resuming.
//!
//! Resuming through the durable stream is the natural follow-up once the fold
//! learns `session.next.*`.
//!
//! # `SendMode::Queue` on opencode
//!
//! `prompt_async` on a `Working` session is ACCEPTED (204) and opencode's runner
//! delivers it per its own rules — it joins the running runner rather than
//! appending to a strict queue. The contract promises acceptance and ordering,
//! nothing about where in a backlog the text sits. [`shed_core::lane::SendMode`]
//! says the same thing from the contract's side.

mod helpers;

pub mod client;
pub mod fold;
pub mod ring;
pub mod watcher;

#[cfg(any(test, feature = "test-support"))]
pub mod testing;

pub use client::{BasicAuth, OpencodeClient};
pub use fold::OpencodeFold;
pub use ring::MessageRing;
