//! The JSONL line tailer — a port of `internal/ext/rc/watch_tail.go`.
//!
//! [`LineTailer`] is an offset-tracked, resilient JSONL tailer over a single
//! file — the shared engine both the codex rollout watcher and the claude
//! transcript watcher sit on. It is deliberately pure I/O + buffering (no
//! fsnotify, no parsing): a caller drives it with [`LineTailer::poll`] — from
//! the hub's reconcile tick and/or an fsnotify nudge — and folds the returned
//! complete lines. This keeps it unit-testable against real temp files with
//! manual poll calls (no filesystem-notification flake).
//!
//! It handles the ways an append-only agent log misbehaves in practice:
//!
//! - **Partial lines**: a poll can land mid-write; only bytes up to the last
//!   `\n` are emitted, the trailing fragment is buffered for the next poll (a
//!   half-written JSON object is never handed to the parser).
//! - **Oversized lines**: a single line longer than [`TAIL_MAX_LINE`]
//!   (corrupt / pathological) is skipped rather than buffered without bound.
//! - **Truncation-in-place**: the file shrinking below our offset (a rewrite)
//!   resets the read to the new start and signals a reset so the fold clears
//!   stale state.
//! - **Rotation / inode swap**: the path pointing at a NEW inode
//!   (rename+recreate) or the file briefly vanishing (rotated away) reopens
//!   from the new file's start.
//! - **Permission / transient errors**: surfaced to the caller, which retains
//!   its prior verdict and retries on the next poll (open is re-attempted).
//!
//! The first successful open honors `catch_up`: with it set, a bounded window
//! from the end of the file is read so a just-correlated session's CURRENT
//! activity is established without parsing the whole history; without it, the
//! tailer seeks to EOF and only follows new appends (the ambiguous-correlation
//! path, where trusting history would risk reading the wrong session's file).

use std::fs::File;
use std::io::{self, Read, Seek, SeekFrom};
use std::os::unix::fs::{FileExt, MetadataExt};
use std::time::SystemTime;

/// Bounds the initial read on attach (`tailCatchUpWindow`,
/// `watch_tail.go:56`): at most this many bytes from the end of the file are
/// read to establish the current activity before following new appends, so a
/// large historical transcript is not fully parsed on correlate.
pub const TAIL_CATCH_UP_WINDOW: u64 = 64 * 1024;
/// Caps a single JSONL line (`tailMaxLine`, `watch_tail.go:59`). A longer
/// line (corrupt / pathological record) is skipped rather than buffered
/// without bound.
pub const TAIL_MAX_LINE: usize = 1024 * 1024;
/// Bounds one read of appended bytes (`tailReadChunk`, `watch_tail.go:61`).
const TAIL_READ_CHUNK: usize = 256 * 1024;
/// How many leading bytes are remembered to detect a same-size rewrite (see
/// [`LineTailer::poll`]) — `tailHeaderLen`, `watch_tail.go:65`. Small on
/// purpose: this is a cheap tripwire, not a hash of the file.
const TAIL_HEADER_LEN: usize = 256;

/// One poll's outcome. Go's `(lines, didReset, gapped, err)` multi-return —
/// kept together because the caller consumes `did_reset`/`gapped` EVEN on an
/// errored poll (a reset detected before a failed re-stat must still clear the
/// fold).
#[derive(Debug, Default)]
pub struct TailPoll {
    /// The complete lines appended since the last poll.
    pub lines: Vec<Vec<u8>>,
    /// The tailer detected a truncation or rotation and reset its position —
    /// the caller must clear any accumulated fold state.
    pub did_reset: bool,
    /// An oversized line was SKIPPED since the last poll (a record was lost
    /// mid-stream — the caller's fold should drop any state that depended on
    /// seeing every record, e.g. pending tool-call ids).
    pub gapped: bool,
    /// A non-`None` error (open failure, stat failure, read failure) means the
    /// caller should retain its prior verdict; the next poll re-attempts the
    /// open.
    pub err: Option<io::Error>,
}

/// The tailer's persistent state (`lineTailer`, `watch_tail.go:37`).
pub struct LineTailer {
    path: String,
    catch_up: bool,

    f: Option<File>,
    inode: u64,
    /// Bytes consumed from the current file.
    offset: u64,
    /// Trailing partial-line bytes (after the last `\n`).
    buf: Vec<u8>,
    /// A first open has happened (subsequent opens are rotation reopens).
    started: bool,
    /// Drop bytes until the next `\n` (mid-file seek / oversized recovery).
    skip_to_nl: bool,
    /// An oversized line was skipped since the last poll (record lost).
    saw_gap: bool,
    /// The file's first bytes at (re)open — the same-size-rewrite detector.
    hdr: Vec<u8>,
    /// Last observed mtime (arms the same-size header check).
    mtime: Option<SystemTime>,
}

impl LineTailer {
    pub fn new(path: impl Into<String>, catch_up: bool) -> LineTailer {
        LineTailer {
            path: path.into(),
            catch_up,
            f: None,
            inode: 0,
            offset: 0,
            buf: Vec::new(),
            started: false,
            skip_to_nl: false,
            saw_gap: false,
            hdr: Vec::new(),
            mtime: None,
        }
    }

    /// Advances the tailer and returns the complete lines appended since the
    /// last poll (`(*lineTailer).poll`, `watch_tail.go:75`). See [`TailPoll`]
    /// for the reset/gap/error contract.
    pub fn poll(&mut self) -> TailPoll {
        let mut out = TailPoll::default();

        if self.f.is_none() {
            if self.started {
                // A reopen after the file vanished (rotation): read the new
                // file from start.
                if let Err(err) = self.open_from(0) {
                    out.err = Some(err);
                    return out;
                }
                out.did_reset = true;
            } else {
                if let Err(err) = self.open_initial() {
                    out.err = Some(err);
                    return out;
                }
                self.started = true;
            }
        }

        let mut fi = match std::fs::metadata(&self.path) {
            Ok(fi) => fi,
            Err(err) => {
                // The path is gone (rotated away). Drop the handle; the next
                // poll reopens.
                self.close_file();
                out.gapped = self.take_gap();
                out.err = Some(err);
                return out;
            }
        };

        let ino = fi.ino();
        if self.inode != 0 && ino != 0 && ino != self.inode {
            // The path now points at a different inode (rename+recreate
            // rotation): reopen from the new file's start and treat it as a
            // reset.
            self.close_file();
            if let Err(err) = self.open_from(0) {
                out.did_reset = true;
                out.gapped = self.take_gap();
                out.err = Some(err);
                return out;
            }
            out.did_reset = true;
            fi = match std::fs::metadata(&self.path) {
                Ok(fi) => fi,
                Err(err) => {
                    self.close_file();
                    out.gapped = self.take_gap();
                    out.err = Some(err);
                    return out;
                }
            };
        }

        let size = fi.len();
        let mtime = fi.modified().ok();
        if size < self.offset {
            // Truncation in place: the file was rewritten shorter. Reset to
            // its new start.
            if let Err(err) = self.reset_to_start() {
                out.gapped = self.take_gap();
                out.err = Some(err);
                return out;
            }
            out.did_reset = true;
        } else if mtime != self.mtime && self.header_changed() {
            // In-place rewrite at OR past our offset: the size alone can't
            // reveal a rewrite that lands on exactly our offset, nor one that
            // rewrites the file and grows past it (which would otherwise be
            // mistaken for a plain append and read from the stale offset,
            // mixing old and new JSONL records). So whenever the mtime moved
            // and the file is at least as long as our offset, cheap-verify the
            // remembered leading bytes; a mismatch means new content → reset
            // and reread. Residual (accepted): a rewrite whose first
            // TAIL_HEADER_LEN bytes are also identical is indistinguishable
            // from an append and goes undetected.
            if let Err(err) = self.reset_to_start() {
                out.gapped = self.take_gap();
                out.err = Some(err);
                return out;
            }
            out.did_reset = true;
        }
        self.mtime = mtime;

        out.err = self.read_appended(&mut out.lines).err();
        out.gapped = self.take_gap();
        out
    }

    /// Rewinds the open file to byte 0, clears the partial buffer, and
    /// re-captures the header — the file's content is new (`resetToStart`,
    /// `watch_tail.go:141`).
    fn reset_to_start(&mut self) -> io::Result<()> {
        let f = self.f.as_mut().expect("reset_to_start with an open file");
        f.seek(SeekFrom::Start(0))?;
        self.offset = 0;
        self.buf.clear();
        self.skip_to_nl = false;
        self.capture_header();
        Ok(())
    }

    /// Returns-and-clears the oversized-skip flag — one report per poll
    /// (`takeGap`, `watch_tail.go:153`).
    fn take_gap(&mut self) -> bool {
        std::mem::take(&mut self.saw_gap)
    }

    /// Remembers the file's first [`TAIL_HEADER_LEN`] bytes (positioned reads —
    /// the tail's read offset is unaffected). Used by the same-size-rewrite
    /// check (`captureHeader`, `watch_tail.go:161`).
    fn capture_header(&mut self) {
        let mut buf = [0u8; TAIL_HEADER_LEN];
        let n = self.f.as_ref().map_or(0, |f| read_at_full(f, &mut buf, 0));
        self.hdr.clear();
        self.hdr.extend_from_slice(&buf[..n]);
    }

    /// Whether the file's current leading bytes differ from the remembered
    /// header (`headerChanged`, `watch_tail.go:169`).
    fn header_changed(&self) -> bool {
        let mut buf = vec![0u8; self.hdr.len()];
        let n = self.f.as_ref().map_or(0, |f| read_at_full(f, &mut buf, 0));
        buf[..n] != self.hdr[..]
    }

    /// Opens the file for the first time, honoring `catch_up` (bounded tail
    /// read) vs. seek-to-EOF (follow-only) — `openInitial`,
    /// `watch_tail.go:177`.
    fn open_initial(&mut self) -> io::Result<()> {
        let f = File::open(&self.path)?;
        let fi = f.metadata()?;
        self.inode = fi.ino();
        self.buf.clear();
        self.skip_to_nl = false;
        self.mtime = fi.modified().ok();
        let size = fi.len();

        let mut start = size; // follow-only default: seek to EOF
        if self.catch_up {
            start = 0;
            if size > TAIL_CATCH_UP_WINDOW {
                start = size - TAIL_CATCH_UP_WINDOW;
                // Seeking into the middle of the file usually lands mid-line,
                // so the first partial must be dropped — UNLESS the byte just
                // before the window is '\n', in which case the window starts
                // exactly on a line boundary and that first complete record
                // must be kept.
                self.skip_to_nl = true;
                let mut b = [0u8; 1];
                if read_at_full(&f, &mut b, start - 1) == 1 && b[0] == b'\n' {
                    self.skip_to_nl = false;
                }
            }
        }
        self.f = Some(f);
        self.capture_header();
        if let Err(err) = self
            .f
            .as_mut()
            .expect("just opened")
            .seek(SeekFrom::Start(start))
        {
            self.close_file();
            return Err(err);
        }
        self.offset = start;
        Ok(())
    }

    /// (Re)opens the file and seeks to `pos` (used for rotation reopens,
    /// always 0) — `openFrom`, `watch_tail.go:220`.
    fn open_from(&mut self, pos: u64) -> io::Result<()> {
        let mut f = File::open(&self.path)?;
        let fi = f.metadata()?;
        f.seek(SeekFrom::Start(pos))?;
        self.inode = fi.ino();
        self.offset = pos;
        self.buf.clear();
        self.skip_to_nl = false;
        self.mtime = fi.modified().ok();
        self.f = Some(f);
        self.capture_header();
        Ok(())
    }

    /// Reads from the current position to EOF, pushing every complete line and
    /// buffering the trailing partial (`readAppended`, `watch_tail.go:246`).
    fn read_appended(&mut self, out: &mut Vec<Vec<u8>>) -> io::Result<()> {
        let mut chunk = vec![0u8; TAIL_READ_CHUNK];
        loop {
            let n = match self
                .f
                .as_mut()
                .expect("read with an open file")
                .read(&mut chunk)
            {
                Ok(0) => return Ok(()), // EOF
                Ok(n) => n,
                Err(err) if err.kind() == io::ErrorKind::Interrupted => continue,
                Err(err) => return Err(err),
            };
            self.offset += n as u64;
            self.buf.extend_from_slice(&chunk[..n]);
            self.drain_lines(out);
        }
    }

    /// Splits complete lines out of the partial buffer, honoring the
    /// skip-to-newline (mid-file/oversized recovery) and oversized-line caps
    /// (`drainLines`, `watch_tail.go:269`). The trailing partial is compacted
    /// back into a small buffer so a long-lived tailer's buffer does not
    /// retain a large backing allocation.
    fn drain_lines(&mut self, out: &mut Vec<Vec<u8>>) {
        loop {
            let Some(i) = self.buf.iter().position(|&b| b == b'\n') else {
                if self.buf.len() > TAIL_MAX_LINE {
                    // The pending partial has blown the line cap without a
                    // newline: drop it and skip until the next newline (that
                    // oversized record is abandoned — a real record was lost,
                    // so flag the gap for the fold).
                    self.buf.clear();
                    self.skip_to_nl = true;
                    self.saw_gap = true;
                }
                return;
            };
            let line = &self.buf[..i];

            if self.skip_to_nl {
                // This newline closes a skipped region (mid-file seek or
                // oversized drop).
                self.skip_to_nl = false;
            } else if line.len() > TAIL_MAX_LINE {
                // Oversized complete line — skip it (a record was lost: flag
                // the gap).
                self.saw_gap = true;
            } else if !String::from_utf8_lossy(line).trim().is_empty() {
                out.push(line.to_vec());
            }

            // Compact the remaining bytes to the front of the buffer (Go's
            // `append(t.buf[:0], rest...)`) so the consumed prefix never
            // accumulates across polls.
            self.buf.drain(..=i);
        }
    }

    fn close_file(&mut self) {
        self.f = None;
    }

    /// Releases the underlying file (watcher teardown) — `close`,
    /// `watch_tail.go:313`.
    pub fn close(&mut self) {
        self.close_file();
    }

    /// Whether the tailer currently holds an open handle (the closed-watcher
    /// no-op assertions use this; Go tests reach the field directly).
    #[cfg(test)]
    fn is_open(&self) -> bool {
        self.f.is_some()
    }
}

/// A full positioned read: loops `read_at` until `buf` is full, EOF, or an
/// error, returning the byte count. Go's `File.ReadAt` loops internally and
/// the tailer ignores its error, using the returned prefix — same posture.
fn read_at_full(f: &File, buf: &mut [u8], mut pos: u64) -> usize {
    let mut got = 0;
    while got < buf.len() {
        match f.read_at(&mut buf[got..], pos) {
            Ok(0) => break, // EOF
            Ok(n) => {
                got += n;
                pos += n as u64;
            }
            Err(err) if err.kind() == io::ErrorKind::Interrupted => continue,
            Err(_) => break,
        }
    }
    got
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::fs::{self, FileTimes, OpenOptions};
    use std::io::Write;
    use std::time::Duration;

    fn write_file(path: &std::path::Path, content: &str) {
        fs::write(path, content).expect("write file");
    }

    fn append_file(path: &std::path::Path, content: &str) {
        let mut f = OpenOptions::new()
            .append(true)
            .open(path)
            .expect("open for append");
        f.write_all(content.as_bytes()).expect("append");
    }

    /// Forces a distinct mtime in case the filesystem's timestamp granularity
    /// would hide a rewrite (Go's `os.Chtimes(+2s)`).
    fn bump_mtime(path: &std::path::Path) {
        let f = OpenOptions::new().append(true).open(path).expect("open");
        let later = SystemTime::now() + Duration::from_secs(2);
        f.set_times(FileTimes::new().set_modified(later))
            .expect("set mtime");
    }

    fn lines_to_strings(lines: &[Vec<u8>]) -> Vec<String> {
        lines
            .iter()
            .map(|l| String::from_utf8_lossy(l).into_owned())
            .collect()
    }

    // Mirrors TestLineTailerPartialLineBuffering.
    #[test]
    fn partial_line_buffering() {
        let dir = tempfile::tempdir().expect("tempdir");
        let path = dir.path().join("t.jsonl");
        write_file(&path, "");
        let mut tl = LineTailer::new(path.to_str().unwrap(), false); // follow-only
        let p = tl.poll();
        assert!(p.err.is_none() && p.lines.is_empty(), "initial poll clean");
        // A half-written line must not be emitted.
        append_file(&path, r#"{"a":1"#);
        assert!(tl.poll().lines.is_empty(), "partial line must not emit");
        // Completing it emits exactly the whole line.
        append_file(&path, "}\n");
        assert_eq!(lines_to_strings(&tl.poll().lines), vec![r#"{"a":1}"#]);
    }

    // Mirrors TestLineTailerOversizedLineSkipped.
    #[test]
    fn oversized_line_skipped() {
        let dir = tempfile::tempdir().expect("tempdir");
        let path = dir.path().join("t.jsonl");
        write_file(&path, "");
        let mut tl = LineTailer::new(path.to_str().unwrap(), false);
        tl.poll(); // open at EOF

        let big = "x".repeat(TAIL_MAX_LINE + 10);
        append_file(&path, &format!("{big}\nsmall\n"));
        let p = tl.poll();
        assert!(p.err.is_none());
        assert_eq!(
            lines_to_strings(&p.lines),
            vec!["small"],
            "oversized skipped"
        );
        assert!(p.gapped, "an oversized skip must be reported as a gap");
        // The gap flag is one-shot: the next (quiet) poll reports no gap.
        assert!(!tl.poll().gapped, "gap must clear after being reported");
    }

    // Mirrors TestLineTailerTruncationResets.
    #[test]
    fn truncation_resets() {
        let dir = tempfile::tempdir().expect("tempdir");
        let path = dir.path().join("t.jsonl");
        write_file(&path, "a\nb\n");
        let mut tl = LineTailer::new(path.to_str().unwrap(), true);
        assert_eq!(tl.poll().lines.len(), 2, "catch-up read");
        // Rewrite shorter (truncation in place).
        write_file(&path, "c\n");
        let p = tl.poll();
        assert!(p.did_reset, "truncation should report did_reset");
        assert_eq!(lines_to_strings(&p.lines), vec!["c"]);
    }

    // Mirrors TestLineTailerInodeSwapResets.
    #[test]
    fn inode_swap_resets() {
        let dir = tempfile::tempdir().expect("tempdir");
        let path = dir.path().join("t.jsonl");
        write_file(&path, "a\n");
        let mut tl = LineTailer::new(path.to_str().unwrap(), true);
        assert_eq!(tl.poll().lines.len(), 1, "first read");
        // Replace the file with a new inode (remove + recreate).
        fs::remove_file(&path).expect("remove");
        write_file(&path, "x\n");
        let p = tl.poll();
        assert!(p.err.is_none(), "swap poll clean: {:?}", p.err);
        assert!(p.did_reset, "inode swap should report did_reset");
        assert_eq!(lines_to_strings(&p.lines), vec!["x"]);
    }

    // Mirrors TestLineTailerCatchUpBounded.
    #[test]
    fn catch_up_bounded() {
        let dir = tempfile::tempdir().expect("tempdir");
        let path = dir.path().join("t.jsonl");
        let pad = "p".repeat(1024);
        let mut content = String::new();
        for i in 0..200 {
            content.push_str(&format!("L{i:03}-{pad}\n")); // ~1KB/line → ~200KB > 64KB window
        }
        write_file(&path, &content);

        let mut tl = LineTailer::new(path.to_str().unwrap(), true);
        let p = tl.poll();
        assert!(p.err.is_none());
        let got = lines_to_strings(&p.lines);
        assert!(
            !got.is_empty() && got.len() < 200,
            "bounded tail, got {}",
            got.len()
        );
        // The most recent line is present; the very first is not.
        assert!(got.last().unwrap().starts_with("L199-"));
        assert!(!got[0].starts_with("L000-"), "oldest line excluded");
    }

    // Mirrors TestLineTailerPermissionErrorTolerant: a missing path is a poll
    // error, not a panic; a later create is picked up.
    #[test]
    fn missing_file_tolerant() {
        let dir = tempfile::tempdir().expect("tempdir");
        let path = dir.path().join("later.jsonl");
        let mut tl = LineTailer::new(path.to_str().unwrap(), false);
        assert!(
            tl.poll().err.is_some(),
            "poll of a missing file should error"
        );
        write_file(&path, "a\n");
        // follow-only: opening now seeks to EOF, so the pre-existing line is
        // not re-read, but the poll must succeed (no lingering error).
        let p = tl.poll();
        assert!(p.err.is_none(), "poll after create should succeed");
        assert!(p.lines.is_empty());
        append_file(&path, "b\n");
        assert_eq!(lines_to_strings(&tl.poll().lines), vec!["b"]);
    }

    // Mirrors TestLineTailerSameSizeRewriteResets (the 256-byte header
    // tripwire).
    #[test]
    fn same_size_rewrite_resets() {
        let dir = tempfile::tempdir().expect("tempdir");
        let path = dir.path().join("t.jsonl");
        write_file(&path, "aaaa\nbbbb\n");
        let mut tl = LineTailer::new(path.to_str().unwrap(), true);
        assert_eq!(tl.poll().lines.len(), 2, "catch-up read");
        // Rewrite with DIFFERENT content of the SAME size (fs::write
        // truncates+rewrites, so the size never dips below the offset between
        // polls). Force a distinct mtime in case the filesystem's timestamp
        // granularity would hide the rewrite.
        write_file(&path, "cccc\ndddd\n");
        bump_mtime(&path);
        let p = tl.poll();
        assert!(p.err.is_none());
        assert!(
            p.did_reset,
            "a same-size rewrite with a changed header must reset"
        );
        assert_eq!(lines_to_strings(&p.lines), vec!["cccc", "dddd"]);
    }

    // Mirrors TestLineTailerGrowingRewriteResets: an in-place rewrite that
    // ends LONGER than the previous offset. Without the size >= offset check
    // this would be mistaken for a plain append and read from the stale
    // offset, mixing old and rewritten records.
    #[test]
    fn growing_rewrite_resets() {
        let dir = tempfile::tempdir().expect("tempdir");
        let path = dir.path().join("t.jsonl");
        write_file(&path, "aaaa\nbbbb\n");
        let mut tl = LineTailer::new(path.to_str().unwrap(), true);
        assert_eq!(tl.poll().lines.len(), 2, "catch-up read");
        write_file(&path, "cccc\ndddd\neeee\n");
        bump_mtime(&path);
        let p = tl.poll();
        assert!(p.err.is_none());
        assert!(
            p.did_reset,
            "a grow-past-offset rewrite must reset, not append"
        );
        assert_eq!(lines_to_strings(&p.lines), vec!["cccc", "dddd", "eeee"]);
    }

    // Mirrors TestLineTailerCatchUpExactBoundaryKeepsFirstLine: when the
    // catch-up window starts exactly on a line boundary, the first complete
    // record must be kept (the byte just before the window is '\n').
    #[test]
    fn catch_up_exact_boundary_keeps_first_line() {
        let dir = tempfile::tempdir().expect("tempdir");
        let path = dir.path().join("t.jsonl");
        let mut content = String::new();
        // Prefix: 3 lines of 100 bytes each, ending in '\n'.
        let prefix_line = format!("P{}", "x".repeat(98)); // 99 chars + '\n' = 100 bytes
        for _ in 0..3 {
            content.push_str(&prefix_line);
            content.push('\n');
        }
        // Window: exactly TAIL_CATCH_UP_WINDOW bytes of 1024-byte lines
        // ("W%03d-" is 5 chars, so body = 1024-5-1 and each line incl. '\n' is
        // exactly 1024 bytes).
        let line_body = "w".repeat(1024 - 5 - 1);
        let n_window = (TAIL_CATCH_UP_WINDOW / 1024) as usize;
        for i in 0..n_window {
            content.push_str(&format!("W{i:03}-{line_body}\n"));
        }
        write_file(&path, &content);
        // Sanity: the catch-up start must land exactly at the prefix/window
        // boundary, i.e. right after a '\n' — otherwise this test would
        // silently exercise the skip path.
        let size = fs::metadata(&path).expect("stat").len();
        assert_eq!(size - TAIL_CATCH_UP_WINDOW, 300, "boundary math");

        let mut tl = LineTailer::new(path.to_str().unwrap(), true);
        let p = tl.poll();
        assert!(p.err.is_none());
        let got = lines_to_strings(&p.lines);
        assert_eq!(
            got.len(),
            n_window,
            "window starts on a boundary — nothing dropped"
        );
        assert!(
            got[0].starts_with("W000-"),
            "the boundary line must be kept"
        );
    }

    // The closed-tailer half of TestFileWatcherClosedRefreshNoop (the watcher
    // wrapper arrives in H7): close releases the handle and is idempotent.
    #[test]
    fn close_releases_handle() {
        let dir = tempfile::tempdir().expect("tempdir");
        let path = dir.path().join("t.jsonl");
        write_file(&path, "a\n");
        let mut tl = LineTailer::new(path.to_str().unwrap(), true);
        tl.poll();
        assert!(tl.is_open());
        tl.close();
        assert!(!tl.is_open());
        tl.close(); // idempotent
    }
}
