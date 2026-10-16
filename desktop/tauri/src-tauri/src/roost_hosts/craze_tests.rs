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
use shed_craze::testing::{
    full_hub_capabilities, reopen_hub_capabilities, roster_row, HookDial, HubEnd, ScriptedDial,
};
use shed_craze::{CrazeDial, CrazeStream, DialError, Probe};
use tokio::sync::mpsc::UnboundedReceiver;

use super::*;
use crate::craze::{BoxFut, CrazeProbe, CrazeReach, CrazeTimings};
use crate::lane::Lanes;
use crate::machines::ReachOptions;
use shed_app::roost::LocalSession;

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
    /// The scripted dial under `hook`, for a test that fails dials.
    dial: Arc<ScriptedDial>,
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
            hook: HookDial::new(Arc::clone(&dial) as Arc<dyn CrazeDial>),
            dial,
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
            // The scripted dial is ungated (the gate is `SshCrazeDial`'s), so
            // an explicit action dials the same one.
            ensure: None,
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

/// The departure grace these cells' lanes run with (plan 032 §3.1's seam).
const GRACE: Duration = Duration::from_millis(300);

/// The lanes over a craze roster, wired the way `lib.rs` wires them, with a
/// [`GRACE`] departure grace.
fn lanes_over(hosts: &Arc<RoostHosts>) -> Arc<Lanes> {
    lanes_holding(hosts, GRACE, Arc::new(NewsHold::default()))
}

/// [`lanes_over`], with a `grace` departure grace and its craze news passed
/// through `hold` — which holds back nothing until a cell says what.
fn lanes_holding(hosts: &Arc<RoostHosts>, grace: Duration, hold: Arc<NewsHold>) -> Arc<Lanes> {
    let lanes = Arc::new(
        Lanes::for_test(
            tokio::runtime::Handle::current(),
            Arc::clone(hosts) as Arc<dyn crate::lane::LaneMachines>,
        )
        .with_depart_grace(grace),
    );
    let weak = Arc::downgrade(&lanes);
    hosts.set_lane_observer(Arc::new(move |m: &str, open: &_| {
        if let Some(l) = weak.upgrade() {
            l.reconcile(m, open);
        }
    }));
    let weak = Arc::downgrade(&lanes);
    hosts.set_craze_observer(Arc::new(move |m: &str, gen: u64, news: CrazeNews<'_>| {
        if hold.holds(m, gen, &news) {
            return;
        }
        if let Some(l) = weak.upgrade() {
            l.craze_news(m, gen, news);
        }
    }));
    lanes
}

/// **A hand on the craze news hook** (C1 re-review): while it holds, every
/// RETURN is held back instead of applied — the fold that minted it under the
/// state lock paused before publishing it, the window every publisher has —
/// until [`Self::release`] delivers it, after news minted LATER.
#[derive(Default)]
struct NewsHold {
    holding: std::sync::atomic::AtomicBool,
    held: std::sync::Mutex<Vec<HeldReturn>>,
    /// The number of every return published, held or not, in order.
    returns: std::sync::Mutex<Vec<u64>>,
}

/// A return a [`NewsHold`] held back, owned.
struct HeldReturn {
    machine: String,
    gen: u64,
    host_ids: Vec<String>,
    seq: u64,
}

impl NewsHold {
    fn hold_returns(&self, hold: bool) {
        self.holding.store(hold, Ordering::SeqCst);
    }

    /// Note `news`, and hold it back if it is a return and this holds —
    /// answering whether it did.
    fn holds(&self, machine: &str, gen: u64, news: &CrazeNews<'_>) -> bool {
        let CrazeNews::Returned { host_ids, seq } = *news else {
            return false;
        };
        lock(&self.returns).push(seq);
        if !self.holding.load(Ordering::SeqCst) {
            return false;
        }
        lock(&self.held).push(HeldReturn {
            machine: machine.to_string(),
            gen,
            host_ids: host_ids.to_vec(),
            seq,
        });
        true
    }

    /// The number of the first return held back, once one is.
    fn held_seq(&self) -> Option<u64> {
        lock(&self.held).first().map(|held| held.seq)
    }

    /// The number of the last return published.
    fn last_return(&self) -> Option<u64> {
        lock(&self.returns).last().copied()
    }

    /// Deliver every return held back to `lanes`, in the order it was held.
    fn release(&self, lanes: &Lanes) {
        let held = std::mem::take(&mut *lock(&self.held));
        for piece in &held {
            let news = CrazeNews::Returned {
                host_ids: &piece.host_ids,
                seq: piece.seq,
            };
            lanes.craze_news(&piece.machine, piece.gen, news);
        }
    }
}

/// A departure of host A (`Removed`) minted under the state lock with the
/// generation it is judged by — answering that number — to be published
/// later, from another task: the news fence's general case. No publisher but
/// the fold departs a lane today (plan 032 E6: End tab publishes nothing),
/// so the cells play one, both halves apart, to pin that the lane layer
/// orders news by its number and not by when it lands.
fn departure_minted(hosts: &RoostHosts, id: &HostId) -> (u64, u64) {
    let mut state = lock(&hosts.state);
    let m = state.get_mut(id).expect("the host's state");
    (m.craze.gen, m.craze.mint_news())
}

/// …and published, as a publisher would.
fn departure_published(hosts: &RoostHosts, id: &HostId, (gen, seq): (u64, u64)) {
    let ended = [HOST_A.to_string()];
    publish_craze_gone(&hosts.on_craze, id, gen, seq, &ended, Departure::Removed);
}

/// The craze lane on `host_id`'s state on `machine` — `live`, `departing`,
/// `finished` — or `None` with no entry.
fn lane_state(lanes: &Lanes, machine: &str, host_id: &str) -> Option<&'static str> {
    lanes.lane_state(machine, "craze", host_id)
}

/// `lane.messages`' `stale` for the craze lane on `host_id` — a finished
/// lane's final reason.
fn reason(lanes: &Lanes, machine: &str, host_id: &str) -> Value {
    lanes
        .messages(machine, "craze", host_id)
        .expect("a craze lane stays readable")["stale"]
        .clone()
}

/// **A roost snapshot does not evict a craze lane, and a `Ready` swap that
/// drops its row does** (plan 025 §3.6.4): `reconcile` judges only the
/// roost-stamped lanes, and the craze source's own reseed is what ends a
/// craze one — an epoch reseed carries no `Removed`. Since plan 032 the swap
/// starts the lane's DEPARTURE, and the grace ends it `session_gone`: it stays
/// registered and readable, finished — never removed, never `no_lane`.
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

    // The hub reseeds (a roster reset) WITHOUT host A: its lane departs, and
    // ends with the grace.
    roster.reset("sub-1", "omitted").await;
    let resub = roster.expect("sessions.subscribe").await;
    roster
        .reply(
            &resub,
            json!({"subscription": "sub-2", "epoch": EPOCH, "cursor": 3,
                   "sessions": [row(HOST_B, "ses-b")]}),
        )
        .await;
    wait_for("the swap to depart A's lane", || {
        (lane_state(&lanes, LOCALHOST, HOST_A) == Some("departing")).then_some(())
    })
    .await;
    wait_for("the grace to end A's lane", || {
        (lane_state(&lanes, LOCALHOST, HOST_A) == Some("finished")).then_some(())
    })
    .await;
    assert_eq!(reason(&lanes, LOCALHOST, HOST_A), "session_gone");
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
/// review): the shed's craze lane on hostId A is open; the shed is removed
/// (the lane ends `host_removed` at once, and stays readable) and registered
/// again, and a lane on the SAME hostId opens through the new source,
/// REPLACING the finished one. The old source's news — published, as its sink
/// does, after its fence was passed — then arrives, and the new lane stays
/// live; the new generation's own news starts its departure.
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
    assert_eq!(
        lane_state(&lanes, &address, HOST_A),
        Some("finished"),
        "removing the host ended its lane"
    );
    assert_eq!(reason(&lanes, &address, HOST_A), "host_removed");
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

    assert_eq!(
        lane_state(&lanes, &address, HOST_A),
        Some("live"),
        "the new generation's lane replaced the finished one"
    );

    // The OLD source's news, late — numbered past anything of the new one's,
    // which no other generation's number ever outranks (C1 re-review).
    let gone = [HOST_A.to_string()];
    publish_craze_gone(
        &hosts.on_craze,
        &id,
        old_gen,
        u64::MAX,
        &gone,
        Departure::Removed,
    );
    publish_craze(
        &hosts.on_craze,
        &id,
        old_gen,
        CrazeNews::HostRemoved { seq: u64::MAX },
    );
    assert_eq!(
        lane_state(&lanes, &address, HOST_A),
        Some("live"),
        "a stale source's eviction never ends a newer generation's lane"
    );
    // The current generation's does.
    let seq = mint_news(&hosts, &id);
    publish_craze_gone(
        &hosts.on_craze,
        &id,
        new_gen,
        seq,
        &gone,
        Departure::Removed,
    );
    assert_eq!(lane_state(&lanes, &address, HOST_A), Some("departing"));
}

/// `id`'s craze source's next news number, minted under the state lock as
/// every publisher mints its own ([`crate::craze::CrazeState::mint_news`]).
fn mint_news(hosts: &RoostHosts, id: &HostId) -> u64 {
    lock(&hosts.state)
        .get_mut(id)
        .expect("the host's state")
        .craze
        .mint_news()
}

/// **A row listed again within the grace keeps its lane** (plan 032 §3.1.1
/// item 3, W1 cell 2 through the real fold): the roster removes A — its lane
/// departs — and lists it again before the grace ends; `HostCrazeSink::fold`
/// publishes the return, and the lane stays live past the grace.
#[tokio::test]
async fn a_row_listed_again_within_the_grace_keeps_its_lane() {
    let craze = Scripted::eager();
    let hosts = Arc::new(start_scripted(
        &ShedConfig::default(),
        &test_options(&[]),
        vec![(local(), Arc::clone(&craze))],
    ));
    let lanes = lanes_over(&hosts);
    let mut roster = craze.roster(json!([row(HOST_A, "ses-a")])).await;
    wait_for("the row", || (craze_rows(&hosts).len() == 1).then_some(())).await;
    lanes
        .open(LOCALHOST, "craze", HOST_A)
        .await
        .expect("the craze lane");

    roster
        .roster("sub-1", EPOCH, json!([]), json!([HOST_A]))
        .await;
    wait_for("the removal to depart the lane", || {
        (lane_state(&lanes, LOCALHOST, HOST_A) == Some("departing")).then_some(())
    })
    .await;
    roster
        .roster("sub-1", EPOCH, json!([row(HOST_A, "ses-a")]), json!([]))
        .await;
    wait_for("the return to keep the lane", || {
        (lane_state(&lanes, LOCALHOST, HOST_A) == Some("live")).then_some(())
    })
    .await;
    tokio::time::sleep(GRACE * 2).await;
    assert_eq!(
        lane_state(&lanes, LOCALHOST, HOST_A),
        Some("live"),
        "the cancelled grace ended nothing"
    );
}

/// **A Dormant departure expires `hub_gone`** (plan 032 PC14g, W1 cell 16
/// through the real source): the remote hub's roster ends, the find-only
/// probe finds no hub, the source goes Dormant — every row leaves WITH the
/// hub — and the lane's grace ends it `hub_gone`, not `session_gone`.
#[tokio::test]
async fn a_dormant_sources_lane_ends_hub_gone() {
    let craze = Scripted::attach_only();
    let id = HostId::Machine("mini3".to_string());
    let hosts = Arc::new(start_scripted(
        &config_with(&["mini3"]),
        &test_options(&[]),
        vec![(id.clone(), Arc::clone(&craze))],
    ));
    let lanes = lanes_over(&hosts);
    let probe = craze.probe.as_ref().unwrap();
    probe.answer(Probe::Hub);
    let roster = craze.roster(json!([row(HOST_A, "ses-a")])).await;
    wait_for("live", || {
        (status_of(&hosts, "mini3").unwrap()["craze"]["state"] == "live").then_some(())
    })
    .await;
    let address = id.address();
    lanes
        .open(&address, "craze", HOST_A)
        .await
        .expect("the craze lane");

    // The hub goes: its roster connection ends, and the probe finds none.
    roster.close().await;
    probe.answer(Probe::NoHub);
    wait_for("the source to go dormant", || {
        (status_of(&hosts, "mini3").unwrap()["craze"]["state"] == "dormant").then_some(())
    })
    .await;
    wait_for("the grace to end the lane", || {
        (lane_state(&lanes, &address, HOST_A) == Some("finished")).then_some(())
    })
    .await;
    assert_eq!(reason(&lanes, &address, HOST_A), "hub_gone");
}

/// **The hub gone between the probe and the roster's dial** (plan 032 §3.5,
/// W5): the probe still saw a hub, but the roster's own dial — the find-only
/// `bridge --hub --no-start` — finds none (`DialError::NoHub`, so the source
/// says `Offline{NoHub}`). That is the source going DORMANT, not offline: every
/// row leaves WITH the hub (a lane on one ends `hub_gone`), the subscription is
/// dropped, and nothing redials — the next thing the source does is probe.
#[tokio::test]
async fn a_roster_that_finds_no_hub_sends_the_source_dormant() {
    let craze = Scripted::attach_only();
    let id = HostId::Machine("mini3".to_string());
    let hosts = Arc::new(start_scripted(
        &config_with(&["mini3"]),
        &test_options(&[]),
        vec![(id.clone(), Arc::clone(&craze))],
    ));
    let lanes = lanes_over(&hosts);
    let probe = craze.probe.as_ref().unwrap();
    probe.answer(Probe::Hub);
    let roster = craze.roster(json!([row(HOST_A, "ses-a")])).await;
    wait_for("live", || {
        (status_of(&hosts, "mini3").unwrap()["craze"]["state"] == "live").then_some(())
    })
    .await;
    let address = id.address();
    lanes
        .open(&address, "craze", HOST_A)
        .await
        .expect("the craze lane");
    // The lane has dialled its own connection (left unanswered), so the next
    // dial is the roster's.
    wait_for("the lane's dial", || {
        (craze.hook.dials() == 2).then_some(())
    })
    .await;

    // The roster ends; the probe still finds a hub; it is gone by the redial.
    craze
        .dial
        .fail_next(DialError::NoHub("craze bridge: no hub is running".into()));
    roster.close().await;
    probe.answer(Probe::Hub);
    wait_for("the source to go dormant", || {
        (status_of(&hosts, "mini3").unwrap()["craze"]["state"] == "dormant").then_some(())
    })
    .await;
    let status = status_of(&hosts, "mini3").unwrap();
    assert_eq!(
        status["craze"]["cause"],
        Value::Null,
        "dormant, not offline: {status}"
    );
    assert!(
        craze_rows(&hosts).is_empty(),
        "nothing is listed from craze"
    );
    assert_eq!(craze.hook.dials(), 3, "the roster's one find-only dial");
    wait_for("the grace to end the lane", || {
        (lane_state(&lanes, &address, HOST_A) == Some("finished")).then_some(())
    })
    .await;
    assert_eq!(reason(&lanes, &address, HOST_A), "hub_gone");

    // Dormant: the next thing is a probe, and it dials nothing.
    probe.answer(Probe::NoHub);
    wait_for("a dormant probe to take its answer", || {
        lock(&probe.answers).is_empty().then_some(())
    })
    .await;
    assert_eq!(craze.hook.dials(), 3, "a dormant source never redials");
    assert_eq!(
        status_of(&hosts, "mini3").unwrap()["craze"]["state"],
        "dormant"
    );
}

/// **A lane on a created-only row departs `hub_gone` when the hub goes
/// before any seed** (plan 032 PC14g, branch review): a dormant remote
/// machine's create starts a hub and answers with a session no roster has
/// listed — the source's created row, which the transcript opens on at once —
/// and the hub is gone again by the source's next probe, before its roster
/// ever seeded. That Dormant departs the created row's lane with the
/// roster's rows, and the grace ends it `hub_gone`. Before the fix a Dormant
/// departed only the rows its roster had listed — none here — and nothing
/// ever ended the lane on screen.
#[tokio::test]
async fn a_lane_on_a_created_row_departs_hub_gone_when_the_hub_goes_before_any_seed() {
    let craze = Scripted::attach_only();
    let id = HostId::Machine("mini3".to_string());
    let hosts = Arc::new(start_scripted(
        &config_with(&["mini3"]),
        &test_options(&[]),
        vec![(id.clone(), Arc::clone(&craze))],
    ));
    let lanes = lanes_over(&hosts);
    let address = id.address();
    let probe = craze.probe.as_ref().unwrap();
    probe.answer(Probe::NoHub);
    wait_for("the source dormant", || {
        lock(&hosts.state)
            .get(&id)
            .is_some_and(|m| m.craze.phase == Some(CrazePhase::Dormant))
            .then_some(())
    })
    .await;
    // The dormant source's next probe is asked before the create.
    wait_for("the next probe", || {
        (probe.calls.load(Ordering::SeqCst) == 2).then_some(())
    })
    .await;

    // The create starts a hub (the explicit action's dial) and answers with a
    // session no roster lists: the source's created row.
    let create = {
        let hosts = Arc::clone(&hosts);
        let address = address.clone();
        tokio::spawn(async move {
            hosts
                .craze_create(&address, create_req("shed-hub-gone-1"), false)
                .await
        })
    };
    let mut hub = craze.next().await;
    hub.hello(EPOCH, full_hub_capabilities()).await;
    let req = hub.expect("session.create").await;
    hub.reply(&req, created_result("ses-new")).await;
    let answer = create.await.unwrap().expect("created");
    assert_eq!(answer["ended"], false, "{answer}");
    assert!(
        lock(&hosts.state)
            .get(&id)
            .is_some_and(|m| !m.craze.rows.contains_key(HOST_NEW)),
        "no roster lists it: only the source's created row does"
    );
    lanes
        .open(&address, "craze", HOST_NEW)
        .await
        .expect("the transcript of a just-created session opens at once");
    // The lane's own connection, left unanswered.
    let _lane_conn = craze.next().await;
    assert_eq!(lane_state(&lanes, &address, HOST_NEW), Some("live"));

    // The hub is gone by the source's next probe — before any seed. (The
    // probe asked before the create looked too early to judge the session's
    // hub — its answer is stale, C8 review finding 2 — so it is asked again,
    // and that answer is the one that folds.)
    probe.answer(Probe::NoHub);
    probe.answer(Probe::NoHub);
    wait_for("the grace to end the created row's lane", || {
        (lane_state(&lanes, &address, HOST_NEW) == Some("finished")).then_some(())
    })
    .await;
    assert_eq!(reason(&lanes, &address, HOST_NEW), "hub_gone");
}

/// **A re-registered host never adopts a departing lane** (plan 032 §3.1.1
/// item 12, W1 cell 14): the lane departs (the roster removed its row), the
/// host is removed — the lane ends `host_removed` at once, no grace — and is
/// registered again, its NEW source listing the same hostId. That return is
/// another generation's: the old lane stays finished. And `open` does not
/// adopt it either — a fresh lane opens through the new source, filed under
/// its generation.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn a_reregistered_host_never_adopts_a_departing_lane() {
    let (id, config) = srv_shed("s1");
    let address = id.address();
    let old = Scripted::attach_only();
    let new = Scripted::attach_only();
    let hosts = start_queued(&config, &id, vec![Arc::clone(&old), Arc::clone(&new)]);
    let lanes = lanes_over(&hosts);
    refresh(&hosts, &["s1"]).await;
    old.probe.as_ref().unwrap().answer(Probe::Hub);
    let mut old_end = old.roster(json!([row(HOST_A, "ses-a")])).await;
    wait_for("the old row", || {
        (craze_rows(&hosts).len() == 1).then_some(())
    })
    .await;
    lanes
        .open(&address, "craze", HOST_A)
        .await
        .expect("the old generation's lane");
    let old_gen = lock(&hosts.state).get(&id).expect("state").craze.gen;
    old_end
        .roster("sub-1", EPOCH, json!([]), json!([HOST_A]))
        .await;
    wait_for("the lane to depart", || {
        (lane_state(&lanes, &address, HOST_A) == Some("departing")).then_some(())
    })
    .await;
    // Another generation's return, while it departs: nothing.
    publish_craze(
        &hosts.on_craze,
        &id,
        old_gen + 1_000,
        CrazeNews::Returned {
            host_ids: &[HOST_A.to_string()],
            seq: u64::MAX,
        },
    );
    assert_eq!(lane_state(&lanes, &address, HOST_A), Some("departing"));

    refresh(&hosts, &[]).await;
    assert_eq!(
        lane_state(&lanes, &address, HOST_A),
        Some("finished"),
        "the host's removal swept its departing lane at once"
    );
    assert_eq!(reason(&lanes, &address, HOST_A), "host_removed");
    refresh(&hosts, &["s1"]).await;
    new.probe.as_ref().unwrap().answer(Probe::Hub);
    let _new_end = new.roster(json!([row(HOST_A, "ses-a")])).await;
    wait_for("the new row", || {
        (craze_rows(&hosts).len() == 1).then_some(())
    })
    .await;
    let new_gen = lock(&hosts.state).get(&id).expect("state").craze.gen;
    assert_ne!(old_gen, new_gen);
    tokio::time::sleep(GRACE).await;
    assert_eq!(
        lane_state(&lanes, &address, HOST_A),
        Some("finished"),
        "the new source's listing revived nothing"
    );
    lanes
        .open(&address, "craze", HOST_A)
        .await
        .expect("a fresh lane through the new source");
    assert_eq!(lane_state(&lanes, &address, HOST_A), Some("live"));
    assert_eq!(
        lanes.messages(&address, "craze", HOST_A).expect("readable")["finished"],
        false
    );
}

/// The departure grace of the cells that order craze news: long enough that
/// a cell's steps between a departure and its late news never outlast it.
const ORDER_GRACE: Duration = Duration::from_secs(2);

/// `localhost`'s craze roster listing host A, and a lane open on A — over an
/// [`ORDER_GRACE`], with `hold` on the lanes' news. Answers the roster's
/// connection, and the scripted craze it rides (kept alive by the cell).
async fn lane_on_a(hold: &Arc<NewsHold>) -> (Arc<RoostHosts>, Arc<Lanes>, HubEnd, Arc<Scripted>) {
    let craze = Scripted::eager();
    let hosts = Arc::new(start_scripted(
        &ShedConfig::default(),
        &test_options(&[]),
        vec![(local(), Arc::clone(&craze))],
    ));
    let lanes = lanes_holding(&hosts, ORDER_GRACE, Arc::clone(hold));
    let roster = craze.roster(json!([row(HOST_A, "ses-a")])).await;
    wait_for("the row", || (craze_rows(&hosts).len() == 1).then_some(())).await;
    lanes
        .open(LOCALHOST, "craze", HOST_A)
        .await
        .expect("the craze lane");
    (hosts, lanes, roster, craze)
}

/// Until A's lane is `state`.
async fn lane_becomes(lanes: &Lanes, state: &'static str) {
    wait_for(state, || {
        (lane_state(lanes, LOCALHOST, HOST_A) == Some(state)).then_some(())
    })
    .await;
}

/// **A delayed return never cancels a newer departure** (C1
/// re-review — the reported interleaving): the roster drops A, and its lane
/// departs; a reseed restores the row, and its fold (the real one) mints the
/// return and pauses before publishing it; a departure of the restored row
/// completes, its departure minted AFTER the return, and publishes it — a
/// duplicate for the departing lane, but newer news; THEN the return lands.
/// The lane does not take it: it stays departing, and its grace still ends
/// it `session_gone`, on time — at the first departure's deadline. Unfenced,
/// the return revived the lane and cancelled its grace, and with no final
/// event the 10 s end was lost.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn a_delayed_return_never_cancels_a_newer_departure() {
    let hold = Arc::new(NewsHold::default());
    let (hosts, lanes, mut roster, _craze) = lane_on_a(&hold).await;

    // The roster drops A: the lane departs, its grace from here.
    let departed = std::time::Instant::now();
    roster
        .roster("sub-1", EPOCH, json!([]), json!([HOST_A]))
        .await;
    lane_becomes(&lanes, "departing").await;

    // A reseed restores A — and its fold pauses, the return minted.
    hold.hold_returns(true);
    roster.reset("sub-1", "omitted").await;
    let resub = roster.expect("sessions.subscribe").await;
    roster
        .reply(
            &resub,
            json!({"subscription": "sub-2", "epoch": EPOCH, "cursor": 3,
                   "sessions": [row(HOST_A, "ses-a")]}),
        )
        .await;
    let back = wait_for("the reseed's return, held", || hold.held_seq()).await;
    hold.hold_returns(false);

    // a departure of the restored row: its departure, minted after the
    // return, lands first.
    let departure = departure_minted(&hosts, &local());
    assert!(
        back < departure.1,
        "the return ({back}) was minted before the departure ({})",
        departure.1
    );
    departure_published(&hosts, &local(), departure);
    assert_eq!(lane_state(&lanes, LOCALHOST, HOST_A), Some("departing"));

    // The delayed return lands.
    hold.release(&lanes);
    assert_eq!(
        lane_state(&lanes, LOCALHOST, HOST_A),
        Some("departing"),
        "a return minted before the departure never revives the lane"
    );
    lane_becomes(&lanes, "finished").await;
    let took = departed.elapsed();
    assert!(
        took >= ORDER_GRACE && took < ORDER_GRACE + Duration::from_secs(1),
        "ended {took:?} after the row left (grace {ORDER_GRACE:?})"
    );
    assert_eq!(reason(&lanes, LOCALHOST, HOST_A), "session_gone");
}

/// **A stale departure never departs a lane a newer return kept**
/// (C1 re-review — the reverse interleaving): a departure of A is minted and
/// its task pauses before publishing it; meanwhile the roster drops A and
/// lists it again — a departure and a return, minted by the real fold after
/// it, and applied; THEN the departure lands. The lane already took
/// newer news, so it stays live — no departure, no grace, no `session_gone` in
/// 10 s for a row that is listed.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn a_stale_departure_never_departs_a_returned_lane() {
    let hold = Arc::new(NewsHold::default());
    let (hosts, lanes, mut roster, _craze) = lane_on_a(&hold).await;

    // A departure minted — and its task paused before publishing.
    let departure = departure_minted(&hosts, &local());

    // The roster drops A and lists it again, both after the departure.
    roster
        .roster("sub-1", EPOCH, json!([]), json!([HOST_A]))
        .await;
    lane_becomes(&lanes, "departing").await;
    roster
        .roster("sub-1", EPOCH, json!([row(HOST_A, "ses-a")]), json!([]))
        .await;
    lane_becomes(&lanes, "live").await;
    let back = hold.last_return().expect("the roster's return");
    assert!(
        departure.1 < back,
        "the departure ({}) was minted before the return ({back})",
        departure.1
    );

    // The stale departure lands.
    departure_published(&hosts, &local(), departure);
    assert_eq!(
        lane_state(&lanes, LOCALHOST, HOST_A),
        Some("live"),
        "a departure minted before the return never departs the lane again"
    );
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
            .is_some_and(|m| m.fold_plan(&[]).hidden.contains(&TAB));
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

// ---------------------------------------------------------------------------
// C10: the create sheet's ops and Open in terminal (plan 025 §3.6.6, §3.8)
// ---------------------------------------------------------------------------

/// The created session's hostId in the C10 cells.
const HOST_NEW: &str = "cccccccccccc";

/// A create request the scripted hub answers.
fn create_req(id: &str) -> LaneCreateRequest {
    crate::craze::create_request(
        Some("/work/new"),
        Some("grok"),
        Some("hello"),
        None,
        Some(id),
    )
    .expect("a well-formed request")
}

/// `session.create`'s result for [`HOST_NEW`], its provider session `psid`.
fn created_result(psid: &str) -> serde_json::Value {
    json!({"session": roster_row(HOST_NEW, "craze-new", "/work/new",
                                 json!({"title": "new one", "activity": "working",
                                        "providerSessionId": psid})),
           "prompt": "accepted"})
}

/// The layer with one host's scripted craze, on chosen clocks.
fn start_scripted_with(
    config: &ShedConfig,
    options: &ReachOptions,
    reaches: Vec<(HostId, Arc<Scripted>)>,
    timings: CrazeTimings,
) -> RoostHosts {
    let map: BTreeMap<HostId, Arc<Scripted>> = reaches.into_iter().collect();
    RoostHosts::start_with(
        &tokio::runtime::Handle::current(),
        config,
        options.clone(),
        false,
        Arc::new(|| {}),
        CrazeReaches::Fixed(Arc::new(move |id: &HostId| map.get(id).map(|s| s.reach()))),
        timings,
    )
}

/// **A just-created session's transcript opens at once** (plan 025 §3.6.4,
/// the C9 hand-off): the roster never lists the new session here (its flush
/// still to come), and `lane.open` on it straight after the create succeeds —
/// the create's row was folded into this host's state (so the stamp
/// resolves) AND kept by the source (so the lane's `session()` answers). The
/// row is listed, live, with its craze facts; the answer names the hostId
/// and echoes the request id.
#[tokio::test]
async fn a_created_session_is_listed_and_openable_at_once() {
    let craze = Scripted::eager();
    let hosts = Arc::new(start_scripted(
        &ShedConfig::default(),
        &test_options(&[]),
        vec![(local(), Arc::clone(&craze))],
    ));
    let lanes = lanes_over(&hosts);
    let _roster = craze.roster(json!([row(HOST_A, "ses-a")])).await;
    wait_for("the roster", || {
        (craze_rows(&hosts).len() == 1).then_some(())
    })
    .await;

    let create = {
        let hosts = Arc::clone(&hosts);
        tokio::spawn(async move {
            hosts
                .craze_create(LOCALHOST, create_req("shed-req-1"), false)
                .await
        })
    };
    let mut hub = craze.next().await;
    hub.hello(EPOCH, full_hub_capabilities()).await;
    let req = hub.expect("session.create").await;
    assert_eq!(req["params"]["requestId"], "shed-req-1");
    hub.reply(&req, created_result("ses-new")).await;
    let answer = create.await.unwrap().expect("created");
    assert_eq!(answer["host_id"], HOST_NEW);
    assert_eq!(answer["request_id"], "shed-req-1");
    assert_eq!(answer["prompt"], "accepted");
    assert_eq!(answer["session"]["slug"], HOST_NEW);
    assert_eq!(answer["session"]["stale"], false);

    // No roster frame names it — and it is listed, and its lane opens.
    let listed = craze_rows(&hosts);
    assert!(
        listed.iter().any(|r| r["slug"] == HOST_NEW),
        "listed at once: {listed:?}"
    );
    lanes
        .open(LOCALHOST, "craze", HOST_NEW)
        .await
        .expect("the transcript of a just-created session opens at once");
    assert!(lanes.is_open(LOCALHOST, "craze", HOST_NEW));
}

/// **Open in terminal** (plan 025 §3.6.6): a roost `tab.open` whose argv is
/// EXACTLY `attach_argv(<hostId>)` and whose cwd is the row's workspace; the
/// hub row shows that tab — and still does after a full roost resync (the
/// opened-tab map, since roost reports the tab unowned); End tab on it only
/// DETACHES (the session's lane stays); and a tab roost stops listing leaves
/// the map.
#[tokio::test]
async fn open_in_terminal_attaches_a_tab_that_survives_roost_snapshots() {
    let fake = FakeRoost::start().await;
    let craze = Scripted::eager();
    let hosts = Arc::new(start_scripted(
        &ShedConfig::default(),
        &test_options(&[(LOCALHOST, fake.socket_path())]),
        vec![(local(), Arc::clone(&craze))],
    ));
    let lanes = lanes_over(&hosts);
    let _roster = craze.roster(json!([row(HOST_A, "ses-a")])).await;
    wait_for("the hub row", || {
        (craze_rows(&hosts).len() == 1).then_some(())
    })
    .await;
    let hub_row = || {
        craze_rows(&hosts)
            .into_iter()
            .find(|r| r["slug"] == HOST_A)
            .expect("the hub row")
    };
    assert!(hub_row().get("tab_id").is_none(), "headless before");

    let opened = hosts
        .craze_open_terminal(LOCALHOST, HOST_A)
        .await
        .expect("Open in terminal");
    let calls = fake.tab_open_calls();
    let call = calls.last().expect("a tab.open reached roost");
    assert_eq!(
        call["argv"],
        json!(shed_core::craze::attach_argv(HOST_A).unwrap()),
        "the argv is attach_argv(<hostId>), verbatim"
    );
    assert_eq!(call["cwd"], "/work", "the row's workspace");
    let tab: i64 = opened["tab_id"].as_str().unwrap().parse().unwrap();
    assert_eq!(
        hub_row()["tab_id"],
        tab.to_string(),
        "the row shows its tab"
    );
    assert!(
        !rows_of(&hosts).iter().any(|r| r["source"] == "roost"),
        "the attach tab is not a roost row"
    );

    // A full roost resync: the tab is unowned there, and the row keeps it.
    let before = fake.tab_list_calls();
    fake.end_stream("backend-switch");
    wait_for("the resync's tab.list", || {
        (fake.tab_list_calls() > before).then_some(())
    })
    .await;
    tokio::time::sleep(Duration::from_millis(50)).await;
    assert_eq!(
        hub_row()["tab_id"],
        tab.to_string(),
        "the row keeps its tab across a roost snapshot"
    );

    // End tab on it: the tab closes, the session (and its lane) run on.
    lanes.open(LOCALHOST, "craze", HOST_A).await.expect("lane");
    hosts
        .kill(LOCALHOST, &tab.to_string())
        .await
        .expect("an attach tab is closable from its row");
    assert!(!fake.tab_ids().contains(&tab));
    assert!(hub_row().get("tab_id").is_none(), "headless again");
    assert!(
        lanes.is_open(LOCALHOST, "craze", HOST_A),
        "closing an attach tab only detaches: the lane stays"
    );

    // A second tab, closed on roost's side: the close's snapshot is past the
    // tab's fence and does not list it, so the map lets it go.
    let again = hosts
        .craze_open_terminal(LOCALHOST, HOST_A)
        .await
        .expect("Open in terminal again");
    let tab2: i64 = again["tab_id"].as_str().unwrap().parse().unwrap();
    let reach = LocalSession::new(LOCALHOST, fake.socket_path());
    tab_close(&reach, tab2).await.expect("closed on roost");
    wait_for("the map to drop the closed tab", || {
        (!lock(&hosts.state)
            .get(&local())
            .is_some_and(|m| m.opened_tabs.contains_key(&tab2)))
        .then_some(())
    })
    .await;
    assert!(hub_row().get("tab_id").is_none());
}

/// A session still starting names no provider session yet, so the shared
/// rule has nothing to match its opened tab on: it attaches by hostId, and
/// the newest of two such tabs wins.
#[tokio::test]
async fn an_opened_tab_on_a_session_with_no_provider_session_attaches_by_host_id() {
    let fake = FakeRoost::start().await;
    let craze = Scripted::eager();
    let hosts = start_scripted(
        &ShedConfig::default(),
        &test_options(&[(LOCALHOST, fake.socket_path())]),
        vec![(local(), Arc::clone(&craze))],
    );
    let starting = roster_row(
        HOST_A,
        "craze-a",
        "/work",
        json!({"title": "starting", "activity": "starting"}),
    );
    let _roster = craze.roster(json!([starting])).await;
    wait_for("the hub row", || {
        (craze_rows(&hosts).len() == 1).then_some(())
    })
    .await;
    hosts.craze_open_terminal(LOCALHOST, HOST_A).await.unwrap();
    let newer = hosts.craze_open_terminal(LOCALHOST, HOST_A).await.unwrap();
    let rows = craze_rows(&hosts);
    assert_eq!(rows.len(), 1, "{rows:?}");
    assert_eq!(
        rows[0]["tab_id"], newer["tab_id"],
        "the newest tab attaches"
    );
}

/// **The desktop's use of `attach_argv` refuses a hostId not in craze's form**
/// before anything reaches roost: the id is spliced into a command line, and a
/// hub's roster is where it came from.
#[tokio::test]
async fn open_in_terminal_refuses_a_host_id_not_in_crazes_form() {
    let fake = FakeRoost::start().await;
    let craze = Scripted::eager();
    let hosts = start_scripted(
        &ShedConfig::default(),
        &test_options(&[(LOCALHOST, fake.socket_path())]),
        vec![(local(), Arc::clone(&craze))],
    );
    const ODD: &str = "0123456789AB";
    let _roster = craze.roster(json!([row(ODD, "ses-odd")])).await;
    wait_for("the odd row", || {
        (craze_rows(&hosts).len() == 1).then_some(())
    })
    .await;
    let refused = hosts
        .craze_open_terminal(LOCALHOST, ODD)
        .await
        .expect_err("not twelve lowercase hex digits");
    assert_eq!(refused.code, "bad_request", "{refused:?}");
    assert!(fake.tab_open_calls().is_empty(), "nothing reached roost");
    let unknown = hosts
        .craze_open_terminal(LOCALHOST, HOST_B)
        .await
        .expect_err("a session this host does not list");
    assert_eq!(unknown.code, "unknown_session");
}

/// **Create on a DORMANT machine** (plan 025 §3.6.5): the sheet's
/// `create_options` dials the host's explicit-action dial even though no hub
/// runs (it Ensures one), and WAKES the source — which probes at once, rather
/// than at its next dormant probe an hour away, finds the hub and attaches.
#[tokio::test]
async fn create_options_on_a_dormant_machine_starts_a_hub_and_wakes_the_source() {
    let craze = Scripted::attach_only();
    let hosts = start_scripted_with(
        &config_with(&["mini3"]),
        &test_options(&[]),
        vec![(HostId::Machine("mini3".to_string()), Arc::clone(&craze))],
        CrazeTimings {
            dormant: Duration::from_secs(3600),
            ..timings()
        },
    );
    let probe = craze.probe.as_ref().unwrap();
    probe.answer(Probe::NoHub);
    wait_for("dormant", || {
        (status_of(&hosts, "mini3")?["craze"]["state"] == "dormant").then_some(())
    })
    .await;
    assert_eq!(craze.hook.dials(), 0);
    let calls = probe.calls.load(Ordering::SeqCst);

    let hosts = Arc::new(hosts);
    let asking = {
        let hosts = Arc::clone(&hosts);
        tokio::spawn(async move { hosts.craze_create_options("mini3").await })
    };
    let mut hub = craze.next().await;
    hub.hello(EPOCH, full_hub_capabilities()).await;
    let req = hub.expect("sessions.createOptions").await;
    hub.reply(
        &req,
        json!({"providers": [{"id": "grok", "label": "grok", "state": "ready"}],
               "defaultProvider": "grok", "recentDirs": [{"dir": "/work"}]}),
    )
    .await;
    let options = asking
        .await
        .unwrap()
        .expect("options from a dormant machine");
    assert_eq!(options["options"]["providers"][0]["id"], "grok");
    assert_eq!(options["options"]["recent_dirs"], json!(["/work"]));

    // Woken: the source probes again now, not in an hour.
    wait_for("the woken probe", || {
        (probe.calls.load(Ordering::SeqCst) > calls).then_some(())
    })
    .await;
    probe.answer(Probe::Hub);
    let _roster = craze.roster(json!([row(HOST_A, "ses-a")])).await;
    wait_for("live", || {
        (status_of(&hosts, "mini3")?["craze"]["state"] == "live").then_some(())
    })
    .await;
}

/// **Too old for the sheet** (plan 025 §3.8): a live hub whose `hello` lacks
/// `createOptions` (or `sessionCreate`) is refused `too_old` — "update craze on
/// this machine" — without a dial; so is a machine whose probe read v0.0.1.
#[tokio::test]
async fn a_hub_too_old_to_create_is_refused_without_a_dial() {
    let craze = Scripted::eager();
    let hosts = start_scripted(
        &ShedConfig::default(),
        &test_options(&[]),
        vec![(local(), Arc::clone(&craze))],
    );
    let mut hub = craze.next().await;
    let mut caps = full_hub_capabilities();
    caps["createOptions"] = json!(false);
    caps["sessionCreate"] = json!(false);
    hub.hello(EPOCH, caps).await;
    hub.subscribed("sub-1", EPOCH, json!([])).await;
    wait_for("live", || {
        (status_of(&hosts, LOCALHOST)?["craze"]["state"] == "live").then_some(())
    })
    .await;
    let dials = craze.hook.dials();
    let e = hosts.craze_create_options(LOCALHOST).await.unwrap_err();
    assert_eq!(e.code, "too_old", "{e:?}");
    assert!(
        e.message.starts_with("update craze on this machine"),
        "{e:?}"
    );
    let e = hosts
        .craze_create(LOCALHOST, create_req("shed-req-old"), false)
        .await
        .unwrap_err();
    assert_eq!(e.code, "too_old", "{e:?}");
    assert_eq!(craze.hook.dials(), dials, "refused before any dial");
}

/// **A held create gets no definite answer but craze's** (plan 026 §3.1): a
/// `craze.create` REPLAYING a kept request id whose host's craze is offline
/// (not installed) or too old, or whose host is no longer registered, answers
/// `outcome_unknown` — refused here before any dial, and without the
/// definite code a fresh create gets — so the sheet keeps the id; and one
/// whose dial never reaches craze is `outcome_unknown` too (the source's
/// `create_held`), where a fresh one is a quiet `unavailable`.
#[tokio::test]
async fn a_held_create_on_an_offline_host_answers_outcome_unknown() {
    // Not installed: every dial says so, the roster's and any create's.
    let craze = Scripted::eager();
    for _ in 0..10_000 {
        craze
            .dial
            .fail_next(DialError::NotInstalled("craze: command not found".into()));
    }
    let hosts = start_scripted(
        &ShedConfig::default(),
        &test_options(&[]),
        vec![(local(), Arc::clone(&craze))],
    );
    // A craze-only localhost is unlisted until a craze Ready: read its state.
    wait_for("craze not installed", || {
        let state = lock(&hosts.state);
        (state.get(&local())?.craze.status()["cause"] == "not_installed").then_some(())
    })
    .await;
    let dials = craze.hook.dials();
    let held = hosts
        .craze_create(LOCALHOST, create_req("shed-held-1"), true)
        .await
        .unwrap_err();
    assert_eq!(held.code, "outcome_unknown", "{held:?}");
    assert!(held.message.starts_with("outcome unknown: "), "{held:?}");
    let fresh = hosts
        .craze_create(LOCALHOST, create_req("shed-fresh-1"), false)
        .await
        .unwrap_err();
    assert_eq!(fresh.code, "not_installed", "{fresh:?}");
    // A host no longer registered: unknown when held, a bad request when not.
    let gone = hosts
        .craze_create("gone-machine", create_req("shed-held-2"), true)
        .await
        .unwrap_err();
    assert_eq!(gone.code, "outcome_unknown", "{gone:?}");
    let gone = hosts
        .craze_create("gone-machine", create_req("shed-fresh-2"), false)
        .await
        .unwrap_err();
    assert_eq!(gone.code, "bad_request", "{gone:?}");
    // An address that names no machine at all is a bad request either way.
    let unparsable = hosts
        .craze_create("roost:no-server", create_req("shed-held-3"), true)
        .await
        .unwrap_err();
    assert_eq!(unparsable.code, "bad_request", "{unparsable:?}");
    assert!(
        craze.hook.dials() - dials <= 1,
        "refused before any dial (a roster redial aside)"
    );

    // Too old: a live hub that cannot create.
    let craze = Scripted::eager();
    let hosts = start_scripted(
        &ShedConfig::default(),
        &test_options(&[]),
        vec![(local(), Arc::clone(&craze))],
    );
    let mut hub = craze.next().await;
    let mut caps = full_hub_capabilities();
    caps["sessionCreate"] = json!(false);
    hub.hello(EPOCH, caps).await;
    hub.subscribed("sub-1", EPOCH, json!([])).await;
    wait_for("live", || {
        (status_of(&hosts, LOCALHOST)?["craze"]["state"] == "live").then_some(())
    })
    .await;
    let dials = craze.hook.dials();
    let held = hosts
        .craze_create(LOCALHOST, create_req("shed-held-4"), true)
        .await
        .unwrap_err();
    assert_eq!(held.code, "outcome_unknown", "{held:?}");
    let fresh = hosts
        .craze_create(LOCALHOST, create_req("shed-fresh-4"), false)
        .await
        .unwrap_err();
    assert_eq!(fresh.code, "too_old", "{fresh:?}");
    assert_eq!(craze.hook.dials(), dials, "refused before any dial");

    // Live and able, and the create's dial never reaches craze.
    let craze = Scripted::eager();
    let hosts = start_scripted(
        &ShedConfig::default(),
        &test_options(&[]),
        vec![(local(), Arc::clone(&craze))],
    );
    let _roster = craze.roster(json!([])).await;
    wait_for("live", || {
        (status_of(&hosts, LOCALHOST)?["craze"]["state"] == "live").then_some(())
    })
    .await;
    for _ in 0..3 {
        craze
            .dial
            .fail_next(DialError::Unreachable("ssh: connection refused".into()));
    }
    let held = hosts
        .craze_create(LOCALHOST, create_req("shed-held-5"), true)
        .await
        .unwrap_err();
    assert_eq!(held.code, "outcome_unknown", "{held:?}");
    let fresh = hosts
        .craze_create(LOCALHOST, create_req("shed-fresh-5"), false)
        .await
        .unwrap_err();
    assert_eq!(fresh.code, "unavailable", "{fresh:?}");
}

/// **A create answered ok with a result shed cannot read is
/// `outcome_unknown`** (plan 026 AM1): craze may have started the session, so
/// the sheet must keep its id — a definite `failed` here would end it while
/// that session runs. Both attempts read nothing (the source's one retry), and
/// nothing is listed.
#[tokio::test]
async fn an_unreadable_create_answer_is_outcome_unknown() {
    let craze = Scripted::eager();
    let hosts = Arc::new(start_scripted(
        &ShedConfig::default(),
        &test_options(&[]),
        vec![(local(), Arc::clone(&craze))],
    ));
    let _roster = craze.roster(json!([])).await;
    wait_for("live", || {
        (status_of(&hosts, LOCALHOST)?["craze"]["state"] == "live").then_some(())
    })
    .await;
    let create = {
        let hosts = Arc::clone(&hosts);
        tokio::spawn(async move {
            hosts
                .craze_create(LOCALHOST, create_req("shed-unreadable"), false)
                .await
        })
    };
    for _ in 0..2 {
        let mut hub = craze.next().await;
        hub.hello(EPOCH, full_hub_capabilities()).await;
        let req = hub.expect("session.create").await;
        assert_eq!(req["params"]["requestId"], "shed-unreadable");
        hub.reply(
            &req,
            json!({"session": {"hostId": HOST_NEW}, "prompt": "accepted"}),
        )
        .await;
    }
    let e = create.await.unwrap().unwrap_err();
    assert_eq!(e.code, "outcome_unknown", "{e:?}");
    assert!(
        !craze_rows(&hosts).iter().any(|r| r["slug"] == HOST_NEW),
        "nothing listed from an answer shed cannot read"
    );
}

/// **An opened tab attaches to ITS session** even when another row claims the
/// same provider session (a re-hosted session briefly listed twice — or a fake
/// agent that reuses one id): the app ran `craze attach --session <hostId>`,
/// so it attaches by hostId rather than letting the shared rule's tie-break
/// hand it to the newer row.
#[tokio::test]
async fn an_opened_tab_attaches_to_its_own_row_when_a_provider_session_is_shared() {
    let fake = FakeRoost::start().await;
    let craze = Scripted::eager();
    let hosts = start_scripted(
        &ShedConfig::default(),
        &test_options(&[(LOCALHOST, fake.socket_path())]),
        vec![(local(), Arc::clone(&craze))],
    );
    let older = row(HOST_A, "ses-shared");
    let mut newer = row(HOST_B, "ses-shared");
    newer["row"]["since"] = json!("2026-06-01T00:00:00Z");
    let _roster = craze.roster(json!([older, newer])).await;
    wait_for("both rows", || {
        (craze_rows(&hosts).len() == 2).then_some(())
    })
    .await;
    let opened = hosts.craze_open_terminal(LOCALHOST, HOST_A).await.unwrap();
    let rows = craze_rows(&hosts);
    let a = rows.iter().find(|r| r["slug"] == HOST_A).unwrap();
    let b = rows.iter().find(|r| r["slug"] == HOST_B).unwrap();
    assert_eq!(a["tab_id"], opened["tab_id"], "the tab is A's: {rows:?}");
    assert!(b.get("tab_id").is_none(), "never the newer row's: {rows:?}");
}

/// A craze TUI's own tab and an Open-in-terminal tab on the same session: the
/// shared rule's tie-break decides — the NEWER tab attaches, the other folds
/// silently (neither is a roost row).
#[tokio::test]
async fn a_tui_tab_and_an_attach_tab_of_one_session_newest_attaches() {
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
    let _roster = craze.roster(json!([row(HOST_A, "ses-a")])).await;
    wait_for("the TUI tab folded into the row", || {
        let rows = rows_of(&hosts);
        let tab = rows
            .first()
            .and_then(|r| r["tab_id"].as_str()?.parse::<i64>().ok());
        (rows.len() == 1 && tab == Some(TAB)).then_some(())
    })
    .await;
    let opened = hosts.craze_open_terminal(LOCALHOST, HOST_A).await.unwrap();
    let rows = rows_of(&hosts);
    assert_eq!(rows.len(), 1, "one row, no roost row: {rows:?}");
    assert_eq!(
        rows[0]["tab_id"], opened["tab_id"],
        "the newer (attach) tab"
    );
}

/// A snapshot's identity, as [`OpenedTab::retained`] reads it.
fn snapshot_of(revision: Option<u64>, daemon: &str) -> RoostInventory {
    let mut at = RoostInventory::default();
    at.revision = revision;
    at.daemon_session_id = daemon.to_string();
    at.started_at = "2026-10-06T00:00:00Z".to_string();
    at
}

/// The fence of `daemon` at `revision`.
fn fence_of(revision: u64, daemon: &str) -> TabFence {
    TabFence {
        revision,
        daemon_session_id: daemon.to_string(),
        started_at: "2026-10-06T00:00:00Z".to_string(),
    }
}

/// **An opened tab is judged by its FENCE, within its daemon** (C10 review and
/// confirmation): a snapshot that lists it keeps it; one of the SAME daemon
/// taken before the open (its revision below the fence) is silent about it
/// and says nothing; any other of that daemon that does not list it says it
/// is gone; and a snapshot from ANOTHER incarnation — whose revisions restart
/// from 1 — is judged by presence alone.
#[test]
fn an_opened_tab_is_judged_by_its_fence_within_its_daemon() {
    let now = Instant::now();
    let fenced = OpenedTab {
        host_id: HOST_A.to_string(),
        fence: Some(fence_of(100, "d1")),
        opened: now,
    };
    assert!(
        fenced.retained(true, &snapshot_of(Some(3), "d2"), now),
        "listed: kept"
    );
    assert!(
        fenced.retained(false, &snapshot_of(Some(99), "d1"), now),
        "the same daemon, before the open"
    );
    assert!(
        !fenced.retained(false, &snapshot_of(Some(100), "d1"), now),
        "the same daemon, at the fence, unlisted: gone"
    );
    assert!(!fenced.retained(false, &snapshot_of(Some(150), "d1"), now));
    assert!(
        !fenced.retained(false, &snapshot_of(Some(2), "d2"), now),
        "a restarted daemon's revision 2 is not 'before' revision 100: absent is gone"
    );
    assert!(
        !fenced.retained(false, &snapshot_of(None, "d1"), now),
        "no revision: at its word"
    );
}

/// **An UNFENCED tab gets a grace** (C10 confirmation): with no fence a
/// snapshot queued before the open cannot be told from one after a close, so
/// for [`UNFENCED_GRACE`] no snapshot removes it; after it, presence decides.
#[test]
fn an_unfenced_tab_outlasts_snapshots_for_its_grace_then_presence_decides() {
    let opened = Instant::now();
    let unfenced = OpenedTab {
        host_id: HOST_A.to_string(),
        fence: None,
        opened,
    };
    let at = snapshot_of(Some(7), "d1");
    assert!(
        unfenced.retained(false, &at, opened),
        "within the grace: kept"
    );
    assert!(unfenced.retained(
        false,
        &at,
        opened + UNFENCED_GRACE - Duration::from_millis(1)
    ));
    assert!(
        !unfenced.retained(false, &at, opened + UNFENCED_GRACE),
        "past the grace, absent: gone"
    );
    assert!(
        unfenced.retained(true, &at, opened + UNFENCED_GRACE * 10),
        "listed: kept"
    );
}

/// **A roost restart does not keep a closed tab mapped** (C10 confirmation):
/// a tab opened at a fence well past 1; roost restarts (a new daemon, its
/// revision back to 1, tab ids kept); the tab closes under the new daemon,
/// whose snapshot's small revision is NOT "before the open" — it is another
/// incarnation, so absence removes the tab.
#[tokio::test]
async fn a_restarted_roost_judges_an_opened_tab_by_presence() {
    let fake = FakeRoost::start().await;
    for _ in 0..5 {
        fake.bump_revision();
    }
    let craze = Scripted::eager();
    let hosts = start_scripted(
        &ShedConfig::default(),
        &test_options(&[(LOCALHOST, fake.socket_path())]),
        vec![(local(), Arc::clone(&craze))],
    );
    let _roster = craze.roster(json!([row(HOST_A, "ses-a")])).await;
    wait_for("the hub row", || {
        (craze_rows(&hosts).len() == 1).then_some(())
    })
    .await;
    let opened = hosts.craze_open_terminal(LOCALHOST, HOST_A).await.unwrap();
    let tab: i64 = opened["tab_id"].as_str().unwrap().parse().unwrap();
    let fence = lock(&hosts.state)
        .get(&local())
        .and_then(|m| m.opened_tabs.get(&tab).and_then(|o| o.fence.clone()))
        .expect("fenced");
    assert!(
        fence.revision > 3,
        "a fence past a restarted daemon's first revisions"
    );

    let before = fake.session_id();
    fake.restart();
    wait_for("the watcher on the new daemon", || {
        let resynced = fake.session_id() != before && fake.stream_count() > 0;
        resynced.then_some(())
    })
    .await;
    // Still listed by the new daemon (tab ids persist): kept.
    tokio::time::sleep(Duration::from_millis(50)).await;
    assert_eq!(craze_rows(&hosts)[0]["tab_id"], tab.to_string());

    let reach = LocalSession::new(LOCALHOST, fake.socket_path());
    tab_close(&reach, tab)
        .await
        .expect("closed under the new daemon");
    assert!(
        fake.revision() < fence.revision,
        "the new daemon counts below the fence"
    );
    wait_for("the closed tab to leave the map", || {
        (!lock(&hosts.state)
            .get(&local())
            .is_some_and(|m| m.opened_tabs.contains_key(&tab)))
        .then_some(())
    })
    .await;
    assert!(craze_rows(&hosts)[0].get("tab_id").is_none());
}

/// **A tab closed before any snapshot listed it leaves the map** (C10
/// review): a fresh attach tab is a hidden one — its open publishes no
/// snapshot — and it is closed right away; the close's snapshot (roost
/// publishes one for a tab the watcher knew) is past its fence, and the row
/// is headless again, for good.
#[tokio::test]
async fn a_tab_closed_before_any_snapshot_listed_it_leaves_the_map() {
    let fake = FakeRoost::start().await;
    let craze = Scripted::eager();
    let hosts = start_scripted(
        &ShedConfig::default(),
        &test_options(&[(LOCALHOST, fake.socket_path())]),
        vec![(local(), Arc::clone(&craze))],
    );
    let _roster = craze.roster(json!([row(HOST_A, "ses-a")])).await;
    wait_for("the hub row", || {
        (craze_rows(&hosts).len() == 1).then_some(())
    })
    .await;
    let lists = fake.tab_list_calls();
    let opened = hosts.craze_open_terminal(LOCALHOST, HOST_A).await.unwrap();
    let tab: i64 = opened["tab_id"].as_str().unwrap().parse().unwrap();
    let fence = lock(&hosts.state)
        .get(&local())
        .and_then(|m| m.opened_tabs.get(&tab).and_then(|o| o.fence.clone()));
    assert!(fence.is_some(), "the open read its fence");
    assert_eq!(
        fake.tab_list_calls(),
        lists + 1,
        "the one tab.list is the fence read: no resync listed the tab"
    );
    let reach = LocalSession::new(LOCALHOST, fake.socket_path());
    tab_close(&reach, tab).await.expect("closed on roost");
    wait_for("the closed tab to leave the map", || {
        (!lock(&hosts.state)
            .get(&local())
            .is_some_and(|m| m.opened_tabs.contains_key(&tab)))
        .then_some(())
    })
    .await;
    let row = craze_rows(&hosts).pop().unwrap();
    assert!(row.get("tab_id").is_none(), "headless again: {row}");
}

/// **While the hub feed is down, an attach tab stays on its row** (C10
/// review): the opened-tab map is this app's own knowledge, so the retained
/// (stale) row keeps showing its still-open tab — and so offers End tab, not a
/// second Open in terminal — while D4 still absorbs nothing by roost
/// ownership.
#[tokio::test]
async fn an_attach_tab_stays_on_its_row_while_the_feed_is_down() {
    let fake = FakeRoost::start().await;
    fake.set_tab_axes(
        TAB,
        "working",
        Some(ownership("craze", "ses-b", "", 1_700_000_100)),
        false,
    );
    let craze = Scripted::eager();
    let hosts = start_scripted(
        &ShedConfig::default(),
        &test_options(&[(LOCALHOST, fake.socket_path())]),
        vec![(local(), Arc::clone(&craze))],
    );
    let hub = craze
        .roster(json!([row(HOST_A, "ses-a"), row(HOST_B, "ses-b")]))
        .await;
    wait_for("both hub rows, B absorbing its TUI tab", || {
        let rows = rows_of(&hosts);
        (rows.len() == 2 && rows.iter().all(|r| r["source"] == "craze")).then_some(())
    })
    .await;
    let opened = hosts.craze_open_terminal(LOCALHOST, HOST_A).await.unwrap();

    craze.hook.hold_dials();
    hub.close().await;
    wait_for("the feed down", || {
        (status_of(&hosts, LOCALHOST)?["craze"]["state"] == "offline").then_some(())
    })
    .await;
    let rows = rows_of(&hosts);
    let a = rows
        .iter()
        .find(|r| r["source"] == "craze" && r["slug"] == HOST_A)
        .expect("A's retained row");
    assert_eq!(a["stale"], true);
    assert_eq!(
        a["tab_id"], opened["tab_id"],
        "the attach tab stays on its stale row: {rows:?}"
    );
    // D4 unchanged: B's roost-owned TUI tab is roost's row again, not B's.
    assert!(
        rows.iter().any(|r| r["source"] == "roost"
            && r["slug"].as_str().and_then(|t| t.parse::<i64>().ok()) == Some(TAB)),
        "{rows:?}"
    );
    let b = rows
        .iter()
        .find(|r| r["source"] == "craze" && r["slug"] == HOST_B)
        .unwrap();
    assert!(b.get("tab_id").is_none(), "{b}");
}

/// One scripted create, answered with `result`, on `craze`'s next connection.
async fn create_answering(
    hosts: &Arc<RoostHosts>,
    craze: &Scripted,
    request_id: &str,
    result: serde_json::Value,
) -> Result<Value, CrazeFailure> {
    let create = {
        let hosts = Arc::clone(hosts);
        let request_id = request_id.to_string();
        tokio::spawn(async move {
            hosts
                .craze_create(LOCALHOST, create_req(&request_id), false)
                .await
        })
    };
    let mut hub = craze.next().await;
    hub.hello(EPOCH, full_hub_capabilities()).await;
    let req = hub.expect("session.create").await;
    hub.reply(&req, result).await;
    create.await.unwrap()
}

/// **The source is the one authority on created rows** (C10 confirmation): a
/// create craze answered for a session a roster has already let go — a
/// REPLAY of its first create's answer, within craze's ten-minute window — is
/// refused by the source, and so the desktop does not list it either (it keeps
/// no created rows of its own): the answer says `ended`, nothing is listed, and
/// no lane opens on it.
#[tokio::test]
async fn a_replayed_create_of_an_ended_session_is_not_listed() {
    let craze = Scripted::eager();
    let hosts = Arc::new(start_scripted(
        &ShedConfig::default(),
        &test_options(&[]),
        vec![(local(), Arc::clone(&craze))],
    ));
    let lanes = lanes_over(&hosts);
    let mut roster = craze.roster(json!([row(HOST_A, "ses-a")])).await;
    wait_for("the roster", || {
        (craze_rows(&hosts).len() == 1).then_some(())
    })
    .await;

    // The first create: listed at once (the source's created row).
    let first = create_answering(&hosts, &craze, "shed-replay-1", created_result("ses-new"))
        .await
        .expect("created");
    assert_eq!(first["ended"], false);
    assert!(craze_rows(&hosts).iter().any(|r| r["slug"] == HOST_NEW));

    // The roster lists it, then removes it: the session ended.
    roster
        .roster("sub-1", EPOCH, json!([row(HOST_NEW, "ses-new")]), json!([]))
        .await;
    roster
        .roster("sub-1", EPOCH, json!([]), json!([HOST_NEW]))
        .await;
    wait_for("the ended session gone", || {
        (!craze_rows(&hosts).iter().any(|r| r["slug"] == HOST_NEW)).then_some(())
    })
    .await;

    // craze replays the first create's answer under the same request id.
    let replay = create_answering(&hosts, &craze, "shed-replay-1", created_result("ses-new"))
        .await
        .expect("craze answered");
    assert_eq!(replay["ended"], true, "{replay}");
    assert!(
        !craze_rows(&hosts).iter().any(|r| r["slug"] == HOST_NEW),
        "a replay never resurrects an ended session's row"
    );
    let refused = lanes.open(LOCALHOST, "craze", HOST_NEW).await;
    assert!(refused.is_err(), "no lane on it: {refused:?}");
}

/// **The desktop's created rows ARE the source's** (C10 confirmation): listed
/// at read time from [`shed_craze::CrazeSource::created_rows`], so they follow
/// the source's bound — 64, the oldest out first — with no roster frame and
/// no `Ready` in between.
#[tokio::test]
async fn the_desktops_created_rows_follow_the_sources_bound() {
    let craze = Scripted::eager();
    let hosts = Arc::new(start_scripted(
        &ShedConfig::default(),
        &test_options(&[]),
        vec![(local(), Arc::clone(&craze))],
    ));
    let _roster = craze.roster(json!([])).await;
    wait_for("live", || {
        (status_of(&hosts, LOCALHOST)?["craze"]["state"] == "live").then_some(())
    })
    .await;
    let keep = shed_craze::source::CREATED_KEEP;
    for i in 0..=keep {
        let host_id = format!("{i:012x}");
        let result = json!({"session": roster_row(&host_id, &format!("s-{i}"), "/work/new",
                                                  json!({"activity": "idle"})),
                            "prompt": "none"});
        create_answering(&hosts, &craze, &format!("shed-bound-{i}"), result)
            .await
            .expect("created");
    }
    let rows = craze_rows(&hosts);
    assert_eq!(rows.len(), keep, "the source's bound, read at listing time");
    assert!(
        !rows.iter().any(|r| r["slug"] == format!("{:012x}", 0)),
        "the oldest went first"
    );
    assert_eq!(
        status_of(&hosts, LOCALHOST).unwrap()["sessions"],
        json!(keep),
        "the status count agrees"
    );
}

/// The layer with `reaches` as each host's scripted craze, counting every
/// `on_change` (the UI's "re-read") in `changes`.
fn start_counting(
    options: &ReachOptions,
    reaches: Vec<(HostId, Arc<Scripted>)>,
    changes: Arc<AtomicUsize>,
) -> RoostHosts {
    start_counting_on(&ShedConfig::default(), options, reaches, changes)
}

/// [`start_counting`], under `config` (a server whose sheds can be observed).
fn start_counting_on(
    config: &ShedConfig,
    options: &ReachOptions,
    reaches: Vec<(HostId, Arc<Scripted>)>,
    changes: Arc<AtomicUsize>,
) -> RoostHosts {
    let map: BTreeMap<HostId, Arc<Scripted>> = reaches.into_iter().collect();
    RoostHosts::start_with(
        &tokio::runtime::Handle::current(),
        config,
        options.clone(),
        false,
        Arc::new(move || {
            changes.fetch_add(1, Ordering::SeqCst);
        }),
        CrazeReaches::Fixed(Arc::new(move |id: &HostId| map.get(id).map(|s| s.reach()))),
        timings(),
    )
}

/// Whether the listing carries a craze row for `host_id`.
fn lists(hosts: &RoostHosts, host_id: &str) -> bool {
    craze_rows(hosts).iter().any(|r| r["slug"] == host_id)
}

/// **A session created and stopped before any roster lists it leaves the
/// listing** (live leg 1's ghost row, re-run on 82eabbd: a sheet-created
/// session stopped 0.4 s after its create stayed in `rc.list` — and
/// `lane.open` on it still answered — for more than eight minutes). The
/// roster never lists it, so no `Removed` will ever come; its lane's
/// `Down{"session_closed"}` lets the source's created row go, and the listing,
/// which reads the source's created rows at every read and keeps none of its
/// own, drops it — `rc.list`, the machine's count and `lane.open` alike. The
/// stop's receipt alone drops nothing, and the UI is told to re-read
/// (`on_change`) when the row goes, since no roster frame will say so.
#[tokio::test]
async fn a_created_session_stopped_before_any_roster_lists_it_leaves_the_listing() {
    use shed_craze::testing::{
        attach_result, host_session_row, session_caps, session_info, snapshot_at,
    };
    // A quiet roost on this machine: nothing re-reads but what the test does.
    let fake = FakeRoost::start().await;
    let craze = Scripted::eager();
    let changes = Arc::new(AtomicUsize::new(0));
    let hosts = Arc::new(start_counting(
        &test_options(&[(LOCALHOST, fake.socket_path())]),
        vec![(local(), Arc::clone(&craze))],
        Arc::clone(&changes),
    ));
    let lanes = lanes_over(&hosts);
    let _roster = craze.roster(json!([row(HOST_A, "ses-a")])).await;
    wait_for("the roster", || {
        (craze_rows(&hosts).len() == 1).then_some(())
    })
    .await;
    create_answering(&hosts, &craze, "shed-ghost-1", created_result("ses-new"))
        .await
        .expect("created");
    assert!(lists(&hosts, HOST_NEW), "listed at once, by the source");

    // Its transcript opens, and the lane's own connection seeds.
    lanes
        .open(LOCALHOST, "craze", HOST_NEW)
        .await
        .expect("the transcript of a just-created session opens");
    let mut conn = craze.next().await;
    conn.splice(HOST_NEW).await;
    let info = session_info(HOST_NEW, "craze-new", "INC-1", session_caps(true));
    conn.listed(host_session_row(&info, json!({}))).await;
    conn.attached(attach_result(
        "s-1",
        &info,
        ("INC-1", 1),
        Some(snapshot_at("INC-1", 1, json!({}))),
        None,
    ))
    .await;
    conn.synchronized("s-1", 1).await;
    wait_for("the seed", || {
        let v = lanes.messages(LOCALHOST, "craze", HOST_NEW).ok()?;
        (!v["session"].is_null()).then_some(())
    })
    .await;

    // Stop: answered on the receipt — the row stays while the session closes.
    let stopping = {
        let lanes = Arc::clone(&lanes);
        tokio::spawn(async move { lanes.stop(LOCALHOST, "craze", HOST_NEW).await })
    };
    let req = conn.expect("session.stop").await;
    conn.reply(&req, json!({})).await;
    stopping.await.unwrap().expect("the host's receipt");
    assert!(lists(&hosts, HOST_NEW), "closing, not closed");
    let before = changes.load(Ordering::SeqCst);

    // The close: the row leaves the listing, though no roster frame says so —
    // and the UI was told to re-read before the lane's `Down` reached it.
    conn.reset("s-1", "session_closed").await;
    wait_for("the lane's Down", || {
        let v = lanes.messages(LOCALHOST, "craze", HOST_NEW).ok()?;
        (v["ended"] == true).then_some(())
    })
    .await;
    assert!(
        !lists(&hosts, HOST_NEW),
        "the stopped session is gone from the listing"
    );
    assert!(
        changes.load(Ordering::SeqCst) > before,
        "the UI is told to re-read: no roster frame will"
    );
    assert!(lists(&hosts, HOST_A), "the roster's own row stays");
    assert_eq!(
        status_of(&hosts, LOCALHOST).unwrap()["sessions"],
        json!(1),
        "the machine's count agrees"
    );
    // `lane.open` on it answers the readable ENDED lane (plan 032 §3.1.1 item
    // 9) — never a fresh one: its row is gone. And once the panel closes,
    // nothing opens on it at all.
    lanes
        .open(LOCALHOST, "craze", HOST_NEW)
        .await
        .expect("the ended lane, readable");
    assert_eq!(
        lanes
            .messages(LOCALHOST, "craze", HOST_NEW)
            .expect("readable")["finished"],
        true
    );
    lanes.close(LOCALHOST, "craze", HOST_NEW);
    let refused = lanes.open(LOCALHOST, "craze", HOST_NEW).await;
    assert!(refused.is_err(), "no lane opens on it: {refused:?}");
}

/// **A create answered as the roster lets its session go is not listed
/// again** (the ghost-row review of `craze_create`): the roster lists the new
/// session, and removes it while the create's answer is between the source
/// and this layer's state lock. The source's rows are read UNDER that lock,
/// so the roster row the answer folds in is never one whose `Removed` this
/// state already applied — read before it, `listed_now` folded the row back,
/// and no later frame would ever remove it.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn a_create_answered_as_the_roster_lets_it_go_is_not_listed_again() {
    let craze = Scripted::eager();
    let hosts = Arc::new(start_scripted(
        &ShedConfig::default(),
        &test_options(&[]),
        vec![(local(), Arc::clone(&craze))],
    ));
    let mut roster = craze.roster(json!([row(HOST_A, "ses-a")])).await;
    wait_for("the roster", || {
        (craze_rows(&hosts).len() == 1).then_some(())
    })
    .await;
    // The roster lists the new session before its create is answered.
    roster
        .roster("sub-1", EPOCH, json!([row(HOST_NEW, "ses-new")]), json!([]))
        .await;
    wait_for("the roster's row", || lists(&hosts, HOST_NEW).then_some(())).await;

    let reached = Arc::new(tokio::sync::Notify::new());
    let proceed = Arc::new(tokio::sync::Notify::new());
    *lock(&hosts.create_gap) = Some((Arc::clone(&reached), Arc::clone(&proceed)));
    let create = {
        let hosts = Arc::clone(&hosts);
        tokio::spawn(async move {
            hosts
                .craze_create(LOCALHOST, create_req("shed-race-1"), false)
                .await
        })
    };
    let mut hub = craze.next().await;
    hub.hello(EPOCH, full_hub_capabilities()).await;
    let req = hub.expect("session.create").await;
    hub.reply(&req, created_result("ses-new")).await;
    reached.notified().await;
    // The session ends while the answer is in hand: the roster lets it go,
    // and this layer applies the `Removed`.
    roster
        .roster("sub-1", EPOCH, json!([]), json!([HOST_NEW]))
        .await;
    wait_for("the Removed applied", || {
        (!lists(&hosts, HOST_NEW)).then_some(())
    })
    .await;
    proceed.notify_one();
    let answer = create.await.unwrap().expect("craze answered");
    assert!(
        !lists(&hosts, HOST_NEW),
        "a row the roster let go is not folded back"
    );
    assert_eq!(answer["ended"], true, "{answer}");
}

// ---------------------------------------------------------------------------
// offer, don't list (plan 026 §3.2, shed#399)
// ---------------------------------------------------------------------------

/// The shed every cell below observes running.
const SHED: &str = "roost:srv/s1";

/// `rc.list`'s `shed_craze`, unfiltered.
fn shed_craze_of(hosts: &RoostHosts) -> Vec<Value> {
    hosts.snapshot(HostFilter::ALL).2
}

/// Answer the source's pending find-only probe with `answer`, and return once
/// it has been FOLDED: the loop folds an answer (and repaints) before it waits
/// and asks the next probe, so the next probe being asked is the proof.
async fn probe_answers(probe: &ScriptedProbe, answer: Probe) {
    wait_for("a probe pending", || {
        (probe.calls.load(Ordering::SeqCst) >= 1).then_some(())
    })
    .await;
    let asked = probe.calls.load(Ordering::SeqCst);
    probe.answer(answer);
    wait_for("the answer folded (the next probe asked)", || {
        (probe.calls.load(Ordering::SeqCst) > asked).then_some(())
    })
    .await;
}

/// The shed `s1` on `srv`, its craze attach-only and scripted, the layer
/// counting every repaint in `changes` — observed running by the
/// authoritative refresh, so its source is started and probing. It has no
/// roost (nothing is mapped), so only craze can ever list it.
async fn observed_shed(changes: &Arc<AtomicUsize>) -> (RoostHosts, Arc<Scripted>) {
    let (shed, config) = srv_shed("s1");
    let craze = Scripted::attach_only();
    let hosts = start_counting_on(
        &config,
        &test_options(&[]),
        vec![(shed, Arc::clone(&craze))],
        Arc::clone(changes),
    );
    refresh(&hosts, &["s1"]).await;
    (hosts, craze)
}

fn too_old(reason: &str) -> Probe {
    Probe::Offline {
        cause: SourceOffline::TooOld,
        reason: reason.to_string(),
    }
}

/// **A dormant shed is OFFERED, not listed** (plan 026 §3.2, shed#399): a
/// running shed whose find-only probe says no hub is running is an `rc.list`
/// `shed_craze` entry — the status row's identity fields and the same `craze`
/// object a status row carries — and NOT a status row: no sessions, not in
/// `machines.list`. The entry honours `rc.list`'s shed filter as the status
/// rows do, and nothing dialled `bridge --hub`: the probe is find-only.
#[tokio::test]
async fn a_dormant_shed_is_offered_not_listed() {
    let changes = Arc::new(AtomicUsize::new(0));
    let (hosts, craze) = observed_shed(&changes).await;
    probe_answers(craze.probe.as_ref().unwrap(), Probe::NoHub).await;

    let (sessions, status, offered) = hosts.snapshot(HostFilter::ALL);
    assert!(
        status.iter().all(|s| s["origin"] != SHED),
        "a dormant shed is not a status row: {status:?}"
    );
    assert!(sessions.is_empty(), "and lists no sessions: {sessions:?}");
    assert!(
        status_of(&hosts, SHED).is_none(),
        "nor is it in machines.list"
    );
    let gen = lock(&hosts.state)
        .get(&srv_shed("s1").0)
        .expect("state")
        .craze
        .gen;
    assert_eq!(
        offered,
        vec![
            json!({"name": SHED, "label": "s1", "origin": SHED, "kind": "shed",
                    "craze": {"state": "dormant", "cause": null,
                              "create": false, "create_options": false,
                              "create_title": false, "source_gen": gen}})
        ]
    );

    let filtered = |server: Option<&str>, shed: Option<&str>| {
        hosts.snapshot(HostFilter { server, shed }).2.len()
    };
    assert_eq!(filtered(Some("srv"), Some("s1")), 1, "its own filter");
    assert_eq!(filtered(Some("srv"), None), 1, "its server's filter");
    assert_eq!(
        filtered(Some("srv"), Some("s2")),
        0,
        "another shed's filter"
    );
    assert_eq!(filtered(Some("other"), None), 0, "another server's filter");
    assert_eq!(
        craze.hook.dials(),
        0,
        "no bridge --hub: the probe is find-only"
    );
}

/// **The entry follows the probe, and repaints once per change** (plan 026
/// §3.2): dormant → too old → dormant changes the entry's `state`/`cause`
/// and tells the UI to re-read once per change, though the shed is unlisted;
/// a repeated probe — the 30 s dormant one, or a too-old one in other words —
/// changes neither and repaints nothing. No `bridge --hub` throughout.
#[tokio::test]
async fn a_dormant_sheds_entry_follows_its_probe_and_repaints_once_per_change() {
    let changes = Arc::new(AtomicUsize::new(0));
    let (hosts, craze) = observed_shed(&changes).await;
    let probe = craze.probe.as_ref().unwrap();
    let repaints = || changes.load(Ordering::SeqCst);
    let entry = || {
        shed_craze_of(&hosts)
            .first()
            .map(|e| (e["craze"]["state"].clone(), e["craze"]["cause"].clone()))
    };
    let base = repaints();

    probe_answers(probe, Probe::NoHub).await;
    assert_eq!(entry(), Some((json!("dormant"), json!(null))));
    assert_eq!(repaints(), base + 1, "dormant: one repaint");
    probe_answers(probe, Probe::NoHub).await;
    assert_eq!(repaints(), base + 1, "a repeated dormant probe: none");

    probe_answers(probe, too_old("unknown flag: --hub")).await;
    assert_eq!(entry(), Some((json!("offline"), json!("too_old"))));
    assert_eq!(repaints(), base + 2, "too old: one repaint");
    probe_answers(probe, too_old("unknown flag: --hub")).await;
    probe_answers(probe, too_old("unknown flag: --hub, said otherwise")).await;
    assert_eq!(
        repaints(),
        base + 2,
        "a repeated too-old probe: none, whatever its words"
    );

    probe_answers(probe, Probe::NoHub).await;
    assert_eq!(entry(), Some((json!("dormant"), json!(null))));
    assert_eq!(repaints(), base + 3, "dormant again: one repaint");
    assert!(status_of(&hosts, SHED).is_none(), "never listed");
    assert_eq!(craze.hook.dials(), 0, "no bridge --hub");
}

/// **Not installed — or unreachable — drops the entry, with one repaint**
/// (plan 026 §3.2): a shed whose craze turns out not installed (exit 127)
/// says nothing, as a shed with no craze always has, and so does one the
/// probe cannot ask. No `bridge --hub`.
#[tokio::test]
async fn a_dormant_shed_that_turns_out_not_installed_leaves_shed_craze() {
    let changes = Arc::new(AtomicUsize::new(0));
    let (hosts, craze) = observed_shed(&changes).await;
    let probe = craze.probe.as_ref().unwrap();
    let repaints = || changes.load(Ordering::SeqCst);
    let not_installed = || Probe::Offline {
        cause: SourceOffline::NotInstalled,
        reason: "exit 127".to_string(),
    };
    let base = repaints();

    probe_answers(probe, Probe::NoHub).await;
    assert_eq!(shed_craze_of(&hosts).len(), 1);
    probe_answers(probe, not_installed()).await;
    assert!(shed_craze_of(&hosts).is_empty(), "not installed: no entry");
    assert_eq!(repaints(), base + 2, "one repaint for the drop");
    probe_answers(probe, not_installed()).await;
    assert_eq!(repaints(), base + 2, "a repeated probe: none");

    probe_answers(probe, Probe::NoHub).await;
    assert_eq!(shed_craze_of(&hosts).len(), 1, "dormant again");
    probe_answers(
        probe,
        Probe::Offline {
            cause: SourceOffline::Unreachable,
            reason: "ssh: connect to host: no route to host".to_string(),
        },
    )
    .await;
    assert!(shed_craze_of(&hosts).is_empty(), "unreachable: no entry");
    assert_eq!(repaints(), base + 4, "one repaint each way");
    assert!(status_of(&hosts, SHED).is_none(), "never listed");
    assert_eq!(craze.hook.dials(), 0, "no bridge --hub");
}

/// **The first `Ready` moves a dormant shed from `shed_craze` to a listed
/// row** (plan 026 §3.2): once the probe finds a hub (the sheet started one)
/// and the roster seeds, the shed is listed — a status row, with its craze
/// rows — and no longer an entry, in one answer: never both. Until the probe
/// found that hub nothing dialled `bridge --hub`; the one dial after it is
/// the roster's.
#[tokio::test]
async fn the_first_ready_moves_a_dormant_shed_from_shed_craze_to_a_listed_row() {
    let changes = Arc::new(AtomicUsize::new(0));
    let (hosts, craze) = observed_shed(&changes).await;
    let probe = craze.probe.as_ref().unwrap();
    probe_answers(probe, Probe::NoHub).await;
    assert_eq!(shed_craze_of(&hosts).len(), 1);
    assert_eq!(craze.hook.dials(), 0, "dormant: no bridge --hub");

    probe.answer(Probe::Hub);
    let _hub = craze.roster(json!([row(HOST_A, "ses-a")])).await;
    wait_for("listed by its first Ready", || status_of(&hosts, SHED)).await;
    let (sessions, status, offered) = hosts.snapshot(HostFilter::ALL);
    let row = status
        .iter()
        .find(|s| s["origin"] == SHED)
        .expect("a status row");
    assert_eq!(row["craze"]["state"], "live", "{row}");
    assert!(
        offered.is_empty(),
        "no longer an entry — never both: {offered:?}"
    );
    assert!(
        sessions
            .iter()
            .any(|r| r["origin"] == SHED && r["slug"] == HOST_A),
        "its craze rows are listed: {sessions:?}"
    );
    assert_eq!(
        craze.hook.dials(),
        1,
        "one bridge --hub: the roster's, after the probe found a hub"
    );
}

/// **A stopped (or deleted) shed leaves `shed_craze`, and the UI is told**
/// (plan 026 §3.2): the authoritative refresh that no longer lists it removes
/// the host, its entry goes with it, and the removal repaints though the shed
/// was never listed — so "Where" never offers a stopped shed. (A stop and a
/// delete are one thing here: a shed the answering server no longer lists
/// running.)
#[tokio::test]
async fn a_stopped_dormant_shed_leaves_shed_craze_and_repaints() {
    let changes = Arc::new(AtomicUsize::new(0));
    let (hosts, craze) = observed_shed(&changes).await;
    probe_answers(craze.probe.as_ref().unwrap(), Probe::NoHub).await;
    assert_eq!(shed_craze_of(&hosts).len(), 1);
    let base = changes.load(Ordering::SeqCst);

    refresh(&hosts, &[]).await;
    assert!(
        shed_craze_of(&hosts).is_empty(),
        "the entry went with the host"
    );
    assert_eq!(
        changes.load(Ordering::SeqCst),
        base + 1,
        "its removal repaints"
    );
    assert_eq!(craze.hook.dials(), 0, "no bridge --hub");
}

/// **A configured machine is never in `shed_craze`** (plan 026 §3.2): it is
/// listed from the start, so a dormant or a too-old craze there is its status
/// row's to say, as it always was.
#[tokio::test]
async fn a_configured_machine_is_never_in_shed_craze() {
    let craze = Scripted::attach_only();
    let hosts = start_scripted(
        &config_with(&["mini3"]),
        &test_options(&[]),
        vec![(HostId::Machine("mini3".to_string()), Arc::clone(&craze))],
    );
    let probe = craze.probe.as_ref().unwrap();
    probe_answers(probe, Probe::NoHub).await;
    assert_eq!(
        status_of(&hosts, "mini3").unwrap()["craze"]["state"],
        "dormant"
    );
    assert_eq!(
        status_of(&hosts, "mini3").unwrap()["roost_watched"],
        true,
        "a configured machine is watched from the start: never craze only"
    );
    assert!(
        shed_craze_of(&hosts).is_empty(),
        "dormant: its status row's"
    );
    probe_answers(probe, too_old("unknown flag: --hub")).await;
    assert_eq!(
        status_of(&hosts, "mini3").unwrap()["craze"]["cause"],
        "too_old"
    );
    assert!(
        shed_craze_of(&hosts).is_empty(),
        "too old: its status row's"
    );
    assert_eq!(craze.hook.dials(), 0, "no bridge --hub");
}

/// **A shed whose roost answered is WATCHED before its first inventory**
/// (plan 026 §3.2, the C2 review's P2): the probe's `session.identify`
/// answers and the watcher is installed, but no first inventory has arrived
/// when the shed's craze `Ready` lists it — every subscribe here acks a
/// DIFFERENT daemon incarnation than its identify, so the watcher resyncs and
/// publishes nothing (`FakeRoost::restart_between_dials`; the probe only
/// identifies, so it still answers). The status row says
/// `roost_watched: true` with `connected_once` still false, so the UI does not
/// read it as "craze only" (no roost watcher); once the daemon settles the
/// inventory lands and it is simply reachable. A shed with no roost at all,
/// listed by its craze, is `roost_watched: false`.
#[tokio::test]
async fn a_shed_whose_roost_answered_is_watched_before_its_first_inventory() {
    let fake = FakeRoost::start().await;
    fake.restart_between_dials(10_000);
    let (shed, config) = srv_shed("s1");
    let craze = Scripted::attach_only();
    let hosts = start_scripted(
        &config,
        &test_options(&[(SHED, fake.socket_path())]),
        vec![(shed.clone(), Arc::clone(&craze))],
    );
    refresh(&hosts, &["s1"]).await;
    craze.probe.as_ref().unwrap().answer(Probe::Hub);
    let _hub = craze.roster(json!([row(HOST_A, "ses-a")])).await;
    let status = wait_for("listed by its craze Ready, its roost watched", || {
        status_of(&hosts, SHED).filter(|s| s["roost_watched"] == true)
    })
    .await;
    assert_eq!(status["craze"]["state"], "live", "{status}");
    assert_eq!(
        status["connected_once"], false,
        "no inventory yet: {status}"
    );
    assert_eq!(status["reachable"], false, "{status}");

    // The daemon settles: the first inventory lands, and it is reachable.
    fake.restart_between_dials(0);
    let status = wait_for("the first inventory", || {
        status_of(&hosts, SHED).filter(|s| s["connected_once"] == true)
    })
    .await;
    assert_eq!(status["reachable"], true, "{status}");
    assert_eq!(status["roost_watched"], true, "{status}");

    // A shed with no roost at all, listed by its craze alone: unwatched.
    let (other, config) = srv_shed("s2");
    let alone = Scripted::attach_only();
    let hosts = start_scripted(
        &config,
        &test_options(&[]),
        vec![(other, Arc::clone(&alone))],
    );
    refresh(&hosts, &["s2"]).await;
    alone.probe.as_ref().unwrap().answer(Probe::Hub);
    let _hub = alone.roster(json!([row(HOST_B, "ses-b")])).await;
    let status = wait_for("listed by craze alone", || {
        status_of(&hosts, "roost:srv/s2")
    })
    .await;
    assert_eq!(
        status["roost_watched"], false,
        "no roost answered: {status}"
    );
    assert_eq!(status["connected_once"], false, "{status}");
}

// ---- plan 032 §3.4: the create sheet's Resume mode (C5) ----

/// The saved session the resume cells list and reopen (craze's recipe id).
const SAVED_ID: &str = "0192f0aa-6666-7000-8000-0000000reopen";

/// A roster connection on a 0.2.0 hub — its `hello` says `reopen` — with
/// `rows`.
async fn reopen_roster(craze: &Scripted, rows: serde_json::Value) -> HubEnd {
    let mut hub = craze.next().await;
    hub.hello(EPOCH, reopen_hub_capabilities()).await;
    hub.subscribed("sub-1", EPOCH, rows).await;
    hub
}

/// The layer on `localhost` over `craze`, its roster live on a 0.2.0 hub
/// listing [`HOST_A`]; the roster connection is handed back so it stays up.
async fn live_reopen_host(craze: &Arc<Scripted>) -> (Arc<RoostHosts>, HubEnd) {
    let hosts = Arc::new(start_scripted(
        &ShedConfig::default(),
        &test_options(&[]),
        vec![(local(), Arc::clone(craze))],
    ));
    let roster = reopen_roster(craze, json!([row(HOST_A, "ses-a")])).await;
    wait_for("the roster, live", || {
        (status_of(&hosts, LOCALHOST)?["craze"]["state"] == "live" && craze_rows(&hosts).len() == 1)
            .then_some(())
    })
    .await;
    (hosts, roster)
}

/// The next connection of `craze`'s: a 0.2.0 hub's `hello`, then the one
/// `session.reopen` it carries — for [`SAVED_ID`] — handed back.
async fn reopen_asked(craze: &Scripted) -> (HubEnd, Value) {
    let mut hub = craze.next().await;
    hub.hello(EPOCH, reopen_hub_capabilities()).await;
    let req = hub.expect("session.reopen").await;
    assert_eq!(req["params"], json!({ "id": SAVED_ID }), "{req}");
    (hub, req)
}

/// **A reopened session is listed and its transcript opens at once** (plan
/// 032 §3.4, the create path's twin): `craze.reopen` asks the hub on a
/// connection of its own, the answer's row — a hostId no roster frame names
/// yet — is listed at once, live, and `lane.open` on it succeeds straight
/// after; the answer says the session was started (`joined: false`), not
/// ended, and echoes the saved id it was asked for — and names the host's
/// CURRENT incarnation (`source_gen`, not `superseded`).
#[tokio::test]
async fn a_reopened_session_is_listed_and_openable_at_once() {
    let craze = Scripted::eager();
    let (hosts, _roster) = live_reopen_host(&craze).await;
    let lanes = lanes_over(&hosts);
    let gen = lock(&hosts.state).get(&local()).expect("state").craze.gen;

    let reopening = {
        let hosts = Arc::clone(&hosts);
        tokio::spawn(async move { hosts.craze_reopen(LOCALHOST, SAVED_ID).await })
    };
    let (mut hub, req) = reopen_asked(&craze).await;
    hub.reply(
        &req,
        json!({"session": roster_row(HOST_NEW, SAVED_ID, "/work/saved",
                                     json!({"title": "a saved session", "activity": "idle",
                                            "providerSessionId": "ses-saved"})),
               "joined": false}),
    )
    .await;
    let answer = reopening.await.unwrap().expect("reopened");
    assert_eq!(answer["host_id"], HOST_NEW);
    assert_eq!(answer["joined"], false);
    assert_eq!(answer["ended"], false);
    assert_eq!(answer["id"], SAVED_ID);
    assert_eq!(answer["session"]["slug"], HOST_NEW);
    assert_eq!(answer["session"]["stale"], false);
    assert_eq!(answer["superseded"], false);
    assert_eq!(answer["source_gen"], gen);

    let listed = craze_rows(&hosts);
    assert!(
        listed.iter().any(|r| r["slug"] == HOST_NEW),
        "listed at once: {listed:?}"
    );
    lanes
        .open(LOCALHOST, "craze", HOST_NEW)
        .await
        .expect("the transcript of a just-reopened session opens at once");
    assert!(lanes.is_open(LOCALHOST, "craze", HOST_NEW));
}

/// The reopen of [`SAVED_ID`] on `address`, answered `joined: true` with
/// host A's row — the session a host still serves — on `craze`'s next
/// connection.
async fn rejoin_a(hosts: &Arc<RoostHosts>, craze: &Scripted, address: &str) -> Value {
    let reopening = {
        let hosts = Arc::clone(hosts);
        let address = address.to_string();
        tokio::spawn(async move { hosts.craze_reopen(&address, SAVED_ID).await })
    };
    let (mut hub, req) = reopen_asked(craze).await;
    hub.reply(
        &req,
        json!({"session": row(HOST_A, "ses-a"), "joined": true}),
    )
    .await;
    let answer = reopening.await.unwrap().expect("reopened");
    assert_eq!(answer["host_id"], HOST_A);
    assert_eq!(answer["joined"], true);
    assert_eq!(answer["ended"], false, "{answer}");
    assert_eq!(answer["superseded"], false, "{answer}");
    answer
}

/// **A reopen that rejoins a departing lane's host keeps its lane** (plan 032
/// §3.1.1 item 3, branch review): a remote hub goes — the source Dormant,
/// the lane on its row departing `hub_gone` — while the session's host
/// outlives it. Resume rejoins that host (`joined: true`) through a hub the
/// reopen starts, and its listing of the answered row publishes the RETURN
/// that cancels the departure: the lane is live at once, and stays live past
/// the grace, though the source has not seeded again (its probe is held).
/// Before the fix the listing published nothing, and the grace ended the
/// panel of a session that runs before any roster caught up.
#[tokio::test]
async fn a_reopen_that_rejoins_a_departing_lanes_host_keeps_its_lane() {
    let craze = Scripted::attach_only();
    let id = HostId::Machine("mini3".to_string());
    let hosts = Arc::new(start_scripted(
        &config_with(&["mini3"]),
        &test_options(&[]),
        vec![(id.clone(), Arc::clone(&craze))],
    ));
    // A grace no step below outlasts before the reopen answers.
    let lanes = lanes_holding(&hosts, ORDER_GRACE, Arc::new(NewsHold::default()));
    let address = id.address();
    let probe = craze.probe.as_ref().unwrap();
    probe.answer(Probe::Hub);
    let roster = reopen_roster(&craze, json!([row(HOST_A, "ses-a")])).await;
    wait_for("live", || {
        (status_of(&hosts, "mini3")?["craze"]["state"] == "live").then_some(())
    })
    .await;
    lanes
        .open(&address, "craze", HOST_A)
        .await
        .expect("the craze lane");
    // The lane's own connection, left unanswered: the session's host lives.
    let _lane_conn = craze.next().await;

    // The hub goes: its roster ends, and the probe finds none.
    roster.close().await;
    probe.answer(Probe::NoHub);
    wait_for("the lane to depart", || {
        (lane_state(&lanes, &address, HOST_A) == Some("departing")).then_some(())
    })
    .await;

    // Resume rejoins the host, through the hub the reopen starts.
    rejoin_a(&hosts, &craze, &address).await;
    assert_eq!(
        lane_state(&lanes, &address, HOST_A),
        Some("live"),
        "the listing's return cancelled the departure"
    );
    tokio::time::sleep(ORDER_GRACE + Duration::from_millis(500)).await;
    assert_eq!(
        lane_state(&lanes, &address, HOST_A),
        Some("live"),
        "the cancelled grace ended nothing"
    );
    assert!(
        lock(&hosts.state)
            .get(&id)
            .is_some_and(|m| m.craze.phase == Some(CrazePhase::Dormant)),
        "and no seed did it: the source is still dormant"
    );
}

/// **A listing's delayed return never cancels a newer departure** (plan 032
/// §3.1.1 item 3, branch review — C1's first finding, for the publisher a
/// create's or reopen's listing now is): a reopen joins a session the roster
/// lists, and its listing mints the return under the state lock and pauses
/// before publishing it; the roster then drops the row — its fold's
/// departure minted after that return, and applied; THEN the return lands.
/// The lane does not take it: it stays departing, and its grace ends it
/// `session_gone`. The news fence orders the listing's return as it orders
/// every other piece of craze news.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn a_listings_delayed_return_never_cancels_a_newer_departure() {
    let hold = Arc::new(NewsHold::default());
    let craze = Scripted::eager();
    let hosts = Arc::new(start_scripted(
        &ShedConfig::default(),
        &test_options(&[]),
        vec![(local(), Arc::clone(&craze))],
    ));
    let lanes = lanes_holding(&hosts, ORDER_GRACE, Arc::clone(&hold));
    let mut roster = reopen_roster(&craze, json!([row(HOST_A, "ses-a")])).await;
    wait_for("the row", || (craze_rows(&hosts).len() == 1).then_some(())).await;
    lanes
        .open(LOCALHOST, "craze", HOST_A)
        .await
        .expect("the craze lane");
    let _lane_conn = craze.next().await;

    // The reopen joins A: its listing's return is minted, and held back.
    hold.hold_returns(true);
    rejoin_a(&hosts, &craze, LOCALHOST).await;
    assert!(
        hold.held_seq().is_some(),
        "the listing published a return, held"
    );
    hold.hold_returns(false);

    // The roster drops A: the fold's departure, minted after it, lands first.
    roster
        .roster("sub-1", EPOCH, json!([]), json!([HOST_A]))
        .await;
    lane_becomes(&lanes, "departing").await;

    // The delayed return lands.
    hold.release(&lanes);
    assert_eq!(
        lane_state(&lanes, LOCALHOST, HOST_A),
        Some("departing"),
        "a return minted before the departure never revives the lane"
    );
    lane_becomes(&lanes, "finished").await;
    assert_eq!(reason(&lanes, LOCALHOST, HOST_A), "session_gone");
}

// ---- C8 review: a no-hub answer judged against the listings since it was
// asked; a create's or reopen's listing always returns its row ----

/// `mini3`, attach-only through `craze`, with its lanes over a `grace`
/// departure grace: the layer, the lanes, and the machine's id.
fn remote_host(craze: &Arc<Scripted>, grace: Duration) -> (Arc<RoostHosts>, Arc<Lanes>, HostId) {
    let id = HostId::Machine("mini3".to_string());
    let hosts = Arc::new(start_scripted(
        &config_with(&["mini3"]),
        &test_options(&[]),
        vec![(id.clone(), Arc::clone(craze))],
    ));
    let lanes = lanes_holding(&hosts, grace, Arc::new(NewsHold::default()));
    (hosts, lanes, id)
}

/// Until `id`'s craze source is Dormant.
async fn dormant_at(hosts: &RoostHosts, id: &HostId) {
    wait_for("the source dormant", || {
        lock(&hosts.state)
            .get(id)
            .is_some_and(|m| m.craze.phase == Some(CrazePhase::Dormant))
            .then_some(())
    })
    .await;
}

/// The reopen of [`SAVED_ID`] on `address`, answered `joined: false` with
/// [`HOST_NEW`]'s row — a session no roster lists, which the source keeps as
/// a created row — on `craze`'s next connection.
async fn reopen_new(hosts: &Arc<RoostHosts>, craze: &Scripted, address: &str) {
    let reopening = {
        let hosts = Arc::clone(hosts);
        let address = address.to_string();
        tokio::spawn(async move { hosts.craze_reopen(&address, SAVED_ID).await })
    };
    let (mut hub, req) = reopen_asked(craze).await;
    hub.reply(
        &req,
        json!({"session": roster_row(HOST_NEW, SAVED_ID, "/work/saved",
                                     json!({"title": "a saved session", "activity": "idle",
                                            "providerSessionId": "ses-saved"})),
               "joined": false}),
    )
    .await;
    let answer = reopening.await.unwrap().expect("reopened");
    assert_eq!(answer["host_id"], HOST_NEW);
    assert_eq!(answer["ended"], false, "{answer}");
}

/// **A create listed after its hub went still revives its lane — the
/// accepted limitation** (C8 review): a remote create is answered, and its
/// listing is delayed; a lane opens on the created row meanwhile; the hub
/// goes — the roster ends, the probe finds none — and the Dormant departs
/// that lane. THEN the listing runs. The source still lists the row (it
/// retains its created rows), and the listing returns it as every listing
/// does: the lane is live again, and the row stays listed — openable. The
/// desktop cannot tell this Dormant from one resting on a probe that looked
/// before the create's hub started, and sides with the session. If the hub
/// stays gone, later evidence ends the lane: the next probe started after
/// the listing finds no hub (one asked before it is stale, and asked again),
/// its Dormant departs the created row again, and the grace ends the lane
/// `hub_gone`.
#[tokio::test]
async fn a_create_listed_after_its_hub_went_still_revives_its_lane() {
    let craze = Scripted::attach_only();
    let (hosts, lanes, id) = remote_host(&craze, ORDER_GRACE);
    let address = id.address();
    let probe = craze.probe.as_ref().unwrap();
    probe.answer(Probe::Hub);
    let roster = craze.roster(json!([])).await;
    wait_for("live", || {
        (status_of(&hosts, "mini3")?["craze"]["state"] == "live").then_some(())
    })
    .await;

    // The create is answered, and its listing held back.
    let (gap, reached, proceed) = test_gap();
    *lock(&hosts.create_gap) = Some(gap);
    let create = {
        let hosts = Arc::clone(&hosts);
        let address = address.clone();
        tokio::spawn(async move {
            hosts
                .craze_create(&address, create_req("shed-late-1"), false)
                .await
        })
    };
    let mut hub = craze.next().await;
    hub.hello(EPOCH, full_hub_capabilities()).await;
    let req = hub.expect("session.create").await;
    hub.reply(&req, created_result("ses-new")).await;
    reached.notified().await;
    lanes
        .open(&address, "craze", HOST_NEW)
        .await
        .expect("the created row's lane opens before its listing");
    let _lane_conn = craze.next().await;

    // The hub goes, and the Dormant departs the lane.
    roster.close().await;
    probe.answer(Probe::NoHub);
    wait_for("the lane to depart", || {
        (lane_state(&lanes, &address, HOST_NEW) == Some("departing")).then_some(())
    })
    .await;

    // The delayed listing returns the row all the same.
    proceed.notify_one();
    let answer = create.await.unwrap().expect("created");
    assert_eq!(
        answer["ended"], false,
        "the source still lists it: {answer}"
    );
    assert_eq!(
        lane_state(&lanes, &address, HOST_NEW),
        Some("live"),
        "the listing's return cancelled the departure"
    );
    assert!(
        hosts.craze_lanes(&address).contains_key(HOST_NEW),
        "and the row is listed: openable"
    );

    // The hub stays gone: a probe from after the listing finds none.
    probe.answer(Probe::NoHub);
    probe.answer(Probe::NoHub);
    wait_for("the lane to depart again", || {
        (lane_state(&lanes, &address, HOST_NEW) == Some("departing")).then_some(())
    })
    .await;
    wait_for("the grace to end the lane", || {
        (lane_state(&lanes, &address, HOST_NEW) == Some("finished")).then_some(())
    })
    .await;
    assert_eq!(reason(&lanes, &address, HOST_NEW), "hub_gone");
}

/// **A reopen answered after its hub went still revives its lane, and its
/// row stays openable** (C8 review; c8b review finding 1): Resume rejoins
/// host A's session while A's lane is live, and the hub goes while the
/// reopen is in flight — the Dormant departs the lane and drops A from the
/// listing — before craze's answer arrives. The answer's listing returns A
/// all the same and folds the roster's row (the shared source still holds
/// it) back in ahead of its frame: the lane is live at once, A is listed
/// again, and a panel that closes the lane and opens it again resolves the
/// row — no seed needed, the source still Dormant. With C8's dormancy epoch
/// the listing returned nothing and folded nothing ahead: the lane waited on
/// the next seed, and `lane.open` on A failed until one came.
#[tokio::test]
async fn a_reopen_answered_after_its_hub_went_revives_its_lane_and_stays_openable() {
    let craze = Scripted::attach_only();
    let (hosts, lanes, id) = remote_host(&craze, ORDER_GRACE);
    let address = id.address();
    let probe = craze.probe.as_ref().unwrap();
    probe.answer(Probe::Hub);
    let roster = reopen_roster(&craze, json!([row(HOST_A, "ses-a")])).await;
    wait_for("live", || {
        (status_of(&hosts, "mini3")?["craze"]["state"] == "live").then_some(())
    })
    .await;
    lanes
        .open(&address, "craze", HOST_A)
        .await
        .expect("the craze lane");
    let _lane_conn = craze.next().await;

    // Resume rejoins A; craze has not answered yet.
    let reopening = {
        let hosts = Arc::clone(&hosts);
        let address = address.clone();
        tokio::spawn(async move { hosts.craze_reopen(&address, SAVED_ID).await })
    };
    let (mut hub, req) = reopen_asked(&craze).await;

    // The hub goes meanwhile: the Dormant departs the lane, and drops A.
    roster.close().await;
    probe.answer(Probe::NoHub);
    wait_for("the lane to depart", || {
        (lane_state(&lanes, &address, HOST_A) == Some("departing")).then_some(())
    })
    .await;
    assert!(
        !hosts.craze_lanes(&address).contains_key(HOST_A),
        "the Dormant dropped A from the listing"
    );

    // craze's answer arrives after the Dormant.
    hub.reply(
        &req,
        json!({"session": row(HOST_A, "ses-a"), "joined": true}),
    )
    .await;
    let answer = reopening.await.unwrap().expect("reopened");
    assert_eq!(answer["ended"], false, "{answer}");
    assert_eq!(
        lane_state(&lanes, &address, HOST_A),
        Some("live"),
        "the listing's return cancelled the departure"
    );
    assert!(
        hosts.craze_lanes(&address).contains_key(HOST_A),
        "listed again: openable"
    );

    // The panel closes the lane and opens it again: the row resolves.
    lanes.close(&address, "craze", HOST_A);
    lanes
        .open(&address, "craze", HOST_A)
        .await
        .expect("the row resolves: a fresh lane");
    let _again = craze.next().await;
    assert_eq!(lane_state(&lanes, &address, HOST_A), Some("live"));
    assert!(
        lock(&hosts.state)
            .get(&id)
            .is_some_and(|m| m.craze.phase == Some(CrazePhase::Dormant)),
        "and no seed did any of it: the source is still dormant"
    );
}

/// **A no-hub probe that folds before a reopen is listed never ends the
/// resumed lane** (c8b review finding 2): A's lane is live on the roster;
/// the hub goes — the roster ends — while A's host outlives it, and the
/// source's next probe is asked, and held: it looks while no hub runs.
/// Resume is sent, and starts a hub of its own that rejoins A's host; THEN
/// the held probe answers what it saw — no hub. Nothing was listed since it
/// started, so its Dormant folds and departs A's lane. Resume's answer is
/// listed after that: its return cancels the departure, and the lane stays
/// live past the grace while the replacement probe is held (an ordinary slow
/// connection) — and when that probe finds Resume's hub, the seed listing A
/// keeps it live. With C8's dormancy epoch the return was withheld (the
/// Dormant folded after the request went out), and the grace ended a running
/// session's panel `hub_gone` whenever the replacement seed came later than
/// it.
#[tokio::test]
async fn a_no_hub_folded_before_a_reopen_is_listed_never_ends_the_resumed_lane() {
    let craze = Scripted::attach_only();
    let (hosts, lanes, id) = remote_host(&craze, ORDER_GRACE);
    let address = id.address();
    let probe = craze.probe.as_ref().unwrap();
    probe.answer(Probe::Hub);
    let roster = reopen_roster(&craze, json!([row(HOST_A, "ses-a")])).await;
    wait_for("live", || {
        (status_of(&hosts, "mini3")?["craze"]["state"] == "live").then_some(())
    })
    .await;
    lanes
        .open(&address, "craze", HOST_A)
        .await
        .expect("the craze lane");
    let _lane_conn = craze.next().await;

    // The hub goes: the roster ends, and the next probe is asked — and held.
    roster.close().await;
    wait_for("the next probe", || {
        (probe.calls.load(Ordering::SeqCst) == 2).then_some(())
    })
    .await;

    // Resume is sent (its own dial starts a hub that rejoins A's host).
    let reopening = {
        let hosts = Arc::clone(&hosts);
        let address = address.clone();
        tokio::spawn(async move { hosts.craze_reopen(&address, SAVED_ID).await })
    };
    let (mut hub, req) = reopen_asked(&craze).await;

    // The held probe answers what it saw: no hub. Its Dormant departs A.
    probe.answer(Probe::NoHub);
    wait_for("the lane to depart", || {
        (lane_state(&lanes, &address, HOST_A) == Some("departing")).then_some(())
    })
    .await;

    // Resume's answer is listed after that Dormant.
    hub.reply(
        &req,
        json!({"session": row(HOST_A, "ses-a"), "joined": true}),
    )
    .await;
    let answer = reopening.await.unwrap().expect("reopened");
    assert_eq!(answer["ended"], false, "{answer}");
    assert_eq!(
        lane_state(&lanes, &address, HOST_A),
        Some("live"),
        "the listing's return cancelled the departure"
    );

    // The replacement probe is slow: the lane outlives the grace.
    tokio::time::sleep(ORDER_GRACE + Duration::from_millis(500)).await;
    assert_eq!(
        lane_state(&lanes, &address, HOST_A),
        Some("live"),
        "the cancelled grace ended nothing"
    );
    assert!(
        lock(&hosts.state)
            .get(&id)
            .is_some_and(|m| m.craze.phase == Some(CrazePhase::Dormant)),
        "no seed has come"
    );

    // The probe finds Resume's hub, and its seed lists A: the lane stays.
    probe.answer(Probe::Hub);
    let _roster = reopen_roster(&craze, json!([row(HOST_A, "ses-a")])).await;
    wait_for("the seed", || {
        (status_of(&hosts, "mini3")?["craze"]["state"] == "live").then_some(())
    })
    .await;
    assert_eq!(lane_state(&lanes, &address, HOST_A), Some("live"));
}

/// **A no-hub probe older than a reopen never departs the session it
/// started** (C8 review finding 2 — the reported interleaving): a dormant
/// machine's next probe is asked, and has not answered; Resume starts a hub
/// and a session on it, the row is listed, and its transcript opens; THEN
/// that probe answers what it saw — no hub. A listing came after the probe
/// started, so its answer is stale: it is not folded, the lane stays live
/// past the grace, and the source asks again at once. Before the fix the
/// answer departed the new session's row with news newer than the listing's,
/// and the grace ended a running session's panel `hub_gone`.
#[tokio::test]
async fn a_no_hub_probe_older_than_a_reopen_never_departs_its_session() {
    let craze = Scripted::attach_only();
    let (hosts, lanes, id) = remote_host(&craze, GRACE);
    let address = id.address();
    let probe = craze.probe.as_ref().unwrap();
    probe.answer(Probe::NoHub);
    dormant_at(&hosts, &id).await;
    // The next probe is asked — and held.
    wait_for("the next probe", || {
        (probe.calls.load(Ordering::SeqCst) == 2).then_some(())
    })
    .await;

    // Resume starts the session; its row is listed, and its lane opens.
    reopen_new(&hosts, &craze, &address).await;
    lanes
        .open(&address, "craze", HOST_NEW)
        .await
        .expect("the reopened session's lane");
    let _lane_conn = craze.next().await;

    // The held probe answers: no hub — as there was none when it looked.
    probe.answer(Probe::NoHub);
    wait_for("the source to ask again", || {
        (probe.calls.load(Ordering::SeqCst) == 3).then_some(())
    })
    .await;
    assert_eq!(
        lane_state(&lanes, &address, HOST_NEW),
        Some("live"),
        "a no-hub answer older than the listing departs nothing"
    );
    tokio::time::sleep(GRACE * 2).await;
    assert_eq!(lane_state(&lanes, &address, HOST_NEW), Some("live"));
}

/// A dial whose next dial waits at a [`TestGap`] before dialling `inner` —
/// the roster's, in the cell below, whose explicit actions dial `inner`
/// itself (the reach's `ensure`) and never wait.
struct PauseNext {
    inner: Arc<dyn CrazeDial>,
    gap: std::sync::Mutex<Option<TestGap>>,
}

impl CrazeDial for PauseNext {
    fn dial(&self) -> BoxFut<Result<CrazeStream, DialError>> {
        let gap = lock(&self.gap).take();
        let inner = Arc::clone(&self.inner);
        Box::pin(async move {
            if let Some((reached, proceed)) = gap {
                reached.notify_one();
                proceed.notified().await;
            }
            inner.dial().await
        })
    }

    fn fall_back(&self, err: &DialError) -> bool {
        self.inner.fall_back(err)
    }
}

/// **A roster's no-hub dial older than a reopen never departs the session it
/// started** (C8 review finding 2, the roster's path): a dormant machine's
/// probe finds a hub, and its roster's `--no-start` dial starts — and is held
/// — while Resume starts a session on a hub of its own, lists the row, and
/// opens its transcript; THEN that dial finds no hub. It started before the
/// listing, so its answer is stale like a probe's: not folded — the lane
/// stays live, the source Dormant as its last folded answer left it — and
/// the source probes again at once.
#[tokio::test]
async fn a_roster_no_hub_older_than_a_reopen_never_departs_its_session() {
    let craze = Scripted::attach_only();
    let pause = Arc::new(PauseNext {
        inner: Arc::clone(&craze.hook) as Arc<dyn CrazeDial>,
        gap: std::sync::Mutex::new(None),
    });
    let id = HostId::Machine("mini3".to_string());
    // The roster and the lanes dial through `pause`; Resume through the hook.
    let reach = CrazeReach {
        dial: Arc::clone(&pause) as Arc<dyn CrazeDial>,
        ensure: Some(Arc::clone(&craze.hook) as Arc<dyn CrazeDial>),
        ..craze.reach()
    };
    let only = id.clone();
    let hosts = Arc::new(RoostHosts::start_with(
        &tokio::runtime::Handle::current(),
        &config_with(&["mini3"]),
        test_options(&[]),
        false,
        Arc::new(|| {}),
        CrazeReaches::Fixed(Arc::new(move |h: &HostId| {
            (*h == only).then(|| reach.clone())
        })),
        timings(),
    ));
    let lanes = lanes_holding(&hosts, GRACE, Arc::new(NewsHold::default()));
    let address = id.address();
    let probe = craze.probe.as_ref().unwrap();
    probe.answer(Probe::NoHub);
    dormant_at(&hosts, &id).await;

    // The probe finds a hub, and the roster's dial is held.
    let (gap, reached, proceed) = test_gap();
    *lock(&pause.gap) = Some(gap);
    probe.answer(Probe::Hub);
    reached.notified().await;

    // Resume starts the session; its row is listed, and its lane opens.
    reopen_new(&hosts, &craze, &address).await;
    lanes
        .open(&address, "craze", HOST_NEW)
        .await
        .expect("the reopened session's lane");
    let _lane_conn = craze.next().await;

    // The held roster dial finds no hub.
    let calls = probe.calls.load(Ordering::SeqCst);
    craze
        .dial
        .fail_next(DialError::NoHub("craze bridge: no hub is running".into()));
    proceed.notify_one();
    wait_for("the source to ask again", || {
        (probe.calls.load(Ordering::SeqCst) > calls).then_some(())
    })
    .await;
    assert_eq!(
        lane_state(&lanes, &address, HOST_NEW),
        Some("live"),
        "a no-hub dial older than the listing departs nothing"
    );
    assert_eq!(
        status_of(&hosts, "mini3").unwrap()["craze"]["state"],
        "dormant",
        "nothing folded: the source is as its last folded answer left it"
    );
    tokio::time::sleep(GRACE * 2).await;
    assert_eq!(lane_state(&lanes, &address, HOST_NEW), Some("live"));
}

/// **The saved sessions are craze's, read afresh** (plan 032 §3.4):
/// `craze.saved` asks `sessions.saved{limit: 50}` on a connection of its own
/// and answers craze's rows as the contract's — the id opaque, the times in
/// Unix ms, Go's zero creation time absent — with craze's `more`, and the
/// incarnation it came through (`source_gen`).
#[tokio::test]
async fn saved_lists_crazes_saved_sessions() {
    let craze = Scripted::eager();
    let (hosts, _roster) = live_reopen_host(&craze).await;
    let gen = lock(&hosts.state).get(&local()).expect("state").craze.gen;
    let listing = {
        let hosts = Arc::clone(&hosts);
        tokio::spawn(async move { hosts.craze_saved(LOCALHOST).await })
    };
    let mut hub = craze.next().await;
    hub.hello(EPOCH, reopen_hub_capabilities()).await;
    let req = hub.expect("sessions.saved").await;
    assert_eq!(req["params"], json!({ "limit": 50 }), "{req}");
    hub.reply(
        &req,
        json!({"sessions": [
                  {"id": SAVED_ID, "crazeSessionId": SAVED_ID, "provider": "grok",
                   "providerSessionId": "ses-saved", "workspace": "/work/saved",
                   "title": "a saved session", "createdAt": "0001-01-01T00:00:00Z",
                   "updatedAt": "2026-10-12T10:00:00Z"},
                  {"id": "grok:legacy", "provider": "grok", "providerSessionId": "legacy",
                   "workspace": "/work/old", "createdAt": "2026-10-01T09:00:00Z",
                   "updatedAt": "2026-10-01T10:00:00Z"}],
               "more": true}),
    )
    .await;
    let answer = listing.await.unwrap().expect("the saved sessions");
    assert_eq!(answer["machine"], LOCALHOST);
    let sessions = answer["saved"]["sessions"].as_array().unwrap();
    assert_eq!(
        sessions.iter().map(|s| s["id"].clone()).collect::<Vec<_>>(),
        [json!(SAVED_ID), json!("grok:legacy")],
        "craze's order, the ids opaque"
    );
    assert_eq!(sessions[0]["workspace"], "/work/saved");
    assert_eq!(sessions[0]["title"], "a saved session");
    assert_eq!(
        sessions[0]["updated_unix_ms"],
        shed_core::time::rfc3339_unix_ms("2026-10-12T10:00:00Z").unwrap()
    );
    assert!(
        sessions[0].get("created_unix_ms").is_none(),
        "Go's zero time is no time: {}",
        sessions[0]
    );
    assert!(sessions[1].get("title").is_none(), "{}", sessions[1]);
    assert_eq!(answer["saved"]["more"], true);
    assert_eq!(answer["source_gen"], gen);
}

/// **A reopen that outlives its host's incarnation is `superseded`, never
/// folded** (C5 review): the shed is removed and registered again — the same
/// address, a NEW craze source — while its reopen is held at the hub. The
/// answer, when it comes, is the OLD source's: `superseded` (not `ended` —
/// the session runs), its row the last known (stale), `source_gen` the old
/// generation — and nothing is listed under the new incarnation, whose source
/// never saw it, so nothing could open on it. A reopen on the CURRENT
/// incarnation says `superseded: false` ([`a_reopened_session_is_listed_and_openable_at_once`]).
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn a_reopen_that_outlives_its_hosts_incarnation_is_superseded() {
    let (id, config) = srv_shed("s1");
    let address = id.address();
    let old = Scripted::attach_only();
    let new = Scripted::attach_only();
    let hosts = start_queued(&config, &id, vec![Arc::clone(&old), Arc::clone(&new)]);
    refresh(&hosts, &["s1"]).await;
    old.probe.as_ref().unwrap().answer(Probe::Hub);
    let _old_roster = reopen_roster(&old, json!([row(HOST_A, "ses-a")])).await;
    wait_for("the old source's row", || {
        craze_rows(&hosts)
            .iter()
            .any(|r| r["slug"] == HOST_A && r["stale"] == false)
            .then_some(())
    })
    .await;
    let old_gen = lock(&hosts.state).get(&id).expect("state").craze.gen;

    let reopening = {
        let hosts = Arc::clone(&hosts);
        let address = address.clone();
        tokio::spawn(async move { hosts.craze_reopen(&address, SAVED_ID).await })
    };
    let (mut hub, req) = reopen_asked(&old).await;
    // Removed, and registered again, while the reopen is held at the hub.
    refresh(&hosts, &[]).await;
    refresh(&hosts, &["s1"]).await;
    let new_gen = lock(&hosts.state)
        .get(&id)
        .expect("the new registration's state")
        .craze
        .gen;
    assert_ne!(old_gen, new_gen, "a new incarnation");

    hub.reply(
        &req,
        json!({"session": roster_row(HOST_NEW, SAVED_ID, "/work/saved",
                                     json!({"title": "a saved session", "activity": "idle",
                                            "providerSessionId": "ses-saved"})),
               "joined": false}),
    )
    .await;
    let answer = reopening.await.unwrap().expect("answered");
    assert_eq!(answer["superseded"], true, "{answer}");
    assert_eq!(answer["ended"], false, "the session runs: {answer}");
    assert_eq!(answer["session"]["stale"], true, "the last known: {answer}");
    assert_eq!(answer["source_gen"], old_gen);
    let listed = craze_rows(&hosts);
    assert!(
        !listed.iter().any(|r| r["slug"] == HOST_NEW),
        "nothing listed under the new incarnation: {listed:?}"
    );
}

/// **Every craze status names the host's incarnation** (C5 re-review): a
/// shed's `shed_craze` entry while dormant, and its status row once listed,
/// carry `craze.source_gen` — the generation `craze.create_options`,
/// `craze.saved` and `craze.reopen` report. Removed and registered again
/// under the same address, the shed is a new incarnation whose status names
/// a new, LARGER generation from its first appearance: what the sheet resets
/// its Resume state on (a reopen the old one left running included) with no
/// create-options read, which may fail.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn every_craze_status_names_the_hosts_incarnation() {
    let (id, config) = srv_shed("s1");
    let old = Scripted::attach_only();
    let new = Scripted::attach_only();
    let hosts = start_queued(&config, &id, vec![Arc::clone(&old), Arc::clone(&new)]);
    let gen_now = || lock(&hosts.state).get(&id).expect("state").craze.gen;
    let offered_gen = || {
        shed_craze_of(&hosts)
            .into_iter()
            .find(|e| e["origin"] == SHED)
            .map(|e| e["craze"]["source_gen"].clone())
    };

    refresh(&hosts, &["s1"]).await;
    probe_answers(old.probe.as_ref().unwrap(), Probe::NoHub).await;
    let old_gen = gen_now();
    assert_eq!(offered_gen(), Some(json!(old_gen)), "the dormant entry's");

    // Removed, and registered again under the same address.
    refresh(&hosts, &[]).await;
    assert_eq!(offered_gen(), None, "removed: nothing names it");
    refresh(&hosts, &["s1"]).await;
    let new_probe = new.probe.as_ref().unwrap();
    probe_answers(new_probe, Probe::NoHub).await;
    let new_gen = gen_now();
    assert!(
        new_gen > old_gen,
        "a later incarnation's is the larger: {old_gen}, then {new_gen}"
    );
    assert_eq!(
        offered_gen(),
        Some(json!(new_gen)),
        "the new entry names the new incarnation"
    );

    // Listed by its first Ready: its status row names the same one.
    new_probe.answer(Probe::Hub);
    let _hub = new.roster(json!([row(HOST_A, "ses-a")])).await;
    let listed = wait_for("listed by its first Ready", || status_of(&hosts, SHED)).await;
    assert_eq!(listed["craze"]["source_gen"], new_gen, "{listed}");
}

/// **A hub without `reopen` is refused before any dial** (plan 032 PC14i):
/// a LIVE source whose hub's `hello` lacks `reopen` (craze 0.1.x) answers
/// both resume ops `unavailable`, in shed-craze's one older-hub wording —
/// naming no version or pid, which this layer never holds (PC10) — and
/// nothing is dialled.
#[tokio::test]
async fn a_hub_without_reopen_refuses_resume_before_any_dial() {
    let craze = Scripted::eager();
    let hosts = start_scripted(
        &ShedConfig::default(),
        &test_options(&[]),
        vec![(local(), Arc::clone(&craze))],
    );
    let _roster = craze.roster(json!([])).await;
    wait_for("live", || {
        (status_of(&hosts, LOCALHOST)?["craze"]["state"] == "live").then_some(())
    })
    .await;
    let dials = craze.hook.dials();
    let words = shed_craze::errors::reopen_unsupported_by("", None);
    assert!(
        words.starts_with("this craze hub (craze version unknown) does not offer reopening"),
        "{words}"
    );
    let e = hosts.craze_saved(LOCALHOST).await.unwrap_err();
    assert_eq!(
        (e.code, e.message.as_str()),
        ("unavailable", words.as_str())
    );
    let e = hosts.craze_reopen(LOCALHOST, SAVED_ID).await.unwrap_err();
    assert_eq!(
        (e.code, e.message.as_str()),
        ("unavailable", words.as_str())
    );
    assert_eq!(craze.hook.dials(), dials, "refused before any dial");
}

/// **A reopen's refusal keeps its variant** (plan 032 §3.4): craze's
/// `unknown_session` is `gone` (the sheet drops the row),
/// `not_accepting`/`start_failed` is `start_failed` with craze's cause (the
/// row kept), and `unavailable` stays `unavailable` (Retry) — and none of
/// them lists anything.
#[tokio::test]
async fn a_reopen_refusal_keeps_its_variant_as_the_code() {
    let craze = Scripted::eager();
    let (hosts, _roster) = live_reopen_host(&craze).await;
    let cases = [
        ("unknown_session", "unknown_session", json!({}), "gone"),
        (
            "not_accepting",
            "start_failed",
            json!({"cause": "grok exited: no key"}),
            "start_failed",
        ),
        ("unavailable", "busy", json!({}), "unavailable"),
    ];
    for (code, reason, extra, want) in cases {
        let reopening = {
            let hosts = Arc::clone(&hosts);
            tokio::spawn(async move { hosts.craze_reopen(LOCALHOST, SAVED_ID).await })
        };
        let (mut hub, req) = reopen_asked(&craze).await;
        hub.refuse(&req, code, reason, extra).await;
        let e = reopening.await.unwrap().unwrap_err();
        assert_eq!(e.code, want, "{code}/{reason}: {e:?}");
        if want == "start_failed" {
            assert_eq!(e.message, "could not start: grok exited: no key");
        }
    }
    let rows = craze_rows(&hosts);
    assert_eq!(rows.len(), 1, "nothing listed: {rows:?}");
}

/// **The home a workspace is shortened by is one this app KNOWS** (plan 032
/// §3.4): this machine's own `HOME` for the implicit `localhost`, every
/// shed's `/home/shed`, and none for a configured machine — reached over
/// ssh, its home never learned (a machine configured AS `localhost` too).
#[tokio::test]
async fn the_resume_home_is_known_never_guessed() {
    let hosts = start_scripted(&config_with(&["mini3"]), &test_options(&[]), vec![]);
    assert_eq!(
        hosts.craze_home(&local()),
        std::env::var("HOME")
            .ok()
            .filter(|h| h.starts_with('/') && h.len() > 1)
    );
    assert_eq!(
        hosts.craze_home(&HostId::Shed {
            server: "srv".into(),
            name: "s1".into()
        }),
        Some("/home/shed".to_string())
    );
    assert_eq!(hosts.craze_home(&HostId::Machine("mini3".into())), None);
    let configured = start_scripted(&config_with(&[LOCALHOST]), &test_options(&[]), vec![]);
    assert_eq!(
        configured.craze_home(&local()),
        None,
        "an ssh-reached localhost"
    );
}
