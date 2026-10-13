//! **The transcript composer's `/` menu** — the Tauri glue over
//! [`shed_app::slash_menu`] (plan 029 §3.5, §3.6 D12; P5).
//!
//! Every rule the menu has — which rows show, what a typed token matches,
//! what Enter, Tab or a click does, when a command is refused — is
//! `shed_app::slash_menu`'s, written once for both clients. The panel
//! (`LanePanel.tsx`) only renders and handles keys: it reads the session's
//! RESOLVED menu off `lane.messages` (`slash_menu`, cached by the view) and
//! hands it back here with the draft, through three pure commands —
//! `lane_slash_filter`, `lane_slash_dispatch` and `lane_slash_accept` —
//! exactly as the phone calls the same three functions over FRB. Nothing here
//! holds state or touches a lane: the verbs a dispatch leads to (`lane_send`,
//! `lane_set`, `lane_set_title`, `lane_stop`) are the lane's own commands.
//!
//! The menu crosses as the JSON `lane.messages` answered (the panel never
//! builds one), and so does the session's `activity`: whether a turn is
//! running is [`shed_app::slash_menu::is_working`]'s rule (P23), read here,
//! never re-derived in TypeScript. A per-keystroke invoke is the plan's
//! accepted cost (R1): local IPC, no I/O.
//!
//! A malformed argument is `bad_request`, under the lane commands' one
//! vocabulary ([`LaneFailure`]), so `bridge.ts` splits it like any other.

use serde_json::Value;
use shed_app::slash_menu::{self, SlashAcceptKey, SlashMenu};
use shed_core::rc::RcActivity;

use crate::lane::LaneFailure;

/// The resolved menu the panel hands back — `lane.messages`' `slash_menu`,
/// verbatim. Anything else is `bad_request`: a client that sends no menu has
/// no menu to filter, and must not have one guessed for it.
fn menu(value: Value) -> Result<SlashMenu, LaneFailure> {
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

/// `lane_slash_filter`: what the menu panel shows for `draft` with the caret
/// at `caret` (UTF-16 code units, the textarea's `selectionStart`) —
/// [`shed_app::SlashFilter`] as JSON.
pub fn filter(
    menu_json: Value,
    draft: &str,
    caret: u32,
    activity: Option<&str>,
) -> Result<Value, LaneFailure> {
    let menu = menu(menu_json)?;
    Ok(to_json(&slash_menu::filter(
        &menu,
        draft,
        caret,
        working(activity),
    )))
}

/// `lane_slash_dispatch`: what sending `draft` does — Send, and Enter on a
/// closed menu — as [`shed_app::SlashDispatch`] JSON (`not_command`,
/// `prompt {text}`, `run {action, arg?}`, `refuse {reason}`).
pub fn dispatch(
    menu_json: Value,
    draft: &str,
    activity: Option<&str>,
) -> Result<Value, LaneFailure> {
    let menu = menu(menu_json)?;
    Ok(to_json(&slash_menu::dispatch(
        &menu,
        draft,
        working(activity),
    )))
}

/// `lane_slash_accept`: what choosing the row named `row` with `key` does —
/// [`shed_app::SlashAccept`] JSON (`complete {draft, caret}` or `dispatch
/// {dispatch}`), or `null` when there is nothing to accept (the draft is no
/// command draft, or no shown row has that name: the menu moved under the
/// highlight, and the panel re-filters).
pub fn accept(
    menu_json: Value,
    draft: &str,
    row: &str,
    key: &str,
    activity: Option<&str>,
) -> Result<Value, LaneFailure> {
    let key = accept_key(key)?;
    let menu = menu(menu_json)?;
    Ok(to_json(&slash_menu::accept(
        &menu,
        draft,
        row,
        key,
        working(activity),
    )))
}

/// Every `shed_app::slash_menu` type is plain serde data; serialising one
/// cannot fail.
fn to_json<T: serde::Serialize>(value: &T) -> Value {
    serde_json::to_value(value).expect("a slash menu value serialises")
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    /// A session's resolved menu as `lane.messages` answers it: a prompt row,
    /// a rename, a mode that waits for the turn, and `/help` hidden.
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

    /// **The three commands answer `shed_app::slash_menu`'s values as JSON**,
    /// the shapes `bridge.ts` mirrors: a filter with its rows and `dimmed`
    /// while working; each dispatch kind; an accept that completes, one that
    /// runs at once, and `null` for a row the menu does not show.
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

    /// **A turn is `working` or `needs_approval`** (P23), read off the
    /// activity word; anything else — an unknown word, none — is not.
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

    /// **A malformed argument is `bad_request`**, never a guessed menu or key.
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
        assert_eq!(accept_key("tab").unwrap(), SlashAcceptKey::Tab);
        assert_eq!(accept_key("pick").unwrap(), SlashAcceptKey::Pick);
    }

    /// **No cap** (P16): a 600-row menu crosses, filters and dispatches — a
    /// bare `/` lists every row, in order.
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
