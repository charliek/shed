//! Credential sources (plan 035 §3.4, plan 040 §3.9): who answers credential requests
//! for this user on each configured server (this machine, another machine, the account
//! service, or nobody), "Use this machine" and "Stop answering from here" — the
//! desktop's half of the pin. Shared by the `source.status` / `sources.list` /
//! `source.use` / `source.unbind` IPC ops and the app's commands of the same names, so
//! the two doors cannot disagree.
//!
//! The calls go out on the Backend's ordinary control-scope client per server (the
//! credential the app uses for shed lifecycle calls), never the source's
//! credentials-scope certificate. The id a move sends is THIS machine's: the identity
//! the source runtime ([`DeliveryRuntime`]) loaded at start from `shed-source`
//! (`<state>/broker/machine.json`), or, before it started, the one that file holds.
//!
//! Every machine name or id here came from other machines by way of the server and is
//! already cleaned for display by `shed_app::HostSource`; this machine's own name is
//! cleaned the same way before it leaves.

use serde_json::{json, Value};

use shed_app::{source_error_class, Backend, HostSource};
use shed_core::http::{SourceError, SOURCE_MACHINE_NOT_FOUND};
use shed_core::models::{display_source_text, SOURCE_IS_SERVICE, SOURCE_TEXT_MAX_BYTES};
use shed_desktop_core::delivery::source::machine::{self, MachineIdentity};
use shed_desktop_core::delivery::DeliveryRuntime;

fn err(code: &str, message: impl Into<String>) -> (String, String) {
    (code.to_string(), message.into())
}

/// This machine's identity, or why there is none: the source runtime's, or, with no
/// runtime yet (only before setup finished), the identity file read directly.
async fn this_machine(rt: Option<&DeliveryRuntime>) -> Result<MachineIdentity, String> {
    match rt {
        Some(rt) => rt.machine(),
        None => tokio::task::spawn_blocking(|| machine::load_or_create().map(|l| l.identity))
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
pub async fn status(backend: &Backend, rt: Option<&DeliveryRuntime>) -> Value {
    let me = this_machine(rt).await;
    let rows = backend
        .source_status(me.as_ref().ok().map(|m| m.id.as_str()))
        .await;
    let (this_machine, this_machine_error) = identity_json(&me);
    json!({
        "rows": rows,
        "this_machine": this_machine,
        "this_machine_error": this_machine_error,
    })
}

/// Who answers on a server, as `sources.list` names it: a closed list.
///
/// | kind | the row |
/// |---|---|
/// | `this_machine` | this machine is the bound source (`machine` is it) |
/// | `other_machine` | another machine is (`machine` names it) |
/// | `service` | the account service answers, and no machine (`url`, and `state` `not_delivering` when the service does not deliver to that server) |
/// | `nobody` | no source is bound |
/// | `unsupported` | the server is older than credential sources |
/// | `unknown` | the server could not say (unreachable, busy, or it refused the read: `error`) |
pub fn kind_of(row: &HostSource) -> &'static str {
    if !row.supported && row.error.is_none() && !row.busy {
        "unsupported"
    } else if row.busy || row.unreachable || row.error.is_some() || !row.supported {
        "unknown"
    } else if row.service.is_some() {
        "service"
    } else if row.source.is_some() && row.this_machine {
        "this_machine"
    } else if row.source.is_some() {
        "other_machine"
    } else {
        "nobody"
    }
}

/// One `sources.list` row: the server's [`HostSource`] in the view's terms, with this
/// machine's delivery stream to it (`stream`, the runtime's status for that server, or
/// null when none runs) and whether this machine stopped answering there (`paused`).
fn list_row(row: &HostSource, stream: Option<Value>, paused: bool) -> Value {
    let service = row.service.as_ref();
    json!({
        "server": row.host,
        "kind": kind_of(row),
        "machine": row.source.as_ref().map(|m| json!({"id": m.id, "name": m.name})),
        "connected": row.connected,
        "url": service.map(|s| s.url.clone()),
        "state": service.and_then(|s| s.state.clone()),
        "stream": stream.unwrap_or(Value::Null),
        "paused": paused,
        "busy": row.busy,
        "unreachable": row.unreachable,
        "error": row.error_class,
        "error_message": row.error_class.map(list_error_message),
    })
}

/// The fixed words for a row's error class ([`shed_app::SOURCE_ERROR_CLASSES`]): never
/// the server's.
fn list_error_message(class: &str) -> &'static str {
    match class {
        "unreachable" => "the server could not be reached, or its answer could not be read",
        "busy" => "the server is busy; it is asked again later",
        "unauthorized" => "the server refused this app's credentials",
        "forbidden" => "the server does not let this client see its credential source",
        "source_is_service" => "the account service answers for you on this server",
        "refused" => "the server refused to say who answers",
        _ => "the server could not be asked who answers",
    }
}

/// `sources.list` → `{servers, this_machine, this_machine_error, paused_error}`: one row
/// per configured server, by name — `{server, kind, machine, connected, url, state,
/// stream, paused, busy, unreachable, error, error_message}` ([`kind_of`] for `kind`;
/// `paused`: this machine stopped answering there, so it holds no stream; `error` a class
/// from [`shed_app::SOURCE_ERROR_CLASSES`] and `error_message` its fixed words, never the
/// server's) — this machine's `{id, name}` (or why there is none), and `paused_error`,
/// what is wrong with the paused set ([`shed_desktop_core::delivery::source::paused::PausedError`],
/// e.g. `unparseable`: every server is then paused), or null. Every name, id and URL
/// came from the server and is cleaned for display.
pub async fn list(backend: &Backend, rt: Option<&DeliveryRuntime>) -> Value {
    let me = this_machine(rt).await;
    let rows = backend
        .source_status(me.as_ref().ok().map(|m| m.id.as_str()))
        .await;
    let streams = rt.map(|rt| rt.streams()).unwrap_or_default();
    let servers: Vec<Value> = rows
        .iter()
        .map(|row| {
            let stream = streams
                .iter()
                .find(|s| s.server == row.host)
                .and_then(|s| serde_json::to_value(&s.status).ok());
            list_row(row, stream, rt.is_some_and(|rt| rt.is_paused(&row.host)))
        })
        .collect();
    let (this_machine, this_machine_error) = identity_json(&me);
    json!({
        "servers": servers,
        "this_machine": this_machine,
        "this_machine_error": this_machine_error,
        "paused_error": rt.and_then(|rt| rt.paused_error()),
    })
}

fn identity_json(me: &Result<MachineIdentity, String>) -> (Value, Value) {
    match me {
        Ok(m) => (
            json!({
                "id": m.id,
                "name": display_source_text(&m.name, SOURCE_TEXT_MAX_BYTES),
            }),
            Value::Null,
        ),
        Err(e) => (Value::Null, Value::from(e.clone())),
    }
}

/// `source.unbind {host}` → "Stop answering from here": clear this user's credential
/// source on `host` (`shed source unbind`) and stop this machine answering there.
/// Answers `{server, unbound, previous, previous_was_this_machine, forgotten_machines,
/// paused, verified}`: `unbound` false when there was no source (nothing changed),
/// `previous` the machine that was the source (`{id, name}`, cleaned) or null, `paused`
/// that this machine now holds no stream there (false only with no source runtime),
/// `verified` that a read afterwards found this machine not bound (false when it could
/// not be read). A stream attempt admitted after the unbind is unbound once more; still
/// bound then is `rebound` (the pause stays). Moves and unbinds on one server run one
/// at a time.
///
/// The server binds the first of the user's machines whose stream arrives after an
/// unbind, so this machine's stream is stopped FIRST: `host` joins the paused set
/// (written before anything is sent), and the unbind is sent only once that server's
/// stream task has ended (at most [`PAUSE_QUIESCE`]; past it, `stream_not_stopped` and
/// nothing is sent). The pause is taken back only after a refusal that proves nothing
/// changed ([`refusal_changed_nothing`]); on any other failure (no answer, an answer
/// that cannot be read, a server error) it stays, since the unbind may have happened.
/// One request, never retried; refusals in fixed words ([`refusal`]).
pub async fn unbind(
    backend: &Backend,
    rt: Option<&DeliveryRuntime>,
    host: &str,
) -> Result<Value, (String, String)> {
    if !backend.host_names().iter().any(|h| h == host) {
        return Err(err("bad_request", format!("unknown server: {host:?}")));
    }
    // One move or unbind per server at a time: a second waits for the first, so its
    // stream wait and its rollback are its own.
    let lock = server_lock(host);
    let _held = lock.lock().await;
    let was_paused = rt.is_some_and(|rt| rt.is_paused(host));
    if let Some(rt) = rt {
        rt.pause(host).map_err(|_| {
            err(
                "action_failed",
                format!("could not record that this machine stops answering on {host}; nothing was sent"),
            )
        })?;
        if !rt.wait_stopped(host, PAUSE_QUIESCE).await {
            return Err(err(
                "stream_not_stopped",
                format!(
                    "this machine's stream to {host} did not stop in time, so nothing was sent; \
                     it stays stopped: try again"
                ),
            ));
        }
    }
    let done = match backend.source_unbind(host).await {
        Ok(done) => done,
        Err(e) => {
            if let (Some(rt), false, true) = (rt, was_paused, refusal_changed_nothing(&e)) {
                let _ = rt.resume(host);
            }
            return Err(refusal(host, &e, "unbind"));
        }
    };
    let me = this_machine(rt).await.ok();
    // A stream attempt the server received before the unbind can still be admitted
    // after it and bind this machine again. Look once; if so, unbind once more; if it
    // is still bound then, say so (the pause stays).
    let verified = match me.as_ref() {
        Some(m) => match still_bound(backend, host, &m.id).await {
            Some(true) => {
                let _ = backend.source_unbind(host).await;
                match still_bound(backend, host, &m.id).await {
                    Some(true) => {
                        return Err(err(
                            "rebound",
                            format!(
                                "{host} bound this machine again after the unbind; it stays \
                                 stopped here: try again"
                            ),
                        ))
                    }
                    Some(false) => true,
                    None => false,
                }
            }
            Some(false) => true,
            None => false,
        },
        None => false,
    };
    let previous = done.previous.as_ref();
    Ok(json!({
        "server": host,
        "unbound": done.unbound,
        "previous": previous.map(|p| json!({
            "id": display_source_text(&p.id, SOURCE_TEXT_MAX_BYTES),
            "name": display_source_text(&p.name, SOURCE_TEXT_MAX_BYTES),
        })),
        "previous_was_this_machine": previous
            .zip(me.as_ref())
            .is_some_and(|(p, m)| p.id == m.id),
        "forgotten_machines": done.forgotten_machines,
        "paused": rt.is_some(),
        "verified": verified,
    }))
}

/// Whether `host`'s sources document binds the machine `id` (one `GET /api/sources`);
/// `None` when it could not be read.
async fn still_bound(backend: &Backend, host: &str, id: &str) -> Option<bool> {
    let (_, client) = backend
        .host_clients()
        .into_iter()
        .find(|(n, _)| n == host)?;
    client
        .source_status()
        .await
        .ok()
        .map(|doc| doc.source_machine_id() == Some(id))
}

/// The lock a move or an unbind on `host` holds from its first step to its last.
fn server_lock(host: &str) -> std::sync::Arc<tokio::sync::Mutex<()>> {
    static LOCKS: std::sync::OnceLock<
        std::sync::Mutex<std::collections::HashMap<String, std::sync::Arc<tokio::sync::Mutex<()>>>>,
    > = std::sync::OnceLock::new();
    LOCKS
        .get_or_init(Default::default)
        .lock()
        .unwrap_or_else(|e| e.into_inner())
        .entry(host.to_string())
        .or_default()
        .clone()
}

/// How long an unbind waits for this machine's stream to that server to stop.
pub const PAUSE_QUIESCE: std::time::Duration = std::time::Duration::from_secs(10);

/// The refusals the server's unbind route gives BEFORE it changes anything: a server
/// older than the route (a plain 404), and a 4xx with one of these codes. Anything else
/// (no answer, an unreadable one, a 5xx, a busy server, another code) may follow a
/// change.
fn refusal_changed_nothing(e: &SourceError) -> bool {
    const BEFORE_ACTING: &[&str] = &[
        "INVALID_REQUEST",
        "UNAUTHORIZED",
        "FORBIDDEN",
        SOURCE_IS_SERVICE,
    ];
    match e {
        SourceError::Unsupported => true,
        SourceError::Api { status, code, .. } => {
            (400..500).contains(status) && BEFORE_ACTING.contains(&code.as_str())
        }
        _ => false,
    }
}

/// The refusal for a user the account service answers for on `host` (`409
/// SOURCE_IS_SERVICE` on every source route): fixed words, not the server's.
fn service_refusal(host: &str) -> (String, String) {
    err(
        "source_is_service",
        format!("the account service answers for you on {host}; no machine does there"),
    )
}

/// A refused move or unbind (`what`), as a code and fixed words a person can act on:
/// never the server's message, code or a decode error's text.
fn refusal(host: &str, e: &SourceError, what: &str) -> (String, String) {
    match e {
        SourceError::Unsupported => err(
            "unsupported",
            format!(
                "{host} does not support credential sources (the server is older than this app)"
            ),
        ),
        SourceError::Api { code, .. } if code == SOURCE_IS_SERVICE => service_refusal(host),
        SourceError::Api { code, .. } if code == SOURCE_MACHINE_NOT_FOUND => err(
            "source_machine_not_found",
            format!(
                "{host} has not seen this machine yet: its delivery stream has not reached \
                 that server. Check the server, then try again."
            ),
        ),
        SourceError::Api { status: 500, .. } => err(
            "action_failed",
            format!(
                "{host} could not confirm the {what}; check which machine is the source \
                 before trying again"
            ),
        ),
        _ => match source_error_class(e) {
            Some("unauthorized") => err(
                "unauthorized",
                format!("{host} refused this app's credentials"),
            ),
            Some("forbidden") => err(
                "forbidden",
                format!("{host} does not let this client change its credential source"),
            ),
            Some("busy") => err(
                "action_failed",
                format!("{host} is busy; try again in a moment"),
            ),
            Some("unreachable") => err(
                "action_failed",
                format!(
                    "{host} could not be reached, or its answer could not be read; check which \
                     machine is the source before trying again"
                ),
            ),
            _ => err("action_failed", format!("{host} refused the {what}")),
        },
    }
}

/// `source.use {host}` → "Answer from here": take `host` out of the paused set (so a
/// stream runs for it again), make THIS machine the one that answers on `host`, then
/// reopen its delivery stream to `host` so it connects at once. Answers `{row,
/// woke_source}` (`woke_source`: a stream was reopened).
///
/// One request, never retried: a refusal is reported in fixed words ([`refusal`]). An unknown `host` is `bad_request`; no identity is
/// `machine_identity_unavailable`, with the reason, and nothing is sent.
pub async fn use_this_machine(
    backend: &Backend,
    rt: Option<&DeliveryRuntime>,
    host: &str,
) -> Result<Value, (String, String)> {
    if !backend.host_names().iter().any(|h| h == host) {
        return Err(err("bad_request", format!("unknown server: {host:?}")));
    }
    let lock = server_lock(host);
    let _held = lock.lock().await;
    let me = this_machine(rt).await.map_err(|why| {
        err(
            "machine_identity_unavailable",
            format!("this machine has no identity, so it cannot become the source: {why}"),
        )
    })?;
    if let Some(rt) = rt {
        let was_paused = rt.resume(host).map_err(|_| {
            err(
                "action_failed",
                format!(
                    "could not record that this machine answers on {host} again; nothing was sent"
                ),
            )
        })?;
        // After "Stop answering from here" the server has forgotten this machine, and a
        // move to a machine it has not seen is refused: let the stream that now runs
        // again reach it first (with no binding there, it is bound by arriving).
        if was_paused {
            rt.wait_first_attempt(host, RESUME_FIRST_ATTEMPT).await;
        }
    }
    let row = backend
        .source_use(host, &me.id)
        .await
        .map_err(|e| refusal(host, &e, "move"))?;
    let woke_source = rt.is_some_and(|rt| rt.retry_now(host));
    Ok(json!({ "row": row, "woke_source": woke_source }))
}

/// How long "Answer from here" waits for a resumed stream's first attempt before the
/// move.
const RESUME_FIRST_ATTEMPT: std::time::Duration = std::time::Duration::from_secs(10);

#[cfg(test)]
mod tests {
    use super::*;
    use shed_app::{SourceMachineRow, SourceServiceRow};
    use shed_core::http::ShedError;
    use shed_core::models::SourceDocument;

    fn row_of(text: &str, me: Option<&str>) -> HostSource {
        let doc: SourceDocument = serde_json::from_str(text).unwrap();
        HostSource::from_document("mini".into(), &doc, me)
    }

    const ME: &str = "5f0c6e2a9b8d4c1e7a3f2b6d8e9c0a1b";
    const DESK: &str = "fedcba9876543210fedcba9876543210";

    fn machine_doc(bound: Option<&str>) -> String {
        let binding = bound.map_or("null".to_string(), |id| {
            format!(r#"{{"delivery":[{{"kind":"machine","id":"{id}"}}],"signing":[{{"kind":"machine","id":"{id}"}}]}}"#)
        });
        format!(
            r#"{{"user":"ada","binding":{binding},"machines":[{{"id":"{ME}","name":"box","connected":true}},{{"id":"{DESK}","name":"desk","connected":false}}]}}"#
        )
    }

    /// Each "Where" state of the Credentials view, from the server's document (the
    /// account service's from the shared fixtures) or its failure.
    #[test]
    fn every_row_says_who_answers() {
        let me = Some(ME);
        let row = row_of(&machine_doc(Some(ME)), me);
        assert_eq!(kind_of(&row), "this_machine");
        let shown = list_row(&row, Some(json!({"state": "connected"})), false);
        assert_eq!(shown["machine"], json!({"id": ME, "name": "box"}));
        assert_eq!(shown["connected"], true);
        assert_eq!(shown["stream"]["state"], "connected");
        assert_eq!(shown["paused"], false);
        let paused = list_row(&row_of(&machine_doc(None), me), None, true);
        assert_eq!(
            (paused["kind"].clone(), paused["paused"].clone()),
            (json!("nobody"), json!(true))
        );
        assert_eq!(
            (shown["url"].clone(), shown["state"].clone()),
            (Value::Null, Value::Null)
        );

        let row = row_of(&machine_doc(Some(DESK)), me);
        assert_eq!(kind_of(&row), "other_machine");
        assert_eq!(list_row(&row, None, false)["machine"]["name"], "desk");

        assert_eq!(kind_of(&row_of(&machine_doc(None), me)), "nobody");

        let row = row_of(
            include_str!("../../../../crates/fixtures/sources/service.json"),
            me,
        );
        assert_eq!(kind_of(&row), "service");
        let shown = list_row(&row, None, false);
        assert_eq!(shown["url"], "https://accounts.example.test");
        assert_eq!(shown["state"], Value::Null);
        assert_eq!(shown["machine"], Value::Null);
        assert_eq!(shown["stream"], Value::Null);
        let row = row_of(
            include_str!("../../../../crates/fixtures/sources/service-not-delivering.json"),
            me,
        );
        assert_eq!(list_row(&row, None, false)["state"], "not_delivering");

        assert_eq!(
            kind_of(&HostSource::from_error(
                "mini".into(),
                &SourceError::Unsupported
            )),
            "unsupported"
        );
        for e in [
            SourceError::Busy {
                retry_after: None,
                message: "x".into(),
            },
            SourceError::Shed(ShedError::Transport("refused".into())),
            api(403, "FORBIDDEN", "a script key"),
        ] {
            assert_eq!(
                kind_of(&HostSource::from_error("mini".into(), &e)),
                "unknown",
                "{e:?}"
            );
        }
        // A row that names both (a server's mistake) is the service's: it says so.
        let mut both = row_of(&machine_doc(Some(ME)), me);
        both.service = Some(SourceServiceRow {
            url: "https://accounts.example.test".into(),
            state: None,
        });
        assert_eq!(kind_of(&both), "service");
        both.service = None;
        both.source = Some(SourceMachineRow {
            id: DESK.into(),
            name: String::new(),
        });
        both.this_machine = false;
        assert_eq!(kind_of(&both), "other_machine");
    }

    fn api(status: u16, code: &str, message: &str) -> SourceError {
        SourceError::Api {
            status,
            code: code.into(),
            message: message.into(),
        }
    }

    /// Every refusal of a move or an unbind is a code and fixed words: no server
    /// message, no server code outside the known ones, no decode error's text.
    #[test]
    fn refusals_are_fixed_words() {
        let busy = SourceError::Busy {
            retry_after: None,
            message: "ghp_SENTINEL".into(),
        };
        let cases: Vec<(SourceError, &str, &str)> = vec![
            (
                api(409, SOURCE_IS_SERVICE, "ghp_SENTINEL"),
                "source_is_service",
                "the account service answers for you on mini; no machine does there",
            ),
            (
                SourceError::Unsupported,
                "unsupported",
                "mini does not support credential sources (the server is older than this app)",
            ),
            (
                api(404, SOURCE_MACHINE_NOT_FOUND, "ghp_SENTINEL"),
                "source_machine_not_found",
                "mini has not seen this machine yet: its delivery stream has not reached that \
                 server. Check the server, then try again.",
            ),
            (
                api(403, "ghp_SENTINEL_CODE", "ghp_SENTINEL"),
                "forbidden",
                "mini does not let this client change its credential source",
            ),
            (
                api(401, "UNAUTHORIZED", "ghp_SENTINEL"),
                "unauthorized",
                "mini refused this app's credentials",
            ),
            (
                api(500, "INTERNAL_ERROR", "ghp_SENTINEL"),
                "action_failed",
                "mini could not confirm the move; check which machine is the source before \
                 trying again",
            ),
            (
                api(404, "ghp_SENTINEL_CODE", "ghp_SENTINEL"),
                "action_failed",
                "mini refused the move",
            ),
            (busy, "action_failed", "mini is busy; try again in a moment"),
            (
                SourceError::Shed(ShedError::Transport("ghp_SENTINEL".into())),
                "action_failed",
                "mini could not be reached, or its answer could not be read; check which \
                 machine is the source before trying again",
            ),
            (
                SourceError::Shed(ShedError::Decode("ghp_SENTINEL".into())),
                "action_failed",
                "mini could not be reached, or its answer could not be read; check which \
                 machine is the source before trying again",
            ),
        ];
        for (e, code, message) in cases {
            let got = refusal("mini", &e, "move");
            assert_eq!((got.0.as_str(), got.1.as_str()), (code, message), "{e:?}");
            let unbind = refusal("mini", &e, "unbind");
            assert!(!unbind.1.contains("SENTINEL"), "{e:?}");
        }
        assert_eq!(
            refusal("mini", &api(418, "X", "ghp_SENTINEL"), "unbind").1,
            "mini refused the unbind"
        );
    }

    /// Finding 2: an unbind's pause is taken back only after a refusal the server gives
    /// before it changes anything.
    #[test]
    fn only_a_refusal_before_acting_takes_the_pause_back() {
        for e in [
            SourceError::Unsupported,
            api(400, "INVALID_REQUEST", "x"),
            api(401, "UNAUTHORIZED", "x"),
            api(403, "FORBIDDEN", "x"),
            api(409, SOURCE_IS_SERVICE, "x"),
        ] {
            assert!(refusal_changed_nothing(&e), "{e:?}");
        }
        for e in [
            api(500, "INTERNAL_ERROR", "x"),
            api(503, "PLUGINS_DISABLED", "x"),
            api(409, "SOMETHING_ELSE", "x"),
            api(500, "FORBIDDEN", "x"),
            SourceError::Busy {
                retry_after: None,
                message: "x".into(),
            },
            SourceError::Shed(ShedError::Transport("reset".into())),
            SourceError::Shed(ShedError::Decode("not the answer".into())),
            SourceError::Shed(ShedError::BadStatus(502)),
        ] {
            assert!(!refusal_changed_nothing(&e), "{e:?}");
        }
    }

    /// A row's error is a class and fixed words, never the server's.
    #[test]
    fn a_row_error_is_a_class() {
        let row = HostSource::from_error(
            "mini".into(),
            &api(403, "ghp_SENTINEL_CODE", "ghp_SENTINEL"),
        );
        let shown = list_row(&row, None, false);
        assert_eq!(shown["error"], "forbidden");
        assert_eq!(
            shown["error_message"],
            "the server does not let this client see its credential source"
        );
        assert!(!shown.to_string().contains("SENTINEL"), "{shown}");
        for class in shed_app::SOURCE_ERROR_CLASSES {
            assert!(!list_error_message(class).is_empty());
        }
    }

    /// One lock per server: the same one for the same name, another for another, and a
    /// second holder waits for the first.
    #[tokio::test]
    async fn moves_and_unbinds_on_one_server_run_one_at_a_time() {
        let a = server_lock("lock-test-a");
        assert!(std::sync::Arc::ptr_eq(&a, &server_lock("lock-test-a")));
        assert!(!std::sync::Arc::ptr_eq(&a, &server_lock("lock-test-b")));
        let held = a.lock().await;
        assert!(server_lock("lock-test-a").try_lock().is_err());
        assert!(server_lock("lock-test-b").try_lock().is_ok());
        drop(held);
        assert!(server_lock("lock-test-a").try_lock().is_ok());
    }
}
