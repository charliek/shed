//! The machine rc-hub **role**: hosting [`super::hub`] inside a long-lived
//! process — bind-as-lock on the loopback port, the mixed-fleet bind-retry FSM
//! (a foreign hub may hold the port; the role takes it over whenever that
//! holder exits), the reconcile-thread lifecycle, and the foreground
//! diagnostic. Both entrypoints share ONE `HubConfig` builder so they cannot
//! drift.
//!
//! Landed in plan 010 §2.6 as `shed-host-agent`'s bin-local `rc_hub_role.rs`
//! and **graduated here in plan 012 (roadmap R4)** at its second consumer: the
//! desktop app hosts the hub in-process through `shed-app`'s broker bridge, so
//! the role can no longer live in the daemon bin. Nothing about the behaviour
//! moved with it — `tests/rc-parity`'s hub family is the proof (the wire is
//! frozen; a diff there means the graduation changed something).
//!
//! What stayed in the daemon: its CLI/arg parsing, its signal handling, and the
//! `main.rs` task mount. Both entrypoints here take their shutdown signal from
//! the caller rather than installing one.

use std::sync::{Arc, Mutex, PoisonError};
use std::time::Duration;

use shed_rc_engine::tmux::ExecRunner;

use crate::bus::{BusLog, FileBusLog};
use crate::rc_hub::hub::{
    self as hub, apply_hub_env_overrides, run_reconcile_loop, spawn_fs_nudger, Hub, HubConfig,
    LoopSignal, HUB_ADDR,
};
use crate::status::RcHubStatus;

/// The role's log prefix (§2.6 observability).
const LABEL: &str = "rc-hub";

/// The mixed-fleet retry window: 30s doubling to a 5m cap.
const RETRY_MIN: Duration = Duration::from_secs(30);
const RETRY_MAX: Duration = Duration::from_secs(300);

/// The production process-env reader, shared by the config's `getenv` seam and
/// the §2.5 override pass so there is exactly one definition of "the env".
fn env_get(key: &str) -> String {
    std::env::var(key).unwrap_or_default()
}

/// The ONE HubConfig builder both entrypoints share (§2.6): production seams
/// (real tmux, process env, wall clock) + the §2.5 env-seam overrides, with
/// every rejected override reported through `note`.
fn build_hub_config(
    version: &str,
    logf: crate::rc_hub::watch::LogFn,
    note: &mut dyn FnMut(&str),
) -> HubConfig {
    let mut cfg = HubConfig {
        runner: Arc::new(ExecRunner),
        getenv: Arc::new(env_get),
        now: None,
        logf: Some(logf),
        // The production loopback invariant, spelled once here rather than
        // leaning on `HubConfig::resolve`'s ""-means-1029 rule: this value is
        // what the role binds, dials and reports, so it must be concrete
        // before an override gets a chance at it.
        addr: HUB_ADDR.to_string(),
        version: version.to_string(),
        active_interval: Duration::ZERO,
        idle_interval: Duration::ZERO,
        quiet_period: Duration::ZERO,
        // "Never" for the supervised resident role (§2.4): the Go seam cannot
        // express it, so the effectively-infinite value is the daemon-role
        // default — an env override (the harness pins a large finite value on
        // both legs) replaces it.
        idle_timeout: Duration::from_secs(u64::MAX / 4),
        heartbeat: Duration::ZERO,
        write_timeout: Duration::ZERO,
        subscriber_buffer: 0,
        send_line_settle: None,
    };
    apply_hub_env_overrides(&mut cfg, &env_get, note);
    cfg
}

// ---------------------------------------------------------------------------
// The bind-retry FSM (§2.6) — bind-as-lock: `AlreadyBound` is a SIGNAL the FSM
// acts on (probe identity, back off), never an error.
// ---------------------------------------------------------------------------

/// The outcome of one rc-hub bind attempt (`bindHubListener`, hub.go:881).
enum BindOutcome {
    Bound(std::net::TcpListener),
    /// EADDRINUSE — a hub (or a squatter) holds the port.
    AlreadyBound,
    /// Any other bind error: retried like AlreadyBound, because a transient
    /// EACCES/ENOBUFS must not kill the role for the process's lifetime.
    Failed(std::io::Error),
}

fn bind_hub(addr: &str) -> BindOutcome {
    match hub::bind_hub_listener(addr) {
        Ok((Some(listener), _)) => BindOutcome::Bound(listener),
        Ok((None, _already)) => BindOutcome::AlreadyBound,
        Err(e) => BindOutcome::Failed(e),
    }
}

/// What one AlreadyBound probe concluded.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub(crate) enum ProbeVerdict {
    /// A verified hub answered /v1/health (another hub during the mixed
    /// window) — expected, log at info.
    Hub,
    /// Something else is listening — log at warn.
    Squatter,
    /// The port was in use at bind time but nothing answers now (the holder
    /// is mid-exit) — retry soon, log at info.
    Nothing,
}

/// The retry backoff state: 30s doubling to a 5m cap; reset on a successful
/// bind so a later loss of the port starts the window over.
pub(crate) struct BindRetry {
    next: Duration,
}

impl BindRetry {
    pub(crate) fn new() -> BindRetry {
        BindRetry { next: RETRY_MIN }
    }

    /// The wait before the next bind attempt; doubles up to the cap.
    pub(crate) fn next_wait(&mut self) -> Duration {
        let wait = self.next;
        self.next = (self.next * 2).min(RETRY_MAX);
        wait
    }

    pub(crate) fn reset(&mut self) {
        self.next = RETRY_MIN;
    }
}

/// One probe of a held port, classified (`queryHubHealth`'s three-way answer).
fn probe_holder(addr: &str) -> ProbeVerdict {
    match hub::query_hub_health(addr, Duration::from_secs(1)) {
        Ok(true) => ProbeVerdict::Hub,
        Ok(false) => ProbeVerdict::Squatter,
        Err(_) => ProbeVerdict::Nothing,
    }
}

fn set_status(status: &Arc<Mutex<RcHubStatus>>, state: &str, addr: &str) {
    let mut s = status.lock().unwrap_or_else(PoisonError::into_inner);
    s.state = state.to_string();
    s.addr = addr.to_string();
}

/// Why [`serve_until_shutdown`] returned.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum ServeEnd {
    /// The shutdown watch flipped — the ordinary path.
    Shutdown,
    /// The serve task ended on its own. [`hub::serve`] never returns by design
    /// (accept errors are logged and retried so the bind-as-lock is never
    /// released), so in practice this means the task PANICKED — or the
    /// listener could not be adopted into the runtime at all.
    Abnormal,
}

// ---------------------------------------------------------------------------
// The hosted role task
// ---------------------------------------------------------------------------

/// Host the hub inside a supervising process: bind (retrying per the FSM),
/// serve + run the reconcile thread, and on shutdown stop in the §2.6 order —
/// reconcile stops (its Stop arm closes subscribers + watchers,
/// `closeAllWatchers` parity) → THEN the listener is released.
///
/// Returns only when `shutdown` flips; a lost or held port is a state the role
/// sits in (reported through `status`), never a reason to return.
pub async fn run_rc_hub_role(
    version: String,
    enabled: bool,
    status: Arc<Mutex<RcHubStatus>>,
    shutdown: tokio::sync::watch::Receiver<bool>,
    log: Arc<dyn BusLog>,
) {
    if !enabled {
        set_status(&status, "disabled", "");
        log.info(&format!("{LABEL}: disabled by config"));
        return;
    }
    let mut retry = BindRetry::new();
    loop {
        if *shutdown.borrow() {
            return;
        }
        // Env overrides are read at role start / each retry (§2.6); rejected
        // values are warned through the host's log.
        let cfg = {
            let note_log = log.clone();
            let hub_log = log.clone();
            let logf: crate::rc_hub::watch::LogFn = Arc::new(move |line| hub_log.info(line));
            build_hub_config(&version, logf, &mut |n| {
                note_log.warn(&format!("{LABEL}: {n}"));
            })
        };
        let addr = cfg.addr.clone();

        match bind_hub(&addr) {
            BindOutcome::Bound(listener) => {
                retry.reset();
                set_status(&status, "listening", &addr);
                log.info(&format!("{LABEL}: listening {addr}"));
                let end = serve_until_shutdown(cfg, listener, shutdown.clone(), &log).await;
                if end == ServeEnd::Shutdown || *shutdown.borrow() {
                    return;
                }
                set_status(&status, "deferred", &addr);
            }
            BindOutcome::AlreadyBound => {
                // The identity probe is bounded (1s) but still raced against
                // shutdown: a wedged holder must never delay process exit.
                let probe_addr = addr.clone();
                let mut sd = shutdown.clone();
                let verdict = tokio::select! {
                    v = tokio::task::spawn_blocking(move || probe_holder(&probe_addr)) => {
                        v.unwrap_or(ProbeVerdict::Nothing)
                    }
                    _ = sd.wait_for(|f| *f) => return,
                };
                set_status(&status, "deferred", &addr);
                match verdict {
                    ProbeVerdict::Hub => log.info(&format!(
                        "{LABEL}: port {addr} held by another hub, retrying"
                    )),
                    ProbeVerdict::Squatter => log.warn(&format!(
                        "{LABEL}: port {addr} held by a non-hub process, retrying"
                    )),
                    ProbeVerdict::Nothing => log.info(&format!(
                        "{LABEL}: port {addr} in use but not answering, retrying"
                    )),
                }
            }
            BindOutcome::Failed(e) => {
                set_status(&status, "deferred", &addr);
                log.warn(&format!("{LABEL}: bind {addr} failed, retrying error={e}"));
            }
        }
        // Back off, racing shutdown.
        let wait = retry.next_wait();
        let mut sd = shutdown.clone();
        tokio::select! {
            _ = tokio::time::sleep(wait) => {}
            _ = sd.wait_for(|f| *f) => return,
        }
    }
}

/// Serve one bound listener until shutdown flips (or the accept loop dies).
/// Owns the reconcile thread + fs nudger for exactly that span.
async fn serve_until_shutdown(
    cfg: HubConfig,
    listener: std::net::TcpListener,
    mut shutdown: tokio::sync::watch::Receiver<bool>,
    log: &Arc<dyn BusLog>,
) -> ServeEnd {
    let hub = Arc::new(Hub::new(cfg));
    let (sig_tx, sig_rx) = std::sync::mpsc::channel::<LoopSignal>();
    // Best-effort fsnotify nudges. The forwarder thread is intentionally
    // detached: it parks in `nudger.nudge().recv()`, so it only notices this
    // cycle's reconcile receiver going away on the NEXT filesystem event —
    // one parked thread (and its watcher) per SERVE CYCLE until then, released
    // on the first event after. Serve cycles are rare (see `ServeEnd`), and
    // the host exits with the process.
    let _nudger = spawn_fs_nudger(&hub, sig_tx.clone());
    let reconcile_hub = Arc::clone(&hub);
    let reconcile = std::thread::spawn(move || run_reconcile_loop(&reconcile_hub, &sig_rx));

    let _ = listener.set_nonblocking(true);
    let serve_task = match tokio::net::TcpListener::from_std(listener) {
        Ok(l) => tokio::spawn(hub::serve(Arc::clone(&hub), l)),
        Err(e) => {
            log.warn(&format!("{LABEL}: adopting the listener failed error={e}"));
            stop_reconcile(sig_tx, reconcile).await;
            return ServeEnd::Abnormal;
        }
    };

    let mut serve_task = serve_task;
    let end = tokio::select! {
        res = &mut serve_task => {
            // Only reachable if the serve task panicked (`hub::serve` retries
            // accept errors rather than returning, to hold the bind lock).
            if let Ok(Err(e)) = res {
                log.warn(&format!("{LABEL}: accept loop ended error={e}"));
            }
            ServeEnd::Abnormal
        }
        _ = shutdown.wait_for(|f| *f) => ServeEnd::Shutdown,
    };
    // §2.6 shutdown order: reconcile stops (Stop arm = closeAllSubscribers +
    // closeAllWatchers) → THEN the listener is released (aborting the serve
    // task drops it).
    stop_reconcile(sig_tx, reconcile).await;
    serve_task.abort();
    let _ = serve_task.await;
    log.info(&format!("{LABEL}: stopped"));
    end
}

/// Signal the reconcile loop to stop and wait for it. UNBOUNDED on purpose:
/// the loop runs the shutdown cleanup (subscribers + watchers) on its way out,
/// and a reconcile in flight is shelling out to `tmux`, which `ExecRunner`
/// runs without a timeout (same posture as the Go hub) — so process exit can be
/// held up by an unresponsive tmux server.
async fn stop_reconcile(
    sig_tx: std::sync::mpsc::Sender<LoopSignal>,
    reconcile: std::thread::JoinHandle<()>,
) {
    let _ = sig_tx.send(LoopSignal::Stop);
    let _ = tokio::task::spawn_blocking(move || reconcile.join()).await;
}

// ---------------------------------------------------------------------------
// The foreground diagnostic (`shed-host-agent rc-hub`)
// ---------------------------------------------------------------------------

/// Run the hub in the FOREGROUND until `shutdown` resolves: no broker
/// config/socket ceremony, honoring the §2.5 env vars, logging to stderr.
/// Bind-as-lock keeps Go `RunHub`'s meaning: an existing verified hub → exit
/// 0; a foreign holder → error, exit 1. Backs the `shed-host-agent rc-hub`
/// subcommand — the rc-parity harness's Rust leg and a diagnostic surface,
/// documented as such, not user surface.
///
/// It reads NO broker config: `-config` / `-log-file` are ignored, and so is
/// `rc_hub.enabled` — that knob gates the hosted ROLE, and a diagnostic you
/// typed by hand should run on the machine that opted the daemon out.
///
/// `shutdown` is supplied by the caller (the daemon hands it its SIGTERM/SIGINT
/// wait) because installing signal handlers is a process-shell concern, not a
/// broker one.
pub fn run_rc_hub_foreground<F>(version: &str, shutdown: F) -> i32
where
    F: std::future::Future<Output = ()> + Send + 'static,
{
    let logf: crate::rc_hub::watch::LogFn = Arc::new(|line| eprintln!("{line}"));
    let cfg = build_hub_config(version, logf, &mut |n| {
        eprintln!("shed-host-agent: {n}");
    });
    let addr = cfg.addr.clone();

    let listener = match bind_hub(&addr) {
        BindOutcome::Bound(l) => l,
        BindOutcome::AlreadyBound => {
            // Bind-as-lock, identity-verified (`RunHub`, hub.go:718-726).
            match hub::query_hub_health(&addr, Duration::from_secs(1)) {
                Ok(true) => {
                    eprintln!("rc hub: {addr} already in use; another hub is running");
                    return 0;
                }
                _ => {
                    eprintln!(
                        "rc hub: port {addr} is held by another process that is not a shed rc hub"
                    );
                    return 1;
                }
            }
        }
        BindOutcome::Failed(e) => {
            eprintln!("rc hub: binding {addr}: {e}");
            return 1;
        }
    };
    eprintln!("rc hub: listening {addr}");

    let runtime = match tokio::runtime::Builder::new_multi_thread()
        .enable_all()
        .build()
    {
        Ok(rt) => rt,
        Err(e) => {
            eprintln!("rc hub: failed to start async runtime: {e}");
            return 1;
        }
    };
    let (shutdown_tx, shutdown_rx) = tokio::sync::watch::channel(false);
    let end = runtime.block_on(async move {
        tokio::spawn(async move {
            shutdown.await;
            let _ = shutdown_tx.send(true);
        });
        // The bus log's empty path IS stderr (`FileBusLog::new`), which is
        // where a foreground diagnostic belongs.
        let log: Arc<dyn BusLog> = Arc::new(FileBusLog::new(""));
        serve_until_shutdown(cfg, listener, shutdown_rx, &log).await
    });
    // A clean signal-driven stop is 0; the hub falling over on its own is not
    // (Go's `RunHub` returns the server error, which its caller exits on).
    match end {
        ServeEnd::Shutdown => 0,
        ServeEnd::Abnormal => 1,
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::rc_hub::hub::ENV_HUB_ADDR;

    fn test_log() -> Arc<dyn BusLog> {
        Arc::new(FileBusLog::new(""))
    }

    /// A bound-but-silent loopback listener, returned with its address: it
    /// holds the port (so a bind EADDRINUSEs) and answers nothing.
    fn held_port() -> (std::net::TcpListener, String) {
        let ln = std::net::TcpListener::bind("127.0.0.1:0").expect("bind");
        let addr = ln.local_addr().expect("addr").to_string();
        (ln, addr)
    }

    // The backoff schedule: 30s doubling to the 5m cap, reset on success.
    #[test]
    fn bind_retry_backoff_schedule() {
        let mut r = BindRetry::new();
        let waits: Vec<u64> = (0..7).map(|_| r.next_wait().as_secs()).collect();
        assert_eq!(waits, vec![30, 60, 120, 240, 300, 300, 300]);
        r.reset();
        assert_eq!(r.next_wait().as_secs(), 30);
    }

    // The shared builder (§2.6): the production loopback addr + the "never"
    // idle timeout by default, and the §2.5 seams applied on top. The hub-side
    // parsing/rejection rules are pinned in shed-broker.
    #[test]
    fn build_hub_config_defaults_and_env_seams() {
        let _guard = crate::env_lock();
        std::env::remove_var(ENV_HUB_ADDR);
        let cfg = build_hub_config("test", Arc::new(|_| {}), &mut |_| {});
        assert_eq!(cfg.addr, HUB_ADDR, "the production loopback invariant");
        assert!(
            cfg.idle_timeout > Duration::from_secs(86_400 * 365),
            "the supervised role never idle-exits"
        );

        std::env::set_var(ENV_HUB_ADDR, "127.0.0.1:1234");
        let cfg = build_hub_config("test", Arc::new(|_| {}), &mut |_| {});
        std::env::remove_var(ENV_HUB_ADDR);
        assert_eq!(cfg.addr, "127.0.0.1:1234", "the §2.5 addr seam wins");
    }

    // The local half of the AlreadyBound probe: `query_hub_health`'s three-way
    // answer mapped onto the FSM's verdicts. (The handshake itself — including
    // the real-hub arm — is pinned in the hub's own tests.)
    #[test]
    fn probe_holder_maps_the_three_way_answer() {
        let (_held, addr) = held_port();
        assert_eq!(
            probe_holder(&addr),
            ProbeVerdict::Squatter,
            "listening but not answering /v1/health"
        );

        // "Nothing listening at all" is asserted over a just-RELEASED ephemeral
        // port, which another test in this crate can legitimately claim in the
        // gap between the drop and the probe (there are ~670 tests here now,
        // several of which bind loopback ports — a real hazard the bin crate's
        // ~30 tests did not have). Retry on fresh ports rather than asserting
        // once: a genuinely broken mapping never yields `Nothing` on any
        // attempt, so this hardens against the race without weakening the
        // assertion.
        let mut last = ProbeVerdict::Hub;
        for _ in 0..5 {
            let (free, free_addr) = held_port();
            drop(free);
            last = probe_holder(&free_addr);
            if last == ProbeVerdict::Nothing {
                return;
            }
        }
        panic!("nothing listening at all should probe as Nothing, got {last:?}");
    }

    // `rc_hub.enabled: false` → the role reports and returns immediately,
    // without touching the port.
    #[test]
    fn role_disabled_reports_and_returns() {
        let status = Arc::new(Mutex::new(RcHubStatus::default()));
        let rt = tokio::runtime::Builder::new_multi_thread()
            .enable_all()
            .build()
            .unwrap();
        rt.block_on(async {
            let (_tx, rx) = tokio::sync::watch::channel(false);
            // Returns on its own — no shutdown signal is ever sent.
            run_rc_hub_role("test".into(), false, Arc::clone(&status), rx, test_log()).await;
        });
        let s = status.lock().unwrap();
        assert_eq!(s.state, "disabled");
        assert!(s.addr.is_empty(), "a disabled role claims no address");
    }

    // The FSM's AlreadyBound arm end to end: a real holder on the (env-pinned)
    // port → the role reports `deferred` with that addr and stays in the retry
    // loop, and the shutdown flip pulls it out of the backoff. The env guard is
    // held around `block_on`, never across an await.
    #[test]
    fn role_defers_while_the_port_is_held() {
        let _guard = crate::env_lock();
        let (_held, addr) = held_port();
        std::env::set_var(ENV_HUB_ADDR, &addr);

        let rt = tokio::runtime::Builder::new_multi_thread()
            .enable_all()
            .build()
            .unwrap();
        let outcome = rt.block_on(async {
            let status = Arc::new(Mutex::new(RcHubStatus::default()));
            let (tx, rx) = tokio::sync::watch::channel(false);
            let role = tokio::spawn(run_rc_hub_role(
                "test".into(),
                true,
                Arc::clone(&status),
                rx,
                test_log(),
            ));
            let deadline = std::time::Instant::now() + Duration::from_secs(10);
            let reported = loop {
                {
                    let s = status.lock().unwrap();
                    if !s.state.is_empty() {
                        break (s.state.clone(), s.addr.clone());
                    }
                }
                if std::time::Instant::now() > deadline {
                    break (String::from("<never reported>"), String::new());
                }
                tokio::time::sleep(Duration::from_millis(20)).await;
            };
            let _ = tx.send(true);
            // The role must leave the 30s backoff on the shutdown flip.
            let exited = tokio::time::timeout(Duration::from_secs(5), role)
                .await
                .is_ok();
            (reported, exited)
        });
        std::env::remove_var(ENV_HUB_ADDR);

        let ((state, reported_addr), exited) = outcome;
        assert_eq!(state, "deferred", "a held port defers, it never errors out");
        assert_eq!(reported_addr, addr, "status reports the addr it tried");
        assert!(exited, "the shutdown flip must cut the backoff short");
    }
}
