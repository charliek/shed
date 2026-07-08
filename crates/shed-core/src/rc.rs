//! Pure Remote-Control (RC Session Convention v2) logic — the pane classifier,
//! prompt normalization, `shed-ext-rc` argv builders, the non-interactive SSH
//! argv, the neutral wire DTOs, and the enriched `RcSession` model. Ported from
//! shed-desktop's `ShedKit/RC/RemoteControl.swift` + `Models.swift` `RcSession`.
//!
//! No I/O and no feature flag: the SSH+tmux choreography (bootstrap, trust
//! pre-seed, poll-to-ready, prompt delivery) lives in the `shed-ext-rc` guest
//! binary; a client invokes it over SSH — process spawning + the session store
//! live in `shed-app::rc` (feature `rc`) — and decodes this neutral JSON DTO.

use std::sync::LazyLock;

use regex::Regex;
use serde::{Deserialize, Serialize};

use crate::terminal::{shell_quote, ssh_host_key_opts};

/// Fallback workdir for a legacy/unmanaged session whose DTO omits one (the
/// binary resolves `$SHED_WORKSPACE` for managed sessions).
pub const DEFAULT_WORKDIR: &str = "/workspace";
/// Stable tool id for `SHED_RC_CREATED_BY` (`<tool>/<version>`; no `/`).
pub const TOOL_NAME: &str = "shed-desktop";
/// tmux session name prefix.
pub const TMUX_PREFIX: &str = "rc-";

/// RC session kind (Convention v2). `<tool>-<mode>` so the model can grow to
/// other agents later; `shell` is tool-agnostic.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "kebab-case")]
pub enum RcKind {
    ClaudeRc,
    ClaudeBroker,
    Shell,
}

impl RcKind {
    /// Whether this kind accepts a typed kickoff line — an initial prompt for
    /// `claude-rc`, an initial command for `shell`. Every kind except
    /// `claude-broker`, whose input is a remote URL, not the pane.
    pub fn accepts_typed_input(self) -> bool {
        !matches!(self, RcKind::ClaudeBroker)
    }

    pub fn as_str(self) -> &'static str {
        match self {
            RcKind::ClaudeRc => "claude-rc",
            RcKind::ClaudeBroker => "claude-broker",
            RcKind::Shell => "shell",
        }
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
}

/// The `shed-ext-rc list` response shape. Strict like Swift's `RcSessionListDTO`:
/// the binary always emits a `rc_sessions` array (never null/absent), so a
/// missing/null field is a contract violation the list fan-out drops.
#[derive(Debug, Clone, Deserialize)]
pub struct RcSessionListDto {
    pub rc_sessions: Vec<RcSessionDto>,
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
    pub workdir: String,
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
            workdir: dto.workdir.unwrap_or_else(|| DEFAULT_WORKDIR.to_string()),
            kind: dto.kind,
            state: dto.state,
            url: dto.url,
            rc_id: dto.id,
            created_by: dto.created_by,
            created_at: dto.created_at,
            target_label: dto.target_label,
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
pub fn synthetic_url(kind: RcKind, slug: &str) -> Option<String> {
    match kind {
        RcKind::ClaudeBroker => Some(format!("https://claude.ai/code?environment=env_{slug}")),
        RcKind::ClaudeRc => Some(format!("https://claude.ai/code/session_{slug}")),
        RcKind::Shell => None,
    }
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
pub fn normalize_rc_prompt(raw: Option<&str>, kind: RcKind) -> Result<Option<String>, RcError> {
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

// ---- shed-ext-rc argv ----

/// argv for `shed-ext-rc create --wait` (the binary resolves the workdir,
/// pre-seeds trust, polls to ready, accepts trust, and delivers a stdin prompt).
/// `bin` is resolved by the caller (`shed-app` reads `SHED_EXT_RC_BIN`) so this
/// stays pure. `slug` is caller-supplied (generated in `shed-app::rc`, not here).
#[allow(clippy::too_many_arguments)]
pub fn create_argv(
    bin: &str,
    kind: RcKind,
    name: &str,
    slug: &str,
    workdir: Option<&str>,
    created_by: &str,
    target: &str,
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
    if has_prompt {
        a.push("--prompt-stdin".to_string());
    }
    a
}

/// Build the `create` argv and its stdin together, so the `--prompt-stdin` flag
/// and the stdin payload can never disagree. `prompt` must already be normalized
/// (see [`normalize_rc_prompt`]); it is dropped for a kind that doesn't accept
/// typed input.
#[allow(clippy::too_many_arguments)]
pub fn create_invocation(
    bin: &str,
    kind: RcKind,
    name: &str,
    slug: &str,
    workdir: Option<&str>,
    created_by: &str,
    target: &str,
    prompt: Option<&str>,
) -> (Vec<String>, Option<String>) {
    let effective = if kind.accepts_typed_input() { prompt } else { None };
    let argv = create_argv(bin, kind, name, slug, workdir, created_by, target, effective.is_some());
    (argv, effective.map(str::to_string))
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
    serde_json::from_str::<RcSessionListDto>(stdout)
        .map(|l| l.rc_sessions)
        .map_err(|_| RcError::Failed("shed-ext-rc returned an invalid session list".to_string()))
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
static RE_RECONNECTING: LazyLock<Regex> = LazyLock::new(|| Regex::new(r"\bReconnecting\b").unwrap());
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
pub fn classify_pane(kind: RcKind, pane: &str) -> RcClassification {
    let lower = pane.to_lowercase();
    // Trust + auth heuristics apply to both kinds that run claude.
    if kind != RcKind::Shell {
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
            let url = extract_url(RcKind::ClaudeBroker, pane);
            // Reconnecting takes precedence over a (possibly stale) url — Swift parity.
            if RE_RECONNECTING.is_match(pane) {
                return RcClassification {
                    state: RcState::Reconnecting,
                    url,
                };
            }
            classify_by_url(url)
        }
        RcKind::ClaudeRc => classify_by_url(extract_url(RcKind::ClaudeRc, pane)),
        RcKind::Shell => RcClassification {
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
pub fn extract_url(kind: RcKind, pane: &str) -> Option<String> {
    let re = match kind {
        RcKind::ClaudeBroker => &*RE_URL_BROKER,
        RcKind::ClaudeRc => &*RE_URL_SESSION,
        RcKind::Shell => return None,
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
        let c = classify_pane(RcKind::ClaudeBroker, pane);
        assert_eq!(c.state, RcState::Ready);
        assert_eq!(
            c.url.as_deref(),
            Some("https://claude.ai/code?environment=env_01ABC")
        );
    }

    #[test]
    fn classify_repl_needs_trust() {
        let c = classify_pane(
            RcKind::ClaudeRc,
            "Quick safety check: Is this a project you trust?",
        );
        assert_eq!(c.state, RcState::NeedsTrust);
    }

    #[test]
    fn classify_trust_folder_button_needs_trust() {
        let c = classify_pane(RcKind::ClaudeRc, "  Yes,  I trust this folder  ");
        assert_eq!(c.state, RcState::NeedsTrust);
    }

    #[test]
    fn classify_needs_auth() {
        for pane in ["not logged in", "run claude auth login", "requires a claude.ai subscription"] {
            assert_eq!(classify_pane(RcKind::ClaudeRc, pane).state, RcState::NeedsAuth);
        }
    }

    #[test]
    fn classify_broker_reconnecting_no_url() {
        let c = classify_pane(RcKind::ClaudeBroker, "·|· Reconnecting · retrying in 2.5s");
        assert_eq!(c.state, RcState::Reconnecting);
        assert!(c.url.is_none());
    }

    #[test]
    fn classify_rc_ready_with_session_url() {
        let pane = "Remote Control active\nhttps://claude.ai/code/session_XYZ789";
        let c = classify_pane(RcKind::ClaudeRc, pane);
        assert_eq!(c.state, RcState::Ready);
        assert_eq!(c.url.as_deref(), Some("https://claude.ai/code/session_XYZ789"));
    }

    #[test]
    fn classify_rc_connecting_is_starting() {
        let c = classify_pane(RcKind::ClaudeRc, "Remote Control connecting…");
        assert_eq!(c.state, RcState::Starting);
        assert!(c.url.is_none());
    }

    #[test]
    fn classify_shell_empty_vs_content() {
        assert_eq!(classify_pane(RcKind::Shell, "   \n ").state, RcState::Starting);
        assert_eq!(classify_pane(RcKind::Shell, "$ ls").state, RcState::Ready);
        // A shell never runs the trust/auth heuristics.
        assert_eq!(classify_pane(RcKind::Shell, "not logged in").state, RcState::Ready);
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

    // ---- prompt normalization ----

    #[test]
    fn normalize_prompt_trims_and_accepts() {
        assert_eq!(
            normalize_rc_prompt(Some("  summarize this repo\n"), RcKind::ClaudeRc).unwrap(),
            Some("summarize this repo".to_string())
        );
    }

    #[test]
    fn normalize_prompt_blank_is_none() {
        assert_eq!(normalize_rc_prompt(Some("   \n\t"), RcKind::ClaudeRc).unwrap(), None);
        assert_eq!(normalize_rc_prompt(None, RcKind::Shell).unwrap(), None);
    }

    #[test]
    fn normalize_prompt_rejects_control_char() {
        assert!(matches!(
            normalize_rc_prompt(Some("bad\nvalue"), RcKind::ClaudeRc),
            Err(RcError::BadRequest(_))
        ));
    }

    #[test]
    fn normalize_prompt_rejects_overlong() {
        let big = "a".repeat(2001);
        assert!(matches!(
            normalize_rc_prompt(Some(&big), RcKind::Shell),
            Err(RcError::BadRequest(_))
        ));
        // Exactly 2000 bytes is fine.
        assert!(normalize_rc_prompt(Some(&"a".repeat(2000)), RcKind::Shell).unwrap().is_some());
    }

    #[test]
    fn normalize_prompt_rejects_for_broker() {
        assert!(matches!(
            normalize_rc_prompt(Some("nope"), RcKind::ClaudeBroker),
            Err(RcError::BadRequest(_))
        ));
    }

    // ---- ssh argv (the H1 guard) ----

    #[test]
    fn ssh_argv_is_non_interactive_and_quotes_remote() {
        let remote = vec!["shed-ext-rc".to_string(), "list".to_string()];
        let argv = ssh_argv("web", "10.0.0.5", 2222, "/k/known_hosts", &remote, 10);
        // No `-t` (a PTY would corrupt the JSON DTO decode).
        assert!(!argv.contains(&"-t".to_string()), "RC ssh must not allocate a PTY");
        assert!(argv.windows(2).any(|w| w == ["-o", "BatchMode=yes"]));
        assert!(argv.contains(&"ConnectTimeout=10".to_string()));
        assert!(argv.windows(2).any(|w| w == ["-o", "StrictHostKeyChecking=yes"]));
        // The remote command is a single shell-quoted string after `--`.
        let dd = argv.iter().position(|a| a == "--").unwrap();
        assert_eq!(argv[dd + 1], "shed-ext-rc list");
        assert_eq!(argv.last().unwrap(), "shed-ext-rc list");
        // user@host precedes the `--`.
        assert!(argv.contains(&"web@10.0.0.5".to_string()));
    }

    #[test]
    fn ssh_argv_shell_quotes_a_prompt_arg() {
        let remote = vec!["shed-ext-rc".to_string(), "create".to_string(), "a b".to_string()];
        let argv = ssh_argv("s", "h", 22, "/k", &remote, 10);
        assert_eq!(argv.last().unwrap(), "shed-ext-rc create 'a b'");
    }

    // ---- create/list/kill argv ----

    #[test]
    fn create_argv_shape_with_prompt_and_workdir() {
        let a = create_argv(
            "shed-ext-rc",
            RcKind::ClaudeRc,
            "web/abc",
            "abc",
            Some("/work"),
            "shed-desktop/1.0",
            "shed:web@srv",
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
            "b", RcKind::Shell, "n", "s", Some(""), "c", "t", false,
        );
        assert!(!a.contains(&"--workdir".to_string()));
        assert!(!a.contains(&"--prompt-stdin".to_string()));
    }

    #[test]
    fn create_invocation_drops_prompt_for_broker() {
        let (argv, stdin) = create_invocation(
            "b", RcKind::ClaudeBroker, "n", "s", None, "c", "t", Some("hi"),
        );
        assert_eq!(stdin, None);
        assert!(!argv.contains(&"--prompt-stdin".to_string()));
    }

    #[test]
    fn list_and_kill_argv() {
        assert_eq!(list_argv("b"), ["b", "list"]);
        assert_eq!(kill_argv("b", "abc"), ["b", "kill", "--slug", "abc"]);
    }

    // ---- exit-code mapping ----

    #[test]
    fn error_from_exit_maps_codes() {
        assert_eq!(error_from_exit(3, "taken", ""), RcError::SlugTaken("taken".into()));
        assert_eq!(error_from_exit(4, "gone", ""), RcError::NotFound("gone".into()));
        assert_eq!(error_from_exit(2, "bad", ""), RcError::BadRequest("bad".into()));
        assert_eq!(error_from_exit(127, "", ""), RcError::MissingBinary);
        assert_eq!(
            error_from_exit(1, "bash: shed-ext-rc: command not found", ""),
            RcError::MissingBinary
        );
        assert_eq!(error_from_exit(1, "", ""), RcError::Failed("shed-ext-rc exited 1".into()));
        // stdout is the fallback detail when stderr is empty.
        assert_eq!(error_from_exit(5, "", "boom"), RcError::Failed("boom".into()));
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
        };
        let s = RcSession::from_dto(dto, "srv", "web");
        assert_eq!(s.host, "srv");
        assert_eq!(s.shed, "web");
        assert_eq!(s.display_name, "web/abc"); // fallback
        assert_eq!(s.workdir, DEFAULT_WORKDIR); // fallback
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
        assert!(matches!(decode_session("not json"), Err(RcError::Failed(_))));
        // A missing required field is not a valid DTO.
        assert!(matches!(decode_session(r#"{"slug":"x"}"#), Err(RcError::Failed(_))));
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
        assert_eq!(full.rc_id.as_deref(), Some("9f1c0e7a-1111-4222-8333-444455556666"));
        assert_eq!(full.created_by.as_deref(), Some("shed-remote-agent/0.1.0"));
        // Minimal legacy session: absent optionals default, fallbacks applied.
        assert!(!dtos[1].managed);
        let minimal = RcSession::from_dto(dtos[1].clone(), "h", "demo");
        assert_eq!(minimal.display_name, "demo/brk900"); // <shed>/<slug> fallback
        assert_eq!(minimal.workdir, DEFAULT_WORKDIR); // fallback
        assert!(minimal.rc_id.is_none());
    }

    #[test]
    fn synthetic_urls_and_tmux_name() {
        assert_eq!(tmux_name("abc"), "rc-abc");
        assert_eq!(
            synthetic_url(RcKind::ClaudeRc, "abc").as_deref(),
            Some("https://claude.ai/code/session_abc")
        );
        assert_eq!(
            synthetic_url(RcKind::ClaudeBroker, "abc").as_deref(),
            Some("https://claude.ai/code?environment=env_abc")
        );
        assert_eq!(synthetic_url(RcKind::Shell, "abc"), None);
    }
}
