//! Pure Remote-Control (RC Session Convention v2) logic — the pane classifier,
//! prompt normalization, `shed-ext-rc` argv builders, the non-interactive SSH
//! argv, the neutral wire DTOs, and the enriched `RcSession` model. Ported from
//! shed-desktop's `ShedKit/RC/RemoteControl.swift` + `Models.swift` `RcSession`.
//!
//! No I/O and no feature flag: the SSH+tmux choreography (bootstrap, trust
//! pre-seed, poll-to-ready, prompt delivery) lives in the `shed-ext-rc` guest
//! binary; a client invokes it over SSH — process spawning + the session store
//! live in `shed-app::rc` (feature `rc`) — and decodes this neutral JSON DTO.

use std::collections::HashMap;
use std::sync::LazyLock;

use regex::Regex;
use serde::{Deserialize, Serialize};

use crate::models::{clean_display, dart_trim, opt_trimmed};
use crate::terminal::{shell_quote, ssh_host_key_opts};

/// Fallback workdir for a legacy/unmanaged session whose DTO omits one (the
/// binary resolves `$SHED_WORKSPACE` for managed sessions).
pub const DEFAULT_WORKDIR: &str = "/workspace";
/// Stable tool id for `SHED_RC_CREATED_BY` (`<tool>/<version>`; no `/`).
pub const TOOL_NAME: &str = "shed-desktop";
/// tmux session name prefix.
pub const TMUX_PREFIX: &str = "rc-";
/// The default session lane (contract v2) — an rc-tmux pane. Every kind in this
/// phase is `tui`, and an OLD (pre-v2) payload that omits `lane` entirely is read
/// as this value ([`RcSessionDto::lane_or_tui`]).
pub const LANE_TUI: &str = "tui";
/// The default terminal-attach mode (contract v2) — attach to the rc-tmux
/// session. The fallback an absent/empty `kind_features.attach` decodes to
/// ([`RcKindFeatures::attach_kind`]).
pub const ATTACH_TMUX: &str = "tmux";

/// RC session kind (Convention v2). `<tool>-<mode>` so the model can grow to
/// other agents later; `shell` is tool-agnostic. Mirrors the guest's `rc.Kind`
/// (`internal/ext/rc/rc.go`).
///
/// The [`RcKind::Other`] case implements the **unknown-kind policy**: an
/// unrecognized wire value is PRESERVED verbatim (not coerced to claude-broker as
/// old readers did), so a session created by a newer/other tool renders neutrally
/// — its raw kind string is shown, and no claude-specific affordance (no synthetic
/// claude.ai URL, no typed-input prompt) is attached. Because of the owned string
/// this enum is not `Copy`; it is cheap to `Clone`.
#[derive(Debug, Clone, PartialEq, Eq, Hash)]
pub enum RcKind {
    ClaudeRc,
    ClaudeBroker,
    Codex,
    Opencode,
    Cursor,
    Shell,
    /// An unrecognized kind, its raw wire string preserved (unknown-kind policy).
    Other(String),
}

impl RcKind {
    /// Whether this kind accepts a typed kickoff line — an initial prompt for the
    /// agent REPLs/TUIs, an initial command for `shell`. Mirrors the guest's
    /// `AcceptsTypedInput`: every registered kind except `claude-broker` (whose
    /// input is a remote URL, not the pane); an [`RcKind::Other`] is NOT promptable
    /// (no affordances under the unknown-kind policy).
    pub fn accepts_typed_input(&self) -> bool {
        !matches!(self, RcKind::ClaudeBroker | RcKind::Other(_))
    }

    /// A recognized kind (not the preserved-raw unknown case). A `false` here is the
    /// unknown-kind policy's neutral-render signal.
    pub fn is_known(&self) -> bool {
        !matches!(self, RcKind::Other(_))
    }

    /// Whether this kind runs claude — i.e. one of the two claude kinds (and so
    /// gets claude's full `--permission-mode` set and URL affordances). NOT true
    /// for codex/cursor/opencode. Mirrors the guest's `IsClaudeKind` and mobile's
    /// `RcKind.runsClaude` (`rc_models.dart:76`).
    pub fn runs_claude(&self) -> bool {
        matches!(self, RcKind::ClaudeBroker | RcKind::ClaudeRc)
    }

    /// Whether this kind carries an autonomy/permission posture: every known
    /// agent kind does; `shell` has none, and an unknown kind renders neutrally
    /// with none — a caller-supplied mode is dropped silently for both (see
    /// [`validate_permission_mode`]). Mirrors mobile's `RcKind.hasPermissionMode`
    /// (`rc_models.dart:81`).
    pub fn has_permission_mode(&self) -> bool {
        self.is_known() && !matches!(self, RcKind::Shell)
    }

    /// The tool token this kind's agent maps to under `capabilities.agents`, or
    /// `None` for a kind with no installable agent (`shell`) or an unknown kind.
    pub fn tool(&self) -> Option<&'static str> {
        match self {
            RcKind::ClaudeRc | RcKind::ClaudeBroker => Some("claude"),
            RcKind::Codex => Some("codex"),
            RcKind::Opencode => Some("opencode"),
            RcKind::Cursor => Some("cursor"),
            RcKind::Shell | RcKind::Other(_) => None,
        }
    }

    /// The per-agent login remediation surfaced for this kind's `needs-auth` state,
    /// mirroring the guest's `AuthHintFor` (`internal/ext/rc/agents.go`).
    pub fn auth_hint(&self) -> &'static str {
        match self {
            RcKind::ClaudeRc | RcKind::ClaudeBroker => "run `claude` \u{2192} /login",
            RcKind::Codex => "run `codex` and complete login (`codex login`)",
            RcKind::Opencode => "run `opencode auth login`",
            RcKind::Cursor => "run `cursor-agent login`",
            RcKind::Shell | RcKind::Other(_) => "log in to the agent in a terminal",
        }
    }

    pub fn as_str(&self) -> &str {
        match self {
            RcKind::ClaudeRc => "claude-rc",
            RcKind::ClaudeBroker => "claude-broker",
            RcKind::Codex => "codex",
            RcKind::Opencode => "opencode",
            RcKind::Cursor => "cursor",
            RcKind::Shell => "shell",
            RcKind::Other(s) => s,
        }
    }

    /// Parse a wire kind string, preserving an unrecognized value as
    /// [`RcKind::Other`] (unknown-kind policy) rather than failing or defaulting.
    pub fn from_wire(s: &str) -> RcKind {
        match s {
            "claude-rc" => RcKind::ClaudeRc,
            "claude-broker" => RcKind::ClaudeBroker,
            "codex" => RcKind::Codex,
            "opencode" => RcKind::Opencode,
            "cursor" => RcKind::Cursor,
            "shell" => RcKind::Shell,
            other => RcKind::Other(other.to_string()),
        }
    }

    /// The kinds the launch UI can offer for creation (`claude-broker` is
    /// URL-driven, not create-from-a-form; `Other` is never creatable). Capability
    /// gating narrows this further per shed.
    pub fn creatable() -> [RcKind; 5] {
        [
            RcKind::ClaudeRc,
            RcKind::Codex,
            RcKind::Opencode,
            RcKind::Cursor,
            RcKind::Shell,
        ]
    }
}

impl Serialize for RcKind {
    fn serialize<S: serde::Serializer>(&self, s: S) -> Result<S::Ok, S::Error> {
        s.serialize_str(self.as_str())
    }
}

impl<'de> Deserialize<'de> for RcKind {
    fn deserialize<D: serde::Deserializer<'de>>(d: D) -> Result<Self, D::Error> {
        Ok(RcKind::from_wire(&String::deserialize(d)?))
    }
}

/// A pane-derived session state.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "kebab-case")]
pub enum RcState {
    Starting,
    Ready,
    Reconnecting,
    NeedsTrust,
    NeedsAuth,
    Dead,
}

impl RcState {
    /// Tolerant wire decode: an unknown state from a newer binary/server is
    /// treated as `Starting` (transient) rather than `Dead`, so a forward-compat
    /// session is never shown as gone. Mirrors mobile's `RcState.fromWire`
    /// (`rc_models.dart:176-181`). The strict serde derive above stays the
    /// contract for `shed-ext-rc` stdout (golden-pinned); this is for the
    /// tolerant server-enrichment paths (overview `rc` blocks, rc-events).
    pub fn from_wire(s: &str) -> RcState {
        match s {
            "ready" => RcState::Ready,
            "reconnecting" => RcState::Reconnecting,
            "needs-trust" => RcState::NeedsTrust,
            "needs-auth" => RcState::NeedsAuth,
            "dead" => RcState::Dead,
            _ => RcState::Starting, // incl. "starting" and any future value
        }
    }

    /// Whether this lifecycle state permits showing the live activity
    /// dimension. The server already drops activity for a blocking state
    /// (needs-trust / needs-auth / dead — lifecycle trumps activity); the
    /// client mirrors that gate so it never invents — or leaves on screen — an
    /// activity or last-message a blocking state should hide. Mirrors mobile's
    /// `rcStatePermitsActivity` (`rc_models.dart:154-157`); consumed by the
    /// [`crate::rc_events`] fold's suppression rule.
    pub fn permits_activity(&self) -> bool {
        !matches!(
            self,
            RcState::NeedsTrust | RcState::NeedsAuth | RcState::Dead
        )
    }
}

/// A session's live *work* dimension, orthogonal to the lifecycle [`RcState`].
/// Derived live by the rc hub and reported additively inside a session's `rc`
/// block. Mirrors the guest's `rc.Activity` (`internal/ext/rc/activity.go`) and
/// mobile's `RcActivity` (`rc_models.dart:125-147`): `working` (producing
/// output), `needs_input` (idle at a prompt anchor), `needs_approval` (blocked
/// on an approval the user must answer), `idle` (quiescent), and `unknown` (live
/// but indeterminate).
///
/// Deliberately NO `Other(String)` case (unlike [`RcKind`]'s unknown-kind
/// policy): an UNRECOGNIZED token — any future value — maps to
/// [`RcActivity::Unknown`] (Dart parity, `rc_models.dart:125-146`), so it
/// renders neutrally (no badge) and consumers key off a single variant.
///
/// [`RcActivity::NeedsApproval`] is a LEGAL wire value as of contract v2 and is
/// decoded distinctly (it used to fold into `Unknown`), even though no producer
/// derives it yet — the hub's approval-aware lanes land in a later phase and must
/// not have to recontract the clients to emit it.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash)]
pub enum RcActivity {
    Working,
    NeedsInput,
    NeedsApproval,
    Idle,
    Unknown,
}

impl RcActivity {
    pub fn as_str(&self) -> &'static str {
        match self {
            RcActivity::Working => "working",
            RcActivity::NeedsInput => "needs_input",
            RcActivity::NeedsApproval => "needs_approval",
            RcActivity::Idle => "idle",
            RcActivity::Unknown => "unknown",
        }
    }

    /// Parse a wire activity string; any unrecognized value maps to
    /// [`RcActivity::Unknown`] rather than failing (never-throw decode).
    pub fn from_wire(s: &str) -> RcActivity {
        match s {
            "working" => RcActivity::Working,
            "needs_input" => RcActivity::NeedsInput,
            "needs_approval" => RcActivity::NeedsApproval,
            "idle" => RcActivity::Idle,
            _ => RcActivity::Unknown,
        }
    }
}

impl Serialize for RcActivity {
    fn serialize<S: serde::Serializer>(&self, s: S) -> Result<S::Ok, S::Error> {
        s.serialize_str(self.as_str())
    }
}

impl<'de> Deserialize<'de> for RcActivity {
    fn deserialize<D: serde::Deserializer<'de>>(d: D) -> Result<Self, D::Error> {
        Ok(RcActivity::from_wire(&String::deserialize(d)?))
    }
}

/// A pane-derived `(state, url)` — backs the pure `rc.classify` IPC utility.
#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
pub struct RcClassification {
    pub state: RcState,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub url: Option<String>,
}

/// A binary-domain outcome, distinguished from an SSH transport failure by the
/// exit code (the orchestrator maps SSH auth/unreachable; these are the binary's).
/// Mirrors Swift's `RcError`.
#[derive(Debug, Clone, PartialEq, Eq, thiserror::Error)]
pub enum RcError {
    #[error("rc session already exists: {0}")]
    SlugTaken(String),
    #[error("rc session not found: {0}")]
    NotFound(String),
    #[error("invalid rc request: {0}")]
    BadRequest(String),
    #[error("shed-ext-rc is not installed on this shed — update the shed image")]
    MissingBinary,
    #[error("rc operation failed: {0}")]
    Failed(String),
}

/// The neutral, target-agnostic session shape printed by `shed-ext-rc` (it runs
/// inside the shed and can't know the host alias / shed name — the app injects
/// those and maps `id`→`rc_id`). Optional fields are absent (not null) when
/// unknown; `managed` defaults to false on a legacy payload.
/// # Serialization (added for the Rust rc engine — plan 009)
///
/// The `Serialize` half exists so the ported engine can PRODUCE this wire shape,
/// not only consume it, and its field-presence semantics mirror the Go
/// producer's struct tags EXACTLY (`internal/ext/rc/rc.go:154-202`), because the
/// Go↔Rust parity harness compares stdout structurally — key ORDER is
/// irrelevant, key PRESENCE is contract:
///
/// - `slug`, `tmux_session`, `kind`, `state`, `managed` — always present
///   (`managed` notably even when `false`).
/// - `lane` — Go has no `omitempty` and always emits it; here it is `Option`
///   only because an OLD (pre-v2) producer's payload omits it. Emitted whenever
///   present and skipped when absent, so a decode→encode round trip is faithful
///   and the engine (which always sets it) is byte-comparable with Go.
/// - every other field — absent, never `null` and never `""`. The engine's
///   [`crate::rc_agents::parse_session`] maps Go's empty strings to `None` at
///   construction, so `skip_serializing_if = "Option::is_none"` reproduces
///   `omitempty` exactly without an empty-string special case (which would break
///   round-tripping).
#[derive(Debug, Clone, PartialEq, Eq, Deserialize, Serialize)]
pub struct RcSessionDto {
    pub slug: String,
    pub tmux_session: String,
    pub kind: RcKind,
    pub state: RcState,
    // Strict like Swift's `RcSessionDTO` (binary output, golden-pinned): `managed`
    // is required — a DTO omitting it is a shed-ext-rc contract violation, not a
    // silent "unmanaged". (The enriched `RcSession` model below stays defensive.)
    pub managed: bool,
    /// The session's CURRENT lane (contract v2): `"tui"` (an rc-tmux pane) or
    /// `"structured"` (a native-protocol lane). The guest emits it on EVERY
    /// session — managed, unmanaged, unknown-kind alike — but it stays `Option`
    /// here because an OLD (pre-v2) binary's payload omits it; read it through
    /// [`RcSessionDto::lane_or_tui`], which applies the contract's absent-⇒-`tui`
    /// rule. Carried verbatim (never parsed into an enum): a future lane value
    /// must render neutrally, not vanish the session.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub lane: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub display_name: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub workdir: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub url: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub id: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub created_by: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub created_at: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub target_label: Option<String>,
    /// Live-activity dimension (additive inside the `rc` block; derived by the
    /// rc hub). Absent when no hub is running, the kind is unsupported, or the
    /// server suppressed it (a blocking lifecycle state trumps activity).
    /// Mirrors mobile's `RcSession.activity` (`rc_models.dart:222-234`).
    #[serde(skip_serializing_if = "Option::is_none")]
    pub activity: Option<RcActivity>,
    /// RFC3339 timestamp the activity was last derived/changed; absent with
    /// `activity`.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub activity_at: Option<String>,
    /// A short, hub-sanitized (ANSI/control-stripped, ≤200 runes) preview of
    /// the session's most recent message. Absent when the hub has none.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub last_message: Option<String>,
    /// The session's currently-unresolved approval requests (contract v2) — the
    /// snapshot that keeps a session ACTIONABLE after the feed ring evicted (or a
    /// hub restart lost) the `approval_request` rows that announced them. A
    /// HUB-LAYER field: the one-shot `list` path this DTO usually comes from never
    /// sets it, and nothing produces approvals in this phase, so it is absent
    /// (`None`) on every wire today. See [`RcFeedApproval`] for the folding rule.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub pending_approvals: Option<Vec<RcFeedApproval>>,
}

impl RcSessionDto {
    /// The session's lane with the contract's old-payload rule applied (see
    /// [`lane_or_tui`]).
    pub fn lane_or_tui(&self) -> &str {
        lane_or_tui(self.lane.as_deref())
    }
}

/// The shared shape of every contract-v2 "absent means the phase-1 default"
/// read ([`lane_or_tui`], [`RcKindFeatures::attach_kind`]): a non-empty value
/// wins, anything else (absent, or the empty string an out-of-contract producer
/// or a re-emitting server can leave behind) reads as `fallback`.
fn nonempty_or<'a>(value: &'a str, fallback: &'a str) -> &'a str {
    // trim() so a whitespace-only value counts as absent — keeps this serde-path rule
    // byte-equivalent with the overview flat adapter, which trims before its
    // absent-check (models.rs opt_trimmed). A padded-but-real token passes through
    // trimmed, never as its padded form.
    let trimmed = value.trim();
    if trimmed.is_empty() {
        fallback
    } else {
        trimmed
    }
}

/// The contract's old-payload lane rule, in one place for both the DTO and the
/// enriched session: an absent (pre-v2 binary) or empty `lane` reads as
/// [`LANE_TUI`], so a client never has to distinguish "absent" from `"tui"`.
pub fn lane_or_tui(lane: Option<&str>) -> &str {
    nonempty_or(lane.unwrap_or_default(), LANE_TUI)
}

/// The `shed-ext-rc list` response shape. Strict on `rc_sessions` like Swift's
/// `RcSessionListDTO` (the binary always emits the array, never null/absent, so a
/// missing/null value is a contract violation the fan-out drops), but tolerant on
/// `capabilities`: an OLD baked-in binary's bare `{"rc_sessions":[…]}` envelope has
/// no block, so it decodes to `None` (the capability-discovery leg degrades, it
/// does not error).
///
/// Serializes with the producer's presence semantics (`rc.go:208-211`):
/// `rc_sessions` is ALWAYS emitted (as `[]` when there are none), `capabilities`
/// is an `omitempty` pointer and is omitted when absent.
#[derive(Debug, Clone, PartialEq, Deserialize, Serialize)]
pub struct RcSessionListDto {
    pub rc_sessions: Vec<RcSessionDto>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub capabilities: Option<RcCapabilities>,
}

/// One agent's install-probe result under [`RcCapabilities::agents`]. `version` is
/// absent when the agent is not installed (or its version could not be read).
/// Mirrors the guest's `rc.AgentInfo`.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct RcAgentInfo {
    pub installed: bool,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub version: Option<String>,
}

/// Per-kind UI hints from [`RcCapabilities::kind_features`]. Mirrors the guest's
/// `rc.KindFeatures` and mobile's `KindFeatures` (`rc_capabilities.dart:116-144`):
/// `post_input` reports whether a typed line reaches the pane, `approvals` is
/// where approvals surface (`"tui"` — answered in the terminal; `"remote"` —
/// answered through the hub's `POST /approvals/{id}` verb, opencode today).
///
/// `watch` and `input` are additive hub hints (the feed kinds — codex and
/// opencode — carry them; absent → `false` / `""`): `watch` reports whether the
/// hub produces a live message feed for the kind (`GET /messages` +
/// `message.appended`), and `input` is the feed-input posting **mode string**,
/// single-valued — `"gated"` means `POST /input` is accepted only while the
/// session is waiting, `"turn"` means the lane takes whole turns through `POST
/// /turn` (and `/input` no longer applies — opencode today), `""` means no feed
/// input at all. Note
/// the distinction from the adjacent `post_input`: `post_input` is the
/// typed-input *capability* bool (a typed line reaches the pane over the
/// TUI-only path), while `input` is the *gating mode* of the separate feed-input
/// channel — a kind can have `post_input: true` with no feed input at all.
///
/// Contract v2 adds three more (again serde-default, so a v1/v3 payload decodes
/// unchanged): `feed` is what the hub can stream for the kind (`"messages"` — a
/// normalized conversation feed; `"activity"` — the activity dimension only;
/// `"none"`), `interrupt` reports the `turn/interrupt` verb (true for opencode,
/// false elsewhere), and `attach` is how a terminal reaches the session (`"tmux"`,
/// `"native-remote"`, `"none"`). **`watch` is DEPRECATED by `feed`** — the guest
/// holds `watch == (feed == "messages")` in lockstep (invariant-tested on the
/// producer side) until every client reads `feed`, so the two can be trusted to
/// agree; read them through [`RcKindFeatures::feed_messages`], which prefers
/// `feed` and falls back to `watch` on a payload that predates it.
///
/// **Serialization mirrors the Go producer's `omitempty` set exactly**
/// (`internal/ext/rc/capabilities.go:96-104`): `post_input`, `approvals` and
/// `interrupt` are unconditional; `watch` is skipped when `false`, and `input` /
/// `feed` / `attach` when empty. That re-emission fidelity is the whole point of
/// the Go tags — a newer producer re-emitting an OLDER guest's decoded
/// capabilities must emit the unknown fields as ABSENT, not as `""`/`false`, so
/// the client-side absent-field fallbacks ([`RcKindFeatures::feed_messages`],
/// [`RcKindFeatures::attach_kind`]) still apply on a mixed-version fleet.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct RcKindFeatures {
    pub post_input: bool,
    pub approvals: String,
    /// DEPRECATED by [`RcKindFeatures::feed`] (kept until clients migrate; the
    /// producer maintains the lockstep described on the struct).
    #[serde(default, skip_serializing_if = "is_false")]
    pub watch: bool,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub input: String,
    /// Empty on a pre-v2 payload — and, per the producer's omitempty note, on a
    /// newer server re-emitting an older guest's decoded capabilities. Use
    /// [`RcKindFeatures::feed_messages`] rather than comparing this directly.
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub feed: String,
    #[serde(default)]
    pub interrupt: bool,
    /// Empty on a pre-v2 payload; read it through
    /// [`RcKindFeatures::attach_kind`], which applies the `"tmux"` fallback.
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub attach: String,
}

/// Go's `omitempty` on a `bool` field: `false` is the zero value and is omitted.
/// (`serde` has no built-in for this — `skip_serializing_if` needs a predicate
/// over a reference.)
fn is_false(b: &bool) -> bool {
    !*b
}

impl RcKindFeatures {
    /// Whether feed input is gated (`input == "gated"`) — a watch view's input
    /// bar is only ever enabled for a gated kind waiting for input. Mirrors
    /// mobile's `KindFeatures.inputGated` (`rc_capabilities.dart:136`).
    pub fn input_gated(&self) -> bool {
        self.input == "gated"
    }

    /// Whether the hub streams a normalized MESSAGE feed for this kind (`GET
    /// /messages` + `message.appended`) — the pinned v3-fallback read of the
    /// deprecated `watch` bit: `feed == "messages"`, or, when `feed` is absent
    /// (a pre-v2 payload, or an older guest's capabilities re-emitted by a newer
    /// server), the legacy `watch` flag. A kind whose `feed` is `"activity"` or
    /// `"none"` has no message feed even though it may carry activity.
    pub fn feed_messages(&self) -> bool {
        // trim() so a whitespace-only feed counts as absent — the same policy as
        // nonempty_or (lane/attach) and the overview flat adapter, so every path
        // answers identically on a padded payload.
        let feed = self.feed.trim();
        feed == "messages" || (feed.is_empty() && self.watch)
    }

    /// How a terminal reaches this kind's sessions, with the contract's
    /// absent-⇒-[`ATTACH_TMUX`] fallback applied (every kind in this phase
    /// attaches over tmux, so a payload that predates the field means `"tmux"`).
    pub fn attach_kind(&self) -> &str {
        nonempty_or(&self.attach, ATTACH_TMUX)
    }
}

/// The `shed-ext-rc capabilities` payload, also embedded in the `list` envelope.
/// Tells a client which kinds a shed offers, which agents are installed (and at
/// what version), the feature set, and per-kind UI hints. Mirrors the guest's
/// `rc.Capabilities` (`internal/ext/rc/capabilities.go`). Optional maps/lists
/// default to empty so a partial payload still decodes.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct RcCapabilities {
    pub rc_version: i64,
    #[serde(default)]
    pub kinds: Vec<RcKind>,
    #[serde(default)]
    pub agents: HashMap<String, RcAgentInfo>,
    #[serde(default)]
    pub features: Vec<String>,
    #[serde(default)]
    pub kind_features: HashMap<String, RcKindFeatures>,
}

impl RcCapabilities {
    /// Whether the launch UI should OFFER `kind` for creation given these
    /// capabilities: the kind is advertised in `kinds` AND its backing agent (if
    /// any) is installed. `shell` (no agent) is offered whenever advertised. This is
    /// the capability gate the desktop/mobile create forms apply per shed.
    pub fn offers(&self, kind: &RcKind) -> bool {
        if !self.kinds.contains(kind) {
            return false;
        }
        match kind.tool() {
            None => true, // shell / unknown — no agent to require
            Some(tool) => self.agents.get(tool).is_some_and(|a| a.installed),
        }
    }

    /// The creatable kinds this shed offers, in the canonical create-form order —
    /// the gated list a launch UI renders (empty if nothing is installed).
    pub fn creatable_kinds(&self) -> Vec<RcKind> {
        RcKind::creatable()
            .into_iter()
            .filter(|k| self.offers(k))
            .collect()
    }

    /// Whether `feature` is advertised (feature discovery, replacing error-sniffing).
    pub fn has_feature(&self, feature: &str) -> bool {
        self.features.iter().any(|f| f == feature)
    }
}

/// The app's enriched session — the binary DTO with the host/shed injected and
/// the `<shed>/<slug>` display fallback applied. The wire shape the clients
/// store, list, and inject. `rc_id` is the `SHED_RC_ID`; the table/wire identity
/// is the computed `composite_id` (`host/shed/slug`), NOT encoded.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct RcSession {
    pub host: String,
    pub shed: String,
    pub slug: String,
    pub tmux_session: String,
    pub display_name: String,
    /// The session's working directory when known. `None` for a server-enriched
    /// overview row that carries none (Dart parity, `rc_models.dart:261`);
    /// [`RcSession::from_dto`] (the `shed-ext-rc` stdout path) still fills the
    /// [`DEFAULT_WORKDIR`] fallback, so it is always `Some` there.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub workdir: Option<String>,
    pub kind: RcKind,
    pub state: RcState,
    /// The session's current lane (see [`RcSessionDto::lane`]). `None` on a row
    /// that came from a pre-v2 producer; [`RcSession::lane_or_tui`] applies the
    /// absent-⇒-`"tui"` rule.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub lane: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub url: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub rc_id: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub created_by: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub created_at: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub target_label: Option<String>,
    /// Live-activity dimension (see [`RcSessionDto::activity`]).
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub activity: Option<RcActivity>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub activity_at: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub last_message: Option<String>,
    /// Pending approvals snapshot (see [`RcSessionDto::pending_approvals`]).
    /// Carried through [`RcSession::from_dto`] so a hub-listed session's
    /// actionable approvals reach app/IPC consumers instead of being dropped at
    /// the adaptation boundary — always absent in this phase (no producer), but
    /// the lane that populates it must not need a client contract change.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub pending_approvals: Option<Vec<RcFeedApproval>>,
    #[serde(default)]
    pub managed: bool,
}

impl RcSession {
    /// The table/wire identity — `host/shed/slug`.
    pub fn id(&self) -> String {
        composite_id(&self.host, &self.shed, &self.slug)
    }

    /// The session's lane with the contract's old-payload rule applied (see
    /// [`lane_or_tui`]).
    pub fn lane_or_tui(&self) -> &str {
        lane_or_tui(self.lane.as_deref())
    }

    /// Adapt a binary DTO into an `RcSession`, injecting the host/shed the binary
    /// can't know and applying the `<shed>/<slug>` display fallback. `id`→`rc_id`.
    pub fn from_dto(dto: RcSessionDto, server_name: &str, shed: &str) -> RcSession {
        let display_name = dto
            .display_name
            .unwrap_or_else(|| format!("{shed}/{}", dto.slug));
        RcSession {
            host: server_name.to_string(),
            shed: shed.to_string(),
            slug: dto.slug,
            tmux_session: dto.tmux_session,
            display_name,
            workdir: Some(dto.workdir.unwrap_or_else(|| DEFAULT_WORKDIR.to_string())),
            kind: dto.kind,
            state: dto.state,
            // Verbatim, absence included: the fallback lives in the accessor, so
            // "the producer said tui" stays distinguishable from "the producer is
            // too old to say".
            lane: dto.lane,
            url: dto.url,
            rc_id: dto.id,
            created_by: dto.created_by,
            created_at: dto.created_at,
            target_label: dto.target_label,
            activity: dto.activity,
            activity_at: dto.activity_at,
            // Sanitize the guest-controlled preview text exactly as the
            // overview adapter's `clean_display` does — strip Unicode format
            // characters (category Cf: bidi overrides, zero-widths, BOM), then
            // trim; a value that is only such chars degrades to None. The feed
            // decoder and the overview path both clean this text, so the
            // shed-ext-rc stdout path (`from_dto`) must not be laxer.
            last_message: dto.last_message.and_then(|m| {
                let cleaned = dart_trim(&strip_format_chars(&m)).to_string();
                if cleaned.is_empty() {
                    None
                } else {
                    Some(cleaned)
                }
            }),
            pending_approvals: dto.pending_approvals,
            managed: dto.managed,
        }
    }
}

/// The table/wire identity — `host/shed/slug`. The single source of truth so a
/// kill that has only those three parts keys exactly the same entry.
pub fn composite_id(host: &str, shed: &str, slug: &str) -> String {
    format!("{host}/{shed}/{slug}")
}

/// The tmux session name for a slug (`rc-<slug>`).
pub fn tmux_name(slug: &str) -> String {
    format!("{TMUX_PREFIX}{slug}")
}

/// The synthetic claude.ai URL for a slug — the test-mode analog of what the
/// pane classifier extracts live (broker → `?environment=env_…`, rc → `/session_…`).
/// Only the claude kinds have one; every other kind — including `Other` under the
/// unknown-kind policy — gets `None` (no synthetic claude affordance).
pub fn synthetic_url(kind: &RcKind, slug: &str) -> Option<String> {
    match kind {
        RcKind::ClaudeBroker => Some(format!("https://claude.ai/code?environment=env_{slug}")),
        RcKind::ClaudeRc => Some(format!("https://claude.ai/code/session_{slug}")),
        _ => None,
    }
}

// ---- guest-text sanitization ----

static RE_FORMAT_CHARS: LazyLock<Regex> = LazyLock::new(|| Regex::new(r"\p{Cf}").unwrap());

/// Remove every Unicode format character (category Cf — bidi overrides like
/// U+202E, zero-widths, BOM, soft hyphen, …) from `s`. Client-side defense for
/// guest-controlled display text (last-message previews, feed messages): the
/// rc hub strips ANSI escapes and C0/C1 control characters but NOT Cf, and a
/// bidi override can visually reverse rendered text to spoof what a message
/// appears to say. Mirrors mobile's `stripFormatChars` (`text_sanitize.dart`).
/// Shared by the overview session adapter and the rc feed decoder.
pub fn strip_format_chars(s: &str) -> String {
    RE_FORMAT_CHARS.replace_all(s, "").into_owned()
}

// ---- prompt normalization ----

/// True when `s` carries no control characters. Rust's `char::is_control` covers
/// Unicode Cc (C0/C1 + DEL) — a superset of the guest's `<= 0x1f`/`0x7f` check,
/// so the client stays stricter than the guest (never sends a value it'd reject).
pub fn is_safe_rc_value(s: &str) -> bool {
    !s.chars().any(char::is_control)
}

/// Normalize + validate a caller-supplied kickoff line: trim (incl. newlines);
/// an empty/blank value → `None` (the caller omits `--prompt-stdin`); else reject
/// a prompt on a non-typed-input kind, an embedded control char, or an over-long
/// value (>2000 UTF-8 bytes). Mirrors Swift's `normalizeRcPrompt`.
pub fn normalize_rc_prompt(raw: Option<&str>, kind: &RcKind) -> Result<Option<String>, RcError> {
    let trimmed = match raw {
        Some(s) => s.trim(),
        None => return Ok(None),
    };
    if trimmed.is_empty() {
        return Ok(None);
    }
    if !kind.accepts_typed_input() {
        return Err(RcError::BadRequest(format!(
            "kind {} does not accept an initial prompt",
            kind.as_str()
        )));
    }
    if !is_safe_rc_value(trimmed) {
        return Err(RcError::BadRequest(
            "initial prompt must not contain control characters".to_string(),
        ));
    }
    // UTF-8 byte cap (what actually crosses stdin) — matches shed-remote-agent's
    // 2000-char create limit. `str::len` is the byte length.
    if trimmed.len() > 2000 {
        return Err(RcError::BadRequest(
            "initial prompt exceeds 2000 bytes".to_string(),
        ));
    }
    Ok(Some(trimmed.to_string()))
}

// ---- permission modes ----

/// The generic permission tri-state accepted by EVERY kind and mapped per agent
/// to that tool's real flags by shed-ext-rc (the VM is already the sandbox).
/// Mirrors the guest's `genericPermModes` (`internal/ext/rc/rc.go`) and mobile's
/// `rcGenericPermissionModes` (`rc_service.dart:22`).
pub const GENERIC_PERMISSION_MODES: [&str; 3] = ["default", "auto", "skip"];

/// claude's historical `--permission-mode` values, accepted on top of the
/// generic tri-state by the claude kinds ONLY. Mirrors the claude spec's
/// `ExtraModes` and mobile's `rcClaudeExtraModes` (`rc_service.dart:27-32`).
pub const CLAUDE_EXTRA_MODES: [&str; 4] = ["acceptEdits", "plan", "dontAsk", "bypassPermissions"];

/// The create-time default permission mode. `auto` keeps a session running
/// autonomously rather than blocking on permission prompts; it is a member of
/// both the generic tri-state and the claude set, so it is valid for every
/// agent kind. Mirrors mobile's `defaultRcPermissionMode` (`rc_service.dart:65`).
pub const DEFAULT_RC_PERMISSION_MODE: &str = "auto";

/// The permission modes valid for `kind`: the full claude set (generic
/// tri-state + historical extras, in that display order) for the claude kinds,
/// else the generic tri-state (codex/cursor/opencode/shell). Mirrors the
/// guest's `PermModeAcceptedBy` and mobile's `permissionModesFor`
/// (`rc_service.dart:58-59`).
pub fn permission_modes_for(kind: &RcKind) -> Vec<&'static str> {
    let mut modes = GENERIC_PERMISSION_MODES.to_vec();
    if kind.runs_claude() {
        modes.extend(CLAUDE_EXTRA_MODES);
    }
    modes
}

/// Validate a caller-supplied permission mode against `kind`, returning the
/// EFFECTIVE mode to pass to [`create_argv`]/[`create_invocation`]. A kind
/// without a permission posture (`shell`, unknown) silently drops the mode —
/// `Ok(None)`, no error (the UI hides the picker, but state can linger across a
/// kind switch; same posture as a claude-broker's dropped prompt). For a
/// supporting kind, a mode outside [`permission_modes_for`] is rejected with
/// [`RcError::BadRequest`] before any SSH call. Mirrors mobile's `RcService.create`
/// gate (`rc_service.dart:142-145`). [`create_invocation`] calls this itself (the
/// single validating entry point); only [`create_argv`], the low-level builder,
/// requires the pre-validated effective mode.
pub fn validate_permission_mode<'a>(
    kind: &RcKind,
    mode: Option<&'a str>,
) -> Result<Option<&'a str>, RcError> {
    if !kind.has_permission_mode() {
        return Ok(None);
    }
    match mode {
        None => Ok(None),
        Some(m) if permission_modes_for(kind).contains(&m) => Ok(Some(m)),
        Some(_) => Err(RcError::BadRequest("invalid permission mode".to_string())),
    }
}

// ---- shed-ext-rc argv ----

/// argv for `shed-ext-rc create --wait` (the binary resolves the workdir,
/// pre-seeds trust, polls to ready, accepts trust, and delivers a stdin prompt).
/// `bin` is resolved by the caller (`shed-app` reads `SHED_EXT_RC_BIN`) so this
/// stays pure. `slug` is caller-supplied (generated in `shed-app::rc`, not here).
/// `permission_mode` is the already-EFFECTIVE mode and must only ever be the
/// output of [`validate_permission_mode`] (the validating gate is
/// [`create_invocation`]; this stays the low-level infallible builder): `Some`
/// emits `--permission-mode <mode>` between `--workdir` and `--prompt-stdin`
/// (mobile's exact ordering, `rc_service.dart:168-174`), `None` emits no flag
/// (each tool's own default).
#[allow(clippy::too_many_arguments)]
pub fn create_argv(
    bin: &str,
    kind: &RcKind,
    name: &str,
    slug: &str,
    workdir: Option<&str>,
    created_by: &str,
    target: &str,
    permission_mode: Option<&str>,
    has_prompt: bool,
) -> Vec<String> {
    let mut a = vec![
        bin.to_string(),
        "create".to_string(),
        "--kind".to_string(),
        kind.as_str().to_string(),
        "--name".to_string(),
        name.to_string(),
        "--slug".to_string(),
        slug.to_string(),
        "--created-by".to_string(),
        created_by.to_string(),
        "--target".to_string(),
        target.to_string(),
        "--wait".to_string(),
    ];
    if let Some(w) = workdir.filter(|s| !s.is_empty()) {
        a.push("--workdir".to_string());
        a.push(w.to_string());
    }
    if let Some(m) = permission_mode {
        a.push("--permission-mode".to_string());
        a.push(m.to_string());
    }
    if has_prompt {
        a.push("--prompt-stdin".to_string());
    }
    a
}

/// Build the `create` argv and its stdin together, so the `--prompt-stdin` flag
/// and the stdin payload can never disagree. `prompt` must already be normalized
/// (see [`normalize_rc_prompt`]); it is dropped for a kind that doesn't accept
/// typed input.
///
/// This is the **validating gate** for `permission_mode` (a deliberate, safer
/// deviation from the plan's "both builders infallible"): the raw
/// caller-supplied mode is run through [`validate_permission_mode`] here —
/// silently dropped for a kind without a permission posture, rejected with
/// [`RcError::BadRequest`] when outside the kind's set (no argv is built) — and
/// only the returned EFFECTIVE mode reaches [`create_argv`]. Mirrors mobile's
/// derive-then-validate-then-emit order (`rc_service.dart:142-145`, `:171-173`);
/// [`create_argv`] stays the low-level infallible builder.
#[allow(clippy::too_many_arguments)]
pub fn create_invocation(
    bin: &str,
    kind: &RcKind,
    name: &str,
    slug: &str,
    workdir: Option<&str>,
    created_by: &str,
    target: &str,
    permission_mode: Option<&str>,
    prompt: Option<&str>,
) -> Result<(Vec<String>, Option<String>), RcError> {
    let mode = validate_permission_mode(kind, permission_mode)?;
    let effective = if kind.accepts_typed_input() {
        prompt
    } else {
        None
    };
    let argv = create_argv(
        bin,
        kind,
        name,
        slug,
        workdir,
        created_by,
        target,
        mode,
        effective.is_some(),
    );
    Ok((argv, effective.map(str::to_string)))
}

pub fn list_argv(bin: &str) -> Vec<String> {
    vec![bin.to_string(), "list".to_string()]
}

pub fn kill_argv(bin: &str, slug: &str) -> Vec<String> {
    vec![
        bin.to_string(),
        "kill".to_string(),
        "--slug".to_string(),
        slug.to_string(),
    ]
}

/// Argv for a `prompt` — the kickoff line sent to an already-ready claude-rc/shell
/// session on `slug`. The prompt text goes on **stdin**, not argv (like the other
/// builders, this only produces argv). `session_id`, when present and non-empty,
/// guards against a slug that was recreated under a new session. Mirrors mobile's
/// `prompt()` (`rc_service.dart:201-209`).
pub fn prompt_argv(bin: &str, slug: &str, session_id: Option<&str>) -> Vec<String> {
    let mut argv = vec![
        bin.to_string(),
        "prompt".to_string(),
        "--slug".to_string(),
        slug.to_string(),
    ];
    if let Some(id) = session_id {
        if !id.is_empty() {
            argv.push("--session-id".to_string());
            argv.push(id.to_string());
        }
    }
    argv
}

/// Map a non-zero exit code + stderr to an `RcError`. SSH-transport failures (the
/// binary never ran) surface as `Failed` with the ssh stderr. Mirrors Swift's
/// `RemoteControl.error`.
pub fn error_from_exit(exit_code: i32, stderr: &str, stdout: &str) -> RcError {
    let detail = if stderr.is_empty() { stdout } else { stderr }
        .trim()
        .to_string();
    match exit_code {
        3 => RcError::SlugTaken(detail),
        4 => RcError::NotFound(detail),
        2 => RcError::BadRequest(detail),
        127 => RcError::MissingBinary,
        _ => {
            if stderr.to_lowercase().contains("command not found") {
                RcError::MissingBinary
            } else if detail.is_empty() {
                RcError::Failed(format!("shed-ext-rc exited {exit_code}"))
            } else {
                RcError::Failed(detail)
            }
        }
    }
}

// ---- DTO decode ----

/// Decode a single-session DTO from the binary's stdout.
pub fn decode_session(stdout: &str) -> Result<RcSessionDto, RcError> {
    serde_json::from_str(stdout)
        .map_err(|_| RcError::Failed("shed-ext-rc returned an invalid session DTO".to_string()))
}

/// Decode the `list` response from the binary's stdout. Strict, matching Swift's
/// `decodeList`: a malformed/empty/null payload is an error (the list fan-out in
/// `shed-app::rc` drops it to `[]`), never silently treated as "no sessions".
pub fn decode_list(stdout: &str) -> Result<Vec<RcSessionDto>, RcError> {
    decode_list_response(stdout).map(|l| l.rc_sessions)
}

/// Decode the full `list` envelope — sessions PLUS the optional `capabilities`
/// block. An old baked-in binary's bare `{"rc_sessions":[…]}` yields
/// `capabilities: None` (tolerant of absence). Same strictness on `rc_sessions` as
/// [`decode_list`].
pub fn decode_list_response(stdout: &str) -> Result<RcSessionListDto, RcError> {
    serde_json::from_str::<RcSessionListDto>(stdout)
        .map_err(|_| RcError::Failed("shed-ext-rc returned an invalid session list".to_string()))
}

// ---- rc hub messages feed ----
//
// The codex message feed served by the rc hub through the server proxy
// (`GET /api/sheds/{name}/rc/v1/sessions/{slug}/messages`,
// `internal/api/rchub.go:280-375`). Mirrors the guest's `feedMessage` /
// `hubMessagesResponse` (`internal/ext/rc/hub_messages.go:44-201`,
// handler `hub.go:332-385`) and mobile's decoder (`rc_feed.dart`): each
// message is already hub-sanitized (ANSI/control-stripped, per-field capped),
// so a client renders it as plain text. The one client-side addition: Unicode
// format characters (category Cf — bidi overrides like U+202E) are stripped
// from display text at decode via [`strip_format_chars`], because the hub's
// sanitizer covers ANSI + C0/C1 controls but not Cf.
//
// Tolerant field readers: Dart's feed `_str`/`_text` (`rc_feed.dart:85-98`)
// are byte-identical to `rc_models.dart`'s `_str`/`_cleanDisplay`, so this
// section reuses [`crate::models::opt_trimmed`] / [`crate::models::clean_display`]
// rather than re-declaring them.

/// Dart's feed `_int` (`rc_feed.dart:83`), narrowed to the wire's non-negative
/// seq: an integer as-is, another number truncated (negatives saturate to 0),
/// anything else `0`. NOT shared with `models.rs`'s `int_or_zero` — that one
/// mirrors a signed Dart `_int` and keeps negatives.
fn feed_u64(v: Option<&serde_json::Value>) -> u64 {
    v.and_then(|v| v.as_u64().or_else(|| v.as_f64().map(|f| f as u64)))
        .unwrap_or(0)
}

/// One tool call/result block on a feed message: a name plus a compact detail
/// (invocation args for a `tool_use`, output for a `tool_result`). Both are
/// hub-sanitized AND Cf-stripped here; either may be absent. Mirrors mobile's
/// `RcFeedTool` (`rc_feed.dart:16-23`).
#[derive(Debug, Clone, PartialEq, Eq, Default)]
pub struct RcFeedTool {
    pub name: Option<String>,
    pub detail: Option<String>,
}

impl RcFeedTool {
    fn from_map(o: &serde_json::Map<String, serde_json::Value>) -> RcFeedTool {
        RcFeedTool {
            name: clean_display(o.get("name")),
            detail: clean_display(o.get("detail")),
        }
    }
}

/// The machine-readable state of an approval request (contract v2), carried by an
/// `approval_request` feed row and — once a lane produces approvals — by a
/// session's [`RcSessionDto::pending_approvals`] snapshot. Mirrors the guest's
/// `rc.FeedApproval` (`internal/ext/rc/hub_messages.go`).
///
/// **CLIENT FOLDING RULE: approval rows are an id-keyed, LAST-WRITE-WINS stream.**
/// A resolution is a SECOND appended row with the same `id` and `status`
/// `"resolved"` — never an edit of the first. A client must NOT require having
/// seen the `pending` row before the `resolved` one: ring eviction (or a hub
/// restart) can drop the earlier row entirely, and the session's
/// `pending_approvals` snapshot is the authoritative answer to "what is still
/// open".
///
/// Every field decodes tolerantly (wrong-typed → default), like the rest of the
/// feed: an approval a client cannot interpret must degrade to an un-actionable
/// row, never break the page.
#[derive(Debug, Clone, PartialEq, Eq, Default, Serialize)]
pub struct RcFeedApproval {
    /// The lane-assigned approval id — the address the approval verb resolves
    /// (`POST /v1/sessions/{slug}/approvals/{id}`). Hub-sanitized/bounded;
    /// grammar `^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`.
    pub id: String,
    /// `"pending"` or `"resolved"`.
    pub status: String,
    /// The decision that resolved it (`None` while pending). Go tags it
    /// `omitempty` (hub_messages.go), so serialization skips `None` — absent,
    /// never `null` — matching every other optional on the wire.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub decision: Option<String>,
    /// The decisions this request accepts (a subset of `allow`/`allow_always`/
    /// `deny`), advertised per request so a client renders exactly the buttons
    /// the lane will honor. Empty when the producer advertised none — and, like
    /// Go's `omitempty`, skipped entirely when empty rather than emitted as `[]`.
    #[serde(skip_serializing_if = "Vec::is_empty")]
    pub decisions: Vec<String>,
}

impl RcFeedApproval {
    fn from_map(o: &serde_json::Map<String, serde_json::Value>) -> RcFeedApproval {
        RcFeedApproval {
            // Identifier-shaped fields, not prose: trimmed-verbatim (no
            // Cf-stripping — the hub already strips every control/whitespace
            // rune from an approval token).
            id: opt_trimmed(o.get("id")).unwrap_or_default(),
            status: opt_trimmed(o.get("status")).unwrap_or_default(),
            decision: opt_trimmed(o.get("decision")),
            decisions: o
                .get("decisions")
                .and_then(serde_json::Value::as_array)
                .map(|a| a.iter().filter_map(|d| opt_trimmed(Some(d))).collect())
                .unwrap_or_default(),
        }
    }

    /// Whether this row reports an approval still awaiting an answer — the
    /// actionable half of the last-write-wins fold.
    pub fn is_pending(&self) -> bool {
        self.status == "pending"
    }
}

impl<'de> Deserialize<'de> for RcFeedApproval {
    /// Tolerant (a non-object — including `null` — decodes to the default
    /// approval), so the [`RcSessionDto::pending_approvals`] serde path inherits
    /// the feed's never-throw posture.
    fn deserialize<D: serde::Deserializer<'de>>(d: D) -> Result<Self, D::Error> {
        let v = serde_json::Value::deserialize(d)?;
        Ok(v.as_object()
            .map(RcFeedApproval::from_map)
            .unwrap_or_default())
    }
}

/// One normalized conversation message in the feed. `role` ∈ {user, assistant,
/// tool, system}; `msg_type` (wire key `type`) ∈ {text, tool_use, tool_result,
/// reasoning, status, approval_request}. `seq` is monotonic per hub run (restarts
/// from 1 on hub restart — a client that sees a seq lower than one it holds does a
/// full refetch). Mirrors mobile's `RcFeedMessage` (`rc_feed.dart:29-58`); every
/// field decodes tolerantly (wrong-typed → default/`None`, never an error).
#[derive(Debug, Clone, PartialEq, Eq, Default)]
pub struct RcFeedMessage {
    pub seq: u64,
    /// RFC3339, verbatim (crate convention: timestamps are never parsed here).
    pub ts: Option<String>,
    pub role: String,
    /// The wire's `type` field (renamed: `type` is a Rust keyword).
    pub msg_type: String,
    pub text: Option<String>,
    pub tool: Option<RcFeedTool>,
    /// The approval block of an `approval_request` row (contract v2) — `None` on
    /// every other message type. `text`/`tool` still carry the human-readable
    /// summary of what is being approved.
    pub approval: Option<RcFeedApproval>,
}

impl RcFeedMessage {
    fn from_map(o: &serde_json::Map<String, serde_json::Value>) -> RcFeedMessage {
        RcFeedMessage {
            seq: feed_u64(o.get("seq")),
            ts: opt_trimmed(o.get("ts")),
            role: opt_trimmed(o.get("role")).unwrap_or_default(),
            msg_type: opt_trimmed(o.get("type")).unwrap_or_default(),
            text: clean_display(o.get("text")),
            // Only a map decodes; anything else is no tool block
            // (`rc_feed.dart:54-56`).
            tool: o
                .get("tool")
                .and_then(serde_json::Value::as_object)
                .map(RcFeedTool::from_map),
            // Same map-only rule as `tool`: anything else is no approval block,
            // so an `approval_request` row with a malformed block degrades to an
            // un-actionable message instead of a decode failure.
            approval: o
                .get("approval")
                .and_then(serde_json::Value::as_object)
                .map(RcFeedApproval::from_map),
        }
    }

    /// Whether this row is an `approval_request` carrying a usable approval block
    /// — the predicate a client keys its approve/deny affordance off (a row typed
    /// `approval_request` with no decodable block is not actionable).
    pub fn approval_request(&self) -> Option<&RcFeedApproval> {
        if self.msg_type == "approval_request" {
            self.approval.as_ref()
        } else {
            None
        }
    }
}

/// A page of the feed: `GET …/messages?since=<seq>[&limit=<n>]`. `truncated`
/// means the requested `since` cursor predates the ring (drop-oldest discarded
/// unseen messages) OR points beyond the ring's current tail (the ring
/// restarted) — in either case the client must refetch from the earliest
/// retained message (`internal/ext/rc/hub_messages.go:129-140`). Mirrors
/// mobile's `RcMessagesPage` (`rc_feed.dart:65-81`).
#[derive(Debug, Clone, PartialEq, Eq, Default)]
pub struct RcMessagesPage {
    pub messages: Vec<RcFeedMessage>,
    pub truncated: bool,
}

impl RcMessagesPage {
    /// Tolerant decode; never fails. A non-object body, a missing/non-list
    /// `messages` key → `[]`; only map elements decode to messages
    /// (`rc_feed.dart:70-80`); `truncated` is `true` only when literally
    /// boolean `true`.
    pub fn from_value(v: &serde_json::Value) -> RcMessagesPage {
        let Some(obj) = v.as_object() else {
            return RcMessagesPage::default();
        };
        RcMessagesPage {
            messages: obj
                .get("messages")
                .and_then(serde_json::Value::as_array)
                .map(|rows| {
                    rows.iter()
                        .filter_map(serde_json::Value::as_object)
                        .map(RcFeedMessage::from_map)
                        .collect()
                })
                .unwrap_or_default(),
            truncated: matches!(obj.get("truncated"), Some(serde_json::Value::Bool(true))),
        }
    }
}

impl<'de> Deserialize<'de> for RcMessagesPage {
    fn deserialize<D: serde::Deserializer<'de>>(d: D) -> Result<Self, D::Error> {
        Ok(RcMessagesPage::from_value(&serde_json::Value::deserialize(
            d,
        )?))
    }
}

// ---- SSH ----

/// Build the **non-interactive** ssh argv that runs `remote_argv` on the target.
///
/// Critically NOT `terminal::ssh_command`: RC must have **no `-t`** — a PTY merges
/// stderr into stdout and injects terminal control bytes, which corrupts the JSON
/// DTO decode. Adds `BatchMode=yes` (no prompts) + the shared host-key opts +
/// `ConnectTimeout`, and shell-quotes the remote command into one string after
/// `--`. Mirrors Swift `RemoteControl.sshArgv`.
pub fn ssh_argv(
    user: &str,
    host: &str,
    port: u16,
    known_hosts: &str,
    remote_argv: &[String],
    connect_timeout: u32,
) -> Vec<String> {
    let remote = remote_argv
        .iter()
        .map(|a| shell_quote(a))
        .collect::<Vec<_>>()
        .join(" ");
    let mut argv = vec![
        "ssh".to_string(),
        "-o".to_string(),
        "BatchMode=yes".to_string(),
    ];
    argv.extend(ssh_host_key_opts(known_hosts));
    argv.push("-o".to_string());
    argv.push(format!("ConnectTimeout={connect_timeout}"));
    argv.push("-p".to_string());
    argv.push(port.to_string());
    argv.push(format!("{user}@{host}"));
    argv.push("--".to_string());
    argv.push(remote);
    argv
}

// ---- pure pane classifier ----

static RE_TRUST_FOLDER: LazyLock<Regex> =
    LazyLock::new(|| Regex::new(r"(?i)Yes,\s*I trust this folder").unwrap());
static RE_RECONNECTING: LazyLock<Regex> =
    LazyLock::new(|| Regex::new(r"\bReconnecting\b").unwrap());
static RE_URL_BROKER: LazyLock<Regex> = LazyLock::new(|| {
    Regex::new(r"https?://claude\.ai/code\?environment=env_[A-Za-z0-9_-]+").unwrap()
});
static RE_URL_SESSION: LazyLock<Regex> =
    LazyLock::new(|| Regex::new(r"https?://claude\.ai/code/session_[A-Za-z0-9_-]+").unwrap());

/// Classify a tmux pane capture into a session state (+ url). Mirrors Swift's
/// `RemoteControl.classifyPane`. The pane's status words (`connecting`/`active`/
/// `Connected`) are informational: the extracted claude.ai URL is the actual
/// "ready" signal (as in Swift, where a bare URL already means ready regardless
/// of the banner text), so only the trust/auth heuristics + the broker
/// `Reconnecting` state gate the outcome. The pane is lowercased once for the
/// case-insensitive substring checks.
pub fn classify_pane(kind: &RcKind, pane: &str) -> RcClassification {
    let is_claude = matches!(kind, RcKind::ClaudeRc | RcKind::ClaudeBroker);
    // Trust + auth heuristics use claude-specific pane text, so they gate ONLY the
    // claude kinds. The per-agent pane classifiers for codex/opencode/cursor are
    // owned by the guest binary (`internal/ext/rc/agents.go`), authoritative over
    // the client — clients consume the DTO's `state`; this pure classifier stays a
    // best-effort utility and renders every non-claude/unknown kind neutrally.
    if is_claude {
        let lower = pane.to_lowercase();
        if lower.contains("workspace not trusted")
            || lower.contains("quick safety check")
            || RE_TRUST_FOLDER.is_match(pane)
        {
            return RcClassification {
                state: RcState::NeedsTrust,
                url: extract_url(kind, pane),
            };
        }
        if lower.contains("requires a claude.ai subscription")
            || lower.contains("not logged in")
            || lower.contains("claude auth login")
        {
            return RcClassification {
                state: RcState::NeedsAuth,
                url: extract_url(kind, pane),
            };
        }
    }

    match kind {
        RcKind::ClaudeBroker => {
            let url = extract_url(&RcKind::ClaudeBroker, pane);
            // Reconnecting takes precedence over a (possibly stale) url — Swift parity.
            if RE_RECONNECTING.is_match(pane) {
                return RcClassification {
                    state: RcState::Reconnecting,
                    url,
                };
            }
            classify_by_url(url)
        }
        RcKind::ClaudeRc => classify_by_url(extract_url(&RcKind::ClaudeRc, pane)),
        // Shell, the non-claude agent kinds (codex/opencode/cursor), and unknown
        // kinds: neutral — blank pane is still starting, anything drawn reads ready,
        // and no claude URL is attached (the guest owns the real per-agent states).
        _ => RcClassification {
            state: if pane.trim().is_empty() {
                RcState::Starting
            } else {
                RcState::Ready
            },
            url: None,
        },
    }
}

/// A present url means ready; its absence means still starting.
fn classify_by_url(url: Option<String>) -> RcClassification {
    match url {
        Some(u) => RcClassification {
            state: RcState::Ready,
            url: Some(u),
        },
        None => RcClassification {
            state: RcState::Starting,
            url: None,
        },
    }
}

/// Extract the claude.ai URL for the given kind (broker uses `?environment=env_…`,
/// claude-rc uses `/session_…`).
pub fn extract_url(kind: &RcKind, pane: &str) -> Option<String> {
    let re = match kind {
        RcKind::ClaudeBroker => &*RE_URL_BROKER,
        RcKind::ClaudeRc => &*RE_URL_SESSION,
        _ => return None,
    };
    re.find(pane).map(|m| m.as_str().to_string())
}

#[cfg(test)]
mod tests {
    use super::*;

    // ---- classifier (mirrors test_agents.py) ----

    #[test]
    fn classify_broker_ready_with_environment_url() {
        let pane = "·✔︎· Connected\nContinue at https://claude.ai/code?environment=env_01ABC";
        let c = classify_pane(&RcKind::ClaudeBroker, pane);
        assert_eq!(c.state, RcState::Ready);
        assert_eq!(
            c.url.as_deref(),
            Some("https://claude.ai/code?environment=env_01ABC")
        );
    }

    #[test]
    fn classify_repl_needs_trust() {
        let c = classify_pane(
            &RcKind::ClaudeRc,
            "Quick safety check: Is this a project you trust?",
        );
        assert_eq!(c.state, RcState::NeedsTrust);
    }

    #[test]
    fn classify_trust_folder_button_needs_trust() {
        let c = classify_pane(&RcKind::ClaudeRc, "  Yes,  I trust this folder  ");
        assert_eq!(c.state, RcState::NeedsTrust);
    }

    #[test]
    fn classify_needs_auth() {
        for pane in [
            "not logged in",
            "run claude auth login",
            "requires a claude.ai subscription",
        ] {
            assert_eq!(
                classify_pane(&RcKind::ClaudeRc, pane).state,
                RcState::NeedsAuth
            );
        }
    }

    #[test]
    fn classify_broker_reconnecting_no_url() {
        let c = classify_pane(&RcKind::ClaudeBroker, "·|· Reconnecting · retrying in 2.5s");
        assert_eq!(c.state, RcState::Reconnecting);
        assert!(c.url.is_none());
    }

    #[test]
    fn classify_rc_ready_with_session_url() {
        let pane = "Remote Control active\nhttps://claude.ai/code/session_XYZ789";
        let c = classify_pane(&RcKind::ClaudeRc, pane);
        assert_eq!(c.state, RcState::Ready);
        assert_eq!(
            c.url.as_deref(),
            Some("https://claude.ai/code/session_XYZ789")
        );
    }

    #[test]
    fn classify_rc_connecting_is_starting() {
        let c = classify_pane(&RcKind::ClaudeRc, "Remote Control connecting…");
        assert_eq!(c.state, RcState::Starting);
        assert!(c.url.is_none());
    }

    #[test]
    fn classify_shell_empty_vs_content() {
        assert_eq!(
            classify_pane(&RcKind::Shell, "   \n ").state,
            RcState::Starting
        );
        assert_eq!(classify_pane(&RcKind::Shell, "$ ls").state, RcState::Ready);
        // A shell never runs the trust/auth heuristics.
        assert_eq!(
            classify_pane(&RcKind::Shell, "not logged in").state,
            RcState::Ready
        );
    }

    #[test]
    fn classification_serializes_state_kebab_and_omits_none_url() {
        let j = serde_json::to_value(RcClassification {
            state: RcState::NeedsTrust,
            url: None,
        })
        .unwrap();
        assert_eq!(j["state"], "needs-trust");
        assert!(j.get("url").is_none());
    }

    // ---- guest-text sanitization ----

    #[test]
    fn strip_format_chars_removes_cf_category() {
        // Bidi override (U+202E), zero-width space (U+200B), BOM (U+FEFF).
        assert_eq!(strip_format_chars("safe\u{202E}txt"), "safetxt");
        assert_eq!(strip_format_chars("a\u{200B}b\u{FEFF}c"), "abc");
        assert_eq!(strip_format_chars("\u{202E}"), "");
        // Non-Cf text passes through untouched (incl. non-ASCII).
        assert_eq!(strip_format_chars("héllo → wörld"), "héllo → wörld");
    }

    // ---- rc hub messages feed (ported from mobile's rc_feed_test.dart:9-67) ----

    fn page(json: &str) -> RcMessagesPage {
        serde_json::from_str(json).unwrap()
    }

    #[test]
    fn feed_page_decodes_mixed_text_and_tool_blocks() {
        // rc_feed_test.dart:11-31.
        let p = page(
            r#"{
              "messages": [
                {"seq": 1, "ts": "2026-06-19T18:53:00Z", "role": "user", "type": "text", "text": "hi"},
                {"seq": 2, "role": "assistant", "type": "reasoning", "text": "thinking"},
                {"seq": 3, "role": "tool", "type": "tool_use",
                 "tool": {"name": "shell", "detail": "ls -la"}}
              ],
              "truncated": false
            }"#,
        );
        assert_eq!(p.messages.len(), 3);
        assert!(!p.truncated);
        assert_eq!(p.messages[0].role, "user");
        assert_eq!(p.messages[0].text.as_deref(), Some("hi"));
        assert_eq!(p.messages[0].ts.as_deref(), Some("2026-06-19T18:53:00Z"));
        assert_eq!(p.messages[1].msg_type, "reasoning");
        let tool = p.messages[2].tool.as_ref().unwrap();
        assert_eq!(tool.name.as_deref(), Some("shell"));
        assert_eq!(tool.detail.as_deref(), Some("ls -la"));
        assert_eq!(p.messages[2].text, None);
    }

    #[test]
    fn feed_page_truncated_flag_and_empty_list_not_null() {
        // rc_feed_test.dart:33-37: [] decodes to an empty page, flag carried.
        let p = page(r#"{"messages": [], "truncated": true}"#);
        assert!(p.messages.is_empty());
        assert!(p.truncated);
        // truncated is true ONLY when literally boolean true.
        let p = page(r#"{"messages": [], "truncated": "yes"}"#);
        assert!(!p.truncated);
    }

    #[test]
    fn feed_page_tolerates_missing_messages_key_and_absent_fields() {
        // rc_feed_test.dart:39-49.
        let p = page(r#"{"truncated": false}"#);
        assert!(p.messages.is_empty());
        let one = page(r#"{"messages":[{"seq":5,"role":"system","type":"status"}]}"#);
        assert_eq!(one.messages.len(), 1);
        assert_eq!(one.messages[0].seq, 5);
        assert_eq!(one.messages[0].ts, None);
        assert_eq!(one.messages[0].text, None);
        assert!(one.messages[0].tool.is_none());
    }

    #[test]
    fn feed_page_skips_non_map_elements_and_wrong_types() {
        // Only Map elements decode (rc_feed.dart:74-77); wrong-typed fields
        // degrade per-field, never error.
        let p = page(
            r#"{"messages":[42,"nope",{"seq":"x","role":7,"type":null,"tool":"bad"}],
                "truncated":null}"#,
        );
        assert_eq!(p.messages.len(), 1); // non-maps skipped
        let m = &p.messages[0];
        assert_eq!(m.seq, 0); // non-numeric → 0
        assert_eq!(m.role, ""); // non-string → ""
        assert_eq!(m.msg_type, "");
        assert!(m.tool.is_none()); // non-map tool → None
        assert!(!p.truncated); // null → false
                               // A non-object body degrades to the empty page.
        assert_eq!(
            RcMessagesPage::from_value(&serde_json::json!([1, 2])),
            RcMessagesPage::default()
        );
    }

    #[test]
    fn feed_page_strips_format_chars_from_text_and_tool_fields() {
        // rc_feed_test.dart:51-65: the hub strips ANSI + C0/C1 controls but
        // not category Cf — a U+202E (RLO) can visually reverse rendered
        // text; U+200B hides content.
        let p = page(
            "{\"messages\":[{\"seq\":1,\"role\":\"assistant\",\"type\":\"text\",\
              \"text\":\"safe\u{202E} EVIL\"},\
             {\"seq\":2,\"role\":\"tool\",\"type\":\"tool_use\",\
              \"tool\":{\"name\":\"sh\u{200B}ell\",\"detail\":\"rm\u{202E} x\"}}]}",
        );
        assert_eq!(p.messages[0].text.as_deref(), Some("safe EVIL"));
        let tool = p.messages[1].tool.as_ref().unwrap();
        assert_eq!(tool.name.as_deref(), Some("shell"));
        assert_eq!(tool.detail.as_deref(), Some("rm x"));
        // Cf-only text degrades to None.
        let p = page("{\"messages\":[{\"seq\":1,\"role\":\"assistant\",\"type\":\"text\",\"text\":\"\u{202E}\"}]}");
        assert_eq!(p.messages[0].text, None);
    }

    // ---- prompt normalization ----

    #[test]
    fn normalize_prompt_trims_and_accepts() {
        assert_eq!(
            normalize_rc_prompt(Some("  summarize this repo\n"), &RcKind::ClaudeRc).unwrap(),
            Some("summarize this repo".to_string())
        );
    }

    #[test]
    fn normalize_prompt_blank_is_none() {
        assert_eq!(
            normalize_rc_prompt(Some("   \n\t"), &RcKind::ClaudeRc).unwrap(),
            None
        );
        assert_eq!(normalize_rc_prompt(None, &RcKind::Shell).unwrap(), None);
    }

    #[test]
    fn normalize_prompt_rejects_control_char() {
        assert!(matches!(
            normalize_rc_prompt(Some("bad\nvalue"), &RcKind::ClaudeRc),
            Err(RcError::BadRequest(_))
        ));
    }

    #[test]
    fn normalize_prompt_rejects_overlong() {
        let big = "a".repeat(2001);
        assert!(matches!(
            normalize_rc_prompt(Some(&big), &RcKind::Shell),
            Err(RcError::BadRequest(_))
        ));
        // Exactly 2000 bytes is fine.
        assert!(normalize_rc_prompt(Some(&"a".repeat(2000)), &RcKind::Shell)
            .unwrap()
            .is_some());
    }

    #[test]
    fn normalize_prompt_rejects_for_broker() {
        assert!(matches!(
            normalize_rc_prompt(Some("nope"), &RcKind::ClaudeBroker),
            Err(RcError::BadRequest(_))
        ));
    }

    // ---- ssh argv (the H1 guard) ----

    #[test]
    fn ssh_argv_is_non_interactive_and_quotes_remote() {
        let remote = vec!["shed-ext-rc".to_string(), "list".to_string()];
        let argv = ssh_argv("web", "10.0.0.5", 2222, "/k/known_hosts", &remote, 10);
        // No `-t` (a PTY would corrupt the JSON DTO decode).
        assert!(
            !argv.contains(&"-t".to_string()),
            "RC ssh must not allocate a PTY"
        );
        assert!(argv.windows(2).any(|w| w == ["-o", "BatchMode=yes"]));
        assert!(argv.contains(&"ConnectTimeout=10".to_string()));
        assert!(argv
            .windows(2)
            .any(|w| w == ["-o", "StrictHostKeyChecking=yes"]));
        // The remote command is a single shell-quoted string after `--`.
        let dd = argv.iter().position(|a| a == "--").unwrap();
        assert_eq!(argv[dd + 1], "shed-ext-rc list");
        assert_eq!(argv.last().unwrap(), "shed-ext-rc list");
        // user@host precedes the `--`.
        assert!(argv.contains(&"web@10.0.0.5".to_string()));
    }

    #[test]
    fn ssh_argv_shell_quotes_a_prompt_arg() {
        let remote = vec![
            "shed-ext-rc".to_string(),
            "create".to_string(),
            "a b".to_string(),
        ];
        let argv = ssh_argv("s", "h", 22, "/k", &remote, 10);
        assert_eq!(argv.last().unwrap(), "shed-ext-rc create 'a b'");
    }

    // ---- create/list/kill argv ----

    #[test]
    fn create_argv_shape_with_prompt_and_workdir() {
        let a = create_argv(
            "shed-ext-rc",
            &RcKind::ClaudeRc,
            "web/abc",
            "abc",
            Some("/work"),
            "shed-desktop/1.0",
            "shed:web@srv",
            None,
            true,
        );
        assert_eq!(a[0], "shed-ext-rc");
        assert_eq!(a[1], "create");
        assert!(a.windows(2).any(|w| w == ["--kind", "claude-rc"]));
        assert!(a.windows(2).any(|w| w == ["--slug", "abc"]));
        assert!(a.windows(2).any(|w| w == ["--workdir", "/work"]));
        assert!(a.contains(&"--wait".to_string()));
        assert!(a.contains(&"--prompt-stdin".to_string()));
    }

    #[test]
    fn create_argv_omits_empty_workdir_and_promptless() {
        let a = create_argv(
            "b",
            &RcKind::Shell,
            "n",
            "s",
            Some(""),
            "c",
            "t",
            None,
            false,
        );
        assert!(!a.contains(&"--workdir".to_string()));
        assert!(!a.contains(&"--prompt-stdin".to_string()));
    }

    #[test]
    fn create_invocation_drops_prompt_for_broker() {
        let (argv, stdin) = create_invocation(
            "b",
            &RcKind::ClaudeBroker,
            "n",
            "s",
            None,
            "c",
            "t",
            None,
            Some("hi"),
        )
        .unwrap();
        assert_eq!(stdin, None);
        assert!(!argv.contains(&"--prompt-stdin".to_string()));
    }

    // ---- permission modes (ported from mobile's rc_service_test.dart:58-253) ----

    #[test]
    fn default_permission_mode_is_a_member_of_both_sets() {
        // rc_service_test.dart:59-64: the picker pre-selects the default; it must
        // be a member of the full claude set AND (being generic) every kind's set.
        assert_eq!(DEFAULT_RC_PERMISSION_MODE, "auto");
        assert!(GENERIC_PERMISSION_MODES.contains(&DEFAULT_RC_PERMISSION_MODE));
        assert!(permission_modes_for(&RcKind::ClaudeRc).contains(&DEFAULT_RC_PERMISSION_MODE));
        assert!(permission_modes_for(&RcKind::Codex).contains(&DEFAULT_RC_PERMISSION_MODE));
    }

    #[test]
    fn permission_modes_for_claude_is_union_others_generic_only() {
        // rc_service.dart:58-59: claude kinds get generic ∪ extras (display
        // order: tri-state first); the other kinds get the tri-state only.
        for kind in [RcKind::ClaudeRc, RcKind::ClaudeBroker] {
            assert!(kind.runs_claude());
            assert_eq!(
                permission_modes_for(&kind),
                vec![
                    "default",
                    "auto",
                    "skip",
                    "acceptEdits",
                    "plan",
                    "dontAsk",
                    "bypassPermissions"
                ]
            );
        }
        for kind in [
            RcKind::Codex,
            RcKind::Opencode,
            RcKind::Cursor,
            RcKind::Shell,
            RcKind::Other("borg".into()),
        ] {
            assert!(!kind.runs_claude());
            assert_eq!(permission_modes_for(&kind), vec!["default", "auto", "skip"]);
        }
    }

    #[test]
    fn has_permission_mode_excludes_shell_and_unknown() {
        // rc_models.dart:81: every known agent kind has a permission posture;
        // shell has none, and an unknown kind renders neutrally with none.
        for kind in [
            RcKind::ClaudeRc,
            RcKind::ClaudeBroker,
            RcKind::Codex,
            RcKind::Opencode,
            RcKind::Cursor,
        ] {
            assert!(kind.has_permission_mode());
        }
        assert!(!RcKind::Shell.has_permission_mode());
        assert!(!RcKind::Other("borg".into()).has_permission_mode());
    }

    #[test]
    fn validate_permission_mode_accepts_valid_modes() {
        // rc_service_test.dart:183-201: codex takes the generic tri-state;
        // rc_service_test.dart:145-158, 231-241: claude takes its full set.
        assert_eq!(
            validate_permission_mode(&RcKind::Codex, Some("auto")),
            Ok(Some("auto"))
        );
        assert_eq!(
            validate_permission_mode(&RcKind::Codex, Some("skip")),
            Ok(Some("skip"))
        );
        assert_eq!(
            validate_permission_mode(&RcKind::ClaudeRc, Some("bypassPermissions")),
            Ok(Some("bypassPermissions"))
        );
        assert_eq!(
            validate_permission_mode(&RcKind::ClaudeRc, Some("auto")),
            Ok(Some("auto"))
        );
        assert_eq!(
            validate_permission_mode(&RcKind::ClaudeRc, Some("plan")),
            Ok(Some("plan"))
        );
        // No mode chosen → no flag (each tool's own default),
        // rc_service_test.dart:243-253.
        assert_eq!(validate_permission_mode(&RcKind::ClaudeRc, None), Ok(None));
    }

    #[test]
    fn validate_permission_mode_rejects_invalid_before_any_argv() {
        // rc_service_test.dart:171-181 (unknown mode) + 203-217 (a claude-only
        // mode on a non-claude kind) → RC_BAD_REQUEST, never reaching SSH.
        assert_eq!(
            validate_permission_mode(&RcKind::Codex, Some("plan")),
            Err(RcError::BadRequest("invalid permission mode".to_string()))
        );
        assert_eq!(
            validate_permission_mode(&RcKind::ClaudeRc, Some("nope")),
            Err(RcError::BadRequest("invalid permission mode".to_string()))
        );
    }

    #[test]
    fn validate_permission_mode_drops_silently_for_shell_and_unknown() {
        // rc_service_test.dart:160-169: a shell has no permission mode; the mode
        // is silently dropped (no error, no flag) even if a caller passes one —
        // state can linger across a kind switch. Same for an unknown kind.
        assert_eq!(
            validate_permission_mode(&RcKind::Shell, Some("auto")),
            Ok(None)
        );
        assert_eq!(
            validate_permission_mode(&RcKind::Shell, Some("plan")),
            Ok(None)
        );
        assert_eq!(
            validate_permission_mode(&RcKind::Other("borg".into()), Some("auto")),
            Ok(None)
        );
    }

    #[test]
    fn create_argv_emits_permission_mode_between_workdir_and_prompt_stdin() {
        // rc_service_test.dart:145-158 + the emission ordering of
        // rc_service.dart:168-174: --workdir, then --permission-mode, then
        // --prompt-stdin.
        let a = create_argv(
            "shed-ext-rc",
            &RcKind::ClaudeRc,
            "web/abc",
            "abc",
            Some("/work/dir"),
            "shed-desktop/1.0",
            "shed:web@srv",
            Some("bypassPermissions"),
            true,
        );
        assert!(a
            .windows(2)
            .any(|w| w == ["--permission-mode", "bypassPermissions"]));
        let wd = a.iter().position(|x| x == "--workdir").unwrap();
        let pm = a.iter().position(|x| x == "--permission-mode").unwrap();
        let ps = a.iter().position(|x| x == "--prompt-stdin").unwrap();
        assert!(
            wd < pm && pm < ps,
            "ordering must be --workdir < --permission-mode < --prompt-stdin"
        );
    }

    #[test]
    fn create_argv_omits_permission_mode_when_none() {
        // rc_service_test.dart:243-253: a null mode means "pass no flag at all".
        let a = create_argv(
            "shed-ext-rc",
            &RcKind::ClaudeRc,
            "n",
            "s",
            None,
            "c",
            "t",
            None,
            false,
        );
        assert!(!a.contains(&"--permission-mode".to_string()));
    }

    #[test]
    fn create_invocation_passes_permission_mode_through() {
        // rc_service_test.dart:183-193: codex passes a generic mode; the
        // invocation gate validates it and emits the flag.
        let (argv, stdin) = create_invocation(
            "b",
            &RcKind::Codex,
            "n",
            "s",
            None,
            "c",
            "t",
            Some("auto"),
            None,
        )
        .unwrap();
        assert!(argv.windows(2).any(|w| w == ["--permission-mode", "auto"]));
        assert_eq!(stdin, None);
    }

    #[test]
    fn create_invocation_rejects_invalid_mode_and_builds_no_argv() {
        // rc_service_test.dart:203-217: a claude-only mode on a non-claude kind
        // is rejected (RC_BAD_REQUEST) BEFORE any argv/SSH — the invocation gate
        // validates, it does not forward raw modes.
        assert_eq!(
            create_invocation(
                "b",
                &RcKind::Codex,
                "n",
                "s",
                None,
                "c",
                "t",
                Some("plan"),
                None
            ),
            Err(RcError::BadRequest("invalid permission mode".to_string()))
        );
    }

    #[test]
    fn create_invocation_silently_drops_mode_for_shell() {
        // rc_service_test.dart:160-169: a shell has no permission mode; even a
        // GARBAGE mode is dropped silently (Ok, no flag, no error) — Dart derives
        // the effective mode BEFORE validating (rc_service.dart:142).
        let (argv, _) = create_invocation(
            "b",
            &RcKind::Shell,
            "n",
            "s",
            None,
            "c",
            "t",
            Some("garbage"),
            None,
        )
        .unwrap();
        assert!(!argv.contains(&"--permission-mode".to_string()));
    }

    #[test]
    fn list_and_kill_argv() {
        assert_eq!(list_argv("b"), ["b", "list"]);
        assert_eq!(kill_argv("b", "abc"), ["b", "kill", "--slug", "abc"]);
    }

    #[test]
    fn prompt_argv_builder() {
        assert_eq!(
            prompt_argv("b", "abc", None),
            ["b", "prompt", "--slug", "abc"]
        );
        assert_eq!(
            prompt_argv("b", "abc", Some("sid")),
            ["b", "prompt", "--slug", "abc", "--session-id", "sid"]
        );
        // An empty session id is guarded — no `--session-id` flag emitted.
        assert_eq!(
            prompt_argv("b", "abc", Some("")),
            ["b", "prompt", "--slug", "abc"]
        );
    }

    // ---- exit-code mapping ----

    #[test]
    fn error_from_exit_maps_codes() {
        assert_eq!(
            error_from_exit(3, "taken", ""),
            RcError::SlugTaken("taken".into())
        );
        assert_eq!(
            error_from_exit(4, "gone", ""),
            RcError::NotFound("gone".into())
        );
        assert_eq!(
            error_from_exit(2, "bad", ""),
            RcError::BadRequest("bad".into())
        );
        assert_eq!(error_from_exit(127, "", ""), RcError::MissingBinary);
        assert_eq!(
            error_from_exit(1, "bash: shed-ext-rc: command not found", ""),
            RcError::MissingBinary
        );
        assert_eq!(
            error_from_exit(1, "", ""),
            RcError::Failed("shed-ext-rc exited 1".into())
        );
        // stdout is the fallback detail when stderr is empty.
        assert_eq!(
            error_from_exit(5, "", "boom"),
            RcError::Failed("boom".into())
        );
    }

    // ---- DTO → RcSession ----

    #[test]
    fn from_dto_injects_host_shed_and_falls_back() {
        let dto = RcSessionDto {
            slug: "abc".into(),
            tmux_session: "rc-abc".into(),
            kind: RcKind::ClaudeRc,
            state: RcState::Ready,
            lane: Some(LANE_TUI.to_string()),
            managed: true,
            display_name: None,
            workdir: None,
            url: Some("u".into()),
            id: Some("id-1".into()),
            created_by: Some("shed-desktop/1.0".into()),
            created_at: Some("2026-01-01T00:00:00Z".into()),
            target_label: None,
            activity: None,
            activity_at: None,
            last_message: None,
            pending_approvals: None,
        };
        let s = RcSession::from_dto(dto, "srv", "web");
        assert_eq!(s.host, "srv");
        assert_eq!(s.shed, "web");
        assert_eq!(s.display_name, "web/abc"); // fallback
        assert_eq!(s.workdir.as_deref(), Some(DEFAULT_WORKDIR)); // fallback
        assert_eq!(s.rc_id.as_deref(), Some("id-1")); // id → rc_id
        assert_eq!(s.id(), "srv/web/abc");
        assert_eq!(s.lane.as_deref(), Some(LANE_TUI)); // propagated verbatim
        assert_eq!(s.lane_or_tui(), LANE_TUI);
    }

    #[test]
    fn rc_session_serializes_expected_keys() {
        let s = RcSession::from_dto(
            RcSessionDto {
                slug: "abc".into(),
                tmux_session: "rc-abc".into(),
                kind: RcKind::Shell,
                state: RcState::Ready,
                lane: Some(LANE_TUI.to_string()),
                managed: false,
                display_name: Some("dev".into()),
                workdir: Some("/w".into()),
                url: None,
                id: None,
                created_by: None,
                created_at: None,
                target_label: None,
                activity: None,
                activity_at: None,
                last_message: None,
                pending_approvals: None,
            },
            "srv",
            "web",
        );
        let j = serde_json::to_value(&s).unwrap();
        assert_eq!(j["tmux_session"], "rc-abc");
        assert_eq!(j["kind"], "shell");
        assert_eq!(j["managed"], false);
        assert_eq!(j["lane"], "tui");
        // None optionals are omitted (Swift's encodeIfPresent parity), and `id`
        // (the computed key) is never on the wire.
        assert!(j.get("url").is_none());
        assert!(j.get("rc_id").is_none());
        assert!(j.get("id").is_none());
        // …including `lane` itself when the producer was too old to send one.
        let legacy = RcSession {
            lane: None,
            ..s.clone()
        };
        assert!(serde_json::to_value(&legacy).unwrap().get("lane").is_none());
        assert_eq!(legacy.lane_or_tui(), LANE_TUI); // read through the fallback
    }

    #[test]
    fn decode_list_is_strict_like_swift() {
        // Empty / null rc_sessions / a DTO missing a required field are all errors
        // (the fan-out drops them) — matching Swift's strict decodeList + DTO,
        // never masking a broken shed-ext-rc response as "no sessions".
        assert!(decode_list("").is_err());
        assert!(decode_list(r#"{"rc_sessions": null}"#).is_err());
        assert!(decode_list(r#"{"rc_sessions":[{"slug":"a"}]}"#).is_err()); // missing required fields
        let one = decode_list(
            r#"{"rc_sessions":[{"slug":"a","tmux_session":"rc-a","kind":"shell","state":"ready","managed":true}]}"#,
        )
        .unwrap();
        assert_eq!(one.len(), 1);
        assert!(one[0].managed);
    }

    #[test]
    fn decode_session_rejects_garbage() {
        assert!(matches!(
            decode_session("not json"),
            Err(RcError::Failed(_))
        ));
        // A missing required field is not a valid DTO.
        assert!(matches!(
            decode_session(r#"{"slug":"x"}"#),
            Err(RcError::Failed(_))
        ));
    }

    /// The crates-local copy of the canonical `list` golden
    /// (`internal/ext/rc/testdata/rcSessionDto.golden.json`), byte-compared
    /// against every other copy by the Go parity test in `internal/ext/rc`. It
    /// lives under `crates/` — not read across the tree — because the
    /// `make -C desktop core-linux` Docker leg mounts only this workspace.
    const LIST_GOLDEN: &str = include_str!("../../fixtures/rcSessionDto.golden.json");

    /// Decode the canonical golden fixture: a full managed session + a minimal
    /// legacy one (only required fields), plus the v4 capabilities block. Pins
    /// cross-repo wire parity for the whole `list` envelope — deliberately
    /// through [`decode_list_response`], the only decode path that VALIDATES the
    /// capabilities half.
    #[test]
    fn decode_list_matches_golden_fixture() {
        let resp = decode_list_response(LIST_GOLDEN).unwrap();
        let dtos = resp.rc_sessions;
        assert_eq!(dtos.len(), 2);
        // Full session: all fields present, id → rc_id via from_dto.
        let full = RcSession::from_dto(dtos[0].clone(), "mini3", "demo");
        assert!(full.managed);
        assert_eq!(full.kind, RcKind::ClaudeRc);
        assert_eq!(full.display_name, "charliek/abc234"); // present, not the fallback
        assert_eq!(
            full.rc_id.as_deref(),
            Some("9f1c0e7a-1111-4222-8333-444455556666")
        );
        assert_eq!(full.created_by.as_deref(), Some("shed-remote-agent/0.1.0"));
        assert_eq!(full.activity, Some(RcActivity::Working));
        // Minimal legacy session: absent optionals default, fallbacks applied.
        assert!(!dtos[1].managed);
        let minimal = RcSession::from_dto(dtos[1].clone(), "h", "demo");
        assert_eq!(minimal.display_name, "demo/brk900"); // <shed>/<slug> fallback
        assert_eq!(minimal.workdir.as_deref(), Some(DEFAULT_WORKDIR)); // fallback
        assert!(minimal.rc_id.is_none());
        // `lane` is ALWAYS present on the wire — on the managed row and on the
        // minimal unmanaged one alike (contract v2's always-present rule).
        assert_eq!(dtos[0].lane.as_deref(), Some(LANE_TUI));
        assert_eq!(dtos[1].lane.as_deref(), Some(LANE_TUI));
        assert_eq!(full.lane_or_tui(), LANE_TUI);
        assert_eq!(minimal.lane_or_tui(), LANE_TUI);
        // Nothing produces approvals in this phase.
        assert!(dtos[0].pending_approvals.is_none());

        // Capabilities: the v4 block, validated (not skipped) on this path.
        let caps = resp.capabilities.expect("golden carries capabilities");
        assert_eq!(caps.rc_version, 4);
        assert!(caps.has_feature("contract-v2")); // the route-existence token
        assert!(caps.has_feature("messages")); // v1 tokens retained
        let codex = &caps.kind_features["codex"];
        assert_eq!(codex.feed, "messages");
        assert!(codex.feed_messages());
        assert!(!codex.interrupt); // no lane implements the verb yet
        assert_eq!(codex.attach_kind(), ATTACH_TMUX);
        assert!(codex.watch); // held in lockstep with feed == "messages"
        assert!(codex.input_gated());
        // opencode is the first LIVE lane: whole turns, interrupt and remotely
        // answerable approvals all go through the hub's verbs, so its row diverges
        // from codex's on purpose (input "turn" supersedes "gated" — the single
        // input field is one-of, so `/input` no longer applies to the kind).
        let opencode = &caps.kind_features["opencode"];
        assert_eq!(opencode.feed, "messages");
        assert_eq!(opencode.input, "turn");
        assert!(!opencode.input_gated());
        assert_eq!(opencode.approvals, "remote");
        assert!(opencode.interrupt);
        assert_eq!(opencode.attach_kind(), ATTACH_TMUX);
        // cursor joined the feed kinds in plan 008: its own hook scripts push turn
        // boundaries, tool calls and messages into the hub, and its composer anchor gates
        // `/input` — so its row reads like codex's (messages + gated), while approvals stay
        // "tui" (the hub can only OBSERVE cursor's prompt, never answer it).
        let cursor = &caps.kind_features["cursor"];
        assert_eq!(cursor.feed, "messages");
        assert!(cursor.feed_messages());
        assert!(cursor.input_gated());
        assert_eq!(cursor.approvals, "tui");
        assert!(!cursor.interrupt);
        assert_eq!(cursor.attach_kind(), ATTACH_TMUX);
        // A kind with the activity dimension only: no message feed, still tmux.
        let claude = &caps.kind_features["claude-rc"];
        assert_eq!(claude.feed, "activity");
        assert!(!claude.feed_messages());
        assert!(!claude.watch); // lockstep the other way
        assert_eq!(claude.attach_kind(), ATTACH_TMUX);
        // claude-broker and shell stay OMITTED — absent entry = no affordances.
        assert!(!caps.kind_features.contains_key("claude-broker"));
        assert!(!caps.kind_features.contains_key("shell"));
    }

    /// The `Serialize` half added for the Rust rc engine (plan 009) must be a
    /// faithful inverse of the decoder: decode the canonical golden → re-encode →
    /// decode again → identical. A `skip_serializing_if` that is too eager (an
    /// emitted field dropped) or too lax (an absent field materialized as
    /// `null`/`""`) breaks this immediately, which is exactly the class of bug the
    /// Go↔Rust differential would otherwise catch a whole commit later.
    #[test]
    fn list_envelope_round_trips_through_serialization() {
        let first = decode_list_response(LIST_GOLDEN).unwrap();
        let encoded = serde_json::to_string(&first).unwrap();
        let second = decode_list_response(&encoded).unwrap();
        assert_eq!(first, second);
        // Stronger: the re-encode is STRUCTURALLY identical to the golden itself
        // — the exact comparison model the Go↔Rust parity harness applies to DTO
        // stdout (parse → compare; key order irrelevant, key PRESENCE contract).
        // Byte comparison is deliberately NOT asserted: Go's `json.Marshal` sorts
        // map keys and HTML-escapes `<`/`>`/`&`, serde_json does neither, and no
        // consumer of this stdout byte-compares it (they all parse).
        assert_eq!(
            serde_json::from_str::<serde_json::Value>(&encoded).unwrap(),
            serde_json::from_str::<serde_json::Value>(LIST_GOLDEN).unwrap(),
        );
    }

    /// Mirrors the Go producer's `TestSessionMarshalOmitsEmptyOptionals`
    /// (`internal/ext/rc/golden_test.go:119`): a minimal DTO re-marshals with its
    /// optional fields ABSENT (not `null`, not `""`), while `managed` and `lane`
    /// are the always-present exceptions. This is the wire contract the Swift
    /// Codable and TS Zod consumers rely on.
    #[test]
    fn minimal_session_marshals_without_empty_optionals() {
        let dto = RcSessionDto {
            slug: "x".into(),
            tmux_session: "rc-x".into(),
            kind: RcKind::Shell,
            state: RcState::Starting,
            managed: false,
            lane: Some(LANE_TUI.into()),
            display_name: None,
            workdir: None,
            url: None,
            id: None,
            created_by: None,
            created_at: None,
            target_label: None,
            activity: None,
            activity_at: None,
            last_message: None,
            pending_approvals: None,
        };
        let s = serde_json::to_string(&dto).unwrap();
        for omitted in [
            "display_name",
            "workdir",
            "url",
            "\"id\"",
            "created_by",
            "created_at",
            "target_label",
            "activity",
            "activity_at",
            "last_message",
            "pending_approvals",
            "null",
        ] {
            assert!(
                !s.contains(omitted),
                "expected {omitted} to be omitted, got {s}"
            );
        }
        // managed is always present (even when false); so is lane.
        assert!(s.contains(r#""managed":false"#), "{s}");
        assert!(s.contains(r#""lane":"tui""#), "{s}");
    }

    /// The `kind_features` omitempty set, pinned on the SERIALIZE side: a zero
    /// `watch`/`input`/`feed`/`attach` must vanish from the payload (so an older
    /// guest's capabilities re-emitted by a newer producer keep triggering the
    /// client-side absent-field fallbacks), while `post_input`, `approvals` and
    /// `interrupt` are always written.
    #[test]
    fn kind_features_marshal_omits_the_go_omitempty_set() {
        let bare = RcKindFeatures {
            post_input: true,
            approvals: "tui".into(),
            watch: false,
            input: String::new(),
            feed: String::new(),
            interrupt: false,
            attach: String::new(),
        };
        let s = serde_json::to_string(&bare).unwrap();
        assert_eq!(
            s,
            r#"{"post_input":true,"approvals":"tui","interrupt":false}"#
        );
        // A populated row keeps every field.
        let full = RcKindFeatures {
            watch: true,
            input: "gated".into(),
            feed: "messages".into(),
            attach: "tmux".into(),
            ..bare
        };
        let s = serde_json::to_string(&full).unwrap();
        for key in ["watch", "input", "feed", "attach"] {
            assert!(s.contains(key), "{key} missing from {s}");
        }
        // `version` on an uninstalled agent is omitempty on the Go side too.
        let uninstalled = RcAgentInfo {
            installed: false,
            version: None,
        };
        assert_eq!(
            serde_json::to_string(&uninstalled).unwrap(),
            r#"{"installed":false}"#
        );
    }

    /// The crates-local copy of the canonical feed golden
    /// (`internal/ext/rc/testdata/feedMessage.golden.json`), byte-locked to it by
    /// the same Go parity test.
    const FEED_GOLDEN: &str = include_str!("../../fixtures/feedMessage.golden.json");

    /// Pin the feed page shape, including the contract-v2 `approval_request`
    /// rows: a pending request and its resolution carrying the SAME id (the
    /// id-keyed, last-write-wins folding rule — a resolution is a second appended
    /// row, never an edit of the first).
    #[test]
    fn feed_golden_decodes_approval_rows() {
        let page: RcMessagesPage = serde_json::from_str(FEED_GOLDEN).unwrap();
        assert!(!page.truncated);
        assert_eq!(page.messages.len(), 4);
        // The ordinary rows are unaffected by the new field.
        assert_eq!(page.messages[0].msg_type, "text");
        assert!(page.messages[0].approval.is_none());
        assert_eq!(page.messages[1].msg_type, "tool_use");
        assert_eq!(
            page.messages[1].tool.as_ref().unwrap().name.as_deref(),
            Some("exec")
        );
        // Pending: decisions advertised per request, no decision yet.
        let pending = page.messages[2]
            .approval_request()
            .expect("row 3 is an actionable approval request");
        assert_eq!(pending.id, "call_01HQ8Z3K.tool:2");
        assert!(pending.is_pending());
        assert_eq!(pending.decision, None);
        assert_eq!(pending.decisions, ["allow", "allow_always", "deny"]);
        // The human-readable half rides the ordinary text/tool fields.
        assert_eq!(
            page.messages[2].text.as_deref(),
            Some("Allow running `rm -rf build/`?")
        );
        // Resolved: same id, status flipped, the decision that resolved it.
        let resolved = page.messages[3].approval_request().unwrap();
        assert_eq!(resolved.id, pending.id);
        assert!(!resolved.is_pending());
        assert_eq!(resolved.status, "resolved");
        assert_eq!(resolved.decision.as_deref(), Some("allow"));
        assert!(resolved.decisions.is_empty()); // absent once resolved
    }

    #[test]
    fn feed_approval_decode_is_tolerant() {
        // Wrong-typed / malformed approval fields degrade that field — a feed
        // page never fails to decode over one bad row.
        let page = RcMessagesPage::from_value(
            &serde_json::from_str(
                r#"{"messages":[
                    {"seq":1,"role":"tool","type":"approval_request","approval":42},
                    {"seq":2,"role":"tool","type":"approval_request",
                     "approval":{"id":7,"status":"  pending  ","decisions":["allow",5,"deny"]}},
                    {"seq":3,"role":"tool","type":"text",
                     "approval":{"id":"a1","status":"pending"}}
                ]}"#,
            )
            .unwrap(),
        );
        // A non-object approval is no approval block at all (the `tool` rule).
        assert!(page.messages[0].approval.is_none());
        assert!(page.messages[0].approval_request().is_none());
        // Wrong-typed id → "", tokens trimmed, non-string decisions dropped.
        let a = page.messages[1].approval.as_ref().unwrap();
        assert_eq!(a.id, "");
        assert_eq!(a.status, "pending");
        assert_eq!(a.decisions, ["allow", "deny"]);
        // A block on a non-approval row decodes but is not an approval request.
        assert!(page.messages[2].approval.is_some());
        assert!(page.messages[2].approval_request().is_none());
    }

    #[test]
    fn pending_approvals_decode_tolerantly_on_the_session_dto() {
        // The hub-layer snapshot (nothing produces it yet): absent → None, and a
        // present list decodes through the same tolerant approval reader.
        let dtos = decode_list(
            r#"{"rc_sessions":[
                {"slug":"a","tmux_session":"rc-a","kind":"codex","state":"ready","managed":true,
                 "lane":"tui","pending_approvals":[
                    {"id":"call_1","status":"pending","decisions":["allow","deny"]},
                    "nonsense"]}
            ]}"#,
        )
        .unwrap();
        let pending = dtos[0].pending_approvals.as_ref().unwrap();
        assert_eq!(pending.len(), 2);
        assert_eq!(pending[0].id, "call_1");
        assert!(pending[0].is_pending());
        assert_eq!(pending[1], RcFeedApproval::default()); // non-object → default
    }

    #[test]
    fn synthetic_urls_and_tmux_name() {
        assert_eq!(tmux_name("abc"), "rc-abc");
        assert_eq!(
            synthetic_url(&RcKind::ClaudeRc, "abc").as_deref(),
            Some("https://claude.ai/code/session_abc")
        );
        assert_eq!(
            synthetic_url(&RcKind::ClaudeBroker, "abc").as_deref(),
            Some("https://claude.ai/code?environment=env_abc")
        );
        assert_eq!(synthetic_url(&RcKind::Shell, "abc"), None);
    }

    // ---- new kinds + unknown-kind policy ----

    #[test]
    fn new_kinds_round_trip_and_accept_input() {
        for (wire, kind) in [
            ("codex", RcKind::Codex),
            ("opencode", RcKind::Opencode),
            ("cursor", RcKind::Cursor),
        ] {
            assert_eq!(RcKind::from_wire(wire), kind);
            assert_eq!(kind.as_str(), wire);
            assert!(kind.is_known());
            assert!(kind.accepts_typed_input()); // bare-TUI kinds take a kickoff prompt
            assert_eq!(serde_json::to_value(&kind).unwrap(), wire);
            // No claude affordance: none of the new kinds get a synthetic claude URL.
            assert_eq!(synthetic_url(&kind, "abc"), None);
        }
    }

    #[test]
    fn unknown_kind_is_preserved_and_neutral() {
        let k = RcKind::from_wire("borg");
        assert_eq!(k, RcKind::Other("borg".into()));
        assert!(!k.is_known());
        assert!(!k.accepts_typed_input()); // no affordances for an unknown kind
        assert_eq!(k.tool(), None);
        // Round-trips as its raw string, and gets no synthetic claude URL.
        assert_eq!(serde_json::to_value(&k).unwrap(), "borg");
        assert_eq!(synthetic_url(&k, "abc"), None);
        // A pane classifies neutrally — no claude URL even if the pane contains one.
        let c = classify_pane(&k, "https://claude.ai/code/session_X");
        assert_eq!(c.state, RcState::Ready);
        assert!(c.url.is_none());
    }

    #[test]
    fn decode_list_preserves_unknown_and_new_kinds() {
        // A session created by a newer/other tool must survive decode (not be
        // dropped or coerced to claude-broker) — the unknown-kind policy.
        let stdout = r#"{"rc_sessions":[
            {"slug":"a","tmux_session":"rc-a","kind":"codex","state":"ready","managed":true},
            {"slug":"b","tmux_session":"rc-b","kind":"borg","state":"starting","managed":true}
        ]}"#;
        let dtos = decode_list(stdout).unwrap();
        assert_eq!(dtos.len(), 2);
        assert_eq!(dtos[0].kind, RcKind::Codex);
        assert_eq!(dtos[1].kind, RcKind::Other("borg".into()));
    }

    // ---- kind_features watch/input hints ----

    #[test]
    fn kind_features_watch_input_absent_default() {
        // A pre-hub payload carries neither hint → additive defaults (false/"").
        let f: RcKindFeatures =
            serde_json::from_str(r#"{"post_input":true,"approvals":"tui"}"#).unwrap();
        assert!(f.post_input);
        assert!(!f.watch);
        assert_eq!(f.input, "");
        assert!(!f.input_gated());
    }

    #[test]
    fn kind_features_gated_input() {
        let f: RcKindFeatures = serde_json::from_str(
            r#"{"post_input":true,"approvals":"tui","watch":true,"input":"gated"}"#,
        )
        .unwrap();
        assert!(f.watch);
        assert!(f.input_gated());
        // A non-"gated" mode string is preserved verbatim but not gated.
        let f: RcKindFeatures =
            serde_json::from_str(r#"{"post_input":false,"approvals":"","input":"open"}"#).unwrap();
        assert_eq!(f.input, "open");
        assert!(!f.input_gated());
    }

    // ---- contract v2: lane + feed/interrupt/attach, and the v3 fallbacks ----

    /// A whole v3-shaped payload — no `lane`, no `feed`/`interrupt`/`attach`,
    /// `rc_version: 3`, no `contract-v2` token — must decode with the pinned
    /// client defaults rather than degrading or failing. This is the mixed-fleet
    /// case: a new client against an old baked-in guest binary.
    #[test]
    fn v3_payload_decodes_with_contract_v2_defaults() {
        let resp = decode_list_response(
            r#"{"rc_sessions":[
                {"slug":"cdx1","tmux_session":"rc-cdx1","kind":"codex","state":"ready",
                 "managed":true,"activity":"needs_input"},
                {"slug":"brk1","tmux_session":"rc-brk1","kind":"claude-broker",
                 "state":"starting","managed":false}
              ],
              "capabilities":{"rc_version":3,
                "kinds":["codex","claude-rc","shell"],
                "agents":{"codex":{"installed":true}},
                "features":["generic-perm","serve","activity","messages"],
                "kind_features":{
                  "codex":{"post_input":true,"approvals":"tui","watch":true,"input":"gated"},
                  "claude-rc":{"post_input":true,"approvals":"tui"}}}}"#,
        )
        .unwrap();
        // Absent lane → "tui" through the accessor, on the DTO and the enriched
        // session alike; the raw field stays None (absent ≠ asserted).
        for dto in &resp.rc_sessions {
            assert_eq!(dto.lane, None);
            assert_eq!(dto.lane_or_tui(), LANE_TUI);
        }
        let s = RcSession::from_dto(resp.rc_sessions[0].clone(), "srv", "web");
        assert_eq!(s.lane, None);
        assert_eq!(s.lane_or_tui(), LANE_TUI);

        let caps = resp.capabilities.unwrap();
        assert_eq!(caps.rc_version, 3);
        assert!(!caps.has_feature("contract-v2")); // the routes may 404 on this guest
        let codex = &caps.kind_features["codex"];
        // Absent feed → the deprecated `watch` bit answers "is there a message
        // feed?"; absent attach → tmux; absent interrupt → false.
        assert_eq!(codex.feed, "");
        assert!(codex.feed_messages());
        assert_eq!(codex.attach_kind(), ATTACH_TMUX);
        assert!(!codex.interrupt);
        // A v3 kind with neither hint has no message feed at all.
        let claude = &caps.kind_features["claude-rc"];
        assert!(!claude.feed_messages());
        assert_eq!(claude.attach_kind(), ATTACH_TMUX);
    }

    #[test]
    fn feed_hint_supersedes_watch_and_attach_is_carried_verbatim() {
        // With `feed` present it WINS: a v2 producer that (wrongly) let the
        // deprecated lockstep drift can't resurrect a message feed the kind
        // does not have — and vice versa.
        let f: RcKindFeatures = serde_json::from_str(
            r#"{"post_input":true,"approvals":"tui","watch":true,"feed":"activity",
                "interrupt":false,"attach":"tmux"}"#,
        )
        .unwrap();
        assert!(!f.feed_messages());
        let f: RcKindFeatures = serde_json::from_str(
            r#"{"post_input":true,"approvals":"remote","watch":false,"feed":"messages",
                "interrupt":true,"attach":"native-remote"}"#,
        )
        .unwrap();
        assert!(f.feed_messages());
        assert!(f.interrupt);
        // Future values ride through verbatim (no enum, no coercion).
        assert_eq!(f.attach_kind(), "native-remote");
        assert_eq!(f.approvals, "remote");
        // "none" is a real feed value, distinct from absent.
        let f: RcKindFeatures =
            serde_json::from_str(r#"{"post_input":false,"approvals":"tui","feed":"none"}"#)
                .unwrap();
        assert!(!f.feed_messages());
    }

    #[test]
    fn needs_approval_survives_the_dto_to_session_path() {
        // Contract v2 promotes needs_approval from "reserved" to a decoded value
        // (the wire round-trip is pinned in `rc_activity_wire_round_trip`); here,
        // that it reaches an enriched session as itself rather than as Unknown.
        let dtos = decode_list(
            r#"{"rc_sessions":[{"slug":"a","tmux_session":"rc-a","kind":"codex",
                "state":"ready","managed":true,"lane":"tui","activity":"needs_approval"}]}"#,
        )
        .unwrap();
        assert_eq!(dtos[0].activity, Some(RcActivity::NeedsApproval));
        let s = RcSession::from_dto(dtos[0].clone(), "srv", "web");
        assert_eq!(s.activity, Some(RcActivity::NeedsApproval));
    }

    #[test]
    fn lane_is_carried_verbatim_including_future_values() {
        // A structured-lane session from a future guest renders neutrally, not
        // dropped and not coerced (same posture as the unknown-kind policy).
        let dtos = decode_list(
            r#"{"rc_sessions":[
                {"slug":"a","tmux_session":"rc-a","kind":"codex","state":"ready",
                 "managed":true,"lane":"structured"},
                {"slug":"b","tmux_session":"rc-b","kind":"codex","state":"ready",
                 "managed":true,"lane":""}]}"#,
        )
        .unwrap();
        assert_eq!(dtos[0].lane.as_deref(), Some("structured"));
        assert_eq!(dtos[0].lane_or_tui(), "structured");
        // An explicit empty string is out-of-contract; it reads as tui, so no
        // client ever renders a blank lane.
        assert_eq!(dtos[1].lane_or_tui(), LANE_TUI);
    }

    // ---- activity dimension ----

    #[test]
    fn rc_activity_wire_round_trip() {
        for (wire, activity) in [
            ("working", RcActivity::Working),
            ("needs_input", RcActivity::NeedsInput),
            ("needs_approval", RcActivity::NeedsApproval),
            ("idle", RcActivity::Idle),
            ("unknown", RcActivity::Unknown),
        ] {
            assert_eq!(RcActivity::from_wire(wire), activity);
            assert_eq!(activity.as_str(), wire);
            assert_eq!(serde_json::to_value(activity).unwrap(), wire);
            assert_eq!(
                serde_json::from_value::<RcActivity>(wire.into()).unwrap(),
                activity
            );
        }
        // Any unrecognized token — a future value or garbage — maps to Unknown
        // (Dart parity, rc_models.dart:125-146; deliberately NOT RcKind's
        // preserve-raw policy), and Unknown round-trips as the real "unknown"
        // wire value. (needs_approval left this set in contract v2 — it is a
        // decoded variant now, so it rides the round-trip table above.)
        assert_eq!(RcActivity::from_wire("borg"), RcActivity::Unknown);
        assert_eq!(
            serde_json::from_value::<RcActivity>("borg".into()).unwrap(),
            RcActivity::Unknown
        );
        assert_eq!(
            serde_json::to_value(RcActivity::Unknown).unwrap(),
            "unknown"
        );
    }

    #[test]
    fn rc_state_from_wire_is_tolerant() {
        assert_eq!(RcState::from_wire("ready"), RcState::Ready);
        assert_eq!(RcState::from_wire("needs-auth"), RcState::NeedsAuth);
        assert_eq!(RcState::from_wire("dead"), RcState::Dead);
        // Unknown/missing states read as transient, never as gone.
        assert_eq!(RcState::from_wire("starting"), RcState::Starting);
        assert_eq!(RcState::from_wire("some-future-state"), RcState::Starting);
        assert_eq!(RcState::from_wire(""), RcState::Starting);
    }

    #[test]
    fn rc_state_permits_activity_blocks_gating_states() {
        // Blocking states (lifecycle trumps activity) — rc_models.dart:154-157.
        assert!(!RcState::NeedsTrust.permits_activity());
        assert!(!RcState::NeedsAuth.permits_activity());
        assert!(!RcState::Dead.permits_activity());
        // Everything else permits the live activity dimension.
        assert!(RcState::Starting.permits_activity());
        assert!(RcState::Ready.permits_activity());
        assert!(RcState::Reconnecting.permits_activity());
    }

    #[test]
    fn dto_carries_activity_fields_and_session_flows_them_through() {
        let dto = decode_session(
            r#"{"slug":"a","tmux_session":"rc-a","kind":"codex","state":"ready",
                "managed":true,"activity":"working",
                "activity_at":"2026-06-19T18:54:12Z","last_message":"hi"}"#,
        )
        .unwrap();
        assert_eq!(dto.activity, Some(RcActivity::Working));
        let s = RcSession::from_dto(dto, "srv", "web");
        assert_eq!(s.activity, Some(RcActivity::Working));
        assert_eq!(s.activity_at.as_deref(), Some("2026-06-19T18:54:12Z"));
        assert_eq!(s.last_message.as_deref(), Some("hi"));
        // Absent activity → None, and None keys stay off the serialized wire.
        let plain = decode_session(
            r#"{"slug":"b","tmux_session":"rc-b","kind":"shell","state":"ready","managed":true}"#,
        )
        .unwrap();
        assert_eq!(plain.activity, None);
        let j = serde_json::to_value(RcSession::from_dto(plain, "srv", "web")).unwrap();
        assert!(j.get("activity").is_none());
        assert!(j.get("activity_at").is_none());
        assert!(j.get("last_message").is_none());
    }

    // Finding 3: the guest-controlled last_message on the shed-ext-rc stdout
    // path must be sanitized exactly like the overview / feed paths — a bidi
    // override (U+202E) that could visually reverse the preview is stripped.
    #[test]
    fn from_dto_strips_format_chars_from_last_message() {
        // U+202E (right-to-left override) embedded via a Rust escape so the
        // source stays reviewable — the guest ships it in the rc stdout JSON.
        let json = format!(
            r#"{{"slug":"a","tmux_session":"rc-a","kind":"codex","state":"ready",
                "managed":true,"last_message":"run{}evil"}}"#,
            '\u{202E}'
        );
        let dto = decode_session(&json).unwrap();
        // The DTO carries the raw guest text verbatim…
        assert_eq!(dto.last_message.as_deref(), Some("run\u{202E}evil"));
        // …and from_dto sanitizes it (Cf stripped) before it reaches RcSession.
        let s = RcSession::from_dto(dto, "srv", "web");
        assert_eq!(s.last_message.as_deref(), Some("runevil"));

        // A value that is ONLY format characters degrades to None.
        let json = format!(
            r#"{{"slug":"b","tmux_session":"rc-b","kind":"shell","state":"ready",
                "managed":true,"last_message":"{}{}"}}"#,
            '\u{202E}', '\u{200B}'
        );
        let only_cf = decode_session(&json).unwrap();
        assert_eq!(
            RcSession::from_dto(only_cf, "srv", "web").last_message,
            None
        );
    }

    // ---- capabilities ----

    #[test]
    fn decode_list_response_carries_capabilities() {
        let stdout = r#"{
          "rc_sessions": [],
          "capabilities": {
            "rc_version": 3,
            "kinds": ["claude-rc","codex","opencode","cursor","shell"],
            "agents": { "claude": {"installed": true, "version": "2.1.206"},
                        "codex":  {"installed": true, "version": "0.143.0"},
                        "cursor": {"installed": false} },
            "features": ["generic-perm","plan-stdin","prompt-b64"],
            "kind_features": { "codex": {"post_input": true, "approvals": "tui"} }
          }
        }"#;
        let resp = decode_list_response(stdout).unwrap();
        let caps = resp.capabilities.expect("capabilities present");
        assert_eq!(caps.rc_version, 3);
        assert!(caps.has_feature("generic-perm"));
        assert!(caps.kinds.contains(&RcKind::Codex));
        assert_eq!(caps.kind_features["codex"].approvals, "tui");
        // Gating: claude + codex installed, cursor not, opencode absent from agents,
        // shell always offered when advertised.
        assert!(caps.offers(&RcKind::ClaudeRc));
        assert!(caps.offers(&RcKind::Codex));
        assert!(!caps.offers(&RcKind::Cursor)); // advertised but not installed
        assert!(!caps.offers(&RcKind::Opencode)); // advertised but no agents entry
        assert!(caps.offers(&RcKind::Shell));
        assert!(!caps.offers(&RcKind::ClaudeBroker)); // not advertised (URL-driven)
        assert_eq!(
            caps.creatable_kinds(),
            vec![RcKind::ClaudeRc, RcKind::Codex, RcKind::Shell]
        );
    }

    #[test]
    fn old_binary_envelope_has_no_capabilities() {
        // An old baked-in binary's bare envelope decodes with capabilities == None
        // (tolerant of absence) — the capability leg degrades, it does not error.
        let resp = decode_list_response(
            r#"{"rc_sessions":[{"slug":"a","tmux_session":"rc-a","kind":"shell","state":"ready","managed":true}]}"#,
        )
        .unwrap();
        assert!(resp.capabilities.is_none());
        assert_eq!(resp.rc_sessions.len(), 1);
    }

    #[test]
    fn present_but_empty_capabilities_offer_nothing() {
        // Present-but-EMPTY capabilities are NOT the same as absent: a shed that
        // advertises kinds with no installed agents yields an empty creatable set
        // (clients show "unavailable"); only an ABSENT block may fall back to
        // claude+shell.
        let resp = decode_list_response(
            r#"{"rc_sessions":[],
                "capabilities":{"rc_version":3,
                  "kinds":["claude-rc","codex"],
                  "agents":{"claude":{"installed":false},"codex":{"installed":false}},
                  "features":[],"kind_features":{}}}"#,
        )
        .unwrap();
        let caps = resp.capabilities.unwrap();
        assert!(caps.creatable_kinds().is_empty());
        assert!(!caps.offers(&RcKind::ClaudeRc));
        assert!(!caps.offers(&RcKind::Shell)); // not even advertised
    }
}
