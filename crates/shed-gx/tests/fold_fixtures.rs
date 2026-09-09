//! The replay golden — this crate's fold over a **committed recording of real
//! gx wire**.
//!
//! It is a **regression pin on shed-gx's own behaviour**, not a fidelity proof
//! against anything: nobody else minted it, so a diff here is a behaviour
//! change to read, never evidence of agreement with another implementation.
//! (`shed-opencode` has both kinds and its `fixtures/README.md` is scrupulous
//! about the difference; shed-gx has only this one, because gx's lane has no
//! second implementation to agree with.)
//!
//! What makes the recording worth having is that it carries things no
//! hand-written fixture would think to: **counters that are not monotonic in
//! transcript order** (gx's ids come off an atomic several sessions bump),
//! **id-less envelopes**, `hook_execution` interleaved through chunk streaks,
//! and a real permission whose five option ids are opaque and two of which
//! declare `allow_once`.
//!
//! ## Regenerating
//!
//! ```text
//! SHED_GX_REGOLD=1 cargo test -p shed-gx --test fold_fixtures
//! ```
//!
//! rewrites the golden from the COMMITTED recording — offline, free,
//! deterministic, no gx — and then asserts against what it wrote, so a regen
//! that does not round-trip still fails. Re-recording the WIRE is a separate,
//! live step; `fixtures/README.md` documents both and which one spends quota.

use serde_json::{json, Map, Value};
use shed_core::lane::LaneApproval;
use shed_core::rc::RcFeedMessage;
use shed_gx::fold::{lane_approval, GxApprovalResource};
use shed_gx::{EventId, GxEnvelope, GxFold};

/// The recording. Named for the gx that produced it: a newer gx records BESIDE
/// this directory, never over it.
const RECORDING: &str = "fixtures/1.0.16+gx.12";
const GOLDEN: &str = "fixtures/1.0.16+gx.12/fold.golden.json";

const REGOLD_ENV: &str = "SHED_GX_REGOLD";

fn regold() -> bool {
    std::env::var_os(REGOLD_ENV).is_some_and(|v| !v.is_empty() && v != "0")
}

fn fixture(name: &str) -> String {
    let path = format!("{}/{RECORDING}/{name}", env!("CARGO_MANIFEST_DIR"));
    std::fs::read_to_string(&path).unwrap_or_else(|e| panic!("reading {path}: {e}"))
}

/// The `updates` array of the recorded `GET …/history` body.
fn history() -> Vec<GxEnvelope> {
    let page: Value = serde_json::from_str(&fixture("history.json")).expect("history.json is JSON");
    page.get("updates")
        .and_then(Value::as_array)
        .expect("the history page carries `updates`")
        .iter()
        .map(|v| serde_json::from_value(v.clone()).expect("an envelope decodes"))
        .collect()
}

/// The recorded SSE `update` payloads, in the order a resuming client receives
/// them.
fn frames() -> Vec<GxEnvelope> {
    fixture("event-frames.jsonl")
        .lines()
        .filter(|l| !l.trim().is_empty())
        .map(|l| serde_json::from_str(l).expect("a recorded frame decodes"))
        .collect()
}

fn approvals() -> Vec<GxApprovalResource> {
    fixture("approvals.jsonl")
        .lines()
        .filter(|l| !l.trim().is_empty())
        .map(|l| serde_json::from_str(l).expect("a recorded approval decodes"))
        .collect()
}

/// The session every recorded envelope belongs to, taken from the recording
/// rather than hardcoded — the fold refuses an id whose prefix is somebody
/// else's, so getting this from the file is what keeps a re-record honest.
fn session_id(envs: &[GxEnvelope]) -> String {
    envs.iter()
        .map(|e| e.params.session_id.clone())
        .find(|s| !s.is_empty())
        .expect("the recording names its session")
}

fn counter(env: &GxEnvelope) -> Option<u64> {
    env.event_id
        .as_deref()
        .and_then(EventId::parse)
        .map(|e| e.counter)
}

fn kind_of(env: &GxEnvelope) -> String {
    let k = env.session_update();
    if k.is_empty() {
        env.method.clone()
    } else {
        k.to_string()
    }
}

// ---------------------------------------------------------------------------
// the projection
// ---------------------------------------------------------------------------

fn row(m: &RcFeedMessage) -> Value {
    serde_json::to_value(m).expect("a feed row serializes")
}

/// One replay, step by step: what each envelope was, whether it was applied,
/// the verdict and cursor after it, and the rows it produced.
fn project(envs: &[GxEnvelope], session: &str) -> Value {
    let mut fold = GxFold::new(session);
    let mut steps = Vec::new();
    for (i, env) in envs.iter().enumerate() {
        let applied = fold.apply(env);
        let rows: Vec<Value> = fold.drain_messages().iter().map(row).collect();
        steps.push(json!({
            "i": i,
            "eventId": env.event_id,
            "kind": kind_of(env),
            "applied": applied,
            "activity": fold.activity().as_str(),
            "cursor": fold.resume_cursor(),
            "rows": rows,
        }));
    }
    // A standalone replay ENDS here, so the last streak is flushed as its own
    // row — the same rule `AgentLane::history` follows and the live watcher
    // deliberately does not.
    fold.flush_open();
    let tail: Vec<Value> = fold.drain_messages().iter().map(row).collect();
    json!({
        "steps": steps,
        "final": {
            "flushed": tail,
            "activity": fold.activity().as_str(),
            "cursor": fold.resume_cursor(),
            "open_approvals": fold.open_approvals(),
            "tools_retained": fold.tools_len(),
        },
    })
}

/// What a client actually RECEIVES when the stream breaks after `split` frames
/// and it resumes silently.
///
/// Faithful to `gx-remote-api`'s replay, which is by COUNTER: the client sends
/// the highest counter it has seen, and gx answers with every ring entry whose
/// counter is greater. So the resumed client gets
///
/// - the frames it already had (`..split`), plus
/// - every ID-BEARING frame in the session whose counter beats that maximum,
///
/// and **nothing else**. Two things are therefore left behind:
///
/// - a frame minted after the break whose counter happens to be SMALLER than
///   the maximum. That is gx's documented residual (plan 017 §9), and modelling
///   it is what makes this test say something instead of trivially replaying
///   everything;
/// - an ID-LESS envelope in the gap. gx's replay is by counter on both its ring
///   and its disk path (`plan_replay` case 4 skips an envelope with no
///   counter), so one is simply never resumable. It is not counted as loss
///   here, and `id_less_envelopes_fold_to_nothing` is what earns that: in this
///   recording every one of them is a lifecycle envelope the fold ignores, so
///   losing it costs no row.
fn received_after_resume(envs: &[GxEnvelope], split: usize) -> (Vec<&GxEnvelope>, bool) {
    let cursor = envs[..split].iter().filter_map(counter).max();
    let Some(cursor) = cursor else {
        // Nothing id-bearing was seen, so there is no cursor and a real
        // watcher would reseed rather than resume. Everything arrives.
        return (envs.iter().collect(), true);
    };
    let mut received: Vec<&GxEnvelope> = envs[..split].iter().collect();
    received.extend(
        envs.iter()
            .filter(|e| counter(e).is_some_and(|k| k > cursor)),
    );
    let lossless = envs[split..].iter().filter_map(counter).all(|k| k > cursor);
    (received, lossless)
}

/// Every row a replay of `envs` produces, in order — the one definition, taken
/// by anything iterable so a slice and a filtered borrow share it.
fn rows_from<'a, I: IntoIterator<Item = &'a GxEnvelope>>(
    envs: I,
    session: &str,
) -> Vec<RcFeedMessage> {
    let mut fold = GxFold::new(session);
    let mut out = Vec::new();
    for env in envs {
        fold.apply(env);
        out.extend(fold.drain_messages());
    }
    fold.flush_open();
    out.extend(fold.drain_messages());
    out
}

// ---------------------------------------------------------------------------
// the golden
// ---------------------------------------------------------------------------

fn header() -> Vec<(&'static str, Value)> {
    vec![
        (
            "_comment",
            json!(
                "REGRESSION PIN on shed-gx's OWN fold, replayed over the committed gx \
                 1.0.16+gx.12 recording. Nothing but this crate minted it, so a diff here \
                 is a behaviour change to review — never evidence of agreement with another \
                 implementation. Regenerate OFFLINE with SHED_GX_REGOLD=1 cargo test -p \
                 shed-gx --test fold_fixtures; re-recording the wire itself is a separate \
                 live step (see fixtures/README.md)."
            ),
        ),
        ("fixture", json!(RECORDING)),
    ]
}

fn build_golden() -> Value {
    let history = history();
    let frames = frames();
    let session = session_id(&frames);

    let approval_dtos: Vec<Value> = approvals()
        .iter()
        .map(|res| serde_json::to_value(lane_approval(res)).expect("a LaneApproval serializes"))
        .collect();

    let straight = rows_from(&frames, &session);
    let mut lossy_splits = Vec::new();
    for split in 0..=frames.len() {
        let (_, lossless) = received_after_resume(&frames, split);
        if !lossless {
            lossy_splits.push(split);
        }
    }

    let mut out = Map::new();
    for (k, v) in header() {
        out.insert(k.to_string(), v);
    }
    out.insert("session".to_string(), json!(session));
    out.insert("history".to_string(), project(&history, &session));
    out.insert("stream".to_string(), project(&frames, &session));
    out.insert("approvals".to_string(), json!(approval_dtos));
    out.insert(
        "resume".to_string(),
        json!({
            "_comment": "Which break points gx's own by-counter replay cannot heal. \
                         Every OTHER split reproduces the straight replay row for row; \
                         these are the ones where a frame minted after the break carries \
                         a counter below the cursor and is never replayed (plan 017 §9).",
            "frames": frames.len(),
            "rows_straight_through": straight.len(),
            "lossy_splits": lossy_splits,
        }),
    );
    Value::Object(out)
}

fn read_golden() -> Value {
    let path = format!("{}/{GOLDEN}", env!("CARGO_MANIFEST_DIR"));
    let data = std::fs::read_to_string(&path).unwrap_or_else(|e| panic!("reading {GOLDEN}: {e}"));
    serde_json::from_str(&data).expect("the golden is JSON")
}

#[test]
fn replays_the_recording_against_the_golden() {
    let got = build_golden();
    if regold() {
        let path = format!("{}/{GOLDEN}", env!("CARGO_MANIFEST_DIR"));
        let mut text = serde_json::to_string_pretty(&got).expect("the golden serializes");
        text.push('\n');
        std::fs::write(&path, &text).unwrap_or_else(|e| panic!("writing {GOLDEN}: {e}"));
        eprintln!("regenerated {GOLDEN}");
    }
    // Asserted even on a regen, so a regeneration that does not round-trip
    // still fails rather than laundering itself into the file.
    assert_eq!(
        serde_json::to_string_pretty(&got).unwrap(),
        serde_json::to_string_pretty(&read_golden()).unwrap(),
        "the fold's output drifted from {GOLDEN}. If the change is deliberate, \
         regenerate with {REGOLD_ENV}=1 and read the diff AS the change."
    );
}

/// The recording exists for the things a hand-written fixture would not think
/// of. A truncated or sanitized re-record must not silently delete that
/// coverage.
#[test]
fn the_recording_still_carries_what_the_golden_is_for() {
    let frames = frames();
    assert!(frames.len() >= 40, "only {} frames", frames.len());

    let kinds: Vec<String> = frames.iter().map(kind_of).collect();
    for want in [
        "user_message_chunk",
        "agent_message_chunk",
        "agent_thought_chunk",
        "tool_call",
        "tool_call_update",
        "turn_completed",
        "hook_execution",
    ] {
        assert!(
            kinds.iter().any(|k| k == want),
            "the recording no longer carries {want}: {:?}",
            dedup(&kinds)
        );
    }

    // Non-monotonic counters in ARRIVAL order — the property the whole cursor
    // model exists for, and the reason `resume_cursor` is the maximum rather
    // than the last applied.
    let counters: Vec<u64> = frames.iter().filter_map(counter).collect();
    assert!(
        counters.windows(2).any(|w| w[0] > w[1]),
        "the recording's counters are monotonic, so it no longer exercises the \
         out-of-order case: {counters:?}"
    );

    // Id-less envelopes: never deduped, never a cursor.
    assert!(
        frames.iter().any(|e| e.event_id.is_none()),
        "the recording no longer carries an id-less envelope"
    );

    // The approval half: a real permission with OPAQUE ids, two of which
    // declare `allow_once` — which is what makes a three-valued decision
    // ambiguous on real gx and is unrepresentable in a tidy fixture.
    let approvals: Vec<LaneApproval> = approvals().iter().map(lane_approval).collect();
    let permission = approvals
        .iter()
        .find(|a| !a.options.is_empty())
        .expect("the recording carries an approval with options");
    assert!(
        permission
            .options
            .iter()
            .filter(|o| o.kind.as_deref() == Some("allow_once"))
            .count()
            >= 2,
        "the recorded permission no longer offers two allow_once options: {:?}",
        permission.options
    );
    assert!(
        permission.options.iter().any(|o| o.id == "allow-once"),
        "the option ids are the ones gx offered, verbatim: {:?}",
        permission.options
    );
    // And gx's placeholder — `method` and `request` both null — is in there
    // too, which is what a client sees FIRST when an agent blocks.
    assert!(
        approvals.iter().any(|a| a.options.is_empty()),
        "the recording no longer carries gx's option-less placeholder"
    );
}

/// **No row lost or duplicated across a simulated silent resume.**
///
/// Every possible break point is tried, because the interesting ones are not
/// where you would guess: gx replays by COUNTER and its counters are not
/// monotonic in arrival order, so whether a break is healable depends on the
/// frame, not on the position.
#[test]
fn a_silent_resume_loses_and_duplicates_nothing_that_gx_can_replay() {
    let frames = frames();
    let session = session_id(&frames);
    let straight = rows_from(&frames, &session);
    assert!(!straight.is_empty(), "the recording produces rows at all");

    let mut lossy = Vec::new();
    for split in 0..=frames.len() {
        let (received, lossless) = received_after_resume(&frames, split);
        let rows = rows_from(received, &session);
        if lossless {
            assert_eq!(
                rows, straight,
                "a resume at frame {split} is inside gx's replay window, so the \
                 transcript must come out identical — the `seen` set absorbs the \
                 overlap and nothing is folded twice"
            );
        } else {
            lossy.push(split);
            assert!(
                rows.len() <= straight.len(),
                "a resume at frame {split} produced MORE rows than a straight replay \
                 ({} > {}), which can only mean something was folded twice",
                rows.len(),
                straight.len()
            );
        }
    }
    assert!(
        !lossy.is_empty(),
        "no break point in this recording exercises gx's by-counter residual \
         (plan 017 §9). Either the recording became monotonic — in which case \
         re-record something interleaved — or the residual is gone and this \
         test should say so instead."
    );
}

/// The premise `received_after_resume` leans on: gx cannot replay an envelope
/// that carries no id, so this recording must not contain one that folds to a
/// row.
///
/// It holds because every id-less envelope here is a lifecycle frame the fold
/// ignores (`_x.ai/session/prompt_complete`, `last_turn_summary`). If a
/// re-record ever catches an id-less CHUNK, this fails — and it should: the
/// resume model would then be hiding a real lost row behind an exemption.
#[test]
fn id_less_envelopes_fold_to_nothing() {
    let frames = frames();
    let session = session_id(&frames);
    let id_less: Vec<&GxEnvelope> = frames.iter().filter(|e| e.event_id.is_none()).collect();
    assert!(
        !id_less.is_empty(),
        "the recording carries id-less envelopes"
    );

    for env in id_less {
        let mut fold = GxFold::new(&session);
        fold.apply(env);
        fold.flush_open();
        assert!(
            fold.drain_messages().is_empty(),
            "the id-less envelope {} folds to a row, so a resume that cannot \
             replay it would lose transcript",
            kind_of(env)
        );
    }
}

/// …and the mirror image: a RESEED forgets everything, so the same stream
/// replays in full and produces exactly the same rows.
///
/// That is what makes a `Reset` … `Ready` bracket a complete rebuild rather
/// than a silent no-op — and, together with the test above, what lets a client
/// swap the staged view in without comparing it to what it held.
#[test]
fn a_reseed_rebuilds_the_transcript_exactly() {
    let frames = frames();
    let session = session_id(&frames);

    let mut fold = GxFold::new(&session);
    let mut first = Vec::new();
    for env in &frames {
        fold.apply(env);
        first.extend(fold.drain_messages());
    }
    fold.flush_open();
    first.extend(fold.drain_messages());

    // A replay on a KEPT fold emits nothing: the dedup set is the whole point.
    for env in &frames {
        fold.apply(env);
    }
    assert!(
        fold.drain_messages().is_empty(),
        "replaying the same stream into a fold that was not reset must emit nothing"
    );

    fold.reset();
    let mut second = Vec::new();
    for env in &frames {
        fold.apply(env);
        second.extend(fold.drain_messages());
    }
    fold.flush_open();
    second.extend(fold.drain_messages());

    assert_eq!(
        second, first,
        "a reseed rebuilds the transcript row for row"
    );
}

/// The history page and the resumed stream are two views of ONE transcript, and
/// where they overlap they must fold to the same rows.
///
/// They are not the same bytes and not the same span: the page is what gx had
/// persisted when the run started, the stream is what a client resuming from
/// that page's FIRST id is handed. Because gx replays by counter and its
/// counters are not monotonic, the stream picks up somewhere in the MIDDLE of
/// the page and then runs past its end — so the honest shape of the claim is
/// "a suffix of the page is a prefix of the stream", and the test finds that
/// join rather than assuming where it is.
///
/// It is a real assertion about the fold being source-agnostic, which is what
/// lets a seed and a live stream be spliced at all.
#[test]
fn the_history_page_and_the_replayed_stream_agree_where_they_overlap() {
    let history = history();
    let frames = frames();
    let session = session_id(&frames);

    let text = |m: &RcFeedMessage| (m.role.clone(), m.msg_type.clone(), m.text.clone());
    let page: Vec<_> = rows_from(&history, &session).iter().map(text).collect();
    let stream: Vec<_> = rows_from(&frames, &session).iter().map(text).collect();
    assert!(!page.is_empty() && !stream.is_empty());

    // The longest join first: a short accidental match at the end would be no
    // evidence of anything.
    let join = (1..=page.len().min(stream.len()))
        .rev()
        .find(|n| page[page.len() - n..] == stream[..*n]);
    let Some(n) = join else {
        panic!(
            "no suffix of the persisted page is a prefix of the replayed stream — \
             the two views of one transcript disagree.\npage:   {page:#?}\nstream: {stream:#?}"
        )
    };
    assert!(
        n >= 3,
        "the two views overlap by only {n} row(s), which is not enough to be \
         evidence. Re-record with the stream resuming further back."
    );
    eprintln!("the page and the stream join over {n} rows");
}

fn dedup(items: &[String]) -> Vec<String> {
    let mut out: Vec<String> = Vec::new();
    for i in items {
        if !out.contains(i) {
            out.push(i.clone());
        }
    }
    out
}
