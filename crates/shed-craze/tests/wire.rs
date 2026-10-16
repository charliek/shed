//! shed-craze against craze's own wire fixtures — `fixtures/wire/`, WIRE/01–33
//! vendored verbatim at the pin `fixtures/wire.PIN` names (plan 025 §3.3.1),
//! and diffed against craze's tree by CI's `craze-binaries` action.
//!
//! Three claims, each over the fixtures as craze recorded them:
//!
//! - **Every `s2c` line decodes**: as a reply or a notification, and — where
//!   this crate reads that method's result — into its typed result (a hub's
//!   and a host's `hello`, the roster, `createOptions`, a create, every
//!   refusal's `data.code`). The side-question (`aside`) notifications of
//!   WIRE/27–28 are recognised and not read: shed asks no side question yet
//!   (plan 029 X1).
//! - **Every request this crate composes is the fixture's `c2s` line, field
//!   for field**: `hello` (the hub's and the host's), `sessions.subscribe`,
//!   `sessions.createOptions` and `session.create` — composed by the wire types
//!   AND written by the source itself, replayed against the fixture's own
//!   answers — and the lane's `session.connect`, `sessions.list`,
//!   `session.attach` (with and without a cursor), `session.prompt`,
//!   `session.cancel`, `asks.get`, `asks.answer`, `session.stop`, the
//!   fenced read's `session.sync` (Amendment A11), `session.set` (WIRE/31's
//!   mode change) and `session.setTitle` (WIRE/31's rename) — and
//!   `session.create`'s `title` (WIRE/32, plan 029 C2). `hello`'s `client` is
//!   the one member that differs, by design (`{kind: "shed", name, version}`,
//!   plan 025 §3.3.2); everything else is compared exactly, ids included.
//!   The lane requests no fixture line composes as this crate does —
//!   `session.snapshot` at its default budget and the fenced read's
//!   `asks.list` (no fixture calls either) — are pinned by hand, and so are
//!   `session.set` whole (its model and config shapes, which no fixture line
//!   carries) and `session.setTitle`.
//! - **The slash catalog and the title (plan 029)**: WIRE/30's and WIRE/31's
//!   catalogs — the snapshot's and each delta's — fold into the rows craze
//!   wrote, the rename's delta into the title, and the hub's `createTitle`
//!   and the session's `slashCatalog` into the contract's capabilities.
//! - **What the source makes of those answers** is the contract's: rows keyed
//!   by hostId, an upsert with an open ask, a remove; craze's provider order;
//!   the created row and its prompt; P14's start failure with its cause.

use std::collections::HashMap;
use std::path::PathBuf;
use std::sync::Arc;

use serde_json::{json, Value};
use shed_core::lane::conformance::{drive_source, SourceChecker};
use shed_core::lane::{
    AgentSource, LaneCreateRequest, LaneError, LanePromptOutcome, LaneProviderState,
    LaneSettingChange, LaneSlashArg, LaneSlashKind, LaneSlashMethod, LaneSlashPicker,
    LaneSlashRequires, LaneSlashRow, LaneSlashSource, SourceEvent,
};
use shed_core::rc::RcActivity;
use shed_craze::conn::{check_protocol_and_codecs, judge_hub_hello};
use shed_craze::testing::{HubEnd, ScriptedDial, SCRIPT_WAIT};
use shed_craze::wire::{
    self, method, ClientInfo, CreateOptionsResult, CreateParams, CreateResult, Empty, HelloParams,
    HelloResult, HostRow, Incoming, ResetParams, RosterParams, RosterRow, SubscribeResult,
};
use shed_craze::{source_capabilities, CrazeSource};

fn fixtures_dir() -> PathBuf {
    PathBuf::from(env!("CARGO_MANIFEST_DIR")).join("fixtures")
}

/// Every fixture, by file name, as its lines.
fn fixtures() -> Vec<(String, Vec<Value>)> {
    let mut names: Vec<String> = std::fs::read_dir(fixtures_dir().join("wire"))
        .expect("fixtures/wire")
        .flatten()
        .map(|e| e.file_name().to_string_lossy().into_owned())
        .filter(|n| n.ends_with(".ndjson"))
        .collect();
    names.sort();
    names
        .into_iter()
        .map(|n| {
            let text = std::fs::read_to_string(fixtures_dir().join("wire").join(&n)).unwrap();
            let lines = text
                .lines()
                .map(|l| serde_json::from_str(l).unwrap_or_else(|e| panic!("{n}: {e}: {l}")))
                .collect();
            (n, lines)
        })
        .collect()
}

fn fixture(prefix: &str) -> Vec<Value> {
    fixtures()
        .into_iter()
        .find(|(n, _)| n.starts_with(prefix))
        .unwrap_or_else(|| panic!("no fixture {prefix}"))
        .1
}

#[test]
fn the_fixtures_are_vendored_whole_at_the_pin() {
    let all = fixtures();
    assert_eq!(all.len(), 33, "WIRE/01–33");
    for (i, (name, _)) in all.iter().enumerate() {
        assert!(name.starts_with(&format!("{:02}-", i + 1)), "{name}");
    }
    assert!(
        fixtures_dir().join("wire/README.md").is_file(),
        "craze's own README is vendored too, so CI's drift diff compares like with like"
    );
    let pin = std::fs::read_to_string(fixtures_dir().join("wire.PIN")).unwrap();
    let pin = pin.trim();
    assert_eq!(pin.len(), 40, "wire.PIN is a full sha");
    assert!(pin
        .bytes()
        .all(|b| b.is_ascii_hexdigit() && !b.is_ascii_uppercase()));
}

/// Every `s2c` line decodes, and every result this crate reads decodes into
/// its type.
#[test]
fn every_s2c_line_decodes() {
    let mut replies = 0;
    let mut notifications = 0;
    for (name, lines) in fixtures() {
        // (conn, id) → method, from the c2s lines; conn → hub?
        let mut asked: HashMap<(u64, String), String> = HashMap::new();
        let mut hub_conn: HashMap<u64, bool> = HashMap::new();
        for line in &lines {
            let (Some(conn), Some(msg)) = (line["conn"].as_u64(), line.get("msg")) else {
                continue;
            };
            hub_conn.entry(conn).or_insert(line["sock"] == "hub");
            if line["dir"] == "c2s" {
                // Keyed by the id's compact JSON, as the client keys it.
                asked.insert(
                    (conn, msg["id"].to_string()),
                    msg["method"].as_str().unwrap().to_string(),
                );
                continue;
            }
            let incoming = wire::parse_line(&serde_json::to_vec(msg).unwrap())
                .unwrap_or_else(|e| panic!("{name}: an s2c line did not decode: {e:?}: {msg}"));
            match incoming {
                Incoming::Reply { id, outcome } => {
                    replies += 1;
                    let method = &asked[&(conn, id.to_string())];
                    match outcome {
                        Ok(result) => decode_result(&name, method, hub_conn[&conn], &result),
                        Err(e) => {
                            let code = e.data_code.as_deref().unwrap_or_else(|| {
                                panic!("{name}: a refusal with no data.code: {msg}")
                            });
                            assert!(
                                KNOWN_CODES.contains(&code),
                                "{name}: data.code {code} is craze's closed set"
                            );
                        }
                    }
                }
                Incoming::Notification { method, params } => {
                    notifications += 1;
                    match method.as_str() {
                        "roster" => {
                            let p: RosterParams = serde_json::from_value(params).unwrap();
                            for row in &p.upserts {
                                let r = RosterRow::from_value(row).unwrap();
                                assert!(!r.malformed, "{name}: {row}");
                            }
                        }
                        "reset" => {
                            let p: ResetParams = serde_json::from_value(params).unwrap();
                            assert!(!p.reason.is_empty());
                        }
                        // The attachment's: each decodes as the lane reads
                        // it, and every event body folds without a panic.
                        "event" => {
                            let p: wire::EventParams = serde_json::from_value(params).unwrap();
                            let mut fold = shed_craze::fold::CrazeFold::new("0123456789ab");
                            fold.apply(&p.event);
                        }
                        "synchronized" => {
                            let _: wire::SyncParams = serde_json::from_value(params).unwrap();
                        }
                        "presence" => {
                            let p: wire::PresenceParams = serde_json::from_value(params).unwrap();
                            assert!(p.attached >= 1, "{name}");
                        }
                        "ready" => {
                            let _: wire::ReadyParams = serde_json::from_value(params).unwrap();
                        }
                        // A side question's streamed answer (craze 0.1.1,
                        // WIRE/27–28): recognised and skipped — shed asks no
                        // side question yet (plan 029 X1), so nothing reads it.
                        "aside" => {
                            assert!(params["asideId"].is_string(), "{name}: {params}");
                        }
                        other => panic!("{name}: an unexpected notification {other}"),
                    }
                }
            }
        }
    }
    assert!(
        replies > 100 && notifications > 40,
        "{replies} replies, {notifications} notifications"
    );
}

const KNOWN_CODES: &[&str] = &[
    "bad_request",
    "stale_version",
    "stale_turn",
    "unknown_session",
    "unknown_ask",
    "already_submitted",
    "already_resolved",
    "not_accepting",
    "foreign_turn",
    "in_progress",
    "stale_model",
    "unavailable",
    "unsupported",
    "queue_full",
    "text_too_long",
    "prompt_in_flight",
    "prompt_cancelled",
    "unknown_row",
    "unknown_command",
    "unknown_subagent",
    "aborted",
    "failed",
    "index_write",
];

fn decode_result(name: &str, method: &str, hub: bool, result: &Value) {
    match method {
        "hello" => {
            let hello: HelloResult = serde_json::from_value(result.clone()).unwrap();
            check_protocol_and_codecs(&hello).unwrap_or_else(|e| panic!("{name}: {e}"));
            // A hub connection's SECOND hello is the host's, through the
            // splice (WIRE/21): the endpoint says which answered.
            assert!(hub || hello.endpoint.kind == "host", "{name}");
            if hello.endpoint.kind == "hub" {
                judge_hub_hello(result)
                    .unwrap_or_else(|e| panic!("{name}: a hub hello this crate refuses: {e}"));
            }
        }
        "sessions.subscribe" => {
            let r: SubscribeResult = serde_json::from_value(result.clone()).unwrap();
            for row in &r.sessions {
                assert!(!RosterRow::from_value(row).unwrap().malformed, "{name}");
            }
        }
        "sessions.list" => {
            for row in result["sessions"].as_array().unwrap() {
                if hub {
                    assert!(!RosterRow::from_value(row).unwrap().malformed, "{name}");
                } else {
                    let _: HostRow = serde_json::from_value(row.clone())
                        .unwrap_or_else(|e| panic!("{name}: {e}"));
                    // The lane reads a host's row whole: the session id to
                    // carry, and the row.
                    let (roster, _) = RosterRow::from_host_row(row)
                        .unwrap_or_else(|e| panic!("{name}: a host row the lane refuses: {e}"));
                    assert_eq!(roster.session_id, "session-fake-1", "{name}");
                }
            }
        }
        "sessions.createOptions" => {
            let _: CreateOptionsResult = serde_json::from_value(result.clone()).unwrap();
        }
        "session.create" => {
            let r: CreateResult = serde_json::from_value(result.clone()).unwrap();
            assert!(
                !RosterRow::from_value(&r.session).unwrap().malformed,
                "{name}"
            );
        }
        // The lane's (C8).
        "session.attach" => {
            let r: wire::AttachResult = serde_json::from_value(result.clone())
                .unwrap_or_else(|e| panic!("{name}: an attach reply: {e}"));
            if let Some(snap) = &r.snapshot {
                let (_rows, restored) = shed_craze::fold::snapshot_rows("0123456789ab", snap)
                    .unwrap_or_else(|e| panic!("{name}: a snapshot the fold refuses: {e}"));
                assert_eq!(
                    (restored.incarnation.as_str(), restored.seq),
                    (r.after.incarnation.as_str(), r.after.seq),
                    "{name}: a snapshot is cut where the stream continues"
                );
            } else {
                assert!(r.reset.is_none(), "{name}");
            }
        }
        "session.snapshot" => {
            shed_craze::fold::snapshot_rows("0123456789ab", &result["snapshot"])
                .unwrap_or_else(|e| panic!("{name}: {e}"));
        }
        "asks.get" => {
            let _: wire::AskGetResult =
                serde_json::from_value(result.clone()).unwrap_or_else(|e| panic!("{name}: {e}"));
        }
        // The fenced read's (Amendment A11).
        "session.sync" => {
            let _: wire::SyncResult =
                serde_json::from_value(result.clone()).unwrap_or_else(|e| panic!("{name}: {e}"));
        }
        "asks.list" => {
            let _: wire::AsksListResult =
                serde_json::from_value(result.clone()).unwrap_or_else(|e| panic!("{name}: {e}"));
        }
        _ => {}
    }
}

/// The request `wire` composes for `method` with the fixture line's params,
/// `hello`'s client set to the fixture's own words (its `version` dropped, the
/// one member a fixture line does not carry).
fn composed(msg: &Value) -> Value {
    let id = msg["id"].as_str().unwrap();
    let p = &msg["params"];
    let line = match msg["method"].as_str().unwrap() {
        method::HELLO => {
            let client = ClientInfo {
                kind: p["client"]["kind"].as_str().unwrap().into(),
                name: p["client"]["name"].as_str().unwrap().into(),
                version: String::new(),
            };
            let mut v: Value = serde_json::from_slice(
                &wire::request_line(id, method::HELLO, &HelloParams::new(client)).unwrap(),
            )
            .unwrap();
            v["params"]["client"]
                .as_object_mut()
                .unwrap()
                .remove("version");
            return v;
        }
        m @ (method::SESSIONS_SUBSCRIBE | method::SESSIONS_CREATE_OPTIONS) => {
            wire::request_line(id, m, &Empty {})
        }
        method::SESSION_CREATE => wire::request_line(
            id,
            method::SESSION_CREATE,
            &CreateParams {
                cwd: p["cwd"].as_str().unwrap().into(),
                prompt: p["prompt"].as_str().map(Into::into),
                provider: p["provider"].as_str().map(Into::into),
                request_id: p["requestId"].as_str().unwrap().into(),
                title: p["title"].as_str().map(Into::into),
            },
        ),
        // The lane's (C8).
        m @ method::SESSIONS_LIST => wire::request_line(id, m, &Empty {}),
        m @ method::SESSION_CONNECT => wire::request_line(
            id,
            m,
            &wire::ConnectParams {
                session_id: str_of(p, "sessionId"),
            },
        ),
        m @ method::SESSION_ATTACH => wire::request_line(
            id,
            m,
            &wire::AttachParams {
                session_id: str_of(p, "sessionId"),
                cursor: p
                    .get("cursor")
                    .map(|c| serde_json::from_value(c.clone()).unwrap()),
            },
        ),
        m @ method::SESSION_PROMPT => wire::request_line(
            id,
            m,
            &wire::PromptParams {
                session_id: str_of(p, "sessionId"),
                command_id: str_of(p, "commandId"),
                text: str_of(p, "text"),
                mode: match p["mode"].as_str().unwrap() {
                    "queue" => "queue",
                    "interject" => "interject",
                    other => panic!("a mode this crate never sends: {other}"),
                },
            },
        ),
        m @ (method::SESSION_CANCEL | method::SESSION_STOP) => wire::request_line(
            id,
            m,
            &wire::CommandParams {
                session_id: str_of(p, "sessionId"),
                command_id: str_of(p, "commandId"),
            },
        ),
        m @ method::SESSION_SYNC => wire::request_line(
            id,
            m,
            &wire::SessionParams {
                session_id: str_of(p, "sessionId"),
            },
        ),
        m @ method::ASKS_GET => wire::request_line(
            id,
            m,
            &wire::AskParams {
                session_id: str_of(p, "sessionId"),
                ask_id: str_of(p, "askId"),
            },
        ),
        m @ method::ASKS_ANSWER => wire::request_line(
            id,
            m,
            &wire::AnswerParams {
                session_id: str_of(p, "sessionId"),
                command_id: str_of(p, "commandId"),
                ask_id: str_of(p, "askId"),
                answer: p["answer"].clone(),
            },
        ),
        // The rename (plan 029 D8): the title verbatim — the host normalizes.
        m @ method::SESSION_SET_TITLE => wire::request_line(
            id,
            m,
            &wire::SetTitleParams {
                session_id: str_of(p, "sessionId"),
                command_id: str_of(p, "commandId"),
                title: str_of(p, "title"),
            },
        ),
        // The change as a client asks for it, bound to no folded model: the
        // fixture's `setting` is exactly what `setting_for` must make of it.
        m @ method::SESSION_SET => {
            let s = &p["setting"];
            let change = match s["kind"].as_str().unwrap() {
                "model" => LaneSettingChange::Model {
                    id: str_of(s, "value"),
                },
                "mode" => LaneSettingChange::Mode {
                    id: str_of(s, "value"),
                },
                "config" => LaneSettingChange::Config {
                    id: str_of(s, "id"),
                    value: str_of(s, "value"),
                    for_model: s["forModel"].as_str().map(Into::into),
                },
                other => panic!("a setting kind this crate never sends: {other}"),
            };
            wire::request_line(
                id,
                m,
                &wire::SetParams {
                    session_id: str_of(p, "sessionId"),
                    command_id: str_of(p, "commandId"),
                    setting: shed_craze::setting_for(&change, None),
                },
            )
        }
        other => panic!("not a method this crate composes: {other}"),
    };
    serde_json::from_slice(&line.unwrap()).unwrap()
}

fn str_of(p: &Value, k: &str) -> String {
    p[k].as_str()
        .unwrap_or_else(|| panic!("{k} in {p}"))
        .to_string()
}

/// Every c2s line of every method this crate composes is what `wire` composes
/// — `session.create`'s `model` lines excepted (D6 composes no model; WIRE/22
/// and WIRE/32 each carry one). WIRE/32's titled creates are composed (plan
/// 029 C2), the title verbatim.
#[test]
fn every_composed_request_is_the_fixtures_c2s_line() {
    let mut seen: HashMap<&str, usize> = HashMap::new();
    let mut titled_creates = 0;
    for (name, lines) in fixtures() {
        for line in lines
            .iter()
            .filter(|l| l["dir"] == "c2s" && l["invalid"] != true)
        {
            let msg = &line["msg"];
            let m = msg["method"].as_str().unwrap();
            let m: &str = match m {
                method::HELLO => method::HELLO,
                method::SESSIONS_SUBSCRIBE => method::SESSIONS_SUBSCRIBE,
                method::SESSIONS_CREATE_OPTIONS => method::SESSIONS_CREATE_OPTIONS,
                method::SESSION_CREATE => method::SESSION_CREATE,
                method::SESSION_CONNECT => method::SESSION_CONNECT,
                method::SESSIONS_LIST => method::SESSIONS_LIST,
                method::SESSION_ATTACH => method::SESSION_ATTACH,
                method::SESSION_PROMPT => method::SESSION_PROMPT,
                method::SESSION_CANCEL => method::SESSION_CANCEL,
                method::SESSION_STOP => method::SESSION_STOP,
                method::ASKS_GET => method::ASKS_GET,
                method::ASKS_ANSWER => method::ASKS_ANSWER,
                method::SESSION_SYNC => method::SESSION_SYNC,
                method::SESSION_SET => method::SESSION_SET,
                method::SESSION_SET_TITLE => method::SESSION_SET_TITLE,
                _ => continue,
            };
            if m == method::SESSION_CREATE && msg["params"].get("model").is_some() {
                assert!(
                    name == "22-hub-create.ndjson" || name == "32-create-title.ndjson",
                    "{name}"
                );
                continue;
            }
            if m == method::SESSION_CREATE && msg["params"].get("title").is_some() {
                assert_eq!(name, "32-create-title.ndjson");
                titled_creates += 1;
            }
            // A lowered budget (WIRE/04's deliberate slow consumer): the lane
            // attaches at the host's default.
            if m == method::SESSION_ATTACH && msg["params"].get("budget").is_some() {
                assert_eq!(name, "04-slow-consumer-reattach.ndjson");
                continue;
            }
            // A host hello taking a client id back: no token resume in plan
            // 025 (§3.3.4), so no shed hello carries `resume`.
            if m == method::HELLO && msg["params"].get("resume").is_some() {
                assert_eq!(name, "13-hello-resume-tokens.ndjson");
                continue;
            }
            assert_eq!(&composed(msg), msg, "{name}: {m}");
            *seen.entry(m).or_default() += 1;
        }
    }
    assert_eq!(seen[method::SESSIONS_SUBSCRIBE], 1);
    assert_eq!(seen[method::SESSIONS_CREATE_OPTIONS], 2);
    // WIRE/22's three, and WIRE/32's six: its five titled creates (its model
    // line aside) and its one untitled replay of a titled request id.
    assert_eq!(seen[method::SESSION_CREATE], 9);
    assert_eq!(titled_creates, 5, "WIRE/32's titled creates, composed");
    assert_eq!(seen[method::SESSION_SET], 1, "WIRE/31's mode change");
    assert_eq!(seen[method::SESSION_SET_TITLE], 1, "WIRE/31's rename");
    assert!(seen[method::HELLO] >= 40, "{}", seen[method::HELLO]);
    // The lane's, each with at least one fixture line.
    for m in [
        method::SESSION_CONNECT,
        method::SESSIONS_LIST,
        method::SESSION_ATTACH,
        method::SESSION_PROMPT,
        method::SESSION_CANCEL,
        method::SESSION_STOP,
        method::ASKS_GET,
        method::ASKS_ANSWER,
        method::SESSION_SYNC,
    ] {
        assert!(
            seen.get(m).is_some_and(|n| *n >= 1),
            "no fixture line for {m}"
        );
    }
    assert!(
        seen[method::SESSION_ATTACH] >= 20,
        "with and without a cursor"
    );
}

/// `session.snapshot`, the one lane request no fixture line composes as the
/// lane does (every fixture line names a budget or a sub-agent): pinned by
/// hand — the session id, at the host's default budget.
#[test]
fn the_snapshot_request_is_pinned_by_hand() {
    let line = wire::request_line(
        "7",
        method::SESSION_SNAPSHOT,
        &wire::SessionParams {
            session_id: "session-fake-1".into(),
        },
    )
    .unwrap();
    assert_eq!(
        std::str::from_utf8(&line).unwrap(),
        r#"{"jsonrpc":"2.0","id":"7","method":"session.snapshot","params":{"sessionId":"session-fake-1"}}"#
    );
}

/// `asks.list`, the fenced read's registry (Amendment A11), which no fixture
/// line calls: pinned by hand — the session id and nothing else.
#[test]
fn the_registry_request_is_pinned_by_hand() {
    let line = wire::request_line(
        "8",
        method::ASKS_LIST,
        &wire::SessionParams {
            session_id: "session-fake-1".into(),
        },
    )
    .unwrap();
    assert_eq!(
        std::str::from_utf8(&line).unwrap(),
        r#"{"jsonrpc":"2.0","id":"8","method":"asks.list","params":{"sessionId":"session-fake-1"}}"#
    );
    // Its answer as craze's protocol reference writes it.
    let r: wire::AsksListResult = serde_json::from_value(json!({"asks": [
        {"id": "perm-1", "kind": "permission", "label": "Shell", "openedAt": "2026-01-01T00:00:00Z"}]}))
    .unwrap();
    assert_eq!(r.asks[0].id, "perm-1");
}

/// `session.set` (the settings milestone, C11), whose one fixture line
/// (WIRE/31) changes a mode only: pinned by hand whole against craze's closed
/// schema (`session.set.json`) — the
/// session and command ids, and the setting: a model or a mode by its value
/// alone, a config option by its id and value with `forModel`, the model it
/// was chosen for. Its answer is the confirmed value and the delta's seq.
#[test]
fn the_set_request_is_pinned_by_hand() {
    let line = |cmd: &str, change: LaneSettingChange, model: Option<&str>| {
        let l = wire::request_line(
            "9",
            method::SESSION_SET,
            &wire::SetParams {
                session_id: "session-fake-1".into(),
                command_id: cmd.into(),
                setting: shed_craze::setting_for(&change, model),
            },
        )
        .unwrap();
        String::from_utf8(l).unwrap()
    };
    assert_eq!(
        line(
            "3",
            LaneSettingChange::Config {
                id: "effort".into(),
                value: "low".into(),
                for_model: Some("grok-4.6".into()),
            },
            Some("claude-opus-5")
        ),
        r#"{"jsonrpc":"2.0","id":"9","method":"session.set","params":{"sessionId":"session-fake-1","commandId":"3","setting":{"kind":"config","id":"effort","value":"low","forModel":"grok-4.6"}}}"#
    );
    assert_eq!(
        line(
            "4",
            LaneSettingChange::Model {
                id: "claude-opus-5".into()
            },
            Some("grok-4.6")
        ),
        r#"{"jsonrpc":"2.0","id":"9","method":"session.set","params":{"sessionId":"session-fake-1","commandId":"4","setting":{"kind":"model","value":"claude-opus-5"}}}"#
    );
    assert_eq!(
        line("5", LaneSettingChange::Mode { id: "plan".into() }, None),
        r#"{"jsonrpc":"2.0","id":"9","method":"session.set","params":{"sessionId":"session-fake-1","commandId":"5","setting":{"kind":"mode","value":"plan"}}}"#
    );
    // Its answer as the schema writes it.
    let r: wire::SetResult = serde_json::from_value(json!({"value": "low", "rev": 4})).unwrap();
    assert_eq!((r.value.as_str(), r.rev), ("low", Some(4)));
    // `rev: 0` is craze's "confirmed, and no event will carry it" — read as
    // 0, while an answer that does not say is no revision at all.
    let r: wire::SetResult = serde_json::from_value(json!({"value": "low", "rev": 0})).unwrap();
    assert_eq!(r.rev, Some(0));
    let r: wire::SetResult = serde_json::from_value(json!({"value": "low"})).unwrap();
    assert_eq!(r.rev, None);
}

/// `session.setTitle` (plan 029 D8), whose one fixture line (WIRE/31) is
/// composed above: pinned by hand whole against craze's closed schema
/// (`session.setTitle.json`) — the session and command ids, and the title,
/// verbatim — nothing else, and an answer of `{}`.
#[test]
fn the_set_title_request_is_pinned_by_hand() {
    let line = wire::request_line(
        "9",
        method::SESSION_SET_TITLE,
        &wire::SetTitleParams {
            session_id: "session-fake-1".into(),
            command_id: "6".into(),
            title: "  plan 029 live ".into(),
        },
    )
    .unwrap();
    assert_eq!(
        std::str::from_utf8(&line).unwrap(),
        r#"{"jsonrpc":"2.0","id":"9","method":"session.setTitle","params":{"sessionId":"session-fake-1","commandId":"6","title":"  plan 029 live "}}"#
    );
    // The schema's answer: `{}` — the delta, not the reply, says the title.
    let reply = fixture("31-")
        .into_iter()
        .filter(|l| l["dir"] == "s2c")
        .find(|l| l["msg"]["id"] == "4")
        .unwrap();
    assert_eq!(reply["msg"]["result"], json!({}));
}

/// The rows a catalog section folds into, by name.
fn slash_names(rows: &[LaneSlashRow]) -> Vec<&str> {
    rows.iter().map(|r| r.name.as_str()).collect()
}

/// The `s2c` messages of conn 1 of a fixture, in order.
fn s2c(lines: &[Value]) -> Vec<Value> {
    lines
        .iter()
        .filter(|l| l["dir"] == "s2c" && l["conn"] == 1)
        .map(|l| l["msg"].clone())
        .collect()
}

/// **WIRE/30 (an ACP session's catalog) folds into the rows craze wrote**
/// (plan 029 AC3): the attach snapshot's `settings.slash` — every builtin of
/// a cursor session that advertises its modes, the `local` help row, the
/// picker, the rename, the mode rows resolved to ids, `/btw` requiring
/// `aside` and `/exit` requiring `stop` — then each delta replacing it whole:
/// the agent's own `/btw` listed BESIDE the builtin (a `requires` row claims
/// no name), then a disk skill, the one its command shadows not listed
/// twice. The second connection's snapshot carries the catalog as the
/// deltas left it. The session says `slashCatalog`, so the lane's
/// capabilities say `slash`.
#[test]
fn wire_30_folds_into_crazes_catalog() {
    let lines = fixture("30-");
    let msgs = s2c(&lines);
    let attach: wire::AttachResult = serde_json::from_value(msgs[1]["result"].clone()).unwrap();
    assert!(shed_craze::lane_capabilities(&attach.session.capabilities, true).slash);
    let mut fold = shed_craze::fold::CrazeFold::new("0123456789ab");
    fold.restore(attach.snapshot.as_ref().unwrap()).unwrap();
    let seeded = fold
        .slash()
        .expect("the snapshot carries the catalog")
        .clone();
    assert_eq!(
        slash_names(&seeded.rows),
        [
            "help", "model", "clear", "tasks", "theme", "rename", "plan", "ask", "agent", "btw",
            "exit", "effort", "research"
        ]
    );
    let row = |name: &str| seeded.rows.iter().find(|r| r.name == name).unwrap().clone();
    let help = row("help");
    assert_eq!(
        (
            &help.source,
            help.while_working,
            &help.run.kind,
            help.run.local.as_deref()
        ),
        (
            &LaneSlashSource::Builtin,
            true,
            &LaneSlashKind::Local,
            Some("help")
        )
    );
    let model = row("model");
    assert_eq!(model.run.kind, LaneSlashKind::Picker);
    assert_eq!(model.run.picker, Some(LaneSlashPicker::Model));
    assert_eq!(model.run.arg, LaneSlashArg::Optional);
    assert!(!model.while_working);
    let rename = row("rename");
    assert_eq!(rename.run.method, Some(LaneSlashMethod::SetTitle));
    assert_eq!(rename.run.param.as_deref(), Some("title"));
    assert_eq!(rename.run.arg, LaneSlashArg::Required);
    assert_eq!(rename.run.arg_hint.as_deref(), Some("<title>"));
    assert!(rename.while_working);
    for mode in ["plan", "ask", "agent"] {
        let r = row(mode);
        assert_eq!(r.run.method, Some(LaneSlashMethod::Set), "{mode}");
        assert_eq!(r.run.mode.as_deref(), Some(mode), "resolved to its id");
    }
    let btw = row("btw");
    assert_eq!(btw.requires, Some(LaneSlashRequires::Aside));
    assert_eq!(btw.run.kind, LaneSlashKind::Aside);
    let exit = row("exit");
    assert_eq!(exit.requires, Some(LaneSlashRequires::Stop));
    assert_eq!(exit.run.method, Some(LaneSlashMethod::Stop));
    // craze 0.2.0's `/effort`: a picker shed has no verb for yet, kept as
    // craze wrote it.
    let effort = row("effort");
    assert_eq!(
        (&effort.requires, &effort.run.kind, &effort.run.picker),
        (
            &Some(LaneSlashRequires::Other("effort".into())),
            &LaneSlashKind::Picker,
            &Some(LaneSlashPicker::Other("effort".into()))
        )
    );
    let research = row("research");
    assert_eq!(
        (&research.source, &research.run.kind, research.while_working),
        (&LaneSlashSource::Agent, &LaneSlashKind::Prompt, false)
    );

    // The agent's catalog changes: a delta carrying commands restates the
    // catalog whole beside them.
    let delta: wire::EventParams = serde_json::from_value(msgs[3]["params"].clone()).unwrap();
    fold.apply(&delta.event);
    assert!(fold.take_slash_changed());
    let rows = &fold.slash().unwrap().rows;
    assert_eq!(rows.len(), 15);
    let btws: Vec<&LaneSlashRow> = rows.iter().filter(|r| r.name == "btw").collect();
    assert_eq!(btws.len(), 2, "the agent's /btw beside the builtin");
    assert_eq!(
        (&btws[0].source, &btws[1].source),
        (&LaneSlashSource::Builtin, &LaneSlashSource::Agent)
    );
    assert_eq!(btws[1].description.as_deref(), Some("The agent's own btw"));
    assert_eq!(btws[1].run.kind, LaneSlashKind::Prompt);
    // The same commands again: no delta at all (craze publishes nothing for
    // an update that changes nothing). Then the disk skills: a slash-only
    // delta, the shadowed `review` skill not listed twice.
    let skills: wire::EventParams = serde_json::from_value(msgs[4]["params"].clone()).unwrap();
    assert!(
        skills.event["state"].get("commands").is_none(),
        "slash alone"
    );
    fold.apply(&skills.event);
    let rows = &fold.slash().unwrap().rows;
    assert_eq!(rows.len(), 16);
    let last = rows.last().unwrap();
    assert_eq!(
        (last.name.as_str(), &last.source),
        ("deploy", &LaneSlashSource::Skill)
    );
    assert_eq!(rows.iter().filter(|r| r.name == "review").count(), 1);
    let held = fold.slash().unwrap().clone();

    // A second connection's attach: its snapshot carries the catalog the
    // deltas left.
    let reply = conn_lines(&lines, 2)
        .into_iter()
        .find(|l| l["dir"] == "s2c" && l["msg"]["id"] == "2")
        .unwrap();
    let again: wire::AttachResult = serde_json::from_value(reply["msg"]["result"].clone()).unwrap();
    let mut reseeded = shed_craze::fold::CrazeFold::new("0123456789ab");
    reseeded.restore(again.snapshot.as_ref().unwrap()).unwrap();
    assert_eq!(reseeded.slash(), Some(&held), "snapshot = the deltas' fold");
}

/// **WIRE/31 (a native session's catalog, and a rename)**: the native rows —
/// `/connect` local, a plugin row with its `qualified` spelling and one
/// whose bare name a builtin took (listed as `pr-tools:help`, no second
/// spelling) — then the rename: the composed `session.setTitle` line (above),
/// its `meta` delta folding into the title, its reply `{}`.
#[test]
fn wire_31_folds_into_the_native_catalog_and_the_renamed_title() {
    let msgs = s2c(&fixture("31-"));
    let attach: wire::AttachResult = serde_json::from_value(msgs[1]["result"].clone()).unwrap();
    assert!(shed_craze::lane_capabilities(&attach.session.capabilities, true).slash);
    let mut fold = shed_craze::fold::CrazeFold::new("0123456789ab");
    fold.restore(attach.snapshot.as_ref().unwrap()).unwrap();
    assert_eq!(fold.title(), None, "an untitled session");
    let rows = fold.slash().unwrap().rows.clone();
    let connect = rows.iter().find(|r| r.name == "connect").unwrap();
    assert_eq!(connect.run.local.as_deref(), Some("connect"));
    assert!(!connect.while_working);
    let review = rows.iter().find(|r| r.name == "review").unwrap();
    assert_eq!(
        (
            &review.source,
            review.plugin.as_deref(),
            review.qualified.as_deref()
        ),
        (
            &LaneSlashSource::Plugin,
            Some("pr-tools"),
            Some("pr-tools:review")
        )
    );
    let help = rows.iter().find(|r| r.name == "pr-tools:help").unwrap();
    assert_eq!(
        (help.plugin.as_deref(), help.qualified.as_deref()),
        (Some("pr-tools"), None)
    );
    assert_eq!(
        rows.iter().filter(|r| r.name == "help").count(),
        1,
        "the builtin's"
    );

    let renamed = msgs
        .iter()
        .find(|m| m["params"]["event"]["state"].get("title").is_some())
        .unwrap();
    fold.apply(&renamed["params"]["event"]);
    assert_eq!(fold.title(), Some("ship the release"));
    assert!(
        !fold.take_slash_changed(),
        "a title-only delta leaves the catalog"
    );
}

/// An older host's session (WIRE/01, craze 0.1.0's shape) says no
/// `slashCatalog`: the lane's capabilities say no `slash` (plan 029 P18).
#[test]
fn an_older_hosts_session_has_no_slash_capability() {
    let msgs = s2c(&fixture("01-"));
    let attach = msgs
        .iter()
        .find_map(|m| serde_json::from_value::<wire::AttachResult>(m["result"].clone()).ok())
        .unwrap();
    let caps = shed_craze::lane_capabilities(&attach.session.capabilities, true);
    assert!(!caps.slash && caps.set_title, "{caps:?}");
}

/// The create a fixture's `session.create` line asks for, every member as the
/// line has it.
fn create_request_of(p: &Value) -> LaneCreateRequest {
    LaneCreateRequest {
        cwd: p["cwd"].as_str().unwrap().into(),
        provider: p["provider"].as_str().map(Into::into),
        prompt: p["prompt"].as_str().map(Into::into),
        request_id: p["requestId"].as_str().unwrap().into(),
        title: p["title"].as_str().map(Into::into),
    }
}

/// A fixture conn's lines, in order.
fn conn_lines(lines: &[Value], conn: u64) -> Vec<Value> {
    lines
        .iter()
        .filter(|l| l["conn"] == conn)
        .cloned()
        .collect()
}

/// Play craze's side of a fixture connection: for each `c2s` line, read the
/// source's request and check it IS that line (client excepted, by design);
/// for each `s2c` line, write it verbatim.
async fn replay(hub: &mut HubEnd, script: &[Value]) {
    for line in script {
        let msg = &line["msg"];
        if line["dir"] == "c2s" {
            let mut ours = hub
                .recv()
                .await
                .expect("the source wrote the fixture's next request");
            if ours["method"] == "hello" {
                let client = ours["params"]["client"].take();
                assert_eq!(client["kind"], "shed", "{client}");
                assert!(
                    client["name"].is_string() && client["version"].is_string(),
                    "{client}"
                );
                let mut theirs = msg.clone();
                theirs["params"]["client"] = Value::Null;
                assert_eq!(ours, theirs, "hello, its client aside");
            } else {
                assert_eq!(&ours, msg, "the source's request is the fixture's c2s line");
            }
        } else {
            hub.send(msg).await;
        }
    }
}

/// WIRE/20 through the source: the hub's `hello`, a subscription seeded with
/// the fixture host's row, its upsert when an ask opens, its removal when it
/// leaves the registry — the source's requests are the fixture's own lines.
#[tokio::test]
async fn wire_20_through_the_source() {
    let lines = fixture("20-");
    let script: Vec<Value> = conn_lines(&lines, 1);
    let (dial, mut conns) = ScriptedDial::new();
    let source = CrazeSource::new(dial, "shed-wire-test");
    let (mut rx, _stop) = source.subscribe().await.unwrap().into_parts();
    let mut hub = conns.recv().await.unwrap();
    replay(&mut hub, &script).await;

    let mut checker = SourceChecker::new();
    let all = drive_source(&mut rx, &mut checker, SCRIPT_WAIT, |e| {
        matches!(e, SourceEvent::Removed { .. })
    })
    .await
    .expect("the stream conforms");
    let sessions: Vec<_> = all
        .frames
        .iter()
        .filter_map(|e| match e {
            SourceEvent::Session { session } => Some(session),
            _ => None,
        })
        .collect();
    assert_eq!(sessions.len(), 2, "the seeded row, then its upsert");
    let (seeded, upserted) = (sessions[0], sessions[1]);
    assert_eq!(seeded.id, "0123456789ab");
    assert_eq!(
        (seeded.cwd.as_str(), seeded.title.as_str()),
        ("/work", "work")
    );
    assert_eq!(seeded.activity, RcActivity::Idle);
    assert_eq!(seeded.provider, None, "the fixture host names no provider");
    assert_eq!(
        seeded.provider_session_id.as_deref(),
        Some("stub-session-1")
    );
    assert_eq!(upserted.id, "0123456789ab");
    assert_eq!(upserted.pending_approvals, 1);
    assert_eq!(upserted.activity, RcActivity::NeedsApproval);
    assert_eq!(
        upserted.head_ask_summary.as_deref(),
        Some("permission Shell"),
        "no rowFacts: the label"
    );
    assert_eq!(
        all.frames.last(),
        Some(&SourceEvent::Removed {
            session_id: "0123456789ab".into()
        })
    );
    // WIRE/20's hub does not create (sessionCreate false, no createOptions).
    assert!(all
        .frames
        .iter()
        .any(|e| matches!(e, SourceEvent::Capabilities { capabilities }
        if !capabilities.create && !capabilities.create_options)));
}

/// WIRE/19's and WIRE/26's hub `hello`s, through `source_capabilities`.
#[test]
fn the_capabilities_of_wire_19_and_26() {
    let hello_of = |lines: &[Value]| -> HelloResult {
        let reply = lines
            .iter()
            .find(|l| l["dir"] == "s2c" && l["sock"] == "hub")
            .unwrap();
        serde_json::from_value(reply["msg"]["result"].clone()).unwrap()
    };
    let old = source_capabilities(&hello_of(&fixture("19-")).capabilities);
    assert_eq!(
        (old.kind.as_str(), old.create, old.create_options),
        ("craze", false, false)
    );
    let new = source_capabilities(&hello_of(&fixture("26-")).capabilities);
    assert_eq!((new.create, new.create_options), (true, true));
    assert!(!new.create_title, "0.1.0's hub names no session at create");
    // Plan 029: WIRE/32's hub takes a title.
    let titled = source_capabilities(&hello_of(&fixture("32-")).capabilities);
    assert!(titled.create && titled.create_title);
}

/// **WIRE/32 through the source** (plan 029 D8): a titled create is the
/// fixture's own line — the title verbatim, unnormalized; the hub normalizes
/// it — and the created row carries the title craze stored, from the first
/// frame.
#[tokio::test]
async fn wire_32_through_the_source() {
    let lines = fixture("32-");
    let script: Vec<Value> = conn_lines(&lines, 1).into_iter().take(4).collect();
    let request = create_request_of(&script[2]["msg"]["params"]);
    assert!(request.title.as_deref().unwrap().starts_with("  Fix the\t"));
    let (dial, mut conns) = ScriptedDial::new();
    let source = CrazeSource::new(dial, "shed-wire-test");
    let create = tokio::spawn(async move { source.create(request).await });
    let mut hub = conns.recv().await.unwrap();
    replay(&mut hub, &script).await;
    let created = create.await.unwrap().unwrap();
    assert_eq!(created.session.id, "cccccccccccc");
    assert_eq!(
        created.session.title, "Fix the login bug !",
        "craze's normalized title"
    );
}

/// WIRE/26 through the source: `createOptions`, its answer as the contract's.
#[tokio::test]
async fn wire_26_through_the_source() {
    let lines = fixture("26-");
    // conn 1 up to its stray-member line, which no client composes.
    let script: Vec<Value> = conn_lines(&lines, 1).into_iter().take(4).collect();
    let (dial, mut conns) = ScriptedDial::new();
    let source = CrazeSource::new(dial, "shed-wire-test");
    let asked = tokio::spawn(async move { source.create_options().await });
    let mut hub = conns.recv().await.unwrap();
    replay(&mut hub, &script).await;
    let got = asked.await.unwrap().unwrap();
    let ids: Vec<_> = got
        .providers
        .iter()
        .map(|p| (p.id.as_str(), p.state.clone()))
        .collect();
    assert_eq!(
        ids,
        [
            ("cursor", LaneProviderState::Unavailable),
            ("grok", LaneProviderState::Ready),
            ("native", LaneProviderState::NeedsSetup)
        ]
    );
    assert!(got.providers[0]
        .reason
        .as_deref()
        .unwrap()
        .contains("macOS login session"));
    assert_eq!(got.default_provider.as_deref(), Some("grok"));
    assert_eq!(
        got.recent_dirs,
        ["/home/me/projects/lumen", "/home/me/projects/shed"]
    );
}

/// WIRE/22 through the source: the create, and the created row. Then the
/// fixture's `start_failed` answer to a create of ours: P14's cause.
#[tokio::test]
async fn wire_22_through_the_source() {
    let lines = fixture("22-");
    let conn = conn_lines(&lines, 1);
    // hello, its answer, the first create, its answer.
    let script: Vec<Value> = conn.iter().take(4).cloned().collect();
    let request = create_request_of(&script[2]["msg"]["params"]);
    let (dial, mut conns) = ScriptedDial::new();
    let source = Arc::new(CrazeSource::new(dial, "shed-wire-test"));
    let s = Arc::clone(&source);
    let create = tokio::spawn(async move { s.create(request).await });
    let mut hub = conns.recv().await.unwrap();
    replay(&mut hub, &script).await;
    let created = create.await.unwrap().unwrap();
    assert_eq!(
        created.session.id, "cccccccccccc",
        "the new row's id is its new hostId"
    );
    assert_eq!(created.session.cwd, "/");
    assert_eq!(created.session.activity, RcActivity::Working);
    assert_eq!(created.prompt, LanePromptOutcome::Accepted);

    // The fixture's start failure, answering a create of ours.
    let failure = conn.iter().rev().find(|l| l["dir"] == "s2c").unwrap()["msg"].clone();
    let s = Arc::clone(&source);
    let create = tokio::spawn(async move {
        s.create(LaneCreateRequest {
            cwd: "/".into(),
            provider: Some("cursor".into()),
            prompt: None,
            request_id: "fixture-22-b".into(),
            title: None,
        })
        .await
    });
    let mut hub = conns.recv().await.unwrap();
    replay(&mut hub, &conn[..2]).await;
    let ours = hub.expect("session.create").await;
    let mut answer = failure;
    answer["id"] = ours["id"].clone();
    hub.send(&answer).await;
    assert_eq!(
        create.await.unwrap().unwrap_err(),
        LaneError::Failed("the agent could not start: no such binary".into()),
        "P14: the cause, not NotAccepting"
    );
}

/// Every refusal line in the fixtures, through the table: its variant is the
/// one its `data.code` names.
#[test]
fn every_fixture_refusal_maps_by_its_code() {
    for (name, lines) in fixtures() {
        for line in lines.iter().filter(|l| l["dir"] == "s2c") {
            let Some(err) = line["msg"].get("error") else {
                continue;
            };
            let e = wire::RpcError::from_value(err);
            let mapped = shed_craze::lane_error(&e);
            let want = match e.data_code.as_deref().unwrap() {
                "bad_request" => matches!(mapped, LaneError::BadRequest(_)),
                "unknown_session" => mapped == LaneError::UnknownSession,
                "already_resolved" => mapped == LaneError::AlreadyResolved,
                "not_accepting" => mapped == LaneError::NotAccepting,
                "unavailable" => matches!(mapped, LaneError::Unavailable(_)),
                // `aborted`: WIRE/28's cancelled side question.
                "unsupported" | "unknown_subagent" | "aborted" => {
                    matches!(mapped, LaneError::Failed(_))
                }
                other => panic!("{name}: a code this test does not list: {other}"),
            };
            assert!(want, "{name}: {err} → {mapped:?}");
        }
    }
}

/// A hub's `hello` from WIRE/21 with its codecs bumped is refused as too old —
/// a codec this crate does not know is never folded.
#[test]
fn a_fixture_hello_with_another_codec_is_too_old() {
    let lines = fixture("21-");
    let reply = lines
        .iter()
        .find(|l| l["dir"] == "s2c" && l["sock"] == "hub")
        .unwrap();
    let mut result = reply["msg"]["result"].clone();
    assert!(judge_hub_hello(&result).is_ok());
    result["codecs"] = json!({"event": 2, "snapshot": 1});
    assert!(matches!(
        judge_hub_hello(&result),
        Err(shed_craze::conn::HelloError::TooOld(_))
    ));
}
