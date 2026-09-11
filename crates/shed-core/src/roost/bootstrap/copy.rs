//! What a user is told, and by whom.
//!
//! Two authors, and the split is deliberate.
//!
//! **roost writes the sentences that ARE the rollback promise.**
//! [`roost_ipc::bootstrap::BootstrapError::message`] is the copy that goes with
//! roost's install order — "nothing was written there", "the staged file was
//! removed and the existing install is unchanged", the commit message that
//! carefully does *not* claim an unchanged install because the rename may
//! already have landed. Shed keeps that order exactly (see the module doc), so
//! shed repeating those promises in its own words would be one paraphrase away
//! from promising something the order does not deliver. Those stages delegate.
//!
//! **shed writes the sentences where the two products genuinely differ.** The
//! protocol-only gate means shed refuses a binary for a different reason than
//! roost does and must say a different thing; pin P6's report names a session
//! shed will not touch; the Start copy points at `roost-session start` rather
//! than roost's `roostctl session start`, because a host shed just bootstrapped
//! has a `roost-session` on it and need not have a `roostctl`; and the
//! fingerprint-drift sentence is shed's own idea. Those are here, verbatim from
//! plan 019 §3.4 where it pinned them.
//!
//! Every one of them is asserted, by exact string, in this module's tests and in
//! the failure-injection table — copy nobody pins is copy that drifts.

use roost_ipc::bootstrap::{BootstrapError, InstallPhase};
use roost_ipc::messages::SESSION_PROTOCOL_VERSION;

/// Where a bootstrap stopped. Reaches the desktop as `error: {stage, message}`
/// (plan 019 §3.6) and Dart as a plain enum.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Stage {
    /// Looking at the far side — the exec, its output, or its shape.
    Probe,
    /// The far side is not Linux.
    UnsupportedOs,
    /// The far side's architecture has no build.
    UnsupportedArch,
    /// The host changed between the consent card and the install.
    Fingerprint,
    /// There were no bytes to install.
    Source,
    /// A session shed cannot talk to is serving, and pin P6 says shed reports it
    /// rather than touching it. A refusal, not a fault: the client should not
    /// have offered a button at all.
    Report,
    /// Deciding the destination and staging a temporary beside it.
    Prepare,
    /// Sending the binary.
    Stream,
    /// Asking the *staged* file who it is, before anything is replaced.
    Verify,
    /// Putting it in place.
    Commit,
    /// Asking the *installed* file who it is.
    PostCommit,
    /// `roost-session start`.
    Start,
    /// Asking the session that came up who it is.
    PostStart,
    /// The lease dialogue. Never fatal — see [`super::hooks`].
    Hooks,
}

impl Stage {
    /// A stable kebab name for the IPC payload and the logs.
    pub fn as_str(self) -> &'static str {
        match self {
            Stage::Probe => "probe",
            Stage::UnsupportedOs => "unsupported-os",
            Stage::UnsupportedArch => "unsupported-arch",
            Stage::Fingerprint => "fingerprint",
            Stage::Source => "source",
            Stage::Report => "report",
            Stage::Prepare => "prepare",
            Stage::Stream => "stream",
            Stage::Verify => "verify",
            Stage::Commit => "commit",
            Stage::PostCommit => "post-commit",
            Stage::Start => "start",
            Stage::PostStart => "post-start",
            Stage::Hooks => "hooks",
        }
    }
}

/// A bootstrap that stopped, and what to tell the person who asked for it.
///
/// Owned strings end to end (the FRB-mirror rule); `restored` is `Some` only
/// where the answer is knowable and changes what the user should do next.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct BootstrapFailure {
    pub stage: Stage,
    /// One user-facing sentence — or two. Never a `Debug` rendering.
    pub message: String,
    /// After a post-commit failure: was an incumbent put back? `None` where the
    /// question does not arise.
    pub restored: Option<bool>,
}

impl BootstrapFailure {
    pub fn new(stage: Stage, message: impl Into<String>) -> BootstrapFailure {
        BootstrapFailure {
            stage,
            message: message.into(),
            restored: None,
        }
    }

    /// roost's own copy for a failure roost's own type describes.
    pub fn from_roost(stage: Stage, error: &BootstrapError, target: &str) -> BootstrapFailure {
        BootstrapFailure::new(stage, error.message(target))
    }
}

impl std::fmt::Display for BootstrapFailure {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(&self.message)
    }
}

// ============================================================================
// Delegated to roost — these sentences are the rollback promise
// ============================================================================

/// A probe step that would not run, or answered with something its own script
/// could not have produced.
pub fn probe_failed(target: &str, detail: &str) -> BootstrapFailure {
    BootstrapFailure::from_roost(
        Stage::Probe,
        &BootstrapError::Probe(detail.to_string()),
        target,
    )
}

/// One of the three remote install steps roost names.
pub fn install_phase_failed(target: &str, phase: InstallPhase, detail: &str) -> BootstrapFailure {
    let stage = match phase {
        InstallPhase::Prepare => Stage::Prepare,
        InstallPhase::Stream => Stage::Stream,
        InstallPhase::Commit => Stage::Commit,
    };
    BootstrapFailure::from_roost(
        stage,
        &BootstrapError::Install {
            phase,
            detail: detail.to_string(),
        },
        target,
    )
}

/// The commit landed and then the installed file would not answer.
pub fn post_commit_failed(target: &str, detail: &str, restored: bool) -> BootstrapFailure {
    let mut failure = BootstrapFailure::from_roost(
        Stage::PostCommit,
        &BootstrapError::PostCommit {
            detail: detail.to_string(),
            restored,
        },
        target,
    );
    failure.restored = Some(restored);
    failure
}

/// The commit failed, and the incumbent it had already moved aside could not be
/// put back.
///
/// **Not roost's sentence, deliberately.** roost's commit copy ends "the staged
/// file was removed and any previous install there was put back", which is a
/// promise about an undo step whose own outcome roost's runtime checks and this
/// one used to throw away. When the undo did not work, repeating that sentence
/// and appending a correction would be two contradictory claims in one
/// paragraph; the whole sentence is rebuilt instead, and it ends with the path a
/// human needs to finish the job by hand.
pub fn commit_not_put_back(
    target: &str,
    detail: &str,
    backup: &str,
    dest: &str,
    undo: &str,
) -> BootstrapFailure {
    BootstrapFailure::new(
        Stage::Commit,
        format!(
            "couldn't put the new roost-session in place on {target}: {detail}. Shed then \
             couldn't put the previous install back either: {undo}. It is still on {target} at \
             {backup} — move it to {dest} to finish the job."
        ),
    )
}

/// The post-commit identify failed, and the incumbent could not be put back.
///
/// The same reasoning as [`commit_not_put_back`]: roost's `restored: false` arm
/// says "there was no previous install to fall back to", which is exactly the
/// wrong thing to say when there *was* one and the rename that would have
/// restored it is what failed.
pub fn post_commit_not_put_back(
    target: &str,
    detail: &str,
    backup: &str,
    dest: &str,
    undo: &str,
) -> BootstrapFailure {
    BootstrapFailure {
        stage: Stage::PostCommit,
        message: format!(
            "roost-session was put in place on {target} but wouldn't identify itself afterwards: \
             {detail}. Shed then couldn't put the previous install back: {undo}. It is still on \
             {target} at {backup} — move it to {dest} to finish the job."
        ),
        restored: Some(false),
    }
}

/// The cleanup that every failure path ends with did not work.
///
/// Appended to whatever sentence the failure already carries, and phrased as the
/// correction it is: the sentence it follows — roost's, in most cases — has
/// already said the staged file was removed, because on every other path it was.
pub fn cleanup_failed(target: &str, tmp: &str, detail: &str) -> String {
    format!("The staged file could not be removed after all: {detail}; it is still on {target} at {tmp}.")
}

/// The backup could not be discarded once the new install had answered.
///
/// **Not fatal and never treated as one** — a stale copy of the old binary beside
/// the new one is untidy, not broken. But it is reported, because it is also
/// exactly the state that leaves a `<dest>.bak.<pid>` on the host for a later
/// run to trip over (see the module doc's note on why shed will not restore a
/// backup this attempt did not make).
pub fn discard_failed(target: &str, backup: &str, detail: &str) -> String {
    format!(
        "Shed couldn't remove its copy of the previous roost-session on {target}: {detail}. It \
         is still at {backup}, and nothing will clean it up."
    )
}

/// A session appeared between the re-probe and the commit, and shed can talk to
/// it — so the install it was about to perform is both unnecessary and unsafe.
///
/// See the module doc: this narrows the post-probe race, it does not close it.
pub fn session_appeared(target: &str) -> BootstrapFailure {
    BootstrapFailure::new(
        Stage::Report,
        format!(
            "a roost-session started on {target} while shed was installing, and it is one this \
             shed can talk to — nothing was replaced; look again."
        ),
    )
}

/// The same race, with pin P6's session on the other end of it: the binary shed
/// was about to replace now has a process running out of it.
///
/// P6's sentence, word for word, plus the one fact it cannot carry on its own —
/// that an install was in flight and was abandoned.
pub fn session_appeared_mismatched(target: &str, theirs: u32) -> BootstrapFailure {
    BootstrapFailure::new(
        Stage::Report,
        format!(
            "{} Nothing was replaced on {target}.",
            protocol_report(target, theirs)
        ),
    )
}

/// The binary at the destination is not the binary shed staged.
///
/// Plan 019 §3.4 pins that two concurrent installers are **last-writer-wins**
/// and names the post-commit identify as the detector. It only *is* a detector
/// if it compares what it should: a protocol-only check passes happily on
/// somebody else's protocol-4 build, and shed would then discard its backup and
/// report that it installed bytes it never installed.
pub fn foreign_install(
    target: &str,
    dest: &str,
    staged: &super::Identity,
    found: &super::Identity,
    backup: Option<&str>,
) -> BootstrapFailure {
    let aftermath = match backup {
        Some(backup) => format!(
            " Shed left it alone rather than overwrite it, so its copy of the previous install \
             is still at {backup}."
        ),
        None => " Shed left it alone rather than overwrite it.".to_string(),
    };
    BootstrapFailure::new(
        Stage::PostCommit,
        format!(
            "the roost-session now at {dest} on {target} is not the one shed just staged: it \
             reports itself as roost-session {} (session protocol {}), and shed staged \
             roost-session {} (session protocol {}). Another install landed there at the same \
             moment.{aftermath}",
            found.app_version, found.session_protocol, staged.app_version, staged.session_protocol,
        ),
    )
}

/// `uname -m` named an architecture roost publishes no build for.
pub fn unsupported_arch(target: &str, arch: &str) -> BootstrapFailure {
    BootstrapFailure::from_roost(
        Stage::UnsupportedArch,
        &BootstrapError::UnsupportedArch(arch.to_string()),
        target,
    )
}

// ============================================================================
// shed's own
// ============================================================================

/// `uname -s` did not say Linux.
///
/// Shorter than roost's, and pinned by plan 019 §3.4: a macOS or BSD host is not
/// a failure anyone can fix, so the sentence says what is true and stops.
pub fn unsupported_os(target: &str, os: &str) -> BootstrapFailure {
    BootstrapFailure::new(
        Stage::UnsupportedOs,
        format!("{target} reports itself as {os}; roost-session is built for Linux only."),
    )
}

/// The re-probe disagreed with the probe the consent card was built from.
///
/// The whole value of the sentence is its second clause: a refusal that does not
/// say "nothing was changed" reads like a half-done install.
pub fn fingerprint_changed(target: &str) -> BootstrapFailure {
    BootstrapFailure::new(
        Stage::Fingerprint,
        format!("{target} changed since you were asked — nothing was changed; look again."),
    )
}

/// A plan that needs bytes, with no [`SourceHandle`](super::SourceHandle).
///
/// A client is supposed to have resolved a source *before* it showed a consent
/// card (plan 019 §3.5: the `NoSource` row has no button at all), so reaching
/// here is a client bug rather than a user's problem — but it still has to say
/// the one thing that matters.
pub fn missing_source(target: &str) -> BootstrapFailure {
    BootstrapFailure::new(
        Stage::Source,
        format!(
            "there was no roost-session to send to {target}, so nothing was sent. \
             {target} was left untouched."
        ),
    )
}

/// The staged bytes are not something shed can talk to.
///
/// roost's sentence for this says "isn't the build this Roost needs", which is
/// roost's rule and roost's voice. shed refuses on the protocol number alone, so
/// it says so — and keeps roost's promise about the filesystem word for word,
/// because roost's order is what makes that promise true.
pub fn staged_verify_refused(target: &str, detail: &str) -> BootstrapFailure {
    BootstrapFailure::new(
        Stage::Verify,
        format!(
            "the roost-session staged on {target} isn't one this shed can talk to: {detail}. \
             It was removed and {target}'s existing install was left exactly as it was."
        ),
    )
}

/// Why shed is refusing, in the one sentence fragment every gate refuses with.
///
/// There are three of them — the staged verify, the post-commit check and the
/// post-start identify — and the module doc's claim that shed applies *one* rule
/// at three gates is only worth anything if a user reading two of those
/// refusals sees the same reason. So the clause is written once.
fn protocol_clause(theirs: u32) -> String {
    format!("it speaks session protocol {theirs}, and this shed speaks {SESSION_PROTOCOL_VERSION}")
}

/// How a rejected *binary's* identity reads in the middle of a sentence — the
/// shape both the staged verify and the post-commit check refuse with.
pub fn identity_detail(found: Option<&super::Identity>) -> String {
    match found {
        Some(found) => format!(
            "{} (it reports itself as roost-session {})",
            protocol_clause(found.session_protocol),
            found.app_version
        ),
        None => "it would not identify itself at all".to_string(),
    }
}

/// The same clause about a *running session* — the post-start gate.
///
/// No parenthetical here: a session that answered `session.identify` is named
/// by the sentence around this one, and the version that matters to the reader
/// is the protocol number both sides disagree about.
pub fn session_identity_detail(found: &super::SessionIdentity) -> String {
    protocol_clause(found.session_protocol)
}

/// `roost-session start` refused, or the session it launched never answered.
///
/// `installed` is the whole point of the parameter list. After an install the
/// backup has already been discarded — there is nothing to roll back to and the
/// new binary is on the host, so the copy says that plainly and hands over the
/// one command that makes progress. In a start-only flow shed wrote nothing at
/// all, and claiming an install would be a lie.
pub fn start_failed(target: &str, detail: &str, installed: bool) -> BootstrapFailure {
    let message = if installed {
        format!(
            "roost-session was installed on {target} but wouldn't start: {detail}. \
             The new binary is in place — try `roost-session start` on {target}."
        )
    } else {
        format!(
            "roost-session on {target} wouldn't start: {detail}. Nothing was installed or \
             changed there — try `roost-session start` on {target}."
        )
    };
    BootstrapFailure::new(Stage::Start, message)
}

/// It started, and then the session that answered was not one shed can talk to.
pub fn post_start_refused(target: &str, detail: &str, installed: bool) -> BootstrapFailure {
    let aftermath = if installed {
        format!("The new binary is in place on {target}; nothing was rolled back.")
    } else {
        format!("Nothing was installed or changed on {target}.")
    };
    BootstrapFailure::new(
        Stage::PostStart,
        format!("the roost-session that came up on {target} isn't one this shed can talk to: {detail}. {aftermath}"),
    )
}

/// Pin P6's sentence: a session shed cannot talk to, which shed will not touch.
///
/// Every clause earns its place — the two protocol numbers so a reader can tell
/// which side is behind, "upgrade whichever is older" because shed does not know
/// which, and the stop command **for them to run**, because shed stopping
/// somebody's terminal multiplexer over a version number is exactly what P6
/// forbids.
pub fn protocol_report(target: &str, theirs: u32) -> String {
    format!(
        "roost-session on {target} speaks protocol {theirs}, this build speaks \
         {SESSION_PROTOCOL_VERSION} — upgrade whichever is older; stop it there with \
         `roostctl session stop` and reconnect once it is."
    )
}

/// The far side's own shell does not find what was just installed.
///
/// A warning and never a failure, and **never a dotfile edit** (pin P5): shed
/// execs the absolute path, so a `PATH` that misses the install costs the user
/// nothing until they type `roost-session` there themselves. Modelled on roost's
/// `path_warning`, which is private to its runtime.
pub fn path_warning(target: &str, dest: &str, resolved: Option<&str>) -> Option<String> {
    let dir = dest.rsplit_once('/').map_or(dest, |(dir, _)| dir);
    match resolved {
        Some(path) if path == dest => None,
        Some(other) => Some(format!(
            "a shell on {target} finds roost-session at {other}, not the {dest} that was just \
             installed — shed doesn't need its PATH, but that shell's own roost-session won't \
             be this one."
        )),
        None => Some(format!(
            "{dir} isn't on {target}'s PATH — shed doesn't need it, but roost-session won't be \
             runnable by name in a shell there."
        )),
    }
}

/// How an exec that failed reads in the middle of a sentence. roost's
/// `ExecOutcome::detail`, which is private to its runtime.
///
/// Deliberately terse: every message above already names the target and says
/// what to do next, so splicing a second piece of advice into the middle of one
/// produces a sentence that contradicts itself.
pub fn exec_detail(exit: Option<i32>, stderr_tail: &str) -> String {
    let line = stderr_tail
        .lines()
        .map(str::trim)
        .rfind(|line| !line.is_empty());
    match (exit, line) {
        (Some(code), Some(line)) => format!("it exited {code}: {line}"),
        (Some(code), None) => format!("it exited {code} with nothing on stderr"),
        // `None` is the budget, the transport, or a channel that closed with no
        // status — see the module doc. All three are "it did not finish".
        (None, Some(line)) => format!("it did not finish: {line}"),
        (None, None) => "it did not finish, and said nothing about why".to_string(),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    const TARGET: &str = "roost:popos/p019-a";

    /// The sentences plan 019 §3.4 pinned, pinned.
    #[test]
    fn the_plan_pinned_copy_is_word_for_word() {
        assert_eq!(
            unsupported_os(TARGET, "Darwin").message,
            "roost:popos/p019-a reports itself as Darwin; roost-session is built for Linux only."
        );
        assert_eq!(
            fingerprint_changed(TARGET).message,
            "roost:popos/p019-a changed since you were asked — nothing was changed; look again."
        );
        assert_eq!(
            protocol_report(TARGET, 2),
            "roost-session on roost:popos/p019-a speaks protocol 2, this build speaks 4 — \
             upgrade whichever is older; stop it there with `roostctl session stop` and \
             reconnect once it is."
        );
        assert!(
            start_failed(TARGET, "it exited 1: boom", true)
                .message
                .contains("try `roost-session start` on roost:popos/p019-a."),
            "the start failure names the command that makes progress"
        );
    }

    /// The two Start sentences differ in the one claim that matters.
    #[test]
    fn a_start_only_failure_never_claims_an_install() {
        let installed = start_failed(TARGET, "it exited 1", true).message;
        let start_only = start_failed(TARGET, "it exited 1", false).message;
        assert!(installed.contains("The new binary is in place"));
        assert!(start_only.contains("Nothing was installed or changed there"));
        assert!(!start_only.contains("was installed on"));
    }

    /// roost's promises arrive verbatim, including the commit sentence's
    /// careful refusal to claim an unchanged install.
    #[test]
    fn the_delegated_sentences_are_roosts_own() {
        let stream = install_phase_failed(TARGET, InstallPhase::Stream, "it exited 1: ENOSPC");
        assert_eq!(stream.stage, Stage::Stream);
        assert!(stream.message.contains("the existing install is unchanged"));

        let commit = install_phase_failed(TARGET, InstallPhase::Commit, "it exited 1");
        assert!(commit
            .message
            .contains("any previous install there was put back"));
        assert!(
            !commit.message.contains("unchanged"),
            "the commit copy must not claim an unchanged install — the rename may have landed"
        );

        let prepare = install_phase_failed(TARGET, InstallPhase::Prepare, "it exited 1");
        assert!(prepare.message.contains("Nothing was written there"));
    }

    /// `restored` reaches the copy and the field, and the two agree.
    #[test]
    fn a_post_commit_failure_says_which_way_it_went() {
        let restored = post_commit_failed(TARGET, "it would not identify itself at all", true);
        assert_eq!(restored.restored, Some(true));
        assert!(restored.message.contains("has been put back"));

        let kept = post_commit_failed(TARGET, "it would not identify itself at all", false);
        assert_eq!(kept.restored, Some(false));
        assert!(kept.message.contains("the new one is still there"));
    }

    /// The sentences an undo step's *failure* produces, word for word.
    ///
    /// These are the ones nobody sees in a good week, which is exactly why they
    /// are pinned: each replaces a roost sentence that would otherwise promise
    /// something that did not happen, and each has to end with a path a human
    /// can act on.
    #[test]
    fn an_undo_that_failed_names_the_path_a_human_has_to_act_on() {
        const BACKUP: &str = "/home/shed/.local/bin/roost-session.bak.4242";
        const TMP: &str = "/home/shed/.local/bin/roost-session.tmp.4242";
        const DEST: &str = "/home/shed/.local/bin/roost-session";

        let commit = commit_not_put_back(
            TARGET,
            "it exited 1: mv: boom",
            BACKUP,
            DEST,
            "it exited 1: mv: boom",
        );
        assert_eq!(
            commit.message,
            "couldn't put the new roost-session in place on roost:popos/p019-a: it exited 1: \
             mv: boom. Shed then couldn't put the previous install back either: it exited 1: \
             mv: boom. It is still on roost:popos/p019-a at \
             /home/shed/.local/bin/roost-session.bak.4242 — move it to \
             /home/shed/.local/bin/roost-session to finish the job."
        );
        assert_eq!(commit.stage, Stage::Commit);

        let post = post_commit_not_put_back(
            TARGET,
            "it would not identify itself at all",
            BACKUP,
            DEST,
            "it exited 1: mv: boom",
        );
        assert_eq!(post.restored, Some(false));
        assert!(
            !post
                .message
                .contains("There was no previous install to fall back to"),
            "there was one, and it is stranded: {}",
            post.message
        );
        assert!(post.message.ends_with(
            "Shed then couldn't put the previous install back: it exited 1: mv: boom. It is \
             still on roost:popos/p019-a at /home/shed/.local/bin/roost-session.bak.4242 — \
             move it to /home/shed/.local/bin/roost-session to finish the job."
        ));

        assert_eq!(
            cleanup_failed(TARGET, TMP, "it exited 1: rm: boom"),
            "The staged file could not be removed after all: it exited 1: rm: boom; it is \
             still on roost:popos/p019-a at /home/shed/.local/bin/roost-session.tmp.4242."
        );
        assert_eq!(
            discard_failed(TARGET, BACKUP, "it exited 1: rm: boom"),
            "Shed couldn't remove its copy of the previous roost-session on \
             roost:popos/p019-a: it exited 1: rm: boom. It is still at \
             /home/shed/.local/bin/roost-session.bak.4242, and nothing will clean it up."
        );
    }

    /// The two races' refusals: one is shed's own, the other is P6's pinned
    /// sentence with the one fact it cannot carry appended to it.
    #[test]
    fn the_race_refusals_say_that_nothing_was_replaced() {
        assert_eq!(
            session_appeared(TARGET).message,
            "a roost-session started on roost:popos/p019-a while shed was installing, and it \
             is one this shed can talk to — nothing was replaced; look again."
        );
        let mismatched = session_appeared_mismatched(TARGET, 2);
        assert_eq!(mismatched.stage, Stage::Report);
        assert!(
            mismatched.message.starts_with(&protocol_report(TARGET, 2)),
            "P6's sentence arrives word for word: {}",
            mismatched.message
        );
        assert!(mismatched
            .message
            .ends_with("Nothing was replaced on roost:popos/p019-a."));
    }

    /// Somebody else's build landed at the destination: both identities are
    /// named, and the sentence never claims an install.
    #[test]
    fn a_foreign_install_is_described_rather_than_claimed() {
        let staged = super::super::Identity {
            app_version: "0.0.19".into(),
            session_protocol: 4,
            libghostty_build: "ghostty-ours".into(),
        };
        let found = super::super::Identity {
            app_version: "0.0.20".into(),
            session_protocol: 4,
            libghostty_build: "ghostty-theirs".into(),
        };
        let failure = foreign_install(
            TARGET,
            "/home/shed/.local/bin/roost-session",
            &staged,
            &found,
            Some("/home/shed/.local/bin/roost-session.bak.4242"),
        );
        assert_eq!(failure.stage, Stage::PostCommit);
        assert_eq!(
            failure.message,
            "the roost-session now at /home/shed/.local/bin/roost-session on \
             roost:popos/p019-a is not the one shed just staged: it reports itself as \
             roost-session 0.0.20 (session protocol 4), and shed staged roost-session 0.0.19 \
             (session protocol 4). Another install landed there at the same moment. Shed left \
             it alone rather than overwrite it, so its copy of the previous install is still \
             at /home/shed/.local/bin/roost-session.bak.4242."
        );
        assert!(
            foreign_install(TARGET, "/x", &staged, &found, None)
                .message
                .ends_with("Shed left it alone rather than overwrite it."),
            "with no incumbent there is no backup to name"
        );
    }

    /// One rule at three gates reads as one reason at three gates.
    ///
    /// The post-start refusal used to build this clause itself, in
    /// `machines.rs`, with its own reference to `SESSION_PROTOCOL_VERSION` — so
    /// a reword here would have left that one refusal phrased the old way and
    /// no test would have noticed.
    #[test]
    fn every_gate_refuses_with_the_same_protocol_clause() {
        let binary = super::super::Identity {
            app_version: "0.0.19".into(),
            session_protocol: 2,
            libghostty_build: "ghostty-older".into(),
        };
        let session = super::super::SessionIdentity {
            app_version: "0.0.19".into(),
            session_protocol: 2,
            libghostty_build: "ghostty-older".into(),
            session_id: "s-1".into(),
            started_at: "2026-09-11T10:00:00Z".into(),
        };
        assert_eq!(
            session_identity_detail(&session),
            "it speaks session protocol 2, and this shed speaks 4"
        );
        assert!(
            identity_detail(Some(&binary)).starts_with(&session_identity_detail(&session)),
            "the binary gate says the same thing and then names the build"
        );
        assert!(
            identity_detail(Some(&binary)).ends_with("(it reports itself as roost-session 0.0.19)")
        );
        assert_eq!(identity_detail(None), "it would not identify itself at all");
    }

    #[test]
    fn the_path_warning_is_silent_only_when_the_shell_agrees() {
        let dest = "/home/shed/.local/bin/roost-session";
        assert_eq!(path_warning(TARGET, dest, Some(dest)), None);
        assert!(path_warning(TARGET, dest, Some("/usr/bin/roost-session"))
            .expect("a different answer warns")
            .contains("/usr/bin/roost-session"));
        assert!(path_warning(TARGET, dest, None)
            .expect("no answer warns")
            .contains("/home/shed/.local/bin isn't on"));
    }

    #[test]
    fn an_exec_detail_reads_the_last_stderr_line() {
        assert_eq!(
            exec_detail(Some(1), "warming up\nmkdir: Permission denied\n"),
            "it exited 1: mkdir: Permission denied"
        );
        assert_eq!(
            exec_detail(Some(127), ""),
            "it exited 127 with nothing on stderr"
        );
        assert_eq!(
            exec_detail(None, ""),
            "it did not finish, and said nothing about why"
        );
    }

    #[test]
    fn every_stage_has_a_distinct_kebab_name() {
        let stages = [
            Stage::Probe,
            Stage::UnsupportedOs,
            Stage::UnsupportedArch,
            Stage::Fingerprint,
            Stage::Source,
            Stage::Report,
            Stage::Prepare,
            Stage::Stream,
            Stage::Verify,
            Stage::Commit,
            Stage::PostCommit,
            Stage::Start,
            Stage::PostStart,
            Stage::Hooks,
        ];
        let names: std::collections::BTreeSet<&str> =
            stages.iter().map(|stage| stage.as_str()).collect();
        assert_eq!(names.len(), stages.len());
    }
}
