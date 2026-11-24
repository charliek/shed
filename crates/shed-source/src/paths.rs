//! Where the broker keeps its persistent state, and the home-directory helpers it is
//! built from.

use std::ffi::OsString;
use std::path::PathBuf;

/// `user_home_dir` returns `$HOME`, falling back to `/tmp` when unset (mirrors the
/// Go daemon's `userHomeDir` → `os.UserHomeDir` with a `/tmp` fallback). Moved here
/// from the retired broker's `config` (plan 039 §3.7).
pub fn user_home_dir() -> PathBuf {
    match std::env::var_os("HOME") {
        Some(h) if !h.is_empty() => PathBuf::from(h),
        _ => PathBuf::from("/tmp"),
    }
}

/// `expand_tilde` expands a leading `~/` to the user's home directory (mirrors the
/// Go daemon's `expandTilde`). Moved here from the retired broker's `config` (plan
/// 039 §3.7).
pub fn expand_tilde(path: &str) -> String {
    if let Some(rest) = path.strip_prefix("~/") {
        return user_home_dir().join(rest).to_string_lossy().into_owned();
    }
    path.to_string()
}

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
    fn expand_tilde_expands_home_prefix_only() {
        assert_eq!(expand_tilde("/abs/path"), "/abs/path");
        assert_eq!(expand_tilde("plain"), "plain");
        let home = user_home_dir();
        assert_eq!(
            expand_tilde("~/x/y"),
            home.join("x/y").to_string_lossy().into_owned()
        );
    }

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
