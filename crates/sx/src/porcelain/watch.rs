//! `sx watch <slug> [--on <target>]` — a v1-minimal LINE STREAM of a session's
//! activity (plan 009 §4: not a TUI; steering verbs are follow-up work).
//!
//! ## Three transports, one renderer
//!
//! | target | feed |
//! |---|---|
//! | `local` | the hub on `127.0.0.1:1029` directly |
//! | `machine:<m>` | `ssh -N -L <ephemeral>:127.0.0.1:1029 <m>`, then the same client |
//! | `shed:<s>` | the server's aggregate `GET /api/rc/events` + the `/messages` proxy |
//!
//! ## The contract-v2 client rules (`docs/extensions/rc-helper.md`)
//!
//! Transport selection reads CAPABILITIES first, never a bare error:
//!
//! - the `list` envelope's capability block decides whether the kind has a
//!   message feed at all (`kind_features[kind].feed`, with the deprecated
//!   `watch` bit as the documented absent-field fallback);
//! - no capability block, or no feed for that kind, → **probe polling** with a
//!   note saying so, rather than opening a stream that will never carry a row;
//! - `RC_HUB_UNAVAILABLE` (the server's 503 when a shed's hub isn't up) and an
//!   unreachable local/machine hub degrade to the same probe polling — a
//!   missing hub is a degraded feed, not a failed command. So does a hub that
//!   stops answering MID-stream, including on the message-body fetch behind a
//!   `message.appended`: one note, then polling.
//!
//! The shed transport is always opened against the server the shed was resolved
//! ON (`porcelain::pin_shed_server`), never `default_server` — the aggregate
//! stream is per-host, so the wrong server yields a feed whose every frame the
//! shed filter drops.

use std::time::Duration;

use shed_core::rc::{RcCapabilities, RcMessagesPage, RcSessionDto, RcSessionListDto, RcState};
use shed_core::rc_events::RcEvent;

use crate::args::Parsed;
use crate::cli::Deps;
use crate::porcelain::hub::{HubClient, HubError, HUB_PORT};
use crate::porcelain::{
    load_config, remote_exec, remote_prefix, resolve_target, VerbError, VerbResult,
};
use crate::ssh;
use crate::target::Resolved;

/// How often probe polling re-reads the session when there is no feed.
const POLL_EVERY: Duration = Duration::from_secs(2);
/// How long to wait for an `ssh -L` tunnel's local end to answer.
const TUNNEL_READY_TIMEOUT: Duration = Duration::from_secs(10);

/// Which feed `sx watch` should open.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Transport {
    /// The hub carries a message feed for this kind — stream it.
    Feed,
    /// No feed available; poll `probe` and print state transitions. The string
    /// is the reason, printed once as a note so the degradation is never silent.
    ProbePolling(String),
}

/// **The contract-v2 transport decision**, as a pure function of what the `list`
/// envelope said (see the module doc's rules).
pub fn select_transport(kind: &str, caps: Option<&RcCapabilities>) -> Transport {
    let Some(caps) = caps else {
        return Transport::ProbePolling(
            "the rc binary reported no capabilities (it predates capability discovery)".to_string(),
        );
    };
    if !caps.has_feature("messages") {
        return Transport::ProbePolling(
            "this rc binary advertises no `messages` feature".to_string(),
        );
    }
    match caps.kind_features.get(kind) {
        // `feed_messages()` already applies the documented fallback: absent
        // `feed` → the deprecated `watch` bit.
        Some(features) if features.feed_messages() => Transport::Feed,
        Some(_) => Transport::ProbePolling(format!("kind {kind} has no message feed")),
        None => {
            Transport::ProbePolling(format!("kind {kind} advertises no feed/steer affordances"))
        }
    }
}

pub fn run(deps: &Deps, slug: &str, p: &Parsed) -> VerbResult {
    let resolved = resolve_target(deps, p)?;
    // ONE one-shot `list` gives both the session (does the slug exist?) and the
    // capabilities the transport decision needs — no extra probe round-trip.
    let envelope = super::ls::envelope_for(deps, &resolved)?;
    let session = find_session(&envelope, slug)?;
    let transport = select_transport(session.kind.as_str(), envelope.capabilities.as_ref());

    deps.write_out(&format!(
        "watching {} ({}) on {} — state {}\n",
        session.slug,
        session.kind.as_str(),
        resolved.display(),
        session.state.as_str()
    ));

    match transport {
        Transport::ProbePolling(reason) => degrade(deps, &resolved, slug, session.state, &reason),
        Transport::Feed => match &resolved {
            Resolved::Local => stream_hub(deps, &resolved, slug, HUB_PORT, None),
            Resolved::Machine(_) => stream_machine(deps, &resolved, slug),
            Resolved::Shed { name, server } => stream_shed(
                deps,
                &resolved,
                name,
                server.as_deref(),
                slug,
                session.state,
            ),
        },
    }
}

fn find_session<'a>(
    envelope: &'a RcSessionListDto,
    slug: &str,
) -> Result<&'a RcSessionDto, VerbError> {
    envelope
        .rc_sessions
        .iter()
        .find(|s| s.slug == slug)
        .ok_or_else(|| VerbError {
            message: format!("rc session {slug} not found"),
            // The engine's not-found class, so a script reads the same 4 it
            // would get from `sx rc probe --slug <gone>`.
            code: 4,
        })
}

/// Drain decoded events onto stdout until the stream ends: one line per rendered
/// event, plus the bodies behind a `message.appended` — which is
/// notification-only, so the body is a targeted fetch (the reason this consumer
/// awaits, and the reason the transports feed it through a channel rather than a
/// callback).
///
/// Both transports converge here. They differ only in which events reach the
/// renderer (`keep`) and where a message page comes from (`fetch`).
///
/// **A failed fetch ENDS the loop** with the reason, rather than being swallowed.
/// The 503 the shed proxy returns when a guest's hub is down (`RC_HUB_UNAVAILABLE`)
/// arrives here, and the module doc promises it degrades to probe polling; a
/// dropped `Err` instead left `sx watch` printing bare notification-free
/// activity forever. Returning also bounds the noise: the caller's `degrade`
/// prints ONE note and switches transport, so a persistently-broken proxy can
/// never spam a line per event.
pub(crate) async fn consume_events<Fut>(
    deps: &Deps<'_>,
    rx: &mut tokio::sync::mpsc::UnboundedReceiver<RcEvent>,
    slug: &str,
    keep: impl Fn(&RcEvent) -> bool,
    fetch: impl Fn(u64) -> Fut,
) -> Result<(), String>
where
    Fut: std::future::Future<Output = Result<RcMessagesPage, String>>,
{
    while let Some(event) = rx.recv().await {
        if !keep(&event) {
            continue;
        }
        for line in render_event(&event, slug) {
            deps.write_out(&format!("{line}\n"));
        }
        if let RcEvent::MessageAppended { seq, .. } = &event {
            if event_slug(&event) != slug {
                continue;
            }
            let page = fetch(seq.saturating_sub(1)).await?;
            for message in &page.messages {
                deps.write_out(&format!("{}\n", render_message(message)));
            }
        }
    }
    Ok(())
}

/// **The shed transport's event filter.** The aggregate stream carries every shed
/// on the host, so frames are narrowed to this shed — and then to this slug,
/// EXCEPT for the shed-scoped synthetic events, whose slug is `""` by
/// construction.
///
/// Letting the slug test drop those was the whole bug: `HubUnavailable` and
/// `ShedStopped` are produced by NO other transport, so the one feed that can
/// report "the activity you are reading is stale" silently discarded it, and a
/// hub drop or a stopped shed read as a healthy idle stream.
pub fn shed_event_keep(event: &RcEvent, shed: &str, slug: &str) -> bool {
    if event.shed() != shed {
        return false;
    }
    let event_slug = event_slug(event);
    event_slug.is_empty() || event_slug == slug
}

// ---------------------------------------------------------------------------
// the hub transports (local + machine)
// ---------------------------------------------------------------------------

/// Stream a hub reachable on `127.0.0.1:<port>`. `_tunnel` (when present) is the
/// `ssh -L` child held alive for the duration of this call; dropping it kills the
/// forward.
fn stream_hub(
    deps: &Deps,
    resolved: &Resolved,
    slug: &str,
    port: u16,
    _tunnel: Option<Child>,
) -> VerbResult {
    let client = match HubClient::loopback(port).and_then(|c| {
        deps.block_on(async {
            c.health().await?;
            Ok(c)
        })
    }) {
        Ok(client) => client,
        Err(err) => {
            return degrade(
                deps,
                resolved,
                slug,
                RcState::Starting,
                &format!("no rc activity hub reachable ({err})"),
            )
        }
    };
    // The hub's snapshot is the ENRICHED one (it overlays the activity dimension
    // the one-shot `list` never carries), so re-reading it here is not redundant
    // with the envelope that chose this transport — it is the opening frame.
    if let Ok(sessions) = deps.block_on(client.sessions()) {
        if let Some(session) = sessions.iter().find(|s| s.slug == slug) {
            deps.write_out(&format!(
                "{slug}: state {}{}\n",
                session.state.as_str(),
                session
                    .activity
                    .map(|a| format!(" activity {}", a.as_str()))
                    .unwrap_or_default()
            ));
        }
    }
    let owned_slug = slug.to_string();
    let outcome: Result<i32, VerbError> = deps.block_on(async {
        let (tx, mut rx) = tokio::sync::mpsc::unbounded_channel();
        let stream = client.events(&tx);
        let (client, slug) = (&client, owned_slug.as_str());
        // The hub's stream carries only this hub's sessions, so nothing is filtered
        // out here — `render_event` narrows to the slug on its own.
        let consume = consume_events(
            deps,
            &mut rx,
            slug,
            |_| true,
            move |since| async move {
                client
                    .messages(slug, since)
                    .await
                    .map_err(|e: HubError| format!("fetching the message body: {e}"))
            },
        );
        tokio::select! {
            result = stream => result.map_err(|e: HubError| VerbError::failed(e.to_string()))?,
            outcome = consume => outcome.map_err(VerbError::failed)?,
        }
        Ok(0)
    });
    match outcome {
        Ok(code) => Ok(code),
        // A hub that stops answering mid-stream is the same DEGRADED feed as one
        // that never answered — one note, then probe polling. (Settled out here,
        // outside `block_on`: `poll` is synchronous and must never run inside the
        // runtime.)
        Err(err) => degrade(deps, resolved, slug, RcState::Starting, &err.message),
    }
}

/// Open an `ssh -L` tunnel to a machine's hub, then stream through it.
fn stream_machine(deps: &Deps, resolved: &Resolved, slug: &str) -> VerbResult {
    let Resolved::Machine(entry) = resolved else {
        return Err(VerbError::failed("internal: not a machine target"));
    };
    // The port is grabbed the same way the engine allocates opencode's: bind
    // :0, read the assignment, release. Racy in principle; `ExitOnForwardFailure`
    // turns a lost race into an immediate, visible tunnel failure.
    let port = shed_app::rc_engine::free_loopback_port()
        .map_err(|e| VerbError::failed(format!("allocating a local forward port: {e}")))?;
    let argv = ssh::machine_forward_argv(entry, port, HUB_PORT);
    let (bin, rest) = argv.split_first().expect("ssh argv is never empty");
    let child = std::process::Command::new(bin)
        .args(rest)
        .stdin(std::process::Stdio::null())
        .stdout(std::process::Stdio::null())
        .spawn()
        .map_err(|e| VerbError::failed(format!("opening the hub tunnel: {e}")))?;
    let mut tunnel = Child(child);

    // Deadline-poll the local end rather than sleeping a fixed amount — AND watch
    // the ssh child, because the two common failures are instant: a taken local
    // port (`ExitOnForwardFailure`) and an unreachable/refused host both exit ssh
    // in well under a second. Waiting out the full timeout for a process that is
    // already gone is 10s of a stopped terminal for no information.
    let deadline = std::time::Instant::now() + TUNNEL_READY_TIMEOUT;
    while std::net::TcpStream::connect(("127.0.0.1", port)).is_err() {
        let reason = match tunnel.exited() {
            Some(status) => format!(
                "the hub tunnel to {} exited immediately ({status})",
                resolved.display()
            ),
            None if std::time::Instant::now() >= deadline => {
                format!("the hub tunnel to {} did not come up", resolved.display())
            }
            None => {
                std::thread::sleep(Duration::from_millis(100));
                continue;
            }
        };
        return degrade(deps, resolved, slug, RcState::Starting, &reason);
    }
    stream_hub(deps, resolved, slug, port, Some(tunnel))
}

/// An owned child process that is killed and reaped when dropped — so a Ctrl-C
/// or an early return can never leave an `ssh -L` forward running.
struct Child(std::process::Child);

impl Child {
    /// The child's exit status if it has ALREADY exited, without blocking. A
    /// probe error (the child was reaped elsewhere) counts as exited: either way
    /// there is no tunnel to wait for.
    fn exited(&mut self) -> Option<String> {
        match self.0.try_wait() {
            Ok(Some(status)) => Some(status.to_string()),
            Ok(None) => None,
            Err(err) => Some(err.to_string()),
        }
    }
}

impl Drop for Child {
    fn drop(&mut self) {
        let _ = self.0.kill();
        let _ = self.0.wait();
    }
}

// ---------------------------------------------------------------------------
// the shed transport (the server's aggregate stream + the messages proxy)
// ---------------------------------------------------------------------------

/// Stream a shed's activity off its server's aggregate `GET /api/rc/events`.
///
/// `server` is `Some` for every real caller — [`super::pin_shed_server`] resolves
/// an unqualified `shed:<name>` to the server it was found on before dispatch, so
/// the stream and the shed can never disagree about the host. The `Option`
/// survives only because [`Resolved::Shed`] models the qualifier as one; a `None`
/// here would fall back to `default_server` and silently stream the wrong host.
fn stream_shed(
    deps: &Deps,
    resolved: &Resolved,
    shed: &str,
    server: Option<&str>,
    slug: &str,
    state: RcState,
) -> VerbResult {
    let client = match server_client(deps, server) {
        Ok(client) => client,
        Err(err) => return degrade(deps, resolved, slug, state, &err.message),
    };
    let (shed, slug) = (shed.to_string(), slug.to_string());
    let outcome: Result<i32, VerbError> = deps.block_on(async {
        let (tx, mut rx) = tokio::sync::mpsc::unbounded_channel();
        struct ChannelSink(tokio::sync::mpsc::UnboundedSender<RcEvent>);
        impl shed_core::http::RcEventSink for ChannelSink {
            fn on_event(&self, ev: RcEvent) {
                let _ = self.0.send(ev);
            }
        }
        let sink = ChannelSink(tx);
        let stream = client.rc_events(&sink);
        let (client, shed, slug) = (&client, shed.as_str(), slug.as_str());
        let consume = consume_events(
            deps,
            &mut rx,
            slug,
            |event| shed_event_keep(event, shed, slug),
            move |since| async move {
                client
                    .rc_messages(shed, slug, since, None)
                    .await
                    // The proxy's `RC_HUB_UNAVAILABLE` 503 lands here.
                    .map_err(|e| format!("the message feed stopped answering ({e})"))
            },
        );
        tokio::select! {
            result = stream => {
                result.map_err(|e| VerbError::failed(format!("rc events: {e}")))?;
            }
            outcome = consume => outcome.map_err(VerbError::failed)?,
        }
        Ok(0)
    });
    match outcome {
        Ok(code) => Ok(code),
        // A hub that isn't up on the guest is a DEGRADED feed (the proxy's 503
        // RC_HUB_UNAVAILABLE), not a failed command.
        Err(err) => degrade(deps, resolved, &slug, state, &err.message),
    }
}

/// A plain (static-token) shed-server client for one server entry.
///
/// Deliberately NOT on the host-agent minter path that [`crate::backend`] wires
/// for the one-shot fan-out, and the difference is the LIFETIME: this client
/// holds an SSE stream open until the user stops watching, and the agent's
/// credential socket is single-consumer/last-writer-wins — minting here would
/// keep a running desktop app superseded for as long as `sx watch` runs, to buy
/// a feed. An mTLS server (no static token) therefore 401s here and `sx watch`
/// does what it already does for any unreachable feed: degrades to probe polling
/// over SSH, with a note saying why.
fn server_client(deps: &Deps, server: Option<&str>) -> Result<shed_core::http::Client, VerbError> {
    let config = load_config(deps);
    let entry = select_server(&config, server)?;
    let resolved = entry.resolved_endpoint();
    let pin = (!resolved.pin.is_empty()).then_some(resolved.pin);
    shed_core::http::Client::new(
        resolved.base_url,
        entry.name.clone(),
        entry.control_token.clone(),
        pin,
        None,
    )
    .map_err(|e| VerbError::failed(e.to_string()))
}

/// **Which server entry the aggregate stream is opened against.** Pure, so the
/// choice is unit-testable without a client.
///
/// A NAMED server is looked up by name and nothing else — the default-server
/// fallback applies only when the caller genuinely has no opinion. That matters
/// because `--on shed:<name>` (no `@server`) reaches here with the server the
/// fan-out FOUND the shed on ([`super::pin_shed_server`]): silently substituting
/// `default_server` there streamed a different host's events, whose frames the
/// shed filter then dropped in their entirety — a watch that hangs, forever,
/// looking exactly like an idle session.
pub fn select_server<'a>(
    config: &'a shed_core::config::ShedConfig,
    server: Option<&str>,
) -> Result<&'a shed_core::config::ShedServerEntry, VerbError> {
    match server {
        Some(name) => config.servers.iter().find(|s| s.name == name),
        None => config
            .default_server
            .as_deref()
            .and_then(|d| config.servers.iter().find(|s| s.name == d))
            .or_else(|| config.servers.first()),
    }
    .ok_or_else(|| {
        VerbError::bad_args(format!(
            "no configured server{}",
            server.map(|s| format!(" {s:?}")).unwrap_or_default()
        ))
    })
}

// ---------------------------------------------------------------------------
// probe polling (the universal fallback) + rendering
// ---------------------------------------------------------------------------

/// Say why the feed is degraded, then fall back to probe polling. EVERY "no
/// feed" path lands here — a capability that ruled a stream out, an unreachable
/// hub, a tunnel that never came up, a server's `RC_HUB_UNAVAILABLE` — so the
/// note reads the same wherever it came from and the degradation is never silent.
fn degrade(
    deps: &Deps,
    resolved: &Resolved,
    slug: &str,
    state: RcState,
    reason: &str,
) -> VerbResult {
    deps.write_out(&format!("note: {reason}; polling every {POLL_EVERY:?}\n"));
    poll(deps, resolved, slug, state)
}

/// Re-read the session on a fixed cadence and print every transition. The
/// fallback for every "no feed" reason; ends when the session dies or is gone.
///
/// Deliberately synchronous (a blocking `thread::sleep`, `remote_exec`'s own
/// `block_on` per probe): it must never run INSIDE the runtime, which is why the
/// streaming transports settle their degrade decision before entering one.
fn poll(deps: &Deps, resolved: &Resolved, slug: &str, mut last: RcState) -> VerbResult {
    loop {
        std::thread::sleep(POLL_EVERY);
        let session = match probe_once(deps, resolved, slug) {
            Ok(session) => session,
            Err(err) if err.code == 4 => {
                deps.write_out(&format!("{slug}: gone\n"));
                return Ok(0);
            }
            Err(err) => return Err(err),
        };
        if session.state != last {
            last = session.state;
            deps.write_out(&format!("{slug}: state {}\n", session.state.as_str()));
        }
        if session.state == RcState::Dead {
            return Ok(0);
        }
    }
}

fn probe_once(deps: &Deps, resolved: &Resolved, slug: &str) -> Result<RcSessionDto, VerbError> {
    match resolved {
        Resolved::Local => Ok(deps.engine(false).probe(slug, None)?),
        remote => {
            let prefix = remote_prefix(deps, remote)?;
            let argv = prefix.splice(shed_core::rc::probe_argv(prefix.bin(), slug));
            let stdout = remote_exec(deps, remote, &argv, None)?;
            shed_core::rc::decode_session(&stdout).map_err(|e| VerbError::failed(e.to_string()))
        }
    }
}

/// The slug an event pertains to (`""` for the shed-scoped synthetic events).
fn event_slug(event: &RcEvent) -> &str {
    match event {
        RcEvent::ActivityChanged { slug, .. }
        | RcEvent::SessionUpdated { slug, .. }
        | RcEvent::MessageAppended { slug, .. } => slug,
        RcEvent::HubUnavailable { .. } | RcEvent::ShedStopped { .. } => "",
    }
}

/// One event → zero or more printable lines, filtered to `slug`. Pure, so the
/// line shapes are unit-tested without a stream.
pub fn render_event(event: &RcEvent, slug: &str) -> Vec<String> {
    match event {
        RcEvent::HubUnavailable { shed } => {
            vec![format!("! hub unavailable for {shed} — activity is stale")]
        }
        RcEvent::ShedStopped { shed } => vec![format!("! shed {shed} stopped")],
        _ if event_slug(event) != slug => Vec::new(),
        RcEvent::ActivityChanged {
            activity,
            state,
            last_message,
            ..
        } => {
            let mut line = format!("{slug}:");
            if let Some(state) = state {
                line.push_str(&format!(" state {}", state.as_str()));
            }
            if let Some(activity) = activity {
                line.push_str(&format!(" activity {}", activity.as_str()));
            }
            if let Some(message) = last_message.as_deref().filter(|m| !m.is_empty()) {
                line.push_str(&format!(" — {message}"));
            }
            vec![line]
        }
        RcEvent::SessionUpdated { removed, state, .. } => {
            if *removed {
                vec![format!("{slug}: gone")]
            } else {
                vec![format!(
                    "{slug}: state {}",
                    state.map(|s| s.as_str()).unwrap_or("?")
                )]
            }
        }
        // The notification itself prints nothing — the fetched body does.
        RcEvent::MessageAppended { .. } => Vec::new(),
    }
}

/// One feed message → one line.
pub fn render_message(message: &shed_core::rc::RcFeedMessage) -> String {
    let mut line = format!("[{}] {}", message.seq, message.role);
    if !message.msg_type.is_empty() && message.msg_type != message.role {
        line.push_str(&format!("/{}", message.msg_type));
    }
    if let Some(tool) = message.tool.as_ref().and_then(|t| t.name.as_deref()) {
        line.push_str(&format!(" {tool}"));
    }
    if let Some(text) = message.text.as_deref().filter(|t| !t.is_empty()) {
        line.push_str(&format!(": {text}"));
    }
    if let Some(approval) = message.approval_request() {
        line.push_str(&format!(" [approval {} {}]", approval.id, approval.status));
    }
    line
}
