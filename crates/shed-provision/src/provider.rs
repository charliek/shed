//! The [`Provider`] trait and what a provider contributes to an answer.

use std::time::{Duration, Instant};

use crate::answer::{ItemBody, ProblemCode};
use crate::only::Only;
use crate::reader::Reader;
use crate::request::Request;

/// What a delivery is for: the server the source is connected to, and the
/// request, which a preview (the Preferences pane, before any shed asks)
/// does not have; and the two things a host injects, its wall clock and how
/// long the source has.
#[derive(Debug, Clone, Copy)]
pub struct Context<'a> {
    /// The server, by the name the person's config gives it, when known.
    pub server: Option<&'a str>,
    /// The request being answered, when there is one.
    pub request: Option<&'a Request>,
    /// The host's wall clock, in Unix seconds: what a provider judges an
    /// expiry against.
    pub now: i64,
    /// The instant by which the source must have answered (the request's
    /// deadline less a margin for the reply), or `None` for no bound. The
    /// builder starts no provider past it, and a provider starts no read or
    /// command past it and runs no command for longer than what is left.
    pub budget: Option<Instant>,
}

impl<'a> Context<'a> {
    /// An answer to `request`, from the server named `server`, at `now`
    /// (Unix seconds), to be given by `budget`.
    pub fn answer(
        server: &'a str,
        request: &'a Request,
        now: i64,
        budget: Option<Instant>,
    ) -> Context<'a> {
        Context {
            server: Some(server),
            request: Some(request),
            now,
            budget,
        }
    }

    /// A preview at `now`: no request, so no `only` rule can be judged, and
    /// no budget.
    pub fn preview(now: i64) -> Context<'a> {
        Context {
            server: None,
            request: None,
            now,
            budget: None,
        }
    }

    /// Whether `only` lets a delivery through. A preview lets every rule
    /// through (it shows what could be delivered somewhere); an answer
    /// holds every list given.
    pub fn allows(&self, only: &Only) -> bool {
        match self.request {
            None => true,
            Some(r) => only.matches(self.server.unwrap_or(""), r),
        }
    }

    /// What is left of the budget: `None` with no budget, zero once it has
    /// passed.
    pub fn time_left(&self) -> Option<Duration> {
        self.budget
            .map(|b| b.saturating_duration_since(Instant::now()))
    }

    /// Whether the budget has passed: nothing more may be started.
    pub fn out_of_time(&self) -> bool {
        self.time_left().is_some_and(|d| d.is_zero())
    }
}

/// One item a provider has for the shed, before the builder names its
/// provider and fingerprints it.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Draft {
    /// The item's name: unique in the answer, `[a-z0-9][a-z0-9._-]{0,63}`.
    /// A provider prefixes its own names with its own.
    pub name: String,
    /// The file or the variable.
    pub body: ItemBody,
    /// When the item goes stale, if it does.
    pub expires_at: Option<String>,
}

/// A problem a provider reports. The message is fixed text for people:
/// never a value, a path from the person's disk or a command's output.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ProblemDraft {
    /// What went wrong.
    pub code: ProblemCode,
    /// For people.
    pub message: String,
}

impl ProblemDraft {
    /// A problem with `code` and `message`.
    pub fn new(code: ProblemCode, message: impl Into<String>) -> ProblemDraft {
        ProblemDraft {
            code,
            message: message.into(),
        }
    }
}

/// One section of a git config fragment: `[name]` or `[name "subsection"]`
/// and its entries, in order (a key may repeat: `helper =` then
/// `helper = ...` resets a multi-valued key).
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct GitSection {
    /// The section name.
    pub name: String,
    /// The subsection, when there is one.
    pub subsection: Option<String>,
    /// `key = value` entries.
    pub entries: Vec<(String, String)>,
}

impl GitSection {
    /// A section with no subsection.
    pub fn new(name: &str, entries: &[(&str, &str)]) -> GitSection {
        GitSection {
            name: name.into(),
            subsection: None,
            entries: entries
                .iter()
                .map(|(k, v)| ((*k).into(), (*v).into()))
                .collect(),
        }
    }

    /// A section with a subsection.
    pub fn sub(name: &str, subsection: &str, entries: &[(&str, &str)]) -> GitSection {
        GitSection {
            subsection: Some(subsection.into()),
            ..GitSection::new(name, entries)
        }
    }
}

/// Everything one provider contributes to an answer.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct Contribution {
    /// Its items.
    pub items: Vec<Draft>,
    /// Its git config fragment, merged by the builder with every other
    /// provider's into the one `.config/git/config` item.
    pub git_config: Vec<GitSection>,
    /// What it could not deliver.
    pub problems: Vec<ProblemDraft>,
}

impl Contribution {
    /// A contribution of one problem and nothing else.
    pub fn problem(code: ProblemCode, message: impl Into<String>) -> Contribution {
        Contribution {
            problems: vec![ProblemDraft::new(code, message)],
            ..Contribution::default()
        }
    }
}

/// A source of items: an SSH key, a sign-in, a set of API keys, files.
pub trait Provider {
    /// The provider's name: the item-name alphabet, and the `provider` of
    /// its items and problems.
    fn name(&self) -> &str;

    /// Where it delivers: the builder skips it for a request this does not
    /// match. `None` delivers everywhere.
    fn only(&self) -> Option<&Only>;

    /// What it has for the shed `ctx` describes, read through `reader`.
    fn provide(&self, ctx: &Context<'_>, reader: &dyn Reader) -> Contribution;
}

/// A provider whose contribution a host has already made: from inputs it
/// holds (an account service's stored entries, built by the providers'
/// `build`), handed to the builder with the name and `only` rule it
/// delivers under. It reads nothing.
#[derive(Debug, Clone)]
pub struct Prepared {
    /// The provider's name.
    pub name: String,
    /// Where it delivers; `None` delivers everywhere.
    pub only: Option<Only>,
    /// What it contributes.
    pub contribution: Contribution,
}

impl Provider for Prepared {
    fn name(&self) -> &str {
        &self.name
    }

    fn only(&self) -> Option<&Only> {
        self.only.as_ref()
    }

    fn provide(&self, _ctx: &Context<'_>, _reader: &dyn Reader) -> Contribution {
        self.contribution.clone()
    }
}
