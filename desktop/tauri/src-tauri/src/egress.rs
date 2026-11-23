//! The egress decision feed's wiring (plan 039 PR B): one consumer of each configured
//! server's `/api/egress/stream` on the Backend's own control-scope clients
//! ([`shed_desktop_core::egress::EgressFeed`]), and the bodies of its ops, shared by the
//! socket IPC (`ipc.rs`) and the Tauri commands (`lib.rs`).
//!
//! | op | params | result |
//! |---|---|---|
//! | `egress.recent` | `server`, `shed`, `created_at`, `blocks`? | `{server, shed, created_at, events}`: that shed incarnation's last decisions (at most 50), newest first; `created_at` is the shed row's own; `blocks: true` keeps the `deny` ones |
//! | `egress.status` | — | `{servers}`: each server's stream (`state`, `error`, `connects`, `decisions`) |
//! | `egress.server_profiles` | — | `{hosts}`: each server's egress profiles, or its error (the `egress_profiles` command's rows) |
//!
//! A new decision emits `egress-changed` `{server}` to the webviews.

use std::sync::Arc;

use serde_json::{json, Value};
use tauri::Emitter;

use shed_app::Backend;
use shed_desktop_core::egress::{blocks, EgressFeed, EgressOptions};

/// An op's refusal: a code and a message.
pub type OpError = (String, String);

fn bad(message: impl Into<String>) -> OpError {
    ("bad_request".to_string(), message.into())
}

/// Starts the feed on the app's runtime: a consumer per server the Backend lists.
pub fn start(
    backend: &Backend,
    handle: &tokio::runtime::Handle,
    app: tauri::AppHandle,
) -> Arc<EgressFeed> {
    EgressFeed::start(
        handle,
        backend.host_clients(),
        EgressOptions::default(),
        Some(Arc::new(move |server: &str| {
            let _ = app.emit("egress-changed", json!({ "server": server }));
        })),
    )
}

/// `egress.recent`.
pub fn recent(
    feed: &EgressFeed,
    server: &str,
    shed: &str,
    created_at: &str,
    only_blocks: bool,
) -> Result<Value, OpError> {
    if server.is_empty() || shed.is_empty() {
        return Err(bad("'server' and 'shed' are required"));
    }
    if shed_app::timefmt::parse_instant(created_at).is_none() {
        return Err(bad(
            "'created_at' is required: the shed's own creation time (its row's), RFC 3339",
        ));
    }
    let events = feed.recent(server, shed, created_at);
    let events = if only_blocks { blocks(&events) } else { events };
    Ok(json!({ "server": server, "shed": shed, "created_at": created_at, "events": events }))
}

/// `egress.status`.
pub fn status(feed: &EgressFeed) -> Value {
    json!({ "servers": feed.status() })
}
