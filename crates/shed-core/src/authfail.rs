//! "The server refused our credential" — the trigger for a reactive re-mint.
//!
//! The Rust sibling of Go's `internal/clienttoken/authfail.go`, and it has to
//! answer the same question for the same three situations:
//!
//!   * token mode — an HTTP **401**;
//!   * mtls mode, TLS 1.2 — the server rejects the client certificate inside the
//!     handshake, so the failure surfaces while CONNECTING, with no HTTP response
//!     at all;
//!   * mtls mode, TLS 1.3 — the client finishes its half of the handshake
//!     optimistically and only learns of the rejection when the alert arrives, so
//!     the failure surfaces on the first request SENT over the connection.
//!
//! It is applied REGARDLESS of the mode the client believes it is in: an entry
//! that says "token" against a server flipped to mtls sees a TLS alert, and an
//! entry that says "mtls" against a server flipped back to token sees a 401.
//! Keying the trigger on the observed failure rather than the recorded mode is
//! what makes a server-side mode flip recoverable in both directions with no
//! operator action (plan 001 D5).
//!
//! # Empirically derived shapes (do not edit from memory)
//!
//! Every string below was READ OFF a live handshake by `src/testtls.rs` —
//! a real rustls listener requiring a client certificate, plus a raw-socket
//! emitter that answers a ClientHello with one chosen TLS alert (that second
//! shape is what covers alerts a rustls server never sends but the **Go**
//! shed-server does). `alert_renderings_are_still_what_this_module_matches`
//! re-derives them on every test run, so a rustls rewording fails loudly here
//! instead of silently degrading this classifier to "401 only".
//!
//! Findings, rustls 0.23 + reqwest 0.12 + hyper 1.x:
//!
//! 1. **`reqwest::Error`'s own `Display` carries NO alert information** — it is
//!    always `error sending request for url (…)`. The alert lives two or three
//!    levels down the `source()` chain. Any classifier that looks only at
//!    `err.to_string()` is dead code. This is why [`flatten`] exists and why
//!    `http.rs` stores the FLATTENED chain in `ShedError::Transport`.
//! 2. The alert renders as `received fatal alert: <Description>`, where
//!    `<Description>` is rustls's `AlertDescription` in Debug form —
//!    `CertificateRequired`, `CertificateExpired`, `UnknownCA`, … (noun-first,
//!    unlike Go's adjective-first `expired certificate`).
//! 3. TLS **1.2**: rejection arrives during connect —
//!    `client error (Connect)` → `received fatal alert: …`.
//!    TLS **1.3**: rejection arrives after the client's handshake completes —
//!    `client error (SendRequest)` → `connection error` → `received fatal alert: …`.
//!    Both must classify identically, which is why the chain is scanned rather
//!    than any single level being matched.
//! 4. Observed per case (both TLS versions unless noted):
//!    no certificate presented → `CertificateRequired` (a **Go** server sends
//!    `HandshakeFailure` here on TLS 1.2 — see below); expired certificate →
//!    `CertificateExpired`; certificate from a foreign CA → `UnknownCA`;
//!    de-authorized identity (the Go allowlist check) → `AccessDenied`.
//! 5. Negative controls, both confirmed to render WITHOUT the alert prefix:
//!    a pin mismatch is a LOCAL verifier rejection
//!    (`unexpected error: leaf certificate does not match pin …`) and a dead port
//!    is `tcp connect error` → `Connection refused`.
//! 6. **The ambiguous shape — and its asymmetry.** Under TLS 1.3 the rejection
//!    sometimes races the pool checkout — hyper's dispatch cancelling the
//!    request rather than handing it to the connection — and reqwest reports
//!    `client error (Canceled): operation was canceled: connection was not ready`,
//!    with the alert nowhere in the chain — observed once in ~100 handshakes
//!    on an idle box, never on TLS 1.2, but MUCH more often on a CPU-starved
//!    one (a concurrent `cargo build --release`, e.g.: see `load-run.txt`).
//!    It is NOT classified as an auth failure: it is equally what a server
//!    restart or a network blip produces, and a false positive costs a real
//!    SSH mint (which, on desktop, can raise a Touch ID prompt). It is
//!    instead recognized by [`is_connection_lost_message`] as "the request
//!    was never dispatched", which `http.rs` (and `shed-broker`'s bus)
//!    answer with ONE plain re-send on a fresh connection — safe because
//!    this rendering specifically proves the request never reached the wire.
//!    **The same hyper `Kind::Canceled` category also renders as**
//!    `operation was canceled: connection closed` (`client/dispatch.rs`,
//!    raised when the connection task had ALREADY taken the request before
//!    being cancelled) — the same race, one step later, where a re-send
//!    could duplicate a write. [`is_connection_lost_message`] deliberately
//!    does NOT match this second rendering — see [`CONNECTION_LOST`]'s own
//!    doc — so production callers keep their "nothing was written" guarantee.
//!    Only a test that can independently prove nothing it sends ever reaches
//!    a real peer (this module's own live-handshake tests, rejected before
//!    the server does anything with the request) may retry on both.
//!
//! # The alert-40 exclusion (same call as Go, same reasoning)
//!
//! `HandshakeFailure` (40) is deliberately NOT in the allowlist. A Go TLS 1.2
//! server answers "no client certificate presented" with alert 40, but alert 40
//! is equally what a genuine negotiation failure produces — no shared cipher,
//! curve, or version. Treating it as an auth failure would make an unfixable
//! misconfiguration re-enroll over SSH on every request, forever, and still fail.
//! The case it would have caught is covered twice over:
//!
//!   * a rustls client and a Go `MinVersion: TLS12`-no-maximum server always
//!     negotiate TLS 1.3, where the no-certificate case is the unambiguous
//!     `CertificateRequired`;
//!   * "we hold no credential at all" never needs an alert to be discovered — a
//!     provider that can mint but holds nothing usable enrolls BEFORE the first
//!     request ([`crate::token::ControlTokenProvider::credential`]). The alert
//!     path only has to catch rejection of a certificate we DO hold, and every
//!     one of those produces a specific alert in both TLS versions.

use std::error::Error;

/// The prefix rustls puts on a received alert. Everything after it is the alert
/// description in `AlertDescription` Debug form.
const ALERT_PREFIX: &str = "received fatal alert: ";

/// Alert descriptions that mean "your certificate is missing, unacceptable, or no
/// longer valid". An ALLOWLIST: an alert not named here is not a credential
/// problem (see the module docs on the alert-40 exclusion).
const AUTH_ALERTS: &[&str] = &[
    "certificaterequired", // 116 — TLS 1.3 (and rustls TLS 1.2): no cert presented
    "badcertificate",      // 42
    "unsupportedcertificate", // 43
    "certificaterevoked",  // 44
    "certificateexpired",  // 45 — observed for an expired cert, TLS 1.2 AND 1.3
    "certificateunknown",  // 46
    "unknownca",           // 48 — observed for a foreign CA, TLS 1.2 AND 1.3
    "accessdenied",        // 49 — the Go server's "identity is not authorized"
];

/// The same conditions as raw alert numbers, for the day a TLS stack renders an
/// alert it has no name for (rustls prints `Unknown(116)`). Costs nothing and
/// keeps the classifier from silently degrading to "401 only".
const AUTH_ALERT_CODES: &[u8] = &[42, 43, 44, 45, 46, 48, 49, 116];

/// HTTP status that means "credential refused" — in token mode, and in mtls mode
/// when the server's per-request re-validation rejects an expired or
/// de-authorized certificate on an already-established connection.
pub const UNAUTHORIZED: u16 = 401;

/// Flatten an error and its `source()` chain into one line.
///
/// Load-bearing, not cosmetic: `reqwest::Error`'s own `Display` says only
/// `error sending request for url (…)`, so a transport error that is NOT
/// flattened loses both the reason a human needs and the alert this module
/// classifies on.
pub fn flatten(err: &(dyn Error + 'static)) -> String {
    let mut out = err.to_string();
    let mut src = err.source();
    // Bounded: an error chain is a handful of levels; the cap is purely
    // defensive against a pathological cyclic-looking wrapper.
    for _ in 0..8 {
        let Some(e) = src else { break };
        let text = e.to_string();
        if !out.contains(&text) {
            out.push_str(": ");
            out.push_str(&text);
        }
        src = e.source();
    }
    out
}

/// Does this (already flattened) transport-error message name a TLS alert that
/// means our certificate was refused?
///
/// Anchored on the `received fatal alert: ` prefix, which is what distinguishes
/// an alert the PEER sent from a local failure that merely mentions certificates
/// — a pin mismatch, say, which must never trigger a re-mint.
pub fn is_auth_shaped_message(msg: &str) -> bool {
    let mut rest = msg;
    while let Some(i) = rest.find(ALERT_PREFIX) {
        let tail = &rest[i + ALERT_PREFIX.len()..];
        if matches_auth_alert(tail) {
            return true;
        }
        rest = tail;
    }
    false
}

/// Match one alert rendering (possibly with trailing context) against the
/// allowlist, by name or by number.
fn matches_auth_alert(tail: &str) -> bool {
    let token: String = tail
        .chars()
        .take_while(|c| c.is_ascii_alphanumeric() || *c == '(' || *c == ')')
        .flat_map(char::to_lowercase)
        .collect();
    if AUTH_ALERTS.iter().any(|a| token.starts_with(a)) {
        return true;
    }
    // `Unknown(116)` — a stack that has no name for the alert.
    if let Some(open) = token.find('(') {
        if let Some(close) = token[open..].find(')') {
            if let Ok(code) = token[open + 1..open + close].parse::<u8>() {
                return AUTH_ALERT_CODES.contains(&code);
            }
        }
    }
    false
}

/// The hyper/reqwest rendering of "the connection died before this request
/// COULD be dispatched" — see finding 6 in the module docs. Deliberately
/// narrow: hyper's `Kind::Canceled` category (`Display` "operation was
/// canceled") covers TWO renderings of the same dispatch-cancellation race,
/// and only this one — `client/conn/http{1,2}.rs`'s "connection was not
/// ready", raised when the connection task had not yet taken the request —
/// proves the request was never handed to the wire. The other rendering,
/// `client/dispatch.rs`'s "connection closed", is raised when the connection
/// task ALREADY HAD the request, so the write may have happened; matching on
/// the bare "operation was canceled" description (as an earlier version of
/// this constant did) would make [`is_connection_lost_message`]'s callers
/// re-send a request that could have reached the peer — for
/// `shed-broker/src/bus.rs`'s POST of a plugin-listener response, a real
/// duplicate-delivery bug, not a cosmetic one. Do not widen this past what a
/// caller may safely act on twice; a test that needs to recognize BOTH
/// renderings (because IT can prove neither ever reached its own server —
/// see `tests::is_ambiguous_canceled`) has its own, test-local predicate for
/// that.
const CONNECTION_LOST: &str = "connection was not ready";

/// Was the request never actually sent, because the connection it was queued on
/// went away first?
///
/// Two properties make the answer actionable: re-sending is SAFE for any method
/// (nothing was written), and the ambiguity resolves itself — a fresh connection
/// either works or produces the deterministic alert this module can classify.
pub fn is_connection_lost_message(msg: &str) -> bool {
    msg.contains(CONNECTION_LOST)
}

/// The single decision both the CLI-shaped and streaming paths ask for: does this
/// request outcome mean the server refused our credential, so exactly one silent
/// re-mint is worth attempting?
///
/// `status` is a completed response's status (or `None` when the request never
/// produced one); `err` is the failure, whose `Transport` message is expected to
/// be a [`flatten`]ed chain.
///
/// True for a 401 and for a peer TLS alert naming a certificate problem. False
/// for everything else, including 403 (authenticated but not permitted — a
/// re-mint of the same identity cannot help), connection refused, DNS failures,
/// timeouts, decode errors, and a pin mismatch.
pub fn is_auth_failure(status: Option<u16>, err: Option<&crate::http::ShedError>) -> bool {
    if status == Some(UNAUTHORIZED) {
        return true;
    }
    match err {
        Some(crate::http::ShedError::BadStatus(code)) => *code == UNAUTHORIZED,
        Some(crate::http::ShedError::Transport(msg)) => is_auth_shaped_message(msg),
        _ => false,
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::http::ShedError;
    use crate::testtls::*;
    use std::sync::Arc;

    /// The client-error stage a flattened transport error names, classified
    /// from the text rather than re-derived by every call site. `Connect` is a
    /// TLS-handshake-time rejection, `SendRequest` a post-handshake one, and
    /// `Canceled` is hyper's dispatch being cancelled when a TLS 1.3
    /// post-handshake alert lands after the request was already handed to the
    /// connection task (module docs, finding 6) — a legitimate rendering of
    /// the SAME rejection, not a different failure. `None` for a message that
    /// names none of the three: the alert/negative-control tests below only
    /// ever read `.message`, so they never have to care.
    #[derive(Debug, Clone, Copy, PartialEq, Eq)]
    enum Stage {
        Connect,
        SendRequest,
        Canceled,
    }

    impl Stage {
        fn classify(msg: &str) -> Option<Stage> {
            if msg.contains("(Connect)") {
                Some(Stage::Connect)
            } else if msg.contains("(SendRequest)") {
                Some(Stage::SendRequest)
            } else if msg.contains("Canceled") {
                Some(Stage::Canceled)
            } else {
                None
            }
        }
    }

    /// [`probe_once`]'s error: the flattened message every assertion below
    /// pattern-matches, plus the [`Stage`] classified from it — computed once
    /// alongside the message instead of re-derived per caller.
    struct ProbeError {
        message: String,
        stage: Option<Stage>,
    }

    impl ProbeError {
        fn new(message: String) -> Self {
            let stage = Stage::classify(&message);
            ProbeError { message, stage }
        }
    }

    /// Recognizes EITHER rendering of hyper's `Kind::Canceled` dispatch race
    /// (module docs, finding 6) — [`is_connection_lost_message`]'s
    /// "connection was not ready", plus its sibling "connection closed"
    /// that [`is_connection_lost_message`] deliberately excludes (see
    /// [`CONNECTION_LOST`]'s own doc). Test-local and NOT a relaxation of
    /// that production function: production cannot tell whether a real peer
    /// received the write before the cancellation landed, so it only
    /// retries the rendering that proves it didn't. This module's own
    /// live-handshake tests have a guarantee production lacks — the TLS
    /// handshake itself is what the server rejected, so no request body was
    /// ever accepted regardless of which rendering comes back — which is
    /// what makes retrying on both safe HERE without proving anything about
    /// `http.rs` or `shed-broker`'s bus.
    fn is_ambiguous_canceled(msg: &str) -> bool {
        // Exactly the two renderings finding 6 documents, and no wider. A bare
        // `contains("operation was canceled")` would swallow every future
        // `Kind::Canceled` reason hyper invents, and this ladder retries 200
        // times — so an unrelated cancellation would be retried into silence
        // instead of failing loudly. Same discipline as `CONNECTION_LOST`
        // itself: match what was measured, not the whole category.
        is_connection_lost_message(msg) || msg.contains("operation was canceled: connection closed")
    }

    /// Retry `attempt` while `is_retryable` holds on its error, up to
    /// `extra_attempts` more times. The one ladder every retry in this module
    /// shares, replacing what used to be two near-identical copies of it
    /// (`probe`'s own retry over [`probe_once`], and a second one inlined in
    /// `reqwest_display_hides_the_alert_and_flatten_recovers_it` over a raw
    /// `reqwest::Client`): under TLS 1.3 the rejection occasionally races the
    /// pool checkout and reqwest reports an ambiguous `Kind::Canceled`
    /// rendering (module docs, finding 6) instead of the deterministic
    /// alert. `is_retryable` is supplied per call site rather than fixed
    /// here — production (`http.rs`) and this module's own tests are NOT
    /// allowed the same predicate; see [`is_ambiguous_canceled`]'s doc for
    /// why. It cannot manufacture a false pass: the retried attempt still
    /// has to land on one of the shapes this module actually claims to
    /// know, or the final assertion fails same as ever.
    async fn retry_while<T, E, F, Fut>(
        extra_attempts: usize,
        is_retryable: impl Fn(&E) -> bool,
        mut attempt: F,
    ) -> Result<T, E>
    where
        F: FnMut() -> Fut,
        Fut: std::future::Future<Output = Result<T, E>>,
    {
        let mut outcome = attempt().await;
        for _ in 0..extra_attempts {
            match &outcome {
                Err(e) if is_retryable(e) => outcome = attempt().await,
                _ => break,
            }
        }
        outcome
    }

    /// [`probe_once`] via [`retry_while`], re-sending on EITHER ambiguous
    /// `Kind::Canceled` rendering — [`is_ambiguous_canceled`], not the
    /// narrower production [`is_connection_lost_message`] — because this
    /// probe drives a handshake the SERVER rejects, so no request body is
    /// ever accepted regardless of which rendering comes back (see
    /// [`is_ambiguous_canceled`]'s own doc for why that guarantee doesn't
    /// extend to `http.rs`, which retries only the rendering that proves
    /// nothing was written). Returns the flattened message alone; a caller
    /// that also needs the classified [`Stage`] (only the TLS-version
    /// assertions below) calls [`probe_with_stage`] instead of re-parsing the
    /// string a second time.
    async fn probe(pin: &str, url: &str, cert: Option<(&str, &[u8])>) -> Result<u16, String> {
        probe_with_stage(pin, url, cert)
            .await
            .map_err(|e| e.message)
    }

    /// Extra attempts a ladder takes past its first on an ambiguous
    /// `Kind::Canceled` rendering (module docs, finding 6) — many more than
    /// the one plain re-send `http.rs` makes in production: a real client
    /// hands the ambiguity back to its caller after one try, but a TEST
    /// asserting on the deterministic alert needs to actually reach it, and
    /// (unlike production) is free to keep retrying because nothing it sends
    /// here is ever accepted by a peer.
    ///
    /// **This is a bound on a pathological environment, not a latency
    /// budget** — same reasoning as `shed-app::machine`'s 15s `wait_for`
    /// deadline: the ladder stops on the FIRST deterministic attempt in the
    /// common case (a fraction of a millisecond), so a large `N` costs
    /// nothing there; it only spends time getting exhausted, and exhaustion
    /// is exactly the pathological case this constant exists to make
    /// vanishingly unlikely rather than merely rare.
    ///
    /// Sized off a MEASURED rate, not a guess (`measure_ambiguity_rate_under_
    /// load`, `load-run.txt`): under the same sustained `cargo build
    /// --release -p shed-gx` load this module's own tests run against, 500
    /// consecutive `probe_once` calls (V13, no-cert) came back ambiguous
    /// 80-85% of the time across four runs (423, 405, 401, 399 / 500) — FAR
    /// above the ~1-in-100 unloaded rate the module docs record (finding 6),
    /// because back-to-back loopback handshakes under CPU starvation give
    /// the dispatch-cancellation race far more opportunities to land inside
    /// the request-handoff window. Treating attempts as independent
    /// (borne out by every real test run recovering within `N = 20` far more
    /// often than not — a genuine cluster that never resolved would fail
    /// every run, not ~1.3% of them) and rounding the worst measured rate UP
    /// to `p = 0.85` for margin: the probability of exhausting a ladder of
    /// `N` extra attempts (`N + 1` total) is `p^(N+1)`. The previous `N = 20`
    /// (21 total) gives `0.85^21 ≈ 3.2%` — matching the observed ~1.3-2%
    /// empirical failure rate (each real test iteration runs several such
    /// ladders back to back, compounding a per-ladder chance in the low
    /// single digits into a noticeably worse per-run chance) — nowhere near
    /// enough. `N = 200` (201 total) gives `0.85^201 ≈ 6.5×10⁻¹⁵`, comfortably
    /// past a 1e-12 target with room to spare even if the true rate on some
    /// future box is worse than anything measured here (`0.90^201 ≈
    /// 6.3×10⁻¹⁰` — still negligible). At ~1ms per loopback attempt (500
    /// attempts measured in 0.50s), the worst case — full exhaustion — costs
    /// about 200ms; the common case costs a fraction of that.
    const AMBIGUOUS_RETRY_ATTEMPTS: usize = 200;

    /// Same ladder as [`probe`], keeping the classified [`Stage`] alongside
    /// the message.
    async fn probe_with_stage(
        pin: &str,
        url: &str,
        cert: Option<(&str, &[u8])>,
    ) -> Result<u16, ProbeError> {
        let outcome = retry_while(
            AMBIGUOUS_RETRY_ATTEMPTS,
            |e: &ProbeError| is_ambiguous_canceled(&e.message),
            || probe_once(pin, url, cert),
        )
        .await;
        // `retry_while` only stops early on a NON-ambiguous error (or success),
        // so an ambiguous final error here means every one of
        // `AMBIGUOUS_RETRY_ATTEMPTS + 1` attempts landed on the ambiguous shape —
        // ladder exhaustion, not a classification miss. Say so plainly: a bare
        // "connection was not ready" panic reads like the alert classifier is
        // broken, when the real story is "no alert was ever observable to
        // classify" (see [`AMBIGUOUS_RETRY_ATTEMPTS`]'s doc for how unlikely
        // this is meant to be).
        outcome.map_err(|e| {
            if is_ambiguous_canceled(&e.message) {
                ProbeError {
                    message: format!(
                        "retry ladder exhausted: all {} attempts came back as the \
                         ambiguous Kind::Canceled shape (module docs, finding 6) — no \
                         alert ever appeared in the chain to classify, which is NOT a \
                         regression in alert classification, just an astronomically \
                         unlucky run (see AMBIGUOUS_RETRY_ATTEMPTS's doc). Last message: {}",
                        AMBIGUOUS_RETRY_ATTEMPTS + 1,
                        e.message
                    ),
                    stage: e.stage,
                }
            } else {
                e
            }
        })
    }

    /// Drive a real handshake and return the flattened transport error (or the
    /// HTTP status on success), classified into a [`ProbeError`].
    async fn probe_once(
        pin: &str,
        url: &str,
        cert: Option<(&str, &[u8])>,
    ) -> Result<u16, ProbeError> {
        let resolver = Arc::new(crate::tls::ClientCertResolver::new());
        if let Some((pem, key)) = cert {
            resolver.set(Some(Arc::new(
                crate::tls::certified_key_from_pem(pem, key).unwrap(),
            )));
        }
        let cfg = crate::tls::pinned_client_config_with_client_auth(pin, resolver).unwrap();
        let client = reqwest::Client::builder()
            .use_preconfigured_tls(cfg)
            .build()
            .unwrap();
        match client.get(url).send().await {
            Ok(r) => Ok(r.status().as_u16()),
            Err(e) => Err(ProbeError::new(flatten(&e))),
        }
    }

    /// Manual calibration for [`AMBIGUOUS_RETRY_ATTEMPTS`] — NOT part of the regular
    /// gate (`#[ignore]`d): its result is only meaningful under the SAME sustained
    /// parallel-compile load the retry ladder is sized against, and doing hundreds of
    /// live TLS handshakes on every `cargo test` would be needless cost for a number
    /// that changes only when hyper/tokio/rustls internals do. Re-run it with
    ///
    ///     cargo test -p shed-core -- --ignored measure_ambiguity_rate_under_load --nocapture
    ///
    /// alongside a concurrent `cargo build --release -p shed-gx` loop (see
    /// `load-run.txt` for the exact loop) whenever this module's dependencies bump
    /// enough that the measured rate might have moved, and update
    /// [`AMBIGUOUS_RETRY_ATTEMPTS`]'s doc with the new numbers. Every attempt is
    /// classified into exactly one of "ambiguous" (the [`Stage`] this module cannot
    /// see an alert on) or "deterministic" (carries `CertificateRequired`); anything
    /// else is a real bug in this test's own setup, not data, so it panics rather
    /// than being silently counted.
    #[ignore]
    #[tokio::test(flavor = "multi_thread")]
    async fn measure_ambiguity_rate_under_load() {
        let ca = TestCa::new("shed-ca");
        let srv = spawn_mtls_server(&ca, TlsVersion::V13, false).await;
        let url = format!("{}/api/info", srv.base_url());
        const ATTEMPTS: usize = 500;
        let mut ambiguous = 0usize;
        let mut deterministic = 0usize;
        for _ in 0..ATTEMPTS {
            match probe_once(&srv.pin, &url, None).await {
                Err(e) if is_ambiguous_canceled(&e.message) => ambiguous += 1,
                Err(e) if e.message.contains("CertificateRequired") => deterministic += 1,
                Err(e) => panic!("neither ambiguous nor the expected alert: {}", e.message),
                Ok(status) => panic!("expected a TLS rejection, got HTTP {status}"),
            }
        }
        assert_eq!(ambiguous + deterministic, ATTEMPTS);
        eprintln!(
            "measure_ambiguity_rate_under_load: {ambiguous}/{ATTEMPTS} ambiguous \
             ({:.4}), {deterministic}/{ATTEMPTS} deterministic",
            ambiguous as f64 / ATTEMPTS as f64
        );
    }

    // The finding recorded in the module docs, asserted rather than remembered:
    // reqwest's own Display carries no alert, and flatten() recovers it.
    #[tokio::test(flavor = "multi_thread")]
    async fn reqwest_display_hides_the_alert_and_flatten_recovers_it() {
        let ca = TestCa::new("shed-ca");
        let srv = spawn_mtls_server(&ca, TlsVersion::V13, false).await;
        let url = format!("{}/api/info", srv.base_url());
        let cfg = crate::tls::pinned_client_config_with_client_auth(
            &srv.pin,
            Arc::new(crate::tls::ClientCertResolver::new()),
        )
        .unwrap();
        let client = reqwest::Client::builder()
            .use_preconfigured_tls(cfg)
            .build()
            .unwrap();
        // Re-send on EITHER ambiguous `Kind::Canceled` rendering via the same
        // `retry_while` ladder `probe` uses (module docs, finding 6) — safe
        // here for the same reason it's safe in `probe`: the server rejects
        // the handshake itself, so nothing this client sends is ever
        // accepted (see `is_ambiguous_canceled`'s doc). Retrying is what
        // keeps this from flaking under load rather than falsifying the
        // assertion below.
        let err = retry_while(
            AMBIGUOUS_RETRY_ATTEMPTS,
            |e: &reqwest::Error| is_ambiguous_canceled(&flatten(e)),
            || client.get(&url).send(),
        )
        .await
        .unwrap_err();
        // Same diagnostic as `probe_with_stage`'s exhaustion wrap: a bare
        // assertion failure below would read like flatten()/the alert prefix
        // regressed, when exhausting `AMBIGUOUS_RETRY_ATTEMPTS + 1` attempts
        // means no alert was ever observable in the first place.
        assert!(
            !is_ambiguous_canceled(&flatten(&err)),
            "retry ladder exhausted: all {} attempts came back as the ambiguous \
             Kind::Canceled shape (module docs, finding 6) — no alert ever appeared \
             to classify, which is NOT a regression here, just an astronomically \
             unlucky run (see AMBIGUOUS_RETRY_ATTEMPTS's doc). Last error: {err}",
            AMBIGUOUS_RETRY_ATTEMPTS + 1,
        );
        assert!(
            !err.to_string().contains("alert"),
            "reqwest Display unexpectedly carries the alert: {err}"
        );
        let flat = flatten(&err);
        assert!(
            flat.contains("received fatal alert: CertificateRequired"),
            "{flat}"
        );
        assert!(is_auth_shaped_message(&flat), "{flat}");
    }

    // The four rejection cases, at BOTH TLS versions, against a real listener.
    #[tokio::test(flavor = "multi_thread")]
    async fn real_handshake_rejections_are_auth_shaped_at_both_tls_versions() {
        let ca = TestCa::new("shed-ca");
        let foreign_ca = TestCa::new("other-ca");
        for version in [TlsVersion::V12, TlsVersion::V13] {
            let srv = spawn_mtls_server(&ca, version, false).await;
            let url = format!("{}/api/info", srv.base_url());

            // (a) no certificate presented
            let msg = probe(&srv.pin, &url, None).await.unwrap_err();
            assert!(is_auth_shaped_message(&msg), "{version:?} no-cert: {msg}");
            assert!(msg.contains("CertificateRequired"), "{version:?}: {msg}");

            // (b) a valid certificate authenticates (the positive control)
            let good = ca.client_cert("SHA256:good", "control", valid_window());
            assert_eq!(
                probe(&srv.pin, &url, Some((&good.cert_pem, &good.key_pkcs8_der)))
                    .await
                    .unwrap(),
                200
            );

            // (c) expired certificate
            let expired = ca.client_cert("SHA256:expired", "control", expired_window());
            let msg = probe(
                &srv.pin,
                &url,
                Some((&expired.cert_pem, &expired.key_pkcs8_der)),
            )
            .await
            .unwrap_err();
            assert!(is_auth_shaped_message(&msg), "{version:?} expired: {msg}");
            assert!(msg.contains("CertificateExpired"), "{version:?}: {msg}");

            // (d) certificate from a foreign CA
            let foreign = foreign_ca.client_cert("SHA256:foreign", "control", valid_window());
            let msg = probe(
                &srv.pin,
                &url,
                Some((&foreign.cert_pem, &foreign.key_pkcs8_der)),
            )
            .await
            .unwrap_err();
            assert!(is_auth_shaped_message(&msg), "{version:?} wrong-ca: {msg}");
            assert!(msg.contains("UnknownCA"), "{version:?}: {msg}");

            // TLS 1.3 rejects AFTER the client's handshake completes (SendRequest
            // stage), TLS 1.2 during connect — the shape difference the plan
            // predicted, pinned here as a typed Stage rather than a second
            // substring match. The alert CONTENT was already asserted above by
            // (a) (deterministic — CertificateRequired both ways); this only
            // pins WHEN the rejection was reported.
            let outcome = probe_with_stage(&srv.pin, &url, None).await.unwrap_err();
            match version {
                TlsVersion::V12 => {
                    assert_eq!(outcome.stage, Some(Stage::Connect), "{}", outcome.message)
                }
                // TLS 1.3 rejects post-handshake, so the failure is reported by
                // the request rather than the connect — either as SendRequest,
                // or, when it races the pool checkout, hyper's dispatch
                // Canceled (module docs, finding 6). This is a DOCUMENTED
                // two-valued outcome, not an under-specified assertion:
                // `probe_with_stage`'s own retry ladder already re-sent once
                // on the ambiguous shape, so a Canceled surviving that retry
                // is a legitimate second rendering of the same rejection, not
                // a flake.
                TlsVersion::V13 => assert!(
                    matches!(
                        outcome.stage,
                        Some(Stage::SendRequest) | Some(Stage::Canceled)
                    ),
                    "{}",
                    outcome.message
                ),
                TlsVersion::Any => unreachable!(),
            }
        }
    }

    // Every alert this module claims to know, produced as raw wire bytes — the
    // only way to cover the codes a GO server sends that a rustls server does
    // not. Re-derives the renderings so a rustls rewording fails here.
    #[tokio::test(flavor = "multi_thread")]
    async fn alert_renderings_are_still_what_this_module_matches() {
        let cases: &[(u8, &str, bool)] = &[
            (42, "BadCertificate", true),
            (43, "UnsupportedCertificate", true),
            (44, "CertificateRevoked", true),
            (45, "CertificateExpired", true),
            (46, "CertificateUnknown", true),
            (48, "UnknownCA", true),
            (49, "AccessDenied", true),
            (116, "CertificateRequired", true),
            // Deliberately NOT auth-shaped (see the module docs).
            (40, "HandshakeFailure", false),
            (80, "InternalError", false),
            (70, "ProtocolVersion", false),
        ];
        for (code, rendering, want) in cases {
            let (addr, task) = spawn_alert_server(*code).await;
            let url = format!("https://localhost:{}/api/info", addr.port());
            let msg = probe("sha256:00", &url, None).await.unwrap_err();
            task.abort();
            assert!(
                msg.contains(rendering),
                "alert {code} rendered as {msg:?}, expected to contain {rendering:?}"
            );
            assert_eq!(
                is_auth_shaped_message(&msg),
                *want,
                "alert {code} ({rendering}) classified wrong: {msg}"
            );
        }
    }

    // Negative controls that must never trigger a re-mint.
    #[tokio::test(flavor = "multi_thread")]
    async fn pin_mismatch_and_dead_port_are_not_auth_shaped() {
        let ca = TestCa::new("shed-ca");
        let srv = spawn_mtls_server(&ca, TlsVersion::Any, false).await;
        let good = ca.client_cert("SHA256:good", "control", valid_window());
        let msg = probe(
            "sha256:0000000000000000000000000000000000000000000000000000000000000000",
            &format!("{}/api/info", srv.base_url()),
            Some((&good.cert_pem, &good.key_pkcs8_der)),
        )
        .await
        .unwrap_err();
        assert!(msg.contains("does not match pin"), "{msg}");
        assert!(
            !is_auth_shaped_message(&msg),
            "pin mismatch must not re-mint: {msg}"
        );

        let msg = probe(&srv.pin, "https://localhost:1/api/info", None)
            .await
            .unwrap_err();
        assert!(!is_auth_shaped_message(&msg), "{msg}");
    }

    #[test]
    fn is_auth_failure_covers_status_and_transport() {
        assert!(is_auth_failure(Some(401), None));
        assert!(!is_auth_failure(Some(403), None));
        assert!(!is_auth_failure(Some(200), None));
        assert!(is_auth_failure(None, Some(&ShedError::BadStatus(401))));
        assert!(!is_auth_failure(None, Some(&ShedError::BadStatus(500))));
        assert!(is_auth_failure(
            None,
            Some(&ShedError::Transport(
                "client error (Connect): received fatal alert: UnknownCA".into()
            ))
        ));
        assert!(!is_auth_failure(
            None,
            Some(&ShedError::Transport("tcp connect error".into()))
        ));
        assert!(!is_auth_failure(
            None,
            Some(&ShedError::Decode("received fatal alert: UnknownCA".into())),
        ));
    }

    #[test]
    fn message_classifier_handles_numeric_and_trailing_context() {
        assert!(is_auth_shaped_message("received fatal alert: Unknown(116)"));
        assert!(!is_auth_shaped_message("received fatal alert: Unknown(40)"));
        assert!(is_auth_shaped_message(
            "connection error: received fatal alert: CertificateExpired (while reading)"
        ));
        // The prefix is required: a message that merely names a certificate
        // problem locally is not a peer rejection.
        assert!(!is_auth_shaped_message("certificate expired"));
        assert!(!is_auth_shaped_message(
            "unexpected error: leaf certificate does not match pin sha256:aa"
        ));
        assert!(!is_auth_shaped_message(""));
    }

    #[test]
    fn connection_lost_is_recognized_but_never_auth_shaped() {
        let not_ready =
            "error sending request for url (https://h/api/info): client error (Canceled): \
                   operation was canceled: connection was not ready";
        assert!(is_connection_lost_message(not_ready));
        assert!(!is_auth_shaped_message(not_ready));
        assert!(!is_connection_lost_message("tcp connect error"));
    }

    // The SAME hyper `Kind::Canceled` category as the case above, but observed
    // via `client/dispatch.rs`'s "already had the request" path rather than
    // `client/conn/http{1,2}.rs`'s "hadn't yet" one (module docs, finding 6;
    // load-run.txt is what surfaced this rendering under load). The two
    // renderings are NOT interchangeable: `is_connection_lost_message` — the
    // production, safe-to-blindly-resend predicate — must reject this one,
    // because it does NOT prove the request was never written; only the
    // test-local `is_ambiguous_canceled` (used by handshakes THIS module
    // itself rejects, where nothing is ever accepted regardless) recognizes
    // both.
    #[test]
    fn connection_closed_is_the_unsafe_canceled_rendering_production_must_not_retry() {
        let closed =
            "error sending request for url (https://h/api/info): client error (Canceled): \
                   operation was canceled: connection closed";
        assert!(
            !is_connection_lost_message(closed),
            "production must not treat this as provably-undispatched: {closed}"
        );
        assert!(!is_auth_shaped_message(closed));
        assert!(is_ambiguous_canceled(closed));
        // And the production shape is still one `is_ambiguous_canceled` too —
        // it is a superset, not a disjoint check.
        let not_ready =
            "error sending request for url (https://h/api/info): client error (Canceled): \
                   operation was canceled: connection was not ready";
        assert!(is_ambiguous_canceled(not_ready));
    }

    #[test]
    fn flatten_appends_sources_without_duplicating() {
        #[derive(Debug)]
        struct Inner;
        impl std::fmt::Display for Inner {
            fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
                write!(f, "received fatal alert: AccessDenied")
            }
        }
        impl Error for Inner {}
        #[derive(Debug)]
        struct Outer(Inner);
        impl std::fmt::Display for Outer {
            fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
                write!(f, "connection error")
            }
        }
        impl Error for Outer {
            fn source(&self) -> Option<&(dyn Error + 'static)> {
                Some(&self.0)
            }
        }
        let flat = flatten(&Outer(Inner));
        assert_eq!(flat, "connection error: received fatal alert: AccessDenied");
        assert!(is_auth_shaped_message(&flat));
    }
}
