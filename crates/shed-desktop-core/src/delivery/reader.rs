//! The disk [`Reader`] the providers read through: the person's files and commands,
//! exactly as `shed_provision::Reader`'s documentation requires.
//!
//! * A file is opened non-blocking (`O_NONBLOCK | O_NOCTTY`), following links; the
//!   OPENED descriptor is checked to be a regular file (`fstat`), and at most `limit`
//!   bytes (plus one, to tell a file that is too large) are read from it. Its mode is
//!   that same descriptor's.
//! * A command is an explicit argv, run with no shell, no stdin, in a process group of
//!   its own; it is killed (the whole group) past its timeout or once either of its
//!   outputs passes the bound. A bare program name is looked up on the `PATH` the reader
//!   was given (the app's own), an absolute path is run as it is, and anything else is
//!   refused.
//! * **Cancellation.** Every running command's process group is registered with the
//!   reader's [`Children`] (shared by its clones). [`DiskReader::cancel`] (the app's
//!   shutdown) refuses every later command, kills every registered group, and waits,
//!   bounded, until each has been reaped: a quit leaves no provider command behind.

use std::collections::HashSet;
use std::ffi::OsString;
use std::fs::File;
use std::io::Read;
use std::os::unix::fs::{OpenOptionsExt, PermissionsExt};
use std::os::unix::process::CommandExt;
use std::path::{Path, PathBuf};
use std::process::Stdio;
use std::sync::{mpsc, Arc, Condvar, Mutex};
use std::time::Duration;

use shed_provision::{Command, CommandOutput, FileData, ReadError, Reader, RunError};

/// The commands a reader (and its clones) is running: their process groups, and
/// whether the reader was cancelled.
#[derive(Debug, Default)]
pub struct Children {
    state: Mutex<ChildState>,
    reaped: Condvar,
}

#[derive(Debug, Default)]
struct ChildState {
    cancelled: bool,
    running: HashSet<libc::pid_t>,
}

impl Children {
    fn lock(&self) -> std::sync::MutexGuard<'_, ChildState> {
        self.state.lock().unwrap_or_else(|e| e.into_inner())
    }

    /// Refuses every later command, kills every running command's process group, and
    /// waits up to `bound` until each has been reaped; `true` when all were.
    pub fn cancel(&self, bound: Duration) -> bool {
        let mut st = self.lock();
        st.cancelled = true;
        for pid in &st.running {
            // SAFETY: a plain syscall on a registered child's own process group: a
            // registered child is not reaped (only `finish` reaps, under this lock), so
            // the group id is still its own.
            unsafe {
                libc::kill(-*pid, libc::SIGKILL);
            }
        }
        let (st, _) = self
            .reaped
            .wait_timeout_while(st, bound, |st| !st.running.is_empty())
            .unwrap_or_else(|e| e.into_inner());
        st.running.is_empty()
    }

    /// How many commands are running.
    pub fn running(&self) -> usize {
        self.lock().running.len()
    }

    /// Kills `pid`'s process group if `pid` is still registered: registered means not
    /// yet reaped (only [`Self::finish`] reaps, under this lock), so the group id cannot
    /// have been reused.
    fn kill_group(&self, pid: libc::pid_t) {
        let st = self.lock();
        if st.running.contains(&pid) {
            // SAFETY: a plain syscall on an unreaped child's own process group.
            unsafe {
                libc::kill(-pid, libc::SIGKILL);
            }
        }
    }

    /// Called once `pid` has exited (seen, not reaped): under the lock, kills whatever
    /// it left in its group, reaps it, and unregisters it. The wait status, or `None`
    /// when it could not be reaped.
    fn finish(&self, pid: libc::pid_t) -> Option<libc::c_int> {
        let mut st = self.lock();
        let mut status: libc::c_int = 0;
        // SAFETY: plain syscalls on our own unreaped child; it has exited, so `waitpid`
        // returns at once.
        let reaped = unsafe {
            libc::kill(-pid, libc::SIGKILL);
            loop {
                let r = libc::waitpid(pid, &mut status, 0);
                if r == -1 && std::io::Error::last_os_error().raw_os_error() == Some(libc::EINTR) {
                    continue;
                }
                break r == pid;
            }
        };
        st.running.remove(&pid);
        drop(st);
        self.reaped.notify_all();
        reaped.then_some(status)
    }
}

/// Blocks until `pid` (our child) has exited, without reaping it (`waitid` with
/// `WNOWAIT`): its id stays its own until [`Children::finish`] reaps it.
fn wait_exit_unreaped(pid: libc::pid_t) {
    loop {
        // SAFETY: a zeroed siginfo_t is a valid out-parameter; `pid` is our child.
        let r = unsafe {
            let mut info: libc::siginfo_t = std::mem::zeroed();
            libc::waitid(
                libc::P_PID,
                pid as libc::id_t,
                &mut info,
                libc::WEXITED | libc::WNOWAIT,
            )
        };
        if r == 0 {
            return;
        }
        if std::io::Error::last_os_error().raw_os_error() != Some(libc::EINTR) {
            // Not our child any more (cannot happen): nothing to wait for.
            return;
        }
    }
}

/// The disk and this person's commands.
#[derive(Debug, Clone)]
pub struct DiskReader {
    home: PathBuf,
    path: Option<OsString>,
    children: Arc<Children>,
}

impl DiskReader {
    /// A reader whose `~` is `home`, finding bare program names on `path` (the value of
    /// a `PATH` variable; `None` finds none).
    pub fn new(home: impl Into<PathBuf>, path: Option<OsString>) -> Self {
        DiskReader {
            home: home.into(),
            path,
            children: Arc::default(),
        }
    }

    /// Stops this reader and its clones: see [`Children::cancel`].
    pub fn cancel(&self, bound: Duration) -> bool {
        self.children.cancel(bound)
    }

    /// The commands this reader and its clones are running.
    pub fn children(&self) -> &Arc<Children> {
        &self.children
    }

    /// The reader of this process: `$HOME` and `$PATH`.
    pub fn from_process() -> Self {
        Self::new(
            shed_source::paths::user_home_dir(),
            std::env::var_os("PATH"),
        )
    }

    /// Where `program` is: an absolute path as it is, a bare name on the `PATH`.
    fn locate(&self, program: &str) -> Option<PathBuf> {
        if program.is_empty() {
            return None;
        }
        let p = Path::new(program);
        if p.is_absolute() {
            return Some(p.to_path_buf());
        }
        if program.contains('/') {
            return None;
        }
        let path = self.path.as_ref()?;
        std::env::split_paths(path)
            .filter(|dir| dir.is_absolute())
            .map(|dir| dir.join(program))
            .find(|c| is_executable(c))
    }
}

fn is_executable(p: &Path) -> bool {
    std::fs::metadata(p).is_ok_and(|m| m.is_file() && m.permissions().mode() & 0o111 != 0)
}

fn read_error(e: &std::io::Error) -> ReadError {
    match e.kind() {
        std::io::ErrorKind::NotFound => ReadError::NotFound,
        std::io::ErrorKind::PermissionDenied => ReadError::Denied,
        _ if e.raw_os_error() == Some(libc::ENOTDIR) => ReadError::NotFound,
        _ => ReadError::Other,
    }
}

/// Reads at most `max + 1` bytes of `r`, so `> max` says there was more.
fn read_capped(r: &mut impl Read, max: usize) -> std::io::Result<Vec<u8>> {
    let mut out = Vec::new();
    r.take(max as u64 + 1).read_to_end(&mut out)?;
    Ok(out)
}

impl Reader for DiskReader {
    fn home(&self) -> &Path {
        &self.home
    }

    fn read_file(&self, path: &Path, limit: usize) -> Result<FileData, ReadError> {
        let mut file: File = std::fs::OpenOptions::new()
            .read(true)
            .custom_flags(libc::O_NONBLOCK | libc::O_NOCTTY)
            .open(path)
            .map_err(|e| read_error(&e))?;
        let meta = file.metadata().map_err(|e| read_error(&e))?;
        if !meta.file_type().is_file() {
            return Err(ReadError::NotAFile);
        }
        let bytes = read_capped(&mut file, limit).map_err(|e| read_error(&e))?;
        if bytes.len() > limit {
            return Err(ReadError::TooLarge);
        }
        Ok(FileData {
            bytes,
            mode: meta.permissions().mode() & 0o7777,
        })
    }

    fn run(&self, command: &Command) -> Result<CommandOutput, RunError> {
        let program = self.locate(&command.program).ok_or(RunError::NotFound)?;
        // Spawned and registered under the registry's lock, so a cancel either refuses
        // it or sees it.
        let mut registry = self.children.lock();
        if registry.cancelled {
            return Err(RunError::Failed);
        }
        let mut child = std::process::Command::new(&program)
            .args(&command.args)
            .stdin(Stdio::null())
            .stdout(Stdio::piped())
            .stderr(Stdio::piped())
            .process_group(0)
            .spawn()
            .map_err(|e| match e.kind() {
                std::io::ErrorKind::NotFound => RunError::NotFound,
                _ => RunError::Failed,
            })?;
        let pid = child.id() as libc::pid_t;
        registry.running.insert(pid);
        drop(registry);
        let children = self.children.clone();
        let max = command.max_output;
        let (tx, rx) = mpsc::channel::<Option<()>>();
        let reader = |mut pipe: Box<dyn Read + Send>, tx: mpsc::Sender<Option<()>>| {
            std::thread::spawn(move || {
                let got = read_capped(&mut pipe, max).unwrap_or_default();
                if got.len() > max {
                    let _ = tx.send(None);
                }
                got
            })
        };
        let out = reader(Box::new(child.stdout.take().expect("piped")), tx.clone());
        let err = reader(Box::new(child.stderr.take().expect("piped")), tx.clone());
        // The child is never reaped by `std` (no `wait` on it): the waiter sees it exit
        // without reaping it, and `Children::finish` signals its group one last time and
        // reaps it under the registry's lock, so no group is ever signalled once its
        // leader's id could have been reused.
        let waiter = {
            let children = children.clone();
            std::thread::spawn(move || {
                wait_exit_unreaped(pid);
                let status = children.finish(pid);
                let _ = tx.send(Some(()));
                status
            })
        };
        let deadline = std::time::Instant::now() + command.timeout;
        let mut failure = None;
        loop {
            let left = deadline.saturating_duration_since(std::time::Instant::now());
            match rx.recv_timeout(left.max(Duration::from_millis(1))) {
                Ok(Some(())) => break,
                Ok(None) => {
                    failure.get_or_insert(RunError::OutputTooLarge);
                    children.kill_group(pid);
                }
                Err(mpsc::RecvTimeoutError::Timeout) => {
                    failure.get_or_insert(RunError::TimedOut);
                    children.kill_group(pid);
                }
                Err(mpsc::RecvTimeoutError::Disconnected) => break,
            }
        }
        let status = waiter.join().map_err(|_| RunError::Failed)?;
        drop(child);
        let stdout = out.join().unwrap_or_default();
        let stderr = err.join().unwrap_or_default();
        if children.lock().cancelled {
            // Killed by a cancel: not a result.
            return Err(RunError::Failed);
        }
        if let Some(f) = failure {
            return Err(f);
        }
        if stdout.len() > max || stderr.len() > max {
            return Err(RunError::OutputTooLarge);
        }
        let status = status.ok_or(RunError::Failed)?;
        let exit_code = if libc::WIFEXITED(status) {
            Some(libc::WEXITSTATUS(status))
        } else {
            None
        };
        Ok(CommandOutput {
            exit_code,
            stdout,
            stderr,
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::os::unix::fs::symlink;

    fn reader(home: &Path) -> DiskReader {
        DiskReader::new(home, std::env::var_os("PATH"))
    }

    #[test]
    fn a_regular_file_is_read_with_its_mode_and_links_are_followed() {
        let dir = tempfile::tempdir().unwrap();
        let f = dir.path().join("secret");
        std::fs::write(&f, b"hello").unwrap();
        std::fs::set_permissions(&f, std::fs::Permissions::from_mode(0o600)).unwrap();
        let link = dir.path().join("link");
        symlink(&f, &link).unwrap();
        let r = reader(dir.path());
        for p in [&f, &link] {
            let d = r.read_file(p, 16).unwrap();
            assert_eq!((d.bytes.as_slice(), d.mode), (&b"hello"[..], 0o600));
            assert!(d.owner_only());
        }
        assert_eq!(r.read_file(&f, 5).unwrap().bytes, b"hello");
        assert_eq!(r.read_file(&f, 4), Err(ReadError::TooLarge));
        assert_eq!(
            r.read_file(&dir.path().join("absent"), 4),
            Err(ReadError::NotFound)
        );
        assert_eq!(
            r.read_file(&f.join("under-a-file"), 4),
            Err(ReadError::NotFound)
        );
    }

    #[test]
    fn a_directory_and_a_fifo_are_not_files_and_the_fifo_does_not_block() {
        let dir = tempfile::tempdir().unwrap();
        let r = reader(dir.path());
        assert_eq!(r.read_file(dir.path(), 4), Err(ReadError::NotAFile));
        let fifo = dir.path().join("fifo");
        let c = std::ffi::CString::new(fifo.to_str().unwrap()).unwrap();
        // SAFETY: a valid C string.
        assert_eq!(unsafe { libc::mkfifo(c.as_ptr(), 0o600) }, 0);
        assert_eq!(r.read_file(&fifo, 4), Err(ReadError::NotAFile));
        let link = dir.path().join("to-fifo");
        symlink(&fifo, &link).unwrap();
        assert_eq!(r.read_file(&link, 4), Err(ReadError::NotAFile));
    }

    #[test]
    fn an_unreadable_file_is_denied() {
        // Root reads anything; the check means nothing there.
        // SAFETY: a plain syscall.
        if unsafe { libc::geteuid() } == 0 {
            return;
        }
        let dir = tempfile::tempdir().unwrap();
        let f = dir.path().join("locked");
        std::fs::write(&f, b"x").unwrap();
        std::fs::set_permissions(&f, std::fs::Permissions::from_mode(0o000)).unwrap();
        assert_eq!(reader(dir.path()).read_file(&f, 4), Err(ReadError::Denied));
    }

    fn script(dir: &Path, name: &str, body: &str) -> PathBuf {
        let p = dir.join(name);
        std::fs::write(&p, format!("#!/bin/sh\n{body}\n")).unwrap();
        std::fs::set_permissions(&p, std::fs::Permissions::from_mode(0o755)).unwrap();
        p
    }

    #[test]
    fn a_command_runs_by_argv_with_no_shell() {
        let dir = tempfile::tempdir().unwrap();
        let s = script(
            dir.path(),
            "echoer",
            "printf '%s|' \"$@\"; echo err >&2; exit 3",
        );
        let r = reader(dir.path());
        let mut c = Command::new(s.to_str().unwrap(), ["a b", "$HOME", "; rm -rf /"]);
        c.timeout = Duration::from_secs(5);
        let out = r.run(&c).unwrap();
        assert_eq!(out.stdout, b"a b|$HOME|; rm -rf /|");
        assert_eq!(out.stderr, b"err\n");
        assert_eq!(out.exit_code, Some(3));
        assert!(!out.success());
    }

    #[test]
    fn a_bare_name_is_found_on_the_given_path_only() {
        let dir = tempfile::tempdir().unwrap();
        script(dir.path(), "fake-gh", "echo ok");
        let r = DiskReader::new(dir.path(), Some(dir.path().as_os_str().to_owned()));
        assert_eq!(
            r.run(&Command::new("fake-gh", ["x"])).unwrap().stdout,
            b"ok\n"
        );
        let none = DiskReader::new(dir.path(), None);
        assert_eq!(
            none.run(&Command::new("fake-gh", ["x"])),
            Err(RunError::NotFound)
        );
        assert_eq!(
            r.run(&Command::new("./fake-gh", ["x"])),
            Err(RunError::NotFound),
            "a relative path is refused"
        );
    }

    /// Whether `pid` is a live process (a zombie is not).
    fn alive(pid: i32) -> bool {
        // SAFETY: signal 0 only checks.
        if unsafe { libc::kill(pid, 0) } != 0 {
            return false;
        }
        match std::fs::read_to_string(format!("/proc/{pid}/stat")) {
            Ok(stat) => stat
                .rsplit_once(") ")
                .is_some_and(|(_, rest)| !rest.starts_with('Z')),
            // No /proc (macOS): it answered signal 0.
            Err(_) => !cfg!(target_os = "linux"),
        }
    }

    /// A cancel (the app's shutdown) during a slow command kills its whole group, waits
    /// for it to be reaped, and refuses every later command.
    #[test]
    fn a_cancel_kills_and_reaps_a_running_command_and_refuses_the_next() {
        let dir = tempfile::tempdir().unwrap();
        let pids = dir.path().join("pids");
        let slow = script(
            dir.path(),
            "slowpass",
            &format!(
                "sleep 30 & echo $! >> {p}; echo $$ >> {p}; wait",
                p = pids.display()
            ),
        );
        let r = reader(dir.path());
        let mut c = Command::new(slow.to_str().unwrap(), Vec::<String>::new());
        c.timeout = Duration::from_secs(60);
        let runner = {
            let r = r.clone();
            std::thread::spawn(move || r.run(&c))
        };
        let deadline = std::time::Instant::now() + Duration::from_secs(10);
        let read_pids = || -> Vec<i32> {
            std::fs::read_to_string(&pids)
                .unwrap_or_default()
                .lines()
                .filter_map(|l| l.trim().parse().ok())
                .collect()
        };
        while read_pids().len() < 2 {
            assert!(
                std::time::Instant::now() < deadline,
                "the command did not start"
            );
            std::thread::sleep(Duration::from_millis(10));
        }
        assert_eq!(r.children().running(), 1);
        assert!(r.cancel(Duration::from_secs(5)), "reaped within the bound");
        assert_eq!(r.children().running(), 0);
        assert!(runner.join().unwrap().is_err());
        let deadline = std::time::Instant::now() + Duration::from_secs(5);
        while read_pids().into_iter().any(alive) {
            assert!(
                std::time::Instant::now() < deadline,
                "a process of the group lives"
            );
            std::thread::sleep(Duration::from_millis(10));
        }
        let echo = script(dir.path(), "echoer", "echo hi");
        assert_eq!(
            r.run(&Command::new(echo.to_str().unwrap(), Vec::<String>::new())),
            Err(RunError::Failed),
            "nothing runs after a cancel"
        );
    }

    #[test]
    fn a_command_past_its_timeout_or_bound_is_killed() {
        let dir = tempfile::tempdir().unwrap();
        let slow = script(dir.path(), "slow", "sleep 30");
        let r = reader(dir.path());
        let mut c = Command::new(slow.to_str().unwrap(), Vec::<String>::new());
        c.timeout = Duration::from_millis(200);
        let started = std::time::Instant::now();
        assert_eq!(r.run(&c), Err(RunError::TimedOut));
        assert!(started.elapsed() < Duration::from_secs(10));

        let loud = script(dir.path(), "loud", "yes");
        let mut c = Command::new(loud.to_str().unwrap(), Vec::<String>::new());
        c.max_output = 1024;
        c.timeout = Duration::from_secs(10);
        assert_eq!(r.run(&c), Err(RunError::OutputTooLarge));
    }
}
