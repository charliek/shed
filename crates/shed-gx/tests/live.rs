//! The live smoke — the one test that talks to a REAL gx leader.
//!
//! **Gated by `SHED_GX_LIVE=1`, and never run in CI.** Without the variable it
//! prints why it is skipping and passes, so an ordinary
//! `cargo test -p shed-gx` is unaffected. It spends real model quota (one cheap
//! turn) and depends on a leader being up on this host, which is exactly why it
//! is opt-in.
//!
//! What it proves that the `FakeGx` suite cannot: that the routes, the query
//! parameters, the event vocabulary, the timestamp units and the approval
//! shapes this adapter assumes are the ones gx actually serves.
//!
//! ```bash
//! SHED_GX_LIVE=1 cargo test -p shed-gx --features test-support --test live -- --nocapture
//! # and, to refresh the committed wire fixtures:
//! SHED_GX_LIVE=1 SHED_GX_RECORD=1 \
//!   cargo test -p shed-gx --features test-support --test live -- --nocapture
//! ```
//!
//! # It never touches a leader it was not pointed at
//!
//! Everything comes from `$GROK_HOME` — the discovery record says the URL, and
//! the record's own `tokenFile` says where the credential is. A developer with a
//! personal leader on another port is unaffected, because this reads a home, not
//! a port.
//!
//! # The recorder REFUSES rather than scrubs
//!
//! A fixture is committed, so it has to be clean by construction and not by
//! remembering to look. [`Guard`] fails the run if the captured bytes carry a
//! 64-hex run (the shape of a gx token) or an absolute path outside the scratch
//! directories. Refusing is the whole design: a scrubber that silently rewrote
//! its input would make "the fixture is clean" a claim about the scrubber
//! rather than about the recording, and the first thing it failed to recognize
//! would be committed looking fine.

use std::path::{Path, PathBuf};
use std::time::Duration;

use serde_json::value::RawValue;
use serde_json::Value;
use shed_core::lane::{AgentLane, LaneEvent, SendMode};
use shed_core::rc::RcActivity;
use shed_gx::discovery::{record_files, GxRecord, StaticCredentials, DEFAULT_GROK_HOME};
use shed_gx::transport::FixedDial;
use shed_gx::{GxClient, GxTimings};
use tokio::sync::mpsc::Receiver;

/// The gx this smoke was written against. It names the fixtures directory, so a
/// later run on a newer gx records BESIDE it rather than over it.
const RECORDED_VERSION: &str = "1.0.16+gx.12";

/// The prompt the recorded turn is driven with. Deliberately trivial: the
/// fixture is about the WIRE, and a long answer costs quota without adding a
/// single frame kind.
const PROMPT: &str = "reply with the single word pong";

/// Every wait against a real agent. Model latency, not loopback latency.
const TURN_DEADLINE: Duration = Duration::from_secs(120);

const PROVENANCE: &str = "PROVENANCE.md";

#[tokio::test(flavor = "multi_thread")]
async fn live_smoke() {
    if std::env::var("SHED_GX_LIVE").as_deref() != Ok("1") {
        eprintln!(
            "SKIP live_smoke: set SHED_GX_LIVE=1 to run it. It talks to a real gx \
             leader found through $GROK_HOME, spends real model quota on one turn, \
             and is never run in CI."
        );
        return;
    }

    // ---- the leader, from its own record --------------------------------
    let home = grok_home();
    let record = pick_record(&home).unwrap_or_else(|| {
        panic!(
            "no gx discovery record under {}: start a leader there first",
            home.display()
        )
    });
    let reported = record.url.clone();
    eprintln!(
        "live_smoke: gx {} at {reported} (instance {})",
        record.version, record.instance_id
    );
    let discovery = shed_gx::local_discovery(&home, &reported, caller_uid())
        .expect("reading the leader's token out of its own record");

    let lane = GxClient::new(
        reported.clone(),
        std::sync::Arc::new(FixedDial::parse(&reported).expect("the reported URL parses")),
        std::sync::Arc::new(StaticCredentials::new(discovery)),
        GxTimings::default(),
    )
    .expect("the gx client builds");

    // ---- the roster ------------------------------------------------------
    let sessions = lane.sessions().await.expect("GET /v1/sessions");
    assert!(!sessions.is_empty(), "the leader has no sessions to drive");
    let session = match std::env::var("SHED_GX_SESSION") {
        Ok(id) if !id.is_empty() => id,
        _ => sessions
            .iter()
            .max_by_key(|s| s.last_change_unix_ms.unwrap_or(0))
            .map(|s| s.id.clone())
            .expect("a most-recently-changed session"),
    };
    eprintln!("live_smoke: driving session {session}");
    let row = lane.session(&session).await.expect("GET the session row");
    assert_eq!(row.id, session);
    assert!(
        !row.cwd.is_empty(),
        "gx reports a cwd for every session it knows"
    );

    // ---- history, both ways ---------------------------------------------
    let page = lane
        .history(&session, None, 200)
        .await
        .expect("GET the history tail");
    assert!(
        !page.messages.is_empty(),
        "the chosen session has no transcript; pick another with SHED_GX_SESSION"
    );
    let cursor = page.cursor.clone().expect("gx stamps the page's newest id");
    eprintln!(
        "live_smoke: {} rows, cursor {cursor}, truncated {}",
        page.messages.len(),
        page.truncated
    );

    // The same cursor, back: everything after it, which for the newest id is
    // nothing at all. That is the assertion — a cursor that came back with
    // MORE than it was asked for would mean the positional cut is wrong.
    let after = lane
        .history(&session, Some(&cursor), 200)
        .await
        .expect("GET the history from a cursor");
    assert!(
        after.messages.len() < page.messages.len(),
        "resuming from the newest id must not replay the whole transcript: \
         {} rows from the cursor vs {} in the tail",
        after.messages.len(),
        page.messages.len()
    );

    let approvals = lane.approvals(&session).await.expect("GET the approvals");
    eprintln!("live_smoke: {} open approvals", approvals.len());

    // ---- the raw recorder, alongside the lane ---------------------------
    let mut recorder = Recorder::start();
    recorder.history(&lane, &session).await;
    // Approvals are captured LATER — see `raise_and_answer_an_approval`. gx
    // forgets a resolved approval after an hour, so whatever is open right now
    // is usually nothing at all.

    // ---- subscribe, drive one turn --------------------------------------
    let (mut rx, _stop) = lane
        .subscribe(&session, None)
        .await
        .expect("subscribe never fails")
        .into_parts();
    let seed = until_ready(&mut rx).await;
    assert!(
        matches!(seed.first(), Some(LaneEvent::Reset { .. })),
        "the first frame is always the Reset that opens generation 1"
    );
    assert!(
        seed.iter().any(|e| matches!(e, LaneEvent::Ready { .. })),
        "the seed completed"
    );
    assert!(
        seed.iter().any(|e| matches!(e, LaneEvent::Message { .. })),
        "a real session's seed carries its transcript"
    );

    // A session with an approval already open is BLOCKED — a prompt queued
    // behind one would never run. So it is settled first, and it doubles as the
    // fixture (a real approval beats a manufactured one, and costs no quota).
    //
    // **Not gated on the recorder.** Unblocking is an OPERATIONAL step and the
    // fixture role is incidental: gating it on `SHED_GX_RECORD` meant a plain
    // `SHED_GX_LIVE=1` run against a blocked session queued behind the approval
    // and burned the whole `TURN_DEADLINE` before failing — spending scarce paid
    // quota to prove nothing. `settle_an_approval` already keeps the CAPTURE
    // behind `recorder.on()` internally, which is where that gate belongs.
    let answered_an_open_one = !approvals.is_empty()
        && settle_an_approval(&lane, &session, &mut rx, &mut recorder, false).await;

    lane.send(&session, PROMPT, SendMode::Queue)
        .await
        .expect("POST a queued prompt");
    eprintln!("live_smoke: queued {PROMPT:?}");

    // The turn, live. `turn_completed` is gx's own extension and is what moves
    // the fold to `Idle`; waiting for the row alone would race the model.
    let mut idle = false;
    let mut rows = 0usize;
    let deadline = tokio::time::Instant::now() + TURN_DEADLINE;
    while tokio::time::Instant::now() < deadline {
        let Ok(Some(ev)) = tokio::time::timeout(Duration::from_secs(10), rx.recv()).await else {
            continue;
        };
        recorder.frame(&ev);
        match &ev {
            LaneEvent::Message { message, cursor } => {
                rows += 1;
                assert!(
                    cursor.is_some(),
                    "gx advertises history_cursor, so every row carries one"
                );
                eprintln!(
                    "live_smoke:   #{} {:<9} {:<14} {}",
                    message.seq,
                    message.role,
                    message.msg_type,
                    message
                        .text
                        .as_deref()
                        .unwrap_or_default()
                        .replace('\n', " ")
                );
            }
            LaneEvent::Session { session } if session.activity == RcActivity::Idle && rows > 0 => {
                idle = true;
                break;
            }
            LaneEvent::Down { reason } => panic!("the subscription ended: {reason}"),
            _ => {}
        }
    }
    assert!(rows > 0, "the turn produced no transcript rows");
    assert!(
        idle,
        "the turn never came back to idle within {TURN_DEADLINE:?}"
    );

    // ---- an approval, if the fixture needs one ---------------------------
    //
    // gx serves `resolved` approvals for an HOUR and then forgets them, so a
    // re-record cannot rely on one being lying around: the fixture's approval
    // shapes — the opaque option ids, the two options that BOTH declare
    // `allow_once` — have to be raised on purpose.
    // Unlike the settle above, RAISING one is purely for the fixture — it costs
    // a turn and unblocks nothing — so this half stays behind the recorder.
    if recorder.on() && !answered_an_open_one {
        settle_an_approval(&lane, &session, &mut rx, &mut recorder, true).await;
    }

    // ---- record the frames, guarded --------------------------------------
    recorder.stream(&lane, &session).await;
    recorder.finish(&record);
}

/// Record one approval VERBATIM and answer it, raising one first if asked.
///
/// Returns whether an approval was actually answered.
///
/// **gx announces an approval TWICE, and the first one is empty.** The leader
/// files a `pending_interaction` placeholder — `method` and `request` both
/// `null`, so the adapter maps it to `Other("placeholder")` with no options —
/// the instant the agent blocks, and sends the real request in a second frame
/// on the SAME id once it has it. That is not a bug to work around: it is why
/// `LaneEvent::Approval` is id-keyed and LAST-WRITE-WINS, and a client that
/// rendered the first frame's (empty) option list forever would be showing a
/// question with no buttons.
///
/// Two live assertions ride along, and neither is reachable from `FakeGx`
/// because both are about what a real gx offers:
///
/// - a real permission offers **two** options declaring `allow_once` (one of
///   them turns prompting off for the whole session), so
///   `LaneAnswer::Permission{AllowOnce}` is genuinely ambiguous and the adapter
///   refuses it rather than guessing which one the human meant;
/// - `LaneAnswer::Choice` with the id gx actually offered is accepted.
async fn settle_an_approval(
    lane: &GxClient,
    session: &str,
    rx: &mut Receiver<LaneEvent>,
    recorder: &mut Recorder,
    raise_it: bool,
) -> bool {
    if raise_it {
        // Overridable, and it has to be: gx's classifier decides what needs
        // asking, and what it waves through moves with the model and the
        // session's own history (a command it has already approved once is not
        // asked about again). A run that captures no approval says so and
        // leaves the committed fixture alone, rather than pretending.
        let ask = std::env::var("SHED_GX_ASK").unwrap_or_else(|_| {
            "run the shell command: curl -sS https://example.com/ -o /dev/null -w '%{http_code}'"
                .to_string()
        });
        lane.send(session, &ask, SendMode::Queue)
            .await
            .expect("POST the permission-raising prompt");
        eprintln!("live_smoke: queued {ask:?} to raise a permission");
    }

    // Wait for an approval that is actually ANSWERABLE — pending, and carrying
    // the options the agent offered. A placeholder is noted and waited past.
    let deadline = tokio::time::Instant::now() + TURN_DEADLINE;
    let mut answerable = None;
    let mut saw_placeholder = false;
    while tokio::time::Instant::now() < deadline {
        for open in lane.approvals(session).await.unwrap_or_default() {
            if !open.status.is_pending() {
                continue;
            }
            if open.options.is_empty() {
                if !saw_placeholder {
                    eprintln!(
                        "live_smoke: approval {} is gx's placeholder ({}) — waiting for the \
                         request itself",
                        open.id,
                        open.kind.as_str()
                    );
                    saw_placeholder = true;
                }
                continue;
            }
            answerable = Some(open);
            break;
        }
        if answerable.is_some() {
            break;
        }
        // Drain frames while waiting, so the inventory sees them and the
        // channel does not grow.
        if let Ok(Some(ev)) = tokio::time::timeout(Duration::from_millis(500), rx.recv()).await {
            recorder.frame(&ev);
        }
    }
    let Some(approval) = answerable else {
        eprintln!(
            "live_smoke: no answerable approval within {TURN_DEADLINE:?} — the scratch gx \
             is probably not in `ask` permission mode. approvals.jsonl is NOT refreshed; \
             the provenance card says so."
        );
        return false;
    };
    eprintln!(
        "live_smoke: approval {} [{}] with options {:?}",
        approval.id,
        approval.kind.as_str(),
        approval
            .options
            .iter()
            .map(|o| (o.id.as_str(), o.kind.as_deref()))
            .collect::<Vec<_>>()
    );

    // Verbatim, before it is answered and while gx still serves it.
    recorder.approvals(lane, session).await;

    let allow_once: Vec<&str> = approval
        .options
        .iter()
        .filter(|o| o.kind.as_deref() == Some("allow_once"))
        .map(|o| o.id.as_str())
        .collect();
    if allow_once.len() > 1 {
        let err = lane
            .answer(
                session,
                &approval.id,
                shed_core::lane::LaneAnswer::Permission {
                    decision: shed_core::lane::LaneDecision::AllowOnce,
                },
            )
            .await
            .expect_err("two allow_once options make a three-valued decision ambiguous");
        eprintln!("live_smoke: AllowOnce was refused, correctly: {err}");
    }

    // The id gx itself offered — never a guess, never a nearest match. The LAST
    // `allow_once` is the plain "yes, proceed"; the first is the one that turns
    // prompting off for the session, which a fixture run must not press.
    let chosen = allow_once.last().copied().unwrap_or_else(|| {
        approval
            .options
            .first()
            .map(|o| o.id.as_str())
            .expect("an answerable approval has options")
    });
    lane.answer(
        session,
        &approval.id,
        shed_core::lane::LaneAnswer::Choice {
            option_id: chosen.to_string(),
        },
    )
    .await
    .unwrap_or_else(|e| panic!("answering with the offered option {chosen}: {e}"));
    eprintln!("live_smoke: answered with {chosen:?}");
    true
}

/// The recorder's guard and its provenance writer, OFFLINE — no gx, no network,
/// no `SHED_GX_LIVE`.
///
/// It is the half of the recording path that must never be exercised for the
/// first time on the day someone re-records: a guard that has never rejected
/// anything is a guard nobody has checked.
#[test]
fn the_recorder_refuses_a_token_or_a_path_outside_the_scratch_directories() {
    let guard = Guard::new(&[PathBuf::from("/scratch/project")]);

    let token = "a".repeat(64);
    let refusal = guard
        .check("history.json", &format!(r#"{{"note":"{token}"}}"#))
        .expect_err("a 64-hex run is a gx token's shape and is refused");
    assert!(refusal.contains("64"), "{refusal}");

    let refusal = guard
        .check("history.json", r#"{"cwd":"/home/someone/real-work"}"#)
        .expect_err("a path outside the scratch directories is refused");
    assert!(refusal.contains("/home/someone/real-work"), "{refusal}");

    // …and what is genuinely from the scratch tree passes, including ids that
    // are hex but nothing like 64 characters of it.
    guard
        .check(
            "history.json",
            r#"{"cwd":"/scratch/project","id":"call_e57d6b5d334047fb90b93eb7",
                "sessionId":"01a08518-bd64-7720-a72c-6161c6587164"}"#,
        )
        .expect("a clean recording is written");
}

/// **A re-record must not clobber the curated fixtures README.** The writer
/// emits `PROVENANCE.md`; `README.md` is hand-maintained and carries everything
/// a run cannot know.
#[test]
fn a_re_record_writes_a_provenance_card_and_leaves_the_readme_alone() {
    let dir = std::env::temp_dir().join(format!("shed-gx-provenance-{}", std::process::id()));
    let _ = std::fs::remove_dir_all(&dir);
    std::fs::create_dir_all(&dir).expect("the scratch directory");
    std::fs::write(dir.join("README.md"), "hand written\n").expect("seeding a README");

    write_provenance(
        &dir,
        &GxRecord {
            url: "http://127.0.0.1:2431".to_string(),
            pid: 1,
            instance_id: "an-instance".to_string(),
            socket_path: "/tmp/leader.sock".to_string(),
            token_file: "/scratch/gx-remote.token".to_string(),
            version: "9.9.9".to_string(),
            started_at: 0,
        },
        3,
        &["update: 3".to_string()],
    );

    assert_eq!(
        std::fs::read_to_string(dir.join("README.md")).expect("the README survives"),
        "hand written\n",
        "a re-record must never rewrite the page that documents re-recording"
    );
    let card = std::fs::read_to_string(dir.join(PROVENANCE)).expect("the card is written");
    for want in ["9.9.9", "SHED_GX_RECORD=1", "README.md", "update: 3"] {
        assert!(
            card.contains(want),
            "the card says nothing about {want:?}:\n{card}"
        );
    }
    // The card must never name the token FILE's contents, and it does not name
    // the file either — a provenance note is not a treasure map.
    assert!(!card.contains("gx-remote.token"), "{card}");
    let _ = std::fs::remove_dir_all(&dir);
}

// ---------------------------------------------------------------------------
// discovery
// ---------------------------------------------------------------------------

fn grok_home() -> PathBuf {
    match std::env::var_os("GROK_HOME") {
        Some(h) if !h.is_empty() => PathBuf::from(h),
        _ => PathBuf::from(std::env::var_os("HOME").expect("HOME")).join(DEFAULT_GROK_HOME),
    }
}

/// The record to drive, honoring `SHED_GX_URL` when the home holds several.
fn pick_record(home: &Path) -> Option<GxRecord> {
    let records: Vec<GxRecord> = record_files(home)
        .iter()
        .filter_map(|p| std::fs::read_to_string(p).ok())
        .filter_map(|s| serde_json::from_str::<GxRecord>(&s).ok())
        .filter(|r| !r.url.is_empty())
        .collect();
    match std::env::var("SHED_GX_URL") {
        Ok(url) if !url.is_empty() => records.into_iter().find(|r| r.url == url),
        // The newest, which on a home with a stale record is the live one.
        _ => records.into_iter().max_by_key(|r| r.started_at),
    }
}

/// The caller's effective uid, for the token file's owner check.
///
/// `std::os::unix::fs::MetadataExt` reports a FILE's owner and nothing in `std`
/// reports the PROCESS's, so the uid is read back off a file this process just
/// created: whoever owns it IS the effective uid. Taking a `libc` dependency
/// for one number — in a crate whose dependency set is an invariant — would be
/// a worse trade than four lines here.
fn caller_uid() -> u32 {
    use std::os::unix::fs::MetadataExt as _;
    let probe = std::env::temp_dir().join(format!("shed-gx-uid-{}", std::process::id()));
    std::fs::write(&probe, b"").expect("writing a uid probe file");
    let uid = std::fs::metadata(&probe).expect("stat-ing the probe").uid();
    let _ = std::fs::remove_file(&probe);
    uid
}

// ---------------------------------------------------------------------------
// waiting
// ---------------------------------------------------------------------------

async fn until_ready(rx: &mut Receiver<LaneEvent>) -> Vec<LaneEvent> {
    let mut out = Vec::new();
    loop {
        let ev = match tokio::time::timeout(TURN_DEADLINE, rx.recv()).await {
            Err(_) => panic!("timed out waiting for the seed's Ready"),
            Ok(None) => panic!("the lane stream ended during the seed"),
            Ok(Some(ev)) => ev,
        };
        let done = matches!(ev, LaneEvent::Ready { .. } | LaneEvent::Down { .. });
        out.push(ev);
        if done {
            return out;
        }
    }
}

// ---------------------------------------------------------------------------
// the recorder
// ---------------------------------------------------------------------------

/// Captures the wire into memory, checks it, and only then writes.
struct Recorder {
    on: bool,
    history: String,
    approvals: Vec<String>,
    frames: Vec<String>,
    /// What the run saw, as labels — the inventory the fixtures README and the
    /// replay test depend on. Kinds, never byte counts: a reader opening the
    /// card wants to know whether a `tool_call` is in there, not how long it
    /// was.
    inventory: Vec<String>,
}

impl Recorder {
    fn start() -> Recorder {
        Recorder {
            on: std::env::var("SHED_GX_RECORD").as_deref() == Ok("1"),
            history: String::new(),
            approvals: Vec::new(),
            frames: Vec::new(),
            inventory: Vec::new(),
        }
    }

    /// `GET …/history`, VERBATIM. Re-serializing would sort the object keys and
    /// the fixture would stop being a recording of what gx wrote.
    async fn history(&mut self, lane: &GxClient, session: &str) {
        if !self.on {
            return;
        }
        self.history = lane
            .raw_history(session, -200, 200)
            .await
            .expect("recording the history page");
    }

    async fn approvals(&mut self, lane: &GxClient, session: &str) {
        if !self.on {
            return;
        }
        let body = lane
            .raw_approvals(session)
            .await
            .expect("recording the approvals");
        #[derive(serde::Deserialize)]
        struct Page<'a> {
            #[serde(borrow, default)]
            approvals: Vec<&'a RawValue>,
        }
        let page: Page = serde_json::from_str(&body).expect("the approvals page decodes");
        self.approvals = page.approvals.iter().map(|r| r.get().to_string()).collect();
    }

    /// The SSE frames, re-read from the ring by resuming from the START of the
    /// page the recording covers.
    ///
    /// It is a REPLAY rather than a tee of the live stream, and that is the
    /// point: it captures exactly the envelopes gx would hand a resuming
    /// client, `id:` lines and all, without a second live connection racing the
    /// first for the same turn.
    async fn stream(&mut self, lane: &GxClient, session: &str) {
        if !self.on {
            return;
        }
        let page: Value = serde_json::from_str(&self.history).expect("the recorded page decodes");
        let from = page
            .get("updates")
            .and_then(Value::as_array)
            .and_then(|u| u.first())
            .and_then(|u| u.get("eventId"))
            .and_then(Value::as_str)
            .map(str::to_string);
        let Some(from) = from else {
            panic!("the recorded history page carries no event id to resume from")
        };
        let frames = lane
            .raw_events(session, Some(&from), Duration::from_secs(5))
            .await
            .expect("recording the replayed stream");
        for (event, data) in frames {
            self.inventory
                .push(format!("replayed {event}/{}", update_kind(&data)));
            if event == "update" {
                self.frames.push(data);
            }
        }
    }

    /// Note a LIVE frame's kind, so the provenance card can say what the run
    /// actually saw even though the committed jsonl is the replay.
    fn frame(&mut self, ev: &LaneEvent) {
        if !self.on {
            return;
        }
        let label = match ev {
            LaneEvent::Message { message, .. } => format!("live message/{}", message.msg_type),
            LaneEvent::Session { .. } => "live session".to_string(),
            LaneEvent::Approval { .. } => "live approval".to_string(),
            LaneEvent::Reset { reason, .. } => format!("live reset/{reason}"),
            LaneEvent::Ready { .. } => "live ready".to_string(),
            LaneEvent::Down { reason } => format!("live down/{reason}"),
            LaneEvent::Unknown => "live unknown".to_string(),
        };
        self.inventory.push(label);
    }

    /// Whether this run writes fixtures at all.
    fn on(&self) -> bool {
        self.on
    }

    fn finish(self, record: &GxRecord) {
        if !self.on {
            eprintln!("live_smoke: not recording (set SHED_GX_RECORD=1 to refresh the fixtures)");
            return;
        }
        assert!(
            !self.frames.is_empty(),
            "the replay produced no update frames; there is nothing to commit"
        );
        let dir = fixtures_dir(&record.version);
        let guard = Guard::new(&scratch_roots());

        let frames = format!("{}\n", self.frames.join("\n"));
        let approvals = if self.approvals.is_empty() {
            String::new()
        } else {
            format!("{}\n", self.approvals.join("\n"))
        };
        let history = format!("{}\n", self.history.trim_end());

        // EVERYTHING is checked before ANYTHING is written: a partial write
        // would leave a directory that looks recorded and is not.
        let files = [
            ("history.json", &history),
            ("event-frames.jsonl", &frames),
            ("approvals.jsonl", &approvals),
        ];
        for (name, body) in files {
            if let Err(why) = guard.check(name, body) {
                panic!(
                    "REFUSING to write the gx fixtures: {why}\n\
                     Nothing was written. Re-record against the scratch leader and \
                     the scratch project, or widen SHED_GX_SCRATCH if the run is \
                     genuinely somewhere else."
                );
            }
        }

        std::fs::create_dir_all(&dir).expect("creating the fixtures directory");
        for (name, body) in files {
            if body.is_empty() {
                continue;
            }
            std::fs::write(dir.join(name), body).unwrap_or_else(|e| panic!("writing {name}: {e}"));
        }
        if approvals.is_empty() {
            eprintln!(
                "live_smoke: no approval was captured, so approvals.jsonl is left as it \
                 was (or absent). The provenance card records that."
            );
        }
        let mut inventory = self.inventory.clone();
        inventory.push(if approvals.is_empty() {
            "approvals.jsonl: NOT refreshed by this run (nothing was open)".to_string()
        } else {
            format!(
                "approvals.jsonl: {} resource(s) captured while pending",
                self.approvals.len()
            )
        });
        write_provenance(&dir, record, self.frames.len(), &inventory);
        eprintln!(
            "live_smoke: recorded {} update frames into {}",
            self.frames.len(),
            dir.display()
        );
    }
}

/// The `sessionUpdate` discriminator of one recorded payload, for the card's
/// inventory. `-` when the frame carries none (a `session`, an `approval`, or
/// one of gx's id-less lifecycle envelopes).
fn update_kind(data: &str) -> String {
    serde_json::from_str::<Value>(data)
        .ok()
        .and_then(|v| {
            v.get("params")
                .and_then(|p| p.get("update"))
                .and_then(|u| u.get("sessionUpdate"))
                .and_then(Value::as_str)
                .map(str::to_string)
                .or_else(|| v.get("method").and_then(Value::as_str).map(str::to_string))
        })
        .unwrap_or_else(|| "-".to_string())
}

fn fixtures_dir(version: &str) -> PathBuf {
    PathBuf::from(env!("CARGO_MANIFEST_DIR"))
        .join("fixtures")
        .join(if version.is_empty() {
            RECORDED_VERSION
        } else {
            version
        })
}

/// The directories a committed fixture is allowed to mention.
fn scratch_roots() -> Vec<PathBuf> {
    match std::env::var_os("SHED_GX_SCRATCH") {
        Some(s) if !s.is_empty() => std::env::split_paths(&s).collect(),
        _ => {
            let home = PathBuf::from(std::env::var_os("HOME").expect("HOME"));
            vec![home.join(".cache/shed-plan017")]
        }
    }
}

/// Refuses a recording that carries a secret's SHAPE or a path from outside the
/// scratch tree.
struct Guard {
    roots: Vec<String>,
}

impl Guard {
    fn new(roots: &[PathBuf]) -> Guard {
        Guard {
            roots: roots
                .iter()
                .map(|p| p.to_string_lossy().into_owned())
                .collect(),
        }
    }

    fn check(&self, name: &str, body: &str) -> Result<(), String> {
        if let Some(run) = first_hex64(body) {
            return Err(format!(
                "{name} carries a {}-character lowercase hex run, which is a gx \
                 token's exact shape (starts {}…)",
                run.len(),
                &run[..8]
            ));
        }
        for path in absolute_paths(body) {
            if !self.roots.iter().any(|root| path.starts_with(root)) {
                return Err(format!(
                    "{name} names the path {path}, which is outside the scratch \
                     directories {:?}",
                    self.roots
                ));
            }
        }
        Ok(())
    }
}

/// The first run of 64 or more lowercase hex digits, bounded by non-hex on both
/// sides.
///
/// Bounded, because a token is exactly 64 and a longer hex blob is not one —
/// but a longer run CONTAINS 64 consecutive hex characters, so it is refused
/// too rather than sliced. A fixture has no business carrying either.
fn first_hex64(body: &str) -> Option<String> {
    let is_hex = |c: char| c.is_ascii_digit() || ('a'..='f').contains(&c);
    let mut run = String::new();
    for c in body.chars() {
        if is_hex(c) {
            run.push(c);
            continue;
        }
        if run.len() >= 64 {
            return Some(run);
        }
        run.clear();
    }
    (run.len() >= 64).then_some(run)
}

/// Every absolute path the text mentions, under the roots a home directory can
/// live below.
///
/// Deliberately crude and deliberately noisy: it is a REFUSAL gate, so a false
/// positive costs a `SHED_GX_SCRATCH` entry and a false negative costs a
/// committed secret.
fn absolute_paths(body: &str) -> Vec<String> {
    const ROOTS: [&str; 4] = ["/home/", "/Users/", "/root/", "/var/"];
    let mut out = Vec::new();
    for root in ROOTS {
        let mut rest = body;
        while let Some(at) = rest.find(root) {
            let tail = &rest[at..];
            let end = tail
                .find(|c: char| {
                    c.is_whitespace() || matches!(c, '"' | '\'' | ',' | ')' | ']' | '}' | '\\')
                })
                .unwrap_or(tail.len());
            out.push(tail[..end].to_string());
            rest = &tail[end.max(1)..];
        }
    }
    out
}

/// The machine-written card a re-record leaves beside the frames.
///
/// **Deliberately NOT `README.md`.** That file is hand-maintained and carries
/// the regeneration recipe, the frame inventory a reader needs and the
/// paragraph about what the golden is. A run that wrote its template over it
/// would delete the documentation for the path it is part of, on the very run
/// that exercises it.
fn write_provenance(dir: &Path, record: &GxRecord, frames: usize, inventory: &[String]) {
    let mut kinds: Vec<String> = Vec::new();
    for label in inventory {
        if !kinds.contains(label) {
            kinds.push(label.clone());
        }
    }
    let card = format!(
        "# gx {version} — recording provenance\n\
         \n\
         Written by `crates/shed-gx/tests/live.rs` on each re-record. It is\n\
         GENERATED: edit `README.md` (hand-maintained, beside this file) for anything\n\
         a run cannot know, and `../README.md` for the regeneration recipe.\n\
         \n\
         Recorded on {date} with:\n\
         \n\
         ```bash\n\
         SHED_GX_LIVE=1 SHED_GX_RECORD=1 \\\n\
         \x20 cargo test -p shed-gx --features test-support --test live -- --nocapture\n\
         ```\n\
         \n\
         - gx version (from the leader's own `healthz`/record): `{version}`\n\
         - `history.json`: the body of `GET /v1/sessions/{{id}}/history?offset=-200&limit=200`, verbatim.\n\
         - `event-frames.jsonl`: {frames} `event: update` payloads, verbatim, in the order a\n\
         \x20 RESUMING client receives them — captured by reconnecting with `Last-Event-ID` set to\n\
         \x20 the first id in the history page above, not by teeing the live stream.\n\
         - `approvals.jsonl`: one approval resource per line, verbatim, from `GET …/approvals`.\n\
         \n\
         ## What the run saw\n\
         \n\
         {kinds}\n\
         \n\
         Re-record on a gx upgrade into a directory named for the new version; do not\n\
         overwrite this one.\n",
        version = record.version,
        date = today(),
        frames = frames,
        kinds = if kinds.is_empty() {
            "- (nothing recorded)".to_string()
        } else {
            kinds
                .iter()
                .map(|k| format!("- `{k}`"))
                .collect::<Vec<_>>()
                .join("\n")
        },
    );
    std::fs::write(dir.join(PROVENANCE), card).expect("writing the fixtures provenance card");
}

/// Today, as `YYYY-MM-DD`.
///
/// This crate has no `chrono` (its Cargo.toml says why), so the date is
/// computed from `SystemTime` with the civil-date algorithm
/// `shed_core::roost::rfc3339_z` uses — which is already in the dependency
/// graph and already tested.
fn today() -> String {
    let secs = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_secs() as i64)
        .unwrap_or(0);
    shed_core::roost::rfc3339_z(secs).chars().take(10).collect()
}
