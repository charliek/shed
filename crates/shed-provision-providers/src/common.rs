//! What the providers share: reading a setting's file into a problem when
//! it cannot be, running a command within the budget, stripping a command's
//! line end, drafting items, and putting a provider's gather and build
//! together.

use shed_provision::{
    Base, Command, CommandOutput, Context, Contribution, Draft, EnvItem, FileData, FileItem,
    ItemBody, Mode, ProblemCode, ReadError, Reader, RunError, Stage,
};

use crate::Gathered;

/// Reads the file a setting names, or says why not in a problem whose
/// message names the setting (`what`), never the path. Nothing is read once
/// the budget has passed.
pub(crate) fn read_setting_file(
    ctx: &Context<'_>,
    reader: &dyn Reader,
    path: &str,
    limit: usize,
    what: &str,
) -> Result<FileData, Contribution> {
    if ctx.out_of_time() {
        return Err(Contribution::problem(
            ProblemCode::TimedOut,
            format!("{what} was not read: the request's time ran out"),
        ));
    }
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
///
/// Nothing is started once the budget has passed, and a command runs for no
/// longer than what is left of it; one stopped by that cap is `timed_out`.
pub(crate) fn run(
    ctx: &Context<'_>,
    reader: &dyn Reader,
    command: &Command,
    what: &str,
) -> Result<CommandOutput, Contribution> {
    let mut command = command.clone();
    let mut capped = false;
    if let Some(left) = ctx.time_left() {
        if left.is_zero() {
            return Err(Contribution::problem(
                ProblemCode::TimedOut,
                format!("{what} was not run: the request's time ran out"),
            ));
        }
        if left < command.timeout {
            command.timeout = left;
            capped = true;
        }
    }
    reader.run(&command).map_err(|e| {
        let (code, message) = match e {
            RunError::NotFound => (
                ProblemCode::MissingInput,
                format!("{what}: the program was not found"),
            ),
            RunError::TimedOut if capped => (
                ProblemCode::TimedOut,
                format!("{what} did not finish before the request's time ran out"),
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

/// The gather of a provider that stops at its first problem: the inputs, or
/// none and that problem.
pub(crate) fn gathered<I>(r: Result<I, Contribution>) -> Gathered<I> {
    match r {
        Ok(inputs) => Gathered {
            inputs: Some(inputs),
            problems: Vec::new(),
        },
        Err(c) => Gathered {
            inputs: None,
            problems: c.problems,
        },
    }
}

/// A provider's contribution on a machine: `build` over what `gathered`
/// holds, with gather's problems appended to build's.
pub(crate) fn provide<I>(
    gathered: Gathered<I>,
    build: impl FnOnce(&I) -> Contribution,
) -> Contribution {
    let mut c = gathered.inputs.as_ref().map(build).unwrap_or_default();
    c.problems.extend(gathered.problems);
    c
}

/// A file item: under `base`, at stage 2 when that is the project.
pub(crate) fn file_draft(name: &str, base: Base, path: &str, mode: Mode, content: &[u8]) -> Draft {
    Draft {
        name: name.into(),
        body: ItemBody::File(FileItem {
            stage: if base == Base::Project {
                Stage::Two
            } else {
                Stage::One
            },
            base,
            path: path.into(),
            mode,
            content: content.to_vec(),
        }),
        expires_at: None,
    }
}

/// An env item.
pub(crate) fn env_draft(name: &str, variable: &str, value: &str) -> Draft {
    Draft {
        name: name.into(),
        body: ItemBody::Env(EnvItem {
            variable: variable.into(),
            value: value.into(),
        }),
        expires_at: None,
    }
}

/// The desktop's provider over one settings table: `name()` is the module's
/// `NAME`, `only()` the table's, and `provide` the module's `gather` then
/// `build`.
macro_rules! desktop_provider {
    ($(#[$doc:meta])* $ty:ident, $settings:ty) => {
        $(#[$doc])*
        #[derive(Debug, Clone)]
        pub struct $ty {
            settings: $settings,
        }

        impl $ty {
            /// The provider for these settings.
            pub fn new(settings: $settings) -> $ty {
                $ty { settings }
            }
        }

        impl shed_provision::Provider for $ty {
            fn name(&self) -> &str {
                NAME
            }

            fn only(&self) -> Option<&shed_provision::Only> {
                self.settings.only.as_ref()
            }

            fn provide(
                &self,
                ctx: &shed_provision::Context<'_>,
                reader: &dyn shed_provision::Reader,
            ) -> shed_provision::Contribution {
                $crate::common::provide(gather(&self.settings, ctx, reader), |i| build(i, ctx))
            }
        }
    };
}
pub(crate) use desktop_provider;

/// `n` bytes, for a redacting `Debug`.
pub(crate) struct Redacted(pub usize);

impl std::fmt::Debug for Redacted {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "<{} bytes>", self.0)
    }
}
