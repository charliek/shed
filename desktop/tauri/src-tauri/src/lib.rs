//! shed-desktop (Tauri) — a real Linux client toward Mac parity, on the shared
//! shed-core. Runs on Linux (WebKitGTK, the shipped target) and macOS (WKWebView,
//! the dev / UI-comparison loop vs the SwiftUI app).
//!
//! Thin entry: resolve the `SHED_TAURI_*` env, take the single-instance flock,
//! and in `setup` bind the JSON IPC server (the drivability North Star) on Tauri's
//! async runtime before the window paints — so a harness `identify` right after
//! launch succeeds.

mod approval;
mod env;
mod ipc;
mod prefs;
mod screenshot;
mod single_instance;
mod state;
mod termctl;
mod tray;

use std::collections::HashMap;
use std::sync::{Arc, Mutex};

use env::Env;
use ipc::{Handler, IpcServer};
use shed_app::traits::{AuthGateRef, NotifierRef};
use shed_app::{
    AlwaysApprovedGate, AuditStore, Backend, Coordinator, CoordinatorDeps, FakeNotifier,
    HelloClientInfo, HostAgentClient, HostAgentTokenMinter, RcService, SshPrefs,
};
use shed_core::approval::{
    ApprovalChoice, ApprovalDecision, ApprovalMethod, ApprovalScope, SshApprovalPolicy,
};
use shed_core::models::CreateShedRequest;
use shed_core::rc::RcKind;
use shed_core::token::TokenMinter;
use single_instance::AcquireError;
use state::{SharedUi, UiState};
use tauri::Manager;

/// A React frontend reports its rendered snapshot (`{pane, style, sheds,
/// refresh_token}`) here, so the harness reads the real rendered state over IPC
/// (`ui.current_pane` / `ui.computed_style` / `dashboard.dump` / `tray.dump`).
/// Invoked from `useUiBridge` on mount + every render. Keyed by the calling
/// WINDOW's label so the mac popover (`popover`) can't clobber the dashboard
/// (`main`) — see [`state::UiState`]. One JSON blob per window, not a field per op.
#[tauri::command]
fn ui_report(
    ui: tauri::State<'_, SharedUi>,
    window: tauri::WebviewWindow,
    snapshot: serde_json::Value,
) {
    // Mirror the running-shed count onto the menu-bar status item (Swift parity).
    // The dashboard (`main`) reports the full shed list here even while hidden at
    // launch, so the tray count is live without a Rust-side poller. macOS-only;
    // computed before the snapshot is moved into `merge`.
    #[cfg(target_os = "macos")]
    if window.label() == "main" {
        let running = snapshot
            .get("sheds")
            .and_then(|v| v.as_array())
            .map(|sheds| {
                sheds
                    .iter()
                    .filter(|s| s.get("status").and_then(|v| v.as_str()) == Some("running"))
                    .count()
            })
            .unwrap_or(0);
        crate::tray::update_running_count(window.app_handle(), running);
    }
    if let Ok(mut s) = ui.lock() {
        s.merge(window.label(), snapshot);
    }
}

/// The WebView's live shed list — `invoke("list_sheds")` on mount + on each
/// `refresh` event. Returns host-stamped sheds (all configured hosts, concurrently
/// via the shared `Backend`); the harness reads the same data via the `sheds.list`
/// IPC op.
#[tauri::command]
async fn list_sheds(backend: tauri::State<'_, Arc<Backend>>) -> Result<serde_json::Value, String> {
    Ok(serde_json::json!(backend.list_sheds().await))
}

/// The configured hosts a create can target (the New-Shed dialog's picker) — even
/// hosts with no sheds yet, unlike the sidebar's sheds-derived list.
#[tauri::command]
fn list_hosts(backend: tauri::State<'_, Arc<Backend>>) -> Vec<String> {
    backend.host_names()
}

// -- create (the New-Shed dialog; the harness drives the parallel create.* ops) --

/// The New-Shed dialog's form (`vm_backend` avoids clashing with the shed backend).
#[derive(serde::Deserialize)]
struct CreateForm {
    name: String,
    host: Option<String>,
    image: Option<String>,
    vm_backend: Option<String>,
    cpus: Option<i64>,
    memory_mb: Option<i64>,
    repo: Option<String>,
}

/// Kick off a create; returns the id the dialog polls via `create_status`. The
/// SSE stream runs on Tauri's tokio runtime.
#[tauri::command]
async fn create_start(
    backend: tauri::State<'_, Arc<Backend>>,
    form: CreateForm,
) -> Result<String, String> {
    let req = CreateShedRequest {
        name: form.name,
        repo: form.repo,
        local_dir: None,
        image: form.image,
        backend: form.vm_backend,
        cpus: form.cpus,
        memory_mb: form.memory_mb,
        no_provision: None,
    };
    backend
        .create_start(
            &tokio::runtime::Handle::current(),
            form.host.as_deref(),
            req,
        )
        .map_err(|e| e.to_string())
}

/// The in-flight create's progress snapshot (`{id,state,messages,shed,error}`), or
/// `{state:"unknown"}` once it's cancelled/gone.
#[tauri::command]
fn create_status(backend: tauri::State<'_, Arc<Backend>>, create_id: String) -> serde_json::Value {
    backend
        .create_status(&create_id)
        .map(|p| serde_json::json!(p))
        .unwrap_or_else(|| serde_json::json!({ "state": "unknown" }))
}

/// Abort a create's stream + drop its state (idempotent).
#[tauri::command]
fn create_cancel(backend: tauri::State<'_, Arc<Backend>>, create_id: String) {
    backend.create_cancel(&create_id);
}

/// A lifecycle action from a shed card's buttons (`start`/`stop`/`reset`/`delete`).
/// The frontend re-fetches (a `sheds.refresh`) after it resolves; the harness
/// drives the same via the `shed.*` IPC ops.
#[tauri::command]
async fn shed_action(
    backend: tauri::State<'_, Arc<Backend>>,
    action: String,
    name: String,
    host: Option<String>,
) -> Result<(), String> {
    backend
        .shed_action(host.as_deref(), &name, &action)
        .await
        .map_err(|e| e.to_string())
}

/// The WebView's live per-host disk usage — `invoke("system_df")` when the System
/// pane mounts / on its Refresh. Each row is a host's `SystemDiskUsage` or the
/// error it returned (unreachable hosts are kept, not dropped). The harness reads
/// the same via the `system.df` IPC op.
#[tauri::command]
async fn system_df(backend: tauri::State<'_, Arc<Backend>>) -> Result<serde_json::Value, String> {
    Ok(serde_json::json!(backend.system_df().await))
}

// -- terminal + prefs commands (the frontend Preferences view + the shed-card
//    "Open in Terminal" button; the same TerminalCtl the IPC ops use) -----------

/// The offerable terminal presets + install detection (the picker's source).
#[tauri::command]
fn terminal_presets(terminal: tauri::State<'_, termctl::SharedTerminal>) -> serde_json::Value {
    terminal.presets()
}

/// The persisted prefs (seeds the Preferences view).
#[tauri::command]
fn get_prefs(terminal: tauri::State<'_, termctl::SharedTerminal>) -> serde_json::Value {
    terminal.prefs_get()
}

/// Persist the chosen terminal preset (+ optional custom template).
#[tauri::command]
fn set_terminal_pref(
    terminal: tauri::State<'_, termctl::SharedTerminal>,
    preset: String,
    template: Option<String>,
) -> Result<(), String> {
    terminal
        .prefs_set_terminal(&preset, template)
        .map(|_| ())
        .map_err(|(_code, msg)| msg)
}

/// Open a shed in the user's chosen terminal (the persisted pref). Gated off in
/// test mode — the button never spawns under the harness.
#[tauri::command]
fn open_terminal(
    terminal: tauri::State<'_, termctl::SharedTerminal>,
    env: tauri::State<'_, Env>,
    shed: String,
    host: Option<String>,
    session: Option<String>,
) -> Result<(), String> {
    if env.test_mode {
        return Err("terminal.open is disabled in test mode (use terminal.preview)".to_string());
    }
    terminal
        .open(host.as_deref(), &shed, session.as_deref(), None, None)
        .map(|_| ())
        .map_err(|(_code, msg)| msg)
}

// -- approvals (the frontend Approvals/Activity panes + approval prefs) --------

/// The pending approval cards (each with gate + scope/TTL defaults). The pane
/// The Agents pane launches/lists/kills RC sessions over these invoke commands
/// (the harness drives the same ops over the IPC socket). The shed→ssh target
/// resolution stays in `Backend`; `RcService` owns the store + process seam.
#[tauri::command]
async fn rc_list(
    backend: tauri::State<'_, Arc<Backend>>,
    rc: tauri::State<'_, Arc<RcService>>,
    host: Option<String>,
    shed: Option<String>,
) -> Result<serde_json::Value, String> {
    let targets = backend.rc_targets(host.as_deref(), shed.as_deref()).await;
    Ok(serde_json::json!({
        "sessions": rc.list(targets, host.as_deref(), shed.as_deref()).await
    }))
}

#[tauri::command]
#[allow(clippy::too_many_arguments)] // a flat invoke arg list mirrors the launch form fields
async fn rc_launch(
    backend: tauri::State<'_, Arc<Backend>>,
    rc: tauri::State<'_, Arc<RcService>>,
    shed: String,
    kind: RcKind,
    host: Option<String>,
    display_name: Option<String>,
    workdir: Option<String>,
    initial_prompt: Option<String>,
) -> Result<serde_json::Value, String> {
    let target = backend
        .resolve_rc_target(host.as_deref())
        .map_err(|e| e.to_string())?;
    let session = rc
        .launch(target, &shed, kind, display_name, workdir, initial_prompt)
        .await
        .map_err(|e| e.to_string())?;
    Ok(serde_json::json!(session))
}

#[tauri::command]
async fn rc_kill(
    backend: tauri::State<'_, Arc<Backend>>,
    rc: tauri::State<'_, Arc<RcService>>,
    shed: String,
    slug: String,
    host: Option<String>,
) -> Result<(), String> {
    let target = backend
        .resolve_rc_target(host.as_deref())
        .map_err(|e| e.to_string())?;
    rc.kill(target, &shed, &slug).await.map_err(|e| e.to_string())
}

/// re-fetches on the `approvals-changed` event (see TauriEventSink).
#[tauri::command]
async fn approvals_list(
    coordinator: tauri::State<'_, Coordinator>,
) -> Result<serde_json::Value, String> {
    Ok(serde_json::json!(coordinator.approvals_list().await))
}

/// Approve/deny a pending request (the card's buttons). Tauri deserializes the
/// enum args from their wire strings ("approve"|"deny", "per-request"|…) via serde.
#[tauri::command]
async fn approval_decide(
    coordinator: tauri::State<'_, Coordinator>,
    id: String,
    decision: ApprovalDecision,
    scope: Option<ApprovalScope>,
    ttl: Option<String>,
    persist: Option<bool>,
) -> Result<(), String> {
    coordinator
        .decide_approval(
            id,
            ApprovalChoice {
                decision,
                scope,
                ttl,
                persist: persist.unwrap_or(false),
            },
        )
        .await;
    Ok(())
}

/// The merged audit feed (most-recent-first). Re-fetched on `activity-changed`.
#[tauri::command]
async fn activity_list(
    coordinator: tauri::State<'_, Coordinator>,
    limit: Option<usize>,
) -> Result<serde_json::Value, String> {
    Ok(serde_json::json!(coordinator.activity_list(limit.unwrap_or(200)).await))
}

/// The namespaces the host agent delegates to us (drives which approval-prefs
/// sections show + the "host agent · connected" indicator).
#[tauri::command]
async fn gate_namespaces(coordinator: tauri::State<'_, Coordinator>) -> Result<Vec<String>, String> {
    Ok(coordinator.gate_namespaces().await)
}

/// Serialize the coordinator's typed SSH prefs to their wire strings (the same
/// `{method, policy, ttl}` shape the UI reads back), so persistence round-trips
/// through serde rather than a hand-maintained enum→string match.
pub(crate) fn ssh_prefs_wire(p: &SshPrefs) -> (String, String, String) {
    let v = serde_json::to_value(p).unwrap_or_default();
    let field = |k: &str| v.get(k).and_then(|x| x.as_str()).unwrap_or_default().to_string();
    (field("method"), field("policy"), field("ttl"))
}

/// Rebuild `SshPrefs` from the persisted wire strings, parsing each enum via serde
/// and falling back to the default for any absent/unparseable field — so an old or
/// corrupt prefs.json never panics and never blocks startup.
fn ssh_prefs_from_store(store: &prefs::PrefsStore) -> SshPrefs {
    let stored = store.get();
    let mut ssh = SshPrefs::default();
    if let Some(m) = stored.ssh_method.as_deref().and_then(parse_wire::<ApprovalMethod>) {
        ssh.method = m;
    }
    if let Some(p) = stored
        .ssh_policy
        .as_deref()
        .and_then(parse_wire::<SshApprovalPolicy>)
    {
        ssh.policy = p;
    }
    if let Some(t) = stored.ssh_ttl.filter(|t| !t.is_empty()) {
        ssh.ttl = t;
    }
    ssh
}

/// Parse a serde enum from its wire string (`"time-based-allow"` → the variant),
/// `None` on any value the current build doesn't recognize.
fn parse_wire<T: serde::de::DeserializeOwned>(s: &str) -> Option<T> {
    serde_json::from_value(serde_json::Value::String(s.to_string())).ok()
}

/// Apply SSH approval prefs (method/policy/TTL) + re-evaluate the pending queue,
/// then persist so the choice survives a restart. Tauri deserializes the enum args
/// from their wire strings via serde.
#[tauri::command]
async fn set_ssh_approval(
    coordinator: tauri::State<'_, Coordinator>,
    prefs: tauri::State<'_, prefs::SharedPrefs>,
    method: Option<ApprovalMethod>,
    policy: Option<SshApprovalPolicy>,
    ttl: Option<String>,
) -> Result<(), String> {
    coordinator.set_ssh_approval(method, policy, ttl).await;
    // Persist the coordinator's RESULTING prefs (reading them back composes this
    // command's partial update with the existing method/policy/TTL) so a restart
    // rehydrates exactly what the running coordinator holds.
    let (m, p, t) = ssh_prefs_wire(&coordinator.ssh_prefs().await);
    prefs.set_ssh(m, p, t);
    Ok(())
}

/// The current SSH approval prefs (`{method, policy, ttl}`) — drives the
/// Preferences dropdown so it reflects the running coordinator, not a guess.
#[tauri::command]
async fn ssh_prefs_get(
    coordinator: tauri::State<'_, Coordinator>,
) -> Result<serde_json::Value, String> {
    Ok(serde_json::json!(coordinator.ssh_prefs().await))
}

// -- launch-at-login (B4) ------------------------------------------------------

/// The macOS test-mode login-item state. A real login-item write on macOS hits a
/// LaunchAgent / TCC — NOT hermetic — so under the harness we round-trip through
/// this in-memory cell instead of the OS. On Linux the harness redirects HOME
/// (which is what `auto-launch` keys its `$HOME/.config/autostart` write off — it
/// ignores `XDG_CONFIG_HOME`), so the real `.desktop` write IS contained + hermetic,
/// and this cell is unused (real enable/disable runs, exercising the shipped path).
struct LoginItemCell(Mutex<bool>);

/// Whether login-item writes must be faked: macOS under the harness only. Elsewhere
/// (Linux tests → redirected HOME/XDG; any production build) the real `auto-launch`
/// path runs.
fn login_item_faked(env: &Env) -> bool {
    env.test_mode && cfg!(target_os = "macos")
}

/// Whether the app is registered to launch at login (best-effort — a query error
/// reads as `false`, never a crash).
pub(crate) fn login_item_enabled(app: &tauri::AppHandle, env: &Env) -> bool {
    if login_item_faked(env) {
        return *app.state::<LoginItemCell>().0.lock().unwrap();
    }
    use tauri_plugin_autostart::ManagerExt;
    app.autolaunch().is_enabled().unwrap_or(false)
}

/// Enable/disable launch-at-login. Guarded to the in-memory cell under the macOS
/// harness (both true AND false suppress the real write); a real, hermetic write on
/// Linux/production.
pub(crate) fn login_item_set(
    app: &tauri::AppHandle,
    env: &Env,
    enabled: bool,
) -> Result<(), String> {
    if login_item_faked(env) {
        *app.state::<LoginItemCell>().0.lock().unwrap() = enabled;
        return Ok(());
    }
    use tauri_plugin_autostart::ManagerExt;
    if enabled {
        // auto-launch 0.5.0 writes `$HOME/.config/autostart/<app>.desktop` with a
        // single-level `create_dir` (it hard-codes `$HOME/.config`, ignoring
        // `XDG_CONFIG_HOME`), so a HOME whose `.config` doesn't exist yet makes
        // `enable()` fail ENOENT on the missing parent — the render gate's throwaway
        // HOME hits exactly this, and so would a real user missing `~/.config`.
        // Ensure the parent exists first (the render gate caught this).
        #[cfg(target_os = "linux")]
        if let Some(home) = std::env::var_os("HOME") {
            let _ = std::fs::create_dir_all(std::path::Path::new(&home).join(".config"));
        }
        app.autolaunch().enable()
    } else {
        app.autolaunch().disable()
    }
    .map_err(|e| e.to_string())
}

/// The launch-at-login state (the Preferences "General" toggle reads this on mount
/// + reconciles to it after a set).
#[tauri::command]
fn loginitem_status(app: tauri::AppHandle, env: tauri::State<'_, Env>) -> serde_json::Value {
    serde_json::json!({ "enabled": login_item_enabled(&app, &env) })
}

/// Set launch-at-login (the toggle). Returns an error string on a failed write so
/// the toggle reconciles from `loginitem_status` rather than silently lying.
#[tauri::command]
fn loginitem_set(
    app: tauri::AppHandle,
    env: tauri::State<'_, Env>,
    enabled: bool,
) -> Result<(), String> {
    login_item_set(&app, &env, enabled)
}

// -- menu-bar popover footer commands (B1b) — a 2nd webview can't call the IPC ops
//    or emit the main-window events, so the footer invokes these dedicated commands
//    (test-mode-safe: they only show/emit/exit, never spawn or write). --

/// The popover footer's "Open dashboard" → raise the dashboard on Sheds + dismiss.
#[tauri::command]
fn open_dashboard(app: tauri::AppHandle) {
    tray::open_dashboard(&app);
}

/// The popover footer's "Preferences…" → raise the dashboard + open Preferences.
#[tauri::command]
fn open_preferences(app: tauri::AppHandle) {
    tray::open_preferences(&app);
}

/// The popover footer's "Quit".
#[tauri::command]
fn app_exit(app: tauri::AppHandle) {
    app.exit(0);
}

/// Content-size the menu-bar popover (Swift `NSPopover` parity — no dead space): the
/// popover webview measures its rendered content height and reports it here; the width
/// stays fixed and the height is clamped. Targets the `popover` window EXPLICITLY, so
/// it can only ever resize the popover — a stray call from another window can't resize
/// the dashboard. macOS-only; a no-op stub keeps the Linux build + the handler list
/// compiling (the popover window is never created off macOS).
#[cfg(target_os = "macos")]
#[tauri::command]
fn resize_popover(app: tauri::AppHandle, height: f64) {
    if let Some(win) = app.get_webview_window(tray::POPOVER_ID) {
        let h = height.clamp(tray::POPOVER_MIN_HEIGHT, tray::POPOVER_MAX_HEIGHT);
        let _ = win.set_size(tauri::LogicalSize::new(tray::POPOVER_WIDTH, h));
    }
}
#[cfg(not(target_os = "macos"))]
#[tauri::command]
fn resize_popover(_app: tauri::AppHandle, _height: f64) {}

#[cfg_attr(mobile, tauri::mobile_entry_point)]
pub fn run() {
    let env = Env::from_process();

    // Single-instance: flock a pidfile (keyed to the socket's runtime dir) BEFORE
    // binding the socket, so a second launch never unlinks the live socket. On
    // contention, raise the running instance (an `app.activate` IPC) and exit.
    // Scoped to the runtime dir, so hermetic test runs don't collide — unlike an
    // identifier-scoped plugin. This flock-before-bind ordering is what makes
    // `IpcServer::bind`'s unconditional stale-socket remove safe. `_instance` holds
    // the lock (drops → releases) for the whole app run.
    let lock_path = single_instance::lock_path_for(&env.socket_path);
    let _instance = match single_instance::acquire(&lock_path) {
        Ok(lock) => Some(lock),
        Err(AcquireError::AlreadyHeld(pid)) => {
            eprintln!("shed-desktop-tauri: already running (pid {pid}); activating it");
            let _ = single_instance::activate_running_instance(&env.socket_path);
            return;
        }
        Err(AcquireError::Io(e)) => {
            // Can't determine instance state — proceed rather than block the user
            // (the flock is best-effort robustness, not a correctness gate).
            eprintln!("shed-desktop-tauri: single-instance check failed ({e}); continuing");
            None
        }
    };

    // Shared with the IPC handler so `ui.current_pane` / `ui.computed_style` /
    // `dashboard.dump` read what the frontend reported.
    let ui: SharedUi = Arc::new(Mutex::new(UiState::default()));

    // The host-agent connection (approvals + the all-namespace audit feed) + the
    // control-token minter it backs. Construct BEFORE the Backend so each secure
    // (non-mock) server's client mints its bearer via the agent's token.get (C2;
    // fail-closed on a mint failure). The client CONNECTS in `setup`.
    let clock = shed_app::traits::system_clock();
    let host = HostAgentClient::new(env.host_agent_socket.clone(), clock.clone());
    // Minting is for real (non-mock) servers only — the hermetic mock is tokenless.
    let minter: Option<Arc<dyn TokenMinter>> = env
        .mock_base_url
        .is_none()
        .then(|| Arc::new(HostAgentTokenMinter::new(host.clone())) as Arc<dyn TokenMinter>);

    // One shared shed-core-backed Backend behind both surfaces: the WebView's
    // `invoke` commands (list_sheds/shed_action) and the harness's IPC ops
    // (sheds.*/shed.*/create.*). Hermetic in test mode (every host → the mock).
    let backend = Arc::new(Backend::from_env_parts_with_minter(
        env.test_mode,
        env.mock_base_url.as_deref(),
        &env.config_path,
        minter.as_ref(),
    ));

    // The Agents / Remote-Control service (session store + process seam). Same
    // test-mode flag as the coordinator fakes — test mode synthesizes sessions;
    // the real path shells out `shed-ext-rc` over SSH.
    let rc_service = Arc::new(RcService::new_default(env.test_mode, env!("CARGO_PKG_VERSION")));

    tauri::Builder::default()
        // Launch-at-login (B4): register the plugin so `app.autolaunch()` resolves;
        // it does NOT enable autostart on its own (no startup side effect). The
        // React toggle drives our guarded `loginitem_*` commands, not the plugin's
        // JS API — so a test-mode write can't bypass the guard.
        .plugin(tauri_plugin_autostart::init(
            tauri_plugin_autostart::MacosLauncher::LaunchAgent,
            None,
        ))
        // Anchors the mac menu-bar popover at the tray icon (B1b); needs the tray
        // event forwarded (see `tray.rs::build`'s `on_tray_icon_event`).
        .plugin(tauri_plugin_positioner::init())
        .manage(ui.clone())
        .manage(backend.clone())
        .manage(env.clone())
        .manage(rc_service.clone())
        // The macOS test-mode login-item cell (see [`LoginItemCell`]).
        .manage(LoginItemCell(Mutex::new(false)))
        .invoke_handler(tauri::generate_handler![
            ui_report,
            list_sheds,
            list_hosts,
            shed_action,
            system_df,
            create_start,
            create_status,
            create_cancel,
            terminal_presets,
            get_prefs,
            set_terminal_pref,
            rc_list,
            rc_launch,
            rc_kill,
            open_terminal,
            approvals_list,
            approval_decide,
            activity_list,
            gate_namespaces,
            set_ssh_approval,
            ssh_prefs_get,
            loginitem_status,
            loginitem_set,
            open_dashboard,
            open_preferences,
            app_exit,
            resize_popover
        ])
        .setup(move |app| {
            // The bundled terminal openers live in <resources>/bin; None in an
            // unbundled dev/test run — resolve_launch then falls back to a default
            // terminal (and terminal.open is disabled in test mode regardless).
            let scripts_dir = app
                .path()
                .resource_dir()
                .ok()
                .map(|d| d.join("bin"))
                .filter(|d| d.exists())
                .map(|d| d.to_string_lossy().into_owned());
            // Persisted prefs (terminal preset + template) in the app config dir
            // ($XDG_CONFIG_HOME/<id> on Linux; the harness redirects it, so the
            // file is hermetic in test mode).
            let prefs_path = app
                .path()
                .app_config_dir()
                .unwrap_or_else(|_| std::path::PathBuf::from("."))
                .join("prefs.json");
            let prefs: prefs::SharedPrefs = Arc::new(prefs::PrefsStore::load(prefs_path));
            // Managed so `set_ssh_approval` (and its IPC twin) can write the chosen
            // SSH prefs through the same store.
            app.manage(prefs.clone());
            // The terminal ops (preset resolution, launch, detection, the pref),
            // shared by the IPC handler + the frontend invoke commands.
            let terminal: termctl::SharedTerminal = Arc::new(termctl::TerminalCtl::new(
                backend.clone(),
                prefs.clone(),
                scripts_dir,
            ));
            app.manage(terminal.clone());

            // The approval spine: start the host-agent connection (its event stream
            // feeds the coordinator), pick the seam impls (test-mode fakes vs the
            // prod stubs — the real native gate + notifier land in B6), spawn the
            // coordinator actor + its 1s expiry tick. The audit log lives under the
            // app data dir (redirected + hermetic in test mode).
            let hello = HelloClientInfo {
                name: "shed-desktop".to_string(),
                version: env!("CARGO_PKG_VERSION").to_string(),
                pid: std::process::id() as i32,
                capabilities: vec!["approval.ssh".to_string(), "event.stream".to_string()],
                replay_events: 50,
            };
            let (notifier, gate): (NotifierRef, AuthGateRef) = if env.test_mode {
                (Arc::new(FakeNotifier::new()), Arc::new(AlwaysApprovedGate))
            } else {
                // Linux: real polkit gate + zbus D-Bus notifier; other targets: the
                // fail-closed stubs (the Tauri client's native gate is Linux-only).
                approval::production_seams()
            };
            let audit = AuditStore::new(
                app.path()
                    .app_data_dir()
                    .unwrap_or_else(|_| std::path::PathBuf::from("."))
                    .join("audit.jsonl"),
            );
            let coord_clock = clock.clone();
            // Hydrate the SSH approval prefs from the persisted store (falling back
            // to the default on an absent/corrupt file), so the user's choice
            // survives a restart rather than resetting to the coordinator default.
            let ssh_prefs = ssh_prefs_from_store(&prefs);
            // Pushes coordinator changes to the webview (app.emit) so the
            // Approvals/Activity panes re-fetch reactively.
            let coord_sink: shed_app::traits::EventSinkRef =
                Arc::new(approval::TauriEventSink::new(app.handle().clone()));
            // Start the client loop + coordinator actor + expiry tick INSIDE the
            // Tauri (tokio) runtime — tokio::spawn needs a runtime context, and the
            // setup hook itself has none (the same reason the IPC bind below uses
            // block_on). The spawned tasks are detached and outlive the block.
            let coordinator = tauri::async_runtime::block_on(async move {
                let host_events = host.start(hello);
                let responder: shed_app::traits::ResponderRef = Arc::new(host);
                let coordinator = Coordinator::spawn(
                    CoordinatorDeps {
                        responder,
                        notifier,
                        gate,
                        clock: coord_clock,
                        sink: coord_sink,
                        audit,
                        ssh: ssh_prefs,
                        extra_rules: Vec::new(),
                        provider_modes: HashMap::new(),
                    },
                    host_events,
                );
                coordinator.start_expiry_tick();
                coordinator
            });
            app.manage(coordinator.clone());

            let handler = Handler::new(
                env.clone(),
                app.handle().clone(),
                ui.clone(),
                backend.clone(),
                terminal,
                coordinator,
                rc_service.clone(),
                prefs,
            );
            // block_on enters Tauri's tokio runtime so tokio's UnixListener can
            // register with the reactor; then serve on the same runtime.
            let server = tauri::async_runtime::block_on(IpcServer::bind(&env.socket_path, handler))
                .map_err(|e| {
                    format!(
                        "bind shed-desktop IPC server at {}: {e}",
                        env.socket_path.display()
                    )
                })?;
            tauri::async_runtime::spawn(async move { server.run().await });

            // The system tray. Best-effort: a headless / no-SNI host has nowhere to
            // show it, so a failure logs and the app keeps running (the window is
            // always reachable).
            if let Err(e) = tray::build(app.handle()) {
                eprintln!("shed-desktop-tauri: tray unavailable ({e}); window-only");
            }

            // The mac menu-bar popover (B1b): a 2nd, opaque webview mirroring the
            // Swift MenuBarContentView, created HIDDEN + anchored at the tray on a
            // left-click. macOS-only (Linux emits no tray click events / has no
            // popover). It fetches its own data + reports under the `popover` window
            // key, so it never clobbers the dashboard's `main` snapshot.
            #[cfg(target_os = "macos")]
            {
                if let Err(e) = tauri::WebviewWindowBuilder::new(
                    app.handle(),
                    tray::POPOVER_ID,
                    tauri::WebviewUrl::App("popover.html".into()),
                )
                .title("shed")
                // Built at MAX height; the webview content-sizes it down via
                // `resize_popover` (Swift NSPopover parity). `resizable(true)` is
                // REQUIRED — a borderless NSWindow without it silently ignores
                // programmatic `set_size`. It stays borderless + dismiss-on-blur, so
                // there are no visible resize handles and the user can't drag it.
                .inner_size(tray::POPOVER_WIDTH, tray::POPOVER_MAX_HEIGHT)
                .decorations(false) // borderless
                // TRANSPARENT so the webview can round its own 12px corners + cast a
                // window shadow; the card itself paints an OPAQUE `--shed-surface` fill
                // (a clean, consistent near-white — the maintainer preferred that over
                // the native `Popover` vibrancy material, which tints with the desktop
                // behind it and reads gray, 2026-07-07).
                .transparent(true)
                .shadow(true)
                .always_on_top(true)
                .skip_taskbar(true)
                .resizable(true)
                .visible(false)
                .focused(false)
                .build()
                {
                    eprintln!("shed-desktop-tauri: popover window unavailable ({e})");
                }
                // Menu-bar-first in PRODUCTION: hide the dashboard at launch + become
                // an accessory (no Dock icon), matching the Swift app. "Open dashboard"
                // brings it back (Regular). Guarded so the harness keeps `main` shown +
                // never flips policy (else the webview may not mount → ui_report never
                // fires → wait_until(current_pane) times out).
                if !env.test_mode {
                    if let Some(main) = app.get_webview_window("main") {
                        let _ = main.hide();
                    }
                    ipc::set_activation_policy_prod(app.handle(), false);
                }
            }
            Ok(())
        })
        .build(tauri::generate_context!())
        .expect("build shed-desktop tauri app")
        .run(|app_handle, event| match event {
            // Menu-bar/tray behavior: closing the window HIDES it (the app lives in
            // the tray); a deliberate Quit (tray → app.exit(0)) still exits.
            tauri::RunEvent::WindowEvent {
                label,
                event: tauri::WindowEvent::CloseRequested { api, .. },
                ..
            } => {
                if let Some(w) = app_handle.get_webview_window(&label) {
                    let _ = w.hide();
                }
                // Closing the dashboard → menu-bar-first (Accessory, no Dock icon) in
                // production; the popover isn't a normal window (no policy effect).
                if label == "main" {
                    ipc::set_activation_policy_prod(app_handle, false);
                }
                api.prevent_close();
            }
            // Dismiss the popover on blur (a click outside it) — gated on the label,
            // so a Focused(false) on `main` never hides the dashboard.
            tauri::RunEvent::WindowEvent {
                label,
                event: tauri::WindowEvent::Focused(false),
                ..
            } if label == tray::POPOVER_ID => {
                if let Some(w) = app_handle.get_webview_window(&label) {
                    let _ = w.hide();
                }
            }
            // An auto-exit (e.g. the last window closed) is prevented so we stay in
            // the tray; a deliberate exit carries a code and is allowed through.
            tauri::RunEvent::ExitRequested { code, api, .. } if code.is_none() => {
                api.prevent_exit();
            }
            _ => {}
        });
}
