//! **shed-gx** — the gx adapter for the agent-lane contract.
//!
//! [`shed_core::lane::AgentLane`] normalizes "a coding agent with sessions, a
//! transcript and approvals"; this crate is its `gx` implementation, talking to
//! a gx leader's `gx-remote-api` lane over `/v1`. It is the SECOND adapter, and
//! the twelve corrections in [`shed_core::lane`]'s module doc are what writing
//! it forced onto a contract that had only ever been validated against one
//! implementation.
//!
//! ```text
//! GxClient  — reqwest (bearer, sensitive), ensure_pinned, the verbs, AgentLane
//!   ├─ discovery.rs — PROBE_SCRIPT, parse_probe, GxCredentialSource, GxToken
//!   ├─ transport.rs — GxTransport, FixedDial
//!   ├─ fold.rs      — pure: envelopes → rows + activity; approvals → LaneApproval
//!   ├─ (ring / backoff / feed from shed_core::lane)
//!   └─ watcher.rs   — the pump (plan 017 C3)
//! ```
//!
//! # What is different about gx, in one place
//!
//! - **It needs a credential**, and the contract deliberately has no seat for
//!   one. [`discovery`] is the seam: the client supplies a
//!   [`discovery::GxCredentialSource`], and the token — read from the host that
//!   runs gx — never appears in a `LaneEvent`, an error, a log line or a
//!   `Debug`.
//! - **It has two URLs.** The REPORTED one (roost's `gx.remote`) is what a
//!   discovery record is matched against; the DIAL one is where HTTP goes, and
//!   over SSH they differ. They are never conflated, and
//!   [`transport::GxTransport::dial`] is the hook that lets a client repair a
//!   forward without the contract growing an event for it.
//! - **It has a resumable cursor**, so a reconnect can be SILENT: no `Reset`,
//!   same generation, the client's view untouched. That is bounded (three
//!   attempts in thirty seconds) and it is why `history_cursor` exists on
//!   [`shed_core::lane::LaneCapabilities`].
//! - **Its permission options are request-supplied and opaque.** An `optionId`
//!   is whatever the agent called it; the semantics ride
//!   [`shed_core::lane::LaneApprovalOption::kind`]. Nothing here parses an id.
//! - **Its question answers are keyed by the question's TEXT**, which is what
//!   its own TUI files them under.
//!
//! # What C2 does not have yet
//!
//! The watcher (`watcher.rs`), the fake's SSE stream and resume rules, the live
//! smoke and the fold golden are plan 017 C3.
//! [`shed_core::lane::AgentLane::subscribe`] answers
//! [`shed_core::lane::LaneError::Failed`] until then — loudly, so a missing
//! implementation cannot be mistaken for a lane that is merely unreachable.

pub mod client;
pub mod discovery;
pub mod fold;
pub mod transport;

#[cfg(any(test, feature = "test-support"))]
pub mod testing;

pub use client::{GxClient, GxTimings};
/// The local reader's assembly (C4's `ReachKind::Local` path). Unix-only: the
/// mode and owner its checks are about do not exist elsewhere.
#[cfg(unix)]
pub use discovery::{local_discovery, read_token_file, token_file_refusal};
pub use discovery::{
    parse_probe, GxCredentialSource, GxDiscovery, GxRecord, GxToken, Probe, ProbeError,
    StaticCredentials, PROBE_SCRIPT,
};
pub use fold::{EventId, GxEnvelope, GxFold};
pub use transport::{FixedDial, GxTransport};
