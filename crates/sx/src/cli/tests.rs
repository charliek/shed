//! Dispatch tests — the Rust twin of `internal/ext/clirc/clirc_test.go`'s tables.
//!
//! Everything runs against [`FakeTmux`] with no bin probe, no hub hook, a scripted
//! stdin and captured stdio, so a case asserts exactly what the CLI layer does:
//! flag grammar, stdin framing, the exit-code classes, and the JSON document.

use std::cell::RefCell;
use std::collections::HashMap;
use std::io::{self, Cursor, Write};
use std::rc::Rc;
use std::time::Duration;

use serde_json::Value;
use shed_app::rc_engine::fake::FakeTmux;
use shed_app::rc_engine::tmux::{TmuxResult, TmuxRunner};

use super::*;

/// A `Write` sink whose bytes stay readable after the borrow ends.
#[derive(Clone, Default)]
struct Sink(Rc<RefCell<Vec<u8>>>);

impl Sink {
    fn text(&self) -> String {
        String::from_utf8_lossy(&self.0.borrow()).into_owned()
    }
}

impl Write for Sink {
    fn write(&mut self, buf: &[u8]) -> io::Result<usize> {
        self.0.borrow_mut().extend_from_slice(buf);
        Ok(buf.len())
    }
    fn flush(&mut self) -> io::Result<()> {
        Ok(())
    }
}

/// The env table every case runs under: the engine reads `$HOME` for the default
/// workdir, and nothing here needs a second variable.
const HOME_ENV: &[(&str, &str)] = &[("HOME", "/home/shed")];

struct Out {
    code: i32,
    stdout: String,
    stderr: String,
}

impl Out {
    /// The DTO the command printed (asserting it printed exactly one document).
    fn json(&self) -> Value {
        serde_json::from_str(self.stdout.trim_end())
            .unwrap_or_else(|e| panic!("stdout is not one JSON document ({e}): {:?}", self.stdout))
    }
}

/// Run `args` against a fake tmux with a fixed env table and stdin payload.
fn run_with(runner: &dyn TmuxRunner, env: &[(&str, &str)], stdin: &str, args: &[&str]) -> Out {
    let table: HashMap<String, String> = env
        .iter()
        .map(|(k, v)| ((*k).to_string(), (*v).to_string()))
        .collect();
    let stdout = Sink::default();
    let stderr = Sink::default();
    let deps = Deps {
        runner,
        env: Box::new(move |key| table.get(key).cloned().unwrap_or_default()),
        stdin: RefCell::new(Box::new(Cursor::new(stdin.as_bytes().to_vec()))),
        stdout: RefCell::new(Box::new(stdout.clone())),
        stderr: RefCell::new(Box::new(stderr.clone())),
        // No probe (the create gate is skipped, exactly as a nil BinProbe does in
        // Go's tests) and no hub hook — a unit test forks nothing.
        bin_probe: None,
        ensure_hub: None,
        sleep: Some(Box::new(|_| {})),
        settle: Some(Duration::ZERO),
    };
    let argv: Vec<String> = args.iter().map(|s| (*s).to_string()).collect();
    let code = run(&deps, &argv);
    Out {
        code,
        stdout: stdout.text(),
        stderr: stderr.text(),
    }
}

/// The common case: a fake where every verb succeeds, `$HOME` is `/home/shed`.
fn run_ok(args: &[&str]) -> (Out, FakeTmux) {
    let fake = FakeTmux::ok();
    let out = run_with(&fake, HOME_ENV, "", args);
    (out, fake)
}

fn err(code: i32, stderr_needle: &str, args: &[&str], stdin: &str) {
    let fake = FakeTmux::ok();
    let out = run_with(&fake, HOME_ENV, stdin, args);
    assert_eq!(out.code, code, "args {args:?} stderr={:?}", out.stderr);
    assert!(
        out.stderr.contains(stderr_needle),
        "args {args:?}: stderr {:?} does not contain {stderr_needle:?}",
        out.stderr
    );
    assert!(out.stdout.is_empty(), "args {args:?} wrote to stdout");
}

// ---------------------------------------------------------------------------
// top-level dispatch
// ---------------------------------------------------------------------------

#[test]
fn version_prints_prog_and_crate_version() {
    for args in [&["version"][..], &["rc", "version"][..], &["--version"][..]] {
        let (out, _) = run_ok(args);
        assert_eq!(out.code, 0);
        assert_eq!(
            out.stdout,
            format!("{PROG} {}\n", env!("CARGO_PKG_VERSION")),
            "args {args:?}"
        );
    }
}

#[test]
fn no_args_and_unknown_commands_are_usage_errors() {
    let (out, _) = run_ok(&[]);
    assert_eq!(out.code, 2);
    assert!(out.stderr.contains("usage: sx"));

    let (out, _) = run_ok(&["frobnicate"]);
    assert_eq!(out.code, 2);
    assert!(out.stderr.contains("unknown command \"frobnicate\""));

    // `serve` is deliberately NOT ported (the hub stays the Go binary).
    let (out, _) = run_ok(&["rc", "serve"]);
    assert_eq!(out.code, 2);
    assert!(out.stderr.contains("unknown command \"serve\""));

    let (out, _) = run_ok(&["rc"]);
    assert_eq!(out.code, 2);
    assert!(out.stderr.contains("usage: sx rc"));
}

#[test]
fn help_exits_zero() {
    let (out, _) = run_ok(&["help"]);
    assert_eq!(out.code, 0);
    assert!(out.stderr.contains("usage: sx"));
}

#[test]
fn capabilities_is_stubbed_until_c5() {
    let (out, _) = run_ok(&["rc", "capabilities"]);
    assert_eq!(out.code, 1);
    assert!(out.stderr.contains("capabilities: not yet ported"));
    assert!(out.stdout.is_empty());
}

// ---------------------------------------------------------------------------
// create
// ---------------------------------------------------------------------------

#[test]
fn create_emits_the_dto_and_stamps_sx_provenance() {
    let (out, fake) = run_ok(&[
        "rc", "create", "--kind", "shell", "--slug", "aa1111", "--name", "pinned",
    ]);
    assert_eq!(out.code, 0, "stderr={:?}", out.stderr);
    let dto = out.json();
    assert_eq!(dto["slug"], "aa1111");
    assert_eq!(dto["tmux_session"], "rc-aa1111");
    assert_eq!(dto["kind"], "shell");
    assert_eq!(dto["display_name"], "pinned");
    assert_eq!(dto["workdir"], "/home/shed");
    assert_eq!(dto["managed"], true);
    assert_eq!(dto["state"], "starting");
    // The bare prog name, matching Go's bare `shed-machine-rc` default.
    assert_eq!(dto["created_by"], "sx");
    // ... and the same token reaches the session environment.
    let argv = fake.call_with("new-session").expect("new-session");
    assert!(
        argv.contains(&"SHED_RC_CREATED_BY=sx".to_string()),
        "{argv:?}"
    );
    assert!(argv.contains(&"rc-aa1111".to_string()), "{argv:?}");
}

#[test]
fn create_accepts_every_contracted_flag_form() {
    // -flag value / --flag value / --flag=value / -flag=value all reach the engine.
    let forms: [&[&str]; 4] = [
        &["rc", "create", "-kind", "shell", "-slug", "aa1111"],
        &["rc", "create", "--kind", "shell", "--slug", "aa1111"],
        &["rc", "create", "--kind=shell", "--slug=aa1111"],
        &["rc", "create", "-kind=shell", "-slug=aa1111"],
    ];
    for args in forms {
        let (out, _) = run_ok(args);
        assert_eq!(out.code, 0, "args {args:?} stderr={:?}", out.stderr);
        let dto = out.json();
        assert_eq!(dto["kind"], "shell", "args {args:?}");
        assert_eq!(dto["slug"], "aa1111", "args {args:?}");
    }
}

#[test]
fn create_tolerates_an_empty_target() {
    // shed-core's create_argv ALWAYS passes --target, even when empty.
    let (out, _) = run_ok(&[
        "rc", "create", "--kind", "shell", "--slug", "aa1111", "--target", "",
    ]);
    assert_eq!(out.code, 0, "stderr={:?}", out.stderr);
    // An empty target is absent, not `""` (Go's omitempty).
    assert!(out.json().get("target_label").is_none());
}

#[test]
fn create_rejects_a_stray_positional() {
    err(
        2,
        "invalid arguments: unexpected argument \"oops\"",
        &["rc", "create", "--kind", "shell", "oops"],
        "",
    );
}

#[test]
fn create_rejects_an_unknown_flag() {
    err(
        2,
        "flag provided but not defined: -bogus",
        &["rc", "create", "--bogus", "x"],
        "",
    );
}

#[test]
fn create_rejects_an_unknown_kind() {
    err(
        2,
        "invalid arguments: unknown kind \"bogus\"",
        &["rc", "create", "--kind", "bogus"],
        "",
    );
}

#[test]
fn create_maps_a_duplicate_slug_to_exit_3() {
    let fake = FakeTmux::new(|args| {
        if args.first() == Some(&"new-session") {
            return TmuxResult {
                code: 1,
                stdout: String::new(),
                stderr: "duplicate session: rc-aa1111".to_string(),
            };
        }
        TmuxResult::default()
    });
    let out = run_with(
        &fake,
        HOME_ENV,
        "",
        &["rc", "create", "--kind", "shell", "--slug", "aa1111"],
    );
    assert_eq!(out.code, 3, "stderr={:?}", out.stderr);
    assert!(out.stderr.contains("rc session already exists"));
}

#[test]
fn skip_and_permission_mode_are_mutually_exclusive() {
    err(
        2,
        "--skip and --permission-mode are mutually exclusive",
        &[
            "rc",
            "create",
            "--kind",
            "claude-rc",
            "--skip",
            "--permission-mode",
            "auto",
        ],
        "",
    );
}

#[test]
fn skip_resolves_to_the_generic_skip_posture() {
    let (out, fake) = run_ok(&[
        "rc", "create", "--kind", "codex", "--slug", "aa1111", "--skip",
    ]);
    assert_eq!(out.code, 0, "stderr={:?}", out.stderr);
    // codex's registry mapping for the generic full-bypass posture.
    let argv = fake.call_with("new-session").expect("new-session");
    let inner = argv.last().expect("inner command");
    assert!(
        inner.contains("--dangerously-bypass-approvals-and-sandbox"),
        "inner={inner:?}"
    );
}

// ---------------------------------------------------------------------------
// stdin framing
// ---------------------------------------------------------------------------

#[test]
fn prompt_stdin_and_plan_stdin_are_mutually_exclusive() {
    err(
        2,
        "--prompt-stdin and --plan-stdin are mutually exclusive",
        &[
            "rc",
            "create",
            "--kind",
            "shell",
            "--prompt-stdin",
            "--plan-stdin",
        ],
        "hello\n",
    );
}

#[test]
fn prompt_b64_requires_plan_stdin() {
    err(
        2,
        "--prompt-b64 is only valid with --plan-stdin",
        &["rc", "create", "--kind", "shell", "--prompt-b64", "aGk="],
        "",
    );
}

#[test]
fn prompt_b64_accepts_go_lenient_base64() {
    // Go's StdEncoding IGNORES \r/\n (base64(1)/openssl wrap at 76 cols) and
    // ACCEPTS non-canonical trailing bits — both were rejected by the stock
    // Rust STANDARD engine (C4 review MEDIUM). decode_b64_go mirrors Go.
    assert_eq!(super::decode_b64_go("aGVs\nbG8=").unwrap(), b"hello");
    assert_eq!(super::decode_b64_go("aGVs\r\nbG8=").unwrap(), b"hello");
    // "QR==": 'R' carries non-zero trailing bits; Go decodes it to b"A".
    assert_eq!(super::decode_b64_go("QR==").unwrap(), b"A");
    // Padding is still REQUIRED, like Go.
    assert!(super::decode_b64_go("aGVsbG8").is_err());
}

#[test]
fn prompt_b64_rejects_bad_base64_and_non_utf8() {
    // Not base64 at all.
    err(
        2,
        "--prompt-b64 is not valid base64",
        &[
            "rc",
            "create",
            "--kind",
            "shell",
            "--plan-stdin",
            "--prompt-b64",
            "!!!!",
        ],
        "a plan\n",
    );
    // Valid base64 of a lone C1 byte (0x9b) — rejected BEFORE the string conversion.
    err(
        2,
        "--prompt-b64 does not decode to valid UTF-8",
        &[
            "rc",
            "create",
            "--kind",
            "shell",
            "--plan-stdin",
            "--prompt-b64",
            "mw==",
        ],
        "a plan\n",
    );
}

#[test]
fn empty_stdin_payloads_are_rejected_with_gos_wording() {
    err(
        2,
        "--prompt-stdin given but stdin is empty",
        &["rc", "create", "--kind", "shell", "--prompt-stdin"],
        "\n",
    );
    err(
        2,
        "--plan-stdin given but stdin is empty",
        &["rc", "create", "--kind", "shell", "--plan-stdin"],
        "",
    );
}

#[test]
fn an_oversize_plan_is_rejected_at_the_transport_boundary() {
    let oversize = "x".repeat(PLAN_MAX_BYTES + 1);
    err(
        2,
        &format!("plan exceeds {PLAN_MAX_BYTES} bytes"),
        &["rc", "create", "--kind", "shell", "--plan-stdin"],
        &oversize,
    );
}

/// A fake whose pane has already drawn: a non-empty capture classifies a shell
/// session ready, so the kickoff poller delivers on the first tick.
fn drawn_pane_fake() -> FakeTmux {
    FakeTmux::new(|args| match args.first() {
        Some(&"capture-pane") => TmuxResult {
            code: 0,
            stdout: "$ ".to_string(),
            stderr: String::new(),
        },
        _ => TmuxResult::default(),
    })
}

#[test]
fn prompt_stdin_strips_one_trailing_crlf_and_delivers_the_line() {
    let fake = drawn_pane_fake();
    let out = run_with(
        &fake,
        HOME_ENV,
        "hello world\r\n",
        &[
            "rc",
            "create",
            "--kind",
            "shell",
            "--slug",
            "aa1111",
            "--prompt-stdin",
        ],
    );
    assert_eq!(out.code, 0, "stderr={:?}", out.stderr);
    assert_eq!(out.json()["state"], "ready");
    let typed = fake
        .calls()
        .into_iter()
        .find(|c| c.contains(&"-l".to_string()))
        .expect("a literal send-keys");
    assert_eq!(
        typed,
        vec!["send-keys", "-t", "rc-aa1111", "-l", "--", "hello world"]
    );
}

#[test]
fn prompt_stdin_is_not_a_flag_source() {
    // The one-line read exists so a piped kickoff starting with `-` is text.
    let fake = drawn_pane_fake();
    let out = run_with(
        &fake,
        HOME_ENV,
        "--not-a-flag\n",
        &[
            "rc",
            "create",
            "--kind",
            "shell",
            "--slug",
            "aa1111",
            "--prompt-stdin",
        ],
    );
    assert_eq!(out.code, 0, "stderr={:?}", out.stderr);
    assert!(fake.any_arg("--not-a-flag"));
}

// ---------------------------------------------------------------------------
// probe / prompt / kill / accept-trust
// ---------------------------------------------------------------------------

/// A fake whose every verb reports the target session as gone.
fn missing_session_fake() -> FakeTmux {
    FakeTmux::new(|_| TmuxResult {
        code: 1,
        stdout: String::new(),
        stderr: "can't find session: rc-aa1111".to_string(),
    })
}

#[test]
fn missing_slug_exit_classes() {
    // probe / prompt / accept-trust are exit 4 ...
    for (args, stdin) in [
        (&["rc", "probe", "--slug", "aa1111"][..], ""),
        (&["rc", "accept-trust", "--slug", "aa1111"][..], ""),
        (&["rc", "prompt", "--slug", "aa1111"][..], "hi\n"),
    ] {
        let fake = missing_session_fake();
        let out = run_with(&fake, HOME_ENV, stdin, args);
        assert_eq!(out.code, 4, "args {args:?} stderr={:?}", out.stderr);
        assert!(out.stderr.contains("rc session not found"), "args {args:?}");
    }
    // ... and kill is exit 0: idempotent by contract.
    let fake = missing_session_fake();
    let out = run_with(&fake, HOME_ENV, "", &["rc", "kill", "--slug", "aa1111"]);
    assert_eq!(out.code, 0, "stderr={:?}", out.stderr);
    assert!(out.stdout.is_empty());
}

#[test]
fn slug_is_required() {
    for args in [
        &["rc", "probe"][..],
        &["rc", "kill"][..],
        &["rc", "accept-trust"][..],
        &["rc", "prompt"][..],
    ] {
        err(2, "invalid arguments: --slug is required", args, "hi\n");
    }
}

#[test]
fn prompt_requires_non_empty_stdin() {
    err(
        2,
        "prompt text (stdin) is empty",
        &["rc", "prompt", "--slug", "aa1111"],
        "\n",
    );
}

#[test]
fn list_emits_an_envelope_without_a_capabilities_block() {
    // C4: the Rust list carries `rc_sessions` only — capabilities lands with C5,
    // and the parity harness strips the block from the Go side until then.
    let fake = FakeTmux::ok();
    let out = run_with(&fake, HOME_ENV, "", &["rc", "list"]);
    assert_eq!(out.code, 0, "stderr={:?}", out.stderr);
    let env = out.json();
    assert_eq!(env["rc_sessions"], Value::Array(vec![]));
    assert!(env.get("capabilities").is_none());
}

#[test]
fn subcommands_reject_stray_positionals_and_unknown_flags() {
    err(2, "unexpected argument \"x\"", &["rc", "list", "x"], "");
    err(
        2,
        "unexpected argument \"x\"",
        &["rc", "kill", "--slug", "aa1111", "x"],
        "",
    );
    err(
        2,
        "flag provided but not defined: -nope",
        &["rc", "probe", "--nope"],
        "",
    );
}
