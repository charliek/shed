//! The delivery stream's framing (`CONTRACT.md`, section 3): server-sent
//! events as the HTML standard defines them, read incrementally, and the
//! requests they carry. Pure: the caller owns the connection and feeds the
//! bytes it reads.

use crate::limits::EVENT_REQUEST;
use crate::request::{parse_request, Request};

/// The most bytes one event may take while it is read: its pending line,
/// its type and its data lines with the line feeds between them. A request
/// is bounded by the contract: its strings come to at most 3,774 bytes
/// (user, shed name and id 64 each, URL 2,048, owner/name and local
/// directory 255 each, project directory 1,024), which even written wholly
/// as six-byte `\u` escapes, with the members around them, is under 24 KiB.
/// Anything longer is not a request, and ends the stream.
pub const MAX_EVENT_BYTES: usize = 64 << 10;

/// One dispatched server-sent event.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Event {
    /// The event type (`message` when none was given).
    pub kind: String,
    /// Its data lines, joined with a line feed.
    pub data: Vec<u8>,
}

/// The stream broke its framing: an event (or a line) over
/// [`MAX_EVENT_BYTES`]. The reader keeps nothing of it and takes nothing
/// more; the caller ends the stream.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct FrameTooLarge;

impl std::fmt::Display for FrameTooLarge {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "a stream event is over {MAX_EVENT_BYTES} bytes")
    }
}

impl std::error::Error for FrameTooLarge {}

/// Reads server-sent events from bytes as they arrive: lines ended by LF,
/// CRLF or CR; comment lines ignored; `data` lines joined with LF; an event
/// dispatched at a blank line, and one with no data dropped. `id` and
/// `retry` are ignored (the contract's server writes neither).
#[derive(Debug, Default)]
pub struct EventReader {
    line: Vec<u8>,
    /// The previous byte was a CR: a LF right after it ends no new line.
    after_cr: bool,
    /// Past the stream's first bytes (and any byte order mark).
    started: bool,
    head: Vec<u8>,
    kind: String,
    data: Vec<u8>,
    has_data: bool,
    /// An event went over the bound: the stream is over.
    broken: bool,
}

impl EventReader {
    /// A reader at the start of a stream.
    pub fn new() -> EventReader {
        EventReader::default()
    }

    /// Feeds `bytes` and returns the events they complete.
    pub fn push(&mut self, bytes: &[u8]) -> Result<Vec<Event>, FrameTooLarge> {
        const BOM: &[u8] = b"\xEF\xBB\xBF";
        if self.broken {
            return Err(FrameTooLarge);
        }
        let mut out = Vec::new();
        let head;
        let bytes = if self.started {
            bytes
        } else {
            // One leading byte order mark is not part of the stream.
            self.head.extend_from_slice(bytes);
            if self.head.len() < BOM.len() && BOM.starts_with(&self.head) {
                return Ok(out);
            }
            self.started = true;
            head = std::mem::take(&mut self.head);
            head.strip_prefix(BOM).unwrap_or(&head)
        };
        for &b in bytes {
            match b {
                b'\n' if self.after_cr => self.after_cr = false,
                b'\n' | b'\r' => {
                    self.after_cr = b == b'\r';
                    let line = std::mem::take(&mut self.line);
                    if let Some(ev) = self.line_done(&line) {
                        out.push(ev);
                    }
                }
                _ => {
                    self.after_cr = false;
                    // The line, and the event it may join: its type, its
                    // data and the line feed that would go between.
                    let pending = self.kind.len() + self.data.len() + usize::from(self.has_data);
                    if self.line.len() + pending >= MAX_EVENT_BYTES {
                        self.broken = true;
                        self.line = Vec::new();
                        self.kind = String::new();
                        self.data = Vec::new();
                        return Err(FrameTooLarge);
                    }
                    self.line.push(b);
                }
            }
        }
        Ok(out)
    }

    fn line_done(&mut self, line: &[u8]) -> Option<Event> {
        if line.is_empty() {
            let ev = self.has_data.then(|| Event {
                kind: if self.kind.is_empty() {
                    "message".to_string()
                } else {
                    std::mem::take(&mut self.kind)
                },
                data: std::mem::take(&mut self.data),
            });
            self.kind.clear();
            self.data.clear();
            self.has_data = false;
            return ev;
        }
        if line[0] == b':' {
            return None;
        }
        let (field, mut value) = match line.iter().position(|&c| c == b':') {
            Some(i) => (&line[..i], &line[i + 1..]),
            None => (line, &line[line.len()..]),
        };
        if value.first() == Some(&b' ') {
            value = &value[1..];
        }
        match field {
            b"event" => self.kind = String::from_utf8_lossy(value).into_owned(),
            b"data" => {
                if self.has_data {
                    self.data.push(b'\n');
                }
                self.data.extend_from_slice(value);
                self.has_data = true;
            }
            _ => {}
        }
        None
    }
}

/// What a source makes of one event: a request to answer, a request it
/// cannot parse (and so does not answer; it ends at its deadline), or an
/// event of another type (ignored).
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum StreamItem {
    /// A request to answer.
    Request(Box<Request>),
    /// A `request` event whose data is not a valid request.
    Unreadable,
    /// An event of another type.
    Ignored,
}

/// Reads one event as the contract's source does.
pub fn read_event(ev: &Event) -> StreamItem {
    if ev.kind != EVENT_REQUEST {
        return StreamItem::Ignored;
    }
    match parse_request(&ev.data) {
        Ok(r) => StreamItem::Request(Box::new(r)),
        Err(_) => StreamItem::Unreadable,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn events(chunks: &[&[u8]]) -> Vec<Event> {
        let mut r = EventReader::new();
        let mut out = Vec::new();
        for c in chunks {
            out.extend(r.push(c).unwrap());
        }
        out
    }

    #[test]
    fn line_endings_and_fields() {
        let want = vec![
            Event {
                kind: "request".into(),
                data: b"a\nb".to_vec(),
            },
            Event {
                kind: "message".into(),
                data: b"".to_vec(),
            },
        ];
        assert_eq!(
            events(&[b"event: request\ndata: a\ndata:b\n\ndata\n\n"]),
            want
        );
        assert_eq!(
            events(&[b"event: request\r\ndata: a\r\ndata:b\r\n\r\ndata\r\n\r\n"]),
            want
        );
        assert_eq!(
            events(&[b"event: request\rdata: a\rdata:b\r\rdata\r\r"]),
            want
        );
        // CRLF split across chunks is one line end.
        assert_eq!(
            events(&[
                b"event: request\r",
                b"\ndata: a\r",
                b"\ndata:b\r\n\r",
                b"\ndata\r\n\r\n"
            ]),
            want
        );
    }

    #[test]
    fn comments_unknown_fields_and_no_data() {
        let evs =
            events(&[b": keepalive\n\nevent: x\nid: 1\nretry: 5\n\nevent: request\ndata: z\n\n"]);
        assert_eq!(
            evs,
            vec![Event {
                kind: "request".into(),
                data: b"z".to_vec()
            }]
        );
    }

    #[test]
    fn byte_order_mark_and_byte_by_byte() {
        let text = b"\xEF\xBB\xBFevent: request\ndata: q\n\n";
        let whole = events(&[text]);
        let split: Vec<&[u8]> = text.chunks(1).collect();
        assert_eq!(whole, events(&split));
        assert_eq!(whole.len(), 1);
        assert_eq!(whole[0].kind, "request");
    }

    #[test]
    fn long_line() {
        let mut r = EventReader::new();
        assert_eq!(r.push(&vec![b'a'; MAX_EVENT_BYTES + 1]), Err(FrameTooLarge));
        // Broken for good: nothing more is read.
        assert_eq!(
            r.push(b"\n\nevent: request\ndata: x\n\n"),
            Err(FrameTooLarge)
        );
    }

    #[test]
    fn an_event_of_many_short_lines_is_bounded() {
        let mut r = EventReader::new();
        let mut fed = 0usize;
        let err = loop {
            match r.push(b"data: abcdefgh\n") {
                Ok(evs) => assert!(evs.is_empty()),
                Err(e) => break e,
            }
            fed += 15;
            assert!(
                r.data.len() < MAX_EVENT_BYTES,
                "an unfinished event grew past the bound"
            );
            assert!(fed < 2 * MAX_EVENT_BYTES, "the bound never fired");
        };
        assert_eq!(err, FrameTooLarge);
        assert!(r.data.is_empty(), "the oversized event was kept");
        // Empty data lines add only their separators, and are bounded too.
        let mut r = EventReader::new();
        let many = b"data:\n".repeat(MAX_EVENT_BYTES);
        assert_eq!(r.push(&many), Err(FrameTooLarge));
    }

    #[test]
    fn the_largest_request_fits() {
        // Every string of a request at its limit, every byte written as a
        // six-byte escape where the alphabet allows one.
        let esc = |n: usize| "\\u003c".repeat(n);
        let body = format!(
            "{{\"version\":2,\"id\":\"0123456789abcdef0123456789abcdef\",\"reason\":\"provision\",\"deadline\":\"2026-11-23T10:00:30Z\",\"user\":\"{}\",\"shed\":{{\"name\":\"{}\",\"id\":\"{}\",\"backend\":\"firecracker\",\"repo\":{{\"url\":\"{}\",\"name\":\"{}/{}\"}},\"local_dir\":\"{}\",\"project_dir\":\"/{}\"}}}}",
            esc(64), esc(64), "q".repeat(64), esc(2048), esc(127), esc(127), esc(255), esc(1023)
        );
        let text = format!("event: request\ndata: {body}\n\n");
        assert!(text.len() < MAX_EVENT_BYTES, "{}", text.len());
        let evs = EventReader::new().push(text.as_bytes()).unwrap();
        assert!(matches!(read_event(&evs[0]), StreamItem::Request(_)));
    }
}
