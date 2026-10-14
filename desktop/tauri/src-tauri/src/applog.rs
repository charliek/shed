//! **The app's log file** (#414): one rotating sink for everything this process
//! says — its own lines, the shed crates' `tracing` events (roost-ipc's only
//! with `SHED_TAURI_LOG=debug`, see [`filter`]), and the embedded broker's
//! `BusLog` — so an app launched from Finder, the Dock or a desktop launcher
//! (whose stderr goes nowhere) still leaves a record.
//!
//! * **Where:** macOS `~/Library/Logs/ShedDesktop/shed-desktop.log` (the path the
//!   retired Swift app wrote); Linux
//!   `${XDG_STATE_HOME:-$HOME/.local/state}/ai.stridelabs.shed-desktop/shed-desktop.log`.
//!   `SHED_TAURI_LOG_FILE` overrides both ([`resolve_path`], read by `env.rs`).
//! * **When:** the sink opens only once the single-instance lock is held
//!   (`lib.rs`), so a second launch that just raises the running app never
//!   touches the file. Lines said before that wait in a bounded pre-sink
//!   buffer ([`PRE_SINK_LINES`]) and are written, in order, when it opens. A
//!   process that could not take the lock (the check itself failed) never
//!   opens it: another instance may be appending to and rotating the same
//!   file under a mutex of its own ([`init`]).
//! * **How:** one mutex, one synchronous append per line, flushed per line —
//!   the second-signal path exits the process right after its line. Each line
//!   is `<RFC 3339 UTC> <LEVEL> <message>`. At open, and whenever a write takes
//!   the file to [`MAX_BYTES`], it rotates: `.log` → `.log.1` → `.log.2` →
//!   `.log.3`, the oldest dropped ([`KEEP`]).
//! * **Never fatal:** a failed open, write or rotation turns file logging off —
//!   stderr only from then on — and says so once, on stderr. The guarantee is
//!   "never fatal", not "never blocks": a line is a local-disk write, like the
//!   `eprintln!` it replaced. A path naming anything but a regular file — a
//!   symlink (never followed), a FIFO (never waited on), a device — is refused
//!   the same way ([`open_append`]).
//! * **Permissions:** directories the app creates are `0700` (an existing parent
//!   such as `/tmp` is never touched); the file is `0600`, and a rotated file
//!   keeps its mode. Rotation moves only regular files, and the live one only
//!   while its name still names the file this sink has open ([`rotate`]).
//!
//! The app's own lines go through [`app_info!`], [`app_warn!`] and
//! [`app_error!`], which write the message to stderr exactly as the
//! `eprintln!` they replaced did (the harness captures it) **and** to the sink.

use std::collections::VecDeque;
use std::fs::{DirBuilder, File, Metadata, OpenOptions, Permissions};
use std::io::{self, Write};
use std::os::unix::fs::{DirBuilderExt, FileTypeExt, MetadataExt, OpenOptionsExt, PermissionsExt};
use std::path::{Path, PathBuf};
use std::sync::{Mutex, MutexGuard};
use std::time::SystemTime;

use shed_app::timefmt::format_rfc3339_millis;
use tracing::Level;
use tracing_subscriber::filter::{LevelFilter, Targets};
use tracing_subscriber::fmt::format::Writer;
use tracing_subscriber::fmt::time::FormatTime;

/// The size at which the file rotates.
const MAX_BYTES: u64 = 5 * 1024 * 1024;
/// How many rotated files are kept beside the live one (`.1` … `.3`).
const KEEP: usize = 3;
/// How many lines said before the sink opens are held for it.
const PRE_SINK_LINES: usize = 256;
/// The file's name on every platform.
const FILE_NAME: &str = "shed-desktop.log";
/// The Linux directory under `$XDG_STATE_HOME` — the identifier the app's
/// config and data directories already use.
const LINUX_DIR: &str = "ai.stridelabs.shed-desktop";
/// Which default [`resolve_path`] takes on this build.
pub const MAC: bool = cfg!(target_os = "macos");

/// An INFO line to stderr and the log file — `format!` arguments, the text the
/// `eprintln!` it replaced printed.
macro_rules! app_info {
    ($($arg:tt)*) => {
        $crate::applog::emit(::tracing::Level::INFO, format_args!($($arg)*))
    };
}

/// A WARN line to stderr and the log file (see [`app_info!`]).
macro_rules! app_warn {
    ($($arg:tt)*) => {
        $crate::applog::emit(::tracing::Level::WARN, format_args!($($arg)*))
    };
}

/// An ERROR line to stderr and the log file (see [`app_info!`]).
macro_rules! app_error {
    ($($arg:tt)*) => {
        $crate::applog::emit(::tracing::Level::ERROR, format_args!($($arg)*))
    };
}

/// The log file's path: `override_path` (`SHED_TAURI_LOG_FILE`) when set, else
/// the platform default under `home` — on Linux under `xdg_state_home` when it
/// is set (and absolute, per the XDG spec), else `~/.local/state`. `None` when
/// nothing resolves one (no override and no absolute `HOME`): file logging is
/// then off. Pure, so both platforms' branches are tested on either.
pub fn resolve_path(
    mac: bool,
    override_path: Option<&str>,
    xdg_state_home: Option<&str>,
    home: Option<&str>,
) -> Option<PathBuf> {
    if let Some(p) = override_path.filter(|p| !p.is_empty()) {
        return Some(PathBuf::from(p));
    }
    fn absolute(v: Option<&str>) -> Option<&Path> {
        v.map(Path::new).filter(|p| p.is_absolute())
    }
    let home = absolute(home);
    if mac {
        return home.map(|h| h.join("Library/Logs/ShedDesktop").join(FILE_NAME));
    }
    let state = absolute(xdg_state_home)
        .map(Path::to_path_buf)
        .or_else(|| home.map(|h| h.join(".local/state")))?;
    Some(state.join(LINUX_DIR).join(FILE_NAME))
}

// ---------------------------------------------------------------------------
// The sink
// ---------------------------------------------------------------------------

struct OpenFile {
    path: PathBuf,
    file: File,
    len: u64,
}

enum State {
    /// Before [`Sink::open`]: lines are held, the oldest dropped past the cap.
    Pending {
        lines: VecDeque<String>,
        dropped: usize,
    },
    Open(OpenFile),
    /// No file: never opened one, or a write/rotation failed. Stderr only.
    Off,
}

/// The rotating file sink. The app has one ([`SINK`]); tests build their own.
pub struct Sink {
    state: Mutex<State>,
    cap: u64,
    keep: usize,
    pending_cap: usize,
}

impl Sink {
    pub const fn new(cap: u64, keep: usize, pending_cap: usize) -> Self {
        Sink {
            state: Mutex::new(State::Pending {
                lines: VecDeque::new(),
                dropped: 0,
            }),
            cap,
            keep,
            pending_cap,
        }
    }

    fn lock(&self) -> MutexGuard<'_, State> {
        // A panic mid-write must not take logging down with it.
        self.state.lock().unwrap_or_else(|e| e.into_inner())
    }

    /// Open the file at `path` (`None`: there is none — logging stays on
    /// stderr) and write the pre-sink lines into it, in order. Once only; a
    /// failure turns file logging off and hands back the line that says so.
    pub fn open(&self, path: Option<&Path>) -> Result<(), String> {
        let mut state = self.lock();
        // Off until the file is open and the held lines are in it.
        let (mut lines, dropped) = match std::mem::replace(&mut *state, State::Off) {
            State::Pending { lines, dropped } => (lines, dropped),
            opened_or_off => {
                *state = opened_or_off;
                return Ok(());
            }
        };
        let Some(path) = path else {
            return Err(
                "shed-desktop-tauri: no log file (neither SHED_TAURI_LOG_FILE nor an \
                 absolute HOME is set); logging to stderr only"
                    .to_string(),
            );
        };
        if dropped > 0 {
            lines.push_front(stamp(
                Level::WARN,
                &format!(
                    "shed-desktop-tauri: {dropped} earlier line(s) were dropped before the \
                     log file opened"
                ),
            ));
        }
        let file = open_file(path, self.cap, self.keep)
            .and_then(|mut file| {
                for line in &lines {
                    self.write(&mut file, line)?;
                }
                Ok(file)
            })
            .map_err(|e| off_notice(path, &e))?;
        *state = State::Open(file);
        Ok(())
    }

    /// Never open a file: the held lines are dropped (each was on stderr
    /// already) and file logging is off from here on, `notice` handed back to
    /// say why. Once only, like [`Sink::open`]: a no-op on a sink already open
    /// or off.
    pub fn disable(&self, notice: &str) -> Result<(), String> {
        let mut state = self.lock();
        if !matches!(*state, State::Pending { .. }) {
            return Ok(());
        }
        *state = State::Off;
        Err(notice.to_string())
    }

    /// The file being written, or `None` when file logging is off (or not yet
    /// open).
    pub fn active_path(&self) -> Option<PathBuf> {
        match &*self.lock() {
            State::Open(f) => Some(f.path.clone()),
            _ => None,
        }
    }

    /// Stamp and record one line. Answers the notice to print when this write
    /// turned file logging off — once, ever.
    pub fn line(&self, level: Level, msg: &str) -> Option<String> {
        self.raw(&stamp(level, msg))
    }

    /// Record a line that is already stamped (a `tracing` event, formatted by
    /// the `fmt` layer with [`Rfc3339`]).
    fn raw(&self, line: &str) -> Option<String> {
        let mut state = self.lock();
        match &mut *state {
            State::Pending { lines, dropped } => {
                if lines.len() >= self.pending_cap {
                    lines.pop_front();
                    *dropped += 1;
                }
                lines.push_back(line.to_string());
                None
            }
            State::Open(file) => match self.write(file, line) {
                Ok(()) => None,
                Err(e) => {
                    let notice = off_notice(&file.path, &e);
                    *state = State::Off;
                    Some(notice)
                }
            },
            State::Off => None,
        }
    }

    /// One whole line in one append, then a rotation if it took the file to
    /// the cap.
    fn write(&self, file: &mut OpenFile, line: &str) -> io::Result<()> {
        let line = format!("{line}\n");
        file.file.write_all(line.as_bytes())?;
        file.file.flush()?;
        file.len += line.len() as u64;
        file.rotate_if_full(self.cap, self.keep)
    }
}

impl OpenFile {
    /// At `cap` or past it: rotate, and carry on in a fresh file.
    fn rotate_if_full(&mut self, cap: u64, keep: usize) -> io::Result<()> {
        if self.len >= cap {
            rotate(&self.path, keep, &self.file)?;
            (self.file, self.len) = open_append(&self.path)?;
        }
        Ok(())
    }
}

fn off_notice(path: &Path, e: &io::Error) -> String {
    format!(
        "shed-desktop-tauri: log file {} unavailable ({e}); logging to stderr only",
        path.display()
    )
}

fn stamp(level: Level, msg: &str) -> String {
    format!(
        "{} {:>5} {msg}",
        format_rfc3339_millis(SystemTime::now()),
        level.as_str()
    )
}

/// Open (creating its missing directories) and rotate first if it is already
/// at the cap.
fn open_file(path: &Path, cap: u64, keep: usize) -> io::Result<OpenFile> {
    if let Some(dir) = path.parent().filter(|d| !d.as_os_str().is_empty()) {
        create_private_dirs(dir)?;
    }
    let (file, len) = open_append(path)?;
    let mut file = OpenFile {
        path: path.to_path_buf(),
        file,
        len,
    };
    file.rotate_if_full(cap, keep)?;
    Ok(file)
}

/// Create `dir` and whichever of its ancestors are missing, each `0700` — and
/// only those: an existing ancestor keeps its mode.
fn create_private_dirs(dir: &Path) -> io::Result<()> {
    let mut missing = Vec::new();
    let mut at = dir;
    while !at.exists() {
        missing.push(at.to_path_buf());
        match at.parent() {
            Some(p) if !p.as_os_str().is_empty() => at = p,
            _ => break,
        }
    }
    for d in missing.iter().rev() {
        match DirBuilder::new().mode(0o700).create(d) {
            // Explicitly, so the umask cannot narrow or the mode drift.
            Ok(()) => std::fs::set_permissions(d, Permissions::from_mode(0o700))?,
            Err(e) if e.kind() == io::ErrorKind::AlreadyExists => {}
            Err(e) => return Err(e),
        }
    }
    Ok(())
}

/// Append-only, created `0600`, with its current length — and only ever a
/// regular file. `O_NOFOLLOW` refuses a symlink at the path rather than
/// writing (and chmodding) whatever it points at; `O_NONBLOCK` makes a FIFO
/// fail or open at once instead of waiting for a reader (it changes nothing
/// for a regular file); the handle's own `fstat` then refuses anything that
/// is not a regular file. One found with a wider mode (one the retired Swift
/// app left on macOS) is narrowed to `0600`.
fn open_append(path: &Path) -> io::Result<(File, u64)> {
    let file = OpenOptions::new()
        .create(true)
        .append(true)
        .mode(0o600)
        .custom_flags(libc::O_NOFOLLOW | libc::O_NONBLOCK)
        .open(path)
        .map_err(|e| match std::fs::symlink_metadata(path) {
            // Only to say why: the open itself is what refused it.
            Ok(meta) if !meta.is_file() => not_regular(&meta),
            _ => e,
        })?;
    let meta = file.metadata()?;
    if !meta.is_file() {
        return Err(not_regular(&meta));
    }
    if meta.permissions().mode() & 0o777 != 0o600 {
        file.set_permissions(Permissions::from_mode(0o600))?;
    }
    Ok((file, meta.len()))
}

/// The refusal of a path that names something other than a regular file.
fn not_regular(meta: &Metadata) -> io::Error {
    let t = meta.file_type();
    let what = if t.is_symlink() {
        "a symbolic link"
    } else if t.is_dir() {
        "a directory"
    } else if t.is_fifo() {
        "a FIFO"
    } else if t.is_socket() {
        "a socket"
    } else if t.is_char_device() || t.is_block_device() {
        "a device"
    } else {
        "an unknown kind of file"
    };
    io::Error::other(format!("{what}, not a regular file"))
}

fn numbered(path: &Path, n: usize) -> PathBuf {
    let mut s = path.as_os_str().to_os_string();
    s.push(format!(".{n}"));
    PathBuf::from(s)
}

/// `.log` → `.log.1` → … → `.log.<keep>`, the oldest overwritten (dropped).
/// A rename keeps the file's mode. Only regular files move: anything else at a
/// numbered name stays put (a symlink there is replaced by the rename onto it —
/// a rename never follows one), and the live name moves only while it still
/// names `live`, the file this sink has open; if something else took its place
/// the reopen after this decides about that.
///
/// It assumes it is the directory's only rotator: each check and its rename
/// are two steps, so another process moving these names in between (an
/// external rotator, a second app writing the same path) can have a file moved
/// that the check did not see. The app writes the file only while it holds the
/// single-instance lock, so nothing of its own races here.
fn rotate(path: &Path, keep: usize, live: &File) -> io::Result<()> {
    let rename = |from: &Path, to: &Path| match std::fs::rename(from, to) {
        Err(e) if e.kind() == io::ErrorKind::NotFound => Ok(()),
        other => other,
    };
    for n in (1..keep).rev() {
        let from = numbered(path, n);
        if regular_at(&from)?.is_some() {
            rename(&from, &numbered(path, n + 1))?;
        }
    }
    let open = live.metadata()?;
    match regular_at(path)? {
        Some(at) if (at.dev(), at.ino()) == (open.dev(), open.ino()) => {
            rename(path, &numbered(path, 1))
        }
        _ => Ok(()),
    }
}

/// What `path` itself names (never followed), when that is a regular file.
fn regular_at(path: &Path) -> io::Result<Option<Metadata>> {
    match std::fs::symlink_metadata(path) {
        Ok(meta) if meta.is_file() => Ok(Some(meta)),
        Ok(_) => Ok(None),
        Err(e) if e.kind() == io::ErrorKind::NotFound => Ok(None),
        Err(e) => Err(e),
    }
}

// ---------------------------------------------------------------------------
// The app's sink, and the doors to it
// ---------------------------------------------------------------------------

static SINK: Sink = Sink::new(MAX_BYTES, KEEP, PRE_SINK_LINES);

/// Open the app's log file and install the `tracing` subscriber. Called once,
/// right after the single-instance check; `instance_locked` says whether this
/// process holds the lock ([`start`]).
pub fn init(path: Option<&Path>, instance_locked: bool, debug: bool) {
    if let Err(notice) = start(&SINK, path, instance_locked) {
        eprintln!("{notice}");
    }
    // Only fails if one is already installed — never, in the app.
    let _ = tracing::subscriber::set_global_default(subscriber(&SINK, debug));
}

/// [`init`]'s decision, on any sink: the file opens only when this process
/// holds the single-instance lock. Without it (the check itself failed and the
/// app carried on) another instance may be appending to and rotating the same
/// file under a mutex of its own, so the file stays shut: stderr only, the
/// held lines dropped, `identify.log_path` null, and one notice saying why.
fn start(sink: &Sink, path: Option<&Path>, instance_locked: bool) -> Result<(), String> {
    if instance_locked {
        sink.open(path)
    } else {
        sink.disable(
            "shed-desktop-tauri: no log file: the single-instance lock was not acquired, so \
             another instance may be writing it; logging to stderr only",
        )
    }
}

/// The macros' body: the message to stderr exactly as given, then to the file.
pub fn emit(level: Level, args: std::fmt::Arguments<'_>) {
    let msg = std::fmt::format(args);
    eprintln!("{msg}");
    if let Some(notice) = SINK.line(level, &msg) {
        eprintln!("{notice}");
    }
}

/// The file the app is writing, or `None` when file logging is off —
/// `identify.log_path`.
pub fn active_path() -> Option<PathBuf> {
    SINK.active_path()
}

// ---------------------------------------------------------------------------
// tracing
// ---------------------------------------------------------------------------

/// The level filter: INFO and up for every target but roost-ipc, which is off;
/// with `debug` (`SHED_TAURI_LOG=debug`), DEBUG for the shed crates (`shed_*` —
/// a target matches a directive it starts with) and roost-ipc, and still INFO
/// for everything else (tauri, hyper, …).
///
/// roost-ipc is off by default because its events carry ssh's own diagnostics
/// verbatim — a tunnel failure's `failure` field ends with the last line ssh
/// printed to stderr, bootstrap warnings carry stderr-derived detail — and
/// what ssh prints (a `ProxyCommand`'s connection URL, say) is not this app's
/// to keep in a file. `SHED_TAURI_LOG=debug` is the explicit opt-in to them, on
/// stderr and in the file alike. The shed crates' own events are kept at INFO.
fn filter(debug: bool) -> Targets {
    let base = Targets::new().with_default(LevelFilter::INFO);
    if debug {
        base.with_target("shed_", LevelFilter::DEBUG)
            .with_target("roost_ipc", LevelFilter::DEBUG)
    } else {
        base.with_target("roost_ipc", LevelFilter::OFF)
    }
}

/// The subscriber: a `fmt` layer (no ANSI, the sink's timestamp) writing each
/// event as one line to stderr and into `sink`, under [`filter`]. A factory so
/// a test installs the same thing with `tracing::subscriber::with_default`
/// against a sink of its own.
fn subscriber(
    sink: &'static Sink,
    debug: bool,
) -> impl tracing::Subscriber + Send + Sync + 'static {
    use tracing_subscriber::layer::SubscriberExt;
    tracing_subscriber::registry().with(filter(debug)).with(
        tracing_subscriber::fmt::layer()
            .with_ansi(false)
            .with_timer(Rfc3339)
            .with_writer(move || EventWriter {
                sink,
                buf: Vec::new(),
            }),
    )
}

/// The `fmt` layer's timestamp: the same RFC 3339 UTC the app's lines carry.
struct Rfc3339;

impl FormatTime for Rfc3339 {
    fn format_time(&self, w: &mut Writer<'_>) -> std::fmt::Result {
        w.write_str(&format_rfc3339_millis(SystemTime::now()))
    }
}

/// Collects one formatted event and, when dropped, records it as one line on
/// stderr and in the sink.
struct EventWriter {
    sink: &'static Sink,
    buf: Vec<u8>,
}

impl Write for EventWriter {
    fn write(&mut self, bytes: &[u8]) -> io::Result<usize> {
        self.buf.extend_from_slice(bytes);
        Ok(bytes.len())
    }

    fn flush(&mut self) -> io::Result<()> {
        Ok(())
    }
}

impl Drop for EventWriter {
    fn drop(&mut self) {
        let text = String::from_utf8_lossy(&self.buf);
        let text = text.trim_end_matches('\n');
        if text.is_empty() {
            return;
        }
        eprintln!("{text}");
        if let Some(notice) = self.sink.raw(text) {
            eprintln!("{notice}");
        }
    }
}

// ---------------------------------------------------------------------------
// The embedded broker's log
// ---------------------------------------------------------------------------

/// The embedded broker's `BusLog` (its bus connections, rejections, handler
/// warnings): every line a `tracing` event with the `shed_broker` target, so it
/// reaches stderr and the app's file through [`subscriber`] — and the bus's
/// quiet tier (`debug` — a server still down, retrying) is dropped by
/// [`filter`], as the daemon drops it, unless `SHED_TAURI_LOG=debug`.
pub struct BrokerLog;

impl shed_app::BusLog for BrokerLog {
    fn info(&self, msg: &str) {
        tracing::info!(target: "shed_broker", "{msg}");
    }
    fn warn(&self, msg: &str) {
        tracing::warn!(target: "shed_broker", "{msg}");
    }
    fn debug(&self, msg: &str) {
        tracing::debug!(target: "shed_broker", "{msg}");
    }
    fn error(&self, msg: &str) {
        tracing::error!(target: "shed_broker", "{msg}");
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn leak(sink: Sink) -> &'static Sink {
        Box::leak(Box::new(sink))
    }

    fn mode(path: &Path) -> u32 {
        std::fs::metadata(path).unwrap().permissions().mode() & 0o777
    }

    fn lines(path: &Path) -> Vec<String> {
        std::fs::read_to_string(path)
            .unwrap_or_default()
            .lines()
            .map(str::to_string)
            .collect()
    }

    /// The message part of a stamped line (after `<ts> <LEVEL> `).
    fn message(line: &str) -> &str {
        let (_ts, rest) = line.split_once(' ').unwrap();
        rest.trim_start().split_once(' ').unwrap().1
    }

    #[test]
    fn the_path_is_the_override_or_the_platform_default() {
        const LINUX: bool = false;
        // The override wins on both platforms, over everything.
        for mac in [LINUX, true] {
            assert_eq!(
                resolve_path(mac, Some("/tmp/x/app.log"), Some("/s"), Some("/h")),
                Some(PathBuf::from("/tmp/x/app.log"))
            );
        }
        // Linux: XDG_STATE_HOME when set, else ~/.local/state.
        assert_eq!(
            resolve_path(LINUX, None, Some("/xdg/state"), Some("/home/u")),
            Some(PathBuf::from(
                "/xdg/state/ai.stridelabs.shed-desktop/shed-desktop.log"
            ))
        );
        assert_eq!(
            resolve_path(LINUX, None, None, Some("/home/u")),
            Some(PathBuf::from(
                "/home/u/.local/state/ai.stridelabs.shed-desktop/shed-desktop.log"
            ))
        );
        // An empty or relative XDG_STATE_HOME is ignored (the XDG spec).
        for bad in ["", "rel/state"] {
            assert_eq!(
                resolve_path(LINUX, None, Some(bad), Some("/home/u")),
                Some(PathBuf::from(
                    "/home/u/.local/state/ai.stridelabs.shed-desktop/shed-desktop.log"
                )),
                "{bad:?}"
            );
        }
        // macOS: ~/Library/Logs/ShedDesktop, XDG or not.
        assert_eq!(
            resolve_path(true, None, Some("/xdg/state"), Some("/Users/u")),
            Some(PathBuf::from(
                "/Users/u/Library/Logs/ShedDesktop/shed-desktop.log"
            ))
        );
        // Nothing to resolve against: no file.
        assert_eq!(resolve_path(true, None, None, None), None);
        assert_eq!(resolve_path(LINUX, None, None, Some("relative")), None);
        assert_eq!(resolve_path(LINUX, Some(""), None, None), None);
        // ...unless XDG names one on Linux.
        assert!(resolve_path(LINUX, None, Some("/s"), None).is_some());
    }

    /// The build's own branch: the one `env.rs` asks for.
    #[test]
    fn this_build_takes_its_own_platform_default() {
        let p = resolve_path(MAC, None, Some("/xdg"), Some("/home/u")).unwrap();
        if cfg!(target_os = "macos") {
            assert!(p.starts_with("/home/u/Library/Logs/ShedDesktop"), "{p:?}");
        } else {
            assert!(p.starts_with("/xdg/ai.stridelabs.shed-desktop"), "{p:?}");
        }
    }

    #[test]
    fn a_line_is_stamped_with_its_level() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("app.log");
        let sink = Sink::new(MAX_BYTES, KEEP, 8);
        sink.open(Some(&path)).unwrap();
        assert_eq!(sink.line(Level::WARN, "hello there"), None);
        let got = lines(&path);
        assert_eq!(got.len(), 1);
        let (ts, rest) = got[0].split_once(' ').unwrap();
        assert!(ts.ends_with('Z') && ts.contains('T'), "{ts}");
        assert_eq!(rest, " WARN hello there");
        assert_eq!(sink.active_path(), Some(path.clone()));
        // Opening is once only: a second call changes nothing.
        sink.open(Some(&dir.path().join("other.log"))).unwrap();
        assert_eq!(sink.active_path(), Some(path));
        assert!(!dir.path().join("other.log").exists());
    }

    /// Directories the sink creates are 0700; an existing parent keeps its
    /// mode; the file is 0600, and so is every rotated file.
    #[test]
    fn rotation_shifts_keeps_three_and_keeps_0600() {
        let dir = tempfile::tempdir().unwrap();
        std::fs::set_permissions(dir.path(), Permissions::from_mode(0o755)).unwrap();
        let path = dir.path().join("a/b/app.log");
        // Each line is 32 bytes with its newline; the cap is two lines.
        let sink = Sink::new(64, 3, 8);
        sink.open(Some(&path)).unwrap();
        assert_eq!(
            mode(dir.path()),
            0o755,
            "an existing parent is never touched"
        );
        assert_eq!(mode(&dir.path().join("a")), 0o700);
        assert_eq!(mode(&dir.path().join("a/b")), 0o700);

        let raw = |n: usize| format!("line-{n:026}");
        for n in 0..9 {
            assert_eq!(sink.raw(&raw(n)), None);
        }
        // 9 lines, rotated after every second: .3 [0,1] dropped by the
        // fourth rotation, so .3 = [2,3], .2 = [4,5], .1 = [6,7], live = [8].
        assert_eq!(lines(&path), vec![raw(8)]);
        assert_eq!(lines(&numbered(&path, 1)), vec![raw(6), raw(7)]);
        assert_eq!(lines(&numbered(&path, 2)), vec![raw(4), raw(5)]);
        assert_eq!(lines(&numbered(&path, 3)), vec![raw(2), raw(3)]);
        assert!(!numbered(&path, 4).exists(), "only three are kept");
        for p in [
            path.clone(),
            numbered(&path, 1),
            numbered(&path, 2),
            numbered(&path, 3),
        ] {
            assert_eq!(mode(&p), 0o600, "{p:?}");
        }
    }

    /// A file already at the cap when the app starts is rotated at open; one
    /// under it is appended to — and a wider mode is narrowed to 0600.
    #[test]
    fn open_rotates_a_full_file_and_appends_to_a_short_one() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("app.log");
        std::fs::write(&path, "x".repeat(100)).unwrap();
        std::fs::set_permissions(&path, Permissions::from_mode(0o644)).unwrap();
        let sink = Sink::new(100, 3, 8);
        sink.open(Some(&path)).unwrap();
        assert_eq!(
            std::fs::read_to_string(numbered(&path, 1)).unwrap().len(),
            100
        );
        assert_eq!(std::fs::metadata(&path).unwrap().len(), 0);

        let short = dir.path().join("short.log");
        std::fs::write(&short, "earlier\n").unwrap();
        std::fs::set_permissions(&short, Permissions::from_mode(0o644)).unwrap();
        let sink = Sink::new(100, 3, 8);
        sink.open(Some(&short)).unwrap();
        sink.line(Level::INFO, "later");
        let got = lines(&short);
        assert_eq!(got[0], "earlier");
        assert_eq!(message(&got[1]), "later");
        assert_eq!(mode(&short), 0o600);
        assert!(!numbered(&short, 1).exists());
    }

    /// Several threads at once, across many rotations: every line lands whole
    /// and alone, and each thread's lines keep their order.
    #[test]
    fn concurrent_writers_produce_whole_lines_across_rotations() {
        const THREADS: usize = 8;
        const PER: usize = 200;
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("app.log");
        // Small cap, many kept: every line survives, so each can be checked.
        let sink = leak(Sink::new(4096, 1000, 8));
        sink.open(Some(&path)).unwrap();
        let handles: Vec<_> = (0..THREADS)
            .map(|t| {
                std::thread::spawn(move || {
                    for n in 0..PER {
                        sink.line(Level::INFO, &format!("t{t} n{n:04} {} end", "x".repeat(40)));
                    }
                })
            })
            .collect();
        for h in handles {
            h.join().unwrap();
        }
        let mut files: Vec<PathBuf> = (1..1000)
            .map(|n| numbered(&path, n))
            .take_while(|p| p.exists())
            .collect();
        assert!(files.len() > 2, "the run rotated: {}", files.len());
        files.reverse();
        files.push(path.clone());
        let all: Vec<String> = files.iter().flat_map(|p| lines(p)).collect();
        assert_eq!(all.len(), THREADS * PER);
        let mut next = [0usize; THREADS];
        for line in &all {
            let msg = message(line);
            let parts: Vec<&str> = msg.split(' ').collect();
            assert_eq!(parts.len(), 4, "a whole, uninterleaved line: {line:?}");
            assert_eq!(parts[2], "x".repeat(40), "{line:?}");
            assert_eq!(parts[3], "end", "{line:?}");
            let t: usize = parts[0][1..].parse().unwrap();
            let n: usize = parts[1][1..].parse().unwrap();
            assert_eq!(n, next[t], "thread {t}'s lines in order");
            next[t] += 1;
        }
    }

    /// A destination that cannot be opened: file logging is off, said once;
    /// later lines say nothing more and `identify.log_path` reads null.
    #[test]
    fn an_unopenable_destination_falls_back_with_one_notice() {
        let dir = tempfile::tempdir().unwrap();
        let blocker = dir.path().join("a-file");
        std::fs::write(&blocker, "").unwrap();
        let path = blocker.join("sub/app.log");
        let sink = Sink::new(MAX_BYTES, KEEP, 8);
        sink.line(Level::INFO, "before");
        let notice = sink.open(Some(&path)).expect_err("a file is no directory");
        assert!(notice.contains(&path.display().to_string()), "{notice}");
        assert!(notice.contains("stderr only"), "{notice}");
        assert_eq!(sink.line(Level::WARN, "after"), None, "no second notice");
        assert_eq!(sink.active_path(), None);
        // No path at all: the same, with its own reason.
        let sink = Sink::new(MAX_BYTES, KEEP, 8);
        assert!(sink.open(None).unwrap_err().contains("SHED_TAURI_LOG_FILE"));
        assert_eq!(sink.line(Level::INFO, "x"), None);
    }

    /// A WRITE that fails (a full disk) turns file logging off with exactly one
    /// notice. The failure is made portably: the open handle is swapped for a
    /// read-only one on the same file, so the next append fails.
    #[test]
    fn a_failing_write_falls_back_with_exactly_one_notice() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("app.log");
        let sink = Sink::new(MAX_BYTES, KEEP, 8);
        sink.open(Some(&path)).unwrap();
        match &mut *sink.lock() {
            State::Open(open) => open.file = File::open(&path).unwrap(),
            _ => panic!("the sink is open"),
        }
        let notice = sink.line(Level::INFO, "one").expect("the notice");
        assert!(
            notice.contains(&path.display().to_string()) && notice.contains("stderr only"),
            "{notice}"
        );
        for _ in 0..5 {
            assert_eq!(sink.line(Level::INFO, "more"), None);
        }
        assert_eq!(sink.active_path(), None);
    }

    /// A symlink at the live path is refused, never followed: one notice, the
    /// held lines and every later one kept out of its target (whose mode is
    /// untouched too). A dangling one is refused the same way, and the file it
    /// names is never created.
    #[test]
    fn a_symlink_at_the_live_path_is_refused_and_never_followed() {
        let dir = tempfile::tempdir().unwrap();
        let target = dir.path().join("elsewhere.log");
        std::fs::write(&target, "precious\n").unwrap();
        std::fs::set_permissions(&target, Permissions::from_mode(0o644)).unwrap();
        let path = dir.path().join("app.log");
        std::os::unix::fs::symlink(&target, &path).unwrap();
        let sink = Sink::new(MAX_BYTES, KEEP, 8);
        sink.line(Level::INFO, "held before the open");
        let notice = sink.open(Some(&path)).expect_err("a symlink is refused");
        assert!(notice.contains(&path.display().to_string()), "{notice}");
        assert!(
            notice.contains("symbolic link") && notice.contains("stderr only"),
            "{notice}"
        );
        assert_eq!(sink.line(Level::WARN, "after"), None, "one notice only");
        assert_eq!(sink.active_path(), None);
        assert_eq!(std::fs::read_to_string(&target).unwrap(), "precious\n");
        assert_eq!(mode(&target), 0o644, "the target's mode is untouched");
        assert!(std::fs::symlink_metadata(&path)
            .unwrap()
            .file_type()
            .is_symlink());

        let missing = dir.path().join("missing.log");
        let dangling = dir.path().join("dangling.log");
        std::os::unix::fs::symlink(&missing, &dangling).unwrap();
        let sink = Sink::new(MAX_BYTES, KEEP, 8);
        let notice = sink.open(Some(&dangling)).expect_err("a dangling one too");
        assert!(notice.contains("symbolic link"), "{notice}");
        assert!(!missing.exists(), "its target is never created");
    }

    /// A FIFO at the path is refused without waiting for a reader — the open
    /// runs on a thread the test bounds — and refused when a reader is there
    /// (the handle's `fstat`); a device is refused the same way. A regular file
    /// beside them opens as ever.
    #[test]
    fn a_fifo_or_a_device_at_the_path_is_refused_without_blocking() {
        use std::os::unix::ffi::OsStrExt;
        let dir = tempfile::tempdir().unwrap();
        let fifo = dir.path().join("app.log");
        let c_path = std::ffi::CString::new(fifo.as_os_str().as_bytes()).unwrap();
        // SAFETY: a valid NUL-terminated path; mkfifo reads nothing else.
        assert_eq!(unsafe { libc::mkfifo(c_path.as_ptr(), 0o600) }, 0);
        let open = |path: PathBuf| {
            let (tx, rx) = std::sync::mpsc::channel();
            std::thread::spawn(move || {
                let sink = Sink::new(MAX_BYTES, KEEP, 8);
                let opened = sink.open(Some(&path));
                let _ = tx.send((opened, sink.active_path()));
            });
            rx.recv_timeout(std::time::Duration::from_secs(5))
                .expect("the open returned at once")
        };

        let (opened, active) = open(fifo.clone());
        let notice = opened.expect_err("a FIFO with no reader is refused");
        assert!(
            notice.contains("FIFO") && notice.contains("stderr only"),
            "{notice}"
        );
        assert_eq!(active, None);

        let _reader = OpenOptions::new()
            .read(true)
            .custom_flags(libc::O_NONBLOCK)
            .open(&fifo)
            .unwrap();
        let (opened, active) = open(fifo.clone());
        let notice = opened.expect_err("a FIFO with a reader is refused too");
        assert!(notice.contains("FIFO"), "{notice}");
        assert_eq!(active, None);

        let (opened, _) = open(PathBuf::from("/dev/null"));
        let notice = opened.expect_err("a device is refused");
        assert!(notice.contains("a device"), "{notice}");

        let regular = dir.path().join("regular.log");
        let (opened, active) = open(regular.clone());
        opened.unwrap();
        assert_eq!(active, Some(regular));
    }

    /// Rotation never follows a symlink, nor moves one: a link at a rotated
    /// name stays put until a regular file is renamed onto it (replacing the
    /// link, not its target); and once the live name stops naming the file the
    /// sink has open (a symlink took its place), that name is not renamed and
    /// the reopen refuses the link — off, with one notice.
    #[test]
    fn rotation_never_follows_or_moves_a_symlink() {
        let dir = tempfile::tempdir().unwrap();
        let victim = dir.path().join("victim");
        std::fs::write(&victim, "precious\n").unwrap();
        let path = dir.path().join("app.log");
        std::fs::write(&path, "x".repeat(100)).unwrap();
        std::os::unix::fs::symlink(&victim, numbered(&path, 1)).unwrap();
        std::os::unix::fs::symlink(&victim, numbered(&path, 3)).unwrap();
        // Full at open: rotates now — the link at `.1` stays, then is replaced.
        let sink = Sink::new(100, 3, 8);
        sink.open(Some(&path)).unwrap();
        assert_eq!(lines(&numbered(&path, 1)), vec!["x".repeat(100)]);
        assert!(!numbered(&path, 2).exists(), "the link was not moved along");
        // Two more rotations carry that file to `.3`, over the other link.
        for _ in 0..2 {
            assert_eq!(sink.raw(&"y".repeat(100)), None);
        }
        assert_eq!(lines(&numbered(&path, 3)), vec!["x".repeat(100)]);
        for n in 1..=3 {
            assert!(regular_at(&numbered(&path, n)).unwrap().is_some(), ".{n}");
        }
        assert_eq!(std::fs::read_to_string(&victim).unwrap(), "precious\n");

        // The live name taken by a symlink after the open.
        let path = dir.path().join("b.log");
        let sink = Sink::new(64, 3, 8);
        sink.open(Some(&path)).unwrap();
        std::fs::remove_file(&path).unwrap();
        std::os::unix::fs::symlink(&victim, &path).unwrap();
        let notice = sink.raw(&"z".repeat(70)).expect("the notice");
        assert!(notice.contains("symbolic link"), "{notice}");
        assert!(!numbered(&path, 1).exists(), "the link was not renamed");
        assert!(std::fs::symlink_metadata(&path)
            .unwrap()
            .file_type()
            .is_symlink());
        assert_eq!(std::fs::read_to_string(&victim).unwrap(), "precious\n");
        assert_eq!(sink.raw("more"), None, "one notice only");
        assert_eq!(sink.active_path(), None);
    }

    /// Without the single-instance lock the file never opens: the held lines
    /// are dropped, one notice says why, nothing touches the path, and
    /// `identify.log_path` reads null. With the lock it opens as usual.
    #[test]
    fn without_the_instance_lock_the_file_stays_shut() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("logs/app.log");
        let sink = Sink::new(MAX_BYTES, KEEP, 4);
        for n in 0..10 {
            assert_eq!(sink.line(Level::INFO, &format!("early {n}")), None);
        }
        let notice = start(&sink, Some(&path), false).expect_err("the notice");
        assert!(
            notice.contains("single-instance lock") && notice.contains("stderr only"),
            "{notice}"
        );
        assert!(!notice.contains('\n'), "one line: {notice:?}");
        assert!(
            matches!(*sink.lock(), State::Off),
            "the held lines are gone, and nothing more is held"
        );
        assert_eq!(sink.line(Level::WARN, "later"), None, "no second notice");
        assert_eq!(sink.active_path(), None);
        // Once only: a later open changes nothing.
        start(&sink, Some(&path), true).unwrap();
        assert_eq!(sink.active_path(), None);
        assert!(
            !dir.path().join("logs").exists(),
            "nothing touched the path"
        );

        let sink = Sink::new(MAX_BYTES, KEEP, 4);
        sink.line(Level::INFO, "early");
        start(&sink, Some(&path), true).unwrap();
        assert_eq!(sink.active_path(), Some(path.clone()));
        assert_eq!(lines(&path).len(), 1);
    }

    /// A ROTATION that fails turns file logging off with exactly one notice.
    #[test]
    fn a_failing_rotation_falls_back_with_exactly_one_notice() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("app.log");
        // `.log.1` is a non-empty directory: renaming the file onto it fails
        // (as root too).
        std::fs::create_dir(numbered(&path, 1)).unwrap();
        std::fs::write(numbered(&path, 1).join("x"), "").unwrap();
        let sink = Sink::new(10, 1, 8);
        sink.open(Some(&path)).unwrap();
        let notice = sink
            .line(Level::INFO, "long enough to cross the cap")
            .expect("the notice");
        assert!(notice.contains("stderr only"), "{notice}");
        assert_eq!(sink.line(Level::INFO, "more"), None);
        assert_eq!(sink.active_path(), None);
    }

    /// Lines said before the sink opens are written first, in order; past the
    /// cap the oldest go, and the file says how many.
    #[test]
    fn the_pre_sink_buffer_flushes_in_order_and_is_bounded() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("app.log");
        let sink = Sink::new(MAX_BYTES, KEEP, 4);
        for n in 0..6 {
            assert_eq!(sink.line(Level::INFO, &format!("early {n}")), None);
        }
        assert!(!path.exists(), "nothing touches the file before it opens");
        sink.open(Some(&path)).unwrap();
        sink.line(Level::INFO, "late");
        let got: Vec<String> = lines(&path)
            .iter()
            .map(|l| message(l).to_string())
            .collect();
        assert_eq!(
            got,
            vec![
                "shed-desktop-tauri: 2 earlier line(s) were dropped before the log file opened",
                "early 2",
                "early 3",
                "early 4",
                "early 5",
                "late",
            ]
        );

        // Under the cap: all of them, no drop line.
        let path = dir.path().join("b.log");
        let sink = Sink::new(MAX_BYTES, KEEP, PRE_SINK_LINES);
        for n in 0..3 {
            sink.line(Level::WARN, &format!("early {n}"));
        }
        sink.open(Some(&path)).unwrap();
        let got: Vec<String> = lines(&path)
            .iter()
            .map(|l| message(l).to_string())
            .collect();
        assert_eq!(got, vec!["early 0", "early 1", "early 2"]);
    }

    /// A `tracing` event, through the same subscriber factory the app
    /// installs, lands in the sink as one stamped line.
    #[test]
    fn a_tracing_event_lands_in_the_sink() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("app.log");
        let sink = leak(Sink::new(MAX_BYTES, KEEP, 8));
        sink.open(Some(&path)).unwrap();
        tracing::subscriber::with_default(subscriber(sink, false), || {
            tracing::warn!(target: "shed_app::roost", "a resync after a gap");
            tracing::debug!(target: "shed_app::roost", "quiet by default");
        });
        let got = lines(&path);
        assert_eq!(got.len(), 1, "{got:?}");
        let line = &got[0];
        assert!(line.contains(" WARN "), "{line}");
        assert!(
            line.ends_with("shed_app::roost: a resync after a gap"),
            "{line}"
        );
        assert!(line.split(' ').next().unwrap().ends_with('Z'), "{line}");
    }

    /// The embedded broker's lines are `shed_broker` events: stamped like the
    /// app's own (`<LEVEL> shed_broker: <msg>`), the quiet tier only with debug.
    #[test]
    fn the_brokers_lines_land_as_shed_broker_events() {
        use shed_app::BusLog;
        for debug in [false, true] {
            let dir = tempfile::tempdir().unwrap();
            let path = dir.path().join("app.log");
            let sink = leak(Sink::new(MAX_BYTES, KEEP, 8));
            sink.open(Some(&path)).unwrap();
            tracing::subscriber::with_default(subscriber(sink, debug), || {
                BrokerLog.warn("SSE connection lost server=mini2");
                BrokerLog.debug("SSE still down server=mini2");
            });
            let got: Vec<String> = lines(&path)
                .iter()
                .map(|l| l.split_once(' ').unwrap().1.trim_start().to_string())
                .collect();
            let mut want = vec!["WARN shed_broker: SSE connection lost server=mini2"];
            if debug {
                want.push("DEBUG shed_broker: SSE still down server=mini2");
            }
            assert_eq!(got, want, "debug={debug}");
        }
    }

    /// By default roost-ipc is off — its warnings and errors too, since they
    /// carry ssh's own stderr — while the shed crates keep INFO and up and other
    /// crates INFO; `SHED_TAURI_LOG=debug` admits roost-ipc, down to DEBUG.
    #[test]
    fn roost_ipc_is_off_by_default_and_admitted_with_debug() {
        for debug in [false, true] {
            let dir = tempfile::tempdir().unwrap();
            let path = dir.path().join("app.log");
            let sink = leak(Sink::new(MAX_BYTES, KEEP, 8));
            sink.open(Some(&path)).unwrap();
            tracing::subscriber::with_default(subscriber(sink, debug), || {
                tracing::warn!(target: "roost_ipc::ssh", "warn-roost-ipc");
                tracing::error!(target: "roost_ipc::bootstrap", "error-roost-ipc");
                tracing::debug!(target: "roost_ipc::client", "debug-roost-ipc");
                tracing::info!(target: "shed_app::roost", "info-shed-app");
                tracing::warn!(target: "shed_broker", "warn-shed-broker");
                tracing::info!(target: "tauri::app", "info-tauri");
            });
            let text = std::fs::read_to_string(&path).unwrap();
            for kept in ["info-shed-app", "warn-shed-broker", "info-tauri"] {
                assert!(text.contains(kept), "debug={debug}: {kept} missing: {text}");
            }
            for roost in ["warn-roost-ipc", "error-roost-ipc", "debug-roost-ipc"] {
                assert_eq!(
                    text.contains(roost),
                    debug,
                    "debug={debug}: {roost}: {text}"
                );
            }
        }
    }

    /// `SHED_TAURI_LOG=debug` raises the shed crates and roost-ipc only.
    #[test]
    fn the_debug_filter_admits_shed_and_roost_but_no_other_crate() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("app.log");
        let sink = leak(Sink::new(MAX_BYTES, KEEP, 8));
        sink.open(Some(&path)).unwrap();
        tracing::subscriber::with_default(subscriber(sink, true), || {
            tracing::debug!(target: "shed_app::roost", "debug-shed-app");
            tracing::debug!(target: "shed_craze::watcher", "debug-shed-craze");
            tracing::debug!(target: "roost_ipc::client", "debug-roost-ipc");
            tracing::debug!(target: "hyper_util::client", "debug-hyper");
            tracing::debug!(target: "tauri::app", "debug-tauri");
            tracing::info!(target: "tauri::app", "info-tauri");
        });
        let text = std::fs::read_to_string(&path).unwrap();
        for admitted in [
            "debug-shed-app",
            "debug-shed-craze",
            "debug-roost-ipc",
            "info-tauri",
        ] {
            assert!(text.contains(admitted), "{admitted} missing: {text}");
        }
        for refused in ["debug-hyper", "debug-tauri"] {
            assert!(!text.contains(refused), "{refused} leaked: {text}");
        }
    }
}
