//! `sx` — the Rust porcelain for RC agent sessions (plan 009).
//!
//! Today it exposes ONE namespace: `sx rc <subcommand>`, the ported one-shot RC
//! engine, wire-compatible with `shed-machine-rc <subcommand>` under the
//! comparison model the `tests/rc-parity` differential harness enforces. The
//! porcelain verbs proper (`sx agent`, `sx plan`, `sx ls`, `sx watch`,
//! `sx attach`, `sx kill`, with `--on local|machine:<m>|shed:<s>`) land in C7.
//!
//! House style: hand-rolled argument parsing (no clap anywhere in this
//! workspace), a pure parse layer ([`args`]) feeding a dispatch layer ([`cli`])
//! whose every side effect is injected, so both are unit-testable without a
//! process.
//!
//! **Runtime requirement:** tmux ≥ 3.2 (`new-session -e`, how the `SHED_RC_*`
//! session metadata is stamped). The Go engine has the same implicit floor and
//! does not check it either; the parity CI job asserts the installed version.

mod args;
mod cli;

use shed_app::rc_engine::tmux::ExecRunner;

fn main() {
    let argv: Vec<String> = std::env::args().skip(1).collect();
    let runner = ExecRunner;
    let deps = cli::Deps::production(&runner);
    std::process::exit(cli::run(&deps, &argv));
}
