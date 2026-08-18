//! # rc_hub — the RC activity hub (Go `serve`) ported into the broker core
//!
//! A file-per-file port of the Go machine hub (`internal/ext/rc/hub*.go` +
//! `watch*.go` + `stability.go`) so every function reads against its original —
//! the plan-009 auditability discipline. The **normative wire** is
//! `docs/extensions/rc-helper.md` § "The RC activity hub"; the file→module map
//! and the invariants the port must preserve live in
//! `docs/discovery/rc-hub-port.md`; the Go↔Rust differential harness is
//! `tests/rc-parity/` (hub family).
//!
//! The Go hub stays alive for shed guests (`shed-ext-rc`, baked into images)
//! and serves as the differential oracle; this port is what retires the
//! native-machine binary (`shed-machine-rc`) once the `shed-host-agent` daemon
//! hosts it at parity.
//!
//! Landed so far (plan 010 H4 — the pure cores, no HTTP dependency yet):
//!
//! - [`messages`] — the per-session feed ring + wire vocabulary
//!   (`hub_messages.go`).
//! - [`stability`] — the pane-stability engine + normalizers (`stability.go`).
//! - [`watch`] — the watcher freshness rule, the `mergedActivity` precedence
//!   merge, the correlation helpers, and the tmux env seams (`watch.go`'s pure
//!   parts; the `fileWatcher`/`fsNudger` transports follow in H7).
//! - [`tail`] — the resilient JSONL line tailer (`watch_tail.go`).

pub mod messages;
pub mod stability;
pub mod tail;
pub mod watch;
