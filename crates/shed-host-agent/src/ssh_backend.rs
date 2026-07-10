//! A minimal deterministic **ed25519-only** SSH signing backend — the local-keys
//! sliver of the Go daemon's `ssh_backend.go` + `ssh_backend_localkeys.go`. It loads
//! `~/.ssh/id_ed25519` (an OpenSSH private key) and signs a challenge byte-identically
//! to Go's `x/crypto/ssh` ed25519 signer: format `"ssh-ed25519"`, blob = the raw
//! 64-byte ed25519 signature of the data.
//!
//! **Why the bytes match Go, unmasked:** ed25519 signing is *deterministic* per
//! RFC 8032 (the nonce is derived from the key + message, not randomness), so Go's
//! `ed25519.Sign(priv, data)` and this backend's `ed25519_dalek::SigningKey::sign`
//! over the SAME 32-byte seed + message produce the SAME 64 bytes. The differential
//! harness can therefore compare the signature blob directly (no masking) — see the
//! `signs_go_reference_vector` unit test, which pins a vector generated with Go's
//! `x/crypto/ssh` signer.
//!
//! **Scope (this slice):** ed25519 ONLY. The full agent-forward / local-keys /
//! rsa / ecdsa backend + mode auto-detection is a later task. Keeping it minimal:
//! only `id_ed25519` is read, only ed25519 keys are loaded, `mode()` is fixed to
//! `"local-keys"`. This module has NO desktop/bus-only feature gate — the message
//! bus (always compiled, even headless) owns `sign`, so its deps (ssh-key +
//! ed25519-dalek) are always-on and the `--no-default-features` build compiles it.

use std::path::{Path, PathBuf};

use ed25519_dalek::Signer;
use ssh_key::private::PrivateKey;

use crate::config::user_home_dir;

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
/// the raw signature `blob` (64 bytes for ed25519).
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct SshSignature {
    pub format: String,
    pub blob: Vec<u8>,
}

/// The host SSH key backend (Go `ssh_backend.go:SSHBackend`). `Send + Sync` so the
/// bus can hold it as `Arc<dyn SshBackend>` across the async handler.
pub trait SshBackend: Send + Sync {
    /// The available public keys.
    fn list(&self) -> Result<Vec<SshKeyInfo>, String>;
    /// Sign `data` with the key whose SSH-wire marshaled public key equals
    /// `public_key` (Go's `keysEqual` = `bytes.Equal(a.Marshal(), b.Marshal())`).
    /// `flags` are ignored for ed25519 (they select rsa-sha2 variants). No matching
    /// key → `Err("key not found")` (Go's local-keys backend), which the handler maps
    /// to `{sign operation failed, SIGN_FAILED}` — NOT `KEY_NOT_FOUND` (that code is
    /// only for an unparsable public key).
    fn sign(&self, public_key: &[u8], data: &[u8], flags: u32) -> Result<SshSignature, String>;
    /// The backend mode name (`"agent-forward"` or `"local-keys"`).
    fn mode(&self) -> &str;
}

/// One loaded ed25519 key: its SSH-wire marshaled public key (for matching + `list`)
/// and its dalek signing key.
struct LoadedKey {
    marshaled_pub: Vec<u8>,
    signing: ed25519_dalek::SigningKey,
    comment: String,
}

/// The local-keys ed25519 backend. Load with [`LocalEd25519Backend::load`] (reads
/// `$HOME/.ssh/id_ed25519`); infallible — a missing, unreadable, encrypted, or
/// non-ed25519 key yields an empty backend (matching Go's `newLocalKeysBackend`,
/// which warns and skips rather than failing), so the daemon still starts.
pub struct LocalEd25519Backend {
    keys: Vec<LoadedKey>,
}

impl LocalEd25519Backend {
    /// Load `$HOME/.ssh/id_ed25519` (via the same `$HOME` resolution as the rest of
    /// the daemon). Never fails — see the type doc.
    pub fn load() -> LocalEd25519Backend {
        Self::load_from_ssh_dir(&user_home_dir().join(".ssh"))
    }

    /// Load `id_ed25519` from an explicit `.ssh` directory (the testable core of
    /// [`load`](Self::load)).
    pub fn load_from_ssh_dir(ssh_dir: &Path) -> LocalEd25519Backend {
        let mut keys = Vec::new();
        let key_path: PathBuf = ssh_dir.join("id_ed25519");
        if let Some(loaded) = load_ed25519_key(&key_path) {
            keys.push(loaded);
        }
        LocalEd25519Backend { keys }
    }

    /// The number of loaded keys — for the daemon's startup log line only.
    pub fn key_count(&self) -> usize {
        self.keys.len()
    }
}

/// Parse an OpenSSH `id_ed25519` private key file into a [`LoadedKey`]. Returns
/// `None` (skip) on any problem — missing/unreadable file, unparsable/encrypted key,
/// or a non-ed25519 key — mirroring Go's per-file `continue`.
fn load_ed25519_key(path: &Path) -> Option<LoadedKey> {
    let data = std::fs::read_to_string(path).ok()?;
    let private = PrivateKey::from_openssh(data.as_str()).ok()?;
    // ed25519 only this slice; an encrypted key has no plaintext keypair here.
    let keypair = private.key_data().ed25519()?;
    let seed = keypair.private.to_bytes();
    let signing = ed25519_dalek::SigningKey::from_bytes(&seed);
    // Marshaled public key == Go `ssh.PublicKey.Marshal()` (KeyData wire form, no
    // comment): the string algo name + the 32-byte public key.
    let marshaled_pub = private.public_key().to_bytes().ok()?;
    Some(LoadedKey {
        marshaled_pub,
        signing,
        // Go's local-keys backend uses the key filename as the comment.
        comment: "id_ed25519".to_string(),
    })
}

impl SshBackend for LocalEd25519Backend {
    fn list(&self) -> Result<Vec<SshKeyInfo>, String> {
        Ok(self
            .keys
            .iter()
            .map(|k| SshKeyInfo {
                format: "ssh-ed25519".to_string(),
                blob: k.marshaled_pub.clone(),
                comment: k.comment.clone(),
            })
            .collect())
    }

    fn sign(&self, public_key: &[u8], data: &[u8], _flags: u32) -> Result<SshSignature, String> {
        for k in &self.keys {
            if k.marshaled_pub == public_key {
                // Deterministic (RFC 8032) — byte-identical to Go's ssh ed25519 signer.
                let sig = k.signing.sign(data);
                return Ok(SshSignature {
                    format: "ssh-ed25519".to_string(),
                    blob: sig.to_bytes().to_vec(),
                });
            }
        }
        Err("key not found".to_string())
    }

    fn mode(&self) -> &str {
        "local-keys"
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use base64::Engine as _;
    use ssh_key::private::{Ed25519Keypair, Ed25519PrivateKey, KeypairData};
    use ssh_key::public::{Ed25519PublicKey, PublicKey};
    use ssh_key::LineEnding;

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

    fn b64(s: &str) -> Vec<u8> {
        base64::engine::general_purpose::STANDARD.decode(s).unwrap()
    }

    /// Build an OpenSSH `id_ed25519` file from the fixed seed and write it into a
    /// fresh temp `.ssh` dir; returns the dir. Mirrors how a real `id_ed25519` on
    /// disk would be loaded (round-trips through the ssh-key OpenSSH codec).
    fn write_fixed_key(dir: &Path) {
        let verifying = ed25519_dalek::SigningKey::from_bytes(&SEED).verifying_key();
        let keypair = Ed25519Keypair {
            public: Ed25519PublicKey(verifying.to_bytes()),
            private: Ed25519PrivateKey::from_bytes(&SEED),
        };
        let pk = PrivateKey::new(KeypairData::Ed25519(keypair), "test").unwrap();
        std::fs::create_dir_all(dir).unwrap();
        std::fs::write(
            dir.join("id_ed25519"),
            pk.to_openssh(LineEnding::LF).unwrap().as_bytes(),
        )
        .unwrap();
    }

    // The whole point: our ed25519 path is byte-identical to Go's ssh signer.
    #[test]
    fn signs_go_reference_vector() {
        let tmp = std::env::temp_dir().join(format!("shed-ssh-be-{}", uuid::Uuid::new_v4()));
        write_fixed_key(&tmp);
        let backend = LocalEd25519Backend::load_from_ssh_dir(&tmp);
        assert_eq!(backend.key_count(), 1);

        // The loaded marshaled pubkey matches Go's PublicKey().Marshal().
        let keys = backend.list().unwrap();
        assert_eq!(keys.len(), 1);
        assert_eq!(keys[0].format, "ssh-ed25519");
        assert_eq!(keys[0].comment, "id_ed25519");
        assert_eq!(keys[0].blob, b64(GO_PUB_MARSHAL_B64));

        // Sign the fixed challenge and assert the blob equals Go's, byte-for-byte.
        let sig = backend.sign(&keys[0].blob, CHALLENGE, 0).unwrap();
        assert_eq!(sig.format, "ssh-ed25519");
        assert_eq!(sig.blob.len(), 64);
        assert_eq!(
            sig.blob,
            b64(GO_BLOB_B64),
            "ed25519 sign blob must match Go x/crypto/ssh"
        );

        // A re-parse of the marshaled pubkey (Go's ssh.ParsePublicKey path) round-trips.
        let reparsed = PublicKey::from_bytes(&keys[0].blob).unwrap();
        assert_eq!(reparsed.to_bytes().unwrap(), keys[0].blob);

        let _ = std::fs::remove_dir_all(&tmp);
    }

    #[test]
    fn deterministic_across_repeated_signs() {
        let tmp = std::env::temp_dir().join(format!("shed-ssh-be-{}", uuid::Uuid::new_v4()));
        write_fixed_key(&tmp);
        let backend = LocalEd25519Backend::load_from_ssh_dir(&tmp);
        let pub_blob = backend.list().unwrap()[0].blob.clone();
        let a = backend.sign(&pub_blob, CHALLENGE, 0).unwrap();
        let b = backend.sign(&pub_blob, CHALLENGE, 7).unwrap(); // flags ignored for ed25519
        assert_eq!(a.blob, b.blob);
        let _ = std::fs::remove_dir_all(&tmp);
    }

    #[test]
    fn unknown_key_is_not_found() {
        let tmp = std::env::temp_dir().join(format!("shed-ssh-be-{}", uuid::Uuid::new_v4()));
        write_fixed_key(&tmp);
        let backend = LocalEd25519Backend::load_from_ssh_dir(&tmp);
        // A different (well-formed) ed25519 pubkey the backend never loaded.
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
        let _ = std::fs::remove_dir_all(&tmp);
    }

    #[test]
    fn missing_key_yields_empty_backend() {
        let tmp = std::env::temp_dir().join(format!("shed-ssh-be-empty-{}", uuid::Uuid::new_v4()));
        std::fs::create_dir_all(&tmp).unwrap();
        let backend = LocalEd25519Backend::load_from_ssh_dir(&tmp);
        assert_eq!(backend.key_count(), 0);
        assert!(backend.list().unwrap().is_empty());
        assert_eq!(backend.mode(), "local-keys");
        let _ = std::fs::remove_dir_all(&tmp);
    }
}
