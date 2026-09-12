//! **Roost hosts in the desktop app** — the machines and the sheds whose
//! `roost-session` this app reads (plan 013 S3 for machines; plan 019 §3.6 for
//! sheds).
//!
//! `machines:` has lived in `shed-core`'s config since plan 009. Until plan 013
//! this module read each machine's **RC hub** over an `ssh -N -L` forward; it now
//! reads the host's **`roost-session`** directly, because that is the
//! substrate the pivot is moving to. The reach itself lives in the shared layer:
//!
//! * the roost wire (`session.identify`, `tab.list`, `tab.open/close`) →
//!   [`shed_core::roost`]
//! * the transport seam + the observer watcher → [`shed_app::roost`]
//! * the bootstrap choreography → [`shed_core::roost::bootstrap`], driven by
//!   [`shed_app::roost::BootstrapRunner`]
//!
//! What is left here is what a desktop app actually owns: which hosts exist, one
//! watcher per host, the last inventory each one reported, whether it is
//! currently reachable, and the four ops (`roost.probe` / `preview` /
//! `bootstrap` / `launch`) a user's click lands in.
//!
//! ## Two kinds of host, one registry
//!
//! A [`HostId`] is either a `machines:` entry or a shed, and it carries plan 019
//! §0's **target grammar** — `machine:<name>` and `roost:<server>/<shed>` — which
//! is the same word every row origin, capabilities key, log line and IPC
//! parameter uses. It replaced a bare `String` keyed on a machine name, which
//! could not tell a shed named `mini3` from the machine named `mini3` and had no
//! way to say which server a shed belonged to.
//!
//! Sheds are **probed, not watched by default** (see
//! [`RoostHosts::observe_sheds`]): a watcher is a live `ssh` client-bridge, and
//! opening one per running shed on the off-chance would be a connection storm
//! against hosts that mostly run no session at all.
//!
//! ## Unreachable is a STATE, not an error
//!
//! A host that is asleep, off the network, or simply runs no `roost-session`
//! is the normal case, not a failure. Every configured machine therefore always
//! has a row; `reachable` and `detail` say how much to trust it. Nothing here
//! returns an error to the UI for a host being down.
//!
//! ## The implicit `localhost` host
//!
//! The one machine a user always has is the one they are sitting at, and it needs
//! no config entry: when nothing in `machines:` is named `localhost`, this module
//! registers a [`LocalSession`](shed_app::roost::LocalSession) reach under that
//! name. It follows roost's connect-if-present rule in both directions — **a
//! `localhost` whose socket has never existed in this process is not listed at
//! all** ([`HostState::listed`]), because a host that has never run a session is
//! not a thing the user asked about. Once one has answered, the host stays listed
//! and a later disappearance is an ordinary unreachable row with the reason,
//! exactly like a configured machine that went to sleep.
//!
//! A configured entry named `localhost` WINS (the user said what they meant), and
//! [`RoostHosts::add`] refuses the name so the two can never both exist.
//!
//! ## One overlay per feed
//!
//! Sessions are held per host and never merged into a shared activity overlay.
//! Roost reports no shed (there is none), so `(shed, slug)` — the key
//! [`shed_core::rc_events::ActivityOverlay`] uses — would collide across two
//! hosts whose tab ids happen to match. Rows are keyed by ORIGIN + slug here
//! instead.

use std::collections::{BTreeMap, BTreeSet};
use std::sync::{Arc, Mutex, MutexGuard};
use std::time::{Duration, Instant};

use serde_json::{json, Value};

use roost_ipc::agent::Ownership;
use roost_ipc::messages::{Tab, TabOpenParams};
use shed_app::roost::{
    launch_argv, roost_capabilities, shed_reach_entry, tab_close, tab_open, BootstrapRunner,
    ReachKind as ReachFamily, RoostLeases, RoostReach, RoostUpdate, RoostWatcher, SshExec,
};
use shed_core::config::{MachineEntry, ShedConfig};
use shed_core::rc::RcKind;
use shed_core::roost::bootstrap::{
    self, BootstrapFailure, HooksResult, InstallRequest, Installed, Plan, Probe, ProbeOutcome,
    SessionState, SourceEnv, Stage,
};
use shed_core::roost::{AgentLaneStamp, Conn, RoostSession};

use crate::machines::{
    build_local_reach, build_ssh_reach, reject_reserved_name, ReachKind, ReachOptions, Registered,
    LOCALHOST,
};

/// Why a roost row offers no terminal, in the words BOTH doors answer with —
/// the `terminal.open`/`terminal.preview` IPC ops and the `open_terminal` Tauri
/// command (plan 013 S3). One string because it is one rule: two copies would
/// drift and only one of them would be under the harness's eye.
pub const NO_TERMINAL: &str = "terminal unavailable: attach is native-remote";

/// Who shed says it is on roost's wire — the lease label and
/// `session.set_agent_hooks`'s `client` (plan 019 §3.4). The phone's twin is
/// `shed-mobile`.
pub const CLIENT_LABEL: &str = "shed-desktop";

/// How long one shed's "is anything serving over there?" probe may take.
///
/// Deliberately short: it runs for every running shed on every authoritative
/// refresh, and a host that is asleep must cost the refresh nothing. A shed that
/// answers slower than this is simply probed again on the next refresh — the
/// probe is idempotent and free of side effects, which is what makes giving up
/// early the cheap option rather than a lost result.
const SHED_PROBE_BUDGET: Duration = Duration::from_secs(8);

/// How many shed probes may be in flight at once.
///
/// Each one is an `ssh` exec. A user with thirty running sheds across four
/// servers would otherwise fork thirty `ssh` children in one breath on every
/// refresh, which is a worse citizen than a slightly slower discovery.
const SHED_PROBE_CONCURRENCY: usize = 4;

/// How long a shed that did NOT answer is left alone before it is probed again.
///
/// **What turns "probe on every refresh" into something a UI can poll.** A
/// refresh is not a rare event — the dashboard runs one on a timer, the shed card
/// runs one per open, and the harness runs one per assertion poll — and each one
/// asks `observe_sheds` about every running shed. Without a floor, a shed with no
/// session gets an `ssh` exec per refresh: at ten polls a second that is a fork
/// storm against a host whose answer has not changed and will not change until
/// somebody starts a session there.
///
/// Plan 019 §3.6 pins the triggers as "on each sheds refresh", "on the user's
/// action" and "when the shed list changes"; this is what makes the first of
/// those cheap while keeping the other two immediate — a shed that has never been
/// probed (a NEW one in the list) has no entry here and is probed at once, and a
/// [`RoostHosts::remove`] forgets the entry so a shed that stops and starts again
/// is too.
const SHED_PROBE_COOLDOWN: Duration = Duration::from_secs(30);

/// Take a lock, ignoring poisoning.
///
/// Every mutex here guards plain data (a name list, a row cache) that a panicking
/// holder cannot leave half-updated in a way the next reader would misread. The
/// alternative — unwrapping — turns one unrelated panic into a permanently dead
/// host layer, which is strictly worse than reading a slightly stale row.
pub(crate) fn lock<T>(m: &Mutex<T>) -> MutexGuard<'_, T> {
    m.lock().unwrap_or_else(|e| e.into_inner())
}

// ---------------------------------------------------------------------------
// the typed id
// ---------------------------------------------------------------------------

/// One roost host, in plan 019 §0's **target grammar**.
///
/// Two kinds, because there are two: an entry the user wrote under `machines:`,
/// and a shed on one of their servers. The grammar — `machine:<name>` and
/// `roost:<server>/<shed>` — is not a display detail. It is the origin a row is
/// stamped with, the key its capabilities are filed under, the word a log line
/// uses and the `target` every `roost.*` op takes, and having ONE spelling is
/// what lets a client move from a row to an op without a lookup table.
///
/// **A shed's is deliberately distinct from the hub's `<server>/<shed>`.** Both
/// rows are about the same shed, and under the union rule (plan 019 §3.6) they
/// sit side by side in one payload; an origin that collided would make the two
/// indistinguishable to every consumer that keys on it.
#[derive(Debug, Clone, PartialEq, Eq, PartialOrd, Ord, Hash)]
pub enum HostId {
    /// A `machines:` entry, by its bare name.
    Machine(String),
    /// A shed, by the server it lives on and its own name.
    Shed { server: String, name: String },
}

impl HostId {
    /// The grammar's word for this host — what every row, key and op says.
    pub fn token(&self) -> String {
        match self {
            HostId::Machine(name) => format!("machine:{name}"),
            HostId::Shed { server, name } => format!("roost:{server}/{name}"),
        }
    }

    /// `machine` or `shed` — what a row's `origin_kind` says.
    pub fn kind(&self) -> &'static str {
        match self {
            HostId::Machine(_) => "machine",
            HostId::Shed { .. } => "shed",
        }
    }

    /// **The word every caller addresses this host by** — a machine's bare name,
    /// a shed's full grammar token.
    ///
    /// The one place the asymmetry is decided, because it has to be the same in
    /// four places at once: a row's `machine` field, a status row's `name`, the
    /// key the lane layer files an entry under, and the parameter `machine.kill`
    /// / `lane.open` / `roost.launch` take. A machine's is bare because it has
    /// been bare since plan 013 and every existing caller sends it that way; a
    /// shed's cannot be, because a shed name alone does not say which server.
    ///
    /// [`HostId::parse`] reads both back, which is what makes this safe to hand
    /// out as an address.
    pub fn address(&self) -> String {
        match self {
            HostId::Machine(name) => name.clone(),
            HostId::Shed { .. } => self.token(),
        }
    }

    /// The bare name a human reads: the machine's, or the shed's.
    pub fn label(&self) -> &str {
        match self {
            HostId::Machine(name) => name,
            HostId::Shed { name, .. } => name,
        }
    }

    /// The server a shed lives on; `None` for a machine, which lives on none.
    pub fn server(&self) -> Option<&str> {
        match self {
            HostId::Machine(_) => None,
            HostId::Shed { server, .. } => Some(server),
        }
    }

    /// Read a token back — **tolerantly**, because the two doors into this layer
    /// speak slightly different dialects and always have.
    ///
    /// `machine:<name>` and `roost:<server>/<shed>` are the grammar. A bare word
    /// is a MACHINE, which is not a guess: `machine.kill`, `machine.launch`,
    /// `lane.open` and every row's `machine` field have carried a bare machine
    /// name since plan 013, and an id that refused them would break every
    /// existing caller to no purpose.
    ///
    /// A `roost:` token with no `/` has no server, and a server is not
    /// optional — a shed name alone does not identify a shed — so it is refused
    /// rather than defaulted.
    pub fn parse(raw: &str) -> Result<HostId, String> {
        let raw = raw.trim();
        if raw.is_empty() {
            return Err("an empty host id names nothing".to_string());
        }
        if let Some(rest) = raw.strip_prefix("roost:") {
            let (server, name) = rest
                .split_once('/')
                .ok_or_else(|| format!("{raw:?} names no server (want roost:<server>/<shed>)"))?;
            if server.is_empty() || name.is_empty() {
                return Err(format!(
                    "{raw:?} names no server (want roost:<server>/<shed>)"
                ));
            }
            return Ok(HostId::Shed {
                server: server.to_string(),
                name: name.to_string(),
            });
        }
        if let Some(name) = raw.strip_prefix("machine:") {
            if name.is_empty() {
                return Err(format!("{raw:?} names no machine"));
            }
            return Ok(HostId::Machine(name.to_string()));
        }
        Ok(HostId::Machine(raw.to_string()))
    }
}

impl std::fmt::Display for HostId {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(&self.token())
    }
}

/// Called whenever a host's state changes, so the embedder can tell its UI
/// to re-read. Without it the app would only ever show the state it happened to
/// fetch at mount: a host that comes up (or drops) later changes nothing the
/// frontend is watching, and the rows sit stale until a manual Refresh.
pub type OnChange = Arc<dyn Fn() + Send + Sync>;

/// Called with one host's CURRENT set of agent lanes (`session_id` →
/// [`AgentLaneStamp`]) every time a fresh roost snapshot replaces its row set.
///
/// This is the eviction signal for [`crate::lane::Lanes`] (plan 015 §3.4: an
/// entry is dropped "when the tab disappears from the roost snapshot, or when a
/// row's `server_url` changes"). It hangs off a SNAPSHOT and not off a `Down`
/// on purpose: a snapshot is authoritative and replaces the row set outright, so
/// "absent from this map" genuinely means the tab is gone, whereas a `Down` is a
/// machine that went quiet with its last rows still on screen.
///
/// **A tab that dies before any adapter claims it reaches here too.** roost
/// publishes a snapshot when a tab the inventory knew about stops existing, even
/// if nothing else about it changed (plan 014's ghost-row fix) — without that,
/// a launched process that died young would leave a lane entry, and its `ssh -N`
/// child, behind a row nobody can see.
pub type OnLanes = Arc<dyn Fn(&str, &BTreeMap<String, AgentLaneStamp>) + Send + Sync>;

/// One host's live view, as the UI reads it.
struct HostState {
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
    /// **What kind of unreachable**, from the transport's own classification
    /// (plan 019 §3.6). `detail` is for the user; this is what a client branches
    /// on: `not-installed` offers an install, `no-session` offers a start, and
    /// the other two offer nothing.
    ///
    /// `None` while reachable, and never inferred from `detail` — a substring
    /// search over a sentence is exactly what [`shed_app::roost::ReachError`]
    /// exists to remove.
    down_kind: Option<ReachFamily>,
    /// Whether a snapshot has EVER arrived, so the UI can distinguish "still
    /// connecting" from "connected, and this host genuinely has no sessions".
    seen: bool,
    /// Whether this host may appear in a listing at all.
    ///
    /// `true` from the start for every CONFIGURED machine — the user named it, so
    /// its row (unreachable or not) is the information. `false` until the first
    /// snapshot for the implicit [`LOCALHOST`] host, which nobody asked for: a
    /// machine that has never run a `roost-session` should show no roost UI at
    /// all, and for every SHED, whose roost host exists only once a session has
    /// answered there (plan 019 §3.6's probe-then-watch). Once flipped it stays
    /// flipped, so a session that stops leaves a normal unreachable row rather
    /// than making the host vanish mid-look.
    listed: bool,
}

impl HostState {
    fn new(listed: bool) -> Self {
        Self {
            sessions: Vec::new(),
            reachable: false,
            detail: None,
            down_kind: None,
            seen: false,
            listed,
        }
    }
}

/// The app's roost-host layer: one watcher per live host, plus the state each
/// reports, plus the bootstrap that puts a session on one that has none.
pub struct RoostHosts {
    /// Keyed by the typed id, which is also the row origin.
    state: Arc<Mutex<BTreeMap<HostId, HostState>>>,
    /// The hosts this app knows about, and the watchers keeping them live.
    ///
    /// Behind a lock because the set GROWS *and shrinks*: adding a machine has
    /// to start watching it now, not on the next launch, and a shed that stops
    /// has to lose its watcher without waiting for a relaunch.
    reg: Arc<Mutex<Registry>>,
    /// Kept so a host registered later gets a watcher on the same runtime, with
    /// the same test-mode reach substitution and the same change callback as
    /// the ones started at boot — one code path, not two.
    handle: tokio::runtime::Handle,
    /// The config as the app read it at launch: the servers a shed's reach is
    /// composed from, and the `machines:` entries.
    ///
    /// A snapshot rather than a re-read per op, for the reason
    /// [`Registry::reaches`] gives about control verbs: a `machines:` file
    /// edited mid-session must not silently re-point the host the rows on screen
    /// belong to.
    config: ShedConfig,
    reach_options: ReachOptions,
    /// roost's `jail_fs_root`, for the bootstrap machines. **`false` in
    /// production** — see [`crate::env::Env::roost_jail_fs_root`].
    jail_fs_root: bool,
    /// The `ssh` ControlMaster runners the bootstrap's `Exec` steps go out on,
    /// one per host, built on first use and kept so a probe and the install
    /// that follows it share a handshake.
    ///
    /// Their `Drop` removes the scratch directory and asks the master to exit;
    /// [`RoostHosts::shutdown`] is the same teardown for app quit, which is the
    /// one path where `Drop` alone would be too late (plan 019 §3.6).
    execs: Mutex<BTreeMap<HostId, Arc<SshExec>>>,
    /// The interactive leases shed holds, one per target, for the app run.
    leases: Arc<RoostLeases>,
    /// Sheds a probe is in flight for, so a second refresh landing while the
    /// first is still dialling does not fork a second `ssh` for the same host.
    probing: Arc<Mutex<BTreeSet<HostId>>>,
    /// When each shed was last probed, for [`SHED_PROBE_COOLDOWN`].
    probed: Arc<Mutex<BTreeMap<HostId, Instant>>>,
    /// The shed-probe concurrency bound.
    probe_slots: Arc<tokio::sync::Semaphore>,
    on_change: OnChange,
    /// The lane layer's reconcile hook, installed after construction (plan 015
    /// §3.4).
    ///
    /// Late-bound rather than a constructor argument because the lane layer
    /// holds an `Arc<RoostHosts>` of its own: the two would otherwise have to be
    /// built at the same instant. `None` in every context that has no lanes —
    /// the unit tests, and any embedder that never opens one.
    on_lanes: Arc<Mutex<Option<OnLanes>>>,
}

/// The mutable half of [`RoostHosts`]: the registered set and its live watchers.
///
/// `ids` carries ORDER (config order, then arrival order) because the UI lists
/// hosts in it, and it is also the membership set a duplicate `add` is checked
/// against — the implicit [`LOCALHOST`] host has no [`MachineEntry`], so a map
/// keyed by entry would not see it.
struct Registry {
    ids: Vec<HostId>,
    /// The reach each host is addressed through, keyed by id.
    ///
    /// Control verbs resolve through this rather than re-deriving one from the
    /// config, so a kill can never address a different host than the row the user
    /// is looking at: if `machines:` is edited to repoint `mini3` mid-session, the
    /// watcher (and therefore the displayed rows) still belong to the reach that
    /// was built at start, and the kill must follow the rows.
    ///
    /// **One reach per host, watched or not.** A probe, a launch and a bootstrap
    /// all resolve through here, and building a second [`SshBridge`] beside a live
    /// one would hit roost's own "another Roost owns this target" refusal — its
    /// `open` sweeps the host's older scratch directories and will not reclaim one
    /// whose `bridge.sock` still answers.
    ///
    /// A host whose reach could not even be BUILT is absent here but present in
    /// `ids` — it is a listed, permanently-unreachable row.
    reaches: BTreeMap<HostId, Registered>,
    /// The live watchers, keyed so one can be dropped on its own: a shed that
    /// stopped loses its watcher without waiting for a relaunch (plan 019 §3.6).
    /// Dropping one aborts its loop.
    watchers: BTreeMap<HostId, RoostWatcher>,
}

impl Registry {
    /// Take a freshly spawned watcher for `id`, or refuse it and let it drop.
    ///
    /// **The removal race.** Spawning a watcher cannot happen under this lock's
    /// first acquisition (see [`RoostHosts::watch`]): the reach is read, the lock
    /// is released, the watcher is spawned, and the lock is taken again to
    /// register it. A [`RoostHosts::remove`] landing in that gap has already
    /// dropped the reach, the watcher, the id and the row — so an insert that
    /// only asked "did somebody else win?" would leave a watcher publishing
    /// state for a shed that has stopped, or is gone. The concrete ordering: a
    /// shed probe answers and clones the reach, an authoritative sheds refresh
    /// removes the shed, and the probe resumes and installs its watcher into a
    /// registry that no longer knows the host.
    ///
    /// So the host must still be here — and be reachable through the SAME reach
    /// this watcher was spawned against. A remove followed by a fresh
    /// [`RoostHosts::ensure_registered`] (the shed came back) re-registers the id
    /// with a NEW bridge, and a watcher holding the torn-down one would report
    /// that host as permanently down.
    ///
    /// Refusing DROPS `watcher` right here, which aborts its loop — the same stop
    /// `remove` performs, and the same one the lost-race arm has always relied
    /// on. Nothing it published survives either, because its updates only reach
    /// the state through the [`consume`] task the caller starts *after* this
    /// answers `true`.
    fn install_watcher(
        &mut self,
        id: &HostId,
        reach: &Arc<dyn RoostReach>,
        watcher: RoostWatcher,
    ) -> bool {
        if self.watchers.contains_key(id) {
            // Lost the race; the loser's watcher is stopped by its own drop here
            // rather than left running beside the winner's.
            return false;
        }
        match self.reaches.get(id) {
            Some(registered) if Arc::ptr_eq(&registered.reach, reach) => {
                self.watchers.insert(id.clone(), watcher);
                true
            }
            // Removed, or re-registered behind our back with a different reach.
            _ => false,
        }
    }
}

impl RoostHosts {
    /// Start a watcher per configured machine, plus the implicit [`LOCALHOST`]
    /// one. Never fails: a machine whose reach cannot even be built is still
    /// listed, as unreachable with the reason — the same posture as one that is
    /// merely asleep.
    ///
    /// **Sheds are not started here.** They arrive through
    /// [`Self::observe_sheds`] as the app learns which ones are running, are
    /// probed rather than watched, and get a watcher only once a session has
    /// answered on one (plan 019 §3.6).
    ///
    /// `reach_options` carries the test-mode seams (see
    /// [`crate::machines::build_ssh_reach`]): a per-host socket map that
    /// replaces the SSH bridge with a direct `LocalSession`, and the fake `ssh`
    /// a hermetic bootstrap cell execs. In test mode a host that has neither is
    /// permanently unreachable, so a hermetic run cannot leak an ssh child and
    /// the everyday "asleep / off-network" state is coverable with no real host.
    ///
    /// `on_change` fires whenever any LISTED host's state moves, so the
    /// embedder can push a refresh to its UI rather than leaving rows stale until
    /// someone clicks Refresh.
    pub fn start(
        handle: &tokio::runtime::Handle,
        config: &ShedConfig,
        reach_options: ReachOptions,
        jail_fs_root: bool,
        on_change: OnChange,
    ) -> RoostHosts {
        let hosts = RoostHosts {
            state: Arc::new(Mutex::new(BTreeMap::new())),
            reg: Arc::new(Mutex::new(Registry {
                ids: Vec::new(),
                reaches: BTreeMap::new(),
                watchers: BTreeMap::new(),
            })),
            handle: handle.clone(),
            config: config.clone(),
            reach_options,
            jail_fs_root,
            execs: Mutex::new(BTreeMap::new()),
            leases: Arc::new(RoostLeases::new()),
            probing: Arc::new(Mutex::new(BTreeSet::new())),
            probed: Arc::new(Mutex::new(BTreeMap::new())),
            probe_slots: Arc::new(tokio::sync::Semaphore::new(SHED_PROBE_CONCURRENCY)),
            on_change,
            on_lanes: Arc::new(Mutex::new(None)),
        };
        for entry in &config.machines {
            hosts.watch_machine(entry.clone());
        }
        // A configured entry WINS: the user spelling `localhost` in `machines:`
        // means an ssh target they chose, and shadowing it with the implicit
        // local reach would make the config a lie.
        if !config.machines.iter().any(|m| m.name == LOCALHOST) {
            hosts.watch_localhost();
        }
        hosts
    }

    /// Start watching one configured machine: register it, seed its row, and
    /// spawn its watcher + consumer.
    ///
    /// The SINGLE path a configured machine enters by, whether it came from the
    /// config at boot or from the Add dialog a minute ago — so a machine added
    /// later behaves identically rather than nearly so.
    fn watch_machine(&self, entry: MachineEntry) {
        let id = HostId::Machine(entry.name.clone());
        lock(&self.reg).ids.push(id.clone());
        let reach = build_ssh_reach(&entry, &self.reach_options);
        self.start_watching(id, reach, true);
    }

    /// Start watching the implicit local host. Registered LAST so it sorts after
    /// the machines the user actually configured, and UNLISTED until its session
    /// answers (see the module doc).
    fn watch_localhost(&self) {
        let id = HostId::Machine(LOCALHOST.to_string());
        lock(&self.reg).ids.push(id.clone());
        let reach = Ok(build_local_reach(&self.reach_options));
        self.start_watching(id, reach, false);
    }

    /// Seed the row and start the watcher for an ALREADY-REGISTERED id.
    ///
    /// Split from registration so `add` can claim the id and register it in
    /// one lock acquisition — a check-then-register across two would let two
    /// concurrent adds both win.
    fn start_watching(&self, id: HostId, reach: Result<Registered, String>, listed: bool) {
        lock(&self.state).insert(id.clone(), HostState::new(listed));

        let reach = match reach {
            Ok(reach) => reach,
            Err(e) => {
                // The reach could not even be constructed (an entry with no host,
                // a `known_hosts` file we cannot write beside). Record it and move
                // on: a host that cannot be reached is a row, not an error.
                let mut guard = lock(&self.state);
                if let Some(m) = guard.get_mut(&id) {
                    m.detail = Some(e);
                }
                return;
            }
        };

        let label = id.token();
        let (watcher, rx) = RoostWatcher::spawn(&self.handle, Arc::clone(&reach.reach), label);
        {
            let mut reg = lock(&self.reg);
            reg.reaches.insert(id.clone(), reach);
            reg.watchers.insert(id.clone(), watcher);
        }
        self.handle.spawn(consume(
            id,
            rx,
            Arc::clone(&self.state),
            self.on_change.clone(),
            Arc::clone(&self.on_lanes),
        ));
    }

    /// Add a machine and start watching it now.
    ///
    /// Rejects a name already being watched rather than shadowing it: two rows
    /// with one name is a UI that cannot be reasoned about, and the config write
    /// upstream refuses the same case for the same reason. `localhost` is
    /// reserved (see [`crate::machines::reject_reserved_name`]).
    pub fn add(&self, entry: MachineEntry) -> Result<(), String> {
        reject_reserved_name(&entry.name)?;
        let id = HostId::Machine(entry.name.clone());
        // Claim the id under the SAME lock acquisition that registers it.
        // Checking and then registering through two acquisitions lets two adds
        // both pass the check and both register, leaving one name with two
        // watchers and two rows.
        {
            let mut reg = lock(&self.reg);
            if reg.ids.contains(&id) {
                return Err(format!(
                    "a machine named {:?} is already watched",
                    entry.name
                ));
            }
            reg.ids.push(id.clone());
        }
        let reach = build_ssh_reach(&entry, &self.reach_options);
        self.start_watching(id, reach, true);
        (self.on_change)();
        Ok(())
    }

    /// The sessions AND the per-machine health, read under ONE lock.
    ///
    /// Taken together on purpose: read separately, a disconnect landing between
    /// the two calls yields a payload where a row says `stale: false` while its
    /// machine says `reachable: false` — a self-contradicting frame the UI would
    /// render as "live session on an offline machine".
    /// `filter` is `rc.list`'s `{host, shed}` (plan 019 §3.6). It is a **shed**
    /// filter: a machine belongs to no server, so any filter at all omits every
    /// machine, while a shed host is kept when its server and name match. A
    /// filtered `rc.list` that dropped the roost rows — which is what this layer
    /// used to do, by omitting its snapshot entirely — is exactly the bug §3.6
    /// names, because the shed card asks about one shed and would then see only
    /// its hub half.
    pub fn snapshot(&self, filter: HostFilter<'_>) -> (Vec<Value>, Vec<Value>) {
        let guard = lock(&self.state);
        (
            self.sessions_locked(&guard, filter),
            self.status_locked(&guard, filter),
        )
    }

    /// Every listed host's rows, flattened for the sessions view, each stamped
    /// with its origin so the UI can key and label it without inspecting `shed`
    /// (which is empty for every machine session — see the module doc).
    fn sessions_locked(
        &self,
        guard: &BTreeMap<HostId, HostState>,
        filter: HostFilter<'_>,
    ) -> Vec<Value> {
        let mut out = Vec::new();
        for (id, m) in guard.iter() {
            if !m.listed || !filter.keeps(id) {
                continue;
            }
            for session in &m.sessions {
                out.push(host_row(id, session, !m.reachable));
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
    pub async fn kill(&self, host: &str, slug: &str) -> Result<(), String> {
        let id = HostId::parse(host)?;
        let reach = self.reach(&id)?;
        let tab_id = parse_tab_id(slug)?;
        tab_close(reach.as_ref(), tab_id).await?;
        {
            let mut guard = lock(&self.state);
            if let Some(m) = guard.get_mut(&id) {
                m.sessions.retain(|s| s.tab_id != tab_id);
            }
        }
        // The row is gone from this app's view, so the lane on it is too — the
        // same optimism, for the same reason: waiting for the confirming
        // snapshot would leave a subscription (and, on a remote machine, an
        // `ssh -N` child) open against a tab the user just closed.
        self.publish_lanes(&id);
        // Outside the lock (the callback re-enters the app) and unconditional,
        // exactly as [`Self::create`] does it. A Tauri caller happens to refresh
        // afterwards, but the `machine.kill` socket op does not — so without this
        // the one case the optimistic drop exists FOR (the stream is mid-resync,
        // or the machine drops right after the close) is the one case an open UI
        // never hears about.
        (self.on_change)();
        Ok(())
    }

    /// This host's RC capabilities — what a create form may offer.
    ///
    /// **Synthesized, never probed.** roost is not shed's guest agent and has no
    /// `shed-ext-rc capabilities` to ask; the honest answer is the contract this
    /// client implements against it, which is a constant
    /// ([`shed_app::roost::roost_capabilities`]). So this is no longer an SSH
    /// round-trip — it cannot fail, cannot be stale, and answers for a host
    /// that is currently asleep.
    ///
    /// Still resolved through the registry so an unknown host is an error
    /// rather than a confident answer about a host nobody is watching.
    pub fn capabilities(&self, host: &str) -> Result<Value, String> {
        self.known(&HostId::parse(host)?)?;
        Ok(json!(roost_capabilities()))
    }

    /// Open a session ON this host — a roost `tab.open` running the kind's
    /// agent — and fold it into the local snapshot so the row appears immediately
    /// rather than whenever the push feed next catches up (see [`Self::kill`] for
    /// why "normally milliseconds" is not the same as "always").
    async fn create(
        &self,
        id: &HostId,
        kind: &RcKind,
        workdir: Option<&str>,
    ) -> Result<Value, String> {
        let reach = self.reach(id)?;
        let params = open_params(kind, workdir)?;
        let tab = tab_open(reach.as_ref(), params).await?;
        let session = opened_session(id, kind, &tab);
        let value = host_row(id, &session, false);
        {
            let mut guard = lock(&self.state);
            // Only if the watcher has not already delivered it. `get_mut` also
            // means a host dropped from the registry mid-open is not
            // resurrected by its own result.
            if let Some(m) = guard.get_mut(id) {
                if !m.sessions.iter().any(|s| s.tab_id == session.tab_id) {
                    m.sessions.push(session);
                }
                // **A `tab.open` that answered is a session answering**, which is
                // what `listed` means (see [`HostState::listed`]). Without this
                // the optimistic row is invisible on a host whose first watcher
                // snapshot has not landed yet — which is exactly the host a
                // launch is most likely to be the first thing to reach, a shed
                // that has just been bootstrapped.
                m.listed = true;
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
        host: &str,
        kind: &RcKind,
        _display_name: Option<&str>,
        workdir: Option<&str>,
        _permission_mode: Option<&str>,
        _initial_prompt: Option<&str>,
    ) -> Result<Value, String> {
        let id = HostId::parse(host)?;
        // A launch is a user action on a host they are looking at, so it is
        // allowed to be the thing that FIRST reaches an unwatched shed: a shed
        // whose session shed just installed has rows the moment its tab opens,
        // rather than at whatever later refresh happens to probe it.
        self.ensure_registered(&id)?;
        let row = self
            .create(&id, kind, workdir.map(str::trim).filter(|s| !s.is_empty()))
            .await?;
        // A `tab.open` that answered IS a session answering, which is the
        // probe-then-watch rule's own condition (plan 019 §3.6) — so this host
        // is worth a watcher now. It also makes the optimistic row [`create`]
        // just inserted visible: a registered-but-unwatched host is unlisted,
        // and a row on an unlisted host is not in the payload.
        self.watch(&id);
        Ok(row)
    }

    /// One registered host's reach, or an error naming the ones there are.
    fn reach(&self, id: &HostId) -> Result<Arc<dyn RoostReach>, String> {
        let reg = lock(&self.reg);
        if let Some(reach) = reg.reaches.get(id) {
            return Ok(Arc::clone(&reach.reach));
        }
        if reg.ids.contains(id) {
            return Err(format!(
                "{id} has no usable transport (its reach could not be built)"
            ));
        }
        Err(unknown_host(id, &reg.ids))
    }

    /// Assert a host is registered, without needing its reach — for the
    /// answers (capabilities) that do not touch the wire.
    fn known(&self, id: &HostId) -> Result<(), String> {
        let reg = lock(&self.reg);
        if reg.ids.contains(id) {
            return Ok(());
        }
        Err(unknown_host(id, &reg.ids))
    }

    /// Install the lane layer's reconcile hook. See [`OnLanes`].
    ///
    /// Idempotent by replacement: the last caller wins. Called once, from
    /// `lib.rs`'s setup, right after the lane layer is built.
    pub fn set_lane_observer(&self, observer: OnLanes) {
        *lock(&self.on_lanes) = Some(observer);
    }

    /// Every agent lane `host` currently exposes: agent session id → the
    /// [`AgentLaneStamp`] [`host_row`] stamps the row with.
    ///
    /// The one reader is the lane layer, which needs all three parts: the
    /// `kind` to pick an adapter, the reported URL, and (through
    /// [`Self::reach_kind`]) how to get to it. A host with no rows, no rows
    /// an adapter exists for, or no usable URL on them answers empty — which is
    /// also how `lane.open` decides a row has `no_lane`.
    pub fn agent_lanes(&self, host: &str) -> BTreeMap<String, AgentLaneStamp> {
        let Ok(id) = HostId::parse(host) else {
            return BTreeMap::new();
        };
        let guard = lock(&self.state);
        let Some(m) = guard.get(&id) else {
            return BTreeMap::new();
        };
        lanes_of(&m.sessions)
    }

    /// How `host` is reached — the lane's transport choice. See
    /// [`ReachKind`].
    pub fn reach_kind(&self, host: &str) -> Result<ReachKind, String> {
        let id = HostId::parse(host)?;
        let reg = lock(&self.reg);
        if let Some(reg_entry) = reg.reaches.get(&id) {
            return Ok(reg_entry.kind.clone());
        }
        if reg.ids.contains(&id) {
            return Err(format!(
                "{id} has no usable transport (its reach could not be built)"
            ));
        }
        Err(unknown_host(&id, &reg.ids))
    }

    /// Tell the lane layer what `id` now exposes. Call OUTSIDE the state
    /// lock — the hook tears lane entries (and their `ssh -N` children) down.
    fn publish_lanes(&self, id: &HostId) {
        let observer = lock(&self.on_lanes).clone();
        let Some(observer) = observer else { return };
        // The ADDRESS, not the token: the lane layer files its entries under
        // whatever string `lane.open` is given, and `lane.open` is given the
        // row's `machine` field. Publishing a different spelling here would evict
        // under a key nothing was ever stored beneath — which is to say it would
        // evict nothing, and leave a subscription and an `ssh -N` child behind
        // every row that went away.
        let address = id.address();
        observer(&address, &self.agent_lanes(&address));
    }

    /// Per-host health, for the UI's group headers.
    pub fn status(&self) -> Vec<Value> {
        let guard = lock(&self.state);
        self.status_locked(&guard, HostFilter::ALL)
    }

    fn status_locked(
        &self,
        guard: &BTreeMap<HostId, HostState>,
        filter: HostFilter<'_>,
    ) -> Vec<Value> {
        // Config order, then arrival order — a host registered mid-session
        // appears at the end rather than reshuffling the list someone is looking
        // at.
        //
        // `reg` is taken UNDER the caller's `state` guard, which is the only
        // place the two are held at once — nothing takes them the other way
        // round (`add` releases `reg` before `start_watching` touches `state`).
        let reg = lock(&self.reg);
        reg.ids
            .iter()
            .filter(|id| filter.keeps(id))
            .filter_map(|id| {
                let m = guard.get(id);
                // An unlisted host (the implicit `localhost` before its first
                // snapshot, or a shed nothing has answered on) is not a row at
                // all — see the module doc. An id with no state yet is
                // mid-registration and is listed as unreachable, which is what
                // it is.
                if m.is_some_and(|m| !m.listed) {
                    return None;
                }
                Some(json!({
                    // The ADDRESSABLE name — see [`HostId::address`].
                    "name": id.address(),
                    "label": id.label(),
                    "origin": id.token(),
                    "kind": id.kind(),
                    "server": id.server(),
                    "reachable": m.is_some_and(|m| m.reachable),
                    "connected_once": m.is_some_and(|m| m.seen),
                    "sessions": m.map_or(0, |m| m.sessions.len()),
                    "detail": m.and_then(|m| m.detail.clone()),
                    // The transport's own classification (plan 019 §3.6), so a
                    // client offers an install or a start without reading the
                    // sentence beside it.
                    "down_kind": m.and_then(|m| m.down_kind).map(|k| k.as_str()),
                }))
            })
            .collect()
    }

    // -- sheds: probe, then watch (plan 019 §3.6) ---------------------------

    /// Take one look at which sheds are running and reconcile the shed half of
    /// the registry against it.
    ///
    /// **Probed, not watched.** A watcher is a live `ssh` client-bridge held
    /// open for the app's lifetime; opening one per running shed would be a
    /// connection storm against hosts that mostly run no `roost-session` at all.
    /// So a shed that is not yet watched gets ONE cheap `session.identify` over
    /// the bridge — bounded ([`SHED_PROBE_BUDGET`]) and concurrent
    /// ([`SHED_PROBE_CONCURRENCY`]) — and earns a watcher only by answering it.
    /// The probe is idempotent and writes nothing, so a shed that is slow, or
    /// asleep, simply costs this refresh nothing and is asked again next time.
    ///
    /// `authoritative` is what makes removal safe. On an unfiltered refresh the
    /// caller knows which servers ANSWERED, and a running-shed list from those
    /// servers is the whole truth about them — so a watched shed of theirs that
    /// is missing has stopped, and its watcher goes. A shed missing because its
    /// **server** failed to refresh is not in that set and keeps its watcher,
    /// which is the difference between "your shed stopped" and "your laptop
    /// changed networks". A narrowed view (`rc.list {host, shed}`) passes an
    /// empty set: it may add, never remove.
    pub fn observe_sheds(&self, running: &[(String, String)], authoritative: &[String]) {
        let answered: BTreeSet<&str> = authoritative.iter().map(String::as_str).collect();
        let live: BTreeSet<HostId> = running
            .iter()
            .map(|(server, name)| HostId::Shed {
                server: server.clone(),
                name: name.clone(),
            })
            .collect();

        // Every shed host this layer holds anything for — watched or merely
        // registered by a probe — read once under the registry lock so the two
        // passes below decide on one view of it.
        let (known, watched): (Vec<HostId>, BTreeSet<HostId>) = {
            let reg = lock(&self.reg);
            (
                reg.ids
                    .iter()
                    .filter(|id| matches!(id, HostId::Shed { .. }))
                    .cloned()
                    .collect(),
                reg.watchers.keys().cloned().collect(),
            )
        };

        for id in &known {
            let Some(server) = id.server() else { continue };
            if !answered.contains(server) || live.contains(id) {
                continue;
            }
            // The shed stopped (or went away). Its rows are about a session that
            // no longer exists, so the watcher, the reach and the row all go —
            // including for a shed that was only ever registered by a probe,
            // which would otherwise keep a bridge for a host that is gone.
            self.remove(id);
        }

        for id in live {
            if watched.contains(&id) {
                continue;
            }
            self.probe_shed_in_background(id);
        }
    }

    /// Probe one shed's roost-session, and watch it if something answers.
    ///
    /// Spawned rather than awaited: this rides on a `rc.list`/`sheds.refresh`
    /// answer, and a user's listing must not wait on an `ssh` handshake to a
    /// sleeping host.
    fn probe_shed_in_background(&self, id: HostId) {
        {
            // Not yet, if this shed was asked recently — see
            // [`SHED_PROBE_COOLDOWN`]. Recorded at the START of the attempt, so
            // a slow probe does not license a second one behind it either.
            let mut probed = lock(&self.probed);
            let now = Instant::now();
            if probed
                .get(&id)
                .is_some_and(|last| now.duration_since(*last) < SHED_PROBE_COOLDOWN)
            {
                return;
            }
            probed.insert(id.clone(), now);
        }
        {
            // One probe per host at a time. Two refreshes a second apart would
            // otherwise fork two `ssh` children for the same shed, and the
            // second's answer would tell us nothing the first's would not.
            let mut probing = lock(&self.probing);
            if !probing.insert(id.clone()) {
                return;
            }
        }
        let Ok(reach) = self.ensure_registered(&id).and_then(|()| self.reach(&id)) else {
            lock(&self.probing).remove(&id);
            return;
        };
        let probing = Arc::clone(&self.probing);
        let slots = Arc::clone(&self.probe_slots);
        let hosts = self.watch_handle();
        self.handle.spawn(async move {
            let answered = {
                // The permit is held for the dial and dropped before the watch,
                // so a slow host occupies a slot for its own probe and not for
                // anyone else's.
                let _permit = slots.acquire().await;
                session_answers(reach.as_ref()).await
            };
            if answered {
                hosts.watch(&id);
            }
            lock(&probing).remove(&id);
        });
    }

    /// Register a host without watching it: build its reach once, and keep it.
    ///
    /// The entry point for everything that needs to REACH a host rather than
    /// observe it — a probe, a bootstrap, a launch. It is idempotent, and it is
    /// the reason there is exactly one [`SshBridge`] per host: roost's `open`
    /// sweeps a host's older scratch directories and refuses to reclaim one
    /// whose `bridge.sock` still answers, so a second bridge beside a live one
    /// would fail with "another Roost owns this target".
    fn ensure_registered(&self, id: &HostId) -> Result<(), String> {
        {
            let reg = lock(&self.reg);
            if reg.reaches.contains_key(id) {
                return Ok(());
            }
        }
        let entry = self.ssh_entry(id)?;
        let built = build_ssh_reach(&entry, &self.reach_options)?;
        let mut reg = lock(&self.reg);
        if reg.reaches.contains_key(id) {
            // Somebody won the race; theirs is the one the rows belong to.
            return Ok(());
        }
        if !reg.ids.contains(id) {
            reg.ids.push(id.clone());
        }
        reg.reaches.insert(id.clone(), built);
        drop(reg);
        lock(&self.state)
            .entry(id.clone())
            // UNLISTED: a registration is not an answer. A shed becomes a listed
            // host when a session answers on it (the snapshot arm of [`consume`]
            // flips this), which is the same rule the implicit `localhost`
            // follows.
            .or_insert_with(|| HostState::new(false));
        Ok(())
    }

    /// Start a watcher on an already-registered host. Idempotent.
    ///
    /// A host REMOVED while the watcher was being spawned gets none — see
    /// [`Registry::install_watcher`], which is also where that is tested.
    fn watch(&self, id: &HostId) {
        let reach = {
            let reg = lock(&self.reg);
            if reg.watchers.contains_key(id) {
                return;
            }
            match reg.reaches.get(id) {
                Some(registered) => Arc::clone(&registered.reach),
                None => return,
            }
        };
        let (watcher, rx) = RoostWatcher::spawn(&self.handle, Arc::clone(&reach), id.token());
        if !lock(&self.reg).install_watcher(id, &reach, watcher) {
            return;
        }
        self.handle.spawn(consume(
            id.clone(),
            rx,
            Arc::clone(&self.state),
            self.on_change.clone(),
            Arc::clone(&self.on_lanes),
        ));
        (self.on_change)();
    }

    /// Forget a host: stop its watcher, drop its reach and its rows.
    ///
    /// Only ever a SHED in practice — a `machines:` entry is the user's own
    /// declaration and stays listed however unreachable it is (see the module
    /// doc), while a shed's roost host exists only while the shed does.
    pub fn remove(&self, id: &HostId) {
        {
            let mut reg = lock(&self.reg);
            // Dropping the watcher aborts its loop; dropping the reach tears
            // down the `ssh` master behind it.
            reg.watchers.remove(id);
            reg.reaches.remove(id);
            reg.ids.retain(|known| known != id);
        }
        lock(&self.execs).remove(id);
        // Forgotten, not kept: a shed that stops and starts again is a NEW
        // question, and making the user wait out a cooldown for an answer that
        // has certainly changed would be the wrong way round.
        lock(&self.probed).remove(id);
        self.leases.forget(&id.token());
        let was_listed = lock(&self.state).remove(id).is_some_and(|m| m.listed);
        // The lane layer holds subscriptions (and `ssh -N` children) against
        // rows that have just gone. Publishing an empty set is how they are
        // released — the same signal a snapshot with no tabs sends.
        let observer = lock(&self.on_lanes).clone();
        if let Some(observer) = observer {
            observer(&id.address(), &BTreeMap::new());
        }
        if was_listed {
            (self.on_change)();
        }
    }

    // -- the bootstrap ops (plan 019 §3.6) ----------------------------------

    /// `roost.probe` — one read-only look at a host: what `roost-session`
    /// binaries are on it, and whether one is serving.
    ///
    /// Writes nothing, which is what makes it safe to run before the consent
    /// card — and what lets the card carry the fingerprint the install
    /// re-checks.
    pub async fn probe(&self, target: &str) -> Result<Value, BootstrapFailure> {
        let (id, probe) = self.run_probe(target).await?;
        let plan = Plan::for_probe(&id.token(), &probe);
        Ok(json!({
            "target": id.token(),
            "probe": probe_json(&probe),
            "plan": plan_json(&plan),
        }))
    }

    /// `roost.preview` — the probe, the plan matrix row it lands on, and the
    /// sentence naming where the bytes would come from.
    ///
    /// Nothing is resolved or downloaded here (plan 019 §3.5): a preview is what
    /// the consent card is built from, and consent has to come before the fetch.
    /// The `fingerprint` it answers with is what [`Self::bootstrap`] re-checks.
    pub async fn preview(&self, target: &str) -> Result<Value, BootstrapFailure> {
        let (id, probe) = self.run_probe(target).await?;
        let token = id.token();
        let plan = Plan::for_probe(&token, &probe);
        let env = SourceEnv::from_env();
        let source = bootstrap::preview(&env, &token, &probe.arch);
        Ok(json!({
            "target": token,
            "probe": probe_json(&probe),
            "plan": plan_json(&plan),
            "source": {
                "rung": source.source.as_str(),
                "sentence": source.describe(&token),
                "available": source.available(),
                "skipped": source.skipped,
            },
            // The button is offered only when the host implies an action AND
            // there are bytes to perform it with — plan 019 §3.4's sixth
            // plan-matrix row, which is a property of the ladder and not of the
            // host, and so is overlaid here rather than folded into `Plan`.
            "actionable": plan.actionable() && (!plan.needs_source() || source.available()),
            "client_label": CLIENT_LABEL,
        }))
    }

    /// `roost.bootstrap` — install (or update, or merely start) a
    /// `roost-session` on `target`, and wire its agent hooks.
    ///
    /// **Refuses without `consent`**, which is a caller bug and answers as one;
    /// **refuses a stale fingerprint**, which is a fact about the host and
    /// answers as a [`Stage::Fingerprint`] failure. The machine re-probes before
    /// it touches anything, so the check below is not the only one — it is the
    /// one that happens before a single byte is resolved or fetched.
    pub async fn bootstrap(
        &self,
        target: &str,
        fingerprint: &str,
        consent: bool,
    ) -> Result<Result<Installed, BootstrapFailure>, String> {
        if !consent {
            return Err(format!(
                "{target}: roost.bootstrap needs consent: true — nothing was changed"
            ));
        }
        let id = HostId::parse(target)?;
        let token = id.token();
        // The consent card's own probe, again: it is what decides the plan, the
        // architecture the source ladder resolves for, and whether the host is
        // still the one the user was asked about.
        let probe = match self.run_probe(&token).await {
            Ok((_, probe)) => probe,
            Err(failure) => return Ok(Err(failure)),
        };
        if probe.fingerprint != fingerprint {
            return Ok(Err(bootstrap::copy::fingerprint_changed(&token)));
        }
        let plan = Plan::for_probe(&token, &probe);
        if let Plan::Report { message, .. } = &plan {
            // Pin P6: a session shed cannot talk to is reported, never stopped
            // and never restarted. The client should not have offered a button.
            return Ok(Err(BootstrapFailure::new(Stage::Report, message.clone())));
        }
        if !plan.actionable() {
            return Ok(Err(BootstrapFailure::new(
                Stage::Probe,
                format!("{token} already runs a roost-session this build can read"),
            )));
        }

        // The source, resolved only now — after consent, after the fingerprint
        // matched, and only for a plan that needs bytes at all.
        let scratch = std::env::temp_dir().join(format!(
            "shed-roost-src-{}-{}",
            std::process::id(),
            fingerprint.get(..8).unwrap_or("0")
        ));
        let source = if plan.needs_source() {
            match bootstrap::resolve(&SourceEnv::from_env(), &token, &probe.arch, &scratch).await {
                Ok(handle) => Some(handle),
                Err(failure) => return Ok(Err(failure)),
            }
        } else {
            None
        };

        let request = InstallRequest {
            target: token.clone(),
            jail_fs_root: self.jail_fs_root,
            fingerprint: fingerprint.to_string(),
            client_label: CLIENT_LABEL.to_string(),
        };
        let outcome = {
            let exec = self.exec_for(&id)?;
            let reach = self.reach(&id)?;
            let runner = BootstrapRunner {
                exec: exec.as_ref(),
                reach: reach.as_ref(),
                leases: self.leases.as_ref(),
                target: &token,
                jail_fs_root: self.jail_fs_root,
            };
            runner.install(request, source).await
        };
        // The scratch directory is the asset rung's, and `resolve` removes it
        // before it returns — this is the belt to that braces, for the paths
        // where a fetch died mid-flight.
        let _ = std::fs::remove_dir_all(&scratch);

        if outcome.is_ok() {
            // shed just started a session there (or proved one was already
            // serving), so this host is worth watching now rather than at
            // whatever refresh next probes it. `watch` is idempotent.
            self.watch(&id);
        }
        Ok(outcome)
    }

    /// Run a probe against `target`, resolving the id and the transports.
    async fn run_probe(&self, target: &str) -> Result<(HostId, Probe), BootstrapFailure> {
        let fail = |message: String| BootstrapFailure::new(Stage::Probe, message);
        let id = HostId::parse(target).map_err(fail)?;
        let token = id.token();
        self.ensure_registered(&id).map_err(fail)?;
        let exec = self.exec_for(&id).map_err(fail)?;
        let reach = self.reach(&id).map_err(fail)?;
        let runner = BootstrapRunner {
            exec: exec.as_ref(),
            reach: reach.as_ref(),
            leases: self.leases.as_ref(),
            target: &token,
            jail_fs_root: self.jail_fs_root,
        };
        let probe = runner.probe().await?;
        Ok((id, probe))
    }

    /// The `ssh` ControlMaster runner for one host, built once and kept.
    ///
    /// Kept because the master is the point: a probe and the install that
    /// follows it are a dozen remote execs, and paying a fresh handshake for
    /// each of them turns a 20-second install into a two-minute one.
    fn exec_for(&self, id: &HostId) -> Result<Arc<SshExec>, String> {
        if let Some(exec) = lock(&self.execs).get(id) {
            return Ok(Arc::clone(exec));
        }
        let entry = self.ssh_entry(id)?;
        let exec = Arc::new(
            SshExec::new(&entry, &self.reach_options.bridge_options())
                .map_err(|e| e.to_string())?,
        );
        let mut execs = lock(&self.execs);
        // Last writer loses, not wins: a runner built in a lost race is dropped
        // here, which exits its own master and removes its own scratch dir.
        Ok(Arc::clone(execs.entry(id.clone()).or_insert_with(|| exec)))
    }

    /// The ssh identity of one host: a `machines:` entry as written, or a shed's
    /// synthesized one (plan 019 §3.6's [`shed_reach_entry`]).
    ///
    /// Read from the config SNAPSHOT this layer took at launch, not a re-read —
    /// see [`RoostHosts::config`].
    fn ssh_entry(&self, id: &HostId) -> Result<MachineEntry, String> {
        match id {
            HostId::Machine(name) => self
                .config
                .machine(name)
                .cloned()
                .ok_or_else(|| format!("no machine {name:?} in ~/.shed/config.yaml")),
            HostId::Shed { server, name } => {
                let entry = self
                    .config
                    .servers
                    .iter()
                    .find(|s| &s.name == server)
                    .ok_or_else(|| format!("no server {server:?} in ~/.shed/config.yaml"))?;
                Ok(shed_reach_entry(entry, name))
            }
        }
    }

    /// A handle the background probe task can call [`Self::watch`] through.
    ///
    /// `RoostHosts` is held by the app as an `Arc` and this layer has no weak
    /// reference to itself, so the spawned probe takes the few pieces it needs
    /// rather than the whole layer: the registry, the state, and the two hooks.
    fn watch_handle(&self) -> WatchHandle {
        WatchHandle {
            state: Arc::clone(&self.state),
            reg: Arc::clone(&self.reg),
            handle: self.handle.clone(),
            on_change: self.on_change.clone(),
            on_lanes: Arc::clone(&self.on_lanes),
        }
    }

    /// The configured server names — every one a shed could live on.
    ///
    /// Read by the listing ops' reconcile ([`crate::ipc::observe_reachability`]):
    /// a server that answered with NO sheds appears nowhere in a refresh, and is
    /// otherwise indistinguishable from one that failed.
    pub fn servers(&self) -> Vec<String> {
        self.config.servers.iter().map(|s| s.name.clone()).collect()
    }

    /// Ask every `ssh` ControlMaster this layer opened to exit, and remove their
    /// scratch directories — the explicit half of the teardown, for app quit.
    ///
    /// `SshExec`'s own `Drop` does the same thing blockingly for every other way
    /// a runner can end; this is the path where waiting is possible and a
    /// `ControlPersist` master outliving the app that opened it would be visible
    /// (plan 019 §3.6).
    pub async fn shutdown(&self) {
        let execs: Vec<Arc<SshExec>> = lock(&self.execs).values().cloned().collect();
        for exec in &execs {
            exec.shutdown().await;
        }
        lock(&self.execs).clear();
    }
}

/// The pieces a spawned shed probe needs to promote its host to a watcher.
///
/// Not an `Arc<RoostHosts>`: this layer is held by the app and has no weak
/// self-reference, and a strong one captured in a background task would keep it
/// alive past app quit. Every field here is already shared state.
struct WatchHandle {
    state: Arc<Mutex<BTreeMap<HostId, HostState>>>,
    reg: Arc<Mutex<Registry>>,
    handle: tokio::runtime::Handle,
    on_change: OnChange,
    on_lanes: Arc<Mutex<Option<OnLanes>>>,
}

impl WatchHandle {
    /// [`RoostHosts::watch`], from a background task. Same rules: idempotent,
    /// and it does nothing for a host whose reach has since been dropped —
    /// including one removed *while this was spawning*, which is the ordering
    /// this path is the likeliest to hit (see [`Registry::install_watcher`]: the
    /// probe that calls this has been away for an `ssh` round trip, and a sheds
    /// refresh may have removed the shed in the meantime).
    fn watch(&self, id: &HostId) {
        let reach = {
            let reg = lock(&self.reg);
            if reg.watchers.contains_key(id) {
                return;
            }
            match reg.reaches.get(id) {
                Some(registered) => Arc::clone(&registered.reach),
                None => return,
            }
        };
        let (watcher, rx) = RoostWatcher::spawn(&self.handle, Arc::clone(&reach), id.token());
        if !lock(&self.reg).install_watcher(id, &reach, watcher) {
            return;
        }
        self.handle.spawn(consume(
            id.clone(),
            rx,
            Arc::clone(&self.state),
            self.on_change.clone(),
            Arc::clone(&self.on_lanes),
        ));
        (self.on_change)();
    }
}

/// Does a `roost-session` answer over this reach? One bounded
/// `session.identify`, and nothing else.
///
/// **Deliberately not a watcher.** A watcher would subscribe, list, and then sit
/// on the connection reconnecting forever; this asks the one question that
/// decides whether that is worth doing. A failure of any kind — no session, not
/// installed, unreachable, a protocol shed cannot read — is the same answer
/// here: `false`, try again next refresh.
async fn session_answers(reach: &dyn RoostReach) -> bool {
    let probe = async {
        let endpoint = reach.ensure().await.ok()?;
        let mut conn = Conn::endpoint(&endpoint).await.ok()?;
        conn.session_identify().await.ok()
    };
    match tokio::time::timeout(SHED_PROBE_BUDGET, probe).await {
        Ok(Some(_)) => true,
        // Both arms invalidate: a reach whose dial failed is holding a transport
        // nothing should be handed next time (the bridge's socket outlives its
        // `ssh`), and a probe that timed out is by definition unfinished.
        _ => {
            reach.invalidate().await;
            false
        }
    }
}

// ---------------------------------------------------------------------------
// the bootstrap DTOs, as JSON
// ---------------------------------------------------------------------------
//
// Hand-written rather than derived. The types are `shed-core`'s FRB DTOs (plan
// 019 §3.4): mobile hand-mirrors them into Dart sealed classes, so putting
// `serde` attributes on them would make this app's wire shape the schema two
// other languages have to follow. The shapes below are this client's own — flat,
// kebab-tagged, and readable by a harness cell without a decoder.

/// A [`Probe`] as `roost.probe`/`roost.preview` answer with it.
fn probe_json(probe: &Probe) -> Value {
    let outcome = match &probe.outcome {
        ProbeOutcome::Compatible { path, identity } => json!({
            "kind": "compatible",
            "path": path,
            "identity": identity_json(Some(identity)),
        }),
        ProbeOutcome::Mismatch { path, identity } => json!({
            "kind": "mismatch",
            "path": path,
            "identity": identity_json(identity.as_ref()),
        }),
        ProbeOutcome::Missing => json!({ "kind": "missing" }),
    };
    let session = match &probe.session {
        SessionState::Running { identity } => json!({
            "state": "running",
            "app_version": identity.app_version,
            "session_protocol": identity.session_protocol,
            "session_id": identity.session_id,
            "started_at": identity.started_at,
        }),
        SessionState::NoSession => json!({ "state": "no-session" }),
        SessionState::NotInstalled => json!({ "state": "not-installed" }),
    };
    json!({
        "outcome": outcome,
        "arch": probe.arch,
        "home": probe.home,
        "session": session,
        "candidates": probe.candidates,
        "dest": probe.install_dest(),
        "fingerprint": probe.fingerprint,
    })
}

fn identity_json(identity: Option<&bootstrap::Identity>) -> Value {
    match identity {
        None => Value::Null,
        Some(identity) => json!({
            "app_version": identity.app_version,
            "session_protocol": identity.session_protocol,
            "libghostty_build": identity.libghostty_build,
        }),
    }
}

/// A [`Plan`] as the client reads it: the kebab name the button is chosen by,
/// plus the fields that row carries.
fn plan_json(plan: &Plan) -> Value {
    let mut value = json!({ "kind": plan.as_str(), "needs_source": plan.needs_source() });
    let object = value.as_object_mut().expect("a json object");
    match plan {
        Plan::Install { dest } => {
            object.insert("dest".into(), json!(dest));
        }
        Plan::Update {
            path,
            incumbent,
            replaces_newer,
            dest,
        } => {
            object.insert("path".into(), json!(path));
            object.insert("incumbent".into(), identity_json(incumbent.as_ref()));
            object.insert("replaces_newer".into(), json!(replaces_newer));
            object.insert("dest".into(), json!(dest));
        }
        Plan::Start { path } => {
            object.insert("path".into(), json!(path));
        }
        Plan::UpToDate { identity } => {
            object.insert("session_protocol".into(), json!(identity.session_protocol));
            object.insert("app_version".into(), json!(identity.app_version));
        }
        Plan::Report { protocol, message } => {
            object.insert("session_protocol".into(), json!(protocol));
            object.insert("message".into(), json!(message));
        }
    }
    value
}

/// A successful [`Installed`] as `roost.bootstrap` answers with it.
pub(crate) fn installed_json(installed: &Installed) -> Value {
    json!({
        "ok": true,
        "target": installed.target,
        "plan": plan_json(&installed.plan),
        "dest": installed.dest,
        "verdict": installed.verdict,
        "session_protocol": installed.session.as_ref().map(|s| s.session_protocol),
        "session_id": installed.session.as_ref().map(|s| s.session_id.clone()),
        // Pin P5: a PATH disagreement is reported and never acted on.
        "path_warning": installed.path_warning,
        "backup_warning": installed.backup_warning,
        "hooks": installed.hooks.as_ref().map(hooks_json),
    })
}

/// A [`HooksResult`] — always reported, never fatal (see
/// [`shed_core::roost::bootstrap::hooks`]).
fn hooks_json(hooks: &HooksResult) -> Value {
    json!({
        "client": hooks.client_label,
        "mode": "auto",
        "applied": hooks.applied(),
        // The lease itself is NEVER published: it is a bearer token for the
        // host's interactive session, and a client that logged it would put a
        // usable credential in a test artifact. Whether shed still holds one is
        // the part a UI needs.
        "lease_held": hooks.lease.is_some(),
        "skipped_code": hooks.skipped_code,
        "wired": hooks.wired,
        "refreshed": hooks.refreshed,
        "removed": hooks.removed,
        "skipped": hooks.skipped.iter().map(|s| json!({ "agent": s.agent, "reason": s.reason }))
            .collect::<Vec<_>>(),
        "errors": hooks.errors.iter().map(|e| json!({ "agent": e.agent, "error": e.error }))
            .collect::<Vec<_>>(),
        "error": hooks.error,
    })
}

/// A [`BootstrapFailure`] as `roost.bootstrap` answers with it.
///
/// An `ok` envelope carrying `ok: false`, not an IPC error: the STAGE is the
/// payload. "prepare refused" and "the host changed under you" are different
/// instructions to the user, and flattening them into one error string would
/// throw away the only field that distinguishes them.
pub(crate) fn failure_json(failure: &BootstrapFailure) -> Value {
    json!({
        "ok": false,
        "error": {
            "stage": failure.stage.as_str(),
            "message": failure.message,
            "restored": failure.restored,
        },
    })
}

/// Which hosts an `rc.list` answer is about.
///
/// A **shed** filter, always: `{host, shed}` narrows to one server's sheds, and
/// a machine belongs to no server, so any filter at all excludes every machine
/// (plan 019 §3.6). `ALL` is the unfiltered listing.
#[derive(Debug, Clone, Copy)]
pub struct HostFilter<'a> {
    pub server: Option<&'a str>,
    pub shed: Option<&'a str>,
}

impl HostFilter<'_> {
    /// Every host, machines included — the unfiltered `rc.list`.
    pub const ALL: HostFilter<'static> = HostFilter {
        server: None,
        shed: None,
    };

    fn filtered(&self) -> bool {
        self.server.is_some() || self.shed.is_some()
    }

    fn keeps(&self, id: &HostId) -> bool {
        match id {
            HostId::Machine(_) => !self.filtered(),
            HostId::Shed { server, name } => {
                self.server.is_none_or(|want| want == server)
                    && self.shed.is_none_or(|want| want == name)
            }
        }
    }
}

fn unknown_host(id: &HostId, ids: &[HostId]) -> String {
    format!(
        "no host {id} is being watched (have: {})",
        ids.iter().map(HostId::token).collect::<Vec<_>>().join(", ")
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
fn opened_session(id: &HostId, kind: &RcKind, tab: &Tab) -> RoostSession {
    RoostSession {
        host_label: id.token(),
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
        // BOTH map to roost's `grok`, because roost has ONE adapter for the two
        // (plan 017 §3.2): `gx` is not a source roost ever writes. Which of the
        // two a tab reads as is decided on the way BACK, by
        // [`RoostSession::agent_kind`], from whether the tab carries a usable
        // `gx.remote` — so predicting `grok` here is right for a launch of
        // either, and a `gx` launch that binds its lane promotes itself on the
        // next snapshot.
        RcKind::Gx | RcKind::Grok => Some("grok"),
        RcKind::ClaudeBroker | RcKind::Shell | RcKind::Other(_) => None,
    }
}

/// Fold one host's watcher updates into its state.
async fn consume(
    id: HostId,
    mut rx: tokio::sync::mpsc::UnboundedReceiver<RoostUpdate>,
    state: Arc<Mutex<BTreeMap<HostId, HostState>>>,
    on_change: OnChange,
    on_lanes: Arc<Mutex<Option<OnLanes>>>,
) {
    while let Some(update) = rx.recv().await {
        // Set by the SNAPSHOT arm only. A `Down` deliberately keeps the last
        // rows on screen, so it says nothing about which tabs still exist and
        // must not evict a lane — see [`OnLanes`].
        let mut lanes: Option<BTreeMap<String, AgentLaneStamp>> = None;
        let visible = {
            let mut guard = lock(&state);
            let Some(m) = guard.get_mut(&id) else {
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
                    m.down_kind = None;
                    m.seen = true;
                    // A session answered here at least once, so this host is real
                    // and stays listed from now on.
                    m.listed = true;
                    lanes = Some(lanes_of(&m.sessions));
                }
                RoostUpdate::Down { reason, kind } => {
                    // Sessions are deliberately NOT cleared: the last snapshot
                    // stays on screen, marked stale, until the next connect
                    // resyncs it.
                    m.reachable = false;
                    m.detail = Some(reason);
                    // **The kind is kept beside the sentence, not derived from
                    // it** (plan 019 §3.6). It is what a client turns into an
                    // Install or a Start button, and it reaches `rc.list` on the
                    // status row.
                    m.down_kind = Some(kind);
                }
            }
            m.listed
        };
        // Outside the lock, and BEFORE the repaint: the hook tears down lane
        // entries whose tab has gone, and a UI that repainted first would offer
        // a Transcript affordance for a row that is about to vanish.
        //
        // Unconditional on `visible`, unlike the repaint below: an unlisted host
        // still has state a lane could be holding, and skipping it would leak a
        // subscription for exactly the host nobody is looking at.
        if let Some(lanes) = lanes {
            let observer = lock(&on_lanes).clone();
            if let Some(observer) = observer {
                // The ADDRESS — see `RoostHosts::publish_lanes`.
                observer(&id.address(), &lanes);
            }
        }
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

/// One roost session as the UI reads it: the roost row mapped onto the DTO
/// every card already renders, stamped with where it lives.
///
/// Shared by the list and by `create`'s return value — a caller that keys off
/// `origin`/`machine` (or calls `sessionKey()`) must get the same shape from
/// both, or the one place they differ becomes the one place a caller breaks.
///
/// ## The stamps that make the union rule work (plan 019 §3.6)
///
/// A shed has TWO row sets in one payload: the hub's, listed over ssh by
/// `shed-ext-rc`, and roost's, read from the session on that shed. They describe
/// the same shed and they are not the same rows, so three fields carry the
/// difference:
///
/// * **`origin`** — `roost:<server>/<shed>` here, `<server>/<shed>` on a hub row.
///   The key a UI de-duplicates and groups by, and the one the capabilities map
///   is filed under.
/// * **`source`** — `roost`, against the hub row's `hub`. What a card reads to
///   decide which launch/kill op a button belongs to.
/// * **`host` / `shed`** — the SERVER and the shed name, exactly as the hub row
///   spells them, which is what makes a filtered `rc.list {host, shed}` able to
///   select both halves with one predicate. A machine has neither, and says so
///   explicitly rather than leaving the UI to infer it from an empty string.
///
/// `attention` and `tab_id` are stamped HERE rather than carried on
/// [`shed_core::rc::RcSessionDto`]: that shape is pinned byte-for-byte by the
/// Go↔Rust parity harness and built as a struct literal at a dozen sites, so
/// roost's two extra facts travel on [`RoostSession`] and each client adds them
/// to its own row payload (plan 013 §3.2).
fn host_row(id: &HostId, session: &RoostSession, stale: bool) -> Value {
    let mut row = serde_json::to_value(session.to_rc_dto()).unwrap_or_else(|_| json!({}));
    if let Some(obj) = row.as_object_mut() {
        obj.insert("origin".into(), json!(id.token()));
        obj.insert("origin_kind".into(), json!(id.kind()));
        // The ADDRESS every `machine.*` / `lane.*` op takes for this row. A
        // machine's is its bare name (unchanged since plan 013); a shed's is its
        // grammar token, which [`HostId::parse`] reads back.
        obj.insert("machine".into(), json!(id.address()));
        obj.insert("source".into(), json!("roost"));
        match id {
            HostId::Machine(_) => {
                obj.insert("host".into(), json!(id.token()));
                obj.insert("shed".into(), json!(""));
            }
            HostId::Shed { server, name } => {
                obj.insert("host".into(), json!(server));
                obj.insert("shed".into(), json!(name));
            }
        }
        obj.insert("stale".into(), json!(stale));
        // roost's sticky notification bit. Its own affordance (a dot), NOT part
        // of `needsYou`: roost clears it on UI focus and shed never clears it, so
        // folding it into activity would leave a card stuck asking for attention.
        obj.insert("attention".into(), json!(session.attention));
        // A STRING, like every other id on roost's wire: a JavaScript client
        // cannot round an i64 through a `Number` without losing it.
        obj.insert("tab_id".into(), json!(session.tab_id.to_string()));
        // The agent-lane capability signal (plan 015 §3.4). Absent unless the
        // tab's adapter reported a server this app can actually talk to.
        if let Some(lane) = agent_lane(session) {
            obj.insert("agent_lane".into(), lane);
        }
    }
    row
}

/// The `agent_lane` stamp for one row, or `None`.
///
/// **Its PRESENCE is the capability signal** — the UI offers a Transcript
/// affordance for a row that has it and nothing for a row that does not, and
/// `lane.open` answers `no_lane` for the latter.
///
/// **The derivation is [`RoostSession::agent_lane`]'s, not this module's.** It
/// used to be spelled here: `source != "opencode"` and a hardcoded
/// `"kind": "opencode"`, from the cut where opencode was the only adapter (plan
/// 015). Plan 017 §3.5 retired that, because the phone mirrors these rows too
/// and two hand-written copies of "which tabs have a lane" is exactly how the
/// desktop and shed-mobile come to offer a transcript on different sets of
/// rows. shed-core owns the rule; this function is the JSON it rides on.
///
/// The key is **`agent_lane`, not `lane`**: `lane` is taken on the session DTO
/// (`RcSession.lane` is the RC hub's lane token) and a second meaning on the
/// same row would be read by the wrong consumer. The stamp's own field names
/// ([`AgentLaneStamp`]) are the wire — `server_url` keeps that name for gx too,
/// whose roost key is `gx.remote`.
///
/// `ownership.metadata` reaches [`RoostSession::agent_lane`] because
/// [`shed_core::roost::RoostSession`] keeps roost's `Ownership` whole;
/// `to_rc_dto()` drops it, which is why this is stamped beside the DTO rather
/// than carried on it.
fn agent_lane(session: &RoostSession) -> Option<Value> {
    session.agent_lane().map(|stamp| json!(stamp))
}

/// The `session_id → stamp` map for a row set, for the lane layer's reconcile.
///
/// **The whole stamp, not just the URL.** The lane layer keys a live entry by
/// `(kind, server_url)` (plan 017 §3.5) — a tab that restarts as a different
/// agent on the same loopback port is a different lane, and an entry compared on
/// the URL alone would survive it and keep pumping the wrong adapter.
///
/// Deliberately derived from the SAME rule the row is stamped from
/// ([`RoostSession::agent_lane`]): a lane the UI can see and a lane the backend
/// will keep alive have to be the same set, or an entry survives a row it no
/// longer belongs to.
fn lanes_of(sessions: &[RoostSession]) -> BTreeMap<String, AgentLaneStamp> {
    sessions
        .iter()
        .filter_map(|s| {
            let stamp = s.agent_lane()?;
            Some((stamp.session_id.clone(), stamp))
        })
        .collect()
}

#[cfg(test)]
mod tests {
    use super::*;

    use std::sync::atomic::{AtomicUsize, Ordering};
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

    /// One host's health, read straight out of the in-process state.
    fn machine_health(hosts: &RoostHosts, name: &str) -> (bool, Option<String>) {
        let guard = hosts.state.lock().unwrap();
        let m = guard
            .get(&HostId::Machine(name.to_string()))
            .expect("a registered machine");
        (m.reachable, m.detail.clone())
    }

    /// One host's `Down` classification, as `rc.list` publishes it.
    fn machine_down_kind(hosts: &RoostHosts, name: &str) -> Option<&'static str> {
        let guard = hosts.state.lock().unwrap();
        guard
            .get(&HostId::Machine(name.to_string()))
            .expect("a registered machine")
            .down_kind
            .map(|kind| kind.as_str())
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

    /// The test-mode reach options for a socket map: every named host answers on
    /// its socket, everything else is unreachable and spawns no ssh.
    fn sockets(pairs: &[(&str, &std::path::Path)]) -> ReachOptions {
        ReachOptions {
            roost_sockets: pairs
                .iter()
                .map(|(name, path)| ((*name).to_string(), path.to_path_buf()))
                .collect(),
            ssh_bin: None,
            test_mode: true,
        }
    }

    fn start(config: &ShedConfig, sockets: &ReachOptions) -> RoostHosts {
        RoostHosts::start(
            &tokio::runtime::Handle::current(),
            config,
            sockets.clone(),
            false,
            Arc::new(|| {}),
        )
    }

    /// Like [`start`], plus the count of `on_change` calls the layer has made —
    /// what an open UI would have been told to re-read.
    fn start_counting(
        config: &ShedConfig,
        sockets: &ReachOptions,
    ) -> (RoostHosts, Arc<AtomicUsize>) {
        let calls = Arc::new(AtomicUsize::new(0));
        let counter = Arc::clone(&calls);
        let hosts = RoostHosts::start(
            &tokio::runtime::Handle::current(),
            config,
            sockets.clone(),
            false,
            Arc::new(move || {
                counter.fetch_add(1, Ordering::SeqCst);
            }),
        );
        (hosts, calls)
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

    fn rows(hosts: &RoostHosts) -> Vec<Value> {
        hosts.snapshot(HostFilter::ALL).0
    }

    /// A bare roost row, with whatever ownership a lane test needs on it.
    fn owned(source: Option<&str>, session_id: &str, metadata: &[(&str, &str)]) -> RoostSession {
        RoostSession {
            host_label: "mini3".to_string(),
            tab_id: 4,
            project_id: 1,
            project_name: String::new(),
            title: "oc".to_string(),
            user_titled: false,
            cwd: "/home/shed/work".to_string(),
            shell_state: roost_ipc::agent::ShellState::Unknown,
            lifecycle: roost_ipc::agent::AgentLifecycle::Working,
            attention: false,
            ownership: source.map(|source| Ownership {
                source: source.to_string(),
                session_id: session_id.to_string(),
                metadata: metadata
                    .iter()
                    .map(|(k, v)| ((*k).to_string(), (*v).to_string()))
                    .collect(),
                ..Ownership::default()
            }),
            created_at: 0,
        }
    }

    /// **`agent_lane` is the lane capability signal, and it is stamped from the
    /// tab's own report** (plan 015 §3.4).
    ///
    /// Its PRESENCE is what makes the UI offer a transcript, so every case that
    /// cannot actually be opened must leave it off: a different agent, a tab that
    /// reported no server, a blank one, and a server with no session id to
    /// address. The key is `agent_lane` and NOT `lane` — `lane` is already taken
    /// on this DTO by the RC hub's lane token.
    #[test]
    fn the_lane_stamp_needs_an_opencode_tab_that_reported_a_server_and_a_session() {
        let url = "http://127.0.0.1:41234";
        let row = host_row(
            &HostId::Machine("mini3".to_string()),
            &owned(Some("opencode"), "ses_abc", &[("server_url", url)]),
            false,
        );
        assert_eq!(
            row["agent_lane"],
            json!({"kind": "opencode", "session_id": "ses_abc", "server_url": url})
        );
        assert!(row.get("lane").is_none() || row["lane"].is_null(), "{row}");

        // Everything that must NOT carry one.
        for (what, session) in [
            ("a plain shell tab", owned(None, "", &[])),
            (
                "another agent",
                owned(Some("claude"), "ses_abc", &[("server_url", url)]),
            ),
            (
                "opencode with no server reported",
                owned(Some("opencode"), "ses_abc", &[("model", "sonnet")]),
            ),
            (
                "opencode with a blank server",
                owned(Some("opencode"), "ses_abc", &[("server_url", "   ")]),
            ),
            (
                "opencode with no session id to address",
                owned(Some("opencode"), "", &[("server_url", url)]),
            ),
        ] {
            let row = host_row(&HostId::Machine("mini3".to_string()), &session, false);
            assert!(
                row.get("agent_lane").is_none(),
                "{what} was stamped with a lane: {row}"
            );
        }
    }

    /// The reconcile map and the row stamp are the SAME fact, derived from the
    /// same place — an entry that outlived the row it belongs to would hold a
    /// subscription (and an `ssh -N` child) nobody can see.
    #[test]
    fn the_reconcile_map_is_exactly_the_stamped_rows() {
        let url = "http://127.0.0.1:41234";
        let mut second = owned(Some("opencode"), "ses_two", &[("server_url", url)]);
        second.tab_id = 7;
        let sessions = vec![
            owned(Some("opencode"), "ses_abc", &[("server_url", url)]),
            owned(Some("claude"), "ses_zzz", &[("server_url", url)]),
            owned(Some("opencode"), "ses_bare", &[]),
            second,
        ];
        let stamp = |id: &str| AgentLaneStamp {
            kind: "opencode".to_string(),
            session_id: id.to_string(),
            server_url: url.to_string(),
        };
        assert_eq!(
            lanes_of(&sessions),
            BTreeMap::from([
                ("ses_abc".to_string(), stamp("ses_abc")),
                ("ses_two".to_string(), stamp("ses_two")),
            ])
        );
    }

    /// **Stamping is stricter than it was, and that is deliberate.**
    ///
    /// Until plan 017 this module minted the stamp itself and validated nothing
    /// beyond "non-empty". It now defers to `RoostSession::agent_lane`, which
    /// applies `loopback_base_url` on BOTH paths — so a `server_url` roost could
    /// once have published and this app would once have dialled is now refused,
    /// silently, as `no_lane`.
    ///
    /// That is the right posture (the desktop DIALS this value, and it should
    /// not depend on an upstream process's filtering staying correct), but it is
    /// a behaviour change with a quiet failure mode, so it is pinned here rather
    /// than left to be rediscovered. Every shape below is one roost's own rule
    /// already rejects; if this test ever starts failing on a shape a real
    /// daemon emits, the bug is upstream and the symptom will be a Transcript
    /// affordance that vanished.
    #[test]
    fn a_server_url_that_is_not_a_bare_loopback_base_carries_no_lane() {
        let ok = owned(
            Some("opencode"),
            "ses_ok",
            &[("server_url", "http://127.0.0.1:41234")],
        );
        assert!(agent_lane(&ok).is_some(), "the shape roost actually stamps");

        for bad in [
            // A trailing slash: `loopback_base_url` rejects it, and it is the
            // shape a hand-written config or a helpful URL-joiner produces.
            "http://127.0.0.1:41234/",
            // No port — nothing to forward to.
            "http://127.0.0.1",
            // Not loopback: a lane URL is the MACHINE's own loopback, never an
            // address this host could route to.
            "http://0.0.0.0:41234",
            "http://10.0.0.7:41234",
            // Not http, and not a bare base.
            "https://127.0.0.1:41234",
            "http://127.0.0.1:41234/v1",
            "http://user@127.0.0.1:41234",
            "",
            "   ",
        ] {
            let session = owned(Some("opencode"), "ses_bad", &[("server_url", bad)]);
            assert!(
                agent_lane(&session).is_none(),
                "{bad:?} is not a loopback base URL and must not be stamped"
            );
        }

        // A gx tab is stamped from `gx.remote`, and is refused for the same
        // reasons — including the one that makes it read as plain `grok`.
        let gx = owned(
            Some("grok"),
            "ses_gx",
            &[("gx.remote", "http://127.0.0.1:2431")],
        );
        assert_eq!(
            agent_lane(&gx).and_then(|v| v["kind"].as_str().map(str::to_string)),
            Some("gx".to_string())
        );
        let demoted = owned(
            Some("grok"),
            "ses_gx",
            &[("gx.remote", "http://127.0.0.1:2431/")],
        );
        assert!(
            agent_lane(&demoted).is_none(),
            "a gx.remote that fails the rule leaves the tab as plain grok, lane-less"
        );

        // And the session id is the other half: a tab with a usable URL but no
        // id would advertise a panel that can never open.
        let idless = owned(
            Some("opencode"),
            "",
            &[("server_url", "http://127.0.0.1:41234")],
        );
        assert!(agent_lane(&idless).is_none());
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
        let mini3 = HostId::Machine("mini3".to_string());
        let state: Arc<Mutex<BTreeMap<HostId, HostState>>> = Arc::new(Mutex::new(BTreeMap::from(
            [(mini3.clone(), HostState::new(true))],
        )));
        let history: PublishedStates = Arc::new(Mutex::new(Vec::new()));
        let on_change: OnChange = {
            let state = Arc::clone(&state);
            let history = Arc::clone(&history);
            Arc::new(move || {
                let guard = lock(&state);
                if let Some(m) = guard.get(&HostId::Machine("mini3".to_string())) {
                    history
                        .lock()
                        .unwrap()
                        .push((m.reachable, m.detail.clone()));
                }
            })
        };
        let reach: Arc<dyn RoostReach> = Arc::new(shed_app::roost::LocalSession::new(
            "mini3",
            fake.socket_path(),
        ));
        let (watcher, rx) = RoostWatcher::spawn(
            &tokio::runtime::Handle::current(),
            reach,
            "machine:mini3".to_string(),
        );
        tokio::spawn(consume(
            mini3.clone(),
            rx,
            Arc::clone(&state),
            on_change,
            Arc::new(Mutex::new(None)),
        ));

        fn activity(state: &Arc<Mutex<BTreeMap<HostId, HostState>>>) -> Option<Value> {
            let id = HostId::Machine("mini3".to_string());
            let guard = lock(state);
            let m = guard.get(&id)?;
            let session = m.sessions.first()?;
            Some(host_row(&id, session, !m.reachable)["activity"].clone())
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
            guard
                .get(&HostId::Machine(LOCALHOST.to_string()))?
                .detail
                .clone()
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
        assert_eq!(reg.ids, vec![HostId::Machine(LOCALHOST.to_string())]);
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

    /// **An optimistic drop that nobody is told about is not optimistic.**
    ///
    /// The Tauri commands happen to re-read afterwards; the `machine.kill`
    /// socket op does not. So `kill` publishes the change itself, the way
    /// [`RoostHosts::create`] does — otherwise the exact case the optimistic drop
    /// exists for (no snapshot is coming, because the stream is resyncing or the
    /// machine just dropped) is the case an open UI never hears about.
    #[tokio::test]
    async fn kill_publishes_the_drop_it_made() {
        let fake = FakeRoost::start().await;
        claim_opencode(&fake, "working", "session_status", false);
        let (machines, calls) = start_counting(
            &config_with(&["mini3"]),
            &sockets(&[("mini3", fake.socket_path())]),
        );
        wait_for("the row to kill", || rows(&machines).into_iter().next()).await;

        // Read the count the instant `kill` returns: the watcher's own snapshot
        // for this same `tab.closed` publishes too, but it arrives later and is
        // exactly the delivery the unhappy path does not get.
        let before = calls.load(Ordering::SeqCst);
        machines.kill("mini3", "5").await.expect("the close lands");
        assert!(rows(&machines).is_empty());
        assert!(
            calls.load(Ordering::SeqCst) > before,
            "kill returned without publishing the drop it made"
        );
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
                .get(&HostId::Machine("mini3".to_string()))
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

    /// **A failed launch must not leave a ghost row.**
    ///
    /// [`RoostHosts::create`] shows a provisional row the moment the tab opens,
    /// before any adapter has claimed it — so the watcher's inventory carries
    /// that tab in its HIDDEN half. If the launched process dies before it ever
    /// reports (a missing binary, an immediate crash), the `tab.closed` is a
    /// hidden-half event too. The 2 s poller repaired this on its next
    /// `tab.list`; the observer-only watcher publishes the vanished tab instead
    /// (`shed_app::roost::observe_once`), and the authoritative snapshot then
    /// replaces the provisional row set. Without either, the card is permanent.
    #[tokio::test]
    async fn a_launched_tab_that_dies_unclaimed_takes_its_row_with_it() {
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
                .get(&HostId::Machine("mini3".to_string()))
                .filter(|m| m.seen)
                .map(|_| ())
        })
        .await;

        machines
            .launch("mini3", &RcKind::Opencode, None, None, None, None)
            .await
            .expect("the open lands");
        assert!(
            rows(&machines).iter().any(|r| r["slug"] == "6"),
            "the optimistic row is showing"
        );

        // The launched process dies without ever claiming the tab, and roost
        // closes it. Out of band — through a second reach, NOT through
        // `RoostHosts::kill`, whose own optimistic drop would hide the bug.
        let reach: Arc<dyn RoostReach> = Arc::new(shed_app::roost::LocalSession::new(
            "mini3",
            fake.socket_path(),
        ));
        tab_close(reach.as_ref(), 6).await.expect("the tab closes");

        wait_for("the provisional row to retire", || {
            rows(&machines)
                .iter()
                .all(|r| r["slug"] != "6")
                .then_some(())
        })
        .await;
    }

    /// **Eviction's signal, at the layer that delivers it** (plan 015 §3.4).
    ///
    /// A lane entry holds a live subscription and, on a remote machine, an
    /// `ssh -N` child. What retires it is the roost SNAPSHOT: the observer is
    /// handed the machine's current lane set every time a fresh inventory
    /// replaces the row set, and an entry absent from that set is dropped. So
    /// this asserts the two ways a lane-carrying tab can stop existing, both of
    /// which reach the observer as an EMPTY map:
    ///
    /// * the agent exits and its adapter releases the tab (roost's wire removes
    ///   `ownership`, which drops the row from the agent-owned inventory), and
    /// * the tab itself closes.
    ///
    /// Both matter, because after a release the tab is in the inventory's HIDDEN
    /// half and its eventual close publishes nothing on its own account — the
    /// case plan 014's ghost-row fix exists for. An entry that survived either
    /// would sit behind a row nobody can see.
    #[tokio::test]
    async fn a_lane_carrying_tab_that_goes_away_publishes_an_empty_lane_set() {
        let url = "http://127.0.0.1:41234";
        let fake = FakeRoost::start().await;
        let mut owned = ownership("opencode", "ses_abc", "session_status", 1_700_000_100);
        owned["metadata"] = json!({ "server_url": url });
        fake.set_tab_axes(VECTOR_TAB, "working", Some(owned), false);

        let machines = start(
            &config_with(&["mini3"]),
            &sockets(&[("mini3", fake.socket_path())]),
        );
        // Record every lane set published, in order — a history, not a sample:
        // an entry evicted and re-created between two polls looks like nothing
        // happened.
        type Published = Arc<Mutex<Vec<BTreeMap<String, AgentLaneStamp>>>>;
        let history: Published = Arc::new(Mutex::new(Vec::new()));
        {
            let history = Arc::clone(&history);
            machines.set_lane_observer(Arc::new(move |machine, lanes| {
                assert_eq!(machine, "mini3");
                history.lock().unwrap().push(lanes.clone());
            }));
        }

        let last = || history.lock().unwrap().last().cloned();
        wait_for("the lane to be published", || {
            last().filter(|l| l.get("ses_abc").map(|s| s.server_url.as_str()) == Some(url))
        })
        .await;

        // The agent exits: its adapter releases the tab, so the row leaves the
        // agent-owned inventory even though the tab is still open.
        fake.set_tab_axes(VECTOR_TAB, "inactive", None, false);
        wait_for("the released tab's lane to retire", || {
            last().filter(|l| l.is_empty())
        })
        .await;

        // And the now-hidden tab closes — out of band, the way a dead process's
        // shell does. It must not resurrect anything.
        let reach: Arc<dyn RoostReach> = Arc::new(shed_app::roost::LocalSession::new(
            "mini3",
            fake.socket_path(),
        ));
        tab_close(reach.as_ref(), VECTOR_TAB).await.expect("close");
        wait_for("the closed tab to be seen", || {
            rows(&machines).is_empty().then_some(())
        })
        .await;
        assert!(
            history
                .lock()
                .unwrap()
                .iter()
                .all(|l| l.is_empty()
                    || l.get("ses_abc").map(|s| s.server_url.as_str()) == Some(url)),
            "a lane set was published that named a server nobody reported"
        );
        assert_eq!(last(), Some(BTreeMap::new()), "the last word is: no lanes");
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
        assert!(e.contains("no host machine:nope"), "{e}");
    }

    // -- plan 019: the typed id, the union stamp, the filter, `Down.kind` -----

    /// **The grammar is the id** (plan 019 §0). Every row origin, capabilities
    /// key and `roost.*` target is one of these two spellings, and a bare word
    /// is a machine because every `machine.*` op and every row's `machine` field
    /// has sent one since plan 013.
    #[test]
    fn a_host_id_round_trips_its_token_and_reads_a_bare_name_as_a_machine() {
        let machine = HostId::Machine("mini3".to_string());
        assert_eq!(machine.token(), "machine:mini3");
        assert_eq!(HostId::parse("machine:mini3").unwrap(), machine);
        assert_eq!(
            HostId::parse("mini3").unwrap(),
            machine,
            "a bare name is the machine door every existing caller uses"
        );

        let shed = HostId::Shed {
            server: "popos".to_string(),
            name: "p019-a".to_string(),
        };
        assert_eq!(shed.token(), "roost:popos/p019-a");
        assert_eq!(HostId::parse("roost:popos/p019-a").unwrap(), shed);
        assert_eq!(shed.server(), Some("popos"));
        assert_eq!(shed.label(), "p019-a");
        assert_eq!(shed.kind(), "shed");

        // A shed name with no server identifies no shed, so it is refused
        // rather than defaulted onto whichever server happens to be first.
        for bad in ["roost:p019-a", "roost:/p019-a", "roost:popos/", "", "   "] {
            assert!(HostId::parse(bad).is_err(), "{bad:?} should not parse");
        }
        // A machine's name may contain a slash; only the `roost:` prefix splits.
        assert_eq!(
            HostId::parse("machine:a/b").unwrap(),
            HostId::Machine("a/b".to_string())
        );
    }

    /// The union stamp: a shed's roost row carries the SERVER and the shed name
    /// the hub row carries, so one filter predicate selects both halves — and an
    /// origin that cannot collide with the hub's, so a consumer keying on it can
    /// tell them apart (plan 019 §3.6).
    #[test]
    fn a_shed_roost_row_is_stamped_for_the_union_rule() {
        let id = HostId::Shed {
            server: "popos".to_string(),
            name: "p019-a".to_string(),
        };
        let row = host_row(&id, &owned(Some("codex"), "ses_x", &[]), false);
        assert_eq!(row["origin"], json!("roost:popos/p019-a"));
        assert_eq!(row["origin_kind"], json!("shed"));
        assert_eq!(row["source"], json!("roost"));
        assert_eq!(
            row["host"],
            json!("popos"),
            "the SERVER, as the hub spells it"
        );
        assert_eq!(row["shed"], json!("p019-a"));
        assert_eq!(
            row["machine"],
            json!("roost:popos/p019-a"),
            "the address `lane.*` and `machine.kill` take for this row"
        );

        // A machine's row is unchanged from plan 013, plus the source stamp.
        let row = host_row(
            &HostId::Machine("mini3".to_string()),
            &owned(Some("codex"), "ses_x", &[]),
            false,
        );
        assert_eq!(row["origin"], json!("machine:mini3"));
        assert_eq!(row["machine"], json!("mini3"));
        assert_eq!(row["host"], json!("machine:mini3"));
        assert_eq!(row["shed"], json!(""));
        assert_eq!(row["source"], json!("roost"));
    }

    /// A filtered `rc.list {host, shed}` keeps that shed's roost rows and drops
    /// every machine — the bug plan 019 §3.6 names, as a predicate.
    #[test]
    fn the_filter_narrows_sheds_and_omits_machines() {
        let mini3 = HostId::Machine("mini3".to_string());
        let a = HostId::Shed {
            server: "popos".to_string(),
            name: "p019-a".to_string(),
        };
        let b = HostId::Shed {
            server: "popos".to_string(),
            name: "p019-b".to_string(),
        };
        let elsewhere = HostId::Shed {
            server: "mini2".to_string(),
            name: "p019-a".to_string(),
        };

        for id in [&mini3, &a, &b, &elsewhere] {
            assert!(HostFilter::ALL.keeps(id), "{id} survives no filter");
        }

        let one = HostFilter {
            server: Some("popos"),
            shed: Some("p019-a"),
        };
        assert!(one.keeps(&a));
        assert!(!one.keeps(&b), "a different shed on the same server");
        assert!(
            !one.keeps(&elsewhere),
            "the same shed NAME on another server is another shed"
        );
        assert!(!one.keeps(&mini3), "a machine belongs to no server");

        // A host-only filter is still a shed filter.
        let server_only = HostFilter {
            server: Some("popos"),
            shed: None,
        };
        assert!(server_only.keeps(&a) && server_only.keeps(&b));
        assert!(!server_only.keeps(&elsewhere) && !server_only.keeps(&mini3));
    }

    /// **`Down.kind` reaches the status row** (plan 019 §3.6 / acceptance 8).
    ///
    /// The classification is carried beside the sentence, never re-derived from
    /// it: a client offers "start a session" for a `no-session` and an install
    /// for a `not-installed`, and a substring search over prose is what
    /// `ReachError` exists to remove.
    #[tokio::test]
    async fn a_down_kind_reaches_the_status_row_and_clears_on_recovery() {
        let fake = FakeRoost::start().await;
        let socket = fake.socket_path().to_path_buf();
        let machines = start(&config_with(&["mini3"]), &sockets(&[("mini3", &socket)]));
        wait_for("the first snapshot", || {
            machine_health(&machines, "mini3").0.then_some(())
        })
        .await;
        assert_eq!(
            machine_down_kind(&machines, "mini3"),
            None,
            "a reachable host has no down kind"
        );

        // The session goes away, socket and all (the fake owns its scratch dir,
        // so dropping it takes the socket with it) — a `LocalSession` then
        // reports roost's own `no-session` family, which is the state an
        // install/start button is chosen from.
        drop(fake);
        let kind = wait_for("the down kind", || machine_down_kind(&machines, "mini3")).await;
        assert_eq!(kind, "no-session");
        let status = machines.status();
        let row = status_named(&status, "mini3").expect("mini3 is listed");
        assert_eq!(row["down_kind"], json!("no-session"));
        assert_eq!(row["kind"], json!("machine"));
        assert_eq!(row["origin"], json!("machine:mini3"));
        assert!(
            row["detail"]
                .as_str()
                .is_some_and(|d| d.contains("roost-session")),
            "the sentence rides beside the kind: {row}"
        );
    }

    /// **A watcher spawned around a removal is dropped, never installed.**
    ///
    /// The ordering: a shed probe answers and clones the reach, an authoritative
    /// sheds refresh calls [`RoostHosts::remove`] — which deletes the reach, the
    /// watcher, the id and the row — and the probe then resumes and registers the
    /// watcher it had already spawned. The registry used to ask only "did
    /// somebody else win?", so that watcher survived the removal and went on
    /// publishing state for a shed that had stopped.
    ///
    /// The removal and the watcher here are the real ones ([`RoostHosts::remove`]
    /// and a real [`RoostWatcher`] on the reach the registry handed out); the gap
    /// itself is a few instructions inside [`RoostHosts::watch`], between
    /// releasing the lock and taking it again, which no test can schedule — so
    /// the sequence is driven through [`Registry::install_watcher`], the guard
    /// that decides it.
    #[tokio::test]
    async fn a_watcher_spawned_around_a_removal_is_dropped_instead_of_installed() {
        let fake = FakeRoost::start().await;
        let id = HostId::Shed {
            server: "popos".to_string(),
            name: "p019-a".to_string(),
        };
        let config = ShedConfig {
            servers: vec![shed_core::config::ShedServerEntry {
                name: "popos".to_string(),
                host: "127.0.0.1".to_string(),
                ssh_port: 2222,
                ..Default::default()
            }],
            ..Default::default()
        };
        let hosts = start(&config, &sockets(&[(&id.token(), fake.socket_path())]));

        // Where a probe starts: the shed is registered, and not yet watched.
        hosts.ensure_registered(&id).expect("the shed registers");
        // …and what it carries across the `ssh` round trip — the same clone
        // `watch` takes out of the registry before it lets the lock go.
        let reach = hosts.reach(&id).expect("the registered reach");

        // The refresh that says the shed has stopped.
        hosts.remove(&id);

        // The probe resumes: its watcher is already spawned, and is now offered
        // to a registry that no longer knows this host.
        let (watcher, mut rx) = RoostWatcher::spawn(
            &tokio::runtime::Handle::current(),
            Arc::clone(&reach),
            id.token(),
        );
        assert!(
            !lock(&hosts.reg).install_watcher(&id, &reach, watcher),
            "a removed host takes no watcher"
        );
        assert!(
            !lock(&hosts.reg).watchers.contains_key(&id),
            "and none is registered for it"
        );

        // The refusal STOPPED it — the channel only closes once the task is gone,
        // which is what dropping a `RoostWatcher` does.
        tokio::time::timeout(Duration::from_secs(5), async {
            while rx.recv().await.is_some() {}
        })
        .await
        .expect("the refused watcher's loop is aborted by its own drop");

        // Nothing of the removed shed survives: no state, and so no row and no
        // status line for it in any listing.
        assert!(
            !lock(&hosts.state).contains_key(&id),
            "the removed shed has no state left to publish into"
        );
        assert!(rows(&hosts).is_empty(), "and no row: {:?}", rows(&hosts));
        assert!(
            hosts
                .status()
                .iter()
                .all(|row| row["origin"] != json!(id.token())),
            "nor a status line"
        );

        // The whole path, not just the guard: `WatchHandle::watch` — what the
        // background probe actually calls — installs nothing for a host that has
        // been removed.
        hosts.watch_handle().watch(&id);
        assert!(
            !lock(&hosts.reg).watchers.contains_key(&id),
            "the probe's own entry point refuses a removed host too"
        );

        // And the shed coming straight back does not rescue the stale watcher:
        // a fresh registration builds a NEW bridge, and a watcher holding the
        // torn-down one would report the returned shed as permanently down.
        hosts.ensure_registered(&id).expect("the shed re-registers");
        let fresh = hosts.reach(&id).expect("a new reach");
        assert!(
            !Arc::ptr_eq(&fresh, &reach),
            "the re-registration built its own reach"
        );
        let (stale, _rx) = RoostWatcher::spawn(
            &tokio::runtime::Handle::current(),
            Arc::clone(&reach),
            id.token(),
        );
        assert!(
            !lock(&hosts.reg).install_watcher(&id, &reach, stale),
            "a watcher spawned against a torn-down reach is refused"
        );
        assert!(!lock(&hosts.reg).watchers.contains_key(&id));
    }
}
