//! The shed-core-backed data layer shared by every client: it owns one HTTP
//! client per configured server (+ a create store) and performs the reads,
//! lifecycle actions, and creates. In test mode every client is pointed at the
//! single mock base URL (hermetic), mirroring the Swift ShedBackend/AppModel.
//!
//! Env-agnostic: [`from_env_parts`](Backend::from_env_parts) takes the resolved
//! `(test_mode, mock_base_url, config_path)` rather than a client's `SHED_*_` env
//! struct, so the GTK + Tauri clients share one implementation.

use std::path::Path;
use std::sync::{Arc, LazyLock};

use tokio::runtime::Handle;

use shed_core::config::ShedConfig;
use shed_core::create::{CreateProgress, CreateStore};
use shed_core::http::{Client, ShedError};
use shed_core::models::{CreateShedRequest, Shed, ShedStatus, SystemDiskUsage};
use shed_core::terminal::{self, TerminalCommand};
use shed_core::token::TokenMinter;

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
        Self::from_env_parts_with_minter(test_mode, mock_base_url, config_path, None)
    }

    /// As [`from_env_parts`](Self::from_env_parts), but threads a control-token
    /// `minter` (the host-agent bridge) into every non-mock server's client, so a
    /// secure server mints/refreshes its bearer via `token.get` and fails closed
    /// on a mint failure (F6). GTK passes `None` (static-token only).
    pub fn from_env_parts_with_minter(
        test_mode: bool,
        mock_base_url: Option<&str>,
        config_path: &Path,
        minter: Option<&Arc<dyn TokenMinter>>,
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
        Self::from_config_with_minter(&config, mock_base_url, minter)
    }

    /// Build clients from an already-parsed config. When `mock_base_url` is set
    /// (test mode) every server is redirected to that single hermetic mock —
    /// plain HTTP, no pin, no token — so no real host is touched.
    pub fn from_config(config: &ShedConfig, mock_base_url: Option<&str>) -> Self {
        Self::from_config_with_minter(config, mock_base_url, None)
    }

    /// As [`from_config`](Self::from_config), but threads a control-token `minter`
    /// into every non-mock server (see
    /// [`from_env_parts_with_minter`](Self::from_env_parts_with_minter)). The mock
    /// path never gets a minter (hermetic — a static, tokenless mock).
    pub fn from_config_with_minter(
        config: &ShedConfig,
        mock_base_url: Option<&str>,
        minter: Option<&Arc<dyn TokenMinter>>,
    ) -> Self {
        let clients = config
            .servers
            .iter()
            .filter_map(|s| {
                let client = match mock_base_url {
                    Some(mock) => {
                        Client::new(mock.to_string(), s.name.clone(), String::new(), None, None)
                    }
                    None => {
                        // A pin on a non-https URL fails closed in Client::new → that
                        // server is skipped rather than sent plaintext. Secure servers
                        // mint/refresh their control token via the host agent; on a
                        // mint failure the client sends no token — never the static
                        // config token (F6).
                        let r = s.resolved_endpoint();
                        Client::new(
                            r.base_url,
                            s.name.clone(),
                            s.control_token.clone(),
                            (!r.pin.is_empty()).then_some(r.pin),
                            minter.map(Arc::clone),
                        )
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
    /// unreachable"`) — the Swift AppModel's reachability rollup. Additive:
    /// `list_sheds` keeps its error-dropping contract (GTK uses it and never
    /// surfaces the rollup); the Tauri client surfaces `last_error`.
    pub async fn refresh(&self) -> Reachability {
        let fetches = self
            .clients
            .iter()
            .map(|(name, c)| async move { (name.clone(), c.list_sheds().await) });
        let mut sheds = Vec::new();
        let mut errors = Vec::new();
        for (name, result) in futures::future::join_all(fetches).await {
            match result {
                Ok(mut s) => sheds.append(&mut s),
                Err(e) => errors.push(format!("{name}: {e}")),
            }
        }
        let last_error = match errors.len() {
            0 => None,
            1 => errors.into_iter().next(),
            n => Some(format!("{n} hosts unreachable")),
        };
        Reachability { sheds, last_error }
    }

    /// Per-host disk usage (`GET /api/system/df`) for the System pane. Unlike
    /// `list_sheds`, a per-host failure is KEPT as an error row (the pane shows
    /// which host is unreachable + why), mirroring the Swift `system.df`. Concurrent.
    pub async fn system_df(&self) -> Vec<HostDiskUsage> {
        let fetches = self
            .clients
            .iter()
            .map(|(name, c)| async move { (name.clone(), c.system_df().await) });
        futures::future::join_all(fetches)
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
    /// without touching `Backend`. Mirrors the Swift `rcList` target selection.
    pub async fn rc_targets(&self, host: Option<&str>, shed: Option<&str>) -> Vec<(Shed, RcTarget)> {
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
fn known_hosts_path() -> String {
    // Cached: HOME is stable for the process, and `rc_targets` resolves this once
    // per running shed — no need to re-read the env + reformat each time.
    static PATH: LazyLock<String> = LazyLock::new(|| {
        let home = std::env::var("HOME").unwrap_or_default();
        format!("{home}/.shed/known_hosts")
    });
    PATH.clone()
}

/// All hosts' sheds + a rollup of per-host reachability failures (what the Swift
/// AppModel surfaces as "N hosts unreachable").
#[derive(Debug, Clone, serde::Serialize)]
pub struct Reachability {
    pub sheds: Vec<Shed>,
    pub last_error: Option<String>,
}

/// One host's disk usage, or the error that host returned — the System pane shows
/// an error row per host rather than dropping unreachable hosts (unlike list_sheds).
#[derive(Debug, Clone, serde::Serialize)]
pub struct HostDiskUsage {
    pub host: String,
    pub usage: Option<SystemDiskUsage>,
    pub error: Option<String>,
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

    #[test]
    fn terminal_preview_targets_the_configured_ssh_endpoint() {
        use shed_core::config::{ShedConfig, ShedServerEntry};
        let config = ShedConfig {
            servers: vec![ShedServerEntry {
                name: "prod".into(),
                host: "10.0.0.9".into(),
                http_port: 8080,
                ssh_port: 2200,
                control_token: String::new(),
                api_url: String::new(),
                tls_cert_fingerprint: String::new(),
            }],
            default_server: Some("prod".into()),
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
