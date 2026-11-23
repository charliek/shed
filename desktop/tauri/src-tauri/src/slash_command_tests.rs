//! The composer's `/` menu COMMANDS (`lane_slash_filter`,
//! `lane_slash_dispatch`, `lane_slash_accept`, `lane_set_title`; plan 029 D12),
//! driven through Tauri's own argument extraction on its mock runtime.
//!
//! A `#[tauri::command]` receives only the arguments it declares and drops
//! every other key of the invoke without a word, so a command that misnamed
//! `caret` or `session_id` would run as if the panel had sent nothing — a
//! closed menu, a refusal — and nothing below the command could tell. These
//! cells send exactly the camelCase keys `bridge.ts` sends and assert the
//! command READ each one.
//!
//! The lane address is a shed `target` (`roost:<server>/<shed>`) plus
//! `sessionId`. There is no `machine` and no `kind`: craze is the only lane.

use std::sync::Arc;

use serde_json::{json, Value};
use shed_core::config::ShedConfig;
use shed_desktop_core::{ShedDesktopRuntime, ShedReachOptions};
use tauri::ipc::{CallbackFn, InvokeBody};
use tauri::test::{get_ipc_response, mock_builder, mock_context, noop_assets, INVOKE_KEY};
use tauri::webview::InvokeRequest;

/// One invoke of `cmd` with `body` as its arguments, answered the way the
/// frontend's `invoke` sees it: the command's value, or its error string.
fn invoke(
    webview: &tauri::WebviewWindow<tauri::test::MockRuntime>,
    cmd: &str,
    body: Value,
) -> Result<Value, Value> {
    get_ipc_response(
        webview,
        InvokeRequest {
            cmd: cmd.into(),
            callback: CallbackFn(0),
            error: CallbackFn(1),
            url: "tauri://localhost".parse().expect("a url"),
            body: InvokeBody::Json(body),
            headers: Default::default(),
            invoke_key: INVOKE_KEY.to_string(),
        },
    )
    .map(|response| response.deserialize::<Value>().expect("a JSON answer"))
}

/// A resolved menu as `lane_messages` answers it: a rename and a mode that
/// waits for the turn.
fn menu() -> Value {
    json!({
        "rows": [
            {"name": "rename", "source": "builtin", "arg": "required", "while_working": true,
             "action": {"kind": "set_title"}},
            {"name": "plan", "source": "builtin", "arg": "none", "while_working": false,
             "action": {"kind": "set_mode", "change": {"kind": "mode", "id": "plan"}}}
        ],
        "hidden": [{"name": "help", "reason": "not available here yet"}]
    })
}

/// **Each command reads every key the panel sends**: the caret moves what
/// `lane_slash_filter` answers (0 is "not in the token", so closed), the
/// activity dims and refuses (`working`), `row` and `key` pick what
/// `lane_slash_accept` does, and `lane_set_title` reaches the lane layer with
/// `target`, `sessionId` and `title` — a blank title is refused there, a real
/// one finds no lane. A command missing a key it needs is refused by Tauri,
/// never run on a guess.
#[test]
fn the_slash_commands_read_every_key_the_panel_sends() {
    let runtime = tokio::runtime::Builder::new_multi_thread()
        .worker_threads(1)
        .enable_all()
        .build()
        .expect("a test runtime");
    // No listed shed and no open lane: a real title reaches the lane layer and
    // answers `no_lane`; the slash commands do not touch the runtime at all.
    let desktop = Arc::new(ShedDesktopRuntime::start_headless(
        runtime.handle().clone(),
        &ShedConfig::default(),
        ShedReachOptions {
            test_mode: true,
            ..ShedReachOptions::default()
        },
        false,
    ));
    let app = mock_builder()
        .manage(desktop)
        .invoke_handler(tauri::generate_handler![
            crate::lane_slash_filter,
            crate::lane_slash_dispatch,
            crate::lane_slash_accept,
            crate::lane_set_title
        ])
        .build(mock_context(noop_assets()))
        .expect("the mock app");
    let webview = tauri::WebviewWindowBuilder::new(&app, "main", Default::default())
        .build()
        .expect("the mock webview");

    let open = invoke(
        &webview,
        "lane_slash_filter",
        json!({ "menu": menu(), "draft": "/pl", "caret": 3, "activity": "working" }),
    )
    .expect("a filter");
    assert_eq!(open["open"], true, "{open}");
    assert_eq!(open["rows"][0]["row"]["name"], "plan");
    assert_eq!(
        open["rows"][0]["dimmed"], true,
        "`activity` was read: {open}"
    );
    let closed = invoke(
        &webview,
        "lane_slash_filter",
        json!({ "menu": menu(), "draft": "/pl", "caret": 0, "activity": "idle" }),
    )
    .expect("a filter");
    assert_eq!(closed["open"], false, "`caret` was read: {closed}");

    assert_eq!(
        invoke(
            &webview,
            "lane_slash_dispatch",
            json!({ "menu": menu(), "draft": "/plan", "activity": "needs_approval" })
        ),
        Ok(json!({"kind": "refuse", "reason": "/plan: runs after this turn"}))
    );
    assert_eq!(
        invoke(
            &webview,
            "lane_slash_dispatch",
            json!({ "menu": menu(), "draft": "/rename x" })
        ),
        Ok(json!({"kind": "run", "action": {"kind": "set_title"}, "arg": "x"})),
        "`activity` is optional"
    );
    assert_eq!(
        invoke(
            &webview,
            "lane_slash_accept",
            json!({ "menu": menu(), "draft": "/re", "row": "rename", "key": "tab", "activity": "idle" })
        ),
        Ok(json!({"kind": "complete", "draft": "/rename ", "caret": 8}))
    );
    assert_eq!(
        invoke(
            &webview,
            "lane_slash_accept",
            json!({ "menu": menu(), "draft": "/re", "row": "rename", "key": "sideways" })
        ),
        Err(json!(
            "bad_request: 'key' must be enter, tab or pick, got \"sideways\""
        )),
        "`key` was read"
    );

    let target = json!({ "target": "roost:srv/dev", "sessionId": "0123456789ab" });
    let with = |extra: Value| {
        let mut body = target.clone();
        body.as_object_mut()
            .unwrap()
            .extend(extra.as_object().unwrap().clone());
        body
    };
    let blank = invoke(&webview, "lane_set_title", with(json!({ "title": "  " })));
    assert!(
        matches!(&blank, Err(Value::String(e)) if e.starts_with("bad_request: ")),
        "`title` was read: {blank:?}"
    );
    let unopened = invoke(&webview, "lane_set_title", with(json!({ "title": "x" })));
    assert!(
        matches!(&unopened, Err(Value::String(e)) if e.starts_with("no_lane: ")),
        "the lane layer was reached: {unopened:?}"
    );
    let untitled = invoke(&webview, "lane_set_title", target.clone());
    assert!(
        matches!(&untitled, Err(Value::String(e)) if e.contains("title")),
        "no title, no rename: {untitled:?}"
    );
}
