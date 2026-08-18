//! The stateful Remote-Control layer (feature `rc`): an in-memory session store
//! (the mac `rcTable` analog) over a process-execution seam, on top of the pure
//! `shed_core::rc`. Ported from the Swift `AppModel` `rcLaunch`/`rcKill`/`rcList`/
//! `rcInjectTest`/`listReal` + `ProcessRunner`.
//!
//! **The `RcRunner` trait is the portability boundary, not just a test seam.**
//! Desktop spawns `ssh` subprocesses via [`TokioProcessRunner`]; mobile (iOS/
//! Android) can't spawn subprocesses, so a future mobile client plugs an
//! in-process-ssh / relay runner in here; unit tests use a `FakeRunner`. One
//! [`RcService`] therefore serves every frontend (Swift-FFI, Tauri desktop,
//! mobile, a future headless Rust CLI). `RcService` is frontend-agnostic — the
//! caller resolves `RcTarget`s (from [`Backend`](crate::Backend)) and passes them
//! in — so it stays a clean (future) UniFFI unit; `new_default` wires the real
//! runner so FFI consumers never foreign-implement the async trait.

use std::collections::{HashMap, HashSet};
use std::process::Stdio;
use std::sync::{Arc, Mutex};
use std::time::Duration;

use async_trait::async_trait;
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::process::Command;

use shed_core::models::Shed;
use shed_core::rc::{self, RcCapabilities, RcClassification, RcError, RcKind, RcSession, RcState};

use crate::backend::RcTarget;
use crate::traits::{system_clock, ClockRef};

/// ssh `ConnectTimeout` — bounds connection setup only, not a hung remote command.
const CONNECT_TIMEOUT_SECS: u32 = 10;
/// Per-op process watchdogs (mirroring the Swift `AppModel`): `create --wait`
/// blocks ~20s inside the shed → give it headroom; list/kill are quick.
const CREATE_TIMEOUT: Duration = Duration::from_secs(30);
const LIST_TIMEOUT: Duration = Duration::from_secs(15);
const KILL_TIMEOUT: Duration = Duration::from_secs(10);

/// The captured result of running an external command.
#[derive(Debug, Clone)]
pub struct RunOutput {
    pub stdout: String,
    pub stderr: String,
    pub exit_code: i32,
}

/// The process-execution seam (the RC portability boundary). Runs `argv`
/// (resolved on PATH via `/usr/bin/env`), optionally feeding `stdin`, and returns
/// captured output; must terminate a process that overruns `timeout`.
#[async_trait]
pub trait RcRunner: Send + Sync {
    async fn run(
        &self,
        argv: Vec<String>,
        stdin: Option<String>,
        timeout: Duration,
    ) -> std::io::Result<RunOutput>;
}

pub type RcRunnerRef = Arc<dyn RcRunner>;

/// The real runner: spawns `/usr/bin/env argv` via tokio, draining stdout/stderr
/// concurrently (so a large stdout can't deadlock against unread stderr) and
/// terminating a process that overruns `timeout` (exit 124). Mirrors the Swift
/// `ProcessRunner`.
pub struct TokioProcessRunner;

#[async_trait]
impl RcRunner for TokioProcessRunner {
    async fn run(
        &self,
        argv: Vec<String>,
        stdin: Option<String>,
        timeout: Duration,
    ) -> std::io::Result<RunOutput> {
        let mut child = Command::new("/usr/bin/env")
            .args(&argv)
            .stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .stderr(Stdio::piped())
            .spawn()?;

        // Drain both pipes concurrently, started before we write stdin.
        let mut out = child.stdout.take().expect("piped stdout");
        let mut err = child.stderr.take().expect("piped stderr");
        let out_task = tokio::spawn(async move {
            let mut buf = Vec::new();
            let _ = out.read_to_end(&mut buf).await;
            buf
        });
        let err_task = tokio::spawn(async move {
            let mut buf = Vec::new();
            let _ = err.read_to_end(&mut buf).await;
            buf
        });
        // Write + close stdin (dropping the handle closes it), then wait — both
        // INSIDE the watchdog.
        //
        // The contract is no longer "a short prompt that cannot block": stdin
        // now carries plan documents up to `PLAN_MAX_BYTES` (1 MiB), far past
        // any pipe buffer, so `write_all` genuinely blocks until the far side
        // drains it. That is fine — ssh reads its stdin continuously — but it
        // means (a) the write must be bounded by the same watchdog as the wait,
        // or a wedged reader hangs the caller forever, and (b) a short/failed
        // write must PROPAGATE: silently swallowing it ships a truncated plan
        // and the remote agent starts work on half a document.
        let write_then_wait = async {
            if let Some(mut si) = child.stdin.take() {
                if let Some(s) = &stdin {
                    si.write_all(s.as_bytes()).await?;
                }
            }
            child.wait().await
        };
        // Bound to a `let` so the watchdog future — which holds `&mut child` —
        // is dropped HERE. As a match scrutinee it would live to the end of the
        // match and the timeout arm could not reach `child.start_kill()`.
        let waited = tokio::time::timeout(timeout, write_then_wait).await;
        match waited {
            Ok(status) => {
                // Completed within the watchdog: drain both pipes (they EOF as the
                // exited child's write-ends close).
                let stdout =
                    String::from_utf8_lossy(&out_task.await.unwrap_or_default()).into_owned();
                let stderr =
                    String::from_utf8_lossy(&err_task.await.unwrap_or_default()).into_owned();
                Ok(RunOutput {
                    stdout,
                    stderr,
                    exit_code: status?.code().unwrap_or(-1),
                })
            }
            Err(_) => {
                // Overran the watchdog: terminate + reap FIRST so the pipes EOF,
                // THEN drain. Draining before the kill would block forever on a hung
                // child that holds stdout open (the exact hung-remote case the
                // watchdog exists for — `read_to_end` never returns without EOF).
                // Report a timeout (exit 124), matching the Swift `ProcessRunner`
                // (whose watchdog terminates concurrently with the reads).
                // ConnectTimeout only bounds SSH setup, not a hung remote command.
                let _ = child.start_kill();
                let _ = child.wait().await;
                let stdout =
                    String::from_utf8_lossy(&out_task.await.unwrap_or_default()).into_owned();
                err_task.abort();
                Ok(RunOutput {
                    stdout,
                    stderr: "operation timed out".to_string(),
                    exit_code: 124,
                })
            }
        }
    }
}

/// The stateful RC service over the runner seam. `test_mode` synthesizes ready
/// sessions into the store (the hermetic-harness path, mirroring
/// `AppModel.rcLaunch` test-mode); the real path shells out via the `RcRunner`.
pub struct RcService {
    store: Mutex<HashMap<String, RcSession>>,
    /// Per-shed RC capabilities captured from the `list` envelope, keyed by the
    /// `host/shed` composite. Feeds the launch UI's kind gating (which agents this
    /// shed has installed). Absent for a shed whose binary predates capabilities.
    capabilities: Mutex<HashMap<String, RcCapabilities>>,
    runner: RcRunnerRef,
    clock: ClockRef,
    /// Serializes the real (SSH) launch/kill/list so a `list` store-rebuild can't
    /// race a concurrent launch/kill — Tauri serves IPC concurrently, whereas the
    /// Swift app is `@MainActor`-serialized. Test-mode ops are instant + already
    /// serialized by the `store` mutex, so they skip it.
    op_guard: tokio::sync::Mutex<()>,
    test_mode: bool,
    tool_version: String,
}

impl RcService {
    /// Production: the real `TokioProcessRunner` + system clock. FFI consumers call
    /// this and never touch the `RcRunner` seam.
    pub fn new_default(test_mode: bool, tool_version: impl Into<String>) -> Self {
        Self::with_parts(
            Arc::new(TokioProcessRunner),
            system_clock(),
            test_mode,
            tool_version,
        )
    }

    /// Inject the seams (a `FakeRunner` / `FakeClock`) — for unit tests.
    pub fn with_parts(
        runner: RcRunnerRef,
        clock: ClockRef,
        test_mode: bool,
        tool_version: impl Into<String>,
    ) -> Self {
        Self {
            store: Mutex::new(HashMap::new()),
            capabilities: Mutex::new(HashMap::new()),
            runner,
            clock,
            op_guard: tokio::sync::Mutex::new(()),
            test_mode,
            tool_version: tool_version.into(),
        }
    }

    /// The pure pane classifier (backs the `rc.classify` IPC utility).
    pub fn classify(&self, kind: &RcKind, pane: &str) -> RcClassification {
        rc::classify_pane(kind, pane)
    }

    /// Launch an RC session in `shed` on `target`. Validates the name/workdir/prompt
    /// (before the test-mode branch, so the hermetic harness exercises it), then
    /// either synthesizes a ready session (test mode) or runs `shed-ext-rc create
    /// --wait` over SSH and decodes the DTO.
    pub async fn launch(
        &self,
        target: RcTarget,
        shed: &str,
        kind: RcKind,
        display_name: Option<String>,
        workdir: Option<String>,
        initial_prompt: Option<String>,
    ) -> Result<RcSession, RcError> {
        let slug = generate_slug();
        // The app picks the slug so it can build the `<shed>/<slug>` display name.
        let name = display_name.unwrap_or_else(|| format!("{shed}/{slug}"));
        let created_by = format!("{}/{}", rc::TOOL_NAME, self.tool_version);
        let target_label = format!("shed:{shed}@{}", target.server_name);
        // Reject control chars: a newline would corrupt the SHED_RC_* env / DTO.
        if !rc::is_safe_rc_value(&name)
            || workdir.as_deref().is_some_and(|w| !rc::is_safe_rc_value(w))
        {
            return Err(RcError::BadRequest(
                "display name and workdir must not contain newlines or control characters".into(),
            ));
        }
        let prompt = rc::normalize_rc_prompt(initial_prompt.as_deref(), &kind)?;

        if self.test_mode {
            // No SSH under the harness — synthesize a ready session carrying the
            // managed metadata the hermetic tests assert. Compute the (claude-only)
            // synthetic url before the struct so `kind` can then be moved in.
            let url = rc::synthetic_url(&kind, &slug);
            let session = RcSession {
                host: target.server_name.clone(),
                shed: shed.to_string(),
                slug: slug.clone(),
                tmux_session: rc::tmux_name(&slug),
                display_name: name,
                workdir: Some(workdir.unwrap_or_else(|| rc::DEFAULT_WORKDIR.to_string())),
                kind,
                state: RcState::Ready,
                // Every kind is tui-laned in this phase (the guest derives the
                // same value from the kind's spec on the real path).
                lane: Some(rc::LANE_TUI.to_string()),
                url,
                rc_id: Some(uuid::Uuid::new_v4().to_string()),
                created_by: Some(created_by),
                created_at: Some(self.clock.now_iso8601()),
                target_label: Some(target_label),
                activity: None,
                activity_at: None,
                last_message: None,
                pending_approvals: None,
                managed: true,
            };
            self.store
                .lock()
                .unwrap()
                .insert(session.id(), session.clone());
            return Ok(session);
        }

        // Real path — serialized against list/kill.
        let _guard = self.op_guard.lock().await;
        // Build argv + stdin together so `--prompt-stdin` and the payload can't disagree.
        // The invocation is the permission-mode validating gate; desktop has no
        // permission-mode UI yet, and a `None` mode can never fail validation, so
        // the `?` here is unreachable in practice (argv stays byte-identical).
        let (argv, stdin) = rc::create_invocation(
            &binary_name(),
            &kind,
            &name,
            &slug,
            workdir.as_deref(),
            &created_by,
            &target_label,
            None,
            prompt.as_deref(),
        )?;
        let out = self
            .runner
            .run(ssh_for(shed, &target, &argv), stdin, CREATE_TIMEOUT)
            .await
            .map_err(|e| RcError::Failed(format!("ssh failed: {e}")))?;
        if out.exit_code != 0 {
            return Err(rc::error_from_exit(out.exit_code, &out.stderr, &out.stdout));
        }
        let session =
            RcSession::from_dto(rc::decode_session(&out.stdout)?, &target.server_name, shed);
        self.store
            .lock()
            .unwrap()
            .insert(session.id(), session.clone());
        Ok(session)
    }

    /// Kill an RC session. `shed-ext-rc kill` is idempotent (exit 0 for an
    /// already-gone session); a genuine failure is surfaced but the row is left for
    /// a refresh to reconcile. The store entry is removed on success (or test mode).
    pub async fn kill(&self, target: RcTarget, shed: &str, slug: &str) -> Result<(), RcError> {
        let id = rc::composite_id(&target.server_name, shed, slug);
        if !self.test_mode {
            let _guard = self.op_guard.lock().await;
            let out = self
                .runner
                .run(
                    ssh_for(shed, &target, &rc::kill_argv(&binary_name(), slug)),
                    None,
                    KILL_TIMEOUT,
                )
                .await
                .map_err(|e| RcError::Failed(format!("ssh failed: {e}")))?;
            if out.exit_code != 0 {
                return Err(rc::error_from_exit(out.exit_code, &out.stderr, &out.stdout));
            }
        }
        self.store.lock().unwrap().remove(&id);
        Ok(())
    }

    /// List RC sessions. Real path: probe each running shed in `targets`
    /// concurrently (`shed-ext-rc list`) and reconcile the results into the store;
    /// test mode: just filter the store. `targets` is resolved + filtered by the
    /// caller (`Backend::rc_targets`).
    pub async fn list(
        &self,
        targets: Vec<(Shed, RcTarget)>,
        host: Option<&str>,
        shed: Option<&str>,
    ) -> Vec<RcSession> {
        if !self.test_mode {
            let _guard = self.op_guard.lock().await;
            // The (host, shed) pairs we're refreshing — used to reconcile without
            // clobbering sessions on sheds we didn't probe.
            // Use target.server_name (same key as launch/kill), not shed_item.host.
            let probed: HashSet<(String, String)> = targets
                .iter()
                .map(|(s, target)| (target.server_name.clone(), s.name.clone()))
                .collect();
            let bin = binary_name();
            let probes = targets.into_iter().map(|(shed_item, target)| {
                let runner = Arc::clone(&self.runner);
                let bin = bin.clone();
                async move {
                    let argv = ssh_for(&shed_item.name, &target, &rc::list_argv(&bin));
                    let (server, shed) = (target.server_name.clone(), shed_item.name.clone());
                    match runner.run(argv, None, LIST_TIMEOUT).await {
                        // Decode the envelope once, moving the sessions + capabilities
                        // straight out. A bad DTO degrades to none — best-effort,
                        // mirroring the Swift `listReal`.
                        Ok(out) if out.exit_code == 0 => {
                            let (sessions, caps) = match rc::decode_list_response(&out.stdout) {
                                Ok(resp) => (
                                    resp.rc_sessions
                                        .into_iter()
                                        .map(|dto| RcSession::from_dto(dto, &server, &shed))
                                        .collect::<Vec<_>>(),
                                    resp.capabilities,
                                ),
                                Err(_) => (Vec::new(), None),
                            };
                            (server, shed, sessions, caps)
                        }
                        // A per-shed transport/exit failure yields none.
                        _ => (server, shed, Vec::new(), None),
                    }
                }
            });
            let probe_results = futures::future::join_all(probes).await;
            // Reconcile (gotcha #5) — collected first, so the std Mutex is never held
            // across an await; op_guard already serializes this against launch/kill.
            let mut store = self.store.lock().unwrap();
            let mut caps = self.capabilities.lock().unwrap();
            if host.is_none() && shed.is_none() {
                // Unfiltered full refresh: `targets` is every running shed, so the
                // fresh set IS the whole truth — drop everything else (sessions on
                // sheds since stopped/deleted), matching Swift's full-table rebuild
                // (`rcTable = Dictionary(lists…)`).
                store.clear();
                caps.clear();
            } else {
                // Filtered: replace only the probed sheds' prior entries, preserving
                // other sheds' sessions — a blanket rebuild would wrongly clobber
                // them (a latent bug in Swift's unconditional rebuild).
                store.retain(|_, s| !probed.contains(&(s.host.clone(), s.shed.clone())));
                caps.retain(|k, _| !probed.iter().any(|(h, n)| *k == caps_key(h, n)));
            }
            for (server, shed, sessions, shed_caps) in probe_results {
                for s in sessions {
                    store.insert(s.id(), s);
                }
                // Only record capabilities a probe actually returned — an old
                // binary's bare envelope (None) leaves no stale entry.
                if let Some(c) = shed_caps {
                    caps.insert(caps_key(&server, &shed), c);
                }
            }
        }
        let store = self.store.lock().unwrap();
        let mut result: Vec<RcSession> = store
            .values()
            .filter(|s| host.is_none_or(|h| s.host == h) && shed.is_none_or(|n| s.shed == n))
            .cloned()
            .collect();
        result.sort_by_key(|s| s.id());
        result
    }

    /// The RC capabilities captured for the probed sheds during the last `list`,
    /// keyed by the `host/shed` composite and filtered by the same optional
    /// host/shed as the sessions. The launch UI reads the selected shed's block to
    /// gate which kinds it offers; a shed with an old binary (no capabilities in its
    /// envelope) is simply absent from the map (the UI degrades to claude+shell).
    pub fn capabilities(
        &self,
        host: Option<&str>,
        shed: Option<&str>,
    ) -> HashMap<String, RcCapabilities> {
        let caps = self.capabilities.lock().unwrap();
        caps.iter()
            .filter(|(k, _)| {
                let (h, n) = k.split_once('/').unwrap_or((k.as_str(), ""));
                host.is_none_or(|want| h == want) && shed.is_none_or(|want| n == want)
            })
            .map(|(k, v)| (k.clone(), v.clone()))
            .collect()
    }

    /// Inject a full session into the store directly — test-only (a legacy/
    /// unmanaged row for an e2e screenshot). Errors outside test mode (the ipc
    /// layer guards it first with `not_enabled`).
    pub fn inject_test(&self, session: RcSession) -> Result<(), RcError> {
        if !self.test_mode {
            return Err(RcError::Failed("rc.inject_test requires test mode".into()));
        }
        self.store.lock().unwrap().insert(session.id(), session);
        Ok(())
    }
}

/// Build the non-interactive ssh argv for a remote `shed-ext-rc` command against
/// `target` (the shed name is the ssh user). Shared by launch/kill/list.
fn ssh_for(shed: &str, target: &RcTarget, remote_argv: &[String]) -> Vec<String> {
    rc::ssh_argv(
        shed,
        &target.ssh_host,
        target.ssh_port,
        &target.known_hosts,
        remote_argv,
        CONNECT_TIMEOUT_SECS,
    )
}

/// The `host/shed` composite key under which a shed's RC capabilities are stored.
fn caps_key(host: &str, shed: &str) -> String {
    format!("{host}/{shed}")
}

/// Generate a 6-char confusable-free slug. Derived from a v4 UUID's random bytes
/// to avoid pulling an RNG dep into `shed-core`; lives HERE (the stateful layer,
/// where the mac app also generates it), not in the pure crate.
fn generate_slug() -> String {
    // Confusable-free alphabet (no i, l, o, 0, 1) — matches the convention.
    const ALPHA: &[u8] = b"abcdefghjkmnpqrstuvwxyz23456789";
    let uuid = uuid::Uuid::new_v4();
    uuid.as_bytes()[..6]
        .iter()
        .map(|b| ALPHA[*b as usize % ALPHA.len()] as char)
        .collect()
}

/// The `shed-ext-rc` binary name (or path). Defaults to `shed-ext-rc` (on PATH in
/// the shed `full` image); overridable via `SHED_EXT_RC_BIN` for dev/proof. Read
/// here (the stateful layer), keeping `shed-core::rc` env-free.
fn binary_name() -> String {
    std::env::var("SHED_EXT_RC_BIN").unwrap_or_else(|_| "shed-ext-rc".to_string())
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::traits::Clock;

    struct FakeRunner {
        stdout: String,
        exit_code: i32,
        calls: Mutex<Vec<Vec<String>>>,
    }
    impl FakeRunner {
        fn ok(stdout: &str) -> Arc<Self> {
            Arc::new(Self {
                stdout: stdout.to_string(),
                exit_code: 0,
                calls: Mutex::new(Vec::new()),
            })
        }
        fn failing(exit_code: i32) -> Arc<Self> {
            Arc::new(Self {
                stdout: String::new(),
                exit_code,
                calls: Mutex::new(Vec::new()),
            })
        }
    }
    #[async_trait]
    impl RcRunner for FakeRunner {
        async fn run(
            &self,
            argv: Vec<String>,
            _stdin: Option<String>,
            _timeout: Duration,
        ) -> std::io::Result<RunOutput> {
            self.calls.lock().unwrap().push(argv.clone());
            Ok(RunOutput {
                stdout: self.stdout.clone(),
                stderr: String::new(),
                exit_code: self.exit_code,
            })
        }
    }

    struct FakeClock(i64);
    impl Clock for FakeClock {
        fn now_unix(&self) -> i64 {
            self.0
        }
    }

    fn target() -> RcTarget {
        RcTarget {
            server_name: "srv".into(),
            ssh_host: "10.0.0.5".into(),
            ssh_port: 2222,
            known_hosts: "/k/known_hosts".into(),
        }
    }

    fn test_service(runner: RcRunnerRef, test_mode: bool) -> RcService {
        RcService::with_parts(runner, Arc::new(FakeClock(1_000)), test_mode, "1.2.3")
    }

    #[tokio::test]
    async fn test_mode_launch_synthesizes_ready_session_without_ssh() {
        let runner = FakeRunner::ok("");
        let svc = test_service(runner.clone(), true);
        let s = svc
            .launch(
                target(),
                "web",
                RcKind::ClaudeRc,
                Some("demo".into()),
                None,
                None,
            )
            .await
            .unwrap();
        assert_eq!(s.state, RcState::Ready);
        assert_eq!(s.host, "srv");
        assert_eq!(s.display_name, "demo");
        assert_eq!(s.tmux_session, format!("rc-{}", s.slug));
        assert!(s
            .url
            .unwrap()
            .starts_with("https://claude.ai/code/session_"));
        assert!(s.managed);
        assert!(s.rc_id.is_some());
        assert_eq!(s.created_by.as_deref(), Some("shed-desktop/1.2.3"));
        // Deterministic created_at via the injected clock (a real ISO-8601 `…Z`).
        assert!(s.created_at.as_deref().unwrap().ends_with('Z'));
        assert_eq!(s.target_label.as_deref(), Some("shed:web@srv"));
        // Synthesized, not shelled out.
        assert!(runner.calls.lock().unwrap().is_empty());
        // …and it's in the store.
        let listed = svc.list(vec![], None, None).await;
        assert_eq!(listed.len(), 1);
        assert_eq!(listed[0].slug, s.slug);
    }

    #[tokio::test]
    async fn test_mode_broker_gets_environment_url() {
        let svc = test_service(FakeRunner::ok(""), true);
        let s = svc
            .launch(target(), "web", RcKind::ClaudeBroker, None, None, None)
            .await
            .unwrap();
        assert!(s
            .url
            .unwrap()
            .starts_with("https://claude.ai/code?environment=env_"));
    }

    #[tokio::test]
    async fn test_mode_kill_removes_and_list_filters() {
        let svc = test_service(FakeRunner::ok(""), true);
        let a = svc
            .launch(target(), "web", RcKind::Shell, None, None, None)
            .await
            .unwrap();
        let _b = svc
            .launch(target(), "api", RcKind::Shell, None, None, None)
            .await
            .unwrap();
        assert_eq!(svc.list(vec![], None, None).await.len(), 2);
        // filter by shed
        let only_web = svc.list(vec![], None, Some("web")).await;
        assert_eq!(only_web.len(), 1);
        assert_eq!(only_web[0].shed, "web");
        // kill removes
        svc.kill(target(), "web", &a.slug).await.unwrap();
        assert_eq!(svc.list(vec![], None, None).await.len(), 1);
    }

    #[tokio::test]
    async fn launch_rejects_bad_prompt_before_test_mode_branch() {
        let svc = test_service(FakeRunner::ok(""), true);
        // control char
        assert!(matches!(
            svc.launch(
                target(),
                "web",
                RcKind::ClaudeRc,
                None,
                None,
                Some("bad\nvalue".into())
            )
            .await,
            Err(RcError::BadRequest(_))
        ));
        // prompt for broker
        assert!(matches!(
            svc.launch(
                target(),
                "web",
                RcKind::ClaudeBroker,
                None,
                None,
                Some("nope".into())
            )
            .await,
            Err(RcError::BadRequest(_))
        ));
        // control char in display name
        assert!(matches!(
            svc.launch(
                target(),
                "web",
                RcKind::Shell,
                Some("a\nb".into()),
                None,
                None
            )
            .await,
            Err(RcError::BadRequest(_))
        ));
    }

    #[tokio::test]
    async fn inject_test_inserts_and_guards_test_mode() {
        let legacy = RcSession::from_dto(
            shed_core::rc::RcSessionDto {
                slug: "legacy1".into(),
                tmux_session: "rc-legacy1".into(),
                kind: RcKind::ClaudeBroker,
                state: RcState::Ready,
                // A pre-contract-v2 payload carries no lane at all.
                lane: None,
                managed: false,
                display_name: None,
                workdir: None,
                url: None,
                id: None,
                created_by: None,
                created_at: None,
                target_label: None,
                activity: None,
                activity_at: None,
                last_message: None,
                pending_approvals: None,
            },
            "srv",
            "web",
        );
        let svc = test_service(FakeRunner::ok(""), true);
        svc.inject_test(legacy.clone()).unwrap();
        let listed = svc.list(vec![], None, None).await;
        assert_eq!(listed.len(), 1);
        assert!(!listed[0].managed);
        // Guarded outside test mode.
        let real = test_service(FakeRunner::ok(""), false);
        assert!(real.inject_test(legacy).is_err());
    }

    // ---- real path (FakeRunner, test_mode = false) ----

    const DTO_JSON: &str = r#"{"slug":"abc234","tmux_session":"rc-abc234","kind":"claude-rc","state":"ready","managed":true,"url":"https://claude.ai/code/session_01","id":"uid-9","created_by":"shed-desktop/1.2.3","created_at":"2026-01-01T00:00:00Z","target_label":"shed:web@srv"}"#;

    #[tokio::test]
    async fn real_launch_shells_out_decodes_dto_and_stores() {
        let runner = FakeRunner::ok(DTO_JSON);
        let svc = test_service(runner.clone(), false);
        let s = svc
            .launch(
                target(),
                "web",
                RcKind::ClaudeRc,
                None,
                Some("/w".into()),
                Some("hi".into()),
            )
            .await
            .unwrap();
        // from_dto: the DTO's slug/id win; host/shed injected.
        assert_eq!(s.slug, "abc234");
        assert_eq!(s.rc_id.as_deref(), Some("uid-9"));
        assert_eq!(s.host, "srv");
        assert_eq!(s.id(), "srv/web/abc234");
        // The runner got a NON-interactive ssh argv carrying the create command
        // (scoped so the std MutexGuard isn't held across the later await).
        {
            let calls = runner.calls.lock().unwrap();
            assert_eq!(calls.len(), 1);
            let argv = &calls[0];
            assert_eq!(argv[0], "ssh");
            assert!(!argv.contains(&"-t".to_string()));
            assert!(argv.contains(&"BatchMode=yes".to_string()));
            assert!(argv.contains(&"2222".to_string())); // ssh port
            assert!(argv.last().unwrap().contains("shed-ext-rc create"));
            assert!(argv.last().unwrap().contains("--prompt-stdin"));
            assert!(argv.last().unwrap().contains("--workdir /w"));
        }
        // stored under the composite id (a filtered read, so the launched row
        // isn't pruned by the no-running-targets full refresh).
        assert_eq!(
            svc.list(vec![], Some("srv"), Some("web")).await[0].id(),
            "srv/web/abc234"
        );
    }

    #[tokio::test]
    async fn real_launch_maps_nonzero_exit_to_rc_error() {
        let svc = test_service(FakeRunner::failing(3), false); // 3 = slug taken
        let e = svc
            .launch(target(), "web", RcKind::Shell, None, None, None)
            .await
            .unwrap_err();
        assert!(matches!(e, RcError::SlugTaken(_)));
        // nothing stored on failure
        assert!(svc.list(vec![], None, None).await.is_empty());
    }

    #[tokio::test]
    async fn generate_slug_is_confusable_free_and_6_chars() {
        let s = generate_slug();
        assert_eq!(s.chars().count(), 6);
        assert!(s
            .chars()
            .all(|c| "abcdefghjkmnpqrstuvwxyz23456789".contains(c)));
    }

    // ---- regression: the timeout watchdog actually fires (adversarial finding 1) ----

    #[tokio::test]
    async fn runner_timeout_kills_hung_child_and_returns_124() {
        // A child that holds stdout open past the timeout (the hung-remote case):
        // draining before the kill would block forever. Assert it returns a 124
        // timeout well before the child's own 30s, proving the watchdog fires.
        let started = std::time::Instant::now();
        let out = TokioProcessRunner
            .run(
                vec!["sleep".to_string(), "30".to_string()],
                None,
                Duration::from_millis(300),
            )
            .await
            .unwrap();
        assert_eq!(out.exit_code, 124);
        assert_eq!(out.stderr, "operation timed out");
        assert!(
            started.elapsed() < Duration::from_secs(5),
            "watchdog must fire (~300ms), not wait for the child's 30s"
        );
    }

    /// A plan-sized stdin (`PLAN_MAX_BYTES` is 1 MiB) is orders of magnitude past
    /// a 64 KiB pipe buffer, so `write_all` only completes because the child is
    /// draining concurrently. Pins that the write is inside the watchdog scope
    /// AND that nothing is truncated on the way through.
    #[tokio::test]
    async fn runner_writes_a_plan_sized_stdin_without_truncating_or_deadlocking() {
        let payload = "x".repeat(1024 * 1024);
        let out = TokioProcessRunner
            .run(
                vec!["cat".to_string()],
                Some(payload.clone()),
                Duration::from_secs(30),
            )
            .await
            .unwrap();
        assert_eq!(out.exit_code, 0, "stderr: {}", out.stderr);
        assert_eq!(out.stdout.len(), payload.len());
        assert_eq!(out.stdout, payload);
    }

    /// A child that never reads stdin and never exits: the write blocks once the
    /// pipe fills, and the WATCHDOG — not the caller — is what ends it. Before the
    /// write moved inside the timeout this hung forever.
    #[tokio::test]
    async fn runner_watchdog_bounds_a_wedged_stdin_writer() {
        let started = std::time::Instant::now();
        let out = TokioProcessRunner
            .run(
                vec!["sleep".to_string(), "30".to_string()],
                Some("y".repeat(1024 * 1024)),
                Duration::from_millis(300),
            )
            .await
            .unwrap();
        assert_eq!(out.exit_code, 124);
        assert!(
            started.elapsed() < Duration::from_secs(5),
            "the watchdog must bound the WRITE too, not just the wait"
        );
    }

    // ---- regression: list reconcile prunes stopped sheds but not filtered peers
    //      (adversarial finding 2) ----

    fn shed_running(name: &str, host: &str) -> Shed {
        let mut s: Shed =
            serde_json::from_str(&format!(r#"{{"name":"{name}","status":"running"}}"#)).unwrap();
        s.host = host.to_string();
        s
    }

    /// The list DTO the FakeRunner returns for every probe (one session `s1`).
    const LIST_ONE: &str = r#"{"rc_sessions":[{"slug":"s1","tmux_session":"rc-s1","kind":"shell","state":"ready","managed":true}]}"#;

    #[tokio::test]
    async fn unfiltered_list_prunes_a_stopped_shed() {
        let svc = test_service(FakeRunner::ok(LIST_ONE), false);
        // web running → probe → store has srv/web/s1
        let listed = svc
            .list(vec![(shed_running("web", "srv"), target())], None, None)
            .await;
        assert_eq!(listed.len(), 1);
        assert_eq!(listed[0].id(), "srv/web/s1");
        // web stops → unfiltered refresh with no running targets → pruned (Swift parity).
        assert!(svc.list(vec![], None, None).await.is_empty());
    }

    #[tokio::test]
    async fn filtered_list_preserves_other_hosts_sessions() {
        let svc = test_service(FakeRunner::ok(LIST_ONE), false);
        // full refresh with web + api → srv/web/s1 + srv/api/s1
        let both = svc
            .list(
                vec![
                    (shed_running("web", "srv"), target()),
                    (shed_running("api", "srv"), target()),
                ],
                None,
                None,
            )
            .await;
        assert_eq!(both.len(), 2);
        // A list filtered to web probes only web; it must NOT clobber the api session.
        let web = svc
            .list(
                vec![(shed_running("web", "srv"), target())],
                None,
                Some("web"),
            )
            .await;
        assert_eq!(web.len(), 1);
        // The api session survives (a filtered read keeps un-probed sheds' rows).
        let api = svc.list(vec![], None, Some("api")).await;
        assert_eq!(api.len(), 1);
        assert_eq!(api[0].shed, "api");
    }

    // ---- capabilities captured from the list envelope ----

    /// A list envelope carrying a capabilities block (a current shed-ext-rc).
    const LIST_WITH_CAPS: &str = r#"{
        "rc_sessions":[{"slug":"s1","tmux_session":"rc-s1","kind":"codex","state":"ready","managed":true}],
        "capabilities":{
            "rc_version":3,
            "kinds":["claude-rc","codex","shell"],
            "agents":{"claude":{"installed":true},"codex":{"installed":true,"version":"0.143.0"}},
            "features":["generic-perm"],
            "kind_features":{"codex":{"post_input":true,"approvals":"tui"}}
        }
    }"#;

    #[tokio::test]
    async fn list_captures_capabilities_and_filters_them() {
        let svc = test_service(FakeRunner::ok(LIST_WITH_CAPS), false);
        svc.list(vec![(shed_running("web", "srv"), target())], None, None)
            .await;
        // Captured under the host/shed composite key and offered per the gate.
        let caps = svc.capabilities(Some("srv"), Some("web"));
        let c = caps.get("srv/web").expect("capabilities captured");
        assert_eq!(c.rc_version, 3);
        assert_eq!(
            c.creatable_kinds(),
            vec![RcKind::ClaudeRc, RcKind::Codex, RcKind::Shell]
        );
        // Filter by a different shed → empty.
        assert!(svc.capabilities(Some("srv"), Some("api")).is_empty());
    }

    #[tokio::test]
    async fn old_binary_list_leaves_no_capabilities() {
        // LIST_ONE has no capabilities block (an old baked-in binary).
        let svc = test_service(FakeRunner::ok(LIST_ONE), false);
        svc.list(vec![(shed_running("web", "srv"), target())], None, None)
            .await;
        assert!(svc.capabilities(None, None).is_empty());
    }
}
