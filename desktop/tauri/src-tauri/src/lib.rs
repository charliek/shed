//! shed-desktop (Tauri) — the desktop client, on the shared shed-core. Runs on
//! Linux (WebKitGTK) and macOS (WKWebView).
//!
//! Thin entry: resolve the `SHED_TAURI_*` env, take the single-instance flock,
//! and in `setup` bind the JSON IPC server (the drivability North Star) on Tauri's
//! async runtime before the window paints — so a harness `identify` right after
//! launch succeeds.

// First, so its `app_info!`/`app_warn!`/`app_error!` macros are in scope in every
// module below (#414: the app's lines go to stderr AND its log file).
#[macro_use]
mod applog;
mod delivery;
mod egress;
mod env;
mod ipc;
mod prefs;
mod screenshot;
mod shutdown;
mod single_instance;
mod source;
mod state;
mod termctl;
mod tray;
mod updater;

#[cfg(test)]
mod slash_command_tests;

use std::sync::{Arc, Mutex};

use env::Env;
use ipc::{Handler, IpcServer};
use shed_app::Backend;
use shed_core::models::CreateShedRequest;
use shed_desktop_core::{
    failure_json, installed_json, HostEvents, LaneEvents, LaneFailure, LaneUiEvent,
    ShedDesktopRuntime, ShedId,
};
use single_instance::AcquireError;
use state::{SharedUi, UiState};
use tauri::{Emitter, Manager};

/// Pushes a host-snapshot invalidation. The frontend re-reads `rc.list`.
struct TauriHostEvents {
    app: tauri::AppHandle,
}

impl HostEvents for TauriHostEvents {
    fn on_hosts_changed(&self) {
        let _ = self.app.emit("refresh", serde_json::json!({}));
    }
}

/// Pushes one lane frame as `{ target, session_id, event }`.
struct TauriLaneEvents {
    app: tauri::AppHandle,
}

impl LaneEvents for TauriLaneEvents {
    fn on_lane_event(&self, event: LaneUiEvent) {
        let _ = self.app.emit("lane-event", event);
    }
}

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
    // Mirror the running-shed count onto the menu-bar status item.
    // The dashboard (`main`) reports the full shed list here even while hidden at
    // launch, so the tray count is live without a Rust-side poller. macOS-only;
    // computed before the snapshot is moved into `merge`. Guarded on the `sheds` key
    // being PRESENT (defense-in-depth): the shell only sends full snapshots now, but
    // a report that omitted `sheds` must NOT be read as "zero running" and blank the
    // count — skip it and keep the last known count instead.
    #[cfg(target_os = "macos")]
    if window.label() == "main" {
        if let Some(sheds) = snapshot.get("sheds").and_then(|v| v.as_array()) {
            let running = sheds
                .iter()
                .filter(|s| s.get("status").and_then(|v| v.as_str()) == Some("running"))
                .count();
            crate::tray::update_running_count(window.app_handle(), running);
        }
    }
    if let Ok(mut s) = ui.lock() {
        s.merge(window.label(), snapshot);
    }
}

/// The WebView's live shed list — `invoke("list_sheds")` on mount + on each
/// `refresh` event. Returns `{sheds, host_errors}`: host-stamped sheds (all
/// configured hosts, concurrently via the shared `Backend`) PLUS the per-host
/// failures, so a host that can't be listed is surfaced instead of silently
/// dropped (plan 006 D6 / shed#300). Same payload the harness reads via the
/// `sheds.list` IPC op — [`ipc::sheds_payload`] is the single shaper.
#[tauri::command]
async fn list_sheds(
    app: tauri::AppHandle,
    backend: tauri::State<'_, Arc<Backend>>,
    runtime: tauri::State<'_, Arc<ShedDesktopRuntime>>,
) -> Result<serde_json::Value, String> {
    // The frontend's own refresh is an authoritative shed listing too — it is
    // the one the dashboard runs on a timer, so a shed that stops while nobody
    // is driving the socket still loses its roost watcher (plan 019 §3.6).
    // The sequence is stamped inside [`ipc::refresh_hosts`], before the fetch,
    // so a slow answer cannot reauthorize a shed a newer refresh removed.
    let reachability = ipc::refresh_hosts(&backend, runtime.hosts()).await;
    if let Ok(feed) = egress_feed(&app) {
        feed.observe(&reachability);
    }
    Ok(ipc::sheds_payload(&reachability))
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

/// Move a cloud shed's lease `hours` later (plan 041) from its row's Extend: the
/// shed as its server answered it and its row (`{shed, cloud_row}`), or the
/// refusal (`{code, reason, status, words}`; hours outside 1 to 720 are
/// `bad_request`, refused by [`Backend::extend`] before anything is sent, the same
/// check as the IPC op's). As for the other actions, the
/// frontend re-fetches after it resolves; the harness drives the same through
/// the `shed.extend` IPC op.
#[tauri::command]
async fn extend_shed(
    backend: tauri::State<'_, Arc<Backend>>,
    name: String,
    hours: i64,
    host: Option<String>,
) -> Result<serde_json::Value, serde_json::Value> {
    backend
        .extend(host.as_deref(), &name, hours)
        .await
        .map(|shed| ipc::extend_answer(&shed))
        .map_err(|e| ipc::cloud_failure(&e))
}

/// A remote server's cloud usage for the line under its heading (plan 041):
/// `{host, usage, words}`, or the refusal (`{code, reason, status, words}`; a
/// server without compute answers `FEATURE_UNSUPPORTED`). The harness reads the
/// same through the `cloud.usage` IPC op.
#[tauri::command]
async fn cloud_usage(
    backend: tauri::State<'_, Arc<Backend>>,
    host: String,
) -> Result<serde_json::Value, serde_json::Value> {
    backend
        .cloud_usage(&host)
        .await
        .map(|u| ipc::usage_answer(&host, &u))
        .map_err(|e| ipc::cloud_failure(&e))
}

/// The WebView's live per-host disk usage — `invoke("system_df")` when the System
/// pane mounts / on its Refresh. Each row is a host's `SystemDiskUsage` or the
/// error it returned (unreachable hosts are kept, not dropped). The harness reads
/// the same via the `system.df` IPC op.
#[tauri::command]
async fn system_df(backend: tauri::State<'_, Arc<Backend>>) -> Result<serde_json::Value, String> {
    Ok(serde_json::json!(backend.system_df().await))
}

/// The WebView's per-host egress profiles — `invoke("egress_profiles")` when the
/// Egress pane mounts / on its Refresh. Each row is a host's profiles or the
/// error it returned (unreachable / egress-disabled hosts are kept as error rows,
/// not dropped) — the same fan-out shape as `system_df`. The harness reads the
/// RENDERED rows via the `egress.profiles` IPC op instead (UI truth).
#[tauri::command]
async fn egress_profiles(
    backend: tauri::State<'_, Arc<Backend>>,
) -> Result<serde_json::Value, String> {
    Ok(serde_json::json!(backend.egress_profiles().await))
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

// -- agents (the Agents pane, its lanes and craze) -----------------------------

/// The Agents pane's session list. The same shaper the socket IPC `rc.list`
/// uses: `{ sessions, capabilities, hosts, shed_craze }`.
#[tauri::command]
async fn rc_list(
    backend: tauri::State<'_, Arc<Backend>>,
    runtime: tauri::State<'_, Arc<ShedDesktopRuntime>>,
    host: Option<String>,
    shed: Option<String>,
) -> Result<serde_json::Value, String> {
    Ok(shed_desktop_core::rc_list_payload(
        &backend,
        runtime.hosts(),
        host.as_deref(),
        shed.as_deref(),
    )
    .await)
}

/// A shed target (`roost:<server>/<shed>`), or `bad_request` when it is not one.
fn shed_id(target: &str) -> Result<ShedId, String> {
    ShedId::parse(target).map_err(|message| format!("bad_request: {message}"))
}

// -- roost bootstrap (plan 019 §3.6, C8) -----------------------------------
//
// The frontend's own door onto `ShedHosts::probe/preview/bootstrap`: the
// harness drives these over the `roost.*` socket ops (`ipc.rs`), the card and
// consent dialog drive these, and both land in the same methods, so what the
// harness proves about a bootstrap is what a click actually runs. The target
// is a server-listed shed (`roost:<server>/<shed>`).
//
// A failure crosses as `"{stage}: {message}"`, the same shape `lane_error`
// uses for the same reason: a Tauri command's error channel is a bare string,
// and `bridge.ts` splits it back into a stage/code and a message so the UI can
// tell "the host changed under you" (`fingerprint`) from everything else.

fn roost_failure_string(stage: &str, message: String) -> String {
    format!("{stage}: {message}")
}

/// `roost_probe` — one read-only look at `target`: what's on it, and the plan
/// matrix row it lands on. Safe to call before consent; writes nothing.
#[tauri::command]
async fn roost_probe(
    runtime: tauri::State<'_, Arc<ShedDesktopRuntime>>,
    target: String,
) -> Result<serde_json::Value, String> {
    let id = shed_id(&target)?;
    runtime
        .hosts()
        .probe(&id)
        .await
        .map_err(|f| roost_failure_string(f.stage.as_str(), f.message))
}

/// `roost_preview` — the probe, the plan, and the sentence naming where the
/// bytes would come from: the consent card's content. Nothing is fetched here.
#[tauri::command]
async fn roost_preview(
    runtime: tauri::State<'_, Arc<ShedDesktopRuntime>>,
    target: String,
) -> Result<serde_json::Value, String> {
    let id = shed_id(&target)?;
    runtime
        .hosts()
        .preview(&id)
        .await
        .map_err(|f| roost_failure_string(f.stage.as_str(), f.message))
}

/// `roost_bootstrap` — install/update/start a `roost-session` on `target` and
/// wire its agent hooks, after consent. Mirrors `ipc::IpcHandler::roost_bootstrap`
/// exactly (same 10-minute outer budget, same two-refusal split: a
/// [`shed_desktop_core::Refusal`] is about the CALL and is a command Err
/// carrying its code; a stale fingerprint is a fact about the host and an
/// `ok` envelope carrying `ok: false`).
///
/// **`consent` is a caller-supplied boolean, and that is the pinned wire shape**
/// (plan 019 §3.6). It asserts that the caller rendered the §3.5 consent card and
/// a person confirmed it; it proves nothing on its own, because the backend keeps
/// no record that a card was ever shown — `roost.preview` followed by
/// `consent: true` is indistinguishable here from a confirmed click. The trust
/// boundary is the webview this command is reachable from, which loads only
/// bundled local assets. Making consent unforgeable would mean the backend
/// minting a token at preview time, bound to the fingerprint, that a bootstrap
/// required and consumed — a deliberate follow-up with a wire change in it, not a
/// gap nobody noticed. The full note lives on `ipc.rs`'s twin.
#[tauri::command]
async fn roost_bootstrap(
    runtime: tauri::State<'_, Arc<ShedDesktopRuntime>>,
    target: String,
    fingerprint: String,
    consent: bool,
) -> Result<serde_json::Value, String> {
    let id = shed_id(&target)?;
    let run = runtime.hosts().bootstrap(&id, &fingerprint, consent);
    let outcome = match tokio::time::timeout(ipc::BOOTSTRAP_BUDGET, run).await {
        Ok(outcome) => outcome,
        Err(_) => {
            return Err(roost_failure_string(
                "action_failed",
                format!(
                    "{target}: the bootstrap did not finish within {} minutes",
                    ipc::BOOTSTRAP_BUDGET.as_secs() / 60
                ),
            ))
        }
    };
    match outcome {
        Ok(Ok(installed)) => Ok(installed_json(&installed)),
        Ok(Err(failure)) => Ok(failure_json(&failure)),
        Err(refusal) => {
            let code = refusal.code();
            Err(roost_failure_string(code, refusal.message()))
        }
    }
}

// -- agent lanes (plan 015 §3.4) ------------------------------------------
//
// The frontend twins of the `lane.*` IPC ops. Both doors land in the SAME
// [`ShedLanes`] — the harness drives the socket, the panel drives these, and
// what is tested is what ships.
//
// A failure crosses as its `error.code` and message joined, because a Tauri
// command's error channel is a bare string: `bridge.ts` splits on the first ": "
// to recover the code. The socket ops keep the structured envelope.
//
// EVERY failure, including the ones the grammar raises before a lane op is
// reached. [`parse_mode`] / [`parse_answer`] answer with a typed
// [`LaneFailure`] precisely so this door cannot `?` a bare string through and
// lose the `bad_request` code that the socket door supplies for the same
// input — `bridge.ts` would then be splitting a code out of whatever the parse
// message happened to contain.
//
// The address is `{ target, session_id }` where `target` is
// `roost:<server>/<shed>`. There is no `machine` and no `kind`.

/// One lane failure as a string a `#[tauri::command]` can return.
pub(crate) fn lane_error(failure: LaneFailure) -> String {
    failure.command_string()
}

/// `lane_open` — start (or re-answer) a live transcript for one agent session
/// on a server-listed shed.
#[tauri::command]
async fn lane_open(
    runtime: tauri::State<'_, Arc<ShedDesktopRuntime>>,
    target: String,
    session_id: String,
) -> Result<serde_json::Value, String> {
    let id = shed_id(&target)?;
    runtime
        .lanes()
        .open(&id, &session_id)
        .await
        .map_err(lane_error)
}

/// `lane_messages` — the staged-then-swapped transcript view.
#[tauri::command]
fn lane_messages(
    runtime: tauri::State<'_, Arc<ShedDesktopRuntime>>,
    target: String,
    session_id: String,
) -> Result<serde_json::Value, String> {
    let id = shed_id(&target)?;
    runtime
        .lanes()
        .messages(&id, &session_id)
        .map_err(lane_error)
}

/// `lane_approvals` — what the session (or a descendant) is blocked on.
#[tauri::command]
fn lane_approvals(
    runtime: tauri::State<'_, Arc<ShedDesktopRuntime>>,
    target: String,
    session_id: String,
) -> Result<serde_json::Value, String> {
    let id = shed_id(&target)?;
    runtime
        .lanes()
        .approvals(&id, &session_id)
        .map_err(lane_error)
}

/// `lane_send` — a prompt. `mode` defaults to `queue`.
#[tauri::command]
async fn lane_send(
    runtime: tauri::State<'_, Arc<ShedDesktopRuntime>>,
    target: String,
    session_id: String,
    text: String,
    mode: Option<String>,
) -> Result<serde_json::Value, String> {
    let id = shed_id(&target)?;
    let mode = shed_desktop_core::parse_mode(mode.as_deref()).map_err(lane_error)?;
    runtime
        .lanes()
        .send(&id, &session_id, &text, mode)
        .await
        .map_err(lane_error)
}

/// `lane_cancel` — stop the turn in flight.
#[tauri::command]
async fn lane_cancel(
    runtime: tauri::State<'_, Arc<ShedDesktopRuntime>>,
    target: String,
    session_id: String,
) -> Result<serde_json::Value, String> {
    let id = shed_id(&target)?;
    runtime
        .lanes()
        .cancel(&id, &session_id)
        .await
        .map_err(lane_error)
}

/// `lane_stop` — end the SESSION (plan 025 §3.6.4), the twin of the
/// `lane.stop` socket op. The panel offers it only when the session's
/// capabilities say `stop`, behind an inline confirm.
#[tauri::command]
async fn lane_stop(
    runtime: tauri::State<'_, Arc<ShedDesktopRuntime>>,
    target: String,
    session_id: String,
) -> Result<serde_json::Value, String> {
    let id = shed_id(&target)?;
    runtime
        .lanes()
        .stop(&id, &session_id)
        .await
        .map_err(lane_error)
}

/// `lane_settings` — the session's settings, read now (plan 025 §3.10); the
/// twin of the `lane.settings` socket op. The sheet renders the stream's copy
/// (`lane_messages`' `settings`); this is the one-shot read.
#[tauri::command]
async fn lane_settings(
    runtime: tauri::State<'_, Arc<ShedDesktopRuntime>>,
    target: String,
    session_id: String,
) -> Result<serde_json::Value, String> {
    let id = shed_id(&target)?;
    runtime
        .lanes()
        .settings(&id, &session_id)
        .await
        .map_err(lane_error)
}

/// `lane_set` — change one setting (plan 025 §3.10), the twin of the
/// `lane.set` socket op and read by the same [`shed_desktop_core::parse_setting`]: the
/// settings sheet's every press. A refusal crosses with its code
/// (`not_accepting` — craze's `stale_model` among others — or
/// `outcome_unknown` for an answer lost to a drop, which the sheet shows "not
/// confirmed" until the next `Settings`).
#[tauri::command]
async fn lane_set(
    runtime: tauri::State<'_, Arc<ShedDesktopRuntime>>,
    target: String,
    session_id: String,
    change: serde_json::Value,
) -> Result<serde_json::Value, String> {
    let id = shed_id(&target)?;
    let change = shed_desktop_core::parse_setting(&change).map_err(lane_error)?;
    runtime
        .lanes()
        .set(&id, &session_id, change)
        .await
        .map_err(lane_error)
}

/// `lane_set_title` — rename the SESSION (plan 029 D12), the twin of the
/// `lane.set_title` socket op: the composer's `/rename <title>`. `{}`, or
/// `{warning}` ([`shed_desktop_core::renamed`]); every refusal crosses with its code.
#[tauri::command]
async fn lane_set_title(
    runtime: tauri::State<'_, Arc<ShedDesktopRuntime>>,
    target: String,
    session_id: String,
    title: String,
) -> Result<serde_json::Value, String> {
    let id = shed_id(&target)?;
    runtime
        .lanes()
        .set_title(&id, &session_id, &title)
        .await
        .map_err(lane_error)
}

// -- the composer's `/` menu (plan 029 §3.6 D12): pure, see `shed_desktop_core::slash` --

/// `lane_slash_filter` — what the menu panel shows for `draft` with the caret
/// at `caret` (UTF-16 code units): `{open, token, rows: [{row, dimmed}]}`.
#[tauri::command]
fn lane_slash_filter(
    menu: serde_json::Value,
    draft: String,
    caret: u32,
    activity: Option<String>,
) -> Result<serde_json::Value, String> {
    shed_desktop_core::slash::filter(menu, &draft, caret, activity.as_deref()).map_err(lane_error)
}

/// `lane_slash_dispatch` — what sending `draft` does: `not_command`,
/// `prompt {text}`, `run {action, arg?}` or `refuse {reason}`.
#[tauri::command]
fn lane_slash_dispatch(
    menu: serde_json::Value,
    draft: String,
    activity: Option<String>,
) -> Result<serde_json::Value, String> {
    shed_desktop_core::slash::dispatch(menu, &draft, activity.as_deref()).map_err(lane_error)
}

/// `lane_slash_accept` — what choosing the menu row `row` with `key`
/// (`enter` | `tab` | `pick`) does: `complete {draft, caret}`, `dispatch
/// {dispatch}`, or `null` (nothing to accept: re-filter).
#[tauri::command]
fn lane_slash_accept(
    menu: serde_json::Value,
    draft: String,
    row: String,
    key: String,
    activity: Option<String>,
) -> Result<serde_json::Value, String> {
    shed_desktop_core::slash::accept(menu, &draft, &row, &key, activity.as_deref())
        .map_err(lane_error)
}

/// `lane_answer` — resolve one approval. See [`shed_desktop_core::parse_answer`]
/// for the three shapes `answer` takes.
#[tauri::command]
async fn lane_answer(
    runtime: tauri::State<'_, Arc<ShedDesktopRuntime>>,
    target: String,
    session_id: String,
    approval_id: String,
    answer: serde_json::Value,
) -> Result<serde_json::Value, String> {
    let id = shed_id(&target)?;
    let answer = shed_desktop_core::parse_answer(&answer).map_err(lane_error)?;
    runtime
        .lanes()
        .answer(&id, &session_id, &approval_id, answer)
        .await
        .map_err(lane_error)
}

/// `lane_close` — the panel's unmount. Idempotent.
#[tauri::command]
fn lane_close(
    runtime: tauri::State<'_, Arc<ShedDesktopRuntime>>,
    target: String,
    session_id: String,
) -> Result<serde_json::Value, String> {
    let id = shed_id(&target)?;
    Ok(runtime.lanes().close(&id, &session_id))
}

// -- craze: the create sheet and Open in terminal (plan 025 §3.6.6, §3.8) --
//
// The frontend twins of the `craze.*` socket ops — the same [`ShedHosts`]
// methods, so what the harness proves about a create is what the sheet runs.
// `target` is a server-listed shed (`roost:<server>/<shed>`).
// A refusal crosses as `"<code>: <message>"` (the `lane_*` commands' rule),
// which `bridge.ts` splits back so the sheet can tell `outcome_unknown` (keep
// the request id) from a definite refusal (mint a new one).

/// `craze_create_options` — what a create can start on `target` (D8: read
/// afresh on every sheet open). May start a hub on a dormant shed.
#[tauri::command]
async fn craze_create_options(
    runtime: tauri::State<'_, Arc<ShedDesktopRuntime>>,
    target: String,
) -> Result<serde_json::Value, String> {
    let id = shed_id(&target)?;
    runtime
        .hosts()
        .craze_create_options(&id)
        .await
        .map_err(|f| f.command_string())
}

/// `craze_create` — a new craze session; `request_id` is the sheet's own
/// (reused only while the last outcome is unknown), and `held` says this
/// submission REPLAYS it (plan 026 §3.1). `title` is the sheet's "Name
/// (optional)" (plan 029 D14; blank is absent), kept exactly with a held
/// request. Read through [`shed_desktop_core::create_request`] and
/// [`shed_desktop_core::held_create`], the socket door's one reading.
#[tauri::command]
#[allow(clippy::too_many_arguments)]
async fn craze_create(
    runtime: tauri::State<'_, Arc<ShedDesktopRuntime>>,
    target: String,
    cwd: Option<String>,
    provider: Option<String>,
    prompt: Option<String>,
    title: Option<String>,
    request_id: Option<String>,
    held: Option<bool>,
) -> Result<serde_json::Value, String> {
    let id = shed_id(&target)?;
    let held = shed_desktop_core::held_create(held.unwrap_or(false), request_id.as_deref())
        .map_err(|f| f.command_string())?;
    let request = shed_desktop_core::create_request(
        cwd.as_deref(),
        provider.as_deref(),
        prompt.as_deref(),
        title.as_deref(),
        request_id.as_deref(),
    )
    .map_err(|f| f.command_string())?;
    runtime
        .hosts()
        .craze_create(&id, request, held)
        .await
        .map_err(|f| f.command_string())
}

/// `craze_saved` — the saved sessions the create sheet's Resume mode lists on
/// `target` (plan 032 §3.4), read afresh on every call; plus the host's
/// `home` where the app knows it. May start a hub on a dormant shed.
#[tauri::command]
async fn craze_saved(
    runtime: tauri::State<'_, Arc<ShedDesktopRuntime>>,
    target: String,
) -> Result<serde_json::Value, String> {
    let id = shed_id(&target)?;
    runtime
        .hosts()
        .craze_saved(&id)
        .await
        .map_err(|f| f.command_string())
}

/// `craze_reopen` — run the saved session `id` again (or join it where it
/// already runs), its row listed at once. A refusal's code keeps the reopen
/// error's variant (`gone`, `start_failed`, `unavailable`, or the inner lane
/// code) — [`shed_desktop_core::CrazeFailure::reopen`]. Read through
/// [`shed_desktop_core::reopen_id`], the socket door's one reading.
#[tauri::command]
async fn craze_reopen(
    runtime: tauri::State<'_, Arc<ShedDesktopRuntime>>,
    target: String,
    id: Option<String>,
) -> Result<serde_json::Value, String> {
    let target = shed_id(&target)?;
    let id = shed_desktop_core::reopen_id(id.as_deref()).map_err(|f| f.command_string())?;
    runtime
        .hosts()
        .craze_reopen(&target, &id)
        .await
        .map_err(|f| f.command_string())
}

/// `craze_open_terminal` — Open in terminal on a headless craze row: a roost
/// tab running `craze attach` on it.
#[tauri::command]
async fn craze_open_terminal(
    runtime: tauri::State<'_, Arc<ShedDesktopRuntime>>,
    target: String,
    session_id: String,
) -> Result<serde_json::Value, String> {
    let id = shed_id(&target)?;
    runtime
        .hosts()
        .craze_open_terminal(&id, &session_id)
        .await
        .map_err(|f| f.command_string())
}

// -- appearance (light/dark) sync across webviews ------------------------------

/// The app-wide light/dark appearance, held in Rust so a second webview (the
/// Preferences window) reads the initial value synchronously and every window stays
/// in sync via the `set-appearance` event. `None` = unset (fall back to the OS
/// `prefers-color-scheme`, popover-parity default).
pub(crate) struct AppearanceState(pub(crate) Mutex<Option<String>>);

/// The current app-wide appearance (`{mode: "light"|"dark"|null}`) — the Preferences
/// window reads this on mount so it opens in the dashboard's mode without a flash.
#[tauri::command]
fn get_appearance_state(app: tauri::AppHandle) -> serde_json::Value {
    let mode = app
        .state::<AppearanceState>()
        .0
        .lock()
        .ok()
        .and_then(|m| m.clone());
    serde_json::json!({ "mode": mode })
}

/// Set the app-wide appearance + broadcast `set-appearance` to every window.
/// Idempotent: a no-op (no re-emit) when unchanged, so the dashboard's mode effect
/// re-invoking this on an IPC-driven change can't loop. Invoked from the dashboard's
/// mode effect (both the manual toggle and IPC-driven changes flow through it).
#[tauri::command]
fn set_appearance_state(app: tauri::AppHandle, mode: String) -> Result<(), String> {
    if !matches!(mode.as_str(), "light" | "dark") {
        return Err(format!("unknown mode: {mode:?}"));
    }
    let cell = app.state::<AppearanceState>();
    {
        let mut cur = cell.0.lock().map_err(|_| "appearance state poisoned")?;
        if cur.as_deref() == Some(mode.as_str()) {
            return Ok(()); // unchanged — no re-emit (breaks the echo loop)
        }
        *cur = Some(mode.clone());
    }
    let _ = app.emit("set-appearance", serde_json::json!({ "mode": mode }));
    Ok(())
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

/// Whether the app applies its launch-at-login default: a release build, or the
/// harness (test mode). Never a debug build outside the harness.
fn login_default_applies(env: &Env) -> bool {
    !cfg!(debug_assertions) || env.test_mode
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

// -- credential sources (plan 035 §3.4): the Credentials view's Where rows --------
// (`source.status` is an IPC op only: the view reads `sources_list`.)

/// A refused move, unbind or Check as the Credentials view takes it: its code, from the
/// closed lists [`source::use_this_machine`], [`source::unbind`] and
/// [`delivery::preview`] give, which the view turns into its own fixed words (the
/// message is the same refusal, for a log).
fn coded_refusal((code, message): (String, String)) -> serde_json::Value {
    serde_json::json!({ "code": code, "message": message })
}

/// How the source doors resolve the servers ([`source::Resolver`]).
fn resolver(app: &tauri::AppHandle, backend: &Arc<Backend>) -> source::Resolver {
    let current = app
        .try_state::<Arc<source::CurrentServers>>()
        .map(|s| s.inner().clone());
    source::Resolver::new(current, backend.clone())
}

/// "Answer from here": make this machine the source on `host` (and reopen its delivery
/// stream there). The error is `{code, message}` ([`coded_refusal`]). Mirrors the
/// `source.use` IPC op (see [`source::use_this_machine`]).
#[tauri::command]
async fn source_use(
    app: tauri::AppHandle,
    backend: tauri::State<'_, Arc<Backend>>,
    host: String,
) -> Result<serde_json::Value, serde_json::Value> {
    let rt = delivery_runtime(&app).ok();
    source::use_this_machine(&resolver(&app, &backend), rt.as_deref(), &host)
        .await
        .map_err(coded_refusal)
}

/// Who answers on each configured server (this machine, another machine, the account
/// service, nobody) and this machine's stream there. Mirrors the `sources.list` IPC op
/// (see [`source::list`]).
#[tauri::command]
async fn sources_list(
    app: tauri::AppHandle,
    backend: tauri::State<'_, Arc<Backend>>,
) -> Result<serde_json::Value, String> {
    let rt = delivery_runtime(&app).ok();
    let servers = resolver(&app, &backend).now().await;
    Ok(source::list(&servers, rt.as_deref()).await)
}

/// "Stop answering from here": clear this user's credential source on `host`. The error
/// is `{code, message}` ([`coded_refusal`]). Mirrors the `source.unbind` IPC op (see
/// [`source::unbind`]).
#[tauri::command]
async fn source_unbind(
    app: tauri::AppHandle,
    backend: tauri::State<'_, Arc<Backend>>,
    host: String,
) -> Result<serde_json::Value, serde_json::Value> {
    let rt = delivery_runtime(&app).ok();
    source::unbind(&resolver(&app, &backend), rt.as_deref(), &host)
        .await
        .map_err(coded_refusal)
}

// -- provisioning by delivery (plan 039 A6): the same ops as the IPC socket's -------

fn delivery_runtime(
    app: &tauri::AppHandle,
) -> Result<Arc<shed_desktop_core::delivery::DeliveryRuntime>, String> {
    app.try_state::<Arc<shed_desktop_core::delivery::DeliveryRuntime>>()
        .map(|s| Arc::clone(&s))
        .ok_or_else(|| "the delivery runtime has not started".to_string())
}

/// `delivery.status`.
#[tauri::command]
fn delivery_status(app: tauri::AppHandle) -> Result<serde_json::Value, String> {
    Ok(delivery_runtime(&app)?.status())
}

/// `delivery.describe`.
#[tauri::command]
fn delivery_describe() -> serde_json::Value {
    delivery::describe()
}

/// `delivery.recent`.
#[tauri::command]
fn delivery_recent(app: tauri::AppHandle) -> Result<serde_json::Value, String> {
    Ok(delivery::recent(&*delivery_runtime(&app)?))
}

/// `delivery.settings`.
#[tauri::command]
fn delivery_settings(app: tauri::AppHandle) -> Result<serde_json::Value, String> {
    Ok(delivery_runtime(&app)?.settings())
}

/// `delivery.set`. `base` is the provider's `config` as the form read it, as JSON text
/// (text, so a provider not set up, `"null"`, is told from no base at all); a table
/// changed since is refused `conflict`. The error is `{code, message}`.
#[tauri::command]
fn delivery_set(
    app: tauri::AppHandle,
    provider: String,
    config: serde_json::Value,
    base: Option<String>,
) -> Result<serde_json::Value, serde_json::Value> {
    let rt = delivery_runtime(&app)
        .map_err(|m| serde_json::json!({ "code": "unavailable", "message": m }))?;
    let base = match base
        .as_deref()
        .map(serde_json::from_str::<serde_json::Value>)
    {
        None => None,
        Some(Ok(b)) => Some(b),
        Some(Err(_)) => {
            return Err(coded_refusal((
                "bad_request".into(),
                "base is not JSON".into(),
            )))
        }
    };
    delivery::set(&rt, &provider, &config, base.as_ref()).map_err(coded_refusal)
}

/// `delivery.preview` (the Credentials view's Check). The error is `{code, message}`;
/// the view words it from the code alone.
#[tauri::command]
async fn delivery_preview(
    app: tauri::AppHandle,
    backend: tauri::State<'_, Arc<Backend>>,
    server: String,
    shed: Option<String>,
    vm_backend: Option<String>,
    repo: Option<String>,
    runs: Option<Vec<String>>,
) -> Result<serde_json::Value, serde_json::Value> {
    let rt = delivery_runtime(&app)
        .map_err(|m| serde_json::json!({ "code": "unavailable", "message": m }))?;
    let p = delivery::PreviewParams {
        shed: shed.as_deref().filter(|s| !s.is_empty()),
        backend: vm_backend.as_deref().filter(|s| !s.is_empty()),
        repo: repo.as_deref().filter(|s| !s.is_empty()),
        expect_runs: runs,
    };
    delivery::preview(&rt, &resolver(&app, &backend), &server, p)
        .await
        .map_err(coded_refusal)
}

/// `shed.delivery`.
#[tauri::command]
async fn shed_delivery(
    backend: tauri::State<'_, Arc<Backend>>,
    server: String,
    shed: String,
) -> Result<serde_json::Value, String> {
    delivery::shed_delivery(&backend, &server, &shed)
        .await
        .map_err(|(_, m)| m)
}

/// `shed.provision`.
#[tauri::command]
async fn shed_provision(
    backend: tauri::State<'_, Arc<Backend>>,
    server: String,
    shed: String,
) -> Result<serde_json::Value, String> {
    delivery::shed_provision(&backend, &server, &shed)
        .await
        .map_err(|(_, m)| m)
}

/// `activity.server_list`.
#[tauri::command]
async fn activity_server_list(
    backend: tauri::State<'_, Arc<Backend>>,
    limit: Option<u64>,
    cursor: Option<String>,
    shed: Option<String>,
) -> Result<serde_json::Value, String> {
    delivery::activity(&backend, limit, cursor.as_deref(), shed.as_deref())
        .await
        .map_err(|(_, m)| m)
}

// -- the egress decision feed (plan 039 PR B; `crate::egress`) ----------------------

fn egress_feed(
    app: &tauri::AppHandle,
) -> Result<Arc<shed_desktop_core::egress::EgressFeed>, String> {
    app.try_state::<Arc<shed_desktop_core::egress::EgressFeed>>()
        .map(|s| Arc::clone(&s))
        .ok_or_else(|| "the egress feed has not started".to_string())
}

/// `egress.recent`: a shed incarnation's last decisions (`blocks`: only the denied ones).
#[tauri::command]
fn egress_recent(
    app: tauri::AppHandle,
    server: String,
    shed: String,
    created_at: String,
    blocks: Option<bool>,
) -> Result<serde_json::Value, String> {
    let feed = egress_feed(&app)?;
    egress::recent(&feed, &server, &shed, &created_at, blocks.unwrap_or(false)).map_err(|(_, m)| m)
}

/// `egress.status`: each server's egress stream.
#[tauri::command]
fn egress_status(app: tauri::AppHandle) -> Result<serde_json::Value, String> {
    let feed = egress_feed(&app)?;
    Ok(egress::status(&feed))
}

// -- menu-bar popover footer commands (B1b) — a 2nd webview can't call the IPC ops
//    or emit the main-window events, so the footer invokes these dedicated commands
//    (test-mode-safe: they only show/emit/exit, never spawn or write). --

/// The popover footer's "Open dashboard" → raise the dashboard on Sheds + dismiss.
#[tauri::command]
fn open_dashboard(app: tauri::AppHandle) {
    tray::open_dashboard(&app);
}

/// The popover footer's "Preferences…" + the dashboard header gear → open/focus
/// the dedicated Preferences window (and dismiss the popover if shown).
#[tauri::command]
fn open_preferences(app: tauri::AppHandle) {
    tray::open_preferences(&app);
}

/// The popover footer's "Quit".
#[tauri::command]
fn app_exit(app: tauri::AppHandle) {
    app.exit(0);
}

/// Content-size the menu-bar popover so it hugs its content (no dead space): the
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

// -- Sparkle updater (the popover "Check for Updates…" row + the drivable ops) --

/// The updater's status — `{ os: "macos"|"linux", enabled, reason }` (enabled ==
/// reason=="ok"). Drives the popover row (enabled vs a truthful disabled tooltip).
/// Platform-truthful everywhere: Linux ⇒ `linux_apt`, mac test mode ⇒ `test_mode`,
/// mac unbundled ⇒ `no_bundle`, else `ok`. Mirrors the `updater.status` IPC op.
#[tauri::command]
fn updater_status(app: tauri::AppHandle, env: tauri::State<'_, Env>) -> serde_json::Value {
    updater::status(&app, env.test_mode)
}

/// A user-invoked update check (the popover row's click). On the enabled path fronts
/// the app + presents Sparkle; otherwise returns an error string that distinguishes a
/// policy-disabled check (`updater_disabled:<reason>`, unchanged) from an operational
/// failure on the enabled path (`updater_failed:<msg>`). Mirrors the `updater.check`
/// IPC op (which maps the same split onto the `updater_disabled`/`action_failed` codes).
#[tauri::command]
fn updater_check(app: tauri::AppHandle, env: tauri::State<'_, Env>) -> Result<(), String> {
    updater::check(&app, env.test_mode).map_err(|e| e.to_string())
}

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
            // Before the log file opens, so a launch that only raises the running
            // app never touches it (the line waits in the pre-sink buffer and goes
            // with this process).
            app_info!("shed-desktop-tauri: already running (pid {pid}); activating it");
            let _ = single_instance::activate_running_instance(&env.socket_path);
            return;
        }
        Err(AcquireError::Io(e)) => {
            // Can't determine instance state — proceed rather than block the user
            // (the flock is best-effort robustness, not a correctness gate).
            app_warn!("shed-desktop-tauri: single-instance check failed ({e}); continuing");
            None
        }
    };

    // The log file (#414): opened only now that this process IS the running
    // instance; whatever was said before it (the env refusals above) is written
    // first. Installs the `tracing` subscriber too. Without the lock (the check
    // failed above and the app carries on) the file stays shut — another
    // instance may be writing it — so this run logs to stderr only, says why
    // once, and `identify.log_path` is null.
    applog::init(env.log_file.as_deref(), _instance.is_some(), env.log_debug);
    app_info!(
        "shed-desktop-tauri: starting: version {}, platform {}-{}, pid {}, log {}",
        env!("CARGO_PKG_VERSION"),
        std::env::consts::OS,
        std::env::consts::ARCH,
        std::process::id(),
        applog::active_path().map_or_else(
            || "off (stderr only)".to_string(),
            |p| p.display().to_string()
        )
    );

    // Shared with the IPC handler so `ui.current_pane` / `ui.computed_style` /
    // `dashboard.dump` read what the frontend reported.
    let ui: SharedUi = Arc::new(Mutex::new(UiState::default()));

    #[allow(unused_mut)] // only the macOS+non-test arm re-binds it (the sparkle plugin)
    let mut builder = tauri::Builder::default()
        // Launch-at-login (B4): register the plugin so `app.autolaunch()` resolves;
        // the plugin enables nothing on its own. The first-launch default in `setup`
        // and the React toggle both go through our guarded `login_item_set`, not the
        // plugin's JS API — so a test-mode write can't bypass the guard.
        .plugin(tauri_plugin_autostart::init(
            tauri_plugin_autostart::MacosLauncher::LaunchAgent,
            None,
        ))
        // Anchors the mac menu-bar popover at the tray icon (B1b); needs the tray
        // event forwarded (see `tray.rs::build`'s `on_tray_icon_event`).
        .plugin(tauri_plugin_positioner::init())
        .manage(ui.clone())
        .manage(env.clone())
        // The macOS test-mode login-item cell (see [`LoginItemCell`]).
        .manage(LoginItemCell(Mutex::new(false)))
        // The app-wide light/dark appearance, shared across webviews (unset at
        // launch → the OS `prefers-color-scheme` is the fallback).
        .manage(AppearanceState(Mutex::new(None)));

    // Real Sparkle updater — registered ONLY on macOS AND outside test mode, so the
    // updater is never instantiated under the harness (enforced at
    // registration, not inside the crate). The plugin's setup manages a
    // `SparkleUpdater` state iff it's a valid bundle (inert under `cargo run` / the
    // raw harness binary). Its post-init config (auto-checks off + the channel set)
    // is applied in `.setup` below via `updater::configure`.
    #[cfg(target_os = "macos")]
    if !env.test_mode {
        builder = builder.plugin(tauri_plugin_sparkle_updater::init());
    }

    builder
        .invoke_handler(tauri::generate_handler![
            ui_report,
            list_sheds,
            list_hosts,
            shed_action,
            extend_shed,
            cloud_usage,
            system_df,
            egress_profiles,
            create_start,
            create_status,
            create_cancel,
            terminal_presets,
            get_prefs,
            set_terminal_pref,
            rc_list,
            roost_probe,
            roost_preview,
            roost_bootstrap,
            lane_open,
            lane_messages,
            lane_approvals,
            lane_send,
            lane_cancel,
            lane_answer,
            lane_close,
            lane_stop,
            lane_settings,
            lane_set,
            lane_set_title,
            lane_slash_filter,
            lane_slash_dispatch,
            lane_slash_accept,
            craze_create_options,
            craze_create,
            craze_saved,
            craze_reopen,
            craze_open_terminal,
            open_terminal,
            get_appearance_state,
            set_appearance_state,
            loginitem_status,
            loginitem_set,
            source_use,
            sources_list,
            source_unbind,
            delivery_status,
            delivery_describe,
            delivery_recent,
            delivery_settings,
            delivery_set,
            delivery_preview,
            shed_delivery,
            shed_provision,
            activity_server_list,
            egress_recent,
            egress_status,
            open_dashboard,
            open_preferences,
            app_exit,
            resize_popover,
            updater_status,
            updater_check
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

            // Launch at login is on by default (plan 037 §3.1.4): turned on once, at the
            // first launch that can record having done so. Not in a debug build outside
            // the harness, so a developer's `make run` never registers its target/debug
            // binary; the harness (test mode) exercises it, contained.
            if login_default_applies(&env) {
                match prefs::apply_login_default(&prefs, || {
                    login_item_set(app.handle(), &env, true)
                }) {
                    prefs::LoginDefault::AlreadyApplied => {}
                    prefs::LoginDefault::TurnedOn => {
                        app_info!("shed-desktop-tauri: launch at login turned on (first launch)");
                    }
                    prefs::LoginDefault::NotRecorded(e) => app_warn!(
                        "shed-desktop-tauri: could not record that launch at login was set \
                         up ({e}); not turning it on"
                    ),
                    prefs::LoginDefault::SetFailed(e) => app_warn!(
                        "shed-desktop-tauri: could not turn on launch at login ({e}); switch \
                         it on in Preferences"
                    ),
                }
            }

            // The control-certificate mint (plan 039 PR B): `shed-source`'s SSH-bootstrap
            // minter over the shed CLI config, handed to the Backend BEFORE its first
            // request, so a cold launch enrols against an mtls server with no broker.
            // `backend_modes` is the per-server credential-mode registry (plan 002 §7
            // P1/P5), seeded from the config's `auth_mode` entries and attached as every
            // provider's CredentialObserver, so an adopted mode is learned. In-memory
            // only: config.yaml is CLI-owned and this app never writes it. Hermetic in
            // test mode (every host → the mock, which nothing mints for).
            let auth_modes = Arc::new(shed_app::AuthModeRegistry::new());
            let minter = shed_desktop_core::control_mint::control_minter(
                &env.config_path.to_string_lossy(),
                shed_desktop_core::control_mint::KNOWN_HOSTS,
            );
            let backend = Arc::new(Backend::from_env_parts_with_credentials(
                env.test_mode,
                env.mock_base_url.as_deref(),
                &env.config_path,
                Some(&minter),
                Some(&auth_modes),
                &env.mock_unreachable_hosts,
            ));
            app.manage(backend.clone());
            app.manage(auth_modes.clone());
            // The servers the source doors read: the config as it reads at each call,
            // each server's client kept while its whole entry is unchanged
            // (`source::CurrentServers`).
            {
                let build: source::BuildClients = {
                    let (env, minter, modes) = (env.clone(), minter.clone(), auth_modes.clone());
                    Arc::new(move |config: &shed_core::config::ShedConfig| {
                        let mock = if env.test_mode {
                            env.mock_base_url.as_deref()
                        } else {
                            None
                        };
                        if env.test_mode && mock.is_none() {
                            return Backend::from_config(&Default::default(), None);
                        }
                        modes.seed_from_config(config);
                        Backend::from_config_with_credentials(
                            config,
                            mock,
                            Some(&minter),
                            Some(&modes),
                            &env.mock_unreachable_hosts,
                        )
                    })
                };
                app.manage(Arc::new(source::CurrentServers::new(
                    env.config_path.clone(),
                    build,
                    &backend,
                    &shed_core::config::ShedConfig::load(&env.config_path.to_string_lossy()),
                )));
            }

            // Shed hosts and agent lanes (plan 038): one runtime owns both,
            // the spawn gate, and the orderly shutdown. Sheds are probed as
            // the app learns which ones are running (`observe_reachability_at`,
            // driven from the listing ops) and watched only once a session
            // answers on one. A host coming up or dropping emits `rc-changed`;
            // a lane frame emits `lane-event`.
            let runtime = Arc::new(ShedDesktopRuntime::start(
                tauri::async_runtime::handle().inner().clone(),
                &shed_core::config::ShedConfig::load(&env.config_path.to_string_lossy()),
                env.reach_options(),
                env.roost_jail_fs_root,
                Arc::new(TauriHostEvents {
                    app: app.handle().clone(),
                }),
                Arc::new(TauriLaneEvents {
                    app: app.handle().clone(),
                }),
            ));
            shutdown::install_shutdown_log(&runtime);
            // The source runtime (plan 039): a delivery stream per configured server,
            // and the egress decision feed on the Backend's control-scope clients.
            // Their drain is the shutdown's first step, so a tray quit,
            // `RunEvent::Exit`, and a signal all drain them.
            let delivery_rt = tauri::async_runtime::block_on(async {
                let handle = tokio::runtime::Handle::current();
                let env = env.clone();
                let backend = backend.clone();
                let fresh: delivery::FreshBackend = {
                    let (env, minter, modes) = (env.clone(), minter.clone(), auth_modes.clone());
                    Arc::new(move || {
                        Backend::from_env_parts_with_credentials(
                            env.test_mode,
                            env.mock_base_url.as_deref(),
                            &env.config_path,
                            Some(&minter),
                            Some(&modes),
                            &env.mock_unreachable_hosts,
                        )
                    })
                };
                tokio::task::spawn_blocking(move || delivery::start(&env, &handle, backend, fresh))
                    .await
                    .expect("the delivery runtime starts")
            });
            let egress_feed = tauri::async_runtime::block_on(async {
                egress::start(
                    &backend,
                    &tokio::runtime::Handle::current(),
                    app.handle().clone(),
                )
            });
            {
                let rt = delivery_rt.clone();
                let feed = egress_feed.clone();
                runtime.set_source_drain(Box::new(move || {
                    rt.drain();
                    feed.drain();
                }));
            }
            app.manage(delivery_rt);
            app.manage(egress_feed);
            app.manage(runtime.clone());
            shutdown::spawn_signal_thread(app.handle().clone(), runtime.shutdown_coordinator());

            // The terminal ops (preset resolution, launch, detection, the pref), shared
            // by the IPC handler + the frontend invoke commands (needs the Backend above).
            let terminal: termctl::SharedTerminal = Arc::new(termctl::TerminalCtl::new(
                backend.clone(),
                prefs.clone(),
                scripts_dir,
            ));
            app.manage(terminal.clone());

            let handler = Handler::new(
                env.clone(),
                app.handle().clone(),
                ui.clone(),
                backend.clone(),
                terminal,
                runtime,
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
                app_warn!("shed-desktop-tauri: tray unavailable ({e}); window-only");
            }

            // The mac menu-bar popover (B1b): a 2nd, opaque webview, created
            // HIDDEN + anchored at the tray on a left-click. macOS-only (Linux
            // emits no tray click events / has no popover). It fetches its own
            // data + reports under the `popover` window key, so it never clobbers
            // the dashboard's `main` snapshot.
            #[cfg(target_os = "macos")]
            {
                if let Err(e) = tauri::WebviewWindowBuilder::new(
                    app.handle(),
                    tray::POPOVER_ID,
                    tauri::WebviewUrl::App("popover.html".into()),
                )
                .title("shed")
                // Built at MAX height; the webview content-sizes it down via
                // `resize_popover` (no dead space). `resizable(true)` is
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
                    app_warn!("shed-desktop-tauri: popover window unavailable ({e})");
                }
                // Menu-bar-first in PRODUCTION: hide the dashboard at launch + become
                // an accessory (no Dock icon). "Open dashboard"
                // brings it back (Regular). Guarded so the harness keeps `main` shown +
                // never flips policy (else the webview may not mount → ui_report never
                // fires → wait_until(current_pane) times out).
                if !env.test_mode {
                    if let Some(main) = app.get_webview_window("main") {
                        let _ = main.hide();
                    }
                    ipc::set_activation_policy_prod(app.handle(), false);
                    // Post-init updater config (the plugin's setup already ran +
                    // managed the SparkleUpdater state if this is a valid bundle):
                    // automatic checks off + the beta-iff-prerelease channel set,
                    // applied before any user-invoked check_for_updates().
                    updater::configure(app.handle(), env!("CARGO_PKG_VERSION"));
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
            // A deliberate quit (a code carried: the tray's Quit, the popover's,
            // the test-mode `app.quit`, or the signal thread once its shutdown
            // finished) — the orderly shutdown (plan 026 §3.3), blocked on
            // here. This is the main thread, never a runtime worker, so
            // `block_on` is legal; the run itself is on the app's runtime.
            tauri::RunEvent::ExitRequested { code: Some(_), .. } => {
                shutdown::await_orderly_shutdown(app_handle);
            }
            // **The only event a macOS Dock Quit, logout or AppleScript quit
            // produces** (tao's `applicationWillTerminate`): the same orderly
            // shutdown. After any other exit path it is a wait for a run that
            // has already finished.
            tauri::RunEvent::Exit => {
                shutdown::await_orderly_shutdown(app_handle);
            }
            _ => {}
        });
}
