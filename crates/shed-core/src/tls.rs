//! TLS leaf-cert pinning for shed-server's self-signed HTTPS cert.
//!
//! The pin is `sha256:<lowercase-hex>` of the leaf cert's DER — byte-for-byte
//! with Swift's `certFingerprint`, the server (`internal/servertls.Fingerprint`),
//! and the Go clients. A pinned client accepts a handshake iff the leaf hashes
//! to the pin (chain/name checks skipped, like the Swift pinning delegate), but
//! still verifies the handshake signature against the presented cert so a
//! different key can't MITM. Fail-closed on a non-https URL is enforced by the
//! caller (`Client::new`).
//!
//! The pure `fingerprint`/`pin_matches` decision is unit-tested here; the rustls
//! handshake wiring is the production path (test mode drops the pin, so e2e can't
//! reach it).
//!
//! # The adaptive transport (plan 001 D5)
//!
//! Client identity is presented through a [`ClientCertResolver`] that is
//! installed on EVERY pinned config ([`pinned_client_config_with_client_auth`]),
//! whether or not a certificate exists yet. The resolver reads shared state at
//! handshake time: it hands back the credential provider's current certificate
//! in mtls state and `None` — "continue without client authentication" — in token
//! state. A credential rotation, and even a token↔mtls mode flip, is therefore a
//! write into that shared state, never a rebuild of the `reqwest::Client`, and no
//! layer above the transport has to own a swap.
//!
//! One case is deliberately outside that: a connection POOL holding connections
//! authenticated as an identity the server has refused. Those are stale
//! credentials rather than reusable state, and `http.rs` purges them
//! (`Client::recycle_transport`) — the analogue of Go's
//! `Transport.CloseIdleConnections()` after a refresh, which reqwest 0.12 has no
//! direct equivalent for. Rotations and mode flips that are NOT refusals still
//! keep their pool.

use std::sync::{Arc, RwLock};

use rustls::client::danger::{HandshakeSignatureValid, ServerCertVerified, ServerCertVerifier};
use rustls::client::ResolvesClientCert;
use rustls::crypto::{verify_tls12_signature, verify_tls13_signature, WebPkiSupportedAlgorithms};
use rustls::pki_types::{
    CertificateDer, PrivateKeyDer, PrivatePkcs8KeyDer, PrivateSec1KeyDer, ServerName, UnixTime,
};
use rustls::sign::CertifiedKey;
use rustls::{DigitallySignedStruct, Error as TlsError, SignatureScheme};
use sha2::{Digest, Sha256};

use crate::csr::{
    pem_decode, PEM_LABEL_CERTIFICATE, PEM_LABEL_EC_PRIVATE_KEY, PEM_LABEL_PRIVATE_KEY,
};
use crate::http::ShedError;

/// `sha256:<lowercase-hex>` of a DER-encoded cert.
pub fn fingerprint(der: &[u8]) -> String {
    let digest = Sha256::digest(der);
    let mut out = String::with_capacity(7 + digest.len() * 2);
    out.push_str("sha256:");
    for b in digest {
        out.push_str(&format!("{b:02x}"));
    }
    out
}

/// Does the leaf cert's DER hash to the expected pin?
pub fn pin_matches(leaf_der: &[u8], pin: &str) -> bool {
    fingerprint(leaf_der) == pin
}

#[derive(Debug)]
struct LeafPinVerifier {
    pin: String,
    supported: WebPkiSupportedAlgorithms,
}

impl ServerCertVerifier for LeafPinVerifier {
    fn verify_server_cert(
        &self,
        end_entity: &CertificateDer<'_>,
        _intermediates: &[CertificateDer<'_>],
        _server_name: &ServerName<'_>,
        _ocsp: &[u8],
        _now: UnixTime,
    ) -> Result<ServerCertVerified, TlsError> {
        if pin_matches(end_entity.as_ref(), &self.pin) {
            Ok(ServerCertVerified::assertion())
        } else {
            Err(TlsError::General(format!(
                "leaf certificate does not match pin {}",
                self.pin
            )))
        }
    }

    fn verify_tls12_signature(
        &self,
        message: &[u8],
        cert: &CertificateDer<'_>,
        dss: &DigitallySignedStruct,
    ) -> Result<HandshakeSignatureValid, TlsError> {
        verify_tls12_signature(message, cert, dss, &self.supported)
    }

    fn verify_tls13_signature(
        &self,
        message: &[u8],
        cert: &CertificateDer<'_>,
        dss: &DigitallySignedStruct,
    ) -> Result<HandshakeSignatureValid, TlsError> {
        verify_tls13_signature(message, cert, dss, &self.supported)
    }

    fn supported_verify_schemes(&self) -> Vec<SignatureScheme> {
        self.supported.supported_schemes()
    }
}

/// A rustls client config that pins the leaf cert to `pin`, presenting NO client
/// certificate. Uses the ring provider to match reqwest's rustls-tls stack.
///
/// Kept for callers that have no client identity to present at all — an open or
/// static-token server, which could never enroll. Everything backed by a credential
/// provider builds its config with [`pinned_client_config_with_client_auth`]
/// instead — an empty resolver behaves identically to this until a certificate
/// arrives.
pub fn pinned_client_config(pin: &str) -> Result<rustls::ClientConfig, ShedError> {
    Ok(pinned_builder(pin)?.with_no_client_auth())
}

/// A pinned rustls client config whose client identity is resolved at handshake
/// time from `resolver` — the adaptive transport's server-facing half.
///
/// Installing the resolver is unconditional: in token state it resolves to `None`
/// (rustls then continues without client authentication, byte-identically to
/// [`pinned_client_config`]), and in mtls state it resolves to the current
/// certificate. Nothing about the transport changes when the credential does.
pub fn pinned_client_config_with_client_auth(
    pin: &str,
    resolver: Arc<ClientCertResolver>,
) -> Result<rustls::ClientConfig, ShedError> {
    let mut config = pinned_builder(pin)?.with_client_cert_resolver(resolver);
    // Session RESUMPTION is disabled here, and it is load-bearing rather than
    // conservative. Found empirically (`http::mtls_tests`, which failed on it):
    // rustls resumes TLS 1.3 sessions by default, a resumed handshake sends no
    // Certificate message, and the server carries the ORIGINAL peer certificate
    // into the resumed connection. A client that had rotated — or been told to
    // stop presenting a certificate at all after a flip to token mode — kept
    // authenticating as its previous identity for as long as the ticket lived,
    // which would make both rotation and revocation quietly ineffective.
    //
    // It also restores Go parity: `crypto/tls.Config.ClientSessionCache` is nil
    // by default and the Go client never sets one, so no shed client has ever
    // resumed. The cost is one extra round trip on connections that are NEWLY
    // dialed — and shed clients keep a pooled connection alive across requests,
    // so that is a rare event rather than a per-request one.
    config.resumption = rustls::client::Resumption::disabled();
    Ok(config)
}

/// Shared tail of both config constructors: the ring provider + the leaf-pin
/// verifier, stopping one builder step short of the client-auth decision.
fn pinned_builder(
    pin: &str,
) -> Result<rustls::ConfigBuilder<rustls::ClientConfig, rustls::client::WantsClientCert>, ShedError>
{
    let provider = rustls::crypto::ring::default_provider();
    let supported = provider.signature_verification_algorithms;
    let verifier = Arc::new(LeafPinVerifier {
        pin: pin.to_lowercase(),
        supported,
    });
    let builder = rustls::ClientConfig::builder_with_provider(Arc::new(provider))
        .with_safe_default_protocol_versions()
        .map_err(|e| ShedError::Config(e.to_string()))?
        .dangerous()
        .with_custom_certificate_verifier(verifier);
    Ok(builder)
}

/// The handshake-time source of the client certificate: shared, swappable state
/// installed once on a `reqwest::Client`'s TLS config and written by the
/// credential provider on every rotation or mode flip.
///
/// An `RwLock` rather than a lock-free cell: reads happen once per HANDSHAKE (not
/// per request), writes once per mint, so contention is a non-issue and the
/// std-only implementation keeps shed-core dependency-clean.
#[derive(Debug, Default)]
pub struct ClientCertResolver {
    current: RwLock<Option<Arc<CertifiedKey>>>,
}

impl ClientCertResolver {
    /// A resolver holding no certificate — the token-state / not-yet-enrolled
    /// starting point.
    pub fn new() -> Self {
        Self::default()
    }

    /// Install (or, with `None`, withdraw) the certificate presented by
    /// subsequent handshakes. Existing pooled connections are unaffected — they
    /// already authenticated — which is exactly the semantics a rotation wants.
    pub fn set(&self, certified: Option<Arc<CertifiedKey>>) {
        // A poisoned lock (a panic inside a set/resolve) must not wedge every
        // later handshake: recover the guard and carry on, since the only
        // invariant is "holds the newest value written".
        let mut guard = self.current.write().unwrap_or_else(|e| e.into_inner());
        *guard = certified;
    }

    /// The certificate current handshakes would present, if any.
    pub fn current(&self) -> Option<Arc<CertifiedKey>> {
        self.current
            .read()
            .unwrap_or_else(|e| e.into_inner())
            .clone()
    }

    /// Test seam: hold the resolver's inner lock so any concurrent [`Self::set`]
    /// BLOCKS until the returned guard is dropped.
    ///
    /// It exists to make a lock-ORDERING bug observable
    /// (`token::tests::resolver_withdrawal_is_ordered_under_the_state_lock`):
    /// stall the resolver write and then ask whether the provider's state lock is
    /// still held. There is no production reason to reach inside the cell.
    #[cfg(test)]
    pub(crate) fn block_writes(&self) -> std::sync::RwLockReadGuard<'_, Option<Arc<CertifiedKey>>> {
        self.current.read().unwrap_or_else(|e| e.into_inner())
    }
}

impl ResolvesClientCert for ClientCertResolver {
    fn resolve(
        &self,
        _root_hint_subjects: &[&[u8]],
        _sigschemes: &[SignatureScheme],
    ) -> Option<Arc<CertifiedKey>> {
        // The hints are ignored on purpose: a shed client holds at most one
        // certificate, issued by the one CA of the one server this transport
        // talks to. Filtering by hint could only ever turn "present it" into
        // "present nothing", which is strictly worse than letting the server
        // decide.
        self.current()
    }

    fn has_certs(&self) -> bool {
        // Deliberately UNCONDITIONAL, and not `current().is_some()`.
        //
        // rustls asks this once, at ClientHello time
        // (`client/hs.rs:start_handshake`), for exactly one purpose: deciding
        // whether to RETAIN the client-auth handshake transcript, which TLS 1.2
        // needs later to sign its CertificateVerify. `resolve()` is the decision
        // point for "do I present a certificate", and returning `None` there is
        // still the documented way to decline.
        //
        // This resolver is populated dynamically — a mint can install a
        // certificate at any moment, including between the ClientHello and the
        // server's CertificateRequest. Answering "no certs" from a momentarily
        // empty cell would abandon the transcript, and the certificate that
        // arrives a millisecond later is then unusable on TLS 1.2: rustls fails
        // the handshake with `Expected transcript` instead of authenticating
        // (`client/tls12.rs:emit_certverify`). So the honest answer to "are any
        // certificates available?" for a dynamic resolver is "yes, potentially".
        //
        // The cost of always retaining the transcript is a few hundred buffered
        // bytes per handshake on connections that end up presenting nothing.
        true
    }
}

/// Build the rustls signing identity from an issued certificate (PEM, as the
/// bootstrap bundle delivers it) and the PKCS#8 private key it was issued for.
///
/// The key/cert pairing is VERIFIED here (`CertifiedKey::from_der`). That is not
/// a trust check — the certificate arrived over the host-key-pinned SSH channel
/// and its trustworthiness is established by the server's own CA at handshake
/// time — but a correctness one: a mismatched pair otherwise surfaces much later
/// as an opaque handshake failure against a server that looks broken.
pub fn certified_key_from_pem(
    cert_pem: &str,
    key_pkcs8_der: &[u8],
) -> Result<CertifiedKey, ShedError> {
    certified_key_from_der(
        cert_pem,
        PrivateKeyDer::Pkcs8(PrivatePkcs8KeyDer::from(key_pkcs8_der.to_vec())),
    )
}

/// [`certified_key_from_pem`] for a key that arrives as PEM rather than DER — the shape a
/// credential read back from a file has.
///
/// It accepts BOTH private-key labels on purpose. This crate's [`crate::csr`] writes
/// PKCS#8 (`-----BEGIN PRIVATE KEY-----`) while the Go client writes SEC1
/// (`-----BEGIN EC PRIVATE KEY-----`, `x509.MarshalECPrivateKey`). Each process owns its
/// own key and never reads another language's file (plan 001 D6's ownership table), so
/// this is not a shared-file contract — but a reader that assumed one label would reject a
/// hand-restored or externally-provisioned key with an opaque parse error instead of
/// comparing it, which is exactly the failure mode a credential loader should not have.
///
/// Anything else (an encrypted block, an RSA/PKCS#1 key, no PEM at all) is an error rather
/// than a guess — the caller treats that as "no credential" and re-enrolls.
pub fn certified_key_from_pem_pair(
    cert_pem: &str,
    key_pem: &str,
) -> Result<CertifiedKey, ShedError> {
    let key = if let Ok(der) = pem_decode(PEM_LABEL_PRIVATE_KEY, key_pem) {
        PrivateKeyDer::Pkcs8(PrivatePkcs8KeyDer::from(der))
    } else if let Ok(der) = pem_decode(PEM_LABEL_EC_PRIVATE_KEY, key_pem) {
        PrivateKeyDer::Sec1(PrivateSec1KeyDer::from(der))
    } else {
        return Err(ShedError::Config(
            "client key is not a PEM PRIVATE KEY or EC PRIVATE KEY block".into(),
        ));
    };
    certified_key_from_der(cert_pem, key)
}

/// The shared tail of both constructors: decode the leaf and pair it with an
/// already-decoded private key.
fn certified_key_from_der(
    cert_pem: &str,
    key: PrivateKeyDer<'static>,
) -> Result<CertifiedKey, ShedError> {
    let cert_der = pem_decode(PEM_LABEL_CERTIFICATE, cert_pem)?;
    let provider = rustls::crypto::ring::default_provider();
    CertifiedKey::from_der(vec![CertificateDer::from(cert_der)], key, &provider)
        .map_err(|e| ShedError::Config(format!("client certificate does not match its key: {e}")))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn an_empty_resolver_still_advertises_potential_certificates() {
        // `has_certs` gates the TLS 1.2 client-auth TRANSCRIPT, decided at
        // ClientHello time; `resolve` is what actually declines. A dynamically
        // populated resolver must answer "potentially" to the first and `None` to
        // the second, or a certificate installed after the ClientHello is
        // unusable (see the functional test below).
        let r = ClientCertResolver::new();
        assert!(r.has_certs());
        assert!(r.resolve(&[], &[]).is_none());
        assert!(r.current().is_none());
    }

    // The failure `has_certs() == current().is_some()` produced, end to end: a
    // resolver that is EMPTY when the ClientHello goes out and populated before
    // the server's CertificateRequest arrives. rustls would have abandoned the
    // transcript at ClientHello and then failed the handshake with
    // "Expected transcript" (`client/tls12.rs:emit_certverify`) instead of
    // authenticating.
    //
    // Deterministic without any race: the listener delays its half of the
    // handshake, which opens the window explicitly rather than hoping to hit one.
    #[tokio::test(flavor = "multi_thread")]
    async fn certificate_installed_mid_handshake_is_presented_on_tls12() {
        use crate::testtls::*;
        let ca = TestCa::new("shed-ca");
        let srv = spawn_server(
            &ca,
            ServerOpts {
                version: TlsVersion::V12,
                handshake_delay: std::time::Duration::from_millis(400),
                ..ServerOpts::default()
            },
        )
        .await;
        let resolver = Arc::new(ClientCertResolver::new());
        let cfg = pinned_client_config_with_client_auth(&srv.pin, resolver.clone()).unwrap();
        let client = reqwest::Client::builder()
            .use_preconfigured_tls(cfg)
            .build()
            .unwrap();

        let url = format!("{}/api/info", srv.base_url());
        let request =
            tokio::spawn(async move { client.get(&url).send().await.map(|r| r.status()) });

        // The ClientHello is on the wire and the server is still asleep: install
        // the certificate INSIDE the handshake.
        tokio::time::sleep(std::time::Duration::from_millis(100)).await;
        assert!(
            resolver.current().is_none(),
            "the certificate must arrive after the ClientHello, or this proves nothing"
        );
        let issued = ca.client_cert("SHA256:late", "control", valid_window());
        resolver.set(Some(Arc::new(
            certified_key_from_pem(&issued.cert_pem, &issued.key_pkcs8_der).unwrap(),
        )));

        let status = request
            .await
            .unwrap()
            .expect("the late certificate must complete a TLS 1.2 handshake");
        // A 200 IS the proof: the listener is `RequireAndVerifyClientCert`, so it
        // could only have been reached by presenting — and signing with — the
        // certificate installed mid-handshake.
        assert_eq!(status.as_u16(), 200);
        assert_eq!(srv.handshake_count(), 1);
        assert_eq!(srv.client_cns().len(), 1, "a client certificate was seen");
    }

    #[test]
    fn fingerprint_matches_known_vector() {
        // SHA-256("hello") is a well-known vector.
        let der = b"hello";
        assert_eq!(
            fingerprint(der),
            "sha256:2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
        );
    }

    #[test]
    fn pin_matches_exact_and_rejects_mismatch() {
        let der = b"hello";
        let good = "sha256:2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824";
        assert!(pin_matches(der, good));
        assert!(!pin_matches(der, "sha256:deadbeef"));
        assert!(!pin_matches(b"world", good));
    }

    #[test]
    fn verifier_accepts_matching_leaf_and_rejects_mismatch() {
        // verify_server_cert hashes the raw DER, so arbitrary bytes stand in for a
        // leaf cert (the pin path does no X.509 parse). This exercises the actual
        // rustls ServerCertVerifier decision on Linux, not just the pin_matches
        // helper — the GTK e2e's plain-HTTP mock never reaches this path.
        let leaf = CertificateDer::from(b"pretend-leaf-der".to_vec());
        let provider = rustls::crypto::ring::default_provider();
        let verifier = LeafPinVerifier {
            pin: fingerprint(leaf.as_ref()),
            supported: provider.signature_verification_algorithms,
        };
        let name = ServerName::try_from("shed.local").unwrap();
        let now = UnixTime::since_unix_epoch(std::time::Duration::from_secs(0));
        assert!(verifier
            .verify_server_cert(&leaf, &[], &name, &[], now)
            .is_ok());
        let other = CertificateDer::from(b"different-der".to_vec());
        assert!(verifier
            .verify_server_cert(&other, &[], &name, &[], now)
            .is_err());
    }
}
