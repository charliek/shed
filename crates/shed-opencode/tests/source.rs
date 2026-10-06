//! [`OpencodeSource`]'s subscription: the polled session list (plan 025 P7,
//! §3.2.6), against [`FakeOpencode`].
//!
//! Every stream here is ALSO judged by the contract's conformance kit
//! ([`SourceChecker`]), so a cell that asserts one property cannot pass on a
//! stream that broke another.
//!
//! # How a lag is forced, deterministically
//!
//! The poller publishes a whole round (a seed, or a diff) with no `await`
//! between its frames, and every test runs on the current-thread runtime — so
//! a consumer that is not inside `recv()` cannot drain a frame of a round. A lag
//! then needs only arithmetic: a seed that FITS the bound, left unread, plus a
//! diff that takes the queue past it. And a reseed after the lag has to fit
//! again, or it lags identically forever — so the list a reseed reads is shrunk
//! (or grown) before the drain that lets it start.

mod common;

use std::time::{Duration, Instant};

use common::{assert_clean, wait_for, DEADLINE};
use shed_core::lane::conformance::SourceChecker;
use shed_core::lane::{
    AgentSource, SourceEvent, SourceOffline, SourceSubscription, LANE_CHANNEL_CAPACITY,
    MAX_SOURCE_ROWS,
};
use shed_core::rc::RcActivity;
use shed_opencode::testing::FakeOpencode;
use shed_opencode::OpencodeSource;
use tokio::sync::mpsc::Receiver;

/// Fast enough that a test watches several polls in milliseconds.
const POLL: Duration = Duration::from_millis(20);

fn source(fake: &FakeOpencode) -> OpencodeSource {
    common::source(fake).with_poll_interval(POLL)
}

async fn subscribe(fake: &FakeOpencode) -> SourceSubscription {
    source(fake)
        .subscribe()
        .await
        .expect("subscribe returns at once")
}

/// The next frame, judged by the kit as it is read.
async fn next(
    rx: &mut Receiver<SourceEvent>,
    checker: &mut SourceChecker,
    what: &str,
) -> SourceEvent {
    let ev = tokio::time::timeout(DEADLINE, rx.recv())
        .await
        .unwrap_or_else(|_| panic!("timed out after {DEADLINE:?} waiting for {what}"))
        .unwrap_or_else(|| panic!("the source stream ENDED while waiting for {what}"));
    checker
        .observe(&ev)
        .unwrap_or_else(|v| panic!("{v} — while waiting for {what}"));
    ev
}

/// Every frame up to and including the next `Ready`, judged.
///
/// Bounded OVERALL, not per frame: a source that reseeds forever without
/// reaching `Ready` (a seed too big for the channel lags at the same frame on
/// every attempt) keeps every single `recv` busy, so only a total deadline
/// turns that into a failure instead of a hang.
async fn until_ready(
    rx: &mut Receiver<SourceEvent>,
    checker: &mut SourceChecker,
) -> Vec<SourceEvent> {
    let deadline = Instant::now() + DEADLINE;
    let mut out = Vec::new();
    loop {
        assert!(
            Instant::now() < deadline,
            "no Ready within {DEADLINE:?} — {} frames, {} Resets, {} Readies: a seed \
             that never fits the channel reseeds forever",
            out.len(),
            resets(&out).len(),
            readies(&out)
        );
        let ev = next(rx, checker, "the seed's Ready").await;
        let done = matches!(ev, SourceEvent::Ready { .. });
        out.push(ev);
        if done {
            return out;
        }
    }
}

/// Everything already queued, judged, without waiting.
fn drain_now(rx: &mut Receiver<SourceEvent>, checker: &mut SourceChecker) -> Vec<SourceEvent> {
    let mut out = Vec::new();
    while let Ok(ev) = rx.try_recv() {
        checker.observe(&ev).unwrap_or_else(|v| panic!("{v}"));
        out.push(ev);
    }
    out
}

/// Nothing arrives for `quiet` — several polls' worth.
async fn assert_quiet(rx: &mut Receiver<SourceEvent>, quiet: Duration, why: &str) {
    if let Ok(ev) = tokio::time::timeout(quiet, rx.recv()).await {
        panic!("{why}, but the source said {ev:?}");
    }
}

fn row_ids(events: &[SourceEvent]) -> Vec<String> {
    events
        .iter()
        .filter_map(|e| match e {
            SourceEvent::Session { session } => Some(session.id.clone()),
            _ => None,
        })
        .collect()
}

fn resets(events: &[SourceEvent]) -> Vec<(String, u64)> {
    events
        .iter()
        .filter_map(|e| match e {
            SourceEvent::Reset { reason, generation } => Some((reason.clone(), *generation)),
            _ => None,
        })
        .collect()
}

fn readies(events: &[SourceEvent]) -> usize {
    events
        .iter()
        .filter(|e| matches!(e, SourceEvent::Ready { .. }))
        .count()
}

/// The roster GETs served so far — one per poll.
fn polls(fake: &FakeOpencode) -> usize {
    fake.get_paths().iter().filter(|p| *p == "/session").count()
}

/// The seed: `Reset`, the roots (never a child), the capabilities, `Ready` —
/// and then a diff, not a reseed: an upsert for a row that changed or joined, a
/// `Removed` for one that left, and silence for one that did not change.
#[tokio::test]
async fn the_seed_then_upserts_and_removals_between_seeds() {
    let fake = FakeOpencode::start().await;
    fake.add_session("ses_a", "a", "/w", None);
    fake.add_session("ses_b", "b", "/w", None);
    fake.add_session("ses_child", "child", "/w", Some("ses_a"));
    let (mut rx, _stop) = subscribe(&fake).await.into_parts();
    let mut checker = SourceChecker::new();

    let seed = until_ready(&mut rx, &mut checker).await;
    assert_eq!(resets(&seed), vec![("seed".to_string(), 1)]);
    assert_eq!(
        row_ids(&seed),
        ["ses_a", "ses_b"],
        "roots only, in id order"
    );
    let caps = seed
        .iter()
        .find_map(|e| match e {
            SourceEvent::Capabilities { capabilities } => Some(capabilities.clone()),
            _ => None,
        })
        .expect("every seed carries the source's capabilities");
    assert_eq!(caps, shed_opencode::opencode_source_capabilities());
    assert!(caps.create && caps.create_options);
    assert!(matches!(
        seed.last(),
        Some(SourceEvent::Ready {
            generation: 1,
            truncated: false
        })
    ));

    // An unchanged list says nothing, poll after poll.
    assert_quiet(&mut rx, POLL * 6, "nothing changed").await;

    fake.set_status("ses_a", "busy");
    match next(&mut rx, &mut checker, "ses_a's upsert").await {
        SourceEvent::Session { session } => {
            assert_eq!(session.id, "ses_a");
            assert_eq!(session.activity, RcActivity::Working);
            assert!(session.approximate, "a poll's row says it is one");
        }
        other => panic!("a changed row is an upsert, not {other:?}"),
    }
    fake.add_session("ses_c", "c", "/w", None);
    match next(&mut rx, &mut checker, "ses_c's arrival").await {
        SourceEvent::Session { session } => assert_eq!(session.id, "ses_c"),
        other => panic!("a new row is an upsert, not {other:?}"),
    }
    fake.remove_session("ses_b");
    assert_eq!(
        next(&mut rx, &mut checker, "ses_b's removal").await,
        SourceEvent::Removed {
            session_id: "ses_b".into()
        }
    );
    assert_eq!(checker.resets(), 1, "between seeds there is no reseed");
    assert_clean(&fake);
}

/// **A failed poll emits ONLY `Offline{Unreachable}`** — once, no `Reset`, no
/// `Removed`: the client keeps its last `Ready` view — **and the first good
/// poll after it reseeds** the whole list, which is how a row that left during
/// the outage is learned (a reseed carries no `Removed`).
#[tokio::test]
async fn a_failed_poll_is_offline_alone_and_the_next_good_one_reseeds() {
    let fake = FakeOpencode::start().await;
    fake.add_session("ses_a", "a", "/w", None);
    fake.add_session("ses_b", "b", "/w", None);
    let (mut rx, _stop) = subscribe(&fake).await.into_parts();
    let mut checker = SourceChecker::new();
    until_ready(&mut rx, &mut checker).await;

    fake.fail_get("/session", 500);
    match next(&mut rx, &mut checker, "the outage").await {
        SourceEvent::Offline { cause, reason } => {
            assert_eq!(cause, SourceOffline::Unreachable);
            assert!(!reason.is_empty(), "the reason says what failed");
        }
        other => panic!("a failed poll is Offline, not {other:?}"),
    }
    let polled = polls(&fake);
    wait_for("several more failed polls", || polls(&fake) >= polled + 3).await;
    assert_quiet(
        &mut rx,
        POLL * 3,
        "an outage is announced ONCE, and nothing else is said during it",
    )
    .await;

    // A row leaves while the source cannot see; then the server comes back.
    fake.remove_session("ses_b");
    fake.unfail_get("/session");
    let reseed = until_ready(&mut rx, &mut checker).await;
    assert_eq!(
        resets(&reseed),
        vec![("reconnect".to_string(), 2)],
        "the first good poll after an outage reseeds"
    );
    assert_eq!(row_ids(&reseed), ["ses_a"], "the reseed is the whole truth");
    assert!(
        !reseed
            .iter()
            .any(|e| matches!(e, SourceEvent::Removed { .. })),
        "a reseed replaces the list; it does not diff it"
    );
    assert_clean(&fake);
}

/// A source may say `Offline` before it ever says `Reset` — and an unreachable
/// server is the stream's business, never `subscribe`'s error.
#[tokio::test]
async fn a_server_that_is_down_from_the_start_is_offline_before_any_seed() {
    let fake = FakeOpencode::start().await;
    fake.add_session("ses_a", "a", "/w", None);
    fake.fail_get("/session", 503);
    let (mut rx, _stop) = subscribe(&fake).await.into_parts();
    let mut checker = SourceChecker::new();

    assert!(matches!(
        next(&mut rx, &mut checker, "the first word").await,
        SourceEvent::Offline {
            cause: SourceOffline::Unreachable,
            ..
        }
    ));
    fake.unfail_get("/session");
    let seed = until_ready(&mut rx, &mut checker).await;
    assert_eq!(
        resets(&seed),
        vec![("seed".to_string(), 1)],
        "the first seed is a seed, however late"
    );
    assert_eq!(row_ids(&seed), ["ses_a"]);

    // Nothing listening at all: still a subscription, and still Offline.
    let dead = {
        let socket = tokio::net::TcpListener::bind(("127.0.0.1", 0))
            .await
            .expect("a throwaway port");
        let addr = socket.local_addr().expect("its address");
        drop(socket);
        OpencodeSource::new(format!("http://{addr}/").parse().expect("url"), None)
            .expect("the source builds")
            .with_poll_interval(POLL)
    };
    let (mut rx, _stop) = dead
        .subscribe()
        .await
        .expect("subscribe never fails on an outage")
        .into_parts();
    let mut checker = SourceChecker::new();
    assert!(matches!(
        next(&mut rx, &mut checker, "a refused dial").await,
        SourceEvent::Offline {
            cause: SourceOffline::Unreachable,
            ..
        }
    ));
    assert_clean(&fake);
}

/// `n` idle roots in one directory, ids sorting in creation order.
fn add_roots(fake: &FakeOpencode, from: usize, n: usize) {
    for i in from..from + n {
        fake.add_session(&format!("ses_{i:05}"), "r", "/w", None);
    }
}

/// **Correction 13, on the source's level**: a consumer that reads nothing
/// while a diff takes its queue past the bound gets the round abandoned at the
/// first dropped frame — no frame after the gap, and no `Ready` after it — the
/// poller stops POLLING while it waits for the drain, and once the client
/// drains it gets one `Reset{lagged}` … `Ready` carrying the whole list.
#[tokio::test]
async fn a_diff_past_the_bound_is_abandoned_and_reseeds_as_lagged() {
    // A full seed (515 frames) fits; it plus 512 upserts does not.
    const ROWS: usize = MAX_SOURCE_ROWS;
    let fake = FakeOpencode::start().await;
    add_roots(&fake, 0, ROWS);
    let (mut rx, _stop) = subscribe(&fake).await.into_parts();
    let mut checker = SourceChecker::new();

    // The consumer reads NOTHING from here on.
    wait_for("the seed to be queued", || rx.len() == ROWS + 3).await;
    for i in 0..ROWS {
        fake.set_status(&format!("ses_{i:05}"), "busy");
    }
    wait_for("the diff to fill the queue", || {
        rx.len() == LANE_CHANNEL_CAPACITY
    })
    .await;
    // The poller is parked on the drain: it polls no more, so a client that
    // never reads costs the server nothing.
    let parked = polls(&fake);
    tokio::time::sleep(POLL * 10).await;
    assert_eq!(
        polls(&fake),
        parked,
        "a lagged source waits for the drain; it does not keep polling"
    );

    let abandoned = drain_now(&mut rx, &mut checker);
    assert_eq!(
        abandoned.len(),
        LANE_CHANNEL_CAPACITY,
        "exactly the bound was queued; everything past it was dropped"
    );
    assert_eq!(
        readies(&abandoned),
        1,
        "the seed's Ready, and no Ready after the gap"
    );
    assert!(
        matches!(abandoned.last(), Some(SourceEvent::Session { .. })),
        "the queue ends mid-diff: nothing was published past the gap"
    );

    let drained_at = Instant::now();
    let reseed = until_ready(&mut rx, &mut checker).await;
    assert!(
        drained_at.elapsed() >= Duration::from_millis(100),
        "the reseed waits out the (jittered) failure backoff, not just the drain"
    );
    assert_eq!(resets(&reseed), vec![("lagged".to_string(), 2)]);
    assert_eq!(row_ids(&reseed).len(), ROWS, "the whole list, once");
    assert!(reseed.iter().all(|e| match e {
        SourceEvent::Session { session } => session.activity == RcActivity::Working,
        _ => true,
    }));
    assert_clean(&fake);
}

/// **A seed past the channel is cut to `MAX_SOURCE_ROWS` and says so**
/// (review, astra 3). A server with more sessions than the client channel holds
/// frames used to publish a seed — `Reset`, every row, `Capabilities`, `Ready`
/// — that lagged at the same frame on every attempt, reseeding forever and
/// never reaching `Ready`, even for a client reading as fast as it could: on a
/// current-thread runtime a seed is one burst nobody can drain mid-way. Now the
/// seed carries the first 512 rows by id and `Ready { truncated: true }`; and a
/// poll whose list falls back under the cap reseeds, so `truncated` is never a
/// stale claim.
#[tokio::test]
async fn a_seed_past_the_channel_is_cut_to_the_cap_and_says_truncated() {
    const ROWS: usize = LANE_CHANNEL_CAPACITY + 76; // a seed of them could never fit
    let fake = FakeOpencode::start().await;
    add_roots(&fake, 0, ROWS);
    let (mut rx, _stop) = subscribe(&fake).await.into_parts();
    let mut checker = SourceChecker::new();

    let seed = until_ready(&mut rx, &mut checker).await;
    assert_eq!(
        resets(&seed),
        vec![("seed".to_string(), 1)],
        "it seeded at once"
    );
    let ids = row_ids(&seed);
    assert_eq!(ids.len(), MAX_SOURCE_ROWS, "the seed is cut to the cap");
    assert_eq!(
        ids,
        (0..MAX_SOURCE_ROWS)
            .map(|i| format!("ses_{i:05}"))
            .collect::<Vec<_>>(),
        "the first rows by id, so the cut is stable poll to poll"
    );
    assert!(
        matches!(
            seed.last(),
            Some(SourceEvent::Ready {
                generation: 1,
                truncated: true
            })
        ),
        "a cut seed says so: {:?}",
        seed.last()
    );
    assert_quiet(
        &mut rx,
        POLL * 6,
        "a cut list that did not change says nothing",
    )
    .await;

    // The list falls back under the cap: a diff cannot clear `truncated`, so
    // the source reseeds.
    for i in 100..ROWS {
        fake.remove_session(&format!("ses_{i:05}"));
    }
    let reseed = until_ready(&mut rx, &mut checker).await;
    assert_eq!(resets(&reseed), vec![("truncated".to_string(), 2)]);
    assert_eq!(row_ids(&reseed).len(), 100);
    assert!(matches!(
        reseed.last(),
        Some(SourceEvent::Ready {
            generation: 2,
            truncated: false
        })
    ));
    assert_clean(&fake);
}

/// **Isolated lags each restart the backoff at the floor.** A source seed
/// always fits a drained channel (it is capped), so a lagged reseed reaches its
/// `Ready` — and a seed that reached `Ready` WORKED, so the next lag is a first
/// failure again, not the next step of a climb. Three lags, each separated by a
/// recovered seed, each wait one first step (jittered `2 × OC_BACKOFF_BASE`:
/// 100–200 ms), never the climb's 400+.
#[tokio::test]
async fn isolated_lags_each_restart_the_backoff_at_the_floor() {
    const ROWS: usize = MAX_SOURCE_ROWS;
    let fake = FakeOpencode::start().await;
    add_roots(&fake, 0, ROWS);
    let (mut rx, _stop) = subscribe(&fake).await.into_parts();
    let mut checker = SourceChecker::new();

    for cycle in 0..3usize {
        // The (re)seed, queued whole and unread.
        wait_for("the seed to be queued", || rx.len() == ROWS + 3).await;
        // Every row's activity flips, so the next diff is 512 upserts on top
        // of it: past the bound.
        let status = if cycle % 2 == 0 { "busy" } else { "" };
        for i in 0..ROWS {
            fake.set_status(&format!("ses_{i:05}"), status);
        }
        wait_for("the lag", || rx.len() == LANE_CHANNEL_CAPACITY).await;
        let abandoned = drain_now(&mut rx, &mut checker);
        assert_eq!(
            readies(&abandoned),
            1,
            "cycle {cycle}: the seed's Ready and none after the gap"
        );

        let drained_at = Instant::now();
        wait_for("the lagged reseed to say anything at all", || {
            !rx.is_empty()
        })
        .await;
        let waited = drained_at.elapsed();
        assert!(
            waited >= Duration::from_millis(100),
            "cycle {cycle}: a lag waits out the first backoff step: {waited:?}"
        );
        assert!(
            waited < Duration::from_millis(350),
            "cycle {cycle}: the previous seed reached Ready, so this lag starts the \
             curve over — it waited {waited:?}, the climb's next step"
        );
    }
    let last = until_ready(&mut rx, &mut checker).await;
    assert_eq!(resets(&last), vec![("lagged".to_string(), 4)]);
    assert_clean(&fake);
}
