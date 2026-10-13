//! The composer's `/` menu COMMANDS (`lane_slash_filter`,
//! `lane_slash_dispatch`, `lane_slash_accept`, `lane_set_title`; plan 029 D12),
//! driven through Tauri's own argument extraction on its mock runtime.
//!
//! `launch_command_tests`'s reason: a `#[tauri::command]` receives only the
//! arguments it declares and drops every other key of the invoke without a
//! word, so a command that misnamed `caret` or `sessionId` would run as if the
//! panel had sent nothing — a closed menu, a refusal — and nothing below the
//! command could tell. These cells send exactly the camelCase keys `bridge.ts`
//! sends and assert the command READ each one.

use std::collections::BTreeMap;
use std::sync::Arc;

use serde_json::{json, Value};
use shed_core::roost::AgentLaneStamp;
use tauri::test::{mock_builder, mock_context, noop_assets};

use crate::lane::{LaneMachines, Lanes};
use crate::launch_command_tests::invoke;
use crate::machines::ReachKind;

/// A machine layer with no lanes at all: every lane op answers `no_lane`.
struct NoMachines;

impl LaneMachines for NoMachines {
    fn agent_lanes(&self, _machine: &str) -> BTreeMap<String, AgentLaneStamp> {
        BTreeMap::new()
    }

    fn reach_kind(&self, machine: &str) -> Result<ReachKind, String> {
        Err(format!("{machine} is not configured"))
    }
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
/// `sessionId` and `title` — a blank title is refused there, a real one finds
/// no lane. A command missing a key it needs is refused by Tauri, never run on
/// a guess.
#[test]
fn the_slash_commands_read_every_key_the_panel_sends() {
    let runtime = tokio::runtime::Builder::new_multi_thread()
        .worker_threads(1)
        .enable_all()
        .build()
        .expect("a test runtime");
    let lanes = Arc::new(Lanes::for_test(
        runtime.handle().clone(),
        Arc::new(NoMachines),
    ));
    let app = mock_builder()
        .manage(lanes)
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

    let target = json!({ "machine": "m1", "kind": "craze", "sessionId": "0123456789ab" });
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
