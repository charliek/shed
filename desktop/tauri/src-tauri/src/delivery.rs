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
//! | `shed.delivery` | `server`, `shed` | the shed document's delivery record, and the shed's effective egress |
//! | `shed.provision` | `server`, `shed` | the provision's progress and its outcome |
//! | `activity.server_list` | `limit`?, `cursor`?, `shed`? | the servers' activity, merged newest first |
//!
//! The runtime is this machine's only source: it holds this machine's identity and the
//! credentials-scope store (see `shed_desktop_core::delivery::runtime`).

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

/// Starts the source runtime: this machine's identity (`<state>/broker/machine.json`),
/// the fingerprint key under the same state directory, a stream per
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
        known_hosts: source::paths::expand_tilde(shed_desktop_core::control_mint::KNOWN_HOSTS),
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

/// The five providers, as `ui.*` doors name them.
const PROVIDERS: [&str; 5] = ["ssh-key", "github", "api-keys", "envsecrets-login", "files"];

fn provider_param(params: &Value) -> Result<&str, OpError> {
    let p = params.get("provider").and_then(Value::as_str).unwrap_or("");
    if PROVIDERS.contains(&p) {
        Ok(p)
    } else {
        Err(bad(format!(
            "'provider' is one of {}",
            PROVIDERS.join(", ")
        )))
    }
}

fn str_param<'a>(params: &'a Value, key: &str) -> Result<&'a str, OpError> {
    params
        .get(key)
        .and_then(Value::as_str)
        .filter(|s| !s.is_empty())
        .ok_or_else(|| bad(format!("missing '{key}'")))
}

/// Where a UI door's event goes: the dashboard (`Main`, raised first) or an open
/// Preferences window (`Prefs`).
#[derive(Debug, PartialEq, Eq)]
pub enum DoorTarget {
    Main,
    Prefs,
}

/// The provisioning UI's doors (plan 039 A7): the event to emit, its payload and where
/// it goes, or the refusal. Opening and closing a shed's detail and scrolling
/// Preferences to a section are what a person's click does, and are open; the doors
/// that type, press, preview or deliver are presses a harness has no pointer for, and
/// are TEST-MODE ONLY, the `ui.press_source_use` rule: each acts on the real control in
/// the open window (typing sets the input's value and fires its own input event), so
/// a disabled control takes no press.
///
/// | op | params | event |
/// |---|---|---|
/// | `ui.prefs_section` | `section` | `prefs-section` |
/// | `ui.show_shed_delivery` | `server`, `shed` | `show-shed-delivery` |
/// | `ui.close_shed_delivery` | — | `close-shed-delivery` |
/// | `ui.press_deliver_again` | `server`, `shed` | `press-deliver-again` (test) |
/// | `ui.press_delivery_toggle` | `provider` | `press-delivery-toggle` (test) |
/// | `ui.press_delivery_button` | `provider`, `button` | `press-delivery-button` (test) |
/// | `ui.type_delivery_field` | `provider`, `field`, `index`?, `value` | `type-delivery-field` (test) |
/// | `ui.preview_delivery` | `server`, `shed`? | `preview-delivery` (test) |
pub fn ui_door(
    test_mode: bool,
    op: &str,
    params: &Value,
) -> Result<(&'static str, Value, DoorTarget), OpError> {
    let gated = !matches!(
        op,
        "ui.prefs_section" | "ui.show_shed_delivery" | "ui.close_shed_delivery"
    );
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
        "ui.prefs_section" => Ok((
            "prefs-section",
            json!({ "section": str_param(params, "section")? }),
            DoorTarget::Prefs,
        )),
        "ui.show_shed_delivery" => Ok(("show-shed-delivery", shed_pair(params)?, DoorTarget::Main)),
        "ui.close_shed_delivery" => Ok(("close-shed-delivery", json!({}), DoorTarget::Main)),
        "ui.press_deliver_again" => {
            Ok(("press-deliver-again", shed_pair(params)?, DoorTarget::Main))
        }
        "ui.press_delivery_toggle" => Ok((
            "press-delivery-toggle",
            json!({ "provider": provider_param(params)? }),
            DoorTarget::Prefs,
        )),
        "ui.press_delivery_button" => Ok((
            "press-delivery-button",
            json!({ "provider": provider_param(params)?, "button": str_param(params, "button")? }),
            DoorTarget::Prefs,
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
                DoorTarget::Prefs,
            ))
        }
        "ui.preview_delivery" => {
            let server = str_param(params, "server")?;
            let shed = match params.get("shed") {
                None | Some(Value::Null) => "",
                Some(Value::String(s)) => s.as_str(),
                Some(_) => return Err(bad("'shed' must be a string")),
            };
            if !shed.is_empty() {
                shed_param(shed)?;
            }
            Ok((
                "preview-delivery",
                json!({ "server": server, "shed": shed }),
                DoorTarget::Prefs,
            ))
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
            "ui.press_delivery_toggle",
            "ui.press_delivery_button",
            "ui.type_delivery_field",
            "ui.preview_delivery",
            "ui.press_deliver_again",
        ] {
            let e = ui_door(false, op, &p).unwrap_err();
            assert_eq!(e.0, "not_enabled", "{op}");
        }
        let (ev, payload, to) = ui_door(
            false,
            "ui.prefs_section",
            &json!({"section": "provisioning"}),
        )
        .unwrap();
        assert_eq!((ev, to), ("prefs-section", DoorTarget::Prefs));
        assert_eq!(payload["section"], "provisioning");
        let (ev, payload, to) = ui_door(
            false,
            "ui.show_shed_delivery",
            &json!({"server": "mock", "shed": "w"}),
        )
        .unwrap();
        assert_eq!((ev, to), ("show-shed-delivery", DoorTarget::Main));
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
        assert_eq!(
            ui_door(
                true,
                "ui.press_delivery_toggle",
                &json!({"provider": "nope"})
            )
            .unwrap_err()
            .0,
            "bad_request"
        );
        let (_, typed, _) = ui_door(
            true,
            "ui.type_delivery_field",
            &json!({"provider": "ssh-key", "field": "only.repos", "index": 0, "value": "a/*"}),
        )
        .unwrap();
        assert_eq!(
            typed,
            json!({"provider": "ssh-key", "field": "only.repos", "index": 0, "value": "a/*"})
        );
        let (_, typed, _) = ui_door(
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
        ] {
            assert!(
                ui_door(true, "ui.type_delivery_field", &bad).is_err(),
                "{bad}"
            );
        }
        let (_, press, _) = ui_door(
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
        let (_, pv, _) = ui_door(true, "ui.preview_delivery", &json!({"server": "mock"})).unwrap();
        assert_eq!(pv, json!({"server": "mock", "shed": ""}));
        assert!(ui_door(
            true,
            "ui.preview_delivery",
            &json!({"server": "mock", "shed": "../x"})
        )
        .is_err());
        assert!(ui_door(
            true,
            "ui.preview_delivery",
            &json!({"server": "mock", "shed": 3})
        )
        .is_err());
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
