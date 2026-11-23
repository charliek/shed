//! Fingerprints: the same content gives the same fingerprint on one
//! machine, and the fingerprint means nothing to the server, because it is
//! keyed by a random key the machine keeps.

use base64::Engine as _;
use hmac::{Hmac, Mac};
use sha2::Sha256;

use crate::answer::ItemBody;

/// The key fingerprints are computed with: 32 random bytes the caller makes
/// once and keeps (the desktop app keeps it in its state directory).
#[derive(Clone, PartialEq, Eq)]
pub struct FingerprintKey([u8; 32]);

impl FingerprintKey {
    /// The key made of `bytes`.
    pub fn from_bytes(bytes: [u8; 32]) -> FingerprintKey {
        FingerprintKey(bytes)
    }

    /// The key's bytes, to keep it.
    pub fn as_bytes(&self) -> &[u8; 32] {
        &self.0
    }
}

impl std::fmt::Debug for FingerprintKey {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str("FingerprintKey(..)")
    }
}

/// The bytes of a fingerprint before encoding: 144 bits, 24 characters.
const FINGERPRINT_BYTES: usize = 18;

/// The fingerprint of `content` under `key`: HMAC-SHA256 over a label, the
/// kind and the content, cut to 18 bytes and written as unpadded URL-safe
/// base64 (24 of `[A-Za-z0-9_-]`).
pub fn fingerprint(key: &FingerprintKey, kind: &str, content: &[u8]) -> String {
    let mut mac =
        <Hmac<Sha256> as Mac>::new_from_slice(&key.0).expect("HMAC takes a key of any length");
    mac.update(b"shed-provision fingerprint v1\0");
    mac.update(kind.as_bytes());
    mac.update(b"\0");
    mac.update(content);
    let sum = mac.finalize().into_bytes();
    base64::engine::general_purpose::URL_SAFE_NO_PAD.encode(&sum[..FINGERPRINT_BYTES])
}

/// The fingerprint of what an item writes: a file's content, or an env
/// value.
pub fn fingerprint_body(key: &FingerprintKey, body: &ItemBody) -> String {
    match body {
        ItemBody::File(f) => fingerprint(key, "file", &f.content),
        ItemBody::Env(e) => fingerprint(key, "env", e.value.as_bytes()),
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::text::valid_fingerprint;

    #[test]
    fn keyed_stable_and_in_the_alphabet() {
        let k1 = FingerprintKey::from_bytes([1; 32]);
        let k2 = FingerprintKey::from_bytes([2; 32]);
        let a = fingerprint(&k1, "file", b"secret");
        assert_eq!(a, fingerprint(&k1, "file", b"secret"));
        assert_ne!(a, fingerprint(&k2, "file", b"secret"));
        assert_ne!(a, fingerprint(&k1, "file", b"secret2"));
        assert_ne!(a, fingerprint(&k1, "env", b"secret"));
        assert_eq!(a.len(), 24);
        assert!(valid_fingerprint(&a));
        assert_eq!(format!("{k1:?}"), "FingerprintKey(..)");
    }
}
