//! The hermetic rig, and the failure-injection table plan 019 §5 requires.
//!
//! **Nothing here is mocked that could be real.** Every `Step::Exec` runs
//! through a real `/bin/sh`, against a real temporary `$HOME` with
//! `jail_fs_root: true` so the ladder's absolute rungs cannot reach the
//! developer's own `/usr/bin`, executing roost's own scripts byte for byte. Every
//! `Step::Call` and `Step::Hooks` goes over a real [`Conn`] to a real
//! [`FakeRoost`]. The `roost-session` on the far side is a shell script that
//! answers `identify` and `start` the way the binary does, with a per-role queue
//! of answers a test seeds — which is how "the post-commit identify fails" is
//! produced by the *host* rather than by a stubbed outcome.
//!
//! That matters because the thing under test is a rollback promise about a
//! filesystem. Every failure row below asserts what is on disk afterwards — the
//! incumbent's bytes, the absence of a `.tmp.<pid>`, the absence of a
//! `.bak.<pid>` — and a stubbed exec would assert nothing at all.
//!
//! Three things are injected rather than provoked, because no environment
//! produces them on demand: a full disk mid-stream, a budget that expires, and a
//! runner that answers the wrong kind of outcome. Those arrive as a replacement
//! [`Outcome`] for one matching exec ([`Rig::inject`]) — and the *recovery* that
//! follows is still real, which is the half that carries the promise.

use std::borrow::Cow;
use std::collections::BTreeSet;
use std::io::Write as _;
use std::path::{Path, PathBuf};
use std::process::{Command, Stdio};

use serde_json::{json, Value};

use crate::roost::testing::{write_exec, FakeRoost, ScratchDir};
use crate::roost::Conn;

use super::*;

const TARGET: &str = "roost:popos/p019-a";
const LABEL: &str = "shed-desktop";

/// The identity a protocol-4 `roost-session` prints. `libghostty_build` is
/// deliberately something shed could never have guessed — the whole point of the
/// protocol-only gate is that shed does not know it and does not care.
fn identity_v4() -> String {
    json!({
        "app_version": "0.0.19",
        "session_protocol": 4,
        "libghostty_build": "ghostty-f2d5758f6305867d+snapshot.v1",
    })
    .to_string()
}

/// A protocol-2 `roost-session` — every released build today, which is why this
/// is the realistic "stale incumbent".
fn identity_v2() -> String {
    json!({
        "app_version": "0.0.19",
        "session_protocol": 2,
        "libghostty_build": "ghostty-older",
    })
    .to_string()
}

/// A `roost-session` stand-in: one `sh` script that answers `identify` and
/// `start` the way the real binary does.
///
/// `role` names its answer queue (`$HOME/.control/identify.<role>`), so an
/// incumbent and the binary that replaces it can be steered independently even
/// though both end up at the same path. A queue line is consumed per call: empty
/// means "the baked default", `FAIL` means "exit 2 saying nothing", anything else
/// is printed verbatim.
fn fake_session(role: &str, identity: &str) -> String {
    format!(
        r#"#!/bin/sh
# a fake roost-session ({role}) — plan 019 C4's hermetic rig
ctl="${{HOME}}/.control"
pop() {{
  q="$ctl/$1"
  line=""
  if [ -s "$q" ]; then
    line=$(head -n 1 "$q")
    tail -n +2 "$q" > "$q.rest" && mv "$q.rest" "$q"
  fi
  printf '%s' "$line"
}}
case "$1" in
  identify)
    line=$(pop "identify.{role}")
    [ -n "$line" ] || line='{identity}'
    if [ "$line" = FAIL ]; then exit 2; fi
    printf '%s\n' "$line"
    ;;
  start)
    line=$(pop "start.{role}")
    [ -n "$line" ] || line='ready pid=4242'
    printf '%s\n' "$line"
    case "$line" in
      error:*) exit 1 ;;
      # Both `ready` and `already-running` mean a session is serving there, and
      # the marker is what the rig reads to decide that the bridge would reach
      # one. The far side starting is what makes it reachable — not a flag this
      # side sets.
      *) : > "${{HOME}}/.session-running" ;;
    esac
    ;;
  client-bridge)
    printf '%s\n' 'client-bridge: no session' >&2
    exit 1
    ;;
  *)
    exit 2
    ;;
esac
"#
    )
}

/// A `roost-session` from before the `identify` subcommand ever existed: it
/// exits non-zero and says nothing.
///
/// `identity_script` records that as an **empty second field** and keeps walking
/// the ladder — which is precisely the case where "the first pair wins" is not
/// the same rule as "the best pair wins".
const FAKE_ANCIENT: &str = "#!/bin/sh\nexit 2\n";

/// A `uname` that lies, for the unsupported-OS row.
const FAKE_UNAME_DARWIN: &str = r#"#!/bin/sh
case "$1" in
  -s) printf '%s\n' Darwin ;;
  -m) printf '%s\n' arm64 ;;
  *) printf '%s\n' Darwin ;;
esac
"#;

// ============================================================================
// The rig
// ============================================================================

/// One recorded step, for the assertions about *what was asked* rather than
/// about what came back.
#[derive(Debug, Clone, PartialEq, Eq)]
struct Asked {
    kind: &'static str,
    /// A short name for the step, derived from what it actually runs.
    what: &'static str,
    capture_stdout: bool,
}

struct Rig {
    dir: ScratchDir,
    home: PathBuf,
    jail: PathBuf,
    bin: PathBuf,
    path: String,
    fake: FakeRoost,
    /// Replacement outcomes, each fired once on the first exec whose command
    /// contains the needle.
    injections: Vec<(String, Outcome)>,
    asked: Vec<Asked>,
    /// The lease `wire_agent_hooks` last handed back — the in-memory table the
    /// real clients keep per target (plan 019 C6), in miniature.
    lease: Option<String>,
    hooks_seed_lease: Option<String>,
}

impl Rig {
    async fn new() -> Rig {
        let dir = ScratchDir::with_prefix("shed-bootstrap");
        let home = dir.0.join("home");
        let jail = dir.0.join("jail");
        let bin = dir.0.join("shims");
        let utils = dir.0.join("utils");
        for path in [&home, &jail, &bin, &utils, &home.join(".control")] {
            std::fs::create_dir_all(path).expect("creating a rig directory");
        }
        // **`jail_fs_root` does not jail the `PATH` rung**, and it cannot: that
        // rung resolves through `command -v`, so what jails it is the `PATH` the
        // runner hands the child. A bare `/usr/bin:/bin` would let the
        // developer's own `roost-session` — protocol 2 on this machine, which is
        // exactly the interesting case — walk into a test about a cold host. So
        // the utilities the scripts need are symlinked into a directory of their
        // own and that is the whole `PATH`.
        //
        // `$HOME/.local/bin` is deliberately NOT on it: that is the ordinary
        // shed case (roost execs the absolute path) and it is what makes the
        // post-install PATH warning fire in the happy-path test.
        for tool in [
            "sh", "uname", "head", "tail", "mv", "mkdir", "rm", "tee", "chmod", "cat",
        ] {
            std::os::unix::fs::symlink(real_tool(tool), utils.join(tool))
                .expect("linking a utility");
        }
        let path = format!("{}:{}", bin.display(), utils.display());
        Rig {
            fake: FakeRoost::start().await,
            dir,
            home,
            jail,
            bin,
            path,
            injections: Vec::new(),
            asked: Vec::new(),
            lease: None,
            hooks_seed_lease: None,
        }
    }

    fn dest(&self) -> PathBuf {
        self.home.join(".local/bin/roost-session")
    }

    /// Put a `roost-session` at the install destination — an incumbent.
    fn seed_incumbent(&self, identity: &str) {
        let dest = self.dest();
        std::fs::create_dir_all(dest.parent().expect("a parent")).expect("mkdir .local/bin");
        write_exec(&dest, &fake_session("incumbent", identity));
    }

    /// A `SourceHandle` over a fake `roost-session` with this identity baked in.
    fn source(&self, identity: &str) -> SourceHandle {
        let path = self.dir.0.join("source-roost-session");
        write_exec(&path, &fake_session("new", identity));
        let file = std::fs::File::open(&path).expect("opening the source");
        let len = file.metadata().expect("the source's metadata").len();
        SourceHandle::from_open_file("a fake roost-session", file, len, None)
    }

    /// Seed an answer queue. One line is consumed per call; an empty line means
    /// the script's baked default.
    fn queue(&self, key: &str, lines: &[&str]) {
        let path = self.home.join(".control").join(key);
        std::fs::write(&path, format!("{}\n", lines.join("\n"))).expect("seeding a queue");
    }

    /// Replace the outcome of the next exec whose command **or script** contains
    /// `needle`.
    ///
    /// The script half is not a convenience: every step that runs a roost script
    /// has the same command, `/bin/sh -s`, because the script arrives on stdin.
    /// Naming one of them any other way would mean naming it by position.
    fn inject(&mut self, needle: &str, outcome: Outcome) {
        self.injections.push((needle.to_string(), outcome));
    }

    fn shim(&self, name: &str, body: &str) {
        write_exec(&self.bin.join(name), body);
    }

    /// Would the bridge reach a session, and if not, why not?
    ///
    /// Exactly the three-way answer a real transport gives: the exec chain
    /// reached a running session, fell through to `client-bridge: no session`, or
    /// fell off the end of the ladder at 127.
    fn reach(&self) -> Result<(), CallError> {
        if self.home.join(".session-running").exists() {
            return Ok(());
        }
        let ladder_hit = self.dest().is_file() || self.jail.join("usr/bin/roost-session").is_file();
        if ladder_hit {
            Err(CallError::new(
                reach_code::NO_SESSION,
                "client-bridge: no session",
            ))
        } else {
            Err(CallError::new(
                reach_code::NOT_INSTALLED,
                "roost-session: command not found",
            ))
        }
    }

    fn exec(&mut self, command: &str, stdin: Stdin, capture_stdout: bool) -> Outcome {
        self.asked.push(Asked {
            kind: "exec",
            what: label_exec(command, &stdin),
            capture_stdout,
        });
        let script = script_text(&stdin);
        if let Some(index) = self.injections.iter().position(|(needle, _)| {
            command.contains(needle.as_str()) || script.contains(needle.as_str())
        }) {
            return self.injections.remove(index).1;
        }

        let mut child = Command::new("/bin/sh")
            .arg("-c")
            .arg(command)
            .env_clear()
            .env("HOME", &self.home)
            .env("ROOST_BOOTSTRAP_FS_ROOT", &self.jail)
            .env("PATH", &self.path)
            .stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .stderr(Stdio::piped())
            .spawn()
            .expect("spawning /bin/sh");

        {
            let mut sink = child.stdin.take().expect("the child's stdin");
            match stdin {
                Stdin::Empty => {}
                Stdin::Bytes(bytes) => sink.write_all(&bytes).expect("writing a script"),
                Stdin::Source(source) => loop {
                    let chunk = source.read_chunk(8192).expect("reading the source");
                    if chunk.is_empty() {
                        break;
                    }
                    sink.write_all(&chunk).expect("streaming the source");
                },
            }
        }
        let output = child.wait_with_output().expect("waiting for /bin/sh");
        Outcome::Exec {
            exit: output.status.code(),
            // Honour the flag the way a real runner must: a step that asked for
            // no capture gets none, however much the far side echoed.
            stdout: if capture_stdout {
                output.stdout
            } else {
                Vec::new()
            },
            stderr_tail: String::from_utf8_lossy(&output.stderr).into_owned(),
        }
    }

    async fn call(&mut self, op: &str, params: Value) -> Outcome {
        self.asked.push(Asked {
            kind: "call",
            what: "session.identify",
            capture_stdout: false,
        });
        assert_eq!(op, "session.identify", "the machines make one op");
        if let Err(error) = self.reach() {
            return Outcome::Call(Err(error));
        }
        let mut conn = Conn::unix(self.fake.socket_path())
            .await
            .expect("dialling the fake");
        // **Raw, ungated** — see `Conn::call_raw`. A protocol-2 session has to
        // arrive as an answer, not as a refusal, or pin P6's report row could
        // never be produced.
        Outcome::Call(match conn.call_raw(op, params).await {
            Ok(value) => Ok(value),
            Err(crate::roost::RoostError::Server { code, message }) => {
                Err(CallError::new(code, message))
            }
            Err(other) => Err(CallError::new("transport", other.to_string())),
        })
    }

    async fn hooks(&mut self, client_label: &str) -> Outcome {
        self.asked.push(Asked {
            kind: "hooks",
            what: "session.set_agent_hooks",
            capture_stdout: false,
        });
        let mut conn = Conn::unix(self.fake.socket_path())
            .await
            .expect("dialling the fake");
        let cached = self.hooks_seed_lease.clone().or_else(|| self.lease.clone());
        let result = wire_agent_hooks(&mut conn, client_label, cached.as_deref()).await;
        self.lease = result.lease.clone();
        Outcome::Hooks(result)
    }

    /// Every `.tmp.<pid>` / `.bak.<pid>` beside the destination. The rollback
    /// promise is mostly a statement about this list being empty.
    fn residue(&self) -> Vec<String> {
        let dir = self.home.join(".local/bin");
        let Ok(entries) = std::fs::read_dir(&dir) else {
            return Vec::new();
        };
        let mut names: Vec<String> = entries
            .filter_map(Result::ok)
            .map(|entry| entry.file_name().to_string_lossy().into_owned())
            .filter(|name| name.contains(".tmp.") || name.contains(".bak."))
            .collect();
        names.sort();
        names
    }

    /// The one piece of residue whose name contains `needle` — `.bak.` for a
    /// stranded incumbent, `.tmp.` for a staged file the cleanup could not
    /// remove. Each caller still says, in its own `expect`, what it is claiming
    /// about that file.
    fn residue_named(&self, needle: &str) -> Option<String> {
        self.residue()
            .into_iter()
            .find(|name| name.contains(needle))
    }

    /// Somebody started a session over there — which the rig's `reach` reads,
    /// exactly as a real bridge would learn it from the far side.
    fn mark_session_running(&self) {
        std::fs::write(self.home.join(".session-running"), "").expect("marking it running");
    }

    fn dest_text(&self) -> Option<String> {
        std::fs::read_to_string(self.dest()).ok()
    }

    fn asked_what(&self) -> Vec<&'static str> {
        self.asked.iter().map(|step| step.what).collect()
    }
}

/// Where a utility the rig symlinks actually lives.
///
/// A shim that wants to hand the job on to the real thing cannot say
/// `exec mv "$@"`: `$PATH` puts the shim directory first, so that is the shim
/// calling itself. It needs the absolute path — the same one [`Rig::new`] links
/// into the rig's `$PATH`, which is why both go through here.
fn real_tool(name: &str) -> PathBuf {
    ["/usr/bin", "/bin"]
        .iter()
        .map(|dir| Path::new(dir).join(name))
        .find(|path| path.exists())
        .unwrap_or_else(|| panic!("the rig needs {name} in /usr/bin or /bin"))
}

/// A shim that refuses some invocations of a real utility and performs every
/// other one **for real** — which is what makes the recovery the rig then
/// asserts a real recovery rather than another stub.
///
/// Two independent refusal conditions, because roost's own scripts distinguish
/// their steps two different ways. `first_arg` matches `$1`: roost's commit runs
/// `mv --` and its rollback `mv -f --` (an unlink-then-rename has a window with
/// no binary in it; a rename over a regular file has none), so `-f` tells them
/// apart without guessing at paths. `needle` matches any argument, which is how
/// a step is named by the file it is touching.
fn refusing(tool: &str, first_arg: Option<&str>, needle: Option<&str>, message: &str) -> String {
    let mut script = String::from("#!/bin/sh\n");
    if let Some(first) = first_arg {
        script.push_str(&format!(
            "if [ \"$1\" = {first} ]; then printf '%s\\n' '{message}' >&2; exit 1; fi\n"
        ));
    }
    if let Some(needle) = needle {
        script.push_str(&format!(
            "for arg in \"$@\"; do\n  case \"$arg\" in\n    \
             *{needle}*) printf '%s\\n' '{message}' >&2; exit 1 ;;\n  esac\ndone\n"
        ));
    }
    script.push_str(&format!("exec {} \"$@\"\n", real_tool(tool).display()));
    script
}

const MV_REFUSED: &str = "mv: cannot move: simulated failure";
const RM_REFUSED: &str = "rm: cannot remove: simulated failure";

/// An `mv` that refuses exactly the renames whose arguments match `needle`.
///
/// This is what makes "the commit failed **after** the incumbent was moved
/// aside" reachable. roost's commit script is two renames — `dest` → `backup`,
/// then `tmp` → `dest` — and only a failure of the second leaves an incumbent
/// stranded at `.bak.<pid>` with nothing at `dest`. A test that instead makes
/// the commit die at its own pre-rename guards (a directory at `dest`, say)
/// asserts nothing about the rollback at all: there is no backup to put back, so
/// its filesystem assertions pass even with the restore deleted outright.
fn failing_mv(needle: &str) -> String {
    refusing("mv", None, Some(needle), MV_REFUSED)
}

/// An `mv` that refuses the **rollback's** rename and nothing else.
fn failing_rollback_mv() -> String {
    refusing("mv", Some("-f"), None, MV_REFUSED)
}

/// An `mv` that fails the commit's second rename **and** the rollback that would
/// undo it.
///
/// The compound case, and the one the copy used to get most wrong: the incumbent
/// has been moved aside, the temporary never landed, and the restore that would
/// have put things back did not run — so roost's "any previous install there was
/// put back" is exactly false, and the only useful sentence is the one that names
/// where the old binary actually is.
fn failing_commit_and_rollback_mv() -> String {
    refusing("mv", Some("-f"), Some(".tmp."), MV_REFUSED)
}

/// An `rm` that refuses to remove anything whose path matches `needle`.
///
/// `.tmp.` fails the cleanup; `.bak.` fails the discard. Both are best-effort
/// steps whose outcome used to be dropped on the floor.
fn failing_rm(needle: &str) -> String {
    refusing("rm", None, Some(needle), RM_REFUSED)
}

/// A `tee` that writes the first `bytes` bytes of what it is fed and then
/// reports a full disk.
///
/// **A genuinely partial file at the staged path**, which is the thing a stream
/// that dies part-way actually leaves behind — as distinct from replacing the
/// step's outcome before a single byte is written, which exercises deleting
/// prepare's empty reservation and calls it a truncated stream. A copy of the
/// prefix is kept at `$HOME/.partial-evidence` so the test can prove the
/// partial write happened after the cleanup has removed the evidence.
fn truncating_tee(bytes: usize) -> String {
    format!(
        r#"#!/bin/sh
for arg in "$@"; do out="$arg"; done
cat > "$out.whole"
head -c {bytes} "$out.whole" > "$out"
head -c {bytes} "$out.whole" > "${{HOME}}/.partial-evidence"
rm -f "$out.whole"
printf '%s\n' 'tee: write error: No space left on device' >&2
exit 1
"#
    )
}

/// A `tee` that plants a **stale backup at this attempt's own `.bak.<pid>`**
/// and then fails the stream.
///
/// The corruption path, staged: an earlier install's discard failed, the pid
/// that named its backup has been handed out again, and the file is sitting at
/// exactly the path this attempt's rollback would restore from. The backup name
/// is derived here the way roost derives it — `dest` plus `.bak.` plus the same
/// `$$` — from the temporary's own name, which is the only place this side can
/// learn the remote pid.
fn stale_backup_planting_tee(stale: &str) -> String {
    format!(
        r#"#!/bin/sh
for arg in "$@"; do out="$arg"; done
cat > /dev/null
printf '%s\n' '{stale}' > "${{out%.tmp.*}}.bak.${{out##*.tmp.}}"
printf '%s\n' 'tee: write error: No space left on device' >&2
exit 1
"#
    )
}

/// A `chmod` that marks the far side as having a session, then does the job.
///
/// The verify step's `chmod -- 700 <tmp>` is the last exec before the commit, so
/// a session that appears there appears inside exactly the window the
/// pre-commit `session.identify` exists to narrow.
fn session_starting_chmod() -> String {
    format!(
        r#"#!/bin/sh
: > "${{HOME}}/.session-running"
exec {real} "$@"
"#,
        real = real_tool("chmod").display(),
    )
}

/// The script a step is feeding to `/bin/sh -s`, as text. Borrowed for the
/// ordinary case — roost's scripts are valid UTF-8 — so the two callers that
/// only ever `contains()` it do not each copy it onto the heap first.
fn script_text(stdin: &Stdin) -> Cow<'_, str> {
    match stdin {
        Stdin::Bytes(bytes) => String::from_utf8_lossy(bytes),
        _ => Cow::Borrowed(""),
    }
}

/// Name a step by what it actually runs — derived from roost's own script text,
/// so a builder that changed shape would show up here rather than silently
/// relabel a row.
fn label_exec(command: &str, stdin: &Stdin) -> &'static str {
    if command.contains("tee --") {
        return "stream";
    }
    if command.contains("command -v roost-session") {
        return "path-check";
    }
    if command.contains(" start'") || command.contains(" start\"") {
        return "start";
    }
    let script = script_text(stdin);
    if script.contains("uname -s") {
        "discovery"
    } else if script.contains("chmod -- 700") {
        "verify-staged"
    } else if script.contains("chmod -- 755") {
        "commit"
    } else if script.contains("mv -f --") {
        "rollback"
    } else if script.contains(".tmp.$$") {
        "prepare"
    } else if script.contains("identify 2>/dev/null") {
        "identity"
    } else if script.contains("rm -f --") {
        // Same builder shape, different path, and the difference matters to
        // every assertion about the undo chain: one removes this attempt's
        // temporary, the other drops the incumbent's backup.
        if script.contains(".bak.") {
            "discard"
        } else {
            "cleanup"
        }
    } else {
        "unknown"
    }
}

// ============================================================================
// Driving
// ============================================================================

trait Driveable {
    type Out;
    fn begin(&mut self) -> Step<Self::Out>;
    fn feed(&mut self, outcome: Outcome) -> Step<Self::Out>;
}

impl Driveable for ProbeMachine {
    type Out = Result<Probe, BootstrapFailure>;
    fn begin(&mut self) -> Step<Self::Out> {
        ProbeMachine::begin(self)
    }
    fn feed(&mut self, outcome: Outcome) -> Step<Self::Out> {
        ProbeMachine::feed(self, outcome)
    }
}

impl Driveable for InstallMachine {
    type Out = Result<Installed, BootstrapFailure>;
    fn begin(&mut self) -> Step<Self::Out> {
        InstallMachine::begin(self)
    }
    fn feed(&mut self, outcome: Outcome) -> Step<Self::Out> {
        InstallMachine::feed(self, outcome)
    }
}

/// Run a machine to `Done`, doing every step for real.
async fn drive<M: Driveable>(rig: &mut Rig, machine: &mut M) -> M::Out {
    let mut step = machine.begin();
    for _ in 0..64 {
        let outcome = match step {
            Step::Done(result) => return result,
            Step::Exec {
                command,
                stdin,
                capture_stdout,
                ..
            } => rig.exec(&command, stdin, capture_stdout),
            Step::Call { op, params } => rig.call(&op, params).await,
            Step::Hooks { client_label } => rig.hooks(&client_label).await,
        };
        step = machine.feed(outcome);
    }
    panic!("the machine never finished — 64 steps is a loop, not a bootstrap");
}

async fn probe(rig: &mut Rig) -> Result<Probe, BootstrapFailure> {
    let mut machine = ProbeMachine::new(TARGET, true);
    drive(rig, &mut machine).await
}

/// Probe, then install against that probe's own fingerprint — the ordinary
/// consent flow, with the consent implied.
async fn probe_then_install(
    rig: &mut Rig,
    source: Option<SourceHandle>,
) -> Result<Installed, BootstrapFailure> {
    let found = probe(rig).await.expect("the consent probe");
    install_with(rig, &found.fingerprint, source).await
}

async fn install_with(
    rig: &mut Rig,
    fingerprint: &str,
    source: Option<SourceHandle>,
) -> Result<Installed, BootstrapFailure> {
    let mut machine = InstallMachine::new(
        InstallRequest {
            target: TARGET.to_string(),
            jail_fs_root: true,
            fingerprint: fingerprint.to_string(),
            client_label: LABEL.to_string(),
        },
        source,
    );
    rig.asked.clear();
    drive(rig, &mut machine).await
}

// ============================================================================
// The happy paths
// ============================================================================

/// A cold host: nothing installed, nothing running, no `$HOME/.local/bin` at
/// all. Install, start, wire hooks — and the PATH warning rides out on the
/// success value because `~/.local/bin` is not on that host's PATH.
#[tokio::test]
async fn a_cold_host_is_installed_started_and_wired() {
    let mut rig = Rig::new().await;

    let found = probe(&mut rig).await.expect("the probe");
    assert_eq!(found.outcome, ProbeOutcome::Missing);
    assert_eq!(found.session, SessionState::NotInstalled);
    assert_eq!(found.arch, expected_arch());
    assert_eq!(
        Plan::for_probe(TARGET, &found),
        Plan::Install {
            dest: Some(rig.dest().display().to_string()),
        }
    );
    assert!(
        rig.fake.agent_hooks_calls().is_empty(),
        "no hook op happens without consent — a probe wires nothing"
    );

    let source = rig.source(&identity_v4());
    let installed = install_with(&mut rig, &found.fingerprint, Some(source))
        .await
        .expect("the install");

    // The order is roost's, and the order is the safety argument.
    assert_eq!(
        rig.asked_what(),
        vec![
            "discovery",
            "path-check",
            "session.identify",
            "prepare",
            "stream",
            "verify-staged",
            // The race narrower: one last look at whether anything is serving,
            // after the staged bytes have proved themselves and before the
            // commit's `mv` replaces anything.
            "session.identify",
            "commit",
            "identity",
            "discard",
            "start",
            "session.identify",
            "path-check",
            "session.set_agent_hooks",
        ]
    );
    assert_eq!(
        installed.dest.as_deref(),
        Some(rig.dest().display().to_string().as_str())
    );
    assert_eq!(installed.verdict.as_deref(), Some("ready pid=4242"));
    assert_eq!(installed.session.map(|s| s.session_protocol), Some(4));
    assert!(rig.dest().is_file(), "the binary is at the destination");
    assert!(rig.residue().is_empty(), "no .tmp or .bak is left behind");

    let warning = installed.path_warning.expect("a PATH warning");
    assert!(warning.contains("isn't on roost:popos/p019-a's PATH"));
    assert!(
        warning.contains("shed doesn't need it"),
        "the warning says shed does not act on it — pin P5"
    );

    // The hooks dialogue reached the host with exactly what §3.4 pins.
    let hooks = installed.hooks.expect("a hooks result");
    assert!(hooks.applied());
    assert_eq!(hooks.client_label, LABEL);
    assert!(hooks.lease.is_some(), "the lease is kept, not dropped");
    let calls = rig.fake.agent_hooks_calls();
    assert_eq!(calls.len(), 1);
    assert_eq!(calls[0]["mode"], json!("auto"));
    assert_eq!(calls[0]["skip"], json!([]));
    assert_eq!(calls[0]["client"], json!(LABEL));
    assert_eq!(calls[0]["lease"], json!(hooks.lease.as_deref().unwrap()));
    assert_eq!(
        rig.fake.lease_label().as_deref(),
        Some(LABEL),
        "shed holds the lease under its own label"
    );
}

/// The stream step must not ask for its stdout, and must be fed the source.
#[tokio::test]
async fn the_stream_step_never_captures_stdout() {
    let mut rig = Rig::new().await;
    let source = rig.source(&identity_v4());
    probe_then_install(&mut rig, Some(source))
        .await
        .expect("the install");

    let stream = rig
        .asked
        .iter()
        .find(|step| step.what == "stream")
        .expect("a stream step");
    assert!(
        !stream.capture_stdout,
        "`tee` echoes every byte; buffering a whole binary back would be absurd"
    );
    for step in &rig.asked {
        if step.kind == "exec" && step.what != "stream" {
            assert!(step.capture_stdout, "{} wants its answer", step.what);
        }
    }
}

/// A compatible binary with nothing serving: start only, nothing written.
#[tokio::test]
async fn a_compatible_binary_that_is_not_running_is_just_started() {
    let mut rig = Rig::new().await;
    rig.seed_incumbent(&identity_v4());
    let before = rig.dest_text().expect("the seeded binary");

    let found = probe(&mut rig).await.expect("the probe");
    assert!(matches!(found.outcome, ProbeOutcome::Compatible { .. }));
    assert_eq!(found.session, SessionState::NoSession);
    let plan = Plan::for_probe(TARGET, &found);
    assert_eq!(
        plan,
        Plan::Start {
            path: rig.dest().display().to_string()
        }
    );
    assert!(!plan.needs_source());

    let installed = install_with(&mut rig, &found.fingerprint, None)
        .await
        .expect("the start-only flow");
    assert_eq!(installed.dest, None, "nothing was written");
    assert_eq!(rig.dest_text().as_deref(), Some(before.as_str()));
    assert!(rig.residue().is_empty());
    assert!(installed
        .hooks
        .expect("hooks after a start shed made")
        .applied());
    assert_eq!(
        rig.asked_what(),
        vec![
            "discovery",
            "path-check",
            "identity",
            "session.identify",
            "start",
            "session.identify",
            "session.set_agent_hooks",
        ],
        "no post-install PATH check: nothing was installed for a shell to \
         disagree about"
    );
    assert_eq!(installed.path_warning, None);
}

/// A session shed can already talk to: nothing to do, and no hooks — shed only
/// wires hooks after a Start **it performed**.
#[tokio::test]
async fn a_running_compatible_session_is_left_entirely_alone() {
    let mut rig = Rig::new().await;
    rig.seed_incumbent(&identity_v4());
    rig.mark_session_running();

    let found = probe(&mut rig).await.expect("the probe");
    let SessionState::Running { identity } = &found.session else {
        panic!("expected a running session, got {:?}", found.session);
    };
    assert_eq!(identity.session_protocol, 4);
    assert_eq!(
        Plan::for_probe(TARGET, &found),
        Plan::UpToDate {
            identity: identity.clone()
        }
    );

    let installed = install_with(&mut rig, &found.fingerprint, None)
        .await
        .expect("an up-to-date host is a success, not a failure");
    assert!(matches!(installed.plan, Plan::UpToDate { .. }));
    assert_eq!(installed.dest, None);
    assert_eq!(installed.verdict, None, "nothing was started");
    assert_eq!(installed.hooks, None, "no Start of shed's, so no hooks");
    assert!(rig.fake.agent_hooks_calls().is_empty());
}

/// A stale binary, nothing running: back it up, replace it, start it.
#[tokio::test]
async fn a_stale_binary_is_updated_and_the_backup_is_discarded() {
    let mut rig = Rig::new().await;
    rig.seed_incumbent(&identity_v2());

    let found = probe(&mut rig).await.expect("the probe");
    let ProbeOutcome::Mismatch { identity, .. } = &found.outcome else {
        panic!("expected a mismatch, got {:?}", found.outcome);
    };
    assert_eq!(identity.as_ref().map(|i| i.session_protocol), Some(2));
    let plan = Plan::for_probe(TARGET, &found);
    let Plan::Update { replaces_newer, .. } = &plan else {
        panic!("expected an update, got {plan:?}");
    };
    assert!(!replaces_newer, "protocol 2 is older than 4");

    let source = rig.source(&identity_v4());
    install_with(&mut rig, &found.fingerprint, Some(source))
        .await
        .expect("the update");
    assert!(rig.dest_text().expect("a binary").contains("(new)"));
    assert!(rig.residue().is_empty(), "the backup was discarded");
}

/// A **newer** incumbent is a downgrade wearing an update's clothes, and the
/// plan says so rather than letting a user find out afterwards.
#[test]
fn an_incumbent_on_a_newer_protocol_is_flagged_as_a_replacement() {
    let probe = Probe {
        outcome: ProbeOutcome::Mismatch {
            path: "/home/shed/.local/bin/roost-session".into(),
            identity: Some(Identity {
                app_version: "0.1.0".into(),
                session_protocol: 9,
                libghostty_build: "later".into(),
            }),
        },
        arch: "arm64".into(),
        home: "/home/shed".into(),
        session: SessionState::NoSession,
        candidates: vec!["/home/shed/.local/bin/roost-session".into()],
        fingerprint: String::new(),
    };
    let Plan::Update { replaces_newer, .. } = Plan::for_probe(TARGET, &probe) else {
        panic!("expected an update");
    };
    assert!(replaces_newer);
}

// ============================================================================
// The failure-injection table (plan 019 §5, C4)
// ============================================================================

/// prepare fails — `$HOME/.local` is a file, so `mkdir -p` cannot win.
#[tokio::test]
async fn prepare_fails_and_nothing_is_written() {
    let mut rig = Rig::new().await;
    std::fs::write(rig.home.join(".local"), "not a directory").expect("blocking .local");

    let source = rig.source(&identity_v4());
    let failure = probe_then_install(&mut rig, Some(source))
        .await
        .expect_err("prepare cannot succeed");

    assert_eq!(failure.stage, Stage::Prepare);
    assert!(
        failure
            .message
            .starts_with("couldn't prepare roost:popos/p019-a for the install: "),
        "{}",
        failure.message
    );
    // **The far side's own diagnosis reaches the user.** The exit status is read
    // before the output is parsed, so a `mkdir` that could not win is reported
    // as a `mkdir` that could not win — not as "it reported 0 paths, not 2",
    // which is what parsing an empty stdout would have said about a full disk or
    // a permission problem.
    assert!(
        failure.message.contains("it exited 1: mkdir: "),
        "{}",
        failure.message
    );
    assert!(failure.message.ends_with("Nothing was written there."));
    assert!(!rig.home.join(".local/bin").exists());
    assert!(rig.residue().is_empty());
    // Nothing to undo, so no undo steps were asked for.
    assert!(!rig.asked_what().contains(&"rollback"));
}

/// The stream dies part-way — a full disk, roost's own diagnosis — with a
/// **genuinely partial file** at the staged path. It goes, and the incumbent is
/// never touched at all.
///
/// The partial write is the point. Replacing the step's outcome before any bytes
/// are streamed leaves prepare's empty reservation at `<dest>.tmp.<pid>`, so the
/// test would be about deleting an empty file — which every arrangement of this
/// code does correctly. Here `tee` writes a prefix of the real binary and then
/// fails, and `$HOME/.partial-evidence` keeps a copy of that prefix so the
/// assertion survives the cleanup that removes it.
#[tokio::test]
async fn a_truncated_stream_removes_the_staged_file_and_keeps_the_incumbent() {
    let mut rig = Rig::new().await;
    rig.seed_incumbent(&identity_v2());
    let before = rig.dest_text().expect("the incumbent");
    rig.shim("tee", &truncating_tee(64));

    let source = rig.source(&identity_v4());
    let whole = std::fs::read(rig.dir.0.join("source-roost-session")).expect("the source bytes");
    let failure = probe_then_install(&mut rig, Some(source))
        .await
        .expect_err("a truncated stream fails");

    let partial = std::fs::read(rig.home.join(".partial-evidence")).expect("a partial write");
    assert_eq!(partial.len(), 64, "a prefix of the binary reached the host");
    assert_eq!(
        partial.as_slice(),
        &whole[..64],
        "and it is a prefix of the bytes that were being streamed"
    );

    assert_eq!(failure.stage, Stage::Stream);
    assert!(failure.message.contains("usually a full disk"));
    assert!(failure
        .message
        .ends_with("The staged file was removed and the existing install is unchanged."));
    assert_eq!(
        rig.dest_text().as_deref(),
        Some(before.as_str()),
        "byte for byte, the incumbent is what it was"
    );
    assert!(rig.residue().is_empty(), "the partial file was removed");
    assert_eq!(
        rig.asked_what().last(),
        Some(&"cleanup"),
        "the cleanup is the whole undo: nothing has moved the incumbent, so there is \
         nothing this attempt is entitled to put back"
    );
    assert!(
        !rig.asked_what().contains(&"rollback"),
        "a pre-commit failure never runs the rollback — see `commit_ran`"
    );
}

/// The staged verify refuses on the protocol number — shed's gate, not roost's
/// triple — and the existing install is left exactly as it was.
#[tokio::test]
async fn the_staged_verify_refuses_a_wrong_protocol_before_anything_is_replaced() {
    let mut rig = Rig::new().await;
    rig.seed_incumbent(&identity_v2());
    let before = rig.dest_text().expect("the incumbent");

    // Bytes that arrive, execute, identify — and speak protocol 2.
    let source = rig.source(&identity_v2());
    let failure = probe_then_install(&mut rig, Some(source))
        .await
        .expect_err("protocol 2 is not installable by this build");

    assert_eq!(failure.stage, Stage::Verify);
    assert_eq!(
        failure.message,
        "the roost-session staged on roost:popos/p019-a isn't one this shed can talk to: \
         it speaks session protocol 2, and this shed speaks 4 (it reports itself as \
         roost-session 0.0.19). It was removed and roost:popos/p019-a's existing install \
         was left exactly as it was."
    );
    assert_eq!(rig.dest_text().as_deref(), Some(before.as_str()));
    assert!(rig.residue().is_empty());
}

/// The commit fails **after** the incumbent has been renamed aside — the only
/// shape of commit failure the rollback promise is actually about.
///
/// roost's commit is two renames: `dest` → `backup`, then `tmp` → `dest`. A
/// failure of the first, or of either guard before it, leaves nothing to undo;
/// only a failure of the *second* leaves the incumbent stranded at
/// `.bak.<pid>` with nothing at `dest` at all. So the `mv` here refuses exactly
/// the rename that carries the temporary, and the assertion that matters is
/// that the incumbent's own bytes are back where they started.
#[tokio::test]
async fn a_commit_that_fails_after_the_incumbent_moved_puts_it_back() {
    let mut rig = Rig::new().await;
    rig.seed_incumbent(&identity_v2());
    let before = rig.dest_text().expect("the incumbent");
    rig.shim("mv", &failing_mv(".tmp."));

    let source = rig.source(&identity_v4());
    let failure = probe_then_install(&mut rig, Some(source))
        .await
        .expect_err("a commit whose rename fails is not an install");

    assert_eq!(failure.stage, Stage::Commit);
    assert!(failure
        .message
        .ends_with("The staged file was removed and any previous install there was put back."));
    assert!(
        !failure.message.contains("unchanged"),
        "the commit copy must never claim an unchanged install"
    );
    assert_eq!(
        rig.dest_text().as_deref(),
        Some(before.as_str()),
        "the incumbent's own bytes are back at the destination"
    );
    assert!(
        rig.residue().is_empty(),
        "no .bak and no .tmp is left behind"
    );
    assert_eq!(
        rig.asked_what().iter().rev().take(2).collect::<Vec<_>>(),
        vec![&"cleanup", &"rollback"],
        "roost's order: put the incumbent back first, then remove the temporary"
    );
}

/// The commit's other guard: `dest` is a directory, which POSIX `mv` would move
/// the file *into* while exiting 0. roost's explicit `[ ! -d ]` is what turns
/// that into a failure.
///
/// **This row proves the guard and nothing about the rollback.** The commit dies
/// before either rename, so there is no backup and the undo chain has nothing to
/// restore — which is why it is a separate row from the one above rather than
/// the failure-injection table's answer for "a commit that goes wrong".
#[tokio::test]
async fn a_commit_that_cannot_win_removes_the_staged_file() {
    let mut rig = Rig::new().await;
    std::fs::create_dir_all(rig.dest()).expect("making the destination a directory");

    let source = rig.source(&identity_v4());
    let failure = probe_then_install(&mut rig, Some(source))
        .await
        .expect_err("a directory at dest is not installable");

    assert_eq!(failure.stage, Stage::Commit);
    assert!(failure
        .message
        .ends_with("The staged file was removed and any previous install there was put back."));
    assert!(rig.dest().is_dir(), "the directory is still there");
    assert!(rig.residue().is_empty());
}

/// The post-commit identify fails **with** an incumbent: it is put back, and the
/// copy says so.
#[tokio::test]
async fn a_post_commit_failure_restores_the_incumbent() {
    let mut rig = Rig::new().await;
    rig.seed_incumbent(&identity_v2());
    let before = rig.dest_text().expect("the incumbent");
    // The staged verify answers normally; the post-commit check does not.
    rig.queue("identify.new", &["", "FAIL"]);

    let source = rig.source(&identity_v4());
    let failure = probe_then_install(&mut rig, Some(source))
        .await
        .expect_err("a file that will not identify itself is not an install");

    assert_eq!(failure.stage, Stage::PostCommit);
    assert_eq!(failure.restored, Some(true));
    assert!(failure
        .message
        .contains("wouldn't identify itself afterwards"));
    assert!(failure
        .message
        .contains("The previous install on roost:popos/p019-a has been put back"));
    assert_eq!(
        rig.dest_text().as_deref(),
        Some(before.as_str()),
        "the incumbent's own bytes are back at the destination"
    );
    assert!(
        rig.residue().is_empty(),
        "the backup and temporary are gone"
    );
}

/// The same failure **without** an incumbent: there is nothing to fall back to,
/// the new binary stays, and the copy sends the user to look at it.
#[tokio::test]
async fn a_post_commit_failure_with_no_incumbent_keeps_the_new_binary() {
    let mut rig = Rig::new().await;
    rig.queue("identify.new", &["", "FAIL"]);

    let source = rig.source(&identity_v4());
    let failure = probe_then_install(&mut rig, Some(source))
        .await
        .expect_err("a file that will not identify itself is not an install");

    assert_eq!(failure.stage, Stage::PostCommit);
    assert_eq!(failure.restored, Some(false));
    assert!(failure
        .message
        .contains("There was no previous install to fall back to"));
    assert!(
        rig.dest_text().expect("a binary at dest").contains("(new)"),
        "the new one is still there — which is what the copy says"
    );
    assert!(rig.residue().is_empty());
}

/// **The corruption path, as a regression test.**
///
/// An earlier install's discard failed and nobody came back for the
/// `<dest>.bak.<pid>` it left — `prepare_script` sweeps `.tmp.*` and never
/// `.bak.*` — and the pid that named it has since been handed out again. The
/// stream then fails. If a pre-commit failure runs the rollback, that rollback
/// renames a *stranger's* stale bytes over a perfectly good incumbent and the
/// copy reports the existing install as unchanged.
///
/// The assertion that carries this is the byte-for-byte one: the incumbent at
/// the destination afterwards has to be the file that was seeded, not the stale
/// backup. With the `commit_ran` gate removed, it is the stale backup.
#[tokio::test]
async fn a_stale_backup_at_a_reused_pid_is_never_rolled_forward() {
    const STALE: &str = "STALE BYTES FROM AN EARLIER INSTALL";

    let mut rig = Rig::new().await;
    rig.seed_incumbent(&identity_v2());
    let before = rig.dest_text().expect("the incumbent");
    rig.shim("tee", &stale_backup_planting_tee(STALE));

    let source = rig.source(&identity_v4());
    let failure = probe_then_install(&mut rig, Some(source))
        .await
        .expect_err("the stream failed");

    assert_eq!(failure.stage, Stage::Stream);
    assert_eq!(
        rig.dest_text().as_deref(),
        Some(before.as_str()),
        "the incumbent is byte-identical: shed will not restore a backup this attempt \
         did not make"
    );
    assert!(
        !rig.asked_what().contains(&"rollback"),
        "and it did not even ask — the rollback is gated on this attempt's own commit"
    );

    // The stale file is still exactly where it was found. Shed neither restored
    // it nor deleted it: it is not this attempt's to touch.
    let residue = rig.residue();
    assert_eq!(residue.len(), 1, "{residue:?}");
    assert!(residue[0].contains(".bak."), "{residue:?}");
    let stale = std::fs::read_to_string(rig.home.join(".local/bin").join(&residue[0]))
        .expect("the stale backup");
    assert_eq!(stale.trim(), STALE);
}

/// The rollback itself fails: the incumbent is stranded at `.bak.<pid>`, and the
/// copy says so and names the path rather than claiming it was put back.
#[tokio::test]
async fn a_rollback_that_fails_says_where_the_incumbent_is() {
    let mut rig = Rig::new().await;
    rig.seed_incumbent(&identity_v2());
    // The commit lands; the post-commit identify does not; the rollback that
    // would undo it cannot run.
    rig.queue("identify.new", &["", "FAIL"]);
    rig.shim("mv", &failing_rollback_mv());

    let source = rig.source(&identity_v4());
    let failure = probe_then_install(&mut rig, Some(source))
        .await
        .expect_err("a file that will not identify itself is not an install");

    assert_eq!(failure.stage, Stage::PostCommit);
    assert_eq!(failure.restored, Some(false));
    assert!(
        failure
            .message
            .contains("Shed then couldn't put the previous install back"),
        "{}",
        failure.message
    );
    assert!(
        !failure
            .message
            .contains("There was no previous install to fall back to"),
        "there WAS one, and saying otherwise is the worst of the three answers: {}",
        failure.message
    );
    let backup = rig
        .residue_named(".bak.")
        .expect("the incumbent is stranded at its backup");
    assert!(
        failure.message.contains(&backup),
        "the sentence names the path a human has to move: {}",
        failure.message
    );
    assert!(
        failure.message.contains(&rig.dest().display().to_string()),
        "and where to move it to: {}",
        failure.message
    );
}

/// The same undo failure one stage earlier: the **commit** moved the incumbent
/// aside, failed, and could not put it back.
///
/// roost's commit copy is careful — it refuses to claim an unchanged install
/// because the rename may have landed — but it does promise the incumbent was
/// restored, because on every path roost's own runtime takes, it was. When the
/// restore is the thing that failed, that promise is the one sentence the user
/// must not be given: the old binary is at `.bak.<pid>` and nothing is at the
/// destination at all.
#[tokio::test]
async fn a_commit_whose_rollback_also_fails_names_the_stranded_incumbent() {
    let mut rig = Rig::new().await;
    rig.seed_incumbent(&identity_v2());
    let before = rig.dest_text().expect("the incumbent");
    rig.shim("mv", &failing_commit_and_rollback_mv());

    let source = rig.source(&identity_v4());
    let failure = probe_then_install(&mut rig, Some(source))
        .await
        .expect_err("a commit whose rename fails is not an install");

    assert_eq!(failure.stage, Stage::Commit);
    assert!(
        !failure
            .message
            .contains("any previous install there was put back"),
        "it was not put back, and that is the claim that matters: {}",
        failure.message
    );
    assert!(
        failure
            .message
            .contains("Shed then couldn't put the previous install back either"),
        "{}",
        failure.message
    );

    let backup = rig
        .residue_named(".bak.")
        .expect("the incumbent is stranded at its backup");
    assert!(failure.message.contains(&backup), "{}", failure.message);
    assert!(
        failure.message.contains(&rig.dest().display().to_string()),
        "and says where it belongs: {}",
        failure.message
    );
    assert_eq!(rig.dest_text(), None, "nothing is at the destination");
    assert_eq!(
        std::fs::read_to_string(rig.home.join(".local/bin").join(&backup)).ok(),
        Some(before),
        "and the old bytes are intact where the sentence says they are"
    );
}

/// The cleanup fails: the staged file is still on the host, and the sentence
/// corrects the clause that said it was removed.
#[tokio::test]
async fn a_cleanup_that_fails_says_the_staged_file_is_still_there() {
    let mut rig = Rig::new().await;
    rig.seed_incumbent(&identity_v2());
    let before = rig.dest_text().expect("the incumbent");
    rig.inject(
        "tee --",
        Outcome::Exec {
            exit: Some(1),
            stdout: Vec::new(),
            stderr_tail: "tee: write error: No space left on device\n".into(),
        },
    );
    rig.shim("rm", &failing_rm(".tmp."));

    let source = rig.source(&identity_v4());
    let failure = probe_then_install(&mut rig, Some(source))
        .await
        .expect_err("the stream failed");

    assert_eq!(failure.stage, Stage::Stream);
    let staged = rig
        .residue_named(".tmp.")
        .expect("the staged file is still on the host");
    assert!(
        failure
            .message
            .contains("The staged file could not be removed after all"),
        "{}",
        failure.message
    );
    assert!(
        failure.message.contains(&staged),
        "the sentence names the file that is still there: {}",
        failure.message
    );
    assert_eq!(
        rig.dest_text().as_deref(),
        Some(before.as_str()),
        "the incumbent is still untouched — a failed cleanup is not a failed promise"
    );
}

/// The discard fails: the install still succeeded, and the leftover backup rides
/// out on the success value.
///
/// This is also the state that arms the stale-`.bak` trap two rows up, which is
/// the reason it is reported at all rather than silently swallowed.
#[tokio::test]
async fn a_discard_that_fails_is_not_fatal_and_is_still_reported() {
    let mut rig = Rig::new().await;
    rig.seed_incumbent(&identity_v2());
    rig.shim("rm", &failing_rm(".bak."));

    let source = rig.source(&identity_v4());
    let installed = probe_then_install(&mut rig, Some(source))
        .await
        .expect("a backup that would not go away is not a failed install");

    assert!(rig.dest_text().expect("a binary").contains("(new)"));
    let warning = installed
        .backup_warning
        .expect("the leftover backup is reported");
    let backup = rig
        .residue_named(".bak.")
        .expect("the backup is still on the host");
    assert!(warning.contains(&backup), "{warning}");
    assert!(warning.contains("nothing will clean it up"), "{warning}");
    assert!(
        installed.hooks.expect("hooks still ran").applied(),
        "the install finished — the backup is untidy, not broken"
    );
}

/// **Somebody else's binary is what landed.**
///
/// Plan 019 §3.4 pins that two concurrent installers are last-writer-wins, with
/// the post-commit identify as the detector. A detector that compares only the
/// protocol number is not one: client B's *different* protocol-4 build passes it,
/// and client A then discards its backup and reports that it installed bytes it
/// never installed. So the check is against the identity the **staged verify**
/// accepted.
#[tokio::test]
async fn a_foreign_install_that_lands_first_is_reported_and_not_overwritten() {
    let mut rig = Rig::new().await;
    rig.seed_incumbent(&identity_v2());
    // The staged verify sees shed's own bytes; by the time the destination is
    // asked, a different protocol-4 build is answering there.
    let theirs = json!({
        "app_version": "0.0.20",
        "session_protocol": 4,
        "libghostty_build": "ghostty-somebody-elses-build",
    })
    .to_string();
    rig.queue("identify.new", &["", &theirs]);

    let source = rig.source(&identity_v4());
    let failure = probe_then_install(&mut rig, Some(source))
        .await
        .expect_err("shed did not install what it staged, and will not say it did");

    assert_eq!(failure.stage, Stage::PostCommit);
    assert!(
        failure.message.contains("is not the one shed just staged"),
        "{}",
        failure.message
    );
    assert!(failure.message.contains("0.0.20"), "{}", failure.message);
    assert!(failure.message.contains("0.0.19"), "{}", failure.message);
    assert!(
        !rig.asked_what().contains(&"rollback"),
        "shed does not put its own backup back over somebody else's install"
    );
    assert!(
        !rig.asked_what().contains(&"discard"),
        "and it does not discard the backup on the strength of a success it did not have"
    );
    let backup = rig
        .residue_named(".bak.")
        .expect("the previous install's copy is still there");
    assert!(failure.message.contains(&backup), "{}", failure.message);
}

/// **The post-probe race, narrowed.** A session starts between the re-probe and
/// the commit; the last `session.identify` catches it and nothing is replaced.
#[tokio::test]
async fn a_session_that_appears_before_the_commit_stops_the_install() {
    // Pin P6's session: protocol 2, in use, and shed will not touch it.
    let mut mismatched = Rig::new().await;
    mismatched.seed_incumbent(&identity_v2());
    mismatched.fake.set_session_protocol(2);
    let before = mismatched.dest_text().expect("the incumbent");
    mismatched.shim("chmod", &session_starting_chmod());

    let source = mismatched.source(&identity_v4());
    let failure = probe_then_install(&mut mismatched, Some(source))
        .await
        .expect_err("the binary now has a live process behind it");

    assert_eq!(failure.stage, Stage::Report);
    assert_eq!(
        failure.message,
        "roost-session on roost:popos/p019-a speaks protocol 2, this build speaks 4 — \
         upgrade whichever is older; stop it there with `roostctl session stop` and \
         reconnect once it is. Nothing was replaced on roost:popos/p019-a."
    );
    assert_eq!(
        mismatched.dest_text().as_deref(),
        Some(before.as_str()),
        "the binary under the running session is byte-identical"
    );
    assert!(mismatched.residue().is_empty(), "the temporary was removed");
    assert!(
        !mismatched.asked_what().contains(&"commit"),
        "the commit never ran"
    );

    // The same race with a session shed CAN talk to: there is nothing left to
    // install, and the refusal says so in shed's own words rather than P6's.
    let mut compatible = Rig::new().await;
    compatible.seed_incumbent(&identity_v2());
    compatible.shim("chmod", &session_starting_chmod());
    let source = compatible.source(&identity_v4());
    let failure = probe_then_install(&mut compatible, Some(source))
        .await
        .expect_err("something is serving there now");
    assert_eq!(failure.stage, Stage::Report);
    assert_eq!(
        failure.message,
        "a roost-session started on roost:popos/p019-a while shed was installing, and it \
         is one this shed can talk to — nothing was replaced; look again."
    );
    assert!(!compatible.asked_what().contains(&"commit"));
}

/// `roost-session start` answers `error:` — read off **stdout**, before the exit
/// status, which is the only place the reason ever appears.
#[tokio::test]
async fn a_start_that_refuses_reports_its_own_reason() {
    let mut rig = Rig::new().await;
    rig.queue("start.new", &["error: the profile socket is locked"]);

    let source = rig.source(&identity_v4());
    let failure = probe_then_install(&mut rig, Some(source))
        .await
        .expect_err("a start that refuses is a failed bootstrap");

    assert_eq!(failure.stage, Stage::Start);
    assert_eq!(
        failure.message,
        "roost-session was installed on roost:popos/p019-a but wouldn't start: the profile \
         socket is locked. The new binary is in place — try `roost-session start` on \
         roost:popos/p019-a."
    );
    // The rollback promise has already ended: the backup was discarded before
    // the start, and the new binary is where it was put.
    assert!(rig.dest_text().expect("a binary").contains("(new)"));
    assert!(rig.residue().is_empty());
    assert!(
        !rig.asked_what().contains(&"rollback"),
        "there is nothing left to roll back to after the backup is discarded"
    );
}

/// **A readiness line is not a verdict when the step did not finish.**
///
/// `exit: None` with a perfectly well-formed `ready pid=…` on stdout is what a
/// budget that expired mid-read, or a transport that died holding what it had,
/// looks like from here. roost's rule — read the readiness line before the exit
/// status — is about a *failure's* detail: a `roost-session start` that refuses
/// writes `error: …` to stdout and exits 1, and demanding a zero exit first
/// would replace that reason with "it exited 1". Extending it to successes let
/// this outcome wire agent hooks and return success.
#[tokio::test]
async fn a_start_with_no_exit_status_is_a_failure_however_ready_it_looks() {
    let mut rig = Rig::new().await;
    rig.inject(
        "start'",
        Outcome::Exec {
            exit: None,
            stdout: b"ready pid=4242\n".to_vec(),
            stderr_tail: String::new(),
        },
    );

    let source = rig.source(&identity_v4());
    let failure = probe_then_install(&mut rig, Some(source))
        .await
        .expect_err("a step that never finished proves nothing about a session");

    assert_eq!(failure.stage, Stage::Start);
    assert!(
        failure
            .message
            .contains("it did not finish, and said nothing about why"),
        "{}",
        failure.message
    );
    assert!(
        failure.message.contains("The new binary is in place"),
        "the backup was discarded before the start — that much is still true: {}",
        failure.message
    );
    assert!(
        rig.fake.agent_hooks_calls().is_empty(),
        "no session shed could vouch for, so no hooks"
    );
    assert!(
        !rig.asked_what().contains(&"session.set_agent_hooks"),
        "and the dialogue was never even reached"
    );
}

/// The same rule at the other step that reads prose rather than a NUL record:
/// the staged verify will not accept an identity off a step that did not finish.
#[tokio::test]
async fn the_staged_verify_needs_the_step_to_have_finished() {
    let mut rig = Rig::new().await;
    rig.seed_incumbent(&identity_v2());
    let before = rig.dest_text().expect("the incumbent");
    rig.inject(
        "chmod -- 700",
        Outcome::Exec {
            exit: None,
            stdout: identity_v4().into_bytes(),
            stderr_tail: String::new(),
        },
    );

    let source = rig.source(&identity_v4());
    let failure = probe_then_install(&mut rig, Some(source))
        .await
        .expect_err("an identity read off an unfinished step is not an identity");

    assert_eq!(failure.stage, Stage::Verify);
    assert!(
        failure
            .message
            .contains("it did not finish, and said nothing about why"),
        "{}",
        failure.message
    );
    assert_eq!(rig.dest_text().as_deref(), Some(before.as_str()));
    assert!(rig.residue().is_empty());
}

/// It started, and the session that answered speaks a protocol shed cannot read.
#[tokio::test]
async fn a_post_start_protocol_mismatch_is_a_failure_that_keeps_the_binary() {
    let mut rig = Rig::new().await;
    rig.fake.set_session_protocol(2);

    let source = rig.source(&identity_v4());
    let failure = probe_then_install(&mut rig, Some(source))
        .await
        .expect_err("a protocol-2 session is not one shed can read");

    assert_eq!(failure.stage, Stage::PostStart);
    assert_eq!(
        failure.message,
        "the roost-session that came up on roost:popos/p019-a isn't one this shed can talk \
         to: it speaks session protocol 2, and this shed speaks 4. The new binary is in \
         place on roost:popos/p019-a; nothing was rolled back."
    );
    assert!(rig.dest().is_file());
    assert!(
        rig.fake.agent_hooks_calls().is_empty(),
        "no start, no hooks"
    );
}

/// A budget that expires mid-stream — `exit: None`, the case three different
/// things produce and none of them means success.
#[tokio::test]
async fn a_budget_that_expires_mid_stream_is_a_failure_that_cleans_up() {
    let mut rig = Rig::new().await;
    rig.seed_incumbent(&identity_v2());
    let before = rig.dest_text().expect("the incumbent");
    rig.inject(
        "tee --",
        Outcome::Exec {
            exit: None,
            stdout: Vec::new(),
            stderr_tail: String::new(),
        },
    );

    let source = rig.source(&identity_v4());
    let failure = probe_then_install(&mut rig, Some(source))
        .await
        .expect_err("no exit status is a failure");

    assert_eq!(failure.stage, Stage::Stream);
    assert!(failure
        .message
        .contains("it did not finish, and said nothing about why"));
    assert_eq!(rig.dest_text().as_deref(), Some(before.as_str()));
    assert!(rig.residue().is_empty());
}

/// The other half of the `exit: None` rule, pinned: a runner that saw a clean
/// EOF for a step whose stdout it read to the end passes `Some(0)` and the
/// machine proceeds — **and that cannot launder a truncated answer**, because
/// roost's parser refuses output with no final NUL.
#[tokio::test]
async fn a_clean_eof_is_the_runners_call_and_the_parser_is_still_the_gate() {
    let complete = {
        let mut rig = Rig::new().await;
        // Exactly what the real discovery script would have printed on a cold
        // host, handed back with the status a clean-EOF runner synthesizes.
        rig.inject(
            "/bin/sh -s",
            Outcome::Exec {
                exit: Some(0),
                stdout: b"Linux\0aarch64\0/home/shed\0".to_vec(),
                stderr_tail: String::new(),
            },
        );
        probe(&mut rig).await
    };
    let found = complete.expect("a complete answer with a synthesized Some(0) is accepted");
    assert_eq!(found.home, "/home/shed");
    assert_eq!(found.arch, "arm64");

    let truncated = {
        let mut rig = Rig::new().await;
        rig.inject(
            "/bin/sh -s",
            Outcome::Exec {
                // The same synthesized status over output that was cut off.
                exit: Some(0),
                stdout: b"Linux\0aarch64\0/home/sh".to_vec(),
                stderr_tail: String::new(),
            },
        );
        probe(&mut rig).await
    };
    let failure = truncated.expect_err("a cut-off answer is refused whatever the status said");
    assert_eq!(failure.stage, Stage::Probe);
    assert!(failure.message.contains("its output was cut off"));

    let no_status = {
        let mut rig = Rig::new().await;
        rig.inject(
            "/bin/sh -s",
            Outcome::Exec {
                exit: None,
                stdout: b"Linux\0aarch64\0/home/shed\0".to_vec(),
                stderr_tail: String::new(),
            },
        );
        probe(&mut rig).await
    };
    assert!(
        no_status.is_err(),
        "`exit: None` is a failure at the machine — the exception is the runner's to apply"
    );
}

/// The host changed between the consent card and the install: cold → running.
///
/// **Only the session state moves here.** The binary on disk is the same file
/// with the same identity before and after, so the refusal can only come from
/// the session half of the fingerprint — which is the half a client would be
/// most tempted to leave out.
#[tokio::test]
async fn a_host_that_started_a_session_since_consent_is_refused() {
    let mut rig = Rig::new().await;
    rig.seed_incumbent(&identity_v4());
    let found = probe(&mut rig).await.expect("the consent probe");
    assert_eq!(found.session, SessionState::NoSession);
    assert!(matches!(found.outcome, ProbeOutcome::Compatible { .. }));

    // Somebody started it in the meantime. Nothing else about the host moved.
    rig.mark_session_running();

    let failure = install_with(&mut rig, &found.fingerprint, None)
        .await
        .expect_err("the fingerprint moved");

    assert_eq!(failure.stage, Stage::Fingerprint);
    assert_eq!(
        failure.message,
        "roost:popos/p019-a changed since you were asked — nothing was changed; look again."
    );
    assert!(rig.residue().is_empty());
    assert!(
        rig.fake.agent_hooks_calls().is_empty(),
        "a refused install wires no hooks"
    );
}

/// The same gate the other way round: compatible → missing.
#[tokio::test]
async fn a_host_whose_binary_vanished_since_consent_is_refused() {
    let mut rig = Rig::new().await;
    rig.seed_incumbent(&identity_v4());
    let found = probe(&mut rig).await.expect("the consent probe");
    assert!(matches!(found.outcome, ProbeOutcome::Compatible { .. }));

    // `shed reset` wipes `~/.local/bin`.
    std::fs::remove_file(rig.dest()).expect("removing the binary");

    let failure = install_with(&mut rig, &found.fingerprint, None)
        .await
        .expect_err("the fingerprint moved");
    assert_eq!(failure.stage, Stage::Fingerprint);
    assert!(!rig.dest().exists(), "nothing was written");
    assert!(rig.residue().is_empty());
}

/// A fingerprint is a function of the outcome and the session state, and of
/// nothing that moves on its own.
#[tokio::test]
async fn the_fingerprint_moves_for_the_two_changes_that_matter_and_no_others() {
    let mut rig = Rig::new().await;
    let cold = probe(&mut rig).await.expect("a cold probe");
    let again = probe(&mut rig).await.expect("the same host again");
    assert_eq!(cold.fingerprint, again.fingerprint, "a probe is stable");

    rig.seed_incumbent(&identity_v2());
    let stale = probe(&mut rig).await.expect("a stale binary");
    assert_ne!(cold.fingerprint, stale.fingerprint);

    rig.seed_incumbent(&identity_v4());
    let fresh = probe(&mut rig).await.expect("a compatible binary");
    assert_ne!(
        stale.fingerprint, fresh.fingerprint,
        "the identity is part of it, not merely the path"
    );

    rig.mark_session_running();
    let running = probe(&mut rig).await.expect("a running session");
    assert_ne!(fresh.fingerprint, running.fingerprint);
}

/// The two things the fingerprint used to miss, both of which a consent card had
/// already promised something about.
///
/// **`$HOME`** decides where the install lands: the card names an expanded path,
/// and `prepare_script` re-derives it from whatever `$HOME` the far side reports
/// at install time. An account whose home moved in between — a remount, a
/// changed passwd entry, a different user behind the same target name — has the
/// same arch and the same session state, so without this shed would write to a
/// path nobody was shown.
///
/// **A restarted session** is a different session, with different tabs and
/// possibly a different binary behind it, and consent against the first is not
/// consent against the second.
#[test]
fn the_fingerprint_covers_home_and_a_restarted_session() {
    let outcome = ProbeOutcome::Missing;
    let cold = SessionState::NoSession;
    assert_ne!(
        fingerprint(TARGET, "arm64", "/home/shed", &outcome, &cold),
        fingerprint(TARGET, "arm64", "/home/someone-else", &outcome, &cold),
        "$HOME is what the install destination is built from"
    );

    let running = |id: &str, started: &str| SessionState::Running {
        identity: SessionIdentity {
            app_version: "0.0.19".into(),
            session_protocol: 4,
            libghostty_build: "ghostty-f2d5758f6305867d+snapshot.v1".into(),
            session_id: id.into(),
            started_at: started.into(),
        },
    };
    let first = fingerprint(
        TARGET,
        "arm64",
        "/home/shed",
        &outcome,
        &running("s-1", "2026-09-11T10:00:00Z"),
    );
    assert_ne!(
        first,
        fingerprint(
            TARGET,
            "arm64",
            "/home/shed",
            &outcome,
            &running("s-1", "2026-09-11T10:05:00Z"),
        ),
        "the same id with a later start is a session that went away and came back"
    );
    assert_ne!(
        first,
        fingerprint(
            TARGET,
            "arm64",
            "/home/shed",
            &outcome,
            &running("s-2", "2026-09-11T10:00:00Z"),
        ),
        "and a different id is a different session whatever the clock says"
    );
}

/// Somebody else holds the interactive lease. shed steps back — it never takes
/// a session away from whoever is driving it.
#[tokio::test]
async fn hooks_skip_when_somebody_else_is_driving() {
    let mut rig = Rig::new().await;
    rig.fake.take_over("roost-ui");

    let source = rig.source(&identity_v4());
    let installed = probe_then_install(&mut rig, Some(source))
        .await
        .expect("the install still succeeds — hooks are an enrichment");

    let hooks = installed.hooks.expect("a hooks result");
    assert!(!hooks.applied());
    assert_eq!(hooks.skipped_code.as_deref(), Some("already-connected"));
    assert_eq!(hooks.lease, None, "nothing to keep re-sending");
    assert!(rig.fake.agent_hooks_calls().is_empty());
    assert_eq!(
        rig.fake.lease_label().as_deref(),
        Some("roost-ui"),
        "shed never takes over"
    );
}

/// The lease shed was holding got taken while it was using it.
#[tokio::test]
async fn hooks_stop_re_sending_once_the_lease_is_taken_over() {
    let mut rig = Rig::new().await;
    let mut owner = Conn::unix(rig.fake.socket_path())
        .await
        .expect("dialling the fake");
    let ours = owner
        .session_connect(false, Some(LABEL))
        .await
        .expect("minting a lease")
        .lease;
    rig.fake.take_over("roost-ui");

    // The cached lease a client would re-send with on its next reconnect.
    rig.hooks_seed_lease = Some(ours);
    let source = rig.source(&identity_v4());
    let installed = probe_then_install(&mut rig, Some(source))
        .await
        .expect("the install still succeeds");

    let hooks = installed.hooks.expect("a hooks result");
    assert_eq!(hooks.skipped_code.as_deref(), Some("taken-over"));
    assert_eq!(
        hooks.lease, None,
        "dropping the lease is what stops the re-sending"
    );
    assert!(rig.fake.agent_hooks_calls().is_empty());
}

/// A lease the far side has forgotten earns exactly one fresh connect.
#[tokio::test]
async fn a_forgotten_lease_is_re_minted_once() {
    let mut rig = Rig::new().await;
    rig.hooks_seed_lease = Some("0123456789abcdef0123456789abcdef".to_string());

    let source = rig.source(&identity_v4());
    let installed = probe_then_install(&mut rig, Some(source))
        .await
        .expect("the install");

    let hooks = installed.hooks.expect("a hooks result");
    assert!(hooks.applied(), "the retry found nobody else holding it");
    assert_eq!(rig.fake.agent_hooks_calls().len(), 1);
    assert_ne!(
        hooks.lease.as_deref(),
        Some("0123456789abcdef0123456789abcdef"),
        "the stale one was replaced"
    );
}

/// roost reports per-agent failures inside a **successful** reply. A client that
/// read a non-empty `errors` as a failed call would throw away four wired agents
/// over one that was not.
#[tokio::test]
async fn hooks_partial_errors_are_reported_and_are_not_a_failure() {
    let mut rig = Rig::new().await;
    rig.fake.set_agent_hooks_result(json!({
        "wired": ["claude"],
        "refreshed": ["codex"],
        "removed": [],
        "skipped": [{ "agent": "cursor", "reason": "not installed" }],
        "errors": [{ "agent": "grok", "error": "permission denied writing ~/.grok/hooks" }],
    }));

    let source = rig.source(&identity_v4());
    let installed = probe_then_install(&mut rig, Some(source))
        .await
        .expect("partial hook failures do not fail the bootstrap");

    let hooks = installed.hooks.expect("a hooks result");
    assert!(hooks.applied());
    assert_eq!(hooks.wired, vec!["claude".to_string()]);
    assert_eq!(hooks.refreshed, vec!["codex".to_string()]);
    assert_eq!(hooks.skipped.len(), 1);
    assert_eq!(hooks.skipped[0].agent, "cursor");
    assert_eq!(hooks.errors.len(), 1);
    assert_eq!(hooks.errors[0].agent, "grok");
    assert!(hooks.errors[0].error.contains("permission denied"));
}

/// A macOS or BSD host: refused before anything is asked of it.
#[tokio::test]
async fn an_unsupported_os_is_refused_with_its_own_sentence() {
    let mut rig = Rig::new().await;
    rig.shim("uname", FAKE_UNAME_DARWIN);

    let failure = probe(&mut rig).await.expect_err("Darwin has no build");
    assert_eq!(failure.stage, Stage::UnsupportedOs);
    assert_eq!(
        failure.message,
        "roost:popos/p019-a reports itself as Darwin; roost-session is built for Linux only."
    );
    assert_eq!(
        rig.asked_what(),
        vec!["discovery"],
        "the probe stops at the first answer — nothing else is asked of that host"
    );
}

/// Pin P6, at the machine: a mismatched **running** session is reported, and no
/// step that could stop or restart it is ever yielded.
#[tokio::test]
async fn a_mismatched_running_session_is_reported_and_never_touched() {
    let mut rig = Rig::new().await;
    rig.seed_incumbent(&identity_v2());
    rig.mark_session_running();
    rig.fake.set_session_protocol(2);

    let found = probe(&mut rig).await.expect("the probe");
    let plan = Plan::for_probe(TARGET, &found);
    assert_eq!(
        plan,
        Plan::Report {
            protocol: 2,
            message: "roost-session on roost:popos/p019-a speaks protocol 2, this build speaks \
                      4 — upgrade whichever is older; stop it there with `roostctl session \
                      stop` and reconnect once it is."
                .to_string(),
        }
    );
    assert!(!plan.actionable());

    let failure = install_with(&mut rig, &found.fingerprint, None)
        .await
        .expect_err("shed refuses to act on somebody else's session");
    assert_eq!(failure.stage, Stage::Report);
    assert!(failure.message.contains("speaks protocol 2"));
    assert_eq!(
        rig.asked_what(),
        vec!["discovery", "path-check", "identity", "session.identify"],
        "no start, no stop, no install — only the re-probe ran"
    );
}

/// A plan that needs bytes with no source refuses before prepare.
#[tokio::test]
async fn an_install_with_no_source_writes_nothing() {
    let mut rig = Rig::new().await;
    let failure = probe_then_install(&mut rig, None)
        .await
        .expect_err("an install needs something to install");
    assert_eq!(failure.stage, Stage::Source);
    assert!(failure
        .message
        .ends_with("roost:popos/p019-a was left untouched."));
    assert!(!rig.home.join(".local").exists());
}

/// A runner that answers the wrong kind of outcome fails as a bootstrap failure,
/// not as a panic — the two sides of that seam are in different languages on the
/// phone.
#[tokio::test]
async fn a_runner_that_answers_the_wrong_kind_fails_cleanly() {
    let mut machine = ProbeMachine::new(TARGET, true);
    machine.begin();
    let step = machine.feed(Outcome::Call(Ok(json!({}))));
    let Step::Done(Err(failure)) = step else {
        panic!("expected a failure, got {step:?}");
    };
    assert_eq!(failure.stage, Stage::Probe);
    assert!(failure
        .message
        .contains("answered a exec step with something else"));
}

// ============================================================================
// Shape assertions that need no rig
// ============================================================================

/// The ladder rung the remote shell resolves is appended AFTER every candidate,
/// never spliced among them — **and the verdict is about the first pair, not the
/// best one.**
///
/// This is the case that needs care and the only one where the two rules differ.
/// Rung 1 is a build too old to know `identify`, so it answers nothing and
/// `identity_script` keeps walking; rung 2 is a perfectly good protocol-4 binary.
/// Calling the host `Compatible` on the strength of rung 2 would offer no install
/// — forever — while the transport went on exec'ing the stale rung 1 that
/// shadows it.
#[tokio::test]
async fn the_first_pair_decides_the_verdict_and_the_shell_hit_lands_last() {
    let mut rig = Rig::new().await;
    std::fs::create_dir_all(rig.dest().parent().expect("a parent")).expect("mkdir .local/bin");
    write_exec(&rig.dest(), FAKE_ANCIENT);
    rig.shim("roost-session", &fake_session("shim", &identity_v4()));

    let found = probe(&mut rig).await.expect("the probe");
    assert_eq!(found.candidates.len(), 2);
    assert_eq!(found.candidates[0], rig.dest().display().to_string());
    assert!(
        found.candidates[1].ends_with("shims/roost-session"),
        "the remote shell's own hit is appended after every ladder rung"
    );
    assert_eq!(
        found.outcome,
        ProbeOutcome::Mismatch {
            path: rig.dest().display().to_string(),
            identity: None,
        },
        "the rung the transport will exec is the rung the verdict is about"
    );
    assert!(matches!(
        Plan::for_probe(TARGET, &found),
        Plan::Update { .. }
    ));
}

/// `begin` and `feed` compute their step rather than stashing one, so asking
/// twice asks for the same thing.
#[test]
fn a_step_can_be_read_twice() {
    let mut machine = ProbeMachine::new(TARGET, false);
    let first = machine.begin();
    let again = machine.begin();
    let (Step::Exec { command: a, .. }, Step::Exec { command: b, .. }) = (&first, &again) else {
        panic!("expected two exec steps");
    };
    assert_eq!(a, b);
}

// **What makes this whole file's `jail_fs_root: true` mean anything** — that the
// jailed scripts differ from the shipped ones by the `${ROOST_BOOTSTRAP_FS_ROOT}`
// prefix and nothing else — is asserted in `tests/roost_bootstrap_goldens.rs`,
// against every builder rather than one, with the rung count derived from
// roost's own `CANDIDATES`. It lives beside the goldens because it is the same
// tripwire on the same `roost-ipc` rev.

/// Every stage that can reach a user has a distinct name, and the two reach
/// codes are the kebab spellings roost's own classifier produces.
#[test]
fn the_reach_codes_are_roosts_own_spellings() {
    use roost_ipc::ssh::{classify_ssh_failure, SshFailure};
    assert!(matches!(
        classify_ssh_failure(Some(127), "roost-session: command not found"),
        SshFailure::NotFound
    ));
    assert!(matches!(
        classify_ssh_failure(Some(1), "client-bridge: no session"),
        SshFailure::NoSession
    ));
    let names: BTreeSet<&str> = [reach_code::NOT_INSTALLED, reach_code::NO_SESSION].into();
    assert_eq!(names, BTreeSet::from(["not-found", "no-session"]));
}

/// What this machine's own architecture reports, so the rig's assertion is not a
/// statement about the developer's laptop.
///
/// Through roost's own mapping rather than a second copy of it: the rig runs the
/// real `uname -m`, the machine runs the real `map_arch`, and an arch roost
/// publishes no build for should fail here saying so rather than as a string
/// diff against a hand-written `arm64`.
fn expected_arch() -> &'static str {
    roost_ipc::bootstrap::map_arch(std::env::consts::ARCH)
        .expect("the rig runs on an arch roost publishes a roost-session for")
        .as_str()
}

/// The same question — `session.identify` — is asked at three points, and a
/// failure has to be reported against the gate that actually asked it.
///
/// This is a direct unit test rather than a driven row because the rig produces
/// a `Call` failure by *being* unreachable, which is one of the two codes
/// [`machines::session_state`] answers rather than fails on. The uncovered case is the
/// third one: an answer that is neither, or an identity that will not parse.
/// Before this was a parameter the stage was hard-wired to `Probe`, so a
/// pre-commit refusal claimed it happened during the probe and the post-start
/// caller patched the field back by struct update.
#[test]
fn an_identify_failure_is_reported_against_the_gate_that_asked() {
    let unexpected = || {
        Outcome::Call(Err(CallError::new(
            "internal",
            "the session fell over mid-answer",
        )))
    };
    for stage in [Stage::Probe, Stage::Commit, Stage::PostStart] {
        let answer = match unexpected() {
            Outcome::Call(answer) => answer,
            _ => unreachable!("built one line up"),
        };
        let failure = machines::session_state(TARGET, stage, answer)
            .expect_err("an unrecognised code is a failure, not a state");
        assert_eq!(
            failure.stage, stage,
            "the reported stage must be the gate that asked"
        );
        assert!(
            failure.message.contains("the session fell over mid-answer"),
            "the far side's own words survive: {}",
            failure.message
        );
    }

    // An identity that parses as JSON but not as an identity takes the same
    // route, and must carry the same stage.
    let garbled = machines::session_state(
        TARGET,
        Stage::Commit,
        Ok(json!({"session_protocol": "four"})),
    )
    .expect_err("a protocol that is not a number is not an identity");
    assert_eq!(garbled.stage, Stage::Commit);
}
