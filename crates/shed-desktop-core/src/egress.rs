//! The egress decision feed (plan 039 §3.10, PR B): one consumer of
//! `GET /api/egress/stream` per configured server, on the app's own control-scope client
//! (`shed_core::http::Client`, the one the Backend lists sheds with), keeping the last
//! [`KEEP_PER_SHED`] decisions of each shed in memory.
//!
//! It replaces the retired broker's consumer, which fed the decisions into its audit log.
//! Nothing is written to disk: a restart starts empty, and what a shed shows is what its
//! server streamed since the app connected.
//!
//! # Reconnecting
//!
//! A stream that ends, fails or is refused is opened again after a wait that starts at
//! [`EgressOptions::base`] and doubles to [`EgressOptions::max`]. It resets to the base
//! once a stream has held for [`EgressOptions::hold`]. A `501` or `404` (egress control
//! is not enabled on that server, or the server is older than the route) waits
//! [`EgressOptions::unavailable`] instead and then starts again from the base. A `401`
//! is the client's: it re-mints once inside the request, and a refusal after that is an
//! ordinary failure here. The server sends no keepalive, so a stream silent for
//! [`EgressOptions::idle`] is ended and opened afresh (it counts as held).
//!
//! # What the UI reads
//!
//! [`EgressFeed::recent`] is the shed's decisions, newest first; [`blocks`] keeps the
//! `deny` ones, which is what a shed's detail shows as its recent blocks.
//!
//! # Incarnations
//!
//! Decisions are kept per shed **incarnation**: its server, its name and its creation
//! time, the `shed_created_at` the server stamps on every decision and the `created_at`
//! the shed listing carries (compared as instants, [`shed_app::timefmt::parse_instant`]).
//! A shed deleted and created again under its name is another incarnation and starts
//! empty. A decision that names no incarnation (one written before the server recorded
//! it) is dropped: the server itself attributes such a record to no one
//! (`Bridge.IncarnationUser`), and there is no other way to tie it to a shed. A reader
//! must name the incarnation ([`EgressFeed::recent`]'s `created_at`, from the shed's
//! row), and [`EgressFeed::observe`] drops the incarnations a server's listing no longer
//! has.
//!
//! Every string kept is cleaned for display ([`display_source_text`]) and bounded: a
//! decision's text comes from the server and, through the host a shed dialled, from
//! inside the shed.

use std::collections::{HashMap, HashSet, VecDeque};
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

use serde::Serialize;
use shed_core::http::Client;
use shed_core::models::{display_source_text, EgressDecision};
use tokio::sync::watch;

/// The decisions kept per shed.
pub const KEEP_PER_SHED: usize = 50;
/// The sheds kept per server: past it, the shed whose last decision is oldest goes.
pub const MAX_SHEDS_PER_SERVER: usize = 512;
/// The longest a kept string is, in bytes.
const MAX_TEXT_BYTES: usize = 256;

/// How the consumers reconnect.
#[derive(Debug, Clone, Copy)]
pub struct EgressOptions {
    pub base: Duration,
    pub max: Duration,
    /// After a stream has held this long, the wait starts again from `base`.
    pub hold: Duration,
    /// The wait after a `501`/`404`.
    pub unavailable: Duration,
    /// A stream silent this long is ended and opened again.
    pub idle: Duration,
}

impl Default for EgressOptions {
    fn default() -> Self {
        EgressOptions {
            base: Duration::from_secs(1),
            max: Duration::from_secs(30),
            hold: Duration::from_secs(60),
            unavailable: Duration::from_secs(5 * 60),
            idle: Duration::from_secs(10 * 60),
        }
    }
}

/// One decision, as kept: the server's fields, cleaned for display.
#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
pub struct EgressEvent {
    pub server: String,
    pub ts: String,
    pub shed: String,
    pub shed_created_at: String,
    pub host: String,
    pub port: i64,
    pub resolved_ip: String,
    pub protocol: String,
    /// `allow` or `deny`.
    pub verdict: String,
    pub reason: String,
}

impl EgressEvent {
    fn from_decision(server: &str, d: &EgressDecision) -> Self {
        let text = |s: &str| display_source_text(s, MAX_TEXT_BYTES);
        EgressEvent {
            server: server.to_string(),
            ts: text(&d.ts),
            shed: text(&d.shed),
            shed_created_at: text(&d.shed_created_at),
            host: text(&d.host),
            port: d.port,
            resolved_ip: text(&d.resolved_ip),
            protocol: text(&d.protocol),
            verdict: text(&d.verdict),
            reason: text(&d.reason),
        }
    }

    /// Whether the decision blocked the connection.
    pub fn is_block(&self) -> bool {
        self.verdict == "deny"
    }
}

/// The `deny` decisions of `events`, in their order: a shed's recent blocks.
pub fn blocks(events: &[EgressEvent]) -> Vec<EgressEvent> {
    events.iter().filter(|e| e.is_block()).cloned().collect()
}

/// A server's stream, for the status.
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize)]
pub struct StreamState {
    pub server: String,
    /// `connecting`, `connected`, `waiting`, `unavailable` or `stopped`.
    pub state: String,
    /// The last failure, cleaned for display; null once a stream opens.
    pub error: Option<String>,
    pub connects: u64,
    pub decisions: u64,
    /// Decisions dropped because they named no shed incarnation.
    pub unattributed: u64,
}

/// What is kept: per server, per shed, the newest decisions last.
#[derive(Default)]
pub struct EgressLog {
    sheds: HashMap<String, HashMap<Incarnation, (u64, VecDeque<EgressEvent>)>>,
    tick: u64,
}

/// A shed incarnation within one server: its name and its creation instant.
#[derive(Debug, Clone, PartialEq, Eq, Hash, PartialOrd, Ord)]
pub struct Incarnation {
    pub name: String,
    pub created: (i64, u32),
}

impl Incarnation {
    /// `None` when `created_at` is not an instant (empty, unparseable, Go's zero time).
    pub fn new(name: &str, created_at: &str) -> Option<Self> {
        if name.is_empty() {
            return None;
        }
        Some(Incarnation {
            name: name.to_string(),
            created: shed_app::timefmt::parse_instant(created_at)?,
        })
    }
}

impl EgressLog {
    /// Keeps `e` under its server and shed incarnation, dropping the incarnation's oldest
    /// past [`KEEP_PER_SHED`] and, past [`MAX_SHEDS_PER_SERVER`], the server's least
    /// recently heard-from one. A decision that names no incarnation is not kept; `false`
    /// says so.
    pub fn push(&mut self, e: EgressEvent) -> bool {
        let Some(key) = Incarnation::new(&e.shed, &e.shed_created_at) else {
            return false;
        };
        self.tick += 1;
        let tick = self.tick;
        let sheds = self.sheds.entry(e.server.clone()).or_default();
        if !sheds.contains_key(&key) && sheds.len() >= MAX_SHEDS_PER_SERVER {
            if let Some(oldest) = sheds
                .iter()
                .min_by_key(|(_, (t, _))| *t)
                .map(|(k, _)| k.clone())
            {
                sheds.remove(&oldest);
            }
        }
        let (at, events) = sheds.entry(key).or_default();
        *at = tick;
        events.push_back(e);
        while events.len() > KEEP_PER_SHED {
            events.pop_front();
        }
        true
    }

    /// That incarnation's kept decisions, newest first: `created_at` is the shed's own
    /// (its listing row's), and one that is not an instant reads nothing.
    pub fn recent(&self, server: &str, shed: &str, created_at: &str) -> Vec<EgressEvent> {
        let Some(key) = Incarnation::new(shed, created_at) else {
            return Vec::new();
        };
        self.sheds
            .get(server)
            .and_then(|s| s.get(&key))
            .map(|(_, events)| events.iter().rev().cloned().collect())
            .unwrap_or_default()
    }

    /// Every kept decision, newest first within each incarnation, grouped by server then
    /// incarnation (both sorted).
    pub fn all(&self) -> Vec<(String, Incarnation, Vec<EgressEvent>)> {
        let mut out = Vec::new();
        let mut servers: Vec<&String> = self.sheds.keys().collect();
        servers.sort();
        for server in servers {
            let sheds = &self.sheds[server];
            let mut keys: Vec<&Incarnation> = sheds.keys().collect();
            keys.sort();
            for key in keys {
                out.push((
                    server.clone(),
                    key.clone(),
                    sheds[key].1.iter().rev().cloned().collect(),
                ));
            }
        }
        out
    }

    /// Drops `server`'s incarnations not in `listed`.
    pub fn retain(&mut self, server: &str, listed: &HashSet<Incarnation>) {
        if let Some(sheds) = self.sheds.get_mut(server) {
            sheds.retain(|key, _| listed.contains(key));
        }
    }
}

/// Told when a server's decisions changed (the shells push a UI event).
pub type OnEgress = Arc<dyn Fn(&str) + Send + Sync>;

struct Shared {
    log: Mutex<EgressLog>,
    streams: Mutex<HashMap<String, StreamState>>,
    on_change: Option<OnEgress>,
}

impl Shared {
    fn set_state(&self, server: &str, f: impl FnOnce(&mut StreamState)) {
        let mut streams = self.streams.lock().unwrap_or_else(|e| e.into_inner());
        let s = streams
            .entry(server.to_string())
            .or_insert_with(|| StreamState {
                server: server.to_string(),
                ..StreamState::default()
            });
        f(s);
    }
}

/// The feed: one consumer per server.
pub struct EgressFeed {
    shared: Arc<Shared>,
    stop: watch::Sender<bool>,
    tasks: Mutex<Vec<tokio::task::JoinHandle<()>>>,
}

impl EgressFeed {
    /// Starts a consumer on `handle` for each of `clients` (the Backend's
    /// `host_clients()`: name and control-scope client).
    pub fn start(
        handle: &tokio::runtime::Handle,
        clients: Vec<(String, Client)>,
        opts: EgressOptions,
        on_change: Option<OnEgress>,
    ) -> Arc<Self> {
        let shared = Arc::new(Shared {
            log: Mutex::new(EgressLog::default()),
            streams: Mutex::new(HashMap::new()),
            on_change,
        });
        let (stop, stop_rx) = watch::channel(false);
        let tasks = clients
            .into_iter()
            .map(|(server, client)| {
                shared.set_state(&server, |s| s.state = "connecting".into());
                handle.spawn(run(shared.clone(), server, client, opts, stop_rx.clone()))
            })
            .collect();
        Arc::new(EgressFeed {
            shared,
            stop,
            tasks: Mutex::new(tasks),
        })
    }

    /// That shed incarnation's recent decisions, newest first (see [`EgressLog::recent`]).
    pub fn recent(&self, server: &str, shed: &str, created_at: &str) -> Vec<EgressEvent> {
        self.shared
            .log
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .recent(server, shed, created_at)
    }

    /// Every kept decision ([`EgressLog::all`]).
    pub fn all(&self) -> Vec<(String, Incarnation, Vec<EgressEvent>)> {
        self.shared
            .log
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .all()
    }

    /// Drops `server`'s incarnations not in `listed` (the server's listing just answered).
    pub fn retain(&self, server: &str, listed: &HashSet<Incarnation>) {
        self.shared
            .log
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .retain(server, listed);
    }

    /// Drops, for each server the listing `r` answered for, the incarnations it no longer
    /// lists: a deleted shed's decisions go with it, and a shed created again under its
    /// name keeps none of the old one's. A listed shed with no creation time keeps none.
    pub fn observe(&self, r: &shed_app::Reachability) {
        let failed: HashSet<&str> = r.host_errors.iter().map(|e| e.server.as_str()).collect();
        let servers: Vec<String> = self
            .shared
            .streams
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .keys()
            .filter(|s| !failed.contains(s.as_str()))
            .cloned()
            .collect();
        let mut log = self.shared.log.lock().unwrap_or_else(|e| e.into_inner());
        for server in servers {
            let listed: HashSet<Incarnation> = r
                .sheds
                .iter()
                .filter(|s| s.host == server)
                .filter_map(|s| Incarnation::new(&s.name, s.created_at.as_deref()?))
                .collect();
            log.retain(&server, &listed);
        }
    }

    /// Each server's stream, sorted by server.
    pub fn status(&self) -> Vec<StreamState> {
        let mut v: Vec<StreamState> = self
            .shared
            .streams
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .values()
            .cloned()
            .collect();
        v.sort_by(|a, b| a.server.cmp(&b.server));
        v
    }

    /// Stops every consumer (non-blocking; the shutdown's drain).
    pub fn drain(&self) {
        let _ = self.stop.send(true);
    }

    /// [`Self::drain`], then waits up to `bound` for the consumers to end.
    pub async fn join(&self, bound: Duration) -> bool {
        self.drain();
        let tasks: Vec<_> =
            std::mem::take(&mut *self.tasks.lock().unwrap_or_else(|e| e.into_inner()));
        tokio::time::timeout(bound, futures_util::future::join_all(tasks))
            .await
            .is_ok()
    }
}

/// The wait after a stream, and the wait after the next: see the module doc.
fn next_wait(
    opts: &EgressOptions,
    prev: Duration,
    held: Duration,
    unavailable: bool,
) -> (Duration, Duration) {
    if unavailable {
        return (opts.unavailable, opts.base);
    }
    let wait = if held >= opts.hold { opts.base } else { prev };
    (wait, (wait * 2).min(opts.max))
}

async fn run(
    shared: Arc<Shared>,
    server: String,
    client: Client,
    opts: EgressOptions,
    mut stop: watch::Receiver<bool>,
) {
    let mut wait = opts.base;
    loop {
        if *stop.borrow() {
            break;
        }
        shared.set_state(&server, |s| s.state = "connecting".into());
        let opened = Instant::now();
        let mut on_open = || {
            shared.set_state(&server, |s| {
                s.state = "connected".into();
                s.error = None;
                s.connects += 1;
            });
        };
        let mut on_decision = |d: EgressDecision| {
            let e = EgressEvent::from_decision(&server, &d);
            let kept = shared.log.lock().unwrap_or_else(|e| e.into_inner()).push(e);
            shared.set_state(&server, |s| {
                s.decisions += 1;
                if !kept {
                    s.unattributed += 1;
                }
            });
            if kept {
                if let Some(f) = &shared.on_change {
                    f(&server);
                }
            }
        };
        let ended = tokio::select! {
            _ = stop.wait_for(|v| *v) => break,
            r = client.egress_stream(opts.idle, &mut on_open, &mut on_decision) => r,
        };
        let held = opened.elapsed();
        let unavailable = matches!(&ended, Err(e) if matches!(e.status, Some(404 | 501)));
        let (this_wait, next) = next_wait(&opts, wait, held, unavailable);
        wait = next;
        shared.set_state(&server, |s| {
            s.state = if unavailable {
                "unavailable"
            } else {
                "waiting"
            }
            .into();
            s.error = ended
                .as_ref()
                .err()
                .map(|e| display_source_text(&e.message, 512));
        });
        tokio::select! {
            _ = stop.wait_for(|v| *v) => break,
            _ = tokio::time::sleep(this_wait) => {}
        }
    }
    shared.set_state(&server, |s| s.state = "stopped".into());
}

#[cfg(test)]
#[path = "egress_tests.rs"]
mod tests;
