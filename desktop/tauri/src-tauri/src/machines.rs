//! **Machine targets in the desktop app** — read from `roost-session`s
//! (plan 013 S3, the Roost Pivot's first milestone; originally plan 012 S4).
//!
//! `machines:` has lived in `shed-core`'s config since plan 009. Until plan 013
//! this module read each machine's **RC hub** over an `ssh -N -L` forward; it now
//! reads the machine's **`roost-session`** directly, because that is the
//! substrate the pivot is moving to. The reach itself lives in the shared layer:
//!
//! * the roost wire (`session.identify`, `tab.list`, `tab.open/close`) →
//!   [`shed_core::roost`]
//! * the transport seam + the observer watcher → [`shed_app::roost`]
//!
//! What is left here is what a desktop app actually owns: which machines exist,
//! one watcher per machine, the last inventory each one reported, and whether it
//! is currently reachable.
//!
//! ## Unreachable is a STATE, not an error
//!
//! A machine that is asleep, off the network, or simply runs no `roost-session`
//! is the normal case, not a failure. Every configured machine therefore always
//! has a row; `reachable` and `detail` say how much to trust it. Nothing here
//! returns an error to the UI for a machine being down.
//!
//! ## The implicit `localhost` host
//!
//! The one machine a user always has is the one they are sitting at, and it needs
//! no config entry: when nothing in `machines:` is named `localhost`, this module
//! registers a [`LocalSession`] reach under that name. It follows roost's
//! connect-if-present rule in both directions — **a `localhost` whose socket has
//! never existed in this process is not listed at all** ([`MachineState::listed`]),
//! because a host that has never run a session is not a thing the user asked
//! about. Once one has answered, the host stays listed and a later disappearance
//! is an ordinary unreachable row with the reason, exactly like a configured
//! machine that went to sleep.
//!
//! A configured entry named `localhost` WINS (the user said what they meant), and
//! [`Machines::add`] refuses the name so the two can never both exist.
//!
//! ## One overlay per feed
//!
//! Sessions are held per machine and never merged into a shared activity overlay.
//! Roost reports no shed (there is none), so `(shed, slug)` — the key
//! [`shed_core::rc_events::ActivityOverlay`] uses — would collide across two
//! machines whose tab ids happen to match. Rows are keyed by ORIGIN + slug here
//! instead.

use std::collections::{BTreeMap, HashMap};
use std::path::PathBuf;
use std::sync::{Arc, Mutex, MutexGuard};

use serde_json::{json, Value};

use roost_ipc::agent::Ownership;
use roost_ipc::messages::{Tab, TabOpenParams};
use shed_app::roost::{
    launch_argv, roost_capabilities, tab_close, tab_open, LocalSession, RoostReach, RoostUpdate,
    RoostWatcher, SshBridge, SshBridgeOptions, UnreachableReach,
};
use shed_core::config::{MachineEntry, ShedConfig};
use shed_core::rc::RcKind;
use shed_core::roost::RoostSession;

/// The name the machine the app is running on is always known by — never a
/// configured entry's name unless the user wrote one, and never an ssh target.
///
/// It matters that this string never reaches `roost_ipc::ssh::classify`: roost
/// treats `localhost` there as a sentinel for the LOCAL session socket, resolved
/// through its own build-profile-sensitive resolver (the `-dev` trap). The
/// implicit host is a [`LocalSession`], which never goes near `classify`; a
/// configured machine literally named `localhost` is an [`SshBridge`] like any
/// other, and [`shed_app::roost`] spells its target `ssh://localhost` precisely so
/// the sentinel is not hit.
const LOCALHOST: &str = "localhost";

/// Why a machine row offers no terminal, in the words BOTH doors answer with —
/// the `terminal.open`/`terminal.preview` IPC ops and the `open_terminal` Tauri
/// command (plan 013 S3). One string because it is one rule: two copies would
/// drift and only one of them would be under the harness's eye.
pub const NO_TERMINAL: &str = "terminal unavailable: attach is native-remote";

/// Take a lock, ignoring poisoning.
///
/// Every mutex here guards plain data (a name list, a row cache) that a panicking
/// holder cannot leave half-updated in a way the next reader would misread. The
/// alternative — unwrapping — turns one unrelated panic into a permanently dead
/// machine layer, which is strictly worse than reading a slightly stale row.
fn lock<T>(m: &Mutex<T>) -> MutexGuard<'_, T> {
    m.lock().unwrap_or_else(|e| e.into_inner())
}

/// Called whenever a machine's state changes, so the embedder can tell its UI
/// to re-read. Without it the app would only ever show the state it happened to
/// fetch at mount: a machine that comes up (or drops) later changes nothing the
/// frontend is watching, and the rows sit stale until a manual Refresh.
pub type OnChange = Arc<dyn Fn() + Send + Sync>;

/// One machine's live view, as the UI reads it.
struct MachineState {
    /// The last inventory the session reported — agent-owned tabs only
    /// ([`shed_core::roost::RoostInventory`] filters plain shells out; a user
    /// with fifteen terminals must not get fifteen cards).
    ///
    /// Retained across a disconnect on purpose: the UI keeps rendering the last
    /// known sessions (dimmed, with a reason) rather than blanking the machine,
    /// matching how the shed feed treats a blip.
    sessions: Vec<RoostSession>,
    reachable: bool,
    /// Why it is unreachable, verbatim from the watcher. Shown to the user —
    /// "no roost-session at /run/user/1000/roost-session/roost.sock" and "no
    /// route to host" are different problems and the app should not flatten
    /// them into "offline".
    detail: Option<String>,
    /// Whether a snapshot has EVER arrived, so the UI can distinguish "still
    /// connecting" from "connected, and this machine genuinely has no sessions".
    seen: bool,
    /// Whether this host may appear in a listing at all.
    ///
    /// `true` from the start for every CONFIGURED machine — the user named it, so
    /// its row (unreachable or not) is the information. `false` until the first
    /// snapshot for the implicit [`LOCALHOST`] host, which nobody asked for: a
    /// machine that has never run a `roost-session` should show no roost UI at
    /// all. Once flipped it stays flipped, so a session that stops leaves a
    /// normal unreachable row rather than making the host vanish mid-look.
    listed: bool,
}

impl MachineState {
    fn new(listed: bool) -> Self {
        Self {
            sessions: Vec::new(),
            reachable: false,
            detail: None,
            seen: false,
            listed,
        }
    }
}

/// The app's machine layer: one watcher per machine, plus the state each reports.
pub struct Machines {
    /// Keyed by machine NAME, which is also the origin handle (`machine:<name>`).
    state: Arc<Mutex<BTreeMap<String, MachineState>>>,
    /// The machines this app knows about, and the watchers keeping them live.
    ///
    /// Behind a lock because the set GROWS: adding a machine has to start
    /// watching it now, not on the next launch. A relaunch-to-see-it would be a
    /// worse affordance than editing the config by hand, which is what this
    /// replaces.
    reg: Mutex<Registry>,
    /// Kept so a machine added later gets a watcher on the same runtime, with
    /// the same test-mode reach substitution and the same change callback as
    /// the ones started at boot — one code path, not two.
    handle: tokio::runtime::Handle,
    test_roost_sockets: HashMap<String, PathBuf>,
    on_change: OnChange,
}

/// The reserved-name gate, shared by both doors into [`Machines::add`].
///
/// Checked BEFORE the config write in [`add_from_json`] as well as inside
/// [`Machines::add`]: refusing only at the second step would leave a `localhost:`
/// entry in the user's `~/.shed/config.yaml` that the next launch would silently
/// prefer over the implicit host.
fn reject_reserved_name(name: &str) -> Result<(), String> {
    if name == LOCALHOST {
        return Err(format!(
            "{LOCALHOST:?} is this machine's own roost-session and is always present — \
             it cannot be added as a machine"
        ));
    }
    Ok(())
}

/// Append a machine to the shed config, then start watching it.
///
/// Shared by the Tauri command (the dialog's path) and the IPC op (the
/// harness's), so the thing under test is the thing that ships. Two steps, in
/// this order, because they fail differently: the config write is the durable
/// half and refuses a duplicate, while a watcher that cannot reach its machine
/// is still a legitimate row. Writing first also means a failed start leaves a
/// configured machine the next launch picks up, rather than a watcher with
/// nothing behind it.
///
/// The write is INSERT-ONLY (see `shed_core::config_edit`) and takes a backup
/// first: that file is hand-maintained, and this app is a guest in it.
pub fn add_from_json(
    machines: &Machines,
    path: &std::path::Path,
    machine: &Value,
) -> Result<(), String> {
    use shed_core::config_edit::{insert_machine, NewMachine};

    // ONE add at a time. The IPC op and the Tauri command are separate entry
    // points into this function, and the IPC server serves connections
    // concurrently — so without this two adds can both read the original text
    // and the second write silently drops the first's entry.
    //
    // In-process only. A concurrent `shed server add` from the CLI (which takes
    // its own `%config.lock`) is still a lost-update window; closing that means
    // adopting the same lock file, which is worth doing but is not this change.
    static ADD_LOCK: Mutex<()> = Mutex::new(());
    let _serialized = lock(&ADD_LOCK);

    let name = machine
        .get("name")
        .and_then(Value::as_str)
        .unwrap_or("")
        .trim()
        .to_string();
    // Before the write, not after — see `reject_reserved_name`.
    reject_reserved_name(&name)?;
    let field = |k: &str| {
        machine
            .get(k)
            .and_then(Value::as_str)
            .map(str::trim)
            .filter(|v| !v.is_empty())
            .map(str::to_string)
    };
    let (host, user, rc_bin) = (field("host"), field("user"), field("rc_bin"));
    // A port that cannot be understood is REJECTED, not silently defaulted:
    // "22" appearing where the user typed 2200 is worse than an error, because
    // the dialog would report success and the machine would be unreachable for
    // a reason nothing on screen explains. An absent field still means "use the
    // default", which is what makes the field optional.
    let ssh_port = match machine.get("ssh_port") {
        None | Some(Value::Null) => None,
        Some(v) => {
            let n = v
                .as_u64()
                .or_else(|| v.as_str().and_then(|s| s.trim().parse::<u64>().ok()))
                .filter(|n| (1..=65535).contains(n))
                .ok_or_else(|| format!("{v} is not a usable SSH port (1-65535)"))?;
            Some(n as u16)
        }
    };

    // ONLY a missing file means "start from empty". Any other read error — a
    // permission problem, non-UTF-8 bytes, a directory in the way — must abort:
    // treating it as absent would skip the backup and then replace the whole
    // config with just this one block.
    let text = match std::fs::read_to_string(path) {
        Ok(t) => t,
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => String::new(),
        Err(e) => return Err(format!("could not read {}: {e}", path.display())),
    };
    let updated = insert_machine(
        &text,
        &NewMachine {
            name: &name,
            host: host.as_deref(),
            user: user.as_deref(),
            ssh_port,
            rc_bin: rc_bin.as_deref(),
        },
    )
    .map_err(|e| e.to_string())?;

    if let Some(dir) = path.parent() {
        std::fs::create_dir_all(dir).map_err(|e| format!("{}: {e}", dir.display()))?;
    }
    if !text.is_empty() {
        let backup = path.with_extension("yaml.bak");
        std::fs::write(&backup, &text)
            .map_err(|e| format!("could not back up {}: {e}", backup.display()))?;
    }
    write_atomically(path, &updated)?;

    // Re-read rather than trusting our own construction: whatever the READER
    // makes of the file is what every other client sees, so the watcher should
    // start from that and not from the form.
    let entry = ShedConfig::parse(&updated)
        .machine(&name)
        .cloned()
        .ok_or_else(|| format!("{name:?} was written but does not parse back"))?;
    machines.add(entry)
}

/// Write `text` to `path` without ever leaving a half-written file there.
///
/// `fs::write` truncates in place, so a failure partway (a full disk, a crash)
/// leaves the config corrupt — and the backup only helps someone who knows to
/// look for it. A temp file in the same directory plus a rename is atomic on
/// every platform this runs on: the config is either the old bytes or the new
/// ones, never half of each.
fn write_atomically(path: &std::path::Path, text: &str) -> Result<(), String> {
    let tmp = path.with_extension("yaml.tmp");
    std::fs::write(&tmp, text).map_err(|e| format!("{}: {e}", tmp.display()))?;
    std::fs::rename(&tmp, path).map_err(|e| {
        let _ = std::fs::remove_file(&tmp);
        format!("{}: {e}", path.display())
    })
}

/// The mutable half of [`Machines`]: the registered set and its live watchers.
///
/// `names` carries ORDER (config order, then arrival order) because the UI lists
/// machines in it, and it is also the membership set a duplicate `add` is checked
/// against — the implicit [`LOCALHOST`] host has no [`MachineEntry`], so a map
/// keyed by entry would not see it.
struct Registry {
    names: Vec<String>,
    /// The reach each watcher was STARTED with, keyed by name.
    ///
    /// Control verbs resolve through this rather than re-deriving one from the
    /// config, so a kill can never address a different host than the row the user
    /// is looking at: if `machines:` is edited to repoint `mini3` mid-session, the
    /// watcher (and therefore the displayed rows) still belong to the reach that
    /// was built at start, and the kill must follow the rows.
    ///
    /// A machine whose reach could not even be BUILT is absent here but present
    /// in `names` — it is a listed, permanently-unreachable row.
    reaches: BTreeMap<String, Arc<dyn RoostReach>>,
    /// Held so the watchers (and the SSH bridges behind them) live as long as the
    /// app does. Dropping one aborts its loop.
    watchers: Vec<RoostWatcher>,
}

impl Machines {
    /// Start a watcher per configured machine, plus the implicit [`LOCALHOST`]
    /// one. Never fails: a machine whose reach cannot even be built is still
    /// listed, as unreachable with the reason — the same posture as one that is
    /// merely asleep.
    ///
    /// `test_roost_sockets` (from the test-mode-only
    /// [`crate::env::Env::roost_sockets`]) replaces the SSH bridge with a direct
    /// [`LocalSession`] on the named socket, per machine. When it is non-empty NO
    /// machine spawns ssh — an unmapped entry gets an [`UnreachableReach`] — so a
    /// hermetic run cannot leak an ssh child, and the "machine is asleep" state is
    /// coverable without a real machine. `localhost` goes through the same map, so
    /// a hermetic run does not read the developer's own session either.
    ///
    /// `on_change` fires whenever any LISTED machine's state moves, so the
    /// embedder can push a refresh to its UI rather than leaving rows stale until
    /// someone clicks Refresh.
    pub fn start(
        handle: &tokio::runtime::Handle,
        config: &ShedConfig,
        test_roost_sockets: &HashMap<String, PathBuf>,
        on_change: OnChange,
    ) -> Machines {
        let machines = Machines {
            state: Arc::new(Mutex::new(BTreeMap::new())),
            reg: Mutex::new(Registry {
                names: Vec::new(),
                reaches: BTreeMap::new(),
                watchers: Vec::new(),
            }),
            handle: handle.clone(),
            test_roost_sockets: test_roost_sockets.clone(),
            on_change,
        };
        for entry in &config.machines {
            machines.watch(entry.clone());
        }
        // A configured entry WINS: the user spelling `localhost` in `machines:`
        // means an ssh target they chose, and shadowing it with the implicit
        // local reach would make the config a lie.
        if !config.machines.iter().any(|m| m.name == LOCALHOST) {
            machines.watch_localhost();
        }
        machines
    }

    /// Start watching one configured machine: register it, seed its row, and
    /// spawn its watcher + consumer.
    ///
    /// The SINGLE path a configured machine enters by, whether it came from the
    /// config at boot or from the Add dialog a minute ago — so a machine added
    /// later behaves identically rather than nearly so.
    fn watch(&self, entry: MachineEntry) {
        lock(&self.reg).names.push(entry.name.clone());
        let reach = build_reach(&entry, &self.test_roost_sockets);
        self.start_watching(entry.name, reach, true);
    }

    /// Start watching the implicit local host. Registered LAST so it sorts after
    /// the machines the user actually configured, and UNLISTED until its session
    /// answers (see the module doc).
    fn watch_localhost(&self) {
        lock(&self.reg).names.push(LOCALHOST.to_string());
        let reach = Ok(build_local_reach(&self.test_roost_sockets));
        self.start_watching(LOCALHOST.to_string(), reach, false);
    }

    /// Seed the row and start the watcher for an ALREADY-REGISTERED name.
    ///
    /// Split from registration so `add` can claim the name and register it in
    /// one lock acquisition — a check-then-register across two would let two
    /// concurrent adds both win.
    fn start_watching(
        &self,
        name: String,
        reach: Result<Arc<dyn RoostReach>, String>,
        listed: bool,
    ) {
        lock(&self.state).insert(name.clone(), MachineState::new(listed));

        let reach = match reach {
            Ok(reach) => reach,
            Err(e) => {
                // The reach could not even be constructed (an entry with no host,
                // a `known_hosts` file we cannot write beside). Record it and move
                // on: a machine that cannot be reached is a row, not an error.
                let mut guard = lock(&self.state);
                if let Some(m) = guard.get_mut(&name) {
                    m.detail = Some(e);
                }
                return;
            }
        };

        let (watcher, rx) = RoostWatcher::spawn(&self.handle, Arc::clone(&reach), name.clone());
        {
            let mut reg = lock(&self.reg);
            reg.reaches.insert(name.clone(), reach);
            reg.watchers.push(watcher);
        }
        self.handle.spawn(consume(
            name,
            rx,
            Arc::clone(&self.state),
            self.on_change.clone(),
        ));
    }

    /// Add a machine and start watching it now.
    ///
    /// Rejects a name already being watched rather than shadowing it: two rows
    /// with one name is a UI that cannot be reasoned about, and the config write
    /// upstream refuses the same case for the same reason. `localhost` is
    /// reserved (see [`reject_reserved_name`]).
    pub fn add(&self, entry: MachineEntry) -> Result<(), String> {
        reject_reserved_name(&entry.name)?;
        // Claim the name under the SAME lock acquisition that registers it.
        // Checking and then registering through two acquisitions lets two adds
        // both pass the check and both register, leaving one name with two
        // watchers and two rows.
        {
            let mut reg = lock(&self.reg);
            if reg.names.iter().any(|n| n == &entry.name) {
                return Err(format!(
                    "a machine named {:?} is already watched",
                    entry.name
                ));
            }
            reg.names.push(entry.name.clone());
        }
        let reach = build_reach(&entry, &self.test_roost_sockets);
        self.start_watching(entry.name, reach, true);
        (self.on_change)();
        Ok(())
    }

    /// The sessions AND the per-machine health, read under ONE lock.
    ///
    /// Taken together on purpose: read separately, a disconnect landing between
    /// the two calls yields a payload where a row says `stale: false` while its
    /// machine says `reachable: false` — a self-contradicting frame the UI would
    /// render as "live session on an offline machine".
    pub fn snapshot(&self) -> (Vec<Value>, Vec<Value>) {
        let guard = lock(&self.state);
        (self.sessions_locked(&guard), self.status_locked(&guard))
    }

    /// Every listed machine's rows, flattened for the sessions view, each stamped
    /// with its origin so the UI can key and label it without inspecting `shed`
    /// (which is empty for every machine session — see the module doc).
    fn sessions_locked(&self, guard: &BTreeMap<String, MachineState>) -> Vec<Value> {
        let mut out = Vec::new();
        for (name, m) in guard.iter() {
            if !m.listed {
                continue;
            }
            for session in &m.sessions {
                out.push(machine_row(name, session, !m.reachable));
            }
        }
        out
    }

    /// Close a session on a machine — `tab.close` on its roost tab — then drop
    /// the row optimistically.
    ///
    /// **The optimistic drop is still worth keeping, for a different reason than
    /// it was written for.** It used to cover a 2 s poll cadence; since plan 014
    /// the watcher observes a push feed, so the `tab.closed` this very call
    /// commits normally comes back within milliseconds and the row would leave on
    /// its own. What is *not* bounded is the unhappy path: if the stream is
    /// mid-resync (a gap, an EOF, a daemon restart) the close is only seen by the
    /// next cycle's `tab.list`, and if the machine drops right after the close
    /// lands there is no next snapshot at all — [`consume`] deliberately keeps
    /// the last row set across a `Down`, so a session the user just killed would
    /// sit there greyed out until the machine came back. Dropping it here makes
    /// the answer immediate in every case, and matches [`Self::create`]'s
    /// optimistic insert on the other side. The next snapshot is authoritative
    /// and will restore the row if the close somehow did not take.
    ///
    /// The slug IS the tab id (`RoostSession::to_rc_dto` stringifies it), so a
    /// slug that is not an integer is a row from somewhere else and is refused by
    /// name rather than sent to roost as a zero.
    pub async fn kill(&self, machine: &str, slug: &str) -> Result<(), String> {
        let reach = self.reach(machine)?;
        let tab_id = parse_tab_id(slug)?;
        tab_close(reach.as_ref(), tab_id).await?;
        let mut guard = lock(&self.state);
        if let Some(m) = guard.get_mut(machine) {
            m.sessions.retain(|s| s.tab_id != tab_id);
        }
        Ok(())
    }

    /// This machine's RC capabilities — what a create form may offer.
    ///
    /// **Synthesized, never probed.** roost is not shed's guest agent and has no
    /// `shed-ext-rc capabilities` to ask; the honest answer is the contract this
    /// client implements against it, which is a constant
    /// ([`shed_app::roost::roost_capabilities`]). So this is no longer an SSH
    /// round-trip — it cannot fail, cannot be stale, and answers for a machine
    /// that is currently asleep.
    ///
    /// Still resolved through the registry so an unknown machine name is an error
    /// rather than a confident answer about a host nobody is watching.
    pub fn capabilities(&self, machine: &str) -> Result<Value, String> {
        self.known(machine)?;
        Ok(json!(roost_capabilities()))
    }

    /// Open a session ON this machine — a roost `tab.open` running the kind's
    /// agent — and fold it into the local snapshot so the row appears immediately
    /// rather than whenever the push feed next catches up (see [`Self::kill`] for
    /// why "normally milliseconds" is not the same as "always").
    async fn create(
        &self,
        machine: &str,
        kind: &RcKind,
        workdir: Option<&str>,
    ) -> Result<Value, String> {
        let reach = self.reach(machine)?;
        let params = open_params(kind, workdir)?;
        let tab = tab_open(reach.as_ref(), params).await?;
        let session = opened_session(machine, kind, &tab);
        let value = machine_row(machine, &session, false);
        {
            let mut guard = lock(&self.state);
            // Only if the watcher has not already delivered it. `get_mut` also
            // means a machine dropped from the registry mid-open is not
            // resurrected by its own result.
            if let Some(m) = guard.get_mut(machine) {
                if !m.sessions.iter().any(|s| s.tab_id == session.tab_id) {
                    m.sessions.push(session);
                }
            }
        }
        // Outside the lock (it re-enters the app), and unconditional: a caller
        // that is not the dialog — socket IPC, say — has nothing else that
        // would tell the open UI the row exists.
        (self.on_change)();
        Ok(value)
    }

    /// Create a session from CALLER-SUPPLIED, un-normalized fields.
    ///
    /// The one place blank-vs-absent is decided, shared by the `machine.launch`
    /// IPC op and the `machine_launch` Tauri command: the same request must not
    /// mean two things depending on which door it came through. A field that is
    /// blank or all whitespace is ABSENT — `"   "` as a working directory is
    /// someone leaving the box empty, not a directory named three spaces.
    ///
    /// **`display_name`, `permission_mode` and `initial_prompt` are accepted and
    /// not used** in M1 (plan 013 §4). Kickoff is the minimal `tab.open`: the
    /// agent binary and a cwd. roost owns the tab's title (it follows the
    /// foreground process, and shed showing a second divergent name would be
    /// worse than showing roost's), and prompts + permission modes need the
    /// provider script that is S4's. They stay in the signature so both doors keep
    /// one wire while that lands, and so a caller is not silently rejected for
    /// sending what the old hub accepted.
    pub async fn launch(
        &self,
        machine: &str,
        kind: &RcKind,
        _display_name: Option<&str>,
        workdir: Option<&str>,
        _permission_mode: Option<&str>,
        _initial_prompt: Option<&str>,
    ) -> Result<Value, String> {
        self.create(
            machine,
            kind,
            workdir.map(str::trim).filter(|s| !s.is_empty()),
        )
        .await
    }

    /// One watched machine's reach, or an error naming the ones there are.
    fn reach(&self, machine: &str) -> Result<Arc<dyn RoostReach>, String> {
        let reg = lock(&self.reg);
        if let Some(reach) = reg.reaches.get(machine) {
            return Ok(Arc::clone(reach));
        }
        if reg.names.iter().any(|n| n == machine) {
            return Err(format!(
                "machine {machine:?} has no usable transport (its reach could not be built)"
            ));
        }
        Err(unknown_machine(machine, &reg.names))
    }

    /// Assert a machine is registered, without needing its reach — for the
    /// answers (capabilities) that do not touch the wire.
    fn known(&self, machine: &str) -> Result<(), String> {
        let reg = lock(&self.reg);
        if reg.names.iter().any(|n| n == machine) {
            return Ok(());
        }
        Err(unknown_machine(machine, &reg.names))
    }

    /// Per-machine health, for the UI's machine group headers.
    pub fn status(&self) -> Vec<Value> {
        let guard = lock(&self.state);
        self.status_locked(&guard)
    }

    fn status_locked(&self, guard: &BTreeMap<String, MachineState>) -> Vec<Value> {
        // Config order, then arrival order — a machine added mid-session appears
        // at the end rather than reshuffling the list someone is looking at.
        //
        // `reg` is taken UNDER the caller's `state` guard, which is the only
        // place the two are held at once — nothing takes them the other way
        // round (`add` releases `reg` before `start_watching` touches `state`).
        let reg = lock(&self.reg);
        reg.names
            .iter()
            .filter_map(|name| {
                let m = guard.get(name);
                // An unlisted host (the implicit `localhost` before its first
                // snapshot) is not a row at all — see the module doc. A name with
                // no state yet is mid-registration and is listed as unreachable,
                // which is what it is.
                if m.is_some_and(|m| !m.listed) {
                    return None;
                }
                Some(json!({
                    "name": name,
                    "origin": format!("machine:{name}"),
                    "reachable": m.is_some_and(|m| m.reachable),
                    "connected_once": m.is_some_and(|m| m.seen),
                    "sessions": m.map_or(0, |m| m.sessions.len()),
                    "detail": m.and_then(|m| m.detail.clone()),
                }))
            })
            .collect()
    }
}

fn unknown_machine(machine: &str, names: &[String]) -> String {
    format!(
        "no machine {machine:?} is being watched (have: {})",
        names.join(", ")
    )
}

/// A row slug back into the roost tab id it is.
fn parse_tab_id(slug: &str) -> Result<i64, String> {
    slug.trim()
        .parse::<i64>()
        .map_err(|_| format!("{slug:?} is not a roost tab id"))
}

/// The `tab.open` request for one kind, or a refusal naming the kind.
///
/// Everything but `argv` and `cwd` is deliberately zero/empty: `project_id: 0`
/// lets roost put the tab in its own default project (shed has no opinion about
/// somebody's project layout), `cols`/`rows` let roost size the PTY, and `title`
/// stays roost's — it follows the foreground process, which is the name the user
/// sees in roost's own sidebar.
///
/// A kind with no launch recipe (`shell`, `claude-broker`, `grok`, anything
/// unknown) is refused HERE, before any connection is made, so the failure names
/// the kind rather than leaving an empty tab open on somebody's machine.
fn open_params(kind: &RcKind, workdir: Option<&str>) -> Result<TabOpenParams, String> {
    let argv = launch_argv(kind).ok_or_else(|| {
        format!(
            "unknown kind {:?}: roost has no launch recipe for it",
            kind.as_str()
        )
    })?;
    Ok(TabOpenParams {
        project_id: 0,
        cwd: workdir.unwrap_or_default().to_string(),
        argv,
        cols: 0,
        rows: 0,
        title: String::new(),
    })
}

/// The session for a tab that was JUST opened.
///
/// `tab.open` answers with the tab **before any adapter has claimed it**:
/// `ownership` is `None`, so [`RoostSession::agent_kind`] would read `shell` and
/// the card would show the wrong kind for the second or two until the adapter's
/// first report promotes it. The kind the caller ASKED for is the honest answer
/// for that window — the process is starting — so it is stamped as a provisional
/// ownership carrying roost's own `source` spelling and nothing else (no agent
/// session id, no detail, no timestamp: shed knows none of them yet, and
/// inventing one would put a fake id on the card).
///
/// The next snapshot REPLACES the row set outright, so this never outlives the
/// adapter's first report.
fn opened_session(machine: &str, kind: &RcKind, tab: &Tab) -> RoostSession {
    RoostSession {
        host_label: machine.to_string(),
        tab_id: tab.id,
        project_id: tab.project_id,
        project_name: String::new(),
        title: tab.title.clone(),
        user_titled: tab.user_titled,
        cwd: tab.cwd.clone(),
        shell_state: tab.shell_state,
        lifecycle: tab.agent_lifecycle,
        attention: tab.has_notification,
        ownership: roost_source(kind).map(|source| Ownership {
            source: source.to_string(),
            ..Ownership::default()
        }),
        created_at: tab.created_at,
    }
}

/// The `ownership.source` string roost's own adapter writes for a kind — the
/// inverse of [`RoostSession::agent_kind`], for the one moment shed has to
/// predict it ([`opened_session`]).
///
/// `None` for every kind with no roost adapter, which is also every kind
/// [`launch_argv`] refuses — so in practice this is only ever called for the four
/// launchable ones, and a `None` simply leaves the fresh tab unowned rather than
/// labelling it with a source roost will never write.
fn roost_source(kind: &RcKind) -> Option<&'static str> {
    match kind {
        RcKind::ClaudeRc => Some("claude"),
        RcKind::Codex => Some("codex"),
        RcKind::Opencode => Some("opencode"),
        RcKind::Cursor => Some("cursor"),
        RcKind::ClaudeBroker | RcKind::Shell | RcKind::Other(_) => None,
    }
}

/// The transport choice — the ONLY per-client part of reaching a machine's
/// `roost-session`.
///
/// Production is [`SshBridge`]: roost's own client-bridge over a shared
/// `ControlMaster`, because a roost-session's socket path is resolved on the FAR
/// side and so cannot be named in an `ssh -L`.
fn build_reach(
    entry: &MachineEntry,
    test_roost_sockets: &HashMap<String, PathBuf>,
) -> Result<Arc<dyn RoostReach>, String> {
    if test_roost_sockets.is_empty() {
        return SshBridge::new(entry, SshBridgeOptions::default())
            .map(|b| Arc::new(b) as Arc<dyn RoostReach>)
            .map_err(|e| e.to_string());
    }
    // Test mode with a map present: reach the harness's fake session on its own
    // Unix socket. That needs no transport at all, which is the point —
    // everything ABOVE the socket is the shared code under test.
    //
    // An UNMAPPED machine gets a reach that simply REFUSES — that is how the
    // suite exercises an unreachable machine, and it guarantees a hermetic run
    // never spawns ssh for a machine the harness forgot to map.
    Ok(mapped_reach(&entry.name, test_roost_sockets))
}

/// The implicit [`LOCALHOST`] host's reach: this machine's own session socket,
/// resolved by shed's own path table (roost's resolver picks the `-dev` socket
/// from the CONSUMING crate's build profile, which would make a debug build of
/// this app read a different session than a release one).
///
/// It goes through the same test-mode map as a configured machine, so a hermetic
/// run reads the harness's fake session rather than the developer's real one.
fn build_local_reach(test_roost_sockets: &HashMap<String, PathBuf>) -> Arc<dyn RoostReach> {
    if test_roost_sockets.is_empty() {
        return Arc::new(LocalSession::default_local());
    }
    mapped_reach(LOCALHOST, test_roost_sockets)
}

fn mapped_reach(name: &str, test_roost_sockets: &HashMap<String, PathBuf>) -> Arc<dyn RoostReach> {
    match test_roost_sockets.get(name) {
        Some(socket) => Arc::new(LocalSession::new(name, socket.clone())),
        None => Arc::new(UnreachableReach::new(
            name,
            "no roost-session mapped for this machine in test mode",
        )),
    }
}

/// Fold one machine's watcher updates into its state.
async fn consume(
    name: String,
    mut rx: tokio::sync::mpsc::UnboundedReceiver<RoostUpdate>,
    state: Arc<Mutex<BTreeMap<String, MachineState>>>,
    on_change: OnChange,
) {
    while let Some(update) = rx.recv().await {
        let visible = {
            let mut guard = lock(&state);
            let Some(m) = guard.get_mut(&name) else {
                return;
            };
            match update {
                RoostUpdate::Snapshot(inventory) => {
                    // The snapshot is authoritative — it REPLACES rather than
                    // merges, which is what makes a reconnect (or a daemon
                    // restart, which resets roost's revision counter) a complete
                    // resync with no replay protocol.
                    m.sessions = inventory.sessions;
                    m.reachable = true;
                    m.detail = None;
                    m.seen = true;
                    // A session answered here at least once, so this host is real
                    // and stays listed from now on.
                    m.listed = true;
                }
                RoostUpdate::Down { reason } => {
                    // Sessions are deliberately NOT cleared: the last snapshot
                    // stays on screen, marked stale, until the next connect
                    // resyncs it.
                    m.reachable = false;
                    m.detail = Some(reason);
                }
            }
            m.listed
        };
        // Outside the lock: the callback re-enters the app (it emits a Tauri
        // event), and holding a std mutex across that is how a deadlock starts.
        //
        // Skipped entirely for an unlisted host: an implicit `localhost` with no
        // session running reports `Down` on every backoff step forever, and
        // nothing the user can see changes — repainting the UI for it would be
        // pure noise.
        if visible {
            on_change();
        }
    }
}

/// One machine session as the UI reads it: the roost row mapped onto the DTO
/// every card already renders, stamped with where it lives.
///
/// Shared by the list and by `create`'s return value — a caller that keys off
/// `origin`/`machine` (or calls `sessionKey()`) must get the same shape from
/// both, or the one place they differ becomes the one place a caller breaks.
///
/// `attention` and `tab_id` are stamped HERE rather than carried on
/// [`shed_core::rc::RcSessionDto`]: that shape is pinned byte-for-byte by the
/// Go↔Rust parity harness and built as a struct literal at a dozen sites, so
/// roost's two extra facts travel on [`RoostSession`] and each client adds them
/// to its own row payload (plan 013 §3.2).
fn machine_row(name: &str, session: &RoostSession, stale: bool) -> Value {
    let mut row = serde_json::to_value(session.to_rc_dto()).unwrap_or_else(|_| json!({}));
    if let Some(obj) = row.as_object_mut() {
        obj.insert("origin".into(), json!(format!("machine:{name}")));
        obj.insert("origin_kind".into(), json!("machine"));
        obj.insert("machine".into(), json!(name));
        // A machine session belongs to no shed and no server. Spell that
        // explicitly rather than leaving the UI to infer it from an empty
        // string.
        obj.insert("host".into(), json!(format!("machine:{name}")));
        obj.insert("shed".into(), json!(""));
        obj.insert("stale".into(), json!(stale));
        // roost's sticky notification bit. Its own affordance (a dot), NOT part
        // of `needsYou`: roost clears it on UI focus and shed never clears it, so
        // folding it into activity would leave a card stuck asking for attention.
        obj.insert("attention".into(), json!(session.attention));
        // A STRING, like every other id on roost's wire: a JavaScript client
        // cannot round an i64 through a `Number` without losing it.
        obj.insert("tab_id".into(), json!(session.tab_id.to_string()));
    }
    row
}

#[cfg(test)]
mod tests {
    use super::*;

    use std::time::Duration;

    use shed_core::roost::testing::{ownership, FakeRoost};

    /// The tab the vendored `tab.list` vector carries — a plain `zsh` shell in
    /// project "Roost". Every test that wants a SESSION claims it first.
    const VECTOR_TAB: i64 = 5;
    const VECTOR_CWD: &str = "/Users/me/projects/roost";

    /// Poll `f` until it answers, or fail naming what never happened.
    ///
    /// **Polling the ASSERTION, not the watcher.** Since plan 014 nothing here
    /// has a cadence — a change arrives on roost's push feed — so this is only
    /// how a test observes an in-process snapshot that another task writes, and
    /// it is never a fixed sleep.
    async fn wait_for<T>(what: &str, mut f: impl FnMut() -> Option<T>) -> T {
        // Ten seconds, sampled every 5 ms. The sampling rate is deliberately
        // finer than the watcher's 500 ms first backoff step, so a test that
        // wants the FIRST `Down`'s reason (the stopping one, before a later
        // re-dial overwrites it) is reading a window it cannot plausibly miss.
        for _ in 0..2_000 {
            if let Some(value) = f() {
                return value;
            }
            tokio::time::sleep(Duration::from_millis(5)).await;
        }
        panic!("timed out waiting for {what}");
    }

    /// One machine's health, read straight out of the in-process state.
    fn machine_health(machines: &Machines, name: &str) -> (bool, Option<String>) {
        let guard = machines.state.lock().unwrap();
        let m = guard.get(name).expect("a registered machine");
        (m.reachable, m.detail.clone())
    }

    fn config_with(names: &[&str]) -> ShedConfig {
        ShedConfig {
            machines: names
                .iter()
                .map(|n| MachineEntry {
                    name: (*n).to_string(),
                    host: (*n).to_string(),
                    ssh_port: 22,
                    ..Default::default()
                })
                .collect(),
            ..Default::default()
        }
    }

    fn sockets(pairs: &[(&str, &std::path::Path)]) -> HashMap<String, PathBuf> {
        pairs
            .iter()
            .map(|(name, path)| ((*name).to_string(), path.to_path_buf()))
            .collect()
    }

    fn start(config: &ShedConfig, sockets: &HashMap<String, PathBuf>) -> Machines {
        Machines::start(
            &tokio::runtime::Handle::current(),
            config,
            sockets,
            Arc::new(|| {}),
        )
    }

    /// The vector's shell tab, claimed by an opencode adapter.
    fn claim_opencode(fake: &FakeRoost, lifecycle: &str, detail: &str, notify: bool) {
        fake.set_tab_axes(
            VECTOR_TAB,
            lifecycle,
            Some(ownership("opencode", "ses_abc", detail, 1_700_000_100)),
            notify,
        );
    }

    fn rows(machines: &Machines) -> Vec<Value> {
        machines.snapshot().0
    }

    fn status_named<'a>(status: &'a [Value], name: &str) -> Option<&'a Value> {
        status.iter().find(|m| m["name"] == json!(name))
    }

    /// A machine mapped to a live session lists its AGENT-OWNED tabs, with the
    /// kind, cwd and activity roost reported — and the origin stamps every card
    /// keys off.
    #[tokio::test]
    async fn a_mapped_machine_lists_its_agent_tabs() {
        let fake = FakeRoost::start().await;
        claim_opencode(&fake, "working", "session_status", false);

        let machines = start(
            &config_with(&["mini3"]),
            &sockets(&[("mini3", fake.socket_path())]),
        );

        let row = wait_for("mini3's opencode row", || {
            rows(&machines).into_iter().next()
        })
        .await;
        assert_eq!(row["kind"], json!("opencode"));
        assert_eq!(row["workdir"], json!(VECTOR_CWD));
        assert_eq!(row["activity"], json!("working"));
        assert_eq!(row["state"], json!("ready"), "roost tabs are always live");
        assert_eq!(row["slug"], json!("5"), "the slug IS the tab id");
        assert_eq!(row["tab_id"], json!("5"), "stamped as a string");
        assert_eq!(row["attention"], json!(false));
        assert_eq!(row["origin"], json!("machine:mini3"));
        assert_eq!(row["origin_kind"], json!("machine"));
        assert_eq!(row["machine"], json!("mini3"));
        assert_eq!(row["host"], json!("machine:mini3"));
        assert_eq!(row["shed"], json!(""), "a machine session has no shed");
        assert_eq!(row["stale"], json!(false));
        assert_eq!(row["tmux_session"], json!(""), "roost has no tmux");
        // The plain shell tab beside it is NOT a session (§3.2: a roost user with
        // fifteen terminals must not get fifteen cards) — the vector's only tab
        // became the agent one, so the count is the proof there is no second row.
        assert_eq!(rows(&machines).len(), 1);

        let status = machines.status();
        assert_eq!(status_named(&status, "mini3").unwrap()["reachable"], true);
        assert_eq!(status_named(&status, "mini3").unwrap()["sessions"], 1);
    }

    /// **The negative control for `a_mapped_machine_lists_its_agent_tabs`.** An
    /// UNMAPPED machine — the same code path, the same start, nothing to connect
    /// to — is a listed row with a reason and NO sessions. Without this a
    /// "machine lists its tabs" test that quietly listed every machine's tabs
    /// under every name would still pass.
    #[tokio::test]
    async fn an_unmapped_machine_is_an_unreachable_row_with_a_reason() {
        let fake = FakeRoost::start().await;
        let machines = start(
            &config_with(&["mini3", "ghost"]),
            &sockets(&[("mini3", fake.socket_path())]),
        );
        assert_eq!(machines.status().len(), 2, "both are listed at once");

        let detail = wait_for("ghost's reason for being unreachable", || {
            let status = machines.status();
            status_named(&status, "ghost")?["detail"]
                .as_str()
                .map(str::to_string)
        })
        .await;
        assert!(
            detail.contains("no roost-session mapped"),
            "the reason names the missing mapping, not a generic offline: {detail}"
        );

        let status = machines.status();
        let ghost = status_named(&status, "ghost").unwrap();
        assert_eq!(ghost["reachable"], json!(false));
        assert_eq!(ghost["connected_once"], json!(false));
        assert_eq!(ghost["sessions"], json!(0));
        assert_eq!(ghost["origin"], json!("machine:ghost"));
        assert!(
            rows(&machines).iter().all(|r| r["machine"] == "mini3"),
            "an unreachable machine contributes no rows"
        );
    }

    /// A lifecycle flip (and roost's sticky notification bit) reaches
    /// `snapshot()` off the **push feed** — the S3 acceptance cell, with no
    /// cadence anywhere to turn down.
    ///
    /// The `tab.list` count is the load-bearing half: exactly one per cycle
    /// means the flip arrived as a pushed batch that the inventory folded, not
    /// as a re-read that a poll happened to catch.
    #[tokio::test]
    async fn a_lifecycle_flip_is_pushed_into_the_snapshot() {
        let fake = FakeRoost::start().await;
        claim_opencode(&fake, "working", "session_status", false);
        let machines = start(
            &config_with(&["mini3"]),
            &sockets(&[("mini3", fake.socket_path())]),
        );
        wait_for("the first row", || {
            rows(&machines)
                .into_iter()
                .find(|r| r["activity"] == "working")
        })
        .await;
        assert_eq!(fake.tab_list_calls(), 1, "one list for the first cycle");

        // opencode's approval spelling, exactly — `permission_asked` is an
        // approval, `question_asked` would be plain input.
        claim_opencode(&fake, "waiting", "permission_asked", true);

        let row = wait_for("the flipped row", || {
            rows(&machines)
                .into_iter()
                .find(|r| r["activity"] == "needs_approval")
        })
        .await;
        assert_eq!(row["attention"], json!(true), "the sticky notification bit");
        assert_eq!(row["slug"], json!("5"), "the same tab, not a new row");
        assert_eq!(
            fake.tab_list_calls(),
            1,
            "the flip rode the event stream — a re-list would mean the watcher \
             still polls"
        );
    }

    /// **Somebody else taking the interactive lease changes nothing here.**
    ///
    /// At protocol 4 a takeover no longer ends an event stream; it reclassifies
    /// it and says so once with a non-terminal `session.driver_changed`. Shed
    /// never held the lease to begin with, so the rows must not move — and the
    /// stream must still be delivering, which the flip afterwards is what
    /// proves. (Two takeovers: the first mints into an unheld session and
    /// deposes nobody, so roost announces nothing; the second is the real one.)
    #[tokio::test]
    async fn a_driver_change_leaves_the_rows_alone_and_the_stream_alive() {
        let fake = FakeRoost::start().await;
        claim_opencode(&fake, "working", "session_status", false);
        let machines = start(
            &config_with(&["mini3"]),
            &sockets(&[("mini3", fake.socket_path())]),
        );
        let before = wait_for("the first row", || rows(&machines).into_iter().next()).await;
        let lists_before = fake.tab_list_calls();

        fake.take_over("roost ui");
        fake.take_over("somebody else");

        // Nothing to wait FOR — the assertion is that nothing happens — so give
        // the frame time to be delivered and mishandled before reading.
        tokio::time::sleep(Duration::from_millis(100)).await;
        assert_eq!(rows(&machines), vec![before], "a takeover moved a row");
        assert_eq!(
            machine_health(&machines, "mini3"),
            (true, None),
            "a takeover is not a reason to call a machine down"
        );
        assert_eq!(
            fake.tab_list_calls(),
            lists_before,
            "a takeover is informational — it must not cost a resync"
        );

        // The stream survived it: the next commit still arrives.
        claim_opencode(&fake, "finished", "session_idle", false);
        let after = wait_for("the flip after the takeover", || {
            rows(&machines)
                .into_iter()
                .find(|r| r["activity"] == "idle")
        })
        .await;
        assert_eq!(after["slug"], json!("5"));
        assert_eq!(
            fake.tab_list_calls(),
            lists_before,
            "and it was still the SAME stream, not a reconnect"
        );
    }

    /// **A daemon that stops says why, and the row recovers when it comes back.**
    ///
    /// `session.stopping` is the one terminal envelope an event stream sees at
    /// protocol 4, and it is the reason the user reads. The last known rows stay
    /// on screen, marked stale — a machine going away must never blank the view.
    #[tokio::test]
    async fn a_stopping_session_goes_stale_with_its_reason_and_then_recovers() {
        let fake = FakeRoost::start().await;
        claim_opencode(&fake, "working", "session_status", false);
        let machines = start(
            &config_with(&["mini3"]),
            &sockets(&[("mini3", fake.socket_path())]),
        );
        wait_for("the first row", || rows(&machines).into_iter().next()).await;

        // Latches the fake unavailable, so "further attempts fail" is real.
        fake.stop();

        let reason = wait_for("mini3 to report why it went down", || {
            machine_health(&machines, "mini3").1
        })
        .await;
        assert!(
            reason.contains("session stopping: stop"),
            "the FIRST reason after a stop is roost's own, not a dial failure: {reason}"
        );
        let stale = rows(&machines);
        assert_eq!(stale.len(), 1, "the last known rows survive the stop");
        assert_eq!(stale[0]["stale"], json!(true));

        fake.restart();
        wait_for("mini3 to come back", || {
            machine_health(&machines, "mini3").0.then_some(())
        })
        .await;
        let back = rows(&machines);
        assert_eq!(back.len(), 1, "the row set is re-listed, not replayed");
        assert_eq!(
            back[0]["slug"],
            json!("5"),
            "tab ids persist across a restart"
        );
        assert_eq!(back[0]["stale"], json!(false));
    }

    /// **A lost commit resyncs, and the row never flickers stale.**
    ///
    /// `skip_revision` is the only way to manufacture the loss a resync exists
    /// for (roost itself closes the stream instead). The watcher answers with a
    /// whole new cycle — one more `tab.list` — and NO `Down`, so the UI sees a
    /// row that simply updates.
    #[tokio::test]
    async fn a_revision_gap_resyncs_without_a_stale_flicker() {
        let fake = FakeRoost::start().await;
        claim_opencode(&fake, "working", "session_status", false);
        let machines = start(
            &config_with(&["mini3"]),
            &sockets(&[("mini3", fake.socket_path())]),
        );
        wait_for("the first row", || {
            rows(&machines)
                .into_iter()
                .find(|r| r["activity"] == "working")
        })
        .await;
        let lists_before = fake.tab_list_calls();

        // A commit nobody was told about, then one they are: the batch arrives
        // at `expected + 1` and the client's own stream raises the gap.
        fake.skip_revision();
        claim_opencode(&fake, "finished", "session_idle", false);

        let row = wait_for("the resynced row", || {
            // Checked on every sample, not once at the end: a `Down` between
            // the gap and the recovery is exactly the flicker this rules out,
            // and it would be gone again by the time the loop finished.
            assert_eq!(
                machine_health(&machines, "mini3"),
                (true, None),
                "a resync must never render the machine down"
            );
            rows(&machines)
                .into_iter()
                .find(|r| r["activity"] == "idle")
        })
        .await;
        assert_eq!(row["slug"], json!("5"));
        assert_eq!(row["stale"], json!(false));
        assert_eq!(
            fake.tab_list_calls(),
            lists_before + 1,
            "exactly one re-list — a resync is a fresh cycle, not a retry storm"
        );
    }

    /// Every `(reachable, detail)` pair the machine layer published, in order.
    ///
    /// Named because clippy asks, but the name earns its place: this is a
    /// history, not a sample, and that distinction is the whole point of the
    /// test below.
    type PublishedStates = Arc<Mutex<Vec<(bool, Option<String>)>>>;

    /// **No unreachable state is ever PUBLISHED across a resync** — the claim a
    /// periodically-sampled test can only approximate.
    ///
    /// [`consume`] calls `on_change` after every update it applies, so a
    /// callback that reads the row back sees EVERY transition the UI would have
    /// been told about — including one a poll-and-compare test cannot see at
    /// all, because a spurious `Down` followed microseconds later by a fresh
    /// snapshot looks exactly like no `Down` at any sampling rate. The harness's
    /// end-to-end cell samples; this one is the actual assertion.
    ///
    /// Both resyncs a healthy daemon produces are exercised: a revision gap
    /// (a commit the stream never carried) and a reorder (which
    /// `shed_app::roost` answers with a deliberate re-list). Neither may render
    /// the machine unreachable, because the daemon was never down.
    #[tokio::test]
    async fn a_resync_never_publishes_an_unreachable_state() {
        let fake = FakeRoost::start().await;
        claim_opencode(&fake, "working", "session_status", false);

        // The consumer under test, wired by hand so the callback can be one that
        // RECORDS rather than one that repaints.
        let state: Arc<Mutex<BTreeMap<String, MachineState>>> = Arc::new(Mutex::new(
            BTreeMap::from([("mini3".to_string(), MachineState::new(true))]),
        ));
        let history: PublishedStates = Arc::new(Mutex::new(Vec::new()));
        let on_change: OnChange = {
            let state = Arc::clone(&state);
            let history = Arc::clone(&history);
            Arc::new(move || {
                let guard = lock(&state);
                if let Some(m) = guard.get("mini3") {
                    history
                        .lock()
                        .unwrap()
                        .push((m.reachable, m.detail.clone()));
                }
            })
        };
        let reach: Arc<dyn RoostReach> = Arc::new(LocalSession::new("mini3", fake.socket_path()));
        let (watcher, rx) = RoostWatcher::spawn(
            &tokio::runtime::Handle::current(),
            reach,
            "mini3".to_string(),
        );
        tokio::spawn(consume(
            "mini3".to_string(),
            rx,
            Arc::clone(&state),
            on_change,
        ));

        fn activity(state: &Arc<Mutex<BTreeMap<String, MachineState>>>) -> Option<Value> {
            let guard = lock(state);
            let m = guard.get("mini3")?;
            let session = m.sessions.first()?;
            Some(machine_row("mini3", session, !m.reachable)["activity"].clone())
        }

        wait_for("the first snapshot", || {
            activity(&state).filter(|a| a == &json!("working"))
        })
        .await;

        // A commit the stream never carried: the client's own event stream
        // raises the gap and the watcher answers with a whole new cycle.
        let lists = fake.tab_list_calls();
        fake.skip_revision();
        claim_opencode(&fake, "finished", "session_idle", false);
        wait_for("the gap to resync", || {
            activity(&state).filter(|a| a == &json!("idle"))
        })
        .await;
        assert_eq!(fake.tab_list_calls(), lists + 1, "one re-list per resync");

        // A reorder: applied, and then deliberately re-listed for the new order.
        fake.reorder_tabs();
        wait_for("the reorder to re-list", || {
            (fake.tab_list_calls() > lists + 1).then_some(())
        })
        .await;

        let published = history.lock().unwrap().clone();
        assert!(!published.is_empty(), "nothing was ever published");
        assert!(
            published.iter().all(|(reachable, _)| *reachable),
            "a resync published an unreachable state: {published:?}"
        );
        watcher.stop();
    }

    /// The implicit `localhost` host is INVISIBLE until a session has answered —
    /// connect-if-present in both directions. The watcher still runs (it has a
    /// reason recorded), it is simply not something the user is shown.
    #[tokio::test]
    async fn localhost_is_absent_until_its_session_answers() {
        // A mapped-but-nonexistent socket: the reach exists, the session does not.
        let missing = std::env::temp_dir().join("shed-tauri-no-such-roost.sock");
        let machines = start(
            &config_with(&["mini3"]),
            &sockets(&[("localhost", &missing)]),
        );

        wait_for("localhost's watcher to report", || {
            let guard = machines.state.lock().unwrap();
            guard.get(LOCALHOST)?.detail.clone()
        })
        .await;

        assert!(
            status_named(&machines.status(), LOCALHOST).is_none(),
            "a host that has never run a session is not listed"
        );
        assert!(rows(&machines).iter().all(|r| r["machine"] != "localhost"));
        // ...but it IS registered, so a verb addressed at it is not "unknown".
        assert!(machines.capabilities(LOCALHOST).is_ok());
    }

    /// Once `localhost`'s session has answered it is listed with its rows — and
    /// it STAYS listed when the session goes away, as an ordinary unreachable row
    /// keeping its last known sessions.
    #[tokio::test]
    async fn localhost_is_listed_once_its_session_answers_and_stays() {
        let fake = FakeRoost::start().await;
        claim_opencode(&fake, "finished", "session_idle", false);
        let fake_socket = fake.socket_path().to_path_buf();
        let machines = start(&config_with(&[]), &sockets(&[(LOCALHOST, &fake_socket)]));

        let row = wait_for("the localhost row", || rows(&machines).into_iter().next()).await;
        assert_eq!(row["origin"], json!("machine:localhost"));
        assert_eq!(row["activity"], json!("idle"));
        assert_eq!(row["stale"], json!(false));
        assert_eq!(
            status_named(&machines.status(), LOCALHOST).unwrap()["reachable"],
            json!(true)
        );

        // DROP rather than `close_all`: a hang-up is transient (the socket is
        // still there, so the next dial succeeds and the row would flicker back),
        // and what this asserts is the DURABLE gone state. Dropping the fake takes
        // its scratch directory — and the socket — with it.
        drop(fake);

        // The FIRST `Down` is the held connection dying ("Connection reset by
        // peer"), which is true but transient; the SETTLED reason — what the row
        // keeps saying while the session stays gone — is the reach's, and it names
        // the socket. Waiting for that is the assertion worth making.
        let detail = wait_for("localhost to settle on the socket-gone reason", || {
            let status = machines.status();
            status_named(&status, LOCALHOST)?["detail"]
                .as_str()
                .filter(|d| d.contains("no roost-session at"))
                .map(str::to_string)
        })
        .await;
        assert!(
            detail.contains(fake_socket.to_str().unwrap()),
            "the reason names the socket that is gone: {detail}"
        );
        let after = rows(&machines);
        assert_eq!(after.len(), 1, "the last known rows survive the disconnect");
        assert_eq!(after[0]["stale"], json!(true));
        assert_eq!(
            status_named(&machines.status(), LOCALHOST).unwrap()["connected_once"],
            json!(true),
            "still listed — the host is real, it is just not answering"
        );
    }

    /// `localhost` is reserved: the implicit host and a configured one must never
    /// both exist under the name.
    #[tokio::test]
    async fn add_refuses_the_reserved_localhost_name() {
        let machines = start(
            &config_with(&[]),
            &sockets(&[("x", std::path::Path::new("/nope"))]),
        );
        let e = machines
            .add(MachineEntry {
                name: LOCALHOST.to_string(),
                host: "example.internal".to_string(),
                ssh_port: 22,
                ..Default::default()
            })
            .expect_err("localhost is refused");
        assert!(e.contains("always present"), "{e}");
        assert_eq!(
            machines
                .status()
                .iter()
                .filter(|m| m["name"] == json!(LOCALHOST))
                .count(),
            0,
            "the refusal registered nothing"
        );
    }

    /// A configured entry named `localhost` WINS — the implicit host is not
    /// registered beside it, so the name resolves to exactly one reach.
    #[tokio::test]
    async fn a_configured_localhost_wins_over_the_implicit_one() {
        let fake = FakeRoost::start().await;
        let machines = start(
            &config_with(&[LOCALHOST]),
            &sockets(&[(LOCALHOST, fake.socket_path())]),
        );
        let reg = machines.reg.lock().unwrap();
        assert_eq!(reg.names, vec![LOCALHOST.to_string()]);
    }

    /// `kill` is a roost `tab.close`: the tab really leaves the session (a second
    /// close of the same id is refused by the daemon), and the row is dropped
    /// optimistically rather than waiting for the close's own `tab.closed` to
    /// come back off the stream.
    #[tokio::test]
    async fn kill_routes_to_tab_close() {
        let fake = FakeRoost::start().await;
        claim_opencode(&fake, "working", "session_status", false);
        let machines = start(
            &config_with(&["mini3"]),
            &sockets(&[("mini3", fake.socket_path())]),
        );
        wait_for("the row to kill", || rows(&machines).into_iter().next()).await;

        machines.kill("mini3", "5").await.expect("the close lands");
        assert!(rows(&machines).is_empty(), "the row drops optimistically");

        // The tab is GONE from the session, not just from our snapshot: roost
        // refuses a second close by name.
        let again = machines
            .kill("mini3", "5")
            .await
            .expect_err("the tab is already closed");
        assert!(
            again.contains("not-found") || again.contains("no such tab"),
            "{again}"
        );

        let bad = machines
            .kill("mini3", "rc-abc123")
            .await
            .expect_err("a non-roost slug is refused");
        assert!(bad.contains("not a roost tab id"), "{bad}");
    }

    /// The `tab.open` request for a kind: the agent's argv and the cwd, and
    /// nothing else invented.
    #[test]
    fn open_params_carry_the_kinds_argv_and_nothing_else() {
        let params = open_params(&RcKind::Opencode, Some("/tmp/work")).expect("a launchable kind");
        assert_eq!(params.argv, vec!["opencode".to_string()]);
        assert_eq!(params.cwd, "/tmp/work");
        assert_eq!(params.project_id, 0, "roost picks the project");
        assert_eq!((params.cols, params.rows), (0, 0), "roost sizes the PTY");
        assert_eq!(params.title, "", "the title is roost's");

        assert_eq!(
            open_params(&RcKind::ClaudeRc, None).unwrap().argv,
            vec!["claude".to_string()]
        );
        let e = open_params(&RcKind::Other("borg".into()), None).expect_err("no recipe");
        assert!(e.contains("borg"), "the refusal names the kind: {e}");
    }

    /// `launch` opens a tab on the session and answers with the row for it,
    /// carrying the kind that was ASKED for (the adapter has not claimed the fresh
    /// tab yet, so roost would report it as a plain shell).
    #[tokio::test]
    async fn launch_routes_to_tab_open() {
        let fake = FakeRoost::start().await;
        let machines = start(
            &config_with(&["mini3"]),
            &sockets(&[("mini3", fake.socket_path())]),
        );
        wait_for("the first snapshot", || {
            machines
                .state
                .lock()
                .unwrap()
                .get("mini3")
                .filter(|m| m.seen)
                .map(|_| ())
        })
        .await;

        let row = machines
            .launch(
                "mini3",
                &RcKind::Opencode,
                None,
                Some("  /tmp/x  "),
                None,
                None,
            )
            .await
            .expect("the open lands");
        assert_eq!(
            row["kind"],
            json!("opencode"),
            "the kind that was asked for"
        );
        assert_eq!(row["workdir"], json!("/tmp/x"), "trimmed");
        assert_eq!(row["origin"], json!("machine:mini3"));
        assert_eq!(row["slug"], json!("6"), "the id the fake's next tab gets");
        // And it is in the snapshot immediately, rather than when the stream
        // delivers the `tab.opened` this call just caused.
        assert!(rows(&machines).iter().any(|r| r["slug"] == "6"));
    }

    /// A kind roost has no recipe for is refused BEFORE anything is opened — the
    /// session's revision does not move.
    #[tokio::test]
    async fn an_unknown_kind_is_rejected_without_opening_a_tab() {
        let fake = FakeRoost::start().await;
        let machines = start(
            &config_with(&["mini3"]),
            &sockets(&[("mini3", fake.socket_path())]),
        );
        let before = fake.revision();
        let e = machines
            .launch(
                "mini3",
                &RcKind::Other("borg".into()),
                None,
                None,
                None,
                None,
            )
            .await
            .expect_err("no launch recipe");
        assert!(e.contains("borg"), "{e}");
        assert_eq!(fake.revision(), before, "nothing was opened");
    }

    /// Capabilities are the synthesized roost contract — no probe, no SSH, and an
    /// answer even for a machine that is asleep. An unknown machine is still an
    /// error.
    #[tokio::test]
    async fn capabilities_are_synthesized_not_probed() {
        let machines = start(
            &config_with(&["ghost"]),
            &sockets(&[("nothing", std::path::Path::new("/nope"))]),
        );
        let caps = machines
            .capabilities("ghost")
            .expect("an unreachable machine still has capabilities");
        assert_eq!(caps["rc_version"], json!(2));
        assert_eq!(
            caps["kind_features"]["opencode"]["attach"],
            json!("native-remote"),
            "the desktop shows no terminal action for a roost row"
        );

        let e = machines.capabilities("nope").expect_err("unknown machine");
        assert!(e.contains("no machine \"nope\""), "{e}");
    }
}
