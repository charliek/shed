//! craze's `agent.DisplayLine` (`internal/agent/displayline.go` and
//! `strip.go`, craze v0.2.0), ported exactly: the one rule craze folds a
//! string onto a line by, which its `/effort` refusal runs the typed word and
//! every level through (plan 032 §3.2, PC9: the refusal text is ported
//! exactly). An escape sequence goes WHOLE — `ESC[31mbogusESC[0m` is `bogus`,
//! not `[31mbogus[0m` — so this carries craze's transcription of the
//! terminal parser it strips by, not just a control filter.
//!
//! The rule: a clean line (printable ASCII words, single inner spaces, none at
//! either end) comes back as it was; anything else is `stripANSI`, then every
//! control dropped (a newline, carriage return or tab first read as a space),
//! then the whitespace-separated words joined by single spaces. The strip
//! works on BYTES, as craze's does, so its few odd edges are craze's too: a
//! C1 control spelled in UTF-8 (`U+009B`) is a character, not a CSI, and the
//! control filter drops it; a string sequence (OSC, DCS) that ends inside a
//! multi-byte character can leave a stray continuation byte, which reads as
//! one `U+FFFD` per byte, as Go's `range` over a string reads it.
//!
//! `stripANSI` and its transition table are, in craze's words, "ansi.Strip from
//! github.com/charmbracelet/x/ansi v0.10.1 (width.go, parser.go's utf8ByteLen,
//! and parser/transition_table.go), transcribed". This is that transcription
//! again, statement for statement, under the same license:
//!
//! > MIT License
//! >
//! > Copyright (c) 2023 Charmbracelet, Inc.
//! >
//! > Permission is hereby granted, free of charge, to any person obtaining a
//! > copy of this software and associated documentation files (the
//! > "Software"), to deal in the Software without restriction, including
//! > without limitation the rights to use, copy, modify, merge, publish,
//! > distribute, sublicense, and/or sell copies of the Software, and to permit
//! > persons to whom the Software is furnished to do so, subject to the
//! > following conditions:
//! >
//! > The above copyright notice and this permission notice shall be included
//! > in all copies or substantial portions of the Software.
//! >
//! > THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS
//! > OR IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF
//! > MERCHANTABILITY, FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN
//! > NO EVENT SHALL THE AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM,
//! > DAMAGES OR OTHER LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR
//! > OTHERWISE, ARISING FROM, OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE
//! > USE OR OTHER DEALINGS IN THE SOFTWARE.

/// craze's `DisplayLine`: `s` folded onto one clean line (the module doc).
pub(super) fn display_line(s: &str) -> String {
    if s.is_empty() {
        return String::new();
    }
    if is_clean_line(s) {
        return s.to_string();
    }
    let kept = drop_controls(&strip_ansi(s));
    kept.split_whitespace().collect::<Vec<_>>().join(" ")
}

/// craze's `displayCleanLine`: printable ASCII with single inner spaces and
/// none at either end — a string the full path returns unchanged. `s` is not
/// empty.
fn is_clean_line(s: &str) -> bool {
    let bytes = s.as_bytes();
    if bytes[0] == b' ' || bytes[bytes.len() - 1] == b' ' {
        return false;
    }
    let mut prev_space = false;
    for &c in bytes {
        match c {
            b' ' if prev_space => return false,
            b' ' => prev_space = true,
            0x21..=0x7e => prev_space = false,
            _ => return false,
        }
    }
    true
}

/// craze's `displayDropControls`: a newline, carriage return or tab becomes a
/// space and every other control code point (the whole Cc category — Go's
/// `unicode.IsControl` is exactly Rust's [`char::is_control`]) goes. It reads
/// `bytes` as Go's `range` reads a string ([`GoRunes`]).
fn drop_controls(bytes: &[u8]) -> String {
    let mut out = String::with_capacity(bytes.len());
    for c in (GoRunes { rest: bytes }) {
        match c {
            '\n' | '\r' | '\t' => out.push(' '),
            c if c.is_control() => {}
            c => out.push(c),
        }
    }
    out
}

/// `bytes` decoded as Go's `for _, r := range s` decodes a string: each valid
/// UTF-8 sequence its character, and each byte that does not start one a
/// `U+FFFD` of its own (Go's `utf8.DecodeRuneInString` answers `(RuneError,
/// 1)` for any invalid encoding) — not Rust's lossy decode, which replaces a
/// maximal invalid prefix once.
struct GoRunes<'a> {
    rest: &'a [u8],
}

impl Iterator for GoRunes<'_> {
    type Item = char;

    fn next(&mut self) -> Option<char> {
        let first = *self.rest.first()?;
        let width = match first {
            0x00..=0x7f => 1,
            0xc2..=0xdf => 2,
            0xe0..=0xef => 3,
            0xf0..=0xf4 => 4,
            _ => 0,
        };
        let decoded = self
            .rest
            .get(..width)
            .filter(|_| width > 0)
            .and_then(|seq| std::str::from_utf8(seq).ok())
            .and_then(|seq| seq.chars().next());
        match decoded {
            Some(c) => {
                self.rest = &self.rest[width..];
                Some(c)
            }
            None => {
                self.rest = &self.rest[1..];
                Some(char::REPLACEMENT_CHARACTER)
            }
        }
    }
}

// The DEC ANSI parser's states and actions (parser/const.go).
const GROUND: u8 = 0;
const CSI_ENTRY: u8 = 1;
const CSI_INTERMEDIATE: u8 = 2;
const CSI_PARAM: u8 = 3;
const DCS_ENTRY: u8 = 4;
const DCS_INTERMEDIATE: u8 = 5;
const DCS_PARAM: u8 = 6;
const DCS_STRING: u8 = 7;
const ESCAPE: u8 = 8;
const ESCAPE_INTERMEDIATE: u8 = 9;
const OSC_STRING: u8 = 10;
const SOS_STRING: u8 = 11;
const PM_STRING: u8 = 12;
const APC_STRING: u8 = 13;
const UTF8: u8 = 14;

const NONE: u8 = 0;
const CLEAR: u8 = 1;
const COLLECT: u8 = 2;
const PREFIX: u8 = 3;
const DISPATCH: u8 = 4;
const EXECUTE: u8 = 5;
const START: u8 = 6;
const PUT: u8 = 7;
const PARAM: u8 = 8;
const PRINT: u8 = 9;
const IGNORE: u8 = NONE;

const ACTION_SHIFT: u8 = 4;
const STATE_MASK: u8 = 15;
const INDEX_SHIFT: usize = 8;
const TABLE_SIZE: usize = 4096;

/// `parser.Table`: index `state << 8 | byte`, value `action << 4 | next`.
type Table = [u8; TABLE_SIZE];

/// craze's `stripTable`, built at compile time.
static STRIP_TABLE: Table = strip_table();

const fn add_one(t: &mut Table, code: u8, state: u8, action: u8, next: u8) {
    t[((state as usize) << INDEX_SHIFT) | code as usize] = (action << ACTION_SHIFT) | next;
}

const fn add_many(t: &mut Table, codes: &[u8], state: u8, action: u8, next: u8) {
    let mut i = 0;
    while i < codes.len() {
        add_one(t, codes[i], state, action, next);
        i += 1;
    }
}

const fn add_range(t: &mut Table, start: u8, end: u8, state: u8, action: u8, next: u8) {
    let mut code = start as usize;
    while code <= end as usize {
        add_one(t, code as u8, state, action, next);
        code += 1;
    }
}

/// The next state and the action for `code` in `state`.
fn transition(state: u8, code: u8) -> (u8, u8) {
    let v = STRIP_TABLE[((state as usize) << INDEX_SHIFT) | code as usize];
    (v & STATE_MASK, v >> ACTION_SHIFT)
}

/// craze's `genStripTable` (`GenerateTransitionTable`), statement for
/// statement.
const fn strip_table() -> Table {
    let mut t: Table = [(NONE << ACTION_SHIFT) | GROUND; TABLE_SIZE];

    // Anywhere
    let mut state = GROUND;
    while state <= UTF8 {
        // Anywhere -> Ground
        add_many(&mut t, &[0x18, 0x1a, 0x99, 0x9a], state, EXECUTE, GROUND);
        add_range(&mut t, 0x80, 0x8f, state, EXECUTE, GROUND);
        add_range(&mut t, 0x90, 0x97, state, EXECUTE, GROUND);
        add_one(&mut t, 0x9c, state, EXECUTE, GROUND);
        // Anywhere -> Escape
        add_one(&mut t, 0x1b, state, CLEAR, ESCAPE);
        // Anywhere -> SosStringState
        add_one(&mut t, 0x98, state, START, SOS_STRING);
        // Anywhere -> PmStringState
        add_one(&mut t, 0x9e, state, START, PM_STRING);
        // Anywhere -> ApcStringState
        add_one(&mut t, 0x9f, state, START, APC_STRING);
        // Anywhere -> CsiEntry
        add_one(&mut t, 0x9b, state, CLEAR, CSI_ENTRY);
        // Anywhere -> DcsEntry
        add_one(&mut t, 0x90, state, CLEAR, DCS_ENTRY);
        // Anywhere -> OscString
        add_one(&mut t, 0x9d, state, START, OSC_STRING);
        // Anywhere -> Utf8
        add_range(&mut t, 0xc2, 0xdf, state, COLLECT, UTF8); // UTF8 2 byte sequence
        add_range(&mut t, 0xe0, 0xef, state, COLLECT, UTF8); // UTF8 3 byte sequence
        add_range(&mut t, 0xf0, 0xf4, state, COLLECT, UTF8); // UTF8 4 byte sequence
        state += 1;
    }

    // Ground
    add_range(&mut t, 0x00, 0x17, GROUND, EXECUTE, GROUND);
    add_one(&mut t, 0x19, GROUND, EXECUTE, GROUND);
    add_range(&mut t, 0x1c, 0x1f, GROUND, EXECUTE, GROUND);
    add_range(&mut t, 0x20, 0x7e, GROUND, PRINT, GROUND);
    add_one(&mut t, 0x7f, GROUND, EXECUTE, GROUND);

    // EscapeIntermediate
    add_range(
        &mut t,
        0x00,
        0x17,
        ESCAPE_INTERMEDIATE,
        EXECUTE,
        ESCAPE_INTERMEDIATE,
    );
    add_one(
        &mut t,
        0x19,
        ESCAPE_INTERMEDIATE,
        EXECUTE,
        ESCAPE_INTERMEDIATE,
    );
    add_range(
        &mut t,
        0x1c,
        0x1f,
        ESCAPE_INTERMEDIATE,
        EXECUTE,
        ESCAPE_INTERMEDIATE,
    );
    add_range(
        &mut t,
        0x20,
        0x2f,
        ESCAPE_INTERMEDIATE,
        COLLECT,
        ESCAPE_INTERMEDIATE,
    );
    add_one(
        &mut t,
        0x7f,
        ESCAPE_INTERMEDIATE,
        IGNORE,
        ESCAPE_INTERMEDIATE,
    );
    // EscapeIntermediate -> Ground
    add_range(&mut t, 0x30, 0x7e, ESCAPE_INTERMEDIATE, DISPATCH, GROUND);

    // Escape
    add_range(&mut t, 0x00, 0x17, ESCAPE, EXECUTE, ESCAPE);
    add_one(&mut t, 0x19, ESCAPE, EXECUTE, ESCAPE);
    add_range(&mut t, 0x1c, 0x1f, ESCAPE, EXECUTE, ESCAPE);
    add_one(&mut t, 0x7f, ESCAPE, IGNORE, ESCAPE);
    // Escape -> Ground
    add_range(&mut t, 0x30, 0x4f, ESCAPE, DISPATCH, GROUND);
    add_range(&mut t, 0x51, 0x57, ESCAPE, DISPATCH, GROUND);
    add_one(&mut t, 0x59, ESCAPE, DISPATCH, GROUND);
    add_one(&mut t, 0x5a, ESCAPE, DISPATCH, GROUND);
    add_one(&mut t, 0x5c, ESCAPE, DISPATCH, GROUND);
    add_range(&mut t, 0x60, 0x7e, ESCAPE, DISPATCH, GROUND);
    // Escape -> Escape_intermediate
    add_range(&mut t, 0x20, 0x2f, ESCAPE, COLLECT, ESCAPE_INTERMEDIATE);
    // Escape -> Sos_pm_apc_string
    add_one(&mut t, b'X', ESCAPE, START, SOS_STRING); // SOS
    add_one(&mut t, b'^', ESCAPE, START, PM_STRING); // PM
    add_one(&mut t, b'_', ESCAPE, START, APC_STRING); // APC
                                                      // Escape -> Dcs_entry
    add_one(&mut t, b'P', ESCAPE, CLEAR, DCS_ENTRY);
    // Escape -> Csi_entry
    add_one(&mut t, b'[', ESCAPE, CLEAR, CSI_ENTRY);
    // Escape -> Osc_string
    add_one(&mut t, b']', ESCAPE, START, OSC_STRING);

    // Sos_pm_apc_string
    let mut state = SOS_STRING;
    while state <= APC_STRING {
        add_range(&mut t, 0x00, 0x17, state, PUT, state);
        add_one(&mut t, 0x19, state, PUT, state);
        add_range(&mut t, 0x1c, 0x1f, state, PUT, state);
        add_range(&mut t, 0x20, 0x7f, state, PUT, state);
        // ESC, ST, CAN, and SUB terminate the sequence
        add_one(&mut t, 0x1b, state, DISPATCH, ESCAPE);
        add_one(&mut t, 0x9c, state, DISPATCH, GROUND);
        add_many(&mut t, &[0x18, 0x1a], state, IGNORE, GROUND);
        state += 1;
    }

    // Dcs_entry
    add_range(&mut t, 0x00, 0x07, DCS_ENTRY, IGNORE, DCS_ENTRY);
    add_range(&mut t, 0x0e, 0x17, DCS_ENTRY, IGNORE, DCS_ENTRY);
    add_one(&mut t, 0x19, DCS_ENTRY, IGNORE, DCS_ENTRY);
    add_range(&mut t, 0x1c, 0x1f, DCS_ENTRY, IGNORE, DCS_ENTRY);
    add_one(&mut t, 0x7f, DCS_ENTRY, IGNORE, DCS_ENTRY);
    // Dcs_entry -> Dcs_intermediate
    add_range(&mut t, 0x20, 0x2f, DCS_ENTRY, COLLECT, DCS_INTERMEDIATE);
    // Dcs_entry -> Dcs_param
    add_range(&mut t, 0x30, 0x3b, DCS_ENTRY, PARAM, DCS_PARAM);
    add_range(&mut t, 0x3c, 0x3f, DCS_ENTRY, PREFIX, DCS_PARAM);
    // Dcs_entry -> Dcs_passthrough
    add_range(&mut t, 0x08, 0x0d, DCS_ENTRY, PUT, DCS_STRING); // Follows ECMA-48 § 8.3.27
                                                               // XXX: allows passing ESC (not a ECMA-48 standard) this to allow for
                                                               // passthrough of ANSI sequences like in Screen or Tmux passthrough mode.
    add_one(&mut t, 0x1b, DCS_ENTRY, PUT, DCS_STRING);
    add_range(&mut t, 0x40, 0x7e, DCS_ENTRY, START, DCS_STRING);

    // Dcs_intermediate
    add_range(
        &mut t,
        0x00,
        0x17,
        DCS_INTERMEDIATE,
        IGNORE,
        DCS_INTERMEDIATE,
    );
    add_one(&mut t, 0x19, DCS_INTERMEDIATE, IGNORE, DCS_INTERMEDIATE);
    add_range(
        &mut t,
        0x1c,
        0x1f,
        DCS_INTERMEDIATE,
        IGNORE,
        DCS_INTERMEDIATE,
    );
    add_range(
        &mut t,
        0x20,
        0x2f,
        DCS_INTERMEDIATE,
        COLLECT,
        DCS_INTERMEDIATE,
    );
    add_one(&mut t, 0x7f, DCS_INTERMEDIATE, IGNORE, DCS_INTERMEDIATE);
    // Dcs_intermediate -> Dcs_passthrough
    add_range(&mut t, 0x30, 0x3f, DCS_INTERMEDIATE, START, DCS_STRING);
    add_range(&mut t, 0x40, 0x7e, DCS_INTERMEDIATE, START, DCS_STRING);

    // Dcs_param
    add_range(&mut t, 0x00, 0x17, DCS_PARAM, IGNORE, DCS_PARAM);
    add_one(&mut t, 0x19, DCS_PARAM, IGNORE, DCS_PARAM);
    add_range(&mut t, 0x1c, 0x1f, DCS_PARAM, IGNORE, DCS_PARAM);
    add_range(&mut t, 0x30, 0x3b, DCS_PARAM, PARAM, DCS_PARAM);
    add_one(&mut t, 0x7f, DCS_PARAM, IGNORE, DCS_PARAM);
    add_range(&mut t, 0x3c, 0x3f, DCS_PARAM, IGNORE, DCS_PARAM);
    // Dcs_param -> Dcs_intermediate
    add_range(&mut t, 0x20, 0x2f, DCS_PARAM, COLLECT, DCS_INTERMEDIATE);
    // Dcs_param -> Dcs_passthrough
    add_range(&mut t, 0x40, 0x7e, DCS_PARAM, START, DCS_STRING);

    // Dcs_passthrough
    add_range(&mut t, 0x00, 0x17, DCS_STRING, PUT, DCS_STRING);
    add_one(&mut t, 0x19, DCS_STRING, PUT, DCS_STRING);
    add_range(&mut t, 0x1c, 0x1f, DCS_STRING, PUT, DCS_STRING);
    add_range(&mut t, 0x20, 0x7e, DCS_STRING, PUT, DCS_STRING);
    add_one(&mut t, 0x7f, DCS_STRING, PUT, DCS_STRING);
    // Allow Utf8 characters by extending the printable range from 0x7F to 0xFF
    add_range(&mut t, 0x80, 0xff, DCS_STRING, PUT, DCS_STRING);
    // ST, CAN, SUB, and ESC terminate the sequence
    add_one(&mut t, 0x1b, DCS_STRING, DISPATCH, ESCAPE);
    add_one(&mut t, 0x9c, DCS_STRING, DISPATCH, GROUND);
    add_many(&mut t, &[0x18, 0x1a], DCS_STRING, IGNORE, GROUND);

    // Csi_param
    add_range(&mut t, 0x00, 0x17, CSI_PARAM, EXECUTE, CSI_PARAM);
    add_one(&mut t, 0x19, CSI_PARAM, EXECUTE, CSI_PARAM);
    add_range(&mut t, 0x1c, 0x1f, CSI_PARAM, EXECUTE, CSI_PARAM);
    add_range(&mut t, 0x30, 0x3b, CSI_PARAM, PARAM, CSI_PARAM);
    add_one(&mut t, 0x7f, CSI_PARAM, IGNORE, CSI_PARAM);
    add_range(&mut t, 0x3c, 0x3f, CSI_PARAM, IGNORE, CSI_PARAM);
    // Csi_param -> Ground
    add_range(&mut t, 0x40, 0x7e, CSI_PARAM, DISPATCH, GROUND);
    // Csi_param -> Csi_intermediate
    add_range(&mut t, 0x20, 0x2f, CSI_PARAM, COLLECT, CSI_INTERMEDIATE);

    // Csi_intermediate
    add_range(
        &mut t,
        0x00,
        0x17,
        CSI_INTERMEDIATE,
        EXECUTE,
        CSI_INTERMEDIATE,
    );
    add_one(&mut t, 0x19, CSI_INTERMEDIATE, EXECUTE, CSI_INTERMEDIATE);
    add_range(
        &mut t,
        0x1c,
        0x1f,
        CSI_INTERMEDIATE,
        EXECUTE,
        CSI_INTERMEDIATE,
    );
    add_range(
        &mut t,
        0x20,
        0x2f,
        CSI_INTERMEDIATE,
        COLLECT,
        CSI_INTERMEDIATE,
    );
    add_one(&mut t, 0x7f, CSI_INTERMEDIATE, IGNORE, CSI_INTERMEDIATE);
    // Csi_intermediate -> Ground
    add_range(&mut t, 0x40, 0x7e, CSI_INTERMEDIATE, DISPATCH, GROUND);
    // Csi_intermediate -> Csi_ignore
    add_range(&mut t, 0x30, 0x3f, CSI_INTERMEDIATE, IGNORE, GROUND);

    // Csi_entry
    add_range(&mut t, 0x00, 0x17, CSI_ENTRY, EXECUTE, CSI_ENTRY);
    add_one(&mut t, 0x19, CSI_ENTRY, EXECUTE, CSI_ENTRY);
    add_range(&mut t, 0x1c, 0x1f, CSI_ENTRY, EXECUTE, CSI_ENTRY);
    add_one(&mut t, 0x7f, CSI_ENTRY, IGNORE, CSI_ENTRY);
    // Csi_entry -> Ground
    add_range(&mut t, 0x40, 0x7e, CSI_ENTRY, DISPATCH, GROUND);
    // Csi_entry -> Csi_intermediate
    add_range(&mut t, 0x20, 0x2f, CSI_ENTRY, COLLECT, CSI_INTERMEDIATE);
    // Csi_entry -> Csi_param
    add_range(&mut t, 0x30, 0x3b, CSI_ENTRY, PARAM, CSI_PARAM);
    add_range(&mut t, 0x3c, 0x3f, CSI_ENTRY, PREFIX, CSI_PARAM);

    // Osc_string
    add_range(&mut t, 0x00, 0x06, OSC_STRING, IGNORE, OSC_STRING);
    add_range(&mut t, 0x08, 0x17, OSC_STRING, IGNORE, OSC_STRING);
    add_one(&mut t, 0x19, OSC_STRING, IGNORE, OSC_STRING);
    add_range(&mut t, 0x1c, 0x1f, OSC_STRING, IGNORE, OSC_STRING);
    // Allow Utf8 characters by extending the printable range from 0x7F to 0xFF
    add_range(&mut t, 0x20, 0xff, OSC_STRING, PUT, OSC_STRING);

    // ST, CAN, SUB, ESC, and BEL terminate the sequence
    add_one(&mut t, 0x1b, OSC_STRING, DISPATCH, ESCAPE);
    add_one(&mut t, 0x07, OSC_STRING, DISPATCH, GROUND);
    add_one(&mut t, 0x9c, OSC_STRING, DISPATCH, GROUND);
    add_many(&mut t, &[0x18, 0x1a], OSC_STRING, IGNORE, GROUND);

    t
}

/// craze's `utf8ByteLen` (parser.go's): the length a UTF-8 sequence's first
/// byte announces — 0 for a byte that starts none (craze's -1; either way no
/// count reaches it).
fn utf8_byte_len(b: u8) -> usize {
    match b {
        0x00..=0x7f => 1,
        0xc0..=0xdf => 2,
        0xe0..=0xef => 3,
        0xf0..=0xf7 => 4,
        _ => 0,
    }
}

/// craze's `stripANSI` (`ansi.Strip`): the escape sequences removed, keeping
/// printable characters and the executed controls (which [`drop_controls`]
/// deals with next). Bytes in, bytes out, as craze's.
fn strip_ansi(s: &str) -> Vec<u8> {
    let mut buf = Vec::with_capacity(s.len()); // collects printable characters
    let mut ri = 0; // rune index
    let mut rw = 0; // rune width
    let mut pstate = GROUND;
    // This implements a subset of the Parser to only collect runes and
    // printable characters.
    for &b in s.as_bytes() {
        if pstate == UTF8 {
            // During this state, collect rw bytes to form a valid rune in the
            // buffer. After getting all the rune bytes into the buffer,
            // transition to GroundState and reset the counters.
            buf.push(b);
            ri += 1;
            if ri < rw {
                continue;
            }
            pstate = GROUND;
            ri = 0;
            rw = 0;
            continue;
        }

        let (state, action) = transition(pstate, b);
        match action {
            // This action happens when we transition to the Utf8State.
            COLLECT if state == UTF8 => {
                rw = utf8_byte_len(b);
                buf.push(b);
                ri += 1;
            }
            // collects printable ASCII and non-printable characters
            PRINT | EXECUTE => buf.push(b),
            _ => {}
        }

        // Transition to the next state (the Utf8State is managed above).
        pstate = state;
    }
    buf
}
