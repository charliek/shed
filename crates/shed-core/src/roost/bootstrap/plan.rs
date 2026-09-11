//! What a probe concluded, and the one button that follows from it.
//!
//! Two halves, both pure:
//!
//! * the **DTOs** a probe answers with — [`Probe`] and the fielded enums under
//!   it, hand-mirrored into Dart (see the module doc's FRB section);
//! * the **plan matrix** ([`Plan::for_probe`]) — roost's six rows with pin P6
//!   applied, which is what a client turns into a status line and a button.

use roost_ipc::messages::{SessionIdentify, SESSION_PROTOCOL_VERSION};
use sha2::{Digest, Sha256};

/// A `roost-session` binary's own account of itself — `roost-session identify`,
/// one JSON line.
///
/// An owned mirror of [`roost_ipc::messages::SessionBinaryIdentity`], because
/// this crosses to Dart and roost's type is roost's to change.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Identity {
    pub app_version: String,
    pub session_protocol: u32,
    pub libghostty_build: String,
}

impl Identity {
    /// Whether shed can talk to a binary that identifies like this.
    ///
    /// **The protocol number and nothing else** — see the module doc for why
    /// shed cannot apply roost's exact-triple rule, and what it costs.
    pub fn compatible(&self) -> bool {
        self.session_protocol == SESSION_PROTOCOL_VERSION
    }

    /// Parse one `identify` line, roost's way: unparseable or absent degrades to
    /// "no identity" rather than to an error, because the overwhelmingly common
    /// cause is a binary too old to know the subcommand — and the honest reading
    /// of that is "needs an upgrade", not "the probe failed".
    pub fn parse(stdout: &str) -> Option<Identity> {
        roost_ipc::bootstrap::parse_identity_line(stdout).map(|found| Identity {
            app_version: found.app_version,
            session_protocol: found.session_protocol,
            libghostty_build: found.libghostty_build,
        })
    }
}

/// The **running** session's account of itself — `session.identify` over the
/// wire, which is a different question from [`Identity`]'s: one is a file, the
/// other is a process.
///
/// roost's [`SessionIdentify`] carries two open lists shed has no use for here
/// (`payload_kinds` is attach negotiation, which shed never does, and `features`
/// is read by the watcher on its own connection). They are dropped rather than
/// mirrored: a DTO that crosses to Dart carrying a list of enum values nothing
/// reads is a schema two languages then have to keep in step for nothing.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct SessionIdentity {
    pub app_version: String,
    pub session_protocol: u32,
    pub libghostty_build: String,
    pub session_id: String,
    pub started_at: String,
}

impl SessionIdentity {
    pub fn compatible(&self) -> bool {
        self.session_protocol == SESSION_PROTOCOL_VERSION
    }
}

impl From<SessionIdentify> for SessionIdentity {
    fn from(identify: SessionIdentify) -> SessionIdentity {
        SessionIdentity {
            app_version: identify.app_version,
            session_protocol: identify.session_protocol,
            libghostty_build: identify.libghostty_build,
            session_id: identify.session_id,
            started_at: identify.started_at,
        }
    }
}

/// What the probe concluded about the binaries on the far side.
///
/// Shaped like [`roost_ipc::bootstrap::ProbeOutcome`], with shed's
/// compatibility rule and an owned identity.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum ProbeOutcome {
    /// A binary shed can talk to, at `path`.
    Compatible { path: String, identity: Identity },
    /// A `roost-session` is there and shed cannot talk to it. `identity` is
    /// `None` when it would not identify itself at all — a build older than the
    /// `identify` subcommand. Either way the answer is the same offer, so the
    /// distinction is for copy and logs, not for routing.
    Mismatch {
        path: String,
        identity: Option<Identity>,
    },
    /// No rung of the ladder exists.
    Missing,
}

impl ProbeOutcome {
    /// The rung a start-only flow would start from.
    pub fn path(&self) -> Option<&str> {
        match self {
            ProbeOutcome::Compatible { path, .. } | ProbeOutcome::Mismatch { path, .. } => {
                Some(path)
            }
            ProbeOutcome::Missing => None,
        }
    }
}

/// Whether anything is *serving* over there, which the on-disk probe cannot
/// answer.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum SessionState {
    /// A session answered `session.identify`. Its protocol may or may not be
    /// shed's — the plan matrix is what turns on that, not this.
    Running { identity: SessionIdentity },
    /// A `roost-session` is installed and is not running: the bridge exec found
    /// a binary and it said `client-bridge: no session`.
    NoSession,
    /// The bridge exec fell off the end of the ladder — exit 127.
    NotInstalled,
}

/// One read-only look at a host. Nothing here changed anything.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Probe {
    pub outcome: ProbeOutcome,
    /// `amd64` or `arm64` — the spelling roost's release assets use.
    pub arch: String,
    /// The remote's own `$HOME`, verbatim and untrimmed (a `$HOME` with a
    /// trailing space is a real directory). Its one job is to expand the
    /// install destination the far side would write, so a consent card can tell
    /// "the rung I found IS the file I am about to overwrite" from "a different
    /// file that will be shadowed".
    pub home: String,
    pub session: SessionState,
    /// Every ladder rung that exists and is executable, in ladder order, plus
    /// the remote shell's own `command -v` hit when that is something the ladder
    /// did not already find.
    pub candidates: Vec<String>,
    /// What the user consented against. See [`fingerprint`].
    pub fingerprint: String,
}

impl Probe {
    /// Where an install would land: the far side's own `$HOME` plus roost's
    /// fixed suffix. `None` when the remote reported no `$HOME` — in which case
    /// the prepare script will refuse too, and for the same reason.
    pub fn install_dest(&self) -> Option<String> {
        if self.home.is_empty() {
            return None;
        }
        Some(format!(
            "{}{}",
            self.home,
            roost_ipc::bootstrap::INSTALL_DEST_SUFFIX
        ))
    }
}

/// The hash a consent card carries and an install re-checks.
///
/// Over **(target, arch, `$HOME`, the outcome's path and identity, the session
/// state)** — plan 019 §3.4 — and deliberately **not** over the full candidate
/// list: what it exists to catch is the host materially changing between "may
/// I?" and "doing it", and a fingerprint that also moved when a second nix
/// profile appeared would refuse installs for no reason.
///
/// **`$HOME` is in it because `$HOME` decides the destination.** The consent
/// card names a path — `~/.local/bin/roost-session`, expanded — and
/// [`prepare_script`](roost_ipc::bootstrap::prepare_script) re-derives that path
/// from whatever `$HOME` the far side reports at install time. An account whose
/// home moved between the two moments (a remounted `/home`, a changed passwd
/// entry, a different user behind the same target name) has the same arch and
/// the same session state, so without this the fingerprint would match and shed
/// would write to a path nobody was ever shown.
///
/// **A running session contributes its `session_id` *and* its `started_at`.** A
/// session that stopped and started again between consent and install is a
/// different session — different tabs, different agents, possibly a different
/// binary behind it — and consent given against the first one is not consent
/// against the second. `session_id` alone is roost's identifier and ought to
/// move on a restart, but "ought to" is the far side's promise about a value
/// shed only reads; `started_at` is the one field whose whole job is to say
/// when this process began, so it costs a few bytes to stop depending on that
/// promise.
pub fn fingerprint(
    target: &str,
    arch: &str,
    home: &str,
    outcome: &ProbeOutcome,
    session: &SessionState,
) -> String {
    // Field-tagged and newline-delimited, so no two different states can
    // serialize to the same bytes by concatenation.
    let mut text = String::new();
    text.push_str("target\t");
    text.push_str(target);
    text.push_str("\narch\t");
    text.push_str(arch);
    text.push_str("\nhome\t");
    text.push_str(home);
    text.push_str("\noutcome\t");
    match outcome {
        ProbeOutcome::Missing => text.push_str("missing"),
        ProbeOutcome::Compatible { path, identity } => {
            text.push_str("compatible\t");
            text.push_str(path);
            push_identity(&mut text, Some(identity));
        }
        ProbeOutcome::Mismatch { path, identity } => {
            text.push_str("mismatch\t");
            text.push_str(path);
            push_identity(&mut text, identity.as_ref());
        }
    }
    text.push_str("\nsession\t");
    match session {
        SessionState::NotInstalled => text.push_str("not-installed"),
        SessionState::NoSession => text.push_str("no-session"),
        SessionState::Running { identity } => {
            text.push_str("running\t");
            text.push_str(&identity.session_id);
            text.push('\t');
            text.push_str(&identity.started_at);
            text.push('\t');
            text.push_str(&identity.session_protocol.to_string());
            text.push('\t');
            text.push_str(&identity.app_version);
        }
    }
    text.push('\n');

    let digest = Sha256::digest(text.as_bytes());
    digest.iter().fold(String::with_capacity(64), |mut out, b| {
        use std::fmt::Write as _;
        let _ = write!(out, "{b:02x}");
        out
    })
}

fn push_identity(text: &mut String, identity: Option<&Identity>) {
    match identity {
        None => text.push_str("\t-"),
        Some(identity) => {
            text.push('\t');
            text.push_str(&identity.app_version);
            text.push('\t');
            text.push_str(&identity.session_protocol.to_string());
            text.push('\t');
            text.push_str(&identity.libghostty_build);
        }
    }
}

/// Turn [`roost_ipc::bootstrap::identity_script`]'s pairs into an outcome.
///
/// **The verdict is always about the first pair**, because that is the rung
/// roost's `exec_chain_command` will exec. Nothing deeper in the ladder can
/// change it: a compatible binary further down is shadowed by whatever is above
/// it, and calling the host compatible on the strength of a rung that never runs
/// would offer no fix for a problem the user still has.
///
/// This is exactly the case that needs care, because `identity_script` stops at
/// the first candidate that *answers*: a rung too old to know `identify` emits
/// an empty second field and the loop continues, so a match lands past
/// `pairs[0]` precisely when `pairs[0]` is the stale binary the transport is
/// about to exec. Reporting `Compatible` there would offer no install, forever.
///
/// roost's rule, therefore; shed's [`Identity::compatible`] as the test.
pub fn classify_candidates(pairs: &[(String, String)]) -> ProbeOutcome {
    let Some((path, stdout)) = pairs.first() else {
        return ProbeOutcome::Missing;
    };
    match Identity::parse(stdout) {
        Some(identity) if identity.compatible() => ProbeOutcome::Compatible {
            path: path.clone(),
            identity,
        },
        identity => ProbeOutcome::Mismatch {
            path: path.clone(),
            identity,
        },
    }
}

/// The one action a probe implies — roost's six-row matrix with pin P6 applied.
///
/// | probe | session | plan |
/// |---|---|---|
/// | `Missing` | `NoSession` / `NotInstalled` | [`Plan::Install`] then start |
/// | `Mismatch` | `NoSession` / `NotInstalled` | [`Plan::Update`] then start |
/// | `Compatible` | `NoSession` / `NotInstalled` | [`Plan::Start`] |
/// | any | `Running`, protocol 4 | [`Plan::UpToDate`] — status only |
/// | any | `Running`, protocol ≠ 4 | [`Plan::Report`] — **never stopped, never restarted** |
///
/// The sixth row — "the source preview is `NoSource`, so there is no button at
/// all" — is deliberately **not** a variant here. A plan is what the *host*
/// implies; whether shed has any bytes to send is what the *source ladder*
/// (plan 019 C5) implies, and the client overlays the second on the first at
/// preview time. Folding them together would make `Plan` un-computable without a
/// network fetch, which is exactly what consent has to happen before.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Plan {
    /// Nothing is there. Install, then start.
    Install {
        /// Where it will land, when the remote reported a `$HOME`.
        dest: Option<String>,
    },
    /// Something is there that shed cannot talk to. Back it up, replace it,
    /// then start.
    Update {
        /// The rung that would be shadowed — informational; an install always
        /// lands on rung 1.
        path: String,
        incumbent: Option<Identity>,
        /// The incumbent speaks a **newer** protocol than this build. Then this
        /// is a downgrade wearing an update's clothes, and the copy says so
        /// rather than letting a user find out afterwards.
        replaces_newer: bool,
        dest: Option<String>,
    },
    /// A binary shed can talk to is there and nothing is serving. Just start it.
    Start { path: String },
    /// A session shed can talk to is already serving. Nothing to do.
    UpToDate { identity: SessionIdentity },
    /// A session is serving and shed cannot talk to it.
    ///
    /// **P6: reported, never stopped and never restarted.** Somebody is using
    /// that session — it is their terminal multiplexer, with their processes in
    /// it — and shed's opinion about its protocol number is not a reason to take
    /// it away from them. The copy names the mismatch and the command *they*
    /// would run.
    Report { protocol: u32, message: String },
}

impl Plan {
    /// The row this probe lands on.
    pub fn for_probe(target: &str, probe: &Probe) -> Plan {
        // A running session wins over anything on disk: the question a user is
        // actually asking is "can I read this host", and a session that is
        // already answering has answered it.
        if let SessionState::Running { identity } = &probe.session {
            if identity.compatible() {
                return Plan::UpToDate {
                    identity: identity.clone(),
                };
            }
            return Plan::Report {
                protocol: identity.session_protocol,
                message: super::copy::protocol_report(target, identity.session_protocol),
            };
        }

        match &probe.outcome {
            ProbeOutcome::Missing => Plan::Install {
                dest: probe.install_dest(),
            },
            ProbeOutcome::Mismatch { path, identity } => Plan::Update {
                path: path.clone(),
                incumbent: identity.clone(),
                replaces_newer: identity
                    .as_ref()
                    .is_some_and(|found| found.session_protocol > SESSION_PROTOCOL_VERSION),
                dest: probe.install_dest(),
            },
            ProbeOutcome::Compatible { path, .. } => Plan::Start { path: path.clone() },
        }
    }

    /// Whether acting on this plan needs a [`SourceHandle`](super::SourceHandle)
    /// — i.e. whether bytes cross the wire.
    pub fn needs_source(&self) -> bool {
        matches!(self, Plan::Install { .. } | Plan::Update { .. })
    }

    /// Whether acting on this plan does anything at all.
    pub fn actionable(&self) -> bool {
        matches!(
            self,
            Plan::Install { .. } | Plan::Update { .. } | Plan::Start { .. }
        )
    }

    /// A stable kebab name, for an IPC payload and a log line.
    pub fn as_str(&self) -> &'static str {
        match self {
            Plan::Install { .. } => "install",
            Plan::Update { .. } => "update",
            Plan::Start { .. } => "start",
            Plan::UpToDate { .. } => "up-to-date",
            Plan::Report { .. } => "report",
        }
    }
}
