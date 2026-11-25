//! The answer (`CONTRACT.md`, section 2): what a source sends for one
//! request, its items and problems, and every rule that judges it, in the
//! contract's order (`internal/delivery/answer.go`).

use base64::Engine as _;

use crate::json::{
    check_members, check_types, index, join, parse_document, write_string, Kind, Node,
};
use crate::limits::{
    HOME_DIR, MAX_BODY_BYTES, MAX_ENV_FILE_BYTES, MAX_FILE_BYTES, MAX_ITEMS, MAX_MESSAGE_BYTES,
    MAX_PROBLEMS, MAX_PROJECT_DIR_BYTES, MAX_TOTAL_FILE_BYTES,
};
use crate::request::Request;
use crate::rules::{Refusal, Rule};
use crate::text::{
    bounded_text, canonical_absolute, check_path, check_value, check_variable, printable,
    refused_home_target, valid_fingerprint, valid_id, valid_name, valid_time,
};

/// An item's kind.
#[derive(Debug, Clone, Copy, PartialEq, Eq, serde::Serialize)]
#[serde(rename_all = "snake_case")]
pub enum ItemKind {
    /// A file.
    File,
    /// An env variable.
    Env,
}

impl ItemKind {
    /// The kind as the contract writes it.
    pub fn as_str(self) -> &'static str {
        match self {
            ItemKind::File => "file",
            ItemKind::Env => "env",
        }
    }
}

/// The directory a file item's path is relative to.
#[derive(Debug, Clone, Copy, PartialEq, Eq, serde::Serialize, serde::Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum Base {
    /// The shed user's home directory.
    Home,
    /// The project directory (stage 2 only).
    Project,
}

impl Base {
    /// The base as the contract writes it.
    pub fn as_str(self) -> &'static str {
        match self {
            Base::Home => "home",
            Base::Project => "project",
        }
    }
}

/// A file item's mode.
#[derive(Debug, Clone, Copy, PartialEq, Eq, serde::Serialize)]
pub enum Mode {
    /// `0400`
    #[serde(rename = "0400")]
    M0400,
    /// `0600`
    #[serde(rename = "0600")]
    M0600,
    /// `0644`: a public key only, by the providers' convention.
    #[serde(rename = "0644")]
    M0644,
}

impl Mode {
    /// The mode as the contract writes it.
    pub fn as_str(self) -> &'static str {
        match self {
            Mode::M0400 => "0400",
            Mode::M0600 => "0600",
            Mode::M0644 => "0644",
        }
    }

    /// The mode written `s`, if it is one the contract allows.
    pub fn from_name(s: &str) -> Option<Mode> {
        match s {
            "0400" => Some(Mode::M0400),
            "0600" => Some(Mode::M0600),
            "0644" => Some(Mode::M0644),
            _ => None,
        }
    }
}

/// The stage a file item is written at.
#[derive(Debug, Clone, Copy, PartialEq, Eq, PartialOrd, Ord, serde::Serialize)]
pub enum Stage {
    /// Before the clone.
    #[serde(rename = "1")]
    One,
    /// After the clone.
    #[serde(rename = "2")]
    Two,
}

impl Stage {
    /// The stage as a number.
    pub fn number(self) -> u8 {
        match self {
            Stage::One => 1,
            Stage::Two => 2,
        }
    }
}

/// A file item's own members.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct FileItem {
    /// `1` or `2`; a `project` base is stage 2 only.
    pub stage: Stage,
    /// What `path` is relative to.
    pub base: Base,
    /// Relative and canonical (the path rules).
    pub path: String,
    /// The file's mode.
    pub mode: Mode,
    /// The decoded content.
    pub content: Vec<u8>,
}

/// An env item's own members. An env item is written at stage 1.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct EnvItem {
    /// `[A-Z_][A-Z0-9_]{0,127}`, not refused.
    pub variable: String,
    /// 1 to 8,192 bytes of printable characters, no space at either end,
    /// used byte for byte.
    pub value: String,
}

/// What an item writes.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum ItemBody {
    /// A file.
    File(FileItem),
    /// An env variable.
    Env(EnvItem),
}

/// One thing to write into the shed.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Item {
    /// Unique in the answer, `[a-z0-9][a-z0-9._-]{0,63}`.
    pub name: String,
    /// The provider that made it, same alphabet.
    pub provider: String,
    /// The file or the variable.
    pub body: ItemBody,
    /// 1 to 64 of `[A-Za-z0-9_-]`, chosen by the source.
    pub fingerprint: String,
    /// A time after which the item is stale.
    pub expires_at: Option<String>,
}

impl Item {
    /// The item's kind.
    pub fn kind(&self) -> ItemKind {
        match self.body {
            ItemBody::File(_) => ItemKind::File,
            ItemBody::Env(_) => ItemKind::Env,
        }
    }

    /// The stage it is written at: an env item's is 1.
    pub fn stage(&self) -> Stage {
        match &self.body {
            ItemBody::File(f) => f.stage,
            ItemBody::Env(_) => Stage::One,
        }
    }

    /// What it writes: a file's base-relative path, or a variable.
    pub fn target(&self) -> &str {
        match &self.body {
            ItemBody::File(f) => &f.path,
            ItemBody::Env(e) => &e.variable,
        }
    }

    /// The size of what it carries: a file's decoded content, or an env
    /// value's bytes.
    pub fn size(&self) -> usize {
        match &self.body {
            ItemBody::File(f) => f.content.len(),
            ItemBody::Env(e) => e.value.len(),
        }
    }
}

/// The closed list of a problem's code.
#[derive(Debug, Clone, Copy, PartialEq, Eq, serde::Serialize)]
#[serde(rename_all = "snake_case")]
pub enum ProblemCode {
    /// The provider needs something the person has not set.
    MissingInput,
    /// An input exists but could not be read.
    Unreadable,
    /// The provider's tool is not signed in.
    SignedOut,
    /// A key has a passphrase, and keys with one are not delivered.
    PassphraseProtected,
    /// Two providers produced the same target, and neither was delivered.
    Conflict,
    /// A command the provider runs failed.
    CommandFailed,
    /// The provider ran out of the request's time.
    TimedOut,
    /// What the provider holds has itself expired.
    Expired,
}

impl ProblemCode {
    /// Every code, in the contract's order.
    pub const ALL: [ProblemCode; 8] = [
        ProblemCode::MissingInput,
        ProblemCode::Unreadable,
        ProblemCode::SignedOut,
        ProblemCode::PassphraseProtected,
        ProblemCode::Conflict,
        ProblemCode::CommandFailed,
        ProblemCode::TimedOut,
        ProblemCode::Expired,
    ];

    /// The code as the contract writes it.
    pub fn as_str(self) -> &'static str {
        match self {
            ProblemCode::MissingInput => "missing_input",
            ProblemCode::Unreadable => "unreadable",
            ProblemCode::SignedOut => "signed_out",
            ProblemCode::PassphraseProtected => "passphrase_protected",
            ProblemCode::Conflict => "conflict",
            ProblemCode::CommandFailed => "command_failed",
            ProblemCode::TimedOut => "timed_out",
            ProblemCode::Expired => "expired",
        }
    }

    /// The code written `s`, if there is one.
    pub fn from_name(s: &str) -> Option<ProblemCode> {
        ProblemCode::ALL.into_iter().find(|c| c.as_str() == s)
    }
}

/// Something a provider could not deliver, and why. The message is for
/// people, at most 256 printable bytes, and never holds a value.
#[derive(Debug, Clone, PartialEq, Eq, serde::Serialize)]
pub struct Problem {
    /// The provider, item-name alphabet.
    pub provider: String,
    /// What went wrong.
    pub code: ProblemCode,
    /// For people.
    pub message: String,
}

/// What a source sends for one request.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Answer {
    /// The id of the request it answers.
    pub id: String,
    /// At most 64 items.
    pub items: Vec<Item>,
    /// At most 16 problems.
    pub problems: Vec<Problem>,
}

impl Answer {
    /// The answer's wire form: compact JSON, each item with the members of
    /// its kind only, content as padded standard base64.
    pub fn to_json(&self) -> Vec<u8> {
        let mut out = String::with_capacity(
            256 + self
                .items
                .iter()
                .map(|i| i.size() * 4 / 3 + 200)
                .sum::<usize>(),
        );
        out.push_str("{\"version\":");
        out.push_str(&crate::limits::VERSION.to_string());
        out.push_str(",\"id\":");
        write_string(&mut out, &self.id);
        out.push_str(",\"items\":[");
        for (i, it) in self.items.iter().enumerate() {
            if i > 0 {
                out.push(',');
            }
            write_item(&mut out, it);
        }
        out.push_str("],\"problems\":[");
        for (i, p) in self.problems.iter().enumerate() {
            if i > 0 {
                out.push(',');
            }
            out.push_str("{\"provider\":");
            write_string(&mut out, &p.provider);
            out.push_str(",\"code\":");
            write_string(&mut out, p.code.as_str());
            out.push_str(",\"message\":");
            write_string(&mut out, &p.message);
            out.push('}');
        }
        out.push_str("]}");
        out.into_bytes()
    }

    /// Holds the answer to the contract against `request`: it is encoded
    /// and parsed as the server would parse it.
    pub fn validate(&self, request: &Request) -> Result<(), Refusal> {
        parse_answer(&self.to_json(), Some(request)).map(|_| ())
    }
}

fn write_item(out: &mut String, it: &Item) {
    let field = |out: &mut String, name: &str, first: bool| {
        if !first {
            out.push(',');
        }
        write_string(out, name);
        out.push(':');
    };
    out.push('{');
    field(out, "name", true);
    write_string(out, &it.name);
    field(out, "provider", false);
    write_string(out, &it.provider);
    field(out, "kind", false);
    write_string(out, it.kind().as_str());
    match &it.body {
        ItemBody::File(f) => {
            field(out, "stage", false);
            out.push_str(if f.stage == Stage::One { "1" } else { "2" });
            field(out, "base", false);
            write_string(out, f.base.as_str());
            field(out, "path", false);
            write_string(out, &f.path);
            field(out, "mode", false);
            write_string(out, f.mode.as_str());
            field(out, "content", false);
            write_string(
                out,
                &base64::engine::general_purpose::STANDARD.encode(&f.content),
            );
        }
        ItemBody::Env(e) => {
            field(out, "variable", false);
            write_string(out, &e.variable);
            field(out, "value", false);
            write_string(out, &e.value);
        }
    }
    field(out, "fingerprint", false);
    write_string(out, &it.fingerprint);
    if let Some(t) = &it.expires_at {
        field(out, "expires_at", false);
        write_string(out, t);
    }
    out.push('}');
}

const ANSWER_MEMBERS: &[&str] = &["version", "id", "items", "problems"];
const ANSWER_TYPES: &[(&str, Kind)] = &[
    ("version", Kind::Number),
    ("id", Kind::String),
    ("items", Kind::Array),
    ("problems", Kind::Array),
];
const ITEM_FILE: &[&str] = &["stage", "base", "path", "mode", "content"];
const ITEM_ENV: &[&str] = &["variable", "value"];
const ITEM_ALL: &[&str] = &[
    "name",
    "provider",
    "kind",
    "fingerprint",
    "expires_at",
    "stage",
    "base",
    "path",
    "mode",
    "content",
    "variable",
    "value",
];
const ITEM_COMMON_REQUIRED: &[&str] = &["name", "provider", "kind", "fingerprint"];
const ITEM_TYPES: &[(&str, Kind)] = &[
    ("name", Kind::String),
    ("provider", Kind::String),
    ("kind", Kind::String),
    ("fingerprint", Kind::String),
    ("expires_at", Kind::String),
    ("stage", Kind::Number),
    ("base", Kind::String),
    ("path", Kind::String),
    ("mode", Kind::String),
    ("content", Kind::String),
    ("variable", Kind::String),
    ("value", Kind::String),
];
const PROBLEM_MEMBERS: &[&str] = &["provider", "code", "message"];
const PROBLEM_TYPES: &[(&str, Kind)] = &[
    ("provider", Kind::String),
    ("code", Kind::String),
    ("message", Kind::String),
];

/// Decodes `body` as an answer to `request` and applies every rule of the
/// contract, in the contract's order, returning the answer or the first
/// rule broken. One bad item refuses the whole answer.
///
/// The answer is judged against its request: its id, and the shed's project
/// directory, which places every `base: project` item at an absolute path so
/// that the target rules (refused, duplicate and nested targets) see where
/// each file really lands, across both bases. With no request, the id must
/// still be well formed but matches nothing (`id_mismatch`).
pub fn parse_answer(body: &[u8], request: Option<&Request>) -> Result<Answer, Refusal> {
    let (request_id, project_dir) = match request {
        Some(r) => (r.id.as_str(), r.shed.project_dir.as_deref()),
        None => ("", None),
    };
    // The placement below is lexical, so it holds only for a canonical
    // absolute directory. parse_request checks that; a request built in
    // code is checked here, never trusted.
    if let Some(d) = project_dir {
        if !bounded_text(d, MAX_PROJECT_DIR_BYTES) || !canonical_absolute(d) {
            return Err(Refusal::new(Rule::BadValue, "shed.project_dir"));
        }
    }
    if body.len() > MAX_BODY_BYTES {
        return Err(Refusal::new(Rule::BodyTooLarge, ""));
    }
    let root = parse_document(body)?;
    check_members(&root, "", ANSWER_MEMBERS, ANSWER_MEMBERS)?;
    check_types(&root, "", ANSWER_TYPES)?;
    if root.get("version").map(|v| v.text.as_str()) != Some("2") {
        return Err(Refusal::new(Rule::UnsupportedVersion, "version"));
    }
    let id = root.get("id").map(|v| v.text.clone()).unwrap_or_default();
    if !valid_id(&id) {
        return Err(Refusal::new(Rule::BadId, "id"));
    }
    if id != request_id {
        return Err(Refusal::new(Rule::IdMismatch, "id"));
    }
    let item_nodes = &root.get("items").expect("checked").elems;
    let problem_nodes = &root.get("problems").expect("checked").elems;
    if item_nodes.len() > MAX_ITEMS {
        return Err(Refusal::new(Rule::TooManyItems, "items"));
    }
    if problem_nodes.len() > MAX_PROBLEMS {
        return Err(Refusal::new(Rule::TooManyProblems, "problems"));
    }
    let mut items = Vec::with_capacity(item_nodes.len());
    for (i, n) in item_nodes.iter().enumerate() {
        items.push(parse_item(n, &index("items", i), project_dir)?);
    }
    check_across(&items, project_dir)?;
    if env_file_size(&items) > MAX_ENV_FILE_BYTES {
        return Err(Refusal::new(Rule::EnvTooLarge, "items"));
    }
    let mut problems = Vec::with_capacity(problem_nodes.len());
    for (i, n) in problem_nodes.iter().enumerate() {
        problems.push(parse_problem(n, &index("problems", i))?);
    }
    Ok(Answer {
        id,
        items,
        problems,
    })
}

fn parse_item(n: &Node, at: &str, project_dir: Option<&str>) -> Result<Item, Refusal> {
    if n.kind != Kind::Object {
        return Err(Refusal::new(Rule::WrongType, at));
    }
    check_members(n, at, ITEM_ALL, &[])?;
    // The kind decides the rest.
    let kind = match n.get("kind") {
        None => return Err(Refusal::new(Rule::MissingMember, join(at, "kind"))),
        Some(k) if k.kind != Kind::String => {
            return Err(Refusal::new(Rule::WrongType, join(at, "kind")))
        }
        Some(k) if k.text == "file" => ItemKind::File,
        Some(k) if k.text == "env" => ItemKind::Env,
        Some(_) => return Err(Refusal::new(Rule::BadKind, join(at, "kind"))),
    };
    let (own, other) = match kind {
        ItemKind::File => (ITEM_FILE, ITEM_ENV),
        ItemKind::Env => (ITEM_ENV, ITEM_FILE),
    };
    for (name, _) in &n.members {
        if other.contains(&name.as_str()) {
            return Err(Refusal::new(Rule::WrongKindMember, join(at, name)));
        }
    }
    let required: Vec<&str> = ITEM_COMMON_REQUIRED
        .iter()
        .chain(own.iter())
        .copied()
        .collect();
    check_members(n, at, ITEM_ALL, &required)?;
    check_types(n, at, ITEM_TYPES)?;
    let s = |name: &str| n.get(name).map(|v| v.text.clone()).unwrap_or_default();

    let name = s("name");
    let provider = s("provider");
    if !valid_name(&name) {
        return Err(Refusal::new(Rule::BadName, join(at, "name")));
    }
    if !valid_name(&provider) {
        return Err(Refusal::new(Rule::BadProvider, join(at, "provider")));
    }
    let body = match kind {
        ItemKind::File => {
            let stage = match s("stage").as_str() {
                "1" => Stage::One,
                "2" => Stage::Two,
                _ => return Err(Refusal::new(Rule::BadStage, join(at, "stage"))),
            };
            let base = match s("base").as_str() {
                "home" => Base::Home,
                "project" => Base::Project,
                _ => return Err(Refusal::new(Rule::BadBase, join(at, "base"))),
            };
            if base == Base::Project && stage != Stage::Two {
                return Err(Refusal::new(Rule::ProjectBaseStage, join(at, "base")));
            }
            let path = s("path");
            if let Some(rule) = check_path(&path) {
                return Err(Refusal::new(rule, join(at, "path")));
            }
            if let Some(rel) =
                landing(base, &path, project_dir).strip_prefix(&format!("{HOME_DIR}/"))
            {
                if refused_home_target(rel) {
                    return Err(Refusal::new(Rule::RefusedTarget, join(at, "path")));
                }
            }
            let mode = Mode::from_name(&s("mode"))
                .ok_or_else(|| Refusal::new(Rule::BadMode, join(at, "mode")))?;
            let content = decode_content(&s("content"))
                .ok_or_else(|| Refusal::new(Rule::BadBase64, join(at, "content")))?;
            if content.len() > MAX_FILE_BYTES {
                return Err(Refusal::new(Rule::FileTooLarge, join(at, "content")));
            }
            ItemBody::File(FileItem {
                stage,
                base,
                path,
                mode,
                content,
            })
        }
        ItemKind::Env => {
            let variable = s("variable");
            if let Some(rule) = check_variable(&variable) {
                return Err(Refusal::new(rule, join(at, "variable")));
            }
            let value = s("value");
            if let Some(rule) = check_value(&value) {
                return Err(Refusal::new(rule, join(at, "value")));
            }
            ItemBody::Env(EnvItem { variable, value })
        }
    };
    let fingerprint = s("fingerprint");
    if !valid_fingerprint(&fingerprint) {
        return Err(Refusal::new(Rule::BadFingerprint, join(at, "fingerprint")));
    }
    let expires_at = n.get("expires_at").map(|v| v.text.clone());
    if let Some(t) = &expires_at {
        if !valid_time(t) {
            return Err(Refusal::new(Rule::BadTime, join(at, "expires_at")));
        }
    }
    Ok(Item {
        name,
        provider,
        body,
        fingerprint,
        expires_at,
    })
}

/// Decodes padded standard base64 with no line break and zero padding
/// bits.
pub(crate) fn decode_content(s: &str) -> Option<Vec<u8>> {
    if s.contains(['\r', '\n']) {
        return None;
    }
    base64::engine::general_purpose::STANDARD.decode(s).ok()
}

/// Where a file item lands: an absolute path, its base's directory joined
/// with its path. A `base: project` item of a request with no project
/// directory has no place; it is keyed in a space of its own
/// (`project:<path>`), compared only with other such items, and is never
/// written.
pub fn landing(base: Base, path: &str, project_dir: Option<&str>) -> String {
    match (base, project_dir) {
        (Base::Project, None) => format!("project:{path}"),
        (Base::Project, Some("/")) => format!("/{path}"),
        (Base::Project, Some(d)) => format!("{d}/{path}"),
        (Base::Home, _) => format!("{HOME_DIR}/{path}"),
    }
}

/// The key two items are compared by: where a file lands, or an env
/// item's variable.
pub(crate) fn target_key(it: &Item, project_dir: Option<&str>) -> String {
    match &it.body {
        ItemBody::Env(e) => format!("env:{}", e.variable),
        ItemBody::File(f) => landing(f.base, &f.path, project_dir),
    }
}

/// Whether one key is a directory prefix of the other.
pub(crate) fn nested(a: &str, b: &str) -> bool {
    let under =
        |x: &str, y: &str| x.len() > y.len() && x.starts_with(y) && x.as_bytes()[y.len()] == b'/';
    under(a, b) || under(b, a)
}

/// The rules that compare items, in order: a name used twice, a target
/// named twice, a file landing at a directory prefix of where another
/// lands, and the file contents together.
fn check_across(items: &[Item], project_dir: Option<&str>) -> Result<(), Refusal> {
    for j in 0..items.len() {
        for i in 0..j {
            if items[i].name == items[j].name {
                return Err(Refusal::new(
                    Rule::DuplicateName,
                    format!("{}.name", index("items", j)),
                ));
            }
        }
    }
    let keys: Vec<String> = items.iter().map(|it| target_key(it, project_dir)).collect();
    for j in 0..items.len() {
        for i in 0..j {
            if keys[i] == keys[j] {
                return Err(Refusal::new(Rule::DuplicateTarget, index("items", j)));
            }
        }
    }
    for j in 0..items.len() {
        for i in 0..j {
            if items[i].kind() == ItemKind::File
                && items[j].kind() == ItemKind::File
                && nested(&keys[i], &keys[j])
            {
                return Err(Refusal::new(Rule::NestedTarget, index("items", j)));
            }
        }
    }
    let total: usize = items
        .iter()
        .map(|it| match &it.body {
            ItemBody::File(f) => f.content.len(),
            ItemBody::Env(_) => 0,
        })
        .sum();
    if total > MAX_TOTAL_FILE_BYTES {
        return Err(Refusal::new(Rule::TotalTooLarge, "items"));
    }
    Ok(())
}

/// The size of the env file the env items among `items` are written as:
/// one `VARIABLE=value` line each, with its line feed.
pub fn env_file_size(items: &[Item]) -> usize {
    items
        .iter()
        .map(|it| match &it.body {
            ItemBody::Env(e) => e.variable.len() + 1 + e.value.len() + 1,
            ItemBody::File(_) => 0,
        })
        .sum()
}

/// Whether `m` is a valid problem message: at most 256 bytes, every
/// character printable (it may be empty).
pub fn valid_message(m: &str) -> bool {
    m.len() <= MAX_MESSAGE_BYTES && printable(m)
}

pub(crate) fn parse_problem(n: &Node, at: &str) -> Result<Problem, Refusal> {
    if n.kind != Kind::Object {
        return Err(Refusal::new(Rule::WrongType, at));
    }
    check_members(n, at, PROBLEM_MEMBERS, PROBLEM_MEMBERS)?;
    check_types(n, at, PROBLEM_TYPES)?;
    let s = |name: &str| n.get(name).map(|v| v.text.clone()).unwrap_or_default();
    let provider = s("provider");
    if !valid_name(&provider) {
        return Err(Refusal::new(Rule::BadProvider, join(at, "provider")));
    }
    let code = ProblemCode::from_name(&s("code"))
        .ok_or_else(|| Refusal::new(Rule::BadProblemCode, join(at, "code")))?;
    let message = s("message");
    if !valid_message(&message) {
        return Err(Refusal::new(Rule::BadProblemMessage, join(at, "message")));
    }
    Ok(Problem {
        provider,
        code,
        message,
    })
}
