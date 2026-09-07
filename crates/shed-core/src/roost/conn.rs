//! One connection to a `roost-session`, and the typed ops shed drives it with.
//!
//! [`Conn`] is a **skin**, not a client. The request/response machinery — id
//! allocation and matching, the error envelope, skipping an unsolicited event
//! frame, the 16 MiB line cap — all of it lives in [`roost_ipc::IpcClient`] at
//! the pinned rev, and every future fix to it arrives here for free on a bump.
//! Re-expressing sixty lines of that locally is how two implementations drift
//! silently past each other.
//!
//! What this file adds is the two things `IpcClient` has no opinion about:
//!
//! 1. **A second transport.** `IpcClient::over` takes a
//!    [`tokio::net::UnixStream`] specifically, but a client whose reach to a
//!    machine is a forwarded loopback *port* (mobile's Dart-side bridge; any
//!    `ssh -L`) has a `TcpStream`. [`Conn::tcp_loopback`] bridges the two with a
//!    socketpair and a [`tokio::io::copy_bidirectional`] pump. One extra copy
//!    hop, in exchange for keeping exactly one request/response implementation
//!    in the world. (A generic `IpcClient::over` upstream would delete the pump;
//!    that ask is recorded in plan 013 §4.)
//! 2. **The compatibility gate.** [`Conn::session_identify`] refuses a session
//!    whose `session_protocol` is not this build's, by name, and recognizes a
//!    roost **UI** socket by the `unknown-op` it answers with — so shed never
//!    reads somebody's desktop window as machine inventory.
//!
//! Everything else is a one-line typed wrapper over `IpcClient::call`.

use std::path::{Path, PathBuf};

use roost_ipc::client::{EventFrame, EventStream, ServerCode};
use roost_ipc::messages::{
    ops, IdentifyParams, IdentifyResult, SessionConnectParams, SessionConnectResult,
    SessionIdentify, SessionIdentifyParams, Tab, TabCloseParams, TabDumpParams, TabDumpResult,
    TabListResult, TabOpenParams, TabOpenResult, TabWriteParams, WireTabRef,
    SESSION_PROTOCOL_VERSION,
};
use roost_ipc::{ClientError, IpcClient};
use tokio::net::{TcpStream, UnixStream};
use tokio::task::JoinHandle;

use super::error::RoostError;

/// Where a roost-session can be reached.
///
/// Two shapes because the two reaches are genuinely different, not because one
/// is a special case of the other: a local session (and an SSH bridge socket,
/// which roost's own `SshTunnel` also presents as a Unix socket) is a path, and
/// a forwarded port is a port. `shed-app`'s `RoostReach` returns one of these,
/// and `shed-app` re-exports the type so a client never has to name this module.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum RoostEndpoint {
    /// A Unix socket: the local session, or a bridge socket a tunnel owns.
    Unix(PathBuf),
    /// A loopback TCP port that something else has already pointed at a session.
    TcpLoopback(u16),
}

impl std::fmt::Display for RoostEndpoint {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            RoostEndpoint::Unix(path) => write!(f, "{}", path.display()),
            RoostEndpoint::TcpLoopback(port) => write!(f, "127.0.0.1:{port}"),
        }
    }
}

/// The loopback-TCP pump, owned so it dies with whatever holds the connection.
///
/// Its own type rather than a field on [`Conn`] with a `Drop` impl: a `Drop` on
/// `Conn` would make [`Conn::subscribe`] — which must move the inner client out
/// — impossible to write without `ManuallyDrop`.
struct PumpGuard(JoinHandle<()>);

impl Drop for PumpGuard {
    fn drop(&mut self) {
        self.0.abort();
    }
}

/// One live connection to a roost-session.
///
/// Sequential by construction (that is `IpcClient`'s contract): one request in
/// flight, responses matched by id. Hold one per watcher/peek rather than
/// dialing per call — a redial over SSH is a fresh remote exec.
pub struct Conn {
    client: IpcClient,
    /// `Some` only for [`Self::tcp_loopback`]; aborted when this value dies.
    pump: Option<PumpGuard>,
}

impl Conn {
    // -----------------------------------------------------------------
    // Dialing
    // -----------------------------------------------------------------

    /// Dial a session's Unix socket.
    ///
    /// A missing socket, a refused dial and a permission error are all
    /// [`RoostError::Unavailable`] naming the path: roost's rule is
    /// connect-if-present — shed never spawns a session — so "not there" is an
    /// ordinary state, not a failure.
    pub async fn unix(path: impl AsRef<Path>) -> Result<Conn, RoostError> {
        let path = path.as_ref();
        let client = IpcClient::connect(path).await.map_err(|e| {
            RoostError::Unavailable(format!("no roost-session at {}: {e}", path.display()))
        })?;
        Ok(Conn { client, pump: None })
    }

    /// Dial a loopback TCP port that fronts a session.
    ///
    /// Bridges TCP to the `UnixStream` `IpcClient::over` requires: a socketpair,
    /// one half handed to the client, the other pumped against the TCP stream by
    /// a task that is aborted when this `Conn` (or the [`RoostEventStream`] it
    /// became) is dropped. Nothing about the protocol changes — the pump copies
    /// bytes and has no opinion about frames.
    pub async fn tcp_loopback(port: u16) -> Result<Conn, RoostError> {
        let mut tcp = TcpStream::connect(("127.0.0.1", port)).await.map_err(|e| {
            RoostError::Unavailable(format!("no roost-session on 127.0.0.1:{port}: {e}"))
        })?;
        let (mut ours, theirs) = UnixStream::pair().map_err(|e| {
            RoostError::Wire(format!("creating the loopback bridge socketpair: {e}"))
        })?;
        let pump = tokio::spawn(async move {
            // The result is deliberately dropped: every way this ends — either
            // side closing, the far end vanishing — surfaces to the caller as
            // the next request failing, which is where it belongs. Logging a
            // "connection closed" per teardown would be noise on every poll.
            let _ = tokio::io::copy_bidirectional(&mut ours, &mut tcp).await;
        });
        Ok(Conn {
            client: IpcClient::over(theirs),
            pump: Some(PumpGuard(pump)),
        })
    }

    /// Dial whichever shape a reach handed back.
    pub async fn endpoint(endpoint: &RoostEndpoint) -> Result<Conn, RoostError> {
        match endpoint {
            RoostEndpoint::Unix(path) => Conn::unix(path).await,
            RoostEndpoint::TcpLoopback(port) => Conn::tcp_loopback(*port).await,
        }
    }

    // -----------------------------------------------------------------
    // The compatibility gate
    // -----------------------------------------------------------------

    /// `session.identify` — the first thing to ask, and the gate.
    ///
    /// Two refusals, both by name:
    ///
    /// * `unknown-op` → [`RoostError::NotASession`]. Only a session socket
    ///   serves this op; a roost UI socket answers `unknown-op`, and that is
    ///   roost's documented way of telling the two apart. Shed must never read a
    ///   UI socket as inventory.
    /// * `session_protocol` ≠ [`SESSION_PROTOCOL_VERSION`] →
    ///   [`RoostError::ProtocolMismatch`]. The number covers the lease semantics
    ///   and the lease-gated op set; a client that ignores it does not fail
    ///   until it tries to subscribe.
    ///
    /// The returned [`SessionIdentify`] identifies the **daemon instance**:
    /// `session_id` changes on restart and `revision` resets with it, which is
    /// why a watcher fences per connection rather than globally.
    pub async fn session_identify(&mut self) -> Result<SessionIdentify, RoostError> {
        let identify: SessionIdentify = match self
            .client
            .call(ops::SESSION_IDENTIFY, SessionIdentifyParams {})
            .await
        {
            Ok(identify) => identify,
            Err(ClientError::Server { code, .. })
                if ServerCode::from_wire(&code) == ServerCode::UnknownOp =>
            {
                return Err(RoostError::NotASession)
            }
            Err(e) => return Err(e.into()),
        };
        if identify.session_protocol != SESSION_PROTOCOL_VERSION {
            return Err(RoostError::ProtocolMismatch {
                theirs: identify.session_protocol,
                ours: SESSION_PROTOCOL_VERSION,
            });
        }
        Ok(identify)
    }

    // -----------------------------------------------------------------
    // Typed ops
    // -----------------------------------------------------------------

    /// `identify` — who is on the other end (socket path, pid, app label).
    /// Served by every roost socket, session or UI; informational only, so it is
    /// **not** the gate. [`Self::session_identify`] is.
    pub async fn identify(
        &mut self,
        client_name: &str,
        client_version: &str,
    ) -> Result<IdentifyResult, RoostError> {
        Ok(self
            .client
            .call(
                ops::IDENTIFY,
                IdentifyParams {
                    client_name: client_name.to_string(),
                    client_version: client_version.to_string(),
                },
            )
            .await?)
    }

    /// `tab.list` — the whole workspace, plus the `revision` it was taken at.
    ///
    /// `revision` is `Some` only on a session socket (where it fences an event
    /// stream); a UI socket omits it. Params are `()` — the op takes none, and
    /// sending an object would be inventing a shape.
    pub async fn tab_list(&mut self) -> Result<TabListResult, RoostError> {
        Ok(self.client.call(ops::TAB_LIST, ()).await?)
    }

    /// `tab.dump` — one tab's live viewport as text. Read-only; this is the
    /// whole of the "peek" affordance until roost R3 lands attach.
    pub async fn tab_dump(&mut self, tab_id: i64) -> Result<TabDumpResult, RoostError> {
        Ok(self
            .client
            .call(
                ops::TAB_DUMP,
                TabDumpParams {
                    tab_id: WireTabRef::Local(tab_id),
                },
            )
            .await?)
    }

    /// `tab.open` — start a tab (an agent, given its argv) and get it back.
    /// Lease-free, like every other op here except `events.subscribe`.
    pub async fn tab_open(&mut self, params: TabOpenParams) -> Result<Tab, RoostError> {
        let result: TabOpenResult = self.client.call(ops::TAB_OPEN, params).await?;
        Ok(result.tab)
    }

    /// `tab.write` — raw bytes into a tab's PTY, base64 on the wire. Byte-exact:
    /// this is how a prompt gets typed at an agent.
    pub async fn tab_write(&mut self, tab_id: i64, data: &[u8]) -> Result<(), RoostError> {
        self.client
            .call_raw(
                ops::TAB_WRITE,
                TabWriteParams {
                    tab_id,
                    data: data.to_vec(),
                },
            )
            .await?;
        Ok(())
    }

    /// `tab.close` — end a tab. A closed tab leaves `tab.list` entirely, which
    /// is why the row model has no "dead" state.
    pub async fn tab_close(&mut self, tab_id: i64) -> Result<(), RoostError> {
        self.client
            .call_raw(ops::TAB_CLOSE, TabCloseParams { tab_id })
            .await?;
        Ok(())
    }

    /// `session.connect` — take the session's single interactive lease.
    ///
    /// **A watcher must not call this.** The lease is singular and a
    /// `takeover: true` closes the current holder's event stream — which, on a
    /// machine somebody is using, is the roost UI itself. Shed polls `tab.list`
    /// until roost R1 re-cuts the lease; this op exists for the code that comes
    /// after that, and for [`Self::subscribe`]'s tests.
    pub async fn session_connect(
        &mut self,
        takeover: bool,
    ) -> Result<SessionConnectResult, RoostError> {
        Ok(self
            .client
            .call(ops::SESSION_CONNECT, SessionConnectParams { takeover })
            .await?)
    }

    /// `events.subscribe` — flip this connection into the server's push stream.
    ///
    /// Consumes the `Conn` because that is `IpcClient`'s contract: the ack is
    /// the last request/response frame the connection will ever carry. `lease`
    /// is what [`Self::session_connect`] minted — see the warning there before
    /// reaching for either.
    pub async fn subscribe(self, lease: &str) -> Result<RoostEventStream, RoostError> {
        // Destructured rather than dropped: the pump has to outlive the
        // handover, or the stream reads from a socketpair with nobody feeding
        // it. (It is aborted on the error path too — `pump` is a local here.)
        let Conn { client, pump } = self;
        let stream = client.subscribe_events(lease).await?;
        Ok(RoostEventStream {
            stream,
            _pump: pump,
        })
    }
}

/// A subscribed connection, reading roost's push stream.
///
/// Thin over [`roost_ipc::client::EventStream`]: the gap check that makes a
/// missed revision *detectable* is roost's, and this only re-labels it as
/// [`RoostError::RevisionGap`] so a caller matches on shed's error type
/// throughout.
pub struct RoostEventStream {
    stream: EventStream,
    /// Held, never read: dropping it aborts the loopback pump.
    _pump: Option<PumpGuard>,
}

impl RoostEventStream {
    /// The ack's fence — the commit this subscription starts from. A `tab.list`
    /// taken alongside is fenced by discarding every batch `<=` this.
    pub fn revision(&self) -> u64 {
        self.stream.revision()
    }

    /// Why the stream ended, once the terminal envelope has arrived: `"stop"`
    /// (the session is shutting down) or `"taken-over"` (another client took the
    /// lease).
    pub fn stopping_reason(&self) -> Option<&str> {
        self.stream.stopping_reason()
    }

    /// The next pushed frame, or `Ok(None)` when the server closed the stream.
    /// A close is a documented signal (resync), not an error.
    pub async fn next(&mut self) -> Result<Option<EventFrame>, RoostError> {
        Ok(self.stream.next().await?)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::roost::testing::FakeRoost;

    /// Both reaches to one fake, labelled — dialed through [`Conn::endpoint`],
    /// which is what a client's reach actually hands a [`RoostEndpoint`] to.
    fn endpoints(fake: &FakeRoost) -> [(&'static str, RoostEndpoint); 2] {
        [
            ("unix", RoostEndpoint::Unix(fake.socket_path().to_owned())),
            ("tcp", RoostEndpoint::TcpLoopback(fake.tcp_port())),
        ]
    }

    /// A connection over each transport — every typed op test runs over both,
    /// because the TCP path is a *different* byte path (socketpair + pump) and
    /// a Unix-only test proves nothing about it.
    async fn conns(fake: &FakeRoost) -> Vec<(&'static str, Conn)> {
        let mut conns = Vec::new();
        for (via, endpoint) in endpoints(fake) {
            conns.push((via, Conn::endpoint(&endpoint).await.expect(via)));
        }
        conns
    }

    #[tokio::test]
    async fn identify_answers_on_both_transports() {
        let fake = FakeRoost::start().await;
        for (via, mut conn) in conns(&fake).await {
            let identified = conn.identify("shed", "0.0.0").await.expect(via);
            assert_eq!(identified.app_label, "Roost", "via {via}");
            assert!(identified.pid > 0, "via {via}");
        }
    }

    #[tokio::test]
    async fn session_identify_reports_the_daemon_instance() {
        let fake = FakeRoost::start().await;
        for (via, mut conn) in conns(&fake).await {
            let session = conn.session_identify().await.expect(via);
            assert_eq!(session.session_protocol, SESSION_PROTOCOL_VERSION);
            assert_eq!(session.session_id, fake.session_id(), "via {via}");
            assert!(!session.started_at.is_empty(), "via {via}");
        }
    }

    #[tokio::test]
    async fn tab_list_carries_the_session_revision() {
        let fake = FakeRoost::start().await;
        for (via, mut conn) in conns(&fake).await {
            let listed = conn.tab_list().await.expect(via);
            assert_eq!(listed.revision, Some(fake.revision()), "via {via}");
            assert_eq!(listed.projects.len(), 1, "via {via}");
            assert_eq!(listed.projects[0].tabs.len(), 1, "via {via}");
        }

        // A revision bump is visible to the next list — the whole basis of the
        // watcher's change detection.
        fake.bump_revision();
        let mut conn = Conn::unix(fake.socket_path()).await.expect("dial");
        assert_eq!(conn.tab_list().await.expect("list").revision, Some(43));
    }

    #[tokio::test]
    async fn an_opened_tab_appears_in_the_next_list() {
        let fake = FakeRoost::start().await;
        for (via, mut conn) in conns(&fake).await {
            let before = conn.tab_list().await.expect(via).projects[0].tabs.len();
            let opened = conn
                .tab_open(TabOpenParams {
                    project_id: 1,
                    cwd: "/home/shed/work".into(),
                    argv: vec!["opencode".into()],
                    title: "opencode".into(),
                    ..TabOpenParams::default()
                })
                .await
                .expect(via);
            assert_eq!(opened.cwd, "/home/shed/work", "via {via}");

            let listed = conn.tab_list().await.expect(via);
            let tabs = &listed.projects[0].tabs;
            assert_eq!(tabs.len(), before + 1, "via {via}");
            assert!(
                tabs.iter().any(|t| t.id == opened.id),
                "the opened tab is in the list, via {via}"
            );
            // `tab.open` is a mutation, so it commits a new revision.
            assert!(listed.revision > Some(42), "via {via}");
        }
    }

    #[tokio::test]
    async fn tab_write_round_trips_every_byte() {
        let fake = FakeRoost::start().await;
        // 0x00..=0xff: base64 on the wire, so a byte that a naive string
        // encoding would mangle (NUL, 0x0a, everything above 0x7f) is exactly
        // what has to survive.
        let payload: Vec<u8> = (0..=u8::MAX).collect();
        for (via, mut conn) in conns(&fake).await {
            let tab = conn.tab_open(TabOpenParams::default()).await.expect(via).id;
            conn.tab_write(tab, &payload).await.expect(via);
            assert_eq!(fake.written(tab), payload, "via {via}");
        }
    }

    #[tokio::test]
    async fn tab_dump_returns_the_viewport() {
        let fake = FakeRoost::start().await;
        for (via, mut conn) in conns(&fake).await {
            let dump = conn.tab_dump(5).await.expect(via);
            assert_eq!(dump.rows as usize, dump.rows_text.len(), "via {via}");
            assert!(dump.rows_text[0].contains("tab 5"), "via {via}");
            assert_eq!(dump.cursor.map(|c| c.visible), Some(true), "via {via}");
        }
    }

    #[tokio::test]
    async fn tab_close_removes_the_tab() {
        let fake = FakeRoost::start().await;
        let mut conn = Conn::unix(fake.socket_path()).await.expect("dial");
        conn.tab_close(5).await.expect("close");
        assert!(conn.tab_list().await.expect("list").projects[0]
            .tabs
            .is_empty());
    }

    #[tokio::test]
    async fn a_protocol_mismatch_is_refused_by_name() {
        let fake = FakeRoost::start().await;
        fake.set_session_protocol(SESSION_PROTOCOL_VERSION + 7);
        for (via, mut conn) in conns(&fake).await {
            match conn.session_identify().await {
                Err(RoostError::ProtocolMismatch { theirs, ours }) => {
                    assert_eq!(theirs, SESSION_PROTOCOL_VERSION + 7, "via {via}");
                    assert_eq!(ours, SESSION_PROTOCOL_VERSION, "via {via}");
                }
                other => panic!("expected ProtocolMismatch via {via}, got {other:?}"),
            }
        }
    }

    #[tokio::test]
    async fn a_ui_socket_is_not_a_session() {
        let fake = FakeRoost::start().await;
        // roost's own tell: only a session socket serves `session.identify`.
        fake.serve_as_ui_socket(true);
        for (via, mut conn) in conns(&fake).await {
            match conn.session_identify().await {
                Err(RoostError::NotASession) => {}
                other => panic!("expected NotASession via {via}, got {other:?}"),
            }
            // …and it is still a perfectly good roost socket for `identify`,
            // which is exactly why the plain op cannot be the gate.
            conn.identify("shed", "0.0.0").await.expect(via);
        }
    }

    #[tokio::test]
    async fn an_unknown_op_surfaces_as_a_server_refusal() {
        let fake = FakeRoost::start().await;
        let mut conn = Conn::unix(fake.socket_path()).await.expect("dial");
        // `session.connect` is not served by the fake — it is the one op shed
        // must not call from a watcher, so the fake refuses it on purpose.
        match conn.session_connect(false).await {
            Err(err @ RoostError::Server { .. }) => {
                assert_eq!(err.server_code(), Some(ServerCode::UnknownOp));
            }
            other => panic!("expected a server refusal, got {other:?}"),
        }
    }

    #[tokio::test]
    async fn no_socket_is_quietly_unavailable() {
        let missing = std::env::temp_dir().join("shed-roost-does-not-exist/roost.sock");
        match Conn::unix(&missing).await {
            Err(err @ RoostError::Unavailable(_)) => {
                assert!(err.is_unavailable());
                assert!(
                    err.to_string().contains("shed-roost-does-not-exist"),
                    "the reason names the path tried: {err}"
                );
            }
            Err(other) => panic!("expected Unavailable, got {other:?}"),
            Ok(_) => panic!("dialing a socket that does not exist must not succeed"),
        }

        // Same for a port with nothing behind it. Port 1 is privileged and
        // unbindable, so nothing can be listening there.
        match Conn::tcp_loopback(1).await {
            Err(err @ RoostError::Unavailable(_)) => {
                assert!(err.to_string().contains("127.0.0.1:1"), "{err}");
            }
            Err(other) => panic!("expected Unavailable, got {other:?}"),
            Ok(_) => panic!("dialing a port with nothing behind it must not succeed"),
        }
    }

    #[tokio::test]
    async fn a_dropped_connection_is_unavailable_not_wire() {
        let fake = FakeRoost::start().await;
        // Dialed one at a time on purpose: `close_all` hangs up on everything
        // live, so a pair opened up front would have the first iteration close
        // the second's connection out from under it.
        for (via, endpoint) in endpoints(&fake) {
            let mut conn = Conn::endpoint(&endpoint).await.expect(via);
            // One successful round trip first: it proves the fake has ACCEPTED
            // this connection and its handler is subscribed to the hang-up, so
            // the close below is not racing the accept loop.
            conn.tab_list().await.expect(via);
            fake.close_all();
            match conn.tab_list().await {
                Err(err) => assert!(
                    err.is_unavailable(),
                    "a hung-up session is Unavailable, via {via}: {err}"
                ),
                Ok(_) => panic!("expected the closed connection to fail, via {via}"),
            }
        }
    }

    #[tokio::test]
    async fn a_restart_changes_the_session_id_and_resets_the_revision() {
        let fake = FakeRoost::start().await;
        let mut conn = Conn::unix(fake.socket_path()).await.expect("dial");
        let first = conn.session_identify().await.expect("identify");
        assert_eq!(conn.tab_list().await.expect("list").revision, Some(42));

        fake.restart();

        let mut conn = Conn::unix(fake.socket_path()).await.expect("redial");
        let second = conn.session_identify().await.expect("identify");
        assert_ne!(first.session_id, second.session_id);
        // Tab ids persist across a restart; the in-process revision counter
        // does not. Both halves are why a fence is per connection.
        assert_eq!(conn.tab_list().await.expect("list").revision, Some(1));
        assert_eq!(
            conn.tab_list().await.expect("list").projects[0].tabs[0].id,
            5
        );
    }
}
