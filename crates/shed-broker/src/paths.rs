//! Where the broker keeps its persistent state.

use std::ffi::OsString;
use std::path::PathBuf;

use crate::config::user_home_dir;

/// `state_dir` returns the directory holding the broker's own PERSISTENT state: its
/// client-credential store ([`crate::credstore`]) and this machine's identity
/// ([`crate::machine`]).
///
/// Never `$XDG_RUNTIME_DIR`: that is a per-boot tmpfs, exactly the wrong home for a
/// private key (the broker would silently re-enroll over SSH on every reboot). State
/// goes to the durable per-user data dir instead.
///
/// Order: the `SHED_BROKER_STATE_DIR` override, else macOS
/// `~/Library/Application Support/shed`, else `$XDG_DATA_HOME/shed`, else
/// `~/.local/share/shed`.
pub fn state_dir() -> PathBuf {
    if let Some(d) = env_nonempty("SHED_BROKER_STATE_DIR") {
        return PathBuf::from(d);
    }
    if cfg!(target_os = "macos") {
        return user_home_dir()
            .join("Library")
            .join("Application Support")
            .join("shed");
    }
    if let Some(d) = env_nonempty("XDG_DATA_HOME") {
        return PathBuf::from(d).join("shed");
    }
    user_home_dir().join(".local").join("share").join("shed")
}

/// The value of an env var, or `None` when unset or empty. Uses the OS string so a
/// non-UTF-8 path still works.
fn env_nonempty(key: &str) -> Option<OsString> {
    match std::env::var_os(key) {
        Some(v) if !v.is_empty() => Some(v),
        _ => None,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn state_dir_honors_env_override() {
        let _guard = crate::env_lock();
        std::env::set_var("SHED_BROKER_STATE_DIR", "/custom/shed/state");
        assert_eq!(state_dir(), PathBuf::from("/custom/shed/state"));
        std::env::remove_var("SHED_BROKER_STATE_DIR");
    }

    /// `$XDG_RUNTIME_DIR` is a tmpfs and must never hold a persisted private key.
    #[test]
    fn state_dir_never_follows_xdg_runtime_dir() {
        let _guard = crate::env_lock();
        std::env::remove_var("SHED_BROKER_STATE_DIR");
        std::env::remove_var("XDG_DATA_HOME");
        std::env::set_var("XDG_RUNTIME_DIR", "/run/user/1000");

        let state = state_dir();
        assert!(
            !state.starts_with("/run/user/1000"),
            "state dir must not live on the runtime tmpfs: {}",
            state.display()
        );
        if cfg!(target_os = "macos") {
            assert!(state.ends_with("Library/Application Support/shed"));
        } else {
            assert!(state.ends_with(".local/share/shed"));
        }
        std::env::remove_var("XDG_RUNTIME_DIR");
    }
}
