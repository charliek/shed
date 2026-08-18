//! HTTP-level mirrors of the Go hub suites over the REAL axum shell — the
//! `httptest.NewServer(h.handler())` idiom: `hub_test.go`'s loopback-endpoint
//! half, `hub_input_test.go`'s /messages + /input families,
//! `hub_verbs_test.go` (routing matrix + the live opencode lane), and
//! `hub_ingest_test.go`. Everything runs against `serve()`'s real accept loop
//! on an ephemeral loopback port, so the mux's own 404/405 behavior and the
//! per-connection posture are exercised alongside the handlers.

use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::Arc;
use std::time::Duration;

use reqwest::Method;
use shed_core::rc::{RcActivity, RcKind};

use super::hub::{
    apply_hub_env_overrides, bind_hub_listener, probe_hub_identity, query_hub_health, Hub,
    HubConfig, HubSessionsResponse, ENV_HUB_ACTIVE_MS, ENV_HUB_ADDR,
};
use super::hub_test_support::{
    codex_ready_pane, do_request, http_client, hub_config, managed_env, new_test_hub,
    opencode_ready_pane, pane_fixture, raw_post, serve_hub, wait_for, want_envelope, HubClock,
    HubTmux, CURSOR_SID,
};
use super::messages::{FeedMessage, MAX_RING_MESSAGES};

fn text_msg(text: &str) -> FeedMessage {
    FeedMessage {
        role: "assistant".into(),
        typ: "text".into(),
        text: text.into(),
        ..FeedMessage::default()
    }
}

/// A reconciled hub + served router (`newInputHub` reconciles TWICE across the
/// quiet period so the stability verdict has SETTLED — a first-tick stability
/// is always `working`, which would 409 every post).
async fn new_input_hub(f: &Arc<HubTmux>, clk: &Arc<HubClock>) -> (Arc<Hub>, String) {
    let h = Arc::new(new_test_hub(f, clk));
    h.reconcile();
    clk.advance(Duration::from_secs(5)); // past the 4s test quiet period
    h.reconcile();
    let (url, _handle) = serve_hub(&h).await;
    (h, url)
}

async fn get_status(url: &str) -> u16 {
    http_client()
        .get(url)
        .send()
        .await
        .expect("get")
        .status()
        .as_u16()
}

// ---- loopback HTTP endpoints (hub_test.go:707-874) ----

// Mirrors TestHubHTTPSessions.
#[tokio::test(flavor = "multi_thread")]
async fn http_sessions() {
    let f = HubTmux::new();
    let clk = HubClock::new();
    let h = Arc::new(new_test_hub(&f, &clk));
    f.set(
        "rc-hhh888",
        "> Find and fix a bug in @filename",
        &managed_env("id-8", &RcKind::Codex),
    );
    h.reconcile();
    let (url, _s) = serve_hub(&h).await;

    let resp = http_client()
        .get(format!("{url}/v1/sessions"))
        .send()
        .await
        .unwrap();
    assert_eq!(resp.status().as_u16(), 200);
    let body: HubSessionsResponse = resp.json().await.unwrap();
    assert_eq!(body.sessions.len(), 1);
    assert_eq!(body.sessions[0].slug, "hhh888");
    assert_eq!(
        body.sessions[0].activity,
        Some(RcActivity::Working),
        "want working overlaid"
    );
}

// Mirrors TestHubHTTPRoutesAndMethods.
#[tokio::test(flavor = "multi_thread")]
async fn http_routes_and_methods() {
    let f = HubTmux::new();
    let clk = HubClock::new();
    let h = Arc::new(new_test_hub(&f, &clk));
    let (url, _s) = serve_hub(&h).await;
    let client = http_client();

    for (method, path, body, want) in [
        (Method::GET, "/v1/sessions/x/messages", "", 404), // unknown slug
        (
            Method::POST,
            "/v1/sessions/x/input",
            r#"{"text":"hi"}"#,
            404,
        ),
        (Method::POST, "/v1/sessions", "", 405), // sessions is GET-only
        (Method::GET, "/v1/nope", "", 404),      // unknown path
    ] {
        let resp = do_request(&client, method.clone(), &format!("{url}{path}"), body).await;
        assert_eq!(resp.status().as_u16(), want, "{method} {path}");
    }
}

// Mirrors TestHubHTTPEventsStreamAndHeartbeat: events are delivered and a
// heartbeat comment arrives on the interval; the `: ok` opener and
// Content-Type are pinned.
#[tokio::test(flavor = "multi_thread")]
async fn http_events_stream_and_heartbeat() {
    let f = HubTmux::new();
    let clk = HubClock::new();
    let h = Arc::new(new_test_hub(&f, &clk)); // heartbeat = 20ms
    let (url, _s) = serve_hub(&h).await;

    let resp = http_client()
        .get(format!("{url}/v1/events"))
        .send()
        .await
        .unwrap();
    assert_eq!(
        resp.headers()
            .get("Content-Type")
            .and_then(|v| v.to_str().ok()),
        Some("text/event-stream")
    );

    // Wait until the server registered the subscriber, then broadcast.
    wait_for("subscriber registered", || h.subscriber_count() == 1).await;
    h.broadcast(&super::events::activity_changed_event(
        "s1",
        RcActivity::Working,
        "2026-01-01T00:00:00Z",
        shed_core::rc::RcState::Ready,
        "",
    ));

    let (mut saw_ok, mut saw_event, mut saw_heartbeat) = (false, false, false);
    let mut stream = resp.bytes_stream();
    let deadline = tokio::time::Instant::now() + Duration::from_secs(3);
    use futures_util::StreamExt;
    while tokio::time::Instant::now() < deadline && (!saw_event || !saw_heartbeat || !saw_ok) {
        let chunk = match tokio::time::timeout_at(deadline, stream.next()).await {
            Ok(Some(Ok(c))) => c,
            _ => break,
        };
        let s = String::from_utf8_lossy(&chunk).to_string();
        saw_ok |= s.contains(": ok");
        saw_event |= s.contains("event: activity.changed");
        saw_heartbeat |= s.contains(": heartbeat");
    }
    assert!(
        saw_ok,
        "the stream must open with the literal `: ok` comment"
    );
    assert!(saw_event, "did not receive the broadcast activity.changed");
    assert!(saw_heartbeat, "did not receive a heartbeat comment");
}

// Mirrors TestHubEventsWedgedClientUnsubscribes: a connected-but-never-reading
// client must not park the events handler forever — the frame handoff misses
// the write deadline and the subscriber is removed.
#[tokio::test(flavor = "multi_thread")]
async fn events_wedged_client_unsubscribes() {
    let f = HubTmux::new();
    let clk = HubClock::new();
    let h = Arc::new(Hub::new(HubConfig {
        heartbeat: Duration::from_secs(3600), // heartbeats out of the picture
        write_timeout: Duration::from_millis(100), // fast deadline
        ..hub_config(&f, &clk)
    }));
    let (url, _s) = serve_hub(&h).await;
    let addr = url.strip_prefix("http://").unwrap().to_string();

    // Raw TCP client: send the request, then never read the response.
    use tokio::io::AsyncWriteExt;
    let mut conn = tokio::net::TcpStream::connect(&addr).await.unwrap();
    conn.write_all(b"GET /v1/events HTTP/1.1\r\nHost: hub\r\n\r\n")
        .await
        .unwrap();
    wait_for("subscriber registered", || h.subscriber_count() == 1).await;

    // Push large frames until the buffers fill, the handoff blocks, and the
    // deadline trips. Broadcast never blocks us (drop-on-full).
    let stop = Arc::new(AtomicBool::new(false));
    let bh = Arc::clone(&h);
    let bstop = Arc::clone(&stop);
    let pusher = std::thread::spawn(move || {
        let big = "x".repeat(64 * 1024);
        while !bstop.load(Ordering::Relaxed) {
            bh.broadcast(&super::events::HubEvent {
                name: "bulk",
                data: big.clone(),
            });
        }
    });
    wait_for("wedged subscriber removed", || h.subscriber_count() == 0).await;
    stop.store(true, Ordering::Relaxed);
    pusher.join().unwrap();
    drop(conn);
}

// ---- bind-as-lock (`bindHubListener`) ----

// Mirrors TestBindHubListenerReportsAlreadyInUse + FreePortSucceeds.
#[test]
fn bind_hub_listener_reports_already_in_use() {
    let (l, already) = bind_hub_listener("127.0.0.1:0").expect("free bind");
    let l = l.expect("listener");
    assert!(!already);
    let addr = l.local_addr().unwrap().to_string();

    let (l2, already2) = bind_hub_listener(&addr).expect("in-use is not an error");
    assert!(already2, "EADDRINUSE must report already=true");
    assert!(l2.is_none());
    drop(l);
    let (l3, already3) = bind_hub_listener(&addr).expect("rebind");
    assert!(!already3 && l3.is_some(), "a freed port binds again");
}

// ---- GET /v1/sessions/{slug}/messages ----

// Mirrors TestHubHTTPMessagesPagingTruncatedAnd404.
#[tokio::test(flavor = "multi_thread")]
async fn http_messages_paging_truncated_and_404() {
    let f = HubTmux::new();
    let clk = HubClock::new();
    let h = Arc::new(new_test_hub(&f, &clk));
    f.set(
        "rc-msg111",
        &codex_ready_pane(),
        &managed_env("id-m", &RcKind::Codex),
    );
    h.reconcile();
    let ring = {
        let ts = h.lock_track();
        Arc::clone(&ts.tracked.get("msg111").unwrap().ring)
    };
    for _ in 0..5 {
        ring.append(text_msg("m"), clk.now());
    }
    let (url, _s) = serve_hub(&h).await;
    let client = http_client();

    // since=2 (exclusive) + limit=2 → seqs 3,4.
    let body: super::messages::HubMessagesResponse = client
        .get(format!("{url}/v1/sessions/msg111/messages?since=2&limit=2"))
        .send()
        .await
        .unwrap()
        .json()
        .await
        .unwrap();
    let seqs: Vec<u64> = body.messages.iter().map(|m| m.seq).collect();
    assert_eq!(seqs, vec![3, 4]);
    assert!(!body.truncated, "in-ring since must not be truncated");

    // Drop the head, then a fresh since=0 reports truncated.
    for _ in 0..MAX_RING_MESSAGES + 10 {
        ring.append(text_msg("m"), clk.now());
    }
    let body2: super::messages::HubMessagesResponse = client
        .get(format!("{url}/v1/sessions/msg111/messages"))
        .send()
        .await
        .unwrap()
        .json()
        .await
        .unwrap();
    assert!(
        body2.truncated,
        "since=0 after drop-oldest must report truncated"
    );

    assert_eq!(
        get_status(&format!("{url}/v1/sessions/nope/messages")).await,
        404
    );
    assert_eq!(
        get_status(&format!("{url}/v1/sessions/msg111/messages?since=abc")).await,
        400
    );
    assert_eq!(
        get_status(&format!("{url}/v1/sessions/msg111/messages?limit=-1")).await,
        400
    );
    // F5: Go reads `since` with ParseUint, which rejects a SIGN, while
    // u64::from_str accepts a leading '+' — the digits-only guard keeps them
    // agreeing. (`limit` is read with Atoi on both sides, which does accept
    // one, so `limit=%2B5` stays a 200.)
    assert_eq!(
        get_status(&format!("{url}/v1/sessions/msg111/messages?since=%2B5")).await,
        400
    );
    assert_eq!(
        get_status(&format!("{url}/v1/sessions/msg111/messages?limit=%2B5")).await,
        200
    );
}

// Mirrors TestHubHTTPMessagesEmptyForKnownSlug: 200 with an empty (non-null)
// array — the resolved empty-page wire pin (Go's handler coerces its nil,
// `hub.go:440-442`).
#[tokio::test(flavor = "multi_thread")]
async fn http_messages_empty_for_known_slug() {
    let f = HubTmux::new();
    let clk = HubClock::new();
    let h = Arc::new(new_test_hub(&f, &clk));
    f.set(
        "rc-empty1",
        &codex_ready_pane(),
        &managed_env("id-e", &RcKind::Codex),
    );
    h.reconcile();
    let (url, _s) = serve_hub(&h).await;

    let resp = http_client()
        .get(format!("{url}/v1/sessions/empty1/messages"))
        .send()
        .await
        .unwrap();
    assert_eq!(resp.status().as_u16(), 200);
    let raw = resp.text().await.unwrap();
    assert!(
        raw.contains(r#""messages":[]"#),
        "empty page must encode [] not null: {raw}"
    );
}

// ---- POST /v1/sessions/{slug}/input ----

// Mirrors TestHubInputHappyPathReachesPane.
#[tokio::test(flavor = "multi_thread")]
async fn input_happy_path_reaches_pane() {
    let f = HubTmux::new();
    let clk = HubClock::new();
    f.set(
        "rc-inp111",
        &codex_ready_pane(),
        &managed_env("id-i", &RcKind::Codex),
    );
    let (_h, url) = new_input_hub(&f, &clk).await;

    let resp = do_request(
        &http_client(),
        Method::POST,
        &format!("{url}/v1/sessions/inp111/input"),
        r#"{"text":"hello there"}"#,
    )
    .await;
    assert_eq!(
        resp.status().as_u16(),
        200,
        "{}",
        resp.text().await.unwrap()
    );
    assert_eq!(f.recorded(), vec!["hello there".to_string()]);
}

// Mirrors TestHubInputMultilineUsesBracketedPaste.
#[tokio::test(flavor = "multi_thread")]
async fn input_multiline_uses_bracketed_paste() {
    let f = HubTmux::new();
    let clk = HubClock::new();
    f.set(
        "rc-inp222",
        &codex_ready_pane(),
        &managed_env("id-i2", &RcKind::Codex),
    );
    let (_h, url) = new_input_hub(&f, &clk).await;

    let resp = do_request(
        &http_client(),
        Method::POST,
        &format!("{url}/v1/sessions/inp222/input"),
        r#"{"text":"line one\nline two"}"#,
    )
    .await;
    assert_eq!(resp.status().as_u16(), 200);
    assert_eq!(f.recorded(), vec!["line one\nline two".to_string()]);
}

// Mirrors TestHubInputDegradedIdleAnchorAccepts: no watcher correlated
// (getenv answers "") — the only acceptance signal is the composer anchor.
#[tokio::test(flavor = "multi_thread")]
async fn input_degraded_idle_anchor_accepts() {
    let f = HubTmux::new();
    let clk = HubClock::new();
    f.set(
        "rc-deg111",
        &codex_ready_pane(),
        &managed_env("id-d", &RcKind::Codex),
    );
    let (_h, url) = new_input_hub(&f, &clk).await;

    let resp = do_request(
        &http_client(),
        Method::POST,
        &format!("{url}/v1/sessions/deg111/input"),
        r#"{"text":"go"}"#,
    )
    .await;
    assert_eq!(
        resp.status().as_u16(),
        200,
        "degraded idle+anchor must accept"
    );
}

// Mirrors TestHubInputErrorStatuses.
#[tokio::test(flavor = "multi_thread")]
async fn input_error_statuses() {
    let f = HubTmux::new();
    let clk = HubClock::new();
    f.set(
        "rc-err111",
        &codex_ready_pane(),
        &managed_env("id-e", &RcKind::Codex),
    );
    let (_h, url) = new_input_hub(&f, &clk).await;
    let base = format!("{url}/v1/sessions/err111/input");
    let client = http_client();

    let oversized = format!(r#"{{"text":"{}"}}"#, "x".repeat(17 * 1024));
    let unsafe_text = "{\"text\":\"a\\u001bb\"}".to_string();
    for (name, url, body, want) in [
        (
            "invalid json",
            base.clone(),
            r#"{not json"#.to_string(),
            400,
        ),
        (
            "empty text",
            base.clone(),
            r#"{"text":"   "}"#.to_string(),
            400,
        ),
        ("unsafe control char", base.clone(), unsafe_text, 400),
        (
            "unknown slug",
            format!("{url}/v1/sessions/ghost/input"),
            r#"{"text":"hi"}"#.to_string(),
            404,
        ),
        ("too large", base.clone(), oversized, 413),
    ] {
        let resp = do_request(&client, Method::POST, &url, &body).await;
        assert_eq!(resp.status().as_u16(), want, "{name}");
    }
}

// The wire-differential's non-object-body cells (F1): Go unmarshals the body
// into a STRUCT, so any non-object JSON value is 400 invalid_json on every
// POST route. serde's DERIVED Deserialize also accepts the TUPLE form, so
// without the explicit object gate `["hi"]` would decode as text="hi" and
// DELIVER where Go 400s.
#[tokio::test(flavor = "multi_thread")]
async fn body_must_be_a_json_object() {
    let f = HubTmux::new();
    let clk = HubClock::new();
    f.set(
        "rc-obj111",
        &codex_ready_pane(),
        &managed_env("id-o", &RcKind::Codex),
    );
    let (_h, url) = new_input_hub(&f, &clk).await;
    let client = http_client();

    for path in [
        "/v1/sessions/obj111/input",
        "/v1/sessions/obj111/turn",
        "/v1/sessions/obj111/approvals/call_01",
    ] {
        for body in [
            r#"["hi"]"#,
            "[]",
            r#"["hi",{"a":1}]"#,
            r#""hi""#,
            "42",
            "true",
        ] {
            let resp = do_request(&client, Method::POST, &format!("{url}{path}"), body).await;
            want_envelope_named(&format!("{path} {body}"), resp, 400, "invalid_json").await;
        }
    }
    assert!(
        f.recorded().is_empty(),
        "a non-object body must never reach the pane: {:?}",
        f.recorded()
    );
}

// The wire-differential's null-string cells (F2): Go's `json.Unmarshal(null)`
// into a string field is a NO-OP leaving the zero value, so the request falls
// to the FIELD's own validation error — not to invalid_json, which is what a
// bare serde `String` would produce.
#[tokio::test(flavor = "multi_thread")]
async fn null_string_fields_are_a_go_no_op() {
    let f = HubTmux::new();
    let clk = HubClock::new();
    f.set(
        "rc-nul111",
        &codex_ready_pane(),
        &managed_env("id-n", &RcKind::Codex),
    );
    let (_h, url) = new_input_hub(&f, &clk).await;
    let client = http_client();

    for (path, body, code) in [
        (
            "/v1/sessions/nul111/input",
            r#"{"text":null}"#,
            "empty_text",
        ),
        ("/v1/sessions/nul111/turn", r#"{"text":null}"#, "empty_text"),
        (
            "/v1/sessions/nul111/approvals/call_01",
            r#"{"decision":null}"#,
            "invalid_decision",
        ),
    ] {
        let resp = do_request(&client, Method::POST, &format!("{url}{path}"), body).await;
        want_envelope_named(path, resp, 400, code).await;
    }
    assert!(f.recorded().is_empty(), "{:?}", f.recorded());
}

// The wire-differential's raw-invalid-UTF-8 cell (F3): Go's decoder
// substitutes U+FFFD per bad byte and the request PROCEEDS, so the pane
// receives the replacement character. serde rejects the slice outright, hence
// decode_json_capped's lossy retry.
#[tokio::test(flavor = "multi_thread")]
async fn invalid_utf8_in_a_string_is_replaced_not_rejected() {
    let f = HubTmux::new();
    let clk = HubClock::new();
    f.set(
        "rc-utf111",
        &codex_ready_pane(),
        &managed_env("id-u", &RcKind::Codex),
    );
    let (_h, url) = new_input_hub(&f, &clk).await;

    let mut body = br#"{"text":"a"#.to_vec();
    body.push(0xff); // a lone continuation byte — not valid UTF-8
    body.extend_from_slice(br#"b"}"#);
    let resp = http_client()
        .post(format!("{url}/v1/sessions/utf111/input"))
        .header("Content-Type", "application/json")
        .body(body)
        .send()
        .await
        .expect("post");
    assert_eq!(
        resp.status().as_u16(),
        200,
        "{}",
        resp.text().await.unwrap_or_default()
    );
    assert_eq!(f.recorded(), vec!["a\u{fffd}b".to_string()]);
}

// Mirrors TestHubInputNotAcceptingIs409 (a churning, non-anchor pane).
#[tokio::test(flavor = "multi_thread")]
async fn input_not_accepting_is_409() {
    let f = HubTmux::new();
    let clk = HubClock::new();
    f.set(
        "rc-na111",
        "boot >_ OpenAI Codex (v1.0)\nworking...",
        &managed_env("id-na", &RcKind::Codex),
    );
    let (_h, url) = new_input_hub(&f, &clk).await;
    let resp = do_request(
        &http_client(),
        Method::POST,
        &format!("{url}/v1/sessions/na111/input"),
        r#"{"text":"hi"}"#,
    )
    .await;
    assert_eq!(resp.status().as_u16(), 409);
}

// Mirrors TestHubInputRaceStateFlipIs409: tracked at the anchor, but the
// fresh capture under the input mutex sees a churning pane.
#[tokio::test(flavor = "multi_thread")]
async fn input_race_state_flip_is_409() {
    let f = HubTmux::new();
    let clk = HubClock::new();
    f.set(
        "rc-race22",
        &codex_ready_pane(),
        &managed_env("id-r", &RcKind::Codex),
    );
    let (_h, url) = new_input_hub(&f, &clk).await;

    f.set_pane("rc-race22", "boot >_ OpenAI Codex (v1.0)\nnow working");
    let resp = do_request(
        &http_client(),
        Method::POST,
        &format!("{url}/v1/sessions/race22/input"),
        r#"{"text":"hi"}"#,
    )
    .await;
    assert_eq!(resp.status().as_u16(), 409);
}

// Mirrors TestHubInputIdentityGuardIs409: recreated (new SHED_RC_ID) without
// a reconcile — the locked re-check must reject.
#[tokio::test(flavor = "multi_thread")]
async fn input_identity_guard_is_409() {
    let f = HubTmux::new();
    let clk = HubClock::new();
    f.set(
        "rc-idg111",
        &codex_ready_pane(),
        &managed_env("id-old", &RcKind::Codex),
    );
    let (_h, url) = new_input_hub(&f, &clk).await;

    f.set(
        "rc-idg111",
        &codex_ready_pane(),
        &managed_env("id-new", &RcKind::Codex),
    );
    let resp = do_request(
        &http_client(),
        Method::POST,
        &format!("{url}/v1/sessions/idg111/input"),
        r#"{"text":"hi"}"#,
    )
    .await;
    assert_eq!(resp.status().as_u16(), 409);
}

// Mirrors TestHubInputOpencodeNotGatedAfterTurnFlip — the /input BEHAVIOR
// BREAK, pinned: opencode's row moved to input "turn", so /input 409s on a
// pane that DOES match its composer anchor, finally, regardless of activity.
#[tokio::test(flavor = "multi_thread")]
async fn input_opencode_not_gated_after_turn_flip() {
    let f = HubTmux::new();
    let clk = HubClock::new();
    f.set(
        "rc-ng111",
        &opencode_ready_pane(),
        &managed_env("id-ng", &RcKind::Opencode),
    );
    let (_h, url) = new_input_hub(&f, &clk).await;

    let resp = do_request(
        &http_client(),
        Method::POST,
        &format!("{url}/v1/sessions/ng111/input"),
        r#"{"text":"hi"}"#,
    )
    .await;
    assert_eq!(resp.status().as_u16(), 409);
    let body = resp.text().await.unwrap();
    assert!(
        body.contains("does not accept feed input"),
        "want the kind-gate message, got {body}"
    );
}

// Mirrors TestHubInputTransientCaptureErrorIs500: a transient tmux hiccup at
// the locked re-check is a 500 (retryable), not a 404.
#[tokio::test(flavor = "multi_thread")]
async fn input_transient_capture_error_is_500() {
    let f = HubTmux::new();
    let clk = HubClock::new();
    f.set(
        "rc-tra111",
        &codex_ready_pane(),
        &managed_env("id-t", &RcKind::Codex),
    );
    let (_h, url) = new_input_hub(&f, &clk).await;

    f.set_flaky("rc-tra111", true);
    let resp = do_request(
        &http_client(),
        Method::POST,
        &format!("{url}/v1/sessions/tra111/input"),
        r#"{"text":"hi"}"#,
    )
    .await;
    assert_eq!(resp.status().as_u16(), 500);
}

// Mirrors TestHubInputLockSurvivesEntryReplacement: input serialization is
// keyed by SLUG on the hub — a tracked-entry replacement yields the SAME
// mutex, and a held mutex blocks a live POST across the replacement.
#[tokio::test(flavor = "multi_thread")]
async fn input_lock_survives_entry_replacement() {
    let f = HubTmux::new();
    let clk = HubClock::new();
    let (h, url) = new_input_hub(&f, &clk).await;
    f.set(
        "rc-lok111",
        &codex_ready_pane(),
        &managed_env("id-a", &RcKind::Codex),
    );
    h.reconcile();

    let mu = h.input_lock("lok111");
    f.set(
        "rc-lok111",
        &codex_ready_pane(),
        &managed_env("id-b", &RcKind::Codex),
    );
    h.reconcile();
    assert!(
        Arc::ptr_eq(&mu, &h.input_lock("lok111")),
        "entry replacement must not mint a new input mutex for the slug"
    );

    // Hold the mutex on a helper thread; the live POST must block until it is
    // released (the handler resolves the lock by slug, not via the entry).
    let held = Arc::new(std::sync::Barrier::new(2));
    let release = Arc::new(AtomicBool::new(false));
    let (tmu, theld, trel) = (Arc::clone(&mu), Arc::clone(&held), Arc::clone(&release));
    let holder = std::thread::spawn(move || {
        let _g = tmu.lock().unwrap();
        theld.wait();
        while !trel.load(Ordering::Relaxed) {
            std::thread::sleep(Duration::from_millis(5));
        }
    });
    held.wait();

    let post_url = format!("{url}/v1/sessions/lok111/input");
    let post = tokio::spawn(async move {
        do_request(&http_client(), Method::POST, &post_url, r#"{"text":"hi"}"#)
            .await
            .status()
            .as_u16()
    });
    tokio::time::sleep(Duration::from_millis(300)).await;
    assert!(
        !post.is_finished(),
        "the POST must block on the held slug mutex"
    );
    release.store(true, Ordering::Relaxed);
    holder.join().unwrap();
    // Completion is the contract (Go asserts only that it finishes — the
    // fresh session's first-tick stability legitimately answers 409 here).
    tokio::time::timeout(Duration::from_secs(2), post)
        .await
        .expect("POST did not complete after the mutex was released")
        .unwrap();

    // Disappear → the lock is pruned; a later recreate gets a fresh mutex.
    f.remove("rc-lok111");
    h.reconcile();
    assert!(
        !Arc::ptr_eq(&mu, &h.input_lock("lok111")),
        "disappeared slug's input lock must be pruned"
    );
}

// ---- the sanctioned env seams (§2.5 — clirc.applyHubEnvOverrides) ----

#[test]
fn env_overrides_parse_and_reject() {
    let mk = || hub_config(&HubTmux::new(), &HubClock::new());
    let mut notes: Vec<String> = Vec::new();

    // A valid loopback addr + interval override applies.
    let mut cfg = mk();
    let env = |k: &str| -> String {
        match k {
            "SHED_RC_HUB_ADDR" => "127.0.0.1:18443".into(),
            "SHED_RC_HUB_ACTIVE_MS" => "250".into(),
            _ => String::new(),
        }
    };
    apply_hub_env_overrides(&mut cfg, &env, &mut |n| notes.push(n.to_string()));
    assert_eq!(cfg.addr, "127.0.0.1:18443");
    assert_eq!(cfg.active_interval, Duration::from_millis(250));
    assert!(notes.is_empty(), "{notes:?}");

    // Loopback enforcement + concrete-port rule + ms validation: every
    // rejected value is ignored WITH a note and the config left untouched.
    for (k, v) in [
        (ENV_HUB_ADDR, "0.0.0.0:1029"),
        (ENV_HUB_ADDR, "192.168.1.4:1029"),
        (ENV_HUB_ADDR, "127.0.0.1:0"),
        (ENV_HUB_ADDR, "127.0.0.1:65536"),
        (ENV_HUB_ADDR, "127.0.0.1"),
        (ENV_HUB_ACTIVE_MS, "0"),
        (ENV_HUB_ACTIVE_MS, "-5"),
        (ENV_HUB_ACTIVE_MS, "abc"),
        (ENV_HUB_ACTIVE_MS, "9223372036854775807"), // overflows the multiply
    ] {
        let mut cfg = mk();
        let before_addr = cfg.addr.clone();
        let before_active = cfg.active_interval;
        let mut noted = false;
        let key = k.to_string();
        let val = v.to_string();
        let env = move |q: &str| -> String {
            if q == key {
                val.clone()
            } else {
                String::new()
            }
        };
        apply_hub_env_overrides(&mut cfg, &env, &mut |_| noted = true);
        assert!(noted, "{k}={v} must be noted");
        assert_eq!(cfg.addr, before_addr, "{k}={v} must not apply");
        assert_eq!(cfg.active_interval, before_active, "{k}={v} must not apply");
    }
}

// ---- contract-v2 verbs (hub_verbs_test.go) ----

use super::messages::{
    FeedApproval, APPROVAL_DECISION_ALLOW, APPROVAL_DECISION_ALLOW_ALWAYS, APPROVAL_DECISION_DENY,
    APPROVAL_STATUS_PENDING, APPROVAL_STATUS_RESOLVED,
};
use super::verbs::{
    ApprovalResponse, InterruptResponse, TurnResponse, APPROVAL_ID_RE, ERR_ALREADY_RESOLVED,
    ERR_NOT_ACCEPTING, ERR_NOT_SUPPORTED, ERR_UNKNOWN_APPROVAL,
};
use super::watch_opencode_transport::fake::FakeOpencode;

const VERB_SLUG_CODEX: &str = "vrb001";
const VERB_SLUG_SHELL: &str = "vrb002";
const VERB_SLUG_UNKNOWN: &str = "vrb003";
const VERB_SLUG_CLAUDE: &str = "vrb004";
const VERB_SLUG_CURSOR: &str = "vrb005";
const VERB_SLUG_OPENCODE: &str = "vrb010";

/// `newVerbHub` (`hub_verbs_test.go:38`): a reconciled hub with one session
/// per capability shape a verb can meet.
async fn new_verb_hub() -> (Arc<Hub>, String) {
    let f = HubTmux::new();
    let clk = HubClock::new();
    let h = Arc::new(new_test_hub(&f, &clk));
    f.set(
        &format!("rc-{VERB_SLUG_CODEX}"),
        &codex_ready_pane(),
        &managed_env("id-vc", &RcKind::Codex),
    );
    f.set(
        &format!("rc-{VERB_SLUG_SHELL}"),
        "$ ",
        &managed_env("id-vs", &RcKind::Shell),
    );
    f.set(
        &format!("rc-{VERB_SLUG_UNKNOWN}"),
        "whatever",
        &managed_env("id-vu", &RcKind::Other("some-future-kind".into())),
    );
    f.set(
        &format!("rc-{VERB_SLUG_CLAUDE}"),
        "claude\n> ",
        &managed_env("id-vcl", &RcKind::ClaudeRc),
    );
    f.set(
        &format!("rc-{VERB_SLUG_CURSOR}"),
        "cursor\n> ",
        &managed_env("id-vcu", &RcKind::Cursor),
    );
    h.reconcile();
    let (url, _s) = serve_hub(&h).await;
    (h, url)
}

/// `verbCases` (`hub_verbs_test.go:82`) — one valid request per verb.
const VERB_CASES: [(&str, &str, &str); 3] = [
    ("turn", "/turn", r#"{"text":"do the thing"}"#),
    ("interrupt", "/interrupt", ""),
    ("approvals", "/approvals/per_1", r#"{"decision":"allow"}"#),
];

/// `wantAllVerbs` — every verb against one session answers the SAME envelope.
async fn want_all_verbs(base: &str, status: u16, code: &str) {
    let client = http_client();
    for (name, path, body) in VERB_CASES {
        let resp = do_request(&client, Method::POST, &format!("{base}{path}"), body).await;
        want_envelope_named(name, resp, status, code).await;
    }
}

/// [`want_envelope`] with the case name in the panic message — the table-driven
/// callers' only addition over the shared assertion.
async fn want_envelope_named(name: &str, resp: reqwest::Response, status: u16, code: &str) {
    let got = resp.status().as_u16();
    if got != status {
        let text = resp.text().await.unwrap_or_default();
        panic!("{name}: status = {got}, want {status} ({text})");
    }
    want_envelope(resp, status, code).await;
}

// Mirrors TestHubVerbsNotSupportedForEveryOtherKind: EVERY kind except
// opencode fails every verb's capability check — including a kind with no
// kind_features row at all (shell, unknown), which must reject exactly as
// finally as an explicit false.
#[tokio::test(flavor = "multi_thread")]
async fn verbs_not_supported_for_every_other_kind() {
    let (_h, url) = new_verb_hub().await;
    for slug in [
        VERB_SLUG_CODEX,
        VERB_SLUG_SHELL,
        VERB_SLUG_UNKNOWN,
        VERB_SLUG_CLAUDE,
        VERB_SLUG_CURSOR,
    ] {
        want_all_verbs(&format!("{url}/v1/sessions/{slug}"), 409, ERR_NOT_SUPPORTED).await;
    }
}

// Mirrors TestHubVerbsRejectionMatrix — the full precedence: body size (413)
// → body validation (400) → approval-id validation (400) → tracked lookup
// (404) → capability (409), including the STREAM-ORDER nuance the goldens
// pin: oversized valid JSON reads past the cap (413) while a small valid
// prefix with trailing garbage is a 400.
#[tokio::test(flavor = "multi_thread")]
async fn verbs_rejection_matrix() {
    let (_h, url) = new_verb_hub().await;
    let base = format!("{url}/v1/sessions/{VERB_SLUG_CODEX}");
    let ghost = format!("{url}/v1/sessions/ghost");
    let oversized = format!(r#"{{"text":"{}"}}"#, "x".repeat(17 * 1024));
    let long_id = "a".repeat(129);
    let client = http_client();

    let cases: Vec<(&str, String, String, u16, &str)> = vec![
        // --- size, before anything is parsed ---
        (
            "turn oversized",
            format!("{base}/turn"),
            oversized.clone(),
            413,
            "too_large",
        ),
        (
            "interrupt oversized",
            format!("{base}/interrupt"),
            oversized.clone(),
            413,
            "too_large",
        ),
        (
            "approval oversized",
            format!("{base}/approvals/call_01"),
            oversized.clone(),
            413,
            "too_large",
        ),
        (
            "oversized outranks unknown slug",
            format!("{ghost}/turn"),
            oversized.clone(),
            413,
            "too_large",
        ),
        // --- body is exactly ONE JSON value ---
        (
            "turn oversized whitespace trailer",
            format!("{base}/turn"),
            format!(r#"{{"text":"hi"}}{}"#, " ".repeat(17 * 1024)),
            413,
            "too_large",
        ),
        (
            "turn trailing second value",
            format!("{base}/turn"),
            r#"{"text":"hi"}{"text":"again"}"#.to_string(),
            400,
            "invalid_json",
        ),
        (
            "turn trailing garbage",
            format!("{base}/turn"),
            r#"{"text":"hi"} not-json"#.to_string(),
            400,
            "invalid_json",
        ),
        (
            "turn whitespace tail ok",
            format!("{base}/turn"),
            format!(r#"{{"text":"hi"}}{}"#, "  \n\t "),
            409,
            "not_supported",
        ),
        // --- body validation ---
        (
            "turn malformed json",
            format!("{base}/turn"),
            r#"{not json"#.to_string(),
            400,
            "invalid_json",
        ),
        (
            "turn empty body",
            format!("{base}/turn"),
            String::new(),
            400,
            "invalid_json",
        ),
        (
            "turn missing text",
            format!("{base}/turn"),
            r#"{"options":{"model":"x"}}"#.to_string(),
            400,
            "empty_text",
        ),
        (
            "turn whitespace text",
            format!("{base}/turn"),
            r#"{"text":" \n\t "}"#.to_string(),
            400,
            "empty_text",
        ),
        (
            "approval malformed json",
            format!("{base}/approvals/call_01"),
            r#"{"#.to_string(),
            400,
            "invalid_json",
        ),
        (
            "approval missing decision",
            format!("{base}/approvals/call_01"),
            r#"{}"#.to_string(),
            400,
            "invalid_decision",
        ),
        (
            "approval unknown decision",
            format!("{base}/approvals/call_01"),
            r#"{"decision":"maybe"}"#.to_string(),
            400,
            "invalid_decision",
        ),
        (
            "approval empty decision",
            format!("{base}/approvals/call_01"),
            r#"{"decision":""}"#.to_string(),
            400,
            "invalid_decision",
        ),
        (
            "bad decision outranks bad id",
            format!("{base}/approvals/.hidden"),
            r#"{"decision":"maybe"}"#.to_string(),
            400,
            "invalid_decision",
        ),
        // --- approval-id grammar (a malformed id is a bad REQUEST, never 404) ---
        (
            "id leading dot",
            format!("{base}/approvals/.hidden"),
            r#"{"decision":"allow"}"#.to_string(),
            400,
            "invalid_approval_id",
        ),
        (
            "id leading dash",
            format!("{base}/approvals/-x"),
            r#"{"decision":"allow"}"#.to_string(),
            400,
            "invalid_approval_id",
        ),
        (
            "id with space",
            format!("{base}/approvals/a%20b"),
            r#"{"decision":"allow"}"#.to_string(),
            400,
            "invalid_approval_id",
        ),
        (
            "id too long",
            format!("{base}/approvals/{long_id}"),
            r#"{"decision":"allow"}"#.to_string(),
            400,
            "invalid_approval_id",
        ),
        (
            "bad id outranks unknown slug",
            format!("{ghost}/approvals/.hidden"),
            r#"{"decision":"allow"}"#.to_string(),
            400,
            "invalid_approval_id",
        ),
        // --- tracked lookup ---
        (
            "turn unknown slug",
            format!("{ghost}/turn"),
            r#"{"text":"hi"}"#.to_string(),
            404,
            "unknown_slug",
        ),
        (
            "interrupt unknown slug",
            format!("{ghost}/interrupt"),
            String::new(),
            404,
            "unknown_slug",
        ),
        (
            "approval unknown slug",
            format!("{ghost}/approvals/call_01"),
            r#"{"decision":"deny"}"#.to_string(),
            404,
            "unknown_slug",
        ),
        (
            "malformed body outranks unknown slug",
            format!("{ghost}/turn"),
            r#"{nope"#.to_string(),
            400,
            "invalid_json",
        ),
        // --- capability, last ---
        (
            "turn valid but unsupported",
            format!("{base}/turn"),
            r#"{"text":"hi","options":{"model":"x"}}"#.to_string(),
            409,
            ERR_NOT_SUPPORTED,
        ),
        (
            "interrupt valid but unsupported",
            format!("{base}/interrupt"),
            String::new(),
            409,
            ERR_NOT_SUPPORTED,
        ),
        (
            "approval valid but unsupported",
            format!("{base}/approvals/call_01HQ8Z3K.tool:2-a"),
            r#"{"decision":"allow_always"}"#.to_string(),
            409,
            ERR_NOT_SUPPORTED,
        ),
    ];
    for (name, url, body, status, code) in cases {
        let resp = do_request(&client, Method::POST, &url, &body).await;
        want_envelope_named(name, resp, status, code).await;
    }

    // The escaped-traversal row goes over a RAW request: reqwest's Url parser
    // resolves `%2E%2E` dot segments CLIENT-side (Go's net/url keeps them
    // escaped), so only a verbatim request line reaches the grammar check —
    // which must answer 400 invalid_approval_id, never a 404 or a resolved
    // path.
    let authority = url.strip_prefix("http://").unwrap();
    let status = raw_post(
        authority,
        &format!("/v1/sessions/{VERB_SLUG_CODEX}/approvals/%2E%2E"),
        r#"{"decision":"allow"}"#,
    );
    assert_eq!(status, 400, "id traversal (escaped ..)");
}

// Mirrors TestHubVerbsWrongMethodAndUnknownRoute: the verbs are POST-only —
// the mux answers other methods 405 and unrouted paths 404 (status-only
// contract; §2.2).
#[tokio::test(flavor = "multi_thread")]
async fn verbs_wrong_method_and_unknown_route() {
    let (_h, url) = new_verb_hub().await;
    let base = format!("{url}/v1/sessions/{VERB_SLUG_CODEX}");
    let client = http_client();

    for (name, method, path, want) in [
        ("GET turn", Method::GET, format!("{base}/turn"), 405),
        ("PUT turn", Method::PUT, format!("{base}/turn"), 405),
        (
            "GET interrupt",
            Method::GET,
            format!("{base}/interrupt"),
            405,
        ),
        (
            "DELETE approvals",
            Method::DELETE,
            format!("{base}/approvals/call_01"),
            405,
        ),
        (
            "GET approvals",
            Method::GET,
            format!("{base}/approvals/call_01"),
            405,
        ),
        // An approvals request with NO id is a different (unregistered) path.
        (
            "approvals without an id",
            Method::POST,
            format!("{base}/approvals"),
            404,
        ),
        ("unrouted verb", Method::POST, format!("{base}/rewind"), 404),
    ] {
        let resp = do_request(&client, method, &path, "").await;
        assert_eq!(resp.status().as_u16(), want, "{name}");
    }
}

// Mirrors TestHubInterruptIgnoresBody: garbage of any shape gets the same
// answer as no body at all (oversized is covered in the matrix).
#[tokio::test(flavor = "multi_thread")]
async fn interrupt_ignores_body() {
    let (_h, url) = new_verb_hub().await;
    let base = format!("{url}/v1/sessions/{VERB_SLUG_CODEX}/interrupt");
    for body in ["", "{}", r#"{"text":"ignored"}"#, "not json at all"] {
        let resp = do_request(&http_client(), Method::POST, &base, body).await;
        want_envelope(resp, 409, ERR_NOT_SUPPORTED).await;
    }
}

// Mirrors TestApprovalIDGrammar — the contract grammar (mirrored by the
// server proxy's path classifier).
#[test]
fn approval_id_grammar() {
    let long_ok = "z".repeat(128);
    let long_bad = "z".repeat(129);
    let cases: Vec<(&str, bool)> = vec![
        ("a", true),
        ("9", true),
        ("call_01HQ8Z3K", true),
        ("call_01HQ8Z3K.tool:2", true),
        ("req-42_a.b:c", true),
        (long_ok.as_str(), true), // the ceiling, inclusive
        ("", false),
        (".", false),
        ("..", false),
        ("...", false),
        (".hidden", false),
        ("-lead", false),
        ("_lead", false),
        (":lead", false),
        ("a/b", false),
        ("a b", false),
        ("a\tb", false),
        ("a\nb", false),
        ("a$b", false),
        ("a%b", false),
        ("a?b", false),
        ("a#b", false),
        (long_bad.as_str(), false),
    ];
    for (id, ok) in cases {
        assert_eq!(
            APPROVAL_ID_RE.is_match(id),
            ok,
            "contract grammar on {id:?}"
        );
    }
}

// Mirrors TestVerbSuccessShapesRoundTrip — the schema pin that stops a lane
// from recontracting the success bodies (deny_unknown_fields is the
// DisallowUnknownFields half).
#[test]
fn verb_success_shapes_round_trip() {
    fn pin<T: serde::Serialize + serde::de::DeserializeOwned>(v: &T, want: &str) {
        assert_eq!(serde_json::to_string(v).unwrap(), want);
        let back: T = serde_json::from_str(want).expect("pinned shape decodes strictly");
        assert_eq!(serde_json::to_string(&back).unwrap(), want, "round-trip");
    }
    pin(
        &TurnResponse {
            turn_id: "trn_01HQ8Z3K".into(),
        },
        r#"{"turn_id":"trn_01HQ8Z3K"}"#,
    );
    pin(
        &InterruptResponse { interrupting: true },
        r#"{"interrupting":true}"#,
    );
    pin(
        &ApprovalResponse {
            resolved: true,
            decision: APPROVAL_DECISION_ALLOW.into(),
        },
        r#"{"resolved":true,"decision":"allow"}"#,
    );
}

// Mirrors TestVerbErrorCodeSpellings.
#[test]
fn verb_error_code_spellings() {
    assert_eq!(ERR_NOT_SUPPORTED, "not_supported");
    assert_eq!(ERR_NOT_ACCEPTING, "not_accepting");
    assert_eq!(ERR_ALREADY_RESOLVED, "already_resolved");
    assert_eq!(ERR_UNKNOWN_APPROVAL, "unknown_approval");
}

// Mirrors TestHubSessionsPendingApprovalsOverlay: absent by default; copied,
// not aliased, when a lane publishes into it (Rust's Clone makes the aliasing
// hazard structural, so the wire half is what's pinned here).
#[tokio::test(flavor = "multi_thread")]
async fn sessions_pending_approvals_overlay() {
    let f = HubTmux::new();
    let clk = HubClock::new();
    let h = Arc::new(new_test_hub(&f, &clk));
    f.set(
        "rc-pnd001",
        &codex_ready_pane(),
        &managed_env("id-p", &RcKind::Codex),
    );
    h.reconcile();
    let (url, _s) = serve_hub(&h).await;
    let client = http_client();

    // Default: nothing produces approvals — the key is absent entirely.
    let raw = client
        .get(format!("{url}/v1/sessions"))
        .send()
        .await
        .unwrap()
        .text()
        .await
        .unwrap();
    assert!(
        !raw.contains("pending_approvals"),
        "no producer sets pending_approvals here: {raw}"
    );

    // White-box: publish into the seam the way a lane adapter will.
    {
        let mut ts = h.lock_track();
        ts.tracked.get_mut("pnd001").unwrap().pending_approvals = vec![FeedApproval {
            id: "call_01".into(),
            status: APPROVAL_STATUS_PENDING.into(),
            decisions: vec![
                APPROVAL_DECISION_ALLOW.into(),
                APPROVAL_DECISION_DENY.into(),
            ],
            ..FeedApproval::default()
        }];
    }
    let body: HubSessionsResponse = client
        .get(format!("{url}/v1/sessions"))
        .send()
        .await
        .unwrap()
        .json()
        .await
        .unwrap();
    assert_eq!(body.sessions.len(), 1);
    let got = body.sessions[0]
        .pending_approvals
        .as_ref()
        .expect("overlaid");
    assert_eq!(got.len(), 1);
    assert_eq!(got[0].id, "call_01");
    assert_eq!(got[0].status, APPROVAL_STATUS_PENDING);
    assert_eq!(got[0].decisions.len(), 2);
}

// Mirrors TestReconcilePublishesPendingApprovals: reconcile republishes the
// lane's OPEN approvals every tick; a non-publishing watcher leaves the field
// alone.
#[tokio::test(flavor = "multi_thread")]
async fn reconcile_publishes_pending_approvals() {
    use super::hub_test_support::{StubApprovalWatcher, StubWatcher};
    let f = HubTmux::new();
    let clk = HubClock::new();
    let h = Arc::new(new_test_hub(&f, &clk));
    f.set(
        "rc-apv001",
        &codex_ready_pane(),
        &managed_env("id-a", &RcKind::Codex),
    );
    h.reconcile();

    let pub_w = Arc::new(StubApprovalWatcher {
        stub: StubWatcher {
            activity: RcActivity::NeedsApproval,
            fresh: true,
            ..StubWatcher::default()
        },
        approvals: vec![FeedApproval {
            id: "per_1".into(),
            status: APPROVAL_STATUS_PENDING.into(),
            decisions: vec![
                APPROVAL_DECISION_ALLOW.into(),
                APPROVAL_DECISION_ALLOW_ALWAYS.into(),
                APPROVAL_DECISION_DENY.into(),
            ],
            ..FeedApproval::default()
        }],
        blocked: false,
    });
    {
        let mut ts = h.lock_track();
        ts.tracked.get_mut("apv001").unwrap().watcher = Some(pub_w as _);
    }
    h.reconcile();
    {
        let ts = h.lock_track();
        let got = &ts.tracked.get("apv001").unwrap().pending_approvals;
        assert_eq!(got.len(), 1);
        assert_eq!(got[0].id, "per_1");
    }

    // It surfaces on the wire through the /v1/sessions overlay.
    let (url, _s) = serve_hub(&h).await;
    let body: HubSessionsResponse = http_client()
        .get(format!("{url}/v1/sessions"))
        .send()
        .await
        .unwrap()
        .json()
        .await
        .unwrap();
    assert_eq!(
        body.sessions[0]
            .pending_approvals
            .as_ref()
            .map(|p| p[0].id.as_str()),
        Some("per_1")
    );

    // Resolution is a REMOVAL from the snapshot.
    let empty_pub = Arc::new(StubApprovalWatcher {
        stub: StubWatcher {
            activity: RcActivity::Idle,
            fresh: true,
            ..StubWatcher::default()
        },
        ..StubApprovalWatcher::default()
    });
    {
        let mut ts = h.lock_track();
        ts.tracked.get_mut("apv001").unwrap().watcher = Some(empty_pub as _);
    }
    h.reconcile();
    {
        let ts = h.lock_track();
        assert!(
            ts.tracked
                .get("apv001")
                .unwrap()
                .pending_approvals
                .is_empty(),
            "the lane's cleared snapshot must land"
        );
    }

    // A non-publishing watcher must not blank a snapshot it does not own.
    {
        let mut ts = h.lock_track();
        let tr = ts.tracked.get_mut("apv001").unwrap();
        tr.watcher = Some(Arc::new(StubWatcher {
            activity: RcActivity::Idle,
            ..StubWatcher::default()
        }) as _);
        tr.pending_approvals = vec![FeedApproval {
            id: "pane-1".into(),
            status: APPROVAL_STATUS_PENDING.into(),
            ..FeedApproval::default()
        }];
    }
    h.reconcile();
    {
        let ts = h.lock_track();
        let got = &ts.tracked.get("apv001").unwrap().pending_approvals;
        assert_eq!(got.len(), 1);
        assert_eq!(got[0].id, "pane-1", "the non-lane entry is retained");
    }
}

// ---- the opencode lane, end to end over the real shell ----
// (hub_verbs_test.go:493-860 — the verbs through the hub's own handler chain
// against a tracked opencode session whose watcher is a REAL OpencodeWatcher
// talking to the fake embedded server, so the success shapes, the rejection
// matrix, and the WS-B scoping guard are exercised on the client's path.)

use super::watch::SessionWatcher;

/// The transport suite's fixture session id (`ocFixtureSID`).
const OC_SID: &str = "ses_07cbd4370ffeF17Wb3Ius82a2g";

/// `newOpencodeVerbHub` (`hub_verbs_test.go:517`): a reconciled hub tracking
/// one live opencode session against the fake. `agent_session` "" leaves the
/// watcher UNPINNED (the uncorrelated-TUI case).
async fn new_opencode_verb_hub(
    f: &Arc<FakeOpencode>,
    agent_session: &str,
) -> (
    Arc<Hub>,
    String,
    Arc<dyn SessionWatcher + Send + Sync>,
    Arc<HubClock>,
) {
    f.hold_open_sse();
    let tm = HubTmux::new();
    let clk = HubClock::new();
    let h = Arc::new(new_test_hub(&tm, &clk));
    tm.set(
        &format!("rc-{VERB_SLUG_OPENCODE}"),
        &opencode_ready_pane(),
        &super::hub_test_support::opencode_verb_env("id-vo", f.port(), agent_session),
    );
    {
        let hh = Arc::clone(&h);
        tokio::task::spawn_blocking(move || hh.reconcile())
            .await
            .unwrap();
    }
    let watcher = {
        let ts = h.lock_track();
        ts.tracked
            .get(VERB_SLUG_OPENCODE)
            .expect("tracked")
            .watcher
            .clone()
            .expect("reconcile did not commit an opencode watcher")
    };
    let (url, _s) = serve_hub(&h).await;
    (h, url, watcher, clk)
}

/// `waitForAsk` — drives the watcher until the announced ask reaches the
/// fold, so the approvals verb has a PENDING entry to resolve.
async fn wait_for_ask(w: &Arc<dyn SessionWatcher + Send + Sync>, clk: &Arc<HubClock>, id: &str) {
    let deadline = std::time::Instant::now() + Duration::from_secs(5);
    loop {
        w.refresh(clk.now());
        if w.as_approval_resolver()
            .expect("resolver")
            .approval_state(id)
            .is_some()
        {
            return;
        }
        assert!(
            std::time::Instant::now() < deadline,
            "the ask never reached the fold"
        );
        tokio::time::sleep(Duration::from_millis(10)).await;
    }
}

/// `wantJSON` — status + the EXACT decoded body (key set included).
async fn want_json(resp: reqwest::Response, status: u16, want: serde_json::Value) {
    let got_status = resp.status().as_u16();
    let body = resp.text().await.unwrap();
    assert_eq!(got_status, status, "status (body {body})");
    let got: serde_json::Value = serde_json::from_str(&body).expect("json body");
    assert_eq!(got, want, "body = {body}");
}

// Mirrors TestHubVerbsOpencodeSuccessShapes: 202 {"turn_id"}, 202
// {"interrupting":true}, 200 {"resolved","decision"} — exactly those keys —
// with the resolution recorded synchronously on BOTH sides of the seam, and
// exactly the three pinned session-scoped routes touched.
#[tokio::test(flavor = "multi_thread")]
async fn opencode_verbs_success_shapes() {
    let f = FakeOpencode::new();
    f.set(|s| s.pin_guard(OC_SID));
    f.stream_ask(OC_SID, "per_1");
    let (h, url, w, clk) = new_opencode_verb_hub(&f, OC_SID).await;
    let base = format!("{url}/v1/sessions/{VERB_SLUG_OPENCODE}");
    let client = http_client();

    // turn → 202 with an opaque, non-empty handle.
    let resp = do_request(
        &client,
        Method::POST,
        &format!("{base}/turn"),
        r#"{"text":"run the tests"}"#,
    )
    .await;
    let status = resp.status().as_u16();
    let body = resp.text().await.unwrap();
    assert_eq!(status, 202, "turn: {body}");
    let turn: std::collections::HashMap<String, serde_json::Value> =
        serde_json::from_str(&body).unwrap();
    assert_eq!(
        turn.len(),
        1,
        "turn body = {body}, want exactly {{turn_id}}"
    );
    assert!(
        turn.get("turn_id")
            .and_then(|v| v.as_str())
            .is_some_and(|s| !s.is_empty()),
        "turn_id must be a non-empty opaque handle: {body}"
    );

    // interrupt → 202 {"interrupting":true}.
    want_json(
        do_request(&client, Method::POST, &format!("{base}/interrupt"), "").await,
        202,
        serde_json::json!({"interrupting": true}),
    )
    .await;

    // approvals → 200 {"resolved":true,"decision":"allow"} once pending.
    wait_for_ask(&w, &clk, "per_1").await;
    want_json(
        do_request(
            &client,
            Method::POST,
            &format!("{base}/approvals/per_1"),
            r#"{"decision":"allow"}"#,
        )
        .await,
        200,
        serde_json::json!({"resolved": true, "decision": "allow"}),
    )
    .await;

    // Recorded synchronously: the watcher's fold AND the session's
    // pending_approvals snapshot (no wait for the next tick).
    let resolver = w.as_approval_resolver().unwrap();
    assert_eq!(
        resolver.approval_state("per_1"),
        Some((
            APPROVAL_STATUS_RESOLVED.to_string(),
            APPROVAL_DECISION_ALLOW.to_string()
        ))
    );
    {
        let ts = h.lock_track();
        let pending = &ts
            .tracked
            .get(VERB_SLUG_OPENCODE)
            .unwrap()
            .pending_approvals;
        assert!(
            pending.iter().all(|a| a.id != "per_1"),
            "pending_approvals still lists the resolved ask: {pending:?}"
        );
    }

    // Exactly the three pinned, session-scoped routes were touched (the
    // fake's WS-B guard covers the negative half from underneath).
    assert_eq!(f.post_paths().len(), 3, "one upstream POST per verb");
    assert!(f.violations().is_empty(), "{:?}", f.violations());
    w.close();
}

// Mirrors TestHubVerbsOpencodeRejectionMatrix.
#[tokio::test(flavor = "multi_thread")]
async fn opencode_verbs_rejection_matrix() {
    // Unpinned session: the two delivery verbs answer 409 not_accepting (the
    // NoAgentSession sentinel), approvals resolves state FIRST → 404.
    {
        let f = FakeOpencode::new(); // empty store → the watcher never pins
        let (_h, url, w, _clk) = new_opencode_verb_hub(&f, "").await;
        let base = format!("{url}/v1/sessions/{VERB_SLUG_OPENCODE}");
        let client = http_client();
        for (name, path, body) in VERB_CASES {
            let resp = do_request(&client, Method::POST, &format!("{base}{path}"), body).await;
            let (status, code) = if name == "approvals" {
                (404, ERR_UNKNOWN_APPROVAL)
            } else {
                (409, ERR_NOT_ACCEPTING)
            };
            want_envelope(resp, status, code).await;
        }
        assert!(
            f.post_paths().is_empty(),
            "an unpinned session produced upstream POSTs"
        );
        w.close();
    }

    // Unknown approval id: 404, never reaching the agent.
    {
        let f = FakeOpencode::new();
        f.set(|s| s.pin_guard(OC_SID));
        let (_h, url, w, _clk) = new_opencode_verb_hub(&f, OC_SID).await;
        let resp = do_request(
            &http_client(),
            Method::POST,
            &format!("{url}/v1/sessions/{VERB_SLUG_OPENCODE}/approvals/per_nope"),
            r#"{"decision":"allow"}"#,
        )
        .await;
        want_envelope(resp, 404, ERR_UNKNOWN_APPROVAL).await;
        assert!(
            f.post_paths().is_empty(),
            "an unknown id must not reach the agent"
        );
        w.close();
    }

    // Replay: same decision → 200 idempotent with NO second POST; different
    // decision → 409 already_resolved, still no POST.
    {
        let f = FakeOpencode::new();
        f.set(|s| s.pin_guard(OC_SID));
        f.stream_ask(OC_SID, "per_1");
        let (_h, url, w, clk) = new_opencode_verb_hub(&f, OC_SID).await;
        let base = format!("{url}/v1/sessions/{VERB_SLUG_OPENCODE}/approvals/per_1");
        let client = http_client();
        wait_for_ask(&w, &clk, "per_1").await;

        want_json(
            do_request(&client, Method::POST, &base, r#"{"decision":"allow"}"#).await,
            200,
            serde_json::json!({"resolved": true, "decision": "allow"}),
        )
        .await;
        let posts = f.post_paths().len();
        want_json(
            do_request(&client, Method::POST, &base, r#"{"decision":"allow"}"#).await,
            200,
            serde_json::json!({"resolved": true, "decision": "allow"}),
        )
        .await;
        assert_eq!(
            f.post_paths().len(),
            posts,
            "a same-decision replay POSTed again"
        );
        want_envelope(
            do_request(&client, Method::POST, &base, r#"{"decision":"deny"}"#).await,
            409,
            ERR_ALREADY_RESOLVED,
        )
        .await;
        assert_eq!(
            f.post_paths().len(),
            posts,
            "a conflicting replay POSTed anyway"
        );
        w.close();
    }

    // Upstream failure: a retryable 409 not_accepting on every verb — never a
    // 5xx — and a failed resolve leaves the ask OPEN.
    {
        let f = FakeOpencode::new();
        f.set(|s| {
            s.pin_guard(OC_SID);
            s.prompt_status(500);
            s.abort_status(500);
            s.permission_status(500);
        });
        f.stream_ask(OC_SID, "per_1");
        let (_h, url, w, clk) = new_opencode_verb_hub(&f, OC_SID).await;
        let base = format!("{url}/v1/sessions/{VERB_SLUG_OPENCODE}");
        wait_for_ask(&w, &clk, "per_1").await;

        want_all_verbs(&base, 409, ERR_NOT_ACCEPTING).await;
        let resolver = w.as_approval_resolver().unwrap();
        assert_eq!(
            resolver.approval_state("per_1").map(|(s, _)| s),
            Some(APPROVAL_STATUS_PENDING.to_string()),
            "a failed resolve must leave the ask pending"
        );
        w.close();
    }

    // No watcher yet (no recorded port): the kind advertises the verbs but
    // this session cannot serve them — the no-lane 409, genuinely reachable.
    {
        let tm = HubTmux::new();
        let clk = HubClock::new();
        let h = Arc::new(new_test_hub(&tm, &clk));
        tm.set(
            "rc-vrb011",
            &opencode_ready_pane(),
            &managed_env("id-vo2", &RcKind::Opencode),
        );
        h.reconcile();
        let (url, _s) = serve_hub(&h).await;
        want_all_verbs(&format!("{url}/v1/sessions/vrb011"), 409, ERR_NOT_ACCEPTING).await;
    }
}

// Mirrors TestHubVerbsOpencodeConcurrentResolvePostsOnce: the claim makes
// read-then-POST atomic — exactly ONE upstream POST for two concurrent
// requests; the loser gets "in progress" (409, retryable).
#[tokio::test(flavor = "multi_thread")]
async fn opencode_concurrent_resolve_posts_once() {
    let f = FakeOpencode::new();
    f.set(|s| s.pin_guard(OC_SID));
    f.stream_ask(OC_SID, "per_1");
    let entered = Arc::new(std::sync::Barrier::new(2));
    let release = Arc::new(AtomicBool::new(false));
    let fired = Arc::new(AtomicBool::new(false));
    {
        let (entered, release, fired) = (
            Arc::clone(&entered),
            Arc::clone(&release),
            Arc::clone(&fired),
        );
        f.before_mutation(move |_path| {
            if !fired.swap(true, Ordering::SeqCst) {
                entered.wait();
                while !release.load(Ordering::Relaxed) {
                    std::thread::sleep(Duration::from_millis(5));
                }
            }
        });
    }
    let (_h, url, w, clk) = new_opencode_verb_hub(&f, OC_SID).await;
    let base = format!("{url}/v1/sessions/{VERB_SLUG_OPENCODE}/approvals/per_1");
    wait_for_ask(&w, &clk, "per_1").await;

    let first_url = base.clone();
    let first = tokio::spawn(async move {
        let resp = do_request(
            &http_client(),
            Method::POST,
            &first_url,
            r#"{"decision":"allow"}"#,
        )
        .await;
        (resp.status().as_u16(), resp.text().await.unwrap())
    });
    // Rendezvous with the winner INSIDE the upstream POST.
    let entered2 = Arc::clone(&entered);
    tokio::task::spawn_blocking(move || entered2.wait())
        .await
        .unwrap();

    // The racing request cannot claim the id — it never reaches the network.
    let second = do_request(
        &http_client(),
        Method::POST,
        &base,
        r#"{"decision":"allow"}"#,
    )
    .await;
    let status = second.status().as_u16();
    let body = second.text().await.unwrap();
    assert_eq!(status, 409, "{body}");
    assert!(
        body.contains(ERR_NOT_ACCEPTING) && body.contains("already in progress"),
        "concurrent resolve = {body}"
    );

    release.store(true, Ordering::Relaxed);
    let (fstatus, fbody) = first.await.unwrap();
    assert_eq!(fstatus, 200, "{fbody}");
    assert_eq!(
        f.post_paths().len(),
        1,
        "exactly one upstream POST for the pair"
    );

    // The settled ask answers idempotently afterwards, still without a POST.
    want_json(
        do_request(
            &http_client(),
            Method::POST,
            &base,
            r#"{"decision":"allow"}"#,
        )
        .await,
        200,
        serde_json::json!({"resolved": true, "decision": "allow"}),
    )
    .await;
    assert_eq!(
        f.post_paths().len(),
        1,
        "still exactly one after the replay"
    );
    w.close();
}

// Mirrors TestHubVerbsOpencodeLaneErrorsDoNotLeakInternals: the wire message
// carries the upstream STATUS but never the loopback port, the session id, or
// the upstream path; the unpinned sentinel stays verbatim.
#[tokio::test(flavor = "multi_thread")]
async fn opencode_lane_errors_do_not_leak_internals() {
    let f = FakeOpencode::new();
    f.set(|s| {
        s.pin_guard(OC_SID);
        s.prompt_status(500);
    });
    let (_h, url, w, _clk) = new_opencode_verb_hub(&f, OC_SID).await;
    let resp = do_request(
        &http_client(),
        Method::POST,
        &format!("{url}/v1/sessions/{VERB_SLUG_OPENCODE}/turn"),
        r#"{"text":"hi"}"#,
    )
    .await;
    let status = resp.status().as_u16();
    let body = resp.text().await.unwrap();
    assert_eq!(status, 409, "{body}");
    assert!(body.contains("upstream status 500"), "{body}");
    for secret in [OC_SID, "/session/", "127.0.0.1", &f.port().to_string()] {
        assert!(
            !body.contains(secret),
            "message leaks hub-internal addressing ({secret}): {body}"
        );
    }
    w.close();

    // The unpinned sentinel is operator-facing and stays verbatim.
    let f2 = FakeOpencode::new();
    let (_h2, url2, w2, _clk2) = new_opencode_verb_hub(&f2, "").await;
    let resp2 = do_request(
        &http_client(),
        Method::POST,
        &format!("{url2}/v1/sessions/{VERB_SLUG_OPENCODE}/turn"),
        r#"{"text":"hi"}"#,
    )
    .await;
    let body2 = resp2.text().await.unwrap();
    assert!(
        body2.contains("agent session not established yet"),
        "unpinned message = {body2}, want the remediation sentinel"
    );
    w2.close();
}

// Mirrors TestRepublishApprovalsSkipsAReplacedEntry, under the Rust identity
// mapping (the watcher Arc stands in for Go's entry pointer — entry and
// watcher are replaced together on a recreate).
#[tokio::test(flavor = "multi_thread")]
async fn republish_approvals_skips_a_replaced_entry() {
    use super::hub_test_support::{StubApprovalWatcher, StubWatcher};
    let f = HubTmux::new();
    let clk = HubClock::new();
    let h = Arc::new(new_test_hub(&f, &clk));
    f.set(
        "rc-rpb001",
        &opencode_ready_pane(),
        &managed_env("id-rp", &RcKind::Opencode),
    );
    h.reconcile();

    let publisher: Arc<dyn SessionWatcher + Send + Sync> = Arc::new(StubApprovalWatcher {
        stub: StubWatcher {
            activity: RcActivity::Idle,
            ..StubWatcher::default()
        },
        ..StubApprovalWatcher::default() // nothing open
    });
    {
        let mut ts = h.lock_track();
        let tr = ts.tracked.get_mut("rpb001").unwrap();
        tr.watcher = Some(Arc::clone(&publisher));
        tr.pending_approvals = vec![FeedApproval {
            id: "per_1".into(),
            status: APPROVAL_STATUS_PENDING.into(),
            ..FeedApproval::default()
        }];
    }

    // Still the tracked incarnation → the snapshot refreshes from the lane.
    h.republish_approvals("rpb001", &publisher);
    {
        let ts = h.lock_track();
        assert!(
            ts.tracked
                .get("rpb001")
                .unwrap()
                .pending_approvals
                .is_empty(),
            "want the lane's (empty) snapshot"
        );
    }

    // A recreate replaces the incarnation (new watcher): the republish
    // through the OLD watcher must be skipped — the live entry keeps its own
    // snapshot.
    {
        let mut ts = h.lock_track();
        let tr = ts.tracked.get_mut("rpb001").unwrap();
        tr.watcher = Some(Arc::new(StubWatcher::default()) as _);
        tr.pending_approvals = vec![FeedApproval {
            id: "per_live".into(),
            status: APPROVAL_STATUS_PENDING.into(),
            ..FeedApproval::default()
        }];
    }
    h.republish_approvals("rpb001", &publisher);
    {
        let ts = h.lock_track();
        let got = &ts.tracked.get("rpb001").unwrap().pending_approvals;
        assert_eq!(got.len(), 1);
        assert_eq!(
            got[0].id, "per_live",
            "the live entry keeps its own snapshot"
        );
    }
}

// ---- POST /v1/ingest/cursor (hub_ingest_test.go) ----

use super::ingest::{HUB_INGEST_MAX_BODY_BYTES, MAX_PRE_WATCHER_EVENTS, PRE_WATCHER_TTL};
use super::messages::MAX_MESSAGES_LIMIT;
use shed_rc_engine::tmux::{TmuxResult, TmuxRunner};

/// `ingestHub` (`hub_ingest_test.go:18`): one session of the given kind,
/// optionally reconciled, plus the served router.
async fn ingest_hub(
    kind: &RcKind,
    reconcile: bool,
) -> (Arc<Hub>, Arc<HubTmux>, String, Arc<HubClock>) {
    let f = HubTmux::new();
    let clk = HubClock::new();
    let h = Arc::new(new_test_hub(&f, &clk));
    let pane = if *kind == RcKind::Codex {
        codex_ready_pane()
    } else {
        pane_fixture("cursor-ready")
    };
    f.set("rc-ing001", &pane, &managed_env("id-ing", kind));
    if reconcile {
        h.reconcile();
    }
    let (url, _s) = serve_hub(&h).await;
    (h, f, url, clk)
}

/// `postHook` — one hook payload the way the preseeded script posts it.
async fn post_hook(url: &str, slug: &str, event: &str, payload: &str) -> reqwest::Response {
    http_client()
        .post(format!("{url}/v1/ingest/cursor"))
        .query(&[("slug", slug), ("event", event)])
        .header("Content-Type", "application/json")
        .body(payload.to_string())
        .send()
        .await
        .expect("post hook")
}

/// `feedRowsOf` — every feed row in a tracked session's ring, oldest first.
fn feed_rows_of(h: &Arc<Hub>, slug: &str) -> Vec<FeedMessage> {
    let ring = {
        let ts = h.lock_track();
        Arc::clone(&ts.tracked.get(slug).expect("tracked").ring)
    };
    ring.since(0, MAX_MESSAGES_LIMIT as i64).0
}

fn drop_watcher(h: &Arc<Hub>, slug: &str) {
    let mut ts = h.lock_track();
    ts.tracked.get_mut(slug).unwrap().watcher = None;
}

// Mirrors TestHubIngestCursorReachesWatcherAndFeed.
#[tokio::test(flavor = "multi_thread")]
async fn ingest_cursor_reaches_watcher_and_feed() {
    let (h, _f, url, _clk) = ingest_hub(&RcKind::Cursor, true).await;

    let resp = post_hook(
        &url,
        "ing001",
        "beforeSubmitPrompt",
        &format!(r#"{{"session_id":"{CURSOR_SID}","prompt":"build the thing"}}"#),
    )
    .await;
    assert_eq!(resp.status().as_u16(), 202);
    h.reconcile();

    let rows = feed_rows_of(&h, "ing001");
    assert_eq!(rows.len(), 1, "{rows:?}");
    assert_eq!(rows[0].role, "user");
    assert_eq!(rows[0].text, "build the thing");
    let ts = h.lock_track();
    assert_eq!(
        ts.tracked.get("ing001").unwrap().activity,
        Some(RcActivity::Working),
        "want working after a submitted prompt"
    );
}

// Mirrors TestHubIngestCursorOversizeIs413: the ingest cap is its OWN 256 KiB
// — a 200 KiB payload is accepted (the whole reason for the larger cap), one
// past the cap is a 413 with the event dropped.
#[tokio::test(flavor = "multi_thread")]
async fn ingest_cursor_oversize_is_413() {
    let (h, _f, url, _clk) = ingest_hub(&RcKind::Cursor, true).await;

    let big = format!(
        r#"{{"session_id":"{CURSOR_SID}","command":"make","output":"{}"}}"#,
        "x".repeat(200 << 10)
    );
    let resp = post_hook(&url, "ing001", "afterShellExecution", &big).await;
    assert_eq!(
        resp.status().as_u16(),
        202,
        "a 200 KiB payload must not hit the 16 KiB verb cap"
    );

    let over = format!(
        r#"{{"session_id":"{CURSOR_SID}","output":"{}"}}"#,
        "x".repeat(HUB_INGEST_MAX_BODY_BYTES + 1024)
    );
    let resp = post_hook(&url, "ing001", "afterShellExecution", &over).await;
    assert_eq!(resp.status().as_u16(), 413);

    h.reconcile();
    let rows = feed_rows_of(&h, "ing001");
    assert_eq!(
        rows.len(),
        1,
        "exactly the accepted event (the oversized one dropped): {}",
        rows.len()
    );
}

// Mirrors TestHubIngestCursorRejections — the pinned precedence.
#[tokio::test(flavor = "multi_thread")]
async fn ingest_cursor_rejections() {
    let (_h, _f, url, _clk) = ingest_hub(&RcKind::Cursor, true).await;

    for (name, slug, event, want) in [
        ("unknown slug", "nosuch", "stop", 404),
        ("malformed slug", "not_a_slug!", "stop", 400),
        ("missing slug", "", "stop", 400),
        ("malformed event", "ing001", "st op!", 400),
        ("missing event", "ing001", "", 400),
    ] {
        let resp = post_hook(&url, slug, event, "{}").await;
        assert_eq!(resp.status().as_u16(), want, "{name}");
    }

    // A tracked session of ANOTHER kind: 409 not_supported.
    let (_h2, _f2, codex_url, _clk2) = ingest_hub(&RcKind::Codex, true).await;
    let resp = post_hook(&codex_url, "ing001", "stop", "{}").await;
    want_envelope(resp, 409, ERR_NOT_SUPPORTED).await;
}

// Mirrors TestHubIngestCursorPreWatcherQueueDrains: events landing before the
// watcher exists are held and drained the moment it is constructed.
#[tokio::test(flavor = "multi_thread")]
async fn ingest_cursor_pre_watcher_queue_drains() {
    let (h, _f, url, _clk) = ingest_hub(&RcKind::Cursor, false).await;
    h.reconcile();
    drop_watcher(&h, "ing001"); // the create→first-tick window shape

    for (event, payload) in [
        (
            "sessionStart",
            format!(r#"{{"session_id":"{CURSOR_SID}"}}"#),
        ),
        (
            "beforeSubmitPrompt",
            format!(r#"{{"session_id":"{CURSOR_SID}","prompt":"the kickoff prompt"}}"#),
        ),
    ] {
        let resp = post_hook(&url, "ing001", event, &payload).await;
        assert_eq!(resp.status().as_u16(), 202, "{event}");
    }
    assert_eq!(
        h.ingest.queued_events("ing001"),
        2,
        "both events held for the not-yet-built watcher"
    );

    // The next tick builds the watcher, drains the queue into it, folds it.
    h.reconcile();
    assert_eq!(
        h.ingest.len(),
        0,
        "the queue clears once the watcher takes it"
    );
    let rows = feed_rows_of(&h, "ing001");
    assert_eq!(rows.len(), 2, "{rows:?}");
    assert_eq!(rows[1].text, "the kickoff prompt");
}

/// `hookedTmux` (`hub_ingest_test.go:213`): wraps the fake runner so a test
/// can act at a precise point INSIDE reconcile's unlocked section.
type RunHook = Box<dyn Fn(&[&str]) + Send>;

struct HookedTmux {
    inner: Arc<HubTmux>,
    on_run: std::sync::Mutex<Option<RunHook>>,
}

impl TmuxRunner for HookedTmux {
    fn run(&self, args: &[&str]) -> TmuxResult {
        let res = self.inner.run(args);
        if let Some(hook) = &*self.on_run.lock().unwrap() {
            hook(args); // called with the fake's lock RELEASED; may re-enter
        }
        res
    }
}

// Mirrors TestHubIngestCursorPreWatcherDrainsAfterCommit — THE
// CONSTRUCT→COMMIT WINDOW: a hook arriving between ensureWatcher's
// construction-time drain and the commit-phase publish queues into a queue
// the first drain already passed; the post-commit drain must take it.
#[tokio::test(flavor = "multi_thread")]
async fn ingest_cursor_pre_watcher_drains_after_commit() {
    let f = HubTmux::new();
    let clk = HubClock::new();
    let hooked = Arc::new(HookedTmux {
        inner: Arc::clone(&f),
        on_run: std::sync::Mutex::new(None),
    });
    let h = {
        let clk2 = Arc::clone(&clk);
        Arc::new(Hub::new(HubConfig {
            quiet_period: Duration::from_secs(4),
            send_line_settle: Some(Duration::ZERO),
            ..super::hub_test_support::hub_config_with_runner(hooked.clone() as _, move || {
                clk2.now()
            })
        }))
    };
    f.set(
        "rc-win001",
        &pane_fixture("cursor-ready"),
        &managed_env("id-win", &RcKind::Cursor),
    );
    let (url, _s) = serve_hub(&h).await;
    let authority = url.strip_prefix("http://").unwrap().to_string();

    let captures = Arc::new(std::sync::atomic::AtomicUsize::new(0));
    let fired = Arc::new(AtomicBool::new(false));
    let in_window = Arc::new(AtomicBool::new(false));
    {
        let (captures, fired, in_window) = (
            Arc::clone(&captures),
            Arc::clone(&fired),
            Arc::clone(&in_window),
        );
        let hub_for_hook = Arc::clone(&h);
        let payload = format!(r#"{{"session_id":"{CURSOR_SID}","prompt":"the kickoff prompt"}}"#);
        *hooked.on_run.lock().unwrap() = Some(Box::new(move |args| {
            // The FIRST capture-pane of a tick is the enumeration one (before
            // ensureWatcher); the second is the stability tracker's, by which
            // point the watcher exists but is not yet published. Fire once.
            if args.first() != Some(&"capture-pane") {
                return;
            }
            let n = captures.fetch_add(1, Ordering::SeqCst) + 1;
            if n != 2 || fired.swap(true, Ordering::SeqCst) {
                return;
            }
            // Self-verification: the window is only interesting while
            // tr.watcher is still None.
            {
                let ts = hub_for_hook.lock_track();
                in_window.store(
                    ts.tracked
                        .get("win001")
                        .is_some_and(|tr| tr.watcher.is_none()),
                    Ordering::SeqCst,
                );
            }
            raw_post(
                &authority,
                "/v1/ingest/cursor?slug=win001&event=beforeSubmitPrompt",
                &payload,
            );
        }));
    }

    let hh = Arc::clone(&h);
    tokio::task::spawn_blocking(move || hh.reconcile())
        .await
        .unwrap();
    assert!(
        fired.load(Ordering::SeqCst) && in_window.load(Ordering::SeqCst),
        "test premise: the hook must fire inside the construct→commit window"
    );

    // The post-commit drain took it: nothing stranded…
    assert_eq!(
        h.ingest.len(),
        0,
        "an event queued during the construct→commit window was left stranded"
    );
    // …and it folds on the next tick rather than waiting out the 60s TTL.
    h.reconcile();
    let rows = feed_rows_of(&h, "win001");
    assert_eq!(rows.len(), 1, "{rows:?}");
    assert_eq!(rows[0].text, "the kickoff prompt");
}

// Mirrors TestHubIngestCursorRefusedPushIsDroppedNotQueued: once a watcher
// EXISTS the queue is never used again — a refused push (closed watcher) is
// DROPPED.
#[tokio::test(flavor = "multi_thread")]
async fn ingest_cursor_refused_push_is_dropped_not_queued() {
    let (h, _f, url, _clk) = ingest_hub(&RcKind::Cursor, true).await;

    {
        let ts = h.lock_track();
        ts.tracked
            .get("ing001")
            .unwrap()
            .watcher
            .as_ref()
            .expect("cursor watcher built")
            .close();
    }
    let resp = post_hook(
        &url,
        "ing001",
        "beforeSubmitPrompt",
        &format!(r#"{{"session_id":"{CURSOR_SID}","prompt":"into the void"}}"#),
    )
    .await;
    assert_eq!(
        resp.status().as_u16(),
        202,
        "the hook script cannot act on anything else"
    );
    assert_eq!(
        h.ingest.len(),
        0,
        "an event refused by an existing watcher must be dropped, never queued"
    );
}

// Mirrors TestHubIngestCursorPreWatcherBoundsAndTTL.
#[tokio::test(flavor = "multi_thread")]
async fn ingest_cursor_pre_watcher_bounds_and_ttl() {
    let (h, f, url, clk) = ingest_hub(&RcKind::Cursor, false).await;
    h.reconcile();
    drop_watcher(&h, "ing001");

    for _ in 0..MAX_PRE_WATCHER_EVENTS + 10 {
        post_hook(&url, "ing001", "stop", r#"{"status":"completed"}"#).await;
    }
    assert_eq!(
        h.ingest.queued_events("ing001"),
        MAX_PRE_WATCHER_EVENTS,
        "the count bound"
    );

    // TTL: still no watcher after 60s → the whole queue drops.
    drop_watcher(&h, "ing001");
    clk.advance(PRE_WATCHER_TTL + Duration::from_secs(1));
    h.ingest
        .prune(clk.now(), &std::iter::once("ing001".to_string()).collect());
    assert_eq!(h.ingest.len(), 0, "a queue past the TTL drops wholesale");

    // A queue for a vanished slug drops on the next tick regardless of age.
    post_hook(&url, "ing001", "stop", r#"{"status":"completed"}"#).await;
    // (the watcher was dropped above, so the event queues again)
    f.remove("rc-ing001");
    h.reconcile();
    assert_eq!(h.ingest.len(), 0, "a queue for a vanished slug drops");
}

// Mirrors TestHubIngestCursorBackWritesAgentSession: reconcile back-writes
// SHED_RC_AGENT_SESSION from the hook stream; the same pin is not re-stamped;
// a re-pin stamps the new id and announces the switch in the feed.
#[tokio::test(flavor = "multi_thread")]
async fn ingest_cursor_back_writes_agent_session() {
    let (h, f, url, _clk) = ingest_hub(&RcKind::Cursor, true).await;
    let env_key = shed_core::rc_agents::ENV_AGENT_SESSION;

    post_hook(
        &url,
        "ing001",
        "sessionStart",
        &format!(r#"{{"session_id":"{CURSOR_SID}"}}"#),
    )
    .await;
    h.reconcile();
    assert_eq!(
        f.set_env_calls(),
        vec![format!("{env_key}={CURSOR_SID}")],
        "one back-write"
    );

    // The same id again is not re-stamped.
    post_hook(
        &url,
        "ing001",
        "stop",
        &format!(r#"{{"session_id":"{CURSOR_SID}","status":"completed"}}"#),
    )
    .await;
    h.reconcile();
    assert_eq!(
        f.set_env_calls().len(),
        1,
        "the repeated pin is not re-stamped"
    );

    // A different chat: re-pin, re-stamp, and a status row announcing it.
    const OTHER: &str = "9129668a-885b-48ef-b61b-d80f981d4d68";
    post_hook(
        &url,
        "ing001",
        "beforeSubmitPrompt",
        &format!(r#"{{"session_id":"{OTHER}","prompt":"new chat"}}"#),
    )
    .await;
    h.reconcile();
    let calls = f.set_env_calls();
    assert_eq!(calls.len(), 2, "{calls:?}");
    assert_eq!(calls[1], format!("{env_key}={OTHER}"));
    assert!(
        feed_rows_of(&h, "ing001")
            .iter()
            .any(|m| m.typ == "status" && m.text.contains("switched to another chat")),
        "a chat switch must be announced in the feed"
    );
}

// Mirrors TestHubIngestCursorMethodGate: a GET is a 405 from the mux, never a
// silent success.
#[tokio::test(flavor = "multi_thread")]
async fn ingest_cursor_method_gate() {
    let (_h, _f, url, _clk) = ingest_hub(&RcKind::Cursor, true).await;
    let status = get_status(&format!("{url}/v1/ingest/cursor?slug=ing001&event=stop")).await;
    assert_eq!(status, 405);
}

// H10 review MEDIUM pin: query params follow Go's url.Values.Get — the FIRST
// occurrence wins on duplicates (axum's Query<HashMap> was last-wins, which
// re-addressed a hook event and flipped precedence rows).
#[tokio::test(flavor = "multi_thread")]
async fn query_params_first_occurrence_wins() {
    // Ingest: ?slug=&slug=b → Go reads "" → 400 invalid_slug (never a lookup
    // on "b").
    let (_h, _f, url, _clk) = ingest_hub(&RcKind::Cursor, true).await;
    let resp = http_client()
        .post(format!(
            "{url}/v1/ingest/cursor?slug=&slug=ing001&event=stop"
        ))
        .body("{}")
        .send()
        .await
        .unwrap();
    assert_eq!(resp.status().as_u16(), 400, "first (empty) slug must win");
    // …and the first NON-empty one addresses the session.
    let resp = http_client()
        .post(format!(
            "{url}/v1/ingest/cursor?slug=ing001&slug=ghost&event=stop"
        ))
        .body("{}")
        .send()
        .await
        .unwrap();
    assert_eq!(
        resp.status().as_u16(),
        202,
        "the first slug addresses the session"
    );

    // Messages: ?since=1&since=2 pages from seq 1.
    let f = HubTmux::new();
    let clk = HubClock::new();
    let h = Arc::new(new_test_hub(&f, &clk));
    f.set(
        "rc-dupq11",
        &codex_ready_pane(),
        &managed_env("id-dq", &RcKind::Codex),
    );
    h.reconcile();
    {
        let ts = h.lock_track();
        let ring = &ts.tracked.get("dupq11").unwrap().ring;
        for _ in 0..3 {
            ring.append(text_msg("m"), clk.now());
        }
    }
    let (url2, _s) = serve_hub(&h).await;
    let body: super::messages::HubMessagesResponse = http_client()
        .get(format!(
            "{url2}/v1/sessions/dupq11/messages?since=1&since=2"
        ))
        .send()
        .await
        .unwrap()
        .json()
        .await
        .unwrap();
    let seqs: Vec<u64> = body.messages.iter().map(|m| m.seq).collect();
    assert_eq!(
        seqs,
        vec![2, 3],
        "paging starts after the FIRST since value"
    );
}

// H10 review LOW pin: net.SplitHostPort strips a bracketed host, so
// SHED_RC_HUB_ADDR=[127.0.0.1]:80 is accepted (loopback) on both sides — and
// H11 review LOW: it is NORMALIZED at acceptance, because the stored value is
// what std binds and dials, and neither accepts brackets around an IPv4
// literal.
#[test]
fn env_addr_accepts_bracketed_loopback() {
    let mut cfg = hub_config(&HubTmux::new(), &HubClock::new());
    let mut noted = false;
    let env = |k: &str| -> String {
        if k == ENV_HUB_ADDR {
            "[127.0.0.1]:80".into()
        } else {
            String::new()
        }
    };
    apply_hub_env_overrides(&mut cfg, &env, &mut |_| noted = true);
    assert!(!noted, "a bracketed loopback host must not be rejected");
    assert_eq!(cfg.addr, "127.0.0.1:80", "brackets stripped at acceptance");
    assert!(
        cfg.addr.parse::<std::net::SocketAddr>().is_ok(),
        "the stored addr must be bindable/dialable as-is"
    );
}

// ---------------------------------------------------------------------------
// The identity handshake CLIENT (`queryHubHealth`/`probeHubIdentity`,
// hub.go:916-956). These live here — beside the hub they probe — rather than in
// the host-agent bin, so `cargo test -p shed-broker` covers the client half of
// the frozen wire.
// ---------------------------------------------------------------------------

/// A canned-response HTTP holder: every accepted connection gets `response`
/// verbatim and is then closed. `query_hub_health` dials twice (the raw
/// identity dial, then the HTTP one), so it serves a few.
fn canned_health_server(response: String) -> String {
    let ln = std::net::TcpListener::bind("127.0.0.1:0").expect("bind");
    let addr = ln.local_addr().expect("addr").to_string();
    std::thread::spawn(move || {
        use std::io::Write;
        for conn in ln.incoming().flatten().take(4) {
            let mut conn = conn;
            let _ = conn.write_all(response.as_bytes());
        }
    });
    addr
}

fn http_200(body: &str) -> String {
    format!(
        "HTTP/1.1 200 OK\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{body}",
        body.len()
    )
}

/// A free loopback address with nothing listening on it.
fn dead_addr() -> String {
    let ln = std::net::TcpListener::bind("127.0.0.1:0").expect("bind");
    let addr = ln.local_addr().expect("addr").to_string();
    drop(ln);
    addr
}

// The three-way answer: a REAL hub → true, a listening non-hub → false ("it
// answered, but it is not us"), nothing listening → Err.
#[tokio::test(flavor = "multi_thread")]
async fn query_hub_health_classifies_hub_squatter_nothing() {
    let (url, _s) = serve_hub(&Arc::new(new_test_hub(&HubTmux::new(), &HubClock::new()))).await;
    let hub_addr = url.trim_start_matches("http://").to_string();
    let verdict = tokio::task::spawn_blocking(move || {
        query_hub_health(&hub_addr, Duration::from_secs(2)).unwrap()
    })
    .await
    .unwrap();
    assert!(verdict, "a real hub answers its own identity handshake");

    let squat = canned_health_server("not http at all\n".to_string());
    let verdict = tokio::task::spawn_blocking(move || {
        query_hub_health(&squat, Duration::from_secs(2)).unwrap()
    })
    .await
    .unwrap();
    assert!(!verdict, "a listening non-hub is Ok(false), never an error");

    let dead = dead_addr();
    let err = tokio::task::spawn_blocking(move || query_hub_health(&dead, Duration::from_secs(2)));
    assert!(
        err.await.unwrap().is_err(),
        "nothing listening is the error arm"
    );
}

// H11 review HIGH: the budget is ABSOLUTE. A holder dripping one byte per
// (timeout - epsilon) keeps every individual read "successful", so a per-read
// timeout would never fire — Go's http.Client Timeout covers the whole
// exchange, and so does ours.
#[test]
fn query_hub_health_honors_an_absolute_deadline_against_a_drip() {
    let ln = std::net::TcpListener::bind("127.0.0.1:0").expect("bind");
    let addr = ln.local_addr().expect("addr").to_string();
    std::thread::spawn(move || {
        use std::io::Write;
        for conn in ln.incoming().flatten().take(4) {
            std::thread::spawn(move || {
                let mut conn = conn;
                // FASTER than the whole budget (200ms drip vs a 250ms budget)
                // — that is the attack: every individual read completes, so a
                // per-read timeout re-arms forever. Capped at 20 drips (4s) so
                // the thread can never outlive the suite.
                for _ in 0..20 {
                    if conn.write_all(b"H").is_err() {
                        return;
                    }
                    let _ = conn.flush();
                    std::thread::sleep(Duration::from_millis(200));
                }
            });
        }
    });

    let budget = Duration::from_millis(250);
    let started = std::time::Instant::now();
    let verdict = query_hub_health(&addr, budget).expect("something IS listening");
    let elapsed = started.elapsed();
    assert!(!verdict, "a dripping holder is not a hub");
    // A per-read timeout would run the full 20-drip (4s) script here.
    assert!(
        elapsed < budget * 4,
        "the probe must respect its total budget (took {elapsed:?} of a {budget:?} budget)"
    );
}

// probe_hub_identity: a verified hub → Ok; a foreign listener → fast error;
// nothing listening → the budget is the ceiling, not a suggestion.
#[tokio::test(flavor = "multi_thread")]
async fn probe_hub_identity_verified_foreign_and_budgeted() {
    let (url, _s) = serve_hub(&Arc::new(new_test_hub(&HubTmux::new(), &HubClock::new()))).await;
    let hub_addr = url.trim_start_matches("http://").to_string();
    tokio::task::spawn_blocking(move || {
        probe_hub_identity(&hub_addr, Duration::from_secs(3)).expect("verified hub")
    })
    .await
    .unwrap();

    let squat = canned_health_server("HTTP/1.1 500 Nope\r\nContent-Length: 0\r\n\r\n".to_string());
    let err = tokio::task::spawn_blocking(move || {
        probe_hub_identity(&squat, Duration::from_secs(3)).unwrap_err()
    })
    .await
    .unwrap();
    assert!(err.contains("not a shed rc hub"), "{err}");

    let dead = dead_addr();
    let (err, elapsed) = tokio::task::spawn_blocking(move || {
        let started = std::time::Instant::now();
        let err = probe_hub_identity(&dead, Duration::from_millis(300)).unwrap_err();
        (err, started.elapsed())
    })
    .await
    .unwrap();
    assert!(err.contains("did not come up"), "{err}");
    assert!(
        elapsed < Duration::from_millis(1500),
        "the budget bounds the poll loop (took {elapsed:?})"
    );
}

// The probe decode is as tolerant as Go's `json.Decoder` + `LimitReader`:
// trailing bytes after the first JSON value are ignored, a chunked body is
// de-chunked, and `version`/`pid` are never load-bearing (absent, or negative
// — Go's `PID int` accepts both).
#[test]
fn probe_decode_tolerates_trailing_chunked_and_odd_pid() {
    let hub_body = r#"{"app":"shed-rc-hub","version":"t","pid":1}"#;
    let cells: Vec<(&str, String)> = vec![
        (
            "trailing garbage after the first value",
            http_200(&format!("{hub_body} NOT JSON AT ALL")),
        ),
        (
            "absent pid",
            http_200(r#"{"app":"shed-rc-hub","version":"t"}"#),
        ),
        (
            "negative pid",
            http_200(r#"{"app":"shed-rc-hub","version":"t","pid":-1}"#),
        ),
        (
            "absent version and pid",
            http_200(r#"{"app":"shed-rc-hub"}"#),
        ),
        ("chunked body", {
            let (a, b) = hub_body.split_at(10);
            format!(
                "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\nConnection: close\r\n\r\n\
                 {:x}\r\n{a}\r\n{:x}\r\n{b}\r\n0\r\n\r\n",
                a.len(),
                b.len()
            )
        }),
    ];
    for (name, response) in cells {
        let addr = canned_health_server(response);
        assert!(
            query_hub_health(&addr, Duration::from_secs(2)).unwrap(),
            "{name}: must still read as a hub"
        );
    }

    // …and the refusals stay refusals.
    for (name, response) in [
        (
            "non-200",
            "HTTP/1.1 503 Nope\r\nContent-Length: 0\r\n\r\n".to_string(),
        ),
        ("wrong app", http_200(r#"{"app":"something-else","pid":1}"#)),
        (
            "no body at all",
            "HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n".to_string(),
        ),
    ] {
        let addr = canned_health_server(response);
        assert!(
            !query_hub_health(&addr, Duration::from_secs(2)).unwrap(),
            "{name}: must not read as a hub"
        );
    }
}
