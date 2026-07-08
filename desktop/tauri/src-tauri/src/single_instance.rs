//! Single-instance guard: flock a pidfile *before* the IPC socket is bound, so a
//! second launch never unlinks the live socket. On contention the caller sends an
//! `app.activate` IPC op to the running instance and exits (roost's pattern).
//!
//! Deliberately NOT the `tauri-plugin-single-instance`: that keys its singleton on
//! the app *identifier* (a global D-Bus name / mach port), so two hermetic test
//! runs — or a dev instance — would collide. The flock is keyed to the socket's
//! runtime dir instead, so a throwaway `XDG_RUNTIME_DIR` isolates each run.

use std::fs::{File, OpenOptions};
use std::io::{Read, Seek, SeekFrom, Write};
use std::os::unix::net::UnixStream;
use std::path::{Path, PathBuf};
use std::time::Duration;

use fs2::FileExt;

/// Held for the whole process lifetime; the advisory lock releases when this
/// `File`'s descriptor closes (i.e. the process exits). Drop deliberately does
/// NOT unlink the pidfile — the lock release is the signal, and a stale pidfile
/// is harmless (the next winner truncates it).
pub struct InstanceLock {
    // Never read — the field's lifetime IS the lock; hence the allowance.
    #[allow(dead_code)]
    file: File,
}

/// Why [`acquire`] didn't return a lock.
#[derive(Debug)]
pub enum AcquireError {
    /// Another live instance holds the lock; carries its PID (0 if unreadable).
    AlreadyHeld(u32),
    /// The lock file couldn't be opened/locked for an unexpected reason.
    Io(std::io::Error),
}

impl From<std::io::Error> for AcquireError {
    fn from(e: std::io::Error) -> Self {
        AcquireError::Io(e)
    }
}

/// The pidfile beside the IPC socket (`<socket_dir>/shed-tauri.lock`), so the
/// guard is scoped to the same runtime dir the socket lives in.
pub fn lock_path_for(socket_path: &Path) -> PathBuf {
    socket_path
        .parent()
        .unwrap_or_else(|| Path::new("/tmp"))
        .join("shed-tauri.lock")
}

/// Try to become the single instance by flocking `lock_path`. `Ok(lock)` → we own
/// it (keep the guard alive for the whole run); `Err(AlreadyHeld(pid))` → another
/// instance does. TOCTOU-safe: the file is opened without truncation so a losing
/// caller can still read the holder's PID, and it's truncated only after winning.
pub fn acquire(lock_path: &Path) -> Result<InstanceLock, AcquireError> {
    if let Some(parent) = lock_path.parent() {
        std::fs::create_dir_all(parent)?;
    }
    let mut file = OpenOptions::new()
        .create(true)
        .truncate(false)
        .read(true)
        .write(true)
        .open(lock_path)?;

    if let Err(e) = file.try_lock_exclusive() {
        let pid = read_pid(&mut file).unwrap_or(0);
        return Err(match e.kind() {
            std::io::ErrorKind::WouldBlock => AcquireError::AlreadyHeld(pid),
            _ => AcquireError::Io(e),
        });
    }

    // We own it — record our PID (truncate first to clear a prior holder's).
    file.set_len(0)?;
    file.seek(SeekFrom::Start(0))?;
    write!(file, "{}", std::process::id())?;
    file.flush()?;
    Ok(InstanceLock { file })
}

fn read_pid(file: &mut File) -> Option<u32> {
    file.seek(SeekFrom::Start(0)).ok()?;
    let mut s = String::new();
    file.read_to_string(&mut s).ok()?;
    s.trim().parse().ok()
}

/// Tell the already-running instance to raise its window: connect to its socket,
/// send one `app.activate` frame, and read the reply (best-effort). A blocking
/// std client, so this exit path needs no tokio runtime.
pub fn activate_running_instance(socket_path: &Path) -> std::io::Result<()> {
    let mut stream = UnixStream::connect(socket_path)?;
    stream.set_read_timeout(Some(Duration::from_secs(2)))?;
    let mut frame = serde_json::to_vec(&serde_json::json!({
        "id": "activate", "op": "app.activate", "params": {},
    }))
    .unwrap_or_default();
    frame.push(b'\n');
    stream.write_all(&frame)?;
    stream.flush()?;
    // Best-effort read of the single reply line so the primary finishes handling.
    let mut buf = [0u8; 256];
    let _ = stream.read(&mut buf);
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn second_acquire_reports_already_held_with_pid() {
        let dir = tempfile::tempdir().unwrap();
        let lock = dir.path().join("shed-tauri.lock");
        let _first = acquire(&lock).expect("first acquire wins the lock");
        match acquire(&lock) {
            Err(AcquireError::AlreadyHeld(pid)) => {
                assert_eq!(pid, std::process::id(), "reports the holder's pid");
            }
            Err(AcquireError::Io(e)) => panic!("unexpected io error: {e}"),
            Ok(_) => panic!("second acquire unexpectedly won the held lock"),
        }
    }

    #[test]
    fn reacquire_after_release_succeeds() {
        let dir = tempfile::tempdir().unwrap();
        let lock = dir.path().join("shed-tauri.lock");
        {
            let _first = acquire(&lock).expect("first acquire");
        } // dropping the guard closes the fd → releases the flock
        acquire(&lock).expect("re-acquire after the first instance is gone");
    }
}
