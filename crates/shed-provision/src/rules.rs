//! The contract's rule names (`CONTRACT.md`, "Rule names" and "Record rule
//! names"), and a refusal under one.

use std::fmt;

macro_rules! rules {
    ($( $variant:ident => $name:literal ),* $(,)?) => {
        /// One rule a document can break. The answer rules travel on the
        /// wire (a `400`, `413` or `422` names the rule an answer broke); the
        /// record rules and [`Rule::BadValue`] (a request member whose value
        /// breaks its rule) do not.
        #[derive(Debug, Clone, Copy, PartialEq, Eq, Hash)]
        pub enum Rule {
            $(
                #[doc = concat!("`", $name, "`")]
                $variant,
            )*
        }

        impl Rule {
            /// The rule's name, as the contract writes it.
            pub fn as_str(self) -> &'static str {
                match self {
                    $( Rule::$variant => $name, )*
                }
            }

            /// The rule named `name`, if there is one.
            pub fn from_name(name: &str) -> Option<Rule> {
                match name {
                    $( $name => Some(Rule::$variant), )*
                    _ => None,
                }
            }
        }
    };
}

rules! {
    BodyTooLarge => "body_too_large",
    EchoesCredential => "echoes_credential",
    NotJson => "not_json",
    DuplicateMember => "duplicate_member",
    NullValue => "null_value",
    UnknownMember => "unknown_member",
    MissingMember => "missing_member",
    WrongType => "wrong_type",
    UnsupportedVersion => "unsupported_version",
    BadId => "bad_id",
    IdMismatch => "id_mismatch",
    TooManyItems => "too_many_items",
    TooManyProblems => "too_many_problems",
    BadKind => "bad_kind",
    WrongKindMember => "wrong_kind_member",
    BadName => "bad_name",
    BadProvider => "bad_provider",
    BadStage => "bad_stage",
    BadBase => "bad_base",
    ProjectBaseStage => "project_base_stage",
    PathEmpty => "path_empty",
    PathTooLong => "path_too_long",
    PathNotPrintable => "path_not_printable",
    PathBackslash => "path_backslash",
    PathAbsolute => "path_absolute",
    PathTrailingSlash => "path_trailing_slash",
    PathEmptySegment => "path_empty_segment",
    PathDotSegment => "path_dot_segment",
    PathDotDotSegment => "path_dot_dot_segment",
    RefusedTarget => "refused_target",
    BadMode => "bad_mode",
    BadBase64 => "bad_base64",
    FileTooLarge => "file_too_large",
    BadVariable => "bad_variable",
    RefusedVariable => "refused_variable",
    ValueEmpty => "value_empty",
    ValueTooLong => "value_too_long",
    ValueNotPrintable => "value_not_printable",
    ValueEdgeSpace => "value_edge_space",
    BadFingerprint => "bad_fingerprint",
    BadTime => "bad_time",
    DuplicateName => "duplicate_name",
    DuplicateTarget => "duplicate_target",
    NestedTarget => "nested_target",
    TotalTooLarge => "total_too_large",
    EnvTooLarge => "env_too_large",
    BadProblemCode => "bad_problem_code",
    BadProblemMessage => "bad_problem_message",
    UnexpectedMember => "unexpected_member",
    EmptyList => "empty_list",
    BadState => "bad_state",
    BadStepState => "bad_step_state",
    BadReason => "bad_reason",
    BadLastReason => "bad_last_reason",
    BadOutcome => "bad_outcome",
    BadFailure => "bad_failure",
    BadRule => "bad_rule",
    BadSource => "bad_source",
    BadResult => "bad_result",
    StateMismatch => "state_mismatch",
    BadValue => "bad_value",
}

/// Every answer rule, in the order the contract checks them. These are the
/// rules that travel.
pub const ANSWER_RULES: [Rule; 48] = [
    Rule::BodyTooLarge,
    Rule::EchoesCredential,
    Rule::NotJson,
    Rule::DuplicateMember,
    Rule::NullValue,
    Rule::UnknownMember,
    Rule::MissingMember,
    Rule::WrongType,
    Rule::UnsupportedVersion,
    Rule::BadId,
    Rule::IdMismatch,
    Rule::TooManyItems,
    Rule::TooManyProblems,
    Rule::BadKind,
    Rule::WrongKindMember,
    Rule::BadName,
    Rule::BadProvider,
    Rule::BadStage,
    Rule::BadBase,
    Rule::ProjectBaseStage,
    Rule::PathEmpty,
    Rule::PathTooLong,
    Rule::PathNotPrintable,
    Rule::PathBackslash,
    Rule::PathAbsolute,
    Rule::PathTrailingSlash,
    Rule::PathEmptySegment,
    Rule::PathDotSegment,
    Rule::PathDotDotSegment,
    Rule::RefusedTarget,
    Rule::BadMode,
    Rule::BadBase64,
    Rule::FileTooLarge,
    Rule::BadVariable,
    Rule::RefusedVariable,
    Rule::ValueEmpty,
    Rule::ValueTooLong,
    Rule::ValueNotPrintable,
    Rule::ValueEdgeSpace,
    Rule::BadFingerprint,
    Rule::BadTime,
    Rule::DuplicateName,
    Rule::DuplicateTarget,
    Rule::NestedTarget,
    Rule::TotalTooLarge,
    Rule::EnvTooLarge,
    Rule::BadProblemCode,
    Rule::BadProblemMessage,
];

/// Every rule only a record can break, in the contract's order.
pub const RECORD_RULES: [Rule; 12] = [
    Rule::UnexpectedMember,
    Rule::EmptyList,
    Rule::BadState,
    Rule::BadStepState,
    Rule::BadReason,
    Rule::BadLastReason,
    Rule::BadOutcome,
    Rule::BadFailure,
    Rule::BadRule,
    Rule::BadSource,
    Rule::BadResult,
    Rule::StateMismatch,
];

impl Rule {
    /// Whether the rule is an answer rule: one that travels in a refusal.
    pub fn is_answer_rule(self) -> bool {
        ANSWER_RULES.contains(&self)
    }

    /// The HTTP status the server answers a refusal under this rule with:
    /// `413` for an oversize body, `400` for a body that is not a document,
    /// `422` for every other rule.
    pub fn status(self) -> u16 {
        match self {
            Rule::BodyTooLarge => 413,
            Rule::NotJson => 400,
            _ => 422,
        }
    }
}

impl fmt::Display for Rule {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(self.as_str())
    }
}

/// A document refused under a rule. `at` locates it (`items[3].path`,
/// `problems[0].code`, or empty for the whole document); it names members
/// the contract defines and indexes only, never a value (a member name the
/// contract does not define is written `*`), so a refusal may be logged and
/// shown.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Refusal {
    /// The rule broken.
    pub rule: Rule,
    /// Where, by member names and indexes.
    pub at: String,
}

impl Refusal {
    pub(crate) fn new(rule: Rule, at: impl Into<String>) -> Refusal {
        Refusal {
            rule,
            at: at.into(),
        }
    }
}

impl fmt::Display for Refusal {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        if self.at.is_empty() {
            write!(f, "refused: {}", self.rule)
        } else {
            write!(f, "refused: {} at {}", self.rule, self.at)
        }
    }
}

impl std::error::Error for Refusal {}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn names_round_trip_and_lists_are_disjoint() {
        for r in ANSWER_RULES.iter().chain(RECORD_RULES.iter()) {
            assert_eq!(Rule::from_name(r.as_str()), Some(*r));
        }
        for r in RECORD_RULES {
            assert!(!r.is_answer_rule(), "{r}");
        }
        assert!(!Rule::BadValue.is_answer_rule());
        assert_eq!(Rule::from_name("nope"), None);
        assert_eq!(Rule::BodyTooLarge.status(), 413);
        assert_eq!(Rule::NotJson.status(), 400);
        assert_eq!(Rule::NestedTarget.status(), 422);
    }
}
