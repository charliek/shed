//! The **porcelain verbs** (plan 009 C7): `sx agent`, `sx plan`, `sx ls`,
//! `sx watch`, `sx attach`, `sx kill`.
//!
//! These are the surface a skill (or a human) actually types. They sit ABOVE the
//! engine-compat `sx rc *` namespace: `sx rc create` is a wire-compatible sibling
//! of `shed-machine-rc create` and must stay byte-stable, while `sx agent claude`
//! is free to choose sensible defaults, print prose, and reach a shed or a remote
//! machine instead of local tmux.
//!
//! ## Subject-first grammar
//!
//! Every verb that has a subject takes it FIRST — `sx agent claude --on …`,
//! `sx watch abc234 --on …`. That is not decoration: [`crate::args`] reproduces
//! Go's `flag` rule that the first non-flag token ENDS flag parsing, so a
//! trailing subject would swallow every flag after it. Taking the subject before
//! the flag parse makes the two rules agree.
//!
//! ## Where a verb runs
//!
//! [`crate::target`] resolves `--on`; the dispatch table (plan 009 §3.2) then
//! decides the posture:
//!
//! | target | create runs | `--interactive-shell` |
//! |---|---|---|
//! | `local` | the in-process engine, this machine's tmux | on |
//! | `machine:<m>` | `ssh <m> sx rc create …` (or `<rc_bin> create …`) | on |
//! | `shed:<s>` | `ssh <shed> shed-ext-rc create …` | **off** (the guest contract) |
//!
//! Everything remote goes through the async [`RcRunner`](shed_app::RcRunner)
//! seam, so the dispatch is testable against a recording fake with no ssh.

pub mod attach;
pub mod hub;
pub mod kickoff;
pub mod ls;
pub mod watch;

use std::sync::Arc;

use shed_app::rc_engine::ops::EngineError;
use shed_app::{RcRunnerRef, TokioProcessRunner};
use shed_core::config::ShedConfig;

use crate::args::{boolean, parse, value, ArgError, Parsed, Spec};
use crate::cli::{Deps, PROG};
use crate::target::{self, Resolved, Target};

pub const USAGE: &str = "\
usage: sx <command> [flags]

kickoff:
  agent <tool> [--on <target>] [-p <prompt> | --plan <file>] [--skip |
        --permission-mode <m>] [--workdir d] [--name n] [--slug s]
        [--no-wait] [--json]
  plan  <file>  [--on <target>] [--tool <t>] [-p <framing>] [--skip |
        --permission-mode <m>] [--workdir d] [--name n] [--slug s] [--json]

observe:
  ls    [--on <target>]
  watch <slug> [--on <target>]
  attach <slug> [--on <target>] [--print]
  kill  <slug> [--on <target>]

engine-compat:
  rc <subcommand>   the one-shot RC engine (create|list|capabilities|probe|
                    accept-trust|prompt|kill|version) — the frozen machine/guest
                    RC wire; what `--on machine:` invokes on the far side

  version
  help

targets:
  local (default) | machine:<name> | shed:<name>[@<server>]
  machines come from the `machines:` section of ~/.shed/config.yaml.
";

// The two kickoff verbs share most of their grammar but not all of it (`--plan`
// + `--no-wait` are agent-only, `--tool` is plan-only), and `args::Spec` tables
// are `const` slices with no concat — so each verb spells its own out. The
// dispatch tests assert both accept the shared flags.
const AGENT_SPECS: &[Spec] = &[
    value("on"),
    value("permission-mode"),
    boolean("skip"),
    value("workdir"),
    value("name"),
    value("slug"),
    value("p"),
    value("prompt"),
    boolean("json"),
    value("plan"),
    boolean("no-wait"),
];

const PLAN_SPECS: &[Spec] = &[
    value("on"),
    value("permission-mode"),
    boolean("skip"),
    value("workdir"),
    value("name"),
    value("slug"),
    value("p"),
    value("prompt"),
    boolean("json"),
    value("tool"),
];

const TARGET_ONLY_SPECS: &[Spec] = &[value("on")];
const ATTACH_SPECS: &[Spec] = &[value("on"), boolean("print")];

/// A verb's failure: a message (printed as `sx: <msg>`) plus an exit code.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct VerbError {
    pub message: String,
    pub code: i32,
}

impl VerbError {
    /// A usage/argument error — exit 2, the same class the engine's
    /// `ErrBadArgs` maps to.
    pub fn bad_args(message: impl Into<String>) -> Self {
        Self {
            message: message.into(),
            code: 2,
        }
    }

    /// A generic runtime failure — exit 1.
    pub fn failed(message: impl Into<String>) -> Self {
        Self {
            message: message.into(),
            code: 1,
        }
    }
}

impl From<EngineError> for VerbError {
    fn from(e: EngineError) -> Self {
        Self {
            code: e.exit_code(),
            message: e.to_string(),
        }
    }
}

pub type VerbResult = Result<i32, VerbError>;

/// Dispatch a porcelain verb. Returns `None` when `cmd` is not one of ours, so
/// [`crate::cli::run`] can fall through to `rc`/`version`/`help`.
pub fn dispatch(deps: &Deps, cmd: &str, args: &[String]) -> Option<i32> {
    let outcome = match cmd {
        "agent" => run(deps, args, AGENT_SPECS, true, kickoff::agent),
        "plan" => run(deps, args, PLAN_SPECS, true, kickoff::plan),
        "ls" => run(deps, args, TARGET_ONLY_SPECS, false, |d, _, p| {
            ls::run(d, p)
        }),
        "watch" => run(deps, args, TARGET_ONLY_SPECS, true, watch::run),
        "attach" => run(deps, args, ATTACH_SPECS, true, attach::run),
        "kill" => run(deps, args, TARGET_ONLY_SPECS, true, kill),
        _ => return None,
    };
    Some(finish(deps, outcome))
}

type VerbFn = fn(&Deps, &str, &Parsed) -> VerbResult;

/// Take the subject (when the verb has one), parse the remaining flags, and call
/// the verb body.
fn run(
    deps: &Deps,
    args: &[String],
    specs: &[Spec],
    wants_subject: bool,
    body: VerbFn,
) -> VerbResult {
    let (subject, rest) = if wants_subject {
        match args.split_first() {
            // Non-empty AND non-flag: `sx plan ""` must be the same usage error
            // as `sx plan` (an empty subject would otherwise reach a verb body as
            // a legitimate-looking value).
            Some((first, rest)) if !first.is_empty() && !first.starts_with('-') => {
                (first.as_str(), rest)
            }
            _ => {
                return Err(VerbError::bad_args(
                    "a subject is required and must come first (see `sx help`)",
                ))
            }
        }
    } else {
        ("", args)
    };
    let parsed = parse(specs, rest).map_err(|e: ArgError| VerbError::bad_args(e.to_string()))?;
    if let Some(stray) = parsed.positionals().first() {
        return Err(VerbError::bad_args(format!(
            "unexpected argument {stray:?}"
        )));
    }
    body(deps, subject, &parsed)
}

fn finish(deps: &Deps, outcome: VerbResult) -> i32 {
    match outcome {
        Ok(code) => code,
        Err(err) => {
            deps.write_err(&format!("{PROG}: {}", err.message));
            err.code
        }
    }
}

/// `sx kill <slug> [--on <target>]`.
fn kill(deps: &Deps, slug: &str, p: &Parsed) -> VerbResult {
    let resolved = resolve_target(deps, p)?;
    match &resolved {
        Resolved::Local => deps.engine(false).kill(slug)?,
        remote => {
            let prefix = remote_prefix(deps, remote)?;
            let argv = prefix.splice(shed_core::rc::kill_argv(prefix.bin(), slug));
            remote_exec(deps, remote, &argv, None)?;
        }
    }
    deps.write_out(&format!("Killed {slug} on {}\n", resolved.display()));
    Ok(0)
}

// ---------------------------------------------------------------------------
// shared plumbing
// ---------------------------------------------------------------------------

/// Load `~/.shed/config.yaml` through the injected env (so tests can point
/// `HOME`/`SHED_CONFIG` at a fixture without touching the developer's real one).
pub fn load_config(deps: &Deps) -> ShedConfig {
    ShedConfig::load(&target::default_config_path(&*deps.env))
}

/// Parse + resolve `--on`.
pub fn resolve_target(deps: &Deps, p: &Parsed) -> Result<Resolved, VerbError> {
    let parsed = Target::parse(p.value("on")).map_err(VerbError::bad_args)?;
    if matches!(parsed, Target::Local) {
        // Local needs no config at all — never read (or fail on) a file for it.
        return Ok(Resolved::Local);
    }
    let resolved = target::resolve(&parsed, &load_config(deps)).map_err(VerbError::bad_args)?;
    pin_shed_server(deps, resolved)
}

/// **Pin an unqualified `shed:<name>` to the server it was actually found on.**
///
/// The fan-out that locates the shed ([`shed_ssh_target`]) already knows which
/// server answered; throwing that away left `server: None` travelling downstream,
/// where every consumer had to guess again — and the guess is
/// `default_server`-or-first, which on a multi-server config is simply the WRONG
/// host. `sx watch` was the visible casualty: it opened the aggregate event
/// stream against the default server, whose frames never carry the shed, so the
/// filter dropped 100% of them and watch hung silently.
///
/// Resolving ONCE here fixes that class at the source, and costs nothing: every
/// later lookup takes the `@<server>` branch, which is pure config. It is also
/// what makes the session's `target_label` read `shed:<name>@<server>` — the same
/// provenance shape `RcService::launch` stamps (`shed-app/src/rc.rs`), so a
/// session created by `sx` and one created by the desktop are indistinguishable
/// to a watcher.
fn pin_shed_server(deps: &Deps, resolved: Resolved) -> Result<Resolved, VerbError> {
    let Resolved::Shed { name, server: None } = &resolved else {
        return Ok(resolved);
    };
    let target = shed_ssh_target(deps, name, None)?;
    Ok(Resolved::Shed {
        name: name.clone(),
        server: Some(target.server_name),
    })
}

/// The RC argv prefix a REMOTE target is invoked through: a shed always runs
/// the guest helper (one token), a machine runs `sx rc` (two tokens) unless its
/// entry named an `rc_bin` override — see [`target::machine_rc_prefix`].
///
/// The shed-core argv builders take a single `bin` for argv[0], so a call site
/// builds with [`Self::bin`] (the prefix's LAST token) and then [`Self::splice`]s
/// the full prefix back over argv[0] — that keeps a multi-token prefix as
/// separate argv words under the one quoter.
pub struct RemotePrefix(Vec<String>);

impl RemotePrefix {
    /// The `bin` string the shed-core argv builders take: the prefix's last token.
    pub fn bin(&self) -> &str {
        self.0.last().map(String::as_str).unwrap_or_default()
    }

    /// Replace `argv[0]` (built from [`Self::bin`]) with the full prefix.
    pub fn splice(&self, mut argv: Vec<String>) -> Vec<String> {
        argv.splice(0..1, self.0.iter().cloned());
        argv
    }
}

/// The [`RemotePrefix`] for a resolved remote target.
pub fn remote_prefix(deps: &Deps, resolved: &Resolved) -> Result<RemotePrefix, VerbError> {
    match resolved {
        Resolved::Local => Err(VerbError::failed(
            "internal: remote_prefix called for the local target",
        )),
        Resolved::Machine(entry) => Ok(RemotePrefix(target::machine_rc_prefix(entry))),
        Resolved::Shed { .. } => {
            // `SHED_EXT_RC_BIN` is the same dev/proof override `RcService` honors.
            let overridden = deps.env("SHED_EXT_RC_BIN");
            Ok(RemotePrefix(vec![if overridden.is_empty() {
                target::SHED_RC_BIN.to_string()
            } else {
                overridden
            }]))
        }
    }
}

/// How long a one-shot remote op may take end-to-end. Generous because a `create
/// --wait` polls the pane to ready on the far side; ssh's own `ConnectTimeout`
/// ([`crate::ssh::CONNECT_TIMEOUT_SECS`]) is what bounds an unreachable host.
const REMOTE_TIMEOUT: std::time::Duration = std::time::Duration::from_secs(60);

/// Run `remote_argv` on a remote target over SSH, returning its stdout.
///
/// A non-zero exit is mapped through [`shed_core::rc::error_from_exit_with_bin`], so the
/// remote engine's exit-code classes (2 bad args / 3 duplicate slug / 4 not
/// found) survive the SSH hop as the porcelain's own exit code — the property
/// that makes `sx agent … --on machine:x` and `sx agent …` fail the same way.
pub fn remote_exec(
    deps: &Deps,
    resolved: &Resolved,
    remote_argv: &[String],
    stdin: Option<String>,
) -> Result<String, VerbError> {
    let argv = match resolved {
        Resolved::Local => {
            return Err(VerbError::failed(
                "internal: remote_exec called for the local target",
            ))
        }
        Resolved::Machine(entry) => crate::ssh::machine_argv(entry, remote_argv),
        Resolved::Shed { name, server } => {
            let target = shed_ssh_target(deps, name, server.as_deref())?;
            crate::ssh::shed_argv(name, &target, remote_argv)
        }
    };
    let runner = deps.remote_runner();
    let out = deps
        .block_on(async move { runner.run(argv, stdin, REMOTE_TIMEOUT).await })
        .map_err(|e| VerbError::failed(format!("ssh failed: {e}")))?;
    if out.exit_code != 0 {
        // Name the binary that actually exited: on a machine that is `sx`
        // (or whatever the entry's rc_bin configured), not the guest's
        // `shed-ext-rc` — the default the Swift-parity wrapper assumes.
        let bin = remote_argv.first().map(String::as_str).unwrap_or_default();
        let err =
            shed_core::rc::error_from_exit_with_bin(bin, out.exit_code, &out.stderr, &out.stdout);
        return Err(VerbError {
            message: err.to_string(),
            code: rc_error_exit_code(out.exit_code),
        });
    }
    Ok(out.stdout)
}

/// The exit code the porcelain reports for a remote engine's exit code. The
/// contract classes (2/3/4) pass through verbatim; everything else — including a
/// transport failure that never reached the engine — collapses to the generic 1,
/// so a caller can never read an ssh-level 255 as an engine class.
fn rc_error_exit_code(remote: i32) -> i32 {
    match remote {
        2..=4 => remote,
        _ => 1,
    }
}

/// Resolve a shed's SSH endpoint through `shed-app`'s `Backend`.
///
/// With `@<server>` this is pure config (no HTTP at all). Without it the shed
/// must be located: every configured server is asked for its RUNNING sheds and
/// exactly one match is required — an ambiguous name is an error naming the
/// candidates, never a silent pick.
///
/// **Auth posture:** the fan-out's Backend is built WITH the host-agent control-
/// token minter (see [`crate::backend`]), the same way the desktop builds its
/// external-mode one — that is what lets a server enrolled for `auth.mode: mtls`
/// (which by construction holds no static `control_token`) be listed at all.
/// Wiring is gated on the agent actually answering: with no host agent running,
/// the Backend is the plain static-token one and behavior is unchanged. Either
/// way the failure stays per-server, so the `@server` form — pure config plus
/// SSH, no HTTP and no credential — always works, and `sx watch --on shed:…`
/// degrades to probe polling rather than failing (see [`watch`]).
pub fn shed_ssh_target(
    deps: &Deps,
    shed: &str,
    server: Option<&str>,
) -> Result<shed_app::RcTarget, VerbError> {
    if let Some(server) = server {
        // Pure config: no HTTP, so no credential and no agent.
        return crate::backend::config_backend(deps)
            .resolve_rc_target(Some(server))
            .map_err(|e| VerbError::bad_args(format!("shed:{shed}@{server}: {e}")));
    }
    let targets =
        crate::backend::with_backend(deps, async |b| b.rc_targets(None, Some(shed)).await);
    pick_shed_target(shed, targets.into_iter().map(|(_, t)| t).collect())
}

/// The PURE half of [`shed_ssh_target`]: exactly one candidate, or an error that
/// names what to do. Split out so the "which server is this shed on?" decision —
/// the one [`pin_shed_server`] stamps into the target and every later lookup
/// trusts — is unit-testable with no HTTP.
pub fn pick_shed_target(
    shed: &str,
    targets: Vec<shed_app::RcTarget>,
) -> Result<shed_app::RcTarget, VerbError> {
    match targets.len() {
        0 => Err(VerbError::bad_args(format!(
            "no running shed named {shed:?} on any configured server \
             (use --on shed:{shed}@<server> to skip the lookup)"
        ))),
        1 => Ok(targets.into_iter().next().expect("len 1")),
        _ => {
            let servers: Vec<String> = targets.iter().map(|t| t.server_name.clone()).collect();
            Err(VerbError::bad_args(format!(
                "shed {shed:?} exists on more than one server ({}) — \
                 disambiguate with --on shed:{shed}@<server>",
                servers.join(", ")
            )))
        }
    }
}

/// The production remote runner (real ssh subprocesses).
pub fn default_remote_runner() -> RcRunnerRef {
    Arc::new(TokioProcessRunner)
}

#[cfg(test)]
mod tests;
