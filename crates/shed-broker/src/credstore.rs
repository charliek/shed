//! The broker's own on-disk client-credential store: the certificate + private key an
//! mtls-mode shed-server issues over the SSH bootstrap channel, kept out of any config
//! file.
//!
//! It is a [`CredStore`] rooted at a caller-supplied directory rather than a set of
//! functions bound to one location, because the processes that hold shed client
//! credentials must NOT share a directory (plan 001 D6's ownership table):
//!
//! ```text
//! shed CLI (Go)              control-scope credential      ~/.shed/creds/<server>/
//! host-agent (Go)            credentials-scope             its own state dir
//! broker (this crate)        credentials-scope             <state>/host-agent/creds/credentials/<server>/
//! ```
//!
//! One certificate carries one scope, and two processes rotating independently would
//! otherwise overwrite each other's material in place.
//!
//! Layout, one directory per server entry (mirroring Go's `sdk/creds`):
//!
//! ```text
//! <root>/<escaped-name>/client.pem   0600  the issued certificate
//! <root>/<escaped-name>/client.key   0600  the matching private key
//! <root>/%lock/<escaped-name>        0600  the per-server advisory lock
//! ```
//!
//! The certificate is public material and 0644 would be defensible, but it is written
//! 0600 alongside the key: nothing needs to read it but its owner, and a uniform rule is
//! one fewer thing to get wrong when the pair is rewritten on every rotation.
//!
//! **Ordering and atomicity are the load-bearing parts**, mirrored from `sdk/creds`
//! rule-for-rule: each file is written atomically (temp in the same directory, fsynced,
//! renamed), the KEY is written first and the certificate second, and the whole write —
//! plus the whole load — runs under one per-server advisory `flock`. Per-FILE atomicity
//! is not enough on its own, because two renames are two commits: without the lock a
//! reader can observe a new certificate beside an old key, and two rotations at once can
//! interleave into cert-A-with-key-B, a credential that belongs to nobody and fails a
//! handshake at some later, unrelated moment. The one case no lock can cover — a crash
//! BETWEEN the two renames — is why [`CredStore::load`] verifies the pair and treats a
//! mismatch as "no credential" (re-enroll) rather than as corruption to report.

use std::fs;
use std::io::Write as _;
use std::os::unix::fs::{OpenOptionsExt as _, PermissionsExt as _};
use std::os::unix::io::AsRawFd as _;
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::Arc;
use std::time::{SystemTime, UNIX_EPOCH};

use rustls::sign::CertifiedKey;
use shed_core::csr::{pem_decode, PEM_LABEL_CERTIFICATE};
use shed_core::tls::certified_key_from_pem_pair;

/// The fixed basenames inside a server's credential directory. Fixed rather than
/// serial-stamped: rotation replaces the pair in place, so there is never a second
/// generation to name and no stale keys accumulate.
pub const CERT_FILE_NAME: &str = "client.pem";
pub const KEY_FILE_NAME: &str = "client.key";

/// Holds one advisory lock file per server, as a SIBLING of the per-server credential
/// directories rather than a file inside them.
///
/// Sibling placement buys two things. [`CredStore::remove`] recursively deletes a server's
/// directory, and deleting a lock file out from under a process that holds it would let
/// the next locker create a fresh inode and lock THAT instead — the classic broken-mutex
/// shape. And the per-server directory stays exactly the two files it documents.
///
/// The `%` prefix makes the name collision-proof: directory names in the root come from
/// [`escape_server_name`], which emits `%` only as the start of a two-hex-digit escape,
/// and `%l` is not one — so no server name, however chosen, can escape to `%lock`.
const LOCK_DIR_NAME: &str = "%lock";

const DIR_PERM: u32 = 0o700;
const FILE_PERM: u32 = 0o600;

/// A credential read back from the store, already assembled for the TLS stack.
pub struct StoredCredential {
    /// The certificate PEM exactly as it was written.
    pub cert_pem: String,
    /// The rustls signing identity (leaf + key), pair-verified on load.
    pub certified: Arc<CertifiedKey>,
    /// The leaf's `notAfter` as unix seconds, or `None` when it could not be read.
    ///
    /// `None` is deliberately NOT fatal: an unparseable validity means the credential's
    /// freshness is unknown, which the caller treats as "usable but never proactively
    /// re-minted on expiry" — the proactive refresh loop still replaces it, and a server
    /// that rejects it drives a re-mint through the ordinary invalidation path.
    pub not_after_unix: Option<i64>,
}

impl std::fmt::Debug for StoredCredential {
    /// A stored credential contains a private key; only its public shape is renderable.
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("StoredCredential")
            .field("cert_pem_len", &self.cert_pem.len())
            .field("key", &"<redacted>")
            .field("not_after_unix", &self.not_after_unix)
            .finish()
    }
}

/// A credential store rooted at one directory.
#[derive(Debug, Clone)]
pub struct CredStore {
    root: PathBuf,
}

impl CredStore {
    /// The store rooted at `root`. The directory is created lazily, on the first write, so
    /// constructing a store never touches the filesystem.
    pub fn new(root: impl Into<PathBuf>) -> Self {
        Self { root: root.into() }
    }

    /// The production store for a credential `scope`, under the agent's durable state
    /// directory (`<state>/host-agent/creds/<scope>` — see [`crate::sockets::state_dir`]
    /// for why that is NOT the socket dir on Linux).
    ///
    /// The scope is part of the path rather than of the filename because one certificate
    /// carries exactly one scope: a `credentials` cert and a `control` cert for the same
    /// server are two independent credentials that must never overwrite each other.
    pub fn for_scope(scope: &str) -> Self {
        Self::new(
            crate::sockets::state_dir()
                .join("host-agent")
                .join("creds")
                .join(escape_server_name(scope)),
        )
    }

    pub fn root(&self) -> &Path {
        &self.root
    }

    /// The credential directory for a named server entry.
    ///
    /// The name is escaped because it is user-chosen and otherwise unconstrained: an entry
    /// called `../../.ssh` must not be able to steer a 0600 write — or the recursive
    /// delete in [`Self::remove`] — outside the root.
    pub fn server_dir(&self, name: &str) -> PathBuf {
        self.root.join(escape_server_name(name))
    }

    /// The certificate and key paths for a named server, without touching the filesystem.
    pub fn paths(&self, name: &str) -> (PathBuf, PathBuf) {
        let dir = self.server_dir(name);
        (dir.join(CERT_FILE_NAME), dir.join(KEY_FILE_NAME))
    }

    /// Read a server's stored certificate + key under that server's credential lock and
    /// assemble them for the TLS stack.
    ///
    /// `Ok(None)` means "nothing stored" (the ordinary first-run state). `Err` means
    /// "stored but unusable" — unreadable files, malformed PEM, or a certificate that does
    /// not match its key (what a crash between the two renames of a rotation leaves
    /// behind). Callers treat BOTH as "no credential, enroll" and differ only in whether
    /// they log a reason: recovery is a fresh enrollment, never an operator with a text
    /// editor.
    pub fn load(&self, name: &str) -> Result<Option<StoredCredential>, String> {
        if name.is_empty() {
            return Ok(None);
        }
        let (cert_path, key_path) = self.paths(name);
        if !cert_path.exists() && !key_path.exists() {
            return Ok(None);
        }
        let _lock = self.lock(name)?;

        let cert_pem = read_to_string(&cert_path)?;
        let key_pem = read_to_string(&key_path)?;
        // Pair verification is what turns a half-committed rotation into a recoverable
        // "no credential" instead of an opaque handshake failure later.
        let certified = certified_key_from_pem_pair(&cert_pem, &key_pem)
            .map_err(|e| format!("stored client credential for {name:?} is unusable: {e}"))?;
        let not_after_unix = pem_decode(PEM_LABEL_CERTIFICATE, &cert_pem)
            .ok()
            .and_then(|der| cert_not_after_unix(&der));
        Ok(Some(StoredCredential {
            cert_pem,
            certified: Arc::new(certified),
            not_after_unix,
        }))
    }

    /// Persist a freshly issued client certificate and its private key for `name`.
    ///
    /// The KEY is written first and the certificate second. Both orders can be
    /// interrupted, and both leave a mismatched pair the next load rejects — but a key
    /// with no certificate is inert, whereas a certificate with no key is the shape of a
    /// credential whose private half has gone missing.
    pub fn write(&self, name: &str, cert_pem: &str, key_pem: &str) -> Result<(), String> {
        if name.is_empty() {
            return Err("client credentials: server name required".into());
        }
        if cert_pem.is_empty() || key_pem.is_empty() {
            return Err("client credentials: empty certificate or key".into());
        }
        let _lock = self.lock(name)?;

        let dir = self.server_dir(name);
        ensure_dir(&dir)?;
        let (cert_path, key_path) = self.paths(name);
        atomic_write(&key_path, key_pem.as_bytes())?;
        if let Err(e) = atomic_write(&cert_path, cert_pem.as_bytes()) {
            // Roll the key back so the next start sees "no credential" (and re-enrolls)
            // rather than a key that certifies nothing.
            let _ = fs::remove_file(&key_path);
            return Err(e);
        }
        Ok(())
    }

    /// Delete a server's credential directory. Leaving a private key behind for a server
    /// that is no longer brokered for is exactly the kind of quiet residue that turns up
    /// years later. A missing directory is not an error.
    pub fn remove(&self, name: &str) -> Result<(), String> {
        if name.is_empty() {
            return Ok(());
        }
        let dir = self.server_dir(name);
        fs::remove_dir_all(&dir).or_else(|e| {
            if e.kind() == std::io::ErrorKind::NotFound {
                Ok(())
            } else {
                Err(format!("remove credentials dir {}: {e}", dir.display()))
            }
        })
    }

    /// Take the exclusive advisory lock guarding one server's credential pair; the guard
    /// releases it on drop. Held across the WHOLE write (both renames) and the whole load,
    /// which is what makes a rotation atomic as a PAIR rather than merely per file.
    fn lock(&self, name: &str) -> Result<FileLock, String> {
        ensure_dir(&self.root)?;
        let lock_dir = self.root.join(LOCK_DIR_NAME);
        ensure_dir(&lock_dir)?;
        FileLock::acquire(&lock_dir.join(escape_server_name(name)))
    }
}

/// An exclusive `flock(2)` held for the lifetime of the guard. A lock file is never
/// removed once created (see [`LOCK_DIR_NAME`]).
struct FileLock(fs::File);

impl FileLock {
    fn acquire(path: &Path) -> Result<Self, String> {
        let file = fs::OpenOptions::new()
            .create(true)
            .read(true)
            .write(true)
            .truncate(false)
            .mode(FILE_PERM)
            .open(path)
            .map_err(|e| format!("open credentials lock {}: {e}", path.display()))?;
        // SAFETY: `file` owns a live descriptor for the whole call.
        if unsafe { libc::flock(file.as_raw_fd(), libc::LOCK_EX) } != 0 {
            return Err(format!(
                "lock credentials {}: {}",
                path.display(),
                std::io::Error::last_os_error()
            ));
        }
        Ok(Self(file))
    }
}

impl Drop for FileLock {
    fn drop(&mut self) {
        // SAFETY: the descriptor is still owned by `self.0` until this returns.
        unsafe { libc::flock(self.0.as_raw_fd(), libc::LOCK_UN) };
    }
}

/// Create `dir` (and parents) and TIGHTEN it to 0700.
///
/// The chmod is not redundant with the create: `create_dir_all` is a no-op on a directory
/// that already exists, so a root left group- or world-readable by an older build, a
/// careless restore, or a permissive umask would keep those permissions forever — and
/// every private key in the store sits under it.
fn ensure_dir(dir: &Path) -> Result<(), String> {
    fs::create_dir_all(dir).map_err(|e| format!("create dir {}: {e}", dir.display()))?;
    fs::set_permissions(dir, fs::Permissions::from_mode(DIR_PERM))
        .map_err(|e| format!("tighten dir {}: {e}", dir.display()))
}

fn read_to_string(path: &Path) -> Result<String, String> {
    fs::read_to_string(path).map_err(|e| format!("read {}: {e}", path.display()))
}

/// Write `data` to `path` via a temp file in the SAME directory, fsynced and renamed.
///
/// The temp file is created with the final permissions before any bytes are written, so a
/// key is never briefly world-readable, and `O_EXCL` means a hostile pre-created temp path
/// fails the write rather than being followed.
fn atomic_write(path: &Path, data: &[u8]) -> Result<(), String> {
    let dir = path.parent().unwrap_or(Path::new("."));
    let tmp = dir.join(temp_name(path));
    let mut f = fs::OpenOptions::new()
        .create_new(true)
        .write(true)
        .mode(FILE_PERM)
        .open(&tmp)
        .map_err(|e| format!("create temp file {}: {e}", tmp.display()))?;
    let commit = (|| -> Result<(), String> {
        f.write_all(data)
            .map_err(|e| format!("write temp file {}: {e}", tmp.display()))?;
        f.sync_all()
            .map_err(|e| format!("fsync temp file {}: {e}", tmp.display()))?;
        fs::rename(&tmp, path).map_err(|e| format!("rename into {}: {e}", path.display()))
    })();
    if commit.is_err() {
        let _ = fs::remove_file(&tmp);
        return commit;
    }
    // Persist the rename itself. Best effort: some filesystems reject fsync on a directory
    // handle, and the rename has already succeeded.
    if let Ok(d) = fs::File::open(dir) {
        let _ = d.sync_all();
    }
    Ok(())
}

/// A collision-resistant temp basename beside `path`: pid + a process-local counter + the
/// sub-nanosecond clock. No `rand` dependency (matching the crate's dependency-minimal
/// posture) — and uniqueness is only a convenience anyway, since the create is `O_EXCL`.
fn temp_name(path: &Path) -> String {
    static SEQ: AtomicU64 = AtomicU64::new(0);
    let base = path
        .file_name()
        .map(|n| n.to_string_lossy().into_owned())
        .unwrap_or_else(|| "cred".into());
    let n = SEQ.fetch_add(1, Ordering::Relaxed);
    let nanos = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.subsec_nanos())
        .unwrap_or(0);
    format!(".{base}.tmp.{}.{n}.{nanos}", std::process::id())
}

/// Map a server name onto exactly one inert path component (mirror Go's
/// `creds.EscapeServerName` = `url.PathEscape` + the dot carve-out).
///
/// `url.PathEscape` does most of it — it encodes `/` and every other separator — but it
/// deliberately leaves `.` alone, because a dot is a legal path character. That is fine
/// for a name embedded in a longer filename and NOT fine here, where the escaped name is
/// the WHOLE component: a server named `..` would resolve `<root>/..` to the root's parent,
/// and `remove` would then recursively delete it.
///
/// Only the exact components ``/`.`/`..` carry that meaning; a name like `..foo` is already
/// inert. So those three get a leading `%2E`, which keeps the result a single ordinary
/// directory name, keeps the mapping injective, and leaves every realistic server name
/// untouched.
pub fn escape_server_name(name: &str) -> String {
    let mut out = String::with_capacity(name.len());
    for b in name.bytes() {
        if path_segment_byte_is_safe(b) {
            out.push(b as char);
        } else {
            out.push('%');
            out.push_str(&format!("{b:02X}"));
        }
    }
    match out.as_str() {
        "" | "." | ".." => format!("%2E{out}"),
        _ => out,
    }
}

/// The unescaped set of Go's `url.PathEscape` (`shouldEscape(c, encodePathSegment)`):
/// alphanumerics, the unreserved marks `-_.~`, and the reserved characters a path SEGMENT
/// may carry literally (`$ & + : = @`) — but not `/ ; , ?`, which the RFC saves for
/// assigning meaning to segments.
fn path_segment_byte_is_safe(b: u8) -> bool {
    b.is_ascii_alphanumeric()
        || matches!(b, b'-' | b'_' | b'.' | b'~')
        || matches!(b, b'$' | b'&' | b'+' | b':' | b'=' | b'@')
}

// ---------------------------------------------------------------------------
// Minimal X.509 validity reader
// ---------------------------------------------------------------------------

/// Extract a certificate's `notAfter` as unix seconds from its DER.
///
/// A deliberately tiny structural walk instead of an x509 dependency. The broker needs
/// exactly ONE field of a certificate it already trusts (it arrived over the host-key
/// pinned SSH channel and is verified by the server's CA at handshake time), and it needs
/// it for one purpose: so a credential rehydrated from disk knows when to re-mint, exactly
/// as a freshly minted one does from its bundle's `expires_at`. Pulling a full X.509
/// parser into `crates/` for that would be a poor trade against the dependency-clean
/// posture, and every malformed shape here simply returns `None` — "freshness unknown",
/// which the caller already handles.
///
/// ```text
/// Certificate ::= SEQUENCE { tbsCertificate TBSCertificate, ... }
/// TBSCertificate ::= SEQUENCE {
///     version [0] EXPLICIT Version DEFAULT v1,
///     serialNumber CertificateSerialNumber,
///     signature    AlgorithmIdentifier,
///     issuer       Name,
///     validity     Validity,          <-- the target
///     ... }
/// Validity ::= SEQUENCE { notBefore Time, notAfter Time }
/// ```
pub fn cert_not_after_unix(der: &[u8]) -> Option<i64> {
    let cert = der_content(der, Some(TAG_SEQUENCE))?; // Certificate SEQUENCE
    let tbs = der_content(cert, Some(TAG_SEQUENCE))?; // TBSCertificate SEQUENCE

    let mut rest = tbs;
    // The version is `[0] EXPLICIT` and optional (absent for a v1 cert).
    if rest.first() == Some(&TAG_CONTEXT_0) {
        rest = der_rest(rest)?;
    }
    rest = der_rest(rest)?; // serialNumber
    rest = der_rest(rest)?; // signature AlgorithmIdentifier
    rest = der_rest(rest)?; // issuer Name

    let validity = der_content(rest, Some(TAG_SEQUENCE))?;
    let after_not_before = der_rest(validity)?; // skip notBefore
    let (tag, _, content) = der_tlv(after_not_before)?;
    asn1_time_to_unix(tag, content)
}

const TAG_SEQUENCE: u8 = 0x30;
const TAG_CONTEXT_0: u8 = 0xA0;
const TAG_UTC_TIME: u8 = 0x17;
const TAG_GENERALIZED_TIME: u8 = 0x18;

/// Read the TLV at the start of `buf`, returning `(tag, total_encoded_len, content)`.
/// Only single-byte tags and definite lengths occur in DER.
fn der_tlv(buf: &[u8]) -> Option<(u8, usize, &[u8])> {
    let tag = *buf.first()?;
    let first_len = *buf.get(1)?;
    let (len, header) = if first_len < 0x80 {
        (first_len as usize, 2usize)
    } else {
        let n = (first_len & 0x7f) as usize;
        // 0 would be the indefinite form (not DER); >8 cannot fit a usize.
        if n == 0 || n > 8 {
            return None;
        }
        let mut len = 0usize;
        for b in buf.get(2..2 + n)? {
            len = len.checked_mul(256)?.checked_add(*b as usize)?;
        }
        (len, 2 + n)
    };
    let end = header.checked_add(len)?;
    Some((tag, end, buf.get(header..end)?))
}

/// The content of the leading TLV, requiring `want` when given.
fn der_content(buf: &[u8], want: Option<u8>) -> Option<&[u8]> {
    let (tag, _, content) = der_tlv(buf)?;
    if want.is_some_and(|w| w != tag) {
        return None;
    }
    Some(content)
}

/// Everything after the leading TLV, whatever its tag.
fn der_rest(buf: &[u8]) -> Option<&[u8]> {
    let (_, end, _) = der_tlv(buf)?;
    buf.get(end..)
}

/// Decode an ASN.1 `UTCTime` (0x17, `YYMMDDHHMMSSZ`) or `GeneralizedTime` (0x18,
/// `YYYYMMDDHHMMSSZ`) to unix seconds by reshaping it into RFC3339 and handing it to the
/// crate's existing strict parser. RFC 5280 pins both to UTC with seconds present, and a
/// two-digit year >= 50 means 19xx.
fn asn1_time_to_unix(tag: u8, body: &[u8]) -> Option<i64> {
    let s = std::str::from_utf8(body).ok()?;
    let s = s.strip_suffix('Z')?;
    let (year, rest) = match tag {
        TAG_UTC_TIME if s.len() == 12 => {
            let yy: u32 = s.get(..2)?.parse().ok()?;
            let full = if yy >= 50 { 1900 + yy } else { 2000 + yy };
            (full, s.get(2..)?)
        }
        TAG_GENERALIZED_TIME if s.len() == 14 => (s.get(..4)?.parse().ok()?, s.get(4..)?),
        _ => return None,
    };
    // rest = MMDDHHMMSS
    let f = |a: usize, b: usize| -> Option<&str> { rest.get(a..b) };
    let iso = format!(
        "{year:04}-{}-{}T{}:{}:{}Z",
        f(0, 2)?,
        f(2, 4)?,
        f(4, 6)?,
        f(6, 8)?,
        f(8, 10)?
    );
    crate::status::parse_rfc3339_to_unix(&iso).ok().flatten()
}

#[cfg(test)]
mod tests {
    use super::*;
    use shed_core::csr::pem_encode;
    use std::time::Duration;

    /// Issue a throwaway self-signed P-256 leaf with an explicit validity window, so the
    /// notAfter reader is exercised against a REAL certificate rather than a hand-built
    /// byte string.
    fn issue(not_after: SystemTime) -> (String, String) {
        let key = rcgen::KeyPair::generate().unwrap();
        let mut params = rcgen::CertificateParams::new(Vec::<String>::new()).unwrap();
        params
            .distinguished_name
            .push(rcgen::DnType::CommonName, "SHA256:test");
        params.not_before = not_after
            .checked_sub(Duration::from_secs(3600))
            .unwrap()
            .into();
        params.not_after = not_after.into();
        let cert = params.self_signed(&key).unwrap();
        // rcgen's own `pem` feature is deliberately off workspace-wide (see
        // shed-core's csr.rs), so the fixture uses shed-core's PEM writer.
        (
            pem_encode(PEM_LABEL_CERTIFICATE, cert.der()),
            pem_encode(shed_core::csr::PEM_LABEL_PRIVATE_KEY, &key.serialize_der()),
        )
    }

    fn store() -> (tempfile::TempDir, CredStore) {
        let dir = tempfile::tempdir().unwrap();
        let s = CredStore::new(dir.path().join("creds"));
        (dir, s)
    }

    fn mode_of(p: &Path) -> u32 {
        fs::metadata(p).unwrap().permissions().mode() & 0o777
    }

    #[test]
    fn write_then_load_round_trips_and_reads_not_after() {
        let (_d, s) = store();
        let deadline = SystemTime::now() + Duration::from_secs(24 * 3600);
        let (cert_pem, key_pem) = issue(deadline);

        assert!(s.load("mini3").unwrap().is_none(), "nothing stored yet");
        s.write("mini3", &cert_pem, &key_pem).unwrap();

        let got = s.load("mini3").unwrap().expect("stored credential");
        assert_eq!(got.cert_pem, cert_pem);
        // The leaf's notAfter is recovered to the second (rcgen truncates to seconds).
        let want = deadline.duration_since(UNIX_EPOCH).unwrap().as_secs() as i64;
        assert_eq!(got.not_after_unix, Some(want));
        assert!(!got.certified.cert.is_empty());
        // Debug must not render the private half.
        let rendered = format!("{got:?}");
        assert!(rendered.contains("<redacted>"), "{rendered}");
    }

    #[test]
    fn write_enforces_0700_dirs_and_0600_files() {
        let (_d, s) = store();
        let (cert_pem, key_pem) = issue(SystemTime::now() + Duration::from_secs(3600));
        s.write("mini3", &cert_pem, &key_pem).unwrap();

        let (cert_path, key_path) = s.paths("mini3");
        assert_eq!(mode_of(&cert_path), FILE_PERM);
        assert_eq!(mode_of(&key_path), FILE_PERM);
        assert_eq!(mode_of(&s.server_dir("mini3")), DIR_PERM);
        assert_eq!(mode_of(s.root()), DIR_PERM);
        assert_eq!(mode_of(&s.root().join(LOCK_DIR_NAME)), DIR_PERM);

        // A pre-existing loose directory is TIGHTENED, not left as it was found.
        fs::set_permissions(s.server_dir("mini3"), fs::Permissions::from_mode(0o755)).unwrap();
        s.write("mini3", &cert_pem, &key_pem).unwrap();
        assert_eq!(mode_of(&s.server_dir("mini3")), DIR_PERM);
    }

    #[test]
    fn write_leaves_no_temp_files_and_replaces_in_place() {
        let (_d, s) = store();
        let (a_cert, a_key) = issue(SystemTime::now() + Duration::from_secs(3600));
        let (b_cert, b_key) = issue(SystemTime::now() + Duration::from_secs(7200));
        s.write("mini3", &a_cert, &a_key).unwrap();
        s.write("mini3", &b_cert, &b_key).unwrap();

        let names: Vec<String> = fs::read_dir(s.server_dir("mini3"))
            .unwrap()
            .map(|e| e.unwrap().file_name().to_string_lossy().into_owned())
            .collect();
        assert_eq!(names.len(), 2, "temp residue in {names:?}");
        assert!(names.contains(&CERT_FILE_NAME.to_string()));
        assert!(names.contains(&KEY_FILE_NAME.to_string()));
        assert_eq!(s.load("mini3").unwrap().unwrap().cert_pem, b_cert);
    }

    /// A mismatched pair — what a crash between the two renames of a rotation leaves — is
    /// reported as unusable rather than assembled into a credential that would fail a
    /// handshake much later against a server that looks broken.
    #[test]
    fn mismatched_pair_is_not_a_credential() {
        let (_d, s) = store();
        let (cert_a, _key_a) = issue(SystemTime::now() + Duration::from_secs(3600));
        let (_cert_b, key_b) = issue(SystemTime::now() + Duration::from_secs(3600));
        s.write("mini3", &cert_a, &key_b).unwrap();
        let err = s.load("mini3").unwrap_err();
        assert!(err.contains("unusable"), "{err}");
    }

    /// Only the KEY on disk (the interrupted-write shape) is likewise "no credential": the
    /// key is inert without a certificate, and the recovery is a fresh enrollment.
    #[test]
    fn key_without_cert_is_not_a_credential() {
        let (_d, s) = store();
        let (cert_pem, key_pem) = issue(SystemTime::now() + Duration::from_secs(3600));
        s.write("mini3", &cert_pem, &key_pem).unwrap();
        fs::remove_file(s.paths("mini3").0).unwrap();
        assert!(s.load("mini3").is_err());
    }

    /// The key is written FIRST: a certificate write that fails must not leave a key
    /// certifying nothing, so the rollback removes it and the next load sees nothing.
    #[test]
    fn failed_cert_write_rolls_the_key_back() {
        let (_d, s) = store();
        let (cert_pem, key_pem) = issue(SystemTime::now() + Duration::from_secs(3600));
        // Make the certificate path un-writable by pre-creating it as a DIRECTORY: the
        // rename onto it fails, which is the branch under test.
        let (cert_path, key_path) = s.paths("mini3");
        ensure_dir(&s.server_dir("mini3")).unwrap();
        fs::create_dir(&cert_path).unwrap();

        assert!(s.write("mini3", &cert_pem, &key_pem).is_err());
        assert!(!key_path.exists(), "the key must have been rolled back");
    }

    #[test]
    fn escape_server_name_neutralizes_traversal() {
        // The three components that would escape the root.
        assert_eq!(escape_server_name(".."), "%2E..");
        assert_eq!(escape_server_name("."), "%2E.");
        assert_eq!(escape_server_name(""), "%2E");
        // Separators are percent-encoded, so a crafted name stays ONE component.
        assert_eq!(escape_server_name("../../.ssh"), "..%2F..%2F.ssh");
        assert!(!escape_server_name("a/b").contains('/'));
        // The lock/staging sentinels are unreachable: "%" only ever starts a real escape.
        assert_eq!(escape_server_name("%lock"), "%25lock");
        // Ordinary names are untouched (Go's PathEscape leaves these alone too).
        for name in ["mini3", "my-server", "my_server.dev", "a~b", "x@y", "p+q"] {
            assert_eq!(escape_server_name(name), name, "{name}");
        }
        // Space and non-ASCII escape to upper-case hex, matching url.PathEscape.
        assert_eq!(escape_server_name("a b"), "a%20b");
        assert_eq!(escape_server_name("é"), "%C3%A9");
    }

    #[test]
    fn a_traversal_name_writes_inside_the_root() {
        let (_d, s) = store();
        let (cert_pem, key_pem) = issue(SystemTime::now() + Duration::from_secs(3600));
        s.write("../../escape", &cert_pem, &key_pem).unwrap();
        let dir = s.server_dir("../../escape");
        assert!(dir.starts_with(s.root()), "{}", dir.display());
        assert!(dir.join(CERT_FILE_NAME).exists());
        // ...and it round-trips under the same escaped name.
        assert!(s.load("../../escape").unwrap().is_some());
    }

    #[test]
    fn remove_deletes_the_pair_and_is_idempotent() {
        let (_d, s) = store();
        let (cert_pem, key_pem) = issue(SystemTime::now() + Duration::from_secs(3600));
        s.write("mini3", &cert_pem, &key_pem).unwrap();
        s.remove("mini3").unwrap();
        assert!(s.load("mini3").unwrap().is_none());
        s.remove("mini3").unwrap(); // missing directory is not an error
                                    // The lock file survives the removal (see LOCK_DIR_NAME).
        assert!(s.root().join(LOCK_DIR_NAME).join("mini3").exists());
    }

    /// The reader accepts BOTH private-key PEM labels: this crate writes PKCS#8, the Go
    /// client writes SEC1.
    #[test]
    fn load_accepts_sec1_and_pkcs8_key_labels() {
        let (_d, s) = store();
        let (cert, pkcs8) = issue(SystemTime::now() + Duration::from_secs(3600));

        // PKCS#8 (what shed-core's ClientKeyPair emits).
        assert!(pkcs8.contains("BEGIN PRIVATE KEY"));
        s.write("pkcs8", &cert, &pkcs8).unwrap();
        assert!(s.load("pkcs8").unwrap().is_some());

        // SEC1 (what the GO credential store writes). A PKCS#8 EC key's `privateKey`
        // OCTET STRING *is* the SEC1 ECPrivateKey DER, so re-labelling it is the whole
        // conversion — and it re-uses this module's own DER walker.
        //   PrivateKeyInfo ::= SEQUENCE { version INTEGER, algorithm SEQUENCE,
        //                                 privateKey OCTET STRING }
        let pkcs8_der = pem_decode(shed_core::csr::PEM_LABEL_PRIVATE_KEY, &pkcs8).unwrap();
        let body = der_content(&pkcs8_der, Some(TAG_SEQUENCE)).unwrap();
        let after_version = der_rest(body).unwrap();
        let after_alg = der_rest(after_version).unwrap();
        let sec1_der = der_content(after_alg, Some(0x04)).unwrap();
        let sec1 = pem_encode(shed_core::csr::PEM_LABEL_EC_PRIVATE_KEY, sec1_der);
        assert!(sec1.contains("BEGIN EC PRIVATE KEY"));
        s.write("sec1", &cert, &sec1).unwrap();
        assert!(
            s.load("sec1").unwrap().is_some(),
            "a SEC1-labelled key must be readable, not rejected as unparseable"
        );

        // An unrecognized label is "no credential", not a panic or a silent accept.
        s.write(
            "weird",
            &cert,
            "-----BEGIN RSA PRIVATE KEY-----\nAA==\n-----END RSA PRIVATE KEY-----\n",
        )
        .unwrap();
        assert!(s.load("weird").is_err());
    }

    #[test]
    fn cert_not_after_handles_far_future_generalized_time() {
        // Past 2049 an X.509 validity is encoded as GeneralizedTime, not UTCTime — the
        // other branch of the time decoder.
        let deadline = UNIX_EPOCH + Duration::from_secs(4_102_444_800); // 2100-01-01Z
        let (cert_pem, _key) = issue(deadline);
        let der = pem_decode(PEM_LABEL_CERTIFICATE, &cert_pem).unwrap();
        assert_eq!(cert_not_after_unix(&der), Some(4_102_444_800));
    }

    #[test]
    fn cert_not_after_returns_none_for_garbage() {
        assert_eq!(cert_not_after_unix(b""), None);
        assert_eq!(cert_not_after_unix(b"\x30\x03\x02\x01\x00"), None);
        assert_eq!(cert_not_after_unix(&[0u8; 64]), None);
    }

    #[test]
    fn for_scope_is_state_rooted_and_scope_separated() {
        let _guard = crate::env_lock();
        std::env::set_var("SHED_HOST_AGENT_STATE_DIR", "/tmp/shed-state-test");
        let creds = CredStore::for_scope("credentials");
        assert_eq!(
            creds.root(),
            Path::new("/tmp/shed-state-test/host-agent/creds/credentials")
        );
        // A different scope is a different root — one certificate carries one scope.
        assert_ne!(creds.root(), CredStore::for_scope("control").root());
        assert_eq!(
            creds.paths("mini3").0,
            Path::new("/tmp/shed-state-test/host-agent/creds/credentials/mini3/client.pem")
        );
        std::env::remove_var("SHED_HOST_AGENT_STATE_DIR");
    }
}
