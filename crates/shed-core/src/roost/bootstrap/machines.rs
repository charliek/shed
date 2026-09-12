//! The two machines: [`ProbeMachine`] (read-only) and [`InstallMachine`].
//!
//! Both are driven the same way, and it is the shape plan 019 §3.6 pins for
//! mobile's FRB seam:
//!
//! ```ignore
//! let mut machine = ProbeMachine::new("roost:popos/p019-a", false);
//! let mut step = machine.begin();
//! loop {
//!     match step {
//!         Step::Done(result) => break result,
//!         other => step = machine.feed(run(other).await),
//!     }
//! }
//! ```
//!
//! Two properties hold that are worth knowing before reading the code:
//!
//! * **A failure is a `Done(Err(_))`, never a short circuit.** The install's
//!   rollback and cleanup are themselves remote execs the caller has to run, so
//!   a machine that returned `Err` from `feed` the moment a step failed could
//!   not undo anything. What goes wrong is *remembered*, the undo steps are
//!   yielded, and the failure is reported afterwards — which is also how the
//!   post-commit copy learns whether an incumbent was actually restored.
//! * **`begin` and `feed` are idempotent in their step.** Both end by *computing*
//!   the step for the current state rather than by returning one they stashed,
//!   so asking twice asks for the same thing. A runner that retries a transport
//!   error by re-reading the step is doing something well-defined.

use roost_ipc::bootstrap::{
    self as rb, BootstrapError, Discovery, InstallPhase, RemoteArch, Staged,
};
use roost_ipc::messages::{ops, SessionIdentify};
use roost_ipc::session_launch::Verdict;

use super::copy::{self, BootstrapFailure, Stage};
use super::hooks::HooksResult;
use super::plan::{
    classify_candidates, fingerprint, Identity, Plan, Probe, ProbeOutcome, SessionIdentity,
    SessionState,
};
use super::{
    reach_code, Outcome, SourceHandle, Stdin, Step, IDENTITY_STDOUT_CAP, INSTALL_BUDGET,
    PROBE_BUDGET, PROBE_STDOUT_CAP, SH_STDIN, SMALL_STDOUT_CAP, START_BUDGET, START_RETRIES,
    STREAM_BUDGET,
};

/// What a [`ProbeMachine`] yields.
pub type ProbeStep = Step<Result<Probe, BootstrapFailure>>;

/// What an [`InstallMachine`] yields.
pub type InstallStep = Step<Result<Installed, BootstrapFailure>>;

// ============================================================================
// Reading an outcome
// ============================================================================

/// An [`Outcome::Exec`], destructured.
struct ExecAnswer {
    exit: Option<i32>,
    stdout: Vec<u8>,
    stderr_tail: String,
}

impl ExecAnswer {
    /// **`exit: Some(0)` and nothing else.** See the module doc for why `None`
    /// is a failure here and where its one exception lives.
    fn ok(&self) -> bool {
        self.exit == Some(0)
    }

    fn detail(&self) -> String {
        copy::exec_detail(self.exit, &self.stderr_tail)
    }

    /// The first non-empty line of stdout, trimmed — roost's `first_line`.
    fn first_line(&self) -> Option<String> {
        String::from_utf8_lossy(&self.stdout)
            .lines()
            .map(str::trim)
            .find(|line| !line.is_empty())
            .map(str::to_string)
    }

    fn text(&self) -> std::borrow::Cow<'_, str> {
        String::from_utf8_lossy(&self.stdout)
    }
}

/// A runner that answers an exec step with a call outcome (or the other way
/// round) is broken in a way no user can act on — but it must still fail as a
/// bootstrap failure rather than as a panic, because on mobile the two sides of
/// this seam are in different languages.
fn wrong_kind(stage: Stage, wanted: &str) -> BootstrapFailure {
    BootstrapFailure::new(
        stage,
        format!("the bootstrap runner answered a {wanted} step with something else"),
    )
}

fn as_exec(stage: Stage, outcome: Outcome) -> Result<ExecAnswer, BootstrapFailure> {
    match outcome {
        Outcome::Exec {
            exit,
            stdout,
            stderr_tail,
        } => Ok(ExecAnswer {
            exit,
            stdout,
            stderr_tail,
        }),
        _ => Err(wrong_kind(stage, "exec")),
    }
}

type CallAnswer = Result<serde_json::Value, super::CallError>;

fn as_call(stage: Stage, outcome: Outcome) -> Result<CallAnswer, BootstrapFailure> {
    match outcome {
        Outcome::Call(answer) => Ok(answer),
        _ => Err(wrong_kind(stage, "call")),
    }
}

fn as_hooks(stage: Stage, outcome: Outcome) -> Result<HooksResult, BootstrapFailure> {
    match outcome {
        Outcome::Hooks(result) => Ok(result),
        _ => Err(wrong_kind(stage, "hooks")),
    }
}

/// roost's own failure types, in shed's copy.
fn probe_error(target: &str, error: &BootstrapError) -> BootstrapFailure {
    match error {
        BootstrapError::UnsupportedOs(os) => copy::unsupported_os(target, os),
        BootstrapError::UnsupportedArch(arch) => copy::unsupported_arch(target, arch),
        other => BootstrapFailure::from_roost(Stage::Probe, other, target),
    }
}

/// The one `Step::Call` both machines make: `session.identify`, raw.
///
/// Raw and ungated on purpose. `Conn::session_identify` refuses a protocol
/// mismatch by name, which is exactly right for a watcher and exactly wrong
/// here: a mismatched session is the plan matrix's fifth row and has to arrive
/// as an *answer* so shed can report it (pin P6) rather than as a transport
/// error indistinguishable from a dead host.
fn identify_call<T>() -> Step<T> {
    Step::Call {
        op: ops::SESSION_IDENTIFY.to_string(),
        params: serde_json::json!({}),
    }
}

/// Read a `session.identify` answer into the machine's session state.
///
/// The two reach codes are how one call distinguishes three states — see
/// [`reach_code`].
/// Read a `session.identify` answer into a [`SessionState`].
///
/// `stage` is the gate that asked, not a constant: the same question is put at
/// three different points — the probe, the last look before the commit, and the
/// look at whatever came up after a start — and a failure has to be reported
/// against the one that actually failed. Hard-coding [`Stage::Probe`] here made
/// a pre-commit refusal claim it happened during the probe, which is the wrong
/// thing to hand a log or a progress line; the caller that cared then patched
/// the field back by struct update, which is the shape a parameter is for.
pub(super) fn session_state(
    target: &str,
    stage: Stage,
    answer: CallAnswer,
) -> Result<SessionState, BootstrapFailure> {
    let failed = |detail: String| BootstrapFailure {
        stage,
        ..copy::probe_failed(target, &detail)
    };
    match answer {
        Ok(value) => {
            let identify: SessionIdentify = serde_json::from_value(value)
                .map_err(|error| failed(format!("its identity did not parse: {error}")))?;
            Ok(SessionState::Running {
                identity: identify.into(),
            })
        }
        Err(error) if error.code == reach_code::NOT_INSTALLED => Ok(SessionState::NotInstalled),
        Err(error) if error.code == reach_code::NO_SESSION => Ok(SessionState::NoSession),
        Err(error) => Err(failed(format!("{}: {}", error.code, error.message))),
    }
}

// ============================================================================
// The probe
// ============================================================================

#[derive(Debug)]
enum ProbeState {
    /// Nothing asked yet, or asked and not yet answered — the same step either
    /// way, because `next_step` is a function of the state and a discovery that
    /// has not been answered still wants the discovery script.
    Discovery,
    /// The ladder's answer, waiting on the remote shell's own `command -v`.
    AwaitPathCheck {
        found: Discovery,
        arch: RemoteArch,
    },
    AwaitIdentity {
        arch: RemoteArch,
        home: String,
        candidates: Vec<String>,
    },
    AwaitSession {
        arch: RemoteArch,
        home: String,
        candidates: Vec<String>,
        outcome: ProbeOutcome,
    },
    Done(Result<Probe, BootstrapFailure>),
}

/// One read-only look at a host: what platform it is, what `roost-session`
/// binaries are on it, and whether one of them is serving.
///
/// **Nothing here writes, starts or stops anything**, which is what makes it
/// safe to run *before* the consent card — and why the consent card can carry
/// its [`Probe::fingerprint`] as the thing the install re-checks.
#[derive(Debug)]
pub struct ProbeMachine {
    target: String,
    /// roost's `BootstrapOptions::jail_fs_root`. `false` in production; `true`
    /// only in a hermetic lane, where it prefixes the ladder's absolute rungs
    /// with `${ROOST_BOOTSTRAP_FS_ROOT}` so a test cannot probe the developer's
    /// own `/usr/bin`.
    jail_fs_root: bool,
    state: ProbeState,
}

impl ProbeMachine {
    pub fn new(target: impl Into<String>, jail_fs_root: bool) -> ProbeMachine {
        ProbeMachine {
            target: target.into(),
            jail_fs_root,
            state: ProbeState::Discovery,
        }
    }

    /// The first step.
    pub fn begin(&mut self) -> ProbeStep {
        self.next_step()
    }

    /// Record what happened, and say what is next.
    pub fn feed(&mut self, outcome: Outcome) -> ProbeStep {
        self.advance(outcome);
        self.next_step()
    }

    /// The answer, once there is one.
    pub fn result(&self) -> Option<&Result<Probe, BootstrapFailure>> {
        match &self.state {
            ProbeState::Done(result) => Some(result),
            _ => None,
        }
    }

    fn next_step(&self) -> ProbeStep {
        match &self.state {
            ProbeState::Discovery => discovery_step(self.jail_fs_root),
            ProbeState::AwaitPathCheck { .. } => path_check_step(),
            ProbeState::AwaitIdentity { candidates, .. } => script_step(
                rb::identity_script(candidates),
                PROBE_BUDGET,
                IDENTITY_STDOUT_CAP,
            ),
            ProbeState::AwaitSession { .. } => identify_call(),
            ProbeState::Done(result) => Step::Done(result.clone()),
        }
    }

    /// The state transition, with no step computation in it — so
    /// [`InstallMachine`] can drive a probe without having to interpret the step
    /// it would have yielded.
    fn advance(&mut self, outcome: Outcome) {
        let state = std::mem::replace(&mut self.state, ProbeState::Discovery);
        self.state = match self.step_from(state, outcome) {
            Ok(next) => next,
            Err(failure) => ProbeState::Done(Err(failure)),
        };
    }

    fn step_from(
        &self,
        state: ProbeState,
        outcome: Outcome,
    ) -> Result<ProbeState, BootstrapFailure> {
        let target = &self.target;
        match state {
            ProbeState::Discovery => {
                let answer = as_exec(Stage::Probe, outcome)?;
                if !answer.ok() {
                    return Err(copy::probe_failed(target, &answer.detail()));
                }
                let found =
                    rb::parse_discovery(&answer.stdout).map_err(|e| probe_error(target, &e))?;
                rb::check_os(&found.os).map_err(|e| probe_error(target, &e))?;
                let arch = rb::map_arch(&found.arch).map_err(|e| probe_error(target, &e))?;
                Ok(ProbeState::AwaitPathCheck { found, arch })
            }
            ProbeState::AwaitPathCheck { found, arch } => {
                // **Every failure here is silence**, roost's rule: `command -v`
                // exits non-zero for "not found", which is an answer and not a
                // fault, and a step that exists to catch an unusual `PATH` must
                // not be able to fail a probe that has already succeeded
                // without it.
                let mut candidates = found.candidates;
                if let Ok(answer) = as_exec(Stage::Probe, outcome) {
                    if answer.ok() {
                        // Appended AFTER every ladder rung, never spliced among
                        // them: the verdict is about the rung the transport will
                        // exec, and a path the transport cannot reach must not
                        // get to decide whether an install is offered.
                        if let Some(extra) = answer.first_line().filter(|path| {
                            path.starts_with('/') && !candidates.iter().any(|known| known == path)
                        }) {
                            candidates.push(extra);
                        }
                    }
                }
                if candidates.is_empty() {
                    return Ok(ProbeState::AwaitSession {
                        arch,
                        home: found.home,
                        candidates,
                        outcome: ProbeOutcome::Missing,
                    });
                }
                Ok(ProbeState::AwaitIdentity {
                    arch,
                    home: found.home,
                    candidates,
                })
            }
            ProbeState::AwaitIdentity {
                arch,
                home,
                candidates,
            } => {
                let answer = as_exec(Stage::Probe, outcome)?;
                if !answer.ok() {
                    return Err(copy::probe_failed(target, &answer.detail()));
                }
                let pairs = rb::parse_identity_pairs(&answer.stdout, &candidates)
                    .map_err(|e| probe_error(target, &e))?;
                Ok(ProbeState::AwaitSession {
                    arch,
                    home,
                    candidates,
                    outcome: classify_candidates(&pairs),
                })
            }
            ProbeState::AwaitSession {
                arch,
                home,
                candidates,
                outcome: found,
            } => {
                let answer = as_call(Stage::Probe, outcome)?;
                let session = session_state(target, Stage::Probe, answer)?;
                let arch = arch.as_str().to_string();
                let fingerprint = fingerprint(target, &arch, &home, &found, &session);
                Ok(ProbeState::Done(Ok(Probe {
                    outcome: found,
                    arch,
                    home,
                    session,
                    candidates,
                    fingerprint,
                })))
            }
            ProbeState::Done(result) => Ok(ProbeState::Done(result)),
        }
    }
}

// ============================================================================
// The install
// ============================================================================

/// What an [`InstallMachine`] was asked to do.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct InstallRequest {
    /// The target grammar's name for the host — `machine:<name>` or
    /// `roost:<server>/<shed>`. Appears in every sentence the user reads.
    pub target: String,
    /// See [`ProbeMachine::jail_fs_root`](ProbeMachine).
    pub jail_fs_root: bool,
    /// The [`Probe::fingerprint`] the consent card was built from. The install
    /// re-probes and refuses if the host moved underneath it.
    pub fingerprint: String,
    /// Who shed says it is on the wire: `shed-desktop`, `shed-mobile`. Becomes
    /// the lease's label and `session.set_agent_hooks`'s `client`.
    pub client_label: String,
}

/// What an install came to.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Installed {
    pub target: String,
    /// The row the re-probe landed on — which is what actually ran, not what the
    /// consent card guessed.
    pub plan: Plan,
    /// Where a binary was written, when this run wrote one.
    pub dest: Option<String>,
    /// roost's readiness verdict, rendered (`ready pid=1234`,
    /// `already-running`). `None` when nothing needed starting.
    pub verdict: Option<String>,
    /// The session that is serving now.
    pub session: Option<SessionIdentity>,
    /// The far side's own shell disagrees about where `roost-session` is. A
    /// warning that rides out on the *success* value, because it is not a
    /// failure and pin P5 forbids doing anything about it.
    pub path_warning: Option<String>,
    /// Shed's copy of the previous install could not be discarded, so it is
    /// still on the host at `<dest>.bak.<pid>`.
    ///
    /// A warning and not a failure — the install worked — but it is *said*,
    /// because a backup nobody was told about is exactly the file a later run
    /// finds at a predictable, pid-derived path and has to be careful of (see
    /// the module doc).
    pub backup_warning: Option<String>,
    /// What the lease dialogue came to. `None` when shed started nothing, so
    /// there was no Start of shed's for hooks to follow.
    pub hooks: Option<HooksResult>,
}

#[derive(Debug)]
enum State {
    /// The re-probe.
    Probing(Box<ProbeMachine>),
    Prepare,
    Stream(Staged),
    Verify(Staged),
    /// One last `session.identify` after the staged bytes have proved
    /// themselves and **before** anything is replaced — the post-probe race
    /// narrower. See the module doc for exactly how much it closes.
    PreCommitIdentify(Staged),
    Commit(Staged),
    PostCommit(Staged),
    /// The last step of the install, and the point of no return: once this has
    /// run there is nothing to roll back to.
    DiscardBackup(Staged),
    Start {
        path: String,
        attempts: usize,
    },
    PostStart,
    PathCheck,
    Hooks,
    /// Best-effort undo, carrying the failure that caused it. **Only reached
    /// when this attempt actually ran the commit script** — see
    /// [`InstallMachine::fail_pending`], which is where the corruption path
    /// this used to have is closed.
    Rollback(Staged, Box<Pending>),
    Cleanup(Staged, Box<Pending>, Undo),
    Done(Box<Result<Installed, BootstrapFailure>>),
}

/// A failure on its way out through the undo steps.
///
/// The sentence is built at the **end** of that chain rather than where the
/// failure happened, because on two of these families the sentence turns on what
/// the undo did — and an undo step's answer is an [`Outcome`] that has not
/// arrived yet at the moment the failure is recorded. A cleanup that could not
/// remove the staged file falsifies a clause in every one of them, so it is
/// appended to all three.
#[derive(Debug)]
enum Pending {
    /// A sentence nothing but the cleanup can change.
    Failure(Box<BootstrapFailure>),
    /// A commit-stage failure. roost's copy for it ends "any previous install
    /// there was put back", which is a claim about the rollback — true on every
    /// path except the one where the rollback itself failed.
    Commit { detail: String },
    /// A post-commit failure. roost's copy has two arms and `restored` picks
    /// between them; a rollback that *failed* is a third case neither arm can
    /// say, and saying "there was no previous install to fall back to" when
    /// there was one and it is stranded at `.bak.<pid>` is the worst of the
    /// three answers.
    PostCommit { detail: String },
}

/// What the rollback did, when one was run at all.
#[derive(Debug)]
enum Rollback {
    /// It printed `restored` — an incumbent was genuinely put back.
    Restored,
    /// It ran, and printed nothing: there was no backup to put back.
    Nothing,
    /// It would not run. Any incumbent is stranded at `<dest>.bak.<pid>`, which
    /// a human can act on only if they are told where it is.
    Failed(String),
}

/// What the undo steps came to, gathered on the way out.
///
/// Every field here used to be a `let _ =`. Three separate sentences in the
/// copy — "the existing install is unchanged", "any previous install there was
/// put back", "the staged file was removed" — are claims about these outcomes,
/// so discarding them made the copy a statement about what shed *attempted*.
#[derive(Debug, Default)]
struct Undo {
    /// `None` when no rollback was run at all — which is every failure before
    /// the commit script ran.
    rollback: Option<Rollback>,
    /// `Some(detail)` when the cleanup could not remove the staged file.
    cleanup_failed: Option<String>,
}

impl Pending {
    /// The sentence, once every undo step has answered.
    fn into_failure(self, target: &str, staged: Option<&Staged>, undo: &Undo) -> BootstrapFailure {
        let (backup, dest, tmp) = match staged {
            Some(staged) => (
                staged.backup.as_str(),
                staged.dest.as_str(),
                staged.tmp.as_str(),
            ),
            // Unreachable for the two families that name a path: both are
            // reached only with a `Staged` in hand.
            None => ("", "", ""),
        };
        let mut failure = match self {
            Pending::Failure(failure) => *failure,
            Pending::Commit { detail } => match &undo.rollback {
                Some(Rollback::Failed(undone)) => {
                    copy::commit_not_put_back(target, &detail, backup, dest, undone)
                }
                _ => copy::install_phase_failed(target, InstallPhase::Commit, &detail),
            },
            Pending::PostCommit { detail } => match &undo.rollback {
                Some(Rollback::Restored) => copy::post_commit_failed(target, &detail, true),
                Some(Rollback::Failed(undone)) => {
                    copy::post_commit_not_put_back(target, &detail, backup, dest, undone)
                }
                _ => copy::post_commit_failed(target, &detail, false),
            },
        };
        if let Some(detail) = &undo.cleanup_failed {
            failure.message.push(' ');
            failure
                .message
                .push_str(&copy::cleanup_failed(target, tmp, detail));
        }
        failure
    }
}

/// Install (or update, or merely start) a `roost-session`, in roost's order.
///
/// The order is the whole safety argument, and it is roost's: re-probe, prepare,
/// stream, **verify the staged file**, commit, re-verify the committed file,
/// discard the backup — and only then start. Nothing replaces the destination
/// until the bytes that arrived have identified themselves as something shed can
/// talk to. See the module doc for the rollback promise that order buys, and for
/// exactly where it stops.
#[derive(Debug)]
pub struct InstallMachine {
    request: InstallRequest,
    source: Option<SourceHandle>,
    state: State,
    /// Filled in by the re-probe.
    plan: Option<Plan>,
    /// Where a binary was written, when one was.
    dest: Option<String>,
    /// The path a start would exec — the just-committed destination, or the rung
    /// the probe found for a start-only flow.
    start_path: Option<String>,
    /// **True once THIS attempt has run the commit script**, which is the only
    /// step that can create `<dest>.bak.<pid>`.
    ///
    /// The gate on the rollback, and the whole of fix 1. `<dest>.bak.<pid>` is
    /// named from the far side's `$$` and nothing else, `prepare_script` sweeps
    /// `.tmp.*` but never `.bak.*`, and pids are reused — so a backup left
    /// behind by an earlier attempt (a discard that failed, a connection that
    /// dropped) is a file this attempt can find at exactly the path its own
    /// rollback would restore from. Running the rollback on a failure that
    /// happened *before* the commit would then move a stranger's stale bytes
    /// over a perfectly good incumbent, and report the existing install as
    /// unchanged.
    ///
    /// Set when the commit step's outcome is consumed, before it is read:
    /// a commit whose outcome cannot even be classified still ran on the far
    /// side, and "the incumbent may be at `.bak`" is the assumption that errs
    /// toward putting it back.
    commit_ran: bool,
    /// The identity the **staged verify** accepted, kept so the post-commit
    /// identify can check that what is at the destination is what shed staged
    /// and not merely something with the right protocol number.
    staged_identity: Option<Identity>,
    /// Whether the re-probe found a binary at the install destination — i.e.
    /// whether a commit would have had an incumbent to move aside. Only used to
    /// decide whether a sentence should name `<dest>.bak.<pid>`.
    incumbent_at_dest: bool,
    verdict: Option<String>,
    session: Option<SessionIdentity>,
    path_warning: Option<String>,
    backup_warning: Option<String>,
    hooks: Option<HooksResult>,
}

impl InstallMachine {
    /// `source` is `Some` exactly when the consented plan needed bytes. A plan
    /// that needs them and has none fails at the re-probe with
    /// [`copy::missing_source`] — before anything is written.
    pub fn new(request: InstallRequest, source: Option<SourceHandle>) -> InstallMachine {
        let probe = ProbeMachine::new(request.target.clone(), request.jail_fs_root);
        InstallMachine {
            request,
            source,
            state: State::Probing(Box::new(probe)),
            plan: None,
            dest: None,
            start_path: None,
            commit_ran: false,
            staged_identity: None,
            incumbent_at_dest: false,
            verdict: None,
            session: None,
            path_warning: None,
            backup_warning: None,
            hooks: None,
        }
    }

    /// The verified descriptor this install is streaming, for a client that
    /// reads it in chunks of its own (mobile's `bootstrap_source_read`).
    pub fn source(&self) -> Option<SourceHandle> {
        self.source.clone()
    }

    pub fn begin(&mut self) -> InstallStep {
        self.next_step()
    }

    pub fn feed(&mut self, outcome: Outcome) -> InstallStep {
        self.advance(outcome);
        self.next_step()
    }

    pub fn result(&self) -> Option<&Result<Installed, BootstrapFailure>> {
        match &self.state {
            State::Done(result) => Some(result),
            _ => None,
        }
    }

    fn next_step(&self) -> InstallStep {
        match &self.state {
            // The re-probe's own steps, re-tagged. Its `Done` is unreachable in
            // practice — `advance` transitions out of `Probing` the moment the
            // inner machine finishes — and yielding the discovery step again is
            // the harmless answer.
            State::Probing(probe) => probe
                .next_step()
                .retag()
                .unwrap_or_else(|_| discovery_step(self.request.jail_fs_root)),
            State::Prepare => script_step(rb::prepare_script(), INSTALL_BUDGET, SMALL_STDOUT_CAP),
            State::Stream(staged) => Step::Exec {
                command: rb::stream_command(&staged.tmp),
                stdin: match self.source.clone() {
                    Some(source) => Stdin::Source(source),
                    // Cannot happen: the re-probe refuses a byte-needing plan
                    // with no source. An empty stdin then fails the staged
                    // verify, which is the safe direction.
                    None => Stdin::Empty,
                },
                budget: STREAM_BUDGET,
                stdout_cap: SMALL_STDOUT_CAP,
                // `tee` echoes every byte it is fed. Buffering a whole binary
                // back into memory for a stdout nothing reads would be absurd.
                capture_stdout: false,
            },
            State::Verify(staged) => script_step(
                rb::verify_staged_script(&staged.tmp),
                INSTALL_BUDGET,
                SMALL_STDOUT_CAP,
            ),
            State::PreCommitIdentify(_) => identify_call(),
            State::Commit(staged) => script_step(
                rb::commit_script(&staged.tmp, &staged.dest, &staged.backup),
                INSTALL_BUDGET,
                SMALL_STDOUT_CAP,
            ),
            State::PostCommit(staged) => script_step(
                rb::identity_script(std::slice::from_ref(&staged.dest)),
                INSTALL_BUDGET,
                IDENTITY_STDOUT_CAP,
            ),
            State::DiscardBackup(staged) => script_step(
                rb::discard_backup_script(&staged.backup),
                INSTALL_BUDGET,
                SMALL_STDOUT_CAP,
            ),
            State::Start { path, .. } => Step::Exec {
                command: rb::start_script(path),
                stdin: Stdin::Empty,
                budget: START_BUDGET,
                stdout_cap: SMALL_STDOUT_CAP,
                capture_stdout: true,
            },
            State::PostStart => identify_call(),
            State::PathCheck => path_check_step(),
            State::Hooks => Step::Hooks {
                client_label: self.request.client_label.clone(),
            },
            State::Rollback(staged, _) => script_step(
                rb::rollback_script(&staged.dest, &staged.backup),
                INSTALL_BUDGET,
                SMALL_STDOUT_CAP,
            ),
            State::Cleanup(staged, _, _) => script_step(
                rb::cleanup_script(&staged.tmp),
                INSTALL_BUDGET,
                SMALL_STDOUT_CAP,
            ),
            State::Done(result) => Step::Done((**result).clone()),
        }
    }

    fn advance(&mut self, outcome: Outcome) {
        let state = std::mem::replace(&mut self.state, State::Prepare);
        self.state = self.step_from(state, outcome);
    }

    /// Fail this install, running the undo steps first when there is anything to
    /// undo. The pending failure rides along to the end of the chain.
    fn fail(&self, staged: Option<Staged>, failure: BootstrapFailure) -> State {
        self.fail_pending(staged, Pending::Failure(Box::new(failure)))
    }

    /// Choose the undo chain, which is **not** the same choice as "is there a
    /// `Staged`".
    ///
    /// A rollback restores `<dest>.bak.<pid>`, and that name is derived from a
    /// pid and nothing else. A backup at that path is therefore only *known* to
    /// be this attempt's if this attempt ran the step that makes one — the
    /// commit script. Before that, the only thing this attempt has created is
    /// the temporary, so the only undo it is entitled to perform is the cleanup
    /// that removes it. Running the rollback anyway is how a stale backup left
    /// by an earlier attempt gets renamed over a working incumbent, with the
    /// copy then reporting that the existing install is unchanged.
    ///
    /// See [`InstallMachine::commit_ran`] and the module doc.
    fn fail_pending(&self, staged: Option<Staged>, pending: Pending) -> State {
        match staged {
            Some(staged) if self.commit_ran => {
                // roost's order, and it matters: put the incumbent back FIRST (a
                // rename over a regular file has no window with nothing in it),
                // then remove the temporary.
                State::Rollback(staged, Box::new(pending))
            }
            Some(staged) => State::Cleanup(staged, Box::new(pending), Undo::default()),
            None => State::Done(Box::new(Err(pending.into_failure(
                &self.request.target,
                None,
                &Undo::default(),
            )))),
        }
    }

    /// True once bytes have landed at the destination — which is exactly when
    /// this run has a `dest` to report, since the commit sets both in the same
    /// breath. The Start copy turns on it: "the new binary is in place" is a lie
    /// in a start-only flow, and two fields that must agree are one invariant
    /// nobody is enforcing.
    fn installed(&self) -> bool {
        self.dest.is_some()
    }

    /// A failure after the backup has been discarded, which is the point past
    /// which there is nothing left to undo — so the only thing still worth
    /// saying is whether that discard worked.
    fn fail_after_discard(&self, failure: BootstrapFailure) -> State {
        State::Done(Box::new(Err(self.annotate(failure))))
    }

    /// A Start-or-after failure, in the copy that turns on whether this run
    /// wrote a binary. Past the discard there is nothing left to undo, so this
    /// is always the end.
    fn start_failed(&self, detail: &str) -> State {
        self.fail_after_discard(copy::start_failed(
            &self.request.target,
            detail,
            self.installed(),
        ))
    }

    /// Append the discard warning, when there is one, to a sentence built after
    /// it.
    fn annotate(&self, mut failure: BootstrapFailure) -> BootstrapFailure {
        if let Some(warning) = &self.backup_warning {
            failure.message.push(' ');
            failure.message.push_str(warning);
        }
        failure
    }

    fn finish(&self) -> State {
        State::Done(Box::new(Ok(Installed {
            target: self.request.target.clone(),
            plan: self.plan.clone().unwrap_or(Plan::Start {
                path: String::new(),
            }),
            dest: self.dest.clone(),
            verdict: self.verdict.clone(),
            session: self.session.clone(),
            path_warning: self.path_warning.clone(),
            backup_warning: self.backup_warning.clone(),
            hooks: self.hooks.clone(),
        })))
    }

    fn step_from(&mut self, state: State, outcome: Outcome) -> State {
        let target = self.request.target.clone();
        match state {
            State::Probing(mut probe) => {
                probe.advance(outcome);
                // Cloned out rather than borrowed, so the `None` arm can hand
                // the machine back into the state it came from.
                match probe.result().cloned() {
                    None => State::Probing(probe),
                    Some(Err(failure)) => State::Done(Box::new(Err(failure))),
                    Some(Ok(found)) => self.plan_from(&found),
                }
            }
            State::Prepare => {
                let answer = match as_exec(Stage::Prepare, outcome) {
                    Ok(answer) => answer,
                    Err(failure) => return self.fail(None, failure),
                };
                if !answer.ok() {
                    return self.fail(
                        None,
                        copy::install_phase_failed(
                            &target,
                            InstallPhase::Prepare,
                            &answer.detail(),
                        ),
                    );
                }
                match rb::parse_prepare(&answer.stdout) {
                    // Nothing has been staged that this side knows the name of,
                    // so there is nothing to clean up — and the temporary the
                    // far side may have made is swept by the next prepare, which
                    // is why roost's script sweeps.
                    Err(error) => self.fail(
                        None,
                        BootstrapFailure::from_roost(Stage::Prepare, &error, &target),
                    ),
                    Ok(staged) => State::Stream(staged),
                }
            }
            State::Stream(staged) => {
                let answer = match as_exec(Stage::Stream, outcome) {
                    Ok(answer) => answer,
                    Err(failure) => return self.fail(Some(staged), failure),
                };
                if !answer.ok() {
                    let failure =
                        copy::install_phase_failed(&target, InstallPhase::Stream, &answer.detail());
                    return self.fail(Some(staged), failure);
                }
                State::Verify(staged)
            }
            State::Verify(staged) => {
                let answer = match as_exec(Stage::Verify, outcome) {
                    Ok(answer) => answer,
                    Err(failure) => return self.fail(Some(staged), failure),
                };
                if !answer.ok() {
                    let failure = copy::staged_verify_refused(&target, &answer.detail());
                    return self.fail(Some(staged), failure);
                }
                let found = Identity::parse(&answer.text());
                if !found.as_ref().is_some_and(Identity::compatible) {
                    let failure = copy::staged_verify_refused(
                        &target,
                        &copy::identity_detail(found.as_ref()),
                    );
                    return self.fail(Some(staged), failure);
                }
                // Kept for the post-commit check: "something protocol-4 is at
                // the destination" and "the bytes shed staged are at the
                // destination" are different claims, and only the second is what
                // the user consented to.
                self.staged_identity = found;
                State::PreCommitIdentify(staged)
            }
            State::PreCommitIdentify(staged) => {
                // **The race narrower.** Both the consent probe and the re-probe
                // can see a binary with nothing running out of it, and a session
                // can start in the window between the second of those and this
                // moment — at which point the commit's `mv` would replace a file
                // that has a live process behind it. Asking once more, as late
                // as it can be asked, turns most of that window into a refusal.
                // It does not turn all of it into one; the module doc says so
                // out loud.
                let answer = match as_call(Stage::Commit, outcome) {
                    Ok(answer) => answer,
                    Err(failure) => return self.fail(Some(staged), failure),
                };
                match session_state(&target, Stage::Commit, answer) {
                    // A probe failure here is not a reason to write to the host:
                    // shed came to replace a binary and can no longer tell what
                    // is using it.
                    Err(failure) => self.fail(Some(staged), failure),
                    Ok(SessionState::Running { identity }) if identity.compatible() => {
                        self.fail(Some(staged), copy::session_appeared(&target))
                    }
                    Ok(SessionState::Running { identity }) => self.fail(
                        Some(staged),
                        copy::session_appeared_mismatched(&target, identity.session_protocol),
                    ),
                    Ok(_) => State::Commit(staged),
                }
            }
            State::Commit(staged) => {
                // **Before the outcome is even read.** The commit script is the
                // only step that creates `<dest>.bak.<pid>`, and an outcome this
                // side could not classify is not evidence that the far side
                // never ran it.
                self.commit_ran = true;
                let answer = match as_exec(Stage::Commit, outcome) {
                    Ok(answer) => answer,
                    Err(failure) => return self.fail(Some(staged), failure),
                };
                if !answer.ok() {
                    return self.fail_pending(
                        Some(staged),
                        Pending::Commit {
                            detail: answer.detail(),
                        },
                    );
                }
                // The bytes are at the destination from here on, whatever
                // happens next — which is what `installed()` reads.
                self.dest = Some(staged.dest.clone());
                self.start_path = Some(staged.dest.clone());
                State::PostCommit(staged)
            }
            State::PostCommit(staged) => {
                // The same question the staged verify asked, of the file that is
                // now installed: the staged verify proved the *temporary* was
                // right, and a `mv` is not the only thing that can happen to a
                // path between two execs.
                let answer = match as_exec(Stage::PostCommit, outcome) {
                    Ok(answer) => answer,
                    Err(failure) => return self.fail(Some(staged), failure),
                };
                let mut landed = None;
                let detail = if !answer.ok() {
                    Some(answer.detail())
                } else {
                    match rb::parse_identity_pairs(
                        &answer.stdout,
                        std::slice::from_ref(&staged.dest),
                    ) {
                        Err(error) => Some(error.message(&target)),
                        Ok(pairs) => match pairs.first() {
                            None => Some(format!(
                                "{} was gone by the time it was asked to identify itself",
                                staged.dest
                            )),
                            Some((_, stdout)) => {
                                let found = Identity::parse(stdout);
                                if found.as_ref().is_some_and(Identity::compatible) {
                                    landed = found;
                                    None
                                } else {
                                    Some(copy::identity_detail(found.as_ref()))
                                }
                            }
                        },
                    }
                };
                if let Some(detail) = detail {
                    // The sentence is built at the end of the undo chain: the
                    // rollback is the only step that knows whether there was an
                    // incumbent to put back, and whether putting it back worked.
                    return self.fail_pending(Some(staged), Pending::PostCommit { detail });
                }
                // **Last-writer-wins, made honest** (plan 019 §3.4 pins the
                // policy; fix 6 pins that shed does not misreport it). Two
                // clients installing at once is the normal case — the desktop
                // and the phone — and this check is what the plan calls the
                // detector. A detector that compares only the protocol number
                // reports success for somebody *else's* protocol-4 build: shed
                // would discard its backup and say it installed bytes it never
                // installed. Nothing is rolled back on this path either, because
                // the file at the destination is not shed's to replace.
                let staged_identity = self.staged_identity.clone();
                match (staged_identity, landed) {
                    (Some(wanted), Some(found)) if wanted != found => {
                        let backup = self.incumbent_at_dest.then(|| staged.backup.clone());
                        let failure = copy::foreign_install(
                            &target,
                            &staged.dest,
                            &wanted,
                            &found,
                            backup.as_deref(),
                        );
                        State::Cleanup(
                            staged,
                            Box::new(Pending::Failure(Box::new(failure))),
                            Undo::default(),
                        )
                    }
                    _ => State::DiscardBackup(staged),
                }
            }
            State::DiscardBackup(staged) => {
                // Best-effort: a backup left behind is a stale file, not a broken
                // install, and failing the whole job over it would be worse. The
                // rollback promise ends here — see the module doc.
                //
                // **Best-effort is not the same as unexamined.** A discard that
                // failed leaves `<dest>.bak.<pid>` on the host, which is both a
                // thing the user may want to delete and the exact state that
                // arms the stale-backup trap `commit_ran` exists to defuse. It
                // rides out on the success value, and on any later failure's
                // sentence.
                if let Err(detail) = undo_answer(outcome) {
                    self.backup_warning =
                        Some(copy::discard_failed(&target, &staged.backup, &detail));
                }
                State::Start {
                    path: staged.dest,
                    attempts: 0,
                }
            }
            State::Start { path, attempts } => {
                let answer = match as_exec(Stage::Start, outcome) {
                    Ok(answer) => answer,
                    Err(failure) => return self.fail_after_discard(failure),
                };
                // **A verdict is only a verdict when the step succeeded.**
                //
                // roost's rule is that the readiness line is read from *stdout*
                // — a `roost-session start` that refuses writes `error: …` there
                // and exits 1, so demanding a zero exit before looking would
                // throw the reason away and replace it with "it exited 1". That
                // rule is about a **failure's** detail, and the previous version
                // of this arm quietly extended it to successes: an
                // `Outcome::Exec { exit: None, stdout: "ready pid=4242" }` —
                // which is a budget that expired or a transport that died,
                // holding whatever had been read so far — parsed as `ready`,
                // wired hooks and returned success.
                //
                // So: a failed step's stdout is mined for the reason and never
                // for a verdict. The module doc's `exit: None` exception belongs
                // to the runner, which is the only layer that can tell a clean
                // EOF from a dead channel; here it is a failure, and this is one
                // of the two places it had stopped being one.
                if !answer.ok() {
                    let detail = match answer.first_line().map(|line| Verdict::parse(&line)) {
                        Some(Verdict::Error(reason)) => reason,
                        _ => answer.detail(),
                    };
                    return self.start_failed(&detail);
                }
                let Some(line) = answer.first_line() else {
                    return self.start_failed("it printed no readiness line at all");
                };
                let verdict = Verdict::parse(&line);
                // Remembered on every pass, including the retries, so an
                // `already-running` that is eventually accepted reports the
                // verdict it was accepted on.
                self.verdict = Some(verdict.to_string());
                match verdict {
                    Verdict::Error(reason) => self.start_failed(&reason),
                    // Re-tried and then accepted, because after a start the only
                    // thing it can mean is a loser of the socket-lock race
                    // overtaking the winner. What makes accepting it safe is the
                    // post-start identify below, which asks the session that is
                    // actually serving who it is.
                    Verdict::AlreadyRunning(_) if attempts + 1 < START_RETRIES => State::Start {
                        path,
                        attempts: attempts + 1,
                    },
                    _ => State::PostStart,
                }
            }
            State::PostStart => {
                let answer = match as_call(Stage::PostStart, outcome) {
                    Ok(answer) => answer,
                    Err(failure) => return self.fail_after_discard(failure),
                };
                match session_state(&target, Stage::PostStart, answer) {
                    Err(failure) => self.fail_after_discard(failure),
                    Ok(SessionState::Running { identity }) if identity.compatible() => {
                        self.session = Some(identity);
                        // **Only after an install.** The warning's whole subject
                        // is the file that was just written — "that shell's own
                        // roost-session won't be this one" — and in a start-only
                        // flow shed wrote nothing, so there is no "this one" to
                        // contrast with and the round trip buys nothing. roost
                        // computes it in `install()` alone for the same reason.
                        if self.installed() {
                            State::PathCheck
                        } else {
                            State::Hooks
                        }
                    }
                    Ok(SessionState::Running { identity }) => {
                        self.fail_after_discard(copy::post_start_refused(
                            &target,
                            &copy::session_identity_detail(&identity),
                            self.installed(),
                        ))
                    }
                    Ok(_) => self.start_failed("it reported ready and then no session was there"),
                }
            }
            State::PathCheck => {
                // Never a failure and never a dotfile edit (pin P5). A check
                // that could not RUN says nothing about the far side's PATH; a
                // check that ran and found nothing says plenty, because
                // `command -v` exits non-zero for "not found".
                if let (Ok(answer), Some(dest)) = (
                    as_exec(Stage::PostStart, outcome),
                    self.start_path.as_deref(),
                ) {
                    if answer.exit.is_some() {
                        let resolved = if answer.ok() {
                            answer.first_line()
                        } else {
                            None
                        };
                        self.path_warning = copy::path_warning(&target, dest, resolved.as_deref());
                    }
                }
                State::Hooks
            }
            State::Hooks => {
                // A hooks dialogue that refused is a missing enrichment, never a
                // failed bootstrap: the binary is installed and the session is
                // up. See `hooks.rs`.
                self.hooks = as_hooks(Stage::Hooks, outcome).ok();
                self.finish()
            }
            State::Rollback(staged, pending) => {
                // `rollback_script` prints `restored` on stdout, and only when
                // the rename actually happened: this side cannot tell "there was
                // no incumbent" from "the incumbent was put back" by looking at
                // `dest` afterwards, because both leave a regular file there.
                //
                // **And a rollback that would not run is its own answer**, not a
                // third spelling of "there was nothing to put back". Collapsing
                // the two — which is what reading only for the word `restored`
                // did — tells a user their old roost-session is back while it is
                // sitting at `<dest>.bak.<pid>` with nothing pointing at it.
                let rollback = match undo_answer(outcome) {
                    Err(undone) => Rollback::Failed(undone),
                    Ok(answer) if answer.first_line().as_deref() == Some("restored") => {
                        Rollback::Restored
                    }
                    Ok(_) => Rollback::Nothing,
                };
                State::Cleanup(
                    staged,
                    pending,
                    Undo {
                        rollback: Some(rollback),
                        cleanup_failed: None,
                    },
                )
            }
            State::Cleanup(staged, pending, mut undo) => {
                // Best-effort, like roost's: this runs on a path that is already
                // failing, and a cleanup that can fail loudly is one more failure
                // to classify.
                //
                // **Best-effort still has an outcome.** Every sentence this chain
                // can end with says the staged file was removed, because on every
                // other path it was; a cleanup that failed makes that clause
                // false, and the temporary is then a real file at a real path the
                // user may want to delete.
                undo.cleanup_failed = undo_answer(outcome).err();
                let failure = pending.into_failure(&target, Some(&staged), &undo);
                State::Done(Box::new(Err(failure)))
            }
            State::Done(result) => State::Done(result),
        }
    }

    /// The re-probe landed. Decide what to do, or refuse.
    fn plan_from(&mut self, probe: &Probe) -> State {
        let target = self.request.target.clone();
        // **The fingerprint gate, before anything else.** A host that changed
        // between the consent card and this moment is a host the user did not
        // consent to, and the sentence has to say that nothing was changed —
        // because nothing was.
        if probe.fingerprint != self.request.fingerprint {
            return State::Done(Box::new(Err(copy::fingerprint_changed(&target))));
        }
        // Whether a commit would have had an incumbent to move aside: rung 1 of
        // roost's ladder *is* the install destination, so a destination that
        // exists and is executable is a candidate the discovery script listed.
        // Only ever used to decide whether a sentence should name
        // `<dest>.bak.<pid>`; nothing turns on it that touches the filesystem.
        self.incumbent_at_dest = probe
            .install_dest()
            .is_some_and(|dest| probe.candidates.contains(&dest));
        let plan = Plan::for_probe(&target, probe);
        self.plan = Some(plan.clone());
        match &plan {
            // Nothing to do, and saying so is a success.
            Plan::UpToDate { identity } => {
                self.session = Some(identity.clone());
                self.finish()
            }
            // Pin P6: reported, never stopped and never restarted.
            Plan::Report { message, .. } => State::Done(Box::new(Err(BootstrapFailure::new(
                Stage::Report,
                message.clone(),
            )))),
            Plan::Start { path } => {
                self.start_path = Some(path.clone());
                State::Start {
                    path: path.clone(),
                    attempts: 0,
                }
            }
            Plan::Install { .. } | Plan::Update { .. } => {
                if self.source.is_none() {
                    return State::Done(Box::new(Err(copy::missing_source(&target))));
                }
                State::Prepare
            }
        }
    }
}

/// One `/bin/sh -s` step with a script on stdin — roost's convention, and every
/// script step in both machines.
///
/// The budget is an argument rather than a property of which helper the caller
/// reached for: the probe's steps only look, the install's also write, and the
/// difference between [`PROBE_BUDGET`] and [`INSTALL_BUDGET`] is worth seeing at
/// the call site.
fn script_step<T>(script: String, budget: std::time::Duration, stdout_cap: usize) -> Step<T> {
    Step::Exec {
        command: SH_STDIN.to_string(),
        stdin: Stdin::script(script),
        budget,
        stdout_cap,
        capture_stdout: true,
    }
}

/// The probe's first step, and the one an install re-probes with. One literal,
/// because two copies of it in two machines is two caps and two budgets to keep
/// in step by hand.
fn discovery_step<T>(jail_fs_root: bool) -> Step<T> {
    script_step(
        rb::discovery_script(jail_fs_root),
        PROBE_BUDGET,
        PROBE_STDOUT_CAP,
    )
}

/// The far side's own `command -v roost-session` — roost's self-contained
/// command, parsed by whatever shell the remote sshd hands it to.
///
/// Both machines ask it, for different reasons (the probe appends the answer to
/// the ladder; the install compares it against what it just wrote), and it is
/// the same step either way.
fn path_check_step<T>() -> Step<T> {
    Step::Exec {
        command: rb::path_check_command(),
        stdin: Stdin::Empty,
        budget: PROBE_BUDGET,
        stdout_cap: SMALL_STDOUT_CAP,
        capture_stdout: true,
    }
}

/// A best-effort undo step's answer, or the reason it did not work.
///
/// The three of them — rollback, cleanup, discard — ask the same question, and
/// both spellings of "it did not work" are answers rather than faults: a
/// non-zero exit and an outcome this side could not even classify each leave a
/// real file at a real path, which is a clause in the sentence the user reads.
/// The `Stage` never surfaces from here (every caller keeps the reason and drops
/// the rest), so it is this function's business rather than each call site's.
fn undo_answer(outcome: Outcome) -> Result<ExecAnswer, String> {
    match as_exec(Stage::PostCommit, outcome) {
        Ok(answer) if answer.ok() => Ok(answer),
        Ok(answer) => Err(answer.detail()),
        Err(failure) => Err(failure.message),
    }
}
