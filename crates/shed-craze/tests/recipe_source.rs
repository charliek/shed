//! `CrazeSource` against the REAL hub — craze's hermetic recipe
//! (`shed_craze::testing::Recipe`): the hub a bridge births, `craze-fake-host`
//! registry entries, creates spawning `craze-fake-agent` as `grok`, every
//! craze process under the recipe's six variables (plan 025 §3.3.8, the
//! source half).
//!
//! Every cell skips with a message without `SHED_CRAZE_BIN_DIR`, and FAILS
//! instead under `SHED_CRAZE_REQUIRE=1` (CI). Every frame a source emits here
//! goes through the contract's conformance kit.

use std::sync::Arc;
use std::time::Duration;

use serde_json::{json, Value};
use shed_core::lane::conformance::{
    drive_lane, drive_source, Drive, DriveEnd, LaneChecker, SourceChecker,
};
use shed_core::lane::{
    AgentSource, LaneCreateRequest, LaneError, LaneEvent, LanePromptOutcome, LaneProviderState,
    LaneReopenError, LaneSavedSession, LaneSavedSessions, LaneSession, SourceEvent, SourceOffline,
};
use shed_core::rc::RcActivity;
use shed_craze::testing::{bins, saved_index_row, HookDial, PathKind, Recipe, RECIPE_WAIT};
use shed_craze::{is_outcome_unknown, new_request_id, CrazeDial, Probe};
use tokio::sync::mpsc::Receiver;

const FAKE: &str = "0a0a0a0a0a0a";
const FAKE_SESSION: &str = "recipe-fake";

async fn drive(
    rx: &mut Receiver<SourceEvent>,
    checker: &mut SourceChecker,
    what: &str,
    until: impl FnMut(&SourceEvent) -> bool,
) -> Drive<SourceEvent> {
    let d = drive_source(rx, checker, RECIPE_WAIT, until)
        .await
        .unwrap_or_else(|v| panic!("{what}: the stream broke a contract rule: {v}"));
    assert_eq!(
        d.end,
        DriveEnd::Matched,
        "{what}: not seen within {RECIPE_WAIT:?}: {:?}",
        d.frames
    );
    d
}

fn is_ready(e: &SourceEvent) -> bool {
    matches!(e, SourceEvent::Ready { .. })
}

fn sessions(frames: &[SourceEvent]) -> Vec<&LaneSession> {
    frames
        .iter()
        .filter_map(|e| match e {
            SourceEvent::Session { session } => Some(session),
            _ => None,
        })
        .collect()
}

fn permission(id: &str, tool: &str) -> Value {
    json!({"name": "permission", "id": id, "tool": tool,
           "options": [{"optionId": "allow", "name": "Allow", "kind": "allow_once"}]})
}

fn request(recipe: &Recipe, prompt: Option<&str>, id: &str) -> LaneCreateRequest {
    LaneCreateRequest {
        cwd: recipe.work.to_string_lossy().into_owned(),
        provider: None,
        prompt: prompt.map(str::to_string),
        request_id: id.to_string(),
        title: None,
    }
}

/// The roster through the fake host's ops: the seed lists it, an ask opening
/// upserts it, a second fake host appearing upserts it, the first leaving the
/// registry removes it — by hostId, through the conformance kit.
#[tokio::test(flavor = "multi_thread")]
async fn the_roster_seeds_upserts_and_removes() {
    let Some(bins) = bins("the_roster_seeds_upserts_and_removes") else {
        return;
    };
    let recipe = Recipe::start(&bins);
    let ready = recipe.fake_host(FAKE, FAKE_SESSION).await;
    assert_eq!(ready["hostId"], FAKE);
    {
        let (mut rx, _stop) = recipe.source().subscribe().await.unwrap().into_parts();
        let mut checker = SourceChecker::new();
        let seed = drive(&mut rx, &mut checker, "the seed", is_ready).await;
        let rows = sessions(&seed.frames);
        assert_eq!(rows.len(), 1, "{:?}", seed.frames);
        assert_eq!(rows[0].id, FAKE, "a row's id is its hostId (P11)");
        assert_eq!(rows[0].activity, RcActivity::Idle);
        assert!(seed
            .frames
            .iter()
            .any(|e| matches!(e, SourceEvent::Capabilities { capabilities }
            if capabilities.kind == "craze" && capabilities.create && capabilities.create_options
                && capabilities.reopen)));

        recipe.op(FAKE, &permission("perm-1", "Shell")).await;
        let up = drive(
            &mut rx,
            &mut checker,
            "the ask's upsert",
            |e| matches!(e, SourceEvent::Session { session } if session.pending_approvals == 1),
        )
        .await;
        let row = sessions(&up.frames).pop().unwrap().clone();
        assert_eq!(
            (row.id.as_str(), row.activity),
            (FAKE, RcActivity::NeedsApproval)
        );
        assert_eq!(row.head_ask_summary.as_deref(), Some("permission Shell"));

        recipe.fake_host("0b0b0b0b0b0b", "recipe-fake-2").await;
        drive(
            &mut rx,
            &mut checker,
            "the second host's upsert",
            |e| matches!(e, SourceEvent::Session { session } if session.id == "0b0b0b0b0b0b"),
        )
        .await;

        recipe.op(FAKE, &json!({"name": "unlist"})).await;
        drive(
            &mut rx,
            &mut checker,
            "the removal",
            |e| matches!(e, SourceEvent::Removed { session_id } if session_id == FAKE),
        )
        .await;
        assert_eq!(checker.offlines(), 0);
    }
    recipe.teardown().await.unwrap();
}

/// `hub_closing` (SIGTERM to the recipe's own hub, by its recorded pid):
/// `Offline{Unreachable}`, then a redial that births a new hub and reseeds the
/// SAME row id under a new generation.
#[tokio::test(flavor = "multi_thread")]
async fn hub_closing_is_offline_then_a_reseed_under_a_new_hub() {
    let Some(bins) = bins("hub_closing_is_offline_then_a_reseed_under_a_new_hub") else {
        return;
    };
    let recipe = Recipe::start(&bins);
    recipe.fake_host(FAKE, FAKE_SESSION).await;
    {
        let (mut rx, _stop) = recipe.source().subscribe().await.unwrap().into_parts();
        let mut checker = SourceChecker::new();
        drive(&mut rx, &mut checker, "the seed", is_ready).await;
        let old_hub = recipe.sigterm_hub();
        let lost = drive(&mut rx, &mut checker, "the Offline", |e| {
            matches!(e, SourceEvent::Offline { .. })
        })
        .await;
        assert!(
            matches!(
                lost.frames.last(),
                Some(SourceEvent::Offline {
                    cause: SourceOffline::Unreachable,
                    ..
                })
            ),
            "{:?}",
            lost.frames
        );
        let reseed = drive(&mut rx, &mut checker, "the reseed", is_ready).await;
        assert!(checker.live_generation() >= Some(2));
        let ids: Vec<&str> = sessions(&reseed.frames)
            .iter()
            .map(|s| s.id.as_str())
            .collect();
        assert_eq!(ids, [FAKE], "the same row id under the new epoch");
        let new_hubs = recipe.hub_pids();
        assert_eq!(new_hubs.len(), 1);
        assert_ne!(new_hubs[0], old_hub, "a new hub");
    }
    recipe.teardown().await.unwrap();
}

/// `slow_consumer`: the source stops reading (a hook holds its connection's
/// reads) while sixteen fake hosts' big rows churn, until the hub's roster
/// write blocks for 10 s and the hub logs that it resets the subscription —
/// after which the reads resume and the source resubscribes ON THE SAME
/// CONNECTION: a fresh `Reset … Ready`, no `Offline`, no second dial.
#[tokio::test(flavor = "multi_thread")]
async fn slow_consumer_resubscribes_on_the_same_connection() {
    let Some(bins) = bins("slow_consumer_resubscribes_on_the_same_connection") else {
        return;
    };
    const HOSTS: usize = 16;
    let recipe = Recipe::start(&bins);
    let ids: Vec<String> = (0..HOSTS).map(|i| format!("{:012x}", 0xc0 + i)).collect();
    // Each row carries a ~13 KB head ask, under craze's 16 KiB host-row bound,
    // so a few hub rounds fill every buffer between the hub and this reader.
    let tool = "T".repeat(13_000);
    for (i, id) in ids.iter().enumerate() {
        recipe.fake_host(id, &format!("slow-{i}")).await;
        recipe.op(id, &permission("p-0", &tool)).await;
    }
    {
        let hook = HookDial::new(Arc::new(recipe.dial()));
        let source = recipe.source_on(Arc::clone(&hook) as Arc<dyn CrazeDial>);
        let (mut rx, _stop) = source.subscribe().await.unwrap().into_parts();
        let mut checker = SourceChecker::new();
        drive(&mut rx, &mut checker, "the seed", is_ready).await;
        assert_eq!(hook.dials(), 1);

        // Hold the reads, and churn the rows, UNTIL THE HUB SAYS it has reset
        // the subscription — not for a fixed time. The buffers between the hub
        // and this reader (the hub's socket, the bridge's pump, its stdout
        // pipe: ~300 KB on this Linux host, other sizes elsewhere) fill after
        // some seconds of churn; the hub's roster write then blocks for its
        // 10 s (`slowWait`), it logs "…reset slow_consumer" to its own log
        // (craze `internal/hub/server.go:672`) and only then writes the rest of
        // the line and the reset — with 10 s more (`writeWait`) before it
        // closes the connection. The log line is the deterministic signal:
        // released within one 400 ms churn round of it, the reads always land
        // inside that 10 s, whatever the buffering. 60 s is a generous upper
        // bound on the fill (measured here: about 3 s).
        hook.hold_reads();
        let start = tokio::time::Instant::now();
        let mut n = 1;
        let signalled = loop {
            if recipe.hub_log().contains("reset slow_consumer") {
                break true;
            }
            if start.elapsed() >= Duration::from_secs(60) {
                break false;
            }
            for id in &ids {
                recipe.op(id, &permission(&format!("p-{n}"), "Shell")).await;
            }
            n += 1;
            tokio::time::sleep(Duration::from_millis(400)).await;
        };
        hook.release_reads();
        assert!(
            signalled,
            "the hub never reported a slow consumer within 60 s of held reads; its log:\n{}",
            recipe.hub_log()
        );
        let resub = drive(
            &mut rx,
            &mut checker,
            "the reset's resubscribe",
            |e| matches!(e, SourceEvent::Reset { generation, .. } if *generation >= 2),
        )
        .await;
        assert!(
            matches!(resub.frames.last(), Some(SourceEvent::Reset { reason, .. }) if reason.contains("slow_consumer")),
            "{:?}",
            resub.frames.last()
        );
        let reseed = drive(&mut rx, &mut checker, "the reseed", is_ready).await;
        assert_eq!(sessions(&reseed.frames).len(), HOSTS);
        assert_eq!(
            checker.offlines(),
            0,
            "never offline: the connection stayed"
        );
        assert_eq!(hook.dials(), 1, "the same connection");
    }
    recipe.teardown().await.unwrap();
}

/// What a create can start, from the real hub — craze's recipe's answer
/// exactly: cursor unavailable (not found), grok ready, native needs setup,
/// grok the default — through the direct dial and through the jailed ladder
/// alike. And the find-only probe: dormant before any hub, a hub after.
#[tokio::test(flavor = "multi_thread")]
async fn create_options_and_the_probe_from_the_real_hub() {
    let Some(bins) = bins("create_options_and_the_probe_from_the_real_hub") else {
        return;
    };
    let recipe = Recipe::start(&bins);
    assert_eq!(
        recipe.probe().await,
        Probe::NoHub,
        "dormant: nothing has started a hub"
    );
    assert!(recipe.hub_pids().is_empty(), "and the probe started none");

    let options = recipe.source().create_options().await.unwrap();
    let states: Vec<(&str, LaneProviderState)> = options
        .providers
        .iter()
        .map(|p| (p.id.as_str(), p.state.clone()))
        .collect();
    assert_eq!(
        states,
        [
            ("cursor", LaneProviderState::Unavailable),
            ("grok", LaneProviderState::Ready),
            ("native", LaneProviderState::NeedsSetup)
        ]
    );
    assert_eq!(
        options.providers[0].reason.as_deref(),
        Some("cursor-agent not found on PATH")
    );
    assert_eq!(
        options.providers[0].fix.as_deref(),
        Some(
            format!(
                "install cursor-agent, or set [agents].cursor in {}",
                recipe.config_path().display()
            )
            .as_str()
        )
    );
    assert_eq!(
        options.providers[2].reason.as_deref(),
        Some("no model provider has a key")
    );
    assert_eq!(options.default_provider.as_deref(), Some("grok"));
    assert!(options.recent_dirs.is_empty());

    let via_ladder = recipe
        .source_on(Arc::new(recipe.ladder_dial()))
        .create_options()
        .await
        .unwrap();
    assert_eq!(
        via_ladder, options,
        "the production-shaped ladder reaches the same hub"
    );
    assert_eq!(recipe.probe().await, Probe::Hub);
    recipe.teardown().await.unwrap();
}

/// A create with a first prompt: accepted, and the roster subscription is told
/// of the new row (its id the create's new hostId). The same `requestId` again
/// is answered with the SAME session.
#[tokio::test(flavor = "multi_thread")]
async fn a_create_is_accepted_upserted_and_idempotent_by_request_id() {
    let Some(bins) = bins("a_create_is_accepted_upserted_and_idempotent_by_request_id") else {
        return;
    };
    let recipe = Recipe::start(&bins);
    {
        let source = recipe.source();
        let (mut rx, _stop) = source.subscribe().await.unwrap().into_parts();
        let mut checker = SourceChecker::new();
        let seed = drive(&mut rx, &mut checker, "the empty seed", is_ready).await;
        assert!(sessions(&seed.frames).is_empty());

        let id = new_request_id();
        let created = source
            .create(request(&recipe, Some("hello recipe"), &id))
            .await
            .unwrap();
        assert_eq!(created.prompt, LanePromptOutcome::Accepted, "{created:?}");
        assert_eq!(
            created.session.provider.as_deref(),
            Some("grok"),
            "the default provider"
        );
        assert_eq!(created.session.cwd, recipe.work.to_string_lossy());
        let host = created.session.id.clone();
        drive(
            &mut rx,
            &mut checker,
            "the new row's upsert",
            |e| matches!(e, SourceEvent::Session { session } if session.id == host),
        )
        .await;

        let again = source
            .create(request(&recipe, Some("hello recipe"), &id))
            .await
            .unwrap();
        assert_eq!(
            again.session.id, host,
            "the same requestId is the same session"
        );
        assert_eq!(
            recipe.registered_hosts(),
            std::slice::from_ref(&host),
            "exactly one session"
        );
    }
    recipe.teardown().await.unwrap();
}

/// The bridge is killed after `session.create` is written — the hub has the
/// create, the client never sees its answer: an UNKNOWN outcome, retried once
/// under the same `requestId` on a fresh connection, which craze answers with
/// the session the first create started. Exactly one session.
#[tokio::test(flavor = "multi_thread")]
async fn a_bridge_killed_after_the_create_is_written_retries_into_one_session() {
    let Some(bins) = bins("a_bridge_killed_after_the_create_is_written_retries_into_one_session")
    else {
        return;
    };
    let recipe = Recipe::start(&bins);
    {
        let hook = HookDial::new(Arc::new(recipe.dial()));
        let source = recipe.source_on(Arc::clone(&hook) as Arc<dyn CrazeDial>);
        let mut cut = hook.sever_after("\"method\":\"session.create\"");
        let id = new_request_id();
        let req = request(&recipe, Some("hello recipe"), &id);
        let s = source.clone();
        let create = tokio::spawn(async move { s.create(req).await });
        cut.written(RECIPE_WAIT).await;
        // Proof the hub has it: the host it spawns is listed.
        let deadline = tokio::time::Instant::now() + RECIPE_WAIT;
        while recipe.registered_hosts().is_empty() {
            assert!(
                tokio::time::Instant::now() < deadline,
                "the hub never started the create's host"
            );
            tokio::time::sleep(Duration::from_millis(50)).await;
        }
        cut.sever();
        let created = create
            .await
            .unwrap()
            .expect("the retry answered with the session");
        assert_eq!(hook.dials(), 2, "the first connection, then the one retry");
        assert_eq!(
            recipe.registered_hosts(),
            std::slice::from_ref(&created.session.id),
            "exactly one session"
        );
    }
    recipe.teardown().await.unwrap();
}

/// A definite failure — the agent dies at its start with two lines, as cursor
/// does on a locked keychain — is `Failed(<craze's cause>)` (P14), and that
/// requestId stays answered with it; once `[agents]` is fixed, a NEW id
/// starts a session.
#[tokio::test(flavor = "multi_thread")]
async fn a_start_failure_says_why_and_a_new_id_succeeds_once_fixed() {
    let Some(bins) = bins("a_start_failure_says_why_and_a_new_id_succeeds_once_fixed") else {
        return;
    };
    let recipe = Recipe::start(&bins);
    let source = recipe.source();
    recipe.set_grok_agent(&recipe.exit_two_lines_agent());
    let failed_id = new_request_id();
    let err = source
        .create(request(&recipe, Some("hi"), &failed_id))
        .await
        .unwrap_err();
    let LaneError::Failed(cause) = &err else {
        panic!("{err:?}")
    };
    assert!(
        cause.contains("KEYCHAIN LOCKED"),
        "craze's cause reaches the caller: {cause}"
    );
    assert!(
        !is_outcome_unknown(&err),
        "a definite answer: the id's life is over"
    );
    // craze keeps that answer for the id, failure included.
    assert_eq!(
        source
            .create(request(&recipe, Some("hi"), &failed_id))
            .await
            .unwrap_err(),
        err
    );

    recipe.set_grok_agent(&bins.fake_agent);
    let created = source
        .create(request(&recipe, Some("hi"), &new_request_id()))
        .await
        .unwrap();
    assert_eq!(created.prompt, LanePromptOutcome::Accepted);
    recipe.teardown().await.unwrap();
}

// ---- a held request id, replayed (plan 026 §3.1) ----

/// **A held replay whose dial never reaches craze stays unknown** (no craze
/// on the `PATH`, through the ladder): a FRESH create there is a definite,
/// quiet `Unavailable` (nothing was sent), but a replay of a held id says
/// nothing about the request it replays — both of its attempts are retries,
/// and the caller keeps the id.
#[tokio::test(flavor = "multi_thread")]
async fn a_held_create_whose_dial_never_reaches_craze_stays_unknown() {
    let Some(bins) = bins("a_held_create_whose_dial_never_reaches_craze_stays_unknown") else {
        return;
    };
    let recipe = Recipe::start_with(&bins, PathKind::Empty);
    {
        let hook = HookDial::new(Arc::new(recipe.ladder_dial()));
        let source = recipe.source_on(Arc::clone(&hook) as Arc<dyn CrazeDial>);
        let fresh = source
            .create(request(&recipe, None, &new_request_id()))
            .await
            .unwrap_err();
        assert!(
            matches!(&fresh, LaneError::Unavailable(m) if m.contains("not installed")),
            "{fresh:?}"
        );
        assert!(!is_outcome_unknown(&fresh), "a fresh create sent nothing");
        assert_eq!(hook.dials(), 1);

        let held = source
            .create_held(request(&recipe, None, &new_request_id()))
            .await
            .unwrap_err();
        assert!(is_outcome_unknown(&held), "{held:?}");
        assert_eq!(hook.dials(), 3, "the held attempt, then the one retry");
    }
    recipe.teardown().await.unwrap();
}

/// **craze could not tell whether the request started a session** — the REAL
/// hub, a live `craze-fake-host` whose registry entry cannot be read: craze
/// refuses any create its memory does not hold `unavailable`, reason
/// `host_unreachable` ("whether a create of requestId X started a session
/// already cannot be known: try again"), spawning nothing. A fresh create so
/// refused started nothing (definite); a held replay so refused stays
/// UNKNOWN, and the caller keeps its id.
#[tokio::test(flavor = "multi_thread")]
async fn a_held_create_craze_cannot_answer_for_stays_unknown() {
    let Some(bins) = bins("a_held_create_craze_cannot_answer_for_stays_unknown") else {
        return;
    };
    let recipe = Recipe::start(&bins);
    {
        recipe.fake_host(FAKE, FAKE_SESSION).await;
        let entry = recipe.home.join(format!(".cache/craze/hosts/{FAKE}.json"));
        assert!(entry.exists(), "the fake host's registry entry");
        std::fs::write(&entry, "{not json").unwrap();
        let source = recipe.source();

        let fresh = source
            .create(request(&recipe, None, &new_request_id()))
            .await
            .unwrap_err();
        assert!(
            matches!(&fresh, LaneError::Unavailable(m) if m.contains("cannot be read")),
            "a fresh create craze refused: {fresh:?}"
        );
        assert!(!is_outcome_unknown(&fresh));

        let held = source
            .create_held(request(&recipe, None, &new_request_id()))
            .await
            .unwrap_err();
        assert!(is_outcome_unknown(&held), "{held:?}");
        assert!(
            held.to_string().contains("cannot be read"),
            "craze's own words: {held}"
        );
        assert_eq!(
            recipe.registered_hosts(),
            [FAKE.to_string()],
            "craze spawned nothing"
        );
    }
    recipe.teardown().await.unwrap();
}

/// **craze's own answer about the request ends a held id** — the REAL hub: a
/// held replay of a created session is that session (exactly one); one with
/// other params is `request_conflict`, a directory that does not exist is
/// `bad_request`, and a start failure is replayed as itself — each definite,
/// never `outcome_unknown`.
#[tokio::test(flavor = "multi_thread")]
async fn a_held_create_ends_on_crazes_answer_about_the_request() {
    let Some(bins) = bins("a_held_create_ends_on_crazes_answer_about_the_request") else {
        return;
    };
    let recipe = Recipe::start(&bins);
    {
        let source = recipe.source();
        let id = new_request_id();
        let created = source.create(request(&recipe, None, &id)).await.unwrap();
        let replayed = source
            .create_held(request(&recipe, None, &id))
            .await
            .unwrap();
        assert_eq!(replayed.session.id, created.session.id, "the same session");
        assert_eq!(
            recipe.registered_hosts(),
            std::slice::from_ref(&created.session.id),
            "exactly one session"
        );

        let conflict = source
            .create_held(request(&recipe, Some("other params"), &id))
            .await
            .unwrap_err();
        assert!(
            matches!(&conflict, LaneError::BadRequest(m) if m.contains("used already")),
            "request_conflict: {conflict:?}"
        );

        let mut nowhere = request(&recipe, None, &new_request_id());
        nowhere.cwd = recipe
            .root()
            .join("no-such-dir")
            .to_string_lossy()
            .into_owned();
        let bad = source.create_held(nowhere).await.unwrap_err();
        assert!(
            matches!(&bad, LaneError::BadRequest(m) if m.contains("does not exist")),
            "bad_request: {bad:?}"
        );

        recipe.set_grok_agent(&recipe.exit_two_lines_agent());
        let failed_id = new_request_id();
        let failed = source
            .create(request(&recipe, Some("hi"), &failed_id))
            .await
            .unwrap_err();
        let again = source
            .create_held(request(&recipe, Some("hi"), &failed_id))
            .await
            .unwrap_err();
        assert_eq!(again, failed, "the start failure, replayed");
        assert!(
            matches!(&again, LaneError::Failed(c) if c.contains("KEYCHAIN LOCKED")),
            "{again:?}"
        );
        for e in [&conflict, &bad, &again] {
            assert!(!is_outcome_unknown(e), "definite: {e:?}");
        }
        recipe.set_grok_agent(&bins.fake_agent);
    }
    recipe.teardown().await.unwrap();
}

/// No craze on the machine (an empty `PATH`, through the ladder): the roster is
/// `Offline{NotInstalled}`, and so is the probe.
#[tokio::test(flavor = "multi_thread")]
async fn not_installed_is_offline_not_installed() {
    let Some(bins) = bins("not_installed_is_offline_not_installed") else {
        return;
    };
    let recipe = Recipe::start_with(&bins, PathKind::Empty);
    {
        let (mut rx, _stop) = recipe
            .source_on(Arc::new(recipe.ladder_dial()))
            .subscribe()
            .await
            .unwrap()
            .into_parts();
        let mut checker = SourceChecker::new();
        let first = drive(&mut rx, &mut checker, "the Offline", |_| true).await;
        assert!(
            matches!(
                &first.frames[0],
                SourceEvent::Offline {
                    cause: SourceOffline::NotInstalled,
                    ..
                }
            ),
            "{:?}",
            first.frames
        );
    }
    assert!(matches!(
        recipe.probe().await,
        Probe::Offline {
            cause: SourceOffline::NotInstalled,
            ..
        }
    ));
    recipe.teardown().await.unwrap();
}

/// The REAL craze v0.0.1 (`unknown flag: --hub`): `Offline{TooOld}` for the
/// roster form AND for the find-only probe form (plan 025 Amendment A2).
#[tokio::test(flavor = "multi_thread")]
async fn too_old_is_offline_too_old_in_both_forms() {
    let Some(bins) = bins("too_old_is_offline_too_old_in_both_forms") else {
        return;
    };
    let recipe = Recipe::start_with(&bins, PathKind::TooOld);
    {
        let (mut rx, _stop) = recipe
            .source_on(Arc::new(recipe.ladder_dial()))
            .subscribe()
            .await
            .unwrap()
            .into_parts();
        let mut checker = SourceChecker::new();
        let first = drive(&mut rx, &mut checker, "the Offline", |_| true).await;
        assert!(
            matches!(
                &first.frames[0],
                SourceEvent::Offline {
                    cause: SourceOffline::TooOld,
                    ..
                }
            ),
            "{:?}",
            first.frames
        );
    }
    let probe = recipe.probe().await;
    assert!(
        matches!(
            probe,
            Probe::Offline {
                cause: SourceOffline::TooOld,
                ..
            }
        ),
        "{probe:?}"
    );
    recipe.teardown().await.unwrap();
}

/// A teardown cut short — its caller's timeout cancels it mid-way — leaves the
/// cleanup to `Drop`, which still runs it: no process of the recipe's and no
/// file of it is left. A process of the recipe's that ignores SIGTERM (a copy
/// of `sleep` in its private `PATH`, started with TERM ignored, which `exec`
/// keeps) holds the teardown in its bounded wait, so the cut lands mid-way
/// every time rather than racing a teardown that finished.
#[tokio::test(flavor = "multi_thread")]
async fn a_cancelled_teardown_still_cleans_up() {
    let Some(bins) = bins("a_cancelled_teardown_still_cleans_up") else {
        return;
    };
    let recipe = Recipe::start(&bins);
    recipe.fake_host(FAKE, FAKE_SESSION).await;
    // A hub, so the backstop has a real one to end.
    recipe.source().create_options().await.unwrap();
    assert_eq!(recipe.hub_pids().len(), 1);
    let stubborn = recipe.path_dir.join("stubborn");
    std::fs::copy("/bin/sleep", &stubborn).unwrap();
    let mut held = std::process::Command::new("/bin/sh")
        .arg("-c")
        .arg(format!("trap '' TERM; exec '{}' 30", stubborn.display()))
        .spawn()
        .unwrap();
    let root = recipe.root().to_path_buf();
    let path_dir = format!("{}/", recipe.path_dir.display());
    let cut = tokio::time::timeout(Duration::from_secs(1), recipe.teardown()).await;
    assert!(cut.is_err(), "the teardown was cut short");
    let gone = !root.exists();
    let deadline = std::time::Instant::now() + Duration::from_secs(5);
    let left = loop {
        let out = std::process::Command::new("ps")
            .args(["-A", "-ww", "-o", "pid=", "-o", "args="])
            .output()
            .unwrap();
        let left: Vec<String> = String::from_utf8_lossy(&out.stdout)
            .lines()
            .filter(|l| {
                l.trim_start()
                    .split_once(' ')
                    .is_some_and(|(_, a)| a.trim_start().starts_with(&path_dir))
            })
            .map(str::to_string)
            .collect();
        if left.is_empty() || std::time::Instant::now() >= deadline {
            break left;
        }
        std::thread::sleep(Duration::from_millis(100));
    };
    let _ = held.kill();
    let _ = held.wait();
    assert!(gone, "Drop removed {}", root.display());
    assert!(left.is_empty(), "left running: {left:?}");
}

// ---- plan 032 §3.4: saved sessions, reopened — craze's own reopen recipe
// ("Reopening a saved session in the recipe": the session index seeded with a
// saved grok session, the fake agent's `grok-load` replaying every load) ----

/// craze's reopen recipe's saved session: its craze id.
const SAVED_ID: &str = "0192f0aa-6666-7000-8000-0000000reopen";
/// What the fake agent's `grok-load` replays on every `session/load`.
const REPLAYED: &str = "the workspace holds main.py and README.md";

/// The recipe with the grok agent the fake agent's `grok-load` script; a cell
/// seeds the session index ([`Recipe::write_saved_sessions`]) before any
/// bridge starts the hub.
fn reopen_recipe(bins: &shed_craze::testing::Bins) -> Recipe {
    let recipe = Recipe::start(bins);
    recipe.set_grok_agent(&recipe.script_agent("grok-load"));
    recipe
}

/// **A saved session is listed, reopened, joined, and its transcript is
/// back** — craze's reopen cell through the source, against the real hub: the
/// create options say `reopen`; `saved` lists the seeded session by its craze
/// id; `reopen` starts a host that loads it (`joined: false`), whose row the
/// source binds at once — a lane opened on it seeds the replayed answer, and
/// its `history` holds it; running, it is no longer saved; the roster lists
/// it; and a second `reopen` JOINS it (`joined: true`), the same hostId,
/// starting nothing, the roster's row kept.
#[tokio::test(flavor = "multi_thread")]
async fn a_saved_session_is_listed_reopened_joined_and_replayed() {
    let Some(bins) = bins("a_saved_session_is_listed_reopened_joined_and_replayed") else {
        return;
    };
    let recipe = reopen_recipe(&bins);
    recipe.write_saved_sessions(&[saved_index_row(
        SAVED_ID,
        "grok",
        "recipe-saved",
        &recipe.work,
        "a saved session",
        "2026-10-12T10:00:00Z",
    )]);
    {
        let source = recipe.source();
        assert!(
            source.create_options().await.unwrap().reopen,
            "a 0.2.0 hub's hello says reopen"
        );
        let ms = |t: &str| shed_core::time::rfc3339_unix_ms(t).unwrap();
        assert_eq!(
            source.saved().await.unwrap(),
            LaneSavedSessions {
                sessions: vec![LaneSavedSession {
                    id: SAVED_ID.into(),
                    provider: "grok".into(),
                    workspace: recipe.work.to_string_lossy().into_owned(),
                    title: Some("a saved session".into()),
                    updated_unix_ms: ms("2026-10-12T10:00:00Z"),
                    created_unix_ms: Some(ms("2026-10-12T09:00:00Z")),
                }],
                more: false,
            }
        );

        let reopened = source.reopen(SAVED_ID).await.unwrap();
        assert!(!reopened.joined, "the hub started a host to load it");
        assert_eq!(reopened.session.provider.as_deref(), Some("grok"));
        assert_eq!(reopened.session.cwd, recipe.work.to_string_lossy());
        let host = reopened.session.id.clone();
        assert_eq!(
            source.listed(&host).map(|(_, sid)| sid).as_deref(),
            Some(SAVED_ID),
            "the row is the source's at once, its craze id the saved one"
        );

        let lane = source.open(&host).await.unwrap();
        {
            let (mut rx, _stop) = lane.subscribe(None).await.unwrap().into_parts();
            let mut checker = LaneChecker::new();
            let d = drive_lane(&mut rx, &mut checker, RECIPE_WAIT, |e| {
                matches!(e, LaneEvent::Message { message, .. }
                    if message.text.as_deref() == Some(REPLAYED))
            })
            .await
            .unwrap_or_else(|v| panic!("the reopened lane broke a contract rule: {v}"));
            assert_eq!(
                d.end,
                DriveEnd::Matched,
                "the replayed answer, on the reopened lane: {:#?}",
                d.frames
            );
            let history = lane.history(None, 50).await.unwrap();
            assert!(
                history
                    .messages
                    .iter()
                    .any(|m| m.text.as_deref() == Some(REPLAYED)),
                "{:#?}",
                history.messages
            );
        }

        assert_eq!(
            source.saved().await.unwrap(),
            LaneSavedSessions::default(),
            "running, it is no longer saved"
        );

        let (mut rx, _stop) = source.subscribe().await.unwrap().into_parts();
        let mut checker = SourceChecker::new();
        drive(
            &mut rx,
            &mut checker,
            "the roster lists the reopened host",
            |e| matches!(e, SourceEvent::Session { session } if session.id == host),
        )
        .await;
        let again = source.reopen(SAVED_ID).await.unwrap();
        assert!(again.joined, "joined: a host already serves it");
        assert_eq!(again.session.id, host, "the same host");
        assert_eq!(
            recipe.registered_hosts(),
            std::slice::from_ref(&host),
            "exactly one session"
        );
        assert!(
            source.created_rows().is_empty(),
            "the roster speaks for it: no created row beside it"
        );
    }
    recipe.teardown().await.unwrap();
}

/// **A saved session that cannot run is `Gone`** — the real hub's refusals:
/// an id no index row has is `unknown_session`, and a session whose directory
/// has gone (still LISTED: craze stats no workspace when it lists) is
/// `not_accepting`/`not_resumable`. Both drop the row; neither starts a host.
#[tokio::test(flavor = "multi_thread")]
async fn a_reopen_of_an_unknown_id_or_a_gone_workspace_is_gone() {
    let Some(bins) = bins("a_reopen_of_an_unknown_id_or_a_gone_workspace_is_gone") else {
        return;
    };
    const GONE_ID: &str = "0192f0aa-6666-7000-8000-00000000gone";
    let recipe = reopen_recipe(&bins);
    // A directory the session ran in, and that is gone now.
    let gone_dir = recipe.root().join("gone");
    recipe.write_saved_sessions(&[saved_index_row(
        GONE_ID,
        "grok",
        "recipe-gone",
        &gone_dir,
        "a session whose directory went",
        "2026-10-11T10:00:00Z",
    )]);
    {
        let source = recipe.source();
        let saved = source.saved().await.unwrap();
        assert_eq!(
            saved
                .sessions
                .iter()
                .map(|s| s.id.as_str())
                .collect::<Vec<_>>(),
            [GONE_ID],
            "a session whose directory has gone is still listed"
        );

        let got = source.reopen(GONE_ID).await.unwrap_err();
        assert!(
            matches!(&got, LaneReopenError::Gone(m) if m.contains("no longer a directory")),
            "not_resumable: {got:?}"
        );
        let got = source
            .reopen("0192f0aa-6666-7000-8000-00000000none")
            .await
            .unwrap_err();
        assert!(
            matches!(got, LaneReopenError::Gone(_)),
            "unknown_session: {got:?}"
        );
        assert!(recipe.registered_hosts().is_empty(), "no host was started");
        assert!(source.created_rows().is_empty());
    }
    recipe.teardown().await.unwrap();
}
