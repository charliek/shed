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
//!
//! On a machine [`gather`] reads the key file and the `.pub` beside it;
//! [`build`] judges the pair, so a host holding the two itself judges them
//! the same way. [`public_line`] reads the public half out of a private key
//! (the formats carry it: no cryptography), and [`encode_ed25519`] writes an
//! OpenSSH private key from an Ed25519 key's bytes.

use std::fmt;

use base64::Engine as _;
use shed_provision::{
    limits::HOME_DIR, printable, Base, Context, Contribution, GitSection, Mode, ProblemCode, Reader,
};

use crate::common::{desktop_provider, file_draft, gathered, read_setting_file, Redacted};
use crate::settings::SshKeySettings;
use crate::Gathered;

/// The provider's name.
pub const NAME: &str = "ssh-key";

/// The most a key file may hold: far more than any real key.
const MAX_KEY_BYTES: usize = 64 << 10;

/// What the provider delivers from.
#[derive(Clone, PartialEq, Eq)]
pub struct Inputs {
    /// The private key, with no passphrase: OpenSSH, PEM or PKCS#8.
    pub private_key: Vec<u8>,
    /// The public key line, as a `.pub` file holds it: exactly one OpenSSH
    /// public key line (blank lines aside).
    pub public_key: Vec<u8>,
    /// Whether git in the shed signs commits with the key.
    pub sign_commits: bool,
}

impl fmt::Debug for Inputs {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("Inputs")
            .field("private_key", &Redacted(self.private_key.len()))
            .field("public_key", &Redacted(self.public_key.len()))
            .field("sign_commits", &self.sign_commits)
            .finish()
    }
}

desktop_provider!(
    /// The `ssh-key` provider.
    SshKey,
    SshKeySettings
);

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

impl KeyForm {
    /// The public blob, or why there is none.
    fn blob(self) -> Result<Vec<u8>, KeyError> {
        match self {
            KeyForm::Clear(blob) => Ok(blob),
            KeyForm::Encrypted => Err(KeyError::Passphrase),
            KeyForm::Unchecked => Err(KeyError::Unchecked),
            KeyForm::Unknown => Err(KeyError::Unknown),
        }
    }
}

/// An SSH wire string: its length, then its bytes.
pub(crate) fn put(out: &mut Vec<u8>, s: &[u8]) {
    out.extend((s.len() as u32).to_be_bytes());
    out.extend_from_slice(s);
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
    use super::{put as string, Derived};

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

/// The known key type named `t`, and its file name.
fn known_type(t: &[u8]) -> Option<(&'static str, &'static str)> {
    KEY_TYPES.iter().copied().find(|(k, _)| k.as_bytes() == t)
}

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
    let (key_type, _) =
        known_type(key_type.as_bytes()).ok_or("the key's type is not one the provider knows")?;
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

/// Why a private key gives no public line.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum KeyError {
    /// It has a passphrase.
    Passphrase,
    /// It is not a private key in a form this provider knows.
    Unknown,
    /// Its public half cannot be read out of it without key arithmetic.
    Unchecked,
}

impl fmt::Display for KeyError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(match self {
            KeyError::Passphrase => PASSPHRASE,
            KeyError::Unknown => "the key is not a private key in a form the provider knows",
            KeyError::Unchecked => UNCHECKED,
        })
    }
}

impl std::error::Error for KeyError {}

const PASSPHRASE: &str = "the key has a passphrase; keys with one are not delivered";
const UNCHECKED: &str =
    "the key's public half cannot be checked against a key of this form; use an OpenSSH key";

/// The public key line of `private_key` (`<type> <base64 blob>`, and the
/// key's comment when an OpenSSH key holds a printable one), with no line
/// end. Read out of the key, never computed: an OpenSSH key carries its
/// public half, and a PEM or PKCS#8 key the fields it is made of.
pub fn public_line(private_key: &[u8]) -> Result<String, KeyError> {
    let text = std::str::from_utf8(private_key).map_err(|_| KeyError::Unknown)?;
    let blob = classify(text).blob()?;
    let mut r = blob.as_slice();
    let (key_type, _) = take_string(&mut r)
        .and_then(known_type)
        .ok_or(KeyError::Unknown)?;
    let comment = openssh_comment(text, key_type)
        .filter(|c| !c.is_empty() && c.len() <= 1024 && printable(c) && c.trim() == c);
    let line = PublicKey {
        key_type,
        blob,
        comment,
    }
    .line();
    Ok(line.trim_end_matches('\n').to_string())
}

/// The comment an unencrypted OpenSSH key's private section holds, for the
/// key types whose private fields this reader can count.
fn openssh_comment(text: &str, key_type: &str) -> Option<String> {
    let (_, der) = pem_block(text, "OPENSSH PRIVATE KEY")?;
    let der = der?;
    let mut r = der.strip_prefix(b"openssh-key-v1\0")?;
    for _ in 0..3 {
        take_string(&mut r)?; // cipher (none), kdf, kdf options
    }
    r = r.get(4..)?; // the key count: one
    take_string(&mut r)?; // the public key
    let mut p = take_string(&mut r)?;
    if p.get(..4)? != p.get(4..8)? {
        return None;
    }
    p = &p[8..];
    if take_string(&mut p)? != key_type.as_bytes() {
        return None;
    }
    let fields = match key_type {
        "ssh-ed25519" => 2,
        "ssh-rsa" => 6,
        "ssh-dss" => 5,
        t if t.starts_with("ecdsa-sha2-") => 3,
        _ => return None,
    };
    for _ in 0..fields {
        take_string(&mut p)?;
    }
    String::from_utf8(take_string(&mut p)?.to_vec()).ok()
}

/// An OpenSSH private key (`-----BEGIN OPENSSH PRIVATE KEY-----`, no
/// passphrase) for the Ed25519 key whose 32-byte seed is `seed` and whose
/// public key is `public`, with `comment`. The key's bytes are written as
/// they are given: the caller makes the pair. The check integers OpenSSH
/// puts before the private fields only have to match each other in a key
/// with no cipher; they are taken from the public key here, so the output
/// depends on its inputs alone.
pub fn encode_ed25519(seed: &[u8; 32], public: &[u8; 32], comment: &str) -> String {
    let mut public_blob = Vec::new();
    put(&mut public_blob, b"ssh-ed25519");
    put(&mut public_blob, public);
    let mut private = Vec::new();
    private.extend_from_slice(&public[..4]);
    private.extend_from_slice(&public[..4]);
    put(&mut private, b"ssh-ed25519");
    put(&mut private, public);
    put(&mut private, &[seed.as_slice(), public.as_slice()].concat());
    put(&mut private, comment.as_bytes());
    let mut pad = 1u8;
    while private.len() % 8 != 0 {
        private.push(pad);
        pad += 1;
    }
    let mut blob = b"openssh-key-v1\0".to_vec();
    put(&mut blob, b"none");
    put(&mut blob, b"none");
    put(&mut blob, b"");
    blob.extend(1u32.to_be_bytes());
    put(&mut blob, &public_blob);
    put(&mut blob, &private);
    let body = b64().encode(&blob);
    let mut out = String::from("-----BEGIN OPENSSH PRIVATE KEY-----\n");
    for chunk in body.as_bytes().chunks(70) {
        out.push_str(std::str::from_utf8(chunk).expect("base64 is ASCII"));
        out.push('\n');
    }
    out.push_str("-----END OPENSSH PRIVATE KEY-----\n");
    out
}

/// Reads `key_file` and the `.pub` beside it. Nothing is judged here: the
/// pair is [`build`]'s.
pub fn gather(
    settings: &SshKeySettings,
    ctx: &Context<'_>,
    reader: &dyn Reader,
) -> Gathered<Inputs> {
    gathered(read_pair(settings, ctx, reader))
}

fn read_pair(
    settings: &SshKeySettings,
    ctx: &Context<'_>,
    reader: &dyn Reader,
) -> Result<Inputs, Contribution> {
    let Some(key_file) = &settings.key_file else {
        return Err(Contribution::problem(
            ProblemCode::MissingInput,
            "ssh-key.key_file is not set",
        ));
    };
    let private = read_setting_file(ctx, reader, key_file, MAX_KEY_BYTES, "ssh-key.key_file")?;
    let public = read_setting_file(
        ctx,
        reader,
        &format!("{key_file}.pub"),
        MAX_KEY_BYTES,
        "the key's public half",
    )?;
    Ok(Inputs {
        private_key: private.bytes,
        public_key: public.bytes,
        sign_commits: settings.sign_commits,
    })
}

/// The key pair's two items, and with `sign_commits` the git config that
/// signs commits with it, once the pair is judged: a key with no
/// passphrase, in a known form, whose public half is one line of a known
/// type and is the private key's.
pub fn build(inputs: &Inputs, _ctx: &Context<'_>) -> Contribution {
    const UNKNOWN: &str = "ssh-key.key_file is not a private key in a form the provider knows";
    let derived = match std::str::from_utf8(&inputs.private_key)
        .map_err(|_| KeyError::Unknown)
        .and_then(|text| classify(text).blob())
    {
        Ok(blob) => blob,
        Err(KeyError::Passphrase) => {
            return Contribution::problem(ProblemCode::PassphraseProtected, PASSPHRASE)
        }
        Err(KeyError::Unknown) => return Contribution::problem(ProblemCode::Unreadable, UNKNOWN),
        Err(KeyError::Unchecked) => {
            return Contribution::problem(ProblemCode::Unreadable, UNCHECKED)
        }
    };
    let public = match parse_public(&inputs.public_key) {
        Ok(p) => p,
        Err(message) => return Contribution::problem(ProblemCode::Unreadable, message),
    };
    if public.blob != derived {
        return Contribution::problem(
            ProblemCode::Unreadable,
            "the public half is not the private key's",
        );
    }
    let (_, ty) = known_type(public.key_type.as_bytes()).expect("a known type");
    let path = format!(".ssh/id_{ty}");
    let mut c = Contribution {
        items: vec![
            file_draft(NAME, Base::Home, &path, Mode::M0600, &inputs.private_key),
            file_draft(
                &format!("{NAME}.pub"),
                Base::Home,
                &format!("{path}.pub"),
                Mode::M0644,
                public.line().as_bytes(),
            ),
        ],
        ..Contribution::default()
    };
    if inputs.sign_commits {
        let signing_key = format!("{HOME_DIR}/{path}");
        c.git_config = vec![
            GitSection::new("gpg", &[("format", "ssh")]),
            GitSection::new("user", &[("signingkey", &signing_key)]),
            GitSection::new("commit", &[("gpgsign", "true")]),
        ];
    }
    c
}

#[cfg(test)]
mod tests {
    use super::*;
    use shed_provision::{FileItem, ItemBody, Stage};

    fn wire(s: &[u8]) -> Vec<u8> {
        let mut out = Vec::new();
        put(&mut out, s);
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

    /// RFC 8032's first Ed25519 test vector: a real key pair.
    const SEED: [u8; 32] = [
        0x9d, 0x61, 0xb1, 0x9d, 0xef, 0xfd, 0x5a, 0x60, 0xba, 0x84, 0x4a, 0xf4, 0x92, 0xec, 0x2c,
        0xc4, 0x44, 0x49, 0xc5, 0x69, 0x7b, 0x32, 0x69, 0x19, 0x70, 0x3b, 0xac, 0x03, 0x1c, 0xae,
        0x7f, 0x60,
    ];
    const PUBLIC: [u8; 32] = [
        0xd7, 0x5a, 0x98, 0x01, 0x82, 0xb1, 0x0a, 0xb7, 0xd5, 0x4b, 0xfe, 0xd3, 0xc9, 0x64, 0x07,
        0x3a, 0x0e, 0xe1, 0x72, 0xf3, 0xda, 0xa6, 0x23, 0x25, 0xaf, 0x02, 0x1a, 0x68, 0xf7, 0x07,
        0x51, 0x1a,
    ];

    #[test]
    fn an_encoded_key_round_trips_through_public_line_and_build() {
        let key = encode_ed25519(&SEED, &PUBLIC, "ada@example.test");
        let blob = [wire(b"ssh-ed25519"), wire(&PUBLIC)].concat();
        let want = format!("ssh-ed25519 {} ada@example.test", b64().encode(&blob));
        assert_eq!(public_line(key.as_bytes()), Ok(want.clone()));
        assert_eq!(classify(&key), KeyForm::Clear(blob));
        let c = build(
            &Inputs {
                private_key: key.clone().into_bytes(),
                public_key: want.clone().into_bytes(),
                sign_commits: true,
            },
            &Context::preview(0),
        );
        assert!(c.problems.is_empty(), "{:?}", c.problems);
        assert_eq!(c.items.len(), 2);
        assert_eq!(
            c.items[1].body,
            ItemBody::File(FileItem {
                stage: Stage::One,
                base: Base::Home,
                path: ".ssh/id_ed25519.pub".into(),
                mode: Mode::M0644,
                content: format!("{want}\n").into_bytes(),
            })
        );
        assert_eq!(c.git_config.len(), 3);
        // Every line is at most 70 characters, as ssh-keygen writes them, and
        // the container's private section is padded to whole blocks.
        assert!(key.lines().all(|l| l.len() <= 70));
        // No comment: none in the line.
        let bare = encode_ed25519(&SEED, &PUBLIC, "");
        assert_eq!(
            public_line(bare.as_bytes()).unwrap(),
            format!(
                "ssh-ed25519 {}",
                b64().encode([wire(b"ssh-ed25519"), wire(&PUBLIC)].concat())
            )
        );
        // OpenSSH itself reads the key and derives the same public half, when it
        // is installed here.
        let dir = std::env::temp_dir().join(format!("shed-encode-{}", std::process::id()));
        std::fs::create_dir_all(&dir).unwrap();
        let path = dir.join("id_ed25519");
        std::fs::write(&path, &key).unwrap();
        use std::os::unix::fs::PermissionsExt as _;
        std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o600)).unwrap();
        if let Ok(out) = std::process::Command::new("ssh-keygen")
            .arg("-y")
            .arg("-f")
            .arg(&path)
            .output()
        {
            assert!(
                out.status.success(),
                "{}",
                String::from_utf8_lossy(&out.stderr)
            );
            let got = String::from_utf8(out.stdout).unwrap();
            assert_eq!(got.trim_end(), want);
        }
        std::fs::remove_dir_all(&dir).unwrap();
    }

    #[test]
    fn public_line_refuses_what_build_refuses() {
        assert_eq!(public_line(b"not a key"), Err(KeyError::Unknown));
        assert_eq!(public_line(&[0xff]), Err(KeyError::Unknown));
        assert_eq!(
            public_line(
                b"-----BEGIN ENCRYPTED PRIVATE KEY-----\nMII\n-----END ENCRYPTED PRIVATE KEY-----\n"
            ),
            Err(KeyError::Passphrase)
        );
        let shown = format!(
            "{:?}",
            Inputs {
                private_key: b"SENTINEL".to_vec(),
                public_key: Vec::new(),
                sign_commits: false
            }
        );
        assert!(!shown.contains("SENTINEL"), "{shown}");
    }
}
