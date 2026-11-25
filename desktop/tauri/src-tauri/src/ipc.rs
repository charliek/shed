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

use shed_app::{Backend, Reachability};
use shed_core::models::CreateShedRequest;
use shed_desktop_core::{
    failure_json, installed_json, InjectedRow, LaneFailure, ShedDesktopRuntime, ShedHosts, ShedId,
};
use shed_lane::rc::RcKind;

use crate::env::Env;
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
/// The whole-op budget for `roost.bootstrap` (plan 019 §3.6).
///
/// Every remote step inside it is bounded by roost's own numbers (a 300-second
/// stream, a 60-second start, …); this is the outer guarantee that a transport
/// which wedges between them cannot hold an IPC connection open forever. Ten
/// minutes is comfortably more than the sum of the steps on a slow link and far
/// less than "until the app quits".
// `pub(crate)`: the `roost_bootstrap` Tauri command in `lib.rs` (the frontend's
// own door onto `ShedHosts::bootstrap`, beside this socket op) shares the same
// outer guarantee rather than picking its own number.
pub(crate) const BOOTSTRAP_BUDGET: Duration = Duration::from_secs(600);

/// Build an `(code, message)` error pair for the IPC error envelope.
fn err(code: &str, message: impl Into<String>) -> (String, String) {
    (code.to_string(), message.into())
}

/// `(target, session_id)` — the address every `lane.*` op takes. `target` is a
/// server-listed shed (`roost:<server>/<shed>`).
fn lane_target(params: &Value) -> Result<(ShedId, String), (String, String)> {
    let target = parse_shed(req_str(params, "target")?)?;
    let session_id = req_str(params, "session_id")?.to_string();
    Ok((target, session_id))
}

/// A shed target, or `bad_request` when the string is not `roost:<server>/<shed>`.
fn parse_shed(target: &str) -> Result<ShedId, (String, String)> {
    ShedId::parse(target).map_err(|message| err("bad_request", message))
}

/// A lane failure as the IPC error envelope. The codes are the lane contract's
/// own, so a client branches on the same vocabulary the DTOs speak.
fn lane_err(failure: LaneFailure) -> (String, String) {
    err(failure.code(), failure.message())
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
///
/// Each shed carries its `delivery` record (plan 039 §3.6) as its server sent it,
/// or `null` from a server older than delivery: the row's badge and detail read it.
pub(crate) fn sheds_payload(r: &Reachability) -> Value {
    let sheds: Vec<Value> = r
        .sheds
        .iter()
        .map(|s| {
            let mut v = serde_json::to_value(s).unwrap_or(Value::Null);
            if let Some(o) = v.as_object_mut() {
                let rec = r
                    .delivery
                    .get(&(s.host.clone(), s.name.clone()))
                    .cloned()
                    .unwrap_or(Value::Null);
                o.insert("delivery".to_string(), rec);
            }
            v
        })
        .collect();
    json!({ "sheds": sheds, "host_errors": r.host_errors })
}

/// Refresh reachability and reconcile shed hosts against it.
///
/// The sequence is taken BEFORE the fetch, so a slow answer cannot
/// reauthorize a shed a newer refresh already removed
/// ([`shed_desktop_core::observe_reachability_at`]).
pub(crate) async fn refresh_hosts(backend: &Backend, hosts: &ShedHosts) -> Reachability {
    let seq = hosts.next_observe_seq();
    let reachability = backend.refresh().await;
    shed_desktop_core::observe_reachability_at(hosts, seq, &reachability).await;
    reachability
}

/// A required string param, or a `bad_request` error naming the missing key — the
/// shared shape behind the shed_action/create ops' param extraction.
fn req_str<'a>(params: &'a Value, key: &str) -> Result<&'a str, (String, String)> {
    params
        .get(key)
        .and_then(Value::as_str)
        .ok_or_else(|| err("bad_request", format!("missing '{key}'")))
}

/// The one gate + router behind the three roost-consent driver ops
/// (`ui.show_roost_consent` / `ui.confirm_roost_consent` /
/// `ui.close_roost_consent`): the event name and payload to emit, or the refusal.
///
/// **TEST-MODE ONLY**, the `rc.inject_test` rule. These exist so
/// the harness can drive a dialog that only opens from a card's plan-matrix
/// BUTTON — it has no click. But showing the card and confirming it are the two
/// halves of a user's consent, and a socket client that can do both in
/// succession can start a bootstrap nobody clicked: an install, a dotfile-mutating
/// hook wiring, on a host of the user's. That is a privilege, so it is gated like
/// the other privileged driver ops.
///
/// One function rather than a check per arm, so the gate cannot be true of two
/// doors and forgotten on the third: the dispatch arm has no event to emit unless
/// this answers `Ok`, and this is also what the unit test below drives.
fn roost_consent_door(
    env: &Env,
    op: &str,
    params: &Value,
) -> Result<(&'static str, Value), (String, String)> {
    if !env.test_mode {
        return Err(err("not_enabled", format!("{op} requires test mode")));
    }
    match op {
        // `target` rides the OPEN event rather than being inferred from whatever
        // the frontend last previewed: `roost_consent.dump` (the dialog's own
        // rendered copy) only means anything once one is mounted for a known host.
        "ui.show_roost_consent" => Ok((
            "show-roost-consent",
            json!({ "target": req_str(params, "target")? }),
        )),
        // Drives the dialog's own Confirm button — the mounted dialog listens for
        // this the same way it would a pointer event, and runs the identical
        // handler.
        "ui.confirm_roost_consent" => Ok(("confirm-roost-consent", json!({}))),
        "ui.close_roost_consent" => Ok(("close-roost-consent", json!({}))),
        other => Err(err("unknown_op", format!("{other} is not a consent door"))),
    }
}

/// Reject an unknown (preserved-raw) `RcKind`. The unknown-kind policy
/// preserves such a value on READ, but `rc.inject_test` will not plant a kind
/// this build does not understand.
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

/// The one gate + router behind the craze create sheet's two driver doors
/// (`ui.fill_craze_create` / `ui.submit_craze_create`, plan 025 C10): the event
/// name and payload to emit, or the refusal.
///
/// **TEST-MODE ONLY**, [`roost_consent_door`]'s rule and for its reason: the sheet's
/// request-id lifecycle (§3.8) is only reachable by filling it in and pressing
/// its button — the very thing a harness has no keyboard or pointer for — while
/// `craze_create.dump` (the sheet's own rendered truth) is what proves it.
/// Filling a person's sheet and pressing Create for them is nothing a shipped
/// app needs; the production door onto the same backend is `craze.create`.
///
/// `ui.fill_craze_create {provider?, cwd?, title?, prompt?, recent?}` sets
/// those controls the way a person would (an absent key is left as it is):
/// `provider` clicks that provider's row (a dimmed one refuses the click, as it
/// refuses a pointer), `cwd`, `title` ("Name (optional)", plan 029 D14) and
/// `prompt` type into their fields, and `recent: <n>` taps the n-th
/// recent-directory chip. `ui.submit_craze_create` presses the sheet's
/// primary button — Create, or Try again — through the button's own gate.
///
/// The sheet's Resume mode (plan 032 §3.4, PC14i) has two more, on the same
/// rule: `ui.choose_craze_mode {mode: "new" | "resume"}` presses that half of
/// the sheet's `New | Resume` control (refused, as a pointer is, while the
/// control is not shown or is disabled — choosing Resume the first time is
/// what lists the saved sessions), and `ui.resume_saved {id}` presses the
/// saved session `id`'s button — Resume, or Retry after `unavailable` —
/// through ITS gate (one reopen at a time: every other button is disabled
/// while one runs).
fn craze_create_door(
    env: &Env,
    op: &str,
    params: &Value,
) -> Result<(&'static str, Value), (String, String)> {
    if !env.test_mode {
        return Err(err("not_enabled", format!("{op} requires test mode")));
    }
    match op {
        "ui.fill_craze_create" => {
            let mut fill = serde_json::Map::new();
            for key in ["provider", "cwd", "title", "prompt"] {
                match params.get(key) {
                    None | Some(Value::Null) => {}
                    Some(Value::String(s)) => {
                        fill.insert(key.to_string(), json!(s));
                    }
                    Some(_) => return Err(err("bad_request", format!("'{key}' must be a string"))),
                }
            }
            match params.get("recent") {
                None | Some(Value::Null) => {}
                Some(v) => match v.as_u64() {
                    Some(n) => {
                        fill.insert("recent".to_string(), json!(n));
                    }
                    None => {
                        return Err(err("bad_request", "'recent' must be a non-negative index"))
                    }
                },
            }
            Ok(("fill-craze-create", Value::Object(fill)))
        }
        "ui.submit_craze_create" => Ok(("submit-craze-create", json!({}))),
        "ui.choose_craze_mode" => match params.get("mode").and_then(Value::as_str) {
            Some(mode @ ("new" | "resume")) => Ok(("choose-craze-mode", json!({ "mode": mode }))),
            _ => Err(err("bad_request", "'mode' must be \"new\" or \"resume\"")),
        },
        "ui.resume_saved" => match params.get("id").and_then(Value::as_str) {
            Some(id) if !id.trim().is_empty() => Ok(("resume-saved", json!({ "id": id }))),
            _ => Err(err(
                "bad_request",
                "'id' must name a saved session, as craze_create.dump lists it",
            )),
        },
        other => Err(err(
            "unknown_op",
            format!("{other} is not a craze create door"),
        )),
    }
}

/// The gate + reader behind `ui.press_source_use {host}` (plan 035 §3.4): the payload
/// to emit, or the refusal.
///
/// **TEST-MODE ONLY**, [`pick_setting_door`]'s rule. The open Preferences window finds
/// ITS OWN "Use this machine" button for `host` (`[data-source-use]`) and clicks it, so
/// the press runs the button's handler and honours its disabled state exactly as a
/// pointer would; a host with no enabled button is a press that lands nowhere.
/// The gate on plan 039 PR B's harness ops — `activity.dump`, `shed_egress.dump`,
/// `ui.press_activity_older`, `ui.scroll_to`, `ui.scrolled`: **TEST-MODE ONLY**, the
/// `ui.report_history` rule. They exist so the harness can read what the Activity pane
/// and a shed's egress section rendered, and drive what a pointer does; a shipped app
/// answers `not_enabled`.
fn test_mode_door(env: &Env, op: &str) -> Result<(), (String, String)> {
    if !env.test_mode {
        return Err(err("not_enabled", format!("{op} requires test mode")));
    }
    Ok(())
}

fn press_source_door(env: &Env, op: &str, params: &Value) -> Result<Value, (String, String)> {
    if !env.test_mode {
        return Err(err("not_enabled", format!("{op} requires test mode")));
    }
    let host = req_str(params, "host")?;
    if host.is_empty() {
        return Err(err("bad_request", "'host' names a server"));
    }
    Ok(json!({ "host": host }))
}

/// The gate + reader behind the settings sheet's one driver door,
/// `ui.pick_lane_setting {row, value}` (plan 025 §3.10, C11): the payload to
/// emit, or the refusal.
///
/// **TEST-MODE ONLY**, [`craze_create_door`]'s rule and for its reason: what
/// the sheet does with a change — the row's `pending` until craze answers, a
/// refusal inline on its row, "not confirmed" after a lost answer until the
/// next `Settings` — happens only when a person presses one of its values,
/// which a harness has no pointer for; `lane_settings.dump` (the sheet's own
/// rendered truth) is what proves it. The production door onto the same
/// backend is `lane.set`.
///
/// `row` is the row's id as the dump names it — `model`, `mode`, or an
/// option's id — and `value` the id of the value to press, both verbatim
/// (opaque: never trimmed). The sheet presses it through the control's own
/// gate: a value it does not render, or a row whose change is still pending,
/// takes no press.
fn pick_setting_door(env: &Env, params: &Value) -> Result<Value, (String, String)> {
    if !env.test_mode {
        return Err(err(
            "not_enabled",
            "ui.pick_lane_setting requires test mode",
        ));
    }
    let row = req_str(params, "row")?;
    let value = req_str(params, "value")?;
    if row.is_empty() {
        return Err(err("bad_request", "'row' names a settings row"));
    }
    Ok(json!({ "row": row, "value": value }))
}

/// The gate + reader behind `ui.hold_lane_view {hold}` (plan 025 Amendment
/// A13's harness door): the payload to emit, or the refusal.
///
/// **TEST-MODE ONLY**, [`pick_setting_door`]'s rule. While held, the open
/// transcript panel goes on reading its lane and commits nothing it reads, so
/// it keeps SHOWING what it showed — the moment between the adapter folding a
/// change and the panel rendering it, which in a shipped app lasts a frame,
/// held open so a harness can press inside it (a settings option chosen on
/// the model the sheet still displays). `hold: false` releases it and the
/// panel reads afresh. Nothing a shipped app needs.
fn hold_view_door(env: &Env, params: &Value) -> Result<Value, (String, String)> {
    if !env.test_mode {
        return Err(err("not_enabled", "ui.hold_lane_view requires test mode"));
    }
    Ok(json!({ "hold": req_bool(params, "hold")? }))
}

/// The keys `ui.key_lane_composer` presses — the ones the composer's `/` menu
/// and its Send handle (plan 029 P9). A closed set, so a typo (`Esc`) is a
/// refusal rather than a key the composer silently ignores: a cell asserting
/// "nothing happened" must not pass because nothing was pressed.
const COMPOSER_KEYS: [&str; 5] = ["Enter", "Tab", "Escape", "ArrowUp", "ArrowDown"];

/// The one gate + router behind the transcript composer's driver doors (plan
/// 029 D13): the event name and payload to emit, or the refusal.
///
/// **TEST-MODE ONLY**, [`craze_create_door`]'s rule and for its reason: the
/// `/` menu is reachable only by typing into the composer and pressing keys —
/// what a harness has no keyboard for — while `lane.dump`'s `slash_menu` (the
/// panel's own rendered truth) is what proves it. The production doors onto
/// the same backend are the lane verbs (`lane.send`, `lane.set`,
/// `lane.set_title`, `lane.stop`).
///
/// * `ui.type_lane_composer {text}` — the composer's draft becomes `text`, the
///   caret at its end, as if typed (an empty `text` clears it).
/// * `ui.key_lane_composer {key, shift?, composing?}` — a REAL `keydown`
///   (`KeyboardEvent`, P27) dispatched on the composer's textarea: `key` one
///   of [`COMPOSER_KEYS`], `shift` its Shift, `composing` an IME composition
///   in progress (`isComposing`). The browser's own default for an Enter the
///   composer did not take — a newline — is applied by the door, since a
///   synthetic event carries none.
/// * `ui.pick_slash_row {name}` — a click on the open menu's row named
///   `name`, through the menu's own gate (a row it does not list takes none).
/// * `ui.answer_lane_stop {stop}` — a click on the open Stop confirm's
///   buttons: `true` is Stop session, `false` is Keep (the confirm `/exit`
///   opens, §3.5: Keep sends nothing and keeps the draft). Nothing while no
///   confirm is open.
fn composer_door(
    env: &Env,
    op: &str,
    params: &Value,
) -> Result<(&'static str, Value), (String, String)> {
    if !env.test_mode {
        return Err(err("not_enabled", format!("{op} requires test mode")));
    }
    match op {
        "ui.type_lane_composer" => {
            let text = req_str(params, "text")?;
            Ok(("type-lane-composer", json!({ "text": text })))
        }
        "ui.key_lane_composer" => {
            let key = req_str(params, "key")?;
            if !COMPOSER_KEYS.contains(&key) {
                return Err(err(
                    "bad_request",
                    format!(
                        "unknown composer key {key:?} ({})",
                        COMPOSER_KEYS.join(" | ")
                    ),
                ));
            }
            Ok((
                "key-lane-composer",
                json!({ "key": key, "shift": opt_bool(params, "shift")?,
                        "composing": opt_bool(params, "composing")? }),
            ))
        }
        "ui.pick_slash_row" => {
            let name = req_str(params, "name")?;
            if name.is_empty() {
                return Err(err("bad_request", "'name' names a menu row"));
            }
            Ok(("pick-slash-row", json!({ "name": name })))
        }
        "ui.answer_lane_stop" => Ok((
            "answer-lane-stop",
            json!({ "stop": req_bool(params, "stop")? }),
        )),
        other => Err(err("unknown_op", format!("{other} is not a composer door"))),
    }
}

/// `lane.dump`'s `lane`: the transcript panel's report with its composer's
/// (`draft`, `slash_menu`) joined in. The panel reports the composer apart so
/// a keystroke re-sends a few fields rather than the whole transcript; no
/// panel (`lane` absent or `null`) is `null`, whatever the composer said last.
fn lane_report(lane: Option<Value>, composer: Option<Value>) -> Value {
    match (lane, composer) {
        (Some(Value::Object(mut lane)), Some(Value::Object(composer))) => {
            lane.extend(composer);
            Value::Object(lane)
        }
        (lane, _) => lane.unwrap_or(Value::Null),
    }
}

/// The gate behind `ui.report_history` (plan 026 §3.4, shed#400): whether to
/// clear the ring after reading it, or the refusal.
///
/// **TEST-MODE ONLY**, the other driver doors' rule. It answers what every one
/// of the dashboard's full reports carried ([`crate::state::ReportRecord`]), so
/// a cell can assert that NO report zeroed the Hosts badge — a sampled read can
/// fall between two reports and miss one. Nothing a shipped app needs.
fn report_history_door(env: &Env, params: &Value) -> Result<bool, (String, String)> {
    if !env.test_mode {
        return Err(err("not_enabled", "ui.report_history requires test mode"));
    }
    opt_bool(params, "clear")
}

/// The gate behind `app.quit` (plan 026 §3.3): the tray's Quit, drivable.
///
/// **TEST-MODE ONLY**, the other driver doors' rule. It is `app.exit(0)` —
/// exactly what the tray's Quit and the popover's call — so the harness can
/// prove the tray's path runs the orderly shutdown. A socket client that could
/// quit the user's app is nothing a shipped app needs.
fn app_quit_door(env: &Env) -> Result<(), (String, String)> {
    if !env.test_mode {
        return Err(err("not_enabled", "app.quit requires test mode"));
    }
    Ok(())
}

/// What `app.terminate` answers, in test mode, on every platform but macOS.
const TERMINATE_NOT_SUPPORTED: &str = "app.terminate is the macOS system-quit path; use app.quit";

/// The gate behind `app.terminate` (plan 026 AM7): the macOS system quit — a
/// Dock Quit, a logout, an AppleScript `quit` — drivable.
///
/// **TEST-MODE ONLY**, [`app_quit_door`]'s rule: outside test mode it answers
/// `not_enabled`, on every platform. In test mode it opens on macOS only.
/// `[NSApplication terminate:]` is an AppKit path, so every other platform
/// answers `not_supported` and points at `app.quit`.
fn app_terminate_door(env: &Env) -> Result<(), (String, String)> {
    if !env.test_mode {
        return Err(err("not_enabled", "app.terminate requires test mode"));
    }
    if cfg!(target_os = "macos") {
        Ok(())
    } else {
        Err(err("not_supported", TERMINATE_NOT_SUPPORTED))
    }
}

/// `[NSApp terminate:nil]`: the AppKit path that a Dock Quit, a logout and an
/// AppleScript `quit` all take. AppKit sends `applicationShouldTerminate:`,
/// then `applicationWillTerminate:`, which tao turns into `RunEvent::Exit` and
/// nothing else. That arm runs the orderly shutdown. AppKit then ends the
/// process with its own `exit(0)`.
///
/// **Queued from the main thread, not sent inline.** A `run_on_main_thread`
/// closure runs inside tao's event handler, which holds that handler's lock
/// (tao `app_state.rs`, `Handler::handle_user_events`). A `terminate:` sent
/// from inside it would reach `applicationWillTerminate:` and then
/// `AppState::exit`, which takes the same lock again on the same thread: a
/// deadlock. So the closure only queues the message, with
/// `performSelectorOnMainThread:withObject:waitUntilDone:NO` in the common
/// run-loop modes. The main run loop delivers it on its next pass, outside any
/// tao callback, the way AppKit delivers a Dock or AppleScript quit. The IPC
/// answer is on its way before then.
#[cfg(target_os = "macos")]
fn system_terminate(app: &AppHandle) -> Result<(), (String, String)> {
    app.run_on_main_thread(|| {
        use objc2::runtime::AnyObject;
        use objc2::{msg_send, sel, MainThreadMarker};
        use objc2_app_kit::NSApplication;
        let Some(mtm) = MainThreadMarker::new() else {
            app_warn!("shed-desktop-tauri: app.terminate: not on the main thread; nothing sent");
            return;
        };
        let ns_app = NSApplication::sharedApplication(mtm);
        let sender: Option<&AnyObject> = None;
        // SAFETY: `performSelectorOnMainThread:withObject:waitUntilDone:` is
        // NSObject's `(SEL, id, BOOL) -> void`, and `terminate:` takes one
        // object argument, its sender, which may be nil.
        unsafe {
            let _: () = msg_send![
                &*ns_app,
                performSelectorOnMainThread: sel!(terminate:),
                withObject: sender,
                waitUntilDone: false
            ];
        }
    })
    .map_err(|e| err("internal", format!("app.terminate: {e}")))
}
#[cfg(not(target_os = "macos"))]
fn system_terminate(_app: &AppHandle) -> Result<(), (String, String)> {
    // Unreachable: [`app_terminate_door`] answers `not_supported` off macOS,
    // before this is called.
    Err(err("not_supported", TERMINATE_NOT_SUPPORTED))
}

/// An OPTIONAL string param: absent or `null` is `None`; a string is itself;
/// anything else is `bad_request` — never read as absent, because absence has a
/// meaning of its own (a default, or a freshly minted id).
fn opt_str<'a>(params: &'a Value, key: &str) -> Result<Option<&'a str>, (String, String)> {
    match params.get(key) {
        None | Some(Value::Null) => Ok(None),
        Some(Value::String(s)) => Ok(Some(s)),
        Some(_) => Err(err("bad_request", format!("'{key}' must be a string"))),
    }
}

/// An OPTIONAL bool param: absent or `null` is `false`; anything but a bool is
/// `bad_request` — never read as absent.
fn opt_bool(params: &Value, key: &str) -> Result<bool, (String, String)> {
    match params.get(key) {
        None | Some(Value::Null) => Ok(false),
        _ => req_bool(params, key),
    }
}

/// A REQUIRED bool param, or `bad_request`.
fn req_bool(params: &Value, key: &str) -> Result<bool, (String, String)> {
    params
        .get(key)
        .and_then(Value::as_bool)
        .ok_or_else(|| err("bad_request", format!("'{key}' must be true or false")))
}

/// `craze.create`'s request from the socket's params: every field TYPED
/// ([`opt_str`]) — a `request_id: 7` is `bad_request`, not an absent id that
/// [`shed_desktop_core::create_request`] would replace with a fresh one, which
/// would turn a retry of an unknown outcome into a second session — then read
/// the one way both doors read it.
fn craze_create_request(
    params: &Value,
) -> Result<shed_lane::lane::LaneCreateRequest, (String, String)> {
    shed_desktop_core::create_request(
        opt_str(params, "cwd")?,
        opt_str(params, "provider")?,
        opt_str(params, "prompt")?,
        opt_str(params, "title")?,
        opt_str(params, "request_id")?,
    )
    .map_err(craze_err)
}

/// `craze.create`'s `held` from the socket's params (plan 026 §3.1): TYPED
/// like every other field — absent or `null` is a fresh create, a non-bool is
/// `bad_request` — and, set, it needs the `request_id` it replays
/// ([`shed_desktop_core::held_create`]).
fn craze_create_held(params: &Value) -> Result<bool, (String, String)> {
    shed_desktop_core::held_create(opt_bool(params, "held")?, opt_str(params, "request_id")?)
        .map_err(craze_err)
}

/// A craze op's refusal as the IPC error envelope ([`shed_desktop_core::CrazeFailure`]).
fn craze_err(failure: shed_desktop_core::CrazeFailure) -> (String, String) {
    err(failure.code, failure.message)
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
/// front-if-already-open after. Closing it only HIDES it (the generic
/// `CloseRequested` arm in `lib.rs` hides + prevents close for every window), so a
/// reopen is a plain show+focus. Creation is marshalled to the main
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
            app_warn!("shed-desktop-tauri: preferences window unavailable ({e})");
            return;
        }
        set_activation_policy_prod(&handle, true);
    });
}

/// Flip the macOS activation policy in PRODUCTION only (guarded off under the
/// harness — an unguarded flip can leave `main` unmounted so `ui_report` never fires
/// and `wait_until(current_pane)` times out). `regular` shows the Dock icon (a
/// dashboard window is open); otherwise `Accessory` = menu-bar-first (no Dock icon,
/// the tray/popover is the surface).
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
/// a running Tauri app / `AppHandle`. `log_path` is the log file the app is
/// writing (#414) — `null` when file logging is off.
fn identify_payload(env: &Env, pid: u32, log_path: Option<&std::path::Path>) -> Value {
    json!({
        "socket_path": env.socket_path.to_string_lossy(),
        "pid": pid,
        "core": "rust",
        "platform": "tauri",
        "test_mode": env.test_mode,
        "mock_base_url": env.mock_base_url,
        "log_path": log_path.map(|p| p.to_string_lossy()),
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
    /// Shed hosts, agent lanes, and the orderly shutdown (plan 038).
    runtime: Arc<ShedDesktopRuntime>,
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
        runtime: Arc<ShedDesktopRuntime>,
    ) -> Self {
        Self {
            env,
            app,
            ui,
            backend,
            terminal,
            runtime,
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
            "identify" => Ok(identify_payload(
                &self.env,
                self.pid,
                crate::applog::active_path().as_deref(),
            )),
            "ui.navigate" => self.navigate(params),
            "ui.current_pane" => Ok(json!({ "pane": self.ui_get("pane") })),
            "ui.computed_style" => Ok(json!({ "style": self.ui_get("style") })),
            // Which modal (if any) the frontend has open: "create" | "launch" |
            // null. (Preferences is a dedicated window, not a modal — see prefs.dump.)
            "ui.modal" => Ok(json!({ "modal": self.ui_get("modal") })),
            // The sidebar nav badge counts the shell reported {sheds, agents, hosts,
            // pending}, or null before its first report.
            "ui.badges" => Ok(json!({ "badges": self.ui_get("badges") })),
            "ui.report_history" => {
                let clear = report_history_door(&self.env, params)?;
                let (reports, dropped) = self
                    .ui
                    .lock()
                    .map(|mut s| s.report_history(clear))
                    .unwrap_or_default();
                Ok(json!({ "reports": reports, "dropped": dropped }))
            }
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
            // The roost bootstrap consent dialog's drivable doors (plan 019
            // §3.6/C8) — TEST-MODE ONLY, see [`roost_consent_door`].
            "ui.show_roost_consent" | "ui.confirm_roost_consent" | "ui.close_roost_consent" => {
                let (event, payload) = roost_consent_door(&self.env, op, params)?;
                // Opening raises the window, like `show-create`/`show-launch`;
                // confirming and closing act on one that is already up.
                if event == "show-roost-consent" {
                    present_main_window(&self.app);
                }
                let _ = self.app.emit(event, payload);
                Ok(json!({}))
            }
            // The agent-lane transcript panel (plan 015 §3.4), on the
            // show-create/show-launch pattern. The panel opens from a card's
            // Transcript affordance — a CLICK, which no caller here has — and
            // `lane.dump` (the panel's own rendered truth) only means anything
            // once one is mounted, so open and close are drivable ops.
            //
            // Deliberately no lane bookkeeping: this only asks the frontend to
            // mount a panel, and the panel's own mount/unmount is what opens and
            // closes the lane. Two doors into one `Lanes` (`lib.rs`'s commands
            // and this module's `lane.*`), not three.
            "ui.show_lane" => {
                let (target, session_id) = lane_target(params)?;
                present_main_window(&self.app);
                let _ = self.app.emit(
                    "show-lane",
                    json!({ "target": target, "session_id": session_id }),
                );
                Ok(json!({}))
            }
            "ui.close_lane" => {
                let _ = self.app.emit("close-lane", json!({}));
                Ok(json!({}))
            }
            // The craze create sheet (plan 025 §3.8, C10), on the show-lane
            // pattern: it opens from a shed row's "New craze session" — a
            // CLICK — and `craze_create.dump` only means anything once one is
            // mounted. Opening it runs `create_options`, an explicit action
            // that may start a hub on a dormant shed, exactly as the click does.
            "ui.show_craze_create" => {
                let target = parse_shed(req_str(params, "target")?)?;
                present_main_window(&self.app);
                let _ = self
                    .app
                    .emit("show-craze-create", json!({ "target": target }));
                Ok(json!({}))
            }
            "ui.close_craze_create" => {
                let _ = self.app.emit("close-craze-create", json!({}));
                Ok(json!({}))
            }
            // Its typing-and-clicking doors — TEST-MODE ONLY, see
            // [`craze_create_door`]. They act on a sheet that is already open:
            // its create form, and its Resume mode (plan 032 §3.4).
            "ui.fill_craze_create"
            | "ui.submit_craze_create"
            | "ui.choose_craze_mode"
            | "ui.resume_saved" => {
                let (event, payload) = craze_create_door(&self.env, op, params)?;
                let _ = self.app.emit(event, payload);
                Ok(json!({}))
            }
            // The settings sheet (plan 025 §3.10, C11), on the show-lane
            // pattern: it opens from the transcript header's settings chip — a
            // CLICK — and `lane_settings.dump` only means anything once one is
            // open. Showing it mounts the transcript panel for that session
            // too (the sheet is the panel's); a session whose capabilities say
            // `settings: false` has no chip, and no sheet opens.
            "ui.show_lane_settings" => {
                let (target, session_id) = lane_target(params)?;
                present_main_window(&self.app);
                let _ = self.app.emit(
                    "show-lane-settings",
                    json!({ "target": target, "session_id": session_id }),
                );
                Ok(json!({}))
            }
            "ui.close_lane_settings" => {
                let _ = self.app.emit("close-lane-settings", json!({}));
                Ok(json!({}))
            }
            // Its press door — TEST-MODE ONLY, see [`pick_setting_door`]. It
            // acts on a sheet that is already open.
            "ui.pick_lane_setting" => {
                let payload = pick_setting_door(&self.env, params)?;
                let _ = self.app.emit("pick-lane-setting", payload);
                Ok(json!({}))
            }
            // The composer's typing, key, menu-click and Stop-confirm doors
            // (plan 029 D13) — TEST-MODE ONLY, see [`composer_door`]. They act
            // on a transcript panel that is already open.
            "ui.type_lane_composer"
            | "ui.key_lane_composer"
            | "ui.pick_slash_row"
            | "ui.answer_lane_stop" => {
                let (event, payload) = composer_door(&self.env, op, params)?;
                let _ = self.app.emit(event, payload);
                Ok(json!({}))
            }
            // The transcript panel's view hold — TEST-MODE ONLY, see
            // [`hold_view_door`].
            "ui.hold_lane_view" => {
                let payload = hold_view_door(&self.env, params)?;
                let _ = self.app.emit("hold-lane-view", payload);
                Ok(json!({}))
            }
            "app.screenshot" => self.screenshot().await,
            // The tray's Quit, drivable (plan 026 §3.3) — TEST-MODE ONLY, see
            // [`app_quit_door`]. The answer goes out while the orderly
            // shutdown runs; the app is gone once it finishes.
            "app.quit" => {
                app_quit_door(&self.env)?;
                self.app.exit(0);
                Ok(json!({}))
            }
            // The macOS system quit, drivable (plan 026 AM7): TEST-MODE ONLY
            // and macOS only, see [`app_terminate_door`]. It reaches the
            // orderly shutdown through `RunEvent::Exit` alone. As with
            // `app.quit`, the answer goes out first: the terminate is only
            // queued here, see [`system_terminate`].
            "app.terminate" => {
                app_terminate_door(&self.env)?;
                system_terminate(&self.app)?;
                Ok(json!({}))
            }
            "sheds.list" => {
                let reachability = refresh_hosts(&self.backend, self.runtime.hosts()).await;
                self.observe_egress(&reachability);
                Ok(sheds_payload(&reachability))
            }
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
            // A shed's egress lives on its detail (plan 039 B4): `egress.show` opens
            // that detail exactly as `ui.show_shed_delivery` does, and
            // `shed_egress.dump` reads what its egress section rendered.
            "egress.show" => {
                let (event, payload) =
                    crate::delivery::ui_door(self.env.test_mode, "ui.show_shed_delivery", params)?;
                present_main_window(&self.app);
                let _ = self.app.emit(event, payload);
                Ok(json!({}))
            }
            "shed_egress.dump" => {
                test_mode_door(&self.env, op)?;
                Ok(json!({
                    "detail": self.ui_get("shed_egress").unwrap_or(Value::Null),
                }))
            }
            // -- the egress decision feed (plan 039 PR B; `crate::egress`) --
            "egress.recent" => {
                let feed = self.egress_feed()?;
                crate::egress::recent(
                    &feed,
                    params.get("server").and_then(Value::as_str).unwrap_or(""),
                    params.get("shed").and_then(Value::as_str).unwrap_or(""),
                    params
                        .get("created_at")
                        .and_then(Value::as_str)
                        .unwrap_or(""),
                    params
                        .get("blocks")
                        .and_then(Value::as_bool)
                        .unwrap_or(false),
                )
            }
            "egress.status" => {
                let feed = self.egress_feed()?;
                Ok(crate::egress::status(&feed))
            }
            "egress.server_profiles" => {
                Ok(json!({ "hosts": self.backend.egress_profiles().await }))
            }
            "terminal.preview" => self.terminal_preview(params),
            "terminal.open" => self.terminal_open(params),
            "terminal.presets" => Ok(self.terminal_presets()),
            "rc.list" => self.rc_list(params).await,
            "rc.inject_test" => self.rc_inject_test(params),
            "sidebar.dump" => Ok(self.sidebar_dump()),
            "shed_roost.dump" => Ok(self.shed_roost_dump()),
            "roost_consent.dump" => Ok(self.roost_consent_dump()),
            "toast.dump" => Ok(self.toast_dump()),
            // -- roost hosts: probe, preview, bootstrap (plan 019 §3.6) --
            "roost.probe" => self.roost_probe(params).await,
            "roost.preview" => self.roost_preview(params).await,
            "roost.bootstrap" => self.roost_bootstrap(params).await,
            // -- agent lanes (plan 015 §3.4) --
            "lane.open" => self.lane_open(params).await,
            "lane.messages" => self.lane_messages(params),
            "lane.approvals" => self.lane_approvals(params),
            "lane.send" => self.lane_send(params).await,
            "lane.cancel" => self.lane_cancel(params).await,
            "lane.answer" => self.lane_answer(params).await,
            "lane.close" => self.lane_close(params),
            "lane.stop" => self.lane_stop(params).await,
            "lane.settings" => self.lane_settings(params).await,
            "lane.set" => self.lane_set(params).await,
            "lane.set_title" => self.lane_set_title(params).await,
            "lane.dump" => Ok(self.lane_dump()),
            "lane_settings.dump" => Ok(self.lane_settings_dump()),
            "craze.create_options" => self.craze_create_options(params).await,
            "craze.create" => self.craze_create(params).await,
            "craze.saved" => self.craze_saved(params).await,
            "craze.reopen" => self.craze_reopen(params).await,
            "craze.open_terminal" => self.craze_open_terminal(params).await,
            "craze_create.dump" => Ok(self.craze_create_dump()),
            "agents.dump" => Ok(self.agents_dump()),
            "prefs.get" => Ok(self.prefs_get()),
            "prefs.set_terminal" => self.prefs_set_terminal(params),
            // The Preferences window's drivable surface: its merged snapshot +
            // native state, and hide (close-hides contract).
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
            "loginitem.status" => {
                Ok(json!({ "enabled": crate::login_item_enabled(&self.app, &self.env) }))
            }
            "loginitem.set" => self.login_item_set(params),
            // -- credential sources (plan 035 §3.4) --
            "source.status" => {
                let rt = self.delivery_rt().ok();
                let servers = self.resolver().now().await;
                Ok(crate::source::status(&servers, rt.as_deref()).await)
            }
            "source.use" => self.source_use(params).await,
            // -- the Credentials view's Rust side (plan 040 §3.9) --
            "sources.list" => {
                let rt = self.delivery_rt().ok();
                let servers = self.resolver().now().await;
                Ok(crate::source::list(&servers, rt.as_deref()).await)
            }
            "source.unbind" => self.source_unbind(params).await,
            "delivery.describe" => Ok(crate::delivery::describe()),
            "delivery.recent" => Ok(crate::delivery::recent(&*self.delivery_rt()?)),
            // -- provisioning by delivery (plan 039 A6; `crate::delivery`) --
            "delivery.status" => Ok(self.delivery_rt()?.status()),
            "delivery.settings" => Ok(self.delivery_rt()?.settings()),
            "delivery.set" => {
                let provider = req_str(params, "provider")?;
                let config = params
                    .get("config")
                    .ok_or_else(|| err("bad_request", "missing 'config' (null removes)"))?;
                crate::delivery::set(&*self.delivery_rt()?, provider, config)
            }
            "delivery.preview" => {
                let server = req_str(params, "server")?;
                let p = crate::delivery::PreviewParams {
                    shed: opt_str(params, "shed")?,
                    backend: opt_str(params, "backend")?,
                    repo: opt_str(params, "repo")?,
                    expect_runs: match params.get("runs") {
                        None | Some(Value::Null) => None,
                        Some(v) => {
                            Some(serde_json::from_value::<Vec<String>>(v.clone()).map_err(
                                |_| err("bad_request", "'runs' must be a list of strings"),
                            )?)
                        }
                    },
                };
                crate::delivery::preview(&*self.delivery_rt()?, &self.resolver(), server, p).await
            }
            "shed.delivery" => {
                let (server, shed) = (req_str(params, "server")?, req_str(params, "shed")?);
                crate::delivery::shed_delivery(&self.backend, server, shed).await
            }
            "shed.provision" => {
                let (server, shed) = (req_str(params, "server")?, req_str(params, "shed")?);
                crate::delivery::shed_provision(&self.backend, server, shed).await
            }
            "activity.server_list" => {
                let limit = match params.get("limit") {
                    None | Some(Value::Null) => None,
                    Some(v) => Some(
                        v.as_u64()
                            .ok_or_else(|| err("bad_request", "'limit' must be an integer"))?,
                    ),
                };
                let cursor = opt_str(params, "cursor")?;
                let shed = opt_str(params, "shed")?;
                crate::delivery::activity(&self.backend, limit, cursor, shed).await
            }
            // The provisioning UI's doors (plan 039 A7; the Credentials view's since plan
            // 040 §3.9) — see [`crate::delivery::ui_door`] for which are TEST-MODE ONLY.
            "ui.show_shed_delivery"
            | "ui.close_shed_delivery"
            | "ui.press_deliver_again"
            | "ui.press_delivery_button"
            | "ui.type_delivery_field"
            | "ui.fill_check"
            | "ui.press_check" => {
                let (event, payload) = crate::delivery::ui_door(self.env.test_mode, op, params)?;
                if op == "ui.show_shed_delivery" {
                    present_main_window(&self.app);
                }
                let _ = self.app.emit(event, payload);
                Ok(json!({}))
            }
            // Bring the element `[data-scroll-anchor=<anchor>]` into view — TEST-MODE
            // ONLY: a screenshot's way to what is below the fold. Answers the token the
            // dashboard reports back through `ui.scrolled` once it has scrolled.
            "ui.scroll_to" => {
                test_mode_door(&self.env, op)?;
                let anchor = req_str(params, "anchor")?;
                if anchor.is_empty() || anchor.len() > 256 {
                    return Err(err("bad_request", "'anchor' names a section"));
                }
                static SCROLLS: std::sync::atomic::AtomicU64 = std::sync::atomic::AtomicU64::new(0);
                let token = SCROLLS.fetch_add(1, Ordering::SeqCst) + 1;
                let _ = self
                    .app
                    .emit("scroll-to", json!({ "anchor": anchor, "token": token }));
                Ok(json!({ "token": token }))
            }
            // The last `ui.scroll_to` the dashboard carried out: `{anchor, token,
            // found}`, or null before one.
            "ui.scrolled" => {
                test_mode_door(&self.env, op)?;
                Ok(json!({
                    "scrolled": self.ui_get("scrolled").unwrap_or(Value::Null),
                }))
            }
            // What the Activity pane rendered (plan 039 PR B): `null` off the pane.
            "activity.dump" => {
                test_mode_door(&self.env, op)?;
                let on_pane = self
                    .ui_get("pane")
                    .and_then(|p| p.as_str().map(|s| s == "activity"))
                    .unwrap_or(false);
                let activity = if on_pane {
                    self.ui_get("activity").unwrap_or(Value::Null)
                } else {
                    Value::Null
                };
                Ok(json!({ "activity": activity }))
            }
            // The Activity pane's "Load older" press — TEST-MODE ONLY, the
            // `ui.press_source_use` rule: the pane clicks its own button, so a
            // hidden or disabled one takes no press.
            "ui.press_activity_older" => {
                test_mode_door(&self.env, op)?;
                let _ = self.app.emit("press-activity-older", json!({}));
                Ok(json!({}))
            }
            // What the open shed delivery detail rendered (plan 039 A7), or null.
            "shed_delivery.dump" => Ok(json!({
                "detail": self.ui_get("shed_delivery").unwrap_or(Value::Null),
            })),
            // The Credentials view's "Answer from here" and "Stop answering from here"
            // presses — TEST-MODE ONLY, see [`press_source_door`]. Each acts on the view
            // when it is shown; a missing or disabled button takes no press.
            "ui.press_source_use" | "ui.press_source_unbind" => {
                let payload = press_source_door(&self.env, op, params)?;
                let event = if op == "ui.press_source_use" {
                    "press-source-use"
                } else {
                    "press-source-unbind"
                };
                let _ = self.app.emit(event, payload);
                Ok(json!({}))
            }
            // What the Credentials view rendered (plan 040 §3.9): its four parts, read
            // back off its own nodes; `null` off the view.
            "credentials.dump" => {
                test_mode_door(&self.env, op)?;
                let on_pane =
                    self.ui_get("pane").as_ref().and_then(Value::as_str) == Some("credentials");
                let credentials = if on_pane {
                    self.ui_get("credentials").unwrap_or(Value::Null)
                } else {
                    Value::Null
                };
                Ok(json!({ "credentials": credentials }))
            }
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
            "sheds" | "agents" | "credentials" | "activity" | "system"
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
            let reachability = refresh_hosts(&self.backend, self.runtime.hosts()).await;
            self.observe_egress(&reachability);
            return Ok(sheds_payload(&reachability));
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
        let r = self
            .terminal
            .prefs_set_terminal(req_str(params, "preset")?, template)?;
        self.emit_prefs_changed();
        Ok(r)
    }

    // -- RC / Agents (B2.3) — the launcher + session table -------------------

    // The pure pane-classifier op that lived here — `{kind, pane}` → `{state,
    // url?}` — went with S2 (`charliek/shed#324`). A shed row's `state` comes off
    // the wire from the guest (where it is liveness now) and a machine row's from
    // roost; no client re-derives one from a pane.

    /// `rc.list {host?, shed?}` → `{sessions, capabilities, hosts, shed_craze}`.
    /// Which sheds are running comes from `Backend`; the rows come from each
    /// shed's own `roost-session`.
    async fn rc_list(&self, params: &Value) -> Result<Value, (String, String)> {
        let host = params.get("host").and_then(Value::as_str);
        let shed = params.get("shed").and_then(Value::as_str);
        Ok(
            shed_desktop_core::rc_list_payload(&self.backend, self.runtime.hosts(), host, shed)
                .await,
        )
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

    /// `shed_roost.dump` → what the SHEDS PANE rendered about each shed's own
    /// roost-session (the status line + plan-matrix button), on the
    /// `machines.dump` rule: UI truth, `null` off the Sheds pane rather than a
    /// stale snapshot from the last time it was mounted.
    fn shed_roost_dump(&self) -> Value {
        let on_pane = self
            .ui_get("pane")
            .and_then(|p| p.as_str().map(|s| s == "sheds"))
            .unwrap_or(false);
        let rows = if on_pane {
            self.ui_get("shed_roost").unwrap_or(Value::Null)
        } else {
            Value::Null
        };
        json!({ "sheds": rows })
    }

    /// `roost_consent.dump` → the bootstrap consent dialog's own rendered copy
    /// (what/where/from-where/hooks/backup, the plan, the fingerprint), or
    /// `null` while none is mounted. UI truth, same rule as `machines.dump`: the
    /// dialog reports itself, this only relays the last report.
    fn roost_consent_dump(&self) -> Value {
        // Wrapped under a named key, like `machines.dump`/`lane.dump` — a bare
        // `null` RESULT is indistinguishable from "no result" once the socket
        // client's `resp.get("result") or {}` runs, which would turn a closed
        // dialog into `{}` instead of the `None` a cell checks for.
        json!({ "consent": self.ui_get("roost_consent").unwrap_or(Value::Null) })
    }

    /// `toast.dump` → the last (or currently shown) toast the shell reported —
    /// the PATH warning / hook-wiring summary a bootstrap ends with, or an error.
    /// `null` before any has been shown.
    fn toast_dump(&self) -> Value {
        json!({ "toast": self.ui_get("toast").unwrap_or(Value::Null) })
    }

    /// `roost.probe {target}` → what is on that shed, and whether anything is
    /// serving. Read-only: it writes nothing and is safe to run before consent.
    async fn roost_probe(&self, params: &Value) -> Result<Value, (String, String)> {
        let target = parse_shed(req_str(params, "target")?)?;
        self.runtime
            .hosts()
            .probe(&target)
            .await
            .map_err(|failure| err(failure.stage.as_str(), failure.message))
    }

    /// `roost.preview {target}` → the probe, the plan matrix row, and the
    /// sentence naming where the bytes would come from — the consent card's
    /// content.
    ///
    /// Nothing is resolved or fetched (plan 019 §3.5).
    async fn roost_preview(&self, params: &Value) -> Result<Value, (String, String)> {
        let target = parse_shed(req_str(params, "target")?)?;
        self.runtime
            .hosts()
            .preview(&target)
            .await
            .map_err(|failure| err(failure.stage.as_str(), failure.message))
    }

    /// `roost.bootstrap {target, fingerprint, consent}` → install/update/start a
    /// `roost-session` on that host and wire its agent hooks.
    ///
    /// ## Two different refusals, answered two different ways
    ///
    /// **No `consent: true`** is a CALLER bug — the client is supposed to have
    /// shown the card — so it is an error envelope (`consent_required`) and the
    /// host is never touched, not even probed.
    ///
    /// **A stale fingerprint** is a fact about the HOST: it changed between the
    /// card and the click. That is an `ok` envelope carrying `ok: false` and
    /// `error.stage = "fingerprint"`, like every other stage failure, because
    /// the stage is what tells a user whether anything was written and what to
    /// do next — and flattening "the host moved" and "the commit failed after
    /// the backup was discarded" into one error string would lose exactly that.
    ///
    /// ## What `consent: true` does and does not prove
    ///
    /// It is a CALLER-SUPPLIED boolean, and that is the pinned wire shape (plan
    /// 019 §3.6 pins `roost.bootstrap {target, fingerprint, consent: true}`). So
    /// read it for exactly what it is: the caller's assertion that it showed the
    /// §3.5 consent card and a person confirmed it. The backend keeps no record
    /// that a card was ever rendered, so nothing here can tell a confirmed card
    /// from a `roost.preview` followed immediately by `consent: true` — and the
    /// frontend twin (`roost_bootstrap` in `lib.rs`) is reachable from any code
    /// running in the webview, which loads only bundled local assets. That
    /// webview is the trust boundary, and it is where the claim is honoured.
    ///
    /// Making consent UNFORGEABLE would mean the backend minting a token at
    /// preview time (beside the fingerprint, bound to it) that `bootstrap`
    /// required and consumed — a deliberate follow-up with a wire change in it,
    /// not a gap nobody noticed. What IS enforced here is the other half of the
    /// promise: the fingerprint must still match, so a caller that skipped the
    /// card still cannot act on a host that has changed since it looked.
    ///
    /// The 10-minute budget is the op's, not a step's: the machines bound each
    /// remote step themselves (roost's own numbers), and this is the outer
    /// guarantee that a wedged transport cannot hold the socket open forever.
    async fn roost_bootstrap(&self, params: &Value) -> Result<Value, (String, String)> {
        let target = parse_shed(req_str(params, "target")?)?;
        let fingerprint = req_str(params, "fingerprint")?.to_string();
        let consent = params
            .get("consent")
            .and_then(Value::as_bool)
            .unwrap_or(false);
        let run = self
            .runtime
            .hosts()
            .bootstrap(&target, &fingerprint, consent);
        let outcome = match tokio::time::timeout(BOOTSTRAP_BUDGET, run).await {
            Ok(outcome) => outcome,
            Err(_) => {
                return Err(err(
                    "action_failed",
                    format!(
                        "{target}: the bootstrap did not finish within {} minutes",
                        BOOTSTRAP_BUDGET.as_secs() / 60
                    ),
                ))
            }
        };
        match outcome {
            Ok(Ok(installed)) => Ok(installed_json(&installed)),
            Ok(Err(failure)) => Ok(failure_json(&failure)),
            // A [`Refusal`] is about the CALL, not the host: no consent, an
            // unaddressable target, or a bootstrap for this target already
            // running. Its own code, so a driver can tell them apart.
            Err(refusal) => {
                let code = refusal.code();
                Err(err(code, refusal.message()))
            }
        }
    }

    // -- agent lanes (plan 015 §3.4) ------------------------------------------
    //
    // Every op is addressed by `(machine, session_id)` — the machine name the
    // row is stamped with, and the AGENT's own session id from its `agent_lane`
    // stamp. That id is not the roost tab id (`slug`): a lane talks to the
    // agent's HTTP server, which has never heard of roost.
    //
    // Failures answer the same `{code, message}` envelope everything else does,
    // with the lane contract's own snake_case codes (`unknown_session`,
    // `already_resolved`, `unauthorized`, …) plus `no_lane` for a row that has
    // no transcript to show. See [`LaneFailure`].

    /// `lane.open {machine, kind, session_id}` → `{session}`.
    ///
    /// The session row alone: what the session can do rides its stream and is
    /// answered by `lane.messages` (plan 025 §3.2.6).
    ///
    /// Idempotent: a second call for an already-open lane re-answers from the
    /// entry rather than opening a second subscription, and two concurrent calls
    /// build ONE.
    async fn lane_open(&self, params: &Value) -> Result<Value, (String, String)> {
        let (target, session_id) = lane_target(params)?;
        self.runtime
            .lanes()
            .open(&target, &session_id)
            .await
            .map_err(lane_err)
    }

    /// `lane.messages {machine, kind, session_id}` → `{messages, activity,
    /// generation, stale, ended, finished, capabilities, settings,
    /// slash_menu}` — the staged-then-swapped view, never a half-seeded one,
    /// and the one place a client reads what the session can do (its `/` menu
    /// included, resolved); `finished` is the lane over for good (plan 032).
    fn lane_messages(&self, params: &Value) -> Result<Value, (String, String)> {
        let (target, session_id) = lane_target(params)?;
        self.runtime
            .lanes()
            .messages(&target, &session_id)
            .map_err(lane_err)
    }

    /// `lane.approvals {machine, kind, session_id}` → `{approvals}` — what is
    /// blocking on the human, this session's and its descendants'.
    fn lane_approvals(&self, params: &Value) -> Result<Value, (String, String)> {
        let (target, session_id) = lane_target(params)?;
        self.runtime
            .lanes()
            .approvals(&target, &session_id)
            .map_err(lane_err)
    }

    /// `lane.send {machine, kind, session_id, text, mode?}` → `{}`.
    async fn lane_send(&self, params: &Value) -> Result<Value, (String, String)> {
        let (target, session_id) = lane_target(params)?;
        let text = req_str(params, "text")?.to_string();
        let mode = shed_desktop_core::parse_mode(params.get("mode").and_then(Value::as_str))
            .map_err(lane_err)?;
        self.runtime
            .lanes()
            .send(&target, &session_id, &text, mode)
            .await
            .map_err(lane_err)
    }

    /// `lane.cancel {machine, kind, session_id}` → `{}`.
    async fn lane_cancel(&self, params: &Value) -> Result<Value, (String, String)> {
        let (target, session_id) = lane_target(params)?;
        self.runtime
            .lanes()
            .cancel(&target, &session_id)
            .await
            .map_err(lane_err)
    }

    /// `lane.stop {machine, kind, session_id}` → `{}` — end the SESSION (plan
    /// 025 §3.6.4): answered on craze's receipt; the lane ends, and the row
    /// leaves, when the session's close arrives. Refused by a session whose
    /// capabilities say `stop: false`.
    async fn lane_stop(&self, params: &Value) -> Result<Value, (String, String)> {
        let (target, session_id) = lane_target(params)?;
        self.runtime
            .lanes()
            .stop(&target, &session_id)
            .await
            .map_err(lane_err)
    }

    /// `lane.settings {machine, kind, session_id}` → `{settings}` — the
    /// session's settings, read now (plan 025 §3.10): the model and models
    /// (craze's order), the mode and modes, the current model's options (in
    /// the order a client shows them) and the usage. `lane.messages`'
    /// `settings` is the same thing as the stream last said it.
    async fn lane_settings(&self, params: &Value) -> Result<Value, (String, String)> {
        let (target, session_id) = lane_target(params)?;
        self.runtime
            .lanes()
            .settings(&target, &session_id)
            .await
            .map_err(lane_err)
    }

    /// `lane.set {machine, kind, session_id, change}` → `{}` — change one
    /// setting (plan 025 §3.10). `change` is `{kind: "model"|"mode", id}` or
    /// `{kind: "config", id, value}` ([`shed_desktop_core::parse_setting`]); the new
    /// value arrives on the stream. `not_accepting` is craze's `stale_model`
    /// among others (the session left the model an option was chosen for),
    /// and `outcome_unknown` an answer lost to a drop — never resent.
    async fn lane_set(&self, params: &Value) -> Result<Value, (String, String)> {
        let (target, session_id) = lane_target(params)?;
        let change = params
            .get("change")
            .ok_or_else(|| err("bad_request", "missing 'change'"))?;
        let change = shed_desktop_core::parse_setting(change).map_err(lane_err)?;
        self.runtime
            .lanes()
            .set(&target, &session_id, change)
            .await
            .map_err(lane_err)
    }

    /// `lane.set_title {machine, kind, session_id, title}` → `{}`, or
    /// `{warning}` — rename the session (plan 029 D4/D12, craze's
    /// `session.setTitle`). The title is trimmed and a blank one is
    /// `bad_request`, unsent. The new title arrives on the stream (the
    /// session row, `lane.messages`' `session.title`). `{warning}` is a rename
    /// that TOOK and craze could not record (`index_write`): a success, never
    /// a failure; an answer lost to a drop is `outcome_unknown`, never resent.
    async fn lane_set_title(&self, params: &Value) -> Result<Value, (String, String)> {
        let (target, session_id) = lane_target(params)?;
        let title = req_str(params, "title")?.to_string();
        self.runtime
            .lanes()
            .set_title(&target, &session_id, &title)
            .await
            .map_err(lane_err)
    }

    /// `lane.answer {machine, kind, session_id, approval_id, answer}` → `{}`.
    ///
    /// `answer` is one of `{choice: "<option id>"}` (the offered option, by its
    /// own id — what the panel sends), `{permission:
    /// "allow-once"|"allow-always"|"reject"}` (by semantic kind),
    /// `{question: [[…]]}` — optionally with `custom_text: [string|null]`, the
    /// free text per question — or `{reject: true}`. See
    /// [`shed_desktop_core::parse_answer`].
    async fn lane_answer(&self, params: &Value) -> Result<Value, (String, String)> {
        let (target, session_id) = lane_target(params)?;
        let approval_id = req_str(params, "approval_id")?.to_string();
        let answer = params
            .get("answer")
            .ok_or_else(|| err("bad_request", "missing 'answer'"))?;
        let answer = shed_desktop_core::parse_answer(answer).map_err(lane_err)?;
        self.runtime
            .lanes()
            .answer(&target, &session_id, &approval_id, answer)
            .await
            .map_err(lane_err)
    }

    /// `lane.close {machine, kind, session_id}` → `{}`. Idempotent.
    fn lane_close(&self, params: &Value) -> Result<Value, (String, String)> {
        let (target, session_id) = lane_target(params)?;
        Ok(self.runtime.lanes().close(&target, &session_id))
    }

    /// `lane.dump` → what the transcript PANEL rendered (UI truth, like
    /// `agents.dump`), `null` once it unmounts.
    ///
    /// Deliberately not gated on the current pane, unlike `agents.dump`: the
    /// panel is an overlay that can be open on top of any pane, and the frontend
    /// clears the key on unmount — so absence here means "no panel", which is
    /// exactly the question a caller is asking.
    fn lane_dump(&self) -> Value {
        json!({ "lane": lane_report(self.ui_get("lane"), self.ui_get("lane_composer")) })
    }

    /// `lane_settings.dump` → what the settings SHEET rendered (UI truth, the
    /// `lane.dump` rule): its session, its rows — the model list, each
    /// option, the mode — with their control, values, current value and the
    /// row's state (`pending`, `refused` with its inline text, `not_confirmed`),
    /// and the context meter — or `null` while none is open.
    fn lane_settings_dump(&self) -> Value {
        json!({ "lane_settings": self.ui_get("lane_settings").unwrap_or(Value::Null) })
    }

    // -- craze: the create sheet and Open in terminal (plan 025 §3.6.6, §3.8) --
    //
    // Addressed by `target` (`roost:<server>/<shed>`). Refusals carry
    // [`shed_desktop_core::CrazeFailure`]'s codes: the lane contract's, plus
    // `outcome_unknown` (the one a caller keeps its request id on), `too_old`,
    // `not_installed` and `no_craze` — and a reopen's own `gone` and
    // `start_failed` (plan 032 §3.4).

    /// `craze.create_options {machine}` → `{machine, options: {providers,
    /// default_provider?, recent_dirs, reopen}, source_gen}` — what a create
    /// can start there, read afresh (D8), and the incarnation of the machine's
    /// craze source it came through (plan 032 §3.4). May start a hub on a
    /// dormant machine: an explicit action.
    async fn craze_create_options(&self, params: &Value) -> Result<Value, (String, String)> {
        let target = parse_shed(req_str(params, "target")?)?;
        self.runtime
            .hosts()
            .craze_create_options(&target)
            .await
            .map_err(craze_err)
    }

    /// `craze.create {machine, cwd, provider?, prompt?, title?, request_id?,
    /// held?}` →
    /// `{session, host_id, prompt, prompt_error?, request_id}`: a new craze
    /// session, its row folded into the machine's listing at once. `cwd` must
    /// be absolute; a blank provider or prompt is absent
    /// ([`shed_desktop_core::create_request`]); `request_id` is the caller's (reused
    /// only to retry an unknown outcome) or minted here — and answered back
    /// either way. `held: true` says the call REPLAYS a request id kept from
    /// an unknown outcome (plan 026 §3.1): only craze's own answer about that
    /// request ends it, so anything else is `outcome_unknown` again.
    async fn craze_create(&self, params: &Value) -> Result<Value, (String, String)> {
        let target = parse_shed(req_str(params, "target")?)?;
        let held = craze_create_held(params)?;
        let request = craze_create_request(params)?;
        self.runtime
            .hosts()
            .craze_create(&target, request, held)
            .await
            .map_err(craze_err)
    }

    /// `craze.saved {machine}` → `{machine, saved: {sessions, more}, home?,
    /// source_gen}` (plan 032 §3.4): the saved sessions a resume can run
    /// again there — each `{id, provider, workspace, title?,
    /// updated_unix_ms, created_unix_ms?}`, newest first, at most 50 (`more`
    /// when craze left some out) — read afresh; `home` is the host's home
    /// directory where the app knows it (`null` otherwise); `source_gen` the
    /// craze source incarnation it came through. May start a hub on a dormant
    /// machine.
    async fn craze_saved(&self, params: &Value) -> Result<Value, (String, String)> {
        let target = parse_shed(req_str(params, "target")?)?;
        self.runtime
            .hosts()
            .craze_saved(&target)
            .await
            .map_err(craze_err)
    }

    /// `craze.reopen {machine, id}` → `{session, host_id, ended, superseded,
    /// joined, id, source_gen}`: the saved session `id` running again (or
    /// joined where something already runs it), its row folded into the
    /// machine's listing at once — unless `superseded`: the machine was
    /// removed (or removed and registered again) while it ran, nothing was
    /// folded, and there is nothing to open on it here.
    /// `id` is TYPED like every craze field and must be non-blank
    /// ([`shed_desktop_core::reopen_id`]). Refusal codes keep the reopen error's
    /// variant ([`shed_desktop_core::CrazeFailure::reopen`]).
    async fn craze_reopen(&self, params: &Value) -> Result<Value, (String, String)> {
        let target = parse_shed(req_str(params, "target")?)?;
        let id = shed_desktop_core::reopen_id(opt_str(params, "id")?).map_err(craze_err)?;
        self.runtime
            .hosts()
            .craze_reopen(&target, &id)
            .await
            .map_err(craze_err)
    }

    /// `craze.open_terminal {machine, session_id}` → `{origin, machine,
    /// session_id, tab_id, cwd, argv}`: a roost tab running `craze attach` on
    /// that session (the row's `slug`, its hostId), in its workspace; the row
    /// shows that tab from now on.
    async fn craze_open_terminal(&self, params: &Value) -> Result<Value, (String, String)> {
        let target = parse_shed(req_str(params, "target")?)?;
        let session_id = req_str(params, "session_id")?.to_string();
        self.runtime
            .hosts()
            .craze_open_terminal(&target, &session_id)
            .await
            .map_err(craze_err)
    }

    /// `craze_create.dump` → what the create SHEET rendered (UI truth, the
    /// `lane.dump` rule): its machine, its state, the providers with their
    /// state, dimming and reason/fix, the selected one, the recent
    /// directories, the typed fields, the request id it holds, the note and
    /// the outcome — and its Resume mode (plan 032 §3.4): which mode it shows,
    /// whether it offers the choice, and the saved sessions it lists with
    /// each row's state — or `null` while none is mounted.
    fn craze_create_dump(&self) -> Value {
        json!({ "craze_create": self.ui_get("craze_create").unwrap_or(Value::Null) })
    }

    /// The roost host a shed-addressed op is about: `roost:<server>/<shed>`.
    ///
    /// `rc.inject_test {target, slug, kind?, display_name?, workdir?,
    /// lifecycle?, attention?}` → put a row into the shed's roost snapshot
    /// (test-only, guarded like the other privileged driver ops).
    ///
    /// It lands in the ROOST snapshot since S6: that is the only row source the
    /// Agents pane has, so the render fixture has to land there. `slug` is the
    /// roost tab id, so it must parse as one — an invented string is refused by
    /// name rather than injected as a row nothing could later close.
    fn rc_inject_test(&self, params: &Value) -> Result<Value, (String, String)> {
        if !self.env.test_mode {
            return Err(err("not_enabled", "rc.inject_test requires test mode"));
        }
        let target = parse_shed(req_str(params, "target")?)?;
        let row = self.build_inject_row(params)?;
        self.runtime
            .hosts()
            .inject_test(&target, row)
            .map_err(|e| err("bad_request", e))?;
        Ok(json!({}))
    }

    /// Build the roost row an inject-test param bag describes, filling the title
    /// and workdir defaults the harness omits.
    fn build_inject_row(&self, params: &Value) -> Result<InjectedRow, (String, String)> {
        let slug = req_str(params, "slug")?;
        let tab_id = slug.parse::<i64>().map_err(|_| {
            err(
                "bad_request",
                format!("slug {slug:?} is not a roost tab id"),
            )
        })?;
        let opt = |k: &str| params.get(k).and_then(Value::as_str).map(str::to_string);
        Ok(InjectedRow {
            tab_id,
            // Defaults like the rest of this op: the harness sends what the cell
            // is about and nothing else.
            kind: match params.get("kind") {
                Some(_) => rc_kind(params)?,
                None => RcKind::ClaudeRc,
            },
            title: opt("display_name").unwrap_or_else(|| slug.to_string()),
            cwd: opt("workdir").unwrap_or_else(|| "/home/shed".to_string()),
            lifecycle: opt("lifecycle"),
            attention: params
                .get("attention")
                .and_then(Value::as_bool)
                .unwrap_or(false),
        })
    }

    /// `agents.dump` → `{sessions, empty}`: the sessions the frontend reported
    /// (UI truth, like `dashboard.dump` reads the reported sheds) so the pane is
    /// drivable by logical content, not just a screenshot — plus, when the list
    /// is empty, the empty state's own words.
    ///
    /// `empty` is how an EMPTY pane is drivable at all. Since S6 an empty list
    /// is a meaningful answer ("this shed has no roost-session"), not a blank
    /// waiting to fill, and a `sessions: []` alone cannot tell a caller which of
    /// the two the pane actually said. `null` whenever rows are rendered.
    fn agents_dump(&self) -> Value {
        // UI truth = what's rendered: the Agents pane only reports its sessions
        // while mounted, so off-pane the `agents` snapshot is stale — report [] unless
        // the UI is actually on the agents pane (like `dashboard.dump` reflects the
        // current sheds, not a stale set).
        let on_agents = self
            .ui_get("pane")
            .and_then(|p| p.as_str().map(|s| s == "agents"))
            .unwrap_or(false);
        let (sessions, empty) = if on_agents {
            (
                self.ui_get("agents").unwrap_or_else(|| json!([])),
                self.ui_get("agents_empty").unwrap_or(Value::Null),
            )
        } else {
            (json!([]), Value::Null)
        };
        json!({ "sessions": sessions, "empty": empty })
    }

    // -- egress profiles: the System pane's UI truth --------------------------

    /// `egress.profiles` → the server-wide egress profiles the System pane rendered
    /// (UI truth, like `agents.dump`): `{profiles, errors}`. The pane reports them only
    /// while mounted, so off System this answers `null` rather than a stale snapshot.
    fn egress_dump(&self) -> Value {
        let on_system = self
            .ui_get("pane")
            .and_then(|p| p.as_str().map(|s| s == "system"))
            .unwrap_or(false);
        let egress = if on_system {
            self.ui_get("egress").unwrap_or(Value::Null)
        } else {
            Value::Null
        };
        json!({ "egress": egress })
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

    /// A listing drops the egress decisions of the sheds it no longer has.
    fn observe_egress(&self, r: &Reachability) {
        if let Ok(feed) = self.egress_feed() {
            feed.observe(r);
        }
    }

    /// The egress decision feed (plan 039 PR B), or `not_ready` before setup has made it.
    fn egress_feed(&self) -> Result<Arc<shed_desktop_core::egress::EgressFeed>, (String, String)> {
        self.app
            .try_state::<Arc<shed_desktop_core::egress::EgressFeed>>()
            .map(|s| Arc::clone(&s))
            .ok_or_else(|| err("not_ready", "the egress feed has not started"))
    }

    /// The source runtime (plan 039), or `not_ready` before setup has made it.
    fn delivery_rt(
        &self,
    ) -> Result<Arc<shed_desktop_core::delivery::DeliveryRuntime>, (String, String)> {
        self.app
            .try_state::<Arc<shed_desktop_core::delivery::DeliveryRuntime>>()
            .map(|s| Arc::clone(&s))
            .ok_or_else(|| err("not_ready", "the delivery runtime has not started"))
    }

    /// `source.use {host}` → make THIS machine the credential source on `host` (the
    /// same path as the Preferences "Use this machine" button), reopen its delivery
    /// stream there, and tell an open Preferences window to read the rows again. See
    /// [`crate::source::use_this_machine`] for the refusals.
    async fn source_use(&self, params: &Value) -> Result<Value, (String, String)> {
        let host = req_str(params, "host")?;
        let rt = self.delivery_rt().ok();
        let res = crate::source::use_this_machine(&self.resolver(), rt.as_deref(), host).await;
        if res.is_ok() {
            self.emit_prefs_changed();
        }
        res
    }

    /// How the source doors resolve the servers ([`crate::source::Resolver`]).
    fn resolver(&self) -> crate::source::Resolver {
        let current = self
            .app
            .try_state::<Arc<crate::source::CurrentServers>>()
            .map(|s| s.inner().clone());
        crate::source::Resolver::new(current, self.backend.clone())
    }

    /// `source.unbind {host}` → clear this user's credential source on `host` ("Stop
    /// answering from here"), and tell an open Preferences window to read the rows
    /// again. See [`crate::source::unbind`] for the answer and the refusals.
    async fn source_unbind(&self, params: &Value) -> Result<Value, (String, String)> {
        let host = req_str(params, "host")?;
        let rt = self.delivery_rt().ok();
        let res = crate::source::unbind(&self.resolver(), rt.as_deref(), host).await;
        if res.is_ok() {
            self.emit_prefs_changed();
        }
        res
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
                    app_error!("shed-desktop-tauri: ipc accept error: {e}");
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

    // -- the two lane doors ---------------------------------------------------

    /// `bridge.ts`'s `laneFailure`, transcribed: split on the FIRST `": "`, and
    /// take the prefix as the code only if it looks like one (`LANE_CODE`,
    /// `^[a-z][a-z_]*$`); anything else means the whole string is the message.
    ///
    /// It is duplicated here rather than described, because what these cells are
    /// about is whether the two doors survive THIS rule — a rule the frontend
    /// applies to the command door's bare string and the socket door never needs.
    fn recover(raw: &str) -> (String, String) {
        match raw.split_once(": ") {
            Some((code, message))
                if code.starts_with(|c: char| c.is_ascii_lowercase())
                    && code.chars().all(|c| c.is_ascii_lowercase() || c == '_') =>
            {
                (code.to_string(), message.to_string())
            }
            _ => ("failed".to_string(), raw.to_string()),
        }
    }

    /// **Review finding: the two IPC doors disagreed on the same input.** A
    /// `lane.*` refusal reaches the socket as `{code, message}` and reaches the
    /// `#[tauri::command]` twin as `code + ": " + message`. The grammar refusals
    /// (`parse_mode`, `parse_answer`) used to skip that join on the command door
    /// — the same malformed answer was `bad_request` through the socket and an
    /// uncoded `failed` through the command.
    #[test]
    fn both_lane_doors_answer_a_malformed_input_with_the_same_code() {
        let bad_answers = [
            json!({"choice": ""}),
            json!({"choice": 3}),
            json!({"choice": "p-1", "permission": "allow-once"}),
            json!({"question": "yes"}),
            json!({"question": [["yes"], "no"]}),
            json!({"permission": "maybe"}),
            json!({"permission": 3}),
            json!({"reject": false}),
            json!({}),
            json!({"permission": "reject", "question": [["yes"]]}),
            // `custom_text` in every invalid combination (plan 018 §3.2): the
            // three forms it does not modify, a shape that is not a list of
            // strings-or-null, and the key on its own.
            json!({"choice": "p-1", "custom_text": ["typed"]}),
            json!({"permission": "allow-once", "custom_text": ["typed"]}),
            json!({"reject": true, "custom_text": ["typed"]}),
            json!({"custom_text": ["typed"]}),
            json!({"question": [["yes"]], "custom_text": "typed"}),
            json!({"question": [["yes"]], "custom_text": [3]}),
        ];
        for bad in bad_answers {
            let socket = lane_err(
                shed_desktop_core::parse_answer(&bad).expect_err("this answer is malformed"),
            );
            let command = recover(&crate::lane_error(
                shed_desktop_core::parse_answer(&bad).expect_err("this answer is malformed"),
            ));
            assert_eq!(
                socket.0, "bad_request",
                "the socket door lost the code on {bad}"
            );
            assert_eq!(
                command, socket,
                "the two doors disagree on {bad}: command={command:?} socket={socket:?}"
            );
        }

        for bad in ["later", "  interject please", "QUEUE"] {
            let socket =
                lane_err(shed_desktop_core::parse_mode(Some(bad)).expect_err("a bad mode"));
            let command = recover(&crate::lane_error(
                shed_desktop_core::parse_mode(Some(bad)).expect_err("a bad mode"),
            ));
            assert_eq!(socket.0, "bad_request");
            assert_eq!(command, socket, "the two doors disagree on mode {bad:?}");
        }
    }

    /// The other half of the same finding: the code the frontend recovers is
    /// always OURS. A refusal whose MESSAGE happens to be shaped like a coded
    /// failure — `parse_answer`'s `question` refusal embeds a serde error and
    /// therefore a `": "` — must not have a code read out of it.
    #[test]
    fn a_lane_message_that_looks_coded_does_not_mint_a_code() {
        for message in [
            "already_resolved: someone else answered it",
            "`question` must be a list of lists of option ids: invalid type:              string \"yes\", expected a sequence",
            "no colon here at all",
        ] {
            let raw = crate::lane_error(LaneFailure::bad_request(message.to_string()));
            assert_eq!(
                recover(&raw),
                ("bad_request".to_string(), message.to_string()),
                "a parse message minted its own code out of {message:?}"
            );
        }
    }

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
            delivery: Default::default(),
        }
    }

    #[test]
    fn sheds_payload_carries_host_errors_for_two_failed_hosts() {
        // The shape shed#300 needs: two failed hosts, BOTH present — the singular
        // shape couldn't represent two failed hosts (plan 006 D6).
        let r = reachability(
            &[("mock", "alpha")],
            &[
                ("mini2", ShedError::Transport("connection refused".into())),
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
        assert_eq!(errs[0]["kind"], "other");
        assert!(
            errs[0]["summary"].as_str().unwrap().starts_with("mini2: "),
            "the summary must name its host: {}",
            errs[0]["summary"]
        );
        assert!(errs[0]["detail"]
            .as_str()
            .unwrap()
            .contains("connection refused"));
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
    fn sheds_payload_carries_each_sheds_delivery_record() {
        let mut r = reachability(&[("mock", "alpha"), ("mock", "beta")], &[]);
        r.delivery.insert(
            ("mock".to_string(), "alpha".to_string()),
            json!({"state": "partial", "owed": ["install"]}),
        );
        let p = sheds_payload(&r);
        assert_eq!(p["sheds"][0]["delivery"]["state"], "partial");
        assert_eq!(p["sheds"][0]["delivery"]["owed"], json!(["install"]));
        assert_eq!(p["sheds"][1]["delivery"], Value::Null, "no record: null");
        assert_eq!(p["sheds"][1]["name"], "beta");
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
        // The gate `rc.inject_test` applies: serde preserves an unknown kind as
        // Other, and an inject must reject it here.
        assert!(ensure_known_kind(&RcKind::Opencode).is_ok());
        assert!(ensure_known_kind(&RcKind::Other("borg".into())).is_err());
    }

    fn env(mock: Option<&str>) -> Env {
        Env {
            test_mode: true,
            mock_base_url: mock.map(str::to_string),
            mock_unreachable_hosts: std::collections::HashSet::new(),
            roost_sockets: std::collections::HashMap::new(),
            ssh_bin: None,
            roost_jail_fs_root: false,
            config_path: PathBuf::new(),
            socket_path: PathBuf::from("/run/user/0/shed-tauri/shed-tauri.sock"),
            delivery_settings_path: PathBuf::from("/run/user/0/shed/delivery.toml"),
            source_recheck: None,
            log_file: None,
            log_debug: false,
        }
    }

    /// **The consent-dialog driver doors are privileged, and gated like the
    /// other privileged ones** (`rc.inject_test`).
    ///
    /// They exist because the dialog only opens from a card's plan-matrix BUTTON
    /// and the harness has no click. But SHOWING the card and CONFIRMING it are
    /// the two halves of a user's consent, so a socket client that can do both in
    /// succession can start a bootstrap nobody clicked — an install plus a
    /// dotfile-mutating hook wiring, on one of the user's hosts. Outside test
    /// mode all three answer `not_enabled` and emit nothing; the dispatch arm has
    /// no event name unless this function hands it one, which is what keeps the
    /// gate from being true of two doors and forgotten on the third.
    #[test]
    fn the_roost_consent_doors_are_test_mode_only() {
        const DOORS: [&str; 3] = [
            "ui.show_roost_consent",
            "ui.confirm_roost_consent",
            "ui.close_roost_consent",
        ];
        let params = json!({ "target": "roost:mock/alpha" });

        let mut prod = env(None);
        prod.test_mode = false;
        for op in DOORS {
            let (code, message) = roost_consent_door(&prod, op, &params)
                .expect_err("a driver door must not be reachable in a shipped app");
            assert_eq!(code, "not_enabled", "{op}");
            assert!(message.contains(op), "the refusal names the op: {message}");
        }

        // In test mode each one routes to its own event, and the open door
        // carries the target it was asked about.
        let test = env(None);
        assert_eq!(
            roost_consent_door(&test, DOORS[0], &params).unwrap(),
            (
                "show-roost-consent",
                json!({ "target": "roost:mock/alpha" })
            )
        );
        assert_eq!(
            roost_consent_door(&test, DOORS[1], &json!({})).unwrap(),
            ("confirm-roost-consent", json!({}))
        );
        assert_eq!(
            roost_consent_door(&test, DOORS[2], &json!({})).unwrap(),
            ("close-roost-consent", json!({}))
        );
        // A missing `target` is still a bad request, after the gate.
        assert_eq!(
            roost_consent_door(&test, DOORS[0], &json!({}))
                .expect_err("no target")
                .0,
            "bad_request"
        );
    }

    #[test]
    fn identify_reports_tauri_core_and_hermeticity() {
        let v = identify_payload(&env(Some("http://mock")), 4242, None);
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
        let v = identify_payload(&env(None), 1, None);
        assert!(v["mock_base_url"].is_null());
    }

    /// `identify.log_path` is the ACTIVE log file, or `null` when file logging is
    /// off (#414).
    #[test]
    fn identify_reports_the_active_log_file_or_null() {
        let v = identify_payload(&env(None), 1, None);
        assert!(v["log_path"].is_null(), "{v}");
        let path = std::path::Path::new("/home/u/.local/state/x/shed-desktop.log");
        let v = identify_payload(&env(None), 1, Some(path));
        assert_eq!(v["log_path"], "/home/u/.local/state/x/shed-desktop.log");
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

    /// **`craze.create`'s `request_id` is read strictly** (C10 review): only an
    /// ABSENT (or null) id is minted; a present one is used as given or refused
    /// — a non-string, an empty or a malformed one is `bad_request`, because
    /// minting in its place would turn the retry of an unknown outcome into a
    /// second session. Every other field is typed the same way.
    #[test]
    fn craze_create_reads_its_request_id_strictly() {
        let base = json!({ "target": "roost:localhost/dev", "cwd": "/w" });
        let with = |key: &str, v: Value| {
            let mut p = base.clone();
            p[key] = v;
            p
        };
        for bad in [
            json!(7),
            json!(true),
            json!(["shed-1"]),
            json!({}),
            json!(""),
            json!("no spaces!"),
        ] {
            let refused = craze_create_request(&with("request_id", bad.clone()))
                .expect_err("a present, unusable request id is refused");
            assert_eq!(refused.0, "bad_request", "{bad}");
        }
        assert_eq!(
            craze_create_request(&with("request_id", json!("shed-retry-1")))
                .unwrap()
                .request_id,
            "shed-retry-1",
            "a given id is used as given"
        );
        for absent in [base.clone(), with("request_id", Value::Null)] {
            let minted = craze_create_request(&absent).unwrap().request_id;
            assert!(minted.starts_with("shed-"), "{minted}");
        }
        for key in ["cwd", "provider", "prompt", "title"] {
            assert_eq!(
                craze_create_request(&with(key, json!(7))).unwrap_err().0,
                "bad_request",
                "{key}: not a string"
            );
        }
        // The sheet's "Name (optional)" (plan 029 D14): trimmed, and a blank
        // one is absent — never sent empty.
        assert_eq!(
            craze_create_request(&with("title", json!("  Fix the login bug ")))
                .unwrap()
                .title
                .as_deref(),
            Some("Fix the login bug")
        );
        for blank in [json!(""), json!(" \t "), Value::Null] {
            assert_eq!(
                craze_create_request(&with("title", blank.clone()))
                    .unwrap()
                    .title,
                None,
                "{blank}"
            );
        }
    }

    /// `held` (plan 026 §3.1) is typed like every other field — absent or
    /// `null` is a fresh create, a non-bool is `bad_request` — and a held
    /// create needs the request id it replays.
    #[test]
    fn craze_create_reads_held_strictly() {
        let base =
            json!({ "target": "roost:localhost/dev", "cwd": "/w", "request_id": "shed-held-1" });
        let with = |key: &str, v: Value| {
            let mut p = base.clone();
            p[key] = v;
            p
        };
        assert_eq!(craze_create_held(&base), Ok(false));
        assert_eq!(craze_create_held(&with("held", Value::Null)), Ok(false));
        assert_eq!(craze_create_held(&with("held", json!(true))), Ok(true));
        assert_eq!(craze_create_held(&with("held", json!(false))), Ok(false));
        for bad in [json!(1), json!("true"), json!({})] {
            assert_eq!(
                craze_create_held(&with("held", bad.clone())).unwrap_err().0,
                "bad_request",
                "{bad}"
            );
        }
        let mut no_id = with("held", json!(true));
        no_id.as_object_mut().unwrap().remove("request_id");
        assert_eq!(craze_create_held(&no_id).unwrap_err().0, "bad_request");
    }

    /// **The craze create sheet's driver doors are test-mode only** (plan 025
    /// C10), the consent doors' rule; in test mode they route to their events
    /// with only the controls that were named, and a control of the wrong type
    /// is a bad request.
    #[test]
    fn the_craze_create_doors_are_test_mode_only() {
        let fill = json!({ "provider": "grok", "cwd": "/w", "title": " Fix it ", "prompt": "two\nlines",
                           "recent": 1 });
        let mut prod = env(None);
        prod.test_mode = false;
        for op in ["ui.fill_craze_create", "ui.submit_craze_create"] {
            let (code, message) = craze_create_door(&prod, op, &fill)
                .expect_err("a driver door must not be reachable in a shipped app");
            assert_eq!(code, "not_enabled", "{op}");
            assert!(message.contains(op), "{message}");
        }
        let test = env(None);
        assert_eq!(
            craze_create_door(&test, "ui.fill_craze_create", &fill).unwrap(),
            ("fill-craze-create", fill.clone())
        );
        assert_eq!(
            craze_create_door(&test, "ui.fill_craze_create", &json!({ "cwd": "/w" })).unwrap(),
            ("fill-craze-create", json!({ "cwd": "/w" })),
            "nothing invented for the controls not named"
        );
        assert_eq!(
            craze_create_door(&test, "ui.submit_craze_create", &json!({})).unwrap(),
            ("submit-craze-create", json!({}))
        );
        for bad in [
            json!({ "prompt": 7 }),
            json!({ "title": false }),
            json!({ "recent": -1 }),
            json!({ "recent": "0" }),
        ] {
            assert_eq!(
                craze_create_door(&test, "ui.fill_craze_create", &bad)
                    .expect_err("a control of the wrong type")
                    .0,
                "bad_request",
                "{bad}"
            );
        }
    }

    /// **The sheet's Resume doors are test-mode only** (plan 032 §3.4,
    /// PC14i), the create doors' rule: `ui.choose_craze_mode` routes only
    /// `new` or `resume`, and `ui.resume_saved` only a non-blank id, verbatim
    /// (opaque — a legacy `grok:<id>` included); anything else is refused
    /// before it reaches the sheet.
    #[test]
    fn the_craze_resume_doors_are_test_mode_only() {
        let mut prod = env(None);
        prod.test_mode = false;
        for (op, params) in [
            ("ui.choose_craze_mode", json!({ "mode": "resume" })),
            ("ui.resume_saved", json!({ "id": "0192f0aa" })),
        ] {
            let (code, message) = craze_create_door(&prod, op, &params)
                .expect_err("a driver door must not be reachable in a shipped app");
            assert_eq!(code, "not_enabled", "{op}");
            assert!(message.contains(op), "{message}");
        }
        let test = env(None);
        for mode in ["new", "resume"] {
            assert_eq!(
                craze_create_door(&test, "ui.choose_craze_mode", &json!({ "mode": mode })).unwrap(),
                ("choose-craze-mode", json!({ "mode": mode }))
            );
        }
        assert_eq!(
            craze_create_door(&test, "ui.resume_saved", &json!({ "id": "grok:abc" })).unwrap(),
            ("resume-saved", json!({ "id": "grok:abc" }))
        );
        for bad in [json!({}), json!({ "mode": "Resume" }), json!({ "mode": 1 })] {
            assert_eq!(
                craze_create_door(&test, "ui.choose_craze_mode", &bad)
                    .expect_err("not a mode")
                    .0,
                "bad_request",
                "{bad}"
            );
        }
        for bad in [json!({}), json!({ "id": "  " }), json!({ "id": 7 })] {
            assert_eq!(
                craze_create_door(&test, "ui.resume_saved", &bad)
                    .expect_err("not a saved session id")
                    .0,
                "bad_request",
                "{bad}"
            );
        }
    }

    /// **The composer's driver doors are test-mode only** (plan 029 D13), the
    /// craze create doors' rule; in test mode each routes to its event: the
    /// typed text verbatim (an empty one clears), a key from the composer's
    /// closed set with its Shift and IME flags (absent is `false`), a row by
    /// name, the Stop confirm's answer. A key outside the set — a typo — and a wrong type are refused,
    /// so no cell can pass on a key that was never pressed.
    #[test]
    fn the_composer_doors_are_test_mode_only() {
        let mut prod = env(None);
        prod.test_mode = false;
        for (op, params) in [
            ("ui.type_lane_composer", json!({ "text": "/re" })),
            ("ui.key_lane_composer", json!({ "key": "Enter" })),
            ("ui.pick_slash_row", json!({ "name": "rename" })),
            ("ui.answer_lane_stop", json!({ "stop": false })),
        ] {
            let (code, message) = composer_door(&prod, op, &params)
                .expect_err("a driver door must not be reachable in a shipped app");
            assert_eq!(code, "not_enabled", "{op}");
            assert!(message.contains(op), "{message}");
        }

        let test = env(None);
        for text in ["/rename  a  b ", "", "two\nlines"] {
            assert_eq!(
                composer_door(&test, "ui.type_lane_composer", &json!({ "text": text })).unwrap(),
                ("type-lane-composer", json!({ "text": text })),
                "verbatim"
            );
        }
        for key in COMPOSER_KEYS {
            assert_eq!(
                composer_door(&test, "ui.key_lane_composer", &json!({ "key": key })).unwrap(),
                (
                    "key-lane-composer",
                    json!({ "key": key, "shift": false, "composing": false })
                )
            );
        }
        assert_eq!(
            composer_door(
                &test,
                "ui.key_lane_composer",
                &json!({ "key": "Enter", "shift": true, "composing": null })
            )
            .unwrap(),
            (
                "key-lane-composer",
                json!({ "key": "Enter", "shift": true, "composing": false })
            )
        );
        assert_eq!(
            composer_door(
                &test,
                "ui.key_lane_composer",
                &json!({ "key": "Escape", "composing": true })
            )
            .unwrap()
            .1["composing"],
            true
        );
        assert_eq!(
            composer_door(&test, "ui.pick_slash_row", &json!({ "name": "rename" })).unwrap(),
            ("pick-slash-row", json!({ "name": "rename" }))
        );
        for stop in [true, false] {
            assert_eq!(
                composer_door(&test, "ui.answer_lane_stop", &json!({ "stop": stop })).unwrap(),
                ("answer-lane-stop", json!({ "stop": stop }))
            );
        }
        for (op, bad) in [
            ("ui.type_lane_composer", json!({})),
            ("ui.type_lane_composer", json!({ "text": 7 })),
            ("ui.key_lane_composer", json!({})),
            ("ui.key_lane_composer", json!({ "key": "Esc" })),
            ("ui.key_lane_composer", json!({ "key": "enter" })),
            (
                "ui.key_lane_composer",
                json!({ "key": "Enter", "shift": "yes" }),
            ),
            (
                "ui.key_lane_composer",
                json!({ "key": "Enter", "composing": 1 }),
            ),
            ("ui.pick_slash_row", json!({})),
            ("ui.pick_slash_row", json!({ "name": "" })),
            ("ui.pick_slash_row", json!({ "name": ["rename"] })),
            ("ui.answer_lane_stop", json!({})),
            ("ui.answer_lane_stop", json!({ "stop": "yes" })),
        ] {
            assert_eq!(
                composer_door(&test, op, &bad)
                    .expect_err("not a door call")
                    .0,
                "bad_request",
                "{op} {bad}"
            );
        }
    }

    /// **`lane.dump` joins the composer's report into the panel's**: its
    /// fields beside the transcript's, and no panel is `null` even with a
    /// composer report left over.
    #[test]
    fn the_lane_dump_joins_the_composer_report() {
        let lane = json!({ "session_id": "s1", "rows": [] });
        let composer = json!({ "draft": "/re", "slash_menu": null });
        assert_eq!(
            lane_report(Some(lane.clone()), Some(composer.clone())),
            json!({ "session_id": "s1", "rows": [], "draft": "/re", "slash_menu": null })
        );
        assert_eq!(lane_report(Some(lane.clone()), None), lane);
        assert_eq!(lane_report(Some(lane.clone()), Some(Value::Null)), lane);
        assert_eq!(
            lane_report(Some(Value::Null), Some(composer.clone())),
            Value::Null
        );
        assert_eq!(lane_report(None, Some(composer)), Value::Null);
    }

    /// **The settings sheet's press door is test-mode only** (plan 025 C11),
    /// the craze create doors' rule; in test mode it carries the row and the
    /// value verbatim, and anything but two strings (a named row) is a bad
    /// request.
    #[test]
    fn the_settings_press_door_is_test_mode_only() {
        let press = json!({ "row": "effort", "value": " low " });
        let mut prod = env(None);
        prod.test_mode = false;
        let (code, message) = pick_setting_door(&prod, &press)
            .expect_err("a driver door must not be reachable in a shipped app");
        assert_eq!(code, "not_enabled");
        assert!(message.contains("ui.pick_lane_setting"), "{message}");
        let test = env(None);
        assert_eq!(
            pick_setting_door(&test, &press).unwrap(),
            press,
            "ids are opaque: never trimmed"
        );
        for bad in [
            json!({ "row": "effort" }),
            json!({ "value": "low" }),
            json!({ "row": "", "value": "low" }),
            json!({ "row": "effort", "value": 3 }),
        ] {
            assert_eq!(
                pick_setting_door(&test, &bad).expect_err("not a press").0,
                "bad_request",
                "{bad}"
            );
        }
    }

    /// **The Credentials view's source press doors are test-mode only** (plan 035 §3.4,
    /// plan 040 §3.9): a shipped app answers `not_enabled`; in test mode each carries the
    /// named server, and a missing, empty or non-string host is a bad request.
    #[test]
    fn the_source_press_doors_are_test_mode_only() {
        let press = json!({ "host": "mock" });
        let mut prod = env(None);
        prod.test_mode = false;
        for op in ["ui.press_source_use", "ui.press_source_unbind"] {
            let (code, message) = press_source_door(&prod, op, &press)
                .expect_err("a driver door must not be reachable in a shipped app");
            assert_eq!(code, "not_enabled");
            assert!(message.contains(op), "{message}");
            assert_eq!(press_source_door(&env(None), op, &press).unwrap(), press);
            for bad in [json!({}), json!({ "host": "" }), json!({ "host": 3 })] {
                assert_eq!(
                    press_source_door(&env(None), op, &bad)
                        .expect_err("not a press")
                        .0,
                    "bad_request",
                    "{bad}"
                );
            }
        }
    }

    /// **The report history is test-mode only** (plan 026 §3.4): outside test
    /// mode it answers `not_enabled`; in test mode `clear` is optional and must
    /// be a boolean.
    #[test]
    fn the_report_history_door_is_test_mode_only() {
        let mut prod = env(None);
        prod.test_mode = false;
        for params in [json!({}), json!({ "clear": true })] {
            let (code, message) = report_history_door(&prod, &params)
                .expect_err("a driver door must not be reachable in a shipped app");
            assert_eq!(code, "not_enabled", "{params}");
            assert!(message.contains("ui.report_history"), "{message}");
        }
        let test = env(None);
        assert!(!report_history_door(&test, &json!({})).unwrap());
        assert!(!report_history_door(&test, &json!({ "clear": null })).unwrap());
        assert!(!report_history_door(&test, &json!({ "clear": false })).unwrap());
        assert!(report_history_door(&test, &json!({ "clear": true })).unwrap());
        for bad in [json!({ "clear": "yes" }), json!({ "clear": 1 })] {
            assert_eq!(
                report_history_door(&test, &bad)
                    .expect_err("not a clear flag")
                    .0,
                "bad_request",
                "{bad}"
            );
        }
    }

    /// **Plan 039 PR B's harness ops are test-mode only**: outside test mode each
    /// answers `not_enabled` naming itself; in test mode each passes the gate.
    #[test]
    fn the_activity_and_egress_harness_ops_are_test_mode_only() {
        let mut prod = env(None);
        prod.test_mode = false;
        let test = env(None);
        for op in [
            "activity.dump",
            "shed_egress.dump",
            "ui.press_activity_older",
            "ui.scroll_to",
            "ui.scrolled",
        ] {
            let (code, message) = test_mode_door(&prod, op)
                .expect_err("a harness op must not be reachable in a shipped app");
            assert_eq!(code, "not_enabled", "{op}");
            assert!(message.contains(op), "{message}");
            assert!(test_mode_door(&test, op).is_ok(), "{op} in test mode");
        }
    }

    /// **The view hold is test-mode only** (Amendment A13's harness door): in
    /// test mode it carries the boolean, and anything else is a bad request.
    #[test]
    fn the_view_hold_is_test_mode_only() {
        let mut prod = env(None);
        prod.test_mode = false;
        let (code, message) = hold_view_door(&prod, &json!({ "hold": true }))
            .expect_err("a driver door must not be reachable in a shipped app");
        assert_eq!(code, "not_enabled");
        assert!(message.contains("ui.hold_lane_view"), "{message}");
        let test = env(None);
        for hold in [true, false] {
            assert_eq!(
                hold_view_door(&test, &json!({ "hold": hold })).unwrap(),
                json!({ "hold": hold })
            );
        }
        for bad in [json!({}), json!({ "hold": "yes" }), json!({ "hold": 1 })] {
            assert_eq!(
                hold_view_door(&test, &bad).expect_err("not a hold").0,
                "bad_request",
                "{bad}"
            );
        }
    }

    /// **`app.quit` is the tray's Quit, and a privilege**: outside test mode it
    /// answers `not_enabled` (plan 026 §3.3).
    #[test]
    fn the_quit_door_is_test_mode_only() {
        let mut prod = env(None);
        prod.test_mode = false;
        let (code, message) =
            app_quit_door(&prod).expect_err("a driver door must not be reachable in a shipped app");
        assert_eq!(code, "not_enabled");
        assert!(message.contains("app.quit"), "{message}");
        assert!(app_quit_door(&env(None)).is_ok());
    }

    /// **`app.terminate` is the macOS system quit, and a privilege** (plan
    /// 026 AM7). Outside test mode it answers `not_enabled` on every
    /// platform. In test mode it opens on macOS, and everywhere else it
    /// answers `not_supported` and points at `app.quit`.
    #[test]
    fn the_terminate_door_is_test_mode_only() {
        let mut prod = env(None);
        prod.test_mode = false;
        let (code, message) = app_terminate_door(&prod)
            .expect_err("a driver door must not be reachable in a shipped app");
        assert_eq!(code, "not_enabled");
        assert!(message.contains("app.terminate"), "{message}");

        let test = env(None);
        if cfg!(target_os = "macos") {
            assert!(app_terminate_door(&test).is_ok());
        } else {
            let (code, message) =
                app_terminate_door(&test).expect_err("only macOS has the system-quit path");
            assert_eq!(code, "not_supported");
            assert_eq!(
                message,
                "app.terminate is the macOS system-quit path; use app.quit"
            );
        }
    }
}
