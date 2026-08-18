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
//! Lock-poisoning posture (H7 review): every rc_hub mutex recovers via
//! `PoisonError::into_inner` rather than propagating the poison — Go's
//! `defer mu.Unlock()` runs during a panic's unwind, so a panicking fold
//! leaves the Go hub usable, and one cross-thread panic must not cascade into
//! a permanently dead watcher here either. (The same partially-updated-state
//! risk exists in both semantics.)
//!
//! Landed so far (plan 010 H4/H5 — the pure cores, no HTTP dependency yet):
//!
//! - [`messages`] — the per-session feed ring + wire vocabulary
//!   (`hub_messages.go`) and the `activity.go` text-hygiene helpers.
//! - [`stability`] — the pane-stability engine + normalizers (`stability.go`).
//! - [`watch`] — the watcher freshness rule, the `mergedActivity` precedence
//!   merge, the correlation helpers, the fold contracts, and the tmux env
//!   seams (`watch.go`'s pure parts; the `fileWatcher`/`fsNudger` transports
//!   follow in H7).
//! - [`tail`] — the resilient JSONL line tailer (`watch_tail.go`).
//! - [`watch_claude`] / [`watch_codex`] — the claude/codex folds + their
//!   JSONL correlation (`watch_claude.go`, `watch_codex.go`).
//! - [`watch_cursor`] — the cursor hook-event fold + transcript
//!   restart-backfill (`watch_cursor.go`'s pure half; the push-fed watcher
//!   wrapper follows in H7).
//! - [`watch_opencode`] — the opencode pure fold: approval seed halves,
//!   reopen rule, tombstones, question rows (`watch_opencode.go`; its
//!   SSE/REST transport follows in H8).
//! - [`watch_opencode_transport`] — the opencode SSE/REST transport + verb
//!   lane (H8): the run-thread connect→pin→seed→live state machine with its
//!   generation barriers, the approvals claim FSM, the hand-rolled loopback
//!   HTTP client (the plan's sanctioned TcpStream fallback — close() must
//!   unblock a blocked read), and the private SSE scanner.
//! - Transports I (H7): the [`watch`] module additionally carries the
//!   `SessionWatcher` contract + `FileWatcher` + the `notify`-backed
//!   `FsNudger`; [`watch_cursor`] the push-fed `CursorWatcher`; [`ingest`]
//!   the pre-watcher queue half of `hub_ingest.go` (the HTTP handler follows
//!   in H10).

pub mod ingest;
pub mod messages;
pub mod stability;
pub mod tail;
pub mod watch;
pub mod watch_claude;
pub mod watch_codex;
pub mod watch_cursor;
pub mod watch_opencode;
pub mod watch_opencode_transport;
