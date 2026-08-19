//! The **RC one-shot engine** — the I/O half of the Remote-Control engine ported
//! from the Go guest binary (`internal/ext/rc/`), sitting on top of the pure
//! registry kernel in [`shed_core::rc_agents`] (plan 009 C1).
//!
//! ```text
//!   shed_core::rc_agents   pure data + classifiers + BuildEnvArgs + parse_session
//!   shed-rc-engine         tmux process I/O, the create order, the wait poller,
//!                          plan files, port allocation   <- YOU ARE HERE
//!   sx (crates/sx)         argv parsing, stdin framing, JSON stdout, exit codes
//!   shed-broker rc_hub     the resident activity hub (plan 010) — the engine's
//!                          second consumer, and why it graduated out of
//!                          shed-app's `rc` feature into this crate (a
//!                          broker→shed-app dependency would cycle through
//!                          shed-app's `broker` feature)
//! ```
//!
//! shed-app re-exports this crate as `shed_app::rc_engine` under its `rc`
//! feature, so sx and the desktop consume the same paths as before graduation.
//!
//! **This crate is SYNCHRONOUS on purpose.** The engine is a short-lived,
//! strictly-ordered sequence of `tmux` invocations with sleeps between them; the
//! Go original is the same shape, and a differential harness compares the two
//! wire-for-wire. Async would buy nothing (there is no concurrency to exploit in
//! a one-shot verb — the capability probes that DO fan out use threads) and
//! would cost the property that makes the port auditable: every function here
//! can be read against its Go original line by line. It therefore takes **no
//! tokio dependency at all**; shed-app's async `rc` module (`RcService`, the
//! thin SSH client OF this engine running on a remote) is a different layer
//! entirely and stays in shed-app.
//!
//! ## What is here
//!
//! [`tmux`] (the runner seam + verb helpers, `tmux.go`), [`ops`] (create / list /
//! probe / prompt / kill / accept-trust + the `--wait` poller, `ops.go`),
//! [`plan`] (`plan.go`), [`netutil`] (`netutil.go`), [`text`] (the two prompt
//! helpers from `rc.go` that C1 did not carry over), the create-time
//! **preseeds** ([`trust`] = `trust.go`, [`preseed_cursor`] =
//! `preseed_cursor.go`, dispatched per kind by [`preseed`] the way
//! `AgentSpec.Preseed` is), the byte-exact Go-`encoding/json` writer they rewrite
//! their files with ([`go_json`]), and **capability discovery**
//! ([`capabilities`] = `capabilities.go`).
//!
//! Never here: the activity **hub** (`serve`). Its Rust home is shed-broker's
//! `rc_hub` (plan 010), which CONSUMES this crate; the engine only carries the
//! best-effort *ensure* hook ([`ops::Engine::with_ensure_hub`]) and honors the
//! same [`ENV_NO_HUB`] kill-switch the Go side gained in 009 C2, so a hermetic
//! harness can neutralize the side effect symmetrically on both implementations.
//!
//! ## Port fidelity notes (read before changing anything)
//!
//! - **Order of operations is the contract**, not just the outcome: the create
//!   flow's 15 steps, the poller's per-tick precedence (missing-session → bypass
//!   → classify → trust → break), and "the plan file is written AFTER tmux
//!   accepts the session name" are all observable to a differential harness.
//! - Every function names its Go original as `file.go:line`. When the Go side
//!   moves, re-anchor the comment rather than dropping it.
//! - Error **classes** (and their `invalid arguments: ` / `rc session …` message
//!   prefixes) are wire contract — see [`ops::EngineError`].

pub mod capabilities;
pub mod clock;
pub mod go_json;
pub mod netutil;
pub mod ops;
pub mod plan;
pub mod preseed;
pub mod preseed_cursor;
pub mod text;
pub mod tmux;
pub mod trust;

/// The engine's test doubles. Compiled only for this crate's own tests or when a
/// consumer enables the `test-support` feature from its `[dev-dependencies]`
/// (`sx`'s dispatch tests do) — never in a production build.
#[cfg(any(test, feature = "test-support"))]
pub mod fake;

pub use capabilities::{
    build_capabilities, real_agent_probe, real_installed_probe, AgentProbe, InstalledProbe,
    CAPABILITY_FEATURES, CAPABILITY_VERSION, PROBE_BUDGET,
};
pub use clock::{system_clock, Clock, ClockRef, SystemClock};
pub use netutil::free_loopback_port;
pub use ops::{
    capture_pane_checked, capture_visible_pane_checked, real_bin_probe, CreateOptions, Engine,
    EngineError, GetEnv, PromptOptions, DEFAULT_CREATED_BY, DEFAULT_POLL_EVERY,
    DEFAULT_WAIT_TIMEOUT, ENV_NO_HUB, PROMPT_DELIVER_SETTLE,
};
pub use plan::{compose_plan_kickoff, plan_from_bytes, plan_path, PLAN_MAX_BYTES};
pub use preseed::{dispatch as preseed_for_kind, PreseedError};
pub use text::{has_unsafe_prompt_chars, normalize_newlines};
pub use tmux::{
    is_duplicate_session, is_missing_session, ExecRunner, Tmux, TmuxResult, TmuxRunner,
    DEFAULT_SEND_LINE_SETTLE, PROMPT_BUFFER,
};
