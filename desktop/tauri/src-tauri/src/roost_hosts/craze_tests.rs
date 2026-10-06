//! The craze half of the roost-host layer (plan 025 §3.6.1–§3.6.4), against
//! SCRIPTED hubs (`shed_craze::testing::ScriptedDial`, the test playing craze
//! line by line) and scripted find-only probes — deterministic, and no craze
//! binary anywhere. The real hub, end to end through the app, is
//! `desktop/tools/shedtest/test_tauri_craze.py`'s.

use std::collections::VecDeque;
use std::sync::atomic::{AtomicUsize, Ordering};
use std::time::Duration;

use serde_json::json;
use shed_core::lane::SourceOffline;
use shed_core::roost::testing::{ownership, FakeRoost};
use shed_craze::testing::{full_hub_capabilities, roster_row, HookDial, HubEnd, ScriptedDial};
use shed_craze::{CrazeDial, Probe};
use tokio::sync::mpsc::UnboundedReceiver;

use super::*;
use crate::craze::{BoxFut, CrazeProbe, CrazeReach, CrazeTimings};
use crate::lane::Lanes;
use crate::machines::ReachOptions;

const EPOCH: &str = "0a1b2c3d4e5f";
const HOST_A: &str = "aaaaaaaaaaaa";
const HOST_B: &str = "bbbbbbbbbbbb";
/// The vendored `tab.list`'s shell tab — claimed below as a craze TUI.
const TAB: i64 = 5;

/// Fast clocks, so a dormant source re-probes inside a test's patience.
fn timings() -> CrazeTimings {
    CrazeTimings {
        dormant: Duration::from_millis(40),
        backoff_base: Duration::from_millis(10),
        backoff_max: Duration::from_millis(40),
    }
}

async fn wait_for<T>(what: &str, mut f: impl FnMut() -> Option<T>) -> T {
    for _ in 0..2_000 {
        if let Some(v) = f() {
            return v;
        }
        tokio::time::sleep(Duration::from_millis(5)).await;
    }
    panic!("timed out waiting for {what}");
}

/// A find-only probe the test answers, one answer per probe, in order; a
/// probe with no answer queued WAITS (an await boundary a test can hold).
#[derive(Default)]
struct ScriptedProbe {
    answers: std::sync::Mutex<VecDeque<Probe>>,
    queued: tokio::sync::Notify,
    calls: AtomicUsize,
}

impl ScriptedProbe {
    fn answer(&self, probe: Probe) {
        lock(&self.answers).push_back(probe);
        self.queued.notify_waiters();
        self.queued.notify_one();
    }
}

struct ProbeRef(Arc<ScriptedProbe>);

impl CrazeProbe for ProbeRef {
    fn probe(&self) -> BoxFut<Probe> {
        let p = Arc::clone(&self.0);
        Box::pin(async move {
            p.calls.fetch_add(1, Ordering::SeqCst);
            loop {
                let notified = p.queued.notified();
                if let Some(answer) = lock(&p.answers).pop_front() {
                    return answer;
                }
                notified.await;
            }
        })
    }
}

/// One host's scripted craze: its dial (held-able, counting) and, for an
/// attach-only host, its probe.
struct Scripted {
    hook: Arc<HookDial>,
    conns: tokio::sync::Mutex<UnboundedReceiver<HubEnd>>,
    probe: Option<Arc<ScriptedProbe>>,
}

impl Scripted {
    fn eager() -> Arc<Scripted> {
        Self::new(None)
    }

    fn attach_only() -> Arc<Scripted> {
        Self::new(Some(Arc::new(ScriptedProbe::default())))
    }

    fn new(probe: Option<Arc<ScriptedProbe>>) -> Arc<Scripted> {
        let (dial, conns) = ScriptedDial::new();
        Arc::new(Scripted {
            hook: HookDial::new(dial as Arc<dyn CrazeDial>),
            conns: tokio::sync::Mutex::new(conns),
            probe,
        })
    }

    fn reach(&self) -> CrazeReach {
        CrazeReach {
            dial: Arc::clone(&self.hook) as Arc<dyn CrazeDial>,
            probe: self
                .probe
                .as_ref()
                .map(|p| Arc::new(ProbeRef(Arc::clone(p))) as Arc<dyn CrazeProbe>),
        }
    }

    /// The next connection the source dialled.
    async fn next(&self) -> HubEnd {
        tokio::time::timeout(Duration::from_secs(10), self.conns.lock().await.recv())
            .await
            .expect("a dial in time")
            .expect("the dial is alive")
    }

    /// A roster connection: `hello`, then `sessions.subscribe` answered with
    /// `rows`.
    async fn roster(&self, rows: serde_json::Value) -> HubEnd {
        let mut hub = self.next().await;
        hub.hello(EPOCH, full_hub_capabilities()).await;
        hub.subscribed("sub-1", EPOCH, rows).await;
        hub
    }
}

/// A roster row for `host_id` running provider session `provider_session`.
fn row(host_id: &str, provider_session: &str) -> serde_json::Value {
    roster_row(
        host_id,
        &format!("craze-{host_id}"),
        "/work",
        json!({"title": format!("session {host_id}"), "activity": "idle",
               "providerSessionId": provider_session, "since": "2026-01-01T00:00:00Z"}),
    )
}

/// The layer with `reaches` as each host's scripted craze.
fn start_scripted(
    config: &ShedConfig,
    options: &ReachOptions,
    reaches: Vec<(HostId, Arc<Scripted>)>,
) -> RoostHosts {
    let map: BTreeMap<HostId, Arc<Scripted>> = reaches.into_iter().collect();
    RoostHosts::start_with(
        &tokio::runtime::Handle::current(),
        config,
        options.clone(),
        false,
        Arc::new(|| {}),
        CrazeReaches::Fixed(Arc::new(move |id: &HostId| map.get(id).map(|s| s.reach()))),
        timings(),
    )
}

fn config_with(names: &[&str]) -> ShedConfig {
    ShedConfig {
        machines: names
            .iter()
            .map(|n| MachineEntry {
                name: (*n).to_string(),
                host: (*n).to_string(),
                ssh_port: 22,
                ..Default::default()
            })
            .collect(),
        ..Default::default()
    }
}

fn local() -> HostId {
    HostId::Machine(LOCALHOST.to_string())
}

fn test_options(pairs: &[(&str, &std::path::Path)]) -> ReachOptions {
    ReachOptions {
        roost_sockets: pairs
            .iter()
            .map(|(n, p)| ((*n).to_string(), p.to_path_buf()))
            .collect(),
        test_mode: true,
        ..ReachOptions::default()
    }
}

fn rows_of(hosts: &RoostHosts) -> Vec<Value> {
    hosts.snapshot(HostFilter::ALL).0
}

fn craze_rows(hosts: &RoostHosts) -> Vec<Value> {
    rows_of(hosts)
        .into_iter()
        .filter(|r| r["source"] == "craze")
        .collect()
}

fn status_of(hosts: &RoostHosts, name: &str) -> Option<Value> {
    hosts.status().into_iter().find(|s| s["name"] == name)
}

/// **A craze-only `localhost` is listed by its first craze `Ready`** (plan
/// 025 §3.6.2, §2.1 item 7): no roost-session here at all, and still the
/// machine and its craze sessions show — and it is somewhere to create one.
#[tokio::test]
async fn a_craze_only_localhost_is_listed_by_its_first_ready() {
    let craze = Scripted::eager();
    let hosts = start_scripted(
        &ShedConfig::default(),
        &test_options(&[]),
        vec![(local(), Arc::clone(&craze))],
    );
    assert!(
        status_of(&hosts, LOCALHOST).is_none(),
        "unlisted before anything answers"
    );
    let _hub = craze.roster(json!([row(HOST_A, "ses-a")])).await;
    let status = wait_for("localhost listed by craze", || status_of(&hosts, LOCALHOST)).await;
    assert_eq!(status["craze"]["state"], "live", "{status}");
    assert_eq!(status["craze"]["create"], true);
    assert_eq!(
        status["reachable"], false,
        "roost's half is still unreachable"
    );
    let rows = craze_rows(&hosts);
    assert_eq!(rows.len(), 1, "{rows:?}");
    let r = &rows[0];
    assert_eq!(r["slug"], HOST_A, "a craze row's slug is its hostId (P11)");
    assert_eq!(r["kind"], "craze");
    assert_eq!(r["origin"], "machine:localhost");
    assert_eq!(r["machine"], "localhost");
    assert_eq!(r["stale"], false);
    assert_eq!(
        r["agent_lane"],
        json!({"kind": "craze", "session_id": HOST_A})
    );
    assert!(r.get("tab_id").is_none(), "no roost tab to attach");
}

/// **A craze-only SHED gets its craze source when it is first observed
/// running** — not when its roost watcher is promoted, which waits on a
/// `session.identify` this shed will never answer (plan 025 §3.6.1).
#[tokio::test]
async fn a_craze_only_shed_is_sourced_when_first_observed_running() {
    let shed = HostId::Shed {
        server: "srv".to_string(),
        name: "s1".to_string(),
    };
    let craze = Scripted::attach_only();
    let mut config = ShedConfig::default();
    config.servers.push(shed_core::config::ShedServerEntry {
        name: "srv".to_string(),
        host: "127.0.0.1".to_string(),
        ssh_port: 2222,
        ..Default::default()
    });
    let hosts = start_scripted(
        &config,
        &test_options(&[]),
        vec![(shed.clone(), Arc::clone(&craze))],
    );
    hosts
        .observe_sheds(
            &[("srv".to_string(), "s1".to_string())],
            &["srv".to_string()],
        )
        .await;
    let probe = craze.probe.as_ref().unwrap();
    probe.answer(Probe::Hub);
    let _hub = craze.roster(json!([row(HOST_A, "ses-a")])).await;
    let rows = wait_for("the shed's craze row", || {
        let rows = craze_rows(&hosts);
        (!rows.is_empty()).then_some(rows)
    })
    .await;
    assert_eq!(rows[0]["origin"], "roost:srv/s1");
    assert_eq!(rows[0]["machine"], "roost:srv/s1");
    assert_eq!(rows[0]["host"], "srv");
    assert_eq!(rows[0]["shed"], "s1");
    let status = status_of(&hosts, "roost:srv/s1").expect("the shed is listed by craze");
    assert_eq!(status["craze"]["state"], "live");
}

/// **The row merge, on the desktop** (plan 025 §3.6.3): a roost tab owned
/// `(craze, X)` folds into the hub row whose providerSessionId is X — one row,
/// the hub's, carrying the tab's id; the hub feed dropping brings roost's row
/// back and leaves the hub row as the last-known, STALE one.
#[tokio::test]
async fn a_craze_tab_folds_into_its_hub_row_and_returns_when_the_feed_drops() {
    let fake = FakeRoost::start().await;
    fake.set_tab_axes(
        TAB,
        "working",
        Some(ownership("craze", "ses-a", "", 1_700_000_100)),
        false,
    );
    let craze = Scripted::eager();
    let hosts = start_scripted(
        &ShedConfig::default(),
        &test_options(&[(LOCALHOST, fake.socket_path())]),
        vec![(local(), Arc::clone(&craze))],
    );
    // Before the feed is live, roost's craze row stands alone.
    let roost_row = wait_for("roost's craze row", || {
        rows_of(&hosts).into_iter().find(|r| r["source"] == "roost")
    })
    .await;
    assert_eq!(roost_row["kind"], "craze");
    assert_eq!(roost_row["slug"], TAB.to_string());

    let hub = craze.roster(json!([row(HOST_A, "ses-a")])).await;
    let merged = wait_for("the tab folded into the hub row", || {
        let rows = rows_of(&hosts);
        (rows.len() == 1 && rows[0]["source"] == "craze").then_some(rows)
    })
    .await;
    assert_eq!(merged[0]["slug"], HOST_A);
    assert_eq!(
        merged[0]["tab_id"],
        TAB.to_string(),
        "the hub row carries the tab"
    );

    // The feed drops (the hub's connection closes) and the machine's craze is
    // no longer live: roost's row returns, and the hub row is the last known.
    craze.hook.hold_dials();
    hub.close().await;
    let rows = wait_for("roost's row back beside the stale hub row", || {
        let rows = rows_of(&hosts);
        (rows.len() == 2).then_some(rows)
    })
    .await;
    let roost_back = rows.iter().find(|r| r["source"] == "roost").unwrap();
    assert_eq!(roost_back["slug"], TAB.to_string());
    let retained = rows.iter().find(|r| r["source"] == "craze").unwrap();
    assert_eq!(retained["slug"], HOST_A);
    assert_eq!(
        retained["stale"], true,
        "a retained row renders stale: {retained}"
    );
    assert_eq!(retained["approximate"], true);
    assert!(
        retained.get("tab_id").is_none(),
        "nothing is absorbed while the feed is down"
    );
    let status = status_of(&hosts, LOCALHOST).unwrap();
    assert_eq!(status["craze"]["state"], "offline", "{status}");
    assert_eq!(status["craze"]["cause"], "unreachable");
}

/// **Only craze ownership folds** (plan 025 §3.6.3): an opencode tab whose
/// own session id happens to equal a hub row's providerSessionId stays
/// roost's row — the merge reads the tab's OWNER, not just its id.
#[tokio::test]
async fn a_tab_of_another_source_naming_the_session_is_never_absorbed() {
    let fake = FakeRoost::start().await;
    fake.set_tab_axes(
        TAB,
        "working",
        Some(ownership("opencode", "ses-a", "", 1_700_000_100)),
        false,
    );
    let craze = Scripted::eager();
    let hosts = start_scripted(
        &ShedConfig::default(),
        &test_options(&[(LOCALHOST, fake.socket_path())]),
        vec![(local(), Arc::clone(&craze))],
    );
    let _hub = craze.roster(json!([row(HOST_A, "ses-a")])).await;
    let rows = wait_for("the hub row beside roost's opencode row", || {
        let rows = rows_of(&hosts);
        (rows.len() == 2).then_some(rows)
    })
    .await;
    let roost_row = rows.iter().find(|r| r["source"] == "roost").unwrap();
    assert_eq!(roost_row["kind"], "opencode");
    let hub_row = rows.iter().find(|r| r["source"] == "craze").unwrap();
    assert!(hub_row.get("tab_id").is_none(), "{hub_row}");
}

/// **A fold never crosses machines** (plan 025 §3.6.3): `mini3`'s craze tab
/// names the session localhost's hub lists, and is still mini3's roost row —
/// mini3 has no live craze feed of its own.
#[tokio::test]
async fn a_fold_never_crosses_machines() {
    let here = FakeRoost::start().await;
    let there = FakeRoost::start().await;
    there.set_tab_axes(
        TAB,
        "working",
        Some(ownership("craze", "ses-a", "", 1_700_000_100)),
        false,
    );
    let craze = Scripted::eager();
    let hosts = start_scripted(
        &config_with(&["mini3"]),
        &test_options(&[
            (LOCALHOST, here.socket_path()),
            ("mini3", there.socket_path()),
        ]),
        vec![(local(), Arc::clone(&craze))],
    );
    let _hub = craze.roster(json!([row(HOST_A, "ses-a")])).await;
    let rows = wait_for("localhost's hub row and mini3's roost row", || {
        let rows = rows_of(&hosts);
        (rows.len() == 2).then_some(rows)
    })
    .await;
    let mini3 = rows
        .iter()
        .find(|r| r["origin"] == "machine:mini3")
        .expect("mini3's craze tab is still its roost row");
    assert_eq!(mini3["source"], "roost");
    let hub_row = rows.iter().find(|r| r["source"] == "craze").unwrap();
    assert!(
        hub_row.get("tab_id").is_none(),
        "another machine's tab never attaches: {hub_row}"
    );
}

/// **The attach-only cycle** (plan 025 §3.6.1): a remote host with craze and
/// no hub stays DORMANT — probing, never dialling `bridge --hub` — and a
/// probe that finds a hub opens the one roster connection.
#[tokio::test]
async fn a_remote_source_is_dormant_until_a_probe_finds_a_hub() {
    let craze = Scripted::attach_only();
    let hosts = start_scripted(
        &config_with(&["mini3"]),
        &test_options(&[]),
        vec![(HostId::Machine("mini3".to_string()), Arc::clone(&craze))],
    );
    let probe = craze.probe.as_ref().unwrap();
    for _ in 0..3 {
        probe.answer(Probe::NoHub);
    }
    // Three dormant probes answered (a fourth pending) — or a dial, which a
    // dormant source must never make.
    wait_for("three probes, or a dial", || {
        (probe.calls.load(Ordering::SeqCst) >= 4 || craze.hook.dials() > 0).then_some(())
    })
    .await;
    assert_eq!(
        craze.hook.dials(),
        0,
        "a dormant remote source never dials bridge --hub"
    );
    assert_eq!(
        status_of(&hosts, "mini3").unwrap()["craze"]["state"],
        "dormant"
    );

    probe.answer(Probe::Hub);
    let _hub = craze.roster(json!([row(HOST_A, "ses-a")])).await;
    wait_for("live", || {
        (status_of(&hosts, "mini3").unwrap()["craze"]["state"] == "live").then_some(())
    })
    .await;
    assert_eq!(craze.hook.dials(), 1, "one roster connection");

    // not installed / too old: the probe's own words, the slow retry.
    let gone = Scripted::attach_only();
    let hosts2 = start_scripted(
        &config_with(&["mini4"]),
        &test_options(&[]),
        vec![(HostId::Machine("mini4".to_string()), Arc::clone(&gone))],
    );
    gone.probe.as_ref().unwrap().answer(Probe::Offline {
        cause: SourceOffline::TooOld,
        reason: "unknown flag: --hub".to_string(),
    });
    let status = wait_for("too old", || {
        let s = status_of(&hosts2, "mini4").unwrap();
        (s["craze"]["state"] == "offline").then_some(s)
    })
    .await;
    assert_eq!(status["craze"]["cause"], "too_old");
    assert_eq!(gone.hook.dials(), 0);
}

/// **A host removed at every await of its craze start leaves no task and no
/// bridge behind** (plan 025 §3.6.1, "Ownership and teardown"): removed while
/// its probe is pending, while its dial is pending, while its `hello` is
/// unanswered, and once it is live — each time the connection (if one was
/// made) is closed, nothing dials afterwards, and its state is gone.
///
/// **Asserted at `remove`'s return, not some time after** (C9 review):
/// `remove` is a bounded best-effort teardown — it joins the source's task
/// and waits, up to `CRAZE_STOP_WAIT`, until every holder of its dial (its
/// roster's pump, and so the host's `SshExec` in production) is gone. Nothing
/// here holds a lane verb, so nothing outlives the wait, and nothing below
/// waits for cleanup.
#[tokio::test]
async fn a_host_removed_at_each_await_of_a_start_leaks_nothing() {
    let shed = |n: &str| HostId::Shed {
        server: "srv".to_string(),
        name: n.to_string(),
    };
    let mut config = ShedConfig::default();
    config.servers.push(shed_core::config::ShedServerEntry {
        name: "srv".to_string(),
        host: "127.0.0.1".to_string(),
        ssh_port: 2222,
        ..Default::default()
    });
    let names = ["probing", "dialling", "helloing", "live"];
    let scripted: Vec<(HostId, Arc<Scripted>)> = names
        .iter()
        .map(|n| (shed(n), Scripted::attach_only()))
        .collect();
    let by_name: BTreeMap<&str, Arc<Scripted>> = names
        .iter()
        .zip(&scripted)
        .map(|(n, (_, s))| (*n, Arc::clone(s)))
        .collect();
    let hosts = start_scripted(&config, &test_options(&[]), scripted.clone());
    let running: Vec<(String, String)> = names
        .iter()
        .map(|n| ("srv".to_string(), (*n).to_string()))
        .collect();
    hosts.observe_sheds(&running, &["srv".to_string()]).await;

    // 1. The probe is pending.
    let probing = &by_name["probing"];
    wait_for("the probe asked", || {
        (probing.probe.as_ref().unwrap().calls.load(Ordering::SeqCst) == 1).then_some(())
    })
    .await;
    // 2. The dial is pending.
    let dialling = &by_name["dialling"];
    dialling.hook.hold_dials();
    dialling.probe.as_ref().unwrap().answer(Probe::Hub);
    wait_for("the dial started", || {
        (dialling.hook.dials() == 1).then_some(())
    })
    .await;
    // 3. The hello is unanswered.
    let helloing = &by_name["helloing"];
    helloing.probe.as_ref().unwrap().answer(Probe::Hub);
    let mut hello_end = helloing.next().await;
    let _hello = hello_end.expect("hello").await;
    // 4. Live.
    let live = &by_name["live"];
    live.probe.as_ref().unwrap().answer(Probe::Hub);
    let mut live_end = live.roster(json!([row(HOST_A, "ses-a")])).await;
    wait_for("the live shed's row", || {
        rows_of(&hosts)
            .iter()
            .any(|r| r["origin"] == "roost:srv/live")
            .then_some(())
    })
    .await;

    // Every one of them stops — and by the time the refresh that removed them
    // RETURNS, each source has stopped: its task joined, and nothing of it is
    // left holding its dial (the test's own reference is the only one).
    hosts.observe_sheds(&[], &["srv".to_string()]).await;
    for n in names {
        assert!(
            !lock(&hosts.reg).crazes.contains_key(&shed(n)),
            "{n}: the craze source left the registry"
        );
        assert!(
            !lock(&hosts.state).contains_key(&shed(n)),
            "{n}: no state left"
        );
        assert_eq!(
            Arc::strong_count(&by_name[n].hook),
            1,
            "{n}: when remove returns, nothing of the removed source holds its dial"
        );
    }

    // Released afterwards, nothing they unblock reaches anything — there is
    // nothing left running to reach it.
    probing.probe.as_ref().unwrap().answer(Probe::Hub);
    dialling.hook.release_dials();
    tokio::task::yield_now().await;
    assert_eq!(
        probing.hook.dials(),
        0,
        "a start removed at its probe never dials"
    );
    assert!(
        dialling.conns.lock().await.try_recv().is_err(),
        "a start removed at its dial never connects"
    );
    assert_eq!(dialling.hook.dials(), 1, "and never dialled again");
    assert!(
        hello_end.recv().await.is_none(),
        "a start removed at its hello closed its bridge"
    );
    assert!(
        live_end.recv().await.is_none(),
        "a live source removed closed its roster's bridge"
    );
    assert_eq!(live.hook.dials(), 1, "and did not redial");
    assert!(rows_of(&hosts).is_empty(), "{:?}", rows_of(&hosts));
}

/// The lanes over a craze roster, wired the way `lib.rs` wires them.
fn lanes_over(hosts: &Arc<RoostHosts>) -> Arc<Lanes> {
    let lanes = Arc::new(Lanes::for_test(
        tokio::runtime::Handle::current(),
        Arc::clone(hosts) as Arc<dyn crate::lane::LaneMachines>,
    ));
    let weak = Arc::downgrade(&lanes);
    hosts.set_lane_observer(Arc::new(move |m: &str, open: &_| {
        if let Some(l) = weak.upgrade() {
            l.reconcile(m, open);
        }
    }));
    let weak = Arc::downgrade(&lanes);
    hosts.set_craze_observer(Arc::new(move |m: &str, gen: u64, gone: &[String]| {
        if let Some(l) = weak.upgrade() {
            l.evict_craze(m, gen, gone);
        }
    }));
    lanes
}

/// **A roost snapshot does not evict a craze lane, and a `Ready` swap that
/// drops its row does** (plan 025 §3.6.4): `reconcile` judges only the
/// roost-stamped lanes, and the craze source's own reseed is what retires a
/// craze one — an epoch reseed carries no `Removed`.
#[tokio::test]
async fn a_craze_lane_outlives_roost_snapshots_and_leaves_with_its_row() {
    let fake = FakeRoost::start().await;
    let craze = Scripted::eager();
    let hosts = Arc::new(start_scripted(
        &ShedConfig::default(),
        &test_options(&[(LOCALHOST, fake.socket_path())]),
        vec![(local(), Arc::clone(&craze))],
    ));
    let lanes = lanes_over(&hosts);
    let mut roster = craze
        .roster(json!([row(HOST_A, "ses-a"), row(HOST_B, "ses-b")]))
        .await;
    wait_for("both rows", || {
        (craze_rows(&hosts).len() == 2).then_some(())
    })
    .await;

    lanes
        .open(LOCALHOST, "craze", HOST_A)
        .await
        .expect("a craze lane opens on a listed row");
    assert!(lanes.is_open(LOCALHOST, "craze", HOST_A));

    // A roost snapshot on the same machine: the craze lane is not roost's.
    fake.set_tab_axes(
        TAB,
        "working",
        Some(ownership("claude", "c-1", "", 1_700_000_100)),
        false,
    );
    wait_for("the roost row", || {
        rows_of(&hosts)
            .iter()
            .any(|r| r["source"] == "roost")
            .then_some(())
    })
    .await;
    tokio::time::sleep(Duration::from_millis(50)).await;
    assert!(
        lanes.is_open(LOCALHOST, "craze", HOST_A),
        "a roost snapshot does not evict a craze lane"
    );

    // The hub reseeds (a roster reset) WITHOUT host A: its lane goes.
    roster.reset("sub-1", "omitted").await;
    let resub = roster.expect("sessions.subscribe").await;
    roster
        .reply(
            &resub,
            json!({"subscription": "sub-2", "epoch": EPOCH, "cursor": 3,
                   "sessions": [row(HOST_B, "ses-b")]}),
        )
        .await;
    wait_for("the swap dropped A's lane", || {
        (!lanes.is_open(LOCALHOST, "craze", HOST_A)).then_some(())
    })
    .await;
    assert_eq!(craze_rows(&hosts).len(), 1);
}

/// A shed id on server `srv`, and the config that serves it.
fn srv_shed(name: &str) -> (HostId, ShedConfig) {
    let mut config = ShedConfig::default();
    config.servers.push(shed_core::config::ShedServerEntry {
        name: "srv".to_string(),
        host: "127.0.0.1".to_string(),
        ssh_port: 2222,
        ..Default::default()
    });
    (
        HostId::Shed {
            server: "srv".to_string(),
            name: name.to_string(),
        },
        config,
    )
}

/// The layer whose craze starts for `id` take their scripted reaches from
/// `queue`, in order — the first registration's, then a re-registration's.
/// Every other host (the implicit `localhost`) has no craze.
fn start_queued(config: &ShedConfig, id: &HostId, queue: Vec<Arc<Scripted>>) -> Arc<RoostHosts> {
    let queue = Arc::new(std::sync::Mutex::new(VecDeque::from(queue)));
    let only = id.clone();
    Arc::new(RoostHosts::start_with(
        &tokio::runtime::Handle::current(),
        config,
        test_options(&[]),
        false,
        Arc::new(|| {}),
        CrazeReaches::Fixed(Arc::new(move |id: &HostId| {
            if *id != only {
                return None;
            }
            lock(&queue).pop_front().map(|s| s.reach())
        })),
        timings(),
    ))
}

/// `srv` lists `sheds` running — the authoritative refresh.
async fn refresh(hosts: &RoostHosts, sheds: &[&str]) {
    let running: Vec<(String, String)> = sheds
        .iter()
        .map(|n| ("srv".to_string(), (*n).to_string()))
        .collect();
    hosts.observe_sheds(&running, &["srv".to_string()]).await;
}

/// **A remove racing a re-registration leaves the new registration WHOLE**
/// (C9 review): the shed is removed, and a refresh that lists it running
/// again lands in the gap between `remove`'s one critical section and its
/// teardown. The new registration's state, ids, reach and craze source are
/// all there, consistent (the craze source's generation is the state's), and
/// alive; the OLD source is gone — every holder of its dial dropped, its
/// roster's bridge closed — when `remove` returns.
///
/// The removal is called directly, not through a refresh: refreshes are one
/// pass at a time (`observe_pass`), so a second one cannot land inside the
/// first's removal — `remove` must stay whole against a registration from
/// anywhere else regardless.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn a_remove_racing_a_reregistration_leaves_the_new_registration_whole() {
    let (id, config) = srv_shed("s1");
    let old = Scripted::attach_only();
    let new = Scripted::attach_only();
    let hosts = start_queued(&config, &id, vec![Arc::clone(&old), Arc::clone(&new)]);
    refresh(&hosts, &["s1"]).await;
    old.probe.as_ref().unwrap().answer(Probe::Hub);
    let mut old_end = old.roster(json!([row(HOST_A, "ses-a")])).await;
    wait_for("the old source's row", || {
        craze_rows(&hosts)
            .iter()
            .any(|r| r["slug"] == HOST_A)
            .then_some(())
    })
    .await;

    let reached = Arc::new(tokio::sync::Notify::new());
    let proceed = Arc::new(tokio::sync::Notify::new());
    *lock(&hosts.remove_gap) = Some((Arc::clone(&reached), Arc::clone(&proceed)));
    let removing = {
        let hosts = Arc::clone(&hosts);
        let id = id.clone();
        tokio::spawn(async move { hosts.remove(std::slice::from_ref(&id)).await })
    };
    reached.notified().await;
    // The refresh that says it is running again, landing IN the gap.
    refresh(&hosts, &["s1"]).await;
    proceed.notify_one();
    removing.await.expect("the removal finishes");

    let new_gen = {
        let state = lock(&hosts.state);
        let m = state
            .get(&id)
            .expect("the new registration's state survived the stale remove");
        m.craze.gen
    };
    {
        let reg = lock(&hosts.reg);
        assert!(reg.ids.contains(&id), "the new registration's id");
        assert!(
            reg.reaches.contains_key(&id),
            "the new registration's reach"
        );
        let craze = reg
            .crazes
            .get(&id)
            .expect("the new registration's craze source");
        assert_eq!(craze.gen, new_gen, "the registry and the state agree");
    }
    // …and it is alive: its probe finds a hub, its roster lists.
    new.probe.as_ref().unwrap().answer(Probe::Hub);
    let _new_end = new.roster(json!([row(HOST_B, "ses-b")])).await;
    wait_for("the new source's row", || {
        craze_rows(&hosts)
            .iter()
            .any(|r| r["slug"] == HOST_B && r["origin"] == "roost:srv/s1")
            .then_some(())
    })
    .await;
    // The old source is gone, entirely, by remove's return.
    assert_eq!(
        Arc::strong_count(&old.hook),
        1,
        "nothing of the old source holds its dial (its ssh, in production)"
    );
    assert!(
        old_end.recv().await.is_none(),
        "the old roster's bridge is closed"
    );
}

/// **A stale source's eviction never ends a newer generation's lane** (C9
/// review): the shed's craze lane on hostId A is open; the shed is removed and
/// registered again, and a lane on the SAME hostId opens through the new
/// source. The old source's news — published, as its sink does, after its
/// fence was passed — then arrives, and the new lane stays; the new
/// generation's own news does end it.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn a_stale_sources_eviction_never_ends_a_new_generations_lane() {
    let (id, config) = srv_shed("s1");
    let address = id.address();
    let old = Scripted::attach_only();
    let new = Scripted::attach_only();
    let hosts = start_queued(&config, &id, vec![Arc::clone(&old), Arc::clone(&new)]);
    let lanes = lanes_over(&hosts);
    refresh(&hosts, &["s1"]).await;
    old.probe.as_ref().unwrap().answer(Probe::Hub);
    let _old_end = old.roster(json!([row(HOST_A, "ses-a")])).await;
    wait_for("the old row", || {
        (craze_rows(&hosts).len() == 1).then_some(())
    })
    .await;
    lanes
        .open(&address, "craze", HOST_A)
        .await
        .expect("the old generation's lane");
    let old_gen = lock(&hosts.state).get(&id).expect("state").craze.gen;

    refresh(&hosts, &[]).await;
    assert!(
        !lanes.is_open(&address, "craze", HOST_A),
        "removing the host ended its lane"
    );
    refresh(&hosts, &["s1"]).await;
    new.probe.as_ref().unwrap().answer(Probe::Hub);
    let _new_end = new.roster(json!([row(HOST_A, "ses-a")])).await;
    wait_for("the new row", || {
        (craze_rows(&hosts).len() == 1).then_some(())
    })
    .await;
    lanes
        .open(&address, "craze", HOST_A)
        .await
        .expect("the new generation's lane, on the same hostId");
    let new_gen = lock(&hosts.state).get(&id).expect("state").craze.gen;
    assert_ne!(old_gen, new_gen);

    // The OLD source's news, late.
    publish_craze_gone(&hosts.on_craze, &id, old_gen, &[HOST_A.to_string()]);
    assert!(
        lanes.is_open(&address, "craze", HOST_A),
        "a stale source's eviction never ends a newer generation's lane"
    );
    // The current generation's does.
    publish_craze_gone(&hosts.on_craze, &id, new_gen, &[HOST_A.to_string()]);
    assert!(!lanes.is_open(&address, "craze", HOST_A));
}

/// **A craze tab the merge HIDES is still killable** (C9 review): with the
/// feed live, a craze-owned tab the roster names no row for is not a row
/// (D4), but it is an agent-owned tab of this host's, and `machine.kill` on
/// its id ends it.
#[tokio::test]
async fn a_hidden_craze_tab_is_still_killable() {
    let fake = FakeRoost::start().await;
    fake.set_tab_axes(
        TAB,
        "working",
        Some(ownership("craze", "ses-unmatched", "", 1_700_000_100)),
        false,
    );
    let craze = Scripted::eager();
    let hosts = start_scripted(
        &ShedConfig::default(),
        &test_options(&[(LOCALHOST, fake.socket_path())]),
        vec![(local(), Arc::clone(&craze))],
    );
    let _hub = craze.roster(json!([row(HOST_A, "ses-a")])).await;
    // The roost snapshot (the tab) AND the live roster (no row for it) both
    // in: the tab is HIDDEN — not a row, and not absorbed into one.
    wait_for("the tab hidden behind a live feed", || {
        let hidden = lock(&hosts.state)
            .get(&local())
            .is_some_and(|m| m.fold_plan().hidden.contains(&TAB));
        let rows = rows_of(&hosts);
        (hidden && rows.len() == 1 && rows[0]["source"] == "craze").then_some(())
    })
    .await;
    hosts
        .kill(LOCALHOST, &TAB.to_string())
        .await
        .expect("a hidden craze tab can still be ended");
    wait_for("the tab closed on roost", || {
        (!fake.tab_ids().contains(&TAB)).then_some(())
    })
    .await;
}

/// A [`TestGap`] to arm, and its two ends.
fn test_gap() -> (TestGap, Arc<tokio::sync::Notify>, Arc<tokio::sync::Notify>) {
    let reached = Arc::new(tokio::sync::Notify::new());
    let proceed = Arc::new(tokio::sync::Notify::new());
    (
        (Arc::clone(&reached), Arc::clone(&proceed)),
        reached,
        proceed,
    )
}

/// **A lane opening across its host's removal is never committed** (C9
/// confirmation, N1): `lane.open` has resolved the host's craze source, and
/// the host is removed before the open declares itself — so the removal's
/// eviction finds no open to cancel. The open re-reads the host's generation
/// once declared, finds no source, and fails; no lane of the removed source's
/// generation is committed.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn a_lane_opening_across_its_hosts_removal_is_never_committed() {
    let (id, config) = srv_shed("s1");
    let address = id.address();
    let old = Scripted::attach_only();
    let hosts = start_queued(&config, &id, vec![Arc::clone(&old)]);
    let lanes = lanes_over(&hosts);
    refresh(&hosts, &["s1"]).await;
    old.probe.as_ref().unwrap().answer(Probe::Hub);
    let _old_end = old.roster(json!([row(HOST_A, "ses-a")])).await;
    wait_for("the row", || (craze_rows(&hosts).len() == 1).then_some(())).await;

    let (gap, reached, proceed) = test_gap();
    *lock(&lanes.open_gap) = Some(gap);
    let opening = {
        let lanes = Arc::clone(&lanes);
        let address = address.clone();
        tokio::spawn(async move { lanes.open(&address, "craze", HOST_A).await })
    };
    reached.notified().await;
    // Removed while the open holds the resolved source and has declared
    // nothing. (The open's clone holds the source's dial, so the removal
    // waits its bounded wait out, and returns anyway.)
    refresh(&hosts, &[]).await;
    assert!(
        !lock(&hosts.reg).crazes.contains_key(&id),
        "the source is gone"
    );
    proceed.notify_one();
    let opened = opening.await.expect("the open finishes");
    let Err(failure) = opened else {
        panic!("an open across its host's removal must fail, and it opened: {opened:?}");
    };
    assert!(
        format!("{failure:?}").contains("stopped while the lane"),
        "{failure:?}"
    );
    assert!(
        !lanes.is_open(&address, "craze", HOST_A),
        "no lane of the removed source's generation was committed"
    );
}

/// **Refreshes are one pass at a time, and the registry ends on the LATEST
/// observation** (C9 confirmation, N2). A refresh that lists the shed STOPPED
/// has read the registry and is held before it acts on it; a later refresh
/// lists it RUNNING. Unserialized, the later one found the shed still
/// registered and did nothing, and the earlier one then removed it — the
/// older observation applied last. Serialized, the later one waits for the
/// earlier, and registers the shed it removed. The other way round (running,
/// then stopped) ends removed.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn concurrent_refreshes_end_on_the_latest_observation() {
    let (id, config) = srv_shed("s1");
    let hosts = start_queued(&config, &id, Vec::new());
    refresh(&hosts, &["s1"]).await;
    assert!(lock(&hosts.reg).ids.contains(&id));

    let cases: [(&'static [&'static str], &'static [&'static str], bool); 2] =
        [(&[], &["s1"], true), (&["s1"], &[], false)];
    for (earlier, latest, registered) in cases {
        // The registering probe of the previous step done, so a probe in
        // flight cannot stand in for (or block) this step's.
        wait_for("no probe in flight", || {
            (!lock(&hosts.probing).contains(&id)).then_some(())
        })
        .await;
        let (gap, reached, proceed) = test_gap();
        *lock(&hosts.observe_gap) = Some(gap);
        let first = {
            let hosts = Arc::clone(&hosts);
            tokio::spawn(async move { refresh(&hosts, earlier).await })
        };
        reached.notified().await;
        let second = {
            let hosts = Arc::clone(&hosts);
            tokio::spawn(async move { refresh(&hosts, latest).await })
        };
        // Time for an UNSERIALIZED later refresh to run to its end — it has
        // no await to be held at. A serialized one waits for the first
        // whatever this sleep is.
        tokio::time::sleep(Duration::from_millis(100)).await;
        proceed.notify_one();
        first.await.expect("the earlier refresh");
        second.await.expect("the later refresh");
        assert_eq!(
            lock(&hosts.reg).ids.contains(&id),
            registered,
            "the latest observation ({latest:?} running) is the one the registry ends on"
        );
        assert_eq!(lock(&hosts.state).contains_key(&id), registered);
    }
}

/// **A refresh's removals are waited for TOGETHER** (C9 confirmation, N3):
/// three sheds stop at once with each craze source's dial still held — as a
/// lane verb in flight holds it — so each stop waits its whole
/// `CRAZE_STOP_WAIT` out, and the refresh returns after about one wait, not
/// three.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn a_refreshs_removals_wait_once_not_once_per_host() {
    let names = ["s1", "s2", "s3"];
    let (_, config) = srv_shed("s1");
    let scripted: Vec<(HostId, Arc<Scripted>)> = names
        .iter()
        .map(|n| (srv_shed(n).0, Scripted::attach_only()))
        .collect();
    let hosts = start_scripted(&config, &test_options(&[]), scripted.clone());
    refresh(&hosts, &names).await;
    let held: Vec<(CrazeSource, u64)> = {
        let mut held = Vec::new();
        for (id, _) in &scripted {
            held.push(
                wait_for("the craze source", || {
                    hosts.craze_source(&id.address()).ok()
                })
                .await,
            );
        }
        held
    };

    let started = std::time::Instant::now();
    refresh(&hosts, &[]).await;
    let took = started.elapsed();
    for (id, _) in &scripted {
        assert!(!lock(&hosts.state).contains_key(id), "{id}: removed");
    }
    assert!(
        took >= CRAZE_STOP_WAIT,
        "each stop waited its held dial out (took {took:?})"
    );
    assert!(
        took < CRAZE_STOP_WAIT * 2,
        "the three waits ran together, not one after another (took {took:?})"
    );
    drop(held);
}
