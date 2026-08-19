//! `sx agent <tool>` and `sx plan <file>` — the two kickoff verbs.
//!
//! Both funnel into ONE pure planner ([`plan_kickoff`]) that applies the plan 009
//! §3.2 dispatch table, and one executor ([`execute`]) that runs the resulting
//! [`Kickoff`] either in-process (local) or over SSH (machine/shed). Keeping the
//! decisions in a pure function is what makes the posture matrix — which target
//! gets `--interactive-shell`, which tool gets a default permission mode, when
//! `--no-wait` is legal — a table-driven unit test instead of a live experiment.
//!
//! `sx agent` is the generalization of the Go `shed-machine-rc claude` verb (plan
//! 009 §0: absorbed, not ported): same wait-by-default posture, same
//! `<shorthost>/<slug>` display name, same `--permission-mode auto` default *for
//! claude*, now for every tool and every target.

use base64::Engine as _;

use shed_app::rc_engine::ops::CreateOptions;
use shed_core::rc::{
    self, CreatePayload, CreateSpec, RcKind, RcSessionDto, RcState, DEFAULT_RC_PERMISSION_MODE,
    PERM_MODE_SKIP,
};
use shed_core::rc_agents;

use crate::args::Parsed;
use crate::cli::{Deps, DEFAULT_CREATED_BY};
use crate::porcelain::{remote_exec, remote_prefix, resolve_target, VerbError, VerbResult};
use crate::target::{Resolved, Target};

/// What rides on the remote create's stdin (the owned twin of
/// [`shed_core::rc::CreatePayload`], which borrows).
#[derive(Debug, Clone, PartialEq, Eq, Default)]
pub enum Payload {
    #[default]
    None,
    Prompt(String),
    Plan {
        text: String,
        framing_b64: Option<String>,
    },
}

/// A fully-decided create, ready to run anywhere.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Kickoff {
    pub kind: RcKind,
    pub slug: String,
    pub display_name: String,
    pub workdir: String,
    pub created_by: String,
    pub target_label: String,
    /// `""` means "no posture flag" — the tool's own default.
    pub permission_mode: String,
    pub wait: bool,
    pub interactive_shell: bool,
    pub payload: Payload,
}

/// The planner's inputs: the raw flags plus the two pieces of ambient state the
/// decisions depend on (the target and this machine's hostname).
pub struct Request<'a> {
    pub kind: RcKind,
    pub target: &'a Target,
    /// `--permission-mode` verbatim (empty when absent).
    pub permission_mode: &'a str,
    pub skip: bool,
    /// The posture applied when neither flag is given. `sx agent` passes `auto`
    /// only for a claude kind (the absorbed verb's parity); `sx plan` passes
    /// `auto` for every kind (parity with the Go `shed plan`).
    pub default_permission_mode: &'a str,
    pub name: &'a str,
    pub slug: &'a str,
    pub workdir: &'a str,
    pub no_wait: bool,
    /// `-p`/`--prompt`: a kickoff line on its own, or the framing prepended to a
    /// plan's kickoff when `plan` is also set.
    pub prompt: &'a str,
    pub plan: Option<String>,
    pub hostname: &'a str,
    pub created_by: &'a str,
}

/// Apply the dispatch table. Pure: no config, no clock, no process.
pub fn plan_kickoff(req: &Request) -> Result<Kickoff, VerbError> {
    // --skip and --permission-mode are mutually exclusive (resolveMode parity).
    if req.skip && !req.permission_mode.is_empty() {
        return Err(VerbError::bad_args(
            "--skip and --permission-mode are mutually exclusive",
        ));
    }
    let permission_mode = if req.skip {
        PERM_MODE_SKIP.to_string()
    } else if !req.permission_mode.is_empty() {
        req.permission_mode.to_string()
    } else {
        req.default_permission_mode.to_string()
    };
    // Reject an invalid posture HERE, before a slug is minted or a session is
    // created — the same "no side effect before validation" order the engine and
    // mobile both keep. (An unsupported mode for the kind is dropped, not an
    // error; that is `validate_permission_mode`'s rule, applied at build time.)
    rc::validate_permission_mode(
        &req.kind,
        Some(permission_mode.as_str()).filter(|m| !m.is_empty()),
    )
    .map_err(|e| VerbError::bad_args(e.to_string()))?;

    let has_kickoff = !req.prompt.is_empty() || req.plan.is_some();
    if req.no_wait && has_kickoff {
        // The engine waits on `Wait || Prompt != ""`: a kickoff is delivered only
        // AFTER the pane reaches ready, so "don't wait" and "deliver this" are a
        // contradiction rather than a combination (plan 009 §3.2).
        return Err(VerbError::bad_args(
            "--no-wait cannot be combined with -p/--plan: a kickoff is delivered \
             only after the session is ready",
        ));
    }

    let slug = if req.slug.is_empty() {
        rc_agents::gen_slug()
    } else {
        if !rc_agents::valid_caller_slug(req.slug) {
            return Err(VerbError::bad_args(format!("invalid slug {:?}", req.slug)));
        }
        req.slug.to_string()
    };

    // The display name: `<shorthost>/<slug>` on this machine and on a native
    // machine (Go's `claude` verb), the bare slug in a shed — where the host name
    // is the shed's own and the SERVER already renders `<shed>/<slug>`, so a
    // second prefix would read as `web/web/abc234`.
    let display_name = if !req.name.is_empty() {
        req.name.to_string()
    } else if matches!(req.target, Target::Shed { .. }) || req.hostname.is_empty() {
        slug.clone()
    } else {
        format!("{}/{}", req.hostname, slug)
    };

    let payload = match (&req.plan, req.prompt) {
        (Some(text), framing) => Payload::Plan {
            text: text.clone(),
            framing_b64: (!framing.is_empty())
                .then(|| base64::engine::general_purpose::STANDARD.encode(framing)),
        },
        (None, "") => Payload::None,
        (None, prompt) => Payload::Prompt(prompt.to_string()),
    };

    Ok(Kickoff {
        kind: req.kind.clone(),
        slug,
        display_name,
        workdir: req.workdir.to_string(),
        created_by: req.created_by.to_string(),
        target_label: req.target.label(),
        permission_mode,
        wait: !req.no_wait,
        interactive_shell: req.target.interactive_shell(),
        payload,
    })
}

impl Kickoff {
    /// The engine's own options struct (the LOCAL path).
    pub fn create_options(&self) -> CreateOptions {
        let (prompt, plan, plan_framing) = match &self.payload {
            Payload::None => (String::new(), String::new(), String::new()),
            Payload::Prompt(p) => (p.clone(), String::new(), String::new()),
            Payload::Plan { text, framing_b64 } => (
                String::new(),
                text.clone(),
                // The local engine takes the framing as PLAIN text — the base64
                // exists only because the remote path carries it through an argv.
                framing_b64
                    .as_deref()
                    .and_then(decode_framing)
                    .unwrap_or_default(),
            ),
        };
        CreateOptions {
            kind: self.kind.clone(),
            display_name: self.display_name.clone(),
            slug: self.slug.clone(),
            workdir: self.workdir.clone(),
            created_by: self.created_by.clone(),
            target: self.target_label.clone(),
            prompt,
            plan,
            plan_framing,
            wait: self.wait,
            interactive_shell: self.interactive_shell,
            permission_mode: self.permission_mode.clone(),
        }
    }

    /// The remote `create` argv + stdin (the MACHINE/SHED path).
    pub fn remote_invocation(&self, bin: &str) -> Result<(Vec<String>, Option<String>), VerbError> {
        let payload = match &self.payload {
            Payload::None => CreatePayload::None,
            Payload::Prompt(p) => CreatePayload::Prompt(p),
            Payload::Plan { text, framing_b64 } => CreatePayload::Plan {
                text,
                framing_b64: framing_b64.as_deref(),
            },
        };
        rc::create_invocation_v2(&CreateSpec {
            bin,
            kind: &self.kind,
            name: &self.display_name,
            slug: &self.slug,
            workdir: Some(self.workdir.as_str()).filter(|w| !w.is_empty()),
            created_by: &self.created_by,
            target: &self.target_label,
            permission_mode: Some(self.permission_mode.as_str()).filter(|m| !m.is_empty()),
            wait: self.wait,
            interactive_shell: self.interactive_shell,
            payload,
        })
        .map_err(|e| VerbError::bad_args(e.to_string()))
    }
}

fn decode_framing(b64: &str) -> Option<String> {
    base64::engine::general_purpose::STANDARD
        .decode(b64)
        .ok()
        .and_then(|raw| String::from_utf8(raw).ok())
}

/// Run a planned kickoff on its target.
pub fn execute(
    deps: &Deps,
    resolved: &Resolved,
    kickoff: &Kickoff,
) -> Result<RcSessionDto, VerbError> {
    match resolved {
        Resolved::Local => Ok(deps
            .engine(kickoff.interactive_shell)
            .create(kickoff.create_options())?),
        remote => {
            let prefix = remote_prefix(deps, remote)?;
            let (argv, stdin) = kickoff.remote_invocation(prefix.bin())?;
            let stdout = remote_exec(deps, remote, &prefix.splice(argv), stdin)?;
            rc::decode_session(&stdout).map_err(|e| VerbError::failed(e.to_string()))
        }
    }
}

// ---------------------------------------------------------------------------
// the verbs
// ---------------------------------------------------------------------------

/// The flags the two kickoff verbs read IDENTICALLY, in one place. Each verb
/// then overrides only the axes the dispatch table gives it a rule of its own
/// (`default_permission_mode`, `no_wait`, `plan`) — so what is left spelled out
/// at a call site is exactly what makes that verb different.
fn shared_request<'a>(
    p: &'a Parsed,
    kind: RcKind,
    target: &'a Target,
    hostname: &'a str,
) -> Request<'a> {
    Request {
        kind,
        target,
        permission_mode: p.value("permission-mode"),
        skip: p.flag("skip"),
        default_permission_mode: "",
        name: p.value("name"),
        slug: p.value("slug"),
        workdir: p.value("workdir"),
        no_wait: false,
        prompt: prompt_of(p),
        plan: None,
        hostname,
        created_by: DEFAULT_CREATED_BY,
    }
}

/// `sx agent <tool> [flags]`.
pub fn agent(deps: &Deps, tool: &str, p: &Parsed) -> VerbResult {
    let kind = kind_for_tool(tool)?;
    let resolved = resolve_target(deps, p)?;
    let (target, hostname) = (resolved.target(), deps.hostname());
    let kickoff = plan_kickoff(&Request {
        // `claude` keeps the absorbed verb's autonomous default; every other tool
        // keeps ITS own default (no posture flag at all), because "auto" means
        // something different in each agent's CLI and inventing one for codex or
        // cursor would silently change what the tool does.
        default_permission_mode: if kind.runs_claude() {
            DEFAULT_RC_PERMISSION_MODE
        } else {
            ""
        },
        no_wait: p.flag("no-wait"),
        plan: read_plan(p.value("plan"))?,
        ..shared_request(p, kind, &target, &hostname)
    })?;
    let session = execute(deps, &resolved, &kickoff)?;
    report(deps, &resolved, &kickoff, &session, p.flag("json"))
}

/// `sx plan <file> [flags]` — ship a plan document to a fresh session.
pub fn plan(deps: &Deps, file: &str, p: &Parsed) -> VerbResult {
    let tool = match p.value("tool") {
        "" => "claude",
        given => given,
    };
    let kind = kind_for_tool(tool)?;
    if !kind.accepts_typed_input() {
        // `shed plan`'s own gate (`cmd/shed/plan.go:77-79`), applied before any
        // side effect rather than after a session exists.
        return Err(VerbError::bad_args(format!(
            "--tool {tool} does not accept a plan (it is driven from elsewhere)"
        )));
    }
    let resolved = resolve_target(deps, p)?;
    // The subject is non-empty (the dispatcher guarantees it), so `read_plan`
    // always yields a document here.
    let text = read_plan(file)?.ok_or_else(|| VerbError::bad_args("a plan file is required"))?;
    let (target, hostname) = (resolved.target(), deps.hostname());
    let kickoff = plan_kickoff(&Request {
        // Parity with `shed plan`, which defaults to `auto` for every kind.
        default_permission_mode: DEFAULT_RC_PERMISSION_MODE,
        // A plan is a kickoff: `sx plan` always waits, and offers no --no-wait.
        no_wait: false,
        plan: Some(text),
        ..shared_request(p, kind, &target, &hostname)
    })?;
    let session = execute(deps, &resolved, &kickoff)?;
    report(deps, &resolved, &kickoff, &session, p.flag("json"))
}

/// `-p` and `--prompt` are the same flag; the short form wins when both appear
/// (last-wins is already the parser's rule WITHIN a spelling).
fn prompt_of(p: &Parsed) -> &str {
    match p.value("p") {
        "" => p.value("prompt"),
        short => short,
    }
}

/// Map a `sx agent <tool>` word to an RC kind.
pub fn kind_for_tool(tool: &str) -> Result<RcKind, VerbError> {
    match tool {
        // The tool is `claude`; the KIND is `claude-rc` (`claude-broker` is the
        // other, browser-driven claude kind and is not creatable from here).
        "claude" | "claude-rc" => Ok(RcKind::ClaudeRc),
        "codex" => Ok(RcKind::Codex),
        "cursor" => Ok(RcKind::Cursor),
        "opencode" => Ok(RcKind::Opencode),
        "shell" => Ok(RcKind::Shell),
        other => Err(VerbError::bad_args(format!(
            "unknown tool {other:?}: expected claude, codex, cursor, opencode, or shell"
        ))),
    }
}

/// Read a plan file (absent path → `None`). The CONTENT gates — size, UTF-8 —
/// are the engine's ([`shed_app::rc_engine::plan::plan_from_bytes`]), applied
/// identically on every target because the file is read here, not remotely.
fn read_plan(path: &str) -> Result<Option<String>, VerbError> {
    if path.is_empty() {
        return Ok(None);
    }
    let raw = std::fs::read(path)
        .map_err(|e| VerbError::bad_args(format!("reading plan {path}: {e}")))?;
    if raw.len() > shed_app::rc_engine::plan::PLAN_MAX_BYTES {
        return Err(VerbError::bad_args(format!(
            "plan {path} exceeds {} bytes",
            shed_app::rc_engine::plan::PLAN_MAX_BYTES
        )));
    }
    if raw.is_empty() {
        return Err(VerbError::bad_args(format!("plan {path} is empty")));
    }
    shed_app::rc_engine::plan::plan_from_bytes(&raw)
        .map(Some)
        .map_err(|_| VerbError::bad_args(format!("plan {path} is not valid UTF-8")))
}

/// Print the kickoff outcome and pick the exit code.
///
/// Exit is non-zero when a WAITING create did not reach a usable state (the Go
/// `claude` verb's contract: a script can tell "ready" from "needs auth / still
/// starting"), and always zero for `--no-wait`, where "not ready yet" is the
/// point rather than a failure.
fn report(
    deps: &Deps,
    resolved: &Resolved,
    kickoff: &Kickoff,
    session: &RcSessionDto,
    json: bool,
) -> VerbResult {
    if json {
        let encoded = serde_json::to_string(session)
            .map_err(|e| VerbError::failed(format!("encoding output: {e}")))?;
        deps.write_out(&format!("{encoded}\n"));
        return Ok(exit_for(kickoff, session));
    }
    // One sentence, shaped like the Go `claude` verb's: what started, where, and
    // — loudly — under what permission posture.
    let posture = if kickoff.permission_mode.is_empty() {
        "the tool's own permission posture".to_string()
    } else {
        format!(
            "permission-mode={} (tools run UNATTENDED)",
            kickoff.permission_mode
        )
    };
    deps.write_out(&format!(
        "Started {} session {:?} on {} — {posture}.\n",
        session.kind.as_str(),
        session.slug,
        resolved.display()
    ));

    let on = match resolved {
        Resolved::Local => String::new(),
        other => format!(" --on {}", other.display()),
    };
    match session.state {
        RcState::Ready => {
            if let Some(url) = session.url.as_deref().filter(|u| !u.is_empty()) {
                deps.write_out(&format!("  Watch/steer from a browser: {url}\n"));
            }
        }
        RcState::NeedsAuth => deps.write_out(&format!(
            "  {} is not logged in there — authenticate once, then retry.\n",
            session.kind.auth_hint()
        )),
        other if kickoff.wait => deps.write_out(&format!(
            "  State: {} (no URL yet — `sx watch {}{on}` to follow it).\n",
            other.as_str(),
            session.slug
        )),
        _ => {}
    }
    deps.write_out(&format!(
        "  Watch:  sx watch {}{on}\n  Attach: sx attach {}{on}\n",
        session.slug, session.slug
    ));
    Ok(exit_for(kickoff, session))
}

fn exit_for(kickoff: &Kickoff, session: &RcSessionDto) -> i32 {
    if kickoff.wait && session.state != RcState::Ready {
        1
    } else {
        0
    }
}
