//! The host-agent's **local-keys** SSH signing backend — the Rust port of the Go
//! daemon's `ssh_backend.go` (the `SSHBackend` seam + `ResolveSSHBackend`) and
//! `ssh_backend_localkeys.go` (the `~/.ssh/{id_ed25519,id_rsa,id_ecdsa}` reader +
//! multi-algorithm signer). It loads the private-key encodings Go's
//! `ssh.ParsePrivateKey` accepts (`load_key` dispatches on the PEM label like
//! `x/crypto/ssh`'s `ParseRawPrivateKey`): OpenSSH v1 (`-----BEGIN OPENSSH PRIVATE
//! KEY-----`), plus the legacy OpenSSL PEM encodings a real `~/.ssh/id_rsa`/`id_ecdsa`
//! often is — PKCS#1 (`RSA PRIVATE KEY`), PKCS#8 (`PRIVATE KEY`, RSA/EC/ed25519), and
//! SEC1 (`EC PRIVATE KEY`). It signs a challenge byte-for-byte the way Go's
//! `x/crypto/ssh` signer does:
//!
//! - **ed25519** (`ssh-ed25519`) — raw 64-byte signature. *Deterministic* per RFC
//!   8032, so the blob is byte-identical to Go's (compared UNMASKED in the
//!   differential; see `signs_go_reference_vector`).
//! - **RSA** — the raw PKCS#1 v1.5 signature bytes. `flags` select the digest exactly
//!   like Go's `SignWithFlags`/`SignWithAlgorithm` (`ssh_backend_localkeys.go:93`):
//!   `flags&2` → `rsa-sha2-256`, else `flags&4` → `rsa-sha2-512`, else the **`ssh-rsa`
//!   SHA-1 default** (that is what `ssh.Signer.Sign` does — do NOT "improve" it).
//!   PKCS#1 v1.5 is mathematically deterministic, but the differential treats RSA as
//!   **verify-not-bytes** (the program brief pins this): the `format` is compared and
//!   the blob is cryptographically verified against the fixture pubkey on both impls.
//! - **ECDSA** (`ecdsa-sha2-nistp256/384/521`) — the blob is the x/crypto/ssh ecdsa
//!   encoding `mpint(r) ‖ mpint(s)` (`ssh.Marshal(struct{ R, S *big.Int }{})`), signed
//!   over the curve's canonical hash (P-256→SHA-256, P-384→SHA-384, P-521→SHA-512).
//!   Non-deterministic in Go (random `k`), so ECDSA is **verify-only** everywhere.
//!
//! **Why RustCrypto directly, not ssh-key's signer:** `ssh-key` 0.6's own RSA signer
//! emits SHA-512 only — it cannot produce Go's `ssh-rsa` (SHA-1) default nor
//! `rsa-sha2-256`. So we parse the OpenSSH key with `ssh-key`, extract the key
//! material, and drive the `rsa` / `p256|p384|p521` signers directly (see
//! `Cargo.toml` for the version-unification note).
//!
//! **Feature posture:** no `desktop-forwarding` gate — the message bus (always
//! compiled, even headless) owns `sign`, so the signing deps are always-on and the
//! `--no-default-features` build compiles this module.
//!
//! **Accepted divergences from Go's `ssh.ParsePrivateKey`** (both narrow, both
//! warn+skip where Go would load — fail-closed, one fewer usable key, never a
//! wrong signature):
//! - **DSA** (`-----BEGIN DSA PRIVATE KEY-----`, or OpenSSH `ssh-dss`) is skipped.
//!   Go's `ParsePrivateKey` still loads it, but DSA is deprecated (OpenSSH disabled
//!   `ssh-dss` by default in 7.0, 2015) and wiring the `dsa` crate's non-mpint 40-byte
//!   `ssh-dss` signature encoding + legacy-PEM decode is not worth the surface for a
//!   key type that no modern server accepts. `key_material` returns `None` for it and
//!   `load_key` reports the `DSA PRIVATE KEY` label as unsupported.
//! - **P-521 shortened-scalar keys** — ssh-key 0.6.7's OpenSSH decoder rejects a
//!   P-521 private scalar whose mpint stripped a leading zero to 65 bytes (see the
//!   `ECDSA_P521_KEY` fixture note). A real user's P-521 `id_ecdsa` may therefore
//!   warn+skip where Go loads it. (Legacy-PEM P-521 via `SecretKey::from_sec1_pem`
//!   is *not* affected — the SEC1/PKCS#8 scalar is fixed-width.)

use std::path::{Path, PathBuf};
use std::sync::Arc;

use ed25519_dalek::Signer; // = signature::Signer — also covers the p256/p384/p521 SigningKeys.
use ssh_key::private::{KeypairData, PrivateKey};

use crate::config::user_home_dir;

/// The standard SSH private-key filenames, loaded in this exact order (Go's
/// `ssh_backend_localkeys.go:standardKeyFiles`).
const STANDARD_KEY_FILES: [&str; 3] = ["id_ed25519", "id_rsa", "id_ecdsa"];

/// A public key the backend can offer (the `list` op's per-key shape; mirrors Go's
/// `agent.Key`). `blob` is the SSH-wire marshaled public key (Go
/// `ssh.PublicKey.Marshal()`).
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct SshKeyInfo {
    pub format: String,
    pub blob: Vec<u8>,
    pub comment: String,
}

/// A produced signature (mirrors Go's `ssh.Signature`): the algorithm `format` and
/// the raw signature `blob`.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct SshSignature {
    pub format: String,
    pub blob: Vec<u8>,
}

/// SSH agent signature flags (x/crypto/ssh/agent `SignatureFlagRsaSha256`/`512`) that
/// select an rsa-sha2 variant. These are **bit tests**, not exact values, and
/// `SHA256` is checked FIRST (so `flags = 6` → sha256). Reserved bit 0 (=1) and the
/// unassigned bit 3 (=8) are neither of these and fall through to the default sign.
const SIG_FLAG_RSA_SHA256: u32 = 2;
const SIG_FLAG_RSA_SHA512: u32 = 4;

/// The host SSH key backend (Go `ssh_backend.go:SSHBackend`). `Send + Sync` so the
/// bus can hold it as `Arc<dyn SshBackend>` across the async handler.
pub trait SshBackend: Send + Sync {
    /// The available public keys.
    fn list(&self) -> Result<Vec<SshKeyInfo>, String>;
    /// Sign `data` with the key whose SSH-wire marshaled public key equals
    /// `public_key` (Go's `keysEqual` = `bytes.Equal(a.Marshal(), b.Marshal())`).
    /// An **rsa-sha2 `flags` bit on a non-RSA key is a sign error** (Go's
    /// `SignWithAlgorithm` rejects it) → `Err` → `{sign operation failed,
    /// SIGN_FAILED}`; other/reserved flags produce the key's default signature. No
    /// matching key → `Err("key not found")`, which the handler also maps to
    /// `SIGN_FAILED` — NOT `KEY_NOT_FOUND` (that code is only for an unparsable
    /// public key).
    fn sign(&self, public_key: &[u8], data: &[u8], flags: u32) -> Result<SshSignature, String>;
    /// The backend mode name (`"agent-forward"` or `"local-keys"`).
    fn mode(&self) -> &str;
}

/// The RSA digest an `rsa-sha2` flag selects (or the `ssh-rsa` SHA-1 default).
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum RsaHash {
    Sha1,
    Sha256,
    Sha512,
}

impl RsaHash {
    /// The SSH signature `format` name this hash produces for an RSA key.
    fn ssh_format(self) -> &'static str {
        match self {
            RsaHash::Sha1 => "ssh-rsa",
            RsaHash::Sha256 => "rsa-sha2-256",
            RsaHash::Sha512 => "rsa-sha2-512",
        }
    }
}

/// Map the `flags` bitfield to an rsa-sha2 override, mirroring Go's
/// `ssh_backend_localkeys.go:93-100` switch (bit 2 checked before bit 4). `None`
/// means "no override" → the key's default algorithm.
fn rsa_algo_from_flags(flags: u32) -> Option<RsaHash> {
    if flags & SIG_FLAG_RSA_SHA256 != 0 {
        Some(RsaHash::Sha256)
    } else if flags & SIG_FLAG_RSA_SHA512 != 0 {
        Some(RsaHash::Sha512)
    } else {
        None
    }
}

/// The signable key material for one loaded key (ed25519 / RSA / ECDSA-by-curve).
enum KeyMaterial {
    Ed25519(ed25519_dalek::SigningKey),
    Rsa(Box<rsa::RsaPrivateKey>),
    Ecdsa(EcdsaMaterial),
}

/// An ECDSA signing key, split by curve (each curve is its own RustCrypto type).
enum EcdsaMaterial {
    P256(p256::ecdsa::SigningKey),
    P384(p384::ecdsa::SigningKey),
    P521(p521::ecdsa::SigningKey),
}

impl EcdsaMaterial {
    /// The SSH public-key / signature algorithm name for this curve (Go's
    /// `pubKey.Type()`), also used as the signature `format`.
    fn format(&self) -> &'static str {
        match self {
            EcdsaMaterial::P256(_) => "ecdsa-sha2-nistp256",
            EcdsaMaterial::P384(_) => "ecdsa-sha2-nistp384",
            EcdsaMaterial::P521(_) => "ecdsa-sha2-nistp521",
        }
    }

    /// The `nistp256`/`nistp384`/`nistp521` curve identifier (the second SSH-wire
    /// `string` in an `ecdsa-sha2-nistpX` public key).
    fn curve_id(&self) -> &'static str {
        match self {
            EcdsaMaterial::P256(_) => "nistp256",
            EcdsaMaterial::P384(_) => "nistp384",
            EcdsaMaterial::P521(_) => "nistp521",
        }
    }

    /// The SSH-wire marshaled public key for a legacy-PEM ECDSA key:
    /// `string(format) ‖ string(curve_id) ‖ string(uncompressed SEC1 point)` — the
    /// same bytes ssh-key's `public_key().to_bytes()` produces for the OpenSSH path
    /// (round-trip-checked in tests, so `keysEqual` matches across on-disk formats).
    fn marshaled_pub(&self) -> Vec<u8> {
        // `string(uncompressed SEC1 point)` — the `EncodedPoint` temporary lives to the
        // end of each arm, so its `as_bytes()` slice feeds `ssh_string` directly (no
        // intermediate owned copy of the point bytes).
        let point_str = match self {
            EcdsaMaterial::P256(sk) => ssh_string(sk.verifying_key().to_encoded_point(false).as_bytes()),
            EcdsaMaterial::P384(sk) => ssh_string(sk.verifying_key().to_encoded_point(false).as_bytes()),
            // p521 0.13's newtype `SigningKey::verifying_key` is gated on a `verifying`
            // feature the crate never defines; `VerifyingKey::from(&SigningKey)` is the
            // supported path (the crate's own doctest uses it).
            EcdsaMaterial::P521(sk) => {
                ssh_string(p521::ecdsa::VerifyingKey::from(sk).to_encoded_point(false).as_bytes())
            }
        };
        let mut out = ssh_string(self.format().as_bytes());
        out.extend(ssh_string(self.curve_id().as_bytes()));
        out.extend(point_str);
        out
    }

    /// Sign `data`, hashing with the curve's canonical digest (P-256→SHA-256, etc.,
    /// via the RustCrypto `Signer` impl) and encoding the blob as x/crypto/ssh's
    /// `mpint(r) ‖ mpint(s)`.
    fn sign(&self, data: &[u8]) -> SshSignature {
        let blob = match self {
            EcdsaMaterial::P256(sk) => {
                let sig: p256::ecdsa::Signature = sk.sign(data);
                let (r, s) = sig.split_bytes();
                ecdsa_blob(&r, &s)
            }
            EcdsaMaterial::P384(sk) => {
                let sig: p384::ecdsa::Signature = sk.sign(data);
                let (r, s) = sig.split_bytes();
                ecdsa_blob(&r, &s)
            }
            EcdsaMaterial::P521(sk) => {
                let sig: p521::ecdsa::Signature = sk.sign(data);
                let (r, s) = sig.split_bytes();
                ecdsa_blob(&r, &s)
            }
        };
        SshSignature {
            format: self.format().to_string(),
            blob,
        }
    }
}

impl KeyMaterial {
    /// The SSH key-format name for the error message on an rsa-sha2-on-non-RSA sign.
    /// Also the `LoadedKey::format` for a legacy-PEM key (Go's `pubKey.Type()`).
    fn key_format(&self) -> &str {
        match self {
            KeyMaterial::Ed25519(_) => "ssh-ed25519",
            KeyMaterial::Rsa(_) => "ssh-rsa",
            KeyMaterial::Ecdsa(m) => m.format(),
        }
    }

    /// The SSH-wire marshaled public key (Go `ssh.PublicKey.Marshal()`), derived from
    /// the loaded private key. Used by the legacy-PEM `load_key` path, which has no
    /// ssh-key `PrivateKey` to call `public_key().to_bytes()` on. Produces the same
    /// bytes as that OpenSSH path for the same key (round-trip-checked in tests), so
    /// `keysEqual`/`list` are identical regardless of the on-disk encoding.
    fn marshaled_pub(&self) -> Vec<u8> {
        match self {
            KeyMaterial::Ed25519(sk) => {
                // string("ssh-ed25519") ‖ string(32-byte public key).
                let mut out = ssh_string(b"ssh-ed25519");
                out.extend(ssh_string(sk.verifying_key().as_bytes()));
                out
            }
            KeyMaterial::Rsa(sk) => {
                // string("ssh-rsa") ‖ mpint(e) ‖ mpint(n).
                let pk = sk.to_public_key();
                let mut out = ssh_string(b"ssh-rsa");
                out.extend(ssh_mpint(&rsa::traits::PublicKeyParts::e(&pk).to_bytes_be()));
                out.extend(ssh_mpint(&rsa::traits::PublicKeyParts::n(&pk).to_bytes_be()));
                out
            }
            KeyMaterial::Ecdsa(m) => m.marshaled_pub(),
        }
    }

    /// Sign `data` under `flags`, dispatching on the key type + rsa-sha2 override.
    fn sign(&self, data: &[u8], flags: u32) -> Result<SshSignature, String> {
        match (self, rsa_algo_from_flags(flags)) {
            // RSA: the flag selects the digest; no flag → the ssh-rsa SHA-1 default.
            // (Matched first so an rsa-sha2 flag on an RSA key selects the digest rather
            // than falling into the non-RSA error arm below.)
            (KeyMaterial::Rsa(sk), rsa_algo) => {
                rsa_sign(sk, data, rsa_algo.unwrap_or(RsaHash::Sha1))
            }
            // ed25519 / ecdsa with an rsa-sha2 flag is a sign ERROR (Go's
            // SignWithAlgorithm rejects rsa-sha2-* for a non-RSA key format), which the
            // handler maps to SIGN_FAILED + audits result:error.
            (_, Some(algo)) => Err(unsupported_algo_err(self.key_format(), algo)),
            // Deterministic (RFC 8032) — byte-identical to Go's ssh ed25519 signer.
            (KeyMaterial::Ed25519(sk), None) => {
                let sig: ed25519_dalek::Signature = sk.sign(data);
                Ok(SshSignature {
                    format: "ssh-ed25519".to_string(),
                    blob: sig.to_bytes().to_vec(),
                })
            }
            (KeyMaterial::Ecdsa(m), None) => Ok(m.sign(data)),
        }
    }
}

/// The error string for an rsa-sha2 `flags` bit on a non-RSA key. Byte-identical to
/// x/crypto/ssh's `algorithmSigner.SignWithAlgorithm` error
/// (`golang.org/x/crypto@v0.53.0/ssh/keys.go:1216`:
/// `fmt.Errorf("ssh: unsupported signature algorithm %q for key format %q", ...)`).
/// The `%q` verb double-quotes each value — Rust's `{:?}` on a `&str` produces the
/// same for these ASCII algorithm/format names. The handler still maps any sign error
/// to SIGN_FAILED and audits `result:error` with `detail = key type` (not this text),
/// but matching Go's wording keeps the operational log identical across the two ports.
fn unsupported_algo_err(key_format: &str, algo: RsaHash) -> String {
    format!(
        "ssh: unsupported signature algorithm {:?} for key format {:?}",
        algo.ssh_format(),
        key_format
    )
}

/// Sign `data` with `key` using PKCS#1 v1.5 over `hash`, returning the raw signature
/// bytes as the blob (byte-identical to Go's `rsa.SignPKCS1v15`).
fn rsa_sign(key: &rsa::RsaPrivateKey, data: &[u8], hash: RsaHash) -> Result<SshSignature, String> {
    use rsa::Pkcs1v15Sign;
    use sha2::Digest as _; // brings the `digest::Digest` trait into scope (covers Sha1 too).
    // The signature `format` name (`ssh-rsa`/`rsa-sha2-256`/`rsa-sha2-512`) is owned by
    // `RsaHash::ssh_format()`; only the blob differs per digest.
    let blob = match hash {
        RsaHash::Sha1 => key.sign(Pkcs1v15Sign::new::<sha1::Sha1>(), &sha1::Sha1::digest(data)),
        RsaHash::Sha256 => key.sign(
            Pkcs1v15Sign::new::<sha2::Sha256>(),
            &sha2::Sha256::digest(data),
        ),
        RsaHash::Sha512 => key.sign(
            Pkcs1v15Sign::new::<sha2::Sha512>(),
            &sha2::Sha512::digest(data),
        ),
    };
    match blob {
        Ok(blob) => Ok(SshSignature {
            format: hash.ssh_format().to_string(),
            blob,
        }),
        Err(e) => Err(format!("rsa sign failed: {e}")),
    }
}

/// Encode an ECDSA signature `{r, s}` as the x/crypto/ssh blob `mpint(r) ‖ mpint(s)`
/// (`ssh.Marshal(struct{ R, S *big.Int }{})`). `r`/`s` are the curve's fixed-width
/// big-endian scalar bytes.
fn ecdsa_blob(r: &[u8], s: &[u8]) -> Vec<u8> {
    let mut out = ssh_mpint(r);
    out.extend(ssh_mpint(s));
    out
}

/// SSH `string` wire encoding (RFC 4251): a `uint32` big-endian length prefix followed
/// by the raw bytes. Used to build the marshaled public-key blob for a legacy-PEM key
/// by hand (the OpenSSH path gets it from ssh-key's `public_key().to_bytes()`).
fn ssh_string(bytes: &[u8]) -> Vec<u8> {
    let mut out = Vec::with_capacity(4 + bytes.len());
    out.extend_from_slice(&(bytes.len() as u32).to_be_bytes());
    out.extend_from_slice(bytes);
    out
}

/// SSH `mpint` wire encoding (RFC 4251): a `uint32` length prefix followed by the
/// minimal two's-complement big-endian bytes — leading zero bytes stripped, one `0x00`
/// prepended when the top bit is set (to keep the value positive). Zero is the empty
/// string. Inputs here are positive non-zero ECDSA scalars.
fn ssh_mpint(scalar_be: &[u8]) -> Vec<u8> {
    let start = scalar_be.iter().position(|&b| b != 0).unwrap_or(scalar_be.len());
    let trimmed = &scalar_be[start..];
    let mut body = Vec::with_capacity(trimmed.len() + 1);
    if let Some(&first) = trimmed.first() {
        if first & 0x80 != 0 {
            body.push(0);
        }
        body.extend_from_slice(trimmed);
    }
    let mut out = Vec::with_capacity(4 + body.len());
    out.extend_from_slice(&(body.len() as u32).to_be_bytes());
    out.extend(body);
    out
}

/// One loaded key: its SSH-wire marshaled public key (for `keysEqual` matching +
/// `list`), the public-key algorithm name, the filename comment, and the signable
/// material.
struct LoadedKey {
    marshaled_pub: Vec<u8>,
    format: String,
    comment: String,
    material: KeyMaterial,
}

/// The local-keys backend (Go `localKeysBackend`). Load with
/// [`LocalKeysBackend::load`] / [`load_from_ssh_dir`](Self::load_from_ssh_dir), which
/// read the standard key files. A missing file is a SILENT skip; an
/// encrypted/unparsable/unsupported key is skipped with a warning (returned to the
/// caller to log). Never fails to construct — an empty backend still lets the daemon
/// start, matching Go's per-file `continue`.
pub struct LocalKeysBackend {
    keys: Vec<LoadedKey>,
}

impl LocalKeysBackend {
    /// Load the standard key files from an explicit `.ssh` directory. Returns the
    /// backend + any per-key warnings for the caller to log. Production goes through
    /// [`resolve_ssh_backend_from_env`], which resolves `$HOME/.ssh`.
    ///
    /// **Go divergence (accepted):** Go's `newLocalKeysBackend` errors (→ daemon
    /// `os.Exit(1)`) if `os.UserHomeDir()` fails; the crate's `user_home_dir()` is
    /// infallible (falls back to `/tmp`, consistent with every other consumer in this
    /// crate), so there is no home-unresolvable failure path to mirror.
    pub fn load_from_ssh_dir(ssh_dir: &Path) -> (LocalKeysBackend, Vec<String>) {
        let mut keys = Vec::new();
        let mut warnings = Vec::new();
        for name in STANDARD_KEY_FILES {
            let key_path: PathBuf = ssh_dir.join(name);
            match load_key(&key_path, name) {
                Ok(Some(loaded)) => keys.push(loaded),
                Ok(None) => {} // missing/unreadable file → silent skip (Go `continue`).
                Err(warning) => warnings.push(warning),
            }
        }
        if keys.is_empty() {
            // Go `newLocalKeysBackend` warns when nothing loaded (`ssh_backend_localkeys.go:67`).
            warnings.push("no SSH keys found in ~/.ssh/".to_string());
        }
        (LocalKeysBackend { keys }, warnings)
    }
}

/// Load one private-key file into a [`LoadedKey`], dispatching on the PEM label the
/// way Go's `ssh.ParsePrivateKey` (`ParseRawPrivateKey`) does.
///
/// - Missing/unreadable file → `Ok(None)` (SILENT skip; Go's `os.ReadFile` `continue`).
/// - Unparsable or **encrypted** key → `Err(warning)` (Go's `ssh.ParsePrivateKey`
///   failure → `logger.Warn("skipping SSH key (encrypted or invalid)")`).
/// - Parsed but an unsupported key type (DSA, FIDO `sk-*`) → `Err(warning)` (see the
///   module-doc "Accepted divergences": these narrow cases warn+skip where Go loads).
///
/// Accepted labels → parser:
/// - `OPENSSH PRIVATE KEY` → ssh-key's `PrivateKey::from_openssh` (all algos, encrypted
///   detection, marshaled pubkey from ssh-key).
/// - `RSA PRIVATE KEY` → PKCS#1 (`rsa::pkcs1::DecodeRsaPrivateKey`).
/// - `PRIVATE KEY` → PKCS#8 (RSA, then EC p256/p384/p521, then ed25519).
/// - `EC PRIVATE KEY` → SEC1 (`elliptic_curve::SecretKey::from_sec1_pem`, per curve).
/// - `DSA PRIVATE KEY` / `ENCRYPTED PRIVATE KEY` / legacy `Proc-Type: 4,ENCRYPTED` →
///   warn+skip (unsupported / passphrase-protected — Go skips encrypted too).
fn load_key(path: &Path, comment: &str) -> Result<Option<LoadedKey>, String> {
    let data = match std::fs::read_to_string(path) {
        Ok(d) => d,
        Err(_) => return Ok(None),
    };
    // The OpenSSH path keeps its own marshaling (ssh-key's `public_key().to_bytes()`,
    // pinned against the Go RSA/ed25519 goldens); the legacy-PEM paths build the
    // marshaled pubkey by hand from the loaded material (`KeyMaterial::marshaled_pub`).
    let material = match pem_label(&data).as_deref() {
        Some("OPENSSH PRIVATE KEY") => return load_openssh_key(&data, path, comment).map(Some),
        Some("RSA PRIVATE KEY") => load_pkcs1_rsa(&data),
        Some("PRIVATE KEY") => load_pkcs8(&data),
        Some("EC PRIVATE KEY") => load_sec1_ec(&data),
        Some("ENCRYPTED PRIVATE KEY") => {
            Err("encrypted or invalid: key is passphrase-protected".to_string())
        }
        Some("DSA PRIVATE KEY") => Err("unsupported key type: DSA/ssh-dss".to_string()),
        Some(other) => Err(format!("encrypted or invalid: unsupported PEM type {other:?}")),
        None => Err("encrypted or invalid: not a PEM private key".to_string()),
    }
    .map_err(|reason| skip_warning(path, &reason))?;
    Ok(Some(LoadedKey {
        marshaled_pub: material.marshaled_pub(),
        format: material.key_format().to_string(),
        comment: comment.to_string(),
        material,
    }))
}

/// Load an OpenSSH v1 (`-----BEGIN OPENSSH PRIVATE KEY-----`) private key — the
/// original path, unchanged (marshaled pubkey from ssh-key, Go-golden-pinned).
fn load_openssh_key(data: &str, path: &Path, comment: &str) -> Result<LoadedKey, String> {
    let private = PrivateKey::from_openssh(data)
        .map_err(|e| skip_warning(path, &format!("encrypted or invalid: {e}")))?;
    if private.is_encrypted() {
        return Err(skip_warning(path, "encrypted or invalid: key is passphrase-protected"));
    }
    let material = key_material(&private)
        .ok_or_else(|| skip_warning(path, "unsupported key type"))?;
    let marshaled_pub = private
        .public_key()
        .to_bytes()
        .map_err(|e| skip_warning(path, &format!("cannot marshal public key: {e}")))?;
    let format = private.algorithm().as_str().to_string();
    Ok(LoadedKey {
        marshaled_pub,
        format,
        comment: comment.to_string(),
        material,
    })
}

/// The PEM `-----BEGIN <label>-----` label of the first PEM block, if any (leading
/// comments/blank lines tolerated, matching how OpenSSL/OpenSSH readers scan).
fn pem_label(data: &str) -> Option<String> {
    data.lines().find_map(|line| {
        line.trim()
            .strip_prefix("-----BEGIN ")
            .and_then(|rest| rest.strip_suffix("-----"))
            .map(|label| label.trim().to_string())
    })
}

/// A legacy OpenSSL PEM with a `Proc-Type: 4,ENCRYPTED` header is passphrase-encrypted
/// (`RSA`/`EC`/`DSA PRIVATE KEY` bodies); the RustCrypto decoders don't handle that
/// encryption, so detect it up front and warn+skip (Go skips encrypted keys too).
fn is_legacy_encrypted(pem: &str) -> bool {
    pem.contains("Proc-Type:") && pem.contains("ENCRYPTED")
}

/// PKCS#1 (`-----BEGIN RSA PRIVATE KEY-----`).
fn load_pkcs1_rsa(pem: &str) -> Result<KeyMaterial, String> {
    use rsa::pkcs1::DecodeRsaPrivateKey as _;
    if is_legacy_encrypted(pem) {
        return Err("encrypted or invalid: key is passphrase-protected".to_string());
    }
    rsa::RsaPrivateKey::from_pkcs1_pem(pem)
        .map(|k| KeyMaterial::Rsa(Box::new(k)))
        .map_err(|e| format!("encrypted or invalid: {e}"))
}

/// PKCS#8 (`-----BEGIN PRIVATE KEY-----`): try RSA, then EC (p256/p384/p521 by OID),
/// then ed25519 — the algorithm OID inside the key selects exactly one.
fn load_pkcs8(pem: &str) -> Result<KeyMaterial, String> {
    // `ed25519_dalek::pkcs8::DecodePrivateKey` and `rsa::pkcs8::DecodePrivateKey` are
    // the same re-exported `pkcs8` trait, so this one import covers both RSA + ed25519.
    use ed25519_dalek::pkcs8::DecodePrivateKey as _;
    if let Ok(k) = rsa::RsaPrivateKey::from_pkcs8_pem(pem) {
        return Ok(KeyMaterial::Rsa(Box::new(k)));
    }
    if let Some(m) = ec_from_pkcs8(pem) {
        return Ok(KeyMaterial::Ecdsa(m));
    }
    if let Ok(sk) = ed25519_dalek::SigningKey::from_pkcs8_pem(pem) {
        return Ok(KeyMaterial::Ed25519(sk));
    }
    Err("encrypted or invalid: unsupported PKCS#8 private key".to_string())
}

/// SEC1 (`-----BEGIN EC PRIVATE KEY-----`): try each NIST curve; the SEC1 body carries
/// the curve OID so exactly one parses.
fn load_sec1_ec(pem: &str) -> Result<KeyMaterial, String> {
    if is_legacy_encrypted(pem) {
        return Err("encrypted or invalid: key is passphrase-protected".to_string());
    }
    if let Ok(sk) = p256::SecretKey::from_sec1_pem(pem) {
        return Ok(KeyMaterial::Ecdsa(EcdsaMaterial::P256(sk.into())));
    }
    if let Ok(sk) = p384::SecretKey::from_sec1_pem(pem) {
        return Ok(KeyMaterial::Ecdsa(EcdsaMaterial::P384(sk.into())));
    }
    if let Ok(sk) = p521::SecretKey::from_sec1_pem(pem) {
        return Ok(KeyMaterial::Ecdsa(EcdsaMaterial::P521(p521_signing_key(&sk))));
    }
    Err("encrypted or invalid: unsupported SEC1 EC private key".to_string())
}

/// PKCS#8 EC helper: `SecretKey::from_pkcs8_pem` per curve, converted to the ECDSA
/// signing key (`SigningKey: From<SecretKey>`).
fn ec_from_pkcs8(pem: &str) -> Option<EcdsaMaterial> {
    use p256::pkcs8::DecodePrivateKey as _;
    if let Ok(sk) = p256::SecretKey::from_pkcs8_pem(pem) {
        return Some(EcdsaMaterial::P256(sk.into()));
    }
    if let Ok(sk) = p384::SecretKey::from_pkcs8_pem(pem) {
        return Some(EcdsaMaterial::P384(sk.into()));
    }
    if let Ok(sk) = p521::SecretKey::from_pkcs8_pem(pem) {
        return Some(EcdsaMaterial::P521(p521_signing_key(&sk)));
    }
    None
}

/// Convert a `p521::SecretKey` to its ECDSA `SigningKey`. Unlike p256/p384, p521 0.13
/// does not impl `From<SecretKey>` for its `ecdsa::SigningKey`, so go via the raw
/// fixed-width scalar bytes (which a valid `SecretKey` always yields).
fn p521_signing_key(sk: &p521::SecretKey) -> p521::ecdsa::SigningKey {
    p521::ecdsa::SigningKey::from_slice(&sk.to_bytes())
        .expect("valid p521 secret scalar yields a valid signing key")
}

/// The warn message for a skipped key (mirrors Go's `logger.Warn` shape).
fn skip_warning(path: &Path, reason: &str) -> String {
    format!(
        "skipping SSH key ({reason}) path={}",
        path.display()
    )
}

/// Extract the signable material from a parsed (non-encrypted) private key. `None`
/// for a key type this backend does not sign (DSA, FIDO `sk-*`, …).
fn key_material(private: &PrivateKey) -> Option<KeyMaterial> {
    match private.key_data() {
        KeypairData::Ed25519(kp) => {
            let seed = kp.private.to_bytes();
            Some(KeyMaterial::Ed25519(ed25519_dalek::SigningKey::from_bytes(
                &seed,
            )))
        }
        KeypairData::Rsa(kp) => rsa_private_from_keypair(kp).map(|k| KeyMaterial::Rsa(Box::new(k))),
        KeypairData::Ecdsa(kp) => ecdsa_material_from_keypair(kp).map(KeyMaterial::Ecdsa),
        _ => None,
    }
}

/// Build a `rsa::RsaPrivateKey` from ssh-key's `RsaKeypair`. Done by hand rather than
/// via ssh-key 0.6.7's `TryFrom<&RsaKeypair>` because that impl passes the first prime
/// twice (`[p, p]`) instead of `[p, q]` — this uses the correct primes.
fn rsa_private_from_keypair(kp: &ssh_key::private::RsaKeypair) -> Option<rsa::RsaPrivateKey> {
    let n = rsa::BigUint::try_from(&kp.public.n).ok()?;
    let e = rsa::BigUint::try_from(&kp.public.e).ok()?;
    let d = rsa::BigUint::try_from(&kp.private.d).ok()?;
    let p = rsa::BigUint::try_from(&kp.private.p).ok()?;
    let q = rsa::BigUint::try_from(&kp.private.q).ok()?;
    rsa::RsaPrivateKey::from_components(n, e, d, vec![p, q]).ok()
}

/// Build the curve-specific ECDSA signing key from ssh-key's `EcdsaKeypair` (its raw
/// private scalar bytes).
fn ecdsa_material_from_keypair(kp: &ssh_key::private::EcdsaKeypair) -> Option<EcdsaMaterial> {
    use ssh_key::private::EcdsaKeypair;
    match kp {
        EcdsaKeypair::NistP256 { .. } => {
            p256::ecdsa::SigningKey::from_slice(kp.private_key_bytes())
                .ok()
                .map(EcdsaMaterial::P256)
        }
        EcdsaKeypair::NistP384 { .. } => {
            p384::ecdsa::SigningKey::from_slice(kp.private_key_bytes())
                .ok()
                .map(EcdsaMaterial::P384)
        }
        EcdsaKeypair::NistP521 { .. } => {
            p521::ecdsa::SigningKey::from_slice(kp.private_key_bytes())
                .ok()
                .map(EcdsaMaterial::P521)
        }
    }
}

impl SshBackend for LocalKeysBackend {
    fn list(&self) -> Result<Vec<SshKeyInfo>, String> {
        Ok(self
            .keys
            .iter()
            .map(|k| SshKeyInfo {
                format: k.format.clone(),
                blob: k.marshaled_pub.clone(),
                comment: k.comment.clone(),
            })
            .collect())
    }

    fn sign(&self, public_key: &[u8], data: &[u8], flags: u32) -> Result<SshSignature, String> {
        for k in &self.keys {
            if k.marshaled_pub == public_key {
                return k.material.sign(data, flags);
            }
        }
        Err("key not found".to_string())
    }

    fn mode(&self) -> &str {
        "local-keys"
    }
}

/// Resolve the SSH backend from the configured `ssh.mode`, mirroring Go's
/// `ResolveSSHBackend` (`ssh_backend.go`). `ssh_dir` is the directory the local-keys
/// backend reads (`$HOME/.ssh` in production; a fixture dir in tests).
///
/// - `"local-keys"` → the local-keys backend.
/// - `""` (auto) → auto-detect. **This commit** selects local-keys unconditionally;
///   the `SSH_AUTH_SOCK` dial that picks agent-forward lands in commit 2.
/// - `"agent-forward"` → `Err` (**commit-2 wires this** — until the agent-forward
///   backend exists, an explicit request is a fatal startup error rather than a
///   silently-wrong backend).
/// - anything else → `Err` with Go's exact string.
///
/// Returns the backend plus any load warnings for the caller to log.
pub fn resolve_ssh_backend(
    mode: &str,
    ssh_dir: &Path,
) -> Result<(Arc<dyn SshBackend>, Vec<String>), String> {
    match mode {
        "local-keys" => {
            let (backend, warnings) = LocalKeysBackend::load_from_ssh_dir(ssh_dir);
            Ok((Arc::new(backend), warnings))
        }
        "" => {
            // Auto-detect. commit-2 wires this: SSH_AUTH_SOCK set AND a live UDS dial
            // → agent-forward; else local-keys. Until then, always local-keys.
            let (backend, warnings) = LocalKeysBackend::load_from_ssh_dir(ssh_dir);
            Ok((Arc::new(backend), warnings))
        }
        "agent-forward" => {
            // commit-2 wires this.
            Err("agent-forward backend not yet implemented".to_string())
        }
        other => Err(format!(
            "unknown ssh mode: \"{other}\" (expected agent-forward, local-keys, or empty)"
        )),
    }
}

/// Production entry point: resolve from `$HOME/.ssh` (Go's implicit `~/.ssh`).
pub fn resolve_ssh_backend_from_env(
    mode: &str,
) -> Result<(Arc<dyn SshBackend>, Vec<String>), String> {
    resolve_ssh_backend(mode, &user_home_dir().join(".ssh"))
}

#[cfg(test)]
mod tests {
    use super::*;
    use base64::Engine as _;
    use ssh_key::private::{Ed25519Keypair, Ed25519PrivateKey, KeypairData};
    use ssh_key::public::{Ed25519PublicKey, PublicKey};
    use ssh_key::LineEnding;

    // ---- ed25519 Go-reference vector (unchanged from the original sliver) ----

    /// The fixed seed used to generate the Go reference vector (bytes 1..=32).
    const SEED: [u8; 32] = [
        1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 21, 22, 23, 24, 25,
        26, 27, 28, 29, 30, 31, 32,
    ];
    const CHALLENGE: &[u8] = b"shed-host-agent ed25519 determinism vector";
    /// Generated by Go `x/crypto/ssh` `NewSignerFromKey(ed25519.NewKeyFromSeed(SEED))`
    /// `.Sign(_, CHALLENGE).Blob` → base64(StdEncoding). See the module doc.
    const GO_BLOB_B64: &str =
        "oqa1xw8Qs9MlHx1SGmULiUqv85QhOIcha2Gls77unKYh+Q2cAsodu1ze09oj1h7++tcYTETiKQeL8/2xKWLBBg==";
    /// Go `signer.PublicKey().Marshal()` → base64(StdEncoding), for the same seed.
    const GO_PUB_MARSHAL_B64: &str =
        "AAAAC3NzaC1lZDI1NTE5AAAAIHm1Vi6P5lT5QHixEuipi6eQH4U65pW+1+DjkQutBJZk";

    // ---- Throwaway test-only OpenSSH keys (generated once with ssh-keygen; NOT
    //      used anywhere but these unit tests) ----

    /// Go `x/crypto/ssh` reference vectors over `RSA_KEY` + `CHALLENGE`, produced by a
    /// standalone Go program (`signer.Sign` / `SignWithAlgorithm`). RSA PKCS#1 v1.5 is
    /// deterministic, so these are exact goldens (verified byte-equal to the Rust
    /// output once during implementation). `GO_RSA_PUB_MARSHAL_B64` =
    /// `signer.PublicKey().Marshal()`.
    const GO_RSA_PUB_MARSHAL_B64: &str = "AAAAB3NzaC1yc2EAAAADAQABAAABAQDD8PdTw3hl5nXiaDW7FueLtNxb8XM3oTLrLIpQmEWXrxXya2utxi3Nqw/uF74fbU5hZQx4u8nfuJRMarNqZny5/r4Oy0CJN7+cwa5gTraQJ267lE6SFQy9V3rWs5rExIzP+A2qlGhu9My/SW4IDQt1YZR0YF3oqH34lpk7ffjEiorZ1MkHtzA6YJPfhN7efZMM/rW2fpH0LlZoYN/rjh6oNYGPDipwfwtv0ewNLkVXN6xROh1ka9bx/u4q9fRQxxwmJGwwkjhElWlBSn5lBKoqskqwJP2SxsKDkZLi/rfbWaYysvpjPFfKVj5WUssVfpm1czYd+GBMZFq6C/d4DzIH";
    const GO_RSA_SHA1_B64: &str = "JpijxtBrt8+qp8Xgjl7ERfnvuoRXEgcbh4X5ZIfoLXDhT9cTrNWqp7qQr9L1VqUxDhRiN+zvXpE0YN8eoyTWEzxJdlv9NtKcyn2myGkiACIsKb8rUoY2mKOCiqXzrSnKBah+SF1mc6vgfPv34g25QZ1OBVZG7J+HxZgBFHCmUyZKqeOxTEFKMH3CQSNiVUlAREucXssQS78dtQSL4gXn+MEAtt//VcBSHDPl8kaBb58VXTyz4yWeuW+vMMK5P9z/Tzwb2DxHNO6IJ7c2lvFdDhOkjxnd+aszqlWzJnhJrqMUKYfXIul2frFwdHrlPPsXPOLIPijhF6XX1/I2xwHJUA==";
    const GO_RSA_SHA256_B64: &str = "DFQjbOi8QP+Q+WQ2uIAQ2EEyD41H8n5sNktIR2syQAubcwvZGCs/2glJX/hkg8EokTkDUDK1edhZ9Lwa3KwUpoSLwNpqpW+Cv0guH4biKzI9WTepsOnGFegn5CD94ZAgNpHQk7YaGfqxdwnqrancuLhDAfG/FRoVp48ykyisLDCA3TH346o/Y2/Zd58WIn10xxvyRz2+KP4wLpusxqeowNyElFUsgvRaRk3WgxhYM4QEvMtf1nEkA7kA33XkGH0zqRNKutbsQIpAFKOgcFbiq7LNnCNN11cs/Zs59EHqJ/Iln/LYJQcQtE23MsLcDMp/aHe7WLda++/iN5pfUi7tXQ==";
    const GO_RSA_SHA512_B64: &str = "ONdKN4O2ZzolGSa661Vw2+6Pv8u/HQ2sW7GbSoICjWpYgjT8j8YiaqjqDUIoqTIqafwLGoEgKXfsTra7MoBhq1ZjC9c+M2wZyBAmcQiKNurToR8mjjs5+hyo0thBNfQfWY3C2F5ZLyrHtE20kiez4CEWqBhSwwzCD1xWEhvtyBkDLbLUKIEIse3KOd+5mzu8BJifqILwHo9FPjnQGcYiMq9bPrsl8Xse3OfMIGY+vuGQtRXciIO4iUVeR1eS+ZdHfcnJBOmsq6DxzLhhaO7XCJ0tCe/a85Lgqg43vPfjPwuzDgfBfbpNO/07zl7JXvm2qLybRHH8anFTntRxOYQvYw==";

    const RSA_KEY: &str = "-----BEGIN OPENSSH PRIVATE KEY-----\n\
b3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQAAAAAAAAABAAABFwAAAAdzc2gtcn\n\
NhAAAAAwEAAQAAAQEAw/D3U8N4ZeZ14mg1uxbni7TcW/FzN6Ey6yyKUJhFl68V8mtrrcYt\n\
zasP7he+H21OYWUMeLvJ37iUTGqzamZ8uf6+DstAiTe/nMGuYE62kCduu5ROkhUMvVd61r\n\
OaxMSMz/gNqpRobvTMv0luCA0LdWGUdGBd6Kh9+JaZO334xIqK2dTJB7cwOmCT34Te3n2T\n\
DP61tn6R9C5WaGDf644eqDWBjw4qcH8Lb9HsDS5FVzesUTodZGvW8f7uKvX0UMccJiRsMJ\n\
I4RJVpQUp+ZQSqKrJKsCT9ksbCg5GS4v6321mmMrL6YzxXylY+VlLLFX6ZtXM2HfhgTGRa\n\
ugv3eA8yBwAAA8CwAqRwsAKkcAAAAAdzc2gtcnNhAAABAQDD8PdTw3hl5nXiaDW7FueLtN\n\
xb8XM3oTLrLIpQmEWXrxXya2utxi3Nqw/uF74fbU5hZQx4u8nfuJRMarNqZny5/r4Oy0CJ\n\
N7+cwa5gTraQJ267lE6SFQy9V3rWs5rExIzP+A2qlGhu9My/SW4IDQt1YZR0YF3oqH34lp\n\
k7ffjEiorZ1MkHtzA6YJPfhN7efZMM/rW2fpH0LlZoYN/rjh6oNYGPDipwfwtv0ewNLkVX\n\
N6xROh1ka9bx/u4q9fRQxxwmJGwwkjhElWlBSn5lBKoqskqwJP2SxsKDkZLi/rfbWaYysv\n\
pjPFfKVj5WUssVfpm1czYd+GBMZFq6C/d4DzIHAAAAAwEAAQAAAQBaOoV6EiJIMmcImlpb\n\
zAFWKTPsNvSKonWTLFCJKoWpgtvFZUgRnpgLBIHybwaC7E/Ss7iZhEhC+Hl58wypq4Y2FC\n\
OrJleSmJRo+Bt3h+ez3CS2xmWkCYNzUWxkoBJeF/CL+Ds62Np6dcovL/42QOOM6yF0sces\n\
0qInrhnj9m9u+VqEC46KJ074jhst0oPyQ4Cp6C3cgKzhqmUZUeIe9ZT3uKw9sCAmRnF+UW\n\
OeoOnKN3b6CqMKeU1bcmGPFzcIzCWWXu+6jDsWy1wq5FlDX/9anVX4OKXEfRlr6zK7D0Ax\n\
0y7mbZaW5jnTIS33RADrkHrH7k55FI6RK9FXHXmRg4MxAAAAgQCauTNTYBcSsijeSbnUTm\n\
/obOycgwrE7r7z+sBj84bpR7B24kbW5aFQKIawDNYF77kKQ++ZCI5vrJONVl9qzj23G/DF\n\
GlqofuqZf60Od/PWjbTd20W+sp1wA9uaeq9UL/BTsVGDZ5WfMtcFIOSH8fyBSa/OP4Jj2p\n\
gUkgjeosa9ggAAAIEA8OBOgdDR1CcY373pB/ADLgCpTncPqxWbBQuDIXTIR4cyapp2MFC7\n\
viZW6+oy0gmgcF24Zc4oSnt/UDq1n3S64nyRygyVX3jnvNRAc76qCJJTfUC9Uq1hkysGvq\n\
1RPxGL7S5nXQraKlpMTmNNSN8kJwMlFqRya0Up/i8zInClCH8AAACBANA+Z1Hc2mhM0VaI\n\
l3Bixbr5gwRRB8HW55zT0/fZ0/tOIFAeEARz311dIsw45aVxfvamzP35w2Sn1hKhmfeiZc\n\
ysCWytqZ4T+Qr2tZpxV+KX1H7JgvtlW7O38196Dpac5r/Diumb94bO/o18ka8bReiPd0Tq\n\
BVYu4aUWUB5r6tJ5AAAABmlkX3JzYQECAwQ=\n\
-----END OPENSSH PRIVATE KEY-----\n";

    const ECDSA_P256_KEY: &str = "-----BEGIN OPENSSH PRIVATE KEY-----\n\
b3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQAAAAAAAAABAAAAaAAAABNlY2RzYS\n\
1zaGEyLW5pc3RwMjU2AAAACG5pc3RwMjU2AAAAQQQJm5soc637UilmPbW/J1SI7G+bi4j3\n\
sOLVRJ6pQ36zQCLdDRyPBpTDVZ8K9FVdjZLgSHdtQUy/45/lM9+F9Az1AAAAqLzloHm85a\n\
B5AAAAE2VjZHNhLXNoYTItbmlzdHAyNTYAAAAIbmlzdHAyNTYAAABBBAmbmyhzrftSKWY9\n\
tb8nVIjsb5uLiPew4tVEnqlDfrNAIt0NHI8GlMNVnwr0VV2NkuBId21BTL/jn+Uz34X0DP\n\
UAAAAhAOOB5/+HEAzaqLen//yKUhWrRk87tdTlWrlxmD45WP8IAAAACGlkX2VjZHNhAQID\n\
BAUGBw==\n\
-----END OPENSSH PRIVATE KEY-----\n";

    const ECDSA_P384_KEY: &str = "-----BEGIN OPENSSH PRIVATE KEY-----\n\
b3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQAAAAAAAAABAAAAiAAAABNlY2RzYS\n\
1zaGEyLW5pc3RwMzg0AAAACG5pc3RwMzg0AAAAYQQk/PZF4sszye9/6m1xbt3ZzFH4zcwd\n\
mNIXfv7LcfKGIWdleLy3Ig7zkF67sxP66ak6ZL3fGyikTnxfyQFuhG5HPdl/asPSEvU/uy\n\
6AMhddW6uqfM+K+7cOVbxnNlE4/ksAAADYp2A/c6dgP3MAAAATZWNkc2Etc2hhMi1uaXN0\n\
cDM4NAAAAAhuaXN0cDM4NAAAAGEEJPz2ReLLM8nvf+ptcW7d2cxR+M3MHZjSF37+y3Hyhi\n\
FnZXi8tyIO85Beu7MT+umpOmS93xsopE58X8kBboRuRz3Zf2rD0hL1P7sugDIXXVurqnzP\n\
ivu3DlW8ZzZROP5LAAAAMQD+d1loZFbnjEXNFfJyVaoLxTO8vGGymbiRcvFm74yVEHtOdX\n\
FeFTU/CJ+ImAsdIWkAAAAIaWRfZWNkc2EBAgMEBQYH\n\
-----END OPENSSH PRIVATE KEY-----\n";

    // Note: ssh-key 0.6.7 only decodes a P-521 private scalar of exactly 66 (or 67
    // with sign pad) bytes. Because a P-521 scalar's top byte holds just 9 bits, an
    // OpenSSH mpint often strips a leading zero → 65 bytes → ssh-key rejects it
    // ("length invalid"). This fixture was chosen (of several generated) to have
    // bit-512 set so its scalar encodes to 66 bytes and parses. A real user's P-521
    // `id_ecdsa` may therefore warn+skip where Go would load it — a documented,
    // upstream-bounded divergence (the harness fixture curve is P-256).
    const ECDSA_P521_KEY: &str = "-----BEGIN OPENSSH PRIVATE KEY-----\n\
b3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQAAAAAAAAABAAAArAAAABNlY2RzYS\n\
1zaGEyLW5pc3RwNTIxAAAACG5pc3RwNTIxAAAAhQQBpEviJGFDw2q4lqMtycgmYjVBuC6y\n\
K9vdaA8vcYYUHR0R7P9JWhzQwlVslQRUC1qGOY+yt4EcqF+BDI9ApOmdoQMBu8XcQIoc/7\n\
MAL72x3Ujds3zAAzVF72UVjLjNX2RufrTfJK6oFk0wHedh998F9/QdT4LlAG5B5CHFh4em\n\
el4Nd1IAAAEIVT2gQ1U9oEMAAAATZWNkc2Etc2hhMi1uaXN0cDUyMQAAAAhuaXN0cDUyMQ\n\
AAAIUEAaRL4iRhQ8NquJajLcnIJmI1Qbgusivb3WgPL3GGFB0dEez/SVoc0MJVbJUEVAta\n\
hjmPsreBHKhfgQyPQKTpnaEDAbvF3ECKHP+zAC+9sd1I3bN8wAM1Re9lFYy4zV9kbn603y\n\
SuqBZNMB3nYfffBff0HU+C5QBuQeQhxYeHpnpeDXdSAAAAQgGdfsvRiYWc+9IdEiWzpFx1\n\
Sz4I2uVi4fUTL8a9o4rGwJ+eRUOiIKjkOxVtC5Deuv/V39DgcMy2YlIAHqe9cEkbagAAAA\n\
hpZF9lY2RzYQEC\n\
-----END OPENSSH PRIVATE KEY-----\n";

    /// An encrypted (passphrase-protected) ed25519 key — parses but is skipped+warned.
    const ENCRYPTED_ED25519_KEY: &str = "-----BEGIN OPENSSH PRIVATE KEY-----\n\
b3BlbnNzaC1rZXktdjEAAAAACmFlczI1Ni1jdHIAAAAGYmNyeXB0AAAAGAAAABB+4GYa47\n\
52LC/3HeT3Xhf0AAAAGAAAAAEAAAAzAAAAC3NzaC1lZDI1NTE5AAAAIBrZY3J7pNkotwHT\n\
6GYo4hNtsLvsnWM9hzpjcYR/FvS1AAAAkNYYwni7wSn2FwEs9mBr+8ZwfA5tUsczO/m6xL\n\
SRzbqlcHiIY4hsvvOD23m25dNk3EuqCNvvu1qMMcOdOO4SshlvDo4uR99xAU2I8eGmLdCj\n\
Qbeqbu/7TCjyNZ2o1/gm31rEmBBcyujJbr4/ECcYISVpaf1n8e757h3zUW538Pil9atcnw\n\
HcVUMC4Znkj1WtUg==\n\
-----END OPENSSH PRIVATE KEY-----\n";

    // ---- Throwaway legacy-PEM fixtures (generated once with openssl; NOT used
    //      anywhere but these unit tests). The RSA PKCS#1 and PKCS#8 fixtures are the
    //      SAME underlying key in two encodings (so they share one pubkey golden).
    //      `*_PUB_MARSHAL_B64` = `ssh-keygen -y` ssh-wire pubkey blob — an INDEPENDENT
    //      oracle that the hand-built `marshaled_pub` matches OpenSSH's marshaling. ----

    const RSA_PKCS1_KEY: &str = "-----BEGIN RSA PRIVATE KEY-----\n\
MIIEogIBAAKCAQEAtqDWuBOFPSjUaetZCytwVmU8K7FRGwbifw9t+QwetLmse/y0\n\
KQaGtK/V+JM1qpgnG9M+CsHe021Q7I7t6TaKZAd7LgJMqTD6AR6ICBTOcwBf0ZRe\n\
B8LDo5oXYZwTj0GuK+IXXW6vp8G9u6DyjE6OaRTdEeTlA/OBEaUn5ra4faA7aSfX\n\
zQnDi9Wv/bsDVHhS1yTHnZzuaSUFZBvlPoUZipK2QY9WZBRMQwVz6hSWEHzY7PM8\n\
zmN4daCjgOpfyum8r2eJBAqOObK6TsFqzyFatTJkqVHndrLW/HqYeliyCWdZU3+D\n\
6J2/Vbp5ReClJWd2WhtIB/fm9sEGeRXajXaYFwIDAQABAoIBAB0iCi6iGoqXlU7y\n\
Nqmj+88kZhVYO2Zy0jXPqczlRI6y4dODi9/RhTKUrC7zmMeGbxKuv4Jqy9dxZEvg\n\
Pw6JX0k2sk00G7OPtwnvq2aSnx5UTHS71MYrKRdTkPBGvA4JvbWNYwnKCuZZbyFb\n\
uuVr8KbNp7hfibL4KLo+XN+efU64uFBOU0M0Gv9gawXrRCZTRacB2LlYPM5P/KBd\n\
tzpEM6P21mbw/R9zMZXG0WccworrL85ctqvz5yAUYuK8szSblIzPEz0yCeFEp8U1\n\
Na7gf1F8Brgp85mIt1wJ9IA6zCKk+qQJXZI19eJ0NRfbi2VGbt3dl5A87YE5KS4I\n\
bp7l8gkCgYEA3WL42vm7xCWsAeCsRRvGGYc5f9d2P70Q2ImKfN6YwO04FAtW8KG2\n\
QSgTIQPmqpojBC+t3ctcptveGUqG3UxOaD281rPcvtTX13FDJXzszzs/pY7bm06O\n\
q24B8TTmPMNqosc2VnYSvSkITy6U/BQkUisnrulzdoH1EXLTL6dXPK8CgYEA0y6P\n\
Lu/+l+rMjuvFMckyPideVuPd+f5ADrnekGx5feqpBR7WfAHaxHLVQNd1aw9dbNPJ\n\
Tn3BBDc/iPLHH+Croma68UmMY7qlA4KDFc1F1xSjBhx3Qh4RwiuSlZAHAx26Rc8l\n\
yoSQkJGnY5KiLePB28FvJjrl53KKMxHmuiVxxRkCgYApHvIUUmCzDUBG1QGKkJ8a\n\
LMjcWxwGuMqBPgLwMLR02VsaNgT/Czp8HcJ31m6o75pjc6u6z8Q05g/56KLmRf8m\n\
U5lY0+3DsGsrBEmxk+O0lk+7I67cyRms8/D+aZH+ZVnQRGpuYt4WLqHxeziHHgKl\n\
FIj5bzlYIMlxZT+e0Vld1wKBgGvGKSCFLmMNWxPdUyfTTCbYJJcnd1Nr4/kf9muy\n\
UFZoeZW5ZTCoKaN0D00mKDBZCQ7PDr9WAjlKkMwtSl4EZNNepi0ZoeILkMc3xfpM\n\
ZkYbrA8kW+CMQ/faENbvSATZGQUjcF/oQ3bkPo7ceJP+1iJ2l2jlSgtSMyFZE20Q\n\
Sv2RAoGAYWwjfD7XIOFMmQvfaXw4Jc4ffRKKpu61uWr0NBfDUDDGbLr14Yed42S0\n\
D6wmqFwpiX8t6//GLEZfV1sGlfR71Uok18egBKLIQ5I7S2SG9v9eg57iVIX2ZhPo\n\
TurxNKFTUF4nHWfgLRF/yWRgAKVJDGCn12H4/cL1EhWD72o8U1g=\n\
-----END RSA PRIVATE KEY-----\n";

    const RSA_PKCS8_KEY: &str = "-----BEGIN PRIVATE KEY-----\n\
MIIEvAIBADANBgkqhkiG9w0BAQEFAASCBKYwggSiAgEAAoIBAQC2oNa4E4U9KNRp\n\
61kLK3BWZTwrsVEbBuJ/D235DB60uax7/LQpBoa0r9X4kzWqmCcb0z4Kwd7TbVDs\n\
ju3pNopkB3suAkypMPoBHogIFM5zAF/RlF4HwsOjmhdhnBOPQa4r4hddbq+nwb27\n\
oPKMTo5pFN0R5OUD84ERpSfmtrh9oDtpJ9fNCcOL1a/9uwNUeFLXJMednO5pJQVk\n\
G+U+hRmKkrZBj1ZkFExDBXPqFJYQfNjs8zzOY3h1oKOA6l/K6byvZ4kECo45srpO\n\
wWrPIVq1MmSpUed2stb8eph6WLIJZ1lTf4Ponb9VunlF4KUlZ3ZaG0gH9+b2wQZ5\n\
FdqNdpgXAgMBAAECggEAHSIKLqIaipeVTvI2qaP7zyRmFVg7ZnLSNc+pzOVEjrLh\n\
04OL39GFMpSsLvOYx4ZvEq6/gmrL13FkS+A/DolfSTayTTQbs4+3Ce+rZpKfHlRM\n\
dLvUxispF1OQ8Ea8Dgm9tY1jCcoK5llvIVu65Wvwps2nuF+Jsvgouj5c3559Tri4\n\
UE5TQzQa/2BrBetEJlNFpwHYuVg8zk/8oF23OkQzo/bWZvD9H3MxlcbRZxzCiusv\n\
zly2q/PnIBRi4ryzNJuUjM8TPTIJ4USnxTU1ruB/UXwGuCnzmYi3XAn0gDrMIqT6\n\
pAldkjX14nQ1F9uLZUZu3d2XkDztgTkpLghunuXyCQKBgQDdYvja+bvEJawB4KxF\n\
G8YZhzl/13Y/vRDYiYp83pjA7TgUC1bwobZBKBMhA+aqmiMEL63dy1ym294ZSobd\n\
TE5oPbzWs9y+1NfXcUMlfOzPOz+ljtubTo6rbgHxNOY8w2qixzZWdhK9KQhPLpT8\n\
FCRSKyeu6XN2gfURctMvp1c8rwKBgQDTLo8u7/6X6syO68UxyTI+J15W4935/kAO\n\
ud6QbHl96qkFHtZ8AdrEctVA13VrD11s08lOfcEENz+I8scf4KuiZrrxSYxjuqUD\n\
goMVzUXXFKMGHHdCHhHCK5KVkAcDHbpFzyXKhJCQkadjkqIt48HbwW8mOuXncooz\n\
Eea6JXHFGQKBgCke8hRSYLMNQEbVAYqQnxosyNxbHAa4yoE+AvAwtHTZWxo2BP8L\n\
OnwdwnfWbqjvmmNzq7rPxDTmD/noouZF/yZTmVjT7cOwaysESbGT47SWT7sjrtzJ\n\
Gazz8P5pkf5lWdBEam5i3hYuofF7OIceAqUUiPlvOVggyXFlP57RWV3XAoGAa8Yp\n\
IIUuYw1bE91TJ9NMJtgklyd3U2vj+R/2a7JQVmh5lbllMKgpo3QPTSYoMFkJDs8O\n\
v1YCOUqQzC1KXgRk016mLRmh4guQxzfF+kxmRhusDyRb4IxD99oQ1u9IBNkZBSNw\n\
X+hDduQ+jtx4k/7WInaXaOVKC1IzIVkTbRBK/ZECgYBhbCN8Ptcg4UyZC99pfDgl\n\
zh99Eoqm7rW5avQ0F8NQMMZsuvXhh53jZLQPrCaoXCmJfy3r/8YsRl9XWwaV9HvV\n\
SiTXx6AEoshDkjtLZIb2/16DnuJUhfZmE+hO6vE0oVNQXicdZ+AtEX/JZGAApUkM\n\
YKfXYfj9wvUSFYPvajxTWA==\n\
-----END PRIVATE KEY-----\n";

    const EC_SEC1_P256_KEY: &str = "-----BEGIN EC PRIVATE KEY-----\n\
MHcCAQEEINsESB648cmeHWoyRkTD+jkXzONtUOakvAdcqEwoVJi9oAoGCCqGSM49\n\
AwEHoUQDQgAElYDCa/fOKLkuE12oDl3jVnbZkRU+WLM1rQQvaiEb60Ot46xd5+Rk\n\
WV94/4qG0QihF7xuRvJiBCF8HJHarZNf8A==\n\
-----END EC PRIVATE KEY-----\n";

    const ED25519_PKCS8_KEY: &str = "-----BEGIN PRIVATE KEY-----\n\
MC4CAQAwBQYDK2VwBCIEICpYLybq3HO9QaetmuTi5lkFrMFonFea71bP97LPOtC/\n\
-----END PRIVATE KEY-----\n";

    /// An encrypted (passphrase-protected) legacy PKCS#1 RSA key — the `Proc-Type:
    /// 4,ENCRYPTED` header is detected up front and the key warn+skipped (Go skips
    /// encrypted keys too).
    const ENCRYPTED_RSA_PKCS1_KEY: &str = "-----BEGIN RSA PRIVATE KEY-----\n\
Proc-Type: 4,ENCRYPTED\n\
DEK-Info: AES-256-CBC,96416E23735227E2214CD09776F0FD5D\n\
\n\
EfltBMAO1wV1p33Z4kdt3EtQr3S802Zen54fPdsbNybnB+HEIfbC//cZqSs4qT80\n\
NjLsAPNtHiRQm9/f4bUmfpH9oe6kghaM5tg/vTr8vSiIz4kATOQEs7HIpYmpCDmW\n\
oRFtyqLsjsqbGBp40xx1RyyGrGTBD2PoiIH6nrp1o7qPEi5Tn15hlbAPI/azp2fK\n\
9WZfgHQeP5Cm7fDICoqKy62opHwXznlXjthi9m0U+axZkl5E5+vyFy5m1gJjTh06\n\
l1dKb0R8upBK7URZ0qZgPsrow6sg3gUT1B6PPkHeRPGTtWHYClOUcrLl6Sy0SaU7\n\
UqkmPK+gSCTma+JfAT6jWIgUNgBQ9A7/zgi3MGAASu/KXyxVb78b3u1rz2Rkk5SE\n\
sdLMfAl3rXL5PnyY3YpA49jGqKw6nnHPLi9/9K7l1Wu3HWApgWqDl49wV/om/uj1\n\
D+eM4kmM+z/q82n/woshI+7tR60vh78hIrgOYLxIMXZ7Cpr0aYykmModh5SMPw4B\n\
emkP89BT+6AXLzLqhrJKsHiZvFiWH+S6C1hm2Hgi8M6ykt9rhs7kYMksaQRdfh7L\n\
H9YOTtt0WV9BufJbj/NRAhI0KHn4dMcnR0W9qK7tyvCQ0O5hokXOabqOvVPTaDA+\n\
rGNjs2ffw+Y/nS9YcCt7CAhrQWphHBkPnLJ0Tn1SfyVjF2P+KAC3Dfrn5SBGepl3\n\
3noWP/mnRbFYqCRjrswmv8/GeCsuJkuR7WVE6xCOX0KfnoRgRbvX7kFrQX6AM9P0\n\
5YyGWohuXxS1zSbqwzN2EO32DiPoL2fU0rRL8XS37aUFr8rce9i0zGC5xaJNZOVQ\n\
KCdDxemkayjHOklXyePvmmGLoPA/BPt53/EYm10S0dIwJ96N9o1fPI3PHofBfD6f\n\
6FH+fHum++Dy3cppHU5MDtRUmIjp0aNbpDmIdpcSRed8v+KUiP/V811s+0TEnu6v\n\
pAW9WYda/cPiLp38HzC3JZMq6dtsTXuKVDyD/AxEsiMLdYQ3DuRLgS29OyV0DAwz\n\
gagDqC12jvDsngdDApvHmj45pES/IaBE8tOJo76Fpdah/rNDWsoFE49iV7B2+TXa\n\
tneWznn6ybHKWRMULz0wOlcBCD68t6rMQMhSv1QjGE7OyCRHsR/QrBiQN/jfwOAB\n\
HLbLLh/hzmiRb7ZQvbd1dY28VDhhOzO4N5FJfExIYABshCqvDuF8ylRa9NmHdByu\n\
E5L02Hj36gju0b+I9MXke26MkfYocGacutI+hpV8g8hDPCfh1DQbY3VoJb7Nxz/x\n\
Wr2niMs0AOoeLZ2ZzpLnwoY0TcVWIt7boOcrVql9l0uD9gVF92PfDxslrzu+7dsl\n\
tGt/zoDPscoKRNz2iFyx4Tdqv5UwgwYywPTCwrYd3LtIG9ERfuAcGQpDmpslnMqI\n\
BwvXuouylfCFNu597h8+JSc1giV94Lqk22aXunoFhJuH4itF/qBvPUmmJk+84VHI\n\
ghQlDmmIuqWhWbPPosOm2yPIeWT7+38JTUCv9GIIAWe4voJWqOJZc/ze0/q6yeKP\n\
thaWnPuu0EWHWhqSQuP3zx/47Kd98x5FMIW92hlf+vZxuURwYr0VwYB/tz9p4s4Z\n\
-----END RSA PRIVATE KEY-----\n";

    /// `ssh-keygen -y` ssh-wire pubkey blob for the RSA fixture (shared by PKCS#1 +
    /// PKCS#8 — same key), base64(StdEncoding).
    const RSA_PEM_PUB_MARSHAL_B64: &str = "AAAAB3NzaC1yc2EAAAADAQABAAABAQC2oNa4E4U9KNRp61kLK3BWZTwrsVEbBuJ/D235DB60uax7/LQpBoa0r9X4kzWqmCcb0z4Kwd7TbVDsju3pNopkB3suAkypMPoBHogIFM5zAF/RlF4HwsOjmhdhnBOPQa4r4hddbq+nwb27oPKMTo5pFN0R5OUD84ERpSfmtrh9oDtpJ9fNCcOL1a/9uwNUeFLXJMednO5pJQVkG+U+hRmKkrZBj1ZkFExDBXPqFJYQfNjs8zzOY3h1oKOA6l/K6byvZ4kECo45srpOwWrPIVq1MmSpUed2stb8eph6WLIJZ1lTf4Ponb9VunlF4KUlZ3ZaG0gH9+b2wQZ5FdqNdpgX";
    /// `ssh-keygen -y` ssh-wire pubkey blob for the SEC1 P-256 fixture.
    const EC_SEC1_P256_PUB_MARSHAL_B64: &str = "AAAAE2VjZHNhLXNoYTItbmlzdHAyNTYAAAAIbmlzdHAyNTYAAABBBJWAwmv3zii5LhNdqA5d41Z22ZEVPlizNa0EL2ohG+tDreOsXefkZFlfeP+KhtEIoRe8bkbyYgQhfByR2q2TX/A=";

    fn b64(s: &str) -> Vec<u8> {
        base64::engine::general_purpose::STANDARD.decode(s).unwrap()
    }

    /// A fresh unique temp dir (auto-removed on drop — matches the crate's `tempfile`
    /// test convention; also cleans up if an assertion panics). `tag` is a debug prefix.
    fn temp_dir(tag: &str) -> tempfile::TempDir {
        tempfile::Builder::new()
            .prefix(&format!("shed-ssh-be-{tag}-"))
            .tempdir()
            .unwrap()
    }

    /// Write `contents` to `<dir>/<name>`, creating the dir.
    fn write_key(dir: &Path, name: &str, contents: &str) {
        std::fs::create_dir_all(dir).unwrap();
        std::fs::write(dir.join(name), contents).unwrap();
    }

    /// Build an OpenSSH `id_ed25519` file from the fixed seed into a fresh `.ssh` dir.
    fn write_fixed_ed25519(dir: &Path) {
        let verifying = ed25519_dalek::SigningKey::from_bytes(&SEED).verifying_key();
        let keypair = Ed25519Keypair {
            public: Ed25519PublicKey(verifying.to_bytes()),
            private: Ed25519PrivateKey::from_bytes(&SEED),
        };
        let pk = PrivateKey::new(KeypairData::Ed25519(keypair), "test").unwrap();
        write_key(dir, "id_ed25519", pk.to_openssh(LineEnding::LF).unwrap().as_str());
    }

    /// Decode an SSH `mpint`-encoded ECDSA blob `mpint(r)‖mpint(s)` back into the two
    /// scalar byte strings (leading sign-padding stripped), for verification tests.
    fn decode_ecdsa_blob(blob: &[u8]) -> (Vec<u8>, Vec<u8>) {
        fn take(cur: &mut &[u8]) -> Vec<u8> {
            let len = u32::from_be_bytes(cur[..4].try_into().unwrap()) as usize;
            let body = cur[4..4 + len].to_vec();
            *cur = &cur[4 + len..];
            // Strip a single leading sign-pad 0x00 if present.
            if body.first() == Some(&0) {
                body[1..].to_vec()
            } else {
                body
            }
        }
        let mut cur = blob;
        let r = take(&mut cur);
        let s = take(&mut cur);
        assert!(cur.is_empty(), "ecdsa blob has trailing bytes");
        (r, s)
    }

    /// Left-pad a scalar to `width` bytes (for rebuilding a fixed-width r||s signature).
    fn left_pad(bytes: &[u8], width: usize) -> Vec<u8> {
        let mut out = vec![0u8; width - bytes.len()];
        out.extend_from_slice(bytes);
        out
    }

    #[test]
    fn signs_go_reference_vector() {
        let tmp = temp_dir("ed25519");
        write_fixed_ed25519(tmp.path());
        let (backend, warnings) = LocalKeysBackend::load_from_ssh_dir(tmp.path());
        assert!(warnings.is_empty());
        let keys = backend.list().unwrap();
        assert_eq!(keys.len(), 1);
        assert_eq!(keys[0].format, "ssh-ed25519");
        assert_eq!(keys[0].comment, "id_ed25519");
        assert_eq!(keys[0].blob, b64(GO_PUB_MARSHAL_B64));

        let sig = backend.sign(&keys[0].blob, CHALLENGE, 0).unwrap();
        assert_eq!(sig.format, "ssh-ed25519");
        assert_eq!(sig.blob.len(), 64);
        assert_eq!(
            sig.blob,
            b64(GO_BLOB_B64),
            "ed25519 sign blob must match Go x/crypto/ssh"
        );

        let reparsed = PublicKey::from_bytes(&keys[0].blob).unwrap();
        assert_eq!(reparsed.to_bytes().unwrap(), keys[0].blob);
    }

    #[test]
    fn localkeys_load_list_sign_ed25519() {
        // The ed25519 leg of the multi-algo backend (mirror of Go
        // TestLocalKeysBackendLoadAndSign for this key type).
        let tmp = temp_dir("ed25519-only");
        write_fixed_ed25519(tmp.path());
        let (backend, _w) = LocalKeysBackend::load_from_ssh_dir(tmp.path());
        let keys = backend.list().unwrap();
        assert_eq!(keys.len(), 1);
        let sig = backend.sign(&keys[0].blob, CHALLENGE, 0).unwrap();
        assert_eq!(sig.format, "ssh-ed25519");
        // Verify against the public key (deterministic path already byte-checked above).
        let vk = ed25519_dalek::VerifyingKey::from_bytes(
            &ssh_key::public::PublicKey::from_bytes(&keys[0].blob)
                .unwrap()
                .key_data()
                .ed25519()
                .unwrap()
                .0,
        )
        .unwrap();
        use ed25519_dalek::Verifier;
        let dalek_sig = ed25519_dalek::Signature::from_slice(&sig.blob).unwrap();
        vk.verify(CHALLENGE, &dalek_sig).expect("ed25519 sig verifies");
    }

    #[test]
    fn localkeys_load_list_sign_rsa() {
        let tmp = temp_dir("rsa");
        write_key(tmp.path(), "id_rsa", RSA_KEY);
        let (backend, warnings) = LocalKeysBackend::load_from_ssh_dir(tmp.path());
        assert!(warnings.is_empty());
        let keys = backend.list().unwrap();
        assert_eq!(keys.len(), 1);
        assert_eq!(keys[0].format, "ssh-rsa");
        assert_eq!(keys[0].comment, "id_rsa");
        // Reparse the marshaled pubkey (Go's ssh.ParsePublicKey round-trip).
        let reparsed = PublicKey::from_bytes(&keys[0].blob).unwrap();
        assert_eq!(reparsed.to_bytes().unwrap(), keys[0].blob);

        // Default (flags=0) is ssh-rsa (SHA-1); verify against the pubkey.
        let sig = backend.sign(&keys[0].blob, CHALLENGE, 0).unwrap();
        assert_eq!(sig.format, "ssh-rsa");
        rsa_verify(&keys[0].blob, CHALLENGE, &sig.blob, RsaHash::Sha1);

        // flags=2 → rsa-sha2-256; flags=4 → rsa-sha2-512.
        let s256 = backend.sign(&keys[0].blob, CHALLENGE, 2).unwrap();
        assert_eq!(s256.format, "rsa-sha2-256");
        rsa_verify(&keys[0].blob, CHALLENGE, &s256.blob, RsaHash::Sha256);
        let s512 = backend.sign(&keys[0].blob, CHALLENGE, 4).unwrap();
        assert_eq!(s512.format, "rsa-sha2-512");
        rsa_verify(&keys[0].blob, CHALLENGE, &s512.blob, RsaHash::Sha512);

        // RSA PKCS#1 v1.5 is deterministic and — verified once with a standalone Go
        // `x/crypto/ssh` program over this exact key + challenge — BYTE-IDENTICAL to
        // Go's `ssh.Signer.Sign` / `SignWithAlgorithm`. So the blobs are pinned as Go
        // reference goldens (the differential still treats RSA as verify-not-bytes on
        // the wire per the program brief; this unit golden is the stronger check).
        assert_eq!(keys[0].blob, b64(GO_RSA_PUB_MARSHAL_B64));
        assert_eq!(sig.blob, b64(GO_RSA_SHA1_B64), "ssh-rsa (SHA-1) matches Go");
        assert_eq!(s256.blob, b64(GO_RSA_SHA256_B64), "rsa-sha2-256 matches Go");
        assert_eq!(s512.blob, b64(GO_RSA_SHA512_B64), "rsa-sha2-512 matches Go");
    }

    /// Verify a raw PKCS#1 v1.5 RSA signature blob against a marshaled SSH pubkey.
    fn rsa_verify(marshaled_pub: &[u8], data: &[u8], blob: &[u8], hash: RsaHash) {
        use rsa::Pkcs1v15Sign;
        use sha2::Digest as _;
        let pk = PublicKey::from_bytes(marshaled_pub).unwrap();
        let rsa_pub =
            rsa::RsaPublicKey::try_from(pk.key_data().rsa().expect("rsa pubkey")).unwrap();
        let ok = match hash {
            RsaHash::Sha1 => rsa_pub.verify(
                Pkcs1v15Sign::new::<sha1::Sha1>(),
                &sha1::Sha1::digest(data),
                blob,
            ),
            RsaHash::Sha256 => rsa_pub.verify(
                Pkcs1v15Sign::new::<sha2::Sha256>(),
                &sha2::Sha256::digest(data),
                blob,
            ),
            RsaHash::Sha512 => rsa_pub.verify(
                Pkcs1v15Sign::new::<sha2::Sha512>(),
                &sha2::Sha512::digest(data),
                blob,
            ),
        };
        ok.expect("rsa signature verifies against pubkey");
    }

    #[test]
    fn localkeys_load_list_sign_ecdsa() {
        // P-256 (the committed harness fixture curve).
        let tmp = temp_dir("ecdsa256");
        write_key(tmp.path(), "id_ecdsa", ECDSA_P256_KEY);
        let (backend, warnings) = LocalKeysBackend::load_from_ssh_dir(tmp.path());
        assert!(warnings.is_empty());
        let keys = backend.list().unwrap();
        assert_eq!(keys.len(), 1);
        assert_eq!(keys[0].format, "ecdsa-sha2-nistp256");
        assert_eq!(keys[0].comment, "id_ecdsa");
        let reparsed = PublicKey::from_bytes(&keys[0].blob).unwrap();
        assert_eq!(reparsed.to_bytes().unwrap(), keys[0].blob);

        let sig = backend.sign(&keys[0].blob, CHALLENGE, 0).unwrap();
        assert_eq!(sig.format, "ecdsa-sha2-nistp256");
        ecdsa_p256_verify(&keys[0].blob, CHALLENGE, &sig.blob);
        // An rsa-sha2 flag on an ECDSA key is a sign error (Go SignWithAlgorithm rejects).
        assert!(backend.sign(&keys[0].blob, CHALLENGE, 2).is_err());
    }

    fn ecdsa_p256_verify(marshaled_pub: &[u8], data: &[u8], blob: &[u8]) {
        use p256::ecdsa::signature::Verifier;
        let pk = PublicKey::from_bytes(marshaled_pub).unwrap();
        let sec1 = match pk.key_data() {
            ssh_key::public::KeyData::Ecdsa(e) => e.as_sec1_bytes().to_vec(),
            _ => panic!("not ecdsa"),
        };
        let vk = p256::ecdsa::VerifyingKey::from_sec1_bytes(&sec1).unwrap();
        let (r, s) = decode_ecdsa_blob(blob);
        let mut rs = left_pad(&r, 32);
        rs.extend(left_pad(&s, 32));
        let sig = p256::ecdsa::Signature::from_slice(&rs).unwrap();
        vk.verify(data, &sig).expect("p256 sig verifies");
    }

    #[test]
    fn localkeys_load_list_sign_ecdsa_p384() {
        let tmp = temp_dir("ecdsa384");
        write_key(tmp.path(), "id_ecdsa", ECDSA_P384_KEY);
        let (backend, _w) = LocalKeysBackend::load_from_ssh_dir(tmp.path());
        let keys = backend.list().unwrap();
        assert_eq!(keys[0].format, "ecdsa-sha2-nistp384");
        let sig = backend.sign(&keys[0].blob, CHALLENGE, 0).unwrap();
        assert_eq!(sig.format, "ecdsa-sha2-nistp384");
        use p384::ecdsa::signature::Verifier;
        let pk = PublicKey::from_bytes(&keys[0].blob).unwrap();
        let sec1 = match pk.key_data() {
            ssh_key::public::KeyData::Ecdsa(e) => e.as_sec1_bytes().to_vec(),
            _ => panic!("not ecdsa"),
        };
        let vk = p384::ecdsa::VerifyingKey::from_sec1_bytes(&sec1).unwrap();
        let (r, s) = decode_ecdsa_blob(&sig.blob);
        let mut rs = left_pad(&r, 48);
        rs.extend(left_pad(&s, 48));
        let esig = p384::ecdsa::Signature::from_slice(&rs).unwrap();
        vk.verify(CHALLENGE, &esig).expect("p384 sig verifies");
    }

    #[test]
    fn localkeys_load_list_sign_ecdsa_p521() {
        let tmp = temp_dir("ecdsa521");
        write_key(tmp.path(), "id_ecdsa", ECDSA_P521_KEY);
        let (backend, _w) = LocalKeysBackend::load_from_ssh_dir(tmp.path());
        let keys = backend.list().unwrap();
        assert_eq!(keys[0].format, "ecdsa-sha2-nistp521");
        let sig = backend.sign(&keys[0].blob, CHALLENGE, 0).unwrap();
        assert_eq!(sig.format, "ecdsa-sha2-nistp521");
        use p521::ecdsa::signature::Verifier;
        let pk = PublicKey::from_bytes(&keys[0].blob).unwrap();
        let sec1 = match pk.key_data() {
            ssh_key::public::KeyData::Ecdsa(e) => e.as_sec1_bytes().to_vec(),
            _ => panic!("not ecdsa"),
        };
        let vk = p521::ecdsa::VerifyingKey::from_sec1_bytes(&sec1).unwrap();
        let (r, s) = decode_ecdsa_blob(&sig.blob);
        let mut rs = left_pad(&r, 66);
        rs.extend(left_pad(&s, 66));
        let esig = p521::ecdsa::Signature::from_slice(&rs).unwrap();
        vk.verify(CHALLENGE, &esig).expect("p521 sig verifies");
    }

    #[test]
    fn all_three_key_files_load_together_in_order() {
        // Mirror of Go TestLocalKeysBackendLoadAndSign: all three files present →
        // three keys in the load order id_ed25519, id_rsa, id_ecdsa.
        let tmp = temp_dir("all3");
        write_fixed_ed25519(tmp.path());
        write_key(tmp.path(), "id_rsa", RSA_KEY);
        write_key(tmp.path(), "id_ecdsa", ECDSA_P256_KEY);
        let (backend, warnings) = LocalKeysBackend::load_from_ssh_dir(tmp.path());
        assert!(warnings.is_empty());
        let keys = backend.list().unwrap();
        assert_eq!(keys.len(), 3);
        assert_eq!(keys[0].format, "ssh-ed25519");
        assert_eq!(keys[1].format, "ssh-rsa");
        assert_eq!(keys[2].format, "ecdsa-sha2-nistp256");
        assert_eq!(backend.mode(), "local-keys");
    }

    #[test]
    fn flag_matrix() {
        // flags 0,1,2,3,4,6,8 × {rsa key, ed25519 key}. Bit-2 priority, reserved bits
        // (1, 8) fall through, rsa-sha2 bits on the ed25519 key error. Mirror of the
        // Go flag switch (`ssh_backend_localkeys.go:93-100`); Rust-only (no Go test).
        let tmp = temp_dir("flags");
        write_fixed_ed25519(tmp.path());
        write_key(tmp.path(), "id_rsa", RSA_KEY);
        let (backend, _w) = LocalKeysBackend::load_from_ssh_dir(tmp.path());
        let keys = backend.list().unwrap();
        let ed = &keys[0].blob; // id_ed25519 (loaded first)
        let rsa_pub = &keys[1].blob; // id_rsa

        // RSA key: format follows the bit tests.
        let rsa_cases = [
            (0u32, "ssh-rsa"),
            (1, "ssh-rsa"),      // reserved bit 0 alone → default
            (2, "rsa-sha2-256"), // bit 1
            (3, "rsa-sha2-256"), // bits 0+1 → sha256 (reserved doesn't change it)
            (4, "rsa-sha2-512"), // bit 2
            (6, "rsa-sha2-256"), // bits 1+2 → sha256 wins (checked first)
            (8, "ssh-rsa"),      // unassigned bit 3 alone → default
        ];
        for (flags, want) in rsa_cases {
            let sig = backend.sign(rsa_pub, CHALLENGE, flags).unwrap();
            assert_eq!(sig.format, want, "rsa flags={flags:#x}");
        }

        // ed25519 key: reserved/unassigned bits → ssh-ed25519; rsa-sha2 bits → error.
        for flags in [0u32, 1, 8] {
            let sig = backend.sign(ed, CHALLENGE, flags).unwrap();
            assert_eq!(sig.format, "ssh-ed25519", "ed flags={flags:#x}");
        }
        for flags in [2u32, 3, 4, 6] {
            assert!(
                backend.sign(ed, CHALLENGE, flags).is_err(),
                "ed flags={flags:#x} (rsa-sha2 bit) must error"
            );
        }
    }

    #[test]
    fn missing_file_silent_skip() {
        // No key files → empty backend. Missing files are a silent per-file skip
        // (Go `continue`, no log); the only warning is the aggregate zero-keys one
        // (Go `ssh_backend_localkeys.go:67`).
        let tmp = temp_dir("empty");
        let (backend, warnings) = LocalKeysBackend::load_from_ssh_dir(tmp.path());
        assert!(backend.list().unwrap().is_empty());
        assert_eq!(
            warnings,
            vec!["no SSH keys found in ~/.ssh/".to_string()],
            "missing files must not warn per-file; zero keys warns once"
        );
        assert_eq!(backend.mode(), "local-keys");
    }

    #[test]
    fn encrypted_key_warn_skip() {
        // An encrypted id_ed25519 warns + is skipped; a valid id_rsa still loads
        // (backend loads the rest). Mirrors Go's per-file warn+continue.
        let tmp = temp_dir("encrypted");
        write_key(tmp.path(), "id_ed25519", ENCRYPTED_ED25519_KEY);
        write_key(tmp.path(), "id_rsa", RSA_KEY);
        let (backend, warnings) = LocalKeysBackend::load_from_ssh_dir(tmp.path());
        assert_eq!(warnings.len(), 1, "encrypted key warns once");
        assert!(warnings[0].contains("skipping SSH key"), "{}", warnings[0]);
        let keys = backend.list().unwrap();
        assert_eq!(keys.len(), 1, "the valid rsa key still loads");
        assert_eq!(keys[0].format, "ssh-rsa");
    }

    #[test]
    fn corrupt_key_warn_skip() {
        // A garbage/unparsable id_ed25519 warns + is skipped (unparsable path).
        let tmp = temp_dir("corrupt");
        write_key(tmp.path(), "id_ed25519", "-----BEGIN OPENSSH PRIVATE KEY-----\nnot base64!!\n-----END OPENSSH PRIVATE KEY-----\n");
        let (backend, warnings) = LocalKeysBackend::load_from_ssh_dir(tmp.path());
        // One skip warning + the aggregate zero-keys warning (nothing else loaded).
        assert_eq!(warnings.len(), 2);
        assert!(warnings[0].contains("skipping SSH key"), "{}", warnings[0]);
        assert_eq!(warnings[1], "no SSH keys found in ~/.ssh/");
        assert!(backend.list().unwrap().is_empty());
    }

    #[test]
    fn unknown_key_is_not_found() {
        let tmp = temp_dir("notfound");
        write_fixed_ed25519(tmp.path());
        let (backend, _w) = LocalKeysBackend::load_from_ssh_dir(tmp.path());
        let other = ed25519_dalek::SigningKey::from_bytes(&[9u8; 32]).verifying_key();
        let other_pub = PublicKey::from(ssh_key::public::KeyData::Ed25519(Ed25519PublicKey(
            other.to_bytes(),
        )))
        .to_bytes()
        .unwrap();
        assert_eq!(
            backend.sign(&other_pub, CHALLENGE, 0).unwrap_err(),
            "key not found"
        );
    }

    #[test]
    fn resolve_mode_matrix() {
        // This commit's resolve subset (the full auto-detect dial matrix lands in
        // commit 2, with the agent-forward backend). Rust-only (no Go test).
        let tmp = temp_dir("resolve");
        write_fixed_ed25519(tmp.path());

        // explicit local-keys → local backend.
        let (be, _w) = resolve_ssh_backend("local-keys", tmp.path()).unwrap();
        assert_eq!(be.mode(), "local-keys");
        assert_eq!(be.list().unwrap().len(), 1);

        // auto ("") → local-keys this commit.
        let (be, _w) = resolve_ssh_backend("", tmp.path()).unwrap();
        assert_eq!(be.mode(), "local-keys");

        // agent-forward → error (commit-2 wires the backend). `.err()` because the
        // Ok variant (`Arc<dyn SshBackend>`) is not `Debug`.
        assert_eq!(
            resolve_ssh_backend("agent-forward", tmp.path()).err().unwrap(),
            "agent-forward backend not yet implemented"
        );

        // unknown mode → Go's exact string.
        assert_eq!(
            resolve_ssh_backend("bogus", tmp.path()).err().unwrap(),
            "unknown ssh mode: \"bogus\" (expected agent-forward, local-keys, or empty)"
        );
    }

    #[test]
    fn ssh_mpint_encoding_edges() {
        // No sign pad needed (top bit clear).
        assert_eq!(ssh_mpint(&[0x7f, 0x01]), vec![0, 0, 0, 2, 0x7f, 0x01]);
        // Top bit set → 0x00 prepended.
        assert_eq!(ssh_mpint(&[0x80, 0x00]), vec![0, 0, 0, 3, 0x00, 0x80, 0x00]);
        // Leading zeros stripped.
        assert_eq!(ssh_mpint(&[0x00, 0x00, 0x2a]), vec![0, 0, 0, 1, 0x2a]);
        // All-zero scalar → empty string.
        assert_eq!(ssh_mpint(&[0x00, 0x00]), vec![0, 0, 0, 0]);
    }

    // ---- Legacy-PEM loaders (the formats Go's ssh.ParsePrivateKey accepts beyond
    //      OpenSSH). Each proves load → list → sign → cryptographic verify, and — for
    //      RSA/EC — that the hand-built marshaled pubkey equals ssh-keygen's blob. ----

    #[test]
    fn pkcs1_rsa_loads_and_signs() {
        let tmp = temp_dir("pkcs1");
        write_key(tmp.path(), "id_rsa", RSA_PKCS1_KEY);
        let (backend, warnings) = LocalKeysBackend::load_from_ssh_dir(tmp.path());
        assert!(warnings.is_empty(), "{warnings:?}");
        let keys = backend.list().unwrap();
        assert_eq!(keys.len(), 1);
        assert_eq!(keys[0].format, "ssh-rsa");
        assert_eq!(keys[0].comment, "id_rsa");
        // Hand-marshaled pubkey == ssh-keygen's ssh-wire blob (independent oracle) ...
        assert_eq!(keys[0].blob, b64(RSA_PEM_PUB_MARSHAL_B64));
        // ... and is canonical ssh-wire (round-trips through ssh-key, so keysEqual works).
        let reparsed = PublicKey::from_bytes(&keys[0].blob).unwrap();
        assert_eq!(reparsed.to_bytes().unwrap(), keys[0].blob);

        // Default (SHA-1) + rsa-sha2-256 both sign and verify against the pubkey.
        let sig = backend.sign(&keys[0].blob, CHALLENGE, 0).unwrap();
        assert_eq!(sig.format, "ssh-rsa");
        rsa_verify(&keys[0].blob, CHALLENGE, &sig.blob, RsaHash::Sha1);
        let s256 = backend.sign(&keys[0].blob, CHALLENGE, 2).unwrap();
        assert_eq!(s256.format, "rsa-sha2-256");
        rsa_verify(&keys[0].blob, CHALLENGE, &s256.blob, RsaHash::Sha256);
    }

    #[test]
    fn pkcs8_rsa_loads_and_signs() {
        let tmp = temp_dir("pkcs8rsa");
        write_key(tmp.path(), "id_rsa", RSA_PKCS8_KEY);
        let (backend, warnings) = LocalKeysBackend::load_from_ssh_dir(tmp.path());
        assert!(warnings.is_empty(), "{warnings:?}");
        let keys = backend.list().unwrap();
        assert_eq!(keys.len(), 1);
        assert_eq!(keys[0].format, "ssh-rsa");
        // Same underlying key as the PKCS#1 fixture → identical marshaled pubkey.
        assert_eq!(keys[0].blob, b64(RSA_PEM_PUB_MARSHAL_B64));
        let sig = backend.sign(&keys[0].blob, CHALLENGE, 4).unwrap();
        assert_eq!(sig.format, "rsa-sha2-512");
        rsa_verify(&keys[0].blob, CHALLENGE, &sig.blob, RsaHash::Sha512);
    }

    #[test]
    fn sec1_ec_p256_loads_and_signs() {
        let tmp = temp_dir("sec1");
        write_key(tmp.path(), "id_ecdsa", EC_SEC1_P256_KEY);
        let (backend, warnings) = LocalKeysBackend::load_from_ssh_dir(tmp.path());
        assert!(warnings.is_empty(), "{warnings:?}");
        let keys = backend.list().unwrap();
        assert_eq!(keys.len(), 1);
        assert_eq!(keys[0].format, "ecdsa-sha2-nistp256");
        assert_eq!(keys[0].blob, b64(EC_SEC1_P256_PUB_MARSHAL_B64));
        let reparsed = PublicKey::from_bytes(&keys[0].blob).unwrap();
        assert_eq!(reparsed.to_bytes().unwrap(), keys[0].blob);
        let sig = backend.sign(&keys[0].blob, CHALLENGE, 0).unwrap();
        assert_eq!(sig.format, "ecdsa-sha2-nistp256");
        ecdsa_p256_verify(&keys[0].blob, CHALLENGE, &sig.blob);
    }

    #[test]
    fn pkcs8_ed25519_loads_and_signs() {
        let tmp = temp_dir("pkcs8ed");
        write_key(tmp.path(), "id_ed25519", ED25519_PKCS8_KEY);
        let (backend, warnings) = LocalKeysBackend::load_from_ssh_dir(tmp.path());
        assert!(warnings.is_empty(), "{warnings:?}");
        let keys = backend.list().unwrap();
        assert_eq!(keys.len(), 1);
        assert_eq!(keys[0].format, "ssh-ed25519");
        let reparsed = PublicKey::from_bytes(&keys[0].blob).unwrap();
        assert_eq!(reparsed.to_bytes().unwrap(), keys[0].blob);
        let sig = backend.sign(&keys[0].blob, CHALLENGE, 0).unwrap();
        assert_eq!(sig.format, "ssh-ed25519");
        let vk = ed25519_dalek::VerifyingKey::from_bytes(
            &PublicKey::from_bytes(&keys[0].blob)
                .unwrap()
                .key_data()
                .ed25519()
                .unwrap()
                .0,
        )
        .unwrap();
        use ed25519_dalek::Verifier;
        vk.verify(
            CHALLENGE,
            &ed25519_dalek::Signature::from_slice(&sig.blob).unwrap(),
        )
        .expect("ed25519 pkcs8 sig verifies");
    }

    #[test]
    fn encrypted_pkcs1_rsa_warn_skip() {
        // A `Proc-Type: 4,ENCRYPTED` legacy PEM warns + is skipped (Go skips encrypted
        // keys too). Only id_rsa present → one skip warning + the aggregate zero-keys one.
        let tmp = temp_dir("encpkcs1");
        write_key(tmp.path(), "id_rsa", ENCRYPTED_RSA_PKCS1_KEY);
        let (backend, warnings) = LocalKeysBackend::load_from_ssh_dir(tmp.path());
        assert_eq!(warnings.len(), 2, "{warnings:?}");
        assert!(warnings[0].contains("skipping SSH key"), "{}", warnings[0]);
        assert!(
            warnings[0].contains("passphrase-protected"),
            "{}",
            warnings[0]
        );
        assert_eq!(warnings[1], "no SSH keys found in ~/.ssh/");
        assert!(backend.list().unwrap().is_empty());
    }

    #[test]
    fn unsupported_algo_error_matches_go() {
        // FIX 2: byte-identical to x/crypto/ssh keys.go:1216
        // (`fmt.Errorf("ssh: unsupported signature algorithm %q for key format %q", ...)`,
        // `%q` = double-quoted). An rsa-sha2 flag on the ed25519 key hits this arm.
        let tmp = temp_dir("errstr");
        write_fixed_ed25519(tmp.path());
        let (backend, _w) = LocalKeysBackend::load_from_ssh_dir(tmp.path());
        let ed = backend.list().unwrap()[0].blob.clone();
        assert_eq!(
            backend.sign(&ed, CHALLENGE, 2).unwrap_err(),
            r#"ssh: unsupported signature algorithm "rsa-sha2-256" for key format "ssh-ed25519""#
        );
        assert_eq!(
            backend.sign(&ed, CHALLENGE, 4).unwrap_err(),
            r#"ssh: unsupported signature algorithm "rsa-sha2-512" for key format "ssh-ed25519""#
        );
    }
}

