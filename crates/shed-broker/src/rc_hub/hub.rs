//! The hub's state + config + input gate — `internal/ext/rc/hub.go`. Plan 010
//! H9 lands the daemon's CORE (config resolution, the four-lock state shape,
//! the per-slug input locks, `inputAccepted`, the idle-exit decision); the
//! HTTP shell (`handler`/`RunHub`/`serveOn`/bind-as-lock/health) arrives with
//! the axum shell in H10.
//!
//! Machine-posture deltas (plan 010 §2.3/§2.4 — deliberate, not parity debt):
//! the Rust hub is a SUPERVISED resident role inside shed-host-agent, so the
//! Go daemonization surface (`DetachHub`'s setsid double-fork, `EnsureHub`,
//! the respawn handoff, the advisory pidfile) is NOT ported. The idle-exit
//! DECISION (`shouldIdleExit`) is ported and unit-mirrored for Go parity, but
//! the host-agent role configures "never".

use std::collections::HashMap;
use std::sync::{Arc, Mutex, PoisonError};
use std::time::Duration;

use chrono::{DateTime, Utc};
use shed_core::rc::{RcActivity, RcKind, RcState};
use shed_core::rc_agents::{approval_anchor_for, composer_under_modal, prompt_anchor_for};
use shed_rc_engine::tmux::TmuxRunner;

use super::events::Subscriber;
use super::ingest::PreWatcherQueues;
use super::reconcile::TrackedSession;
use super::watch::{merged_activity, LogFn, SessionWatcher};

/// The fixed loopback TCP port the rc hub listens on (`HubPort`, `hub.go:55`).
/// 1029 sits just past the guest agent's 1028 TCP-proxy port; on a machine the
/// same fixed port keeps `sx`'s probe and the mixed-window bind handoff
/// (Go hub ⇄ agent-hosted hub) trivially aligned.
pub const HUB_PORT: u16 = 1029;

/// The hub's bind/dial address (`HubAddr`, `hub.go:62`). The bind is
/// 127.0.0.1 ONLY — a SECURITY invariant, not a default: the hub is
/// unauthenticated and trusts the loopback. Binding 0.0.0.0 would expose an
/// unauthenticated control surface. Never widen this to a non-loopback
/// interface.
pub const HUB_ADDR: &str = "127.0.0.1:1029";

/// The identity token GET /v1/health returns in `app` (`HubAppID`,
/// `hub.go:378`). Byte-frozen: the bind-as-lock and probe paths verify this
/// token so a foreign process squatting the port is an error, never mistaken
/// for a running hub.
pub const HUB_APP_ID: &str = "shed-rc-hub";

// Hub tuning defaults (`hub.go:84-106`). All overridable via HubConfig for
// tests; production uses these.
pub const DEFAULT_ACTIVE_INTERVAL: Duration = Duration::from_secs(2);
pub const DEFAULT_IDLE_INTERVAL: Duration = Duration::from_secs(10);
pub const DEFAULT_IDLE_TIMEOUT: Duration = Duration::from_secs(15 * 60);
pub const DEFAULT_HEARTBEAT: Duration = Duration::from_secs(25);
pub const DEFAULT_WRITE_TIMEOUT: Duration = Duration::from_secs(10);
pub const DEFAULT_SUBSCRIBER_BUFFER: usize = 256;

/// Configures a hub (`HubConfig`, `hub.go:112`). `runner` and `getenv` are
/// required; everything else is optional and falls back to the defaults (the
/// zero durations/ints are the "use default" signal), so tests pin a fast
/// clock, tiny intervals, and a throwaway loopback address while production
/// passes only the seams.
pub struct HubConfig {
    /// Runs tmux (the same injectable seam as the one-shot engine).
    pub runner: Arc<dyn TmuxRunner + Send + Sync>,
    /// Reads the environment (for $HOME → the JSONL roots). Injected for tests.
    pub getenv: Arc<dyn Fn(&str) -> String + Send + Sync>,
    /// The clock for activity timestamps + the idle-exit decision. None → Utc::now.
    pub now: Option<Arc<dyn Fn() -> DateTime<Utc> + Send + Sync>>,
    /// Hub diagnostics. None → stderr (Go's `log.Printf` default).
    pub logf: Option<LogFn>,
    /// Overrides the bind/dial address. "" → [`HUB_ADDR`] (the loopback
    /// invariant). Tests set an ephemeral 127.0.0.1 address.
    pub addr: String,
    /// The embedding binary's version string, served by /v1/health (plan 010
    /// §2.4 — the Rust hub has no `version.Info()`; the host-agent passes its
    /// own).
    pub version: String,

    // Tuning overrides (zero → the matching default).
    pub active_interval: Duration,
    pub idle_interval: Duration,
    pub quiet_period: Duration,
    /// Zero → the 15 m default, mirroring Go's `resolve()`. "Never" is
    /// expressed by the embedder passing a huge value (the host-agent role
    /// does) — the Go seam cannot express "never" either (§2.5).
    pub idle_timeout: Duration,
    pub heartbeat: Duration,
    pub write_timeout: Duration,
    pub subscriber_buffer: usize,
}

/// HubConfig with every default applied (`hubResolved`, `hub.go:145`).
pub struct HubResolved {
    pub runner: Arc<dyn TmuxRunner + Send + Sync>,
    pub getenv: Arc<dyn Fn(&str) -> String + Send + Sync>,
    pub now: Arc<dyn Fn() -> DateTime<Utc> + Send + Sync>,
    pub logf: LogFn,
    pub addr: String,
    pub version: String,
    pub active_interval: Duration,
    pub idle_interval: Duration,
    pub quiet: Duration,
    pub idle_timeout: Duration,
    pub heartbeat: Duration,
    pub write_timeout: Duration,
    pub sub_buffer: usize,
}

impl HubConfig {
    /// Applies the defaults (`resolve`, `hub.go:161`). The stability quiet
    /// period's own default lives in the tracker
    /// ([`super::stability::DEFAULT_QUIET_PERIOD`] — a zero `quiet` passes
    /// through and the tracker applies it, same net effect as Go resolving it
    /// here).
    fn resolve(self) -> HubResolved {
        fn dur(v: Duration, def: Duration) -> Duration {
            if v.is_zero() {
                def
            } else {
                v
            }
        }
        HubResolved {
            runner: self.runner,
            getenv: self.getenv,
            // shed-broker's chrono has no `clock` feature — the wall clock
            // comes from SystemTime (same instant Go's time.Now reads).
            now: self.now.unwrap_or_else(|| {
                Arc::new(|| DateTime::<Utc>::from(std::time::SystemTime::now()))
            }),
            logf: self
                .logf
                .unwrap_or_else(|| Arc::new(|line| eprintln!("{line}"))),
            addr: if self.addr.is_empty() {
                HUB_ADDR.to_string()
            } else {
                self.addr
            },
            version: self.version,
            active_interval: dur(self.active_interval, DEFAULT_ACTIVE_INTERVAL),
            idle_interval: dur(self.idle_interval, DEFAULT_IDLE_INTERVAL),
            quiet: self.quiet_period,
            idle_timeout: dur(self.idle_timeout, DEFAULT_IDLE_TIMEOUT),
            heartbeat: dur(self.heartbeat, DEFAULT_HEARTBEAT),
            write_timeout: dur(self.write_timeout, DEFAULT_WRITE_TIMEOUT),
            sub_buffer: if self.subscriber_buffer == 0 {
                DEFAULT_SUBSCRIBER_BUFFER
            } else {
                self.subscriber_buffer
            },
        }
    }
}

/// The reconcile state guarded by the track lock (`trackMu`-guarded fields,
/// `hub.go:224-226`).
pub(crate) struct TrackState {
    pub tracked: HashMap<String, TrackedSession>,
    /// When the session count first hit zero (`idleSince`; `None` = sessions
    /// exist). Go's zero `time.Time` maps to `None`.
    pub idle_since: Option<DateTime<Utc>>,
}

/// A running rc hub (`Hub`, `hub.go:220`). Construct with [`Hub::new`].
///
/// FOUR independent locks, with Go's documented order preserved
/// (`hub.go:243-251`): `track` guards the reconcile state; `subs` is kept
/// separate so broadcast can never deadlock against reconcile; `input_locks`
/// holds the per-SLUG input mutexes (hub-keyed, NOT per tracked entry, so
/// input serialization survives a tracked-entry replacement); `ingest` (inside
/// [`PreWatcherQueues`]) guards the cursor pre-watcher queues. **Lock order:
/// track → ingest → watcher.mu, never reversed** — reconcile is the one path
/// holding track and ingest together (`drainPreWatcher` under the commit
/// lock), and no path takes ingest first. `input_locks` ALSO nests inside
/// track (never the reverse): reconcile's disappearance sweep calls
/// [`Hub::prune_input_lock`] while still holding the track lock, exactly as Go
/// does (`hub_reconcile.go:508`).
pub struct Hub {
    pub(crate) cfg: HubResolved,
    pub(crate) track: Mutex<TrackState>,
    pub(crate) subs: Mutex<Vec<Arc<Subscriber>>>,
    input_locks: Mutex<HashMap<String, Arc<Mutex<()>>>>,
    pub(crate) ingest: PreWatcherQueues,
}

impl Hub {
    /// `newHub`, `hub.go:254`.
    pub fn new(cfg: HubConfig) -> Hub {
        Hub {
            cfg: cfg.resolve(),
            track: Mutex::new(TrackState {
                tracked: HashMap::new(),
                idle_since: None,
            }),
            subs: Mutex::new(Vec::new()),
            input_locks: Mutex::new(HashMap::new()),
            ingest: PreWatcherQueues::new(),
        }
    }

    pub(crate) fn lock_track(&self) -> std::sync::MutexGuard<'_, TrackState> {
        self.track.lock().unwrap_or_else(PoisonError::into_inner)
    }

    /// The SSE fan-out set (`subMu`-guarded `subs`, `hub.go:231`). Its own
    /// lock, kept off the track lock, so broadcast can never deadlock against
    /// reconcile.
    pub(crate) fn lock_subs(&self) -> std::sync::MutexGuard<'_, Vec<Arc<Subscriber>>> {
        self.subs.lock().unwrap_or_else(PoisonError::into_inner)
    }

    /// The slug's input-delivery mutex, created on first use (`inputLock`,
    /// `hub.go:268`). The same slug always yields the same mutex until the
    /// session disappears (pruned), so input serialization survives a
    /// tracked-entry replacement (kill+recreate keeps the slug present →
    /// keeps the lock).
    #[allow(dead_code)] // consumed by the H10 input handler
    pub(crate) fn input_lock(&self, slug: &str) -> Arc<Mutex<()>> {
        let mut locks = self
            .input_locks
            .lock()
            .unwrap_or_else(PoisonError::into_inner);
        Arc::clone(locks.entry(slug.to_string()).or_default())
    }

    /// Drops a disappeared slug's input mutex (`pruneInputLock`,
    /// `hub.go:282`). A request still holding the old mutex finishes against a
    /// gone pane (its delivery 404s); a later recreate at the same slug gets a
    /// fresh lock.
    pub(crate) fn prune_input_lock(&self, slug: &str) {
        self.input_locks
            .lock()
            .unwrap_or_else(PoisonError::into_inner)
            .remove(slug);
    }

    /// Whether the hub has held zero rc sessions for at least the idle
    /// timeout (`shouldIdleExit`, `hub.go:873`). Subscribers deliberately do
    /// not extend the window. Ported for Go parity (§2.4); the host-agent
    /// role passes an effectively-infinite timeout.
    pub fn should_idle_exit(&self, now: DateTime<Utc>) -> bool {
        let ts = self.lock_track();
        let Some(since) = ts.idle_since else {
            return false;
        };
        let Ok(timeout) = chrono::Duration::from_std(self.cfg.idle_timeout) else {
            return false; // an effectively-infinite timeout can never elapse
        };
        now.signed_duration_since(since) >= timeout
    }

    /// Releases every tracked session's watcher — hub shutdown
    /// (`closeAllWatchers`, `hub.go:859`).
    pub fn close_all_watchers(&self) {
        let mut ts = self.lock_track();
        for tr in ts.tracked.values_mut() {
            if let Some(w) = tr.watcher.take() {
                w.close();
            }
        }
    }

    /// Re-derives, from a FRESH pane capture, whether the session is waiting
    /// for typed input (`inputAccepted`, `hub.go:657`) — running the SAME
    /// watcher+stability merge the reconcile loop uses, so the handler can
    /// never be more permissive than the hub's own displayed activity. The
    /// SEVEN arms, in precedence order (each rationale in full at
    /// `hub.go:600-656`):
    ///
    /// 1. merged working OR needs_approval → reject (mid-turn delivery /
    ///    dialog owns the keyboard — the lane-derived kinds' approval arm).
    /// 2. the watcher reports ANY open approval → reject, DELIBERATELY
    ///    ignoring transport health/freshness: the merge demotes an unhealthy
    ///    watcher to pane stability, which would re-open exactly the hole arm
    ///    1 closes. A stale reject costs a retry; a stale accept costs an
    ///    approval nobody meant to give. Also catches opencode QUESTIONS.
    /// 3. the kind's ApprovalAnchor visible on the FRESH visible pane →
    ///    reject (pane-derived kinds — codex, cursor). UNDEBOUNCED, unlike
    ///    reconcile's derivation: one frame showing a dialog is enough to
    ///    refuse a keystroke.
    /// 4. expired-working + ComposerUnderModal (cursor) → GUARDED recovery:
    ///    accept ONLY when the fresh visible pane shows no known approval
    ///    surface AND stability has independently SETTLED on needs_input.
    ///    cursor's `stop` hook fires reliably only on turn 1, so a blanket
    ///    reject would make phone steering work once. codex is unaffected
    ///    (ComposerUnderModal=false — its overlay replaces the composer).
    /// 5. fresh watcher needs_input → accept outright (the structured signal
    ///    is settled and authoritative).
    /// 6. /7. anything else → the degraded-path policy: accept only if the
    ///    kind's prompt anchor is visible on the FRESH pane (closing the
    ///    lookup→lock race — a pane that flipped back to churning no longer
    ///    shows the composer).
    #[allow(dead_code)] // consumed by the H10 input handler
    pub(crate) fn input_accepted(
        &self,
        watcher: Option<&dyn SessionWatcher>,
        stability: RcActivity,
        kind: &RcKind,
        pane: &str,
        visible_pane: &str,
    ) -> bool {
        let mut watcher_act = RcActivity::Unknown;
        let (mut watcher_fresh, mut expired_working) = (false, false);
        if let Some(w) = watcher {
            w.refresh((self.cfg.now)());
            // The message is discarded, as in Go (`_`) — the gate consults
            // activity only.
            let (a, _msg, fresh, expired) = w.snapshot((self.cfg.now)());
            (watcher_act, watcher_fresh, expired_working) = (a, fresh, expired);
        }
        let (merged, _) =
            merged_activity(watcher_act, "", watcher_fresh, expired_working, stability);
        if merged == RcActivity::Working || merged == RcActivity::NeedsApproval {
            return false;
        }
        if watcher
            .and_then(SessionWatcher::as_approval_blocker)
            .is_some_and(|b| b.has_open_approvals())
        {
            return false;
        }
        if approval_anchor_for(kind).is_some_and(|a| a.is_match(visible_pane)) {
            return false;
        }
        if expired_working && composer_under_modal(kind) {
            // Guarded recovery (arm 4). The anchor arm above already rejected
            // on a matching visible pane; re-checking it here keeps this arm
            // self-evidently safe under any future re-ordering (`hub.go:676`).
            let modal_on_screen =
                approval_anchor_for(kind).is_some_and(|a| a.is_match(visible_pane));
            return !modal_on_screen && stability == RcActivity::NeedsInput;
        }
        if watcher_fresh && watcher_act == RcActivity::NeedsInput {
            return true;
        }
        prompt_anchor_for(kind).is_some_and(|a| a.is_match(pane))
    }
}

/// The lifecycle-trumps-activity precedence rule (`DisplayActivity`,
/// `activity.go:60`): a blocking lifecycle state (needs-trust / needs-auth /
/// dead) suppresses the session's whole activity dimension — Go's empty
/// `Activity` return is `None` here, so the omitted DTO fields drop out.
/// Suppression covers activity_at AND last_message alongside the activity.
pub fn display_activity(state: RcState, activity: RcActivity) -> Option<RcActivity> {
    match state {
        RcState::NeedsTrust | RcState::NeedsAuth | RcState::Dead => None,
        _ => Some(activity),
    }
}

#[cfg(test)]
mod tests {
    use std::sync::Arc;
    use std::time::Duration;

    use super::super::hub_test_support::{hook_ev, pane_fixture, rig, test_hub, CURSOR_SID};
    use super::super::messages::{FeedApproval, FeedMessage};
    use super::super::watch::{
        ApprovalBlocker, ApprovalPublisher, CursorIngester, WATCHER_WORKING_GRACE,
    };
    use super::super::watch_cursor::CursorWatcher;
    use super::*;

    /// `codexReadyPane` (`hub_input_test.go:15`).
    fn codex_ready_pane() -> String {
        "codex\n> Find and fix a bug in @filename".to_string()
    }

    /// A scripted watcher verdict (`stubWatcher`, `hub_input_test.go:302`).
    struct StubWatcher {
        activity: RcActivity,
        message: String,
        fresh: bool,
        expired_working: bool,
    }

    impl Default for StubWatcher {
        fn default() -> StubWatcher {
            StubWatcher {
                // Go's zero Activity ("") maps to Unknown (the H4 decision).
                activity: RcActivity::Unknown,
                message: String::new(),
                fresh: false,
                expired_working: false,
            }
        }
    }

    impl SessionWatcher for StubWatcher {
        fn refresh(&self, _now: DateTime<Utc>) {}
        fn snapshot(&self, _now: DateTime<Utc>) -> (RcActivity, String, bool, bool) {
            (
                self.activity,
                self.message.clone(),
                self.fresh,
                self.expired_working,
            )
        }
        fn drain_pending(&self) -> Vec<FeedMessage> {
            Vec::new()
        }
        fn had_event(&self) -> bool {
            true
        }
        fn close(&self) {}
    }

    /// `stubApprovalWatcher` (`hub_input_test.go:322`): adds the snapshot
    /// reconcile publishes and the blocked-on-a-dialog question the input
    /// gate asks. `blocked` models an open ask NOT in the snapshot — an
    /// opencode question — so the two can be driven apart.
    #[derive(Default)]
    struct StubApprovalWatcher {
        stub: StubWatcher,
        approvals: Vec<FeedApproval>,
        blocked: bool,
    }

    impl SessionWatcher for StubApprovalWatcher {
        fn refresh(&self, now: DateTime<Utc>) {
            self.stub.refresh(now);
        }
        fn snapshot(&self, now: DateTime<Utc>) -> (RcActivity, String, bool, bool) {
            self.stub.snapshot(now)
        }
        fn drain_pending(&self) -> Vec<FeedMessage> {
            Vec::new()
        }
        fn had_event(&self) -> bool {
            true
        }
        fn close(&self) {}
        fn as_approval_publisher(&self) -> Option<&dyn ApprovalPublisher> {
            Some(self)
        }
        fn as_approval_blocker(&self) -> Option<&dyn ApprovalBlocker> {
            Some(self)
        }
    }

    impl ApprovalPublisher for StubApprovalWatcher {
        fn pending_approvals(&self) -> Vec<FeedApproval> {
            self.approvals.clone()
        }
    }
    impl ApprovalBlocker for StubApprovalWatcher {
        fn has_open_approvals(&self) -> bool {
            self.blocked || !self.approvals.is_empty()
        }
    }

    fn settled_stub() -> StubWatcher {
        StubWatcher {
            activity: RcActivity::NeedsInput,
            fresh: true,
            ..StubWatcher::default()
        }
    }

    /// `expiredCursorWatcher` (`cursor_approval_test.go:323`): an
    /// expired-working verdict — the state a stuck cursor fold is in.
    fn expired_stub() -> StubWatcher {
        StubWatcher {
            activity: RcActivity::Working,
            expired_working: true,
            ..StubWatcher::default()
        }
    }

    // The per-slug input mutex is HUB-keyed: the same slug yields the same
    // mutex until pruned, so input serialization survives a tracked-entry
    // replacement (the unit half of TestHubInputLockSurvivesEntryReplacement
    // — the HTTP half lands with the H10 handler).
    #[test]
    fn input_lock_survives_replacement_and_prunes() {
        let h = test_hub();
        let l1 = h.input_lock("abc123");
        let l2 = h.input_lock("abc123");
        assert!(Arc::ptr_eq(&l1, &l2), "same slug → same mutex");
        h.prune_input_lock("abc123");
        let l3 = h.input_lock("abc123");
        assert!(!Arc::ptr_eq(&l1, &l3), "a pruned slug gets a fresh lock");
    }

    // Mirrors TestCursorInputGateAcceptsReadyRejectsApproval
    // (cursor_approval_test.go:159).
    #[test]
    fn cursor_input_gate_accepts_ready_rejects_approval() {
        let h = test_hub();
        let settled = settled_stub();
        let ready = pane_fixture("cursor-ready");
        let approval = pane_fixture("cursor-ready-approval-shell");

        assert!(
            h.input_accepted(
                Some(&settled),
                RcActivity::NeedsInput,
                &RcKind::Cursor,
                &ready,
                &ready
            ),
            "a ready cursor composer must accept feed input"
        );
        assert!(
            !h.input_accepted(
                Some(&settled),
                RcActivity::NeedsInput,
                &RcKind::Cursor,
                &approval,
                &approval
            ),
            "an approval prompt on the visible frame must reject input"
        );
        // The approval fixture still matches the READY/prompt anchor (cursor
        // keeps its composer drawn, disabled, under the decision surface) —
        // exactly why the approval arm has to exist.
        assert!(
            prompt_anchor_for(&RcKind::Cursor)
                .expect("cursor prompt anchor")
                .is_match(&approval),
            "premise: the approval fixture still shows the composer anchor"
        );
        // Scrollback is not evidence about the present.
        assert!(
            h.input_accepted(
                Some(&settled),
                RcActivity::NeedsInput,
                &RcKind::Cursor,
                &approval,
                &ready
            ),
            "an approval prompt only in the scrollback must not gate input"
        );
        // Post-resolution and quoted prose both flow again.
        for fx in [
            "cursor-ready-approval-resolved",
            "cursor-ready-approval-quoted",
        ] {
            let pane = pane_fixture(fx);
            assert!(
                h.input_accepted(
                    Some(&settled),
                    RcActivity::NeedsInput,
                    &RcKind::Cursor,
                    &pane,
                    &pane
                ),
                "{fx}: input must be accepted"
            );
        }
    }

    // Mirrors TestCursorInputGateRejectsExpiredWorkingUnderAnUnknownModal
    // (cursor_approval_test.go:235).
    #[test]
    fn cursor_input_gate_rejects_expired_working_under_unknown_modal() {
        let (h, _, clk) = rig();

        // A cursor turn in flight, then the operator walks away: no `stop`
        // ever arrives, so the verdict expires.
        let w = CursorWatcher::new("", None);
        w.push_hook_event(hook_ev(
            "preToolUse",
            &format!(
                r#"{{"session_id":"{CURSOR_SID}","tool_name":"Delete","tool_input":{{"file_path":"/home/shed/proj/build.json"}}}}"#
            ),
        ));
        w.refresh(clk.now());
        clk.advance(WATCHER_WORKING_GRACE + Duration::from_secs(1));
        let (_, _, fresh, expired) = w.snapshot(clk.now());
        assert!(
            !fresh && expired,
            "premise: the verdict must be expired-working"
        );

        // A widget the anchor does not know, cursor's composer still drawn
        // beneath it, stability idle — NOT the settled needs_input the
        // recovery requires, so the arm rejects.
        let unknown_modal =
            pane_fixture("cursor-ready") + "\n Some future approval widget?\n   → Yes, do it (y)\n";
        assert!(
            !approval_anchor_for(&RcKind::Cursor)
                .expect("anchor")
                .is_match(&unknown_modal),
            "premise: must NOT match the approval anchor"
        );
        assert!(
            prompt_anchor_for(&RcKind::Cursor)
                .expect("anchor")
                .is_match(&unknown_modal),
            "premise: the composer must still be visible under the widget"
        );
        assert!(
            !h.input_accepted(
                Some(&w),
                RcActivity::Idle,
                &RcKind::Cursor,
                &unknown_modal,
                &unknown_modal
            ),
            "expired-working cursor with a non-needs_input stability must reject"
        );

        // The legitimate case is unaffected: when `stop` DOES fire it settles
        // the fold, and a settled verdict accepts however long it's been quiet.
        w.push_hook_event(hook_ev(
            "stop",
            &format!(r#"{{"session_id":"{CURSOR_SID}","status":"completed"}}"#),
        ));
        w.refresh(clk.now());
        clk.advance(Duration::from_secs(24 * 3600));
        let ready = pane_fixture("cursor-ready");
        assert!(
            h.input_accepted(Some(&w), RcActivity::Idle, &RcKind::Cursor, &ready, &ready),
            "a settled cursor verdict must still accept input"
        );

        // And a cursor session with NO watcher verdict at all keeps the
        // degraded anchor path.
        assert!(
            h.input_accepted(None, RcActivity::Idle, &RcKind::Cursor, &ready, &ready),
            "with no watcher verdict the composer anchor must still accept"
        );
    }

    // Mirrors TestCursorInputGateRejectsWhileWorking
    // (cursor_approval_test.go:292).
    #[test]
    fn cursor_input_gate_rejects_while_working() {
        let (h, _, clk) = rig();

        let w = CursorWatcher::new("", None);
        w.push_hook_event(hook_ev(
            "beforeSubmitPrompt",
            &format!(r#"{{"session_id":"{CURSOR_SID}","prompt":"go"}}"#),
        ));
        w.refresh(clk.now());
        let ready = pane_fixture("cursor-ready");
        assert!(
            !h.input_accepted(
                Some(&w),
                RcActivity::NeedsInput,
                &RcKind::Cursor,
                &ready,
                &ready
            ),
            "a working hook verdict must reject input even at the composer"
        );

        w.push_hook_event(hook_ev(
            "stop",
            &format!(r#"{{"session_id":"{CURSOR_SID}","status":"completed"}}"#),
        ));
        w.refresh(clk.now());
        assert!(
            h.input_accepted(Some(&w), RcActivity::Idle, &RcKind::Cursor, &ready, &ready),
            "a settled hook verdict must accept input"
        );
    }

    // Mirrors TestCursorInputGateExpiredWorkingRealDialogRejected — THE
    // F1-SAFETY PIN (cursor_approval_test.go:333): stability=needs_input is
    // the worst case (the composer under the dialog settles, so recovery
    // condition (b) holds) — only the exhaustive anchor stands between a
    // posted line and the widget.
    #[test]
    fn cursor_input_gate_expired_working_real_dialog_rejected() {
        let h = test_hub();
        assert!(
            approval_anchor_for(&RcKind::Cursor)
                .expect("anchor")
                .is_match(&pane_fixture("cursor-ready-approval-shell")),
            "premise: the approval fixture matches the anchor on the visible frame"
        );
        for fx in [
            "cursor-ready-approval-shell",
            "cursor-ready-approval-delete",
            "cursor-ready-approval-write",
        ] {
            let dialog = pane_fixture(fx);
            assert!(
                !h.input_accepted(
                    Some(&expired_stub()),
                    RcActivity::NeedsInput,
                    &RcKind::Cursor,
                    &dialog,
                    &dialog
                ),
                "{fx}: an expired-working cursor with a real dialog on the visible frame must REJECT"
            );
        }
    }

    // Mirrors TestCursorInputGateExpiredWorkingIdleComposerRecovers — THE
    // RECOVERY (cursor_approval_test.go:356).
    #[test]
    fn cursor_input_gate_expired_working_idle_composer_recovers() {
        let h = test_hub();
        let ready = pane_fixture("cursor-ready");
        assert!(
            !approval_anchor_for(&RcKind::Cursor)
                .expect("anchor")
                .is_match(&ready),
            "premise: the clean composer must NOT match the ApprovalAnchor"
        );
        assert!(
            h.input_accepted(
                Some(&expired_stub()),
                RcActivity::NeedsInput,
                &RcKind::Cursor,
                &ready,
                &ready
            ),
            "expired-working + clean idle composer + settled needs_input must ACCEPT"
        );
        // A dialog answered long ago sits in the 200-line history (`pane`)
        // while the visible frame is clean — still a recovery.
        let stale = pane_fixture("cursor-ready-approval-shell");
        assert!(
            h.input_accepted(
                Some(&expired_stub()),
                RcActivity::NeedsInput,
                &RcKind::Cursor,
                &stale,
                &ready
            ),
            "a dialog only in scrollback must not block the recovery"
        );
    }

    // Mirrors TestCursorInputGateExpiredWorkingUnsettledStabilityRejected —
    // NO FALSE ACCEPT MID-WORK (cursor_approval_test.go:380).
    #[test]
    fn cursor_input_gate_expired_working_unsettled_stability_rejected() {
        let h = test_hub();
        let ready = pane_fixture("cursor-ready");
        // Still churning: stability=working ⇒ merged stays working ⇒ reject.
        assert!(
            !h.input_accepted(
                Some(&expired_stub()),
                RcActivity::Working,
                &RcKind::Cursor,
                &ready,
                &ready
            ),
            "a still-churning (working) stability must REJECT"
        );
        // Settled but idle: passes the merge but fails recovery condition (b).
        assert!(
            !h.input_accepted(
                Some(&expired_stub()),
                RcActivity::Idle,
                &RcKind::Cursor,
                &ready,
                &ready
            ),
            "a settled-idle (not needs_input) stability must REJECT"
        );
    }

    // Mirrors TestCursorInputGateFreshFirstTurnAccepts
    // (cursor_approval_test.go:400).
    #[test]
    fn cursor_input_gate_fresh_first_turn_accepts() {
        let (h, _, clk) = rig();

        let w = CursorWatcher::new("", None);
        w.push_hook_event(hook_ev(
            "beforeSubmitPrompt",
            &format!(r#"{{"session_id":"{CURSOR_SID}","prompt":"go"}}"#),
        ));
        w.refresh(clk.now());
        w.push_hook_event(hook_ev(
            "stop",
            &format!(r#"{{"session_id":"{CURSOR_SID}","status":"completed"}}"#),
        ));
        w.refresh(clk.now());
        let (_, _, fresh, expired) = w.snapshot(clk.now());
        assert!(
            fresh && !expired,
            "premise: a just-settled fold must be fresh, not expired"
        );
        let ready = pane_fixture("cursor-ready");
        assert!(
            h.input_accepted(
                Some(&w),
                RcActivity::NeedsInput,
                &RcKind::Cursor,
                &ready,
                &ready
            ),
            "a fresh settled cursor fold (turn 1) must accept /input"
        );
    }

    // Mirrors TestCursorInputGateCodexExpiredWorkingUnchanged
    // (cursor_approval_test.go:424): the guarded arm is gated on
    // ComposerUnderModal, FALSE for codex.
    #[test]
    fn cursor_input_gate_codex_expired_working_unchanged() {
        let h = test_hub();
        assert!(
            !composer_under_modal(&RcKind::Codex),
            "premise: codex must NOT declare ComposerUnderModal"
        );
        let ready = codex_ready_pane();
        assert!(
            h.input_accepted(
                Some(&expired_stub()),
                RcActivity::NeedsInput,
                &RcKind::Codex,
                &ready,
                &ready
            ),
            "codex expired-working + settled needs_input + clean composer must accept"
        );
        let dialog = pane_fixture("codex-ready-approval-exec");
        assert!(
            !h.input_accepted(
                Some(&expired_stub()),
                RcActivity::NeedsInput,
                &RcKind::Codex,
                &dialog,
                &dialog
            ),
            "codex expired-working + a dialog on the visible frame must reject via codex's own anchor"
        );
    }

    // The open-approval blocker arm DELIBERATELY ignores transport
    // health/freshness (the unit half of
    // TestHubInputOpenApprovalRejectsWhenTransportUnhealthy): an UNFRESH
    // watcher's merge falls to stability (needs_input — would accept), but an
    // open ask still owns the keyboard. Also covers opencode QUESTIONS
    // (blocked without a snapshot entry).
    #[test]
    fn input_gate_open_approval_rejects_when_transport_unhealthy() {
        let h = test_hub();
        let pane = "opencode\n> Ask anything...";
        let blocked = StubApprovalWatcher {
            stub: StubWatcher {
                activity: RcActivity::Idle,
                ..StubWatcher::default()
            },
            blocked: true,
            ..StubApprovalWatcher::default()
        };
        assert!(
            !h.input_accepted(
                Some(&blocked),
                RcActivity::NeedsInput,
                &RcKind::Opencode,
                pane,
                ""
            ),
            "an open approval must reject even with the watcher unfresh"
        );
        let clear = StubApprovalWatcher {
            stub: StubWatcher {
                activity: RcActivity::Idle,
                ..StubWatcher::default()
            },
            ..StubApprovalWatcher::default()
        };
        assert!(
            h.input_accepted(
                Some(&clear),
                RcActivity::NeedsInput,
                &RcKind::Opencode,
                pane,
                ""
            ),
            "no open approvals + settled stability at the composer must accept"
        );
    }

    // The SEVEN-ARM table, pinned by ARM IDENTITY (plan 010 AC2): each row
    // names the arm that decides it, with inputs that make every EARLIER arm
    // pass through — so a precedence reorder fails the table even where the
    // outcome would coincide.
    #[test]
    fn input_accepted_seven_arm_table() {
        let h = test_hub();
        let cursor_ready = pane_fixture("cursor-ready");
        let cursor_dialog = pane_fixture("cursor-ready-approval-shell");
        let codex_ready = codex_ready_pane();
        let codex_dialog = pane_fixture("codex-ready-approval-exec");

        // ARM 1a (merged working): fresh working watcher; pane at the
        // composer (later arms would accept).
        let fresh_working = StubWatcher {
            activity: RcActivity::Working,
            fresh: true,
            ..StubWatcher::default()
        };
        assert!(
            !h.input_accepted(
                Some(&fresh_working),
                RcActivity::NeedsInput,
                &RcKind::Codex,
                &codex_ready,
                &codex_ready
            ),
            "arm 1a: merged working rejects"
        );

        // ARM 1b (merged needs_approval): the lane-derived approval arm.
        let fresh_approval = StubWatcher {
            activity: RcActivity::NeedsApproval,
            fresh: true,
            ..StubWatcher::default()
        };
        assert!(
            !h.input_accepted(
                Some(&fresh_approval),
                RcActivity::NeedsInput,
                &RcKind::Opencode,
                "opencode\n> Ask anything...",
                ""
            ),
            "arm 1b: merged needs_approval rejects"
        );

        // ARM 2 (open-approval blocker): merged is idle (arm 1 passes), no
        // anchor on the pane (arm 3 passes) — the blocker alone rejects.
        let blocked = StubApprovalWatcher {
            stub: StubWatcher {
                activity: RcActivity::Idle,
                fresh: true,
                ..StubWatcher::default()
            },
            blocked: true,
            ..StubApprovalWatcher::default()
        };
        assert!(
            !h.input_accepted(
                Some(&blocked),
                RcActivity::Idle,
                &RcKind::Opencode,
                "opencode\n> Ask anything...",
                ""
            ),
            "arm 2: an open approval rejects regardless of the merge"
        );

        // ARM 3 (approval anchor on the visible frame): fresh needs_input
        // watcher (arm 5 would accept) — the anchor rejects first.
        assert!(
            !h.input_accepted(
                Some(&settled_stub()),
                RcActivity::NeedsInput,
                &RcKind::Cursor,
                &cursor_dialog,
                &cursor_dialog
            ),
            "arm 3: a visible approval anchor rejects even a fresh needs_input"
        );

        // ARM 4 (guarded recovery): expired-working + ComposerUnderModal;
        // accepts ONLY with a clean visible pane AND settled needs_input.
        assert!(
            h.input_accepted(
                Some(&expired_stub()),
                RcActivity::NeedsInput,
                &RcKind::Cursor,
                &cursor_ready,
                &cursor_ready
            ),
            "arm 4: recovery accepts"
        );
        assert!(
            !h.input_accepted(
                Some(&expired_stub()),
                RcActivity::Idle,
                &RcKind::Cursor,
                &cursor_ready,
                &cursor_ready
            ),
            "arm 4: unsettled stability rejects INSIDE the arm (never falls through to the anchor path)"
        );

        // ARM 5 (fresh needs_input accepts outright): the pane deliberately
        // does NOT match the prompt anchor — only arm 5 can accept it.
        assert!(
            h.input_accepted(
                Some(&settled_stub()),
                RcActivity::NeedsInput,
                &RcKind::Codex,
                "no anchor here",
                ""
            ),
            "arm 5: a fresh needs_input accepts without the anchor"
        );

        // ARM 6 (degraded path, anchor visible): no watcher, stale stability.
        assert!(
            h.input_accepted(None, RcActivity::Idle, &RcKind::Codex, &codex_ready, ""),
            "arm 6: the fresh prompt anchor accepts on the degraded path"
        );

        // ARM 7 (degraded path, no anchor): everything else rejects.
        assert!(
            !h.input_accepted(
                None,
                RcActivity::Idle,
                &RcKind::Codex,
                "churning output",
                ""
            ),
            "arm 7: no anchor on the fresh pane rejects"
        );
        // ...including a codex dialog pane: its overlay REPLACES the composer,
        // so the prompt anchor cannot match (the arm-3 anchor also fires, but
        // arm 7 is what this row pins — visiblePane is "" here).
        assert!(
            !h.input_accepted(None, RcActivity::Idle, &RcKind::Codex, &codex_dialog, ""),
            "arm 7: a codex dialog pane cannot match its prompt anchor"
        );
    }
}
