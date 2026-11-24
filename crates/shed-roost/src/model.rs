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

use shed_lane::rc::{
    RcActivity, RcAgentInfo, RcCapabilities, RcKind, RcKindFeatures, RcSessionDto, RcState,
    ATTACH_NATIVE_REMOTE,
};
pub use shed_lane::time::rfc3339_z;

/// The `ownership.detail` values that mean **an approval is pending**, exactly.
///
/// `permission_prompt` is claude's (and grok's) spelling, `permission_asked` is
/// opencode's. Matching is by equality, never by substring: `permission_replied`
/// is the *answer* to an approval and a substring test would read it as a new
/// one, leaving a card stuck asking for a decision the user already made.
/// Anything else under `waiting` is input, not approval — a `question_asked` is
/// a question.
pub const APPROVAL_DETAILS: [&str; 2] = ["permission_prompt", "permission_asked"];

/// The `ownership.source` string roost's own adapter writes for a kind — the
/// inverse of [`RoostSession::agent_kind`].
pub fn roost_source(kind: &RcKind) -> Option<&'static str> {
    match kind {
        RcKind::ClaudeRc => Some("claude"),
        RcKind::Opencode => Some("opencode"),
        RcKind::ClaudeBroker | RcKind::Craze | RcKind::Shell | RcKind::Other(_) => None,
    }
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
/// FRB-mirror clean (the rule in [`shed_lane::lane`]'s module doc): three owned
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
    /// sources), and — since plan 025 (shed#390) retired the direct-agent
    /// kinds — `codex`, `cursor`, `grok` (and any shape of `gx`, which roost
    /// never wrote as a source in the first place) all land there now: a
    /// roost tab running one of those agents directly is D1's plain row, with
    /// no lane and no typed affordances, exactly like any other unrecognized
    /// source.
    ///
    /// `craze` is the one addition: a roost tab a craze hub has claimed
    /// reports `source: "craze"` and maps to [`RcKind::Craze`], which the fold
    /// (plan 025 C9+) keys into the hub row it names (D4).
    ///
    /// A tab with no ownership is [`RcKind::Shell`]. Unreachable from an
    /// inventory — which lists owned tabs only — but the function is total.
    pub fn agent_kind(&self) -> RcKind {
        let Some(ownership) = self.ownership.as_ref() else {
            return RcKind::Shell;
        };
        match ownership.source.as_str() {
            "claude" => RcKind::ClaudeRc,
            "opencode" => RcKind::Opencode,
            "craze" => RcKind::Craze,
            other => RcKind::Other(other.to_string()),
        }
    }

    /// Build a synthetic [`RoostSession`] without requiring downstream crates
    /// (such as `shed-desktop-core`'s `rc.inject_test` handler or unit tests)
    /// to depend on `roost-ipc` directly.
    ///
    /// `lifecycle` is roost's wire `agent_lifecycle` word (`working`, `waiting`,
    /// `finished`, `failed`, `inactive`); `None` defaults to `inactive`.
    pub fn synthetic(
        host_label: String,
        tab_id: i64,
        kind: &RcKind,
        title: String,
        cwd: String,
        lifecycle: Option<&str>,
        attention: bool,
    ) -> Result<Self, String> {
        let lifecycle = match lifecycle {
            None => AgentLifecycle::default(),
            Some(word) => serde_json::from_value(serde_json::json!(word))
                .map_err(|_| format!("{word:?} is not a roost agent_lifecycle"))?,
        };
        Ok(Self {
            host_label,
            tab_id,
            project_id: 0,
            project_name: String::new(),
            title,
            user_titled: false,
            cwd,
            shell_state: ShellState::default(),
            lifecycle,
            attention,
            ownership: roost_source(kind).map(|source| Ownership {
                source: source.to_string(),
                ..Ownership::default()
            }),
            created_at: 0,
        })
    }

    /// The agent-lane stamp for this tab, or `None` when there is no lane to
    /// open.
    ///
    /// Since plan 038 retired the opencode adapter, no roost-tab kind carries a
    /// per-tab agent-lane stamp (craze lanes come from the craze hub roster,
    /// not a roost tab's metadata). Always returns `None`.
    pub fn agent_lane(&self) -> Option<AgentLaneStamp> {
        None
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
/// roost has no RC-helper `capabilities` call to probe: it is not shed's guest
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
///
/// The per-kind rows come from [`roost_kind_features`], asked about the kind and
/// whether an agent-lane adapter exists for it — so opencode advertises the
/// message feed its adapter produces and claude-rc advertises the activity
/// dimension it does carry. No row says `"none"`.
///
/// # What this block is, and what it is NOT
///
/// It is per-**origin**, minted once for a whole roost host, and this function
/// has no row in hand. So every row it can state is a **CEILING** — "this is the
/// most an opencode row here could offer" — and a ceiling is all
/// [`RcKindFeatures`] has ever been able to mean on this path. It is deliberately
/// not narrowed to a per-row API: no consumer asked for one, and the per-row
/// truth already exists and is cheaper to read.
///
/// **The per-row truth is [`RoostSession::agent_lane`].** A row whose tab never
/// reported a `server_url` (or reported an unusable one, or has no session id)
/// gets `None` there and `lane.open` answers `no_lane`, even though its kind's
/// row here says `feed: "messages"`, `watch: true`. That is not a contradiction
/// to be fixed — it is the ceiling and the row disagreeing, which is what a
/// ceiling is for. `capabilities_are_a_kind_ceiling_not_a_row_promise` pins it.
///
/// So: a client picking which affordances a KIND could ever have reads this; a
/// client deciding whether to offer THIS ROW a transcript reads
/// [`RoostSession::agent_lane`] and nothing else. (The Tauri client does —
/// `desktop/tauri/ui/src/App.tsx` gates its Transcript button on
/// `s.agent_lane && s.machine`, "and on nothing about the kind".)
pub fn roost_capabilities() -> RcCapabilities {
    let kinds = vec![RcKind::ClaudeRc, RcKind::Opencode];
    let kind_features: HashMap<String, RcKindFeatures> = kinds
        .iter()
        .map(|kind| {
            (
                kind.as_str().to_string(),
                // `lane_adapter_exists`, NOT "a lane is attached": there is no
                // row here to ask. This is the kind's ceiling — see this
                // function's doc, and read [`RoostSession::agent_lane`] for the
                // per-row answer.
                roost_kind_features(kind, lane_adapter_exists(kind)),
            )
        })
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

/// Whether a per-tab roost agent-lane adapter exists for `kind`.
///
/// Since plan 038 retired the opencode adapter, no roost-tab kind has a per-tab lane
/// adapter (craze lanes are driven via `shed-craze` against the craze hub), so
/// `roost_capabilities()` advertises `feed: "activity", watch: false` for every
/// roost kind.
fn lane_adapter_exists(_kind: &RcKind) -> bool {
    false
}

/// One roost kind's feature row — **per kind and client-side** since plan 022
/// (S6, `charliek/shed#328`). The guest hub used to mint this block and it is
/// now synthesized here, which is why the shape is a function of the two things
/// a client actually knows: the kind, and whether a lane is attached.
///
/// **Per KIND, which makes it a ceiling.** Its only production caller,
/// [`roost_capabilities`], is per-origin and passes `lane_adapter_exists(kind)`
/// because it has no row — so what lands on the wire states what a kind COULD do
/// here, never what a given row does. [`RoostSession::agent_lane`] is the per-row
/// truth, and it is allowed to say `None` under a kind row that says
/// `feed: "messages"`.
///
/// **`feed` is `"messages"` when a lane is attached and `"activity"` when it is
/// not, and it is never `"none"`.** Three words are in play:
///
/// * `""` means the field is absent because the producer predates v2 — wrong,
///   since shed synthesizes this block as a v2 producer (`rc_version: 2`,
///   `contract-v2` in `features`);
/// * `"none"` means "no signal at all" — wrong for every roost row, and the
///   value this briefly carried. A roost row ALWAYS carries a live activity
///   dimension, folded out of `agent_lifecycle` and the adapter's `detail` by
///   [`RoostSession::activity`] and refreshed by every batch the observer stream
///   delivers. With the hub gone there is no producer of `"none"` left anywhere;
///   `feed_is_never_none` pins that.
/// * `"messages"` means a normalized conversation feed exists — true exactly
///   when a lane is attached ([`RoostSession::agent_lane`] stamped it), because
///   the lane adapter IS the feed's producer.
///
/// The distinction that makes this easy to get backwards: `feed` describes the
/// **message** feed, and no client gates its activity chip on it (the chip reads
/// the row's own `activity` field).
///
/// `approvals` is `"none"`, a third value beside the documented `tui` | `remote`
/// pair (recorded in rc-helper.md's field table): roost answers approvals in the
/// tab, but shed cannot reach that tab at all, so claiming `tui` would promise an
/// affordance no shed client has. Clients branch on `== "remote"` only, so the
/// value is inert on the wire — it is stated here because the alternatives are
/// both lies.
pub fn roost_kind_features(kind: &RcKind, lane_attached: bool) -> RcKindFeatures {
    // `kind` is not decoration: a caller can only honestly claim an attached
    // lane for a kind an adapter exists for, and `&&`-ing the two here makes the
    // capability row and [`RoostSession::agent_lane`] unable to contradict each
    // other even if a future caller passes the flag from somewhere looser.
    let messages = lane_attached && lane_adapter_exists(kind);
    RcKindFeatures {
        post_input: false,
        approvals: "none".to_string(),
        // DEPRECATED by `feed`, and held in the lockstep the contract promises:
        // `watch == (feed == "messages")`.
        watch: messages,
        input: String::new(),
        feed: if messages { "messages" } else { "activity" }.to_string(),
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
/// [`RcKind::Craze`] has none either: craze sessions are created through
/// craze's own create sheet (plan 025 C10+, D6), never through roost's
/// `tab.open` launch form.
pub fn launch_argv(kind: &RcKind) -> Option<Vec<String>> {
    let bin = match kind {
        RcKind::ClaudeRc => "claude",
        RcKind::Opencode => "opencode",
        RcKind::ClaudeBroker | RcKind::Craze | RcKind::Shell | RcKind::Other(_) => return None,
    };
    Some(vec![bin.to_string()])
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::result_of;
    use roost_ipc::messages::{TabState, SESSION_PROTOCOL_VERSION};
    use serde_json::Value;

    // Shed-recorded vectors: real adapter output from the plan-013 spikes. See the
    // README beside them.
    const SHED_TAB_LIST_FINISHED: &str =
        include_str!("../../fixtures/roost-vectors/shed.tab.list.opencode.finished.json");
    const SHED_TAB_LIST_OVER_SSH: &str =
        include_str!("../../fixtures/roost-vectors/shed.tab.list.opencode.over-ssh.json");
    const SHED_SESSION_IDENTIFY: &str =
        include_str!("../../fixtures/roost-vectors/shed.session.identify.json");

    /// The generation [`SHED_SESSION_IDENTIFY`] was recorded from.
    ///
    /// **It is a RECORDING, so it lags the pin by design, and by at most one
    /// generation.** A re-pin is a source edit; re-recording needs a live daemon
    /// of the new generation, which is a live leg and lands a commit later. So
    /// the number is pinned here rather than read off `SESSION_PROTOCOL_VERSION`,
    /// and the gap is bounded in both directions: the moment the recording is
    /// refreshed the test below goes red (edit this), and the moment a second
    /// bump lands without a re-record the compile-time assertion below does
    /// (record it). Neither can pass by drifting.
    ///
    /// Currently **equal** to the pin: plan 021 re-recorded this off a live
    /// protocol-6 `roost-session` bootstrapped onto a shed.
    const RECORDED_GENERATION: u32 = 6;

    const _: () = assert!(
        RECORDED_GENERATION == SESSION_PROTOCOL_VERSION
            || RECORDED_GENERATION + 1 == SESSION_PROTOCOL_VERSION,
        "shed.session.identify.json lags the pinned session protocol by more than one \
         generation: re-record it from a live daemon of the pinned generation — see \
         crates/fixtures/roost-vectors/README.md, \"Shed-recorded vectors\""
    );

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
            ("opencode", RcKind::Opencode),
            // craze's hub claims a tab under its own source string (plan 025,
            // shed#390) — the one addition since the direct-agent kinds left.
            ("craze", RcKind::Craze),
            // roost's own non-agent sources stay unknown-kind.
            ("manual", RcKind::Other("manual".to_string())),
            ("legacy", RcKind::Other("legacy".to_string())),
            ("something-new", RcKind::Other("something-new".to_string())),
            // The four retired direct-agent kinds (plan 025, shed#390) are now
            // plain `Other` rows too — D1's "a tab running one of those agents
            // directly is a plain row", with no lane and no typed affordances.
            // roost never said `gx` as a source even before the retirement
            // (the promotion used to be shed's, off metadata); it is simply
            // one more unrecognized string now.
            ("codex", RcKind::Other("codex".to_string())),
            ("cursor", RcKind::Other("cursor".to_string())),
            ("grok", RcKind::Other("grok".to_string())),
            ("gx", RcKind::Other("gx".to_string())),
        ];
        for (source, expected) in cases {
            let s = session_with(source, &[]);
            assert_eq!(s.agent_kind(), expected, "source {source}");
            assert_eq!(s.to_rc_dto().kind, expected, "source {source} on the DTO");
            // The retired kinds carry no lane stamp either — the plain-row
            // claim in full, not just the kind (P3's audit).
            if matches!(expected, RcKind::Other(_)) {
                assert!(
                    s.agent_lane().is_none(),
                    "source {source} must stamp no lane"
                );
            }
        }

        // Total even off the inventory path: an unowned tab is a shell.
        let mut t = tab(3, AgentLifecycle::Inactive, ShellState::AtPrompt, "");
        t.ownership = None;
        let s = session_of(&t);
        assert_eq!(s.agent_kind(), RcKind::Shell);
    }

    /// After the opencode adapter's retirement (plan 038), no roost-tab kind stamps a
    /// per-tab agent lane — even an opencode tab reporting a valid loopback
    /// `server_url` renders passively with its kind and activity intact.
    #[test]
    fn agent_lane_is_none_for_every_kind_including_opencode_with_server_url() {
        for (source, meta) in [
            ("opencode", &[("server_url", "http://127.0.0.1:4096")][..]),
            ("craze", &[("server_url", "http://127.0.0.1:4096")][..]),
            ("claude", &[("server_url", "http://127.0.0.1:4096")][..]),
            ("codex", &[("server_url", "http://127.0.0.1:4096")][..]),
            ("grok", &[("server_url", "http://127.0.0.1:2421")][..]),
            ("manual", &[("server_url", "http://127.0.0.1:4096")][..]),
        ] {
            let s = session_with(source, meta);
            assert!(
                s.agent_lane().is_none(),
                "source {source} must not stamp a lane",
            );
        }

        // Wire-level regression: decode the real recorded opencode vector,
        // inject `server_url` into its ownership metadata, and assert that the
        // tab keeps its Opencode kind + activity while stamping no agent lane.
        let mut list: TabListResult = result_of(SHED_TAB_LIST_FINISHED);
        let owned_tab = list.projects[0]
            .tabs
            .iter_mut()
            .find(|t| t.ownership.is_some())
            .expect("recorded opencode tab");
        owned_tab.ownership.as_mut().unwrap().metadata.insert(
            "server_url".to_string(),
            "http://127.0.0.1:4096".to_string(),
        );
        let inventory = RoostInventory::from_list("roost:srv/box", &list, &identify());
        assert_eq!(inventory.sessions.len(), 1);
        let row = &inventory.sessions[0];
        assert_eq!(row.agent_kind(), RcKind::Opencode);
        assert_eq!(row.activity(), Some(RcActivity::Idle));
        assert_eq!(row.to_rc_dto().kind, RcKind::Opencode);
        assert_eq!(row.agent_lane(), None);

        // An unowned tab is total, like every other accessor here.
        let mut t = tab(3, AgentLifecycle::Inactive, ShellState::AtPrompt, "");
        t.ownership = None;
        assert!(session_of(&t).agent_lane().is_none());
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
        // The identify half embeds the generation integer, so unlike the
        // `tab.list` recordings beside it (whose shapes are byte-identical
        // across every re-cut so far) it has to move with the pin. **Asserted
        // rather than trusted:** `SessionIdentify` is not `deny_unknown_fields`,
        // so a stale `features` key or a leftover `4` would decode in silence
        // and this fixture would go on describing a daemon shed refuses.
        // See [`RECORDED_GENERATION`] for why the number is that constant and
        // not `SESSION_PROTOCOL_VERSION`.
        let recorded: Value = serde_json::from_str(SHED_SESSION_IDENTIFY).expect("the recording");
        assert_eq!(
            recorded["result"]["session_protocol"],
            serde_json::json!(RECORDED_GENERATION),
            "the recording moved; set RECORDED_GENERATION to the generation it now carries"
        );
        assert!(
            recorded["result"].get("features").is_none(),
            "`features` retired with the lease; a stale key would decode in silence"
        );
        assert_eq!(
            inventory.daemon_session_id,
            "f7cde10a67e226085e8398fe93e03aaf"
        );
        assert_eq!(inventory.started_at, "2026-09-19T22:58:30Z");
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
        assert_eq!(caps.kinds, vec![RcKind::ClaudeRc, RcKind::Opencode]);
        // roost publishes no agent inventory, so each launchable kind's tool is
        // claimed installed — that is what lets `offers` say yes below.
        let mut tools: Vec<&str> = caps.agents.keys().map(String::as_str).collect();
        tools.sort_unstable();
        assert_eq!(tools, vec!["claude", "opencode"]);
        assert!(caps
            .agents
            .values()
            .all(|a| a.installed && a.version.is_none()));

        // After plan 038 retired the opencode adapter, both roost kinds advertise
        // `"activity"` and `watch: false` — never `""` and never `"none"`.
        let wire_names = [("claude-rc", "activity"), ("opencode", "activity")];
        assert_eq!(caps.kind_features.len(), wire_names.len());
        for (name, feed) in wire_names {
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
            assert_eq!(features.input, "");
            assert!(!features.input_gated());
            assert!(!features.interrupt);
            assert_eq!(features.feed, feed, "{name}'s feed");
            assert_eq!(features.feed_messages(), feed == "messages", "{name}");
            // `watch` is DEPRECATED by `feed` and the contract promises the two
            // stay in lockstep; this is the producer side of that invariant.
            assert_eq!(features.watch, feed == "messages", "{name}'s watch");
        }

        // **No producer of `"none"` is left.** Every roost row carries activity.
        for kind in &caps.kinds {
            for lane_attached in [false, true] {
                let row = roost_kind_features(kind, lane_attached);
                assert_eq!(
                    row.feed,
                    "activity",
                    "{}/{lane_attached} produced feed {:?}",
                    kind.as_str(),
                    row.feed
                );
                assert!(!row.watch);
            }
        }
        assert_eq!(
            roost_kind_features(&RcKind::ClaudeRc, true).feed,
            "activity"
        );
        assert_eq!(
            roost_kind_features(&RcKind::Opencode, true).feed,
            "activity"
        );
        assert_eq!(
            roost_kind_features(&RcKind::Opencode, false).feed,
            "activity"
        );
        assert_eq!(roost_kind_features(&RcKind::Craze, true).feed, "activity");

        // Every advertised kind is offered for creation: `offers` requires an
        // installed agent, and roost publishes no agent inventory, so the
        // capabilities claim each kind's tool as installed (a launch of a
        // missing binary fails visibly in the tab instead).
        assert_eq!(
            caps.creatable_kinds(),
            vec![RcKind::ClaudeRc, RcKind::Opencode]
        );
        assert!(!caps.offers(&RcKind::Shell));
        assert!(!caps.offers(&RcKind::ClaudeBroker));
        assert!(!caps.offers(&RcKind::Craze)); // not advertised: craze states its own availability (D5)
    }

    #[test]
    fn opencode_capabilities_and_rows_both_advertise_activity_only() {
        let caps = roost_capabilities();
        let f = &caps.kind_features["opencode"];
        assert_eq!(f.feed, "activity");
        assert!(!f.watch);
        assert!(!f.feed_messages());

        let with_url = session_with("opencode", &[("server_url", "http://127.0.0.1:4096")]);
        assert_eq!(with_url.agent_kind(), RcKind::Opencode);
        assert!(with_url.agent_lane().is_none());
    }

    #[test]
    fn launch_argv_table() {
        assert_eq!(
            launch_argv(&RcKind::ClaudeRc),
            Some(vec!["claude".to_string()])
        );
        assert_eq!(
            launch_argv(&RcKind::Opencode),
            Some(vec!["opencode".to_string()])
        );
        assert_eq!(launch_argv(&RcKind::ClaudeBroker), None);
        assert_eq!(launch_argv(&RcKind::Shell), None);
        // craze is created through craze's own create sheet, never roost's
        // launch form (D6) — no recipe here either.
        assert_eq!(launch_argv(&RcKind::Craze), None);
        // An unknown kind still has no launch recipe — including the raw
        // strings the four retired direct-agent kinds now decode as.
        assert_eq!(launch_argv(&RcKind::Other("codex".to_string())), None);
        assert_eq!(launch_argv(&RcKind::Other("cursor".to_string())), None);
        assert_eq!(launch_argv(&RcKind::Other("gx".to_string())), None);
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
