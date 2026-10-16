//! **The orderly shutdown** (plan 026 §3.3, shed#389) run over the REAL
//! registries — a [`RoostHosts`], a [`Lanes`], the app's [`SpawnGate`] — with
//! fake children: scripted hubs, a fake roost reach, and real processes a
//! test spawned (`/bin/sh` writing its pid, then `sleep`), whose deaths are
//! checked with `kill(pid, 0)` alone (POSIX; these cells run on macOS too).
//!
//! The lanes phase's own cells — a forward closed through the trait, a pump
//! awaited, an open refused at its gate — are `crate::lane`'s; the forward's
//! slot and the masters' pid fallback are `shed_app`'s.

use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicUsize, Ordering};
use std::time::{Duration, Instant};

use serde_json::json;
use shed_app::roost::{ReachError, RoostEndpoint, SshBridgeOptions, SshExec, TeardownSink};
use shed_app::spawn_gate::{SpawnGate, SHUTTING_DOWN};
use shed_core::config::MachineEntry;
use shed_core::roost::bootstrap::{Outcome, Stdin};
use shed_core::roost::testing::FakeRoost;
use shed_craze::testing::{full_hub_capabilities, HubEnd, ScriptedDial};
use shed_craze::{CrazeDial, CrazeStream, DialError, EnvPolicy, ExitWatch, ProcessDial};
use tokio::sync::mpsc::UnboundedReceiver;

use super::*;
use crate::craze::{gated, BoxFut, CrazeReach, CrazeTimings, GatedDial};
use crate::lane::{LaneMachines, Lanes};
use crate::machines::{ReachKind as Transport, Registered};
use crate::shutdown::{shutdown_children, Shutdown};

const EPOCH: &str = "0a1b2c3d4e5f";

fn timings() -> CrazeTimings {
    CrazeTimings {
        dormant: Duration::from_millis(40),
        backoff_base: Duration::from_millis(10),
        backoff_max: Duration::from_millis(40),
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

/// A layer whose transports spawn inside `gate`, and whose `localhost` craze
/// is `craze` (or none).
fn hosts_with(gate: &SpawnGate, craze: Option<CrazeReach>) -> Arc<RoostHosts> {
    let options = ReachOptions {
        test_mode: true,
        spawn_gate: Some(gate.clone()),
        // The app's shared teardown sink, as `setup` builds it (F4).
        teardowns: Some(TeardownSink::default()),
        ..ReachOptions::default()
    };
    Arc::new(RoostHosts::start_with(
        &tokio::runtime::Handle::current(),
        &ShedConfig::default(),
        options,
        false,
        Arc::new(|| {}),
        CrazeReaches::Fixed(Arc::new(move |id: &HostId| {
            (*id == HostId::Machine(LOCALHOST.to_string()))
                .then(|| craze.clone())
                .flatten()
        })),
        timings(),
    ))
}

fn lanes_over(hosts: &Arc<RoostHosts>) -> Arc<Lanes> {
    Arc::new(Lanes::for_test(
        tokio::runtime::Handle::current(),
        Arc::clone(hosts) as Arc<dyn LaneMachines>,
    ))
}

/// Does `pid` still exist? `kill(pid, 0)` signals nothing.
fn pid_alive(pid: i32) -> bool {
    // SAFETY: signal 0 performs only an existence/permission check.
    unsafe { libc::kill(pid, 0) == 0 }
}

/// A local dial whose every connection is a real child that never answers:
/// `/bin/sh` appends its pid to `pids`, then becomes `sleep`.
fn pid_dial(pids: &Path) -> ProcessDial {
    ProcessDial::new(
        vec![
            "/bin/sh".to_string(),
            "-c".to_string(),
            format!("printf '%s\\n' $$ >> '{}'; exec sleep 30", pids.display()),
        ],
        EnvPolicy::Inherit,
    )
}

fn pids_in(pids: &Path) -> Vec<i32> {
    std::fs::read_to_string(pids)
        .unwrap_or_default()
        .lines()
        .filter_map(|l| l.trim().parse().ok())
        .collect()
}

/// Every dial a fake spawner was asked for, and every SPAWN it made — stamped
/// once its inner dial has answered with the child, inside the spawn section
/// and before the child is registered, so the spawn itself is no later than its
/// stamp (F9: not at the call, which precedes the spawn) — with the child's
/// exit watch when it has one, so a cell can ask whether each child's exit was
/// SEEN. (Each of these children is killed explicitly — at its registration or
/// by the drain — so the clone kept here keeps none alive past that.)
#[derive(Default)]
struct SpawnLog {
    dials: AtomicUsize,
    spawned: std::sync::Mutex<Vec<(Instant, Option<ExitWatch>)>>,
}

impl SpawnLog {
    /// How many dials reached the spawner.
    fn dials(&self) -> usize {
        self.dials.load(Ordering::SeqCst)
    }

    /// How many children it spawned.
    fn count(&self) -> usize {
        self.spawned.lock().unwrap().len()
    }

    /// Each spawn's stamp, in order.
    fn stamps(&self) -> Vec<Instant> {
        self.spawned
            .lock()
            .unwrap()
            .iter()
            .map(|(at, _)| *at)
            .collect()
    }

    /// The spawns later than `at`.
    fn after(&self, at: Instant) -> usize {
        self.stamps()
            .into_iter()
            .filter(|stamp| *stamp > at)
            .count()
    }

    /// The children whose exit has not been seen.
    fn unexited(&self) -> usize {
        self.spawned
            .lock()
            .unwrap()
            .iter()
            .filter(|(_, exit)| exit.as_ref().is_some_and(|exit| exit.now().is_none()))
            .count()
    }
}

/// A dial that records every dial it passes on, and every child it got back.
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
            log.spawned
                .lock()
                .unwrap()
                .push((Instant::now(), stream.exit().cloned()));
            Ok(stream)
        })
    }

    fn fall_back(&self, err: &DialError) -> bool {
        self.inner.fall_back(err)
    }
}

/// Writes `script` to `<dir>/<name>`, executable, under a staging name renamed
/// into place (shed-app's `write_staged_script`: a script still open for
/// writing while a sibling test forks answers `execve` with ETXTBSY).
fn write_script(dir: &Path, name: &str, script: &str) -> PathBuf {
    use std::os::unix::fs::PermissionsExt as _;

    let staged = dir.join(format!("{name}.staging"));
    let path = dir.join(name);
    std::fs::write(&staged, script).expect("write the fake script");
    std::fs::set_permissions(&staged, std::fs::Permissions::from_mode(0o700)).expect("chmod");
    std::fs::rename(&staged, &path).expect("rename into place");
    path
}

/// A runner's first exec, through a fake ssh this test has just written —
/// retried while it answers ETXTBSY: a sibling test thread's fork can hold the
/// script's write descriptor for an instant.
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

/// A machine entry for `name`, as a `machines:` line would give it.
fn machine_entry(name: &str) -> MachineEntry {
    MachineEntry {
        name: name.to_string(),
        host: name.to_string(),
        ssh_port: 22,
        ..MachineEntry::default()
    }
}

/// An eager `localhost` craze on a scripted hub, its dial counted and gated.
fn scripted_reach(gate: &SpawnGate) -> (CrazeReach, UnboundedReceiver<HubEnd>, Arc<SpawnLog>) {
    let (dial, conns) = ScriptedDial::new();
    let log = Arc::new(SpawnLog::default());
    let counting: Arc<dyn CrazeDial> = Arc::new(CountingDial {
        inner: dial,
        log: Arc::clone(&log),
    });
    (
        CrazeReach {
            dial: gated(counting, Some(gate)),
            probe: None,
            ensure: None,
        },
        conns,
        log,
    )
}

async fn next_conn(conns: &mut UnboundedReceiver<HubEnd>) -> HubEnd {
    tokio::time::timeout(Duration::from_secs(10), conns.recv())
        .await
        .expect("a dial in time")
        .expect("the dial is alive")
}

/// Register `id` with `reach`, unwatched — as a probe's registration would.
fn register(hosts: &RoostHosts, id: &HostId, reach: Arc<dyn RoostReach>) {
    let mut state = lock(&hosts.state);
    let mut reg = lock(&hosts.reg);
    reg.ids.push(id.clone());
    reg.reaches.insert(
        id.clone(),
        Registered {
            reach,
            kind: Transport::Local,
        },
    );
    state.insert(id.clone(), HostState::new(false));
}

fn shed(name: &str) -> HostId {
    HostId::Shed {
        server: "mock".to_string(),
        name: name.to_string(),
    }
}

// ---------------------------------------------------------------------------
// the gate's barrier
// ---------------------------------------------------------------------------

/// **The gate is a write barrier** (plan 026 §3.3, R9; F1): a spawn section
/// paused between its check and its `spawn()` makes `close()` WAIT. Its spawn,
/// which lands after the closing flag was set, is killed at its registration on
/// the closed gate — its exit SEEN — before the close returns, and the dial
/// answers `shutting_down`; a spawn attempted after the close answers
/// `shutting_down` and spawns nothing. Neither order leaves a child alive, and
/// nothing is registered alive past the closing flag.
#[tokio::test]
async fn the_gate_is_a_barrier_and_neither_order_leaves_a_child_alive() {
    let dir = tempfile::tempdir().expect("tempdir");
    let pids = dir.path().join("pids");
    let gate = SpawnGate::new();
    let log = Arc::new(SpawnLog::default());
    let counting: Arc<dyn CrazeDial> = Arc::new(CountingDial {
        inner: Arc::new(pid_dial(&pids)),
        log: Arc::clone(&log),
    });
    let dial = Arc::new(GatedDial::new(counting, gate.clone()));
    let (reached, proceed) = (
        Arc::new(tokio::sync::Notify::new()),
        Arc::new(tokio::sync::Notify::new()),
    );
    dial.pause_next((Arc::clone(&reached), Arc::clone(&proceed)));

    let dialing = tokio::spawn({
        let dial = Arc::clone(&dial);
        async move { dial.dial().await }
    });
    reached.notified().await;
    let closing = tokio::spawn({
        let (gate, log) = (gate.clone(), Arc::clone(&log));
        async move {
            let all_ended = gate.close(Duration::from_secs(10)).await;
            // Read AT the close's return: what it left alive.
            (all_ended, log.unexited())
        }
    });
    tokio::time::sleep(Duration::from_millis(200)).await;
    assert!(
        !closing.is_finished(),
        "close() returned while a spawn section was paused before its spawn"
    );
    proceed.notify_one();
    let refused = dialing
        .await
        .expect("the dial task")
        .err()
        .expect("the paused section's registration, on a closed gate, is refused");
    assert!(refused.to_string().starts_with(SHUTTING_DOWN), "{refused}");
    let (all_ended, unexited) = closing.await.expect("close");
    assert!(all_ended, "the close waited for the section");
    assert_eq!(
        unexited, 0,
        "the paused section's child was not killed, its exit seen, before the close returned"
    );
    let flagged = gate.closing_at().expect("the closing flag was set");
    assert_eq!(log.count(), 1, "the paused section spawned its child");
    assert_eq!(
        log.after(flagged),
        1,
        "the paused section's spawn landed after the closing flag"
    );
    assert_eq!(
        gate.registered_exits(),
        0,
        "a child spawned after the closing flag was registered alive"
    );

    let refused = dial
        .dial()
        .await
        .err()
        .expect("a spawn after the close is refused");
    assert!(refused.to_string().starts_with(SHUTTING_DOWN), "{refused}");
    assert_eq!(log.dials(), 1, "the refused dial reached the spawner");

    let drained = gate.kill_exits(Duration::from_secs(2)).await;
    assert_eq!(
        (drained.killed, drained.exited, drained.sections_open),
        (0, 0, 0),
        "the drain was owed a child"
    );
    for pid in pids_in(&pids) {
        assert!(!pid_alive(pid), "pid {pid} survived");
    }
}

/// **A spawn admitted before the close that lands past the close's bound is
/// killed at its registration, and the drain waits for it** (plan 026 §3.3,
/// F1): the close gives up on a paused section at its bound; the drain does not
/// sweep one snapshot and leave — it waits for that section; when the section
/// finally spawns, its registration on the closed gate kills the child and
/// awaits it, and the dial answers `shutting_down`. The child's exit was seen
/// before the shutdown returned.
#[tokio::test]
async fn a_spawn_past_the_closes_bound_is_killed_at_its_registration() {
    let dir = tempfile::tempdir().expect("tempdir");
    let pids = dir.path().join("pids");
    let gate = SpawnGate::new();
    let hosts = hosts_with(&gate, None);
    let lanes = lanes_over(&hosts);
    let log = Arc::new(SpawnLog::default());
    let counting: Arc<dyn CrazeDial> = Arc::new(CountingDial {
        inner: Arc::new(pid_dial(&pids)),
        log: Arc::clone(&log),
    });
    let dial = Arc::new(GatedDial::new(counting, gate.clone()));
    let (reached, proceed) = (
        Arc::new(tokio::sync::Notify::new()),
        Arc::new(tokio::sync::Notify::new()),
    );
    dial.pause_next((Arc::clone(&reached), Arc::clone(&proceed)));
    let dialing = tokio::spawn({
        let dial = Arc::clone(&dial);
        async move { dial.dial().await }
    });
    reached.notified().await;

    let shutting = tokio::spawn({
        let (gate, hosts, lanes, log) = (
            gate.clone(),
            Arc::clone(&hosts),
            Arc::clone(&lanes),
            Arc::clone(&log),
        );
        async move {
            shutdown_children(&gate, &hosts, &lanes, None).await;
            // Read AT the shutdown's return.
            (Instant::now(), log.unexited())
        }
    });
    let gave_up = wait_for("the close to give up on the paused section", || {
        gate.closed_at()
    })
    .await;
    assert_eq!(
        gate.sections(),
        1,
        "the paused section is still open past the close's bound"
    );
    tokio::time::sleep(Duration::from_millis(300)).await;
    assert!(
        !shutting.is_finished(),
        "the shutdown returned with a spawn section still open"
    );
    proceed.notify_one();
    let refused = dialing
        .await
        .expect("the dial task")
        .err()
        .expect("a registration on a closed gate is refused");
    assert!(refused.to_string().starts_with(SHUTTING_DOWN), "{refused}");
    let (returned, unexited) = tokio::time::timeout(Duration::from_secs(20), shutting)
        .await
        .expect("the shutdown returned")
        .expect("the shutdown task");
    let stamps = log.stamps();
    assert_eq!(stamps.len(), 1, "the paused section spawned once");
    assert!(
        stamps[0] > gave_up,
        "the paused section spawned after the close's bound"
    );
    assert!(
        stamps[0] < returned,
        "the paused section spawned after the shutdown returned"
    );
    assert_eq!(
        unexited, 0,
        "the late child's exit was not seen before the shutdown returned"
    );
    assert_eq!(
        gate.registered_exits(),
        0,
        "the late child was registered alive"
    );
    for pid in pids_in(&pids) {
        assert!(!pid_alive(pid), "pid {pid} survived");
    }
}

// ---------------------------------------------------------------------------
// the coordinator over real registries
// ---------------------------------------------------------------------------

/// **A lane verb holding a craze connection cannot keep its bridge alive**
/// (plan 026 §3.3, R11): the bridge child never answers, a verb holds the
/// connection across a 30 s request, and the shutdown still kills the child
/// through its registered `ExitWatch` and sees its exit — within 2 s, while
/// the verb holds on. The verb then fails with its connection closed.
#[tokio::test]
async fn a_lane_verb_holding_a_craze_connection_cannot_keep_its_bridge_alive() {
    let dir = tempfile::tempdir().expect("tempdir");
    let pids = dir.path().join("pids");
    let gate = SpawnGate::new();
    let hosts = hosts_with(&gate, None);
    let lanes = lanes_over(&hosts);
    let dial = GatedDial::new(Arc::new(pid_dial(&pids)), gate.clone());
    let stream = dial.dial().await.expect("a bridge");
    let (conn, _notes) = shed_craze::conn::Conn::start(stream);
    let conn = Arc::new(conn);
    let pid = wait_for("the bridge's pid", || pids_in(&pids).first().copied()).await;

    let verb = tokio::spawn({
        let conn = Arc::clone(&conn);
        async move {
            conn.request("session.send", &json!({}), Duration::from_secs(30))
                .await
        }
    });
    tokio::time::sleep(Duration::from_millis(100)).await;
    assert!(
        !verb.is_finished(),
        "the verb is in flight, holding the connection"
    );

    let started = Instant::now();
    let report = shutdown_children(&gate, &hosts, &lanes, None).await;
    let exit = conn.exit().expect("a process dial has an exit").clone();
    assert!(
        exit.now().is_some(),
        "the held bridge's exit was not seen before the shutdown returned"
    );
    assert!(
        !pid_alive(pid),
        "pid {pid} outlived the shutdown, held by a verb"
    );
    assert!(report.children_killed >= 1, "{report:?}");
    assert!(
        started.elapsed() < Duration::from_secs(4),
        "took {:?}",
        started.elapsed()
    );
    let answer = tokio::time::timeout(Duration::from_secs(5), verb)
        .await
        .expect("the verb returned once its connection closed")
        .expect("the verb task");
    assert!(
        answer.is_err(),
        "the verb's connection closed under it: {answer:?}"
    );
}

/// **A craze source's own dial child is dead when the shutdown returns** — its
/// roster's bridge (a real child that never says `hello`).
#[tokio::test]
async fn a_craze_dial_child_is_dead_on_return() {
    let dir = tempfile::tempdir().expect("tempdir");
    let pids = dir.path().join("pids");
    let gate = SpawnGate::new();
    let reach = CrazeReach {
        dial: gated(Arc::new(pid_dial(&pids)), Some(&gate)),
        probe: None,
        ensure: None,
    };
    let hosts = hosts_with(&gate, Some(reach));
    let lanes = lanes_over(&hosts);
    let pid = wait_for("the roster's bridge", || pids_in(&pids).first().copied()).await;
    assert!(pid_alive(pid));

    shutdown_children(&gate, &hosts, &lanes, None).await;
    for pid in pids_in(&pids) {
        assert!(
            !pid_alive(pid),
            "the craze dial child {pid} outlived the shutdown"
        );
    }
}

/// A roost reach whose `ensure` never answers — every client of it parks
/// there — and which records when each client let go and when it was shut
/// down.
struct ParkedReach {
    label: String,
    events: Arc<std::sync::Mutex<Vec<(Instant, &'static str)>>>,
}

struct Gone(Arc<std::sync::Mutex<Vec<(Instant, &'static str)>>>);

impl Drop for Gone {
    fn drop(&mut self) {
        self.0.lock().unwrap().push((Instant::now(), "client gone"));
    }
}

#[async_trait::async_trait]
impl RoostReach for ParkedReach {
    fn label(&self) -> &str {
        &self.label
    }

    async fn ensure(&self) -> Result<RoostEndpoint, ReachError> {
        let _gone = Gone(Arc::clone(&self.events));
        self.events.lock().unwrap().push((Instant::now(), "client"));
        std::future::pending::<()>().await;
        unreachable!()
    }

    async fn invalidate(&self) {}

    async fn shutdown(&self) {
        self.events
            .lock()
            .unwrap()
            .push((Instant::now(), "shutdown"));
    }
}

/// **Every roost client is joined before any tunnel is shut down** (plan 026
/// §3.3, R10; F2): a watcher, a background probe and an admitted one-shot (a
/// "Run a command in a tab", parked holding its endpoint request) park in
/// their reaches' `ensure`; the shutdown's phase 0 joins all three — the
/// one-shot cancelled past the soft bound, answering the quit's own copy —
/// before phase 3 calls any reach's `shutdown`, so no connection can reach a
/// tunnel's bridge socket while roost tears it down.
#[tokio::test]
async fn every_roost_client_is_joined_before_any_tunnel_shuts_down() {
    let gate = SpawnGate::new();
    let hosts = hosts_with(&gate, None);
    let lanes = lanes_over(&hosts);
    let events = Arc::new(std::sync::Mutex::new(Vec::new()));
    let watched = HostId::Machine("watched".to_string());
    let probed = shed("probed");
    let oneshot = HostId::Machine("oneshot".to_string());
    for id in [&watched, &probed, &oneshot] {
        register(
            &hosts,
            id,
            Arc::new(ParkedReach {
                label: id.token(),
                events: Arc::clone(&events),
            }),
        );
    }
    hosts.watch(&watched);
    hosts.probe_shed_in_background(probed.clone());
    let running = tokio::spawn({
        let hosts = Arc::clone(&hosts);
        let token = oneshot.token();
        async move {
            let command = crate::roost_hosts::RunCommand::parse(Some("true")).expect("a command");
            hosts.run(&token, &command, None).await
        }
    });
    wait_for("all three clients parked in ensure", || {
        (events
            .lock()
            .unwrap()
            .iter()
            .filter(|(_, e)| *e == "client")
            .count()
            == 3)
            .then_some(())
    })
    .await;

    shutdown_children(&gate, &hosts, &lanes, None).await;
    let ran = tokio::time::timeout(Duration::from_secs(5), running)
        .await
        .expect("the one-shot returned")
        .expect("the one-shot's task");
    match &ran {
        Err(message) => assert!(
            message.starts_with(SHUTTING_DOWN) && message.contains("the app quit"),
            "{message}"
        ),
        Ok(answer) => panic!("a one-shot parked through the shutdown answered: {answer}"),
    }
    let events = events.lock().unwrap().clone();
    let gone: Vec<Instant> = events
        .iter()
        .filter(|(_, e)| *e == "client gone")
        .map(|(t, _)| *t)
        .collect();
    let shut: Vec<Instant> = events
        .iter()
        .filter(|(_, e)| *e == "shutdown")
        .map(|(t, _)| *t)
        .collect();
    assert_eq!(gone.len(), 3, "every client was joined: {events:?}");
    assert!(shut.len() >= 3, "every reach was shut down: {events:?}");
    let first_shutdown = shut.iter().min().expect("a shutdown");
    assert!(
        gone.iter().all(|t| t < first_shutdown),
        "a tunnel was shut down before every client was joined: {events:?}"
    );
}

/// A fake `ssh` whose ordinary exec "establishes a master" (creates the
/// control path and returns), whose `-O exit` logs `exit` to `log` and removes
/// it, and whose `-O check` finds no master.
fn master_ssh(log: &Path) -> String {
    format!(
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
        log = log.display()
    )
}

/// **A removed host's transports, still held by a borrower, are drained by
/// the shutdown** (plan 026 §3.3, F4): an admitted op holds a host's exec
/// runner and its bridge across the host's removal — which therefore drops
/// neither — and the shutdown still sends that runner's master `-O exit` and
/// calls that bridge's `shutdown`, from the retired set.
#[tokio::test]
async fn a_removed_hosts_transports_held_by_a_borrower_are_shut_down() {
    let dir = tempfile::tempdir().expect("tempdir");
    let control = dir.path().join("control.log");
    let ssh = write_script(dir.path(), "ssh", &master_ssh(&control));
    let gate = SpawnGate::new();
    let hosts = hosts_with(&gate, None);
    let lanes = lanes_over(&hosts);
    let events = Arc::new(std::sync::Mutex::new(Vec::new()));
    let id = HostId::Machine("retiring".to_string());
    let reach: Arc<dyn RoostReach> = Arc::new(ParkedReach {
        label: id.token(),
        events: Arc::clone(&events),
    });
    register(&hosts, &id, Arc::clone(&reach));
    let exec = Arc::new(
        SshExec::new(
            &machine_entry("retiring"),
            &SshBridgeOptions {
                ssh_bin: Some(ssh),
                ..SshBridgeOptions::default()
            },
        )
        .expect("a runner"),
    );
    let outcome = first_exec(&exec).await;
    assert!(
        matches!(outcome, Outcome::Exec { exit: Some(0), .. }),
        "the runner's first exec established its master: {outcome:?}"
    );
    lock(&hosts.execs).insert(id.clone(), Arc::clone(&exec));

    // The borrower: an op mid-flight holding both.
    let borrowed = (exec, reach);
    hosts.remove(std::slice::from_ref(&id)).await;
    assert!(
        !lock(&hosts.execs).contains_key(&id) && !lock(&hosts.reg).reaches.contains_key(&id),
        "the host was detached"
    );
    assert!(
        std::fs::read_to_string(&control)
            .unwrap_or_default()
            .is_empty(),
        "the detach ended a master its borrower still holds"
    );

    shutdown_children(&gate, &hosts, &lanes, None).await;
    let exits = std::fs::read_to_string(&control).unwrap_or_default();
    assert_eq!(
        exits.lines().filter(|line| *line == "exit").count(),
        1,
        "the shutdown did not send the retired runner's master -O exit: {exits:?}"
    );
    assert!(
        events
            .lock()
            .unwrap()
            .iter()
            .any(|(_, event)| *event == "shutdown"),
        "the shutdown did not shut the retired bridge down"
    );
    drop(borrowed);
    assert_eq!(
        std::fs::read_to_string(&control)
            .unwrap_or_default()
            .lines()
            .count(),
        1,
        "the borrower's drop asked an ended master to exit again"
    );
}

/// **An admitted craze create's automatic retry is refused at the gated
/// dialer** (plan 026 §3.3, R1): the create was admitted before the shutdown,
/// its first attempt's outcome is lost once the gate has closed, and the
/// source's one retry — which redials under the same request id without
/// consulting anything — is refused by the dialer and spawns nothing.
#[tokio::test]
async fn an_admitted_creates_retry_is_refused_by_the_gated_dialer() {
    let gate = SpawnGate::new();
    let (reach, mut conns, log) = scripted_reach(&gate);
    let hosts = hosts_with(&gate, Some(reach));
    let lanes = lanes_over(&hosts);
    let mut roster = next_conn(&mut conns).await;
    roster.hello(EPOCH, full_hub_capabilities()).await;
    roster.subscribed("sub-1", EPOCH, json!([])).await;
    wait_for("the roster to go live", || {
        let status = hosts.status();
        status
            .iter()
            .any(|s| s["name"] == LOCALHOST && s["craze"]["state"] == "live")
            .then_some(())
    })
    .await;

    let create = tokio::spawn({
        let hosts = Arc::clone(&hosts);
        async move {
            let request = crate::craze::create_request(
                Some("/work/new"),
                Some("grok"),
                Some("hello"),
                None,
                Some("shed-retry-1"),
            )
            .expect("a request");
            hosts.craze_create(LOCALHOST, request, false).await
        }
    });
    let mut hub = next_conn(&mut conns).await;
    hub.hello(EPOCH, full_hub_capabilities()).await;
    let req = hub.expect("session.create").await;
    assert_eq!(req["params"]["requestId"], "shed-retry-1");

    // The shutdown begins with the create admitted and its outcome open…
    let orderly = Shutdown::new(
        gate.clone(),
        Arc::clone(&hosts),
        Arc::clone(&lanes),
        None,
        tokio::runtime::Handle::current(),
    );
    let mut done = orderly.start();
    wait_for("the gate to close", || gate.closed_at().map(|_| ())).await;
    // …and only then is the answer lost: the source retries under the same id.
    drop(hub);
    let answer = tokio::time::timeout(Duration::from_secs(10), create)
        .await
        .expect("the create returned")
        .expect("the create task");
    assert!(
        answer.is_err(),
        "a create whose retry was refused cannot succeed: {answer:?}"
    );
    assert_eq!(
        log.dials(),
        2,
        "the retry reached the spawner past the closed gate (the roster's dial and the first attempt only)"
    );
    assert_eq!(
        log.after(gate.closing_at().expect("the closing flag was set")),
        0,
        "a spawn after the closing flag"
    );
    assert!(conns.try_recv().is_err(), "a third connection was made");
    tokio::time::timeout(Duration::from_secs(15), done.wait_for(|d| *d))
        .await
        .expect("the shutdown finished in time")
        .expect("the shutdown's done channel");
    drop(roster);
}

/// A roost reach whose first `ensure` waits for the test, then answers with a
/// real (fake) roost-session's socket.
struct HeldReach {
    socket: PathBuf,
    release: tokio::sync::Notify,
    ensures: AtomicUsize,
}

#[async_trait::async_trait]
impl RoostReach for HeldReach {
    fn label(&self) -> &str {
        "held"
    }

    async fn ensure(&self) -> Result<RoostEndpoint, ReachError> {
        if self.ensures.fetch_add(1, Ordering::SeqCst) == 0 {
            self.release.notified().await;
        }
        Ok(RoostEndpoint::Unix(self.socket.clone()))
    }

    async fn invalidate(&self) {}
}

/// **A probe that completes after the shutdown began promotes nothing** (plan
/// 026 §3.3): its session answers, and it re-checks the closing flag before
/// `watch` — no watcher is installed and none dials the reach.
#[tokio::test]
async fn a_probe_completing_after_shutdown_began_spawns_no_watcher() {
    let gate = SpawnGate::new();
    let hosts = hosts_with(&gate, None);
    let fake = FakeRoost::start().await;
    let reach = Arc::new(HeldReach {
        socket: fake.socket_path().to_path_buf(),
        release: tokio::sync::Notify::new(),
        ensures: AtomicUsize::new(0),
    });
    let id = shed("late");
    register(&hosts, &id, reach.clone() as Arc<dyn RoostReach>);
    hosts.probe_shed_in_background(id.clone());
    wait_for("the probe to dial", || {
        (reach.ensures.load(Ordering::SeqCst) == 1).then_some(())
    })
    .await;

    hosts.begin_closing();
    reach.release.notify_one();
    wait_for("the probe to finish", || {
        lock(&hosts.probing).is_empty().then_some(())
    })
    .await;
    let watchers: Vec<HostId> = lock(&hosts.reg).watchers.keys().cloned().collect();
    assert!(
        !watchers.contains(&id),
        "a probe completing after the shutdown began installed a watcher: {watchers:?}"
    );
    tokio::time::sleep(Duration::from_millis(100)).await;
    assert_eq!(
        reach.ensures.load(Ordering::SeqCst),
        1,
        "something dialled the reach after the probe"
    );
}

/// A fake `ssh` whose every exec is a child that never answers: it appends its
/// pid to `pids`, then becomes `sleep`. `-O check` finds no master.
fn hanging_ssh(pids: &Path) -> String {
    format!(
        r#"#!/bin/sh
prev=
op=
for arg in "$@"; do
    if [ "$prev" = "-O" ]; then op="$arg"; fi
    prev="$arg"
done
case "$op" in
  check) exit 255 ;;
  exit) exit 0 ;;
esac
printf '%s\n' $$ >> '{pids}'
exec sleep 30
"#,
        pids = pids.display()
    )
}

/// **A craze create and a bootstrap racing the shutdown** (plan 026 §3.3; F9):
/// a bootstrap ADMITTED BEFORE the close, its first remote step's child
/// running, is cancelled by phase 0 — it answers the quit's own copy — and
/// that child is dead when the shutdown returns; a create and a bootstrap that
/// arrive after the close are refused at the door, cleanly, and spawn nothing.
#[tokio::test]
async fn a_create_and_a_bootstrap_racing_the_shutdown_are_refused() {
    let dir = tempfile::tempdir().expect("tempdir");
    let pids = dir.path().join("pids");
    let ssh = write_script(dir.path(), "ssh", &hanging_ssh(&pids));
    let gate = SpawnGate::new();
    let (reach, mut conns, log) = scripted_reach(&gate);
    let hosts = hosts_with(&gate, Some(reach));
    let lanes = lanes_over(&hosts);
    let mut roster = next_conn(&mut conns).await;
    roster.hello(EPOCH, full_hub_capabilities()).await;
    roster.subscribed("sub-1", EPOCH, json!([])).await;

    // The admitted bootstrap: a host whose runner — gated, as `exec_for`
    // builds it — hangs in the probe's first remote step.
    let early = HostId::Machine("early".to_string());
    register(
        &hosts,
        &early,
        Arc::new(ParkedReach {
            label: early.token(),
            events: Arc::new(std::sync::Mutex::new(Vec::new())),
        }),
    );
    let exec = SshExec::new(
        &machine_entry("early"),
        &SshBridgeOptions {
            ssh_bin: Some(ssh),
            spawn_gate: Some(gate.clone()),
            ..SshBridgeOptions::default()
        },
    )
    .expect("a runner");
    lock(&hosts.execs).insert(early.clone(), Arc::new(exec));
    let bootstrapping = tokio::spawn({
        let hosts = Arc::clone(&hosts);
        let token = early.token();
        async move { hosts.bootstrap(&token, "fp", true).await }
    });
    let step = wait_for("the bootstrap's first step to spawn", || {
        pids_in(&pids).first().copied()
    })
    .await;
    assert!(pid_alive(step));

    let orderly = Shutdown::new(
        gate.clone(),
        Arc::clone(&hosts),
        Arc::clone(&lanes),
        None,
        tokio::runtime::Handle::current(),
    );
    let mut done = orderly.start();
    wait_for("the shutdown to begin", || gate.is_closed().then_some(())).await;

    let request =
        crate::craze::create_request(Some("/work/new"), None, None, None, Some("shed-late-1"))
            .expect("a request");
    let refused = hosts
        .craze_create(LOCALHOST, request, false)
        .await
        .expect_err("a create after the shutdown began is refused");
    assert_eq!(refused.code, SHUTTING_DOWN, "{refused:?}");
    match hosts.bootstrap("machine:localhost", "fp", true).await {
        Err(refusal) => assert_eq!(refusal.code(), SHUTTING_DOWN, "{refusal:?}"),
        Ok(outcome) => panic!("a bootstrap after the shutdown began ran: {outcome:?}"),
    }
    assert_eq!(log.dials(), 1, "only the roster ever dialled");
    assert!(
        conns.try_recv().is_err(),
        "a connection was made after the shutdown began"
    );

    tokio::time::timeout(Duration::from_secs(15), done.wait_for(|d| *d))
        .await
        .expect("the shutdown finished in time")
        .expect("the shutdown's done channel");
    let survivors: Vec<i32> = pids_in(&pids)
        .into_iter()
        .filter(|pid| pid_alive(*pid))
        .collect();
    assert!(
        survivors.is_empty(),
        "the admitted bootstrap's children {survivors:?} outlived the shutdown"
    );
    let answer = tokio::time::timeout(Duration::from_secs(5), bootstrapping)
        .await
        .expect("the admitted bootstrap returned")
        .expect("the bootstrap task");
    match &answer {
        Ok(Err(failure)) => assert!(
            failure.message.contains(SHUTTING_DOWN)
                && failure
                    .message
                    .contains("the app quit while this bootstrap ran"),
            "{failure:?}"
        ),
        other => panic!("the admitted bootstrap was not cancelled by the shutdown: {other:?}"),
    }
    drop(roster);
}

/// **One run, however many callers** (plan 026 §3.3): two concurrent callers
/// and a later one all wait for — and only for — the first run; the broker is
/// drained once.
#[tokio::test]
async fn a_second_and_a_concurrent_shutdown_wait_for_the_one_run() {
    let gate = SpawnGate::new();
    let hosts = hosts_with(&gate, None);
    let lanes = lanes_over(&hosts);
    let drains = Arc::new(AtomicUsize::new(0));
    let counted = Arc::clone(&drains);
    let orderly = Shutdown::new(
        gate,
        hosts,
        lanes,
        Some(Box::new(move || {
            counted.fetch_add(1, Ordering::SeqCst);
        })),
        tokio::runtime::Handle::current(),
    );
    let first = {
        let orderly = Arc::clone(&orderly);
        async move {
            orderly.run().await;
            orderly.is_done()
        }
    };
    let second = {
        let orderly = Arc::clone(&orderly);
        async move {
            orderly.run().await;
            orderly.is_done()
        }
    };
    let (a, b) = tokio::join!(first, second);
    assert!(a && b, "a caller returned before the run finished");
    orderly.run().await;
    assert!(orderly.is_done());
    assert_eq!(
        drains.load(Ordering::SeqCst),
        1,
        "the shutdown ran more than once"
    );
}

/// **No child is spawned after the shutdown begins** (plan 026 §3.3; F1, F9):
/// a spawner dialling as fast as it can through the gated dial, all through
/// the shutdown — every child it was handed was spawned before the closing
/// flag was set; the one section that may have been mid-spawn at the flag was
/// refused at its registration; and every child it made — that one included —
/// was seen to exit before the shutdown returned.
#[tokio::test]
async fn no_child_is_spawned_after_the_shutdown_begins() {
    let dir = tempfile::tempdir().expect("tempdir");
    let pids = dir.path().join("pids");
    let gate = SpawnGate::new();
    let hosts = hosts_with(&gate, None);
    let lanes = lanes_over(&hosts);
    let log = Arc::new(SpawnLog::default());
    let counting: Arc<dyn CrazeDial> = Arc::new(CountingDial {
        inner: Arc::new(pid_dial(&pids)),
        log: Arc::clone(&log),
    });
    let dial = gated(counting, Some(&gate));
    let hammer = tokio::spawn({
        let dial = Arc::clone(&dial);
        async move {
            let mut held = Vec::new();
            loop {
                match dial.dial().await {
                    Ok(stream) => held.push(stream),
                    Err(e) => return (held, e),
                }
                tokio::time::sleep(Duration::from_millis(2)).await;
            }
        }
    });
    wait_for("a few spawns", || (log.count() >= 3).then_some(())).await;
    shutdown_children(&gate, &hosts, &lanes, None).await;
    // Read AT the shutdown's return.
    let unexited = log.unexited();
    let flagged = gate.closing_at().expect("the closing flag was set");
    let (held, refused) = tokio::time::timeout(Duration::from_secs(5), hammer)
        .await
        .expect("the spawner was refused")
        .expect("the spawner task");
    assert!(refused.to_string().starts_with(SHUTTING_DOWN), "{refused}");
    // The spawner is sequential, so its stamps are in dial order: the first
    // `held.len()` are the dials that answered with a child, and at most one
    // more — the section in flight when the flag was set — was refused at its
    // registration.
    let stamps = log.stamps();
    assert!(
        stamps.len() == held.len() || stamps.len() == held.len() + 1,
        "{} spawns for {} children handed out",
        stamps.len(),
        held.len()
    );
    assert!(
        stamps[..held.len()].iter().all(|at| *at < flagged),
        "a dial that answered with a child spawned it after the closing flag"
    );
    assert_eq!(unexited, 0, "a child outlived the shutdown");
    for pid in pids_in(&pids) {
        assert!(!pid_alive(pid), "child {pid} outlived the shutdown");
    }
    drop(held);
}

// ---------------------------------------------------------------------------
// C3 fix round 2: what a phase abandons at its bound (plan 026 AM4)
// ---------------------------------------------------------------------------

/// A fake `ssh` whose ordinary exec "establishes a master" (creates the
/// control path); whose `-O exit` logs `exit` to `log` and then WEDGES; and
/// whose `-O check` answers with a LIVE pid that is not this runner's master —
/// its parent, this test process — so the master stays UNCONFIRMED. (Its own
/// pid would not do: gone by the time `ps` looks, that reads as the master
/// having ended.)
fn unconfirmed_ssh(log: &Path) -> String {
    format!(
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
  exit) printf 'exit\n' >> '{log}'; exec sleep 30 ;;
  check) printf 'Master running (pid=%s)\r\n' "$PPID" >&2; exit 0 ;;
esac
: > "$ctl"
exit 0
"#,
        log = log.display()
    )
}

/// **Phase 4 takes one bound for unconfirmed masters, whatever their number**
/// (R2-1): three runners whose `-O exit` wedges and whose `-O check` names a
/// pid that is not theirs. Each runner's shutdown — side by side — sends its
/// `-O exit`, finds the master unconfirmed, and sends the retry its `Drop`
/// would have (two `exit`s each), inside one `MASTER_SHUTDOWN_BOUND`; the
/// registry then lets go of all three with no destructor work, so the phase is
/// one bound, not one plus three serial 2 s destructor waits.
#[tokio::test]
async fn phase_four_takes_one_bound_for_unconfirmed_masters_whatever_their_number() {
    let dir = tempfile::tempdir().expect("tempdir");
    let log = dir.path().join("control.log");
    let ssh = write_script(dir.path(), "ssh", &unconfirmed_ssh(&log));
    let gate = SpawnGate::new();
    let hosts = hosts_with(&gate, None);
    for name in ["unconfirmed-a", "unconfirmed-b", "unconfirmed-c"] {
        let exec = Arc::new(
            SshExec::new(
                &machine_entry(name),
                &SshBridgeOptions {
                    ssh_bin: Some(ssh.clone()),
                    ..SshBridgeOptions::default()
                },
            )
            .expect("a runner"),
        );
        let outcome = first_exec(&exec).await;
        assert!(
            matches!(outcome, Outcome::Exec { exit: Some(0), .. }),
            "{name}'s first exec established its master: {outcome:?}"
        );
        lock(&hosts.execs).insert(HostId::Machine(name.to_string()), exec);
    }

    let started = Instant::now();
    let (masters, unconfirmed) = hosts.shutdown_execs().await;
    let took = started.elapsed();
    assert_eq!((masters, unconfirmed), (3, 3));
    assert!(
        took < shed_app::roost::MASTER_SHUTDOWN_BOUND + Duration::from_secs(1),
        "phase 4 took {took:?} for three unconfirmed masters"
    );
    let exits = std::fs::read_to_string(&log).unwrap_or_default();
    assert_eq!(
        exits.lines().count(),
        6,
        "each master got its -O exit and its retry, and no drop sent a third: {exits:?}"
    );
}

/// **A section that outlasts the drain is reported, and its late child is
/// still killed** (R2-4, R2-7; plan 026 AM4): a spawn section admitted before
/// the close is paused past every phase's bound. The shutdown reports it open
/// at the close AND at the drain, and returns; the gate stays closed. When the
/// section is finally released — after the shutdown returned — its spawn's
/// registration finds the gate closed, SIGKILLs the child and sees it exit, and
/// the dial answers `shutting_down`.
#[tokio::test]
async fn a_section_that_outlasts_the_drain_is_reported_and_its_late_child_still_killed() {
    let dir = tempfile::tempdir().expect("tempdir");
    let pids = dir.path().join("pids");
    let gate = SpawnGate::new();
    let hosts = hosts_with(&gate, None);
    let lanes = lanes_over(&hosts);
    let log = Arc::new(SpawnLog::default());
    let counting: Arc<dyn CrazeDial> = Arc::new(CountingDial {
        inner: Arc::new(pid_dial(&pids)),
        log: Arc::clone(&log),
    });
    let dial = Arc::new(GatedDial::new(counting, gate.clone()));
    let (reached, proceed) = (
        Arc::new(tokio::sync::Notify::new()),
        Arc::new(tokio::sync::Notify::new()),
    );
    dial.pause_next((Arc::clone(&reached), Arc::clone(&proceed)));
    let dialing = tokio::spawn({
        let dial = Arc::clone(&dial);
        async move { dial.dial().await }
    });
    reached.notified().await;

    let report = shutdown_children(&gate, &hosts, &lanes, None).await;
    assert_eq!(
        (report.sections_open_at_close, report.sections_open_at_drain),
        (1, 1),
        "the section the close and the drain gave up on is reported: {report:?}"
    );
    assert_eq!(
        log.count(),
        0,
        "nothing spawned before the shutdown returned"
    );
    assert!(gate.is_closed(), "the gate stays closed");

    proceed.notify_one();
    let refused = tokio::time::timeout(Duration::from_secs(5), dialing)
        .await
        .expect("the late dial returned")
        .expect("the dial task")
        .err()
        .expect("a registration on a closed gate is refused");
    assert!(refused.to_string().starts_with(SHUTTING_DOWN), "{refused}");
    assert_eq!(log.count(), 1, "the late section spawned, after the return");
    assert_eq!(
        log.unexited(),
        0,
        "its child was killed at registration and its exit seen"
    );
    assert_eq!(
        gate.late(),
        shed_app::spawn_gate::LateRegistrations {
            killed: 1,
            unseen: Vec::new()
        }
    );
    for pid in pids_in(&pids) {
        assert!(!pid_alive(pid), "pid {pid} survived");
    }
}

/// A roost reach that answers every `ensure` at once with a real (fake)
/// roost-session's socket.
struct ReadyReach {
    socket: PathBuf,
}

#[async_trait::async_trait]
impl RoostReach for ReadyReach {
    fn label(&self) -> &str {
        "ready"
    }

    async fn ensure(&self) -> Result<RoostEndpoint, ReachError> {
        Ok(RoostEndpoint::Unix(self.socket.clone()))
    }

    async fn invalidate(&self) {}
}

/// **An op still inside one poll past phase 0's bound is counted and
/// abandoned, with the gate closed** (R2-7; plan 026 AM4). An admitted
/// one-shot gets its endpoint before the close, then its worker is stalled
/// inside that poll (a synchronous wait) past every bound — no cancel can reach
/// it there. The shutdown counts it as a one-shot left at the bound and
/// returns. When the stall ends, the same poll tries a spawn through the gate:
/// refused, nothing spawned. (What it does with the endpoint it already held is
/// the residual AM4 records — §9's roost accept race — and is not asserted.)
#[tokio::test(flavor = "multi_thread", worker_threads = 3)]
async fn an_op_stalled_past_phase_0_is_counted_and_abandoned_with_the_gate_closed() {
    let dir = tempfile::tempdir().expect("tempdir");
    let pids = dir.path().join("pids");
    let gate = SpawnGate::new();
    let hosts = hosts_with(&gate, None);
    let lanes = lanes_over(&hosts);
    let fake = FakeRoost::start().await;
    let reach = Arc::new(ReadyReach {
        socket: fake.socket_path().to_path_buf(),
    });
    let dial = gated(Arc::new(pid_dial(&pids)), Some(&gate));
    let admitted = hosts.admit().expect("admitted before the close");
    let (holding_tx, holding_rx) = tokio::sync::oneshot::channel::<()>();
    let op = tokio::spawn(async move {
        admitted
            .run(
                async move {
                    let _endpoint = reach.ensure().await.expect("an endpoint, before the close");
                    let _ = holding_tx.send(());
                    // Descheduled inside ONE poll, past every phase-0 bound.
                    std::thread::sleep(Duration::from_millis(2_500));
                    Some(dial.dial().await.map(|_| ()))
                },
                || None,
            )
            .await
    });
    holding_rx.await.expect("the op holds its endpoint");

    let report = shutdown_children(&gate, &hosts, &lanes, None).await;
    assert_eq!(
        (report.clients.cancelled, report.clients.one_shots_left),
        (1, 1),
        "the op stalled past the bound is counted (and logged): {report:?}"
    );
    assert!(gate.is_closed(), "the gate stays closed");

    let late = tokio::time::timeout(Duration::from_secs(10), op)
        .await
        .expect("the op's stall ended")
        .expect("the op task");
    match late {
        Some(Err(refused)) => assert!(refused.to_string().starts_with(SHUTTING_DOWN), "{refused}"),
        other => panic!("the stalled op's late spawn was not refused: {other:?}"),
    }
    assert!(pids_in(&pids).is_empty(), "the stalled op spawned a child");
}
