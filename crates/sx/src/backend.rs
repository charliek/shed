//! Building the `shed-app` [`Backend`] that `sx` reads sheds through — with the
//! host-agent control-token minter wired in when the agent is actually there.
//!
//! Two constructors, because the two callers want opposite things:
//!
//! * [`config_backend`] — pure config, NO minter. Everything that only needs a
//!   server entry's SSH endpoint (`--on shed:<name>@<server>`) takes this: it
//!   performs no HTTP, so it cannot fail on a credential and must never pay for
//!   one.
//! * [`with_backend`] — the HTTP fan-out (`sx ls`, and the unqualified
//!   `shed:<name>` lookup). It wires [`HostAgentTokenMinter`] exactly as the
//!   desktop's external mode does (`desktop/tauri/src-tauri/src/broker.rs`), so a
//!   server enrolled for `auth.mode: mtls` — which by construction holds no
//!   static `control_token` — can be listed at all.
//!
//! ## Why the minter is GATED on a live agent
//!
//! Installing a minter replaces a client's static-token path outright: shed-core
//! builds a `ControlTokenProvider` and the entry's `control_token` is never sent
//! again (`shed-core/src/http.rs`, `Client::credential`). That is right when the
//! agent can mint, and a regression when it cannot — every token-mode server in
//! the config would start failing closed on a machine with no host agent, which
//! is strictly worse than today's behavior.
//!
//! So the minter is wired only when the agent is answering: the desktop socket
//! must exist AND the agent's read-only STATUS socket must accept a connection
//! within [`PROBE_TIMEOUT`]. No agent (or a stale socket file left by a crashed
//! one) → the plain [`Backend::from_env_parts`] this module replaced, i.e. byte
//! for byte today's behavior.
//!
//! The gate covers agent-ABSENT, deliberately not agent-present-but-can't-mint:
//! with a live agent the minter is attached to EVERY server (the Backend API is
//! all-or-nothing), so a token-mode server whose config token works but whose
//! host-agent SSH key is NOT allowlisted there would fail its mint and drop out
//! of the fan-out. This matches the desktop's accepted posture (the F6 note in
//! `shed-app/src/backend.rs`) — a machine's agent is expected to hold keys for
//! the servers that machine uses — and the `@server` form always bypasses HTTP
//! entirely. Narrowing the minter to only the entries that NEED it (empty
//! `control_token` / `auth_mode: mtls`) is a shed-app API change shared with
//! the desktop, recorded as follow-up work rather than forked here.
//!
//! The liveness probe deliberately dials the STATUS socket, never the desktop
//! one: the desktop channel is single-consumer/last-writer-wins, so a probe
//! connection there would supersede a running desktop app for no reason. The
//! status socket is read-only and multi-client, and the daemon binds both
//! unconditionally (`crates/shed-host-agent/src/main.rs`).
//!
//! The MINTING connection is on the desktop channel and therefore does supersede
//! a running desktop app for the length of one verb — the app is told
//! `hello_ack{accepted:false, reason:"superseded"}`, and reconnects (superseding
//! `sx` back) after its own 500 ms backoff. That is the price of the one socket
//! that can mint, and it is bounded by [`with_backend`] stopping the connection
//! as soon as the fan-out returns; approvals are dark only for that window.
//!
//! ## Bounds
//!
//! Nothing here can hang. The probe is `PROBE_TIMEOUT`-bounded; a mint against a
//! connected-but-silent agent is bounded by the minter's own pre-ack capability
//! wait (2 s, memoized per burst — `shed-app/src/token_minter.rs`) and then by
//! shed-core's per-request credential bound (the 8 s `GET_TIMEOUT` that already
//! bounds the same fan-out today). A per-server failure stays a per-server
//! failure: `Backend::list_sheds` drops it, so the other servers still answer and
//! the "no running shed named …" message still points at `--on shed:<n>@<server>`.

use std::collections::HashSet;
use std::path::{Path, PathBuf};
use std::sync::Arc;
use std::time::Duration;

use shed_app::{AuthModeRegistry, Backend, HelloClientInfo, HostAgentClient, HostAgentTokenMinter};
use shed_core::token::TokenMinter;

use crate::cli::{Deps, PROG};
use crate::target;

/// How long the agent-liveness probe waits for the status socket to accept.
/// Mirrors the daemon's own stale-socket probe bound (`shed-broker`'s
/// `SOCKET_PROBE_TIMEOUT`, 500 ms): a live local agent accepts in microseconds,
/// and a stale socket file fails fast with ECONNREFUSED anyway.
const PROBE_TIMEOUT: Duration = Duration::from_millis(500);

/// A `Backend` built from config alone — no host-agent connection, no minter.
///
/// The right (and only) choice for the pure-config lookups: they resolve an SSH
/// endpoint out of `~/.shed/config.yaml` and never issue a request, so paying for
/// a credential would be pure cost.
pub fn config_backend(deps: &Deps) -> Backend {
    let config_path = target::default_config_path(&*deps.env);
    Backend::from_env_parts(false, None, Path::new(&config_path))
}

/// Run `f` against a `Backend` whose HTTP clients can mint control credentials
/// through the host agent, when one is running.
///
/// The whole borrow is scoped: the agent connection is started before `f` and
/// stopped after it, so a one-shot `sx` verb owns the socket for the length of
/// one fan-out rather than for the process lifetime.
///
/// Called OUTSIDE any runtime — it owns the `block_on` (the agent's connection
/// loop is a `tokio::spawn`, so it must be started from inside one).
pub fn with_backend<T>(deps: &Deps, f: impl AsyncFnOnce(&Backend) -> T) -> T {
    let config_path = target::default_config_path(&*deps.env);
    let sockets = AgentSockets::resolve(&*deps.env);
    deps.block_on(async move {
        let agent = connect_agent(&sockets).await;
        let backend = match &agent {
            Some(a) => Backend::from_env_parts_with_credentials(
                false,
                None,
                Path::new(&config_path),
                Some(&a.minter),
                Some(&a.modes),
                &HashSet::new(),
            ),
            None => Backend::from_env_parts(false, None, Path::new(&config_path)),
        };
        let out = f(&backend).await;
        if let Some(a) = agent {
            a.client.stop();
        }
        out
    })
}

/// A live host-agent connection plus the two handles the `Backend` is built
/// from. Held only for the length of one [`with_backend`] call.
struct Agent {
    client: HostAgentClient,
    minter: Arc<dyn TokenMinter>,
    modes: Arc<AuthModeRegistry>,
}

/// Dial the host agent, or `None` when it is not answering.
async fn connect_agent(sockets: &AgentSockets) -> Option<Agent> {
    if !is_socket(&sockets.desktop) || !status_socket_live(&sockets.status).await {
        return None;
    }
    let client = HostAgentClient::new(sockets.desktop.clone(), Arc::new(shed_app::SystemClock));
    // The registry is shared: `Backend::from_env_parts_with_credentials` SEEDS it
    // from the config's `auth_mode` entries and attaches it as each provider's
    // observer, and the minter reads it to decide whether a mint that begins
    // before the agent's `hello_ack` should expect a certificate (plan 002 §7 P5).
    // Constructed here because the minter needs it BEFORE the Backend exists.
    let modes = Arc::new(AuthModeRegistry::new());
    let minter: Arc<dyn TokenMinter> =
        Arc::new(HostAgentTokenMinter::new(client.clone()).with_modes(Arc::clone(&modes)));
    // The event stream is dropped on purpose: `sx` mints, it does not broker.
    // Announcing NO capabilities is what says so (an `approval.ssh` claim would
    // route approvals to a process that exits in a second), and `replay_events: 0`
    // asks for no audit backlog. Dropping the receiver closes the channel, so the
    // connection loop's event sends fail silently instead of buffering.
    drop(client.start(HelloClientInfo {
        name: PROG.to_string(),
        version: env!("CARGO_PKG_VERSION").to_string(),
        pid: std::process::id() as i32,
        capabilities: Vec::new(),
        replay_events: 0,
    }));
    Some(Agent {
        client,
        minter,
        modes,
    })
}

/// The agent's two well-known socket paths.
struct AgentSockets {
    /// The stateful approval/credential channel — what the minter dials.
    desktop: PathBuf,
    /// The read-only status socket — what the liveness probe dials.
    status: PathBuf,
}

impl AgentSockets {
    fn resolve(env: &dyn Fn(&str) -> String) -> Self {
        let dir = socket_dir(env);
        Self {
            desktop: dir.join("host-agent.sock"),
            status: dir.join("host-agent-status.sock"),
        }
    }
}

/// Where `shed-host-agent` places its sockets, per platform — the same rule the
/// daemon itself applies (`shed-broker`'s `sockets::socket_dir`) and the desktop
/// clients mirror: an explicit `SHED_HOST_AGENT_SOCKET_DIR` wins everywhere; else
/// macOS uses `~/Library/Application Support/shed` and Linux the XDG convention
/// (`$XDG_RUNTIME_DIR/shed`, else `~/.local/share/shed`).
///
/// Read through `sx`'s injected env reader, so a test (or a parallel dev agent)
/// can point it somewhere hermetic.
fn socket_dir(env: &dyn Fn(&str) -> String) -> PathBuf {
    let explicit = env("SHED_HOST_AGENT_SOCKET_DIR");
    if !explicit.is_empty() {
        return PathBuf::from(explicit);
    }
    let home = || PathBuf::from(env("HOME"));
    if cfg!(target_os = "macos") {
        return home().join("Library/Application Support/shed");
    }
    let xdg = env("XDG_RUNTIME_DIR");
    if !xdg.is_empty() {
        return PathBuf::from(xdg).join("shed");
    }
    home().join(".local/share/shed")
}

/// Is there a real socket (not a symlink, not a regular file) at `path`? The same
/// pre-connect check the host-agent client itself applies before dialing.
fn is_socket(path: &Path) -> bool {
    use std::os::unix::fs::FileTypeExt;
    std::fs::symlink_metadata(path)
        .map(|m| m.file_type().is_socket())
        .unwrap_or(false)
}

/// Does something accept on the agent's status socket within [`PROBE_TIMEOUT`]?
/// A connect to a Unix stream socket resolves immediately — it succeeds if a
/// listener is bound and fails fast (ECONNREFUSED/ENOENT) otherwise; the timeout
/// only guards a pathological peer with a full accept backlog.
async fn status_socket_live(path: &Path) -> bool {
    if !is_socket(path) {
        return false;
    }
    matches!(
        tokio::time::timeout(PROBE_TIMEOUT, tokio::net::UnixStream::connect(path)).await,
        Ok(Ok(_))
    )
}

#[cfg(test)]
mod tests {
    use super::*;

    fn env_of(pairs: &[(&'static str, &'static str)]) -> impl Fn(&str) -> String + use<> {
        let pairs: Vec<(String, String)> = pairs
            .iter()
            .map(|(k, v)| (k.to_string(), v.to_string()))
            .collect();
        move |key: &str| {
            pairs
                .iter()
                .find(|(k, _)| k == key)
                .map(|(_, v)| v.clone())
                .unwrap_or_default()
        }
    }

    #[test]
    fn socket_dir_prefers_the_explicit_override() {
        let env = env_of(&[
            ("HOME", "/Users/dev"),
            ("SHED_HOST_AGENT_SOCKET_DIR", "/tmp/agent"),
            ("XDG_RUNTIME_DIR", "/run/user/501"),
        ]);
        let sockets = AgentSockets::resolve(&env);
        assert_eq!(sockets.desktop, PathBuf::from("/tmp/agent/host-agent.sock"));
        assert_eq!(
            sockets.status,
            PathBuf::from("/tmp/agent/host-agent-status.sock")
        );
    }

    #[test]
    fn socket_dir_follows_the_platform_default() {
        let env = env_of(&[("HOME", "/Users/dev"), ("XDG_RUNTIME_DIR", "/run/user/501")]);
        let dir = socket_dir(&env);
        if cfg!(target_os = "macos") {
            // The load-bearing mac branch: resolving the Linux path here is what
            // makes a mac client silently miss the agent that is actually running.
            assert_eq!(
                dir,
                PathBuf::from("/Users/dev/Library/Application Support/shed")
            );
        } else {
            assert_eq!(dir, PathBuf::from("/run/user/501/shed"));
        }
        // No XDG_RUNTIME_DIR on Linux → the stable home path.
        let env = env_of(&[("HOME", "/home/dev")]);
        if !cfg!(target_os = "macos") {
            assert_eq!(
                socket_dir(&env),
                PathBuf::from("/home/dev/.local/share/shed")
            );
        }
    }

    #[test]
    fn a_missing_or_non_socket_path_is_not_an_agent() {
        let dir = tempdir();
        assert!(!is_socket(&dir.join("nothing.sock")));
        std::fs::write(dir.join("regular"), b"x").unwrap();
        assert!(!is_socket(&dir.join("regular")));
    }

    /// The degradation gate: with no agent sockets at all, `connect_agent`
    /// answers `None` promptly — which is what makes `with_backend` fall back to
    /// the plain, static-token `Backend` (today's behavior) rather than installing
    /// a minter that could never mint.
    #[test]
    fn no_agent_means_no_minter() {
        let dir = tempdir();
        let sockets = AgentSockets {
            desktop: dir.join("host-agent.sock"),
            status: dir.join("host-agent-status.sock"),
        };
        let rt = tokio::runtime::Builder::new_current_thread()
            .enable_all()
            .build()
            .unwrap();
        let started = std::time::Instant::now();
        assert!(rt.block_on(connect_agent(&sockets)).is_none());
        assert!(
            started.elapsed() < PROBE_TIMEOUT,
            "an absent socket must fail the gate without waiting out the probe: {:?}",
            started.elapsed()
        );
    }

    /// A STALE socket file (the crashed-agent shape: the file survives, nothing
    /// accepts) must also degrade — otherwise every token-mode server would lose
    /// its static token to a minter with no agent behind it.
    #[test]
    fn a_stale_socket_file_does_not_pass_the_liveness_probe() {
        let dir = tempdir();
        let rt = tokio::runtime::Builder::new_current_thread()
            .enable_all()
            .build()
            .unwrap();
        // Bind + drop: the path is a real socket file with no listener.
        let desktop = dir.join("host-agent.sock");
        let status = dir.join("host-agent-status.sock");
        for p in [&desktop, &status] {
            drop(std::os::unix::net::UnixListener::bind(p).unwrap());
            // Binding leaves the file behind after the listener is dropped.
            assert!(is_socket(p), "the stale file is still a socket inode");
        }
        let sockets = AgentSockets { desktop, status };
        let started = std::time::Instant::now();
        assert!(rt.block_on(connect_agent(&sockets)).is_none());
        assert!(
            started.elapsed() < PROBE_TIMEOUT,
            "a refused connect must fail fast, not wait out the probe: {:?}",
            started.elapsed()
        );
    }

    /// The positive half: a listener that accepts is enough to pass the gate, and
    /// the connection sx starts is a real one it can also stop.
    #[test]
    fn a_live_status_listener_wires_the_minter() {
        let dir = tempdir();
        let desktop = dir.join("host-agent.sock");
        let status = dir.join("host-agent-status.sock");
        let rt = tokio::runtime::Builder::new_current_thread()
            .enable_all()
            .build()
            .unwrap();
        let agent = rt.block_on(async {
            // The desktop socket only has to EXIST as a socket for the gate; the
            // status one has to accept.
            let _desktop_listener = tokio::net::UnixListener::bind(&desktop).unwrap();
            let _status_listener = tokio::net::UnixListener::bind(&status).unwrap();
            let sockets = AgentSockets {
                desktop: desktop.clone(),
                status: status.clone(),
            };
            let agent = connect_agent(&sockets).await;
            agent.map(|a| {
                a.client.stop();
                // The minter is the host-agent one, bound to the shared registry
                // the Backend will seed from config.
                (a.minter.supports_mtls(), Arc::strong_count(&a.modes))
            })
        });
        let (supports_mtls, modes_refs) = agent.expect("a live status socket passes the gate");
        // Nothing is known about any server yet (the registry is seeded by the
        // Backend), so the pre-ack answer is "no certificates expected".
        assert!(!supports_mtls);
        assert!(modes_refs >= 2, "the registry is shared with the minter");
    }

    /// A short-pathed scratch dir: a Unix socket path has a ~104-byte ceiling
    /// (`SUN_LEN`), which macOS's per-user `TMPDIR` alone can nearly exhaust — so
    /// this deliberately uses `/tmp` rather than `std::env::temp_dir()`.
    fn tempdir() -> PathBuf {
        static N: std::sync::atomic::AtomicU32 = std::sync::atomic::AtomicU32::new(0);
        let n = N.fetch_add(1, std::sync::atomic::Ordering::SeqCst);
        let base = PathBuf::from(format!("/tmp/sx-b-{}-{n}", std::process::id()));
        let _ = std::fs::remove_dir_all(&base);
        std::fs::create_dir_all(&base).unwrap();
        base
    }
}
