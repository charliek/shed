//! A craze session's SETTINGS against the REAL hub — craze's hermetic recipe
//! (`shed_craze::testing::Recipe`) with a hub-created `craze serve` running
//! `craze-fake-agent`'s `permodel` script as `cursor` (plan 025 §3.10, C11).
//!
//! `permodel` is cursor as it answers today: four models, each with an option
//! catalog of its OWN (grok-4.6: effort and fast; composer-2.5: fast alone;
//! claude-opus-5: thinking, context, effort and fast; glm-5.2: a reasoning
//! select), the mode and the model as options of their own, and the three
//! modes. It speaks cursor's auth, so it runs as `cursor` (`[agents]` makes
//! cursor ready on any machine); the hub reads `config.toml` at each create.
//! Its wrapper sets `CRAZE_FAKE_DUMP_CALLS`, the agent's own record of every
//! message it read — `session/set_config_option <id>=<value>` per set — so a
//! cell can count what craze actually asked the agent to change.
//!
//! Every cell skips with a message without `SHED_CRAZE_BIN_DIR` and FAILS
//! instead under `SHED_CRAZE_REQUIRE=1` (CI). Every frame goes through the
//! contract's conformance kit; every lane is opened through a source, as both
//! clients open them.

use std::path::{Path, PathBuf};
use std::sync::Arc;
use std::time::Duration;

use shed_core::lane::conformance::{drive_lane, Drive, DriveEnd, LaneChecker};
use shed_core::lane::{
    AgentLane, AgentSource, LaneCreateRequest, LaneError, LaneEvent, LaneSettingChange,
    LaneSettings, SourceEvent,
};
use shed_craze::testing::{bins, HookDial, Recipe, RECIPE_WAIT};
use shed_craze::{is_outcome_unknown, new_request_id, CrazeDial, CrazeSource};
use tokio::sync::mpsc::Receiver;

async fn drive(
    rx: &mut Receiver<LaneEvent>,
    checker: &mut LaneChecker,
    what: &str,
    until: impl FnMut(&LaneEvent) -> bool,
) -> Vec<LaneEvent> {
    let d: Drive<LaneEvent> = drive_lane(rx, checker, RECIPE_WAIT, until)
        .await
        .unwrap_or_else(|v| panic!("{what}: the stream broke a contract rule: {v}"));
    assert_eq!(
        d.end,
        DriveEnd::Matched,
        "{what}: not seen within {RECIPE_WAIT:?}: {:#?}",
        d.frames
    );
    d.frames
}

fn is_ready(e: &LaneEvent) -> bool {
    matches!(e, LaneEvent::Ready { .. })
}

/// The last `Settings` among `frames`.
fn last_settings(frames: &[LaneEvent]) -> Option<LaneSettings> {
    frames.iter().rev().find_map(|e| match e {
        LaneEvent::Settings { settings } => Some(settings.clone()),
        _ => None,
    })
}

/// Drive until a `Settings` satisfying `want` arrives; it.
async fn settings_where(
    rx: &mut Receiver<LaneEvent>,
    checker: &mut LaneChecker,
    what: &str,
    mut want: impl FnMut(&LaneSettings) -> bool,
) -> LaneSettings {
    let frames = drive(
        rx,
        checker,
        what,
        |e| matches!(e, LaneEvent::Settings { settings } if want(settings)),
    )
    .await;
    last_settings(&frames).unwrap()
}

fn ids<T>(items: &[T], id: impl Fn(&T) -> &str) -> Vec<String> {
    items.iter().map(|i| id(i).to_string()).collect()
}

fn option_current(s: &LaneSettings, id: &str) -> Option<String> {
    s.options
        .iter()
        .find(|o| o.id == id)
        .map(|o| o.current.clone())
}

/// The recipe with `permodel` as `cursor` (grok, the default, stays the echo
/// agent), and the agent's call record.
fn permodel(recipe: &Recipe) -> PathBuf {
    let calls = recipe.root().join("permodel-calls");
    let agent = recipe.script_agent_with(
        "permodel",
        &[("CRAZE_FAKE_DUMP_CALLS", &calls.to_string_lossy())],
    );
    let echo = recipe.script_agent("grok-echo");
    recipe.set_agents(&[("grok", &echo), ("cursor", &agent)]);
    calls
}

/// The config sets the agent was asked for, in order (`<id>=<value>`).
fn agent_sets(calls: &Path) -> Vec<String> {
    std::fs::read_to_string(calls)
        .unwrap_or_default()
        .lines()
        .filter_map(|l| l.strip_prefix("session/set_config_option "))
        .map(str::to_string)
        .collect()
}

/// A cursor session the hub creates (no prompt), its hostId.
async fn create_cursor(source: &CrazeSource, recipe: &Recipe) -> String {
    source
        .create(LaneCreateRequest {
            cwd: recipe.work.to_string_lossy().into_owned(),
            provider: Some("cursor".into()),
            prompt: None,
            request_id: new_request_id(),
        })
        .await
        .unwrap()
        .session
        .id
}

/// A source subscribed until it lists `host_id`, and a lane opened on that
/// row through it (the subscription returned so it outlives the cell).
async fn lane_on(
    source: &CrazeSource,
    host_id: &str,
) -> (Arc<dyn AgentLane>, shed_core::lane::SourceSubscription) {
    let mut sub = source.subscribe().await.unwrap();
    let deadline = tokio::time::Instant::now() + RECIPE_WAIT;
    loop {
        let ev = tokio::time::timeout_at(deadline, sub.rx.recv())
            .await
            .unwrap_or_else(|_| panic!("the roster never listed {host_id}"))
            .expect("the roster stream is alive");
        if matches!(&ev, SourceEvent::Session { session } if session.id == host_id) {
            break;
        }
    }
    let lane = source.open(host_id).await.unwrap();
    (lane, sub)
}

/// A lane's subscription, seeded; its settings as the seed carried them.
async fn seeded(
    lane: &Arc<dyn AgentLane>,
) -> (
    Receiver<LaneEvent>,
    shed_core::lane::LaneStop,
    LaneChecker,
    LaneSettings,
) {
    let (mut rx, stop) = lane.subscribe(None).await.unwrap().into_parts();
    let mut checker = LaneChecker::new();
    let seed = drive(&mut rx, &mut checker, "the seed", is_ready).await;
    let caps = seed
        .iter()
        .rev()
        .find_map(|e| match e {
            LaneEvent::Capabilities { capabilities } => Some(capabilities.clone()),
            _ => None,
        })
        .expect("the seed's Capabilities");
    assert!(caps.settings, "a per-model session has settings: {caps:?}");
    let s = last_settings(&seed).expect("the seed's Settings");
    (rx, stop, checker, s)
}

/// **The settings, in craze's order, and a model change redraws them** (plan
/// 025 §3.10): the current model first, then the rest in the catalog's order
/// (no model is remembered on an ACP session); the options are the CURRENT
/// model's — the model and mode rows excluded — `thought_level` first, then
/// `model_config`, each in the provider's order; the modes as the catalog
/// lists them. A model change re-emits `Settings` with the new model's own
/// options (claude-opus-5's four, reordered); a config change and a mode
/// change each come back the same way.
#[tokio::test(flavor = "multi_thread")]
async fn the_settings_follow_crazes_order_and_a_model_change_redraws_the_options() {
    let Some(bins) =
        bins("the_settings_follow_crazes_order_and_a_model_change_redraws_the_options")
    else {
        return;
    };
    let recipe = Recipe::start(&bins);
    let calls = permodel(&recipe);
    {
        let source = recipe.source();
        let host = create_cursor(&source, &recipe).await;
        let lane = source.open(&host).await.unwrap();
        let (mut rx, _stop, mut checker, s) = seeded(&lane).await;
        assert_eq!(s.model.as_deref(), Some("grok-4.6"));
        assert_eq!(
            ids(&s.models, |m| &m.id),
            ["grok-4.6", "composer-2.5", "claude-opus-5", "glm-5.2"]
        );
        assert_eq!(
            ids(&s.options, |o| &o.id),
            ["effort", "fast"],
            "the model and mode rows are rows of their own"
        );
        assert_eq!(ids(&s.modes, |m| &m.id), ["agent", "plan", "ask"]);
        assert_eq!(s.mode.as_deref(), Some("agent"));
        assert_eq!(s.usage, None, "an ACP session reports no usage");

        lane.set(LaneSettingChange::Model {
            id: "claude-opus-5".into(),
        })
        .await
        .unwrap();
        let s = settings_where(&mut rx, &mut checker, "the model's Settings", |s| {
            s.model.as_deref() == Some("claude-opus-5")
        })
        .await;
        assert_eq!(
            ids(&s.models, |m| &m.id),
            ["claude-opus-5", "grok-4.6", "composer-2.5", "glm-5.2"],
            "the current model first, the rest in catalog order"
        );
        assert_eq!(
            ids(&s.options, |o| &o.id),
            ["thinking", "effort", "context", "fast"],
            "claude-opus-5's own options: thought_level, then model_config, each in cursor's order"
        );
        let effort = s.options.iter().find(|o| o.id == "effort").unwrap();
        assert_eq!(
            ids(&effort.values, |v| &v.id),
            ["low", "medium", "high", "xhigh", "max"],
            "a select's values keep the provider's order"
        );

        lane.set(LaneSettingChange::Config {
            id: "effort".into(),
            value: "low".into(),
            for_model: None,
        })
        .await
        .unwrap();
        settings_where(&mut rx, &mut checker, "the option's Settings", |s| {
            option_current(s, "effort").as_deref() == Some("low")
        })
        .await;
        assert_eq!(agent_sets(&calls), ["model=claude-opus-5", "effort=low"]);

        lane.set(LaneSettingChange::Mode { id: "plan".into() })
            .await
            .unwrap();
        settings_where(&mut rx, &mut checker, "the mode's Settings", |s| {
            s.mode.as_deref() == Some("plan")
        })
        .await;

        lane.set(LaneSettingChange::Model {
            id: "composer-2.5".into(),
        })
        .await
        .unwrap();
        let s = settings_where(&mut rx, &mut checker, "composer's Settings", |s| {
            s.model.as_deref() == Some("composer-2.5")
        })
        .await;
        assert_eq!(
            ids(&s.options, |o| &o.id),
            ["fast"],
            "a model with no effort offers none"
        );
    }
    recipe.teardown().await.unwrap();
}

/// **A config change is bound to the model it was chosen for** (plan 025
/// §3.10, `forModel`): a lane whose reads are held still shows grok-4.6 when a
/// second client moves the session to composer-2.5; its `fast` change — an
/// option composer-2.5 ALSO has — is refused `stale_model` (`NotAccepting`)
/// and never reaches the agent, rather than being applied to a model nobody
/// chose it for. The retry, once the lane has folded the move, is a new
/// command bound to composer-2.5, and takes. And once the lane's fold shows
/// composer-2.5, a change whose client still DISPLAYS grok-4.6 (its
/// `for_model`, Amendment A13) is bound to grok-4.6 and refused the same way.
#[tokio::test(flavor = "multi_thread")]
async fn a_config_change_is_bound_to_the_model_it_was_chosen_for() {
    let Some(bins) = bins("a_config_change_is_bound_to_the_model_it_was_chosen_for") else {
        return;
    };
    let recipe = Recipe::start(&bins);
    let calls = permodel(&recipe);
    {
        let source = recipe.source();
        let host = create_cursor(&source, &recipe).await;
        // The second client: its own lane, on the creating source.
        let other = source.open(&host).await.unwrap();
        let (_orx, _ostop, _ochecker, _) = seeded(&other).await;
        // The lane under test, on a dial whose reads can be held.
        let hook = HookDial::new(Arc::new(recipe.dial()));
        let held = recipe.source_on(Arc::clone(&hook) as Arc<dyn CrazeDial>);
        let (lane, _sub) = lane_on(&held, &host).await;
        let (mut rx, _stop, mut checker, s) = seeded(&lane).await;
        assert_eq!(option_current(&s, "fast").as_deref(), Some("true"));

        hook.hold_reads();
        other
            .set(LaneSettingChange::Model {
                id: "composer-2.5".into(),
            })
            .await
            .unwrap();
        let l = Arc::clone(&lane);
        let chosen_on_grok = tokio::spawn(async move {
            l.set(LaneSettingChange::Config {
                id: "fast".into(),
                value: "true".into(),
                for_model: None,
            })
            .await
        });
        // Let the request leave before the lane can fold the move.
        tokio::time::sleep(Duration::from_millis(500)).await;
        hook.release_reads();
        let got = chosen_on_grok.await.unwrap();
        assert_eq!(
            got,
            Err(LaneError::NotAccepting),
            "stale_model: the session left the model the change was chosen for"
        );
        assert!(
            !agent_sets(&calls).iter().any(|c| c == "fast=true"),
            "a change chosen for grok-4.6 reached the agent on composer-2.5: {:?}",
            agent_sets(&calls)
        );

        settings_where(&mut rx, &mut checker, "the move, folded", |s| {
            s.model.as_deref() == Some("composer-2.5")
        })
        .await;
        lane.set(LaneSettingChange::Config {
            id: "fast".into(),
            value: "true".into(),
            for_model: None,
        })
        .await
        .expect("the retry is bound to the model the lane now shows");
        settings_where(&mut rx, &mut checker, "composer's fast on", |s| {
            option_current(s, "fast").as_deref() == Some("true")
        })
        .await;
        assert_eq!(
            agent_sets(&calls)
                .iter()
                .filter(|c| *c == "fast=true")
                .count(),
            1
        );

        // Amendment A13: the lane's fold shows composer-2.5, but a client
        // still DISPLAYING grok-4.6 chose this option there — it is bound to
        // grok-4.6, and craze refuses it rather than apply it to composer.
        let got = lane
            .set(LaneSettingChange::Config {
                id: "fast".into(),
                value: "false".into(),
                for_model: Some("grok-4.6".into()),
            })
            .await;
        assert_eq!(
            got,
            Err(LaneError::NotAccepting),
            "bound to the model the client displayed, not the fold's"
        );
        assert!(
            !agent_sets(&calls).iter().any(|c| c == "fast=false"),
            "an option chosen on grok-4.6 reached the agent on composer-2.5: {:?}",
            agent_sets(&calls)
        );
    }
    recipe.teardown().await.unwrap();
}

/// **A set lost to a drop ran once, and the next `Settings` says so** (plan
/// 025 §3.10): the lane's bridge is cut once craze has handed the change to
/// the agent, before its answer is read — the call is "outcome unknown"; the
/// lane resumes silently; the resume's `Settings` (ahead of its lone `Ready`)
/// shows the change, and the agent was asked for it exactly once.
#[tokio::test(flavor = "multi_thread")]
async fn a_set_lost_to_a_drop_ran_once_and_the_next_settings_say_so() {
    let Some(bins) = bins("a_set_lost_to_a_drop_ran_once_and_the_next_settings_say_so") else {
        return;
    };
    let recipe = Recipe::start(&bins);
    let calls = permodel(&recipe);
    {
        let source = recipe.source();
        let host = create_cursor(&source, &recipe).await;
        let hook = HookDial::new(Arc::new(recipe.dial()));
        let hooked = recipe.source_on(Arc::clone(&hook) as Arc<dyn CrazeDial>);
        let (lane, _sub) = lane_on(&hooked, &host).await;
        // Armed now, so the lane's own connection — its next dial — takes it.
        let mut cut = hook.sever_after("\"method\":\"session.set\"");
        let (mut rx, _stop, mut checker, s) = seeded(&lane).await;
        assert_eq!(option_current(&s, "effort").as_deref(), Some("high"));

        let l = Arc::clone(&lane);
        let lost = tokio::spawn(async move {
            l.set(LaneSettingChange::Config {
                id: "effort".into(),
                value: "low".into(),
                for_model: None,
            })
            .await
        });
        cut.written(RECIPE_WAIT).await;
        let deadline = tokio::time::Instant::now() + RECIPE_WAIT;
        while !agent_sets(&calls).iter().any(|c| c == "effort=low") {
            assert!(
                tokio::time::Instant::now() < deadline,
                "craze never asked the agent: {:?}",
                agent_sets(&calls)
            );
            tokio::time::sleep(Duration::from_millis(50)).await;
        }
        cut.sever();
        let got = lost.await.unwrap().unwrap_err();
        assert!(is_outcome_unknown(&got), "{got:?}");

        let resumed = drive(&mut rx, &mut checker, "the resume's Ready", is_ready).await;
        assert!(
            !resumed.iter().any(|e| matches!(e, LaneEvent::Reset { .. })),
            "a silent resume: {resumed:#?}"
        );
        let s = last_settings(&resumed).expect("the resume's Settings");
        assert_eq!(
            option_current(&s, "effort").as_deref(),
            Some("low"),
            "the change took, and the resume says so"
        );
        tokio::time::sleep(Duration::from_millis(500)).await;
        assert_eq!(
            agent_sets(&calls)
                .iter()
                .filter(|c| *c == "effort=low")
                .count(),
            1,
            "never resent: {:?}",
            agent_sets(&calls)
        );
    }
    recipe.teardown().await.unwrap();
}
