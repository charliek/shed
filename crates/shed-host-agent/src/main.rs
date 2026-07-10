//! shed-host-agent — the host-side credential-broker daemon (Rust port), slice 0.
//!
//! This slice ports the daemon's scaffold + public CLI surface: the `version` and
//! `status` subcommands, the read-only status UDS server, and a minimal
//! LiveStatus-scoped config reader — all wire-compatible with the Go
//! `cmd/shed-host-agent` (`main.go` / `status.go` / `status_server.go` /
//! `sockets.go`). Surface B (the shed-server plugin bus, `bus.rs`) adds the
//! single-server `ssh-agent` subscribe + ping/pong responder. The credential
//! minter, the sign/aws/docker backends, multi-server discovery, and audit
//! logging are later slices; in multi-server (`discovery:`) mode the single-server
//! bus stays off, matching the Go daemon's `cfg.Discovery == nil` gate.

mod bus;
mod config;
#[cfg(feature = "desktop-forwarding")]
mod desktop;
mod sockets;
mod status;
mod version;

use std::io::{self, Write};
use std::path::Path;
use std::process;
use std::sync::Arc;

use config::HostAgentConfig;
use sockets::{bind_unix_socket, status_socket_path};
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

    // Surface B: in single-server mode (no `discovery:` block) the message-bus
    // daemon connects to the single `server:` URL and answers ssh-agent pings. In
    // multi-server (`discovery:`) mode it stays off — matching Go's
    // `cfg.Discovery == nil` gate — since discovery/backends are later slices.
    let bus_server = cfg.is_single_server().then(|| cfg.server.clone());
    if let Some(url) = &bus_server {
        log.info(&format!("message bus: single-server mode server={url}"));
    }

    let version = full_info();
    let status_path = status_socket_path();
    let status_listener = bind_unix_socket("status socket", &status_path, &mut log);

    // The desktop approval channel (feature-gated). Bind its socket + build the
    // server here so the status snapshot can report its live consumer info. The
    // real control-token minter is a later slice — a stub keeps `token.get`
    // answerable end-to-end for now.
    #[cfg(feature = "desktop-forwarding")]
    let (desktop_server, desktop_listener, desktop_path) = {
        let server = desktop::DesktopServer::new(
            version.clone(),
            cfg.gate_namespaces(),
            cfg.approval_timeout(),
            Some(Arc::new(desktop::StubControlMinter)),
        );
        let path = sockets::desktop_socket_path();
        let listener = bind_unix_socket("desktop", &path, &mut log);
        (server, listener, path)
    };

    // The status snapshot is recomputed per connection (fresh written_at/pid). It
    // reports the desktop consumer identity when the feature is on, else no consumer.
    let health: Arc<dyn Fn() -> LiveStatus + Send + Sync> = {
        let cfg = cfg.clone();
        let config_path = resolved_config_path;
        let started_at = started_at.clone();
        let version = version.clone();
        #[cfg(feature = "desktop-forwarding")]
        let desktop_server = desktop_server.clone();
        Arc::new(move || {
            #[cfg(feature = "desktop-forwarding")]
            let consumer = desktop_server.consumer_info();
            #[cfg(not(feature = "desktop-forwarding"))]
            let consumer = None;
            build_live_status(&cfg, &config_path, &started_at, &version, consumer)
        })
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
        // One shutdown signal shared by every socket server: a task flips the watch
        // on SIGTERM/SIGINT; each server's shutdown future resolves when it flips.
        let (shutdown_tx, shutdown_rx) = tokio::sync::watch::channel(false);
        tokio::spawn(async move {
            wait_for_shutdown().await;
            let _ = shutdown_tx.send(true);
        });

        let mut tasks: Vec<tokio::task::JoinHandle<()>> = Vec::new();

        if let Some(listener) = status_listener.and_then(into_tokio_listener) {
            let rx = shutdown_rx.clone();
            tasks.push(tokio::spawn(async move {
                serve_status_socket(listener, status_path, health, wait_shutdown(rx)).await;
            }));
        }

        #[cfg(feature = "desktop-forwarding")]
        if let Some(listener) = desktop_listener.and_then(into_tokio_listener) {
            let rx = shutdown_rx.clone();
            let server = desktop_server.clone();
            tasks.push(tokio::spawn(async move {
                server
                    .serve(listener, desktop_path, wait_shutdown(rx))
                    .await;
            }));
        }

        // The message bus (surface B). Shares the same shutdown watch, so a
        // SIGTERM/SIGINT tears the subscribe loop + ping responder down cleanly
        // alongside the socket servers.
        if let Some(server_url) = bus_server {
            let rx = shutdown_rx.clone();
            let bus_log: Arc<dyn bus::BusLog> = Arc::new(bus::FileBusLog::new(log_file));
            tasks.push(tokio::spawn(async move {
                bus::run_single_server_bus(server_url, rx, bus_log).await;
            }));
        }

        if tasks.is_empty() {
            // No sockets bound: still behave like a daemon (wait for a signal).
            wait_shutdown(shutdown_rx).await;
        } else {
            for task in tasks {
                let _ = task.await;
            }
        }
    });

    log.info("stopped");
    0
}

/// Resolve when the shared shutdown watch is flipped (or its sender is dropped).
async fn wait_shutdown(mut rx: tokio::sync::watch::Receiver<bool>) {
    let _ = rx.wait_for(|flagged| *flagged).await;
}

/// Flip a bound blocking std listener to non-blocking and adopt it into the tokio
/// runtime. `None` on the (should-not-happen) conversion failure, so the caller
/// skips that socket. Shared by the status + desktop socket setup.
fn into_tokio_listener(
    std_listener: std::os::unix::net::UnixListener,
) -> Option<tokio::net::UnixListener> {
    let _ = std_listener.set_nonblocking(true);
    tokio::net::UnixListener::from_std(std_listener).ok()
}

/// Resolve the absolute config path the daemon loaded, surfaced verbatim in
/// `status`. Best-effort, lexical (no symlink resolution) — mirrors Go's
/// `filepath.Abs(expandTilde(path))`, which cleans `.`/`..` components.
fn resolve_config_path(config_path: &str) -> String {
    let expanded = config::expand_tilde(config_path);
    let p = Path::new(&expanded);
    let abs = if p.is_absolute() {
        p.to_path_buf()
    } else {
        match std::env::current_dir() {
            Ok(cwd) => cwd.join(p),
            Err(_) => p.to_path_buf(),
        }
    };
    lexical_clean(&abs).to_string_lossy().into_owned()
}

/// Lexically clean a path (resolve `.`/`..` without touching the filesystem),
/// matching Go's `filepath.Clean` for the cases `filepath.Abs` produces: `.` is
/// dropped, `..` pops a preceding normal component, and `..` at the root is
/// discarded (`/..` => `/`). No symlink resolution (Go's `Abs` doesn't either).
fn lexical_clean(p: &Path) -> std::path::PathBuf {
    use std::path::{Component, PathBuf};
    let mut stack: Vec<Component> = Vec::new();
    for comp in p.components() {
        match comp {
            Component::CurDir => {}
            Component::ParentDir => match stack.last() {
                Some(Component::Normal(_)) => {
                    stack.pop();
                }
                // `/..` collapses to `/`; a leading `..` in a relative path is kept.
                Some(Component::RootDir) | Some(Component::Prefix(_)) => {}
                _ => stack.push(comp),
            },
            c => stack.push(c),
        }
    }
    if stack.is_empty() {
        return PathBuf::from(".");
    }
    stack.iter().collect()
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
    fn lexical_clean_resolves_dot_and_dotdot() {
        use std::path::PathBuf;
        let clean = |p: &str| lexical_clean(Path::new(p)).to_string_lossy().into_owned();
        assert_eq!(clean("/a/b/../c"), "/a/c");
        assert_eq!(clean("/a/./b"), "/a/b");
        assert_eq!(clean("/a/b/.."), "/a");
        // `..` at the root collapses to the root (matches Go filepath.Clean).
        assert_eq!(clean("/.."), "/");
        assert_eq!(clean("/../.."), "/");
        // A relative leading `..` is preserved (no cwd to pop into lexically).
        assert_eq!(lexical_clean(Path::new("../x")), PathBuf::from("../x"));
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
