//! The `sx` dispatch layer — argv → the ported RC engine → JSON stdout + exit code.
//!
//! This is the Rust twin of Go's `internal/ext/clirc` (the shared dispatcher behind
//! `shed-ext-rc` and `shed-machine-rc`), namespaced under `sx rc <subcommand>` so
//! the porcelain verbs (plan 009 C7) get the bare verb names. The compatibility
//! contract it implements — pinned by `tests/rc-parity`, the Go↔Rust differential
//! harness — is:
//!
//! - the same subcommands: `create`, `list`, `capabilities`, `probe`,
//!   `accept-trust`, `prompt`, `kill`, `version` (the hub's `serve` is NOT ported;
//!   plan 009 §0 keeps it in the Go binary);
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

use std::cell::RefCell;
use std::io::{Read, Write};
use std::time::Duration;

use base64::Engine as _;
use shed_app::rc_engine::ops::{real_bin_probe, CreateOptions, Engine, EngineError, PromptOptions};
use shed_app::rc_engine::plan::{plan_from_bytes, PLAN_MAX_BYTES};
use shed_app::rc_engine::text::quote_go;
use shed_app::rc_engine::tmux::TmuxRunner;
use shed_core::rc::RcKind;

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

/// The generic full-bypass posture `--skip` expands to (Go's `rc.PermModeSkip`);
/// the registry maps it per agent to that tool's real flag.
const PERM_MODE_SKIP: &str = "skip";

/// The binary whose `serve` verb backs the local activity hub. The hub is NOT
/// ported (plan 009 §0) — on a machine it stays the Go daemon, so `sx`'s
/// ensure-hub hook simply drives it when it is installed.
const HUB_BIN: &str = "shed-machine-rc";

const USAGE: &str = "\
usage: sx <command> [flags]

commands:
  rc <subcommand>   the one-shot RC engine (create|list|capabilities|probe|
                    accept-trust|prompt|kill|version) — wire-compatible with
                    `shed-machine-rc <subcommand>`
  version
  help
";

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
        }
    }

    fn write_out(&self, text: &str) {
        let mut w = self.stdout.borrow_mut();
        let _ = w.write_all(text.as_bytes());
        let _ = w.flush();
    }

    fn write_err(&self, line: &str) {
        let mut w = self.stderr.borrow_mut();
        let _ = w.write_all(line.as_bytes());
        let _ = w.write_all(b"\n");
        let _ = w.flush();
    }

    /// An engine over the injected seams for a create with this `interactive`
    /// posture (the bin probe MUST match it — see `real_bin_probe`'s doc).
    fn engine(&self, interactive: bool) -> Engine<'_> {
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
        engine
    }
}

/// Dispatch `args` (argv minus the program name) and return a process exit code.
pub fn run(deps: &Deps, args: &[String]) -> i32 {
    let Some((cmd, rest)) = args.split_first() else {
        return print_usage(deps, USAGE, 2);
    };
    match cmd.as_str() {
        "rc" => run_rc(deps, rest),
        "version" | "--version" | "-v" => print_version(deps),
        "help" | "-h" | "--help" => print_usage(deps, USAGE, 0),
        other => unknown_command(deps, other, USAGE),
    }
}

/// The `sx rc <subcommand>` namespace — the engine-compat surface.
fn run_rc(deps: &Deps, args: &[String]) -> i32 {
    let Some((cmd, rest)) = args.split_first() else {
        return print_usage(deps, RC_USAGE, 2);
    };
    match cmd.as_str() {
        "create" => finish(deps, do_create(deps, rest)),
        "list" => finish(deps, do_list(deps, rest)),
        "capabilities" => {
            // C5 ports `capabilities.go` (the budgeted concurrent agent probes).
            // Until then this exits non-zero rather than emitting a partial
            // payload a consumer would cache as truth.
            deps.write_err(&format!("{PROG}: capabilities: not yet ported"));
            1
        }
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
fn print_version(deps: &Deps) -> i32 {
    deps.write_out(&format!("{PROG} {}\n", env!("CARGO_PKG_VERSION")));
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
    // The `capabilities` block Go's `doList` embeds lands with C5; until then
    // the envelope carries `rc_sessions` only (the harness strips the block
    // from the Go side so the differential stays honest — plan 009 §5, C4).
    let resp = deps.engine(false).list(None);
    Ok(print_json(deps, serde_json::to_string(&resp)))
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

/// The best-effort hub ensure: spawn `shed-machine-rc serve --detach` when that
/// binary is on PATH.
///
/// The hub is not ported (plan 009 §0) — on a machine it stays the Go daemon — so
/// this drives it rather than reimplementing it, and does nothing at all when the
/// binary is absent (a machine with only `sx` simply has no activity hub). Never
/// fatal: `create` has already succeeded by the time this runs, and the engine
/// skips it entirely when `SHED_RC_NO_HUB` is set.
fn ensure_hub() {
    let Some(bin) = look_path(HUB_BIN) else {
        return;
    };
    let spawned = std::process::Command::new(bin)
        .args(["serve", "--detach"])
        .stdin(std::process::Stdio::null())
        .stdout(std::process::Stdio::null())
        .stderr(std::process::Stdio::null())
        .spawn();
    match spawned {
        // `serve --detach` double-forks and returns as soon as the port is up, so
        // reaping it here is bounded; a child left unwaited would be a zombie for
        // the rest of the (short) process lifetime. A NON-ZERO exit (e.g. a
        // foreign process squatting the hub port) gets one best-effort line,
        // matching Go's EnsureHub diagnostics (`hub.go:964`) — never an error.
        Ok(mut child) => match child.wait() {
            Ok(status) if !status.success() => {
                eprintln!("{PROG}: rc hub ensure failed (best-effort): {HUB_BIN} serve --detach exited {status}");
            }
            _ => {}
        },
        Err(err) => eprintln!("{PROG}: rc hub ensure failed (best-effort): {err}"),
    }
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

/// `exec.LookPath` for a bare binary name: the first executable match on `PATH`.
fn look_path(bin: &str) -> Option<std::path::PathBuf> {
    let path = std::env::var_os("PATH")?;
    std::env::split_paths(&path)
        .map(|dir| dir.join(bin))
        .find(|candidate| is_executable(candidate))
}

fn is_executable(path: &std::path::Path) -> bool {
    use std::os::unix::fs::PermissionsExt;
    std::fs::metadata(path)
        .map(|m| m.is_file() && m.permissions().mode() & 0o111 != 0)
        .unwrap_or(false)
}

#[cfg(test)]
mod tests;
