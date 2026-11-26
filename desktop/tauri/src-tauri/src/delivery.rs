//! Provisioning by delivery (plan 039 A6): the source runtime's wiring, and the bodies of
//! the delivery ops shared by the socket IPC (`ipc.rs`) and the Tauri commands
//! (`lib.rs`), so the two doors cannot disagree.
//!
//! | op | params | result |
//! |---|---|---|
//! | `delivery.status` | — | this machine, each server's stream, recent answers (redacted) |
//! | `delivery.describe` | — | `{providers}`: each provider's descriptor (title, summary, where it lands, fields) |
//! | `delivery.recent` | — | `{recent}`: the answers this machine gave, newest first, redacted |
//! | `delivery.settings` | — | `delivery.toml` as `{path, exists, error, providers}` |
//! | `delivery.set` | `provider`, `config` (object, or null to remove) | the new settings view |
//! | `delivery.preview` | `server`, `shed`? or `backend` and `repo`?, `runs`? | what that shed (or a new shed on that backend with that repo) would get, redacted |
//! | `shed.delivery` | `server`, `shed` | the shed document's delivery record, and the shed's effective egress |
//! | `shed.provision` | `server`, `shed` | the provision's progress and its outcome |
//! | `activity.server_list` | `limit`?, `cursor`?, `shed`? | the servers' activity, merged newest first |
//!
//! The runtime is this machine's only source: it holds this machine's identity and the
//! credentials-scope store (see `shed_desktop_core::delivery::runtime`).

use std::sync::Arc;

use serde_json::{json, Value};

use shed_app::Backend;
use shed_core::http::{Client, SourceError};
use shed_core::models::{SourceDocument, SOURCE_IS_SERVICE};
use shed_desktop_core::delivery::settings_file::provider_names;
use shed_desktop_core::delivery::source;
use shed_desktop_core::delivery::source::delivery::{ServiceCheck, SourceCheck};
use shed_desktop_core::delivery::source::ServerTarget;
use shed_desktop_core::delivery::{DeliveryOptions, DeliveryRuntime, DiskReader, PreviewOf};

use crate::env::Env;

/// An op's refusal: a code and a message.
pub type OpError = (String, String);

fn bad(message: impl Into<String>) -> OpError {
    ("bad_request".to_string(), message.into())
}

/// What a server's `GET /api/sources` says of the account service, for a stream settled
/// on it: still answering (the document names it, or the route refuses
/// `SOURCE_IS_SERVICE`), no longer (a machine's document, or a server older than the
/// route), or unknown (any other failure: ask again at the next re-check).
pub fn service_check_of(read: &Result<SourceDocument, SourceError>) -> ServiceCheck {
    match read {
        Ok(doc) if doc.service().is_some() => ServiceCheck::Service,
        Ok(_) | Err(SourceError::Unsupported) => ServiceCheck::NotService,
        Err(SourceError::Api { code, .. }) if code == SOURCE_IS_SERVICE => ServiceCheck::Service,
        Err(_) => ServiceCheck::Unknown,
    }
}

/// Builds the app's control-scope clients from the shed CLI config as it reads NOW (the
/// same parts the startup Backend was built from).
pub type FreshBackend = Arc<dyn Fn() -> Backend + Send + Sync>;

fn same_url(a: &str, b: &str) -> bool {
    a.trim_end_matches('/') == b.trim_end_matches('/')
}

fn named_client(backend: &Backend, target: &ServerTarget) -> Option<Client> {
    backend
        .host_clients()
        .into_iter()
        .find(|(name, c)| *name == target.name && same_url(c.base_url(), &target.url))
        .map(|(_, c)| c)
}

/// A control-scope client for the stream's CURRENT target: the startup Backend's, when
/// it has one of the same name at the same endpoint; else one from the config as it
/// reads now (a server added, or its endpoint changed, since the app started). `None`
/// when neither has it (then the re-check cannot tell).
pub fn client_for_target(
    backend: &Backend,
    fresh: &dyn Fn() -> Backend,
    target: &ServerTarget,
) -> Option<Client> {
    named_client(backend, target).or_else(|| named_client(&fresh(), target))
}

/// The source runtime's re-check of a server settled on the account service: `GET
/// /api/sources` on a control-scope client for the stream's current target
/// ([`client_for_target`]; the stream's credentials-scope certificate cannot reach the
/// route). A target no client matches is unknown.
fn service_check(backend: Arc<Backend>, fresh: FreshBackend) -> SourceCheck {
    Arc::new(move |target: &ServerTarget| {
        let (backend, fresh, target) = (backend.clone(), fresh.clone(), target.clone());
        Box::pin(async move {
            let client =
                tokio::task::spawn_blocking(move || client_for_target(&backend, &*fresh, &target))
                    .await
                    .ok()
                    .flatten();
            match client {
                Some(c) => service_check_of(&c.source_status().await),
                None => ServiceCheck::Unknown,
            }
        })
    })
}

/// Starts the source runtime: this machine's identity (`<state>/broker/machine.json`),
/// the fingerprint key under the same state directory, a stream per
/// server of the app's shed config, and the re-check a stream settled on the account
/// service asks through `backend` (or, for a target it does not have, through a client
/// `fresh` builds from the config as it reads then). Blocking file I/O; the setup hook
/// calls it.
pub fn start(
    env: &Env,
    handle: &tokio::runtime::Handle,
    backend: Arc<Backend>,
    fresh: FreshBackend,
) -> Arc<DeliveryRuntime> {
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
        known_hosts: source::paths::expand_tilde(shed_desktop_core::control_mint::KNOWN_HOSTS),
        mock_base_url: if env.test_mode {
            env.mock_base_url.clone()
        } else {
            None
        },
        unreachable_hosts: env.mock_unreachable_hosts.clone(),
        reader: DiskReader::from_process(),
        rescan: shed_desktop_core::delivery::runtime::RESCAN,
        service_recheck: env
            .source_recheck
            .unwrap_or(source::delivery::SERVICE_RECHECK),
        paused_path: source::paused::paused_state_path(),
    };
    let rt = DeliveryRuntime::start(handle, opts, machine, Some(service_check(backend, fresh)));
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

/// `delivery.describe`: every provider's descriptor, in the order they are asked (what a
/// form is drawn from: title, summary, where its items land, its fields).
pub fn describe() -> Value {
    json!({ "providers": shed_desktop_core::delivery::providers::describe() })
}

/// `delivery.recent`: the answers this machine gave, newest first (at most
/// [`shed_desktop_core::delivery::runtime::RECENT`]), redacted: when, the server, the
/// shed, the reason, the items (name, target, size, fingerprint), the problems, and what
/// the server made of each. No value.
pub fn recent(rt: &DeliveryRuntime) -> Value {
    json!({ "recent": rt.recent() })
}

/// `delivery.set`. With `base` (the provider's `config` as the form read it, null for a
/// provider not set up) a table changed since is refused `conflict`, nothing written.
pub fn set(
    rt: &DeliveryRuntime,
    provider: &str,
    config: &Value,
    base: Option<&Value>,
) -> Result<Value, OpError> {
    if !config.is_object() && !config.is_null() {
        return Err(bad("config is an object, or null to remove the provider"));
    }
    if base.is_some_and(|b| !b.is_object() && !b.is_null()) {
        return Err(bad("base is an object, or null for a provider not set up"));
    }
    rt.set(provider, config, base)
}

/// What a `delivery.preview` names: a shed on the server, or a new shed's backend and
/// repository (the Credentials view's Check), or neither (everything the providers have).
#[derive(Debug, Default, PartialEq, Eq)]
pub struct PreviewParams<'a> {
    pub shed: Option<&'a str>,
    pub backend: Option<&'a str>,
    pub repo: Option<&'a str>,
    /// The commands the person was told would run: a preview whose settings would run
    /// others runs nothing (`settings_changed`).
    pub expect_runs: Option<Vec<String>>,
}

/// A shed document that could not be read for a preview, as a code and fixed words:
/// never the server's text.
fn shed_read_failure(e: &shed_core::http::ApiFailure) -> OpError {
    let (code, _) = failure(e);
    let words = match code.as_str() {
        "not_found" => "the server has no shed by that name",
        "server_refused" => "the server refused to send the shed",
        _ => "the server could not be reached, or its answer could not be read",
    };
    (code, words.to_string())
}

/// `delivery.preview`: with a shed, its document is read from `server` first; with a
/// backend (and a repository, or none), the request a new shed of that kind would draw;
/// a shed and a backend together are refused, as is a repository with no backend. The
/// server is resolved as the source doors resolve it ([`crate::source::Resolver`]): the
/// shed config as it reads now, so a server added while the app runs is known and a
/// changed endpoint is read at the new one, for both forms.
pub async fn preview(
    rt: &DeliveryRuntime,
    servers: &crate::source::Resolver,
    server: &str,
    p: PreviewParams<'_>,
) -> Result<Value, OpError> {
    let c = servers.client(server).await?;
    let of = match (p.shed, p.backend) {
        (Some(_), Some(_)) => return Err(bad("name a shed, or a backend and a repo, not both")),
        (Some(name), None) => {
            if p.repo.is_some() {
                return Err(bad("a shed's repo is its own"));
            }
            shed_param(name)?;
            PreviewOf::Shed(
                c.shed_document(name)
                    .await
                    .map_err(|e| shed_read_failure(&e))?,
            )
        }
        (None, Some(b)) => PreviewOf::Target {
            backend: b.to_string(),
            repo: p.repo.map(str::to_string),
        },
        (None, None) if p.repo.is_some() => return Err(bad("a repo needs a backend")),
        (None, None) => PreviewOf::Everything,
    };
    rt.preview(server, of, p.expect_runs).await
}

/// `shed.delivery`: the delivery record of the shed's document (null for a server
/// older than delivery, whose document has none), and the shed's EFFECTIVE egress
/// (`egress`, see [`effective_egress`]) from `GET /api/egress/{shed}`, which resolves
/// the server's default profiles a shed created without `--egress` runs under. The
/// document's own `egress_profiles` is the explicit selection only, so an empty one
/// says nothing about whether egress is restricted. An egress read that fails leaves
/// `egress` null and says why in `egress_error`; the delivery record still comes back.
pub async fn shed_delivery(backend: &Backend, server: &str, shed: &str) -> Result<Value, OpError> {
    shed_param(shed)?;
    let c = client(backend, server)?;
    let (doc, status) = tokio::join!(c.shed_document(shed), c.egress_shed_status(shed));
    let doc = doc.map_err(|e| failure(&e))?;
    let explicit = doc.get("egress_profiles").cloned().unwrap_or(Value::Null);
    let (egress, egress_error) = match status {
        Ok(s) => (effective_egress(&explicit, &s), Value::Null),
        Err(e) => (Value::Null, Value::String(e.message)),
    };
    Ok(json!({
        "server": server,
        "shed": shed,
        "delivery": doc.get("delivery").cloned().unwrap_or(Value::Null),
        // The explicit selection (absent: none, the shed takes the server's default).
        "egress_profiles": explicit,
        "egress": egress,
        "egress_error": egress_error,
    }))
}

/// A list of non-empty strings off `v` (anything else: none).
fn names(v: Option<&Value>) -> Vec<String> {
    v.and_then(Value::as_array)
        .map(|a| {
            a.iter()
                .filter_map(Value::as_str)
                .filter(|s| !s.is_empty())
                .map(str::to_string)
                .collect()
        })
        .unwrap_or_default()
}

/// A shed's effective egress, read the way `shed egress show` reads it
/// (`cmd/shed/egress.go:printEgressStatus`), off the shed document's explicit
/// `egress_profiles` and `GET /api/egress/{shed}`'s `status`:
///
/// - `enabled`: the server's egress master switch.
/// - `restricted`: the shed runs behind the egress proxy — the switch is on and the
///   shed has a listener port or profiles in force. Off, it is not restricted.
/// - `profiles`: the profiles in force (empty when not restricted). The server
///   substitutes its `egress.default` for a shed with no explicit selection.
/// - `inherited`: those profiles are the server's default, not the shed's own choice.
/// - `mode`: the composed fall-through (`internal/egress/policy.go`): `audit` when any
///   profile in force says audit, `enforce` when every one's definition came back and
///   none does, else null (unknown).
pub fn effective_egress(explicit: &Value, status: &Value) -> Value {
    let enabled = status.get("enabled").and_then(Value::as_bool) == Some(true);
    let port = status.get("port").and_then(Value::as_u64).unwrap_or(0);
    let mut profiles = names(status.get("profiles"));
    let restricted = enabled && (port != 0 || !profiles.is_empty());
    if !restricted {
        profiles.clear();
    }
    let inherited = restricted && !profiles.is_empty() && names(Some(explicit)).is_empty();
    let rules = status.get("rules").and_then(Value::as_object);
    let mode_of = |p: &String| {
        rules
            .and_then(|r| r.get(p))
            .filter(|d| d.is_object())
            .map(|d| d.get("mode").and_then(Value::as_str).unwrap_or(""))
    };
    let mode = if profiles.is_empty() {
        Value::Null
    } else if profiles
        .iter()
        .any(|p| mode_of(p).is_some_and(|m| m.eq_ignore_ascii_case("audit")))
    {
        json!("audit")
    } else if profiles.iter().all(|p| mode_of(p).is_some()) {
        json!("enforce")
    } else {
        Value::Null
    };
    json!({
        "enabled": enabled,
        "restricted": restricted,
        "profiles": profiles,
        "inherited": inherited,
        "mode": mode,
    })
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

/// A provider as `ui.*` doors name one: one of [`provider_names`].
fn provider_param(params: &Value) -> Result<&str, OpError> {
    let p = params.get("provider").and_then(Value::as_str).unwrap_or("");
    let names = provider_names();
    if names.contains(&p) {
        Ok(p)
    } else {
        Err(bad(format!("'provider' is one of {}", names.join(", "))))
    }
}

fn str_param<'a>(params: &'a Value, key: &str) -> Result<&'a str, OpError> {
    params
        .get(key)
        .and_then(Value::as_str)
        .filter(|s| !s.is_empty())
        .ok_or_else(|| bad(format!("missing '{key}'")))
}

/// The provisioning UI's doors (plan 039 A7, re-hosted on the Credentials view in plan
/// 040 §3.9): the event to emit (to the dashboard, which holds the Credentials view) and
/// its payload, or the refusal. Opening and closing a shed's detail are what a person's
/// click does, and are open; the doors that type, press, check or deliver are
/// presses a harness has no pointer for, and are TEST-MODE ONLY, the
/// `ui.press_source_use` rule: each acts on the real control in the Credentials view
/// (typing sets the input's value and fires its own input event), so a disabled or
/// missing control takes no press.
///
/// | op | params | event |
/// |---|---|---|
/// | `ui.show_shed_delivery` | `server`, `shed` | `show-shed-delivery` |
/// | `ui.close_shed_delivery` | — | `close-shed-delivery` |
/// | `ui.press_deliver_again` | `server`, `shed` | `press-deliver-again` (test) |
/// | `ui.press_delivery_button` | `provider`, `button` | `press-delivery-button` (test) |
/// | `ui.type_delivery_field` | `provider`, `field`, `index`?, `value` | `type-delivery-field` (test) |
/// | `ui.fill_check` | `field` (by, shed, server, backend, repo), `value` | `fill-check` (test) |
/// | `ui.press_check` | `button` (run, details) | `press-check` (test) |
/// The Check form's controls a `ui.fill_check` fills: which kind of Check (`by`: `shed`
/// or `target`), the shed picked (`<server>/<shed>`), the server, the backend, the repo.
const CHECK_FIELDS: &[&str] = &["by", "shed", "server", "backend", "repo"];

pub fn ui_door(
    test_mode: bool,
    op: &str,
    params: &Value,
) -> Result<(&'static str, Value), OpError> {
    let gated = !matches!(op, "ui.show_shed_delivery" | "ui.close_shed_delivery");
    if gated && !test_mode {
        return Err((
            "not_enabled".to_string(),
            format!("{op} requires test mode"),
        ));
    }
    let shed_pair = |params: &Value| -> Result<Value, OpError> {
        let server = str_param(params, "server")?;
        let shed = str_param(params, "shed")?;
        shed_param(shed)?;
        Ok(json!({ "server": server, "shed": shed }))
    };
    match op {
        "ui.show_shed_delivery" => Ok(("show-shed-delivery", shed_pair(params)?)),
        "ui.close_shed_delivery" => Ok(("close-shed-delivery", json!({}))),
        "ui.press_deliver_again" => Ok(("press-deliver-again", shed_pair(params)?)),
        "ui.press_delivery_button" => Ok((
            "press-delivery-button",
            json!({ "provider": provider_param(params)?, "button": str_param(params, "button")? }),
        )),
        "ui.type_delivery_field" => {
            let provider = provider_param(params)?;
            let field = str_param(params, "field")?;
            let index = match params.get("index") {
                None | Some(Value::Null) => Value::Null,
                Some(v) => json!(v
                    .as_u64()
                    .ok_or_else(|| bad("'index' must be a non-negative integer"))?),
            };
            let value = params
                .get("value")
                .and_then(Value::as_str)
                .ok_or_else(|| bad("'value' must be a string"))?;
            Ok((
                "type-delivery-field",
                json!({ "provider": provider, "field": field, "index": index, "value": value }),
            ))
        }
        "ui.fill_check" => {
            let field = str_param(params, "field")?;
            if !CHECK_FIELDS.contains(&field) {
                return Err(bad(format!(
                    "'field' is one of {}",
                    CHECK_FIELDS.join(", ")
                )));
            }
            let value = params
                .get("value")
                .and_then(Value::as_str)
                .ok_or_else(|| bad("'value' must be a string"))?;
            Ok(("fill-check", json!({ "field": field, "value": value })))
        }
        "ui.press_check" => {
            let button = str_param(params, "button")?;
            if !matches!(button, "run" | "details") {
                return Err(bad("'button' is run or details"));
            }
            Ok(("press-check", json!({ "button": button })))
        }
        other => Err(("unknown_op".to_string(), format!("unknown op: {other}"))),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_ui_doors_are_gated_and_validated() {
        let p = json!({ "provider": "github" });
        for op in [
            "ui.press_delivery_button",
            "ui.type_delivery_field",
            "ui.fill_check",
            "ui.press_deliver_again",
        ] {
            let e = ui_door(false, op, &p).unwrap_err();
            assert_eq!(e.0, "not_enabled", "{op}");
        }
        // The switch, the Preferences preview and its section scroll are gone with the
        // Provisioning pane.
        for op in [
            "ui.press_delivery_toggle",
            "ui.preview_delivery",
            "ui.prefs_section",
        ] {
            assert_eq!(ui_door(true, op, &p).unwrap_err().0, "unknown_op", "{op}");
        }
        let (ev, payload) = ui_door(
            false,
            "ui.show_shed_delivery",
            &json!({"server": "mock", "shed": "w"}),
        )
        .unwrap();
        assert_eq!(ev, "show-shed-delivery");
        assert_eq!(payload, json!({"server": "mock", "shed": "w"}));
        assert_eq!(
            ui_door(false, "ui.close_shed_delivery", &json!({}))
                .unwrap()
                .0,
            "close-shed-delivery"
        );
        for bad in [
            json!({"server": "mock"}),
            json!({"server": "mock", "shed": "a/b"}),
            json!({"shed": "w"}),
        ] {
            assert_eq!(
                ui_door(false, "ui.show_shed_delivery", &bad).unwrap_err().0,
                "bad_request",
                "{bad}"
            );
        }
        let (_, typed) = ui_door(
            true,
            "ui.type_delivery_field",
            &json!({"provider": "ssh-key", "field": "only.repos", "index": 0, "value": "a/*"}),
        )
        .unwrap();
        assert_eq!(
            typed,
            json!({"provider": "ssh-key", "field": "only.repos", "index": 0, "value": "a/*"})
        );
        let (_, typed) = ui_door(
            true,
            "ui.type_delivery_field",
            &json!({"provider": "ssh-key", "field": "key_file", "value": ""}),
        )
        .unwrap();
        assert_eq!(typed["index"], Value::Null);
        for bad in [
            json!({"provider": "ssh-key", "field": "x"}),
            json!({"provider": "ssh-key", "field": "x", "value": ["a"]}),
            json!({"provider": "ssh-key", "field": "x", "index": -1, "value": "a"}),
            json!({"provider": "ssh-key", "value": "a"}),
            json!({"provider": "nope", "field": "x", "value": "a"}),
        ] {
            assert!(
                ui_door(true, "ui.type_delivery_field", &bad).is_err(),
                "{bad}"
            );
        }
        let (_, press) = ui_door(
            true,
            "ui.press_delivery_button",
            &json!({"provider": "files", "button": "add:files"}),
        )
        .unwrap();
        assert_eq!(press, json!({"provider": "files", "button": "add:files"}));
        assert!(ui_door(
            true,
            "ui.press_delivery_button",
            &json!({"provider": "files"})
        )
        .is_err());
        // Check: its controls filled and pressed one by one, as a person does.
        let (ev, fill) = ui_door(
            true,
            "ui.fill_check",
            &json!({"field": "repo", "value": "a/b"}),
        )
        .unwrap();
        assert_eq!(
            (ev, fill),
            ("fill-check", json!({"field": "repo", "value": "a/b"}))
        );
        let (ev, press) = ui_door(true, "ui.press_check", &json!({"button": "run"})).unwrap();
        assert_eq!((ev, press), ("press-check", json!({"button": "run"})));
        for (op, bad) in [
            ("ui.fill_check", json!({"field": "shed"})),
            ("ui.fill_check", json!({"field": "nope", "value": "x"})),
            ("ui.fill_check", json!({"field": "repo", "value": 3})),
            ("ui.press_check", json!({"button": "save"})),
            ("ui.press_check", json!({})),
        ] {
            assert_eq!(
                ui_door(true, op, &bad).unwrap_err().0,
                "bad_request",
                "{op} {bad}"
            );
        }
        for op in ["ui.fill_check", "ui.press_check"] {
            assert_eq!(ui_door(false, op, &json!({})).unwrap_err().0, "not_enabled");
        }
        assert_eq!(
            ui_door(
                true,
                "ui.run_check",
                &json!({"server": "mock", "shed": "w"})
            )
            .unwrap_err()
            .0,
            "unknown_op"
        );
    }

    /// The re-check of a stream settled on the account service reads the sources
    /// document (the shared fixtures): the service still answering keeps it settled, a
    /// machine's document or an older server reopens the stream, and anything else is
    /// unknown (asked again later, never a reconnect loop).
    #[test]
    fn a_settled_stream_is_rechecked_from_the_sources_document() {
        let doc = |text: &str| -> Result<SourceDocument, SourceError> {
            Ok(serde_json::from_str(text).unwrap())
        };
        let api = |status: u16, code: &str| -> Result<SourceDocument, SourceError> {
            Err(SourceError::Api {
                status,
                code: code.into(),
                message: "x".into(),
            })
        };
        for text in [
            include_str!("../../../../crates/fixtures/sources/service.json"),
            include_str!("../../../../crates/fixtures/sources/service-not-delivering.json"),
        ] {
            assert_eq!(service_check_of(&doc(text)), ServiceCheck::Service);
        }
        assert_eq!(
            service_check_of(&api(409, SOURCE_IS_SERVICE)),
            ServiceCheck::Service
        );
        assert_eq!(
            service_check_of(&doc(
                r#"{"user":"ada","binding":{"delivery":[{"kind":"machine","id":"fedcba9876543210fedcba9876543210"}]},"machines":[]}"#
            )),
            ServiceCheck::NotService
        );
        assert_eq!(
            service_check_of(&doc(r#"{"user":"ada","binding":null,"machines":[]}"#)),
            ServiceCheck::NotService
        );
        assert_eq!(
            service_check_of(&Err(SourceError::Unsupported)),
            ServiceCheck::NotService
        );
        assert_eq!(
            service_check_of(&api(403, "FORBIDDEN")),
            ServiceCheck::Unknown
        );
        assert_eq!(
            service_check_of(&Err(SourceError::Busy {
                retry_after: None,
                message: "x".into()
            })),
            ServiceCheck::Unknown
        );
        assert_eq!(
            service_check_of(&Err(SourceError::Shed(
                shed_core::http::ShedError::Transport("refused".into())
            ))),
            ServiceCheck::Unknown
        );
    }

    /// Finding 5: the re-check asks a client for the stream's current target: the
    /// startup one at the same endpoint, else one from the config as it reads now (a
    /// server added since, or an endpoint changed); none when neither matches.
    #[test]
    fn the_recheck_client_matches_the_current_target() {
        let dir = tempfile::tempdir().unwrap();
        let cfg = dir.path().join("config.yaml");
        let write = |servers: &[&str]| {
            let mut text = String::from("servers:\n");
            for s in servers {
                text.push_str(&format!("    {s}:\n        host: {s}.example.test\n"));
            }
            std::fs::write(&cfg, text).unwrap();
        };
        let backend_at = |mock: &str| {
            Backend::from_env_parts_with_credentials(
                true,
                Some(mock),
                &cfg,
                None,
                None,
                &Default::default(),
            )
        };
        write(&["a"]);
        let startup = backend_at("http://127.0.0.1:9");
        let target = |name: &str, url: &str| ServerTarget {
            name: name.into(),
            url: url.into(),
            token: String::new(),
            tls_fingerprint: String::new(),
            ssh_host: String::new(),
            ssh_port: 0,
            auth_mode: String::new(),
            user: String::new(),
            identity_file: String::new(),
        };
        let built = std::cell::Cell::new(0);
        write(&["a", "b"]);
        let fresh = || {
            built.set(built.get() + 1);
            backend_at("http://127.0.0.1:9,a=http://127.0.0.1:7")
        };
        // The startup client, as it is: nothing built.
        let c = client_for_target(&startup, &fresh, &target("a", "http://127.0.0.1:9/")).unwrap();
        assert_eq!(c.base_url(), "http://127.0.0.1:9");
        assert_eq!(built.get(), 0);
        // The endpoint changed: the new one, never the old.
        let c = client_for_target(&startup, &fresh, &target("a", "http://127.0.0.1:7")).unwrap();
        assert_eq!(c.base_url(), "http://127.0.0.1:7");
        // A server added since the start.
        let c = client_for_target(&startup, &fresh, &target("b", "http://127.0.0.1:9")).unwrap();
        assert_eq!(c.base_url(), "http://127.0.0.1:9");
        // Neither knows it.
        assert!(client_for_target(&startup, &fresh, &target("c", "http://127.0.0.1:9")).is_none());
        assert!(client_for_target(&startup, &fresh, &target("a", "http://127.0.0.1:5")).is_none());
    }

    /// The descriptors are the providers' own, in their order: the `ui.*` doors take
    /// exactly those names.
    #[test]
    fn describe_lists_the_providers_the_doors_take() {
        let d = describe();
        let names: Vec<&str> = d["providers"]
            .as_array()
            .unwrap()
            .iter()
            .map(|p| p["name"].as_str().unwrap())
            .collect();
        assert_eq!(names, provider_names());
        assert_eq!(
            names,
            ["ssh-key", "github", "api-keys", "envsecrets-login", "files"]
        );
        for p in d["providers"].as_array().unwrap() {
            assert!(p["title"].as_str().is_some_and(|t| !t.is_empty()), "{p}");
            assert!(p["fields"].as_array().is_some_and(|f| !f.is_empty()), "{p}");
            assert!(provider_param(&json!({ "provider": p["name"] })).is_ok());
        }
        let e = provider_param(&json!({ "provider": "nope" })).unwrap_err();
        assert_eq!(
            e.1,
            "'provider' is one of ssh-key, github, api-keys, envsecrets-login, files"
        );
    }

    /// The Credentials view's view-model tests (`ui/test/credentials.test.mjs`) draw their
    /// forms from a copy of `delivery.describe`: it is this answer exactly.
    /// `UPDATE_DESCRIBE_FIXTURE=1` rewrites it.
    #[test]
    fn the_ui_tests_describe_fixture_is_the_answer() {
        let path = std::path::Path::new(env!("CARGO_MANIFEST_DIR"))
            .join("../ui/test/fixtures/describe.json");
        let want = serde_json::to_string_pretty(&describe()).unwrap() + "\n";
        if std::env::var_os("UPDATE_DESCRIBE_FIXTURE").is_some() {
            std::fs::create_dir_all(path.parent().unwrap()).unwrap();
            std::fs::write(&path, &want).unwrap();
        }
        let got = std::fs::read_to_string(&path).unwrap_or_default();
        assert_eq!(
            got,
            want,
            "{} differs from delivery.describe: run with UPDATE_DESCRIBE_FIXTURE=1",
            path.display()
        );
    }

    /// A shed that could not be read for a Check is a code and fixed words: the
    /// server's message never comes through.
    #[test]
    fn a_shed_read_failure_is_fixed_words() {
        for (status, code) in [
            (Some(404), "not_found"),
            (Some(500), "server_refused"),
            (None, "action_failed"),
        ] {
            let e = shed_core::http::ApiFailure {
                status,
                code: "SHED_NOT_FOUND".into(),
                message: "SERVER SENTINEL".into(),
                reason: String::new(),
            };
            let (c, m) = shed_read_failure(&e);
            assert_eq!(c, code);
            assert!(
                !m.contains("SENTINEL") && !m.contains("SHED_NOT_FOUND"),
                "{m}"
            );
        }
    }

    #[test]
    fn shed_names_are_validated() {
        for ok in ["hello-world", "a", "w.1_x", &"a".repeat(64)] {
            assert!(valid_shed(ok), "{ok}");
        }
        for bad in ["", ".a", "-a", "a/b", "a b", "../x", &"a".repeat(65), "é"] {
            assert!(!valid_shed(bad), "{bad}");
        }
    }

    #[test]
    fn effective_egress_reads_what_the_server_resolved() {
        let rules = json!({
            "default": {"mode": "audit", "allow": ["*.github.com"]},
            "base": {"allow": ["example.test"]},
        });
        // Created without --egress: no explicit selection, the server's default in force.
        let e = effective_egress(
            &Value::Null,
            &json!({"shed": "s", "enabled": true, "profiles": ["default"], "port": 20001, "rules": rules}),
        );
        assert_eq!(
            e,
            json!({"enabled": true, "restricted": true, "profiles": ["default"],
                   "inherited": true, "mode": "audit"})
        );
        // An empty explicit list reads the same as an absent one.
        assert_eq!(
            effective_egress(
                &json!([]),
                &json!({"enabled": true, "profiles": ["default"], "port": 20001})
            )["inherited"],
            true
        );
        // An explicit selection is the shed's own; no profile says audit: enforce.
        let e = effective_egress(
            &json!(["base"]),
            &json!({"enabled": true, "profiles": ["base"], "port": 20002, "rules": rules}),
        );
        assert_eq!(
            (e["inherited"].clone(), e["mode"].clone()),
            (json!(false), json!("enforce"))
        );
        // A profile whose definition did not come back: mode unknown.
        let e = effective_egress(
            &json!(["base", "gone"]),
            &json!({"enabled": true, "profiles": ["base", "gone"], "port": 20002, "rules": rules}),
        );
        assert_eq!(e["mode"], Value::Null);
        // Explicit profiles on a stopped shed whose port is not assigned yet: still restricted.
        assert_eq!(
            effective_egress(
                &json!(["base"]),
                &json!({"enabled": true, "profiles": ["base"]})
            )["restricted"],
            true
        );
    }

    #[test]
    fn effective_egress_is_unrestricted_only_when_nothing_applies() {
        // The switch on, no port and no profiles: egress was off for this shed.
        let e = effective_egress(&Value::Null, &json!({"shed": "s", "enabled": true}));
        assert_eq!(
            e,
            json!({"enabled": true, "restricted": false, "profiles": [],
                   "inherited": false, "mode": null})
        );
        // The switch off: nothing applies, whatever the shed's metadata still says.
        let e = effective_egress(
            &json!(["base"]),
            &json!({"enabled": false, "profiles": ["base"], "port": 20002}),
        );
        assert_eq!(
            (e["restricted"].clone(), e["profiles"].clone()),
            (json!(false), json!([]))
        );
        // A malformed answer is not read as restricted.
        assert_eq!(
            effective_egress(&Value::Null, &json!("nope"))["restricted"],
            false
        );
    }
}
