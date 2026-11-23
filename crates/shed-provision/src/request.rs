//! The request (`CONTRACT.md`, section 1): what the server sends a source.

use crate::json::{check_members, check_types, parse_document, Kind, Node};
use crate::limits::{
    MAX_BODY_BYTES, MAX_LOCAL_DIR_BYTES, MAX_NAME_BYTES, MAX_PROJECT_DIR_BYTES,
    MAX_REPO_NAME_BYTES, MAX_URL_BYTES,
};
use crate::rules::{Refusal, Rule};
use crate::text::{bounded_text, canonical_absolute, valid_id, valid_time};

/// Why a request was made.
#[derive(Debug, Clone, Copy, PartialEq, Eq, serde::Serialize)]
#[serde(rename_all = "snake_case")]
pub enum Reason {
    /// The shed was created.
    Create,
    /// A person ran `shed provision` on it.
    Provision,
}

impl Reason {
    /// The reason as the contract writes it.
    pub fn as_str(self) -> &'static str {
        match self {
            Reason::Create => "create",
            Reason::Provision => "provision",
        }
    }

    fn parse(s: &str) -> Option<Reason> {
        match s {
            "create" => Some(Reason::Create),
            "provision" => Some(Reason::Provision),
            _ => None,
        }
    }
}

/// The backend a shed runs on.
#[derive(Debug, Clone, Copy, PartialEq, Eq, serde::Serialize, serde::Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum Backend {
    /// Firecracker microVMs.
    Firecracker,
    /// Apple VZ.
    Vz,
    /// A cloud VM.
    Remote,
}

impl Backend {
    /// The backend as the contract writes it.
    pub fn as_str(self) -> &'static str {
        match self {
            Backend::Firecracker => "firecracker",
            Backend::Vz => "vz",
            Backend::Remote => "remote",
        }
    }

    /// The backend named `s`, if there is one.
    pub fn from_name(s: &str) -> Option<Backend> {
        match s {
            "firecracker" => Some(Backend::Firecracker),
            "vz" => Some(Backend::Vz),
            "remote" => Some(Backend::Remote),
            _ => None,
        }
    }
}

/// What the server sends a source: one shed, created or provisioned,
/// wanting its items.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Request {
    /// 32 lowercase hex digits, one per request.
    pub id: String,
    /// Why it was made.
    pub reason: Reason,
    /// The time after which the server takes no answer.
    pub deadline: String,
    /// The shed's user, or empty for the implicit user of a server with no
    /// user list.
    pub user: String,
    /// The shed.
    pub shed: RequestShed,
}

/// The shed a request is for.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct RequestShed {
    /// The shed's name.
    pub name: String,
    /// The shed's permanent id.
    pub id: String,
    /// The backend it runs on.
    pub backend: Backend,
    /// Its repository, when it has one.
    pub repo: Option<RequestRepo>,
    /// The base name of the host directory mounted as its project
    /// directory, when it has one.
    pub local_dir: Option<String>,
    /// The absolute, canonical path of its project directory, when it has
    /// one: where `base: project` items land.
    pub project_dir: Option<String>,
}

/// A shed's repository.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct RequestRepo {
    /// The URL it is cloned from.
    pub url: String,
    /// The `owner/name` the URL parses to, when it parses to one.
    pub name: Option<String>,
}

const REQUEST_MEMBERS: &[&str] = &["version", "id", "reason", "deadline", "user", "shed"];
const REQUEST_TYPES: &[(&str, Kind)] = &[
    ("version", Kind::Number),
    ("id", Kind::String),
    ("reason", Kind::String),
    ("deadline", Kind::String),
    ("user", Kind::String),
    ("shed", Kind::Object),
];
const SHED_MEMBERS: &[&str] = &["name", "id", "backend", "repo", "local_dir", "project_dir"];
const SHED_REQUIRED: &[&str] = &["name", "id", "backend"];
const SHED_TYPES: &[(&str, Kind)] = &[
    ("name", Kind::String),
    ("id", Kind::String),
    ("backend", Kind::String),
    ("repo", Kind::Object),
    ("local_dir", Kind::String),
    ("project_dir", Kind::String),
];
const REPO_MEMBERS: &[&str] = &["url", "name"];
const REPO_TYPES: &[(&str, Kind)] = &[("url", Kind::String), ("name", Kind::String)];

fn text_of(n: &Node, name: &str) -> String {
    n.get(name).map(|v| v.text.clone()).unwrap_or_default()
}

/// Decodes `body` as a request under the contract's conventions and rules.
/// A source does not answer a request it cannot parse; a value that breaks
/// its rule is refused under [`Rule::BadValue`] (request rules do not
/// travel).
pub fn parse_request(body: &[u8]) -> Result<Request, Refusal> {
    if body.len() > MAX_BODY_BYTES {
        return Err(Refusal::new(Rule::BodyTooLarge, ""));
    }
    let root = parse_document(body)?;
    check_members(&root, "", REQUEST_MEMBERS, REQUEST_MEMBERS)?;
    check_types(&root, "", REQUEST_TYPES)?;
    if text_of(&root, "version") != "1" {
        return Err(Refusal::new(Rule::UnsupportedVersion, "version"));
    }
    let id = text_of(&root, "id");
    if !valid_id(&id) {
        return Err(Refusal::new(Rule::BadId, "id"));
    }
    let reason = Reason::parse(&text_of(&root, "reason"))
        .ok_or_else(|| Refusal::new(Rule::BadValue, "reason"))?;
    let deadline = text_of(&root, "deadline");
    if !valid_time(&deadline) {
        return Err(Refusal::new(Rule::BadTime, "deadline"));
    }
    let user = text_of(&root, "user");
    if !user.is_empty() && !bounded_text(&user, MAX_NAME_BYTES) {
        return Err(Refusal::new(Rule::BadValue, "user"));
    }

    let sn = root.get("shed").expect("checked present");
    check_members(sn, "shed", SHED_MEMBERS, SHED_REQUIRED)?;
    check_types(sn, "shed", SHED_TYPES)?;
    let name = text_of(sn, "name");
    if !bounded_text(&name, MAX_NAME_BYTES) {
        return Err(Refusal::new(Rule::BadValue, "shed.name"));
    }
    let shed_id = text_of(sn, "id");
    if shed_id.is_empty()
        || shed_id.len() > MAX_NAME_BYTES
        || !shed_id
            .bytes()
            .all(|c| c.is_ascii_lowercase() || c.is_ascii_digit())
    {
        return Err(Refusal::new(Rule::BadValue, "shed.id"));
    }
    let backend = Backend::from_name(&text_of(sn, "backend"))
        .ok_or_else(|| Refusal::new(Rule::BadValue, "shed.backend"))?;
    let mut repo = None;
    if let Some(rn) = sn.get("repo") {
        check_members(rn, "shed.repo", REPO_MEMBERS, &["url"])?;
        check_types(rn, "shed.repo", REPO_TYPES)?;
        let url = text_of(rn, "url");
        if !bounded_text(&url, MAX_URL_BYTES) {
            return Err(Refusal::new(Rule::BadValue, "shed.repo.url"));
        }
        let name = rn.get("name").map(|n| n.text.clone());
        if let Some(n) = &name {
            if !repo_name(n) {
                return Err(Refusal::new(Rule::BadValue, "shed.repo.name"));
            }
        }
        repo = Some(RequestRepo { url, name });
    }
    let local_dir = sn.get("local_dir").map(|n| n.text.clone());
    if let Some(d) = &local_dir {
        if !bounded_text(d, MAX_LOCAL_DIR_BYTES) || d.contains('/') || d == "." || d == ".." {
            return Err(Refusal::new(Rule::BadValue, "shed.local_dir"));
        }
    }
    let project_dir = sn.get("project_dir").map(|n| n.text.clone());
    if let Some(d) = &project_dir {
        if !bounded_text(d, MAX_PROJECT_DIR_BYTES) || !canonical_absolute(d) {
            return Err(Refusal::new(Rule::BadValue, "shed.project_dir"));
        }
    }
    Ok(Request {
        id,
        reason,
        deadline,
        user,
        shed: RequestShed {
            name,
            id: shed_id,
            backend,
            repo,
            local_dir,
            project_dir,
        },
    })
}

/// Whether `s` is `owner/name`: two non-empty parts, one slash, at most 255
/// bytes, no control character.
fn repo_name(s: &str) -> bool {
    match s.split_once('/') {
        Some((owner, name)) => {
            !owner.is_empty()
                && !name.is_empty()
                && !name.contains('/')
                && bounded_text(s, MAX_REPO_NAME_BYTES)
        }
        None => false,
    }
}

impl Request {
    /// The project directory items of `base: project` land under, when the
    /// shed has one and the server writes stage 2 into it: a shed whose
    /// project directory is a mounted host directory (`local_dir`) gets no
    /// stage 2.
    pub fn stage2_dir(&self) -> Option<&str> {
        if self.shed.local_dir.is_some() {
            return None;
        }
        self.shed.project_dir.as_deref()
    }

    /// The `owner/name` of the shed's repository, when it has one.
    pub fn repo_name(&self) -> Option<&str> {
        self.shed.repo.as_ref().and_then(|r| r.name.as_deref())
    }
}
