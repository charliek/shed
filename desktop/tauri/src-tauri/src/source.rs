//! Credential sources (plan 035 §3.4): which machine answers credential requests for
//! this user on each configured server, and "Use this machine" — the desktop's half of
//! the pin. Shared by the `source.status` / `source.use` IPC ops and the Preferences
//! window's `source_status` / `source_use` commands, so the two doors cannot disagree.
//!
//! The calls go out on the Backend's ordinary control-scope client per server (the
//! credential the app uses for shed lifecycle calls), never the broker's
//! credentials-scope certificate. The id a move sends is THIS machine's, from
//! [`BrokerRuntime::this_machine`]: the identity the embedded broker presents, or the
//! one its state directory holds when the broker did not start.
//!
//! Every machine name or id here came from other machines by way of the server and is
//! already cleaned for display by `shed_app::HostSource`; this machine's own name is
//! cleaned the same way before it leaves.

use serde_json::{json, Value};

use shed_app::Backend;
use shed_core::http::{SourceError, SOURCE_MACHINE_NOT_FOUND};
use shed_core::models::{display_source_text, SOURCE_TEXT_MAX_BYTES};

use crate::broker::BrokerRuntime;

fn err(code: &str, message: impl Into<String>) -> (String, String) {
    (code.to_string(), message.into())
}

/// This machine's identity, or why there is none. With no broker runtime at all (only
/// before setup finished) the shared file is read directly.
async fn this_machine(rt: Option<&BrokerRuntime>) -> Result<shed_app::MachineIdentity, String> {
    match rt {
        Some(rt) => rt.this_machine().await,
        None => tokio::task::spawn_blocking(shed_app::machine_identity)
            .await
            .unwrap_or_else(|e| Err(format!("machine identity load task failed: {e}"))),
    }
}

/// `source.status` → `{rows, this_machine, this_machine_error}`.
///
/// `rows` is one per configured server ([`shed_app::HostSource`]: `host`, `supported`,
/// `source` `{id, name}` or null, `this_machine`, `connected`, `busy`, `unreachable`,
/// `error`); a server older than the route is `supported: false` with no error, and one
/// that could not be reached or read is `unreachable` (unknown, not unsupported).
/// `this_machine` is `{id, name}` or null, with `this_machine_error` saying why it is
/// null.
pub async fn status(backend: &Backend, rt: Option<&BrokerRuntime>) -> Value {
    let me = this_machine(rt).await;
    let rows = backend
        .source_status(me.as_ref().ok().map(|m| m.id.as_str()))
        .await;
    let (this_machine, this_machine_error) = match &me {
        Ok(m) => (
            json!({
                "id": m.id,
                "name": display_source_text(&m.name, SOURCE_TEXT_MAX_BYTES),
            }),
            Value::Null,
        ),
        Err(e) => (Value::Null, Value::from(e.clone())),
    };
    json!({
        "rows": rows,
        "this_machine": this_machine,
        "this_machine_error": this_machine_error,
    })
}

/// `source.use {host}` → make THIS machine the one that answers on `host`, then wake
/// the broker (when it is running) so it connects at once. Answers `{row, woke_broker}`.
///
/// One request, never retried: a refusal is reported as it is, in plain words
/// ([`use_error`]). An unknown `host` is `bad_request`; no identity is
/// `machine_identity_unavailable`, with the reason, and nothing is sent.
pub async fn use_this_machine(
    backend: &Backend,
    rt: Option<&BrokerRuntime>,
    host: &str,
) -> Result<Value, (String, String)> {
    if !backend.host_names().iter().any(|h| h == host) {
        return Err(err("bad_request", format!("unknown server: {host:?}")));
    }
    let me = this_machine(rt).await.map_err(|why| {
        err(
            "machine_identity_unavailable",
            format!("this machine has no identity, so it cannot become the source: {why}"),
        )
    })?;
    let row = backend
        .source_use(host, &me.id)
        .await
        .map_err(|e| use_error(host, &e, rt))?;
    let woke_broker = rt.is_some_and(BrokerRuntime::retry_now);
    Ok(json!({ "row": row, "woke_broker": woke_broker }))
}

/// A refused move, in words a person can act on. The server's own message (already
/// cleaned) is kept where it says something this app cannot.
fn use_error(host: &str, e: &SourceError, rt: Option<&BrokerRuntime>) -> (String, String) {
    match e {
        SourceError::Unsupported => err(
            "unsupported",
            format!(
                "{host} does not support credential sources (the server is older than this app)"
            ),
        ),
        SourceError::Api { code, .. } if code == SOURCE_MACHINE_NOT_FOUND => err(
            "source_machine_not_found",
            format!(
                "{host} has not seen this machine yet: its credential broker has not reached \
                 that server. {} Then try again.",
                broker_hint(host, rt)
            ),
        ),
        SourceError::Api {
            status: 403,
            message,
            ..
        } => err("forbidden", format!("{host} refused the move: {message}")),
        SourceError::Api {
            status: 500,
            message,
            ..
        } => err(
            "action_failed",
            format!(
                "{host} could not confirm the move ({message}); check which machine is the \
                 source before trying again"
            ),
        ),
        SourceError::Api { message, .. } => err(
            "action_failed",
            format!("{host} refused the move: {message}"),
        ),
        // A move waits for the user's records itself, so this is not expected; if a
        // server answers it, the move did not happen and can simply be tried again.
        SourceError::Busy { message, .. } => err(
            "action_failed",
            format!("{host} is busy ({message}); try again in a moment"),
        ),
        SourceError::Shed(e) => err("action_failed", format!("{host}: {e}")),
    }
}

/// The state of this app's broker, for the "has not seen this machine" message.
fn broker_hint(host: &str, rt: Option<&BrokerRuntime>) -> String {
    match rt.map(BrokerRuntime::broker_failed) {
        Some(true) => {
            "The in-app broker did not start (see Preferences → Credential broker).".to_string()
        }
        Some(false) => format!(
            "The in-app broker is running; check that it lists {host} under Credential broker."
        ),
        None => "Check that this machine's credential broker is running.".to_string(),
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use shed_core::http::ShedError;

    fn api(status: u16, code: &str, message: &str) -> SourceError {
        SourceError::Api {
            status,
            code: code.into(),
            message: message.into(),
        }
    }

    /// Each refusal a move can draw reads as something a person can act on, keeps the
    /// server's own words where they matter, and names the server it came from.
    #[test]
    fn a_refused_move_is_worded_by_what_the_server_said() {
        let (code, message) = use_error("mini", &SourceError::Unsupported, None);
        assert_eq!(code, "unsupported");
        assert!(message.contains("mini does not support"), "{message}");

        let (code, message) = use_error(
            "mini",
            &api(404, SOURCE_MACHINE_NOT_FOUND, "no such machine"),
            None,
        );
        assert_eq!(code, "source_machine_not_found");
        assert!(
            message.starts_with("mini has not seen this machine yet"),
            "{message}"
        );
        assert!(message.contains("credential broker"), "{message}");

        let (code, message) = use_error("mini", &api(403, "FORBIDDEN", "a script key"), None);
        assert_eq!(code, "forbidden");
        assert_eq!(message, "mini refused the move: a script key");

        let (code, message) = use_error("mini", &api(500, "INTERNAL", "not confirmed"), None);
        assert_eq!(code, "action_failed");
        assert!(message.contains("could not confirm"), "{message}");
        assert!(message.contains("not confirmed"), "{message}");

        // A 404 with another code is the server's refusal, not "not seen".
        let (code, _) = use_error("mini", &api(404, "NOT_FOUND", "x"), None);
        assert_eq!(code, "action_failed");

        let (code, message) = use_error(
            "mini",
            &SourceError::Busy {
                retry_after: None,
                message: "being saved".into(),
            },
            None,
        );
        assert_eq!(code, "action_failed");
        assert_eq!(message, "mini is busy (being saved); try again in a moment");

        let (code, message) = use_error(
            "mini",
            &SourceError::Shed(ShedError::Transport("refused".into())),
            None,
        );
        assert_eq!(code, "action_failed");
        assert!(message.starts_with("mini: "), "{message}");
    }
}
