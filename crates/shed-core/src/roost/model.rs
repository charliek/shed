//! The session model — a roost tab as shed's clients understand it, and the
//! mapping onto the wire DTO they already render.
//!
//! Two rules shape everything here.
//!
//! **A row is an agent-owned tab.** A `roost-session` is somebody's terminal
//! multiplexer: a user with fifteen shells open has fifteen tabs, and none of
//! them is a *session* in shed's sense. [`RoostInventory::from_list`] keeps only
//! the tabs an agent adapter has claimed (`ownership.is_some()`); the rest are
//! remembered but not listed, because a plain shell tab can become an agent tab
//! later (`agent_report.changed`) and the fold needs its base [`Tab`] to promote
//! it — see [`super::fence`].
//!
//! **[`RcSessionDto`] does not change.** Every card in every client already
//! renders that shape, it is pinned byte-for-byte by the Go↔Rust parity harness,
//! and it is built as a struct literal at a dozen sites. So roost data is
//! *mapped* onto it ([`RoostSession::to_rc_dto`]) rather than the shape being
//! widened to fit roost. The two facts that have nowhere to go in the DTO —
//! `attention` (roost's sticky notification bit) and `tab_id` — stay on
//! [`RoostSession`], and each client stamps them onto its own row payload.
//!
//! ## Status is read, never derived
//!
//! `activity` comes out of roost's four agent axes — `agent_lifecycle`,
//! `shell_state`, `ownership.detail`, `has_notification` — which the adapters
//! write. Nothing in this file looks at terminal output. That is the whole point
//! of the pivot.

use std::collections::{BTreeMap, BTreeSet, HashMap};

use roost_ipc::agent::{AgentLifecycle, Ownership, ShellState};
use roost_ipc::messages::{Project, SessionIdentify, Tab, TabListResult};
use serde::{Deserialize, Serialize};

use crate::rc::{
    RcActivity, RcAgentInfo, RcCapabilities, RcKind, RcKindFeatures, RcSessionDto, RcState,
    ATTACH_NATIVE_REMOTE,
};

/// The `ownership.detail` values that mean **an approval is pending**, exactly.
///
/// `permission_prompt` is claude's (and grok's) spelling, `permission_asked` is
/// opencode's. Matching is by equality, never by substring: `permission_replied`
/// is the *answer* to an approval and a substring test would read it as a new
/// one, leaving a card stuck asking for a decision the user already made.
/// Anything else under `waiting` is input, not approval — a `question_asked` is
/// a question.
pub const APPROVAL_DETAILS: [&str; 2] = ["permission_prompt", "permission_asked"];

/// The `ownership.metadata` key roost's grok adapter stamps gx's remote-lane
/// base URL under (roost R8). Present only once the lane binds, and it is a
/// discovery HINT — roost keeps it on the tab until its adapter says otherwise,
/// so it never means "the lane is up".
pub const GX_REMOTE_KEY: &str = "gx.remote";

/// The `ownership.metadata` key roost's opencode adapter stamps the opencode
/// server's base URL under (roost R10).
pub const OPENCODE_SERVER_URL_KEY: &str = "server_url";

/// `true` for a `http://` URL whose host is `127.0.0.1`, `localhost` or `[::1]`,
/// followed by `:` and a decimal port in `1..=65535` and **nothing else** — no
/// userinfo, path, query or fragment.
///
/// A port of roost's own `roost_agent::common::loopback_base_url`, kept
/// character-for-character rather than reimplemented with a URL parser. Two
/// things ride on it and both need the SAME answer roost gave:
///
/// * [`RoostSession::agent_kind`] promotes a `grok` tab to [`RcKind::Gx`] on it,
///   so a shape roost accepted and shed rejected would be a row that renders as
///   lane-less while roost thinks it published a lane;
/// * a gx discovery record's `url` is matched against the reported URL under it,
///   so a normalisation difference would make a live leader look like no record
///   at all.
///
/// The value being judged is **agent-supplied** — the agent tells roost where it
/// listens and roost re-publishes it verbatim — which is why the rule is a
/// whitelist of three hosts and refuses everything after the port, rather than
/// "parse it and check the host". A caller filters an absent/empty value first;
/// this only judges the shape of a non-empty string.
pub fn loopback_base_url(value: &str) -> bool {
    let Some(rest) = value.strip_prefix("http://") else {
        return false;
    };
    for host in ["127.0.0.1", "localhost", "[::1]"] {
        let Some(after_host) = rest.strip_prefix(host) else {
            continue;
        };
        let Some(port) = after_host.strip_prefix(':') else {
            continue;
        };
        if port.is_empty() || !port.bytes().all(|b| b.is_ascii_digit()) {
            continue;
        }
        return matches!(port.parse::<u32>(), Ok(p) if (1..=65535).contains(&p));
    }
    false
}

/// A trimmed copy of `s`, or `None` when there is nothing left.
fn non_empty(s: &str) -> Option<String> {
    let s = s.trim();
    (!s.is_empty()).then(|| s.to_string())
}

/// Where a row's agent lane is, and which adapter speaks to it — the whole of
/// what a client needs to open one.
///
/// Stamped beside the session DTO rather than on it: [`RcSessionDto`] is pinned
/// byte-for-byte by the Go↔Rust parity harness and gains no field for roost
/// (plan 013 §3.2), so this rides the client's own row payload under the key
/// **`agent_lane`** — not `lane`, which already means the RC hub's lane token on
/// that DTO and would be read by the wrong consumer.
///
/// `kind` is the wire token ([`RcKind::as_str`]), not the enum: a client
/// dispatches on it to pick an adapter, and one it has never heard of is a lane
/// it must refuse by name rather than silently not render. `server_url` keeps
/// that name for both adapters even though gx's own key is `gx.remote` — it is
/// "the base URL of the thing this adapter talks to", and renaming the wire
/// field per agent would push the difference into every client.
///
/// **This is the REPORTED URL.** On a remote machine a client dials somewhere
/// else entirely (a forwarded `127.0.0.1:<local>`); the two are never conflated,
/// and matching a discovery record is done against this one.
///
/// FRB-mirror clean (the rule in [`crate::lane`]'s module doc): three owned
/// `String`s, no map, no `Value`.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct AgentLaneStamp {
    /// The adapter token — `"opencode"` or `"gx"` today.
    pub kind: String,
    /// The AGENT's own session id, the address every lane verb takes.
    pub session_id: String,
    /// The reported loopback base URL of the agent's control surface.
    pub server_url: String,
}

/// One agent-owned roost tab, with the project it lives in and the host it was
/// read from.
///
/// A flat record on purpose: the clients render a flat list of cards, and the
/// project is a label on the card, not a level of nesting. `host_label` is the
/// machine name the reach was opened for (`localhost` for the local session), and
/// it is what a client turns into `origin: "machine:<label>"`.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct RoostSession {
    /// The machine this session was read from — a configured machine's name, or
    /// `localhost`.
    pub host_label: String,
    /// roost's tab id. Persisted across daemon restarts (roost stores `next_id`),
    /// so it is never reused and is safe as a row identity.
    pub tab_id: i64,
    pub project_id: i64,
    pub project_name: String,
    /// roost's own sidebar name for the tab. It follows the foreground process
    /// unless `user_titled`, which is what the user sees in roost — so shed shows
    /// the same string rather than a second, divergent name.
    pub title: String,
    pub user_titled: bool,
    pub cwd: String,
    /// OSC 133 shell activity. Read, not derived.
    pub shell_state: ShellState,
    /// Agent turn state, written by the adapter.
    pub lifecycle: AgentLifecycle,
    /// roost's `has_notification`. **Sticky** — roost clears it on UI focus or an
    /// explicit `tab.clear_notification`, and shed never clears it — so it is an
    /// attention *dot*, not an activity state.
    pub attention: bool,
    /// Who owns the tab. `None` only on a tab that is not a session row (kept for
    /// totality; [`RoostInventory`] never lists one).
    pub ownership: Option<Ownership>,
    /// Tab creation time, unix seconds.
    pub created_at: i64,
}

impl RoostSession {
    /// Build a session from one tab and the project it was listed under.
    ///
    /// The listing project's id wins over the tab's own — on this path the
    /// nesting is what says which project the tab is in.
    pub fn from_tab(host_label: &str, project: &Project, tab: &Tab) -> RoostSession {
        RoostSession {
            project_id: project.id,
            ..RoostSession::from_tab_parts(host_label, &project.name, tab)
        }
    }

    /// Build a session from a tab whose project is known only by id and name —
    /// the event-fold path, where a `tab.opened` carries the tab but not its
    /// project.
    pub(super) fn from_tab_parts(host_label: &str, project_name: &str, tab: &Tab) -> RoostSession {
        RoostSession {
            host_label: host_label.to_string(),
            tab_id: tab.id,
            project_id: tab.project_id,
            project_name: project_name.to_string(),
            title: tab.title.clone(),
            user_titled: tab.user_titled,
            cwd: tab.cwd.clone(),
            shell_state: tab.shell_state,
            lifecycle: tab.agent_lifecycle,
            attention: tab.has_notification,
            ownership: tab.ownership.clone(),
            created_at: tab.created_at,
        }
    }

    /// Whether this tab is a session row at all (an agent has claimed it).
    pub fn is_agent_owned(&self) -> bool {
        self.ownership.is_some()
    }

    /// The RC kind this tab's agent maps onto.
    ///
    /// `ownership.source` is an **open string** on roost's wire (adding an agent
    /// must not be an enum change there), so the tail is
    /// [`RcKind::Other`] — the unknown-kind policy, which renders the raw kind
    /// with no affordances. `manual` and `legacy` (roost's own non-agent
    /// sources) land there.
    ///
    /// ## `grok` is two kinds, and shed decides which
    ///
    /// **roost never says `gx`.** Its adapter reports `source: "grok"` for
    /// grok's `gx` agent whether or not gx's remote lane is up, and stamps
    /// `metadata["gx.remote"]` with the lane's base URL once it binds (roost
    /// R8). So the promotion happens HERE: a `grok` tab carrying a
    /// `gx.remote` that passes [`loopback_base_url`] is [`RcKind::Gx`] — a row
    /// with a transcript — and every other `grok` tab is [`RcKind::Grok`], a
    /// row with a status chip and nothing to open. `gx --no-leader` and
    /// `GX_REMOTE_DISABLE=1` therefore stay `grok`, with no special case.
    ///
    /// **The promotion is a HINT, not liveness.** roost keeps the key on the tab
    /// until its adapter reports otherwise, so a row can read `gx` with a dead
    /// leader behind it; opening the lane is what discovers that, and it answers
    /// `unavailable`. Deriving liveness from metadata is exactly what the epic
    /// forbids, and the shape check is the only judgement made here.
    ///
    /// A tab with no ownership is [`RcKind::Shell`]. Unreachable from an
    /// inventory — which lists owned tabs only — but the function is total.
    pub fn agent_kind(&self) -> RcKind {
        let Some(ownership) = self.ownership.as_ref() else {
            return RcKind::Shell;
        };
        match ownership.source.as_str() {
            "claude" => RcKind::ClaudeRc,
            "codex" => RcKind::Codex,
            "opencode" => RcKind::Opencode,
            "cursor" => RcKind::Cursor,
            "grok" => match ownership.metadata.get(GX_REMOTE_KEY) {
                Some(url) if loopback_base_url(url) => RcKind::Gx,
                _ => RcKind::Grok,
            },
            other => RcKind::Other(other.to_string()),
        }
    }

    /// The agent-lane stamp for this tab, or `None` when there is no lane to
    /// open.
    ///
    /// Three things must hold, and each absence is a lane that would fail on the
    /// first tap rather than one that is merely unreachable:
    ///
    /// * the kind is one an adapter exists for — [`RcKind::Opencode`] or
    ///   [`RcKind::Gx`]. `Gx` already implies the URL passed
    ///   [`loopback_base_url`] ([`RoostSession::agent_kind`]);
    /// * the agent announced where its control surface listens — opencode's
    ///   `server_url`, gx's `gx.remote`, both under the same loopback rule;
    /// * the agent reported its OWN session id, which is the address every lane
    ///   verb takes. A stamp without one advertises a panel that can never open.
    ///
    /// This is the SHED-CORE half of the stamp: the derivation, so the desktop,
    /// the phone and any later client all promote the same rows. What a client
    /// then does with it — build the adapter, key a live entry by
    /// `(kind, server_url)` — is the client's.
    pub fn agent_lane(&self) -> Option<AgentLaneStamp> {
        let ownership = self.ownership.as_ref()?;
        let kind = self.agent_kind();
        let url_key = match kind {
            RcKind::Opencode => OPENCODE_SERVER_URL_KEY,
            RcKind::Gx => GX_REMOTE_KEY,
            _ => return None,
        };
        // Validated on BOTH paths, so the stamp carries ONE contract — "a
        // stamped `server_url` passed the loopback rule" — rather than a
        // per-kind one. roost's own adapters already apply the same rule before
        // publishing either key, so this refuses nothing roost can produce; the
        // point is that the desktop and the phone DIAL this value, and a URL we
        // dial must not depend on an upstream process's filtering staying
        // correct. That is exactly the coupling the `Gx` path already refuses to
        // accept — there is no reason opencode's should accept it.
        let server_url = non_empty(ownership.metadata.get(url_key)?)?;
        if !loopback_base_url(&server_url) {
            return None;
        }
        Some(AgentLaneStamp {
            kind: kind.as_str().to_string(),
            session_id: non_empty(&ownership.session_id)?,
            server_url,
        })
    }

    /// The live-activity dimension, from the three agent axes.
    ///
    /// | lifecycle | activity |
    /// |---|---|
    /// | `working` | [`RcActivity::Working`] |
    /// | `waiting` | [`RcActivity::NeedsApproval`] when `detail` is one of [`APPROVAL_DETAILS`] exactly, else [`RcActivity::NeedsInput`] |
    /// | `finished` | [`RcActivity::Idle`] |
    /// | `failed` | [`RcActivity::NeedsInput`] — roost projects `failed` onto `needs_input` too; the card reads "this tab wants you" |
    /// | `inactive` | [`RcActivity::Idle`] when the shell is at a prompt, else `None` |
    ///
    /// `inactive` with ownership present is the OSC 133 failsafe: the agent is
    /// gone but the tab is still labelled with it. At a prompt that is plainly
    /// idle; mid-foreground-process shed does not know, and claims nothing.
    pub fn activity(&self) -> Option<RcActivity> {
        let detail = self
            .ownership
            .as_ref()
            .map(|o| o.detail.as_str())
            .unwrap_or("");
        match self.lifecycle {
            AgentLifecycle::Working => Some(RcActivity::Working),
            AgentLifecycle::Waiting => Some(if APPROVAL_DETAILS.contains(&detail) {
                RcActivity::NeedsApproval
            } else {
                RcActivity::NeedsInput
            }),
            AgentLifecycle::Finished => Some(RcActivity::Idle),
            AgentLifecycle::Failed => Some(RcActivity::NeedsInput),
            AgentLifecycle::Inactive => match self.shell_state {
                ShellState::AtPrompt => Some(RcActivity::Idle),
                ShellState::Unknown | ShellState::ForegroundProcess => None,
            },
        }
    }

    /// Map onto the wire DTO every client already renders.
    ///
    /// The DTO gains no field for roost (§3.2 of plan 013): `attention` and
    /// `tab_id` stay on this struct and are stamped client-side. The fields that
    /// have no roost counterpart are absent, not empty — `lane`, `url`,
    /// `created_by`, `target_label`, `last_message`, `pending_approvals` are all
    /// `None`, and `tmux_session` is the empty string it must always be present
    /// as (Go emits it unconditionally, so the parity goldens require the key).
    ///
    /// `state` is [`RcState::Ready`] for every row: roost's `TabState` has no
    /// not-live member, and a tab that stops existing leaves the list rather than
    /// turning into a dead row.
    ///
    /// `activity_at` is `ownership.last_event_at`, **paired with `activity`**:
    /// the shared DTO defines it as "when the activity was last derived" and
    /// requires it absent when the activity is ([`RcSessionDto::activity_at`]),
    /// and rc-parity's normalizer enforces that pairing on hub rows. The one
    /// case that would otherwise emit a lone timestamp is `inactive`
    /// mid-foreground-process, where roost has stamped a `last_event_at` but
    /// shed deliberately claims no activity — so the timestamp goes with the
    /// claim it dates rather than standing on its own.
    pub fn to_rc_dto(&self) -> RcSessionDto {
        let ownership = self.ownership.as_ref();
        let activity = self.activity();
        RcSessionDto {
            slug: self.tab_id.to_string(),
            tmux_session: String::new(),
            kind: self.agent_kind(),
            state: RcState::Ready,
            managed: true,
            lane: None,
            display_name: Some(self.title.clone()),
            workdir: Some(self.cwd.clone()),
            url: None,
            // The agent's OWN session id (opencode's `ses_…`, claude's uuid) —
            // the handle a later native verb addresses the agent by.
            id: ownership
                .map(|o| o.session_id.as_str())
                .filter(|s| !s.is_empty())
                .map(str::to_string),
            created_by: None,
            created_at: Some(rfc3339_z(self.created_at)),
            target_label: None,
            activity,
            activity_at: ownership
                .filter(|_| activity.is_some())
                .filter(|o| o.last_event_at > 0)
                .map(|o| rfc3339_z(o.last_event_at)),
            last_message: None,
            pending_approvals: None,
        }
    }
}

/// Everything one `tab.list` + `session.identify` pair says about a session's
/// inventory.
///
/// `daemon_session_id` and `started_at` identify the daemon **instance**: roost's
/// `revision` is an in-process counter that resets on restart, so a watcher
/// compares the pair, not the number alone. Tab ids, by contrast, persist — which
/// is why a restart *replaces* the row set instead of aliasing old cards onto new
/// tabs.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct RoostInventory {
    /// The reach this inventory was read through — the label every row (and
    /// every row the event fold creates later) carries. Stored rather than
    /// recomputed from the rows so an inventory that is empty, or has just
    /// folded its last `tab.closed`, still stamps the right label.
    pub host_label: String,
    /// The agent-owned tabs, flattened across projects, in list order.
    pub sessions: Vec<RoostSession>,
    /// The revision this inventory is current as of. `None` from a UI socket,
    /// which serves no event stream and so publishes no fence.
    pub revision: Option<u64>,
    pub daemon_session_id: String,
    pub started_at: String,
    /// Tabs that are NOT session rows (no ownership), kept so the event fold can
    /// promote one when an adapter claims it: `agent_report.changed` carries the
    /// axes but not the tab's title/cwd/created_at, so without this a newly
    /// claimed tab would appear as a row with empty everything.
    pub(super) hidden: Vec<RoostSession>,
    /// Project id → name, so a `tab.opened` (which carries no project) and a
    /// `project.renamed` can both be folded.
    pub(super) projects: BTreeMap<i64, String>,
}

impl RoostInventory {
    /// Fold one `tab.list` snapshot and the `session.identify` that preceded it
    /// into an inventory.
    ///
    /// Projects are flattened: roost nests tabs under projects, shed's clients
    /// render one list of cards and put the project name on the card.
    pub fn from_list(
        host_label: &str,
        list: &TabListResult,
        identify: &SessionIdentify,
    ) -> RoostInventory {
        let mut sessions = Vec::new();
        let mut hidden = Vec::new();
        let mut projects = BTreeMap::new();
        for project in &list.projects {
            projects.insert(project.id, project.name.clone());
            for tab in &project.tabs {
                let session = RoostSession::from_tab(host_label, project, tab);
                if session.is_agent_owned() {
                    sessions.push(session);
                } else {
                    hidden.push(session);
                }
            }
        }
        RoostInventory {
            host_label: host_label.to_string(),
            sessions,
            revision: list.revision,
            daemon_session_id: identify.session_id.clone(),
            started_at: identify.started_at.clone(),
            hidden,
            projects,
        }
    }

    /// The host label these rows were read from (empty on an empty inventory).
    pub fn host_label(&self) -> &str {
        &self.host_label
    }

    /// Every row as the DTO the clients render.
    pub fn to_rc_dtos(&self) -> Vec<RcSessionDto> {
        self.sessions.iter().map(RoostSession::to_rc_dto).collect()
    }

    /// Every tab id this inventory knows about — the listed rows **and** the
    /// hidden tabs.
    ///
    /// The hidden half is otherwise private, and deliberately so: it is a fold
    /// implementation detail, not a row set. What a client legitimately needs
    /// from it is EXISTENCE — "does this daemon still have a tab with this id" —
    /// because a client may be carrying a row of its own for a tab it just
    /// opened, before any adapter has claimed it (the Tauri client's optimistic
    /// insert). This pair answers that and nothing else.
    pub fn known_tab_ids(&self) -> BTreeSet<i64> {
        self.sessions
            .iter()
            .chain(self.hidden.iter())
            .map(|s| s.tab_id)
            .collect()
    }

    /// Whether this inventory still knows `tab_id` at all — as a row or as a
    /// hidden tab. See [`RoostInventory::known_tab_ids`].
    pub fn knows(&self, tab_id: i64) -> bool {
        self.sessions
            .iter()
            .chain(self.hidden.iter())
            .any(|s| s.tab_id == tab_id)
    }
}

/// The capabilities shed **synthesizes** for a roost-backed host.
///
/// roost has no `shed-ext-rc capabilities` to probe: it is not shed's guest
/// agent, it is a terminal multiplexer with agent adapters. So the client states
/// the contract itself, and states it honestly — for M1 a roost row can be
/// listed, launched and closed, and nothing else. Everything the RC hub used to
/// offer for a machine (feed, typed input, approvals, interrupt) is off here,
/// which is exactly what makes the existing per-feature gates in both clients
/// hide those controls with no new UI conditionals.
///
/// `attach` is [`ATTACH_NATIVE_REMOTE`]: the terminal belongs to roost, and a
/// client reaches it with its own affordance (mobile's read-only `tab.dump`
/// peek) or not at all — never a tmux attach.
pub fn roost_capabilities() -> RcCapabilities {
    let kinds = vec![
        RcKind::ClaudeRc,
        RcKind::Codex,
        RcKind::Opencode,
        RcKind::Cursor,
        RcKind::Gx,
        RcKind::Grok,
    ];
    let kind_features: HashMap<String, RcKindFeatures> = kinds
        .iter()
        .map(|kind| (kind.as_str().to_string(), roost_kind_features()))
        .collect();
    // `RcCapabilities::offers` gates a kind on its backing agent being
    // installed, and roost's wire carries no agent inventory. Claim every
    // launchable kind's tool as installed: the alternative is a launch picker
    // that offers nothing, and a `tab.open` of a missing binary fails visibly
    // in the tab itself (the shell prints "command not found"), which is the
    // honest signal roost gives.
    let agents: HashMap<String, RcAgentInfo> = kinds
        .iter()
        .filter_map(|kind| kind.tool())
        .map(|tool| {
            (
                tool.to_string(),
                RcAgentInfo {
                    installed: true,
                    version: None,
                },
            )
        })
        .collect();
    RcCapabilities {
        // Contract v2 — `attach` is a v2 field, and claiming v2 is what tells a
        // client to read it rather than assume tmux.
        rc_version: 2,
        kinds,
        agents,
        features: vec!["contract-v2".to_string()],
        kind_features,
    }
}

/// The one per-kind feature set every roost kind gets. See
/// [`roost_capabilities`].
///
/// **`feed` is `"activity"`.** Three words are in play and only one is true here.
/// An EMPTY `feed` means the field is absent because the producer predates v2 —
/// wrong, since shed synthesizes this block as a v2 producer (`rc_version: 2`,
/// `contract-v2` in `features`). `"none"` means "no signal at all" — also wrong,
/// and the value this briefly carried: a roost row DOES carry a live activity
/// dimension, folded out of `agent_lifecycle` and the adapter's `detail` by
/// [`RoostSession::activity`] and refreshed by every batch the observer stream
/// delivers. `"activity"` is the vocabulary's word for exactly that — the
/// activity dimension and no message feed — so it is the honest one.
///
/// The distinction that makes this easy to get backwards: `feed` describes the
/// **message** feed, and no client gates its activity chip on it (the chip reads
/// the row's own `activity` field). So the guest hub, whose codex and cursor
/// rows have had no activity producer since A6, correctly says `"none"`, while
/// roost — which reports activity for those same kinds — says `"activity"`.
/// Same two kinds, different answer, because the answer is about where the
/// session lives, not what it is.
///
/// `approvals` is `"none"`, a third value beside the documented `tui` | `remote`
/// pair (recorded in rc-helper.md's field table): roost answers approvals in the
/// tab, but shed cannot reach that tab at all, so claiming `tui` would promise an
/// affordance no shed client has. Clients branch on `== "remote"` only, so the
/// value is inert on the wire — it is stated here because the alternatives are
/// both lies.
fn roost_kind_features() -> RcKindFeatures {
    RcKindFeatures {
        post_input: false,
        approvals: "none".to_string(),
        watch: false,
        input: String::new(),
        feed: "activity".to_string(),
        interrupt: false,
        attach: ATTACH_NATIVE_REMOTE.to_string(),
    }
}

/// The argv that starts `kind`'s agent in a fresh roost tab.
///
/// Minimal by design (plan 013 §4): the binary and nothing else. Prompts,
/// permission modes and the `shed` provider script are a later slice, and a kind
/// with no launch recipe — `shell`, `claude-broker`, or any unknown kind —
/// returns `None` so the caller rejects the launch by name instead of opening an
/// empty tab.
///
/// [`RcKind::Gx`] and [`RcKind::Grok`] each launch their OWN binary — `gx` and
/// `grok`, two programs in one family sharing a `$GROK_HOME`. roost reports
/// either tab as `source: "grok"`, so which kind the resulting ROW reads as is
/// decided afterwards by whether a lane binds
/// ([`RoostSession::agent_kind`]): a `grok` tab has no remote lane and stays
/// [`RcKind::Grok`], and a `gx` tab is promoted once `gx.remote` arrives. The
/// launch argv is therefore the kind the user asked for, not the kind the row
/// will settle on.
pub fn launch_argv(kind: &RcKind) -> Option<Vec<String>> {
    let bin = match kind {
        RcKind::ClaudeRc => "claude",
        RcKind::Codex => "codex",
        RcKind::Opencode => "opencode",
        RcKind::Cursor => "cursor-agent",
        RcKind::Gx => "gx",
        RcKind::Grok => "grok",
        RcKind::ClaudeBroker | RcKind::Shell | RcKind::Other(_) => return None,
    };
    Some(vec![bin.to_string()])
}

/// Format unix seconds as RFC3339 UTC with a `Z` suffix and no fraction — the
/// shape every other `created_at` / `activity_at` on this wire already has.
///
/// Hand-rolled rather than pulled from `chrono`: `shed-core` is the
/// dependency-clean crate (it is what the Swift staticlib and the Android build
/// link), formatting one UTC instant is thirty lines of integer arithmetic, and
/// the alternative is a date-time crate in the FFI tree for a `format!`.
/// `shed-app` keeps `timefmt` (which *parses* the many shapes shed-server emits,
/// and does need a real library); this is the one-way, one-shape half.
///
/// **Byte-equivalent to chrono's `to_rfc3339_opts(Secs, true)` only inside
/// `0000..=9999`.** RFC 3339's expanded-year form prefixes a `+` on years past
/// 9999 (and chrono zero-pads negative ones); this emits a bare
/// `10000-01-01T00:00:00Z`. Reaching that needs a timestamp about eight
/// thousand years out, so the divergence is stated rather than fixed — this is
/// shared code with several call sites, and widening it for an unreachable case
/// would be the worse trade.
pub fn rfc3339_z(unix: i64) -> String {
    // Euclidean so a pre-epoch instant floors instead of truncating toward zero.
    let days = unix.div_euclid(86_400);
    let secs = unix.rem_euclid(86_400);
    let (year, month, day) = civil_from_days(days);
    format!(
        "{year:04}-{month:02}-{day:02}T{:02}:{:02}:{:02}Z",
        secs / 3600,
        (secs / 60) % 60,
        secs % 60,
    )
}

/// Days-since-1970 → proleptic-Gregorian `(year, month, day)`.
///
/// Howard Hinnant's `civil_from_days`, whose trick is to shift the era to start
/// on 1 March so the leap day is the last day of the year and the month-length
/// pattern becomes a single linear formula. Exact for every year in `i64` days;
/// the 400-year era arithmetic is what makes 1900/2000 come out right.
fn civil_from_days(days: i64) -> (i64, u32, u32) {
    // Shift the epoch to 0000-03-01.
    let z = days + 719_468;
    let era = z.div_euclid(146_097);
    let doe = z.rem_euclid(146_097); // day of era, [0, 146096]
    let yoe = (doe - doe / 1_460 + doe / 36_524 - doe / 146_096) / 365; // [0, 399]
    let year = yoe + era * 400;
    let doy = doe - (365 * yoe + yoe / 4 - yoe / 100); // day of year (March-based), [0, 365]
    let mp = (5 * doy + 2) / 153; // March-based month, [0, 11]
    let day = (doy - (153 * mp + 2) / 5 + 1) as u32; // [1, 31]
    let month = if mp < 10 { mp + 3 } else { mp - 9 } as u32; // [1, 12]
    (year + i64::from(month <= 2), month, day)
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::roost::result_of;
    use roost_ipc::messages::TabState;
    use serde_json::Value;

    // Shed-recorded vectors: real adapter output from the plan-013 spikes. See the
    // README beside them.
    const SHED_TAB_LIST_FINISHED: &str =
        include_str!("../../../fixtures/roost-vectors/shed.tab.list.opencode.finished.json");
    const SHED_TAB_LIST_OVER_SSH: &str =
        include_str!("../../../fixtures/roost-vectors/shed.tab.list.opencode.over-ssh.json");
    const SHED_SESSION_IDENTIFY: &str =
        include_str!("../../../fixtures/roost-vectors/shed.session.identify.json");

    fn identify() -> SessionIdentify {
        result_of(SHED_SESSION_IDENTIFY)
    }

    /// A tab with the axes under test and otherwise unremarkable values.
    fn tab(id: i64, lifecycle: AgentLifecycle, shell: ShellState, detail: &str) -> Tab {
        Tab {
            id,
            project_id: 2,
            title: "OC | Exact pong reply".to_string(),
            cwd: "/home/shed/oc-work".to_string(),
            state: TabState::Idle,
            has_notification: false,
            is_active: true,
            user_titled: false,
            position: 1,
            created_at: 1_788_768_867,
            last_active: 1_788_768_867,
            hook_active: true,
            shell_state: shell,
            agent_lifecycle: lifecycle,
            ownership: Some(Ownership {
                source: "opencode".to_string(),
                session_id: "ses_f8510bbf0ffePFCHCY6iyzAieq".to_string(),
                last_event_at: 1_788_768_899,
                detail: detail.to_string(),
                metadata: BTreeMap::new(),
            }),
        }
    }

    fn project(tabs: Vec<Tab>) -> Project {
        Project {
            id: 2,
            name: "Roost".to_string(),
            cwd: "/home/shed".to_string(),
            position: 0,
            created_at: 1_788_769_854,
            tabs,
        }
    }

    /// The session one tab maps to, under the shared test project.
    fn session_of(tab: &Tab) -> RoostSession {
        RoostSession::from_tab("mini3", &project(vec![tab.clone()]), tab)
    }

    fn session(lifecycle: AgentLifecycle, shell: ShellState, detail: &str) -> RoostSession {
        session_of(&tab(4, lifecycle, shell, detail))
    }

    // ---- the mapping table, cell by cell ------------------------------------

    /// Every `(lifecycle, shell_state, detail)` cell of the activity table, as
    /// one golden block. 75 cells: 5 lifecycles x 3 shell states x 5 details
    /// (including both approval spellings and opencode's `question_asked`,
    /// which is input, not approval).
    #[test]
    fn activity_matrix_pins_every_cell() {
        const EXPECTED: &str = "\
inactive unknown            \"\"                -> -
inactive unknown            session_idle      -> -
inactive unknown            permission_prompt -> -
inactive unknown            permission_asked  -> -
inactive unknown            question_asked    -> -
inactive at_prompt          \"\"                -> idle
inactive at_prompt          session_idle      -> idle
inactive at_prompt          permission_prompt -> idle
inactive at_prompt          permission_asked  -> idle
inactive at_prompt          question_asked    -> idle
inactive foreground_process \"\"                -> -
inactive foreground_process session_idle      -> -
inactive foreground_process permission_prompt -> -
inactive foreground_process permission_asked  -> -
inactive foreground_process question_asked    -> -
working  unknown            \"\"                -> working
working  unknown            session_idle      -> working
working  unknown            permission_prompt -> working
working  unknown            permission_asked  -> working
working  unknown            question_asked    -> working
working  at_prompt          \"\"                -> working
working  at_prompt          session_idle      -> working
working  at_prompt          permission_prompt -> working
working  at_prompt          permission_asked  -> working
working  at_prompt          question_asked    -> working
working  foreground_process \"\"                -> working
working  foreground_process session_idle      -> working
working  foreground_process permission_prompt -> working
working  foreground_process permission_asked  -> working
working  foreground_process question_asked    -> working
waiting  unknown            \"\"                -> needs_input
waiting  unknown            session_idle      -> needs_input
waiting  unknown            permission_prompt -> needs_approval
waiting  unknown            permission_asked  -> needs_approval
waiting  unknown            question_asked    -> needs_input
waiting  at_prompt          \"\"                -> needs_input
waiting  at_prompt          session_idle      -> needs_input
waiting  at_prompt          permission_prompt -> needs_approval
waiting  at_prompt          permission_asked  -> needs_approval
waiting  at_prompt          question_asked    -> needs_input
waiting  foreground_process \"\"                -> needs_input
waiting  foreground_process session_idle      -> needs_input
waiting  foreground_process permission_prompt -> needs_approval
waiting  foreground_process permission_asked  -> needs_approval
waiting  foreground_process question_asked    -> needs_input
finished unknown            \"\"                -> idle
finished unknown            session_idle      -> idle
finished unknown            permission_prompt -> idle
finished unknown            permission_asked  -> idle
finished unknown            question_asked    -> idle
finished at_prompt          \"\"                -> idle
finished at_prompt          session_idle      -> idle
finished at_prompt          permission_prompt -> idle
finished at_prompt          permission_asked  -> idle
finished at_prompt          question_asked    -> idle
finished foreground_process \"\"                -> idle
finished foreground_process session_idle      -> idle
finished foreground_process permission_prompt -> idle
finished foreground_process permission_asked  -> idle
finished foreground_process question_asked    -> idle
failed   unknown            \"\"                -> needs_input
failed   unknown            session_idle      -> needs_input
failed   unknown            permission_prompt -> needs_input
failed   unknown            permission_asked  -> needs_input
failed   unknown            question_asked    -> needs_input
failed   at_prompt          \"\"                -> needs_input
failed   at_prompt          session_idle      -> needs_input
failed   at_prompt          permission_prompt -> needs_input
failed   at_prompt          permission_asked  -> needs_input
failed   at_prompt          question_asked    -> needs_input
failed   foreground_process \"\"                -> needs_input
failed   foreground_process session_idle      -> needs_input
failed   foreground_process permission_prompt -> needs_input
failed   foreground_process permission_asked  -> needs_input
failed   foreground_process question_asked    -> needs_input";

        let lifecycles = [
            (AgentLifecycle::Inactive, "inactive"),
            (AgentLifecycle::Working, "working"),
            (AgentLifecycle::Waiting, "waiting"),
            (AgentLifecycle::Finished, "finished"),
            (AgentLifecycle::Failed, "failed"),
        ];
        let shells = [
            (ShellState::Unknown, "unknown"),
            (ShellState::AtPrompt, "at_prompt"),
            (ShellState::ForegroundProcess, "foreground_process"),
        ];
        let details = [
            "",
            "session_idle",
            "permission_prompt",
            "permission_asked",
            "question_asked",
        ];

        let mut rendered = Vec::new();
        for (lifecycle, lname) in lifecycles {
            for (shell, sname) in shells {
                for detail in details {
                    let s = session(lifecycle, shell, detail);
                    // The DTO's activity and the model's must never disagree.
                    assert_eq!(s.to_rc_dto().activity, s.activity());
                    let shown = if detail.is_empty() { "\"\"" } else { detail };
                    let activity = s.activity().map(|a| a.as_str()).unwrap_or("-");
                    rendered.push(format!("{lname:<8} {sname:<18} {shown:<17} -> {activity}"));
                }
            }
        }
        assert_eq!(rendered.len(), 75, "the matrix is 5 x 3 x 5 cells");
        assert_eq!(rendered.join("\n"), EXPECTED);
    }

    /// NEGATIVE CONTROL. The approval set is matched by EQUALITY, not by
    /// substring or prefix — pinned by §10 of plan 013. `permission_replied` is
    /// the answer to an approval, and a substring test would read it as a fresh
    /// one and leave the card stuck asking for a decision already made.
    #[test]
    fn approval_details_are_matched_exactly_not_by_substring() {
        // The two that ARE approvals.
        for detail in APPROVAL_DETAILS {
            assert_eq!(
                session(AgentLifecycle::Waiting, ShellState::Unknown, detail).activity(),
                Some(RcActivity::NeedsApproval),
                "{detail} is an approval"
            );
        }
        // Near misses: each CONTAINS or IS CONTAINED BY an approval spelling, and
        // each must still be plain input.
        for detail in [
            "permission_replied",
            "permission_asked_at",
            "pre_permission_prompt",
            "permission",
            "PERMISSION_ASKED",
            " permission_asked",
            "permission_asked ",
        ] {
            assert_eq!(
                session(AgentLifecycle::Waiting, ShellState::Unknown, detail).activity(),
                Some(RcActivity::NeedsInput),
                "{detail} is not an approval"
            );
        }
    }

    #[test]
    fn attention_is_has_notification_and_is_not_a_dto_field() {
        let mut t = tab(
            4,
            AgentLifecycle::Finished,
            ShellState::Unknown,
            "session_idle",
        );
        let quiet = session_of(&t);
        assert!(!quiet.attention);

        t.has_notification = true;
        let noisy = session_of(&t);
        assert!(noisy.attention);

        // Sticky attention is an independent affordance: it must not perturb the
        // rendered row at all (the clients stamp it themselves).
        assert_eq!(quiet.to_rc_dto(), noisy.to_rc_dto());
    }

    #[test]
    fn dto_maps_every_field() {
        let s = session(AgentLifecycle::Working, ShellState::ForegroundProcess, "");
        let dto = s.to_rc_dto();
        assert_eq!(dto.slug, "4");
        assert_eq!(dto.tmux_session, "");
        assert_eq!(dto.kind, RcKind::Opencode);
        assert_eq!(dto.state, RcState::Ready);
        assert!(dto.managed);
        assert_eq!(dto.lane, None);
        assert_eq!(dto.display_name.as_deref(), Some("OC | Exact pong reply"));
        assert_eq!(dto.workdir.as_deref(), Some("/home/shed/oc-work"));
        assert_eq!(dto.url, None);
        assert_eq!(dto.id.as_deref(), Some("ses_f8510bbf0ffePFCHCY6iyzAieq"));
        assert_eq!(dto.created_by, None);
        assert_eq!(dto.created_at.as_deref(), Some("2026-09-07T08:14:27Z"));
        assert_eq!(dto.target_label, None);
        assert_eq!(dto.activity, Some(RcActivity::Working));
        assert_eq!(dto.activity_at.as_deref(), Some("2026-09-07T08:14:59Z"));
        assert_eq!(dto.last_message, None);
        assert_eq!(dto.pending_approvals, None);
    }

    /// `activity_at` NEVER travels alone. The DTO defines it as the time the
    /// activity was derived, so a row carrying a timestamp and no activity is
    /// out of contract — and rc-parity's normalizer rejects that pairing on hub
    /// rows. `inactive` with a foreground process is the one cell that produces
    /// it: roost has stamped `last_event_at`, and shed still claims nothing.
    #[test]
    fn activity_at_is_omitted_when_there_is_no_activity() {
        let t = tab(
            4,
            AgentLifecycle::Inactive,
            ShellState::ForegroundProcess,
            "session_idle",
        );
        assert!(
            t.ownership.as_ref().unwrap().last_event_at > 0,
            "the case is only interesting with a timestamp to suppress"
        );
        let dto = session_of(&t).to_rc_dto();
        assert_eq!(dto.activity, None, "inactive mid-process claims nothing");
        assert_eq!(dto.activity_at, None, "so its timestamp dates nothing");

        // The control: the same tab at a prompt IS idle, and keeps the stamp.
        let at_prompt = tab(
            4,
            AgentLifecycle::Inactive,
            ShellState::AtPrompt,
            "session_idle",
        );
        let dto = session_of(&at_prompt).to_rc_dto();
        assert_eq!(dto.activity, Some(RcActivity::Idle));
        assert_eq!(dto.activity_at.as_deref(), Some("2026-09-07T08:14:59Z"));
    }

    #[test]
    fn activity_at_is_omitted_when_last_event_at_is_zero() {
        let mut t = tab(
            4,
            AgentLifecycle::Finished,
            ShellState::Unknown,
            "session_idle",
        );
        t.ownership.as_mut().unwrap().last_event_at = 0;
        let s = session_of(&t);
        assert_eq!(s.to_rc_dto().activity_at, None);
        // A negative timestamp is nonsense on this wire and is dropped too.
        t.ownership.as_mut().unwrap().last_event_at = -1;
        let s = session_of(&t);
        assert_eq!(s.to_rc_dto().activity_at, None);
    }

    #[test]
    fn id_is_the_agents_own_session_id_and_absent_when_empty() {
        let mut t = tab(
            4,
            AgentLifecycle::Finished,
            ShellState::Unknown,
            "session_idle",
        );
        let s = session_of(&t);
        assert_eq!(
            s.to_rc_dto().id.as_deref(),
            Some("ses_f8510bbf0ffePFCHCY6iyzAieq")
        );

        // `manual` ownership carries no session concept — roost sends "".
        t.ownership.as_mut().unwrap().session_id = String::new();
        let s = session_of(&t);
        assert_eq!(s.to_rc_dto().id, None);
    }

    #[test]
    fn rfc3339_z_formats_known_instants() {
        assert_eq!(rfc3339_z(0), "1970-01-01T00:00:00Z");
        // The spike's opencode tab.
        assert_eq!(rfc3339_z(1_788_768_867), "2026-09-07T08:14:27Z");
        assert_eq!(rfc3339_z(1_788_768_899), "2026-09-07T08:14:59Z");
        assert_eq!(rfc3339_z(1_700_000_000), "2023-11-14T22:13:20Z");
        // 2000 is a leap year (divisible by 400) — the era arithmetic's own test.
        assert_eq!(rfc3339_z(951_782_400), "2000-02-29T00:00:00Z");
        assert_eq!(rfc3339_z(951_868_800), "2000-03-01T00:00:00Z");
        // 1900 was not (divisible by 100, not 400).
        assert_eq!(rfc3339_z(-2_203_977_600), "1900-02-28T00:00:00Z");
        assert_eq!(rfc3339_z(-2_203_891_200), "1900-03-01T00:00:00Z");
        // Pre-epoch floors rather than truncating toward zero.
        assert_eq!(rfc3339_z(-1), "1969-12-31T23:59:59Z");
    }

    /// A session on `source` with `metadata` — the two axes `agent_kind`
    /// reads, and nothing else.
    fn session_with(source: &str, metadata: &[(&str, &str)]) -> RoostSession {
        let mut t = tab(4, AgentLifecycle::Finished, ShellState::Unknown, "");
        let own = t.ownership.as_mut().expect("the sample tab is owned");
        own.source = source.to_string();
        own.metadata = metadata
            .iter()
            .map(|(k, v)| (k.to_string(), v.to_string()))
            .collect();
        session_of(&t)
    }

    #[test]
    fn agent_kind_maps_every_source() {
        let cases = [
            ("claude", RcKind::ClaudeRc),
            ("codex", RcKind::Codex),
            ("opencode", RcKind::Opencode),
            ("cursor", RcKind::Cursor),
            // A bare grok tab: gx with no lane bound. A real kind now, not the
            // raw-string `Other` it used to be — creatable, lane-less, and it
            // renders a status chip with no Transcript affordance.
            ("grok", RcKind::Grok),
            // roost's own non-agent sources stay unknown-kind.
            ("manual", RcKind::Other("manual".to_string())),
            ("legacy", RcKind::Other("legacy".to_string())),
            ("something-new", RcKind::Other("something-new".to_string())),
            // roost NEVER says `gx` — if it somehow did, that is an unrecognized
            // source and the unknown-kind policy applies. The promotion is
            // shed's, off the metadata, and only off the metadata.
            ("gx", RcKind::Other("gx".to_string())),
        ];
        for (source, expected) in cases {
            let s = session_with(source, &[]);
            assert_eq!(s.agent_kind(), expected, "source {source}");
            assert_eq!(s.to_rc_dto().kind, expected, "source {source} on the DTO");
        }

        // Total even off the inventory path: an unowned tab is a shell.
        let mut t = tab(3, AgentLifecycle::Inactive, ShellState::AtPrompt, "");
        t.ownership = None;
        let s = session_of(&t);
        assert_eq!(s.agent_kind(), RcKind::Shell);
    }

    /// The `grok` → `gx` promotion, over EVERY axis `loopback_base_url` judges.
    ///
    /// The table is the same one [`loopback_base_url_accepts_only_roosts_shape`]
    /// runs directly, driven through the derivation instead — so a shape the
    /// rule rejects can never reach a client as a lane-bearing row, and a shape
    /// it accepts always does. `grok` is the only source this applies to; a
    /// stray `gx.remote` on any other tab is inert.
    #[test]
    fn a_grok_tab_is_promoted_to_gx_on_every_loopback_axis() {
        for &(url, promoted) in LOOPBACK_CASES {
            let s = session_with("grok", &[(GX_REMOTE_KEY, url)]);
            let want = if promoted { RcKind::Gx } else { RcKind::Grok };
            assert_eq!(s.agent_kind(), want, "gx.remote {url:?}");
            assert_eq!(s.to_rc_dto().kind, want, "gx.remote {url:?} on the DTO");
        }

        // Absent, and present-but-empty, are both "no lane".
        assert_eq!(session_with("grok", &[]).agent_kind(), RcKind::Grok);
        assert_eq!(
            session_with("grok", &[(GX_REMOTE_KEY, "")]).agent_kind(),
            RcKind::Grok
        );
        // Another agent's metadata does not promote anything, and neither does
        // the key on a tab that is not grok's.
        assert_eq!(
            session_with("grok", &[("server_url", "http://127.0.0.1:2421")]).agent_kind(),
            RcKind::Grok
        );
        assert_eq!(
            session_with("codex", &[(GX_REMOTE_KEY, "http://127.0.0.1:2421")]).agent_kind(),
            RcKind::Codex
        );
    }

    /// Every axis of roost's rule: the scheme, the three hosts, the required
    /// explicit port, the port's grammar and range, and "nothing after it".
    ///
    /// This is a PORT of `roost_agent::common::loopback_base_url` and the whole
    /// point is that it answers identically, so the table is written from the
    /// rule's clauses rather than from the cases shed happens to see.
    /// A slice, not a sized array: adding a case must not also mean editing a
    /// count that has nothing to do with it.
    const LOOPBACK_CASES: &[(&str, bool)] = &[
        // the three accepted hosts, with an explicit port and nothing after
        ("http://127.0.0.1:2421", true),
        ("http://localhost:2421", true),
        ("http://[::1]:2421", true),
        // port range boundaries
        ("http://127.0.0.1:1", true),
        ("http://127.0.0.1:65535", true),
        ("http://127.0.0.1:0", false),
        ("http://127.0.0.1:65536", false),
        ("http://127.0.0.1:99999999999", false),
        // the port must be present, decimal, and nothing but digits
        ("http://127.0.0.1", false),
        ("http://127.0.0.1:", false),
        ("http://127.0.0.1:24a1", false),
        ("http://127.0.0.1:+2421", false),
        ("http://127.0.0.1: 2421", false),
        // nothing after the port — no path, query, fragment or trailing slash
        ("http://127.0.0.1:2421/", false),
        ("http://127.0.0.1:2421/v1", false),
        ("http://127.0.0.1:2421?t=1", false),
        ("http://127.0.0.1:2421#x", false),
        // scheme: http only, and spelled exactly
        ("https://127.0.0.1:2421", false),
        ("HTTP://127.0.0.1:2421", false),
        ("ws://127.0.0.1:2421", false),
        ("127.0.0.1:2421", false),
        // no userinfo, and no host that merely starts with a loopback one
        ("http://user@127.0.0.1:2421", false),
        ("http://127.0.0.1.evil.com:2421", false),
        ("http://localhost.evil.com:2421", false),
        // a non-loopback host, and the empty string
        ("http://10.0.0.5:2421", false),
        ("", false),
    ];

    #[test]
    fn loopback_base_url_accepts_only_roosts_shape() {
        for &(url, want) in LOOPBACK_CASES {
            assert_eq!(loopback_base_url(url), want, "{url:?}");
        }
    }

    /// The lane stamp: which rows carry one, and what it says.
    ///
    /// Three things gate it — an adapter exists for the kind, the agent
    /// announced a URL, and it reported its own session id — and each absence
    /// is a panel that could never open rather than one that is merely
    /// unreachable.
    #[test]
    fn agent_lane_stamps_opencode_and_gx_only() {
        let oc = session_with("opencode", &[("server_url", "http://127.0.0.1:4096")]);
        assert_eq!(
            oc.agent_lane(),
            Some(AgentLaneStamp {
                kind: "opencode".to_string(),
                session_id: "ses_f8510bbf0ffePFCHCY6iyzAieq".to_string(),
                server_url: "http://127.0.0.1:4096".to_string(),
            })
        );

        // gx: the SAME stamp shape, off `gx.remote`. The wire field stays
        // `server_url` — it means "where this adapter talks to", not "opencode's
        // key".
        let gx = session_with("grok", &[(GX_REMOTE_KEY, "http://127.0.0.1:2421")]);
        assert_eq!(
            gx.agent_lane(),
            Some(AgentLaneStamp {
                kind: "gx".to_string(),
                session_id: "ses_f8510bbf0ffePFCHCY6iyzAieq".to_string(),
                server_url: "http://127.0.0.1:2421".to_string(),
            })
        );

        // Every other kind: no stamp, whatever metadata it carries.
        for (source, meta) in [
            ("grok", &[][..]),
            ("claude", &[("server_url", "http://127.0.0.1:4096")][..]),
            ("codex", &[("server_url", "http://127.0.0.1:4096")][..]),
            ("cursor", &[(GX_REMOTE_KEY, "http://127.0.0.1:2421")][..]),
            ("manual", &[("server_url", "http://127.0.0.1:4096")][..]),
        ] {
            assert!(
                session_with(source, meta).agent_lane().is_none(),
                "source {source} must not stamp a lane",
            );
        }

        // A gx tab whose URL fails the shape check is never `Gx` in the first
        // place, so it cannot stamp one either.
        assert!(
            session_with("grok", &[(GX_REMOTE_KEY, "http://127.0.0.1:2421/v1")])
                .agent_lane()
                .is_none()
        );

        // The SAME rule on the opencode path. The stamp carries one contract —
        // a stamped `server_url` passed the loopback rule — so a client that
        // dials it never depends on roost's filtering having stayed correct.
        // An off-loopback `server_url` is the case that matters: without this,
        // `source="opencode"` + `http://evil.com:80` would hand a client a URL
        // it would go and dial.
        for bad in [
            "http://evil.com:80",
            "http://10.0.0.5:4096",
            "https://127.0.0.1:4096",
            "http://127.0.0.1:4096/v1",
            "http://127.0.0.1",
        ] {
            assert!(
                session_with("opencode", &[("server_url", bad)])
                    .agent_lane()
                    .is_none(),
                "opencode server_url {bad:?} must not stamp a lane",
            );
        }
        // …and the kind is untouched by that refusal: the row is still an
        // opencode row, it just has no lane to open (the promotion axis and the
        // stamp axis are independent).
        assert_eq!(
            session_with("opencode", &[("server_url", "http://evil.com:80")]).agent_kind(),
            RcKind::Opencode,
        );

        // No URL, and a whitespace-only URL, are both "no lane".
        assert!(session_with("opencode", &[]).agent_lane().is_none());
        assert!(session_with("opencode", &[("server_url", "   ")])
            .agent_lane()
            .is_none());

        // No session id: the address every lane verb takes is missing, so the
        // stamp would advertise a panel that can never open.
        let mut t = tab(4, AgentLifecycle::Finished, ShellState::Unknown, "");
        {
            let own = t.ownership.as_mut().expect("owned");
            own.session_id = String::new();
            own.metadata = [(
                "server_url".to_string(),
                "http://127.0.0.1:4096".to_string(),
            )]
            .into_iter()
            .collect();
        }
        assert!(session_of(&t).agent_lane().is_none());

        // An unowned tab is total, like every other accessor here.
        let mut t = tab(3, AgentLifecycle::Inactive, ShellState::AtPrompt, "");
        t.ownership = None;
        assert!(session_of(&t).agent_lane().is_none());
    }

    /// The stamp is FRB-mirror shaped and round-trips through the JSON a client
    /// puts on its own row payload — three owned strings, no map, no `Value`.
    #[test]
    fn agent_lane_stamp_round_trips_on_the_wire() {
        let stamp = session_with("grok", &[(GX_REMOTE_KEY, "http://127.0.0.1:2421")])
            .agent_lane()
            .expect("a gx tab stamps a lane");
        let encoded = serde_json::to_value(&stamp).expect("the stamp serializes");
        assert_eq!(
            encoded,
            serde_json::json!({
                "kind": "gx",
                "session_id": "ses_f8510bbf0ffePFCHCY6iyzAieq",
                "server_url": "http://127.0.0.1:2421",
            })
        );
        let decoded: AgentLaneStamp =
            serde_json::from_value(encoded).expect("the stamp decodes back");
        assert_eq!(decoded, stamp);
    }

    // ---- the shed-recorded vectors ------------------------------------------

    #[test]
    fn recorded_opencode_inventory_lists_the_agent_tab_only() {
        let list: TabListResult = result_of(SHED_TAB_LIST_FINISHED);
        let inventory = RoostInventory::from_list("localhost", &list, &identify());

        // The vector carries TWO tabs: a plain shell (id 3, unowned) beside the
        // opencode tab (id 4). Fifteen terminals must not be fifteen cards.
        assert_eq!(inventory.sessions.len(), 1, "the shell tab is not a row");
        assert_eq!(inventory.hidden.len(), 1);
        assert_eq!(inventory.hidden[0].tab_id, 3);

        let row = &inventory.sessions[0];
        assert_eq!(row.tab_id, 4);
        assert_eq!(row.host_label, "localhost");
        assert_eq!(row.project_id, 2);
        assert_eq!(row.project_name, "Roost");
        assert_eq!(row.title, "OC | Exact pong reply");
        assert!(!row.user_titled);
        assert_eq!(row.lifecycle, AgentLifecycle::Finished);
        assert_eq!(row.shell_state, ShellState::Unknown);
        assert!(!row.attention);
        // The real adapter's metadata rides along untouched.
        let ownership = row.ownership.as_ref().unwrap();
        assert_eq!(ownership.source, "opencode");
        assert_eq!(ownership.detail, "session_idle");
        assert_eq!(
            ownership.metadata.get("agent").map(String::as_str),
            Some("build")
        );
        assert_eq!(
            ownership.metadata.get("version").map(String::as_str),
            Some("1.18.25")
        );

        let dto = row.to_rc_dto();
        assert_eq!(dto.kind, RcKind::Opencode);
        assert_eq!(dto.activity, Some(RcActivity::Idle));
        assert_eq!(dto.id.as_deref(), Some("ses_f8510bbf0ffePFCHCY6iyzAieq"));
        assert_eq!(
            dto.workdir.as_deref(),
            Some("/tmp/claude-1000/-home-charliek-projects-shed/43aeecdb-e298-458d-90ab-2445a361d63b/scratchpad/oc-work")
        );
        assert_eq!(dto.slug, "4");
        assert_eq!(dto.created_at.as_deref(), Some("2026-09-07T08:14:27Z"));

        assert_eq!(inventory.revision, Some(18));
        // The identify half is the **re-recorded** protocol-4 reply — that one
        // embeds the generation integer, so unlike the `tab.list` recordings
        // beside it (whose shapes are byte-identical across the R1 re-cut) it
        // had to be taken again from a `c67ac27` daemon.
        assert_eq!(
            inventory.daemon_session_id,
            "05124e114e2f57de4d0336f7761e7bb9"
        );
        assert_eq!(inventory.started_at, "2026-09-07T17:08:49Z");
        assert_eq!(inventory.to_rc_dtos(), vec![dto]);
    }

    #[test]
    fn recorded_over_ssh_inventory_maps_the_same_way() {
        let list: TabListResult = result_of(SHED_TAB_LIST_OVER_SSH);
        let inventory = RoostInventory::from_list("roost-m1", &list, &identify());
        assert_eq!(inventory.sessions.len(), 1);
        assert_eq!(inventory.revision, Some(17));

        let dto = inventory.sessions[0].to_rc_dto();
        assert_eq!(dto.slug, "4");
        assert_eq!(dto.kind, RcKind::Opencode);
        assert_eq!(dto.activity, Some(RcActivity::Idle));
        assert_eq!(dto.id.as_deref(), Some("ses_f85010d7effexVvTJ1mRHZkDqL"));
        assert_eq!(dto.workdir.as_deref(), Some("/home/shed/oc-work"));
        assert_eq!(dto.created_at.as_deref(), Some("2026-09-07T08:31:46Z"));
        assert_eq!(dto.activity_at.as_deref(), Some("2026-09-07T08:32:19Z"));
        assert_eq!(inventory.sessions[0].host_label, "roost-m1");
        assert_eq!(inventory.host_label(), "roost-m1");
    }

    /// The two `waiting` branches on a REAL recorded row rather than a synthetic
    /// one: the same opencode tab, waiting on an approval vs on a question.
    #[test]
    fn recorded_row_waiting_splits_approval_from_input() {
        let list: TabListResult = result_of(SHED_TAB_LIST_FINISHED);
        let project = list.projects[0].clone();
        let mut tab = project
            .tabs
            .iter()
            .find(|t| t.ownership.is_some())
            .expect("the recorded vector has an owned tab")
            .clone();
        tab.agent_lifecycle = AgentLifecycle::Waiting;

        tab.ownership.as_mut().unwrap().detail = "permission_asked".to_string();
        let approving = RoostSession::from_tab("localhost", &project, &tab);
        assert_eq!(approving.activity(), Some(RcActivity::NeedsApproval));
        assert_eq!(
            approving.to_rc_dto().activity,
            Some(RcActivity::NeedsApproval)
        );

        tab.ownership.as_mut().unwrap().detail = "question_asked".to_string();
        let asking = RoostSession::from_tab("localhost", &project, &tab);
        assert_eq!(asking.activity(), Some(RcActivity::NeedsInput));
        assert_eq!(asking.to_rc_dto().activity, Some(RcActivity::NeedsInput));
    }

    // ---- capabilities, launch, serialization --------------------------------

    #[test]
    fn synthesized_capabilities_advertise_native_remote_attach() {
        let caps = roost_capabilities();
        assert_eq!(caps.rc_version, 2);
        assert_eq!(caps.features, vec!["contract-v2".to_string()]);
        assert!(caps.has_feature("contract-v2"));
        assert_eq!(
            caps.kinds,
            vec![
                RcKind::ClaudeRc,
                RcKind::Codex,
                RcKind::Opencode,
                RcKind::Cursor,
                RcKind::Gx,
                RcKind::Grok,
            ]
        );
        // roost publishes no agent inventory, so each launchable kind's tool is
        // claimed installed — that is what lets `offers` say yes below.
        let mut tools: Vec<&str> = caps.agents.keys().map(String::as_str).collect();
        tools.sort_unstable();
        assert_eq!(
            tools,
            vec!["claude", "codex", "cursor", "grok", "gx", "opencode"]
        );
        assert!(caps
            .agents
            .values()
            .all(|a| a.installed && a.version.is_none()));

        let wire_names = ["claude-rc", "codex", "opencode", "cursor", "gx", "grok"];
        assert_eq!(caps.kind_features.len(), wire_names.len());
        for name in wire_names {
            let features = caps
                .kind_features
                .get(name)
                .unwrap_or_else(|| panic!("kind_features has {name}"));
            assert_eq!(features.attach, ATTACH_NATIVE_REMOTE);
            // The gated read every client uses — no tmux fallback must apply.
            assert_eq!(features.attach_kind(), "native-remote");
            assert!(!features.post_input);
            // Outside the documented `tui` | `remote` pair on purpose, and
            // recorded as such in rc-helper.md's field table — see
            // `roost_kind_features`.
            assert_eq!(features.approvals, "none");
            assert!(!features.watch);
            assert!(!features.feed_messages());
            assert_eq!(features.input, "");
            assert!(!features.input_gated());
            // A roost row carries a live activity dimension (see the matrix in
            // `activity_matrix_pins_every_cell`), so the honest word is
            // `"activity"` — the vocabulary's "activity dimension, no message
            // feed". NOT `""` (which means "pre-v2, fall back to `watch`") and
            // not `"none"` (which would deny the status this whole pivot
            // exists to deliver). The guest hub says `"none"` for these same
            // kinds because ITS producer is gone; the answer depends on where
            // the session lives.
            assert_eq!(features.feed, "activity");
            assert!(!features.feed_messages(), "activity is not a message feed");
            assert!(!features.interrupt);
        }

        // Every advertised kind is offered for creation: `offers` requires an
        // installed agent, and roost publishes no agent inventory, so the
        // capabilities claim each kind's tool as installed (a launch of a
        // missing binary fails visibly in the tab instead).
        assert_eq!(
            caps.creatable_kinds(),
            vec![
                RcKind::ClaudeRc,
                RcKind::Codex,
                RcKind::Opencode,
                RcKind::Cursor,
                RcKind::Gx,
                RcKind::Grok,
            ]
        );
        // Both new kinds are OFFERED, which needs the kind advertised AND its
        // tool claimed installed — the two halves `roost_capabilities` builds
        // from the same list.
        assert!(caps.offers(&RcKind::Gx));
        assert!(caps.offers(&RcKind::Grok));
        assert!(!caps.offers(&RcKind::Shell));
        assert!(!caps.offers(&RcKind::ClaudeBroker));
    }

    #[test]
    fn launch_argv_table() {
        assert_eq!(
            launch_argv(&RcKind::ClaudeRc),
            Some(vec!["claude".to_string()])
        );
        assert_eq!(launch_argv(&RcKind::Codex), Some(vec!["codex".to_string()]));
        assert_eq!(
            launch_argv(&RcKind::Opencode),
            Some(vec!["opencode".to_string()])
        );
        assert_eq!(
            launch_argv(&RcKind::Cursor),
            Some(vec!["cursor-agent".to_string()])
        );
        // Each launches its OWN binary — two programs in one family. roost
        // reports either tab as `source: "grok"`, and which kind the ROW settles
        // on is decided later by whether a lane binds.
        assert_eq!(launch_argv(&RcKind::Gx), Some(vec!["gx".to_string()]));
        assert_eq!(launch_argv(&RcKind::Grok), Some(vec!["grok".to_string()]));
        assert_eq!(launch_argv(&RcKind::ClaudeBroker), None);
        assert_eq!(launch_argv(&RcKind::Shell), None);
        // An unknown kind still has no launch recipe — including the raw string
        // `grok`, which is no longer how a grok tab is spelled but is still an
        // unknown kind if it arrives as one.
        assert_eq!(launch_argv(&RcKind::Other("grok".to_string())), None);
        assert_eq!(launch_argv(&RcKind::Other("borg".to_string())), None);
        // Every advertised kind IS launchable — the two tables must not drift.
        for kind in roost_capabilities().kinds {
            assert!(
                launch_argv(&kind).is_some(),
                "{} is launchable",
                kind.as_str()
            );
        }
    }

    /// The parity goldens require `tmux_session` to be PRESENT on every row
    /// (Go emits it with no `omitempty`), so a roost row must serialize it as
    /// `""` rather than dropping the key.
    #[test]
    fn dto_serializes_with_an_empty_tmux_session_key() {
        let dto = session(
            AgentLifecycle::Finished,
            ShellState::Unknown,
            "session_idle",
        )
        .to_rc_dto();
        let value = serde_json::to_value(&dto).expect("the DTO serializes");
        let object = value.as_object().expect("a DTO is an object");
        assert_eq!(
            object.get("tmux_session"),
            Some(&Value::String(String::new()))
        );
        // ... and the absent-not-null fields really are absent.
        for absent in [
            "lane",
            "url",
            "created_by",
            "target_label",
            "last_message",
            "pending_approvals",
        ] {
            assert!(
                !object.contains_key(absent),
                "{absent} is absent, never null"
            );
        }
        assert_eq!(
            object.get("kind"),
            Some(&Value::String("opencode".to_string()))
        );
        assert_eq!(
            object.get("state"),
            Some(&Value::String("ready".to_string()))
        );
        assert_eq!(object.get("managed"), Some(&Value::Bool(true)));
        assert_eq!(
            object.get("activity"),
            Some(&Value::String("idle".to_string()))
        );

        // And a round trip through the wire is faithful.
        let back: RcSessionDto = serde_json::from_value(value).expect("the DTO round-trips");
        assert_eq!(back, dto);
    }
}
