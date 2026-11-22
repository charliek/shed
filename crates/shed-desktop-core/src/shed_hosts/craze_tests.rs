//! Shed-only craze lifecycle, fold, removal-race, generation-fence, and
//! reopen tests against scripted hubs (`ScriptedDial`) and scripted find-only
//! probes (`ScriptedProbe`) — deterministic, zero external binaries.

use std::collections::{HashMap, VecDeque};
use std::sync::atomic::{AtomicUsize, Ordering};
use std::time::Duration;

use serde_json::json;
use shed_core::config::ShedServerEntry;
use shed_craze::testing::{
    full_hub_capabilities, reopen_hub_capabilities, roster_row, HookDial, HubEnd, ScriptedDial,
};
use shed_craze::{CrazeDial, DialError, Probe};
use shed_lane::lane::{LaneCreateRequest, SourceOffline};
use shed_roost::testing::{ownership, FakeRoost};
use tokio::sync::mpsc::UnboundedReceiver;

use super::*;
use crate::shed_craze::{BoxFut, CrazeProbe, CrazeReach, CrazeTimings};
use crate::shed_lanes::{
    LaneEvents, LaneHosts, LaneUiEvent, ShedLanes, DOWN_HOST_REMOVED, DOWN_HUB_GONE,
    DOWN_SESSION_GONE,
};

const EPOCH: &str = "0a1b2c3d4e5f";
const HOST_A: &str = "aaaaaaaaaaaa";
const HOST_B: &str = "bbbbbbbbbbbb";
const TAB: i64 = 5;

fn timings() -> CrazeTimings {
    CrazeTimings {
        dormant: Duration::from_millis(40),
        backoff_base: Duration::from_millis(10),
        backoff_max: Duration::from_millis(40),
    }
}

fn shed(server: &str, name: &str) -> ShedId {
    ShedId::new(server, name).unwrap()
}

fn config_with_servers(names: &[&str]) -> ShedConfig {
    ShedConfig {
        servers: names
            .iter()
            .map(|name| ShedServerEntry {
                name: (*name).to_string(),
                host: "127.0.0.1".to_string(),
                http_port: 8080,
                ssh_port: 2222,
                ..Default::default()
            })
            .collect(),
        ..Default::default()
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

#[derive(Default)]
struct CountingEvents {
    changes: AtomicUsize,
    craze_changes: Mutex<Vec<(ShedId, u64)>>,
}

impl HostEvents for CountingEvents {
    fn on_hosts_changed(&self) {
        self.changes.fetch_add(1, Ordering::SeqCst);
    }

    fn on_craze_source_changed(&self, shed: &ShedId, generation: u64) {
        lock(&self.craze_changes).push((shed.clone(), generation));
    }
}

#[derive(Default)]
struct ScriptedProbe {
    answers: Mutex<VecDeque<Probe>>,
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

struct Scripted {
    hook: Arc<HookDial>,
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
            ensure: None,
        }
    }

    async fn next(&self) -> HubEnd {
        tokio::time::timeout(Duration::from_secs(10), self.conns.lock().await.recv())
            .await
            .expect("a dial in time")
            .expect("the dial is alive")
    }

    async fn roster(&self, rows: Value) -> HubEnd {
        let mut hub = self.next().await;
        hub.hello(EPOCH, full_hub_capabilities()).await;
        hub.subscribed("sub-1", EPOCH, rows).await;
        hub
    }

    async fn reopen_roster(&self, rows: Value) -> HubEnd {
        let mut hub = self.next().await;
        hub.hello(EPOCH, reopen_hub_capabilities()).await;
        hub.subscribed("sub-1", EPOCH, rows).await;
        hub
    }
}

fn row(host_id: &str, provider_session: &str) -> Value {
    roster_row(
        host_id,
        &format!("craze-{host_id}"),
        "/work",
        json!({
            "title": format!("session {host_id}"),
            "activity": "idle",
            "providerSessionId": provider_session,
            "since": "2026-01-01T00:00:00Z"
        }),
    )
}

fn test_options(pairs: &[(&ShedId, &std::path::Path)]) -> ShedReachOptions {
    ShedReachOptions {
        roost_sockets: pairs
            .iter()
            .map(|(id, p)| (id.as_target(), p.to_path_buf()))
            .collect::<HashMap<_, _>>(),
        test_mode: true,
        ..Default::default()
    }
}

fn start_scripted(
    config: &ShedConfig,
    options: &ShedReachOptions,
    reaches: Vec<(ShedId, Arc<Scripted>)>,
    events: Arc<dyn HostEvents>,
) -> Arc<ShedHosts> {
    let map: BTreeMap<ShedId, Arc<Scripted>> = reaches.into_iter().collect();
    Arc::new(ShedHosts::start_with(
        &tokio::runtime::Handle::current(),
        config,
        options.clone(),
        false,
        events,
        CrazeReaches::Fixed(Arc::new(move |id: &ShedId| map.get(id).map(|s| s.reach()))),
        timings(),
    ))
}

fn start_queued(config: &ShedConfig, id: &ShedId, queue: Vec<Arc<Scripted>>) -> Arc<ShedHosts> {
    let id = id.clone();
    let queue = Mutex::new(VecDeque::from(queue));
    Arc::new(ShedHosts::start_with(
        &tokio::runtime::Handle::current(),
        config,
        test_options(&[]),
        false,
        Arc::new(NoopHostEvents),
        CrazeReaches::Fixed(Arc::new(move |want: &ShedId| {
            (*want == id)
                .then(|| lock(&queue).pop_front().map(|s| s.reach()))
                .flatten()
        })),
        timings(),
    ))
}

#[derive(Default)]
struct RecordedLaneEvents(Mutex<Vec<LaneUiEvent>>);

impl RecordedLaneEvents {
    fn down_reason(&self, target: &ShedId, session_id: &str) -> Option<String> {
        lock(&self.0).iter().rev().find_map(|ev| {
            if ev.target == *target && ev.session_id == session_id {
                if let shed_lane::lane::LaneEvent::Down { reason } = &ev.event {
                    return Some(reason.clone());
                }
            }
            None
        })
    }
}

impl LaneEvents for RecordedLaneEvents {
    fn on_lane_event(&self, event: LaneUiEvent) {
        lock(&self.0).push(event);
    }
}

fn lanes_over(
    hosts: &Arc<ShedHosts>,
    grace: Duration,
) -> (Arc<ShedLanes>, Arc<RecordedLaneEvents>) {
    let events = Arc::new(RecordedLaneEvents::default());
    let lanes = Arc::new(
        ShedLanes::new(
            tokio::runtime::Handle::current(),
            Arc::clone(&events) as Arc<dyn LaneEvents>,
            Arc::clone(hosts) as Arc<dyn LaneHosts>,
        )
        .with_depart_grace(grace),
    );
    crate::wire_craze_observer(hosts, &lanes);
    (lanes, events)
}

fn craze_rows(hosts: &ShedHosts) -> Vec<Value> {
    hosts
        .snapshot(HostFilter::ALL)
        .sessions
        .into_iter()
        .filter(|r| r["source"] == "craze")
        .collect()
}

fn status_of(hosts: &ShedHosts, id: &ShedId) -> Option<Value> {
    let target = id.as_target();
    hosts.status().into_iter().find(|s| s["target"] == target)
}

fn test_gap() -> (TestGap, Arc<tokio::sync::Notify>, Arc<tokio::sync::Notify>) {
    let reached = Arc::new(tokio::sync::Notify::new());
    let proceed = Arc::new(tokio::sync::Notify::new());
    (
        (Arc::clone(&reached), Arc::clone(&proceed)),
        reached,
        proceed,
    )
}

fn create_req(id: &str) -> LaneCreateRequest {
    crate::shed_craze::create_request(
        Some("/work"),
        Some("claude"),
        None,
        Some("my session"),
        Some(id),
    )
    .unwrap()
}

fn created_result(psid: &str) -> Value {
    json!({
        "session": row(HOST_A, psid),
        "prompt": "none",
        "promptError": null,
    })
}

/// A craze-only shed is sourced when first observed running and lists its rows
/// with `target` (never `machine`).
#[tokio::test]
async fn a_craze_only_shed_is_sourced_when_first_observed_running() {
    let s1 = shed("srv", "s1");
    let craze = Scripted::attach_only();
    let events = Arc::new(CountingEvents::default());
    let hosts = start_scripted(
        &config_with_servers(&["srv"]),
        &test_options(&[]),
        vec![(s1.clone(), Arc::clone(&craze))],
        Arc::clone(&events) as Arc<dyn HostEvents>,
    );

    hosts
        .observe_sheds(std::slice::from_ref(&s1), &["srv".to_string()])
        .await;
    craze.probe.as_ref().unwrap().answer(Probe::Hub);
    let _hub = craze.roster(json!([row(HOST_A, "ses-a")])).await;

    let rows = wait_for("shed craze row", || {
        let rows = craze_rows(&hosts);
        (!rows.is_empty()).then_some(rows)
    })
    .await;
    assert_eq!(rows[0]["target"], "roost:srv/s1");
    assert_eq!(rows[0]["origin"], "roost:srv/s1");
    assert_eq!(rows[0]["host"], "srv");
    assert_eq!(rows[0]["shed"], "s1");
    assert!(rows[0].get("machine").is_none());
    let status = status_of(&hosts, &s1).expect("shed listed by craze");
    assert_eq!(status["craze"]["state"], "live");
    assert_eq!(lock(&events.craze_changes).len(), 1);
    assert_eq!(lock(&events.craze_changes)[0].0, s1);
}

/// A roost craze tab folds into its hub row and unfolds when the craze feed drops.
#[tokio::test]
async fn a_craze_tab_folds_into_its_hub_row_and_returns_when_the_feed_drops() {
    let fake = FakeRoost::start().await;
    fake.set_tab_axes(
        TAB,
        "working",
        Some(ownership("craze", "ses-a", "", 1_700_000_100)),
        false,
    );
    let s1 = shed("srv", "s1");
    let craze = Scripted::eager();
    let hosts = start_scripted(
        &config_with_servers(&["srv"]),
        &test_options(&[(&s1, fake.socket_path())]),
        vec![(s1.clone(), Arc::clone(&craze))],
        Arc::new(NoopHostEvents),
    );
    hosts
        .observe_sheds(std::slice::from_ref(&s1), &["srv".to_string()])
        .await;

    let hub = craze.roster(json!([row(HOST_A, "ses-a")])).await;
    let tab_str = TAB.to_string();
    let folded = wait_for("roost tab folded into craze row", || {
        let rows = hosts.snapshot(HostFilter::ALL).sessions;
        (rows.len() == 1
            && rows[0]["source"] == "craze"
            && rows[0]["tab_id"].as_str() == Some(tab_str.as_str()))
        .then_some(rows)
    })
    .await;
    assert_eq!(folded[0]["slug"], HOST_A);

    // Drop the craze feed (EOF) and hold the next dial so craze stays down.
    craze.hook.hold_dials();
    drop(hub);

    wait_for("roost tab reappears when craze is not live", || {
        let rows = hosts.snapshot(HostFilter::ALL).sessions;
        rows.iter()
            .any(|r| r["source"] == "roost" && r["tab_id"].as_str() == Some(tab_str.as_str()))
            .then_some(())
    })
    .await;
}

/// Folds never cross sheds (even on the same server with matching providerSessionId).
#[tokio::test]
async fn a_fold_never_crosses_sheds() {
    let fake_a = FakeRoost::start().await;
    fake_a.set_tab_axes(
        TAB,
        "working",
        Some(ownership("craze", "shared-ses", "", 1_700_000_100)),
        false,
    );
    let s1 = shed("srv", "s1");
    let s2 = shed("srv", "s2");
    let craze_b = Scripted::eager();
    let hosts = start_scripted(
        &config_with_servers(&["srv"]),
        &test_options(&[(&s1, fake_a.socket_path())]),
        vec![(s2.clone(), Arc::clone(&craze_b))],
        Arc::new(NoopHostEvents),
    );
    hosts
        .observe_sheds(&[s1.clone(), s2.clone()], &["srv".to_string()])
        .await;
    let _hub = craze_b.roster(json!([row(HOST_A, "shared-ses")])).await;

    let snap = wait_for("both sheds listed without cross-folding", || {
        let snap = hosts.snapshot(HostFilter::ALL);
        (snap.sessions.len() == 2).then_some(snap)
    })
    .await;
    let roost = snap
        .sessions
        .iter()
        .find(|r| r["source"] == "roost")
        .unwrap();
    let craze = snap
        .sessions
        .iter()
        .find(|r| r["source"] == "craze")
        .unwrap();
    assert_eq!(roost["target"], "roost:srv/s1");
    assert_eq!(craze["target"], "roost:srv/s2");
    assert!(craze.get("tab_id").is_none());
}

/// A dormant shed is offered in `shed_craze` (not listed in `hosts`) until its
/// first `Ready` moves it into `hosts`; `NotInstalled` withdraws it from `shed_craze`.
#[tokio::test]
async fn dormant_shed_transitions_between_shed_craze_and_hosts() {
    let s1 = shed("srv", "s1");
    let craze = Scripted::attach_only();
    let hosts = start_scripted(
        &config_with_servers(&["srv"]),
        &test_options(&[]),
        vec![(s1.clone(), Arc::clone(&craze))],
        Arc::new(NoopHostEvents),
    );
    hosts
        .observe_sheds(std::slice::from_ref(&s1), &["srv".to_string()])
        .await;

    let probe = craze.probe.as_ref().unwrap();
    probe.answer(Probe::NoHub);
    let snap = wait_for("dormant shed in shed_craze", || {
        let snap = hosts.snapshot(HostFilter::ALL);
        (!snap.shed_craze.is_empty()).then_some(snap)
    })
    .await;
    assert!(snap.hosts.is_empty());
    assert_eq!(snap.shed_craze[0]["target"], "roost:srv/s1");
    assert_eq!(snap.shed_craze[0]["craze"]["state"], "dormant");

    probe.answer(Probe::Hub);
    let _hub = craze.roster(json!([])).await;
    let snap = wait_for("shed promoted to hosts on Ready", || {
        let snap = hosts.snapshot(HostFilter::ALL);
        (!snap.hosts.is_empty()).then_some(snap)
    })
    .await;
    assert!(snap.shed_craze.is_empty());
    assert_eq!(snap.hosts[0]["target"], "roost:srv/s1");
    assert_eq!(snap.hosts[0]["craze"]["state"], "live");

    // A second shed that turns out NotInstalled leaves shed_craze.
    let s2 = shed("srv", "s2");
    let craze2 = Scripted::attach_only();
    let hosts2 = start_scripted(
        &config_with_servers(&["srv"]),
        &test_options(&[]),
        vec![(s2.clone(), Arc::clone(&craze2))],
        Arc::new(NoopHostEvents),
    );
    hosts2
        .observe_sheds(std::slice::from_ref(&s2), &["srv".to_string()])
        .await;
    let probe2 = craze2.probe.as_ref().unwrap();
    probe2.answer(Probe::NoHub);
    wait_for("s2 offered", || {
        (!hosts2.snapshot(HostFilter::ALL).shed_craze.is_empty()).then_some(())
    })
    .await;
    probe2.answer(Probe::Offline {
        cause: SourceOffline::NotInstalled,
        reason: "missing".into(),
    });
    wait_for("s2 leaves shed_craze", || {
        hosts2
            .snapshot(HostFilter::ALL)
            .shed_craze
            .is_empty()
            .then_some(())
    })
    .await;
}

/// **Generation fence**: a late Craze notification from a replaced/older source
/// generation is ignored and NEVER evicts or corrupts a newer generation's lane.
#[tokio::test]
async fn late_craze_notification_from_replaced_source_is_ignored() {
    let s1 = shed("srv", "s1");
    let cfg = config_with_servers(&["srv"]);
    let craze1 = Scripted::eager();
    let craze2 = Scripted::eager();
    let hosts = start_queued(&cfg, &s1, vec![Arc::clone(&craze1), Arc::clone(&craze2)]);
    let (lanes, _lane_events) = lanes_over(&hosts, Duration::from_millis(20));

    hosts
        .observe_sheds(std::slice::from_ref(&s1), &["srv".to_string()])
        .await;
    let _hub1 = craze1.roster(json!([row(HOST_A, "ses-1")])).await;
    wait_for("gen 1 live", || {
        (!craze_rows(&hosts).is_empty()).then_some(())
    })
    .await;
    let gen1 = hosts.craze_source(&s1).unwrap().1;

    // Remove and re-register -> new generation `gen2`.
    hosts.observe_sheds(&[], &["srv".to_string()]).await;
    hosts
        .observe_sheds(std::slice::from_ref(&s1), &["srv".to_string()])
        .await;
    let _hub2 = craze2.roster(json!([row(HOST_A, "ses-2")])).await;
    wait_for("gen 2 live", || {
        (!craze_rows(&hosts).is_empty()).then_some(())
    })
    .await;
    let gen2 = hosts.craze_source(&s1).unwrap().1;
    assert_ne!(gen1, gen2);

    // Open a lane on gen2.
    lanes
        .open(&s1, HOST_A)
        .await
        .expect("open on gen2 succeeds");

    // Inject a stale notification from gen1 directly into `lanes.craze_news`.
    lanes.craze_news(
        &s1,
        gen1,
        CrazeNews::Departed {
            host_ids: &[HOST_A.to_string()],
            cause: Departure::Removed,
            seq: 999,
        },
    );
    lanes.craze_news(&s1, gen1, CrazeNews::HostRemoved { seq: 1000 });
    tokio::time::sleep(Duration::from_millis(60)).await;

    // The gen2 lane must still be live (neither finished nor ended).
    let view = lanes.messages(&s1, HOST_A).unwrap();
    assert_eq!(
        view["finished"], false,
        "stale gen1 news must be ignored: {view}"
    );
    assert_eq!(view["ended"], false);
}

/// Removing a shed while a lane is open ends the lane with `host_removed`, and
/// a removal racing with a re-registration leaves the new registration whole.
#[tokio::test]
async fn remove_racing_reregistration_leaves_new_registration_whole() {
    let s1 = shed("srv", "s1");
    let cfg = config_with_servers(&["srv"]);
    let craze1 = Scripted::eager();
    let craze2 = Scripted::eager();
    let hosts = start_queued(&cfg, &s1, vec![Arc::clone(&craze1), Arc::clone(&craze2)]);
    let (lanes, lane_events) = lanes_over(&hosts, Duration::from_millis(20));

    hosts
        .observe_sheds(std::slice::from_ref(&s1), &["srv".to_string()])
        .await;
    let _hub1 = craze1.roster(json!([row(HOST_A, "ses-1")])).await;
    wait_for("gen 1 ready", || {
        (!craze_rows(&hosts).is_empty()).then_some(())
    })
    .await;

    lanes.open(&s1, HOST_A).await.expect("open");

    // Pause `detach` after it removes state/reg, then run a new registration.
    let (gap, reached, proceed) = test_gap();
    *lock(&hosts.remove_gap) = Some(gap);
    let removing = tokio::spawn({
        let hosts = Arc::clone(&hosts);
        async move { hosts.observe_sheds(&[], &["srv".to_string()]).await }
    });
    reached.notified().await;
    proceed.notify_one();
    removing.await.unwrap();

    // Gen 1 lane must end with host_removed.
    wait_for("lane ended with host_removed", || {
        let v = lanes.messages(&s1, HOST_A).ok()?;
        (v["finished"] == true
            && lane_events.down_reason(&s1, HOST_A).as_deref() == Some(DOWN_HOST_REMOVED))
        .then_some(())
    })
    .await;

    // Re-registering works cleanly on gen2.
    hosts
        .observe_sheds(std::slice::from_ref(&s1), &["srv".to_string()])
        .await;
    let _hub2 = craze2.roster(json!([row(HOST_B, "ses-2")])).await;
    let rows = wait_for("gen 2 row", || {
        let r = craze_rows(&hosts);
        (r.len() == 1 && r[0]["slug"] == HOST_B).then_some(r)
    })
    .await;
    assert_eq!(rows[0]["target"], "roost:srv/s1");
}

/// A lane whose session disappears ends after departure grace with `session_gone`;
/// when the hub goes dormant it ends with `hub_gone`.
#[tokio::test]
async fn lane_ends_session_gone_or_hub_gone_on_departure() {
    let s1 = shed("srv", "s1");
    let craze = Scripted::attach_only();
    let hosts = start_scripted(
        &config_with_servers(&["srv"]),
        &test_options(&[]),
        vec![(s1.clone(), Arc::clone(&craze))],
        Arc::new(NoopHostEvents),
    );
    let (lanes, lane_events) = lanes_over(&hosts, Duration::from_millis(30));

    hosts
        .observe_sheds(std::slice::from_ref(&s1), &["srv".to_string()])
        .await;
    craze.probe.as_ref().unwrap().answer(Probe::Hub);
    let mut hub = craze
        .roster(json!([row(HOST_A, "ses-a"), row(HOST_B, "ses-b")]))
        .await;
    wait_for("two craze rows", || {
        (craze_rows(&hosts).len() == 2).then_some(())
    })
    .await;

    // Open lanes on both A and B.
    lanes.open(&s1, HOST_A).await.expect("open A");
    lanes.open(&s1, HOST_B).await.expect("open B");

    // Remove HOST_A from the roster -> ends with session_gone after grace.
    hub.roster("sub-1", EPOCH, json!([]), json!([HOST_A])).await;
    wait_for("A ends session_gone", || {
        let v = lanes.messages(&s1, HOST_A).ok()?;
        (v["finished"] == true
            && lane_events.down_reason(&s1, HOST_A).as_deref() == Some(DOWN_SESSION_GONE))
        .then_some(())
    })
    .await;

    // Drop the hub and have the probe report NoHub -> B ends with hub_gone.
    drop(hub);
    craze.probe.as_ref().unwrap().answer(Probe::NoHub);
    wait_for("B ends hub_gone", || {
        let v = lanes.messages(&s1, HOST_B).ok()?;
        (v["finished"] == true
            && lane_events.down_reason(&s1, HOST_B).as_deref() == Some(DOWN_HUB_GONE))
        .then_some(())
    })
    .await;
}

/// `craze.create`, `craze.saved`, `craze.reopen`, and `craze.open_terminal`
/// (with `tab_open_command_fenced`) all work end-to-end on a shed and emit
/// `"target"` (never `"machine"`).
#[tokio::test]
async fn craze_create_saved_reopen_and_open_terminal_fenced() {
    let fake = FakeRoost::start().await;
    let s1 = shed("srv", "s1");
    let craze = Scripted::eager();
    let hosts = start_scripted(
        &config_with_servers(&["srv"]),
        &test_options(&[(&s1, fake.socket_path())]),
        vec![(s1.clone(), Arc::clone(&craze))],
        Arc::new(NoopHostEvents),
    );
    hosts
        .observe_sheds(std::slice::from_ref(&s1), &["srv".to_string()])
        .await;
    let _hub = craze.reopen_roster(json!([])).await;
    wait_for("reopen hub live", || {
        status_of(&hosts, &s1).filter(|s| s["craze"]["state"] == "live")
    })
    .await;

    // 1. craze.create
    let create_fut = tokio::spawn({
        let hosts = Arc::clone(&hosts);
        let s1 = s1.clone();
        async move { hosts.craze_create(&s1, create_req("req-1"), false).await }
    });
    let mut create_conn = craze.next().await;
    create_conn.hello(EPOCH, reopen_hub_capabilities()).await;
    let req = create_conn.expect("session.create").await;
    create_conn.reply(&req, created_result("psid-1")).await;
    let created = create_fut.await.unwrap().expect("create ok");
    assert_eq!(created["host_id"], HOST_A);
    assert_eq!(created["session"]["target"], "roost:srv/s1");
    assert!(created["session"].get("machine").is_none());

    // 2. craze.open_terminal via tab_open_command_fenced
    let opened = hosts
        .craze_open_terminal(&s1, HOST_A)
        .await
        .expect("open_terminal ok");
    assert_eq!(opened["target"], "roost:srv/s1");
    assert_eq!(opened["origin"], "roost:srv/s1");
    assert_eq!(opened["session_id"], HOST_A);
    assert!(opened.get("machine").is_none());
    let tab_id_str = opened["tab_id"].as_str().unwrap().to_string();
    let rows = craze_rows(&hosts);
    assert_eq!(rows[0]["tab_id"], tab_id_str);

    // 3. craze.saved
    let saved_fut = tokio::spawn({
        let hosts = Arc::clone(&hosts);
        let s1 = s1.clone();
        async move { hosts.craze_saved(&s1).await }
    });
    let mut saved_conn = craze.next().await;
    saved_conn.hello(EPOCH, reopen_hub_capabilities()).await;
    let req = saved_conn.expect("sessions.saved").await;
    saved_conn.reply(&req, json!({ "sessions": [] })).await;
    let saved = saved_fut.await.unwrap().expect("saved ok");
    assert_eq!(saved["target"], "roost:srv/s1");
    assert_eq!(saved["home"], SHED_HOME);
    assert!(saved.get("machine").is_none());

    // 4. craze.reopen
    let reopen_fut = tokio::spawn({
        let hosts = Arc::clone(&hosts);
        let s1 = s1.clone();
        async move { hosts.craze_reopen(&s1, "saved-1").await }
    });
    let mut reopen_conn = craze.next().await;
    reopen_conn.hello(EPOCH, reopen_hub_capabilities()).await;
    let req = reopen_conn.expect("session.reopen").await;
    reopen_conn
        .reply(
            &req,
            json!({
                "session": row(HOST_B, "psid-2"),
                "joined": false,
            }),
        )
        .await;
    let reopened = reopen_fut.await.unwrap().expect("reopen ok");
    assert_eq!(reopened["host_id"], HOST_B);
    assert_eq!(reopened["superseded"], false);
    assert_eq!(reopened["session"]["target"], "roost:srv/s1");
}

/// Held create on an offline/failing shed answers `outcome_unknown`, and a
/// hub without `reopen` refuses `craze.saved`/`craze.reopen` without dialling.
#[tokio::test]
async fn held_create_offline_and_missing_reopen_cap() {
    let s1 = shed("srv", "s1");
    let craze = Scripted::eager();
    let hosts = start_scripted(
        &config_with_servers(&["srv"]),
        &test_options(&[]),
        vec![(s1.clone(), Arc::clone(&craze))],
        Arc::new(NoopHostEvents),
    );
    hosts
        .observe_sheds(std::slice::from_ref(&s1), &["srv".to_string()])
        .await;
    let _hub = craze.roster(json!([])).await;
    wait_for("live without reopen", || {
        status_of(&hosts, &s1).filter(|s| s["craze"]["state"] == "live")
    })
    .await;

    // `full_hub_capabilities()` has `reopen = false` -> refused before any dial.
    let dials_before = craze.hook.dials();
    let err = hosts.craze_saved(&s1).await.unwrap_err();
    assert_eq!(err.code, "unavailable");
    assert_eq!(craze.hook.dials(), dials_before);

    // Offline shed: held create must answer `outcome_unknown`.
    let s2 = shed("srv", "s2");
    let craze2 = Scripted::eager();
    craze2
        .dial
        .fail_next(DialError::NotInstalled("missing".into()));
    let hosts2 = start_scripted(
        &config_with_servers(&["srv"]),
        &test_options(&[]),
        vec![(s2.clone(), Arc::clone(&craze2))],
        Arc::new(NoopHostEvents),
    );
    hosts2
        .observe_sheds(std::slice::from_ref(&s2), &["srv".to_string()])
        .await;
    wait_for("s2 craze offline", || {
        let guard = lock(&hosts2.state);
        guard
            .get(&s2)
            .filter(|m| m.craze.phase == Some(CrazePhase::Offline))
            .map(|_| ())
    })
    .await;
    let err = hosts2
        .craze_create(&s2, create_req("req-held"), true)
        .await
        .unwrap_err();
    assert_eq!(err.code, "outcome_unknown");
}

/// `OpenedTab::retained` honours fence within its daemon and falls back to
/// `UNFENCED_GRACE` when no fence was issued.
#[test]
fn opened_tab_fence_and_unfenced_grace() {
    let now = Instant::now();
    let fenced = OpenedTab {
        host_id: HOST_A.to_string(),
        fence: Some(TabFence {
            revision: 10,
            daemon_session_id: "d1".into(),
            started_at: "t1".into(),
        }),
        opened: now,
    };
    let mut older = RoostInventory::default();
    older.revision = Some(9);
    older.daemon_session_id = "d1".into();
    older.started_at = "t1".into();
    let mut covered = RoostInventory::default();
    covered.revision = Some(10);
    covered.daemon_session_id = "d1".into();
    covered.started_at = "t1".into();
    assert!(fenced.retained(false, &older, now));
    assert!(!fenced.retained(false, &covered, now));
    assert!(fenced.retained(true, &covered, now));

    let unfenced = OpenedTab {
        host_id: HOST_A.to_string(),
        fence: None,
        opened: now,
    };
    assert!(unfenced.retained(false, &covered, now + Duration::from_secs(1)));
    assert!(!unfenced.retained(
        false,
        &covered,
        now + UNFENCED_GRACE + Duration::from_secs(1)
    ));
}

/// Concurrent `observe_sheds` passes serialize and finish on the latest listing.
#[tokio::test]
async fn concurrent_observe_sheds_ends_on_latest_observation() {
    let s1 = shed("srv", "s1");
    let s2 = shed("srv", "s2");
    let hosts = start_scripted(
        &config_with_servers(&["srv"]),
        &test_options(&[]),
        vec![],
        Arc::new(NoopHostEvents),
    );
    let (gap, reached, proceed) = test_gap();
    *lock(&hosts.observe_gap) = Some(gap);

    let first = tokio::spawn({
        let hosts = Arc::clone(&hosts);
        let s1 = s1.clone();
        async move {
            hosts
                .observe_sheds(std::slice::from_ref(&s1), &["srv".to_string()])
                .await;
        }
    });
    reached.notified().await;
    let second = tokio::spawn({
        let hosts = Arc::clone(&hosts);
        let s2 = s2.clone();
        async move {
            hosts
                .observe_sheds(std::slice::from_ref(&s2), &["srv".to_string()])
                .await;
        }
    });
    proceed.notify_one();
    first.await.unwrap();
    second.await.unwrap();

    let listed = lock(&hosts.listed_sheds).clone();
    assert!(!listed.contains(&s1));
    assert!(listed.contains(&s2));
}

/// A `craze.reopen` whose host was removed & re-registered while in flight
/// answers `superseded: true` and never pollutes the new incarnation's rows.
#[tokio::test]
async fn reopen_outliving_host_incarnation_is_superseded() {
    let s1 = shed("srv", "s1");
    let cfg = config_with_servers(&["srv"]);
    let craze1 = Scripted::eager();
    let craze2 = Scripted::eager();
    let hosts = start_queued(&cfg, &s1, vec![Arc::clone(&craze1), Arc::clone(&craze2)]);

    hosts
        .observe_sheds(std::slice::from_ref(&s1), &["srv".to_string()])
        .await;
    let _hub1 = craze1.reopen_roster(json!([])).await;
    wait_for("gen1 live", || {
        status_of(&hosts, &s1).filter(|s| s["craze"]["state"] == "live")
    })
    .await;

    // Start reopen on gen1 and pause after receiving its RPC request.
    let reopen_fut = tokio::spawn({
        let hosts = Arc::clone(&hosts);
        let s1 = s1.clone();
        async move { hosts.craze_reopen(&s1, "saved-old").await }
    });
    let mut reopen_conn = craze1.next().await;
    reopen_conn.hello(EPOCH, reopen_hub_capabilities()).await;
    let req = reopen_conn.expect("session.reopen").await;

    // Remove and re-register s1 on gen2 while the gen1 reopen is still in flight.
    // Note: `craze1` still has a handle held by `reopen_fut`, so `remove` runs
    // cleanly while we re-observe after replying or directly via detach.
    hosts.detach(&s1).await;
    hosts
        .observe_sheds(std::slice::from_ref(&s1), &["srv".to_string()])
        .await;
    let _hub2 = craze2.reopen_roster(json!([])).await;
    wait_for("gen2 live", || {
        status_of(&hosts, &s1).filter(|s| s["craze"]["state"] == "live")
    })
    .await;

    // Now answer gen1's in-flight reopen.
    reopen_conn
        .reply(
            &req,
            json!({
                "session": row(HOST_A, "psid-old"),
                "joined": false,
            }),
        )
        .await;
    let answer = reopen_fut.await.unwrap().expect("reopen returns ok");
    assert_eq!(answer["superseded"], true);
    assert!(
        craze_rows(&hosts).is_empty(),
        "superseded reopen must not insert a row into gen2"
    );
}
