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

/// The bytes buffered for a single SSE event exceeded the configured cap
/// ([`SseParser::with_max_event_bytes`]). A caller that opted into a cap treats
/// this as a read error — disconnect + reconnect — mirroring Go's `bufio.Scanner`
/// token-too-long, which surfaces as a stream read error rather than growing memory
/// unboundedly for a hostile / never-terminating `data:` event.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct SseOverflow {
    /// The cap that was exceeded, in bytes.
    pub limit: usize,
}

impl std::fmt::Display for SseOverflow {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "SSE event exceeded {} bytes", self.limit)
    }
}

impl std::error::Error for SseOverflow {}

#[derive(Default)]
pub struct SseParser {
    event: String,
    data: String,
    line_buf: Vec<u8>,
    /// Optional cap on the bytes buffered for one in-flight event (line buffer +
    /// accumulated `data`). `None` (the default) is uncapped, so the existing
    /// consumers (`http.rs`'s create-stream reader) are unaffected.
    max_event_bytes: Option<usize>,
}

impl SseParser {
    pub fn new() -> Self {
        Self::default()
    }

    /// Cap the bytes buffered for a single SSE event. Once the in-flight line
    /// buffer plus accumulated `data` would exceed `max`, [`try_feed`] returns an
    /// [`SseOverflow`] so the caller can disconnect + reconnect instead of buffering
    /// unboundedly. Default (`new`/`default`) is uncapped — this is opt-in, so
    /// other consumers keep their current behavior.
    ///
    /// [`try_feed`]: Self::try_feed
    pub fn with_max_event_bytes(mut self, max: usize) -> Self {
        self.max_event_bytes = Some(max);
        self
    }

    /// Feed a chunk of bytes; returns any events completed within it. Bytes that
    /// don't complete a line are buffered for the next call.
    ///
    /// Infallible framing — the cap ([`with_max_event_bytes`]) is not enforced on
    /// this path (the `http.rs` create-stream reader sets no cap). Callers that opt
    /// into a cap should use [`try_feed`], which surfaces the overflow instead of
    /// silently dropping the in-flight event.
    ///
    /// [`with_max_event_bytes`]: Self::with_max_event_bytes
    /// [`try_feed`]: Self::try_feed
    pub fn feed(&mut self, chunk: &[u8]) -> Vec<SseEvent> {
        // With no cap set, `feed_inner` never errs → identical to the pre-cap `feed`.
        self.feed_inner(chunk).unwrap_or_default()
    }

    /// Like [`feed`], but enforces the [`with_max_event_bytes`] cap: if the bytes
    /// buffered for the in-flight event exceed the cap, returns an [`SseOverflow`]
    /// (the caller should disconnect + reconnect). With no cap set this never errs.
    ///
    /// [`feed`]: Self::feed
    /// [`with_max_event_bytes`]: Self::with_max_event_bytes
    pub fn try_feed(&mut self, chunk: &[u8]) -> Result<Vec<SseEvent>, SseOverflow> {
        self.feed_inner(chunk)
    }

    fn feed_inner(&mut self, chunk: &[u8]) -> Result<Vec<SseEvent>, SseOverflow> {
        let mut out = Vec::new();
        for &b in chunk {
            if b == b'\n' {
                if let Some(ev) = self.take_line() {
                    out.push(ev);
                }
            } else {
                self.line_buf.push(b);
            }
            if let Some(max) = self.max_event_bytes {
                // Bound total in-flight memory: the current line plus the data
                // accumulated across `data:` lines awaiting a dispatching blank line.
                if self.line_buf.len() + self.data.len() > max {
                    return Err(SseOverflow { limit: max });
                }
            }
        }
        Ok(out)
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
    fn uncapped_by_default_buffers_large_events() {
        // The default parser (the http.rs create-stream path) has no cap: a large
        // event is buffered and dispatched intact — behavior is unchanged.
        let mut p = SseParser::new();
        let big = "x".repeat(4 * 1024 * 1024); // 4 MiB
        let input = format!("data: {big}\n\n");
        let events = p.feed(input.as_bytes());
        assert_eq!(events.len(), 1);
        assert_eq!(events[0].data, big);
    }

    #[test]
    fn try_feed_caps_a_runaway_data_line() {
        // A never-terminating `data:` line (no newline) that exceeds the cap must
        // surface an overflow rather than growing the line buffer unboundedly.
        let mut p = SseParser::new().with_max_event_bytes(64);
        let runaway = format!("data: {}", "x".repeat(1024)); // no trailing '\n'
        let err = p
            .try_feed(runaway.as_bytes())
            .expect_err("a >cap data line must overflow");
        assert_eq!(err.limit, 64);
        assert_eq!(err.to_string(), "SSE event exceeded 64 bytes");
    }

    #[test]
    fn try_feed_caps_across_many_data_lines() {
        // Many small `data:` lines with no dispatching blank line accumulate in
        // `data`; the cap covers that accumulation, not just a single long line.
        let mut p = SseParser::new().with_max_event_bytes(64);
        let mut hit = false;
        for _ in 0..100 {
            if p.try_feed(b"data: aaaaaaaaaa\n").is_err() {
                hit = true;
                break;
            }
        }
        assert!(hit, "accumulated data: lines past the cap must overflow");
    }

    #[test]
    fn try_feed_under_cap_dispatches_normally() {
        // Below the cap, try_feed behaves exactly like feed.
        let mut p = SseParser::new().with_max_event_bytes(1024);
        let events = p.try_feed(b"event: progress\ndata: hi\n\n").unwrap();
        assert_eq!(
            events,
            vec![SseEvent {
                event: "progress".into(),
                data: "hi".into()
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
