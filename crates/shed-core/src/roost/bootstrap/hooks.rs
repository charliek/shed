//! The step that makes the payoff true (plan 019 §3.4).
//!
//! Installing and starting a `roost-session` on a shed gets shed a *terminal
//! multiplexer* it can read. It does not, on its own, get it **agent activity** —
//! roost knows what codex is doing because codex's hook reports it, and the
//! hooks are dotfile entries that have to exist on that host. Wiring them is one
//! op: `session.set_agent_hooks`, served by the host session, which links
//! `roost-agent-install` and does the writes itself.
//!
//! So: **nothing shed does edits a dotfile.** Shed asks; the host session writes,
//! under its own `$HOME`, with roost's own installer. That is pin P5 kept intact
//! and it is worth saying out loud on the consent card, because the user is
//! consenting to a file under their home directory changing.
//!
//! ## One op, no token, last writer wins
//!
//! At session protocol 5 this op is **open to every same-UID client**: roost
//! deleted the lease that used to gate it, and with it the two-op dialogue this
//! module was built around. So [`wire_agent_hooks`] is one wire call with no
//! retry loop and nothing to carry between calls.
//!
//! What makes that safe is that the op is **declarative rather than
//! incremental**. shed sends one request and only ever that request —
//! `{mode: "auto", skip: [], client: "shed-desktop"|"shed-mobile"}` — and the
//! host brings its hook entries in line with it. The destructive direction is
//! `mode: "off"`, which shed has no code path that sends. A connection that died
//! mid-call is therefore a call that did not land, and the next watcher cycle
//! re-sends the identical request.
//!
//! **The hook entries are idempotent; roost's own bookkeeping is not.** Ten
//! calls leave the same lines in the same agent config files, but
//! `roost-agent-install`'s state record rewrites `wired_at` and `by` every time,
//! so `by` flips between labels while a desktop and a phone are both running.
//! That churn is roost's metadata about the write, not the user's dotfile
//! content, and "who wired these last" changing is precisely what that field is
//! for — `roostctl agent status` on the host is where a user reads it.
//!
//! What genuinely changed with the lease: if a roost UI on that host had turned
//! hooks off or excluded an agent, shed's next reconnect switches it back on. At
//! protocol 4 shed would have seen `already-connected` and stepped back. One
//! user owns every client, and shed's `auto` only ever wires what is already
//! configured there.
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
//! The install worked, the session is up, the host is readable. A hooks call that
//! refused is a missing *enrichment*, and failing the whole bootstrap over it
//! would throw away the part that succeeded. Everything below lands in
//! [`HooksResult`], which rides out on the success value.

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

/// What the call came to. Owned end to end — it crosses to Dart.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct HooksResult {
    /// The label shed sent as `client`. roost records it as the `by` of the
    /// state entry, which is how a user finds out which of their clients wired
    /// these hooks last (`roostctl agent status` on the host).
    pub client_label: String,
    /// roost's first-announcement list — the agents this host has wired and
    /// never announced. Not "what this call wrote".
    pub wired: Vec<String>,
    pub refreshed: Vec<String>,
    pub removed: Vec<String>,
    pub skipped: Vec<HooksSkip>,
    /// Per-agent failures. Partial success is the normal case and is shown.
    pub errors: Vec<HooksError>,
    /// The call itself failed — a dead connection, a refusal shed has no
    /// narrower name for. Never fatal to the bootstrap.
    pub error: Option<String>,
}

impl HooksResult {
    /// Whether `session.set_agent_hooks` actually ran.
    ///
    /// One condition, because there is one way to not run it now: at protocol 4
    /// a live lease elsewhere was a *third* outcome, neither applied nor failed,
    /// and that outcome no longer exists on the wire.
    pub fn applied(&self) -> bool {
        self.error.is_none()
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

/// Wire the host's agent hooks. One op.
///
/// **The one implementation of the call**, invoked by both clients over
/// whichever `Conn` they have — that is the point of putting it here instead of
/// in each client's runner. `Step::Hooks` is the sans-IO machine's way of saying
/// "call this now"; on mobile it is called on the Rust side, on the desktop by
/// the app layer, and neither writes its own version of it.
///
/// There is nothing to retry and nothing to carry: a failure means the request
/// did not land, and the next successful watcher cycle sends the same one again.
pub async fn wire_agent_hooks(conn: &mut Conn, client_label: &str) -> HooksResult {
    match conn
        .session_set_agent_hooks(AgentHooksMode::Auto, &[], client_label)
        .await
    {
        Ok(result) => {
            let mut result = HooksResult::from(result);
            result.client_label = client_label.to_string();
            result
        }
        Err(RoostError::Server { code, message }) => {
            HooksResult::failed(client_label, format!("{code}: {message}"))
        }
        Err(error) => HooksResult::failed(client_label, error.to_string()),
    }
}
