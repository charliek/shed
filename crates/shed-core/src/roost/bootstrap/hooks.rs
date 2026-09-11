//! The step that makes the payoff true (plan 019 §3.4).
//!
//! Installing and starting a `roost-session` on a shed gets shed a *terminal
//! multiplexer* it can read. It does not, on its own, get it **agent activity** —
//! roost knows what codex is doing because codex's hook reports it, and the
//! hooks are dotfile entries that have to exist on that host. Wiring them is one
//! op: `session.set_agent_hooks`, lease-gated, served by the host session, which
//! links `roost-agent-install` and does the writes itself.
//!
//! So: **nothing shed does edits a dotfile.** Shed asks; the host session writes,
//! under its own `$HOME`, with roost's own installer. That is pin P5 kept intact
//! and it is worth saying out loud on the consent card, because the user is
//! consenting to a file under their home directory changing.
//!
//! ## The lease is a bearer token, and shed keeps it
//!
//! `session.set_agent_hooks` is lease-gated, so the dialogue is two ops:
//! `session.connect {takeover: false, client_label}` to mint one, then the op
//! itself. Three facts about roost's lease shape everything here, and the first
//! draft of this plan got all three wrong:
//!
//! 1. **The lease outlives its connections.** It is not per-connection state; a
//!    reconnect is a *takeover*. So shed mints it once per target and keeps it in
//!    memory for the app run (`shed-app`'s table, plan 019 C6), re-sending
//!    `set_agent_hooks` on every watcher reconnect while it is still valid — the
//!    way roost's own UI re-sends it on every connect, because the op is
//!    idempotent and a config change has to reach the host somehow.
//! 2. **`takeover: false` against ANY live lease answers `already-connected`** —
//!    including one this very connection holds. So that code never means "you
//!    already have it"; it means *somebody is driving*, and the only correct
//!    response is to step back. [`wire_agent_hooks`] returns
//!    [`HooksResult::skipped_code`] `already-connected` and does nothing else.
//! 3. **`taken-over` is terminal for this lease.** Whoever took it will wire the
//!    hooks themselves — that is what every roost client does on connect — so
//!    shed stops re-sending rather than fighting for it.
//!
//! **shed never takes over.** There is no code path here that passes
//! `takeover: true`, and there is a test that says so.
//!
//! ## What `mode: auto` actually does, and when it recurs
//!
//! `auto` wires **only agents whose config directory already exists**
//! (`roost-agent-install`'s `home.rs`). An agent a user sets up tomorrow is not
//! wired by today's call — it is wired the next time a session starts (every
//! `shed start` is one) or the next time any roost client connects. That is why
//! the consent copy names the agents rather than promising "your agents", and
//! why this is worth documenting on the S5 docs page rather than leaving as a
//! surprise.
//!
//! `wired` in the result is roost's **first-announcement** list — the agents this
//! host has wired and never told any client about — not the set this call wrote.
//! `refreshed` covers the rest. Shed reports both and never treats an empty
//! `wired` as a failure.
//!
//! ## Failures here are never fatal
//!
//! The install worked, the session is up, the host is readable. A hooks dialogue
//! that refused is a missing *enrichment*, and failing the whole bootstrap over
//! it would throw away the part that succeeded. Everything below lands in
//! [`HooksResult`], which rides out on the success value.

use roost_ipc::client::ServerCode;
use roost_ipc::messages::{AgentHooksMode, SessionSetAgentHooksResult};

use crate::roost::{Conn, RoostError};

/// One agent the host did not act on, and why.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct HooksSkip {
    pub agent: String,
    pub reason: String,
}

/// One agent the host tried to wire and could not. Reported, never swallowed.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct HooksError {
    pub agent: String,
    pub error: String,
}

/// What the lease dialogue came to. Owned end to end — it crosses to Dart.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct HooksResult {
    /// The label shed connected under. Echoed by roost as
    /// `session.driver_changed.taken_by` if anyone ever displaces it, which is
    /// how a user finds out who is holding their session.
    pub client_label: String,
    /// The bearer token, **while it is still good**. The caller keeps this in
    /// memory per target and re-sends `set_agent_hooks` with it on every
    /// reconnect; `None` means stop.
    pub lease: Option<String>,
    /// Why nothing was wired, when nothing was: `already-connected` (somebody
    /// else drives) or `taken-over` (somebody else took it mid-dialogue).
    pub skipped_code: Option<String>,
    /// roost's first-announcement list — the agents this host has wired and
    /// never announced. Not "what this call wrote".
    pub wired: Vec<String>,
    pub refreshed: Vec<String>,
    pub removed: Vec<String>,
    pub skipped: Vec<HooksSkip>,
    /// Per-agent failures. Partial success is the normal case and is shown.
    pub errors: Vec<HooksError>,
    /// The dialogue itself failed — a dead connection, a refusal with no code
    /// shed knows. Never fatal to the bootstrap.
    pub error: Option<String>,
}

impl HooksResult {
    /// Whether `session.set_agent_hooks` actually ran.
    pub fn applied(&self) -> bool {
        self.skipped_code.is_none() && self.error.is_none()
    }

    fn skipped(client_label: &str, code: &str) -> HooksResult {
        HooksResult {
            client_label: client_label.to_string(),
            skipped_code: Some(code.to_string()),
            ..HooksResult::default()
        }
    }

    fn failed(client_label: &str, error: String) -> HooksResult {
        HooksResult {
            client_label: client_label.to_string(),
            error: Some(error),
            ..HooksResult::default()
        }
    }
}

impl From<SessionSetAgentHooksResult> for HooksResult {
    fn from(result: SessionSetAgentHooksResult) -> HooksResult {
        HooksResult {
            wired: result.wired,
            refreshed: result.refreshed,
            removed: result.removed,
            skipped: result
                .skipped
                .into_iter()
                .map(|skip| HooksSkip {
                    agent: skip.agent,
                    reason: skip.reason,
                })
                .collect(),
            errors: result
                .errors
                .into_iter()
                .map(|failed| HooksError {
                    agent: failed.agent,
                    error: failed.error,
                })
                .collect(),
            ..HooksResult::default()
        }
    }
}

/// Mint a lease if there isn't one, then wire the host's agent hooks.
///
/// **The one implementation of the dialogue**, called by both clients over
/// whichever `Conn` they have — that is the point of putting it here instead of
/// in each client's runner. `Step::Hooks` is the sans-IO machine's way of saying
/// "call this now"; on mobile it is called on the Rust side, on the desktop by
/// the app layer, and neither writes its own version of the lease table above.
///
/// `lease` is the one the caller is holding from a previous call, if any. A
/// cached lease that has been forgotten by the far side (`connect-required` —
/// roost keeps exactly one tombstone, so a lease displaced twice is forgotten)
/// earns **one** fresh connect and one retry; anything beyond that is somebody
/// else's session and shed leaves it alone.
pub async fn wire_agent_hooks(
    conn: &mut Conn,
    client_label: &str,
    lease: Option<&str>,
) -> HooksResult {
    let mut cached = lease.map(str::to_string);
    // At most two passes: one with whatever the caller had, one with a freshly
    // minted lease. A third would be a loop against a session that is being
    // fought over, and shed is not a participant in that fight.
    for pass in 0..2 {
        let minted = cached.is_none();
        let lease = match cached.take() {
            Some(lease) => lease,
            None => match conn.session_connect(false, Some(client_label)).await {
                Ok(result) => result.lease,
                Err(RoostError::Server { code, .. })
                    if ServerCode::from_wire(&code) == ServerCode::AlreadyConnected =>
                {
                    // Somebody is driving. They wire the hooks; shed never
                    // takes a lease away to do it.
                    return HooksResult::skipped(client_label, &code);
                }
                Err(error) => return HooksResult::failed(client_label, error.to_string()),
            },
        };

        match conn
            .session_set_agent_hooks(&lease, AgentHooksMode::Auto, &[], client_label)
            .await
        {
            Ok(result) => {
                let mut result = HooksResult::from(result);
                result.client_label = client_label.to_string();
                result.lease = Some(lease);
                return result;
            }
            Err(RoostError::Server { code, .. })
                if ServerCode::from_wire(&code) == ServerCode::TakenOver =>
            {
                // Terminal: the new driver wires them. Dropping the lease is
                // what stops the caller re-sending on every reconnect.
                return HooksResult::skipped(client_label, &code);
            }
            Err(RoostError::Server { code, .. })
                if ServerCode::from_wire(&code) == ServerCode::ConnectRequired
                    && !minted
                    && pass == 0 =>
            {
                // The cached lease was forgotten (displaced twice). Re-mint and
                // try once — the connect itself is what decides whether anyone
                // else is holding it now.
                continue;
            }
            Err(RoostError::Server { code, message }) => {
                return HooksResult::failed(client_label, format!("{code}: {message}"));
            }
            Err(error) => return HooksResult::failed(client_label, error.to_string()),
        }
    }
    HooksResult::failed(
        client_label,
        "the interactive lease could not be established".to_string(),
    )
}
