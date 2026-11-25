//! # shed-provision
//!
//! The source side of the provision contract (`tests/provision-contract/v2/`):
//! when a shed is created or provisioned, shed-server asks the person's
//! source for the files and environment variables the shed should hold, and
//! the source answers with items.
//!
//! - The contract's documents and every rule that judges them, matching the
//!   server's Go implementation rule for rule and in the same order:
//!   [`parse_request`], [`parse_answer`] (and [`Answer::to_json`],
//!   [`Answer::validate`]), [`parse_record`], with the rule names in
//!   [`Rule`]. The reader of `tests/provision-contract/v2/` runs in this
//!   crate's tests.
//! - The stream's framing: [`stream::EventReader`] and [`stream::read_event`].
//! - Building an answer: the [`Provider`] and [`Reader`] traits, the
//!   [`Only`] rules, and [`build_answer`], which merges the git config
//!   fragments, settles conflicting and nested targets, keeps the limits,
//!   fingerprints each item ([`FingerprintKey`]) and returns a redacted
//!   [`Preview`] beside the answer. [`preview`] shows the same without a
//!   request.
//!
//! It depends on no workspace crate and has no file or process access of
//! its own: providers read through a [`Reader`] the caller gives them. The
//! five providers and the `delivery.toml` settings are in
//! `shed-provision-providers`.

mod answer;
mod bundle;
mod fingerprint;
mod json;
pub mod limits;
mod only;
mod printable_table;
mod provider;
mod reader;
mod record;
mod request;
mod rules;
pub mod stream;
pub mod testing;
mod text;

#[cfg(test)]
mod contract_tests;

pub use answer::{
    env_file_size, landing, parse_answer, valid_message, Answer, Base, EnvItem, FileItem, Item,
    ItemBody, ItemKind, Mode, Problem, ProblemCode, Stage,
};
pub use bundle::{
    build_answer, check_draft, preview, render_git_config, Built, Preview, PreviewItem,
    ProviderReport, GIT_CONFIG_NAME, GIT_CONFIG_PATH,
};
pub use fingerprint::{fingerprint, fingerprint_body, FingerprintKey};
pub use only::{repo_pattern_matches, valid_repo_pattern, Only};
pub use provider::{Context, Contribution, Draft, GitSection, Prepared, ProblemDraft, Provider};
pub use reader::{Command, CommandOutput, FileData, NoReader, ReadError, Reader, RunError};
pub use record::{
    derive_state, parse_record, Failure, ItemResult, Last, Outcome, Record, RecordItem,
    SourceMachine, State, Step, StepReason, StepState, Steps, STEP_NAMES,
};
pub use request::{parse_request, Backend, Reason, Request, RequestRepo, RequestShed};
pub use rules::{Refusal, Rule, ANSWER_RULES, RECORD_RULES};
pub use text::{
    check_path, check_value, check_variable, format_time, has_control, parse_time, printable,
    printable_char, refused_home_target, refused_variable, unicode_version, valid_fingerprint,
    valid_id, valid_name, valid_time, valid_variable, REFUSED_HOME_FILES, REFUSED_HOME_TREE,
    REFUSED_VARIABLES, REFUSED_VARIABLE_PREFIXES,
};
