//! shed-host-agent — the host-side credential-broker daemon (Rust port), slice 0.
//!
//! This slice ports the daemon's scaffold + public CLI surface: the `version` and
//! `status` subcommands, the read-only status UDS server, and a minimal
//! LiveStatus-scoped config reader — all wire-compatible with the Go
//! `cmd/shed-host-agent` (`main.go` / `status.go` / `status_server.go` /
//! `sockets.go`). Surface B (the shed-server plugin bus, `bus.rs`) subscribes to
//! `ssh-agent` and answers `ping` + the cross-surface gated **`sign`** flow: a bus
//! sign request runs the approval gate (`approval.rs` / the desktop gate), signs
//! with the local-keys SSH backend (`ssh_backend.rs` — ed25519 + rsa + ecdsa,
//! resolved from `ssh.mode` at startup), and records an audit entry (`audit.rs`)
//! that fans out to the desktop app. The credential minter, the agent-forward
//! backend, the aws/docker backends, the ssh `list`/`status` ops, and discovery are
//! later slices; in multi-server (`discovery:`) mode the single-server bus stays
//! off, matching the Go daemon's `cfg.Discovery == nil` gate.

// The broker core lives in the sibling `shed-broker` crate; this bin is the daemon
// shell. The only modules that stay here are the daemon-only concerns:
// The Surface-A desktop approval channel + its wire codec — gated by
// `desktop-forwarding` (a headless build drops these MODULES only; the broker core
// is always linked).
#[cfg(feature = "desktop-forwarding")]
mod desktop;
#[cfg(feature = "desktop-forwarding")]
mod desktop_protocol;
// The socket bind ceremony (touches `Log`) + the status UDS server / `status` CLI
// client (only the daemon serves that socket). Path resolution + liveness probes +
// the LiveStatus snapshot builder live in `shed_broker::{sockets,status}`.
mod socket_bind;
mod status_server;
mod version;

use std::io::{self, Write};
use std::path::Path;
use std::process;
use std::sync::Arc;

// Bring the broker-core module names into scope so the daemon wiring below reads
// against them (`config::HostAgentConfig`, `bus::FileBusLog`, `supervisor::…`, …) —
// the same bare paths the pre-extraction single-crate daemon used.
use shed_broker::{
    approval, audit, aws_backend, bus, config, docker_backend, minter, sockets, ssh_backend,
    supervisor, watcher,
};
use shed_broker::config::HostAgentConfig;
use shed_broker::status::{build_live_status, now_rfc3339, LiveStatus};

use crate::socket_bind::bind_unix_socket;
use crate::status_server::{run_status, serve_status_socket};
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

    // Resolve the SSH backend UNCONDITIONALLY at startup — after config load, BEFORE
    // any socket binds — mirroring Go's `main.go:114` (`ResolveSSHBackend` runs before
    // the desktop/status sockets). A resolve error (unknown `ssh.mode`, or an explicit
    // `agent-forward` with `$SSH_AUTH_SOCK` unset) is FATAL: log + return 1,
    // matching Go's `os.Exit(1)`. Resolving here — not inside the single-server bus
    // block — means a multi-server (`discovery:`) config also validates the mode and
    // loads keys at startup, even though its bus stays off.
    let (ssh_backend, ssh_warnings) =
        match ssh_backend::resolve_ssh_backend_from_env(cfg.ssh_mode()) {
            Ok(pair) => pair,
            Err(e) => {
                log.error(&format!("failed to initialize SSH backend error={e}"));
                return 1;
            }
        };
    for warning in &ssh_warnings {
        log.warn(warning);
    }
    // Enumerate keys at startup ONLY for local-keys (a free local file read, matching
    // Go's `newLocalKeysBackend`, which logs each loaded file). For agent-forward, Go
    // NEVER probes the forwarded agent at startup — it only logs "auto-detected …
    // agent-forward". Listing here would issue an extra REQUEST_IDENTITIES to the host
    // agent on every daemon start that Go does not, a wire-visible divergence the
    // agent-forward transcript differential (test_ssh_backend.py) catches. So gate the
    // enumeration on the mode.
    let ssh_keys = if ssh_backend.mode() == "local-keys" {
        ssh_backend.list().unwrap_or_default()
    } else {
        Vec::new()
    };
    for key in &ssh_keys {
        log.info(&format!(
            "ssh backend loaded key type={} comment={}",
            key.format, key.comment
        ));
    }
    log.info(&format!(
        "ssh backend mode={} keys={}",
        ssh_backend.mode(),
        ssh_keys.len()
    ));

    let version = full_info();
    let status_path = sockets::status_socket_path();
    let status_listener = bind_unix_socket("status socket", &status_path, &mut log);

    // Two shutdown watches (creatable without a runtime). `shutdown` flips on SIGTERM/SIGINT
    // and drives the supervisor's watch loop (Go's `ctx.Done()`); `listener_shutdown` is
    // flipped by that watch-loop task ONLY AFTER `sup.shutdown()` has drained every watcher
    // group, so the status/desktop listeners finalize (unlink their sockets) only after the
    // groups are down — delivering Go's `main.go:241-247` order (drain groups, THEN close
    // listeners) rather than a siblings-race on one watch.
    let (shutdown_tx, shutdown_rx) = tokio::sync::watch::channel(false);
    let (listener_tx, listener_rx) = tokio::sync::watch::channel(false);

    // The credential minter — built UNCONDITIONALLY (Go `main.go:136`), shared by the
    // supervisor (each secure server's bus + egress token sources self-mint over SSH) and,
    // when the desktop feature is on, the `token.get` control-token provider. known_hosts pin
    // `~/.shed/known_hosts` (the trust `shed server add` wrote); the SSH identity is resolved
    // by the system ssh client, so no key file is read here.
    let minter: Arc<dyn minter::Minter> =
        Arc::new(minter::CredentialMinter::new("~/.shed/known_hosts"));

    // The desktop approval channel (feature-gated). Bind its socket + build the server here so
    // the status snapshot can report its live consumer info. The control-token provider
    // answers `token.get`, reusing the shared `minter`. Mirrors `main.go:136-148`.
    #[cfg(feature = "desktop-forwarding")]
    let (desktop_server, desktop_listener, desktop_path) = {
        // token.get resolves servers from the shed CLI config (DefaultDiscoverySource; the
        // discovery-source override is a per-server concern the supervisor path owns).
        let control_source = shed_broker::controltoken::DEFAULT_DISCOVERY_SOURCE;
        let control_minter = Arc::new(shed_broker::controltoken::ControlTokenProvider::new(
            minter.clone(),
            control_source,
        ));
        let server = desktop::DesktopServer::new(
            version.clone(),
            cfg.gate_namespaces(),
            cfg.approval_timeout(),
            Some(control_minter),
        );
        let path = sockets::desktop_socket_path();
        let listener = bind_unix_socket("desktop", &path, &mut log);
        (server, listener, path)
    };

    // The bus log sink — shared across every watcher group and the backend-construction
    // warnings below.
    let bus_log: Arc<dyn bus::BusLog> = Arc::new(bus::FileBusLog::new(log_file));

    // Select each namespace's approval gate from its own effective policy (empty → deny-all,
    // fail-closed). The built-in policy→gate routing (approve-all / biometrics / unknown →
    // deny) is hoisted into `shed_broker::approval::select_builtin_gate`; the `shed-desktop`
    // arm is bin-only, so this shell composes it: `None` from the core means "supply the
    // desktop gate" — the daemon delegates to its UDS `DesktopServer`, and a headless build
    // (no desktop server) falls closed to deny-all (matching the pre-extraction `#[cfg]`'d
    // match arm). Gives ssh/aws/docker their OWN per-namespace gate. Mirrors `main.go:157-159`.
    let select_gate = |policy: &str| -> Arc<dyn approval::ApprovalGate> {
        approval::select_builtin_gate(policy, &cfg).unwrap_or_else(|| {
            #[cfg(feature = "desktop-forwarding")]
            {
                Arc::new(desktop::DesktopGate::new(desktop_server.clone()))
            }
            #[cfg(not(feature = "desktop-forwarding"))]
            {
                Arc::new(approval::DenyAllGate)
            }
        })
    };
    let ssh_gate = select_gate(&cfg.effective_policy(config::NS_SSH_AGENT));

    // The audit sink (durable JSONL + optional fan-out), shared across every group. The
    // desktop server implements the always-compiled `AuditFanout` seam (bin-side),
    // forwarding each entry as an `event` frame; a headless build wires no fan-out.
    #[cfg(feature = "desktop-forwarding")]
    let audit_fanout: Option<Arc<dyn audit::AuditFanout>> = Some(desktop_server.clone());
    #[cfg(not(feature = "desktop-forwarding"))]
    let audit_fanout: Option<Arc<dyn audit::AuditFanout>> = None;
    let audit: Arc<dyn audit::AuditSink> = Arc::new(audit::JsonlAuditSink::new(&cfg, audit_fanout));

    // The AWS credential backend (optional; Go `main.go:166-173`): a construction error —
    // incl. the "no AWS credentials configured…" not-enabled case — warns and leaves `aws`
    // None, so the aws-credentials namespace is never subscribed. Its gate is the
    // aws-credentials per-namespace gate, distinct from ssh's.
    let aws = match aws_backend::new_sts_backend(cfg.aws.clone(), bus_log.clone()) {
        Ok(sts) => {
            let backend_dyn: Arc<dyn aws_backend::AwsBackend> = Arc::new(sts);
            let aws_gate = select_gate(&cfg.effective_policy(config::NS_AWS_CREDENTIALS));
            Some(bus::AwsHandlers {
                backend: backend_dyn,
                gate: aws_gate,
            })
        }
        Err(e) => {
            bus_log.warn(&format!("AWS handler disabled error={e}"));
            None
        }
    };

    // The Docker credential backend (Go `main.go:175-180`). The ASYMMETRY vs aws: no
    // `enabled()` gate — `new_docker_backend` errors ONLY on an unstat-able explicit
    // `config_path`, so an absent/empty `docker:` block still constructs a live backend
    // (denying everything under an empty allowlist) and the docker-credentials namespace is
    // subscribed for every server. `None` only on that explicit-config_path error.
    let docker = match docker_backend::new_docker_backend(cfg.docker.clone(), bus_log.clone()) {
        Ok(backend) => {
            let backend_dyn: Arc<dyn docker_backend::DockerBackend> = Arc::new(backend);
            let docker_gate = select_gate(&cfg.effective_policy(config::NS_DOCKER_CREDENTIALS));
            Some(bus::DockerHandlers {
                backend: backend_dyn,
                gate: docker_gate,
            })
        }
        Err(e) => {
            bus_log.warn(&format!("Docker handler disabled error={e}"));
            None
        }
    };

    // The server-agnostic deps + the supervisor. ONE supervisor drives BOTH modes: Go has no
    // separate single-server path — single-server is just a discovery config that reconciles
    // once and never reloads, and the single unnamed target has `ssh_host=""` → `should_mint`
    // false → open/no-pin (why the existing single-server bus surface survives unchanged).
    let deps = supervisor::SharedDeps {
        ssh_backend,
        ssh_gate,
        aws,
        docker,
        audit,
        minter: Some(minter),
        log: bus_log.clone(),
    };
    let sup = Arc::new(supervisor::Supervisor::new(shutdown_rx.clone(), deps));

    // The watch mode: single-server = discovery-off (reconcile once, never reload); else the
    // parsed discovery config (Go `main.go:234-240`).
    let watch_cfg = match cfg.discovery.clone() {
        Some(dc) => {
            bus_log.info(&format!(
                "multi-server discovery enabled source={} watch={}",
                dc.source, dc.watch
            ));
            dc
        }
        None => {
            bus_log.info(&format!(
                "brokering for single server server={}",
                cfg.server
            ));
            config::DiscoveryConfig {
                watch: "off".to_string(),
                ..Default::default()
            }
        }
    };
    // `server:` is ignored when `discovery:` is configured (op-log only; Go `main.go:207`).
    if cfg.discovery.is_some() && !cfg.server.is_empty() && cfg.server != config::DEFAULT_SERVER_URL
    {
        bus_log.warn(&format!(
            "`server:` is ignored when `discovery:` is configured server={}",
            cfg.server
        ));
    }

    // The status snapshot is recomputed per connection (fresh written_at/pid) and now
    // includes the supervisor's per-server health (`servers[]`). It reports the desktop
    // consumer identity when the feature is on, else no consumer.
    let health: Arc<dyn Fn() -> LiveStatus + Send + Sync> = {
        let cfg = cfg.clone();
        let config_path = resolved_config_path;
        let started_at = started_at.clone();
        let version = version.clone();
        let sup = sup.clone();
        #[cfg(feature = "desktop-forwarding")]
        let desktop_server = desktop_server.clone();
        Arc::new(move || {
            #[cfg(feature = "desktop-forwarding")]
            let consumer = desktop_server.consumer_info();
            #[cfg(not(feature = "desktop-forwarding"))]
            let consumer = None;
            build_live_status(
                &cfg,
                &config_path,
                &started_at,
                &version,
                consumer,
                sup.health(),
            )
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
        // A task flips `shutdown` on SIGTERM/SIGINT; the watch-loop task races it.
        tokio::spawn(async move {
            wait_for_shutdown().await;
            let _ = shutdown_tx.send(true);
        });

        let mut tasks: Vec<tokio::task::JoinHandle<()>> = Vec::new();

        // The status + desktop listeners race `listener_shutdown` (flipped after the groups
        // drain), NOT the raw SIGTERM watch — so their sockets outlive the group teardown.
        if let Some(listener) = status_listener.and_then(into_tokio_listener) {
            let rx = listener_rx.clone();
            tasks.push(tokio::spawn(async move {
                serve_status_socket(listener, status_path, health, wait_shutdown(rx)).await;
            }));
        }

        #[cfg(feature = "desktop-forwarding")]
        if let Some(listener) = desktop_listener.and_then(into_tokio_listener) {
            let rx = listener_rx.clone();
            let server = desktop_server.clone();
            tasks.push(tokio::spawn(async move {
                server
                    .serve(listener, desktop_path, wait_shutdown(rx))
                    .await;
            }));
        }

        // The watch-loop/supervisor task (H1 shutdown ordering): reconcile per the watch mode
        // until `shutdown` flips, then drain every group (`sup.shutdown()`), THEN release the
        // listeners. The reconcile closure resolves the desired server set from the discovery
        // source and reconciles — on a discovery READ error it logs + keeps the current
        // servers (no reconcile), so a transient/partial-write read can't tear all groups down
        // (Go `main.go:216-224`). A MISSING file is `Ok(vec![])`, which DOES reconcile to empty.
        {
            let sup = sup.clone();
            // Share one config across every reconcile tick — each tick only reads it
            // (`resolve_targets`), so an `Arc` bump replaces a full deep clone per tick.
            let cfg = Arc::new(cfg.clone());
            let reconcile_log = bus_log.clone();
            let watch_log = bus_log.clone();
            let shutdown_log = bus_log.clone();
            let shutdown = shutdown_rx.clone();
            tasks.push(tokio::spawn(async move {
                let reconcile = {
                    // A reconcile-owned clone; the task keeps its own `sup` for the
                    // post-drain `sup.shutdown()` below.
                    let sup = sup.clone();
                    move || {
                        let sup = sup.clone();
                        let cfg = cfg.clone();
                        let log = reconcile_log.clone();
                        async move {
                            match cfg.resolve_targets() {
                                Ok(desired) => sup.reconcile(desired).await,
                                Err(e) => log.warn(&format!(
                                    "discovery read failed; keeping current servers error={e}"
                                )),
                            }
                        }
                    }
                };
                watcher::run_watch_loop(watch_cfg, reconcile, shutdown, watch_log).await;
                // Shutdown was signalled (the watch loop returned) — log before draining the
                // groups, matching Go's `Info("stopping server watchers")` (main.go:243).
                shutdown_log.info("stopping server watchers");
                sup.shutdown().await;
                let _ = listener_tx.send(true);
            }));
        }

        for task in tasks {
            let _ = task.await;
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
