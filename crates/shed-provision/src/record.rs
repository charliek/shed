//! The record (`CONTRACT.md`, section 4): what the server keeps per shed
//! about its delivery, as the shed document carries it. A reader holds it
//! to the contract on the document as written (`internal/delivery/record.go`),
//! so an absent member and an empty one are never confused.

use crate::answer::{parse_problem, Base, ItemKind, Problem};
use crate::json::{check_members, check_types, index, join, parse_document, Kind, Node};
use crate::limits::MAX_PROBLEMS;
use crate::request::Reason;
use crate::rules::{Refusal, Rule};
use crate::text::{
    check_path, check_variable, has_control, refused_home_target, valid_fingerprint, valid_id,
    valid_name, valid_time,
};

macro_rules! closed_list {
    ($(#[$doc:meta])* $ty:ident { $( $variant:ident => $name:literal ),* $(,)? }) => {
        $(#[$doc])*
        #[derive(Debug, Clone, Copy, PartialEq, Eq, serde::Serialize)]
        pub enum $ty {
            $(
                #[doc = concat!("`", $name, "`")]
                #[serde(rename = $name)]
                $variant,
            )*
        }

        impl $ty {
            /// Every value, in the contract's order.
            pub const ALL: &'static [$ty] = &[$( $ty::$variant, )*];

            /// The value as the contract writes it.
            pub fn as_str(self) -> &'static str {
                match self {
                    $( $ty::$variant => $name, )*
                }
            }

            /// The value written `s`, if there is one.
            pub fn from_name(s: &str) -> Option<$ty> {
                match s {
                    $( $name => Some($ty::$variant), )*
                    _ => None,
                }
            }
        }
    };
}

closed_list!(
    /// A shed's delivery state.
    State {
        None => "none",
        NotApplicable => "not_applicable",
        Delivered => "delivered",
        Partial => "partial",
        NotDelivered => "not_delivered",
    }
);

closed_list!(
    /// The state of one tracked step.
    StepState {
        Done => "done",
        Skipped => "skipped",
        Failed => "failed",
        Owed => "owed",
    }
);

closed_list!(
    /// Why a step was skipped or failed, or why a delivery failed.
    StepReason {
        NotApplicable => "not_applicable",
        FromSnapshot => "from_snapshot",
        Adopted => "adopted",
        NoItems => "no_items",
        NoRepo => "no_repo",
        LocalDir => "local_dir",
        NoProjectDir => "no_project_dir",
        NoHook => "no_hook",
        NoProvision => "no_provision",
        SourceOffline => "source_offline",
        SourceTimeout => "source_timeout",
        SourceDisconnected => "source_disconnected",
        AnswerRefused => "answer_refused",
        WriteFailed => "write_failed",
        ImageTooOld => "image_too_old",
        CloneFailed => "clone_failed",
        ProjectDirExists => "project_dir_exists",
        HookFailed => "hook_failed",
    }
);

closed_list!(
    /// What became of the last delivery's request.
    Outcome {
        Accepted => "accepted",
        Refused => "refused",
        NoAnswer => "no_answer",
        NotRequested => "not_requested",
    }
);

closed_list!(
    /// What became of one item in the shed. The first seven are the
    /// writer's; the last two are the record's own.
    ItemResult {
        Ok => "ok",
        IsDirectory => "is_directory",
        Symlink => "symlink",
        NotDirectory => "not_directory",
        Permission => "permission",
        OtherFilesystem => "other_filesystem",
        Io => "io",
        NotWritten => "not_written",
        NotInLastAnswer => "not_in_last_answer",
    }
);

impl ItemResult {
    /// Whether the writer reports it (every result but the record's own
    /// two).
    pub fn is_writer_result(self) -> bool {
        !matches!(self, ItemResult::NotWritten | ItemResult::NotInLastAnswer)
    }
}

/// The four tracked steps, in the order they run.
pub const STEP_NAMES: [&str; 4] = ["stage1", "clone", "stage2", "install"];

/// A shed's delivery record.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Record {
    /// The state the rest of the record gives.
    pub state: State,
    /// Absent only for a shed with no record.
    pub steps: Option<Steps>,
    /// The last delivery attempt.
    pub last: Option<Last>,
    /// The items the shed holds or was sent (never an empty list).
    pub items: Vec<RecordItem>,
}

/// The state of each tracked step.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Steps {
    /// Stage 1 items.
    pub stage1: Step,
    /// The repository.
    pub clone: Step,
    /// Stage 2 items.
    pub stage2: Step,
    /// The repository's install hook.
    pub install: Step,
}

impl Steps {
    /// The steps in the order they run.
    pub fn list(&self) -> [&Step; 4] {
        [&self.stage1, &self.clone, &self.stage2, &self.install]
    }
}

/// One step's state and, when skipped or failed, why.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Step {
    /// The step's state.
    pub state: StepState,
    /// Present exactly when the step is skipped or failed.
    pub reason: Option<StepReason>,
}

/// The last delivery: when, why, and what became of it.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Last {
    /// When.
    pub at: String,
    /// Why the request was made.
    pub reason: Reason,
    /// What became of it.
    pub outcome: Outcome,
    /// Why it failed, when it did.
    pub failure: Option<Failure>,
    /// The machine asked.
    pub source: Option<SourceMachine>,
    /// The answer's problems (never an empty list).
    pub problems: Vec<Problem>,
}

/// Why the last delivery failed.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Failure {
    /// The failure reason.
    pub reason: StepReason,
    /// The rule an answer broke (`answer_refused` only).
    pub rule: Option<Rule>,
    /// The item a write failed at (`write_failed` only, and optional then).
    pub item: Option<String>,
    /// The writer's result (`write_failed` only).
    pub result: Option<ItemResult>,
}

/// The machine a request went to.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct SourceMachine {
    /// 32 lowercase hex digits.
    pub machine_id: String,
    /// Its display name.
    pub machine_name: String,
}

/// One delivered item as the record keeps it: never its content or value.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct RecordItem {
    /// The item's name.
    pub name: String,
    /// Its provider.
    pub provider: String,
    /// `1` or `2` (an env item's is `1`).
    pub stage: u8,
    /// File or env.
    pub kind: ItemKind,
    /// A file item's base.
    pub base: Option<Base>,
    /// A file's path or an env item's variable.
    pub target: String,
    /// The source's fingerprint.
    pub fingerprint: String,
    /// When it goes stale.
    pub expires_at: Option<String>,
    /// When it was written.
    pub written_at: Option<String>,
    /// What became of it.
    pub result: ItemResult,
}

/// The state the contract gives a record, from its steps, last delivery and
/// items, in order: none (no steps, or every step skipped as adopted),
/// not_applicable (no request made and no failure), delivered (every step
/// done or skipped, no failure and no problem: an image too old for
/// delivery skips both stages with a failure), partial (an item written),
/// else not_delivered.
pub fn derive_state(r: &Record) -> State {
    let Some(steps) = &r.steps else {
        return State::None;
    };
    let list = steps.list();
    if list
        .iter()
        .all(|s| s.state == StepState::Skipped && s.reason == Some(StepReason::Adopted))
    {
        return State::None;
    }
    if let Some(l) = &r.last {
        if l.outcome == Outcome::NotRequested && l.failure.is_none() {
            return State::NotApplicable;
        }
    }
    let settled = list
        .iter()
        .all(|s| matches!(s.state, StepState::Done | StepState::Skipped));
    if settled
        && r.last
            .as_ref()
            .is_none_or(|l| l.failure.is_none() && l.problems.is_empty())
    {
        return State::Delivered;
    }
    if r.items.iter().any(|it| it.result == ItemResult::Ok) {
        return State::Partial;
    }
    State::NotDelivered
}

fn skip_reasons(step: &str) -> &'static [StepReason] {
    use StepReason::*;
    match step {
        "stage1" => &[NotApplicable, NoItems, ImageTooOld, FromSnapshot, Adopted],
        "clone" => &[NoRepo, LocalDir, FromSnapshot, Adopted],
        "stage2" => &[
            NotApplicable,
            NoItems,
            NoProjectDir,
            LocalDir,
            ImageTooOld,
            FromSnapshot,
            Adopted,
        ],
        _ => &[NoHook, NoProvision, FromSnapshot, Adopted],
    }
}

fn fail_reasons(step: &str) -> &'static [StepReason] {
    use StepReason::*;
    match step {
        "stage1" => &[
            SourceOffline,
            SourceTimeout,
            SourceDisconnected,
            AnswerRefused,
            WriteFailed,
        ],
        "clone" => &[CloneFailed, ProjectDirExists],
        "stage2" => &[WriteFailed],
        _ => &[HookFailed],
    }
}

const RECORD_MEMBERS: &[&str] = &["state", "steps", "last", "items"];
const RECORD_TYPES: &[(&str, Kind)] = &[
    ("state", Kind::String),
    ("steps", Kind::Object),
    ("last", Kind::Object),
    ("items", Kind::Array),
];
const STEPS_TYPES: &[(&str, Kind)] = &[
    ("stage1", Kind::Object),
    ("clone", Kind::Object),
    ("stage2", Kind::Object),
    ("install", Kind::Object),
];
const STEP_MEMBERS: &[&str] = &["state", "reason"];
const STEP_TYPES: &[(&str, Kind)] = &[("state", Kind::String), ("reason", Kind::String)];
const LAST_MEMBERS: &[&str] = &["at", "reason", "outcome", "failure", "source", "problems"];
const LAST_TYPES: &[(&str, Kind)] = &[
    ("at", Kind::String),
    ("reason", Kind::String),
    ("outcome", Kind::String),
    ("failure", Kind::Object),
    ("source", Kind::Object),
    ("problems", Kind::Array),
];
const FAILURE_MEMBERS: &[&str] = &["reason", "rule", "item", "result"];
const FAILURE_TYPES: &[(&str, Kind)] = &[
    ("reason", Kind::String),
    ("rule", Kind::String),
    ("item", Kind::String),
    ("result", Kind::String),
];
const SOURCE_MEMBERS: &[&str] = &["machine_id", "machine_name"];
const SOURCE_TYPES: &[(&str, Kind)] =
    &[("machine_id", Kind::String), ("machine_name", Kind::String)];
const RITEM_MEMBERS: &[&str] = &[
    "name",
    "provider",
    "stage",
    "kind",
    "base",
    "target",
    "fingerprint",
    "expires_at",
    "written_at",
    "result",
];
const RITEM_REQUIRED: &[&str] = &[
    "name",
    "provider",
    "stage",
    "kind",
    "target",
    "fingerprint",
    "result",
];
const RITEM_TYPES: &[(&str, Kind)] = &[
    ("name", Kind::String),
    ("provider", Kind::String),
    ("stage", Kind::Number),
    ("kind", Kind::String),
    ("base", Kind::String),
    ("target", Kind::String),
    ("fingerprint", Kind::String),
    ("expires_at", Kind::String),
    ("written_at", Kind::String),
    ("result", Kind::String),
];

fn check_object(
    n: &Node,
    at: &str,
    allowed: &[&str],
    required: &[&str],
    types: &[(&str, Kind)],
) -> Result<(), Refusal> {
    if n.kind != Kind::Object {
        return Err(Refusal::new(Rule::WrongType, at));
    }
    check_members(n, at, allowed, required)?;
    check_types(n, at, types)
}

/// A conditional member must be present exactly when `want` holds.
fn presence(n: &Node, at: &str, name: &str, want: bool) -> Result<(), Refusal> {
    match (want, n.get(name).is_some()) {
        (true, false) => Err(Refusal::new(Rule::MissingMember, join(at, name))),
        (false, true) => Err(Refusal::new(Rule::UnexpectedMember, join(at, name))),
        _ => Ok(()),
    }
}

fn text(n: &Node, name: &str) -> String {
    n.get(name).map(|v| v.text.clone()).unwrap_or_default()
}

/// Decodes a record and holds it to the contract: the document rules, then
/// each object's members (unknown, missing, types), then what each member
/// must be and when it must be present, then the state against
/// [`derive_state`]. It returns the record or the first rule broken.
pub fn parse_record(body: &[u8]) -> Result<Record, Refusal> {
    let root = parse_document(body)?;
    check_object(&root, "", RECORD_MEMBERS, &["state"], RECORD_TYPES)?;
    let state = State::from_name(&text(&root, "state"))
        .ok_or_else(|| Refusal::new(Rule::BadState, "state"))?;
    let (sn, ln, inn) = (root.get("steps"), root.get("last"), root.get("items"));
    let mut rec = Record {
        state,
        steps: None,
        last: None,
        items: Vec::new(),
    };
    match sn {
        None => {
            if ln.is_some() {
                return Err(Refusal::new(Rule::UnexpectedMember, "last"));
            }
            if inn.is_some() {
                return Err(Refusal::new(Rule::UnexpectedMember, "items"));
            }
        }
        Some(sn) => {
            check_object(sn, "steps", &STEP_NAMES, &STEP_NAMES, STEPS_TYPES)?;
            let mut parsed = Vec::with_capacity(4);
            for name in STEP_NAMES {
                parsed.push(parse_step(
                    sn.get(name).expect("checked"),
                    &format!("steps.{name}"),
                    name,
                )?);
            }
            let mut it = parsed.into_iter();
            rec.steps = Some(Steps {
                stage1: it.next().expect("four"),
                clone: it.next().expect("four"),
                stage2: it.next().expect("four"),
                install: it.next().expect("four"),
            });
        }
    }
    if let Some(ln) = ln {
        rec.last = Some(parse_last(ln)?);
    }
    if let Some(inn) = inn {
        if inn.elems.is_empty() {
            return Err(Refusal::new(Rule::EmptyList, "items"));
        }
        let mut seen = std::collections::HashSet::new();
        for (i, n) in inn.elems.iter().enumerate() {
            let at = index("items", i);
            let it = parse_record_item(n, &at)?;
            let key = format!(
                "{}\0{}\0{}",
                it.kind.as_str(),
                it.base.map(Base::as_str).unwrap_or(""),
                it.target
            );
            if !seen.insert(key) {
                return Err(Refusal::new(Rule::DuplicateTarget, at));
            }
            rec.items.push(it);
        }
    }
    if rec.state != derive_state(&rec) {
        return Err(Refusal::new(Rule::StateMismatch, "state"));
    }
    Ok(rec)
}

fn parse_step(n: &Node, at: &str, name: &str) -> Result<Step, Refusal> {
    check_object(n, at, STEP_MEMBERS, &["state"], STEP_TYPES)?;
    let state = StepState::from_name(&text(n, "state"))
        .ok_or_else(|| Refusal::new(Rule::BadStepState, join(at, "state")))?;
    presence(
        n,
        at,
        "reason",
        matches!(state, StepState::Skipped | StepState::Failed),
    )?;
    let Some(r) = n.get("reason") else {
        return Ok(Step {
            state,
            reason: None,
        });
    };
    let allowed = if state == StepState::Skipped {
        skip_reasons(name)
    } else {
        fail_reasons(name)
    };
    let reason = StepReason::from_name(&r.text)
        .filter(|x| allowed.contains(x))
        .ok_or_else(|| Refusal::new(Rule::BadReason, join(at, "reason")))?;
    Ok(Step {
        state,
        reason: Some(reason),
    })
}

fn parse_last(n: &Node) -> Result<Last, Refusal> {
    const AT: &str = "last";
    check_object(
        n,
        AT,
        LAST_MEMBERS,
        &["at", "reason", "outcome"],
        LAST_TYPES,
    )?;
    let at = text(n, "at");
    if !valid_time(&at) {
        return Err(Refusal::new(Rule::BadTime, "last.at"));
    }
    let reason = match text(n, "reason").as_str() {
        "create" => Reason::Create,
        "provision" => Reason::Provision,
        _ => return Err(Refusal::new(Rule::BadLastReason, "last.reason")),
    };
    let outcome = Outcome::from_name(&text(n, "outcome"))
        .ok_or_else(|| Refusal::new(Rule::BadOutcome, "last.outcome"))?;
    let allowed: &[StepReason] = match outcome {
        Outcome::Accepted => &[StepReason::WriteFailed, StepReason::ImageTooOld],
        Outcome::Refused => &[StepReason::AnswerRefused],
        Outcome::NoAnswer => &[
            StepReason::SourceOffline,
            StepReason::SourceTimeout,
            StepReason::SourceDisconnected,
        ],
        Outcome::NotRequested => &[StepReason::ImageTooOld],
    };
    let mut last = Last {
        at,
        reason,
        outcome,
        failure: None,
        source: None,
        problems: Vec::new(),
    };

    // failure: required when the request was refused or not answered.
    if matches!(outcome, Outcome::Refused | Outcome::NoAnswer) {
        presence(n, AT, "failure", true)?;
    }
    if let Some(fnode) = n.get("failure") {
        const FW: &str = "last.failure";
        check_object(fnode, FW, FAILURE_MEMBERS, &["reason"], FAILURE_TYPES)?;
        let reason = StepReason::from_name(&text(fnode, "reason"))
            .filter(|r| allowed.contains(r))
            .ok_or_else(|| Refusal::new(Rule::BadFailure, format!("{FW}.reason")))?;
        presence(fnode, FW, "rule", reason == StepReason::AnswerRefused)?;
        let is_write = reason == StepReason::WriteFailed;
        // item only with write_failed, and optional then: absent, the
        // failure is the env file's own, no item's.
        if !is_write {
            presence(fnode, FW, "item", false)?;
        }
        presence(fnode, FW, "result", is_write)?;
        let mut failure = Failure {
            reason,
            rule: None,
            item: None,
            result: None,
        };
        if let Some(rn) = fnode.get("rule") {
            failure.rule = Some(
                Rule::from_name(&rn.text)
                    .filter(|r| r.is_answer_rule())
                    .ok_or_else(|| Refusal::new(Rule::BadRule, format!("{FW}.rule")))?,
            );
        }
        if is_write {
            if let Some(inode) = fnode.get("item") {
                if !valid_name(&inode.text) {
                    return Err(Refusal::new(Rule::BadName, format!("{FW}.item")));
                }
                failure.item = Some(inode.text.clone());
            }
            failure.result = Some(
                ItemResult::from_name(&text(fnode, "result"))
                    .filter(|r| *r != ItemResult::Ok && r.is_writer_result())
                    .ok_or_else(|| Refusal::new(Rule::BadResult, format!("{FW}.result")))?,
            );
        }
        last.failure = Some(failure);
    }

    // source: required when an answer came.
    if matches!(outcome, Outcome::Accepted | Outcome::Refused) {
        presence(n, AT, "source", true)?;
    }
    if let Some(snode) = n.get("source") {
        check_object(
            snode,
            "last.source",
            SOURCE_MEMBERS,
            SOURCE_MEMBERS,
            SOURCE_TYPES,
        )?;
        let s = SourceMachine {
            machine_id: text(snode, "machine_id"),
            machine_name: text(snode, "machine_name"),
        };
        if !valid_id(&s.machine_id) {
            return Err(Refusal::new(Rule::BadMachine, "last.source.machine_id"));
        }
        if s.machine_name.is_empty() || s.machine_name.len() > 63 || has_control(&s.machine_name) {
            return Err(Refusal::new(Rule::BadMachine, "last.source.machine_name"));
        }
        last.source = Some(s);
    }

    // problems: only with an accepted answer, and never an empty list.
    if let Some(pn) = n.get("problems") {
        if outcome != Outcome::Accepted {
            return Err(Refusal::new(Rule::UnexpectedMember, "last.problems"));
        }
        if pn.elems.is_empty() {
            return Err(Refusal::new(Rule::EmptyList, "last.problems"));
        }
        if pn.elems.len() > MAX_PROBLEMS {
            return Err(Refusal::new(Rule::TooManyProblems, "last.problems"));
        }
        for (i, e) in pn.elems.iter().enumerate() {
            last.problems
                .push(parse_problem(e, &index("last.problems", i))?);
        }
    }
    Ok(last)
}

fn parse_record_item(n: &Node, at: &str) -> Result<RecordItem, Refusal> {
    check_object(n, at, RITEM_MEMBERS, RITEM_REQUIRED, RITEM_TYPES)?;
    let name = text(n, "name");
    let provider = text(n, "provider");
    if !valid_name(&name) {
        return Err(Refusal::new(Rule::BadName, join(at, "name")));
    }
    if !valid_name(&provider) {
        return Err(Refusal::new(Rule::BadProvider, join(at, "provider")));
    }
    let stage = match text(n, "stage").as_str() {
        "1" => 1,
        "2" => 2,
        _ => return Err(Refusal::new(Rule::BadStage, join(at, "stage"))),
    };
    let kind = match text(n, "kind").as_str() {
        "file" => ItemKind::File,
        "env" => ItemKind::Env,
        _ => return Err(Refusal::new(Rule::BadKind, join(at, "kind"))),
    };
    presence(n, at, "base", kind == ItemKind::File)?;
    let target = text(n, "target");
    let mut base = None;
    match kind {
        ItemKind::File => {
            let b = match text(n, "base").as_str() {
                "home" => Base::Home,
                "project" => Base::Project,
                _ => return Err(Refusal::new(Rule::BadBase, join(at, "base"))),
            };
            if b == Base::Project && stage != 2 {
                return Err(Refusal::new(Rule::ProjectBaseStage, join(at, "base")));
            }
            if let Some(rule) = check_path(&target) {
                return Err(Refusal::new(rule, join(at, "target")));
            }
            if b == Base::Home && refused_home_target(&target) {
                return Err(Refusal::new(Rule::RefusedTarget, join(at, "target")));
            }
            base = Some(b);
        }
        ItemKind::Env => {
            if stage != 1 {
                return Err(Refusal::new(Rule::BadStage, join(at, "stage")));
            }
            if let Some(rule) = check_variable(&target) {
                return Err(Refusal::new(rule, join(at, "target")));
            }
        }
    }
    let fingerprint = text(n, "fingerprint");
    if !valid_fingerprint(&fingerprint) {
        return Err(Refusal::new(Rule::BadFingerprint, join(at, "fingerprint")));
    }
    let expires_at = n.get("expires_at").map(|v| v.text.clone());
    if let Some(t) = &expires_at {
        if !valid_time(t) {
            return Err(Refusal::new(Rule::BadTime, join(at, "expires_at")));
        }
    }
    let result = ItemResult::from_name(&text(n, "result"))
        .ok_or_else(|| Refusal::new(Rule::BadResult, join(at, "result")))?;
    presence(
        n,
        at,
        "written_at",
        matches!(result, ItemResult::Ok | ItemResult::NotInLastAnswer),
    )?;
    let written_at = n.get("written_at").map(|v| v.text.clone());
    if let Some(t) = &written_at {
        if !valid_time(t) {
            return Err(Refusal::new(Rule::BadTime, join(at, "written_at")));
        }
    }
    Ok(RecordItem {
        name,
        provider,
        stage,
        kind,
        base,
        target,
        fingerprint,
        expires_at,
        written_at,
        result,
    })
}
