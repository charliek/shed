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
//!   └─ watcher.rs   — the pump: seed, bounded silent resume, reconcile, Down
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
//! # Where to start reading
//!
//! [`watcher`] is the interesting half. It is the only place the two-URL rule,
//! the credential pin, the fold's cursor and the contract's reconnect bracket
//! all meet, and its module doc is the one-page description of what a gx
//! subscription actually does.

pub mod client;
pub mod discovery;
pub mod fold;
pub mod transport;
pub mod watcher;

#[cfg(any(test, feature = "test-support"))]
pub mod testing;

pub use client::{GxClient, GxTimings};
pub use discovery::{
    gx_home, parse_probe, records_for, redact_hex64, GxCredentialSource, GxDiscovery, GxRecord,
    GxToken, Probe, ProbeError, StaticCredentials, PROBE_SCRIPT,
};
/// The local reader's assembly (C4's `ReachKind::Local` path). Unix-only: the
/// mode and owner its checks are about do not exist elsewhere.
#[cfg(unix)]
pub use discovery::{local_discovery, read_token_file, token_file_refusal};
pub use fold::{EventId, GxEnvelope, GxFold};
pub use transport::{FixedDial, GxTransport};
// The watcher's buffer bounds are deliberately NOT re-exported here. They are
// `pub` in their module so the doc links resolve, but they are the pump's
// internal sizing — not contract — and hoisting them to the crate root is how a
// consumer comes to read them as one. (`shed-opencode` draws the same line.)
