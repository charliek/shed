//! shed-broker-harness — a TEST-ONLY process that runs the desktop app's embedded
//! credential broker, so the wire harness (`tests/broker-wire`) can drive it as a
//! process: a config file in, the bus and the audit file out, a control socket for
//! status, approval frames and decisions.
//!
//! ```text
//! shed-broker-harness --config <path> --log-file <path> --control <socket>
//! ```
//!
//! It is the app's broker exactly (`shed_app::load_or_synthesize`, then
//! `EmbeddedBroker::new` + `spawn`), minus the app's Coordinator: the control
//! socket (`control.rs`) stands where the Coordinator sits. It lists no keys at
//! spawn, as the app does not.
//!
//! Exit codes: 0 after SIGTERM/SIGINT once the broker has drained every server group
//! (`harness drained` in the log), 3 when the drain did not finish within five
//! seconds (`harness drain timed out`), 1 when the config does not load, the broker
//! does not spawn or the control socket cannot be bound, 2 on a usage error.
//!
//! It ships in no release and nothing depends on it (`tests/not_shipped.rs`). It
//! offers no compatibility promise: its interface is whatever the harness needs.

mod control;
mod status;

use std::path::{Path, PathBuf};
use std::process;
use std::sync::Arc;
use std::time::Duration;

use serde_json::{json, Value};
use tokio::sync::watch;

use shed_app::{load_or_synthesize, EmbeddedBroker};
use shed_broker::bus::{BusLog, FileBusLog};
use shed_broker::config::expand_tilde;
use shed_broker::status::now_rfc3339;

/// The harness's name: the `version` its status reports, and the prefix of its stderr
/// lines.
const HARNESS_NAME: &str = "shed-broker-harness";

/// How long a SIGTERM waits for the broker to drain before exiting 3.
const DRAIN_BOUND: Duration = Duration::from_secs(5);

const USAGE: &str =
    "usage: shed-broker-harness --config <path> --log-file <path> --control <socket>";

fn main() {
    let args: Vec<String> = std::env::args().skip(1).collect();
    process::exit(run(&args));
}

/// The parsed command line: all three are required.
#[derive(Debug, PartialEq, Eq)]
struct Args {
    config: String,
    log_file: String,
    control: PathBuf,
}

/// Parse `--name value` or `--name=value` for each of the three flags, in any order.
fn parse_args(args: &[String]) -> Result<Args, String> {
    let (mut config, mut log_file, mut control) = (None, None, None);
    let mut rest = args.iter();
    while let Some(arg) = rest.next() {
        let (name, inline) = match arg.split_once('=') {
            Some((name, value)) => (name, Some(value.to_string())),
            None => (arg.as_str(), None),
        };
        let slot: &mut Option<String> = match name {
            "--config" => &mut config,
            "--log-file" => &mut log_file,
            "--control" => &mut control,
            _ => return Err(format!("unknown argument {arg:?}")),
        };
        let value = match inline {
            Some(value) => value,
            None => rest
                .next()
                .cloned()
                .ok_or_else(|| format!("{name} needs a value"))?,
        };
        if value.is_empty() {
            return Err(format!("{name} needs a value"));
        }
        *slot = Some(value);
    }
    Ok(Args {
        config: config.ok_or("--config is required")?,
        log_file: log_file.ok_or("--log-file is required")?,
        control: PathBuf::from(control.ok_or("--control is required")?),
    })
}

fn run(args: &[String]) -> i32 {
    let args = match parse_args(args) {
        Ok(args) => args,
        Err(e) => {
            eprintln!("{HARNESS_NAME}: {e}\n{USAGE}");
            return 2;
        }
    };
    let runtime = match tokio::runtime::Builder::new_multi_thread()
        .enable_all()
        .build()
    {
        Ok(rt) => rt,
        Err(e) => {
            eprintln!("{HARNESS_NAME}: failed to start async runtime: {e}");
            return 1;
        }
    };
    runtime.block_on(serve(args))
}

/// The config path the status reports: tilde-expanded and absolute.
fn status_config_path(config: &str) -> String {
    let expanded = expand_tilde(config);
    std::path::absolute(Path::new(&expanded))
        .unwrap_or_else(|_| PathBuf::from(&expanded))
        .to_string_lossy()
        .into_owned()
}

async fn serve(args: Args) -> i32 {
    // The app's own loader: a missing file is the app's synthesized default; a file
    // that does not load stops here, as the app's broker would fail closed.
    let cfg = match load_or_synthesize(&args.config) {
        Ok(cfg) => cfg.into_config(),
        Err(e) => {
            eprintln!("{HARNESS_NAME}: {e}");
            return 1;
        }
    };
    let log: Arc<dyn BusLog> = Arc::new(FileBusLog::new(&args.log_file));
    log.info(&format!("starting {HARNESS_NAME} config={}", args.config));
    let started_at = now_rfc3339();
    let config_path = status_config_path(&args.config);

    let (agent, events) = EmbeddedBroker::new(cfg.clone(), log.clone());
    let agent = Arc::new(agent);
    if let Err(e) = agent.spawn().await {
        log.error(&format!("broker did not start error={e}"));
        eprintln!("{HARNESS_NAME}: {e}");
        return 1;
    }

    let hub = Arc::new(control::Hub::default());
    tokio::spawn(control::pump(events, hub.clone()));

    let status = {
        let agent = agent.clone();
        Box::new(move || -> Value {
            let machine = agent
                .machine_identity()
                .unwrap_or_else(|| Err("not loaded".to_string()));
            let snapshot = status::build_live_status(
                &cfg,
                &config_path,
                &started_at,
                HARNESS_NAME,
                agent.health(),
                &machine,
                agent.ssh_signing_status(),
            );
            serde_json::to_value(snapshot)
                .unwrap_or_else(|e| json!({"error": format!("status: {e}")}))
        })
    };

    let mut signals = Signals::install();
    let listener = match control::bind(&args.control) {
        Ok(listener) => listener,
        Err(e) => {
            log.error(&format!(
                "control socket not bound path={} error={e}",
                args.control.display()
            ));
            eprintln!(
                "{HARNESS_NAME}: control socket {}: {e}",
                args.control.display()
            );
            agent.shutdown_and_wait(DRAIN_BOUND).await;
            return 1;
        }
    };
    let control = Arc::new(control::Control::new(hub, status, agent.responder()));
    let (stop_tx, stop_rx) = watch::channel(false);
    let server = tokio::spawn(control::serve(listener, control, stop_rx));
    log.info(&format!("harness ready control={}", args.control.display()));

    signals.wait().await;
    let drained = agent.shutdown_and_wait(DRAIN_BOUND).await;
    let _ = stop_tx.send(true);
    let _ = server.await;
    let _ = std::fs::remove_file(&args.control);
    if drained {
        log.info("harness drained");
        0
    } else {
        log.error("harness drain timed out");
        3
    }
}

/// The SIGTERM and SIGINT handlers. Installed before the control socket exists, so
/// a signal sent the moment the socket appears is still handled (drained, socket
/// removed), never left to the default action.
struct Signals {
    term: tokio::signal::unix::Signal,
    interrupt: tokio::signal::unix::Signal,
}

impl Signals {
    fn install() -> Self {
        use tokio::signal::unix::{signal, SignalKind};
        Self {
            term: signal(SignalKind::terminate()).expect("install SIGTERM handler"),
            interrupt: signal(SignalKind::interrupt()).expect("install SIGINT handler"),
        }
    }

    /// Resolve when SIGTERM or SIGINT arrives (or has arrived since `install`).
    async fn wait(&mut self) {
        tokio::select! {
            _ = self.term.recv() => {},
            _ = self.interrupt.recv() => {},
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn owned(items: &[&str]) -> Vec<String> {
        items.iter().map(|s| s.to_string()).collect()
    }

    #[test]
    fn takes_the_three_flags_in_either_form_and_any_order() {
        let want = Args {
            config: "/c.yaml".into(),
            log_file: "/l.log".into(),
            control: PathBuf::from("/s/control.sock"),
        };
        for args in [
            owned(&[
                "--config",
                "/c.yaml",
                "--log-file",
                "/l.log",
                "--control",
                "/s/control.sock",
            ]),
            owned(&[
                "--control=/s/control.sock",
                "--config=/c.yaml",
                "--log-file=/l.log",
            ]),
        ] {
            assert_eq!(parse_args(&args).unwrap(), want, "{args:?}");
        }
    }

    #[test]
    fn refuses_a_missing_flag_value_or_an_unknown_argument() {
        for (args, said) in [
            (
                owned(&["--log-file", "/l", "--control", "/s"]),
                "--config is required",
            ),
            (
                owned(&["--config", "/c", "--control", "/s"]),
                "--log-file is required",
            ),
            (
                owned(&["--config", "/c", "--log-file", "/l"]),
                "--control is required",
            ),
            (owned(&["--config"]), "--config needs a value"),
            (owned(&["--config="]), "--config needs a value"),
            (owned(&["-config", "/c"]), "unknown argument"),
            (owned(&["status"]), "unknown argument"),
        ] {
            let err = parse_args(&args).unwrap_err();
            assert!(err.contains(said), "{args:?}: {err}");
        }
    }

    #[test]
    fn the_status_config_path_is_absolute() {
        assert_eq!(status_config_path("/a/b.yaml"), "/a/b.yaml");
        assert!(Path::new(&status_config_path("rel.yaml")).is_absolute());
    }
}
