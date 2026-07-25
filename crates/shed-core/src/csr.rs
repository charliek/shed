//! Client half of mtls enrollment: generate a fresh P-256 keypair, wrap its
//! public half in a PKCS#10 CertificationRequest, and hand the base64 DER to the
//! `_bootstrap` request line (`<scope> [<kind>] csr=<base64 std DER>`).
//!
//! The private key NEVER leaves the process that generated it — only the CSR
//! crosses the SSH channel, and the server treats it as a carrier for the public
//! key alone: every subject field, SAN, extension and attribute it could request
//! is discarded, and the issued identity is composed server-side from the
//! authenticated SSH key (`internal/servertls.CA.SignClientCSR`). That is why the
//! CSR built here carries an EMPTY subject: populating one would imply an
//! influence the client does not have.
//!
//! Language parity, not shared code: the Go client does the same thing with
//! `crypto/ecdsa` + `crypto/x509` (`sdk/bootstrap/csr.go`). One implementation per
//! language world is the accepted duplication (plan 001 D8) — the wire artifact
//! (a P-256, ECDSA-SHA256-signed PKCS#10 DER, standard-base64 with padding) is
//! what has to match, and it is asserted against the server's rules by the
//! integration suite.
//!
//! Dependency note (plan 001 D8, `crates/CLAUDE.md` dependency-clean posture):
//! CSR generation uses `rcgen` with `default-features = false` + the `ring`
//! backend — the same ring stack rustls already pulls in. rcgen's `pem` feature
//! is deliberately NOT enabled (this module encodes PEM itself, ~15 lines), and
//! `aws_lc_rs` never is. The only crates this adds to the workspace lock are
//! `rcgen` and `yasna`, both pure Rust with no serde.

use base64::engine::general_purpose::STANDARD as BASE64;
use base64::Engine as _;

use crate::http::ShedError;

/// PEM label for a PKCS#8 private key — what [`ClientKeyPair::key_pem`] emits and
/// what the Go client's credential store reads back.
pub const PEM_LABEL_PRIVATE_KEY: &str = "PRIVATE KEY";
/// PEM label for a SEC1 EC private key — what the GO client's credential store writes
/// (`x509.MarshalECPrivateKey`). Never emitted here; accepted on READ so a credential
/// loader compares a hand-restored or externally-provisioned key rather than rejecting it
/// (see [`crate::tls::certified_key_from_pem_pair`]).
pub const PEM_LABEL_EC_PRIVATE_KEY: &str = "EC PRIVATE KEY";
/// PEM label for an X.509 certificate — the `client_cert` field of an mtls
/// bootstrap bundle.
pub const PEM_LABEL_CERTIFICATE: &str = "CERTIFICATE";

/// A freshly generated enrollment keypair plus the two encodings its consumers
/// need: standard-base64 DER of the CSR for the bootstrap request line, and
/// PKCS#8 for the private key (DER for the rustls signing key, PEM for a caller
/// that persists it).
///
/// Held by the credential provider across the mint round-trip: the certificate
/// that comes back is paired with THIS key, and a mint that returns a
/// certificate for a CSR this process did not send is refused
/// ([`crate::tls::certified_key_from_pem`] verifies the pairing).
pub struct ClientKeyPair {
    key_pkcs8_der: Vec<u8>,
    csr_der: Vec<u8>,
}

impl ClientKeyPair {
    /// Generate a P-256 keypair and the matching CSR (ECDSA-SHA256, empty
    /// subject). The server accepts exactly P-256 signed with ECDSA-SHA256/384,
    /// so neither is configurable.
    pub fn generate() -> Result<Self, ShedError> {
        let key = rcgen::KeyPair::generate().map_err(|e| csr_err("generate client key", &e))?;
        let mut params = rcgen::CertificateParams::default();
        // rcgen's Default injects `CN=rcgen self signed cert`. Clear it: the module's
        // whole premise is that this CSR requests NO identity (the server composes the
        // subject from the authenticated SSH key and discards everything the CSR asked
        // for), and a placeholder CN implies an influence the client does not have. It
        // also keeps the wire artifact shaped like Go's `sdk/bootstrap/csr.go`, which
        // submits an empty `pkix.Name`.
        params.distinguished_name = rcgen::DistinguishedName::new();
        let csr = params
            .serialize_request(&key)
            .map_err(|e| csr_err("create CSR", &e))?;
        Ok(Self {
            key_pkcs8_der: key.serialize_der(),
            csr_der: csr.der().to_vec(),
        })
    }

    /// The CSR as STANDARD base64 with padding — the server decodes with Go's
    /// `base64.StdEncoding.Strict()` and rejects the URL-safe alphabet.
    pub fn csr_base64(&self) -> String {
        BASE64.encode(&self.csr_der)
    }

    /// The raw CSR DER (tests and callers that re-encode it themselves).
    pub fn csr_der(&self) -> &[u8] {
        &self.csr_der
    }

    /// The private key as PKCS#8 DER — what rustls's signing-key loader wants.
    pub fn key_pkcs8_der(&self) -> &[u8] {
        &self.key_pkcs8_der
    }

    /// The private key as **PKCS#8** PEM (`-----BEGIN PRIVATE KEY-----`), for a
    /// caller that persists it (the broker's state dir). Desktop and mobile hold
    /// the key in memory only and never call this.
    ///
    /// Deliberate divergence worth knowing: the Go client writes SEC1
    /// (`-----BEGIN EC PRIVATE KEY-----`, `x509.MarshalECPrivateKey`) into
    /// `~/.shed/creds/<server>/`. Nothing reads the other language's file today —
    /// each process generates its own key and never shares it (plan 001 D6's
    /// ownership table) — but anything that ever DOES cross the boundary must
    /// accept both labels rather than assume this one.
    pub fn key_pem(&self) -> String {
        pem_encode(PEM_LABEL_PRIVATE_KEY, &self.key_pkcs8_der)
    }
}

/// Redacted Debug: a keypair must never render its private half into a log line,
/// an error payload, or a `{:?}` of an enclosing struct.
impl std::fmt::Debug for ClientKeyPair {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("ClientKeyPair")
            .field("key", &"<redacted>")
            .field("csr_der_len", &self.csr_der.len())
            .finish()
    }
}

fn csr_err(what: &str, e: &rcgen::Error) -> ShedError {
    ShedError::Config(format!("csr: {what}: {e}"))
}

/// Wrap DER in a PEM block with 64-character base64 lines (RFC 7468).
pub fn pem_encode(label: &str, der: &[u8]) -> String {
    let body = BASE64.encode(der);
    let mut out = String::with_capacity(body.len() + body.len() / 64 + 2 * label.len() + 32);
    out.push_str("-----BEGIN ");
    out.push_str(label);
    out.push_str("-----\n");
    for chunk in body.as_bytes().chunks(64) {
        out.push_str(std::str::from_utf8(chunk).unwrap_or_default());
        out.push('\n');
    }
    out.push_str("-----END ");
    out.push_str(label);
    out.push_str("-----\n");
    out
}

/// Decode the FIRST PEM block with `label` out of `text`, returning its DER.
///
/// Deliberately narrow — this reads machine-generated PEM (a bootstrap bundle's
/// `client_cert`, a credential file this process wrote), not arbitrary user
/// input: no support for encrypted blocks, headers, or multi-block chains beyond
/// taking the first match. Anything else is an error rather than a guess.
pub fn pem_decode(label: &str, text: &str) -> Result<Vec<u8>, ShedError> {
    let begin = format!("-----BEGIN {label}-----");
    let end = format!("-----END {label}-----");
    let start = text
        .find(&begin)
        .ok_or_else(|| ShedError::Config(format!("pem: no {label} block")))?
        + begin.len();
    let rest = &text[start..];
    let stop = rest
        .find(&end)
        .ok_or_else(|| ShedError::Config(format!("pem: unterminated {label} block")))?;
    let body: String = rest[..stop]
        .chars()
        .filter(|c| !c.is_whitespace())
        .collect();
    BASE64
        .decode(body.as_bytes())
        .map_err(|e| ShedError::Config(format!("pem: {label} body is not base64: {e}")))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn generates_a_p256_csr_the_server_would_accept() {
        let kp = ClientKeyPair::generate().unwrap();
        // Standard alphabet with padding, decodable back to the exact DER.
        let b64 = kp.csr_base64();
        assert!(
            !b64.contains('-') && !b64.contains('_'),
            "must not be url-safe base64: {b64}"
        );
        assert_eq!(BASE64.decode(b64.as_bytes()).unwrap(), kp.csr_der());
        // An EMPTY-subject P-256 CSR is ~185-200 DER bytes — it straddles 200,
        // because the ECDSA signature's DER length varies by a couple of bytes
        // with r/s leading zeros (a 200 floor failed roughly half the time). The
        // bound that matters is the server's 8 KiB decode cap.
        assert!(
            (150..2048).contains(&kp.csr_der().len()),
            "unexpected CSR size {}",
            kp.csr_der().len()
        );
        // PKCS#10 is a SEQUENCE.
        assert_eq!(kp.csr_der()[0], 0x30);
        // The P-256 OID (1.2.840.10045.3.1.7) appears in the SubjectPublicKeyInfo,
        // and the ECDSA-with-SHA256 OID (1.2.840.10045.4.3.2) in the signature —
        // the two properties the server's CSR validation enforces.
        const P256_OID: &[u8] = &[0x2a, 0x86, 0x48, 0xce, 0x3d, 0x03, 0x01, 0x07];
        const ECDSA_SHA256_OID: &[u8] = &[0x2a, 0x86, 0x48, 0xce, 0x3d, 0x04, 0x03, 0x02];
        assert!(kp.csr_der().windows(8).any(|w| w == P256_OID));
        assert!(kp.csr_der().windows(8).any(|w| w == ECDSA_SHA256_OID));
    }

    /// The subject is EMPTY — no CN, no placeholder. rcgen's `Default` supplies
    /// `CN=rcgen self signed cert` unless it is cleared, and shipping that would put a
    /// requested identity on the wire that the server discards anyway (and that a reader
    /// of the argv would reasonably mistake for one this client claims).
    #[test]
    fn the_csr_requests_no_subject() {
        let kp = ClientKeyPair::generate().unwrap();
        // The CN attribute type OID (2.5.4.3) is `55 04 03`; an empty subject has none.
        const CN_OID: &[u8] = &[0x55, 0x04, 0x03];
        assert!(
            !kp.csr_der().windows(3).any(|w| w == CN_OID),
            "the CSR must not carry a Common Name"
        );
        assert!(
            !String::from_utf8_lossy(kp.csr_der()).contains("rcgen"),
            "rcgen's default subject leaked into the CSR"
        );
    }

    #[test]
    fn each_generation_is_a_fresh_key() {
        let a = ClientKeyPair::generate().unwrap();
        let b = ClientKeyPair::generate().unwrap();
        assert_ne!(a.key_pkcs8_der(), b.key_pkcs8_der());
        assert_ne!(a.csr_der(), b.csr_der());
    }

    #[test]
    fn debug_never_renders_the_private_key() {
        let kp = ClientKeyPair::generate().unwrap();
        let rendered = format!("{kp:?}");
        assert!(rendered.contains("<redacted>"), "{rendered}");
        assert!(!rendered.contains(&BASE64.encode(kp.key_pkcs8_der())));
    }

    #[test]
    fn key_pem_round_trips_through_pem_decode() {
        let kp = ClientKeyPair::generate().unwrap();
        let pem = kp.key_pem();
        assert!(pem.starts_with("-----BEGIN PRIVATE KEY-----\n"));
        assert!(pem.ends_with("-----END PRIVATE KEY-----\n"));
        // Every base64 line is <= 64 chars (RFC 7468 line folding).
        for line in pem.lines().filter(|l| !l.starts_with("-----")) {
            assert!(line.len() <= 64, "long PEM line: {}", line.len());
        }
        assert_eq!(
            pem_decode(PEM_LABEL_PRIVATE_KEY, &pem).unwrap(),
            kp.key_pkcs8_der()
        );
    }

    #[test]
    fn pem_decode_rejects_missing_and_malformed_blocks() {
        assert!(pem_decode(PEM_LABEL_CERTIFICATE, "").is_err());
        assert!(pem_decode(PEM_LABEL_CERTIFICATE, "-----BEGIN CERTIFICATE-----\nAAAA").is_err());
        assert!(pem_decode(
            PEM_LABEL_CERTIFICATE,
            "-----BEGIN CERTIFICATE-----\n!!!!\n-----END CERTIFICATE-----\n"
        )
        .is_err());
        // A different label in the same document is not a match.
        assert!(pem_decode(PEM_LABEL_CERTIFICATE, &pem_encode("PRIVATE KEY", b"x")).is_err());
    }

    #[test]
    fn pem_encode_decode_handles_crlf_and_leading_text() {
        let der = b"\x30\x82\x01\x02some-der-bytes";
        let pem = pem_encode(PEM_LABEL_CERTIFICATE, der).replace('\n', "\r\n");
        let with_preamble = format!("issued for you:\r\n{pem}");
        assert_eq!(
            pem_decode(PEM_LABEL_CERTIFICATE, &with_preamble).unwrap(),
            der
        );
    }
}
