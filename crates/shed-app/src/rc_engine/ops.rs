//! The one-shot verbs — a port of `internal/ext/rc/ops.go`.
//!
//! `create`, `list`, `probe`, `prompt`, `kill`, `accept-trust` and the `--wait`
//! readiness poller, over the [`Tmux`] seam and a handful of injected process
//! seams ([`Engine`]'s builder). Everything pure (the registry, classifiers,
//! `SHED_RC_*` metadata, DTO shapes) comes from [`shed_core::rc_agents`].

use std::process::{Command, Stdio};
use std::time::{Duration, Instant};

use shed_core::rc::{tmux_name, RcKind, RcSessionDto, RcSessionListDto, RcState};
use shed_core::rc_agents::{
    build_env_args, classify_pane, gen_slug, inner_command, is_bypass_accept_prompt,
    is_trust_prompt, lane_for_kind, parse_session, perm_flags, shell_quote_always, tool_for,
    valid_caller_slug, validate_engine_permission_mode, RcMetadata, PERMISSION_MODE_BYPASS,
};

use super::plan::{compose_plan_kickoff, validate_plan_inputs, write_plan};
use super::text::{has_unsafe_prompt_chars, normalize_newlines, quote_go};
use super::tmux::{is_duplicate_session, is_missing_session, Tmux, TmuxRunner};
use crate::traits::{system_clock, ClockRef};

// ---------------------------------------------------------------------------
// tunables (ops.go:25-31, clirc.go:403)
// ---------------------------------------------------------------------------

/// How long `--wait` polls for a terminal state (`defaultWaitTimeout`,
/// `ops.go:26`).
pub const DEFAULT_WAIT_TIMEOUT: Duration = Duration::from_secs(20);

/// The poll cadence inside that window (`defaultPollEvery`, `ops.go:27`).
pub const DEFAULT_POLL_EVERY: Duration = Duration::from_millis(750);

/// The extra settle between "the pane says ready" and typing the kickoff
/// (`promptDeliverSettle`, `ops.go:30`). A session can report ready — URL
/// present — a beat before its REPL accepts input.
pub const PROMPT_DELIVER_SETTLE: Duration = Duration::from_secs(1);

/// Per-probe budget for the installed-agent gate (`agentProbeTimeout`,
/// `clirc.go:403`), so an unresponsive agent binary cannot stall a create.
const AGENT_PROBE_TIMEOUT: Duration = Duration::from_secs(2);

/// The PROCESS-environment kill-switch for the best-effort hub ensure
/// (`rc.EnvNoHub`, `hub.go:79`, added by plan 009's C2 oracle seam).
///
/// Any non-empty value makes create skip the hub side effect entirely. It exists
/// so a hermetic harness (the Go↔Rust rc parity suite) does not have every
/// `create` spawn a detached daemon on the fixed loopback port. **The literal is
/// cross-implementation contract** — the Go binary reads the same variable, so
/// one switch neutralizes both sides symmetrically. Duplicated here rather than
/// imported because the hub itself is not ported (plan 009 §3.1).
pub const ENV_NO_HUB: &str = "SHED_RC_NO_HUB";

/// The engine's LAST-RESORT `created_by` when a caller supplies none
/// (`rc.ToolName`, `rc.go:107`, consumed at `ops.go:165`).
///
/// Deliberately the Go guest binary's token even here, because it is a pure
/// port-fidelity value: every real CLI supplies its own provenance
/// (`clirc.go:307` always passes `cfg.DefaultCreatedBy` — the bare prog name, so
/// `shed-ext-rc` / `shed-machine-rc` / `sx`), so this fallback is only ever seen
/// by a library caller that forgot one, and a differential harness must see the
/// same string on both sides if it ever is. NOT to be confused with
/// [`shed_core::rc::TOOL_NAME`], which is the CLIENT's (`shed-desktop`) token.
pub const DEFAULT_CREATED_BY: &str = "shed-ext-rc";

// ---------------------------------------------------------------------------
// errors (ops.go:13-23, clirc.go:153-166)
// ---------------------------------------------------------------------------

/// A create/list/probe/prompt/kill/accept-trust failure, carrying its **exit-code
/// class**.
///
/// Go models these as sentinel errors wrapped with `%w`, which is what puts the
/// `invalid arguments: ` / `rc session already exists: ` / `rc session not
/// found: ` prefix on stderr, and `clirc.go:153-166` maps the sentinel to the
/// process exit code. Both halves are wire contract (the orchestrator maps exit 3
/// to `409 RC_SLUG_TAKEN`, exit 4 to a gone session, exit 2 to a bad request),
/// so the classes AND their message prefixes are reproduced exactly here — the
/// parity harness diffs the masked stderr for these classes.
///
/// Note [`shed_core::rc_agents::RcAgentError`]'s doc: the kernel's messages are
/// the UNWRAPPED Go text on purpose, and adding the `invalid arguments: ` wrap is
/// this layer's job (see [`EngineError::bad_args`]).
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum EngineError {
    /// `ErrBadArgs` — exit **2**. Validation: unknown kind, bad slug/mode,
    /// control chars, a prompt for a kind that takes none, plan-cap violations.
    BadArgs(String),
    /// `ErrDuplicateSlug` — exit **3**. The tmux session name is taken.
    DuplicateSlug(String),
    /// `ErrSessionNotFound` — exit **4**. The target session is gone (or a
    /// `--session-id` no longer matches).
    SessionNotFound(String),
    /// Anything else — exit **1**: a tmux transport failure, a plan-file I/O
    /// failure, a post-ready kickoff-delivery failure.
    Other(String),
}

impl EngineError {
    /// Build a [`EngineError::BadArgs`] from any message (the `%w`-of-ErrBadArgs
    /// wrap, spelled once).
    pub fn bad_args(msg: impl Into<String>) -> Self {
        EngineError::BadArgs(msg.into())
    }

    /// The process exit code this error class maps to (`exitCode`,
    /// `clirc.go:153`).
    pub fn exit_code(&self) -> i32 {
        match self {
            EngineError::BadArgs(_) => 2,
            EngineError::DuplicateSlug(_) => 3,
            EngineError::SessionNotFound(_) => 4,
            EngineError::Other(_) => 1,
        }
    }
}

impl std::fmt::Display for EngineError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            // The three prefixes are Go's sentinel `Error()` strings; the `: `
            // is `fmt.Errorf("%w: …")`'s join.
            EngineError::BadArgs(m) => write!(f, "invalid arguments: {m}"),
            EngineError::DuplicateSlug(m) => write!(f, "rc session already exists: {m}"),
            EngineError::SessionNotFound(m) => write!(f, "rc session not found: {m}"),
            EngineError::Other(m) => write!(f, "{m}"),
        }
    }
}

impl std::error::Error for EngineError {}

// ---------------------------------------------------------------------------
// injected seams
// ---------------------------------------------------------------------------

/// The environment reader (`rc.Getenv`, `ops.go:34`) — injected so tests (and the
/// harness) drive `HOME`/`SHED_WORKSPACE`/`CLAUDE_CONFIG_DIR` without touching
/// the process environment.
pub type GetEnv<'a> = &'a dyn Fn(&str) -> String;

type EnvFn<'a> = Box<dyn Fn(&str) -> String + 'a>;
type SleepFn<'a> = Box<dyn Fn(Duration) + 'a>;
type MonotonicFn<'a> = Box<dyn Fn() -> Instant + 'a>;
type WarnFn<'a> = Box<dyn Fn(&str) + 'a>;
type BinProbeFn<'a> = Box<dyn Fn(&str) -> bool + 'a>;
type HookFn<'a> = Box<dyn Fn() + 'a>;
/// `(kind, workdir, getenv) -> Result<(), reason>` — the per-tool preseed
/// dispatch (`AgentSpec.Preseed`, called at `ops.go:202`). C5 supplies the claude
/// and cursor implementations; until then the engine simply has none wired.
type PreseedFn<'a> = Box<dyn Fn(&RcKind, &str, &dyn Fn(&str) -> String) -> Result<(), String> + 'a>;

/// Go registers `defer opts.EnsureHub()` at a very specific point in `Create`
/// (`ops.go:237`) — AFTER the plan write, so a plan-write failure never ensures
/// the hub, and BEFORE the wait, so a kickoff-delivery failure still does. A Drop
/// guard is the faithful Rust spelling of that placement.
struct EnsureHubGuard<'h> {
    hook: Option<&'h dyn Fn()>,
}

impl Drop for EnsureHubGuard<'_> {
    fn drop(&mut self) {
        if let Some(hook) = self.hook {
            hook();
        }
    }
}

// ---------------------------------------------------------------------------
// options
// ---------------------------------------------------------------------------

/// Create's inputs (`rc.CreateOptions`, `ops.go:37`), minus the hooks — those
/// live on the [`Engine`] because they are per-process wiring, not per-call data.
///
/// Empty-string fields mean ABSENT, exactly as in Go (an `Option<String>` would
/// read better but would quietly diverge on `Some("")`, which the Go side treats
/// as absent everywhere).
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct CreateOptions {
    pub kind: RcKind,
    /// Defaults to the slug.
    pub display_name: String,
    /// Generated when empty; validated against the caller grammar when not.
    pub slug: String,
    /// Defaults to `$SHED_WORKSPACE`, then `$HOME`.
    pub workdir: String,
    /// Provenance `<tool>/<version>`; the CLI supplies its own default.
    pub created_by: String,
    /// Advisory target label.
    pub target: String,
    /// Kickoff line (implies wait); mutually exclusive with [`Self::plan`].
    pub prompt: String,
    /// Plan content: written to a per-kind HOME-rooted file and delivered as a
    /// composed kickoff (so it also implies wait).
    pub plan: String,
    /// Caller framing prepended to the composed plan kickoff (plan only).
    pub plan_framing: String,
    /// Block until ready, accept trust, deliver the prompt.
    pub wait: bool,
    /// Wrap the inner command in `bash -ic` (the native-machine rc-file PATH
    /// accommodation; OFF for guest sessions, whose SSH `bash -lc` wrap already
    /// supplies a login PATH).
    pub interactive_shell: bool,
    /// `""` omits the posture entirely (the tool's own default).
    pub permission_mode: String,
}

impl CreateOptions {
    /// A create for `kind` with every other field at its absent/default value.
    pub fn new(kind: RcKind) -> Self {
        Self {
            kind,
            display_name: String::new(),
            slug: String::new(),
            workdir: String::new(),
            created_by: String::new(),
            target: String::new(),
            prompt: String::new(),
            plan: String::new(),
            plan_framing: String::new(),
            wait: false,
            interactive_shell: false,
            permission_mode: String::new(),
        }
    }
}

/// `prompt`'s inputs (`rc.PromptOptions`, `ops.go:422`).
#[derive(Debug, Clone, PartialEq, Eq, Default)]
pub struct PromptOptions {
    pub slug: String,
    pub text: String,
    /// Optional; must equal the session's `SHED_RC_ID` when set — the guard
    /// against a recreated slug silently receiving another run's prompt.
    pub session_id: String,
}

// ---------------------------------------------------------------------------
// the engine
// ---------------------------------------------------------------------------

/// The one-shot RC engine over an injected tmux runner.
///
/// Built with [`Engine::new`] and configured through the `with_*` builders; every
/// seam has a production default so a caller only overrides what it needs:
///
/// | seam | default | Go original |
/// |---|---|---|
/// | env | `std::env::var` | `d.getenv` |
/// | sleep | `thread::sleep` | the `sleep` parameter (`ops.go:99`) |
/// | monotonic clock | `Instant::now` | `time.Now` in `waitUntilReady` |
/// | wall clock | [`system_clock`] | `time.Now().UTC()` for `created_at` |
/// | settle | 750 ms | `sendLineSettle` (`tmux.go:150`) |
/// | warn | discard | `CreateOptions.Warnf` |
/// | bin probe | **none** (gate skipped) | `CreateOptions.BinProbe` |
/// | preseed | **none** | `AgentSpec.Preseed` (C5) |
/// | ensure-hub | **none** | `CreateOptions.EnsureHub` |
pub struct Engine<'a> {
    tmux: Tmux<'a>,
    env: EnvFn<'a>,
    sleep: SleepFn<'a>,
    monotonic: MonotonicFn<'a>,
    clock: ClockRef,
    warn: Option<WarnFn<'a>>,
    bin_probe: Option<BinProbeFn<'a>>,
    preseed: Option<PreseedFn<'a>>,
    ensure_hub: Option<HookFn<'a>>,
}

impl<'a> Engine<'a> {
    /// An engine over `runner` with production defaults for every seam.
    pub fn new(runner: &'a dyn TmuxRunner) -> Self {
        Self {
            tmux: Tmux::new(runner),
            env: Box::new(|key| std::env::var(key).unwrap_or_default()),
            sleep: Box::new(std::thread::sleep),
            monotonic: Box::new(Instant::now),
            clock: system_clock(),
            warn: None,
            bin_probe: None,
            preseed: None,
            ensure_hub: None,
        }
    }

    /// Override the environment reader.
    #[must_use]
    pub fn with_env(mut self, env: impl Fn(&str) -> String + 'a) -> Self {
        self.env = Box::new(env);
        self
    }

    /// Override the poll/settle sleep (tests pass a no-op, optionally advancing a
    /// fake clock).
    #[must_use]
    pub fn with_sleep(mut self, sleep: impl Fn(Duration) + 'a) -> Self {
        self.sleep = Box::new(sleep);
        self
    }

    /// Override the MONOTONIC clock the `--wait` deadline is measured against.
    #[must_use]
    pub fn with_monotonic(mut self, now: impl Fn() -> Instant + 'a) -> Self {
        self.monotonic = Box::new(now);
        self
    }

    /// Override the WALL clock (`created_at`).
    #[must_use]
    pub fn with_clock(mut self, clock: ClockRef) -> Self {
        self.clock = clock;
        self
    }

    /// Override the inter-key settle (tests pass [`Duration::ZERO`]).
    #[must_use]
    pub fn with_settle(mut self, settle: Duration) -> Self {
        self.tmux = self.tmux.with_settle(settle);
        self
    }

    /// Wire the non-fatal create-time diagnostic sink (`CreateOptions.Warnf`).
    /// Today it carries preseed outcomes: a preseed never fails a create, but a
    /// silently skipped one is invisible.
    #[must_use]
    pub fn with_warn(mut self, warn: impl Fn(&str) + 'a) -> Self {
        self.warn = Some(Box::new(warn));
        self
    }

    /// Wire the installed-agent gate (see [`real_bin_probe`]). **The caller MUST
    /// bind the same shell verb as the create's `interactive_shell`** — see
    /// [`real_bin_probe`]'s doc for why the two launch paths genuinely differ.
    /// Unwired (the default) skips the gate entirely, exactly as a nil
    /// `BinProbe` does in Go.
    #[must_use]
    pub fn with_bin_probe(mut self, probe: impl Fn(&str) -> bool + 'a) -> Self {
        self.bin_probe = Some(Box::new(probe));
        self
    }

    /// Wire the per-tool preseed dispatch (C5). Best-effort: a failure is
    /// reported through the warn sink and NEVER fails the create.
    #[must_use]
    pub fn with_preseed(
        mut self,
        preseed: impl Fn(&RcKind, &str, &dyn Fn(&str) -> String) -> Result<(), String> + 'a,
    ) -> Self {
        self.preseed = Some(Box::new(preseed));
        self
    }

    /// Wire the best-effort hub ensure. Called once, after everything else a
    /// successful create does; skipped entirely when [`ENV_NO_HUB`] is set to any
    /// non-empty value.
    #[must_use]
    pub fn with_ensure_hub(mut self, hook: impl Fn() + 'a) -> Self {
        self.ensure_hub = Some(Box::new(hook));
        self
    }

    fn warn(&self, msg: &str) {
        if let Some(warn) = &self.warn {
            warn(msg);
        }
    }

    /// The hub hook to fire on the way out of a successful create, or `None` when
    /// no hook is wired or [`ENV_NO_HUB`] neutralizes it (mirroring Go's
    /// `ensureHubHook`, `clirc.go:591`, which returns a nil hook in both cases).
    fn hub_hook(&self) -> Option<&(dyn Fn() + 'a)> {
        let hook = self.ensure_hub.as_ref()?;
        if !(self.env)(ENV_NO_HUB).is_empty() {
            return None;
        }
        Some(&**hook)
    }

    // -----------------------------------------------------------------------
    // create (ops.go:99)
    // -----------------------------------------------------------------------

    /// Bootstrap a managed RC session and return its DTO.
    ///
    /// With `wait` (or a prompt/plan) it blocks until ready, auto-accepts the
    /// trust — and, for a bypass posture, the bypass — dialog, and delivers the
    /// kickoff line.
    ///
    /// **The step order below is contract, not incidental.** In particular:
    /// validation happens before ANY tmux work (a rejected create must leave
    /// nothing behind), the preseed runs before `new-session` (it seeds the
    /// config the agent reads at startup), and the plan file is written AFTER
    /// tmux accepts the session name (so a duplicate `--slug` cannot clobber the
    /// live session's plan).
    pub fn create(&self, mut opts: CreateOptions) -> Result<RcSessionDto, EngineError> {
        // 1. Kind. `is_known` is the registry membership `IsValidKind` tests
        //    (`rc.go:52`): every kind outside the registry decodes to
        //    `RcKind::Other`.
        if !opts.kind.is_known() {
            return Err(EngineError::bad_args(format!(
                "unknown kind {}",
                quote_go(opts.kind.as_str())
            )));
        }

        // 2. Installed-agent gate (`ops.go:109`). Before any tmux work, confirm
        //    the kind's binary is reachable on the LAUNCH PATH. Without this a
        //    missing binary surfaces only as an opaque "session died on create
        //    (state=dead)" once the inner command exits immediately. Skipped for
        //    a kind with no bin (shell) and when no probe is wired.
        if let (Some(bin), Some(probe)) = (
            tool_for(&opts.kind).and_then(|t| t.bin()),
            self.bin_probe.as_ref(),
        ) {
            if !probe(bin) {
                // The engine backs both a shed's `shed-ext-rc` (agents baked into
                // the rootfs image) and a machine's `shed-machine-rc` (agents
                // user-installed), so the remediation names both possibilities.
                return Err(EngineError::bad_args(format!(
                    "agent {bin:?} was not found on the session PATH — it may be missing from this shed's image (recreate from a newer image) or not installed on this machine; or pick another --kind"
                )));
            }
        }

        // 3. Prompt validation (`ops.go:118`).
        if !opts.prompt.is_empty() {
            if !opts.kind.accepts_typed_input() {
                return Err(EngineError::bad_args(format!(
                    "kind {} does not accept a prompt",
                    quote_go(opts.kind.as_str())
                )));
            }
            opts.prompt = normalize_newlines(&opts.prompt);
            if has_unsafe_prompt_chars(&opts.prompt) {
                return Err(EngineError::bad_args(
                    "prompt contains an unsupported control character",
                ));
            }
        }

        // 4. Plan validation (`ops.go:130`) — before any side effect; the file is
        //    written and the kickoff composed after the slug resolves.
        if !opts.plan.is_empty() {
            opts.plan_framing =
                validate_plan_inputs(&opts.kind, &opts.plan, &opts.prompt, &opts.plan_framing)?;
        } else if !opts.plan_framing.is_empty() {
            return Err(EngineError::bad_args("plan framing given without a plan"));
        }

        // 5. Permission mode (`ops.go:139`). The kernel raises the domain error;
        //    the `invalid arguments: ` wrap + exit class is this layer's.
        validate_engine_permission_mode(&opts.kind, &opts.permission_mode)
            .map_err(|e| EngineError::bad_args(e.to_string()))?;

        // 6. Slug (`ops.go:143`).
        let slug = if opts.slug.is_empty() {
            gen_slug()
        } else if valid_caller_slug(&opts.slug) {
            opts.slug.clone()
        } else {
            return Err(EngineError::bad_args(format!(
                "invalid slug {}",
                quote_go(&opts.slug)
            )));
        };

        // 7. Workdir (`ops.go:154`).
        let workdir = first_non_empty([
            opts.workdir.as_str(),
            &(self.env)("SHED_WORKSPACE"),
            &(self.env)("HOME"),
        ]);
        if workdir.is_empty() {
            return Err(EngineError::bad_args(
                "no --workdir and SHED_WORKSPACE/HOME unset",
            ));
        }

        // 8. Names (`ops.go:159`).
        let display_name = if opts.display_name.is_empty() {
            slug.clone()
        } else {
            opts.display_name.clone()
        };
        let created_by = if opts.created_by.is_empty() {
            DEFAULT_CREATED_BY.to_string()
        } else {
            opts.created_by.clone()
        };
        let name = tmux_name(&slug);

        // 9. opencode-only loopback port (`ops.go:175`), allocated BEFORE the
        //    metadata so it can be stamped into the session env for the hub's
        //    watcher and passed into the inner command. A failed allocation is
        //    NON-FATAL: the port stays 0 and the session is created and usable,
        //    just not SSE-watchable.
        let port = if opts.kind == RcKind::Opencode {
            super::netutil::free_loopback_port().unwrap_or(0)
        } else {
            0
        };

        // 10. Metadata + the `-e KEY=value` argv fragment (`ops.go:181`).
        let meta = RcMetadata {
            id: uuid::Uuid::new_v4().to_string(),
            display_name: display_name.clone(),
            kind: opts.kind.clone(),
            workdir: workdir.clone(),
            created_by: created_by.clone(),
            created_at: self.clock.now_iso8601(),
            target: opts.target.clone(),
            slug: slug.clone(),
            port,
        };
        let env_args = build_env_args(&meta).map_err(|e| EngineError::bad_args(e.to_string()))?;

        // 11. Best-effort per-tool preseed (`ops.go:202`) — claude's trust +
        //     onboarding, cursor's hook relay. A failure NEVER fails the create;
        //     it is reported through the warn sink so a skipped preseed is
        //     visible instead of silent (cursor's device guard deliberately
        //     declines to write into a host auth mount and would otherwise leave
        //     the operator wondering why the session has no feed).
        if let Some(preseed) = &self.preseed {
            if let Err(reason) = preseed(&opts.kind, &workdir, &*self.env) {
                let tool = tool_for(&opts.kind).map(|t| t.as_str()).unwrap_or_default();
                self.warn(&format!("{tool} preseed skipped: {reason}"));
            }
        }

        // 12. The session itself (`ops.go:208`).
        let inner = inner_command(
            &opts.kind,
            &display_name,
            &opts.permission_mode,
            opts.interactive_shell,
            port,
        );
        let res = self.tmux.create_session(&name, &workdir, &env_args, &inner);
        if res.code != 0 {
            if is_duplicate_session(&res.stderr) {
                return Err(EngineError::DuplicateSlug(name));
            }
            return Err(EngineError::Other(format!(
                "tmux new-session failed: {}",
                format!("{}{}", res.stderr, res.stdout).trim()
            )));
        }

        // 13. Plan delivery (`ops.go:225`): write the plan to its per-kind
        //     HOME-rooted file (0600) and compose the kickoff the poller types
        //     once the session is ready. AFTER the tmux create so a duplicate
        //     `--slug` never clobbers the live session's plan. A write failure IS
        //     fatal (unlike the preseed) — the whole point of a plan run is that
        //     the file is there to read — and the just-created session is torn
        //     down so a failed plan create leaves nothing behind.
        if !opts.plan.is_empty() {
            match write_plan(&opts.kind, &slug, &opts.plan, &*self.env) {
                Ok(path) => opts.prompt = compose_plan_kickoff(&path, &opts.plan_framing),
                Err(err) => {
                    self.tmux.kill_session(&name);
                    return Err(err);
                }
            }
        }

        // 14. The session now exists: ensure the local hub is running so it
        //     starts watching. Deferred (see [`EnsureHubGuard`]) so it fires on
        //     the way out regardless of the wait/kickoff outcome.
        let _ensure_hub = EnsureHubGuard {
            hook: self.hub_hook(),
        };

        let mut session = RcSessionDto {
            slug: slug.clone(),
            tmux_session: name.clone(),
            kind: opts.kind.clone(),
            state: RcState::Starting,
            managed: true,
            // Derived exactly as parse_session derives it, so the create DTO and
            // a later list/probe of the same session agree.
            lane: Some(lane_for_kind(&opts.kind).to_string()),
            display_name: Some(display_name),
            workdir: Some(workdir),
            url: None,
            id: Some(meta.id),
            created_by: none_if_empty(created_by),
            created_at: Some(meta.created_at),
            target_label: none_if_empty(opts.target.clone()),
            activity: None,
            activity_at: None,
            last_message: None,
            pending_approvals: None,
        };

        // 15. Wait (`ops.go:258`).
        if opts.wait || !opts.prompt.is_empty() {
            // The one-time bypass-acceptance dialog appears only for a claude
            // session whose RESOLVED posture is full bypass — true for both the
            // generic `skip` and the claude-historical `bypassPermissions`, since
            // both map to the same flag.
            let bypass = perm_flags(&opts.kind, &opts.permission_mode)
                .unwrap_or_default()
                .contains(&PERMISSION_MODE_BYPASS);
            let (state, url, outcome) =
                self.wait_until_ready(&name, &opts.kind, &opts.prompt, bypass);
            session.state = state;
            session.url = url;
            // A delivery failure after ready IS the create's outcome: reporting
            // success would let a plan/prompt run exit 0 with nothing started.
            // (The session is left running for the caller to inspect/retry.)
            outcome?;
        }
        Ok(session)
    }

    /// Poll the pane until a terminal state (or timeout), auto-accepting the
    /// bypass and trust dialogs once each, then deliver `prompt` if the session
    /// reached ready (`waitUntilReady`, `ops.go:281`).
    ///
    /// The returned error is non-`Ok` ONLY for a kickoff-delivery failure after
    /// ready — a classified non-ready state is a RESULT, not an error.
    fn wait_until_ready(
        &self,
        name: &str,
        kind: &RcKind,
        prompt: &str,
        bypass: bool,
    ) -> (RcState, Option<String>, Result<(), EngineError>) {
        let start = (self.monotonic)();
        let mut state = RcState::Starting;
        let mut url = None;
        let mut trust_accepted = false;
        let mut bypass_accepted = false;
        while (self.monotonic)().duration_since(start) < DEFAULT_WAIT_TIMEOUT {
            let cap = self.tmux.capture_pane(name);
            if cap.code != 0 {
                if is_missing_session(&cap.stderr) {
                    // The session is gone (the inner command exited immediately)
                    // — report dead now rather than polling empty output until
                    // the deadline.
                    return (RcState::Dead, None, Ok(()));
                }
                (self.sleep)(DEFAULT_POLL_EVERY); // transient; keep polling
                continue;
            }
            // A bypassPermissions session shows a one-time acceptance dialog
            // before anything else; accept it once so the session can proceed
            // unattended. Gated on `bypass` so a look-alike screen never draws a
            // stray keypress otherwise.
            if bypass
                && kind.runs_claude()
                && !bypass_accepted
                && is_bypass_accept_prompt(&cap.stdout)
            {
                // Only latch on a SUCCESSFUL send: a transient send-keys failure
                // must stay retryable rather than stalling until timeout.
                if self.tmux.accept_bypass_prompt(name).code == 0 {
                    bypass_accepted = true;
                }
                (self.sleep)(DEFAULT_POLL_EVERY);
                continue;
            }
            let classified = classify_pane(kind, &cap.stdout);
            state = classified.state;
            url = classified.url;
            if state == RcState::NeedsTrust && !trust_accepted {
                // Every agent's directory-trust gate captured so far pre-selects
                // "yes" and is accepted with Enter (claude's "Yes, I trust this
                // folder"; codex's "1. Yes, continue · Press enter to continue"),
                // so a single Enter accepts it for any kind.
                trust_accepted = true;
                self.tmux.send_enter(name);
                (self.sleep)(DEFAULT_POLL_EVERY);
                continue;
            }
            if state != RcState::Starting {
                break;
            }
            (self.sleep)(DEFAULT_POLL_EVERY);
        }
        if state == RcState::Ready && !prompt.is_empty() {
            (self.sleep)(PROMPT_DELIVER_SETTLE);
            let res = self.tmux.send_line(name, prompt);
            if res.code != 0 {
                if is_missing_session(&res.stderr) {
                    // Killed between classification and delivery: that is a dead
                    // session, not a transport failure.
                    return (RcState::Dead, None, Ok(()));
                }
                return (
                    state,
                    url,
                    Err(EngineError::Other(format!(
                        "session {name} is ready but kickoff delivery failed: {}",
                        res.stderr.trim()
                    ))),
                );
            }
        }
        (state, url, Ok(()))
    }

    // -----------------------------------------------------------------------
    // list / probe / prompt / kill / accept-trust
    // -----------------------------------------------------------------------

    /// Every `rc-*` session's DTO (`List`, `ops.go:348`). `display_fallback`
    /// receives a slug; the one-shot verbs pass `None`, so an unstored display
    /// name is omitted and the consuming app applies its own target-aware
    /// fallback.
    ///
    /// **The one-shot list never consults the hub** — activity enrichment is
    /// server-side. The `capabilities` block Go's CLI embeds is added by the CLI
    /// layer (`clirc.go:361`), not here; it lands with C5's `capabilities.rs`.
    pub fn list(&self, display_fallback: Option<&dyn Fn(&str) -> String>) -> RcSessionListDto {
        let names = self.tmux.list_session_names();
        let rc_sessions = names
            .iter()
            .map(|name| {
                let env = self.tmux.show_environment(name);
                let pane = self.tmux.capture_pane(name).stdout;
                parse_session(name, &env, &pane, display_fallback)
            })
            .collect();
        RcSessionListDto {
            rc_sessions,
            capabilities: None,
        }
    }

    /// One session's DTO, state/url derived live (`Probe`, `ops.go:403`).
    /// [`EngineError::SessionNotFound`] when the session is gone.
    pub fn probe(
        &self,
        slug: &str,
        display_fallback: Option<&dyn Fn(&str) -> String>,
    ) -> Result<RcSessionDto, EngineError> {
        self.load_session(slug, display_fallback)
    }

    /// Accept a still-showing workspace-trust prompt (`AcceptTrust`,
    /// `ops.go:409`): re-capture, RE-VERIFY the dialog is up, then send a single
    /// Enter. A no-op success when the dialog is absent — never a stray keypress
    /// into a live session.
    pub fn accept_trust(&self, slug: &str) -> Result<(), EngineError> {
        let name = tmux_name(slug);
        let pane = self.capture_pane_checked(&name)?;
        if is_trust_prompt(&pane) {
            self.tmux.send_enter(&name);
        }
        Ok(())
    }

    /// Deliver a line to a READY session (`Prompt`, `ops.go:430`), re-verifying
    /// kind + state + the optional session id before sending. Prints nothing on
    /// success.
    pub fn prompt(&self, opts: &PromptOptions) -> Result<(), EngineError> {
        let text = normalize_newlines(&opts.text);
        if has_unsafe_prompt_chars(&text) {
            return Err(EngineError::bad_args(
                "text contains an unsupported control character",
            ));
        }
        let session = self.load_session(&opts.slug, None)?;
        if !opts.session_id.is_empty() && session.id.as_deref() != Some(opts.session_id.as_str()) {
            return Err(EngineError::SessionNotFound(
                "session id mismatch (recreated?)".to_string(),
            ));
        }
        if !session.kind.accepts_typed_input() {
            return Err(EngineError::bad_args(format!(
                "kind {} does not accept a prompt",
                quote_go(session.kind.as_str())
            )));
        }
        if session.state != RcState::Ready {
            return Err(EngineError::bad_args(format!(
                "session not ready (state={})",
                state_wire(session.state)
            )));
        }
        // Surface a delivery failure (e.g. the session was killed between the
        // check and the send) instead of reporting a false success.
        let name = tmux_name(&opts.slug);
        let res = self.tmux.send_line(&name, &text);
        if res.code != 0 {
            if is_missing_session(&res.stderr) {
                return Err(EngineError::SessionNotFound(name));
            }
            return Err(EngineError::Other(format!(
                "tmux send-keys failed: {}",
                res.stderr.trim()
            )));
        }
        Ok(())
    }

    /// Tear a session down (`Kill`, `ops.go:461`). **Idempotent: a missing
    /// session is success** (exit 0) — pinned by the parity harness, because a
    /// caller reconciling state must not have to distinguish "killed it" from
    /// "it was already gone".
    pub fn kill(&self, slug: &str) -> Result<(), EngineError> {
        let res = self.tmux.kill_session(&tmux_name(slug));
        if res.code == 0 || is_missing_session(&res.stderr) {
            return Ok(());
        }
        Err(EngineError::Other(format!(
            "tmux kill-session failed: {}",
            res.stderr.trim()
        )))
    }

    /// A session's pane text (visible frame + scrollback), mapping a gone session
    /// to [`EngineError::SessionNotFound`] (`capturePaneChecked`, `ops.go:368`) —
    /// shared by probe/prompt/accept-trust, which is what makes all three exit 4
    /// on a missing slug.
    fn capture_pane_checked(&self, name: &str) -> Result<String, EngineError> {
        let res = self.tmux.capture_pane(name);
        if res.code != 0 {
            if is_missing_session(&res.stderr) {
                return Err(EngineError::SessionNotFound(name.to_string()));
            }
            return Err(EngineError::Other(format!(
                "tmux capture-pane failed: {}",
                res.stderr.trim()
            )));
        }
        Ok(res.stdout)
    }

    /// Capture a session's pane + env and parse it into a DTO (`loadSession`,
    /// `ops.go:392`).
    fn load_session(
        &self,
        slug: &str,
        display_fallback: Option<&dyn Fn(&str) -> String>,
    ) -> Result<RcSessionDto, EngineError> {
        let name = tmux_name(slug);
        let pane = self.capture_pane_checked(&name)?;
        let env = self.tmux.show_environment(&name);
        Ok(parse_session(&name, &env, &pane, display_fallback))
    }
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

/// The production installed-agent gate (`realBinProbe`, `clirc.go:447`):
/// `bash <-lc|-ic> 'command -v <bin>'`, bounded by [`AGENT_PROBE_TIMEOUT`].
///
/// **The shell verb must match the create's `interactive_shell`**, because the
/// two launch paths genuinely consult different PATHs:
///
/// - `interactive_shell = false` (the guest path) — the inner tmux command is a
///   bare exec inheriting the pane's environment, and that pane was created under
///   the server's `bash -lc` wrap (a LOGIN shell: `/etc/profile.d/*.sh` +
///   `/etc/environment.d`). A `-ic` probe here would consult a NARROWER PATH than
///   the real launch and false-negative-reject an installed agent.
/// - `interactive_shell = true` (native machines) — the inner command really is
///   wrapped in `bash -ic` (an rc-file PATH: nvm/asdf/mise shims from
///   `.bashrc`), so the probe matches with `-ic`.
///
/// `command` is a bash builtin, so even a shell too minimal to define anything
/// else still answers (as not-found) rather than crashing. `bin` values come from
/// the registry (fixed literals), never user input — and are shell-quoted anyway.
///
/// Rust has no `CommandContext`, so the timeout is a `try_wait` poll that kills
/// the child on expiry; Go's context cancellation kills it the same way.
pub fn real_bin_probe(bin: &str, interactive_shell: bool) -> bool {
    let flag = if interactive_shell { "-ic" } else { "-lc" };
    let script = format!("command -v {}", shell_quote_always(bin));
    let Ok(mut child) = Command::new("bash")
        .arg(flag)
        .arg(&script)
        .stdin(Stdio::null())
        .stdout(Stdio::null())
        .stderr(Stdio::null())
        .spawn()
    else {
        return false;
    };
    let deadline = Instant::now() + AGENT_PROBE_TIMEOUT;
    loop {
        match child.try_wait() {
            Ok(Some(status)) => return status.success(),
            Ok(None) if Instant::now() < deadline => {
                std::thread::sleep(Duration::from_millis(10));
            }
            _ => {
                let _ = child.kill();
                let _ = child.wait();
                return false;
            }
        }
    }
}

/// `firstNonEmpty` (`ops.go:480`).
fn first_non_empty<'v, I: IntoIterator<Item = &'v str>>(values: I) -> String {
    values
        .into_iter()
        .find(|v| !v.is_empty())
        .unwrap_or_default()
        .to_string()
}

/// Go's `omitempty` on a string field, in the DTO's `Option` idiom.
///
/// Deliberately a local twin of `rc_agents`'s private helper of the same name
/// (`parse_session` uses it for the very same DTO fields): exporting a two-line
/// predicate across the crate boundary would widen the kernel's public surface
/// for no reader benefit, and both must stay pinned to Go's `omitempty` anyway.
fn none_if_empty(s: String) -> Option<String> {
    if s.is_empty() {
        None
    } else {
        Some(s)
    }
}

/// The wire token for a state — what Go's `State` string type prints in
/// `state=%s`. Mirrors [`RcState`]'s kebab-case serde derive (pinned by a test
/// below so the two cannot drift).
fn state_wire(state: RcState) -> &'static str {
    match state {
        RcState::Starting => "starting",
        RcState::Ready => "ready",
        RcState::Reconnecting => "reconnecting",
        RcState::NeedsTrust => "needs-trust",
        RcState::NeedsAuth => "needs-auth",
        RcState::Dead => "dead",
    }
}

#[cfg(test)]
mod tests;
