//! A [`Reader`] made of maps, for tests here and in the crates that build
//! on this one: files with modes, and canned command results by argv.

use std::collections::HashMap;
use std::path::{Path, PathBuf};
use std::sync::Mutex;

use crate::reader::{Command, CommandOutput, FileData, ReadError, Reader, RunError};

/// What stands at a path in a [`FakeReader`].
#[derive(Debug, Clone)]
enum Entry {
    /// A regular file.
    File(FileData),
    /// A symbolic link to another path, which the reader follows.
    Link(PathBuf),
    /// A FIFO, device, socket or directory: never a regular file.
    Special,
    /// Reading it fails.
    Error(ReadError),
}

/// The most links one read follows before it gives up, as a kernel does.
const MAX_LINK_HOPS: usize = 40;

/// A reader of files and commands held in memory, modelling the [`Reader`]
/// contract: links are followed, the type is judged where the links end
/// (anything but a regular file is [`ReadError::NotAFile`]), and a file over
/// the limit is [`ReadError::TooLarge`]. It records every command it is
/// asked to run.
#[derive(Debug)]
pub struct FakeReader {
    home: PathBuf,
    files: HashMap<PathBuf, Entry>,
    commands: HashMap<Vec<String>, Result<CommandOutput, RunError>>,
    ran: Mutex<Vec<Command>>,
}

impl FakeReader {
    /// A reader whose home is `home`, holding nothing.
    pub fn new(home: impl Into<PathBuf>) -> FakeReader {
        FakeReader {
            home: home.into(),
            files: HashMap::new(),
            commands: HashMap::new(),
            ran: Mutex::new(Vec::new()),
        }
    }

    /// Adds a file at `path` (absolute, or `~/...`) with `mode`.
    pub fn file(mut self, path: &str, content: impl Into<Vec<u8>>, mode: u32) -> FakeReader {
        let p = self.expand(path);
        self.files.insert(
            p,
            Entry::File(FileData {
                bytes: content.into(),
                mode,
            }),
        );
        self
    }

    /// Makes `path` a symbolic link to `target` (absolute, or `~/...`).
    pub fn link(mut self, path: &str, target: &str) -> FakeReader {
        let (p, t) = (self.expand(path), self.expand(target));
        self.files.insert(p, Entry::Link(t));
        self
    }

    /// Puts something that is not a regular file at `path`: a FIFO, a
    /// device, a socket or a directory.
    pub fn special(mut self, path: &str) -> FakeReader {
        let p = self.expand(path);
        self.files.insert(p, Entry::Special);
        self
    }

    /// Makes reading `path` fail with `err`.
    pub fn file_error(mut self, path: &str, err: ReadError) -> FakeReader {
        let p = self.expand(path);
        self.files.insert(p, Entry::Error(err));
        self
    }

    /// Makes `argv` exit with `code` and `stdout`.
    pub fn command(mut self, argv: &[&str], code: i32, stdout: &str) -> FakeReader {
        self.commands.insert(
            argv.iter().map(|s| (*s).to_string()).collect(),
            Ok(CommandOutput {
                exit_code: Some(code),
                stdout: stdout.as_bytes().to_vec(),
                stderr: Vec::new(),
            }),
        );
        self
    }

    /// Makes `argv` fail with `err`.
    pub fn command_error(mut self, argv: &[&str], err: RunError) -> FakeReader {
        self.commands
            .insert(argv.iter().map(|s| (*s).to_string()).collect(), Err(err));
        self
    }

    /// The commands run so far.
    pub fn ran(&self) -> Vec<Command> {
        self.ran.lock().expect("not poisoned").clone()
    }
}

impl Reader for FakeReader {
    fn home(&self) -> &Path {
        &self.home
    }

    fn read_file(&self, path: &Path, limit: usize) -> Result<FileData, ReadError> {
        let mut at = path.to_path_buf();
        for _ in 0..=MAX_LINK_HOPS {
            match self.files.get(&at) {
                None => return Err(ReadError::NotFound),
                Some(Entry::Link(t)) => at = t.clone(),
                Some(Entry::Special) => return Err(ReadError::NotAFile),
                Some(Entry::Error(e)) => return Err(*e),
                Some(Entry::File(f)) if f.bytes.len() > limit => return Err(ReadError::TooLarge),
                Some(Entry::File(f)) => return Ok(f.clone()),
            }
        }
        Err(ReadError::Other)
    }

    fn run(&self, command: &Command) -> Result<CommandOutput, RunError> {
        self.ran.lock().expect("not poisoned").push(command.clone());
        let argv: Vec<String> = std::iter::once(command.program.clone())
            .chain(command.args.iter().cloned())
            .collect();
        match self.commands.get(&argv) {
            None => Err(RunError::NotFound),
            Some(Err(e)) => Err(*e),
            Some(Ok(o)) if o.stdout.len() > command.max_output => Err(RunError::OutputTooLarge),
            Some(Ok(o)) => Ok(o.clone()),
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn models_the_reader_contract() {
        let r = FakeReader::new("/home/ada")
            .file("~/real", "x", 0o600)
            .link("~/.ssh/id", "~/dotfiles/id")
            .link("~/dotfiles/id", "~/real")
            .special("~/fifo")
            .link("~/to-fifo", "~/fifo")
            .link("~/loop", "~/loop")
            .file("~/big", vec![0; 10], 0o600);
        let read = |p: &str, limit| r.read_file(&r.expand(p), limit);
        assert_eq!(read("~/.ssh/id", 1).unwrap().bytes, b"x");
        assert_eq!(read("~/fifo", 1), Err(ReadError::NotAFile));
        assert_eq!(read("~/to-fifo", 1), Err(ReadError::NotAFile));
        assert_eq!(read("~/loop", 1), Err(ReadError::Other));
        assert_eq!(read("~/big", 9), Err(ReadError::TooLarge));
        assert_eq!(read("~/big", 10).unwrap().bytes.len(), 10);
        assert_eq!(read("~/none", 1), Err(ReadError::NotFound));
    }
}
