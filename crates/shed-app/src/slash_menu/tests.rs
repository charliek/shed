//! The `/` menu's rules, table by table (plan 029 §3.4, P5/P6/P9/P16/P23/P28).
//!
//! Rows are built in Rust from the contract's variants rather than decoded
//! from JSON: the catalog's wire words are the adapter's business (shed-craze
//! pins them), and this module only ever sees the decoded rows. The builders
//! below copy fixture 30/31's real rows (`crates/shed-craze/fixtures/wire/`)
//! and the protocol's `unavailable` illustration (plan amendment E3).

use super::*;

use serde_json::json;
use shed_core::lane::LaneSlashRun;

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

/// One row alone against `caps`: its action when shown; when hidden, the
/// reason `hidden` lists it with (`None` for a hidden non-builtin, which is
/// not listed).
fn verdict(r: LaneSlashRow, caps: &LaneCapabilities) -> Result<SlashAction, Option<String>> {
    let menu = resolve(&LaneSlashCatalog { rows: vec![r] }, caps);
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
        assert_eq!(verdict(r, caps), want, "{what}");
    }
}

/// **Fixture 30 resolved**: the order kept, the hidden builtins listed with
/// their reasons, and the agent's `/btw` shown where the builtin is hidden.
#[test]
fn fixture_30_resolves_to_its_menu() {
    let menu = resolve(&fixture_30(), &all_caps());
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
    let menu = resolve(&fixture_30(), &no_stop());
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
    let menu = resolve(&catalog, &no_stop());
    assert_eq!(names(&menu), ["btw", "exit"]);
    assert!(menu.rows.iter().all(|r| r.source == LaneSlashSource::Agent));
    assert!(menu.hidden.is_empty(), "{:?}", menu.hidden);

    // With stop, the builtin /exit shows and the agent's is the duplicate.
    let menu = resolve(&catalog, &all_caps());
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
    let menu = resolve(&catalog, &all_caps());
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
    let menu = resolve(&catalog, &all_caps());
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
    let menu = resolve(&catalog, &all_caps());
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
    let menu = resolve(&fixture_30(), &all_caps());
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
    let menu = resolve(&catalog, &all_caps());
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
    let menu = resolve(&fixture_30(), &all_caps());
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
    let menu = resolve(&fixture_30(), &all_caps());
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
    let menu = resolve(&fixture_30(), &all_caps());
    let f = filter(&menu, "/zzz", 4, false);
    assert!(!f.open);
    assert!(f.rows.is_empty());
    assert_eq!(f.token, "/zzz", "the token still keys an Esc dismissal");

    let nothing = resolve(
        &LaneSlashCatalog {
            rows: vec![local("help")],
        },
        &all_caps(),
    );
    assert!(nothing.rows.is_empty());
    assert!(!filter(&nothing, "/", 1, false).open);
}

/// **While working the rows stay listed** (X7), and the ones a turn would
/// refuse come back dimmed: not a `while_working` row, not a prompt (which
/// queues).
#[test]
fn rows_that_wait_for_the_turn_are_dimmed_while_working() {
    let menu = resolve(&fixture_30(), &all_caps());
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

/// **Carets are UTF-16 code units**: a non-BMP character in the token counts
/// two.
#[test]
fn the_caret_counts_utf16_code_units() {
    let menu = resolve(
        &LaneSlashCatalog {
            rows: vec![prompt("😀go", LaneSlashSource::Agent)],
        },
        &all_caps(),
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
    let menu = resolve(&fixture_30(), &all_caps());
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
    let menu = resolve(&fixture_30(), &all_caps());
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
    );
    assert_eq!(dispatch(&odd, "/", false), SlashDispatch::NotCommand);
}

/// **The grammar** (P6): the token to the first whitespace; the argument the
/// rest, trimmed, literal — inner whitespace and quotes kept.
#[test]
fn the_argument_is_the_rest_trimmed_and_literal() {
    let menu = resolve(&fixture_30(), &all_caps());
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
    let menu = resolve(&fixture_30(), &all_caps());
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
    let menu = resolve(&fixture_30(), &all_caps());
    let cursor = resolve(&fixture_30(), &no_stop());
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
    let menu = resolve(&fixture_30(), &all_caps());
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
    let menu = resolve(&catalog, &all_caps());
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
    let menu = resolve(&fixture_30(), &all_caps());
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
    let menu = resolve(&fixture_31(), &all_caps());
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
    let menu = resolve(&fixture_30(), &all_caps());
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
    let menu = resolve(&fixture_30(), &all_caps());
    let tab = |draft: &str, row: &str| accept(&menu, draft, row, SlashAcceptKey::Tab, false);
    assert_eq!(tab("/ex", "exit"), complete("/exit ", 6));
    assert_eq!(tab("/rename", "rename"), complete("/rename ", 8));
    assert_eq!(tab("/RENAME x", "rename"), complete("/rename x", 8));
}

/// A pick on a row that takes an argument completes it, spelled or not.
#[test]
fn a_pick_completes_an_argument_row_even_when_spelled() {
    let menu = resolve(&fixture_30(), &all_caps());
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
    let menu = resolve(&fixture_31(), &all_caps());
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
    let menu = resolve(&LaneSlashCatalog { rows: vec![rename] }, &all_caps());
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
    let menu = resolve(&fixture_30(), &all_caps());
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
    let menu = resolve(&fixture_30(), &all_caps());
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
