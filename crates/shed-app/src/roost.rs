//! **The roost reach seam** and the polling inventory watcher on top of it
//! (plan 013 S1, the Roost Pivot's first milestone).
//!
//! [`shed_core::roost`] knows how to *talk* to a `roost-session` — one
//! [`Conn`][shed_core::roost::Conn], typed ops, the compatibility gate, the row
//! model. What it deliberately does not know is how a given client *gets* to
//! one. This module is that half, and it is the exact analogue of
//! [`crate::machine`]: the pure wire lives in `shed-core`, and what is left —
//! the part that genuinely differs per client — is one trait.
//!
//! ## The seam is an endpoint, not a port
//!
//! [`crate::machine::MachineForward`] hands back a `u16` and promises it never
//! moves, because a hub answers on a fixed remote port and every client's job is
//! to get a local socket pointed at it. A roost-session is not like that:
//!
//! | client | how it reaches a session |
//! |---|---|
//! | desktop, local | [`LocalSession`] — the session's own Unix socket, if one is there |
//! | desktop, a machine | [`SshBridge`] — roost's own [`SshTunnel`][roost_ipc::ssh::SshTunnel], whose `bridge.sock` lives in a **per-attempt** scratch directory |
//! | shed-mobile | a `dartssh2` bridge on the Dart side; Rust is handed the port ([`LabelledPort`], or the pinned [`FixedPort`]) |
//!
//! The middle row is why [`RoostReach::ensure`] returns a
//! [`RoostEndpoint`] rather than a fixed address. roost names each connect
//! attempt's scratch directory for the attempt (`roost-ssh-<host>-<pid>-<seq>`)
//! precisely so a disconnect racing the reconnect behind it cannot delete the
//! winner's files — which means the bridge socket's *path* legitimately moves
//! across a re-establish. A port-shaped seam would have to fight that; an
//! endpoint-shaped one just asks again.
//!
//! ## `invalidate` is not a probe
//!
//! [`RoostReach::invalidate`] is called after **any** request error, and the
//! next [`RoostReach::ensure`] rebuilds rather than trusting a liveness check.
//! That is deliberate: an SSH bridge socket is a local `UnixListener` that goes
//! on accepting connections perfectly happily after the `ssh` master behind it
//! has died — the failure only surfaces when the far side never answers. "Can I
//! connect to it" is therefore not the question worth asking, and a reach that
//! asked it would hand back a socket that accepts and then hangs.

use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::sync::Arc;
use std::time::Duration;

use roost_ipc::messages::{Tab, TabDumpResult, TabOpenParams};
use roost_ipc::ssh::{
    classify, ResolvedTransport, SshConfigPaths, SshTarget, SshTunnel, SshTunnelOptions,
};
use tokio::sync::mpsc;

use shed_core::config::MachineEntry;
use shed_core::roost::{local_session_socket, Conn, RoostError, RoostInventory};

use crate::backoff;
use crate::machine::{FixedPort, ForwardError};

/// Re-exported so a client that drives roost never has to name `shed_core`'s
/// module as well as this one: the endpoint shape the seam hands back, the
/// launch argv for a kind, and the capabilities a roost host advertises (roost
/// has no `shed-ext-rc` to probe, so they are synthesized).
pub use shed_core::roost::{launch_argv, roost_capabilities, RoostEndpoint};

// ---------------------------------------------------------------------------
// the reach seam
// ---------------------------------------------------------------------------

/// A way to reach one roost-session.
///
/// **Contract:** `ensure` is idempotent and may return a *different* endpoint
/// than last time (see the module doc — an SSH bridge's socket moves across a
/// re-establish); `invalidate` marks whatever is held as not to be trusted, so
/// the next `ensure` rebuilds it from scratch. Neither ever spawns a
/// roost-session: roost's rule is connect-if-present, and a client that started
/// somebody's session behind their back would be inventing state.
#[async_trait::async_trait]
pub trait RoostReach: Send + Sync {
    /// What to call this reach in a failure message. Not the row label — the
    /// watcher stamps rows from its own `label` argument — this one answers
    /// "which reach could not be built".
    fn label(&self) -> &str;

    /// Make the reach usable and say where to dial.
    async fn ensure(&self) -> Result<RoostEndpoint, ForwardError>;

    /// Mark the held transport as suspect. Cheap and infallible: the rebuild
    /// happens in the next [`RoostReach::ensure`], where it can fail properly.
    async fn invalidate(&self);
}

/// This machine's own `roost-session`, if one is running.
///
/// **Never spawns.** A missing socket is an ordinary state — most machines are
/// not running a session most of the time — so it is a plain
/// [`ForwardError`] naming every path that was looked at, not a fault.
pub struct LocalSession {
    label: String,
    socket: PathBuf,
    /// Every candidate the resolver considered, so the failure message names the
    /// place a session would normally be rather than saying "not found".
    tried: Vec<PathBuf>,
}

impl LocalSession {
    /// A session at a known socket path.
    pub fn new(label: impl Into<String>, socket: impl Into<PathBuf>) -> LocalSession {
        let socket = socket.into();
        LocalSession {
            label: label.into(),
            tried: vec![socket.clone()],
            socket,
        }
    }

    /// The local session, resolved through [`shed_core::roost::paths`] — which
    /// shed owns rather than roost's own resolver, because roost's picks the
    /// `-dev` socket from the *consuming* crate's build profile.
    ///
    /// Labelled `localhost`, the name the clients show this host under.
    pub fn default_local() -> LocalSession {
        let resolved = local_session_socket();
        LocalSession {
            label: "localhost".to_string(),
            socket: resolved.path,
            tried: resolved.tried,
        }
    }

    /// Where this reach dials.
    pub fn socket(&self) -> &Path {
        &self.socket
    }
}

#[async_trait::async_trait]
impl RoostReach for LocalSession {
    fn label(&self) -> &str {
        &self.label
    }

    async fn ensure(&self) -> Result<RoostEndpoint, ForwardError> {
        if self.socket.exists() {
            return Ok(RoostEndpoint::Unix(self.socket.clone()));
        }
        Err(ForwardError(format!(
            "no roost-session at {}",
            self.tried
                .iter()
                .map(|p| p.display().to_string())
                .collect::<Vec<_>>()
                .join(", ")
        )))
    }

    async fn invalidate(&self) {}
}

/// A loopback port somebody else already pointed at a session, with a name.
///
/// shed-mobile's reach: Dart stands up the `dartssh2` bridge, keeps it working
/// across a network change, and hands Rust the port — so `ensure` has nothing to
/// do and `invalidate` nothing to rebuild. The `label` is the machine's name,
/// which is what makes a failure message read in the vocabulary the user typed.
pub struct LabelledPort {
    label: String,
    port: u16,
}

impl LabelledPort {
    pub fn new(label: impl Into<String>, port: u16) -> LabelledPort {
        LabelledPort {
            label: label.into(),
            port,
        }
    }

    pub fn port(&self) -> u16 {
        self.port
    }
}

#[async_trait::async_trait]
impl RoostReach for LabelledPort {
    fn label(&self) -> &str {
        &self.label
    }

    async fn ensure(&self) -> Result<RoostEndpoint, ForwardError> {
        Ok(RoostEndpoint::TcpLoopback(self.port))
    }

    async fn invalidate(&self) {}
}

/// The hub seam's [`FixedPort`] reaches a roost-session too — same port, same
/// "somebody else owns it" contract. It carries no name of its own, so its label
/// is a constant; a client with a machine name to report should use
/// [`LabelledPort`].
#[async_trait::async_trait]
impl RoostReach for FixedPort {
    fn label(&self) -> &str {
        "fixed-port"
    }

    async fn ensure(&self) -> Result<RoostEndpoint, ForwardError> {
        Ok(RoostEndpoint::TcpLoopback(self.0))
    }

    async fn invalidate(&self) {}
}

/// A reach that is known not to exist, and says why.
///
/// The honest answer for a machine nothing has been mapped for — the Tauri
/// harness's unmapped-machine case, and any client that wants a row rendered as
/// unreachable-with-a-reason rather than silently missing.
pub struct UnreachableReach {
    label: String,
    reason: String,
}

impl UnreachableReach {
    pub fn new(label: impl Into<String>, reason: impl Into<String>) -> UnreachableReach {
        UnreachableReach {
            label: label.into(),
            reason: reason.into(),
        }
    }
}

#[async_trait::async_trait]
impl RoostReach for UnreachableReach {
    fn label(&self) -> &str {
        &self.label
    }

    async fn ensure(&self) -> Result<RoostEndpoint, ForwardError> {
        Err(ForwardError(self.reason.clone()))
    }

    async fn invalidate(&self) {}
}

// ---------------------------------------------------------------------------
// the SSH bridge
// ---------------------------------------------------------------------------

/// A live SSH transport to one machine's roost-session, as this module needs it.
///
/// Two methods, because two are all [`SshBridge`] uses — and behind a trait
/// because the production implementation spawns `ssh` child processes, which a
/// unit test cannot assert against without either a real host or a fake binary.
#[async_trait::async_trait]
pub trait Tunnel: Send + Sync {
    /// The local Unix socket a client dials. Bound only after the tunnel has
    /// been established.
    fn bridge_socket(&self) -> &Path;

    /// Close the mux and remove the scratch directory. Idempotent.
    async fn shutdown(&self);
}

#[async_trait::async_trait]
impl Tunnel for SshTunnel {
    fn bridge_socket(&self) -> &Path {
        SshTunnel::bridge_socket(self)
    }

    async fn shutdown(&self) {
        SshTunnel::shutdown(self).await;
    }
}

/// How a [`SshBridge`] gets a [`Tunnel`] — the seam a test replaces.
///
/// One method rather than roost's two (`open` then `establish`), because the two
/// are never useful apart here: a tunnel that opened but did not establish has
/// no bound socket and nothing to hand back, and the caller's only sane move is
/// to tear it down. Folding them keeps that teardown in one place.
#[async_trait::async_trait]
pub trait TunnelOpener: Send + Sync {
    async fn open(
        &self,
        host_id: &str,
        target: &SshTarget,
        options: SshTunnelOptions,
    ) -> Result<Box<dyn Tunnel>, String>;
}

/// The production opener: roost's own [`SshTunnel`], opened and established.
pub struct SystemSshTunnels;

#[async_trait::async_trait]
impl TunnelOpener for SystemSshTunnels {
    async fn open(
        &self,
        host_id: &str,
        target: &SshTarget,
        options: SshTunnelOptions,
    ) -> Result<Box<dyn Tunnel>, String> {
        let tunnel = SshTunnel::open(host_id, target, options)
            .await
            .map_err(|e| e.to_string())?;
        if let Err(e) = tunnel.establish().await {
            // The async teardown, not `Drop`'s blocking one: we are already in a
            // runtime, and roost's `Drop` explicitly exists for the case where
            // nobody could await. An establish that failed still owns a scratch
            // directory and possibly a `ControlPersist` master.
            tunnel.shutdown().await;
            return Err(e.to_string());
        }
        Ok(Box::new(tunnel))
    }
}

/// Everything [`SshBridge`] would otherwise read from the environment.
///
/// A bundle rather than lookups so nothing about a shipped shed is steered by a
/// variable meant for roost's own test lane. In particular this module NEVER
/// calls [`SshTunnelOptions::from_env`]: that reads `ROOST_SSH_BIN` and
/// `ROOST_TEST_MODE`, and the latter sets `jail_fs_root`, which decides which
/// remote binary the exec chain resolves. shed pins it to `false`.
#[derive(Clone)]
pub struct SshBridgeOptions {
    /// The `ssh` binary. `None` → `ssh` on the PATH.
    pub ssh_bin: Option<PathBuf>,
    /// Candidate parents for roost's per-attempt scratch directory, in
    /// preference order. Empty → `$TMPDIR` then `/tmp`, roost's own order.
    pub scratch_parents: Vec<PathBuf>,
    /// The `ssh_config` files roost's generated config includes. `None` →
    /// `$HOME/.ssh/config` + `/etc/ssh/ssh_config`.
    pub config_paths: Option<SshConfigPaths>,
    /// How a tunnel is obtained. The test seam.
    pub opener: Arc<dyn TunnelOpener>,
}

impl Default for SshBridgeOptions {
    fn default() -> SshBridgeOptions {
        SshBridgeOptions {
            ssh_bin: None,
            scratch_parents: Vec::new(),
            config_paths: None,
            opener: Arc::new(SystemSshTunnels),
        }
    }
}

/// `$TMPDIR` then `/tmp` — roost's own candidate order, spelled here rather than
/// taken from [`SshTunnelOptions::from_env`] so the two variables that steer the
/// exec chain are never read alongside it.
fn default_scratch_parents() -> Vec<PathBuf> {
    let mut parents: Vec<PathBuf> = Vec::new();
    if let Some(tmpdir) = std::env::var_os("TMPDIR").filter(|value| !value.is_empty()) {
        parents.push(PathBuf::from(tmpdir));
    }
    let fallback = PathBuf::from("/tmp");
    if !parents.contains(&fallback) {
        parents.push(fallback);
    }
    parents
}

/// A directory this process owns and removes when the value dies.
///
/// Hand-rolled rather than `tempfile` because it is used by the *library*, and
/// [`SshBridge`] must not add a non-dev dependency to `shed-app` for four lines
/// of `create_dir_all`. Mode 0700: it holds an `ssh_config` naming a
/// `known_hosts` file, which is a host-key pinning decision, and a
/// world-writable one would be worth nothing.
struct ScratchDir(PathBuf);

impl ScratchDir {
    fn new(name: &str) -> std::io::Result<ScratchDir> {
        use std::os::unix::fs::DirBuilderExt as _;

        static NEXT: AtomicU64 = AtomicU64::new(0);
        let path = std::env::temp_dir().join(format!(
            "shed-roost-{name}-{}-{}",
            std::process::id(),
            NEXT.fetch_add(1, Ordering::Relaxed)
        ));
        let _ = std::fs::remove_dir_all(&path);
        std::fs::DirBuilder::new().mode(0o700).create(&path)?;
        Ok(ScratchDir(path))
    }
}

impl Drop for ScratchDir {
    fn drop(&mut self) {
        let _ = std::fs::remove_dir_all(&self.0);
    }
}

/// One machine's roost-session, over roost's own SSH client-bridge.
///
/// ## Why not `ssh -N -L`
///
/// The reach `sx` and the Tauri app use for a machine's RC hub is a local
/// forward onto a known remote port. That is not available here: a
/// roost-session's socket path is resolved on the *far* side (XDG runtime dir,
/// uid, the `-dev` sibling) and never sent, so there is nothing to name in a
/// `-L`. roost's answer — a local `bridge.sock` whose every accepted connection
/// gets its own `ssh -T <target> "… exec roost-session client-bridge"` over a
/// shared `ControlMaster` — is the transport, and reusing it means the candidate
/// ladder and the failure classification behind it come from the pin.
///
/// ## The re-establish order is load-bearing
///
/// [`RoostReach::ensure`] on an invalidated bridge **shuts the old tunnel down
/// and drops it first**, then opens a fresh one. Not an optimisation: roost's
/// `open` sweeps the host's older scratch directories and refuses to reclaim one
/// whose `bridge.sock` still answers ("another Roost owns this target"). Opening
/// before tearing down would make the bridge refuse to rebuild itself.
pub struct SshBridge {
    label: String,
    /// The machine name, sanitized into the token roost names its scratch
    /// directory with.
    host_id: String,
    target: SshTarget,
    options: SshTunnelOptions,
    opener: Arc<dyn TunnelOpener>,
    /// The held tunnel. A `tokio` mutex, and it is held across the open: two
    /// concurrent `ensure`s must not each spawn a tunnel to the same machine —
    /// the loser's would be a live `ssh` master nothing ever tears down.
    tunnel: tokio::sync::Mutex<Option<Box<dyn Tunnel>>>,
    invalidated: AtomicBool,
    /// Holds the generated per-machine `ssh_config` when the entry pins a
    /// `known_hosts` file. Never read — it is owned for its `Drop`, which is
    /// what removes the directory when the bridge goes.
    _config_dir: Option<ScratchDir>,
    config_path: Option<PathBuf>,
}

impl SshBridge {
    /// Build a bridge for one `machines:` entry. Nothing is spawned until
    /// [`RoostReach::ensure`] runs.
    pub fn new(entry: &MachineEntry, options: SshBridgeOptions) -> Result<SshBridge, ForwardError> {
        let SshBridgeOptions {
            ssh_bin,
            scratch_parents,
            config_paths,
            opener,
        } = options;
        let target = ssh_target(entry)?;
        let host_id = host_id(&entry.name);

        let base = config_paths.unwrap_or_else(SshConfigPaths::from_env);
        let (config_dir, config_path, tunnel_config_paths) = match entry.known_hosts.as_deref() {
            Some(known_hosts) if !known_hosts.is_empty() => {
                let dir = ScratchDir::new(&host_id).map_err(|e| {
                    ForwardError(format!(
                        "machine:{}: could not create the ssh config directory: {e}",
                        entry.name
                    ))
                })?;
                let path = dir.0.join("ssh_config");
                let body =
                    pinned_ssh_config(&target_host(entry), known_hosts, base.user.as_deref());
                write_private(&path, body.as_bytes()).map_err(|e| {
                    ForwardError(format!(
                        "machine:{}: could not write {}: {e}",
                        entry.name,
                        path.display()
                    ))
                })?;
                let paths = SshConfigPaths {
                    user: Some(path.clone()),
                    system: base.system,
                };
                (Some(dir), Some(path), paths)
            }
            // No pin: the user's own config and strictness apply, exactly as for
            // any other `ssh` to this host.
            _ => (None, None, base),
        };

        let scratch_parents = if scratch_parents.is_empty() {
            default_scratch_parents()
        } else {
            scratch_parents
        };

        Ok(SshBridge {
            label: entry.name.clone(),
            host_id,
            target,
            options: SshTunnelOptions {
                config_paths: tunnel_config_paths,
                scratch_parents,
                ssh_bin: ssh_bin.unwrap_or_else(|| "ssh".into()),
                // Never from the environment. `ROOST_TEST_MODE` steers which
                // remote binary roost's exec chain resolves; a shed that read it
                // would exec whatever a stray variable pointed at.
                jail_fs_root: false,
            },
            opener,
            tunnel: tokio::sync::Mutex::new(None),
            invalidated: AtomicBool::new(false),
            _config_dir: config_dir,
            config_path,
        })
    }

    /// The ssh target string roost was handed — `ssh://[user@]host[:port]`.
    pub fn target(&self) -> &str {
        &self.target.raw
    }

    /// The generated per-machine `ssh_config`, when the entry pinned a
    /// `known_hosts` file. `None` when it did not.
    pub fn ssh_config_path(&self) -> Option<&Path> {
        self.config_path.as_deref()
    }
}

#[async_trait::async_trait]
impl RoostReach for SshBridge {
    fn label(&self) -> &str {
        &self.label
    }

    async fn ensure(&self) -> Result<RoostEndpoint, ForwardError> {
        let mut held = self.tunnel.lock().await;
        if !self.invalidated.load(Ordering::SeqCst) {
            if let Some(tunnel) = held.as_ref() {
                return Ok(RoostEndpoint::Unix(tunnel.bridge_socket().to_path_buf()));
            }
        }
        // Shut down BEFORE opening — see the type's doc: roost refuses to
        // reclaim a scratch directory whose bridge socket still answers.
        if let Some(tunnel) = held.take() {
            tunnel.shutdown().await;
            drop(tunnel);
        }
        // Cleared before the open, not after: an `invalidate` that lands while
        // this open is in flight must survive it, or the tunnel it was warning
        // about would be kept.
        self.invalidated.store(false, Ordering::SeqCst);

        let tunnel = self
            .opener
            .open(&self.host_id, &self.target, self.options.clone())
            .await
            .map_err(|e| ForwardError(format!("machine:{}: {e}", self.label)))?;
        let socket = tunnel.bridge_socket().to_path_buf();
        *held = Some(tunnel);
        Ok(RoostEndpoint::Unix(socket))
    }

    async fn invalidate(&self) {
        self.invalidated.store(true, Ordering::SeqCst);
    }
}

/// The `[user@]host` half of the target — what an `ssh_config` `Host` pattern
/// has to match, which is the host as written and never the user or the port.
fn target_host(entry: &MachineEntry) -> String {
    if entry.host.is_empty() {
        entry.name.clone()
    } else {
        entry.host.clone()
    }
}

/// `ssh://[user@]host[:port]` from a `machines:` entry, classified by roost.
///
/// The scheme is always spelled, for two reasons. It is the only form roost's
/// `classify` accepts a port in — a bare `host:port` is explicitly refused — and
/// it keeps the string away from `classify`'s `localhost` sentinel, which
/// resolves the LOCAL session socket through roost's own build-profile-sensitive
/// resolver. A machine named `localhost` is still an ssh target here.
///
/// The user is omitted when the entry names none and the port when it is 22, so
/// a bare `~/.ssh/config` alias reaches `ssh` as itself and is resolved by `ssh`.
///
/// An **IPv6 literal is bracketed** here and nowhere else. `2001:db8::1` with
/// port 2222 written plainly is `ssh://2001:db8::1:2222`, whose authority every
/// parser — roost's own `split_host_port`, and `ssh`'s — reads from the right as
/// host `2001:db8:` port `:1:2222` or worse. The brackets are the URL form's
/// only way to say where the address ends, which is why roost's own refusal
/// message spells `ssh://[::1]:22`. The `Host` line of the generated pin config
/// keeps the address UNBRACKETED ([`target_host`]) because that is the hostname
/// `ssh` extracts and matches patterns against.
fn ssh_target(entry: &MachineEntry) -> Result<SshTarget, ForwardError> {
    let host = target_host(entry);
    if host.trim().is_empty() {
        return Err(ForwardError(format!(
            "machine:{}: has no host to reach",
            entry.name
        )));
    }
    let user = match entry.user.as_deref().filter(|u| !u.is_empty()) {
        Some(user) => format!("{user}@"),
        None => String::new(),
    };
    let port = if entry.ssh_port == 0 || entry.ssh_port == 22 {
        String::new()
    } else {
        format!(":{}", entry.ssh_port)
    };
    let raw = format!("ssh://{user}{}{port}", url_host(&host));
    match classify(&raw) {
        Ok(ResolvedTransport::Ssh(target)) => Ok(target),
        Ok(other) => Err(ForwardError(format!(
            "machine:{}: {raw} is not an ssh target ({other:?})",
            entry.name
        ))),
        Err(e) => Err(ForwardError(format!("machine:{}: {e}", entry.name))),
    }
}

/// The host as the authority of an `ssh://` URL: an IPv6 literal bracketed,
/// anything else untouched.
///
/// The `:` test is the whole rule. A hostname and an `ssh_config` alias cannot
/// contain a colon, an IPv4 address cannot either, and an IPv6 literal always
/// does — so a colon in a host is an address that needs delimiting, and there is
/// no case where bracketing something else would be right. An address the user
/// already bracketed is left as it is rather than double-wrapped.
fn url_host(host: &str) -> String {
    if host.contains(':') && !(host.starts_with('[') && host.ends_with(']')) {
        format!("[{host}]")
    } else {
        host.to_string()
    }
}

/// The machine name as a scratch-directory token.
///
/// roost reads its own directory names back as `<host_id>-<pid>-<seq>`, parsed
/// from the right, so a `-` inside the id is fine — but a `/` would make the
/// leaf a path and an empty id would make the name unparseable. The length cap
/// is the `sun_path` budget: the directory holds a `bridge.sock`, and 103 bytes
/// is the whole of it.
fn host_id(name: &str) -> String {
    let sanitized: String = name
        .chars()
        .take(32)
        .map(|c| {
            if c.is_ascii_alphanumeric() || matches!(c, '.' | '_' | '-') {
                c
            } else {
                '-'
            }
        })
        .collect();
    if sanitized.is_empty() {
        "machine".to_string()
    } else {
        sanitized
    }
}

/// The per-machine `ssh_config` that pins a host key.
///
/// Ordering is the whole content of this function. `ssh` takes the **first**
/// value it obtains for a keyword, and roost's generated wrapper `Include`s this
/// file first — so the pin block goes at the top, where it beats anything the
/// user's own config says about `UserKnownHostsFile` or
/// `StrictHostKeyChecking` for this host, and the user's config is included
/// *after* it, where it still supplies the `HostName`/`Port`/`IdentityFile` an
/// alias needs.
///
/// The `Include` line is kept (rather than dropped as a double-include) because
/// roost includes *this file*, not `~/.ssh/config`: passing this as
/// `config_paths.user` displaces the user's own config, and without the line
/// nothing would resolve their aliases. `~` is expanded here rather than left to
/// `ssh` so the file says exactly which path it means.
///
/// **Both paths are quoted.** `UserKnownHostsFile` takes a *list* of files, so
/// `ssh` splits its argument on whitespace: an unquoted
/// `/Users/me/Library/Application Support/shed/known_hosts` becomes two
/// half-paths, neither of which exists, and the pin silently degrades into
/// trusting nothing — with `StrictHostKeyChecking yes` above it, into refusing
/// every connection. Double quotes are ssh_config's own escape for exactly this.
fn pinned_ssh_config(host: &str, known_hosts: &str, user_config: Option<&Path>) -> String {
    let mut out = format!(
        "Host {host}\n  UserKnownHostsFile \"{}\"\n  StrictHostKeyChecking yes\n",
        expand_tilde(known_hosts).display()
    );
    // Only an existing file: an `Include` of a path that is not there is at best
    // silently ignored and at worst an error, and neither is worth risking on
    // every connection to a host whose user simply has no `~/.ssh/config`.
    if let Some(user_config) = user_config.filter(|path| path.exists()) {
        out.push_str(&format!("Include \"{}\"\n", user_config.display()));
    }
    out
}

/// `~` / `~/x` against `$HOME`. Anything else is returned as it was.
fn expand_tilde(path: &str) -> PathBuf {
    let Some(rest) = path.strip_prefix('~') else {
        return PathBuf::from(path);
    };
    let Some(home) = std::env::var_os("HOME").filter(|home| !home.is_empty()) else {
        return PathBuf::from(path);
    };
    let rest = rest.strip_prefix('/').unwrap_or(rest);
    if rest.is_empty() {
        PathBuf::from(home)
    } else {
        PathBuf::from(home).join(rest)
    }
}

/// Write a file only this user can read. It names a host-key pin.
fn write_private(path: &Path, contents: &[u8]) -> std::io::Result<()> {
    use std::io::Write as _;
    use std::os::unix::fs::OpenOptionsExt as _;

    let mut file = std::fs::OpenOptions::new()
        .write(true)
        .create(true)
        .truncate(true)
        .mode(0o600)
        .open(path)?;
    file.write_all(contents)
}

// ---------------------------------------------------------------------------
// the watcher
// ---------------------------------------------------------------------------

/// How often a held connection re-reads `tab.list`.
///
/// **Polling is pinned for M1.** roost's push feed (`events.subscribe`) is
/// lease-gated, and the lease is singular: a shed subscription would take it
/// from the roost UI the user is looking at. roost R1 re-cuts that, and the
/// migration is a swap of this loop's body onto `Conn::subscribe` + `Fence` —
/// [`RoostUpdate`] does not change.
pub const POLL_INTERVAL: Duration = Duration::from_secs(2);

/// Override for [`POLL_INTERVAL`], in milliseconds. Read **once**, when a
/// watcher is spawned — the harness sets it, a shipped client does not.
pub const POLL_MS_ENV: &str = "SHED_ROOST_POLL_MS";

/// The floor the override is clamped to. A zero or one-millisecond poll is a
/// busy loop against somebody's session, over SSH.
const MIN_POLL: Duration = Duration::from_millis(10);

fn poll_interval() -> Duration {
    std::env::var(POLL_MS_ENV)
        .ok()
        .and_then(|raw| raw.trim().parse::<u64>().ok())
        .map(|ms| Duration::from_millis(ms).max(MIN_POLL))
        .unwrap_or(POLL_INTERVAL)
}

/// One update from a [`RoostWatcher`].
///
/// Two members, not three: roost's inventory is read whole on every poll, so
/// there is no patch stream to fold and no partial update to reconcile. When R1
/// lands the event feed, batches are folded into the inventory behind this same
/// enum ([`shed_core::roost::fence`] is the fold) rather than published as their
/// own variant — the consumer does not change.
#[derive(Debug, Clone, PartialEq)]
pub enum RoostUpdate {
    /// A complete inventory. Emitted on the first `tab.list` of every
    /// connection, and afterwards only when `(daemon_session_id, revision)`
    /// changed.
    Snapshot(RoostInventory),
    /// The session is not readable: no socket, the tunnel would not build, the
    /// thing on the other end is not a roost-session, or a request failed.
    ///
    /// **A normal state, not an error.** A machine that is asleep, or simply
    /// runs no roost-session, is expected — the consumer renders the row as
    /// stale-with-a-reason.
    Down { reason: String },
}

/// A reconnecting, polling watcher over one roost-session's inventory.
///
/// Deliberately the same shape as [`crate::machine::MachineHubWatcher`]:
/// [`spawn`] starts the loop and hands back the receiver, [`stop`] (and `Drop`)
/// aborts it, and it is not restartable. The backoff is the same shared
/// schedule with the same reset-on-worked rule, so a roost row and a hub row in
/// one sessions view go stale at the same rate.
///
/// [`spawn`]: RoostWatcher::spawn
/// [`stop`]: RoostWatcher::stop
pub struct RoostWatcher {
    label: String,
    task: tokio::task::JoinHandle<()>,
}

impl RoostWatcher {
    /// Spawn the connect-identify-poll-retry loop for `reach` onto `handle`.
    ///
    /// `label` is stamped on every row of every inventory this watcher emits
    /// (`RoostSession::host_label`), which is what a client turns into
    /// `origin: "machine:<label>"`. It is NOT `reach.label()` — a reach's label
    /// answers "which reach failed", and a client may well want to poll one
    /// reach under a name of its own.
    pub fn spawn(
        handle: &tokio::runtime::Handle,
        reach: Arc<dyn RoostReach>,
        label: String,
    ) -> (RoostWatcher, mpsc::UnboundedReceiver<RoostUpdate>) {
        Self::spawn_inner(
            handle,
            reach,
            label,
            poll_interval(),
            BackoffSleeper::default(),
        )
    }

    /// [`spawn`](Self::spawn) with the poll cadence and the backoff-sleep seam
    /// supplied — the real clock and the resolved interval everywhere but this
    /// module's own tests.
    fn spawn_inner(
        handle: &tokio::runtime::Handle,
        reach: Arc<dyn RoostReach>,
        label: String,
        poll: Duration,
        sleeper: BackoffSleeper,
    ) -> (RoostWatcher, mpsc::UnboundedReceiver<RoostUpdate>) {
        let (tx, rx) = mpsc::unbounded_channel();
        let task = handle.spawn(run_loop(reach, tx, label.clone(), poll, sleeper));
        (RoostWatcher { label, task }, rx)
    }

    /// The label this watcher stamps on its rows.
    pub fn label(&self) -> &str {
        &self.label
    }

    /// Abort the loop. Dropping the in-flight future closes the held
    /// connection; the reach is torn down when the last reference to it goes.
    pub fn stop(&self) {
        self.task.abort();
    }
}

impl Drop for RoostWatcher {
    fn drop(&mut self) {
        self.stop();
    }
}

/// **Where the loop's backoff sleep goes — a `cfg(test)` seam that is an EMPTY
/// struct in a normal build**, its `sleep` a plain `tokio::time::sleep`.
///
/// Lifted verbatim from [`crate::machine`]'s, and for the same reason: the reset
/// rule is only observable as *when* the next attempt happens, and the schedule
/// (500 ms → 30 s) is far too long to assert against a real clock.
#[derive(Default)]
struct BackoffSleeper {
    #[cfg(test)]
    scripted: Option<Arc<tests::ScriptedSleeper>>,
}

impl BackoffSleeper {
    async fn sleep(&self, wait: Duration) {
        #[cfg(test)]
        if let Some(scripted) = &self.scripted {
            return scripted.sleep(wait).await;
        }
        tokio::time::sleep(wait).await;
    }
}

async fn run_loop(
    reach: Arc<dyn RoostReach>,
    tx: mpsc::UnboundedSender<RoostUpdate>,
    label: String,
    poll: Duration,
    sleeper: BackoffSleeper,
) {
    let mut backoff = backoff::INITIAL;
    loop {
        if tx.is_closed() {
            break;
        }
        // **The reset is keyed on the connection having WORKED, not on how it
        // later ended** — the same rule `machine.rs` documents. Almost every
        // real disconnect is an `Err` (the session restarted, the ssh master
        // died, the phone changed networks), so resetting only on a clean end
        // would ratchet a healthy feed up to the 30 s ceiling and keep it there.
        let mut worked = false;
        let outcome = poll_once(&reach, &tx, &label, poll, &mut worked).await;
        if worked {
            backoff = backoff::INITIAL;
        }
        // `Ok` means only one thing here: the consumer went away. There is no
        // clean end to a poll loop.
        let Err(reason) = outcome else { break };
        // After ANY error, unconditionally — a bridge socket that accepts while
        // its `ssh` is gone would pass any liveness probe this could run
        // instead.
        reach.invalidate().await;
        if tx.send(RoostUpdate::Down { reason }).is_err() {
            break;
        }
        let (wait, next) = backoff::step(backoff);
        backoff = next;
        // Race the sleep against the consumer going away: a session that stays
        // down delivers nothing, so a send failure alone would never be observed
        // here and an abandoned receiver would leak the task.
        tokio::select! {
            () = sleeper.sleep(wait) => {}
            _ = tx.closed() => break,
        }
    }
}

/// One connection, held for as many polls as it survives.
///
/// `worked` is set once a snapshot has actually gone out — the caller's signal
/// that this attempt was good, whatever happens to it afterwards.
///
/// Returns `Ok(())` only when the consumer has gone; every other exit is an
/// `Err` carrying the reason to publish.
async fn poll_once(
    reach: &Arc<dyn RoostReach>,
    tx: &mpsc::UnboundedSender<RoostUpdate>,
    label: &str,
    poll: Duration,
    worked: &mut bool,
) -> Result<(), String> {
    let endpoint = reach.ensure().await.map_err(|e| e.to_string())?;
    let mut conn = Conn::endpoint(&endpoint).await.map_err(|e| e.to_string())?;

    // **Per connection, and compared with `!=` rather than `>`.** roost's
    // revision is an in-process counter that resets to 1 when the daemon
    // restarts, so a number going DOWN is a real change and a `>` test would
    // silently stop emitting for the rest of the session's life.
    let mut last: Option<(String, Option<u64>)> = None;
    loop {
        // **The gate, on every poll and not once per connection.** Two reasons,
        // and it costs one small request on a connection that is already open:
        //
        // * `session_id` identifies the daemon INSTANCE, and a restart does not
        //   have to drop this socket for us to be talking to a new one. Taking
        //   the id once and re-stamping it forever means a restart whose
        //   revision happens to land on the number we last emitted is invisible
        //   — no snapshot, and every row afterwards carries the dead instance's
        //   id, which is the field a client keys "is this still the same
        //   daemon" off.
        // * It re-applies the compatibility gate. `NotASession` and
        //   `ProtocolMismatch` are `Down` reasons that name themselves, and a
        //   session that was replaced by something else on the same socket has
        //   to be caught the same way a fresh connection would catch it.
        let identify = conn.session_identify().await.map_err(|e| e.to_string())?;
        let list = conn.tab_list().await.map_err(|e| e.to_string())?;
        let seen = (identify.session_id.clone(), list.revision);
        // A list with no revision publishes no fence, so there is nothing to
        // compare and every poll is news. (The gate above means this should not
        // happen — only a UI socket omits it — but a silent stall would be the
        // worst possible way to find out otherwise.)
        if list.revision.is_none() || last.as_ref() != Some(&seen) {
            last = Some(seen);
            let inventory = RoostInventory::from_list(label, &list, &identify);
            *worked = true;
            if tx.send(RoostUpdate::Snapshot(inventory)).is_err() {
                return Ok(());
            }
        }
        tokio::select! {
            () = tokio::time::sleep(poll) => {}
            _ = tx.closed() => return Ok(()),
        }
    }
}

// ---------------------------------------------------------------------------
// one-shots
// ---------------------------------------------------------------------------

/// Dial through a reach, invalidating it if the dial fails.
async fn dial(reach: &dyn RoostReach) -> Result<Conn, String> {
    let endpoint = match reach.ensure().await {
        Ok(endpoint) => endpoint,
        Err(e) => {
            reach.invalidate().await;
            return Err(e.to_string());
        }
    };
    finish(reach, Conn::endpoint(&endpoint).await).await
}

/// A request's outcome, with the reach invalidated when it failed — after ANY
/// error, unconditionally, for the reason in the module doc.
async fn finish<T>(reach: &dyn RoostReach, result: Result<T, RoostError>) -> Result<T, String> {
    match result {
        Ok(value) => Ok(value),
        Err(e) => {
            reach.invalidate().await;
            Err(e.to_string())
        }
    }
}

/// Whether a failed request means **the wire** is suspect, rather than the
/// request.
///
/// A one-shot does not need this — it dials per call, so invalidating after any
/// error costs it nothing. A held connection does: it is the only caller that
/// can be told "that tab is gone" by a transport that is working perfectly, and
/// tearing the transport down for that would make every stale tab id cost an
/// `ssh` re-establish.
///
/// Matched exhaustively on purpose: a new [`RoostError`] variant should fail to
/// compile here rather than default into either answer.
fn is_transport_error(error: &RoostError) -> bool {
    match error {
        // Nothing is there, the frame did not decode, or the stream lost a
        // revision. All three say the connection cannot be trusted for the next
        // request, whatever the reach thinks it is holding.
        RoostError::Unavailable(_) | RoostError::Wire(_) | RoostError::RevisionGap { .. } => true,
        // A refusal the session MINTED is proof the wire works end to end: it
        // was read, dispatched, and answered. `not-found` on a closed tab is the
        // common one, and it is not a reason to rebuild anything.
        RoostError::Server { .. } => false,
        // The gate's two. They describe the peer, not the pipe — a reconnect
        // reaches the same wrong thing.
        RoostError::ProtocolMismatch { .. } | RoostError::NotASession => false,
    }
}

/// `tab.open` — start an agent in a new tab and get the tab back.
///
/// One connection per call, deliberately: over SSH each is a fresh remote exec,
/// which is the honest cost of a one-shot and is why the *watcher* holds one
/// connection instead.
pub async fn tab_open(reach: &dyn RoostReach, params: TabOpenParams) -> Result<Tab, String> {
    let mut conn = dial(reach).await?;
    finish(reach, conn.tab_open(params).await).await
}

/// `tab.close` — end a tab. It leaves `tab.list` entirely.
pub async fn tab_close(reach: &dyn RoostReach, tab_id: i64) -> Result<(), String> {
    let mut conn = dial(reach).await?;
    finish(reach, conn.tab_close(tab_id).await).await
}

/// `tab.dump` — one tab's viewport as text. For a repeated peek use
/// [`RoostPeek`], which holds the connection.
pub async fn tab_dump(reach: &dyn RoostReach, tab_id: i64) -> Result<TabDumpResult, String> {
    let mut conn = dial(reach).await?;
    finish(reach, conn.tab_dump(tab_id).await).await
}

/// `tab.write` — raw bytes into a tab's PTY. Byte-exact; this is how a prompt
/// gets typed at an agent.
pub async fn tab_write(reach: &dyn RoostReach, tab_id: i64, data: &[u8]) -> Result<(), String> {
    let mut conn = dial(reach).await?;
    finish(reach, conn.tab_write(tab_id, data).await).await
}

/// A held connection for repeatedly dumping one tab.
///
/// The read-only terminal affordance until roost R3 lands attach. It holds ONE
/// connection for the life of the peek because the alternative — a dial per
/// frame — is a remote `ssh` exec per frame on a machine.
///
/// **Dropping it closes the connection**, which is the whole of closing a peek:
/// there is no server-side state to release (`tab.dump` is lease-free and
/// stateless), so there is nothing an explicit `close()` could do that the drop
/// does not.
///
/// ## It keeps the reach, not just the connection
///
/// A peek is the one thing in this module that survives across requests, so it
/// is the one thing that has to invalidate the reach itself. Every other caller
/// routes through [`finish`], which invalidates after any error; a peek that
/// held only its `Conn` would leave the reach believing its transport was fine.
/// Over an [`SshBridge`] that is a concrete bug: the `ssh` master dies, `dump`
/// returns [`RoostError::Unavailable`], the caller re-opens the peek, and
/// [`RoostReach::ensure`] hands back the same dead `bridge.sock` — which accepts
/// connections happily and answers nothing — forever.
///
/// It invalidates on a **transport-shaped** error only ([`is_transport_error`]):
/// a `not-found` for a tab somebody closed is the session working, not the wire
/// failing.
pub struct RoostPeek {
    /// Held so a failed [`dump`](Self::dump) can mark the transport suspect —
    /// the whole reason this is an `Arc<dyn RoostReach>` and not a `Conn` alone.
    reach: Arc<dyn RoostReach>,
    conn: Conn,
    tab_id: i64,
}

impl RoostPeek {
    /// Dial and hold. The tab is not validated here — the first
    /// [`dump`](Self::dump) is what says whether it exists.
    ///
    /// **Takes the reach by `Arc`** because the peek outlives the call: it keeps
    /// the reach for as long as it keeps the connection, to invalidate it when a
    /// frame fails on the wire. A caller holding a bare reach wraps it —
    /// `RoostPeek::open(Arc::new(FixedPort(port)), tab_id)`.
    pub async fn open(reach: Arc<dyn RoostReach>, tab_id: i64) -> Result<RoostPeek, String> {
        let conn = dial(reach.as_ref()).await?;
        Ok(RoostPeek {
            reach,
            conn,
            tab_id,
        })
    }

    /// The tab being peeked at.
    pub fn tab_id(&self) -> i64 {
        self.tab_id
    }

    /// One frame.
    ///
    /// Returns the typed [`RoostError`] rather than a string: a peek loop wants
    /// to tell "that tab is gone" (`Server { code: "not-found" }` — stop) from
    /// "the wire died" (`Unavailable` — re-open), and a rendered message makes
    /// that a spelling comparison.
    ///
    /// A wire failure also **invalidates the reach** on the way out, so the
    /// re-open the caller is about to do rebuilds the transport instead of
    /// being handed the dead one back.
    pub async fn dump(&mut self) -> Result<TabDumpResult, RoostError> {
        match self.conn.tab_dump(self.tab_id).await {
            Ok(dump) => Ok(dump),
            Err(e) => {
                if is_transport_error(&e) {
                    self.reach.invalidate().await;
                }
                Err(e)
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    use std::sync::atomic::AtomicUsize;

    use shed_core::rc::RcActivity;
    use shed_core::roost::testing::{ownership, FakeRoost};

    // ---- doubles ----

    /// The backoff-sleep seam's test half: record the wait the loop is ABOUT to
    /// take and return immediately rather than spend it. After `park_after`
    /// waits it pends forever, parking the loop at a known point instead of
    /// letting it spin while the test finishes its assertions.
    pub(super) struct ScriptedSleeper {
        waits: mpsc::UnboundedSender<Duration>,
        taken: AtomicUsize,
        park_after: usize,
    }

    impl ScriptedSleeper {
        fn new(park_after: usize) -> (Arc<ScriptedSleeper>, mpsc::UnboundedReceiver<Duration>) {
            let (waits, rx) = mpsc::unbounded_channel();
            (
                Arc::new(ScriptedSleeper {
                    waits,
                    taken: AtomicUsize::new(0),
                    park_after,
                }),
                rx,
            )
        }

        pub(super) async fn sleep(&self, wait: Duration) {
            let _ = self.waits.send(wait);
            if self.taken.fetch_add(1, Ordering::SeqCst) + 1 >= self.park_after {
                std::future::pending::<()>().await;
            }
        }
    }

    /// A reach that refuses its first `failures_left` `ensure`s and then hands
    /// over a working endpoint — "the machine is asleep, then wakes up". The
    /// refusals cost no I/O, so a whole failing ladder runs in the test's own
    /// time.
    struct FlakyReach {
        endpoint: RoostEndpoint,
        failures_left: AtomicUsize,
        /// Every `ensure`, refused or not — i.e. how many connection attempts
        /// the loop has made. What "the loop is still running" is read off.
        ensures: AtomicUsize,
        invalidations: AtomicUsize,
    }

    impl FlakyReach {
        fn new(endpoint: RoostEndpoint, failures: usize) -> Arc<FlakyReach> {
            Arc::new(FlakyReach {
                endpoint,
                failures_left: AtomicUsize::new(failures),
                ensures: AtomicUsize::new(0),
                invalidations: AtomicUsize::new(0),
            })
        }
    }

    #[async_trait::async_trait]
    impl RoostReach for FlakyReach {
        fn label(&self) -> &str {
            "flaky"
        }

        async fn ensure(&self) -> Result<RoostEndpoint, ForwardError> {
            self.ensures.fetch_add(1, Ordering::SeqCst);
            if self.failures_left.load(Ordering::SeqCst) > 0 {
                self.failures_left.fetch_sub(1, Ordering::SeqCst);
                return Err(ForwardError("the machine is asleep".to_string()));
            }
            Ok(self.endpoint.clone())
        }

        async fn invalidate(&self) {
            self.invalidations.fetch_add(1, Ordering::SeqCst);
        }
    }

    // ---- helpers ----

    /// A watcher on a fake, polling fast enough that a test never waits on the
    /// real 2 s cadence.
    fn watch(reach: Arc<dyn RoostReach>) -> (RoostWatcher, mpsc::UnboundedReceiver<RoostUpdate>) {
        RoostWatcher::spawn_inner(
            &tokio::runtime::Handle::current(),
            reach,
            "roost-host".to_string(),
            Duration::from_millis(5),
            BackoffSleeper::default(),
        )
    }

    async fn next_update(rx: &mut mpsc::UnboundedReceiver<RoostUpdate>) -> RoostUpdate {
        tokio::time::timeout(Duration::from_secs(5), rx.recv())
            .await
            .expect("an update should arrive")
            .expect("the channel stays open")
    }

    async fn next_snapshot(rx: &mut mpsc::UnboundedReceiver<RoostUpdate>) -> RoostInventory {
        match next_update(rx).await {
            RoostUpdate::Snapshot(inventory) => inventory,
            other => panic!("expected a snapshot, got {other:?}"),
        }
    }

    /// The next snapshot, past however many `Down`s a failing ladder emitted
    /// first. Only for a test whose subject is the ladder itself — everywhere
    /// else an unexpected `Down` is the bug and [`next_snapshot`] should say so.
    async fn snapshot_past_downs(rx: &mut mpsc::UnboundedReceiver<RoostUpdate>) -> RoostInventory {
        loop {
            if let RoostUpdate::Snapshot(inventory) = next_update(rx).await {
                return inventory;
            }
        }
    }

    async fn next_down(rx: &mut mpsc::UnboundedReceiver<RoostUpdate>) -> String {
        match next_update(rx).await {
            RoostUpdate::Down { reason } => reason,
            other => panic!("expected Down, got {other:?}"),
        }
    }

    /// The next snapshot carrying `session_id`.
    ///
    /// A restart lands between the two requests one poll makes, so the first
    /// snapshot after it can legitimately still carry the previous instance's id
    /// alongside the new revision. Skipping to the id under test removes that
    /// race without weakening anything: under the bug these tests exist for, the
    /// id never arrives at all and [`next_update`]'s timeout fails the test.
    async fn snapshot_from_daemon(
        rx: &mut mpsc::UnboundedReceiver<RoostUpdate>,
        session_id: &str,
    ) -> RoostInventory {
        loop {
            let inventory = next_snapshot(rx).await;
            if inventory.daemon_session_id == session_id {
                return inventory;
            }
        }
    }

    fn entry(name: &str) -> MachineEntry {
        MachineEntry {
            name: name.to_string(),
            host: name.to_string(),
            user: None,
            ssh_port: 22,
            rc_bin: None,
            known_hosts: None,
        }
    }

    /// The tab the vendored `tab.list` vector carries that this suite drives.
    const TAB: i64 = 5;

    fn owned(detail: &str) -> serde_json::Value {
        ownership("opencode", "ses_test", detail, 1_700_000_060)
    }

    // ---- the reaches ----

    #[tokio::test]
    async fn a_local_session_is_the_socket_when_it_exists_and_names_it_when_it_does_not() {
        let fake = FakeRoost::start().await;
        let present = LocalSession::new("localhost", fake.socket_path());
        assert_eq!(
            present.ensure().await.expect("the socket is there"),
            RoostEndpoint::Unix(fake.socket_path().to_path_buf())
        );

        let missing_path = fake.socket_path().with_file_name("nothing.sock");
        let missing = LocalSession::new("localhost", &missing_path);
        let err = missing.ensure().await.expect_err("nothing is bound there");
        assert!(
            err.to_string()
                .contains(&missing_path.display().to_string()),
            "the reason must name the path that was tried: {err}"
        );
        // Never spawns: a failed `ensure` leaves nothing behind.
        assert!(!missing_path.exists());
    }

    /// The resolver's whole candidate ladder reaches the error message — "no
    /// roost-session" with no path in it is the least actionable thing this
    /// could say.
    #[test]
    fn the_default_local_reach_reports_every_path_it_looked_at() {
        let resolved = local_session_socket();
        let reach = LocalSession::default_local();
        assert_eq!(reach.label(), "localhost");
        assert_eq!(reach.socket(), resolved.path.as_path());
        assert_eq!(reach.tried, resolved.tried);
        assert!(reach.tried.len() >= 2, "release and the -dev sibling");
    }

    #[tokio::test]
    async fn a_labelled_port_is_the_port_and_a_fixed_port_reaches_roost_too() {
        let reach = LabelledPort::new("mini3", 41234);
        assert_eq!(reach.label(), "mini3");
        assert_eq!(
            reach.ensure().await.expect("a handed-over port is ready"),
            RoostEndpoint::TcpLoopback(41234)
        );
        reach.invalidate().await;
        assert_eq!(
            reach.ensure().await.expect("still ready"),
            RoostEndpoint::TcpLoopback(41234),
            "invalidating a port somebody else owns must not move it"
        );

        let pinned = FixedPort(41234);
        assert_eq!(
            RoostReach::ensure(&pinned).await.expect("ready"),
            RoostEndpoint::TcpLoopback(41234)
        );
    }

    #[tokio::test]
    async fn an_unreachable_reach_says_why() {
        let reach = UnreachableReach::new("mini9", "no roost transport is mapped for mini9");
        assert_eq!(reach.label(), "mini9");
        let err = reach.ensure().await.expect_err("it is unreachable by name");
        assert!(err.to_string().contains("mini9"), "{err}");
    }

    // ---- the watcher ----

    /// Both transports, because the TCP one goes through a socketpair and a copy
    /// pump that a Unix-only test would never touch.
    #[tokio::test]
    async fn the_first_list_of_a_connection_is_always_a_snapshot_over_both_transports() {
        let fake = FakeRoost::start().await;

        let (unix_watcher, mut unix_rx) =
            watch(Arc::new(LocalSession::new("localhost", fake.socket_path())));
        let over_unix = next_snapshot(&mut unix_rx).await;
        unix_watcher.stop();

        let (tcp_watcher, mut tcp_rx) =
            watch(Arc::new(LabelledPort::new("mini3", fake.tcp_port())));
        let over_tcp = next_snapshot(&mut tcp_rx).await;
        tcp_watcher.stop();

        assert_eq!(over_unix.host_label, "roost-host");
        assert_eq!(over_unix.revision, Some(fake.revision()));
        assert_eq!(over_unix.daemon_session_id, fake.session_id());
        assert_eq!(
            over_unix.sessions, over_tcp.sessions,
            "the transport must not change what the inventory says"
        );
    }

    /// A committed change emits, and the emitted row carries it.
    #[tokio::test]
    async fn an_axis_change_emits_a_snapshot_carrying_the_changed_row() {
        let fake = FakeRoost::start().await;
        fake.set_tab_axes(TAB, "working", Some(owned("session_status")), false);

        let (watcher, mut rx) = watch(Arc::new(LocalSession::new("localhost", fake.socket_path())));
        let first = next_snapshot(&mut rx).await;
        let before = first
            .sessions
            .iter()
            .find(|s| s.tab_id == TAB)
            .expect("the owned tab is a row");
        assert_eq!(before.activity(), Some(RcActivity::Working));
        assert!(!before.attention);

        fake.set_tab_axes(TAB, "waiting", Some(owned("permission_asked")), true);
        let second = next_snapshot(&mut rx).await;
        let after = second
            .sessions
            .iter()
            .find(|s| s.tab_id == TAB)
            .expect("still a row");
        assert_eq!(after.activity(), Some(RcActivity::NeedsApproval));
        assert!(after.attention, "roost's sticky notification bit");
        assert!(second.revision > first.revision);
        watcher.stop();
    }

    /// **The negative control is the second half.** An "it stayed silent"
    /// assertion passes just as well against a watcher that died, a receiver
    /// nobody feeds, or a poll interval of a week — so the same test then
    /// commits a revision and requires the snapshot to arrive. Only the pair
    /// says the silence was a decision.
    #[tokio::test]
    async fn an_unchanged_revision_is_silent_and_the_control_that_bumps_it_is_not() {
        let fake = FakeRoost::start().await;
        let (watcher, mut rx) = watch(Arc::new(LocalSession::new("localhost", fake.socket_path())));
        let first = next_snapshot(&mut rx).await;

        // Five poll intervals' worth of quiet.
        tokio::time::sleep(Duration::from_millis(25)).await;
        assert!(
            rx.try_recv().is_err(),
            "an unchanged revision must not re-emit"
        );

        // The control: the ONLY thing that changed is the revision.
        fake.bump_revision();
        let second = next_snapshot(&mut rx).await;
        assert_eq!(second.revision, Some(first.revision.expect("a fence") + 1));
        assert_eq!(
            second.sessions, first.sessions,
            "an empty commit still emits — the rows are simply the same"
        );
        watcher.stop();
    }

    /// The sticky notification bit clearing is a change like any other. It is
    /// worth its own lane because it is the one axis that moves *backwards*
    /// under normal use (roost clears it on UI focus).
    #[tokio::test]
    async fn clearing_has_notification_emits() {
        let fake = FakeRoost::start().await;
        fake.set_tab_axes(TAB, "waiting", Some(owned("question_asked")), true);

        let (watcher, mut rx) = watch(Arc::new(LocalSession::new("localhost", fake.socket_path())));
        let first = next_snapshot(&mut rx).await;
        assert!(first
            .sessions
            .iter()
            .any(|s| s.tab_id == TAB && s.attention));

        fake.set_tab_axes(TAB, "waiting", Some(owned("question_asked")), false);
        let second = next_snapshot(&mut rx).await;
        assert!(second
            .sessions
            .iter()
            .any(|s| s.tab_id == TAB && !s.attention));
        watcher.stop();
    }

    /// **A restart makes the revision go DOWN.** roost persists tab ids and
    /// keeps `revision` in process, so a restarted daemon reports 1 after having
    /// reported 42. A `>` comparison would stop emitting for the rest of that
    /// connection's life; `!=` is why this passes.
    #[tokio::test]
    async fn a_daemon_restart_emits_even_though_the_revision_went_backwards() {
        let fake = FakeRoost::start().await;
        let (watcher, mut rx) = watch(Arc::new(LocalSession::new("localhost", fake.socket_path())));
        let before = next_snapshot(&mut rx).await;
        assert!(before.revision > Some(1), "the vector starts well above 1");

        fake.restart();
        // **The new instance's id, not the old one's.** `session.identify` is
        // re-run on every poll precisely so this is fresh: a snapshot stamped
        // with a dead daemon's id tells a client the daemon it is looking at is
        // one it is not. Under the old once-per-connection identify this id
        // never arrives and the wait below times the test out.
        let restarted = fake.session_id();
        let after = snapshot_from_daemon(&mut rx, &restarted).await;
        assert_eq!(after.revision, Some(1));
        assert!(
            after.revision < before.revision,
            "the whole point: the number went down and it still emitted"
        );
        assert_ne!(
            after.daemon_session_id, before.daemon_session_id,
            "a restarted daemon is a new instance and the rows must say so"
        );
        // Tab ids persist across a restart, which is why rows key off them.
        assert_eq!(
            after.sessions.iter().map(|s| s.tab_id).collect::<Vec<_>>(),
            before.sessions.iter().map(|s| s.tab_id).collect::<Vec<_>>()
        );
        watcher.stop();
    }

    /// **The restart the revision cannot see.** roost's counter resets to 1, so
    /// a daemon that restarts while the last emitted revision is *already* 1
    /// moves nothing at all on the `tab.list` wire — and the socket does not
    /// have to drop for that to happen (a session handed off, a fake's
    /// `restart()`, any real restart racing the poll). If `session.identify`
    /// were taken once per connection, this snapshot would never be sent and
    /// every row afterwards would carry a dead instance's id.
    #[tokio::test]
    async fn a_restart_that_does_not_move_the_revision_still_emits() {
        let fake = FakeRoost::start().await;
        let (watcher, mut rx) = watch(Arc::new(LocalSession::new("localhost", fake.socket_path())));
        next_snapshot(&mut rx).await;

        // Get the last emitted revision down to 1, so the restart under test has
        // no revision movement left to be noticed by.
        fake.restart();
        let first_restart = fake.session_id();
        let at_one = snapshot_from_daemon(&mut rx, &first_restart).await;
        assert_eq!(at_one.revision, Some(1));

        // The ONLY thing that changes now is the daemon instance: same revision,
        // same tabs, same socket, new `session_id`.
        fake.restart();
        let second_restart = fake.session_id();
        let after = snapshot_from_daemon(&mut rx, &second_restart).await;
        assert_eq!(
            after.revision,
            Some(1),
            "the revision is the same number it already was"
        );
        assert_ne!(
            after.daemon_session_id, at_one.daemon_session_id,
            "the session id is the only signal there was, and it is what fired"
        );
        assert_eq!(
            after.sessions, at_one.sessions,
            "the rows are unchanged — the news is which daemon they came from"
        );
        watcher.stop();
    }

    /// A dropped connection is `Down`, then a reconnect with a fresh snapshot —
    /// the reach is invalidated on the way through so the next `ensure` rebuilds.
    #[tokio::test]
    async fn a_hangup_is_a_down_then_a_reconnect_with_a_fresh_snapshot() {
        let fake = FakeRoost::start().await;
        let reach = FlakyReach::new(RoostEndpoint::Unix(fake.socket_path().to_path_buf()), 0);
        let (watcher, mut rx) = watch(reach.clone());
        next_snapshot(&mut rx).await;

        fake.close_all();
        let reason = next_down(&mut rx).await;
        assert!(!reason.is_empty(), "a Down always says why");
        assert!(
            reach.invalidations.load(Ordering::SeqCst) >= 1,
            "any error invalidates the reach"
        );

        let again = next_snapshot(&mut rx).await;
        assert_eq!(again.daemon_session_id, fake.session_id());
        watcher.stop();
    }

    /// Mirrors `machine.rs`'s `a_connection_that_worked_resets_the_delay…`: the
    /// reset is keyed on the connection having WORKED, not on how it ended.
    #[tokio::test]
    async fn a_connection_that_worked_resets_the_delay_however_it_later_ended() {
        let fake = FakeRoost::start().await;
        let (sleeper, mut waits) = ScriptedSleeper::new(3);
        // Two dead attempts, then a live one whose feed is killed under it.
        let reach = FlakyReach::new(RoostEndpoint::Unix(fake.socket_path().to_path_buf()), 2);
        let (watcher, mut rx) = RoostWatcher::spawn_inner(
            &tokio::runtime::Handle::current(),
            reach,
            "roost-host".to_string(),
            Duration::from_millis(5),
            BackoffSleeper {
                scripted: Some(sleeper),
            },
        );

        async fn next_wait(waits: &mut mpsc::UnboundedReceiver<Duration>) -> Duration {
            tokio::time::timeout(Duration::from_secs(5), waits.recv())
                .await
                .expect("the loop should reach its backoff")
                .expect("the sleeper outlives the loop")
        }

        assert_eq!(
            next_wait(&mut waits).await,
            backoff::INITIAL,
            "a first dead attempt waits the initial delay"
        );
        assert_eq!(
            next_wait(&mut waits).await,
            backoff::INITIAL * 2,
            "a second dead attempt ratchets"
        );

        // The third attempt reaches the fake and emits; then the fake hangs up.
        // (The two dead attempts' `Down`s are queued ahead of it — each one is
        // sent before the wait that was just read.)
        snapshot_past_downs(&mut rx).await;
        fake.close_all();
        assert_eq!(
            next_wait(&mut waits).await,
            backoff::INITIAL,
            "the third attempt connected and sent its snapshot, so the schedule \
             must start over — resetting only on a clean end would leave a \
             healthy feed reconnecting at the ceiling"
        );
        watcher.stop();
    }

    /// A roost **UI** socket is never read as machine inventory, and the reason
    /// says which of the two it found.
    #[tokio::test]
    async fn a_ui_socket_is_a_down_that_names_itself() {
        let fake = FakeRoost::start().await;
        fake.serve_as_ui_socket(true);
        let (watcher, mut rx) = watch(Arc::new(LocalSession::new("localhost", fake.socket_path())));
        let reason = next_down(&mut rx).await;
        assert!(
            reason.contains("UI socket"),
            "the reason must name what it found: {reason}"
        );
        watcher.stop();
    }

    #[tokio::test]
    async fn a_protocol_mismatch_is_a_down_naming_both_numbers() {
        let fake = FakeRoost::start().await;
        let theirs = roost_ipc::messages::SESSION_PROTOCOL_VERSION + 7;
        fake.set_session_protocol(theirs);
        let (watcher, mut rx) = watch(Arc::new(LocalSession::new("localhost", fake.socket_path())));
        let reason = next_down(&mut rx).await;
        assert!(reason.contains(&theirs.to_string()), "{reason}");
        assert!(
            reason.contains(&roost_ipc::messages::SESSION_PROTOCOL_VERSION.to_string()),
            "both numbers, or the message cannot say which side to upgrade: {reason}"
        );
        watcher.stop();
    }

    #[tokio::test]
    async fn stopping_a_watcher_ends_its_loop() {
        let fake = FakeRoost::start().await;
        let (watcher, mut rx) = watch(Arc::new(LocalSession::new("localhost", fake.socket_path())));
        next_snapshot(&mut rx).await;
        assert_eq!(watcher.label(), "roost-host");

        watcher.stop();
        fake.bump_revision();
        tokio::time::sleep(Duration::from_millis(50)).await;
        assert!(
            rx.try_recv().is_err(),
            "a stopped watcher must publish nothing, even against a live session"
        );
    }

    /// An abandoned receiver must end the task, not leak it — the watcher itself
    /// is deliberately kept alive here, so only the dropped receiver can stop
    /// the loop.
    ///
    /// Read off `ensure` calls rather than off the channel that was just
    /// dropped: one attempt is all a stopped loop ever makes, and a loop that
    /// kept reconnecting against a session that keeps hanging up would climb.
    #[tokio::test]
    async fn dropping_the_receiver_ends_the_loop() {
        let fake = FakeRoost::start().await;
        let reach = FlakyReach::new(RoostEndpoint::Unix(fake.socket_path().to_path_buf()), 0);
        let (_watcher, mut rx) = watch(reach.clone());
        next_snapshot(&mut rx).await;
        assert_eq!(reach.ensures.load(Ordering::SeqCst), 1);
        drop(rx);

        // Keep giving it something to reconnect to, then something to fail on.
        for _ in 0..5 {
            fake.close_all();
            tokio::time::sleep(Duration::from_millis(20)).await;
        }
        assert_eq!(
            reach.ensures.load(Ordering::SeqCst),
            1,
            "a loop with nobody listening must not keep reconnecting"
        );
    }

    // ---- one-shots ----

    #[tokio::test]
    async fn the_one_shots_drive_a_tab_end_to_end() {
        let fake = FakeRoost::start().await;
        // An `Arc` because the peek below keeps it: a peek outlives the call
        // that opened it and has to be able to invalidate the reach itself.
        let reach: Arc<dyn RoostReach> =
            Arc::new(LocalSession::new("localhost", fake.socket_path()));

        let tab = tab_open(
            reach.as_ref(),
            TabOpenParams {
                cwd: "/home/shed/app".to_string(),
                argv: launch_argv(&shed_core::rc::RcKind::Opencode)
                    .expect("opencode is launchable"),
                title: "opencode".to_string(),
                ..Default::default()
            },
        )
        .await
        .expect("tab.open");
        assert_eq!(tab.cwd, "/home/shed/app");

        tab_write(reach.as_ref(), tab.id, b"hello\r")
            .await
            .expect("write");
        assert_eq!(fake.written(tab.id), b"hello\r");

        let dump = tab_dump(reach.as_ref(), tab.id).await.expect("dump");
        assert!(dump.rows_text.iter().any(|line| line.contains("opencode")));

        // A peek holds ONE connection across frames.
        let mut peek = RoostPeek::open(Arc::clone(&reach), tab.id)
            .await
            .expect("peek");
        assert_eq!(peek.tab_id(), tab.id);
        let first = peek.dump().await.expect("first frame");
        let second = peek.dump().await.expect("second frame on the same conn");
        assert_eq!(first.rows_text, second.rows_text);
        drop(peek);

        tab_close(reach.as_ref(), tab.id).await.expect("close");
        let gone = tab_dump(reach.as_ref(), tab.id)
            .await
            .expect_err("a closed tab leaves the list");
        assert!(gone.contains("not-found"), "{gone}");
    }

    /// A failing one-shot invalidates the reach so the next call rebuilds it.
    #[tokio::test]
    async fn a_failed_one_shot_invalidates_the_reach() {
        let fake = FakeRoost::start().await;
        let reach = FlakyReach::new(RoostEndpoint::Unix(fake.socket_path().to_path_buf()), 0);
        tab_dump(reach.as_ref(), 999_999)
            .await
            .expect_err("no such tab");
        assert_eq!(reach.invalidations.load(Ordering::SeqCst), 1);
    }

    /// **A peek is the one holder of a connection, so it is the one thing that
    /// must invalidate the reach itself.** Everything else here dials per call
    /// and routes its errors through `finish`. A peek that only held its `Conn`
    /// would, over an [`SshBridge`] whose `ssh` master has died, get
    /// `Unavailable`, be re-opened by its caller, and be handed the same dead
    /// `bridge.sock` back — which accepts and then answers nothing, forever.
    ///
    /// The second half is the discrimination: a session refusing a tab id is the
    /// wire *working*, and tearing an ssh tunnel down for a closed tab would
    /// make every stale id cost a re-establish.
    #[tokio::test]
    async fn a_peek_invalidates_on_a_dead_wire_and_not_on_a_gone_tab() {
        let fake = FakeRoost::start().await;
        let reach = FlakyReach::new(RoostEndpoint::Unix(fake.socket_path().to_path_buf()), 0);

        // A tab that is not there: the daemon read, dispatched and refused, so
        // the transport is demonstrably fine.
        let mut missing = RoostPeek::open(reach.clone(), 999_999)
            .await
            .expect("the peek opens — the tab is only checked by a dump");
        let refused = missing.dump().await.expect_err("no such tab");
        assert_eq!(
            refused.server_code(),
            Some(roost_ipc::client::ServerCode::NotFound),
            "{refused}"
        );
        assert_eq!(
            reach.invalidations.load(Ordering::SeqCst),
            0,
            "a gone tab is not a dead transport: {refused}"
        );
        drop(missing);

        // The wire dying under a live peek is the other case.
        let mut peek = RoostPeek::open(reach.clone(), TAB).await.expect("peek");
        peek.dump().await.expect("the first frame comes back");
        fake.close_all();
        // The hang-up is the only thing this connection has pending, so it is
        // processed before the next request is even written.
        tokio::time::sleep(Duration::from_millis(100)).await;

        let dead = peek.dump().await.expect_err("the connection is gone");
        assert!(dead.is_unavailable(), "a hang-up is Unavailable: {dead}");
        assert_eq!(
            reach.invalidations.load(Ordering::SeqCst),
            1,
            "the next `ensure` has to rebuild rather than hand back the dead socket"
        );
    }

    // ---- the ssh bridge ----

    #[test]
    fn a_target_omits_the_user_and_the_default_port_and_never_says_localhost() {
        let bare = ssh_target(&entry("mini3")).expect("classified");
        assert_eq!(bare.raw, "ssh://mini3");

        let mut full = entry("mini3");
        full.user = Some("charliek".into());
        full.ssh_port = 2222;
        assert_eq!(
            ssh_target(&full).expect("classified").raw,
            "ssh://charliek@mini3:2222"
        );

        // A machine literally named `localhost` is still an ssh target: the bare
        // string is roost's sentinel for THIS machine's session, resolved through
        // its build-profile-sensitive path resolver.
        let local = ssh_target(&entry("localhost")).expect("classified");
        assert_eq!(local.raw, "ssh://localhost");

        let mut hostless = entry("mini3");
        hostless.host = String::new();
        hostless.name = String::new();
        assert!(ssh_target(&hostless).is_err(), "nothing to reach");
    }

    /// **An IPv6 literal is bracketed, and only an IPv6 literal is.**
    /// `ssh://2001:db8::1:2222` has no reading that recovers the address the
    /// user configured — roost's own authority parser takes the last colon as
    /// the port separator — so the brackets are what make a port and an IPv6
    /// host expressible at the same time.
    #[test]
    fn an_ipv6_literal_is_bracketed_and_nothing_else_is() {
        let mut v6 = entry("edge");
        v6.host = "2001:db8::1".to_string();
        v6.ssh_port = 2222;
        assert_eq!(
            ssh_target(&v6).expect("classified").raw,
            "ssh://[2001:db8::1]:2222"
        );

        // The user goes before the brackets, where an authority's user always is.
        v6.user = Some("me".into());
        assert_eq!(
            ssh_target(&v6).expect("classified").raw,
            "ssh://me@[2001:db8::1]:2222"
        );

        // On the default port there is no port to disambiguate, but the address
        // is still bracketed — one spelling, and `ssh` accepts it.
        let mut v6_default = entry("edge");
        v6_default.host = "2001:db8::1".to_string();
        assert_eq!(
            ssh_target(&v6_default).expect("classified").raw,
            "ssh://[2001:db8::1]"
        );

        // An address the user already bracketed is not double-wrapped.
        let mut prebracketed = entry("edge");
        prebracketed.host = "[2001:db8::1]".to_string();
        prebracketed.ssh_port = 2222;
        assert_eq!(
            ssh_target(&prebracketed).expect("classified").raw,
            "ssh://[2001:db8::1]:2222"
        );

        // The negative control: an alias, a hostname and an IPv4 address are
        // untouched — bracketing any of them would be a target `ssh` cannot
        // resolve.
        let mut alias = entry("work");
        alias.host = "work-box".to_string();
        alias.ssh_port = 2222;
        assert_eq!(
            ssh_target(&alias).expect("classified").raw,
            "ssh://work-box:2222"
        );
        let mut v4 = entry("edge");
        v4.host = "10.0.0.4".to_string();
        assert_eq!(ssh_target(&v4).expect("classified").raw, "ssh://10.0.0.4");
    }

    /// The `Host` line of the pin keeps the address UNBRACKETED: `ssh` matches
    /// `Host` patterns against the hostname it extracted from the target, which
    /// is what was inside the brackets.
    #[test]
    fn the_pin_matches_an_ipv6_host_the_way_ssh_spells_it() {
        let mut v6 = entry("edge");
        v6.host = "2001:db8::1".to_string();
        v6.ssh_port = 2222;
        v6.known_hosts = Some("/k".to_string());

        let bridge = SshBridge::new(&v6, SshBridgeOptions::default()).expect("bridge");
        assert_eq!(bridge.target(), "ssh://[2001:db8::1]:2222");
        let body = std::fs::read_to_string(bridge.ssh_config_path().expect("a pin")).expect("read");
        assert!(
            body.starts_with("Host 2001:db8::1\n"),
            "the pattern is the hostname, not the URL authority: {body}"
        );
    }

    #[test]
    fn a_host_id_stays_a_parseable_scratch_directory_leaf() {
        assert_eq!(host_id("mini3"), "mini3");
        assert_eq!(host_id("work-box.local"), "work-box.local");
        assert_eq!(host_id("a/b c"), "a-b-c", "no path separators, no spaces");
        assert_eq!(host_id(""), "machine");
        assert_eq!(host_id(&"x".repeat(64)).len(), 32, "the sun_path budget");
        // roost reads its own directory names back; ours must survive that.
        let name = roost_ipc::ssh::scratch_dir_name(&host_id("a/b c"));
        let (parsed, _, _) =
            roost_ipc::ssh::parse_scratch_dir_name(&name).expect("roost can read it back");
        assert_eq!(parsed, "a-b-c");
    }

    /// The pin has to win over the user's own config, which means it has to come
    /// FIRST — `ssh` takes the first value it obtains for a keyword.
    #[test]
    fn a_pinned_config_puts_the_pin_above_the_user_config_it_includes() {
        let dir = tempfile::tempdir().expect("tempdir");
        let user = dir.path().join("user_config");
        std::fs::write(&user, "Host mini3\n  HostName 10.0.0.4\n").expect("write");

        let body = pinned_ssh_config("mini3", "/home/me/.shed/known_hosts", Some(&user));
        let lines: Vec<&str> = body.lines().collect();
        assert_eq!(lines[0], "Host mini3");
        assert_eq!(
            lines[1],
            "  UserKnownHostsFile \"/home/me/.shed/known_hosts\""
        );
        assert_eq!(lines[2], "  StrictHostKeyChecking yes");
        assert_eq!(lines[3], format!("Include \"{}\"", user.display()));

        // A user config that is not there is not included at all.
        let absent = dir.path().join("nope");
        assert_eq!(
            pinned_ssh_config("mini3", "/k", Some(&absent))
                .lines()
                .count(),
            3
        );
    }

    #[test]
    fn a_tilde_in_known_hosts_expands_against_home() {
        let home = std::env::var("HOME").expect("a HOME on this host");
        let body = pinned_ssh_config("mini3", "~/.ssh/known_hosts_mini3", None);
        assert!(
            body.contains(&format!(
                "UserKnownHostsFile \"{home}/.ssh/known_hosts_mini3\""
            )),
            "{body}"
        );
        // An absolute path is left exactly as written.
        assert!(
            pinned_ssh_config("mini3", "/etc/kh", None).contains("UserKnownHostsFile \"/etc/kh\"")
        );
    }

    /// **A `known_hosts` path with a space in it must survive as one path.**
    /// `UserKnownHostsFile` takes a whitespace-separated LIST, so an unquoted
    /// `.../Application Support/...` reaches `ssh` as two files that do not
    /// exist — and with `StrictHostKeyChecking yes` directly beneath it, that is
    /// not a loose pin, it is a host that can never be connected to. The
    /// `Include` line has always been quoted for the same reason; this is the
    /// other half.
    #[test]
    fn a_known_hosts_path_with_a_space_is_quoted() {
        let spaced = "/Users/me/Library/Application Support/shed/known_hosts";
        let body = pinned_ssh_config("mini3", spaced, None);
        assert!(
            body.contains(&format!("  UserKnownHostsFile \"{spaced}\"\n")),
            "the path has to arrive as one quoted argument: {body}"
        );
        // Not "quoted somewhere" — quoted around the WHOLE path, so the line
        // holds exactly one file.
        let line = body
            .lines()
            .find(|line| line.contains("UserKnownHostsFile"))
            .expect("the pin line");
        assert_eq!(
            line.matches('"').count(),
            2,
            "one opening and one closing quote: {line}"
        );

        // A `~` path with a space expands AND stays quoted — the expansion must
        // not be what breaks the quoting.
        let home = std::env::var("HOME").expect("a HOME on this host");
        let expanded = pinned_ssh_config("mini3", "~/known hosts", None);
        assert!(
            expanded.contains(&format!("UserKnownHostsFile \"{home}/known hosts\"")),
            "{expanded}"
        );
    }

    /// A `machines:` entry with `known_hosts` writes the pin file up front (it
    /// has to exist before roost renders its own config, which existence-checks
    /// what it includes) and hands it over as the user config.
    #[test]
    fn a_known_hosts_entry_generates_the_per_machine_config() {
        let mut pinned = entry("mini3");
        pinned.known_hosts = Some("/home/me/.shed/known_hosts".to_string());
        let bridge = SshBridge::new(&pinned, SshBridgeOptions::default()).expect("bridge");

        let path = bridge.ssh_config_path().expect("a pin generates a config");
        let body = std::fs::read_to_string(path).expect("the file is written at construction");
        assert!(body.contains("Host mini3\n"), "{body}");
        assert!(
            body.contains("  UserKnownHostsFile \"/home/me/.shed/known_hosts\"\n"),
            "{body}"
        );
        assert!(body.contains("  StrictHostKeyChecking yes\n"), "{body}");
        assert_eq!(bridge.options.config_paths.user.as_deref(), Some(path));
        assert!(!bridge.options.jail_fs_root, "never from the environment");

        // No pin, no generated file — the user's own config and strictness apply.
        let plain = SshBridge::new(&entry("mini3"), SshBridgeOptions::default()).expect("bridge");
        assert!(plain.ssh_config_path().is_none());
    }

    /// The generated config directory is this bridge's, and it goes when the
    /// bridge does.
    #[test]
    fn a_bridges_config_directory_is_removed_with_it() {
        let mut pinned = entry("mini3");
        pinned.known_hosts = Some("/k".to_string());
        let bridge = SshBridge::new(&pinned, SshBridgeOptions::default()).expect("bridge");
        let dir = bridge
            .ssh_config_path()
            .and_then(Path::parent)
            .expect("a config lives in a directory")
            .to_path_buf();
        assert!(dir.exists());
        drop(bridge);
        assert!(!dir.exists(), "the scratch directory outlived the bridge");
    }

    // -- the fake ssh --

    /// A stand-in for `ssh` that pipes a connection's stdio to a Unix socket.
    ///
    /// Modelled on roost's own `tools/roosttest/fixtures/fake-ssh.sh` and cut
    /// down to the two behaviours this suite needs a real [`SshTunnel`] to see:
    ///
    /// * **`-O exit` is a recorded no-op** that removes the control socket, the
    ///   way the real master does on its way out. Teardown ordering — exit the
    ///   master while its socket is still on disk — is otherwise invisible.
    /// * **A remote command of exactly `true` is honoured literally**, because
    ///   that is what roost's warm-up (`establish_argv`) runs. Everything else
    ///   is a connection, and gets pumped.
    ///
    /// The pump is `python3` rather than `nc -U`/`socat`: `python3` is the only
    /// one of the three that can be relied on, and a half-close has to be a real
    /// `shutdown(SHUT_WR)` or the far side never sees EOF.
    fn write_fake_ssh(dir: &Path, socket: &Path, log: &Path) -> PathBuf {
        let script = format!(
            r#"#!/bin/sh
set -u
ctl=
want=0
prev=
is_exit=0
remote=
for arg in "$@"; do
    if [ "$want" -eq 1 ]; then
        ctl="$arg"
        want=0
    elif [ "$arg" = "-S" ]; then
        want=1
    fi
    if [ "$prev" = "-O" ] && [ "$arg" = "exit" ]; then is_exit=1; fi
    prev="$arg"
    remote="$arg"
done
if [ "$is_exit" -eq 1 ]; then
    printf 'exit\n' >>'{log}'
    if [ -n "$ctl" ]; then rm -f "$ctl"; fi
    exit 0
fi
if [ -n "$ctl" ] && [ ! -e "$ctl" ]; then : >"$ctl"; fi
if [ "$remote" = "true" ]; then
    printf 'establish\n' >>'{log}'
    exit 0
fi
printf 'connect\n' >>'{log}'
exec python3 -c '
import os, socket, sys, threading
s = socket.socket(socket.AF_UNIX)
s.connect(sys.argv[1])
def up():
    try:
        while True:
            chunk = os.read(0, 65536)
            if not chunk:
                break
            s.sendall(chunk)
    except Exception:
        pass
    try:
        s.shutdown(socket.SHUT_WR)
    except Exception:
        pass
threading.Thread(target=up, daemon=True).start()
try:
    while True:
        chunk = s.recv(65536)
        if not chunk:
            break
        os.write(1, chunk)
except Exception:
    pass
' '{socket}'
"#,
            log = log.display(),
            socket = socket.display(),
        );
        // Written under a staging name and renamed into place: a script created
        // while a sibling test thread is forking answers ETXTBSY to `execve`,
        // and a rename never leaves the final path open for writing.
        let staged = dir.join("ssh.staging");
        let final_path = dir.join("ssh");
        std::fs::write(&staged, script).expect("write the fake ssh");
        let mut perms = std::fs::metadata(&staged).expect("stat").permissions();
        {
            use std::os::unix::fs::PermissionsExt as _;
            perms.set_mode(0o700);
        }
        std::fs::set_permissions(&staged, perms).expect("chmod");
        std::fs::rename(&staged, &final_path).expect("rename into place");
        final_path
    }

    fn ran(log: &Path) -> Vec<String> {
        std::fs::read_to_string(log)
            .unwrap_or_default()
            .lines()
            .map(str::to_string)
            .collect()
    }

    /// A bridge to `name` that spawns `ssh` and scratches inside the test's own
    /// directory.
    ///
    /// The scratch parent is short, and ours: roost length-checks the whole
    /// `<parent>/<leaf>/bridge.sock` against `sun_path`, and sweeping a shared
    /// /tmp would meet other tests' leftovers. The empty [`SshConfigPaths`] is
    /// the same isolation for the config — a developer's own `~/.ssh/config`
    /// must not reach the fake ssh.
    fn faked_bridge(name: &str, ssh: PathBuf) -> SshBridge {
        SshBridge::new(
            &entry(name),
            SshBridgeOptions {
                ssh_bin: Some(ssh),
                // NOT the per-test tempdir: on macOS that is
                // `/var/folders/<..>/T/.tmpXXXX`, and roost refuses a scratch
                // parent that leaves no room for its `<dir>/ctl.<16 hex>` under
                // the 103-byte AF_UNIX limit (the CI Swift job runs this suite
                // there). `/tmp` is short on every platform, and roost's own
                // per-attempt `roost-ssh-<host>-<pid>-<seq>` naming keeps two
                // processes apart under it.
                scratch_parents: vec![PathBuf::from("/tmp")],
                config_paths: Some(SshConfigPaths {
                    user: None,
                    system: None,
                }),
                ..SshBridgeOptions::default()
            },
        )
        .expect("bridge")
    }

    /// The whole bridge against a real [`SshTunnel`]: establish, answer
    /// `session.identify` through the bridge socket, then re-establish on a fresh
    /// scratch directory after an `invalidate`.
    #[tokio::test]
    async fn an_ssh_bridge_establishes_answers_and_re_establishes_after_invalidate() {
        let fake = FakeRoost::start().await;
        let dir = tempfile::tempdir().expect("tempdir");
        let log = dir.path().join("ssh.log");
        let ssh = write_fake_ssh(dir.path(), fake.socket_path(), &log);
        let bridge = faked_bridge("mini3", ssh);

        let first = bridge.ensure().await.expect("the tunnel establishes");
        let RoostEndpoint::Unix(first_socket) = first.clone() else {
            panic!("an ssh bridge is always a unix socket: {first:?}");
        };
        assert!(first_socket.exists(), "establish binds the bridge socket");
        assert_eq!(ran(&log), vec!["establish"], "the warm-up runs `true`");

        // The wire actually works through it.
        let mut conn = Conn::endpoint(&first).await.expect("dial the bridge");
        let identify = conn.session_identify().await.expect("session.identify");
        assert_eq!(identify.session_id, fake.session_id());
        drop(conn);

        // A held, un-invalidated tunnel is reused rather than rebuilt.
        assert_eq!(bridge.ensure().await.expect("still held"), first);

        bridge.invalidate().await;
        let second = bridge.ensure().await.expect("it re-establishes");
        let RoostEndpoint::Unix(second_socket) = second else {
            panic!("still a unix socket");
        };
        assert_ne!(
            second_socket, first_socket,
            "roost names a scratch directory per ATTEMPT, so the endpoint moves"
        );
        assert!(second_socket.exists());
        // The old tunnel is gone, and its master was exited before the new
        // attempt's warm-up (the log below pins that order). What this CANNOT
        // observe is the reason the order is pinned: roost's sweep reclaims
        // *this process's own* leftovers with no probe, so in one process the
        // two orderings end identically. The refusal the order avoids is
        // another process's live `bridge.sock`, which no unit test can stand up.
        assert!(!first_socket.exists(), "the old scratch directory is gone");
        assert_eq!(
            ran(&log),
            vec!["establish", "connect", "exit", "establish"],
            "one warm-up per attempt, the identify over its own exec, and the \
             old master exited before the new attempt"
        );
    }

    /// A bridge that cannot reach anything fails as a reason, not a panic — and
    /// the message names the machine the way the user addressed it.
    #[tokio::test]
    async fn an_ssh_bridge_that_cannot_connect_names_the_machine() {
        let dir = tempfile::tempdir().expect("tempdir");
        let ssh = dir.path().join("no-such-ssh");
        let bridge = faked_bridge("mini9", ssh);
        let err = bridge.ensure().await.expect_err("there is no ssh to spawn");
        assert!(err.to_string().contains("machine:mini9"), "{err}");
    }

    /// The watcher over the real SSH transport, end to end.
    #[tokio::test]
    async fn a_watcher_reads_an_inventory_through_an_ssh_bridge() {
        let fake = FakeRoost::start().await;
        let dir = tempfile::tempdir().expect("tempdir");
        let log = dir.path().join("ssh.log");
        let ssh = write_fake_ssh(dir.path(), fake.socket_path(), &log);
        fake.set_tab_axes(TAB, "working", Some(owned("session_status")), false);

        let bridge = faked_bridge("mini3", ssh);
        let (watcher, mut rx) = watch(Arc::new(bridge));
        let inventory = next_snapshot(&mut rx).await;
        assert_eq!(inventory.host_label, "roost-host");
        assert!(inventory
            .sessions
            .iter()
            .any(|s| s.tab_id == TAB && s.activity() == Some(RcActivity::Working)));
        watcher.stop();
    }
}
