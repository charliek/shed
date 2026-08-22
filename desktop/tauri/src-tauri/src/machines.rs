//! **Machine targets in the desktop app** (plan 012 S4, roadmap R4).
//!
//! `machines:` has lived in `shed-core`'s config since plan 009, and until this
//! module nothing but the `sx` porcelain read it. This is the second consumer —
//! the thing R4 exists to deliver — and it is deliberately thin, because the
//! reach itself graduated into the shared layer in S2:
//!
//! * addressing + the SSH argv → [`shed_core::machine`]
//! * the hub's `/v1` wire → [`shed_core::hub_client`]
//! * the transport seam + the reconnecting watcher → [`shed_app::machine`]
//!
//! What is left here is what a desktop app actually owns: which machines exist,
//! one watcher per machine, the last snapshot each one reported, and whether it
//! is currently reachable.
//!
//! ## Unreachable is a STATE, not an error
//!
//! A machine that is asleep, off the network, or simply has no hub running is
//! the normal case, not a failure. Every machine therefore always has a row;
//! `reachable` and `detail` say how much to trust it. Nothing here returns an
//! error to the UI for a machine being down — that is the posture `sx` already
//! takes (it degrades to probe polling with a note) and the clients inherit it.
//!
//! ## One overlay per feed
//!
//! Sessions are held per machine and never merged into a shared activity
//! overlay. A directly-read hub reports `shed: ""` on every event (it has no
//! shed to name), so `(shed, slug)` — the key
//! [`shed_core::rc_events::ActivityOverlay`] uses — would collide across two
//! machines that happen to share a slug. Rows are keyed by ORIGIN + slug here
//! instead.

use std::collections::BTreeMap;
use std::sync::{Arc, Mutex};

use serde_json::{json, Value};

use shed_app::machine::{
    FixedPort, ForwardError, MachineForward, MachineHubUpdate, MachineHubWatcher, SshForward,
};
use shed_core::config::{MachineEntry, ShedConfig};
use shed_core::rc::RcSessionDto;

/// Called whenever a machine's state changes, so the embedder can tell its UI
/// to re-read. Without it the app would only ever show the state it happened to
/// fetch at mount: a machine that comes up (or drops) later changes nothing the
/// frontend is watching, and the rows sit stale until a manual Refresh.
pub type OnChange = Arc<dyn Fn() + Send + Sync>;

/// One machine's live view, as the UI reads it.
struct MachineState {
    /// The last snapshot the hub reported. Retained across a disconnect on
    /// purpose: the UI keeps rendering the last known sessions (dimmed, with a
    /// reason) rather than blanking the machine, matching how the shed feed
    /// treats a blip.
    sessions: Vec<RcSessionDto>,
    reachable: bool,
    /// Why it is unreachable, verbatim from the watcher. Shown to the user —
    /// "no route to host" and "nothing is listening on 1029" are different
    /// problems and the app should not flatten them into "offline".
    detail: Option<String>,
    /// Whether a snapshot has EVER arrived, so the UI can distinguish "still
    /// connecting" from "connected, and this machine genuinely has no sessions".
    seen: bool,
}

impl MachineState {
    fn new() -> Self {
        Self {
            sessions: Vec::new(),
            reachable: false,
            detail: None,
            seen: false,
        }
    }
}

/// The app's machine layer: one watcher per configured machine, plus the state
/// each reports.
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
    /// the same test-mode forward substitution and the same change callback as
    /// the ones started at boot — one code path, not two.
    handle: tokio::runtime::Handle,
    test_hub_ports: std::collections::HashMap<String, u16>,
    on_change: OnChange,
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
    let _serialized = ADD_LOCK.lock().unwrap_or_else(|e| e.into_inner());

    let name = machine
        .get("name")
        .and_then(Value::as_str)
        .unwrap_or("")
        .trim()
        .to_string();
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

/// The mutable half of [`Machines`]: the configured set and its live watchers.
///
/// `names` carries ORDER (config order, then arrival order) because the UI
/// lists machines in it; `entries` is the lookup control verbs resolve through;
/// `watchers` is held only so dropping it tears down the ssh children.
struct Registry {
    names: Vec<String>,
    /// The entry each watcher was STARTED with, keyed by name.
    ///
    /// Control verbs resolve through this rather than re-reading the config, so
    /// a kill can never address a different host than the row the user is
    /// looking at: if `machines:` is edited to repoint `mini3` mid-session, the
    /// watcher (and therefore the displayed rows) still belong to the old entry,
    /// and the kill must follow the rows.
    entries: BTreeMap<String, MachineEntry>,
    /// Held so the watchers (and their forwards) live as long as the app does.
    /// Dropping one aborts its loop and tears down its `ssh -N -L` child.
    watchers: Vec<MachineHubWatcher>,
}

impl Machines {
    /// Start a watcher per configured machine. Never fails: a machine whose
    /// forward cannot even be reserved is still listed, as unreachable with the
    /// reason — the same posture as one that is merely asleep.
    ///
    /// `test_hub_ports` (from the test-mode-only `crate::env::Env::machine_hub_ports`)
    /// replaces the `ssh -N -L` forward with a direct [`FixedPort`], per machine.
    /// When it is non-empty NO machine spawns ssh — an unlisted entry gets a
    /// forward that always refuses — so a hermetic run cannot leak an ssh child,
    /// and the "machine is asleep" state is coverable without a real machine.
    ///
    /// `on_change` fires whenever any machine's state moves, so the embedder can
    /// push a refresh to its UI rather than leaving rows stale until someone
    /// clicks Refresh.
    pub fn start(
        handle: &tokio::runtime::Handle,
        config: &ShedConfig,
        test_hub_ports: &std::collections::HashMap<String, u16>,
        on_change: OnChange,
    ) -> Machines {
        let machines = Machines {
            state: Arc::new(Mutex::new(BTreeMap::new())),
            reg: Mutex::new(Registry {
                names: Vec::new(),
                entries: BTreeMap::new(),
                watchers: Vec::new(),
            }),
            handle: handle.clone(),
            test_hub_ports: test_hub_ports.clone(),
            on_change,
        };
        for entry in &config.machines {
            machines.watch(entry.clone());
        }
        machines
    }

    /// Start watching one machine: register it, seed its row, and spawn its
    /// watcher + consumer.
    ///
    /// The SINGLE path a machine enters by, whether it came from the config at
    /// boot or from the Add dialog a minute ago — so a machine added later
    /// behaves identically rather than nearly so.
    fn watch(&self, entry: MachineEntry) {
        {
            let mut reg = self.reg.lock().unwrap_or_else(|e| e.into_inner());
            reg.names.push(entry.name.clone());
            reg.entries.insert(entry.name.clone(), entry.clone());
        }
        self.spawn_watcher(entry);
    }

    /// Seed the row and start the watcher for an ALREADY-REGISTERED machine.
    ///
    /// Split from registration so `add` can claim the name and register it in
    /// one lock acquisition — a check-then-register across two would let two
    /// concurrent adds both win.
    fn spawn_watcher(&self, entry: MachineEntry) {
        self.state
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .insert(entry.name.clone(), MachineState::new());

        let forward = match build_forward(&entry, &self.test_hub_ports) {
            Ok(forward) => forward,
            Err(e) => {
                // Reserving a local port failed — record it and move on. A
                // machine that cannot be reached is a row, not an error.
                let mut guard = self.state.lock().unwrap_or_else(|e| e.into_inner());
                if let Some(m) = guard.get_mut(&entry.name) {
                    m.detail = Some(e);
                }
                return;
            }
        };

        let (watcher, rx) = MachineHubWatcher::spawn(&self.handle, forward, entry.name.clone());
        self.reg
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .watchers
            .push(watcher);
        self.handle.spawn(consume(
            entry.name,
            rx,
            Arc::clone(&self.state),
            self.on_change.clone(),
        ));
    }

    /// Add a machine and start watching it now.
    ///
    /// Rejects a name already being watched rather than shadowing it: two rows
    /// with one name is a UI that cannot be reasoned about, and the config write
    /// upstream refuses the same case for the same reason.
    pub fn add(&self, entry: MachineEntry) -> Result<(), String> {
        // Claim the name under the SAME lock acquisition that registers it.
        // Checking and then registering through two acquisitions lets two adds
        // both pass the check and both register, leaving one name with two
        // watchers and two rows.
        {
            let mut reg = self.reg.lock().unwrap_or_else(|e| e.into_inner());
            if reg.entries.contains_key(&entry.name) {
                return Err(format!("a machine named {:?} is already watched", entry.name));
            }
            reg.names.push(entry.name.clone());
            reg.entries.insert(entry.name.clone(), entry.clone());
        }
        self.spawn_watcher(entry);
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
        let guard = self.state.lock().unwrap_or_else(|e| e.into_inner());
        (self.sessions_locked(&guard), self.status_locked(&guard))
    }

    /// Every machine's rows, flattened for the sessions view, each stamped with
    /// its origin so the UI can key and label it without inspecting `shed`
    /// (which is empty for every machine session — see the module doc).
    fn sessions_locked(&self, guard: &BTreeMap<String, MachineState>) -> Vec<Value> {
        let mut out = Vec::new();
        for (name, m) in guard.iter() {
            for dto in &m.sessions {
                let mut row = serde_json::to_value(dto).unwrap_or_else(|_| json!({}));
                if let Some(obj) = row.as_object_mut() {
                    obj.insert("origin".into(), json!(format!("machine:{name}")));
                    obj.insert("origin_kind".into(), json!("machine"));
                    obj.insert("machine".into(), json!(name));
                    // A machine session belongs to no shed and no server. Spell
                    // that explicitly rather than leaving the UI to infer it
                    // from an empty string.
                    obj.insert("host".into(), json!(format!("machine:{name}")));
                    obj.insert("shed".into(), json!(""));
                    obj.insert("stale".into(), json!(!m.reachable));
                }
                out.push(row);
            }
        }
        out
    }

    /// Kill a session on a machine, then drop the row optimistically.
    ///
    /// The optimistic drop matters because the hub reconciles on a 2 s active /
    /// 10 s idle cadence: without it a killed session lingers in the UI for up
    /// to ten seconds, which reads as "the kill didn't work". The next snapshot
    /// is authoritative and will restore the row if the kill somehow did not
    /// take.
    pub async fn kill(&self, machine: &str, slug: &str) -> Result<(), String> {
        let entry = self.entry(machine)?;
        shed_app::machine::kill(&entry, slug).await?;
        let mut guard = self.state.lock().unwrap_or_else(|e| e.into_inner());
        if let Some(m) = guard.get_mut(machine) {
            m.sessions.retain(|s| s.slug != slug);
        }
        Ok(())
    }

    /// The interactive `ssh -t … tmux attach` command for one of this machine's
    /// sessions — what a terminal opener spawns.
    ///
    /// A shed session has had this since the beginning; a machine session did
    /// not, which left it with no way in at all on the desktop. The command is
    /// built from the SAME `MachineEntry` the watcher and `kill` use, so the
    /// terminal lands on the host the rest of the app is talking about.
    pub fn terminal_command(
        &self,
        machine: &str,
        slug: &str,
    ) -> Result<shed_core::terminal::TerminalCommand, String> {
        let entry = self.entry(machine)?;
        Ok(shed_app::machine::terminal_command(&entry, slug))
    }

    /// One watched machine's config entry, or an error naming the ones there are.
    fn entry(&self, machine: &str) -> Result<MachineEntry, String> {
        let reg = self.reg.lock().unwrap_or_else(|e| e.into_inner());
        reg.entries.get(machine).cloned().ok_or_else(|| {
            let known: Vec<&str> = reg.names.iter().map(String::as_str).collect();
            format!(
                "no machine {machine:?} is being watched (have: {})",
                known.join(", ")
            )
        })
    }

    /// Per-machine health, for the UI's machine group headers.
    pub fn status(&self) -> Vec<Value> {
        let guard = self.state.lock().unwrap_or_else(|e| e.into_inner());
        self.status_locked(&guard)
    }

    fn status_locked(&self, guard: &BTreeMap<String, MachineState>) -> Vec<Value> {
        // Config order, then arrival order — a machine added mid-session appears
        // at the end rather than reshuffling the list someone is looking at.
        let names = self
            .reg
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .names
            .clone();
        names
            .iter()
            .map(|name| {
                let m = guard.get(name);
                json!({
                    "name": name,
                    "origin": format!("machine:{name}"),
                    "reachable": m.map(|m| m.reachable).unwrap_or(false),
                    "connected_once": m.map(|m| m.seen).unwrap_or(false),
                    "sessions": m.map(|m| m.sessions.len()).unwrap_or(0),
                    "detail": m.and_then(|m| m.detail.clone()),
                })
            })
            .collect()
    }
}

/// The transport choice — the ONLY per-client part of reaching a machine.
fn build_forward(
    entry: &MachineEntry,
    test_hub_ports: &std::collections::HashMap<String, u16>,
) -> Result<Arc<dyn MachineForward>, String> {
    if test_hub_ports.is_empty() {
        return SshForward::reserve(entry.clone())
            .map(|f| Arc::new(f) as Arc<dyn MachineForward>)
            .map_err(|e| e.to_string());
    }
    // Test mode with a map present: reach the harness's hub directly. Reaching it
    // needs no transport at all, which is the point — everything ABOVE the port
    // is the shared code under test.
    //
    // An UNLISTED machine gets a forward that simply REFUSES — that is how the
    // suite exercises an unreachable machine, and it guarantees a hermetic run
    // never spawns ssh for a machine the harness forgot to map.
    //
    // Deliberately not `FixedPort(0)`: connecting to port 0 is
    // implementation-defined (EADDRNOTAVAIL on macOS, EINVAL/ECONNREFUSED on
    // Linux, and not guaranteed to fail fast anywhere), so it would make the
    // unreachable path's timing and error text OS-dependent. Failing in
    // `ensure()` with no I/O at all is both portable and instant.
    match test_hub_ports.get(&entry.name).copied() {
        Some(port) => Ok(Arc::new(FixedPort(port))),
        None => Ok(Arc::new(UnreachableForward)),
    }
}

/// A forward that never comes up — the test-mode stand-in for a machine the
/// harness did not map (see [`build_forward`]).
///
/// `ensure()` fails immediately with no I/O, so "this machine is unreachable" is
/// expressed exactly once, portably, and without depending on how a given OS
/// treats a connect to an unusable port.
struct UnreachableForward;

#[async_trait::async_trait]
impl MachineForward for UnreachableForward {
    fn port(&self) -> u16 {
        0
    }

    async fn ensure(&self) -> Result<(), ForwardError> {
        Err(ForwardError(
            "no hub configured for this machine in test mode".to_string(),
        ))
    }
}

/// Fold one machine's watcher updates into its state.
async fn consume(
    name: String,
    mut rx: tokio::sync::mpsc::UnboundedReceiver<MachineHubUpdate>,
    state: Arc<Mutex<BTreeMap<String, MachineState>>>,
    on_change: OnChange,
) {
    while let Some(update) = rx.recv().await {
        {
            let mut guard = state.lock().unwrap_or_else(|e| e.into_inner());
            let Some(m) = guard.get_mut(&name) else {
                return;
            };
            match update {
                MachineHubUpdate::Snapshot { sessions } => {
                    // The snapshot is authoritative — it REPLACES rather than merges,
                    // which is what makes a reconnect a complete resync with no
                    // replay protocol.
                    m.sessions = sessions;
                    m.reachable = true;
                    m.detail = None;
                    m.seen = true;
                }
                MachineHubUpdate::Event { event } => {
                    apply_event(m, &event);
                }
                MachineHubUpdate::Down { reason } => {
                    // Sessions are deliberately NOT cleared: the last snapshot stays
                    // on screen, marked stale, until the next connect resyncs it.
                    m.reachable = false;
                    m.detail = Some(reason);
                }
            }
        }
        // Outside the lock: the callback re-enters the app (it emits a Tauri
        // event), and holding a std mutex across that is how a deadlock starts.
        on_change();
    }
}

/// Patch a machine's held snapshot from one feed event.
///
/// Deliberately narrow: this is the activity dimension only. Anything richer
/// belongs to the next snapshot, which is authoritative and arrives on every
/// reconnect.
fn apply_event(m: &mut MachineState, event: &shed_core::rc_events::RcEvent) {
    use shed_core::rc_events::RcEvent;
    match event {
        RcEvent::ActivityChanged {
            slug,
            activity,
            activity_at,
            state,
            ..
        } => {
            if let Some(s) = m.sessions.iter_mut().find(|s| &s.slug == slug) {
                if let Some(a) = activity {
                    s.activity = Some(*a);
                }
                if let Some(at) = activity_at {
                    s.activity_at = Some(at.clone());
                }
                if let Some(st) = state {
                    s.state = *st;
                }
            }
        }
        RcEvent::SessionUpdated {
            slug,
            removed,
            state,
            ..
        } => {
            if *removed {
                m.sessions.retain(|s| &s.slug != slug);
            } else if let Some(s) = m.sessions.iter_mut().find(|s| &s.slug == slug) {
                if let Some(st) = state {
                    s.state = *st;
                }
            }
            // A session that appeared but is not in the snapshot yet is left to
            // the next snapshot rather than synthesized from a partial event —
            // the event body carries a display subset, not a full DTO.
        }
        // Notification-only; the body would come from a targeted fetch, which
        // the sessions VIEW does not need (a watch screen would).
        RcEvent::MessageAppended { .. } => {}
        // Server-synthesized, shed-only: a machine hub never emits these.
        RcEvent::HubUnavailable { .. } | RcEvent::ShedStopped { .. } => {}
    }
}

#[cfg(test)]
mod tests {
    /// A `Machines` with no watchers, over a pre-seeded state map — for the
    /// readers (snapshot/status), which are the part worth testing without a
    /// runtime. One constructor rather than a struct literal per test, so the
    /// fields can change without touching every case.
    fn fixture(state: Arc<Mutex<BTreeMap<String, MachineState>>>, names: &[&str]) -> Machines {
        Machines {
            state,
            reg: Mutex::new(Registry {
                names: names.iter().map(|s| s.to_string()).collect(),
                entries: BTreeMap::new(),
                watchers: Vec::new(),
            }),
            handle: tokio::runtime::Handle::current(),
            test_hub_ports: Default::default(),
            on_change: std::sync::Arc::new(|| {}),
        }
    }

    use super::*;

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

    /// A machine with nothing listening is LISTED, unreachable, with a reason —
    /// never an error and never absent.
    #[tokio::test]
    async fn an_unreachable_machine_is_a_row_not_an_error() {
        // A port nothing can be listening on.
        let ln = std::net::TcpListener::bind("127.0.0.1:0").expect("bind");
        let port = ln.local_addr().expect("addr").port();
        drop(ln);

        let machines = Machines::start(
            &tokio::runtime::Handle::current(),
            &config_with(&["ghost"]),
            &std::collections::HashMap::from([("ghost".to_string(), port)]),
            Arc::new(|| {}),
        );
        assert_eq!(machines.status().len(), 1, "the machine is listed at once");

        // Give the watcher a moment to report Down.
        for _ in 0..100 {
            let status = machines.status();
            if status[0]["detail"].as_str().is_some() {
                assert_eq!(status[0]["reachable"], json!(false));
                assert_eq!(status[0]["connected_once"], json!(false));
                assert_eq!(status[0]["origin"], json!("machine:ghost"));
                assert!(machines.snapshot().0.is_empty());
                return;
            }
            tokio::time::sleep(std::time::Duration::from_millis(50)).await;
        }
        panic!("the machine never reported a reason for being unreachable");
    }

    /// Sessions carry their ORIGIN, and never a shed — the field the UI must not
    /// key on (§3b.1c: two machines' empty sheds would collide).
    // `#[tokio::test]` only for the reactor `fixture` needs; nothing here awaits.
    #[tokio::test]
    async fn sessions_are_stamped_with_their_origin_and_no_shed() {
        let state = Arc::new(Mutex::new(BTreeMap::new()));
        let mut m = MachineState::new();
        m.reachable = true;
        m.seen = true;
        m.sessions = vec![shed_core::rc::decode_session(
            r#"{"slug":"hkn4vd","tmux_session":"rc-hkn4vd","kind":"shell",
                "state":"ready","managed":true,"display_name":"probe"}"#,
        )
        .expect("fixture decodes")];
        state.lock().unwrap().insert("mini3".to_string(), m);

        let machines = fixture(state, &["mini3"]);
        let rows = machines.snapshot().0;
        assert_eq!(rows.len(), 1);
        assert_eq!(rows[0]["origin"], json!("machine:mini3"));
        assert_eq!(rows[0]["origin_kind"], json!("machine"));
        assert_eq!(rows[0]["machine"], json!("mini3"));
        assert_eq!(rows[0]["shed"], json!(""), "a machine session has no shed");
        assert_eq!(rows[0]["stale"], json!(false));
        assert_eq!(rows[0]["slug"], json!("hkn4vd"));
    }

    /// A disconnect marks rows STALE but keeps them on screen — blanking the
    /// machine on every blip is worse than showing a last-known view.
    // `#[tokio::test]` only for the reactor `fixture` needs; nothing here awaits.
    #[tokio::test]
    async fn a_disconnect_marks_rows_stale_without_dropping_them() {
        let state = Arc::new(Mutex::new(BTreeMap::new()));
        let mut m = MachineState::new();
        m.reachable = false;
        m.seen = true;
        m.detail = Some("the hub feed ended".to_string());
        m.sessions = vec![shed_core::rc::decode_session(
            r#"{"slug":"abc123","tmux_session":"rc-abc123","kind":"shell",
                "state":"ready","managed":true,"display_name":"x"}"#,
        )
        .expect("fixture decodes")];
        state.lock().unwrap().insert("mini2".to_string(), m);

        let machines = fixture(state, &["mini2"]);
        let rows = machines.snapshot().0;
        assert_eq!(rows.len(), 1, "rows survive a disconnect");
        assert_eq!(rows[0]["stale"], json!(true));
        let status = machines.status();
        assert_eq!(status[0]["reachable"], json!(false));
        assert_eq!(status[0]["connected_once"], json!(true));
        assert_eq!(status[0]["detail"], json!("the hub feed ended"));
    }

    /// A removal event drops the row; an activity event patches it in place.
    #[test]
    fn events_patch_the_held_snapshot() {
        use shed_core::rc::{RcActivity, RcState};
        use shed_core::rc_events::RcEvent;

        let mut m = MachineState::new();
        m.sessions = vec![shed_core::rc::decode_session(
            r#"{"slug":"abc123","tmux_session":"t","kind":"shell",
                "state":"starting","managed":true,"display_name":"x"}"#,
        )
        .expect("fixture decodes")];

        apply_event(
            &mut m,
            &RcEvent::ActivityChanged {
                // A machine hub always sends an empty shed — the case that used
                // to be dropped at decode entirely.
                shed: String::new(),
                slug: "abc123".into(),
                activity: Some(RcActivity::Working),
                activity_at: Some("2026-08-22T02:05:30Z".into()),
                state: Some(RcState::Ready),
                last_message: None,
            },
        );
        assert_eq!(m.sessions[0].activity, Some(RcActivity::Working));
        assert_eq!(m.sessions[0].state, RcState::Ready);

        apply_event(
            &mut m,
            &RcEvent::SessionUpdated {
                shed: String::new(),
                slug: "abc123".into(),
                activity: None,
                state: None,
                last_message: None,
                lane: None,
                removed: true,
            },
        );
        assert!(m.sessions.is_empty(), "a removal drops the row");
    }
}
