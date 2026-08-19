//! The per-tool preseed dispatch — the Rust twin of `AgentSpec.Preseed`
//! (`internal/ext/rc/agents.go:423,494`) and its best-effort invocation at
//! `ops.go:201`.
//!
//! Two tools declare one: **claude** (both its kinds) seeds trust + onboarding in
//! `~/.claude.json` ([`super::trust`]), and **cursor** installs the hub's hook
//! relay ([`super::preseed_cursor`]). Every other kind has none. A preseed NEVER
//! fails a create — [`Engine::create`](super::ops::Engine::create) reports the
//! failure through its warn sink and carries on — which is why this returns the
//! reason as a string for that one-line diagnostic.

use shed_core::rc::RcKind;

use super::ops::GetEnv;
use super::preseed_cursor::preseed_cursor_hooks;
use super::trust::preseed_claude_config;

/// A preseed's diagnostic failure.
///
/// The typed [`PreseedError::CursorForeignDevice`] variant exists because that
/// case is not a failure at all but a deliberate DECLINE with a specific
/// remediation (`ErrCursorHooksForeignDevice`, `preseed_cursor.go:92`) — Go's
/// callers match it with `errors.Is`, and so do this crate's tests.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum PreseedError {
    /// `~/.cursor` is on another filesystem than `$HOME` — an auth mount. The
    /// `hooks.json` half is skipped; the (inert) hub script is still written.
    CursorForeignDevice,
    /// Anything else, carrying Go's message verbatim.
    Failed(String),
}

impl PreseedError {
    /// Build a [`PreseedError::Failed`] from any message.
    pub fn failed(msg: impl Into<String>) -> Self {
        PreseedError::Failed(msg.into())
    }
}

impl std::fmt::Display for PreseedError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            // Verbatim from `preseed_cursor.go:92`.
            PreseedError::CursorForeignDevice => f.write_str(
                "~/.cursor is on a different device than $HOME (an auth mount); skipping the hooks.json preseed",
            ),
            PreseedError::Failed(msg) => f.write_str(msg),
        }
    }
}

impl std::error::Error for PreseedError {}

/// The stack the preseed worker thread gets.
///
/// **Why a dedicated thread at all.** [`super::go_json`] is a recursive-descent
/// parser, capped (like Go) at a nesting depth of 10000. Go can afford that
/// depth because goroutine stacks GROW on demand; Rust's are a fixed allocation,
/// and a full-depth parse overflows every default there is — the 8 MiB main
/// thread as readily as the 2 MiB a cargo-test or async-runtime worker gets. The
/// asymmetry matters because a Rust stack overflow is not a catchable error: it
/// is a SIGABRT that takes the whole `sx` process down, where Go would merely
/// decline the preseed and let the create carry on. A preseed is best-effort by
/// contract, so it must not be able to abort its caller — and the only way to
/// keep that promise over a bounded-but-deep recursion is to give it a stack
/// sized for the bound.
///
/// 64 MiB over 10000 frames is ~6.5 KiB of headroom per frame, which has to
/// cover all three recursive passes the same document drives: the parse, the
/// merge's walk, and the encoder's.
const PRESEED_STACK_BYTES: usize = 64 << 20;

/// Every environment key a preseed reads — `trust` looks at `CLAUDE_CONFIG_DIR`
/// then `HOME`, `preseed_cursor` at `HOME`.
///
/// [`dispatch`] SNAPSHOTS these before handing work to the worker thread:
/// [`GetEnv`] is a bare `&dyn Fn`, so it is not `Send`, and copying two strings
/// is a far smaller change than threading a `Sync` bound through every seam of
/// the engine that carries an env closure.
///
/// **A preseed that reads a new key must add it here.** The debug assertion in
/// [`dispatch`]'s replay closure turns a forgotten one into a loud test failure
/// rather than a silent empty read in production.
const PRESEED_ENV_KEYS: &[&str] = &["CLAUDE_CONFIG_DIR", "HOME"];

/// Run `kind`'s preseed, if it has one (`specForKind(kind).Preseed`).
///
/// This is what [`Engine::with_preseed`](super::ops::Engine::with_preseed) is
/// wired to in production; the string is the reason text create appends to
/// `"<tool> preseed skipped: "`.
///
/// The work happens on a [`PRESEED_STACK_BYTES`] worker thread — see that
/// constant for why. This is the single entry both preseeds flow through, so
/// pinning the stack here covers every production path into either of them.
pub fn dispatch(kind: &RcKind, workdir: &str, env: GetEnv) -> Result<(), String> {
    // Snapshot before crossing the thread boundary; replay from the snapshot on
    // the far side.
    let snapshot: Vec<(&'static str, String)> = PRESEED_ENV_KEYS
        .iter()
        .map(|key| (*key, env(key)))
        .collect();
    let kind = kind.clone();
    let workdir = workdir.to_string();

    let worker = move || {
        let env = |key: &str| {
            debug_assert!(
                PRESEED_ENV_KEYS.contains(&key),
                "{key} is read by a preseed but missing from PRESEED_ENV_KEYS, \
                 so it will silently read as empty off the worker thread"
            );
            snapshot
                .iter()
                .find(|(k, _)| *k == key)
                .map(|(_, v)| v.clone())
                .unwrap_or_default()
        };
        run(&kind, &workdir, &env)
    };

    let handle = std::thread::Builder::new()
        .name("rc-preseed".to_string())
        .stack_size(PRESEED_STACK_BYTES)
        .spawn(worker)
        .map_err(|e| format!("spawning the preseed worker: {e}"))?;

    match handle.join() {
        Ok(outcome) => outcome.map_err(|e| e.to_string()),
        // A panic on the worker must come back as a REASON, not unwind into the
        // create — same best-effort contract as any other preseed failure.
        Err(payload) => Err(format!(
            "the preseed worker panicked: {}",
            panic_message(&*payload)
        )),
    }
}

/// The panic payload's message, for the two shapes `panic!` actually produces.
fn panic_message(payload: &(dyn std::any::Any + Send)) -> String {
    if let Some(s) = payload.downcast_ref::<&str>() {
        return (*s).to_string();
    }
    if let Some(s) = payload.downcast_ref::<String>() {
        return s.clone();
    }
    "unknown panic".to_string()
}

/// The dispatch table itself, run on the worker thread.
fn run(kind: &RcKind, workdir: &str, env: GetEnv) -> Result<(), PreseedError> {
    match kind {
        // Both claude kinds share the claude spec, and so its preseed
        // (`agents.go:419-423`).
        RcKind::ClaudeRc | RcKind::ClaudeBroker => preseed_claude_config(workdir, env),
        // workdir is unused by cursor's (its hooks are global), and is accepted
        // only to satisfy the shared signature.
        RcKind::Cursor => preseed_cursor_hooks(workdir, env),
        // codex (its trust gate is a pane prompt the poller answers), opencode,
        // shell and any unregistered kind: no preseed at all.
        _ => Ok(()),
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::fake::{env_from, home_env};

    #[test]
    fn claude_kinds_seed_the_trust_file() {
        for kind in [RcKind::ClaudeRc, RcKind::ClaudeBroker] {
            let home = tempfile::tempdir().unwrap();
            let env = home_env(home.path().to_str().unwrap());
            dispatch(&kind, "/home/shed/proj", &env).unwrap();
            let text = std::fs::read_to_string(home.path().join(".claude.json")).unwrap();
            assert!(
                text.contains("hasTrustDialogAccepted"),
                "{kind:?} did not seed the trust file: {text}"
            );
        }
    }

    #[test]
    fn cursor_seeds_the_hook_relay() {
        let home = tempfile::tempdir().unwrap();
        let env = home_env(home.path().to_str().unwrap());
        dispatch(&RcKind::Cursor, "/home/shed/proj", &env).unwrap();
        assert!(home.path().join(".cursor/hooks.json").exists());
        assert!(home.path().join(".shed-rc-hub/cursor-hook.sh").exists());
    }

    #[test]
    fn other_kinds_have_no_preseed() {
        let home = tempfile::tempdir().unwrap();
        let env = home_env(home.path().to_str().unwrap());
        for kind in [
            RcKind::Codex,
            RcKind::Opencode,
            RcKind::Shell,
            RcKind::Other("future".to_string()),
        ] {
            dispatch(&kind, "/home/shed/proj", &env).unwrap();
        }
        // Nothing at all was written.
        assert_eq!(std::fs::read_dir(home.path()).unwrap().count(), 0);
    }

    #[test]
    fn a_failure_is_reported_as_text_never_as_a_panic() {
        let env = env_from(&[]); // no HOME at all
        let err = dispatch(&RcKind::ClaudeRc, "/x", &env).unwrap_err();
        assert_eq!(err, "no CLAUDE_CONFIG_DIR or HOME; skipping trust preseed");
        let err = dispatch(&RcKind::Cursor, "/x", &env).unwrap_err();
        assert_eq!(err, "no HOME; skipping cursor hook preseed");
    }

    /// The whole point of [`PRESEED_STACK_BYTES`]: a `~/.claude.json` nested far
    /// past Go's limit must come back as a REASON string, on a live dispatch, and
    /// leave the file untouched.
    ///
    /// Before the depth cap this aborted the test binary outright (SIGABRT on
    /// stack overflow) rather than failing, so a regression here is loud in the
    /// worst way. Note the test body itself does NOT need a big stack — the
    /// recursion happens on the worker `dispatch` spawns.
    #[test]
    fn a_pathologically_deep_claude_json_is_declined_not_fatal() {
        let home = tempfile::tempdir().unwrap();
        let path = home.path().join(".claude.json");
        let depth = 100_000;
        let deep = format!("{{\"a\":{}{}}}", "[".repeat(depth), "]".repeat(depth));
        std::fs::write(&path, &deep).unwrap();

        let env = home_env(home.path().to_str().unwrap());
        let err = dispatch(&RcKind::ClaudeRc, "/home/shed/proj", &env).unwrap_err();
        assert!(
            err.contains("is not valid JSON; leaving untouched:")
                && err.contains("exceeded max depth"),
            "want the depth refusal, got: {err}"
        );
        // Merge — never clobber: the refusal left the bytes exactly as they were.
        assert_eq!(std::fs::read_to_string(&path).unwrap(), deep);
    }

    /// A preseed runs on its own thread now; a panic there must surface as a
    /// reason string like any other failure, never unwind into the create.
    #[test]
    fn the_env_snapshot_covers_every_key_a_preseed_reads() {
        // `dispatch`'s replay closure debug-asserts on an unlisted key, so this
        // cell fails (in debug, which is how tests run) the moment a preseed
        // starts reading something `PRESEED_ENV_KEYS` does not carry.
        let home = tempfile::tempdir().unwrap();
        let env = env_from(&[
            ("HOME", home.path().to_str().unwrap()),
            ("CLAUDE_CONFIG_DIR", ""),
        ]);
        for kind in [RcKind::ClaudeRc, RcKind::ClaudeBroker, RcKind::Cursor] {
            dispatch(&kind, "/home/shed/proj", &env).unwrap();
        }
    }

    #[test]
    fn foreign_device_message_is_gos_verbatim() {
        assert_eq!(
            PreseedError::CursorForeignDevice.to_string(),
            "~/.cursor is on a different device than $HOME (an auth mount); skipping the hooks.json preseed"
        );
    }
}
