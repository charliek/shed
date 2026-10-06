//! **shed-craze** — the craze adapter for the agent-lane contract (plan 025,
//! D2/D3/D10).
//!
//! craze is the provider abstraction (D2): cursor, grok, gx and native
//! sessions all live behind one per-machine **hub**, which lists every session
//! (its roster), says what a create can start, starts one, and splices a
//! client through to a session's host. `shed_core::lane` splits an agent into a
//! machine-level [`shed_core::lane::AgentSource`] and a session-scoped
//! [`shed_core::lane::AgentLane`] (D3) precisely so a hub fits: this crate's
//! [`CrazeSource`] is one machine's hub.
//!
//! ```text
//! CrazeSource  — AgentSource: the live roster, createOptions, create (and, C8, open)
//!   ├─ dial.rs   — the seam: one fresh duplex to `craze bridge --hub` per connection
//!   │              (ProcessDial: a local /bin/sh; TcpDial: the phone's loopback port),
//!   │              and the classification of a dial that never reached a hub
//!   ├─ conn.rs   — one NDJSON connection: framing at craze's limits from the client's
//!   │              side, id demux, notifications, the bounded preamble, the hub hello
//!   ├─ wire.rs   — protocol 1's envelope and the params/results this crate composes
//!   └─ errors.rs — craze's published code → LaneError table, verbatim (+ P14's one row)
//! ```
//!
//! **The transport is the client's** (P8): a dial runs `craze bridge --hub`
//! wherever the client can reach — a local `/bin/sh -c '<ladder>'` child on the
//! desktop's own machine, an `SshExec` duplex to a remote one (C9), a loopback
//! port on the phone — and this crate speaks protocol 1 over whatever duplex it
//! is handed. The ladder itself is `shed_core::craze`'s (craze's published one,
//! verbatim, plus an exec-only PATH; plan 025 §3.4). Every connection is its own
//! dial: the roster's, every `createOptions`, every create, and (C8) every open
//! lane's.
//!
//! **The lane is C8.** This crate lists and creates craze sessions;
//! [`CrazeSource`]'s `open` answers `Failed` until the session-scoped lane, its
//! watcher and its fold arrive (plan 025 §3.3.4–3.3.6).
//!
//! Not FFI-exported; it builds for `aarch64-linux-android` because shed-mobile
//! links it. `testing` (behind `test-support`) is craze's own hermetic recipe
//! as a harness — the real hub, `craze-fake-host`, `craze-fake-agent` — plus
//! the dial hooks and a scripted hub for the unit cells.

pub mod conn;
pub mod dial;
pub mod errors;
pub mod source;
pub mod wire;

#[cfg(any(test, feature = "test-support"))]
pub mod testing;

pub use dial::{
    classify_pre_hello, classify_probe, connect_hub, probe_process, CrazeDial, CrazeStream,
    DialError, EnvPolicy, ExitWatch, Probe, ProcessDial, StderrTail, TcpDial,
};
pub use errors::{is_outcome_unknown, lane_error};
pub use source::{
    lane_session, new_request_id, source_capabilities, valid_request_id, CrazeSource, Timings, KIND,
};
