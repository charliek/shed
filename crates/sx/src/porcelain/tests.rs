//! Porcelain unit tests (plan 009 C7 §D).
//!
//! Everything here is hermetic: the dispatch-table decisions are asserted on the
//! PURE planner, the remote argv on the pure builders, and the one end-to-end
//! machine create against a recording [`RcRunner`] that never spawns ssh. No
//! network, no config file, no tmux.

use std::cell::RefCell;
use std::io::{self, Write};
use std::rc::Rc;
use std::sync::{Arc, Mutex};
use std::time::Duration;

use async_trait::async_trait;
use shed_app::rc_engine::fake::FakeTmux;
use shed_app::RcTarget;
use shed_app::{RcRunner, RunOutput};
use shed_core::config::{MachineEntry, ShedConfig};
use shed_core::rc::{
    RcActivity, RcCapabilities, RcKind, RcKindFeatures, RcSessionDto, RcSessionListDto, RcState,
};
use shed_core::rc_events::RcEvent;

use super::kickoff::{kind_for_tool, plan_kickoff, Kickoff, Payload, Request};
use super::ls::{self, Listing, Row};
use super::watch::{select_transport, Transport};
use super::*;
use crate::target::Target;

// ---------------------------------------------------------------------------
// harness
// ---------------------------------------------------------------------------

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

/// A recording remote seam: captures every `(argv, stdin)` and answers with a
/// canned stdout/exit. The ssh boundary, without ssh.
#[derive(Default)]
struct RecordingRunner {
    calls: Mutex<Vec<(Vec<String>, Option<String>)>>,
    stdout: String,
    exit_code: i32,
}

impl RecordingRunner {
    fn ok(stdout: &str) -> Arc<Self> {
        Arc::new(Self {
            stdout: stdout.to_string(),
            ..Default::default()
        })
    }
    fn failing(exit_code: i32) -> Arc<Self> {
        Arc::new(Self {
            exit_code,
            ..Default::default()
        })
    }
    fn only_call(&self) -> (Vec<String>, Option<String>) {
        let calls = self.calls.lock().unwrap();
        assert_eq!(calls.len(), 1, "expected exactly one remote call");
        calls[0].clone()
    }
}

#[async_trait]
impl RcRunner for RecordingRunner {
    async fn run(
        &self,
        argv: Vec<String>,
        stdin: Option<String>,
        _timeout: Duration,
    ) -> io::Result<RunOutput> {
        self.calls.lock().unwrap().push((argv, stdin));
        Ok(RunOutput {
            stdout: self.stdout.clone(),
            stderr: String::new(),
            exit_code: self.exit_code,
        })
    }
}

struct Harness {
    stdout: Sink,
    stderr: Sink,
}

/// Build an inert `Deps` over a fake tmux + a recording remote seam.
fn deps_with<'a>(
    tmux: &'a FakeTmux,
    remote: Option<RcRunnerRef>,
    env: &[(&str, &str)],
) -> (Deps<'a>, Harness) {
    let stdout = Sink::default();
    let stderr = Sink::default();
    let table: std::collections::HashMap<String, String> = env
        .iter()
        .map(|(k, v)| ((*k).to_string(), (*v).to_string()))
        .collect();
    let deps = Deps {
        runner: tmux,
        env: Box::new(move |key| table.get(key).cloned().unwrap_or_default()),
        stdin: RefCell::new(Box::new(io::empty())),
        stdout: RefCell::new(Box::new(stdout.clone())),
        stderr: RefCell::new(Box::new(stderr.clone())),
        bin_probe: None,
        ensure_hub: None,
        sleep: Some(Box::new(|_| {})),
        settle: Some(Duration::ZERO),
        probe: None,
        preseed: None,
        remote,
        hostname: Some(Box::new(|| "testhost".to_string())),
        runtime: std::cell::OnceCell::new(),
    };
    (
        deps,
        Harness {
            stdout: stdout.clone(),
            stderr: stderr.clone(),
        },
    )
}

fn argv(items: &[&str]) -> Vec<String> {
    items.iter().map(|s| (*s).to_string()).collect()
}

fn machine(name: &str) -> MachineEntry {
    MachineEntry {
        name: name.into(),
        host: format!("{name}.local"),
        user: Some("dev".into()),
        ssh_port: 22,
        rc_bin: None,
        known_hosts: None,
    }
}

/// A create DTO the recording runner can answer a remote `create` with.
const CREATED_DTO: &str = r#"{"slug":"abc234","tmux_session":"rc-abc234","kind":"claude-rc","state":"ready","managed":true,"url":"https://claude.ai/code/session_01","display_name":"testhost/abc234"}"#;

// ---------------------------------------------------------------------------
// tool -> kind
// ---------------------------------------------------------------------------

#[test]
fn tool_to_kind_mapping_table() {
    let cases: &[(&str, RcKind)] = &[
        // The tool is spelled `claude`; the KIND is `claude-rc`.
        ("claude", RcKind::ClaudeRc),
        ("claude-rc", RcKind::ClaudeRc),
        ("codex", RcKind::Codex),
        ("cursor", RcKind::Cursor),
        ("opencode", RcKind::Opencode),
        ("shell", RcKind::Shell),
    ];
    for (tool, want) in cases {
        assert_eq!(&kind_for_tool(tool).unwrap(), want, "tool {tool}");
    }
    for bad in ["claude-broker", "aider", "", "CLAUDE"] {
        let err = kind_for_tool(bad).unwrap_err();
        assert_eq!(err.code, 2, "tool {bad:?}");
        assert!(err.message.contains("expected claude"), "{}", err.message);
    }
}

// ---------------------------------------------------------------------------
// the dispatch table (plan 009 §3.2)
// ---------------------------------------------------------------------------

fn request<'a>(kind: RcKind, target: &'a Target) -> Request<'a> {
    Request {
        kind,
        target,
        permission_mode: "",
        skip: false,
        default_permission_mode: "",
        name: "",
        slug: "fixed1",
        workdir: "",
        no_wait: false,
        prompt: "",
        plan: None,
        hostname: "mac-mini",
        created_by: "sx",
    }
}

#[test]
fn posture_matrix_interactive_shell_and_wait_per_target() {
    let cases: &[(&str, bool)] = &[
        ("local", true),
        ("machine:mini2", true),
        // The guest contract: the SSH `bash -lc` wrap already supplies PATH.
        ("shed:web", false),
        ("shed:web@mini3", false),
    ];
    for (raw, interactive) in cases {
        let target = Target::parse(raw).unwrap();
        let k = plan_kickoff(&request(RcKind::ClaudeRc, &target)).unwrap();
        assert_eq!(k.interactive_shell, *interactive, "target {raw}");
        // Wait is ON everywhere by default (the absorbed `claude` verb's posture).
        assert!(k.wait, "target {raw}");
        assert_eq!(k.target_label, target.label(), "target {raw}");
    }
}

#[test]
fn no_wait_turns_the_wait_flag_off_and_only_that() {
    let target = Target::parse("machine:mini2").unwrap();
    let k = plan_kickoff(&Request {
        no_wait: true,
        ..request(RcKind::Codex, &target)
    })
    .unwrap();
    assert!(!k.wait);
    assert!(k.interactive_shell);
}

#[test]
fn no_wait_is_rejected_with_a_kickoff() {
    let target = Target::Local;
    for req in [
        Request {
            no_wait: true,
            prompt: "do the thing",
            ..request(RcKind::ClaudeRc, &target)
        },
        Request {
            no_wait: true,
            plan: Some("# plan\n".to_string()),
            ..request(RcKind::ClaudeRc, &target)
        },
    ] {
        let err = plan_kickoff(&req).unwrap_err();
        assert_eq!(err.code, 2);
        assert!(
            err.message.contains("--no-wait cannot be combined"),
            "{}",
            err.message
        );
    }
}

#[test]
fn display_name_defaults_per_target() {
    // local/machine: `<shorthost>/<slug>` (the Go `claude` verb's default).
    for raw in ["local", "machine:mini2"] {
        let target = Target::parse(raw).unwrap();
        let k = plan_kickoff(&request(RcKind::ClaudeRc, &target)).unwrap();
        assert_eq!(k.display_name, "mac-mini/fixed1", "target {raw}");
    }
    // shed: the bare slug — the server already renders `<shed>/<slug>`.
    let target = Target::parse("shed:web").unwrap();
    let k = plan_kickoff(&request(RcKind::ClaudeRc, &target)).unwrap();
    assert_eq!(k.display_name, "fixed1");

    // No hostname available → the bare slug rather than a leading slash.
    let target = Target::Local;
    let k = plan_kickoff(&Request {
        hostname: "",
        ..request(RcKind::ClaudeRc, &target)
    })
    .unwrap();
    assert_eq!(k.display_name, "fixed1");

    // An explicit --name always wins.
    let k = plan_kickoff(&Request {
        name: "nightly",
        ..request(RcKind::ClaudeRc, &target)
    })
    .unwrap();
    assert_eq!(k.display_name, "nightly");
}

#[test]
fn permission_posture_defaults_and_exclusion() {
    let target = Target::Local;
    // The `sx agent` rule: claude keeps `auto`, every other tool keeps its own.
    let claude = plan_kickoff(&Request {
        default_permission_mode: "auto",
        ..request(RcKind::ClaudeRc, &target)
    })
    .unwrap();
    assert_eq!(claude.permission_mode, "auto");
    let codex = plan_kickoff(&request(RcKind::Codex, &target)).unwrap();
    assert_eq!(codex.permission_mode, "");

    // --skip expands to the generic full-bypass posture.
    let skipped = plan_kickoff(&Request {
        skip: true,
        ..request(RcKind::Codex, &target)
    })
    .unwrap();
    assert_eq!(skipped.permission_mode, "skip");

    // …and cannot be combined with an explicit mode.
    let err = plan_kickoff(&Request {
        skip: true,
        permission_mode: "auto",
        ..request(RcKind::Codex, &target)
    })
    .unwrap_err();
    assert_eq!(err.code, 2);
    assert!(err.message.contains("mutually exclusive"));

    // An invalid mode is rejected BEFORE anything else happens.
    let err = plan_kickoff(&Request {
        permission_mode: "nonsense",
        ..request(RcKind::ClaudeRc, &target)
    })
    .unwrap_err();
    assert_eq!(err.code, 2);
}

#[test]
fn a_caller_slug_is_validated_and_an_absent_one_is_generated() {
    let target = Target::Local;
    let err = plan_kickoff(&Request {
        slug: "NOT-A-SLUG",
        ..request(RcKind::Shell, &target)
    })
    .unwrap_err();
    assert_eq!(err.code, 2);
    assert!(err.message.contains("invalid slug"));

    let k = plan_kickoff(&Request {
        slug: "",
        ..request(RcKind::Shell, &target)
    })
    .unwrap();
    assert_eq!(k.slug.chars().count(), 6);
    assert!(k
        .slug
        .chars()
        .all(|c| "abcdefghjkmnpqrstuvwxyz23456789".contains(c)));
}

#[test]
fn a_prompt_becomes_a_kickoff_and_framing_when_a_plan_is_present() {
    let target = Target::Local;
    let prompt_only = plan_kickoff(&Request {
        prompt: "go",
        ..request(RcKind::ClaudeRc, &target)
    })
    .unwrap();
    assert_eq!(prompt_only.payload, Payload::Prompt("go".to_string()));

    let with_plan = plan_kickoff(&Request {
        prompt: "framing",
        plan: Some("# plan\n".to_string()),
        ..request(RcKind::ClaudeRc, &target)
    })
    .unwrap();
    assert_eq!(
        with_plan.payload,
        Payload::Plan {
            text: "# plan\n".to_string(),
            // base64("framing")
            framing_b64: Some("ZnJhbWluZw==".to_string()),
        }
    );
}

// ---------------------------------------------------------------------------
// remote argv shapes
// ---------------------------------------------------------------------------

fn kickoff_for(raw_target: &str, kind: RcKind) -> Kickoff {
    let target = Target::parse(raw_target).unwrap();
    plan_kickoff(&Request {
        default_permission_mode: "auto",
        workdir: "/work",
        ..request(kind, &target)
    })
    .unwrap()
}

#[test]
fn machine_create_argv_carries_the_machine_posture() {
    let k = kickoff_for("machine:mini2", RcKind::ClaudeRc);
    let (remote_argv, stdin) = k.remote_invocation("/opt/bin/shed-machine-rc").unwrap();
    assert_eq!(stdin, None);
    assert_eq!(remote_argv[0], "/opt/bin/shed-machine-rc");
    assert_eq!(remote_argv[1], "create");
    assert!(remote_argv.contains(&"--wait".to_string()));
    assert!(remote_argv.contains(&"--interactive-shell".to_string()));
    assert!(remote_argv.windows(2).any(|w| w == ["--kind", "claude-rc"]));
    assert!(remote_argv.windows(2).any(|w| w == ["--workdir", "/work"]));
    assert!(remote_argv
        .windows(2)
        .any(|w| w == ["--permission-mode", "auto"]));
    assert!(remote_argv
        .windows(2)
        .any(|w| w == ["--target", "machine:mini2"]));

    // …and the ssh wrapper carries it as one quoted remote line.
    let ssh = crate::ssh::machine_argv(&machine("mini2"), &remote_argv);
    assert_eq!(ssh[0], "ssh");
    assert!(ssh.last().unwrap().contains("'create'"));
    assert!(ssh.last().unwrap().contains("'--interactive-shell'"));
}

#[test]
fn shed_create_argv_leaves_interactive_shell_off() {
    let k = kickoff_for("shed:web@mini3", RcKind::ClaudeRc);
    let (remote_argv, _) = k.remote_invocation(crate::target::SHED_RC_BIN).unwrap();
    assert_eq!(remote_argv[0], "shed-ext-rc");
    assert!(remote_argv.contains(&"--wait".to_string()));
    assert!(
        !remote_argv.contains(&"--interactive-shell".to_string()),
        "the guest contract forbids -ic: {remote_argv:?}"
    );
    assert!(remote_argv
        .windows(2)
        .any(|w| w == ["--target", "shed:web@mini3"]));
    // The shed's display name is the bare slug.
    let at = remote_argv.iter().position(|a| a == "--name").unwrap();
    assert_eq!(remote_argv[at + 1], k.slug);
}

#[test]
fn a_plan_kickoff_ships_plan_stdin_with_b64_framing() {
    let target = Target::parse("shed:web@mini3").unwrap();
    let k = plan_kickoff(&Request {
        default_permission_mode: "auto",
        prompt: "framing",
        plan: Some("# do it\n".to_string()),
        ..request(RcKind::ClaudeRc, &target)
    })
    .unwrap();
    let (remote_argv, stdin) = k.remote_invocation("shed-ext-rc").unwrap();
    assert_eq!(stdin.as_deref(), Some("# do it\n"));
    let at = remote_argv
        .iter()
        .position(|a| a == "--plan-stdin")
        .unwrap();
    assert_eq!(remote_argv[at + 1..], ["--prompt-b64", "ZnJhbWluZw=="]);
    assert!(!remote_argv.contains(&"--prompt-stdin".to_string()));
}

#[test]
fn the_local_path_carries_the_same_decisions_into_engine_options() {
    let k = kickoff_for("local", RcKind::ClaudeRc);
    let opts = k.create_options();
    assert!(opts.interactive_shell);
    assert!(opts.wait);
    assert_eq!(opts.permission_mode, "auto");
    assert_eq!(opts.workdir, "/work");
    assert_eq!(opts.display_name, "mac-mini/fixed1");
    // A local target stamps no target label.
    assert_eq!(opts.target, "");

    // The plan framing reaches the engine as PLAIN text (the base64 exists only
    // for the argv hop).
    let target = Target::Local;
    let k = plan_kickoff(&Request {
        prompt: "framing",
        plan: Some("# p\n".to_string()),
        ..request(RcKind::ClaudeRc, &target)
    })
    .unwrap();
    let opts = k.create_options();
    assert_eq!(opts.plan, "# p\n");
    assert_eq!(opts.plan_framing, "framing");
    assert_eq!(opts.prompt, "");
}

// ---------------------------------------------------------------------------
// end-to-end dispatch against the recording remote seam
// ---------------------------------------------------------------------------

/// A config naming one machine, written where `SHED_CONFIG` points and removed
/// when the test ends — including on a panic, which a manual `remove_file` at the
/// end of the body would skip.
struct ScratchConfig(String);

impl ScratchConfig {
    fn machine(name: &str) -> Self {
        // Unique per CALL, not per machine name: cargo runs these tests in
        // parallel threads of ONE process, and two tests sharing a path meant one
        // deleting the file the other was about to read (a real flake, caught in
        // review).
        static SEQ: std::sync::atomic::AtomicUsize = std::sync::atomic::AtomicUsize::new(0);
        let path = std::env::temp_dir().join(format!(
            "sx-porcelain-{name}-{}-{}.yaml",
            std::process::id(),
            SEQ.fetch_add(1, std::sync::atomic::Ordering::Relaxed)
        ));
        std::fs::write(
            &path,
            format!("machines:\n    {name}:\n        host: {name}.local\n"),
        )
        .expect("write the scratch config");
        Self(path.to_string_lossy().into_owned())
    }

    /// The `SHED_CONFIG` env table a `deps_with` call wants.
    fn env(&self) -> [(&str, &str); 1] {
        [("SHED_CONFIG", self.0.as_str())]
    }
}

impl Drop for ScratchConfig {
    fn drop(&mut self) {
        let _ = std::fs::remove_file(&self.0);
    }
}

#[test]
fn agent_on_a_machine_shells_out_with_the_planned_argv() {
    let config = ScratchConfig::machine("mini2");
    let tmux = FakeTmux::ok();
    let runner = RecordingRunner::ok(CREATED_DTO);
    let (deps, out) = deps_with(&tmux, Some(runner.clone()), &config.env());

    let code = dispatch(
        &deps,
        "agent",
        &argv(&["claude", "--on", "machine:mini2", "--slug", "abc234"]),
    )
    .expect("agent is a porcelain verb");
    assert_eq!(code, 0, "stderr: {}", out.stderr.text());

    let (called, stdin) = runner.only_call();
    assert_eq!(called[0], "ssh");
    assert!(
        called.contains(&"dev@mini2.local".to_string())
            || called.contains(&"mini2.local".to_string())
    );
    let remote = called.last().unwrap();
    assert!(remote.contains("'create'"), "{remote}");
    assert!(remote.contains("'--interactive-shell'"), "{remote}");
    assert!(remote.contains("'--slug' 'abc234'"), "{remote}");
    assert!(remote.contains("'--permission-mode' 'auto'"), "{remote}");
    assert_eq!(stdin, None);

    // …and the human summary points at the right follow-up commands.
    let text = out.stdout.text();
    assert!(
        text.contains("sx watch abc234 --on machine:mini2"),
        "{text}"
    );
    assert!(
        text.contains("sx attach abc234 --on machine:mini2"),
        "{text}"
    );
    assert!(text.contains("https://claude.ai/code/session_01"), "{text}");
}

#[test]
fn agent_json_prints_the_dto_and_the_remote_exit_class_survives_the_ssh_hop() {
    let config = ScratchConfig::machine("mini3");
    let tmux = FakeTmux::ok();
    let (deps, out) = deps_with(&tmux, Some(RecordingRunner::ok(CREATED_DTO)), &config.env());
    let code = dispatch(
        &deps,
        "agent",
        &argv(&["claude", "--on", "machine:mini3", "--json"]),
    )
    .unwrap();
    assert_eq!(code, 0);
    let dto: serde_json::Value = serde_json::from_str(out.stdout.text().trim_end()).unwrap();
    assert_eq!(dto["slug"], "abc234");

    // A duplicate slug (exit 3) stays exit 3 on this side of the hop, while an
    // ssh-level failure (255) collapses to the generic 1 rather than being read
    // as an engine class.
    for (remote_exit, want) in [(3, 3), (255, 1)] {
        let (deps, _) = deps_with(
            &tmux,
            Some(RecordingRunner::failing(remote_exit)),
            &config.env(),
        );
        assert_eq!(
            dispatch(&deps, "agent", &argv(&["claude", "--on", "machine:mini3"])).unwrap(),
            want,
            "remote exit {remote_exit}"
        );
    }
}

#[test]
fn kill_on_a_machine_sends_the_kill_argv() {
    let config = ScratchConfig::machine("mini2");
    let tmux = FakeTmux::ok();
    let runner = RecordingRunner::ok("");
    let (deps, out) = deps_with(&tmux, Some(runner.clone()), &config.env());
    let code = dispatch(&deps, "kill", &argv(&["abc234", "--on", "machine:mini2"])).unwrap();
    assert_eq!(code, 0);
    let (called, _) = runner.only_call();
    assert!(called
        .last()
        .unwrap()
        .contains("'shed-machine-rc' 'kill' '--slug' 'abc234'"));
    assert!(out.stdout.text().contains("Killed abc234 on machine:mini2"));
}

#[test]
fn an_unknown_machine_is_a_usage_error_naming_the_configured_ones() {
    let config = ScratchConfig::machine("mini2");
    let tmux = FakeTmux::ok();
    let (deps, out) = deps_with(&tmux, None, &config.env());
    let code = dispatch(&deps, "kill", &argv(&["abc234", "--on", "machine:ghost"])).unwrap();
    assert_eq!(code, 2);
    assert!(out.stderr.text().contains("mini2"), "{}", out.stderr.text());
}

#[test]
fn a_subject_is_required_and_must_come_first() {
    let tmux = FakeTmux::ok();
    let (deps, out) = deps_with(&tmux, None, &[]);
    // No subject at all.
    assert_eq!(dispatch(&deps, "watch", &[]).unwrap(), 2);
    // A flag where the subject belongs.
    assert_eq!(
        dispatch(&deps, "watch", &argv(&["--on", "local"])).unwrap(),
        2
    );
    assert!(out.stderr.text().contains("a subject is required"));

    // A STRAY positional after the flags is a usage error too, never dropped.
    let (deps, out) = deps_with(&tmux, None, &[]);
    assert_eq!(
        dispatch(&deps, "agent", &argv(&["claude", "--on", "local", "oops"])).unwrap(),
        2
    );
    assert!(out.stderr.text().contains("unexpected argument"));
}

#[test]
fn the_porcelain_never_shadows_the_rc_namespace() {
    let tmux = FakeTmux::ok();
    let (deps, _) = deps_with(&tmux, None, &[]);
    for cmd in ["rc", "version", "help", "bogus"] {
        assert!(
            dispatch(&deps, cmd, &[]).is_none(),
            "{cmd} must fall through to the engine-compat dispatch"
        );
    }
}

// ---------------------------------------------------------------------------
// attach
// ---------------------------------------------------------------------------

#[test]
fn attach_argv_per_target() {
    let tmux = FakeTmux::ok();
    let (deps, _) = deps_with(&tmux, None, &[]);

    // Local: straight to tmux — no ssh in the way.
    assert_eq!(
        attach::attach_argv(&deps, &Resolved::Local, "abc234").unwrap(),
        argv(&["tmux", "attach", "-t", "rc-abc234"])
    );

    // Machine: a TTY ssh carrying the same tmux command as ONE quoted line.
    let on_machine =
        attach::attach_argv(&deps, &Resolved::Machine(machine("mini2")), "abc234").unwrap();
    assert_eq!(&on_machine[..2], ["ssh", "-t"]);
    assert_eq!(
        &on_machine[on_machine.len() - 2..],
        ["--", "'tmux' 'attach' '-t' 'rc-abc234'"]
    );
}

/// The subject is interpolated into a tmux target and (remotely) into a shell
/// command line, so it is validated BEFORE any argv exists — on every target,
/// including the shed one, where the check must also precede the endpoint lookup.
#[test]
fn attach_rejects_a_subject_that_is_not_a_slug() {
    let tmux = FakeTmux::ok();
    let (deps, _) = deps_with(&tmux, None, &[]);
    let targets = [
        Resolved::Local,
        Resolved::Machine(machine("mini2")),
        Resolved::Shed {
            name: "web".into(),
            server: Some("mini3".into()),
        },
    ];
    for resolved in &targets {
        for bad in ["x; touch /tmp/pwn", "abc 234", "-abc", "ABC234", ""] {
            let err = attach::attach_argv(&deps, resolved, bad).unwrap_err();
            assert_eq!(err.code, 2, "target {resolved:?} slug {bad:?}");
            assert!(
                err.message.contains("invalid slug"),
                "target {resolved:?} slug {bad:?}: {}",
                err.message
            );
        }
    }
    // …and a well-formed slug still builds an argv (the two targets that need no
    // endpoint lookup; the shed one is covered by `attach_argv_per_target`).
    for resolved in &targets[..2] {
        assert!(attach::attach_argv(&deps, resolved, "abc234").is_ok());
    }

    // The whole verb reports it as a usage error, not a failed exec.
    let (deps, out) = deps_with(&tmux, None, &[]);
    assert_eq!(
        dispatch(&deps, "attach", &argv(&["x; touch /tmp/pwn", "--print"])).unwrap(),
        2
    );
    assert!(
        out.stderr.text().contains("invalid slug"),
        "{}",
        out.stderr.text()
    );
}

#[test]
fn attach_print_emits_the_command_instead_of_execing() {
    let tmux = FakeTmux::ok();
    let (deps, out) = deps_with(&tmux, None, &[]);
    let code = dispatch(&deps, "attach", &argv(&["abc234", "--print"])).unwrap();
    assert_eq!(code, 0);
    assert_eq!(
        out.stdout.text().trim_end(),
        "'tmux' 'attach' '-t' 'rc-abc234'"
    );
}

// ---------------------------------------------------------------------------
// ls rendering (capability-aware)
// ---------------------------------------------------------------------------

fn session(slug: &str, kind: RcKind, state: RcState) -> RcSessionDto {
    RcSessionDto {
        slug: slug.to_string(),
        tmux_session: format!("rc-{slug}"),
        kind,
        state,
        managed: true,
        lane: Some("tui".to_string()),
        display_name: Some(format!("host/{slug}")),
        workdir: None,
        url: None,
        id: None,
        created_by: Some("sx".to_string()),
        created_at: None,
        target_label: None,
        activity: None,
        activity_at: None,
        last_message: None,
        pending_approvals: None,
    }
}

fn caps(rows: &[(&str, bool)], features: &[&str]) -> RcCapabilities {
    RcCapabilities {
        rc_version: 4,
        kinds: vec![RcKind::ClaudeRc, RcKind::Codex, RcKind::Shell],
        agents: Default::default(),
        features: features.iter().map(|f| (*f).to_string()).collect(),
        kind_features: rows
            .iter()
            .map(|(kind, feed)| {
                (
                    (*kind).to_string(),
                    RcKindFeatures {
                        post_input: true,
                        approvals: "tui".to_string(),
                        watch: *feed,
                        input: String::new(),
                        feed: if *feed { "messages" } else { "activity" }.to_string(),
                        interrupt: false,
                        attach: "tmux".to_string(),
                    },
                )
            })
            .collect(),
    }
}

#[test]
fn watch_cell_follows_the_capability_rules() {
    let block = caps(&[("codex", true), ("claude-rc", false)], &["contract-v2"]);
    // A kind with a message feed, a kind without one, and a kind with NO row.
    assert_eq!(ls::watch_cell("codex", Some(&block)), "feed");
    assert_eq!(ls::watch_cell("claude-rc", Some(&block)), "activity");
    assert_eq!(ls::watch_cell("shell", Some(&block)), "-");
    // No capability block at all → unknown, and (below) a note.
    assert_eq!(ls::watch_cell("codex", None), "?");
}

#[test]
fn a_missing_capability_block_becomes_a_note_not_a_silent_blank() {
    let mut listing = Listing::default();
    listing.add(
        "machine:old",
        &RcSessionListDto {
            rc_sessions: vec![session("aa1111", RcKind::Codex, RcState::Ready)],
            capabilities: None,
        },
    );
    assert_eq!(listing.rows[0].watch, "?");
    assert_eq!(listing.notes.len(), 1);
    assert!(listing.notes[0].contains("no capability block"));

    // A pre-contract-v2 block gets its own note (the fallback rules apply, but
    // `sx watch` will not find a feed).
    let mut listing = Listing::default();
    listing.add(
        "local",
        &RcSessionListDto {
            rc_sessions: vec![],
            capabilities: Some(caps(&[("codex", true)], &["messages"])),
        },
    );
    assert!(listing.notes[0].contains("pre-contract-v2"));
}

#[test]
fn ls_render_shows_activity_beside_state_and_annotates_errors() {
    let mut listing = Listing::default();
    let mut working = session("aa1111", RcKind::Codex, RcState::Ready);
    working.activity = Some(RcActivity::Working);
    listing.add(
        "local",
        &RcSessionListDto {
            rc_sessions: vec![working, session("bb2222", RcKind::Shell, RcState::Dead)],
            capabilities: Some(caps(&[("codex", true)], &["contract-v2"])),
        },
    );
    listing.add_error("machine:asleep", "ssh failed: no route to host");

    let text = ls::render(&listing);
    assert!(text.starts_with("NAME"), "{text}");
    assert!(text.contains("ready (working)"), "{text}");
    assert!(text.contains("dead"), "{text}");
    // The kind with a feed vs the kind with no capability row.
    assert!(text.contains("feed"), "{text}");
    assert!(
        text.contains("error: machine:asleep: ssh failed: no route to host"),
        "{text}"
    );
    // No rows at all still renders something legible.
    assert_eq!(ls::render(&Listing::default()), "no rc sessions\n");
}

#[test]
fn ls_rows_fall_back_to_the_slug_when_a_session_has_no_display_name() {
    let mut listing = Listing::default();
    let mut anonymous = session("cc3333", RcKind::Shell, RcState::Ready);
    anonymous.display_name = None;
    listing.add(
        "local",
        &RcSessionListDto {
            rc_sessions: vec![anonymous],
            capabilities: Some(caps(&[], &["contract-v2"])),
        },
    );
    assert_eq!(
        listing.rows[0],
        Row {
            name: "cc3333".to_string(),
            slug: "cc3333".to_string(),
            target: "local".to_string(),
            kind: "shell".to_string(),
            state: "ready".to_string(),
            watch: "-".to_string(),
            created_by: "sx".to_string(),
        }
    );
}

// ---------------------------------------------------------------------------
// watch: transport selection + line rendering
// ---------------------------------------------------------------------------

#[test]
fn transport_selection_follows_the_contract_v2_client_rules() {
    let block = caps(
        &[("codex", true), ("claude-rc", false)],
        &["contract-v2", "messages"],
    );
    // A kind with a message feed streams.
    assert_eq!(select_transport("codex", Some(&block)), Transport::Feed);
    // A kind without one, a kind with no row, and no block at all all degrade —
    // each with a REASON, so the fallback is never silent.
    for (kind, caps, needle) in [
        ("claude-rc", Some(&block), "no message feed"),
        ("shell", Some(&block), "no feed/steer affordances"),
        ("codex", None, "predates capability discovery"),
    ] {
        match select_transport(kind, caps) {
            Transport::ProbePolling(reason) => assert!(reason.contains(needle), "{reason}"),
            other => panic!("kind {kind} selected {other:?}"),
        }
    }
    // A binary that advertises no `messages` feature at all degrades too.
    let old = caps(&[("codex", true)], &["contract-v2"]);
    assert!(matches!(
        select_transport("codex", Some(&old)),
        Transport::ProbePolling(_)
    ));
}

#[test]
fn watch_renders_only_this_slugs_events() {
    let mine = RcEvent::ActivityChanged {
        shed: "web".into(),
        slug: "abc234".into(),
        activity: Some(RcActivity::Working),
        activity_at: None,
        state: Some(RcState::Ready),
        last_message: Some("running tests".into()),
    };
    let line = &watch::render_event(&mine, "abc234")[0];
    assert!(line.contains("state ready"), "{line}");
    assert!(line.contains("activity working"), "{line}");
    assert!(line.contains("running tests"), "{line}");

    // Another session's event renders nothing.
    assert!(watch::render_event(&mine, "zz9999").is_empty());

    // A removal is announced; the notification-only event prints nothing.
    let gone = RcEvent::SessionUpdated {
        shed: "web".into(),
        slug: "abc234".into(),
        activity: None,
        state: None,
        last_message: None,
        lane: None,
        removed: true,
    };
    assert_eq!(watch::render_event(&gone, "abc234"), ["abc234: gone"]);
    let appended = RcEvent::MessageAppended {
        shed: "web".into(),
        slug: "abc234".into(),
        seq: 7,
    };
    assert!(watch::render_event(&appended, "abc234").is_empty());

    // The shed-scoped synthetic events are ALWAYS shown — they explain why the
    // feed went quiet, and suppressing them would look like a healthy idle.
    let stale = RcEvent::HubUnavailable { shed: "web".into() };
    assert!(watch::render_event(&stale, "abc234")[0].contains("hub unavailable"));
}

/// **The filter, not just the renderer.** `render_event` always printed the
/// shed-scoped degrade signals — but the shed transport's `keep` predicate
/// dropped them before they ever reached it (their slug is `""` by
/// construction), so the ONLY transport that can produce them rendered none of
/// them and a hub drop read as a healthy idle stream. This drives a real event
/// through the real predicate and the real consume loop.
#[test]
fn the_shed_filter_keeps_the_degrade_signals_and_fetches_bodies() {
    let tmux = FakeTmux::ok();
    let (deps, out) = deps_with(&tmux, None, &[]);
    let (tx, mut rx) = tokio::sync::mpsc::unbounded_channel();
    for event in [
        // This shed's degrade signals — slug-less, and must survive the filter.
        RcEvent::HubUnavailable { shed: "web".into() },
        RcEvent::ShedStopped { shed: "web".into() },
        // Another shed's, which must NOT (the stream is host-aggregate).
        RcEvent::HubUnavailable {
            shed: "other".into(),
        },
        // Another slug's activity on this shed, also filtered out.
        RcEvent::ActivityChanged {
            shed: "web".into(),
            slug: "zz9999".into(),
            activity: Some(RcActivity::Working),
            activity_at: None,
            state: Some(RcState::Ready),
            last_message: Some("someone else".into()),
        },
        // …and this slug's message notification, whose BODY is fetched.
        RcEvent::MessageAppended {
            shed: "web".into(),
            slug: "abc234".into(),
            seq: 4,
        },
    ] {
        tx.send(event).unwrap();
    }
    drop(tx);

    let page = shed_core::rc::RcMessagesPage::from_value(&serde_json::json!({
        "messages": [{"seq": 4, "role": "assistant", "type": "assistant", "text": "done"}]
    }));
    let outcome = deps.block_on(watch::consume_events(
        &deps,
        &mut rx,
        "abc234",
        |event| watch::shed_event_keep(event, "web", "abc234"),
        |since| {
            // `seq - 1`: the notification's own row must be in the fetched page.
            assert_eq!(since, 3);
            let page = page.clone();
            async move { Ok(page) }
        },
    ));
    assert!(outcome.is_ok());

    let text = out.stdout.text();
    assert!(text.contains("hub unavailable for web"), "{text}");
    assert!(text.contains("shed web stopped"), "{text}");
    assert!(text.contains("[4] assistant: done"), "{text}");
    assert!(
        !text.contains("other"),
        "another shed leaked through: {text}"
    );
    assert!(
        !text.contains("someone else"),
        "another slug leaked: {text}"
    );
}

/// A hub that stops answering the body fetch (the proxy's `RC_HUB_UNAVAILABLE`
/// 503) ends the consume loop with its reason, so the caller degrades to probe
/// polling — ONE note, not a swallowed error per event.
#[test]
fn a_failed_message_fetch_ends_the_consume_loop_once() {
    let tmux = FakeTmux::ok();
    let (deps, out) = deps_with(&tmux, None, &[]);
    let (tx, mut rx) = tokio::sync::mpsc::unbounded_channel();
    for seq in [4, 5] {
        tx.send(RcEvent::MessageAppended {
            shed: "web".into(),
            slug: "abc234".into(),
            seq,
        })
        .unwrap();
    }
    drop(tx);

    let calls = std::cell::Cell::new(0u32);
    let outcome = deps.block_on(watch::consume_events(
        &deps,
        &mut rx,
        "abc234",
        |event| watch::shed_event_keep(event, "web", "abc234"),
        |_| {
            calls.set(calls.get() + 1);
            async { Err("RC_HUB_UNAVAILABLE".to_string()) }
        },
    ));
    assert_eq!(outcome.unwrap_err(), "RC_HUB_UNAVAILABLE");
    assert_eq!(
        calls.get(),
        1,
        "a broken feed must not be retried per event"
    );
    assert!(out.stdout.text().is_empty(), "{}", out.stdout.text());
}

/// The other half of the multi-server bug: an unqualified `shed:<name>` is pinned
/// to the server it was FOUND on, and the aggregate stream is opened against that
/// server — not against `default_server`, whose frames never carry the shed.
#[test]
fn an_unqualified_shed_pins_the_server_it_was_found_on() {
    let target = |server: &str| RcTarget {
        server_name: server.to_string(),
        ssh_host: format!("{server}.local"),
        ssh_port: 2222,
        known_hosts: "/kh".into(),
    };
    // The shed lives on `mini3`, while the config's default server is `alpha`.
    let found = pick_shed_target("web", vec![target("mini3")]).unwrap();
    assert_eq!(found.server_name, "mini3");

    // What the rest of the porcelain then sees carries that server — the display,
    // and (below) the client the stream is built from.
    let resolved = Resolved::Shed {
        name: "web".into(),
        server: Some(found.server_name.clone()),
    };
    assert_eq!(resolved.display(), "shed:web@mini3");

    let config = ShedConfig::parse(
        "\
servers:
    alpha:
        host: alpha.local
    mini3:
        host: mini3.local
default_server: alpha
",
    );
    // Unpinned, the fallback picks the default — which is exactly the wrong host
    // for this shed, and is why the pin exists.
    assert_eq!(watch::select_server(&config, None).unwrap().name, "alpha");
    let entry = watch::select_server(&config, Some(&found.server_name)).unwrap();
    assert_eq!(entry.name, "mini3");
    assert_eq!(entry.host, "mini3.local");

    // The two failure shapes stay usage errors that say what to do.
    let err = pick_shed_target("web", Vec::new()).unwrap_err();
    assert_eq!(err.code, 2);
    assert!(err.message.contains("no running shed"), "{}", err.message);
    let err = pick_shed_target("web", vec![target("alpha"), target("mini3")]).unwrap_err();
    assert_eq!(err.code, 2);
    assert!(err.message.contains("alpha, mini3"), "{}", err.message);
}

/// Provenance: a shed session's `target_label` is `shed:<shed>@<server>` — the
/// shape `RcService::launch` stamps — never a bare `shed:<name>`.
#[test]
fn a_shed_kickoff_stamps_the_discovered_server_in_its_target_label() {
    let resolved = Resolved::Shed {
        name: "web".into(),
        server: Some("mini3".into()),
    };
    let target = resolved.target();
    let k = plan_kickoff(&request(RcKind::ClaudeRc, &target)).unwrap();
    assert_eq!(k.target_label, "shed:web@mini3");
    let (remote_argv, _) = k.remote_invocation("shed-ext-rc").unwrap();
    assert!(remote_argv
        .windows(2)
        .any(|w| w == ["--target", "shed:web@mini3"]));
}

#[test]
fn watch_renders_a_feed_message_line() {
    let raw = serde_json::json!({
        "messages": [{
            "seq": 3,
            "role": "tool",
            "type": "approval_request",
            "text": "run rm -rf build",
            "tool": {"name": "bash"},
            "approval": {"id": "call_01", "status": "pending"}
        }]
    });
    let page = shed_core::rc::RcMessagesPage::from_value(&raw);
    let line = watch::render_message(&page.messages[0]);
    assert!(line.starts_with("[3] tool/approval_request bash"), "{line}");
    assert!(line.contains("run rm -rf build"), "{line}");
    assert!(line.contains("[approval call_01 pending]"), "{line}");
}
