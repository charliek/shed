//! The launch COMMANDS (`rc_launch`, `machine_launch`), driven through Tauri's
//! own argument extraction on its mock runtime (plan 026 §3.5).
//!
//! Why not call the command bodies directly: the bug this guards against lives
//! in the layer above them. A `#[tauri::command]` receives only the arguments
//! it declares, and Tauri drops every other key of the invoke without a word —
//! so a command that does not declare `permission_mode` opens a tab for
//! `invoke("rc_launch", {…, permissionMode: "plan"})` instead of refusing it,
//! and nothing below the command can see that the field was ever sent.

use std::collections::HashMap;
use std::sync::Arc;

use serde_json::{json, Value};
use shed_app::Backend;
use shed_core::config::{MachineEntry, ShedConfig, ShedServerEntry};
use shed_core::roost::testing::FakeRoost;
use tauri::ipc::{CallbackFn, InvokeBody};
use tauri::test::{get_ipc_response, mock_builder, mock_context, noop_assets, INVOKE_KEY};
use tauri::webview::InvokeRequest;

use crate::machines::ReachOptions;
use crate::roost_hosts::{RoostHosts, MODE_REFUSED, PROMPT_REFUSED};

const SERVER: &str = "mock";
const SHED: &str = "p26-command";
const MACHINE: &str = "mini3";

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

/// **Both launch commands refuse a first prompt and a permission mode they are
/// SENT** — `bad_request: <the refusal>`, with no `tab.open` — for each field
/// under its camelCase invoke key. Then the same two commands without either
/// field open a tab each, so the zero was the refusal's and not a host that
/// could not have opened anything.
#[test]
fn the_launch_commands_refuse_a_first_prompt_or_permission_mode_they_are_sent() {
    let runtime = tokio::runtime::Builder::new_multi_thread()
        .worker_threads(2)
        .enable_all()
        .build()
        .expect("a test runtime");
    let fake = runtime.block_on(FakeRoost::start());

    let config = ShedConfig {
        servers: vec![ShedServerEntry {
            name: SERVER.to_string(),
            host: "127.0.0.1".to_string(),
            ssh_port: 2222,
            ..Default::default()
        }],
        machines: vec![MachineEntry {
            name: MACHINE.to_string(),
            host: MACHINE.to_string(),
            ssh_port: 22,
            ..Default::default()
        }],
        ..Default::default()
    };
    let shed_token = format!("roost:{SERVER}/{SHED}");
    let reach = ReachOptions {
        roost_sockets: HashMap::from([
            (MACHINE.to_string(), fake.socket_path().to_path_buf()),
            (shed_token, fake.socket_path().to_path_buf()),
        ]),
        ssh_bin: None,
        test_mode: true,
        ..ReachOptions::default()
    };
    let machines = Arc::new(RoostHosts::start(
        runtime.handle(),
        &config,
        reach,
        false,
        Arc::new(|| {}),
    ));
    // The refresh that makes the shed addressable at all (`ssh_entry`'s gate).
    runtime.block_on(machines.observe_sheds(
        &[(SERVER.to_string(), SHED.to_string())],
        &[SERVER.to_string()],
    ));

    let app = mock_builder()
        .manage(Arc::new(Backend::from_config(&config, None)))
        .manage(Arc::clone(&machines))
        .invoke_handler(tauri::generate_handler![
            crate::rc_launch,
            crate::machine_launch
        ])
        .build(mock_context(noop_assets()))
        .expect("the mock app");
    let webview = tauri::WebviewWindowBuilder::new(&app, "main", Default::default())
        .build()
        .expect("the mock webview");

    let doors = [
        ("rc_launch", json!({ "shed": SHED, "kind": "opencode" })),
        (
            "machine_launch",
            json!({ "machine": MACHINE, "kind": "opencode" }),
        ),
    ];
    for (cmd, args) in &doors {
        for (key, value, refusal) in [
            ("permissionMode", "plan", MODE_REFUSED),
            ("initialPrompt", "fix the build", PROMPT_REFUSED),
        ] {
            let mut body = args.clone();
            body[key] = json!(value);
            let answer = invoke(&webview, cmd, body);
            assert_eq!(
                answer,
                Err(json!(format!("bad_request: {refusal}"))),
                "{cmd} with {key}"
            );
            assert_eq!(
                fake.tab_open_calls(),
                Vec::<Value>::new(),
                "{cmd} with {key}: a refused launch opened a tab"
            );
        }
    }

    for (cmd, args) in &doors {
        let row = invoke(&webview, cmd, args.clone())
            .unwrap_or_else(|e| panic!("{cmd} without either field launches: {e}"));
        assert_eq!(row["kind"], json!("opencode"), "{cmd}: {row}");
    }
    assert_eq!(fake.tab_open_calls().len(), 2, "one tab per command");
}
