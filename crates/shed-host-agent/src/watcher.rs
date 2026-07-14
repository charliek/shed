//! Discovery-source reload loop — a faithful port of Go's `watcher.go`.
//!
//! [`run_watch_loop`] performs one initial reconcile, then drives further
//! reconciles off the discovery `watch` mode until `shutdown` flips:
//!   - `"off"`      — reconcile once, then idle until shutdown (never reloads).
//!   - `"poll"`     — reconcile on a ticker at `poll_interval`.
//!   - `"fsnotify"` (and the empty default) — reconcile on debounced filesystem
//!     events for the discovery source's PARENT DIRECTORY (so an atomic
//!     write-temp+rename, which shed's CLI uses, is still observed — a single-file
//!     watch would be left pointing at the replaced inode); falls back to polling if
//!     the [`notify`] watcher can't be created or can't watch the directory.
//!
//! The event-driven backend is the [`notify`] crate (FSEvents on macOS, inotify on
//! Linux) — the Rust analogue of Go's `github.com/fsnotify/fsnotify`. Poll is the
//! always-available safety net: any `notify` setup failure downgrades to it, exactly
//! like Go's `runFsnotifyLoop`.
//!
//! The `reconcile` callback is injected: `main.rs`'s daemon loop wires the real
//! `resolve_targets → Supervisor::reconcile` closure and drives this loop; the units
//! here exercise it with a counting fake reconcile.

use std::ffi::OsStr;
use std::future::Future;
use std::path::{Path, PathBuf};
use std::sync::Arc;
use std::time::Duration;

use notify::{RecursiveMode, Watcher};
use tokio::sync::{mpsc, watch};

use crate::aws_backend::parse_duration_or;
use crate::bus::{wait_shutdown, BusLog};
use crate::config::DiscoveryConfig;

/// Drive `reconcile` based on the discovery watch mode. Performs an initial reconcile,
/// then dispatches on `dc.watch` until `shutdown` flips (mirror `runWatchLoop`,
/// `watcher.go:18`): `"off"` idles, `"poll"` tickers, `"fsnotify"`/`""` watches the
/// source directory, and any unknown mode warns then polls.
///
/// `reconcile` is `async` (it awaits the supervisor's cancel+drain in commit 3); it is
/// called once up front and again on each trigger. `shutdown` is the daemon-wide
/// `watch<bool>` (Go's `ctx.Done()`); the loop returns once it is (or becomes) true.
pub(crate) async fn run_watch_loop<F, Fut>(
    dc: DiscoveryConfig,
    mut reconcile: F,
    shutdown: watch::Receiver<bool>,
    log: Arc<dyn BusLog>,
) where
    F: FnMut() -> Fut,
    Fut: Future<Output = ()>,
{
    reconcile().await;

    match dc.watch.as_str() {
        "off" => wait_shutdown(shutdown).await,
        "poll" => run_poll_loop(&dc, reconcile, shutdown, log).await,
        "fsnotify" | "" => run_fsnotify_loop(&dc, reconcile, shutdown, log).await,
        other => {
            log.warn(&format!(
                "unknown discovery.watch mode, using poll: mode={other}"
            ));
            run_poll_loop(&dc, reconcile, shutdown, log).await;
        }
    }
}

/// Reconcile on a ticker at `poll_interval` (default 10s), until shutdown — mirror
/// `runPollLoop` (`watcher.go:34`). The ticker's first tick fires after one interval
/// (not immediately, matching Go's `time.NewTicker`), since `run_watch_loop` has
/// already done the initial reconcile.
async fn run_poll_loop<F, Fut>(
    dc: &DiscoveryConfig,
    mut reconcile: F,
    shutdown: watch::Receiver<bool>,
    log: Arc<dyn BusLog>,
) where
    F: FnMut() -> Fut,
    Fut: Future<Output = ()>,
{
    let interval = parse_duration_or(
        &dc.poll_interval,
        Duration::from_secs(10),
        "discovery.poll_interval",
        log.as_ref(),
    );
    log.info(&format!(
        "watching discovery source by polling: source={} interval={interval:?}",
        dc.source
    ));
    // interval_at (start = now + interval) so the first tick lands one period out,
    // matching Go's ticker — the immediate reconcile already happened in run_watch_loop.
    let start = tokio::time::Instant::now() + interval;
    let mut ticker = tokio::time::interval_at(start, interval);
    // Go's time.Ticker DROPS ticks missed while a slow reconcile ran; tokio's default
    // (Burst) would fire catch-up ticks back-to-back. Delay matches Go (CodeRabbit review).
    ticker.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Delay);
    loop {
        tokio::select! {
            _ = wait_shutdown(shutdown.clone()) => return,
            _ = ticker.tick() => reconcile().await,
        }
    }
}

/// Reconcile on debounced filesystem events for the discovery source's parent
/// directory, until shutdown — mirror `runFsnotifyLoop` (`watcher.go:49`). On ANY
/// `notify` setup failure (watcher creation or the directory `watch()`), falls back to
/// [`run_poll_loop`] — poll is the always-available safety net. Events whose basename
/// differs from the source's are ignored; a matching event (re)arms a `debounce`
/// (default 500ms) timer whose firing runs one reconcile, coalescing the burst editors
/// and atomic-rename saves emit (a spurious extra reconcile is harmless — reconcile is
/// idempotent).
async fn run_fsnotify_loop<F, Fut>(
    dc: &DiscoveryConfig,
    mut reconcile: F,
    shutdown: watch::Receiver<bool>,
    log: Arc<dyn BusLog>,
) where
    F: FnMut() -> Fut,
    Fut: Future<Output = ()>,
{
    // Bridge notify's synchronous, own-thread callback into the async loop via an
    // unbounded channel (an unbounded send never blocks the notify thread).
    let (tx, mut rx) = mpsc::unbounded_channel::<notify::Result<notify::Event>>();
    let mut watcher = match notify::recommended_watcher(move |res| {
        let _ = tx.send(res);
    }) {
        Ok(w) => w,
        Err(e) => {
            log.warn(&format!(
                "fsnotify unavailable, falling back to polling: error={e}"
            ));
            return run_poll_loop(dc, reconcile, shutdown, log).await;
        }
    };

    // Watch the PARENT DIR rather than the file itself so atomic rewrites (write temp +
    // rename) are still observed — a single-file watch would follow the replaced inode.
    let dir = watch_dir(&dc.source);
    if let Err(e) = watcher.watch(&dir, RecursiveMode::NonRecursive) {
        log.warn(&format!(
            "cannot watch discovery directory, falling back to polling: dir={} error={e}",
            dir.display()
        ));
        return run_poll_loop(dc, reconcile, shutdown, log).await;
    }
    log.info(&format!(
        "watching discovery source via fsnotify: dir={} source={}",
        dir.display(),
        dc.source
    ));

    let debounce = parse_duration_or(
        &dc.debounce,
        Duration::from_millis(500),
        "discovery.debounce",
        log.as_ref(),
    );
    let base = Path::new(&dc.source).file_name().map(OsStr::to_os_string);

    // `deadline` is the pending debounce fire time; `None` means no timer armed. Each
    // matching event bumps it forward; when the select's sleep arm reaches it with no
    // further event, one reconcile fires. The sleep future is rebuilt each iteration
    // from `deadline`, so a bump correctly re-arms it.
    let mut deadline: Option<tokio::time::Instant> = None;
    loop {
        tokio::select! {
            _ = wait_shutdown(shutdown.clone()) => return,
            ev = rx.recv() => match ev {
                // The notify watcher was dropped (should not happen while it is held
                // in scope) — end the loop like Go's closed-channel case.
                None => return,
                Some(Ok(event)) => {
                    if event_matches(&event, base.as_deref()) {
                        deadline = Some(tokio::time::Instant::now() + debounce);
                    }
                }
                Some(Err(e)) => log.warn(&format!("fsnotify error: error={e}")),
            },
            _ = async { tokio::time::sleep_until(deadline.expect("guarded by deadline.is_some()")).await },
                if deadline.is_some() =>
            {
                deadline = None;
                reconcile().await;
            }
        }
    }
}

/// The directory to watch for a discovery `source` path — its parent, or `"."` when the
/// source has no parent (mirror Go's `filepath.Dir`). The apply-defaults path
/// tilde-expands `source` to an absolute path, so the parent is normally concrete.
fn watch_dir(source: &str) -> PathBuf {
    match Path::new(source).parent() {
        Some(p) if !p.as_os_str().is_empty() => p.to_path_buf(),
        _ => PathBuf::from("."),
    }
}

/// Whether a notify event touches the discovery source file — its basename matches
/// `base` (mirror Go's `filepath.Base(event.Name) != base` filter). A parent-directory
/// watch reports every sibling; only the source file's own events reconcile.
fn event_matches(event: &notify::Event, base: Option<&OsStr>) -> bool {
    match base {
        None => false,
        Some(base) => event.paths.iter().any(|p| p.file_name() == Some(base)),
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::bus::FileBusLog;
    use std::sync::atomic::{AtomicUsize, Ordering};

    fn test_log() -> Arc<dyn BusLog> {
        Arc::new(FileBusLog::new(""))
    }

    /// A counting fake reconcile: returns the shared counter plus an async `FnMut` that
    /// bumps it (the injected reconcile the supervisor slice replaces with the real one).
    fn counter() -> (Arc<AtomicUsize>, impl FnMut() -> std::future::Ready<()>) {
        let n = Arc::new(AtomicUsize::new(0));
        let c = n.clone();
        let f = move || {
            c.fetch_add(1, Ordering::SeqCst);
            std::future::ready(())
        };
        (n, f)
    }

    /// Poll `n` until it reaches `want` or `deadline` elapses; returns whether it did.
    async fn wait_count(n: &AtomicUsize, want: usize, deadline: Duration) -> bool {
        let start = tokio::time::Instant::now();
        loop {
            if n.load(Ordering::SeqCst) >= want {
                return true;
            }
            if start.elapsed() >= deadline {
                return false;
            }
            tokio::time::sleep(Duration::from_millis(5)).await;
        }
    }

    fn dc(watch: &str) -> DiscoveryConfig {
        DiscoveryConfig {
            watch: watch.to_string(),
            ..Default::default()
        }
    }

    /// `off` reconciles exactly once, then idles until shutdown flips, then returns
    /// (mirror `TestRunWatchLoopOff`).
    #[tokio::test]
    async fn run_watch_loop_off_reconciles_once() {
        let (n, reconcile) = counter();
        let (tx, rx) = watch::channel(false);
        let handle = tokio::spawn(run_watch_loop(dc("off"), reconcile, rx, test_log()));

        assert!(
            wait_count(&n, 1, Duration::from_secs(1)).await,
            "no initial reconcile"
        );
        // Off must not reconcile again.
        tokio::time::sleep(Duration::from_millis(100)).await;
        assert_eq!(n.load(Ordering::SeqCst), 1, "off reconciled more than once");
        // Returns after shutdown.
        tx.send(true).unwrap();
        tokio::time::timeout(Duration::from_secs(1), handle)
            .await
            .expect("off did not return after shutdown")
            .unwrap();
    }

    /// `poll` reconciles initially, then at least once more on a ticker (mirror
    /// `TestRunWatchLoopPoll`).
    #[tokio::test]
    async fn run_watch_loop_poll_ticks() {
        let (n, reconcile) = counter();
        let (_tx, rx) = watch::channel(false);
        let mut cfg = dc("poll");
        cfg.poll_interval = "20ms".to_string();
        tokio::spawn(run_watch_loop(cfg, reconcile, rx, test_log()));

        assert!(
            wait_count(&n, 1, Duration::from_secs(1)).await,
            "no initial reconcile"
        );
        assert!(
            wait_count(&n, 2, Duration::from_secs(1)).await,
            "no poll-tick reconcile"
        );
    }

    /// `fsnotify` reconciles initially, then again after the source file is written —
    /// a real `notify`-backed watch on a temp dir (mirror `TestRunWatchLoopFsnotify`).
    /// The retry-write loop tolerates the brief window before the directory watch is
    /// registered and any backend latency. multi-thread so the notify callback thread
    /// and the async loop make progress concurrently.
    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn run_watch_loop_fsnotify_on_write() {
        let dir = tempfile::tempdir().unwrap();
        let source = dir.path().join("config.yaml");
        let (n, reconcile) = counter();
        let (_tx, rx) = watch::channel(false);
        let cfg = DiscoveryConfig {
            watch: "fsnotify".to_string(),
            source: source.to_string_lossy().into_owned(),
            debounce: "20ms".to_string(),
            ..Default::default()
        };
        tokio::spawn(run_watch_loop(cfg, reconcile, rx, test_log()));

        assert!(
            wait_count(&n, 1, Duration::from_secs(2)).await,
            "no initial reconcile"
        );

        let mut got = false;
        for _ in 0..50 {
            std::fs::write(&source, "servers: {}\n").unwrap();
            if wait_count(&n, 2, Duration::from_millis(200)).await {
                got = true;
                break;
            }
        }
        assert!(got, "fsnotify did not trigger a reconcile on write");
    }

    /// A `notify` setup failure (here: the source's parent directory does not exist, so
    /// `watcher.watch()` errors) downgrades to the poll safety net — a poll tick still
    /// reconciles despite fsnotify being unusable (mirror Go's `runFsnotifyLoop`
    /// fall-through to `runPollLoop`).
    #[tokio::test]
    async fn run_watch_loop_fsnotify_setup_error_falls_back_to_poll() {
        let (n, reconcile) = counter();
        let (_tx, rx) = watch::channel(false);
        let cfg = DiscoveryConfig {
            watch: "fsnotify".to_string(),
            source: "/nonexistent-shed-dir-xyz/config.yaml".to_string(),
            poll_interval: "20ms".to_string(),
            ..Default::default()
        };
        tokio::spawn(run_watch_loop(cfg, reconcile, rx, test_log()));

        assert!(
            wait_count(&n, 1, Duration::from_secs(1)).await,
            "no initial reconcile"
        );
        assert!(
            wait_count(&n, 2, Duration::from_secs(1)).await,
            "fsnotify setup error did not fall back to poll"
        );
    }

    /// An unknown watch mode warns then polls (the `default` arm) — a tick still fires.
    #[tokio::test]
    async fn run_watch_loop_unknown_mode_polls() {
        let (n, reconcile) = counter();
        let (_tx, rx) = watch::channel(false);
        let mut cfg = dc("bogus");
        cfg.poll_interval = "20ms".to_string();
        tokio::spawn(run_watch_loop(cfg, reconcile, rx, test_log()));

        assert!(
            wait_count(&n, 1, Duration::from_secs(1)).await,
            "no initial reconcile"
        );
        assert!(
            wait_count(&n, 2, Duration::from_secs(1)).await,
            "unknown mode did not poll"
        );
    }

    /// The duration helper the loops share (`aws_backend::parse_duration_or`, the Go
    /// `parseDurationOr` port): a valid Go duration parses; empty / unparseable /
    /// non-positive fall back to the default.
    #[test]
    fn parse_duration_or_defaults() {
        let log = test_log();
        let def = Duration::from_secs(10);
        assert_eq!(
            parse_duration_or("20ms", def, "x", log.as_ref()),
            Duration::from_millis(20)
        );
        assert_eq!(parse_duration_or("", def, "x", log.as_ref()), def);
        assert_eq!(parse_duration_or("abc", def, "x", log.as_ref()), def);
        assert_eq!(parse_duration_or("-5s", def, "x", log.as_ref()), def);
        assert_eq!(parse_duration_or("0", def, "x", log.as_ref()), def);
    }

    #[test]
    fn watch_dir_uses_parent_or_dot() {
        assert_eq!(watch_dir("/a/b/config.yaml"), PathBuf::from("/a/b"));
        assert_eq!(watch_dir("config.yaml"), PathBuf::from("."));
    }
}
