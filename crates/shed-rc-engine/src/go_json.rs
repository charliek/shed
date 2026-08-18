//! A **byte-exact** re-implementation of the slice of Go's `encoding/json` the
//! preseeds depend on: decode a document the way `json.Decoder` +
//! `Decoder.UseNumber` does, and re-encode it the way `json.MarshalIndent(v, "",
//! "  ")` does.
//!
//! ## Why this exists
//!
//! `~/.claude.json` and `~/.cursor/hooks.json` are **raw-bytes parity surfaces**
//! (plan 009 §3.5): a mixed Go/Rust fleet merges into the SAME files in place, so
//! "the merge is semantically right" is not enough — two implementations that
//! disagree byte-for-byte would rewrite each other's file on every create,
//! churning content the user never touched (and, worse, silently reformatting a
//! config the agent itself also writes). `serde_json::to_string_pretty` differs
//! from Go on four axes at once, every one of which is observable in a diff:
//!
//! | axis | Go (`MarshalIndent`) | `serde_json` |
//! |---|---|---|
//! | object key order | **sorted**, at every nesting level (Go maps marshal sorted) | insertion/arbitrary |
//! | HTML escaping | `<` `>` `&` → `\u003c` `\u003e` `\u0026`; U+2028/29 → `\u2028`/`\u2029` | emitted raw |
//! | numbers | `json.Number` keeps the ORIGINAL literal (`1e10`, `0.10`, `-0`, `9007199254740993`) | re-rendered through `f64`/`i64` |
//! | empty containers | `{}` / `[]` inline | `{}` / `[]` inline (agrees) |
//!
//! ## The Go semantics reproduced here (verified against a real `go run`, not docs)
//!
//! Decode ([`parse_document`]) mirrors `readJSONObject`'s decoder
//! (`internal/ext/rc/trust.go:147`): `UseNumber` keeps every number as its raw
//! literal, a top-level `null` decodes the map to nil (the caller re-seeds `{}`),
//! a non-object top level is an error, duplicate keys keep the LAST occurrence,
//! and trailing content after the top-level value is an error (Go needs a second
//! `Decode` hitting `io.EOF` to see this; here it falls out of parsing the whole
//! slice).
//!
//! Encode ([`marshal_indent`]) mirrors `json.MarshalIndent(config, "", "  ")`:
//! two-space indent, `": "` after a key, keys sorted bytewise (a [`BTreeMap`] IS
//! that order — Rust's `String: Ord` is bytewise, like Go's `sort.Strings`),
//! numbers written as their preserved literal, strings re-escaped by Go's
//! `escapeHTML` encoder, and NO trailing newline.
//!
//! Deliberate residual deviation, documented rather than chased: for INVALID
//! UTF-8 inside a JSON string both implementations substitute U+FFFD, byte by
//! byte — this port replicates Go's advance-one-byte loop exactly (see
//! [`push_lossy`]), which is *not* what `String::from_utf8_lossy` does (it
//! collapses a maximal invalid subsequence into one replacement char). Matching
//! Go was cheaper than documenting a divergence.

use std::collections::BTreeMap;

/// A decoded JSON value with Go's `UseNumber` fidelity: an object is a sorted map
/// (the order Go marshals in), and a NUMBER keeps the raw literal bytes it was
/// read from — the whole point of the module.
#[derive(Debug, Clone, PartialEq)]
pub enum GoValue {
    Null,
    Bool(bool),
    /// The original literal, verbatim (`json.Number`).
    Number(String),
    String(String),
    Array(Vec<GoValue>),
    Object(GoObject),
}

/// A JSON object. `BTreeMap` because Go marshals map keys **sorted**, and Rust's
/// `String` ordering is bytewise like Go's.
pub type GoObject = BTreeMap<String, GoValue>;

impl GoValue {
    /// The value's shape name for an error message — `jsonShapeOf`
    /// (`preseed_cursor.go:288`), whose wording the cursor preseed's refusals
    /// quote verbatim.
    pub fn shape(&self) -> &'static str {
        match self {
            GoValue::Null => "null",
            GoValue::Object(_) => "object",
            GoValue::Array(_) => "array",
            GoValue::String(_) => "string",
            GoValue::Bool(_) => "boolean",
            GoValue::Number(_) => "number",
        }
    }

    /// The value as an object, or `None` for any other shape.
    pub fn as_object(&self) -> Option<&GoObject> {
        match self {
            GoValue::Object(o) => Some(o),
            _ => None,
        }
    }

    /// The value as an array, or `None` for any other shape.
    pub fn as_array(&self) -> Option<&Vec<GoValue>> {
        match self {
            GoValue::Array(a) => Some(a),
            _ => None,
        }
    }

    /// The value as a string, or `None` for any other shape.
    pub fn as_str(&self) -> Option<&str> {
        match self {
            GoValue::String(s) => Some(s),
            _ => None,
        }
    }

    /// The value parsed as an `i64` when it is a number that fits, else 0 —
    /// `jsonNumberInt` (`trust.go:35`), which is how the
    /// `fullscreenUpsellSeenCount` floor compares against whatever was there.
    pub fn number_int(&self) -> i64 {
        match self {
            GoValue::Number(raw) => raw.parse::<i64>().unwrap_or(0),
            _ => 0,
        }
    }
}

// ---------------------------------------------------------------------------
// decode
// ---------------------------------------------------------------------------

/// Go's `maxNestingDepth` (`encoding/json/scanner.go`): a document may nest
/// 10000 containers, and the 10001st `{`/`[` is refused with
/// `invalid character '<c>' exceeded max depth`, naming the OPENING byte.
///
/// Measured, not read off the docs: against this repo's toolchain a
/// `{"a":` + `[`x9999 + `]`x9999 + `}` document (depth 10000) decodes fine and
/// depth 10001 returns exactly that message; an all-object chain reports
/// `'{'` the same way.
///
/// The cap is load-bearing rather than cosmetic. The parser below is plain
/// recursive descent, so an uncapped hostile (or merely corrupt)
/// `~/.claude.json` runs it off the end of the thread stack — and a Rust stack
/// overflow is not a catchable error but a SIGABRT, killing the whole `sx`
/// process where Go merely declines the preseed and lets the create carry on.
/// Capping alone is not sufficient, though: 10000 accepted frames still need a
/// stack far larger than any default, which is why every preseed runs on the
/// dedicated big-stack worker in [`super::preseed::dispatch`].
const MAX_NESTING_DEPTH: usize = 10000;

/// Parse a whole document as Go's `json.Decoder{UseNumber}.Decode(&map)` does.
///
/// * `Ok(Some(obj))` — a top-level object.
/// * `Ok(None)` — the literal `null` (Go decodes the map to nil; the callers
///   re-seed an empty object rather than panicking).
/// * `Err(msg)` — malformed JSON, a non-object top level, or trailing content
///   after the top-level value. Every caller's contract is to leave the file
///   exactly as it is when this comes back.
pub fn parse_document(data: &[u8]) -> Result<Option<GoObject>, String> {
    let mut p = Parser {
        b: data,
        i: 0,
        depth: 0,
    };
    p.skip_ws();
    let value = p.parse_value()?;
    p.skip_ws();
    if p.i < p.b.len() {
        // Go's decoder stops after ONE value and would happily accept another; the
        // preseed's second `Decode` must hit io.EOF, so anything here is a refusal
        // (`trust.go:174`).
        return Err("trailing data after the top-level JSON value".to_string());
    }
    match value {
        GoValue::Object(obj) => Ok(Some(obj)),
        GoValue::Null => Ok(None),
        other => Err(format!("cannot unmarshal {} into an object", other.shape())),
    }
}

struct Parser<'a> {
    b: &'a [u8],
    i: usize,
    /// Containers currently open — Go's `scanner.parseState` stack depth.
    depth: usize,
}

impl Parser<'_> {
    fn skip_ws(&mut self) {
        while let Some(c) = self.b.get(self.i) {
            // Go's scanner treats exactly these four bytes as whitespace.
            if matches!(c, b' ' | b'\t' | b'\n' | b'\r') {
                self.i += 1;
            } else {
                break;
            }
        }
    }

    fn err<T>(&self, what: &str) -> Result<T, String> {
        match self.b.get(self.i) {
            Some(c) => Err(format!(
                "invalid character {:?} at byte {} ({what})",
                *c as char, self.i
            )),
            None => Err(format!("unexpected end of JSON input ({what})")),
        }
    }

    fn parse_value(&mut self) -> Result<GoValue, String> {
        match self.b.get(self.i) {
            Some(b'{') => self.parse_nested(Self::parse_object),
            Some(b'[') => self.parse_nested(Self::parse_array),
            Some(b'"') => Ok(GoValue::String(self.parse_string()?)),
            Some(b't') => self.parse_literal("true", GoValue::Bool(true)),
            Some(b'f') => self.parse_literal("false", GoValue::Bool(false)),
            Some(b'n') => self.parse_literal("null", GoValue::Null),
            Some(c) if *c == b'-' || c.is_ascii_digit() => self.parse_number(),
            _ => self.err("looking for beginning of value"),
        }
    }

    /// Open one container frame, run `inner`, and close it again — Go's
    /// `pushParseState` depth check, refusing at [`MAX_NESTING_DEPTH`].
    ///
    /// The refusal quotes the OPENING byte and stops at it, which is why the
    /// check runs here (`self.i` is still on the `{`/`[`) rather than inside
    /// `parse_object`/`parse_array` after they have consumed it.
    fn parse_nested(
        &mut self,
        inner: fn(&mut Self) -> Result<GoValue, String>,
    ) -> Result<GoValue, String> {
        self.depth += 1;
        if self.depth > MAX_NESTING_DEPTH {
            // `{:?}` on a char renders Go's single-quoted form verbatim.
            let opener = self.b[self.i] as char;
            return Err(format!("invalid character {opener:?} exceeded max depth"));
        }
        let out = inner(self);
        self.depth -= 1;
        out
    }

    fn parse_literal(&mut self, word: &str, value: GoValue) -> Result<GoValue, String> {
        if self.b[self.i..].starts_with(word.as_bytes()) {
            self.i += word.len();
            return Ok(value);
        }
        self.err("looking for beginning of value")
    }

    fn parse_object(&mut self) -> Result<GoValue, String> {
        self.i += 1; // '{'
        let mut out = GoObject::new();
        self.skip_ws();
        if self.b.get(self.i) == Some(&b'}') {
            self.i += 1;
            return Ok(GoValue::Object(out));
        }
        loop {
            self.skip_ws();
            if self.b.get(self.i) != Some(&b'"') {
                return self.err("looking for beginning of object key string");
            }
            let key = self.parse_string()?;
            self.skip_ws();
            if self.b.get(self.i) != Some(&b':') {
                return self.err("after object key");
            }
            self.i += 1;
            self.skip_ws();
            let value = self.parse_value()?;
            // Go's map decode keeps the LAST occurrence of a duplicate key.
            out.insert(key, value);
            self.skip_ws();
            match self.b.get(self.i) {
                Some(b',') => self.i += 1,
                Some(b'}') => {
                    self.i += 1;
                    return Ok(GoValue::Object(out));
                }
                _ => return self.err("after object key:value pair"),
            }
        }
    }

    fn parse_array(&mut self) -> Result<GoValue, String> {
        self.i += 1; // '['
        let mut out = Vec::new();
        self.skip_ws();
        if self.b.get(self.i) == Some(&b']') {
            self.i += 1;
            return Ok(GoValue::Array(out));
        }
        loop {
            self.skip_ws();
            out.push(self.parse_value()?);
            self.skip_ws();
            match self.b.get(self.i) {
                Some(b',') => self.i += 1,
                Some(b']') => {
                    self.i += 1;
                    return Ok(GoValue::Array(out));
                }
                _ => return self.err("after array element"),
            }
        }
    }

    /// Consume a run of ASCII digits, reporting whether there was at least one.
    fn scan_digits(&mut self) -> bool {
        let start = self.i;
        while self.b.get(self.i).is_some_and(u8::is_ascii_digit) {
            self.i += 1;
        }
        self.i > start
    }

    /// Go's number grammar (`-?(0|[1-9][0-9]*)(\.[0-9]+)?([eE][+-]?[0-9]+)?`),
    /// captured as its RAW literal — never through a float.
    fn parse_number(&mut self) -> Result<GoValue, String> {
        let start = self.i;
        if self.b.get(self.i) == Some(&b'-') {
            self.i += 1;
        }
        match self.b.get(self.i) {
            // A leading `0` stands alone: `01` is not a JSON number, and stopping
            // here is what makes the trailing `1` a parse error further up.
            Some(b'0') => self.i += 1,
            Some(c) if c.is_ascii_digit() => {
                self.scan_digits();
            }
            _ => return self.err("in numeric literal"),
        }
        if self.b.get(self.i) == Some(&b'.') {
            self.i += 1;
            if !self.scan_digits() {
                return self.err("after decimal point in numeric literal");
            }
        }
        if matches!(self.b.get(self.i), Some(b'e' | b'E')) {
            self.i += 1;
            if matches!(self.b.get(self.i), Some(b'+' | b'-')) {
                self.i += 1;
            }
            if !self.scan_digits() {
                return self.err("in exponent of numeric literal");
            }
        }
        // The bytes are ASCII by construction.
        Ok(GoValue::Number(
            String::from_utf8_lossy(&self.b[start..self.i]).into_owned(),
        ))
    }

    /// Go's `unquoteBytes`: standard escapes, `\uXXXX` with surrogate pairing (a
    /// lone or mispaired surrogate becomes U+FFFD), raw control bytes REJECTED,
    /// invalid UTF-8 replaced byte-by-byte.
    fn parse_string(&mut self) -> Result<String, String> {
        self.i += 1; // '"'
        let mut out = String::new();
        let mut raw_start = self.i;
        loop {
            let Some(&c) = self.b.get(self.i) else {
                return Err("unexpected end of JSON input (in string literal)".to_string());
            };
            match c {
                b'"' => {
                    push_lossy(&mut out, &self.b[raw_start..self.i]);
                    self.i += 1;
                    return Ok(out);
                }
                b'\\' => {
                    push_lossy(&mut out, &self.b[raw_start..self.i]);
                    self.i += 1;
                    self.parse_escape(&mut out)?;
                    raw_start = self.i;
                }
                0x00..=0x1f => return self.err("in string literal"),
                _ => self.i += 1,
            }
        }
    }

    fn parse_escape(&mut self, out: &mut String) -> Result<(), String> {
        let Some(&c) = self.b.get(self.i) else {
            return Err("unexpected end of JSON input (in string escape)".to_string());
        };
        self.i += 1;
        let simple = match c {
            b'"' => Some('"'),
            b'\\' => Some('\\'),
            b'/' => Some('/'),
            b'b' => Some('\u{8}'),
            b'f' => Some('\u{c}'),
            b'n' => Some('\n'),
            b'r' => Some('\r'),
            b't' => Some('\t'),
            _ => None,
        };
        if let Some(ch) = simple {
            out.push(ch);
            return Ok(());
        }
        if c != b'u' {
            return Err(format!(
                "invalid character {:?} in string escape code",
                c as char
            ));
        }
        let hi = self.parse_hex4()?;
        // A high surrogate pairs with an immediately following low surrogate;
        // anything else (a lone half, a bad pair) is U+FFFD, exactly as Go does.
        if (0xd800..0xdc00).contains(&hi) {
            if self.b.get(self.i) == Some(&b'\\') && self.b.get(self.i + 1) == Some(&b'u') {
                let save = self.i;
                self.i += 2;
                let lo = self.parse_hex4()?;
                if (0xdc00..0xe000).contains(&lo) {
                    let combined = 0x10000 + ((hi - 0xd800) << 10) + (lo - 0xdc00);
                    out.push(char::from_u32(combined).unwrap_or('\u{fffd}'));
                    return Ok(());
                }
                self.i = save;
            }
            out.push('\u{fffd}');
            return Ok(());
        }
        out.push(char::from_u32(hi).unwrap_or('\u{fffd}'));
        Ok(())
    }

    fn parse_hex4(&mut self) -> Result<u32, String> {
        if self.i + 4 > self.b.len() {
            return Err("unexpected end of JSON input (in \\u escape)".to_string());
        }
        let mut value = 0u32;
        for _ in 0..4 {
            let c = self.b[self.i];
            let digit = (c as char)
                .to_digit(16)
                .ok_or_else(|| format!("invalid character {:?} in \\u hexadecimal", c as char))?;
            value = value * 16 + digit;
            self.i += 1;
        }
        Ok(value)
    }
}

/// Append `bytes` to `out`, substituting U+FFFD for each invalid byte **the way
/// Go does** — `utf8.DecodeRune` returning `(RuneError, 1)` makes Go emit one
/// replacement char and advance exactly one byte, where
/// `String::from_utf8_lossy` would collapse a whole invalid subsequence into one.
fn push_lossy(out: &mut String, mut bytes: &[u8]) {
    loop {
        match std::str::from_utf8(bytes) {
            Ok(valid) => {
                out.push_str(valid);
                return;
            }
            Err(err) => {
                let good = err.valid_up_to();
                // SAFETY-free: `valid_up_to` bytes are valid UTF-8 by definition.
                out.push_str(std::str::from_utf8(&bytes[..good]).unwrap_or_default());
                out.push('\u{fffd}');
                bytes = &bytes[good + 1..];
            }
        }
    }
}

// ---------------------------------------------------------------------------
// encode
// ---------------------------------------------------------------------------

/// The indent unit — Go's `json.MarshalIndent(v, "", "  ")`.
const INDENT: &str = "  ";

/// Encode an object exactly as `json.MarshalIndent(config, "", "  ")` does. No
/// trailing newline (Go's does not add one; `atomicWrite` writes the bytes as
/// they come).
pub fn marshal_indent(obj: &GoObject) -> String {
    let mut out = String::new();
    write_object(&mut out, obj, 0);
    out
}

fn write_value(out: &mut String, value: &GoValue, depth: usize) {
    match value {
        GoValue::Null => out.push_str("null"),
        GoValue::Bool(true) => out.push_str("true"),
        GoValue::Bool(false) => out.push_str("false"),
        GoValue::Number(raw) => out.push_str(raw),
        GoValue::String(s) => write_string(out, s),
        GoValue::Array(items) => {
            if items.is_empty() {
                out.push_str("[]");
                return;
            }
            out.push('[');
            for (n, item) in items.iter().enumerate() {
                if n > 0 {
                    out.push(',');
                }
                out.push('\n');
                indent(out, depth + 1);
                write_value(out, item, depth + 1);
            }
            out.push('\n');
            indent(out, depth);
            out.push(']');
        }
        GoValue::Object(obj) => write_object(out, obj, depth),
    }
}

fn write_object(out: &mut String, obj: &GoObject, depth: usize) {
    if obj.is_empty() {
        out.push_str("{}");
        return;
    }
    out.push('{');
    // BTreeMap iterates in sorted key order — Go's map marshaling sorts too.
    for (n, (key, value)) in obj.iter().enumerate() {
        if n > 0 {
            out.push(',');
        }
        out.push('\n');
        indent(out, depth + 1);
        write_string(out, key);
        out.push_str(": ");
        write_value(out, value, depth + 1);
    }
    out.push('\n');
    indent(out, depth);
    out.push('}');
}

fn indent(out: &mut String, depth: usize) {
    for _ in 0..depth {
        out.push_str(INDENT);
    }
}

/// Go's string encoder with `escapeHTML` ON (the `json.Marshal` default): `"` and
/// `\` backslash-escaped; `\b`/`\f`/`\n`/`\r`/`\t` short forms; every other
/// byte below 0x20 as `\u00xx` with LOWERCASE hex; `<`, `>` and `&` as `\u003c`/`\u003e`/`\u0026`
/// (so a JSON blob can be embedded in HTML); U+2028/U+2029 escaped (they are line
/// terminators to a JavaScript parser). Notably NOT escaped: DEL (0x7f) and every
/// other non-ASCII rune, which are emitted as raw UTF-8.
///
/// The `\b` (0x08) and `\f` (0x0c) short forms are the easy ones to miss: the
/// prose in `encoding/json`'s docs calls out only `\t`/`\n`/`\r`, but the
/// encoder really does emit two-character forms for 0x08 and 0x0c too — an
/// exhaustive `json.Marshal` sweep over U+0000..U+0020 against this repo's
/// toolchain shows those five and NOTHING else escaping short (verified, not
/// read off the docs). Spelling them the generic `\u0008`/`\u000c` way instead
/// is a LIVE byte divergence: any `~/.claude.json` holding either control
/// character would be rewritten wholesale by the other implementation on its
/// very next create.
fn write_string(out: &mut String, s: &str) {
    const HEX: &[u8; 16] = b"0123456789abcdef";
    out.push('"');
    for ch in s.chars() {
        match ch {
            '"' => out.push_str("\\\""),
            '\\' => out.push_str("\\\\"),
            '\u{8}' => out.push_str("\\b"),
            '\u{c}' => out.push_str("\\f"),
            '\n' => out.push_str("\\n"),
            '\r' => out.push_str("\\r"),
            '\t' => out.push_str("\\t"),
            '<' => out.push_str("\\u003c"),
            '>' => out.push_str("\\u003e"),
            '&' => out.push_str("\\u0026"),
            '\u{2028}' => out.push_str("\\u2028"),
            '\u{2029}' => out.push_str("\\u2029"),
            c if (c as u32) < 0x20 => {
                let b = c as u32;
                out.push_str("\\u00");
                out.push(HEX[(b >> 4) as usize] as char);
                out.push(HEX[(b & 0xf) as usize] as char);
            }
            c => out.push(c),
        }
    }
    out.push('"');
}

#[cfg(test)]
mod tests {
    use super::*;

    /// Round-trip a document and return the re-encoded bytes — the shape every
    /// preseed write takes (decode, merge, `MarshalIndent`).
    fn round_trip(src: &str) -> String {
        let obj = parse_document(src.as_bytes())
            .unwrap_or_else(|e| panic!("parse {src:?}: {e}"))
            .expect("a top-level object");
        marshal_indent(&obj)
    }

    /// The reference output was produced by a real `go run` of
    /// `json.NewDecoder(...).UseNumber()` + `json.MarshalIndent(m, "", "  ")` over
    /// this exact input (plan 009 §8: pin what Go DOES, not what the docs say).
    #[test]
    fn matches_go_marshal_indent_byte_for_byte() {
        let src = r#"{"b": 1e10, "a": 0.10, "neg": -0, "big": 18446744073709551615,
            "huge": 9007199254740993, "s": "a<b>c&d e f\"g\\h\ti\njk",
            "uni": "héllo 世界 🎉", "u2028": "x\u2028y\u2029z",
            "emptyObj": {}, "emptyArr": [],
            "arr": [1, "two", null, true, {"z":1,"a":2}],
            "nested": {"zz": {"yy": 3.0, "aa": [ ]}},
            "nul": null, "A": 1, "_z": 2, "": 3}"#;
        let want = concat!(
            "{\n",
            "  \"\": 3,\n",
            "  \"A\": 1,\n",
            "  \"_z\": 2,\n",
            "  \"a\": 0.10,\n",
            "  \"arr\": [\n",
            "    1,\n",
            "    \"two\",\n",
            "    null,\n",
            "    true,\n",
            "    {\n",
            "      \"a\": 2,\n",
            "      \"z\": 1\n",
            "    }\n",
            "  ],\n",
            "  \"b\": 1e10,\n",
            "  \"big\": 18446744073709551615,\n",
            "  \"emptyArr\": [],\n",
            "  \"emptyObj\": {},\n",
            "  \"huge\": 9007199254740993,\n",
            "  \"neg\": -0,\n",
            "  \"nested\": {\n",
            "    \"zz\": {\n",
            "      \"aa\": [],\n",
            "      \"yy\": 3.0\n",
            "    }\n",
            "  },\n",
            "  \"nul\": null,\n",
            "  \"s\": \"a\\u003cb\\u003ec\\u0026d e f\\\"g\\\\h\\ti\\njk\",\n",
            "  \"u2028\": \"x\\u2028y\\u2029z\",\n",
            "  \"uni\": \"héllo 世界 🎉\"\n",
            "}"
        );
        assert_eq!(round_trip(src), want);
    }

    #[test]
    fn numbers_keep_their_raw_literal() {
        // Every one of these is corrupted by an f64 round-trip (or by an integer
        // one): the >2^53 ints lose precision, `0.10` loses its trailing zero,
        // `1e10` is re-rendered as 10000000000, `-0` as 0.
        for literal in [
            "1e10",
            "1E+10",
            "0.10",
            "-0",
            "9007199254740993",
            "18446744073709551615",
            "3.0",
            "1.7976931348623157e308",
            "0.1000000000000000055511151231257827",
        ] {
            let src = format!("{{\"n\":{literal}}}");
            assert_eq!(
                round_trip(&src),
                format!("{{\n  \"n\": {literal}\n}}"),
                "literal {literal} did not survive the round trip"
            );
        }
    }

    #[test]
    fn escaping_table_matches_go() {
        let cases = [
            ("<", "\\u003c"),
            (">", "\\u003e"),
            ("&", "\\u0026"),
            ("\"", "\\\""),
            ("\\", "\\\\"),
            ("\u{8}", "\\b"),
            ("\u{c}", "\\f"),
            ("\n", "\\n"),
            ("\r", "\\r"),
            ("\t", "\\t"),
            // 0x08 and 0x0c are the only sub-0x20 bytes besides tab/LF/CR that
            // Go gives a two-character form; 0x0b sits BETWEEN them and does not.
            ("\u{1}", "\\u0001"),
            ("\u{b}", "\\u000b"),
            ("\u{1f}", "\\u001f"),
            // NOT escaped by Go: DEL, and any non-ASCII rune outside U+2028/29.
            ("\u{7f}", "\u{7f}"),
            ("é", "é"),
            ("世", "世"),
            ("\u{2028}", "\\u2028"),
            ("\u{2029}", "\\u2029"),
        ];
        for (raw, want) in cases {
            let mut out = String::new();
            write_string(&mut out, raw);
            assert_eq!(out, format!("\"{want}\""), "escaping {raw:?}");
        }
    }

    #[test]
    fn keys_are_escaped_and_sorted_at_every_level() {
        let src = r#"{"z":1,"a<b>":{"y":1,"&":2}}"#;
        assert_eq!(
            round_trip(src),
            concat!(
                "{\n",
                "  \"a\\u003cb\\u003e\": {\n",
                "    \"\\u0026\": 2,\n",
                "    \"y\": 1\n",
                "  },\n",
                "  \"z\": 1\n",
                "}"
            )
        );
    }

    #[test]
    fn empty_containers_stay_inline() {
        assert_eq!(round_trip("{}"), "{}");
        assert_eq!(
            round_trip(r#"{"a":[],"b":{}}"#),
            "{\n  \"a\": [],\n  \"b\": {}\n}"
        );
    }

    #[test]
    fn unicode_escapes_decode_like_go() {
        // A surrogate PAIR combines; a lone half becomes U+FFFD (Go's rule).
        assert_eq!(
            round_trip(r#"{"a":"\ud83c\udf89","b":"\ud800","c":"\u0041\u00e9"}"#),
            "{\n  \"a\": \"🎉\",\n  \"b\": \"\u{fffd}\",\n  \"c\": \"Aé\"\n}"
        );
    }

    #[test]
    fn invalid_utf8_becomes_one_replacement_per_byte() {
        // Go: utf8.DecodeRune -> (RuneError, 1) per bad byte. `A\xff\xffB` is
        // therefore TWO replacement chars, not one.
        let raw = b"{\"k\":\"A\xff\xffB\"}";
        let obj = parse_document(raw).unwrap().unwrap();
        assert_eq!(
            obj.get("k"),
            Some(&GoValue::String("A\u{fffd}\u{fffd}B".to_string()))
        );
    }

    #[test]
    fn top_level_null_is_a_reseed_not_an_error() {
        assert_eq!(parse_document(b"null"), Ok(None));
        assert_eq!(parse_document(b"  null  "), Ok(None));
    }

    #[test]
    fn non_object_top_level_is_an_error() {
        for src in [r#"[1]"#, r#""x""#, "42", "true"] {
            assert!(
                parse_document(src.as_bytes()).is_err(),
                "{src} should not decode into an object"
            );
        }
    }

    #[test]
    fn trailing_content_is_refused() {
        for src in [
            r#"{"theme":"dark"}{"extra":true}"#,
            r#"{"theme":"dark"} garbage after"#,
            r#"{"theme":"dark"} null"#,
        ] {
            let err = parse_document(src.as_bytes()).unwrap_err();
            assert!(
                err.contains("trailing data"),
                "{src} -> {err} (want the trailing-data refusal)"
            );
        }
        // …but trailing WHITESPACE is fine (Go's decoder skips it before EOF).
        assert!(parse_document(b"{\"a\":1}\n\t ").is_ok());
    }

    #[test]
    fn malformed_documents_are_errors() {
        for src in [
            "{ this is not json",
            "{\"a\":}",
            "{\"a\" 1}",
            "{'a':1}",
            "{\"a\":01}",
            "{\"a\":1,}",
            "[",
            "",
            "{\"a\":\"unterminated",
            "{\"a\":\"raw\nnewline\"}",
        ] {
            assert!(
                parse_document(src.as_bytes()).is_err(),
                "{src:?} should be rejected"
            );
        }
    }

    #[test]
    fn duplicate_keys_keep_the_last() {
        assert_eq!(round_trip(r#"{"a":1,"a":2}"#), "{\n  \"a\": 2\n}");
    }

    /// A document nested `depth` containers deep: one object frame wrapping
    /// `depth - 1` array frames.
    fn nested_doc(depth: usize) -> String {
        format!(
            "{{\"a\":{}{}}}",
            "[".repeat(depth - 1),
            "]".repeat(depth - 1)
        )
    }

    /// Run `body` on a thread with a stack big enough for a full-depth parse.
    ///
    /// A cargo-test thread gets 2 MiB — far less than [`MAX_NESTING_DEPTH`]
    /// frames need — and overflowing it would ABORT the whole test binary
    /// instead of failing one cell, so the deepest-accepted assertions cannot
    /// run on the default stack. Production has the same problem and solves it
    /// the same way (`preseed::dispatch`).
    fn on_a_big_stack<T: Send + 'static>(body: impl FnOnce() -> T + Send + 'static) -> T {
        std::thread::Builder::new()
            .stack_size(64 << 20)
            .spawn(body)
            .expect("spawning the deep-parse thread")
            .join()
            .expect("the deep-parse thread panicked")
    }

    #[test]
    fn nesting_is_capped_exactly_where_go_caps_it() {
        on_a_big_stack(|| {
            // Go accepts 10000 frames...
            assert!(
                parse_document(nested_doc(MAX_NESTING_DEPTH).as_bytes()).is_ok(),
                "depth {MAX_NESTING_DEPTH} is the deepest Go ACCEPTS"
            );
            // ...and refuses the next one, naming the opening byte.
            assert_eq!(
                parse_document(nested_doc(MAX_NESTING_DEPTH + 1).as_bytes()).unwrap_err(),
                "invalid character '[' exceeded max depth"
            );
            // An all-object chain reports its own opener, like Go's.
            let objects = format!(
                "{}1{}",
                r#"{"a":"#.repeat(MAX_NESTING_DEPTH + 1),
                "}".repeat(MAX_NESTING_DEPTH + 1)
            );
            assert_eq!(
                parse_document(objects.as_bytes()).unwrap_err(),
                "invalid character '{' exceeded max depth"
            );
        });
    }

    #[test]
    fn a_pathologically_deep_document_refuses_instead_of_aborting() {
        // The point of the cap: an input this deep used to blow the stack
        // (SIGABRT, whole process) instead of returning. The bail-out is at
        // 10001, so the recursion is bounded no matter how deep the input goes —
        // which is what makes a FIXED 64 MiB stack a sufficient answer.
        on_a_big_stack(|| {
            let err = parse_document(nested_doc(200_000).as_bytes()).unwrap_err();
            assert_eq!(err, "invalid character '[' exceeded max depth");
        });
    }

    #[test]
    fn number_int_reads_the_raw_literal() {
        assert_eq!(GoValue::Number("999".into()).number_int(), 999);
        assert_eq!(GoValue::Number("100000".into()).number_int(), 100000);
        // Not an integer (or not a number at all) → 0, like Go's json.Number.Int64
        // failing and jsonNumberInt returning the zero value.
        assert_eq!(GoValue::Number("1.5".into()).number_int(), 0);
        assert_eq!(GoValue::Number("1e10".into()).number_int(), 0);
        assert_eq!(GoValue::Bool(true).number_int(), 0);
    }

    #[test]
    fn shape_names_match_go() {
        assert_eq!(GoValue::Null.shape(), "null");
        assert_eq!(GoValue::Object(GoObject::new()).shape(), "object");
        assert_eq!(GoValue::Array(vec![]).shape(), "array");
        assert_eq!(GoValue::String(String::new()).shape(), "string");
        assert_eq!(GoValue::Bool(false).shape(), "boolean");
        assert_eq!(GoValue::Number("1".into()).shape(), "number");
    }
}
