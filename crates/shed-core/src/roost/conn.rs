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
    ops, AgentHooksMode, IdentifyParams, IdentifyResult, SessionIdentify, SessionIdentifyParams,
    SessionSetAgentHooksParams, SessionSetAgentHooksResult, Tab, TabCloseParams, TabDumpParams,
    TabDumpResult, TabListResult, TabOpenParams, TabOpenResult, TabWriteParams, WireTabRef,
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
    ///   [`RoostError::ProtocolMismatch`]. The number covers the whole op set
    ///   and its parameter shapes; a client that ignores it does not fail until
    ///   it sends a request the peer's generation refuses to decode.
    ///
    /// **At generation 5 the integer is the whole negotiation.** roost's
    /// `features` list retired with the lease — there is no additive-op channel
    /// beside the number any more, so equality here is the only compatibility
    /// question there is to ask.
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
    ///
    /// `scrollback: 0` is deliberate and stays deliberate: a peek is a viewport
    /// read, and history above it is a different affordance with a different
    /// cost (roost formats the rows on the thread that owns the terminal). The
    /// key is omit-when-zero on roost's side, so a viewport-only request is
    /// byte-identical to what shed sent before the field existed — which is
    /// what `tab.dump.request.json` pins.
    pub async fn tab_dump(&mut self, tab_id: i64) -> Result<TabDumpResult, RoostError> {
        Ok(self
            .client
            .call(
                ops::TAB_DUMP,
                TabDumpParams {
                    tab_id: WireTabRef::Local(tab_id),
                    scrollback: 0,
                },
            )
            .await?)
    }

    /// `tab.open` — start a tab (an agent, given its argv) and get it back.
    pub async fn tab_open(&mut self, params: TabOpenParams) -> Result<Tab, RoostError> {
        let result: TabOpenResult = self.client.call(ops::TAB_OPEN, params).await?;
        Ok(result.tab)
    }

    /// `tab.write` — raw bytes into a tab's PTY, base64 on the wire. Byte-exact:
    /// this is how a prompt gets typed at an agent.
    ///
    /// **Unowned at session protocol 5.** Generation 4 gated a write behind the
    /// single interactive lease — `connect-required` without one, `taken-over`
    /// on a displaced one. roost retired that token, so a write is now open to
    /// every same-UID client and the request carries nothing but the tab and the
    /// bytes. The mediation that remains is the user's: one person owns every
    /// client that can reach this socket.
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

    /// `session.set_agent_hooks` — ask the host session to bring its agent hook
    /// entries in line with this client's configuration.
    ///
    /// **The host does the writing.** roost's session links
    /// `roost-agent-install` and edits the dotfiles under its own `$HOME`;
    /// nothing on shed's side touches a file on that machine (pin P5). That is
    /// what makes this op the right way to wire hooks and a hand-rolled `ssh`
    /// heredoc the wrong one.
    ///
    /// **Open to every same-UID client at session protocol 5.** Generation 4
    /// gated this behind the interactive lease, so a second client had to take
    /// the token away from the first before it could wire anything; roost
    /// deleted that authority check outright. The op is declarative — the host
    /// brings its entries in line with the request, writing nothing for an agent
    /// whose config directory does not exist — so the last writer wins and
    /// `client` is the record of who that was. One caller should be
    /// [`bootstrap::wire_agent_hooks`](crate::roost::bootstrap::wire_agent_hooks),
    /// which is shed's one composition of it.
    ///
    /// `mode: Off` **removes** roost's entries rather than meaning "do nothing":
    /// a host has no config of its own to consult, so the client is the
    /// authority and `off` on the client means the host comes clean.
    pub async fn session_set_agent_hooks(
        &mut self,
        mode: AgentHooksMode,
        skip: &[String],
        client: &str,
    ) -> Result<SessionSetAgentHooksResult, RoostError> {
        Ok(self
            .client
            .call(
                ops::SESSION_SET_AGENT_HOOKS,
                SessionSetAgentHooksParams {
                    mode,
                    skip: skip.to_vec(),
                    client: client.to_string(),
                },
            )
            .await?)
    }

    /// One op by name, params and result as raw JSON — **the ungated call**.
    ///
    /// The escape hatch exists for exactly one caller and the doc says so, so it
    /// does not quietly become the way ops get added:
    /// [`bootstrap`](crate::roost::bootstrap)'s sans-IO machines name their own
    /// ops (today only `session.identify`) and apply their **own** compatibility
    /// gate, which is the protocol number alone. [`Self::session_identify`]
    /// refuses a mismatch by name, which is exactly right for a watcher — it has
    /// nothing useful to do with a session it cannot read — and exactly wrong
    /// for the bootstrap, whose fifth plan-matrix row IS "a session on protocol
    /// N is serving here; report it and touch nothing" (pin P6). That row needs
    /// the mismatched session's own identity, so the reply has to arrive as an
    /// answer rather than as an error.
    ///
    /// Everything else in this file is a typed wrapper, and should stay one.
    pub async fn call_raw(
        &mut self,
        op: &str,
        params: serde_json::Value,
    ) -> Result<serde_json::Value, RoostError> {
        Ok(self.client.call_raw(op, params).await?)
    }

    /// `events.subscribe` — flip this connection into the server's push stream.
    ///
    /// Consumes the `Conn` because that is `IpcClient`'s contract: the ack is
    /// the last request/response frame the connection will ever carry.
    ///
    /// **A fresh subscribe takes no arguments at session protocol 5.** The lease
    /// that used to classify a stream as driver or observer is gone, and with it
    /// the classification: every subscriber now receives every frame, including
    /// `tab.effect`. shed's fold ignores effects because a watcher views no tab
    /// (see [`crate::roost::Fence`]) — the extra frames cost a match arm, not a
    /// decision.
    ///
    /// The write half must stay open for as long as the stream is read: roost
    /// keeps reading this connection to notice a peer that went away, and a
    /// half-close is how a peer says it is gone.
    pub async fn subscribe(self) -> Result<RoostEventStream, RoostError> {
        // Destructured rather than dropped: the pump has to outlive the
        // handover, or the stream reads from a socketpair with nobody feeding
        // it. (It is aborted on the error path too — `pump` is a local here.)
        let Conn { client, pump } = self;
        let stream = client.subscribe_events().await?;
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

    /// The daemon incarnation that answered *this* subscribe.
    ///
    /// **It exists because a client dials twice.** shed identifies on one
    /// connection and subscribes on another, so a `roost-session` that restarts
    /// between the two hands back a snapshot from one process and an event
    /// stream from another — a pairing that looks healthy and is not. roost
    /// answers every ack with its own id precisely so the client can refuse
    /// that pair itself; it does **not** check on a fresh subscribe, because it
    /// only compares when the request names a `session_id`, which is the resume
    /// path shed does not take.
    ///
    /// Compare it with the `session_id` [`Conn::session_identify`] returned and
    /// start the cycle over on a disagreement; shed's watcher is the caller
    /// that does, before it ever issues the cycle's `tab.list`.
    pub fn session_id(&self) -> &str {
        self.stream.session_id()
    }

    /// Why the stream ended, once the terminal envelope has arrived. At session
    /// protocol 5 the only reachable reason is `"stop"`, and now structurally so
    /// rather than by convention: roost's `CloseReason` has exactly one variant,
    /// so the session stopping is the only thing that ends a stream.
    pub fn stopping_reason(&self) -> Option<&str> {
        self.stream.stopping_reason()
    }

    /// The next pushed frame, or `Ok(None)` when the server closed the stream.
    /// A close is a documented signal (resync), not an error.
    ///
    /// **The gap check lives below this, not above it.** `EventStream::next`
    /// validates the revision sequence against its own ack before it yields a
    /// batch, so a skipped commit arrives here as
    /// [`RoostError::RevisionGap`] rather than as a batch a caller's fence then
    /// rejects.
    pub async fn next(&mut self) -> Result<Option<EventFrame>, RoostError> {
        Ok(self.stream.next().await?)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::roost::testing::FakeRoost;

    // -----------------------------------------------------------------
    // the request-shape harness
    // -----------------------------------------------------------------

    /// A listener that answers exactly one request and hands back the raw line
    /// the client wrote.
    ///
    /// The vendored `*.request.json` vectors are the **new client → old server**
    /// direction of roost's compatibility matrix, and nothing else in this tree
    /// exercises it: every other test asserts what shed does with a *reply*. What
    /// matters there is not what a struct serializes to in isolation but what
    /// `Conn` actually puts on the wire — roost's request structs are
    /// `deny_unknown_fields`, so one stray omit-when-unset key that turns into
    /// `null` is a session that refuses the op outright.
    struct OneShot {
        dir: PathBuf,
        socket: PathBuf,
        task: JoinHandle<Option<String>>,
    }

    impl OneShot {
        /// Bind, and answer the first request with `result`.
        async fn start(result: serde_json::Value) -> OneShot {
            use std::sync::atomic::AtomicU64;
            use tokio::io::{AsyncBufReadExt as _, AsyncWriteExt as _};

            static NEXT: AtomicU64 = AtomicU64::new(0);
            let dir = std::env::temp_dir().join(format!(
                "shed-roost-capture-{}-{}",
                std::process::id(),
                NEXT.fetch_add(1, std::sync::atomic::Ordering::Relaxed)
            ));
            let _ = std::fs::remove_dir_all(&dir);
            std::fs::create_dir_all(&dir).expect("scratch dir");
            let socket = dir.join("roost.sock");
            let listener = tokio::net::UnixListener::bind(&socket).expect("bind");
            let task = tokio::spawn(async move {
                let (stream, _) = listener.accept().await.ok()?;
                let (read, mut write) = tokio::io::split(stream);
                let line = tokio::io::BufReader::new(read)
                    .lines()
                    .next_line()
                    .await
                    .ok()??;
                let id = serde_json::from_str::<serde_json::Value>(&line)
                    .ok()
                    .and_then(|v| v.get("id").cloned())
                    .unwrap_or(serde_json::Value::Null);
                let reply = serde_json::json!({ "id": id, "ok": true, "result": result });
                let mut bytes = serde_json::to_vec(&reply).ok()?;
                bytes.push(b'\n');
                write.write_all(&bytes).await.ok()?;
                Some(line)
            });
            OneShot { dir, socket, task }
        }

        /// The `params` object of the one request that was written.
        async fn captured_params(self) -> serde_json::Value {
            let line = tokio::time::timeout(std::time::Duration::from_secs(5), self.task)
                .await
                .expect("the request should arrive")
                .expect("the capture task should not panic")
                .expect("a request line");
            let _ = std::fs::remove_dir_all(&self.dir);
            let request: serde_json::Value =
                serde_json::from_str(&line).expect("the request is JSON");
            request["params"].clone()
        }
    }

    /// The `params` object of a vendored request vector.
    fn vector_params(text: &str) -> serde_json::Value {
        serde_json::from_str::<serde_json::Value>(text).expect("a vendored vector is JSON")
            ["params"]
            .clone()
    }

    const VECTOR_TAB_WRITE_REQUEST: &str =
        include_str!("../../../fixtures/roost-vectors/tab.write.request.json");
    const VECTOR_TAB_DUMP_REQUEST: &str =
        include_str!("../../../fixtures/roost-vectors/tab.dump.request.json");
    const VECTOR_EVENTS_SUBSCRIBE_REQUEST: &str =
        include_str!("../../../fixtures/roost-vectors/events.subscribe.request.json");
    const VECTOR_SET_AGENT_HOOKS_REQUEST: &str =
        include_str!("../../../fixtures/roost-vectors/session.set_agent_hooks.request.json");

    /// A `tab.write` goes out as roost's own request, key for key — **and with
    /// no `lease`**, which is what generation 5 took away.
    ///
    /// [`TabWriteParams`] is `deny_unknown_fields` on roost's side, so a key
    /// shed kept sending out of habit would be a session refusing every write
    /// rather than ignoring an extra field.
    #[tokio::test]
    async fn a_tab_write_matches_the_vector_and_carries_no_lease() {
        let expected = vector_params(VECTOR_TAB_WRITE_REQUEST);
        let tab_id: i64 = expected["tab_id"]
            .as_str()
            .expect("a string-int64 tab id")
            .parse()
            .expect("numeric");
        let data = {
            use base64::Engine as _;
            base64::engine::general_purpose::STANDARD
                .decode(expected["data"].as_str().expect("base64 data"))
                .expect("the vector's payload decodes")
        };

        let server = OneShot::start(serde_json::json!({})).await;
        let mut conn = Conn::unix(&server.socket).await.expect("dial");
        conn.tab_write(tab_id, &data).await.expect("write");
        drop(conn);

        let sent = server.captured_params().await;
        assert!(
            sent.get("lease").is_none(),
            "the lease retired at generation 5; the key must not be on the wire: {sent}"
        );
        assert_eq!(sent, expected, "the vendored tab.write request vector");
    }

    /// **The op shed calls on every single watcher cycle**, pinned against
    /// roost's own request vector.
    ///
    /// A response fixture cannot prove what shed *sends*, and this is the one
    /// place the retired lease key would survive unnoticed: a stray `lease` here
    /// is a watcher that cannot subscribe to anything at all, on every host, for
    /// as long as nobody looks.
    #[tokio::test]
    async fn a_fresh_events_subscribe_matches_the_vendored_request() {
        let expected = vector_params(VECTOR_EVENTS_SUBSCRIBE_REQUEST);
        let server = OneShot::start(serde_json::json!({
            "revision": 42, "session_id": "01K3S8TQ4F0Q9YB2K6WZ5D7XN"
        }))
        .await;
        let conn = Conn::unix(&server.socket).await.expect("dial");
        let stream = conn.subscribe().await.expect("subscribe");
        drop(stream);

        let sent = server.captured_params().await;
        assert!(
            sent.get("lease").is_none(),
            "a fresh subscribe carries no lease at generation 5: {sent}"
        );
        assert!(
            sent.get("from_revision").is_none() && sent.get("session_id").is_none(),
            "a FRESH subscribe is not a resume — neither resume key is emitted: {sent}"
        );
        assert_eq!(
            sent, expected,
            "the vendored events.subscribe request vector"
        );
    }

    /// **`scrollback: 0` and "no `scrollback` key" are different statements**,
    /// and only the vector tells them apart: roost declares the field
    /// `skip_serializing_if = "is_zero"`, so a struct literal proves nothing
    /// about the bytes. A session that predates the field is
    /// `deny_unknown_fields`, so emitting `"scrollback": 0` would be a peek that
    /// fails outright against an older host rather than reading its viewport.
    #[tokio::test]
    async fn a_tab_dump_matches_the_vector_and_omits_scrollback() {
        let expected = vector_params(VECTOR_TAB_DUMP_REQUEST);
        let tab_id: i64 = expected["tab_id"]
            .as_str()
            .expect("a string-int64 tab id")
            .parse()
            .expect("numeric");

        let server = OneShot::start(serde_json::json!({
            "rows": 0, "cols": 80, "rows_text": [], "cursor": null
        }))
        .await;
        let mut conn = Conn::unix(&server.socket).await.expect("dial");
        conn.tab_dump(tab_id).await.expect("dump");
        drop(conn);

        let sent = server.captured_params().await;
        assert!(
            sent.get("scrollback").is_none(),
            "a viewport read emits no scrollback key at all: {sent}"
        );
        assert_eq!(sent, expected, "the vendored tab.dump request vector");
    }

    /// `session.set_agent_hooks` goes out as roost's own request, key for key.
    ///
    /// `deny_unknown_fields` on roost's side means an extra key is a refusal and
    /// a missing `client` is a decode failure, so the vendored request vector is
    /// the contract: this is the op that makes the host edit dotfiles, and a
    /// request shed cannot get right is a payoff shed never delivers.
    #[tokio::test]
    async fn session_set_agent_hooks_matches_the_vendored_request() {
        let expected = vector_params(VECTOR_SET_AGENT_HOOKS_REQUEST);
        let skip: Vec<String> = expected["skip"]
            .as_array()
            .expect("the vector's skip list")
            .iter()
            .map(|value| value.as_str().expect("a name").to_string())
            .collect();

        let server = OneShot::start(serde_json::json!({
            "wired": [], "refreshed": [], "removed": [], "skipped": [], "errors": []
        }))
        .await;
        let mut conn = Conn::unix(&server.socket).await.expect("dial");
        conn.session_set_agent_hooks(
            AgentHooksMode::Auto,
            &skip,
            expected["client"].as_str().expect("the vector's client"),
        )
        .await
        .expect("set_agent_hooks");
        drop(conn);

        let sent = server.captured_params().await;
        assert!(
            sent.get("lease").is_none(),
            "the op is open to every same-UID client at generation 5; no lease is sent: {sent}"
        );
        assert_eq!(sent, expected);
    }

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

    /// **A write needs nothing but the connection** at session protocol 5 — the
    /// generation-4 lease that made `tab_write` a two-op dialogue is gone, and
    /// two connections writing to the same tab both land.
    ///
    /// This is the shape the deleted `connect-required` / `taken-over` table was
    /// standing in front of, and it is worth an assertion rather than an
    /// absence: "shed no longer takes a lease" and "a second client can still
    /// write" are different claims, and only the second one is the feature.
    #[tokio::test]
    async fn two_connections_both_write_without_taking_anything() {
        let fake = FakeRoost::start().await;
        let mut first = Conn::unix(fake.socket_path()).await.expect("dial");
        let mut second = Conn::unix(fake.socket_path()).await.expect("dial");
        first.tab_write(5, b"ok").await.expect("the first write");
        second.tab_write(5, b"ok").await.expect("the second write");
        assert_eq!(fake.written(5), b"okok");
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

    /// A refusal the session minted keeps its typed code all the way out — the
    /// discrimination `is_transport_error` upstream is built on is "the wire is
    /// fine and the *request* was wrong", and it has to survive the trip.
    ///
    /// `unknown-op` is the surviving witness for that: generation 5 deleted
    /// `already-connected`, `connect-required` and `taken-over` along with the
    /// lease, and every roost socket answers this one.
    #[tokio::test]
    async fn a_server_refusal_keeps_its_typed_code() {
        let fake = FakeRoost::start().await;
        let mut conn = Conn::unix(fake.socket_path()).await.expect("dial");
        match conn.call_raw("no.such.op", serde_json::json!({})).await {
            Err(err @ RoostError::Server { .. }) => {
                assert_eq!(err.server_code(), Some(ServerCode::UnknownOp));
            }
            other => panic!("expected a server refusal, got {other:?}"),
        }
    }

    /// The gate refuses the generation shed's own build predates, by name and
    /// with both numbers — a protocol-2 daemon is the live case on the network
    /// today (an un-upgraded machine), so it gets its own lane beside the
    /// synthetic `+7`.
    #[tokio::test]
    async fn a_protocol_two_daemon_is_refused_by_name() {
        let fake = FakeRoost::start().await;
        fake.set_session_protocol(2);
        let mut conn = Conn::unix(fake.socket_path()).await.expect("dial");
        match conn.session_identify().await {
            Err(RoostError::ProtocolMismatch { theirs, ours }) => {
                assert_eq!(theirs, 2);
                assert_eq!(ours, SESSION_PROTOCOL_VERSION);
                assert_eq!(ours, 5, "this build speaks roost's post-lease generation");
            }
            other => panic!("expected ProtocolMismatch, got {other:?}"),
        }
    }

    /// **The newly interesting number.** Every host plan 019's desktop
    /// bootstrapped is running a protocol-**4** `roost-session`, so 4 is no
    /// longer history — it is the refusal case a real user meets the day this
    /// build ships, and it has to come out named rather than as a limp.
    ///
    /// The `4` in this test is a negative-test datum on purpose and does not
    /// move with the generation.
    #[tokio::test]
    async fn a_protocol_four_daemon_is_refused_by_name() {
        let fake = FakeRoost::start().await;
        fake.set_session_protocol(4);
        let mut conn = Conn::unix(fake.socket_path()).await.expect("dial");
        match conn.session_identify().await {
            Err(RoostError::ProtocolMismatch { theirs, ours }) => {
                assert_eq!(
                    theirs, 4,
                    "what plan 019 installed on every host it touched"
                );
                assert_eq!(ours, SESSION_PROTOCOL_VERSION);
            }
            other => panic!("expected ProtocolMismatch, got {other:?}"),
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

        // Same for a port with nothing behind it. Bind an ephemeral port,
        // take its number, then drop the listener before dialing it — unlike a
        // fixed port number, which could have something bound to it depending on
        // the host.
        //
        // **Re-rolled rather than asserted once.** A released ephemeral port is
        // free for the OS to hand straight back out, and this binary's other
        // tests bind plenty of them (every `FakeRoost` takes one); losing that
        // race means something really is listening there, which says nothing
        // about the code under test. A handful of rolls all finding a listener
        // is not a race, so that still fails.
        let mut refused = false;
        for _ in 0..8 {
            let port = {
                let listener = tokio::net::TcpListener::bind("127.0.0.1:0")
                    .await
                    .expect("bind ephemeral port");
                listener.local_addr().expect("local_addr").port()
            };
            match Conn::tcp_loopback(port).await {
                Err(err @ RoostError::Unavailable(_)) => {
                    assert!(
                        err.to_string().contains(&format!("127.0.0.1:{port}")),
                        "{err}"
                    );
                    refused = true;
                    break;
                }
                Err(other) => panic!("expected Unavailable, got {other:?}"),
                // Somebody else took the port back between the drop and the
                // dial. Roll again.
                Ok(_) => continue,
            }
        }
        assert!(
            refused,
            "dialing a port with nothing behind it must not succeed"
        );
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
