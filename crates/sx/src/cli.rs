//! The `sx` dispatch layer — argv → the ported RC engine → JSON stdout + exit code.
//!
//! This is the Rust twin of Go's `internal/ext/clirc` (the shared dispatcher behind
//! `shed-ext-rc` and `shed-machine-rc`), namespaced under `sx rc <subcommand>` so
//! the porcelain verbs (plan 009 C7) get the bare verb names. The compatibility
//! contract it implements — pinned by `tests/rc-parity`, the Go↔Rust differential
//! harness — is:
//!
//! - the same subcommands: `create`, `list`, `capabilities`, `probe`,
//!   `accept-trust`, `prompt`, `kill`, `version` (the hub's `serve` is not a
//!   one-shot verb — the machine hub is a `shed-host-agent` role, plan 010);
//! - the same stdin framing (`--prompt-stdin` / `--plan-stdin` + `--prompt-b64`),
//!   including the CLI-level error messages, which are Go's verbatim;
//! - wire-equivalent stdout — structurally-equal canonical JSON, one document +
//!   trailing newline (NOT byte-equal: Go's `json.Encoder` HTML-escapes
//!   `<`/`>`/`&` and serde_json does not; plan 009 §3.5 pins that no consumer
//!   byte-compares stdout);
//! - the same exit-code classes: 0 / 1 generic / 2 bad args / 3 duplicate slug /
//!   4 session not found — with `kill` on a MISSING slug deliberately exit 0.
//!
//! Everything side-effecting is injected through [`Deps`] so the dispatch is
//! unit-testable against a fake tmux runner with no process, no stdio and no
//! wall-clock sleep — the same seam shape Go's `clirc.deps` provides.

use std::cell::{OnceCell, RefCell};
use std::io::{Read, Write};
use std::sync::Arc;
use std::time::Duration;

use base64::Engine as _;
use shed_app::rc_engine::capabilities::{
    build_capabilities, real_agent_probe, real_installed_probe, AgentProbe, InstalledProbe,
};
use shed_app::rc_engine::ops::{real_bin_probe, CreateOptions, Engine, EngineError, PromptOptions};
use shed_app::rc_engine::plan::{plan_from_bytes, PLAN_MAX_BYTES};
use shed_app::rc_engine::preseed;
use shed_app::rc_engine::text::quote_go;
use shed_app::rc_engine::tmux::TmuxRunner;
use shed_app::RcRunnerRef;
use shed_core::rc::{RcCapabilities, RcKind, PERM_MODE_SKIP};

use crate::args::{boolean, parse, value, ArgError, Parsed, Spec};

/// The program name in usage, `version` output and every error line — and the
/// token the parity harness masks in `created_by`/stderr on both sides.
pub const PROG: &str = "sx";

/// `created_by` when `--created-by` is absent.
///
/// The BARE program name, matching Go's `cfg.DefaultCreatedBy`
/// (`cmd/shed-machine-rc/main.go:34` passes `shed-machine-rc`, not
/// `shed-machine-rc/<version>`), so the two implementations' provenance stamps
/// differ only by the one token the harness masks.
pub const DEFAULT_CREATED_BY: &str = PROG;

/// The top-level usage — owned by [`crate::porcelain`], which knows every verb.
const USAGE: &str = crate::porcelain::USAGE;

const RC_USAGE: &str = "\
usage: sx rc <command> [flags]

commands:
  create   --kind <k> --name <display> [--slug s] [--workdir d] [--created-by t/v]
           [--target label] [--wait] [--interactive-shell]
           [--prompt-stdin | --plan-stdin [--prompt-b64 <b64>]]
           [--permission-mode <m> | --skip]
  list
  capabilities
  probe    --slug <s>
  accept-trust --slug <s>
  prompt   --slug <s> [--session-id <uuid>]   (text read from stdin)
  kill     --slug <s>
  version
";

/// The environment reader (`HOME`, `SHED_WORKSPACE`, `CLAUDE_CONFIG_DIR`, …).
type EnvFn<'a> = Box<dyn Fn(&str) -> String + 'a>;
/// The create-time installed-agent gate, bound to `(bin, interactive_shell)`.
type BinProbeFn<'a> = Box<dyn Fn(&str, bool) -> bool + 'a>;
/// The best-effort post-create hub ensure.
type HookFn<'a> = Box<dyn Fn() + 'a>;
/// The `--wait` poll / settle sleep.
type SleepFn<'a> = Box<dyn Fn(Duration) + 'a>;
/// The per-tool create-time preseed (`AgentSpec.Preseed`).
type PreseedFn<'a> = Box<dyn Fn(&RcKind, &str, &dyn Fn(&str) -> String) -> Result<(), String> + 'a>;
/// This machine's short hostname (the porcelain's default display-name prefix).
type HostnameFn<'a> = Box<dyn Fn() -> String + 'a>;

/// The side-effecting dependencies — real in [`Deps::production`], fakes in tests.
pub struct Deps<'a> {
    /// The tmux process seam.
    pub runner: &'a dyn TmuxRunner,
    /// The environment reader (also `SHED_RC_NO_HUB`, the hub kill-switch).
    pub env: EnvFn<'a>,
    pub stdin: RefCell<Box<dyn Read + 'a>>,
    pub stdout: RefCell<Box<dyn Write + 'a>>,
    pub stderr: RefCell<Box<dyn Write + 'a>>,
    /// The create-time installed-agent gate, bound to `(bin, interactive_shell)`.
    /// `None` skips the gate entirely — exactly what a nil `BinProbe` does in Go,
    /// and what the dispatch tests want (no real `bash` spawn).
    pub bin_probe: Option<BinProbeFn<'a>>,
    /// The best-effort post-create hub ensure. `None` in tests so a create never
    /// forks a daemon; the engine additionally honors the `SHED_RC_NO_HUB`
    /// kill-switch on its own.
    pub ensure_hub: Option<HookFn<'a>>,
    /// Overrides the `--wait` poll sleep (tests pass a no-op).
    pub sleep: Option<SleepFn<'a>>,
    /// Overrides the inter-keystroke settle (tests pass `Duration::ZERO`).
    pub settle: Option<Duration>,
    /// Overrides the capability probe (`d.probe`, `clirc.go:391`). `None` uses
    /// the real `bash -lc` probes; an injected one ALSO disables the fast
    /// installed-only fallback, exactly as Go's `effectiveInstalled`
    /// (`clirc.go:392`) does — an injected probe answers promptly and never hits
    /// the budget.
    pub probe: Option<AgentProbe>,
    /// The per-tool preseed dispatch (`AgentSpec.Preseed`). `None` skips it
    /// entirely — the same "absent hook" shape as `bin_probe`/`ensure_hub`, and
    /// what the dispatch tests want: a preseed WRITES to `$HOME`.
    pub preseed: Option<PreseedFn<'a>>,
    /// The porcelain's REMOTE process seam (`ssh …`), shared with `shed-app`'s
    /// `RcService` so a test can record argv + stdin without spawning ssh.
    /// `None` builds the real [`TokioProcessRunner`] on first use.
    pub remote: Option<RcRunnerRef>,
    /// This machine's short hostname, for the default `<host>/<slug>` display
    /// name. Injected so the name is deterministic under test.
    pub hostname: Option<HostnameFn<'a>>,
    /// The tokio runtime the remote/HTTP paths block on, built on first use —
    /// a purely local `sx agent` never constructs one. `pub` only so a test in a
    /// sibling module can build a `Deps` literal; nothing reads it directly.
    pub runtime: OnceCell<tokio::runtime::Runtime>,
}

impl<'a> Deps<'a> {
    /// The production wiring: real env, real stdio, the real `bash`-backed
    /// installed-agent gate, and the hub-ensure hook.
    pub fn production(runner: &'a dyn TmuxRunner) -> Self {
        Self {
            runner,
            env: Box::new(|key| std::env::var(key).unwrap_or_default()),
            stdin: RefCell::new(Box::new(std::io::stdin())),
            stdout: RefCell::new(Box::new(std::io::stdout())),
            stderr: RefCell::new(Box::new(std::io::stderr())),
            bin_probe: Some(Box::new(real_bin_probe)),
            ensure_hub: Some(Box::new(ensure_hub)),
            sleep: None,
            settle: None,
            probe: None,
            preseed: Some(Box::new(preseed::dispatch)),
            remote: None,
            hostname: Some(Box::new(short_hostname)),
            runtime: OnceCell::new(),
        }
    }

    /// Read one environment variable through the injected reader.
    pub fn env(&self, key: &str) -> String {
        (self.env)(key)
    }

    /// This machine's short hostname (`""` when unknown), the default display
    /// name's prefix.
    pub fn hostname(&self) -> String {
        match &self.hostname {
            Some(f) => f(),
            None => String::new(),
        }
    }

    /// The remote (ssh) process seam.
    pub fn remote_runner(&self) -> RcRunnerRef {
        match &self.remote {
            Some(runner) => Arc::clone(runner),
            None => crate::porcelain::default_remote_runner(),
        }
    }

    /// Block on an async operation using this process's single lazily-built
    /// runtime. A current-thread runtime is deliberate: `sx` runs ONE remote op
    /// (or one SSE stream) at a time, and a multi-thread pool would cost a
    /// thread-per-core spawn on every invocation of a CLI whose whole job is to
    /// be cheap enough for a skill to call in a loop.
    pub fn block_on<F: std::future::Future>(&self, fut: F) -> F::Output {
        let rt = self.runtime.get_or_init(|| {
            tokio::runtime::Builder::new_current_thread()
                .enable_all()
                .build()
                .expect("build a current-thread tokio runtime")
        });
        rt.block_on(fut)
    }

    /// The capability probe pair (`effectiveProbe`/`effectiveInstalled`,
    /// `clirc.go:383-397`): the injected probe with NO fast fallback, or the real
    /// login-shell probes.
    fn probes(&self) -> (AgentProbe, Option<InstalledProbe>) {
        match &self.probe {
            Some(probe) => (Arc::clone(probe), None),
            None => (
                Arc::new(real_agent_probe),
                Some(Arc::new(real_installed_probe)),
            ),
        }
    }

    /// The capabilities payload (`rc.BuildCapabilities(...)`), shared by
    /// `capabilities` and the `list` envelope — Go assembles it the same way in
    /// both places, one guest exec feeding both.
    pub fn capabilities(&self) -> RcCapabilities {
        let (probe, installed) = self.probes();
        build_capabilities(&probe, installed.as_ref())
    }

    pub fn write_out(&self, text: &str) {
        let mut w = self.stdout.borrow_mut();
        let _ = w.write_all(text.as_bytes());
        let _ = w.flush();
    }

    pub fn write_err(&self, line: &str) {
        let mut w = self.stderr.borrow_mut();
        let _ = w.write_all(line.as_bytes());
        let _ = w.write_all(b"\n");
        let _ = w.flush();
    }

    /// An engine over the injected seams for a create with this `interactive`
    /// posture (the bin probe MUST match it — see `real_bin_probe`'s doc).
    pub fn engine(&self, interactive: bool) -> Engine<'_> {
        let mut engine = Engine::new(self.runner)
            .with_env(&*self.env)
            .with_warn(move |msg| self.write_err(&format!("{PROG}: {msg}")));
        if let Some(probe) = &self.bin_probe {
            engine = engine.with_bin_probe(move |bin| probe(bin, interactive));
        }
        if let Some(hook) = &self.ensure_hub {
            engine = engine.with_ensure_hub(&**hook);
        }
        if let Some(sleep) = &self.sleep {
            engine = engine.with_sleep(&**sleep);
        }
        if let Some(settle) = self.settle {
            engine = engine.with_settle(settle);
        }
        if let Some(preseed) = &self.preseed {
            engine = engine.with_preseed(&**preseed);
        }
        engine
    }
}

/// Dispatch `args` (argv minus the program name) and return a process exit code.
pub fn run(deps: &Deps, args: &[String]) -> i32 {
    let Some((cmd, rest)) = args.split_first() else {
        return print_usage(deps, USAGE, 2);
    };
    // The porcelain verbs get first refusal; `rc`/`version`/`help` are the
    // remainder, so a future verb can never silently shadow the engine-compat
    // namespace (`porcelain::dispatch` returns None for anything it doesn't own).
    if let Some(code) = crate::porcelain::dispatch(deps, cmd, rest) {
        return code;
    }
    match cmd.as_str() {
        "rc" => run_rc(deps, rest),
        "version" | "--version" | "-v" => print_version(deps),
        "help" | "-h" | "--help" => print_usage(deps, USAGE, 0),
        other => unknown_command(deps, other, USAGE),
    }
}

/// This machine's hostname truncated at the first dot (`""` on error) — Go's
/// `shortHostname` (`clirc.go:736`), which the absorbed `claude` verb used for
/// the same default display name.
fn short_hostname() -> String {
    let raw = match hostname_raw() {
        Some(h) => h,
        None => return String::new(),
    };
    match raw.find('.') {
        // `i > 0` matches Go exactly: a leading dot is not a truncation point.
        Some(i) if i > 0 => raw[..i].to_string(),
        _ => raw,
    }
}

/// `gethostname(2)` without a dependency: the POSIX name of this host.
fn hostname_raw() -> Option<String> {
    // SAFETY: `buf` is a live, correctly-sized allocation for the whole call and
    // `gethostname` writes at most `len` bytes into it.
    let mut buf = vec![0u8; 256];
    let rc = unsafe { libc::gethostname(buf.as_mut_ptr().cast(), buf.len()) };
    if rc != 0 {
        return None;
    }
    let end = buf.iter().position(|b| *b == 0).unwrap_or(buf.len());
    buf.truncate(end);
    String::from_utf8(buf).ok().filter(|s| !s.is_empty())
}

/// The `sx rc <subcommand>` namespace — the engine-compat surface.
fn run_rc(deps: &Deps, args: &[String]) -> i32 {
    let Some((cmd, rest)) = args.split_first() else {
        return print_usage(deps, RC_USAGE, 2);
    };
    match cmd.as_str() {
        "create" => finish(deps, do_create(deps, rest)),
        "list" => finish(deps, do_list(deps, rest)),
        "capabilities" => finish(deps, do_capabilities(deps, rest)),
        "probe" => finish(deps, do_probe(deps, rest)),
        "accept-trust" => finish(deps, do_slug_only(deps, rest, |e, s| e.accept_trust(s))),
        "prompt" => finish(deps, do_prompt(deps, rest)),
        "kill" => finish(deps, do_slug_only(deps, rest, |e, s| e.kill(s))),
        "version" | "--version" | "-v" => print_version(deps),
        "help" | "-h" | "--help" => print_usage(deps, RC_USAGE, 0),
        other => unknown_command(deps, other, RC_USAGE),
    }
}

/// The identity line, identical on both namespaces (`sx version`, `sx rc version`).
/// Version source, and why it is not `CARGO_PKG_VERSION`: see [`crate::version`].
fn print_version(deps: &Deps) -> i32 {
    deps.write_out(&format!("{PROG} {}\n", crate::version::version()));
    0
}

/// Print a usage block on stderr and return `code` — 0 for an asked-for `help`,
/// 2 when the argv was wrong.
fn print_usage(deps: &Deps, usage: &str, code: i32) -> i32 {
    deps.write_err(usage.trim_end());
    code
}

fn unknown_command(deps: &Deps, cmd: &str, usage: &str) -> i32 {
    deps.write_err(&format!("{PROG}: unknown command {}", quote_go(cmd)));
    print_usage(deps, usage, 2)
}

/// Map a subcommand outcome to an exit code. A domain error is printed the way
/// Go's `fail` does — `"<prog>: <err>"` on stderr, stdout untouched — while a
/// bare code has already had its usage text printed.
fn finish(deps: &Deps, outcome: Result<i32, ExitOr>) -> i32 {
    match outcome {
        Ok(code) | Err(ExitOr::Code(code)) => code,
        Err(ExitOr::Err(err)) => {
            deps.write_err(&format!("{PROG}: {err}"));
            err.exit_code()
        }
    }
}

/// Parse a subcommand's flags, rejecting a stray positional the way Go's
/// `parseArgs` does (a dropped typo is worse than a usage error).
///
/// A flag-grammar violation is a plain usage error (exit 2, no engine involved);
/// a stray positional goes through the engine's bad-args class so its stderr line
/// carries the same `invalid arguments: unexpected argument "x"` text Go emits.
fn parse_flags(deps: &Deps, specs: &[Spec], args: &[String]) -> Result<Parsed, ExitOr> {
    let parsed = parse(specs, args).map_err(|e: ArgError| {
        deps.write_err(&format!("{PROG}: {e}"));
        ExitOr::Code(print_usage(deps, RC_USAGE, 2))
    })?;
    if let Some(stray) = parsed.positionals().first() {
        return Err(ExitOr::Err(EngineError::bad_args(format!(
            "unexpected argument {}",
            quote_go(stray)
        ))));
    }
    Ok(parsed)
}

/// Either a bare exit code (usage already printed) or a domain error to report —
/// the failure type every subcommand body returns, settled by [`finish`].
enum ExitOr {
    Code(i32),
    Err(EngineError),
}

impl From<EngineError> for ExitOr {
    fn from(e: EngineError) -> Self {
        ExitOr::Err(e)
    }
}

const CREATE_SPECS: &[Spec] = &[
    value("kind"),
    value("name"),
    value("slug"),
    value("workdir"),
    value("created-by"),
    value("target"),
    boolean("wait"),
    boolean("interactive-shell"),
    boolean("prompt-stdin"),
    boolean("plan-stdin"),
    value("prompt-b64"),
    value("permission-mode"),
    boolean("skip"),
];

fn do_create(deps: &Deps, args: &[String]) -> Result<i32, ExitOr> {
    let p = parse_flags(deps, CREATE_SPECS, args)?;
    let mode = resolve_mode(p.value("permission-mode"), p.flag("skip"))?;

    // stdin carries at most one payload: a prompt line (--prompt-stdin) OR a plan
    // (--plan-stdin). Caller framing for a plan travels out-of-band as base64
    // (--prompt-b64), never on stdin (`clirc.go:268`).
    if p.flag("prompt-stdin") && p.flag("plan-stdin") {
        return Err(EngineError::bad_args(
            "--prompt-stdin and --plan-stdin are mutually exclusive",
        )
        .into());
    }
    let prompt_b64 = p.value("prompt-b64");
    if !prompt_b64.is_empty() && !p.flag("plan-stdin") {
        return Err(EngineError::bad_args("--prompt-b64 is only valid with --plan-stdin").into());
    }

    let (mut prompt, mut plan, mut framing) = (String::new(), String::new(), String::new());
    if p.flag("prompt-stdin") {
        let line = read_stdin_line(deps)?;
        if line.is_empty() {
            return Err(EngineError::bad_args("--prompt-stdin given but stdin is empty").into());
        }
        prompt = line;
    } else if p.flag("plan-stdin") {
        plan = read_plan_stdin(deps)?;
        if !prompt_b64.is_empty() {
            let raw = decode_b64_go(prompt_b64).map_err(|e| {
                EngineError::bad_args(format!("--prompt-b64 is not valid base64: {e}"))
            })?;
            // Reject non-UTF-8 payloads BEFORE the bytes→string conversion: an
            // invalid byte (a lone C1 0x9b, say) would otherwise become the
            // replacement character and slip past the control-char scan
            // (`clirc.go:297`).
            framing = String::from_utf8(raw).map_err(|_| {
                EngineError::bad_args("--prompt-b64 does not decode to valid UTF-8")
            })?;
        }
    }

    let created_by = match p.value("created-by") {
        "" => DEFAULT_CREATED_BY.to_string(),
        given => given.to_string(),
    };
    // ABSENT → Go's flag default `claude-rc` (`clirc.go:242`); `--kind=` /
    // `--kind ""` → the empty kind reaches the engine and is REJECTED (exit 2),
    // exactly like Go. Conflating the two made sx create a session Go refuses
    // (C4 review finding).
    let kind = match p.value_opt("kind") {
        None => RcKind::ClaudeRc, // `rc.DefaultKind` (`rc.go:43`)
        Some(given) => RcKind::from_wire(given),
    };
    let interactive = p.flag("interactive-shell");

    let opts = CreateOptions {
        kind,
        display_name: p.value("name").to_string(),
        slug: p.value("slug").to_string(),
        workdir: p.value("workdir").to_string(),
        created_by,
        target: p.value("target").to_string(),
        prompt,
        plan,
        plan_framing: framing,
        wait: p.flag("wait"),
        interactive_shell: interactive,
        permission_mode: mode,
    };
    let session = deps.engine(interactive).create(opts)?;
    Ok(print_json(deps, serde_json::to_string(&session)))
}

fn do_list(deps: &Deps, args: &[String]) -> Result<i32, ExitOr> {
    parse_flags(deps, &[], args)?;
    let mut resp = deps.engine(false).list(None);
    // One invocation feeds both the session list and capability discovery
    // (`doList`, `clirc.go:356-364`) — the block is an `omitempty` pointer on the
    // Go side, and always present here because this producer always assembles it.
    resp.capabilities = Some(deps.capabilities());
    Ok(print_json(deps, serde_json::to_string(&resp)))
}

/// The capabilities payload (kinds, per-agent install/version, features, per-kind
/// UI hints) — the discovery mechanism that replaces error-string sniffing
/// (`doCapabilities`, `clirc.go:371`).
fn do_capabilities(deps: &Deps, args: &[String]) -> Result<i32, ExitOr> {
    parse_flags(deps, &[], args)?;
    Ok(print_json(
        deps,
        serde_json::to_string(&deps.capabilities()),
    ))
}

fn do_probe(deps: &Deps, args: &[String]) -> Result<i32, ExitOr> {
    let slug = require_slug(deps, args)?;
    let session = deps.engine(false).probe(&slug, None)?;
    Ok(print_json(deps, serde_json::to_string(&session)))
}

/// `kill` / `accept-trust`: one `--slug`, no output on success.
fn do_slug_only(
    deps: &Deps,
    args: &[String],
    op: impl Fn(&Engine, &str) -> Result<(), EngineError>,
) -> Result<i32, ExitOr> {
    let slug = require_slug(deps, args)?;
    op(&deps.engine(false), &slug)?;
    Ok(0)
}

const PROMPT_SPECS: &[Spec] = &[value("slug"), value("session-id")];

fn do_prompt(deps: &Deps, args: &[String]) -> Result<i32, ExitOr> {
    let p = parse_flags(deps, PROMPT_SPECS, args)?;
    let slug = slug_of(&p)?;
    let text = read_stdin_line(deps)?;
    if text.is_empty() {
        return Err(EngineError::bad_args("prompt text (stdin) is empty").into());
    }
    deps.engine(false).prompt(&PromptOptions {
        slug,
        text,
        session_id: p.value("session-id").to_string(),
    })?;
    Ok(0)
}

const SLUG_SPECS: &[Spec] = &[value("slug")];

/// Parse a slug-only subcommand's flags (`probe`/`accept-trust`/`kill`) and
/// return the required `--slug` (`runSlugCmd`, `clirc.go:514`).
fn require_slug(deps: &Deps, args: &[String]) -> Result<String, ExitOr> {
    slug_of(&parse_flags(deps, SLUG_SPECS, args)?)
}

/// The `--slug` value, which every slug-taking subcommand requires (one wording,
/// so the two call sites cannot drift apart on a contract message).
fn slug_of(p: &Parsed) -> Result<String, ExitOr> {
    match p.value("slug") {
        "" => Err(EngineError::bad_args("--slug is required").into()),
        slug => Ok(slug.to_string()),
    }
}

/// Apply the `--skip` shorthand and its mutual exclusion with
/// `--permission-mode` (`resolveMode`, `clirc.go:212`, with `dflt` empty — the
/// engine-compat `create` passes no posture of its own).
fn resolve_mode(perm_mode: &str, skip: bool) -> Result<String, EngineError> {
    if skip {
        if !perm_mode.is_empty() {
            return Err(EngineError::bad_args(
                "--skip and --permission-mode are mutually exclusive",
            ));
        }
        return Ok(PERM_MODE_SKIP.to_string());
    }
    Ok(perm_mode.to_string())
}

/// Emit an already-serialized DTO as one JSON document + trailing newline on
/// stdout (`printJSON`, `clirc.go:193`). Structural, not byte, parity with Go's
/// encoder — plan 009 §3.5.
///
/// Takes the `to_string` RESULT rather than a generic `impl Serialize` so this
/// crate needs no direct `serde` dependency (serde_json's own error type carries
/// the encoding failure Go reports as `encoding output:`).
fn print_json(deps: &Deps, encoded: serde_json::Result<String>) -> i32 {
    match encoded {
        Ok(text) => {
            deps.write_out(&format!("{text}\n"));
            0
        }
        Err(err) => {
            deps.write_err(&format!("{PROG}: encoding output: {err}"));
            1
        }
    }
}

/// Read all of stdin and strip ONE trailing `\n` then ONE trailing `\r`
/// (`readStdinLine`, `clirc.go:183`), so a kickoff line piped in is not mistaken
/// for a CLI flag.
///
/// Go tolerates invalid UTF-8 here (a Go string is bytes); Rust cannot carry one,
/// so an invalid payload is rejected as bad args. Documented divergence: the
/// contract cases are the EMPTY line (both) and the plan-stdin UTF-8 gate below
/// (message-identical).
fn read_stdin_line(deps: &Deps) -> Result<String, EngineError> {
    let mut buf = Vec::new();
    deps.stdin
        .borrow_mut()
        .read_to_end(&mut buf)
        .map_err(|e| EngineError::Other(format!("reading stdin: {e}")))?;
    let text =
        String::from_utf8(buf).map_err(|_| EngineError::bad_args("stdin is not valid UTF-8"))?;
    let text = text.strip_suffix('\n').unwrap_or(&text);
    Ok(text.strip_suffix('\r').unwrap_or(text).to_string())
}

/// Read a plan from stdin, capped at [`PLAN_MAX_BYTES`] (`readPlanStdin`,
/// `clirc.go:339`). The plan is NOT newline-trimmed — a plan is a document, not a
/// line, and trailing structure is content.
///
/// The three messages here are Go's VERBATIM (the harness diffs the masked stderr
/// of the exit-2 classes): the library's own `plan_from_bytes` text is the
/// deeper-layer wording, and this is the transport boundary.
fn read_plan_stdin(deps: &Deps) -> Result<String, EngineError> {
    let mut buf = Vec::new();
    deps.stdin
        .borrow_mut()
        .by_ref()
        .take(PLAN_MAX_BYTES as u64 + 1)
        .read_to_end(&mut buf)
        .map_err(|e| EngineError::Other(format!("reading plan from stdin: {e}")))?;
    if buf.len() > PLAN_MAX_BYTES {
        return Err(EngineError::bad_args(format!(
            "plan exceeds {PLAN_MAX_BYTES} bytes"
        )));
    }
    if buf.is_empty() {
        return Err(EngineError::bad_args(
            "--plan-stdin given but stdin is empty",
        ));
    }
    plan_from_bytes(&buf)
        .map_err(|_| EngineError::bad_args("plan is not valid UTF-8 (is stdin a binary file?)"))
}

/// The hub's fixed loopback address + byte-frozen health identity token —
/// local mirrors of `shed_broker::rc_hub::hub::{HUB_ADDR, HUB_APP_ID}` (sx
/// deliberately does not link shed-broker; its axum/notify/aws leaf deps
/// belong to the daemon, not the porcelain).
/// The probe targets the PRODUCTION port only, by design: the
/// `SHED_RC_HUB_ADDR` env seam is a test seam (honored by the daemons, set by
/// the parity harness — which also sets `SHED_RC_NO_HUB`, so sx's ensure
/// never runs there), and sx's whole surface consistently ignores it.
const HUB_ADDR: &str = "127.0.0.1:1029";
const HUB_APP_ID: &str = "shed-rc-hub";

/// The best-effort hub ensure, PROBE-ONLY (plan 010 §2.7; the spawn fallback
/// died at H15 with the `shed-machine-rc` binary): a healthy hub answering on
/// the fixed port means done. Otherwise a best-effort stderr hint names
/// `shed-host-agent` as the machine hub's owner. Never fatal: `create` has
/// already succeeded by the time this runs, and the engine skips it entirely
/// when `SHED_RC_NO_HUB` is set.
fn ensure_hub() {
    if hub_is_healthy(HUB_ADDR, std::time::Duration::from_millis(500)) {
        return;
    }
    eprintln!(
        "{PROG}: no rc hub is running; install and start shed-host-agent to serve \
the machine hub (best-effort — sessions still work, without live activity)"
    );
}

/// ONE bounded identity check against `addr`'s `/v1/health` — the sx-local
/// twin of the hub's `queryHubHealth` (hub.go:916): a dial + GET under one
/// ABSOLUTE deadline, true only when a 200 body carries the byte-frozen
/// `app` token. Any failure — nothing listening, a squatter, a slow peer —
/// reads as "no healthy hub" and falls through to the spawn/hint path.
pub(crate) fn hub_is_healthy(addr: &str, timeout: std::time::Duration) -> bool {
    use std::io::{Read, Write};
    let deadline = std::time::Instant::now() + timeout;
    // Zero folds into None (std rejects a zero socket timeout) — the twin of
    // the broker probe's rule (hub.rs).
    let remaining = |d: std::time::Instant| {
        d.checked_duration_since(std::time::Instant::now())
            .filter(|left| !left.is_zero())
    };
    let probe = || -> Option<bool> {
        let sock_addr: std::net::SocketAddr = addr.parse().ok()?;
        let mut conn =
            std::net::TcpStream::connect_timeout(&sock_addr, remaining(deadline)?).ok()?;
        conn.set_write_timeout(Some(remaining(deadline)?)).ok()?;
        conn.write_all(
            format!("GET /v1/health HTTP/1.1\r\nHost: {addr}\r\nConnection: close\r\n\r\n")
                .as_bytes(),
        )
        .ok()?;
        let mut buf = Vec::new();
        let mut chunk = [0u8; 2048];
        while buf.len() < 8192 {
            // ONE sample: a deadline expiring between a guard and a re-read
            // would otherwise arm set_read_timeout(None) = NO timeout — the
            // exact unbounded-probe class the H11 review fixed hub-side.
            let Some(left) = remaining(deadline) else {
                break;
            };
            if conn.set_read_timeout(Some(left)).is_err() {
                break; // judge whatever is buffered, as the broker probe does
            }
            match conn.read(&mut chunk) {
                Ok(0) => break,
                Ok(n) => buf.extend_from_slice(&chunk[..n]),
                Err(_) => break,
            }
        }
        let text = String::from_utf8_lossy(&buf);
        let (head, body) = text.split_once("\r\n\r\n")?;
        // DELIBERATELY simpler than the broker probe: no chunked decoding and
        // one flat 8 KiB cap. Both hubs emit a small Content-Length health
        // body; anything else parses as not-a-hub, which errs toward the
        // idempotent spawn fallback.
        if head.split_whitespace().nth(1)? != "200" {
            return Some(false);
        }
        // Tolerant one-value read: the app token is the identity, everything
        // else on the body is the hub's own business.
        let v: serde_json::Value = serde_json::Deserializer::from_str(body.trim())
            .into_iter()
            .next()?
            .ok()?;
        Some(v.get("app").and_then(|a| a.as_str()) == Some(HUB_APP_ID))
    };
    probe().unwrap_or(false)
}

/// Decode base64 with Go `encoding/base64.StdEncoding.DecodeString` semantics
/// (`clirc.go:292`): standard alphabet, padding REQUIRED — but `\r`/`\n`
/// IGNORED anywhere in the input (Go documents this; `base64(1)` and
/// `openssl base64` wrap at 76 columns, and a framing block is long enough to
/// wrap), and non-canonical trailing bits ACCEPTED (Go does not reject them).
/// Rust's stock `STANDARD` engine is stricter on both axes — measured live as a
/// C4 review divergence.
fn decode_b64_go(input: &str) -> Result<Vec<u8>, base64::DecodeError> {
    use base64::engine::general_purpose::{GeneralPurpose, GeneralPurposeConfig};
    static GO_STD: GeneralPurpose = GeneralPurpose::new(
        &base64::alphabet::STANDARD,
        GeneralPurposeConfig::new().with_decode_allow_trailing_bits(true),
    );
    let stripped: String = input.chars().filter(|c| *c != '\r' && *c != '\n').collect();
    GO_STD.decode(stripped)
}

#[cfg(test)]
mod tests;
