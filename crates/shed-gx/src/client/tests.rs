//! The client's tests: the pin gate, the error table row by row, every verb's
//! route and body, the answer translation table, and the sentinel-token audit
//! over every error path.

use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::Mutex;

use serde_json::json;

use shed_core::lane::{LaneApprovalOption, LaneDecision, LaneEvent, LaneQuestion};

use crate::discovery::{GxDiscovery, StaticCredentials};
use crate::testing::{CountingDial, FakeGx, SENTINEL_TOKEN};
use crate::transport::FixedDial;

use super::*;

const SID: &str = "01a0fa1e-0000-7000-8000-0000000000ab";

// ---------------------------------------------------------------------------
// counting doubles
// ---------------------------------------------------------------------------

/// A credential source that answers a scripted sequence, keeping the last
/// answer once the script runs out, and counts how many times it was asked.
struct ScriptedCreds {
    answers: Mutex<Vec<GxDiscovery>>,
    calls: AtomicUsize,
}

impl ScriptedCreds {
    fn new(instances: &[&str]) -> Arc<ScriptedCreds> {
        Arc::new(ScriptedCreds {
            answers: Mutex::new(
                instances
                    .iter()
                    .map(|i| GxDiscovery {
                        token: GxToken::parse(SENTINEL_TOKEN).expect("the sentinel parses"),
                        instance_id: (*i).to_string(),
                    })
                    .collect(),
            ),
            calls: AtomicUsize::new(0),
        })
    }

    fn calls(&self) -> usize {
        self.calls.load(Ordering::SeqCst)
    }
}

#[async_trait::async_trait]
impl GxCredentialSource for ScriptedCreds {
    async fn discover(&self, _reported_url: &str) -> Result<GxDiscovery, LaneError> {
        let n = self.calls.fetch_add(1, Ordering::SeqCst);
        let answers = self.answers.lock().expect("the script lock");
        Ok(answers[n.min(answers.len() - 1)].clone())
    }
}

// ---------------------------------------------------------------------------
// harness
// ---------------------------------------------------------------------------

/// A fake with one session, and a client pointed at it whose credentials
/// already match.
async fn wired() -> (FakeGx, GxClient) {
    let fake = FakeGx::start().await;
    fake.add_session(SID, Some("a session"), "/home/u/proj", "idle", 0, false);
    fake.pin(SID);
    let client = client_for(&fake);
    (fake, client)
}

/// A fake carrying the one standard session, pinned to it — the setup ten
/// tests were opening with verbatim. [`wired`] is this plus the default client;
/// the tests that build their own client (a counting transport, scripted
/// credentials, a wrong token) use this directly.
async fn staged() -> FakeGx {
    let fake = FakeGx::start().await;
    fake.add_session(SID, None, "/p", "idle", 0, false);
    fake.pin(SID);
    fake
}

fn client_for(fake: &FakeGx) -> GxClient {
    GxClient::new(
        fake.reported_url(),
        Arc::new(FixedDial::new(fake.dial_url())),
        Arc::new(
            StaticCredentials::from_parts(&fake.token(), &fake.instance_id())
                .expect("the fake's token is well formed"),
        ),
        GxTimings::default(),
    )
    .expect("the client builds")
}

// ---------------------------------------------------------------------------
// capabilities
// ---------------------------------------------------------------------------

#[tokio::test]
async fn capabilities_are_the_pinned_gx_row() {
    let (_fake, client) = wired().await;
    assert_eq!(
        client.capabilities(),
        LaneCapabilities {
            kind: "gx".to_string(),
            interject: true,
            create: true,
            cancel: true,
            approvals: true,
            history_cursor: true,
        }
    );
}

// ---------------------------------------------------------------------------
// the pin
// ---------------------------------------------------------------------------

#[tokio::test]
async fn healthz_precedes_the_first_bearer_request_of_the_clients_life() {
    let (fake, client) = wired().await;
    client.session(SID).await.expect("the row reads");

    let reqs = fake.requests();
    assert_eq!(reqs[0].path, "/v1/healthz", "{reqs:#?}");
    assert!(
        !reqs[0].had_bearer,
        "healthz is gx's ONE unauthenticated route and must carry no token"
    );
    assert!(reqs[1].had_bearer, "and the verb that follows does");
    assert_eq!(reqs[1].path, format!("/v1/sessions/{SID}"));
    assert!(fake.violations().is_empty(), "{:?}", fake.violations());

    // The epoch is pinned, so the second verb re-checks nothing.
    client.sessions().await.expect("the roster reads");
    assert_eq!(
        fake.requests()
            .iter()
            .filter(|r| r.path == "/v1/healthz")
            .count(),
        1,
        "one health check per epoch, not per request"
    );
    assert_eq!(
        client.pinned_instance().await.as_deref(),
        Some(fake.instance_id().as_str())
    );
}

#[tokio::test]
async fn concurrent_first_callers_share_one_pin() {
    let (fake, client) = wired().await;
    let client = Arc::new(client);

    let mut set = Vec::new();
    for _ in 0..8 {
        let c = Arc::clone(&client);
        set.push(tokio::spawn(async move { c.session(SID).await }));
    }
    for h in set {
        h.await.expect("the task joins").expect("the row reads");
    }

    assert_eq!(
        fake.requests()
            .iter()
            .filter(|r| r.path == "/v1/healthz")
            .count(),
        1,
        "eight concurrent first callers must share ONE pin, not race eight \
         health checks"
    );
    assert!(fake.violations().is_empty());
}

#[tokio::test]
async fn an_instance_mismatch_sends_no_token_and_rediscovers_exactly_once() {
    let fake = staged().await;
    // Both answers are wrong: the leader on that port is somebody else.
    let creds = ScriptedCreds::new(&["stale-a", "stale-b"]);
    let client = GxClient::new(
        fake.reported_url(),
        Arc::new(FixedDial::new(fake.dial_url())),
        Arc::clone(&creds) as Arc<dyn GxCredentialSource>,
        GxTimings::default(),
    )
    .expect("builds");

    let err = client
        .session(SID)
        .await
        .expect_err("refuses to send a token");
    match &err {
        LaneError::Unavailable(m) => {
            assert!(m.contains("instance changed"), "{m}");
            assert!(!m.contains(SENTINEL_TOKEN), "leaked: {m}");
        }
        other => panic!("expected a quiet Unavailable, got {other:?}"),
    }

    assert!(
        fake.bearer_requests().is_empty(),
        "NO bearer request may leave the adapter on a mismatch: {:?}",
        fake.bearer_requests()
    );
    assert_eq!(
        creds.calls(),
        2,
        "discovered, mismatched, rediscovered ONCE"
    );
    assert_eq!(
        client.pinned_instance().await,
        None,
        "the epoch stays unpinned so the next call discovers again"
    );

    // A second attempt discovers again rather than inheriting the refusal.
    let _ = client.session(SID).await;
    assert_eq!(creds.calls(), 4);
}

#[tokio::test]
async fn a_rediscovery_that_matches_pins_and_proceeds() {
    let fake = staged().await;
    // Stale first, correct second — a leader that restarted between the
    // client's last discovery and now.
    let creds = ScriptedCreds::new(&["stale", &fake.instance_id()]);
    let client = GxClient::new(
        fake.reported_url(),
        Arc::new(FixedDial::new(fake.dial_url())),
        Arc::clone(&creds) as Arc<dyn GxCredentialSource>,
        GxTimings::default(),
    )
    .expect("builds");

    client.session(SID).await.expect("the retry pins");
    assert_eq!(creds.calls(), 2);
    assert_eq!(
        fake.requests()
            .iter()
            .filter(|r| r.path == "/v1/healthz")
            .count(),
        1,
        "the rediscovery re-reads the RECORD, not the health check"
    );
}

#[tokio::test]
async fn dial_is_called_before_every_request() {
    let fake = staged().await;
    let dial = CountingDial::new(fake.dial_url());
    let client = GxClient::new(
        fake.reported_url(),
        Arc::clone(&dial) as Arc<dyn GxTransport>,
        Arc::new(StaticCredentials::from_parts(&fake.token(), &fake.instance_id()).expect("token")),
        GxTimings::default(),
    )
    .expect("builds");

    client.session(SID).await.expect("row");
    assert_eq!(dial.calls(), 1);
    client.sessions().await.expect("roster");
    client.cancel(SID).await.expect("cancel");
    assert_eq!(
        dial.calls(),
        3,
        "one dial per request — strictly more often than 'before every connect \
         attempt', and what makes a moved forward detectable when it moves"
    );
}

#[tokio::test]
async fn a_changed_dial_url_opens_a_new_epoch() {
    let first = staged().await;
    let second = FakeGx::start_with(&first.token(), "a-different-leader").await;
    second.add_session(SID, None, "/p", "working", 0, false);
    second.pin(SID);

    let dial = CountingDial::new(first.dial_url());
    // The credentials follow the SECOND leader — which is what a re-read
    // discovery record would say after a forward was re-established onto it.
    let creds = ScriptedCreds::new(&[&second.instance_id()]);
    let client = GxClient::new(
        first.reported_url(),
        Arc::clone(&dial) as Arc<dyn GxTransport>,
        Arc::clone(&creds) as Arc<dyn GxCredentialSource>,
        GxTimings::default(),
    )
    .expect("builds");

    // Against the first leader the instance does not match: unpinned.
    assert!(client.session(SID).await.is_err());
    assert!(first.bearer_requests().is_empty());

    // The forward moves. No `LaneEvent` says so — that is the whole point of
    // the hook.
    dial.point_at(second.dial_url());
    let row = client.session(SID).await.expect("the new epoch pins");
    assert_eq!(
        row.activity,
        RcActivity::Working,
        "it really is the second leader"
    );
    assert_eq!(
        client.pinned_instance().await.as_deref(),
        Some("a-different-leader")
    );
    assert_eq!(
        second
            .requests()
            .iter()
            .filter(|r| r.path == "/v1/healthz")
            .count(),
        1,
        "the new dial URL forced a fresh health check"
    );
}

#[tokio::test]
async fn an_unavailable_verb_unpins_so_the_next_call_re_checks() {
    let fake = staged().await;
    let client = client_for(&fake);
    client.session(SID).await.expect("pins");
    assert!(client.pinned_instance().await.is_some());

    // `leader_unavailable` is the quiet, retryable one, and it opens a new
    // epoch: the leader that answered healthz may not be the one that comes
    // back.
    fake.fail(
        "/v1/sessions",
        503,
        "leader_unavailable",
        "the leader is gone",
    );
    let err = client.sessions().await.expect_err("fails");
    assert!(matches!(err, LaneError::Unavailable(_)), "{err:?}");
    assert_eq!(client.pinned_instance().await, None);

    fake.clear_failures();
    client.sessions().await.expect("recovers");
    assert_eq!(
        fake.requests()
            .iter()
            .filter(|r| r.path == "/v1/healthz")
            .count(),
        2,
        "the new epoch health-checked again"
    );
}

/// **The epoch-lifecycle regression, on the ledger rather than on internals.**
///
/// A transport failure used to return past the invalidation (a `?` on the pin
/// and a `?` on `.send()` both bypassed it), leaving the PRIOR epoch pinned. The
/// next call matched the same dial URL, skipped `healthz` entirely, and sent the
/// token into what was effectively a new epoch — which over a forwarded
/// `127.0.0.1:<local>` is the whole exposure, because a leader restart or a
/// re-pointed tunnel changes what answers while the URL string stays identical.
#[tokio::test]
async fn a_transport_failure_unpins_so_the_next_call_health_checks_again() {
    let (fake, client) = wired().await;
    client.sessions().await.expect("pins");
    assert_eq!(healthz_count(&fake), 1);

    // The connection is accepted and then closed with no response at all —
    // a send failure, not an HTTP one.
    fake.hangup("/v1/sessions");
    let err = client.sessions().await.expect_err("the transport died");
    assert!(matches!(err, LaneError::Unavailable(_)), "{err:?}");

    fake.clear_transport_faults();
    client.sessions().await.expect("recovers");
    assert_eq!(
        healthz_count(&fake),
        2,
        "the failure opened a new epoch, so the next bearer request had to be \
         re-pinned first",
    );

    // And the ordering on the wire, not just the count: healthz precedes the
    // bearer request that follows the failure.
    let reqs = fake.requests();
    let last_health = reqs
        .iter()
        .rposition(|r| r.path == "/v1/healthz")
        .expect("a health check");
    let last_bearer = reqs
        .iter()
        .rposition(|r| r.had_bearer)
        .expect("a bearer request");
    assert!(last_health < last_bearer, "{reqs:#?}");
}

fn healthz_count(fake: &FakeGx) -> usize {
    fake.requests()
        .iter()
        .filter(|r| r.path == "/v1/healthz")
        .count()
}

/// A non-2xx whose body stalls or EOFs is a TRANSPORT failure, and used to be
/// laundered into a generic `Failed` built from an empty body — which also meant
/// the epoch was never invalidated.
#[tokio::test]
async fn a_non_2xx_whose_body_eofs_is_unavailable_and_unpins() {
    let (fake, client) = wired().await;
    client.sessions().await.expect("pins");
    assert_eq!(healthz_count(&fake), 1);

    // A complete 503 head promising 4 KiB, then the connection closes.
    fake.truncate_body("/v1/sessions", 503);
    let err = client.sessions().await.expect_err("the body never arrived");
    assert!(
        matches!(err, LaneError::Unavailable(_)),
        "a body that EOFs is a transport failure, not an application one: {err:?}",
    );

    fake.clear_transport_faults();
    client.sessions().await.expect("recovers");
    assert_eq!(healthz_count(&fake), 2, "and it opened a new epoch");
}

/// A leader that rotates behind an UNCHANGED dial URL, after a transient
/// failure, must not receive the token.
///
/// This is the case the whole pin exists for and the one the bug defeated: the
/// URL string is identical, so nothing but the `instanceId` comparison can
/// notice that the process on the far side is a different one.
#[tokio::test]
async fn a_rotation_behind_an_unchanged_url_after_a_failure_sends_no_token() {
    let fake = FakeGx::start().await;
    fake.add_session(SID, None, "/p", "idle", 0, false);
    fake.pin(SID);
    // Discovery keeps naming the ORIGINAL leader — a record not yet rewritten.
    let creds = ScriptedCreds::new(&[&fake.instance_id()]);
    let client = GxClient::new(
        fake.reported_url(),
        Arc::new(FixedDial::new(fake.dial_url())),
        Arc::clone(&creds) as Arc<dyn GxCredentialSource>,
        GxTimings::default(),
    )
    .expect("builds");

    client
        .sessions()
        .await
        .expect("pins against the first leader");

    // A transient failure, and then the leader restarts on the same port.
    fake.hangup("/v1/sessions");
    assert!(client.sessions().await.is_err());
    fake.clear_transport_faults();
    fake.set_instance_id("a-restarted-leader");
    // Counted AT the rotation: the attempts before it were addressed to the
    // leader that really was pinned, and are not what this test is about.
    let bearers_at_rotation = fake.bearer_requests().len();

    let err = client
        .sessions()
        .await
        .expect_err("the leader is not the one that was pinned");
    assert!(matches!(err, LaneError::Unavailable(_)), "{err:?}");
    assert_eq!(
        client.pinned_instance().await,
        None,
        "and it is left unpinned rather than proceeding",
    );

    // The load-bearing assertion: the token never reached the NEW leader.
    let after: Vec<_> = fake
        .bearer_requests()
        .into_iter()
        .skip(bearers_at_rotation)
        .collect();
    assert!(
        after.is_empty(),
        "no bearer request may follow the rotation — the health check has to \
         catch it first: {after:#?}",
    );
    // And it really did re-check, rather than failing for some other reason.
    assert!(healthz_count(&fake) >= 2);
}

#[tokio::test]
async fn a_dead_listener_is_quiet_and_names_what_was_tried() {
    let fake = FakeGx::start().await;
    let url = fake.dial_url();
    let token = fake.token();
    let instance = fake.instance_id();
    let reported = fake.reported_url();
    // Nothing is listening any more.
    drop(fake);

    let client = GxClient::new(
        reported,
        Arc::new(FixedDial::new(url)),
        Arc::new(StaticCredentials::from_parts(&token, &instance).expect("token")),
        GxTimings::default(),
    )
    .expect("builds");
    let err = client.sessions().await.expect_err("nothing to talk to");
    match &err {
        LaneError::Unavailable(m) => {
            assert!(m.contains("/v1/healthz"), "names what was tried: {m}");
            assert!(!m.contains(SENTINEL_TOKEN), "leaked: {m}");
        }
        other => panic!("a dial failure must be quiet, got {other:?}"),
    }
}

// ---------------------------------------------------------------------------
// the error table
// ---------------------------------------------------------------------------

#[tokio::test]
async fn the_error_table_maps_every_gx_code_row_by_row() {
    let rows: Vec<(&str, u16, LaneError)> = vec![
        ("unauthorized", 401, LaneError::Unauthorized),
        ("bad_request", 400, LaneError::BadRequest("why".to_string())),
        ("unknown_session", 404, LaneError::UnknownSession),
        ("unknown_approval", 404, LaneError::UnknownApproval),
        ("already_submitted", 409, LaneError::AlreadySubmitted),
        ("already_resolved", 409, LaneError::AlreadyResolved),
        ("not_accepting", 409, LaneError::NotAccepting),
        (
            "leader_unavailable",
            503,
            LaneError::Unavailable("leader_unavailable: why".to_string()),
        ),
    ];
    for (code, status, want) in rows {
        let fake = staged().await;
        let client = client_for(&fake);
        fake.fail("/v1/sessions", status, code, "why");
        let got = client.sessions().await.expect_err("the row fails");
        assert_eq!(got, want, "gx code {code}");
        assert!(fake.violations().is_empty());
    }
}

#[tokio::test]
async fn a_body_without_a_gx_envelope_is_the_loud_residue() {
    let fake = staged().await;
    let client = client_for(&fake);

    // A proxy's HTML, a 401 with no envelope, an empty body: all `Failed`,
    // carrying the status and a bounded head. A 401 is deliberately NOT
    // guessed into `Unauthorized` — the CODE decides, and there is none.
    for (status, body) in [
        (502u16, "<html><body>Bad gateway</body></html>"),
        (401, "not json"),
        (418, ""),
    ] {
        fake.clear_failures();
        fake.fail_raw("/v1/sessions", status, body);
        let err = client.sessions().await.expect_err("fails");
        match &err {
            LaneError::Failed(m) => {
                assert!(m.contains(&status.to_string()), "{m}");
                assert!(m.contains("/v1/sessions"), "{m}");
            }
            other => panic!("expected Failed for {status}, got {other:?}"),
        }
    }
}

#[test]
fn redact_hex64_catches_a_token_and_leaves_ordinary_prose_alone() {
    assert_eq!(
        redact_hex64(&format!("the token {SENTINEL_TOKEN} was rejected")),
        "the token <redacted> was rejected"
    );
    // Glued to hex on either end: still caught (the threshold is >= 64).
    assert_eq!(redact_hex64(&format!("ab{SENTINEL_TOKEN}cd")), "<redacted>");
    // Two of them.
    assert_eq!(
        redact_hex64(&format!("{SENTINEL_TOKEN} and {SENTINEL_TOKEN}")),
        "<redacted> and <redacted>"
    );
    // Ordinary prose, short hex, uppercase hex and non-ASCII are untouched.
    for plain in [
        "the session is not accepting that right now",
        "deadbeef",
        "facade00facade00facade00facade00",
        &SENTINEL_TOKEN.to_ascii_uppercase(),
        "переполнение — 200 ✓",
        "",
    ] {
        assert_eq!(redact_hex64(plain), plain, "{plain}");
    }
}

/// Redaction runs BEFORE truncation.
///
/// Cutting first leaves a token that straddles the 200-character boundary as a
/// partial run shorter than the redactor's 64-character threshold — so filler
/// followed by the token used to leave the token's head sitting in the error,
/// unscrubbed.
#[test]
fn a_token_straddling_the_truncation_boundary_is_still_redacted() {
    // 150 characters of filler, then the token: the cut lands ~50 characters
    // into it.
    let body = format!("{}{SENTINEL_TOKEN}", "z".repeat(150));
    let err = map_gx_error(reqwest::StatusCode::BAD_GATEWAY, "/v1/sessions", &body);
    let LaneError::Failed(m) = err else {
        panic!("expected Failed");
    };
    assert!(!m.contains(SENTINEL_TOKEN), "{m}");
    // No long fragment of it survives either — the real failure mode, since a
    // partial token is still a partial secret.
    for len in [40usize, 30, 20] {
        assert!(
            !m.contains(&SENTINEL_TOKEN[..len]),
            "a {len}-character fragment of the token survived: {m}",
        );
    }
    assert!(m.contains("<redacted>"), "{m}");
}

/// A server-supplied identifier that IS the token must not reach a `LaneError`
/// through an interpolated path.
///
/// A hostile gx returns the bearer as a `sessionId`; a later malformed response
/// then yields `decoding /v1/sessions/<token>: …`. "The server already knows the
/// token" is true and beside the point — the damage is that WE write it into a
/// log that outlives the request.
#[tokio::test]
async fn a_token_echoed_back_as_an_identifier_never_reaches_an_error_string() {
    let (fake, client) = wired().await;
    // The session id IS the token, and the route answers something undecodable.
    fake.add_session(SENTINEL_TOKEN, None, "/p", "idle", 0, false);
    fake.pin(SENTINEL_TOKEN);
    fake.fail_raw(SENTINEL_TOKEN, 200, "{not json");

    let err = client
        .session(SENTINEL_TOKEN)
        .await
        .expect_err("the body does not decode");
    let rendered = format!("{err}{err:?}");
    assert!(
        !rendered.contains(SENTINEL_TOKEN),
        "the token reached a LaneError through the path: {rendered}",
    );
    assert!(rendered.contains("<redacted>"), "{rendered}");
}

#[test]
fn map_gx_error_bounds_the_body_head_it_echoes() {
    let long = "x".repeat(10_000);
    let err = map_gx_error(
        reqwest::StatusCode::BAD_GATEWAY,
        "/v1/sessions",
        &format!("{long}\nand a second line"),
    );
    let LaneError::Failed(m) = err else {
        panic!("expected Failed");
    };
    assert!(
        m.len() < 400,
        "a log line, not a page of HTML: {} bytes",
        m.len()
    );
    assert!(!m.contains("second line"), "only the FIRST line");
}

// ---------------------------------------------------------------------------
// activity and rows
// ---------------------------------------------------------------------------

#[test]
fn gxs_six_activity_states_map_per_correction_six() {
    assert_eq!(activity_from("working", 0), RcActivity::Working);
    // `needs_input` splits on whether anything is actually pending.
    assert_eq!(activity_from("needs_input", 0), RcActivity::NeedsInput);
    assert_eq!(activity_from("needs_input", 2), RcActivity::NeedsApproval);
    for idle in ["idle", "completed", "dormant"] {
        assert_eq!(activity_from(idle, 0), RcActivity::Idle, "{idle}");
    }
    // `dead` and anything this build has never heard of collapse to Unknown
    // rather than to a claim.
    assert_eq!(activity_from("dead", 0), RcActivity::Unknown);
    assert_eq!(activity_from("a_state_from_2027", 0), RcActivity::Unknown);
    assert_eq!(activity_from("", 0), RcActivity::Unknown);
}

#[test]
fn a_lane_held_approval_overrides_the_row_upward_and_never_downward() {
    let row: GxSessionRow = serde_json::from_value(json!({
        "sessionId": SID,
        "title": null,
        "cwd": "/p",
        "activity": "working",
        "pendingApprovals": 0,
        "approximate": true,
        "lastChangeUnixMs": 1_788_931_056_811i64,
    }))
    .expect("decodes");

    let plain = lane_session(&row, 0);
    assert_eq!(plain.activity, RcActivity::Working);
    assert_eq!(plain.pending_approvals, 0);
    assert_eq!(
        plain.title, "",
        "a null title decodes to empty, not a failure"
    );
    assert!(plain.approximate);
    assert_eq!(plain.parent_id, None, "gx's sessions are flat");
    assert_eq!(plain.last_change_unix_ms, Some(1_788_931_056_811));

    // The fold knows about an approval the roster poll does not.
    let held = lane_session(&row, 2);
    assert_eq!(held.activity, RcActivity::NeedsApproval);
    assert_eq!(held.pending_approvals, 2);

    // And never downward: a lane that has only just attached has seen nothing,
    // and that does not disprove what gx reports.
    let row_with_three: GxSessionRow = serde_json::from_value(json!({
        "sessionId": SID, "cwd": "/p", "activity": "needs_input",
        "pendingApprovals": 3, "approximate": false,
    }))
    .expect("decodes");
    let seen_nothing = lane_session(&row_with_three, 0);
    assert_eq!(seen_nothing.pending_approvals, 3);
    assert_eq!(seen_nothing.activity, RcActivity::NeedsApproval);
    assert!(
        !seen_nothing.approximate,
        "gx is the first producer to report false"
    );
}

// ---------------------------------------------------------------------------
// the verbs
// ---------------------------------------------------------------------------

#[tokio::test]
async fn sessions_and_session_read_the_documented_routes() {
    let (fake, client) = wired().await;
    fake.add_session("other", Some("second"), "/q", "needs_input", 1, true);
    // The pin is on SID, so the roster (a GLOBAL route) is still allowed.
    let all = client.sessions().await.expect("roster");
    assert_eq!(all.len(), 2);
    assert_eq!(all[0].id, SID);
    assert_eq!(all[0].title, "a session");
    assert_eq!(all[1].activity, RcActivity::NeedsApproval);

    let one = client.session(SID).await.expect("row");
    assert_eq!(one.cwd, "/home/u/proj");

    let paths: Vec<String> = fake.requests().iter().map(|r| r.path.clone()).collect();
    assert!(paths.contains(&"/v1/sessions".to_string()));
    assert!(paths.contains(&format!("/v1/sessions/{SID}")));
    assert!(fake.violations().is_empty());
}

/// `n` envelopes, counters ascending from `start`, one text chunk each.
fn history_envelopes(start: u64, n: u64) -> Vec<serde_json::Value> {
    (0..n)
        .map(|i| {
            json!({
                "eventId": format!("{SID}-{}", start + i),
                "method": "session/update",
                "params": {
                    "sessionId": SID,
                    "update": {
                        "sessionUpdate": "agent_message_chunk",
                        "content": { "type": "text", "text": format!("chunk {}", start + i) },
                    },
                    "_meta": { "agentTimestampMs": 1_788_927_600_000i64, "promptId": format!("p{i}") },
                },
                "timestamp": 1_788_927_600i64,
            })
        })
        .collect()
}

#[tokio::test]
async fn history_without_a_cursor_asks_for_the_tail_and_flushes_the_final_streak() {
    let (fake, client) = wired().await;
    fake.set_history(SID, history_envelopes(100, 30));

    let page = client.history(SID, None, 10).await.expect("history");
    assert_eq!(
        fake.requests()
            .iter()
            .find(|r| r.path.ends_with("/history"))
            .map(|r| r.query.clone())
            .unwrap_or_default(),
        "offset=-10&limit=10",
        "the tail is asked for with a NEGATIVE offset"
    );
    assert!(
        page.truncated,
        "hasMore was true — there is history before this page"
    );
    // Each envelope has its own promptId, so each is its own row; the LAST one
    // is only present because a standalone page flushes its open streak.
    assert_eq!(page.messages.len(), 10, "{:#?}", page.messages);
    assert_eq!(
        page.messages.last().unwrap().text.as_deref(),
        Some("chunk 129")
    );
    assert_eq!(page.cursor.as_deref(), Some(&*format!("{SID}-129")));
    assert!(fake.violations().is_empty());
}

#[tokio::test]
async fn history_with_a_cursor_cuts_positionally_at_it() {
    let (fake, client) = wired().await;
    fake.set_history(SID, history_envelopes(100, 30));

    let page = client
        .history(SID, Some(&format!("{SID}-125")), 100)
        .await
        .expect("history");
    assert_eq!(
        page.messages.len(),
        4,
        "everything AFTER the cursor's envelope: 126..129"
    );
    assert_eq!(page.messages[0].text.as_deref(), Some("chunk 126"));
    assert!(
        !page.truncated,
        "the cursor was located, so nothing is missing"
    );
}

#[tokio::test]
async fn a_cursor_that_cannot_be_located_falls_back_to_the_tail_and_says_truncated() {
    let (fake, client) = wired().await;
    fake.set_history(SID, history_envelopes(100, 30));

    for cursor in [
        // Foreign to this session — exactly what gx answers
        // `cursor_unresolvable` to.
        "some-other-session-125".to_string(),
        // Malformed.
        "not-an-event-id".to_string(),
        // Older than anything the transcript still holds.
        format!("{SID}-1"),
    ] {
        let page = client
            .history(SID, Some(&cursor), 5)
            .await
            .expect("history still answers");
        assert!(page.truncated, "{cursor}: refetch, do not splice");
        assert!(!page.messages.is_empty(), "{cursor}");
    }
}

#[tokio::test]
async fn a_cursor_beyond_the_page_budget_gives_up_rather_than_paging_forever() {
    let (fake, client) = wired().await;
    // Deeper than 10 pages of 200.
    fake.set_history(SID, history_envelopes(1, 2_400));

    let page = client
        .history(SID, Some(&format!("{SID}-2")), 5)
        .await
        .expect("history");
    assert!(page.truncated);
    let history_reads = fake
        .requests()
        .iter()
        .filter(|r| r.path.ends_with("/history"))
        .count();
    assert!(
        history_reads <= MAX_HISTORY_PAGES + 1,
        "the hunt is bounded at {MAX_HISTORY_PAGES} pages plus the tail \
         fallback, saw {history_reads}"
    );
}

#[tokio::test]
async fn create_posts_cwd_and_text_then_reads_the_created_row() {
    let (fake, client) = wired().await;
    let row = client
        .create("/home/u/new", "summarize the diff")
        .await
        .expect("create");
    assert!(row.id.starts_with("01a0fake"));

    let body = fake.body_of("/v1/sessions").expect("the create body");
    let parsed: serde_json::Value = serde_json::from_str(&body).expect("json");
    assert_eq!(parsed["cwd"], "/home/u/new");
    assert_eq!(parsed["text"], "summarize the diff");
    assert!(fake.violations().is_empty(), "create is a GLOBAL route");
}

#[tokio::test]
async fn send_carries_the_mode_and_a_refusal_surfaces_as_not_accepting() {
    let (fake, client) = wired().await;

    client
        .send(SID, "hello", SendMode::Queue)
        .await
        .expect("queue");
    let body: serde_json::Value =
        serde_json::from_str(&fake.body_of("/messages").expect("body")).expect("json");
    assert_eq!(body["text"], "hello");
    assert_eq!(body["mode"], "queue");

    let fake2 = staged().await;
    let client2 = client_for(&fake2);
    client2
        .send(SID, "now", SendMode::Interject)
        .await
        .expect("interject");
    let body: serde_json::Value =
        serde_json::from_str(&fake2.body_of("/messages").expect("body")).expect("json");
    assert_eq!(body["mode"], "interject");

    // gx refuses an interject unless the session is working.
    fake2.fail("/messages", 409, "not_accepting", "the session is idle");
    assert_eq!(
        client2
            .send(SID, "again", SendMode::Interject)
            .await
            .expect_err("refused"),
        LaneError::NotAccepting
    );
}

#[tokio::test]
async fn cancel_posts_the_route_and_surfaces_a_refusal_rather_than_swallowing_it() {
    let (fake, client) = wired().await;
    client.cancel(SID).await.expect("cancel");
    assert!(fake
        .requests()
        .iter()
        .any(|r| r.method == "POST" && r.path == format!("/v1/sessions/{SID}/cancel")));

    // Correction 8: a refused cancel is NotAccepting, never a silent Ok.
    fake.fail("/cancel", 409, "not_accepting", "nothing to cancel");
    assert_eq!(
        client.cancel(SID).await.expect_err("refused"),
        LaneError::NotAccepting
    );
}

#[tokio::test]
async fn approvals_drops_resolved_and_keeps_submitted() {
    let (fake, client) = wired().await;
    for id in ["tc-1", "tc-2", "tc-3"] {
        fake.add_approval(
            SID,
            id,
            "permission",
            "session/request_permission",
            &permission_request(),
        );
    }
    fake.resolve_approval(SID, "tc-3");

    // tc-2 gets an answer, so it becomes `submitted`.
    client
        .answer(
            SID,
            "tc-2",
            LaneAnswer::Permission {
                decision: LaneDecision::AllowOnce,
            },
        )
        .await
        .expect("answers");

    let open = client.approvals(SID).await.expect("approvals");
    let ids: Vec<&str> = open.iter().map(|a| a.id.as_str()).collect();
    assert_eq!(
        ids,
        vec!["tc-1", "tc-2"],
        "resolved is history; submitted is the optimistic state a client should \
         still see"
    );
    assert_eq!(open[1].status, LaneApprovalStatus::Submitted);
    assert!(fake.violations().is_empty());
}

fn permission_request() -> serde_json::Value {
    json!({
        "toolCall": { "toolCallId": "tc", "title": "rm -rf build/", "rawInput": { "command": "rm -rf build/" } },
        "options": [
            { "optionId": "p-1", "name": "Allow once", "kind": "allow_once" },
            { "optionId": "p-2", "name": "Always", "kind": "allow_always" },
            { "optionId": "p-3", "name": "Reject", "kind": "reject_once" },
            { "optionId": "p-4", "name": "Never", "kind": "reject_always" },
        ],
    })
}

/// The option set a **live gx leader** really offers, recorded off one
/// `session/request_permission` for `id -un`.
///
/// Five options, and **two of them declare `allow_once`** — the ordinary "Yes,
/// proceed" and an always-approve switch that stops the agent asking about
/// anything for the rest of the session. gx's own generic vocabulary; no user
/// data.
fn gx_real_permission_request() -> serde_json::Value {
    json!({
        "toolCall": { "toolCallId": "tc", "title": "id -un", "rawInput": { "command": "id -un" } },
        "options": [
            { "optionId": "enable-always-approve", "name": "Yes, and don't ask again for anything (always-approve mode)", "kind": "allow_once" },
            { "optionId": "allow-always-command", "name": "Always allow: id -un", "kind": "allow_always" },
            { "optionId": "allow-once", "name": "Yes, proceed", "kind": "allow_once" },
            { "optionId": "reject-once", "name": "No, and tell Grok what to do differently", "kind": "reject_once" },
            { "optionId": "reject-always-command", "name": "Never allow: id -un", "kind": "reject_always" },
        ],
    })
}

/// The escalation, refused end-to-end: over the wire, against the real option
/// set, nothing is posted at all.
///
/// The contract's resolver is what refuses ([`LaneApproval::option_for`]); this
/// asserts the ADAPTER surfaces that as `BadRequest` and — the part only a fake
/// can prove — that no answer reached the server. A `Choice` naming the option
/// the human actually pressed still works, which is what makes refusing cheap.
#[tokio::test]
async fn a_decision_against_two_allow_once_options_is_refused_and_nothing_is_posted() {
    let (fake, client) = wired().await;
    fake.add_approval(
        SID,
        "tc-1",
        "permission",
        "session/request_permission",
        &gx_real_permission_request(),
    );

    let err = client
        .answer(
            SID,
            "tc-1",
            LaneAnswer::Permission {
                decision: LaneDecision::AllowOnce,
            },
        )
        .await
        .expect_err("two allow_once options cannot be told apart by a decision");
    match &err {
        LaneError::BadRequest(m) => {
            // The message names the real cause and both candidates, because
            // "offered none" and "offered two" call for different responses.
            assert!(m.contains("2 options of kind allow_once"), "{m}");
            assert!(m.contains("enable-always-approve"), "{m}");
            assert!(m.contains("allow-once"), "{m}");
        }
        other => panic!("expected BadRequest, got {other:?}"),
    }
    assert_eq!(
        fake.answered_with(SID, "tc-1"),
        None,
        "nothing may be posted — picking either option would be a guess at a \
         privilege escalation",
    );

    // The unambiguous decisions on the SAME approval still resolve, so the
    // refusal is scoped to the ambiguity rather than to the approval.
    client
        .answer(
            SID,
            "tc-1",
            LaneAnswer::Permission {
                decision: LaneDecision::AllowAlways,
            },
        )
        .await
        .expect("allow_always is unique");
    assert_eq!(
        fake.answered_with(SID, "tc-1").expect("answered"),
        json!({ "outcome": { "outcome": "selected", "optionId": "allow-always-command" } })
    );

    // And the way out for a human: name the option. A capability-driven panel
    // sends this anyway, which is why refusing above costs it nothing.
    let fake2 = FakeGx::start().await;
    fake2.add_session(SID, None, "/p", "needs_input", 1, false);
    fake2.pin(SID);
    let client2 = client_for(&fake2);
    fake2.add_approval(
        SID,
        "tc-1",
        "permission",
        "session/request_permission",
        &gx_real_permission_request(),
    );
    client2
        .answer(
            SID,
            "tc-1",
            LaneAnswer::Choice {
                option_id: "allow-once".to_string(),
            },
        )
        .await
        .expect("an offered id is unambiguous by construction");
    assert_eq!(
        fake2.answered_with(SID, "tc-1").expect("answered"),
        json!({ "outcome": { "outcome": "selected", "optionId": "allow-once" } }),
        "the option the human actually pressed, not the escalating one",
    );
    assert!(fake.violations().is_empty() && fake2.violations().is_empty());
}

/// The re-read's identity is verified before its option set is used.
///
/// The whole point of `GET …/approvals/{id}` immediately before answering is
/// race-safety — but the response was never checked against what was asked for.
/// A server answering with a DIFFERENT resource meant the option set came from
/// Y while the chosen `optionId` was POSTed to X: answering one approval with
/// another's option, and nothing downstream could detect it.
#[tokio::test]
async fn answering_refuses_a_re_read_that_is_not_the_approval_asked_for() {
    // A different tool call id.
    let (fake, client) = wired().await;
    fake.add_approval(
        SID,
        "tc-1",
        "permission",
        "session/request_permission",
        &permission_request(),
    );
    fake.misdirect_approval(SID, "tc-1", "tc-99", SID);
    let err = client
        .answer(
            SID,
            "tc-1",
            LaneAnswer::Choice {
                option_id: "p-1".to_string(),
            },
        )
        .await
        .expect_err("the server answered with another approval");
    match &err {
        LaneError::Failed(m) => assert!(m.contains("tc-99"), "{m}"),
        other => panic!("expected Failed, got {other:?}"),
    }
    assert_eq!(
        fake.answered_with(SID, "tc-1"),
        None,
        "nothing may be posted when the re-read is not the approval asked for",
    );

    // A different SESSION's approval, served at this session's address.
    let fake2 = FakeGx::start().await;
    fake2.add_session(SID, None, "/p", "needs_input", 1, false);
    fake2.pin(SID);
    let client2 = client_for(&fake2);
    fake2.add_approval(
        SID,
        "tc-1",
        "permission",
        "session/request_permission",
        &permission_request(),
    );
    fake2.misdirect_approval(SID, "tc-1", "tc-1", "some-other-session");
    let err = client2
        .answer(
            SID,
            "tc-1",
            LaneAnswer::Choice {
                option_id: "p-1".to_string(),
            },
        )
        .await
        .expect_err("the approval belongs to another session");
    match &err {
        LaneError::Failed(m) => assert!(m.contains("some-other-session"), "{m}"),
        other => panic!("expected Failed, got {other:?}"),
    }
    assert_eq!(fake2.answered_with(SID, "tc-1"), None);
}

#[tokio::test]
async fn answer_re_reads_the_authoritative_request_before_translating() {
    let (fake, client) = wired().await;
    fake.add_approval(
        SID,
        "tc-1",
        "permission",
        "session/request_permission",
        &permission_request(),
    );

    client
        .answer(
            SID,
            "tc-1",
            LaneAnswer::Permission {
                decision: LaneDecision::AllowAlways,
            },
        )
        .await
        .expect("answers");

    let methods: Vec<String> = fake
        .requests()
        .iter()
        .filter(|r| r.path.ends_with("/approvals/tc-1"))
        .map(|r| r.method.clone())
        .collect();
    assert_eq!(
        methods,
        vec!["GET".to_string(), "POST".to_string()],
        "the authoritative re-read comes FIRST, then the answer"
    );

    // The option was chosen by its KIND, and its id is unrelated to that kind.
    assert_eq!(
        fake.answered_with(SID, "tc-1").expect("answered"),
        json!({ "outcome": { "outcome": "selected", "optionId": "p-2" } })
    );
}

#[tokio::test]
async fn a_second_answer_is_refused_by_gx_rather_than_by_a_local_ledger() {
    let (fake, client) = wired().await;
    fake.add_approval(
        SID,
        "tc-1",
        "permission",
        "session/request_permission",
        &permission_request(),
    );

    client
        .answer(
            SID,
            "tc-1",
            LaneAnswer::Choice {
                option_id: "p-1".to_string(),
            },
        )
        .await
        .expect("first answer");
    assert_eq!(
        client
            .answer(
                SID,
                "tc-1",
                LaneAnswer::Choice {
                    option_id: "p-1".to_string()
                }
            )
            .await
            .expect_err("second"),
        LaneError::AlreadySubmitted
    );

    fake.resolve_approval(SID, "tc-1");
    assert_eq!(
        client
            .answer(
                SID,
                "tc-1",
                LaneAnswer::Choice {
                    option_id: "p-1".to_string()
                }
            )
            .await
            .expect_err("third"),
        LaneError::AlreadyResolved,
        "the TUI answering first is a race gx reports, not an error to retry"
    );
}

#[tokio::test]
async fn answering_an_approval_that_is_gone_is_unknown_approval() {
    let (_fake, client) = wired().await;
    assert_eq!(
        client
            .answer(SID, "tc-absent", LaneAnswer::Reject)
            .await
            .expect_err("gone"),
        LaneError::UnknownApproval
    );
}

// ---------------------------------------------------------------------------
// the answer translation table (pure)
// ---------------------------------------------------------------------------

fn approval(kind: LaneApprovalKind, options: Vec<LaneApprovalOption>) -> LaneApproval {
    LaneApproval {
        id: "tc-1".to_string(),
        session_id: SID.to_string(),
        kind,
        status: LaneApprovalStatus::Pending,
        title: "t".to_string(),
        detail: None,
        options,
        questions: Vec::new(),
        request_json: "{}".to_string(),
        created_at_unix_ms: None,
    }
}

fn opt(id: &str, kind: Option<&str>) -> LaneApprovalOption {
    LaneApprovalOption {
        id: id.to_string(),
        label: id.to_string(),
        description: None,
        kind: kind.map(str::to_string),
    }
}

#[test]
fn a_permission_answer_selects_by_kind_and_never_by_id() {
    let a = approval(
        LaneApprovalKind::Permission,
        vec![
            opt("p-1", Some("allow_once")),
            opt("p-2", Some("allow_always")),
            opt("p-3", Some("reject_once")),
        ],
    );
    for (decision, want) in [
        (LaneDecision::AllowOnce, "p-1"),
        (LaneDecision::AllowAlways, "p-2"),
        (LaneDecision::Reject, "p-3"),
    ] {
        assert_eq!(
            answer_body(&a, &LaneAnswer::Permission { decision }).expect("translates"),
            json!({ "outcome": { "outcome": "selected", "optionId": want } })
        );
    }

    // The Reject fallback — and only Reject: an agent offering nothing but
    // `reject_always` must still be refusable, while silently upgrading an
    // "allow once" into an "allow always" is the bug the contract exists to
    // prevent.
    let only_always = approval(
        LaneApprovalKind::Permission,
        vec![opt("x", Some("reject_always"))],
    );
    assert_eq!(
        answer_body(
            &only_always,
            &LaneAnswer::Permission {
                decision: LaneDecision::Reject
            }
        )
        .expect("falls back"),
        json!({ "outcome": { "outcome": "selected", "optionId": "x" } })
    );
    let err = answer_body(
        &only_always,
        &LaneAnswer::Permission {
            decision: LaneDecision::AllowOnce,
        },
    )
    .expect_err("no allow option");
    assert!(matches!(err, LaneError::BadRequest(_)), "{err:?}");

    // An option with no kind never matches: an absent kind means "this agent
    // states no semantics", and guessing one from the id is the id-sniffing
    // the rule replaces.
    let kindless = approval(LaneApprovalKind::Permission, vec![opt("allow-once", None)]);
    assert!(answer_body(
        &kindless,
        &LaneAnswer::Permission {
            decision: LaneDecision::AllowOnce
        }
    )
    .is_err());
}

#[test]
fn a_choice_must_name_an_offered_id() {
    let a = approval(
        LaneApprovalKind::Permission,
        vec![
            opt("p-1", Some("allow_once")),
            opt("p-2", Some("reject_once")),
        ],
    );
    assert_eq!(
        answer_body(
            &a,
            &LaneAnswer::Choice {
                option_id: "p-2".to_string()
            }
        )
        .expect("offered"),
        json!({ "outcome": { "outcome": "selected", "optionId": "p-2" } })
    );
    let err = answer_body(
        &a,
        &LaneAnswer::Choice {
            option_id: "p-9".to_string(),
        },
    )
    .expect_err("not offered");
    assert!(matches!(err, LaneError::BadRequest(_)), "{err:?}");

    // A plan approval takes a BARE string outcome.
    let plan = approval(
        LaneApprovalKind::PlanApproval,
        vec![
            opt("approved", Some("allow_once")),
            opt("cancelled", Some("reject_once")),
        ],
    );
    assert_eq!(
        answer_body(
            &plan,
            &LaneAnswer::Choice {
                option_id: "approved".to_string()
            }
        )
        .expect("approves"),
        json!({ "outcome": "approved" }),
        "\"approved\", never \"approve\""
    );
    assert_eq!(
        answer_body(
            &plan,
            &LaneAnswer::Choice {
                option_id: "cancelled".to_string()
            }
        )
        .expect("cancels"),
        json!({ "outcome": "cancelled" })
    );
    assert!(answer_body(
        &plan,
        &LaneAnswer::Choice {
            option_id: "abandoned".to_string()
        }
    )
    .is_err());
}

#[test]
fn a_question_answer_is_positional_in_and_keyed_by_text_out() {
    let mut a = approval(LaneApprovalKind::Question, Vec::new());
    a.questions = vec![
        LaneQuestion {
            id: Some("Which database?".to_string()),
            header: String::new(),
            question: "Which database?".to_string(),
            options: vec![opt("Postgres", None), opt("Redis", None)],
            multiple: false,
            custom: false,
        },
        LaneQuestion {
            id: Some("Which caches?".to_string()),
            header: String::new(),
            question: "Which caches?".to_string(),
            options: vec![opt("None", None), opt("Redis", None)],
            multiple: true,
            custom: false,
        },
    ];

    let body = answer_body(
        &a,
        &LaneAnswer::Question {
            answers: vec![
                vec!["Postgres".to_string()],
                vec!["None".to_string(), "Redis".to_string()],
            ],
        },
    )
    .expect("translates");
    assert_eq!(
        body,
        json!({
            "outcome": "accepted",
            "answers": {
                "Which database?": ["Postgres"],
                "Which caches?": ["None", "Redis"],
            },
        }),
        "keyed by the question's TEXT — what gx's own TUI files answers under"
    );

    // Fewer answers than questions is allowed (the rest are unanswered); more
    // is a mistake worth refusing.
    assert!(answer_body(
        &a,
        &LaneAnswer::Question {
            answers: vec![vec!["Postgres".to_string()]]
        }
    )
    .is_ok());
    let err = answer_body(
        &a,
        &LaneAnswer::Question {
            answers: vec![vec![], vec![], vec![]],
        },
    )
    .expect_err("too many");
    assert!(matches!(err, LaneError::BadRequest(_)), "{err:?}");
}

#[test]
fn reject_declines_each_kind_the_way_that_kind_accepts() {
    assert_eq!(
        answer_body(
            &approval(LaneApprovalKind::Permission, vec![]),
            &LaneAnswer::Reject
        )
        .expect("declines"),
        json!({ "outcome": { "outcome": "cancelled" } })
    );
    assert_eq!(
        answer_body(
            &approval(LaneApprovalKind::McpElicitation, vec![]),
            &LaneAnswer::Reject
        )
        .expect("declines"),
        json!({ "outcome": "decline" })
    );
    for kind in [
        LaneApprovalKind::Question,
        LaneApprovalKind::PlanApproval,
        LaneApprovalKind::Other("placeholder".to_string()),
    ] {
        assert_eq!(
            answer_body(&approval(kind.clone(), vec![]), &LaneAnswer::Reject).expect("declines"),
            json!({ "outcome": "cancelled" }),
            "{kind:?}"
        );
    }
}

#[test]
fn a_raw_answer_is_verbatim_and_must_be_json() {
    let a = approval(LaneApprovalKind::Other("mystery".to_string()), vec![]);
    assert_eq!(
        answer_body(
            &a,
            &LaneAnswer::Raw {
                json: r#"{"outcome":"accept","content":{"email":"me@x"}}"#.to_string()
            }
        )
        .expect("verbatim"),
        json!({ "outcome": "accept", "content": { "email": "me@x" } })
    );
    let err = answer_body(
        &a,
        &LaneAnswer::Raw {
            json: "not json".to_string(),
        },
    )
    .expect_err("refused");
    assert!(matches!(err, LaneError::BadRequest(_)), "{err:?}");
}

// ---------------------------------------------------------------------------
// the sentinel audit
// ---------------------------------------------------------------------------

#[tokio::test]
async fn the_token_never_appears_in_a_debug_string_or_on_any_error_path() {
    let fake = staged().await;
    let client = client_for(&fake);

    assert!(
        !format!("{client:?}").contains(SENTINEL_TOKEN),
        "GxClient's Debug leaked: {client:?}"
    );
    assert!(
        !format!("{:?}", client.timings()).contains(SENTINEL_TOKEN),
        "GxTimings leaked"
    );

    // Every error the client can produce, given a wire that is trying to make
    // it talk about the request it sent.
    let mut errors: Vec<LaneError> = Vec::new();
    for (status, code) in [
        (401u16, "unauthorized"),
        (400, "bad_request"),
        (404, "unknown_session"),
        (409, "not_accepting"),
        (503, "leader_unavailable"),
    ] {
        fake.clear_failures();
        // The message the server sends CONTAINS the token — the worst case.
        fake.fail(
            "/v1/sessions",
            status,
            code,
            &format!("the token {SENTINEL_TOKEN} was rejected"),
        );
        if let Err(e) = client.sessions().await {
            errors.push(e);
        }
    }
    fake.clear_failures();
    fake.fail_raw(
        "/v1/sessions",
        502,
        &format!("<html>{SENTINEL_TOKEN}</html>"),
    );
    if let Err(e) = client.sessions().await {
        errors.push(e);
    }

    // NO exclusions. Two of gx's codes carry its prose verbatim into a
    // LaneError (`bad_request` keeps the message whole on purpose), so a
    // server that quotes the credential it just refused would otherwise travel
    // straight into a client's log — which is what `redact_hex64` is for.
    for e in &errors {
        let rendered = format!("{e}{e:?}");
        assert!(
            !rendered.contains(SENTINEL_TOKEN),
            "an error path leaked the token: {rendered}"
        );
    }
    assert!(
        errors.iter().any(|e| format!("{e}").contains("<redacted>")),
        "and the redaction really fired rather than the message being dropped"
    );
    assert!(errors.len() >= 5, "the audit really exercised the paths");

    // And the token is never in a request the adapter did not mean to send.
    assert!(fake
        .requests()
        .iter()
        .all(|r| !r.path.contains(SENTINEL_TOKEN) && !r.query.contains(SENTINEL_TOKEN)));
    assert!(
        !fake
            .requests()
            .iter()
            .any(|r| r.body.contains(SENTINEL_TOKEN)),
        "never in a body — and never as a `?token=` query, which gx supports \
         for EventSource and this adapter deliberately does not use"
    );
}

/// `subscribe` NEVER fails, and it does not have to be reachable to say so.
///
/// Everything that can go wrong — the dial, the pin, the connect, the seed —
/// happens inside the pump and reaches the client as a `Reset` that never
/// resolves and finally a `Down`. An `Err` here would give a client a second
/// spelling of "the agent is not up" and therefore a second code path; the
/// whole point of `Down` is that there is only one.
///
/// The pump's own behaviour is `tests/watcher.rs`; this pins the SIGNATURE's
/// promise, against a session that does not even exist.
#[tokio::test]
async fn subscribe_never_fails_even_for_a_session_that_is_not_there() {
    // A bare fake: no sessions, and no pin — the pin guard answers `500` to a
    // session it does not recognize, and this test is about the `404` that
    // means "gx does not have this session", which is the one that is terminal.
    let fake = FakeGx::start().await;
    let client = client_for(&fake);
    // `LaneSubscription` has no `Debug` (it holds a JoinHandle), so the result
    // is matched rather than unwrapped.
    let Ok(sub) = client.subscribe("no-such-session-1", None).await else {
        panic!("subscribe must answer Ok and let the stream say Down");
    };
    let (mut rx, _stop) = sub.into_parts();
    let mut saw_down = false;
    while let Some(ev) = tokio::time::timeout(Duration::from_secs(5), rx.recv())
        .await
        .expect("the pump answers within five seconds")
    {
        if let LaneEvent::Down { reason } = ev {
            assert_eq!(reason, "unknown_session");
            saw_down = true;
            break;
        }
    }
    assert!(saw_down, "the subscription ends by SAYING so");
    // And the pin guard never saw a verb addressed at the wrong session.
    assert!(fake.violations().is_empty(), "{:?}", fake.violations());
}

// ---------------------------------------------------------------------------
// the fake's own guards
// ---------------------------------------------------------------------------

#[tokio::test]
async fn the_pin_guard_catches_a_verb_addressed_at_the_wrong_session() {
    let (fake, client) = wired().await;
    fake.add_session("other", None, "/q", "idle", 0, false);
    let err = client.session("other").await.expect_err("guarded");
    assert!(matches!(err, LaneError::Failed(_)), "{err:?}");
    assert_eq!(fake.violations().len(), 1, "{:?}", fake.violations());
    assert!(fake.violations()[0].contains("not the pinned"));
}

#[tokio::test]
async fn a_wrong_token_is_unauthorized_and_the_fake_says_nothing_about_which_half() {
    let fake = staged().await;
    let wrong = "0".repeat(64);
    let client = GxClient::new(
        fake.reported_url(),
        Arc::new(FixedDial::new(fake.dial_url())),
        Arc::new(StaticCredentials::from_parts(&wrong, &fake.instance_id()).expect("token")),
        GxTimings::default(),
    )
    .expect("builds");

    assert_eq!(
        client.sessions().await.expect_err("refused"),
        LaneError::Unauthorized
    );
    // An Unauthorized opens a new epoch: the next call re-discovers rather
    // than retrying a credential the leader has already refused.
    assert_eq!(client.pinned_instance().await, None);
}

#[tokio::test]
async fn a_hostile_session_id_cannot_escape_its_path_segment() {
    let (fake, client) = wired().await;
    // An id off a roost tab is untrusted input; a bare `/` would re-address
    // the request.
    let _ = client.session("../../v1/healthz").await;
    assert!(
        fake.requests()
            .iter()
            .all(|r| r.path.starts_with("/v1/sessions/") || r.path == "/v1/healthz"),
        "{:#?}",
        fake.requests()
    );
}
