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
//! `dashboard.dump` reads the sheds the frontend reported (UI truth, not a
//! backend re-query), which is why `sheds.refresh` round-trips through the
//! frontend before returning.

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

use shed_app::{Backend, Coordinator, RcService};
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

/// A required string param, or a `bad_request` error naming the missing key — the
/// shared shape behind the shed_action/create ops' param extraction.
fn req_str<'a>(params: &'a Value, key: &str) -> Result<&'a str, (String, String)> {
    params
        .get(key)
        .and_then(Value::as_str)
        .ok_or_else(|| err("bad_request", format!("missing '{key}'")))
}

/// Parse the `kind` param into an `RcKind` (its kebab-case wire value).
fn rc_kind(params: &Value) -> Result<RcKind, (String, String)> {
    params
        .get("kind")
        .and_then(|v| serde_json::from_value::<RcKind>(v.clone()).ok())
        .ok_or_else(|| err("bad_request", "missing or invalid 'kind'"))
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

    /// Dispatch one op. `Ok(result)` → an `ok` envelope; `Err((code, message))` →
    /// an error envelope.
    pub async fn dispatch(&self, op: &str, params: &Value) -> Result<Value, (String, String)> {
        match op {
            "identify" => Ok(identify_payload(&self.env, self.pid)),
            "ui.navigate" => self.navigate(params),
            "ui.current_pane" => Ok(json!({ "pane": self.ui_get("pane") })),
            "ui.computed_style" => Ok(json!({ "style": self.ui_get("style") })),
            // Which modal (if any) the frontend has open: "prefs" | "create" | null.
            "ui.modal" => Ok(json!({ "modal": self.ui_get("modal") })),
            "ui.show_window" | "app.activate" => {
                present_main_window(&self.app);
                Ok(json!({}))
            }
            "ui.show_preferences" => {
                present_main_window(&self.app);
                let _ = self.app.emit("show-preferences", json!({}));
                Ok(json!({}))
            }
            "ui.show_create" => {
                present_main_window(&self.app);
                let _ = self.app.emit("show-create", json!({}));
                Ok(json!({}))
            }
            "app.screenshot" => self.screenshot().await,
            "sheds.list" => Ok(json!({ "sheds": self.backend.list_sheds().await })),
            "sheds.refresh" => self.sheds_refresh().await,
            "dashboard.dump" => {
                Ok(json!({ "rows": self.ui_get("sheds").unwrap_or_else(|| json!([])) }))
            }
            "shed.start" => self.shed_action(params, "start").await,
            "shed.stop" => self.shed_action(params, "stop").await,
            "shed.reset" => self.shed_action(params, "reset").await,
            "shed.delete" => self.shed_action(params, "delete").await,
            "create.start" => self.create_start(params).await,
            "create.status" => self.create_status(params),
            "create.cancel" => self.create_cancel(params),
            "system.df" => Ok(json!({ "usage": self.backend.system_df().await })),
            "terminal.preview" => self.terminal_preview(params),
            "terminal.open" => self.terminal_open(params),
            "terminal.presets" => Ok(self.terminal_presets()),
            "rc.classify" => self.rc_classify(params),
            "rc.list" => self.rc_list(params).await,
            "rc.launch" => self.rc_launch(params).await,
            "rc.kill" => self.rc_kill(params).await,
            "rc.inject_test" => self.rc_inject_test(params),
            "agents.dump" => Ok(self.agents_dump()),
            "prefs.get" => Ok(self.prefs_get()),
            "prefs.set_terminal" => self.prefs_set_terminal(params),
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

    /// `ui.navigate {pane}` → tell the frontend to switch panes (a `navigate`
    /// event). A0a's placeholder frontend ignores it; A0b's React wires it up. It
    /// always acks so the harness can drive navigation.
    fn navigate(&self, params: &Value) -> Result<Value, (String, String)> {
        let pane = params.get("pane").and_then(Value::as_str).unwrap_or("");
        if !matches!(
            pane,
            "sheds" | "approvals" | "agents" | "activity" | "system"
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
        Ok(json!({}))
    }

    /// `sheds.refresh` → tell the frontend to re-fetch + re-render, then WAIT for
    /// it to echo the refresh token back before returning — so an immediately-
    /// following `dashboard.dump` reflects the new state (mac/gtk's refresh is
    /// synchronous and the harness relies on that). Best-effort at the edges: if
    /// the frontend hasn't mounted yet (cold start) just emit and return — its
    /// mount-fetch populates the dashboard — and never hang if it's slow/gone.
    async fn sheds_refresh(&self) -> Result<Value, (String, String)> {
        let token = self.refresh_seq.fetch_add(1, Ordering::SeqCst) + 1;
        // snapshot present ⟹ the frontend attached BOTH listeners then reported
        // (same readiness invariant as navigate), so the `refresh` emit is heard.
        let has_frontend = self.ui.lock().ok().is_some_and(|s| s.has("main"));
        let _ = self.app.emit("refresh", json!({ "token": token }));
        if !has_frontend {
            return Ok(json!({}));
        }
        let deadline = Instant::now() + REFRESH_WAIT;
        loop {
            let echoed = self.ui.lock().ok().map_or(0, |s| s.refresh_token("main"));
            if echoed >= token || Instant::now() >= deadline {
                return Ok(json!({}));
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
        self.terminal
            .prefs_set_terminal(req_str(params, "preset")?, template)
    }

    // -- RC / Agents (B2.3) — the launcher + session table -------------------

    /// `rc.classify {kind, pane}` → the pure pane classifier `{state, url?}`.
    fn rc_classify(&self, params: &Value) -> Result<Value, (String, String)> {
        let kind = rc_kind(params)?;
        Ok(json!(self.rc_service.classify(kind, req_str(params, "pane")?)))
    }

    /// `rc.list {host?, shed?}` → `{sessions}`. The running sheds + their ssh
    /// targets come from `Backend` (resolution stays in shed-app); `RcService`
    /// probes + reconciles them (a no-op filter in test mode).
    async fn rc_list(&self, params: &Value) -> Result<Value, (String, String)> {
        let host = params.get("host").and_then(Value::as_str);
        let shed = params.get("shed").and_then(Value::as_str);
        let targets = self.backend.rc_targets(host, shed).await;
        Ok(json!({ "sessions": self.rc_service.list(targets, host, shed).await }))
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
        self.rc_service.kill(target, shed, slug).await.map_err(rc_err)?;
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
        let managed = params.get("managed").and_then(Value::as_bool).unwrap_or(false);
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
            workdir: opt("workdir").unwrap_or_else(|| rc::DEFAULT_WORKDIR.to_string()),
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
            url: opt("url"),
            rc_id: opt("rc_id"),
            created_by: opt("created_by"),
            created_at: opt("created_at"),
            target_label: opt("target_label"),
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
        self.coordinator
            .decide_approval(
                p.id,
                ApprovalChoice {
                    decision: p.decision,
                    scope: p.scope,
                    ttl: p.ttl,
                    persist: p.persist,
                },
            )
            .await;
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
        Ok(json!({}))
    }

    /// `ui.ssh_prefs` → the coordinator's current SSH approval prefs
    /// (`{method, policy, ttl}`) — the observe side of `ui.set_ssh_approval`, so the
    /// harness can assert what a set actually applied (the drivability North Star).
    async fn ssh_prefs(&self) -> Result<Value, (String, String)> {
        Ok(json!(self.coordinator.ssh_prefs().await))
    }

    /// `loginitem.set {enabled}` → enable/disable launch-at-login (the Preferences
    /// "General" toggle's driver). Guarded to an in-memory cell under the macOS
    /// harness; a real hermetic `auto-launch` write on Linux/production.
    fn login_item_set(&self, params: &Value) -> Result<Value, (String, String)> {
        let enabled = params
            .get("enabled")
            .and_then(Value::as_bool)
            .ok_or_else(|| err("bad_request", "missing 'enabled' (bool)"))?;
        crate::login_item_set(&self.app, &self.env, enabled).map_err(|e| err("action_failed", e))?;
        Ok(json!({}))
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
    use std::path::PathBuf;

    #[test]
    fn rc_kind_parses_wire_value_or_rejects() {
        assert_eq!(rc_kind(&json!({"kind": "claude-rc"})).unwrap(), RcKind::ClaudeRc);
        assert_eq!(rc_kind(&json!({"kind": "shell"})).unwrap(), RcKind::Shell);
        assert_eq!(rc_kind(&json!({"kind": "bogus"})).unwrap_err().0, "bad_request");
        assert_eq!(rc_kind(&json!({})).unwrap_err().0, "bad_request");
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
            config_path: PathBuf::new(),
            socket_path: PathBuf::from("/run/user/0/shed-tauri/shed-tauri.sock"),
            host_agent_socket: PathBuf::from("/run/user/0/shed/host-agent.sock"),
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
