//! `sx attach <slug> [--on <target>] [--print]` — hand the terminal over.
//!
//! Attach **execs**; it does not spawn-and-wait. `execvp` replaces this process
//! with `tmux`/`ssh`, so the agent's pane inherits the real controlling terminal
//! (window size, signals, job control, Ctrl-B passthrough) and `sx` leaves no
//! parent between the operator and the session. A spawned child would work until
//! the first resize or the first `SIGWINCH`.
//!
//! `--print` emits the same command instead of running it — for a script, a
//! window-manager binding, or a "what would you do?" check.

use std::os::unix::process::CommandExt as _;

use crate::args::Parsed;
use crate::cli::Deps;
use crate::porcelain::{resolve_target, shed_ssh_target, VerbError, VerbResult};
use crate::ssh;
use crate::target::Resolved;

/// The argv `sx attach` execs for a target. Pure — the whole verb is one table
/// of these, which is what makes it testable without handing over a terminal.
pub fn attach_argv(deps: &Deps, resolved: &Resolved, slug: &str) -> Result<Vec<String>, VerbError> {
    // **Validate the subject BEFORE any argv exists.** `rc-<slug>` is
    // interpolated into a tmux target and (remotely) into a shell command line,
    // so an unconstrained subject is an injection vector. The grammar is the
    // engine's own — the same one `create --slug` enforces — so a slug that can
    // be attached is exactly a slug that could have been created.
    if !shed_core::rc_agents::valid_caller_slug(slug) {
        return Err(VerbError::bad_args(format!(
            "invalid slug {slug:?}: expected 1-32 lowercase alphanumerics with \
             inner hyphens (no leading or trailing hyphen)"
        )));
    }
    // `rc-<slug>`, the one attach/kill target.
    let session = shed_core::rc::tmux_name(slug);
    let tmux_attach = [
        "tmux".to_string(),
        "attach".to_string(),
        "-t".to_string(),
        session.clone(),
    ];
    Ok(match resolved {
        // Local: straight to tmux, no ssh in the way.
        Resolved::Local => tmux_attach.to_vec(),
        Resolved::Machine(entry) => ssh::machine_tty_argv(entry, &tmux_attach),
        Resolved::Shed { name, server } => {
            let target = shed_ssh_target(deps, name, server.as_deref())?;
            ssh::shed_tty_argv(name, &target, &session)
        }
    })
}

pub fn run(deps: &Deps, slug: &str, p: &Parsed) -> VerbResult {
    let resolved = resolve_target(deps, p)?;
    let argv = attach_argv(deps, &resolved, slug)?;
    if p.flag("print") {
        deps.write_out(&format!("{}\n", ssh::display_line(&argv)));
        return Ok(0);
    }
    let (bin, rest) = argv.split_first().expect("attach argv is never empty");
    // `exec` only ever RETURNS on failure (it does not fork), so anything past
    // this line is the error path.
    let err = std::process::Command::new(bin).args(rest).exec();
    Err(VerbError::failed(format!("exec {bin}: {err}")))
}
