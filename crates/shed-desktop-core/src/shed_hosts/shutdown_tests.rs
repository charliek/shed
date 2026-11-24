//! **Orderly shutdown tests** against the real [`ShedHosts`], [`ShedLanes`],
//! [`SpawnGate`], [`ShedDesktopRuntime`], and production [`shutdown_children`]
//! / [`Shutdown`] (plan 026 §3.3, plan 038 D6).
//!
//! Verifies that:
//! - the [`SpawnGate`] write barrier rejects post-shutdown spawns and kills
//!   late-registered children;
//! - app-owned craze bridge children and `SshExec` masters are terminated on
//!   shutdown while a separate remote hub sentinel process is left untouched;
//! - roost watchers, probes, and admitted one-shots are joined before reaches
//!   shut down;
//! - retired transports (from a removed shed) held by a borrower are still
//!   shut down;
//! - concurrent `Shutdown::run` calls share a single run;
//! - phase 4 clamps unconfirmed SSH masters to one `MASTER_SHUTDOWN_BOUND`.

use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicBool, AtomicUsize, Ordering};
use std::time::{Duration, Instant};

use shed_core::config::ShedServerEntry;
use shed_craze::{CrazeDial, CrazeStream, DialError, ExitWatch};
use shed_roost::bootstrap::{Outcome, Stdin};
use shed_roost::testing::FakeRoost;
use shed_roost::{
    ReachError, RoostEndpoint, SpawnGate, SshBridgeOptions, SshEndpoint, SshExec, TeardownSink,
    SHUTTING_DOWN,
};

use super::*;
use crate::shed_craze::{child_stream, gated, BoxFut, CrazeReach, CrazeTimings, GatedDial};
use crate::shed_lanes::{LaneHosts, NoopLaneEvents, ShedLanes};
use crate::shutdown::shutdown_children;
use crate::ShedDesktopRuntime;

fn timings() -> CrazeTimings {
    CrazeTimings {
        dormant: Duration::from_millis(40),
        backoff_base: Duration::from_millis(10),
        backoff_max: Duration::from_millis(40),
    }
}

fn shed(name: &str) -> ShedId {
    ShedId::new("mock", name).unwrap()
}

fn server_config() -> ShedConfig {
    ShedConfig {
        servers: vec![ShedServerEntry {
            name: "mock".to_string(),
            host: "127.0.0.1".to_string(),
            http_port: 8080,
            ssh_port: 2222,
            ..Default::default()
        }],
        ..Default::default()
    }
}

async fn wait_for<T>(what: &str, mut f: impl FnMut() -> Option<T>) -> T {
    for _ in 0..4_000 {
        if let Some(v) = f() {
            return v;
        }
        tokio::time::sleep(Duration::from_millis(5)).await;
    }
    panic!("timed out waiting for {what}");
}

/// Check if `pid` still exists (`kill -0`, portable across Linux & macOS).
fn pid_alive(pid: i32) -> bool {
    std::process::Command::new("kill")
        .args(["-0", &pid.to_string()])
        .stdout(std::process::Stdio::null())
        .stderr(std::process::Stdio::null())
        .status()
        .is_ok_and(|s| s.success())
}

fn pids_in(pids: &Path) -> Vec<i32> {
    std::fs::read_to_string(pids)
        .unwrap_or_default()
        .lines()
        .filter_map(|l| l.trim().parse().ok())
        .collect()
}

/// A real child dial that appends its PID to `pids` and sleeps until killed
/// (simulating an app-owned `ssh ... craze stdio` bridge child).
struct SleepBridgeDial {
    pids: PathBuf,
}

impl CrazeDial for SleepBridgeDial {
    fn dial(&self) -> BoxFut<Result<CrazeStream, DialError>> {
        let pids = self.pids.clone();
        Box::pin(async move {
            let mut cmd = tokio::process::Command::new("/bin/sh");
            cmd.args([
                "-c",
                &format!("printf '%s\\n' $$ >> '{}'; exec sleep 30", pids.display()),
            ])
            .stdin(std::process::Stdio::piped())
            .stdout(std::process::Stdio::piped())
            .stderr(std::process::Stdio::piped())
            .kill_on_drop(true);
            let child = cmd
                .spawn()
                .map_err(|e| DialError::Unreachable(format!("spawn test bridge: {e}")))?;
            child_stream(child)
        })
    }
}

/// Writes an executable script atomically (staged + rename avoids ETXTBSY).
fn write_script(dir: &Path, name: &str, script: &str) -> PathBuf {
    use std::os::unix::fs::PermissionsExt as _;

    let staged = dir.join(format!("{name}.staging"));
    let path = dir.join(name);
    std::fs::write(&staged, script).expect("write script");
    std::fs::set_permissions(&staged, std::fs::Permissions::from_mode(0o700)).expect("chmod");
    std::fs::rename(&staged, &path).expect("rename");
    path
}

async fn first_exec(exec: &SshExec) -> Outcome {
    for _ in 0..20 {
        let outcome = exec
            .run("true", Stdin::Empty, Duration::from_secs(10), 1024, false)
            .await;
        match &outcome {
            Outcome::Exec {
                exit: None,
                stderr_tail,
                ..
            } if stderr_tail.contains("Text file busy") => {
                tokio::time::sleep(Duration::from_millis(50)).await;
            }
            _ => return outcome,
        }
    }
    exec.run("true", Stdin::Empty, Duration::from_secs(10), 1024, false)
        .await
}

fn hosts_with(gate: &SpawnGate, target: &ShedId, craze: Option<CrazeReach>) -> Arc<ShedHosts> {
    let options = ShedReachOptions {
        test_mode: true,
        spawn_gate: Some(gate.clone()),
        teardowns: Some(TeardownSink::default()),
        ..Default::default()
    };
    let target = target.clone();
    Arc::new(ShedHosts::start_with(
        &tokio::runtime::Handle::current(),
        &server_config(),
        options,
        false,
        Arc::new(NoopHostEvents),
        CrazeReaches::Fixed(Arc::new(move |id: &ShedId| {
            (*id == target).then(|| craze.clone()).flatten()
        })),
        timings(),
    ))
}

fn lanes_over(hosts: &Arc<ShedHosts>) -> Arc<ShedLanes> {
    let lanes = Arc::new(ShedLanes::new(
        tokio::runtime::Handle::current(),
        Arc::new(NoopLaneEvents),
        Arc::clone(hosts) as Arc<dyn LaneHosts>,
    ));
    crate::wire_craze_observer(hosts, &lanes);
    lanes
}

#[derive(Default)]
struct SpawnLog {
    dials: AtomicUsize,
    spawned: Mutex<Vec<(Instant, Option<ExitWatch>)>>,
}

struct CountingDial {
    inner: Arc<dyn CrazeDial>,
    log: Arc<SpawnLog>,
}

impl CrazeDial for CountingDial {
    fn dial(&self) -> BoxFut<Result<CrazeStream, DialError>> {
        self.log.dials.fetch_add(1, Ordering::SeqCst);
        let dialing = self.inner.dial();
        let log = Arc::clone(&self.log);
        Box::pin(async move {
            let stream = dialing.await?;
            lock(&log.spawned).push((Instant::now(), stream.exit().cloned()));
            Ok(stream)
        })
    }
}

/// **Gate write barrier**: a spawn section paused mid-spawn blocks `close()`;
/// once it lands after closing, its child is killed at registration and its
/// dial returns `shutting_down`; subsequent dials spawn zero children.
#[tokio::test]
async fn the_gate_is_a_barrier_and_refuses_new_spawns_after_shutdown() {
    let dir = tempfile::tempdir().unwrap();
    let pids = dir.path().join("pids");
    let gate = SpawnGate::new();
    let log = Arc::new(SpawnLog::default());
    let inner: Arc<dyn CrazeDial> = Arc::new(CountingDial {
        inner: Arc::new(SleepBridgeDial { pids: pids.clone() }),
        log: Arc::clone(&log),
    });
    let dial = Arc::new(GatedDial::new(inner, gate.clone()));

    let reached = Arc::new(tokio::sync::Notify::new());
    let proceed = Arc::new(tokio::sync::Notify::new());
    dial.pause_next((Arc::clone(&reached), Arc::clone(&proceed)));

    let racing = tokio::spawn({
        let dial = Arc::clone(&dial);
        async move { dial.dial().await }
    });
    reached.notified().await;

    let closing = tokio::spawn({
        let gate = gate.clone();
        async move { gate.close(Duration::from_secs(2)).await }
    });
    tokio::time::sleep(Duration::from_millis(20)).await;
    assert!(!closing.is_finished(), "close waits for the open section");

    proceed.notify_one();
    assert!(closing.await.unwrap(), "close drained cleanly");
    let err = racing
        .await
        .unwrap()
        .err()
        .expect("racing dial must fail with shutting_down");
    assert!(
        err.to_string().contains(SHUTTING_DOWN),
        "unexpected error: {err}"
    );

    // Every child spawned during the race must have been killed & reaped.
    {
        let spawned = lock(&log.spawned);
        assert_eq!(spawned.len(), 1, "exactly one child spawned during race");
        assert!(
            spawned[0]
                .1
                .as_ref()
                .is_some_and(|exit| exit.now().is_some()),
            "late-registered child's exit must be seen before gate close finishes"
        );
    }
    for pid in pids_in(&pids) {
        wait_for("late child dead", || (!pid_alive(pid)).then_some(())).await;
    }

    // A dial AFTER shutdown begins spawns ZERO children.
    let dials_before = log.dials.load(Ordering::SeqCst);
    let err = dial.dial().await.err().expect("post-close dial refused");
    assert!(err.to_string().contains(SHUTTING_DOWN));
    assert_eq!(log.dials.load(Ordering::SeqCst), dials_before);
}

/// **Production `shutdown_children`**: kills app-owned craze bridge children
/// and `SshExec` masters while leaving an external "remote hub" process
/// untouched, and refuses new spawns afterwards.
#[tokio::test]
async fn production_shutdown_kills_app_children_and_leaves_remote_hub_untouched() {
    let dir = tempfile::tempdir().unwrap();
    let bridge_pids = dir.path().join("bridge-pids");
    let master_log = dir.path().join("master.log");

    // Start a separate process standing in for a persistent remote craze hub
    // that the app does NOT own.
    let mut remote_hub = std::process::Command::new("/bin/sh")
        .args(["-c", "exec sleep 60"])
        .spawn()
        .expect("spawn remote hub sentinel");
    let hub_pid = remote_hub.id() as i32;

    let s1 = shed("s1");
    let gate = SpawnGate::new();
    let craze_reach = CrazeReach {
        dial: gated(
            Arc::new(SleepBridgeDial {
                pids: bridge_pids.clone(),
            }),
            Some(&gate),
        ),
        probe: None,
        ensure: None,
    };
    let hosts = hosts_with(&gate, &s1, Some(craze_reach));
    let lanes = lanes_over(&hosts);

    // Register & start a fake SSH master owned by `hosts.execs` (creates the
    // `-S` control file on normal exec and logs `exit` on `-O exit`).
    let fake_ssh = write_script(
        dir.path(),
        "fake-ssh",
        &format!(
            r#"#!/bin/sh
ctl=
want=0
prev=
op=
for arg in "$@"; do
    if [ "$want" -eq 1 ]; then ctl="$arg"; want=0; fi
    if [ "$arg" = "-S" ]; then want=1; fi
    if [ "$prev" = "-O" ]; then op="$arg"; fi
    prev="$arg"
done
case "$op" in
  exit) printf 'exit\n' >> '{log}'; rm -f "$ctl"; exit 0 ;;
  check) exit 255 ;;
esac
: > "$ctl"
exit 0
"#,
            log = master_log.display()
        ),
    );
    let ep = SshEndpoint {
        name: s1.as_target(),
        host: "127.0.0.1".to_string(),
        user: Some("s1".to_string()),
        ssh_port: 2222,
        known_hosts: None,
        identity_file: None,
    };
    let exec = Arc::new(
        SshExec::new(
            &ep,
            &SshBridgeOptions {
                ssh_bin: Some(fake_ssh),
                spawn_gate: Some(gate.clone()),
                ..Default::default()
            },
        )
        .unwrap(),
    );
    assert!(matches!(
        first_exec(&exec).await,
        Outcome::Exec { exit: Some(0), .. }
    ));
    lock(&hosts.execs).insert(s1.clone(), Arc::clone(&exec));

    // Observe the shed so its craze source spawns a real bridge child.
    hosts
        .observe_sheds(std::slice::from_ref(&s1), &["mock".to_string()])
        .await;
    let bridge_pid = wait_for("bridge child spawned", || {
        pids_in(&bridge_pids).into_iter().next()
    })
    .await;
    assert!(pid_alive(bridge_pid));

    // Run production `shutdown_children` with a source drain hook.
    let source_drained = Arc::new(AtomicBool::new(false));
    let drain: crate::shutdown::SourceDrain = {
        let source_drained = Arc::clone(&source_drained);
        Box::new(move || source_drained.store(true, Ordering::SeqCst))
    };
    let report = shutdown_children(&gate, &hosts, &lanes, Some(&drain)).await;

    assert!(source_drained.load(Ordering::SeqCst));
    assert!(report.gate_drained);
    assert_eq!(report.crazes, 1);
    assert_eq!(report.masters, 1);
    assert_eq!(report.masters_unconfirmed, 0);

    // App-owned bridge child is dead, and master received `-O exit`.
    assert!(
        !pid_alive(bridge_pid),
        "app-owned bridge child {bridge_pid} must be dead after shutdown"
    );
    let log = std::fs::read_to_string(&master_log).unwrap_or_default();
    assert!(
        log.lines().any(|l| l.trim() == "exit"),
        "expected SshExec master `-O exit`, got:\n{log}"
    );

    // Remote hub sentinel (not owned by the app) is STILL alive!
    assert!(
        pid_alive(hub_pid),
        "remote craze hub {hub_pid} must remain untouched by shutdown"
    );
    let _ = remote_hub.kill();
    let _ = remote_hub.wait();

    // New operations after shutdown are refused cleanly.
    let probe_err = hosts.probe(&s1).await.unwrap_err();
    assert!(probe_err.message.contains(SHUTTING_DOWN));
    let open_err = lanes.open(&s1, "aaaaaaaaaaaa").await.unwrap_err();
    assert_eq!(open_err.code(), SHUTTING_DOWN);
}

/// **Roost clients joined before reaches shut down**: a watcher and a retired
/// reach held by a borrower are both torn down cleanly in phase 0 / phase 3.
#[tokio::test]
async fn roost_clients_joined_before_reaches_and_retired_transports_shut_down() {
    struct TrackedReach {
        endpoint: RoostEndpoint,
        shutdown_order: Arc<Mutex<Vec<&'static str>>>,
        shutdowns: AtomicUsize,
    }

    #[async_trait::async_trait]
    impl RoostReach for TrackedReach {
        fn label(&self) -> &str {
            "tracked"
        }
        async fn ensure(&self) -> Result<RoostEndpoint, ReachError> {
            Ok(self.endpoint.clone())
        }
        async fn invalidate(&self) {}
        async fn shutdown(&self) {
            self.shutdowns.fetch_add(1, Ordering::SeqCst);
            lock(&self.shutdown_order).push("reach_shutdown");
        }
    }

    let fake = FakeRoost::start().await;
    let s1 = shed("s1");
    let s2 = shed("s2");
    let gate = SpawnGate::new();
    let hosts = hosts_with(&gate, &s1, None);
    let lanes = lanes_over(&hosts);

    let order = Arc::new(Mutex::new(Vec::new()));
    let r1 = Arc::new(TrackedReach {
        endpoint: RoostEndpoint::Unix(fake.socket_path().to_path_buf()),
        shutdown_order: Arc::clone(&order),
        shutdowns: AtomicUsize::new(0),
    });
    let r2 = Arc::new(TrackedReach {
        endpoint: RoostEndpoint::Unix(fake.socket_path().to_path_buf()),
        shutdown_order: Arc::clone(&order),
        shutdowns: AtomicUsize::new(0),
    });

    // Register s1 (watched) and s2 (then detach s2 while a borrower keeps `r2`,
    // and attach a detached `SshExec` on s2 with NO external borrower to prove
    // Fix 10 retains strong retired execs until shutdown).
    let dir = tempfile::tempdir().unwrap();
    let retired_master_log = dir.path().join("retired-master.log");
    let fake_ssh = write_script(
        dir.path(),
        "fake-ssh-retired",
        &format!(
            r#"#!/bin/sh
ctl=
want=0
prev=
op=
for arg in "$@"; do
    if [ "$want" -eq 1 ]; then ctl="$arg"; want=0; fi
    if [ "$arg" = "-S" ]; then want=1; fi
    if [ "$prev" = "-O" ]; then op="$arg"; fi
    prev="$arg"
done
case "$op" in
  exit) printf 'exit\n' >> '{log}'; rm -f "$ctl"; exit 0 ;;
  check) exit 255 ;;
esac
: > "$ctl"
exit 0
"#,
            log = retired_master_log.display()
        ),
    );
    let ep2 = SshEndpoint {
        name: s2.as_target(),
        host: "127.0.0.1".to_string(),
        user: Some("s2".to_string()),
        ssh_port: 2222,
        known_hosts: None,
        identity_file: None,
    };
    let exec2 = Arc::new(
        SshExec::new(
            &ep2,
            &SshBridgeOptions {
                ssh_bin: Some(fake_ssh),
                spawn_gate: Some(gate.clone()),
                ..Default::default()
            },
        )
        .unwrap(),
    );
    assert!(matches!(
        first_exec(&exec2).await,
        Outcome::Exec { exit: Some(0), .. }
    ));

    let s1_gen = {
        let mut state = lock(&hosts.state);
        let mut reg = lock(&hosts.reg);
        for (id, r) in [
            (s1.clone(), Arc::clone(&r1) as Arc<dyn RoostReach>),
            (s2.clone(), Arc::clone(&r2) as Arc<dyn RoostReach>),
        ] {
            reg.ids.push(id.clone());
            reg.reaches.insert(id.clone(), r);
            state.insert(id, HostState::new(true));
        }
        lock(&hosts.execs).insert(s2.clone(), exec2);
        state.get(&s1).unwrap().reg_gen
    };
    hosts.watch(&s1, s1_gen);
    hosts.detach(&s2).await; // r2 moves into `retired.reaches` (weak); exec2 moves into `retired.execs` (strong, zero external borrowers!)

    let report = shutdown_children(&gate, &hosts, &lanes, None).await;
    assert_eq!(report.clients.watchers, 1);
    assert_eq!(report.clients.watchers_left, 0);
    assert_eq!(report.reaches, 2, "both live and retired reaches shut down");
    assert_eq!(
        report.masters, 1,
        "strong retired SshExec swept at shutdown"
    );
    assert_eq!(report.masters_unconfirmed, 0);
    assert_eq!(r1.shutdowns.load(Ordering::SeqCst), 1);
    assert_eq!(r2.shutdowns.load(Ordering::SeqCst), 1);
    let log = std::fs::read_to_string(&retired_master_log).unwrap_or_default();
    assert!(
        log.lines().any(|l| l.trim() == "exit"),
        "expected retired SshExec master `-O exit`, got:\n{log}"
    );
}

/// **Fix 8**: `ShedDesktopRuntime` owns ONE `Arc<Shutdown>` shared by
/// `shutdown_coordinator()` and `shutdown_children()`; concurrent callers share
/// a single shutdown execution and the source drain runs exactly once.
#[tokio::test]
async fn shutdown_coordinator_is_idempotent_across_concurrent_callers() {
    let runtime = ShedDesktopRuntime::start_headless(
        tokio::runtime::Handle::current(),
        &server_config(),
        ShedReachOptions {
            test_mode: true,
            ..Default::default()
        },
        false,
    );
    let source_drains = Arc::new(AtomicUsize::new(0));
    runtime.set_source_drain({
        let source_drains = Arc::clone(&source_drains);
        Box::new(move || {
            source_drains.fetch_add(1, Ordering::SeqCst);
        })
    });
    let coord_a = runtime.shutdown_coordinator();
    let coord_b = runtime.shutdown_coordinator();
    assert!(Arc::ptr_eq(&coord_a, &coord_b));
    assert!(!coord_a.is_done());

    let (_r1, _r2, _r3) = tokio::join!(coord_a.run(), coord_b.run(), runtime.shutdown_children(),);
    assert!(coord_a.is_done());
    assert_eq!(source_drains.load(Ordering::SeqCst), 1);
    // Subsequent calls return immediately without re-running.
    let _ = runtime.shutdown_children().await;
    assert_eq!(source_drains.load(Ordering::SeqCst), 1);
}
