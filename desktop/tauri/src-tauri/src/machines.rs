//! **The `machines:` section** — reading it, writing to it, and turning one
//! entry into a way of reaching that host's `roost-session`.
//!
//! Everything about the *registry* — which hosts exist, one watcher each, the
//! rows they report, the bootstrap — lives in [`crate::roost_hosts`], which
//! covers sheds as well (plan 019 §3.6). What is left here is the half that is
//! genuinely about `machines:` and nothing else:
//!
//! * [`add_from_json`] — the Add dialog's and `machine.add`'s shared path into
//!   the user's `~/.shed/config.yaml`, which this app is a guest in;
//! * [`build_ssh_reach`] / [`build_local_reach`] — a [`MachineEntry`] to a
//!   [`RoostReach`], including the test-mode substitutions.
//!
//! A **shed**'s roost host arrives through the same door: plan 019 §3.6 pins its
//! identity as a synthesized [`MachineEntry`]
//! ([`shed_app::roost::shed_reach_entry`]), so the transport choice below is
//! made once for both kinds of host rather than twice with a chance to differ.

use std::collections::HashMap;
use std::path::PathBuf;
use std::sync::{Arc, Mutex};

use serde_json::Value;

use shed_app::roost::{LocalSession, RoostReach, SshBridge, SshBridgeOptions, UnreachableReach};
use shed_core::config::{MachineEntry, ShedConfig};

use crate::roost_hosts::{lock, RoostHosts};

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
pub const LOCALHOST: &str = "localhost";

/// The reserved-name gate, shared by both doors into [`RoostHosts::add`].
///
/// Checked BEFORE the config write in [`add_from_json`] as well as inside
/// [`RoostHosts::add`]: refusing only at the second step would leave a
/// `localhost:` entry in the user's `~/.shed/config.yaml` that the next launch
/// would silently prefer over the implicit host.
pub(crate) fn reject_reserved_name(name: &str) -> Result<(), String> {
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
    hosts: &RoostHosts,
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
    hosts.add(entry)
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

/// **How a host is reached** — kept beside its reach so a consumer that needs
/// a TRANSPORT of its own can pick one (plan 015 §3.4).
///
/// [`RoostReach`] deliberately answers only "give me a roost connection": the
/// production [`SshBridge`] runs `roost-session client-bridge` over ssh and hands
/// back a `RoostEndpoint::Unix`, exactly as the test-mode [`LocalSession`] does,
/// so nothing on that trait says whether the far side is this machine or a host
/// three hops away. The opencode lane has to know: a LOCAL machine's agent
/// server is dialable at the loopback address it reported, and a REMOTE one's is
/// only reachable through an `ssh -N -L` tunnel to that same port.
///
/// It is derived from the reach that was BUILT, not from the host's name. A
/// configured entry named `localhost` is an ssh target here (see
/// [`crate::roost_hosts`]'s "implicit `localhost` host": the user spelling it in
/// `machines:` means an ssh target they chose, and its roost reach is an
/// [`SshBridge`] like any other) — so the lane tunnels to it rather than
/// assuming it is this host.
#[derive(Debug, Clone)]
pub enum ReachKind {
    /// This machine: the implicit [`LOCALHOST`] host, or a test-mode socket map
    /// entry. Its loopback ports are OUR loopback ports.
    Local,
    /// An ssh target, carrying the config entry a forward is composed from.
    Ssh(MachineEntry),
}

/// A started reach and how it gets there.
pub(crate) struct Registered {
    pub(crate) reach: Arc<dyn RoostReach>,
    pub(crate) kind: ReachKind,
}

/// Everything the transport choice reads that is not the entry itself — the two
/// test-mode seams, resolved once by [`crate::env::Env`] and passed down rather
/// than read here.
#[derive(Clone, Default)]
pub struct ReachOptions {
    /// `SHED_TAURI_ROOST_SOCKETS`: per-host `roost-session` sockets, reached
    /// directly instead of through roost's SSH client-bridge.
    pub roost_sockets: HashMap<String, PathBuf>,
    /// `SHED_TAURI_SSH_BIN`: the `ssh` a test-mode run execs (plan 019 §3.6).
    /// `None` in production, where it is `ssh` on the PATH.
    pub ssh_bin: Option<PathBuf>,
    /// Whether test mode is on at all. It is what turns the two seams above
    /// from "unset" into "refuse to spawn ssh", which is the hermeticity
    /// promise — see [`build_ssh_reach`].
    pub test_mode: bool,
}

impl ReachOptions {
    /// The bridge options a reach is built with. **Shared with the bootstrap's
    /// [`shed_app::roost::SshExec`]** (plan 019 §3.6: one host-key posture, two
    /// ssh stacks).
    pub fn bridge_options(&self) -> SshBridgeOptions {
        SshBridgeOptions {
            ssh_bin: self.ssh_bin.clone(),
            ..SshBridgeOptions::default()
        }
    }
}

/// The transport choice — the ONLY per-client part of reaching a host's
/// `roost-session`.
///
/// Production is [`SshBridge`]: roost's own client-bridge over a shared
/// `ControlMaster`, because a roost-session's socket path is resolved on the FAR
/// side and so cannot be named in an `ssh -L`.
///
/// ## The test-mode rule, and why it is not "only when a map is set"
///
/// In test mode a host is reached ONLY through something the harness supplied:
/// its own socket in [`ReachOptions::roost_sockets`], or a fake `ssh` in
/// [`ReachOptions::ssh_bin`]. Anything else is [`UnreachableReach`] — the
/// everyday asleep/off-network state, coverable with no real host, and a
/// guarantee that a hermetic run never spawns an `ssh` child.
///
/// It used to be conditional on the socket map being non-empty, which was
/// enough while only `machines:` entries were watched: a suite with no machines
/// had nothing to reach. Plan 019 gives every RUNNING SHED a roost host too, so
/// that rule would have every hermetic suite in the harness dial
/// `shed@127.0.0.1:2222` the moment the mock server reported a running shed.
pub(crate) fn build_ssh_reach(
    entry: &MachineEntry,
    options: &ReachOptions,
) -> Result<Registered, String> {
    if !options.test_mode || options.ssh_bin.is_some() {
        return SshBridge::new(entry, options.bridge_options())
            .map(|b| Registered {
                reach: Arc::new(b) as Arc<dyn RoostReach>,
                kind: ReachKind::Ssh(entry.clone()),
            })
            .map_err(|e| e.to_string());
    }
    // Test mode: reach the harness's fake session on its own Unix socket. That
    // needs no transport at all, which is the point — everything ABOVE the
    // socket is the shared code under test.
    Ok(mapped_reach(
        &entry.name,
        options,
        ReachKind::Ssh(entry.clone()),
    ))
}

/// The implicit [`LOCALHOST`] host's reach: this machine's own session socket,
/// resolved by shed's own path table (roost's resolver picks the `-dev` socket
/// from the CONSUMING crate's build profile, which would make a debug build of
/// this app read a different session than a release one).
///
/// It goes through the same test-mode map as a configured machine, so a hermetic
/// run reads the harness's fake session rather than the developer's real one.
pub(crate) fn build_local_reach(options: &ReachOptions) -> Registered {
    if !options.test_mode {
        return Registered {
            reach: Arc::new(LocalSession::default_local()),
            kind: ReachKind::Local,
        };
    }
    mapped_reach(LOCALHOST, options, ReachKind::Local)
}

/// The test-mode reach for `name`, and the kind that goes with it.
///
/// A MAPPED host is [`ReachKind::Local`] — the harness's fakes (roost's and
/// opencode's) both live in this process's loopback space, which is precisely
/// what the map declares, and it is how the lane's cells reach an opencode
/// server with no ssh anywhere. An UNMAPPED one keeps `unmapped`, the kind it
/// would have had in production: it is permanently unreachable, reports no
/// sessions and so never reaches the lane at all, and claiming it was local
/// would be a lie about a host nobody described.
fn mapped_reach(name: &str, options: &ReachOptions, unmapped: ReachKind) -> Registered {
    match options.roost_sockets.get(name) {
        Some(socket) => Registered {
            reach: Arc::new(LocalSession::new(name, socket.clone())),
            kind: ReachKind::Local,
        },
        None => Registered {
            reach: Arc::new(UnreachableReach::new(
                name,
                "no roost-session mapped for this host in test mode",
            )),
            kind: unmapped,
        },
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn entry(name: &str) -> MachineEntry {
        MachineEntry {
            name: name.to_string(),
            host: "127.0.0.1".to_string(),
            user: Some("nobody".to_string()),
            ssh_port: 22,
            known_hosts: None,
            rc_bin: None,
        }
    }

    /// **A hermetic run never spawns `ssh`.**
    ///
    /// The one promise `SHED_TAURI_ROOST_SOCKETS` makes, and since plan 019 it
    /// has to hold for a suite that mapped NOTHING as well: every running shed
    /// is a roost host now, so an empty map used to mean "dial the mock's
    /// sheds over real ssh".
    #[test]
    fn test_mode_refuses_to_build_a_real_bridge_for_an_unmapped_host() {
        let options = ReachOptions {
            test_mode: true,
            ..ReachOptions::default()
        };
        let built = build_ssh_reach(&entry("mini3"), &options).expect("a reach");
        assert_eq!(
            built.reach.label(),
            "mini3",
            "an unmapped host is a labelled refusal, not a bridge"
        );
        assert!(
            matches!(built.kind, ReachKind::Ssh(_)),
            "it keeps the kind it would have had in production"
        );

        // The same host, with a fake `ssh` supplied: now it IS a bridge, because
        // the harness said which binary an exec may reach.
        let with_ssh = ReachOptions {
            test_mode: true,
            ssh_bin: Some(PathBuf::from("/nonexistent/fake-ssh")),
            ..ReachOptions::default()
        };
        let built = build_ssh_reach(&entry("mini3"), &with_ssh).expect("a reach");
        assert_eq!(built.reach.label(), "mini3");
        assert_eq!(
            with_ssh.bridge_options().ssh_bin,
            Some(PathBuf::from("/nonexistent/fake-ssh")),
            "and the fake is what its execs run"
        );
    }

    /// A mapped host reads as LOCAL whatever it is configured as — the lane
    /// layer's transport choice follows the reach that was built.
    #[test]
    fn a_mapped_host_is_local_and_an_unmapped_local_stays_unreachable() {
        let mut sockets = HashMap::new();
        sockets.insert("mini3".to_string(), PathBuf::from("/tmp/roost.sock"));
        let options = ReachOptions {
            roost_sockets: sockets,
            test_mode: true,
            ..ReachOptions::default()
        };
        let built = build_ssh_reach(&entry("mini3"), &options).expect("a reach");
        assert!(matches!(built.kind, ReachKind::Local));

        let local = build_local_reach(&options);
        assert!(
            matches!(local.kind, ReachKind::Local),
            "an unmapped localhost is still local — it is this machine"
        );
        assert_eq!(local.reach.label(), LOCALHOST);
    }

    #[test]
    fn the_reserved_name_is_refused_by_name() {
        assert!(reject_reserved_name("mini3").is_ok());
        let refusal = reject_reserved_name(LOCALHOST).expect_err("localhost is reserved");
        assert!(refusal.contains("localhost"), "{refusal}");
    }
}
