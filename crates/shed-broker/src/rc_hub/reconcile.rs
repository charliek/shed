//! The reconcile loop — `internal/ext/rc/hub_reconcile.go`. The hub's
//! heartbeat: on each tick it enumerates the shed's rc-* tmux sessions,
//! refreshes each session's structured-signal watcher (opencode's SSE lane —
//! the only one left), and emits SSE events on transitions:
//!
//! - `session.updated` on appear (new slug or a recreated slug), on a
//!   lifecycle-state change, and on disappear (killed → session:null);
//! - `activity.changed` when a session's DISPLAYED activity changes to a
//!   VALID, NON-EMPTY value — the suppressed dimension is never emitted (a
//!   transition INTO suppression coincides with a blocking lifecycle state
//!   change, which already emits session.updated).
//!
//! Everything is driven off the injected runner + clock, so tests script tmux
//! output and time and assert the emitted events with no real tmux.
//!
//! CONCURRENCY (the §2.3 ownership redesign, guarantees re-derived):
//! reconcile is single-threaded and the SOLE WRITER of tracked state, exactly
//! as in Go. Go's mid-pass `trackMu` unlock (release for heavy tmux/disk/
//! network work, re-acquire to commit) maps to per-phase lock scopes here:
//! the entry phase CLONES what the heavy phase needs (the watcher and ring
//! `Arc`s, the reconcile-only counters) under the track lock, the heavy phase
//! runs unlocked against those clones (the sub-objects are self-synchronized —
//! watcher/ring each carry their own mutex — or reconcile-only), and the
//! commit phase re-acquires the lock
//! and writes the handler-visible fields back. The re-lookup on re-acquire is
//! guaranteed to find the same entry BECAUSE reconcile is the sole writer.
//! Handlers may interleave at any unlock boundary — the same windows Go
//! allows. Lock order: track → ingest → watcher.mu, never reversed.

use std::sync::Arc;

use chrono::SecondsFormat;
use shed_core::rc::{RcActivity, RcKind, RcSessionDto, RcState};
use shed_core::rc_agents::parse_session;
use shed_rc_engine::tmux::Tmux;

use super::events::{
    activity_changed_event, message_appended_event, session_gone_event, session_updated_event,
    HubEvent,
};
use super::hub::{display_activity, Hub};
use super::messages::{FeedApproval, MessageRing};
use super::watch::{
    agent_session_env, back_write_agent_session, merged_activity, opencode_port_env,
    watchable_kind, SessionWatcher,
};
use super::watch_opencode_transport::OpencodeWatcher;

// S2 (charliek/shed#324) deleted the PANE-ANCHOR APPROVAL machinery that opened
// this module — `PANE_APPROVAL_DEBOUNCE_TICKS`, `PaneApprovalState`,
// `pane_approval_row`, `first_anchor_line` — along with the anchors it matched.
// A shed row's approvals now come from a lane that has them (opencode's) or not
// at all.

/// The hub's per-session reconcile state (`trackedSession`,
/// `hub_reconcile.go:31`). One per live rc session, ticked from the single
/// reconcile pass.
pub(crate) struct TrackedSession {
    /// id + createdAt are the session's identity pin: a change in EITHER means
    /// the slug was killed and recreated. Both are checked because
    /// legacy/partial sessions can lack SHED_RC_ID — created_at is the
    /// fallback signal.
    pub id: String,
    pub created_at: String,
    /// The hub's capability key, stamped when the entry was built — the verb
    /// handlers authorize against it rather than re-deriving from a fresh
    /// capture. A recreate replaces this whole entry.
    pub kind: RcKind,

    /// The DISPLAYED activity (DisplayActivity already applied): `None` means
    /// the activity dimension is suppressed (Go's "").
    pub activity: Option<RcActivity>,
    /// RFC3339 time the displayed activity last changed.
    pub activity_at: String,
    /// Sanitized preview from the watcher ("" when it has none).
    pub last_message: String,
    pub last_state: RcState,

    /// The session's structured-signal watcher (the opencode SSE transport),
    /// lazily created (`ensureWatcher`). `None` for kinds with no structured
    /// signal — every kind but opencode since A6 (`charliek/shed#322`) — or
    /// before correlation succeeds.
    pub watcher: Option<Arc<dyn SessionWatcher + Send + Sync>>,
    /// An AMBIGUOUS correlation's agent session id, held back until the
    /// watcher's first confirming event settles the pick — only then back-
    /// written to SHED_RC_AGENT_SESSION (a wrong pin would be permanent).
    pub pending_agent_id: String,

    /// The session's message feed. Every tracked session has one so /messages
    /// returns 200-empty for a known slug. Self-synchronized.
    pub ring: Arc<MessageRing>,

    /// The currently-open approval requests, republished every tick from a
    /// lane that knows its approvals (ApprovalPublisher — opencode). PENDING
    /// ONLY by wire contract.
    pub pending_approvals: Vec<FeedApproval>,
}

impl TrackedSession {}

impl TrackedSession {
    /// Whether `s` is still the session this state was built for
    /// (`sameIdentity`, `hub_reconcile.go:252`). Kind participates because
    /// the tracked kind is what the verb handlers authorize against: a legacy
    /// session recreated at the same slug as a DIFFERENT kind must not keep
    /// the old incarnation's kind.
    pub(crate) fn same_identity(&self, s: &RcSessionDto) -> bool {
        self.id == s.id.as_deref().unwrap_or("")
            && self.created_at == s.created_at.as_deref().unwrap_or("")
            && self.kind == s.kind
    }

    /// The session's pending_approvals overlay (`approvalSnapshot`,
    /// `hub_reconcile.go`): the entries its lane watcher published (opencode's
    /// — the only lane with approvals since A6). S2 (charliek/shed#324) removed
    /// the pane-anchor episodes that used to be unioned in here. Called under
    /// the track lock; the caller deep-copies before serving.
    pub(crate) fn approval_snapshot(&self) -> Vec<FeedApproval> {
        self.pending_approvals.clone()
    }
}

/// Deep-copies an approval snapshot for serving (`copyApprovals`,
/// `hub.go:353`). Rust's `Clone` on [`FeedApproval`] already copies the
/// `decisions` vector, so the aliasing hazard Go guards against cannot arise;
/// the helper stays for structural fidelity (and marks the /v1/sessions
/// overlay's copy point). The Go nil-on-empty (→ omitempty) shaping happens at
/// the DTO layer.
pub(crate) fn copy_approvals(approvals: &[FeedApproval]) -> Vec<FeedApproval> {
    approvals.to_vec()
}

// `MAX_CORRELATE_TRIES` bounded the file-correlation retry budget. Only the
// codex rollout lane ever spent it, so it went with that lane in A6
// (`charliek/shed#322`) — opencode's watcher correlates asynchronously over its
// own SSE stream and is built on the first eligible tick.

/// The session DTOs for the given tmux session names (`sessionsForNames`,
/// `ops.go:355`) — the shared enumeration loop behind the /v1/sessions
/// handler's one-shot List and reconcile's pass (which lists names through
/// the CHECKED variant first). The hub passes no display fallback.
pub(crate) fn sessions_for_names(tmux: &Tmux<'_>, names: &[String]) -> Vec<RcSessionDto> {
    names
        .iter()
        .map(|name| {
            let env = tmux.show_environment(name);
            let pane = tmux.capture_pane(name).stdout;
            parse_session(name, &env, &pane, None)
        })
        .collect()
}

/// What the entry phase clones out for one session's unlocked heavy work.
struct HeavySnapshot {
    watcher: Option<Arc<dyn SessionWatcher + Send + Sync>>,
    ring: Arc<MessageRing>,
    pending_agent_id: String,
}

impl Hub {
    /// Builds tracker state for a freshly seen session (`newTrackedSession`,
    /// `hub_reconcile.go`). EVERY enumerated session gets an entry, watcher or
    /// not — that is what makes /messages answer 200 with an empty page for a
    /// tracked feedless kind and 404 only for an unknown slug.
    pub(crate) fn new_tracked_session(&self, s: &RcSessionDto) -> TrackedSession {
        TrackedSession {
            id: s.id.clone().unwrap_or_default(),
            created_at: s.created_at.clone().unwrap_or_default(),
            kind: s.kind.clone(),
            activity: None,
            activity_at: String::new(),
            last_message: String::new(),
            last_state: s.state,
            watcher: None,
            pending_agent_id: String::new(),
            ring: Arc::new(MessageRing::new()),
            pending_approvals: Vec::new(),
        }
    }

    /// One enumeration+tick pass; broadcasts the resulting events
    /// (`reconcile`, `hub_reconcile.go:267`). See the module doc for the
    /// lock-scope mapping of Go's release-for-heavy-work dance.
    pub fn reconcile(&self) {
        // A transient tmux listing failure must NOT read as "every session is
        // gone" — that would wipe the message rings, close the watchers, and
        // broadcast a storm of session-gone events over one hiccup. Skip the
        // whole pass and keep state; the next tick retries. (A genuine "no
        // sessions/no server" answer returns Ok(empty) and proceeds — that IS
        // everything-gone.)
        let tmux = Tmux::new(&*self.cfg.runner);
        let names = match tmux.list_session_names_checked() {
            Ok(names) => names,
            Err(err) => {
                (self.cfg.logf)(&format!(
                    "rc hub: session listing failed ({err}); keeping state this tick"
                ));
                return;
            }
        };
        let sessions = sessions_for_names(&tmux, &names);
        let now = (self.cfg.now)();

        let mut events: Vec<HubEvent> = Vec::new();
        let mut present: std::collections::HashSet<String> =
            std::collections::HashSet::with_capacity(sessions.len());

        for s in &sessions {
            present.insert(s.slug.clone());

            // --- Entry phase (track lock): appear/recreate/state-change
            // detection + the heavy-phase snapshot. ---
            let snap: HeavySnapshot = {
                let mut ts = self.lock_track();
                let needs_replace = match ts.tracked.get(&s.slug) {
                    Some(tr) => !tr.same_identity(s),
                    None => true,
                };
                if needs_replace {
                    // New session, or the slug was recreated (id OR
                    // created_at changed) → start over. A recreate must drop
                    // the previous watcher (a new session gets a new JSONL
                    // file or opencode port).
                    if let Some(old) = ts.tracked.get_mut(&s.slug) {
                        if let Some(w) = old.watcher.take() {
                            w.close();
                        }
                    }
                    ts.tracked
                        .insert(s.slug.clone(), self.new_tracked_session(s));
                    events.push(session_updated_event(s));
                } else if ts.tracked[&s.slug].last_state != s.state {
                    events.push(session_updated_event(s));
                }
                let tr = ts.tracked.get_mut(&s.slug).expect("just ensured");
                tr.last_state = s.state;
                HeavySnapshot {
                    watcher: tr.watcher.clone(),
                    ring: Arc::clone(&tr.ring),
                    pending_agent_id: tr.pending_agent_id.clone(),
                }
            };

            // --- Heavy tmux/network work, track lock RELEASED
            // (`hub_reconcile.go`). The clones above stay valid because
            // reconcile is the sole writer; the sub-objects touched here are
            // self-synchronized (ring, watcher) or reconcile-only (the
            // counters, committed back below). ---
            let mut pending_agent_id = snap.pending_agent_id;
            let new_w = self.ensure_watcher(snap.watcher.is_some(), s);
            let watcher = new_w.clone().or(snap.watcher);

            // Derive activity. Since S2 (charliek/shed#324) there is exactly
            // ONE source: a fresh, correlated watcher. No watcher, a closed or
            // unhealthy transport, or a stale verdict all mean the same thing —
            // this session has no activity dimension (merged_activity returns
            // None, which the DTO omits).
            let mut watcher_activity = RcActivity::Unknown;
            let mut watcher_message = String::new();
            let mut watcher_fresh = false;
            let mut msg_events: Vec<HubEvent> = Vec::new();
            let mut pending_approvals: Vec<FeedApproval> = Vec::new();
            let mut publishes_approvals = false;
            if let Some(w) = &watcher {
                w.refresh(now);
                // A deferred (ambiguous-correlation) back-write happens only
                // once the first confirming event settles the pick.
                if !pending_agent_id.is_empty() && w.had_event() {
                    back_write_agent_session(&tmux, &s.tmux_session, &pending_agent_id);
                    pending_agent_id.clear();
                }
                // The opencode watcher correlates ASYNC: once it pins the
                // session id it surfaces it here for back-write into
                // SHED_RC_AGENT_SESSION, so a hub restart re-correlates
                // exactly. A non-empty drain is always a fresh id to stamp.
                if let Some(d) = w.as_confirmed_agent_id_drainer() {
                    let id = d.drain_confirmed_agent_id();
                    if !id.is_empty() {
                        back_write_agent_session(&tmux, &s.tmux_session, &id);
                    }
                }
                // Drain normalized feed messages into the session ring. A
                // per-message message.appended notification lets subscribers
                // know to fetch /messages — the body is deliberately not on
                // the SSE frame.
                for m in w.drain_pending() {
                    let seq = snap.ring.append(m, now);
                    msg_events.push(message_appended_event(&s.slug, seq));
                }
                // Republish the lane's OPEN approvals every tick
                // (approvalPublisher). Read unlocked (the watcher
                // self-synchronizes), committed under the track lock below.
                if let Some(ap) = w.as_approval_publisher() {
                    pending_approvals = ap.pending_approvals();
                    publishes_approvals = true;
                }
                let snapshot = w.snapshot(now);
                watcher_activity = snapshot.0;
                watcher_message = snapshot.1;
                watcher_fresh = snapshot.2;
            }

            // --- Commit phase: re-acquire the track lock and write the
            // handler-visible fields (`hub_reconcile.go:422`). ---
            {
                let mut ts = self.lock_track();
                let tr = ts
                    .tracked
                    .get_mut(&s.slug)
                    .expect("sole writer: the entry cannot vanish mid-pass");
                if let Some(w) = &new_w {
                    tr.watcher = Some(Arc::clone(w));
                }
                // Only a publishing watcher owns this field: a kind whose
                // approvals are not lane-derived must keep what it holds
                // rather than be blanked by an unrelated tick.
                if publishes_approvals {
                    tr.pending_approvals = pending_approvals;
                }
                tr.pending_agent_id = pending_agent_id;
                events.append(&mut msg_events);

                let (merged_raw, merged_msg) =
                    merged_activity(watcher_activity, &watcher_message, watcher_fresh);
                // `None` from the merge is Go's empty Activity — no dimension at
                // all — and short-circuits the lifecycle rule, exactly as Go's
                // DisplayActivity("") does for every state.
                let eff = merged_raw.and_then(|a| display_activity(s.state, a));
                // last_message rides with the activity dimension: a
                // suppressed activity drops the message too.
                let eff_msg = if eff.is_none() {
                    String::new()
                } else {
                    merged_msg
                };

                if eff != tr.activity {
                    tr.activity = eff;
                    tr.activity_at = now.to_rfc3339_opts(SecondsFormat::Secs, true);
                    // Contract: activity.changed carries only valid non-empty
                    // activity values. A transition INTO suppression is
                    // announced by the session.updated the state change
                    // emitted above.
                    if let Some(a) = eff {
                        events.push(activity_changed_event(
                            &s.slug,
                            a,
                            &tr.activity_at,
                            s.state,
                            &eff_msg,
                        ));
                    }
                }
                // Keep the preview current every tick (the /v1/sessions
                // overlay reads it) even when the activity did not change.
                tr.last_message = eff_msg;
            }
        }

        // Disappearance sweep + idle-exit bookkeeping (one lock scope, as in
        // Go's tail under the same trackMu hold).
        {
            let mut ts = self.lock_track();
            let gone: Vec<String> = ts
                .tracked
                .keys()
                .filter(|slug| !present.contains(*slug))
                .cloned()
                .collect();
            for slug in gone {
                if let Some(tr) = ts.tracked.remove(&slug) {
                    // Release the watcher (its SSE connection now points at a
                    // dead session).
                    if let Some(w) = tr.watcher {
                        w.close();
                    }
                }
                events.push(session_gone_event(&slug));
            }

            // Idle-exit bookkeeping: start the clock when the session count
            // first hits zero, clear it the moment any session exists.
            if sessions.is_empty() {
                if ts.idle_since.is_none() {
                    ts.idle_since = Some(now);
                }
            } else {
                ts.idle_since = None;
            }
        }

        // Publish conversation ownership, after the track lock is released.
        //
        // Age alone cannot settle who owns a conversation in a shared opencode
        // store: a session that started FIRST and then sat idle will happily
        // adopt the conversation a later session is actively using, because
        // that conversation is newer than the adopter. Only the hub sees every
        // session, so the hub is what tells each watcher which ids are already
        // spoken for — every tick, because a neighbour's pin usually does not
        // exist yet when the watcher is built.
        self.publish_claims();

        for e in &events {
            self.broadcast(e);
        }
    }

    /// Tell every claim-holding watcher which agent-session ids belong to the
    /// OTHER tracked sessions. See [`crate::rc_hub::watch::ClaimHolder`].
    ///
    /// Snapshot the (slug, watcher) pairs under the lock, then do the pushing
    /// with the lock released: `set_claimed` takes the watcher's own mutex, and
    /// holding two locks in one order here and the other order anywhere else is
    /// how a deadlock is written.
    fn publish_claims(&self) {
        let holders: Vec<(String, Arc<dyn SessionWatcher + Send + Sync>)> = {
            let ts = self.lock_track();
            ts.tracked
                .iter()
                .filter_map(|(slug, tr)| {
                    let w = tr.watcher.clone()?;
                    w.as_claim_holder()?;
                    Some((slug.clone(), w))
                })
                .collect()
        };
        if holders.len() < 2 {
            return; // nothing can be contested
        }
        let pins: Vec<(String, String)> = holders
            .iter()
            .filter_map(|(slug, w)| {
                let id = w.as_claim_holder()?.pinned_agent_id();
                (!id.is_empty()).then(|| (slug.clone(), id))
            })
            .collect();
        for (slug, w) in &holders {
            let others: Vec<String> = pins
                .iter()
                .filter(|(owner, _)| owner != slug)
                .map(|(_, id)| id.clone())
                .collect();
            if let Some(h) = w.as_claim_holder() {
                h.set_claimed(others);
            }
        }
    }

    /// Lazily builds a watchable session's structured-signal watcher,
    /// RETURNING it (`ensureWatcher`, `hub_reconcile.go`; `None` when none was
    /// created this call). Runs UNLOCKED from reconcile, so it must NOT publish
    /// the watcher — the caller commits it under the track lock.
    ///
    /// opencode is the only watchable kind since A6 (`charliek/shed#322`)
    /// removed the codex JSONL tail and the cursor hook-ingest lane. Its
    /// watcher owns its OWN async correlation over SSE/REST (non-blocking
    /// construction), so none of the file-correlation machinery the other two
    /// needed survives here.
    fn ensure_watcher(
        &self,
        watcher_exists: bool,
        s: &RcSessionDto,
    ) -> Option<Arc<dyn SessionWatcher + Send + Sync>> {
        if watcher_exists || !watchable_kind(&s.kind) {
            return None;
        }
        match s.state {
            RcState::NeedsTrust | RcState::NeedsAuth | RcState::Dead => {
                return None; // no live activity to watch; retry once usable
            }
            _ => {}
        }
        let tmux = Tmux::new(&*self.cfg.runner);
        let workdir = s.workdir.as_deref().unwrap_or("");

        // A session with no valid recorded port is unwatchable over this
        // transport — no watcher is built, so the session simply carries no
        // activity at all (there is no fallback engine to drive it instead).
        let port = opencode_port_env(&tmux, &s.tmux_session)?;
        // A prior back-written SHED_RC_AGENT_SESSION is the trusted pin;
        // "" means the watcher searches its SSE stream for the id.
        let agent_id = agent_session_env(&tmux, &s.tmux_session);
        // When this RC session was created. opencode's store is shared per
        // PROJECT, so `/session` lists a neighbouring RC session's
        // conversations too and the directory alone cannot tell them
        // apart — the watcher refuses to adopt one older than the session
        // itself. Unparseable/absent → the epoch, which disables the check.
        let not_before = s
            .created_at
            .as_deref()
            .and_then(|t| chrono::DateTime::parse_from_rfc3339(t).ok())
            .map(|t| t.with_timezone(&chrono::Utc))
            .unwrap_or_else(|| chrono::DateTime::<chrono::Utc>::UNIX_EPOCH);
        Some(OpencodeWatcher::new(
            port,
            workdir,
            &agent_id,
            not_before,
            Arc::clone(&self.cfg.now),
            Some(Arc::clone(&self.cfg.logf)),
        ))
    }
}

#[cfg(test)]
mod tests {
    use std::time::Duration;

    use super::super::hub_test_support::{
        count_events, drain_events, legacy_env, managed_env, rig,
    };
    use super::super::messages::FeedMessage;
    use super::*;

    fn activity_of(h: &Hub, slug: &str) -> Option<RcActivity> {
        let ts = h.lock_track();
        ts.tracked
            .get(slug)
            .unwrap_or_else(|| panic!("slug {slug} not tracked"))
            .activity
    }

    // ---- reconcile-loop transitions (hub_test.go:292-466) ----

    // Mirrors TestHubReconcileSessionAppearCarriesNoActivity: a session that
    // appears is announced with session.updated and carries NO activity. Since
    // S2 (`charliek/shed#324`) the only activity source is a correlated watcher,
    // and a codex row has none — the pane-stability engine that used to
    // manufacture a working→idle/needs_input verdict for every kind is gone (the
    // quiet-anchor and quiet-no-anchor cells went with it).
    #[test]
    fn reconcile_session_appear_carries_no_activity() {
        let (h, f, _clk) = rig();

        f.set(
            "rc-aaa111",
            "boot >_ OpenAI Codex (v1.0)\nline",
            &managed_env("id-1", &RcKind::Codex),
        );
        let sub = h.subscribe();

        h.reconcile();
        let evs = drain_events(&sub);
        assert!(
            count_events(&evs, "session.updated") > 0,
            "expected session.updated on appear, got {evs:?}"
        );
        assert_eq!(
            count_events(&evs, "activity.changed"),
            0,
            "a watcherless session must announce no activity: {evs:?}"
        );
        assert_eq!(activity_of(&h, "aaa111"), None);
    }

    // Mirrors TestHubReconcileDisappearEmitsGone.
    #[test]
    fn reconcile_disappear_emits_gone() {
        let (h, f, _clk) = rig();

        f.set(
            "rc-ddd444",
            "boot >_ OpenAI Codex (v1.0)",
            &managed_env("id-4", &RcKind::Codex),
        );
        h.reconcile();
        let sub = h.subscribe();

        f.remove("rc-ddd444");
        h.reconcile();
        let evs = drain_events(&sub);
        assert!(
            evs.iter()
                .any(|e| e.name == "session.updated" && e.raw.contains(r#""session":null"#)),
            "expected session.updated with session:null on disappear, got {evs:?}"
        );
        assert!(
            !h.lock_track().tracked.contains_key("ddd444"),
            "disappeared session should be dropped from tracked"
        );
    }

    // Mirrors TestHubReconcileSkipsOnTransientListFailure.
    #[test]
    fn reconcile_skips_on_transient_list_failure() {
        let (h, f, clk) = rig();

        f.set(
            "rc-lsf111",
            "boot >_ OpenAI Codex (v1.0)",
            &managed_env("id-lsf", &RcKind::Codex),
        );
        h.reconcile();
        {
            let ts = h.lock_track();
            let tr = ts.tracked.get("lsf111").expect("precondition: tracked");
            tr.ring.append(
                FeedMessage {
                    role: "assistant".into(),
                    typ: "text".into(),
                    text: "kept".into(),
                    ..FeedMessage::default()
                },
                clk.now(),
            );
        }

        let sub = h.subscribe();
        f.set_ls_fail("error connecting to /tmp/tmux-1000/default (transient)");
        h.reconcile(); // must be a no-op pass

        assert!(
            drain_events(&sub).is_empty(),
            "a skipped pass must emit no events"
        );
        {
            let ts = h.lock_track();
            let tr = ts
                .tracked
                .get("lsf111")
                .expect("tracked state must survive a transient listing failure");
            let (msgs, _) = tr.ring.since(0, 10);
            assert_eq!(
                msgs.len(),
                1,
                "the session ring must survive a transient listing failure"
            );
            assert!(
                ts.idle_since.is_none(),
                "a skipped pass must not start the idle-exit clock"
            );
        }

        // The failure clears → normal reconcile resumes with the same entry
        // (the surviving ring content is the same-entry witness).
        f.set_ls_fail("");
        h.reconcile();
        {
            let ts = h.lock_track();
            let (msgs, _) = ts
                .tracked
                .get("lsf111")
                .expect("still tracked")
                .ring
                .since(0, 10);
            assert_eq!(
                msgs.len(),
                1,
                "recovery must keep the same entry (no reset)"
            );
        }

        // Contrast: a genuine "no server running" answer IS everything-gone.
        f.set_ls_fail("no server running on /tmp/tmux-1000/default");
        h.reconcile();
        assert!(
            !h.lock_track().tracked.contains_key("lsf111"),
            "a no-server answer must drop the tracked session (genuinely gone)"
        );
    }

    // S2 (`charliek/shed#324`) removed `reconcile_state_change_emits_session_updated`,
    // `no_activity_changed_on_suppression` and `reconcile_lifecycle_trumps_activity`
    // with their Go twins: all three moved a LIVE session between lifecycle
    // states by editing its pane, which liveness no longer allows. The
    // lifecycle-trumps-activity precedence itself survives untouched in
    // `display_activity` and is pinned by its own cells.

    // Mirrors TestHubReconcileLegacyRecreateByCreatedAt.
    #[test]
    fn reconcile_legacy_recreate_by_created_at() {
        let (h, f, _clk) = rig();

        f.set(
            "rc-leg111",
            "output A",
            &legacy_env(&RcKind::Shell, "2026-01-01T00:00:00Z"),
        );
        h.reconcile();
        let sub = h.subscribe();

        // Same slug, still no id, NEW created_at → a recreate.
        f.set(
            "rc-leg111",
            "output A",
            &legacy_env(&RcKind::Shell, "2026-01-02T00:00:00Z"),
        );
        h.reconcile();

        let evs = drain_events(&sub);
        assert!(
            count_events(&evs, "session.updated") > 0,
            "expected session.updated on a created_at-detected recreate, got {evs:?}"
        );
        let ts = h.lock_track();
        let tr = ts.tracked.get("leg111").expect("tracked");
        assert_eq!(
            tr.created_at, "2026-01-02T00:00:00Z",
            "tracker not reset to the recreated identity"
        );
    }

    // Mirrors TestHubReconcileKindChangeIsARecreate: a stale kind would
    // become an authorization bug the day any kind advertises a verb.
    #[test]
    fn reconcile_kind_change_is_a_recreate() {
        let (h, f, _clk) = rig();

        // No SHED_RC_ID, fixed created_at: the kind is the only delta.
        const CREATED: &str = "2026-01-01T00:00:00Z";
        f.set(
            "rc-kchg11",
            "output A",
            &legacy_env(&RcKind::Shell, CREATED),
        );
        h.reconcile();
        let sub = h.subscribe();

        f.set(
            "rc-kchg11",
            "output A",
            &legacy_env(&RcKind::Codex, CREATED),
        );
        h.reconcile();

        let evs = drain_events(&sub);
        assert!(
            count_events(&evs, "session.updated") > 0,
            "expected session.updated on a kind-detected recreate, got {evs:?}"
        );
        assert_eq!(
            h.lock_track().tracked.get("kchg11").expect("tracked").kind,
            RcKind::Codex,
            "tracker kept the stale kind"
        );
    }

    // ---- idle-exit with the injected clock (hub_test.go:607-672) ----

    // Mirrors TestHubIdleExit (the resolved default IS Go's explicit 15m).
    #[test]
    fn idle_exit_after_timeout() {
        let (h, _f, clk) = rig();

        h.reconcile(); // no sessions: idle clock starts
        assert!(!h.should_idle_exit(clk.now()), "not immediately");
        clk.advance(Duration::from_secs(14 * 60));
        assert!(!h.should_idle_exit(clk.now()), "not before the timeout");
        clk.advance(Duration::from_secs(2 * 60));
        assert!(
            h.should_idle_exit(clk.now()),
            "should idle-exit after the timeout with zero sessions"
        );
    }

    // Mirrors TestHubIdleClockResetsWhenSessionAppears.
    #[test]
    fn idle_clock_resets_when_session_appears() {
        let (h, f, clk) = rig();

        h.reconcile(); // zero sessions → idle clock starts
        clk.advance(Duration::from_secs(20 * 60));
        f.set(
            "rc-ggg777",
            "boot >_ OpenAI Codex (v1.0)",
            &managed_env("id-7", &RcKind::Codex),
        );
        h.reconcile(); // resets idle clock
        assert!(
            !h.should_idle_exit(clk.now()),
            "idle clock must reset once a session exists"
        );
    }

    // Mirrors TestHubSubscribersDoNotBlockIdleExit.
    #[test]
    fn subscribers_do_not_block_idle_exit() {
        let (h, _f, clk) = rig();
        let sub = h.subscribe();
        h.reconcile();
        clk.advance(Duration::from_secs(16 * 60));
        assert!(
            h.should_idle_exit(clk.now()),
            "zero sessions + subscriber attached must still idle-exit"
        );
        h.close_all_subscribers();
        assert!(
            sub.is_closed(),
            "closeAllSubscribers must close the subscriber's stream"
        );
    }

    // The PANE-ANCHOR APPROVAL suite closed this module: debounced
    // detect/clear, single-tick blips, monotonic episode ids, the long-tool-call
    // and quoted-prose negatives, the SSE+snapshot reach, the scrollback
    // exclusion, the blocked-lifecycle and death exits, the last_message
    // override, `first_anchor_line`, and cursor's episode. Every one of them
    // drove an anchor S2 (`charliek/shed#324`) deleted.
}
