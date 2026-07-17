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

use crate::models::{clean_display, opt_trimmed};
use crate::terminal::{shell_quote, ssh_host_key_opts};

/// Fallback workdir for a legacy/unmanaged session whose DTO omits one (the
/// binary resolves `$SHED_WORKSPACE` for managed sessions).
pub const DEFAULT_WORKDIR: &str = "/workspace";
/// Stable tool id for `SHED_RC_CREATED_BY` (`<tool>/<version>`; no `/`).
pub const TOOL_NAME: &str = "shed-desktop";
/// tmux session name prefix.
pub const TMUX_PREFIX: &str = "rc-";

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
/// output), `needs_input` (idle at a prompt anchor), `idle` (quiescent), and
/// `unknown` (live but indeterminate).
///
/// Deliberately NO `Other(String)` case (unlike [`RcKind`]'s unknown-kind
/// policy): an UNRECOGNIZED token — a future value, or the reserved
/// `needs_approval` the hub does not derive yet — maps to
/// [`RcActivity::Unknown`] (Dart parity, `rc_models.dart:125-146`), so it
/// renders neutrally (no badge) and consumers key off a single variant.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash)]
pub enum RcActivity {
    Working,
    NeedsInput,
    Idle,
    Unknown,
}

impl RcActivity {
    pub fn as_str(&self) -> &'static str {
        match self {
            RcActivity::Working => "working",
            RcActivity::NeedsInput => "needs_input",
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
#[derive(Debug, Clone, PartialEq, Eq, Deserialize)]
pub struct RcSessionDto {
    pub slug: String,
    pub tmux_session: String,
    pub kind: RcKind,
    pub state: RcState,
    // Strict like Swift's `RcSessionDTO` (binary output, golden-pinned): `managed`
    // is required — a DTO omitting it is a shed-ext-rc contract violation, not a
    // silent "unmanaged". (The enriched `RcSession` model below stays defensive.)
    pub managed: bool,
    pub display_name: Option<String>,
    pub workdir: Option<String>,
    pub url: Option<String>,
    pub id: Option<String>,
    pub created_by: Option<String>,
    pub created_at: Option<String>,
    pub target_label: Option<String>,
    /// Live-activity dimension (additive inside the `rc` block; derived by the
    /// rc hub). Absent when no hub is running, the kind is unsupported, or the
    /// server suppressed it (a blocking lifecycle state trumps activity).
    /// Mirrors mobile's `RcSession.activity` (`rc_models.dart:222-234`).
    pub activity: Option<RcActivity>,
    /// RFC3339 timestamp the activity was last derived/changed; absent with
    /// `activity`.
    pub activity_at: Option<String>,
    /// A short, hub-sanitized (ANSI/control-stripped, ≤200 runes) preview of
    /// the session's most recent message. Absent when the hub has none.
    pub last_message: Option<String>,
}

/// The `shed-ext-rc list` response shape. Strict on `rc_sessions` like Swift's
/// `RcSessionListDTO` (the binary always emits the array, never null/absent, so a
/// missing/null value is a contract violation the fan-out drops), but tolerant on
/// `capabilities`: an OLD baked-in binary's bare `{"rc_sessions":[…]}` envelope has
/// no block, so it decodes to `None` (the capability-discovery leg degrades, it
/// does not error).
#[derive(Debug, Clone, Deserialize)]
pub struct RcSessionListDto {
    pub rc_sessions: Vec<RcSessionDto>,
    #[serde(default)]
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
/// where approvals surface (v1 agents are TUI-only → `"tui"`).
///
/// `watch` and `input` are additive hub hints (codex-only in this phase; absent
/// → `false` / `""`): `watch` reports whether the hub produces a live message
/// feed for the kind (`GET /messages` + `message.appended`), and `input` is the
/// feed-input posting **mode string** — `"gated"` means `POST /input` is
/// accepted only while the session is waiting, `""` means no feed input. Note
/// the distinction from the adjacent `post_input`: `post_input` is the
/// typed-input *capability* bool (a typed line reaches the pane over the
/// TUI-only path), while `input` is the *gating mode* of the separate feed-input
/// channel — a kind can have `post_input: true` with no feed input at all.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct RcKindFeatures {
    pub post_input: bool,
    pub approvals: String,
    #[serde(default)]
    pub watch: bool,
    #[serde(default)]
    pub input: String,
}

impl RcKindFeatures {
    /// Whether feed input is gated (`input == "gated"`) — a watch view's input
    /// bar is only ever enabled for a gated kind waiting for input. Mirrors
    /// mobile's `KindFeatures.inputGated` (`rc_capabilities.dart:136`).
    pub fn input_gated(&self) -> bool {
        self.input == "gated"
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
    #[serde(default)]
    pub managed: bool,
}

impl RcSession {
    /// The table/wire identity — `host/shed/slug`.
    pub fn id(&self) -> String {
        composite_id(&self.host, &self.shed, &self.slug)
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
            url: dto.url,
            rc_id: dto.id,
            created_by: dto.created_by,
            created_at: dto.created_at,
            target_label: dto.target_label,
            activity: dto.activity,
            activity_at: dto.activity_at,
            last_message: dto.last_message,
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

/// One normalized conversation message in the feed. `role` ∈ {user, assistant,
/// tool, system}; `msg_type` (wire key `type`) ∈ {text, tool_use, tool_result,
/// reasoning, status}. `seq` is monotonic per hub run (restarts from 1 on hub
/// restart — a client that sees a seq lower than one it holds does a full
/// refetch). Mirrors mobile's `RcFeedMessage` (`rc_feed.dart:29-58`); every
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
        };
        let s = RcSession::from_dto(dto, "srv", "web");
        assert_eq!(s.host, "srv");
        assert_eq!(s.shed, "web");
        assert_eq!(s.display_name, "web/abc"); // fallback
        assert_eq!(s.workdir.as_deref(), Some(DEFAULT_WORKDIR)); // fallback
        assert_eq!(s.rc_id.as_deref(), Some("id-1")); // id → rc_id
        assert_eq!(s.id(), "srv/web/abc");
    }

    #[test]
    fn rc_session_serializes_expected_keys() {
        let s = RcSession::from_dto(
            RcSessionDto {
                slug: "abc".into(),
                tmux_session: "rc-abc".into(),
                kind: RcKind::Shell,
                state: RcState::Ready,
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
            },
            "srv",
            "web",
        );
        let j = serde_json::to_value(&s).unwrap();
        assert_eq!(j["tmux_session"], "rc-abc");
        assert_eq!(j["kind"], "shell");
        assert_eq!(j["managed"], false);
        // None optionals are omitted (Swift's encodeIfPresent parity), and `id`
        // (the computed key) is never on the wire.
        assert!(j.get("url").is_none());
        assert!(j.get("rc_id").is_none());
        assert!(j.get("id").is_none());
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

    /// Decode the canonical golden fixture (byte-identical to shed-remote-agent's
    /// `rcSessionDto.golden.json` + the Swift `RCTests` guard): a full managed
    /// session + a minimal legacy one (only required fields). Pins cross-repo
    /// wire parity for the list DTO.
    #[test]
    fn decode_list_matches_golden_fixture() {
        let golden = r#"{
          "rc_sessions": [
            {
              "slug": "abc234", "tmux_session": "rc-abc234", "kind": "claude-rc",
              "state": "ready", "managed": true, "display_name": "charliek/abc234",
              "workdir": "/home/shed",
              "url": "https://claude.ai/code/session_01RCkTDrdZ2Rr12sD5dfMjgr",
              "id": "9f1c0e7a-1111-4222-8333-444455556666",
              "created_by": "shed-remote-agent/0.1.0", "created_at": "2026-06-19T18:53:00Z",
              "target_label": "shed:t1@localmac-dev"
            },
            {
              "slug": "brk900", "tmux_session": "rc-brk900",
              "kind": "claude-broker", "state": "starting", "managed": false
            }
          ]
        }"#;
        let dtos = decode_list(golden).unwrap();
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
        // Minimal legacy session: absent optionals default, fallbacks applied.
        assert!(!dtos[1].managed);
        let minimal = RcSession::from_dto(dtos[1].clone(), "h", "demo");
        assert_eq!(minimal.display_name, "demo/brk900"); // <shed>/<slug> fallback
        assert_eq!(minimal.workdir.as_deref(), Some(DEFAULT_WORKDIR)); // fallback
        assert!(minimal.rc_id.is_none());
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

    // ---- activity dimension ----

    #[test]
    fn rc_activity_wire_round_trip() {
        for (wire, activity) in [
            ("working", RcActivity::Working),
            ("needs_input", RcActivity::NeedsInput),
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
        // Any unrecognized token — the reserved needs_approval, a future value,
        // or garbage — maps to Unknown (Dart parity, rc_models.dart:125-146;
        // deliberately NOT RcKind's preserve-raw policy), and Unknown
        // round-trips as the real "unknown" wire value.
        assert_eq!(RcActivity::from_wire("needs_approval"), RcActivity::Unknown);
        assert_eq!(RcActivity::from_wire("borg"), RcActivity::Unknown);
        assert_eq!(
            serde_json::from_value::<RcActivity>("needs_approval".into()).unwrap(),
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
