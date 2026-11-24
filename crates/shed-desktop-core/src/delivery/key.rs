//! The fingerprint key (plan 039 §3.8): 32 random bytes in the app's state directory,
//! made once, so the same content gives the same fingerprint on this machine and means
//! nothing to a server.
//!
//! The file is `<state>/delivery/fingerprint.key` (the `<state>` holding
//! `broker/machine.json` and `broker/creds`), mode `0600`, its directory `0700`. It is
//! created by writing a temporary file in the same directory and LINKING it into place,
//! which fails if the key exists: two processes starting at once both end with the one
//! key that won. An existing file that is not exactly 32 bytes, or that others can read,
//! is an error (the key is never silently replaced: every fingerprint recorded would
//! change).

use std::io::{Read, Write};
use std::os::unix::fs::{OpenOptionsExt, PermissionsExt};
use std::path::{Path, PathBuf};

use shed_provision::FingerprintKey;

/// The key file's path under the state directory `state`.
pub fn key_path(state: &Path) -> PathBuf {
    state.join("delivery").join("fingerprint.key")
}

/// The key at `path`, made when there is none.
pub fn load_or_create(path: &Path) -> Result<FingerprintKey, String> {
    match read(path) {
        Ok(Some(k)) => return Ok(k),
        Ok(None) => {}
        Err(e) => return Err(e),
    }
    let dir = path
        .parent()
        .ok_or_else(|| format!("{} has no directory", path.display()))?;
    std::fs::create_dir_all(dir).map_err(|e| format!("create {}: {e}", dir.display()))?;
    std::fs::set_permissions(dir, std::fs::Permissions::from_mode(0o700))
        .map_err(|e| format!("tighten {}: {e}", dir.display()))?;
    let mut bytes = [0u8; 32];
    std::fs::File::open("/dev/urandom")
        .and_then(|mut f| f.read_exact(&mut bytes))
        .map_err(|e| format!("random bytes: {e}"))?;
    let tmp = dir.join(format!(".fingerprint.key.{}", std::process::id()));
    let _ = std::fs::remove_file(&tmp);
    let written = std::fs::OpenOptions::new()
        .write(true)
        .create_new(true)
        .mode(0o600)
        .open(&tmp)
        .and_then(|mut f| {
            f.write_all(&bytes)?;
            f.sync_all()
        });
    if let Err(e) = written {
        let _ = std::fs::remove_file(&tmp);
        return Err(format!("write {}: {e}", tmp.display()));
    }
    let linked = std::fs::hard_link(&tmp, path);
    let _ = std::fs::remove_file(&tmp);
    match linked {
        Ok(()) => {
            if let Ok(d) = std::fs::File::open(dir) {
                let _ = d.sync_all();
            }
            Ok(FingerprintKey::from_bytes(bytes))
        }
        // Another process made it first: theirs is the key.
        Err(e) if e.kind() == std::io::ErrorKind::AlreadyExists => {
            read(path)?.ok_or_else(|| format!("{} vanished while it was made", path.display()))
        }
        Err(e) => Err(format!("create {}: {e}", path.display())),
    }
}

fn read(path: &Path) -> Result<Option<FingerprintKey>, String> {
    let f = match std::fs::File::open(path) {
        Ok(f) => f,
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => return Ok(None),
        Err(e) => return Err(format!("read {}: {e}", path.display())),
    };
    let meta = f
        .metadata()
        .map_err(|e| format!("read {}: {e}", path.display()))?;
    if !meta.is_file() || meta.permissions().mode() & 0o077 != 0 {
        return Err(format!(
            "{} is not a file only its owner can read",
            path.display()
        ));
    }
    let mut buf = Vec::new();
    f.take(33)
        .read_to_end(&mut buf)
        .map_err(|e| format!("read {}: {e}", path.display()))?;
    let bytes: [u8; 32] = buf
        .try_into()
        .map_err(|_| format!("{} is not a 32-byte key", path.display()))?;
    Ok(Some(FingerprintKey::from_bytes(bytes)))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_key_is_made_once_with_owner_only_modes() {
        let dir = tempfile::tempdir().unwrap();
        let path = key_path(dir.path());
        let a = load_or_create(&path).unwrap();
        let b = load_or_create(&path).unwrap();
        assert_eq!(a.as_bytes(), b.as_bytes());
        assert_eq!(
            std::fs::metadata(&path).unwrap().permissions().mode() & 0o777,
            0o600
        );
        assert_eq!(
            std::fs::metadata(path.parent().unwrap())
                .unwrap()
                .permissions()
                .mode()
                & 0o777,
            0o700
        );
        assert_ne!(a.as_bytes(), &[0u8; 32]);
        let other = tempfile::tempdir().unwrap();
        let c = load_or_create(&key_path(other.path())).unwrap();
        assert_ne!(a.as_bytes(), c.as_bytes(), "random per machine");
    }

    #[test]
    fn a_bad_key_file_is_an_error_not_replaced() {
        let dir = tempfile::tempdir().unwrap();
        let path = key_path(dir.path());
        std::fs::create_dir_all(path.parent().unwrap()).unwrap();
        std::fs::write(&path, b"short").unwrap();
        std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o600)).unwrap();
        assert!(load_or_create(&path).unwrap_err().contains("32-byte"));
        assert_eq!(std::fs::read(&path).unwrap(), b"short");
        std::fs::write(&path, [7u8; 32]).unwrap();
        std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o644)).unwrap();
        assert!(load_or_create(&path).unwrap_err().contains("owner"));
    }
}
