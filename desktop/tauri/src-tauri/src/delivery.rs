//! Provisioning by delivery (plan 039 A6): the source runtime's wiring, and the bodies of
//! the delivery ops shared by the socket IPC (`ipc.rs`) and the Tauri commands
//! (`lib.rs`), so the two doors cannot disagree.
//!
//! | op | params | result |
//! |---|---|---|
//! | `delivery.status` | — | this machine, each server's stream, recent answers (redacted) |
//! | `delivery.settings` | — | `delivery.toml` as `{path, exists, error, providers}` |
//! | `delivery.set` | `provider`, `config` (object, or null to remove) | the new settings view |
//! | `delivery.preview` | `server`, `shed`? | what that shed would get, redacted |
//! | `shed.delivery` | `server`, `shed` | the shed document's delivery record |
//! | `shed.provision` | `server`, `shed` | the provision's progress and its outcome |
//! | `activity.server_list` | `limit`?, `cursor`?, `shed`? | the servers' activity, merged newest first |
//!
//! The runtime runs beside the embedded broker in PR A: both use this machine's one
//! identity and the same credential store (see `shed_desktop_core::delivery::runtime`).

use std::sync::Arc;

use serde_json::{json, Value};

use shed_app::Backend;
use shed_core::http::Client;
use shed_desktop_core::delivery::source;
use shed_desktop_core::delivery::{DeliveryOptions, DeliveryRuntime, DiskReader};

use crate::env::Env;

/// An op's refusal: a code and a message.
pub type OpError = (String, String);

fn bad(message: impl Into<String>) -> OpError {
    ("bad_request".to_string(), message.into())
}

/// The broker's `known_hosts`, which the SSH minter pins servers against.
const KNOWN_HOSTS: &str = "~/.shed/known_hosts";

/// Starts the source runtime: this machine's identity (`<state>/broker/machine.json`,
/// the broker's), the fingerprint key under the same state directory, a stream per
/// server of the app's shed config. Blocking file I/O; the setup hook calls it.
pub fn start(env: &Env, handle: &tokio::runtime::Handle) -> Arc<DeliveryRuntime> {
    let state = source::paths::state_dir();
    let machine = source::machine::load_or_create().map(|l| l.identity);
    if let Err(e) = &machine {
        app_warn!(
            "shed-desktop-tauri: delivery: no machine identity ({e}); no delivery stream runs"
        );
    }
    let opts = DeliveryOptions {
        shed_config_path: env.config_path.clone(),
        settings_path: env.delivery_settings_path.clone(),
        key_path: shed_desktop_core::delivery::key::key_path(&state),
        known_hosts: source::paths::expand_tilde(KNOWN_HOSTS),
        mock_base_url: if env.test_mode {
            env.mock_base_url.clone()
        } else {
            None
        },
        unreachable_hosts: env.mock_unreachable_hosts.clone(),
        reader: DiskReader::from_process(),
        rescan: shed_desktop_core::delivery::runtime::RESCAN,
    };
    let rt = DeliveryRuntime::start(handle, opts, machine);
    app_info!(
        "shed-desktop-tauri: delivery: settings at {}",
        env.delivery_settings_path.display()
    );
    rt
}

/// A shed name as the server takes one: 1 to 64 bytes of letters, digits, `.`, `_` and
/// `-`, not starting with `.` or `-`.
fn valid_shed(name: &str) -> bool {
    let b = name.as_bytes();
    !b.is_empty()
        && b.len() <= 64
        && b[0].is_ascii_alphanumeric()
        && b.iter()
            .all(|c| c.is_ascii_alphanumeric() || matches!(c, b'.' | b'_' | b'-'))
}

/// The client of the configured server `server`.
fn client(backend: &Backend, server: &str) -> Result<Client, OpError> {
    backend
        .host_clients()
        .into_iter()
        .find(|(name, _)| name == server)
        .map(|(_, c)| c)
        .ok_or_else(|| bad(format!("unknown server: {server:?}")))
}

fn shed_param(shed: &str) -> Result<(), OpError> {
    if valid_shed(shed) {
        Ok(())
    } else {
        Err(bad("shed is not a shed name"))
    }
}

fn failure(e: &shed_core::http::ApiFailure) -> OpError {
    let code = match e.status {
        Some(404) => "not_found",
        Some(_) => "server_refused",
        None => "action_failed",
    };
    (code.to_string(), e.message.clone())
}

/// `delivery.set`.
pub fn set(rt: &DeliveryRuntime, provider: &str, config: &Value) -> Result<Value, OpError> {
    if !config.is_object() && !config.is_null() {
        return Err(bad("config is an object, or null to remove the provider"));
    }
    rt.set(provider, config)
}

/// `delivery.preview`: with a shed, its document is read from `server` first.
pub async fn preview(
    rt: &DeliveryRuntime,
    backend: &Backend,
    server: &str,
    shed: Option<&str>,
) -> Result<Value, OpError> {
    let c = client(backend, server)?;
    let doc = match shed {
        None => None,
        Some(name) => {
            shed_param(name)?;
            Some(c.shed_document(name).await.map_err(|e| failure(&e))?)
        }
    };
    rt.preview(server, doc).await
}

/// `shed.delivery`: the delivery record of the shed's document (null for a server
/// older than delivery, whose document has none).
pub async fn shed_delivery(backend: &Backend, server: &str, shed: &str) -> Result<Value, OpError> {
    shed_param(shed)?;
    let doc = client(backend, server)?
        .shed_document(shed)
        .await
        .map_err(|e| failure(&e))?;
    Ok(json!({
        "server": server,
        "shed": shed,
        "delivery": doc.get("delivery").cloned().unwrap_or(Value::Null),
    }))
}

/// `shed.provision`: runs `POST /api/sheds/{shed}/provision` and reports its progress
/// and how it ended (the record it ended with, or the server's error).
pub async fn shed_provision(backend: &Backend, server: &str, shed: &str) -> Result<Value, OpError> {
    shed_param(shed)?;
    let out = client(backend, server)?.provision_shed(shed).await;
    let (ok, delivery, error) = match &out.result {
        Ok(doc) => (
            true,
            doc.get("delivery").cloned().unwrap_or(Value::Null),
            Value::Null,
        ),
        Err(e) => (
            false,
            Value::Null,
            serde_json::to_value(e).unwrap_or(Value::Null),
        ),
    };
    Ok(json!({
        "server": server,
        "shed": shed,
        "ok": ok,
        "progress": out.progress,
        "delivery": delivery,
        "error": error,
    }))
}

/// `activity.server_list`.
pub async fn activity(
    backend: &Backend,
    limit: Option<u64>,
    cursor: Option<&str>,
    shed: Option<&str>,
) -> Result<Value, OpError> {
    let limit = match limit {
        None => shed_desktop_core::delivery::activity::DEFAULT_LIMIT,
        Some(l)
            if (1..=u64::from(shed_desktop_core::delivery::activity::MAX_LIMIT)).contains(&l) =>
        {
            l as u32
        }
        Some(_) => return Err(bad("limit is 1 to 500")),
    };
    if let Some(s) = shed {
        shed_param(s)?;
    }
    if cursor.is_some_and(|c| c.len() > 64 << 10) {
        return Err(bad("cursor is too long"));
    }
    shed_desktop_core::delivery::activity::server_list(&backend.host_clients(), cursor, shed, limit)
        .await
        .map_err(bad)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn shed_names_are_validated() {
        for ok in ["hello-world", "a", "w.1_x", &"a".repeat(64)] {
            assert!(valid_shed(ok), "{ok}");
        }
        for bad in ["", ".a", "-a", "a/b", "a b", "../x", &"a".repeat(65), "é"] {
            assert!(!valid_shed(bad), "{bad}");
        }
    }
}
