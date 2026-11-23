//! A strict JSON reader for the contract's documents, and a writer.
//!
//! It holds a document to the contract's Conventions as
//! `internal/delivery/json.go` does: valid UTF-8; exactly one JSON value,
//! an object, with nothing after it but whitespace; nested no deeper than
//! [`MAX_NESTING`](crate::limits::MAX_NESTING); no string escaping an
//! unpaired UTF-16 surrogate (all `not_json`); then the first member named
//! twice in one object (`duplicate_member`) or `null` (`null_value`),
//! whichever comes first in the text. Member names compare after
//! unescaping. A number keeps its literal text: the contract compares
//! numbers as written (`1`, never `1.0`).

use std::collections::HashSet;

use crate::limits::MAX_NESTING;
use crate::rules::{Refusal, Rule};

/// The JSON type of a node.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub(crate) enum Kind {
    Object,
    Array,
    String,
    Number,
    Bool,
    Null,
}

/// One value of a parsed document.
#[derive(Debug, Clone, PartialEq)]
pub(crate) struct Node {
    pub kind: Kind,
    /// A string's value, or a number's literal.
    pub text: String,
    pub boolean: bool,
    /// An object's members, in document order.
    pub members: Vec<(String, Node)>,
    /// An array's elements.
    pub elems: Vec<Node>,
    /// The node's byte span in the document.
    pub span: (usize, usize),
}

impl Node {
    fn new(kind: Kind, start: usize) -> Node {
        Node {
            kind,
            text: String::new(),
            boolean: false,
            members: Vec::new(),
            elems: Vec::new(),
            span: (start, start),
        }
    }

    /// The member named `name`, if any.
    pub fn get(&self, name: &str) -> Option<&Node> {
        self.members.iter().find(|(n, _)| n == name).map(|(_, v)| v)
    }

    /// The member named `name`, if any, for changing.
    #[cfg(test)]
    pub fn get_mut(&mut self, name: &str) -> Option<&mut Node> {
        self.members
            .iter_mut()
            .find(|(n, _)| n == name)
            .map(|(_, v)| v)
    }

    /// A string node.
    #[cfg(test)]
    pub fn string(s: &str) -> Node {
        let mut n = Node::new(Kind::String, 0);
        n.text = s.to_string();
        n
    }

    /// Writes the node as compact JSON.
    #[cfg(test)]
    pub fn write(&self, out: &mut String) {
        match self.kind {
            Kind::Object => {
                out.push('{');
                for (i, (name, v)) in self.members.iter().enumerate() {
                    if i > 0 {
                        out.push(',');
                    }
                    write_string(out, name);
                    out.push(':');
                    v.write(out);
                }
                out.push('}');
            }
            Kind::Array => {
                out.push('[');
                for (i, v) in self.elems.iter().enumerate() {
                    if i > 0 {
                        out.push(',');
                    }
                    v.write(out);
                }
                out.push(']');
            }
            Kind::String => write_string(out, &self.text),
            Kind::Number => out.push_str(&self.text),
            Kind::Bool => out.push_str(if self.boolean { "true" } else { "false" }),
            Kind::Null => out.push_str("null"),
        }
    }
}

/// Parses `body` as one contract document. In order: not UTF-8, not one
/// JSON value, not an object, too deep or an unpaired surrogate is
/// `not_json`; then the first duplicate member or `null` in the text.
pub(crate) fn parse_document(body: &[u8]) -> Result<Node, Refusal> {
    let (root, first) =
        parse_with(body, MAX_NESTING).map_err(|()| Refusal::new(Rule::NotJson, ""))?;
    if root.kind != Kind::Object {
        return Err(Refusal::new(Rule::NotJson, ""));
    }
    if let Some(r) = first {
        return Err(r);
    }
    Ok(root)
}

/// Parses `body` as one JSON value nested at most `max_depth` levels. It
/// returns a syntax error, or the tree and the first duplicate member or
/// null refusal in document order.
pub(crate) fn parse_with(body: &[u8], max_depth: usize) -> Result<(Node, Option<Refusal>), ()> {
    let text = std::str::from_utf8(body).map_err(|_| ())?;
    let mut p = Parser {
        s: text.as_bytes(),
        i: 0,
        max_depth,
        first: None,
    };
    p.ws();
    let root = p.value(0, String::new())?;
    p.ws();
    if p.i != p.s.len() {
        return Err(());
    }
    Ok((root, p.first))
}

struct Parser<'a> {
    s: &'a [u8],
    i: usize,
    max_depth: usize,
    first: Option<Refusal>,
}

impl Parser<'_> {
    fn ws(&mut self) {
        while self.i < self.s.len() && matches!(self.s[self.i], b' ' | b'\t' | b'\n' | b'\r') {
            self.i += 1;
        }
    }

    fn peek(&self) -> Option<u8> {
        self.s.get(self.i).copied()
    }

    fn note(&mut self, r: Refusal) {
        if self.first.is_none() {
            self.first = Some(r);
        }
    }

    fn lit(&mut self, word: &[u8]) -> Result<(), ()> {
        if self.s[self.i..].starts_with(word) {
            self.i += word.len();
            Ok(())
        } else {
            Err(())
        }
    }

    /// One value at `depth` containers deep, located at `at`.
    fn value(&mut self, depth: usize, at: String) -> Result<Node, ()> {
        let start = self.i;
        let mut n = match self.peek().ok_or(())? {
            b'{' => self.object(depth + 1, &at)?,
            b'[' => self.array(depth + 1, &at)?,
            b'"' => {
                let mut n = Node::new(Kind::String, start);
                n.text = self.string()?;
                n
            }
            b't' => {
                self.lit(b"true")?;
                let mut n = Node::new(Kind::Bool, start);
                n.boolean = true;
                n
            }
            b'f' => {
                self.lit(b"false")?;
                Node::new(Kind::Bool, start)
            }
            b'n' => {
                self.lit(b"null")?;
                self.note(Refusal::new(Rule::NullValue, at));
                Node::new(Kind::Null, start)
            }
            b'-' | b'0'..=b'9' => {
                self.number()?;
                let mut n = Node::new(Kind::Number, start);
                n.text = String::from_utf8(self.s[start..self.i].to_vec()).map_err(|_| ())?;
                n
            }
            _ => return Err(()),
        };
        n.span = (start, self.i);
        Ok(n)
    }

    fn object(&mut self, depth: usize, at: &str) -> Result<Node, ()> {
        if depth > self.max_depth {
            return Err(());
        }
        let mut n = Node::new(Kind::Object, self.i);
        // The names seen so far in this object: a set, so an object of many
        // members costs no more than reading them (Go uses a map too).
        let mut seen: HashSet<String> = HashSet::new();
        self.i += 1;
        self.ws();
        if self.peek() == Some(b'}') {
            self.i += 1;
            return Ok(n);
        }
        loop {
            if self.peek() != Some(b'"') {
                return Err(());
            }
            let name = self.string()?;
            let here = join(at, &safe_name(&name));
            if !seen.insert(name.clone()) {
                self.note(Refusal::new(Rule::DuplicateMember, here.clone()));
            }
            self.ws();
            if self.peek() != Some(b':') {
                return Err(());
            }
            self.i += 1;
            self.ws();
            let v = self.value(depth, here)?;
            n.members.push((name, v));
            self.ws();
            match self.peek() {
                Some(b',') => {
                    self.i += 1;
                    self.ws();
                }
                Some(b'}') => {
                    self.i += 1;
                    return Ok(n);
                }
                _ => return Err(()),
            }
        }
    }

    fn array(&mut self, depth: usize, at: &str) -> Result<Node, ()> {
        if depth > self.max_depth {
            return Err(());
        }
        let mut n = Node::new(Kind::Array, self.i);
        self.i += 1;
        self.ws();
        if self.peek() == Some(b']') {
            self.i += 1;
            return Ok(n);
        }
        loop {
            let here = index(at, n.elems.len());
            let v = self.value(depth, here)?;
            n.elems.push(v);
            self.ws();
            match self.peek() {
                Some(b',') => {
                    self.i += 1;
                    self.ws();
                }
                Some(b']') => {
                    self.i += 1;
                    return Ok(n);
                }
                _ => return Err(()),
            }
        }
    }

    fn hex4(&mut self) -> Result<u32, ()> {
        let h = self.s.get(self.i..self.i + 4).ok_or(())?;
        let mut v = 0u32;
        for &c in h {
            v = v * 16 + char::from(c).to_digit(16).ok_or(())?;
        }
        self.i += 4;
        Ok(v)
    }

    /// A string at `self.i` (on its opening quote). An escaped unpaired
    /// surrogate is an error: the contract refuses it, never repairs it.
    fn string(&mut self) -> Result<String, ()> {
        self.i += 1;
        let mut out = String::new();
        let mut run = self.i;
        loop {
            let c = self.peek().ok_or(())?;
            match c {
                b'"' => {
                    out.push_str(std::str::from_utf8(&self.s[run..self.i]).map_err(|_| ())?);
                    self.i += 1;
                    return Ok(out);
                }
                b'\\' => {
                    out.push_str(std::str::from_utf8(&self.s[run..self.i]).map_err(|_| ())?);
                    self.i += 1;
                    let e = self.peek().ok_or(())?;
                    self.i += 1;
                    match e {
                        b'"' => out.push('"'),
                        b'\\' => out.push('\\'),
                        b'/' => out.push('/'),
                        b'b' => out.push('\u{8}'),
                        b'f' => out.push('\u{c}'),
                        b'n' => out.push('\n'),
                        b'r' => out.push('\r'),
                        b't' => out.push('\t'),
                        b'u' => {
                            let hi = self.hex4()?;
                            let cp = match hi {
                                0xdc00..=0xdfff => return Err(()),
                                0xd800..=0xdbff => {
                                    if self.s.get(self.i..self.i + 2) != Some(b"\\u") {
                                        return Err(());
                                    }
                                    self.i += 2;
                                    let lo = self.hex4()?;
                                    if !(0xdc00..=0xdfff).contains(&lo) {
                                        return Err(());
                                    }
                                    0x10000 + ((hi - 0xd800) << 10) + (lo - 0xdc00)
                                }
                                _ => hi,
                            };
                            out.push(char::from_u32(cp).ok_or(())?);
                        }
                        _ => return Err(()),
                    }
                    run = self.i;
                }
                0x00..=0x1f => return Err(()),
                _ => self.i += 1,
            }
        }
    }

    fn digits(&mut self) -> usize {
        let start = self.i;
        while self.peek().is_some_and(|c| c.is_ascii_digit()) {
            self.i += 1;
        }
        self.i - start
    }

    fn number(&mut self) -> Result<(), ()> {
        if self.peek() == Some(b'-') {
            self.i += 1;
        }
        match self.peek() {
            Some(b'0') => self.i += 1,
            Some(b'1'..=b'9') => {
                self.digits();
            }
            _ => return Err(()),
        }
        if self.peek() == Some(b'.') {
            self.i += 1;
            if self.digits() == 0 {
                return Err(());
            }
        }
        if matches!(self.peek(), Some(b'e' | b'E')) {
            self.i += 1;
            if matches!(self.peek(), Some(b'+' | b'-')) {
                self.i += 1;
            }
            if self.digits() == 0 {
                return Err(());
            }
        }
        Ok(())
    }
}

/// The member names the contract defines. A refusal's location names only
/// these: any other name is a source's own text, which could be anything,
/// so it is written `*`.
const KNOWN_NAMES: &[&str] = &[
    // answer, item, problem
    "version",
    "id",
    "items",
    "problems",
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
    "code",
    "message",
    // request
    "reason",
    "deadline",
    "user",
    "shed",
    "backend",
    "repo",
    "local_dir",
    "project_dir",
    "url",
    // record
    "state",
    "steps",
    "last",
    "stage1",
    "clone",
    "stage2",
    "install",
    "at",
    "outcome",
    "failure",
    "source",
    "rule",
    "item",
    "result",
    "machine_id",
    "machine_name",
    "target",
    "written_at",
];

/// `name` when the contract defines it, else `*`.
pub(crate) fn safe_name(name: &str) -> String {
    if KNOWN_NAMES.contains(&name) {
        name.to_string()
    } else {
        "*".to_string()
    }
}

/// `at.name`, or `name` at the top.
pub(crate) fn join(at: &str, name: &str) -> String {
    if at.is_empty() {
        name.to_string()
    } else {
        format!("{at}.{name}")
    }
}

/// `at[i]`.
pub(crate) fn index(at: &str, i: usize) -> String {
    format!("{at}[{i}]")
}

/// Writes `s` as a JSON string: `"` and `\` escaped, control characters
/// below U+0020 as `\u00XX`, everything else as its UTF-8.
pub(crate) fn write_string(out: &mut String, s: &str) {
    out.push('"');
    for c in s.chars() {
        match c {
            '"' => out.push_str("\\\""),
            '\\' => out.push_str("\\\\"),
            '\n' => out.push_str("\\n"),
            '\r' => out.push_str("\\r"),
            '\t' => out.push_str("\\t"),
            c if (c as u32) < 0x20 => out.push_str(&format!("\\u{:04x}", c as u32)),
            c => out.push(c),
        }
    }
    out.push('"');
}

/// Checks an object's members against the names it may hold (unknown
/// member), then the names it must hold (missing member), each in the order
/// of the object's members and of `required`.
pub(crate) fn check_members(
    n: &Node,
    at: &str,
    allowed: &[&str],
    required: &[&str],
) -> Result<(), Refusal> {
    for (name, _) in &n.members {
        if !allowed.contains(&name.as_str()) {
            return Err(Refusal::new(
                Rule::UnknownMember,
                join(at, &safe_name(name)),
            ));
        }
    }
    for name in required {
        if n.get(name).is_none() {
            return Err(Refusal::new(Rule::MissingMember, join(at, name)));
        }
    }
    Ok(())
}

/// Checks each member present of `types` is of its JSON type, in the order
/// of `types`.
pub(crate) fn check_types(n: &Node, at: &str, types: &[(&str, Kind)]) -> Result<(), Refusal> {
    for (name, kind) in types {
        if let Some(v) = n.get(name) {
            if v.kind != *kind {
                return Err(Refusal::new(Rule::WrongType, join(at, name)));
            }
        }
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    fn rule(body: &str) -> Option<Rule> {
        parse_document(body.as_bytes()).err().map(|r| r.rule)
    }

    #[test]
    fn syntax() {
        assert_eq!(rule("{}"), None);
        assert_eq!(rule(" {\"a\": [1, -0.5e+3, true, false, \"x\"]} \n"), None);
        for bad in [
            "",
            "[]",
            "\"x\"",
            "{} x",
            "{}{}",
            "{\"a\":01}",
            "{\"a\":1.}",
            "{\"a\":-}",
            "{\"a\":[1,]}",
            "{\"a\":1,}",
            "{'a':1}",
            "{\"a\":\"\t\"}",
            "{\"a\":\"\\x\"}",
            "{\"a\":\"\\ud800\"}",
            "{\"a\":\"\\udc00\\ud800\"}",
            "{\"a\":\"\\ud800\\u0041\"}",
            "\u{feff}{}",
            "{\"a\":tru}",
        ] {
            assert_eq!(rule(bad), Some(Rule::NotJson), "{bad:?}");
        }
        assert_eq!(rule("{\"a\":\"\\ud83d\\ude00\"}"), None);
        assert_eq!(
            parse_document(&[b'{', b'"', 0xff, b'"', b':', b'1', b'}'])
                .unwrap_err()
                .rule,
            Rule::NotJson
        );
    }

    #[test]
    fn depth() {
        let ok = "{\"a\":".repeat(15) + "{}" + &"}".repeat(15);
        assert_eq!(rule(&ok), None);
        let deep = "{\"a\":".repeat(16) + "{}" + &"}".repeat(16);
        assert_eq!(rule(&deep), Some(Rule::NotJson));
    }

    #[test]
    fn duplicates_and_nulls_first_in_text() {
        assert_eq!(rule("{\"a\":1,\"a\":2}"), Some(Rule::DuplicateMember));
        assert_eq!(rule("{\"a\":1,\"\\u0061\":2}"), Some(Rule::DuplicateMember));
        assert_eq!(rule("{\"a\":null,\"b\":1,\"b\":2}"), Some(Rule::NullValue));
        let r = parse_document(b"{\"x\":{\"q\":1,\"q\":2},\"a\":null}").unwrap_err();
        assert_eq!((r.rule, r.at.as_str()), (Rule::DuplicateMember, "*.*"));
        // A syntax error after a duplicate is still not_json.
        assert_eq!(rule("{\"a\":1,\"a\":2"), Some(Rule::NotJson));
    }

    #[test]
    fn many_members_are_read_in_linear_time() {
        // 80,000 distinct six-character names: one pass, not 3.2 billion
        // comparisons. The answer reader then refuses the first unknown one.
        let mut body = String::from("{");
        for i in 0..80_000u32 {
            if i > 0 {
                body.push(',');
            }
            body.push_str(&format!("\"k{i:05}\":1"));
        }
        body.push('}');
        let start = std::time::Instant::now();
        let n = parse_document(body.as_bytes()).unwrap();
        assert_eq!(n.members.len(), 80_000);
        let r = crate::answer::parse_answer(body.as_bytes(), None).unwrap_err();
        assert_eq!(r.rule, Rule::UnknownMember);
        // A duplicate at the very end is still found, and still loses to a
        // null before it.
        let dup = format!("{},\"k00000\":2}}", &body[..body.len() - 1]);
        assert_eq!(
            parse_document(dup.as_bytes()).unwrap_err().rule,
            Rule::DuplicateMember
        );
        let null_first = format!("{{\"z\":null,{}", &dup[1..]);
        assert_eq!(
            parse_document(null_first.as_bytes()).unwrap_err().rule,
            Rule::NullValue
        );
        assert!(
            start.elapsed() < std::time::Duration::from_secs(5),
            "{:?}",
            start.elapsed()
        );
    }

    #[test]
    fn write_round_trips() {
        let src = "{\"a\":\"q\\\"\\\\\\n\\u0001é\",\"n\":1.0,\"l\":[true,false,{}]}";
        let n = parse_document(src.as_bytes()).unwrap();
        let mut out = String::new();
        n.write(&mut out);
        let again = parse_document(out.as_bytes()).unwrap();
        assert_eq!(again.get("a").unwrap().text, "q\"\\\n\u{1}é");
        assert_eq!(again.get("n").unwrap().text, "1.0");
    }
}
