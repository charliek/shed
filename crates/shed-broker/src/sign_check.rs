//! **What the broker will sign** — the pure parser and policy for `ssh-agent` sign
//! requests that reach this machine from a shed (plan 035 §3.5).
//!
//! A shed forwards SSH signing requests to the user's machine. Without a check the
//! machine signs ANY bytes once approved, so code inside a shed could obtain a
//! signature that logs in to any SSH server as any username with the user's key —
//! shed-server's own `_bootstrap` channel included, which mints credentials. This
//! module decides, from the bytes alone, whether a request is one of the two things a
//! shed legitimately needs signed. **A request is signed only if it parses EXACTLY as
//! one of two known shapes; everything else is refused.**
//!
//! Nothing here performs I/O or calls the approval gate; [`SignPolicy::check`] is a
//! pure function the bus runs before the gate.
//!
//! The two directions of that contract are not equally costly. Accepting something
//! outside the two shapes is the hole this module closes; refusing something a genuine
//! OpenSSH client sends breaks git over SSH for the user. Every rule below is one a
//! genuine OpenSSH client's request already satisfies, and where a rule could refuse a
//! real-world input, the rule's doc says which and why that is acceptable.
//!
//! # Shape 1: an SSH public-key login request
//!
//! The data an SSH client signs for `publickey` user authentication (RFC 4252 §7),
//! and OpenSSH's host-bound variant (OpenSSH `PROTOCOL` §3.1, "Host-bound public key
//! authentication", checked against the file at tag `V_9_6_P1`). Fields in order, RFC
//! 4251 encoding:
//!
//! | # | field | rule |
//! |---|---|---|
//! | 1 | `string` session id | length 20, 32, 48 or 64 (the output sizes of every SSH key-exchange hash: SHA-1, SHA-256, SHA-384, SHA-512) |
//! | 2 | `byte` | 50 (`SSH_MSG_USERAUTH_REQUEST`) |
//! | 3 | `string` username | no NUL; then policy: never beginning with `_`; byte-equal to an `allow_users` entry |
//! | 4 | `string` service | exactly `ssh-connection` |
//! | 5 | `string` method | exactly `publickey` or `publickey-hostbound-v00@openssh.com` |
//! | 6 | `boolean` | exactly 1 (a signature follows) |
//! | 7 | `string` algorithm | consistent with the key: its own type name; for an `ssh-rsa` key one of `ssh-rsa`, `rsa-sha2-256`, `rsa-sha2-512` — and, for RSA, the request's flags must name the same hash (see "The flags") |
//! | 8 | `string` key blob | byte-equal to the key the request asks to sign with |
//! | 9 | `string` server host key | host-bound method ONLY, and exactly one: a valid public key or OpenSSH certificate (see "The host key") — a real capture shows that against a certificate-presenting server the client sends the certificate itself |
//! | 10 | — | no trailing bytes |
//!
//! These are the rules OpenSSH's own agent applies in `parse_userauth_request`
//! (`ssh-agent.c`, same tag) — sig-follows exactly 1, service `ssh-connection`, key
//! equal to the requested key, algorithm of the key's type, a parsed host key for the
//! host-bound method, nothing trailing — plus the session-id length set, NUL-free
//! strings everywhere (OpenSSH reads them with `sshbuf_get_cstring`, which refuses an
//! embedded NUL) and the username policy.
//!
//! # Shape 2: an SSHSIG signed-data blob (`git` commit signing)
//!
//! OpenSSH `PROTOCOL.sshsig` ("Signed Data, of which the signature goes into the
//! blob above"): the six bytes `SSHSIG`; `string` namespace — must be `git`;
//! `string` reserved — must be empty; `string` hash algorithm — `sha256` or `sha512`;
//! `string` H(message) — 32 or 64 bytes to match; no trailing bytes. Data beginning
//! with the six bytes `SSHSIG` is judged by these rules ONLY (it is never retried as a
//! login request; no login request can begin with them anyway, since they would
//! declare a 1.4 GB session id). There is no setting that allows another namespace.
//!
//! # The key
//!
//! The key to sign with must be a valid Ed25519, ECDSA (P-256/384/521) or RSA public
//! key (the rules under "Key validity"), must ALSO parse with
//! `ssh_key::PublicKey::from_bytes` (which the bus uses to decode it) and re-encode to
//! exactly the bytes given. Refused as [`RefusalClass::MalformedKey`]:
//!
//! - **certificates** — today's bus already refuses them (`ssh_key` cannot decode a
//!   certificate as a public key), the local-keys backend cannot load them, and a key
//!   filter by fingerprint could not name one;
//! - **FIDO `sk-` keys** — the broker cannot return their signatures intact: an `sk-`
//!   signature carries a flags byte and counter after the signature blob, which the
//!   agent-forward backend drops (`parse_signature` keeps format and blob only) and the
//!   bus response pins to an empty `rest`; the local-keys backend cannot load them.
//!   (They ARE valid in the host-key field: see "The host key".)
//! - **DSA** — the local-keys backend never signs with it and OpenSSH 9.x disables it;
//! - any other unknown algorithm, and any key that fails "Key validity".
//!
//! # Key validity
//!
//! Applied to the signing key, to a plain host key, to the key a certificate
//! certifies and to a certificate's CA key. Canonical encoding is not validity: an RSA
//! blob with modulus 1 and exponent -1 decodes and re-encodes perfectly well. So:
//!
//! - **RSA** (`ssh-rsa`: `mpint e`, `mpint n`): both positive; `e` odd; `n` at least
//!   1024 bits (OpenSSH's `SSH_RSA_MINIMUM_MODULUS_SIZE`: OpenSSH refuses a smaller
//!   key outright, so no genuine client offers one). Checked by hand on the mpint
//!   bytes.
//! - **ECDSA** (`string` curve, `string` point): the curve name matches the key type,
//!   the point is uncompressed (leading `0x04`, exactly `1 + 2 * field size` bytes —
//!   OpenSSH's `sshbuf_get_ec` refuses any other form) and **on the curve** (checked
//!   with the `p256`/`p384`/`p521` crates, already direct dependencies for local-keys
//!   signing: `PublicKey::from_sec1_bytes` refuses a point off the curve, a coordinate
//!   not below the field prime, and the identity).
//! - **Ed25519** (`string` of exactly 32 bytes): length only, as OpenSSH's own parser
//!   does; it does not decompress the point either.
//! - **DSA** (host keys only; `mpint p, q, g, y`): each positive. DSA is checked
//!   structurally only: OpenSSH 9.x disables it by default and OpenSSH 10 removed it.
//! - **`sk-ssh-ed25519@openssh.com`** / **`sk-ecdsa-sha2-nistp256@openssh.com`**
//!   (host keys only; `PROTOCOL.u2f`): the Ed25519 or P-256 rules above, then a
//!   NUL-free `string` application.
//!
//! Every mpint is read as OpenSSH's `sshbuf_get_bignum2_bytes_direct` reads it:
//! non-negative, at most 2048 magnitude bytes (`SSHBUF_MAX_BIGNUM`, 16384 bits) and at
//! most one byte more as written. A superfluous leading zero is tolerated INSIDE the
//! host-key field, as OpenSSH's reader trims it (a CA signs certificate bytes
//! verbatim), while the signing key's canonical re-encoding check above refuses it
//! there.
//!
//! # The host key (field 9)
//!
//! The field holds the server host key the client received, which may be any type a
//! server can present: a plain key of any type above (security-key types included —
//! the broker refuses them only as SIGNING keys), or an OpenSSH certificate of any of
//! those types (`PROTOCOL.certkeys`, `PROTOCOL.u2f`). A certificate is walked field
//! by field, every nested structure checked to exactly fill its field:
//!
//! - type name; `string` nonce; the certified key's fields, which must pass "Key
//!   validity"; `uint64` serial; `uint32` type, 1 (user) or 2 (host);
//! - `string` key id, NUL-free;
//! - `string` valid principals: a sequence of NUL-free `string`s, at most 256
//!   (OpenSSH's `SSHKEY_CERT_MAX_PRINCIPALS`), exactly filling the field;
//! - `uint64` valid after, `uint64` valid before (any values: "forever" is 2^64-1);
//! - `string` critical options and `string` extensions: each a sequence of `string`
//!   name and `string` data pairs, exactly filling the field;
//! - `string` reserved (any content, as OpenSSH ignores it);
//! - `string` signature key: a plain (not certificate) public key passing "Key
//!   validity";
//! - `string` signature: exactly a `string` format and a non-empty `string` blob, the
//!   format a signature algorithm of the signature key's type (`ssh-rsa`,
//!   `rsa-sha2-256` or `rsa-sha2-512` for RSA; the key's own name otherwise), plus the
//!   `byte` flags and `uint32` counter `PROTOCOL.u2f` appends for an `sk-` format (a
//!   CA held on a security key produces those, so a genuine certificate can carry
//!   them);
//! - no trailing bytes.
//!
//! **The CA signature is NOT verified.** The rule is that the field is a well-formed
//! host key, not that it is trusted: the server already proved to the client that it
//! holds the key, and a signature over a payload naming a host key no server holds is
//! accepted by no server. OpenSSH itself verifies the CA signature when it parses a
//! certificate, so a certificate with a bad signature is one no genuine client sends;
//! accepting one here only means the resulting signature is useless.
//!
//! # The flags
//!
//! The agent request's `flags` word is an input to the check and part of the approval.
//! For an **RSA** key its two hash bits (`SSH_AGENT_RSA_SHA2_256` = 2,
//! `SSH_AGENT_RSA_SHA2_512` = 4) choose the signature algorithm, so they must agree
//! with the payload:
//!
//! - in a login request: `ssh-rsa` — neither bit; `rsa-sha2-256` — only bit 2;
//!   `rsa-sha2-512` — only bit 4. Otherwise a payload naming `rsa-sha2-512` could be
//!   signed with SHA-1. A genuine client derives the flags from the algorithm it puts
//!   in the payload (OpenSSH's `ssh_agent_sign`, x/crypto's `SignWithAlgorithm`), and
//!   a server refuses a signature whose algorithm differs from the payload's, so no
//!   login that could succeed is refused;
//! - in an SSHSIG blob: exactly one of the two bits. Flags of 0 would produce the
//!   legacy `ssh-rsa` (SHA-1) signature, which `PROTOCOL.sshsig` forbids and
//!   `ssh-keygen -Y verify` refuses; `ssh-keygen -Y sign` asks for `rsa-sha2-512`;
//! - both bits set is refused in either shape (no genuine client sets both).
//!
//! Other bits are left alone, and for every other key type the flags pass through
//! unchanged, as the broker passes them today (the backend refuses an RSA hash bit on
//! a non-RSA key). A flags mismatch is refused as [`RefusalClass::UnparseableData`]:
//! the (data, flags) pair is not exactly one of the two shapes, which is what that
//! class means; the detail names the rule.
//!
//! # The policy and its refusals
//!
//! [`SignPolicy`] holds `allow_users` (the later default, [`DEFAULT_ALLOW_USERS`], is
//! `[git]`) and an optional [`KeyFilter`] (a set of SHA-256 digests of key blobs). A
//! username beginning with `_` is refused whatever the list says; username comparison
//! is byte-exact (no case folding, no trimming). Checks run in this order, and the
//! first failure is the refusal: key valid → key offered (when a filter is set) →
//! data shape and flags → username or namespace. Each refusal carries a
//! [`RefusalClass`] and a bounded, escaped detail — never the raw request.
//!
//! # The invariant: the checked bytes are the signed bytes
//!
//! **The key and data buffers and the flags given to [`SignPolicy::check`] are the
//! ones later given to the backend's `sign`.** The types enforce it: `check` returns
//! an [`Approved`] that borrows exactly those two buffers and holds the flags, and the
//! only way to use it is [`Approved::sign`], which consumes it and hands those same
//! buffers and flags to [`SshBackend::sign`]. `Approved` has no public constructor, no
//! byte accessors, is not `Clone`, and its `Debug` prints only lengths and the key's
//! fingerprint, so a caller cannot check one buffer and sign another, change the
//! flags, or sign twice on one check.

use std::collections::HashSet;
use std::fmt::{self, Write as _};

use base64::Engine as _;
use sha2::{Digest, Sha256};
use ssh_key::public::PublicKey;

use crate::ssh_backend::{SshBackend, SshSignature};
use crate::ssh_wire::Reader;

/// The usernames a login signature is allowed for when nothing else is configured.
pub const DEFAULT_ALLOW_USERS: &[&str] = &["git"];

/// The only SSHSIG namespace that is signed (git commit and tag signing).
pub const SSHSIG_NAMESPACE: &str = "git";

/// A username beginning with this byte is never signed, whatever `allow_users` says
/// (shed-server's internal channels, such as `_bootstrap`, use it).
pub const RESERVED_USERNAME_PREFIX: u8 = b'_';

/// The longest [`Refusal::detail`] can be, in bytes.
pub const MAX_DETAIL_BYTES: usize = 320;

/// `SSH_AGENT_RSA_SHA2_256` (draft-miller-ssh-agent §4.5.1): sign with `rsa-sha2-256`.
pub const SSH_AGENT_RSA_SHA2_256: u32 = 2;
/// `SSH_AGENT_RSA_SHA2_512`: sign with `rsa-sha2-512`.
pub const SSH_AGENT_RSA_SHA2_512: u32 = 4;
const RSA_HASH_BITS: u32 = SSH_AGENT_RSA_SHA2_256 | SSH_AGENT_RSA_SHA2_512;

const SSH_MSG_USERAUTH_REQUEST: u8 = 50;
const SESSION_ID_LENGTHS: [usize; 4] = [20, 32, 48, 64];
const SERVICE_CONNECTION: &[u8] = b"ssh-connection";
const METHOD_PUBLICKEY: &[u8] = b"publickey";
const METHOD_HOSTBOUND: &[u8] = b"publickey-hostbound-v00@openssh.com";
const SSHSIG_MAGIC: &[u8] = b"SSHSIG";
/// The RSA login algorithms and the flags' hash bits each requires.
const RSA_LOGIN_ALGORITHMS: [(&[u8], u32); 3] = [
    (b"ssh-rsa", 0),
    (b"rsa-sha2-256", SSH_AGENT_RSA_SHA2_256),
    (b"rsa-sha2-512", SSH_AGENT_RSA_SHA2_512),
];
/// How many bytes of a shed-supplied value (username, namespace, algorithm name) a
/// refusal detail quotes.
const DETAIL_VALUE_BYTES: usize = 64;
/// OpenSSH's `SSH_RSA_MINIMUM_MODULUS_SIZE`.
const RSA_MIN_MODULUS_BITS: usize = 1024;
/// OpenSSH's `SSHBUF_MAX_BIGNUM`: the most magnitude bytes an mpint may have.
const MAX_MPINT_BYTES: usize = 16384 / 8;
/// OpenSSH's `SSHKEY_CERT_MAX_PRINCIPALS`.
const MAX_CERT_PRINCIPALS: usize = 256;
const CERT_SUFFIX: &[u8] = b"-cert-v01@openssh.com";

/// Why a sign request was refused — the class the audit records.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash)]
pub enum RefusalClass {
    /// The sign request envelope itself could not be decoded (bad JSON field, bad
    /// base64). [`SignPolicy::check`] never returns it — it takes decoded buffers —
    /// but the caller decoding the envelope records it through
    /// [`Refusal::malformed_request`], so every class has one name.
    MalformedRequest,
    /// The key to sign with does not parse, is not a valid key, or is of a type the
    /// broker does not sign with (certificate, `sk-`, DSA, unknown).
    MalformedKey,
    /// A key filter is set and the key is not in it.
    KeyNotOffered,
    /// A well-formed login request for a username not in `allow_users`.
    UsernameNotAllowed,
    /// A well-formed login request for a username beginning with `_`.
    ReservedUsername,
    /// A well-formed SSHSIG blob whose namespace is not `git`.
    WrongNamespace,
    /// The data is neither an exact login request nor an exact SSHSIG blob, or (for
    /// an RSA key) the flags do not name the hash the data does.
    UnparseableData,
}

impl RefusalClass {
    /// Every class, in a fixed order.
    pub const ALL: [RefusalClass; 7] = [
        RefusalClass::MalformedRequest,
        RefusalClass::MalformedKey,
        RefusalClass::KeyNotOffered,
        RefusalClass::UsernameNotAllowed,
        RefusalClass::ReservedUsername,
        RefusalClass::WrongNamespace,
        RefusalClass::UnparseableData,
    ];

    /// The stable machine name (what an audit line records).
    pub fn as_str(self) -> &'static str {
        match self {
            RefusalClass::MalformedRequest => "malformed_request",
            RefusalClass::MalformedKey => "malformed_key",
            RefusalClass::KeyNotOffered => "key_not_offered",
            RefusalClass::UsernameNotAllowed => "username_not_allowed",
            RefusalClass::ReservedUsername => "reserved_username",
            RefusalClass::WrongNamespace => "wrong_namespace",
            RefusalClass::UnparseableData => "unparseable_data",
        }
    }
}

impl fmt::Display for RefusalClass {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(self.as_str())
    }
}

/// A refused sign request: its [`RefusalClass`] and a detail of at most
/// [`MAX_DETAIL_BYTES`] printable ASCII bytes. Any shed-supplied value in the detail
/// (a username, a namespace, an algorithm name) is escaped and cut to 64 bytes; the
/// raw request never appears in it.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Refusal {
    class: RefusalClass,
    detail: String,
}

impl Refusal {
    fn new(class: RefusalClass, detail: String) -> Refusal {
        debug_assert!(detail.len() <= MAX_DETAIL_BYTES);
        Refusal { class, detail }
    }

    fn unparseable(reason: &'static str) -> Refusal {
        Refusal::new(RefusalClass::UnparseableData, reason.to_string())
    }

    fn malformed_key(detail: String) -> Refusal {
        Refusal::new(RefusalClass::MalformedKey, detail)
    }

    /// The refusal for a sign request whose envelope could not be decoded, for the
    /// caller that decodes it. The detail is a fixed reason chosen by that caller,
    /// never request content.
    pub fn malformed_request(reason: &'static str) -> Refusal {
        Refusal::new(RefusalClass::MalformedRequest, bounded(reason.as_bytes()))
    }

    pub fn class(&self) -> RefusalClass {
        self.class
    }

    pub fn detail(&self) -> &str {
        &self.detail
    }
}

impl fmt::Display for Refusal {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(f, "{}: {}", self.class, self.detail)
    }
}

impl std::error::Error for Refusal {}

/// Which `publickey` method a login request uses.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum LoginMethod {
    /// `publickey` (RFC 4252 §7).
    PublicKey,
    /// `publickey-hostbound-v00@openssh.com` (OpenSSH `PROTOCOL` §3.1).
    HostBound,
}

/// The hash an SSHSIG blob names.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum SshSigHash {
    Sha256,
    Sha512,
}

/// What an approved request is, for the audit and the approval prompt.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum SignKind {
    /// A login as `username` (an `allow_users` entry).
    Login {
        username: String,
        method: LoginMethod,
    },
    /// A `git` SSHSIG signature.
    SshSig { hash: SshSigHash },
}

/// A request that passed [`SignPolicy::check`]. It borrows the exact key and data
/// buffers that were checked and holds the checked flags, and [`Approved::sign`] is
/// the only thing that can be done with them — see the module doc's invariant.
#[must_use = "an approved request is signed through Approved::sign"]
pub struct Approved<'a> {
    public_key: &'a [u8],
    data: &'a [u8],
    flags: u32,
    key_type: KeyType,
    kind: SignKind,
}

/// Lengths and the key's fingerprint only: never the bytes (an approval may be
/// logged).
impl fmt::Debug for Approved<'_> {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("Approved")
            .field("key", &fingerprint(self.public_key))
            .field("key_len", &self.public_key.len())
            .field("data_len", &self.data.len())
            .finish_non_exhaustive()
    }
}

impl<'a> Approved<'a> {
    /// What the request is (login or SSHSIG, and its details).
    pub fn kind(&self) -> &SignKind {
        &self.kind
    }

    /// The key's type name (`ssh-ed25519`, `ssh-rsa`, `ecdsa-sha2-nistp256`, …).
    pub fn key_type(&self) -> &'static str {
        self.key_type.name_str()
    }

    /// The checked flags word, which [`Approved::sign`] passes to the backend.
    pub fn flags(&self) -> u32 {
        self.flags
    }

    /// Sign the checked data with the checked key under the checked flags: exactly
    /// the two buffers and the flags given to [`SignPolicy::check`] go to
    /// `backend.sign`. Consumes the approval, so one check signs once.
    pub fn sign(self, backend: &dyn SshBackend) -> Result<SshSignature, String> {
        backend.sign(self.public_key, self.data, self.flags)
    }
}

/// The SHA-256 digest of a key blob (the SSH-wire public key), which a
/// [`KeyFilter`] holds. It is what `ssh-add -l` prints, base64-encoded, after
/// `SHA256:`.
pub fn key_blob_digest(blob: &[u8]) -> [u8; 32] {
    Sha256::digest(blob).into()
}

/// The keys a shed may use: a set of [`key_blob_digest`] values.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct KeyFilter {
    digests: HashSet<[u8; 32]>,
}

impl KeyFilter {
    pub fn new() -> KeyFilter {
        KeyFilter::default()
    }

    pub fn from_digests(digests: impl IntoIterator<Item = [u8; 32]>) -> KeyFilter {
        KeyFilter {
            digests: digests.into_iter().collect(),
        }
    }

    pub fn insert_digest(&mut self, digest: [u8; 32]) {
        self.digests.insert(digest);
    }

    pub fn insert_key_blob(&mut self, blob: &[u8]) {
        self.digests.insert(key_blob_digest(blob));
    }

    pub fn contains_key_blob(&self, blob: &[u8]) -> bool {
        self.digests.contains(&key_blob_digest(blob))
    }

    pub fn len(&self) -> usize {
        self.digests.len()
    }

    pub fn is_empty(&self) -> bool {
        self.digests.is_empty()
    }
}

/// What this machine signs for a shed: the usernames a login may name and,
/// optionally, the keys that may be used. See the module doc.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct SignPolicy {
    allow_users: Vec<String>,
    offer_keys: Option<KeyFilter>,
}

impl Default for SignPolicy {
    /// `allow_users = [git]`, no key filter.
    fn default() -> SignPolicy {
        SignPolicy::new(DEFAULT_ALLOW_USERS.iter().copied(), None)
    }
}

impl SignPolicy {
    /// A policy from its two settings. An `allow_users` entry beginning with `_` (or
    /// an empty one) can never match: such usernames are refused at check time
    /// whatever the list holds.
    pub fn new(
        allow_users: impl IntoIterator<Item = impl Into<String>>,
        offer_keys: Option<KeyFilter>,
    ) -> SignPolicy {
        SignPolicy {
            allow_users: allow_users.into_iter().map(Into::into).collect(),
            offer_keys,
        }
    }

    pub fn allow_users(&self) -> &[String] {
        &self.allow_users
    }

    pub fn offer_keys(&self) -> Option<&KeyFilter> {
        self.offer_keys.as_ref()
    }

    /// Decide whether to sign `data` with the key whose SSH-wire blob is
    /// `public_key`, under the agent request's `flags`. On success the returned
    /// [`Approved`] borrows exactly these two buffers, holds these flags, and is the
    /// only way to sign them.
    pub fn check<'a>(
        &self,
        public_key: &'a [u8],
        data: &'a [u8],
        flags: u32,
    ) -> Result<Approved<'a>, Refusal> {
        let key_type = signing_key_type(public_key)?;
        if let Some(filter) = &self.offer_keys {
            if !filter.contains_key_blob(public_key) {
                return Err(Refusal::new(
                    RefusalClass::KeyNotOffered,
                    format!("key {}", fingerprint(public_key)),
                ));
            }
        }
        let kind = if data.starts_with(SSHSIG_MAGIC) {
            let sig = parse_sshsig(data)?;
            if key_type == KeyType::Rsa && !matches!(flags & RSA_HASH_BITS, 2 | 4) {
                return Err(Refusal::unparseable(
                    "sshsig: RSA flags do not name exactly one SHA-2 hash",
                ));
            }
            if sig.namespace != SSHSIG_NAMESPACE.as_bytes() {
                return Err(Refusal::new(
                    RefusalClass::WrongNamespace,
                    format!("sshsig namespace={}", bounded(sig.namespace)),
                ));
            }
            SignKind::SshSig { hash: sig.hash_alg }
        } else {
            let login = parse_login(data, public_key, key_type)?;
            if key_type == KeyType::Rsa && !rsa_login_flags_match(login.algorithm, flags) {
                return Err(Refusal::unparseable(
                    "login: RSA flags do not name the algorithm's hash",
                ));
            }
            SignKind::Login {
                username: self.allowed_username(login.username)?,
                method: login.method,
            }
        };
        Ok(Approved {
            public_key,
            data,
            flags,
            key_type,
            kind,
        })
    }

    /// The `allow_users` entry `username` is byte-equal to, unless it is reserved.
    fn allowed_username(&self, username: &[u8]) -> Result<String, Refusal> {
        let detail = || format!("login user={}", bounded(username));
        if username.first() == Some(&RESERVED_USERNAME_PREFIX) {
            return Err(Refusal::new(RefusalClass::ReservedUsername, detail()));
        }
        self.allow_users
            .iter()
            .find(|allowed| !username.is_empty() && allowed.as_bytes() == username)
            .cloned()
            .ok_or_else(|| Refusal::new(RefusalClass::UsernameNotAllowed, detail()))
    }
}

/// Whether an RSA login's flags carry exactly the hash bits its algorithm needs.
fn rsa_login_flags_match(algorithm: &[u8], flags: u32) -> bool {
    RSA_LOGIN_ALGORITHMS
        .iter()
        .any(|(name, bits)| *name == algorithm && flags & RSA_HASH_BITS == *bits)
}

// ----- key types and key validity -----

/// Every public-key type this module recognises, as a signing key, a host key, a
/// certified key or a CA key.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub(crate) enum KeyType {
    Ed25519,
    Rsa,
    EcdsaP256,
    EcdsaP384,
    EcdsaP521,
    Dsa,
    SkEd25519,
    SkEcdsaP256,
}

impl KeyType {
    const ALL: [KeyType; 8] = [
        KeyType::Ed25519,
        KeyType::Rsa,
        KeyType::EcdsaP256,
        KeyType::EcdsaP384,
        KeyType::EcdsaP521,
        KeyType::Dsa,
        KeyType::SkEd25519,
        KeyType::SkEcdsaP256,
    ];

    fn name_str(self) -> &'static str {
        match self {
            KeyType::Ed25519 => "ssh-ed25519",
            KeyType::Rsa => "ssh-rsa",
            KeyType::EcdsaP256 => "ecdsa-sha2-nistp256",
            KeyType::EcdsaP384 => "ecdsa-sha2-nistp384",
            KeyType::EcdsaP521 => "ecdsa-sha2-nistp521",
            KeyType::Dsa => "ssh-dss",
            KeyType::SkEd25519 => "sk-ssh-ed25519@openssh.com",
            KeyType::SkEcdsaP256 => "sk-ecdsa-sha2-nistp256@openssh.com",
        }
    }

    fn name(self) -> &'static [u8] {
        self.name_str().as_bytes()
    }

    fn from_name(name: &[u8]) -> Option<KeyType> {
        KeyType::ALL.into_iter().find(|t| t.name() == name)
    }

    /// The type a certificate type name certifies (`<name>-cert-v01@openssh.com`;
    /// for the `sk-` types the suffix replaces `@openssh.com`, per `PROTOCOL.u2f`).
    fn from_cert_name(name: &[u8]) -> Option<KeyType> {
        KeyType::ALL.into_iter().find(|t| {
            let base = t.name().strip_suffix(b"@openssh.com").unwrap_or(t.name());
            name.strip_suffix(CERT_SUFFIX) == Some(base)
        })
    }

    /// Whether the broker signs with keys of this type.
    fn is_signing(self) -> bool {
        matches!(
            self,
            KeyType::Ed25519
                | KeyType::Rsa
                | KeyType::EcdsaP256
                | KeyType::EcdsaP384
                | KeyType::EcdsaP521
        )
    }

    /// Whether a signature `format` is one a key of this type produces.
    fn signs_as(self, format: &[u8]) -> bool {
        match self {
            KeyType::Rsa => RSA_LOGIN_ALGORITHMS.iter().any(|(n, _)| *n == format),
            other => other.name() == format,
        }
    }
}

/// Why a key is not valid (a fixed reason, for a refusal detail).
type KeyResult<T> = Result<T, &'static str>;

fn read_string<'a>(r: &mut Reader<'a>) -> KeyResult<&'a [u8]> {
    r.string().map_err(|_| "truncated")
}

/// A non-negative mpint as OpenSSH reads it (`sshbuf_get_bignum2_bytes_direct`): the
/// sign bit clear; at most [`MAX_MPINT_BYTES`] + 1 bytes as written (room for one
/// sign-guarding zero, so a long run of leading zeros is refused as OpenSSH refuses
/// it) and at most [`MAX_MPINT_BYTES`] magnitude bytes. Returns the magnitude with
/// leading zeros removed (empty for zero).
fn read_mpint<'a>(r: &mut Reader<'a>) -> KeyResult<&'a [u8]> {
    let raw = read_string(r)?;
    if raw.first().is_some_and(|b| b & 0x80 != 0) {
        return Err("negative mpint");
    }
    if raw.len() > MAX_MPINT_BYTES + 1 {
        return Err("mpint too large");
    }
    let start = raw.iter().position(|&b| b != 0).unwrap_or(raw.len());
    let magnitude = &raw[start..];
    if magnitude.len() > MAX_MPINT_BYTES {
        return Err("mpint too large");
    }
    Ok(magnitude)
}

/// A positive mpint's magnitude (non-empty, first byte non-zero).
fn read_positive_mpint<'a>(r: &mut Reader<'a>) -> KeyResult<&'a [u8]> {
    let m = read_mpint(r)?;
    if m.is_empty() {
        return Err("zero mpint");
    }
    Ok(m)
}

/// The bit length of a magnitude whose first byte is non-zero.
fn bit_len(magnitude: &[u8]) -> usize {
    (magnitude.len() - 1) * 8 + (8 - magnitude[0].leading_zeros() as usize)
}

/// An ECDSA point for `key_type`'s curve: uncompressed, the right length, on the
/// curve.
fn check_ecdsa_point(key_type: KeyType, point: &[u8]) -> KeyResult<()> {
    let field_bytes = match key_type {
        KeyType::EcdsaP256 | KeyType::SkEcdsaP256 => 32,
        KeyType::EcdsaP384 => 48,
        KeyType::EcdsaP521 => 66,
        _ => return Err("not an ECDSA key"),
    };
    if point.len() != 1 + 2 * field_bytes || point[0] != 0x04 {
        return Err("ECDSA point is not uncompressed");
    }
    let on_curve = match key_type {
        KeyType::EcdsaP384 => p384::PublicKey::from_sec1_bytes(point).is_ok(),
        KeyType::EcdsaP521 => p521::PublicKey::from_sec1_bytes(point).is_ok(),
        _ => p256::PublicKey::from_sec1_bytes(point).is_ok(),
    };
    if !on_curve {
        return Err("ECDSA point is not on the curve");
    }
    Ok(())
}

/// Read and validate the fields of a `key_type` key that follow its type name (the
/// same fields in a plain key and in a certificate, after the nonce). See the module
/// doc's "Key validity".
fn read_key_body(r: &mut Reader<'_>, key_type: KeyType) -> KeyResult<()> {
    match key_type {
        KeyType::Ed25519 | KeyType::SkEd25519 => {
            if read_string(r)?.len() != 32 {
                return Err("Ed25519 key is not 32 bytes");
            }
        }
        KeyType::Rsa => {
            let e = read_positive_mpint(r)?;
            let n = read_positive_mpint(r)?;
            if e[e.len() - 1] & 1 == 0 {
                return Err("RSA exponent is even");
            }
            if bit_len(n) < RSA_MIN_MODULUS_BITS {
                return Err("RSA modulus is under 1024 bits");
            }
        }
        KeyType::EcdsaP256 | KeyType::EcdsaP384 | KeyType::EcdsaP521 | KeyType::SkEcdsaP256 => {
            let curve = read_string(r)?;
            let want: &[u8] = match key_type {
                KeyType::EcdsaP384 => b"nistp384",
                KeyType::EcdsaP521 => b"nistp521",
                _ => b"nistp256",
            };
            if curve != want {
                return Err("ECDSA curve does not match the key type");
            }
            check_ecdsa_point(key_type, read_string(r)?)?;
        }
        KeyType::Dsa => {
            for _ in 0..4 {
                read_positive_mpint(r)?;
            }
        }
    }
    if matches!(key_type, KeyType::SkEd25519 | KeyType::SkEcdsaP256) && read_string(r)?.contains(&0)
    {
        return Err("security-key application contains NUL");
    }
    Ok(())
}

/// Parse `blob` as a valid plain (not certificate) public key of any recognised
/// type, with nothing trailing.
fn parse_plain_key(blob: &[u8]) -> KeyResult<KeyType> {
    let mut r = Reader::new(blob);
    let key_type = KeyType::from_name(read_string(&mut r)?).ok_or("unknown key type")?;
    read_key_body(&mut r, key_type)?;
    if !r.exhausted() {
        return Err("trailing bytes");
    }
    Ok(key_type)
}

/// The key to sign with: valid, of a type the broker signs with, and decoded by
/// `ssh_key` (as the bus decodes it) to exactly these bytes.
fn signing_key_type(public_key: &[u8]) -> Result<KeyType, Refusal> {
    let mut r = Reader::new(public_key);
    let name = r
        .string()
        .map_err(|_| Refusal::malformed_key("not an SSH public key".to_string()))?;
    match KeyType::from_name(name) {
        Some(t) if t.is_signing() => {}
        _ => {
            return Err(Refusal::malformed_key(format!(
                "unsupported key type {}",
                bounded(name)
            )))
        }
    }
    let key_type = parse_plain_key(public_key)
        .map_err(|why| Refusal::malformed_key(format!("invalid key: {why}")))?;
    let decoded = PublicKey::from_bytes(public_key)
        .map_err(|_| Refusal::malformed_key("not an SSH public key".to_string()))?;
    if decoded.to_bytes().ok().as_deref() != Some(public_key) {
        return Err(Refusal::malformed_key(
            "non-canonical key encoding".to_string(),
        ));
    }
    Ok(key_type)
}

/// Whether a login request's `algorithm` field is consistent with the signing key.
fn algorithm_matches_key(key_type: KeyType, algorithm: &[u8]) -> bool {
    key_type.signs_as(algorithm)
}

/// Whether a host-bound request's server host key is a valid plain public key or a
/// valid OpenSSH certificate (module doc, "The host key").
pub(crate) fn host_key_parses(blob: &[u8]) -> bool {
    parse_plain_key(blob).is_ok() || parse_certificate(blob).is_ok()
}

/// Read a `string` that must hold a sequence of NUL-free `string`s exactly, and
/// return how many there are.
fn nul_free_strings(field: &[u8]) -> KeyResult<usize> {
    let mut r = Reader::new(field);
    let mut count = 0;
    while !r.exhausted() {
        if read_string(&mut r)?.contains(&0) {
            return Err("string contains NUL");
        }
        count += 1;
    }
    Ok(count)
}

/// A `string` that must hold a sequence of `string` name and `string` data pairs
/// exactly (critical options, extensions).
fn name_data_pairs(field: &[u8]) -> KeyResult<()> {
    let mut r = Reader::new(field);
    while !r.exhausted() {
        read_string(&mut r)?;
        read_string(&mut r)?;
    }
    Ok(())
}

/// A certificate's signature field, made by a `ca` key: exactly a format the key
/// produces and a non-empty blob, plus an `sk-` format's flags byte and counter.
fn check_signature(blob: &[u8], ca: KeyType) -> KeyResult<()> {
    let mut r = Reader::new(blob);
    let format = read_string(&mut r)?;
    if !ca.signs_as(format) {
        return Err("signature format does not fit the CA key");
    }
    if read_string(&mut r)?.is_empty() {
        return Err("empty signature");
    }
    if matches!(ca, KeyType::SkEd25519 | KeyType::SkEcdsaP256) {
        r.u8().map_err(|_| "truncated")?;
        r.u32().map_err(|_| "truncated")?;
    }
    if !r.exhausted() {
        return Err("trailing bytes in the signature");
    }
    Ok(())
}

/// Walk `blob` as an OpenSSH certificate (`PROTOCOL.certkeys`, `PROTOCOL.u2f`; module
/// doc, "The host key"). Walked by hand rather than with `ssh_key::Certificate`,
/// whose 0.6 decoder refuses a validity of "forever" (`valid before` = 2^64-1, what
/// `ssh-keygen -s` writes by default) — the recorded host certificate has one. The CA
/// signature is not verified.
fn parse_certificate(blob: &[u8]) -> KeyResult<KeyType> {
    let mut r = Reader::new(blob);
    let key_type =
        KeyType::from_cert_name(read_string(&mut r)?).ok_or("unknown certificate type")?;
    let u32_ = |r: &mut Reader<'_>| r.u32().map_err(|_| "truncated");
    let u64_ = |r: &mut Reader<'_>| r.u64().map_err(|_| "truncated");
    read_string(&mut r)?; // nonce
    read_key_body(&mut r, key_type)?;
    u64_(&mut r)?; // serial
    if !matches!(u32_(&mut r)?, 1 | 2) {
        return Err("certificate type is not user or host");
    }
    if read_string(&mut r)?.contains(&0) {
        return Err("key id contains NUL");
    }
    if nul_free_strings(read_string(&mut r)?)? > MAX_CERT_PRINCIPALS {
        return Err("too many principals");
    }
    u64_(&mut r)?; // valid after
    u64_(&mut r)?; // valid before
    name_data_pairs(read_string(&mut r)?)?; // critical options
    name_data_pairs(read_string(&mut r)?)?; // extensions
    read_string(&mut r)?; // reserved
    let ca = parse_plain_key(read_string(&mut r)?)?;
    check_signature(read_string(&mut r)?, ca)?;
    if !r.exhausted() {
        return Err("trailing bytes");
    }
    Ok(key_type)
}

// ----- the two shapes -----

/// The fields of an exact login request (crate-private so the tests' encoder can
/// rebuild it; the free fields are read only there).
#[derive(Debug)]
#[cfg_attr(not(test), allow(dead_code))]
pub(crate) struct LoginFields<'a> {
    pub(crate) session_id: &'a [u8],
    pub(crate) username: &'a [u8],
    pub(crate) method: LoginMethod,
    pub(crate) algorithm: &'a [u8],
    pub(crate) host_key: Option<&'a [u8]>,
}

/// Parse `data` as an exact login request for `public_key` (see the module doc's
/// table). Every failure is [`RefusalClass::UnparseableData`]; the username is
/// returned unjudged, and the flags are judged by the caller.
pub(crate) fn parse_login<'a>(
    data: &'a [u8],
    public_key: &[u8],
    key_type: KeyType,
) -> Result<LoginFields<'a>, Refusal> {
    const TRUNCATED: &str = "login: truncated";
    let mut r = Reader::new(data);
    let session_id = r.string().map_err(|_| Refusal::unparseable(TRUNCATED))?;
    if !SESSION_ID_LENGTHS.contains(&session_id.len()) {
        return Err(Refusal::unparseable(
            "login: session id length is not 20, 32, 48 or 64",
        ));
    }
    if r.u8().map_err(|_| Refusal::unparseable(TRUNCATED))? != SSH_MSG_USERAUTH_REQUEST {
        return Err(Refusal::unparseable(
            "login: message type is not SSH_MSG_USERAUTH_REQUEST",
        ));
    }
    let username = r.string().map_err(|_| Refusal::unparseable(TRUNCATED))?;
    if username.contains(&0) {
        return Err(Refusal::unparseable("login: username contains NUL"));
    }
    if r.string().map_err(|_| Refusal::unparseable(TRUNCATED))? != SERVICE_CONNECTION {
        return Err(Refusal::unparseable("login: service is not ssh-connection"));
    }
    let method = match r.string().map_err(|_| Refusal::unparseable(TRUNCATED))? {
        m if m == METHOD_PUBLICKEY => LoginMethod::PublicKey,
        m if m == METHOD_HOSTBOUND => LoginMethod::HostBound,
        _ => {
            return Err(Refusal::unparseable(
                "login: method is not publickey or publickey-hostbound-v00@openssh.com",
            ))
        }
    };
    if r.u8().map_err(|_| Refusal::unparseable(TRUNCATED))? != 1 {
        return Err(Refusal::unparseable("login: has-signature is not 1"));
    }
    let algorithm = r.string().map_err(|_| Refusal::unparseable(TRUNCATED))?;
    if !algorithm_matches_key(key_type, algorithm) {
        return Err(Refusal::unparseable(
            "login: algorithm does not match the key",
        ));
    }
    if r.string().map_err(|_| Refusal::unparseable(TRUNCATED))? != public_key {
        return Err(Refusal::unparseable(
            "login: embedded key differs from the requested key",
        ));
    }
    let host_key = match method {
        LoginMethod::PublicKey => None,
        LoginMethod::HostBound => {
            let blob = r.string().map_err(|_| Refusal::unparseable(TRUNCATED))?;
            if !host_key_parses(blob) {
                return Err(Refusal::unparseable(
                    "login: server host key does not parse",
                ));
            }
            Some(blob)
        }
    };
    if !r.exhausted() {
        return Err(Refusal::unparseable("login: trailing bytes"));
    }
    Ok(LoginFields {
        session_id,
        username,
        method,
        algorithm,
        host_key,
    })
}

/// The fields of an exact SSHSIG signed-data blob (the hash is read only by the
/// tests' encoder).
#[derive(Debug)]
#[cfg_attr(not(test), allow(dead_code))]
pub(crate) struct SshSigFields<'a> {
    pub(crate) namespace: &'a [u8],
    pub(crate) hash_alg: SshSigHash,
    pub(crate) hash: &'a [u8],
}

/// Parse `data` as an exact SSHSIG signed-data blob. Every failure is
/// [`RefusalClass::UnparseableData`]; the namespace is returned unjudged.
pub(crate) fn parse_sshsig(data: &[u8]) -> Result<SshSigFields<'_>, Refusal> {
    const TRUNCATED: &str = "sshsig: truncated";
    let rest = data
        .strip_prefix(SSHSIG_MAGIC)
        .ok_or_else(|| Refusal::unparseable("sshsig: no SSHSIG preamble"))?;
    let mut r = Reader::new(rest);
    let namespace = r.string().map_err(|_| Refusal::unparseable(TRUNCATED))?;
    if namespace.is_empty() {
        return Err(Refusal::unparseable("sshsig: namespace is empty"));
    }
    if namespace.contains(&0) {
        return Err(Refusal::unparseable("sshsig: namespace contains NUL"));
    }
    if !r
        .string()
        .map_err(|_| Refusal::unparseable(TRUNCATED))?
        .is_empty()
    {
        return Err(Refusal::unparseable("sshsig: reserved field is not empty"));
    }
    let (hash_alg, hash_len) = match r.string().map_err(|_| Refusal::unparseable(TRUNCATED))? {
        b"sha256" => (SshSigHash::Sha256, 32),
        b"sha512" => (SshSigHash::Sha512, 64),
        _ => {
            return Err(Refusal::unparseable(
                "sshsig: hash algorithm is not sha256 or sha512",
            ))
        }
    };
    let hash = r.string().map_err(|_| Refusal::unparseable(TRUNCATED))?;
    if hash.len() != hash_len {
        return Err(Refusal::unparseable(
            "sshsig: hash length does not match the algorithm",
        ));
    }
    if !r.exhausted() {
        return Err(Refusal::unparseable("sshsig: trailing bytes"));
    }
    Ok(SshSigFields {
        namespace,
        hash_alg,
        hash,
    })
}

/// `SHA256:<unpadded base64>` of a key blob, as `ssh-add -l` prints it.
fn fingerprint(blob: &[u8]) -> String {
    format!(
        "SHA256:{}",
        base64::engine::general_purpose::STANDARD_NO_PAD.encode(key_blob_digest(blob))
    )
}

/// A shed-supplied value for a refusal detail: its first 64 bytes, every byte
/// outside printable ASCII (and `\`, `"`, space) written as `\xNN`, and `...` when
/// cut. At most 64 * 4 + 3 bytes.
fn bounded(value: &[u8]) -> String {
    let mut out = String::new();
    for &b in value.iter().take(DETAIL_VALUE_BYTES) {
        if b.is_ascii_graphic() && b != b'\\' && b != b'"' {
            out.push(char::from(b));
        } else {
            let _ = write!(out, "\\x{b:02x}");
        }
    }
    if value.len() > DETAIL_VALUE_BYTES {
        out.push_str("...");
    }
    out
}

#[cfg(test)]
mod tests {
    //! The vectors are REAL: `fixtures/sign_check/vectors.json` was recorded from a
    //! real OpenSSH client and server (see that directory's README). The mutation,
    //! truncation and fuzz tests below judge every input they accept with a LAYOUT
    //! oracle: the two shapes written out as an encoder, which must rebuild the
    //! accepted bytes exactly. The oracle is independent of the parser only for the
    //! field layout. It shares the `Reader`, the production key decoder and the
    //! production host-key and certificate walk (`host_key_parses`), so it cannot catch
    //! a gap in those: key and certificate validity are covered by the targeted tests
    //! under "key validity" and "the host key" instead.

    use super::*;
    use crate::ssh_backend::SshKeyInfo;
    use crate::ssh_wire::put_string;
    use ssh_key::public::KeyData;
    use std::sync::Mutex;

    const VECTORS_JSON: &str = include_str!("../fixtures/sign_check/vectors.json");

    #[derive(serde::Deserialize)]
    struct Fixture {
        versions: Versions,
        host_certificate_b64: String,
        vectors: Vec<RawVector>,
    }

    #[derive(serde::Deserialize)]
    struct Versions {
        client: String,
        server: String,
    }

    #[derive(serde::Deserialize)]
    struct RawVector {
        name: String,
        shape: String,
        user_key: String,
        expect: String,
        public_key_b64: String,
        data_b64: String,
        /// The agent request's flags word, as recorded (required: every vector has
        /// one, and the check judges it).
        flags: u32,
        #[serde(default)]
        username: Option<String>,
        #[serde(default)]
        namespace: Option<String>,
    }

    #[derive(Clone)]
    struct Vector {
        name: String,
        shape: String,
        user_key: String,
        expect: Option<RefusalClass>,
        key: Vec<u8>,
        data: Vec<u8>,
        flags: u32,
        username: Option<String>,
        namespace: Option<String>,
    }

    fn b64(s: &str) -> Vec<u8> {
        base64::engine::general_purpose::STANDARD.decode(s).unwrap()
    }

    fn fixture() -> Fixture {
        serde_json::from_str(VECTORS_JSON).unwrap()
    }

    fn class_named(name: &str) -> RefusalClass {
        *RefusalClass::ALL
            .iter()
            .find(|c| c.as_str() == name)
            .unwrap_or_else(|| panic!("unknown class {name}"))
    }

    fn vectors() -> Vec<Vector> {
        fixture()
            .vectors
            .into_iter()
            .map(|v| Vector {
                expect: match v.expect.as_str() {
                    "accept" => None,
                    other => Some(class_named(other.strip_prefix("refuse:").unwrap())),
                },
                key: b64(&v.public_key_b64),
                data: b64(&v.data_b64),
                flags: v.flags,
                name: v.name,
                shape: v.shape,
                user_key: v.user_key,
                username: v.username,
                namespace: v.namespace,
            })
            .collect()
    }

    fn logins() -> Vec<Vector> {
        vectors()
            .into_iter()
            .filter(|v| v.shape == "login")
            .collect()
    }

    fn sshsigs() -> Vec<Vector> {
        vectors()
            .into_iter()
            .filter(|v| v.shape == "sshsig")
            .collect()
    }

    fn host_certificate() -> Vec<u8> {
        b64(&fixture().host_certificate_b64)
    }

    /// The default policy plus `deploy`, so the recorded `deploy` logins are accepted.
    fn git_and_deploy() -> SignPolicy {
        SignPolicy::new(["git", "deploy"], None)
    }

    /// The flags a genuine client sends with `data` for `key`: for an RSA key, the
    /// hash bits of the login's algorithm (`rsa-sha2-512` for an SSHSIG, as
    /// `ssh-keygen -Y sign` asks); 0 otherwise. Every recorded vector's flags equal
    /// this (`the_recorded_flags_are_what_a_genuine_client_sends`), so the mutation
    /// tests below, which test other rules, send the flags the payload names.
    fn genuine_flags(key: &[u8], data: &[u8]) -> u32 {
        if Reader::new(key).string() != Ok(b"ssh-rsa") {
            return 0;
        }
        if data.starts_with(b"SSHSIG") {
            return SSH_AGENT_RSA_SHA2_512;
        }
        let mut r = Reader::new(data);
        let algorithm = (|| {
            r.string().ok()?;
            r.u8().ok()?;
            for _ in 0..3 {
                r.string().ok()?;
            }
            r.u8().ok()?;
            r.string().ok()
        })();
        match algorithm {
            Some(b"rsa-sha2-256") => SSH_AGENT_RSA_SHA2_256,
            Some(b"rsa-sha2-512") => SSH_AGENT_RSA_SHA2_512,
            _ => 0,
        }
    }

    /// `None` = accepted, with the flags a genuine client sends for this payload.
    fn verdict(policy: &SignPolicy, key: &[u8], data: &[u8]) -> Option<RefusalClass> {
        verdict_flags(policy, key, data, genuine_flags(key, data))
    }

    /// `None` = accepted, under exactly `flags`.
    fn verdict_flags(
        policy: &SignPolicy,
        key: &[u8],
        data: &[u8],
        flags: u32,
    ) -> Option<RefusalClass> {
        match policy.check(key, data, flags) {
            Ok(approved) => {
                assert_eq!(approved.flags(), flags);
                oracle(policy, key, data, flags, &approved);
                None
            }
            Err(refusal) => {
                assert_detail_is_bounded(&refusal);
                Some(refusal.class())
            }
        }
    }

    // ----- the layout oracle: the two shapes, written out as encoders -----

    /// The login type names the spec allows for each recorded key type.
    fn spec_algorithms(key: &[u8]) -> Vec<&'static [u8]> {
        let mut r = Reader::new(key);
        match r.string().unwrap() {
            b"ssh-ed25519" => vec![b"ssh-ed25519"],
            b"ssh-rsa" => vec![b"ssh-rsa", b"rsa-sha2-256", b"rsa-sha2-512"],
            b"ecdsa-sha2-nistp256" => vec![b"ecdsa-sha2-nistp256"],
            b"ecdsa-sha2-nistp384" => vec![b"ecdsa-sha2-nistp384"],
            b"ecdsa-sha2-nistp521" => vec![b"ecdsa-sha2-nistp521"],
            other => panic!("oracle: unexpected key type {other:?}"),
        }
    }

    /// A login request, field by field (each field free to be set to anything).
    #[derive(Clone, Debug)]
    struct Login {
        session_id: Vec<u8>,
        msg: u8,
        username: Vec<u8>,
        service: Vec<u8>,
        method: Vec<u8>,
        has_signature: u8,
        algorithm: Vec<u8>,
        key: Vec<u8>,
        extra: Vec<Vec<u8>>,
        trailing: Vec<u8>,
    }

    impl Login {
        /// Split a recorded login vector into its fields (with the shared Reader,
        /// which has its own tests; never with `parse_login`).
        fn split(data: &[u8]) -> Login {
            let mut r = Reader::new(data);
            let session_id = r.string().unwrap().to_vec();
            let msg = r.u8().unwrap();
            let username = r.string().unwrap().to_vec();
            let service = r.string().unwrap().to_vec();
            let method = r.string().unwrap().to_vec();
            let has_signature = r.u8().unwrap();
            let algorithm = r.string().unwrap().to_vec();
            let key = r.string().unwrap().to_vec();
            let mut extra = Vec::new();
            while !r.exhausted() {
                extra.push(r.string().unwrap().to_vec());
            }
            let login = Login {
                session_id,
                msg,
                username,
                service,
                method,
                has_signature,
                algorithm,
                key,
                extra,
                trailing: Vec::new(),
            };
            assert_eq!(login.encode(), data, "split/encode round trip");
            login
        }

        fn encode(&self) -> Vec<u8> {
            let mut out = Vec::new();
            put_string(&mut out, &self.session_id);
            out.push(self.msg);
            put_string(&mut out, &self.username);
            put_string(&mut out, &self.service);
            put_string(&mut out, &self.method);
            out.push(self.has_signature);
            put_string(&mut out, &self.algorithm);
            put_string(&mut out, &self.key);
            for e in &self.extra {
                put_string(&mut out, e);
            }
            out.extend_from_slice(&self.trailing);
            out
        }

        fn is_host_bound(&self) -> bool {
            self.method == METHOD_HOSTBOUND
        }
    }

    #[derive(Clone, Debug)]
    struct Sig {
        namespace: Vec<u8>,
        reserved: Vec<u8>,
        hash_alg: Vec<u8>,
        hash: Vec<u8>,
        trailing: Vec<u8>,
    }

    impl Sig {
        fn split(data: &[u8]) -> Sig {
            assert_eq!(&data[..6], b"SSHSIG");
            let mut r = Reader::new(&data[6..]);
            let sig = Sig {
                namespace: r.string().unwrap().to_vec(),
                reserved: r.string().unwrap().to_vec(),
                hash_alg: r.string().unwrap().to_vec(),
                hash: r.string().unwrap().to_vec(),
                trailing: Vec::new(),
            };
            assert!(r.exhausted());
            assert_eq!(sig.encode(), data, "split/encode round trip");
            sig
        }

        fn encode(&self) -> Vec<u8> {
            let mut out = b"SSHSIG".to_vec();
            put_string(&mut out, &self.namespace);
            put_string(&mut out, &self.reserved);
            put_string(&mut out, &self.hash_alg);
            put_string(&mut out, &self.hash);
            out.extend_from_slice(&self.trailing);
            out
        }
    }

    /// The RSA hash bits (`flags & 6`) the spec allows for an accepted request.
    fn spec_rsa_hash_bits(kind: &SignKind, algorithm: Option<&[u8]>) -> Vec<u32> {
        match (kind, algorithm) {
            (SignKind::SshSig { .. }, _) => vec![2, 4],
            (SignKind::Login { .. }, Some(b"ssh-rsa")) => vec![0],
            (SignKind::Login { .. }, Some(b"rsa-sha2-256")) => vec![2],
            (SignKind::Login { .. }, Some(b"rsa-sha2-512")) => vec![4],
            other => panic!("oracle: no RSA flags rule for {other:?}"),
        }
    }

    /// An accepted (key, data, flags) must be one of the two shapes EXACTLY: rebuilt
    /// from the spec's constants plus only the fields the spec leaves free, it must
    /// equal the input byte for byte, and each free field must satisfy its own rule.
    /// The layout is checked independently here; the key and the host key are judged
    /// with the production decoder and walk (see the module doc).
    fn oracle(policy: &SignPolicy, key: &[u8], data: &[u8], flags: u32, approved: &Approved<'_>) {
        let parsed = PublicKey::from_bytes(key).expect("oracle: accepted key must parse");
        assert!(
            matches!(
                parsed.key_data(),
                KeyData::Ed25519(_) | KeyData::Ecdsa(_) | KeyData::Rsa(_)
            ),
            "oracle: accepted key type {:?}",
            parsed.algorithm()
        );
        assert_eq!(parsed.to_bytes().unwrap(), key, "oracle: key is canonical");
        assert_eq!(approved.key_type(), parsed.algorithm().as_str());
        if let Some(filter) = policy.offer_keys() {
            assert!(filter.contains_key_blob(key), "oracle: key is offered");
        }
        let is_rsa = matches!(parsed.key_data(), KeyData::Rsa(_));
        match approved.kind() {
            SignKind::SshSig { hash } => {
                if is_rsa {
                    assert!(
                        spec_rsa_hash_bits(approved.kind(), None).contains(&(flags & 6)),
                        "oracle: RSA SSHSIG flags {flags}"
                    );
                }
                let fields = parse_sshsig(data).unwrap();
                let (alg, len): (&[u8], usize) = match hash {
                    SshSigHash::Sha256 => (b"sha256", 32),
                    SshSigHash::Sha512 => (b"sha512", 64),
                };
                assert_eq!(fields.hash.len(), len, "oracle: hash length");
                let rebuilt = Sig {
                    namespace: b"git".to_vec(),
                    reserved: Vec::new(),
                    hash_alg: alg.to_vec(),
                    hash: fields.hash.to_vec(),
                    trailing: Vec::new(),
                };
                assert_eq!(rebuilt.encode(), data, "oracle: SSHSIG rebuilt exactly");
            }
            SignKind::Login { username, method } => {
                let key_type = KeyType::from_name(parsed.algorithm().as_str().as_bytes())
                    .expect("oracle: a signing key type");
                let fields = parse_login(data, key, key_type).unwrap();
                if is_rsa {
                    assert!(
                        spec_rsa_hash_bits(approved.kind(), Some(fields.algorithm))
                            .contains(&(flags & 6)),
                        "oracle: RSA login flags {flags}"
                    );
                }
                assert!(
                    SESSION_ID_LENGTHS.contains(&fields.session_id.len()),
                    "oracle: session id length"
                );
                assert!(
                    policy
                        .allow_users()
                        .iter()
                        .any(|u| u.as_bytes() == fields.username),
                    "oracle: username listed"
                );
                assert_eq!(username.as_bytes(), fields.username);
                assert!(!fields.username.is_empty() && fields.username[0] != b'_');
                assert!(
                    spec_algorithms(key).contains(&fields.algorithm),
                    "oracle: algorithm fits the key"
                );
                let (method_name, extra): (&[u8], Vec<Vec<u8>>) = match method {
                    LoginMethod::PublicKey => (b"publickey", Vec::new()),
                    LoginMethod::HostBound => {
                        let hk = fields.host_key.expect("host-bound has a host key");
                        // Shared with production (see the module doc).
                        assert!(host_key_parses(hk), "oracle: host key parses");
                        (b"publickey-hostbound-v00@openssh.com", vec![hk.to_vec()])
                    }
                };
                let rebuilt = Login {
                    session_id: fields.session_id.to_vec(),
                    msg: 50,
                    username: fields.username.to_vec(),
                    service: b"ssh-connection".to_vec(),
                    method: method_name.to_vec(),
                    has_signature: 1,
                    algorithm: fields.algorithm.to_vec(),
                    key: key.to_vec(),
                    extra,
                    trailing: Vec::new(),
                };
                assert_eq!(rebuilt.encode(), data, "oracle: login rebuilt exactly");
            }
        }
    }

    fn assert_detail_is_bounded(refusal: &Refusal) {
        let d = refusal.detail();
        assert!(d.len() <= MAX_DETAIL_BYTES, "detail too long: {}", d.len());
        assert!(
            d.bytes().all(|b| (0x20..=0x7e).contains(&b)),
            "detail is printable ASCII: {d:?}"
        );
    }

    // ----- the recorded vectors -----

    #[test]
    fn every_recorded_vector_gets_its_recorded_verdict() {
        let all = vectors();
        assert!(all.len() >= 16, "fixture has {} vectors", all.len());
        for v in &all {
            let got = verdict_flags(&SignPolicy::default(), &v.key, &v.data, v.flags);
            assert_eq!(got, v.expect, "{}", v.name);
            if let Ok(approved) = SignPolicy::default().check(&v.key, &v.data, v.flags) {
                match approved.kind() {
                    SignKind::Login { username, .. } => {
                        assert_eq!(Some(username), v.username.as_ref(), "{}", v.name)
                    }
                    SignKind::SshSig { .. } => assert_eq!(v.namespace.as_deref(), Some("git")),
                }
            }
        }
    }

    #[test]
    fn the_recorded_vectors_cover_both_methods_every_session_id_length_and_every_key_type() {
        let fx = fixture();
        assert!(
            fx.versions.client.starts_with("OpenSSH_"),
            "{}",
            fx.versions.client
        );
        assert!(!fx.versions.server.is_empty());
        let mut methods = Vec::new();
        let mut lengths = Vec::new();
        let mut algorithms = Vec::new();
        let mut host_key_types = Vec::new();
        for v in logins() {
            let f = parse_login(&v.data, &v.key, key_type_of(&v.key)).unwrap();
            methods.push(f.method);
            lengths.push(f.session_id.len());
            algorithms.push(f.algorithm.to_vec());
            if let Some(hk) = f.host_key {
                host_key_types.push(Reader::new(hk).string().unwrap().to_vec());
            }
        }
        // The host-bound method is what an OpenSSH client uses against an OpenSSH
        // server; at least one capture must have used it, and one the plain method.
        assert!(methods.contains(&LoginMethod::HostBound));
        assert!(methods.contains(&LoginMethod::PublicKey));
        for len in SESSION_ID_LENGTHS {
            assert!(
                lengths.contains(&len),
                "no capture with a {len}-byte session id"
            );
        }
        for alg in [
            &b"ssh-ed25519"[..],
            b"ssh-rsa",
            b"rsa-sha2-256",
            b"rsa-sha2-512",
            b"ecdsa-sha2-nistp256",
        ] {
            assert!(
                algorithms.iter().any(|a| a == alg),
                "no capture with {alg:?}"
            );
        }
        for hk in [
            &b"ssh-ed25519"[..],
            b"ssh-rsa",
            b"ecdsa-sha2-nistp256",
            b"ssh-ed25519-cert-v01@openssh.com",
        ] {
            assert!(host_key_types.iter().any(|t| t == hk), "no host key {hk:?}");
        }
        let hashes: Vec<_> = sshsigs()
            .iter()
            .filter_map(
                |v| match SignPolicy::default().check(&v.key, &v.data, v.flags) {
                    Ok(a) => Some(a.kind().clone()),
                    Err(_) => None,
                },
            )
            .collect();
        assert!(hashes.contains(&SignKind::SshSig {
            hash: SshSigHash::Sha256
        }));
        assert!(hashes.contains(&SignKind::SshSig {
            hash: SshSigHash::Sha512
        }));
        let key_types: Vec<_> = vectors().iter().map(|v| v.user_key.clone()).collect();
        for t in ["ed25519", "rsa", "ecdsa"] {
            assert!(key_types.iter().any(|k| k == t), "no {t} key");
        }
    }

    /// The key type of a recorded (valid) key.
    fn key_type_of(key: &[u8]) -> KeyType {
        signing_key_type(key).unwrap()
    }

    #[test]
    fn the_recorded_flags_are_what_a_genuine_client_sends() {
        let all = vectors();
        for v in &all {
            assert_eq!(genuine_flags(&v.key, &v.data), v.flags, "{}", v.name);
        }
        // The captures exercise every RSA hash bit and a zero word.
        for flags in [0, SSH_AGENT_RSA_SHA2_256, SSH_AGENT_RSA_SHA2_512] {
            assert!(
                all.iter().any(|v| v.user_key == "rsa" && v.flags == flags),
                "no RSA capture with flags {flags}"
            );
        }
    }

    #[test]
    fn a_recorded_refusal_for_a_username_is_only_the_username() {
        let refused: Vec<_> = logins()
            .into_iter()
            .filter(|v| v.expect.is_some())
            .collect();
        assert!(
            refused.len() >= 2,
            "the fixture's username refusals are missing"
        );
        for v in &refused {
            assert_eq!(
                verdict_flags(&git_and_deploy(), &v.key, &v.data, v.flags),
                None,
                "{}",
                v.name
            );
        }
    }

    // ----- one mutation per rule -----

    type Mutation = (&'static str, fn(&mut Login), Option<RefusalClass>);

    const UNPARSEABLE: Option<RefusalClass> = Some(RefusalClass::UnparseableData);
    const NOT_ALLOWED: Option<RefusalClass> = Some(RefusalClass::UsernameNotAllowed);
    const RESERVED: Option<RefusalClass> = Some(RefusalClass::ReservedUsername);

    fn set<T: Clone>(field: &mut T, value: T) {
        *field = value;
    }

    /// Mutations that apply to every login vector.
    const LOGIN_MUTATIONS: &[Mutation] = &[
        ("unmutated", |_| {}, None),
        ("message type 49", |l| l.msg = 49, UNPARSEABLE),
        ("message type 51", |l| l.msg = 51, UNPARSEABLE),
        ("message type 0", |l| l.msg = 0, UNPARSEABLE),
        (
            "service ssh-userauth",
            |l| set(&mut l.service, b"ssh-userauth".to_vec()),
            UNPARSEABLE,
        ),
        (
            "service with NUL",
            |l| set(&mut l.service, b"ssh-connection\0".to_vec()),
            UNPARSEABLE,
        ),
        (
            "service cut",
            |l| set(&mut l.service, b"ssh-connectio".to_vec()),
            UNPARSEABLE,
        ),
        (
            "service upper",
            |l| set(&mut l.service, b"SSH-CONNECTION".to_vec()),
            UNPARSEABLE,
        ),
        (
            "service empty",
            |l| set(&mut l.service, Vec::new()),
            UNPARSEABLE,
        ),
        (
            "method password",
            |l| set(&mut l.method, b"password".to_vec()),
            UNPARSEABLE,
        ),
        (
            "method extension name",
            |l| set(&mut l.method, b"publickey-hostbound@openssh.com".to_vec()),
            UNPARSEABLE,
        ),
        (
            "method v01",
            |l| {
                set(
                    &mut l.method,
                    b"publickey-hostbound-v01@openssh.com".to_vec(),
                )
            },
            UNPARSEABLE,
        ),
        (
            "method with NUL",
            |l| set(&mut l.method, b"publickey\0".to_vec()),
            UNPARSEABLE,
        ),
        (
            "method upper",
            |l| set(&mut l.method, b"PUBLICKEY".to_vec()),
            UNPARSEABLE,
        ),
        (
            "method empty",
            |l| set(&mut l.method, Vec::new()),
            UNPARSEABLE,
        ),
        ("has-signature 0", |l| l.has_signature = 0, UNPARSEABLE),
        ("has-signature 2", |l| l.has_signature = 2, UNPARSEABLE),
        ("has-signature 255", |l| l.has_signature = 255, UNPARSEABLE),
        ("algorithm with NUL", |l| l.algorithm.push(0), UNPARSEABLE),
        (
            "algorithm empty",
            |l| set(&mut l.algorithm, Vec::new()),
            UNPARSEABLE,
        ),
        (
            "algorithm certificate",
            |l| {
                set(
                    &mut l.algorithm,
                    b"ssh-ed25519-cert-v01@openssh.com".to_vec(),
                )
            },
            UNPARSEABLE,
        ),
        (
            "algorithm sk",
            |l| set(&mut l.algorithm, b"sk-ssh-ed25519@openssh.com".to_vec()),
            UNPARSEABLE,
        ),
        (
            "algorithm rsa-sha2-384",
            |l| set(&mut l.algorithm, b"rsa-sha2-384".to_vec()),
            UNPARSEABLE,
        ),
        (
            "embedded key last byte",
            |l| *l.key.last_mut().unwrap() ^= 1,
            UNPARSEABLE,
        ),
        ("embedded key first byte", |l| l.key[0] ^= 1, UNPARSEABLE),
        ("embedded key + byte", |l| l.key.push(0), UNPARSEABLE),
        (
            "embedded key - byte",
            |l| {
                l.key.pop();
            },
            UNPARSEABLE,
        ),
        ("embedded key empty", |l| l.key.clear(), UNPARSEABLE),
        ("trailing NUL byte", |l| l.trailing.push(0), UNPARSEABLE),
        ("trailing 0xff byte", |l| l.trailing.push(0xff), UNPARSEABLE),
        (
            "trailing empty string",
            |l| l.extra.push(Vec::new()),
            UNPARSEABLE,
        ),
        (
            "username _bootstrap",
            |l| set(&mut l.username, b"_bootstrap".to_vec()),
            RESERVED,
        ),
        (
            "username _api",
            |l| set(&mut l.username, b"_api".to_vec()),
            RESERVED,
        ),
        (
            "username _x",
            |l| set(&mut l.username, b"_x".to_vec()),
            RESERVED,
        ),
        (
            "username _",
            |l| set(&mut l.username, b"_".to_vec()),
            RESERVED,
        ),
        (
            "username not listed",
            |l| set(&mut l.username, b"root".to_vec()),
            NOT_ALLOWED,
        ),
        (
            "username upper",
            |l| set(&mut l.username, b"GIT".to_vec()),
            NOT_ALLOWED,
        ),
        (
            "username trailing space",
            |l| set(&mut l.username, b"git ".to_vec()),
            NOT_ALLOWED,
        ),
        (
            "username leading space",
            |l| set(&mut l.username, b" git".to_vec()),
            NOT_ALLOWED,
        ),
        (
            "username prefix",
            |l| set(&mut l.username, b"gi".to_vec()),
            NOT_ALLOWED,
        ),
        (
            "username extended",
            |l| set(&mut l.username, b"gitx".to_vec()),
            NOT_ALLOWED,
        ),
        (
            "username empty",
            |l| set(&mut l.username, Vec::new()),
            NOT_ALLOWED,
        ),
        (
            "username NUL after",
            |l| set(&mut l.username, b"git\0".to_vec()),
            UNPARSEABLE,
        ),
        (
            "username NUL before",
            |l| set(&mut l.username, b"\0git".to_vec()),
            UNPARSEABLE,
        ),
        (
            "username NUL inside",
            |l| set(&mut l.username, b"g\0it".to_vec()),
            UNPARSEABLE,
        ),
        (
            "username git",
            |l| set(&mut l.username, b"git".to_vec()),
            None,
        ),
    ];

    /// Mutations for host-bound vectors (each carries a host key).
    const HOSTBOUND_MUTATIONS: &[Mutation] = &[
        ("no host key", |l| l.extra.clear(), UNPARSEABLE),
        (
            "two host keys",
            |l| l.extra.push(l.extra[0].clone()),
            UNPARSEABLE,
        ),
        (
            "plain method keeping the host key",
            |l| set(&mut l.method, b"publickey".to_vec()),
            UNPARSEABLE,
        ),
        (
            "host key empty",
            |l| set(&mut l.extra[0], Vec::new()),
            UNPARSEABLE,
        ),
        (
            "host key garbage",
            |l| set(&mut l.extra[0], b"not a key".to_vec()),
            UNPARSEABLE,
        ),
        ("host key + byte", |l| l.extra[0].push(0), UNPARSEABLE),
        (
            "host key - byte",
            |l| {
                l.extra[0].pop();
            },
            UNPARSEABLE,
        ),
        (
            "host key type NUL",
            |l| {
                let mut blob = Vec::new();
                put_string(&mut blob, b"ssh-ed25519\0");
                put_string(&mut blob, &[7; 32]);
                l.extra[0] = blob;
            },
            UNPARSEABLE,
        ),
        (
            "host key ed25519 31 bytes",
            |l| {
                let mut blob = Vec::new();
                put_string(&mut blob, b"ssh-ed25519");
                put_string(&mut blob, &[7; 31]);
                l.extra[0] = blob;
            },
            UNPARSEABLE,
        ),
        (
            "host key unknown algorithm (opaque)",
            |l| {
                // ssh_key reads this as KeyData::Other: it must not count as a parse.
                let mut blob = Vec::new();
                put_string(&mut blob, b"foo@example.com");
                put_string(&mut blob, b"anything");
                l.extra[0] = blob;
            },
            UNPARSEABLE,
        ),
        (
            "host key ed25519 any 32 bytes",
            |l| {
                let mut blob = Vec::new();
                put_string(&mut blob, b"ssh-ed25519");
                put_string(&mut blob, &[7; 32]);
                l.extra[0] = blob;
            },
            None,
        ),
    ];

    /// Mutations for plain `publickey` vectors (no host key).
    const PUBLICKEY_MUTATIONS: &[Mutation] = &[
        (
            "an extra valid host key",
            |l| {
                let mut blob = Vec::new();
                put_string(&mut blob, b"ssh-ed25519");
                put_string(&mut blob, &[7; 32]);
                l.extra.push(blob);
            },
            UNPARSEABLE,
        ),
        (
            "host-bound method without a host key",
            |l| set(&mut l.method, METHOD_HOSTBOUND.to_vec()),
            UNPARSEABLE,
        ),
    ];

    #[test]
    fn one_mutation_per_login_rule_is_refused() {
        let policy = git_and_deploy();
        let mut ran = 0;
        for v in logins() {
            let base = Login::split(&v.data);
            let mut table: Vec<&Mutation> = LOGIN_MUTATIONS.iter().collect();
            if base.is_host_bound() {
                table.extend(HOSTBOUND_MUTATIONS);
            } else {
                table.extend(PUBLICKEY_MUTATIONS);
            }
            for (label, mutate, want) in table {
                let mut l = base.clone();
                mutate(&mut l);
                let got = verdict(&policy, &v.key, &l.encode());
                assert_eq!(got, *want, "{}: {label}", v.name);
                ran += 1;
            }
        }
        assert!(ran > 400, "ran {ran}");
    }

    #[test]
    fn a_listed_underscore_name_is_still_reserved() {
        let policy = SignPolicy::new(["git", "_bootstrap", "_api", "_x", "_"], None);
        let v = &logins()[0];
        for name in ["_bootstrap", "_api", "_x", "_"] {
            let mut l = Login::split(&v.data);
            l.username = name.as_bytes().to_vec();
            assert_eq!(verdict(&policy, &v.key, &l.encode()), RESERVED, "{name}");
        }
        // And a listed NUL-bearing name is still refused as unparseable.
        let policy = SignPolicy::new(["git\0"], None);
        let mut l = Login::split(&v.data);
        l.username = b"git\0".to_vec();
        assert_eq!(verdict(&policy, &v.key, &l.encode()), UNPARSEABLE);
    }

    #[test]
    fn the_algorithm_must_fit_the_key() {
        let candidates: [&[u8]; 9] = [
            b"ssh-ed25519",
            b"ssh-rsa",
            b"rsa-sha2-256",
            b"rsa-sha2-512",
            b"ecdsa-sha2-nistp256",
            b"ecdsa-sha2-nistp384",
            b"ecdsa-sha2-nistp521",
            b"ssh-dss",
            b"ssh-rsa-cert-v01@openssh.com",
        ];
        let mut accepted = 0;
        for v in logins() {
            for alg in candidates {
                let mut l = Login::split(&v.data);
                l.algorithm = alg.to_vec();
                let want = if spec_algorithms(&v.key).contains(&alg) {
                    accepted += 1;
                    None
                } else {
                    UNPARSEABLE
                };
                assert_eq!(
                    verdict(&git_and_deploy(), &v.key, &l.encode()),
                    want,
                    "{} {alg:?}",
                    v.name
                );
            }
        }
        assert!(accepted > 0);
    }

    #[test]
    fn session_id_lengths_around_each_allowed_value() {
        for v in logins() {
            for len in 0..=129usize {
                let mut l = Login::split(&v.data);
                l.session_id = vec![0xa5; len];
                let want = if SESSION_ID_LENGTHS.contains(&len) {
                    None
                } else {
                    UNPARSEABLE
                };
                assert_eq!(
                    verdict(&git_and_deploy(), &v.key, &l.encode()),
                    want,
                    "{} len {len}",
                    v.name
                );
            }
        }
    }

    #[test]
    fn a_certificate_presenting_server_and_other_host_key_types_are_accepted() {
        let cert = host_certificate();
        let host_keys: Vec<Vec<u8>> = logins()
            .iter()
            .filter_map(|v| Login::split(&v.data).extra.first().cloned())
            .chain([cert])
            .collect();
        for v in logins()
            .iter()
            .filter(|v| Login::split(&v.data).is_host_bound())
        {
            for hk in &host_keys {
                let mut l = Login::split(&v.data);
                l.extra = vec![hk.clone()];
                assert_eq!(
                    verdict(&git_and_deploy(), &v.key, &l.encode()),
                    None,
                    "{}",
                    v.name
                );
            }
            // A certificate with its type name damaged no longer parses.
            let mut l = Login::split(&v.data);
            let mut bad = host_certificate();
            bad[10] ^= 0x20;
            l.extra = vec![bad];
            assert_eq!(verdict(&git_and_deploy(), &v.key, &l.encode()), UNPARSEABLE);
            let mut l = Login::split(&v.data);
            let mut cut = host_certificate();
            cut.pop();
            l.extra = vec![cut];
            assert_eq!(verdict(&git_and_deploy(), &v.key, &l.encode()), UNPARSEABLE);
        }
    }

    type SigMutation = (&'static str, fn(&mut Sig), Option<RefusalClass>);
    const WRONG_NS: Option<RefusalClass> = Some(RefusalClass::WrongNamespace);

    const SSHSIG_MUTATIONS: &[SigMutation] = &[
        ("unmutated", |_| {}, None),
        (
            "namespace file",
            |s| set(&mut s.namespace, b"file".to_vec()),
            WRONG_NS,
        ),
        (
            "namespace upper",
            |s| set(&mut s.namespace, b"GIT".to_vec()),
            WRONG_NS,
        ),
        (
            "namespace prefix",
            |s| set(&mut s.namespace, b"gi".to_vec()),
            WRONG_NS,
        ),
        (
            "namespace extended",
            |s| set(&mut s.namespace, b"gitx".to_vec()),
            WRONG_NS,
        ),
        (
            "namespace trailing space",
            |s| set(&mut s.namespace, b"git ".to_vec()),
            WRONG_NS,
        ),
        (
            "namespace empty",
            |s| set(&mut s.namespace, Vec::new()),
            UNPARSEABLE,
        ),
        (
            "namespace NUL after",
            |s| set(&mut s.namespace, b"git\0".to_vec()),
            UNPARSEABLE,
        ),
        (
            "namespace NUL inside",
            |s| set(&mut s.namespace, b"g\0it".to_vec()),
            UNPARSEABLE,
        ),
        (
            "reserved NUL",
            |s| set(&mut s.reserved, vec![0]),
            UNPARSEABLE,
        ),
        (
            "reserved x",
            |s| set(&mut s.reserved, b"x".to_vec()),
            UNPARSEABLE,
        ),
        (
            "hash sha1",
            |s| set(&mut s.hash_alg, b"sha1".to_vec()),
            UNPARSEABLE,
        ),
        (
            "hash sha384",
            |s| set(&mut s.hash_alg, b"sha384".to_vec()),
            UNPARSEABLE,
        ),
        (
            "hash upper",
            |s| set(&mut s.hash_alg, b"SHA512".to_vec()),
            UNPARSEABLE,
        ),
        ("hash NUL", |s| s.hash_alg.push(0), UNPARSEABLE),
        (
            "hash name empty",
            |s| set(&mut s.hash_alg, Vec::new()),
            UNPARSEABLE,
        ),
        ("hash value empty", |s| s.hash.clear(), UNPARSEABLE),
        ("hash value + byte", |s| s.hash.push(0), UNPARSEABLE),
        (
            "hash value - byte",
            |s| {
                s.hash.pop();
            },
            UNPARSEABLE,
        ),
        (
            "hash sha256 with 64 bytes",
            |s| {
                s.hash_alg = b"sha256".to_vec();
                s.hash = vec![1; 64];
            },
            UNPARSEABLE,
        ),
        (
            "hash sha512 with 32 bytes",
            |s| {
                s.hash_alg = b"sha512".to_vec();
                s.hash = vec![1; 32];
            },
            UNPARSEABLE,
        ),
        (
            "hash sha256 with 32 bytes",
            |s| {
                s.hash_alg = b"sha256".to_vec();
                s.hash = vec![1; 32];
            },
            None,
        ),
        (
            "hash sha512 with 64 bytes",
            |s| {
                s.hash_alg = b"sha512".to_vec();
                s.hash = vec![1; 64];
            },
            None,
        ),
        ("trailing NUL byte", |s| s.trailing.push(0), UNPARSEABLE),
        (
            "trailing empty string",
            |s| s.trailing.extend_from_slice(&[0; 4]),
            UNPARSEABLE,
        ),
    ];

    #[test]
    fn one_mutation_per_sshsig_rule_is_refused() {
        let mut ran = 0;
        for v in sshsigs() {
            let mut base = Sig::split(&v.data);
            base.namespace = b"git".to_vec(); // the recorded `file` vector too
            for (label, mutate, want) in SSHSIG_MUTATIONS {
                let mut s = base.clone();
                mutate(&mut s);
                assert_eq!(
                    verdict(&git_and_deploy(), &v.key, &s.encode()),
                    *want,
                    "{}: {label}",
                    v.name
                );
                ran += 1;
            }
            // A damaged preamble is not an SSHSIG and not a login either.
            for (i, flip) in [(0usize, b'X'), (5, b'g')] {
                let mut data = v.data.clone();
                data[i] = flip;
                assert_eq!(
                    verdict(&git_and_deploy(), &v.key, &data),
                    UNPARSEABLE,
                    "{}",
                    v.name
                );
            }
        }
        assert!(ran >= 100, "ran {ran}");
    }

    #[test]
    fn data_beginning_with_sshsig_is_never_judged_as_a_login() {
        // A valid login, behind the six SSHSIG bytes, is refused by the SSHSIG rules.
        for v in logins() {
            let mut data = b"SSHSIG".to_vec();
            data.extend_from_slice(&v.data);
            let r = SignPolicy::default()
                .check(&v.key, &data, v.flags)
                .unwrap_err();
            assert_eq!(r.class(), RefusalClass::UnparseableData);
            assert!(r.detail().starts_with("sshsig:"), "{}", r.detail());
        }
    }

    // ----- truncation, declared lengths -----

    #[test]
    fn every_truncation_of_every_vector_is_refused() {
        for v in vectors() {
            for n in 0..v.data.len() {
                assert_eq!(
                    verdict(&git_and_deploy(), &v.key, &v.data[..n]),
                    UNPARSEABLE,
                    "{} cut at {n}",
                    v.name
                );
            }
        }
    }

    /// The offsets of every top-level `string` length word in a recorded vector.
    fn length_word_offsets(v: &Vector) -> Vec<usize> {
        let d = &v.data;
        let mut offsets = Vec::new();
        let mut pos = if v.shape == "sshsig" { 6 } else { 0 };
        let mut string = |pos: &mut usize| {
            offsets.push(*pos);
            let n = u32::from_be_bytes(d[*pos..*pos + 4].try_into().unwrap()) as usize;
            *pos += 4 + n;
        };
        if v.shape == "sshsig" {
            for _ in 0..4 {
                string(&mut pos);
            }
        } else {
            string(&mut pos); // session id
            pos += 1; // message type
            for _ in 0..3 {
                string(&mut pos); // username, service, method
            }
            pos += 1; // has-signature
            while pos < d.len() {
                string(&mut pos); // algorithm, key, host key
            }
        }
        assert_eq!(pos, d.len());
        offsets
    }

    #[test]
    fn a_declared_length_past_the_remainder_or_u32_max_is_refused() {
        let mut ran = 0;
        for v in vectors() {
            for off in length_word_offsets(&v) {
                let remainder = (v.data.len() - off - 4) as u32;
                for declared in [remainder + 1, remainder + 1000, 0x8000_0000, u32::MAX] {
                    let mut data = v.data.clone();
                    data[off..off + 4].copy_from_slice(&declared.to_be_bytes());
                    assert_eq!(
                        verdict(&git_and_deploy(), &v.key, &data),
                        UNPARSEABLE,
                        "{} word at {off} = {declared}",
                        v.name
                    );
                    ran += 1;
                }
            }
        }
        assert!(ran > 200, "ran {ran}");
    }

    // ----- the key -----

    fn ed25519_blob(seed: u8) -> Vec<u8> {
        let mut b = Vec::new();
        put_string(&mut b, b"ssh-ed25519");
        put_string(&mut b, &[seed; 32]);
        b
    }

    #[test]
    fn keys_the_broker_does_not_sign_with_are_malformed() {
        let login = logins()
            .into_iter()
            .find(|v| v.user_key == "ed25519")
            .unwrap();
        let rsa = logins().into_iter().find(|v| v.user_key == "rsa").unwrap();
        let mut sk_ed25519 = Vec::new();
        put_string(&mut sk_ed25519, b"sk-ssh-ed25519@openssh.com");
        put_string(&mut sk_ed25519, &[7; 32]);
        put_string(&mut sk_ed25519, b"ssh:");
        let mut opaque = Vec::new();
        put_string(&mut opaque, b"foo@example.com");
        put_string(&mut opaque, b"anything");
        let mut dss = Vec::new();
        put_string(&mut dss, b"ssh-dss");
        for part in [&[0x7f; 128][..], &[0x7f; 20], &[0x7f; 128], &[0x7f; 128]] {
            put_string(&mut dss, part);
        }
        // RSA with a non-minimal mpint (an extra leading zero in `e`).
        let mut r = Reader::new(&rsa.key);
        let (name, e, n) = (
            r.string().unwrap(),
            r.string().unwrap(),
            r.string().unwrap(),
        );
        let mut padded_e = vec![0u8];
        padded_e.extend_from_slice(e);
        let mut rsa_padded = Vec::new();
        put_string(&mut rsa_padded, name);
        put_string(&mut rsa_padded, &padded_e);
        put_string(&mut rsa_padded, n);
        let mut key_plus = login.key.clone();
        key_plus.push(0);
        let cases: Vec<(&str, Vec<u8>)> = vec![
            ("empty", Vec::new()),
            ("garbage", b"not a key".to_vec()),
            ("truncated", login.key[..login.key.len() - 1].to_vec()),
            ("trailing byte", key_plus),
            ("ed25519 31 bytes", {
                let mut b = Vec::new();
                put_string(&mut b, b"ssh-ed25519");
                put_string(&mut b, &[7; 31]);
                b
            }),
            ("certificate", host_certificate()),
            ("sk-ed25519", sk_ed25519),
            ("unknown algorithm", opaque),
            ("dsa", dss),
            ("rsa non-canonical", rsa_padded),
            ("u32::MAX type length", u32::MAX.to_be_bytes().to_vec()),
        ];
        for (label, key) in cases {
            // With the matching data (the embedded key swapped in) and with garbage.
            let mut l = Login::split(&login.data);
            l.key = key.clone();
            for data in [l.encode(), b"garbage".to_vec()] {
                let filter = Some(KeyFilter::new());
                for policy in [SignPolicy::default(), SignPolicy::new(["git"], filter)] {
                    assert_eq!(
                        verdict(&policy, &key, &data),
                        Some(RefusalClass::MalformedKey),
                        "{label}"
                    );
                }
            }
        }
    }

    // ----- the key filter -----

    #[test]
    fn the_key_filter_is_checked_after_the_key_and_before_the_data() {
        let all = vectors();
        let mut offered = KeyFilter::new();
        for v in &all {
            offered.insert_key_blob(&v.key);
        }
        let with_all = SignPolicy::new(["git"], Some(offered));
        let other_only = SignPolicy::new(
            ["git"],
            Some(KeyFilter::from_digests([key_blob_digest(&ed25519_blob(9))])),
        );
        let empty = SignPolicy::new(["git"], Some(KeyFilter::new()));
        for v in &all {
            assert_eq!(verdict(&with_all, &v.key, &v.data), v.expect, "{}", v.name);
            for policy in [&other_only, &empty] {
                assert_eq!(
                    verdict(policy, &v.key, &v.data),
                    Some(RefusalClass::KeyNotOffered),
                    "{}",
                    v.name
                );
                // Before the data: garbage data is still "not offered".
                assert_eq!(
                    verdict(policy, &v.key, b"garbage"),
                    Some(RefusalClass::KeyNotOffered)
                );
            }
            // The detail names the key by the fingerprint OpenSSH prints for it.
            let r = empty.check(&v.key, &v.data, v.flags).unwrap_err();
            let fp = PublicKey::from_bytes(&v.key)
                .unwrap()
                .fingerprint(ssh_key::HashAlg::Sha256);
            assert_eq!(r.detail(), format!("key {fp}"));
        }
    }

    // ----- the invariant: the checked bytes are the signed bytes -----

    /// Where and how long a buffer handed to the backend was: its address and length.
    type BufferAt = (usize, usize);

    #[derive(Default)]
    struct Recorder {
        /// Per `sign` call: the key buffer, the data buffer and the flags.
        calls: Mutex<Vec<(BufferAt, BufferAt, u32)>>,
    }

    impl SshBackend for Recorder {
        fn list(&self) -> Result<Vec<SshKeyInfo>, String> {
            Ok(Vec::new())
        }
        fn sign(&self, public_key: &[u8], data: &[u8], flags: u32) -> Result<SshSignature, String> {
            self.calls.lock().unwrap().push((
                (public_key.as_ptr() as usize, public_key.len()),
                (data.as_ptr() as usize, data.len()),
                flags,
            ));
            Ok(SshSignature {
                format: "test".into(),
                blob: Vec::new(),
            })
        }
        fn mode(&self) -> &str {
            "recorder"
        }
    }

    #[test]
    fn the_backend_signs_exactly_the_buffers_and_flags_that_were_checked() {
        let accepted: Vec<_> = vectors()
            .into_iter()
            .filter(|v| v.expect.is_none())
            .collect();
        assert!(
            accepted.len() >= 10,
            "the fixture's accepted vectors are missing"
        );
        for v in &accepted {
            let backend = Recorder::default();
            let approved = SignPolicy::default()
                .check(&v.key, &v.data, v.flags)
                .unwrap();
            let key_type = approved.key_type().to_string();
            approved.sign(&backend).unwrap();
            let calls = backend.calls.lock().unwrap();
            assert_eq!(
                *calls,
                vec![(
                    (v.key.as_ptr() as usize, v.key.len()),
                    (v.data.as_ptr() as usize, v.data.len()),
                    v.flags
                )],
                "{}",
                v.name
            );
            let want_type = match v.user_key.as_str() {
                "ed25519" => "ssh-ed25519",
                "rsa" => "ssh-rsa",
                _ => "ecdsa-sha2-nistp256",
            };
            assert_eq!(key_type, want_type);
        }
    }

    // ----- the refusal record -----

    #[test]
    fn refusal_details_are_bounded_escaped_and_never_the_request() {
        let v = logins()
            .into_iter()
            .find(|v| v.user_key == "ed25519")
            .unwrap();
        let mut long = Login::split(&v.data);
        long.username = vec![b'a'; 10_000];
        let r = SignPolicy::default()
            .check(&v.key, &long.encode(), v.flags)
            .unwrap_err();
        assert_eq!(r.class(), RefusalClass::UsernameNotAllowed);
        assert_eq!(r.detail(), format!("login user={}...", "a".repeat(64)));
        assert_detail_is_bounded(&r);

        let mut odd = Login::split(&v.data);
        odd.username = b"_\x01\x80\"\\ x".to_vec();
        let r = SignPolicy::default()
            .check(&v.key, &odd.encode(), v.flags)
            .unwrap_err();
        assert_eq!(r.class(), RefusalClass::ReservedUsername);
        assert_eq!(r.detail(), r#"login user=_\x01\x80\x22\x5c\x20x"#);

        let s = sshsigs().into_iter().next().unwrap();
        let mut sig = Sig::split(&s.data);
        sig.namespace = vec![0xff; 300];
        let r = SignPolicy::default()
            .check(&s.key, &sig.encode(), s.flags)
            .unwrap_err();
        assert_eq!(r.class(), RefusalClass::WrongNamespace);
        assert_eq!(
            r.detail(),
            format!("sshsig namespace={}...", "\\xff".repeat(64))
        );
        assert_detail_is_bounded(&r);

        // A shape refusal names the rule, never the bytes.
        let mut l = Login::split(&v.data);
        l.service = b"ssh-userauth".to_vec();
        let r = SignPolicy::default()
            .check(&v.key, &l.encode(), v.flags)
            .unwrap_err();
        assert_eq!(
            r.to_string(),
            "unparseable_data: login: service is not ssh-connection"
        );
    }

    #[test]
    fn refusal_classes_have_distinct_stable_names() {
        let names: Vec<_> = RefusalClass::ALL.iter().map(|c| c.as_str()).collect();
        assert_eq!(
            names,
            [
                "malformed_request",
                "malformed_key",
                "key_not_offered",
                "username_not_allowed",
                "reserved_username",
                "wrong_namespace",
                "unparseable_data",
            ]
        );
        let r = Refusal::malformed_request("invalid public key encoding");
        assert_eq!(r.class(), RefusalClass::MalformedRequest);
        assert_eq!(r.detail(), "invalid\\x20public\\x20key\\x20encoding");
    }

    #[test]
    fn the_default_policy_is_git_with_no_filter() {
        let p = SignPolicy::default();
        assert_eq!(p.allow_users(), ["git".to_string()]);
        assert!(p.offer_keys().is_none());
    }

    // ----- key validity -----

    /// An `mpint` with `magnitude` as its value (a leading zero added when the top bit
    /// is set, so it stays positive and canonical).
    fn mpint(magnitude: &[u8]) -> Vec<u8> {
        let mut v = Vec::new();
        if magnitude.first().is_some_and(|b| b & 0x80 != 0) {
            v.push(0);
        }
        v.extend_from_slice(magnitude);
        v
    }

    /// An `ssh-rsa` blob from raw `e` and `n` mpint bytes (written as given).
    fn rsa_blob_raw(e: &[u8], n: &[u8]) -> Vec<u8> {
        let mut b = Vec::new();
        put_string(&mut b, b"ssh-rsa");
        put_string(&mut b, e);
        put_string(&mut b, n);
        b
    }

    /// The three strings of an `ssh-rsa` blob: name, `e`, `n`.
    fn rsa_parts(blob: &[u8]) -> Vec<Vec<u8>> {
        let mut r = Reader::new(blob);
        (0..3).map(|_| r.string().unwrap().to_vec()).collect()
    }

    /// An `ssh-rsa` blob with exponent 65537 and a `bits`-bit odd modulus.
    fn rsa_blob(bits: usize) -> Vec<u8> {
        let mut n = vec![0xa5u8; bits.div_ceil(8)];
        let top = bits - (n.len() - 1) * 8; // bits used in the first byte, 1..=8
        let high = 1u8 << (top - 1);
        n[0] = high | (0xa5 & (high - 1));
        *n.last_mut().unwrap() |= 1;
        rsa_blob_raw(&mpint(&[1, 0, 1]), &mpint(&n))
    }

    /// A valid uncompressed point on `curve` (from a fixed scalar).
    fn ec_point(curve: &str) -> Vec<u8> {
        use p256::elliptic_curve::sec1::ToEncodedPoint;
        fn scalar(len: usize) -> Vec<u8> {
            let mut s = vec![7u8; len];
            s[0] = 0;
            s
        }
        match curve {
            "nistp256" => p256::SecretKey::from_slice(&scalar(32))
                .unwrap()
                .public_key()
                .to_encoded_point(false)
                .as_bytes()
                .to_vec(),
            "nistp384" => p384::SecretKey::from_slice(&scalar(48))
                .unwrap()
                .public_key()
                .to_encoded_point(false)
                .as_bytes()
                .to_vec(),
            "nistp521" => p521::SecretKey::from_slice(&scalar(66))
                .unwrap()
                .public_key()
                .to_encoded_point(false)
                .as_bytes()
                .to_vec(),
            other => panic!("no curve {other}"),
        }
    }

    fn ecdsa_blob_raw(name: &str, curve: &str, point: &[u8]) -> Vec<u8> {
        let mut b = Vec::new();
        put_string(&mut b, name.as_bytes());
        put_string(&mut b, curve.as_bytes());
        put_string(&mut b, point);
        b
    }

    fn ecdsa_blob(curve: &str) -> Vec<u8> {
        ecdsa_blob_raw(&format!("ecdsa-sha2-{curve}"), curve, &ec_point(curve))
    }

    fn sk_ed25519_blob(application: &[u8]) -> Vec<u8> {
        let mut b = Vec::new();
        put_string(&mut b, b"sk-ssh-ed25519@openssh.com");
        put_string(&mut b, &[7; 32]);
        put_string(&mut b, application);
        b
    }

    fn sk_ecdsa_blob(point: &[u8], application: &[u8]) -> Vec<u8> {
        let mut b = Vec::new();
        put_string(&mut b, b"sk-ecdsa-sha2-nistp256@openssh.com");
        put_string(&mut b, b"nistp256");
        put_string(&mut b, point);
        put_string(&mut b, application);
        b
    }

    /// The reviewer's off-curve P-256 key: the point (0, 0).
    fn off_curve_p256() -> Vec<u8> {
        let mut point = vec![0x04];
        point.extend_from_slice(&[0; 64]);
        ecdsa_blob_raw("ecdsa-sha2-nistp256", "nistp256", &point)
    }

    /// The reviewer's RSA key: modulus 1, exponent -1.
    fn rsa_modulus_one() -> Vec<u8> {
        rsa_blob_raw(&[0xff], &[0x01])
    }

    /// A valid security-key host certificate (`sk-ssh-ed25519-cert-v01@openssh.com`)
    /// that OpenSSH accepts, built by the reviewer.
    const SK_HOST_CERT_B64: &str = "AAAAI3NrLXNzaC1lZDI1NTE5LWNlcnQtdjAxQG9wZW5zc2guY29tAAAAIAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAIKnlDVkhIYPQ5JfJNdIGBzTIEWQWbEGuUP86De9WB/yLAAAABHNzaDoAAAAAAAAAAAAAAAIAAAAGcmV2aWV3AAAADQAAAAkxMjcuMC4wLjEAAAAAAAAAAP//////////AAAAAAAAAAAAAAAAAAAAMwAAAAtzc2gtZWQyNTUxOQAAACAZf2sj4WyFMsaryDj6zV6nib4MdrKSAzQDm/qLPTaNYQAAAFMAAAALc3NoLWVkMjU1MTkAAABAQrKTLIeDJT5Shf7Qx+zEn45fWJOPgBjG0QYDuJeiEpVYMdcZx/3cOh9e0Er3HUNH57IdTyW5B0JcsKugaUvYDA==";

    /// The verdict on `host_key` in the host-bound field of every recorded host-bound
    /// login; panics unless every vector agrees.
    fn host_key_verdict(host_key: &[u8]) -> Option<RefusalClass> {
        let mut verdicts = Vec::new();
        for v in logins() {
            let mut l = Login::split(&v.data);
            if !l.is_host_bound() {
                continue;
            }
            l.extra = vec![host_key.to_vec()];
            verdicts.push(verdict(&git_and_deploy(), &v.key, &l.encode()));
        }
        assert!(
            verdicts.len() >= 5,
            "the fixture's host-bound logins are missing"
        );
        assert!(verdicts.windows(2).all(|w| w[0] == w[1]), "{verdicts:?}");
        verdicts[0]
    }

    /// The verdict on `key` as the SIGNING key, for a `git` SSHSIG and for a `git`
    /// login (the recorded Ed25519 login with the key and algorithm swapped in);
    /// panics unless they agree.
    fn signing_key_verdict(key: &[u8]) -> Option<RefusalClass> {
        let sig = sshsigs().into_iter().find(|v| v.expect.is_none()).unwrap();
        let by_sig = verdict(&SignPolicy::default(), key, &sig.data);
        let login = logins()
            .into_iter()
            .find(|v| v.user_key == "ed25519" && v.expect.is_none())
            .unwrap();
        let mut l = Login::split(&login.data);
        l.key = key.to_vec();
        if let Ok(name) = Reader::new(key).string() {
            l.algorithm = if name == b"ssh-rsa" {
                b"rsa-sha2-512".to_vec()
            } else {
                name.to_vec()
            };
        }
        let by_login = verdict(&SignPolicy::default(), key, &l.encode());
        assert_eq!(by_sig, by_login, "SSHSIG and login disagree");
        by_sig
    }

    const MALFORMED_KEY: Option<RefusalClass> = Some(RefusalClass::MalformedKey);

    #[test]
    fn rsa_keys_must_be_valid_not_only_canonical() {
        let cases: Vec<(&str, Vec<u8>, bool)> = vec![
            (
                "the reviewer's: modulus 1, exponent -1",
                rsa_modulus_one(),
                false,
            ),
            ("modulus 1023 bits", rsa_blob(1023), false),
            ("modulus 1024 bits", rsa_blob(1024), true),
            ("modulus 3072 bits", rsa_blob(3072), true),
            ("modulus 16384 bits", rsa_blob(16384), true),
            ("modulus 16385 bits", rsa_blob(16385), false),
            (
                "exponent even",
                rsa_blob_raw(&mpint(&[1, 0, 0]), &rsa_parts(&rsa_blob(2048))[2]),
                false,
            ),
            (
                "exponent zero",
                {
                    let r = rsa_parts(&rsa_blob(2048));
                    rsa_blob_raw(&[], &r[2])
                },
                false,
            ),
            (
                "exponent negative",
                {
                    let r = rsa_parts(&rsa_blob(2048));
                    rsa_blob_raw(&[0x81], &r[2])
                },
                false,
            ),
            ("modulus zero", rsa_blob_raw(&mpint(&[1, 0, 1]), &[]), false),
            (
                "modulus negative",
                {
                    let r = rsa_parts(&rsa_blob(2048));
                    let mut n = r[2].clone();
                    n.remove(0); // drop the sign-guarding zero: the top bit becomes the sign
                    rsa_blob_raw(&r[1], &n)
                },
                false,
            ),
            (
                "exponent one",
                {
                    let r = rsa_parts(&rsa_blob(2048));
                    rsa_blob_raw(&[1], &r[2])
                },
                true,
            ),
        ];
        for (label, key, valid) in cases {
            let want = if valid { None } else { MALFORMED_KEY };
            assert_eq!(signing_key_verdict(&key), want, "signing key: {label}");
            let host = if valid { None } else { UNPARSEABLE };
            assert_eq!(host_key_verdict(&key), host, "host key: {label}");
        }
        // The recorded RSA user key and RSA host key are 3072 bits; both still pass.
        for v in logins().iter().filter(|v| v.user_key == "rsa") {
            assert_eq!(signing_key_verdict(&v.key), None, "{}", v.name);
        }
    }

    #[test]
    fn a_leading_zero_in_a_host_key_mpint_is_tolerated_but_not_in_the_signing_key() {
        // OpenSSH's own reader trims leading zeros, and a CA signs a certificate's
        // bytes verbatim; the signing key must re-encode exactly (the bus decodes it).
        let r = rsa_parts(&rsa_blob(2048));
        let mut e = vec![0];
        e.extend_from_slice(&r[1]);
        let padded = rsa_blob_raw(&e, &r[2]);
        assert_eq!(host_key_verdict(&padded), None);
        assert_eq!(signing_key_verdict(&padded), MALFORMED_KEY);
        // But not a run of leading zeros longer than OpenSSH reads (at most
        // SSHBUF_MAX_BIGNUM + 1 bytes as written), however small the value.
        for (zeros, ok) in [(0, true), (2046, true), (2047, false), (3000, false)] {
            // `zeros` + 3 value bytes against the 2049-byte limit.
            let mut e = vec![0u8; zeros];
            e.extend_from_slice(&[1, 0, 1]);
            let key = rsa_blob_raw(&e, &r[2]);
            let want = if ok { None } else { UNPARSEABLE };
            assert_eq!(host_key_verdict(&key), want, "{zeros} leading zeros");
        }
    }

    #[test]
    fn ecdsa_keys_must_be_on_the_curve_and_uncompressed() {
        let good = ec_point("nistp256");
        let mut compressed = vec![0x02 | (good[64] & 1)];
        compressed.extend_from_slice(&good[1..33]);
        let mut y_flipped = good.clone();
        y_flipped[64] ^= 1;
        let mut p384_off = ec_point("nistp384");
        p384_off[1] ^= 1;
        let mut p521_off = ec_point("nistp521");
        p521_off[1] ^= 1;
        let mut identity = vec![0x04];
        identity.extend_from_slice(&[0; 64]);
        let mut x_is_p = vec![0x04];
        x_is_p.extend_from_slice(&[0xff; 64]);
        let cases: Vec<(&str, Vec<u8>)> = vec![
            ("the reviewer's: the point (0, 0)", off_curve_p256()),
            (
                "y off by one",
                ecdsa_blob_raw("ecdsa-sha2-nistp256", "nistp256", &y_flipped),
            ),
            (
                "compressed",
                ecdsa_blob_raw("ecdsa-sha2-nistp256", "nistp256", &compressed),
            ),
            ("leading 0x02 on 65 bytes", {
                let mut p = good.clone();
                p[0] = 0x02;
                ecdsa_blob_raw("ecdsa-sha2-nistp256", "nistp256", &p)
            }),
            (
                "one byte short",
                ecdsa_blob_raw("ecdsa-sha2-nistp256", "nistp256", &good[..64]),
            ),
            (
                "curve name mismatch",
                ecdsa_blob_raw("ecdsa-sha2-nistp256", "nistp384", &good),
            ),
            (
                "P-256 point under P-384",
                ecdsa_blob_raw("ecdsa-sha2-nistp384", "nistp384", &good),
            ),
            (
                "P-384 off curve",
                ecdsa_blob_raw("ecdsa-sha2-nistp384", "nistp384", &p384_off),
            ),
            (
                "P-521 off curve",
                ecdsa_blob_raw("ecdsa-sha2-nistp521", "nistp521", &p521_off),
            ),
            (
                "coordinates not below p",
                ecdsa_blob_raw("ecdsa-sha2-nistp256", "nistp256", &x_is_p),
            ),
            (
                "identity",
                ecdsa_blob_raw("ecdsa-sha2-nistp256", "nistp256", &[0]),
            ),
        ];
        for (label, key) in cases {
            assert_eq!(
                signing_key_verdict(&key),
                MALFORMED_KEY,
                "signing key: {label}"
            );
            assert_eq!(host_key_verdict(&key), UNPARSEABLE, "host key: {label}");
        }
    }

    #[test]
    fn ecdsa_p256_p384_and_p521_keys_are_accepted_as_signing_and_host_keys() {
        for curve in ["nistp256", "nistp384", "nistp521"] {
            let key = ecdsa_blob(curve);
            assert_eq!(signing_key_verdict(&key), None, "signing key {curve}");
            assert_eq!(host_key_verdict(&key), None, "host key {curve}");
            let sig = &sshsigs()[0].data;
            let approved = SignPolicy::default().check(&key, sig, 0).unwrap();
            assert_eq!(approved.key_type(), format!("ecdsa-sha2-{curve}"));
        }
    }

    #[test]
    fn security_key_types_are_host_keys_but_never_signing_keys() {
        let sk_cert = b64(SK_HOST_CERT_B64);
        let p256 = ec_point("nistp256");
        let valid: Vec<(&str, Vec<u8>)> = vec![
            (
                "the reviewer's sk-ssh-ed25519 host certificate",
                sk_cert.clone(),
            ),
            ("sk-ssh-ed25519 key", sk_ed25519_blob(b"ssh:")),
            ("sk-ecdsa-sha2-nistp256 key", sk_ecdsa_blob(&p256, b"ssh:")),
        ];
        for (label, key) in valid {
            assert_eq!(host_key_verdict(&key), None, "host key: {label}");
            assert_eq!(
                signing_key_verdict(&key),
                MALFORMED_KEY,
                "signing key: {label}"
            );
        }
        let mut point00 = vec![0x04];
        point00.extend_from_slice(&[0; 64]);
        let invalid: Vec<(&str, Vec<u8>)> = vec![
            ("sk-ecdsa off curve", sk_ecdsa_blob(&point00, b"ssh:")),
            (
                "sk-ed25519 application with NUL",
                sk_ed25519_blob(b"ssh:\0x"),
            ),
            ("sk-ed25519 without application", {
                let mut b = Vec::new();
                put_string(&mut b, b"sk-ssh-ed25519@openssh.com");
                put_string(&mut b, &[7; 32]);
                b
            }),
            ("sk certificate cut", sk_cert[..sk_cert.len() - 1].to_vec()),
        ];
        for (label, key) in invalid {
            assert_eq!(host_key_verdict(&key), UNPARSEABLE, "host key: {label}");
        }
    }

    // ----- the host key: certificates -----

    /// A certificate's top-level fields, in order (`S` = string or mpint).
    #[derive(Clone, Debug, PartialEq)]
    enum F {
        S(Vec<u8>),
        Q(u64),
        W(u32),
    }

    /// Split an Ed25519 certificate (one key field) into its fields: 0 type, 1 nonce,
    /// 2 key, 3 serial, 4 type, 5 key id, 6 principals, 7 valid after, 8 valid
    /// before, 9 critical options, 10 extensions, 11 reserved, 12 signature key,
    /// 13 signature.
    fn split_ed25519_cert(blob: &[u8]) -> Vec<F> {
        let mut r = Reader::new(blob);
        let mut out = Vec::new();
        for kind in "SSSQWSSQQSSSSS".bytes() {
            out.push(match kind {
                b'S' => F::S(r.string().unwrap().to_vec()),
                b'Q' => F::Q(r.u64().unwrap()),
                _ => F::W(r.u32().unwrap()),
            });
        }
        assert!(r.exhausted());
        assert_eq!(encode_cert(&out), blob, "split/encode round trip");
        out
    }

    fn encode_cert(fields: &[F]) -> Vec<u8> {
        let mut out = Vec::new();
        for f in fields {
            match f {
                F::S(s) => put_string(&mut out, s),
                F::Q(q) => out.extend_from_slice(&q.to_be_bytes()),
                F::W(w) => out.extend_from_slice(&w.to_be_bytes()),
            }
        }
        out
    }

    fn strings(items: &[&[u8]]) -> Vec<u8> {
        let mut out = Vec::new();
        for s in items {
            put_string(&mut out, s);
        }
        out
    }

    type CertMutation = (&'static str, fn(&mut Vec<F>), Option<RefusalClass>);

    const CERT_MUTATIONS: &[CertMutation] = &[
        ("unmutated", |_| {}, None),
        ("type 3", |c| c[4] = F::W(3), UNPARSEABLE),
        ("user certificate", |c| c[4] = F::W(1), None),
        (
            "certified key 31 bytes",
            |c| c[2] = F::S(vec![7; 31]),
            UNPARSEABLE,
        ),
        (
            "key id with NUL",
            |c| c[5] = F::S(b"a\0b".to_vec()),
            UNPARSEABLE,
        ),
        ("key id empty", |c| c[5] = F::S(Vec::new()), None),
        ("principals empty", |c| c[6] = F::S(Vec::new()), None),
        (
            "principals not a string",
            |c| c[6] = F::S(b"x".to_vec()),
            UNPARSEABLE,
        ),
        (
            "principal with NUL",
            |c| c[6] = F::S(strings(&[b"host\0x"])),
            UNPARSEABLE,
        ),
        (
            "principals with a trailing byte",
            |c| {
                let mut p = strings(&[b"host"]);
                p.push(0);
                c[6] = F::S(p);
            },
            UNPARSEABLE,
        ),
        (
            "256 principals",
            |c| c[6] = F::S(strings(&[&b"h"[..]; 256])),
            None,
        ),
        (
            "257 principals",
            |c| c[6] = F::S(strings(&[&b"h"[..]; 257])),
            UNPARSEABLE,
        ),
        ("valid forever", |c| c[8] = F::Q(u64::MAX), None),
        (
            "critical option pair",
            |c| c[9] = F::S(strings(&[b"force-command", &strings(&[b"true"])])),
            None,
        ),
        (
            "critical option name without data",
            |c| c[9] = F::S(strings(&[b"force-command"])),
            UNPARSEABLE,
        ),
        (
            "critical options garbage",
            |c| c[9] = F::S(vec![0, 0, 1]),
            UNPARSEABLE,
        ),
        (
            "extension pairs",
            |c| {
                c[10] = F::S(strings(&[
                    b"permit-X11-forwarding",
                    b"",
                    b"permit-pty",
                    b"",
                ]))
            },
            None,
        ),
        (
            "extension name without data",
            |c| c[10] = F::S(strings(&[b"permit-pty", b"", b"permit-user-rc"])),
            UNPARSEABLE,
        ),
        ("reserved non-empty", |c| c[11] = F::S(b"x".to_vec()), None),
        (
            "signature key off curve",
            |c| c[12] = F::S(off_curve_p256()),
            UNPARSEABLE,
        ),
        (
            "signature key RSA modulus 1",
            |c| c[12] = F::S(rsa_modulus_one()),
            UNPARSEABLE,
        ),
        (
            "signature key a certificate",
            |c| c[12] = F::S(b64(&fixture().host_certificate_b64)),
            UNPARSEABLE,
        ),
        (
            "the reviewer's: signature S(bogus) S()",
            |c| c[13] = F::S(strings(&[b"bogus", b""])),
            UNPARSEABLE,
        ),
        (
            "signature empty blob",
            |c| c[13] = F::S(strings(&[b"ssh-ed25519", b""])),
            UNPARSEABLE,
        ),
        (
            "signature format of another key type",
            |c| c[13] = F::S(strings(&[b"rsa-sha2-512", &[1; 64]])),
            UNPARSEABLE,
        ),
        (
            "signature with an sk- tail from a plain key",
            |c| {
                let mut s = strings(&[b"ssh-ed25519", &[1; 64]]);
                s.extend_from_slice(&[1, 0, 0, 0, 9]);
                c[13] = F::S(s);
            },
            UNPARSEABLE,
        ),
        (
            "signature with a trailing byte",
            |c| {
                let mut s = strings(&[b"ssh-ed25519", &[1; 64]]);
                s.push(0);
                c[13] = F::S(s);
            },
            UNPARSEABLE,
        ),
        (
            // The CA signature is not verified (module doc): a well-formed signature
            // that is not the CA's still parses.
            "signature well formed but not verified",
            |c| c[13] = F::S(strings(&[b"ssh-ed25519", &[1; 64]])),
            None,
        ),
        (
            "an RSA CA and an rsa-sha2-512 signature",
            |c| {
                c[12] = F::S(rsa_blob(2048));
                c[13] = F::S(strings(&[b"rsa-sha2-512", &[1; 256]]));
            },
            None,
        ),
        (
            "an sk CA and its flags and counter",
            |c| {
                c[12] = F::S(sk_ed25519_blob(b"ssh:"));
                let mut s = strings(&[b"sk-ssh-ed25519@openssh.com", &[1; 64]]);
                s.extend_from_slice(&[1, 0, 0, 0, 9]);
                c[13] = F::S(s);
            },
            None,
        ),
        (
            "an sk CA without flags and counter",
            |c| {
                c[12] = F::S(sk_ed25519_blob(b"ssh:"));
                c[13] = F::S(strings(&[b"sk-ssh-ed25519@openssh.com", &[1; 64]]));
            },
            UNPARSEABLE,
        ),
        (
            "a trailing field",
            |c| c.push(F::S(Vec::new())),
            UNPARSEABLE,
        ),
    ];

    #[test]
    fn certificate_fields_are_validated_to_their_nested_structure() {
        let base = split_ed25519_cert(&host_certificate());
        for (label, mutate, want) in CERT_MUTATIONS {
            let mut c = base.clone();
            mutate(&mut c);
            assert_eq!(host_key_verdict(&encode_cert(&c)), *want, "{label}");
        }
    }

    #[test]
    fn the_reviewers_principal_length_overflow_is_refused() {
        // Byte 142 of the recorded certificate is the high byte of the one
        // principal's inner length: setting it declares 0x01000009 bytes where nine
        // remain.
        let mut cert = host_certificate();
        let F::S(principals) = &split_ed25519_cert(&cert)[6] else {
            unreachable!()
        };
        assert_eq!(principals[..4], [0, 0, 0, 9]);
        assert_eq!(cert[142..146], [0, 0, 0, 9]);
        assert_eq!(host_key_verdict(&cert), None);
        cert[142] = 1;
        assert_eq!(host_key_verdict(&cert), UNPARSEABLE);
    }

    // ----- the flags -----

    /// Flags words to try: every combination of the low bits, plus high bits.
    const FLAG_WORDS: [u32; 12] = [0, 1, 2, 3, 4, 5, 6, 7, 8, 12, 0x8000_0000, 0x8000_0004];

    #[test]
    fn rsa_flags_must_name_the_payloads_hash() {
        let mut checked = 0;
        for v in logins().iter().filter(|v| v.user_key == "rsa") {
            for (algorithm, bits) in [
                (&b"ssh-rsa"[..], 0u32),
                (b"rsa-sha2-256", 2),
                (b"rsa-sha2-512", 4),
            ] {
                let mut l = Login::split(&v.data);
                l.algorithm = algorithm.to_vec();
                for flags in FLAG_WORDS {
                    let want = if flags & 6 == bits { None } else { UNPARSEABLE };
                    assert_eq!(
                        verdict_flags(&git_and_deploy(), &v.key, &l.encode(), flags),
                        want,
                        "{} {algorithm:?} flags {flags:#x}",
                        v.name
                    );
                    checked += 1;
                }
            }
        }
        for v in sshsigs().iter().filter(|v| v.user_key == "rsa") {
            for flags in FLAG_WORDS {
                let want = if matches!(flags & 6, 2 | 4) {
                    None
                } else {
                    UNPARSEABLE
                };
                assert_eq!(
                    verdict_flags(&git_and_deploy(), &v.key, &v.data, flags),
                    want,
                    "{} flags {flags:#x}",
                    v.name
                );
                checked += 1;
            }
        }
        assert!(
            checked >= 100,
            "the fixture's RSA vectors are missing ({checked})"
        );
        // The reviewer's cases, by name.
        let v512 = logins()
            .into_iter()
            .find(|v| v.name == "login-rsa-sha2-512-hostbound")
            .unwrap();
        let r = SignPolicy::default()
            .check(&v512.key, &v512.data, 0)
            .unwrap_err();
        assert_eq!(
            r.to_string(),
            "unparseable_data: login: RSA flags do not name the algorithm's hash"
        );
        let sig = sshsigs().into_iter().find(|v| v.user_key == "rsa").unwrap();
        let r = SignPolicy::default()
            .check(&sig.key, &sig.data, 0)
            .unwrap_err();
        assert_eq!(
            r.to_string(),
            "unparseable_data: sshsig: RSA flags do not name exactly one SHA-2 hash"
        );
    }

    #[test]
    fn flags_for_other_key_types_pass_through_unchanged() {
        let mut checked = 0;
        for v in vectors()
            .iter()
            .filter(|v| v.user_key != "rsa" && v.expect.is_none())
        {
            for flags in FLAG_WORDS {
                let backend = Recorder::default();
                let approved = SignPolicy::default()
                    .check(&v.key, &v.data, flags)
                    .unwrap_or_else(|r| panic!("{} flags {flags:#x}: {r}", v.name));
                assert_eq!(approved.flags(), flags);
                approved.sign(&backend).unwrap();
                assert_eq!(backend.calls.lock().unwrap()[0].2, flags, "{}", v.name);
                checked += 1;
            }
        }
        assert!(
            checked >= 80,
            "the fixture's non-RSA vectors are missing ({checked})"
        );
    }

    #[test]
    fn an_approval_prints_only_lengths_and_the_fingerprint() {
        let v = &vectors()[0];
        let approved = SignPolicy::default()
            .check(&v.key, &v.data, v.flags)
            .unwrap();
        let shown = format!("{approved:?}");
        assert_eq!(
            shown,
            format!(
                "Approved {{ key: {:?}, key_len: {}, data_len: {}, .. }}",
                fingerprint(&v.key),
                v.key.len(),
                v.data.len()
            )
        );
        // No byte list (a derived Debug printed both slices as `[0, 0, 0, 32, ...]`).
        let alt = format!("{approved:#?}");
        assert!(!alt.contains('['), "{alt}");
    }

    // ----- the refusal record: escaping and long names -----

    #[test]
    fn refusal_details_escape_bidi_zero_width_and_non_utf8_bytes() {
        let v = logins()
            .into_iter()
            .find(|v| v.user_key == "ed25519" && v.expect.is_none())
            .unwrap();
        let cases: [(&[u8], &str); 5] = [
            // U+202E RIGHT-TO-LEFT OVERRIDE, which could reverse a log line's text.
            (
                "git\u{202e}tig".as_bytes(),
                r"login user=git\xe2\x80\xaetig",
            ),
            // U+200B ZERO WIDTH SPACE, invisible in a log line.
            ("g\u{200b}it".as_bytes(), r"login user=g\xe2\x80\x8bit"),
            // U+2066 LEFT-TO-RIGHT ISOLATE and U+FEFF (BOM / zero-width no-break).
            (
                "\u{2066}git\u{feff}".as_bytes(),
                r"login user=\xe2\x81\xa6git\xef\xbb\xbf",
            ),
            // Not UTF-8 at all: a lone continuation byte, an invalid lead, 0xff.
            (b"git\x80\xc3\x28\xff", r"login user=git\x80\xc3(\xff"),
            // Controls: a newline could forge a second log line.
            (
                b"git\nok user=git\r\t\x1b[0m",
                r"login user=git\x0aok\x20user=git\x0d\x09\x1b[0m",
            ),
        ];
        for (name, want) in cases {
            let mut l = Login::split(&v.data);
            l.username = name.to_vec();
            let r = SignPolicy::default()
                .check(&v.key, &l.encode(), v.flags)
                .unwrap_err();
            assert_eq!(r.class(), RefusalClass::UsernameNotAllowed);
            assert_eq!(r.detail(), want);
            assert_detail_is_bounded(&r);
        }
        // The same escaping for a namespace.
        let s = sshsigs().into_iter().next().unwrap();
        let mut sig = Sig::split(&s.data);
        sig.namespace = "git\u{202e}".as_bytes().to_vec();
        let r = SignPolicy::default()
            .check(&s.key, &sig.encode(), s.flags)
            .unwrap_err();
        assert_eq!(r.class(), RefusalClass::WrongNamespace);
        assert_eq!(r.detail(), r"sshsig namespace=git\xe2\x80\xae");
    }

    #[test]
    fn a_long_listed_username_is_accepted_only_exactly() {
        let v = logins()
            .into_iter()
            .find(|v| v.user_key == "ed25519" && v.expect.is_none())
            .unwrap();
        let long = "d".repeat(4096);
        let policy = SignPolicy::new(["git".to_string(), long.clone()], None);
        let mut l = Login::split(&v.data);
        l.username = long.as_bytes().to_vec();
        let data = l.encode();
        let approved = policy.check(&v.key, &data, v.flags).unwrap();
        assert_eq!(
            approved.kind(),
            &SignKind::Login {
                username: long.clone(),
                method: LoginMethod::HostBound
            }
        );
        assert_eq!(verdict(&policy, &v.key, &l.encode()), None);
        // One byte more, one byte less, or one byte different is not listed; the
        // detail is still bounded.
        for name in [
            format!("{long}d"),
            long[1..].to_string(),
            format!("{}e", &long[1..]),
        ] {
            let mut l = Login::split(&v.data);
            l.username = name.into_bytes();
            let r = policy.check(&v.key, &l.encode(), v.flags).unwrap_err();
            assert_eq!(r.class(), RefusalClass::UsernameNotAllowed);
            assert_eq!(r.detail(), format!("login user={}...", "d".repeat(64)));
        }
    }

    // ----- exhaustive mutation and fuzzing: never a panic, never a third shape -----

    #[test]
    fn single_byte_edits_of_every_vector_never_accept_anything_but_the_two_shapes() {
        let policy = git_and_deploy();
        let mut accepted = 0usize;
        let mut checked = 0usize;
        for v in vectors() {
            for i in 0..v.data.len() {
                let b = v.data[i];
                for value in 0..=u8::MAX {
                    if value == b {
                        continue;
                    }
                    let mut data = v.data.clone();
                    data[i] = value;
                    accepted += usize::from(verdict(&policy, &v.key, &data).is_none());
                    checked += 1;
                }
                let mut deleted = v.data.clone();
                deleted.remove(i);
                assert!(
                    verdict(&policy, &v.key, &deleted).is_some(),
                    "{} delete {i}",
                    v.name
                );
                for ins in [0x00, 0xff] {
                    let mut inserted = v.data.clone();
                    inserted.insert(i, ins);
                    assert!(
                        verdict(&policy, &v.key, &inserted).is_some(),
                        "{} insert at {i}",
                        v.name
                    );
                }
                checked += 3;
            }
        }
        // Edits inside the free fields (session id, host key body, hash) stay valid;
        // the oracle judged each of those.
        assert!(
            accepted > 0 && checked > 1_000_000,
            "accepted {accepted} of {checked}"
        );
    }

    /// xorshift64*: deterministic, so a failure reproduces.
    struct Rng(u64);
    impl Rng {
        fn next(&mut self) -> u64 {
            self.0 ^= self.0 >> 12;
            self.0 ^= self.0 << 25;
            self.0 ^= self.0 >> 27;
            self.0.wrapping_mul(0x2545_f491_4f6c_dd1d)
        }
        fn below(&mut self, n: usize) -> usize {
            (self.next() % n as u64) as usize
        }
        fn bytes(&mut self, n: usize) -> Vec<u8> {
            (0..n).map(|_| self.next() as u8).collect()
        }
    }

    #[test]
    fn arbitrary_input_never_panics_and_is_never_accepted_outside_the_two_shapes() {
        let policy = git_and_deploy();
        let all = vectors();
        let mut rng = Rng(0x5eed_0f5c_edc0_de17);
        for round in 0..40_000 {
            let v = &all[rng.below(all.len())];
            let (key, data) = match round % 4 {
                // Random bytes, sometimes behind the SSHSIG preamble.
                0 => {
                    let mut d = if rng.below(2) == 0 {
                        b"SSHSIG".to_vec()
                    } else {
                        Vec::new()
                    };
                    let n = rng.below(300);
                    d.extend(rng.bytes(n));
                    (v.key.clone(), d)
                }
                // A vector with a handful of random edits.
                1 | 2 => {
                    let mut d = v.data.clone();
                    for _ in 0..1 + rng.below(6) {
                        let at = rng.below(d.len() + 1);
                        match rng.below(4) {
                            0 if at < d.len() => d[at] = rng.next() as u8,
                            1 => d.insert(at, rng.next() as u8),
                            2 if at < d.len() => {
                                d.remove(at);
                            }
                            _ => {
                                let from = rng.below(d.len().max(1));
                                let len = rng.below(16).min(d.len() - from.min(d.len()));
                                let chunk = d[from..from + len].to_vec();
                                let at = at.min(d.len());
                                d.splice(at..at, chunk);
                            }
                        }
                    }
                    (v.key.clone(), d)
                }
                // A random or edited key with the vector's data.
                _ => {
                    let mut k = v.key.clone();
                    if rng.below(2) == 0 {
                        let at = rng.below(k.len());
                        k[at] = rng.next() as u8;
                    } else {
                        let n = rng.below(64);
                        k = rng.bytes(n);
                    }
                    (k, v.data.clone())
                }
            };
            let _ = verdict(&policy, &key, &data);
        }
    }
}
