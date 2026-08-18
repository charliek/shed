//! The RC **agent registry** — a verbatim Rust port of the guest engine's
//! `internal/ext/rc/agents.go` (plus the pure helpers in `rc.go`/`meta.go` the
//! registry is inseparable from).
//!
//! This is the ENGINE half of the RC logic: the per-kind inner-command builders
//! that become the tmux `new-session` command, the complete pane classifiers
//! (trust / auth / ready / dead, with every anchor regex), permission-flag
//! resolution, the `SHED_RC_*` metadata writer/reader, and slug generation.
//! Deliberately NOT here (deferred with the hub, which is not being ported this
//! block): `ApprovalAnchor` and `ComposerUnderModal` from the Go registry —
//! both are hub-only consumers, and cursor's approval anchor is SAFETY-critical
//! there (the sole guard against typing into a modal, `agents.go:347-358`), so
//! porting them without their consumer would invite silent drift. A future hub
//! port must bring them along; this registry is complete for the ONE-SHOT verbs
//! only. (`PromptAnchor` IS ported — as inert registry data — because it rides
//! the same per-kind table the one-shot classifiers live in.)
//! `rc.rs`'s [`crate::rc::classify_pane`] is a DIFFERENT, deliberately narrower
//! thing — a claude-only best-effort CLIENT classifier pinned to Swift parity —
//! and stays untouched. Engine callers (the Rust rc engine in `shed-app`, the
//! `sx` porcelain) use [`classify_pane`] from THIS module, which is authoritative
//! and covers every agent.
//!
//! **Why a port and not a binding:** the Go one-shot engine
//! (`shed-machine-rc` / `shed-ext-rc`) stays alive as the parity oracle while the
//! Rust engine grows underneath it, and a differential harness compares the two
//! wire-for-wire. Every function here therefore mirrors its Go original's
//! structure and ORDER of checks, not merely its outcome — a classifier that gets
//! the right answer by a different precedence would diverge on the next fixture.
//!
//! **Deliberate Go→Rust regex divergences, spelled out** (Go `regexp` is RE2 with
//! ASCII-only Perl classes; Rust `regex` defaults its Perl classes to Unicode):
//!
//! - `\s` is written out as [`GO_SPACE`] (`[\t\n\f\r ]`), Go's exact class, rather
//!   than `\s`, which in Rust would additionally match NBSP/U+2028/… Pane captures
//!   are full of exotic whitespace-adjacent box drawing, so this is not academic.
//! - `\d` is written `[0-9]` for the same reason (a Unicode digit must not satisfy
//!   the canonical-integer / RFC3339 shapes Go rejects).
//! - `\b` is left as-is. Rust's word boundary is Unicode-aware where Go's is
//!   ASCII, so the two disagree only when a non-ASCII LETTER is directly adjacent
//!   to the anchored word (e.g. `éConnected`). No fixture or realistic pane hits
//!   that, and spelling the boundary out by hand would cost far more clarity than
//!   it buys.

use std::collections::HashMap;
use std::sync::LazyLock;

use regex::Regex;
use uuid::Uuid;

use crate::rc::{
    RcClassification, RcKind, RcSessionDto, RcState, CLAUDE_EXTRA_MODES, LANE_TUI, TMUX_PREFIX,
};

// ---------------------------------------------------------------------------
// constants (rc.go)
// ---------------------------------------------------------------------------

/// Go's `\s` class, written out. See the module doc for why this is not `\s`.
const GO_SPACE: &str = r"[\t\n\f\r ]";

/// The schema version stamped into `SHED_RC_V` at create (`rc.go:100`). Stays 2 —
/// every key added since (`SHED_RC_SLUG`, `SHED_RC_OPENCODE_PORT`) is ADDITIVE.
pub const SCHEMA_VERSION: u32 = 2;

/// The lowest `SHED_RC_V` a reader still understands (`rc.go:104`). Deliberately
/// decoupled from [`SCHEMA_VERSION`] so a future additive bump does not
/// force-drop older managed sessions.
pub const MIN_MANAGED_VERSION: u32 = 2;

/// The `SHED_RC_*` env keys — the on-session metadata store (RC Session
/// Convention v2, `rc.go:111-149`).
pub const ENV_V: &str = "SHED_RC_V";
pub const ENV_ID: &str = "SHED_RC_ID";
pub const ENV_DISPLAY_NAME: &str = "SHED_RC_DISPLAY_NAME";
pub const ENV_KIND: &str = "SHED_RC_KIND";
pub const ENV_WORKDIR: &str = "SHED_RC_WORKDIR";
pub const ENV_CREATED_BY: &str = "SHED_RC_CREATED_BY";
pub const ENV_CREATED_AT: &str = "SHED_RC_CREATED_AT";
pub const ENV_TARGET: &str = "SHED_RC_TARGET";
/// The session's own slug, stamped for EVERY kind so a process launched INSIDE
/// the session (cursor's hook relay) can address it on the hub.
pub const ENV_SLUG: &str = "SHED_RC_SLUG";
/// opencode's allocated loopback HTTP/SSE port (opencode kind only).
pub const ENV_OPENCODE_PORT: &str = "SHED_RC_OPENCODE_PORT";
/// The prefix `parse_env` filters a `tmux show-environment` dump down to.
pub const ENV_PREFIX: &str = "SHED_RC_";

/// The bare (NOT `SHED_RC_`-prefixed) launch-env override stamped for every
/// opencode session so the hub's unauthenticated watcher never hits a 401 from an
/// inherited rc-file password (`meta.go:47-55`).
pub const ENV_OPENCODE_PASSWORD: &str = "OPENCODE_SERVER_PASSWORD";

/// claude's full-bypass `--permission-mode` value — what the generic `skip`
/// resolves to for the claude kinds (`rc.go:306`).
pub const PERMISSION_MODE_BYPASS: &str = "bypassPermissions";

/// The confusable-free slug alphabet (no `0`/`o`, no `1`/`l`/`i`) so a slug
/// survives being read off a QR code or typed from a URL (`rc.go:231`).
pub const SLUG_ALPHABET: &[u8] = b"abcdefghjkmnpqrstuvwxyz23456789";

/// Slug length (`rc.go:237`).
pub const SLUG_LEN: usize = 6;

// ---------------------------------------------------------------------------
// shell quoting (rc.go:290)
// ---------------------------------------------------------------------------

/// Wrap `s` in single quotes, escaping embedded single quotes with the POSIX
/// `'\''` trick — a VERBATIM port of the guest's `shellQuote` (`rc.go:290`).
///
/// **Always quotes**, even a token that needs no quoting (`my-shed/abc` →
/// `'my-shed/abc'`). That is deliberate and load-bearing, and is why this is NOT
/// [`crate::terminal::shell_quote`], which passes safe strings through bare: the
/// quoted form is written verbatim into interoperable artifacts — notably the
/// cursor `hooks.json` entries whose idempotent-match compares the literal
/// `shellQuote(scriptPath)` string — so a Rust engine emitting the bare form
/// would append a DUPLICATE hook next to a Go-written one on a mixed fleet.
pub fn shell_quote_always(s: &str) -> String {
    format!("'{}'", s.replace('\'', r"'\''"))
}

// ---------------------------------------------------------------------------
// the registry's tool axis
// ---------------------------------------------------------------------------

/// The tool backing a kind — the registry's row identity (`AgentSpec.Tool`,
/// `agents.go:181-187`). One tool backs one or more kinds (claude backs both
/// `claude-broker` and `claude-rc`); an unregistered kind resolves to no tool at
/// all, which is the unknown-kind policy's neutral-render signal.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum AgentTool {
    Claude,
    Codex,
    Opencode,
    Cursor,
    Shell,
}

impl AgentTool {
    /// The tool's stable identity token — the key under `capabilities.agents`.
    pub fn as_str(&self) -> &'static str {
        match self {
            AgentTool::Claude => "claude",
            AgentTool::Codex => "codex",
            AgentTool::Opencode => "opencode",
            AgentTool::Cursor => "cursor",
            AgentTool::Shell => "shell",
        }
    }

    /// The executable probed for capabilities (`command -v <bin>` +
    /// `<bin> --version`) — usually the tool token, except cursor, whose binary
    /// is `cursor-agent`. `None` for a tool with nothing to probe (shell).
    pub fn bin(&self) -> Option<&'static str> {
        match self {
            AgentTool::Claude => Some("claude"),
            AgentTool::Codex => Some("codex"),
            AgentTool::Opencode => Some("opencode"),
            AgentTool::Cursor => Some("cursor-agent"),
            AgentTool::Shell => None,
        }
    }
}

/// The tool backing `kind`, or `None` for an unregistered kind (`specForKind`,
/// `agents.go:576`).
///
/// Related to but NOT the same axis as [`RcKind::tool`], the client's
/// capabilities-key mapping: that one folds `shell` in with the unknown kinds
/// (nothing installable to probe), where the registry gives shell a row whose
/// [`AgentTool::bin`] is `None`. The client's answer is therefore
/// `tool_for(k).filter(|t| t.bin().is_some())` — pinned by a test below so the
/// two tables cannot drift apart.
pub fn tool_for(kind: &RcKind) -> Option<AgentTool> {
    match kind {
        RcKind::ClaudeRc | RcKind::ClaudeBroker => Some(AgentTool::Claude),
        RcKind::Codex => Some(AgentTool::Codex),
        RcKind::Opencode => Some(AgentTool::Opencode),
        RcKind::Cursor => Some(AgentTool::Cursor),
        RcKind::Shell => Some(AgentTool::Shell),
        RcKind::Other(_) => None,
    }
}

/// Every recognized kind, in the order pinned by the capabilities wire contract
/// (`allKinds`, `rc.go:49`).
pub fn all_kinds() -> [RcKind; 6] {
    [
        RcKind::ClaudeBroker,
        RcKind::ClaudeRc,
        RcKind::Codex,
        RcKind::Opencode,
        RcKind::Cursor,
        RcKind::Shell,
    ]
}

/// The lane a kind's sessions run in (`laneForKind`, `agents.go:173`). Every spec
/// in this phase declares [`LANE_TUI`], and an UNREGISTERED kind renders as one
/// too (the unknown-kind policy: neutral rendering IS the TUI affordance set), so
/// this never returns anything else — the function exists so the DTO's
/// always-present `lane` has one derivation site to change when a structured lane
/// lands.
pub fn lane_for_kind(_kind: &RcKind) -> &'static str {
    LANE_TUI
}

/// The per-agent login remediation for a kind's `needs-auth` state (`AuthHintFor`,
/// `agents.go:594`), with the neutral fallback for `shell` and unknown kinds.
/// Delegates to [`RcKind::auth_hint`], which already carries the Go table — this
/// alias exists so an engine caller finds the registry entry point where the rest
/// of the registry lives.
pub fn auth_hint_for(kind: &RcKind) -> &'static str {
    kind.auth_hint()
}

// ---------------------------------------------------------------------------
// permission-flag resolution (agents.go:130-143 + the per-spec PermMap tables)
// ---------------------------------------------------------------------------

/// Resolve a kind's permission mode to the tool's argv flags, mirroring
/// `permFlagsFor` → `AgentSpec.permFlags` (`agents.go:130-143,583`).
///
/// - `Some(flags)` — the mode is accepted; `""` (no posture) is always accepted
///   for a REGISTERED kind and yields no flags.
/// - `None` — the kind is unregistered, or the mode is one the kind does not
///   accept.
///
/// Note the Go quirk faithfully preserved: an UNREGISTERED kind rejects even the
/// empty mode, because `permFlagsFor` fails at the spec lookup before it can
/// special-case `""`. It is unreachable in practice (the inner-command builder
/// returns `bash -l` for an unknown kind before asking, and mode validation
/// short-circuits on `""`), but a differential harness would see any divergence.
pub fn perm_flags<'a>(kind: &RcKind, mode: &'a str) -> Option<Vec<&'a str>> {
    let tool = tool_for(kind)?;
    if mode.is_empty() {
        return Some(Vec::new());
    }
    let generic: Option<&[&'a str]> = match (tool, mode) {
        // Every tool defines all three generic keys; a `default` posture is
        // always "pass nothing and let the tool decide".
        (_, "default") => Some(&[]),
        (AgentTool::Claude, "auto") => Some(&["--permission-mode", "auto"]),
        (AgentTool::Claude, "skip") => Some(&["--permission-mode", PERMISSION_MODE_BYPASS]),
        // codex 0.144.1 removed the top-level `--full-auto`; the
        // autonomous-with-approvals posture is now spelled out explicitly.
        (AgentTool::Codex, "auto") => Some(&[
            "--ask-for-approval",
            "on-request",
            "--sandbox",
            "workspace-write",
        ]),
        (AgentTool::Codex, "skip") => Some(&["--dangerously-bypass-approvals-and-sandbox"]),
        // opencode's `--auto` approves everything not denied — the closest
        // mapping for BOTH auto and skip until a finer split exists.
        (AgentTool::Opencode, "auto" | "skip") => Some(&["--auto"]),
        // cursor has no mid-tier posture; auto stays default until one exists.
        (AgentTool::Cursor, "auto") => Some(&[]),
        (AgentTool::Cursor, "skip") => Some(&["--force"]),
        // A shell has no permission posture; the generic modes are accepted
        // (valid for ALL kinds) but produce nothing.
        (AgentTool::Shell, "auto" | "skip") => Some(&[]),
        _ => None,
    };
    if let Some(flags) = generic {
        return Some(flags.to_vec());
    }
    // claude additionally accepts its full historical --permission-mode set,
    // passed through verbatim.
    if tool == AgentTool::Claude && CLAUDE_EXTRA_MODES.contains(&mode) {
        return Some(vec!["--permission-mode", mode]);
    }
    None
}

/// Whether `kind` accepts the (non-empty) permission mode `m` — the kind-aware
/// check mirroring `PermModeAcceptedBy` (`rc.go:325`). Kept distinct from
/// [`crate::rc::validate_permission_mode`], the CLIENT gate, which silently DROPS
/// a mode for a kind without a posture instead of resolving flags for it.
pub fn perm_mode_accepted_by(kind: &RcKind, m: &str) -> bool {
    perm_flags(kind, m).is_some()
}

/// Whether `m` is a permission mode the CLAUDE kinds accept — the generic
/// tri-state plus claude's historical set (`ValidPermissionMode`, `rc.go:317`).
/// `""` is the ABSENCE of a mode, not a mode, so it is rejected here even though
/// `perm_flags(kind, "")` is a valid no-posture resolution.
pub fn valid_claude_permission_mode(m: &str) -> bool {
    !m.is_empty() && perm_flags(&RcKind::ClaudeRc, m).is_some()
}

/// The engine-side permission-mode gate (`validatePermissionMode`, `rc.go:333`).
/// `Ok(())` for the empty mode (no posture) and for any mode the kind accepts;
/// otherwise the domain error, which distinguishes a claude-ONLY mode used on a
/// non-claude kind from a plain invalid mode — the two produce different operator
/// messages and the harness diffs them.
pub fn validate_engine_permission_mode(kind: &RcKind, mode: &str) -> Result<(), RcAgentError> {
    if mode.is_empty() || perm_mode_accepted_by(kind, mode) {
        return Ok(());
    }
    if valid_claude_permission_mode(mode) {
        return Err(RcAgentError::ClaudeOnlyPermissionMode {
            mode: mode.to_string(),
            kind: kind.as_str().to_string(),
        });
    }
    Err(RcAgentError::InvalidPermissionMode {
        mode: mode.to_string(),
    })
}

/// The permission modes valid for `kind`: the generic tri-state, plus claude's
/// historical extras for the claude kinds — the registry's mirror of
/// `genericPermModes` + `ExtraModes` (`rc.go:310`, `agents.go:432`).
///
/// Re-exported under the engine's name rather than re-derived: the client-side
/// [`crate::rc::permission_modes_for`] already computes exactly this, and a
/// second copy of one table is precisely the drift this module exists to
/// prevent. (Unlike [`validate_engine_permission_mode`], which is deliberately
/// its own thing — the client gate has different semantics and different
/// messages.)
pub use crate::rc::permission_modes_for as engine_permission_modes;

// ---------------------------------------------------------------------------
// inner commands (agents.go:606-671)
// ---------------------------------------------------------------------------

/// Build the command the tmux session runs for `kind` — the single argv token
/// handed to `tmux new-session` (`InnerCommand`, `rc.go:366` → the per-spec
/// builders, `agents.go:606-671`).
///
/// - claude has THREE forms: the broker (`claude remote-control --name '<d>'
///   [flags] --spawn same-dir`), claude-rc WITH a posture (`claude
///   --remote-control --name '<d>' --permission-mode <m>` — the bare `/rc` slash
///   command takes no flags), and claude-rc WITHOUT one (the legacy `claude
///   --name '<d>' /rc` form, kept for backward compatibility).
/// - codex/opencode/cursor are the plain-TUI form `<bin> [flags]`; the display
///   name is metadata only (these TUIs take no `--name`).
/// - shell is always `bash -l`, ignoring mode, wrap, and port.
/// - An unregistered kind falls back to `bash -l`.
///
/// `interactive_shell` wraps the result in `bash -ic '<cmd>'` so a login rc-file
/// loads PATH (nvm/asdf) before the tool is exec'd — the native-machine
/// accommodation; sheds bake the tools into the system path.
///
/// **`port` and the wrap are order-coupled.** opencode's allocated loopback port
/// appends `--port <p> --hostname 127.0.0.1` to the command BEFORE the optional
/// wrap: appending after would make `--port …` extra argv tokens handed to `bash`
/// itself rather than part of the quoted string bash execs, so opencode would
/// never see the flag. Only [`RcKind::Opencode`] with a non-zero port consumes
/// it; every other kind ignores it silently.
pub fn inner_command(
    kind: &RcKind,
    display_name: &str,
    permission_mode: &str,
    interactive_shell: bool,
    port: u16,
) -> String {
    let Some(tool) = tool_for(kind) else {
        return "bash -l".to_string();
    };
    // Validity is pre-checked by the create path; an unaccepted mode degrades to
    // "no flags", exactly as Go's discarded `ok` does.
    let flags = perm_flags(kind, permission_mode).unwrap_or_default();
    let cmd = match tool {
        AgentTool::Shell => return "bash -l".to_string(),
        AgentTool::Claude => {
            match kind {
                RcKind::ClaudeBroker => {
                    let mut c = format!(
                        "claude remote-control --name {}",
                        shell_quote_always(display_name)
                    );
                    if !flags.is_empty() {
                        c.push(' ');
                        c.push_str(&flags.join(" "));
                    }
                    c.push_str(" --spawn same-dir");
                    c
                }
                RcKind::ClaudeRc if !flags.is_empty() => format!(
                    "claude --remote-control --name {} {}",
                    shell_quote_always(display_name),
                    flags.join(" ")
                ),
                RcKind::ClaudeRc => {
                    format!("claude --name {} /rc", shell_quote_always(display_name))
                }
                // Unreachable: the claude tool backs exactly the two kinds above.
                // Go's builder has the same `default: return "bash -l"` arm.
                _ => return "bash -l".to_string(),
            }
        }
        AgentTool::Codex | AgentTool::Opencode | AgentTool::Cursor => {
            let mut cmd = tool.bin().unwrap_or_default().to_string();
            // Spec-owned base posture flags, emitted BEFORE the permission
            // flags (Go `innerCommandTUI(bin, baseFlags...)` — order is
            // wire-visible and pinned by the rc-parity argv transcripts).
            // cursor: --trust skips the workspace-trust dialog, which neither
            // classifier models (same posture as claude's trust preseed; the
            // rc environment is a sandbox VM or a deliberately-targeted
            // machine). Verified live 2026-08-17. If any OTHER tool ever grows
            // a base flag, mirror Go's spec-owned baseFlags shape AND add its
            // argv parity cell — this arm is the whole mirror today.
            if matches!(tool, AgentTool::Cursor) {
                cmd.push_str(" --trust");
            }
            if !flags.is_empty() {
                cmd.push(' ');
                cmd.push_str(&flags.join(" "));
            }
            if matches!(kind, RcKind::Opencode) && port != 0 {
                cmd.push_str(&format!(" --port {port} --hostname 127.0.0.1"));
            }
            cmd
        }
    };
    if interactive_shell {
        return format!("bash -ic {}", shell_quote_always(&cmd));
    }
    cmd
}

// ---------------------------------------------------------------------------
// prompt matchers (rc.go:374-398)
// ---------------------------------------------------------------------------

static RE_NOT_TRUSTED: LazyLock<Regex> =
    LazyLock::new(|| Regex::new(r"(?i)Workspace not trusted").unwrap());
static RE_SAFETY_CHECK: LazyLock<Regex> =
    LazyLock::new(|| Regex::new(r"(?i)Quick safety check").unwrap());
static RE_TRUST_FOLDER: LazyLock<Regex> =
    LazyLock::new(|| Regex::new(&format!(r"(?i)Yes,{GO_SPACE}*I trust this folder")).unwrap());

/// Whether the pane is showing claude's first-run workspace-trust prompt
/// (`IsTrustPrompt`, `rc.go:382`) — the gate `accept-trust` re-verifies before
/// sending Enter, and the `--wait` poller's auto-accept trigger.
pub fn is_trust_prompt(pane: &str) -> bool {
    RE_NOT_TRUSTED.is_match(pane)
        || RE_SAFETY_CHECK.is_match(pane)
        || RE_TRUST_FOLDER.is_match(pane)
}

static RE_BYPASS_WARN: LazyLock<Regex> =
    LazyLock::new(|| Regex::new(r"(?i)Bypass Permissions mode").unwrap());
static RE_BYPASS_ACCEPT: LazyLock<Regex> =
    LazyLock::new(|| Regex::new(&format!(r"(?i)Yes,{GO_SPACE}*I accept")).unwrap());

/// Whether the pane is showing claude's one-time "Bypass Permissions mode"
/// acceptance dialog (`IsBypassAcceptPrompt`, `rc.go:396`). Requires **both**
/// halves — the warning headline AND the accept option — because either alone
/// appears in ordinary scrollback. Option "1. No, exit" is pre-selected, so the
/// creator must send `Down` before `Enter`.
pub fn is_bypass_accept_prompt(pane: &str) -> bool {
    RE_BYPASS_WARN.is_match(pane) && RE_BYPASS_ACCEPT.is_match(pane)
}

// ---------------------------------------------------------------------------
// pane classifiers (agents.go:673-925)
// ---------------------------------------------------------------------------

static RE_CLAUDE_NEEDS_AUTH: LazyLock<Regex> = LazyLock::new(|| {
    Regex::new(r"(?i)requires a claude\.ai subscription|not logged in|claude auth login").unwrap()
});
static RE_RECONNECTING: LazyLock<Regex> =
    LazyLock::new(|| Regex::new(r"\bReconnecting\b").unwrap());
static RE_CONNECTED: LazyLock<Regex> = LazyLock::new(|| Regex::new(r"\bConnected\b").unwrap());
static RE_RC_CONNECTING: LazyLock<Regex> =
    LazyLock::new(|| Regex::new(r"(?i)Remote Control connecting").unwrap());
static RE_RC_ACTIVE: LazyLock<Regex> =
    LazyLock::new(|| Regex::new(r"(?i)Remote Control active").unwrap());

/// The claude.ai remote-control URL for a claude kind (broker and rc use
/// different URL shapes); `None` for every other kind (`extractURL`,
/// `agents.go:686`).
///
/// Re-exported from [`crate::rc::extract_url`], which is already a character-for-
/// character match for Go's — the URL grammar is claude.ai's, so unlike the
/// classifier ANCHORS below (whose Go and Swift oracles can word things
/// differently) there is nothing here for the two sides to diverge on.
pub use crate::rc::extract_url;

fn result(state: RcState, url: Option<String>) -> RcClassification {
    RcClassification { state, url }
}

/// `classifyClaude` (`agents.go:699`) — the trust/auth gates precede the per-kind
/// ready logic because either can appear for either claude kind.
///
/// The banner-word arms (`Connected`, `Remote Control active`, `Remote Control
/// connecting`) are OUTCOME-NEUTRAL by construction: each is immediately followed
/// by the same-verdict url-presence arm that subsumes it. They are kept because
/// Go has them in exactly this order, and this file mirrors structure rather than
/// outcome — do not read them as precedence-critical.
fn classify_claude(kind: &RcKind, pane: &str) -> RcClassification {
    if is_trust_prompt(pane) {
        return result(RcState::NeedsTrust, extract_url(kind, pane));
    }
    if RE_CLAUDE_NEEDS_AUTH.is_match(pane) {
        return result(RcState::NeedsAuth, extract_url(kind, pane));
    }
    match kind {
        RcKind::ClaudeBroker => {
            let url = extract_url(&RcKind::ClaudeBroker, pane);
            if RE_RECONNECTING.is_match(pane) {
                return result(RcState::Reconnecting, url);
            }
            if RE_CONNECTED.is_match(pane) && url.is_some() {
                return result(RcState::Ready, url);
            }
            if url.is_some() {
                return result(RcState::Ready, url);
            }
            result(RcState::Starting, None)
        }
        RcKind::ClaudeRc => {
            let url = extract_url(&RcKind::ClaudeRc, pane);
            if RE_RC_CONNECTING.is_match(pane) && url.is_none() {
                return result(RcState::Starting, None);
            }
            if RE_RC_ACTIVE.is_match(pane) && url.is_some() {
                return result(RcState::Ready, url);
            }
            if url.is_some() {
                return result(RcState::Ready, url);
            }
            result(RcState::Starting, None)
        }
        _ => result(RcState::Starting, None),
    }
}

/// codex's empty-composer placeholder — shared by the ready regex (banner OR
/// composer ⇒ ready) and the prompt anchor (composer only ⇒ needs_input), so the
/// two cannot drift if codex rewords the hint (`agents.go:741`).
const CODEX_COMPOSER_PLACEHOLDER: &str = r"Find and fix a bug in @filename";

static RE_CODEX_READY: LazyLock<Regex> = LazyLock::new(|| {
    Regex::new(&format!(
        r">_ OpenAI Codex \(v|{CODEX_COMPOSER_PLACEHOLDER}"
    ))
    .unwrap()
});
static RE_CODEX_TRUST: LazyLock<Regex> =
    LazyLock::new(|| Regex::new(r"(?i)Do you trust the contents of this directory\?").unwrap());
static RE_CODEX_AUTH: LazyLock<Regex> = LazyLock::new(|| {
    Regex::new(r"Provided authentication token is expired|token_expired|Sign in with ChatGPT")
        .unwrap()
});

/// `classifyCodex` (`agents.go:760`). **READY IS CHECKED FIRST**, deliberately:
/// the composer banner means codex is usable and must win even over the inline
/// MCP `token_expired` warning, which is a sub-service failure printed ON the
/// working ready screen, not a core-auth failure.
fn classify_codex(pane: &str) -> RcClassification {
    if RE_CODEX_READY.is_match(pane) {
        return result(RcState::Ready, None);
    }
    if RE_CODEX_TRUST.is_match(pane) {
        return result(RcState::NeedsTrust, None);
    }
    if RE_CODEX_AUTH.is_match(pane) {
        return result(RcState::NeedsAuth, None);
    }
    result(RcState::Starting, None)
}

static RE_OPENCODE_PLACEHOLDER: LazyLock<Regex> =
    LazyLock::new(|| Regex::new(r"Ask anything\.\.\.").unwrap());
static RE_OPENCODE_FOOTER: LazyLock<Regex> =
    LazyLock::new(|| Regex::new(r"ctrl\+p commands").unwrap());
static RE_OPENCODE_AUTH_SCREEN: LazyLock<Regex> = LazyLock::new(|| {
    Regex::new(r"(?i)\bsign in\b|\blog ?in to\b|\bauthenticate\b|\bopencode auth\b").unwrap()
});
/// opencode's auto-opened "Connect a provider" dialog — a CONJUNCTION of the
/// headline and the `Popular` category header that appears alone on its own line
/// a few rows below, because the headline alone is ordinary English an agent
/// could quote back (`agents.go:836`).
static RE_OPENCODE_AUTH_DIALOG: LazyLock<Regex> = LazyLock::new(|| {
    Regex::new(r"(?m)^[ \t]*Connect a provider\b(?:[^\n]*\n){0,10}[ \t]*Popular[ \t]*$").unwrap()
});

/// `classifyOpencode` (`agents.go:849`). The auth dialog is checked FIRST — it is
/// a full-screen overlay that replaces the composer/footer, so it never races the
/// ready checks. The composer placeholder is UNCONDITIONAL ready; the persistent
/// footer alone means ready only when the pane does not otherwise look like an
/// auth/onboarding screen (a wrong ready would deliver a prompt into a login
/// screen, whereas a wrong starting self-corrects on the next poll).
fn classify_opencode(pane: &str) -> RcClassification {
    if RE_OPENCODE_AUTH_DIALOG.is_match(pane) {
        return result(RcState::NeedsAuth, None);
    }
    if RE_OPENCODE_PLACEHOLDER.is_match(pane) {
        return result(RcState::Ready, None);
    }
    if RE_OPENCODE_FOOTER.is_match(pane) && !RE_OPENCODE_AUTH_SCREEN.is_match(pane) {
        return result(RcState::Ready, None);
    }
    result(RcState::Starting, None)
}

static RE_CURSOR_AUTH: LazyLock<Regex> = LazyLock::new(|| {
    Regex::new(
        r"(?i)Press any key to log in\.\.\.|Authentication required to use Cursor Agent|click this link to log in",
    )
    .unwrap()
});
/// cursor's authed composer placeholder, line-anchored with its arrow prefix so
/// the phrase quoted inside agent output can't read as ready. cursor swaps the
/// text after the first exchange (`Plan, search, build anything` → `Add a
/// follow-up`); both are the same ready chrome (`agents.go:876`).
static RE_CURSOR_READY: LazyLock<Regex> = LazyLock::new(|| {
    Regex::new(&format!(
        r"(?m)^{GO_SPACE}*→ (?:Plan, search, build anything|Add a follow-up){GO_SPACE}*$"
    ))
    .unwrap()
});

/// `classifyCursor` (`agents.go:881`). The auth screens and the authed composer
/// are disjoint, so auth is checked first and ready second.
fn classify_cursor(pane: &str) -> RcClassification {
    if RE_CURSOR_AUTH.is_match(pane) {
        return result(RcState::NeedsAuth, None);
    }
    if RE_CURSOR_READY.is_match(pane) {
        return result(RcState::Ready, None);
    }
    result(RcState::Starting, None)
}

/// `classifyShell` (`agents.go:895`) — ready as soon as the pane has drawn
/// anything (a prompt), starting while blank. A shell has no trust/auth/url
/// states and its prompt is the ready signal, never a death.
fn classify_shell(pane: &str) -> RcClassification {
    if pane.trim().is_empty() {
        result(RcState::Starting, None)
    } else {
        result(RcState::Ready, None)
    }
}

/// A line that IS a bare shed guest login-shell prompt (`[shed:<name>] <cwd> $`)
/// and nothing else — fully anchored, applied to the TRIMMED line
/// (`shedShellPromptRe`, `agents.go:909`). The anchoring matters twice over: a
/// launch-line command echo (`[shed:x] ~ $ codex`) has text after the `$`, and a
/// running agent merely PRINTING a prompt-shaped line must not read as a death.
static RE_SHED_SHELL_PROMPT: LazyLock<Regex> =
    LazyLock::new(|| Regex::new(r"^\[shed:[^\]]+\][^$]*\$$").unwrap());

/// Whether the pane's last non-empty line is a bare shed shell prompt — the agent
/// returned to the login shell (`exitedToShell`, `agents.go:915`).
///
/// **An EMPTY pane is NOT dead.** A blank pane is a just-started/ambiguous
/// session; the real death of a whole session surfaces as a capture failure at
/// the ops layer, not here.
pub fn exited_to_shell(pane: &str) -> bool {
    for line in pane.split('\n').rev() {
        let line = line.trim();
        if line.is_empty() {
            continue;
        }
        return RE_SHED_SHELL_PROMPT.is_match(line);
    }
    false
}

/// **The engine pane classifier** — derive `(state, url)` from a captured pane
/// for a kind (`ClassifyPane`, `rc.go:406`).
///
/// An unregistered (unknown) kind renders neutrally as a plain shell pane: a
/// state, never a claude URL affordance (the unknown-kind policy). For every
/// agent kind a shared shed-guest DEAD check runs FIRST — if the agent has exited
/// back to the login shell the session is dead regardless of any auth/trust text
/// still sitting in scrollback. `shell` is exempt (its prompt is the ready state).
///
/// This is the authoritative classifier and covers every agent, unlike
/// [`crate::rc::classify_pane`], the claude-only best-effort CLIENT utility.
pub fn classify_pane(kind: &RcKind, pane: &str) -> RcClassification {
    let Some(tool) = tool_for(kind) else {
        return classify_shell(pane);
    };
    if tool != AgentTool::Shell && exited_to_shell(pane) {
        return result(RcState::Dead, None);
    }
    match tool {
        AgentTool::Claude => classify_claude(kind, pane),
        AgentTool::Codex => classify_codex(pane),
        AgentTool::Opencode => classify_opencode(pane),
        AgentTool::Cursor => classify_cursor(pane),
        AgentTool::Shell => classify_shell(pane),
    }
}

// ---------------------------------------------------------------------------
// prompt anchors (agents.go:193-222) — registry data, hub consumers stay Go
// ---------------------------------------------------------------------------

static RE_CLAUDE_PROMPT_ANCHOR: LazyLock<Regex> = LazyLock::new(|| {
    Regex::new(&format!(
        r#"(?m)^{GO_SPACE}*>{GO_SPACE}+Try "|\? for shortcuts"#
    ))
    .unwrap()
});
static RE_CODEX_PROMPT_ANCHOR: LazyLock<Regex> =
    LazyLock::new(|| Regex::new(CODEX_COMPOSER_PLACEHOLDER).unwrap());
static RE_OPENCODE_PROMPT_ANCHOR: LazyLock<Regex> = LazyLock::new(|| {
    Regex::new(&format!(
        r"{}|{}",
        RE_OPENCODE_PLACEHOLDER.as_str(),
        RE_OPENCODE_FOOTER.as_str()
    ))
    .unwrap()
});
static RE_SHELL_PROMPT_ANCHOR: LazyLock<Regex> = LazyLock::new(|| {
    Regex::new(&format!(
        r"(?m)^{GO_SPACE}*\[shed:[^\]]+\][^$]*\${GO_SPACE}*$"
    ))
    .unwrap()
});

/// The kind's PROMPT ANCHOR — the empty-composer / waiting-for-input chrome the
/// pane-stability engine uses to split `needs_input` from plain `idle` on a quiet
/// pane (`promptAnchorFor`, `agents.go:533`). `None` for an unregistered kind.
///
/// Ported as registry DATA for completeness: the only consumers today are the Go
/// hub's stability engine (which is NOT ported this block — the hub stays Go),
/// so nothing in Rust reads these yet. They live here so the registry is one
/// table rather than one-and-a-bit, and so the port's next consumer does not have
/// to re-derive them from the agent bundles.
pub fn prompt_anchor_for(kind: &RcKind) -> Option<&'static Regex> {
    match tool_for(kind)? {
        AgentTool::Claude => Some(&RE_CLAUDE_PROMPT_ANCHOR),
        AgentTool::Codex => Some(&RE_CODEX_PROMPT_ANCHOR),
        AgentTool::Opencode => Some(&RE_OPENCODE_PROMPT_ANCHOR),
        // cursor reuses its ready regex as the prompt anchor.
        AgentTool::Cursor => Some(&RE_CURSOR_READY),
        AgentTool::Shell => Some(&RE_SHELL_PROMPT_ANCHOR),
    }
}

// ---------------------------------------------------------------------------
// slugs (rc.go:231-250)
// ---------------------------------------------------------------------------

/// Generate a 6-char slug from the confusable-free [`SLUG_ALPHABET`]
/// (`GenSlug`, `rc.go:235`).
///
/// Go draws each character with `crypto/rand.Int`, which is uniform by
/// construction. Rust takes its entropy from **UUID v4 bytes** (`uuid` is already
/// a workspace dependency; adding a CSPRNG crate for six characters is not worth
/// a new dep) and applies **rejection sampling** to stay uniform: the alphabet is
/// 31 characters, so `256 % 31 == 8` bytes would otherwise be over-represented.
/// Bytes ≥ `248` (`31 * 8`) are discarded and another UUID drawn. Two caveats
/// make plain "every byte" sampling WRONG for a v4 UUID and are handled here:
/// bytes 6 and 8 carry the fixed version/variant bits (byte 6 ∈ 0x40..=0x4f,
/// byte 8 ∈ 0x80..=0xbf — never uniform, always < 248, so they'd pass the
/// rejection filter and skew ~17% of slugs toward a 16-value window), so both
/// are skipped; the remaining 14 bytes are full-range random, giving an
/// expected ~0.03 extra draws per slug.
pub fn gen_slug() -> String {
    let n = SLUG_ALPHABET.len() as u8; // 31
    let limit = (u8::MAX / n) * n; // 248 — the largest unbiased multiple
    let mut out = String::with_capacity(SLUG_LEN);
    while out.len() < SLUG_LEN {
        for (i, b) in Uuid::new_v4().as_bytes().iter().enumerate() {
            // Skip the RFC 4122 version (6) and variant (8) bytes — not uniform.
            if i == 6 || i == 8 || *b >= limit {
                continue;
            }
            out.push(SLUG_ALPHABET[(*b % n) as usize] as char);
            if out.len() == SLUG_LEN {
                break;
            }
        }
    }
    out
}

static RE_SLUG: LazyLock<Regex> =
    LazyLock::new(|| Regex::new(r"^[a-z0-9](?:[a-z0-9-]{0,30}[a-z0-9])?$").unwrap());

/// Whether a CALLER-supplied slug matches the grammar (`ValidCallerSlug`,
/// `rc.go:250`): lowercase alphanumerics and inner hyphens, 1–32 chars, no
/// leading/trailing hyphen.
pub fn valid_caller_slug(slug: &str) -> bool {
    RE_SLUG.is_match(slug)
}

/// The tmux session name for a slug (`rc-<slug>`). Re-exported from
/// [`crate::rc::tmux_name`] — engine and client must name the same session from
/// the same slug, so there is one derivation site.
pub use crate::rc::tmux_name;

// ---------------------------------------------------------------------------
// SHED_RC_* metadata (meta.go)
// ---------------------------------------------------------------------------

/// Whether `s` contains a control character — Go's `HasControlChars`
/// (`rc.go:254`): C0 (`<= 0x1f`, so newline/CR/tab included) and DEL (`0x7f`).
///
/// **Narrower than [`crate::rc::is_safe_rc_value`] on purpose.** That client-side
/// check uses `char::is_control`, which also covers the C1 block (U+0080–U+009F)
/// — deliberately stricter so a client never *sends* a value the guest would
/// reject. This one is the ENGINE's gate and must match Go byte for byte, or the
/// two implementations would accept different `SHED_RC_*` values.
pub fn has_control_chars(s: &str) -> bool {
    s.chars().any(|c| c <= '\u{1f}' || c == '\u{7f}')
}

/// The write-once `SHED_RC_*` set stamped into a managed session at create
/// (`rc.Metadata`, `meta.go:11`).
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct RcMetadata {
    pub id: String,
    pub display_name: String,
    pub kind: RcKind,
    pub workdir: String,
    pub created_by: String,
    /// RFC3339 UTC (`…Z`).
    pub created_at: String,
    /// Optional advisory label; omitted from the env when empty.
    pub target: String,
    /// The session's slug — stamped for EVERY kind (see [`ENV_SLUG`]).
    pub slug: String,
    /// opencode's allocated loopback port (`0` = none / not opencode). Ignored
    /// for non-opencode kinds even when non-zero.
    pub port: u16,
}

/// Errors raised by the pure registry/metadata layer. Each variant's message is
/// the Go original's UNWRAPPED text: Go additionally wraps the two
/// permission-mode variants with `%w`-of-`ErrBadArgs`, which prepends
/// `invalid arguments: ` on stderr and drives exit code 2 (`ops.go:15`,
/// `clirc.go:157`). That wrapping is the ENGINE ops layer's job — the Rust
/// engine (shed-app `rc_engine`) adds the same class + prefix when it maps
/// these to CLI outcomes, and the parity harness diffs the wrapped form.
/// `ControlChars` is genuinely unwrapped in Go's `validate` layer and gets its
/// `ErrBadArgs` wrap at the same call sites the Go engine wraps it
/// (`meta.go`→`ops.go:192-195`).
#[derive(Debug, Clone, PartialEq, Eq, thiserror::Error)]
pub enum RcAgentError {
    /// `envValue` (`meta.go:33`).
    #[error("{key} must not contain control characters")]
    ControlChars { key: String },
    /// `validatePermissionMode`'s claude-only arm (`rc.go:341`).
    #[error("permission mode {mode:?} is claude-only; {kind} kinds accept default|auto|skip")]
    ClaudeOnlyPermissionMode { mode: String, kind: String },
    /// `validatePermissionMode`'s generic arm (`rc.go:344`).
    #[error("invalid permission mode {mode:?} (want default|auto|skip)")]
    InvalidPermissionMode { mode: String },
}

/// `envValue`'s guard (`meta.go:33`): a `SHED_RC_*` value carrying a control
/// character would break the `KEY=value` framing of the tmux env dump the reader
/// parses back, so it is rejected at the writer.
fn check_env_value(key: &str, value: &str) -> Result<(), RcAgentError> {
    if has_control_chars(value) {
        return Err(RcAgentError::ControlChars {
            key: key.to_string(),
        });
    }
    Ok(())
}

/// Build the `-e KEY=value …` argv fragment for `tmux new-session`, in the
/// deterministic order pinned by `BuildEnvArgs` (`meta.go:56-85`).
///
/// **The ORDER is the contract** — it is what a differential harness cannot learn
/// from `tmux show-environment` (tmux re-orders its dump), so it is pinned by the
/// unit tests below on both sides instead:
///
/// 1. `SHED_RC_V`, `SHED_RC_ID`, `SHED_RC_DISPLAY_NAME`, `SHED_RC_KIND`,
///    `SHED_RC_WORKDIR`, `SHED_RC_CREATED_BY`, `SHED_RC_CREATED_AT`,
///    `SHED_RC_SLUG` — always, in exactly that order;
/// 2. `SHED_RC_TARGET` — only when non-empty;
/// 3. `SHED_RC_OPENCODE_PORT` — only for the opencode kind with a non-zero port;
/// 4. a bare `OPENCODE_SERVER_PASSWORD=` (EMPTY value) for EVERY opencode
///    session, port or not. Deliberately NOT a `SHED_RC_*` key: it is a plain
///    launch-env override neutralizing any password an inherited rc file would
///    set, so the hub's unauthenticated watcher never hits a 401 — and because it
///    lacks the prefix, [`parse_env`] never reads it back as metadata.
///
/// Every value is control-char validated; the first offender aborts with
/// [`RcAgentError::ControlChars`] naming its key.
pub fn build_env_args(m: &RcMetadata) -> Result<Vec<String>, RcAgentError> {
    // Every borrowed scalar is materialized BEFORE `pairs` so it outlives the
    // borrows the table holds.
    let version = SCHEMA_VERSION.to_string();
    let kind = m.kind.as_str().to_string();
    let port = m.port.to_string();
    let is_opencode = matches!(m.kind, RcKind::Opencode);
    let mut pairs: Vec<(&str, &str)> = vec![
        (ENV_V, &version),
        (ENV_ID, &m.id),
        (ENV_DISPLAY_NAME, &m.display_name),
        (ENV_KIND, &kind),
        (ENV_WORKDIR, &m.workdir),
        (ENV_CREATED_BY, &m.created_by),
        (ENV_CREATED_AT, &m.created_at),
        (ENV_SLUG, &m.slug),
    ];
    if !m.target.is_empty() {
        pairs.push((ENV_TARGET, &m.target));
    }
    if is_opencode && m.port != 0 {
        pairs.push((ENV_OPENCODE_PORT, &port));
    }
    let mut args = Vec::with_capacity(pairs.len() * 2 + 2);
    for (key, value) in pairs {
        check_env_value(key, value)?;
        args.push("-e".to_string());
        args.push(format!("{key}={value}"));
    }
    if is_opencode {
        args.push("-e".to_string());
        args.push(format!("{ENV_OPENCODE_PASSWORD}="));
    }
    Ok(args)
}

/// Turn a `tmux show-environment` dump into `SHED_RC_*` key→value (`parseEnv`,
/// `meta.go:89`). tmux prints `KEY=value` for a set variable and a bare `-KEY`
/// for a REMOVED one — the removal lines have no `=` and are skipped, and every
/// line outside the `SHED_RC_` prefix is ignored (which is how the bare
/// `OPENCODE_SERVER_PASSWORD` override stays out of metadata).
pub fn parse_env(dump: &str) -> HashMap<String, String> {
    let mut out = HashMap::new();
    for line in dump.split('\n') {
        if !line.starts_with(ENV_PREFIX) {
            continue;
        }
        let Some(eq) = line.find('=') else { continue };
        out.insert(line[..eq].to_string(), line[eq + 1..].to_string());
    }
    out
}

static RE_CANONICAL_INT: LazyLock<Regex> = LazyLock::new(|| Regex::new(r"^[0-9]+$").unwrap());

/// Whether a raw `SHED_RC_V` denotes a MANAGED session (`isManagedVersion`,
/// `rc.go:424`): a canonical non-negative integer ≥ [`MIN_MANAGED_VERSION`]. A v1
/// value, a padded/signed/float/hex spelling, or anything unparseable is
/// legacy/unmanaged — there is no aliasing.
pub fn is_managed_version(raw: &str) -> bool {
    // i64, not u64: Go's strconv.Atoi is int-width, so a value in [2^63, 2^64)
    // reads UNMANAGED there — match the overflow behavior exactly.
    RE_CANONICAL_INT.is_match(raw)
        && raw
            .parse::<i64>()
            .is_ok_and(|n| n >= MIN_MANAGED_VERSION as i64)
}

static RE_RFC3339_UTC: LazyLock<Regex> = LazyLock::new(|| {
    Regex::new(r"^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(?:\.[0-9]+)?Z$").unwrap()
});

/// `normalizeCreatedAt` (`meta.go:106`) — a value that is not RFC3339 UTC is
/// DROPPED (empty), never passed through, so a client never renders a timestamp
/// it cannot parse.
fn normalize_created_at(raw: &str) -> String {
    if RE_RFC3339_UTC.is_match(raw) {
        raw.to_string()
    } else {
        String::new()
    }
}

/// Go's `omitempty` on a string field, in Rust's `Option` idiom: an empty value
/// becomes `None` so the serializer omits the key entirely (absent, not null,
/// not `""`).
fn none_if_empty(s: String) -> Option<String> {
    if s.is_empty() {
        None
    } else {
        Some(s)
    }
}

/// Reconstruct one session's DTO from its tmux env dump + pane capture
/// (`ParseSession`, `meta.go:120`).
///
/// - The **slug comes from the tmux session NAME** (`rc-` stripped) — the name is
///   the source of truth, never the redundant `SHED_RC_SLUG` value.
/// - `state`/`url` are derived from the pane (never stored) and `lane` from the
///   kind, which is why `lane` is present on legacy and unknown-kind rows too.
/// - A session with no valid `SHED_RC_V` is **legacy/unmanaged**: kind is forced
///   to `claude-broker`, `managed` is false, and every stray `SHED_RC_*` value is
///   IGNORED (an unmanaged session's env is not trusted metadata — it could have
///   been set by anything).
/// - A managed session's kind is preserved VERBATIM when unrecognized
///   (unknown-kind policy) so a newer tool's session renders neutrally rather
///   than inheriting claude-broker behavior.
/// - `display_fallback` receives the slug; the one-shot commands pass `None`, so
///   an unstored display name is OMITTED and the consuming app applies its own
///   target-aware `<shed>/<slug>` fallback (the binary, running inside the shed,
///   does not know the orchestrator's alias).
pub fn parse_session(
    tmux_session: &str,
    env_dump: &str,
    pane: &str,
    display_fallback: Option<&dyn Fn(&str) -> String>,
) -> RcSessionDto {
    let env = parse_env(env_dump);
    let slug = tmux_session
        .strip_prefix(TMUX_PREFIX)
        .unwrap_or(tmux_session)
        .to_string();
    let fallback_name = display_fallback.map(|f| f(&slug)).unwrap_or_default();
    let val = |k: &str| env.get(k).map(|v| v.trim().to_string()).unwrap_or_default();

    let managed = is_managed_version(&val(ENV_V));
    let kind = if managed {
        RcKind::from_wire(&val(ENV_KIND))
    } else {
        RcKind::ClaudeBroker
    };
    let c = classify_pane(&kind, pane);
    // The one mechanism behind "an unmanaged session's env is not trusted": every
    // stored value reads as absent unless the session is managed.
    let stored = |k: &str| if managed { none_if_empty(val(k)) } else { None };
    let name = stored(ENV_DISPLAY_NAME).unwrap_or(fallback_name);
    RcSessionDto {
        slug,
        tmux_session: tmux_session.to_string(),
        lane: Some(lane_for_kind(&kind).to_string()),
        kind,
        state: c.state,
        managed,
        display_name: none_if_empty(name),
        workdir: stored(ENV_WORKDIR),
        url: c.url,
        id: stored(ENV_ID),
        created_by: stored(ENV_CREATED_BY),
        created_at: stored(ENV_CREATED_AT)
            .and_then(|raw| none_if_empty(normalize_created_at(&raw))),
        target_label: stored(ENV_TARGET),
        activity: None,
        activity_at: None,
        last_message: None,
        pending_approvals: None,
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::rc::GENERIC_PERMISSION_MODES;
    use std::collections::HashSet;
    use std::path::{Path, PathBuf};

    // ---- shellQuote (mirrors Go TestShellQuote, rc_test.go:57) ----

    #[test]
    fn shell_quote_always_wraps() {
        for (input, want) in [
            ("plain", "'plain'"),
            ("two words", "'two words'"),
            ("it's mine", r"'it'\''s mine'"),
            // The pin that separates this from terminal.rs's conditional quoter:
            // a "safe" token is STILL wrapped.
            ("my-shed/abc", "'my-shed/abc'"),
            ("", "''"),
        ] {
            assert_eq!(shell_quote_always(input), want, "input {input:?}");
        }
    }

    // ---- inner commands (mirrors Go TestInnerCommand, rc_test.go:70) ----

    #[test]
    fn inner_command_table() {
        let cases: &[(&str, RcKind, &str, &str, bool, u16, &str)] = &[
            // No permission mode -> original, backward-compatible forms.
            (
                "broker no-mode",
                RcKind::ClaudeBroker,
                "my-shed/abc",
                "",
                false,
                0,
                "claude remote-control --name 'my-shed/abc' --spawn same-dir",
            ),
            (
                "claude-rc no-mode",
                RcKind::ClaudeRc,
                "my-shed/abc",
                "",
                false,
                0,
                "claude --name 'my-shed/abc' /rc",
            ),
            (
                "shell no-mode",
                RcKind::Shell,
                "my-shed/abc",
                "",
                false,
                0,
                "bash -l",
            ),
            (
                "claude-rc name with spaces",
                RcKind::ClaudeRc,
                "Friday Bug Fix",
                "",
                false,
                0,
                "claude --name 'Friday Bug Fix' /rc",
            ),
            (
                "claude-rc interactive wrap",
                RcKind::ClaudeRc,
                "x",
                "",
                true,
                0,
                r"bash -ic 'claude --name '\''x'\'' /rc'",
            ),
            (
                "shell ignores interactive wrap",
                RcKind::Shell,
                "x",
                "",
                true,
                0,
                "bash -l",
            ),
            // With a permission mode -> claude-rc switches to the --remote-control form.
            (
                "claude-rc auto mode",
                RcKind::ClaudeRc,
                "my-shed/abc",
                "auto",
                false,
                0,
                "claude --remote-control --name 'my-shed/abc' --permission-mode auto",
            ),
            (
                "claude-rc bypassPermissions mode",
                RcKind::ClaudeRc,
                "x",
                "bypassPermissions",
                false,
                0,
                "claude --remote-control --name 'x' --permission-mode bypassPermissions",
            ),
            (
                "claude-rc skip maps to bypass",
                RcKind::ClaudeRc,
                "x",
                "skip",
                false,
                0,
                "claude --remote-control --name 'x' --permission-mode bypassPermissions",
            ),
            (
                "broker auto mode",
                RcKind::ClaudeBroker,
                "b",
                "auto",
                false,
                0,
                "claude remote-control --name 'b' --permission-mode auto --spawn same-dir",
            ),
            (
                "claude-rc auto mode interactive wrap",
                RcKind::ClaudeRc,
                "x",
                "auto",
                true,
                0,
                r"bash -ic 'claude --remote-control --name '\''x'\'' --permission-mode auto'",
            ),
            (
                "shell ignores mode",
                RcKind::Shell,
                "x",
                "bypassPermissions",
                false,
                0,
                "bash -l",
            ),
            // opencode: a nonzero port appends --port/--hostname; zero omits it entirely.
            (
                "opencode with port",
                RcKind::Opencode,
                "x",
                "",
                false,
                4096,
                "opencode --port 4096 --hostname 127.0.0.1",
            ),
            (
                "opencode zero port omits flags",
                RcKind::Opencode,
                "x",
                "",
                false,
                0,
                "opencode",
            ),
            // interactiveShell: --port must land INSIDE the bash -ic quotes.
            (
                "opencode interactive wrap port inside quotes",
                RcKind::Opencode,
                "x",
                "",
                true,
                4096,
                r"bash -ic 'opencode --port 4096 --hostname 127.0.0.1'",
            ),
            // codex/cursor ignore a nonzero port even though the signature accepts one.
            (
                "codex ignores port",
                RcKind::Codex,
                "x",
                "",
                false,
                4096,
                "codex",
            ),
            (
                "cursor ignores port",
                RcKind::Cursor,
                "x",
                "",
                false,
                4096,
                "cursor-agent --trust",
            ),
            // Posture flags land before the opencode port flags.
            (
                "opencode auto with port",
                RcKind::Opencode,
                "x",
                "auto",
                false,
                4096,
                "opencode --auto --port 4096 --hostname 127.0.0.1",
            ),
            (
                "codex auto",
                RcKind::Codex,
                "x",
                "auto",
                false,
                0,
                "codex --ask-for-approval on-request --sandbox workspace-write",
            ),
            (
                "codex skip",
                RcKind::Codex,
                "x",
                "skip",
                false,
                0,
                "codex --dangerously-bypass-approvals-and-sandbox",
            ),
            (
                "cursor skip",
                RcKind::Cursor,
                "x",
                "skip",
                false,
                0,
                "cursor-agent --trust --force",
            ),
            (
                "cursor auto has no flag",
                RcKind::Cursor,
                "x",
                "auto",
                false,
                0,
                "cursor-agent --trust",
            ),
            (
                "default posture passes nothing",
                RcKind::Codex,
                "x",
                "default",
                false,
                0,
                "codex",
            ),
            // Unknown kind falls back to a login shell.
            (
                "unknown kind",
                RcKind::Other("future".into()),
                "x",
                "auto",
                true,
                4096,
                "bash -l",
            ),
        ];
        for (name, kind, display, mode, interactive, port, want) in cases {
            assert_eq!(
                inner_command(kind, display, mode, *interactive, *port),
                *want,
                "case {name}"
            );
        }
    }

    // ---- permission flags (mirrors Go TestPermModeAcceptedBy, rc_test.go:179) ----

    #[test]
    fn generic_tri_state_is_valid_for_every_kind() {
        for kind in all_kinds() {
            for mode in GENERIC_PERMISSION_MODES {
                assert!(
                    perm_mode_accepted_by(&kind, mode),
                    "{} should accept {mode}",
                    kind.as_str()
                );
            }
            // "" (no posture) resolves to no flags for every registered kind.
            assert_eq!(perm_flags(&kind, ""), Some(vec![]));
        }
    }

    #[test]
    fn claude_extra_modes_are_claude_only() {
        for mode in CLAUDE_EXTRA_MODES {
            for kind in [RcKind::ClaudeRc, RcKind::ClaudeBroker] {
                assert_eq!(
                    perm_flags(&kind, mode),
                    Some(vec!["--permission-mode", mode]),
                    "claude should accept {mode}"
                );
            }
            for kind in [
                RcKind::Codex,
                RcKind::Opencode,
                RcKind::Cursor,
                RcKind::Shell,
            ] {
                assert_eq!(
                    perm_flags(&kind, mode),
                    None,
                    "{} must reject {mode}",
                    kind.as_str()
                );
            }
        }
        assert_eq!(perm_flags(&RcKind::Codex, "yolo"), None);
        // An unregistered kind resolves nothing at all — not even the empty mode
        // (Go's permFlagsFor fails at the spec lookup first).
        assert_eq!(perm_flags(&RcKind::Other("future".into()), ""), None);
        assert_eq!(perm_flags(&RcKind::Other("future".into()), "auto"), None);
    }

    #[test]
    fn perm_flag_resolution_table() {
        let cases: &[(RcKind, &str, &[&str])] = &[
            (RcKind::ClaudeRc, "default", &[]),
            (RcKind::ClaudeRc, "auto", &["--permission-mode", "auto"]),
            (
                RcKind::ClaudeRc,
                "skip",
                &["--permission-mode", "bypassPermissions"],
            ),
            (
                RcKind::ClaudeBroker,
                "skip",
                &["--permission-mode", "bypassPermissions"],
            ),
            (RcKind::Codex, "default", &[]),
            (
                RcKind::Codex,
                "auto",
                &[
                    "--ask-for-approval",
                    "on-request",
                    "--sandbox",
                    "workspace-write",
                ],
            ),
            (
                RcKind::Codex,
                "skip",
                &["--dangerously-bypass-approvals-and-sandbox"],
            ),
            (RcKind::Opencode, "auto", &["--auto"]),
            (RcKind::Opencode, "skip", &["--auto"]),
            (RcKind::Cursor, "auto", &[]),
            (RcKind::Cursor, "skip", &["--force"]),
            (RcKind::Shell, "auto", &[]),
            (RcKind::Shell, "skip", &[]),
        ];
        for (kind, mode, want) in cases {
            assert_eq!(
                perm_flags(kind, mode).as_deref(),
                Some(*want),
                "{}/{mode}",
                kind.as_str()
            );
        }
    }

    #[test]
    fn valid_claude_permission_mode_matches_go() {
        for m in [
            "default",
            "auto",
            "skip",
            "acceptEdits",
            "plan",
            "dontAsk",
            "bypassPermissions",
        ] {
            assert!(valid_claude_permission_mode(m), "{m} should be valid");
        }
        for m in ["", "yolo", "Auto", "bypass"] {
            assert!(!valid_claude_permission_mode(m), "{m} should be invalid");
        }
    }

    #[test]
    fn engine_permission_mode_errors_distinguish_claude_only() {
        assert_eq!(validate_engine_permission_mode(&RcKind::Codex, ""), Ok(()));
        assert_eq!(
            validate_engine_permission_mode(&RcKind::Codex, "auto"),
            Ok(())
        );
        let err = validate_engine_permission_mode(&RcKind::Codex, "plan").unwrap_err();
        assert_eq!(
            err.to_string(),
            "permission mode \"plan\" is claude-only; codex kinds accept default|auto|skip"
        );
        let err = validate_engine_permission_mode(&RcKind::Codex, "yolo").unwrap_err();
        assert_eq!(
            err.to_string(),
            "invalid permission mode \"yolo\" (want default|auto|skip)"
        );
    }

    #[test]
    fn engine_permission_modes_are_claude_aware() {
        assert_eq!(
            engine_permission_modes(&RcKind::Codex),
            vec!["default", "auto", "skip"]
        );
        assert_eq!(
            engine_permission_modes(&RcKind::ClaudeRc),
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

    // ---- auth hints (mirrors Go TestAuthHintFor, rc_test.go:161) ----

    #[test]
    fn auth_hints_match_the_go_table() {
        let cases: &[(RcKind, &str)] = &[
            (RcKind::ClaudeRc, "run `claude` \u{2192} /login"),
            (RcKind::ClaudeBroker, "run `claude` \u{2192} /login"),
            (
                RcKind::Codex,
                "run `codex` and complete login (`codex login`)",
            ),
            (RcKind::Opencode, "run `opencode auth login`"),
            (RcKind::Cursor, "run `cursor-agent login`"),
            (RcKind::Shell, "log in to the agent in a terminal"),
            (
                RcKind::Other("weird".into()),
                "log in to the agent in a terminal",
            ),
        ];
        for (kind, want) in cases {
            assert_eq!(auth_hint_for(kind), *want, "{}", kind.as_str());
        }
    }

    // ---- trust / bypass prompts (mirrors Go TestIsBypassAcceptPrompt, rc_test.go:202) ----

    #[test]
    fn trust_prompt_matchers() {
        for pane in [
            "Error: Workspace not trusted. run claude",
            "Quick safety check: is this a project",
            "  Yes,  I trust this folder  ",
            "yes, i trust this folder",
        ] {
            assert!(is_trust_prompt(pane), "{pane:?} should be a trust prompt");
        }
        for pane in ["", "Bypass Permissions mode", "I trust this folder"] {
            assert!(
                !is_trust_prompt(pane),
                "{pane:?} must not be a trust prompt"
            );
        }
    }

    #[test]
    fn bypass_accept_prompt_requires_both_halves() {
        let yes = "WARNING: Claude Code running in Bypass Permissions mode\n  1. No, exit\n  2. Yes, I accept\n";
        assert!(is_bypass_accept_prompt(yes));
        for pane in [
            "",
            "Workspace not trusted",
            "Bypass Permissions mode", // warning without the accept option
            "2. Yes, I accept",        // accept option without the bypass warning
        ] {
            assert!(!is_bypass_accept_prompt(pane), "false positive on {pane:?}");
        }
    }

    // ---- classifier (mirrors Go TestClassifyPane, rc_test.go:219) ----

    #[test]
    fn classify_pane_table() {
        let cases: &[(&str, RcKind, &str, RcState, &str)] = &[
            ("broker ready+url", RcKind::ClaudeBroker,
             "·✔︎· Connected · my-shed\nhttps://claude.ai/code?environment=env_01ABC",
             RcState::Ready, "https://claude.ai/code?environment=env_01ABC"),
            ("broker reconnecting", RcKind::ClaudeBroker, "·|· Reconnecting · retrying", RcState::Reconnecting, ""),
            ("broker needs-trust", RcKind::ClaudeBroker, "Error: Workspace not trusted. run claude", RcState::NeedsTrust, ""),
            ("broker needs-auth", RcKind::ClaudeBroker, "Remote Control requires a claude.ai subscription.", RcState::NeedsAuth, ""),
            ("broker starting", RcKind::ClaudeBroker, "booting...", RcState::Starting, ""),
            ("rc ready", RcKind::ClaudeRc,
             "/remote-control is active · https://claude.ai/code/session_01RC\nRemote Control active",
             RcState::Ready, "https://claude.ai/code/session_01RC"),
            ("rc connecting", RcKind::ClaudeRc, "❯ /remote-control\n  ⎿  Remote Control connecting…", RcState::Starting, ""),
            ("rc needs-trust quick-check", RcKind::ClaudeRc, "Quick safety check: is this a project", RcState::NeedsTrust, ""),
            ("rc starting", RcKind::ClaudeRc, "❯ Try \"fix typecheck errors\"", RcState::Starting, ""),
            ("rc ignores broker url", RcKind::ClaudeRc, "banner https://claude.ai/code?environment=env_01ABC", RcState::Starting, ""),
            ("shell ready", RcKind::Shell, "charliek@shed:~$ ", RcState::Ready, ""),
            ("shell starting", RcKind::Shell, "   \n  ", RcState::Starting, ""),
        ];
        for (name, kind, pane, want_state, want_url) in cases {
            let c = classify_pane(kind, pane);
            assert_eq!(c.state, *want_state, "case {name}");
            assert_eq!(c.url.as_deref().unwrap_or(""), *want_url, "case {name}");
        }
    }

    // ---- classifier false positives (mirrors Go TestClassifyFalsePositives) ----

    const SHED_PROMPT: &str = "[shed:agent-fixtures] ~ $ ";

    #[test]
    fn shell_prompt_is_ready_for_shell_and_dead_for_agents() {
        assert_eq!(
            classify_pane(&RcKind::Shell, SHED_PROMPT).state,
            RcState::Ready
        );
        for kind in [
            RcKind::Codex,
            RcKind::Opencode,
            RcKind::Cursor,
            RcKind::ClaudeRc,
        ] {
            assert_eq!(
                classify_pane(&kind, &format!("some agent output\n{SHED_PROMPT}")).state,
                RcState::Dead,
                "{}",
                kind.as_str()
            );
        }
    }

    #[test]
    fn empty_pane_is_starting_not_dead() {
        for kind in [
            RcKind::Codex,
            RcKind::Opencode,
            RcKind::Cursor,
            RcKind::ClaudeRc,
            RcKind::Shell,
        ] {
            assert_eq!(
                classify_pane(&kind, "   \n\n").state,
                RcState::Starting,
                "{}",
                kind.as_str()
            );
        }
    }

    #[test]
    fn launch_echo_and_prompt_shaped_output_are_not_deaths() {
        let pane = format!(
            "{SHED_PROMPT}codex\nDo you trust the contents of this directory?\n1. Yes, continue\nPress enter to continue"
        );
        assert_eq!(
            classify_pane(&RcKind::Codex, &pane).state,
            RcState::NeedsTrust
        );

        let pane = ">_ OpenAI Codex (v0.142.4)\nI'll run the tests now:\n[shed:x] ~ $ make test";
        assert_eq!(classify_pane(&RcKind::Codex, pane).state, RcState::Ready);
        assert!(!exited_to_shell("agent output\n[shed:x] ~ $ make test"));
        assert!(exited_to_shell("agent output\n[shed:x] ~ $ "));
    }

    #[test]
    fn codex_ready_wins_over_inline_token_expired() {
        let pane = "MCP startup failed\n\"code\": \"token_expired\"\nProvided authentication token is expired.";
        assert_eq!(
            classify_pane(&RcKind::Codex, pane).state,
            RcState::NeedsAuth
        );
        let pane =
            ">_ OpenAI Codex (v0.142.4)\n\"code\": \"token_expired\"\nMCP startup incomplete";
        assert_eq!(classify_pane(&RcKind::Codex, pane).state, RcState::Ready);
    }

    #[test]
    fn opencode_footer_only_ready_is_auth_guarded() {
        let auth = "  Sign in to opencode to continue\n\n  ctrl+p commands";
        assert_eq!(
            classify_pane(&RcKind::Opencode, auth).state,
            RcState::Starting
        );
        let chat = "  Hello! How can I help you today?\n\n  8.4K (4%)  ctrl+p commands";
        assert_eq!(classify_pane(&RcKind::Opencode, chat).state, RcState::Ready);
        let fresh = "  Ask anything... \"Fix broken tests\"\n  sign in tips\n  ctrl+p commands";
        assert_eq!(
            classify_pane(&RcKind::Opencode, fresh).state,
            RcState::Ready
        );
    }

    #[test]
    fn opencode_connect_a_provider_needs_the_popular_header() {
        let quoted = "  Ask anything... \"Fix broken tests\"\n  I can help you Connect a provider if you'd like.\n  ctrl+p commands";
        assert_eq!(
            classify_pane(&RcKind::Opencode, quoted).state,
            RcState::Ready
        );
        let headline_only =
            "  Some assistant text that happens to say Connect a provider mid-sentence.\n";
        assert_ne!(
            classify_pane(&RcKind::Opencode, headline_only).state,
            RcState::NeedsAuth
        );
        // The conjunction (headline + the lone "Popular" category header) fires.
        let dialog =
            "Connect a provider                    esc\n\n  Popular\n  opencode\n  anthropic\n";
        assert_eq!(
            classify_pane(&RcKind::Opencode, dialog).state,
            RcState::NeedsAuth
        );
    }

    #[test]
    fn cursor_login_splash_is_needs_auth() {
        assert_eq!(
            classify_pane(&RcKind::Cursor, "Cursor Agent\nPress any key to log in...").state,
            RcState::NeedsAuth
        );
    }

    #[test]
    fn unknown_kind_renders_neutrally() {
        let c = classify_pane(
            &RcKind::Other("opencode-hub".into()),
            "banner https://claude.ai/code/session_01RC",
        );
        assert_eq!(c.url, None);
        assert_ne!(c.state, RcState::Dead);
    }

    // ---- fixture-driven classifier sweep (mirrors classify_fixtures_test.go) ----

    /// The pane fixtures live at `crates/fixtures/panes/` — a byte-identical copy
    /// of `internal/ext/rc/testdata/panes/`, kept in lockstep by the Go sweep in
    /// `golden_parity_test.go`. The path is resolved from `CARGO_MANIFEST_DIR` and
    /// stays INSIDE `crates/`, because `make -C desktop core-linux` mounts only
    /// `crates/` — a fixture read from the Go tree could not be found there.
    fn panes_dir() -> PathBuf {
        Path::new(env!("CARGO_MANIFEST_DIR")).join("../fixtures/panes")
    }

    /// The kind a fixture's `<agent>` prefix classifies under. Non-claude agents
    /// are the bare-tool kinds; claude fixtures use `claude-rc` (the classifier is
    /// shared across claude kinds). Mirrors `fixtureAgentKind`
    /// (`classify_fixtures_test.go:13`).
    fn fixture_agent_kind(agent: &str) -> Option<RcKind> {
        match agent {
            "claude" => Some(RcKind::ClaudeRc),
            "codex" => Some(RcKind::Codex),
            "opencode" => Some(RcKind::Opencode),
            "cursor" => Some(RcKind::Cursor),
            _ => None,
        }
    }

    /// Split `<agent>-<state>[-variant]` into the agent prefix and the state its
    /// content must classify to. States are matched longest-first so
    /// `needs-auth`/`needs-trust` win over the bare tokens, and so a `-login`
    /// variant of a state still resolves to that state. Mirrors
    /// `parseFixtureName` (`classify_fixtures_test.go:30`).
    fn parse_fixture_name(name: &str) -> Option<(&str, RcState)> {
        let dash = name.find('-')?;
        let agent = &name[..dash];
        let rest = &name[dash + 1..];
        for (token, state) in [
            ("needs-trust", RcState::NeedsTrust),
            ("needs-auth", RcState::NeedsAuth),
            ("reconnecting", RcState::Reconnecting),
            ("starting", RcState::Starting),
            ("ready", RcState::Ready),
            ("dead", RcState::Dead),
        ] {
            if rest == token || rest.starts_with(&format!("{token}-")) {
                return Some((agent, state));
            }
        }
        None
    }

    #[test]
    fn every_pane_fixture_classifies_to_its_filename_state() {
        let dir = panes_dir();
        let mut seen = 0usize;
        let mut entries: Vec<_> = std::fs::read_dir(&dir)
            .unwrap_or_else(|e| panic!("reading {}: {e}", dir.display()))
            .map(|e| e.unwrap().path())
            .filter(|p| p.extension().is_some_and(|x| x == "txt"))
            .collect();
        entries.sort();
        assert!(
            !entries.is_empty(),
            "no pane fixtures found in {}",
            dir.display()
        );

        for path in entries {
            let base = path.file_name().unwrap().to_string_lossy().to_string();
            if base == "SUMMARY.txt" {
                continue;
            }
            let name = base.trim_end_matches(".txt");
            let (agent, want) = parse_fixture_name(name).unwrap_or_else(|| {
                panic!("{base}: filename does not encode a known <agent>-<state>")
            });
            let kind = fixture_agent_kind(agent)
                .unwrap_or_else(|| panic!("{base}: unknown agent prefix {agent:?}"));
            let data = std::fs::read_to_string(&path).unwrap();
            let c = classify_pane(&kind, &data);
            // The drift this guards: a fixture falling through to the `starting`
            // default is what a broken anchor looks like after a TUI
            // redraw/rebrand.
            assert_eq!(
                c.state, want,
                "{base}: classified wrong (a `starting` result means a broken anchor)"
            );
            // url/id stay claude-remote-control-specific — never leak for other agents.
            if agent != "claude" {
                assert_eq!(c.url, None, "{base}: non-claude fixture leaked a url");
            }
            seen += 1;
        }
        assert!(seen > 0, "no state fixtures were exercised");
    }

    // ---- slugs (mirrors Go TestGenSlug, rc_test.go:37) ----

    #[test]
    fn gen_slug_alphabet_and_length() {
        let alphabet: HashSet<char> = SLUG_ALPHABET.iter().map(|b| *b as char).collect();
        let mut produced = HashSet::new();
        for _ in 0..200 {
            let s = gen_slug();
            assert_eq!(s.chars().count(), SLUG_LEN, "slug {s:?} wrong length");
            for c in s.chars() {
                assert!(
                    alphabet.contains(&c),
                    "slug {s:?} has a confusable/invalid char {c:?}"
                );
            }
            assert!(
                valid_caller_slug(&s),
                "generated slug {s:?} fails the caller grammar"
            );
            produced.insert(s);
        }
        // Sanity that the entropy source is actually varying (a constant slug
        // would pass every assertion above).
        assert!(
            produced.len() > 190,
            "slugs are not varying: {} unique",
            produced.len()
        );
    }

    #[test]
    fn caller_slug_grammar() {
        for ok in ["a", "abc234", "a-b", "a1-b2-c3", &"a".repeat(32)] {
            assert!(valid_caller_slug(ok), "{ok:?} should be valid");
        }
        for bad in [
            "",
            "-abc",
            "abc-",
            "AB",
            "a_b",
            "a b",
            &"a".repeat(33),
            "abc/def",
        ] {
            assert!(!valid_caller_slug(bad), "{bad:?} should be invalid");
        }
    }

    // ---- metadata (mirrors Go TestBuildEnvArgs*, rc_test.go:295-405) ----

    fn meta(kind: RcKind) -> RcMetadata {
        RcMetadata {
            id: "id-1".into(),
            display_name: "x".into(),
            kind,
            workdir: "/home/shed".into(),
            created_by: "shed-ext-rc/1".into(),
            created_at: "2026-06-19T18:53:00Z".into(),
            target: String::new(),
            slug: "abc234".into(),
            port: 0,
        }
    }

    /// **The ordering pin.** `tmux show-environment` re-orders its dump, so a
    /// differential harness can never observe this; it is pinned here (and by the
    /// matching Go test) instead.
    #[test]
    fn build_env_args_ordering_is_exact() {
        let mut m = meta(RcKind::ClaudeRc);
        m.display_name = "Friday Bug Fix".into();
        m.target = "shed:t1@host".into();
        assert_eq!(
            build_env_args(&m).unwrap(),
            vec![
                "-e",
                "SHED_RC_V=2",
                "-e",
                "SHED_RC_ID=id-1",
                "-e",
                "SHED_RC_DISPLAY_NAME=Friday Bug Fix",
                "-e",
                "SHED_RC_KIND=claude-rc",
                "-e",
                "SHED_RC_WORKDIR=/home/shed",
                "-e",
                "SHED_RC_CREATED_BY=shed-ext-rc/1",
                "-e",
                "SHED_RC_CREATED_AT=2026-06-19T18:53:00Z",
                "-e",
                "SHED_RC_SLUG=abc234",
                "-e",
                "SHED_RC_TARGET=shed:t1@host",
            ]
        );
        // Target omitted entirely when empty (not stamped as an empty value).
        let m = meta(RcKind::ClaudeRc);
        let args = build_env_args(&m).unwrap();
        assert!(!args.iter().any(|a| a.starts_with("SHED_RC_TARGET")));
        assert_eq!(args.len(), 16);
    }

    #[test]
    fn build_env_args_stamps_slug_for_every_kind() {
        assert_eq!(
            SCHEMA_VERSION, 2,
            "SHED_RC_SLUG is ADDITIVE and must not bump the schema"
        );
        for kind in all_kinds() {
            let args = build_env_args(&meta(kind.clone())).unwrap();
            assert!(
                args.iter().any(|a| a == "SHED_RC_SLUG=abc234"),
                "{}: args {args:?}",
                kind.as_str()
            );
        }
        let mut m = meta(RcKind::Shell);
        m.slug = "a\nb".into();
        assert_eq!(
            build_env_args(&m),
            Err(RcAgentError::ControlChars {
                key: "SHED_RC_SLUG".into()
            })
        );
    }

    #[test]
    fn build_env_args_rejects_control_chars() {
        let mut m = meta(RcKind::Shell);
        m.display_name = "a\nb".into();
        assert_eq!(
            build_env_args(&m).unwrap_err().to_string(),
            "SHED_RC_DISPLAY_NAME must not contain control characters"
        );
    }

    #[test]
    fn build_env_args_opencode_port_and_password() {
        // Port allocated: both SHED_RC_OPENCODE_PORT and the password override.
        let mut with_port = meta(RcKind::Opencode);
        with_port.port = 4096;
        let args = build_env_args(&with_port).unwrap();
        assert!(args.iter().any(|a| a == "SHED_RC_OPENCODE_PORT=4096"));
        assert!(args.iter().any(|a| a == "OPENCODE_SERVER_PASSWORD="));
        // The password pair is LAST, after every SHED_RC_ pair.
        assert_eq!(args.last().unwrap(), "OPENCODE_SERVER_PASSWORD=");

        // Port == 0 (allocation failed, non-fatal): no port key, password still set.
        let args = build_env_args(&meta(RcKind::Opencode)).unwrap();
        assert!(!args.iter().any(|a| a.starts_with("SHED_RC_OPENCODE_PORT=")));
        assert!(args.iter().any(|a| a == "OPENCODE_SERVER_PASSWORD="));

        // Non-opencode kind: neither key, even with a nonzero port.
        let mut non_oc = meta(RcKind::Shell);
        non_oc.port = 4096;
        let args = build_env_args(&non_oc).unwrap();
        assert!(!args.iter().any(|a| a.starts_with("SHED_RC_OPENCODE_PORT=")));
        assert!(!args
            .iter()
            .any(|a| a.starts_with("OPENCODE_SERVER_PASSWORD")));
    }

    #[test]
    fn has_control_chars_matches_the_go_class() {
        assert!(has_control_chars("a\nb"));
        assert!(has_control_chars("a\tb"));
        assert!(has_control_chars("a\u{7f}b"));
        assert!(!has_control_chars("plain value"));
        // C1 (0x80-0x9f) is NOT a control char to the Go engine, unlike the
        // client-side is_safe_rc_value. Documented divergence, pinned here.
        assert!(!has_control_chars("a\u{9b}b"));
        assert!(!crate::rc::is_safe_rc_value("a\u{9b}b"));
    }

    // ---- parse_env / parse_session (mirrors Go rc_test.go:322-457) ----

    #[test]
    fn parse_env_skips_removal_lines_and_foreign_keys() {
        let dump = "SHED_RC_V=2\n-SHED_RC_TARGET\nOPENCODE_SERVER_PASSWORD=\nPATH=/usr/bin\nSHED_RC_KIND=codex\nSHED_RC_DISPLAY_NAME=a=b\n";
        let env = parse_env(dump);
        assert_eq!(env.get("SHED_RC_V").map(String::as_str), Some("2"));
        assert_eq!(env.get("SHED_RC_KIND").map(String::as_str), Some("codex"));
        // Everything after the FIRST '=' is the value.
        assert_eq!(
            env.get("SHED_RC_DISPLAY_NAME").map(String::as_str),
            Some("a=b")
        );
        assert!(
            !env.contains_key("SHED_RC_TARGET"),
            "bare -KEY removal must be skipped"
        );
        assert!(!env.contains_key("OPENCODE_SERVER_PASSWORD"));
        assert!(!env.contains_key("PATH"));
    }

    #[test]
    fn build_env_args_round_trips_through_parse_session() {
        let m = RcMetadata {
            id: "id-1".into(),
            display_name: "Friday Bug Fix".into(),
            kind: RcKind::ClaudeRc,
            workdir: "/home/shed/proj".into(),
            created_by: "shed-ext-rc/0.5.0".into(),
            created_at: "2026-06-19T18:53:00Z".into(),
            target: "shed:t1@host".into(),
            slug: "abc".into(),
            port: 0,
        };
        let args = build_env_args(&m).unwrap();
        let dump: String = args
            .chunks(2)
            .filter(|c| c[0] == "-e")
            .map(|c| format!("{}\n", c[1]))
            .collect();
        let s = parse_session(&tmux_name("abc"), &dump, "", None);
        assert!(s.managed);
        assert_eq!(s.kind, RcKind::ClaudeRc);
        assert_eq!(s.display_name.as_deref(), Some("Friday Bug Fix"));
        assert_eq!(s.workdir.as_deref(), Some("/home/shed/proj"));
        assert_eq!(s.id.as_deref(), Some("id-1"));
        assert_eq!(s.created_by.as_deref(), Some("shed-ext-rc/0.5.0"));
        assert_eq!(s.created_at.as_deref(), Some("2026-06-19T18:53:00Z"));
        assert_eq!(s.target_label.as_deref(), Some("shed:t1@host"));
        assert_eq!(s.slug, "abc");
        assert_eq!(s.tmux_session, "rc-abc");
    }

    #[test]
    fn parse_session_legacy_rows_are_forced_broker_and_unmanaged() {
        let fallback = |slug: &str| format!("fb/{slug}");
        let s = parse_session(
            "rc-legacy",
            "SHED_RC_KIND=shell\nSHED_RC_DISPLAY_NAME=spoof",
            "charliek@shed:~$ ",
            Some(&fallback),
        );
        assert!(!s.managed);
        assert_eq!(s.kind, RcKind::ClaudeBroker);
        // Stored values on an unmanaged row are IGNORED, including the name.
        assert_eq!(s.display_name.as_deref(), Some("fb/legacy"));
        assert_eq!(s.workdir, None);
        assert_eq!(s.id, None);
        assert_eq!(s.created_by, None);
        assert_eq!(s.created_at, None);
        assert_eq!(s.target_label, None);

        // A v1 session is below the managed floor — no aliasing.
        let s = parse_session("rc-old", "SHED_RC_V=1\nSHED_RC_KIND=claude-rc", "", None);
        assert!(!s.managed);
        assert_eq!(s.kind, RcKind::ClaudeBroker);
    }

    #[test]
    fn parse_session_preserves_unknown_kinds_verbatim() {
        let s = parse_session(
            "rc-fut001",
            "SHED_RC_V=2\nSHED_RC_KIND=some-future-kind",
            "",
            None,
        );
        assert!(s.managed);
        assert_eq!(s.kind, RcKind::Other("some-future-kind".into()));
        // Neutral render: no claude affordances.
        assert_eq!(s.url, None);
    }

    #[test]
    fn parse_session_always_carries_a_lane() {
        for (tmux, dump) in [
            ("rc-abc234", "SHED_RC_V=2\nSHED_RC_KIND=codex"),
            ("rc-legacy", "SHED_RC_KIND=shell"),
            ("rc-fut001", "SHED_RC_V=2\nSHED_RC_KIND=some-future-kind"),
        ] {
            let s = parse_session(tmux, dump, "", None);
            assert_eq!(s.lane.as_deref(), Some(LANE_TUI), "{tmux}");
        }
    }

    #[test]
    fn parse_session_omits_display_name_without_a_fallback() {
        let s = parse_session(
            "rc-brk900",
            "SHED_RC_V=2\nSHED_RC_KIND=claude-broker",
            "",
            None,
        );
        assert_eq!(s.display_name, None);
    }

    #[test]
    fn parse_session_drops_a_non_rfc3339_created_at() {
        for (raw, want) in [
            ("2026-06-19T18:53:00Z", Some("2026-06-19T18:53:00Z")),
            (
                "2026-06-19T18:53:00.123456Z",
                Some("2026-06-19T18:53:00.123456Z"),
            ),
            ("2026-06-19T18:53:00+02:00", None),
            ("2026-06-19 18:53:00Z", None),
            ("yesterday", None),
            ("", None),
        ] {
            let s = parse_session(
                "rc-abc234",
                &format!("SHED_RC_V=2\nSHED_RC_KIND=codex\nSHED_RC_CREATED_AT={raw}"),
                "",
                None,
            );
            assert_eq!(s.created_at.as_deref(), want, "created_at {raw:?}");
        }
    }

    #[test]
    fn is_managed_version_matches_go() {
        for v in ["2", "3", "10"] {
            assert!(is_managed_version(v), "{v} should be managed");
        }
        for v in ["1", "0", "", "1.0", "1e3", "0x1", " 2", "+2", "abc"] {
            assert!(!is_managed_version(v), "{v} should be unmanaged");
        }
    }

    // ---- prompt anchors (registry data) ----

    #[test]
    fn prompt_anchors_are_declared_for_every_registered_kind() {
        for kind in all_kinds() {
            assert!(prompt_anchor_for(&kind).is_some(), "{}", kind.as_str());
        }
        assert!(prompt_anchor_for(&RcKind::Other("future".into())).is_none());
        assert!(prompt_anchor_for(&RcKind::Codex)
            .unwrap()
            .is_match("  › Find and fix a bug in @filename"));
        assert!(prompt_anchor_for(&RcKind::Shell)
            .unwrap()
            .is_match("banner\n[shed:x] ~ $ \n"));
        assert!(prompt_anchor_for(&RcKind::ClaudeRc)
            .unwrap()
            .is_match("? for shortcuts · <- for agents"));
    }

    #[test]
    fn tool_binaries_match_the_registry() {
        assert_eq!(
            tool_for(&RcKind::Cursor).unwrap().bin(),
            Some("cursor-agent")
        );
        assert_eq!(tool_for(&RcKind::Shell).unwrap().bin(), None);
        assert_eq!(tool_for(&RcKind::ClaudeBroker).unwrap().as_str(), "claude");
        assert!(tool_for(&RcKind::Other("x".into())).is_none());
    }

    /// The registry's tool axis and the client's [`RcKind::tool`] are one table
    /// seen from two sides: the client's answer is the registry's, minus the rows
    /// with nothing to probe. A kind added to only one of them would gate the
    /// create form on a different agent set than the engine launches from — this
    /// is the drift guard that makes the two tables' co-existence safe.
    #[test]
    fn client_tool_axis_is_the_registry_minus_unprobeable_rows() {
        for kind in all_kinds()
            .into_iter()
            .chain([RcKind::Other("future".into())])
        {
            assert_eq!(
                kind.tool(),
                tool_for(&kind)
                    .filter(|t| t.bin().is_some())
                    .map(|t| t.as_str()),
                "{}",
                kind.as_str()
            );
        }
    }
}
