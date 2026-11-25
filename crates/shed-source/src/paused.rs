//! The servers this machine does not answer on (plan 040 §3.9, "Stop answering from
//! here"): `<state>/broker/paused-servers.json`, beside the machine identity.
//!
//! An unbind alone does not stop a running source: the server binds the first of the
//! user's machines whose stream arrives next, and this machine's stream would arrive
//! within a second. So the desktop keeps the servers a person stopped answering on, by
//! their name in the shed CLI config, and holds no stream for them until "Answer from
//! here" takes the server out again. **A pause lasts until then, and nothing else lifts
//! it**: a server that leaves the config (or whose entry is briefly unreadable, or is
//! removed and added back) keeps its name in the set. A name no configured server has
//! is inert; a server added back under it is paused, and says so, until "Answer from
//! here".
//!
//! The file is `{"version":1,"servers":[…]}`, written whole through the credential
//! store's atomic writer (a `0600` temp file beside it, fsynced, renamed). A missing
//! file is the empty set. **A file that exists and cannot be read, parsed, or is of
//! another version fails closed**: every server counts as paused ([`PausedError`] says
//! why) until a pause or resume writes the file afresh, holding every configured server
//! but the one resumed.

use std::collections::BTreeSet;
use std::path::{Path, PathBuf};
use std::sync::Mutex;

use serde::{Deserialize, Serialize};

use crate::credstore::{atomic_write, ensure_dir};

/// The file's name, in `<state>/broker/`.
pub const PAUSED_FILE_NAME: &str = "paused-servers.json";

/// The file's path: `<state>/broker/paused-servers.json`.
pub fn paused_state_path() -> PathBuf {
    crate::paths::state_dir()
        .join("broker")
        .join(PAUSED_FILE_NAME)
}

/// What is wrong with the paused set: a closed list, never a file's text.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum PausedError {
    /// The file exists and could not be read: every server is paused.
    Unreadable,
    /// The file is not the set's JSON: every server is paused.
    Unparseable,
    /// The file is of a version this build does not know: every server is paused.
    UnknownVersion,
    /// A change could not be written (the set in use is the last one written; a
    /// removal pending since a server left the config is tried again at the next
    /// reconcile).
    WriteFailed,
}

#[derive(Serialize, Deserialize)]
struct OnDisk {
    version: u32,
    servers: BTreeSet<String>,
}

#[derive(Debug, Default)]
struct State {
    /// The set as last written (or read).
    set: BTreeSet<String>,
    /// The file could not be used: every server is paused until a write.
    load_error: Option<PausedError>,
    /// The last write failed.
    write_failed: bool,
}

/// The set, held in memory and written through on every change.
#[derive(Debug)]
pub struct PausedServers {
    path: PathBuf,
    state: Mutex<State>,
}

fn read(path: &Path) -> Result<BTreeSet<String>, PausedError> {
    let bytes = match std::fs::read(path) {
        Ok(b) => b,
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => return Ok(BTreeSet::new()),
        Err(_) => return Err(PausedError::Unreadable),
    };
    let value: serde_json::Value =
        serde_json::from_slice(&bytes).map_err(|_| PausedError::Unparseable)?;
    match value.get("version").and_then(serde_json::Value::as_u64) {
        Some(1) => {}
        Some(_) => return Err(PausedError::UnknownVersion),
        None => return Err(PausedError::Unparseable),
    }
    serde_json::from_value::<OnDisk>(value)
        .map(|d| d.servers)
        .map_err(|_| PausedError::Unparseable)
}

impl PausedServers {
    /// The set stored at `path`: empty when there is no file, every server paused when
    /// the file cannot be used ([`Self::error`]).
    pub fn load(path: PathBuf) -> Self {
        let state = match read(&path) {
            Ok(set) => State {
                set,
                ..State::default()
            },
            Err(e) => State {
                load_error: Some(e),
                ..State::default()
            },
        };
        PausedServers {
            path,
            state: Mutex::new(state),
        }
    }

    /// Where it is stored.
    pub fn path(&self) -> &Path {
        &self.path
    }

    /// Whether this machine does not answer on `server` (every server, while the file
    /// cannot be used).
    pub fn contains(&self, server: &str) -> bool {
        let st = self.lock();
        st.load_error.is_some() || st.set.contains(server)
    }

    /// The servers the file holds as paused, by name (none named while it cannot be
    /// used: then every server is paused).
    pub fn names(&self) -> Vec<String> {
        self.lock().set.iter().cloned().collect()
    }

    /// What is wrong with the set: the file could not be used (every server paused),
    /// or the last write failed. `None` when all is well.
    pub fn error(&self) -> Option<PausedError> {
        let st = self.lock();
        st.load_error
            .or(st.write_failed.then_some(PausedError::WriteFailed))
    }

    /// Stop answering on `server`; written before it is answered, and on a failed write
    /// the set is as it was. `configured` (the config's servers) is what the set starts
    /// from while the file cannot be used: all of them stay paused.
    pub fn pause(&self, server: &str, configured: &[&str]) -> Result<(), String> {
        self.change(configured, |s| {
            s.insert(server.to_string());
        })
    }

    /// Answer on `server` again (see [`Self::pause`] for `configured`).
    pub fn resume(&self, server: &str, configured: &[&str]) -> Result<(), String> {
        self.change(configured, |s| {
            s.remove(server);
        })
    }

    fn lock(&self) -> std::sync::MutexGuard<'_, State> {
        self.state.lock().unwrap_or_else(|e| e.into_inner())
    }

    fn write(&self, set: &BTreeSet<String>) -> Result<(), String> {
        let body = serde_json::to_vec(&OnDisk {
            version: 1,
            servers: set.clone(),
        })
        .map_err(|e| e.to_string())?;
        if let Some(dir) = self.path.parent() {
            ensure_dir(dir)?;
        }
        atomic_write(&self.path, &body)
    }

    /// Applies `f` to the set in use (every configured server, while the file cannot
    /// be used) and writes the result; only a written result is kept.
    fn change(
        &self,
        configured: &[&str],
        f: impl FnOnce(&mut BTreeSet<String>),
    ) -> Result<(), String> {
        let mut st = self.lock();
        let mut next = match st.load_error {
            Some(_) => configured.iter().map(|s| s.to_string()).collect(),
            None => st.set.clone(),
        };
        f(&mut next);
        if st.load_error.is_none() && next == st.set {
            return Ok(());
        }
        match self.write(&next) {
            Ok(()) => {
                st.set = next;
                st.load_error = None;
                st.write_failed = false;
                Ok(())
            }
            Err(e) => {
                st.write_failed = true;
                Err(e)
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::os::unix::fs::PermissionsExt;

    const ALL: &[&str] = &["desk", "mini", "other"];

    #[test]
    fn a_change_is_written_and_read_back() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("broker").join(PAUSED_FILE_NAME);
        let p = PausedServers::load(path.clone());
        assert!(
            p.names().is_empty() && p.error().is_none(),
            "no file is an empty set"
        );
        p.pause("mini", ALL).unwrap();
        p.pause("mini", ALL).unwrap();
        p.pause("desk", ALL).unwrap();
        assert!(p.contains("mini") && !p.contains("other"));
        let mode = std::fs::metadata(&path).unwrap().permissions().mode() & 0o777;
        assert_eq!(mode, 0o600);
        assert_eq!(
            std::fs::read_to_string(&path).unwrap(),
            r#"{"version":1,"servers":["desk","mini"]}"#
        );
        let again = PausedServers::load(path.clone());
        assert_eq!(again.names(), ["desk", "mini"]);
        again.resume("mini", ALL).unwrap();
        assert_eq!(PausedServers::load(path).names(), ["desk"]);
    }

    /// A pause lasts until it is lifted: a server that leaves the config keeps its name
    /// in the set (inert while no server has it), and one added back under it is paused.
    #[test]
    fn a_pause_survives_the_server_leaving_the_config() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join(PAUSED_FILE_NAME);
        let p = PausedServers::load(path.clone());
        p.pause("gone", ALL).unwrap();
        p.pause("kept", ALL).unwrap();
        // Another change made while "gone" is not configured keeps it.
        p.pause("new", &["kept", "new"]).unwrap();
        assert!(p.contains("gone"));
        assert_eq!(
            PausedServers::load(path.clone()).names(),
            ["gone", "kept", "new"]
        );
        // Only "Answer from here" lifts it.
        p.resume("gone", &["gone", "kept", "new"]).unwrap();
        assert!(!p.contains("gone"));
        assert_eq!(PausedServers::load(path).names(), ["kept", "new"]);
    }

    /// Finding 1: a file that exists and cannot be used fails closed — every server is
    /// paused and the error class says why — until a change writes it afresh from the
    /// configured servers, the one resumed left out.
    #[test]
    fn an_unusable_file_pauses_every_server_until_it_is_written() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join(PAUSED_FILE_NAME);
        for (text, class) in [
            ("{not json", PausedError::Unparseable),
            ("[]", PausedError::Unparseable),
            (r#"{"servers":["mini"]}"#, PausedError::Unparseable),
            (
                r#"{"version":1,"servers":"mini"}"#,
                PausedError::Unparseable,
            ),
            (
                r#"{"version":2,"servers":["mini"]}"#,
                PausedError::UnknownVersion,
            ),
        ] {
            std::fs::write(&path, text).unwrap();
            let p = PausedServers::load(path.clone());
            assert_eq!(p.error(), Some(class), "{text}");
            assert!(p.contains("mini") && p.contains("anything"), "{text}");
            assert_eq!(
                std::fs::read_to_string(&path).unwrap(),
                text,
                "nothing written"
            );
        }
        let p = PausedServers::load(path.clone());
        p.resume("mini", ALL).unwrap();
        assert_eq!(p.error(), None);
        assert!(!p.contains("mini") && p.contains("desk") && p.contains("other"));
        assert_eq!(PausedServers::load(path).names(), ["desk", "other"]);
    }

    #[test]
    fn an_unreadable_file_pauses_every_server() {
        let dir = tempfile::tempdir().unwrap();
        // A directory where the file should be: it exists and cannot be read.
        let path = dir.path().join(PAUSED_FILE_NAME);
        std::fs::create_dir(&path).unwrap();
        let p = PausedServers::load(path);
        assert_eq!(p.error(), Some(PausedError::Unreadable));
        assert!(p.contains("mini"));
    }

    /// A write that fails leaves the set as it was, and says so.
    #[test]
    fn a_failed_write_changes_nothing() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join(PAUSED_FILE_NAME);
        let p = PausedServers::load(path.clone());
        // A directory where the file goes: the rename over it fails.
        std::fs::create_dir(&path).unwrap();
        assert!(p.pause("mini", ALL).is_err());
        assert!(!p.contains("mini"));
        assert_eq!(p.error(), Some(PausedError::WriteFailed));
    }
}
