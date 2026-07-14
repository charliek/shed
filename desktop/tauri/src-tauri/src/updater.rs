//! The Sparkle auto-updater integration — Swift-parity, user-invoked checks only.
//!
//! Real Sparkle is embedded via `tauri-plugin-sparkle-updater` (macOS only). The
//! plugin is registered ONLY outside test mode (see `lib.rs`), so the harness never
//! instantiates an updater (the Swift-parity guarantee, enforced at registration).
//! The crate itself is inert outside a real bundle (`is_valid_bundle()` → the
//! managed `SparkleUpdater` state is absent under `cargo run`/the raw harness binary).
//!
//! This module keeps the observable surface (`updater_status`/`updater_check` — both
//! the WebView commands and the IPC ops) platform-truthful and drivable, and factors
//! the two decisions the tests pin — the disabled/enabled REASON and the beta-channel
//! rule — into pure functions with unit tests (the only pre-rehearsal coverage).

use serde_json::{json, Value};
use tauri::{AppHandle, Runtime};

/// `enabled == true` — the updater is live and a check presents Sparkle.
pub const REASON_OK: &str = "ok";
/// Registered off under the harness — the plugin is never instantiated in test mode.
pub const REASON_TEST_MODE: &str = "test_mode";
/// macOS, not test mode, but no valid bundle (the `SparkleUpdater` state is absent —
/// a `cargo run`/unbundled dev/raw-harness binary). Updates need the installed .app.
pub const REASON_NO_BUNDLE: &str = "no_bundle";
/// Linux ships via apt — there is no in-app updater (the row is truthful, not present).
pub const REASON_LINUX_APT: &str = "linux_apt";

/// The platform dimension of the reason precedence — Linux is a distinct rollout
/// channel (apt), so it wins even in test mode (keeps the Linux render-gate cell
/// deterministic regardless of the harness's test-mode flag).
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum UpdaterOs {
    Macos,
    Linux,
}

impl UpdaterOs {
    /// The compile target's OS (only macOS + Linux are shed-desktop targets).
    pub fn current() -> Self {
        if cfg!(target_os = "macos") {
            UpdaterOs::Macos
        } else {
            UpdaterOs::Linux
        }
    }

    /// The wire string for the `os` field (NOT `platform` — that already means the
    /// client kind `"tauri"` in `identify`).
    pub fn as_str(self) -> &'static str {
        match self {
            UpdaterOs::Macos => "macos",
            UpdaterOs::Linux => "linux",
        }
    }
}

/// The reason the updater is in its current state, by strict precedence: platform
/// first (Linux ⇒ `linux_apt` ALWAYS, even in test mode), then on macOS test mode ⇒
/// `test_mode`, then a missing updater handle ⇒ `no_bundle`, else `ok`. Pure +
/// unit-tested — it's the disabled/enabled decision every surface reads.
pub fn updater_reason(os: UpdaterOs, test_mode: bool, have_updater: bool) -> &'static str {
    match os {
        UpdaterOs::Linux => REASON_LINUX_APT,
        UpdaterOs::Macos => {
            if test_mode {
                REASON_TEST_MODE
            } else if !have_updater {
                REASON_NO_BUNDLE
            } else {
                REASON_OK
            }
        }
    }
}

/// The Sparkle channels this build subscribes to: `Some(["beta"])` iff the build is a
/// prerelease (`CARGO_PKG_VERSION` contains `-`, e.g. `0.7.11-rc.1`) — so an rc build
/// receives beta appcast entries (making rc1→rc2 a REAL in-place Sparkle update test)
/// while a stable build stays stable-only (a promoted user never sees rc entries).
/// `Some(empty)` = stable-only (Sparkle always includes the default/no-channel items).
/// Returns the EXACT value handed to `set_allowed_channels` (an `Option<Vec<String>>`):
/// always `Some` today — the `Some`-vs-`None` distinction is load-bearing (a future
/// change to `None` means "unrestricted" and MUST break a test), so the call site
/// passes this through unmodified.
// Only called from the macOS-only `configure`; kept uncfg'd so its unit tests
// run everywhere (incl. the Linux Docker test leg).
#[cfg_attr(not(target_os = "macos"), allow(dead_code))]
pub fn allowed_channels(version: &str) -> Option<Vec<String>> {
    if version.contains('-') {
        Some(vec!["beta".to_string()])
    } else {
        Some(Vec::new())
    }
}

/// Whether a real, instantiated `SparkleUpdater` is managed (a valid bundle on
/// macOS). Always false off macOS (no such plugin) — the reason precedence
/// short-circuits Linux before this is consulted.
#[cfg(target_os = "macos")]
fn have_updater<R: Runtime>(app: &AppHandle<R>) -> bool {
    use tauri_plugin_sparkle_updater::SparkleUpdaterExt;
    app.sparkle_updater().is_some()
}
#[cfg(not(target_os = "macos"))]
fn have_updater<R: Runtime>(_app: &AppHandle<R>) -> bool {
    false
}

/// `updater_status`/`updater.status` payload: `{ os, enabled, reason, instantiated }`
/// where `enabled == (reason == "ok")`. `instantiated` is whether a real Sparkle
/// updater was instantiated — the `have_updater` probe (macOS: `sparkle_updater()`
/// resolves to a managed instance; always `false` off macOS). It's independent of
/// `reason` (which short-circuits on platform/test-mode BEFORE consulting the handle),
/// so the harness can PROVE test mode never instantiated Sparkle (`instantiated ==
/// false` while `reason == "test_mode"`).
pub fn status<R: Runtime>(app: &AppHandle<R>, test_mode: bool) -> Value {
    let os = UpdaterOs::current();
    let instantiated = have_updater(app);
    let reason = updater_reason(os, test_mode, instantiated);
    json!({
        "os": os.as_str(),
        "enabled": reason == REASON_OK,
        "reason": reason,
        "instantiated": instantiated,
    })
}

/// Why `check` did not present Sparkle: `Disabled(reason)` is the deterministic
/// policy path (test mode / no bundle / apt — the row is off by design), whereas
/// `Operational(msg)` is a runtime failure on the enabled path (e.g. main-thread
/// scheduling). The two map to different IPC error codes (`updater_disabled` vs the
/// `action_failed` convention), so callers must not collapse them into one string.
#[derive(Debug)]
pub enum CheckError {
    /// The updater is off by policy — `reason` is one of the `REASON_*` constants.
    Disabled(&'static str),
    /// The enabled path failed operationally (main-thread scheduling, etc.).
    Operational(String),
}

impl std::fmt::Display for CheckError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            // Unchanged wire string — the harness asserts `updater_disabled:<reason>`.
            CheckError::Disabled(reason) => write!(f, "updater_disabled:{reason}"),
            CheckError::Operational(msg) => write!(f, "updater_failed:{msg}"),
        }
    }
}

/// `updater_check`/`updater.check`: on the enabled path front the app + present
/// Sparkle; otherwise a `CheckError::Disabled(reason)` (so a caller — and the harness
/// — can assert disablement without a crash). A runtime failure on the enabled path is
/// `CheckError::Operational`.
pub fn check<R: Runtime>(app: &AppHandle<R>, test_mode: bool) -> Result<(), CheckError> {
    let os = UpdaterOs::current();
    let reason = updater_reason(os, test_mode, have_updater(app));
    if reason != REASON_OK {
        return Err(CheckError::Disabled(reason));
    }
    present_check(app).map_err(CheckError::Operational)
}

/// Configure the freshly-instantiated updater (macOS, non-test only — called from
/// `setup` after the plugin registered): disable automatic checks (belt-and-braces
/// with the plist `SUEnableAutomaticChecks=false` — the crate calls `startUpdater`
/// unconditionally) and restrict the allowed channels to this build's set. Both
/// setters route through the delegate/updater; `set_allowed_channels` is stored on
/// the delegate and read by `allowedChannelsForUpdater:` at check time, so applying
/// it here takes effect before any `check_for_updates()`. A no-op if the bundle is
/// invalid (the managed state is absent).
#[cfg(target_os = "macos")]
pub fn configure<R: Runtime>(app: &AppHandle<R>, version: &str) {
    use tauri_plugin_sparkle_updater::SparkleUpdaterExt;
    if let Some(updater) = app.sparkle_updater() {
        if let Err(e) = updater.set_automatically_checks_for_updates(false) {
            eprintln!("shed-desktop-tauri: sparkle set_automatically_checks_for_updates failed: {e}");
        }
        if let Err(e) = updater.set_allowed_channels(allowed_channels(version)) {
            eprintln!("shed-desktop-tauri: sparkle set_allowed_channels failed: {e}");
        }
    }
}

/// Front the app (Sparkle's Gap 1 — on a tray/accessory app the update window can
/// appear behind/unfocused) then present the check. Marshalled onto the main thread:
/// the IPC handler runs on a tokio worker, and both `NSApplication` and Sparkle must
/// be touched on the main thread. Fire-and-forget (Sparkle drives its own UI async);
/// the closure re-fetches the updater handle (a `State` can't cross the thread).
#[cfg(target_os = "macos")]
fn present_check<R: Runtime>(app: &AppHandle<R>) -> Result<(), String> {
    use tauri_plugin_sparkle_updater::SparkleUpdaterExt;
    let handle = app.clone();
    app.run_on_main_thread(move || {
        activate_frontmost();
        if let Some(updater) = handle.sparkle_updater() {
            if let Err(e) = updater.check_for_updates() {
                eprintln!("shed-desktop-tauri: sparkle check_for_updates failed: {e}");
            }
        }
    })
    .map_err(|e| e.to_string())
}
#[cfg(not(target_os = "macos"))]
fn present_check<R: Runtime>(_app: &AppHandle<R>) -> Result<(), String> {
    // Unreachable on the enabled path (Linux short-circuits to `linux_apt` above);
    // kept so both surfaces compile + return the deterministic disabled string.
    Err(format!("updater_disabled:{REASON_LINUX_APT}"))
}

/// Bring the app to the foreground so the Sparkle window is focused (mirrors how the
/// rest of the app does main-thread AppKit work). Must run on the main thread.
#[cfg(target_os = "macos")]
fn activate_frontmost() {
    use objc2::MainThreadMarker;
    use objc2_app_kit::NSApplication;
    if let Some(mtm) = MainThreadMarker::new() {
        let ns_app = NSApplication::sharedApplication(mtm);
        // Deprecated since macOS 14 in favor of `activate`, but it's the documented
        // ignoring-other-apps behavior and works across our minimumSystemVersion.
        #[allow(deprecated)]
        ns_app.activateIgnoringOtherApps(true);
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn reason_precedence_linux_wins_even_in_test_mode() {
        // Linux is the apt channel — deterministic regardless of test mode / handle.
        assert_eq!(updater_reason(UpdaterOs::Linux, false, false), REASON_LINUX_APT);
        assert_eq!(updater_reason(UpdaterOs::Linux, true, false), REASON_LINUX_APT);
        assert_eq!(updater_reason(UpdaterOs::Linux, true, true), REASON_LINUX_APT);
    }

    #[test]
    fn reason_precedence_macos() {
        // test mode wins over a missing handle; then no handle ⇒ no_bundle; else ok.
        assert_eq!(updater_reason(UpdaterOs::Macos, true, false), REASON_TEST_MODE);
        assert_eq!(updater_reason(UpdaterOs::Macos, true, true), REASON_TEST_MODE);
        assert_eq!(updater_reason(UpdaterOs::Macos, false, false), REASON_NO_BUNDLE);
        assert_eq!(updater_reason(UpdaterOs::Macos, false, true), REASON_OK);
    }

    #[test]
    fn all_four_reasons_are_reachable() {
        // Guards the enabled/tooltip contract: exactly one reason maps to enabled.
        let reasons = [
            updater_reason(UpdaterOs::Macos, false, true),
            updater_reason(UpdaterOs::Macos, true, true),
            updater_reason(UpdaterOs::Macos, false, false),
            updater_reason(UpdaterOs::Linux, false, true),
        ];
        assert_eq!(reasons, [REASON_OK, REASON_TEST_MODE, REASON_NO_BUNDLE, REASON_LINUX_APT]);
        assert_eq!(reasons.iter().filter(|r| **r == REASON_OK).count(), 1);
    }

    #[test]
    fn allowed_channels_beta_iff_prerelease() {
        // Assert the FULL Option value (not just the inner Vec) — the `Some`-vs-`None`
        // distinction is the load-bearing contract handed to `set_allowed_channels`
        // (`Some(empty)` = stable-only; a future `None` = unrestricted MUST fail here).
        assert_eq!(allowed_channels("0.7.10"), Some(Vec::<String>::new()));
        assert_eq!(allowed_channels("0.7.11-rc.1"), Some(vec!["beta".to_string()]));
        assert_eq!(allowed_channels("1.0.0-beta.2"), Some(vec!["beta".to_string()]));
        // A plain stable version never subscribes to beta — but is still `Some(empty)`.
        assert_eq!(allowed_channels("2.3.4"), Some(Vec::<String>::new()));
    }

    #[test]
    fn os_wire_strings() {
        assert_eq!(UpdaterOs::Macos.as_str(), "macos");
        assert_eq!(UpdaterOs::Linux.as_str(), "linux");
    }
}
