//! `ssh-key`: one SSH key pair, delivered as `~/.ssh/id_<type>` (`0600`) and
//! `~/.ssh/id_<type>.pub` (`0644`). A key with a passphrase is not
//! delivered (`passphrase_protected`), in the OpenSSH, PEM or PKCS#8 form;
//! a missing public half is `missing_input`.
//!
//! The public half is world-readable in the shed, so it is held to one
//! form and nothing else is sent: exactly one line (blank lines aside) of a
//! known type, a base64 key blob of that type and an optional comment of
//! printable characters, re-written as `<type> <blob> <comment>`. It must
//! be the private key's: the public blob is compared with the one the
//! private key holds (an OpenSSH container's own, or one derived from a PEM
//! or PKCS#8 key's fields), and a key whose public half cannot be derived
//! without key arithmetic (PKCS#8 DSA, an EC or Ed25519 key that omits its
//! public point) is refused rather than sent unchecked. With
//! `sign_commits`, it contributes the git config that signs commits with
//! the key.

use base64::Engine as _;
use shed_provision::{
    limits::HOME_DIR, printable, Base, Context, Contribution, Draft, FileItem, GitSection,
    ItemBody, Mode, Only, ProblemCode, Provider, Reader, Stage,
};

use crate::common::read_setting_file;
use crate::settings::SshKeySettings;

/// The provider's name.
pub const NAME: &str = "ssh-key";

/// The most a key file may hold: far more than any real key.
const MAX_KEY_BYTES: usize = 64 << 10;

/// The `ssh-key` provider.
#[derive(Debug, Clone)]
pub struct SshKey {
    settings: SshKeySettings,
}

impl SshKey {
    /// The provider for these settings.
    pub fn new(settings: SshKeySettings) -> SshKey {
        SshKey { settings }
    }
}

/// What the private key file is.
#[derive(Debug, PartialEq, Eq)]
enum KeyForm {
    /// A key with no passphrase, and the public blob it holds or gives.
    Clear(Vec<u8>),
    /// A key with no passphrase whose public blob cannot be derived here.
    Unchecked,
    /// A key with a passphrase.
    Encrypted,
    /// Not a private key this provider knows.
    Unknown,
}

fn b64() -> base64::engine::GeneralPurpose {
    base64::engine::general_purpose::STANDARD
}

/// The first PEM block labelled `label`: whether its headers mark it
/// encrypted, and its DER body.
fn pem_block(text: &str, label: &str) -> Option<(bool, Option<Vec<u8>>)> {
    let begin = format!("-----BEGIN {label}-----");
    let end = format!("-----END {label}-----");
    let start = text.find(&begin)? + begin.len();
    let body = &text[start..];
    let body = &body[..body.find(&end)?];
    let mut encrypted = false;
    let mut b = String::new();
    for line in body.lines() {
        let line = line.trim();
        // A header line (`Proc-Type: 4,ENCRYPTED`, `DEK-Info: ...`) holds a
        // colon, which base64 never does.
        if line.contains(':') {
            if line.starts_with("Proc-Type:") && line.contains("ENCRYPTED") {
                encrypted = true;
            }
            continue;
        }
        b.push_str(line);
    }
    Some((encrypted, b64().decode(b).ok()))
}

fn classify(text: &str) -> KeyForm {
    if let Some((_, der)) = pem_block(text, "OPENSSH PRIVATE KEY") {
        return der
            .and_then(|blob| openssh(&blob))
            .unwrap_or(KeyForm::Unknown);
    }
    if pem_block(text, "ENCRYPTED PRIVATE KEY").is_some() {
        return KeyForm::Encrypted;
    }
    for (label, derive) in [
        (
            "RSA PRIVATE KEY",
            der::rsa_pkcs1 as fn(&[u8]) -> Option<Derived>,
        ),
        ("EC PRIVATE KEY", der::ec_sec1),
        ("DSA PRIVATE KEY", der::dsa),
        ("PRIVATE KEY", der::pkcs8),
    ] {
        if let Some((encrypted, der)) = pem_block(text, label) {
            if encrypted {
                return KeyForm::Encrypted;
            }
            return match der.as_deref().and_then(derive) {
                Some(Derived::Blob(b)) => KeyForm::Clear(b),
                Some(Derived::Unchecked) => KeyForm::Unchecked,
                None => KeyForm::Unknown,
            };
        }
    }
    KeyForm::Unknown
}

/// Reads an `openssh-key-v1` container's header: its cipher (anything but
/// `none` means a passphrase) and its first public key.
fn openssh(blob: &[u8]) -> Option<KeyForm> {
    let mut r = blob.strip_prefix(b"openssh-key-v1\0")?;
    let cipher = take_string(&mut r)?;
    let _kdf = take_string(&mut r)?;
    let _kdf_options = take_string(&mut r)?;
    if cipher != b"none" {
        return Some(KeyForm::Encrypted);
    }
    let count = u32::from_be_bytes(r.get(..4)?.try_into().ok()?);
    r = &r[4..];
    if count != 1 {
        return None;
    }
    Some(KeyForm::Clear(take_string(&mut r)?.to_vec()))
}

/// An SSH wire string from the front of `r`.
fn take_string<'a>(r: &mut &'a [u8]) -> Option<&'a [u8]> {
    let n = u32::from_be_bytes(r.get(..4)?.try_into().ok()?) as usize;
    let s = r.get(4..4usize.checked_add(n)?)?;
    *r = &r[4 + n..];
    Some(s)
}

/// What a PEM or PKCS#8 key gives: its public blob, or nothing this
/// provider can check.
#[derive(Debug, PartialEq, Eq)]
enum Derived {
    Blob(Vec<u8>),
    Unchecked,
}

/// Just enough DER to find a private key's public fields.
mod der {
    use super::Derived;

    const SEQUENCE: u8 = 0x30;
    const INTEGER: u8 = 0x02;
    const OCTET_STRING: u8 = 0x04;
    const BIT_STRING: u8 = 0x03;
    const OID: u8 = 0x06;

    const RSA: &[u8] = &[0x2a, 0x86, 0x48, 0x86, 0xf7, 0x0d, 0x01, 0x01, 0x01];
    const EC: &[u8] = &[0x2a, 0x86, 0x48, 0xce, 0x3d, 0x02, 0x01];
    const DSA: &[u8] = &[0x2a, 0x86, 0x48, 0xce, 0x38, 0x04, 0x01];
    const ED25519: &[u8] = &[0x2b, 0x65, 0x70];
    const P256: &[u8] = &[0x2a, 0x86, 0x48, 0xce, 0x3d, 0x03, 0x01, 0x07];
    const P384: &[u8] = &[0x2b, 0x81, 0x04, 0x00, 0x22];
    const P521: &[u8] = &[0x2b, 0x81, 0x04, 0x00, 0x23];

    /// A DER reader over one level of content.
    struct Der<'a>(&'a [u8]);

    impl<'a> Der<'a> {
        /// The next element's tag and content.
        fn next(&mut self) -> Option<(u8, &'a [u8])> {
            let (&tag, rest) = self.0.split_first()?;
            let (&first, rest) = rest.split_first()?;
            let (len, rest) = if first < 0x80 {
                (first as usize, rest)
            } else {
                let n = (first & 0x7f) as usize;
                if n == 0 || n > 4 || rest.len() < n {
                    return None;
                }
                let len = rest[..n].iter().fold(0usize, |a, &b| (a << 8) | b as usize);
                (len, &rest[n..])
            };
            let content = rest.get(..len)?;
            self.0 = &rest[len..];
            Some((tag, content))
        }

        fn expect(&mut self, tag: u8) -> Option<&'a [u8]> {
            match self.next()? {
                (t, c) if t == tag => Some(c),
                _ => None,
            }
        }

        /// The rest of the elements, by tag.
        fn rest(&mut self) -> Vec<(u8, &'a [u8])> {
            let mut out = Vec::new();
            while let Some(e) = self.next() {
                out.push(e);
            }
            out
        }
    }

    fn string(out: &mut Vec<u8>, s: &[u8]) {
        out.extend((s.len() as u32).to_be_bytes());
        out.extend_from_slice(s);
    }

    /// A DER INTEGER's content as an SSH mpint: both are minimal
    /// two's-complement big-endian, so a positive integer is the same bytes.
    fn mpint(out: &mut Vec<u8>, int: &[u8]) -> Option<()> {
        if int.first().is_some_and(|b| b & 0x80 != 0) {
            return None; // negative: not a key field
        }
        string(out, if int == [0] { &[] } else { int });
        Some(())
    }

    fn sequence(der: &[u8]) -> Option<Der<'_>> {
        let mut outer = Der(der);
        let seq = outer.expect(SEQUENCE)?;
        outer.0.is_empty().then_some(Der(seq))
    }

    /// `RSAPrivateKey ::= SEQUENCE { version, n, e, d, ... }`
    pub(super) fn rsa_pkcs1(der: &[u8]) -> Option<Derived> {
        let mut s = sequence(der)?;
        s.expect(INTEGER)?;
        let n = s.expect(INTEGER)?;
        let e = s.expect(INTEGER)?;
        let mut blob = Vec::new();
        string(&mut blob, b"ssh-rsa");
        mpint(&mut blob, e)?;
        mpint(&mut blob, n)?;
        Some(Derived::Blob(blob))
    }

    /// `DSAPrivateKey ::= SEQUENCE { version, p, q, g, y, x }`
    pub(super) fn dsa(der: &[u8]) -> Option<Derived> {
        let mut s = sequence(der)?;
        s.expect(INTEGER)?;
        let mut blob = Vec::new();
        string(&mut blob, b"ssh-dss");
        for _ in 0..4 {
            mpint(&mut blob, s.expect(INTEGER)?)?;
        }
        Some(Derived::Blob(blob))
    }

    fn ec_blob(curve: &[u8], point: &[u8]) -> Option<Derived> {
        let name: &[u8] = match curve {
            P256 => b"nistp256",
            P384 => b"nistp384",
            P521 => b"nistp521",
            _ => return None,
        };
        let mut blob = Vec::new();
        string(&mut blob, &[b"ecdsa-sha2-".as_slice(), name].concat());
        string(&mut blob, name);
        string(&mut blob, point);
        Some(Derived::Blob(blob))
    }

    /// A BIT STRING's bytes, with no unused bits.
    fn bits(b: &[u8]) -> Option<&[u8]> {
        b.strip_prefix(&[0])
    }

    /// `ECPrivateKey ::= SEQUENCE { 1, privateKey, [0] parameters OPTIONAL,
    /// [1] publicKey OPTIONAL }`, with the curve from `[0]` or, in PKCS#8,
    /// from the algorithm's parameters.
    fn ec_private(der: &[u8], outer_curve: Option<&[u8]>) -> Option<Derived> {
        let mut s = sequence(der)?;
        s.expect(INTEGER)?;
        s.expect(OCTET_STRING)?;
        let mut curve = outer_curve;
        let mut point = None;
        for (tag, content) in s.rest() {
            match tag {
                0xa0 => curve = Some(Der(content).expect(OID)?),
                0xa1 => point = Some(bits(Der(content).expect(BIT_STRING)?)?),
                _ => return None,
            }
        }
        match (curve, point) {
            (Some(c), Some(p)) => ec_blob(c, p),
            (Some(_), None) => Some(Derived::Unchecked),
            _ => None,
        }
    }

    pub(super) fn ec_sec1(der: &[u8]) -> Option<Derived> {
        ec_private(der, None)
    }

    /// `PrivateKeyInfo ::= SEQUENCE { version, AlgorithmIdentifier,
    /// privateKey OCTET STRING, [0] attributes OPTIONAL, [1] publicKey
    /// OPTIONAL }`
    pub(super) fn pkcs8(der: &[u8]) -> Option<Derived> {
        let mut s = sequence(der)?;
        s.expect(INTEGER)?;
        let mut alg = Der(s.expect(SEQUENCE)?);
        let oid = alg.expect(OID)?;
        let params = alg.next();
        let private = s.expect(OCTET_STRING)?;
        let mut public = None;
        for (tag, content) in s.rest() {
            match tag {
                0xa0 => {}
                0x81 => public = Some(bits(content)?),
                _ => return None,
            }
        }
        match oid {
            RSA => rsa_pkcs1(private),
            EC => match params {
                Some((OID, curve)) => ec_private(private, Some(curve)),
                _ => None,
            },
            ED25519 => match public {
                Some(p) if p.len() == 32 => {
                    let mut blob = Vec::new();
                    string(&mut blob, b"ssh-ed25519");
                    string(&mut blob, p);
                    Some(Derived::Blob(blob))
                }
                Some(_) => None,
                None => Some(Derived::Unchecked),
            },
            DSA => Some(Derived::Unchecked),
            _ => None,
        }
    }
}

/// A public key, held to the one form this provider sends.
#[derive(Debug, PartialEq, Eq)]
struct PublicKey {
    key_type: &'static str,
    blob: Vec<u8>,
    comment: Option<String>,
}

impl PublicKey {
    /// The one line delivered: `<type> <blob> <comment>`.
    fn line(&self) -> String {
        match &self.comment {
            Some(c) => format!("{} {} {c}\n", self.key_type, b64().encode(&self.blob)),
            None => format!("{} {}\n", self.key_type, b64().encode(&self.blob)),
        }
    }
}

/// The key types this provider knows, and the file name OpenSSH gives each.
const KEY_TYPES: [(&str, &str); 8] = [
    ("ssh-ed25519", "ed25519"),
    ("ssh-rsa", "rsa"),
    ("ecdsa-sha2-nistp256", "ecdsa"),
    ("ecdsa-sha2-nistp384", "ecdsa"),
    ("ecdsa-sha2-nistp521", "ecdsa"),
    ("sk-ssh-ed25519@openssh.com", "ed25519_sk"),
    ("sk-ecdsa-sha2-nistp256@openssh.com", "ecdsa_sk"),
    ("ssh-dss", "dsa"),
];

/// Reads a whole `.pub` file: exactly one public key line, blank lines
/// aside, and nothing else. The error is a fixed message.
fn parse_public(bytes: &[u8]) -> Result<PublicKey, &'static str> {
    const NOT_ONE: &str = "the key's public half is not exactly one OpenSSH public key line";
    let text = std::str::from_utf8(bytes).map_err(|_| NOT_ONE)?;
    let mut lines = text
        .split('\n')
        .map(|l| l.strip_suffix('\r').unwrap_or(l))
        .filter(|l| !l.trim().is_empty());
    let line = lines.next().ok_or(NOT_ONE)?;
    if lines.next().is_some() {
        return Err(NOT_ONE);
    }
    let blank = |c: char| c == ' ' || c == '\t';
    let (key_type, rest) = line.split_once(blank).ok_or(NOT_ONE)?;
    let rest = rest.trim_start_matches(blank);
    let (blob_text, comment) = match rest.split_once(blank) {
        Some((b, c)) => (b, c.trim_matches(blank)),
        None => (rest, ""),
    };
    let key_type = KEY_TYPES
        .iter()
        .find(|(t, _)| *t == key_type)
        .map(|(t, _)| *t)
        .ok_or("the key's type is not one the provider knows")?;
    let blob = b64().decode(blob_text).map_err(|_| NOT_ONE)?;
    let mut r = blob.as_slice();
    if take_string(&mut r) != Some(key_type.as_bytes()) {
        return Err("the key's public half names one type and holds another");
    }
    if !printable(comment) || comment.len() > 1024 {
        return Err("the key's public comment is not printable text");
    }
    Ok(PublicKey {
        key_type,
        blob,
        comment: (!comment.is_empty()).then(|| comment.to_string()),
    })
}

impl Provider for SshKey {
    fn name(&self) -> &str {
        NAME
    }

    fn only(&self) -> Option<&Only> {
        self.settings.only.as_ref()
    }

    fn provide(&self, _ctx: &Context<'_>, reader: &dyn Reader) -> Contribution {
        let Some(key_file) = &self.settings.key_file else {
            return Contribution::problem(ProblemCode::MissingInput, "ssh-key.key_file is not set");
        };
        let private = match read_setting_file(reader, key_file, MAX_KEY_BYTES, "ssh-key.key_file") {
            Ok(f) => f,
            Err(c) => return c,
        };
        let public = match read_setting_file(
            reader,
            &format!("{key_file}.pub"),
            MAX_KEY_BYTES,
            "the key's public half",
        ) {
            Ok(f) => f,
            Err(c) => return c,
        };
        const UNKNOWN: &str = "ssh-key.key_file is not a private key in a form the provider knows";
        let Ok(private_text) = std::str::from_utf8(&private.bytes) else {
            return Contribution::problem(ProblemCode::Unreadable, UNKNOWN);
        };
        let derived = match classify(private_text) {
            KeyForm::Encrypted => {
                return Contribution::problem(
                    ProblemCode::PassphraseProtected,
                    "the key has a passphrase; keys with one are not delivered",
                )
            }
            KeyForm::Unknown => return Contribution::problem(ProblemCode::Unreadable, UNKNOWN),
            KeyForm::Unchecked => {
                return Contribution::problem(
                    ProblemCode::Unreadable,
                    "the key's public half cannot be checked against a key of this form; use an OpenSSH key",
                )
            }
            KeyForm::Clear(blob) => blob,
        };
        let public = match parse_public(&public.bytes) {
            Ok(p) => p,
            Err(message) => return Contribution::problem(ProblemCode::Unreadable, message),
        };
        if public.blob != derived {
            return Contribution::problem(
                ProblemCode::Unreadable,
                "the public half is not the private key's",
            );
        }
        let ty = KEY_TYPES
            .iter()
            .find(|(t, _)| *t == public.key_type)
            .map(|(_, f)| *f)
            .expect("a known type");
        let path = format!(".ssh/id_{ty}");
        let file = |name: &str, path: String, mode: Mode, content: Vec<u8>| Draft {
            name: name.into(),
            body: ItemBody::File(FileItem {
                stage: Stage::One,
                base: Base::Home,
                path,
                mode,
                content,
            }),
            expires_at: None,
        };
        let mut c = Contribution {
            items: vec![
                file("ssh-key", path.clone(), Mode::M0600, private.bytes),
                file(
                    "ssh-key.pub",
                    format!("{path}.pub"),
                    Mode::M0644,
                    public.line().into_bytes(),
                ),
            ],
            ..Contribution::default()
        };
        if self.settings.sign_commits {
            let signing_key = format!("{HOME_DIR}/{path}");
            c.git_config = vec![
                GitSection::new("gpg", &[("format", "ssh")]),
                GitSection::new("user", &[("signingkey", &signing_key)]),
                GitSection::new("commit", &[("gpgsign", "true")]),
            ];
        }
        c
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn wire(s: &[u8]) -> Vec<u8> {
        let mut out = (s.len() as u32).to_be_bytes().to_vec();
        out.extend_from_slice(s);
        out
    }

    #[test]
    fn forms() {
        assert_eq!(
            classify(
                "-----BEGIN ENCRYPTED PRIVATE KEY-----\nMII\n-----END ENCRYPTED PRIVATE KEY-----\n"
            ),
            KeyForm::Encrypted
        );
        assert_eq!(
            classify("-----BEGIN RSA PRIVATE KEY-----\nProc-Type: 4,ENCRYPTED\nDEK-Info: AES-128-CBC,00\n\nMII\n-----END RSA PRIVATE KEY-----\n"),
            KeyForm::Encrypted
        );
        assert_eq!(
            classify("-----BEGIN RSA PRIVATE KEY-----\nMIIabc\n-----END RSA PRIVATE KEY-----\n"),
            KeyForm::Unknown
        );
        assert_eq!(classify("ssh-ed25519 AAAA"), KeyForm::Unknown);
        assert_eq!(
            classify(
                "-----BEGIN OPENSSH PRIVATE KEY-----\n!!!\n-----END OPENSSH PRIVATE KEY-----\n"
            ),
            KeyForm::Unknown
        );
    }

    /// A made-up RSA key (not a key): version, n, e and seven small fields.
    #[test]
    fn rsa_pkcs1_and_pkcs8_give_the_ssh_blob() {
        let int = |b: &[u8]| [&[0x02, b.len() as u8][..], b].concat();
        let n = [0x00, 0xc1, 0x02, 0x03];
        let fields = [
            int(&[0]),
            int(&n),
            int(&[1, 0, 1]),
            int(&[5]),
            int(&[6]),
            int(&[7]),
            int(&[8]),
            int(&[9]),
            int(&[10]),
        ]
        .concat();
        let rsa = [&[0x30, fields.len() as u8][..], &fields].concat();
        let want = [wire(b"ssh-rsa"), wire(&[1, 0, 1]), wire(&n)].concat();
        assert_eq!(der::rsa_pkcs1(&rsa), Some(Derived::Blob(want.clone())));
        let alg = [
            0x30, 0x0d, 0x06, 0x09, 0x2a, 0x86, 0x48, 0x86, 0xf7, 0x0d, 0x01, 0x01, 0x01, 0x05,
            0x00,
        ];
        let inner = [&[0x04, rsa.len() as u8][..], &rsa].concat();
        let body = [&[0x02, 0x01, 0x00][..], &alg, &inner].concat();
        let p8 = [&[0x30, body.len() as u8][..], &body].concat();
        assert_eq!(der::pkcs8(&p8), Some(Derived::Blob(want)));
    }

    #[test]
    fn public_halves() {
        let blob = [wire(b"ssh-ed25519"), wire(&[0x5a; 32])].concat();
        let b = b64().encode(&blob);
        let ok = |t: &str| parse_public(t.as_bytes());
        assert!(
            ok(&format!(" ssh-ed25519 {b}\n")).is_err(),
            "a leading blank"
        );
        let p = ok(&format!(
            "\nssh-ed25519   {b}  ada@example.test laptop \r\n\n"
        ))
        .unwrap();
        assert_eq!(
            p.line(),
            format!("ssh-ed25519 {b} ada@example.test laptop\n")
        );
        assert_eq!(
            ok(&format!("ssh-ed25519 {b}")).unwrap().line(),
            format!("ssh-ed25519 {b}\n")
        );
        for bad in [
            format!("ssh-ed25519 {b} ok\n-----BEGIN OPENSSH PRIVATE KEY-----\nAAAA\n-----END OPENSSH PRIVATE KEY-----\n"),
            format!("ssh-ed25519 {b}\nssh-ed25519 {b}\n"),
            format!("ssh-rsa {b}\n"),
            format!("ssh-ed25519 {b} bell\u{7}\n"),
            format!("ssh-ed25519 {b}!\n"),
            format!("command=\"x\" ssh-ed25519 {b}\n"),
            "ssh-foo AAAA\n".to_string(),
            String::new(),
        ] {
            assert!(ok(&bad).is_err(), "{bad:?}");
        }
    }
}
