//! The presence-aware slash decoder (plan 029 P15, P16), table by table.

use serde_json::{json, Value};
use shed_core::lane::{
    LaneSlashArg, LaneSlashCatalog, LaneSlashKind, LaneSlashMethod, LaneSlashPicker,
    LaneSlashRequires, LaneSlashRow, LaneSlashRun, LaneSlashSource,
};

use super::*;

/// The run a row with `run: {kind}` and nothing else reads as.
fn bare_run(kind: LaneSlashKind) -> LaneSlashRun {
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

fn names(c: &LaneSlashCatalog) -> Vec<&str> {
    c.rows.iter().map(|r| r.name.as_str()).collect()
}

/// **Present or absent** (P3, P15): no `slash` key — or `null` — is no
/// section (the catalog held is unchanged); any other value is a section
/// that REPLACES it — `{}` empty, and a `slash` or `items` of the wrong JSON
/// type present and empty, never absent.
#[test]
fn a_section_is_present_or_absent_never_unreadable() {
    assert_eq!(section(&json!({})), None, "absent");
    assert_eq!(
        section(&json!({"mode": "plan"})),
        None,
        "another section only"
    );
    assert_eq!(section(&json!({"slash": null})), None, "null is no section");
    assert_eq!(section(&json!("not an object")), None, "not a holder");
    for present_empty in [
        json!({"slash": {}}),
        json!({"slash": {"items": []}}),
        json!({"slash": {"items": null}}),
        json!({"slash": {"items": 5}}),
        json!({"slash": {"items": {"name": "x"}}}),
        json!({"slash": {"items": "help"}}),
        json!({"slash": 5}),
        json!({"slash": "items"}),
        json!({"slash": [{"name": "help"}]}),
        json!({"slash": true}),
    ] {
        assert_eq!(
            section(&present_empty),
            Some(LaneSlashCatalog::default()),
            "{present_empty}: present and empty"
        );
    }
}

/// **One bad row costs that row** (P15): every way a row fails to read —
/// not an object, a known key of the wrong type, a name that is not one
/// token — drops it alone. Each case sits between two good rows, which
/// survive in order.
#[test]
fn a_malformed_row_is_dropped_alone() {
    let good = |n: &str| json!({"name": n, "source": "agent", "run": {"kind": "prompt"}});
    let bad: Vec<(&str, Value)> = vec![
        ("not an object", json!(5)),
        ("a string", json!("help")),
        ("an array", json!(["help"])),
        ("null", Value::Null),
        ("no name", json!({"source": "agent"})),
        ("a null name", json!({"name": null})),
        ("a numeric name", json!({"name": 42})),
        ("an empty name", json!({"name": ""})),
        ("two words", json!({"name": "two words"})),
        ("a leading space", json!({"name": " help"})),
        ("a tab", json!({"name": "a\tb"})),
        ("a newline", json!({"name": "a\nb"})),
        ("a no-break space", json!({"name": "a\u{a0}b"})),
        ("an ideographic space", json!({"name": "a\u{3000}b"})),
        ("a control character", json!({"name": "a\u{7}b"})),
        ("a C1 control", json!({"name": "a\u{85}b"})),
        ("a replacement character", json!({"name": "a\u{fffd}"})),
        (
            "a numeric description",
            json!({"name": "x", "description": 1}),
        ),
        ("an object source", json!({"name": "x", "source": {}})),
        ("a numeric plugin", json!({"name": "x", "plugin": 3})),
        ("a numeric qualified", json!({"name": "x", "qualified": 3})),
        ("a numeric requires", json!({"name": "x", "requires": 7})),
        (
            "a string whileWorking",
            json!({"name": "x", "whileWorking": "yes"}),
        ),
        ("a numeric run", json!({"name": "x", "run": 5})),
        ("an array run", json!({"name": "x", "run": []})),
        ("a numeric kind", json!({"name": "x", "run": {"kind": 5}})),
        (
            "a numeric method",
            json!({"name": "x", "run": {"kind": "method", "method": 1}}),
        ),
        (
            "a bool arg",
            json!({"name": "x", "run": {"kind": "prompt", "arg": true}}),
        ),
        (
            "a numeric argHint",
            json!({"name": "x", "run": {"kind": "prompt", "argHint": 1}}),
        ),
        (
            "an object param",
            json!({"name": "x", "run": {"kind": "method", "param": {}}}),
        ),
        (
            "a numeric picker",
            json!({"name": "x", "run": {"kind": "picker", "picker": 1}}),
        ),
        (
            "a numeric local",
            json!({"name": "x", "run": {"kind": "local", "local": 1}}),
        ),
        (
            "a numeric reason",
            json!({"name": "x", "run": {"kind": "unavailable", "reason": 1}}),
        ),
        (
            "a string setting",
            json!({"name": "x", "run": {"kind": "method", "setting": "mode"}}),
        ),
        (
            "a numeric setting kind",
            json!({"name": "x", "run": {"kind": "method", "setting": {"kind": 5, "value": "plan"}}}),
        ),
        (
            "a numeric setting value",
            json!({"name": "x", "run": {"kind": "method", "setting": {"kind": "mode", "value": 1}}}),
        ),
    ];
    for (why, row) in bad {
        let got = catalog(&json!({"items": [good("before"), row, good("after")]}));
        assert_eq!(names(&got), ["before", "after"], "{why}");
    }
}

/// **Absent and unknown values** (P15): a sparse row defaults —
/// `whileWorking` false, `arg` none, every option absent, a `run` (or its
/// `kind`) missing the empty and so unknown kind; `null` is absent; `""`
/// says nothing; an unknown key is ignored; an unknown value of every
/// enum is kept as `Other`, for a client to hide the row.
#[test]
fn absent_values_default_and_unknown_values_are_kept() {
    let rows = catalog(&json!({"items": [
        {"name": "sparse", "run": {"kind": "prompt"}},
        {"name": "norun"},
        {"name": "nokind", "run": {}},
        {"name": "nulls", "description": null, "source": null, "requires": null,
         "whileWorking": null, "run": {"kind": "prompt", "arg": null, "setting": null}},
        {"name": "empties", "description": "", "source": "agent", "requires": "",
         "plugin": "", "run": {"kind": "prompt", "arg": "", "argHint": ""}},
        {"name": "extra", "source": "agent", "colour": "red", "run": {"kind": "prompt", "speed": 9}},
        {"name": "unknowns", "source": "galaxy", "requires": "moon",
         "run": {"kind": "dance", "arg": "maybe", "method": "session.dance", "picker": "theme"}},
    ]}))
    .rows;
    assert_eq!(rows.len(), 7, "{rows:#?}");
    let sparse = &rows[0];
    assert_eq!(sparse.source, LaneSlashSource::Other(String::new()));
    assert!(!sparse.while_working);
    assert_eq!(
        (
            &sparse.description,
            &sparse.requires,
            &sparse.plugin,
            &sparse.qualified
        ),
        (&None, &None, &None, &None)
    );
    assert_eq!(sparse.run, bare_run(LaneSlashKind::Prompt));
    for r in &rows[1..3] {
        assert_eq!(
            r.run,
            bare_run(LaneSlashKind::Other(String::new())),
            "{}: no kind is a kind no client knows",
            r.name
        );
        assert!(!r.run.kind.is_known());
    }
    assert_eq!(
        rows[3].run,
        bare_run(LaneSlashKind::Prompt),
        "null is absent"
    );
    assert!(!rows[3].while_working && rows[3].description.is_none());
    let empties = &rows[4];
    assert_eq!(
        (&empties.description, &empties.requires, &empties.plugin),
        (&None, &None, &None),
        "\"\" says nothing"
    );
    assert_eq!(empties.run, bare_run(LaneSlashKind::Prompt));
    assert_eq!(
        rows[5].run,
        bare_run(LaneSlashKind::Prompt),
        "unknown keys ignored"
    );
    let u = &rows[6];
    assert_eq!(u.source, LaneSlashSource::Other("galaxy".into()));
    assert_eq!(u.requires, Some(LaneSlashRequires::Other("moon".into())));
    assert_eq!(u.run.kind, LaneSlashKind::Other("dance".into()));
    assert_eq!(u.run.arg, LaneSlashArg::Other("maybe".into()));
    assert_eq!(
        u.run.method,
        Some(LaneSlashMethod::Other("session.dance".into()))
    );
    assert_eq!(u.run.picker, Some(LaneSlashPicker::Other("theme".into())));
}

/// craze's run words as the contract's: its three methods onto the three
/// verbs; a `session.set` setting that sets a MODE becomes `mode`, any other
/// setting (or none, or an empty value) leaves it `None`; the other run
/// strings verbatim.
#[test]
fn the_run_maps_crazes_words() {
    let run = |r: Value| {
        row(&json!({"name": "x", "source": "builtin", "run": r}))
            .unwrap()
            .run
    };
    assert_eq!(
        run(
            json!({"kind": "method", "method": "session.setTitle", "param": "title",
                   "arg": "required", "argHint": "<title>"})
        ),
        LaneSlashRun {
            method: Some(LaneSlashMethod::SetTitle),
            param: Some("title".into()),
            arg: LaneSlashArg::Required,
            arg_hint: Some("<title>".into()),
            ..bare_run(LaneSlashKind::Method)
        }
    );
    assert_eq!(
        run(json!({"kind": "method", "method": "session.set",
                   "setting": {"kind": "mode", "value": "plan"}})),
        LaneSlashRun {
            method: Some(LaneSlashMethod::Set),
            mode: Some("plan".into()),
            ..bare_run(LaneSlashKind::Method)
        }
    );
    for setting in [
        json!({"kind": "model", "value": "grok"}),
        json!({"kind": "mode"}),
        json!({"kind": "mode", "value": ""}),
        json!({"value": "plan"}),
        json!({}),
    ] {
        assert_eq!(
            run(json!({"kind": "method", "method": "session.set", "setting": setting})).mode,
            None,
            "{setting}"
        );
    }
    assert_eq!(
        run(json!({"kind": "method", "method": "session.stop"})).method,
        Some(LaneSlashMethod::Stop)
    );
    assert_eq!(
        run(
            json!({"kind": "picker", "picker": "model", "arg": "optional",
                   "argHint": "<model> [effort]"})
        ),
        LaneSlashRun {
            picker: Some(LaneSlashPicker::Model),
            arg: LaneSlashArg::Optional,
            arg_hint: Some("<model> [effort]".into()),
            ..bare_run(LaneSlashKind::Picker)
        }
    );
    assert_eq!(
        run(json!({"kind": "unavailable", "reason": "mode ask is not advertised"})).reason,
        Some("mode ask is not advertised".into())
    );
    assert_eq!(
        run(json!({"kind": "local", "local": "help"})).local,
        Some("help".into())
    );
    assert_eq!(
        method_of("session.setTitle"),
        LaneSlashMethod::SetTitle,
        "the wire's own constant"
    );
}

/// The rows the decoder produces round-trip through the CONTRACT's JSON as
/// bare strings — an unknown value included — so a mirror reads exactly what
/// the adapter decoded.
#[test]
fn decoded_rows_round_trip_as_bare_strings() {
    let c = catalog(&json!({"items": [
        {"name": "exit", "description": "End this session", "source": "builtin",
         "requires": "stop", "whileWorking": true, "run": {"kind": "method", "method": "session.stop"}},
        {"name": "review", "source": "plugin", "plugin": "pr-tools", "qualified": "pr-tools:review",
         "run": {"kind": "prompt", "arg": "optional"}},
        {"name": "odd", "source": "galaxy", "run": {"kind": "dance", "method": "session.dance"}},
        {"name": "bare", "source": "builtin", "run": {"kind": "method", "method": "stop"}},
    ]}));
    let encoded = serde_json::to_value(&c).unwrap();
    assert_eq!(
        encoded["rows"][0],
        json!({"name": "exit", "description": "End this session", "source": "builtin",
               "requires": "stop", "while_working": true,
               "run": {"kind": "method", "arg": "none", "method": "session.stop"}})
    );
    assert_eq!(encoded["rows"][2]["source"], "galaxy");
    assert_eq!(encoded["rows"][2]["run"]["kind"], "dance");
    assert_eq!(encoded["rows"][2]["run"]["method"], "session.dance");
    // A bare method word is not craze's: unknown in, unknown out.
    assert_eq!(
        c.rows[3].run.method,
        Some(LaneSlashMethod::Other("stop".into()))
    );
    assert_eq!(encoded["rows"][3]["run"]["method"], "stop");
    let back: LaneSlashCatalog = serde_json::from_value(encoded).unwrap();
    assert_eq!(back, c);
    assert_eq!(
        back.rows[3].run.method,
        Some(LaneSlashMethod::Other("stop".into()))
    );
}

/// **No cap in shed** (P16): craze bounds a SNAPSHOT's catalog at 512 rows;
/// a delta carries it whole, and so does the decoder — a 600-row section is
/// 600 rows, in order.
#[test]
fn a_600_row_delta_decodes_whole() {
    let items: Vec<Value> = (0..600)
        .map(|i| {
            json!({"name": format!("cmd-{i:03}"), "description": "Agent-advertised command",
                        "source": "agent", "run": {"kind": "prompt", "arg": "optional"}})
        })
        .collect();
    let got = section(&json!({"slash": {"items": items}})).expect("present");
    assert_eq!(got.rows.len(), 600);
    assert_eq!(got.rows[0].name, "cmd-000");
    assert_eq!(got.rows[599].name, "cmd-599");
}

/// The rows craze's own illustration names (PM "Slash catalog"), read whole —
/// the shape every client mirrors: the `local` help row, the `unavailable`
/// ask row, and the builtin `/btw` (requiring `aside`) beside the agent's.
#[test]
fn crazes_illustration_reads_whole() {
    let c = catalog(&json!({"items": [
      {"name":"help","description":"Keybindings and commands","source":"builtin","whileWorking":true,"run":{"kind":"local","local":"help"}},
      {"name":"ask","description":"Set ask mode","source":"builtin","run":{"kind":"unavailable","reason":"mode ask is not advertised"}},
      {"name":"btw","description":"Ask a side question (not saved): /btw <question>","source":"builtin","requires":"aside","whileWorking":true,"run":{"kind":"aside","param":"question","arg":"required","argHint":"<question>"}},
      {"name":"btw","description":"The agent's own btw","source":"agent","run":{"kind":"prompt","arg":"optional"}},
    ]}));
    let expect = vec![
        LaneSlashRow {
            name: "help".into(),
            description: Some("Keybindings and commands".into()),
            source: LaneSlashSource::Builtin,
            plugin: None,
            qualified: None,
            requires: None,
            while_working: true,
            run: LaneSlashRun {
                local: Some("help".into()),
                ..bare_run(LaneSlashKind::Local)
            },
        },
        LaneSlashRow {
            name: "ask".into(),
            description: Some("Set ask mode".into()),
            source: LaneSlashSource::Builtin,
            plugin: None,
            qualified: None,
            requires: None,
            while_working: false,
            run: LaneSlashRun {
                reason: Some("mode ask is not advertised".into()),
                ..bare_run(LaneSlashKind::Unavailable)
            },
        },
        LaneSlashRow {
            name: "btw".into(),
            description: Some("Ask a side question (not saved): /btw <question>".into()),
            source: LaneSlashSource::Builtin,
            plugin: None,
            qualified: None,
            requires: Some(LaneSlashRequires::Aside),
            while_working: true,
            run: LaneSlashRun {
                param: Some("question".into()),
                arg: LaneSlashArg::Required,
                arg_hint: Some("<question>".into()),
                ..bare_run(LaneSlashKind::Aside)
            },
        },
        LaneSlashRow {
            name: "btw".into(),
            description: Some("The agent's own btw".into()),
            source: LaneSlashSource::Agent,
            plugin: None,
            qualified: None,
            requires: None,
            while_working: false,
            run: LaneSlashRun {
                arg: LaneSlashArg::Optional,
                ..bare_run(LaneSlashKind::Prompt)
            },
        },
    ];
    assert_eq!(c.rows, expect);
}
