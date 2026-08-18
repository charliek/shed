//! Unit tests for the one-shot verbs, mirroring the Go tables in
//! `internal/ext/rc/rc_test.go` and `plan_test.go` case for case (each test names
//! its original). Every seam is injected: a recording fake tmux, a fixed wall
//! clock, and a monotonic clock the fake sleep ADVANCES — which is what lets the
//! 20 s `--wait` timeout be exercised in microseconds.

use std::cell::Cell;
use std::sync::Arc;

use super::*;
use crate::clock::Clock;
use crate::fake::{env_from, home_env, FakeTmux};
use crate::tmux::TmuxResult;

/// A pinned wall clock so `created_at` (and therefore the `new-session` argv) is
/// deterministic.
const FIXED_UNIX: i64 = 1_770_000_000;

struct FixedClock;
impl Clock for FixedClock {
    fn now_unix(&self) -> i64 {
        FIXED_UNIX
    }
}

fn fixed_created_at() -> String {
    FixedClock.now_iso8601()
}

/// The shared test rig: settle zeroed (Go zeroes `sendLineSettle` in `init`), a
/// fixed wall clock, and a monotonic clock driven by the fake sleep so a poll
/// loop cannot burn real time.
fn engine<'a>(tmux: &'a FakeTmux, elapsed: &'a Cell<Duration>) -> Engine<'a> {
    let base = Instant::now();
    Engine::new(tmux)
        .with_settle(Duration::ZERO)
        .with_clock(Arc::new(FixedClock))
        .with_sleep(|d| elapsed.set(elapsed.get() + d))
        .with_monotonic(move || base + elapsed.get())
        .with_env(|_| String::new())
}

/// The rig of [`engine`] with the default `$HOME` most cases want — the shape
/// almost every test below needs, spelled once.
fn engine_at_home<'a>(tmux: &'a FakeTmux, elapsed: &'a Cell<Duration>) -> Engine<'a> {
    engine(tmux, elapsed).with_env(home_env("/home/shed"))
}

// ---------------------------------------------------------------------------
// create — the happy path argv pin (Go TestCreateSuccess, rc_test.go:455)
// ---------------------------------------------------------------------------

#[test]
fn create_success_pins_the_new_session_argv() {
    let f = FakeTmux::ok();
    let ticks = Cell::new(Duration::ZERO);
    let eng = engine_at_home(&f, &ticks);

    let mut opts = CreateOptions::new(RcKind::ClaudeRc);
    opts.display_name = "demo".to_string();
    opts.slug = "abc123".to_string();
    opts.created_by = "shed-ext-rc/0.5.0".to_string();
    let session = eng.create(opts).unwrap();

    assert_eq!(session.slug, "abc123");
    assert_eq!(session.tmux_session, "rc-abc123");
    assert_eq!(session.kind, RcKind::ClaudeRc);
    assert_eq!(session.state, RcState::Starting);
    assert!(session.managed);
    assert_eq!(session.lane.as_deref(), Some("tui"));
    assert_eq!(session.workdir.as_deref(), Some("/home/shed"));
    assert_eq!(session.display_name.as_deref(), Some("demo"));
    assert_eq!(session.created_by.as_deref(), Some("shed-ext-rc/0.5.0"));
    assert_eq!(
        session.created_at.as_deref(),
        Some(fixed_created_at().as_str())
    );
    assert!(session.id.as_deref().is_some_and(|id| id.len() == 36));
    // No --target ⇒ the field is ABSENT (Go's omitempty), never "".
    assert_eq!(session.target_label, None);
    assert_eq!(session.url, None);

    // The whole argv, in order: BuildEnvArgs' ordering is the contract a
    // differential harness cannot learn from `tmux show-environment` (tmux
    // re-orders its dump), so it is pinned HERE — and the inner command is the
    // LAST element, as ONE token (Go pins the same at rc_test.go:458).
    let id = session.id.clone().unwrap();
    let created_at = session.created_at.clone().unwrap();
    assert_eq!(
        f.call_with("new-session").unwrap(),
        vec![
            "new-session",
            "-d",
            "-s",
            "rc-abc123",
            "-c",
            "/home/shed",
            "-e",
            "SHED_RC_V=2",
            "-e",
            &format!("SHED_RC_ID={id}"),
            "-e",
            "SHED_RC_DISPLAY_NAME=demo",
            "-e",
            "SHED_RC_KIND=claude-rc",
            "-e",
            "SHED_RC_WORKDIR=/home/shed",
            "-e",
            "SHED_RC_CREATED_BY=shed-ext-rc/0.5.0",
            "-e",
            &format!("SHED_RC_CREATED_AT={created_at}"),
            "-e",
            "SHED_RC_SLUG=abc123",
            "claude --name 'demo' /rc",
        ]
    );
    // No --wait and no prompt ⇒ tmux is touched exactly once.
    assert_eq!(f.calls().len(), 1);
}

#[test]
fn create_stamps_target_and_defaults_display_name_to_the_slug() {
    let f = FakeTmux::ok();
    let ticks = Cell::new(Duration::ZERO);
    let eng = engine_at_home(&f, &ticks);

    let mut opts = CreateOptions::new(RcKind::Shell);
    opts.slug = "abc123".to_string();
    opts.target = "mini3".to_string();
    let session = eng.create(opts).unwrap();

    assert_eq!(session.display_name.as_deref(), Some("abc123"));
    assert_eq!(session.target_label.as_deref(), Some("mini3"));
    // No --created-by ⇒ the engine's last-resort provenance token (every real
    // CLI supplies its own; see DEFAULT_CREATED_BY).
    assert_eq!(session.created_by.as_deref(), Some(DEFAULT_CREATED_BY));
    let ns = f.call_with("new-session").unwrap();
    // TARGET rides after SLUG, only when set.
    assert_eq!(ns[ns.len() - 3], "-e");
    assert_eq!(ns[ns.len() - 2], "SHED_RC_TARGET=mini3");
    assert_eq!(ns[ns.len() - 1], "bash -l");
}

#[test]
fn create_generates_a_slug_when_none_is_given() {
    let f = FakeTmux::ok();
    let ticks = Cell::new(Duration::ZERO);
    let eng = engine_at_home(&f, &ticks);
    let session = eng.create(CreateOptions::new(RcKind::Shell)).unwrap();
    assert_eq!(session.slug.len(), 6);
    assert_eq!(session.tmux_session, format!("rc-{}", session.slug));
    assert_eq!(session.display_name.as_deref(), Some(session.slug.as_str()));
}

#[test]
fn create_rejects_an_invalid_caller_slug_before_touching_tmux() {
    let f = FakeTmux::ok();
    let ticks = Cell::new(Duration::ZERO);
    let eng = engine_at_home(&f, &ticks);
    let mut opts = CreateOptions::new(RcKind::Shell);
    opts.slug = "Bad_Slug".to_string();
    let err = eng.create(opts).unwrap_err();
    assert_eq!(err.exit_code(), 2);
    assert_eq!(
        err.to_string(),
        "invalid arguments: invalid slug \"Bad_Slug\""
    );
    assert!(f.calls().is_empty());
}

#[test]
fn create_rejects_an_unknown_kind() {
    let f = FakeTmux::ok();
    let ticks = Cell::new(Duration::ZERO);
    let eng = engine(&f, &ticks);
    let err = eng
        .create(CreateOptions::new(RcKind::Other("bogus".to_string())))
        .unwrap_err();
    assert_eq!(err.exit_code(), 2);
    assert_eq!(err.to_string(), "invalid arguments: unknown kind \"bogus\"");
    assert!(f.calls().is_empty());
}

// Mirrors Go TestCreateDuplicateSlug (rc_test.go:481).
#[test]
fn create_duplicate_slug_is_the_exit_3_class() {
    let f = FakeTmux::new(|_| TmuxResult {
        code: 1,
        stderr: "duplicate session: rc-abc123".to_string(),
        ..Default::default()
    });
    let ticks = Cell::new(Duration::ZERO);
    let eng = engine_at_home(&f, &ticks);
    let mut opts = CreateOptions::new(RcKind::Shell);
    opts.slug = "abc123".to_string();
    let err = eng.create(opts).unwrap_err();
    assert_eq!(err.exit_code(), 3);
    assert_eq!(err.to_string(), "rc session already exists: rc-abc123");
}

#[test]
fn create_other_tmux_failure_is_the_generic_class() {
    let f = FakeTmux::new(|_| TmuxResult {
        code: 1,
        stderr: "  no space left on device\n".to_string(),
        ..Default::default()
    });
    let ticks = Cell::new(Duration::ZERO);
    let eng = engine_at_home(&f, &ticks);
    let err = eng.create(CreateOptions::new(RcKind::Shell)).unwrap_err();
    assert_eq!(err.exit_code(), 1);
    assert_eq!(
        err.to_string(),
        "tmux new-session failed: no space left on device"
    );
}

// ---------------------------------------------------------------------------
// the installed-agent gate (Go TestCreateInstalledAgentGate, rc_test.go:648)
// ---------------------------------------------------------------------------

#[test]
fn installed_agent_gate_rejects_a_missing_binary_before_any_tmux_work() {
    let f = FakeTmux::ok();
    let ticks = Cell::new(Duration::ZERO);
    let probed = Cell::new(String::new());
    let eng = engine_at_home(&f, &ticks).with_bin_probe(|bin| {
        probed.set(bin.to_string());
        false // `bash -lc 'command -v cursor-agent'` exited non-zero
    });
    let mut opts = CreateOptions::new(RcKind::Cursor);
    opts.slug = "abc123".to_string();
    let err = eng.create(opts).unwrap_err();

    assert_eq!(probed.take(), "cursor-agent", "probe gets the registry bin");
    assert_eq!(err.exit_code(), 2);
    let msg = err.to_string();
    assert!(msg.contains("cursor-agent"), "{msg}");
    assert!(msg.contains("not found on the session PATH"), "{msg}");
    assert!(msg.contains("recreate from a newer image"), "{msg}");
    assert!(msg.contains("pick another --kind"), "{msg}");
    assert!(
        f.calls().is_empty(),
        "tmux must not be touched: {:?}",
        f.calls()
    );
}

#[test]
fn installed_agent_gate_proceeds_when_present() {
    let f = FakeTmux::ok();
    let ticks = Cell::new(Duration::ZERO);
    let probed = Cell::new(String::new());
    let eng = engine_at_home(&f, &ticks).with_bin_probe(|bin| {
        probed.set(bin.to_string());
        true
    });
    let mut opts = CreateOptions::new(RcKind::Codex);
    opts.slug = "abc123".to_string();
    eng.create(opts).unwrap();
    assert_eq!(probed.take(), "codex");
    assert!(f.call_with("new-session").is_some());
}

#[test]
fn installed_agent_gate_is_opt_in_and_skips_binless_kinds() {
    // No probe wired ⇒ no gate (Go's nil BinProbe).
    let f = FakeTmux::ok();
    let ticks = Cell::new(Duration::ZERO);
    let eng = engine_at_home(&f, &ticks);
    let mut opts = CreateOptions::new(RcKind::Codex);
    opts.slug = "abc123".to_string();
    eng.create(opts).unwrap();
    assert!(f.call_with("new-session").is_some());

    // shell has no Bin, so the probe is never consulted even when wired.
    let g = FakeTmux::ok();
    let ticks2 = Cell::new(Duration::ZERO);
    let called = Cell::new(false);
    let eng2 = engine_at_home(&g, &ticks2).with_bin_probe(|_| {
        called.set(true);
        false
    });
    let mut opts = CreateOptions::new(RcKind::Shell);
    opts.slug = "abc123".to_string();
    eng2.create(opts).unwrap();
    assert!(!called.get(), "shell has no binary to probe");
}

// ---------------------------------------------------------------------------
// workdir resolution (ops.go:154)
// ---------------------------------------------------------------------------

/// `(case name, --workdir, env table, expected workdir)`.
type WorkdirCase<'a> = (&'a str, &'a str, &'a [(&'a str, &'a str)], &'a str);

#[test]
fn workdir_resolution_table() {
    let cases: &[WorkdirCase] = &[
        (
            "explicit --workdir wins",
            "/opt/work",
            &[("SHED_WORKSPACE", "/ws"), ("HOME", "/home/shed")],
            "/opt/work",
        ),
        (
            "SHED_WORKSPACE next",
            "",
            &[("SHED_WORKSPACE", "/ws"), ("HOME", "/home/shed")],
            "/ws",
        ),
        ("HOME last", "", &[("HOME", "/home/shed")], "/home/shed"),
        (
            "empty SHED_WORKSPACE falls through to HOME",
            "",
            &[("SHED_WORKSPACE", ""), ("HOME", "/home/shed")],
            "/home/shed",
        ),
    ];
    for (name, workdir, env, want) in cases {
        let f = FakeTmux::ok();
        let ticks = Cell::new(Duration::ZERO);
        let env = env_from(env);
        let eng = engine(&f, &ticks).with_env(env);
        let mut opts = CreateOptions::new(RcKind::Shell);
        opts.slug = "abc123".to_string();
        opts.workdir = (*workdir).to_string();
        let session = eng.create(opts).unwrap();
        assert_eq!(session.workdir.as_deref(), Some(*want), "case {name}");
        let ns = f.call_with("new-session").unwrap();
        assert_eq!(ns[5], *want, "case {name}: -c argument");
    }
}

#[test]
fn workdir_unresolvable_is_bad_args() {
    let f = FakeTmux::ok();
    let ticks = Cell::new(Duration::ZERO);
    let eng = engine(&f, &ticks); // env reads everything as ""
    let err = eng.create(CreateOptions::new(RcKind::Shell)).unwrap_err();
    assert_eq!(err.exit_code(), 2);
    assert_eq!(
        err.to_string(),
        "invalid arguments: no --workdir and SHED_WORKSPACE/HOME unset"
    );
    assert!(f.calls().is_empty());
}

// ---------------------------------------------------------------------------
// prompt + permission validation (Go TestCreatePromptOnBrokerRejected :495,
// TestCreateUnsafePromptRejected :508, TestCreatePermissionModeValidation :568)
// ---------------------------------------------------------------------------

#[test]
fn create_prompt_guards() {
    let cases: &[(&str, RcKind, &str, &str)] = &[
        (
            "broker takes no prompt",
            RcKind::ClaudeBroker,
            "hi",
            "invalid arguments: kind \"claude-broker\" does not accept a prompt",
        ),
        (
            "ESC is rejected",
            RcKind::ClaudeRc,
            "a\u{1b}b",
            "invalid arguments: prompt contains an unsupported control character",
        ),
    ];
    for (name, kind, prompt, want) in cases {
        let f = FakeTmux::ok();
        let ticks = Cell::new(Duration::ZERO);
        let eng = engine_at_home(&f, &ticks);
        let mut opts = CreateOptions::new(kind.clone());
        opts.prompt = (*prompt).to_string();
        let err = eng.create(opts).unwrap_err();
        assert_eq!(err.to_string(), *want, "case {name}");
        assert_eq!(err.exit_code(), 2, "case {name}");
        assert!(f.calls().is_empty(), "case {name}: tmux touched");
    }
}

#[test]
fn create_permission_mode_validation() {
    // Invalid mode, and a claude-only mode on a non-claude kind, both exit 2
    // before any tmux work — with DIFFERENT messages (the harness diffs them).
    for (kind, mode, want) in [
        (
            RcKind::ClaudeRc,
            "yolo",
            "invalid arguments: invalid permission mode \"yolo\" (want default|auto|skip)",
        ),
        (
            RcKind::Codex,
            "acceptEdits",
            "invalid arguments: permission mode \"acceptEdits\" is claude-only; codex kinds accept default|auto|skip",
        ),
    ] {
        let f = FakeTmux::ok();
        let ticks = Cell::new(Duration::ZERO);
            let eng = engine_at_home(&f, &ticks);
        let mut opts = CreateOptions::new(kind);
        opts.permission_mode = mode.to_string();
        let err = eng.create(opts).unwrap_err();
        assert_eq!(err.to_string(), want);
        assert_eq!(err.exit_code(), 2);
        assert!(f.calls().is_empty());
    }
}

#[test]
fn permission_mode_flows_into_the_inner_command() {
    for (kind, mode, want_inner) in [
        (RcKind::Shell, "auto", "bash -l"),
        (
            RcKind::Codex,
            "auto",
            "codex --ask-for-approval on-request --sandbox workspace-write",
        ),
        (
            RcKind::ClaudeRc,
            "auto",
            "claude --remote-control --name 'abc123' --permission-mode auto",
        ),
    ] {
        let f = FakeTmux::ok();
        let ticks = Cell::new(Duration::ZERO);
        let eng = engine_at_home(&f, &ticks);
        let mut opts = CreateOptions::new(kind);
        opts.slug = "abc123".to_string();
        opts.permission_mode = mode.to_string();
        eng.create(opts).unwrap();
        let ns = f.call_with("new-session").unwrap();
        assert_eq!(ns.last().unwrap(), want_inner);
    }
}

#[test]
fn interactive_shell_wraps_the_inner_command() {
    let f = FakeTmux::ok();
    let ticks = Cell::new(Duration::ZERO);
    let eng = engine_at_home(&f, &ticks);
    let mut opts = CreateOptions::new(RcKind::ClaudeRc);
    opts.slug = "abc123".to_string();
    opts.display_name = "x".to_string();
    opts.interactive_shell = true;
    eng.create(opts).unwrap();
    assert_eq!(
        f.call_with("new-session").unwrap().last().unwrap(),
        r"bash -ic 'claude --name '\''x'\'' /rc'"
    );
}

// ---------------------------------------------------------------------------
// opencode's port + password (meta.go:47-55,81-83)
// ---------------------------------------------------------------------------

#[test]
fn opencode_create_allocates_a_port_and_blanks_the_password() {
    let f = FakeTmux::ok();
    let ticks = Cell::new(Duration::ZERO);
    let eng = engine_at_home(&f, &ticks);
    let mut opts = CreateOptions::new(RcKind::Opencode);
    opts.slug = "abc123".to_string();
    eng.create(opts).unwrap();

    let ns = f.call_with("new-session").unwrap();
    let port_env = ns
        .iter()
        .find(|a| a.starts_with("SHED_RC_OPENCODE_PORT="))
        .expect("opencode stamps its allocated port");
    let port: u16 = port_env
        .trim_start_matches("SHED_RC_OPENCODE_PORT=")
        .parse()
        .unwrap();
    assert_ne!(port, 0);
    // The bare (non-SHED_RC_) password override is the last env pair, right
    // before the inner command.
    assert_eq!(ns[ns.len() - 3], "-e");
    assert_eq!(ns[ns.len() - 2], "OPENCODE_SERVER_PASSWORD=");
    // …and the port reaches the inner command, inside any wrap.
    assert_eq!(
        ns.last().unwrap(),
        &format!("opencode --port {port} --hostname 127.0.0.1")
    );
}

// ---------------------------------------------------------------------------
// pane fakes, shared by the poller / plan / prompt / accept-trust cases
// ---------------------------------------------------------------------------

/// A fake answering a fixed env dump + pane for every session, every verb else
/// succeeding silently.
fn session_fake(env_dump: &'static str, pane: &'static str) -> FakeTmux {
    FakeTmux::new(move |args| match args[0] {
        "capture-pane" => TmuxResult {
            stdout: pane.to_string(),
            ..Default::default()
        },
        "show-environment" => TmuxResult {
            stdout: env_dump.to_string(),
            ..Default::default()
        },
        _ => TmuxResult::default(),
    })
}

/// A fake whose pane text is answered from a script: each `capture-pane` returns
/// the next entry (the last entry repeats forever).
fn scripted_panes(panes: &[&str]) -> FakeTmux {
    let owned: Vec<String> = panes.iter().map(|s| (*s).to_string()).collect();
    let seen = Cell::new(0usize);
    FakeTmux::new(move |args| {
        if args[0] == "capture-pane" {
            let i = seen.get().min(owned.len() - 1);
            seen.set(seen.get() + 1);
            return TmuxResult {
                stdout: owned[i].clone(),
                ..Default::default()
            };
        }
        TmuxResult::default()
    })
}

const CLAUDE_READY: &str = "Remote Control active https://claude.ai/code/session_01RC";
const CLAUDE_TRUST: &str = "Quick safety check: Yes, I trust this folder";
const CLAUDE_BYPASS: &str = "WARNING: Bypass Permissions mode\n1. No, exit\n2. Yes, I accept";
const CLAUDE_NEEDS_AUTH: &str = "You are not logged in. Run /login to continue.";
const CLAUDE_STARTING: &str = "Remote Control connecting";

#[test]
fn wait_reaches_ready_and_delivers_the_kickoff() {
    let f = scripted_panes(&[CLAUDE_STARTING, CLAUDE_READY]);
    let ticks = Cell::new(Duration::ZERO);
    let eng = engine_at_home(&f, &ticks);
    let mut opts = CreateOptions::new(RcKind::ClaudeRc);
    opts.slug = "abc123".to_string();
    opts.prompt = "do it".to_string();
    let session = eng.create(opts).unwrap();

    assert_eq!(session.state, RcState::Ready);
    assert_eq!(
        session.url.as_deref(),
        Some("https://claude.ai/code/session_01RC")
    );
    assert_eq!(
        f.call_with("send-keys").unwrap(),
        vec!["send-keys", "-t", "rc-abc123", "-l", "--", "do it"]
    );
    // One poll tick + the post-ready settle.
    assert_eq!(ticks.get(), DEFAULT_POLL_EVERY + PROMPT_DELIVER_SETTLE);
}

#[test]
fn wait_auto_accepts_trust_with_exactly_one_enter() {
    let f = scripted_panes(&[CLAUDE_TRUST, CLAUDE_TRUST, CLAUDE_READY]);
    let ticks = Cell::new(Duration::ZERO);
    let eng = engine_at_home(&f, &ticks);
    let mut opts = CreateOptions::new(RcKind::ClaudeRc);
    opts.slug = "abc123".to_string();
    opts.wait = true;
    let session = eng.create(opts).unwrap();

    // The SECOND trust pane is not re-accepted (trust_accepted latches), so the
    // poller reports needs-trust and stops rather than hammering Enter.
    assert_eq!(session.state, RcState::NeedsTrust);
    let enters: Vec<_> = f
        .calls()
        .into_iter()
        .filter(|c| c.last().map(String::as_str) == Some("Enter"))
        .collect();
    assert_eq!(enters, vec![vec!["send-keys", "-t", "rc-abc123", "Enter"]]);
    assert!(!f.any_arg("Down"), "trust is Enter only, never Down");
}

#[test]
fn wait_bypass_dialog_sends_down_then_enter_once() {
    let f = scripted_panes(&[CLAUDE_BYPASS, CLAUDE_READY]);
    let ticks = Cell::new(Duration::ZERO);
    let eng = engine_at_home(&f, &ticks);
    let mut opts = CreateOptions::new(RcKind::ClaudeRc);
    opts.slug = "abc123".to_string();
    opts.permission_mode = "skip".to_string(); // resolves to bypassPermissions
    opts.wait = true;
    let session = eng.create(opts).unwrap();

    assert_eq!(session.state, RcState::Ready);
    let keys: Vec<String> = f
        .calls()
        .into_iter()
        .filter(|c| c[0] == "send-keys")
        .map(|c| c.last().unwrap().clone())
        .collect();
    assert_eq!(keys, vec!["Down", "Enter"]);
}

#[test]
fn wait_bypass_dialog_is_ignored_without_a_bypass_posture() {
    // The same pane WITHOUT the bypass posture must never draw a keypress — a
    // look-alike screen would otherwise get a stray Down/Enter.
    let f = scripted_panes(&[CLAUDE_BYPASS, CLAUDE_READY]);
    let ticks = Cell::new(Duration::ZERO);
    let eng = engine_at_home(&f, &ticks);
    let mut opts = CreateOptions::new(RcKind::ClaudeRc);
    opts.slug = "abc123".to_string();
    opts.permission_mode = "auto".to_string();
    opts.wait = true;
    eng.create(opts).unwrap();
    assert!(!f.any_arg("Down"));
}

#[test]
fn wait_bypass_accept_failure_stays_retryable() {
    // A transient send-keys failure must NOT latch bypass_accepted, or the
    // session stalls until timeout with the dialog still up.
    let f = FakeTmux::new(|args| match args[0] {
        "capture-pane" => TmuxResult {
            stdout: CLAUDE_BYPASS.to_string(),
            ..Default::default()
        },
        "send-keys" => TmuxResult {
            code: 1,
            stderr: "server busy".to_string(),
            ..Default::default()
        },
        _ => TmuxResult::default(),
    });
    let ticks = Cell::new(Duration::ZERO);
    let eng = engine_at_home(&f, &ticks);
    let mut opts = CreateOptions::new(RcKind::ClaudeRc);
    opts.slug = "abc123".to_string();
    opts.permission_mode = "skip".to_string();
    opts.wait = true;
    let session = eng.create(opts).unwrap();
    assert_eq!(session.state, RcState::Starting, "never classified");
    assert!(
        f.count_with("send-keys") > 2,
        "the Down retry must repeat every tick, got {} sends",
        f.count_with("send-keys")
    );
}

// Mirrors Go TestCreateWaitDeadSession (rc_test.go:855).
#[test]
fn wait_reports_dead_when_the_session_vanishes_mid_poll() {
    let f = FakeTmux::new(|args| {
        if args[0] == "capture-pane" {
            return TmuxResult {
                code: 1,
                stderr: "can't find pane: rc-deadx".to_string(),
                ..Default::default()
            };
        }
        TmuxResult::default()
    });
    let ticks = Cell::new(Duration::ZERO);
    let eng = engine_at_home(&f, &ticks);
    let mut opts = CreateOptions::new(RcKind::Shell);
    opts.slug = "deadx".to_string();
    opts.wait = true;
    let session = eng.create(opts).unwrap();
    assert_eq!(session.state, RcState::Dead);
    // Reported immediately, not after burning the whole 20 s budget.
    assert_eq!(ticks.get(), Duration::ZERO);
}

#[test]
fn wait_times_out_on_a_never_ready_pane() {
    // A transient (non-missing) capture failure keeps polling, and a pane that
    // stays `starting` exhausts the deadline — the injected clock makes both
    // observable without burning 20 real seconds.
    let flip = Cell::new(false);
    let f = FakeTmux::new(move |args| {
        if args[0] == "capture-pane" {
            flip.set(!flip.get());
            return if flip.get() {
                TmuxResult {
                    code: 1,
                    stderr: "server not responding".to_string(),
                    ..Default::default()
                }
            } else {
                TmuxResult {
                    stdout: CLAUDE_STARTING.to_string(),
                    ..Default::default()
                }
            };
        }
        TmuxResult::default()
    });
    let ticks = Cell::new(Duration::ZERO);
    let eng = engine_at_home(&f, &ticks);
    let mut opts = CreateOptions::new(RcKind::ClaudeRc);
    opts.slug = "abc123".to_string();
    opts.wait = true;
    let session = eng.create(opts).unwrap();

    assert_eq!(session.state, RcState::Starting);
    assert!(ticks.get() >= DEFAULT_WAIT_TIMEOUT, "deadline exhausted");
    assert_eq!(
        f.count_with("capture-pane"),
        (DEFAULT_WAIT_TIMEOUT.as_millis() / DEFAULT_POLL_EVERY.as_millis() + 1) as usize
    );
    assert!(!f.any_arg("Enter"), "a never-ready pane gets no keystrokes");
}

#[test]
fn wait_stops_on_needs_auth_without_delivering() {
    let f = scripted_panes(&[CLAUDE_NEEDS_AUTH]);
    let ticks = Cell::new(Duration::ZERO);
    let eng = engine_at_home(&f, &ticks);
    let mut opts = CreateOptions::new(RcKind::ClaudeRc);
    opts.slug = "abc123".to_string();
    opts.prompt = "do it".to_string();
    let session = eng.create(opts).unwrap();
    assert_eq!(session.state, RcState::NeedsAuth);
    assert!(
        f.call_with("send-keys").is_none(),
        "a kickoff is delivered ONLY to a ready session"
    );
}

// Mirrors Go TestCreateKickoffDeliveryFailureIsAnError (plan_test.go:277).
#[test]
fn kickoff_delivery_failure_is_the_create_outcome() {
    let f = FakeTmux::new(|args| match args[0] {
        "capture-pane" => TmuxResult {
            stdout: "ready pane text".to_string(),
            ..Default::default()
        },
        "send-keys" => TmuxResult {
            code: 1,
            stderr: "send-keys: server exited unexpectedly".to_string(),
            ..Default::default()
        },
        _ => TmuxResult::default(),
    });
    let ticks = Cell::new(Duration::ZERO);
    let eng = engine_at_home(&f, &ticks);
    let mut opts = CreateOptions::new(RcKind::Shell);
    opts.slug = "abc123".to_string();
    opts.prompt = "go".to_string();
    let err = eng.create(opts).unwrap_err();
    assert_eq!(err.exit_code(), 1);
    assert_eq!(
        err.to_string(),
        "session rc-abc123 is ready but kickoff delivery failed: send-keys: server exited unexpectedly"
    );
}

// Mirrors Go TestCreateKickoffDeliveryToKilledSessionIsDead (plan_test.go:301).
#[test]
fn kickoff_to_a_killed_session_is_dead_not_an_error() {
    let f = FakeTmux::new(|args| match args[0] {
        "capture-pane" => TmuxResult {
            stdout: "ready pane text".to_string(),
            ..Default::default()
        },
        "send-keys" => TmuxResult {
            code: 1,
            stderr: "can't find session: rc-abc123".to_string(),
            ..Default::default()
        },
        _ => TmuxResult::default(),
    });
    let ticks = Cell::new(Duration::ZERO);
    let eng = engine_at_home(&f, &ticks);
    let mut opts = CreateOptions::new(RcKind::Shell);
    opts.slug = "abc123".to_string();
    opts.prompt = "go".to_string();
    let session = eng.create(opts).unwrap();
    assert_eq!(session.state, RcState::Dead);
    assert_eq!(session.url, None);
}

// ---------------------------------------------------------------------------
// plan delivery (Go plan_test.go)
// ---------------------------------------------------------------------------

// Mirrors Go TestCreatePlanWritesFile (plan_test.go:62).
#[test]
fn create_plan_writes_the_file_and_delivers_the_composed_kickoff() {
    use std::os::unix::fs::PermissionsExt;

    let home = tempfile::tempdir().unwrap();
    // Non-empty pane ⇒ the shell classifier reports ready.
    let f = session_fake("", "[shed:x] ~ $");
    let ticks = Cell::new(Duration::ZERO);
    let env = home_env(home.path().to_str().unwrap());
    let eng = engine(&f, &ticks).with_env(env);

    let plan = "# Plan\n\nDo the thing.\n";
    let mut opts = CreateOptions::new(RcKind::Shell);
    opts.slug = "abc123".to_string();
    opts.plan = plan.to_string();
    let session = eng.create(opts).unwrap();
    assert_eq!(session.state, RcState::Ready);

    let path = home.path().join(".shed-plans").join("plan-abc123.md");
    assert_eq!(std::fs::read_to_string(&path).unwrap(), plan);
    assert_eq!(
        std::fs::metadata(&path).unwrap().permissions().mode() & 0o777,
        0o600
    );

    // A single-line kickoff ⇒ typed with `send-keys -l`, carrying the ABSOLUTE
    // plan path.
    let sent = f.call_with("send-keys").unwrap();
    let text = sent.last().unwrap();
    assert!(text.contains(path.to_str().unwrap()), "{text}");
    assert!(text.contains("implement it"), "{text}");
    // The plan file is written AFTER tmux accepted the name.
    let verbs: Vec<String> = f.calls().into_iter().map(|c| c[0].clone()).collect();
    assert_eq!(verbs[0], "new-session");
}

// Mirrors Go TestCreatePlanFramingPrepended (plan_test.go:106).
#[test]
fn create_plan_framing_leads_and_pastes_as_a_block() {
    let home = tempfile::tempdir().unwrap();
    let f = session_fake("", "ready pane text");
    let ticks = Cell::new(Duration::ZERO);
    let env = home_env(home.path().to_str().unwrap());
    let eng = engine(&f, &ticks).with_env(env);
    let mut opts = CreateOptions::new(RcKind::Shell);
    opts.slug = "abc123".to_string();
    opts.plan = "plan body".to_string();
    opts.plan_framing = "focus on X first".to_string();
    eng.create(opts).unwrap();

    // Framing makes the kickoff MULTI-line ⇒ bracketed paste, not `send-keys -l`.
    let buf = f.call_with("set-buffer").unwrap();
    assert!(buf.last().unwrap().starts_with("focus on X first\n\n"));
    assert!(f.call_with("paste-buffer").is_some());
    assert!(!f.any_arg("-l"));
}

// Mirrors Go TestCreatePlanWriteFailureKillsSession (plan_test.go:256).
#[test]
fn plan_write_failure_kills_the_just_created_session() {
    let f = FakeTmux::ok();
    let ticks = Cell::new(Duration::ZERO);
    // A control-char HOME makes the plan path fail AFTER the session exists;
    // an explicit workdir keeps create-side resolution fine.
    let env = home_env("/home/\u{1b}evil");
    let eng = engine(&f, &ticks).with_env(env);
    let mut opts = CreateOptions::new(RcKind::Shell);
    opts.slug = "abc123".to_string();
    opts.workdir = "/tmp".to_string();
    opts.plan = "plan body".to_string();
    let err = eng.create(opts).unwrap_err();

    assert_eq!(err.exit_code(), 2);
    assert!(f.call_with("new-session").is_some(), "session was created");
    assert_eq!(
        f.call_with("kill-session").unwrap(),
        vec!["kill-session", "-t", "rc-abc123"],
        "a failed plan create must leave nothing behind"
    );
}

// Mirrors Go TestCreateDuplicateSlugLeavesPlanFileUntouched (plan_test.go:227).
#[test]
fn duplicate_slug_never_clobbers_the_live_session_plan() {
    let home = tempfile::tempdir().unwrap();
    let dir = home.path().join(".shed-plans");
    std::fs::create_dir_all(&dir).unwrap();
    let existing = dir.join("plan-abc123.md");
    std::fs::write(&existing, "live session's plan").unwrap();

    let f = FakeTmux::new(|args| {
        if args[0] == "new-session" {
            return TmuxResult {
                code: 1,
                stderr: "duplicate session: rc-abc123".to_string(),
                ..Default::default()
            };
        }
        TmuxResult::default()
    });
    let ticks = Cell::new(Duration::ZERO);
    let env = home_env(home.path().to_str().unwrap());
    let eng = engine(&f, &ticks).with_env(env);
    let mut opts = CreateOptions::new(RcKind::Shell);
    opts.slug = "abc123".to_string();
    opts.plan = "usurper plan".to_string();
    let err = eng.create(opts).unwrap_err();

    assert_eq!(err.exit_code(), 3);
    assert_eq!(
        std::fs::read_to_string(&existing).unwrap(),
        "live session's plan"
    );
}

// ---------------------------------------------------------------------------
// the hub-ensure hook (ops.go:237 + clirc.go:591's kill-switch)
// ---------------------------------------------------------------------------

#[test]
fn ensure_hub_fires_after_a_successful_create() {
    let f = FakeTmux::ok();
    let ticks = Cell::new(Duration::ZERO);
    let fired = Cell::new(0);
    let eng = engine_at_home(&f, &ticks).with_ensure_hub(|| fired.set(fired.get() + 1));
    let mut opts = CreateOptions::new(RcKind::Shell);
    opts.slug = "abc123".to_string();
    eng.create(opts).unwrap();
    assert_eq!(fired.get(), 1);
}

#[test]
fn ensure_hub_is_neutralized_by_the_kill_switch() {
    let f = FakeTmux::ok();
    let ticks = Cell::new(Duration::ZERO);
    let fired = Cell::new(0);
    let env = env_from(&[("HOME", "/home/shed"), (ENV_NO_HUB, "1")]);
    let eng = engine(&f, &ticks)
        .with_env(env)
        .with_ensure_hub(|| fired.set(fired.get() + 1));
    let mut opts = CreateOptions::new(RcKind::Shell);
    opts.slug = "abc123".to_string();
    eng.create(opts).unwrap();
    assert_eq!(
        fired.get(),
        0,
        "SHED_RC_NO_HUB must suppress the side effect"
    );
}

#[test]
fn ensure_hub_never_fires_on_a_failed_create() {
    // (a) a validation failure — before the session exists at all;
    // (b) a plan-write failure — AFTER the session exists, which is exactly
    //     where Go's `defer` placement makes the difference.
    for (name, home, workdir, plan, slug) in [
        ("validation", "/home/shed", "", "", "Bad_Slug"),
        (
            "plan write",
            "/home/\u{1b}evil",
            "/tmp",
            "plan body",
            "abc123",
        ),
    ] {
        let f = FakeTmux::ok();
        let ticks = Cell::new(Duration::ZERO);
        let fired = Cell::new(0);
        let env = home_env(home);
        let eng = engine(&f, &ticks)
            .with_env(env)
            .with_ensure_hub(|| fired.set(fired.get() + 1));
        let mut opts = CreateOptions::new(RcKind::Shell);
        opts.slug = slug.to_string();
        opts.workdir = workdir.to_string();
        opts.plan = plan.to_string();
        assert!(eng.create(opts).is_err(), "case {name}");
        assert_eq!(fired.get(), 0, "case {name}");
    }
}

#[test]
fn ensure_hub_fires_even_when_the_kickoff_delivery_fails() {
    // Go registers the defer BEFORE the wait, so a post-ready delivery failure
    // still ensures the hub (the session is live and wants watching).
    let f = FakeTmux::new(|args| match args[0] {
        "capture-pane" => TmuxResult {
            stdout: "ready pane text".to_string(),
            ..Default::default()
        },
        "send-keys" => TmuxResult {
            code: 1,
            stderr: "server exited unexpectedly".to_string(),
            ..Default::default()
        },
        _ => TmuxResult::default(),
    });
    let ticks = Cell::new(Duration::ZERO);
    let fired = Cell::new(0);
    let eng = engine_at_home(&f, &ticks).with_ensure_hub(|| fired.set(fired.get() + 1));
    let mut opts = CreateOptions::new(RcKind::Shell);
    opts.slug = "abc123".to_string();
    opts.prompt = "go".to_string();
    assert!(eng.create(opts).is_err());
    assert_eq!(fired.get(), 1);
}

// ---------------------------------------------------------------------------
// the preseed seam (ops.go:202) — C5 supplies the implementations
// ---------------------------------------------------------------------------

#[test]
fn preseed_runs_before_new_session_and_never_fails_the_create() {
    let f = FakeTmux::ok();
    let ticks = Cell::new(Duration::ZERO);
    let saw = Cell::new(String::new());
    let warned = Cell::new(String::new());
    let eng = engine_at_home(&f, &ticks)
        .with_warn(|m| warned.set(m.to_string()))
        .with_preseed(|kind, workdir, getenv| {
            // The hook sees the resolved workdir and can read the SAME env the
            // engine resolved it from.
            saw.set(format!("{}|{workdir}|{}", kind.as_str(), getenv("HOME")));
            Err("hooks.json lives on another device".to_string())
        });
    let mut opts = CreateOptions::new(RcKind::Cursor);
    opts.slug = "abc123".to_string();
    let session = eng.create(opts).unwrap();

    assert_eq!(
        session.state,
        RcState::Starting,
        "the create still succeeds"
    );
    assert_eq!(saw.take(), "cursor|/home/shed|/home/shed");
    assert_eq!(
        warned.take(),
        "cursor preseed skipped: hooks.json lives on another device"
    );
}

/// The REAL preseed dispatch, on a `~/.claude.json` nested far past the JSON
/// parser's depth cap — end to end through the engine, not just the module.
///
/// This is the regression cell for a process ABORT: an uncapped recursive parser
/// overflows the stack on this input, and a Rust stack overflow is a SIGABRT, so
/// the failure mode was `sx` dying mid-create rather than a create that carries
/// on. Contract: warn-and-skip, `new-session` still runs, file untouched.
#[test]
fn a_pathologically_deep_claude_json_warns_and_the_create_still_succeeds() {
    let home = tempfile::tempdir().unwrap();
    let path = home.path().join(".claude.json");
    let depth = 100_000;
    let deep = format!("{{\"a\":{}{}}}", "[".repeat(depth), "]".repeat(depth));
    std::fs::write(&path, &deep).unwrap();

    let f = FakeTmux::ok();
    let ticks = Cell::new(Duration::ZERO);
    let warned = Cell::new(String::new());
    let eng = engine(&f, &ticks)
        .with_env(home_env(home.path().to_str().unwrap()))
        .with_warn(|m| warned.set(m.to_string()))
        .with_preseed(crate::preseed::dispatch);

    let mut opts = CreateOptions::new(RcKind::ClaudeRc);
    opts.slug = "abc123".to_string();
    let session = eng.create(opts).unwrap();

    assert_eq!(
        session.state,
        RcState::Starting,
        "the create still succeeds"
    );
    let warning = warned.take();
    assert!(
        warning.starts_with("claude preseed skipped: ") && warning.contains("exceeded max depth"),
        "want the depth refusal on the warn sink, got: {warning}"
    );
    assert!(
        f.calls().iter().any(|c| c[0] == "new-session"),
        "the session must still have been created"
    );
    // Merge — never clobber: a declined preseed leaves the bytes as they were.
    assert_eq!(std::fs::read_to_string(&path).unwrap(), deep);
}

// ---------------------------------------------------------------------------
// list / probe (Go TestListParsesAll :874, TestProbeMissing :752)
// ---------------------------------------------------------------------------

#[test]
fn list_parses_every_rc_session() {
    let f = FakeTmux::new(|args| match args[0] {
        "ls" => TmuxResult {
            stdout: "rc-aaa\nother\nrc-bbb\n".to_string(),
            ..Default::default()
        },
        "show-environment" => {
            if args.contains(&"rc-aaa") {
                TmuxResult {
                    stdout: "SHED_RC_V=2\nSHED_RC_KIND=claude-rc\nSHED_RC_DISPLAY_NAME=A"
                        .to_string(),
                    ..Default::default()
                }
            } else {
                TmuxResult::default() // rc-bbb: legacy/unmanaged
            }
        }
        "capture-pane" => TmuxResult {
            stdout: CLAUDE_READY.to_string(),
            ..Default::default()
        },
        _ => TmuxResult::default(),
    });
    let ticks = Cell::new(Duration::ZERO);
    let eng = engine(&f, &ticks);
    let resp = eng.list(None);

    assert_eq!(resp.rc_sessions.len(), 2, "the non-rc session is skipped");
    assert_eq!(resp.rc_sessions[0].slug, "aaa");
    assert!(resp.rc_sessions[0].managed);
    assert_eq!(resp.rc_sessions[0].display_name.as_deref(), Some("A"));
    assert_eq!(resp.rc_sessions[1].slug, "bbb");
    assert!(!resp.rc_sessions[1].managed);
    assert_eq!(resp.rc_sessions[1].kind, RcKind::ClaudeBroker);
    // The one-shot engine never embeds capabilities (the CLI layer does; C5).
    assert!(resp.capabilities.is_none());
}

#[test]
fn list_is_empty_when_tmux_has_no_server() {
    let f = FakeTmux::new(|_| TmuxResult {
        code: 1,
        stderr: "no server running on /tmp/tmux-501/default".to_string(),
        ..Default::default()
    });
    let ticks = Cell::new(Duration::ZERO);
    assert!(engine(&f, &ticks).list(None).rc_sessions.is_empty());
}

#[test]
fn probe_uses_the_display_fallback_for_an_unstored_name() {
    let f = FakeTmux::new(|args| match args[0] {
        "capture-pane" => TmuxResult {
            stdout: CLAUDE_READY.to_string(),
            ..Default::default()
        },
        "show-environment" => TmuxResult {
            stdout: "SHED_RC_V=2\nSHED_RC_KIND=claude-rc".to_string(),
            ..Default::default()
        },
        _ => TmuxResult::default(),
    });
    let ticks = Cell::new(Duration::ZERO);
    let eng = engine(&f, &ticks);
    let fallback = |slug: &str| format!("fb/{slug}");
    let session = eng.probe("abc", Some(&fallback)).unwrap();
    assert_eq!(session.display_name.as_deref(), Some("fb/abc"));
    assert_eq!(session.state, RcState::Ready);

    // …and with no fallback the field is omitted entirely.
    let bare = eng.probe("abc", None).unwrap();
    assert_eq!(bare.display_name, None);
}

#[test]
fn probe_missing_session_is_the_exit_4_class() {
    let f = FakeTmux::new(|_| TmuxResult {
        code: 1,
        stderr: "no server running on /tmp/tmux".to_string(),
        ..Default::default()
    });
    let ticks = Cell::new(Duration::ZERO);
    let err = engine(&f, &ticks).probe("gone", None).unwrap_err();
    assert_eq!(err.exit_code(), 4);
    assert_eq!(err.to_string(), "rc session not found: rc-gone");
}

#[test]
fn probe_transient_capture_failure_is_the_generic_class() {
    let f = FakeTmux::new(|_| TmuxResult {
        code: 1,
        stderr: "protocol version mismatch".to_string(),
        ..Default::default()
    });
    let ticks = Cell::new(Duration::ZERO);
    let err = engine(&f, &ticks).probe("abc", None).unwrap_err();
    assert_eq!(err.exit_code(), 1);
    assert_eq!(
        err.to_string(),
        "tmux capture-pane failed: protocol version mismatch"
    );
}

// ---------------------------------------------------------------------------
// prompt (Go TestPromptGuards :790, TestPromptSendFailureSurfaces :838)
// ---------------------------------------------------------------------------

const READY_CLAUDE_ENV: &str = "SHED_RC_V=2\nSHED_RC_KIND=claude-rc\nSHED_RC_ID=id-1";

#[test]
fn prompt_delivers_to_a_ready_session() {
    let f = session_fake(READY_CLAUDE_ENV, CLAUDE_READY);
    let ticks = Cell::new(Duration::ZERO);
    engine(&f, &ticks)
        .prompt(&PromptOptions {
            slug: "abc".to_string(),
            text: "do it".to_string(),
            session_id: "id-1".to_string(),
        })
        .unwrap();
    assert_eq!(
        f.call_with("send-keys").unwrap(),
        vec!["send-keys", "-t", "rc-abc", "-l", "--", "do it"]
    );
}

#[test]
fn prompt_guards_table() {
    struct Case {
        name: &'static str,
        env: &'static str,
        pane: &'static str,
        session_id: &'static str,
        text: &'static str,
        code: i32,
        message: &'static str,
    }
    let cases = [
        Case {
            name: "session-id mismatch",
            env: READY_CLAUDE_ENV,
            pane: CLAUDE_READY,
            session_id: "other",
            text: "x",
            code: 4,
            message: "rc session not found: session id mismatch (recreated?)",
        },
        Case {
            name: "broker takes no typed input",
            env: "SHED_RC_V=2\nSHED_RC_KIND=claude-broker",
            pane: "Connected https://claude.ai/code?environment=env_1",
            session_id: "",
            text: "x",
            code: 2,
            message: "invalid arguments: kind \"claude-broker\" does not accept a prompt",
        },
        Case {
            name: "not ready",
            env: READY_CLAUDE_ENV,
            pane: CLAUDE_STARTING,
            session_id: "",
            text: "x",
            code: 2,
            message: "invalid arguments: session not ready (state=starting)",
        },
        Case {
            name: "needs-auth is not ready either",
            env: READY_CLAUDE_ENV,
            pane: CLAUDE_NEEDS_AUTH,
            session_id: "",
            text: "x",
            code: 2,
            message: "invalid arguments: session not ready (state=needs-auth)",
        },
        Case {
            name: "needs-trust is not ready either",
            env: READY_CLAUDE_ENV,
            pane: CLAUDE_TRUST,
            session_id: "",
            text: "x",
            code: 2,
            message: "invalid arguments: session not ready (state=needs-trust)",
        },
    ];
    for c in cases {
        let f = session_fake(c.env, c.pane);
        let ticks = Cell::new(Duration::ZERO);
        let err = engine(&f, &ticks)
            .prompt(&PromptOptions {
                slug: "abc".to_string(),
                text: c.text.to_string(),
                session_id: c.session_id.to_string(),
            })
            .unwrap_err();
        assert_eq!(err.exit_code(), c.code, "case {}", c.name);
        assert_eq!(err.to_string(), c.message, "case {}", c.name);
        assert!(
            f.call_with("send-keys").is_none(),
            "case {}: nothing may be typed",
            c.name
        );
    }
}

#[test]
fn prompt_rejects_control_chars_without_touching_tmux() {
    let f = FakeTmux::ok();
    let ticks = Cell::new(Duration::ZERO);
    let err = engine(&f, &ticks)
        .prompt(&PromptOptions {
            slug: "abc".to_string(),
            text: "a\u{1b}b".to_string(),
            session_id: String::new(),
        })
        .unwrap_err();
    assert_eq!(err.exit_code(), 2);
    assert_eq!(
        err.to_string(),
        "invalid arguments: text contains an unsupported control character"
    );
    assert!(f.calls().is_empty());
}

#[test]
fn prompt_normalizes_newlines_and_pastes_multi_line_text() {
    // CRLF normalizes to LF (so it is NOT rejected as a control char) and the
    // resulting multi-line text goes through the bracketed paste.
    let f = session_fake(READY_CLAUDE_ENV, CLAUDE_READY);
    let ticks = Cell::new(Duration::ZERO);
    engine(&f, &ticks)
        .prompt(&PromptOptions {
            slug: "abc".to_string(),
            text: "line one\r\nline two".to_string(),
            session_id: String::new(),
        })
        .unwrap();
    let buf = f.call_with("set-buffer").unwrap();
    assert_eq!(buf.last().unwrap(), "line one\nline two");
}

// Mirrors Go TestPromptSendFailureSurfaces (rc_test.go:838).
#[test]
fn prompt_send_failure_on_a_vanished_session_is_exit_4() {
    let f = FakeTmux::new(|args| match args[0] {
        "capture-pane" => TmuxResult {
            stdout: CLAUDE_READY.to_string(),
            ..Default::default()
        },
        "show-environment" => TmuxResult {
            stdout: READY_CLAUDE_ENV.to_string(),
            ..Default::default()
        },
        "send-keys" => TmuxResult {
            code: 1,
            stderr: "can't find pane: rc-abc".to_string(),
            ..Default::default()
        },
        _ => TmuxResult::default(),
    });
    let ticks = Cell::new(Duration::ZERO);
    let err = engine(&f, &ticks)
        .prompt(&PromptOptions {
            slug: "abc".to_string(),
            text: "go".to_string(),
            session_id: String::new(),
        })
        .unwrap_err();
    assert_eq!(err.exit_code(), 4);
    assert_eq!(err.to_string(), "rc session not found: rc-abc");
}

// ---------------------------------------------------------------------------
// kill (Go TestKillIdempotent, rc_test.go:726)
// ---------------------------------------------------------------------------

#[test]
fn kill_is_idempotent_for_a_missing_session() {
    for stderr in ["can't find session: rc-x", "no server running on /tmp/tmux"] {
        let f = FakeTmux::new(move |_| TmuxResult {
            code: 1,
            stderr: stderr.to_string(),
            ..Default::default()
        });
        let ticks = Cell::new(Duration::ZERO);
        // Exit 0 on a missing slug is a PINNED contract (plan 009 §3.4).
        assert!(engine(&f, &ticks).kill("x").is_ok(), "stderr {stderr:?}");
        assert_eq!(
            f.call_with("kill-session").unwrap(),
            vec!["kill-session", "-t", "rc-x"]
        );
    }
}

#[test]
fn kill_surfaces_a_real_failure() {
    let f = FakeTmux::new(|_| TmuxResult {
        code: 1,
        stderr: "permission denied".to_string(),
        ..Default::default()
    });
    let ticks = Cell::new(Duration::ZERO);
    let err = engine(&f, &ticks).kill("x").unwrap_err();
    assert_eq!(err.exit_code(), 1);
    assert_eq!(
        err.to_string(),
        "tmux kill-session failed: permission denied"
    );
}

// ---------------------------------------------------------------------------
// accept-trust (Go TestAcceptTrustOnlyWhenPromptPresent, rc_test.go:762)
// ---------------------------------------------------------------------------

#[test]
fn accept_trust_only_when_the_dialog_is_present() {
    // Present ⇒ exactly one Enter.
    let f = session_fake("", CLAUDE_TRUST);
    let ticks = Cell::new(Duration::ZERO);
    engine(&f, &ticks).accept_trust("abc").unwrap();
    assert_eq!(
        f.call_with("send-keys").unwrap(),
        vec!["send-keys", "-t", "rc-abc", "Enter"]
    );
    assert_eq!(f.count_with("send-keys"), 1);

    // Absent (ready, and needs-auth) ⇒ no keystroke at all.
    for pane in [CLAUDE_READY, CLAUDE_NEEDS_AUTH] {
        let g = session_fake("", pane);
        let ticks = Cell::new(Duration::ZERO);
        engine(&g, &ticks).accept_trust("abc").unwrap();
        assert!(
            g.call_with("send-keys").is_none(),
            "pane {pane:?} must not draw a keypress"
        );
    }
}

#[test]
fn accept_trust_on_a_missing_session_is_exit_4() {
    // Pinned by the plan (§3.4): accept-trust on a gone slug is exit 4, unlike
    // kill's idempotent 0.
    let f = FakeTmux::new(|_| TmuxResult {
        code: 1,
        stderr: "can't find session: rc-gone".to_string(),
        ..Default::default()
    });
    let ticks = Cell::new(Duration::ZERO);
    let err = engine(&f, &ticks).accept_trust("gone").unwrap_err();
    assert_eq!(err.exit_code(), 4);
    assert_eq!(err.to_string(), "rc session not found: rc-gone");
}

// ---------------------------------------------------------------------------
// error classes + helpers
// ---------------------------------------------------------------------------

#[test]
fn exit_code_classes_and_message_prefixes() {
    for (err, code, text) in [
        (EngineError::bad_args("x"), 2, "invalid arguments: x"),
        (
            EngineError::DuplicateSlug("rc-a".to_string()),
            3,
            "rc session already exists: rc-a",
        ),
        (
            EngineError::SessionNotFound("rc-a".to_string()),
            4,
            "rc session not found: rc-a",
        ),
        (EngineError::Other("boom".to_string()), 1, "boom"),
    ] {
        assert_eq!(err.exit_code(), code);
        assert_eq!(err.to_string(), text);
    }
}

#[test]
fn state_wire_matches_the_dto_serialization() {
    // `state_wire` hand-writes the tokens Go's State string type prints; keep it
    // honest against the serde derive that produces the same values on the wire.
    for state in [
        RcState::Starting,
        RcState::Ready,
        RcState::Reconnecting,
        RcState::NeedsTrust,
        RcState::NeedsAuth,
        RcState::Dead,
    ] {
        let json = serde_json::to_string(&state).unwrap();
        assert_eq!(json, format!("\"{}\"", state_wire(state)));
    }
}

#[test]
fn first_non_empty_picks_the_first_set_value() {
    assert_eq!(first_non_empty(["", "b", "c"]), "b");
    assert_eq!(first_non_empty(["a", "b"]), "a");
    assert_eq!(first_non_empty(["", ""]), "");
    assert_eq!(first_non_empty([]), "");
}

#[test]
fn real_bin_probe_answers_for_a_known_builtin_and_a_missing_binary() {
    // `sh` exists on every machine this runs on; the nonsense name does not.
    // Both shell verbs are exercised — `-ic` is the native-machine path.
    assert!(real_bin_probe("sh", false));
    assert!(!real_bin_probe("definitely-not-a-real-binary-xyzzy", false));
    assert!(real_bin_probe("sh", true));
}

/// The checked captures' error mapping (`checkedCapture`, `ops.go:381`), on both
/// the scrollback and visible-frame variants (plan 010 H3): a gone session is
/// `SessionNotFound` (the hub's disappearance signal), a transient tmux failure
/// stays a generic error (the hub must NOT prune tracked state on it).
#[test]
fn checked_captures_map_gone_vs_transient() {
    let gone = FakeTmux::new(|_| TmuxResult {
        stderr: "can't find session: rc-x".to_string(),
        code: 1,
        ..Default::default()
    });
    let t = Tmux::new(&gone);
    assert!(matches!(
        capture_pane_checked(&t, "rc-x"),
        Err(EngineError::SessionNotFound(_))
    ));
    assert!(matches!(
        capture_visible_pane_checked(&t, "rc-x"),
        Err(EngineError::SessionNotFound(_))
    ));
    assert_eq!(gone.calls()[1], vec!["capture-pane", "-t", "rc-x", "-p"]);

    let hiccup = FakeTmux::new(|_| TmuxResult {
        stderr: "server exited unexpectedly".to_string(),
        code: 1,
        ..Default::default()
    });
    let t = Tmux::new(&hiccup);
    assert!(matches!(
        capture_visible_pane_checked(&t, "rc-x"),
        Err(EngineError::Other(_))
    ));
}
