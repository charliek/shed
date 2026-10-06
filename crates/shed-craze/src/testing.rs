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
    /// Directories after the private one on `PATH` (the live recording only).
    path_after: Vec<String>,
    /// Variables beyond the six (the live recording only).
    extra_env: Vec<(String, String)>,
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
            path_after: Vec::new(),
            extra_env: Vec::new(),
            torn_down: false,
        };
        recipe.set_grok_agent(&recipe.bins.fake_agent);
        recipe
    }

    /// The recipe's six variables, and nothing else (but what
    /// [`Recipe::with_path_after`]/[`Recipe::with_env`] added, which no recipe
    /// cell does).
    pub fn env(&self) -> Vec<(String, String)> {
        let s = |p: &Path| p.to_string_lossy().into_owned();
        let mut path = s(&self.path_dir);
        for dir in &self.path_after {
            path = format!("{path}:{dir}");
        }
        let mut env: Vec<(String, String)> = vec![
            ("HOME".into(), s(&self.home)),
            ("CRAZE_HOME".into(), s(&self.craze_home)),
            ("CRAZE_RUNTIME_DIR".into(), s(&self.runtime)),
            ("PATH".into(), path),
            ("CRAZE_FAKE_SCRIPT".into(), "grok-echo".into()),
            ("CRAZE_FAKE_SESSION_ID".into(), "{dir}".into()),
        ];
        env.extend(self.extra_env.iter().cloned());
        env
    }

    /// One more variable for every craze process (the live recording only:
    /// the recipe's own cells run under exactly the six).
    pub fn with_env(mut self, key: &str, value: &str) -> Recipe {
        self.extra_env.push((key.to_string(), value.to_string()));
        self
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
        self.set_agents(&[("grok", agent)]);
    }

    /// Point each `[agents].<provider>` named at its agent, `grok` staying the
    /// default provider — the settings cells run the fake agent's `permodel`
    /// script (cursor's per-model catalogs, on cursor's own wire) as
    /// `cursor`, which `[agents]` makes ready on any machine.
    pub fn set_agents(&self, agents: &[(&str, &Path)]) {
        let mut config = "provider = \"grok\"\nhost_idle_exit = \"30s\"\n\n[agents]\n".to_string();
        for (provider, agent) in agents {
            config.push_str(&format!("{provider} = \"{}\"\n", agent.display()));
        }
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
        self.script_agent("exit-two-lines")
    }

    /// The fake agent running `script` (`hang`, `grok-ask`, …), behind a
    /// two-line wrapper — `[agents]` names one binary, no arguments, and the
    /// flag wins over the recipe's `CRAZE_FAKE_SCRIPT`.
    pub fn script_agent(&self, script: &str) -> PathBuf {
        self.script_agent_with(script, &[])
    }

    /// [`Recipe::script_agent`] with the fake agent's own knobs set in its
    /// wrapper (the hub's environment is fixed at its birth, so a knob for one
    /// create goes here): `CRAZE_FAKE_DUMP_CALLS` (a file the agent appends
    /// every message it reads to — `session/set_config_option <id>=<value>`
    /// for a set, so a cell can count what reached the agent) and
    /// `CRAZE_FAKE_SET_GATE` among them. Values are single-quoted.
    pub fn script_agent_with(&self, script: &str, env: &[(&str, &str)]) -> PathBuf {
        use std::os::unix::fs::PermissionsExt;
        let p = self.root.join(format!("{script}-agent"));
        let mut exports = String::new();
        for (k, v) in env {
            exports.push_str(&format!("export {k}='{v}'\n"));
        }
        std::fs::write(
            &p,
            format!(
                "#!/bin/sh\n{exports}exec '{}' -script {script} \"$@\"\n",
                self.bins.fake_agent.display()
            ),
        )
        .unwrap_or_else(|e| panic!("{}: {e}", p.display()));
        std::fs::set_permissions(&p, std::fs::Permissions::from_mode(0o755))
            .unwrap_or_else(|e| panic!("{}: {e}", p.display()));
        p
    }

    /// Replace `config.toml` whole (the live recording's own config).
    pub fn write_config(&self, text: &str) {
        std::fs::write(self.config_path(), text).unwrap_or_else(|e| panic!("config.toml: {e}"));
    }

    /// Run every craze process with `dirs` after the private directory on
    /// its `PATH` (the live recording: a real agent's tools need a shell's
    /// usual programs). The recipe's own cells never do.
    pub fn with_path_after(mut self, dirs: &[&str]) -> Recipe {
        self.path_after = dirs.iter().map(|d| d.to_string()).collect();
        self
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
        let mut child = spawn_fresh(&mut cmd, "craze-fake-host").await;
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

/// Spawn a binary [`Recipe::new`] has only just copied, with a bounded
/// `ETXTBSY` retry. The fd table is process-wide: a sibling cell's spawn that
/// forks while this recipe's copy still holds its write fd carries that fd
/// into its child until the child execs, and an exec of the copy in that
/// window fails "Text file busy" — the classic fork/exec race, the same one
/// `shed-broker`'s `run_shim` retries. Anything but `ETXTBSY` is returned at
/// once; a busy file that stays busy fails loudly, because that is no longer
/// the transient race.
async fn spawn_fresh(cmd: &mut Command, what: &str) -> Child {
    let mut delay = Duration::from_millis(10);
    for _ in 0..10 {
        match cmd.spawn() {
            Ok(child) => return child,
            Err(e) if e.kind() == std::io::ErrorKind::ExecutableFileBusy => {
                tokio::time::sleep(delay).await;
                delay = (delay * 2).min(Duration::from_millis(160));
            }
            Err(e) => panic!("{what}: {e}"),
        }
    }
    panic!("{what}: persistently busy (ETXTBSY) — not the transient fork/exec race")
}

// ---- dial hooks ----

/// Every connection a [`HookDial`] made: its own read gate, and its process.
type LiveConns = Arc<Mutex<Vec<(Arc<Gate>, Option<ExitWatch>)>>>;

/// A dial around another that counts its dials and can hold reads, can cut
/// one connection off right after a given request is written, can cut every
/// live connection off at once ([`HookDial::sever_live`] — a lane's bridge
/// killed mid-stream), and can hold new dials ([`HookDial::hold_dials`] — a
/// lane kept disconnected while the far side changes under it).
pub struct HookDial {
    inner: Arc<dyn CrazeDial>,
    gate: Arc<Gate>,
    armed: Mutex<Option<SeverArm>>,
    dials: AtomicUsize,
    /// Every connection dialled so far: its own read gate, and its process.
    live: LiveConns,
    /// `true` while new dials wait.
    held: tokio::sync::watch::Sender<bool>,
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
            live: Arc::default(),
            held: tokio::sync::watch::channel(false).0,
        })
    }

    /// Cut every connection dialled so far: its process killed, its reads at
    /// EOF from now on, whatever was buffered — a transport that dropped.
    /// Returns how many were cut.
    pub fn sever_live(&self) -> usize {
        let conns = std::mem::take(&mut *lock(&self.live));
        for (gate, exit) in &conns {
            gate.sever();
            if let Some(exit) = exit {
                exit.kill();
            }
        }
        conns.len()
    }

    /// Hold every NEW dial until [`HookDial::release_dials`].
    pub fn hold_dials(&self) {
        self.held.send_replace(true);
    }

    /// Let held dials (and later ones) through.
    pub fn release_dials(&self) {
        self.held.send_replace(false);
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
        let inner = Arc::clone(&self.inner);
        let gate = Arc::clone(&self.gate);
        let arm = lock(&self.armed).take();
        let live = Arc::clone(&self.live);
        let mut held = self.held.subscribe();
        Box::pin(async move {
            let _ = held.wait_for(|h| !*h).await;
            let (r, w, tail, exit) = inner.dial().await?.into_parts();
            let r: BoxRead = Box::new(GatedRead::new(r, gate));
            let mine = Gate::new();
            lock(&live).push((Arc::clone(&mine), exit.clone()));
            let r: BoxRead = Box::new(GatedRead::new(r, mine));
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
            Ok(CrazeStream::from_parts(r, w, tail, exit))
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

    /// A `reset` notification — a roster's, or an attachment's (the same
    /// line).
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

// ---- a scripted host behind the hub, for the lane's unit cells ----

/// One open ask as `asks.get` carries it: `kind`'s `payload` as its body,
/// opened at `opened_at`.
pub fn ask_record(kind: &str, payload: &Value, opened_at: &str) -> Value {
    json!({"id": payload["id"], "kind": kind, "status": "open",
           "body": { kind: payload }, "openedAt": opened_at})
}

/// WIRE/01's session capabilities, with `stop` as given.
pub fn session_caps(stop: bool) -> Value {
    json!({"interject": false, "subagentCancel": false, "subagentBackground": false, "modes": true,
           "effort": true, "fastToggle": true, "subagentRows": true, "subagentTranscript": false,
           "todos": true, "askCards": true, "planCards": true, "parameterizedPicker": true,
           "cancel": true, "approvals": true, "historyCursor": true, "stop": stop})
}

/// A session info document (WIRE/01's shape) for `host_id` serving
/// `session_id` in `incarnation`.
pub fn session_info(host_id: &str, session_id: &str, incarnation: &str, caps: Value) -> Value {
    let mut info = json!({"sessionId": session_id, "providerSessionId": "stub-session-1", "incarnation": incarnation,
           "hostId": host_id, "workspace": "/work", "provider": {"name": "grok", "label": "Grok"},
           "catalogs": {"models": [{"id": "grok", "name": "Grok"}, {"id": "fast", "name": "Fast"}],
                        "modes": [{"id": "agent", "name": "Agent", "description": "Full agent capabilities with tool access"}]},
           "retryHorizon": {"commands": 1024, "ageMs": 600000}});
    info["capabilities"] = caps;
    info
}

/// A host's `sessions.list` row: the info document and the live facts
/// (`facts` merged over `{title: "", activity: "idle", foreignTurn: false,
/// pendingAsks: 0}`).
pub fn host_session_row(info: &Value, facts: Value) -> Value {
    let mut row = info.clone();
    for (k, v) in [
        ("title", json!("")),
        ("activity", json!("idle")),
        ("foreignTurn", json!(false)),
        ("pendingAsks", json!(0)),
    ] {
        row[k] = v;
    }
    if let Value::Object(facts) = facts {
        for (k, v) in facts {
            row[k.as_str()] = v;
        }
    }
    row
}

/// A snapshot (codec 1) cut at `incarnation:seq`, with WIRE/01's settings and
/// `main` as given.
pub fn snapshot_at(incarnation: &str, seq: u64, main: Value) -> Value {
    let mut snap = json!({"version": 1, "incarnation": incarnation, "seq": seq,
           "settings": {"mode": "agent", "model": "grok", "config": {"options": [
               {"id": "effort", "name": "Effort", "category": "thought_level", "type": "select", "current": "medium",
                "selectValues": [{"value": "low", "name": "Low"}, {"value": "medium", "name": "Medium"}, {"value": "high", "name": "High"}]}]}}});
    snap["main"] = main;
    snap
}

/// A host `hello` result (WIRE/01's, the host's id given).
pub fn host_hello_result(host_id: &str) -> Value {
    json!({"protocol": 1, "endpoint": {"kind": "host", "hostId": host_id, "crazeVersion": "0.0.0-fakehost", "pid": 4242},
           "clientId": "c-1", "token": "52fdfc072182654f163f5f0f9a621d72", "resumed": false,
           "capabilities": {"rosterSubscribe": false, "sessionCreate": false, "multiplex": false, "connect": false,
                            "snapshot": true, "attachWhenNow": true},
           "codecs": {"event": 1, "snapshot": 1},
           "limits": {"inboundLine": 4194304, "outboundLine": 16777216},
           "retryHorizon": {"commands": 1024, "ageMs": 600000}})
}

/// An attach result: `snapshot` present exactly when the cursor was not
/// honoured; `reset` the refused cursor's reason.
pub fn attach_result(
    subscription: &str,
    info: &Value,
    after: (&str, u64),
    snapshot: Option<Value>,
    reset: Option<&str>,
) -> Value {
    let mut v = json!({"subscription": subscription, "session": info, "ready": true,
                       "after": {"incarnation": after.0, "seq": after.1}});
    if let Some(s) = snapshot {
        v["snapshot"] = s;
    }
    if let Some(r) = reset {
        v["reset"] = json!(r);
    }
    v
}

impl HubEnd {
    /// The next request, waiting at most `within` (a paused-clock cell's own
    /// bound); `None` once the client closed or the time passed.
    pub async fn recv_within(&mut self, within: Duration) -> Option<Value> {
        let line = tokio::time::timeout(within, self.lines.next_line())
            .await
            .ok()?
            .ok()
            .flatten()?;
        Some(serde_json::from_str(&line).unwrap_or_else(|e| panic!("a request {line:?}: {e}")))
    }

    /// A lane's splice: the hub's `hello`, then `session.connect` for
    /// `host_id` and the host `hello` pipelined behind it, both answered.
    /// Returns the connect request.
    pub async fn splice(&mut self, host_id: &str) -> Value {
        self.splice_answered_by(host_id, host_id).await
    }

    /// [`HubEnd::splice`] for `host_id`, the host `hello` answered by
    /// `answering` — a misrouting hub when the two differ.
    pub async fn splice_answered_by(&mut self, host_id: &str, answering: &str) -> Value {
        self.hello("0a1b2c3d4e5f", full_hub_capabilities()).await;
        let connect = self.expect("session.connect").await;
        assert_eq!(
            connect["params"],
            json!({"sessionId": host_id}),
            "connect by hostId"
        );
        let hello = self.expect("hello").await;
        self.reply(&connect, json!({})).await;
        self.reply(&hello, host_hello_result(answering)).await;
        connect
    }

    /// Read `sessions.list` and answer with the one `row`.
    pub async fn listed(&mut self, row: Value) -> Value {
        let req = self.expect("sessions.list").await;
        self.reply(
            &req,
            json!({"epoch": "0123456789ab", "cursor": 1, "sessions": [row]}),
        )
        .await;
        req
    }

    /// Read `session.attach` and answer with `result` — then the lane's
    /// fenced read (Amendment A11) with an empty registry and a
    /// `session.sync` at the reply's `after.seq`. Returns the attach request.
    pub async fn attached(&mut self, result: Value) -> Value {
        let seq = result["after"]["seq"].as_u64().unwrap_or(0);
        let req = self.attached_only(result).await;
        self.registry(json!([]), seq).await;
        req
    }

    /// Read `session.attach` and answer with `result`, nothing more.
    pub async fn attached_only(&mut self, result: Value) -> Value {
        let req = self.expect("session.attach").await;
        self.reply(&req, result).await;
        req
    }

    /// The lane's fenced read after an attach: `asks.list` answered with
    /// `records`' summaries, `asks.get` for each answered with its record (in
    /// order), then `session.sync` answered at `seq`. Nothing at all when the
    /// lane let the connection go instead (a fault in the reply).
    pub async fn registry(&mut self, records: Value, seq: u64) {
        if !self.registry_reads(records).await {
            return;
        }
        let sync = self.expect("session.sync").await;
        self.reply(&sync, json!({ "seq": seq })).await;
    }

    /// The registry half of it: `asks.list` answered with `records`'
    /// summaries, then `asks.get` for each, in order. `false` when the lane
    /// closed instead of asking.
    pub async fn registry_reads(&mut self, records: Value) -> bool {
        let Some(list) = self.recv().await else {
            return false;
        };
        assert_eq!(list["method"], "asks.list", "the registry read: {list}");
        let records = records.as_array().cloned().unwrap_or_default();
        let summaries: Vec<Value> = records
            .iter()
            .map(|r| json!({"id": r["id"], "kind": r["kind"], "label": "", "openedAt": r["openedAt"]}))
            .collect();
        self.reply(&list, json!({ "asks": summaries })).await;
        for record in &records {
            let get = self.expect("asks.get").await;
            assert_eq!(
                get["params"]["askId"], record["id"],
                "the listed ids, in order"
            );
            self.reply(&get, json!({ "ask": record })).await;
        }
        true
    }

    /// One notification.
    pub async fn notify(&mut self, method: &str, params: Value) {
        self.send(&json!({"jsonrpc": "2.0", "method": method, "params": params}))
            .await;
    }

    /// One `event` notification.
    pub async fn event(&mut self, subscription: &str, seq: u64, event: Value) {
        self.notify(
            "event",
            json!({"subscription": subscription, "seq": seq, "event": event}),
        )
        .await;
    }

    /// One `synchronized` notification.
    pub async fn synchronized(&mut self, subscription: &str, seq: u64) {
        self.notify(
            "synchronized",
            json!({"subscription": subscription, "seq": seq}),
        )
        .await;
    }
}

// ---- a tee, for the live recording ----

/// One line a connection carried: which connection (in dial order), which
/// way, and the line.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct TeeLine {
    pub conn: usize,
    /// `"c2s"` or `"s2c"`, as craze's own fixtures say.
    pub dir: &'static str,
    pub line: String,
}

/// A dial around another that records every line each connection writes and
/// reads, in order per direction — the live recording's capture. It records;
/// it never rewrites (the recording's guard refuses, it does not scrub).
pub struct TeeDial {
    inner: Arc<dyn CrazeDial>,
    lines: Arc<Mutex<Vec<TeeLine>>>,
    next: AtomicUsize,
}

impl TeeDial {
    pub fn new(inner: Arc<dyn CrazeDial>) -> Arc<TeeDial> {
        Arc::new(TeeDial {
            inner,
            lines: Arc::default(),
            next: AtomicUsize::new(0),
        })
    }

    /// Every line recorded so far.
    pub fn lines(&self) -> Vec<TeeLine> {
        lock(&self.lines).clone()
    }
}

impl CrazeDial for TeeDial {
    fn dial(&self) -> BoxFuture<'static, Result<CrazeStream, DialError>> {
        let conn = self.next.fetch_add(1, Ordering::SeqCst);
        let inner = self.inner.dial();
        let lines = Arc::clone(&self.lines);
        Box::pin(async move {
            let (r, w, tail, exit) = inner.await?.into_parts();
            let r: BoxRead = Box::new(TeeRead {
                inner: r,
                tee: Tee::new(conn, "s2c", Arc::clone(&lines)),
            });
            let w: BoxWrite = Box::new(TeeWrite {
                inner: w,
                tee: Tee::new(conn, "c2s", lines),
            });
            Ok(CrazeStream::from_parts(r, w, tail, exit))
        })
    }
}

/// One direction's line splitter.
struct Tee {
    conn: usize,
    dir: &'static str,
    buf: Vec<u8>,
    lines: Arc<Mutex<Vec<TeeLine>>>,
}

impl Tee {
    fn new(conn: usize, dir: &'static str, lines: Arc<Mutex<Vec<TeeLine>>>) -> Tee {
        Tee {
            conn,
            dir,
            buf: Vec::new(),
            lines,
        }
    }

    fn take(&mut self, bytes: &[u8]) {
        self.buf.extend_from_slice(bytes);
        while let Some(pos) = self.buf.iter().position(|&b| b == b'\n') {
            let line: Vec<u8> = self.buf.drain(..=pos).collect();
            let text = String::from_utf8_lossy(&line[..line.len() - 1])
                .trim_end_matches('\r')
                .to_string();
            lock(&self.lines).push(TeeLine {
                conn: self.conn,
                dir: self.dir,
                line: text,
            });
        }
    }
}

struct TeeRead {
    inner: BoxRead,
    tee: Tee,
}

impl AsyncRead for TeeRead {
    fn poll_read(
        mut self: std::pin::Pin<&mut Self>,
        cx: &mut std::task::Context<'_>,
        buf: &mut tokio::io::ReadBuf<'_>,
    ) -> std::task::Poll<std::io::Result<()>> {
        let before = buf.filled().len();
        let this = &mut *self;
        let poll = std::pin::Pin::new(&mut this.inner).poll_read(cx, buf);
        if let std::task::Poll::Ready(Ok(())) = &poll {
            this.tee.take(&buf.filled()[before..]);
        }
        poll
    }
}

struct TeeWrite {
    inner: BoxWrite,
    tee: Tee,
}

impl AsyncWrite for TeeWrite {
    fn poll_write(
        mut self: std::pin::Pin<&mut Self>,
        cx: &mut std::task::Context<'_>,
        buf: &[u8],
    ) -> std::task::Poll<std::io::Result<usize>> {
        let this = &mut *self;
        let poll = std::pin::Pin::new(&mut this.inner).poll_write(cx, buf);
        if let std::task::Poll::Ready(Ok(n)) = &poll {
            this.tee.take(&buf[..*n]);
        }
        poll
    }

    fn poll_flush(
        mut self: std::pin::Pin<&mut Self>,
        cx: &mut std::task::Context<'_>,
    ) -> std::task::Poll<std::io::Result<()>> {
        std::pin::Pin::new(&mut self.inner).poll_flush(cx)
    }

    fn poll_shutdown(
        mut self: std::pin::Pin<&mut Self>,
        cx: &mut std::task::Context<'_>,
    ) -> std::task::Poll<std::io::Result<()>> {
        std::pin::Pin::new(&mut self.inner).poll_shutdown(cx)
    }
}
