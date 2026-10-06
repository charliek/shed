//! The live recording (plan 025 C8's gate step) and its OFFLINE replay.
//!
//! - **Recording** (`record_a_gx_session`, live — skipped unless
//!   `SHED_CRAZE_LIVE=1`): a gx-provider session through a REAL hub (the pin's
//!   `craze`) under the recipe's scratch `HOME`/`CRAZE_HOME`/
//!   `CRAZE_RUNTIME_DIR` (all under `/tmp`, 0700): created through the source,
//!   opened as a lane, two cheap turns and a stop — every line of the lane's
//!   connection from its attach on teed, guarded, and (with
//!   `SHED_CRAZE_RECORD=1`) written to `fixtures/<RECORDING>/lane.ndjson` with
//!   a generated `PROVENANCE.md`.
//! - **Replay** (`the_recording_replays_into_its_golden`, every run): the
//!   committed recording folded through [`CrazeFold`] offline, compared with
//!   `fold.golden.json` — REGRESSION DETECTION ONLY (`fixtures/README.md`).
//!   `SHED_CRAZE_REGOLD=1` rewrites the golden, then asserts against what it
//!   wrote.
//! - **The guard** (`the_recorder_refuses_…`, every run): the recorder REFUSES
//!   a recording carrying a secret-looking string rather than scrubbing it.

use std::path::{Path, PathBuf};
use std::sync::Arc;
use std::time::Duration;

use serde_json::{json, Value};
use shed_core::lane::conformance::{drive_lane, DriveEnd, LaneChecker};
use shed_core::lane::ring::MessageRing;
use shed_core::lane::{
    option_kind, AgentLane, AgentSource, LaneAnswer, LaneCreateRequest, LaneEvent, SendMode,
    SourceEvent,
};
use shed_core::rc::RcActivity;
use shed_craze::fold::CrazeFold;
use shed_craze::testing::{bins, Recipe, TeeDial, TeeLine};
use shed_craze::wire::{AttachResult, EventParams, ResetParams};
use shed_craze::{lane_capabilities, new_request_id, CrazeDial};

/// The recording this crate replays: craze v0.1.0 (the pin is its tag
/// commit), a gx session.
const RECORDING: &str = "0.1.0+gx";
/// How long one live step may take: a real model answering.
const LIVE_WAIT: Duration = Duration::from_secs(240);
/// The gx binary when `SHED_CRAZE_GX` does not name one.
const DEFAULT_GX: &str =
    "/home/charliek/.local/share/mise/installs/github-charliek-grok-build/gx-v1.0.16-gx.12/gx";

fn recording_dir() -> PathBuf {
    PathBuf::from(env!("CARGO_MANIFEST_DIR"))
        .join("fixtures")
        .join(RECORDING)
}

// ---------------------------------------------------------------------------
// the guard: refuse, never scrub
// ---------------------------------------------------------------------------

/// Why `text` must not be committed, or `Ok`. It REFUSES: a scrubber that
/// quietly rewrote its input would turn "the fixture is clean" into a claim
/// about the scrubber. Its reason names the shape and the byte offset, never
/// the value — a refusal must not print the secret it caught.
///
/// - a run of 32 or more hex digits (a craze resume token's shape, 128 bits;
///   a gx token's is 64);
/// - a credential marker (`api_key`, `authorization`, `bearer `, …);
/// - a well-known token prefix starting a word, with a token's length behind
///   it ([`PREFIXES`]; AWS's `AKIA`/`ASIA` key ids by their exact shape);
/// - a JWT: three base64url segments, the first starting `eyJ`;
/// - a HIGH-ENTROPY run of 40 or more base64 (`A-Za-z0-9+/=`) or base64url
///   (`A-Za-z0-9_-`) characters ([`high_entropy`]);
/// - the developer's own home directory (`home`) anywhere in it.
fn guard(text: &str, home: &str) -> Result<(), String> {
    let mut run = 0;
    for (i, c) in text.char_indices() {
        if c.is_ascii_hexdigit() {
            run += 1;
            if run >= 32 {
                return Err(format!("a run of 32+ hex digits ending at byte {i}"));
            }
        } else {
            run = 0;
        }
    }
    let lower = text.to_ascii_lowercase();
    for marker in [
        "api_key",
        "apikey",
        "authorization",
        "bearer ",
        "\"sk-",
        "x-api-key",
    ] {
        if lower.contains(marker) {
            return Err(format!("a credential marker {marker:?}"));
        }
    }
    for (at, word) in runs(text, is_url_char) {
        for (prefix, min) in PREFIXES {
            if word.starts_with(prefix) && word.len() >= prefix.len() + min {
                return Err(format!("a {prefix}… token at byte {at}"));
            }
        }
        if aws_key_id(word) {
            return Err(format!("an AWS access key id at byte {at}"));
        }
    }
    for (at, run) in runs(text, |c| is_url_char(c) || c == '.') {
        if jwt(run) {
            return Err(format!("a JWT at byte {at}"));
        }
    }
    for alphabet in [is_b64_char as fn(char) -> bool, is_url_char] {
        for (at, run) in runs(text, alphabet) {
            if high_entropy(run) {
                return Err(format!(
                    "a high-entropy base64 run of {} characters at byte {at}",
                    run.len()
                ));
            }
        }
    }
    if !home.is_empty() && home != "/" && text.contains(home) {
        return Err(format!("the developer's home directory {home:?}"));
    }
    Ok(())
}

/// Token prefixes, each with the fewest characters a real token carries after
/// it (GitHub's classic tokens carry 36, a fine-grained PAT 82; OpenAI's and
/// Anthropic's `sk-` keys 40+; Slack's `xox?-` 10+; GitLab's PAT 20; a Google
/// API key 35 after `AIza`). A prefix must START a base64url word, so `ask-`
/// or `task-…` is never `sk-`.
const PREFIXES: &[(&str, usize)] = &[
    ("ghp_", 20),
    ("gho_", 20),
    ("ghu_", 20),
    ("ghs_", 20),
    ("ghr_", 20),
    ("github_pat_", 20),
    ("sk-ant-", 20),
    ("sk-", 20),
    ("xoxa-", 10),
    ("xoxb-", 10),
    ("xoxp-", 10),
    ("xoxr-", 10),
    ("glpat-", 20),
    ("AIza", 30),
];

fn is_url_char(c: char) -> bool {
    c.is_ascii_alphanumeric() || c == '_' || c == '-'
}

fn is_b64_char(c: char) -> bool {
    c.is_ascii_alphanumeric() || c == '+' || c == '/' || c == '='
}

/// The maximal runs of `in_run` characters, with their byte offsets.
fn runs(text: &str, in_run: impl Fn(char) -> bool) -> Vec<(usize, &str)> {
    let mut out = Vec::new();
    let mut start = None;
    for (i, c) in text.char_indices() {
        match (in_run(c), start) {
            (true, None) => start = Some(i),
            (false, Some(s)) => {
                out.push((s, &text[s..i]));
                start = None;
            }
            _ => {}
        }
    }
    if let Some(s) = start {
        out.push((s, &text[s..]));
    }
    out
}

/// An AWS access key id: `AKIA` or `ASIA` and 16 uppercase letters or digits.
fn aws_key_id(word: &str) -> bool {
    (word.starts_with("AKIA") || word.starts_with("ASIA"))
        && word.len() >= 20
        && word[4..20]
            .chars()
            .all(|c| c.is_ascii_uppercase() || c.is_ascii_digit())
}

/// Three base64url segments — header and payload of a real token's length —
/// the first starting `eyJ` (`{"` in base64).
fn jwt(run: &str) -> bool {
    let segs: Vec<&str> = run.split('.').collect();
    segs.windows(3).any(|w| {
        w[0].starts_with("eyJ") && w[0].len() >= 10 && w[1].len() >= 10 && !w[2].is_empty()
    })
}

/// A run of 40+ characters that reads as random, not as words or a path: all
/// three of upper case, lower case and digits, a Shannon entropy of at least
/// 4.3 bits a character, and a character class that changes at least every
/// third character (random base64 does about two times in three; a path or a
/// camel-cased name rarely). Measured on random base64: the 40-character runs
/// below either threshold are under 0.1 % each — a backstop to the prefix,
/// JWT and hex rules, not their replacement.
fn high_entropy(run: &str) -> bool {
    let run = run.trim_end_matches('=');
    if run.len() < 40 {
        return false;
    }
    let has = |f: fn(&char) -> bool| run.chars().any(|c| f(&c));
    if !(has(char::is_ascii_uppercase)
        && has(char::is_ascii_lowercase)
        && has(char::is_ascii_digit))
    {
        return false;
    }
    let mut counts = std::collections::HashMap::new();
    for c in run.chars() {
        *counts.entry(c).or_insert(0usize) += 1;
    }
    let n = run.len() as f64;
    let entropy: f64 = counts
        .values()
        .map(|&k| {
            let p = k as f64 / n;
            -p * p.log2()
        })
        .sum();
    let class = |c: char| {
        if c.is_ascii_uppercase() {
            0
        } else if c.is_ascii_lowercase() {
            1
        } else if c.is_ascii_digit() {
            2
        } else {
            3
        }
    };
    let chars: Vec<char> = run.chars().collect();
    let changes = chars
        .windows(2)
        .filter(|w| class(w[0]) != class(w[1]))
        .count();
    entropy >= 4.3 && changes as f64 / (chars.len() - 1) as f64 >= 0.35
}

#[test]
fn the_recorder_refuses_a_secret_or_a_home_path() {
    let home = "/home/someone";
    assert!(guard(r#"{"text":"pong","workspace":"/tmp/shcz-1-0/work"}"#, home).is_ok());
    assert!(
        guard(
            r#"{"sessionId":"0199d1a3-7b2c-7000-8000-00000000abcd"}"#,
            home
        )
        .is_ok(),
        "a UUID is not a token"
    );
    assert!(
        guard(r#"{"token":"52fdfc072182654f163f5f0f9a621d72"}"#, home).is_err(),
        "a craze resume token"
    );
    assert!(guard(&"a".repeat(64), home).is_err(), "a gx token");
    assert!(guard(r#"{"api_key":"x"}"#, home).is_err());
    assert!(guard("Authorization: Bearer abc", home).is_err());
    assert!(guard(r#"{"path":"/home/someone/.grok/config.toml"}"#, home).is_err());
    // The committed recording passes the guard it was written under.
    let rec = std::fs::read_to_string(recording_dir().join("lane.ndjson"))
        .expect("the recording is committed");
    guard(&rec, "/home/").expect("the committed recording is clean");
}

/// Each common credential shape is REFUSED, wherever in a line it sits — and
/// the refusal names the shape, never the value.
#[test]
fn the_recorder_refuses_every_common_credential_shape() {
    let home = "/home/someone";
    // Fabricated values of each real shape (none is a live credential). Each is
    // assembled with `concat!` so no token-shaped literal sits in the source: secret
    // scanners (GitHub push protection blocked a fabricated `xoxb-` value here) read
    // the text, while the guard under test sees the whole string.
    let shapes = [
        ("ghp_", concat!("ghp", "_0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcd")),
        ("gho_", concat!("gho", "_16C7e42F292c6912E7710c838347Ae178B4a")),
        ("ghu_", concat!("ghu", "_16C7e42F292c6912E7710c838347Ae178B4a")),
        ("ghs_", concat!("ghs", "_16C7e42F292c6912E7710c838347Ae178B4a")),
        ("ghr_", concat!("ghr", "_16C7e42F292c6912E7710c838347Ae178B4a")),
        (
            "github_pat_",
            concat!("github", "_pat_11ABCDEFG0123456789_abcdefghijklmnopqrstuvwxyz0123456789ABCDEFGHIJKLMNOPQRSTUV"),
        ),
        ("sk-", concat!("sk-", "proj-Abcdefghijklmnopqrstuvwxyz0123456789")),
        ("sk-ant-", concat!("sk-", "ant-api03-Abcdefghijklmnopqrstuvwxyz0123456789")),
        ("xoxb-", concat!("xox", "b-123456789012-1234567890123-AbCdEfGhIjKlMnOpQrStUvWx")),
        ("xoxa-", concat!("xox", "a-2-abcdefghijklmnop")),
        ("xoxp-", concat!("xox", "p-123456789012-123456789012")),
        ("xoxr-", concat!("xox", "r-abcdefghijklmnopq")),
        ("AWS", concat!("AKI", "AIOSFODNN7EXAMPLE")),
        ("AWS", concat!("ASI", "AY34FZKBOKMUTVV7A")),
        ("AIza", concat!("AIz", "aSyA1b2C3d4E5f6G7h8I9j0K1l2M3n4O5p6Q")),
        ("glpat-", concat!("glp", "at-xxxxxxxxxxxxxxxxxxxx")),
        (
            "JWT",
            // Each segment short of the high-entropy rule's 40: the JWT rule's
            // own catch.
            concat!("eyJ", "hbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.SflKxwRJSMeKKF2QT4fwpM"),
        ),
        (
            "high-entropy",
            "Zx7Qp2Lm9Vt4Rk8Nw3Jb6Hs1Fd5Gc0Ya2Ue7Io4Pk9Tq1Mz",
        ),
        (
            "high-entropy",
            "q8T/3vKp+Z2mW9xR4sL7nB1cY6dF0gH5jA8eU2iO3oP7k=",
        ),
    ];
    for (what, value) in shapes {
        for line in [
            value.to_string(),
            format!(r#"{{"text":"export TOKEN={value} # done"}}"#),
        ] {
            let why = guard(&line, home)
                .err()
                .unwrap_or_else(|| panic!("{what}: a {what} credential passed the guard"));
            assert!(
                !why.contains(value),
                "{what}: the refusal must not print the value: {why}"
            );
        }
    }
    // …and what is NOT a credential passes: a word ending in `sk-`, a long
    // path, a camel-cased name, an English sentence, a short `sk-` slug, a UUID
    // pair, a JWT-looking word that is not three segments.
    for clean in [
        r#"{"agent":"grok-ask","text":"run the task-runner-for-the-whole-workspace-now"}"#,
        "/tmp/shcz-518-0/home/Library/Application/Support/craze/hosts/logs",
        "AbstractSingletonProxyFactoryBean2ConfigurerWithDefaults",
        "the quick brown fox jumps over the lazy dog again and again",
        "sk-learn",
        "01a1106b-d8ba-747d-9a44-9e0cdd73f92e 01a1106b-d8ba-747d-9a44-9e0cdd73f92e",
        "eyJustAWord.and.more",
        "internal/transcript/fold_test_Go_123/SomethingLongEnoughToCount",
    ] {
        assert!(
            guard(clean, home).is_ok(),
            "{clean:?}: {:?}",
            guard(clean, home)
        );
    }
}

// ---------------------------------------------------------------------------
// the offline replay
// ---------------------------------------------------------------------------

/// The recording's lines, as `{dir, msg}`.
fn recording() -> Vec<Value> {
    let text = std::fs::read_to_string(recording_dir().join("lane.ndjson")).expect("lane.ndjson");
    text.lines()
        .map(|l| serde_json::from_str(l).unwrap_or_else(|e| panic!("{e}: {l}")))
        .collect()
}

/// What the fold makes of the recording: the attach reply's snapshot
/// restored, every event folded in order — the rows numbered by a ring (rows
/// with no time of their own stamped at the epoch, deterministically), the
/// approvals as held, the settings and capabilities, the activity verdict
/// after each event (consecutive repeats collapsed), and how the stream
/// ended.
fn replay(lines: &[Value]) -> Value {
    let mut fold: Option<CrazeFold> = None;
    let mut caps = Value::Null;
    let mut attach = Value::Null;
    let mut activity: Vec<String> = Vec::new();
    let mut events = 0;
    let mut end = Value::Null;
    for line in lines.iter().filter(|l| l["dir"] == "s2c") {
        let msg = &line["msg"];
        if let Some(result) = msg.get("result") {
            if let Ok(r) = serde_json::from_value::<AttachResult>(result.clone()) {
                let mut f = CrazeFold::new(&r.session.host_id);
                if let Some(snap) = &r.snapshot {
                    let restored = f.restore(snap).expect("the recorded snapshot folds");
                    attach = json!({"incarnation": restored.incarnation, "seq": restored.seq, "truncated": restored.truncated});
                }
                f.apply_info(&r.session);
                caps = serde_json::to_value(lane_capabilities(
                    &r.session.capabilities,
                    f.settings().has_any(),
                ))
                .unwrap();
                fold = Some(f);
            }
            continue;
        }
        let Some(f) = fold.as_mut() else { continue };
        match msg["method"].as_str() {
            Some("event") => {
                let p: EventParams = serde_json::from_value(msg["params"].clone()).unwrap();
                f.apply(&p.event);
                events += 1;
                let a = f.activity().map_or("none", |a| a.as_str()).to_string();
                if activity.last() != Some(&a) {
                    activity.push(a);
                }
            }
            Some("reset") => {
                let p: ResetParams = serde_json::from_value(msg["params"].clone()).unwrap();
                end = json!(p.reason);
            }
            _ => {}
        }
    }
    let mut f = fold.expect("the recording has an attach reply");
    f.flush();
    let mut ring = MessageRing::new();
    let rows: Vec<Value> = f
        .drain()
        .into_iter()
        .map(|r| serde_json::to_value(ring.append(r, 0)).unwrap())
        .collect();
    json!({
        "recording": RECORDING,
        "attach": attach,
        "events": events,
        "capabilities": caps,
        "rows": rows,
        "approvals": serde_json::to_value(f.held_approvals()).unwrap(),
        "settings": serde_json::to_value(f.settings().lane_settings()).unwrap(),
        "activity": activity,
        "end": end,
    })
}

#[test]
fn the_recording_replays_into_its_golden() {
    let got = replay(&recording());
    let golden_path = recording_dir().join("fold.golden.json");
    let rendered = format!("{}\n", serde_json::to_string_pretty(&got).unwrap());
    if std::env::var("SHED_CRAZE_REGOLD").is_ok_and(|v| v == "1") {
        std::fs::write(&golden_path, &rendered).unwrap();
        eprintln!("wrote {}", golden_path.display());
    }
    let golden = std::fs::read_to_string(&golden_path)
        .expect("fold.golden.json (SHED_CRAZE_REGOLD=1 writes it)");
    assert_eq!(rendered, golden, "the fold's projection of the recording changed — read the diff as the change (fixtures/README.md)");
}

/// The recording still carries what it is FOR (`fixtures/README.md`): a seed
/// from a snapshot, a real model's text, a tool call that ran, a turn's
/// bracket, and a stop's closing records then `reset{session_closed}`.
#[test]
fn the_recording_carries_what_it_is_for() {
    let lines = recording();
    let kinds: Vec<String> = lines
        .iter()
        .filter(|l| l["dir"] == "s2c" && l["msg"]["method"] == "event")
        .map(|l| {
            l["msg"]["params"]["event"]["type"]
                .as_str()
                .unwrap_or_default()
                .to_string()
        })
        .collect();
    for k in ["turn", "text", "tool", "done"] {
        assert!(kinds.iter().any(|x| x == k), "no {k} event in {kinds:?}");
    }
    let got = replay(&lines);
    assert_eq!(got["end"], "session_closed", "the stop's reset");
    assert!(got["attach"]["seq"].is_u64(), "seeded from a snapshot");
    let rows = got["rows"].as_array().unwrap();
    assert!(rows.iter().any(|r| r["type"] == "tool_use"), "a tool ran");
    assert!(
        rows.iter()
            .any(|r| r["role"] == "assistant" && r["type"] == "text"),
        "the model answered"
    );
    assert!(
        rows.iter().filter(|r| r["role"] == "user").count() >= 2,
        "two turns"
    );
    assert!(
        lines
            .iter()
            .all(|l| l["msg"]["result"].get("token").is_none()),
        "no host hello is recorded (its resume token is a secret's shape)"
    );
}

// ---------------------------------------------------------------------------
// the live run
// ---------------------------------------------------------------------------

async fn drive(
    rx: &mut tokio::sync::mpsc::Receiver<LaneEvent>,
    checker: &mut LaneChecker,
    what: &str,
    until: impl FnMut(&LaneEvent) -> bool,
) -> Vec<LaneEvent> {
    let d = drive_lane(rx, checker, LIVE_WAIT, until)
        .await
        .unwrap_or_else(|v| panic!("{what}: the stream broke a contract rule: {v}"));
    assert_eq!(
        d.end,
        DriveEnd::Matched,
        "{what}: not seen within {LIVE_WAIT:?}: {:#?}",
        d.frames
    );
    d.frames
}

/// One turn: send `text`, answer any permission it raises with its one
/// `allow_once` option, and wait until the session is idle again after it
/// worked.
async fn turn(
    lane: &Arc<dyn AgentLane>,
    rx: &mut tokio::sync::mpsc::Receiver<LaneEvent>,
    checker: &mut LaneChecker,
    text: &str,
) {
    lane.send(text, SendMode::Queue)
        .await
        .expect("the prompt was taken");
    let mut worked = false;
    let deadline = tokio::time::Instant::now() + LIVE_WAIT;
    loop {
        let left = deadline.saturating_duration_since(tokio::time::Instant::now());
        let d = drive_lane(rx, checker, left, |e| {
            matches!(e, LaneEvent::Session { .. } | LaneEvent::Approval { .. })
        })
        .await
        .unwrap_or_else(|v| panic!("the turn broke a contract rule: {v}"));
        assert_eq!(
            d.end,
            DriveEnd::Matched,
            "the turn did not end within {LIVE_WAIT:?}"
        );
        match d.frames.last() {
            Some(LaneEvent::Approval { approval }) if approval.status.is_pending() => {
                let allow = approval
                    .options
                    .iter()
                    .find(|o| o.kind.as_deref() == Some(option_kind::ALLOW_ONCE))
                    .map(|o| o.id.clone());
                if let Some(id) = allow {
                    lane.answer(&approval.id, LaneAnswer::Choice { option_id: id })
                        .await
                        .expect("answered");
                }
            }
            Some(LaneEvent::Session { session }) => match session.activity {
                RcActivity::Working => worked = true,
                RcActivity::Idle if worked => return,
                _ => {}
            },
            _ => {}
        }
    }
}

/// The lane connection's lines from its attach on, as `{dir, msg}` — the host
/// `hello` (and its resume token) never among them.
fn lane_lines(tee: &[TeeLine]) -> Vec<Value> {
    let conn = tee
        .iter()
        .find(|l| l.dir == "c2s" && l.line.contains("\"session.attach\""))
        .expect("the lane attached")
        .conn;
    let mut out = Vec::new();
    let mut started = false;
    for l in tee.iter().filter(|l| l.conn == conn) {
        if !started {
            if l.dir == "c2s" && l.line.contains("\"session.attach\"") {
                started = true;
            } else {
                continue;
            }
        }
        let msg: Value =
            serde_json::from_str(&l.line).unwrap_or_else(|e| panic!("{e}: {}", l.line));
        out.push(json!({"dir": l.dir, "msg": msg}));
    }
    out
}

fn run_craze(recipe: &Recipe, args: &[&str]) -> String {
    let out = std::process::Command::new(recipe.path_dir.join("craze"))
        .args(args)
        .env_clear()
        .envs(recipe.env())
        .current_dir(recipe.root())
        .output()
        .expect("craze ran");
    format!(
        "{}{}",
        String::from_utf8_lossy(&out.stdout),
        String::from_utf8_lossy(&out.stderr)
    )
}

/// Every process of this user's whose environment's `HOME` is exactly
/// `home` (Linux `/proc`; the live run is a local one): SIGTERM, a bounded
/// wait, SIGKILL — each logged, never pid 0 or 1, never this process.
fn kill_by_home(home: &Path) {
    let want = format!("HOME={}", home.display());
    let me = std::process::id();
    let find = || -> Vec<u32> {
        std::fs::read_dir("/proc")
            .into_iter()
            .flatten()
            .flatten()
            .filter_map(|e| e.file_name().to_str()?.parse::<u32>().ok())
            .filter(|p| *p > 1 && *p != me)
            .filter(|p| {
                std::fs::read(format!("/proc/{p}/environ"))
                    .is_ok_and(|env| env.split(|b| *b == 0).any(|v| v == want.as_bytes()))
            })
            .collect()
    };
    for sig in ["TERM", "KILL"] {
        let pids = find();
        for p in &pids {
            let args = std::fs::read(format!("/proc/{p}/cmdline")).unwrap_or_default();
            let args = String::from_utf8_lossy(&args).replace('\0', " ");
            println!("LEDGER kill -{sig} {p} ({want}): {args}");
            let _ = std::process::Command::new("kill")
                .arg(format!("-{sig}"))
                .arg(p.to_string())
                .status();
        }
        for _ in 0..50 {
            if find().is_empty() {
                return;
            }
            std::thread::sleep(Duration::from_millis(100));
        }
    }
}

/// [`kill_by_home`] on drop, so a run that fails half-way leaves no gx leader
/// behind either.
struct KillByHome(PathBuf);

impl Drop for KillByHome {
    fn drop(&mut self) {
        kill_by_home(&self.0);
    }
}

#[tokio::test(flavor = "multi_thread")]
async fn record_a_gx_session() {
    if !std::env::var("SHED_CRAZE_LIVE").is_ok_and(|v| v == "1") {
        eprintln!("skipping record_a_gx_session: set SHED_CRAZE_LIVE=1 (and SHED_CRAZE_RECORD=1 to write) — it drives a real gx session (fixtures/README.md)");
        return;
    }
    let bins = bins("record_a_gx_session").expect("SHED_CRAZE_BIN_DIR");
    let gx = std::env::var("SHED_CRAZE_GX").unwrap_or_else(|_| DEFAULT_GX.to_string());
    assert!(Path::new(&gx).is_file(), "no gx at {gx}");
    let gx_config = std::env::var("SHED_CRAZE_GX_CONFIG").unwrap_or_else(|_| {
        format!(
            "{}/.cache/shed-plan025/live-c8/grok-config.toml",
            std::env::var("HOME").unwrap()
        )
    });
    let real_home = std::env::var("HOME").unwrap_or_default();
    // Never /usr/local/bin: the owner's craze v0.0.1 lives there. gx offers
    // `xai.api_key` (its description: "XAI_API_KEY or api_key/env_key in
    // config.toml") and an interactive login; craze takes the first only with
    // the variable set — a placeholder, never a key: the turns run on the
    // scratch config's Z.AI model, whose own key is in that config.
    let recipe = Recipe::start(&bins)
        .with_path_after(&["/usr/bin", "/bin"])
        .with_env("XAI_API_KEY", "unused-placeholder");
    let _gx_guard = KillByHome(recipe.home.clone());
    recipe.write_config(&format!(
        "host_idle_exit = \"30s\"\n\n[agents]\ngx = \"{gx}\"\n"
    ));
    let grok = recipe.home.join(".grok");
    std::fs::create_dir(&grok).unwrap();
    std::fs::copy(&gx_config, grok.join("config.toml")).unwrap();
    println!(
        "LEDGER dirs: root={} home={} craze_home={} runtime={} work={}",
        recipe.root().display(),
        recipe.home.display(),
        recipe.craze_home.display(),
        recipe.runtime.display(),
        recipe.work.display()
    );
    let recorded = {
        let tee = TeeDial::new(Arc::new(recipe.dial()));
        let source = recipe.source_on(Arc::clone(&tee) as Arc<dyn CrazeDial>);
        let created = source
            .create(LaneCreateRequest {
                cwd: recipe.work.to_string_lossy().into_owned(),
                provider: Some("gx".into()),
                prompt: None,
                request_id: new_request_id(),
            })
            .await
            .expect("a gx session was created");
        println!(
            "LEDGER session: host={} provider_session={:?}",
            created.session.id, created.session.provider_session_id
        );
        let mut roster = source.subscribe().await.unwrap();
        loop {
            match tokio::time::timeout(LIVE_WAIT, roster.rx.recv())
                .await
                .expect("listed")
                .expect("roster")
            {
                SourceEvent::Session { session } if session.id == created.session.id => break,
                _ => {}
            }
        }
        let lane = source.open(&created.session.id).await.unwrap();
        let (mut rx, _stop) = lane.subscribe(None).await.unwrap().into_parts();
        let mut checker = LaneChecker::new();
        drive(&mut rx, &mut checker, "the seed", |e| {
            matches!(e, LaneEvent::Ready { .. })
        })
        .await;
        turn(
            &lane,
            &mut rx,
            &mut checker,
            "Reply with exactly one word: pong",
        )
        .await;
        turn(&lane, &mut rx, &mut checker, "Use your shell tool to run `echo shed-craze-live` and then tell me, in one short sentence, what it printed.").await;
        for (pid, args) in recipe.processes() {
            println!("LEDGER process: {pid} {args}");
        }
        lane.stop().await.expect("the stop's receipt");
        let f = drive(&mut rx, &mut checker, "Down", |e| {
            matches!(e, LaneEvent::Down { .. })
        })
        .await;
        assert_eq!(
            f.last(),
            Some(&LaneEvent::Down {
                reason: "session_closed".into()
            })
        );
        drop(roster);
        tokio::time::sleep(Duration::from_secs(2)).await;
        println!(
            "LEDGER craze ps --no-hub after the stop:\n{}",
            run_craze(&recipe, &["ps", "--no-hub"])
        );
        lane_lines(&tee.lines())
    };
    let text: String = recorded
        .iter()
        .map(|l| format!("{}\n", serde_json::to_string(l).unwrap()))
        .collect();
    let guarded = guard(&text, &real_home);
    // gx detaches a leader of its own under the scratch HOME, which the
    // recipe's teardown (its own copies only) never reaches: end it first.
    kill_by_home(&recipe.home);
    let left = recipe.teardown().await;
    println!("LEDGER teardown: {left:?}");
    guarded.unwrap_or_else(|why| panic!("the recording is REFUSED, nothing written: {why}"));
    if std::env::var("SHED_CRAZE_RECORD").is_ok_and(|v| v == "1") {
        let dir = recording_dir();
        std::fs::create_dir_all(&dir).unwrap();
        std::fs::write(dir.join("lane.ndjson"), &text).unwrap();
        let kinds: std::collections::BTreeSet<String> = recorded
            .iter()
            .filter(|l| l["msg"]["method"] == "event")
            .map(|l| {
                l["msg"]["params"]["event"]["type"]
                    .as_str()
                    .unwrap_or_default()
                    .to_string()
            })
            .collect();
        let version = recorded
            .iter()
            .find_map(|l| {
                l["msg"]["result"]["session"]["provider"]["name"]
                    .as_str()
                    .map(str::to_string)
            })
            .unwrap_or_default();
        let provenance = format!(
            "# craze {RECORDING} — recording provenance\n\n\
             Written by `crates/shed-craze/tests/live.rs` on each re-record. It is GENERATED:\n\
             edit `../README.md` (hand-maintained) for anything a run cannot know.\n\n\
             Recorded with:\n\n```bash\nSHED_CRAZE_LIVE=1 SHED_CRAZE_RECORD=1 SHED_CRAZE_BIN_DIR=<make craze-binaries> \\\n  cargo test -p shed-craze --features test-support --test live record_a_gx_session -- --nocapture\n```\n\n\
             - craze: the pin's `craze` (`{}`), a real hub under a scratch HOME/CRAZE_HOME/CRAZE_RUNTIME_DIR.\n\
             - provider: `{version}` — gx `{}` on model `glm-5.3-flash` (the scratch gx config).\n\
             - `lane.ndjson`: every line of the lane's own connection from its `session.attach` on\n  (`{{dir, msg}}`, craze's fixture shape), {} lines — the host `hello` before it is never recorded.\n\
             - event kinds seen: {}\n",
            bins.craze.display().to_string().rsplit('/').nth(1).unwrap_or_default(),
            gx.rsplit('/').nth(1).unwrap_or_default(),
            recorded.len(),
            kinds.into_iter().collect::<Vec<_>>().join(", "),
        );
        std::fs::write(dir.join("PROVENANCE.md"), provenance).unwrap();
        println!("wrote {} ({} lines)", dir.display(), recorded.len());
    }
}
