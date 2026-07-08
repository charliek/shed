//! Server-Sent Events framing — ported from Swift's `SSEParser` + the hand-rolled
//! byte loop in `ShedServerClient.createShed`.
//!
//! `reqwest`'s `bytes_stream()` yields `Bytes` chunks, NOT lines, and an SSE
//! event can split across chunk boundaries (mid-line or mid-blank-line), so the
//! parser buffers bytes across `feed()` calls and dispatches on the blank line.
//!
//! Dialect (matches shed-server + shed-remote-agent):
//!   * `event:` sets the event type for the next dispatch
//!   * `data:` lines concatenate with newlines
//!   * a blank line dispatches the accumulated {event, data}
//!   * `:` lines are comments / keep-alive pings
//!   * a final record with no trailing blank line is flushed via `finish()`
//!   * `\r` is stripped (CRLF tolerance); invalid UTF-8 is lossy-decoded

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct SseEvent {
    pub event: String,
    pub data: String,
}

#[derive(Default)]
pub struct SseParser {
    event: String,
    data: String,
    line_buf: Vec<u8>,
}

impl SseParser {
    pub fn new() -> Self {
        Self::default()
    }

    /// Feed a chunk of bytes; returns any events completed within it. Bytes that
    /// don't complete a line are buffered for the next call.
    pub fn feed(&mut self, chunk: &[u8]) -> Vec<SseEvent> {
        let mut out = Vec::new();
        for &b in chunk {
            if b == b'\n' {
                if let Some(ev) = self.take_line() {
                    out.push(ev);
                }
            } else {
                self.line_buf.push(b);
            }
        }
        out
    }

    /// Flush a final record that lacked a trailing blank line (EOF).
    pub fn finish(&mut self) -> Vec<SseEvent> {
        let mut out = Vec::new();
        if !self.line_buf.is_empty() {
            if let Some(ev) = self.take_line() {
                out.push(ev);
            }
        }
        if let Some(ev) = self.flush() {
            out.push(ev);
        }
        out
    }

    fn take_line(&mut self) -> Option<SseEvent> {
        let line = String::from_utf8_lossy(&self.line_buf).into_owned();
        self.line_buf.clear();
        self.push_line(&line)
    }

    fn push_line(&mut self, raw: &str) -> Option<SseEvent> {
        let line = raw.strip_suffix('\r').unwrap_or(raw);
        if line.is_empty() {
            return self.flush();
        }
        if line.starts_with(':') {
            return None; // comment / keep-alive
        }
        if let Some(v) = line.strip_prefix("event:") {
            self.event = v.trim().to_string();
        } else if let Some(v) = line.strip_prefix("data:") {
            let v = v.trim();
            if self.data.is_empty() {
                self.data = v.to_string();
            } else {
                self.data.push('\n');
                self.data.push_str(v);
            }
        }
        None
    }

    fn flush(&mut self) -> Option<SseEvent> {
        if self.event.is_empty() && self.data.is_empty() {
            return None;
        }
        Some(SseEvent {
            event: std::mem::take(&mut self.event),
            data: std::mem::take(&mut self.data),
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn feed_all(chunks: &[&str]) -> Vec<SseEvent> {
        let mut p = SseParser::new();
        let mut out = Vec::new();
        for c in chunks {
            out.extend(p.feed(c.as_bytes()));
        }
        out.extend(p.finish());
        out
    }

    #[test]
    fn whole_event_in_one_chunk() {
        let events = feed_all(&["event: progress\ndata: hello\n\n"]);
        assert_eq!(
            events,
            vec![SseEvent {
                event: "progress".into(),
                data: "hello".into()
            }]
        );
    }

    #[test]
    fn split_mid_line() {
        // The event line is fragmented across two chunks.
        let events = feed_all(&["event: prog", "ress\ndata: hi\n\n"]);
        assert_eq!(
            events,
            vec![SseEvent {
                event: "progress".into(),
                data: "hi".into()
            }]
        );
    }

    #[test]
    fn split_mid_blank_line_dispatches() {
        // The dispatching blank line arrives in a later chunk (the subtle case).
        let events = feed_all(&["event: complete\ndata: {}\n", "\n"]);
        assert_eq!(
            events,
            vec![SseEvent {
                event: "complete".into(),
                data: "{}".into()
            }]
        );
    }

    #[test]
    fn two_events_one_chunk() {
        let events = feed_all(&["event: progress\ndata: a\n\nevent: progress\ndata: b\n\n"]);
        assert_eq!(
            events,
            vec![
                SseEvent {
                    event: "progress".into(),
                    data: "a".into()
                },
                SseEvent {
                    event: "progress".into(),
                    data: "b".into()
                },
            ]
        );
    }

    #[test]
    fn multiline_data_concatenates() {
        let events = feed_all(&["data: line1\ndata: line2\n\n"]);
        assert_eq!(
            events,
            vec![SseEvent {
                event: String::new(),
                data: "line1\nline2".into()
            }]
        );
    }

    #[test]
    fn comments_ignored_and_crlf_stripped() {
        let events = feed_all(&[": keep-alive\r\nevent: progress\r\ndata: x\r\n\r\n"]);
        assert_eq!(
            events,
            vec![SseEvent {
                event: "progress".into(),
                data: "x".into()
            }]
        );
    }

    #[test]
    fn finish_flushes_final_record_without_trailing_blank() {
        // No trailing blank line — finish() flushes the accumulated record.
        let events = feed_all(&["event: complete\ndata: {\"x\":1}"]);
        assert_eq!(
            events,
            vec![SseEvent {
                event: "complete".into(),
                data: "{\"x\":1}".into()
            }]
        );
    }

    #[test]
    fn byte_at_a_time_is_equivalent() {
        // Fragmenting to individual bytes must yield the same events.
        let whole = "event: progress\ndata: hi\n\nevent: complete\ndata: {}\n\n";
        let mut p = SseParser::new();
        let mut out = Vec::new();
        for b in whole.as_bytes() {
            out.extend(p.feed(&[*b]));
        }
        out.extend(p.finish());
        assert_eq!(out.len(), 2);
        assert_eq!(out[0].event, "progress");
        assert_eq!(out[1].event, "complete");
    }
}
