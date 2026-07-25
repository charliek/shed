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
    AwsHandlers, BusError, BusLog, ConnState, DockerHandlers, SubStatus, TokenProvider,
};
use crate::discovery::ServerTarget;
use crate::minter::{CredentialSource, Minter};
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
}

/// The server-agnostic components every per-server watcher group shares — built once in
/// `main.rs` and injected into each group (mirror Go's `SharedDeps`). AWS/Docker are
/// `None` when their backend isn't configured; `minter` is `None` only when minting is
/// disabled (production always builds it, so [`should_mint`] then keys purely on the
/// target's https-scheme + SSH endpoint).
pub struct SharedDeps {
    pub ssh_backend: Arc<dyn SshBackend>,
    pub ssh_gate: Arc<dyn ApprovalGate>,
    pub aws: Option<AwsHandlers>,
    pub docker: Option<DockerHandlers>,
    pub audit: Arc<dyn AuditSink>,
    /// Mints the per-server credentials/control tokens over SSH (secure mode). `None`
    /// disables minting → every server uses its (usually empty) configured static token.
    pub minter: Option<Arc<dyn Minter>>,
    pub log: Arc<dyn BusLog>,
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
                            }
                        })
                        .collect();
                    // Sort by namespace to match Go's `HostClient.Status()`, which
                    // `sort.Slice`s by namespace before the supervisor reads it — so
                    // `servers[].namespaces[]` is in the same order on both impls regardless of
                    // subscribe order (ssh-agent, docker-credentials, ...).
                    namespaces.sort_by(|a, b| a.namespace.cmp(&b.namespace));
                    ServerHealth {
                        name,
                        url,
                        namespaces,
                    }
                },
            )
            .collect();
        out.sort_by(|a, b| a.name.cmp(&b.name));
        out
    }
}

/// The wire string for a connection state (mirror the Go SDK's `Conn*` constants, which
/// `SubStatus.State` carries verbatim).
fn conn_state_str(state: ConnState) -> &'static str {
    match state {
        ConnState::Reconnecting => "reconnecting",
        ConnState::Connected => "connected",
        ConnState::Stopped => "stopped",
        ConnState::Rejected => "rejected",
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::bus::FileBusLog;
    use crate::discovery::ServerTarget;
    use crate::minter::{Minted, MinterError};
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
            aws: None,
            docker: None,
            audit: Arc::new(NoopAuditSink),
            minter,
            log: test_log(),
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
        }
    }

    // ---- fake group factory (mirror supervisor_test.go's fakeGroups) -----------------

    #[derive(Default)]
    struct FakeGroups {
        started: Mutex<Vec<ServerTarget>>,
        stopped: Mutex<Vec<String>>,
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
