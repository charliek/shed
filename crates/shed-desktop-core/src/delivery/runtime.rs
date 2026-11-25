//! The source runtime (plan 039 §3.7, commit 3): one `shed-source` stream per server of
//! the shed CLI config, answers built with `shed-provision` and the providers, and the
//! state and settings a shell shows.
//!
//! # Answering
//!
//! Each request re-reads `delivery.toml` (so a change needs no restart) and runs the
//! enabled providers on a blocking thread, through the [`DiskReader`], fingerprinting
//! under this machine's key ([`super::key`]). A settings file that cannot be read is
//! answered with no items and one problem saying where it is wrong (fixed words, never
//! the file's text), so the shed's record says why rather than waiting out its deadline.
//! What was answered is kept, redacted (names, targets, sizes, fingerprints, problems,
//! and what the server made of it), for [`DeliveryRuntime::status`].
//!
//! # Identity and credentials
//!
//! This machine's one identity is `<state>/broker/machine.json` and, on a secure server,
//! the durable credentials-scope store is `<state>/broker/creds` (the paths the retired
//! embedded broker used, kept so no pin and no certificate is lost):
//! [`shed_source::credentials::SourceCredentials`] presents whatever certificate is
//! stored and mints only when the stored one will not do. Store writes are serialized by
//! the store's own lock, held only for each write.
//!
//! # Test mode
//!
//! With a mock base URL every server is reached there over plain HTTP (as the app's
//! `Backend` does), with no pin and no SSH endpoint: nothing is ever minted, and a server
//! the harness marks unreachable is pointed at a closed port.

use std::collections::{HashSet, VecDeque};
use std::path::PathBuf;
use std::sync::{Arc, Mutex};
use std::time::Duration;

use serde::Serialize;
use serde_json::{json, Value};
use shed_provision::{
    valid_message, Answer, FingerprintKey, PreviewItem, Problem, ProblemCode, Reason, Request,
};
use shed_source::credentials::SourceCredentials;
use shed_source::delivery::{
    AnswerOutcome, Answerer, NoCredentials, SourceCheck, StreamCredentials, Timing,
};
use shed_source::machine::MachineIdentity;
use shed_source::minter::{CredentialMinter, Minter};
use shed_source::paused::{PausedError, PausedServers};
use shed_source::tasks::SourceTasks;
use shed_source::ServerTarget;
use tokio::sync::watch;

use super::reader::DiskReader;
use super::settings_file;

/// How often the server list is read again.
pub const RESCAN: Duration = Duration::from_secs(30);
/// How many answers [`DeliveryRuntime::status`] keeps.
pub const RECENT: usize = 32;
/// What an answer's budget leaves before the request's deadline: the time to post the
/// answer and for the server to take it.
pub const ANSWER_MARGIN: Duration = Duration::from_secs(2);
/// The provider name of the runtime's own problems (the settings file, the key).
pub const RUNTIME_PROVIDER: &str = "delivery";

/// How the runtime is set up.
#[derive(Debug, Clone)]
pub struct DeliveryOptions {
    /// The shed CLI config the servers are read from.
    pub shed_config_path: PathBuf,
    /// `delivery.toml`.
    pub settings_path: PathBuf,
    /// The fingerprint key's file ([`super::key::key_path`]).
    pub key_path: PathBuf,
    /// The `known_hosts` the SSH minter pins servers against.
    pub known_hosts: String,
    /// Test mode: every server at this URL, or its own override in it
    /// ([`shed_app::mock_url_for`]).
    pub mock_base_url: Option<String>,
    /// Test mode: servers pointed at a closed port.
    pub unreachable_hosts: HashSet<String>,
    /// What providers read through.
    pub reader: DiskReader,
    /// How often the server list is read again.
    pub rescan: Duration,
    /// How often a server settled on the account service is checked again
    /// ([`shed_source::delivery::SERVICE_RECHECK`]).
    pub service_recheck: Duration,
    /// The servers this machine does not answer on
    /// ([`shed_source::paused::paused_state_path`]).
    pub paused_path: PathBuf,
}

/// What a [`DeliveryRuntime::preview`] is of.
#[derive(Debug, Clone)]
pub enum PreviewOf {
    /// Everything the providers have, before any `only` rule.
    Everything,
    /// The shed of this document (as its server sent it).
    Shed(Value),
    /// A new shed on `backend` (`firecracker`, `vz`, `remote`) with `repo` (an
    /// `owner/name`, or none).
    Target {
        backend: String,
        repo: Option<String>,
    },
}

/// One answer, redacted.
#[derive(Debug, Clone, Serialize)]
pub struct Answered {
    pub server: String,
    pub request: String,
    pub shed: String,
    pub reason: Reason,
    pub at: String,
    pub items: Vec<PreviewItem>,
    pub problems: Vec<Problem>,
    /// What became of it: `pending` until the server answered the post.
    pub outcome: Value,
}

struct SourceAnswerer {
    settings_path: PathBuf,
    reader: DiskReader,
    key: Result<FingerprintKey, String>,
    recent: Mutex<VecDeque<Answered>>,
}

/// The instant an answer must be built by: the request's deadline (`deadline`, the
/// contract's time form, judged against `now`, the host's wall clock in Unix seconds)
/// less [`ANSWER_MARGIN`]. A deadline already that close, or one that cannot be read,
/// leaves no time at all.
fn budget(now: i64, deadline: &str) -> std::time::Instant {
    let start = std::time::Instant::now();
    let left = shed_provision::parse_time(deadline)
        .map(|d| d.saturating_sub(now))
        .filter(|s| *s > 0)
        .map(|s| Duration::from_secs(s as u64).saturating_sub(ANSWER_MARGIN))
        .unwrap_or(Duration::ZERO);
    start + left
}

fn runtime_problem(message: &str) -> Problem {
    Problem {
        provider: RUNTIME_PROVIDER.into(),
        code: ProblemCode::Unreadable,
        message: if valid_message(message) {
            message.to_string()
        } else {
            "the delivery settings could not be read".to_string()
        },
    }
}

impl SourceAnswerer {
    /// The answer to `request` from `server`, and what it holds, redacted. Blocking.
    fn build(&self, server: &str, request: &Request) -> (Answer, Vec<PreviewItem>) {
        let empty = |problem: Problem| {
            (
                Answer {
                    id: request.id.clone(),
                    items: Vec::new(),
                    problems: vec![problem],
                },
                Vec::new(),
            )
        };
        let key = match &self.key {
            Ok(k) => k,
            Err(_) => {
                return empty(runtime_problem(
                    "this machine's fingerprint key could not be read",
                ))
            }
        };
        let settings = match settings_file::load(&self.settings_path).settings {
            Ok(s) => s,
            Err(e) => return empty(runtime_problem(&e)),
        };
        let now = shed_source::time::now_unix();
        match shed_provision_providers::build_answer(
            &settings,
            request,
            server,
            now,
            Some(budget(now, &request.deadline)),
            &self.reader,
            key,
        ) {
            Ok(built) => (built.answer, built.preview.items),
            // The builder exists to keep the rules; a refusal is a bug, said plainly.
            Err(_) => empty(runtime_problem(
                "the answer broke the contract's rules and was not sent",
            )),
        }
    }

    /// Keeps `a`; when its outcome was told first (a stream that ended while the answer
    /// was built), the outcome is kept.
    fn record(&self, a: Answered) {
        let mut r = self.recent.lock().unwrap_or_else(|e| e.into_inner());
        if let Some(early) = r
            .iter_mut()
            .rev()
            .find(|e| e.server == a.server && e.request == a.request)
        {
            let outcome = std::mem::take(&mut early.outcome);
            *early = Answered { outcome, ..a };
            return;
        }
        r.push_back(a);
        while r.len() > RECENT {
            r.pop_front();
        }
    }
}

/// The [`Answerer`] the streams call: the shared answerer.
struct Answering(Arc<SourceAnswerer>);

#[async_trait::async_trait]
impl Answerer for Answering {
    async fn answer(&self, server: &str, request: Request) -> Option<Answer> {
        let this = self.0.clone();
        let server = server.to_string();
        let built = tokio::task::spawn_blocking(move || {
            let (answer, items) = this.build(&server, &request);
            this.record(Answered {
                server,
                request: request.id.clone(),
                shed: request.shed.name.clone(),
                reason: request.reason,
                at: shed_source::time::now_rfc3339(),
                items,
                problems: answer.problems.clone(),
                outcome: json!({"outcome": "pending"}),
            });
            answer
        })
        .await;
        built.ok()
    }

    fn outcome(&self, server: &str, request_id: &str, outcome: &AnswerOutcome) {
        let mut r = self.0.recent.lock().unwrap_or_else(|e| e.into_inner());
        if let Some(a) = r
            .iter_mut()
            .rev()
            .find(|a| a.server == server && a.request == request_id)
        {
            a.outcome = serde_json::to_value(outcome).unwrap_or(Value::Null);
            return;
        }
        // Told before the answer was built (dropped at its stream's end): kept until
        // the build records the rest.
        r.push_back(Answered {
            server: server.to_string(),
            request: request_id.to_string(),
            shed: String::new(),
            reason: Reason::Create,
            at: shed_source::time::now_rfc3339(),
            items: Vec::new(),
            problems: Vec::new(),
            outcome: serde_json::to_value(outcome).unwrap_or(Value::Null),
        });
        while r.len() > RECENT {
            r.pop_front();
        }
    }
}

/// The source runtime.
pub struct DeliveryRuntime {
    opts: DeliveryOptions,
    machine: Result<MachineIdentity, String>,
    answerer: Arc<SourceAnswerer>,
    tasks: Option<Arc<SourceTasks>>,
    /// Why the config could not be read last; its lock also serializes every change of
    /// the streams (a rescan, a pause, a resume), so a rescan that read the set before a
    /// pause cannot restart the paused server's stream after it.
    config_error: Arc<Mutex<Option<String>>>,
    /// The servers this machine does not answer on ("Stop answering from here").
    paused: PausedServers,
    /// The config's servers as last read.
    targets: Mutex<Vec<ServerTarget>>,
    stop: watch::Sender<bool>,
}

impl DeliveryRuntime {
    /// Starts the runtime on `handle`: loads this machine's identity (`machine`, made by
    /// the caller with `shed_source::machine::load_or_create`) and the fingerprint key,
    /// reads the servers and starts a stream for each, and reads the list again every
    /// [`DeliveryOptions::rescan`]. Without an identity no stream runs (the status says
    /// why). A stream refused `SOURCE_IS_SERVICE` asks `service_check` (the shell's
    /// `GET /api/sources`) whether the account service still answers there, every
    /// [`DeliveryOptions::service_recheck`] and when the server list changes. Blocking
    /// file I/O: call it off an async worker.
    pub fn start(
        handle: &tokio::runtime::Handle,
        opts: DeliveryOptions,
        machine: Result<MachineIdentity, String>,
        service_check: Option<SourceCheck>,
    ) -> Arc<Self> {
        let key = super::key::load_or_create(&opts.key_path);
        let answerer = Arc::new(SourceAnswerer {
            settings_path: opts.settings_path.clone(),
            reader: opts.reader.clone(),
            key,
            recent: Mutex::new(VecDeque::new()),
        });
        let (stop, stop_rx) = watch::channel(false);
        let tasks = machine.as_ref().ok().map(|m| {
            let minter: Arc<dyn Minter> = Arc::new(CredentialMinter::new(&opts.known_hosts));
            let credentials = Arc::new(
                move |t: &ServerTarget,
                      fence: watch::Receiver<bool>|
                      -> Arc<dyn StreamCredentials> {
                    if t.is_secure() {
                        Arc::new(SourceCredentials::new(minter.clone(), t.clone(), fence))
                    } else {
                        Arc::new(NoCredentials)
                    }
                },
            );
            let tasks = SourceTasks::new(
                handle.clone(),
                m.clone(),
                Arc::new(Answering(answerer.clone())),
                credentials,
            )
            .with_timing(Timing {
                service_recheck: opts.service_recheck,
                ..Timing::default()
            });
            Arc::new(match service_check {
                Some(check) => tasks.with_service_check(check),
                None => tasks,
            })
        });
        let paused = PausedServers::load(opts.paused_path.clone());
        let rt = Arc::new(DeliveryRuntime {
            opts,
            machine,
            answerer,
            tasks,
            config_error: Arc::new(Mutex::new(None)),
            paused,
            targets: Mutex::new(Vec::new()),
            stop,
        });
        if let Some(tasks) = rt.tasks.clone() {
            let mut stop_rx = stop_rx;
            let rescan = rt.opts.rescan;
            // Entered so the first reconcile can spawn from a synchronous caller.
            let _guard = handle.enter();
            rt.reconcile(&tasks);
            let weak = Arc::downgrade(&rt);
            handle.spawn(async move {
                loop {
                    tokio::select! {
                        _ = stop_rx.wait_for(|v| *v) => break,
                        _ = tokio::time::sleep(rescan) => {}
                    }
                    let Some(rt) = weak.upgrade() else { break };
                    let path = rt.opts.clone();
                    let targets = tokio::task::spawn_blocking(move || targets_of(&path)).await;
                    if let Ok(t) = targets {
                        rt.apply(&tasks, t);
                    }
                }
            });
        }
        rt
    }

    fn reconcile(&self, tasks: &SourceTasks) {
        self.apply(tasks, targets_of(&self.opts));
    }

    /// A new read of the config: a stream runs for every server that is not paused. The
    /// paused set is not touched: a pause lasts until "Answer from here", whatever the
    /// config does meanwhile (a server that leaves it and comes back is still paused).
    fn apply(&self, tasks: &SourceTasks, targets: Result<Vec<ServerTarget>, String>) {
        let mut err = self.config_error.lock().unwrap_or_else(|e| e.into_inner());
        match targets {
            Ok(t) => {
                *err = None;
                *self.targets.lock().unwrap_or_else(|e| e.into_inner()) = t;
                self.reconcile_unpaused(tasks);
            }
            // A config that cannot be read keeps the streams that run.
            Err(e) => *err = Some(e),
        }
    }

    /// Runs a stream for every server last read that is not paused (the caller holds
    /// the `config_error` lock).
    fn reconcile_unpaused(&self, tasks: &SourceTasks) {
        let wanted: Vec<ServerTarget> = self
            .targets
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .iter()
            .filter(|t| !self.paused.contains(&t.name))
            .cloned()
            .collect();
        tasks.reconcile(&wanted);
    }

    /// Reads the shed config again now and reconciles the streams with it, as the
    /// periodic rescan does: a server added runs, one removed stops, one whose endpoint,
    /// pin or identity changed is restarted on the new target. A config that cannot be
    /// read keeps the streams that run and is the error.
    pub fn rescan_now(&self) -> Result<(), String> {
        let targets = targets_of(&self.opts);
        let out = targets.as_ref().map(|_| ()).map_err(Clone::clone);
        match &self.tasks {
            Some(tasks) => self.apply(tasks, targets),
            None => {
                if let Ok(t) = targets {
                    *self.targets.lock().unwrap_or_else(|e| e.into_inner()) = t;
                }
            }
        }
        out
    }

    /// The target `server`'s stream runs on (endpoint, pin, identity), or `None` when
    /// none runs for it.
    pub fn stream_target(&self, server: &str) -> Option<ServerTarget> {
        self.tasks.as_ref().and_then(|t| t.target_of(server))
    }

    /// The config's servers as last read, by name.
    fn configured(&self) -> Vec<String> {
        self.targets
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .iter()
            .map(|t| t.name.clone())
            .collect()
    }

    /// "Stop answering from here": `server` joins the paused set (written first) and its
    /// stream is told to stop; none runs for it until [`Self::resume`], across restarts.
    /// [`Self::wait_stopped`] waits for the stream to have ended.
    pub fn pause(&self, server: &str) -> Result<(), String> {
        let _serial = self.config_error.lock().unwrap_or_else(|e| e.into_inner());
        let configured = self.configured();
        let names: Vec<&str> = configured.iter().map(String::as_str).collect();
        self.paused.pause(server, &names)?;
        if let Some(tasks) = &self.tasks {
            self.reconcile_unpaused(tasks);
        }
        Ok(())
    }

    /// Waits, at most `bound`, until every stream of `server` told to stop has ended:
    /// no attempt of it can still reach the server. `true` when none runs.
    pub async fn wait_stopped(&self, server: &str, bound: Duration) -> bool {
        match &self.tasks {
            Some(t) => {
                t.wait_stopped(server, bound).await && !t.servers().iter().any(|s| s == server)
            }
            None => true,
        }
    }

    /// "Answer from here": `server` leaves the paused set and, when the config lists it,
    /// its stream starts again; `true` when it was paused.
    pub fn resume(&self, server: &str) -> Result<bool, String> {
        let _serial = self.config_error.lock().unwrap_or_else(|e| e.into_inner());
        let was = self.paused.contains(server);
        let configured = self.configured();
        let names: Vec<&str> = configured.iter().map(String::as_str).collect();
        self.paused.resume(server, &names)?;
        if let Some(tasks) = &self.tasks {
            self.reconcile_unpaused(tasks);
        }
        Ok(was)
    }

    /// What is wrong with the paused set, if anything ([`PausedError`]: a closed list).
    pub fn paused_error(&self) -> Option<PausedError> {
        self.paused.error()
    }

    /// Waits, at most `bound`, until `server`'s stream has made its first attempt (its
    /// state is no longer `connecting`); `false` when it had not by then, or none runs.
    pub async fn wait_first_attempt(&self, server: &str, bound: Duration) -> bool {
        let deadline = tokio::time::Instant::now() + bound;
        loop {
            let state = self
                .streams()
                .into_iter()
                .find(|s| s.server == server)
                .map(|s| s.status.state);
            match state {
                None => return false,
                Some(shed_source::delivery::StreamState::Connecting) => {}
                Some(_) => return true,
            }
            if tokio::time::Instant::now() >= deadline {
                return false;
            }
            tokio::time::sleep(Duration::from_millis(25)).await;
        }
    }

    /// Whether this machine does not answer on `server` (every server, while the
    /// paused set's file cannot be used).
    pub fn is_paused(&self, server: &str) -> bool {
        self.paused.contains(server)
    }

    /// Opens `server`'s delivery stream again at once (after "Use this machine" made
    /// this machine its source). `false` when no stream runs for it.
    pub fn retry_now(&self, server: &str) -> bool {
        self.tasks.as_ref().is_some_and(|t| t.restart(server))
    }

    /// This machine's identity, or why there is none.
    pub fn machine(&self) -> Result<MachineIdentity, String> {
        self.machine.clone()
    }

    /// The answers this machine gave, newest first, redacted (`delivery.recent`): when,
    /// to which server, for which shed, the items by name, target, size and fingerprint,
    /// the problems, and what the server made of each. No value.
    pub fn recent(&self) -> Vec<Answered> {
        self.answerer
            .recent
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .iter()
            .rev()
            .cloned()
            .collect()
    }

    /// Each server's stream, by name ([`shed_source::tasks::ServerStatus`]); none
    /// without an identity.
    pub fn streams(&self) -> Vec<shed_source::tasks::ServerStatus> {
        self.tasks.as_ref().map(|t| t.status()).unwrap_or_default()
    }

    /// `delivery.status`: this machine, the settings file, each server's stream, and the
    /// recent answers, redacted.
    pub fn status(&self) -> Value {
        let (machine, machine_error) = match &self.machine {
            Ok(m) => (json!({"id": m.id, "name": m.name}), Value::Null),
            Err(e) => (Value::Null, Value::from(e.clone())),
        };
        let servers = serde_json::to_value(self.streams()).unwrap_or_else(|_| json!([]));
        let recent = self.recent();
        json!({
            "machine": machine,
            "machine_error": machine_error,
            "key_error": self.answerer.key.as_ref().err(),
            "config_error": self.config_error.lock().unwrap_or_else(|e| e.into_inner()).clone(),
            "paused": self
                .configured()
                .into_iter()
                .filter(|n| self.paused.contains(n))
                .collect::<Vec<_>>(),
            "paused_error": self.paused.error(),
            "settings_path": self.opts.settings_path.to_string_lossy(),
            "servers": servers,
            "recent": recent,
        })
    }

    /// `delivery.settings`.
    pub fn settings(&self) -> Value {
        let path = &self.opts.settings_path;
        settings_file::view(path, &settings_file::load(path))
    }

    /// `delivery.set`: one provider's table replaced (or removed, with null); answers the
    /// new `delivery.settings` view.
    pub fn set(&self, provider: &str, config: &Value) -> Result<Value, (String, String)> {
        settings_file::set(&self.opts.settings_path, provider, config)?;
        Ok(self.settings())
    }

    /// `delivery.preview`: what the shed of a document from `server` would be delivered
    /// ([`PreviewOf::Shed`]), what a new shed on a backend with a repository would be
    /// ([`PreviewOf::Target`]: the request [`super::preview::request_for_target`] draws,
    /// answered `shed: null` with its `request`), or what the providers have at all
    /// ([`PreviewOf::Everything`], every `only` rule let through). Redacted: no value, no
    /// content.
    ///
    /// The answer carries `runs`, the commands the preview ran ([`settings_file::commands_run`]
    /// of the settings it read). With `expect_runs` (what a person was told would run) a
    /// preview whose settings would run anything else runs nothing and is refused
    /// `settings_changed`.
    pub async fn preview(
        &self,
        server: &str,
        of: PreviewOf,
        expect_runs: Option<Vec<String>>,
    ) -> Result<Value, (String, String)> {
        let answerer = self.answerer.clone();
        let server = server.to_string();
        tokio::task::spawn_blocking(move || {
            let key = answerer
                .key
                .clone()
                .map_err(|e| ("key_unavailable".to_string(), e))?;
            let settings = settings_file::load(&answerer.settings_path)
                .settings
                .map_err(|e| ("settings_unreadable".to_string(), e))?;
            let runs = settings_file::commands_run(&settings);
            if expect_runs.is_some_and(|want| want != runs) {
                return Err((
                    "settings_changed".to_string(),
                    "the settings changed since the commands a check runs were shown".to_string(),
                ));
            }
            let now = shed_source::time::now_unix();
            let deadline = shed_provision::format_time(now + 3600);
            let (request, named) = match of {
                PreviewOf::Everything => {
                    let p =
                        shed_provision_providers::preview(&settings, now, &answerer.reader, &key);
                    return Ok(json!({"server": server, "shed": null, "runs": runs, "preview": p}));
                }
                PreviewOf::Shed(doc) => (
                    super::preview::request_for(&doc, Reason::Provision, &deadline)
                        .map_err(|e| ("bad_shed".to_string(), e))?,
                    true,
                ),
                PreviewOf::Target { backend, repo } => (
                    super::preview::request_for_target(
                        &backend,
                        repo.as_deref(),
                        Reason::Create,
                        &deadline,
                    )
                    .map_err(|e| ("bad_request".to_string(), e.to_string()))?,
                    false,
                ),
            };
            // A preview is asked for by a person and bounded by each command's own
            // timeout, as before: it has no budget.
            let built = shed_provision_providers::build_answer(
                &settings,
                &request,
                &server,
                now,
                None,
                &answerer.reader,
                &key,
            )
            .map_err(|r| ("preview_failed".to_string(), r.to_string()))?;
            Ok(json!({
                "server": server,
                "shed": if named { json!(request.shed.name) } else { Value::Null },
                "request": {
                    "backend": request.shed.backend,
                    "repo": request.repo_name(),
                    "local_dir": request.shed.local_dir,
                    "project_dir": request.shed.project_dir,
                },
                "runs": runs,
                "preview": built.preview,
            }))
        })
        .await
        .map_err(|e| ("preview_failed".to_string(), e.to_string()))?
    }

    /// How long the drain waits for running provider commands to be reaped.
    pub const DRAIN_CHILDREN: Duration = Duration::from_secs(1);

    /// The shutdown's drain: stops every stream and the rescan, refuses every later
    /// provider command, and kills the running ones' process groups, waiting (at most
    /// [`Self::DRAIN_CHILDREN`]) until they are reaped.
    pub fn drain(&self) {
        let _ = self.stop.send(true);
        if let Some(t) = &self.tasks {
            t.stop();
        }
        self.opts.reader.cancel(Self::DRAIN_CHILDREN);
    }

    /// [`Self::drain`], then waits up to `bound` for the streams to end.
    pub async fn join(&self, bound: Duration) -> bool {
        self.drain();
        match &self.tasks {
            Some(t) => t.join(bound).await,
            None => true,
        }
    }
}

/// The servers to hold a stream on: the shed CLI config's, redirected in test mode.
fn targets_of(opts: &DeliveryOptions) -> Result<Vec<ServerTarget>, String> {
    let path = opts.shed_config_path.to_string_lossy();
    let mut targets = if path.is_empty() {
        Vec::new()
    } else {
        shed_source::discovery::load_servers(&path)?
    };
    if let Some(mock) = &opts.mock_base_url {
        for t in &mut targets {
            t.url = if opts.unreachable_hosts.contains(&t.name) {
                "http://127.0.0.1:1".into()
            } else {
                shed_app::mock_url_for(mock, &t.name)
            };
            t.tls_fingerprint = String::new();
            t.ssh_port = 0;
        }
    }
    Ok(targets)
}

#[cfg(test)]
mod tests {
    use super::*;
    use shed_provision::{Backend, RequestShed};
    use std::os::unix::fs::PermissionsExt;

    fn request(repo: Option<&str>) -> Request {
        Request {
            id: "0123456789abcdef0123456789abcdef".into(),
            reason: Reason::Create,
            deadline: "2999-01-01T00:00:00Z".into(),
            user: String::new(),
            shed: RequestShed {
                name: "widgets".into(),
                id: "q6c3g4ymb5m2dkxwe7kyz5hhzu".into(),
                backend: Backend::Vz,
                repo: repo.map(|n| shed_provision::RequestRepo {
                    url: format!("git@git.example.test:{n}.git"),
                    name: Some(n.into()),
                }),
                local_dir: None,
                project_dir: Some("/home/shed/widgets".into()),
            },
        }
    }

    fn answerer(home: &std::path::Path, settings: &str) -> SourceAnswerer {
        let settings_path = home.join("delivery.toml");
        std::fs::write(&settings_path, settings).unwrap();
        SourceAnswerer {
            settings_path,
            reader: DiskReader::new(home, None),
            key: Ok(FingerprintKey::from_bytes([1; 32])),
            recent: Mutex::new(VecDeque::new()),
        }
    }

    #[test]
    fn an_answer_is_built_from_the_settings_and_its_only_rules() {
        let home = tempfile::tempdir().unwrap();
        let f = home.path().join("npmrc");
        std::fs::write(&f, "SENTINEL-NPM").unwrap();
        std::fs::set_permissions(&f, std::fs::Permissions::from_mode(0o600)).unwrap();
        let a = answerer(
            home.path(),
            "[files]\nenabled = true\n[[files.file]]\nfrom = \"~/npmrc\"\nto = \".npmrc\"\nonly = { repos = [\"example/*\"] }\n",
        );
        let (answer, items) = a.build("mini", &request(Some("example/widgets")));
        assert_eq!(answer.items.len(), 1);
        assert_eq!(items[0].target, ".npmrc");
        assert!(!serde_json::to_string(&items).unwrap().contains("SENTINEL"));
        let (answer, _) = a.build("mini", &request(Some("other/widgets")));
        assert!(answer.items.is_empty(), "the only rule leaves it out");
    }

    #[test]
    fn a_broken_settings_file_is_one_problem_and_no_items() {
        let home = tempfile::tempdir().unwrap();
        let a = answerer(home.path(), "[github\nSENTINEL");
        let (answer, items) = a.build("mini", &request(None));
        assert!(answer.items.is_empty() && items.is_empty());
        assert_eq!(answer.problems.len(), 1);
        assert_eq!(answer.problems[0].provider, RUNTIME_PROVIDER);
        assert!(!answer.problems[0].message.contains("SENTINEL"));
        answer.validate(&request(None)).unwrap();
    }

    #[test]
    fn test_mode_points_every_server_at_the_mock_and_never_mints() {
        let dir = tempfile::tempdir().unwrap();
        let cfg = dir.path().join("config.yaml");
        std::fs::write(
            &cfg,
            "servers:\n    a:\n        host: a.example.test\n        ssh_port: 2222\n        api_url: https://a.example.test:18443\n        tls_cert_fingerprint: sha256:00\n    b:\n        host: b.example.test\n",
        )
        .unwrap();
        let opts = DeliveryOptions {
            shed_config_path: cfg,
            settings_path: dir.path().join("delivery.toml"),
            key_path: dir.path().join("key"),
            known_hosts: String::new(),
            mock_base_url: Some("http://127.0.0.1:9".into()),
            unreachable_hosts: ["b".to_string()].into_iter().collect(),
            reader: DiskReader::new(dir.path(), None),
            rescan: RESCAN,
            service_recheck: shed_source::delivery::SERVICE_RECHECK,
            paused_path: dir.path().join("paused.json"),
        };
        let t = targets_of(&opts).unwrap();
        assert_eq!(t[0].url, "http://127.0.0.1:9");
        assert_eq!((t[0].ssh_port, t[0].tls_fingerprint.as_str()), (0, ""));
        assert!(!t[0].is_secure());
        assert_eq!(t[1].url, "http://127.0.0.1:1");
        let none = DeliveryOptions {
            shed_config_path: PathBuf::new(),
            ..opts
        };
        assert!(targets_of(&none).unwrap().is_empty());
    }

    /// The delivery stream mints its credentials-scope certificate over the port its
    /// target names: a config with a leading `---` and `servers: # comment` (both read
    /// by the core reader) keeps its explicit port, and an entry with none still has 0.
    #[test]
    fn a_marked_and_commented_config_keeps_its_explicit_port() {
        let dir = tempfile::tempdir().unwrap();
        let cfg = dir.path().join("config.yaml");
        std::fs::write(
            &cfg,
            "---\nservers: # the servers\n    a:\n        host: a.example.test\n        ssh_port: 2222\n        api_url: https://a.example.test:18443\n    b:\n        host: b.example.test\n",
        )
        .unwrap();
        let opts = DeliveryOptions {
            shed_config_path: cfg,
            settings_path: dir.path().join("delivery.toml"),
            key_path: dir.path().join("key"),
            known_hosts: String::new(),
            mock_base_url: None,
            unreachable_hosts: Default::default(),
            reader: DiskReader::new(dir.path(), None),
            rescan: RESCAN,
            service_recheck: shed_source::delivery::SERVICE_RECHECK,
            paused_path: dir.path().join("paused.json"),
        };
        let t = targets_of(&opts).unwrap();
        let port = |n: &str| t.iter().find(|x| x.name == n).map(|x| x.ssh_port);
        assert_eq!(port("a"), Some(2222));
        assert_eq!(port("b"), Some(0));
    }

    #[test]
    fn the_budget_is_the_deadline_less_the_margin() {
        let now = 1_795_428_000;
        let at = |deadline: &str| {
            budget(now, deadline).saturating_duration_since(std::time::Instant::now())
        };
        let left = at(&shed_provision::format_time(now + 30));
        assert!(
            left <= Duration::from_secs(28) && left > Duration::from_secs(27),
            "{left:?}"
        );
        // A deadline within the margin, past, or unreadable leaves nothing.
        assert_eq!(at(&shed_provision::format_time(now + 1)), Duration::ZERO);
        assert_eq!(at(&shed_provision::format_time(now - 5)), Duration::ZERO);
        assert_eq!(at("soon"), Duration::ZERO);
    }

    /// An answer whose deadline is already within the margin starts no provider: each
    /// one is a `timed_out` problem.
    #[test]
    fn an_answer_with_no_time_left_is_timed_out() {
        let dir = tempfile::tempdir().unwrap();
        let settings_path = dir.path().join("delivery.toml");
        std::fs::write(
            &settings_path,
            "[github]\nenabled = true\ngh = \"/nonexistent/gh\"\n",
        )
        .unwrap();
        let answerer = SourceAnswerer {
            settings_path,
            reader: DiskReader::new(dir.path(), None),
            key: Ok(shed_provision::FingerprintKey::from_bytes([1; 32])),
            recent: Mutex::new(VecDeque::new()),
        };
        let mut r = request(None);
        r.deadline = shed_provision::format_time(shed_source::time::now_unix());
        let (answer, _) = answerer.build("work", &r);
        assert!(answer.items.is_empty());
        assert_eq!(answer.problems.len(), 1);
        assert_eq!(answer.problems[0].code, ProblemCode::TimedOut);
        // With time, the provider runs (and finds no gh).
        r.deadline = shed_provision::format_time(shed_source::time::now_unix() + 60);
        let (answer, _) = answerer.build("work", &r);
        assert_eq!(answer.problems[0].code, ProblemCode::MissingInput);
    }

    /// `delivery.recent` is the answers this machine gave, newest first, capped at
    /// [`RECENT`]: names, targets, sizes and fingerprints, never a value.
    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn recent_answers_are_newest_first_and_carry_no_value() {
        let dir = tempfile::tempdir().unwrap();
        let f = dir.path().join("npmrc");
        std::fs::write(&f, "SENTINEL-RECENT").unwrap();
        std::fs::set_permissions(&f, std::fs::Permissions::from_mode(0o600)).unwrap();
        let settings_path = dir.path().join("delivery.toml");
        std::fs::write(
            &settings_path,
            "[files]\nenabled = true\n[[files.file]]\nfrom = \"~/npmrc\"\nto = \".npmrc\"\n",
        )
        .unwrap();
        let opts = DeliveryOptions {
            shed_config_path: PathBuf::new(),
            settings_path,
            key_path: dir.path().join("key"),
            known_hosts: String::new(),
            mock_base_url: None,
            unreachable_hosts: Default::default(),
            reader: DiskReader::new(dir.path(), None),
            rescan: RESCAN,
            service_recheck: shed_source::delivery::SERVICE_RECHECK,
            paused_path: dir.path().join("paused.json"),
        };
        let machine = MachineIdentity {
            id: "5f0c6e2a9b8d4c1e7a3f2b6d8e9c0a1b".into(),
            name: "ada laptop".into(),
        };
        let rt = tokio::task::spawn_blocking(move || {
            DeliveryRuntime::start(&tokio::runtime::Handle::current(), opts, Ok(machine), None)
        })
        .await
        .unwrap();
        assert!(rt.recent().is_empty());
        assert!(rt.streams().is_empty(), "no server is configured");
        let answering = Answering(rt.answerer.clone());
        for (i, shed) in ["first", "second"].into_iter().enumerate() {
            let mut r = request(None);
            r.id = format!("{i:032x}");
            r.shed.name = shed.into();
            assert!(answering.answer("mini", r).await.is_some());
        }
        answering.outcome("mini", &format!("{:032x}", 1), &AnswerOutcome::Accepted);
        let recent = rt.recent();
        assert_eq!(recent.len(), 2);
        assert_eq!(
            (recent[0].shed.as_str(), recent[1].shed.as_str()),
            ("second", "first")
        );
        assert_eq!(recent[0].items[0].target, ".npmrc");
        assert_eq!(recent[0].outcome, json!({"outcome": "accepted"}));
        assert_eq!(recent[1].outcome, json!({"outcome": "pending"}));
        let shown = serde_json::to_string(&recent).unwrap();
        assert!(!shown.contains("SENTINEL"), "{shown}");
        assert_eq!(
            rt.status()["recent"],
            serde_json::to_value(&recent).unwrap()
        );
        rt.drain();
    }

    /// Check by server, backend and repository: each `only` rule judged against a new
    /// shed of that backend and repository, a project file written at stage 2, and no
    /// value in what comes back.
    #[tokio::test]
    async fn a_target_preview_applies_every_rule() {
        let dir = tempfile::tempdir().unwrap();
        for (name, text) in [("npmrc", "SENTINEL-HOME"), ("env", "SENTINEL-PROJECT")] {
            let f = dir.path().join(name);
            std::fs::write(&f, text).unwrap();
            std::fs::set_permissions(&f, std::fs::Permissions::from_mode(0o600)).unwrap();
        }
        let settings_path = dir.path().join("delivery.toml");
        std::fs::write(
            &settings_path,
            "[files]\nenabled = true\nonly = { servers = [\"mini\"] }\n\
             [[files.file]]\nfrom = \"~/npmrc\"\nto = \".npmrc\"\nonly = { backends = [\"vz\"] }\n\
             [[files.file]]\nfrom = \"~/env\"\nto = \".env\"\nbase = \"project\"\n\
             only = { repos = [\"example/*\"] }\n",
        )
        .unwrap();
        let opts = DeliveryOptions {
            shed_config_path: PathBuf::new(),
            settings_path,
            key_path: dir.path().join("key"),
            known_hosts: String::new(),
            mock_base_url: None,
            unreachable_hosts: Default::default(),
            reader: DiskReader::new(dir.path(), None),
            rescan: RESCAN,
            service_recheck: shed_source::delivery::SERVICE_RECHECK,
            paused_path: dir.path().join("paused.json"),
        };
        let machine = MachineIdentity {
            id: "5f0c6e2a9b8d4c1e7a3f2b6d8e9c0a1b".into(),
            name: "ada laptop".into(),
        };
        let rt = tokio::task::spawn_blocking(move || {
            DeliveryRuntime::start(&tokio::runtime::Handle::current(), opts, Ok(machine), None)
        })
        .await
        .unwrap();
        let target = |backend: &str, repo: Option<&str>| PreviewOf::Target {
            backend: backend.into(),
            repo: repo.map(str::to_string),
        };
        let names = |v: &Value| -> Vec<String> {
            v["preview"]["items"]
                .as_array()
                .unwrap()
                .iter()
                .map(|i| i["name"].as_str().unwrap().to_string())
                .collect()
        };
        let both = rt
            .preview("mini", target("vz", Some("example/widgets")), None)
            .await
            .unwrap();
        assert_eq!(names(&both), ["files..npmrc", "files..env"]);
        assert_eq!(both["shed"], Value::Null);
        assert_eq!(both["request"]["backend"], "vz");
        assert_eq!(both["request"]["repo"], "example/widgets");
        assert_eq!(both["request"]["project_dir"], "/home/shed/widgets");
        let env = &both["preview"]["items"][1];
        assert_eq!(
            (env["stage"].clone(), env["base"].clone()),
            (json!(2), json!("project"))
        );
        assert!(!both.to_string().contains("SENTINEL"), "{both}");
        // Another backend, another repository, no repository, another server.
        let home = rt
            .preview("mini", target("remote", Some("example/widgets")), None)
            .await
            .unwrap();
        assert_eq!(names(&home), ["files..env"]);
        let none = rt.preview("mini", target("vz", None), None).await.unwrap();
        assert_eq!(names(&none), ["files..npmrc"]);
        let other = rt
            .preview("other", target("vz", Some("example/widgets")), None)
            .await
            .unwrap();
        assert!(names(&other).is_empty(), "{other}");
        // What it ran is in the answer; held to another list it runs nothing.
        assert_eq!(both["runs"], json!([]));
        let e = rt
            .preview("mini", target("vz", None), Some(vec!["gh".into()]))
            .await
            .unwrap_err();
        assert_eq!(e.0, "settings_changed");
        rt.preview("mini", target("vz", None), Some(vec![]))
            .await
            .unwrap();
        // A target the request cannot be drawn for is refused, not guessed.
        for (b, r) in [("docker", None), ("vz", Some("widgets"))] {
            let e = rt.preview("mini", target(b, r), None).await.unwrap_err();
            assert_eq!(e.0, "bad_request", "{b} {r:?}");
        }
        rt.drain();
    }

    fn two_server_opts(dir: &std::path::Path, servers: &[&str]) -> DeliveryOptions {
        let cfg = dir.join("config.yaml");
        let mut text = String::from("servers:\n");
        for s in servers {
            text.push_str(&format!("    {s}:\n        host: {s}.example.test\n"));
        }
        std::fs::write(&cfg, text).unwrap();
        DeliveryOptions {
            shed_config_path: cfg,
            settings_path: dir.join("delivery.toml"),
            key_path: dir.join("key"),
            known_hosts: String::new(),
            // A closed port: the streams run (and fail), which is all this needs.
            mock_base_url: Some("http://127.0.0.1:1".into()),
            unreachable_hosts: Default::default(),
            reader: DiskReader::new(dir, None),
            rescan: RESCAN,
            service_recheck: shed_source::delivery::SERVICE_RECHECK,
            paused_path: dir.join("broker").join("paused-servers.json"),
        }
    }

    /// "Answer from here" after the server's endpoint changed: a rescan first, and the
    /// resumed stream runs on the new endpoint only. Without it (the negative control)
    /// the stream comes back on the endpoint the runtime last read.
    #[tokio::test]
    async fn a_rescan_before_resume_opens_the_stream_on_the_current_endpoint() {
        let dir = tempfile::tempdir().unwrap();
        let mut opts = two_server_opts(dir.path(), &["x"]);
        opts.mock_base_url = None;
        let cfg = opts.shed_config_path.clone();
        let at = |host: &str| {
            std::fs::write(&cfg, format!("servers:\n    x:\n        host: {host}\n")).unwrap()
        };
        at("a.example.test");
        let rt = started(opts).await;
        let url = |rt: &DeliveryRuntime| rt.stream_target("x").map(|t| t.url);
        assert!(url(&rt).unwrap().contains("a.example.test"));
        rt.pause("x").unwrap();
        assert!(rt.wait_stopped("x", Duration::from_secs(5)).await);
        at("b.example.test");
        // Negative control: resumed on what was last read, the old endpoint.
        rt.resume("x").unwrap();
        assert!(url(&rt).unwrap().contains("a.example.test"));
        rt.pause("x").unwrap();
        assert!(rt.wait_stopped("x", Duration::from_secs(5)).await);
        // Reconciled first, then resumed: the new endpoint only.
        rt.rescan_now().unwrap();
        assert_eq!(url(&rt), None, "still paused: no stream");
        rt.resume("x").unwrap();
        let now = url(&rt).unwrap();
        assert!(
            now.contains("b.example.test") && !now.contains("a.example.test"),
            "{now}"
        );
        // A config that cannot be read is the error, and the stream keeps running.
        std::fs::write(
            &cfg,
            "servers:\n    x:\n        host: b.example.test\nservers:\n",
        )
        .unwrap();
        assert!(rt.rescan_now().is_err());
        assert!(url(&rt).unwrap().contains("b.example.test"));
        rt.drain();
    }

    async fn started(opts: DeliveryOptions) -> Arc<DeliveryRuntime> {
        let machine = MachineIdentity {
            id: "5f0c6e2a9b8d4c1e7a3f2b6d8e9c0a1b".into(),
            name: "ada laptop".into(),
        };
        tokio::task::spawn_blocking(move || {
            DeliveryRuntime::start(&tokio::runtime::Handle::current(), opts, Ok(machine), None)
        })
        .await
        .unwrap()
    }

    fn running(rt: &DeliveryRuntime) -> Vec<String> {
        rt.streams().into_iter().map(|s| s.server).collect()
    }

    /// "Stop answering from here" stops that server's stream and keeps it stopped: across
    /// a rescan, across a restart (the set is on disk), and across the server leaving the
    /// config and coming back, until "Answer from here".
    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn a_paused_server_runs_no_stream_until_it_is_resumed() {
        let dir = tempfile::tempdir().unwrap();
        let opts = two_server_opts(dir.path(), &["a", "b"]);
        let rt = started(opts.clone()).await;
        assert_eq!(running(&rt), ["a", "b"]);

        rt.pause("a").unwrap();
        assert!(rt.is_paused("a") && !rt.is_paused("b"));
        assert!(rt.wait_stopped("a", Duration::from_secs(10)).await);
        assert_eq!(running(&rt), ["b"]);
        assert_eq!(rt.status()["paused"], json!(["a"]));
        // A rescan of the same config does not bring it back.
        rt.reconcile(rt.tasks.as_ref().unwrap());
        assert_eq!(running(&rt), ["b"]);
        rt.drain();

        // A restart reads the set back.
        let rt = started(opts.clone()).await;
        assert_eq!(running(&rt), ["b"]);
        rt.resume("a").unwrap();
        assert_eq!(running(&rt), ["a", "b"]);
        assert!(!rt.is_paused("a"));

        // A pause survives the server leaving the config and coming back: it lasts
        // until "Answer from here", and the name is inert meanwhile.
        rt.pause("a").unwrap();
        two_server_opts(dir.path(), &["b"]);
        rt.reconcile(rt.tasks.as_ref().unwrap());
        assert_eq!(running(&rt), ["b"]);
        assert!(rt.is_paused("a"), "kept while the server is not configured");
        assert_eq!(
            rt.status()["paused"],
            json!([]),
            "an inert name lists no server"
        );
        two_server_opts(dir.path(), &["a", "b"]);
        rt.reconcile(rt.tasks.as_ref().unwrap());
        assert_eq!(running(&rt), ["b"], "added back, still paused");
        assert_eq!(rt.status()["paused"], json!(["a"]));
        assert_eq!(
            shed_source::paused::PausedServers::load(opts.paused_path.clone()).names(),
            ["a"]
        );
        // Only "Answer from here" lifts it.
        rt.resume("a").unwrap();
        assert_eq!(running(&rt), ["a", "b"]);
        rt.drain();
        assert_eq!(
            shed_source::paused::PausedServers::load(opts.paused_path).names(),
            Vec::<String>::new()
        );
    }

    /// Finding 1: a paused set whose file exists and cannot be used starts no stream at
    /// all, and says why; "Answer from here" on one server writes the file afresh and
    /// starts that server only.
    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn an_unusable_paused_file_starts_no_stream() {
        let dir = tempfile::tempdir().unwrap();
        let opts = two_server_opts(dir.path(), &["a", "b"]);
        std::fs::create_dir_all(opts.paused_path.parent().unwrap()).unwrap();
        std::fs::write(&opts.paused_path, "{not json").unwrap();
        let rt = started(opts.clone()).await;
        assert!(running(&rt).is_empty());
        assert_eq!(rt.paused_error(), Some(PausedError::Unparseable));
        assert_eq!(rt.status()["paused"], json!(["a", "b"]));
        assert_eq!(rt.status()["paused_error"], json!("unparseable"));
        assert!(rt.resume("b").unwrap());
        assert_eq!(running(&rt), ["b"]);
        assert_eq!(rt.paused_error(), None);
        assert_eq!(rt.status()["paused"], json!(["a"]));
        rt.drain();
    }
}
