//! The bundle builder: every provider's contribution assembled into one
//! answer that keeps the contract's rules (so the server never refuses what
//! the source sends), and a redacted preview of it.
//!
//! In order: each provider whose `only` matches runs; each item it drafts is
//! held to the item rules, and one that breaks a rule becomes a problem;
//! the git config fragments are merged into one `.config/git/config` item
//! that replaces the file whole; `base: project` items are left out when
//! the shed gets no stage 2 (no project directory, or a mounted host
//! directory); items with one target, one name, or targets one inside the
//! other are all left out, with a `conflict` problem for each provider
//! involved (a `files` entry on `.config/git/config` and the git config
//! item are one such pair); the limits on items, file content and env size
//! are kept by leaving out what does not fit, with a problem; each item is
//! fingerprinted; problems past sixteen are dropped. The answer is then
//! encoded and parsed against the request, as the server will.

use std::collections::HashMap;

use serde::Serialize;

use crate::answer::{
    landing, valid_message, Answer, Base, FileItem, Item, ItemBody, ItemKind, Mode, Problem,
    ProblemCode, Stage,
};
use crate::fingerprint::{fingerprint_body, FingerprintKey};
use crate::limits::{
    HOME_DIR, MAX_ENV_FILE_BYTES, MAX_FILE_BYTES, MAX_ITEMS, MAX_PROBLEMS, MAX_TOTAL_FILE_BYTES,
};
use crate::provider::{Context, Draft, GitSection, Provider};
use crate::reader::Reader;
use crate::request::Request;
use crate::rules::{Refusal, Rule};
use crate::text::{
    check_path, check_value, check_variable, refused_home_target, valid_name, valid_time,
};

/// The git config item's name and provider.
pub const GIT_CONFIG_NAME: &str = "git-config";
/// Where the git config item lands, under home.
pub const GIT_CONFIG_PATH: &str = ".config/git/config";

/// A built answer and its preview.
#[derive(Debug, Clone)]
pub struct Built {
    /// The answer to send.
    pub answer: Answer,
    /// What it holds, with no value.
    pub preview: Preview,
}

/// What a delivery holds, with no value and no content: what the desktop's
/// Preferences preview and its delivery log show.
#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
pub struct Preview {
    /// Each item: its name, provider, target, size and fingerprint.
    pub items: Vec<PreviewItem>,
    /// What could not be delivered.
    pub problems: Vec<Problem>,
    /// Each provider asked, and what came of it.
    pub providers: Vec<ProviderReport>,
}

/// One item, redacted.
#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
pub struct PreviewItem {
    /// The item's name.
    pub name: String,
    /// Its provider.
    pub provider: String,
    /// File or env.
    pub kind: ItemKind,
    /// The stage it is written at.
    pub stage: u8,
    /// A file's base.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub base: Option<Base>,
    /// A file's path, or an env item's variable.
    pub target: String,
    /// A file's mode.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub mode: Option<Mode>,
    /// Bytes of content or value.
    pub size: usize,
    /// The fingerprint.
    pub fingerprint: String,
    /// When it goes stale.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub expires_at: Option<String>,
}

impl PreviewItem {
    /// The redacted form of `item`.
    pub fn of(item: &Item) -> PreviewItem {
        let (base, mode) = match &item.body {
            ItemBody::File(f) => (Some(f.base), Some(f.mode)),
            ItemBody::Env(_) => (None, None),
        };
        PreviewItem {
            name: item.name.clone(),
            provider: item.provider.clone(),
            kind: item.kind(),
            stage: item.stage().number(),
            base,
            target: item.target().to_string(),
            mode,
            size: item.size(),
            fingerprint: item.fingerprint.clone(),
            expires_at: item.expires_at.clone(),
        }
    }
}

/// What became of one provider.
#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
pub struct ProviderReport {
    /// The provider.
    pub provider: String,
    /// Whether it ran: false when its `only` rules left this delivery out.
    pub ran: bool,
    /// Whether it has `only` rules (a preview does not judge them).
    pub restricted: bool,
}

/// Builds the answer to `request`, from the server named `server`, out of
/// `providers` (the enabled ones, in order), reading through `reader` and
/// fingerprinting under `key`. An error is a refusal of the built answer
/// by the contract's own rules, which the builder exists to prevent: a bug,
/// not a person's mistake.
pub fn build_answer(
    request: &Request,
    server: &str,
    providers: &[&dyn Provider],
    reader: &dyn Reader,
    key: &FingerprintKey,
) -> Result<Built, Refusal> {
    let ctx = Context::answer(server, request);
    let assembled = assemble(&ctx, providers, reader, key);
    let mut problems = assembled.problems;
    problems.truncate(MAX_PROBLEMS);
    let answer = Answer {
        id: request.id.clone(),
        items: assembled.items,
        problems,
    };
    answer.validate(request)?;
    let preview = Preview {
        items: answer.items.iter().map(PreviewItem::of).collect(),
        problems: answer.problems.clone(),
        providers: assembled.reports,
    };
    Ok(Built { answer, preview })
}

/// What `providers` would deliver, with no request: every `only` rule is let
/// through (the report says which providers have them), project items are
/// kept, and every problem is kept.
pub fn preview(providers: &[&dyn Provider], reader: &dyn Reader, key: &FingerprintKey) -> Preview {
    let assembled = assemble(&Context::preview(), providers, reader, key);
    Preview {
        items: assembled.items.iter().map(PreviewItem::of).collect(),
        problems: assembled.problems,
        providers: assembled.reports,
    }
}

struct Assembled {
    items: Vec<Item>,
    problems: Vec<Problem>,
    reports: Vec<ProviderReport>,
}

/// A drafted item and who made it: the provider, or for the git config
/// item, every provider whose fragment is in it.
struct Candidate {
    provider: String,
    draft: Draft,
    owners: Vec<String>,
}

fn problem(provider: &str, code: ProblemCode, message: &str) -> Problem {
    // A provider's message that breaks the message rule is replaced, never
    // cut: the rule exists so a message cannot carry what it should not.
    let message = if valid_message(message) {
        message.to_string()
    } else {
        "the provider's message was not printable text of at most 256 bytes".to_string()
    };
    Problem {
        provider: provider.to_string(),
        code,
        message,
    }
}

fn assemble(
    ctx: &Context<'_>,
    providers: &[&dyn Provider],
    reader: &dyn Reader,
    key: &FingerprintKey,
) -> Assembled {
    let request = ctx.request;
    let project_dir = request.and_then(|r| r.shed.project_dir.as_deref());
    let mut problems = Vec::new();
    let mut reports = Vec::new();
    let mut candidates: Vec<Candidate> = Vec::new();
    let mut git: Vec<(String, Vec<GitSection>)> = Vec::new();

    for p in providers {
        let name = p.name().to_string();
        let restricted = p.only().is_some();
        let ran = p.only().is_none_or(|o| ctx.allows(o));
        reports.push(ProviderReport {
            provider: name.clone(),
            ran,
            restricted,
        });
        if !ran {
            continue;
        }
        let c = p.provide(ctx, reader);
        for pd in &c.problems {
            problems.push(problem(&name, pd.code, &pd.message));
        }
        for d in c.items {
            match check_draft(&d, project_dir) {
                None => candidates.push(Candidate {
                    provider: name.clone(),
                    owners: vec![name.clone()],
                    draft: d,
                }),
                Some(rule) => {
                    problems.push(problem(&name, draft_code(rule), &draft_message(&d, rule)))
                }
            }
        }
        if !c.git_config.is_empty() {
            git.push((name, c.git_config));
        }
    }

    if !git.is_empty() {
        let text = render_git_config(git.iter().flat_map(|(_, s)| s.iter()));
        candidates.push(Candidate {
            provider: GIT_CONFIG_NAME.to_string(),
            owners: git.iter().map(|(n, _)| n.clone()).collect(),
            draft: Draft {
                name: GIT_CONFIG_NAME.to_string(),
                body: ItemBody::File(FileItem {
                    stage: Stage::One,
                    base: Base::Home,
                    path: GIT_CONFIG_PATH.to_string(),
                    mode: Mode::M0600,
                    content: text.into_bytes(),
                }),
                expires_at: None,
            },
        });
    }

    // A shed that gets no stage 2 is sent no project item.
    if let Some(r) = request {
        if r.stage2_dir().is_none() {
            candidates
                .retain(|c| !matches!(&c.draft.body, ItemBody::File(f) if f.base == Base::Project));
        }
    }

    // Conflicts: one target, one name, or one file inside another. Indexed,
    // so the work grows with the items' total path length rather than with
    // the square of their number (an env file may hold tens of thousands of
    // lines before any limit applies), and every conflict is still found.
    let keys: Vec<String> = candidates
        .iter()
        .map(|c| match &c.draft.body {
            ItemBody::Env(e) => format!("env:{}", e.variable),
            ItemBody::File(f) => landing(f.base, &f.path, project_dir),
        })
        .collect();
    let conflicted = conflicts(&candidates, &keys);
    let mut told: Vec<String> = Vec::new();
    for (c, _) in candidates.iter().zip(&conflicted).filter(|(_, x)| **x) {
        for owner in &c.owners {
            if !told.contains(owner) {
                told.push(owner.clone());
                problems.push(problem(
                    owner,
                    ProblemCode::Conflict,
                    "two items write the same target, or one inside the other; neither was delivered",
                ));
            }
        }
    }
    let candidates: Vec<Candidate> = candidates
        .into_iter()
        .zip(conflicted)
        .filter_map(|(c, x)| (!x).then_some(c))
        .collect();

    // The limits, in order: what does not fit is left out.
    let mut items: Vec<Item> = Vec::new();
    let (mut file_total, mut env_total) = (0usize, 0usize);
    for c in candidates {
        let (file_bytes, env_bytes) = match &c.draft.body {
            ItemBody::File(f) => (f.content.len(), 0),
            ItemBody::Env(e) => (0, e.variable.len() + 1 + e.value.len() + 1),
        };
        if items.len() >= MAX_ITEMS
            || file_total + file_bytes > MAX_TOTAL_FILE_BYTES
            || env_total + env_bytes > MAX_ENV_FILE_BYTES
        {
            for owner in &c.owners {
                problems.push(problem(
                    owner,
                    ProblemCode::Unreadable,
                    &format!(
                        "{} was left out: the answer is at its size limit",
                        c.draft.name
                    ),
                ));
            }
            continue;
        }
        file_total += file_bytes;
        env_total += env_bytes;
        items.push(Item {
            name: c.draft.name,
            provider: c.provider,
            fingerprint: fingerprint_body(key, &c.draft.body),
            body: c.draft.body,
            expires_at: c.draft.expires_at,
        });
    }
    Assembled {
        items,
        problems,
        reports,
    }
}

/// Which candidates conflict: a name or a target (`keys`) that two share,
/// or a file landing at a directory prefix of where another lands.
fn conflicts(candidates: &[Candidate], keys: &[String]) -> Vec<bool> {
    let mut conflicted = vec![false; candidates.len()];
    let mut by_name: HashMap<&str, Vec<usize>> = HashMap::new();
    let mut by_key: HashMap<&str, Vec<usize>> = HashMap::new();
    for (i, c) in candidates.iter().enumerate() {
        by_name.entry(c.draft.name.as_str()).or_default().push(i);
        by_key.entry(keys[i].as_str()).or_default().push(i);
    }
    for group in by_name.values().chain(by_key.values()) {
        if group.len() > 1 {
            for &i in group {
                conflicted[i] = true;
            }
        }
    }
    // A file is nested under another when one of its ancestors (each prefix
    // ending before a `/`) is another file's key. Env keys are never
    // ancestors: they hold no `/` and live in their own `env:` space.
    let files: HashMap<&str, &Vec<usize>> = by_key
        .iter()
        .filter(|(_, g)| matches!(candidates[g[0]].draft.body, ItemBody::File(_)))
        .map(|(k, g)| (*k, g))
        .collect();
    for (i, c) in candidates.iter().enumerate() {
        if !matches!(c.draft.body, ItemBody::File(_)) {
            continue;
        }
        let key = keys[i].as_str();
        for (cut, _) in key.match_indices('/') {
            if let Some(outer) = files.get(&key[..cut]) {
                conflicted[i] = true;
                for &j in outer.iter() {
                    conflicted[j] = true;
                }
            }
        }
    }
    conflicted
}

/// The item rule a draft breaks on its own, judged where it lands, or
/// `None`.
pub fn check_draft(d: &Draft, project_dir: Option<&str>) -> Option<Rule> {
    if !valid_name(&d.name) {
        return Some(Rule::BadName);
    }
    match &d.body {
        ItemBody::File(f) => {
            if f.base == Base::Project && f.stage != Stage::Two {
                return Some(Rule::ProjectBaseStage);
            }
            if let Some(rule) = check_path(&f.path) {
                return Some(rule);
            }
            if let Some(rel) =
                landing(f.base, &f.path, project_dir).strip_prefix(&format!("{HOME_DIR}/"))
            {
                if refused_home_target(rel) {
                    return Some(Rule::RefusedTarget);
                }
            }
            if f.content.len() > MAX_FILE_BYTES {
                return Some(Rule::FileTooLarge);
            }
        }
        ItemBody::Env(e) => {
            if let Some(rule) = check_variable(&e.variable).or_else(|| check_value(&e.value)) {
                return Some(rule);
            }
        }
    }
    if d.expires_at.as_deref().is_some_and(|t| !valid_time(t)) {
        return Some(Rule::BadTime);
    }
    None
}

fn draft_code(rule: Rule) -> ProblemCode {
    match rule {
        Rule::FileTooLarge
        | Rule::BadVariable
        | Rule::RefusedVariable
        | Rule::ValueEmpty
        | Rule::ValueTooLong
        | Rule::ValueNotPrintable
        | Rule::ValueEdgeSpace => ProblemCode::Unreadable,
        _ => ProblemCode::MissingInput,
    }
}

fn draft_message(d: &Draft, rule: Rule) -> String {
    // The draft's name is the provider's own text, checked before it is
    // shown: a name that breaks its rule is not repeated.
    if valid_name(&d.name) {
        format!(
            "{} breaks the contract's rule {}; it was not delivered",
            d.name, rule
        )
    } else {
        format!("an item breaks the contract's rule {rule}; it was not delivered")
    }
}

/// Renders git config sections as one file, merging sections with the same
/// name and subsection (entries in the order given).
pub fn render_git_config<'a>(sections: impl Iterator<Item = &'a GitSection>) -> String {
    let mut merged: Vec<GitSection> = Vec::new();
    for s in sections {
        match merged
            .iter_mut()
            .find(|m| m.name == s.name && m.subsection == s.subsection)
        {
            Some(m) => m.entries.extend(s.entries.iter().cloned()),
            None => merged.push(s.clone()),
        }
    }
    let mut out = String::from("# Written by shed delivery; replaced whole at every delivery.\n");
    for s in &merged {
        match &s.subsection {
            Some(sub) => {
                out.push('[');
                out.push_str(&s.name);
                out.push_str(" \"");
                for c in sub.chars() {
                    if c == '"' || c == '\\' {
                        out.push('\\');
                    }
                    out.push(c);
                }
                out.push_str("\"]\n");
            }
            None => {
                out.push('[');
                out.push_str(&s.name);
                out.push_str("]\n");
            }
        }
        for (k, v) in &s.entries {
            out.push('\t');
            out.push_str(k);
            if v.is_empty() {
                out.push_str(" =\n");
                continue;
            }
            out.push_str(" = ");
            out.push_str(&git_value(v));
            out.push('\n');
        }
    }
    out
}

/// A git config value, quoted when git would otherwise read it otherwise.
fn git_value(v: &str) -> String {
    let needs =
        v.starts_with(' ') || v.ends_with(' ') || v.contains(['#', ';', '"', '\\', '\n', '\t']);
    if !needs {
        return v.to_string();
    }
    let mut out = String::from("\"");
    for c in v.chars() {
        match c {
            '"' => out.push_str("\\\""),
            '\\' => out.push_str("\\\\"),
            '\n' => out.push_str("\\n"),
            '\t' => out.push_str("\\t"),
            c => out.push(c),
        }
    }
    out.push('"');
    out
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::answer::{env_file_size, EnvItem};
    use crate::only::Only;
    use crate::provider::{Contribution, ProblemDraft};
    use crate::request::parse_request;
    use crate::testing::FakeReader;

    /// A provider that contributes what it was made with.
    struct Toy {
        name: &'static str,
        only: Option<Only>,
        c: Contribution,
    }

    impl Provider for Toy {
        fn name(&self) -> &str {
            self.name
        }
        fn only(&self) -> Option<&Only> {
            self.only.as_ref()
        }
        fn provide(&self, _ctx: &Context<'_>, _reader: &dyn Reader) -> Contribution {
            self.c.clone()
        }
    }

    fn file(name: &str, base: Base, path: &str, size: usize) -> Draft {
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
                mode: Mode::M0600,
                content: vec![b'x'; size],
            }),
            expires_at: None,
        }
    }

    fn env(name: &str, variable: &str, value: &str) -> Draft {
        Draft {
            name: name.into(),
            body: ItemBody::Env(EnvItem {
                variable: variable.into(),
                value: value.into(),
            }),
            expires_at: None,
        }
    }

    fn toy(name: &'static str, items: Vec<Draft>) -> Toy {
        Toy {
            name,
            only: None,
            c: Contribution {
                items,
                ..Contribution::default()
            },
        }
    }

    fn request(extra: &str) -> Request {
        let body = format!(
            "{{\"version\":2,\"id\":\"0123456789abcdef0123456789abcdef\",\"reason\":\"create\",\"deadline\":\"2026-11-23T10:00:30Z\",\"user\":\"ada\",\"shed\":{{\"name\":\"widgets\",\"id\":\"q6c3\",\"backend\":\"firecracker\"{extra}}}}}"
        );
        parse_request(body.as_bytes()).unwrap()
    }

    fn repo_request() -> Request {
        request(",\"repo\":{\"url\":\"git@git.example.test:example/widgets.git\",\"name\":\"example/widgets\"},\"project_dir\":\"/home/shed/widgets\"")
    }

    fn key() -> FingerprintKey {
        FingerprintKey::from_bytes([7; 32])
    }

    fn build(providers: &[&dyn Provider], req: &Request) -> Built {
        build_answer(
            req,
            "work",
            providers,
            &FakeReader::new("/home/ada"),
            &key(),
        )
        .unwrap()
    }

    fn names(b: &Built) -> Vec<&str> {
        b.answer.items.iter().map(|i| i.name.as_str()).collect()
    }

    fn codes(b: &Built) -> Vec<(&str, ProblemCode)> {
        b.answer
            .problems
            .iter()
            .map(|p| (p.provider.as_str(), p.code))
            .collect()
    }

    #[test]
    fn git_fragments_merge_into_one_item() {
        let mut a = toy("a", vec![]);
        a.c.git_config = vec![GitSection::new("commit", &[("gpgsign", "true")])];
        let mut b = toy("b", vec![]);
        b.c.git_config = vec![
            GitSection::sub(
                "credential",
                "https://github.com",
                &[("helper", ""), ("helper", "!gh auth git-credential")],
            ),
            GitSection::new("commit", &[("verbose", "true")]),
        ];
        let built = build(&[&a, &b], &repo_request());
        assert_eq!(names(&built), vec![GIT_CONFIG_NAME]);
        let ItemBody::File(f) = &built.answer.items[0].body else {
            panic!()
        };
        assert_eq!(f.path, GIT_CONFIG_PATH);
        assert_eq!(f.mode, Mode::M0600);
        let text = String::from_utf8(f.content.clone()).unwrap();
        assert!(
            text.contains("[commit]\n\tgpgsign = true\n\tverbose = true\n"),
            "{text}"
        );
        assert!(text.contains("[credential \"https://github.com\"]\n\thelper =\n\thelper = !gh auth git-credential\n"), "{text}");
    }

    #[test]
    fn a_file_on_the_git_config_conflicts_and_neither_is_delivered() {
        let mut signer = toy(
            "ssh-key",
            vec![file("ssh-key", Base::Home, ".ssh/id_ed25519", 3)],
        );
        signer.c.git_config = vec![GitSection::new("commit", &[("gpgsign", "true")])];
        let files = toy(
            "files",
            vec![file("files.git", Base::Home, ".config/git/config", 3)],
        );
        let built = build(&[&signer, &files], &repo_request());
        assert_eq!(names(&built), vec!["ssh-key"]);
        assert_eq!(
            codes(&built),
            vec![
                ("files", ProblemCode::Conflict),
                ("ssh-key", ProblemCode::Conflict)
            ]
        );
        // A project item landing there is the same target.
        let above = request(",\"project_dir\":\"/home/shed/.config/git\"");
        let files = toy("files", vec![file("files.git", Base::Project, "config", 3)]);
        let built = build(&[&signer, &files], &above);
        assert_eq!(names(&built), vec!["ssh-key"]);
    }

    #[test]
    fn duplicate_nested_and_named_twice_are_all_left_out() {
        let a = toy(
            "a",
            vec![
                file("a.one", Base::Home, "widgets/.env", 1),
                file("a.two", Base::Home, "x", 1),
                env("a.v", "TOKEN", "1"),
                file("a.keep", Base::Home, "ab/c", 1),
            ],
        );
        let b = toy(
            "b",
            vec![
                file("b.one", Base::Project, ".env", 1),
                file("b.two", Base::Home, "x/y", 1),
                env("b.v", "TOKEN", "2"),
                file("a.keep", Base::Home, "other", 1),
                file("b.keep", Base::Home, "a", 1),
            ],
        );
        let built = build(&[&a, &b], &repo_request());
        assert_eq!(names(&built), vec!["b.keep"]);
        assert_eq!(
            codes(&built),
            vec![("a", ProblemCode::Conflict), ("b", ProblemCode::Conflict)]
        );
    }

    #[test]
    fn project_items_go_only_where_stage_2_is_written() {
        let p = toy(
            "files",
            vec![
                file("files.env", Base::Project, ".env", 1),
                file("files.rc", Base::Home, ".rc", 1),
            ],
        );
        assert_eq!(
            names(&build(&[&p], &repo_request())),
            vec!["files.env", "files.rc"]
        );
        assert_eq!(names(&build(&[&p], &request(""))), vec!["files.rc"]);
        let local = request(",\"local_dir\":\"widgets\",\"project_dir\":\"/home/shed/widgets\"");
        assert_eq!(names(&build(&[&p], &local)), vec!["files.rc"]);
        // A preview keeps them.
        let pv = preview(&[&p], &FakeReader::new("/home/ada"), &key());
        assert_eq!(pv.items.len(), 2);
    }

    #[test]
    fn drafts_breaking_a_rule_become_problems() {
        let p = toy(
            "p",
            vec![
                file("p.keys", Base::Home, ".ssh/authorized_keys", 1),
                file("p.dot", Base::Home, ".ssh/./authorized_keys", 1),
                file("p.big", Base::Home, "big", MAX_FILE_BYTES + 1),
                env("p.path", "PATH", "/x"),
                env("p.space", "OK", "trailing "),
                env("Bad Name", "OK2", "v"),
                env("p.good", "GOOD", "a \"$b\" c"),
            ],
        );
        let built = build(&[&p], &repo_request());
        assert_eq!(names(&built), vec!["p.good"]);
        let msgs: Vec<&str> = built
            .answer
            .problems
            .iter()
            .map(|p| p.message.as_str())
            .collect();
        assert!(msgs[0].contains("refused_target"), "{msgs:?}");
        assert!(msgs[1].contains("path_dot_segment"));
        assert_eq!(built.answer.problems[2].code, ProblemCode::Unreadable);
        assert!(msgs[3].contains("refused_variable"));
        assert!(msgs[4].contains("value_edge_space"));
        assert!(!msgs[5].contains("Bad Name"));
        assert!(msgs.iter().all(|m| !m.contains("trailing")));
    }

    #[test]
    fn many_items_are_checked_in_linear_time() {
        // A 256 KiB env file of `A=1` lines: 65,536 drafts with one name and
        // one target. All conflict; none is delivered; it returns at once.
        let start = std::time::Instant::now();
        let same: Vec<Draft> = (0..65_536).map(|_| env("p.a", "A", "1")).collect();
        let built = build(&[&toy("p", same)], &repo_request());
        assert!(built.answer.items.is_empty());
        assert_eq!(codes(&built), vec![("p", ProblemCode::Conflict)]);
        // Distinct names and targets, one nested pair at the very end.
        let mut many: Vec<Draft> = (0..65_536)
            .map(|i| file(&format!("p.f{i}"), Base::Home, &format!("d{i}/f"), 1))
            .collect();
        many.push(file("p.outer", Base::Home, "d65535", 1));
        let built = build(&[&toy("p", many)], &repo_request());
        assert_eq!(built.answer.items.len(), MAX_ITEMS);
        assert!(built
            .answer
            .items
            .iter()
            .all(|i| i.name != "p.f65535" && i.name != "p.outer"));
        assert_eq!(built.answer.problems[0].code, ProblemCode::Conflict);
        assert!(
            start.elapsed() < std::time::Duration::from_secs(20),
            "{:?}",
            start.elapsed()
        );
    }

    #[test]
    fn nesting_is_found_at_every_depth_and_across_bases() {
        let p = toy(
            "p",
            vec![
                file("p.deep", Base::Home, "a/b/c/d", 1),
                file("p.mid", Base::Home, "a/b", 1),
                file("p.sib", Base::Home, "a/bc", 1),
                file("p.proj", Base::Project, "x/y", 1),
                file("p.home", Base::Home, "widgets/x", 1),
                env("p.env", "A", "1"),
            ],
        );
        let built = build(&[&p], &repo_request());
        assert_eq!(names(&built), vec!["p.sib", "p.env"]);
    }

    #[test]
    fn limits_leave_out_what_does_not_fit() {
        let many: Vec<Draft> = (0..90)
            .map(|i| env(&format!("p.v{i}"), &format!("V{i}"), "x"))
            .collect();
        let built = build(&[&toy("p", many)], &repo_request());
        assert_eq!(built.answer.items.len(), MAX_ITEMS);
        assert_eq!(built.answer.problems.len(), MAX_PROBLEMS);
        let big: Vec<Draft> = (0..5)
            .map(|i| {
                file(
                    &format!("p.f{i}"),
                    Base::Home,
                    &format!("f{i}"),
                    MAX_FILE_BYTES,
                )
            })
            .collect();
        let built = build(&[&toy("p", big)], &repo_request());
        assert_eq!(built.answer.items.len(), 4);
        assert_eq!(built.answer.problems.len(), 1);
        let wide: Vec<Draft> = (0..40)
            .map(|i| env(&format!("p.w{i}"), &format!("W{i}"), &"y".repeat(8192)))
            .collect();
        let built = build(&[&toy("p", wide)], &repo_request());
        assert!(env_file_size(&built.answer.items) <= MAX_ENV_FILE_BYTES);
        assert_eq!(built.answer.items.len(), 31);
    }

    #[test]
    fn only_rules_skip_a_provider_and_the_preview_says_so() {
        let mut p = toy("p", vec![env("p.v", "V", "x")]);
        p.only = Some(Only {
            servers: Some(vec!["home".into()]),
            ..Only::default()
        });
        let built = build(&[&p], &repo_request());
        assert!(built.answer.items.is_empty());
        assert_eq!(
            built.preview.providers,
            vec![ProviderReport {
                provider: "p".into(),
                ran: false,
                restricted: true
            }]
        );
        let pv = preview(&[&p], &FakeReader::new("/home/ada"), &key());
        assert_eq!(pv.items.len(), 1);
        assert!(pv.providers[0].ran && pv.providers[0].restricted);
    }

    #[test]
    fn problems_keep_the_message_rule_and_fingerprints_are_stable() {
        let mut p = toy("p", vec![env("p.v", "V", "sentinel-value")]);
        p.c.problems = vec![
            ProblemDraft::new(ProblemCode::CommandFailed, "fine"),
            ProblemDraft::new(ProblemCode::CommandFailed, "bad\nline"),
        ];
        let built = build(&[&p], &repo_request());
        assert_eq!(built.answer.problems[0].message, "fine");
        assert!(!built.answer.problems[1].message.contains("line"));
        let again = build(&[&p], &repo_request());
        assert_eq!(
            built.answer.items[0].fingerprint,
            again.answer.items[0].fingerprint
        );
        let other = build_answer(
            &repo_request(),
            "work",
            &[&p],
            &FakeReader::new("/h"),
            &FingerprintKey::from_bytes([8; 32]),
        )
        .unwrap();
        assert_ne!(
            built.answer.items[0].fingerprint,
            other.answer.items[0].fingerprint
        );
        assert_eq!(built.preview.items[0].size, "sentinel-value".len());
        assert_eq!(built.preview.items[0].target, "V");
    }
}
