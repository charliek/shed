//! The [`Reader`]: everything a provider may read, behind one small trait,
//! so this crate has no file or process access of its own. The desktop app
//! gives it the disk and the person's commands; a hosted service can give
//! it a store.

use std::fmt;
use std::path::{Path, PathBuf};
use std::time::Duration;

/// A file as a [`Reader`] read it.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct FileData {
    /// The whole content.
    pub bytes: Vec<u8>,
    /// The permission bits (`0o600`), so a provider can refuse a secret
    /// file others can read.
    pub mode: u32,
}

impl FileData {
    /// Whether neither the group nor others may read, write or run it
    /// (`0600` or stricter).
    pub fn owner_only(&self) -> bool {
        self.mode & 0o077 == 0
    }
}

/// Why a file could not be read. It names no path and no content.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum ReadError {
    /// There is no such file.
    NotFound,
    /// It is over the limit the provider asked for.
    TooLarge,
    /// It is not a regular file.
    NotAFile,
    /// Permission was refused.
    Denied,
    /// Any other failure.
    Other,
}

impl fmt::Display for ReadError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(match self {
            ReadError::NotFound => "not found",
            ReadError::TooLarge => "too large",
            ReadError::NotAFile => "not a regular file",
            ReadError::Denied => "permission denied",
            ReadError::Other => "could not be read",
        })
    }
}

impl std::error::Error for ReadError {}

/// A command to run: an explicit argv, never a shell. The [`Reader`]
/// enforces the timeout and the output bound.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Command {
    /// The program: a name the reader looks up, or an absolute path.
    pub program: String,
    /// Its arguments.
    pub args: Vec<String>,
    /// How long it may run before the reader kills it.
    pub timeout: Duration,
    /// The most bytes of stdout, and separately of stderr, the reader takes.
    pub max_output: usize,
}

impl Command {
    /// The default bound on a command's run time.
    pub const DEFAULT_TIMEOUT: Duration = Duration::from_secs(10);
    /// The default bound on a command's output.
    pub const DEFAULT_MAX_OUTPUT: usize = 64 << 10;

    /// `program` with `args`, under the default timeout and output bound.
    pub fn new<I, S>(program: impl Into<String>, args: I) -> Command
    where
        I: IntoIterator<Item = S>,
        S: Into<String>,
    {
        Command {
            program: program.into(),
            args: args.into_iter().map(Into::into).collect(),
            timeout: Command::DEFAULT_TIMEOUT,
            max_output: Command::DEFAULT_MAX_OUTPUT,
        }
    }
}

/// What a command did.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct CommandOutput {
    /// Its exit code (`None` when a signal ended it).
    pub exit_code: Option<i32>,
    /// Its standard output.
    pub stdout: Vec<u8>,
    /// Its standard error.
    pub stderr: Vec<u8>,
}

impl CommandOutput {
    /// Whether it exited 0.
    pub fn success(&self) -> bool {
        self.exit_code == Some(0)
    }
}

/// Why a command could not be run to its end.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum RunError {
    /// The program was not found.
    NotFound,
    /// It ran past its timeout and was killed.
    TimedOut,
    /// It wrote more than its output bound and was killed.
    OutputTooLarge,
    /// It could not be started, or failed some other way.
    Failed,
}

impl fmt::Display for RunError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(match self {
            RunError::NotFound => "program not found",
            RunError::TimedOut => "timed out",
            RunError::OutputTooLarge => "output too large",
            RunError::Failed => "could not be run",
        })
    }
}

impl std::error::Error for RunError {}

/// What providers read through.
///
/// **Links are followed, on purpose.** Every file a provider reads is in
/// the person's own home or named by their own settings. A link planted
/// there needs write access to that home, and whoever has it can read the
/// files a link would point at directly; meanwhile dotfile managers (stow
/// and the like) link `~/.ssh/...`, `~/.envsecrets/config.yaml` and
/// `~/.craze/...` as a matter of course. Refusing links would break them and
/// protect nothing.
///
/// What a reader of a real disk must do instead, in [`Reader::read_file`]:
///
/// 1. Open the path for reading, without blocking: `O_NONBLOCK` (and
///    `O_NOCTTY`) on the open, so a FIFO or a device at the path cannot
///    stall the open.
/// 2. Check the **opened descriptor** (`fstat`), never the path: it must be
///    a regular file. A FIFO, socket, device or directory is
///    [`ReadError::NotAFile`]. Checking the descriptor leaves no window for
///    the path to be swapped between the check and the read.
/// 3. Read at most `limit` bytes, plus one to tell a file that is too large
///    ([`ReadError::TooLarge`]); never the whole of a larger file.
/// 4. Report the permission bits of that same descriptor.
pub trait Reader {
    /// The person's home directory, absolute: where `~` in a setting
    /// points.
    fn home(&self) -> &Path;

    /// Reads the regular file at `path` (absolute), following links, and
    /// its permission bits, as the trait's documentation requires: the
    /// type checked on the opened descriptor, the open never blocking, and
    /// at most `limit` bytes read. A file over `limit` bytes is
    /// [`ReadError::TooLarge`]; anything but a regular file is
    /// [`ReadError::NotAFile`].
    fn read_file(&self, path: &Path, limit: usize) -> Result<FileData, ReadError>;

    /// Runs `command` with no shell and no stdin, kills it past its timeout
    /// or output bound, and returns what it did.
    fn run(&self, command: &Command) -> Result<CommandOutput, RunError>;

    /// `path` with a leading `~` or `~/` put under [`Reader::home`]; any
    /// other path as it is.
    fn expand(&self, path: &str) -> PathBuf {
        if path == "~" {
            self.home().to_path_buf()
        } else if let Some(rest) = path.strip_prefix("~/") {
            self.home().join(rest)
        } else {
            PathBuf::from(path)
        }
    }
}
