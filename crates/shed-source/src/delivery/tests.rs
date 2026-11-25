use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use rustls::sign::CertifiedKey;
use tokio::sync::{watch, Notify};

use super::fake::Fake;
use super::*;

pub(crate) const MACHINE_ID: &str = "5f0c6e2a9b8d4c1e7a3f2b6d8e9c0a1b";

pub(crate) fn machine() -> MachineIdentity {
    MachineIdentity {
        id: MACHINE_ID.into(),
        name: "ada laptop".into(),
    }
}

pub(crate) fn fast() -> Timing {
    Timing {
        min_backoff: Duration::from_millis(20),
        max_backoff: Duration::from_millis(200),
        held_reset: Duration::from_secs(60),
        silence: Duration::from_secs(10),
        answer_timeout: Duration::from_secs(5),
        header_timeout: Duration::from_secs(5),
        refresh_check: Duration::from_secs(60),
        service_recheck: Duration::from_secs(60),
    }
}

pub(crate) fn target(url: &str) -> ServerTarget {
    ServerTarget {
        name: "mini".into(),
        url: url.into(),
        token: String::new(),
        tls_fingerprint: String::new(),
        ssh_host: "127.0.0.1".into(),
        ssh_port: 0,
        auth_mode: String::new(),
        user: String::new(),
        identity_file: String::new(),
    }
}

/// A `request` event for request `id`, deadline `deadline`.
pub(crate) fn request_event(id: &str, deadline: &str) -> String {
    format!(
        "event: request\ndata: {{\"version\":2,\"id\":\"{id}\",\"reason\":\"create\",\"deadline\":\"{deadline}\",\"user\":\"ada\",\"shed\":{{\"name\":\"widgets\",\"id\":\"q6c3g4ymb5m2dkxwe7kyz5hhzu\",\"backend\":\"firecracker\"}}}}\n\n"
    )
}

pub(crate) const FAR: &str = "2999-01-01T00:00:00Z";

/// Answers every request with no items, after `gate` (when set) is released; records
/// the requests and the outcomes.
#[derive(Default)]
pub(crate) struct Recorder {
    pub gate: Option<Arc<Notify>>,
    pub requests: Mutex<Vec<String>>,
    pub outcomes: Mutex<Vec<(String, AnswerOutcome)>>,
    pub decline: bool,
}

#[async_trait::async_trait]
impl Answerer for Recorder {
    async fn answer(&self, _server: &str, request: Request) -> Option<Answer> {
        self.requests.lock().unwrap().push(request.id.clone());
        if let Some(g) = &self.gate {
            g.notified().await;
        }
        (!self.decline).then(|| Answer {
            id: request.id,
            items: Vec::new(),
            problems: Vec::new(),
        })
    }
    fn outcome(&self, _server: &str, request_id: &str, outcome: &AnswerOutcome) {
        self.outcomes
            .lock()
            .unwrap()
            .push((request_id.to_string(), outcome.clone()));
    }
}

struct Running {
    status: SharedStatus,
    stop: watch::Sender<bool>,
    task: tokio::task::JoinHandle<()>,
}

impl Running {
    async fn stop(self) -> StreamStatus {
        self.stop.send(true).unwrap();
        tokio::time::timeout(Duration::from_secs(10), self.task)
            .await
            .expect("the client stops")
            .unwrap();
        self.status.lock().unwrap().clone()
    }
    fn status(&self) -> StreamStatus {
        self.status.lock().unwrap().clone()
    }
    async fn wait_status(&self, what: &str, cond: impl Fn(&StreamStatus) -> bool) {
        let deadline = tokio::time::Instant::now() + Duration::from_secs(10);
        while !cond(&self.status()) {
            assert!(
                tokio::time::Instant::now() < deadline,
                "timed out waiting for {what}: {:?}",
                self.status()
            );
            tokio::time::sleep(Duration::from_millis(5)).await;
        }
    }
}

fn run(fake: &Fake, creds: Arc<dyn StreamCredentials>, answerer: Arc<dyn Answerer>) -> Running {
    let client = DeliveryClient::new(&target(&fake.url), machine(), creds)
        .unwrap()
        .with_timing(fast());
    let status = new_status();
    let (stop, rx) = watch::channel(false);
    let st = status.clone();
    let task = tokio::spawn(async move { client.run(answerer, st, rx).await });
    Running { status, stop, task }
}

#[tokio::test]
async fn a_request_is_answered_on_its_stream_with_the_machine_headers() {
    let fake = Fake::start().await;
    let rec = Arc::new(Recorder::default());
    let r = run(&fake, Arc::new(NoCredentials), rec.clone());
    let stream = fake.wait_streams(1).await;
    let opened = &fake.streams()[0];
    assert_eq!(opened.machine_id.as_deref(), Some(MACHINE_ID));
    assert_eq!(opened.machine_name.as_deref(), Some("ada%20laptop"));
    let id = "0123456789abcdef0123456789abcdef";
    // A comment, an event of another type and a keepalive are not requests.
    fake.push(": keepalive\n\nevent: other\ndata: x\n\n");
    fake.push(&request_event(id, FAR));
    fake.wait("one answer", |_, p| p.len() == 1).await;
    let post = &fake.posts()[0];
    assert_eq!(post.request_id, id);
    assert_eq!(post.stream.as_deref(), Some(stream.as_str()));
    assert_eq!(post.machine_id.as_deref(), Some(MACHINE_ID));
    let body: serde_json::Value = serde_json::from_slice(&post.body).unwrap();
    assert_eq!(body["id"], id);
    assert_eq!(body["version"], 2);
    r.wait_status("answered", |s| s.answered == 1).await;
    let s = r.stop().await;
    assert_eq!((s.streams, s.requests, s.answered), (1, 1, 1));
    assert_eq!(s.state, StreamState::Stopped);
    assert_eq!(
        rec.outcomes.lock().unwrap().as_slice(),
        &[(id.to_string(), AnswerOutcome::Accepted)]
    );
}

/// A request whose stream ends before its answer is on its way is dropped, never
/// posted, and never answered on the next stream.
#[tokio::test]
async fn a_request_dies_with_its_stream() {
    let fake = Fake::start().await;
    let gate = Arc::new(Notify::new());
    let rec = Arc::new(Recorder {
        gate: Some(gate.clone()),
        ..Recorder::default()
    });
    let r = run(&fake, Arc::new(NoCredentials), rec.clone());
    fake.wait_streams(1).await;
    let first = "11111111111111111111111111111111";
    fake.push(&request_event(first, FAR));
    r.wait_status("the request read", |s| s.requests == 1).await;
    fake.end_stream();
    r.wait_status("the answer dropped", |s| s.dropped == 1)
        .await;
    let second_stream = fake.wait_streams(2).await;
    // The answerer finishes late: nothing is posted for the ended stream.
    gate.notify_one();
    let second = "22222222222222222222222222222222";
    fake.push(&request_event(second, FAR));
    r.wait_status("the second read", |s| s.requests == 2).await;
    gate.notify_one();
    fake.wait("the second answer", |_, p| !p.is_empty()).await;
    let posts = fake.posts();
    assert_eq!(posts.len(), 1, "{posts:?}");
    assert_eq!(posts[0].request_id, second);
    assert_eq!(posts[0].stream.as_deref(), Some(second_stream.as_str()));
    let s = r.stop().await;
    assert_eq!((s.dropped, s.answered), (1, 1));
    assert!(rec
        .outcomes
        .lock()
        .unwrap()
        .contains(&(first.to_string(), AnswerOutcome::DroppedStreamEnded)));
}

#[tokio::test]
async fn an_answer_past_its_deadline_is_dropped() {
    let fake = Fake::start().await;
    let rec = Arc::new(Recorder::default());
    let r = run(&fake, Arc::new(NoCredentials), rec.clone());
    fake.wait_streams(1).await;
    let id = "33333333333333333333333333333333";
    fake.push(&request_event(id, "2000-01-01T00:00:00Z"));
    r.wait_status("dropped", |s| s.dropped == 1).await;
    assert!(fake.posts().is_empty());
    r.stop().await;
    assert_eq!(
        rec.outcomes.lock().unwrap().as_slice(),
        &[(id.to_string(), AnswerOutcome::DroppedDeadline)]
    );
}

#[tokio::test]
async fn an_unreadable_request_is_counted_and_not_answered() {
    let fake = Fake::start().await;
    let rec = Arc::new(Recorder::default());
    let r = run(&fake, Arc::new(NoCredentials), rec.clone());
    fake.wait_streams(1).await;
    fake.push("event: request\ndata: {\"version\":2}\n\n");
    r.wait_status("unreadable", |s| s.unreadable == 1).await;
    let s = r.stop().await;
    assert_eq!(s.requests, 0);
    assert!(rec.requests.lock().unwrap().is_empty());
    assert!(fake.posts().is_empty());
}

#[tokio::test]
async fn refusals_set_the_state_and_are_retried() {
    let fake = Fake::start().await;
    fake.refuse_next_stream(
        403,
        r#"{"error":{"code":"SOURCE_NOT_BOUND","message":"x"},"source":{"machine_id":"fedcba9876543210fedcba9876543210","machine_name":"desk"}}"#,
        &[],
    );
    fake.refuse_next_stream(
        409,
        r#"{"error":{"code":"SOURCE_DUPLICATE_STREAM","message":"x"}}"#,
        &[],
    );
    let r = run(
        &fake,
        Arc::new(NoCredentials),
        Arc::new(Recorder::default()),
    );
    fake.wait_streams(1).await;
    r.wait_status("connected", |s| s.state == StreamState::Connected)
        .await;
    let refused: Vec<_> = fake.streams().iter().map(|s| s.id.is_some()).collect();
    assert_eq!(refused, vec![false, false, true]);
    assert_eq!(r.status().last_error, None, "a 200 clears the last error");
    r.stop().await;
}

#[tokio::test]
async fn not_bound_is_the_state_while_it_lasts() {
    let fake = Fake::start().await;
    for _ in 0..50 {
        fake.refuse_next_stream(403, r#"{"error":{"code":"SOURCE_NOT_BOUND"}}"#, &[]);
    }
    let r = run(
        &fake,
        Arc::new(NoCredentials),
        Arc::new(Recorder::default()),
    );
    r.wait_status("not bound", |s| s.state == StreamState::NotBound)
        .await;
    assert_eq!(r.status().last_error, Some(ErrorClass::NotBound));
    let s = r.stop().await;
    assert_eq!(s.state, StreamState::Stopped);
}

/// A `429`'s `Retry-After` is waited out whole, even when the backoff is shorter.
#[tokio::test]
async fn a_rate_limit_waits_its_retry_after() {
    let fake = Fake::start().await;
    fake.refuse_next_stream(
        429,
        r#"{"error":{"code":"SOURCE_RATE_LIMITED"}}"#,
        &[("Retry-After", "1")],
    );
    let r = run(
        &fake,
        Arc::new(NoCredentials),
        Arc::new(Recorder::default()),
    );
    fake.wait_streams(1).await;
    let s = fake.streams();
    let waited = s[1].at - s[0].at;
    assert!(waited >= Duration::from_secs(1), "{waited:?}");
    r.stop().await;
}

/// Counts mints: `ready` after a `refused` is a mint.
#[derive(Default)]
struct CountingCreds {
    readies: AtomicUsize,
    refusals: AtomicUsize,
}

#[async_trait::async_trait]
impl StreamCredentials for CountingCreds {
    async fn ready(&self) -> Result<String, String> {
        self.readies.fetch_add(1, Ordering::SeqCst);
        Ok(String::new())
    }
    fn certificate(&self) -> Option<Arc<CertifiedKey>> {
        None
    }
    fn refused(&self) {
        self.refusals.fetch_add(1, Ordering::SeqCst);
    }
}

/// A 401 on the stream marks the credential refused; a 401 on an answer does too, and
/// ends the stream (its requests die), never re-sending the answer on another identity.
#[tokio::test]
async fn a_401_marks_the_credential_refused_and_ends_the_stream() {
    let fake = Fake::start().await;
    fake.refuse_next_stream(401, r#"{"error":{"code":"UNAUTHORIZED"}}"#, &[]);
    let creds = Arc::new(CountingCreds::default());
    let rec = Arc::new(Recorder::default());
    let r = run(&fake, creds.clone(), rec.clone());
    fake.wait_streams(1).await;
    assert_eq!(creds.refusals.load(Ordering::SeqCst), 1);
    fake.script_post(401, r#"{"error":{"code":"UNAUTHORIZED"}}"#);
    let id = "44444444444444444444444444444444";
    fake.push(&request_event(id, FAR));
    r.wait_status("refused", |s| s.refused == 1).await;
    fake.wait_streams(2).await;
    assert_eq!(fake.posts().len(), 1, "not re-sent");
    assert!(creds.refusals.load(Ordering::SeqCst) >= 2);
    assert!(rec.outcomes.lock().unwrap().contains(&(
        id.to_string(),
        AnswerOutcome::Failed {
            class: ErrorClass::Unauthorized
        }
    )));
    r.stop().await;
}

#[tokio::test]
async fn already_answered_counts_as_answered_and_refusals_carry_their_rule() {
    let fake = Fake::start().await;
    let rec = Arc::new(Recorder::default());
    let r = run(&fake, Arc::new(NoCredentials), rec.clone());
    fake.wait_streams(1).await;
    fake.script_post(409, r#"{"error":{"code":"already_answered"}}"#);
    fake.script_post(
        422,
        r#"{"error":{"code":"answer_refused","message":"m"},"rule":"duplicate_target"}"#,
    );
    fake.push(&request_event("66666666666666666666666666666666", FAR));
    r.wait_status("one", |s| s.answered == 1).await;
    fake.push(&request_event("77777777777777777777777777777777", FAR));
    r.wait_status("two", |s| s.refused == 1).await;
    r.stop().await;
    let outcomes = rec.outcomes.lock().unwrap().clone();
    assert_eq!(outcomes[0].1, AnswerOutcome::AlreadyAnswered);
    assert_eq!(
        outcomes[1].1,
        AnswerOutcome::Refused {
            status: 422,
            code: "answer_refused".into(),
            rule: Some("duplicate_target".into()),
        }
    );
}

/// A refusal's code and rule are reported from the contract's closed lists only: a
/// server that echoes anything else there is reported as `unknown`.
#[tokio::test]
async fn refusal_fields_outside_the_closed_lists_are_unknown() {
    let fake = Fake::start().await;
    let rec = Arc::new(Recorder::default());
    let r = run(&fake, Arc::new(NoCredentials), rec.clone());
    fake.wait_streams(1).await;
    fake.script_post(
        422,
        r#"{"error":{"code":"ghp_SENTINEL_CODE","message":"ghp_SENTINEL_MSG"},"rule":"ghp_SENTINEL_RULE"}"#,
    );
    fake.push(&request_event("12121212121212121212121212121212", FAR));
    r.wait_status("refused", |s| s.refused == 1).await;
    r.stop().await;
    let outcome = rec.outcomes.lock().unwrap()[0].1.clone();
    assert_eq!(
        outcome,
        AnswerOutcome::Refused {
            status: 422,
            code: UNKNOWN.into(),
            rule: Some(UNKNOWN.into()),
        }
    );
    assert!(!serde_json::to_string(&outcome)
        .unwrap()
        .contains("SENTINEL"));
    assert_eq!(known_code("not_stream_owner"), "not_stream_owner");
    assert_eq!(known_code("SOURCE_NOT_BOUND"), "SOURCE_NOT_BOUND");
    assert_eq!(known_code("SOURCE_IS_SERVICE"), "SOURCE_IS_SERVICE");
    assert_eq!(known_code("source_not_bound"), UNKNOWN);
    assert_eq!(known_code(""), "");
    assert_eq!(known_rule("duplicate_target"), "duplicate_target");
    assert_eq!(known_rule("Duplicate_Target"), UNKNOWN);
}

/// A server that accepts the stream's request and never sends its headers is given up
/// on after the header bound, and the next stream is tried.
#[tokio::test]
async fn a_stream_whose_headers_never_come_is_given_up() {
    let fake = Fake::start().await;
    fake.refuse_next_stream(0, "", &[]);
    let client = DeliveryClient::new(&target(&fake.url), machine(), Arc::new(NoCredentials))
        .unwrap()
        .with_timing(Timing {
            header_timeout: Duration::from_millis(300),
            ..fast()
        });
    let status = new_status();
    let (stop, rx) = watch::channel(false);
    let st = status.clone();
    let task = tokio::spawn(async move { client.run(Arc::new(Recorder::default()), st, rx).await });
    fake.wait_streams(1).await;
    stop.send(true).unwrap();
    task.await.unwrap();
    assert_eq!(fake.streams().len(), 2, "the hung one, then a good one");
}

#[tokio::test]
async fn a_silent_stream_is_dead_and_a_closed_one_reconnects() {
    let fake = Fake::start().await;
    let client = DeliveryClient::new(&target(&fake.url), machine(), Arc::new(NoCredentials))
        .unwrap()
        .with_timing(Timing {
            silence: Duration::from_millis(300),
            ..fast()
        });
    let status = new_status();
    let (stop, rx) = watch::channel(false);
    let st = status.clone();
    let task = tokio::spawn(async move { client.run(Arc::new(Recorder::default()), st, rx).await });
    // Nothing is pushed: each stream goes silent, is taken as dead, and is replaced.
    fake.wait_streams(3).await;
    stop.send(true).unwrap();
    task.await.unwrap();
    let s = status.lock().unwrap().clone();
    assert!(s.streams >= 2, "{s:?}");
}

#[tokio::test]
async fn a_stream_without_a_valid_id_is_a_protocol_error() {
    let fake = Fake::start().await;
    fake.refuse_next_stream(200, "", &[]);
    let r = run(
        &fake,
        Arc::new(NoCredentials),
        Arc::new(Recorder::default()),
    );
    r.wait_status("protocol", |s| s.last_error == Some(ErrorClass::Protocol))
        .await;
    r.stop().await;
}

#[tokio::test]
async fn a_declined_request_is_not_posted() {
    let fake = Fake::start().await;
    let rec = Arc::new(Recorder {
        decline: true,
        ..Recorder::default()
    });
    let r = run(&fake, Arc::new(NoCredentials), rec.clone());
    fake.wait_streams(1).await;
    fake.push(&request_event("88888888888888888888888888888888", FAR));
    r.wait_status("read", |s| s.requests == 1).await;
    let s = r.stop().await;
    assert_eq!((s.answered, s.dropped, s.refused), (0, 0, 0));
    assert!(fake.posts().is_empty());
}

#[test]
fn the_backoff_schedule() {
    let t = Timing::default();
    let s = Duration::from_secs;
    assert_eq!(next_backoff(&t, None, s(0), None), s(1));
    assert_eq!(next_backoff(&t, Some(s(1)), s(0), None), s(2));
    assert_eq!(next_backoff(&t, Some(s(16)), s(59), None), s(30));
    assert_eq!(next_backoff(&t, Some(s(30)), s(5), None), s(30));
    assert_eq!(next_backoff(&t, Some(s(30)), s(60), None), s(1));
    assert_eq!(next_backoff(&t, Some(s(1)), s(0), Some(s(7))), s(7));
    assert_eq!(next_backoff(&t, Some(s(16)), s(0), Some(s(7))), s(30));
}

#[test]
fn refusals_are_classified_by_status_and_code() {
    let c = |status, body: &str| classify_refusal(status, body.as_bytes());
    assert_eq!(c(401, ""), ErrorClass::Unauthorized);
    assert_eq!(
        c(403, r#"{"error":{"code":"SOURCE_NOT_BOUND"}}"#),
        ErrorClass::NotBound
    );
    assert_eq!(
        c(403, r#"{"error":{"code":"SOURCE_MACHINE_ID_REQUIRED"}}"#),
        ErrorClass::MachineIdRequired
    );
    assert_eq!(
        c(403, r#"{"error":{"code":"FORBIDDEN"}}"#),
        ErrorClass::Forbidden
    );
    assert_eq!(c(403, "{not json"), ErrorClass::Forbidden);
    assert_eq!(
        c(409, r#"{"error":{"code":"SOURCE_DUPLICATE_STREAM"}}"#),
        ErrorClass::Duplicate
    );
    assert_eq!(
        c(
            409,
            include_str!("../../../fixtures/sources/source-is-service.json")
        ),
        ErrorClass::SourceIsService
    );
    assert_eq!(
        c(403, r#"{"error":{"code":"SOURCE_IS_SERVICE"}}"#),
        ErrorClass::Forbidden
    );
    assert_eq!(c(409, "{}"), ErrorClass::BadStatus);
    assert_eq!(c(429, ""), ErrorClass::RateLimited);
    assert_eq!(c(404, "404 page not found\n"), ErrorClass::Unsupported);
    assert_eq!(c(503, ""), ErrorClass::Unavailable);
    assert_eq!(c(500, ""), ErrorClass::BadStatus);
}

#[test]
fn a_client_is_refused_for_a_bad_endpoint_or_identity() {
    let creds = || Arc::new(NoCredentials) as Arc<dyn StreamCredentials>;
    let ok = |url: &str| DeliveryClient::new(&target(url), machine(), creds());
    // https: only with a pin.
    let mut https = target("https://mini.example.test:18443");
    let err = DeliveryClient::new(&https, machine(), creds())
        .err()
        .unwrap();
    assert!(err.contains("pinned"), "{err}");
    https.tls_fingerprint = "sha256:00".into();
    assert!(DeliveryClient::new(&https, machine(), creds()).is_ok());
    // http: only on loopback, and never with a pin.
    for url in [
        "http://127.0.0.1:1",
        "http://127.9.8.7:8080",
        "http://[::1]:8080",
        "http://localhost:8080",
    ] {
        assert!(ok(url).is_ok(), "{url}");
    }
    for url in [
        "http://mini.example.test:8080",
        "http://10.0.0.5:8080",
        "http://[::ffff:10.0.0.5]:8080",
        "http://0.0.0.0:8080",
        "http://localhost.example.test:8080",
    ] {
        let err = ok(url).err().unwrap();
        assert!(err.contains("loopback"), "{url}: {err}");
    }
    let mut http = target("http://127.0.0.1:1");
    http.tls_fingerprint = "sha256:00".into();
    let err = DeliveryClient::new(&http, machine(), creds())
        .err()
        .unwrap();
    assert!(err.contains("non-https"), "{err}");
    let bad = MachineIdentity {
        id: "nope".into(),
        name: "x".into(),
    };
    assert!(DeliveryClient::new(&target("http://127.0.0.1:1"), bad, creds()).is_err());
    assert!(ok("ftp://x").is_err());
    assert!(ok("not a url").is_err());
}

#[test]
fn the_machine_name_is_percent_encoded() {
    assert_eq!(percent_encode_name("ada's mac"), "ada%27s%20mac");
    assert_eq!(percent_encode_name("é"), "%C3%A9");
    assert_eq!(percent_encode_name("a-b.c_d~e"), "a-b.c_d~e");
}

// ---- one certificate per stream, over mTLS ----

/// A CA, a server certificate, and client certificates A and B the CA issued.
struct Pki {
    server: Arc<rustls::ServerConfig>,
    pin: String,
    clients: Vec<(Vec<u8>, Arc<CertifiedKey>)>,
}

fn pki() -> Pki {
    use rustls::pki_types::{PrivateKeyDer, PrivatePkcs8KeyDer};
    let provider = Arc::new(rustls::crypto::ring::default_provider());
    let ca_key = rcgen::KeyPair::generate().unwrap();
    let mut ca = rcgen::CertificateParams::new(Vec::<String>::new()).unwrap();
    ca.is_ca = rcgen::IsCa::Ca(rcgen::BasicConstraints::Unconstrained);
    ca.distinguished_name
        .push(rcgen::DnType::CommonName, "test-ca");
    let ca = ca.self_signed(&ca_key).unwrap();
    let mut clients = Vec::new();
    for (serial, name) in [(0xA, "a"), (0xB, "b")] {
        let key = rcgen::KeyPair::generate().unwrap();
        let mut p = rcgen::CertificateParams::new(Vec::<String>::new()).unwrap();
        p.distinguished_name.push(rcgen::DnType::CommonName, name);
        p.extended_key_usages = vec![rcgen::ExtendedKeyUsagePurpose::ClientAuth];
        p.serial_number = Some(rcgen::SerialNumber::from(serial as u64));
        let cert = p.signed_by(&key, &ca, &ca_key).unwrap();
        let der = cert.der().to_vec();
        let signing = rustls::crypto::ring::sign::any_supported_type(&PrivateKeyDer::Pkcs8(
            PrivatePkcs8KeyDer::from(key.serialize_der()),
        ))
        .unwrap();
        clients.push((
            der,
            Arc::new(CertifiedKey::new(vec![cert.der().clone()], signing)),
        ));
    }
    let server_key = rcgen::KeyPair::generate().unwrap();
    let server_cert = rcgen::CertificateParams::new(vec!["127.0.0.1".to_string()])
        .unwrap()
        .self_signed(&server_key)
        .unwrap();
    let mut roots = rustls::RootCertStore::empty();
    roots.add(ca.der().clone()).unwrap();
    let verifier = rustls::server::WebPkiClientVerifier::builder_with_provider(
        Arc::new(roots),
        provider.clone(),
    )
    .build()
    .unwrap();
    let server = rustls::ServerConfig::builder_with_provider(provider)
        .with_safe_default_protocol_versions()
        .unwrap()
        .with_client_cert_verifier(verifier)
        .with_single_cert(
            vec![server_cert.der().clone()],
            PrivateKeyDer::Pkcs8(PrivatePkcs8KeyDer::from(server_key.serialize_der())),
        )
        .unwrap();
    Pki {
        server: Arc::new(server),
        pin: shed_core::tls::fingerprint(server_cert.der()),
        clients,
    }
}

/// A credential whose certificate a test swaps, and whose renewal it asks for.
struct SwapCreds {
    cert: Mutex<Arc<CertifiedKey>>,
    renew: std::sync::atomic::AtomicBool,
}

#[async_trait::async_trait]
impl StreamCredentials for SwapCreds {
    async fn ready(&self) -> Result<String, String> {
        self.renew.store(false, Ordering::SeqCst);
        Ok(String::new())
    }
    fn certificate(&self) -> Option<Arc<CertifiedKey>> {
        Some(self.cert.lock().unwrap().clone())
    }
    fn refused(&self) {}
    fn needs_refresh(&self) -> bool {
        self.renew.load(Ordering::SeqCst)
    }
}

/// A stream opened with certificate A answers with A even after the credential moved
/// to B; when the credential asks for renewal the stream ends, and the next stream and
/// its answers present B. The server sees each answer's certificate match its stream's.
#[tokio::test]
async fn a_stream_and_its_answers_present_one_certificate() {
    let pki = pki();
    let labels = vec![
        (pki.clients[0].0.clone(), "A".to_string()),
        (pki.clients[1].0.clone(), "B".to_string()),
    ];
    let fake = Fake::start_tls(pki.server.clone(), labels).await;
    let creds = Arc::new(SwapCreds {
        cert: Mutex::new(pki.clients[0].1.clone()),
        renew: Default::default(),
    });
    let mut t = target(&fake.url);
    t.tls_fingerprint = pki.pin.clone();
    let client = DeliveryClient::new(&t, machine(), creds.clone())
        .unwrap()
        .with_timing(Timing {
            refresh_check: Duration::from_millis(50),
            ..fast()
        });
    let status = new_status();
    let (stop, rx) = watch::channel(false);
    let st = status.clone();
    let task = tokio::spawn(async move { client.run(Arc::new(Recorder::default()), st, rx).await });
    let first = fake.wait_streams(1).await;
    assert_eq!(fake.streams()[0].peer.as_deref(), Some("A"));

    // The credential moves to B, but this stream's answers stay A's.
    *creds.cert.lock().unwrap() = pki.clients[1].1.clone();
    fake.push(&request_event("a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1", FAR));
    fake.wait("the first answer", |_, p| p.len() == 1).await;
    let p = &fake.posts()[0];
    assert_eq!(
        (p.peer.as_deref(), p.stream.as_deref()),
        (Some("A"), Some(first.as_str()))
    );

    // Renewal ends the stream; the next is B's, and so are its answers.
    creds.renew.store(true, Ordering::SeqCst);
    let second = fake.wait_streams(2).await;
    let streams = fake.streams();
    assert_eq!(streams.last().unwrap().peer.as_deref(), Some("B"));
    fake.push(&request_event("b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2", FAR));
    fake.wait("the second answer", |_, p| p.len() == 2).await;
    let p = &fake.posts()[1];
    assert_eq!(
        (p.peer.as_deref(), p.stream.as_deref()),
        (Some("B"), Some(second.as_str()))
    );
    assert!(fake.posts().iter().all(|p| p.status == 204));
    stop.send(true).unwrap();
    task.await.unwrap();
}

// ---- the silence deadline, and plaintext's proxy and name pins ----

/// The refresh timer ticks more often than the silence bound (as in production) and
/// still a stream that hears nothing is ended at the bound.
#[tokio::test]
async fn refresh_ticks_do_not_restart_the_silence_deadline() {
    let fake = Fake::start().await;
    let client = DeliveryClient::new(&target(&fake.url), machine(), Arc::new(NoCredentials))
        .unwrap()
        .with_timing(Timing {
            silence: Duration::from_millis(400),
            refresh_check: Duration::from_millis(50),
            ..fast()
        });
    let status = new_status();
    let (stop, rx) = watch::channel(false);
    let st = status.clone();
    let task = tokio::spawn(async move { client.run(Arc::new(Recorder::default()), st, rx).await });
    fake.wait_streams(2).await;
    let s = fake.streams();
    let held = s[1].at - s[0].at;
    assert!(
        held < Duration::from_secs(3),
        "the silent stream held {held:?}"
    );
    stop.send(true).unwrap();
    task.await.unwrap();
}

/// A proxy in the environment (not loopback) is never used for loopback plain HTTP:
/// the fake still receives the stream directly.
#[tokio::test]
#[allow(clippy::await_holding_lock)] // the env lock is held across the run on purpose
async fn a_proxy_in_the_environment_is_never_used_for_plaintext() {
    let _env = crate::env_lock();
    let vars = ["HTTP_PROXY", "http_proxy", "ALL_PROXY", "all_proxy"];
    let saved: Vec<_> = vars.iter().map(|v| (*v, std::env::var_os(v))).collect();
    for v in vars {
        std::env::set_var(v, "http://192.0.2.1:9");
    }
    std::env::remove_var("NO_PROXY");
    std::env::remove_var("no_proxy");
    let fake = Fake::start().await;
    let r = run(
        &fake,
        Arc::new(NoCredentials),
        Arc::new(Recorder::default()),
    );
    let reached = tokio::time::timeout(Duration::from_secs(5), fake.wait_streams(1)).await;
    r.stop().await;
    for (v, old) in saved {
        match old {
            Some(o) => std::env::set_var(v, o),
            None => std::env::remove_var(v),
        }
    }
    assert!(reached.is_ok(), "the stream went through the proxy");
}

/// `localhost` is looked up once, when the client is made; its loopback answer is
/// where every connection goes, whatever a later lookup would say. The fake listens
/// on 127.0.0.3 only, so a second (system) lookup of `localhost` would miss it.
#[cfg(target_os = "linux")]
#[tokio::test]
async fn a_validated_name_connects_only_to_its_validated_address() {
    let probe = std::net::TcpListener::bind("127.0.0.3:0").unwrap();
    let port = probe.local_addr().unwrap().port();
    drop(probe);
    let Some(fake) = Fake::start_at(&format!("127.0.0.3:{port}")).await else {
        return;
    };
    let lookups = std::sync::atomic::AtomicUsize::new(0);
    let lookup = |_: &str, port: u16| -> std::io::Result<Vec<std::net::SocketAddr>> {
        let n = lookups.fetch_add(1, Ordering::SeqCst);
        let ip = if n == 0 { "127.0.0.3" } else { "10.0.0.1" };
        Ok(vec![std::net::SocketAddr::new(ip.parse().unwrap(), port)])
    };
    let client = DeliveryClient::with_lookup(
        &target(&format!("http://localhost:{port}")),
        machine(),
        Arc::new(NoCredentials),
        &lookup,
    )
    .unwrap()
    .with_timing(fast());
    assert_eq!(lookups.load(Ordering::SeqCst), 1);
    let status = new_status();
    let (stop, rx) = watch::channel(false);
    let st = status.clone();
    let task = tokio::spawn(async move { client.run(Arc::new(Recorder::default()), st, rx).await });
    let reached = tokio::time::timeout(Duration::from_secs(5), fake.wait_streams(1)).await;
    stop.send(true).unwrap();
    task.await.unwrap();
    assert!(
        reached.is_ok(),
        "the stream did not reach the validated address"
    );
    assert_eq!(lookups.load(Ordering::SeqCst), 1, "never looked up again");

    // A name whose lookup is not all loopback is refused.
    let mixed = |_: &str, port: u16| -> std::io::Result<Vec<std::net::SocketAddr>> {
        Ok(vec![
            std::net::SocketAddr::new("127.0.0.1".parse().unwrap(), port),
            std::net::SocketAddr::new("10.0.0.1".parse().unwrap(), port),
        ])
    };
    assert!(DeliveryClient::with_lookup(
        &target("http://localhost:8080"),
        machine(),
        Arc::new(NoCredentials),
        &mixed
    )
    .is_err());
}

/// The `409 SOURCE_IS_SERVICE` body the server answers on every source route for a user
/// the account service answers for (the shared fixture, `crates/fixtures/sources/`).
pub(crate) const SOURCE_IS_SERVICE_BODY: &str =
    include_str!("../../../fixtures/sources/source-is-service.json");

/// A check that answers from a script (then `then`), counting its calls.
pub(crate) fn scripted_check(
    script: Vec<ServiceCheck>,
    then: ServiceCheck,
) -> (SourceCheck, Arc<AtomicUsize>) {
    let calls = Arc::new(AtomicUsize::new(0));
    let script = Arc::new(Mutex::new(std::collections::VecDeque::from(script)));
    let counted = calls.clone();
    let check: SourceCheck = Arc::new(move |t: &ServerTarget| {
        assert_eq!(t.name, "mini");
        counted.fetch_add(1, Ordering::SeqCst);
        let next = script.lock().unwrap().pop_front().unwrap_or(then);
        Box::pin(async move { next })
    });
    (check, calls)
}

fn run_checked(fake: &Fake, timing: Timing, check: Option<SourceCheck>) -> Running {
    let client = DeliveryClient::new(&target(&fake.url), machine(), Arc::new(NoCredentials))
        .unwrap()
        .with_timing(timing)
        .with_service_check(check, None);
    let status = new_status();
    let (stop, rx) = watch::channel(false);
    let st = status.clone();
    let task = tokio::spawn(async move { client.run(Arc::new(Recorder::default()), st, rx).await });
    Running { status, stop, task }
}

/// `409 SOURCE_IS_SERVICE` is settled, not a failure: the state says the account
/// service answers, the class is from the closed list (never the server's words), and
/// no stream is tried again on the backoff, however short it is.
#[tokio::test]
async fn the_account_service_is_a_settled_state_with_no_reconnect() {
    let fake = Fake::start().await;
    for _ in 0..20 {
        fake.refuse_next_stream(409, SOURCE_IS_SERVICE_BODY, &[]);
    }
    let (check, calls) = scripted_check(Vec::new(), ServiceCheck::Service);
    let r = run_checked(&fake, fast(), Some(check));
    r.wait_status("settled on the service", |s| {
        s.state == StreamState::Service
    })
    .await;
    // Ten floors of the backoff, and then some: not one more attempt.
    tokio::time::sleep(Duration::from_millis(400)).await;
    assert_eq!(fake.streams().len(), 1, "{:?}", fake.streams());
    let s = r.status();
    assert_eq!(s.state, StreamState::Service);
    assert_eq!(s.last_error, Some(ErrorClass::SourceIsService));
    assert_eq!(s.service_checks, 0, "the timer has not come round");
    assert_eq!(calls.load(Ordering::SeqCst), 0);
    let shown = serde_json::to_string(&s).unwrap();
    assert!(shown.contains(r#""state":"service""#), "{shown}");
    assert!(
        shown.contains(r#""last_error":"source_is_service""#),
        "{shown}"
    );
    assert!(
        !shown.contains("account service is"),
        "no server text: {shown}"
    );
    // A shutdown while settled ends it at once.
    assert_eq!(r.stop().await.state, StreamState::Stopped);
}

/// A settled stream checks on its timer: while the check says the service still
/// answers, or cannot tell, it stays settled and tries no stream; once it says
/// otherwise, the stream is opened again at once (from the backoff's floor).
#[tokio::test]
async fn a_settled_stream_rechecks_on_its_timer_and_reopens_when_the_service_stops() {
    let fake = Fake::start().await;
    fake.refuse_next_stream(409, SOURCE_IS_SERVICE_BODY, &[]);
    let (check, calls) = scripted_check(
        vec![
            ServiceCheck::Service,
            ServiceCheck::Unknown,
            ServiceCheck::Service,
        ],
        ServiceCheck::NotService,
    );
    let r = run_checked(
        &fake,
        Timing {
            service_recheck: Duration::from_millis(60),
            ..fast()
        },
        Some(check),
    );
    fake.wait_streams(1).await;
    r.wait_status("connected again", |s| s.state == StreamState::Connected)
        .await;
    assert_eq!(
        calls.load(Ordering::SeqCst),
        4,
        "three that stayed, one that left"
    );
    let opened = fake.streams();
    assert_eq!(opened.len(), 2, "the refused one, then the reopened one");
    let rested = opened[1].at - opened[0].at;
    assert!(rested >= Duration::from_millis(4 * 60), "{rested:?}");
    let s = r.stop().await;
    assert_eq!(s.service_checks, 4);
    assert_eq!(s.last_error, None, "the 200 cleared it");
}

/// With no check (no control-scope client to ask), the timer alone wakes a settled
/// stream, which tries the stream itself again.
#[tokio::test]
async fn with_no_check_the_timer_tries_the_stream_again() {
    let fake = Fake::start().await;
    fake.refuse_next_stream(409, SOURCE_IS_SERVICE_BODY, &[]);
    fake.refuse_next_stream(409, SOURCE_IS_SERVICE_BODY, &[]);
    let r = run_checked(
        &fake,
        Timing {
            service_recheck: Duration::from_millis(80),
            ..fast()
        },
        None,
    );
    fake.wait_streams(1).await;
    let opened = fake.streams();
    assert_eq!(opened.len(), 3);
    for pair in opened.windows(2) {
        assert!(
            pair[1].at - pair[0].at >= Duration::from_millis(80),
            "{opened:?}"
        );
    }
    assert_eq!(r.stop().await.service_checks, 2);
}

/// A change of the server list wakes a settled stream at once, long before its timer;
/// a change from before it settled does not count.
#[tokio::test]
async fn a_server_list_change_rechecks_at_once() {
    let fake = Fake::start().await;
    fake.refuse_next_stream(409, SOURCE_IS_SERVICE_BODY, &[]);
    let (check, calls) = scripted_check(vec![ServiceCheck::Service], ServiceCheck::NotService);
    let (list, list_rx) = watch::channel(0u64);
    list.send_modify(|n| *n += 1);
    let client = DeliveryClient::new(&target(&fake.url), machine(), Arc::new(NoCredentials))
        .unwrap()
        .with_timing(fast())
        .with_service_check(Some(check), Some(list_rx));
    let status = new_status();
    let (stop, rx) = watch::channel(false);
    let st = status.clone();
    let task = tokio::spawn(async move { client.run(Arc::new(Recorder::default()), st, rx).await });
    let r = Running { status, stop, task };
    r.wait_status("settled", |s| s.state == StreamState::Service)
        .await;
    tokio::time::sleep(Duration::from_millis(100)).await;
    assert_eq!(
        calls.load(Ordering::SeqCst),
        0,
        "the earlier change is not one"
    );
    // The first change: the service still answers.
    list.send_modify(|n| *n += 1);
    r.wait_status("one check", |s| s.service_checks == 1).await;
    assert_eq!(r.status().state, StreamState::Service);
    // The second: it no longer does, and the stream opens.
    list.send_modify(|n| *n += 1);
    fake.wait_streams(1).await;
    assert_eq!(calls.load(Ordering::SeqCst), 2);
    r.stop().await;
}
