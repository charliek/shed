//! Putting a `roost-session` on a host you can reach — as a **sans-IO state
//! machine** two very different clients drive over two very different
//! transports (plan 019 §3.4, S5).
//!
//! roost already owns every hard part of this: the scripts, the quoting rules,
//! the NUL-delimited parsers that refuse an answer their own script could not
//! have produced, the install order whose rollback promise is a property of the
//! order itself. All of that is [`roost_ipc::bootstrap`], and this module
//! **composes** it. What it adds is the one thing roost's own runtime cannot
//! give shed:
//!
//! > roost's [`BootstrapJob`] owns a local `ssh` ControlMaster. A phone has no
//! > `ssh` binary. So the *choreography* — which script runs next, what its
//! > answer means, what has to be undone when it goes wrong — is lifted out of
//! > the runtime and into a machine that performs no I/O at all: it yields a
//! > [`Step`] and consumes an [`Outcome`], and the caller owns every socket,
//! > subprocess and timer.
//!
//! [`BootstrapJob`]: roost_ipc::bootstrap::BootstrapJob
//!
//! ## One choreography, two transports
//!
//! | step | desktop (Tauri/Swift) | mobile (Flutter) |
//! |---|---|---|
//! | [`Step::Exec`] | an `ssh` subprocess on a private ControlMaster | `dartssh2` |
//! | [`Step::Call`] | the client's roost [`Conn`] over the ssh bridge | the same `Conn` over a loopback port |
//! | [`Step::Hooks`] | [`hooks::wire_agent_hooks`] on that `Conn` | the same function, on the Rust side |
//!
//! [`Conn`]: crate::roost::Conn
//!
//! That table is the whole reason this is sans-IO. Two transports that agree
//! about *what to run next* because they share one implementation of it is a
//! property; two runtimes that agree today is a coincidence.
//!
//! ## What the machine does NOT reach for
//!
//! * **`BootstrapJob`** — see above.
//! * **`resolve_source`** — roost's source ladder gates its sibling rung on
//!   roost's own exact triple (`app_version` + protocol + `libghostty_build`),
//!   which shed cannot satisfy or even compute, and the descriptor it resolves
//!   to (`ResolvedSource::verified`) is private to roost. shed composes the
//!   *pure* half of roost's bootstrap and brings its own ladder ([`source`],
//!   plan 019 C5), handing the machine a finished [`SourceHandle`].
//! * **`identity_matches` / `classify_probe`** — the exact-triple rule. See the
//!   next section; the *shape* of roost's classification is reproduced in
//!   [`plan::classify_candidates`], the *rule* is shed's.
//!
//! ## shed's compatibility rule is the protocol number, and only that
//!
//! roost's installer requires a remote binary to equal the client's build
//! exactly — `app_version`, `session_protocol` and `libghostty_build`. shed
//! **cannot** apply that rule and must not pretend to:
//!
//! * shed does not build roost. It links `roost-ipc` at a pinned rev and knows
//!   one number from it, [`SESSION_PROTOCOL_VERSION`].
//! * shed never negotiates a ghostty snapshot — it is not a terminal. A
//!   `libghostty_build` is meaningless to it and unknowable in advance for a
//!   release asset it has not downloaded yet.
//!
//! So the rule here is `session_protocol == SESSION_PROTOCOL_VERSION`, applied
//! in **all three** places roost applies its triple: the probe's
//! classification, the staged verify before the commit, and the post-commit /
//! post-start identify. One rule, three gates, no place where a binary is good
//! enough to install but not good enough to keep.
//!
//! **The accepted second-order effect, recorded deliberately:** a roost UI that
//! later connects to a session shed installed applies roost's *own* exact-triple
//! rule, and may well offer to reinstall it. That is not a bug in either side —
//! it is two clients with different needs answering different questions, and the
//! roost UI's answer is the conservative one. shed's install is still the right
//! thing to have done: a protocol-4 session is a session shed can read, and
//! shed's whole claim on the host is reading it.
//!
//! ## The rollback promise, and exactly how far it reaches
//!
//! It is roost's, because it is a property of roost's *order* and this machine
//! keeps that order:
//!
//! * A failure at **prepare** wrote nothing. The host is untouched.
//! * A failure at **stream** or **staged verify** removes the temporary and
//!   leaves the incumbent *alone* — it is never renamed, because nothing has
//!   moved it. Nothing replaces the destination until the staged bytes have
//!   identified themselves, which is what makes "the existing install is
//!   unchanged" a claim rather than a hope.
//! * A failure at **commit** removes the temporary and puts the incumbent back
//!   from `<dest>.bak.<pid>` if the commit's first `mv` had already landed.
//! * A failure at the **post-commit identify** restores the incumbent when
//!   there was one, and says which of the two happened —
//!   [`BootstrapFailure::restored`] — because "your old roost-session is back"
//!   and "the new one is still there, go look at it" are different instructions.
//!
//! **And then it stops.** Once the post-commit identify has passed, the backup
//! is discarded, and from that instant there is nothing left to roll back to. A
//! **Start** or post-start failure after that point leaves the new binary in
//! place, and [`copy::start_failed`] says so in as many words. The consent card
//! promises exactly this much and not one sentence more.
//!
//! ### Why a rollback is gated on this attempt's own commit
//!
//! Every sentence above is about *undoing this attempt*. The rollback script
//! cannot express that: it restores `<dest>.bak.<pid>`, and that name is
//! derived from the far side's `$$` and nothing else.
//!
//! Three facts compose into a corruption path, and the machine is the only
//! place it can be closed:
//!
//! 1. A `.bak` is not always cleaned up. A discard that fails, or a connection
//!    that drops after the commit, leaves one behind.
//! 2. [`prepare_script`](roost_ipc::bootstrap::prepare_script) sweeps `.tmp.*`
//!    and **never** `.bak.*` — deliberately, since a backup is the thing that
//!    makes the install undoable.
//! 3. Pids are reused.
//!
//! So a later attempt whose remote `$$` collides with an earlier one finds a
//! stranger's stale binary at exactly the path its own rollback restores from.
//! Running that rollback on a failure that happened *before* the commit — a
//! dead stream, a staged verify that refused — would rename those stale bytes
//! over a perfectly good incumbent, and then report that the existing install
//! is unchanged. The bug is shed's, not roost's: roost supplies the script,
//! shed decides when to call it, and roost's own runtime only ever calls it
//! after its commit.
//!
//! [`InstallMachine`] therefore tracks whether **this attempt ran the commit
//! script** and rolls back only then; every earlier failure runs the cleanup
//! alone. The gate is the commit *step*, not the commit's first `mv`, because
//! no answer the far side gives distinguishes the two — which leaves one
//! residual, stated rather than papered over: a commit that failed at its own
//! pre-rename guards, on a host that already had a stale `.bak.<reused-pid>`,
//! still rolls that stale file forward. Closing that would take a second
//! observation of the far side, which is a script shed would have to write
//! itself.
//!
//! ### The undo steps have outcomes, and the copy reads them
//!
//! A rollback, a cleanup and a discard are remote execs like any other, and
//! each of them can fail. Each one also *is* a clause in the sentence the user
//! reads — "any previous install there was put back", "the staged file was
//! removed", the whole premise of a discarded backup. So none of them is
//! ignored: [`copy::commit_not_put_back`] and [`copy::post_commit_not_put_back`]
//! replace the sentence outright when the incumbent could not be restored (and
//! **name the `.bak` path it is stranded at**, which is the only thing that
//! makes the state recoverable by hand), [`copy::cleanup_failed`] corrects the
//! removal claim, and [`copy::discard_failed`] rides out on the success value
//! as [`Installed::backup_warning`].
//!
//! ## The post-probe race, narrowed and not closed
//!
//! A probe reads a host; an install writes to it; time passes in between, and
//! the two clients plan 019 §3.6 describes — a desktop and a phone, both
//! watching the same shed — are the *normal* case rather than a pathological
//! one. Two races follow from that, and they are handled differently because
//! only one of them can be handled at all.
//!
//! **A session starting under the binary shed is replacing.** Both the consent
//! probe and the re-probe can see a protocol-2 binary with nothing serving,
//! which is [`Plan::Update`]; somebody can start that session a millisecond
//! later. So the machine asks `session.identify` **once more, immediately
//! before the commit** ([`machines`]'s `PreCommitIdentify`) and refuses if a
//! session appeared — with pin P6's copy when it is one shed cannot talk to,
//! because P6 is precisely about not disturbing a session somebody is using.
//!
//! That narrows the window from "the whole install" to "one round trip", and
//! **it does not eliminate it**: a session that starts between that answer and
//! the commit's `mv` is not detectable from here, and no number of extra checks
//! makes it detectable — there is no lock on the far side to hold. What makes
//! the residual tolerable is a property of the mechanism rather than of the
//! checking: replacing a binary by **rename** does not disturb a process that
//! is already running out of it. The running session keeps the inode it was
//! exec'd from, which stays alive until it exits, so an Update that lands under
//! a live session leaves that session running and P6's "never stopped, never
//! restarted" still holds. What it does not get is the *new* binary, until it
//! is next restarted by whoever owns it.
//!
//! **Two installers at once.** Pinned as **last-writer-wins** by plan 019 §3.4,
//! and deliberately left that way — there is no lock here, and this module adds
//! none. What it does add is honesty: the post-commit identify compares the
//! landed binary against the identity the **staged verify** accepted, not
//! merely against the protocol number, so client A cannot report that it
//! installed its own bytes when client B's different protocol-4 build is what
//! is at the destination ([`copy::foreign_install`]). On that path shed leaves
//! the other install alone — it does not roll its own backup forward over
//! somebody else's binary.
//!
//! One more shared-host behaviour is roost's and is **not** forked here:
//! `prepare_script` sweeps every `<dest>.tmp.<digits>` it finds, including one
//! a *live* second client is streaming into. roost's own comment says so and
//! accepts it (the loser fails cleanly at its own `chmod`, and nothing is
//! corrupted). shed composes roost's builders and does not carry a private copy
//! of one of them to change a trade-off roost has already made.
//!
//! ## `exit: None`
//!
//! [`Outcome::Exec`]'s `exit` is an `Option<i32>` because three real things
//! produce no status: a budget that expired, a transport that died, and
//! `dartssh2` closing a channel without an `exit-status` message. The machine's
//! rule is the simple one — **`exit: None` is a failure** — and the exception
//! lives in the runner, which is the only layer that can tell the three apart:
//!
//! > a runner whose transport reported a *clean* EOF, for a step whose stdout it
//! > read to the end, passes `exit: Some(0)`.
//!
//! The rule holds at **every** step that reads an answer, including the two
//! that read a line of prose rather than a NUL-delimited record. The staged
//! verify requires a zero exit before it parses an identity, and so does the
//! Start: roost's "read the readiness line before the exit status" is a rule
//! about a *failure's* detail — a `roost-session start` that refuses writes
//! `error: …` to stdout and exits 1 — and extending it to successes let an
//! `Outcome::Exec { exit: None, stdout: b"ready pid=4242\n" }` (a budget that
//! expired mid-read, a transport that died holding what it had) parse as
//! `ready`, wire hooks and return success. A failed step's stdout is now mined
//! for the reason and never for a verdict.
//!
//! That is safe precisely because it is not the only check. Every script whose
//! answer matters is NUL-terminated by construction and roost's parsers
//! ([`parse_discovery`](roost_ipc::bootstrap::parse_discovery),
//! [`parse_identity_pairs`](roost_ipc::bootstrap::parse_identity_pairs),
//! [`parse_prepare`](roost_ipc::bootstrap::parse_prepare)) refuse output that
//! does not end in one — so "the transport said it closed cleanly" cannot on its
//! own launder a truncated answer into a good one. Both halves are pinned by
//! tests.
//!
//! ## The FRB mirror
//!
//! `crates/CLAUDE.md`'s rule: mobile hand-mirrors these types into Dart, so
//! every DTO that crosses is owned `String` / `Option` / `Vec` / scalars — no
//! `serde_json::Value`, no `HashMap`, no borrowed lifetimes — and a fielded enum
//! becomes a Dart sealed class. [`Probe`], [`ProbeOutcome`], [`SessionState`],
//! [`Identity`], [`SessionIdentity`], [`Plan`], [`Installed`], [`HooksResult`],
//! [`BootstrapFailure`] and [`Stage`] all obey it.
//!
//! [`Step::Call`] and [`Outcome::Call`] carry a `serde_json::Value` and are the
//! **exception that proves it**: they never cross the bridge. Plan 019 §3.6 pins
//! that mobile runs `Call` and `Hooks` on the Rust side over the loopback `Conn`
//! it already has, and hands Dart only the `Exec` steps — because a phone has no
//! `ssh`, but it does have shed-core. A wire op's params are roost's shape, not
//! shed's, and re-mirroring them into Dart would be inventing a second copy of
//! roost's message schema for a value Dart never sees.

use std::io::{Read, Seek, SeekFrom};
use std::sync::{Arc, Mutex};
use std::time::Duration;

pub mod copy;
pub mod hooks;
pub mod machines;
pub mod plan;
pub mod source;

#[cfg(test)]
mod tests;

pub use copy::{BootstrapFailure, Stage};
pub use hooks::{wire_agent_hooks, HooksError, HooksResult, HooksSkip};
pub use machines::{InstallMachine, InstallRequest, Installed, ProbeMachine};
pub use plan::{fingerprint, Identity, Plan, Probe, ProbeOutcome, SessionIdentity, SessionState};
pub use source::{
    fetch_release, no_source, preview, resolve, unavailable, RoostRelease, Source, SourceEnv,
    SourcePreview, LATEST_KNOWN_RELEASE, RELEASE_PIN,
};

/// Lowercase hex, shared so `roost_ipc`'s own private copy (`bootstrap.rs`'s
/// asset-hashing) does not get a third independent restatement here:
/// [`plan::fingerprint`]'s digest and [`source::fetch_release`]'s checksum both
/// need one, and a two-line loop is not worth two divergent copies.
pub(crate) fn hex(bytes: &[u8]) -> String {
    bytes
        .iter()
        .fold(String::with_capacity(bytes.len() * 2), |mut out, byte| {
            use std::fmt::Write as _;
            let _ = write!(out, "{byte:02x}");
            out
        })
}

// ============================================================================
// Budgets and caps — roost's numbers, restated
// ============================================================================
//
// Every one of these is `private` in `roost_ipc::bootstrap` (they are constants
// of its *runtime*, and shed does not use its runtime). They are restated here
// rather than guessed at, each with the same justification roost gives, so a
// bump that changes one is a visible diff rather than a silent drift.

/// Per-exec budget for the read-only probe steps.
pub const PROBE_BUDGET: Duration = Duration::from_secs(30);

/// The install's remote steps that only *think* — prepare, verify the staged
/// file, commit, clean up, roll back. No bytes cross the wire on any of them.
pub const INSTALL_BUDGET: Duration = Duration::from_secs(60);

/// The stream phase, which carries the whole binary over a link nobody promised
/// anything about.
pub const STREAM_BUDGET: Duration = Duration::from_secs(300);

/// `roost-session start` plus the transport leg. The far side's forking parent
/// has its own 30 s readiness wait, so this must comfortably exceed it or a
/// start that was still going to answer reads as a timeout.
pub const START_BUDGET: Duration = Duration::from_secs(60);

/// Cap on a probe exec's stdout: two `uname` fields, the remote `$HOME`, and at
/// most one path per ladder rung.
pub const PROBE_STDOUT_CAP: usize = 64 * 1024;

/// Cap on an identity exec's stdout: the candidate paths it was asked about
/// plus one `identify` line. A binary that answers that question with more than
/// this is not answering it.
pub const IDENTITY_STDOUT_CAP: usize = 4 * 1024;

/// Cap for an exec whose output is a line or nothing — prepare, commit, the
/// readiness verdict, the `command -v` answer.
pub const SMALL_STDOUT_CAP: usize = 4 * 1024;

/// How many times a `already-running` verdict is re-tried before it is accepted.
///
/// roost sleeps a second between attempts and gives up after five seconds; a
/// sans-IO machine has no clock and the pinned [`Step`] set has no "wait", so
/// shed re-yields the start step immediately and caps the count instead. The
/// pacing that remains is a full transport round trip per attempt, which for an
/// `ssh` exec is the same order of magnitude — and the window `already-running`
/// covers is the sub-second socket-lock race between two starts, not a slow
/// boot. What actually makes accepting the verdict safe is the post-start
/// `session.identify` that follows it, which asks the session that is *serving*
/// who it is rather than trusting the one that was launched.
pub const START_RETRIES: usize = 5;

/// The remote command that reads a script off stdin — roost's convention,
/// restated (its own `SH_STDIN` is private).
///
/// Two words with nothing to quote, so the user's login shell — which is what
/// sshd hands a remote command to — parses it the same way every shell would.
/// `-s` is what makes `sh` read its program from stdin, which is the whole
/// discipline: a script that arrives as data is never re-parsed by anything,
/// however many apostrophes a path has in it.
pub const SH_STDIN: &str = "/bin/sh -s";

/// The refusal codes a runner reports on [`Outcome::Call`] when the *reach*
/// failed rather than the session refusing — the kebab spellings of roost's own
/// [`SshFailure`](roost_ipc::ssh::SshFailure), pinned two-language-style in
/// `crates/fixtures/roost-vectors/stderr-classes.json`.
///
/// These are what let one `session.identify` call distinguish the three states
/// the plan matrix turns on: a session answered, a `roost-session` is there but
/// is not running, or there is no `roost-session` at all. A runner that cannot
/// classify its failure sends anything else, and the machine reports the probe
/// as failed rather than guessing.
pub mod reach_code {
    /// Nothing on the ladder: exit 127 / `command not found`.
    pub const NOT_INSTALLED: &str = "not-found";
    /// Installed, not running: `client-bridge: no session` on stderr.
    pub const NO_SESSION: &str = "no-session";
}

// ============================================================================
// The step / outcome protocol
// ============================================================================

/// What the caller must do next, or the answer.
///
/// `T` is the machine's result type: [`ProbeMachine`] yields
/// `Step<Result<Probe, BootstrapFailure>>` and [`InstallMachine`] yields
/// `Step<Result<Installed, BootstrapFailure>>`. A failure arrives as
/// `Done(Err(_))` and **never** as a short-circuit, which is what lets the
/// install machine run its cleanup and rollback steps — themselves `Exec`s the
/// caller has to perform — before it reports what went wrong.
#[derive(Debug)]
pub enum Step<T> {
    /// Run `command` on the far side, the way a remote shell would run it.
    Exec {
        /// The remote command. Either [`SH_STDIN`] (with the script on
        /// `stdin`), or one of roost's self-contained commands —
        /// `stream_command`, `path_check_command`, `start_script` — which are
        /// `sh -c '…'` words the far side's login shell parses itself.
        command: String,
        stdin: Stdin,
        budget: Duration,
        stdout_cap: usize,
        /// `false` for a [`Stdin::Source`] step: `tee` echoes every byte it is
        /// fed, and buffering a 10 MiB binary back into memory to look at a
        /// stdout nothing reads would be absurd. roost sends that echo to
        /// `/dev/null` on the far side; a runner honours this flag so nothing
        /// on the near side accumulates it either.
        capture_stdout: bool,
    },
    /// One wire call over the client's own roost [`Conn`](crate::roost::Conn) —
    /// the ssh bridge on the desktop, the loopback port on the phone. Answered
    /// with the op's raw `result`, ungated: **the machine owns the protocol
    /// gate** (see the module doc), so a mismatched session must arrive here as
    /// an answer and not as a transport refusal.
    Call {
        op: String,
        params: serde_json::Value,
    },
    /// The lease dialogue, over that same connection. See [`hooks`].
    Hooks { client_label: String },
    /// There is nothing left to do.
    Done(T),
}

impl<T> Step<T> {
    /// Re-tag another machine's step as this one's.
    ///
    /// The three work variants say nothing about *whose* result they are on the
    /// way to, so they cross freely; only [`Step::Done`] carries the `T` the two
    /// machines disagree about, and it comes back as the `Err`. That is what
    /// lets [`InstallMachine`] yield the probe's own steps during its re-probe
    /// without hand-copying each variant — and, more to the point, without a
    /// second copy of the probe's first step drifting away from the first.
    fn retag<U>(self) -> Result<Step<U>, T> {
        match self {
            Step::Exec {
                command,
                stdin,
                budget,
                stdout_cap,
                capture_stdout,
            } => Ok(Step::Exec {
                command,
                stdin,
                budget,
                stdout_cap,
                capture_stdout,
            }),
            Step::Call { op, params } => Ok(Step::Call { op, params }),
            Step::Hooks { client_label } => Ok(Step::Hooks { client_label }),
            Step::Done(result) => Err(result),
        }
    }
}

/// What an [`Step::Exec`] is fed on stdin.
#[derive(Debug)]
pub enum Stdin {
    /// Nothing at all — the child sees an immediate EOF.
    Empty,
    /// A script, for a remote [`SH_STDIN`].
    Bytes(Vec<u8>),
    /// The verified descriptor, streamed. **Never a path:** the bytes shed
    /// hashed and the bytes shed sends have to be the same bytes, and a path
    /// re-opened between those two moments is a different file.
    Source(SourceHandle),
}

impl Stdin {
    /// A script, from the `String` roost's builders produce.
    fn script(script: String) -> Stdin {
        Stdin::Bytes(script.into_bytes())
    }
}

/// What happened when the caller did it.
#[derive(Debug)]
pub enum Outcome {
    Exec {
        /// `None` when the budget expired, the transport died, or the far side
        /// closed without a status. A failure — see the module doc for the one
        /// exception, and whose job it is.
        exit: Option<i32>,
        /// Empty when the step asked for no capture.
        stdout: Vec<u8>,
        /// The tail of stderr. One line of it reaches the user, so a runner
        /// that caps it should cap it at the end.
        stderr_tail: String,
    },
    Call(Result<serde_json::Value, CallError>),
    Hooks(HooksResult),
}

/// A refused or unreachable [`Step::Call`].
///
/// `code` is either a roost server code (`unknown-op`, `connect-required`, …)
/// or one of [`reach_code`]'s transport classifications. Owned strings, because
/// this crosses no FRB boundary but its message does reach a user.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct CallError {
    pub code: String,
    pub message: String,
}

impl CallError {
    pub fn new(code: impl Into<String>, message: impl Into<String>) -> CallError {
        CallError {
            code: code.into(),
            message: message.into(),
        }
    }
}

// ============================================================================
// The source
// ============================================================================

/// An open, verified `roost-session` ready to be streamed — and the only way
/// the machine will take one.
///
/// **Opaque and shared on purpose.** The desktop takes the handle out of a
/// [`Stdin::Source`] step and pumps it into an `ssh` subprocess; mobile keeps
/// the machine's own clone and lets Dart pull chunks through
/// `bootstrap_source_read` (plan 019 §3.6), because Dart must never be handed a
/// path it could re-open. Both read the *same* descriptor, which is the whole
/// point: whatever `sha256` says was checked is what goes across the wire.
///
/// Producing one is [`source`]'s job (plan 019 C5) — the override rung, the
/// sibling rung, the release asset. This file defines only what a machine
/// consumes.
#[derive(Clone)]
pub struct SourceHandle {
    /// A sentence fragment for the consent card: "the roost-session beside this
    /// app", "roost-session 0.0.19 from github.com/charliek/roost", …
    origin: Arc<str>,
    len: u64,
    sha256: Option<Arc<str>>,
    file: Arc<Mutex<std::fs::File>>,
}

impl SourceHandle {
    /// Wrap an already-open, already-verified file.
    ///
    /// The descriptor, not the path: the caller opened it, hashed what it read
    /// through it, and hands it on without ever naming it again.
    pub fn from_open_file(
        origin: impl Into<String>,
        file: std::fs::File,
        len: u64,
        sha256: Option<String>,
    ) -> SourceHandle {
        SourceHandle {
            origin: Arc::from(origin.into()),
            len,
            sha256: sha256.map(Arc::from),
            file: Arc::new(Mutex::new(file)),
        }
    }

    /// Where these bytes came from, for the consent card and the log.
    pub fn origin(&self) -> &str {
        &self.origin
    }

    /// How many bytes there are.
    pub fn len(&self) -> u64 {
        self.len
    }

    pub fn is_empty(&self) -> bool {
        self.len == 0
    }

    /// The hex sha256 the source ladder verified, when there was a published
    /// one to verify against.
    pub fn sha256(&self) -> Option<&str> {
        self.sha256.as_deref()
    }

    /// Read the next chunk, at most `max` bytes. An empty answer is EOF.
    ///
    /// Mobile's `bootstrap_source_read`; also how an in-process runner streams
    /// without a second implementation.
    pub fn read_chunk(&self, max: usize) -> std::io::Result<Vec<u8>> {
        let mut file = self.file.lock().expect("the source handle's lock");
        let mut buffer = vec![0u8; max];
        let read = file.read(&mut buffer)?;
        buffer.truncate(read);
        Ok(buffer)
    }

    /// Go back to the start. The machine never needs this — the stream phase
    /// runs once — but a caller that retries a whole install with the same
    /// handle does.
    pub fn rewind(&self) -> std::io::Result<()> {
        let mut file = self.file.lock().expect("the source handle's lock");
        file.seek(SeekFrom::Start(0))?;
        Ok(())
    }
}

impl std::fmt::Debug for SourceHandle {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        // Never the descriptor, and never a path — there is no path to leak.
        f.debug_struct("SourceHandle")
            .field("origin", &self.origin)
            .field("len", &self.len)
            .field("sha256", &self.sha256)
            .finish()
    }
}
