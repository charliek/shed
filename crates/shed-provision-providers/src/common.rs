//! What the providers share: reading a setting's file into a problem when
//! it cannot be, and stripping a command's line end.

use shed_provision::{
    Command, CommandOutput, Contribution, FileData, ProblemCode, ReadError, Reader, RunError,
};

/// Reads the file a setting names, or says why not in a problem whose
/// message names the setting (`what`), never the path.
pub(crate) fn read_setting_file(
    reader: &dyn Reader,
    path: &str,
    limit: usize,
    what: &str,
) -> Result<FileData, Contribution> {
    let p = reader.expand(path);
    reader.read_file(&p, limit).map_err(|e| {
        let (code, message) = match e {
            ReadError::NotFound => (ProblemCode::MissingInput, format!("{what} does not exist")),
            ReadError::TooLarge => (
                ProblemCode::Unreadable,
                format!("{what} is too large to deliver"),
            ),
            ReadError::NotAFile => (
                ProblemCode::Unreadable,
                format!("{what} is not a regular file"),
            ),
            ReadError::Denied => (
                ProblemCode::Unreadable,
                format!("{what} could not be read: permission denied"),
            ),
            ReadError::Other => (ProblemCode::Unreadable, format!("{what} could not be read")),
        };
        Contribution::problem(code, message)
    })
}

/// Runs a command, or says why it did not run to its end. A command that
/// ran and exited non-zero is returned as it is, for the provider to judge.
/// The message names the command by `what` and never holds its output.
pub(crate) fn run(
    reader: &dyn Reader,
    command: &Command,
    what: &str,
) -> Result<CommandOutput, Contribution> {
    reader.run(command).map_err(|e| {
        let (code, message) = match e {
            RunError::NotFound => (
                ProblemCode::MissingInput,
                format!("{what}: the program was not found"),
            ),
            RunError::TimedOut => (
                ProblemCode::CommandFailed,
                format!("{what} did not finish in time"),
            ),
            RunError::OutputTooLarge => (
                ProblemCode::CommandFailed,
                format!("{what} printed too much"),
            ),
            RunError::Failed => (
                ProblemCode::CommandFailed,
                format!("{what} could not be run"),
            ),
        };
        Contribution::problem(code, message)
    })
}

/// `bytes` as text with one trailing line end (`\n` or `\r\n`) taken off.
pub(crate) fn one_line(bytes: &[u8]) -> Option<&str> {
    let s = std::str::from_utf8(bytes).ok()?;
    let s = s.strip_suffix('\n').unwrap_or(s);
    Some(s.strip_suffix('\r').unwrap_or(s))
}
