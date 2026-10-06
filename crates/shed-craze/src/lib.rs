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
//! CrazeSource  — AgentSource: the live roster, createOptions, create, open
//! CrazeLane    — AgentLane: one session, by its row's hostId
//!   ├─ watcher.rs  — the subscription's pump: seed, silent resume, reseed, Down
//!   ├─ fold.rs     — craze's events and snapshots → append-only rows, activity,
//!   │                the approval book (craze's own wordings, ported)
//!   ├─ segment.rs  — the streak segmenter and its flush clock (ported from shed-gx)
//!   ├─ settings.rs — the session's settings: catalogs, sections, craze's order
//!   ├─ dial.rs   — the seam: one fresh duplex to `craze bridge --hub` per connection
//!   │              (ProcessDial: a local /bin/sh; TcpDial: the phone's loopback port),
//!   │              and the classification of a dial that never reached a hub
//!   ├─ conn.rs   — one NDJSON connection: framing at craze's limits from the client's
//!   │              side, id demux, notifications, the bounded preamble, both hellos
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
//! dial: the roster's, every `createOptions`, every create, and every open
//! lane's (one per lane, which its verbs share).
//!
//! **The lane** ([`CrazeLane`], plan 025 §3.3.4–3.3.6, C8) is opened by hostId
//! (P11) and reaches its host through the hub's splice; its watcher seeds from
//! an attach's snapshot, resumes SILENTLY from its cursor whenever the host
//! honours it (P12), and reseeds when it does not. The settings it reads are
//! [`settings`]'s — craze's model order and the options' order, computed here
//! once — and it changes one with `session.set` (the settings milestone, C11):
//! a config change bound to the model it was chosen for ([`setting_for`]).
//!
//! Not FFI-exported; it builds for `aarch64-linux-android` because shed-mobile
//! links it. `testing` (behind `test-support`) is craze's own hermetic recipe
//! as a harness — the real hub, `craze-fake-host`, `craze-fake-agent` — plus
//! the dial hooks and a scripted hub for the unit cells.

pub mod conn;
pub mod dial;
pub mod errors;
pub mod fold;
pub mod lane;
pub mod segment;
pub mod settings;
pub mod source;
pub mod watcher;
pub mod wire;

#[cfg(any(test, feature = "test-support"))]
pub mod testing;

pub use dial::{
    classify_pre_hello, classify_probe, connect_hub, probe_process, CrazeDial, CrazeStream,
    DialError, EnvPolicy, ExitWatch, Probe, ProcessDial, StderrTail, TcpDial,
};
pub use errors::{is_outcome_unknown, lane_error};
pub use lane::{answer_body, lane_capabilities, setting_for, CrazeLane, LaneTimings};
pub use source::{
    lane_session, new_request_id, source_capabilities, valid_request_id, CrazeSource, Timings, KIND,
};
