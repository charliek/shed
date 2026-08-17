//! The **RC one-shot engine** — the I/O half of the Remote-Control engine ported
//! from the Go guest binary (`internal/ext/rc/`), sitting on top of the pure
//! registry kernel in [`shed_core::rc_agents`] (plan 009 C1).
//!
//! ```text
//!   shed_core::rc_agents   pure data + classifiers + BuildEnvArgs + parse_session
//!   shed_app::rc_engine    tmux process I/O, the create order, the wait poller,
//!                          plan files, port allocation   <- YOU ARE HERE
//!   sx (crates/sx, C4)     argv parsing, stdin framing, JSON stdout, exit codes
//! ```
//!
//! **This module is SYNCHRONOUS on purpose.** The engine is a short-lived,
//! strictly-ordered sequence of `tmux` invocations with sleeps between them; the
//! Go original is the same shape, and a differential harness compares the two
//! wire-for-wire. Async would buy nothing (there is no concurrency to exploit in
//! a one-shot verb — the capability probes that DO fan out land at C5 and use
//! threads) and would cost the property that makes the port auditable: every
//! function here can be read against its Go original line by line. It therefore
//! adds **no tokio features and no new dependencies**; the existing async
//! [`crate::rc`] client (`RcService`, the thin SSH client OF this engine running
//! on a remote) is a different layer entirely and is untouched.
//!
//! ## What is here (C3) and what is not
//!
//! Here: [`tmux`] (the runner seam + verb helpers, `tmux.go`), [`ops`] (create /
//! list / probe / prompt / kill / accept-trust + the `--wait` poller, `ops.go`),
//! [`plan`] (`plan.go`), [`netutil`] (`netutil.go`), and [`text`] (the two prompt
//! helpers from `rc.go` that C1 did not carry over).
//!
//! Not here yet: the claude/cursor **preseeds** (`trust.go`,
//! `preseed_cursor.go`) and **capabilities** probing (`capabilities.go`) — C5.
//! [`ops::Engine::with_preseed`] already carries the seam so create's ordering
//! (preseed strictly BEFORE `tmux new-session`) is pinned now rather than
//! retrofitted later.
//!
//! Never here: the activity **hub** (`serve`). It stays the Go binary this block
//! (plan 009 §0); the engine only carries the best-effort *ensure* hook
//! ([`ops::Engine::with_ensure_hub`]) and honors the same [`ENV_NO_HUB`]
//! kill-switch the Go side gained in C2, so a hermetic harness can neutralize the
//! side effect symmetrically on both implementations.
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

pub mod netutil;
pub mod ops;
pub mod plan;
pub mod text;
pub mod tmux;

/// The engine's test doubles. Compiled only for this crate's own tests or when a
/// consumer enables the `test-support` feature from its `[dev-dependencies]`
/// (`sx`'s dispatch tests do) — never in a production build.
#[cfg(any(test, feature = "test-support"))]
pub mod fake;

pub use netutil::free_loopback_port;
pub use ops::{
    real_bin_probe, CreateOptions, Engine, EngineError, GetEnv, PromptOptions, DEFAULT_CREATED_BY,
    DEFAULT_POLL_EVERY, DEFAULT_WAIT_TIMEOUT, ENV_NO_HUB, PROMPT_DELIVER_SETTLE,
};
pub use plan::{compose_plan_kickoff, plan_from_bytes, plan_path, PLAN_MAX_BYTES};
pub use text::{has_unsafe_prompt_chars, normalize_newlines};
pub use tmux::{
    is_duplicate_session, is_missing_session, ExecRunner, Tmux, TmuxResult, TmuxRunner,
    DEFAULT_SEND_LINE_SETTLE, PROMPT_BUFFER,
};
