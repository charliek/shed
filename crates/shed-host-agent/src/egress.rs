//! The always-on egress-audit SSE consumer — a Rust port of the Go daemon's
//! `cmd/shed-host-agent/egress_handler.go`.
//!
//! Unlike the plugin-bus namespaces this is NOT a bus subscription: it is a plain
//! read-only `GET {server}/api/egress/stream` (`Accept: text/event-stream`) whose
//! `data:` frames decode to [`EgressDecision`] (NOT `Envelope`). It reuses the SSE
//! FRAMER ([`shed_core::sse::SseParser`]) but none of the bus request/response/gate
//! machinery — every decision is mapped by [`egress_audit_entry`] into an
//! [`AuditEntry`] (namespace `egress`) and recorded via the shared [`AuditSink`] (which
//! fans it out to shed-desktop). Read-only: no `respond`, no approval, no new audit shape.
//!
//! It carries its OWN reconnect/backoff state machine ([`EgressSubscriber::run`]),
//! DISTINCT from — and simpler than — the bus's: `base=1s`, `max=30s`, `unavailable=5m`;
//! a 501/404 (egress disabled on the server) backs off the hard 5m and RESETS to base,
//! any other error (or a clean stream-end) exponentially doubles to the 30s cap with NO
//! 60s-held-reset. On a secure server the bearer is a CONTROL-scoped token (the egress
//! route is control-scoped, unlike the credentials-scoped bus); a 401 invalidates the
//! source so the next reconnect re-mints. An open server sends the static config token.
//!
//! NOTE (commit 1 of the egress slice): this module is LANDED but not yet SPAWNED — the
//! always-on per-server task is wired in commit 2. Hence the module-level
//! `#![allow(dead_code)]`; commit 2 removes it once `run_single_server_bus` spawns the
//! side task.
#![allow(dead_code)]

use std::sync::Arc;
use std::time::Duration;

use futures_util::StreamExt;
use serde::Deserialize;
use tokio::sync::watch;

use shed_core::sse::SseParser;
use shed_core::tls::pinned_client_config;

use crate::audit::{AuditEntry, AuditSink};
use crate::bus::BusLog;
use crate::config::NS_EGRESS;
use crate::status::{parse_rfc3339_to_unix, rfc3339_utc};

/// shed-server's egress SSE endpoint (one `data:` frame per decision).
const EGRESS_STREAM_PATH: &str = "/api/egress/stream";

/// The backoff constants (mirror `egress_handler.go:104`). DISTINCT from the bus's:
/// there is NO 60s-held-reset — a flapping-but-reachable server just ramps to `MAX`.
const BASE_BACKOFF: Duration = Duration::from_secs(1);
const MAX_BACKOFF: Duration = Duration::from_secs(30);
const UNAVAILABLE_BACKOFF: Duration = Duration::from_secs(5 * 60);

/// Cap a single SSE event at 1 MiB (Go's `bufio.Scanner` cap): an oversized /
/// never-terminating event surfaces as a read error → disconnect + reconnect.
const MAX_SSE_EVENT_BYTES: usize = 1 << 20;

/// One streamed egress decision — mirrors shed-server's `egress.AuditRecord` JSON
/// (`egress_handler.go:egressDecision`). shed-extensions cannot import shed's
/// `internal/egress`, so the (small, stable) wire shape is duplicated. Absent fields
/// default to their zero value (`#[serde(default)]`); unknown fields are ignored.
///
/// `ts` is modeled **parse-on-deserialize-to-skip** (the plan's finding 12): Go's field
/// is a `time.Time`, so `json.Unmarshal` of a present-but-not-RFC3339 `ts` ERRORS and the
/// whole frame is skipped — while an ABSENT `ts` is the zero time (→ rendered `""`). A
/// naive `ts: String` would NOT skip a malformed value, so the custom deserializer
/// reproduces both halves exactly (`Ok(None)` = absent/zero, a serde error = malformed).
#[derive(Debug, Clone, Default, Deserialize)]
#[serde(default)]
pub struct EgressDecision {
    /// Parsed unix seconds; `None` when absent/zero (→ rendered `""`). A present but
    /// malformed value fails deserialization → the frame is skipped (Go parity).
    #[serde(rename = "ts", deserialize_with = "de_egress_ts")]
    pub ts: Option<i64>,
    pub shed: String,
    pub host: String,
    pub port: i64,
    pub resolved_ip: String,
    pub protocol: String,
    pub verdict: String,
    pub reason: String,
}

/// Deserialize the egress `ts` to `Option<i64>` unix seconds, matching Go's `time.Time`
/// json semantics: `null` / a present zero-time → `None`; a valid instant → `Some(secs)`
/// (offset-normalized to UTC, sub-second truncated); an empty or otherwise malformed
/// non-null string → a serde error so the enclosing frame is skipped. (An ABSENT key
/// never reaches here — `#[serde(default)]` yields `None` directly.)
fn de_egress_ts<'de, D>(d: D) -> Result<Option<i64>, D::Error>
where
    D: serde::Deserializer<'de>,
{
    use serde::de::Error as _;
    let raw: Option<String> = Option::deserialize(d)?;
    // `null` → Go's zero time (no-op unmarshal) → rendered "".
    let Some(s) = raw else { return Ok(None) };
    // A PRESENT string (incl. "") goes through the RFC3339 parser: Go unmarshals into a
    // `time.Time`, so an empty/blank/malformed value is a parse error → skip the frame.
    match parse_rfc3339_to_unix(&s) {
        Ok(v) => Ok(v),
        Err(()) => Err(D::Error::custom("invalid RFC3339 ts")),
    }
}

/// Map one streamed decision into an [`AuditEntry`] (namespace `egress`) for the durable
/// log + desktop feed — the pure `egress_handler.go:egressAuditEntry` port. `ts` renders
/// via [`rfc3339_utc`] when present, else `""` (the shared [`AuditSink::log`] stamps a
/// wall-clock ts when empty, as Go's `LogEntry` does). `code`/`approval`/`decided_by`/
/// `scope`/`ttl` are left empty; the wire nonetheless carries `"approval":""` (present but
/// empty) because `WireEntry.approval` is unconditional — a golden-pinned byte.
pub fn egress_audit_entry(server: &str, d: &EgressDecision) -> AuditEntry {
    let ts = match d.ts {
        Some(secs) => rfc3339_utc(secs),
        None => String::new(),
    };
    let detail = if d.resolved_ip.is_empty() {
        format!("{}:{}", d.host, d.port)
    } else {
        format!("{}:{} ({})", d.host, d.port, d.resolved_ip)
    };
    AuditEntry {
        ts,
        server: server.to_string(),
        shed: d.shed.clone(),
        ns: NS_EGRESS.to_string(),
        op: d.protocol.clone(),
        result: d.verdict.clone(),
        detail,
        reason: d.reason.clone(),
        ..Default::default()
    }
}

/// The reason a single `stream` connection ended. Only [`EgressStreamError::Unavailable`]
/// (501/404 — egress disabled on the server) is special-cased by the backoff machine; a
/// clean stream-end (`Ok(())`) and every [`EgressStreamError::Other`] take the normal
/// exponential backoff (mirror Go's `errors.Is(err, errEgressUnavailable)` branch).
#[derive(Debug)]
pub enum EgressStreamError {
    /// 501 or 404: egress control is disabled on the server → hard 5m backoff.
    Unavailable,
    /// Any other transport / status error → normal exponential backoff.
    Other(String),
}

impl std::fmt::Display for EgressStreamError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            EgressStreamError::Unavailable => {
                f.write_str("egress stream: unavailable (disabled on server)")
            }
            EgressStreamError::Other(m) => f.write_str(m),
        }
    }
}

/// Provides — and on a 401 invalidates — the CONTROL-scoped bearer token for the egress
/// stream on a secure server (mirror Go's `tokenSource`). `Arc<CredentialSource>`
/// satisfies it (the bridge below, behind `desktop-forwarding`); an OPEN server passes
/// `None` and sends the static config token.
///
/// **Async** because `CredentialSource::token` is `async` with `self: &Arc<Self>` — a
/// direct impl on `CredentialSource` is impossible, so the bridge is impl'd for
/// `Arc<CredentialSource>`. Never `block_on`.
#[async_trait::async_trait]
pub trait EgressTokenSource: Send + Sync {
    async fn token(&self) -> Result<String, String>;
    fn invalidate(&self);
}

/// Bridge the caching, invalidatable control-token source onto [`EgressTokenSource`].
/// Behind `desktop-forwarding` because the minter itself is feature-gated (so
/// `--no-default-features` stays green). Ported now; spawned live by the discovery/
/// supervisor slice (the single-server open path this slice wires uses `None`/static).
#[cfg(feature = "desktop-forwarding")]
#[async_trait::async_trait]
impl EgressTokenSource for Arc<crate::minter::CredentialSource> {
    async fn token(&self) -> Result<String, String> {
        // UFCS selects the inherent `CredentialSource::token(self: &Arc<Self>)` — a bare
        // `self.token()` would recurse into THIS trait method.
        crate::minter::CredentialSource::token(self).await
    }
    fn invalidate(&self) {
        // Deref past the Arc to the inherent method (the trait is impl'd on `Arc<_>`, so a
        // bare `self.invalidate()` would recurse into THIS method).
        (**self).invalidate()
    }
}

/// An injectable sleep seam so the backoff machine ([`EgressSubscriber::run`]) is
/// unit-testable without real time (the test sleeper records the requested waits and
/// trips shutdown). Production uses [`TokioSleeper`].
#[async_trait::async_trait]
trait Sleeper: Send + Sync {
    async fn sleep(&self, d: Duration);
}

struct TokioSleeper;

#[async_trait::async_trait]
impl Sleeper for TokioSleeper {
    async fn sleep(&self, d: Duration) {
        tokio::time::sleep(d).await;
    }
}

/// The pure per-iteration backoff transition (mirror `egress_handler.go:Run`'s body):
/// given the previous backoff and this connection's outcome, return `(wait, next_backoff)`.
///
///   * `Err(Unavailable)` (501/404): wait the hard `UNAVAILABLE_BACKOFF` (5m) and RESET
///     the backoff to `BASE_BACKOFF` — NO doubling (re-checks whether egress is re-enabled
///     later without hammering a disabled server every 30s).
///   * `Ok(())` (clean stream-end) OR `Err(Other)`: wait the CURRENT backoff, then double
///     it up to `MAX_BACKOFF`. A clean `Ok(())` is `!errEgressUnavailable` in Go, so it
///     ALSO backs off + doubles — Rust must NOT special-case it (the plan's finding 10).
fn backoff_step(prev: Duration, outcome: &Result<(), EgressStreamError>) -> (Duration, Duration) {
    if matches!(outcome, Err(EgressStreamError::Unavailable)) {
        (UNAVAILABLE_BACKOFF, BASE_BACKOFF)
    } else {
        let next = (prev * 2).min(MAX_BACKOFF);
        (prev, next)
    }
}

/// Build the egress stream's HTTP client (mirror `egress_handler.go:egressHTTPClient`):
/// a fingerprint-pinned transport for an https URL + pin, else a plain client. SSE is
/// long-lived, so there is NO overall request timeout.
///
/// **Fail-closed** when a pin is set on a non-`https://` URL: `Err(_)`, latched by the
/// subscriber so EVERY request errors (refusing unpinned plaintext). shed-core's
/// `build_http_client` does NOT itself fail-closed on pin+non-https — that scheme check
/// lives in `Client::new` — so the check is REPLICATED here (the plan's finding 7).
///
/// DIVERGENCE from Go (deliberate, finding 6): Go's `egressHTTPClient` follows redirects
/// with no https-only guard (an `http://` redirect would be followed UNPINNED); the Rust
/// client REFUSES non-https redirects (matching shed-core's pinned session) — a Rust-side
/// improvement kept on purpose, not parity.
pub fn egress_http_client(server_url: &str, fingerprint: &str) -> Result<reqwest::Client, String> {
    let builder = reqwest::Client::builder().redirect(reqwest::redirect::Policy::custom(|attempt| {
        if attempt.url().scheme() == "https" {
            attempt.follow()
        } else {
            attempt.stop()
        }
    }));
    if fingerprint.is_empty() {
        return builder.build().map_err(|e| e.to_string());
    }
    // Replicated scheme check — refuse to send unpinned plaintext under a pin.
    if !server_url.to_lowercase().starts_with("https://") {
        return Err(format!(
            "egress stream: TLS pin set but server URL {server_url:?} is not https; refusing unpinned plaintext"
        ));
    }
    let cfg = pinned_client_config(fingerprint).map_err(|e| e.to_string())?;
    builder
        .use_preconfigured_tls(cfg)
        .build()
        .map_err(|e| e.to_string())
}

/// Consumes a shed-server's egress-audit SSE stream and records each decision into the
/// shared [`AuditSink`] (namespace `egress`). Read-only. Mirror `EgressSubscriber`.
pub struct EgressSubscriber {
    server: String,
    /// Base URL, trailing `/` trimmed (Go's `strings.TrimRight`).
    url: String,
    /// The static open-server config token (usually empty); sent when there's no source.
    token: String,
    /// The control-token source for a secure server; `None` = open (static token).
    tokens: Option<Arc<dyn EgressTokenSource>>,
    /// The authenticated client, or the fail-closed error (pin on a non-https URL) latched
    /// so every request errors.
    http: Result<reqwest::Client, String>,
    audit: Arc<dyn AuditSink>,
    log: Arc<dyn BusLog>,
}

impl EgressSubscriber {
    /// Build a subscriber for one server target (mirror `NewEgressSubscriber`). `tokens`
    /// supplies the control-scoped token for a secure server; pass `None` for an open
    /// server (which sends the static `token`, usually empty).
    pub fn new(
        server: String,
        mut url: String,
        token: String,
        tls_fingerprint: &str,
        tokens: Option<Arc<dyn EgressTokenSource>>,
        audit: Arc<dyn AuditSink>,
        log: Arc<dyn BusLog>,
    ) -> Self {
        let http = egress_http_client(&url, tls_fingerprint);
        // Trim trailing '/' in place (Go's `strings.TrimRight(t.URL, "/")`), consuming the
        // owned `url` rather than re-allocating.
        url.truncate(url.trim_end_matches('/').len());
        EgressSubscriber {
            server,
            url,
            token,
            tokens,
            http,
            audit,
            log,
        }
    }

    /// The token to send, or `None` (send no `Authorization` header). A control token for
    /// a secure server, else the static config token (`None` when empty). A mint error →
    /// `None` (the request then 401s and `run` retries after `invalidate`). Mirror
    /// `bearer()` + the `if tok != ""` header guard.
    async fn bearer(&self) -> Option<String> {
        let tok = match &self.tokens {
            None => self.token.clone(),
            Some(src) => match src.token().await {
                Ok(t) => t,
                Err(e) => {
                    self.log.debug(&format!(
                        "egress: control-token mint failed server={} error={e}",
                        self.server
                    ));
                    String::new()
                }
            },
        };
        if tok.is_empty() {
            None
        } else {
            Some(tok)
        }
    }

    /// Make one connection and forward decisions until it errors, the stream ends, or
    /// `shutdown` flips (mirror `stream()`). `shutdown` is threaded through the connect +
    /// every read so a SIGTERM tears the loop down promptly (Go's `ctx`).
    async fn stream(
        &self,
        shutdown: &watch::Receiver<bool>,
    ) -> Result<(), EgressStreamError> {
        // Fail-closed latch: a pin on a non-https URL errors every request.
        let http = match &self.http {
            Ok(c) => c,
            Err(e) => return Err(EgressStreamError::Other(e.clone())),
        };
        let url = format!("{}{}", self.url, EGRESS_STREAM_PATH);
        let mut req = http
            .get(&url)
            .header(reqwest::header::ACCEPT, "text/event-stream");
        if let Some(tok) = self.bearer().await {
            req = req.bearer_auth(tok);
        }

        // Race the connect against shutdown so a hung dial can't block teardown.
        let resp = tokio::select! {
            _ = wait_shutdown(shutdown.clone()) => return Ok(()),
            r = req.send() => match r {
                Ok(r) => r,
                Err(e) => return Err(EgressStreamError::Other(format!("connecting: {e}"))),
            }
        };

        let st = resp.status().as_u16();
        if st != 200 {
            // 401 → invalidate the source so the NEXT reconnect re-mints (Go :147-149).
            if st == 401 {
                if let Some(src) = &self.tokens {
                    src.invalidate();
                }
            }
            // 501/404 → egress disabled → hard 5m backoff; any other non-200 → normal.
            if st == 501 || st == 404 {
                return Err(EgressStreamError::Unavailable);
            }
            return Err(EgressStreamError::Other(format!(
                "egress stream: unexpected status {st}"
            )));
        }

        let mut stream = resp.bytes_stream();
        let mut parser = SseParser::new().with_max_event_bytes(MAX_SSE_EVENT_BYTES);
        loop {
            let chunk = tokio::select! {
                _ = wait_shutdown(shutdown.clone()) => return Ok(()),
                c = stream.next() => c,
            };
            match chunk {
                // Clean EOF (Go's `sc.Err() == nil`): flush any final unterminated record,
                // then return Ok — the machine STILL backs off (finding 10).
                None => {
                    for ev in parser.finish() {
                        self.forward(&ev.data);
                    }
                    return Ok(());
                }
                Some(Err(e)) => {
                    return Err(EgressStreamError::Other(format!("reading stream: {e}")))
                }
                Some(Ok(bytes)) => {
                    let events = match parser.try_feed(&bytes) {
                        Ok(events) => events,
                        // Over the 1 MiB cap → treat as a read error and reconnect.
                        Err(e) => {
                            return Err(EgressStreamError::Other(format!("reading stream: {e}")))
                        }
                    };
                    for ev in events {
                        self.forward(&ev.data);
                    }
                }
            }
        }
    }

    /// Decode one `data:` payload as an [`EgressDecision`] and record it; a malformed
    /// frame is skipped, keeping the stream alive (Go's `continue`). The framer already
    /// stripped the `data:` prefix — so unlike the bus (which decodes `Envelope`) we
    /// decode `EgressDecision` from `ev.data`.
    fn forward(&self, data: &str) {
        // A malformed frame is skipped, keeping the stream alive (Go's `continue`).
        if let Ok(dec) = serde_json::from_str::<EgressDecision>(data) {
            self.audit.log(egress_audit_entry(&self.server, &dec));
        }
    }

    /// Stream decisions until `shutdown` flips, reconnecting with the DISTINCT egress
    /// backoff machine (mirror `Run`). Production entry — uses the real [`TokioSleeper`].
    pub async fn run(&self, shutdown: watch::Receiver<bool>) {
        self.run_with_sleeper(shutdown, &TokioSleeper).await;
    }

    /// The backoff loop with an injectable [`Sleeper`] (deterministic in tests).
    async fn run_with_sleeper(&self, shutdown: watch::Receiver<bool>, sleeper: &dyn Sleeper) {
        let mut backoff = BASE_BACKOFF;
        while !*shutdown.borrow() {
            let outcome = self.stream(&shutdown).await;
            if *shutdown.borrow() {
                return;
            }
            let (wait, next) = backoff_step(backoff, &outcome);
            match &outcome {
                Err(EgressStreamError::Unavailable) => self.log.debug(&format!(
                    "egress disabled on server; backing off server={} backoff={wait:?}",
                    self.server
                )),
                Err(e) => self.log.debug(&format!(
                    "egress stream ended; retrying error={e} backoff={backoff:?}"
                )),
                Ok(()) => {} // Go logs nothing for err == nil (the `else if err != nil`)
            }
            tokio::select! {
                _ = wait_shutdown(shutdown.clone()) => return,
                _ = sleeper.sleep(wait) => {}
            }
            backoff = next;
        }
    }
}

/// Resolve when `shutdown` is (or becomes) true; returns immediately if already flagged.
/// Idempotent + cancel-safe (reusable across select arms), mirroring the bus helper.
async fn wait_shutdown(mut shutdown: watch::Receiver<bool>) {
    let _ = shutdown.wait_for(|flagged| *flagged).await;
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::atomic::{AtomicUsize, Ordering};
    use std::sync::Mutex;

    use httpmock::prelude::*;

    // ---- test doubles ---------------------------------------------------------------

    /// Collects fanned-out audit entries.
    struct CollectingAudit(Mutex<Vec<AuditEntry>>);
    impl CollectingAudit {
        fn new() -> Arc<Self> {
            Arc::new(Self(Mutex::new(Vec::new())))
        }
        fn entries(&self) -> Vec<AuditEntry> {
            self.0.lock().unwrap().clone()
        }
    }
    impl AuditSink for CollectingAudit {
        fn log(&self, entry: AuditEntry) {
            self.0.lock().unwrap().push(entry);
        }
    }

    struct SilentLog;
    impl BusLog for SilentLog {
        fn info(&self, _: &str) {}
        fn warn(&self, _: &str) {}
        fn debug(&self, _: &str) {}
        fn error(&self, _: &str) {}
    }
    fn silent_log() -> Arc<dyn BusLog> {
        Arc::new(SilentLog)
    }

    /// A fixed-token [`EgressTokenSource`] counting `invalidate` (mirror Go's
    /// `fakeTokenSource`). Feature-independent.
    struct FakeTokenSource {
        token: String,
        err: Option<String>,
        invalidated: AtomicUsize,
    }
    impl FakeTokenSource {
        fn ok(token: &str) -> Arc<Self> {
            Arc::new(Self {
                token: token.to_string(),
                err: None,
                invalidated: AtomicUsize::new(0),
            })
        }
        fn erroring() -> Arc<Self> {
            Arc::new(Self {
                token: String::new(),
                err: Some("mint failed".into()),
                invalidated: AtomicUsize::new(0),
            })
        }
        fn invalidated(&self) -> usize {
            self.invalidated.load(Ordering::SeqCst)
        }
    }
    #[async_trait::async_trait]
    impl EgressTokenSource for FakeTokenSource {
        async fn token(&self) -> Result<String, String> {
            match &self.err {
                Some(e) => Err(e.clone()),
                None => Ok(self.token.clone()),
            }
        }
        fn invalidate(&self) {
            self.invalidated.fetch_add(1, Ordering::SeqCst);
        }
    }

    fn subscriber(
        url: &str,
        token: &str,
        tokens: Option<Arc<dyn EgressTokenSource>>,
        audit: Arc<dyn AuditSink>,
    ) -> EgressSubscriber {
        EgressSubscriber::new(
            "srv".into(),
            url.into(),
            token.into(),
            "",
            tokens,
            audit,
            silent_log(),
        )
    }

    /// A shutdown receiver that never fires. The sender is intentionally leaked so
    /// `wait_shutdown` blocks forever (a dropped sender would resolve it immediately,
    /// aborting the network await as if shutdown had fired) — for tests whose stream
    /// path must actually reach the mock server. Mirrors bus.rs's `never_shutdown`.
    fn never_shutdown() -> watch::Receiver<bool> {
        let (tx, rx) = watch::channel(false);
        let _ = Box::leak(Box::new(tx));
        rx
    }

    // ---- egress_audit_entry (mirror TestEgressAuditEntry[_NoResolvedIP] + goldens) ---

    #[test]
    fn egress_audit_entry_full() {
        let d = EgressDecision {
            ts: parse_rfc3339_to_unix("2020-01-01T00:00:00Z").unwrap(),
            shed: "web".into(),
            host: "evil.com".into(),
            port: 443,
            resolved_ip: "1.2.3.4".into(),
            protocol: "https".into(),
            verdict: "deny".into(),
            reason: "default-deny".into(),
        };
        let e = egress_audit_entry("srv", &d);
        assert_eq!(e.server, "srv");
        assert_eq!(e.shed, "web");
        assert_eq!(e.ns, "egress");
        assert_eq!(e.op, "https");
        assert_eq!(e.result, "deny");
        assert_eq!(e.reason, "default-deny");
        assert_eq!(e.detail, "evil.com:443 (1.2.3.4)");
        assert_eq!(e.ts, "2020-01-01T00:00:00Z");
        // code/approval/decided_by/scope/ttl all empty.
        assert_eq!(e.approval, "");
        assert_eq!(e.code, "");
    }

    #[test]
    fn egress_audit_entry_no_resolved_ip() {
        let d = EgressDecision {
            shed: "web".into(),
            host: "x.com".into(),
            port: 80,
            protocol: "http".into(),
            verdict: "allow".into(),
            ..Default::default()
        };
        let e = egress_audit_entry("srv", &d);
        assert_eq!(e.detail, "x.com:80"); // no " (ip)" suffix
    }

    #[test]
    fn egress_audit_entry_empty_ts_blank() {
        // Absent ts → ts:"" (the sink stamps now downstream).
        let d = EgressDecision {
            host: "a.com".into(),
            port: 1,
            ..Default::default()
        };
        assert_eq!(egress_audit_entry("srv", &d).ts, "");
    }

    #[test]
    fn egress_audit_entry_offset_ts_normalized_utc() {
        // Rust-stronger: a non-UTC offset ts renders as its UTC numbers + Z (an
        // offset-ignoring formatter would pass Go's UTC-only vector).
        let d = EgressDecision {
            ts: parse_rfc3339_to_unix("2020-01-01T02:00:00+02:00").unwrap(),
            host: "evil.com".into(),
            port: 443,
            protocol: "https".into(),
            verdict: "deny".into(),
            ..Default::default()
        };
        assert_eq!(egress_audit_entry("srv", &d).ts, "2020-01-01T00:00:00Z");
    }

    #[test]
    fn egress_decision_malformed_ts_skips_frame() {
        // A present-but-bad ts fails deserialization (mirrors Go's time.Time unmarshal
        // error → the whole frame is skipped), NOT a blank-ts entry.
        assert!(serde_json::from_str::<EgressDecision>(
            r#"{"ts":"not-a-time","host":"x","port":1}"#
        )
        .is_err());
        // An empty-string ts is likewise a parse error (Go time.Time can't parse "").
        assert!(serde_json::from_str::<EgressDecision>(r#"{"ts":"","host":"x","port":1}"#).is_err());
        // An ABSENT ts is fine → None.
        let d: EgressDecision =
            serde_json::from_str(r#"{"host":"x","port":1}"#).expect("absent ts is valid");
        assert_eq!(d.ts, None);
    }

    // ---- golden runner (Rust half of egress_audit_entry.json) -----------------------

    #[test]
    fn golden_egress_audit_entry() {
        let path = std::path::PathBuf::from(env!("CARGO_MANIFEST_DIR"))
            .join("../../tests/host-agent-diff/fixtures/egress_audit_entry.json");
        let raw = std::fs::read_to_string(&path).expect("read golden fixture");
        let fx: serde_json::Value = serde_json::from_str(&raw).unwrap();
        assert_eq!(fx["protocol_version"], 1, "version skew");
        let vectors = fx["vectors"].as_array().unwrap();
        assert!(!vectors.is_empty(), "no egress golden vectors");
        for v in vectors {
            let name = v["name"].as_str().unwrap();
            let server = v["server"].as_str().unwrap();
            let dec: EgressDecision =
                serde_json::from_value(v["decision"].clone()).expect("decode decision");
            let entry = egress_audit_entry(server, &dec);
            // Compare the durable JSONL wire object (incl. the "approval":"" byte),
            // parsed so key order is irrelevant — the same shape the Go runner marshals.
            let got: serde_json::Value =
                serde_json::from_str(&crate::audit::to_jsonl(&entry)).unwrap();
            assert_eq!(got, v["expected"], "egress golden vector {name:?}");
        }
    }

    // ---- backoff machine (Go code path, no direct Go test) --------------------------

    #[test]
    fn backoff_unavailable_hard_5m_and_resets() {
        // 501/404 → hard 5m wait, reset to base, NO doubling — from base AND mid-ramp.
        assert_eq!(
            backoff_step(BASE_BACKOFF, &Err(EgressStreamError::Unavailable)),
            (UNAVAILABLE_BACKOFF, BASE_BACKOFF)
        );
        assert_eq!(
            backoff_step(
                Duration::from_secs(8),
                &Err(EgressStreamError::Unavailable)
            ),
            (UNAVAILABLE_BACKOFF, BASE_BACKOFF)
        );
    }

    #[test]
    fn backoff_normal_error_exponential_no_held_reset() {
        // Other error → wait current, double to the 30s cap; NO 60s-held-reset. A clean
        // Ok(()) ALSO backs off + doubles (finding 10).
        let err = || Err(EgressStreamError::Other("boom".into()));
        assert_eq!(backoff_step(BASE_BACKOFF, &err()), (Duration::from_secs(1), Duration::from_secs(2)));
        assert_eq!(backoff_step(Duration::from_secs(2), &err()), (Duration::from_secs(2), Duration::from_secs(4)));
        assert_eq!(backoff_step(Duration::from_secs(16), &err()), (Duration::from_secs(16), MAX_BACKOFF));
        assert_eq!(backoff_step(MAX_BACKOFF, &err()), (MAX_BACKOFF, MAX_BACKOFF));
        // A clean stream-end is !unavailable → still exp-backs-off (no no-backoff branch).
        assert_eq!(backoff_step(BASE_BACKOFF, &Ok(())), (Duration::from_secs(1), Duration::from_secs(2)));
    }

    /// Records requested waits + trips shutdown after `stop_after` sleeps so `run`
    /// terminates deterministically without real time.
    struct RecordingSleeper {
        waits: Mutex<Vec<Duration>>,
        stop_after: usize,
        shutdown: watch::Sender<bool>,
    }
    #[async_trait::async_trait]
    impl Sleeper for RecordingSleeper {
        async fn sleep(&self, d: Duration) {
            let mut w = self.waits.lock().unwrap();
            w.push(d);
            if w.len() >= self.stop_after {
                let _ = self.shutdown.send(true);
            }
        }
    }

    #[tokio::test]
    async fn run_drives_exponential_backoff_then_stops_on_shutdown() {
        // A fail-closed subscriber (pin on a non-https URL) errors every stream() with a
        // non-unavailable Other, so `run` exercises the exp-backoff sequence with no
        // network. The recording sleeper trips shutdown after 2 sleeps.
        let audit = CollectingAudit::new();
        let sub = EgressSubscriber::new(
            "srv".into(),
            "http://127.0.0.1:1".into(),
            String::new(),
            "sha256:deadbeef", // pin + non-https → fail-closed → Other error each iter
            None,
            audit,
            silent_log(),
        );
        assert!(sub.http.is_err(), "pin on non-https must latch fail-closed");
        let (tx, rx) = watch::channel(false);
        let sleeper = RecordingSleeper {
            waits: Mutex::new(Vec::new()),
            stop_after: 2,
            shutdown: tx,
        };
        sub.run_with_sleeper(rx, &sleeper).await;
        // 1s then 2s — exponential doubling, no held-reset.
        assert_eq!(
            *sleeper.waits.lock().unwrap(),
            vec![Duration::from_secs(1), Duration::from_secs(2)]
        );
    }

    // ---- stream over a loopback mock (mirror TestEgressSubscriber_*) -----------------

    const DECISION_FRAME: &str = "data: {\"ts\":\"2020-01-01T00:00:00Z\",\"shed\":\"web\",\"host\":\"evil.com\",\"port\":443,\"protocol\":\"https\",\"verdict\":\"deny\",\"reason\":\"default-deny\"}\n\n";

    #[tokio::test]
    async fn stream_forwards_decisions_over_sse() {
        let server = MockServer::start_async().await;
        server
            .mock_async(|w, t| {
                w.method(GET)
                    .path(EGRESS_STREAM_PATH)
                    .header("authorization", "Bearer tok");
                t.status(200)
                    .header("content-type", "text/event-stream")
                    .body(DECISION_FRAME);
            })
            .await;
        let audit = CollectingAudit::new();
        let sub = subscriber(&server.base_url(), "tok", None, audit.clone());
        sub.stream(&never_shutdown()).await.expect("clean stream end");
        let entries = audit.entries();
        assert_eq!(entries.len(), 1);
        assert_eq!(entries[0].ns, "egress");
        assert_eq!(entries[0].shed, "web");
        assert_eq!(entries[0].result, "deny");
    }

    #[tokio::test]
    async fn malformed_frame_skipped_stream_continues() {
        let body = format!("data: not-json\n\n{DECISION_FRAME}");
        let server = MockServer::start_async().await;
        server
            .mock_async(|w, t| {
                w.method(GET).path(EGRESS_STREAM_PATH);
                t.status(200).body(body);
            })
            .await;
        let audit = CollectingAudit::new();
        let sub = subscriber(&server.base_url(), "", None, audit.clone());
        sub.stream(&never_shutdown()).await.expect("clean stream end");
        // Only the good frame fanned out.
        let entries = audit.entries();
        assert_eq!(entries.len(), 1);
        assert_eq!(entries[0].shed, "web");
    }

    #[tokio::test]
    async fn sends_control_token_not_credentials() {
        let server = MockServer::start_async().await;
        // Matches only when the CONTROL token (not the static creds token) is sent.
        let m = server
            .mock_async(|w, t| {
                w.method(GET)
                    .path(EGRESS_STREAM_PATH)
                    .header("authorization", "Bearer ctl-tok");
                t.status(200); // empty body → stream returns promptly
            })
            .await;
        let audit = CollectingAudit::new();
        let sub = subscriber(
            &server.base_url(),
            "creds-tok",
            Some(FakeTokenSource::ok("ctl-tok")),
            audit,
        );
        sub.stream(&never_shutdown()).await.expect("clean stream end");
        m.assert_async().await;
    }

    #[tokio::test]
    async fn status_401_invalidates_source() {
        let server = MockServer::start_async().await;
        server
            .mock_async(|w, t| {
                w.method(GET).path(EGRESS_STREAM_PATH);
                t.status(401);
            })
            .await;
        let fake = FakeTokenSource::ok("ctl-tok");
        let audit = CollectingAudit::new();
        let sub = subscriber(&server.base_url(), "", Some(fake.clone()), audit);
        let err = sub.stream(&never_shutdown()).await.unwrap_err();
        // 401 → invalidate (so the next reconnect re-mints) + a NON-unavailable error.
        assert!(matches!(err, EgressStreamError::Other(_)));
        assert_eq!(fake.invalidated(), 1);
    }

    #[tokio::test]
    async fn status_501_returns_unavailable() {
        let server = MockServer::start_async().await;
        server
            .mock_async(|w, t| {
                w.method(GET).path(EGRESS_STREAM_PATH);
                t.status(501);
            })
            .await;
        let sub = subscriber(&server.base_url(), "", None, CollectingAudit::new());
        assert!(matches!(
            sub.stream(&never_shutdown()).await,
            Err(EgressStreamError::Unavailable)
        ));
    }

    #[tokio::test]
    async fn status_404_returns_unavailable() {
        // Rust-stronger: Go's _DisabledReturnsUnavailable exercises only 501.
        let server = MockServer::start_async().await;
        server
            .mock_async(|w, t| {
                w.method(GET).path(EGRESS_STREAM_PATH);
                t.status(404);
            })
            .await;
        let sub = subscriber(&server.base_url(), "", None, CollectingAudit::new());
        assert!(matches!(
            sub.stream(&never_shutdown()).await,
            Err(EgressStreamError::Unavailable)
        ));
    }

    #[test]
    fn pin_on_plain_url_fails_closed() {
        // A pin set on an http:// URL → the client build fails closed (the scheme check
        // replicated here, since build_http_client does not itself fail closed).
        assert!(egress_http_client("http://localhost:8080", "sha256:deadbeef").is_err());
        // Sanity: a pin on https builds, and no pin builds.
        assert!(egress_http_client("http://localhost:8080", "").is_ok());
    }

    #[tokio::test]
    async fn open_server_sends_static_token_or_none() {
        let audit = CollectingAudit::new();
        // tokens=None + non-empty static token → that token.
        let sub = subscriber("http://x", "static-tok", None, audit.clone());
        assert_eq!(sub.bearer().await, Some("static-tok".to_string()));
        // tokens=None + empty static token → no header.
        let sub = subscriber("http://x", "", None, audit.clone());
        assert_eq!(sub.bearer().await, None);
        // A source that errors on mint → no header (→ 401 → retry).
        let sub = subscriber("http://x", "ignored", Some(FakeTokenSource::erroring()), audit);
        assert_eq!(sub.bearer().await, None);
    }
}
