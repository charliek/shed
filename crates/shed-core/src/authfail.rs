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
//! 6. **The ambiguous shape.** Under TLS 1.3 the rejection sometimes races the
//!    pool checkout and reqwest reports
//!    `client error (Canceled): operation was canceled: connection was not ready`
//!    with the alert nowhere in the chain — observed once in ~100 handshakes here,
//!    never on TLS 1.2. It is NOT classified as an auth failure: it is equally
//!    what a server restart or a network blip produces, and a false positive costs
//!    a real SSH mint (which, on desktop, can raise a Touch ID prompt). It is
//!    instead recognized by [`is_connection_lost_message`] as "the request was
//!    never dispatched", which `http.rs` answers with ONE plain re-send on a fresh
//!    connection — where the rejection then surfaces as the deterministic alert
//!    and the normal re-mint path takes over.
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

/// The hyper/reqwest rendering of "the connection died before this request could
/// be dispatched" — see finding 6 in the module docs.
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

    /// [`probe_once`], re-sending once on the ambiguous "connection was not
    /// ready" shape — exactly what `http.rs` does with it (module docs, finding 6).
    async fn probe(pin: &str, url: &str, cert: Option<(&str, &[u8])>) -> Result<u16, String> {
        // Up to three attempts rather than the one `http.rs` makes: production
        // hands the ambiguity back to the caller, but a TEST that hits it twice in
        // a row under parallel load would report a false regression in the alert
        // shapes it is actually pinning.
        let mut outcome = probe_once(pin, url, cert).await;
        for _ in 0..2 {
            match &outcome {
                Err(msg) if is_connection_lost_message(msg) => {
                    outcome = probe_once(pin, url, cert).await;
                }
                _ => break,
            }
        }
        outcome
    }

    /// Drive a real handshake and return the flattened transport error (or the
    /// HTTP status on success).
    async fn probe_once(pin: &str, url: &str, cert: Option<(&str, &[u8])>) -> Result<u16, String> {
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
            Err(e) => Err(flatten(&e)),
        }
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
        // Re-send on the ambiguous "connection was not ready" shape, exactly as
        // `probe` (and `http.rs`) do — finding 6: under TLS 1.3 the rejection
        // occasionally races the pool checkout and reports no alert at all, which
        // would flake this assertion rather than falsify it.
        let mut err = client.get(&url).send().await.unwrap_err();
        for _ in 0..3 {
            if !is_connection_lost_message(&flatten(&err)) {
                break;
            }
            err = client.get(&url).send().await.unwrap_err();
        }
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
            // predicted, pinned here so a regression is visible.
            let msg = probe(&srv.pin, &url, None).await.unwrap_err();
            match version {
                TlsVersion::V12 => assert!(msg.contains("Connect"), "{msg}"),
                // TLS 1.3 rejects post-handshake, so the failure is reported by
                // the request rather than the connect — either as SendRequest or,
                // when it races the pool checkout, as the ambiguous Canceled shape
                // this probe already re-sent once for (finding 6).
                TlsVersion::V13 => assert!(
                    msg.contains("SendRequest") || msg.contains("Canceled"),
                    "{msg}"
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
        let msg = "error sending request for url (https://h/api/info): client error (Canceled): \
                   operation was canceled: connection was not ready";
        assert!(is_connection_lost_message(msg));
        assert!(!is_auth_shaped_message(msg));
        assert!(!is_connection_lost_message("tcp connect error"));
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
