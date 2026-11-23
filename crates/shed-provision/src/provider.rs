//! The [`Provider`] trait and what a provider contributes to an answer.

use crate::answer::{ItemBody, ProblemCode};
use crate::only::Only;
use crate::reader::Reader;
use crate::request::Request;

/// What a delivery is for: the server the source is connected to, and the
/// request, which a preview (the Preferences pane, before any shed asks)
/// does not have.
#[derive(Debug, Clone, Copy)]
pub struct Context<'a> {
    /// The server, by the name the person's config gives it, when known.
    pub server: Option<&'a str>,
    /// The request being answered, when there is one.
    pub request: Option<&'a Request>,
}

impl<'a> Context<'a> {
    /// An answer to `request`, from the server named `server`.
    pub fn answer(server: &'a str, request: &'a Request) -> Context<'a> {
        Context {
            server: Some(server),
            request: Some(request),
        }
    }

    /// A preview: no request, so no `only` rule can be judged.
    pub fn preview() -> Context<'a> {
        Context {
            server: None,
            request: None,
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
