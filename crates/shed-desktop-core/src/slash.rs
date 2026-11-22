//! **The transcript composer's `/` menu** — JSON glue over
//! [`shed_lane::slash_menu`] (plan 029 §3.5, §3.6 D12; P5).
//!
//! Every rule the menu has — which rows show, what a typed token matches,
//! what Enter, Tab or a click does, when a command is refused — lives in
//! `shed_lane::slash_menu`, written once for every client. The UI reads the
//! session's RESOLVED menu off `lane.messages` (`slash_menu`, cached by the
//! view) and hands it back here with the draft, through [`filter`],
//! [`dispatch`], and [`accept`]. Nothing here holds state or touches a lane.
//!
//! A malformed argument is `bad_request`, under [`LaneFailure`].

use serde_json::Value;
use shed_lane::rc::RcActivity;
use shed_lane::slash_menu::{self, SlashAcceptKey, SlashMenu};

use crate::shed_lanes::LaneFailure;

/// The resolved menu the UI hands back — `lane.messages`' `slash_menu`,
/// verbatim. Anything else is `bad_request`.
fn parse_menu(value: Value) -> Result<SlashMenu, LaneFailure> {
    serde_json::from_value(value)
        .map_err(|e| LaneFailure::bad_request(format!("'menu' is not a slash menu: {e}")))
}

/// Whether a turn is running, from the session's activity as `lane.messages`
/// answered it (`working`, `needs_approval`, …; an unknown word or none is
/// not a turn) — [`slash_menu::is_working`]'s rule.
fn working(activity: Option<&str>) -> bool {
    slash_menu::is_working(RcActivity::from_wire(activity.unwrap_or_default()))
}

/// How a row was chosen: `enter`, `tab` or `pick` (a click).
fn accept_key(key: &str) -> Result<SlashAcceptKey, LaneFailure> {
    serde_json::from_value(Value::String(key.to_string())).map_err(|_| {
        LaneFailure::bad_request(format!("'key' must be enter, tab or pick, got {key:?}"))
    })
}

/// What the menu panel shows for `draft` with the caret at `caret` (UTF-16
/// code units, the textarea's `selectionStart`) — [`shed_lane::SlashFilter`]
/// as JSON.
pub fn filter(
    menu_json: Value,
    draft: &str,
    caret: u32,
    activity: Option<&str>,
) -> Result<Value, LaneFailure> {
    let menu = parse_menu(menu_json)?;
    Ok(to_json(&slash_menu::filter(
        &menu,
        draft,
        caret,
        working(activity),
    )))
}

/// What sending `draft` does — Send, and Enter on a closed menu — as
/// [`shed_lane::SlashDispatch`] JSON (`not_command`, `prompt {text}`,
/// `run {action, arg?}`, `refuse {reason}`).
pub fn dispatch(
    menu_json: Value,
    draft: &str,
    activity: Option<&str>,
) -> Result<Value, LaneFailure> {
    let menu = parse_menu(menu_json)?;
    Ok(to_json(&slash_menu::dispatch(
        &menu,
        draft,
        working(activity),
    )))
}

/// What choosing the row named `row` with `key` does —
/// [`shed_lane::SlashAccept`] JSON (`complete {draft, caret}` or
/// `dispatch {dispatch}`), or `null` when there is nothing to accept.
pub fn accept(
    menu_json: Value,
    draft: &str,
    row: &str,
    key: &str,
    activity: Option<&str>,
) -> Result<Value, LaneFailure> {
    let key = accept_key(key)?;
    let menu = parse_menu(menu_json)?;
    Ok(to_json(&slash_menu::accept(
        &menu,
        draft,
        row,
        key,
        working(activity),
    )))
}

/// Every `shed_lane::slash_menu` type is plain serde data; serialising one
/// cannot fail.
fn to_json<T: serde::Serialize>(value: &T) -> Value {
    serde_json::to_value(value).expect("a slash menu value serialises")
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    fn menu_json() -> Value {
        json!({
            "rows": [
                {"name": "research", "source": "agent", "arg": "optional", "while_working": false,
                 "action": {"kind": "prompt"}},
                {"name": "rename", "source": "builtin", "arg": "required", "arg_hint": "<title>",
                 "while_working": true, "action": {"kind": "set_title"}},
                {"name": "plan", "source": "builtin", "arg": "none", "while_working": false,
                 "action": {"kind": "set_mode", "change": {"kind": "mode", "id": "plan"}}}
            ],
            "hidden": [{"name": "help", "reason": "not available here yet"}]
        })
    }

    #[test]
    fn the_commands_answer_the_shared_rules_as_json() {
        let f = filter(menu_json(), "/re", 3, Some("idle")).unwrap();
        assert_eq!(f["open"], true);
        assert_eq!(f["token"], "/re");
        let names: Vec<&str> = f["rows"]
            .as_array()
            .unwrap()
            .iter()
            .map(|r| r["row"]["name"].as_str().unwrap())
            .collect();
        assert_eq!(
            names,
            ["research", "rename"],
            "both prefix matches, in menu order"
        );
        let f = filter(menu_json(), "/ame", 4, None).unwrap();
        assert_eq!(
            f["rows"][0]["row"]["name"], "rename",
            "a substring match: {f}"
        );
        assert_eq!(f["rows"].as_array().unwrap().len(), 1);

        // Caret at 0 is not in the token, so menu stays closed.
        let closed = filter(menu_json(), "/pl", 0, Some("idle")).unwrap();
        assert_eq!(closed["open"], false);

        let working_rows = filter(menu_json(), "/", 1, Some("working")).unwrap();
        let dimmed: Vec<(String, bool)> = working_rows["rows"]
            .as_array()
            .unwrap()
            .iter()
            .map(|r| {
                (
                    r["row"]["name"].as_str().unwrap().to_string(),
                    r["dimmed"].as_bool().unwrap(),
                )
            })
            .collect();
        assert_eq!(
            dimmed,
            [
                ("research".to_string(), false),
                ("rename".to_string(), false),
                ("plan".to_string(), true)
            ],
            "a prompt row queues and rename runs mid-turn; a mode waits"
        );

        assert_eq!(
            dispatch(menu_json(), "hello", None).unwrap(),
            json!({"kind": "not_command"})
        );
        assert_eq!(
            dispatch(menu_json(), "/Research  deep dive ", None).unwrap(),
            json!({"kind": "prompt", "text": "/research deep dive"}),
            "the row's own spelling, the argument trimmed (E8)"
        );
        assert_eq!(
            dispatch(menu_json(), " /rename  a  b \n", None).unwrap(),
            json!({"kind": "run", "action": {"kind": "set_title"}, "arg": "a  b"}),
            "the draft read as it would be sent (E9)"
        );
        assert_eq!(
            dispatch(menu_json(), "/help", None).unwrap(),
            json!({"kind": "refuse", "reason": "/help: not available here yet"})
        );
        assert_eq!(
            dispatch(menu_json(), "/plan", Some("needs_approval")).unwrap(),
            json!({"kind": "refuse", "reason": "/plan: runs after this turn"})
        );

        assert_eq!(
            accept(menu_json(), "/re", "rename", "enter", None).unwrap(),
            json!({"kind": "complete", "draft": "/rename ", "caret": 8})
        );
        assert_eq!(
            accept(menu_json(), "/re", "rename", "tab", Some("idle")).unwrap(),
            json!({"kind": "complete", "draft": "/rename ", "caret": 8})
        );
        assert_eq!(
            accept(menu_json(), "/pl", "plan", "pick", None).unwrap(),
            json!({"kind": "dispatch", "dispatch": {"kind": "run",
                   "action": {"kind": "set_mode", "change": {"kind": "mode", "id": "plan"}}}})
        );
        assert_eq!(
            accept(menu_json(), "/pl", "plan", "pick", Some("working")).unwrap(),
            json!({"kind": "dispatch", "dispatch": {"kind": "refuse",
                   "reason": "/plan: runs after this turn"}}),
            "a dimmed row selects to its refusal (X7)"
        );
        assert_eq!(
            accept(menu_json(), "/x", "nope", "tab", None).unwrap(),
            Value::Null
        );
    }

    #[test]
    fn an_effort_picker_crosses_and_its_level_is_a_set_effort() {
        let picker = json!({"kind": "effort_picker", "option": "effort", "for_model": "grok-4.6",
                            "levels": [{"id": "low", "name": "Low"}, {"id": "high", "name": "High"}],
                            "default_level": "high"});
        let effort_menu = json!({
            "rows": [{"name": "effort", "source": "builtin", "arg": "optional", "arg_hint": "[level]",
                      "while_working": false, "action": picker}],
            "hidden": []
        });

        assert_eq!(
            dispatch(effort_menu.clone(), "/effort", None).unwrap(),
            json!({"kind": "run", "action": picker}),
            "bare: the picker, every field across"
        );
        assert_eq!(
            accept(effort_menu.clone(), "/eff", "effort", "pick", None).unwrap(),
            json!({"kind": "dispatch", "dispatch": {"kind": "run", "action": picker}}),
            "a pick runs the completed draft"
        );
        assert_eq!(
            dispatch(effort_menu.clone(), "/effort LOW", None).unwrap(),
            json!({"kind": "run", "action": {"kind": "set_effort", "change": {
                "kind": "config", "id": "effort", "value": "low", "for_model": "grok-4.6"}}}),
            "a level, by id in any case, bound to the picker's model"
        );
        assert_eq!(
            dispatch(effort_menu.clone(), "/effort default", None).unwrap(),
            json!({"kind": "run", "action": {"kind": "set_effort", "change": {
                "kind": "config", "id": "effort", "value": "high", "for_model": "grok-4.6"}}}),
            "`default` is the model's default level"
        );
        assert_eq!(
            dispatch(effort_menu.clone(), "/effort bogus", None).unwrap(),
            json!({"kind": "refuse", "reason": "no effort bogus: one of default, low, high"}),
            "craze's own refusal text"
        );
        assert_eq!(
            dispatch(effort_menu, "/effort low", Some("working")).unwrap(),
            json!({"kind": "refuse", "reason": "/effort: runs after this turn"})
        );
    }

    #[test]
    fn working_is_the_shared_rule_on_the_activity_word() {
        assert!(working(Some("working")));
        assert!(working(Some("needs_approval")));
        for idle in [
            Some("idle"),
            Some("needs_input"),
            Some("unknown"),
            Some("busy"),
            None,
        ] {
            assert!(!working(idle), "{idle:?}");
        }
    }

    #[test]
    fn a_malformed_menu_or_key_is_a_bad_request() {
        for bad in [
            json!(null),
            json!("menu"),
            json!({"rows": "no"}),
            json!({"rows": [{"name": 7}]}),
        ] {
            assert_eq!(
                filter(bad.clone(), "/", 1, None).unwrap_err().code(),
                "bad_request",
                "{bad}"
            );
            assert_eq!(
                dispatch(bad.clone(), "/x", None).unwrap_err().code(),
                "bad_request"
            );
        }
        assert_eq!(
            accept(menu_json(), "/re", "rename", "Enter", None)
                .unwrap_err()
                .code(),
            "bad_request",
            "the key words are snake_case"
        );
        assert_eq!(
            accept(menu_json(), "/re", "rename", "sideways", None)
                .unwrap_err()
                .code(),
            "bad_request"
        );
        assert_eq!(accept_key("tab").unwrap(), SlashAcceptKey::Tab);
        assert_eq!(accept_key("pick").unwrap(), SlashAcceptKey::Pick);
    }

    #[test]
    fn a_six_hundred_row_menu_crosses_whole() {
        let rows: Vec<Value> = (0..600)
            .map(|i| {
                json!({"name": format!("cmd-{i:03}"), "source": "agent", "arg": "optional",
                       "while_working": false, "action": {"kind": "prompt"}})
            })
            .collect();
        let big = json!({ "rows": rows, "hidden": [] });
        let f = filter(big.clone(), "/", 1, None).unwrap();
        let shown = f["rows"].as_array().unwrap();
        assert_eq!(shown.len(), 600);
        assert_eq!(shown[599]["row"]["name"], "cmd-599");
        assert_eq!(
            dispatch(big, "/cmd-599 go", None).unwrap(),
            json!({"kind": "prompt", "text": "/cmd-599 go"})
        );
    }
}
