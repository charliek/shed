//! certuser — the user a shed client certificate was issued to.
//!
//! A shed-server writes the user a client certificate was issued to into its subject, as
//! one userId attribute (RFC 4519 "uid", OID 0.9.2342.19200300.100.1.1): the user's name,
//! or `!implicit` for the one user of a server with no user list. The Go reader is
//! `internal/servertls/clientuser.go` (`ClientCertUser`); this is its Rust counterpart, so
//! a client can tell which user a certificate is for before it adopts or presents it.
//!
//! The reader is an in-house DER walk with **its own strict TLV reader** (the credential
//! store's `notAfter` helpers in `shed-broker` are lenient and stay as they are):
//! single-byte tags, definite lengths, a long-form length only for 128 or more and with no
//! leading zero byte, and no TLV that runs past its parent. The certificate must end
//! exactly at the end of its input and hold exactly three elements. Everything before the
//! subject (version, serial, signature algorithm, issuer, validity) is skipped whole, its
//! contents not interpreted, and nothing after the subject (the key, the extensions) is
//! read, so a userId in the issuer or in an extension is never read.
//!
//! **It fails closed.** Three answers are a reading: [`CertUser::Named`] (exactly one
//! userId, a PrintableString or UTF8String holding a valid user name), [`CertUser::Implicit`]
//! (exactly one, the UTF8String `!implicit`) and [`CertUser::Unbound`] (the walk completed
//! and the subject has no userId: the one case Go also reads as naming no user). Anything
//! else is a [`CertUserError`]: a second userId, any other string type (Go also reads
//! IA5String, T61String and BMPString; the CA never writes them), `!implicit` as a
//! PrintableString, any other value, any DER this reader finds malformed.
//!
//! **What is claimed.** For a certificate Go parses, this reader answers `Named`,
//! `Implicit` or `Unbound` only where Go answers the same, and otherwise refuses. For a
//! certificate Go refuses to parse, it refuses what its strict reader sees, but it does not
//! validate what it skips, so it may still read a user from one; no server accepts such a
//! certificate. The two-language fixture set `crates/fixtures/clientuser/` (written and
//! checked by a Go test in `internal/servertls`) holds both readers to this on every
//! fixture; the tests in `tests/certuser_fixtures.rs` also prove that no truncated or
//! random input panics.

use crate::csr::pem_decode;

/// The user a client certificate names.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum CertUser {
    /// Issued to this named user.
    Named(String),
    /// Issued to the implicit user of a server with no user list (`!implicit`).
    Implicit,
    /// The subject has no userId attribute: a shed-server older than 0.10.0 issued it.
    Unbound,
}

/// A certificate this reader cannot read a user from. Every caller treats it as a refusal,
/// never as a certificate that names no user.
#[derive(Debug, Clone, PartialEq, Eq, thiserror::Error)]
#[error("the client certificate cannot be read for its user: {reason}")]
pub struct CertUserError {
    reason: &'static str,
}

impl CertUserError {
    fn new(reason: &'static str) -> Self {
        Self { reason }
    }

    /// Why the certificate was refused (fixed text, never certificate bytes).
    pub fn reason(&self) -> &'static str {
        self.reason
    }
}

/// The value of an implicit user's userId attribute (Go's `ImplicitUserValue`).
pub const IMPLICIT_USER_VALUE: &str = "!implicit";

/// The content octets of the userId OID, 0.9.2342.19200300.100.1.1 (its full encoding is
/// `06 0A 09 92 26 89 93 F2 2C 64 01 01`).
const USER_ID_OID: [u8; 10] = [0x09, 0x92, 0x26, 0x89, 0x93, 0xF2, 0x2C, 0x64, 0x01, 0x01];

const TAG_INTEGER: u8 = 0x02;
const TAG_OID: u8 = 0x06;
const TAG_UTF8_STRING: u8 = 0x0C;
const TAG_PRINTABLE_STRING: u8 = 0x13;
const TAG_SEQUENCE: u8 = 0x30;
const TAG_SET: u8 = 0x31;
const TAG_VERSION: u8 = 0xA0; // [0] EXPLICIT, constructed

/// The user the DER certificate `der` names. See the module docs for the three answers and
/// what is refused.
pub fn cert_user(der: &[u8]) -> Result<CertUser, CertUserError> {
    // Certificate ::= SEQUENCE { tbsCertificate, signatureAlgorithm, signatureValue }
    let mut input = Der::new(der);
    let cert = input.read_expect(TAG_SEQUENCE, "the certificate is not a SEQUENCE")?;
    if !input.is_empty() {
        return Err(CertUserError::new("bytes follow the certificate"));
    }
    let mut cert = Der::new(cert);
    let tbs = cert.read_expect(TAG_SEQUENCE, "the TBSCertificate is not a SEQUENCE")?;
    cert.read()?; // signatureAlgorithm: not interpreted
    cert.read()?; // signatureValue: not interpreted
    if !cert.is_empty() {
        return Err(CertUserError::new(
            "the certificate holds more than three elements",
        ));
    }

    // TBSCertificate ::= SEQUENCE { [0] version OPTIONAL, serialNumber, signature, issuer,
    //                               validity, subject, ... }
    let mut tbs = Der::new(tbs);
    if tbs.peek_tag() == Some(TAG_VERSION) {
        tbs.read()?;
    }
    tbs.read_expect(TAG_INTEGER, "the serial number is not an INTEGER")?;
    tbs.read_expect(TAG_SEQUENCE, "the signature algorithm is not a SEQUENCE")?;
    tbs.read_expect(TAG_SEQUENCE, "the issuer is not a SEQUENCE")?;
    tbs.read_expect(TAG_SEQUENCE, "the validity is not a SEQUENCE")?;
    let subject = tbs.read_expect(TAG_SEQUENCE, "the subject is not a SEQUENCE")?;
    subject_user(subject)
}

/// [`cert_user`] over the first `CERTIFICATE` block of a PEM text.
pub fn cert_user_pem(pem: &str) -> Result<CertUser, CertUserError> {
    let der =
        pem_decode("CERTIFICATE", pem).map_err(|_| CertUserError::new("not a PEM certificate"))?;
    cert_user(&der)
}

/// The user of a subject Name's content: `RDNSequence ::= SEQUENCE OF RDN`, each
/// `RDN ::= SET OF AttributeTypeAndValue`, each `SEQUENCE { type OID, value }`.
fn subject_user(subject: &[u8]) -> Result<CertUser, CertUserError> {
    let mut found: Option<CertUser> = None;
    let mut rdns = Der::new(subject);
    while !rdns.is_empty() {
        let rdn = rdns.read_expect(TAG_SET, "a subject RDN is not a SET")?;
        let mut atvs = Der::new(rdn);
        if atvs.is_empty() {
            return Err(CertUserError::new("a subject RDN is empty"));
        }
        while !atvs.is_empty() {
            let atv = atvs.read_expect(TAG_SEQUENCE, "a subject attribute is not a SEQUENCE")?;
            let mut atv = Der::new(atv);
            let oid = atv.read_expect(TAG_OID, "a subject attribute's type is not an OID")?;
            let (tag, value) = atv.read()?;
            if !atv.is_empty() {
                return Err(CertUserError::new(
                    "a subject attribute holds more than one value",
                ));
            }
            if oid != USER_ID_OID {
                continue;
            }
            if found.is_some() {
                return Err(CertUserError::new(
                    "the subject names a user more than once",
                ));
            }
            found = Some(user_value(tag, value)?);
        }
    }
    Ok(found.unwrap_or(CertUser::Unbound))
}

/// The user a userId value names: `!implicit` as a UTF8String, or a valid user name as a
/// PrintableString or UTF8String. Everything else is refused.
fn user_value(tag: u8, value: &[u8]) -> Result<CertUser, CertUserError> {
    match tag {
        TAG_UTF8_STRING if value == IMPLICIT_USER_VALUE.as_bytes() => Ok(CertUser::Implicit),
        TAG_UTF8_STRING | TAG_PRINTABLE_STRING => {
            if valid_user_name(value) {
                // valid_user_name admits only ASCII, so this cannot fail.
                std::str::from_utf8(value)
                    .map(|name| CertUser::Named(name.to_owned()))
                    .map_err(|_| CertUserError::new("the user attribute is not a user name"))
            } else {
                Err(CertUserError::new("the user attribute is not a user name"))
            }
        }
        _ => Err(CertUserError::new(
            "the user attribute is not a PrintableString or UTF8String",
        )),
    }
}

/// Whether `name` is a valid user name (`^[a-z][a-z0-9-]{0,62}$`, exactly): the same test
/// [`cert_user`] applies to a userId value. For a caller that holds a user name it did not
/// read from a certificate (a config file's `user`) and must decide whether to quote it.
pub fn is_user_name(name: &str) -> bool {
    valid_user_name(name.as_bytes())
}

/// `^[a-z][a-z0-9-]{0,62}$` over bytes, exactly (Go's `userNamePattern`; no trailing
/// newline is admitted).
fn valid_user_name(name: &[u8]) -> bool {
    match name.split_first() {
        Some((first, rest)) => {
            name.len() <= 63
                && first.is_ascii_lowercase()
                && rest
                    .iter()
                    .all(|b| b.is_ascii_lowercase() || b.is_ascii_digit() || *b == b'-')
        }
        None => false,
    }
}

/// The strict TLV reader: a cursor over the content of one constructed value.
struct Der<'a> {
    buf: &'a [u8],
}

impl<'a> Der<'a> {
    fn new(buf: &'a [u8]) -> Self {
        Self { buf }
    }

    fn is_empty(&self) -> bool {
        self.buf.is_empty()
    }

    fn peek_tag(&self) -> Option<u8> {
        self.buf.first().copied()
    }

    /// Read the next TLV, returning its tag and content.
    fn read(&mut self) -> Result<(u8, &'a [u8]), CertUserError> {
        let (&tag, rest) = self
            .buf
            .split_first()
            .ok_or_else(|| CertUserError::new("a value is missing"))?;
        if tag & 0x1F == 0x1F {
            return Err(CertUserError::new("a tag is not a single byte"));
        }
        let (&first, rest) = rest
            .split_first()
            .ok_or_else(|| CertUserError::new("a length is missing"))?;
        let (len, rest) = if first < 0x80 {
            (usize::from(first), rest)
        } else {
            let n = usize::from(first & 0x7F);
            if n == 0 {
                return Err(CertUserError::new("a length is indefinite"));
            }
            // Four octets reach 4 GiB; no certificate is near that.
            if n > 4 {
                return Err(CertUserError::new("a length is too large"));
            }
            let octets = rest
                .get(..n)
                .ok_or_else(|| CertUserError::new("a length runs past its parent"))?;
            if octets.first() == Some(&0) {
                return Err(CertUserError::new("a length is not minimal"));
            }
            let len = octets
                .iter()
                .fold(0usize, |acc, b| (acc << 8) | usize::from(*b));
            if len < 0x80 {
                return Err(CertUserError::new("a length is not minimal"));
            }
            (len, rest.get(n..).unwrap_or_default())
        };
        let content = rest
            .get(..len)
            .ok_or_else(|| CertUserError::new("a value runs past its parent"))?;
        self.buf = rest.get(len..).unwrap_or_default();
        Ok((tag, content))
    }

    /// Read the next TLV, requiring `tag`; `what` names the refusal.
    fn read_expect(&mut self, tag: u8, what: &'static str) -> Result<&'a [u8], CertUserError> {
        let (got, content) = self.read()?;
        if got != tag {
            return Err(CertUserError::new(what));
        }
        Ok(content)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// A TLV with a minimal length.
    fn tlv(tag: u8, content: &[u8]) -> Vec<u8> {
        let mut out = vec![tag];
        let n = content.len();
        if n < 0x80 {
            out.push(n as u8);
        } else {
            let bytes: Vec<u8> = n
                .to_be_bytes()
                .into_iter()
                .skip_while(|b| *b == 0)
                .collect();
            out.push(0x80 | bytes.len() as u8);
            out.extend(bytes);
        }
        out.extend_from_slice(content);
        out
    }

    fn cat(parts: &[&[u8]]) -> Vec<u8> {
        parts.concat()
    }

    /// `is_user_name` is the reader's own name test, exposed for config values.
    #[test]
    fn is_user_name_is_the_readers_rule() {
        for ok in ["pat", "a", "build-1", &format!("a{}", "b".repeat(62))] {
            assert!(is_user_name(ok), "{ok:?}");
        }
        for bad in [
            "",
            "Pat",
            "1pat",
            "-pat",
            "pat\n",
            "pa t",
            "!implicit",
            "\"pat\"",
            &format!("a{}", "b".repeat(63)),
        ] {
            assert!(!is_user_name(bad), "{bad:?}");
        }
    }

    fn uid(tag: u8, value: &[u8]) -> Vec<u8> {
        let oid = tlv(TAG_OID, &USER_ID_OID);
        tlv(TAG_SEQUENCE, &cat(&[&oid, &tlv(tag, value)]))
    }

    fn cn(value: &str) -> Vec<u8> {
        let oid = tlv(TAG_OID, &[0x55, 0x04, 0x03]);
        tlv(
            TAG_SEQUENCE,
            &cat(&[&oid, &tlv(TAG_UTF8_STRING, value.as_bytes())]),
        )
    }

    /// A certificate-shaped DER around `subject_content` (the Name's content), with
    /// `issuer_content` as the issuer's.
    fn cert_with(issuer_content: &[u8], subject_content: &[u8]) -> Vec<u8> {
        cert_with_tail(issuer_content, subject_content, &[])
    }

    /// [`cert_with`], with `tail` (the TBS elements after the subject) appended.
    fn cert_with_tail(issuer_content: &[u8], subject_content: &[u8], tail: &[u8]) -> Vec<u8> {
        let version = tlv(TAG_VERSION, &tlv(TAG_INTEGER, &[2]));
        let serial = tlv(TAG_INTEGER, &[7]);
        let alg = tlv(TAG_SEQUENCE, &tlv(TAG_OID, &[0x2B, 0x65, 0x70]));
        let issuer = tlv(TAG_SEQUENCE, issuer_content);
        let validity = tlv(TAG_SEQUENCE, &[]);
        let subject = tlv(TAG_SEQUENCE, subject_content);
        let tbs = tlv(
            TAG_SEQUENCE,
            &cat(&[&version, &serial, &alg, &issuer, &validity, &subject, tail]),
        );
        let sig = tlv(0x03, &[0x00, 0xAA]);
        tlv(TAG_SEQUENCE, &cat(&[&tbs, &alg, &sig]))
    }

    fn subject(rdns: &[Vec<u8>]) -> Vec<u8> {
        rdns.iter().flat_map(|r| tlv(TAG_SET, r)).collect()
    }

    #[test]
    fn reads_the_three_answers() {
        let named = cert_with(&[], &subject(&[cn("x"), uid(TAG_PRINTABLE_STRING, b"pat")]));
        assert_eq!(cert_user(&named), Ok(CertUser::Named("pat".into())));
        let utf8 = cert_with(&[], &subject(&[uid(TAG_UTF8_STRING, b"pat")]));
        assert_eq!(cert_user(&utf8), Ok(CertUser::Named("pat".into())));
        let implicit = cert_with(&[], &subject(&[uid(TAG_UTF8_STRING, b"!implicit")]));
        assert_eq!(cert_user(&implicit), Ok(CertUser::Implicit));
        let unbound = cert_with(&[], &subject(&[cn("x")]));
        assert_eq!(cert_user(&unbound), Ok(CertUser::Unbound));
        let empty = cert_with(&[], &[]);
        assert_eq!(cert_user(&empty), Ok(CertUser::Unbound));
    }

    #[test]
    fn a_user_in_the_issuer_is_never_read() {
        let issuer = subject(&[uid(TAG_PRINTABLE_STRING, b"admin")]);
        let der = cert_with(&issuer, &subject(&[cn("x")]));
        assert_eq!(cert_user(&der), Ok(CertUser::Unbound));
        let der = cert_with(&issuer, &subject(&[uid(TAG_PRINTABLE_STRING, b"pat")]));
        assert_eq!(cert_user(&der), Ok(CertUser::Named("pat".into())));
    }

    #[test]
    fn a_user_in_an_extension_is_never_read() {
        // [3] { SEQUENCE { subjectAltName, OCTET STRING { SEQUENCE { [4] Name } } } }, the
        // directoryName naming admin.
        let dir_name = tlv(
            TAG_SEQUENCE,
            &subject(&[uid(TAG_PRINTABLE_STRING, b"admin")]),
        );
        let san = tlv(
            TAG_SEQUENCE,
            &cat(&[
                &tlv(TAG_OID, &[0x55, 0x1D, 0x11]),
                &tlv(0x04, &tlv(TAG_SEQUENCE, &tlv(0xA4, &dir_name))),
            ]),
        );
        let extensions = tlv(0xA3, &tlv(TAG_SEQUENCE, &san));
        let der = cert_with_tail(&[], &subject(&[cn("x")]), &extensions);
        assert_eq!(cert_user(&der), Ok(CertUser::Unbound));
        let der = cert_with_tail(
            &[],
            &subject(&[uid(TAG_PRINTABLE_STRING, b"pat")]),
            &extensions,
        );
        assert_eq!(cert_user(&der), Ok(CertUser::Named("pat".into())));
    }

    #[test]
    fn the_name_grammar_is_go_s() {
        let max = format!("a{}", "z9-".repeat(20) + "zz");
        assert_eq!(max.len(), 63);
        for (value, ok) in [
            ("a", true),
            ("a-", true),
            ("a--b", true),
            ("implicit", true),
            (max.as_str(), true),
            (&*format!("{max}a"), false),
            ("", false),
            ("-a", false),
            ("9a", false),
            ("Pat", false),
            ("pat\n", false),
            ("pat ", false),
            ("p\u{0430}t", false),
            ("!unresolved", false),
        ] {
            for tag in [TAG_UTF8_STRING, TAG_PRINTABLE_STRING] {
                let der = cert_with(&[], &subject(&[uid(tag, value.as_bytes())]));
                let read = cert_user(&der);
                if ok {
                    // A valid name reads as exactly that name, never another answer.
                    assert_eq!(read, Ok(CertUser::Named(value.to_owned())), "{value:?}");
                } else {
                    assert!(read.is_err(), "{value:?}: read {read:?}, want an error");
                }
            }
        }
    }

    #[test]
    fn refuses_what_it_cannot_read() {
        let cases: Vec<(&str, Vec<u8>)> = vec![
            (
                "!implicit as PrintableString",
                cert_with(&[], &subject(&[uid(TAG_PRINTABLE_STRING, b"!implicit")])),
            ),
            ("IA5String", cert_with(&[], &subject(&[uid(0x16, b"pat")]))),
            (
                "an empty RDN",
                cert_with(&[], &cat(&[&tlv(TAG_SET, &[]), &tlv(TAG_SET, &cn("x"))])),
            ),
            (
                "two values in one attribute",
                cert_with(
                    &[],
                    &subject(&[tlv(
                        TAG_SEQUENCE,
                        &cat(&[
                            &tlv(TAG_OID, &USER_ID_OID),
                            &tlv(TAG_UTF8_STRING, b"pat"),
                            &tlv(TAG_UTF8_STRING, b"pat"),
                        ]),
                    )]),
                ),
            ),
            (
                "an attribute with no value",
                cert_with(
                    &[],
                    &subject(&[tlv(TAG_SEQUENCE, &tlv(TAG_OID, &[0x55, 4, 3]))]),
                ),
            ),
            (
                "two users in two RDNs",
                cert_with(
                    &[],
                    &subject(&[uid(TAG_UTF8_STRING, b"pat"), uid(TAG_UTF8_STRING, b"pat")]),
                ),
            ),
            (
                "two users in one RDN",
                cert_with(
                    &[],
                    &subject(&[cat(&[
                        &uid(TAG_UTF8_STRING, b"pat"),
                        &uid(TAG_UTF8_STRING, b"admin"),
                    ])]),
                ),
            ),
        ];
        for (what, der) in cases {
            assert!(cert_user(&der).is_err(), "{what}: {:?}", cert_user(&der));
        }
    }

    #[test]
    fn the_strict_reader() {
        // Minimal long form for 128 bytes is accepted.
        let content = vec![0u8; 128];
        let mut der = Der::new(&[0x04, 0x81, 0x80]);
        assert!(der.read().is_err(), "content missing");
        let ok = cat(&[&[0x04, 0x81, 0x80], &content]);
        let mut der = Der::new(&ok);
        assert_eq!(der.read().map(|(t, c)| (t, c.len())), Ok((0x04, 128)));
        assert!(der.is_empty());

        for (what, bytes) in [
            ("long form below 128", vec![0x04, 0x81, 0x05, 1, 2, 3, 4, 5]),
            (
                "leading zero length octet",
                cat(&[&[0x04, 0x82, 0x00, 0x80], &content]),
            ),
            ("indefinite length", vec![0x30, 0x80, 0x00, 0x00]),
            ("reserved length 0xFF", vec![0x04, 0xFF]),
            ("five length octets", vec![0x04, 0x85, 1, 0, 0, 0, 0]),
            // Nine octets would shift the leading 1 out of a usize and leave 128: a
            // length that must be refused, never wrapped.
            (
                "nine length octets",
                cat(&[&[0x04, 0x89, 1, 0, 0, 0, 0, 0, 0, 0, 0x80], &content]),
            ),
            ("multi-byte tag", vec![0x1F, 0x22, 0x01, 0x00]),
            ("runs past its parent", vec![0x04, 0x05, 1, 2]),
            ("no length", vec![0x04]),
            ("nothing", vec![]),
        ] {
            assert!(Der::new(&bytes).read().is_err(), "{what}");
        }
    }

    #[test]
    fn the_certificate_frame_is_exact() {
        let good = cert_with(&[], &subject(&[uid(TAG_UTF8_STRING, b"pat")]));
        assert!(cert_user(&good).is_ok());
        let trailing = cat(&[&good, &[0x00]]);
        assert!(cert_user(&trailing).is_err(), "trailing bytes");
        // A fourth element in the Certificate SEQUENCE.
        let inner = Der::new(&good).read().unwrap().1.to_vec();
        let four = tlv(TAG_SEQUENCE, &cat(&[&inner, &tlv(0x05, &[])]));
        assert!(cert_user(&four).is_err(), "four elements");
    }

    #[test]
    fn pem_wrapping() {
        assert!(cert_user_pem("not a certificate").is_err());
        assert!(
            cert_user_pem("-----BEGIN CERTIFICATE-----\n!!\n-----END CERTIFICATE-----\n").is_err()
        );
    }

    #[test]
    fn the_error_names_the_certificate_and_no_bytes() {
        let err = cert_user(&[0x30, 0x80]).unwrap_err();
        assert_eq!(
            err.to_string(),
            "the client certificate cannot be read for its user: a length is indefinite"
        );
        assert_eq!(err.reason(), "a length is indefinite");
    }
}
