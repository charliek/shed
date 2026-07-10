//! shed-host-agent — the host-side credential-broker daemon (Rust port), slice 0.
//!
//! This slice ports the daemon's scaffold + public CLI surface: the `version` and
//! `status` subcommands, the read-only status UDS server, and a minimal
//! LiveStatus-scoped config reader — all wire-compatible with the Go
//! `cmd/shed-host-agent` (`main.go` / `status.go` / `status_server.go` /
//! `sockets.go`). The desktop approval channel, the credential minter, the message
//! bus handlers, discovery, and audit logging are later slices; here a daemon with
//! a valid config simply serves its status socket and waits for SIGTERM/SIGINT.

mod config;
mod sockets;
mod status;
mod version;

use std::io::{self, Write};
use std::path::Path;
use std::process;
use std::sync::Arc;

use config::HostAgentConfig;
use sockets::status_socket_path;
use status::{build_live_status, now_rfc3339, run_status, serve_status_socket, LiveStatus};
use version::full_info;

/// The default config path (tilde-expanded at load) — matches the Go daemon's
/// `-config` default.
const DEFAULT_CONFIG_PATH: &str = "~/.config/shed/extensions.yaml";

fn main() {
    let args: Vec<String> = std::env::args().skip(1).collect();
    process::exit(run(&args));
}

/// Dispatch the parsed command. Split out from `main` so it returns an exit code
/// (testable) instead of calling `process::exit` directly.
fn run(args: &[String]) -> i32 {
    match parse_args(args) {
        Ok(Command::Version) => {
            println!("{}", full_info());
            0
        }
        Ok(Command::Status { json_out }) => run_status(json_out, &mut io::stdout().lock()),
        Ok(Command::Daemon {
            config_path,
            log_file,
        }) => run_daemon(&config_path, &log_file),
        Err(err) => {
            eprintln!("{}", err.message);
            err.code
        }
    }
}

/// A parsed invocation.
#[derive(Debug)]
enum Command {
    Version,
    Status {
        json_out: bool,
    },
    Daemon {
        config_path: String,
        log_file: String,
    },
}

/// A usage error: a message to print to stderr and the exit code to return.
#[derive(Debug)]
struct ArgError {
    message: String,
    code: i32,
}

/// Parse argv (without argv[0]), mirroring Go's `flag` package + subcommand
/// dispatch in `main.go`: leading `-config`/`-log-file` flags (both `-x` and `--x`,
/// `-x=v` and `-x v`) stop at the first non-flag argument, which is the subcommand.
fn parse_args(args: &[String]) -> Result<Command, ArgError> {
    let mut config_path = DEFAULT_CONFIG_PATH.to_string();
    let mut log_file = String::new();
    let mut i = 0;
    while i < args.len() {
        let a = &args[i];
        // A non-flag positional ends flag parsing (Go's flag.Parse stops at the
        // first non-flag argument) → it is the subcommand.
        if !a.starts_with('-') {
            return parse_subcommand(a, &args[i + 1..], config_path, log_file);
        }
        if let Some(v) = take_flag(a, "config", args, &mut i)? {
            config_path = v;
            continue;
        }
        if let Some(v) = take_flag(a, "log-file", args, &mut i)? {
            log_file = v;
            continue;
        }
        // Unrecognized leading flag: Go's flag package prints an error + exits 2.
        return Err(ArgError {
            message: format!("flag provided but not defined: {a}"),
            code: 2,
        });
    }
    // No subcommand → daemon mode with the (possibly overridden) config.
    Ok(Command::Daemon {
        config_path,
        log_file,
    })
}

/// If `arg` is `-name`/`--name` (value in the next argv element) or
/// `-name=v`/`--name=v`, consume it and return the value. `Ok(None)` means `arg`
/// is not this flag; `Err` means the flag needs a value but none follows.
fn take_flag(
    arg: &str,
    name: &str,
    args: &[String],
    i: &mut usize,
) -> Result<Option<String>, ArgError> {
    let stripped = arg
        .strip_prefix("--")
        .or_else(|| arg.strip_prefix('-'))
        .unwrap_or(arg);
    if stripped == name {
        let Some(value) = args.get(*i + 1) else {
            return Err(ArgError {
                message: format!("flag needs an argument: -{name}"),
                code: 2,
            });
        };
        let value = value.clone();
        *i += 2;
        return Ok(Some(value));
    }
    if let Some(rest) = stripped
        .strip_prefix(name)
        .and_then(|r| r.strip_prefix('='))
    {
        *i += 1;
        return Ok(Some(rest.to_string()));
    }
    Ok(None)
}

/// Dispatch the subcommand token. `version` and `status` are handled; any other
/// positional falls through to daemon mode (Go ignores extra positionals there).
fn parse_subcommand(
    name: &str,
    rest: &[String],
    config_path: String,
    log_file: String,
) -> Result<Command, ArgError> {
    match name {
        "version" => Ok(Command::Version),
        "status" => {
            let mut json_out = false;
            for a in rest {
                match a.as_str() {
                    "--json" | "-json" => json_out = true,
                    "--live" | "-live" => {
                        return Err(ArgError {
                            message:
                                "status: --live was removed; `status` now always queries the running agent"
                                    .to_string(),
                            code: 2,
                        });
                    }
                    other => {
                        return Err(ArgError {
                            message: format!("status: unknown argument {other:?}"),
                            code: 2,
                        });
                    }
                }
            }
            Ok(Command::Status { json_out })
        }
        _ => Ok(Command::Daemon {
            config_path,
            log_file,
        }),
    }
}

/// Run the daemon: load config, bind + serve the status socket, and wait for a
/// SIGTERM/SIGINT to shut down (unlinking the socket) and exit 0. On a config load
/// error, log it and return 1 (matches `main.go:83-85`).
fn run_daemon(config_path: &str, log_file: &str) -> i32 {
    let mut log = Log::new(log_file);
    log.info(&format!("starting shed-host-agent version={}", full_info()));
    let started_at = now_rfc3339();

    let cfg = match HostAgentConfig::load(config_path) {
        Ok(c) => c,
        Err(e) => {
            log.error(&format!(
                "failed to load config path={config_path} error={e}"
            ));
            return 1;
        }
    };
    let resolved_config_path = resolve_config_path(config_path);
    log.info(&format!(
        "approval policies ssh={} aws={} docker={}",
        cfg.effective_policy(config::NS_SSH_AGENT),
        cfg.effective_policy(config::NS_AWS_CREDENTIALS),
        cfg.effective_policy(config::NS_DOCKER_CREDENTIALS),
    ));

    let version = full_info();
    let status_path = status_socket_path();
    let listener = status::bind_status_listener(&status_path, &mut log);

    // The status snapshot is recomputed per connection (fresh written_at/pid).
    let health: Arc<dyn Fn() -> LiveStatus + Send + Sync> = {
        let cfg = cfg.clone();
        let config_path = resolved_config_path;
        let started_at = started_at.clone();
        let version = version.clone();
        Arc::new(move || build_live_status(&cfg, &config_path, &started_at, &version))
    };

    // Build the tokio runtime only for the daemon; the version/status subcommands
    // stay synchronous and never spin up a runtime.
    let runtime = match tokio::runtime::Builder::new_multi_thread()
        .enable_all()
        .build()
    {
        Ok(rt) => rt,
        Err(e) => {
            log.error(&format!("failed to start async runtime error={e}"));
            return 1;
        }
    };

    runtime.block_on(async move {
        match listener {
            Some(std_listener) => {
                let _ = std_listener.set_nonblocking(true);
                match tokio::net::UnixListener::from_std(std_listener) {
                    Ok(listener) => {
                        serve_status_socket(listener, status_path, health, wait_for_shutdown())
                            .await;
                    }
                    // Conversion failure (should not happen): just wait for a signal.
                    Err(_) => wait_for_shutdown().await,
                }
            }
            // Status socket unavailable: still behave like a daemon (wait for signal).
            None => wait_for_shutdown().await,
        }
    });

    log.info("stopped");
    0
}

/// Resolve the absolute config path the daemon loaded, surfaced verbatim in
/// `status`. Best-effort, lexical (no symlink resolution) — mirrors Go's
/// `filepath.Abs(expandTilde(path))`.
fn resolve_config_path(config_path: &str) -> String {
    let expanded = config::expand_tilde(config_path);
    if Path::new(&expanded).is_absolute() {
        return expanded;
    }
    match std::env::current_dir() {
        Ok(cwd) => cwd.join(&expanded).to_string_lossy().into_owned(),
        Err(_) => expanded,
    }
}

/// Resolve when either SIGTERM or SIGINT is received.
async fn wait_for_shutdown() {
    use tokio::signal::unix::{signal, SignalKind};
    let mut term = signal(SignalKind::terminate()).expect("install SIGTERM handler");
    let mut interrupt = signal(SignalKind::interrupt()).expect("install SIGINT handler");
    tokio::select! {
        _ = term.recv() => {},
        _ = interrupt.recv() => {},
    }
}

/// The daemon's operational log sink: stderr, or an append-only file when
/// `-log-file` is set (slice 0 has no rotation — the Go daemon's lumberjack
/// rotation is a later concern). The operational log is not a differential target.
pub(crate) struct Log {
    writer: Box<dyn Write + Send>,
}

impl Log {
    fn new(log_file: &str) -> Log {
        let writer: Box<dyn Write + Send> = if log_file.is_empty() {
            Box::new(io::stderr())
        } else {
            match std::fs::OpenOptions::new()
                .create(true)
                .append(true)
                .open(log_file)
            {
                Ok(f) => Box::new(f),
                Err(_) => Box::new(io::stderr()),
            }
        };
        Log { writer }
    }

    pub(crate) fn info(&mut self, msg: &str) {
        let _ = writeln!(self.writer, "INFO  {msg}");
    }
    pub(crate) fn warn(&mut self, msg: &str) {
        let _ = writeln!(self.writer, "WARN  {msg}");
    }
    pub(crate) fn error(&mut self, msg: &str) {
        let _ = writeln!(self.writer, "ERROR {msg}");
    }
}

/// Serialize the tests that mutate process-global environment variables (Rust runs
/// tests in-process and in parallel, so `set_var`/`remove_var` would otherwise
/// race across modules). Poisoning is ignored — the lock only orders access.
#[cfg(test)]
pub(crate) fn env_lock() -> std::sync::MutexGuard<'static, ()> {
    static ENV_LOCK: std::sync::Mutex<()> = std::sync::Mutex::new(());
    ENV_LOCK
        .lock()
        .unwrap_or_else(std::sync::PoisonError::into_inner)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn owned(items: &[&str]) -> Vec<String> {
        items.iter().map(|s| s.to_string()).collect()
    }

    #[test]
    fn parses_version_subcommand() {
        assert!(matches!(
            parse_args(&owned(&["version"])).unwrap(),
            Command::Version
        ));
        // A leading -config before `version` is accepted and ignored by version.
        assert!(matches!(
            parse_args(&owned(&["-config", "/x.yaml", "version"])).unwrap(),
            Command::Version
        ));
    }

    #[test]
    fn parses_status_flags() {
        assert!(matches!(
            parse_args(&owned(&["status"])).unwrap(),
            Command::Status { json_out: false }
        ));
        assert!(matches!(
            parse_args(&owned(&["status", "--json"])).unwrap(),
            Command::Status { json_out: true }
        ));
        assert!(matches!(
            parse_args(&owned(&["status", "-json"])).unwrap(),
            Command::Status { json_out: true }
        ));
    }

    #[test]
    fn status_removed_live_flag_exits_2() {
        let err = parse_args(&owned(&["status", "--live"])).unwrap_err();
        assert_eq!(err.code, 2);
        assert!(err.message.contains("--live was removed"));
    }

    #[test]
    fn status_unknown_arg_exits_2_with_quotes() {
        let err = parse_args(&owned(&["status", "--bogus"])).unwrap_err();
        assert_eq!(err.code, 2);
        assert_eq!(err.message, "status: unknown argument \"--bogus\"");
    }

    #[test]
    fn daemon_default_and_flag_forms() {
        match parse_args(&[]).unwrap() {
            Command::Daemon {
                config_path,
                log_file,
            } => {
                assert_eq!(config_path, DEFAULT_CONFIG_PATH);
                assert_eq!(log_file, "");
            }
            _ => panic!("expected daemon"),
        }
        // Both `-config X` and `--config=X`, plus `-log-file`.
        for args in [
            owned(&["-config", "/a.yaml", "-log-file", "/l.log"]),
            owned(&["--config=/a.yaml", "--log-file=/l.log"]),
        ] {
            match parse_args(&args).unwrap() {
                Command::Daemon {
                    config_path,
                    log_file,
                } => {
                    assert_eq!(config_path, "/a.yaml");
                    assert_eq!(log_file, "/l.log");
                }
                _ => panic!("expected daemon"),
            }
        }
    }

    #[test]
    fn unknown_leading_flag_exits_2() {
        let err = parse_args(&owned(&["--nope"])).unwrap_err();
        assert_eq!(err.code, 2);
        assert!(err.message.contains("flag provided but not defined"));
    }
}
