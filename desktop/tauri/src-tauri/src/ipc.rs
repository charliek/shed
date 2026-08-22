//! The IPC server: newline-delimited JSON over a Unix socket — the same envelope
//! the shed-desktop harness + `shedctl` speak (`{id, op, params}` in, `{id, ok,
//! result}` / `{id, ok:false, error:{code,message}}` out). Making the app drivable
//! + observable by an agent over IPC is the North Star.
//!
//! Window/UI ops (`identify` / `ui.navigate` / `ui.show_window` / `app.activate` /
//! `app.screenshot` / the `ui.*` truth reads) go straight through the Tauri
//! `AppHandle` (its methods are thread-safe) or the shared [`SharedUi`], so —
//! unlike GTK — no main-thread marshalling channel is needed. The backend ops
//! (`sheds.*` / `shed.*` / `create.*`, A1b) run on the shared [`Backend`], and
//! `dashboard.dump` reads the sheds — plus the host-error strip + empty state —
//! the frontend reported (UI truth, not a backend re-query), which is why
//! `sheds.refresh` round-trips through the frontend before returning.

use std::path::Path;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::Arc;
use std::time::{Duration, Instant};

use base64::Engine as _;
use serde_json::{json, Value};
use tauri::{AppHandle, Emitter, Manager};
use tokio::io::{AsyncReadExt, AsyncWriteExt, BufReader};
use tokio::net::{UnixListener, UnixStream};
use tokio::runtime::Handle;

use shed_app::{Backend, Coordinator, RcService, Reachability};
use shed_core::approval::{
    ApprovalChoice, ApprovalDecision, ApprovalMethod, ApprovalScope, PolicyRule, SshApprovalPolicy,
};
use shed_core::models::CreateShedRequest;
use shed_core::rc::{self, RcError, RcKind, RcSession, RcState};

use crate::env::Env;
use crate::prefs::SharedPrefs;
use crate::state::SharedUi;
use crate::termctl::SharedTerminal;

/// A request line is tiny; cap it so a local client can't force unbounded
/// buffering with a huge/unterminated frame.
const MAX_FRAME_BYTES: usize = 1 << 20; // 1 MiB
/// Upper bound on how long `sheds.refresh` waits for the frontend to re-fetch +
/// re-report before returning anyway (best-effort — a missing/slow WebView must
/// not hang the op). The frontend round-trip is a mock HTTP fetch + a render.
const REFRESH_WAIT: Duration = Duration::from_secs(10);
/// Poll cadence while waiting for that frontend echo.
const REFRESH_POLL: Duration = Duration::from_millis(15);

/// Build an `(code, message)` error pair for the IPC error envelope.
fn err(code: &str, message: impl Into<String>) -> (String, String) {
    (code.to_string(), message.into())
}

/// The sheds listing payload — `{sheds, host_errors}` — the one shape every
/// listing answer takes: the `sheds.list` IPC op, the frontend's `list_sheds`
/// command, and (from the frontend's committed snapshot rather than a fresh
/// query) `sheds.refresh` / `dashboard.dump`. So the harness/`shedctl` and the
/// WebView can never see a different shape.
///
/// `host_errors` is ADDITIVE (plan 006 D6 / shed#300): the `sheds` array keeps its
/// pre-existing contract (healthy hosts' sheds, in host order), and the per-host
/// failures that [`Backend::list_sheds`] drops on the floor are carried beside it
/// — plural, because one failed host is not the only case. Healthy = `[]`, never
/// absent, so a consumer can read it unconditionally.
/// **The single shaper for `rc.list`** — shed sessions, machine sessions, the
/// per-shed capabilities, and each machine's health.
///
/// Shared by the socket IPC op and the frontend's `rc_list` Tauri command, the
/// same way [`sheds_payload`] is shared. Those two used to build this payload
/// independently, with a comment claiming they matched; adding machine sessions
/// to one and not the other made the pane render nothing while the IPC op was
/// correct — exactly the divergence a single shaper prevents.
pub(crate) async fn rc_list_payload(
    backend: &Backend,
    rc_service: &RcService,
    machines: &crate::machines::Machines,
    host: Option<&str>,
    shed: Option<&str>,
) -> Value {
    let targets = backend.rc_targets(host, shed).await;
    let sessions = rc_service.list(targets, host, shed).await;
    // The per-shed capabilities captured during the probe, keyed by `host/shed`,
    // let the launch form gate which kinds it offers (unknown/uninstalled agents
    // are excluded; a shed with an old binary is simply absent → the UI degrades
    // to claude+shell).
    let capabilities = rc_service.capabilities(host, shed);

    // **Machine sessions join the SAME payload** (plan 012 R4). A separate op
    // would force the UI to merge two async sources and reintroduce exactly the
    // split the unified view exists to remove; the two reach paths already
    // produce the same session type, so the only real difference is provenance,
    // which each row carries as `origin`.
    //
    // A host/shed filter is a SHED filter — it never narrows machines, because a
    // machine belongs to no server. When one is given the caller is asking about
    // a specific shed, so machines are omitted entirely.
    // ONE lock acquisition for both halves — see `Machines::snapshot`: reading
    // them separately can produce a frame where a row is `stale: false` while
    // its machine is `reachable: false`.
    let (machine_sessions, machine_status) = if host.is_none() && shed.is_none() {
        machines.snapshot()
    } else {
        (Vec::new(), Vec::new())
    };
    // Shed rows are stamped with their origin too, so the UI has ONE rule for
    // identity and labelling instead of a machine special-case. `origin` is
    // injected client-side (like `host`/`shed` already are) — the hub wire is
    // untouched, which keeps the Swift parity fixtures and shed-mobile's FRB DTOs
    // valid.
    let mut all: Vec<Value> = sessions
        .iter()
        .map(|s| {
            let mut row = serde_json::to_value(s).unwrap_or_else(|_| json!({}));
            if let Some(obj) = row.as_object_mut() {
                obj.insert("origin".into(), json!(format!("{}/{}", s.host, s.shed)));
                obj.insert("origin_kind".into(), json!("shed"));
                obj.insert("stale".into(), json!(false));
            }
            row
        })
        .collect();
    all.extend(machine_sessions);
    json!({
        "sessions": all,
        "capabilities": capabilities,
        "machines": machine_status,
    })
}

pub(crate) fn sheds_payload(r: &Reachability) -> Value {
    json!({ "sheds": r.sheds, "host_errors": r.host_errors })
}

/// A required string param, or a `bad_request` error naming the missing key — the
/// shared shape behind the shed_action/create ops' param extraction.
fn req_str<'a>(params: &'a Value, key: &str) -> Result<&'a str, (String, String)> {
    params
        .get(key)
        .and_then(Value::as_str)
        .ok_or_else(|| err("bad_request", format!("missing '{key}'")))
}

/// Reject an unknown (preserved-raw) `RcKind` on a launch/classify path. The
/// unknown-kind policy preserves such a value on READ (list/decode), but you cannot
/// LAUNCH a kind this build does not understand. Shared by the socket IPC handler
/// and the `#[tauri::command]` invoke path (`lib.rs::rc_launch`) so the two entry
/// points cannot drift.
pub(crate) fn ensure_known_kind(kind: &RcKind) -> Result<(), String> {
    if kind.is_known() {
        Ok(())
    } else {
        Err(format!("unknown rc kind '{}'", kind.as_str()))
    }
}

/// Parse the `kind` param into a KNOWN `RcKind` (its kebab-case wire value).
fn rc_kind(params: &Value) -> Result<RcKind, (String, String)> {
    let raw = params
        .get("kind")
        .and_then(Value::as_str)
        .ok_or_else(|| err("bad_request", "missing or invalid 'kind'"))?;
    let kind = RcKind::from_wire(raw);
    ensure_known_kind(&kind).map_err(|m| err("bad_request", m))?;
    Ok(kind)
}

/// Map an `RcError` to an IPC `(code, message)`. A validation error surfaces as
/// `invalid-param` — the code the shared `test_agents` suite asserts, matching the
/// mac app; every binary/transport failure is `action_failed`.
fn rc_err(e: RcError) -> (String, String) {
    match e {
        RcError::BadRequest(_) => err("invalid-param", e.to_string()),
        _ => err("action_failed", e.to_string()),
    }
}

/// Raise + focus the main window — the shared body of `ui.show_window`,
/// `app.activate`, the tray/popover "Open dashboard", and the single-instance
/// second-launch hand-off. Also the single macOS activation-policy path (a visible
/// dashboard gets a Dock icon), guarded off under the harness.
pub fn present_main_window<R: tauri::Runtime>(app: &AppHandle<R>) {
    if let Some(w) = app.get_webview_window("main") {
        let _ = w.show();
        let _ = w.unminimize();
        let _ = w.set_focus();
    }
    set_activation_policy_prod(app, true);
}

/// The Preferences window label (a dedicated webview, created lazily on first
/// open). Its snapshot is keyed under this label, read by `prefs.dump`.
pub const PREFERENCES_ID: &str = "preferences";

/// The Preferences window's fixed content size. The mac window is 460×560; this
/// carries the same grouped form with the Plex kit's roomier spacing. Fixed —
/// the window is not resizable (mac parity).
const PREFS_WIDTH: f64 = 520.0;
const PREFS_HEIGHT: f64 = 640.0;

/// Show + focus the singleton Preferences window — lazy-create on the first open,
/// front-if-already-open after (mac `AppModel.openPreferences` parity). Closing it
/// only HIDES it (the generic `CloseRequested` arm in `lib.rs` hides + prevents
/// close for every window), so a reopen is a plain show+focus — the mac
/// `isReleasedWhenClosed = false` semantics. Creation is marshalled to the main
/// thread (required on macOS — the IPC handler runs on a tokio worker; harmless on
/// Linux). Shared by the tray menu, the popover footer, the dashboard gear
/// (`open_preferences` command), and the `ui.show_preferences`/`ui.open_preferences`
/// ops — one Rust path. macOS-dev note: prefs closing while main is hidden leaves
/// the Dock icon until the next policy flip — accepted (Linux is the shipped target).
pub fn show_preferences_window(app: &AppHandle) {
    let handle = app.clone();
    // Marshal the WHOLE check-then-create-or-focus onto the main thread so it is
    // atomic with respect to the sibling `prefs.close` hide (also main-thread
    // marshalled). Main-thread closures run FIFO, so a create queued before a
    // close always runs before it — the last op queued wins the final state, and
    // two rapid opens can't race two builders (the second sees the window and
    // focuses instead of double-building).
    let _ = app.run_on_main_thread(move || {
        if let Some(w) = handle.get_webview_window(PREFERENCES_ID) {
            let _ = w.show();
            let _ = w.unminimize();
            let _ = w.set_focus();
            set_activation_policy_prod(&handle, true);
            return;
        }
        if let Err(e) = tauri::WebviewWindowBuilder::new(
            &handle,
            PREFERENCES_ID,
            tauri::WebviewUrl::App("preferences.html".into()),
        )
        .title("shed desktop — Preferences")
        .inner_size(PREFS_WIDTH, PREFS_HEIGHT)
        .resizable(false)
        .maximizable(false)
        .minimizable(false)
        .center()
        .build()
        {
            eprintln!("shed-desktop-tauri: preferences window unavailable ({e})");
            return;
        }
        set_activation_policy_prod(&handle, true);
    });
}

/// Flip the macOS activation policy in PRODUCTION only (guarded off under the
/// harness — an unguarded flip can leave `main` unmounted so `ui_report` never fires
/// and `wait_until(current_pane)` times out). `regular` shows the Dock icon (a
/// dashboard window is open); otherwise `Accessory` = menu-bar-first (no Dock icon,
/// the tray/popover is the surface). Mirrors the Swift app's `!testMode`-guarded flips.
#[cfg(target_os = "macos")]
pub fn set_activation_policy_prod<R: tauri::Runtime>(app: &AppHandle<R>, regular: bool) {
    if app.state::<Env>().test_mode {
        return;
    }
    let policy = if regular {
        tauri::ActivationPolicy::Regular
    } else {
        tauri::ActivationPolicy::Accessory
    };
    let _ = app.set_activation_policy(policy);
}
#[cfg(not(target_os = "macos"))]
pub fn set_activation_policy_prod<R: tauri::Runtime>(_app: &AppHandle<R>, _regular: bool) {}

/// The `identify` payload. A free fn (not a method) so it's unit-testable without
/// a running Tauri app / `AppHandle`.
fn identify_payload(env: &Env, pid: u32) -> Value {
    json!({
        "socket_path": env.socket_path.to_string_lossy(),
        "pid": pid,
        "core": "rust",
        "platform": "tauri",
        "test_mode": env.test_mode,
        "mock_base_url": env.mock_base_url,
    })
}

/// Services one op at a time; owned by the server and shared across connections.
pub struct Handler {
    env: Env,
    app: AppHandle,
    ui: SharedUi,
    backend: Arc<Backend>,
    /// Terminal ops (preset resolution, launch, install detection, the terminal
    /// preference), shared with the frontend invoke commands.
    terminal: SharedTerminal,
    /// The approval coordinator (the security spine): the approvals queue, policy,
    /// grants, audit, and the host-agent decision path.
    coordinator: Coordinator,
    /// The Remote-Control service (Agents pane): the session store + the process
    /// seam. Shared with the frontend invoke commands.
    rc_service: Arc<RcService>,
    /// The persisted prefs store, so `ui.set_ssh_approval` persists the chosen SSH
    /// prefs through the same path as the frontend command (both survive a restart).
    prefs: SharedPrefs,
    /// Machine targets (plan 012 R4): one hub watcher per `machines:` entry, and
    /// the sessions each reports. Reached over an SSH-forwarded hub rather than a
    /// shed server's HTTP proxy — the second reach path the sessions view merges.
    machines: Arc<crate::machines::Machines>,
    /// Monotonic token stamped onto each `sheds.refresh` so it can wait for the
    /// frontend to echo it back (a synchronous refresh — see [`Self::sheds_refresh`]).
    refresh_seq: AtomicU64,
    pid: u32,
}

impl Handler {
    #[allow(clippy::too_many_arguments)]
    pub fn new(
        env: Env,
        app: AppHandle,
        ui: SharedUi,
        backend: Arc<Backend>,
        terminal: SharedTerminal,
        coordinator: Coordinator,
        rc_service: Arc<RcService>,
        prefs: SharedPrefs,
        machines: Arc<crate::machines::Machines>,
    ) -> Self {
        Self {
            env,
            app,
            ui,
            backend,
            terminal,
            coordinator,
            rc_service,
            prefs,
            machines,
            refresh_seq: AtomicU64::new(0),
            pid: std::process::id(),
        }
    }

    /// Read + clone a key from the DASHBOARD window's reported snapshot (`pane`,
    /// `style`, `sheds`, ...), or `None` if it hasn't reported / the key is absent.
    /// The dashboard shell + Agents pane report under the `main` label; the mac
    /// popover reports under `popover` (read by `tray.dump`), so the two never mix.
    fn ui_get(&self, key: &str) -> Option<Value> {
        self.ui.lock().ok().and_then(|s| s.get("main", key))
    }

    /// The `{sheds, host_errors}` payload the DASHBOARD last committed — read from
    /// its reported snapshot, never a fresh backend query. Shared by
    /// `dashboard.dump` and `sheds.refresh` so both answer with the listing the UI
    /// is actually showing.
    fn reported_sheds_payload(&self) -> Value {
        json!({
            "sheds": self.ui_get("sheds").unwrap_or_else(|| json!([])),
            "host_errors": self.ui_get("host_errors").unwrap_or_else(|| json!([])),
        })
    }

    /// Dispatch one op. `Ok(result)` → an `ok` envelope; `Err((code, message))` →
    /// an error envelope.
    pub async fn dispatch(&self, op: &str, params: &Value) -> Result<Value, (String, String)> {
        match op {
            "identify" => {
                // Augment the base payload with the resolved broker mode (leg 3a.2) —
                // read from managed state, so `identify_payload` stays a pure, testable
                // free fn of `Env`.
                let mut v = identify_payload(&self.env, self.pid);
                if let Some(rt) = self.app.try_state::<crate::broker::BrokerRuntime>() {
                    v["broker_mode"] = rt.identify_fragment();
                }
                Ok(v)
            }
            "ui.navigate" => self.navigate(params),
            "ui.current_pane" => Ok(json!({ "pane": self.ui_get("pane") })),
            "ui.computed_style" => Ok(json!({ "style": self.ui_get("style") })),
            // Which modal (if any) the frontend has open: "create" | "launch" |
            // null. (Preferences is a dedicated window, not a modal — see prefs.dump.)
            "ui.modal" => Ok(json!({ "modal": self.ui_get("modal") })),
            // The sidebar nav badge counts the shell reported {sheds, agents, hosts,
            // pending}, or null before its first report.
            "ui.badges" => Ok(json!({ "badges": self.ui_get("badges") })),
            "ui.set_appearance" => self.set_appearance(params),
            "ui.show_window" | "app.activate" => {
                present_main_window(&self.app);
                Ok(json!({}))
            }
            // Open/focus the dedicated Preferences window (it no longer raises the
            // dashboard — mac parity). `ui.open_preferences` is the mac op name,
            // aliased so the now-shared behavior has one name across targets.
            "ui.show_preferences" | "ui.open_preferences" => {
                show_preferences_window(&self.app);
                Ok(json!({}))
            }
            "ui.show_create" => {
                present_main_window(&self.app);
                let _ = self.app.emit("show-create", json!({}));
                Ok(json!({}))
            }
            "ui.show_launch" => {
                present_main_window(&self.app);
                let _ = self.app.emit("show-launch", json!({}));
                Ok(json!({}))
            }
            "app.screenshot" => self.screenshot().await,
            "sheds.list" => Ok(sheds_payload(&self.backend.refresh().await)),
            "sheds.refresh" => self.sheds_refresh().await,
            "dashboard.dump" => {
                let reported = self.reported_sheds_payload();
                Ok(json!({
                    "rows": reported["sheds"],
                    "host_errors": reported["host_errors"],
                    "empty": self.ui_get("empty").unwrap_or(Value::Null),
                }))
            }
            "shed.start" => self.shed_action(params, "start").await,
            "shed.stop" => self.shed_action(params, "stop").await,
            "shed.reset" => self.shed_action(params, "reset").await,
            "shed.delete" => self.shed_action(params, "delete").await,
            "create.start" => self.create_start(params).await,
            "create.status" => self.create_status(params),
            "create.cancel" => self.create_cancel(params),
            "system.df" => Ok(json!({ "usage": self.backend.system_df().await })),
            "hosts.auth" => Ok(self.hosts_auth()),
            "egress.profiles" => Ok(self.egress_dump()),
            "egress.show" => self.egress_show(params),
            "terminal.preview" => self.terminal_preview(params),
            "terminal.open" => self.terminal_open(params),
            "terminal.presets" => Ok(self.terminal_presets()),
            "rc.classify" => self.rc_classify(params),
            "rc.list" => self.rc_list(params).await,
            "rc.launch" => self.rc_launch(params).await,
            "rc.kill" => self.rc_kill(params).await,
            "rc.inject_test" => self.rc_inject_test(params),
            "machines.list" => Ok(self.machines_list()),
            "machines.dump" => Ok(self.machines_dump()),
            "sidebar.dump" => Ok(self.sidebar_dump()),
            "machine.kill" => self.machine_kill(params).await,
            "machine.add" => self.machine_add(params),
            "agents.dump" => Ok(self.agents_dump()),
            "prefs.get" => Ok(self.prefs_get()),
            "prefs.set_terminal" => self.prefs_set_terminal(params),
            // Provider (AWS/Docker) approval modes — the production Preferences
            // surface (ungated, unlike test-mode `policy.set`).
            "prefs.provider_modes" => self.provider_modes().await,
            "prefs.set_provider" => self.set_provider(params).await,
            // The Preferences window's drivable surface: its merged snapshot +
            // native state, hide (close-hides contract), and the per-shed override
            // remove (the row button's path, ungated like prefs.set_provider — it
            // only removes an override, falling back to the namespace policy).
            "prefs.dump" => Ok(self.prefs_dump()),
            "prefs.close" => {
                // Marshal the hide onto the main thread so it stays FIFO-ordered
                // with the main-thread-marshalled create/focus in
                // `show_preferences_window` — otherwise a close could run before a
                // still-queued create and be a no-op, leaving the window open even
                // though close was the last op.
                let handle = self.app.clone();
                let _ = self.app.run_on_main_thread(move || {
                    if let Some(w) = handle.get_webview_window(PREFERENCES_ID) {
                        let _ = w.hide();
                    }
                });
                Ok(json!({}))
            }
            "prefs.remove_shed_rule" => self.remove_shed_rule(params).await,
            // -- approvals (the security spine) --
            "approvals.list" => self.approvals_list().await,
            "approval.decide" => self.approval_decide(params).await,
            "activity.list" => self.activity_list(params).await,
            "activity.log_path" => self.activity_log_path().await,
            "policy.set" => self.policy_set(params).await,
            "policy.list" => self.policy_list().await,
            "notifications.list" => self.notifications_list().await,
            "notification.invoke" => self.notification_invoke(params).await,
            "notification.open" => self.notification_open(),
            "ui.set_ssh_approval" => self.set_ssh_approval(params).await,
            "ui.ssh_prefs" => self.ssh_prefs().await,
            "loginitem.status" => {
                Ok(json!({ "enabled": crate::login_item_enabled(&self.app, &self.env) }))
            }
            "loginitem.set" => self.login_item_set(params),
            // -- credential broker (leg 3a.2) --
            "broker.status" => Ok(self.broker_status()),
            "broker.mode" => Ok(self.broker_mode()),
            "broker.set_mode" => self.broker_set_mode(params),
            // The mac popover's hermetic drive path — OS tray clicks aren't drivable,
            // so these run the EXACT Rust path the tray-icon left-click runs.
            "tray.show" => {
                crate::tray::show_popover(&self.app);
                Ok(json!({}))
            }
            "tray.toggle" => {
                crate::tray::toggle_popover(&self.app);
                Ok(json!({}))
            }
            "tray.hide" => {
                crate::tray::hide_popover(&self.app);
                Ok(json!({}))
            }
            "tray.dump" => Ok(self.tray_dump()),
            // The Sparkle updater's drivable surface — proves the wiring + the
            // test-mode disablement (the plugin is never registered under the
            // harness, so `updater.status` reports `test_mode`/disabled here).
            "updater.status" => Ok(crate::updater::status(&self.app, self.env.test_mode)),
            "updater.check" => self.updater_check(),
            other => Err(err("unknown_op", format!("unknown op: {other}"))),
        }
    }

    /// `tray.dump` → the drivable view of the menu-bar/tray: whether the tray
    /// installed on this host (a headless / no-SNI Linux box has nowhere to show it
    /// → `false`, window-only), its actionable menu-item ids, and — on macOS — the
    /// popover's reported rows (`popover`, from the `popover` window's snapshot, so
    /// it can't clobber the dashboard's `main`) + whether it's currently shown
    /// (`popover_visible`). `popover` is `null` where there's no popover (Linux) or
    /// it hasn't reported yet.
    fn tray_dump(&self) -> Value {
        let popover = self.ui.lock().ok().and_then(|s| s.get("popover", "tray"));
        let popover_win = self.app.get_webview_window(crate::tray::POPOVER_ID);
        let popover_visible = popover_win
            .as_ref()
            .and_then(|w| w.is_visible().ok())
            .unwrap_or(false);
        // The popover's LOGICAL inner height — the drivable proof that the content-size
        // protocol worked (`resize_popover` shrank the MAX-height window to hug its
        // content). Logical (physical / scale) so the assertion is display-independent.
        let popover_height = popover_win.as_ref().and_then(|w| {
            let scale = w.scale_factor().unwrap_or(1.0);
            w.inner_size().ok().map(|s| f64::from(s.height) / scale)
        });
        json!({
            "present": self.app.tray_by_id(crate::tray::TRAY_ID).is_some(),
            "items": crate::tray::menu_item_ids(),
            "popover": popover,
            "popover_visible": popover_visible,
            "popover_height": popover_height,
        })
    }

    /// `updater.check` → on the enabled path front the app + present Sparkle (`{}`);
    /// otherwise split by failure kind: a policy-disabled check is the deterministic
    /// `updater_disabled` error whose message carries the reason
    /// (`updater_disabled:<reason>`, so the harness can assert disablement without a
    /// crash — never registered under the harness ⇒ test mode reports
    /// `updater_disabled:test_mode`), while a runtime failure on the enabled path
    /// surfaces via this file's `action_failed` convention.
    fn updater_check(&self) -> Result<Value, (String, String)> {
        use crate::updater::CheckError;
        match crate::updater::check(&self.app, self.env.test_mode) {
            Ok(()) => Ok(json!({})),
            Err(e @ CheckError::Disabled(_)) => Err(err("updater_disabled", e.to_string())),
            Err(CheckError::Operational(msg)) => Err(err("action_failed", msg)),
        }
    }

    /// `prefs.dump` → the Preferences window's drivable state: the React-reported
    /// snapshot (`{sections, values, mode}`, keyed under the `preferences` window
    /// label) MERGED with Rust-side native window truth (`visible`, `title`) read
    /// like `tray.dump` — a native hide/close never triggers a React report, so
    /// visibility must come from Rust. `visible` is false + `prefs` null before the
    /// first open (the window is created lazily).
    fn prefs_dump(&self) -> Value {
        let snapshot = self
            .ui
            .lock()
            .ok()
            .and_then(|s| s.get(PREFERENCES_ID, "prefs"));
        let win = self.app.get_webview_window(PREFERENCES_ID);
        let visible = win
            .as_ref()
            .and_then(|w| w.is_visible().ok())
            .unwrap_or(false);
        let title = win.as_ref().and_then(|w| w.title().ok());
        json!({
            "visible": visible,
            "title": title,
            "prefs": snapshot,
        })
    }

    /// `prefs.remove_shed_rule {server, shed}` → remove one per-shed override rule +
    /// persist the remaining set (the same path as the window's remove button).
    /// `server` is matched verbatim (`""` = the single/unnamed server — F12).
    async fn remove_shed_rule(&self, params: &Value) -> Result<Value, (String, String)> {
        let server = req_str(params, "server")?.to_string();
        let shed = req_str(params, "shed")?.to_string();
        self.coordinator.remove_shed_rule(server, shed).await;
        crate::persist_shed_rules(&self.prefs, &self.coordinator).await;
        self.emit_prefs_changed();
        Ok(json!({}))
    }

    /// Emit `prefs-changed` so the Preferences window (if open) re-fetches values an
    /// IPC-driven mutation changed behind its back (its own controls keep local
    /// state directly; the dashboard has no prefs UI to notify).
    fn emit_prefs_changed(&self) {
        let _ = self.app.emit("prefs-changed", ());
    }

    /// `ui.navigate {pane}` → tell the frontend to switch panes (a `navigate`
    /// event). A0a's placeholder frontend ignores it; A0b's React wires it up. It
    /// always acks so the harness can drive navigation.
    fn navigate(&self, params: &Value) -> Result<Value, (String, String)> {
        let pane = params.get("pane").and_then(Value::as_str).unwrap_or("");
        if !matches!(
            pane,
            "sheds" | "machines" | "approvals" | "agents" | "activity" | "egress" | "system"
        ) {
            return Err(err("bad_request", format!("unknown pane: {pane:?}")));
        }
        // The frontend's `navigate` listener attaches asynchronously; the snapshot
        // (hence `pane`) is reported only once it's live, so a navigate before then
        // would be lost. Fail fast so a caller retries (the harness waits first).
        if self.ui_get("pane").is_none() {
            return Err(err(
                "frontend_not_ready",
                "frontend has not reported yet; retry",
            ));
        }
        let _ = self.app.emit("navigate", json!({ "pane": pane }));
        // Echo the pane like the mac handler, so both surfaces match ipc.md.
        Ok(json!({ "pane": pane }))
    }

    /// `ui.set_appearance {mode}` → drive the dashboard's light/dark mode (a
    /// `set-appearance` event the shell listens for), so the harness can capture
    /// dark screenshots deterministically instead of relying on the header toggle.
    /// Validates the mode; the shell's own listener ignores anything else too.
    fn set_appearance(&self, params: &Value) -> Result<Value, (String, String)> {
        let mode = params.get("mode").and_then(Value::as_str).unwrap_or("");
        if !matches!(mode, "light" | "dark") {
            return Err(err("bad_request", format!("unknown mode: {mode:?}")));
        }
        // Update the managed appearance cell DIRECTLY (in Rust) before emitting, so a
        // window reading `get_appearance_state` sees the new value even if its
        // `set-appearance` listener hasn't attached yet (kills the attach race).
        if let Some(cell) = self.app.try_state::<crate::AppearanceState>() {
            if let Ok(mut cur) = cell.0.lock() {
                *cur = Some(mode.to_string());
            }
        }
        let _ = self.app.emit("set-appearance", json!({ "mode": mode }));
        Ok(json!({}))
    }

    /// `sheds.refresh` → tell the frontend to re-fetch + re-render, then WAIT for
    /// it to echo the refresh token back before returning — so an immediately-
    /// following `dashboard.dump` reflects the new state (mac/gtk's refresh is
    /// synchronous and the harness relies on that). Best-effort at the edges: if
    /// the frontend hasn't mounted yet (cold start) just emit and return — its
    /// mount-fetch populates the dashboard — and never hang if it's slow/gone.
    ///
    /// Answers with the SAME `{sheds, host_errors}` shape as `sheds.list`, taken
    /// from the snapshot the frontend just committed — the echo this op already
    /// waited for.
    ///
    /// SINGLE-REFRESH INVARIANT: exactly ONE host listing serves both the render
    /// and this reply. Re-querying the backend here would (a) let the reply
    /// disagree with what the user is looking at, and (b) stack a second
    /// multi-host fetch on top of the frontend round-trip — worst case ~2×
    /// `REFRESH_WAIT`, past the harness's per-op timeout. With no frontend (cold
    /// start: nothing was emitted-to and nothing waited on) that one listing is a
    /// direct backend read instead, since there is no UI snapshot to be
    /// authoritative.
    async fn sheds_refresh(&self) -> Result<Value, (String, String)> {
        let token = self.refresh_seq.fetch_add(1, Ordering::SeqCst) + 1;
        // snapshot present ⟹ the frontend attached BOTH listeners then reported
        // (same readiness invariant as navigate), so the `refresh` emit is heard.
        let has_frontend = self.ui.lock().ok().is_some_and(|s| s.has("main"));
        let _ = self.app.emit("refresh", json!({ "token": token }));
        if !has_frontend {
            return Ok(sheds_payload(&self.backend.refresh().await));
        }
        let deadline = Instant::now() + REFRESH_WAIT;
        loop {
            let echoed = self.ui.lock().ok().map_or(0, |s| s.refresh_token("main"));
            if echoed >= token || Instant::now() >= deadline {
                // Timed out ⇒ the snapshot is the pre-refresh one; still the
                // truthful answer to "what is the dashboard showing".
                return Ok(self.reported_sheds_payload());
            }
            tokio::time::sleep(REFRESH_POLL).await;
        }
    }

    /// `shed.{start,stop,reset,delete}` → the lifecycle action on `{host?, name}`,
    /// dispatched by the shared [`Backend::shed_action`].
    async fn shed_action(&self, params: &Value, action: &str) -> Result<Value, (String, String)> {
        let name = req_str(params, "name")?;
        let host = params.get("host").and_then(Value::as_str);
        self.backend
            .shed_action(host, name, action)
            .await
            .map(|()| json!({}))
            .map_err(|e| err("action_failed", e.to_string()))
    }

    /// `create.start` → kick off a create on the pure shed-core CreateStore (its
    /// SSE stream runs on Tauri's tokio runtime); returns `{create_id}` to poll
    /// via `create.status`.
    async fn create_start(&self, params: &Value) -> Result<Value, (String, String)> {
        let name = req_str(params, "name")?;
        let host = params.get("host").and_then(Value::as_str);
        let s = |k: &str| params.get(k).and_then(Value::as_str).map(str::to_string);
        let req = CreateShedRequest {
            name: name.to_string(),
            repo: s("repo"),
            local_dir: s("local_dir"),
            image: s("image"),
            backend: s("backend"),
            cpus: params.get("cpus").and_then(Value::as_i64),
            memory_mb: params.get("memory_mb").and_then(Value::as_i64),
            no_provision: params.get("no_provision").and_then(Value::as_bool),
        };
        let id = self
            .backend
            .create_start(&Handle::current(), host, req)
            .map_err(|e| err("action_failed", e.to_string()))?;
        Ok(json!({ "create_id": id }))
    }

    /// `create.status` → the in-flight create's progress snapshot, or
    /// `{state: "unknown"}` once it's cancelled/gone.
    fn create_status(&self, params: &Value) -> Result<Value, (String, String)> {
        let id = req_str(params, "create_id")?;
        match self.backend.create_status(id) {
            Some(progress) => Ok(json!(progress)),
            None => Ok(json!({ "state": "unknown" })),
        }
    }

    /// `create.cancel` → abort a create's stream + drop its state (idempotent).
    fn create_cancel(&self, params: &Value) -> Result<Value, (String, String)> {
        let id = req_str(params, "create_id")?;
        self.backend.create_cancel(id);
        Ok(json!({}))
    }

    // -- terminal + prefs (delegate to the shared TerminalCtl) ------------

    /// `terminal.preview {shed, host?, session?, preset?, template?}` → the ssh
    /// command + resolved preset/invocation, WITHOUT spawning. `shed` (not `name`)
    /// matches the mac contract; gtk has no terminal.
    fn terminal_preview(&self, params: &Value) -> Result<Value, (String, String)> {
        if let Some(machine) = params.get("machine").and_then(Value::as_str) {
            return self.machine_terminal(machine, params);
        }
        let shed = req_str(params, "shed")?;
        self.terminal.preview(
            params.get("host").and_then(Value::as_str),
            shed,
            params.get("session").and_then(Value::as_str),
            params.get("preset").and_then(Value::as_str),
            params
                .get("template")
                .and_then(Value::as_str)
                .map(str::to_string),
        )
    }

    /// `terminal.open {shed, host?, session?, preset?, template?}` → spawn the
    /// resolved opener. DISABLED under test mode (spawning a terminal isn't
    /// hermetic — the harness drives terminal.preview instead).
    fn terminal_open(&self, params: &Value) -> Result<Value, (String, String)> {
        if self.env.test_mode {
            return Err(err(
                "not_enabled",
                "terminal.open is disabled in test mode (use terminal.preview)",
            ));
        }
        if let Some(machine) = params.get("machine").and_then(Value::as_str) {
            let cmd = self.machine_terminal_command(machine, params)?;
            return self.terminal.spawn_command(&cmd, machine, params);
        }
        let shed = req_str(params, "shed")?;
        self.terminal.open(
            params.get("host").and_then(Value::as_str),
            shed,
            params.get("session").and_then(Value::as_str),
            params.get("preset").and_then(Value::as_str),
            params
                .get("template")
                .and_then(Value::as_str)
                .map(str::to_string),
        )
    }

    /// The `ssh -t … tmux attach` command for a MACHINE session, resolved
    /// through the same watcher that owns the machine's config entry.
    fn machine_terminal_command(
        &self,
        machine: &str,
        params: &Value,
    ) -> Result<shed_core::terminal::TerminalCommand, (String, String)> {
        let slug = req_str(params, "slug")?;
        self.machines
            .terminal_command(machine, slug)
            .map_err(|e| err("bad_request", e))
    }

    /// `terminal.preview {machine, slug}` → the resolved command WITHOUT
    /// spawning, so the harness can assert the wire the opener would run.
    fn machine_terminal(&self, machine: &str, params: &Value) -> Result<Value, (String, String)> {
        let cmd = self.machine_terminal_command(machine, params)?;
        self.terminal
            .preview_command(&cmd, machine, params)
            .map_err(|e| (e.0, e.1))
    }

    /// `terminal.presets` → the offerable presets + install detection.
    fn terminal_presets(&self) -> Value {
        self.terminal.presets()
    }

    /// `prefs.get` → the persisted prefs (terminal preset + template).
    fn prefs_get(&self) -> Value {
        self.terminal.prefs_get()
    }

    /// `prefs.set_terminal {preset, template?}` → persist the terminal preference.
    fn prefs_set_terminal(&self, params: &Value) -> Result<Value, (String, String)> {
        let template = params
            .get("template")
            .and_then(Value::as_str)
            .map(str::to_string);
        let r = self
            .terminal
            .prefs_set_terminal(req_str(params, "preset")?, template)?;
        self.emit_prefs_changed();
        Ok(r)
    }

    // -- RC / Agents (B2.3) — the launcher + session table -------------------

    /// `rc.classify {kind, pane}` → the pure pane classifier `{state, url?}`.
    fn rc_classify(&self, params: &Value) -> Result<Value, (String, String)> {
        let kind = rc_kind(params)?;
        Ok(json!(self
            .rc_service
            .classify(&kind, req_str(params, "pane")?)))
    }

    /// `rc.list {host?, shed?}` → `{sessions}`. The running sheds + their ssh
    /// targets come from `Backend` (resolution stays in shed-app); `RcService`
    /// probes + reconciles them (a no-op filter in test mode).
    async fn rc_list(&self, params: &Value) -> Result<Value, (String, String)> {
        let host = params.get("host").and_then(Value::as_str);
        let shed = params.get("shed").and_then(Value::as_str);
        Ok(rc_list_payload(&self.backend, &self.rc_service, &self.machines, host, shed).await)
    }

    /// `machines.list` → `{machines}`: per-machine reachability for the sessions
    /// view's group headers, without the sessions themselves.
    ///
    /// Separate from `rc.list` because a machine is worth showing even when it
    /// has no sessions AND cannot be reached — that row IS the information
    /// ("mini3 is asleep"), and a sessions-only payload has nowhere to put it.
    fn machines_list(&self) -> Value {
        json!({ "machines": self.machines.status() })
    }

    /// `machine.add {name, host?, user?, ssh_port?, rc_bin?}` → append the
    /// machine to the shed config and start watching it.
    ///
    /// The same implementation the Add dialog invokes — the harness drives the
    /// op, the dialog drives the command, and both land in
    /// `machines::add_from_json`, so what is tested is what ships.
    fn machine_add(&self, params: &Value) -> Result<Value, (String, String)> {
        crate::machines::add_from_json(&self.machines, &self.env.config_path, params)
            .map(|()| json!({}))
            .map_err(|e| err("bad_request", e))
    }

    /// `machines.dump` → what the MACHINES PANE actually rendered (UI truth, like
    /// `agents.dump`/`egress.profiles`), as opposed to `machines.list`, which is
    /// the backend's view and answers off-pane.
    ///
    /// The distinction is the whole point: `machines.list` can be perfect while
    /// nothing reaches the pane — exactly the bug that shipped machine state to
    /// the IPC op but never to the window. Off-pane this is `null`, not a stale
    /// snapshot from the last mount.
    fn machines_dump(&self) -> Value {
        let on_pane = self
            .ui_get("pane")
            .and_then(|p| p.as_str().map(|s| s == "machines"))
            .unwrap_or(false);
        let machines = if on_pane {
            self.ui_get("machines_pane").unwrap_or(Value::Null)
        } else {
            Value::Null
        };
        json!({ "machines": machines })
    }

    /// `sidebar.dump` → the sidebar's status foot as rendered: `{servers,
    /// machines}`.
    ///
    /// Unlike the pane dumps this answers from ANY pane — the sidebar is always
    /// mounted, and that is exactly why it is where "is that box up" lives now
    /// that the Sheds pane carries no error strip.
    fn sidebar_dump(&self) -> Value {
        self.ui_get("sidebar").unwrap_or(Value::Null)
    }

    /// `machine.kill {machine, slug}` → kill a session on a machine.
    ///
    /// Distinct from `rc.kill` because the addressing genuinely differs: a shed
    /// session is `(host, shed, slug)` through the server's SSH endpoint, a
    /// machine session is `(machine, slug)` over the machine's own SSH. Folding
    /// them into one op would mean passing an empty `shed` and having the
    /// backend guess which path was meant.
    async fn machine_kill(&self, params: &Value) -> Result<Value, (String, String)> {
        let machine = req_str(params, "machine")?.to_string();
        let slug = req_str(params, "slug")?.to_string();
        self.machines
            .kill(&machine, &slug)
            .await
            .map_err(|e| err("action_failed", e))?;
        Ok(json!({}))
    }

    /// `rc.launch {shed, kind, host?, display_name?, workdir?, initial_prompt?}` →
    /// the launched `RcSession`. A validation error surfaces as `invalid-param`.
    async fn rc_launch(&self, params: &Value) -> Result<Value, (String, String)> {
        let shed = req_str(params, "shed")?.to_string();
        let kind = rc_kind(params)?;
        let target = self
            .backend
            .resolve_rc_target(params.get("host").and_then(Value::as_str))
            .map_err(|e| err("bad_request", e.to_string()))?;
        let opt = |k: &str| params.get(k).and_then(Value::as_str).map(str::to_string);
        let session = self
            .rc_service
            .launch(
                target,
                &shed,
                kind,
                opt("display_name"),
                opt("workdir"),
                opt("initial_prompt"),
            )
            .await
            .map_err(rc_err)?;
        Ok(json!(session))
    }

    /// `rc.kill {shed, slug, host?}` → remove the session (idempotent guest-side).
    async fn rc_kill(&self, params: &Value) -> Result<Value, (String, String)> {
        let shed = req_str(params, "shed")?;
        let slug = req_str(params, "slug")?;
        let target = self
            .backend
            .resolve_rc_target(params.get("host").and_then(Value::as_str))
            .map_err(|e| err("bad_request", e.to_string()))?;
        self.rc_service
            .kill(target, shed, slug)
            .await
            .map_err(rc_err)?;
        Ok(json!({}))
    }

    /// `rc.inject_test {…session fields…}` → inject a session directly (test-only,
    /// guarded like `policy.set`). Backs the legacy/unmanaged render fixture.
    fn rc_inject_test(&self, params: &Value) -> Result<Value, (String, String)> {
        if !self.env.test_mode {
            return Err(err("not_enabled", "rc.inject_test requires test mode"));
        }
        self.rc_service
            .inject_test(self.build_inject_session(params)?)
            .map_err(rc_err)?;
        Ok(json!({}))
    }

    /// Build the full `RcSession` an inject-test param bag describes, filling the
    /// tmux name + `<shed>/<slug>` display + workdir defaults the harness omits;
    /// a missing host resolves to the default server.
    fn build_inject_session(&self, params: &Value) -> Result<RcSession, (String, String)> {
        let shed = req_str(params, "shed")?;
        let slug = req_str(params, "slug")?;
        let host = match params.get("host").and_then(Value::as_str) {
            Some(h) => h.to_string(),
            None => {
                self.backend
                    .resolve_rc_target(None)
                    .map_err(|e| err("bad_request", e.to_string()))?
                    .server_name
            }
        };
        let opt = |k: &str| params.get(k).and_then(Value::as_str).map(str::to_string);
        let managed = params
            .get("managed")
            .and_then(Value::as_bool)
            .unwrap_or(false);
        Ok(RcSession {
            host,
            shed: shed.to_string(),
            slug: slug.to_string(),
            tmux_session: rc::tmux_name(slug),
            // A managed session with no display_name is the bare slug; a legacy one
            // is `<shed>/<slug>` — mirroring the Swift `rcInjectTestOp` branch.
            display_name: opt("display_name").unwrap_or_else(|| {
                if managed {
                    slug.to_string()
                } else {
                    format!("{shed}/{slug}")
                }
            }),
            workdir: Some(opt("workdir").unwrap_or_else(|| rc::DEFAULT_WORKDIR.to_string())),
            // kind + state both default (like the Swift `RcInjectTestParams`) — this
            // is the test-only fixture op; the harness always sends valid values.
            kind: params
                .get("kind")
                .and_then(|v| serde_json::from_value(v.clone()).ok())
                .unwrap_or(RcKind::ClaudeRc),
            state: params
                .get("state")
                .and_then(|v| serde_json::from_value(v.clone()).ok())
                .unwrap_or(RcState::Ready),
            // Every kind is tui-laned in this phase, so the fixture op stamps the
            // lane the guest would derive rather than taking it as a param.
            lane: Some(rc::LANE_TUI.to_string()),
            url: opt("url"),
            rc_id: opt("rc_id"),
            created_by: opt("created_by"),
            created_at: opt("created_at"),
            target_label: opt("target_label"),
            activity: None,
            activity_at: None,
            last_message: None,
            pending_approvals: None,
            managed,
        })
    }

    /// `agents.dump` → the RC sessions the frontend reported (UI truth, like
    /// `dashboard.dump` reads the reported sheds) so the pane is drivable by
    /// logical content, not just a screenshot.
    fn agents_dump(&self) -> Value {
        // UI truth = what's rendered: the Agents pane only reports its sessions
        // while mounted, so off-pane the `agents` snapshot is stale — report [] unless
        // the UI is actually on the agents pane (like `dashboard.dump` reflects the
        // current sheds, not a stale set).
        let on_agents = self
            .ui_get("pane")
            .and_then(|p| p.as_str().map(|s| s == "agents"))
            .unwrap_or(false);
        let sessions = if on_agents {
            self.ui_get("agents").unwrap_or_else(|| json!([]))
        } else {
            json!([])
        };
        json!({ "sessions": sessions })
    }

    // -- egress (mac parity) — the pane's UI truth + its sub-tab driver --------

    /// `egress.profiles` → the egress state the frontend reported (UI truth, like
    /// `agents.dump` reads the rendered sessions): `{tab, profiles, errors,
    /// selected, activity_count}`. The Egress pane reports only while mounted, so
    /// off-pane this returns `null` rather than a stale snapshot.
    fn egress_dump(&self) -> Value {
        let on_egress = self
            .ui_get("pane")
            .and_then(|p| p.as_str().map(|s| s == "egress"))
            .unwrap_or(false);
        let egress = if on_egress {
            self.ui_get("egress").unwrap_or(Value::Null)
        } else {
            Value::Null
        };
        json!({ "egress": egress })
    }

    /// `egress.show {tab?, profile?, host?}` → drive the Egress pane's sub-tab
    /// and/or selected profile (an `egress-show` event the pane listens for), so the
    /// harness can render the Profiles list→detail deterministically (the
    /// `set-appearance` pattern). Validates the tab; the pane resolves `profile` by
    /// (host, name) when `host` is given, else by name (first match) — so a name
    /// that collides across hosts (e.g. `default` on every server) is selectable
    /// deterministically.
    ///
    /// Readiness (mirrors `ui.navigate`'s `frontend_not_ready` gate): the pane's
    /// `egress-show` LISTENER attaches asynchronously after mount, and — like
    /// `useUiBridge` — the pane publishes its egress snapshot only AFTER that
    /// listener is live. So a non-null reported `egress` snapshot proves the
    /// listener is attached; a null/absent one means an emit would be lost to the
    /// attach race, so fail fast and let the caller retry.
    fn egress_show(&self, params: &Value) -> Result<Value, (String, String)> {
        let tab = params.get("tab").and_then(Value::as_str);
        if let Some(t) = tab {
            if !matches!(t, "activity" | "profiles") {
                return Err(err("bad_request", format!("unknown tab: {t:?}")));
            }
        }
        // snapshot non-null ⟹ the pane mounted AND its egress-show listener attached
        // (see the pane's effect ordering), so the emit below is heard. Null/absent
        // ⟹ not yet reported → fail fast so the caller retries (the harness waits).
        if self.ui_get("egress").is_none_or(|v| v.is_null()) {
            return Err(err(
                "frontend_not_ready",
                "egress pane has not reported yet; retry",
            ));
        }
        let mut payload = serde_json::Map::new();
        if let Some(t) = tab {
            payload.insert("tab".into(), json!(t));
        }
        if let Some(p) = params.get("profile").and_then(Value::as_str) {
            payload.insert("profile".into(), json!(p));
        }
        if let Some(h) = params.get("host").and_then(Value::as_str) {
            payload.insert("host".into(), json!(h));
        }
        let _ = self.app.emit("egress-show", Value::Object(payload));
        Ok(json!({}))
    }

    // -- approvals (the security spine; the harness drives the full matrix) ----

    /// `approvals.list` → the pending approval cards (each with its gate + the SSH
    /// scope/TTL defaults), soonest-to-expire first.
    async fn approvals_list(&self) -> Result<Value, (String, String)> {
        Ok(json!({ "approvals": self.coordinator.approvals_list().await }))
    }

    /// `approval.decide {id, decision, scope?, ttl?, persist?}` → run the user
    /// decision through the coordinator's two-phase gate.
    async fn approval_decide(&self, params: &Value) -> Result<Value, (String, String)> {
        #[derive(serde::Deserialize)]
        struct P {
            id: String,
            decision: ApprovalDecision,
            scope: Option<ApprovalScope>,
            ttl: Option<String>,
            #[serde(default)]
            persist: bool,
        }
        let p: P = serde_json::from_value(params.clone())
            .map_err(|e| err("bad_request", e.to_string()))?;
        let persist = p.persist;
        self.coordinator
            .decide_approval(
                p.id,
                ApprovalChoice {
                    decision: p.decision,
                    scope: p.scope,
                    ttl: p.ttl,
                    persist,
                },
            )
            .await;
        // A persisted decision adds a per-shed rule — mirror it into prefs.json (same
        // path as the frontend command) so the override survives a restart.
        if persist {
            crate::persist_shed_rules(&self.prefs, &self.coordinator).await;
            self.emit_prefs_changed();
        }
        Ok(json!({}))
    }

    /// `activity.list {limit?}` → the merged audit feed, most-recent-first.
    async fn activity_list(&self, params: &Value) -> Result<Value, (String, String)> {
        let limit = params.get("limit").and_then(Value::as_u64).unwrap_or(200) as usize;
        Ok(json!({ "entries": self.coordinator.activity_list(limit).await }))
    }

    /// `activity.log_path` → the audit JSONL path (the "reveal in files" action).
    async fn activity_log_path(&self) -> Result<Value, (String, String)> {
        Ok(json!({ "path": self.coordinator.audit_log_path().await }))
    }

    /// `policy.set {rules}` → replace the policy engine's rules. TEST-MODE ONLY —
    /// installing an auto-approve policy from a driver is a privilege (F8).
    async fn policy_set(&self, params: &Value) -> Result<Value, (String, String)> {
        if !self.env.test_mode {
            return Err(err("not_enabled", "policy.set requires test mode"));
        }
        #[derive(serde::Deserialize)]
        struct P {
            rules: Vec<PolicyRule>,
        }
        let p: P = serde_json::from_value(params.clone())
            .map_err(|e| err("bad_request", e.to_string()))?;
        self.coordinator.set_policy_rules(p.rules).await;
        Ok(json!({}))
    }

    /// `policy.list` → the current policy rules.
    async fn policy_list(&self) -> Result<Value, (String, String)> {
        Ok(json!({ "rules": self.coordinator.policy_list().await }))
    }

    /// `notifications.list` → the posted approval notifications (the test presenter
    /// records them; the prod notifier posts natively — B6 — and lists none).
    async fn notifications_list(&self) -> Result<Value, (String, String)> {
        Ok(json!({ "notifications": self.coordinator.notifications_list().await }))
    }

    /// `notification.invoke {id, action}` → drive an Approve/Deny from a posted
    /// notification (the test presenter).
    async fn notification_invoke(&self, params: &Value) -> Result<Value, (String, String)> {
        let id = req_str(params, "id")?.to_string();
        let action: ApprovalDecision =
            serde_json::from_value(params.get("action").cloned().unwrap_or(Value::Null))
                .map_err(|e| err("bad_request", format!("action: {e}")))?;
        if self
            .coordinator
            .notification_invoke(id.clone(), action)
            .await
        {
            Ok(json!({}))
        } else {
            Err(err("not_found", format!("no posted notification {id}")))
        }
    }

    /// `notification.open` → the banner-body tap: raise the window on the Approvals
    /// pane (mirrors the mac `onOpen`).
    fn notification_open(&self) -> Result<Value, (String, String)> {
        present_main_window(&self.app);
        let _ = self.app.emit("navigate", json!({ "pane": "approvals" }));
        Ok(json!({}))
    }

    /// `ui.set_ssh_approval {method?, policy?, ttl?}` → apply SSH approval prefs +
    /// re-evaluate the pending queue (the same path as the UI Preferences view).
    async fn set_ssh_approval(&self, params: &Value) -> Result<Value, (String, String)> {
        #[derive(serde::Deserialize)]
        struct P {
            method: Option<ApprovalMethod>,
            policy: Option<SshApprovalPolicy>,
            ttl: Option<String>,
        }
        let p: P = serde_json::from_value(params.clone())
            .map_err(|e| err("bad_request", e.to_string()))?;
        self.coordinator
            .set_ssh_approval(p.method, p.policy, p.ttl)
            .await;
        // Persist the resulting prefs (same path as the frontend command) so a
        // harness-driven change also survives a restart.
        let (m, pol, ttl) = crate::ssh_prefs_wire(&self.coordinator.ssh_prefs().await);
        self.prefs.set_ssh(m, pol, ttl);
        self.emit_prefs_changed();
        Ok(json!({}))
    }

    /// `ui.ssh_prefs` → the coordinator's current SSH approval prefs
    /// (`{method, policy, ttl}`) — the observe side of `ui.set_ssh_approval`, so the
    /// harness can assert what a set actually applied (the drivability North Star).
    async fn ssh_prefs(&self) -> Result<Value, (String, String)> {
        Ok(json!(self.coordinator.ssh_prefs().await))
    }

    /// `prefs.provider_modes` → the coordinator's current AWS/Docker approval modes
    /// (`{namespace: "approve"|"deny"}`) — the read side of `prefs.set_provider`.
    async fn provider_modes(&self) -> Result<Value, (String, String)> {
        Ok(json!(self.coordinator.provider_modes().await))
    }

    /// `prefs.set_provider {namespace, decision}` → set an AWS/Docker provider mode +
    /// persist (the same path as the frontend `set_provider_mode` command). The
    /// coordinator rejects a non-provider namespace (surfaced as `bad_request`).
    async fn set_provider(&self, params: &Value) -> Result<Value, (String, String)> {
        #[derive(serde::Deserialize)]
        struct P {
            namespace: String,
            decision: ApprovalDecision,
        }
        let p: P = serde_json::from_value(params.clone())
            .map_err(|e| err("bad_request", e.to_string()))?;
        self.coordinator
            .set_provider_mode(p.namespace, p.decision)
            .await
            .map_err(|e| err("bad_request", e))?;
        crate::persist_provider_modes(&self.prefs, &self.coordinator).await;
        self.emit_prefs_changed();
        Ok(json!({}))
    }

    /// `loginitem.set {enabled}` → enable/disable launch-at-login (the Preferences
    /// "General" toggle's driver). Guarded to an in-memory cell under the macOS
    /// harness; a real hermetic `auto-launch` write on Linux/production.
    fn login_item_set(&self, params: &Value) -> Result<Value, (String, String)> {
        let enabled = params
            .get("enabled")
            .and_then(Value::as_bool)
            .ok_or_else(|| err("bad_request", "missing 'enabled' (bool)"))?;
        crate::login_item_set(&self.app, &self.env, enabled)
            .map_err(|e| err("action_failed", e))?;
        self.emit_prefs_changed();
        Ok(json!({}))
    }

    /// `hosts.auth` → per-server credential mode: what the config entry claims
    /// and what this SESSION has learned (plan 002 §7 P1).
    ///
    /// The app's whole mtls surface, and deliberately a read-only one. Nothing
    /// here is persisted — `~/.shed/config.yaml` is CLI-owned, this parser is
    /// read-only and lossy, and a learned mode costs one silent re-bootstrap on
    /// the next cold launch. `learned` is what distinguishes "the CLI cached
    /// this" from "a mint proved it", which is what a driver needs to assert a
    /// live token→mtls flip.
    fn hosts_auth(&self) -> Value {
        let hosts = self
            .app
            .try_state::<Arc<shed_app::AuthModeRegistry>>()
            .map(|r| {
                r.snapshot()
                    .into_iter()
                    .map(|(name, s)| {
                        json!({
                            "name": name,
                            "auth_mode": s.mode.as_str(),
                            "learned": s.learned,
                        })
                    })
                    .collect::<Vec<_>>()
            })
            .unwrap_or_default();
        json!({ "hosts": hosts })
    }

    /// `broker.status` → the resolved broker mode + probe evidence + config source +
    /// (embedded) resolved ssh mode / gate namespaces / per-server LiveStatus health.
    /// The harness drives + asserts it (C4); shedtest reads it as broker truth.
    fn broker_status(&self) -> Value {
        self.app
            .try_state::<crate::broker::BrokerRuntime>()
            .map(|rt| rt.status())
            .unwrap_or_else(|| json!({}))
    }

    /// `broker.mode` → the effective mode + the LIVE persisted pref + `restart_required`
    /// (the persisted pref drifted from the launch pref ⟹ a relaunch applies it).
    fn broker_mode(&self) -> Value {
        self.app
            .try_state::<crate::broker::BrokerRuntime>()
            .map(|rt| {
                json!({
                    "pref": rt.pref_str(),
                    "effective": rt.effective_mode_str(),
                    "restart_required": rt.restart_required(),
                })
            })
            .unwrap_or_else(|| json!({}))
    }

    /// `broker.set_mode {mode}` → persist the broker mode pref (the same path as the
    /// `broker_set_mode` frontend command). Guarded: an unknown mode is `bad_request`.
    /// The change is NOT hot-swapped — the CURRENT effective mode + the computed
    /// `restart_required` (persisted pref != launch pref) are returned so a driver can
    /// assert the deferred-apply contract.
    fn broker_set_mode(&self, params: &Value) -> Result<Value, (String, String)> {
        let mode = req_str(params, "mode")?;
        if crate::broker::parse_mode_pref(mode).is_none() {
            return Err(err("bad_request", format!("unknown broker mode: {mode:?}")));
        }
        self.prefs.set_broker_mode(mode.to_string());
        let (effective, restart_required) = self
            .app
            .try_state::<crate::broker::BrokerRuntime>()
            .map(|rt| (rt.effective_mode_str().to_string(), rt.restart_required()))
            .unwrap_or_default();
        self.emit_prefs_changed();
        Ok(json!({ "pref": mode, "effective": effective, "restart_required": restart_required }))
    }

    /// `app.screenshot` → shell out to a platform tool and return `{png (base64),
    /// width, height}`. The capture is blocking, so run it off the async worker.
    async fn screenshot(&self) -> Result<Value, (String, String)> {
        let res = tokio::task::spawn_blocking(crate::screenshot::capture)
            .await
            .map_err(|e| err("screenshot_failed", format!("capture task panicked: {e}")))?;
        match res {
            Ok((png, width, height)) => Ok(json!({
                "png": base64::engine::general_purpose::STANDARD.encode(&png),
                "width": width,
                "height": height,
            })),
            Err(e) => Err(err("screenshot_failed", e)),
        }
    }
}

/// A bound IPC server. Bind (in the runtime context) before the window paints so
/// an `identify` right after launch succeeds; then `run()` on the runtime.
pub struct IpcServer {
    listener: UnixListener,
    handler: Arc<Handler>,
}

impl IpcServer {
    pub async fn bind(socket_path: &Path, handler: Handler) -> std::io::Result<Self> {
        if let Some(dir) = socket_path.parent() {
            std::fs::create_dir_all(dir)?;
        }
        // Remove a stale socket so a relaunch can bind. Single-instance (the
        // tauri-plugin) runs first, so a *live* instance never reaches here.
        let _ = std::fs::remove_file(socket_path);
        let listener = UnixListener::bind(socket_path)?;
        // Lock the socket to the owner — it exposes app control + screenshots.
        // Best-effort: $XDG_RUNTIME_DIR is already 0700; the /tmp fallback isn't.
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            let _ = std::fs::set_permissions(socket_path, std::fs::Permissions::from_mode(0o600));
            if let Some(dir) = socket_path.parent() {
                let _ = std::fs::set_permissions(dir, std::fs::Permissions::from_mode(0o700));
            }
        }
        Ok(Self {
            listener,
            handler: Arc::new(handler),
        })
    }

    pub async fn run(self) {
        loop {
            match self.listener.accept().await {
                Ok((stream, _)) => {
                    let handler = self.handler.clone();
                    tokio::spawn(async move { serve_conn(stream, handler).await });
                }
                Err(e) => {
                    eprintln!("shed-desktop-tauri: ipc accept error: {e}");
                    break;
                }
            }
        }
    }
}

async fn serve_conn(mut stream: UnixStream, handler: Arc<Handler>) {
    // Borrowed split (not `into_split`): both halves stay in this task, so we
    // avoid the Arc that owned-halves would allocate per connection.
    let (rd, mut wr) = stream.split();
    let mut reader = BufReader::new(rd);
    loop {
        let line = match read_capped_line(&mut reader, MAX_FRAME_BYTES).await {
            Ok(Some(line)) => line,
            Ok(None) => break, // clean EOF
            Err(_) => {
                let _ = write_line(
                    &mut wr,
                    &json!({"ok": false, "error": {"code": "frame_too_large", "message": "request exceeds 1 MiB"}}),
                )
                .await;
                break;
            }
        };
        let trimmed = line.trim();
        if trimmed.is_empty() {
            continue;
        }
        let resp = handle_line(trimmed, &handler).await;
        if write_line(&mut wr, &resp).await.is_err() {
            break;
        }
    }
}

/// Read one newline-terminated frame, capping its length so an unterminated/huge
/// line can't force unbounded buffering. Generic over the reader so it's unit-
/// testable on an in-memory slice. Returns `None` at a clean EOF.
async fn read_capped_line<R: AsyncReadExt + Unpin>(
    reader: &mut R,
    max: usize,
) -> std::io::Result<Option<String>> {
    let mut buf: Vec<u8> = Vec::new();
    loop {
        match reader.read_u8().await {
            Ok(b'\n') => return Ok(Some(String::from_utf8_lossy(&buf).into_owned())),
            Ok(byte) => {
                if buf.len() >= max {
                    return Err(std::io::Error::new(
                        std::io::ErrorKind::InvalidData,
                        "frame too large",
                    ));
                }
                buf.push(byte);
            }
            Err(e) if e.kind() == std::io::ErrorKind::UnexpectedEof => {
                return Ok((!buf.is_empty()).then(|| String::from_utf8_lossy(&buf).into_owned()));
            }
            Err(e) => return Err(e),
        }
    }
}

async fn write_line(wr: &mut (impl AsyncWriteExt + Unpin), resp: &Value) -> std::io::Result<()> {
    let mut bytes = serde_json::to_vec(resp).unwrap_or_default();
    bytes.push(b'\n');
    wr.write_all(&bytes).await
}

async fn handle_line(line: &str, handler: &Handler) -> Value {
    let req: Value = match serde_json::from_str(line) {
        Ok(v) => v,
        Err(e) => {
            return json!({"ok": false, "error": {"code": "bad_request", "message": e.to_string()}});
        }
    };
    let id = req.get("id").cloned().unwrap_or(Value::Null);
    let op = req.get("op").and_then(Value::as_str).unwrap_or("");
    let params = req.get("params").cloned().unwrap_or_else(|| json!({}));
    match handler.dispatch(op, &params).await {
        Ok(result) => json!({"id": id, "ok": true, "result": result}),
        Err((code, message)) => {
            json!({"id": id, "ok": false, "error": {"code": code, "message": message}})
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use shed_app::HostFailure;
    use shed_core::http::ShedError;
    use std::path::PathBuf;

    /// A `Reachability` as `Backend::refresh()` would return it, without needing a
    /// live backend: `sheds` decoded from the wire shape, `host_errors` built
    /// through the SAME `HostFailure::from_error` mapping the backend uses.
    fn reachability(shed_names: &[(&str, &str)], failures: &[(&str, ShedError)]) -> Reachability {
        let sheds = shed_names
            .iter()
            .map(|(host, name)| {
                serde_json::from_value(json!({"host": host, "name": name, "status": "running"}))
                    .expect("shed fixture decodes")
            })
            .collect();
        let host_errors: Vec<HostFailure> = failures
            .iter()
            .map(|(host, e)| HostFailure::from_error(host, e))
            .collect();
        let last_error = match host_errors.as_slice() {
            [] => None,
            [only] => Some(only.summary.clone()),
            many => Some(format!("{} hosts unreachable", many.len())),
        };
        Reachability {
            sheds,
            last_error,
            host_errors,
        }
    }

    #[test]
    fn sheds_payload_carries_host_errors_for_two_failed_hosts() {
        // The shape shed#300 needs: one host that needs a host-agent upgrade and
        // one generic failure, BOTH present — the singular shape couldn't
        // represent two failed hosts (plan 006 D6).
        let r = reachability(
            &[("mock", "alpha")],
            &[
                (
                    "mini2",
                    ShedError::AgentUpgradeRequired {
                        server: "mini2".into(),
                        detail: "the installed shed-host-agent has no credential.get".into(),
                    },
                ),
                ("mini3", ShedError::BadStatus(500)),
            ],
        );
        let p = sheds_payload(&r);

        // The pre-existing `sheds` array is untouched (additive contract).
        let sheds = p["sheds"].as_array().expect("sheds is an array");
        assert_eq!(sheds.len(), 1);
        assert_eq!(sheds[0]["name"], "alpha");
        assert_eq!(sheds[0]["host"], "mock");

        let errs = p["host_errors"]
            .as_array()
            .expect("host_errors is an array");
        assert_eq!(errs.len(), 2, "both failed hosts survive: {p}");
        assert_eq!(errs[0]["server"], "mini2");
        assert_eq!(errs[0]["kind"], "agent_upgrade_required");
        assert!(
            errs[0]["summary"]
                .as_str()
                .unwrap()
                .starts_with("Upgrade shed-host-agent"),
            "the summary must lead with the remedy: {}",
            errs[0]["summary"]
        );
        assert!(errs[0]["detail"]
            .as_str()
            .unwrap()
            .contains("credential.get"));
        assert_eq!(errs[1]["server"], "mini3");
        assert_eq!(errs[1]["kind"], "other");
        assert!(!errs[1]["summary"].as_str().unwrap().is_empty());
    }

    #[test]
    fn sheds_payload_reports_no_host_errors_when_healthy() {
        // Healthy hosts ⇒ `host_errors` is present-and-empty, never absent, so a
        // consumer (the WebView strip, shedctl) can read it unconditionally.
        let p = sheds_payload(&reachability(&[("mock", "alpha"), ("mock", "beta")], &[]));
        assert_eq!(p["sheds"].as_array().unwrap().len(), 2);
        assert_eq!(p["host_errors"], json!([]));
    }

    #[test]
    fn rc_kind_parses_wire_value_or_rejects() {
        assert_eq!(
            rc_kind(&json!({"kind": "claude-rc"})).unwrap(),
            RcKind::ClaudeRc
        );
        assert_eq!(rc_kind(&json!({"kind": "shell"})).unwrap(), RcKind::Shell);
        assert_eq!(
            rc_kind(&json!({"kind": "bogus"})).unwrap_err().0,
            "bad_request"
        );
        assert_eq!(rc_kind(&json!({})).unwrap_err().0, "bad_request");
    }

    #[test]
    fn ensure_known_kind_gates_unknown() {
        // The shared gate both entry points (socket IPC rc_kind + the tauri
        // rc_launch command) apply: serde preserves an unknown kind as Other, so
        // launching must reject it here.
        assert!(ensure_known_kind(&RcKind::Codex).is_ok());
        assert!(ensure_known_kind(&RcKind::Other("borg".into())).is_err());
    }

    #[test]
    fn rc_err_maps_bad_request_to_invalid_param() {
        // gotcha #7: the shared test_agents suite asserts `invalid-param` for a
        // prompt-validation failure (matching the mac app's code); other RcErrors
        // surface as action_failed.
        assert_eq!(rc_err(RcError::BadRequest("x".into())).0, "invalid-param");
        assert_eq!(rc_err(RcError::SlugTaken("x".into())).0, "action_failed");
        assert_eq!(rc_err(RcError::MissingBinary).0, "action_failed");
    }

    fn env(mock: Option<&str>) -> Env {
        Env {
            test_mode: true,
            mock_base_url: mock.map(str::to_string),
            mock_unreachable_hosts: std::collections::HashSet::new(),
            machine_hub_ports: std::collections::HashMap::new(),
            config_path: PathBuf::new(),
            socket_path: PathBuf::from("/run/user/0/shed-tauri/shed-tauri.sock"),
            host_agent_socket: PathBuf::from("/run/user/0/shed/host-agent.sock"),
            broker_extensions_path: PathBuf::from("/run/user/0/shed/extensions.yaml"),
        }
    }

    #[test]
    fn identify_reports_tauri_core_and_hermeticity() {
        let v = identify_payload(&env(Some("http://mock")), 4242);
        assert_eq!(v["platform"], "tauri");
        assert_eq!(v["core"], "rust");
        assert_eq!(v["test_mode"], true);
        assert_eq!(v["mock_base_url"], "http://mock");
        assert_eq!(v["pid"], 4242);
        assert!(v["socket_path"]
            .as_str()
            .unwrap()
            .ends_with("shed-tauri.sock"));
    }

    #[test]
    fn identify_null_mock_when_unset() {
        let v = identify_payload(&env(None), 1);
        assert!(v["mock_base_url"].is_null());
    }

    #[tokio::test]
    async fn read_line_returns_trimmed_frame_then_eof() {
        let mut data: &[u8] = b"{\"op\":\"identify\"}\n";
        let line = read_capped_line(&mut data, MAX_FRAME_BYTES)
            .await
            .unwrap()
            .unwrap();
        assert_eq!(line, "{\"op\":\"identify\"}");
        // Next read hits a clean EOF.
        assert!(read_capped_line(&mut data, MAX_FRAME_BYTES)
            .await
            .unwrap()
            .is_none());
    }

    #[tokio::test]
    async fn read_line_caps_oversized_frame() {
        let mut data: &[u8] = b"aaaaaaaaaa"; // 10 bytes, no newline
        let e = read_capped_line(&mut data, 4).await.unwrap_err();
        assert_eq!(e.kind(), std::io::ErrorKind::InvalidData);
    }

    #[tokio::test]
    async fn read_line_trailing_unterminated_is_returned_once() {
        // A final line with no newline is returned, then EOF.
        let mut data: &[u8] = b"tail-no-newline";
        assert_eq!(
            read_capped_line(&mut data, MAX_FRAME_BYTES)
                .await
                .unwrap()
                .as_deref(),
            Some("tail-no-newline")
        );
        assert!(read_capped_line(&mut data, MAX_FRAME_BYTES)
            .await
            .unwrap()
            .is_none());
    }
}
