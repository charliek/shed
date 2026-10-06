//! The settings read (§3.10's DATA bullet), cell by cell.

use super::*;
use serde_json::json;

fn model(id: &str, recent: Option<u32>) -> CatalogModel {
    CatalogModel {
        id: id.into(),
        name: id.to_uppercase(),
        recent,
    }
}

fn catalogs(v: Value) -> Catalogs {
    serde_json::from_value(v).unwrap()
}

/// WIRE/01's info document and snapshot settings, as the contract's.
fn wire_01() -> SettingsState {
    let mut s = SettingsState::new();
    s.apply_catalogs(&catalogs(json!({
        "models": [{"id": "grok", "name": "Grok"}, {"id": "fast", "name": "Fast"}],
        "modes": [{"id": "agent", "name": "Agent", "description": "Full agent capabilities with tool access"},
                  {"id": "plan", "name": "Plan", "description": "Read-only mode"}]
    })));
    s.apply_sections(&SettingsSections::from_value(&json!({
        "mode": "agent", "model": "grok",
        "config": {"options": [
            {"id": "effort", "name": "Effort", "category": "thought_level", "type": "select", "current": "medium",
             "selectValues": [{"value": "low", "name": "Low"}, {"value": "medium", "name": "Medium"}, {"value": "high", "name": "High"}]},
            {"id": "fast", "name": "Fast", "category": "model_config", "type": "select", "current": "false",
             "selectValues": [{"value": "false", "name": "Off"}, {"value": "true", "name": "Fast"}]}
        ]},
        "commands": {"commands": [{"name": "research"}]}
    })));
    s
}

#[test]
fn the_info_document_and_the_snapshot_make_one_settings() {
    let s = wire_01().lane_settings();
    assert_eq!(s.model.as_deref(), Some("grok"));
    assert_eq!(s.mode.as_deref(), Some("agent"));
    let ids: Vec<&str> = s.models.iter().map(|m| m.id.as_str()).collect();
    assert_eq!(ids, ["grok", "fast"]);
    assert_eq!(
        s.modes[0].description.as_deref(),
        Some("Full agent capabilities with tool access")
    );
    let opts: Vec<(&str, &str)> = s
        .options
        .iter()
        .map(|o| (o.id.as_str(), o.current.as_str()))
        .collect();
    assert_eq!(opts, [("effort", "medium"), ("fast", "false")]);
    let values: Vec<(&str, &str)> = s.options[0]
        .values
        .iter()
        .map(|v| (v.id.as_str(), v.name.as_str()))
        .collect();
    assert_eq!(
        values,
        [("low", "Low"), ("medium", "Medium"), ("high", "High")],
        "{{id: value, name}}, the provider's order"
    );
    assert_eq!(s.usage, None, "an ACP session reports no usage");
    assert!(wire_01().has_any());
}

/// craze's model order: current first, the ranked by `recent`, the rest in
/// catalog order (WIRE/18's ranks, a current model in the middle).
#[test]
fn models_are_current_then_ranked_then_catalog_order() {
    let models = [
        model("a", None),
        model("muse", Some(2)),
        model("cur", None),
        model("kimi", Some(1)),
        model("z", None),
    ];
    let order: Vec<&str> = order_models(&models, Some("cur"))
        .iter()
        .map(|m| m.id.as_str())
        .collect();
    assert_eq!(order, ["cur", "kimi", "muse", "a", "z"]);
    // A current model that is itself ranked is first, not among the ranked.
    let order: Vec<&str> = order_models(&models, Some("muse"))
        .iter()
        .map(|m| m.id.as_str())
        .collect();
    assert_eq!(order, ["muse", "kimi", "a", "cur", "z"]);
    // No current model: the ranked, then the rest.
    let order: Vec<&str> = order_models(&models, None)
        .iter()
        .map(|m| m.id.as_str())
        .collect();
    assert_eq!(order, ["kimi", "muse", "a", "cur", "z"]);
}

/// Options: the model and mode rows excluded, `thought_level` first, then
/// `model_config`, then the rest — each group in the provider's order.
#[test]
fn options_are_ordered_by_category_and_exclude_the_model_and_mode_rows() {
    let mut s = SettingsState::new();
    s.apply_sections(&SettingsSections::from_value(
        &json!({"config": {"options": [
            {"id": "other1", "category": "x", "current": "a"},
            {"id": "model", "category": "model", "current": "m"},
            {"id": "fast", "category": "model_config", "current": "false"},
            {"id": "effort", "category": "thought_level", "current": "low"},
            {"id": "mode", "category": "mode", "current": "agent"},
            {"id": "ctx", "category": "model_config", "current": "1"},
            {"id": "other2", "category": "", "current": "b"}
        ]}}),
    ));
    let ids: Vec<String> = s
        .lane_settings()
        .options
        .into_iter()
        .map(|o| o.id)
        .collect();
    assert_eq!(ids, ["effort", "fast", "ctx", "other1", "other2"]);
}

/// Each present section replaces its own whole; an absent one is untouched.
#[test]
fn a_meta_delta_replaces_each_present_section_whole() {
    let mut s = wire_01();
    assert!(s.apply_sections(&SettingsSections::from_value(&json!({"model": "fast"}))));
    let l = s.lane_settings();
    assert_eq!(l.model.as_deref(), Some("fast"));
    assert_eq!(
        l.models[0].id, "fast",
        "the new current model is listed first"
    );
    assert_eq!(l.options.len(), 2, "config untouched");
    assert!(s.apply_sections(&SettingsSections::from_value(&json!({"config": {}}))));
    assert!(s.lane_settings().options.is_empty(), "{{}} is no options");
    assert!(
        !s.apply_sections(&SettingsSections::from_value(&json!({"title": "t"}))),
        "a section this crate does not read changes nothing"
    );
}

/// The catalog rule: an equal or higher revision applies; a lower one never
/// brings back a list the session moved past. The info document's list obeys
/// the same rule.
#[test]
fn a_catalog_applies_only_at_an_equal_or_higher_revision() {
    let mut s = wire_01();
    let cat = |models: Value, rev: u64| {
        SettingsSections::from_value(&json!({"catalog": {"models": models, "revision": rev}}))
    };
    assert!(s.apply_sections(&cat(json!([{"id": "k", "name": "K", "recent": 1}]), 2)));
    assert_eq!(s.revision(), 2);
    assert!(
        !s.apply_sections(&cat(json!([{"id": "old", "name": "Old"}]), 1)),
        "older: ignored"
    );
    assert!(
        !s.apply_sections(&cat(json!([{"id": "k", "name": "K", "recent": 1}]), 2)),
        "the same revision is the same list"
    );
    // The info document's revision-0 list never replaces revision 2's.
    assert!(!s.apply_catalogs(&catalogs(
        json!({"models": [{"id": "grok", "name": "Grok"}], "modes": [
        {"id": "agent", "name": "Agent", "description": "Full agent capabilities with tool access"},
        {"id": "plan", "name": "Plan", "description": "Read-only mode"}]})
    )));
    let ids: Vec<String> = s.lane_settings().models.into_iter().map(|m| m.id).collect();
    assert_eq!(ids, ["k"]);
}

/// Usage: tokens and window, a window of 0 read as unknown.
#[test]
fn usage_is_tokens_and_a_known_window() {
    let mut s = SettingsState::new();
    s.apply_sections(&SettingsSections::from_value(&json!({"usage": {
        "contextTokens": 68000, "contextWindow": 200000,
        "turn": {}, "session": {}}})));
    assert_eq!(
        s.lane_settings().usage,
        Some(LaneUsage {
            context_tokens: Some(68000),
            context_window: Some(200000)
        })
    );
    s.apply_sections(&SettingsSections::from_value(
        &json!({"usage": {"contextTokens": 5, "contextWindow": 0}}),
    ));
    assert_eq!(s.lane_settings().usage.unwrap().context_window, None);
}

/// No model, mode or option: nothing to show, so the capability says no.
#[test]
fn a_session_with_nothing_to_show_has_no_settings() {
    let mut s = SettingsState::new();
    assert!(!s.has_any());
    s.apply_sections(&SettingsSections::from_value(&json!({"model": "m",
        "config": {"options": [{"id": "model", "category": "model", "current": "m"}]}})));
    assert!(
        !s.has_any(),
        "a current model alone, with the model row excluded, shows nothing"
    );
    // A wrong-typed section reads as absent and costs nothing else.
    let sec = SettingsSections::from_value(&json!({"model": 5, "mode": "plan"}));
    assert_eq!((sec.model, sec.mode.as_deref()), (None, Some("plan")));
}

/// **Modes are hidden by capability, never shown disabled** (§3.10; PM
/// "Capabilities": a client hides what a capability says the session cannot
/// do): an info document whose capabilities lack `modes` takes the catalog's
/// modes out of what a client is shown — and a session with nothing else is
/// then a session with no settings at all.
#[test]
fn modes_are_hidden_when_the_capabilities_offer_none() {
    let info = |modes: bool| -> SessionInfo {
        serde_json::from_value(json!({
            "sessionId": "s", "hostId": "0123456789ab",
            "catalogs": {"models": [], "modes": [{"id": "agent", "name": "Agent"}, {"id": "plan", "name": "Plan"}]},
            "capabilities": {"modes": modes}
        }))
        .unwrap()
    };
    let mut s = SettingsState::new();
    assert!(s.apply_info(&info(true)));
    let ids: Vec<String> = s.lane_settings().modes.into_iter().map(|m| m.id).collect();
    assert_eq!(ids, ["agent", "plan"]);
    assert!(s.has_any());
    assert!(
        s.apply_info(&info(false)),
        "the capability's change is a change"
    );
    assert!(s.lane_settings().modes.is_empty(), "hidden, not listed");
    assert!(!s.has_any(), "modes it cannot switch are nothing to show");
    assert!(!s.apply_info(&info(false)), "the same document again");
}
