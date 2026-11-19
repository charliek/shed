//! The multi-server supervisor — a faithful port of Go's `supervisor.go`.
//!
//! The daemon runs ONE [`Supervisor`] for BOTH modes (single-server = a discovery
//! config that reconciles once and never reloads; the single unnamed target has
//! `ssh_host=""` → [`should_mint`] false → open/no-pin, behavior-identical to the old
//! single-server bus). [`Supervisor::reconcile`] diffs a desired `Vec<ServerTarget>`
//! against the running per-server [`WatcherGroup`]s by name and starts/stops/restarts
//! groups; [`Supervisor::shutdown`] drains them all; [`Supervisor::health`] snapshots the
//! per-namespace connection state for `LiveStatus.servers[]`.
//!
//! [`SharedDeps`] are the server-agnostic seams (SSH/AWS/Docker backends, the
//! per-namespace approval gates, the audit sink, the credential minter, the log) built
//! ONCE in `main.rs` and injected into every group. The actual group machinery lives in
//! [`crate::bus::spawn_server_group`] (the generalized former `run_single_server_bus`);
//! this module owns the reconcile diff + the lifecycle invariants (cancel-under-lock /
//! drain-off-lock, restart-on-cred-or-TLS-change, warn-empty-once, post-shutdown no-op).

use std::collections::HashMap;
use std::sync::{Arc, Mutex};

use tokio::sync::watch;
use tokio::task::JoinHandle;

use crate::approval::ApprovalGate;
use crate::audit::AuditSink;
use crate::bus::{
    AwsHandlers, BusError, BusLog, ConnState, DockerHandlers, MintRefusal, SubStatus, TokenProvider,
};
use crate::discovery::ServerTarget;
use crate::machine::MachineIdentity;
use crate::minter::{CredentialSource, Minter};
use crate::sign_check::SignPolicy;
use crate::ssh_backend::SshBackend;
use crate::status::{NamespaceHealth, ServerHealth};

/// The bus token provider for a secure server: `Arc<CredentialSource>` bridged onto
/// [`TokenProvider`] (the credentials-scope wire the minter + egress slices each deferred
/// to "the supervisor slice"). `token()` maps the source's `Result<String, String>` onto
/// `Result<String, BusError>`; `invalidate()` clears the cache after a 401.
///
/// **Coherence:** `Arc<CredentialSource>` now carries TWO crate-local trait impls — this
/// `TokenProvider` (credentials scope, the bus) AND `EgressTokenSource` (control scope, the
/// egress side task, `egress.rs`). Both traits are crate-local, so there is no coherence
/// conflict; they are distinct traits selected by context (the bus client vs the egress
/// subscriber), NOT a duplicate impl.
///
/// The impl is on `Arc<CredentialSource>` (not `CredentialSource`) because
/// `CredentialSource::token` takes `self: &Arc<Self>` (it spawns the mint task off an owned
/// clone) — the same shape as the egress bridge.
#[async_trait::async_trait]
impl TokenProvider for Arc<CredentialSource> {
    async fn token(&self) -> Result<String, BusError> {
        // UFCS selects the inherent `CredentialSource::token(self: &Arc<Self>)` — a bare
        // `self.token()` would recurse into THIS trait method.
        CredentialSource::token(self)
            .await
            .map_err(BusError::Config)
    }
    fn invalidate(&self) {
        // Deref past the Arc to the inherent method (the trait is impl'd on `Arc<_>`, so a
        // bare `self.invalidate()` would recurse into THIS method).
        (**self).invalidate()
    }
    fn surviving_token(&self) -> Option<String> {
        (**self).surviving_bearer()
    }
    fn presenting_certificate(&self) -> bool {
        (**self).holds_certificate()
    }
    fn would_mint(&self) -> bool {
        (**self).would_mint()
    }
    async fn token_for_request(&self) -> Result<String, (BusError, Option<MintRefusal>)> {
        CredentialSource::token_for_request(self)
            .await
            .map_err(|e| {
                let refusal = e.refused.then(|| MintRefusal {
                    message: e.message.clone(),
                    held_user: e.held_user,
                });
                (BusError::Config(e.message), refusal)
            })
    }
}

/// The server-agnostic components every per-server watcher group shares — built once in
/// `main.rs` and injected into each group (mirror Go's `SharedDeps`). AWS/Docker are
/// `None` when their backend isn't configured; `minter` is `None` only when minting is
/// disabled (production always builds it, so [`should_mint`] then keys purely on the
/// target's https-scheme + SSH endpoint).
pub struct SharedDeps {
    pub ssh_backend: Arc<dyn SshBackend>,
    pub ssh_gate: Arc<dyn ApprovalGate>,
    /// What this machine signs for a shed (plan 035 §3.5): every `sign` request is checked
    /// against it before the approval gate. Built once from
    /// [`crate::config::HostAgentConfig::ssh_sign_policy`] at start, by both hosts.
    pub ssh_sign_policy: Arc<SignPolicy>,
    pub aws: Option<AwsHandlers>,
    pub docker: Option<DockerHandlers>,
    pub audit: Arc<dyn AuditSink>,
    /// Mints the per-server credentials/control tokens over SSH (secure mode). `None`
    /// disables minting → every server uses its (usually empty) configured static token.
    pub minter: Option<Arc<dyn Minter>>,
    pub log: Arc<dyn BusLog>,
    /// This machine's identity ([`crate::machine`]), sent on every bus request so a
    /// pinning server can tell this machine from another of the same user's. `None` when
    /// it could not be loaded: the bus then sends no identity headers, exactly as before.
    pub machine: Option<MachineIdentity>,
    /// Whether the desktop app's embedded broker runs on this machine
    /// ([`crate::presence`]): the channel the daemon's one lock watcher
    /// ([`crate::presence::LockWatcher`]) publishes on. While it reads present every bus
    /// client stands by: it subscribes nothing and answers nothing, and work it received
    /// before a start of the app (an older yield epoch) is never answered by it
    /// ([`crate::bus::BusClient::with_presence`]). `Some` in the shed-host-agent daemon;
    /// `None` in the embedded broker itself, which holds that lock and never stands by,
    /// and in every other host.
    pub embedded_presence: Option<tokio::sync::watch::Receiver<crate::presence::Presence>>,
    /// When this process took the presence lock (`None` until it has): set only by the
    /// desktop's embedded broker, which holds it ([`crate::presence::Holder`]); every
    /// other host leaves it `None`. Its one use: the older-server grace
    /// ([`crate::bus::BusClient::with_presence_lock`]).
    pub presence_lock: Option<tokio::sync::watch::Receiver<Option<tokio::time::Instant>>>,
}

/// Whether the agent should self-mint a token for `target` (attaching a token provider +
/// TLS pin) rather than send its configured static token — a faithful port of Go's
/// `shouldMint` (`supervisor.go:46`). Minting is warranted only for a SECURE server, whose
/// authoritative local signal is an **https** api_url ([`ServerTarget::is_secure`], the
/// scheme — NOT `tls_fingerprint` presence, NOT `ssh_port > 0` alone; every shed server has
/// an SSH endpoint, incl. open-mode ones). It also needs a usable SSH endpoint to mint over
/// and a configured minter.
pub fn should_mint(deps: &SharedDeps, target: &ServerTarget) -> bool {
    deps.minter.is_some()
        && !target.ssh_host.is_empty()
        && target.ssh_port > 0
        && target.is_secure()
}

/// One shed server's running watcher group: its `target` (the diff key + the
/// credential-bearing fields), the `cancel` handle (an individual reconcile drop), the
/// `done` join (drained after releasing the lock), and the per-namespace status handles
/// `health()` reads. Mirror Go's `watcherGroup`.
struct WatcherGroup {
    target: ServerTarget,
    cancel: Arc<watch::Sender<bool>>,
    done: JoinHandle<()>,
    statuses: Vec<Arc<Mutex<SubStatus>>>,
    /// Bumped by [`Supervisor::retry_now`] to cut the group's waiting backoffs short.
    wake: Arc<watch::Sender<u64>>,
}

/// The group factory (mirror Go's overridable `newGroup` field): builds + spawns a group
/// for a target. The production factory calls [`crate::bus::spawn_server_group`]; tests
/// inject a fake that records lifecycle without real HostClients/SSE.
type GroupFactory =
    Box<dyn Fn(watch::Receiver<bool>, ServerTarget, &SharedDeps) -> WatcherGroup + Send + Sync>;

/// Reconciles the running per-server watcher groups against a desired set. Safe for
/// concurrent `reconcile`/`shutdown` (mirror Go's `Supervisor`).
pub struct Supervisor {
    /// The daemon-wide shutdown watch, threaded into each group so a SIGTERM also tears
    /// the groups down (Go's `context.WithCancel(parent)`).
    parent: watch::Receiver<bool>,
    deps: SharedDeps,
    new_group: GroupFactory,
    inner: Mutex<Inner>,
}

struct Inner {
    groups: HashMap<String, WatcherGroup>,
    closed: bool,
    was_empty: bool,
}

impl Supervisor {
    /// Create a supervisor bound to the daemon shutdown watch + shared deps, using the
    /// production group factory ([`crate::bus::spawn_server_group`]).
    pub fn new(parent: watch::Receiver<bool>, deps: SharedDeps) -> Supervisor {
        let factory: GroupFactory = Box::new(|parent, target, deps| {
            let h = crate::bus::spawn_server_group(parent, &target, deps);
            WatcherGroup {
                target,
                cancel: h.cancel,
                done: h.done,
                statuses: h.statuses,
                wake: h.wake,
            }
        });
        Supervisor::with_factory(parent, deps, factory)
    }

    fn with_factory(
        parent: watch::Receiver<bool>,
        deps: SharedDeps,
        new_group: GroupFactory,
    ) -> Supervisor {
        Supervisor {
            parent,
            deps,
            new_group,
            inner: Mutex::new(Inner {
                groups: HashMap::new(),
                closed: false,
                was_empty: false,
            }),
        }
    }

    /// Diff `desired` against the running groups by name: STOP a group whose name is gone
    /// OR whose whole `ServerTarget` changed (URL **AND** token **AND** tls_fingerprint
    /// **AND** ssh_host/ssh_port — a rotated/added credentials token or a newly-added TLS
    /// pin on the SAME url MUST restart the watcher so it takes effect); START groups for
    /// new names; leave unchanged groups running (a no-op config rewrite causes no churn).
    /// Cancel UNDER the lock (fast), then DRAIN after releasing it so a slow handler can't
    /// block other reconciles. `warn_empty` fires ONCE when the desired set FIRST becomes
    /// empty. Idempotent; a reconcile after [`shutdown`](Self::shutdown) is a no-op.
    pub async fn reconcile(&self, desired: Vec<ServerTarget>) {
        let (draining, warn_empty) = {
            let mut inner = self.inner.lock().unwrap();
            if inner.closed {
                return;
            }

            // Dedup by name (last occurrence wins in the map, matching Go's `want[t.Name]=t`
            // loop). `resolve_targets` already deduped first-wins, so this only collapses a
            // pathological same-name repeat.
            let mut want: HashMap<String, ServerTarget> = HashMap::with_capacity(desired.len());
            for t in desired {
                want.insert(t.name.clone(), t);
            }

            // Stop removed or changed groups (whole-target compare). Cancel under the lock;
            // collect into `draining` and drain after the lock is released.
            let mut draining: Vec<WatcherGroup> = Vec::new();
            let running: Vec<String> = inner.groups.keys().cloned().collect();
            for name in running {
                // `name` is iterated from `inner.groups.keys()`, so the lookup is
                // infallible: stop when the target is gone from `want`, or its whole
                // struct changed (restart-on-change).
                let stop = match want.get(&name) {
                    None => true,
                    Some(t) => *t != inner.groups[&name].target,
                };
                if stop {
                    if let Some(g) = inner.groups.remove(&name) {
                        self.deps.log.info(&format!(
                            "stopping server watcher server={} url={}",
                            name, g.target.url
                        ));
                        let _ = g.cancel.send(true);
                        draining.push(g);
                    }
                }
            }

            // Start new (or restarted-on-change) groups.
            for (name, t) in &want {
                if !inner.groups.contains_key(name) {
                    let g = (self.new_group)(self.parent.clone(), t.clone(), &self.deps);
                    inner.groups.insert(name.clone(), g);
                }
            }

            // Warn only when the desired set NEWLY becomes empty (was_empty starts false).
            let empty = inner.groups.is_empty();
            let warn_empty = empty && !inner.was_empty;
            inner.was_empty = empty;
            (draining, warn_empty)
        };

        for g in draining {
            let _ = g.done.await;
        }
        if warn_empty {
            self.deps
                .log
                .warn("no servers to watch (discovery returned an empty set)");
        }
    }

    /// Cancel all groups and wait for them to drain. After shutdown, further `reconcile`
    /// calls are no-ops (mirror Go's `Shutdown`).
    pub async fn shutdown(&self) {
        let groups = {
            let mut inner = self.inner.lock().unwrap();
            inner.closed = true;
            let groups = std::mem::take(&mut inner.groups);
            for g in groups.values() {
                let _ = g.cancel.send(true);
            }
            groups
        };
        for (_, g) in groups {
            let _ = g.done.await;
        }
    }

    /// The SSH backend every group this supervisor starts answers `ssh-agent` requests
    /// with (so a host can check which backend it installed).
    pub fn ssh_backend(&self) -> Arc<dyn SshBackend> {
        self.deps.ssh_backend.clone()
    }

    /// The signing policy every group this supervisor starts checks `sign` requests
    /// against (so a host can check which policy it installed).
    pub fn ssh_sign_policy(&self) -> Arc<SignPolicy> {
        self.deps.ssh_sign_policy.clone()
    }

    /// The running daemon's per-server connection snapshot for `status`: each watched
    /// server with its per-namespace subscription state (incl. the 409 `Rejected`
    /// terminal), sorted by name (mirror Go's `Health`). The lock is released before
    /// reading each status handle so a slow reader can't stall reconciles; a client-less
    /// (test) group yields no namespaces.
    pub fn health(&self) -> Vec<ServerHealth> {
        // Snapshot the identity + status handles under the lock, then read each handle
        // off-lock (a slow reader can't stall reconciles).
        struct GroupRef {
            name: String,
            url: String,
            statuses: Vec<Arc<Mutex<SubStatus>>>,
        }
        let refs: Vec<GroupRef> = {
            let inner = self.inner.lock().unwrap();
            inner
                .groups
                .values()
                .map(|g| GroupRef {
                    name: g.target.name.clone(),
                    url: g.target.url.clone(),
                    statuses: g.statuses.clone(),
                })
                .collect()
        };
        let mut out: Vec<ServerHealth> = refs
            .into_iter()
            .map(
                |GroupRef {
                     name,
                     url,
                     statuses,
                 }| {
                    let mut namespaces: Vec<NamespaceHealth> = statuses
                        .iter()
                        .map(|s| {
                            let st = s.lock().unwrap();
                            NamespaceHealth {
                                namespace: st.namespace.clone(),
                                state: conn_state_str(st.state).to_string(),
                                last_error: st.last_error.clone(),
                                // The RFC3339-UTC instant the current state began (Go's
                                // `st.Since.UTC().Format(RFC3339)`); the `servers[]` differential
                                // masks the value after shape-asserting it.
                                since: st.since.clone(),
                                active_source: st.active_source.clone(),
                            }
                        })
                        .collect();
                    // Sort by namespace to match Go's `HostClient.Status()`, which
                    // `sort.Slice`s by namespace before the supervisor reads it — so
                    // `servers[].namespaces[]` is in the same order on both impls regardless of
                    // subscribe order (ssh-agent, docker-credentials, ...).
                    namespaces.sort_by(|a, b| a.namespace.cmp(&b.namespace));
                    // Any namespace refused as not-source makes the whole server read as
                    // not the source (a server binds and moves all of a user's
                    // namespaces together); the first one, in namespace order, names it.
                    let active_source = namespaces.iter().find_map(|n| n.active_source.clone());
                    ServerHealth {
                        name,
                        url,
                        namespaces,
                        active_source,
                    }
                },
            )
            .collect();
        out.sort_by(|a, b| a.name.cmp(&b.name));
        out
    }

    /// Cut every waiting subscription backoff short, on every server, so a machine that
    /// has just been made the source connects at once rather than within 30 s. A no-op
    /// for a subscription that is connected or mid-request (its next wait returns at
    /// once instead).
    pub fn retry_now(&self) {
        let inner = self.inner.lock().unwrap();
        for g in inner.groups.values() {
            g.wake.send_modify(|n| *n = n.wrapping_add(1));
        }
    }
}

/// The wire string for a connection state (mirror the Go SDK's `Conn*` constants, which
/// `SubStatus.State` carries verbatim; `not-source` and `duplicate` are plan 035's).
fn conn_state_str(state: ConnState) -> &'static str {
    match state {
        ConnState::Reconnecting => "reconnecting",
        ConnState::Connected => "connected",
        ConnState::Stopped => "stopped",
        ConnState::Rejected => "rejected",
        ConnState::NotSource => "not-source",
        ConnState::Duplicate => "duplicate",
        ConnState::Yielding => "yielding",
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::bus::FileBusLog;
    use crate::discovery::ServerTarget;
    use crate::minter::{Minted, MinterError};
    use crate::status::ActiveSource;
    use std::sync::atomic::{AtomicUsize, Ordering};

    // ---- test stubs: the fake factory never touches the heavy deps, so these satisfy
    // the SharedDeps types without a live bus. ---------------------------------------

    struct NoopSshBackend;
    impl SshBackend for NoopSshBackend {
        fn list(&self) -> Result<Vec<crate::ssh_backend::SshKeyInfo>, String> {
            Ok(Vec::new())
        }
        fn sign(
            &self,
            _public_key: &[u8],
            _data: &[u8],
            _flags: u32,
        ) -> Result<crate::ssh_backend::SshSignature, String> {
            Err("noop".into())
        }
        fn mode(&self) -> &str {
            "local-keys"
        }
    }

    struct NoopAuditSink;
    impl AuditSink for NoopAuditSink {
        fn log(&self, _entry: crate::audit::AuditEntry) {}
    }

    /// A non-dialed minter (never actually mints in these tests — only its presence
    /// matters to `should_mint`).
    struct NoopMinter;
    #[async_trait::async_trait]
    impl Minter for NoopMinter {
        async fn mint(&self, _t: &ServerTarget, _scope: &str) -> Result<Minted, MinterError> {
            unreachable!("should_mint tests never mint");
        }
    }

    fn test_log() -> Arc<dyn BusLog> {
        Arc::new(FileBusLog::new(""))
    }

    fn deps_with_minter(minter: Option<Arc<dyn Minter>>) -> SharedDeps {
        SharedDeps {
            ssh_backend: Arc::new(NoopSshBackend),
            ssh_gate: Arc::new(crate::approval::DenyAllGate),
            ssh_sign_policy: Arc::new(SignPolicy::default()),
            aws: None,
            docker: None,
            audit: Arc::new(NoopAuditSink),
            minter,
            log: test_log(),
            machine: None,
            embedded_presence: None,
            presence_lock: None,
        }
    }

    fn tgt(name: &str, url: &str) -> ServerTarget {
        ServerTarget {
            name: name.into(),
            url: url.into(),
            token: String::new(),
            tls_fingerprint: String::new(),
            ssh_host: String::new(),
            ssh_port: 0,
            auth_mode: String::new(),
            user: String::new(),
            identity_file: String::new(),
        }
    }

    // ---- fake group factory (mirror supervisor_test.go's fakeGroups) -----------------

    #[derive(Default)]
    struct FakeGroups {
        started: Mutex<Vec<ServerTarget>>,
        stopped: Mutex<Vec<String>>,
        /// One wake shared by every fake group, so a test can watch `retry_now` reach it.
        wake: Arc<watch::Sender<u64>>,
    }

    impl FakeGroups {
        fn factory(self: &Arc<Self>, target: ServerTarget) -> WatcherGroup {
            self.started.lock().unwrap().push(target.clone());
            let (cancel_tx, mut cancel_rx) = watch::channel(false);
            let name = target.name.clone();
            let fg = self.clone();
            // Records `stopped` once its group cancel flips — the supervisor cancels every
            // group explicitly on reconcile-drop AND on shutdown (the parent-shutdown wiring
            // is the real `spawn_server_group` factory's job, exercised by the harness, not
            // this fake).
            let done = tokio::spawn(async move {
                let _ = cancel_rx.wait_for(|c| *c).await;
                fg.stopped.lock().unwrap().push(name);
            });
            WatcherGroup {
                target,
                cancel: Arc::new(cancel_tx),
                done,
                statuses: Vec::new(),
                wake: self.wake.clone(),
            }
        }

        fn start_count(&self) -> usize {
            self.started.lock().unwrap().len()
        }

        fn stopped_names(&self) -> Vec<String> {
            let mut v = self.stopped.lock().unwrap().clone();
            v.sort();
            v
        }
    }

    fn test_supervisor(fg: &Arc<FakeGroups>) -> Supervisor {
        let (_tx, rx) = watch::channel(false);
        let fg = fg.clone();
        let factory: GroupFactory = Box::new(move |_parent, target, _deps| fg.factory(target));
        Supervisor::with_factory(rx, deps_with_minter(None), factory)
    }

    fn group_names(s: &Supervisor) -> Vec<String> {
        let inner = s.inner.lock().unwrap();
        let mut names: Vec<String> = inner.groups.keys().cloned().collect();
        names.sort();
        names
    }

    // ---- should_mint matrix (mirror TestShouldMint) ----------------------------------

    fn secure_target() -> ServerTarget {
        ServerTarget {
            name: "s".into(),
            url: "https://h:8443".into(),
            token: String::new(),
            tls_fingerprint: String::new(),
            ssh_host: "h".into(),
            ssh_port: 2222,
            auth_mode: String::new(),
            user: String::new(),
            identity_file: String::new(),
        }
    }

    #[test]
    fn should_mint_matrix() {
        let minter: Arc<dyn Minter> = Arc::new(NoopMinter);
        let with = deps_with_minter(Some(minter));
        let without = deps_with_minter(None);

        // https + ssh + minter → mint.
        assert!(should_mint(&with, &secure_target()));
        // HTTPS uppercase normalized → mint.
        let mut up = secure_target();
        up.url = "HTTPS://h:8443".into();
        assert!(should_mint(&with, &up));
        // http is open → no mint.
        let mut http = secure_target();
        http.url = "http://h:8080".into();
        assert!(!should_mint(&with, &http));
        // empty url → no mint.
        let mut empty = secure_target();
        empty.url = String::new();
        assert!(!should_mint(&with, &empty));
        // nil minter → no mint.
        assert!(!should_mint(&without, &secure_target()));
        // no ssh host → no mint.
        let mut no_host = secure_target();
        no_host.ssh_host = String::new();
        assert!(!should_mint(&with, &no_host));
        // no ssh port → no mint.
        let mut no_port = secure_target();
        no_port.ssh_port = 0;
        assert!(!should_mint(&with, &no_port));
    }

    /// The Rust half of the `should_mint` golden — reads the SAME shared fixture the Go
    /// runner reads (`cmd/shed-host-agent/golden_test.go:TestGoldenShouldMint`), so the
    /// two impls can't drift on the https-scheme / ssh-endpoint / minter-presence matrix.
    #[test]
    fn golden_should_mint() {
        let path = std::path::PathBuf::from(env!("CARGO_MANIFEST_DIR"))
            .join("../../tests/host-agent-diff/fixtures/should_mint.json");
        let raw = std::fs::read_to_string(&path).expect("read golden fixture");
        let fx: serde_json::Value = serde_json::from_str(&raw).unwrap();
        assert_eq!(fx["protocol_version"], 1, "version skew");
        let vectors = fx["vectors"].as_array().unwrap();
        assert!(!vectors.is_empty(), "fixture has no vectors");
        for v in vectors {
            let name = v["name"].as_str().unwrap();
            let minter: Option<Arc<dyn Minter>> = if v["minter_present"].as_bool().unwrap() {
                Some(Arc::new(NoopMinter))
            } else {
                None
            };
            let deps = deps_with_minter(minter);
            let target = ServerTarget {
                name: String::new(),
                url: v["url"].as_str().unwrap().to_string(),
                token: String::new(),
                tls_fingerprint: String::new(),
                ssh_host: v["ssh_host"].as_str().unwrap().to_string(),
                ssh_port: v["ssh_port"].as_u64().unwrap() as u16,
                auth_mode: String::new(),
                user: String::new(),
                identity_file: String::new(),
            };
            assert_eq!(
                should_mint(&deps, &target),
                v["expected"].as_bool().unwrap(),
                "golden vector {name:?}"
            );
        }
    }

    // ---- reconcile / shutdown / health (mirror supervisor_test.go) -------------------

    #[tokio::test]
    async fn reconcile_add_remove() {
        let fg = Arc::new(FakeGroups::default());
        let s = test_supervisor(&fg);

        s.reconcile(vec![
            tgt("mini2", "http://mini2:8080"),
            tgt("mini3", "http://mini3:8080"),
        ])
        .await;
        assert_eq!(group_names(&s).len(), 2);
        assert_eq!(fg.start_count(), 2);

        // Drop mini3.
        s.reconcile(vec![tgt("mini2", "http://mini2:8080")]).await;
        assert_eq!(group_names(&s), vec!["mini2".to_string()]);
        assert_eq!(fg.stopped_names(), vec!["mini3".to_string()]);
        // mini2 was not restarted (no churn).
        assert_eq!(fg.start_count(), 2);
    }

    #[tokio::test]
    async fn reconcile_no_churn() {
        let fg = Arc::new(FakeGroups::default());
        let s = test_supervisor(&fg);
        let target = vec![tgt("mini2", "http://mini2:8080")];
        s.reconcile(target.clone()).await;
        s.reconcile(target.clone()).await;
        s.reconcile(target).await;
        assert_eq!(fg.start_count(), 1, "idempotent reconcile");
    }

    #[tokio::test]
    async fn reconcile_url_change_restarts() {
        let fg = Arc::new(FakeGroups::default());
        let s = test_supervisor(&fg);
        s.reconcile(vec![tgt("mini2", "http://old:8080")]).await;
        s.reconcile(vec![tgt("mini2", "http://new:8080")]).await;
        assert_eq!(fg.start_count(), 2, "URL change restarts");
        assert_eq!(fg.stopped_names(), vec!["mini2".to_string()]);
        let inner = s.inner.lock().unwrap();
        assert_eq!(inner.groups["mini2"].target.url, "http://new:8080");
    }

    #[tokio::test]
    async fn reconcile_credential_or_pin_change_restarts() {
        // A rotated token OR a newly-added TLS pin on the SAME url must restart the group
        // (whole-struct compare) so the new credential takes effect.
        let fg = Arc::new(FakeGroups::default());
        let s = test_supervisor(&fg);
        let mk = |token: &str, pin: &str| ServerTarget {
            name: "s".into(),
            url: "https://h:8443".into(),
            token: token.into(),
            tls_fingerprint: pin.into(),
            ssh_host: String::new(),
            ssh_port: 0,
            auth_mode: String::new(),
            user: String::new(),
            identity_file: String::new(),
        };
        s.reconcile(vec![mk("t1", "")]).await;
        s.reconcile(vec![mk("t2", "")]).await; // token rotated
        s.reconcile(vec![mk("t2", "sha256:aa")]).await; // pin added
        assert_eq!(fg.start_count(), 3, "token change + pin add each restart");
        let inner = s.inner.lock().unwrap();
        let got = &inner.groups["s"].target;
        assert_eq!(got.token, "t2");
        assert_eq!(got.tls_fingerprint, "sha256:aa");
    }

    /// A changed `user` in config (the CLI's `--refetch`, `--allow-user-change`) restarts
    /// the server's group, so its credential sources are rebuilt held to the new user;
    /// an unchanged one does not (plan 037 §3.2.2).
    #[tokio::test]
    async fn reconcile_user_change_restarts() {
        let fg = Arc::new(FakeGroups::default());
        let s = test_supervisor(&fg);
        let mk = |user: &str| ServerTarget {
            user: user.into(),
            ..secure_target()
        };
        s.reconcile(vec![mk("")]).await;
        s.reconcile(vec![mk("pat")]).await; // first recorded
        s.reconcile(vec![mk("pat")]).await; // unchanged: no churn
        s.reconcile(vec![mk("admin")]).await; // changed
        assert_eq!(
            fg.start_count(),
            3,
            "each user change restarts, a no-op does not"
        );
        assert_eq!(fg.stopped_names(), vec!["s".to_string(), "s".to_string()]);
        let inner = s.inner.lock().unwrap();
        assert_eq!(inner.groups["s"].target.user, "admin");
    }

    /// A changed `identity_file` (the CLI's `--identity-file`) restarts the server's group
    /// the same way, so its sources are rebuilt and the ssh they run offers the new key
    /// (plan 037 A8). What bounds who they act as is still the entry's `user`; nothing
    /// checks that a certificate was made for the key.
    #[tokio::test]
    async fn reconcile_identity_file_change_restarts() {
        let fg = Arc::new(FakeGroups::default());
        let s = test_supervisor(&fg);
        let mk = |key: &str| ServerTarget {
            identity_file: key.into(),
            ..secure_target()
        };
        s.reconcile(vec![mk("")]).await;
        s.reconcile(vec![mk("/home/pat/.ssh/id_work")]).await; // set
        s.reconcile(vec![mk("/home/pat/.ssh/id_work")]).await; // unchanged
        s.reconcile(vec![mk("/home/pat/.ssh/id_admin")]).await; // changed
        assert_eq!(
            fg.start_count(),
            3,
            "each change restarts, a no-op does not"
        );
        let inner = s.inner.lock().unwrap();
        assert_eq!(
            inner.groups["s"].target.identity_file,
            "/home/pat/.ssh/id_admin"
        );
    }

    const FAR_FUTURE: Option<i64> = Some(4_102_444_800);

    /// A group's detached credential fetch.
    type MintTask = JoinHandle<Result<String, String>>;

    /// Mints for the target's config user: `admin` gets admin's certificate at once; any
    /// other gets pat's, but only once `release` is notified — the old group's mint, still
    /// running across the reconcile that replaces its group.
    struct HeldUpMinter {
        ca: crate::minter::tests::TestCa,
        entered: tokio::sync::Notify,
        release: tokio::sync::Notify,
    }

    #[async_trait::async_trait]
    impl Minter for HeldUpMinter {
        async fn mint(&self, t: &ServerTarget, _scope: &str) -> Result<Minted, MinterError> {
            use crate::minter::tests::{minted_cert_for, TestUser};
            if t.user == "admin" {
                return Ok(minted_cert_for(
                    &self.ca,
                    2,
                    FAR_FUTURE,
                    &TestUser::Named("admin"),
                ));
            }
            self.entered.notify_one();
            self.release.notified().await;
            Ok(minted_cert_for(
                &self.ca,
                1,
                FAR_FUTURE,
                &TestUser::Named("pat"),
            ))
        }
    }

    /// A reconcile that changes the config `user` while the old group's mint is still
    /// running: the old group is cancelled before its replacement starts, the replacement
    /// (held to admin) stores admin's certificate, and the old mint, finishing last, writes
    /// nothing — so the store never holds a certificate the current config disallows.
    #[tokio::test]
    async fn an_old_groups_late_mint_never_overwrites_the_replacements_store() {
        for old_user in ["", "pat"] {
            let (_d, store) = crate::minter::tests::tmp_store();
            let minter = Arc::new(HeldUpMinter {
                ca: crate::minter::tests::TestCa::new(),
                entered: tokio::sync::Notify::new(),
                release: tokio::sync::Notify::new(),
            });
            let mints: Arc<Mutex<Vec<MintTask>>> = Arc::default();
            let factory: GroupFactory = {
                let (minter, store, mints) = (minter.clone(), store.clone(), mints.clone());
                Box::new(move |_parent, target, _deps| {
                    let (cancel_tx, cancel_rx) = watch::channel(false);
                    let src = crate::minter::new_group_credential_source_with_store(
                        minter.clone(),
                        target.clone(),
                        crate::minter::SCOPE_CREDENTIALS,
                        Some(store.clone()),
                        cancel_rx.clone(),
                    );
                    // The group's first request: a credential fetch, detached like a
                    // subscription's.
                    mints
                        .lock()
                        .unwrap()
                        .push(tokio::spawn(async move { src.token().await }));
                    let mut cancelled = cancel_rx;
                    let done = tokio::spawn(async move {
                        let _ = cancelled.wait_for(|c| *c).await;
                    });
                    WatcherGroup {
                        target,
                        cancel: Arc::new(cancel_tx),
                        done,
                        statuses: Vec::new(),
                        wake: Arc::new(watch::channel(0u64).0),
                    }
                })
            };
            let (_tx, rx) = watch::channel(false);
            let s = Supervisor::with_factory(rx, deps_with_minter(None), factory);
            let mk = |user: &str| ServerTarget {
                user: user.into(),
                ..secure_target()
            };

            s.reconcile(vec![mk(old_user)]).await;
            minter.entered.notified().await; // the old group's mint is running
            s.reconcile(vec![mk("admin")]).await;
            let new_mint = mints.lock().unwrap().pop().unwrap();
            new_mint.await.unwrap().unwrap();
            let user_of_stored = || {
                let cert = std::fs::read_to_string(store.paths("s").0).unwrap();
                shed_core::certuser::cert_user_pem(&cert).unwrap()
            };
            assert_eq!(
                user_of_stored(),
                shed_core::certuser::CertUser::Named("admin".into())
            );

            // The old mint finishes last.
            minter.release.notify_one();
            let old_mint = mints.lock().unwrap().pop().unwrap();
            let _ = old_mint.await.unwrap();
            assert_eq!(
                user_of_stored(),
                shed_core::certuser::CertUser::Named("admin".into()),
                "old user {old_user:?}: the old group's late mint wrote nothing"
            );
        }
    }

    /// Holds a `credentials` mint until `release`, then answers pat's certificate and
    /// signals `returned`; a `control` mint fails at once.
    struct HeldUpCredentialsMinter {
        ca: crate::minter::tests::TestCa,
        entered: tokio::sync::Notify,
        release: tokio::sync::Notify,
        returned: tokio::sync::Notify,
    }

    #[async_trait::async_trait]
    impl Minter for HeldUpCredentialsMinter {
        async fn mint(&self, _t: &ServerTarget, scope: &str) -> Result<Minted, MinterError> {
            use crate::minter::tests::{minted_cert_for, TestUser};
            if scope != crate::minter::SCOPE_CREDENTIALS {
                return Err(MinterError::new("not this scope".into()));
            }
            self.entered.notify_one();
            self.release.notified().await;
            self.returned.notify_one();
            Ok(minted_cert_for(
                &self.ca,
                1,
                FAR_FUTURE,
                &TestUser::Named("pat"),
            ))
        }
    }

    /// The production wiring, `spawn_server_group`, builds the group's credentials source
    /// FENCED by the group's cancel: a mint the group started that finishes after the
    /// group was cancelled writes nothing to the store. (Its store is rooted in a temp
    /// directory for this thread, never the real state directory.)
    #[tokio::test]
    async fn the_production_group_builds_a_fenced_credentials_source() {
        for user in ["", "pat"] {
            let dir = tempfile::tempdir().unwrap();
            let root = dir.path().join("creds");
            let _rooted = crate::minter::root_credentials_store_at(&root);
            let store = crate::credstore::CredStore::new(&root);
            let minter = Arc::new(HeldUpCredentialsMinter {
                ca: crate::minter::tests::TestCa::new(),
                entered: tokio::sync::Notify::new(),
                release: tokio::sync::Notify::new(),
                returned: tokio::sync::Notify::new(),
            });
            let target = ServerTarget {
                url: "https://127.0.0.1:9".into(),
                ssh_host: "127.0.0.1".into(),
                user: user.into(),
                ..secure_target()
            };
            let deps = deps_with_minter(Some(minter.clone() as Arc<dyn Minter>));
            assert!(should_mint(&deps, &target));
            let (_parent_tx, parent) = watch::channel(false);
            let g = crate::bus::spawn_server_group(parent, &target, &deps);

            minter.entered.notified().await; // the group's credentials mint is running
            g.cancel.send(true).unwrap();
            let _ = g.done.await;
            minter.release.notify_one();
            minter.returned.notified().await;
            // The mint's detached task finishes its adoption and (fenced) write.
            for _ in 0..100 {
                tokio::task::yield_now().await;
            }
            tokio::time::sleep(std::time::Duration::from_millis(50)).await;
            let (cert, key) = store.paths("s");
            assert!(
                !cert.exists() && !key.exists(),
                "user {user:?}: the cancelled group's late mint wrote nothing"
            );
        }
    }

    /// A mint scripted in order (the last answer repeats): `Ok(user)` is a certificate for
    /// that user, `Err(())` a transient minter failure.
    struct ScriptMinter {
        ca: crate::minter::tests::TestCa,
        script: Mutex<Vec<Result<&'static str, ()>>>,
    }

    #[async_trait::async_trait]
    impl Minter for ScriptMinter {
        async fn mint(&self, _t: &ServerTarget, _scope: &str) -> Result<Minted, MinterError> {
            use crate::minter::tests::{minted_cert_for, TestUser};
            let next = {
                let mut script = self.script.lock().unwrap();
                if script.len() > 1 {
                    script.remove(0)
                } else {
                    script[0]
                }
            };
            match next {
                Ok(user) => Ok(minted_cert_for(
                    &self.ca,
                    7,
                    FAR_FUTURE,
                    &TestUser::Named(user),
                )),
                Err(()) => Err(MinterError::new("transient".into())),
            }
        }
    }

    /// A request is explained only by the refusal of the mint its own fetch made or
    /// joined: one served from the cache after ANOTHER caller's refused refresh carries
    /// none; one whose own mint is refused carries that refusal; a minter failure is no
    /// refusal.
    #[tokio::test]
    async fn a_request_is_explained_only_by_its_own_mints_refusal() {
        let minter = Arc::new(ScriptMinter {
            ca: crate::minter::tests::TestCa::new(),
            script: Mutex::new(vec![Ok("pat"), Ok("admin"), Ok("admin"), Err(())]),
        });
        let src = crate::minter::new_credential_source_with_store(
            minter,
            ServerTarget {
                user: "pat".into(),
                ..secure_target()
            },
            crate::minter::SCOPE_CREDENTIALS,
            None,
        );
        let p: Arc<dyn TokenProvider> = Arc::new(src.clone());
        p.token_for_request().await.unwrap(); // pat adopted

        src.refresh().await; // another caller's mint: admin, refused
        assert_eq!(
            p.token_for_request().await.unwrap(),
            "",
            "served from the cache: no refusal to explain this request with"
        );

        src.invalidate();
        let (err, refusal) = p.token_for_request().await.unwrap_err();
        let refusal = refusal.expect("this request's own mint was refused");
        assert_eq!(err.to_string(), refusal.message);
        assert!(refusal.message.contains("held to \"pat\""), "{refusal:?}");
        assert!(refusal.held_user, "the held-user rule's refusal");

        let (err, refusal) = p.token_for_request().await.unwrap_err();
        assert_eq!(err.to_string(), "transient");
        assert_eq!(refusal, None, "a minter failure is no refusal");
    }

    #[tokio::test]
    async fn reconcile_dedup_by_name() {
        let fg = Arc::new(FakeGroups::default());
        let s = test_supervisor(&fg);
        // Two targets with the same name collapse to one group.
        s.reconcile(vec![
            tgt("mini2", "http://a:8080"),
            tgt("mini2", "http://b:8080"),
        ])
        .await;
        assert_eq!(group_names(&s).len(), 1);
    }

    #[tokio::test]
    async fn shutdown_drains_and_reconcile_noop() {
        let fg = Arc::new(FakeGroups::default());
        let s = test_supervisor(&fg);
        s.reconcile(vec![
            tgt("mini2", "http://mini2:8080"),
            tgt("mini3", "http://mini3:8080"),
        ])
        .await;

        s.shutdown().await;
        assert!(group_names(&s).is_empty());
        assert_eq!(fg.stopped_names().len(), 2);

        // Reconcile after shutdown is a no-op.
        s.reconcile(vec![tgt("mini4", "http://mini4:8080")]).await;
        assert!(group_names(&s).is_empty());
    }

    #[tokio::test]
    async fn health_sorted_no_namespaces_for_clientless() {
        let fg = Arc::new(FakeGroups::default());
        let s = test_supervisor(&fg);
        s.reconcile(vec![
            tgt("mini3", "http://mini3:8080"),
            tgt("mini2", "http://mini2:8080"),
        ])
        .await;

        let h = s.health();
        assert_eq!(h.len(), 2);
        // Sorted by name.
        assert_eq!(h[0].name, "mini2");
        assert_eq!(h[1].name, "mini3");
        assert_eq!(h[0].url, "http://mini2:8080");
        // A client-less (fake) group → no namespaces, no panic.
        assert!(h[0].namespaces.is_empty());

        s.shutdown().await;
    }

    /// The empty-desired warn fires ONCE (the `was_empty` latch): after the first empty
    /// reconcile, a second empty reconcile does not re-latch. Asserted via the group set
    /// (the warn is a log-only seam; the latch state is what drives it).
    #[tokio::test]
    async fn reconcile_warn_empty_once() {
        let fg = Arc::new(FakeGroups::default());
        let s = test_supervisor(&fg);
        // First empty reconcile: latches was_empty=true.
        s.reconcile(vec![]).await;
        assert!(s.inner.lock().unwrap().was_empty);
        // A non-empty reconcile clears the latch...
        s.reconcile(vec![tgt("mini2", "http://mini2:8080")]).await;
        assert!(!s.inner.lock().unwrap().was_empty);
        // ...so the NEXT empty reconcile latches again (would warn once more).
        s.reconcile(vec![]).await;
        assert!(s.inner.lock().unwrap().was_empty);
        assert!(group_names(&s).is_empty());
        assert_eq!(fg.stopped_names(), vec!["mini2".to_string()]);
    }

    /// A slow-draining group does not block a concurrent reconcile: the reconcile cancels
    /// under the lock and drains AFTER releasing it, so the second reconcile (a different
    /// server) proceeds while the first group is still draining.
    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn reconcile_drains_off_lock() {
        let fg = Arc::new(FakeGroups::default());
        let s = Arc::new(test_supervisor(&fg));
        s.reconcile(vec![tgt("slow", "http://slow:8080")]).await;

        // Drop "slow" and add "fast" — the drain of "slow" happens off-lock; the add of
        // "fast" completes within the same reconcile.
        s.reconcile(vec![tgt("fast", "http://fast:8080")]).await;
        assert_eq!(group_names(&s), vec!["fast".to_string()]);
        assert_eq!(fg.stopped_names(), vec!["slow".to_string()]);
    }

    fn sub_status(ns: &str, state: ConnState, src: Option<ActiveSource>) -> Arc<Mutex<SubStatus>> {
        Arc::new(Mutex::new(SubStatus {
            namespace: ns.into(),
            state,
            last_error: String::new(),
            since: "2026-01-01T00:00:00Z".into(),
            active_source: src,
        }))
    }

    /// `health()` reports the three plan 035 states by their wire names, carries the
    /// active machine per namespace, and marks the SERVER as not the source when any of
    /// its namespaces is — and not otherwise.
    #[tokio::test]
    async fn health_reports_not_source_and_duplicate() {
        let src = ActiveSource {
            machine_id: "0123456789abcdef0123456789abcdef".into(),
            machine_name: "desk".into(),
        };
        let per_server: HashMap<String, Vec<Arc<Mutex<SubStatus>>>> = HashMap::from([
            (
                "pinned".to_string(),
                vec![
                    sub_status("ssh-agent", ConnState::NotSource, Some(src.clone())),
                    sub_status("docker-credentials", ConnState::Connected, None),
                ],
            ),
            (
                "twice".to_string(),
                vec![
                    sub_status("ssh-agent", ConnState::Duplicate, None),
                    sub_status("docker-credentials", ConnState::Yielding, None),
                ],
            ),
        ]);
        let (_tx, rx) = watch::channel(false);
        let factory: GroupFactory = Box::new(move |_parent, target, _deps| {
            let (cancel_tx, _cancel_rx) = watch::channel(false);
            WatcherGroup {
                statuses: per_server[&target.name].clone(),
                target,
                cancel: Arc::new(cancel_tx),
                done: tokio::spawn(async {}),
                wake: Arc::new(watch::channel(0u64).0),
            }
        });
        let s = Supervisor::with_factory(rx, deps_with_minter(None), factory);
        s.reconcile(vec![tgt("pinned", "http://p"), tgt("twice", "http://t")])
            .await;
        let h = s.health();
        assert_eq!(h[0].name, "pinned");
        let by_ns: HashMap<&str, &NamespaceHealth> = h[0]
            .namespaces
            .iter()
            .map(|n| (n.namespace.as_str(), n))
            .collect();
        assert_eq!(by_ns["ssh-agent"].state, "not-source");
        assert_eq!(by_ns["ssh-agent"].active_source, Some(src.clone()));
        assert_eq!(by_ns["docker-credentials"].state, "connected");
        assert_eq!(by_ns["docker-credentials"].active_source, None);
        assert_eq!(
            h[0].active_source,
            Some(src),
            "any not-source namespace marks the server"
        );
        assert_eq!(h[1].name, "twice");
        // Sorted by namespace: docker-credentials, then ssh-agent.
        assert_eq!(h[1].namespaces[0].state, "yielding");
        assert_eq!(h[1].namespaces[1].state, "duplicate");
        assert_eq!(
            h[1].active_source, None,
            "a duplicate or a yielding namespace is not a not-source"
        );
    }

    /// `retry_now` reaches every running group's wake.
    #[tokio::test]
    async fn retry_now_bumps_every_group() {
        let fg = Arc::new(FakeGroups::default());
        let s = test_supervisor(&fg);
        s.reconcile(vec![tgt("a", "http://a"), tgt("b", "http://b")])
            .await;
        let mut rx = fg.wake.subscribe();
        s.retry_now();
        assert!(
            rx.has_changed().unwrap(),
            "retry_now must bump the group wake"
        );
        // Both groups share the fake's wake, so one call bumps it once per group.
        assert_eq!(*rx.borrow_and_update(), 2);
        s.shutdown().await;
    }

    // ---- the bus TokenProvider bridge (the credentials-scope wire) -------------------

    /// `Arc<CredentialSource>` as a bus `TokenProvider`: `token()` returns the minted
    /// token; `invalidate()` clears the cache so the next `token()` re-mints (the
    /// credentials-scope wire the minter + egress slices deferred here).
    #[tokio::test]
    async fn credential_source_is_bus_token_provider() {
        use crate::minter::{new_credential_source, SCOPE_CREDENTIALS};

        let src = new_credential_source(
            Arc::new(TwoTokenMinter::new()),
            secure_target(),
            SCOPE_CREDENTIALS,
        );
        let provider: Arc<dyn TokenProvider> = Arc::new(src);

        assert_eq!(provider.token().await.unwrap(), "tok1");
        assert_eq!(provider.token().await.unwrap(), "tok1"); // cached
        provider.invalidate();
        assert_eq!(provider.token().await.unwrap(), "tok2"); // re-mint after invalidate
    }

    /// A minter that returns "tok1" then "tok2" (then repeats the last), counting mints —
    /// so the bridge test can observe cache + invalidate without a live SSH server.
    struct TwoTokenMinter {
        calls: AtomicUsize,
    }
    impl TwoTokenMinter {
        fn new() -> Self {
            Self {
                calls: AtomicUsize::new(0),
            }
        }
    }
    #[async_trait::async_trait]
    impl Minter for TwoTokenMinter {
        async fn mint(&self, _t: &ServerTarget, _scope: &str) -> Result<Minted, MinterError> {
            let i = self.calls.fetch_add(1, Ordering::SeqCst);
            let token = if i == 0 { "tok1" } else { "tok2" };
            Ok(Minted::token(
                token,
                Some(crate::status::now_unix() + 24 * 3600),
            ))
        }
    }
}
