//! The `/` menu's rules, table by table (plan 029 §3.4, P5/P6/P9/P16/P23/P28).
//!
//! Rows are built in Rust from the contract's variants rather than decoded
//! from JSON: the catalog's wire words are the adapter's business (shed-craze
//! pins them), and this module only ever sees the decoded rows. The builders
//! below copy fixture 30/31's real rows (`crates/shed-craze/fixtures/wire/`)
//! and the protocol's `unavailable` illustration (plan amendment E3).

use super::*;

use serde_json::json;
use shed_core::lane::{LaneModelEffort, LaneSetting, LaneSlashRun};

// ---- builders ----

/// A session that can do everything this client offers.
fn all_caps() -> LaneCapabilities {
    LaneCapabilities {
        kind: "craze".to_string(),
        interject: false,
        cancel: true,
        approvals: true,
        history_cursor: true,
        settings: true,
        stop: true,
        set_title: true,
        slash: true,
    }
}

/// A session that cannot be stopped (cursor's).
fn no_stop() -> LaneCapabilities {
    LaneCapabilities {
        stop: false,
        ..all_caps()
    }
}

fn run(kind: LaneSlashKind) -> LaneSlashRun {
    LaneSlashRun {
        kind,
        arg: LaneSlashArg::None,
        arg_hint: None,
        method: None,
        param: None,
        mode: None,
        picker: None,
        local: None,
        reason: None,
    }
}

fn row(name: &str, source: LaneSlashSource, run: LaneSlashRun) -> LaneSlashRow {
    LaneSlashRow {
        name: name.to_string(),
        description: Some(format!("{name} description")),
        source,
        plugin: None,
        qualified: None,
        requires: None,
        while_working: false,
        run,
    }
}

fn working(mut r: LaneSlashRow) -> LaneSlashRow {
    r.while_working = true;
    r
}

fn requiring(mut r: LaneSlashRow, requires: LaneSlashRequires) -> LaneSlashRow {
    r.requires = Some(requires);
    r
}

fn local(name: &str) -> LaneSlashRow {
    working(row(
        name,
        LaneSlashSource::Builtin,
        LaneSlashRun {
            local: Some(name.to_string()),
            ..run(LaneSlashKind::Local)
        },
    ))
}

pub(crate) fn model() -> LaneSlashRow {
    row(
        "model",
        LaneSlashSource::Builtin,
        LaneSlashRun {
            arg: LaneSlashArg::Optional,
            arg_hint: Some("<model> [effort]".to_string()),
            picker: Some(LaneSlashPicker::Model),
            ..run(LaneSlashKind::Picker)
        },
    )
}

fn rename() -> LaneSlashRow {
    working(row(
        "rename",
        LaneSlashSource::Builtin,
        LaneSlashRun {
            arg: LaneSlashArg::Required,
            arg_hint: Some("<title>".to_string()),
            method: Some(LaneSlashMethod::SetTitle),
            param: Some("title".to_string()),
            ..run(LaneSlashKind::Method)
        },
    ))
}

fn mode(name: &str) -> LaneSlashRow {
    row(
        name,
        LaneSlashSource::Builtin,
        LaneSlashRun {
            method: Some(LaneSlashMethod::Set),
            mode: Some(name.to_string()),
            ..run(LaneSlashKind::Method)
        },
    )
}

/// The protocol's illustration of a mode the session did not advertise.
fn unavailable_ask() -> LaneSlashRow {
    row(
        "ask",
        LaneSlashSource::Builtin,
        LaneSlashRun {
            reason: Some("mode ask is not advertised".to_string()),
            ..run(LaneSlashKind::Unavailable)
        },
    )
}

fn btw_builtin() -> LaneSlashRow {
    requiring(
        working(row(
            "btw",
            LaneSlashSource::Builtin,
            LaneSlashRun {
                arg: LaneSlashArg::Required,
                arg_hint: Some("<question>".to_string()),
                param: Some("question".to_string()),
                ..run(LaneSlashKind::Aside)
            },
        )),
        LaneSlashRequires::Aside,
    )
}

fn exit() -> LaneSlashRow {
    requiring(
        working(row(
            "exit",
            LaneSlashSource::Builtin,
            LaneSlashRun {
                method: Some(LaneSlashMethod::Stop),
                ..run(LaneSlashKind::Method)
            },
        )),
        LaneSlashRequires::Stop,
    )
}

pub(crate) fn prompt(name: &str, source: LaneSlashSource) -> LaneSlashRow {
    row(
        name,
        source,
        LaneSlashRun {
            arg: LaneSlashArg::Optional,
            ..run(LaneSlashKind::Prompt)
        },
    )
}

/// craze 0.2.0's builtin `/effort` (fixture 30/31's row): a picker that
/// requires the session's effort, an optional `<level>`, no `whileWorking`.
pub(crate) fn effort() -> LaneSlashRow {
    requiring(
        row(
            "effort",
            LaneSlashSource::Builtin,
            LaneSlashRun {
                arg: LaneSlashArg::Optional,
                arg_hint: Some("<level>".to_string()),
                picker: Some(LaneSlashPicker::Effort),
                ..run(LaneSlashKind::Picker)
            },
        ),
        LaneSlashRequires::Effort,
    )
}

fn choice(id: &str, name: &str) -> LaneChoice {
    LaneChoice {
        id: id.to_string(),
        name: name.to_string(),
        rank: None,
        description: None,
        effort: None,
    }
}

/// A select option named by its id, its values `(id, name)`.
fn select(id: &str, category: &str, current: &str, values: &[(&str, &str)]) -> LaneSetting {
    LaneSetting {
        id: id.to_string(),
        name: id.to_string(),
        category: category.to_string(),
        current: current.to_string(),
        values: values.iter().map(|(v, n)| choice(v, n)).collect(),
    }
}

/// The permodel fake's grok-4.6 levels (cursor's capture).
const GROK_LEVELS: [(&str, &str); 4] = [
    ("low", "Low"),
    ("medium", "Medium"),
    ("high", "High"),
    ("xhigh", "Extra High"),
];

/// A session on `model` (none for `None`) whose options are `options`, its
/// model list the current model alone.
fn settings_on(model: Option<&str>, options: Vec<LaneSetting>) -> LaneSettings {
    LaneSettings {
        model: model.map(str::to_string),
        models: model.map(|m| choice(m, m)).into_iter().collect(),
        options,
        ..LaneSettings::default()
    }
}

/// The permodel fake's session on grok-4.6: its effort (`high`) and its fast
/// toggle — an ACP session, so no effort facts.
pub(crate) fn grok_46() -> LaneSettings {
    settings_on(
        Some("grok-4.6"),
        vec![
            select("effort", "thought_level", "high", &GROK_LEVELS),
            select(
                "fast",
                "model_config",
                "true",
                &[("false", "Off"), ("true", "Fast")],
            ),
        ],
    )
}

/// The same session moved to composer-2.5: fast alone, no effort.
pub(crate) fn composer_25() -> LaneSettings {
    settings_on(
        Some("composer-2.5"),
        vec![select(
            "fast",
            "model_config",
            "false",
            &[("false", "Off"), ("true", "Fast")],
        )],
    )
}

/// `settings` with effort facts on its current model: `default_effort` its
/// default level.
fn with_default(mut settings: LaneSettings, default_effort: &str) -> LaneSettings {
    let current = settings.model.clone();
    for m in settings
        .models
        .iter_mut()
        .filter(|m| Some(&m.id) == current.as_ref())
    {
        m.effort = Some(LaneModelEffort {
            efforts: vec!["low".into(), "high".into()],
            default_effort: Some(default_effort.to_string()),
            switch_effort: None,
        });
    }
    settings
}

fn plugin(name: &str, plugin: &str) -> LaneSlashRow {
    LaneSlashRow {
        plugin: Some(plugin.to_string()),
        qualified: Some(format!("{plugin}:{name}")),
        ..prompt(name, LaneSlashSource::Plugin)
    }
}

/// Fixture 30's fullest catalog (an ACP session: builtins, the agent's own
/// commands — its own `/btw` among them — and a disk skill), with the ask row
/// swapped for the protocol's `unavailable` illustration.
fn fixture_30() -> LaneSlashCatalog {
    LaneSlashCatalog {
        rows: vec![
            local("help"),
            model(),
            local("clear"),
            local("tasks"),
            LaneSlashRow {
                run: LaneSlashRun {
                    arg: LaneSlashArg::Optional,
                    arg_hint: Some("<name>".to_string()),
                    ..local("theme").run
                },
                ..local("theme")
            },
            rename(),
            mode("plan"),
            unavailable_ask(),
            mode("agent"),
            btw_builtin(),
            exit(),
            prompt("research", LaneSlashSource::Agent),
            prompt("review", LaneSlashSource::Agent),
            prompt("btw", LaneSlashSource::Agent),
            prompt("deploy", LaneSlashSource::Skill),
        ],
    }
}

/// Fixture 31's native catalog's tail: a plugin row with a second spelling,
/// and a plugin row whose name IS its qualified spelling (it collides with
/// the builtin `/help`).
fn fixture_31() -> LaneSlashCatalog {
    LaneSlashCatalog {
        rows: vec![
            local("help"),
            model(),
            local("connect"),
            rename(),
            exit(),
            prompt("research", LaneSlashSource::Agent),
            plugin("review", "pr-tools"),
            LaneSlashRow {
                plugin: Some("pr-tools".to_string()),
                ..prompt("pr-tools:help", LaneSlashSource::Plugin)
            },
        ],
    }
}

fn names(menu: &SlashMenu) -> Vec<&str> {
    menu.rows.iter().map(|r| r.name.as_str()).collect()
}

fn hidden(menu: &SlashMenu) -> Vec<(&str, &str)> {
    menu.hidden
        .iter()
        .map(|h| (h.name.as_str(), h.reason.as_str()))
        .collect()
}

fn filtered(f: &SlashFilter) -> Vec<&str> {
    f.rows.iter().map(|r| r.row.name.as_str()).collect()
}

fn set_mode(id: &str) -> SlashAction {
    SlashAction::SetMode {
        change: LaneSettingChange::Mode { id: id.to_string() },
    }
}

fn refused(reason: &str) -> SlashDispatch {
    SlashDispatch::Refuse {
        reason: reason.to_string(),
    }
}

fn run_action(action: SlashAction) -> SlashDispatch {
    SlashDispatch::Run { action, arg: None }
}

fn title(t: &str) -> SlashDispatch {
    SlashDispatch::Run {
        action: SlashAction::SetTitle,
        arg: Some(t.to_string()),
    }
}

fn prompt_text(text: &str) -> SlashDispatch {
    SlashDispatch::Prompt {
        text: text.to_string(),
    }
}

fn complete(draft: &str, caret: u32) -> Option<SlashAccept> {
    Some(SlashAccept::Complete {
        draft: draft.to_string(),
        caret,
    })
}

fn dispatched(dispatch: SlashDispatch) -> Option<SlashAccept> {
    Some(SlashAccept::Dispatch { dispatch })
}

// ---- resolve ----

/// One row alone against `caps` and `settings`: its action when shown; when
/// hidden, the reason `hidden` lists it with (`None` for a hidden
/// non-builtin, which is not listed).
fn verdict(
    r: LaneSlashRow,
    caps: &LaneCapabilities,
    settings: Option<&LaneSettings>,
) -> Result<SlashAction, Option<String>> {
    let menu = resolve(&LaneSlashCatalog { rows: vec![r] }, caps, settings);
    match (menu.rows.as_slice(), menu.hidden.as_slice()) {
        ([shown], []) => Ok(shown.action.clone()),
        ([], [h]) => Err(Some(h.reason.clone())),
        ([], []) => Err(None),
        other => panic!("one row resolved to {other:?}"),
    }
}

/// One action-table case: what it is, the row, the session, the verdict.
type Case<'a> = (
    &'a str,
    LaneSlashRow,
    &'a LaneCapabilities,
    Result<SlashAction, Option<String>>,
);

/// **Every row of §3.4's action table, and every hidden cause.**
#[test]
fn every_catalog_row_resolves_per_the_table() {
    let caps = all_caps();
    let no_stop = no_stop();
    let no_title = LaneCapabilities {
        set_title: false,
        ..all_caps()
    };
    let no_settings = LaneCapabilities {
        settings: false,
        ..all_caps()
    };
    let not_yet = Err(Some(NOT_YET.to_string()));

    let method = |m: Option<LaneSlashMethod>, param: Option<&str>, mode: Option<&str>| {
        row(
            "cmd",
            LaneSlashSource::Builtin,
            LaneSlashRun {
                method: m,
                param: param.map(str::to_string),
                mode: mode.map(str::to_string),
                ..run(LaneSlashKind::Method)
            },
        )
    };
    let picker = |p: Option<LaneSlashPicker>| {
        row(
            "cmd",
            LaneSlashSource::Builtin,
            LaneSlashRun {
                picker: p,
                ..run(LaneSlashKind::Picker)
            },
        )
    };

    let cases: Vec<Case> = vec![
        // prompt → Prompt, needing nothing.
        (
            "agent prompt",
            prompt("research", LaneSlashSource::Agent),
            &no_settings,
            Ok(SlashAction::Prompt),
        ),
        (
            "skill prompt",
            prompt("deploy", LaneSlashSource::Skill),
            &caps,
            Ok(SlashAction::Prompt),
        ),
        (
            "plugin prompt",
            plugin("review", "pr-tools"),
            &caps,
            Ok(SlashAction::Prompt),
        ),
        (
            "builtin prompt",
            prompt("init", LaneSlashSource::Builtin),
            &caps,
            Ok(SlashAction::Prompt),
        ),
        // method SetTitle, param title → SetTitle, needing set_title.
        ("rename", rename(), &caps, Ok(SlashAction::SetTitle)),
        (
            "rename, no set_title",
            rename(),
            &no_title,
            Err(Some(NOT_ADVERTISED.to_string())),
        ),
        (
            "set_title for another param",
            method(Some(LaneSlashMethod::SetTitle), Some("question"), None),
            &caps,
            not_yet.clone(),
        ),
        (
            "set_title with no param",
            method(Some(LaneSlashMethod::SetTitle), None, None),
            &caps,
            not_yet.clone(),
        ),
        // method Set with a mode → SetMode, needing nothing (P7: not settings).
        ("mode", mode("plan"), &caps, Ok(set_mode("plan"))),
        (
            "mode, no settings",
            mode("plan"),
            &no_settings,
            Ok(set_mode("plan")),
        ),
        (
            "set of no mode (a model setting)",
            method(Some(LaneSlashMethod::Set), None, None),
            &caps,
            not_yet.clone(),
        ),
        // method Stop → Stop, needing stop — with or without `requires`.
        ("exit", exit(), &caps, Ok(SlashAction::Stop)),
        (
            "exit, no stop",
            exit(),
            &no_stop,
            Err(Some(CANNOT_STOP.to_string())),
        ),
        (
            "stop without requires",
            method(Some(LaneSlashMethod::Stop), None, None),
            &caps,
            Ok(SlashAction::Stop),
        ),
        (
            "stop without requires, no stop",
            method(Some(LaneSlashMethod::Stop), None, None),
            &no_stop,
            Err(Some(CANNOT_STOP.to_string())),
        ),
        (
            "method with no method",
            method(None, None, None),
            &caps,
            not_yet.clone(),
        ),
        (
            "an unknown method",
            method(
                Some(LaneSlashMethod::Other("session.compact".into())),
                None,
                None,
            ),
            &caps,
            not_yet.clone(),
        ),
        // picker Model → ModelPicker, needing settings.
        ("model", model(), &caps, Ok(SlashAction::ModelPicker)),
        (
            "model, no settings",
            model(),
            &no_settings,
            Err(Some(NOT_ADVERTISED.to_string())),
        ),
        (
            "an unknown picker",
            picker(Some(LaneSlashPicker::Other("theme".into()))),
            &caps,
            not_yet.clone(),
        ),
        (
            "a picker with none named",
            picker(None),
            &caps,
            not_yet.clone(),
        ),
        // aside, local, unavailable, anything else → hidden.
        ("aside", btw_builtin(), &caps, not_yet.clone()),
        ("local", local("help"), &caps, not_yet.clone()),
        (
            "unavailable, with its reason",
            unavailable_ask(),
            &caps,
            Err(Some("mode ask is not advertised".to_string())),
        ),
        (
            "unavailable, no reason",
            row(
                "ask",
                LaneSlashSource::Builtin,
                run(LaneSlashKind::Unavailable),
            ),
            &caps,
            Err(Some(NOT_ADVERTISED.to_string())),
        ),
        (
            "a run with no kind",
            row(
                "cmd",
                LaneSlashSource::Builtin,
                run(LaneSlashKind::Other(String::new())),
            ),
            &caps,
            not_yet.clone(),
        ),
        (
            "an unknown kind",
            row(
                "cmd",
                LaneSlashSource::Builtin,
                run(LaneSlashKind::Other("macro".into())),
            ),
            &caps,
            not_yet.clone(),
        ),
        // requires: stop needs stop; aside is deferred whatever the caps; an
        // unknown one hides.
        (
            "prompt requiring stop",
            requiring(
                prompt("bye", LaneSlashSource::Builtin),
                LaneSlashRequires::Stop,
            ),
            &caps,
            Ok(SlashAction::Prompt),
        ),
        (
            "prompt requiring stop, no stop",
            requiring(
                prompt("bye", LaneSlashSource::Builtin),
                LaneSlashRequires::Stop,
            ),
            &no_stop,
            Err(Some(CANNOT_STOP.to_string())),
        ),
        (
            "prompt requiring aside, every cap",
            requiring(
                prompt("ask", LaneSlashSource::Builtin),
                LaneSlashRequires::Aside,
            ),
            &caps,
            not_yet.clone(),
        ),
        (
            "an unknown requires",
            requiring(
                prompt("x", LaneSlashSource::Builtin),
                LaneSlashRequires::Other("vision".into()),
            ),
            &caps,
            not_yet.clone(),
        ),
        // Any other value this build does not know hides the row.
        (
            "an unknown arg",
            row(
                "cmd",
                LaneSlashSource::Builtin,
                LaneSlashRun {
                    arg: LaneSlashArg::Other("many".into()),
                    ..run(LaneSlashKind::Prompt)
                },
            ),
            &caps,
            not_yet.clone(),
        ),
        // A name or spelling no token could hold: hidden — and a name no
        // token could spell is not listed, since no draft could type it.
        (
            "a name with a space",
            prompt("two words", LaneSlashSource::Builtin),
            &caps,
            Err(None),
        ),
        (
            "an empty name",
            prompt("", LaneSlashSource::Builtin),
            &caps,
            Err(None),
        ),
        (
            "a qualified with a space",
            LaneSlashRow {
                qualified: Some("pr tools:review".into()),
                ..prompt("review", LaneSlashSource::Builtin)
            },
            &caps,
            not_yet.clone(),
        ),
        // A hidden NON-builtin is hidden and not listed.
        (
            "an unknown source",
            prompt("x", LaneSlashSource::Other("mcp".into())),
            &caps,
            Err(None),
        ),
        (
            "a missing source",
            prompt("x", LaneSlashSource::Other(String::new())),
            &caps,
            Err(None),
        ),
        (
            "an agent row needing aside",
            requiring(
                prompt("x", LaneSlashSource::Agent),
                LaneSlashRequires::Aside,
            ),
            &caps,
            Err(None),
        ),
        (
            "an agent's local row",
            LaneSlashRow {
                source: LaneSlashSource::Agent,
                ..local("help")
            },
            &caps,
            Err(None),
        ),
    ];
    for (what, r, caps, want) in cases {
        assert_eq!(verdict(r, caps, None), want, "{what}");
    }
}

/// **Fixture 30 resolved**: the order kept, the hidden builtins listed with
/// their reasons, and the agent's `/btw` shown where the builtin is hidden.
#[test]
fn fixture_30_resolves_to_its_menu() {
    let menu = resolve(&fixture_30(), &all_caps(), None);
    assert_eq!(
        names(&menu),
        ["model", "rename", "plan", "agent", "exit", "research", "review", "btw", "deploy"]
    );
    assert_eq!(
        hidden(&menu),
        [
            ("help", NOT_YET),
            ("clear", NOT_YET),
            ("tasks", NOT_YET),
            ("theme", NOT_YET),
            ("ask", "mode ask is not advertised"),
        ],
        "btw is not listed: the agent's own answers to it"
    );
    let btw = &menu.rows[7];
    assert_eq!(
        (btw.source.clone(), btw.action.clone()),
        (LaneSlashSource::Agent, SlashAction::Prompt)
    );
    // Display fields ride along.
    let rename = &menu.rows[1];
    assert_eq!(rename.arg, LaneSlashArg::Required);
    assert_eq!(rename.arg_hint.as_deref(), Some("<title>"));
    assert!(rename.while_working);
    assert_eq!(rename.description.as_deref(), Some("rename description"));

    // A cursor-like session: no stop. /exit hides and says why.
    let menu = resolve(&fixture_30(), &no_stop(), None);
    assert!(!names(&menu).contains(&"exit"));
    assert!(hidden(&menu).contains(&("exit", CANNOT_STOP)));
}

/// **A `requires` row claims no name** (A1): the cursor case — the builtin
/// `/btw` needs `aside`, is hidden, and the agent's `/btw` listed after it
/// shows. On a session with every capability it is STILL hidden in phase 1
/// (X1: no aside verb yet), so the agent's still wins.
#[test]
fn a_requires_row_claims_no_name() {
    let catalog = LaneSlashCatalog {
        rows: vec![
            btw_builtin(),
            exit(),
            prompt("btw", LaneSlashSource::Agent),
            prompt("exit", LaneSlashSource::Agent),
        ],
    };
    let menu = resolve(&catalog, &no_stop(), None);
    assert_eq!(names(&menu), ["btw", "exit"]);
    assert!(menu.rows.iter().all(|r| r.source == LaneSlashSource::Agent));
    assert!(menu.hidden.is_empty(), "{:?}", menu.hidden);

    // With stop, the builtin /exit shows and the agent's is the duplicate.
    let menu = resolve(&catalog, &all_caps(), None);
    assert_eq!(names(&menu), ["exit", "btw"]);
    assert_eq!(menu.rows[0].action, SlashAction::Stop);
    assert_eq!(menu.rows[1].source, LaneSlashSource::Agent);
}

/// **Per name the first SHOWN row wins**, names folded with Unicode
/// lowercase — not ASCII-only.
#[test]
fn the_first_shown_row_of_a_name_wins_unicode_folded() {
    let catalog = LaneSlashCatalog {
        rows: vec![
            local("ÜNÏCODE"),
            prompt("Ünïcode", LaneSlashSource::Agent),
            prompt("ünïcode", LaneSlashSource::Skill),
            prompt("Deploy", LaneSlashSource::Agent),
            prompt("deploy", LaneSlashSource::Skill),
        ],
    };
    let menu = resolve(&catalog, &all_caps(), None);
    assert_eq!(names(&menu), ["Ünïcode", "Deploy"]);
    assert!(menu.rows.iter().all(|r| r.source == LaneSlashSource::Agent));
    assert!(
        menu.hidden.is_empty(),
        "the hidden ÜNÏCODE builtin's name is answered by the agent's Ünïcode"
    );
}

/// `hidden` lists each name once (the first reason), and never a name a
/// shown row answers to by either spelling.
#[test]
fn hidden_lists_each_refused_name_once() {
    let catalog = LaneSlashCatalog {
        rows: vec![
            unavailable_ask(),
            local("ASK"),
            local("pr-tools:review"),
            plugin("review", "pr-tools"),
        ],
    };
    let menu = resolve(&catalog, &all_caps(), None);
    assert_eq!(names(&menu), ["review"]);
    assert_eq!(hidden(&menu), [("ask", "mode ask is not advertised")]);
}

/// **No cap** (P16): a 600-row catalog resolves whole and filters whole.
#[test]
fn a_600_row_catalog_resolves_and_filters() {
    let catalog = LaneSlashCatalog {
        rows: (0..600)
            .map(|i| prompt(&format!("cmd{i:03}"), LaneSlashSource::Agent))
            .collect(),
    };
    let menu = resolve(&catalog, &all_caps(), None);
    assert_eq!(menu.rows.len(), 600);
    assert_eq!(menu.rows[599].name, "cmd599");
    let all = filter(&menu, "/", 1, false);
    assert!(all.open);
    assert_eq!(all.rows.len(), 600);
    let some = filter(&menu, "/cmd5", 5, false);
    assert_eq!(some.rows.len(), 100);
    assert_eq!(some.rows[0].row.name, "cmd500");
    assert_eq!(
        dispatch(&menu, "/CMD599 go", false),
        prompt_text("/cmd599 go")
    );
}

// ---- filter ----

/// A bare `/` lists every row, in menu order.
#[test]
fn a_bare_slash_lists_every_row() {
    let menu = resolve(&fixture_30(), &all_caps(), None);
    let f = filter(&menu, "/", 1, false);
    assert!(f.open);
    assert_eq!(f.token, "/");
    assert_eq!(filtered(&f), names(&menu));
}

/// **craze's four buckets**: name prefix, qualified-only prefix, name
/// substring, qualified-only substring — each in menu order, each row in its
/// first bucket only, case-insensitive.
#[test]
fn matches_come_in_four_buckets() {
    let catalog = LaneSlashCatalog {
        rows: vec![
            plugin("fmt", "score"),                       // qualified substring
            prompt("prereq", LaneSlashSource::Agent),     // name substring
            plugin("lint", "review-kit"),                 // qualified prefix
            prompt("Rename-all", LaneSlashSource::Agent), // name prefix
            plugin("research", "core"),                   // name prefix (its first)
            prompt("other", LaneSlashSource::Agent),      // no match
        ],
    };
    let menu = resolve(&catalog, &all_caps(), None);
    let want = ["Rename-all", "research", "lint", "prereq", "fmt"];
    assert_eq!(filtered(&filter(&menu, "/re", 3, false)), want);
    assert_eq!(filtered(&filter(&menu, "/RE", 3, false)), want);
    assert_eq!(
        filtered(&filter(&menu, "/review-kit:", 12, false)),
        ["lint"]
    );
}

/// **P28's open rule**: the caret must sit inside the first token — after the
/// `/`, at most at its end. Before the `/`, or past the first whitespace, the
/// panel is down.
#[test]
fn the_menu_opens_only_with_the_caret_in_the_first_token() {
    let menu = resolve(&fixture_30(), &all_caps(), None);
    let draft = "/re foo";
    for (caret, open) in [
        (0, false), // before the `/`
        (1, true),  // right after it
        (2, true),  // inside the token
        (3, true),  // at its end
        (4, false), // after the first space
        (7, false), // at the draft's end
    ] {
        let f = filter(&menu, draft, caret, false);
        assert_eq!(f.open, open, "caret {caret}");
        assert_eq!(f.token, "/re", "caret {caret}");
        assert_eq!(f.rows.is_empty(), !open, "caret {caret}");
    }
    assert_eq!(
        filtered(&filter(&menu, draft, 3, false)),
        ["rename", "research", "review"]
    );
    // A caret past the end is read as the end.
    assert!(filter(&menu, "/re", 99, false).open);
}

/// Only a one-line draft whose first character is `/` opens the menu.
#[test]
fn only_a_one_line_slash_draft_opens() {
    let menu = resolve(&fixture_30(), &all_caps(), None);
    for (draft, caret) in [
        ("re", 1),
        (" /re", 3),
        ("/re\nmore", 2),
        ("/re\r", 2),
        ("text /re", 7),
        ("", 0),
    ] {
        assert_eq!(
            filter(&menu, draft, caret, false),
            SlashFilter::default(),
            "{draft:?}"
        );
    }
}

/// No matching row, or no rows at all, is a closed menu — so Enter sends.
#[test]
fn no_match_is_a_closed_menu() {
    let menu = resolve(&fixture_30(), &all_caps(), None);
    let f = filter(&menu, "/zzz", 4, false);
    assert!(!f.open);
    assert!(f.rows.is_empty());
    assert_eq!(f.token, "/zzz", "the token still keys an Esc dismissal");

    let nothing = resolve(
        &LaneSlashCatalog {
            rows: vec![local("help")],
        },
        &all_caps(),
        None,
    );
    assert!(nothing.rows.is_empty());
    assert!(!filter(&nothing, "/", 1, false).open);
}

/// **While working the rows stay listed** (X7), and the ones a turn would
/// refuse come back dimmed: not a `while_working` row, not a prompt (which
/// queues).
#[test]
fn rows_that_wait_for_the_turn_are_dimmed_while_working() {
    let menu = resolve(&fixture_30(), &all_caps(), None);
    let dimmed = |working: bool| -> Vec<(String, bool)> {
        filter(&menu, "/", 1, working)
            .rows
            .into_iter()
            .map(|r| (r.row.name, r.dimmed))
            .collect()
    };
    let while_working: Vec<(String, bool)> = [
        ("model", true),
        ("rename", false),
        ("plan", true),
        ("agent", true),
        ("exit", false),
        ("research", false),
        ("review", false),
        ("btw", false),
        ("deploy", false),
    ]
    .into_iter()
    .map(|(n, d)| (n.to_string(), d))
    .collect();
    assert_eq!(dimmed(true), while_working);
    assert!(dimmed(false).iter().all(|(_, d)| !d), "idle dims nothing");
}

/// Where a filter's highlight starts (plan 030 FU8, §3.4): its index, its
/// row's name, and whether that row is dimmed.
type Started<'a> = (u32, Option<&'a str>, Option<bool>);

/// [`Started`] for `f`.
fn started(f: &SlashFilter) -> Started<'_> {
    let row = f.rows.get(f.start as usize);
    (
        f.start,
        row.map(|r| r.row.name.as_str()),
        row.map(|r| r.dimmed),
    )
}

/// **The highlight starts on the first row that is not dimmed** (plan 030
/// FU8, §3.4): idle that is the first row; while working it is the first
/// row the turn does not refuse, WHATEVER its action — a `while_working`
/// row (`rename`, `exit`) counts as much as a prompt row, which queues.
#[test]
fn the_highlight_starts_on_the_first_row_that_is_not_dimmed() {
    let menu = resolve(&fixture_30(), &all_caps(), None);
    // Idle: nothing is dimmed, so the first row, `model`.
    assert_eq!(
        started(&filter(&menu, "/", 1, false)),
        (0, Some("model"), Some(false))
    );
    // Working: `model` (a picker) is dimmed above `rename` (a while-working
    // SetTitle) — the case the desktop's first live row is.
    let f = filter(&menu, "/", 1, true);
    assert!(f.rows[0].dimmed, "the first row is dimmed: not vacuous");
    assert_eq!(started(&f), (1, Some("rename"), Some(false)));

    // Further down, and each kind of live row.
    let catalog = |rows: Vec<LaneSlashRow>| resolve(&LaneSlashCatalog { rows }, &all_caps(), None);
    let cases: [(&str, Vec<LaneSlashRow>, Started); 3] = [
        (
            "a prompt row, which queues",
            vec![
                mode("plan"),
                mode("agent"),
                model(),
                prompt("research", LaneSlashSource::Agent),
                rename(),
            ],
            (3, Some("research"), Some(false)),
        ),
        (
            "a while-working Stop",
            vec![mode("plan"), exit(), rename()],
            (1, Some("exit"), Some(false)),
        ),
        (
            "a while-working SetTitle",
            vec![model(), mode("plan"), mode("agent"), rename()],
            (3, Some("rename"), Some(false)),
        ),
    ];
    for (what, rows, want) in cases {
        let menu = catalog(rows);
        assert_eq!(started(&filter(&menu, "/", 1, true)), want, "{what}");
        assert_eq!(started(&filter(&menu, "/", 1, false)).0, 0, "{what}, idle");
    }
    // The filter's own order decides, not the catalog's: `/re` lists
    // `rename` before `research` (both live).
    let f = filter(&menu, "/re", 3, true);
    assert_eq!(filtered(&f), ["rename", "research", "review"]);
    assert_eq!(started(&f), (0, Some("rename"), Some(false)));
}

/// **Every row dimmed → 0**: there is no live row to start on, so the
/// highlight starts where it always did — and Enter there is refused, as
/// it would be on any of them.
#[test]
fn every_row_dimmed_starts_on_the_first() {
    let menu = resolve(
        &LaneSlashCatalog {
            rows: vec![model(), mode("plan"), mode("agent")],
        },
        &all_caps(),
        None,
    );
    let f = filter(&menu, "/", 1, true);
    assert!(f.open);
    assert!(f.rows.iter().all(|r| r.dimmed), "every row dimmed");
    assert_eq!(started(&f), (0, Some("model"), Some(true)));
}

/// **A closed menu, or no command at all, starts at 0**: no rows, nothing to
/// highlight.
#[test]
fn a_closed_filter_starts_at_zero() {
    let menu = resolve(&fixture_30(), &all_caps(), None);
    for (draft, caret) in [
        ("/zzz", 4),    // no match
        ("/re foo", 7), // the caret past the token
        ("hello", 5),   // not a command
        ("/re\nmore", 2),
        ("", 0),
    ] {
        let f = filter(&menu, draft, caret, true);
        assert!(!f.open, "{draft:?}");
        assert_eq!(f.start, 0, "{draft:?}");
    }
    assert_eq!(SlashFilter::default().start, 0);
}

/// **`start` survives the JSON** the desktop reads (`lane_slash_filter`),
/// and a filter written before it existed reads as 0.
#[test]
fn the_start_survives_serde() {
    let menu = resolve(&fixture_30(), &all_caps(), None);
    let f = filter(&menu, "/", 1, true);
    assert_eq!(f.start, 1);
    let v = serde_json::to_value(&f).unwrap();
    assert_eq!(v["start"], json!(1));
    assert_eq!(serde_json::from_value::<SlashFilter>(v).unwrap(), f);
    let old: SlashFilter =
        serde_json::from_value(json!({"open": false, "token": "/zzz", "rows": []})).unwrap();
    assert_eq!(old.start, 0);
}

/// **Carets are UTF-16 code units**: a non-BMP character in the token counts
/// two.
#[test]
fn the_caret_counts_utf16_code_units() {
    let menu = resolve(
        &LaneSlashCatalog {
            rows: vec![prompt("😀go", LaneSlashSource::Agent)],
        },
        &all_caps(),
        None,
    );
    // "/😀g x": the token "/😀g" is 1 + 2 + 1 = 4 units long.
    assert!(filter(&menu, "/😀g x", 4, false).open);
    assert!(!filter(&menu, "/😀g x", 5, false).open);
    assert_eq!(filter(&menu, "/😀g x", 4, false).token, "/😀g");
}

// ---- dispatch ----

/// **The draft as it would be sent** (P5): both clients trim before sending,
/// so the grammar does too — a leading space or a trailing newline must not
/// carry a hidden builtin out as text, nor stop a real command from running.
#[test]
fn the_grammar_reads_the_draft_as_it_would_be_sent() {
    let menu = resolve(&fixture_30(), &all_caps(), None);
    for draft in [" /help", "/help\n", "  /help \r\n", "\n/help"] {
        assert!(
            matches!(dispatch(&menu, draft, false), SlashDispatch::Refuse { .. }),
            "{draft:?} must be refused, never sent as text"
        );
    }
    assert_eq!(
        dispatch(&menu, "  /rename plan 029 live \n", false),
        title("plan 029 live")
    );
}

/// What is NOT a command goes out as text, as it would without a menu.
#[test]
fn not_commands_are_sent_as_text() {
    let menu = resolve(&fixture_30(), &all_caps(), None);
    for draft in [
        "",
        "hello",
        "/",                 // a bare slash names nothing
        "/rename x\nsecond", // multi-line is prompt text (P6)
        "/rename x\r\nsecond",
        "/nosuch arg", // matches no row and no hidden builtin
        "hello /rename x",
    ] {
        assert_eq!(
            dispatch(&menu, draft, false),
            SlashDispatch::NotCommand,
            "{draft:?}"
        );
    }
    // A builtin with an empty name (no adapter emits one) is no refusal for a
    // bare `/`.
    let odd = resolve(
        &LaneSlashCatalog {
            rows: vec![local("")],
        },
        &all_caps(),
        None,
    );
    assert_eq!(dispatch(&odd, "/", false), SlashDispatch::NotCommand);
}

/// **The grammar** (P6): the token to the first whitespace; the argument the
/// rest, trimmed, literal — inner whitespace and quotes kept.
#[test]
fn the_argument_is_the_rest_trimmed_and_literal() {
    let menu = resolve(&fixture_30(), &all_caps(), None);
    assert_eq!(dispatch(&menu, "/rename   x  y ", false), title("x  y"));
    assert_eq!(dispatch(&menu, "/rename \"a b\"", false), title("\"a b\""));
    assert_eq!(
        dispatch(&menu, "/rename\tplan 029", false),
        title("plan 029")
    );
    assert_eq!(
        dispatch(&menu, "/exit   ", false),
        run_action(SlashAction::Stop)
    );
    assert_eq!(
        dispatch(&menu, "/research  a  b ", false),
        prompt_text("/research a  b")
    );
    assert_eq!(
        dispatch(&menu, "/research", false),
        prompt_text("/research")
    );
}

/// Every action runs.
#[test]
fn each_action_dispatches() {
    let menu = resolve(&fixture_30(), &all_caps(), None);
    assert_eq!(
        dispatch(&menu, "/rename plan 029 live", false),
        title("plan 029 live")
    );
    assert_eq!(
        dispatch(&menu, "/plan", false),
        run_action(set_mode("plan"))
    );
    assert_eq!(
        dispatch(&menu, "/exit", false),
        run_action(SlashAction::Stop)
    );
    assert_eq!(
        dispatch(&menu, "/model", false),
        run_action(SlashAction::ModelPicker)
    );
    assert_eq!(
        dispatch(&menu, "/deploy prod", false),
        prompt_text("/deploy prod")
    );
    assert_eq!(
        dispatch(&menu, "/btw why?", false),
        prompt_text("/btw why?")
    );
}

/// **The guard** (P5): every refusal, each with its reason.
#[test]
fn the_guard_refuses_with_a_reason() {
    let menu = resolve(&fixture_30(), &all_caps(), None);
    let cursor = resolve(&fixture_30(), &no_stop(), None);
    let cases = [
        // A hidden builtin, typed: refused, never sent as text (E1: /help).
        (&menu, "/help", false, "/help: not available here yet"),
        (&menu, "/help me", false, "/help: not available here yet"),
        (
            &menu,
            "/theme dark",
            false,
            "/theme: not available here yet",
        ),
        (
            &cursor,
            "/exit",
            false,
            "/exit: this session cannot be stopped",
        ),
        (&menu, "/ask", false, "/ask: mode ask is not advertised"),
        // An argument to a no-argument action, or to /model (X2).
        (&menu, "/plan now", false, "/plan: takes no argument"),
        (&menu, "/exit now", false, "/exit: takes no argument"),
        (
            &menu,
            "/model gpt-5 high",
            false,
            "/model: takes no argument — pick a model from the list",
        ),
        // An empty title.
        (&menu, "/rename", false, "type a title after /rename"),
        (&menu, "/rename    ", false, "type a title after /rename"),
        // A row that waits for the turn, during one.
        (&menu, "/plan", true, "/plan: runs after this turn"),
        (&menu, "/model", true, "/model: runs after this turn"),
        // The argument is judged before the turn.
        (&menu, "/plan x", true, "/plan: takes no argument"),
    ];
    for (menu, draft, working, reason) in cases {
        assert_eq!(dispatch(menu, draft, working), refused(reason), "{draft:?}");
    }
}

/// While working, the `while_working` rows run and a prompt row queues.
#[test]
fn while_working_rows_run_and_prompts_queue() {
    let menu = resolve(&fixture_30(), &all_caps(), None);
    assert_eq!(dispatch(&menu, "/rename mid-turn", true), title("mid-turn"));
    assert_eq!(
        dispatch(&menu, "/exit", true),
        run_action(SlashAction::Stop)
    );
    assert_eq!(
        dispatch(&menu, "/research more", true),
        prompt_text("/research more")
    );
}

/// **Case folding and the second spelling**: a token matches `name` then
/// `qualified`, case-insensitively (Unicode), and a prompt is sent with the
/// row's own spelling.
#[test]
fn tokens_match_either_spelling_case_insensitively() {
    let mut catalog = fixture_31();
    catalog.rows.push(prompt("ünïcode", LaneSlashSource::Agent));
    let menu = resolve(&catalog, &all_caps(), None);
    assert_eq!(dispatch(&menu, "/RENAME x", false), title("x"));
    assert_eq!(
        dispatch(&menu, "/Ünïcode q", false),
        prompt_text("/ünïcode q")
    );
    assert_eq!(dispatch(&menu, "/ÜNÏCODE", false), prompt_text("/ünïcode"));
    assert_eq!(
        dispatch(&menu, "/PR-TOOLS:Review x", false),
        prompt_text("/pr-tools:review x")
    );
    assert_eq!(
        dispatch(&menu, "/Review x", false),
        prompt_text("/review x")
    );
    assert_eq!(
        dispatch(&menu, "/HELP", false),
        refused("/help: not available here yet")
    );
    // Fixture 31's plugin help: its NAME is the qualified spelling (it
    // collides with the builtin), so it runs where the builtin is refused.
    assert_eq!(
        dispatch(&menu, "/pr-tools:help", false),
        prompt_text("/pr-tools:help")
    );
    assert_eq!(
        dispatch(&menu, "/connect", false),
        refused("/connect: not available here yet")
    );
}

// ---- accept ----

/// **Enter on a row not yet spelled selects it**: a row taking an argument
/// completes, taking in one space that followed and keeping the argument.
#[test]
fn enter_completes_a_row_not_yet_spelled() {
    let menu = resolve(&fixture_30(), &all_caps(), None);
    let enter = |draft: &str, row: &str| accept(&menu, draft, row, SlashAcceptKey::Enter, false);
    assert_eq!(enter("/re", "rename"), complete("/rename ", 8));
    assert_eq!(enter("/re foo", "rename"), complete("/rename foo", 8));
    assert_eq!(enter("/re  foo", "rename"), complete("/rename  foo", 8));
    assert_eq!(enter("/re\tfoo", "rename"), complete("/rename \tfoo", 8));
    assert_eq!(enter("/res", "research"), complete("/research ", 10));
}

/// **Enter on a row the token already spells sends** — the TUI's rule,
/// either spelling, case-insensitively (its `EqualFold`).
#[test]
fn enter_sends_a_row_already_spelled() {
    let menu = resolve(&fixture_31(), &all_caps(), None);
    let enter = |draft: &str, row: &str| accept(&menu, draft, row, SlashAcceptKey::Enter, false);
    assert_eq!(
        enter("/rename", "rename"),
        dispatched(refused("type a title after /rename"))
    );
    assert_eq!(enter("/rename foo", "rename"), dispatched(title("foo")));
    assert_eq!(
        enter("/RENAME", "rename"),
        dispatched(refused("type a title after /rename"))
    );
    assert_eq!(
        enter("/research", "research"),
        dispatched(prompt_text("/research"))
    );
    assert_eq!(
        enter("/pr-tools:review", "review"),
        dispatched(prompt_text("/pr-tools:review"))
    );
}

/// **Selecting a row whose action takes no argument runs it at once**
/// (§3.5): Enter and a pick. A bare `/` selects the highlighted (first) row.
/// An argument suffix still gets its refusal.
#[test]
fn selecting_a_no_argument_row_runs_it() {
    let menu = resolve(&fixture_30(), &all_caps(), None);
    for key in [SlashAcceptKey::Enter, SlashAcceptKey::Pick] {
        let pick = |draft: &str, row: &str| accept(&menu, draft, row, key, false);
        assert_eq!(
            pick("/ex", "exit"),
            dispatched(run_action(SlashAction::Stop)),
            "{key:?}"
        );
        assert_eq!(
            pick("/pl", "plan"),
            dispatched(run_action(set_mode("plan"))),
            "{key:?}"
        );
        assert_eq!(
            pick("/mo", "model"),
            dispatched(run_action(SlashAction::ModelPicker)),
            "{key:?}: /model's catalog arg is optional, but this client takes none (X2)"
        );
        assert_eq!(
            pick("/", "model"),
            dispatched(run_action(SlashAction::ModelPicker)),
            "{key:?}"
        );
        assert_eq!(
            pick("/ex now", "exit"),
            dispatched(refused("/exit: takes no argument")),
            "{key:?}"
        );
    }
    // While working, selecting a row that waits answers its refusal.
    assert_eq!(
        accept(&menu, "/pl", "plan", SlashAcceptKey::Enter, true),
        dispatched(refused("/plan: runs after this turn"))
    );
    assert_eq!(
        accept(&menu, "/re", "rename", SlashAcceptKey::Enter, true),
        complete("/rename ", 8),
        "a row that runs mid-turn still completes"
    );
}

/// **Tab completes, never runs** — even a spelled row or a no-argument one.
#[test]
fn tab_only_completes() {
    let menu = resolve(&fixture_30(), &all_caps(), None);
    let tab = |draft: &str, row: &str| accept(&menu, draft, row, SlashAcceptKey::Tab, false);
    assert_eq!(tab("/ex", "exit"), complete("/exit ", 6));
    assert_eq!(tab("/rename", "rename"), complete("/rename ", 8));
    assert_eq!(tab("/RENAME x", "rename"), complete("/rename x", 8));
}

/// A pick on a row that takes an argument completes it, spelled or not.
#[test]
fn a_pick_completes_an_argument_row_even_when_spelled() {
    let menu = resolve(&fixture_30(), &all_caps(), None);
    assert_eq!(
        accept(&menu, "/rename", "rename", SlashAcceptKey::Pick, false),
        complete("/rename ", 8)
    );
    assert_eq!(
        accept(&menu, "/rename x", "rename", SlashAcceptKey::Pick, false),
        complete("/rename x", 8)
    );
}

/// The qualified spelling is written only when the typed token holds a `:`.
#[test]
fn completion_writes_the_qualified_spelling_after_a_colon() {
    let menu = resolve(&fixture_31(), &all_caps(), None);
    assert_eq!(
        accept(&menu, "/pr:", "review", SlashAcceptKey::Enter, false),
        complete("/pr-tools:review ", 17)
    );
    assert_eq!(
        accept(&menu, "/rev", "review", SlashAcceptKey::Enter, false),
        complete("/review ", 8)
    );
    // A row with no second spelling writes its name, colon or not.
    assert_eq!(
        accept(
            &menu,
            "/pr-tools:h",
            "pr-tools:help",
            SlashAcceptKey::Tab,
            false
        ),
        complete("/pr-tools:help ", 15)
    );
}

/// The caret a completion answers is in UTF-16 code units.
#[test]
fn a_completion_caret_counts_utf16_code_units() {
    let menu = resolve(
        &LaneSlashCatalog {
            rows: vec![prompt("😀go", LaneSlashSource::Agent)],
        },
        &all_caps(),
        None,
    );
    assert_eq!(
        accept(&menu, "/😀", "😀go", SlashAcceptKey::Tab, false),
        complete("/😀go ", 6)
    );
}

/// Nothing to accept: a draft that is not a command, or a row the menu no
/// longer has.
/// **A dimmed row selects to its refusal** (§3.5, X7) even when it takes an
/// argument: picking it, or Enter on a token that does not spell it yet,
/// refuses and keeps the draft; Tab still completes it for after the turn;
/// with no turn running the same row completes as usual.
#[test]
fn a_dimmed_row_that_takes_an_argument_selects_to_its_refusal() {
    let mut rename = rename();
    rename.while_working = false;
    let menu = resolve(&LaneSlashCatalog { rows: vec![rename] }, &all_caps(), None);
    let after = || dispatched(refused("/rename: runs after this turn"));
    for key in [SlashAcceptKey::Pick, SlashAcceptKey::Enter] {
        assert_eq!(
            accept(&menu, "/re new title", "rename", key, true),
            after(),
            "{key:?}"
        );
    }
    assert_eq!(
        accept(&menu, "/re new title", "rename", SlashAcceptKey::Tab, true),
        complete("/rename new title", 8)
    );
    assert_eq!(
        accept(
            &menu,
            "/re new title",
            "rename",
            SlashAcceptKey::Pick,
            false
        ),
        complete("/rename new title", 8)
    );
    // Fully spelled, Enter dispatches — refused the same way.
    assert_eq!(
        accept(
            &menu,
            "/rename new title",
            "rename",
            SlashAcceptKey::Enter,
            true
        ),
        after()
    );
}

#[test]
fn accept_answers_none_when_there_is_nothing_to_accept() {
    let menu = resolve(&fixture_30(), &all_caps(), None);
    assert_eq!(
        accept(&menu, "/re", "nosuch", SlashAcceptKey::Enter, false),
        None
    );
    assert_eq!(
        accept(&menu, "/re", "help", SlashAcceptKey::Enter, false),
        None,
        "hidden"
    );
    assert_eq!(
        accept(&menu, "re", "rename", SlashAcceptKey::Tab, false),
        None
    );
    assert_eq!(
        accept(&menu, "/re\nx", "rename", SlashAcceptKey::Tab, false),
        None
    );
    assert_eq!(
        accept(&menu, "/re", "RENAME", SlashAcceptKey::Tab, false),
        complete("/rename ", 8),
        "the row name is matched as names are"
    );
}

// ---- is_working ----

/// "Working" is `Working` or `NeedsApproval` (P23).
#[test]
fn working_is_working_or_needs_approval() {
    for (activity, want) in [
        (RcActivity::Working, true),
        (RcActivity::NeedsApproval, true),
        (RcActivity::NeedsInput, false),
        (RcActivity::Idle, false),
        (RcActivity::Unknown, false),
    ] {
        assert_eq!(is_working(activity), want, "{activity:?}");
    }
}

// ---- JSON ----

/// The JSON both clients read: snake_case fields, `kind`-tagged enums, `None`
/// omitted — and every type reads back what it wrote.
#[test]
fn the_json_is_snake_case_and_round_trips() {
    let menu = resolve(&fixture_30(), &all_caps(), None);
    let rename = serde_json::to_value(&menu.rows[1]).unwrap();
    assert_eq!(
        rename,
        json!({
            "name": "rename",
            "description": "rename description",
            "source": "builtin",
            "arg": "required",
            "arg_hint": "<title>",
            "while_working": true,
            "action": {"kind": "set_title"},
        })
    );
    assert_eq!(
        serde_json::to_value(&menu.rows[2].action).unwrap(),
        json!({"kind": "set_mode", "change": {"kind": "mode", "id": "plan"}})
    );
    assert_eq!(
        serde_json::to_value(&menu.rows[0].action).unwrap(),
        json!({"kind": "model_picker"})
    );
    assert_eq!(
        serde_json::to_value(&menu.hidden[0]).unwrap(),
        json!({"name": "help", "reason": NOT_YET})
    );

    let dispatches = [
        (SlashDispatch::NotCommand, json!({"kind": "not_command"})),
        (
            prompt_text("/research x"),
            json!({"kind": "prompt", "text": "/research x"}),
        ),
        (
            title("t"),
            json!({"kind": "run", "action": {"kind": "set_title"}, "arg": "t"}),
        ),
        (
            run_action(SlashAction::Stop),
            json!({"kind": "run", "action": {"kind": "stop"}}),
        ),
        (refused("no"), json!({"kind": "refuse", "reason": "no"})),
    ];
    for (d, want) in dispatches {
        assert_eq!(serde_json::to_value(&d).unwrap(), want);
        assert_eq!(serde_json::from_value::<SlashDispatch>(want).unwrap(), d);
    }

    let accepts = [
        (
            SlashAccept::Complete {
                draft: "/rename ".into(),
                caret: 8,
            },
            json!({"kind": "complete", "draft": "/rename ", "caret": 8}),
        ),
        (
            SlashAccept::Dispatch {
                dispatch: SlashDispatch::NotCommand,
            },
            json!({"kind": "dispatch", "dispatch": {"kind": "not_command"}}),
        ),
    ];
    for (a, want) in accepts {
        assert_eq!(serde_json::to_value(&a).unwrap(), want);
        assert_eq!(serde_json::from_value::<SlashAccept>(want).unwrap(), a);
    }
    assert_eq!(
        serde_json::to_value(SlashAcceptKey::Pick).unwrap(),
        json!("pick")
    );

    let f = filter(&menu, "/ex", 3, true);
    let v = serde_json::to_value(&f).unwrap();
    assert_eq!(v["open"], json!(true));
    assert_eq!(v["token"], json!("/ex"));
    assert_eq!(v["rows"][0]["row"]["name"], json!("exit"));
    assert_eq!(v["rows"][0]["dimmed"], json!(false));
    assert_eq!(serde_json::from_value::<SlashFilter>(v).unwrap(), f);

    let back: SlashMenu = serde_json::from_value(serde_json::to_value(&menu).unwrap()).unwrap();
    assert_eq!(back, menu);
}

// ---- /effort (plan 032 §3.2) ----

/// The effort picker [`resolve`] builds for `settings`' effort option on its
/// current model, which has no effort facts (so no default level).
fn picker_for(settings: &LaneSettings) -> SlashAction {
    let option = settings.effort_option().expect("an effort option");
    SlashAction::EffortPicker {
        option: option.id.clone(),
        for_model: settings.model.clone().expect("a current model"),
        levels: option.values.clone(),
        default_level: None,
    }
}

/// `/effort` set to `value`, bound to `model`, on the option `id`.
fn set_effort(id: &str, value: &str, model: &str) -> SlashDispatch {
    run_action(SlashAction::SetEffort {
        change: LaneSettingChange::Config {
            id: id.to_string(),
            value: value.to_string(),
            for_model: Some(model.to_string()),
        },
    })
}

/// **`requires: effort` is the session's effort, not a capability** (plan
/// 032 §3.2, PC9): the row shows only with a current model AND an effort
/// option (craze's `EffortOption` — glm-5.2's `reasoning` counts), hidden
/// otherwise with its reason; the picker itself needs `caps.settings`.
#[test]
fn the_effort_row_follows_the_current_models_effort() {
    let caps = all_caps();
    let no_settings = LaneCapabilities {
        settings: false,
        ..all_caps()
    };
    let no_effort = Err(Some(NO_EFFORT.to_string()));
    let grok = grok_46();
    let composer = composer_25();
    let glm = settings_on(
        Some("glm-5.2"),
        vec![select(
            "reasoning",
            "thought_level",
            "high",
            &[("high", "High"), ("max", "Max")],
        )],
    );
    let no_model = LaneSettings {
        model: None,
        ..grok_46()
    };
    let empty_model = LaneSettings {
        model: Some(String::new()),
        ..grok_46()
    };
    let bare_picker = row(
        "effort",
        LaneSlashSource::Builtin,
        LaneSlashRun {
            picker: Some(LaneSlashPicker::Effort),
            ..run(LaneSlashKind::Picker)
        },
    );
    let cases: Vec<(
        &str,
        LaneSlashRow,
        &LaneCapabilities,
        Option<&LaneSettings>,
        _,
    )> = vec![
        (
            "grok-4.6",
            effort(),
            &caps,
            Some(&grok),
            Ok(picker_for(&grok)),
        ),
        (
            "glm-5.2's reasoning",
            effort(),
            &caps,
            Some(&glm),
            Ok(picker_for(&glm)),
        ),
        (
            "composer-2.5: no effort option",
            effort(),
            &caps,
            Some(&composer),
            no_effort.clone(),
        ),
        (
            "no settings at all",
            effort(),
            &caps,
            None,
            no_effort.clone(),
        ),
        (
            "no current model",
            effort(),
            &caps,
            Some(&no_model),
            no_effort.clone(),
        ),
        (
            "an empty current model",
            effort(),
            &caps,
            Some(&empty_model),
            no_effort.clone(),
        ),
        (
            "the picker without `settings`",
            effort(),
            &no_settings,
            Some(&grok),
            Err(Some(NOT_ADVERTISED.to_string())),
        ),
        (
            "an effort picker with no requires",
            bare_picker.clone(),
            &caps,
            Some(&grok),
            Ok(picker_for(&grok)),
        ),
        (
            "an effort picker with no requires, no effort",
            bare_picker,
            &caps,
            Some(&composer),
            no_effort.clone(),
        ),
        (
            "a prompt row requiring effort",
            requiring(
                prompt("think", LaneSlashSource::Builtin),
                LaneSlashRequires::Effort,
            ),
            &caps,
            Some(&grok),
            Ok(SlashAction::Prompt),
        ),
        (
            "a prompt row requiring effort, no effort",
            requiring(
                prompt("think", LaneSlashSource::Builtin),
                LaneSlashRequires::Effort,
            ),
            &caps,
            Some(&composer),
            no_effort,
        ),
        (
            "an agent's row requiring effort, no effort: hidden, not listed",
            requiring(
                prompt("think", LaneSlashSource::Agent),
                LaneSlashRequires::Effort,
            ),
            &caps,
            None,
            Err(None),
        ),
    ];
    for (what, r, caps, settings, want) in cases {
        assert_eq!(verdict(r, caps, settings), want, "{what}");
    }
}

/// **The default level is the current model's own, and only one the option
/// offers** (plan 032 §3.2, PC9): its effort facts' `default_effort`; none
/// without facts, for another model's facts, or for a default the option
/// does not offer.
#[test]
fn the_default_level_is_the_current_models_offered_default() {
    let default_of = |settings: LaneSettings| match verdict(effort(), &all_caps(), Some(&settings))
    {
        Ok(SlashAction::EffortPicker { default_level, .. }) => default_level,
        other => panic!("not a picker: {other:?}"),
    };
    assert_eq!(
        default_of(with_default(grok_46(), "high")).as_deref(),
        Some("high")
    );
    assert_eq!(
        default_of(grok_46()),
        None,
        "no effort facts (an ACP session)"
    );
    assert_eq!(
        default_of(with_default(grok_46(), "max")),
        None,
        "a default the effort option does not offer"
    );
    let mut elsewhere = with_default(grok_46(), "high");
    elsewhere.models[0].id = "other".into();
    elsewhere.models.insert(0, choice("grok-4.6", "Grok 4.6"));
    assert_eq!(default_of(elsewhere), None, "another model's facts");
    let mut no_default = with_default(grok_46(), "high");
    if let Some(e) = no_default.models[0].effort.as_mut() {
        e.default_effort = None;
    }
    assert_eq!(default_of(no_default), None, "facts with no default");
}

/// **A hidden `/effort` claims no name** (P:2123): on a model with no effort
/// option the agent's own `/effort` after the builtin is the one shown —
/// typed, it goes out as a prompt — and the builtin is not refused; with an
/// effort option the builtin wins the name. Alone and hidden, the builtin is
/// refused with its reason, a level or not.
#[test]
fn an_agents_effort_answers_while_the_builtin_is_hidden() {
    let catalog = LaneSlashCatalog {
        rows: vec![effort(), prompt("effort", LaneSlashSource::Agent)],
    };
    let menu = resolve(&catalog, &all_caps(), Some(&composer_25()));
    assert_eq!(names(&menu), ["effort"]);
    assert_eq!(
        (menu.rows[0].source.clone(), menu.rows[0].action.clone()),
        (LaneSlashSource::Agent, SlashAction::Prompt)
    );
    assert!(menu.hidden.is_empty(), "{:?}", menu.hidden);
    assert_eq!(
        dispatch(&menu, "/effort low", false),
        prompt_text("/effort low")
    );

    let menu = resolve(&catalog, &all_caps(), Some(&grok_46()));
    assert_eq!(names(&menu), ["effort"]);
    assert_eq!(menu.rows[0].source, LaneSlashSource::Builtin);
    assert_eq!(
        dispatch(&menu, "/effort low", false),
        set_effort("effort", "low", "grok-4.6")
    );

    let alone = resolve(
        &LaneSlashCatalog {
            rows: vec![effort()],
        },
        &all_caps(),
        Some(&composer_25()),
    );
    assert_eq!(hidden(&alone), [("effort", NO_EFFORT)]);
    for draft in ["/effort", "/effort low", "/EFFORT"] {
        assert_eq!(
            dispatch(&alone, draft, false),
            refused("/effort: the current model has no effort setting"),
            "{draft}"
        );
    }
}

/// **`/effort` bare runs the picker; `/effort <level>` sets it** (plan 032
/// §3.2, PC8): a level matched by craze's `MatchEffortValue` — its id
/// exactly, then in any case, then its name (trimmed) in any case — is ONE
/// `SetEffort`, a `Config` change bound to the current model, sent even when
/// it is the current level; anything else is refused with craze's text, the
/// list folded through `DisplayLine`.
#[test]
fn effort_runs_the_picker_bare_and_sets_a_level_inline() {
    let grok = grok_46();
    let menu = resolve(
        &LaneSlashCatalog {
            rows: vec![effort(), exit()],
        },
        &all_caps(),
        Some(&grok),
    );
    let picker = run_action(picker_for(&grok));
    let all = "one of low, medium, high, xhigh";
    let cases = [
        ("/effort", picker.clone()),
        ("/effort   ", picker.clone()),
        ("/Effort", picker),
        ("/effort low", set_effort("effort", "low", "grok-4.6")),
        ("/EFFORT low", set_effort("effort", "low", "grok-4.6")),
        (
            "/effort high",
            set_effort("effort", "high", "grok-4.6"), // the current level: still sent
        ),
        ("/effort LOW", set_effort("effort", "low", "grok-4.6")),
        ("/effort  xHigh ", set_effort("effort", "xhigh", "grok-4.6")),
        (
            "/effort Extra High",
            set_effort("effort", "xhigh", "grok-4.6"),
        ),
        (
            "/effort extra high",
            set_effort("effort", "xhigh", "grok-4.6"),
        ),
        ("/effort bogus", refused(&format!("no effort bogus: {all}"))),
        (
            "/effort \u{1b}[31mbogus\u{1b}[0m", // the escapes go whole
            refused(&format!("no effort bogus: {all}")),
        ),
        (
            "/effort extra   high",
            refused(&format!("no effort extra high: {all}")),
        ),
        (
            "/effort default",
            refused(&format!("no effort default: {all}")),
        ),
    ];
    for (draft, want) in cases {
        assert_eq!(dispatch(&menu, draft, false), want, "{draft:?}");
    }
}

/// **The first rule that matches anything decides, and must match one**
/// (craze's `effortValue`): an exact id wins over a case-insensitive one
/// and over a name; a rule matching two levels is refused, never handed to a
/// looser rule; a level's name is matched trimmed (PC14j).
#[test]
fn an_exact_id_wins_and_an_ambiguous_rule_is_refused() {
    let case = |levels: &[(&str, &str)], typed: &str, want: SlashDispatch| {
        let menu = resolve(
            &LaneSlashCatalog {
                rows: vec![effort()],
            },
            &all_caps(),
            Some(&settings_on(
                Some("m"),
                vec![select("effort", "x", levels[0].0, levels)],
            )),
        );
        assert_eq!(
            dispatch(&menu, &format!("/effort {typed}"), false),
            want,
            "{levels:?} {typed:?}"
        );
    };
    let high = [("high", "High"), ("HIGH", "Highest")];
    case(&high, "high", set_effort("effort", "high", "m"));
    case(&high, "HIGH", set_effort("effort", "HIGH", "m"));
    case(&high, "High", refused("no effort High: one of high, HIGH"));
    case(
        &[("a", "Fast"), ("b", "fast")],
        "FAST",
        refused("no effort FAST: one of a, b"),
    );
    case(
        &[("lo", "low"), ("low", "Lowest")],
        "low",
        set_effort("effort", "low", "m"),
    );
    case(
        &[("lo", "low"), ("LOW", "Lowest")],
        "low",
        set_effort("effort", "LOW", "m"),
    );
    case(
        &[("x", "  Max  "), ("y", "Min")],
        "max",
        set_effort("effort", "x", "m"),
    );

    // "In any case" is Go's `EqualFold` — Unicode simple case folding —
    // not a lowercase: `Σ` is `σ` AND `ς`, so it is ambiguous by id and by
    // name alike, while each spelled exactly is still its own level.
    let sigmas = [("σ", "Sigma"), ("ς", "Final sigma")];
    case(&sigmas, "Σ", refused("no effort Σ: one of σ, ς"));
    case(&sigmas, "σ", set_effort("effort", "σ", "m"));
    case(&sigmas, "ς", set_effort("effort", "ς", "m"));
    case(
        &[("a", "σ"), ("b", "ς")],
        "Σ",
        refused("no effort Σ: one of a, b"),
    );
    // The fold classes beyond ASCII (the Kelvin sign is a `k`, the long s
    // an `s`), none for a full-folding expansion (`ß` is not `ss`) or the
    // Turkic-only `ı`.
    case(
        &[("kelvin", "K"), ("fast", "F")],
        "\u{212a}ELVIN",
        set_effort("effort", "kelvin", "m"),
    );
    case(
        &[("kelvin", "K"), ("fast", "F")],
        "FA\u{17f}T",
        set_effort("effort", "fast", "m"),
    );
    case(
        &[("maß", "Maß")],
        "MASS",
        refused("no effort MASS: one of maß"),
    );
    case(&[("i", "I")], "ı", refused("no effort ı: one of i"));
}

/// **`eq_fold` is Go's `strings.EqualFold`** — the fold classes of Go's
/// `unicode.SimpleFold` (plan 032 C2 review): ASCII case-insensitive, the
/// whole Greek sigma class, the Kelvin sign and long s with their ASCII
/// letters, titlecase digraphs with their pairs; never a full-folding
/// expansion, never `ı` with `i`, and the same number of characters.
#[test]
fn eq_fold_is_gos_equal_fold() {
    for (a, b, want) in [
        ("", "", true),
        ("low", "LOW", true),
        ("xHigh", "XHIGH", true),
        ("low", "lo", false),
        ("low", "lowe", false),
        ("default", "DEFAULT", true),
        ("Σ", "σ", true),
        ("Σ", "ς", true),
        ("σ", "ς", true),
        ("\u{212a}", "k", true),
        ("\u{212a}", "K", true),
        ("\u{17f}", "S", true),
        ("\u{b5}", "\u{3bc}", true),  // micro sign, mu
        ("\u{3d1}", "\u{3f4}", true), // theta symbol, capital theta symbol
        ("\u{1c5}", "\u{1c6}", true), // titlecase and lowercase dz caron
        ("\u{1c4}", "\u{1c5}", true),
        ("ß", "\u{1e9e}", true), // capital sharp s
        ("ß", "ss", false),
        ("ı", "i", false),
        ("ı", "I", false),
        ("\u{130}", "i", false), // capital I with dot
        ("ÉLAN", "élan", true),
        ("a", "b", false),
    ] {
        assert_eq!(eq_fold(a, b), want, "{a:?} {b:?}");
        assert_eq!(eq_fold(b, a), want, "{b:?} {a:?}");
    }
}

/// **`default` is reserved** (craze's F2): in any case it is the current
/// model's default level, refused without one — even where a level is
/// spelled `default`, which a refusal for the reserved word then leaves out
/// of its list (and lists for any other word). With a default, every
/// refusal offers `default` first.
#[test]
fn default_is_reserved_for_the_models_default_level() {
    let catalog = LaneSlashCatalog {
        rows: vec![effort()],
    };
    let facts = resolve(
        &catalog,
        &all_caps(),
        Some(&with_default(grok_46(), "high")),
    );
    for draft in ["/effort default", "/effort DEFAULT", "/effort  Default "] {
        assert_eq!(
            dispatch(&facts, draft, false),
            set_effort("effort", "high", "grok-4.6"),
            "{draft:?}"
        );
    }
    assert_eq!(
        dispatch(&facts, "/effort bogus", false),
        refused("no effort bogus: one of default, low, medium, high, xhigh")
    );
    let unoffered = resolve(&catalog, &all_caps(), Some(&with_default(grok_46(), "max")));
    assert_eq!(
        dispatch(&unoffered, "/effort default", false),
        refused("no effort default: one of low, medium, high, xhigh"),
        "a default the option does not offer is none"
    );

    let spelled = |facts: Option<&str>| {
        let s = settings_on(
            Some("m"),
            vec![select(
                "effort",
                "x",
                "low",
                &[("default", "Default"), ("low", "Low")],
            )],
        );
        let s = match facts {
            Some(d) => with_default(s, d),
            None => s,
        };
        resolve(&catalog, &all_caps(), Some(&s))
    };
    assert_eq!(
        dispatch(&spelled(None), "/effort default", false),
        refused("no effort default: one of low"),
        "the reserved word never reaches the level spelled `default`"
    );
    assert_eq!(
        dispatch(&spelled(None), "/effort bogus", false),
        refused("no effort bogus: one of default, low"),
        "listed for any other word"
    );
    assert_eq!(
        dispatch(&spelled(Some("low")), "/effort default", false),
        set_effort("effort", "low", "m"),
        "`default` is the model's default level, not the level of that name"
    );
}

/// **Mid-turn `/effort` waits for the turn, its argument judged first**
/// (plan 032 §3.2): the builtin row has no `whileWorking`, so it is dimmed
/// and a bare or valid `/effort` is refused "runs after this turn" — but a
/// level that names nothing is refused for THAT.
#[test]
fn effort_waits_for_the_turn_after_its_argument_is_judged() {
    let menu = resolve(
        &LaneSlashCatalog {
            rows: vec![effort(), rename()],
        },
        &all_caps(),
        Some(&grok_46()),
    );
    let after = refused("/effort: runs after this turn");
    assert_eq!(dispatch(&menu, "/effort", true), after);
    assert_eq!(dispatch(&menu, "/effort low", true), after);
    assert_eq!(
        dispatch(&menu, "/effort bogus", true),
        refused("no effort bogus: one of low, medium, high, xhigh")
    );
    assert_eq!(
        dispatch(&menu, "/effort default", true),
        refused("no effort default: one of low, medium, high, xhigh")
    );
    let f = filter(&menu, "/", 1, true);
    assert_eq!(
        f.rows
            .iter()
            .map(|r| (r.row.name.as_str(), r.dimmed))
            .collect::<Vec<_>>(),
        [("effort", true), ("rename", false)]
    );
    assert_eq!(f.start, 1, "the highlight starts past the dimmed /effort");
}

/// **Pick, Enter and Tab on `/effort`** (PC8): the picker takes no argument
/// here, so a pick or Enter RUNS the completed draft — bare, the picker;
/// with a level, that level (or its refusal) — Enter on a spelled token
/// dispatches the draft as it stands, and Tab only completes. Mid-turn a
/// pick or Enter on a bare `/effort` refuses for the turn; with an argument
/// they keep dispatch's order (§3.2) — a level that names nothing is refused
/// for THAT, a valid one for the turn — and Tab still completes.
#[test]
fn accepting_effort_runs_the_completed_draft() {
    let grok = grok_46();
    let menu = resolve(
        &LaneSlashCatalog {
            rows: vec![model(), effort()],
        },
        &all_caps(),
        Some(&grok),
    );
    let picker = dispatched(run_action(picker_for(&grok)));
    let low = dispatched(set_effort("effort", "low", "grok-4.6"));
    let accept_with = |draft: &str, key: SlashAcceptKey, working: bool| {
        accept(&menu, draft, "effort", key, working)
    };
    for key in [SlashAcceptKey::Pick, SlashAcceptKey::Enter] {
        assert_eq!(accept_with("/ef", key, false), picker, "{key:?}");
        assert_eq!(accept_with("/", key, false), picker, "{key:?}");
        assert_eq!(accept_with("/ef low", key, false), low, "{key:?}");
        assert_eq!(accept_with("/EF  low", key, false), low, "{key:?}");
        assert_eq!(
            accept_with("/ef bogus", key, false),
            dispatched(refused("no effort bogus: one of low, medium, high, xhigh")),
            "{key:?}"
        );
        let after = dispatched(refused("/effort: runs after this turn"));
        let bogus = dispatched(refused("no effort bogus: one of low, medium, high, xhigh"));
        assert_eq!(accept_with("/ef low", key, true), after, "{key:?}");
        assert_eq!(accept_with("/effort low", key, true), after, "{key:?}");
        assert_eq!(accept_with("/ef", key, true), after, "{key:?} bare");
        assert_eq!(accept_with("/ef   ", key, true), after, "{key:?} blank");
        assert_eq!(accept_with("/ef bogus", key, true), bogus, "{key:?}");
        assert_eq!(accept_with("/effort bogus", key, true), bogus, "{key:?}");
        assert_eq!(
            accept_with("/ef default", key, true),
            dispatched(refused(
                "no effort default: one of low, medium, high, xhigh"
            )),
            "{key:?}: no default level"
        );
    }
    assert_eq!(accept_with("/effort", SlashAcceptKey::Enter, false), picker);
    assert_eq!(
        accept_with("/effort low", SlashAcceptKey::Enter, false),
        low
    );
    assert_eq!(
        accept_with("/ef", SlashAcceptKey::Tab, false),
        complete("/effort ", 8)
    );
    assert_eq!(
        accept_with("/ef low", SlashAcceptKey::Tab, false),
        complete("/effort low", 8)
    );
    assert_eq!(
        accept_with("/effort", SlashAcceptKey::Tab, true),
        complete("/effort ", 8)
    );
}

/// `display_line` is craze's `DisplayLine` exactly: escape sequences go WHOLE
/// (CSI, OSC to BEL or ST, an escape before a character), then controls (a
/// newline, carriage return or tab first read as a space), then whitespace
/// runs collapse — and a clean ASCII line comes back as it was. Every `want`
/// here is what craze v0.2.0's own `DisplayLine` answered for the input (and
/// the port agreed with it on 200,000 random strings heavy in escape bytes,
/// plan 032 C2 review), its byte-level edges included: a C1 CSI spelled in
/// UTF-8 is a control character, not a sequence, and an OSC ended inside a
/// character leaves a stray byte that reads as `U+FFFD`.
#[test]
fn display_line_folds_a_word_onto_one_clean_line() {
    for (raw, want) in [
        ("", ""),
        ("low", "low"),
        ("x-high", "x-high"),
        ("  extra   high ", "extra high"),
        ("a\tb\nc\rd", "a b c d"),
        ("a\u{0b}b\u{85}c", "abc"),
        ("non\u{a0}breaking", "non breaking"),
        ("très  haut", "très haut"),
        ("\u{1b}[31mred", "red"),
        ("\u{1b}[31mbogus\u{1b}[0m", "bogus"),
        ("\u{1b}]0;title\u{07}x", "x"),
        ("\u{1b}]8;;http://x\u{1b}\\link", "link"),
        ("\u{1b}é", "é"),
        ("low\u{1b}", "low"),
        ("\u{9b}31mz", "31mz"),
        ("\u{1b}]\u{e700}x", "\u{fffd}x"),
    ] {
        assert_eq!(display_line(raw), want, "{raw:?}");
    }
}

/// **The effort actions' JSON** — what the desktop hands back to the
/// `lane_slash_*` commands and the phone mirrors: the picker with its option,
/// model, levels and (when there is one) default level; the level a
/// `Config` change bound to the model.
#[test]
fn the_effort_actions_json_round_trips() {
    let grok = with_default(grok_46(), "high");
    let menu = resolve(
        &LaneSlashCatalog {
            rows: vec![effort()],
        },
        &all_caps(),
        Some(&grok),
    );
    let row = serde_json::to_value(&menu.rows[0]).unwrap();
    assert_eq!(
        row,
        json!({
            "name": "effort", "description": "effort description", "source": "builtin",
            "arg": "optional", "arg_hint": "<level>", "while_working": false,
            "action": {"kind": "effort_picker", "option": "effort", "for_model": "grok-4.6",
                       "levels": [{"id": "low", "name": "Low"}, {"id": "medium", "name": "Medium"},
                                  {"id": "high", "name": "High"}, {"id": "xhigh", "name": "Extra High"}],
                       "default_level": "high"},
        })
    );
    let no_default = picker_for(&grok_46());
    assert!(serde_json::to_value(&no_default)
        .unwrap()
        .get("default_level")
        .is_none());
    let set = dispatch(&menu, "/effort low", false);
    assert_eq!(
        serde_json::to_value(&set).unwrap(),
        json!({"kind": "run", "action": {"kind": "set_effort", "change": {
            "kind": "config", "id": "effort", "value": "low", "for_model": "grok-4.6"}}})
    );
    assert_eq!(
        serde_json::from_value::<SlashDispatch>(serde_json::to_value(&set).unwrap()).unwrap(),
        set
    );
    let back: SlashMenu = serde_json::from_value(serde_json::to_value(&menu).unwrap()).unwrap();
    assert_eq!(back, menu);
    assert_eq!(
        dispatch(&back, "/effort default", false),
        set_effort("effort", "high", "grok-4.6"),
        "the menu that crossed the bridge dispatches as the one that did not"
    );
}
