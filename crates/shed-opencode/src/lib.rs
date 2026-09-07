//! **shed-opencode** — the opencode adapter for the agent-lane contract.
//!
//! [`shed_core::lane::AgentLane`] normalizes "a coding agent with sessions, a
//! transcript and approvals"; this crate is its opencode implementation, talking
//! to an opencode server's local HTTP API (the one the TUI already runs and,
//! under the Roost Pivot, reports the URL of).
//!
//! What lands when:
//!
//! - **C2 (here)** — [`fold`]: the pure fold. It turns opencode's `/event`
//!   envelope stream into an activity verdict, [`shed_core::rc::RcFeedMessage`]
//!   transcript rows and [`shed_core::lane::LaneApproval`] state. No I/O.
//! - **C3** — the HTTP client (two reqwest clients: a 5 s REST one and a
//!   timeout-free streaming one), the bounded ring that owns `seq`, the
//!   reconnecting watcher whose generations bracket a seed with
//!   `Reset` … `Ready`, the [`shed_core::lane::AgentLane`] impl, and the
//!   `FakeOpencode` test double.
//!
//! The fold is a **port of the rc hub's** `OpencodeFold`
//! (`shed-broker::rc_hub::watch_opencode`), pinned against it by
//! `fixtures/opencode_turn.golden.json` so the move cost no behavior. The
//! helpers it needs are COPIED into [`helpers`] rather than linked — the hub's
//! `watch` module imports the RC engine's tmux layer, which has no business in
//! an HTTP adapter. See the module docs for the three deliberate differences.

mod helpers;

pub mod fold;

pub use fold::OpencodeFold;
