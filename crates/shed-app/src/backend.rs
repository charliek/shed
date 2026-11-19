//! The shed-core-backed data layer shared by every client: it owns one HTTP
//! client per configured server (+ a create store) and performs the reads,
//! lifecycle actions, and creates. In test mode every client is pointed at the
//! single mock base URL (hermetic).
//!
//! Env-agnostic: [`from_env_parts`](Backend::from_env_parts) takes the resolved
//! `(test_mode, mock_base_url, config_path)` rather than a client's `SHED_*_` env
//! struct, so the GTK + Tauri clients share one implementation.

use std::collections::HashSet;
use std::path::Path;
use std::sync::{Arc, LazyLock};

use tokio::runtime::Handle;

use shed_core::config::ShedConfig;
use shed_core::create::{CreateProgress, CreateStore};
use shed_core::http::{Client, ShedError, SourceError};
use shed_core::models::{
    display_source_text, CreateShedRequest, EgressProfileInfo, Shed, ShedStatus, SourceDocument,
    SystemDiskUsage, SOURCE_TEXT_MAX_BYTES,
};
use shed_core::terminal::{self, TerminalCommand};
use shed_core::token::{ControlTokenProvider, CredentialObserver, TokenMinter};

use crate::auth_modes::AuthModeRegistry;

pub struct Backend {
    /// (server_name, client) — shed-core stamps each shed's `host` with the name.
    clients: Vec<(String, Client)>,
    /// (server_name, ssh host+port) for `terminal.preview`, from config. Built for
    /// EVERY configured server (not the client-filtered set): SSH is independent of
    /// the HTTP client, and unlike the HTTP path it's never mock-redirected.
    ssh_targets: Vec<(String, SshTarget)>,
    /// The config's `default_server`, used to resolve host-less lifecycle/create
    /// ops (`ShedConfig` sorts servers by name, so "first" is not the default).
    default_server: Option<String>,
    /// Pull-based store of in-flight creates (the pure shed-core orchestration).
    creates: CreateStore,
}

/// A server's SSH endpoint (from config) — where `terminal.preview` points ssh.
struct SshTarget {
    host: String,
    port: u16,
}

/// Everything `shed-app::rc` needs to build the RC ssh argv for one shed, resolved
/// from config here — so `RcService` never couples to `Backend`. `server_name`
/// (= `Shed.host`, the config server name) backs the `--target shed:<shed>@<server>`
/// arg; `known_hosts` pins the server's key (`~/.shed/known_hosts`).
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct RcTarget {
    pub server_name: String,
    pub ssh_host: String,
    pub ssh_port: u16,
    pub known_hosts: String,
}

impl Backend {
    /// Build from a client's resolved env parts: load the config file, then
    /// construct the per-server clients.
    ///
    /// Hermeticity: test mode must have a mock to redirect every client to; a
    /// partial test env (`test_mode` without `mock_base_url`) builds NO clients
    /// rather than dialing the developer's real hosts.
    pub fn from_env_parts(
        test_mode: bool,
        mock_base_url: Option<&str>,
        config_path: &Path,
    ) -> Self {
        Self::from_env_parts_with_minter(
            test_mode,
            mock_base_url,
            config_path,
            None,
            &HashSet::new(),
        )
    }

    /// As [`from_env_parts`](Self::from_env_parts), but threads a control-token
    /// `minter` (the host-agent bridge) into every non-mock server's client, so a
    /// secure server mints/refreshes its bearer via `token.get` and fails closed
    /// on a mint failure (F6). GTK passes `None` (static-token only).
    ///
    /// `unreachable_hosts` is a TEST-ONLY per-host down simulation consulted only in
    /// the mock arm (see [`from_config_with_minter`](Self::from_config_with_minter)).
    pub fn from_env_parts_with_minter(
        test_mode: bool,
        mock_base_url: Option<&str>,
        config_path: &Path,
        minter: Option<&Arc<dyn TokenMinter>>,
        unreachable_hosts: &HashSet<String>,
    ) -> Self {
        Self::from_env_parts_with_credentials(
            test_mode,
            mock_base_url,
            config_path,
            minter,
            None,
            unreachable_hosts,
        )
    }

    /// As [`from_env_parts_with_minter`](Self::from_env_parts_with_minter), but
    /// also SEEDS `modes` from the config's `auth_mode` entries and attaches it
    /// as the [`CredentialObserver`] of every provider it builds (plan 002 §7 P1
    /// / P5).
    ///
    /// The registry is passed in rather than returned because the minter — which
    /// reads it to decide whether to expect a certificate — is constructed
    /// BEFORE the Backend (the embedded broker's launch order).
    pub fn from_env_parts_with_credentials(
        test_mode: bool,
        mock_base_url: Option<&str>,
        config_path: &Path,
        minter: Option<&Arc<dyn TokenMinter>>,
        modes: Option<&Arc<AuthModeRegistry>>,
        unreachable_hosts: &HashSet<String>,
    ) -> Self {
        if test_mode && mock_base_url.is_none() {
            return Self {
                clients: Vec::new(),
                ssh_targets: Vec::new(),
                default_server: None,
                creates: CreateStore::new(),
            };
        }
        let config = ShedConfig::load(&config_path.to_string_lossy());
        if let Some(modes) = modes {
            modes.seed_from_config(&config);
        }
        Self::from_config_with_credentials(&config, mock_base_url, minter, modes, unreachable_hosts)
    }

    /// Build clients from an already-parsed config. When `mock_base_url` is set
    /// (test mode) every server is redirected to that single hermetic mock —
    /// plain HTTP, no pin, no token — so no real host is touched.
    pub fn from_config(config: &ShedConfig, mock_base_url: Option<&str>) -> Self {
        Self::from_config_with_minter(config, mock_base_url, None, &HashSet::new())
    }

    /// As [`from_config`](Self::from_config), but threads a control-token `minter`
    /// into every non-mock server (see
    /// [`from_env_parts_with_minter`](Self::from_env_parts_with_minter)). The mock
    /// path never gets a minter (hermetic — a static, tokenless mock).
    ///
    /// `unreachable_hosts` is a TEST-ONLY per-host down simulation: in the mock arm
    /// a server whose NAME is in the set is pointed at a closed port
    /// (`http://127.0.0.1:1`, a fast deterministic ECONNREFUSED) instead of the
    /// mock, so the harness can exercise the per-host error row end-to-end.
    /// Mock-mode-only by construction — the production `None` arm never consults it.
    pub fn from_config_with_minter(
        config: &ShedConfig,
        mock_base_url: Option<&str>,
        minter: Option<&Arc<dyn TokenMinter>>,
        unreachable_hosts: &HashSet<String>,
    ) -> Self {
        Self::from_config_with_credentials(config, mock_base_url, minter, None, unreachable_hosts)
    }

    /// As [`from_config_with_minter`](Self::from_config_with_minter), but
    /// attaches `modes` as each provider's [`CredentialObserver`] so the mode
    /// the core ADOPTS is learned (in memory — this client persists nothing).
    pub fn from_config_with_credentials(
        config: &ShedConfig,
        mock_base_url: Option<&str>,
        minter: Option<&Arc<dyn TokenMinter>>,
        modes: Option<&Arc<AuthModeRegistry>>,
        unreachable_hosts: &HashSet<String>,
    ) -> Self {
        let clients = config
            .servers
            .iter()
            .filter_map(|s| {
                let client = match mock_base_url {
                    Some(mock) => {
                        let url = if unreachable_hosts.contains(&s.name) {
                            "http://127.0.0.1:1".to_string()
                        } else {
                            mock.to_string()
                        };
                        Client::new(url, s.name.clone(), String::new(), None, None)
                    }
                    None => {
                        // A pin on a non-https URL fails closed in Client::new → that
                        // server is skipped rather than sent plaintext. Secure servers
                        // mint/refresh their control credential via the host agent; on
                        // a mint failure the client sends nothing — never the static
                        // config token (F6).
                        let r = s.resolved_endpoint();
                        let pin = (!r.pin.is_empty()).then_some(r.pin);
                        // Three real-server arms. Both minter arms build the
                        // provider here so it is held to the entry's `user`: a
                        // certificate issued for another named user, or one that
                        // names none, is refused (plan 037 §3.2.2). The entry is
                        // read once, at start; a changed `user` needs a restart.
                        let held_user = (!s.user.is_empty()).then(|| s.user.clone());
                        match (minter, modes) {
                            (Some(m), Some(obs)) => {
                                let provider = Arc::new(
                                    ControlTokenProvider::new(s.name.clone(), Arc::clone(m))
                                        .with_held_user(held_user)
                                        .with_observer(
                                            Arc::clone(obs) as Arc<dyn CredentialObserver>
                                        ),
                                );
                                Client::with_provider(r.base_url, s.name.clone(), pin, provider)
                            }
                            (Some(m), None) => {
                                let provider = Arc::new(
                                    ControlTokenProvider::new(s.name.clone(), Arc::clone(m))
                                        .with_held_user(held_user),
                                );
                                Client::with_provider(r.base_url, s.name.clone(), pin, provider)
                            }
                            // No minter: the static open-mode config token, as before.
                            (None, _) => Client::new(
                                r.base_url,
                                s.name.clone(),
                                s.control_token.clone(),
                                pin,
                                None,
                            ),
                        }
                    }
                }
                .ok()?;
                Some((s.name.clone(), client))
            })
            .collect();
        let ssh_targets = config
            .servers
            .iter()
            .map(|s| {
                (
                    s.name.clone(),
                    SshTarget {
                        host: s.host.clone(),
                        port: s.ssh_port,
                    },
                )
            })
            .collect();
        Self {
            clients,
            ssh_targets,
            default_server: config.default_server.clone(),
            creates: CreateStore::new(),
        }
    }

    /// Resolve a client by host name; a host-less op targets the config's
    /// `default_server`, falling back to the first configured host.
    fn client_for(&self, host: Option<&str>) -> Result<&Client, ShedError> {
        resolve(&self.clients, self.default_server.as_deref(), host)
            .ok_or_else(|| no_configured_host(host))
    }

    /// Fan `call` out across every configured host concurrently, tagging each
    /// result with its server name. `join_all` so a slow/down host never
    /// serializes the others (each bounded by shed-core's per-request timeout).
    /// The shared plumbing behind `refresh`/`system_df`/`egress_profiles` — each
    /// caller decides whether a per-host `Err` is rolled up, kept as an error
    /// row, or dropped.
    async fn per_host<T>(
        &self,
        call: impl AsyncFn(&Client) -> Result<T, ShedError>,
    ) -> Vec<(String, Result<T, ShedError>)> {
        let call = &call;
        let fetches = self
            .clients
            .iter()
            .map(|(name, c)| async move { (name.clone(), call(c).await) });
        futures::future::join_all(fetches)
            .await
            .into_iter()
            .collect()
    }

    /// Fetch sheds from every configured host concurrently (host-stamped by
    /// shed-core). `join_all` so a slow/down host doesn't serialize the others —
    /// each is bounded by shed-core's per-request timeout. A per-host failure is
    /// dropped rather than blanking the whole dashboard; surfacing per-host errors
    /// is A1a-add (the reachability rollup).
    pub async fn list_sheds(&self) -> Vec<Shed> {
        let fetches = self.clients.iter().map(|(_, client)| client.list_sheds());
        futures::future::join_all(fetches)
            .await
            .into_iter()
            .filter_map(Result::ok)
            .flatten()
            .collect()
    }

    /// The configured server names with a working client — i.e. the hosts a
    /// create/lifecycle op can target (the New-Shed dialog's host picker), even
    /// ones that have no sheds yet.
    pub fn host_names(&self) -> Vec<String> {
        self.clients.iter().map(|(name, _)| name.clone()).collect()
    }

    /// Every configured host paired with its read client.
    ///
    /// For the long-lived per-host SUBSCRIBERS (the live-activity SSE watcher),
    /// which need a client each and outlive any single request. `Client` clones
    /// cheaply — the transport and token cache are Arc-backed — so each watcher
    /// owning one shares the same token machinery rather than duplicating it.
    pub fn host_clients(&self) -> Vec<(String, Client)> {
        self.clients
            .iter()
            .map(|(name, c)| (name.clone(), c.clone()))
            .collect()
    }

    // -- lifecycle --------------------------------------------------------

    pub async fn start(&self, host: Option<&str>, name: &str) -> Result<(), ShedError> {
        self.client_for(host)?.start(name).await
    }
    pub async fn stop(&self, host: Option<&str>, name: &str) -> Result<(), ShedError> {
        self.client_for(host)?.stop(name).await
    }
    pub async fn reset(&self, host: Option<&str>, name: &str) -> Result<(), ShedError> {
        self.client_for(host)?.reset(name).await
    }
    pub async fn delete(&self, host: Option<&str>, name: &str) -> Result<(), ShedError> {
        self.client_for(host)?.delete(name).await
    }

    /// Dispatch a lifecycle action by name — the single `start`/`stop`/`reset`/
    /// `delete` string map shared by the clients' `shed.*` IPC ops (and the Tauri
    /// `invoke` command), which each used to hand-roll it. An unrecognized action
    /// is a config error, never a silent fallthrough.
    pub async fn shed_action(
        &self,
        host: Option<&str>,
        name: &str,
        action: &str,
    ) -> Result<(), ShedError> {
        match action {
            "start" => self.start(host, name).await,
            "stop" => self.stop(host, name).await,
            "reset" => self.reset(host, name).await,
            "delete" => self.delete(host, name).await,
            other => Err(ShedError::Config(format!("unknown action: {other}"))),
        }
    }

    // -- create (on the pure shed-core CreateStore) -----------------------

    /// Start a create on `host`; the SSE stream runs on `rt` in the background.
    /// Returns the id the caller polls via [`create_status`](Self::create_status).
    pub fn create_start(
        &self,
        rt: &Handle,
        host: Option<&str>,
        req: CreateShedRequest,
    ) -> Result<String, ShedError> {
        let client = self.client_for(host)?;
        Ok(self.creates.start(rt, client, req))
    }

    pub fn create_status(&self, id: &str) -> Option<CreateProgress> {
        self.creates.status(id)
    }

    pub fn create_cancel(&self, id: &str) {
        self.creates.cancel(id);
    }

    // -- A1a-add: reachability rollup (additive; GTK ignores it) ----------

    /// Like [`list_sheds`](Self::list_sheds) but ALSO rolls up per-host failures
    /// into a `last_error` summary (`0 → None`, `1 → that error`, `2+ → "N hosts
    /// unreachable"`) — the reachability rollup. Additive:
    /// `list_sheds` keeps its error-dropping contract (GTK uses it and never
    /// surfaces the rollup); the Tauri client surfaces `last_error`.
    pub async fn refresh(&self) -> Reachability {
        let mut sheds = Vec::new();
        let mut host_errors = Vec::new();
        for (name, result) in self.per_host(async |c: &Client| c.list_sheds().await).await {
            match result {
                Ok(mut s) => sheds.append(&mut s),
                Err(e) => host_errors.push(HostFailure::from_error(&name, &e)),
            }
        }
        let last_error = match host_errors.as_slice() {
            [] => None,
            [only] => Some(only.summary.clone()),
            many => Some(format!("{} hosts unreachable", many.len())),
        };
        Reachability {
            sheds,
            last_error,
            host_errors,
        }
    }

    /// Per-host disk usage (`GET /api/system/df`) for the System pane. Unlike
    /// `list_sheds`, a per-host failure is KEPT as an error row (the pane shows
    /// which host is unreachable + why). Concurrent.
    pub async fn system_df(&self) -> Vec<HostDiskUsage> {
        self.per_host(async |c: &Client| c.system_df().await)
            .await
            .into_iter()
            .map(|(host, result)| match result {
                Ok(usage) => HostDiskUsage {
                    host,
                    usage: Some(usage),
                    error: None,
                },
                Err(e) => HostDiskUsage {
                    host,
                    usage: None,
                    error: Some(e.to_string()),
                },
            })
            .collect()
    }

    /// Per-host egress profiles (`GET /api/egress/profiles`) for the Egress pane.
    /// Like [`system_df`](Self::system_df), a per-host failure is KEPT as an error
    /// row (that host is unreachable or has egress disabled) rather than failing
    /// the call. Concurrent.
    pub async fn egress_profiles(&self) -> Vec<HostEgressProfiles> {
        self.per_host(async |c: &Client| c.egress_profiles().await)
            .await
            .into_iter()
            .map(|(host, result)| match result {
                Ok(profiles) => HostEgressProfiles {
                    host,
                    profiles,
                    error: None,
                },
                Err(e) => HostEgressProfiles {
                    host,
                    profiles: Vec::new(),
                    error: Some(e.to_string()),
                },
            })
            .collect()
    }

    // -- credential sources (plan 035 §3.4) -------------------------------

    /// Per-host credential source (`GET /api/sources`): which machine answers for this
    /// user on each configured server. Like [`system_df`](Self::system_df), a per-host
    /// failure is KEPT as an error row; a server older than the route is a row with
    /// `supported: false` and no error. `this_machine` is this machine's id when known,
    /// so each row can say whether the source is the machine the caller runs on.
    /// Concurrent.
    pub async fn source_status(&self, this_machine: Option<&str>) -> Vec<HostSource> {
        self.per_host(async |c: &Client| Ok(c.source_status().await))
            .await
            .into_iter()
            .map(|(host, result)| match result {
                Ok(Ok(doc)) => HostSource::from_document(host, &doc, this_machine),
                Ok(Err(e)) => HostSource::from_error(host, &e),
                Err(e) => HostSource::from_error(host, &SourceError::Shed(e)),
            })
            .collect()
    }

    /// Make `machine_id` the machine that answers for this user on `host`
    /// (`POST /api/sources/use`), and return that host's row as the server answered it.
    /// `host` names a configured server exactly; there is no default for a move.
    pub async fn source_use(
        &self,
        host: &str,
        machine_id: &str,
    ) -> Result<HostSource, SourceError> {
        let client = resolve(&self.clients, None, Some(host))
            .ok_or_else(|| SourceError::Shed(no_configured_host(Some(host))))?;
        let doc = client.source_use(machine_id).await?;
        Ok(HostSource::from_document(
            host.to_string(),
            &doc,
            Some(machine_id),
        ))
    }

    // -- terminal (A1c-2) -------------------------------------------------

    /// Resolve the ssh command that opens a shell in `shed` on `host` (host-less →
    /// default server), pinning the server's key in `~/.shed/known_hosts`. A pure
    /// build, NO spawn — backs `terminal.preview`; the preset openers are the
    /// clients' platform-specific job.
    pub fn terminal_preview(
        &self,
        host: Option<&str>,
        shed: &str,
        session: Option<&str>,
    ) -> Result<TerminalCommand, ShedError> {
        let target = self.ssh_target_for(host)?;
        Ok(terminal::ssh_command(
            shed,
            &target.host,
            target.port,
            &known_hosts_path(),
            session,
        ))
    }

    /// A server's SSH endpoint by host name (host-less → default → first) — the
    /// same resolution `client_for` uses, over the SSH targets.
    fn ssh_target_for(&self, host: Option<&str>) -> Result<&SshTarget, ShedError> {
        resolve(&self.ssh_targets, self.default_server.as_deref(), host)
            .ok_or_else(|| no_configured_host(host))
    }

    // -- RC targets (B2.2) — resolved here so `shed-app::rc::RcService` never
    //    couples to `Backend`; the ipc layer passes these into launch()/list(). --

    /// The RC ssh target for a single op (launch/kill) — host-less → default →
    /// first, the same resolution as `terminal.preview`. Unlike `ssh_target_for`
    /// this also carries the resolved server *name* (for the `--target
    /// shed:<shed>@<server>` arg) and the `known_hosts` path.
    pub fn resolve_rc_target(&self, host: Option<&str>) -> Result<RcTarget, ShedError> {
        let (name, target) = resolve_entry(&self.ssh_targets, self.default_server.as_deref(), host)
            .ok_or_else(|| no_configured_host(host))?;
        Ok(RcTarget {
            server_name: name.clone(),
            ssh_host: target.host.clone(),
            ssh_port: target.port,
            known_hosts: known_hosts_path(),
        })
    }

    /// The RC ssh targets for every RUNNING shed (optionally filtered by
    /// host/shed) — what `rc.list` fans out over. Pairs each running shed with the
    /// resolved ssh endpoint of its host, so `RcService::list` can probe them all
    /// without touching `Backend`.
    pub async fn rc_targets(
        &self,
        host: Option<&str>,
        shed: Option<&str>,
    ) -> Vec<(Shed, RcTarget)> {
        self.list_sheds()
            .await
            .into_iter()
            .filter(|s| {
                s.status == ShedStatus::Running
                    && host.is_none_or(|h| s.host == h)
                    && shed.is_none_or(|n| s.name == n)
            })
            .filter_map(|s| self.resolve_rc_target(Some(&s.host)).ok().map(|t| (s, t)))
            .collect()
    }
}

/// Resolve a `(name, T)` entry by host name: an explicit host matches by name; a
/// host-less op prefers `default_server`, else the first entry. Shared by
/// `client_for` (HTTP), `ssh_target_for` (SSH), and `resolve_rc_target` (RC — it
/// needs the resolved server *name* too, for the `--target` arg).
fn resolve_entry<'a, T>(
    items: &'a [(String, T)],
    default_server: Option<&str>,
    host: Option<&str>,
) -> Option<&'a (String, T)> {
    let by_name = |n: &str| items.iter().find(|(name, _)| name == n);
    match host {
        Some(h) => by_name(h),
        None => default_server.and_then(by_name).or_else(|| items.first()),
    }
}

/// As [`resolve_entry`], but yields only the value (the common case).
fn resolve<'a, T>(
    items: &'a [(String, T)],
    default_server: Option<&str>,
    host: Option<&str>,
) -> Option<&'a T> {
    resolve_entry(items, default_server, host).map(|(_, t)| t)
}

fn no_configured_host(host: Option<&str>) -> ShedError {
    let which = host.map(|h| format!(": {h}")).unwrap_or_default();
    ShedError::Config(format!("no configured host{which}"))
}

/// The shed CLI's `known_hosts` file (`~/.shed/known_hosts`, the same file
/// `shed server add` pins keys into) — `~/.shed` on both macOS and Linux.
pub(crate) fn known_hosts_path() -> String {
    // Cached: HOME is stable for the process, and `rc_targets` resolves this once
    // per running shed — no need to re-read the env + reformat each time.
    static PATH: LazyLock<String> = LazyLock::new(|| {
        let home = std::env::var("HOME").unwrap_or_default();
        format!("{home}/.shed/known_hosts")
    });
    PATH.clone()
}

/// All hosts' sheds + a rollup of per-host reachability failures (what the
/// dashboard surfaces as "N hosts unreachable").
#[derive(Debug, Clone, serde::Serialize)]
pub struct Reachability {
    pub sheds: Vec<Shed>,
    pub last_error: Option<String>,
    /// The per-host failures in presentation shape — plural, because one failed
    /// host is not the only case (plan 006 D6). `last_error` stays as the
    /// pre-existing string rollup; consumers that can render structure use this.
    pub host_errors: Vec<HostFailure>,
}

/// One host's failure in presentation shape (plan 006 D6 / shed#300): `kind` is
/// what a UI branches on, `summary` is the one-line banner text with the REMEDY
/// FIRST (banners truncate from the end), `detail` is the tooltip/log body.
#[derive(Debug, Clone, PartialEq, serde::Serialize)]
pub struct HostFailure {
    pub server: String,
    pub kind: HostFailureKind,
    pub summary: String,
    pub detail: String,
}

/// The known causes a UI treats specially. `Other` is the open set — an
/// unrecognized failure renders generically but still names its host.
#[derive(Debug, Clone, Copy, PartialEq, Eq, serde::Serialize)]
#[serde(rename_all = "snake_case")]
pub enum HostFailureKind {
    AgentUpgradeRequired,
    Other,
}

impl HostFailure {
    /// Presentation mapping for one host's error. The typed
    /// [`ShedError::AgentUpgradeRequired`] keeps its remedy as the whole
    /// summary; everything else leads with the host name so multi-host strips
    /// stay scannable.
    pub fn from_error(server: &str, e: &ShedError) -> Self {
        match e {
            ShedError::AgentUpgradeRequired {
                server: for_server,
                detail,
            } => Self {
                server: server.to_string(),
                kind: HostFailureKind::AgentUpgradeRequired,
                summary: format!(
                    "Upgrade shed-host-agent — it can't obtain a certificate for {for_server}."
                ),
                detail: detail.clone(),
            },
            other => {
                let detail = other.to_string();
                Self {
                    server: server.to_string(),
                    kind: HostFailureKind::Other,
                    summary: format!("{server}: {detail}"),
                    detail,
                }
            }
        }
    }
}

/// One host's disk usage, or the error that host returned — the System pane shows
/// an error row per host rather than dropping unreachable hosts (unlike list_sheds).
#[derive(Debug, Clone, serde::Serialize)]
pub struct HostDiskUsage {
    pub host: String,
    pub usage: Option<SystemDiskUsage>,
    pub error: Option<String>,
}

/// One host's egress profiles, or the error that host returned — the Egress pane
/// shows an error row per host (unreachable / egress disabled) rather than
/// dropping it. Same row shape as `HostDiskUsage`.
#[derive(Debug, Clone, serde::Serialize)]
pub struct HostEgressProfiles {
    pub host: String,
    pub profiles: Vec<EgressProfileInfo>,
    pub error: Option<String>,
}

/// One host's credential source, in presentation shape (plan 035 §3.4): whether the
/// server supports sources at all, which machine answers (its id and name, cleaned for
/// display), whether that is the machine the caller runs on, and whether it is connected
/// now — or why the server could not say.
#[derive(Debug, Clone, PartialEq, Eq, serde::Serialize)]
pub struct HostSource {
    pub host: String,
    /// False for a server older than the route (a plain 404), and for a host that
    /// could not be asked at all (then `error` says why).
    pub supported: bool,
    /// The machine that answers, or `None` when no machine is bound yet (or the server
    /// could not be read).
    pub source: Option<SourceMachineRow>,
    /// Whether `source` is this machine.
    pub this_machine: bool,
    /// Whether `source` holds a live subscription right now.
    pub connected: bool,
    /// The server was busy (`503 SOURCE_BUSY`, still so after the core's bounded
    /// retries): the source is UNKNOWN for now, not absent. `source` is `None` and
    /// `error` is `None`; whoever shows the row keeps what it last knew for this host
    /// and reads again later.
    pub busy: bool,
    /// The server could not be reached, or its answer could not be read (a transport
    /// failure, an undecodable body, a 5xx with no API error body — what a server
    /// restart looks like): whether it supports sources, and which machine answers, are
    /// UNKNOWN for now — never "not supported". `supported` is false and `error` says
    /// why; whoever shows the row keeps what it last knew for this host, notes that the
    /// server cannot be reached, and reads again later.
    pub unreachable: bool,
    pub error: Option<String>,
}

/// A machine as a source row shows it. Both strings came from other machines by way of
/// the server and are cleaned ([`display_source_text`]); `name` is empty when the
/// server did not list the bound machine.
#[derive(Debug, Clone, PartialEq, Eq, serde::Serialize)]
pub struct SourceMachineRow {
    pub id: String,
    pub name: String,
}

impl HostSource {
    /// The row a source document gives, as seen from the machine whose id is
    /// `this_machine` (`None`: not known, so no row is "this machine").
    pub fn from_document(host: String, doc: &SourceDocument, this_machine: Option<&str>) -> Self {
        let source_id = doc.source_machine_id();
        let entry = source_id.and_then(|id| doc.machine(id));
        let source = source_id.map(|id| SourceMachineRow {
            id: display_source_text(id, SOURCE_TEXT_MAX_BYTES),
            name: entry
                .map(|m| display_source_text(&m.name, SOURCE_TEXT_MAX_BYTES))
                .unwrap_or_default(),
        });
        Self {
            host,
            supported: true,
            this_machine: source_id.is_some() && source_id == this_machine,
            connected: entry.is_some_and(|m| m.connected),
            source,
            busy: false,
            unreachable: false,
            error: None,
        }
    }

    /// The row for a host whose source could not be read. Only an older server (a
    /// plain 404) is `supported: false` with no error: it simply has none to show. A
    /// server that could not be reached or read is `unreachable` — unknown, not
    /// unsupported.
    pub fn from_error(host: String, e: &SourceError) -> Self {
        let (supported, error) = match e {
            SourceError::Unsupported => (false, None),
            // Transient: it has sources, and could not say which right now.
            SourceError::Busy { .. } => (true, None),
            // The server answered the route with a refusal: it has sources, and said
            // why this caller cannot see them.
            SourceError::Api { .. } => (true, Some(e.to_string())),
            SourceError::Shed(_) => (false, Some(e.to_string())),
        };
        Self {
            host,
            supported,
            source: None,
            this_machine: false,
            connected: false,
            busy: matches!(e, SourceError::Busy { .. }),
            unreachable: is_unreachable(e),
            error,
        }
    }
}

/// Whether a failed source read says nothing about the server but that it could not be
/// reached or read just now: a transport failure, an answer that could not be decoded,
/// or a 5xx with no API error body (a restarting server, a proxy in front of one).
fn is_unreachable(e: &SourceError) -> bool {
    matches!(
        e,
        SourceError::Shed(ShedError::Transport(_) | ShedError::Decode(_))
    ) || matches!(e, SourceError::Shed(ShedError::BadStatus(s)) if *s >= 500)
}

#[cfg(test)]
mod tests {
    use super::*;
    use httpmock::prelude::*;
    use std::path::PathBuf;

    /// A backend with clients pointed at explicit URLs (bypasses config + the
    /// mock-redirect) so the multi-host concurrent path is exercised directly.
    fn backend_with(clients: Vec<(String, Client)>) -> Backend {
        Backend {
            clients,
            ssh_targets: Vec::new(),
            default_server: None,
            creates: CreateStore::new(),
        }
    }

    fn client_at(name: &str, base_url: String) -> (String, Client) {
        let client = Client::new(base_url, name.to_string(), String::new(), None, None).unwrap();
        (name.to_string(), client)
    }

    /// The presentation mapping (plan 006 D6 / shed#300): the typed upgrade
    /// case leads with the remedy and keeps its kind; everything else names its
    /// host and stays `Other`.
    #[test]
    fn host_failure_presentation_mapping() {
        let up = HostFailure::from_error(
            "dev-mtls",
            &ShedError::AgentUpgradeRequired {
                server: "dev-mtls".into(),
                detail: "does not support `credential.get`".into(),
            },
        );
        assert_eq!(up.kind, HostFailureKind::AgentUpgradeRequired);
        assert!(
            up.summary.starts_with("Upgrade shed-host-agent"),
            "{}",
            up.summary
        );
        assert!(up.detail.contains("credential.get"), "{}", up.detail);

        let other = HostFailure::from_error("mini3", &ShedError::BadStatus(500));
        assert_eq!(other.kind, HostFailureKind::Other);
        assert!(other.summary.starts_with("mini3: "), "{}", other.summary);
    }

    fn server_entry(name: &str) -> shed_core::config::ShedServerEntry {
        shed_core::config::ShedServerEntry {
            name: name.into(),
            host: "127.0.0.1".into(),
            http_port: 8080,
            ssh_port: 2222,
            ..Default::default()
        }
    }

    /// A minter that answers every mint with the same certificate: one a 0.10
    /// server issued for user `pat` (the shared two-language fixture). Its key is
    /// not the provider's, so it can never be ADOPTED — what it shows is whether
    /// the provider refused it for its user first, which only a provider held to
    /// the entry's `user` does.
    struct PatCertMinter;

    #[async_trait::async_trait]
    impl TokenMinter for PatCertMinter {
        async fn mint(&self, _server: &str) -> Result<shed_core::token::MintedToken, ShedError> {
            unreachable!("an mtls-capable minter is asked through mint_credential")
        }
        fn supports_mtls(&self) -> bool {
            true
        }
        async fn mint_credential(
            &self,
            _server: &str,
            _req: &shed_core::token::CredentialRequest,
        ) -> Result<shed_core::token::MintedCredential, ShedError> {
            Ok(shed_core::token::MintedCredential::Certificate(
                shed_core::token::MintedCertificate {
                    cert_pem: include_str!("../../fixtures/clientuser/issued-pat.pem").into(),
                    serial: "01".into(),
                    expires_at_unix: Some(4_102_444_800),
                },
            ))
        }
    }

    /// A config of one server at `url`, held to `user`.
    fn held_config(url: &str, user: &str) -> ShedConfig {
        ShedConfig {
            servers: vec![shed_core::config::ShedServerEntry {
                name: "shed".into(),
                host: "127.0.0.1".into(),
                api_url: url.into(),
                control_token: "static-tok".into(),
                user: user.into(),
                ..Default::default()
            }],
            ..Default::default()
        }
    }

    /// Both minter arms build a provider held to the entry's `user` (plan 037
    /// §3.2.2): pat's certificate on an entry held to admin is refused with the
    /// held-user message, which reaches the caller once the server refuses the
    /// unauthenticated request.
    #[tokio::test]
    async fn both_minter_arms_hold_the_entrys_user() {
        let server = MockServer::start_async().await;
        server
            .mock_async(|w, t| {
                w.method(GET).path("/api/sheds");
                t.status(401);
            })
            .await;
        let minter: Arc<dyn TokenMinter> = Arc::new(PatCertMinter);
        let modes = Arc::new(crate::auth_modes::AuthModeRegistry::new());
        let config = held_config(&server.base_url(), "admin");
        for (arm, observer) in [("observer", Some(&modes)), ("no observer", None)] {
            let backend = Backend::from_config_with_credentials(
                &config,
                None,
                Some(&minter),
                observer,
                &HashSet::new(),
            );
            let clients = backend.host_clients();
            assert_eq!(clients.len(), 1, "{arm}");
            let err = clients[0].1.list_sheds().await.unwrap_err();
            let ShedError::Config(msg) = &err else {
                panic!("{arm}: the refusal is ShedError::Config, got {err:?}");
            };
            assert!(
                msg.starts_with(
                    "server \"shed\": the server issued a certificate for user \"pat\", \
                     but this entry is held to \"admin\"; it was not used."
                ),
                "{arm}: {msg}"
            );
        }

        // The control: with no `user` the same certificate passes the user rule
        // and fails only on its key — so the message above came from the hold.
        let backend = Backend::from_config_with_credentials(
            &held_config(&server.base_url(), ""),
            None,
            Some(&minter),
            None,
            &HashSet::new(),
        );
        let err = backend.host_clients()[0].1.list_sheds().await.unwrap_err();
        assert!(err.to_string().contains("unusable"), "{err}");
        assert!(!err.to_string().contains("held to"), "{err}");
    }

    /// The no-minter arm keeps sending the static open-mode token from config.
    #[tokio::test]
    async fn the_no_minter_arm_still_sends_the_static_token() {
        let server = MockServer::start_async().await;
        let with_token = server
            .mock_async(|w, t| {
                w.method(GET)
                    .path("/api/sheds")
                    .header("authorization", "Bearer static-tok");
                t.status(200).body(r#"{"sheds":[]}"#);
            })
            .await;
        let backend = Backend::from_config_with_credentials(
            &held_config(&server.base_url(), "pat"),
            None,
            None,
            None,
            &HashSet::new(),
        );
        let sheds = backend.host_clients()[0].1.list_sheds().await.unwrap();
        assert!(sheds.is_empty());
        assert_eq!(with_token.hits_async().await, 1);
    }

    #[tokio::test]
    async fn from_env_parts_test_mode_without_mock_builds_no_clients() {
        // Hermeticity: a partial test env must not dial the developer's real hosts.
        let backend = Backend::from_env_parts(true, None, &PathBuf::from("/does/not/matter"));
        assert!(backend.list_sheds().await.is_empty());
    }

    #[tokio::test]
    async fn list_sheds_zero_hosts_is_empty() {
        assert!(backend_with(Vec::new()).list_sheds().await.is_empty());
    }

    #[tokio::test]
    async fn list_sheds_keeps_healthy_host_when_another_fails() {
        // One host 500s, the other returns a shed → only the healthy one's sheds;
        // the failing host doesn't blank the dashboard (concurrent join_all).
        let bad = MockServer::start_async().await;
        bad.mock_async(|w, t| {
            w.method(GET).path("/api/sheds");
            t.status(500);
        })
        .await;
        let good = MockServer::start_async().await;
        good.mock_async(|w, t| {
            w.method(GET).path("/api/sheds");
            t.status(200)
                .body(r#"{"sheds":[{"name":"g","status":"running"}]}"#);
        })
        .await;
        let backend = backend_with(vec![
            client_at("bad", bad.base_url()),
            client_at("good", good.base_url()),
        ]);
        let sheds = backend.list_sheds().await;
        assert_eq!(sheds.len(), 1);
        assert_eq!(sheds[0].name, "g");
        assert_eq!(sheds[0].host, "good");
    }

    #[tokio::test]
    async fn host_less_op_falls_back_to_first_when_default_missing() {
        // A `default_server` naming a server that isn't configured resolves a
        // host-less lifecycle op to the FIRST client (Vec order) — not an error,
        // and not the (alphabetically-first) second. The second client points at an
        // unused address, so a wrong fallback surfaces as a transport failure rather
        // than a false pass.
        let first = MockServer::start_async().await;
        let hit = first
            .mock_async(|w, t| {
                w.method(POST).path("/api/sheds/thing/start");
                t.status(200);
            })
            .await;
        let backend = Backend {
            clients: vec![
                client_at("z-first", first.base_url()),
                client_at("a-second", "http://127.0.0.1:1".to_string()),
            ],
            ssh_targets: Vec::new(),
            default_server: Some("nonexistent".to_string()),
            creates: CreateStore::new(),
        };
        backend
            .start(None, "thing")
            .await
            .expect("host-less op falls back to the first configured client");
        hit.assert_async().await;
    }

    #[tokio::test]
    async fn shed_action_dispatches_by_name_and_rejects_unknown() {
        let server = MockServer::start_async().await;
        let started = server
            .mock_async(|w, t| {
                w.method(POST).path("/api/sheds/x/start");
                t.status(200);
            })
            .await;
        let backend = backend_with(vec![client_at("s", server.base_url())]);
        backend
            .shed_action(Some("s"), "x", "start")
            .await
            .expect("start dispatched");
        started.assert_async().await;
        // an unknown action is a config error, not a silent delete fallthrough
        let e = backend
            .shed_action(Some("s"), "x", "bogus")
            .await
            .unwrap_err();
        assert!(matches!(e, ShedError::Config(_)));
    }

    #[tokio::test]
    async fn list_sheds_decodes_null_sheds_as_empty() {
        // Defensive decode: `{"sheds": null}` → [] (never an error), per shed-core.
        let server = MockServer::start_async().await;
        server
            .mock_async(|w, t| {
                w.method(GET).path("/api/sheds");
                t.status(200).body(r#"{"sheds": null}"#);
            })
            .await;
        let backend = backend_with(vec![client_at("s", server.base_url())]);
        assert!(backend.list_sheds().await.is_empty());
    }

    #[tokio::test]
    async fn refresh_rolls_up_reachability() {
        let good = MockServer::start_async().await;
        good.mock_async(|w, t| {
            w.method(GET).path("/api/sheds");
            t.status(200)
                .body(r#"{"sheds":[{"name":"g","status":"running"}]}"#);
        })
        .await;
        let down = || async {
            let s = MockServer::start_async().await;
            s.mock_async(|w, t| {
                w.method(GET).path("/api/sheds");
                t.status(500);
            })
            .await;
            s
        };
        let bad1 = down().await;
        let bad2 = down().await;

        // all healthy → sheds, no rollup error
        let r = backend_with(vec![client_at("g", good.base_url())])
            .refresh()
            .await;
        assert_eq!(r.sheds.len(), 1);
        assert!(r.last_error.is_none());

        // one host down → that host's error (not the plural summary)
        let r = backend_with(vec![client_at("b1", bad1.base_url())])
            .refresh()
            .await;
        assert!(r.sheds.is_empty());
        assert!(r
            .last_error
            .as_deref()
            .is_some_and(|e| e.starts_with("b1:")));

        // two down → "2 hosts unreachable"
        let r = backend_with(vec![
            client_at("b1", bad1.base_url()),
            client_at("b2", bad2.base_url()),
        ])
        .refresh()
        .await;
        assert_eq!(r.last_error.as_deref(), Some("2 hosts unreachable"));
    }

    #[tokio::test]
    async fn system_df_keeps_error_row_for_down_host() {
        let good = MockServer::start_async().await;
        good.mock_async(|w, t| {
            w.method(GET).path("/api/system/df");
            t.status(200).body(
                r#"{"server_name":"g","backend":"vz","totals":{"all":{"logical_bytes":42,"physical_bytes":42}}}"#,
            );
        })
        .await;
        let bad = MockServer::start_async().await;
        bad.mock_async(|w, t| {
            w.method(GET).path("/api/system/df");
            t.status(500);
        })
        .await;
        let rows = backend_with(vec![
            client_at("good", good.base_url()),
            client_at("bad", bad.base_url()),
        ])
        .system_df()
        .await;
        assert_eq!(rows.len(), 2);
        // healthy host → usage present, no error
        let g = rows.iter().find(|r| r.host == "good").unwrap();
        assert_eq!(g.usage.as_ref().unwrap().totals.all.logical_bytes, 42);
        assert!(g.error.is_none());
        // down host → KEPT as an error row (not dropped), usage absent
        let b = rows.iter().find(|r| r.host == "bad").unwrap();
        assert!(b.usage.is_none());
        assert!(b.error.is_some());
    }

    #[tokio::test]
    async fn egress_profiles_keeps_error_row_for_down_host() {
        // Mirrors system_df_keeps_error_row_for_down_host: a healthy host's
        // profiles decode; a down (or egress-disabled) host is KEPT as an error
        // row, never a failed call or a dropped host.
        let good = MockServer::start_async().await;
        good.mock_async(|w, t| {
            w.method(GET).path("/api/egress/profiles");
            t.status(200).body(
                r#"[{"name":"default","source":"config","profile":{"mode":"audit","allow":["*.github.com"],"deny":["evil.example.com"]}},{"name":"custom","source":"user","profile":{"allow":["api.example.com"]}}]"#,
            );
        })
        .await;
        let bad = MockServer::start_async().await;
        bad.mock_async(|w, t| {
            w.method(GET).path("/api/egress/profiles");
            t.status(500);
        })
        .await;
        let rows = backend_with(vec![
            client_at("good", good.base_url()),
            client_at("bad", bad.base_url()),
        ])
        .egress_profiles()
        .await;
        assert_eq!(rows.len(), 2);
        // healthy host → both profiles present (config baseline + user), no error
        let g = rows.iter().find(|r| r.host == "good").unwrap();
        assert!(g.error.is_none());
        assert_eq!(g.profiles.len(), 2);
        assert_eq!(g.profiles[0].name, "default");
        assert_eq!(g.profiles[0].source, "config");
        assert_eq!(g.profiles[0].profile.mode.as_deref(), Some("audit"));
        assert_eq!(
            g.profiles[0].profile.deny.as_deref(),
            Some(&["evil.example.com".to_string()][..])
        );
        assert_eq!(g.profiles[1].source, "user");
        // down host → KEPT as an error row (not dropped), profiles absent
        let b = rows.iter().find(|r| r.host == "bad").unwrap();
        assert!(b.profiles.is_empty());
        assert!(b.error.is_some());
    }

    const THIS_ID: &str = "0123456789abcdef0123456789abcdef";
    const OTHER_ID: &str = "fedcba9876543210fedcba9876543210";

    fn sources_body(source: &str, name: &str, connected: bool) -> String {
        serde_json::json!({
            "user": "ada",
            "binding": {
                "delivery": [{"kind": "machine", "id": source}],
                "signing": [{"kind": "machine", "id": source}],
                "bound_at": "2026-11-16T10:00:00Z", "bound_by": "ada", "generation": 1,
            },
            "machines": [{"id": source, "name": name, "connected": connected}],
        })
        .to_string()
    }

    /// One row per host, whatever each answered: a pinning server's row names its
    /// source (cleaned) and says whether it is this machine; an older server is
    /// unsupported with no error; a down host and a refusing one keep their error.
    #[tokio::test]
    async fn source_status_keeps_a_row_per_host() {
        let pinning = MockServer::start_async().await;
        pinning
            .mock_async(|w, t| {
                w.method(GET).path("/api/sources");
                t.status(200)
                    .body(sources_body(OTHER_ID, "desk\u{202E}\u{7}", true));
            })
            .await;
        let mine = MockServer::start_async().await;
        mine.mock_async(|w, t| {
            w.method(GET).path("/api/sources");
            t.status(200).body(sources_body(THIS_ID, "box", false));
        })
        .await;
        let old = MockServer::start_async().await;
        old.mock_async(|w, t| {
            w.method(GET).path("/api/sources");
            t.status(404).body("404 page not found\n");
        })
        .await;
        let refusing = MockServer::start_async().await;
        refusing
            .mock_async(|w, t| {
                w.method(GET).path("/api/sources");
                t.status(403)
                    .body(r#"{"error":{"code":"FORBIDDEN","message":"service clients may not"}}"#);
            })
            .await;
        // Busy on every ask (Retry-After 0 keeps the core's retries instant).
        let busy = MockServer::start_async().await;
        let busy_asks = busy
            .mock_async(|w, t| {
                w.method(GET).path("/api/sources");
                t.status(503).header("retry-after", "0").body(
                    r#"{"error":{"code":"SOURCE_BUSY","message":"being saved; retry in a moment"}}"#,
                );
            })
            .await;
        let rows = backend_with(vec![
            client_at("pinning", pinning.base_url()),
            client_at("mine", mine.base_url()),
            client_at("old", old.base_url()),
            client_at("refusing", refusing.base_url()),
            client_at("down", "http://127.0.0.1:1".to_string()),
            client_at("busy", busy.base_url()),
        ])
        .source_status(Some(THIS_ID))
        .await;
        let row = |h: &str| rows.iter().find(|r| r.host == h).unwrap().clone();
        assert_eq!(rows.len(), 6);

        // Busy is transient: supported, unknown (not "no source"), no error row, and
        // asked the bounded number of times.
        let b = row("busy");
        assert!(b.supported && b.busy, "{b:?}");
        assert!(b.error.is_none() && b.source.is_none(), "{b:?}");
        busy_asks
            .assert_hits_async(shed_core::http::SOURCE_BUSY_ATTEMPTS)
            .await;
        assert!(!row("pinning").busy && !row("refusing").busy && !row("down").busy);

        let p = row("pinning");
        assert!(p.supported && p.error.is_none());
        assert_eq!(
            p.source,
            Some(SourceMachineRow {
                id: OTHER_ID.into(),
                name: "desk".into()
            })
        );
        assert!(!p.this_machine);
        assert!(p.connected);

        let m = row("mine");
        assert!(m.this_machine);
        assert!(!m.connected);

        let o = row("old");
        assert!(!o.supported && o.error.is_none() && o.source.is_none());

        let r = row("refusing");
        assert!(r.supported);
        assert!(
            r.error
                .as_deref()
                .unwrap()
                .contains("service clients may not"),
            "{r:?}"
        );

        let d = row("down");
        assert!(!d.supported && d.error.is_some());
        assert!(
            d.unreachable,
            "a transport failure is unknown, never unsupported: {d:?}"
        );
        // Only those that could not be reached or read are unreachable.
        for h in ["pinning", "mine", "old", "refusing", "busy"] {
            assert!(!row(h).unreachable, "{h}: {:?}", row(h));
        }

        // Without this machine's id, no row is "this machine".
        let rows = backend_with(vec![client_at("mine", mine.base_url())])
            .source_status(None)
            .await;
        assert!(!rows[0].this_machine);
    }

    /// Only a plain 404 means "not supported". A server that cannot be reached or read
    /// right now — a refused connection, a 502 or 503 page with no API error body (a
    /// restarting server behind a proxy), a 200 whose body is not the document — is
    /// `unreachable`: unknown, so whoever shows the row keeps what it last knew and reads
    /// again. Anything else with no API body (a bare 401) stays a plain error row.
    #[tokio::test]
    async fn an_unreachable_server_is_unknown_never_unsupported() {
        let answering = |status: u16, body: &'static str| async move {
            let s = MockServer::start_async().await;
            s.mock_async(|w, t| {
                w.method(GET).path("/api/sources");
                t.status(status).body(body);
            })
            .await;
            s
        };
        let gateway = answering(502, "<html>Bad Gateway</html>").await;
        let restarting = answering(503, "").await;
        let garbled = answering(200, "{not the document").await;
        let bare_401 = answering(401, "").await;
        let old = answering(404, "404 page not found\n").await;
        let rows = backend_with(vec![
            client_at("refused", "http://127.0.0.1:1".to_string()),
            client_at("gateway", gateway.base_url()),
            client_at("restarting", restarting.base_url()),
            client_at("garbled", garbled.base_url()),
            client_at("bare-401", bare_401.base_url()),
            client_at("old", old.base_url()),
        ])
        .source_status(Some(THIS_ID))
        .await;
        let row = |h: &str| rows.iter().find(|r| r.host == h).unwrap().clone();
        for h in ["refused", "gateway", "restarting", "garbled"] {
            let r = row(h);
            assert!(r.unreachable, "{h}: {r:?}");
            assert!(
                r.error.is_some() && r.source.is_none() && !r.busy,
                "{h}: {r:?}"
            );
        }
        let r = row("bare-401");
        assert!(!r.unreachable && r.error.is_some(), "{r:?}");
        let r = row("old");
        assert!(!r.unreachable && !r.supported && r.error.is_none(), "{r:?}");
    }

    #[tokio::test]
    async fn source_use_targets_the_named_host_only() {
        let a = MockServer::start_async().await;
        let moved = a
            .mock_async(|w, t| {
                w.method(POST)
                    .path("/api/sources/use")
                    .json_body(serde_json::json!({ "machine_id": THIS_ID }));
                t.status(200).body(sources_body(THIS_ID, "box", true));
            })
            .await;
        let b = MockServer::start_async().await;
        let untouched = b
            .mock_async(|w, t| {
                w.path_contains("/api/");
                t.status(200);
            })
            .await;
        let backend = backend_with(vec![
            client_at("a", a.base_url()),
            client_at("b", b.base_url()),
        ]);
        let row = backend.source_use("a", THIS_ID).await.unwrap();
        moved.assert_async().await;
        untouched.assert_hits_async(0).await;
        assert_eq!(row.host, "a");
        assert!(row.this_machine && row.connected);
        assert_eq!(row.source.unwrap().name, "box");
        // An unknown host is an error, never the default server.
        let e = backend.source_use("nope", THIS_ID).await.unwrap_err();
        assert!(
            matches!(e, SourceError::Shed(ShedError::Config(_))),
            "{e:?}"
        );
    }

    #[tokio::test]
    async fn from_config_with_unreachable_host_yields_error_row() {
        // The mock-arm per-host down override (backs SHED_*_MOCK_UNREACHABLE_HOSTS):
        // a config-built backend whose `down` server is in the set points that
        // client at a closed port while `mock` reaches the httpmock — so
        // egress_profiles / system_df keep `down` as an error row (the e2e
        // down-host contract, verified at the constructor level rather than only
        // via hand-built clients).
        let mock = MockServer::start_async().await;
        mock.mock_async(|w, t| {
            w.method(GET).path("/api/egress/profiles");
            t.status(200).body(
                r#"[{"name":"default","source":"config","profile":{"allow":["*.github.com"]}}]"#,
            );
        })
        .await;
        mock.mock_async(|w, t| {
            w.method(GET).path("/api/system/df");
            t.status(200).body(
                r#"{"server_name":"mock","backend":"vz","totals":{"all":{"logical_bytes":1,"physical_bytes":1}}}"#,
            );
        })
        .await;
        let config = ShedConfig {
            servers: vec![server_entry("mock"), server_entry("down")],
            default_server: Some("mock".into()),
            ..Default::default()
        };
        let unreachable: HashSet<String> = ["down".to_string()].into_iter().collect();
        let backend =
            Backend::from_config_with_minter(&config, Some(&mock.base_url()), None, &unreachable);

        // egress: `mock` reaches the httpmock (profile decoded); `down` is KEPT as
        // an error row (redirected to the closed port), never a dropped host.
        let egress = backend.egress_profiles().await;
        assert_eq!(egress.len(), 2);
        let m = egress.iter().find(|r| r.host == "mock").unwrap();
        assert!(m.error.is_none());
        assert_eq!(m.profiles.len(), 1);
        let d = egress.iter().find(|r| r.host == "down").unwrap();
        assert!(d.profiles.is_empty());
        assert!(d.error.is_some());

        // system_df: same split — `mock` healthy, `down` an error row with no usage.
        let df = backend.system_df().await;
        assert_eq!(df.len(), 2);
        let m = df.iter().find(|r| r.host == "mock").unwrap();
        assert!(m.usage.is_some() && m.error.is_none());
        let d = df.iter().find(|r| r.host == "down").unwrap();
        assert!(d.usage.is_none() && d.error.is_some());
    }

    #[test]
    fn terminal_preview_targets_the_configured_ssh_endpoint() {
        use shed_core::config::{ShedConfig, ShedServerEntry};
        let config = ShedConfig {
            servers: vec![ShedServerEntry {
                name: "prod".into(),
                host: "10.0.0.9".into(),
                http_port: 8080,
                ssh_port: 2200,
                ..Default::default()
            }],
            default_server: Some("prod".into()),
            ..Default::default()
        };
        let backend = Backend::from_config(&config, Some("http://mock"));
        // host-less → default "prod": ssh web@10.0.0.9 -p 2200, tmux-attaching "main".
        let cmd = backend.terminal_preview(None, "web", Some("main")).unwrap();
        assert!(cmd.argv.contains(&"web@10.0.0.9".to_string()));
        assert!(cmd.command.contains("-p 2200"));
        assert_eq!(
            &cmd.argv[cmd.argv.len() - 4..],
            ["tmux", "attach", "-t", "main"]
        );
        // an unknown host is a config error, not a silent default.
        assert!(backend.terminal_preview(Some("nope"), "web", None).is_err());
    }
}
