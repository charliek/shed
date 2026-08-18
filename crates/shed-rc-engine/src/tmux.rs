//! The tmux transport seam — a port of `internal/ext/rc/tmux.go`.
//!
//! One trait ([`TmuxRunner`]) with one method, a real implementation that spawns
//! a fresh `tmux` process per call, the two stderr error classifiers, and the
//! verb helpers every op is built from. Exactly like Go: no long-lived tmux
//! client, no server socket management, no version check.
//!
//! **Implicit tmux ≥ 3.2 floor.** `new-session -e KEY=value` (how a session's
//! `SHED_RC_*` metadata is stamped) landed in tmux 3.2 and there is no capability
//! negotiation on either side — an older tmux fails the create with its own
//! "unknown option" error. Go never checked either; recorded here so the floor is
//! documented at least once (the parity harness's CI job asserts the version).

use std::process::Command;
use std::time::Duration;

use shed_core::rc::TMUX_PREFIX;
use shed_core::rc_agents::ENV_PREFIX;

/// The outcome of one tmux invocation (`rc.Result`, `tmux.go:14`).
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct TmuxResult {
    pub stdout: String,
    pub stderr: String,
    /// The process exit code, or `-1` when tmux could not be started at all
    /// (matching Go's non-`ExitError` arm, `tmux.go:41`) or was killed by a
    /// signal (Go's `ExitCode()` also reports `-1` there).
    pub code: i32,
}

/// Runs `tmux <args>` and returns the result — injected so every operation is
/// testable against a fake tmux (`rc.Runner`, `tmux.go:22`).
///
/// Takes `&[&str]` rather than Go's variadic: the caller assembles one argv slice
/// and the fake records it verbatim, which is what the argv pins assert against.
pub trait TmuxRunner {
    fn run(&self, args: &[&str]) -> TmuxResult;
}

/// The production runner: a fresh `tmux` child per call, on the default server
/// (`execRunner`, `tmux.go:28`).
///
/// A spawn failure (tmux missing, fork failure) becomes `code = -1` with the OS
/// error text in `stderr`, exactly as Go does — the missing/duplicate
/// classifiers below then read it as "not a known tmux condition", so a broken
/// tmux never masquerades as a gone session.
///
/// **One deliberate divergence:** Go's `string(buf)` preserves non-UTF-8 bytes
/// verbatim, where this uses `from_utf8_lossy`. tmux's own output is text and
/// the classifiers only match ASCII anchors, but a pane capture containing
/// invalid UTF-8 would render `U+FFFD` here and raw bytes there. Recorded for the
/// parity harness: pane text is never byte-compared, only classified.
pub struct ExecRunner;

impl TmuxRunner for ExecRunner {
    fn run(&self, args: &[&str]) -> TmuxResult {
        match Command::new("tmux").args(args).output() {
            Ok(out) => TmuxResult {
                stdout: String::from_utf8_lossy(&out.stdout).into_owned(),
                stderr: String::from_utf8_lossy(&out.stderr).into_owned(),
                code: out.status.code().unwrap_or(-1),
            },
            Err(err) => TmuxResult {
                stdout: String::new(),
                stderr: err.to_string(),
                code: -1,
            },
        }
    }
}

// ---------------------------------------------------------------------------
// stderr error classes (tmux.go:53-62)
// ---------------------------------------------------------------------------

/// `dupSessionRe` (`tmux.go:54`).
const DUPLICATE_NEEDLES: [&str; 2] = ["duplicate session", "already exists"];

/// `missingSessionRe` (`tmux.go:58`). tmux reports a gone target differently per
/// command: `kill-session` says "can't find session", `capture-pane`/`send-keys`
/// say "can't find pane", and killing the LAST session stops the server ("no
/// server running"). All three mean "already gone".
const MISSING_NEEDLES: [&str; 4] = [
    "can't find session",
    "can't find pane",
    "no session",
    "no server running",
];

/// Go spells these two classifiers as `(?i)`-anchored alternations; here they are
/// ASCII-lowercase substring scans, which is **exactly** equivalent for these
/// patterns (every alternative is pure ASCII) and avoids taking a `regex`
/// dependency in `shed-app` for four literals. The only theoretical divergence is
/// Unicode case folding of non-ASCII look-alikes (Go's `(?i)` folds U+212A KELVIN
/// onto `k`); tmux never emits those.
fn contains_any_ascii_ci(haystack: &str, needles: &[&str]) -> bool {
    let lower = haystack.to_ascii_lowercase();
    needles.iter().any(|n| lower.contains(n))
}

/// Whether a `new-session` stderr means the session name is taken
/// (`isDuplicateSession`, `tmux.go:61`) — the exit-3 / `SlugTaken` class.
pub fn is_duplicate_session(stderr: &str) -> bool {
    contains_any_ascii_ci(stderr, &DUPLICATE_NEEDLES)
}

/// Whether a tmux stderr means the target session/pane is gone
/// (`isMissingSession`, `tmux.go:62`) — the exit-4 / `NotFound` class, and the
/// signal that turns a failed `kill` into idempotent success.
pub fn is_missing_session(stderr: &str) -> bool {
    contains_any_ascii_ci(stderr, &MISSING_NEEDLES)
}

// ---------------------------------------------------------------------------
// verb helpers (tmux.go:64-197)
// ---------------------------------------------------------------------------

/// The pause between typing a line and submitting it (`sendLineSettle`,
/// `tmux.go:150`). A freshly-ready REPL can still be ingesting the literal paste,
/// and an Enter that arrives mid-ingest is dropped — leaving the line typed but
/// unsubmitted. Injectable (Go makes it a package var tests zero; here it is a
/// [`Tmux`] field) because it is pure dead time in a unit test.
pub const DEFAULT_SEND_LINE_SETTLE: Duration = Duration::from_millis(750);

/// The dedicated tmux buffer a multi-line prompt is pasted through
/// (`sendBlock`'s `buf`, `tmux.go:173`). **Pinned cross-implementation**: the
/// literal name is observable in a `tmux list-buffers` on a mixed fleet and in
/// the parity harness's transcripts, so it must not be "improved".
pub const PROMPT_BUFFER: &str = "shed-ext-rc-prompt";

/// How many lines of scrollback a pane capture carries (`capturePane`,
/// `tmux.go:76`). Scrollback is what makes lifecycle classification work: a boot
/// banner, a login URL, or the shell prompt an agent exited to has usually
/// scrolled off by the time anyone looks.
const CAPTURE_SCROLLBACK: &str = "-200";

/// The tmux verb layer: a [`TmuxRunner`] plus the inter-key settle. Every op in
/// [`super::ops`] goes through this rather than the raw runner, so the argv shape
/// of each verb lives in exactly one place (and the fake records it).
pub struct Tmux<'a> {
    runner: &'a dyn TmuxRunner,
    settle: Duration,
}

impl<'a> Tmux<'a> {
    pub fn new(runner: &'a dyn TmuxRunner) -> Self {
        Self {
            runner,
            settle: DEFAULT_SEND_LINE_SETTLE,
        }
    }

    /// Override the inter-key settle (tests pass `Duration::ZERO`).
    #[must_use]
    pub fn with_settle(mut self, settle: Duration) -> Self {
        self.settle = settle;
        self
    }

    /// Escape hatch for a caller that needs a verb this layer does not model.
    pub fn run(&self, args: &[&str]) -> TmuxResult {
        self.runner.run(args)
    }

    fn sleep_settle(&self) {
        if !self.settle.is_zero() {
            std::thread::sleep(self.settle);
        }
    }

    /// The shared tail of both delivery verbs (`sendLine`/`sendBlock`,
    /// `tmux.go:156`/`:172`): bail on a failed keystroke, settle, then submit.
    ///
    /// `res` is the delivery call's own outcome, taken by value so each verb's
    /// argv stays spelled out at its own call site — the helper carries only the
    /// short-circuit + settle, never a tmux argument.
    fn settle_then_enter(&self, name: &str, res: TmuxResult) -> TmuxResult {
        if res.code != 0 {
            return res;
        }
        self.sleep_settle();
        self.send_enter(name)
    }

    /// `tmux new-session -d -s <name> -c <workdir> <env args…> <inner>`
    /// (`createSession`, `tmux.go:65`).
    ///
    /// **`inner` is ONE argv token** — the whole command string, not split words.
    /// tmux hands it to the pane's shell; splitting it here would break every
    /// quoted display name (`claude --name 'my-shed/abc' /rc`). Pinned by
    /// `rc_test.go:458` on the Go side and by the argv test below on this one.
    pub fn create_session(
        &self,
        name: &str,
        workdir: &str,
        env_args: &[String],
        inner: &str,
    ) -> TmuxResult {
        let mut args: Vec<&str> = vec!["new-session", "-d", "-s", name, "-c", workdir];
        args.extend(env_args.iter().map(String::as_str));
        args.push(inner);
        self.run(&args)
    }

    /// `tmux capture-pane -t <name> -p -S -200` — the visible frame plus
    /// [`CAPTURE_SCROLLBACK`] lines of history (`capturePane`, `tmux.go:75`).
    ///
    /// (Go's scrollback-free twin `captureVisiblePane` is NOT ported: its only
    /// consumers are the hub's approval anchors, which stay Go this block.)
    pub fn capture_pane(&self, name: &str) -> TmuxResult {
        self.run(&["capture-pane", "-t", name, "-p", "-S", CAPTURE_SCROLLBACK])
    }

    /// The `rc-*` tmux session names (`listSessionNames`, `tmux.go:96`).
    ///
    /// **Any listing failure reads as an empty list** — that is the one-shot
    /// contract (Go keeps a `…Checked` variant for the hub's reconcile loop,
    /// which must not mistake a transient tmux hiccup for "every session is
    /// gone"; the hub is not ported, so neither is that variant).
    pub fn list_session_names(&self) -> Vec<String> {
        let res = self.run(&["ls", "-F", "#{session_name}"]);
        if res.code != 0 {
            return Vec::new();
        }
        res.stdout
            .split('\n')
            .map(str::trim)
            .filter(|line| line.starts_with(TMUX_PREFIX))
            .map(str::to_string)
            .collect()
    }

    /// A session's `SHED_RC_*`-filtered `show-environment` dump
    /// (`showEnvironment`, `tmux.go:125`). A failed call yields an empty dump,
    /// which [`shed_core::rc_agents::parse_session`] renders as an unmanaged row.
    ///
    /// The filter is what keeps the bare `OPENCODE_SERVER_PASSWORD=` override (and
    /// the pane's whole inherited environment) out of the parsed metadata.
    pub fn show_environment(&self, name: &str) -> String {
        let res = self.run(&["show-environment", "-t", name]);
        if res.code != 0 {
            return String::new();
        }
        let mut out = String::new();
        for line in res.stdout.split('\n') {
            if line.starts_with(ENV_PREFIX) {
                out.push_str(line);
                out.push('\n');
            }
        }
        out
    }

    /// `tmux kill-session -t <name>` (`killSession`, `tmux.go:142`). A missing
    /// session is reported through the [`TmuxResult`] for the caller to treat as
    /// idempotent success — never an error here.
    pub fn kill_session(&self, name: &str) -> TmuxResult {
        self.run(&["kill-session", "-t", name])
    }

    /// Deliver `text` into a session and submit it (`sendLine`, `tmux.go:156`).
    ///
    /// A single line is typed literally (`-l`; `--` stops option parsing so a
    /// leading `-` is text, not a flag); a multi-line block goes through
    /// [`Self::send_block`] so embedded newlines don't submit early. The settle
    /// before Enter avoids the Enter being dropped mid-ingest.
    pub fn send_line(&self, name: &str, text: &str) -> TmuxResult {
        if text.contains('\n') {
            return self.send_block(name, text);
        }
        let typed = self.run(&["send-keys", "-t", name, "-l", "--", text]);
        self.settle_then_enter(name, typed)
    }

    /// Deliver a multi-line block as ONE input via a bracketed paste
    /// (`sendBlock`, `tmux.go:172`): load the text into [`PROMPT_BUFFER`], then
    /// `paste-buffer -p` so the TUI inserts the whole block (embedded newlines
    /// stay as input rather than submitting), then settle and press Enter to
    /// submit it as a single prompt. `-d` removes the temp buffer.
    pub fn send_block(&self, name: &str, text: &str) -> TmuxResult {
        let loaded = self.run(&["set-buffer", "-b", PROMPT_BUFFER, "--", text]);
        if loaded.code != 0 {
            return loaded;
        }
        let pasted = self.run(&["paste-buffer", "-p", "-d", "-b", PROMPT_BUFFER, "-t", name]);
        self.settle_then_enter(name, pasted)
    }

    /// Press Enter (`sendEnter`, `tmux.go:185`) — accepts the pre-selected "Yes,
    /// I trust this folder" of every agent's directory-trust gate.
    pub fn send_enter(&self, name: &str) -> TmuxResult {
        self.run(&["send-keys", "-t", name, "Enter"])
    }

    /// Accept claude's "Bypass Permissions mode" dialog (`acceptBypassPrompt`,
    /// `tmux.go:192`) by selecting "2. Yes, I accept": option "1. No, exit" is
    /// pre-selected, so move Down once, then Enter.
    ///
    /// **A failed `Down` short-circuits** — pressing Enter on a dialog still
    /// sitting on "No, exit" would kill the session.
    pub fn accept_bypass_prompt(&self, name: &str) -> TmuxResult {
        let res = self.run(&["send-keys", "-t", name, "Down"]);
        if res.code != 0 {
            return res;
        }
        self.send_enter(name)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::fake::FakeTmux;

    // ---- error classes (mirrors Go TestIsMissingSession, rc_test.go:734) ----

    #[test]
    fn missing_session_classifier() {
        for stderr in [
            "can't find session: rc-x",
            "can't find pane: rc-x", // capture-pane / send-keys phrasing
            "no server running on /tmp/tmux-501/default",
            "no session found",
            "CAN'T FIND SESSION: rc-x", // (?i)
        ] {
            assert!(is_missing_session(stderr), "{stderr:?}");
        }
        assert!(!is_missing_session("connection refused"));
        assert!(!is_missing_session(""));
    }

    #[test]
    fn duplicate_session_classifier() {
        for stderr in [
            "duplicate session: rc-abc123",
            "session already exists",
            "Duplicate Session: rc-x",
        ] {
            assert!(is_duplicate_session(stderr), "{stderr:?}");
        }
        assert!(!is_duplicate_session("can't find session: rc-x"));
    }

    // ---- verb argv shapes ----

    #[test]
    fn create_session_argv_puts_inner_last_as_one_token() {
        let f = FakeTmux::ok();
        let env_args = vec!["-e".to_string(), "SHED_RC_V=2".to_string()];
        Tmux::new(&f).with_settle(Duration::ZERO).create_session(
            "rc-abc123",
            "/home/shed",
            &env_args,
            "claude --name 'demo' /rc",
        );
        assert_eq!(
            f.calls()[0],
            vec![
                "new-session",
                "-d",
                "-s",
                "rc-abc123",
                "-c",
                "/home/shed",
                "-e",
                "SHED_RC_V=2",
                "claude --name 'demo' /rc",
            ]
        );
    }

    #[test]
    fn capture_pane_argv_carries_scrollback() {
        let f = FakeTmux::ok();
        Tmux::new(&f).capture_pane("rc-x");
        assert_eq!(
            f.calls()[0],
            vec!["capture-pane", "-t", "rc-x", "-p", "-S", "-200"]
        );
    }

    #[test]
    fn list_session_names_keeps_rc_prefixed_only() {
        let f = FakeTmux::new(|_| TmuxResult {
            stdout: "rc-aaa\nother\n  rc-bbb  \n\n".to_string(),
            ..Default::default()
        });
        assert_eq!(Tmux::new(&f).list_session_names(), vec!["rc-aaa", "rc-bbb"]);
        assert_eq!(f.calls()[0], vec!["ls", "-F", "#{session_name}"]);
    }

    #[test]
    fn list_session_names_swallows_failure_as_empty() {
        let f = FakeTmux::new(|_| TmuxResult {
            stderr: "no server running on /tmp/tmux".to_string(),
            code: 1,
            ..Default::default()
        });
        assert!(Tmux::new(&f).list_session_names().is_empty());
    }

    #[test]
    fn show_environment_filters_to_the_rc_prefix() {
        let f = FakeTmux::new(|_| {
            TmuxResult {
            // A real dump carries the whole pane environment, tmux's bare `-KEY`
            // removal lines, and the bare opencode password override; only the
            // `SHED_RC_`-PREFIXED lines survive — which is exactly why a removal
            // line (`-SHED_RC_…`, prefixed with the dash) drops out here rather
            // than having to be special-cased downstream.
            stdout: "PATH=/usr/bin\nSHED_RC_V=2\n-SHED_RC_GONE\nSHED_RC_KIND=codex\nOPENCODE_SERVER_PASSWORD=\n"
                .to_string(),
            ..Default::default()
        }
        });
        assert_eq!(
            Tmux::new(&f).show_environment("rc-x"),
            "SHED_RC_V=2\nSHED_RC_KIND=codex\n"
        );
        assert_eq!(f.calls()[0], vec!["show-environment", "-t", "rc-x"]);
    }

    #[test]
    fn show_environment_failure_is_an_empty_dump() {
        let f = FakeTmux::new(|_| TmuxResult {
            code: 1,
            stderr: "can't find session: rc-x".to_string(),
            ..Default::default()
        });
        assert_eq!(Tmux::new(&f).show_environment("rc-x"), "");
    }

    // ---- delivery (mirrors Go TestSendLineMultilineUsesBracketedPaste, rc_test.go:534) ----

    #[test]
    fn send_line_single_types_literally_then_enter() {
        let f = FakeTmux::ok();
        Tmux::new(&f)
            .with_settle(Duration::ZERO)
            .send_line("rc-x", "just one line");
        assert_eq!(
            f.calls(),
            vec![
                vec!["send-keys", "-t", "rc-x", "-l", "--", "just one line"],
                vec!["send-keys", "-t", "rc-x", "Enter"],
            ]
        );
    }

    #[test]
    fn send_line_multiline_uses_a_bracketed_paste() {
        let f = FakeTmux::ok();
        Tmux::new(&f)
            .with_settle(Duration::ZERO)
            .send_line("rc-x", "line one\nline two");
        assert_eq!(
            f.calls(),
            vec![
                vec![
                    "set-buffer",
                    "-b",
                    PROMPT_BUFFER,
                    "--",
                    "line one\nline two"
                ],
                vec![
                    "paste-buffer",
                    "-p",
                    "-d",
                    "-b",
                    PROMPT_BUFFER,
                    "-t",
                    "rc-x"
                ],
                vec!["send-keys", "-t", "rc-x", "Enter"],
            ]
        );
    }

    #[test]
    fn send_line_failure_short_circuits_before_enter() {
        let f = FakeTmux::new(|args| {
            if args[0] == "send-keys" {
                TmuxResult {
                    code: 1,
                    stderr: "can't find pane: rc-x".to_string(),
                    ..Default::default()
                }
            } else {
                TmuxResult::default()
            }
        });
        let res = Tmux::new(&f)
            .with_settle(Duration::ZERO)
            .send_line("rc-x", "text");
        assert_eq!(res.code, 1);
        assert_eq!(f.calls().len(), 1, "no Enter after a failed literal send");
    }

    #[test]
    fn send_block_failure_short_circuits_at_set_buffer() {
        let f = FakeTmux::new(|args| {
            if args[0] == "set-buffer" {
                TmuxResult {
                    code: 1,
                    stderr: "no server running".to_string(),
                    ..Default::default()
                }
            } else {
                TmuxResult::default()
            }
        });
        assert_eq!(
            Tmux::new(&f)
                .with_settle(Duration::ZERO)
                .send_line("rc-x", "a\nb")
                .code,
            1
        );
        assert_eq!(
            f.calls().len(),
            1,
            "no paste-buffer after a failed set-buffer"
        );
    }

    // ---- trust / bypass keystrokes ----

    #[test]
    fn accept_bypass_prompt_sends_down_then_enter() {
        let f = FakeTmux::ok();
        Tmux::new(&f).accept_bypass_prompt("rc-x");
        assert_eq!(
            f.calls(),
            vec![
                vec!["send-keys", "-t", "rc-x", "Down"],
                vec!["send-keys", "-t", "rc-x", "Enter"],
            ]
        );
    }

    #[test]
    fn accept_bypass_prompt_never_enters_after_a_failed_down() {
        // Entering on a dialog still sitting on "1. No, exit" would kill the
        // session — the Down failure must short-circuit.
        let f = FakeTmux::new(|_| TmuxResult {
            code: 1,
            stderr: "can't find pane".to_string(),
            ..Default::default()
        });
        Tmux::new(&f).accept_bypass_prompt("rc-x");
        assert_eq!(f.calls().len(), 1);
    }
}
