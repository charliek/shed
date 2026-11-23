//! Tauri's orderly-exit wiring (plan 026 §3.3, plan 038 U3).
//!
//! The five-phase sequence lives in [`shed_desktop_core::shutdown`] and is not
//! duplicated here. This module is how the app's exit paths reach it:
//!
//! * [`install_shutdown_log`] sends the shutdown's phase lines to the app's log
//!   (the source runtime's drain is registered by the setup hook, on the
//!   runtime's single [`Shutdown`], before any exit can start);
//! * [`await_orderly_shutdown`] is what `RunEvent::Exit` and a coded
//!   `ExitRequested` (tray Quit, `app.quit`, a finished signal) block on —
//!   [`ShedDesktopRuntime::shutdown_children`];
//! * [`spawn_signal_thread`] hears SIGTERM, SIGINT and SIGHUP on a thread of
//!   its own and starts the same coordinator
//!   ([`ShedDesktopRuntime::shutdown_coordinator`]), so a signal and a quit
//!   share one run.
//!
//! Closing a window still hides it (the app lives in the tray). The paths
//! above are the ones that actually end the process.

use std::sync::Arc;

use shed_desktop_core::{ShedDesktopRuntime, Shutdown};
use tauri::Manager;

pub use shed_desktop_core::ESCAPE;

/// Send the shutdown's phase and completion lines to the app's log. No-op once
/// shutdown has already begun.
pub fn install_shutdown_log(runtime: &ShedDesktopRuntime) {
    runtime.set_shutdown_log(Arc::new(|line: &str| {
        app_info!("shed-desktop-tauri: {line}");
    }));
}

/// Run the orderly shutdown (once — a later call waits for the first run) and
/// block the calling thread until it has finished. Called from the event
/// loop's own thread, never a runtime worker.
///
/// The wait is bounded by [`ESCAPE`], and that bound cancels nothing: the run
/// lives on the app's runtime, not in this wait, so only a runtime wedged past
/// every phase's own bound can reach it.
pub fn await_orderly_shutdown(app: &tauri::AppHandle) {
    let Some(runtime) = app.try_state::<Arc<ShedDesktopRuntime>>() else {
        return;
    };
    let runtime = Arc::clone(&runtime);
    tauri::async_runtime::block_on(async move {
        if tokio::time::timeout(ESCAPE, runtime.shutdown_children())
            .await
            .is_err()
        {
            app_warn!(
                "shed-desktop-tauri: shutdown: not finished within {}s; exiting anyway",
                ESCAPE.as_secs()
            );
        }
    });
}

/// **SIGTERM, SIGINT and SIGHUP, heard on a dedicated OS thread** (plan 026
/// §3.3) — never on the app's runtime, so a wedged runtime cannot swallow a
/// signal or block the escape.
///
/// The thread owns a tiny current-thread runtime of its own and selects over
/// the three streams:
///
/// * the FIRST signal starts the orderly shutdown on the app's runtime
///   ([`Shutdown::start`] spawns it there; this thread never `block_on`s
///   inside a runtime task) and keeps selecting;
/// * when the shutdown completes it calls `app.exit(0)`, whose
///   `ExitRequested` arm finds it done;
/// * a SECOND signal before then, or the shutdown still running [`ESCAPE`]
///   after the first, ends the process at once with `128 + signo`.
pub fn spawn_signal_thread(app: tauri::AppHandle, orderly: Arc<Shutdown>) {
    let spawned = std::thread::Builder::new()
        .name("shed-signals".to_string())
        .spawn(move || {
            let rt = match tokio::runtime::Builder::new_current_thread()
                .enable_all()
                .build()
            {
                Ok(rt) => rt,
                Err(e) => {
                    app_error!(
                        "shed-desktop-tauri: no signal runtime ({e}); signals end the app at once"
                    );
                    return;
                }
            };
            rt.block_on(listen_for_signals(app, orderly));
        });
    if let Err(e) = spawned {
        app_error!("shed-desktop-tauri: no signal thread ({e}); signals end the app at once");
    }
}

async fn listen_for_signals(app: tauri::AppHandle, orderly: Arc<Shutdown>) {
    use tokio::signal::unix::{signal, SignalKind};

    let (Ok(mut term), Ok(mut int), Ok(mut hup)) = (
        signal(SignalKind::terminate()),
        signal(SignalKind::interrupt()),
        signal(SignalKind::hangup()),
    ) else {
        app_error!("shed-desktop-tauri: could not install the signal handlers");
        return;
    };
    // Two signals that arrive before this first `recv` consumes the pending
    // one are ONE signal (POSIX and tokio coalesce them) — kept as designed:
    // the 20 s escape below still exits. A signal after this `recv` latches
    // and completes the next one.
    let first = tokio::select! {
        _ = term.recv() => libc::SIGTERM,
        _ = int.recv() => libc::SIGINT,
        _ = hup.recv() => libc::SIGHUP,
    };
    app_info!("shed-desktop-tauri: signal {first}: shutting down");
    let mut done = orderly.start();
    let escape = tokio::time::sleep(ESCAPE);
    tokio::pin!(escape);
    let mut exiting = false;
    loop {
        let again = tokio::select! {
            finished = done.wait_for(|report| report.is_some()), if !exiting => {
                let finished = finished.is_ok();
                exiting = true;
                if finished {
                    app.exit(0);
                }
                continue;
            }
            _ = term.recv() => libc::SIGTERM,
            _ = int.recv() => libc::SIGINT,
            _ = hup.recv() => libc::SIGHUP,
            () = &mut escape => first,
        };
        // Written and flushed before the exit: the sink appends per line.
        app_warn!("shed-desktop-tauri: exiting now (signal {again})");
        std::process::exit(128 + again);
    }
}
