use super::*;

use std::sync::atomic::{AtomicUsize, Ordering};

use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::TcpListener;

/// The incarnation `decision` stamps.
const C: &str = "2026-11-01T00:00:00Z";

fn decision(shed: &str, verdict: &str, host: &str) -> EgressDecision {
    EgressDecision {
        ts: "2026-11-23T10:00:00Z".into(),
        shed: shed.into(),
        shed_created_at: "2026-11-01T00:00:00Z".into(),
        host: host.into(),
        port: 443,
        resolved_ip: "192.0.2.1".into(),
        protocol: "https".into(),
        verdict: verdict.into(),
        reason: String::new(),
    }
}

fn event(server: &str, shed: &str, verdict: &str, host: &str) -> EgressEvent {
    EgressEvent::from_decision(server, &decision(shed, verdict, host))
}

#[test]
fn a_shed_keeps_its_last_fifty_newest_first() {
    let mut log = EgressLog::default();
    for i in 0..(KEEP_PER_SHED + 10) {
        log.push(event(
            "mini",
            "widgets",
            "allow",
            &format!("h{i}.example.test"),
        ));
    }
    log.push(event("mini", "other", "deny", "x.example.test"));
    let recent = log.recent("mini", "widgets", C);
    assert_eq!(recent.len(), KEEP_PER_SHED);
    assert_eq!(
        recent[0].host,
        format!("h{}.example.test", KEEP_PER_SHED + 9)
    );
    assert_eq!(recent[KEEP_PER_SHED - 1].host, "h10.example.test");
    assert_eq!(log.recent("mini", "other", C).len(), 1);
    assert!(log.recent("mini", "nope", C).is_empty());
    assert!(log.recent("elsewhere", "widgets", C).is_empty());
}

#[test]
fn blocks_are_the_deny_decisions_in_order() {
    let events = vec![
        event("mini", "w", "deny", "a.example.test"),
        event("mini", "w", "allow", "b.example.test"),
        event("mini", "w", "deny", "c.example.test"),
    ];
    let b = blocks(&events);
    assert_eq!(
        b.iter().map(|e| e.host.as_str()).collect::<Vec<_>>(),
        ["a.example.test", "c.example.test"]
    );
    assert!(b.iter().all(EgressEvent::is_block));
    assert!(!events[1].is_block());
}

#[test]
fn each_incarnation_is_its_own_and_an_unstamped_decision_is_dropped() {
    let mut log = EgressLog::default();
    assert!(log.push(event("mini", "w", "deny", "old.example.test")));
    let mut newer = decision("w", "deny", "new.example.test");
    newer.shed_created_at = "2026-11-20T00:00:00Z".into();
    assert!(log.push(EgressEvent::from_decision("mini", &newer)));
    for unstamped in ["", "0001-01-01T00:00:00Z", "not a time"] {
        let mut d = decision("w", "deny", "older-server.example.test");
        d.shed_created_at = unstamped.into();
        assert!(
            !log.push(EgressEvent::from_decision("mini", &d)),
            "{unstamped:?}"
        );
    }
    let hosts = |v: Vec<EgressEvent>| v.into_iter().map(|e| e.host).collect::<Vec<_>>();
    assert_eq!(
        hosts(log.recent("mini", "w", "2026-11-20T00:00:00Z")),
        ["new.example.test"]
    );
    assert_eq!(hosts(log.recent("mini", "w", C)), ["old.example.test"]);
    // The same instant written another way is the same incarnation.
    assert_eq!(
        hosts(log.recent("mini", "w", "2026-10-31T19:00:00-05:00")),
        ["old.example.test"]
    );
    // A reader must name the incarnation.
    assert!(log.recent("mini", "w", "").is_empty());
    assert_eq!(log.all().len(), 2);
}

#[test]
fn retain_drops_incarnations_the_server_no_longer_lists_and_sheds_are_bounded() {
    let mut log = EgressLog::default();
    log.push(event("mini", "a", "deny", "x"));
    log.push(event("mini", "b", "deny", "x"));
    log.push(event("prod", "a", "deny", "x"));
    log.retain(
        "mini",
        &[Incarnation::new("b", C).unwrap()].into_iter().collect(),
    );
    assert!(log.recent("mini", "a", C).is_empty());
    assert_eq!(log.recent("mini", "b", C).len(), 1);
    assert_eq!(
        log.recent("prod", "a", C).len(),
        1,
        "another server is untouched"
    );

    let mut log = EgressLog::default();
    for i in 0..=MAX_SHEDS_PER_SERVER {
        log.push(event("mini", &format!("s{i}"), "deny", "x"));
    }
    assert!(
        log.recent("mini", "s0", C).is_empty(),
        "the least recent shed went"
    );
    assert_eq!(log.all().len(), MAX_SHEDS_PER_SERVER);
    // A decision with no shed is not kept.
    assert!(!log.push(event("mini", "", "deny", "x")));
    assert_eq!(log.all().len(), MAX_SHEDS_PER_SERVER);
}

#[test]
fn kept_text_is_cleaned_for_display() {
    let e = EgressEvent::from_decision(
        "mini",
        &decision("w", "deny", "evil\u{1b}[31m.example.test\n"),
    );
    assert!(!e.host.chars().any(char::is_control), "{:?}", e.host);
}

#[test]
fn the_wait_doubles_to_the_cap_resets_after_a_hold_and_waits_long_when_unavailable() {
    let o = EgressOptions::default();
    let short = Duration::from_millis(10);
    let (w, next) = next_wait(&o, o.base, short, false);
    assert_eq!((w, next), (o.base, o.base * 2));
    let (w, next) = next_wait(&o, Duration::from_secs(16), short, false);
    assert_eq!((w, next), (Duration::from_secs(16), o.max));
    let (w, next) = next_wait(&o, o.max, short, false);
    assert_eq!((w, next), (o.max, o.max));
    let (w, next) = next_wait(&o, o.max, o.hold, false);
    assert_eq!(
        (w, next),
        (o.base, o.base * 2),
        "a held stream resets the wait"
    );
    let (w, next) = next_wait(&o, Duration::from_secs(8), short, true);
    assert_eq!((w, next), (o.unavailable, o.base));
}

/// What a scripted connection answers.
#[derive(Clone)]
enum Answer {
    /// `200`, these frames, then the connection closes.
    Frames(Vec<String>),
    /// A status with an API error body.
    Status(u16),
}

/// A plain-HTTP loopback server: connection `n` gets `script[n]` (the last repeats).
async fn scripted(script: Vec<Answer>) -> (String, Arc<AtomicUsize>, tokio::task::JoinHandle<()>) {
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let url = format!("http://127.0.0.1:{}", listener.local_addr().unwrap().port());
    let conns = Arc::new(AtomicUsize::new(0));
    let seen = conns.clone();
    let task = tokio::spawn(async move {
        loop {
            let Ok((mut sock, _)) = listener.accept().await else {
                return;
            };
            let n = seen.fetch_add(1, Ordering::SeqCst);
            let answer = script[n.min(script.len() - 1)].clone();
            tokio::spawn(async move {
                let mut buf = Vec::new();
                let mut chunk = [0u8; 2048];
                while !buf.windows(4).any(|w| w == b"\r\n\r\n") {
                    match sock.read(&mut chunk).await {
                        Ok(0) | Err(_) => return,
                        Ok(n) => buf.extend_from_slice(&chunk[..n]),
                    }
                }
                let head = String::from_utf8_lossy(&buf);
                assert!(head.starts_with("GET /api/egress/stream "), "{head}");
                let reply = match answer {
                    Answer::Frames(frames) => {
                        let body: String = frames.concat();
                        format!(
                            "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{body}",
                            body.len()
                        )
                    }
                    Answer::Status(code) => {
                        let body = r#"{"error":{"code":"BACKEND_ERROR","message":"egress control is not enabled"}}"#;
                        format!(
                            "HTTP/1.1 {code} X\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{body}",
                            body.len()
                        )
                    }
                };
                let _ = sock.write_all(reply.as_bytes()).await;
                let _ = sock.shutdown().await;
            });
        }
    });
    (url, conns, task)
}

fn frame(d: &EgressDecision) -> String {
    format!(
        "event: egress\ndata: {}\n\n",
        serde_json::to_string(d).unwrap()
    )
}

fn fast() -> EgressOptions {
    EgressOptions {
        base: Duration::from_millis(10),
        max: Duration::from_millis(40),
        hold: Duration::from_secs(60),
        unavailable: Duration::from_secs(3600),
        idle: Duration::from_secs(5),
    }
}

async fn eventually(what: &str, mut f: impl FnMut() -> bool) {
    let deadline = Instant::now() + Duration::from_secs(10);
    while !f() {
        assert!(Instant::now() < deadline, "timed out waiting for {what}");
        tokio::time::sleep(Duration::from_millis(5)).await;
    }
}

/// A stream that ends is opened again; what each stream carried is kept, and the
/// change hook hears each server's decisions. Draining stops the consumer.
#[tokio::test]
async fn a_closed_stream_is_reopened_and_its_decisions_kept() {
    let (url, conns, _server) = scripted(vec![
        Answer::Frames(vec![
            frame(&decision("widgets", "deny", "a.example.test")),
            // Not a decision: skipped, the stream goes on.
            "event: egress\ndata: [1,2]\n\n".to_string(),
            frame(&decision("widgets", "allow", "b.example.test")),
            // Names no incarnation: counted, not kept, no change told.
            frame(&EgressDecision {
                shed_created_at: String::new(),
                ..decision("widgets", "deny", "unstamped.example.test")
            }),
        ]),
        Answer::Frames(vec![frame(&decision("widgets", "deny", "c.example.test"))]),
        Answer::Frames(Vec::new()),
    ])
    .await;
    let client = Client::new(url, "mini".into(), String::new(), None, None).unwrap();
    let heard = Arc::new(AtomicUsize::new(0));
    let hook: OnEgress = {
        let heard = heard.clone();
        Arc::new(move |server: &str| {
            assert_eq!(server, "mini");
            heard.fetch_add(1, Ordering::SeqCst);
        })
    };
    let feed = EgressFeed::start(
        &tokio::runtime::Handle::current(),
        vec![("mini".into(), client)],
        fast(),
        Some(hook),
    );
    eventually("three connections", || conns.load(Ordering::SeqCst) >= 3).await;
    let recent = feed.recent("mini", "widgets", C);
    assert_eq!(
        recent.iter().map(|e| e.host.as_str()).collect::<Vec<_>>(),
        ["c.example.test", "b.example.test", "a.example.test"]
    );
    assert_eq!(blocks(&recent).len(), 2);
    assert_eq!(heard.load(Ordering::SeqCst), 3);
    let st = feed.status();
    assert_eq!(st.len(), 1);
    assert!(st[0].connects >= 2, "{st:?}");
    assert_eq!((st[0].decisions, st[0].unattributed), (4, 1));
    assert!(feed.join(Duration::from_secs(5)).await);
    assert_eq!(feed.status()[0].state, "stopped");
}

/// A server without egress control (`501`, or `404` from an older one) is not asked
/// again until the long wait has passed, and its status says so.
#[tokio::test]
async fn an_unavailable_server_waits_the_long_wait() {
    for code in [501, 404] {
        let (url, conns, _server) = scripted(vec![Answer::Status(code)]).await;
        let client = Client::new(url, "mini".into(), String::new(), None, None).unwrap();
        let feed = EgressFeed::start(
            &tokio::runtime::Handle::current(),
            vec![("mini".into(), client)],
            fast(),
            None,
        );
        eventually("the unavailable state", || {
            feed.status()[0].state == "unavailable"
        })
        .await;
        let st = &feed.status()[0];
        assert!(
            st.error
                .as_deref()
                .is_some_and(|e| e.contains("not enabled")),
            "{st:?}"
        );
        tokio::time::sleep(Duration::from_millis(100)).await;
        assert_eq!(conns.load(Ordering::SeqCst), 1, "{code}: asked once");
        assert!(feed.join(Duration::from_secs(5)).await);
    }
}

/// Any other failure backs off and tries again.
#[tokio::test]
async fn a_refused_stream_is_tried_again() {
    let (url, conns, _server) = scripted(vec![Answer::Status(500)]).await;
    let client = Client::new(url, "mini".into(), String::new(), None, None).unwrap();
    let feed = EgressFeed::start(
        &tokio::runtime::Handle::current(),
        vec![("mini".into(), client)],
        fast(),
        None,
    );
    eventually("three attempts", || conns.load(Ordering::SeqCst) >= 3).await;
    assert_eq!(feed.status()[0].connects, 0);
    feed.drain();
    assert!(feed.join(Duration::from_secs(5)).await);
}

/// A listing drops the decisions of the incarnations it no longer has, on the servers
/// it answered for (a server whose listing failed keeps its own); a shed deleted and
/// created again under its name between two listings starts empty, and the old
/// incarnation's decisions are not read under the new one.
#[tokio::test]
async fn a_listing_drops_deleted_and_replaced_incarnations() {
    let closed = |name: &str| {
        (
            name.to_string(),
            Client::new(
                "http://127.0.0.1:1".into(),
                name.into(),
                String::new(),
                None,
                None,
            )
            .unwrap(),
        )
    };
    let feed = EgressFeed::start(
        &tokio::runtime::Handle::current(),
        vec![closed("mini"), closed("prod")],
        fast(),
        None,
    );
    const REUSED: &str = "2026-11-22T08:30:00.5Z";
    {
        let mut log = feed.shared.log.lock().unwrap();
        for (server, shed) in [
            ("mini", "kept"),
            ("mini", "gone"),
            ("mini", "web"),
            ("prod", "a"),
        ] {
            log.push(event(server, shed, "deny", "alice.example.test"));
        }
    }
    let shed = |name: &str, created: Option<&str>| -> shed_core::models::Shed {
        serde_json::from_value(serde_json::json!({
            "host": "mini", "name": name, "created_at": created,
        }))
        .unwrap()
    };
    let listing = |sheds| shed_app::Reachability {
        sheds,
        last_error: None,
        host_errors: vec![shed_app::HostFailure::from_error(
            "prod",
            &shed_core::http::ShedError::Transport("refused".into()),
        )],
        delivery: HashMap::new(),
    };
    // `web` was deleted and created again (by someone else) between two listings.
    feed.observe(&listing(vec![
        shed("kept", Some(C)),
        shed("web", Some(REUSED)),
        shed("nameless", None),
    ]));
    assert_eq!(feed.recent("mini", "kept", C).len(), 1);
    assert!(feed.recent("mini", "gone", C).is_empty());
    assert!(
        feed.recent("mini", "web", C).is_empty(),
        "the old incarnation went"
    );
    assert!(
        feed.recent("mini", "web", REUSED).is_empty(),
        "the new one starts empty"
    );
    assert_eq!(
        feed.recent("prod", "a", C).len(),
        1,
        "a failed listing keeps its sheds"
    );
    // The new incarnation's own decisions are its alone.
    let mut d = decision("web", "deny", "bob.example.test");
    d.shed_created_at = REUSED.into();
    feed.shared
        .log
        .lock()
        .unwrap()
        .push(EgressEvent::from_decision("mini", &d));
    assert_eq!(
        feed.recent("mini", "web", REUSED)[0].host,
        "bob.example.test"
    );
    assert!(feed.recent("mini", "web", C).is_empty());
    assert!(feed.join(Duration::from_secs(5)).await);
}
