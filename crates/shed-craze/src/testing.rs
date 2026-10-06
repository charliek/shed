//! Test support (`test-support` feature): craze's own hermetic recipe as a
//! harness, the dial hooks the recipe cells drive, and a scripted hub for the
//! unit cells.
//!
//! # The recipe (craze `docs/reference/protocol.md`, "Testing a client against a real hub")
//!
//! [`Recipe`] is craze's recipe, executable from Rust exactly as craze's own
//! `tests/cli/test_client_recipe.py` runs it: the REAL hub — born by the first
//! bridge — over `craze-fake-host` registry entries, its creates spawning
//! `craze-fake-agent` as `grok`, with nothing real behind any of it and nothing
//! of the machine's own craze seen or touched.
//!
//! - **The binaries** come from `SHED_CRAZE_BIN_DIR` (`make craze-binaries`
//!   prints it; CI's `craze-binaries` action exports it): `craze`,
//!   `craze-fake-host`, `craze-fake-agent` at the pinned sha, and `craze-0.0.1`,
//!   the real v0.0.1, for the too-old cells. `craze` and `craze-fake-host` are
//!   **copied** — not linked — into a private `PATH` directory, so the command
//!   line of every craze process of the recipe's (each bridge, the fake hosts,
//!   the hub, each host the hub creates, which craze runs again by its own
//!   path) starts with that directory, which is how teardown tells them from
//!   any other process.
//! - **The environment** of every craze process is exactly the recipe's six
//!   variables and nothing inherited ([`Recipe::env`]): `HOME`, `CRAZE_HOME`,
//!   `CRAZE_RUNTIME_DIR` (short, under `/tmp`, mode 0700 — craze refuses a
//!   runtime dir under a group-writable ancestor, plan 025 Amendment A3, and a
//!   socket path over 100 bytes), `PATH` (the private directory),
//!   `CRAZE_FAKE_SCRIPT=grok-echo`, `CRAZE_FAKE_SESSION_ID={dir}`. The first
//!   bridge's environment becomes the hub's, which hands it to every host and
//!   agent, so [`Recipe::dial`] runs the private `craze bridge --hub` directly
//!   under exactly those six. [`Recipe::ladder_dial`] runs the same bridge
//!   through the production-shaped JAILED ladder (`/bin/sh -c`, rungs 1–2):
//!   the not-installed and too-old cells need the ladder's own `exit 127`, and
//!   a happy-path cell proves the ladder reaches the same hub. (`sh` itself
//!   adds `PWD` — the dial's working directory, the recipe's own root — so a
//!   cell that would let the ladder BIRTH a hub dials directly first.)
//! - **`config.toml`**: `provider = "grok"`, `host_idle_exit = "30s"`,
//!   `[agents] grok = <BIN>/craze-fake-agent`. [`Recipe::set_grok_agent`]
//!   rewrites the agent (the hub reads `config.toml` at each create).
//! - **Recorded pids, guarded teardown.** The fake hosts are this harness's
//!   own children (its stdin is how a fake host is told to go: it unlists
//!   itself at its end). The hub's pid is read from its record
//!   (`HOME/.cache/craze/hubs/*.json`) and its command line is checked to run
//!   the recipe's private copy just before any signal ([`Recipe::sigterm_hub`]).
//!   [`Recipe::teardown`] is craze's own `cleanup`: every fake host's stdin
//!   closed; SIGTERM to every process whose program is one of the recipe's
//!   copies (command line re-read just before each signal; never pid 0 or 1,
//!   never this process); a bounded wait; then the same for anything still
//!   there, the fake hosts included; then SIGKILL as the backstop; and it
//!   reports what was left. `Drop` runs the same, best effort. **Drop every
//!   source subscription before teardown** — a live one redials and births a
//!   new hub.
//!
//! # The skip rule
//!
//! [`bins`]: without `SHED_CRAZE_BIN_DIR` (or with a binary missing from it)
//! a recipe cell SKIPS with a message; with `SHED_CRAZE_REQUIRE=1` — set in
//! every CI job that builds the binaries — that skip is a FAILURE, so CI can
//! never go green on skipped craze cells.

use std::collections::VecDeque;
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use futures_util::future::BoxFuture;
use serde_json::{json, Value};
use tokio::io::{
    duplex, AsyncBufReadExt, AsyncRead, AsyncWrite, AsyncWriteExt, BufReader, DuplexStream, Lines,
    ReadHalf, WriteHalf,
};
use tokio::process::{Child, ChildStdin, Command};
use tokio::sync::{mpsc, oneshot};

use crate::conn::lock;
use crate::dial::{
    probe_process, BoxRead, BoxWrite, CrazeDial, CrazeStream, DialError, EnvPolicy, ExitWatch,
    Probe, ProcessDial,
};
use crate::source::CrazeSource;

/// Where the four binaries are (`make craze-binaries` prints the line).
pub const BIN_DIR_ENV: &str = "SHED_CRAZE_BIN_DIR";
/// `1` turns a skipped recipe cell into a failed one.
pub const REQUIRE_ENV: &str = "SHED_CRAZE_REQUIRE";

/// How long a recipe wait may take: generous next to a hub's own start (craze's
/// recipe allows the first bridge 30 s), short enough that a wedged cell fails
/// with a sentence rather than hanging.
pub const RECIPE_WAIT: Duration = Duration::from_secs(45);

/// The binaries under test.
#[derive(Debug, Clone)]
pub struct Bins {
    pub dir: PathBuf,
    pub craze: PathBuf,
    pub fake_host: PathBuf,
    pub fake_agent: PathBuf,
    /// The real craze v0.0.1 — the too-old cells' binary.
    pub craze_0_0_1: PathBuf,
}

/// The binaries, or `None` with a message when a cell must skip — a panic
/// instead when `SHED_CRAZE_REQUIRE=1`.
pub fn bins(cell: &str) -> Option<Bins> {
    let required = std::env::var(REQUIRE_ENV).is_ok_and(|v| v == "1");
    let skip = |why: String| -> Option<Bins> {
        assert!(
            !required,
            "{cell}: {REQUIRE_ENV}=1 but {why} — a craze cell may not skip here"
        );
        eprintln!("skipping {cell}: {why} (run `make craze-binaries` and export the SHED_CRAZE_BIN_DIR= line it prints)");
        None
    };
    let dir = match std::env::var_os(BIN_DIR_ENV) {
        Some(d) if !d.is_empty() => PathBuf::from(d),
        _ => return skip(format!("{BIN_DIR_ENV} is not set")),
    };
    let bins = Bins {
        craze: dir.join("craze"),
        fake_host: dir.join("craze-fake-host"),
        fake_agent: dir.join("craze-fake-agent"),
        craze_0_0_1: dir.join("craze-0.0.1"),
        dir,
    };
    for p in [
        &bins.craze,
        &bins.fake_host,
        &bins.fake_agent,
        &bins.craze_0_0_1,
    ] {
        if !is_executable(p) {
            return skip(format!("{} is missing or not executable", p.display()));
        }
    }
    Some(bins)
}

fn is_executable(p: &Path) -> bool {
    use std::os::unix::fs::PermissionsExt;
    std::fs::metadata(p).is_ok_and(|m| m.is_file() && m.permissions().mode() & 0o111 != 0)
}

/// What the recipe's private `PATH` directory holds.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum PathKind {
    /// `craze` and `craze-fake-host` at the pin — craze's recipe.
    Recipe,
    /// Nothing: craze is not installed.
    Empty,
    /// The real v0.0.1 as `craze` (and the pin's fake host): craze too old.
    TooOld,
}

/// One running fake host: its child, and its stdin — the ops channel, and its
/// end.
struct FakeHost {
    host_id: String,
    child: Child,
    stdin: Option<ChildStdin>,
}

/// craze's hermetic recipe (the module doc).
pub struct Recipe {
    root: PathBuf,
    pub home: PathBuf,
    pub craze_home: PathBuf,
    pub runtime: PathBuf,
    pub path_dir: PathBuf,
    pub work: PathBuf,
    bins: Bins,
    fake_hosts: tokio::sync::Mutex<Vec<FakeHost>>,
    torn_down: bool,
}

static NEXT_ROOT: AtomicUsize = AtomicUsize::new(0);

fn mkdir_0700(p: &Path) -> std::io::Result<()> {
    use std::os::unix::fs::PermissionsExt;
    std::fs::create_dir(p)?;
    std::fs::set_permissions(p, std::fs::Permissions::from_mode(0o700))
}

impl Recipe {
    /// craze's recipe: the private `PATH` holds the pin's `craze` and
    /// `craze-fake-host`.
    pub fn start(bins: &Bins) -> Recipe {
        Recipe::start_with(bins, PathKind::Recipe)
    }

    /// The recipe with a `PATH` of the given kind.
    pub fn start_with(bins: &Bins, kind: PathKind) -> Recipe {
        // Short and under /tmp (Amendment A3: never ~/.cache, which is 0775
        // here; a socket path must stay under 100 bytes).
        let root = loop {
            let n = NEXT_ROOT.fetch_add(1, Ordering::Relaxed);
            let candidate = PathBuf::from(format!("/tmp/shcz-{}-{n}", std::process::id()));
            match mkdir_0700(&candidate) {
                Ok(()) => break candidate,
                Err(e) if e.kind() == std::io::ErrorKind::AlreadyExists => continue,
                Err(e) => panic!("the recipe's root {}: {e}", candidate.display()),
            }
        };
        let dir = |name: &str| {
            let p = root.join(name);
            mkdir_0700(&p).unwrap_or_else(|e| panic!("{}: {e}", p.display()));
            p
        };
        let (home, craze_home, runtime, path_dir, work) =
            (dir("home"), dir("ch"), dir("run"), dir("path"), dir("work"));
        let copy = |from: &Path, name: &str| {
            std::fs::copy(from, path_dir.join(name)).unwrap_or_else(|e| {
                panic!("copying {} into the recipe's PATH: {e}", from.display())
            });
        };
        match kind {
            PathKind::Recipe => {
                copy(&bins.craze, "craze");
                copy(&bins.fake_host, "craze-fake-host");
            }
            PathKind::Empty => {}
            PathKind::TooOld => {
                copy(&bins.craze_0_0_1, "craze");
                copy(&bins.fake_host, "craze-fake-host");
            }
        }
        let recipe = Recipe {
            root,
            home,
            craze_home,
            runtime,
            path_dir,
            work,
            bins: bins.clone(),
            fake_hosts: tokio::sync::Mutex::new(Vec::new()),
            torn_down: false,
        };
        recipe.set_grok_agent(&recipe.bins.fake_agent);
        recipe
    }

    /// The recipe's six variables, and nothing else.
    pub fn env(&self) -> Vec<(String, String)> {
        let s = |p: &Path| p.to_string_lossy().into_owned();
        vec![
            ("HOME".into(), s(&self.home)),
            ("CRAZE_HOME".into(), s(&self.craze_home)),
            ("CRAZE_RUNTIME_DIR".into(), s(&self.runtime)),
            ("PATH".into(), s(&self.path_dir)),
            ("CRAZE_FAKE_SCRIPT".into(), "grok-echo".into()),
            ("CRAZE_FAKE_SESSION_ID".into(), "{dir}".into()),
        ]
    }

    /// The recipe's own `craze bridge --hub`, run by its full path under
    /// exactly [`Recipe::env`] — what births the hub.
    pub fn dial(&self) -> ProcessDial {
        ProcessDial::new(
            vec![
                self.path_dir.join("craze").to_string_lossy().into_owned(),
                "bridge".into(),
                "--hub".into(),
            ],
            EnvPolicy::Exactly(self.env()),
        )
        .with_cwd(self.root.clone())
    }

    /// The same bridge through the production-shaped JAILED ladder
    /// (`/bin/sh -c`, `$HOME/.local/bin` then `command -v` on the private
    /// `PATH`).
    pub fn ladder_dial(&self) -> ProcessDial {
        ProcessDial::bridge_hub_jailed(EnvPolicy::Exactly(self.env())).with_cwd(self.root.clone())
    }

    /// A source on [`Recipe::dial`].
    pub fn source(&self) -> CrazeSource {
        self.source_on(Arc::new(self.dial()))
    }

    /// A source on any dial.
    pub fn source_on(&self, dial: Arc<dyn CrazeDial>) -> CrazeSource {
        CrazeSource::new(dial, "shed-craze-recipe")
    }

    /// The find-only probe (`providers --hub --json`) through the jailed ladder.
    pub async fn probe(&self) -> Probe {
        probe_process(
            shed_core::craze::providers_hub_argv_jailed(),
            EnvPolicy::Exactly(self.env()),
        )
        .await
    }

    /// Point `[agents].grok` at `agent` (the hub reads `config.toml` at every
    /// create).
    pub fn set_grok_agent(&self, agent: &Path) {
        let config = format!(
            "provider = \"grok\"\nhost_idle_exit = \"30s\"\n\n[agents]\ngrok = \"{}\"\n",
            agent.display()
        );
        std::fs::write(self.config_path(), config).unwrap_or_else(|e| panic!("config.toml: {e}"));
    }

    /// The recipe's own directory under `/tmp`: every dir and file of the
    /// recipe's is inside it, and teardown removes it.
    pub fn root(&self) -> &Path {
        &self.root
    }

    /// Everything the recipe's hubs have logged (`HOME/.cache/craze/
    /// host-logs/hub-<namespace>.log`, craze's `hub.LogPath`), concatenated.
    pub fn hub_log(&self) -> String {
        let dir = self.home.join(".cache/craze/host-logs");
        let mut names: Vec<PathBuf> = std::fs::read_dir(dir)
            .into_iter()
            .flatten()
            .flatten()
            .map(|e| e.path())
            .filter(|p| {
                p.file_name()
                    .is_some_and(|n| n.to_string_lossy().starts_with("hub-"))
            })
            .collect();
        names.sort();
        names
            .iter()
            .filter_map(|p| std::fs::read_to_string(p).ok())
            .collect()
    }

    /// The `config.toml` path.
    pub fn config_path(&self) -> PathBuf {
        self.craze_home.join("config.toml")
    }

    /// An agent that dies at its start with two lines on stderr, as cursor
    /// does on a locked keychain: the fake agent's `exit-two-lines`, behind a
    /// two-line wrapper (`[agents]` names one binary, no arguments).
    pub fn exit_two_lines_agent(&self) -> PathBuf {
        use std::os::unix::fs::PermissionsExt;
        let p = self.root.join("exit-two-lines-agent");
        std::fs::write(
            &p,
            format!(
                "#!/bin/sh\nexec '{}' -script exit-two-lines \"$@\"\n",
                self.bins.fake_agent.display()
            ),
        )
        .unwrap_or_else(|e| panic!("{}: {e}", p.display()));
        std::fs::set_permissions(&p, std::fs::Permissions::from_mode(0o755))
            .unwrap_or_else(|e| panic!("{}: {e}", p.display()));
        p
    }

    /// Start a fake host listed in the registry under `HOME` as `host_id`
    /// running `session_id`, and wait for its ready line (printed once its
    /// registry entry is written). Its stdin stays open: [`Recipe::op`] writes
    /// ops to it, and its end is how the fake host is told to go.
    pub async fn fake_host(&self, host_id: &str, session_id: &str) -> Value {
        let mut cmd = Command::new(self.path_dir.join("craze-fake-host"));
        cmd.args(["--registry"])
            .arg(&self.home)
            .args(["--host-id", host_id, "--session-id", session_id])
            .env_clear()
            .envs(self.env())
            .current_dir(&self.root)
            .stdin(std::process::Stdio::piped())
            .stdout(std::process::Stdio::piped())
            .stderr(std::process::Stdio::null())
            .kill_on_drop(true);
        let mut child = cmd
            .spawn()
            .unwrap_or_else(|e| panic!("craze-fake-host: {e}"));
        let stdout = child.stdout.take().expect("piped stdout");
        let stdin = child.stdin.take();
        // Filed before anything waits on it, so a ready line that never comes
        // still leaves it to be ended.
        self.fake_hosts.lock().await.push(FakeHost {
            host_id: host_id.to_string(),
            child,
            stdin,
        });
        let mut lines = BufReader::new(stdout).lines();
        let line = tokio::time::timeout(RECIPE_WAIT, lines.next_line())
            .await
            .unwrap_or_else(|_| panic!("craze-fake-host {host_id}: no ready line"))
            .ok()
            .flatten()
            .unwrap_or_else(|| panic!("craze-fake-host {host_id} exited before its ready line"));
        serde_json::from_str(&line).unwrap_or_else(|e| panic!("a ready line {line:?}: {e}"))
    }

    /// Send one op (craze's fixture `op` object, e.g. `{"name":"text","text":"hi"}`)
    /// to the fake host `host_id`.
    pub async fn op(&self, host_id: &str, op: &Value) {
        let mut hosts = self.fake_hosts.lock().await;
        let host = hosts
            .iter_mut()
            .find(|h| h.host_id == host_id)
            .unwrap_or_else(|| panic!("no fake host {host_id}"));
        let stdin = host.stdin.as_mut().expect("the fake host's stdin is open");
        let mut line = serde_json::to_vec(op).expect("an op encodes");
        line.push(b'\n');
        stdin
            .write_all(&line)
            .await
            .expect("the op reached the fake host");
        stdin.flush().await.expect("the op reached the fake host");
    }

    /// The host ids listed in the registry right now.
    pub fn registered_hosts(&self) -> Vec<String> {
        let dir = self.home.join(".cache/craze/hosts");
        let mut ids: Vec<String> = std::fs::read_dir(dir)
            .into_iter()
            .flatten()
            .flatten()
            .filter_map(|e| {
                let name = e.file_name().to_string_lossy().into_owned();
                name.strip_suffix(".json").map(str::to_string)
            })
            .collect();
        ids.sort();
        ids
    }

    /// The pids the hub records (`HOME/.cache/craze/hubs/*.json`) name, each
    /// checked to be running the recipe's own copy of craze.
    pub fn hub_pids(&self) -> Vec<u32> {
        let dir = self.home.join(".cache/craze/hubs");
        std::fs::read_dir(dir)
            .into_iter()
            .flatten()
            .flatten()
            .filter(|e| e.file_name().to_string_lossy().ends_with(".json"))
            .filter_map(|e| std::fs::read(e.path()).ok())
            .filter_map(|b| serde_json::from_slice::<Value>(&b).ok())
            .filter_map(|v| v.get("pid").and_then(Value::as_u64))
            .filter_map(|p| u32::try_from(p).ok())
            .filter(|p| self.runs_ours(*p))
            .collect()
    }

    /// SIGTERM the recipe's hub — the pid its record names, its command line
    /// checked just before. Returns the pid signalled.
    pub fn sigterm_hub(&self) -> u32 {
        let pids = self.hub_pids();
        let [pid] = pids[..] else {
            panic!("expected exactly one recipe hub, found {pids:?}");
        };
        assert!(self.signal(pid, "TERM"), "the hub {pid} was signalled");
        pid
    }

    /// Whether `pid` runs one of the recipe's private copies.
    fn runs_ours(&self, pid: u32) -> bool {
        command_line(pid).is_some_and(|args| self.runs_from_path_dir(&args, ""))
    }

    /// Whether a command line's program is `program` (`""`: any program) in
    /// the recipe's private `PATH` directory. On macOS `/tmp` resolves to
    /// `/private/tmp` (a process that re-runs itself by its resolved path
    /// shows the latter), so that prefix is read as absent.
    fn runs_from_path_dir(&self, args: &str, program: &str) -> bool {
        let bare = args.strip_prefix("/private").unwrap_or(args);
        bare.starts_with(&format!("{}/{program}", self.path_dir.display()))
    }

    /// Signal `pid` if, read again just now, it runs the recipe's copy —
    /// never pid 0 or 1, never this process.
    fn signal(&self, pid: u32, sig: &str) -> bool {
        if pid <= 1 || pid == std::process::id() || !self.runs_ours(pid) {
            return false;
        }
        std::process::Command::new("kill")
            .arg(format!("-{sig}"))
            .arg(pid.to_string())
            .status()
            .is_ok_and(|s| s.success())
    }

    /// Every live process of the recipe's: `(pid, command line)`.
    pub fn processes(&self) -> Vec<(u32, String)> {
        let Ok(out) = std::process::Command::new("ps")
            .args(["-A", "-ww", "-o", "pid=", "-o", "args="])
            .output()
        else {
            return Vec::new();
        };
        String::from_utf8_lossy(&out.stdout)
            .lines()
            .filter_map(|l| {
                let l = l.trim_start();
                let (pid, args) = l.split_once(' ')?;
                let args = args.trim_start();
                self.runs_from_path_dir(args, "")
                    .then(|| Some((pid.parse().ok()?, args.to_string())))
                    .flatten()
            })
            .collect()
    }

    fn is_fake_host(&self, args: &str) -> bool {
        self.runs_from_path_dir(args, "craze-fake-host")
    }

    /// What is still here: processes and registry or hub records.
    fn left(&self) -> Vec<String> {
        let mut left: Vec<String> = self
            .processes()
            .into_iter()
            .map(|(p, a)| format!("process {p}: {a}"))
            .collect();
        for sub in [".cache/craze/hosts", ".cache/craze/hubs"] {
            for e in std::fs::read_dir(self.home.join(sub))
                .into_iter()
                .flatten()
                .flatten()
            {
                if e.file_name().to_string_lossy().ends_with(".json") {
                    left.push(format!("file {}", e.path().display()));
                }
            }
        }
        left
    }

    /// craze's `cleanup` (the module doc). `Err` lists what was left.
    ///
    /// The recipe counts as torn down only once every step has run: a teardown
    /// cut short — cancelled, or timed out by its caller — leaves `Drop` to do
    /// the cleanup it did not finish.
    pub async fn teardown(mut self) -> Result<(), String> {
        // 1. Every fake host's stdin closed: each unlists itself and exits.
        {
            let mut hosts = self.fake_hosts.lock().await;
            for h in hosts.iter_mut() {
                h.stdin.take();
            }
        }
        // 2. SIGTERM each but the fake hosts — the bridges, the hosts the hub
        //    created (their stop), the hub (its teardown removes its record) —
        //    and wait, bounded, for every one of them to go.
        for (pid, args) in self.processes() {
            if !self.is_fake_host(&args) {
                self.signal(pid, "TERM");
            }
        }
        let deadline = tokio::time::Instant::now() + Duration::from_secs(10);
        while tokio::time::Instant::now() < deadline && !self.processes().is_empty() {
            tokio::time::sleep(Duration::from_millis(100)).await;
        }
        // 3. Anything still running, the fake hosts included.
        for (pid, _) in self.processes() {
            self.signal(pid, "TERM");
        }
        let deadline = tokio::time::Instant::now() + Duration::from_secs(10);
        while tokio::time::Instant::now() < deadline && !self.left().is_empty() {
            tokio::time::sleep(Duration::from_millis(100)).await;
        }
        let left = self.left();
        // 4. The backstop.
        for (pid, _) in self.processes() {
            self.signal(pid, "KILL");
        }
        {
            let mut hosts = self.fake_hosts.lock().await;
            for h in hosts.iter_mut() {
                let _ = h.child.start_kill();
            }
        }
        let _ = std::fs::remove_dir_all(&self.root);
        self.torn_down = true;
        if left.is_empty() {
            Ok(())
        } else {
            Err(format!("the recipe's teardown left:\n{}", left.join("\n")))
        }
    }
}

impl Drop for Recipe {
    /// The backstop for a cell that panicked before its teardown: the same
    /// signals, synchronously and best effort.
    fn drop(&mut self) {
        if self.torn_down {
            return;
        }
        for (pid, _) in self.processes() {
            self.signal(pid, "TERM");
        }
        let deadline = std::time::Instant::now() + Duration::from_secs(5);
        while std::time::Instant::now() < deadline && !self.processes().is_empty() {
            std::thread::sleep(Duration::from_millis(100));
        }
        for (pid, _) in self.processes() {
            self.signal(pid, "KILL");
        }
        let _ = std::fs::remove_dir_all(&self.root);
    }
}

/// `pid`'s whole command line, or `None` when it cannot be read.
fn command_line(pid: u32) -> Option<String> {
    let out = std::process::Command::new("ps")
        .args(["-ww", "-o", "args=", "-p", &pid.to_string()])
        .output()
        .ok()?;
    let s = String::from_utf8_lossy(&out.stdout).trim().to_string();
    (!s.is_empty()).then_some(s)
}

// ---- dial hooks ----

/// A dial around another that counts its dials and can hold reads, and can cut
/// one connection off right after a given request is written.
pub struct HookDial {
    inner: Arc<dyn CrazeDial>,
    gate: Arc<Gate>,
    armed: Mutex<Option<SeverArm>>,
    dials: AtomicUsize,
}

/// Where an armed cut leaves the connection it marked: its own read gate,
/// and the process behind it.
type SeverSlot = Arc<Mutex<Option<(Arc<Gate>, Option<ExitWatch>)>>>;

struct SeverArm {
    marker: Vec<u8>,
    written: oneshot::Sender<()>,
    handle: SeverSlot,
}

/// One armed cut: [`SeverHandle::written`] resolves once the marked request is
/// written (its connection's reads are held from that instant, so no answer
/// can be read); [`SeverHandle::sever`] then kills the process behind the
/// connection and makes its reads end — a transport that dropped before the
/// answer, whatever was buffered.
pub struct SeverHandle {
    written: Option<oneshot::Receiver<()>>,
    handle: SeverSlot,
}

impl SeverHandle {
    pub async fn written(&mut self, within: Duration) {
        let rx = self.written.take().expect("written() is awaited once");
        tokio::time::timeout(within, rx)
            .await
            .expect("the marked request was written in time")
            .expect("the marked request was written");
    }

    pub fn sever(&self) {
        let parts = lock(&self.handle).take();
        let (gate, exit) = parts.expect("the marked connection exists");
        gate.sever();
        if let Some(exit) = exit {
            exit.kill();
        }
    }
}

impl HookDial {
    pub fn new(inner: Arc<dyn CrazeDial>) -> Arc<HookDial> {
        Arc::new(HookDial {
            inner,
            gate: Gate::new(),
            armed: Mutex::new(None),
            dials: AtomicUsize::new(0),
        })
    }

    /// How many connections were dialled.
    pub fn dials(&self) -> usize {
        self.dials.load(Ordering::SeqCst)
    }

    /// Stop every connection's reads (a consumer that stopped reading).
    pub fn hold_reads(&self) {
        self.gate.hold();
    }

    /// Read again.
    pub fn release_reads(&self) {
        self.gate.release();
    }

    /// Arm a cut on the NEXT connection that writes `marker`.
    pub fn sever_after(&self, marker: &str) -> SeverHandle {
        let (tx, rx) = oneshot::channel();
        let handle = Arc::new(Mutex::new(None));
        *lock(&self.armed) = Some(SeverArm {
            marker: marker.as_bytes().to_vec(),
            written: tx,
            handle: Arc::clone(&handle),
        });
        SeverHandle {
            written: Some(rx),
            handle,
        }
    }
}

impl CrazeDial for HookDial {
    fn dial(&self) -> BoxFuture<'static, Result<CrazeStream, DialError>> {
        self.dials.fetch_add(1, Ordering::SeqCst);
        let inner = self.inner.dial();
        let gate = Arc::clone(&self.gate);
        let arm = lock(&self.armed).take();
        Box::pin(async move {
            let (r, w, tail, exit) = inner.await?.into_parts();
            let r: BoxRead = Box::new(GatedRead::new(r, gate));
            let (r, w): (BoxRead, BoxWrite) = match arm {
                None => (r, w),
                Some(arm) => {
                    let own = Gate::new();
                    *lock(&arm.handle) = Some((Arc::clone(&own), exit.clone()));
                    let r: BoxRead = Box::new(GatedRead::new(r, Arc::clone(&own)));
                    let w: BoxWrite = Box::new(MarkWrite {
                        inner: w,
                        marker: arm.marker,
                        seen: Vec::new(),
                        marked: false,
                        gate: own,
                        written: Some(arm.written),
                    });
                    (r, w)
                }
            };
            let mut s = CrazeStream::new(r, w);
            if let Some(t) = tail {
                s = s.with_stderr_tail(t);
            }
            if let Some(e) = exit {
                s = s.with_exit(e);
            }
            Ok(s)
        })
    }
}

/// A write half that holds its connection's reads, and says so, the moment a
/// line carrying `marker` has been written.
struct MarkWrite {
    inner: BoxWrite,
    marker: Vec<u8>,
    /// What was written until the marker was.
    seen: Vec<u8>,
    /// The marker has been written (and the reads held).
    marked: bool,
    gate: Arc<Gate>,
    written: Option<oneshot::Sender<()>>,
}

impl AsyncWrite for MarkWrite {
    fn poll_write(
        mut self: std::pin::Pin<&mut Self>,
        cx: &mut std::task::Context<'_>,
        buf: &[u8],
    ) -> std::task::Poll<std::io::Result<usize>> {
        let this = &mut *self;
        let poll = std::pin::Pin::new(&mut this.inner).poll_write(cx, buf);
        if let std::task::Poll::Ready(Ok(n)) = &poll {
            if !this.marked {
                this.seen.extend_from_slice(&buf[..*n]);
                if this
                    .seen
                    .windows(this.marker.len())
                    .any(|w| w == this.marker)
                {
                    this.marked = true;
                    this.gate.hold();
                }
            }
        }
        poll
    }

    fn poll_flush(
        mut self: std::pin::Pin<&mut Self>,
        cx: &mut std::task::Context<'_>,
    ) -> std::task::Poll<std::io::Result<()>> {
        let this = &mut *self;
        let poll = std::pin::Pin::new(&mut this.inner).poll_flush(cx);
        if poll.is_ready() && this.marked {
            if let Some(tx) = this.written.take() {
                let _ = tx.send(());
            }
        }
        poll
    }

    fn poll_shutdown(
        mut self: std::pin::Pin<&mut Self>,
        cx: &mut std::task::Context<'_>,
    ) -> std::task::Poll<std::io::Result<()>> {
        std::pin::Pin::new(&mut self.inner).poll_shutdown(cx)
    }
}

// ---- a reader that can be held ----

/// A read half whose reads can be held and released, or cut off: while held it
/// reads nothing (so the peer's writes back up exactly as a slow consumer's
/// do); once severed it reads EOF forever, whatever is buffered behind it — a
/// transport that dropped. What [`HookDial`] wraps every connection's reads in.
pub struct GatedRead {
    inner: BoxRead,
    gate: Arc<Gate>,
}

/// The shared switch behind one or more [`GatedRead`]s.
#[derive(Default)]
pub struct Gate {
    state: Mutex<GateState>,
}

#[derive(Default)]
struct GateState {
    held: bool,
    severed: bool,
    wakers: Vec<std::task::Waker>,
}

impl Gate {
    /// A new, open gate.
    pub fn new() -> Arc<Gate> {
        Arc::new(Gate::default())
    }

    /// Stop reading.
    pub fn hold(&self) {
        lock(&self.state).held = true;
    }

    /// Read again.
    pub fn release(&self) {
        self.wake_after(|s| s.held = false);
    }

    /// Read EOF from now on.
    pub fn sever(&self) {
        self.wake_after(|s| s.severed = true);
    }

    /// Change the state, then wake every read parked on it.
    fn wake_after(&self, change: impl FnOnce(&mut GateState)) {
        let wakers = {
            let mut s = lock(&self.state);
            change(&mut s);
            std::mem::take(&mut s.wakers)
        };
        wakers.into_iter().for_each(std::task::Waker::wake);
    }
}

impl GatedRead {
    pub fn new(inner: BoxRead, gate: Arc<Gate>) -> GatedRead {
        GatedRead { inner, gate }
    }
}

impl AsyncRead for GatedRead {
    fn poll_read(
        mut self: std::pin::Pin<&mut Self>,
        cx: &mut std::task::Context<'_>,
        buf: &mut tokio::io::ReadBuf<'_>,
    ) -> std::task::Poll<std::io::Result<()>> {
        {
            let mut s = lock(&self.gate.state);
            if s.severed {
                return std::task::Poll::Ready(Ok(()));
            }
            if s.held {
                s.wakers.push(cx.waker().clone());
                return std::task::Poll::Pending;
            }
        }
        std::pin::Pin::new(&mut self.inner).poll_read(cx, buf)
    }
}

// ---- a scripted hub, for the unit cells ----

/// A dial whose every connection is an in-memory duplex: the test plays craze
/// on the other end ([`HubEnd`]), line by line, deterministically.
pub struct ScriptedDial {
    conns: mpsc::UnboundedSender<HubEnd>,
    failures: Mutex<VecDeque<DialError>>,
    dials: AtomicUsize,
    /// Each duplex's buffer, each way.
    capacity: AtomicUsize,
}

/// craze's end of one scripted connection.
pub struct HubEnd {
    lines: Lines<BufReader<ReadHalf<DuplexStream>>>,
    writer: WriteHalf<DuplexStream>,
}

/// How long a scripted wait may take.
pub const SCRIPT_WAIT: Duration = Duration::from_secs(5);

impl ScriptedDial {
    /// The dial, and the connections it makes, in order.
    pub fn new() -> (Arc<ScriptedDial>, mpsc::UnboundedReceiver<HubEnd>) {
        let (tx, rx) = mpsc::unbounded_channel();
        (
            Arc::new(ScriptedDial {
                conns: tx,
                failures: Mutex::new(VecDeque::new()),
                dials: AtomicUsize::new(0),
                capacity: AtomicUsize::new(4 << 20),
            }),
            rx,
        )
    }

    /// Fail the next dial with `e` (queued: several calls fail several dials).
    pub fn fail_next(&self, e: DialError) {
        lock(&self.failures).push_back(e);
    }

    /// How many dials were attempted, failed ones included.
    pub fn dials(&self) -> usize {
        self.dials.load(Ordering::SeqCst)
    }

    /// Give the NEXT dials' duplexes `bytes` of buffer each way (default 4
    /// MiB): a small one makes a write block once craze's end stops reading.
    pub fn set_capacity(&self, bytes: usize) {
        self.capacity.store(bytes, Ordering::SeqCst);
    }
}

impl CrazeDial for ScriptedDial {
    fn dial(&self) -> BoxFuture<'static, Result<CrazeStream, DialError>> {
        self.dials.fetch_add(1, Ordering::SeqCst);
        let failed = lock(&self.failures).pop_front();
        let result = match failed {
            Some(e) => Err(e),
            None => {
                let (client, server) = duplex(self.capacity.load(Ordering::SeqCst));
                let (cr, cw) = tokio::io::split(client);
                let (sr, sw) = tokio::io::split(server);
                let _ = self.conns.send(HubEnd {
                    lines: BufReader::new(sr).lines(),
                    writer: sw,
                });
                Ok(CrazeStream::new(cr, cw))
            }
        };
        Box::pin(async move { result })
    }
}

/// A full hub `hello` result with these connection capabilities.
pub fn hub_hello_result(epoch: &str, capabilities: Value) -> Value {
    let mut v = json!({"protocol": 1, "endpoint": {"kind": "hub", "hostId": epoch, "crazeVersion": "test", "pid": 5150},
                       "codecs": {"event": 1, "snapshot": 1},
                       "limits": {"inboundLine": 4194304, "outboundLine": 16777216}});
    v["capabilities"] = capabilities;
    v
}

/// Every capability a v0.1.0 hub states.
pub fn full_hub_capabilities() -> Value {
    json!({"rosterSubscribe": true, "sessionCreate": true, "multiplex": false, "connect": true,
           "snapshot": false, "attachWhenNow": false, "createOptions": true})
}

impl HubEnd {
    /// The next request the client wrote, or `None` once it closed.
    pub async fn recv(&mut self) -> Option<Value> {
        let line = tokio::time::timeout(SCRIPT_WAIT, self.lines.next_line())
            .await
            .expect("the client wrote a request in time")
            .ok()
            .flatten()?;
        Some(serde_json::from_str(&line).unwrap_or_else(|e| panic!("a request {line:?}: {e}")))
    }

    /// The next request, which must be `method`.
    pub async fn expect(&mut self, method: &str) -> Value {
        let req = self
            .recv()
            .await
            .unwrap_or_else(|| panic!("the client closed before {method}"));
        assert_eq!(req["method"], method, "the next request: {req}");
        req
    }

    /// Write one line.
    pub async fn send(&mut self, msg: &Value) {
        let mut line = serde_json::to_vec(msg).expect("a line encodes");
        line.push(b'\n');
        // A client that already hung up is the test's business, not a panic.
        let _ = self.writer.write_all(&line).await;
        let _ = self.writer.flush().await;
    }

    /// Answer `req` with `result`.
    pub async fn reply(&mut self, req: &Value, result: Value) {
        self.send(&json!({"jsonrpc": "2.0", "id": req["id"], "result": result}))
            .await;
    }

    /// Refuse `req` with a craze-level error.
    pub async fn refuse(&mut self, req: &Value, code: &str, reason: &str, extra: Value) {
        let mut data = json!({"code": code, "reason": reason});
        if let (Value::Object(d), Value::Object(x)) = (&mut data, extra) {
            d.extend(x);
        }
        self.send(&json!({"jsonrpc": "2.0", "id": req["id"],
                          "error": {"code": -32000, "message": format!("{code}/{reason}"), "data": data}}))
            .await;
    }

    /// Read `hello` and answer as a hub with `capabilities`; returns the request.
    pub async fn hello(&mut self, epoch: &str, capabilities: Value) -> Value {
        let req = self.expect("hello").await;
        self.reply(&req, hub_hello_result(epoch, capabilities))
            .await;
        req
    }

    /// Read `sessions.subscribe` and answer with `rows`.
    pub async fn subscribed(&mut self, sub: &str, epoch: &str, rows: Value) -> Value {
        let req = self.expect("sessions.subscribe").await;
        self.reply(
            &req,
            json!({"subscription": sub, "epoch": epoch, "cursor": 1, "sessions": rows}),
        )
        .await;
        req
    }

    /// A `roster` notification.
    pub async fn roster(&mut self, sub: &str, epoch: &str, upserts: Value, removes: Value) {
        self.send(&json!({"jsonrpc": "2.0", "method": "roster",
                          "params": {"subscription": sub, "epoch": epoch, "cursor": 2,
                                     "upserts": upserts, "removes": removes}}))
            .await;
    }

    /// A `reset` notification.
    pub async fn reset(&mut self, sub: &str, reason: &str) {
        self.send(&json!({"jsonrpc": "2.0", "method": "reset",
                          "params": {"subscription": sub, "reason": reason}}))
            .await;
    }

    /// Close the connection (craze's end of it).
    pub async fn close(mut self) {
        let _ = self.writer.shutdown().await;
    }
}

/// A roster row as the hub writes one, for scripted cells.
pub fn roster_row(host_id: &str, session_id: &str, workspace: &str, row: Value) -> Value {
    let mut v = json!({"hostId": host_id, "sessionId": session_id,
        "host": {"pid": 4242, "crazeVersion": "test", "protocol": 1, "provider": "grok",
                 "workspace": workspace, "startedAt": "2026-01-01T00:00:00Z", "ready": true},
        "status": "reachable", "approximate": false});
    if !row.is_null() {
        v["row"] = row;
    }
    v
}
