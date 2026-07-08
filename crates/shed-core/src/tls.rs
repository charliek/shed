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

use std::sync::Arc;

use rustls::client::danger::{HandshakeSignatureValid, ServerCertVerified, ServerCertVerifier};
use rustls::crypto::{verify_tls12_signature, verify_tls13_signature, WebPkiSupportedAlgorithms};
use rustls::pki_types::{CertificateDer, ServerName, UnixTime};
use rustls::{DigitallySignedStruct, Error as TlsError, SignatureScheme};
use sha2::{Digest, Sha256};

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

/// A rustls client config that pins the leaf cert to `pin`. Uses the ring
/// provider to match reqwest's rustls-tls stack.
pub fn pinned_client_config(pin: &str) -> Result<rustls::ClientConfig, ShedError> {
    let provider = rustls::crypto::ring::default_provider();
    let supported = provider.signature_verification_algorithms;
    let verifier = Arc::new(LeafPinVerifier {
        pin: pin.to_lowercase(),
        supported,
    });
    let config = rustls::ClientConfig::builder_with_provider(Arc::new(provider))
        .with_safe_default_protocol_versions()
        .map_err(|e| ShedError::Config(e.to_string()))?
        .dangerous()
        .with_custom_certificate_verifier(verifier)
        .with_no_client_auth();
    Ok(config)
}

#[cfg(test)]
mod tests {
    use super::*;

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
