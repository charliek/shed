//! The contract reader: `tests/provision-contract/v2/`, read as a copy of
//! the contract would be. Every answer fixture (accept and refuse, each
//! judged against the request fixture it names), every request, every
//! record (accept and refuse), every stream transcript, and the manifest —
//! each with the outcome and rule the fixture expects. The fixture language
//! is the one `CONTRACT.md` ("The fixtures") defines, held strictly: a
//! member outside it fails the fixture.

use std::collections::{BTreeMap, HashSet};
use std::fs;
use std::path::{Path, PathBuf};

use base64::Engine as _;
use sha2::{Digest, Sha256};

use crate::answer::parse_answer;
use crate::json::{parse_with, Kind, Node};
use crate::record::{parse_record, State};
use crate::request::{parse_request, Request};
use crate::rules::{Rule, ANSWER_RULES, RECORD_RULES};
use crate::stream::{read_event, EventReader, StreamItem};
use crate::text::valid_id;

/// Every answer fixture answers this request.
const FIXTURE_REQUEST_ID: &str = "0123456789abcdef0123456789abcdef";

/// The fixture wrapper is a document of its own, one level above the
/// answer it carries: it may nest deeper than the contract's limit.
const FIXTURE_DEPTH: usize = 64;

fn v2() -> PathBuf {
    Path::new(env!("CARGO_MANIFEST_DIR")).join("../../tests/provision-contract/v2")
}

/// The `.json` files directly under `v2/<dir>`, by path under `v2/`;
/// failing when there are none (a check over an empty folder would pass
/// vacuously).
fn json_files(dir: &str) -> Vec<String> {
    let path = v2().join(dir);
    let mut out: Vec<String> = fs::read_dir(&path)
        .unwrap_or_else(|e| {
            panic!(
                "list {}: {e} (is tests/provision-contract/ present?)",
                path.display()
            )
        })
        .map(|e| e.expect("dir entry"))
        .filter(|e| e.file_type().expect("file type").is_file())
        .map(|e| e.file_name().into_string().expect("utf-8 name"))
        .filter(|n| n.ends_with(".json"))
        .map(|n| format!("{dir}/{n}"))
        .collect();
    out.sort();
    assert!(!out.is_empty(), "no .json files in {dir}");
    out
}

fn stem(rel: &str) -> &str {
    rel.rsplit('/').next().unwrap().trim_end_matches(".json")
}

/// A fixture file: its text and its parsed tree.
struct FixtureDoc {
    text: Vec<u8>,
    root: Node,
}

impl FixtureDoc {
    fn load(rel: &str, allowed: &[&str]) -> FixtureDoc {
        let text = fs::read(v2().join(rel)).unwrap_or_else(|e| panic!("read {rel}: {e}"));
        let (root, first) =
            parse_with(&text, FIXTURE_DEPTH).unwrap_or_else(|()| panic!("{rel}: not JSON"));
        assert!(first.is_none(), "{rel}: {first:?}");
        assert_eq!(root.kind, Kind::Object, "{rel}: not an object");
        strict(&root, allowed, rel);
        let d = FixtureDoc { text, root };
        assert_eq!(d.str("name"), stem(rel), "{rel}: name is not the file's");
        assert!(!d.str("description").is_empty(), "{rel}: no description");
        d
    }

    fn get(&self, name: &str) -> Option<&Node> {
        self.root.get(name)
    }

    fn str(&self, name: &str) -> String {
        match self.get(name) {
            Some(n) if n.kind == Kind::String => n.text.clone(),
            other => panic!("{name}: not a string: {other:?}"),
        }
    }

    /// The exact bytes of member `name`'s value as the file writes it.
    fn raw(&self, name: &str) -> Vec<u8> {
        let n = self.get(name).unwrap_or_else(|| panic!("no {name}"));
        self.text[n.span.0..n.span.1].to_vec()
    }
}

/// `n` has no member outside `allowed` (the fixture language).
fn strict(n: &Node, allowed: &[&str], what: &str) {
    for (m, _) in &n.members {
        assert!(
            allowed.contains(&m.as_str()),
            "{what}: member {m:?} is not in the fixture language"
        );
    }
}

fn number(n: &Node, what: &str) -> usize {
    assert_eq!(n.kind, Kind::Number, "{what}: not a number");
    n.text
        .parse()
        .unwrap_or_else(|_| panic!("{what}: not a whole number"))
}

/// One answer fixture, checked against its language.
struct AnswerFixture {
    rel: String,
    doc: FixtureDoc,
    request: String,
    result: String,
    status: Option<usize>,
    rule: Option<String>,
}

impl AnswerFixture {
    fn load(rel: &str) -> AnswerFixture {
        let doc = FixtureDoc::load(
            rel,
            &[
                "name",
                "description",
                "request",
                "answer",
                "answer_text",
                "answer_base64",
                "fill",
                "expect",
            ],
        );
        let forms = ["answer", "answer_text", "answer_base64"]
            .iter()
            .filter(|m| doc.get(m).is_some())
            .count();
        assert_eq!(
            forms, 1,
            "{rel}: exactly one of answer, answer_text and answer_base64"
        );
        if doc.get("fill").is_some() {
            assert!(
                doc.get("answer").is_some(),
                "{rel}: fill goes with answer only"
            );
        }
        let expect = doc
            .get("expect")
            .unwrap_or_else(|| panic!("{rel}: no expect"));
        strict(expect, &["result", "status", "rule"], rel);
        let result = expect.get("result").expect("expect.result").text.clone();
        let status = expect.get("status").map(|n| number(n, "expect.status"));
        let rule = expect.get("rule").map(|n| n.text.clone());
        match result.as_str() {
            "accepted" => assert!(
                status.is_none() && rule.is_none(),
                "{rel}: an accepted fixture names no status or rule"
            ),
            "refused" => assert!(
                status.is_some() && rule.is_some(),
                "{rel}: a refused fixture names its status and rule"
            ),
            other => panic!("{rel}: expect.result {other:?}"),
        }
        let request = doc.str("request");
        assert!(!request.is_empty(), "{rel}: no request");
        AnswerFixture {
            rel: rel.to_string(),
            doc,
            request,
            result,
            status,
            rule,
        }
    }

    /// The bytes a source posts for `request_id`: the answer with its fill
    /// applied, the fixtures' request id replaced by `request_id`.
    fn body(&self, request_id: &str) -> Vec<u8> {
        let mut body = if self.doc.get("answer_text").is_some() {
            self.doc.str("answer_text").into_bytes()
        } else if self.doc.get("answer_base64").is_some() {
            base64::engine::general_purpose::STANDARD
                .decode(self.doc.str("answer_base64"))
                .unwrap_or_else(|e| panic!("{}: answer_base64: {e}", self.rel))
        } else if let Some(fill) = self.doc.get("fill") {
            self.filled(fill)
        } else {
            self.doc.raw("answer")
        };
        if request_id != FIXTURE_REQUEST_ID {
            body = replace_all(&body, FIXTURE_REQUEST_ID.as_bytes(), request_id.as_bytes());
        }
        body
    }

    /// The answer with each `fill` applied: `items[item].content` set to the
    /// padded standard base64 of `bytes` bytes, each `a`.
    fn filled(&self, fill: &Node) -> Vec<u8> {
        let mut answer = self.doc.get("answer").expect("answer").clone();
        assert_eq!(fill.kind, Kind::Array, "{}: fill is not a list", self.rel);
        for f in &fill.elems {
            strict(f, &["item", "bytes"], &self.rel);
            let i = number(f.get("item").expect("fill.item"), "fill.item");
            let n = number(f.get("bytes").expect("fill.bytes"), "fill.bytes");
            let items = answer.get_mut("items").expect("the answer has items");
            let item = items
                .elems
                .get_mut(i)
                .unwrap_or_else(|| panic!("{}: fill item {i}", self.rel));
            assert_eq!(
                item.kind,
                Kind::Object,
                "{}: fill item {i} is not an object",
                self.rel
            );
            let content = base64::engine::general_purpose::STANDARD.encode(vec![b'a'; n]);
            match item.get_mut("content") {
                Some(c) => *c = Node::string(&content),
                None => item
                    .members
                    .push(("content".into(), Node::string(&content))),
            }
        }
        let mut out = String::new();
        answer.write(&mut out);
        out.into_bytes()
    }

    /// The request fixture this answer is judged against; its id must be the
    /// fixtures' request id.
    fn request(&self) -> Request {
        let req = load_request(&format!("fixtures/requests/{}.json", self.request));
        assert_eq!(
            req.id, FIXTURE_REQUEST_ID,
            "{}: request {} has another id",
            self.rel, self.request
        );
        req
    }
}

fn replace_all(hay: &[u8], from: &[u8], to: &[u8]) -> Vec<u8> {
    let mut out = Vec::with_capacity(hay.len());
    let mut i = 0;
    while i < hay.len() {
        if hay[i..].starts_with(from) {
            out.extend_from_slice(to);
            i += from.len();
        } else {
            out.push(hay[i]);
            i += 1;
        }
    }
    out
}

fn load_request(rel: &str) -> Request {
    let doc = FixtureDoc::load(rel, &["name", "description", "request"]);
    parse_request(&doc.raw("request")).unwrap_or_else(|r| panic!("{rel}: request refused: {r}"))
}

/// A refusal's location is member names and indexes, or nothing:
/// `^$|^([a-z0-9_]+|\*)(\[[0-9]+\])?(\.([a-z0-9_]+|\*)(\[[0-9]+\])?)*$`.
fn location_is_names(at: &str) -> bool {
    at.is_empty()
        || at.split('.').all(|seg| {
            let (name, idx) = match seg.find('[') {
                Some(i) => (&seg[..i], Some(&seg[i..])),
                None => (seg, None),
            };
            let name_ok = name == "*"
                || (!name.is_empty()
                    && name
                        .bytes()
                        .all(|c| c.is_ascii_lowercase() || c.is_ascii_digit() || c == b'_'));
            let idx_ok = idx.is_none_or(|s| {
                s.len() > 2
                    && s.ends_with(']')
                    && s[1..s.len() - 1].bytes().all(|c| c.is_ascii_digit())
            });
            name_ok && idx_ok
        })
}

fn outcome(body: &[u8], req: &Request) -> Result<(), crate::rules::Refusal> {
    parse_answer(body, Some(req)).map(|_| ())
}

/// Fails with every message in `failures`, one per line: a fixture run
/// reports each fixture it disagrees with, not only the first.
fn report(failures: &[String]) {
    assert!(
        failures.is_empty(),
        "{} fixture check(s) failed:\n{}",
        failures.len(),
        failures.join("\n")
    );
}

#[test]
fn contract_answer_accept_fixtures() {
    let mut failures = Vec::new();
    for rel in json_files("fixtures/answers/accept") {
        let f = AnswerFixture::load(&rel);
        assert_eq!(f.result, "accepted", "{rel}: expect.result in accept/");
        let req = f.request();
        let a = match parse_answer(&f.body(FIXTURE_REQUEST_ID), Some(&req)) {
            Ok(a) => a,
            Err(r) => {
                failures.push(format!(
                    "{rel}: refused under {} ({r}); want accepted",
                    r.rule
                ));
                continue;
            }
        };
        // Its own encoding is accepted and decodes to the same answer.
        match parse_answer(&a.to_json(), Some(&req)) {
            Ok(again) if again == a => {}
            Ok(_) => failures.push(format!("{rel}: round trip changed the answer")),
            Err(r) => failures.push(format!("{rel}: its own encoding is refused: {r}")),
        }
    }
    report(&failures);
}

#[test]
fn contract_answer_refuse_fixtures() {
    let mut covered = HashSet::new();
    let mut failures = Vec::new();
    for rel in json_files("fixtures/answers/refuse") {
        let f = AnswerFixture::load(&rel);
        assert_eq!(f.result, "refused", "{rel}: expect.result in refuse/");
        let name = f.rule.clone().unwrap();
        let rule = Rule::from_name(&name)
            .filter(|r| r.is_answer_rule())
            .unwrap_or_else(|| panic!("{rel}: expect.rule {name:?} is not an answer rule"));
        covered.insert(rule);
        assert_eq!(
            f.status,
            Some(usize::from(rule.status())),
            "{rel}: status is not {rule}'s"
        );
        match parse_answer(&f.body(FIXTURE_REQUEST_ID), Some(&f.request())) {
            Ok(_) => failures.push(format!("{rel}: accepted; want refused under {rule}")),
            Err(r) if r.rule != rule => failures.push(format!(
                "{rel}: refused under {} ({r}); want {rule}",
                r.rule
            )),
            Err(r) if !location_is_names(&r.at) => failures.push(format!(
                "{rel}: location {:?} is not member names and indexes",
                r.at
            )),
            Err(_) => {}
        }
    }
    for rule in ANSWER_RULES {
        // body_too_large is generated (contract_body_too_large), and an
        // echoed credential exists only on the hosted transport.
        if rule != Rule::BodyTooLarge && rule != Rule::EchoesCredential && !covered.contains(&rule)
        {
            failures.push(format!("no refuse fixture for {rule}"));
        }
    }
    report(&failures);
}

/// Every answer fixture posted to another request id, as a live harness
/// posts it, is judged as it is for the fixtures' own.
#[test]
fn contract_fixtures_answer_any_request() {
    const OTHER: &str = "00112233445566778899aabbccddeeff";
    let mut failures = Vec::new();
    for dir in ["fixtures/answers/accept", "fixtures/answers/refuse"] {
        for rel in json_files(dir) {
            let f = AnswerFixture::load(&rel);
            let mut req = f.request();
            req.id = OTHER.to_string();
            let got = match outcome(&f.body(OTHER), &req) {
                Ok(()) => "accepted".to_string(),
                Err(r) => r.rule.as_str().to_string(),
            };
            let want = f.rule.clone().unwrap_or_else(|| "accepted".into());
            if got != want {
                failures.push(format!("{rel} posted to {OTHER}: {got}, want {want}"));
            }
        }
    }
    report(&failures);
}

#[test]
fn contract_body_too_large() {
    let req = load_request("fixtures/requests/create-repo.json");
    let mut body = format!(
        "{{\"version\":2,\"id\":\"{FIXTURE_REQUEST_ID}\",\"items\":[],\"problems\":[],\"x\":\""
    )
    .into_bytes();
    body.extend(std::iter::repeat_n(b'a', crate::limits::MAX_BODY_BYTES));
    body.extend_from_slice(b"\"}");
    let r = parse_answer(&body, Some(&req)).unwrap_err();
    assert_eq!((r.rule, r.rule.status()), (Rule::BodyTooLarge, 413));
    body.truncate(crate::limits::MAX_BODY_BYTES);
    assert_eq!(
        parse_answer(&body, Some(&req)).unwrap_err().rule,
        Rule::NotJson
    );
}

#[test]
fn contract_request_fixtures() {
    for rel in json_files("fixtures/requests") {
        let req = load_request(&rel);
        assert!(valid_id(&req.id), "{rel}");
    }
}

fn record_fixture(rel: &str) -> (FixtureDoc, Vec<u8>, String, Option<String>) {
    let doc = FixtureDoc::load(
        rel,
        &["name", "description", "record", "record_text", "expect"],
    );
    let body = match (doc.get("record"), doc.get("record_text")) {
        (Some(_), None) => doc.raw("record"),
        (None, Some(_)) => doc.str("record_text").into_bytes(),
        _ => panic!("{rel}: exactly one of record and record_text"),
    };
    let expect = doc
        .get("expect")
        .unwrap_or_else(|| panic!("{rel}: no expect"));
    strict(expect, &["result", "rule"], rel);
    let result = expect.get("result").expect("expect.result").text.clone();
    let rule = expect.get("rule").map(|n| n.text.clone());
    (doc, body, result, rule)
}

#[test]
fn contract_record_accept_fixtures() {
    let mut seen = HashSet::new();
    let mut failures = Vec::new();
    for rel in json_files("fixtures/records/accept") {
        let (_, body, result, rule) = record_fixture(&rel);
        assert!(
            result == "accepted" && rule.is_none(),
            "{rel}: expect in accept/"
        );
        match parse_record(&body) {
            Ok(rec) => {
                seen.insert(rec.state.as_str());
            }
            Err(r) => failures.push(format!(
                "{rel}: refused under {} ({r}); want accepted",
                r.rule
            )),
        }
    }
    for s in State::ALL {
        if !seen.contains(s.as_str()) {
            failures.push(format!("no record fixture in state {}", s.as_str()));
        }
    }
    report(&failures);
}

#[test]
fn contract_record_refuse_fixtures() {
    let mut covered = HashSet::new();
    let mut failures = Vec::new();
    for rel in json_files("fixtures/records/refuse") {
        let (_, body, result, rule) = record_fixture(&rel);
        assert_eq!(result, "refused", "{rel}");
        let name = rule.unwrap_or_else(|| panic!("{rel}: no rule"));
        let rule = Rule::from_name(&name)
            .filter(|r| r.is_answer_rule() || RECORD_RULES.contains(r))
            .unwrap_or_else(|| panic!("{rel}: expect.rule {name:?} is not a rule"));
        covered.insert(rule);
        match parse_record(&body) {
            Ok(_) => failures.push(format!("{rel}: accepted; want refused under {rule}")),
            Err(r) if r.rule != rule => failures.push(format!(
                "{rel}: refused under {} ({r}); want {rule}",
                r.rule
            )),
            Err(r) if !location_is_names(&r.at) => {
                failures.push(format!("{rel}: location {:?}", r.at))
            }
            Err(_) => {}
        }
    }
    for rule in RECORD_RULES {
        if !covered.contains(&rule) {
            failures.push(format!("no record refuse fixture for {rule}"));
        }
    }
    report(&failures);
}

/// Each transcript is read as a source reads a stream: the response's
/// status and three headers, then its chunks in time order through the
/// event reader, whole and split into single bytes; the requests read are
/// the ones `expect` names, in order.
#[test]
fn contract_stream_transcripts() {
    for rel in json_files("fixtures/stream") {
        let doc = FixtureDoc::load(
            &rel,
            &["name", "description", "response", "chunks", "expect"],
        );
        let resp = doc.get("response").expect("response");
        strict(resp, &["status", "headers"], &rel);
        assert_eq!(
            number(resp.get("status").expect("status"), "status"),
            200,
            "{rel}"
        );
        let headers = resp.get("headers").expect("headers");
        let h: BTreeMap<&str, &str> = headers
            .members
            .iter()
            .map(|(k, v)| (k.as_str(), v.text.as_str()))
            .collect();
        assert_eq!(h.len(), 3, "{rel}: headers {h:?}");
        assert_eq!(h.get("Content-Type"), Some(&"text/event-stream"), "{rel}");
        assert_eq!(h.get("Cache-Control"), Some(&"no-store"), "{rel}");
        assert!(
            h.get(crate::limits::HEADER_STREAM)
                .is_some_and(|s| valid_id(s)),
            "{rel}: stream id"
        );

        let chunks = doc.get("chunks").expect("chunks");
        let mut texts = Vec::new();
        let mut last = 0;
        for c in &chunks.elems {
            strict(c, &["at_ms", "text"], &rel);
            let at = number(c.get("at_ms").expect("at_ms"), "at_ms");
            assert!(
                at >= last,
                "{rel}: chunk at {at} ms before the one before it"
            );
            last = at;
            texts.push(c.get("text").expect("text").text.clone());
        }
        let expect = doc.get("expect").expect("expect");
        strict(expect, &["requests"], &rel);
        let want: Vec<String> = expect
            .get("requests")
            .expect("requests")
            .elems
            .iter()
            .map(|n| n.text.clone())
            .collect();

        let read = |pieces: Vec<Vec<u8>>| -> Vec<String> {
            let mut r = EventReader::new();
            let mut ids = Vec::new();
            for p in pieces {
                for ev in r.push(&p).unwrap_or_else(|e| panic!("{rel}: {e}")) {
                    match read_event(&ev) {
                        StreamItem::Request(req) => ids.push(req.id),
                        other => {
                            panic!("{rel}: an event that is not a readable request: {other:?}")
                        }
                    }
                }
            }
            ids
        };
        let whole: Vec<Vec<u8>> = texts.iter().map(|t| t.as_bytes().to_vec()).collect();
        assert_eq!(read(whole), want, "{rel}: requests by chunk");
        let bytes: Vec<Vec<u8>> = texts
            .concat()
            .into_bytes()
            .into_iter()
            .map(|b| vec![b])
            .collect();
        assert_eq!(read(bytes), want, "{rel}: requests byte by byte");
    }
}

/// v2/ against MANIFEST.sha256: every file but the manifest listed with its
/// SHA-256, sorted by path, and nothing else — `sha256sum -c` in a copy.
#[test]
fn contract_manifest() {
    let root = v2();
    let mut files = BTreeMap::new();
    let mut stack = vec![root.clone()];
    while let Some(dir) = stack.pop() {
        for e in fs::read_dir(&dir).unwrap_or_else(|e| panic!("list {}: {e}", dir.display())) {
            let e = e.expect("dir entry");
            let ft = e.file_type().expect("file type");
            let path = e.path();
            if ft.is_dir() {
                stack.push(path);
                continue;
            }
            let rel = path
                .strip_prefix(&root)
                .expect("under v2")
                .components()
                .map(|c| c.as_os_str().to_str().expect("utf-8 path"))
                .collect::<Vec<_>>()
                .join("/");
            if rel == "MANIFEST.sha256" {
                continue;
            }
            assert!(ft.is_file(), "{rel} is not a regular file");
            let sum = Sha256::digest(fs::read(&path).expect("read"));
            files.insert(
                rel,
                sum.iter().map(|b| format!("{b:02x}")).collect::<String>(),
            );
        }
    }
    assert!(!files.is_empty(), "no files under v2/");
    let text = fs::read_to_string(root.join("MANIFEST.sha256")).expect("read the manifest");
    assert!(
        text.ends_with('\n'),
        "the manifest does not end with a newline"
    );
    let mut listed = BTreeMap::new();
    let mut order = Vec::new();
    for (i, line) in text.trim_end_matches('\n').split('\n').enumerate() {
        let (sum, path) = line
            .split_once("  ")
            .unwrap_or_else(|| panic!("manifest line {} is not `<sha256>  <path>`", i + 1));
        assert!(
            sum.len() == 64
                && sum
                    .bytes()
                    .all(|c| c.is_ascii_digit() || (b'a'..=b'f').contains(&c))
                && !path.is_empty(),
            "manifest line {} is not `<sha256>  <path>`",
            i + 1
        );
        assert!(
            listed.insert(path.to_string(), sum.to_string()).is_none(),
            "the manifest lists {path} twice"
        );
        order.push(path.to_string());
    }
    let mut sorted = order.clone();
    sorted.sort();
    assert_eq!(order, sorted, "the manifest is not sorted by path");
    for (path, sum) in &files {
        match listed.get(path) {
            None => panic!("{path} is not in the manifest"),
            Some(want) => assert_eq!(want, sum, "{path} changed"),
        }
    }
    for path in listed.keys() {
        assert!(
            files.contains_key(path),
            "the manifest lists {path}, which is missing"
        );
    }
}
