//! Credential sources (plan 035 §3.4, plan 040 §3.9): who answers credential requests
//! for this user on each configured server (this machine, another machine, the account
//! service, or nobody), "Use this machine" and "Stop answering from here" — the
//! desktop's half of the pin. Shared by the `source.status` / `sources.list` /
//! `source.use` / `source.unbind` IPC ops and the app's commands of the same names
//! (`sources_list`, `source_use`, `source_unbind`), so the two doors cannot disagree.
//!
//! The servers are the shed config's as it reads NOW ([`CurrentServers`]), as the source
//! runtime reloads them: a server added while the app runs is listed and can be acted
//! on, and a changed endpoint is asked at its new address. The calls go out on an
//! ordinary control-scope client per server (the credential the app uses for shed
//! lifecycle calls; the startup Backend's where it has one for that server at that
//! endpoint), never the source's credentials-scope certificate. The id a move sends is THIS machine's: the identity
//! the source runtime ([`DeliveryRuntime`]) loaded at start from `shed-source`
//! (`<state>/broker/machine.json`), or, before it started, the one that file holds.
//!
//! Every machine name or id here came from other machines by way of the server and is
//! already cleaned for display by `shed_app::HostSource`; this machine's own name is
//! cleaned the same way before it leaves.

use serde_json::{json, Value};

use std::path::{Path, PathBuf};
use std::sync::Arc;

use shed_app::{source_error_class, source_error_words, Backend, HostSource};
use shed_core::config::{ShedConfig, ShedServerEntry};
use shed_core::http::{Client, SourceError, SOURCE_MACHINE_NOT_FOUND, SOURCE_MOVED};
use shed_core::models::{display_source_text, SOURCE_IS_SERVICE, SOURCE_TEXT_MAX_BYTES};
use shed_desktop_core::delivery::source::machine::{self, MachineIdentity};
use shed_desktop_core::delivery::DeliveryRuntime;

/// Makes the app's control-scope clients for a parsed shed config, as the startup
/// Backend was made (test mode, the minter and the auth modes captured).
pub type BuildClients = Arc<dyn Fn(&ShedConfig) -> Backend + Send + Sync>;

/// The shed config's servers as they read now, each with a control-scope client, managed
/// as app state so every door into the sources reads the same servers. A client is
/// kept, with its credentials in hand, only while the server's WHOLE entry is unchanged
/// (endpoint, ports, TLS pin, auth mode, certificate, user, identity file); any change
/// makes a new one. When the config cannot be read, the servers last read stay, marked.
pub struct CurrentServers {
    config_path: PathBuf,
    build: BuildClients,
    known: std::sync::Mutex<Vec<(ShedServerEntry, Client)>>,
}

/// The servers as last resolved, and whether the config could be read this time
/// (`config_unreadable`: these are the servers it last listed).
#[derive(Clone)]
pub struct Servers {
    pub clients: Vec<(String, Client)>,
    pub config_unreadable: bool,
}

/// The shed config at `path`: a missing file is no servers, one that cannot be read
/// (or that the reader refuses whole) is `None` — the source runtime's own rule
/// (`shed_source::discovery::load_servers`).
fn read_config(path: &Path) -> Option<ShedConfig> {
    if path.as_os_str().is_empty() {
        return Some(ShedConfig::default());
    }
    match std::fs::read_to_string(path) {
        Ok(text) => Some(ShedConfig::parse(&text)).filter(|c| c.refused.is_none()),
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => Some(ShedConfig::default()),
        Err(_) => None,
    }
}

impl CurrentServers {
    /// Seeded with the startup Backend's clients and the config they were made from.
    pub fn new(
        config_path: PathBuf,
        build: BuildClients,
        startup: &Backend,
        startup_config: &ShedConfig,
    ) -> CurrentServers {
        let known = startup
            .host_clients()
            .into_iter()
            .filter_map(|(name, c)| {
                startup_config
                    .servers
                    .iter()
                    .find(|e| e.name == name)
                    .map(|e| (e.clone(), c))
            })
            .collect();
        CurrentServers {
            config_path,
            build,
            known: std::sync::Mutex::new(known),
        }
    }

    /// Reads the config now (blocking file I/O).
    pub fn read(&self) -> Servers {
        let mut known = self.known.lock().unwrap_or_else(|e| e.into_inner());
        let Some(config) = read_config(&self.config_path) else {
            return Servers {
                clients: known
                    .iter()
                    .map(|(e, c)| (e.name.clone(), c.clone()))
                    .collect(),
                config_unreadable: true,
            };
        };
        let mut next = Vec::new();
        for (name, fresh) in (self.build)(&config).host_clients() {
            let Some(entry) = config.servers.iter().find(|e| e.name == name) else {
                continue;
            };
            let client = known
                .iter()
                .find(|(e, _)| e == entry)
                .map(|(_, c)| c.clone())
                .unwrap_or(fresh);
            next.push((entry.clone(), client));
        }
        *known = next;
        Servers {
            clients: known
                .iter()
                .map(|(e, c)| (e.name.clone(), c.clone()))
                .collect(),
            config_unreadable: false,
        }
    }
}

/// How the source doors resolve the servers: from [`CurrentServers`], or, before setup
/// finished, the startup Backend's.
#[derive(Clone)]
pub struct Resolver {
    current: Option<Arc<CurrentServers>>,
    startup: Arc<Backend>,
}

impl Resolver {
    pub fn new(current: Option<Arc<CurrentServers>>, startup: Arc<Backend>) -> Resolver {
        Resolver { current, startup }
    }

    /// The servers the shed config lists now.
    pub async fn now(&self) -> Servers {
        let fallback = || Servers {
            clients: self.startup.host_clients(),
            config_unreadable: false,
        };
        match &self.current {
            None => fallback(),
            Some(c) => {
                let c = c.clone();
                tokio::task::spawn_blocking(move || c.read())
                    .await
                    .unwrap_or_else(|_| fallback())
            }
        }
    }

    /// `host`'s client among the servers listed now: refused `config_unreadable` while
    /// the config cannot be read (the safer choice: an action is never sent to a server
    /// the config may no longer name, or at an endpoint it may have changed), and
    /// `bad_request` for a server it does not list.
    pub async fn client(&self, host: &str) -> Result<Client, (String, String)> {
        let servers = self.now().await;
        if servers.config_unreadable {
            return Err(err(
                "config_unreadable",
                "the shed config cannot be read; fix it, then try again",
            ));
        }
        client_of(&servers.clients, host)
            .cloned()
            .ok_or_else(|| err("bad_request", format!("unknown server: {host:?}")))
    }
}

fn client_of<'a>(servers: &'a [(String, Client)], host: &str) -> Option<&'a Client> {
    servers.iter().find(|(n, _)| n == host).map(|(_, c)| c)
}

/// Each server's row (`GET /api/sources` on each, concurrently, in the servers' order),
/// as seen from the machine whose id is `me`.
async fn rows_of(servers: &[(String, Client)], me: Option<&str>) -> Vec<HostSource> {
    let tasks: Vec<_> = servers
        .iter()
        .map(|(_, c)| {
            let c = c.clone();
            tokio::spawn(async move { c.source_status().await })
        })
        .collect();
    let mut rows = Vec::with_capacity(tasks.len());
    for ((host, _), t) in servers.iter().zip(tasks) {
        rows.push(match t.await {
            Ok(Ok(doc)) => HostSource::from_document(host.clone(), &doc, me),
            Ok(Err(e)) => HostSource::from_error(host.clone(), &e),
            Err(_) => HostSource::from_error(
                host.clone(),
                &SourceError::Shed(shed_core::http::ShedError::Transport("task".into())),
            ),
        });
    }
    rows
}

fn err(code: &str, message: impl Into<String>) -> (String, String) {
    (code.to_string(), message.into())
}

/// This machine's identity, or why there is none: the source runtime's, or, with no
/// runtime yet (only before setup finished), the identity file read directly.
async fn this_machine(rt: Option<&DeliveryRuntime>) -> Result<MachineIdentity, String> {
    match rt {
        Some(rt) => rt.machine(),
        None => tokio::task::spawn_blocking(|| machine::load_or_create().map(|l| l.identity))
            .await
            .unwrap_or_else(|e| Err(format!("machine identity load task failed: {e}"))),
    }
}

/// `source.status` → `{rows, this_machine, this_machine_error}`.
///
/// `rows` is one per configured server ([`shed_app::HostSource`]: `host`, `supported`,
/// `source` `{id, name}` or null, `this_machine`, `connected`, `busy`, `unreachable`,
/// `error`); a server older than the route is `supported: false` with no error, and one
/// that could not be reached or read is `unreachable` (unknown, not unsupported).
/// `this_machine` is `{id, name}` or null, with `this_machine_error` saying why it is
/// null.
pub async fn status(servers: &Servers, rt: Option<&DeliveryRuntime>) -> Value {
    let me = this_machine(rt).await;
    let rows = rows_of(&servers.clients, me.as_ref().ok().map(|m| m.id.as_str())).await;
    let (this_machine, this_machine_error) = identity_json(&me);
    json!({
        "rows": rows,
        "this_machine": this_machine,
        "this_machine_error": this_machine_error,
        "config_error": servers.config_unreadable.then_some("unreadable"),
    })
}

/// Who answers on a server, as `sources.list` names it: a closed list.
///
/// | kind | the row |
/// |---|---|
/// | `this_machine` | this machine is the bound source (`machine` is it) |
/// | `other_machine` | another machine is (`machine` names it) |
/// | `service` | the account service answers, and no machine (`url`, and `state` `not_delivering` when the service does not deliver to that server) |
/// | `nobody` | no source is bound |
/// | `unsupported` | the server is older than credential sources |
/// | `unknown` | the server could not say (unreachable, busy, or it refused the read: `error`) |
pub fn kind_of(row: &HostSource) -> &'static str {
    if !row.supported && row.error.is_none() && !row.busy {
        "unsupported"
    } else if row.busy || row.unreachable || row.error.is_some() || !row.supported {
        "unknown"
    } else if row.service.is_some() {
        "service"
    } else if row.source.is_some() && row.this_machine {
        "this_machine"
    } else if row.source.is_some() {
        "other_machine"
    } else {
        "nobody"
    }
}

/// One `sources.list` row: the server's [`HostSource`] in the view's terms, with this
/// machine's delivery stream to it (`stream`, the runtime's status for that server, or
/// null when none runs) and whether this machine stopped answering there (`paused`).
fn list_row(row: &HostSource, stream: Option<Value>, paused: bool) -> Value {
    let service = row.service.as_ref();
    json!({
        "server": row.host,
        "kind": kind_of(row),
        "machine": row.source.as_ref().map(|m| json!({"id": m.id, "name": m.name})),
        "connected": row.connected,
        "url": service.map(|s| s.url.clone()),
        "state": service.and_then(|s| s.state.clone()),
        "stream": stream.unwrap_or(Value::Null),
        "paused": paused,
        "busy": row.busy,
        "unreachable": row.unreachable,
        "error": row.error_class,
        "error_message": row.error_class.map(source_error_words),
    })
}

/// `sources.list` → `{servers, this_machine, this_machine_error, paused_error}`: one row
/// per configured server, by name — `{server, kind, machine, connected, url, state,
/// stream, paused, busy, unreachable, error, error_message}` ([`kind_of`] for `kind`;
/// `paused`: this machine stopped answering there, so it holds no stream; `error` a class
/// from [`shed_app::SOURCE_ERROR_CLASSES`] and `error_message` its fixed words, never the
/// server's) — this machine's `{id, name}` (or why there is none), and `paused_error`,
/// what is wrong with the paused set ([`shed_desktop_core::delivery::source::paused::PausedError`],
/// e.g. `unparseable`: every server is then paused), or null; and `config_error`,
/// `unreadable` while the shed config cannot be read (the rows are then the servers it
/// last listed), or null. Every name, id and URL came from the server and is cleaned for
/// display.
pub async fn list(servers: &Servers, rt: Option<&DeliveryRuntime>) -> Value {
    let me = this_machine(rt).await;
    let rows = rows_of(&servers.clients, me.as_ref().ok().map(|m| m.id.as_str())).await;
    let streams = rt.map(|rt| rt.streams()).unwrap_or_default();
    let listed: Vec<Value> = rows
        .iter()
        .map(|row| {
            let stream = streams
                .iter()
                .find(|s| s.server == row.host)
                .and_then(|s| serde_json::to_value(&s.status).ok());
            list_row(row, stream, rt.is_some_and(|rt| rt.is_paused(&row.host)))
        })
        .collect();
    let (this_machine, this_machine_error) = identity_json(&me);
    json!({
        "servers": listed,
        "this_machine": this_machine,
        "this_machine_error": this_machine_error,
        "paused_error": rt.and_then(|rt| rt.paused_error()),
        "config_error": servers.config_unreadable.then_some("unreadable"),
    })
}

fn identity_json(me: &Result<MachineIdentity, String>) -> (Value, Value) {
    match me {
        Ok(m) => (
            json!({
                "id": m.id,
                "name": display_source_text(&m.name, SOURCE_TEXT_MAX_BYTES),
            }),
            Value::Null,
        ),
        Err(e) => (Value::Null, Value::from(e.clone())),
    }
}

/// `source.unbind {host}` → "Stop answering from here": clear this user's credential
/// source on `host` (`shed source unbind`) and stop this machine answering there.
/// Answers `{server, unbound, previous, previous_was_this_machine, forgotten_machines,
/// paused, verified}`: `unbound` false when there was no source (nothing changed),
/// `previous` the machine that was the source (`{id, name}`, cleaned) or null, `paused`
/// that this machine now holds no stream there (false only with no source runtime),
/// `verified` that a read afterwards found this machine not bound (false when it could
/// not be read). A stream attempt admitted after the unbind is unbound once more; still
/// bound then is `rebound` (the pause stays). Moves and unbinds on one server run one
/// at a time.
///
/// The server binds the first of the user's machines whose stream arrives after an
/// unbind, so this machine's stream is stopped FIRST: `host` joins the paused set
/// (written before anything is sent), and the unbind is sent only once that server's
/// stream task has ended (at most [`PAUSE_QUIESCE`]; past it, `stream_not_stopped` and
/// nothing is sent). The pause is taken back only after a refusal that proves nothing
/// changed ([`refusal_changed_nothing`]); on any other failure (no answer, an answer
/// that cannot be read, a server error) it stays, since the unbind may have happened.
/// Every unbind sent (the first and the re-check's) names this machine
/// ([`unbind_this_machine`]): the server clears the binding only while it still names
/// this machine, and when another machine took over meanwhile it refuses `409
/// SOURCE_MOVED`, changing nothing: that is `source_moved`, the pause stays and no
/// further unbind is sent. Refusals in fixed words ([`refusal`]).
pub async fn unbind(
    servers: &Resolver,
    rt: Option<&DeliveryRuntime>,
    host: &str,
) -> Result<Value, (String, String)> {
    servers.client(host).await?;
    // One move or unbind per server at a time: a second waits for the first, so its
    // stream wait and its rollback are its own. The server is resolved again once the
    // lock is held: one removed (or changed) while this waited is not acted on.
    let lock = server_lock(host);
    let _held = lock.lock().await;
    let client = &servers.client(host).await?;
    // Where the account service answers there is no machine to stop: refused before
    // anything changes (no pause, so no stream is stopped and started again), and no
    // unbind is sent.
    let read = client.source_status().await;
    if answers_by_service(&read) {
        return Err(service_refusal(host));
    }
    // A server older than sources has nothing to stop: refused, nothing paused.
    if let Err(e @ SourceError::Unsupported) = &read {
        return Err(refusal(host, e, "unbind"));
    }
    let me_now = this_machine(rt).await.ok();
    let me_id = me_now.as_ref().map(|m| m.id.as_str());
    let was_paused = rt.is_some_and(|rt| rt.is_paused(host));
    if let Some(rt) = rt {
        rt.pause(host).map_err(|_| {
            err(
                "action_failed",
                format!("could not record that this machine stops answering on {host}; nothing was sent"),
            )
        })?;
        if !rt.wait_stopped(host, PAUSE_QUIESCE).await {
            return Err(err(
                "stream_not_stopped",
                format!(
                    "this machine's stream to {host} did not stop in time, so nothing was sent; \
                     it stays stopped: try again"
                ),
            ));
        }
    }
    // Read the binding again now that this machine's stream has stopped (the first read
    // may be stale: a first-connect bind can race it). An unbind is sent only when this
    // read succeeded and names THIS machine. Another machine's binding is never cleared;
    // a read that failed (or is busy) says nothing, so nothing is sent either. In both
    // cases this machine stays paused there.
    let again = client.source_status().await;
    match stop_decision(&again, me_id) {
        StopDecision::Unbind => {}
        StopDecision::AnotherMachine(previous) => {
            return Ok(json!({
                "server": host,
                "unbound": false,
                "previous": previous,
                "previous_was_this_machine": false,
                "forgotten_machines": 0,
                "paused": rt.is_some(),
                "verified": false,
                "another_machine_answers": true,
            }));
        }
        StopDecision::Nobody => {
            return Ok(json!({
                "server": host,
                "unbound": false,
                "previous": null,
                "previous_was_this_machine": false,
                "forgotten_machines": 0,
                "paused": rt.is_some(),
                "verified": true,
            }));
        }
        StopDecision::Unknown => {
            return Ok(json!({
                "server": host,
                "unbound": false,
                "previous": null,
                "previous_was_this_machine": false,
                "forgotten_machines": 0,
                "paused": rt.is_some(),
                "verified": false,
                "unverified": true,
            }));
        }
    }
    // `Unbind` is decided only on a read naming this machine, so the identity is known.
    let Some(me_id) = me_id else {
        return Err(err(
            "machine_identity_unavailable",
            "this machine has no identity, so nothing was sent".to_string(),
        ));
    };
    let done = match unbind_this_machine(client, me_id).await {
        Ok(done) => done,
        Err(e) => {
            if let (Some(rt), false, true) = (rt, was_paused, refusal_changed_nothing(&e)) {
                let _ = rt.resume(host);
            }
            return Err(refusal(host, &e, "unbind"));
        }
    };
    let me = me_now;
    // A stream attempt the server received before the unbind can still be admitted
    // after it and bind this machine again. Look once; if so, unbind once more; if it
    // is still bound then, say so (the pause stays).
    let verified = match me.as_ref() {
        Some(m) => match still_bound(client, &m.id).await {
            Some(true) => {
                // Another machine that took over meanwhile is never cleared: the
                // server refuses SOURCE_MOVED, and nothing more is sent.
                if let Err(e) = unbind_this_machine(client, &m.id).await {
                    if is_source_moved(&e) {
                        return Err(refusal(host, &e, "unbind"));
                    }
                }
                match still_bound(client, &m.id).await {
                    Some(true) => {
                        return Err(err(
                            "rebound",
                            format!(
                                "{host} bound this machine again after the unbind; it stays \
                                 stopped here: try again"
                            ),
                        ))
                    }
                    Some(false) => true,
                    None => false,
                }
            }
            Some(false) => true,
            None => false,
        },
        None => false,
    };
    let previous = done.previous.as_ref();
    Ok(json!({
        "server": host,
        "unbound": done.unbound,
        "previous": previous.map(|p| json!({
            "id": display_source_text(&p.id, SOURCE_TEXT_MAX_BYTES),
            "name": display_source_text(&p.name, SOURCE_TEXT_MAX_BYTES),
        })),
        "previous_was_this_machine": previous
            .zip(me.as_ref())
            .is_some_and(|(p, m)| p.id == m.id),
        "forgotten_machines": done.forgotten_machines,
        "paused": rt.is_some(),
        "verified": verified,
    }))
}

/// Whether a read of a server's sources says the account service answers there: its
/// document names the service, or the route refuses `SOURCE_IS_SERVICE`. Anything else
/// (a machine, nobody, a read that failed) is not.
fn answers_by_service(read: &Result<shed_core::models::SourceDocument, SourceError>) -> bool {
    match read {
        Ok(doc) => doc.service().is_some(),
        Err(SourceError::Api { code, .. }) => code == SOURCE_IS_SERVICE,
        Err(_) => false,
    }
}

/// What a stop does once this machine's stream has stopped, from a fresh read of the
/// binding: unbind (the read names this machine), keep the pause only because another
/// machine answers (its binding is never cleared) or nobody is bound, or keep the pause
/// only because the read said nothing (failed, busy, or a service refusal).
#[derive(Debug, PartialEq)]
enum StopDecision {
    Unbind,
    AnotherMachine(Value),
    Nobody,
    Unknown,
}

fn stop_decision(
    read: &Result<shed_core::models::SourceDocument, SourceError>,
    me: Option<&str>,
) -> StopDecision {
    let Ok(doc) = read else {
        return StopDecision::Unknown;
    };
    if doc.service().is_some() {
        return StopDecision::Unknown;
    }
    match doc.source_machine_id() {
        None => StopDecision::Nobody,
        Some(id) if Some(id) == me => StopDecision::Unbind,
        Some(_) => other_machine_bound(read, me)
            .map(StopDecision::AnotherMachine)
            .unwrap_or(StopDecision::Unknown),
    }
}

/// The machine a read of a server's sources names as this user's source when it is NOT
/// this one (`me`; with no identity, any bound machine is another), as `{id, name}`
/// cleaned; `None` when this machine is the source, nobody is, or the read failed.
fn other_machine_bound(
    read: &Result<shed_core::models::SourceDocument, SourceError>,
    me: Option<&str>,
) -> Option<Value> {
    let doc = read.as_ref().ok()?;
    let id = doc.source_machine_id()?;
    if Some(id) == me {
        return None;
    }
    let name = doc.machine(id).map(|m| m.name.as_str()).unwrap_or("");
    Some(json!({
        "id": display_source_text(id, SOURCE_TEXT_MAX_BYTES),
        "name": display_source_text(name, SOURCE_TEXT_MAX_BYTES),
    }))
}

/// The unbind a stop sends: a compare-and-clear naming this machine (`me`), which the
/// server refuses `409 SOURCE_MOVED` when another machine is the source. A server older
/// than the compare (v0.11) refuses the field `400 INVALID_REQUEST` before changing
/// anything; it gets the plain unbind, as before.
async fn unbind_this_machine(
    client: &Client,
    me: &str,
) -> Result<shed_core::models::SourceUnbound, SourceError> {
    match client.source_unbind_if(me).await {
        Err(SourceError::Api {
            status: 400, code, ..
        }) if code == "INVALID_REQUEST" => client.source_unbind().await,
        other => other,
    }
}

/// Whether an unbind was refused because another machine is the source now.
fn is_source_moved(e: &SourceError) -> bool {
    matches!(e, SourceError::Api { status: 409, code, .. } if code == SOURCE_MOVED)
}

/// Whether `host`'s sources document binds the machine `id` (one `GET /api/sources`);
/// `None` when it could not be read.
async fn still_bound(client: &Client, id: &str) -> Option<bool> {
    client
        .source_status()
        .await
        .ok()
        .map(|doc| doc.source_machine_id() == Some(id))
}

/// The lock a move or an unbind on `host` holds from its first step to its last.
fn server_lock(host: &str) -> std::sync::Arc<tokio::sync::Mutex<()>> {
    static LOCKS: std::sync::OnceLock<
        std::sync::Mutex<std::collections::HashMap<String, std::sync::Arc<tokio::sync::Mutex<()>>>>,
    > = std::sync::OnceLock::new();
    LOCKS
        .get_or_init(Default::default)
        .lock()
        .unwrap_or_else(|e| e.into_inner())
        .entry(host.to_string())
        .or_default()
        .clone()
}

/// How long an unbind waits for this machine's stream to that server to stop.
pub const PAUSE_QUIESCE: std::time::Duration = std::time::Duration::from_secs(10);

/// The refusals the server's unbind route gives BEFORE it changes anything: a server
/// older than the route (a plain 404), and a 4xx with one of these codes. Anything else
/// (no answer, an unreadable one, a 5xx, a busy server, another code) may follow a
/// change.
fn refusal_changed_nothing(e: &SourceError) -> bool {
    const BEFORE_ACTING: &[&str] = &[
        "INVALID_REQUEST",
        "UNAUTHORIZED",
        "FORBIDDEN",
        SOURCE_IS_SERVICE,
    ];
    match e {
        SourceError::Unsupported => true,
        SourceError::Api { status, code, .. } => {
            (400..500).contains(status) && BEFORE_ACTING.contains(&code.as_str())
        }
        _ => false,
    }
}

/// The refusal for a user the account service answers for on `host` (`409
/// SOURCE_IS_SERVICE` on every source route): fixed words, not the server's.
fn service_refusal(host: &str) -> (String, String) {
    err(
        "source_is_service",
        format!("the account service answers for you on {host}; no machine does there"),
    )
}

/// A refused move or unbind (`what`), as a code and fixed words a person can act on:
/// never the server's message, code or a decode error's text.
fn refusal(host: &str, e: &SourceError, what: &str) -> (String, String) {
    match e {
        SourceError::Unsupported => err(
            "unsupported",
            format!(
                "{host} does not support credential sources (the server is older than this app)"
            ),
        ),
        SourceError::Api { code, .. } if code == SOURCE_IS_SERVICE => service_refusal(host),
        e if is_source_moved(e) => err(
            "source_moved",
            format!(
                "another machine took over as your credential source on {host}; nothing was \
                 cleared, and this machine stays stopped there"
            ),
        ),
        SourceError::Api { code, .. } if code == SOURCE_MACHINE_NOT_FOUND => err(
            "source_machine_not_found",
            format!(
                "{host} has not seen this machine yet: its delivery stream has not reached \
                 that server. Check the server, then try again."
            ),
        ),
        SourceError::Api { status: 500, .. } => err(
            "action_failed",
            format!(
                "{host} could not confirm the {what}; check which machine is the source \
                 before trying again"
            ),
        ),
        _ => match source_error_class(e) {
            Some("unauthorized") => err(
                "unauthorized",
                format!("{host} refused this app's credentials"),
            ),
            Some("forbidden") => err(
                "forbidden",
                format!("{host} does not let this client change its credential source"),
            ),
            Some("busy") => err(
                "action_failed",
                format!("{host} is busy; try again in a moment"),
            ),
            Some("unreachable") => err(
                "action_failed",
                format!(
                    "{host} could not be reached, or its answer could not be read; check which \
                     machine is the source before trying again"
                ),
            ),
            _ => err("action_failed", format!("{host} refused the {what}")),
        },
    }
}

/// `source.use {host}` → "Answer from here": take `host` out of the paused set (so a
/// stream runs for it again), make THIS machine the one that answers on `host`, then
/// reopen its delivery stream to `host` so it connects at once. Answers `{row,
/// woke_source}` (`woke_source`: a stream was reopened).
///
/// One request, never retried: a refusal is reported in fixed words ([`refusal`]). An unknown `host` is `bad_request`; no identity is
/// `machine_identity_unavailable`, with the reason, and nothing is sent.
pub async fn use_this_machine(
    servers: &Resolver,
    rt: Option<&DeliveryRuntime>,
    host: &str,
) -> Result<Value, (String, String)> {
    servers.client(host).await?;
    let lock = server_lock(host);
    let _held = lock.lock().await;
    // Resolved again under the lock (a server removed while this waited is refused),
    // and the runtime reconciled with the same config before its stream is resumed, so
    // the resumed stream and the move go to the same, current endpoint.
    let client = servers.client(host).await?;
    if let Some(rt) = rt {
        rt.rescan_now().map_err(|_| {
            err(
                "config_unreadable",
                "the shed config cannot be read; fix it, then try again",
            )
        })?;
    }
    let me = this_machine(rt).await.map_err(|why| {
        err(
            "machine_identity_unavailable",
            format!("this machine has no identity, so it cannot become the source: {why}"),
        )
    })?;
    if let Some(rt) = rt {
        let was_paused = rt.resume(host).map_err(|_| {
            err(
                "action_failed",
                format!(
                    "could not record that this machine answers on {host} again; nothing was sent"
                ),
            )
        })?;
        // After "Stop answering from here" the server has forgotten this machine, and a
        // move to a machine it has not seen is refused: let the stream that now runs
        // again reach it first (with no binding there, it is bound by arriving).
        if was_paused {
            rt.wait_first_attempt(host, RESUME_FIRST_ATTEMPT).await;
        }
    }
    let doc = client
        .source_use(&me.id)
        .await
        .map_err(|e| refusal(host, &e, "move"))?;
    let row = HostSource::from_document(host.to_string(), &doc, Some(&me.id));
    let woke_source = rt.is_some_and(|rt| rt.retry_now(host));
    Ok(json!({ "row": row, "woke_source": woke_source }))
}

/// How long "Answer from here" waits for a resumed stream's first attempt before the
/// move.
const RESUME_FIRST_ATTEMPT: std::time::Duration = std::time::Duration::from_secs(10);

#[cfg(test)]
mod tests {
    use super::*;
    use shed_app::{SourceMachineRow, SourceServiceRow};
    use shed_core::http::ShedError;
    use shed_core::models::SourceDocument;

    fn row_of(text: &str, me: Option<&str>) -> HostSource {
        let doc: SourceDocument = serde_json::from_str(text).unwrap();
        HostSource::from_document("mini".into(), &doc, me)
    }

    const ME: &str = "5f0c6e2a9b8d4c1e7a3f2b6d8e9c0a1b";
    const DESK: &str = "fedcba9876543210fedcba9876543210";

    fn machine_doc(bound: Option<&str>) -> String {
        let binding = bound.map_or("null".to_string(), |id| {
            format!(r#"{{"delivery":[{{"kind":"machine","id":"{id}"}}],"signing":[{{"kind":"machine","id":"{id}"}}]}}"#)
        });
        format!(
            r#"{{"user":"ada","binding":{binding},"machines":[{{"id":"{ME}","name":"box","connected":true}},{{"id":"{DESK}","name":"desk","connected":false}}]}}"#
        )
    }

    /// Builds clients for a config the way the app does in test mode, each server at
    /// `http://127.0.0.1:<port>`, a new port per build: a kept client keeps its port.
    fn counting_build() -> BuildClients {
        let n = Arc::new(std::sync::atomic::AtomicU16::new(9000));
        Arc::new(move |config: &ShedConfig| {
            let port = n.fetch_add(1, std::sync::atomic::Ordering::SeqCst);
            Backend::from_config(config, Some(&format!("http://127.0.0.1:{port}")))
        })
    }

    fn write_config(path: &Path, servers: &[(&str, &str)]) {
        let mut text = String::from("servers:\n");
        for (name, extra) in servers {
            text.push_str(&format!(
                "    {name}:\n        host: {name}.example.test\n{extra}"
            ));
        }
        std::fs::write(path, text).unwrap();
    }

    fn urls(s: &Servers) -> Vec<(String, String)> {
        s.clients
            .iter()
            .map(|(n, c)| (n.clone(), c.base_url().to_string()))
            .collect()
    }

    /// Finding 1: a client is kept only while the server's whole entry is unchanged; a
    /// changed TLS pin (same URL), user or identity file makes a new one. A server added
    /// is there, one removed is gone.
    #[test]
    fn a_client_is_kept_only_while_its_whole_entry_is_unchanged() {
        let dir = tempfile::tempdir().unwrap();
        let cfg = dir.path().join("config.yaml");
        write_config(&cfg, &[("a", ""), ("gone", "")]);
        let build = counting_build();
        let startup_config = ShedConfig::load(&cfg.to_string_lossy());
        let startup = build(&startup_config); // port 9000
        let cs = CurrentServers::new(cfg.clone(), build, &startup, &startup_config);
        write_config(&cfg, &[("a", ""), ("b", "")]);
        let now = cs.read(); // built on 9001
        assert!(!now.config_unreadable);
        assert_eq!(
            urls(&now),
            [
                ("a".into(), "http://127.0.0.1:9000".into()),
                ("b".into(), "http://127.0.0.1:9001".into())
            ],
            "a unchanged keeps the startup client; b added has its own"
        );
        for extra in [
            "        tls_cert_fingerprint: sha256:00\n",
            "        user: other\n",
            "        identity_file: ~/.ssh/other\n",
            "        auth_mode: mtls\n",
        ] {
            let before = urls(&cs.read())[0].1.clone();
            write_config(&cfg, &[("a", extra), ("b", "")]);
            let after = urls(&cs.read())[0].1.clone();
            assert_ne!(before, after, "a changed ({extra:?}) gets a new client");
            // Negative control: read again unchanged, the same client.
            assert_eq!(urls(&cs.read())[0].1, after);
        }
    }

    /// Finding 3: a config that cannot be read is not an empty one: the servers last
    /// read stay, marked, and an action is refused `config_unreadable`; an empty config
    /// read fine lists nothing.
    #[tokio::test]
    async fn an_unreadable_config_keeps_the_last_servers_and_says_so() {
        let dir = tempfile::tempdir().unwrap();
        let cfg = dir.path().join("config.yaml");
        write_config(&cfg, &[("a", "")]);
        let build = counting_build();
        let startup_config = ShedConfig::load(&cfg.to_string_lossy());
        let startup = Arc::new(build(&startup_config));
        let cs = Arc::new(CurrentServers::new(
            cfg.clone(),
            build,
            &startup,
            &startup_config,
        ));
        let r = Resolver::new(Some(cs), startup);
        // A second `servers:` key: the reader refuses the whole file.
        std::fs::write(&cfg, "servers:\n    b:\n        host: b\nservers:\n").unwrap();
        let now = r.now().await;
        assert!(now.config_unreadable);
        assert_eq!(urls(&now)[0].0, "a", "the last servers, never dropped");
        let e = use_this_machine(&r, None, "a").await.unwrap_err();
        assert_eq!(e.0, "config_unreadable");
        let e = unbind(&r, None, "a").await.unwrap_err();
        assert_eq!(e.0, "config_unreadable");
        let shown = list(&now, None).await;
        assert_eq!(shown["config_error"], "unreadable");
        // Negative control: an empty config read fine is no servers, and no error.
        std::fs::write(&cfg, "servers:\n").unwrap();
        let now = r.now().await;
        assert!(!now.config_unreadable && now.clients.is_empty());
        assert_eq!(list(&now, None).await["config_error"], Value::Null);
    }

    /// Finding 4: a move or an unbind waiting for the server's lock resolves the server
    /// again once it holds it: one removed meanwhile is refused `bad_request`.
    #[tokio::test]
    async fn a_server_removed_while_an_action_waits_is_refused() {
        let dir = tempfile::tempdir().unwrap();
        let cfg = dir.path().join("config.yaml");
        write_config(&cfg, &[("lockwait", "")]);
        let build = counting_build();
        let startup_config = ShedConfig::load(&cfg.to_string_lossy());
        let startup = Arc::new(build(&startup_config));
        let cs = Arc::new(CurrentServers::new(
            cfg.clone(),
            build,
            &startup,
            &startup_config,
        ));
        let r = Resolver::new(Some(cs), startup);
        for unbinding in [false, true] {
            write_config(&cfg, &[("lockwait", "")]);
            let lock = server_lock("lockwait");
            let held = lock.lock().await;
            let r2 = r.clone();
            let waiting = tokio::spawn(async move {
                if unbinding {
                    unbind(&r2, None, "lockwait").await
                } else {
                    use_this_machine(&r2, None, "lockwait").await
                }
            });
            // It is past its first check and waiting for the lock.
            tokio::time::sleep(std::time::Duration::from_millis(100)).await;
            assert!(!waiting.is_finished());
            write_config(&cfg, &[("other", "")]);
            drop(held);
            let e = waiting.await.unwrap().unwrap_err();
            assert_eq!(e.0, "bad_request", "unbinding={unbinding}");
        }
    }

    /// Each "Where" state of the Credentials view, from the server's document (the
    /// account service's from the shared fixtures) or its failure.
    #[test]
    fn every_row_says_who_answers() {
        let me = Some(ME);
        let row = row_of(&machine_doc(Some(ME)), me);
        assert_eq!(kind_of(&row), "this_machine");
        let shown = list_row(&row, Some(json!({"state": "connected"})), false);
        assert_eq!(shown["machine"], json!({"id": ME, "name": "box"}));
        assert_eq!(shown["connected"], true);
        assert_eq!(shown["stream"]["state"], "connected");
        assert_eq!(shown["paused"], false);
        let paused = list_row(&row_of(&machine_doc(None), me), None, true);
        assert_eq!(
            (paused["kind"].clone(), paused["paused"].clone()),
            (json!("nobody"), json!(true))
        );
        assert_eq!(
            (shown["url"].clone(), shown["state"].clone()),
            (Value::Null, Value::Null)
        );

        let row = row_of(&machine_doc(Some(DESK)), me);
        assert_eq!(kind_of(&row), "other_machine");
        assert_eq!(list_row(&row, None, false)["machine"]["name"], "desk");

        assert_eq!(kind_of(&row_of(&machine_doc(None), me)), "nobody");

        let row = row_of(
            include_str!("../../../../crates/fixtures/sources/service.json"),
            me,
        );
        assert_eq!(kind_of(&row), "service");
        let shown = list_row(&row, None, false);
        assert_eq!(shown["url"], "https://accounts.example.test");
        assert_eq!(shown["state"], Value::Null);
        assert_eq!(shown["machine"], Value::Null);
        assert_eq!(shown["stream"], Value::Null);
        let row = row_of(
            include_str!("../../../../crates/fixtures/sources/service-not-delivering.json"),
            me,
        );
        assert_eq!(list_row(&row, None, false)["state"], "not_delivering");

        assert_eq!(
            kind_of(&HostSource::from_error(
                "mini".into(),
                &SourceError::Unsupported
            )),
            "unsupported"
        );
        for e in [
            SourceError::Busy {
                retry_after: None,
                message: "x".into(),
            },
            SourceError::Shed(ShedError::Transport("refused".into())),
            api(403, "FORBIDDEN", "a script key"),
        ] {
            assert_eq!(
                kind_of(&HostSource::from_error("mini".into(), &e)),
                "unknown",
                "{e:?}"
            );
        }
        // A row that names both (a server's mistake) is the service's: it says so.
        let mut both = row_of(&machine_doc(Some(ME)), me);
        both.service = Some(SourceServiceRow {
            url: "https://accounts.example.test".into(),
            state: None,
        });
        assert_eq!(kind_of(&both), "service");
        both.service = None;
        both.source = Some(SourceMachineRow {
            id: DESK.into(),
            name: String::new(),
        });
        both.this_machine = false;
        assert_eq!(kind_of(&both), "other_machine");
    }

    fn api(status: u16, code: &str, message: &str) -> SourceError {
        SourceError::Api {
            status,
            code: code.into(),
            message: message.into(),
        }
    }

    /// Every refusal of a move or an unbind is a code and fixed words: no server
    /// message, no server code outside the known ones, no decode error's text.
    #[test]
    fn refusals_are_fixed_words() {
        let busy = SourceError::Busy {
            retry_after: None,
            message: "ghp_SENTINEL".into(),
        };
        let cases: Vec<(SourceError, &str, &str)> = vec![
            (
                api(409, SOURCE_IS_SERVICE, "ghp_SENTINEL"),
                "source_is_service",
                "the account service answers for you on mini; no machine does there",
            ),
            (
                SourceError::Unsupported,
                "unsupported",
                "mini does not support credential sources (the server is older than this app)",
            ),
            (
                api(404, SOURCE_MACHINE_NOT_FOUND, "ghp_SENTINEL"),
                "source_machine_not_found",
                "mini has not seen this machine yet: its delivery stream has not reached that \
                 server. Check the server, then try again.",
            ),
            (
                api(409, SOURCE_MOVED, "ghp_SENTINEL"),
                "source_moved",
                "another machine took over as your credential source on mini; nothing was \
                 cleared, and this machine stays stopped there",
            ),
            (
                api(403, "ghp_SENTINEL_CODE", "ghp_SENTINEL"),
                "forbidden",
                "mini does not let this client change its credential source",
            ),
            (
                api(401, "UNAUTHORIZED", "ghp_SENTINEL"),
                "unauthorized",
                "mini refused this app's credentials",
            ),
            (
                api(500, "INTERNAL_ERROR", "ghp_SENTINEL"),
                "action_failed",
                "mini could not confirm the move; check which machine is the source before \
                 trying again",
            ),
            (
                api(404, "ghp_SENTINEL_CODE", "ghp_SENTINEL"),
                "action_failed",
                "mini refused the move",
            ),
            (busy, "action_failed", "mini is busy; try again in a moment"),
            (
                SourceError::Shed(ShedError::Transport("ghp_SENTINEL".into())),
                "action_failed",
                "mini could not be reached, or its answer could not be read; check which \
                 machine is the source before trying again",
            ),
            (
                SourceError::Shed(ShedError::Decode("ghp_SENTINEL".into())),
                "action_failed",
                "mini could not be reached, or its answer could not be read; check which \
                 machine is the source before trying again",
            ),
        ];
        for (e, code, message) in cases {
            let got = refusal("mini", &e, "move");
            assert_eq!((got.0.as_str(), got.1.as_str()), (code, message), "{e:?}");
            let unbind = refusal("mini", &e, "unbind");
            assert!(!unbind.1.contains("SENTINEL"), "{e:?}");
        }
        assert_eq!(
            refusal("mini", &api(418, "X", "ghp_SENTINEL"), "unbind").1,
            "mini refused the unbind"
        );
    }

    /// An unbind where the account service answers is refused before any pause: from
    /// the service's document (the shared fixtures) or its `SOURCE_IS_SERVICE` refusal;
    /// a machine's document, nobody, or a failed read is not the service.
    #[test]
    fn the_service_is_told_from_the_sources_read() {
        let doc = |t: &str| Ok(serde_json::from_str::<SourceDocument>(t).unwrap());
        for t in [
            include_str!("../../../../crates/fixtures/sources/service.json"),
            include_str!("../../../../crates/fixtures/sources/service-not-delivering.json"),
        ] {
            assert!(answers_by_service(&doc(t)));
        }
        assert!(answers_by_service(&Err(api(409, SOURCE_IS_SERVICE, "x"))));
        assert!(!answers_by_service(&doc(&machine_doc(Some(ME)))));
        assert!(!answers_by_service(&doc(&machine_doc(None))));
        assert!(!answers_by_service(&Err(api(403, "FORBIDDEN", "x"))));
        assert!(!answers_by_service(&Err(SourceError::Unsupported)));
    }

    /// After the pause a stop unbinds only on a read that names this machine; another
    /// machine, nobody, or a read that says nothing keeps the pause and sends nothing.
    #[test]
    fn a_stop_unbinds_only_on_a_read_naming_this_machine() {
        let doc = |t: &str| Ok(serde_json::from_str::<SourceDocument>(t).unwrap());
        assert_eq!(
            stop_decision(&doc(&machine_doc(Some(ME))), Some(ME)),
            StopDecision::Unbind
        );
        assert_eq!(
            stop_decision(&doc(&machine_doc(Some(DESK))), Some(ME)),
            StopDecision::AnotherMachine(json!({"id": DESK, "name": "desk"}))
        );
        assert!(
            matches!(
                stop_decision(&doc(&machine_doc(Some(ME))), None),
                StopDecision::AnotherMachine(_)
            ),
            "with no identity, no machine is this one"
        );
        assert_eq!(
            stop_decision(&doc(&machine_doc(None)), Some(ME)),
            StopDecision::Nobody
        );
        for e in [
            SourceError::Busy {
                retry_after: None,
                message: "x".into(),
            },
            SourceError::Shed(ShedError::Transport("x".into())),
            api(409, SOURCE_IS_SERVICE, "x"),
            SourceError::Unsupported,
        ] {
            assert_eq!(stop_decision(&Err(e), Some(ME)), StopDecision::Unknown);
        }
        assert_eq!(
            stop_decision(
                &doc(include_str!(
                    "../../../../crates/fixtures/sources/service.json"
                )),
                Some(ME)
            ),
            StopDecision::Unknown
        );
    }

    /// A stop where another machine answers pauses this machine only: the other machine
    /// is told from the read, and its binding is never cleared.
    #[test]
    fn another_machine_bound_is_told_from_the_read() {
        let doc = |t: &str| Ok(serde_json::from_str::<SourceDocument>(t).unwrap());
        assert_eq!(
            other_machine_bound(&doc(&machine_doc(Some(DESK))), Some(ME)),
            Some(json!({"id": DESK, "name": "desk"}))
        );
        assert_eq!(
            other_machine_bound(&doc(&machine_doc(Some(DESK))), None),
            Some(json!({"id": DESK, "name": "desk"})),
            "with no identity, any bound machine is another"
        );
        // Negative controls: this machine bound, nobody bound, a failed read.
        assert_eq!(
            other_machine_bound(&doc(&machine_doc(Some(ME))), Some(ME)),
            None
        );
        assert_eq!(
            other_machine_bound(&doc(&machine_doc(None)), Some(ME)),
            None
        );
        assert_eq!(
            other_machine_bound(&Err(SourceError::Unsupported), Some(ME)),
            None
        );
    }

    /// Finding 2: an unbind's pause is taken back only after a refusal the server gives
    /// before it changes anything.
    #[test]
    fn only_a_refusal_before_acting_takes_the_pause_back() {
        for e in [
            SourceError::Unsupported,
            api(400, "INVALID_REQUEST", "x"),
            api(401, "UNAUTHORIZED", "x"),
            api(403, "FORBIDDEN", "x"),
            api(409, SOURCE_IS_SERVICE, "x"),
        ] {
            assert!(refusal_changed_nothing(&e), "{e:?}");
        }
        for e in [
            api(500, "INTERNAL_ERROR", "x"),
            api(503, "PLUGINS_DISABLED", "x"),
            api(409, "SOMETHING_ELSE", "x"),
            // Another machine took over: nothing changed, but the pause stays (this
            // machine was asked to stop answering there).
            api(409, SOURCE_MOVED, "x"),
            api(500, "FORBIDDEN", "x"),
            SourceError::Busy {
                retry_after: None,
                message: "x".into(),
            },
            SourceError::Shed(ShedError::Transport("reset".into())),
            SourceError::Shed(ShedError::Decode("not the answer".into())),
            SourceError::Shed(ShedError::BadStatus(502)),
        ] {
            assert!(!refusal_changed_nothing(&e), "{e:?}");
        }
    }

    /// A row's error is a class and fixed words, never the server's.
    #[test]
    fn a_row_error_is_a_class() {
        let row = HostSource::from_error(
            "mini".into(),
            &api(403, "ghp_SENTINEL_CODE", "ghp_SENTINEL"),
        );
        let shown = list_row(&row, None, false);
        assert_eq!(shown["error"], "forbidden");
        assert_eq!(
            shown["error_message"],
            "the server does not let this client see its credential source"
        );
        assert!(!shown.to_string().contains("SENTINEL"), "{shown}");
        for class in shed_app::SOURCE_ERROR_CLASSES {
            assert!(!source_error_words(class).is_empty());
        }
    }

    /// One lock per server: the same one for the same name, another for another, and a
    /// second holder waits for the first.
    #[tokio::test]
    async fn moves_and_unbinds_on_one_server_run_one_at_a_time() {
        let a = server_lock("lock-test-a");
        assert!(std::sync::Arc::ptr_eq(&a, &server_lock("lock-test-a")));
        assert!(!std::sync::Arc::ptr_eq(&a, &server_lock("lock-test-b")));
        let held = a.lock().await;
        assert!(server_lock("lock-test-a").try_lock().is_err());
        assert!(server_lock("lock-test-b").try_lock().is_ok());
        drop(held);
        assert!(server_lock("lock-test-a").try_lock().is_ok());
    }
}
