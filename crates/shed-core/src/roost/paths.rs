//! Where the local `roost-session` socket is.
//!
//! ## Why shed resolves this itself
//!
//! roost ships a resolver — `roost_ipc::paths::BundleProfile::session()` — and
//! shed deliberately does **not** call it. That resolver picks between
//! `roost-session` and `roost-session-dev` on `cfg!(debug_assertions)`, and
//! `cfg!` in a library crate reads the profile of whatever is **consuming** it.
//! A debug build of shed (every `cargo test`, every dev run of the Tauri app)
//! would therefore go looking for a *dev* roost and quietly find nothing, while
//! a release build of the same source found the real one. The bug is invisible
//! until it isn't. (A build-profile-independent resolver is the recorded ask
//! upstream; until then, this is the fix.)
//!
//! ## The table
//!
//! | | release | `-dev` sibling |
//! |---|---|---|
//! | Linux, `$XDG_RUNTIME_DIR` set | `$XDG_RUNTIME_DIR/roost-session/roost.sock` | `$XDG_RUNTIME_DIR/roost-session-dev/roost.sock` |
//! | Linux, no `$XDG_RUNTIME_DIR` | `/tmp/roost-session-<uid>/roost.sock` | `/tmp/roost-session-dev-<uid>/roost.sock` |
//! | macOS | `$HOME/Library/Caches/RoostSession/roost.sock` | `$HOME/Library/Caches/RoostSessionDev/roost.sock` |
//! | macOS, no usable `$HOME` | `/tmp/RoostSession/roost.sock` | `/tmp/RoostSessionDev/roost.sock` |
//!
//! `SHED_ROOST_SOCKET` overrides everything. Otherwise: **prefer release**, fall
//! back to the `-dev` sibling only when the release path does not exist, and
//! when neither exists hand back the release path with both listed in
//! [`ResolvedSocket::tried`] — because "no roost-session" with no path in it is
//! the least actionable message this module could produce.
//!
//! The XDG-vs-`/tmp` choice is made from the environment, not from what exists,
//! because that is how roost itself decides where to bind: `$XDG_RUNTIME_DIR`
//! counts when it is set, non-empty and absolute, and never otherwise.
//!
//! Keep this table in step with roost's `docs/reference/paths.md` by hand on
//! each `roost-ipc` bump; the tests below pin every cell.

use std::collections::BTreeMap;
use std::path::{Path, PathBuf};

/// The env var that overrides path resolution entirely.
pub const SOCKET_ENV: &str = "SHED_ROOST_SOCKET";

/// Which path table to apply. A parameter rather than a `cfg!` so both are
/// unit-testable from one build — the same discipline roost's own Linux
/// resolver uses, and the discipline whose absence is the `-dev` trap above.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Os {
    Linux,
    MacOs,
}

impl Os {
    /// The OS this build is running on.
    pub fn host() -> Os {
        if cfg!(target_os = "macos") {
            Os::MacOs
        } else {
            Os::Linux
        }
    }
}

/// A resolved socket path, plus everything that was considered getting there.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ResolvedSocket {
    /// Where to dial. Existence is **not** promised: when nothing is there this
    /// is the release path, so the caller's failure message names the place a
    /// session would normally be.
    pub path: PathBuf,
    /// Every candidate, release first. What an `Unavailable` reason quotes.
    pub tried: Vec<PathBuf>,
}

impl ResolvedSocket {
    /// The candidates as one line, for an error message.
    pub fn tried_display(&self) -> String {
        self.tried
            .iter()
            .map(|p| p.display().to_string())
            .collect::<Vec<_>>()
            .join(", ")
    }
}

/// Resolve the local session socket against this process and this filesystem.
pub fn local_session_socket() -> ResolvedSocket {
    let mut env = BTreeMap::new();
    for key in [SOCKET_ENV, "XDG_RUNTIME_DIR", "HOME"] {
        if let Ok(value) = std::env::var(key) {
            env.insert(key.to_string(), value);
        }
    }
    resolve_session_socket(Os::host(), &env, &|path| path.exists(), getuid())
}

/// The pure resolver: no process env, no filesystem, no `cfg!`.
pub fn resolve_session_socket(
    os: Os,
    env: &BTreeMap<String, String>,
    exists: &dyn Fn(&Path) -> bool,
    uid: u32,
) -> ResolvedSocket {
    if let Some(override_path) = env.get(SOCKET_ENV).filter(|v| !v.is_empty()) {
        let path = PathBuf::from(override_path);
        return ResolvedSocket {
            tried: vec![path.clone()],
            path,
        };
    }

    let (release, dev) = candidates(os, env, uid);
    // Prefer release; the `-dev` sibling only stands in for an absent one. With
    // neither present the answer is still release, so the caller's failure
    // message names the place a session would normally be.
    let path = if !exists(&release) && exists(&dev) {
        dev.clone()
    } else {
        release.clone()
    };
    ResolvedSocket {
        path,
        tried: vec![release, dev],
    }
}

/// `(release, dev)` for one OS — the table in the module docs.
fn candidates(os: Os, env: &BTreeMap<String, String>, uid: u32) -> (PathBuf, PathBuf) {
    match os {
        Os::Linux => match absolute(env.get("XDG_RUNTIME_DIR")) {
            Some(runtime) => (
                runtime.join("roost-session").join("roost.sock"),
                runtime.join("roost-session-dev").join("roost.sock"),
            ),
            // roost's own HOME-less/XDG-less fallback: `/tmp/<namespace>-<uid>`,
            // with the namespace already carrying the `-dev` suffix.
            None => (
                PathBuf::from(format!("/tmp/roost-session-{uid}")).join("roost.sock"),
                PathBuf::from(format!("/tmp/roost-session-dev-{uid}")).join("roost.sock"),
            ),
        },
        Os::MacOs => match absolute(env.get("HOME")) {
            Some(home) => {
                let caches = home.join("Library/Caches");
                (
                    caches.join("RoostSession").join("roost.sock"),
                    caches.join("RoostSessionDev").join("roost.sock"),
                )
            }
            None => (
                PathBuf::from("/tmp/RoostSession/roost.sock"),
                PathBuf::from("/tmp/RoostSessionDev/roost.sock"),
            ),
        },
    }
}

/// A directory env var counts only when it is set, non-empty and absolute —
/// the XDG spec's own rule, and what keeps a stray relative value from
/// resolving a socket against the process CWD.
fn absolute(raw: Option<&String>) -> Option<PathBuf> {
    let path = PathBuf::from(raw?);
    (!path.as_os_str().is_empty() && path.is_absolute()).then_some(path)
}

/// The **real** uid, deliberately not the effective one: it names roost's
/// `/tmp/<namespace>-<uid>` fallback directory, and roost stamps it with
/// `getuid`.
fn getuid() -> u32 {
    // SAFETY: `getuid` reads process-global state, takes no arguments and
    // cannot fail; there is no precondition to uphold.
    unsafe { libc::getuid() }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn env(pairs: &[(&str, &str)]) -> BTreeMap<String, String> {
        pairs
            .iter()
            .map(|(k, v)| (k.to_string(), v.to_string()))
            .collect()
    }

    fn nothing_exists(_: &Path) -> bool {
        false
    }

    fn only(paths: &'static [&'static str]) -> impl Fn(&Path) -> bool {
        move |candidate: &Path| paths.iter().any(|p| Path::new(p) == candidate)
    }

    #[test]
    fn linux_uses_the_runtime_dir() {
        let resolved = resolve_session_socket(
            Os::Linux,
            &env(&[("XDG_RUNTIME_DIR", "/run/user/1000")]),
            &nothing_exists,
            1000,
        );
        assert_eq!(
            resolved.path,
            PathBuf::from("/run/user/1000/roost-session/roost.sock")
        );
        assert_eq!(
            resolved.tried,
            vec![
                PathBuf::from("/run/user/1000/roost-session/roost.sock"),
                PathBuf::from("/run/user/1000/roost-session-dev/roost.sock"),
            ]
        );
    }

    #[test]
    fn linux_falls_back_to_tmp_when_the_runtime_dir_is_unusable() {
        for unusable in [None, Some(""), Some("relative/dir")] {
            let env = match unusable {
                Some(value) => env(&[("XDG_RUNTIME_DIR", value)]),
                None => env(&[]),
            };
            let resolved = resolve_session_socket(Os::Linux, &env, &nothing_exists, 501);
            assert_eq!(
                resolved.path,
                PathBuf::from("/tmp/roost-session-501/roost.sock"),
                "XDG_RUNTIME_DIR = {unusable:?}"
            );
            assert_eq!(
                resolved.tried[1],
                PathBuf::from("/tmp/roost-session-dev-501/roost.sock"),
                "XDG_RUNTIME_DIR = {unusable:?}"
            );
        }
    }

    #[test]
    fn macos_uses_the_caches_dir() {
        let resolved = resolve_session_socket(
            Os::MacOs,
            &env(&[("HOME", "/Users/me")]),
            &nothing_exists,
            501,
        );
        assert_eq!(
            resolved.path,
            PathBuf::from("/Users/me/Library/Caches/RoostSession/roost.sock")
        );
        assert_eq!(
            resolved.tried[1],
            PathBuf::from("/Users/me/Library/Caches/RoostSessionDev/roost.sock")
        );
    }

    #[test]
    fn macos_without_a_usable_home_falls_back_to_tmp() {
        let resolved =
            resolve_session_socket(Os::MacOs, &env(&[("HOME", "")]), &nothing_exists, 501);
        assert_eq!(resolved.path, PathBuf::from("/tmp/RoostSession/roost.sock"));
        assert_eq!(
            resolved.tried[1],
            PathBuf::from("/tmp/RoostSessionDev/roost.sock")
        );
    }

    #[test]
    fn the_override_wins_over_everything() {
        let resolved = resolve_session_socket(
            Os::Linux,
            &env(&[
                (SOCKET_ENV, "/tmp/mine/roost.sock"),
                ("XDG_RUNTIME_DIR", "/run/user/1000"),
            ]),
            // Even with the release path present, the override is the answer.
            &only(&["/run/user/1000/roost-session/roost.sock"]),
            1000,
        );
        assert_eq!(resolved.path, PathBuf::from("/tmp/mine/roost.sock"));
        assert_eq!(resolved.tried, vec![PathBuf::from("/tmp/mine/roost.sock")]);

        // An empty override is not an override — it is an unset variable that
        // something exported anyway.
        let resolved = resolve_session_socket(
            Os::Linux,
            &env(&[(SOCKET_ENV, ""), ("XDG_RUNTIME_DIR", "/run/user/1000")]),
            &nothing_exists,
            1000,
        );
        assert_eq!(
            resolved.path,
            PathBuf::from("/run/user/1000/roost-session/roost.sock")
        );
    }

    #[test]
    fn the_dev_sibling_is_used_only_when_the_release_path_is_absent() {
        let env = env(&[("XDG_RUNTIME_DIR", "/run/user/1000")]);

        // Only dev present → dev.
        let resolved = resolve_session_socket(
            Os::Linux,
            &env,
            &only(&["/run/user/1000/roost-session-dev/roost.sock"]),
            1000,
        );
        assert_eq!(
            resolved.path,
            PathBuf::from("/run/user/1000/roost-session-dev/roost.sock")
        );

        // Both present → release wins.
        let resolved = resolve_session_socket(
            Os::Linux,
            &env,
            &only(&[
                "/run/user/1000/roost-session/roost.sock",
                "/run/user/1000/roost-session-dev/roost.sock",
            ]),
            1000,
        );
        assert_eq!(
            resolved.path,
            PathBuf::from("/run/user/1000/roost-session/roost.sock")
        );

        // Neither present → the release path, with both named.
        let resolved = resolve_session_socket(Os::Linux, &env, &nothing_exists, 1000);
        assert_eq!(
            resolved.path,
            PathBuf::from("/run/user/1000/roost-session/roost.sock")
        );
        assert_eq!(resolved.tried.len(), 2);
        assert!(resolved.tried_display().contains("roost-session-dev"));
    }

    #[test]
    fn the_macos_dev_sibling_is_reachable_too() {
        let resolved = resolve_session_socket(
            Os::MacOs,
            &env(&[("HOME", "/Users/me")]),
            &only(&["/Users/me/Library/Caches/RoostSessionDev/roost.sock"]),
            501,
        );
        assert_eq!(
            resolved.path,
            PathBuf::from("/Users/me/Library/Caches/RoostSessionDev/roost.sock")
        );
    }

    /// The host resolver must agree with the pure one for this machine — the
    /// only thing `local_session_socket` adds is reading the real env.
    #[test]
    fn the_host_resolver_answers_a_candidate_from_the_table() {
        let resolved = local_session_socket();
        assert!(!resolved.tried.is_empty());
        assert!(resolved.tried.contains(&resolved.path));
    }
}
