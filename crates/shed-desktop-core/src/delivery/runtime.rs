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
//! # Beside the broker (plan 039 PR A)
//!
//! The embedded broker keeps running its bus. Both use this machine's one identity
//! (`<state>/broker/machine.json`, loaded with the same file lock) and, on a secure
//! server, the same durable credentials-scope store (`<state>/broker/creds`):
//! [`shed_source::credentials::SourceCredentials`] presents whatever certificate is
//! stored and mints only when the stored one will not do, so the two do not mint twice
//! for one certificate. Store writes are serialized by the store's own lock, held only
//! for each write.
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
use shed_source::delivery::{AnswerOutcome, Answerer, NoCredentials, StreamCredentials};
use shed_source::machine::MachineIdentity;
use shed_source::minter::{CredentialMinter, Minter};
use shed_source::tasks::SourceTasks;
use shed_source::ServerTarget;
use tokio::sync::watch;

use super::reader::DiskReader;
use super::settings_file;

/// How often the server list is read again.
pub const RESCAN: Duration = Duration::from_secs(30);
/// How many answers [`DeliveryRuntime::status`] keeps.
pub const RECENT: usize = 32;
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
    /// Test mode: every server at this URL.
    pub mock_base_url: Option<String>,
    /// Test mode: servers pointed at a closed port.
    pub unreachable_hosts: HashSet<String>,
    /// What providers read through.
    pub reader: DiskReader,
    /// How often the server list is read again.
    pub rescan: Duration,
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
        match shed_provision_providers::build_answer(&settings, request, server, &self.reader, key)
        {
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
    config_error: Arc<Mutex<Option<String>>>,
    stop: watch::Sender<bool>,
}

impl DeliveryRuntime {
    /// Starts the runtime on `handle`: loads this machine's identity (`machine`, made by
    /// the caller with `shed_source::machine::load_or_create`) and the fingerprint key,
    /// reads the servers and starts a stream for each, and reads the list again every
    /// [`DeliveryOptions::rescan`]. Without an identity no stream runs (the status says
    /// why). Blocking file I/O: call it off an async worker.
    pub fn start(
        handle: &tokio::runtime::Handle,
        opts: DeliveryOptions,
        machine: Result<MachineIdentity, String>,
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
            Arc::new(SourceTasks::new(
                handle.clone(),
                m.clone(),
                Arc::new(Answering(answerer.clone())),
                credentials,
            ))
        });
        let rt = Arc::new(DeliveryRuntime {
            opts,
            machine,
            answerer,
            tasks,
            config_error: Arc::new(Mutex::new(None)),
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

    fn apply(&self, tasks: &SourceTasks, targets: Result<Vec<ServerTarget>, String>) {
        let mut err = self.config_error.lock().unwrap_or_else(|e| e.into_inner());
        match targets {
            Ok(t) => {
                *err = None;
                tasks.reconcile(&t);
            }
            // A config that cannot be read keeps the streams that run.
            Err(e) => *err = Some(e),
        }
    }

    /// `delivery.status`: this machine, the settings file, each server's stream, and the
    /// recent answers, redacted.
    pub fn status(&self) -> Value {
        let (machine, machine_error) = match &self.machine {
            Ok(m) => (json!({"id": m.id, "name": m.name}), Value::Null),
            Err(e) => (Value::Null, Value::from(e.clone())),
        };
        let servers = self
            .tasks
            .as_ref()
            .map(|t| serde_json::to_value(t.status()).unwrap_or_default())
            .unwrap_or_else(|| json!([]));
        let recent: Vec<Answered> = self
            .answerer
            .recent
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .iter()
            .rev()
            .cloned()
            .collect();
        json!({
            "machine": machine,
            "machine_error": machine_error,
            "key_error": self.answerer.key.as_ref().err(),
            "config_error": self.config_error.lock().unwrap_or_else(|e| e.into_inner()).clone(),
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

    /// `delivery.preview`: what the shed of `doc` (its document from `server`) would be
    /// delivered, or, with no document, what the providers have at all (every `only` rule
    /// let through). Redacted: no value, no content.
    pub async fn preview(
        &self,
        server: &str,
        doc: Option<Value>,
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
            match doc {
                None => {
                    let p = shed_provision_providers::preview(&settings, &answerer.reader, &key);
                    Ok(json!({"server": server, "shed": null, "preview": p}))
                }
                Some(doc) => {
                    let deadline =
                        shed_provision::format_time(shed_source::time::now_unix() + 3600);
                    let request = super::preview::request_for(&doc, Reason::Provision, &deadline)
                        .map_err(|e| ("bad_shed".to_string(), e))?;
                    let built = shed_provision_providers::build_answer(
                        &settings,
                        &request,
                        &server,
                        &answerer.reader,
                        &key,
                    )
                    .map_err(|r| ("preview_failed".to_string(), r.to_string()))?;
                    Ok(json!({
                        "server": server,
                        "shed": request.shed.name,
                        "request": {
                            "backend": request.shed.backend,
                            "repo": request.repo_name(),
                            "local_dir": request.shed.local_dir,
                            "project_dir": request.shed.project_dir,
                        },
                        "preview": built.preview,
                    }))
                }
            }
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
                mock.clone()
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
}
