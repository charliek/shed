//! claude's create-time preseed — a port of `internal/ext/rc/trust.go`.
//!
//! Marks the create's workdir trusted and clears the first-run onboarding gates in
//! `${CLAUDE_CONFIG_DIR:-$HOME}/.claude.json`, so an unattended session reaches
//! `ready` instead of blocking on a modal. It does NOT log in.
//!
//! The three properties that make this more than a JSON edit, all of them
//! cross-implementation contract on a mixed Go/Rust fleet:
//!
//! 1. **Merge — never clobber.** Unknown keys (OAuth state, MCP servers, other
//!    projects) round-trip verbatim; a malformed file, or one with trailing
//!    content, is left EXACTLY as it is and reported.
//! 2. **The literals are the interop.** The sibling lock file is
//!    `<target>.shed-ext-rc.lock` — a Rust engine that spelled it differently
//!    would not exclude a concurrent Go create at all — and the temp pattern is
//!    `.claude.json.*.tmp`, so leftovers are attributable.
//! 3. **The bytes are the wire.** The rewrite goes through [`super::go_json`], not
//!    `serde_json`: this file is a raw-bytes parity surface (plan 009 §3.5).
//!
//! Best-effort by construction: `create` reports a failure through its warn sink
//! and carries on (the `--wait` poller's send-Enter fallback covers the trust
//! dialog anyway).
//!
//! This file also HOSTS the machinery both preseeds merge through —
//! [`lock_sibling`], [`read_json_object`], [`atomic_write`] — because `trust.go`
//! does (`preseed_cursor.go` calls them across the file boundary the same way
//! [`super::preseed_cursor`] does here). Keeping the split where Go put it is
//! what lets every doc comment cite a `trust.go:NNN` line that still matches.

use std::ffi::OsString;
use std::fs;
use std::io::Write;
use std::os::unix::ffi::{OsStrExt, OsStringExt};
use std::os::unix::fs::{DirBuilderExt, OpenOptionsExt, PermissionsExt};
use std::os::unix::io::{AsRawFd, IntoRawFd};
use std::path::{Path, PathBuf};

use super::go_json::{marshal_indent, parse_document, GoObject, GoValue};
use super::ops::GetEnv;
use super::preseed::PreseedError;

/// The sibling-lock suffix. **A cross-implementation literal** (`trust.go:122`):
/// mutual exclusion between a Go `shed-machine-rc` and a Rust `sx` merging the
/// same file depends on both opening the same lock path.
pub const LOCK_SUFFIX: &str = ".shed-ext-rc.lock";

/// `os.CreateTemp` pattern for the claude config rewrite (`trust.go:112`) — the
/// `*` is where the random component goes. Per-preseed so debris in a shared
/// directory is attributable to the writer that left it.
pub const CLAUDE_TMP_PATTERN: &str = ".claude.json.*.tmp";

/// Mode of a freshly created preseed target (`readJSONObject`, `trust.go:149`).
/// An EXISTING file's bits are carried through instead.
const DEFAULT_MODE: u32 = 0o600;

/// A "seen count" high enough that claude stops showing the fullscreen-renderer
/// upsell (`fullscreenUpsellFloor`, `trust.go:31`).
const FULLSCREEN_UPSELL_FLOOR: i64 = 999;

/// `${CLAUDE_CONFIG_DIR:-$HOME}/.claude.json` (`claudeConfigPath`,
/// `trust.go:18`). An EMPTY `CLAUDE_CONFIG_DIR` is treated as unset (matching
/// claude itself); neither set returns `None`, and the caller treats the preseed
/// as a no-op.
pub fn claude_config_path(env: GetEnv) -> Option<PathBuf> {
    let mut dir = env("CLAUDE_CONFIG_DIR");
    if dir.is_empty() {
        dir = env("HOME");
    }
    if dir.is_empty() {
        return None;
    }
    Some(Path::new(&dir).join(".claude.json"))
}

/// Prepare claude's config so a fresh session reaches `ready` unattended
/// (`PreseedClaudeConfig`, `trust.go:53`).
///
/// The merge, in Go's order — each step never clobbers what a user chose:
///
/// | key | rule |
/// |---|---|
/// | `projects[<workdir>].hasTrustDialogAccepted` | forced `true` |
/// | `hasCompletedOnboarding` | forced `true` |
/// | `theme` | `"dark"` **only when absent** |
/// | `fullscreenUpsellSeenCount` | raised to 999 **only when lower** |
/// | `hasSeenAutoModeEntryWarning` | `true` **only when absent** |
pub fn preseed_claude_config(workdir: &str, env: GetEnv) -> Result<(), PreseedError> {
    let Some(path) = claude_config_path(env) else {
        return Err(PreseedError::failed(
            "no CLAUDE_CONFIG_DIR or HOME; skipping trust preseed",
        ));
    };

    // The sh reference does `mkdir -p`: a CLAUDE_CONFIG_DIR pointing at a
    // not-yet-created dir would otherwise no-op (`trust.go:61`).
    if let Some(parent) = path.parent() {
        fs::DirBuilder::new()
            .recursive(true)
            .mode(0o700)
            .create(parent)
            .map_err(|e| PreseedError::failed(format!("creating config dir: {e}")))?;
    }

    let _lock = lock_sibling(&path)
        .map_err(|e| PreseedError::failed(format!("locking trust file: {e}")))?;

    let (mut config, mode) = read_json_object(&path)?;

    // projects[workdir].hasTrustDialogAccepted — everything else in the document,
    // and everything else under `projects`, is preserved (`trust.go:78`).
    let mut projects = take_object(&mut config, "projects");
    let mut project = take_object(&mut projects, workdir);
    project.insert("hasTrustDialogAccepted".to_string(), GoValue::Bool(true));
    projects.insert(workdir.to_string(), GoValue::Object(project));
    config.insert("projects".to_string(), GoValue::Object(projects));

    config.insert("hasCompletedOnboarding".to_string(), GoValue::Bool(true));
    config
        .entry("theme".to_string())
        .or_insert_with(|| GoValue::String("dark".to_string()));

    // A "seen count": raise it past the threshold, never lower an existing value
    // (`trust.go:101`). The comparison reads the RAW literal through
    // `number_int`, so a float or an oversized integer compares as 0 exactly as
    // Go's `json.Number.Int64()` failure does.
    let seen = config
        .get("fullscreenUpsellSeenCount")
        .map_or(0, GoValue::number_int);
    if seen < FULLSCREEN_UPSELL_FLOOR {
        config.insert(
            "fullscreenUpsellSeenCount".to_string(),
            GoValue::Number(FULLSCREEN_UPSELL_FLOOR.to_string()),
        );
    }
    config
        .entry("hasSeenAutoModeEntryWarning".to_string())
        .or_insert(GoValue::Bool(true));

    atomic_write(
        &path,
        CLAUDE_TMP_PATTERN,
        marshal_indent(&config).as_bytes(),
        mode,
    )
}

/// Remove `key`'s value from `obj` and return it as an object, or an empty one
/// when it is absent OR the wrong shape.
///
/// The failed-assertion-discards behavior is Go's here and is DELIBERATE for the
/// claude preseed (`projects, _ := config["projects"].(map[string]any)`,
/// `trust.go:78`): a `projects` that is not an object is replaced. (The cursor
/// preseed refuses instead — see `preseed_cursor::merge_cursor_hooks_config` —
/// because there the surrounding shape is user-authored config.)
fn take_object(obj: &mut GoObject, key: &str) -> GoObject {
    match obj.remove(key) {
        Some(GoValue::Object(inner)) => inner,
        _ => GoObject::new(),
    }
}

/// An exclusive `flock` on a SIBLING lock file (`lockSibling`, `trust.go:121`),
/// released when the guard drops.
///
/// Never on the target file itself: each holder must read the CURRENT file after
/// acquiring the lock, and an atomic rename by another holder would leave a
/// waiter writing through a stale, unlinked inode.
pub(crate) fn lock_sibling(path: &Path) -> std::io::Result<LockGuard> {
    // Concatenated as RAW BYTES, the way Go's `path + lockSuffix` does — NOT
    // through `path.display()`, which is lossy: a non-UTF-8 `$HOME` (legal on
    // every unix) would have its bad bytes replaced by U+FFFD and yield a
    // DIFFERENT lock path than the Go engine computes for the same file. Two
    // implementations locking two different files exclude nothing, and the
    // cross-implementation exclusion this whole function exists for would be
    // silently off.
    let mut raw = path.as_os_str().as_bytes().to_vec();
    raw.extend_from_slice(LOCK_SUFFIX.as_bytes());
    let lock_path = PathBuf::from(OsString::from_vec(raw));
    let file = fs::OpenOptions::new()
        .read(true)
        .write(true)
        .create(true)
        .truncate(false)
        .mode(0o600)
        .open(&lock_path)?;
    // SAFETY: `file` owns a valid fd for the duration of the call.
    let rc = unsafe { libc::flock(file.as_raw_fd(), libc::LOCK_EX) };
    if rc != 0 {
        return Err(std::io::Error::last_os_error());
    }
    Ok(LockGuard { file })
}

/// The held sibling lock. Dropping it unlocks and closes, like Go's returned
/// `unlock` closure.
pub(crate) struct LockGuard {
    file: fs::File,
}

impl Drop for LockGuard {
    fn drop(&mut self) {
        // SAFETY: the fd is still owned by `self.file` until this returns.
        unsafe { libc::flock(self.file.as_raw_fd(), libc::LOCK_UN) };
    }
}

/// Read a merge-never-clobber preseed target and return its top-level object plus
/// the file mode a rewrite must preserve (`readJSONObject`, `trust.go:147`).
/// Shared by both preseeds so the tolerance rules cannot drift:
///
/// * an absent or empty file yields an EMPTY object at mode 0600;
/// * an existing file's permission bits are carried through;
/// * numbers keep their raw literal (Go's `UseNumber`), so nothing round-trips
///   through `f64`;
/// * a literal `null` is re-seeded as an empty object;
/// * anything else malformed — including trailing content after the top-level
///   value — is an ERROR, and the caller leaves the file exactly as it is.
pub(crate) fn read_json_object(path: &Path) -> Result<(GoObject, u32), PreseedError> {
    let data = match fs::read(path) {
        Ok(data) => data,
        Err(err) if err.kind() == std::io::ErrorKind::NotFound => {
            return Ok((GoObject::new(), DEFAULT_MODE))
        }
        Err(err) => {
            return Err(PreseedError::failed(format!(
                "reading {}: {err}",
                path.display()
            )))
        }
    };
    let mode = fs::metadata(path)
        .map(|m| m.permissions().mode() & 0o777)
        .unwrap_or(DEFAULT_MODE);
    if data.is_empty() {
        return Ok((GoObject::new(), mode));
    }
    match parse_document(&data) {
        // `null` decodes the map to nil in Go; re-seed rather than panic.
        Ok(None) => Ok((GoObject::new(), mode)),
        Ok(Some(obj)) => Ok((obj, mode)),
        Err(err) => Err(PreseedError::failed(format!(
            "existing {} is not valid JSON; leaving untouched: {err}",
            path.display()
        ))),
    }
}

/// Write `data` to a temp file in `path`'s directory, fsync it, chmod it to
/// `mode`, and rename it over `path` (`atomicWrite`, `trust.go:193`).
///
/// `tmp_pattern` is `os.CreateTemp`'s pattern — passed in rather than hardcoded
/// so a directory holding two different preseeded files never sees one writer's
/// debris attributed to the other.
pub(crate) fn atomic_write(
    path: &Path,
    tmp_pattern: &str,
    data: &[u8],
    mode: u32,
) -> Result<(), PreseedError> {
    let dir = path.parent().unwrap_or(Path::new("."));
    let (tmp_path, mut tmp) = create_temp(dir, tmp_pattern)
        .map_err(|e| PreseedError::failed(format!("creating temp config: {e}")))?;
    // Best-effort cleanup on every failure path below (a no-op once renamed).
    let cleanup = |err: PreseedError| {
        let _ = fs::remove_file(&tmp_path);
        err
    };
    tmp.write_all(data)
        .map_err(|e| cleanup(PreseedError::failed(format!("writing temp config: {e}"))))?;
    tmp.sync_all()
        .map_err(|e| cleanup(PreseedError::failed(format!("syncing temp config: {e}"))))?;
    // Go CHECKS `tmp.Close()` and wraps it (`closing temp config: %w`,
    // `trust.go:210`); Rust's `File` has no fallible close, and a bare `drop`
    // would swallow the errno. `sync_all` above already forced the data and
    // metadata out, so the only thing left for close(2) to report is a deferred
    // writeback error the kernel held back (EIO on a failing disk, ENOSPC or a
    // stale handle on NFS) — precisely the class Go surfaces here, so it gets
    // Go's message rather than being discarded.
    close_checked(tmp)
        .map_err(|e| cleanup(PreseedError::failed(format!("closing temp config: {e}"))))?;
    fs::set_permissions(&tmp_path, fs::Permissions::from_mode(mode))
        .map_err(|e| cleanup(PreseedError::failed(format!("chmod temp config: {e}"))))?;
    fs::rename(&tmp_path, path)
        .map_err(|e| cleanup(PreseedError::failed(format!("renaming temp config: {e}"))))?;
    Ok(())
}

/// Close `file`, reporting close(2)'s errno instead of dropping it on the floor.
///
/// Like Go's `(*os.File).Close`, an `EINTR` is reported rather than retried: on
/// Linux and the BSDs the descriptor is closed regardless, so a retry would be
/// racing to close someone else's fd.
fn close_checked(file: fs::File) -> std::io::Result<()> {
    let fd = file.into_raw_fd();
    // SAFETY: `fd` was just taken from an owned `File`, so it is valid, and
    // `into_raw_fd` gave up the ownership that would otherwise double-close it.
    if unsafe { libc::close(fd) } != 0 {
        return Err(std::io::Error::last_os_error());
    }
    Ok(())
}

/// `os.CreateTemp(dir, pattern)`: create a new file whose name is `pattern` with
/// its LAST `*` replaced by a random component (a pattern without one appends
/// instead), opened `O_CREATE|O_EXCL` at 0600, retrying on collision.
fn create_temp(dir: &Path, pattern: &str) -> std::io::Result<(PathBuf, fs::File)> {
    let (prefix, suffix) = match pattern.rsplit_once('*') {
        Some((p, s)) => (p, s),
        None => (pattern, ""),
    };
    let mut last_err = None;
    for _ in 0..10_000 {
        // uuid is already a dependency (the session id); its simple form is the
        // same hex-digit shape Go's random suffix has.
        let candidate = dir.join(format!(
            "{prefix}{}{suffix}",
            uuid::Uuid::new_v4().as_simple()
        ));
        match fs::OpenOptions::new()
            .write(true)
            .create_new(true)
            .mode(0o600)
            .open(&candidate)
        {
            Ok(file) => return Ok((candidate, file)),
            Err(err) if err.kind() == std::io::ErrorKind::AlreadyExists => last_err = Some(err),
            Err(err) => return Err(err),
        }
    }
    Err(last_err.unwrap_or_else(|| {
        std::io::Error::new(std::io::ErrorKind::AlreadyExists, "no temp name available")
    }))
}

#[cfg(test)]
mod tests;
