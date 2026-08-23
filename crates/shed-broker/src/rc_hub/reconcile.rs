//! The reconcile loop — `internal/ext/rc/hub_reconcile.go`. The hub's
//! heartbeat: on each tick it enumerates the shed's rc-* tmux sessions, runs a
//! StabilityTracker per session to derive live activity, and emits SSE events
//! on transitions:
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
//! the entry phase CLONES what the heavy phase needs (the tracker and watcher
//! `Arc`s, the pane-approval value, the reconcile-only counters) under the
//! track lock, the heavy phase runs unlocked against those clones (the
//! sub-objects are self-synchronized — tracker/watcher/ring each carry their
//! own mutex — or reconcile-only), and the commit phase re-acquires the lock
//! and writes the handler-visible fields back. The re-lookup on re-acquire is
//! guaranteed to find the same entry BECAUSE reconcile is the sole writer.
//! Handlers may interleave at any unlock boundary — the same windows Go
//! allows. Lock order: track → ingest → watcher.mu, never reversed.

use std::sync::{Arc, Mutex};

use chrono::SecondsFormat;
use regex::Regex;
use shed_core::rc::{RcActivity, RcKind, RcSessionDto, RcState};
use shed_core::rc_agents::{approval_anchor_for, parse_session};
use shed_rc_engine::tmux::Tmux;

use super::events::{
    activity_changed_event, message_appended_event, session_gone_event, session_updated_event,
    HubEvent,
};
use super::hub::{display_activity, Hub};
use super::messages::{
    sanitize_last_message, FeedApproval, FeedMessage, MessageRing, APPROVAL_STATUS_PENDING,
    APPROVAL_STATUS_RESOLVED, FEED_ROLE_TOOL, FEED_TYPE_APPROVAL_REQUEST,
};
use super::stability::StabilityTracker;
use super::watch::{
    agent_session_env, back_write_agent_session, merged_activity, opencode_port_env,
    parse_jsonl_time, watchable_kind, FileWatcher, SessionWatcher,
};
use super::watch_claude::{correlate_claude, ClaudeFold};
use super::watch_codex::{correlate_codex, CodexFold};
use super::watch_cursor::CursorWatcher;
use super::watch_opencode_transport::OpencodeWatcher;

/// How many CONSECUTIVE ticks the anchor must agree before the hub changes its
/// mind — in BOTH directions (`paneApprovalDebounceTicks`,
/// `hub_reconcile.go:113`). Two ticks (4s at the active cadence) is the
/// smallest value that costs a single-tick blip nothing; the cost is one tick
/// of latency on a genuine transition, far below human reaction time on the
/// phone this signal exists for.
pub const PANE_APPROVAL_DEBOUNCE_TICKS: u32 = 2;

/// One session's debounced pane-anchor approval episode (`paneApprovalState`,
/// `hub_reconcile.go:123`). An EPISODE runs from a debounced detection to a
/// debounced clear and owns exactly one id and exactly one pending feed row;
/// ids are `pane-<n>` with n monotonic per session (per hub lifetime).
///
/// Reconcile-only: the streak counters are never read outside the reconcile
/// pass, and the handler-visible part (pending/id) is committed under the
/// track lock with the rest of the tick's output (the whole value is copied
/// out for the heavy phase and committed back, as Go copies the struct).
#[derive(Debug, Clone, Default)]
pub(crate) struct PaneApprovalState {
    match_ticks: u32,
    clear_ticks: u32,
    /// Debounced verdict: an approval episode is open.
    pub pending: bool,
    pub id: String,
    /// The open episode's one-line summary (the matched option row). It
    /// stands in for the session's last_message while the episode runs: the
    /// watcher's own preview describes the tool call the dialog SUSPENDED,
    /// which on a phone reads as "the agent is busy doing this" at the exact
    /// moment the truth is "the agent is waiting on you".
    pub text: String,
    episodes: u32,
}

impl PaneApprovalState {
    /// Drops an open episode WITHOUT announcing a resolution (`abandon`,
    /// `hub_reconcile.go:141`) — the exit for a session that stopped being
    /// observable (a blocking lifecycle state) rather than one whose dialog
    /// was answered. `episodes` is deliberately NOT reset: ids stay monotonic
    /// for the session's whole life.
    pub(crate) fn abandon(&mut self) {
        self.match_ticks = 0;
        self.clear_ticks = 0;
        self.pending = false;
        self.id.clear();
        self.text.clear();
    }

    /// Folds one tick's anchor verdict into the debounce and reports a
    /// debounced TRANSITION as the feed status it should announce (`observe`,
    /// `hub_reconcile.go:153`): `(pending, id)` when an episode just opened,
    /// `(resolved, id)` when the dialog is provably gone, `None` (the common
    /// case — Go's `("", "")`) when nothing changed.
    pub(crate) fn observe(&mut self, matched: bool) -> Option<(&'static str, String)> {
        if matched {
            self.match_ticks += 1;
            self.clear_ticks = 0;
        } else {
            self.clear_ticks += 1;
            self.match_ticks = 0;
        }
        if !self.pending && matched && self.match_ticks >= PANE_APPROVAL_DEBOUNCE_TICKS {
            self.pending = true;
            self.episodes += 1;
            self.id = format!("pane-{}", self.episodes);
            return Some((APPROVAL_STATUS_PENDING, self.id.clone()));
        }
        if self.pending && !matched && self.clear_ticks >= PANE_APPROVAL_DEBOUNCE_TICKS {
            let id = std::mem::take(&mut self.id);
            self.pending = false;
            return Some((APPROVAL_STATUS_RESOLVED, id));
        }
        None
    }
}

/// Builds an INFORMATIONAL approval_request feed row for a pane-derived
/// episode (`paneApprovalRow`, `hub_reconcile.go:182`). `tool` is omitted (the
/// hub never learns which call the dialog guards) and `decisions` is omitted
/// (approvals:"tui" — nothing the hub can honor remotely). A resolved row
/// additionally omits `decision`: the operator answered in the TUI and the
/// hub cannot know which way they went.
pub(crate) fn pane_approval_row(id: &str, status: &str, text: &str) -> FeedMessage {
    FeedMessage {
        role: FEED_ROLE_TOOL.to_string(),
        typ: FEED_TYPE_APPROVAL_REQUEST.to_string(),
        text: text.to_string(),
        approval: Some(FeedApproval {
            id: id.to_string(),
            status: status.to_string(),
            ..FeedApproval::default()
        }),
        ..FeedMessage::default()
    }
}

/// The WHOLE pane line containing the anchor's first match, as a sanitized
/// one-line preview (`firstAnchorLine`, `hub_reconcile.go:196`) — expanded to
/// the enclosing line so the row reads as what the operator sees on screen
/// even for an anchor that matches a fragment (or a multi-line span of
/// chrome).
pub(crate) fn first_anchor_line(anchor: &Regex, pane: &str) -> String {
    let Some(m) = anchor.find(pane) else {
        return String::new();
    };
    let start = pane[..m.start()].rfind('\n').map_or(0, |i| i + 1);
    let line = pane[start..].split('\n').next().unwrap_or("");
    sanitize_last_message(line)
}

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
    /// Behind its own mutex so the heavy phase can tick it with the track
    /// lock released (reconcile is its sole user — the lock is uncontended).
    pub tracker: Arc<Mutex<StabilityTracker>>,

    /// The DISPLAYED activity (DisplayActivity already applied): `None` means
    /// the activity dimension is suppressed (Go's "").
    pub activity: Option<RcActivity>,
    /// RFC3339 time the displayed activity last changed.
    pub activity_at: String,
    /// Sanitized preview from the watcher ("" from stability).
    pub last_message: String,
    pub last_state: RcState,

    /// The session's structured-signal watcher (JSONL tail / opencode SSE /
    /// cursor push), lazily created (`ensureWatcher`). `None` for kinds with
    /// no structured signal, or before correlation succeeds.
    pub watcher: Option<Arc<dyn SessionWatcher + Send + Sync>>,
    /// An AMBIGUOUS correlation's agent session id, held back until the
    /// watcher's first in-file event confirms the pick — only then back-
    /// written to SHED_RC_AGENT_SESSION (a wrong pin would be permanent).
    pub pending_agent_id: String,
    /// Correlation attempts, so a session whose file never appears stops
    /// re-scanning the filesystem.
    pub correlate_tried: u32,

    /// The session's message feed. Every tracked session has one so /messages
    /// returns 200-empty for a known slug. Self-synchronized.
    pub ring: Arc<MessageRing>,
    /// The raw pane-stability verdict from the most recent successful tick
    /// (before the watcher merge / DisplayActivity) — the input handler reads
    /// it so its acceptance re-check runs the SAME merge reconcile uses.
    /// [`RcActivity::Unknown`] covers Go's "" (no verdict yet).
    pub last_stability: RcActivity,

    /// The currently-open approval requests, republished every tick from a
    /// lane that knows its approvals (ApprovalPublisher — opencode). PENDING
    /// ONLY by wire contract.
    pub pending_approvals: Vec<FeedApproval>,
    /// The PANE-derived approval state (ApprovalAnchor kinds — codex and
    /// cursor). Deliberately a SEPARATE field from pending_approvals: that
    /// field is wholly owned by the publishing watcher, so a pane entry
    /// living there would be blanked by the next republish. Reconcile is its
    /// sole writer; the /v1/sessions overlay unions the two.
    pub pane_approval: PaneApprovalState,
}

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
    /// `hub_reconcile.go:215`): the lane-published entries UNIONED with the
    /// open pane-derived episode. Unioned rather than switched so a kind that
    /// someday has both keeps every open ask visible. The pane entry carries
    /// no `decisions` (approvals:"tui" — a capability-driven client renders
    /// zero decision buttons). Called under the track lock; the caller
    /// deep-copies before serving.
    pub(crate) fn approval_snapshot(&self) -> Vec<FeedApproval> {
        if !self.pane_approval.pending {
            return self.pending_approvals.clone();
        }
        let mut out = self.pending_approvals.clone();
        out.push(FeedApproval {
            id: self.pane_approval.id.clone(),
            status: APPROVAL_STATUS_PENDING.to_string(),
            ..FeedApproval::default()
        });
        out
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

/// Bounds how many reconcile ticks a session's correlation is re-attempted
/// (`maxCorrelateTries`, `hub_reconcile.go:540`): ~40 ticks at the active
/// cadence is >1min of retry, comfortably longer than the file-appears
/// latency, while a never-appearing file stops re-scanning forever.
pub const MAX_CORRELATE_TRIES: u32 = 40;

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
    tracker: Arc<Mutex<StabilityTracker>>,
    watcher: Option<Arc<dyn SessionWatcher + Send + Sync>>,
    ring: Arc<MessageRing>,
    pane_approval: PaneApprovalState,
    pending_agent_id: String,
    correlate_tried: u32,
}

impl Hub {
    /// Builds tracker state for a freshly seen session (`newTrackedSession`,
    /// `hub_reconcile.go:227`). The tracker captures the session's pane on
    /// demand via the hub's runner — a modest extra capture-pane per tick.
    pub(crate) fn new_tracked_session(&self, s: &RcSessionDto) -> TrackedSession {
        let name = s.tmux_session.clone();
        let runner = Arc::clone(&self.cfg.runner);
        let capture = Box::new(move || {
            let res = Tmux::new(&*runner).capture_pane(&name);
            if res.code != 0 {
                return Err(format!("capture-pane {name} failed: {}", res.stderr));
            }
            Ok(res.stdout)
        });
        let now = Arc::clone(&self.cfg.now);
        TrackedSession {
            id: s.id.clone().unwrap_or_default(),
            created_at: s.created_at.clone().unwrap_or_default(),
            kind: s.kind.clone(),
            tracker: Arc::new(Mutex::new(StabilityTracker::new(
                &s.kind,
                capture,
                Box::new(move || now()),
                self.cfg.quiet,
            ))),
            activity: None,
            activity_at: String::new(),
            last_message: String::new(),
            last_state: s.state,
            watcher: None,
            pending_agent_id: String::new(),
            correlate_tried: 0,
            ring: Arc::new(MessageRing::new()),
            last_stability: RcActivity::Unknown,
            pending_approvals: Vec::new(),
            pane_approval: PaneApprovalState::default(),
        }
    }

    /// Hands a freshly built watcher every event queued for its slug
    /// (`drainPreWatcher`, `hub_ingest.go:185` — the Hub-level wrapper: the
    /// cursorIngester type-assert lives here; a non-ingesting watcher leaves
    /// the queue untouched for the TTL janitor).
    pub(crate) fn drain_pre_watcher(&self, slug: &str, watcher: &dyn SessionWatcher) {
        let Some(ing) = watcher.as_cursor_ingester() else {
            return;
        };
        self.ingest.drain(slug, ing);
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
                    tracker: Arc::clone(&tr.tracker),
                    watcher: tr.watcher.clone(),
                    ring: Arc::clone(&tr.ring),
                    pane_approval: tr.pane_approval.clone(),
                    pending_agent_id: tr.pending_agent_id.clone(),
                    correlate_tried: tr.correlate_tried,
                }
            };

            // --- Heavy tmux/disk/network work, track lock RELEASED
            // (`hub_reconcile.go:313`). The clones above stay valid because
            // reconcile is the sole writer; the sub-objects touched here are
            // self-synchronized (tracker, ring, watcher) or reconcile-only
            // (the counters, committed back below). ---
            let mut pending_agent_id = snap.pending_agent_id;
            let mut correlate_tried = snap.correlate_tried;
            let new_w = self.ensure_watcher(
                snap.watcher.is_some(),
                &mut correlate_tried,
                &mut pending_agent_id,
                s,
            );
            let watcher = new_w.clone().or(snap.watcher);

            // Derive activity. The pane-stability tracker is the universal
            // fallback; a fresh watcher overrides it (mergedActivity). A
            // capture error with no fresh watcher leaves the prior activity
            // untouched rather than flapping the DTO to unknown.
            let (raw, cap_err) = match snap
                .tracker
                .lock()
                .unwrap_or_else(std::sync::PoisonError::into_inner)
                .tick()
            {
                Ok(a) => (a, false),
                Err(_) => (RcActivity::Unknown, true),
            };

            let mut watcher_activity = RcActivity::Unknown;
            let mut watcher_message = String::new();
            let (mut watcher_fresh, mut watcher_expired_working) = (false, false);
            let mut msg_events: Vec<HubEvent> = Vec::new();
            let mut pending_approvals: Vec<FeedApproval> = Vec::new();
            let mut publishes_approvals = false;
            if let Some(w) = &watcher {
                w.refresh(now);
                // A deferred (ambiguous-correlation) back-write happens only
                // once the first in-file event confirms the pick.
                if !pending_agent_id.is_empty() && w.had_event() {
                    back_write_agent_session(&tmux, &s.tmux_session, &pending_agent_id);
                    pending_agent_id.clear();
                }
                // The opencode/cursor watchers correlate ASYNC: once they pin
                // the session id they surface it here for back-write into
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
                (
                    watcher_activity,
                    watcher_message,
                    watcher_fresh,
                    watcher_expired_working,
                ) = w.snapshot(now);
            }

            // Pane-anchor approvals (ApprovalAnchor kinds: codex and cursor) —
            // the dialog reaches no protocol, so the pane is the only
            // evidence. Its OWN capture, deliberately: the tracker's frame
            // carries 200 lines of SCROLLBACK, and scrollback is exactly the
            // wrong thing to ask "is a modal on screen?" — an answered dialog
            // stays in the history verbatim, so an episode opened off
            // scrollback could never clear. A capture failure means NO
            // EVIDENCE, not "cleared": the episode is held untouched.
            let mut pane_approval = snap.pane_approval;
            if let Some(anchor) = approval_anchor_for(&s.kind) {
                if display_activity(s.state, RcActivity::Working).is_none() {
                    // Blocking lifecycle state. A session that DIED
                    // mid-dialog resolved nothing — the open episode is
                    // dropped SILENTLY, no resolved row (session.updated
                    // already carries the state change).
                    pane_approval.abandon();
                } else {
                    let vis = tmux.capture_visible_pane(&s.tmux_session);
                    if vis.code == 0 {
                        // Exactly ONE pending row per episode and one
                        // resolved row on its debounced clear.
                        if let Some((status, id)) =
                            pane_approval.observe(anchor.is_match(&vis.stdout))
                        {
                            // A resolved row has nothing to preview: the
                            // dialog is gone.
                            let text = if status == APPROVAL_STATUS_PENDING {
                                first_anchor_line(anchor, &vis.stdout)
                            } else {
                                String::new()
                            };
                            // Retained for the episode's duration: while open
                            // it REPLACES last_message (the merge below), and
                            // the resolved transition clears it.
                            pane_approval.text = text.clone();
                            let seq = snap.ring.append(pane_approval_row(&id, status, &text), now);
                            msg_events.push(message_appended_event(&s.slug, seq));
                        }
                    }
                }
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
                    // SECOND drain of the cursor pre-watcher queue, the
                    // load-bearing one: until the assignment above, the
                    // ingest handler still saw watcher == None and kept
                    // queueing; those events would land in a fresh queue
                    // nothing ever drains (ensureWatcher no-ops once the
                    // watcher is set). Draining HERE, immediately after the
                    // publish, closes the window. Lock order: track → ingest
                    // → watcher.mu.
                    self.drain_pre_watcher(&s.slug, &**w);
                }
                // Only a publishing watcher owns this field: a kind whose
                // approvals are not lane-derived must keep what it holds
                // rather than be blanked by an unrelated tick.
                if publishes_approvals {
                    tr.pending_approvals = pending_approvals;
                }
                // Captured BEFORE the move so the override below still has the
                // episode's summary; Go re-reads its own struct copy, which is
                // free there and a two-String clone here.
                let episode_text = pane_approval.pending.then(|| pane_approval.text.clone());
                tr.pane_approval = pane_approval;
                tr.pending_agent_id = pending_agent_id;
                tr.correlate_tried = correlate_tried;
                if !cap_err {
                    // Remember the raw stability verdict for the input
                    // handler's acceptance re-check.
                    tr.last_stability = raw;
                }
                events.append(&mut msg_events);

                if cap_err && !watcher_fresh {
                    continue; // no signal this tick; retain the prior verdict
                }

                let (mut merged_raw, mut merged_msg) = merged_activity(
                    watcher_activity,
                    &watcher_message,
                    watcher_fresh,
                    watcher_expired_working,
                    raw,
                );
                // An open pane-anchor episode OVERRIDES the merge: the dialog
                // owns the session, and every other signal describes the
                // suspended work underneath it (codex's rollout still reads
                // `working`; pane stability reads the frozen dialog as idle —
                // both would be wrong). Lifecycle still trumps this, via
                // DisplayActivity below. last_message rides along: the
                // episode's own summary is what the client should show, and
                // the normal merge is restored the moment the episode clears.
                if let Some(text) = episode_text {
                    merged_raw = RcActivity::NeedsApproval;
                    merged_msg = text;
                }
                let eff = display_activity(s.state, merged_raw);
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
                    // Release the watcher (its tail/SSE now points at a dead
                    // session) and prune the slug's input lock (a recreate
                    // later gets a fresh one; a request already holding the
                    // old lock finishes against a gone pane harmlessly).
                    if let Some(w) = tr.watcher {
                        w.close();
                    }
                }
                self.prune_input_lock(&slug);
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

        // Janitor for the cursor ingest queues: runs after the track release —
        // it takes its own lock and the two are never held together.
        self.ingest.prune(now, &present);

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
    /// RETURNING it (`ensureWatcher`, `hub_reconcile.go:557`; `None` when none
    /// was created this call). Runs UNLOCKED from reconcile, so it must NOT
    /// publish the watcher — the caller commits it under the track lock. It
    /// DOES mutate the correlate/pending counters, which are reconcile-only
    /// (passed by `&mut` here, committed back by the caller — the Rust shape
    /// of Go mutating `tr` fields handlers never read).
    fn ensure_watcher(
        &self,
        watcher_exists: bool,
        correlate_tried: &mut u32,
        pending_agent_id: &mut String,
        s: &RcSessionDto,
    ) -> Option<Arc<dyn SessionWatcher + Send + Sync>> {
        if watcher_exists || !watchable_kind(&s.kind) {
            return None;
        }
        match s.state {
            RcState::NeedsTrust | RcState::NeedsAuth | RcState::Dead => {
                return None; // no live activity to tail; retry once usable
            }
            _ => {}
        }
        let tmux = Tmux::new(&*self.cfg.runner);
        let workdir = s.workdir.as_deref().unwrap_or("");

        // opencode diverges from the file-correlation path: its watcher owns
        // its OWN async correlation over SSE/REST (non-blocking construction)
        // and needs none of the retry-budget machinery. A session with no
        // valid recorded port is unwatchable over this transport → pane
        // stability drives.
        if s.kind == RcKind::Opencode {
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
            return Some(OpencodeWatcher::new(
                port,
                workdir,
                &agent_id,
                not_before,
                Arc::clone(&self.cfg.now),
                Some(Arc::clone(&self.cfg.logf)),
            ));
        }

        // cursor diverges from BOTH paths: push-fed by the agent's own hook
        // scripts — nothing to correlate, nothing to connect. Built on the
        // first eligible tick, immediately usable; the pin arrives inside the
        // hook payloads. The one construction-time step is draining the
        // pre-watcher queue into it (the kickoff prompt above all), so those
        // events fold on this very tick.
        if s.kind == RcKind::Cursor {
            let w = Arc::new(CursorWatcher::new(
                &agent_session_env(&tmux, &s.tmux_session),
                Some(Arc::clone(&self.cfg.logf)),
            ));
            self.drain_pre_watcher(&s.slug, &*w);
            // Restart backfill: a VALIDATED prior pin (a hub restart
            // mid-session) means there is a transcript worth reading once. A
            // fresh session attempts no read at all. Best-effort.
            let prior = w.prior_id();
            if !prior.is_empty() {
                w.seed_from_transcript(&(self.cfg.getenv)("HOME"), workdir, &prior);
            }
            return Some(w);
        }

        if *correlate_tried >= MAX_CORRELATE_TRIES {
            return None;
        }
        *correlate_tried += 1;

        let created_at = parse_jsonl_time(s.created_at.as_deref().unwrap_or(""));
        let agent_id = agent_session_env(&tmux, &s.tmux_session);
        let getenv: &dyn Fn(&str) -> String = &*self.cfg.getenv;

        let (corr, fold): (_, Box<dyn super::watch::ActivityFold + Send>) =
            if s.kind == RcKind::Codex {
                (
                    correlate_codex(getenv, workdir, &agent_id, created_at),
                    Box::new(CodexFold::new()),
                )
            } else if s.kind.runs_claude() {
                (
                    correlate_claude(getenv, workdir, &agent_id, created_at),
                    Box::new(ClaudeFold::new()),
                )
            } else {
                return None;
            };
        let corr = corr?;

        // Unambiguous match → a bounded catch-up read so the current activity
        // is known immediately. Ambiguous → follow only new appends (unknown
        // until an event confirms which file is really this session's).
        let w: Arc<dyn SessionWatcher + Send + Sync> =
            Arc::new(FileWatcher::new(&corr.path, !corr.ambiguous, fold));
        if !agent_id.is_empty() || corr.session_id.is_empty() {
            return Some(w);
        }
        if corr.ambiguous {
            // NEVER back-write an ambiguous pick immediately: a wrong id
            // stamped into the tmux env would be trusted by the exact-id path
            // on every future hub restart, making the mistake permanent. Held
            // until the watcher's first in-file event confirms the pick.
            *pending_agent_id = corr.session_id;
            return Some(w);
        }
        back_write_agent_session(&tmux, &s.tmux_session, &corr.session_id);
        Some(w)
    }
}

#[cfg(test)]
mod tests {
    use std::sync::Arc;
    use std::time::Duration;

    use super::super::hub_test_support::{
        count_events, drain_events, legacy_env, managed_env, pane_fixture, rig, HubTmux,
    };
    use super::super::messages::MAX_MESSAGES_LIMIT;
    use super::*;

    /// The codex empty-composer placeholder (`codexComposerPlaceholder` —
    /// pinned as a literal in rc_agents; inlined here as Go's test inlines the
    /// shared const).
    const CODEX_COMPOSER: &str = "Find and fix a bug in @filename";

    fn activity_of(h: &Hub, slug: &str) -> Option<RcActivity> {
        let ts = h.lock_track();
        ts.tracked
            .get(slug)
            .unwrap_or_else(|| panic!("slug {slug} not tracked"))
            .activity
    }

    fn last_message_of(h: &Hub, slug: &str) -> String {
        h.lock_track()
            .tracked
            .get(slug)
            .expect("tracked")
            .last_message
            .clone()
    }

    /// The session's approval_request rows, oldest first
    /// (`paneApprovalRows`, `hub_pane_approvals_test.go:94`).
    fn pane_approval_rows(h: &Hub, slug: &str) -> Vec<FeedMessage> {
        let ring = Arc::clone(&h.lock_track().tracked.get(slug).expect("tracked").ring);
        let (msgs, _) = ring.since(0, MAX_MESSAGES_LIMIT as i64);
        msgs.into_iter()
            .filter(|m| m.typ == FEED_TYPE_APPROVAL_REQUEST)
            .collect()
    }

    /// The pending_approvals overlay, the way handleSessions reads it
    /// (`hubPendingApprovals`, `hub_pane_approvals_test.go:126`).
    fn pending_approvals_of(h: &Hub, slug: &str) -> Vec<FeedApproval> {
        let ts = h.lock_track();
        copy_approvals(&ts.tracked.get(slug).expect("tracked").approval_snapshot())
    }

    // ---- reconcile-loop transitions (hub_test.go:292-466) ----

    // Mirrors TestHubReconcileSessionAppearAndActivity.
    #[test]
    fn reconcile_session_appear_and_activity() {
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
        assert!(
            count_events(&evs, "activity.changed") > 0,
            "expected activity.changed (working) on first tick, got {evs:?}"
        );
        assert_eq!(activity_of(&h, "aaa111"), Some(RcActivity::Working));
    }

    // Mirrors TestHubReconcileQuietAnchorNeedsInput.
    #[test]
    fn reconcile_quiet_anchor_needs_input() {
        let (h, f, clk) = rig();

        let pane = format!("> {CODEX_COMPOSER}\nother");
        f.set("rc-bbb222", &pane, &managed_env("id-2", &RcKind::Codex));

        h.reconcile(); // first tick: working (fresh session "just changed")
        assert_eq!(activity_of(&h, "bbb222"), Some(RcActivity::Working));

        clk.advance(Duration::from_secs(5));
        let sub = h.subscribe();
        h.reconcile();
        assert_eq!(activity_of(&h, "bbb222"), Some(RcActivity::NeedsInput));
        let evs = drain_events(&sub);
        assert!(
            count_events(&evs, "activity.changed") > 0,
            "expected activity.changed on working→needs_input, got {evs:?}"
        );
    }

    // Mirrors TestHubReconcileQuietNoAnchorIdle.
    #[test]
    fn reconcile_quiet_no_anchor_idle() {
        let (h, f, clk) = rig();

        f.set(
            "rc-ccc333",
            "some output line",
            &managed_env("id-3", &RcKind::Shell),
        );
        h.reconcile(); // working
        clk.advance(Duration::from_secs(5));
        h.reconcile(); // quiet, no anchor → idle
        assert_eq!(activity_of(&h, "ccc333"), Some(RcActivity::Idle));
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

    // Mirrors TestHubReconcileStateChangeEmitsSessionUpdated.
    #[test]
    fn reconcile_state_change_emits_session_updated() {
        let (h, f, _clk) = rig();

        f.set("rc-eee555", "booting", &managed_env("id-5", &RcKind::Codex));
        h.reconcile();
        let sub = h.subscribe();

        f.set_pane("rc-eee555", "boot >_ OpenAI Codex (v1.0)");
        h.reconcile();
        let evs = drain_events(&sub);
        assert!(
            count_events(&evs, "session.updated") > 0,
            "expected session.updated on lifecycle state change, got {evs:?}"
        );
    }

    // Mirrors TestHubNoActivityChangedOnSuppression: activity.changed carries
    // only valid non-empty values — a transition INTO suppression emits
    // session.updated and NO activity.changed.
    #[test]
    fn no_activity_changed_on_suppression() {
        let (h, f, _clk) = rig();

        f.set(
            "rc-sup111",
            "boot >_ OpenAI Codex (v1.0)",
            &managed_env("id-sup", &RcKind::Codex),
        );
        h.reconcile();
        assert_eq!(activity_of(&h, "sup111"), Some(RcActivity::Working));

        let sub = h.subscribe();
        f.set_pane("rc-sup111", "Sign in with ChatGPT");
        h.reconcile();

        assert_eq!(activity_of(&h, "sup111"), None, "want suppressed");
        let evs = drain_events(&sub);
        assert!(
            count_events(&evs, "session.updated") > 0,
            "expected session.updated for the ready→needs-auth transition, got {evs:?}"
        );
        assert_eq!(
            count_events(&evs, "activity.changed"),
            0,
            "suppression must not emit activity.changed: {evs:?}"
        );
        for e in &evs {
            assert!(
                !e.raw.contains(r#""activity":"""#),
                "an event carried the empty activity value: {e:?}"
            );
        }
    }

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

    // Mirrors TestHubReconcileLifecycleTrumpsActivity.
    #[test]
    fn reconcile_lifecycle_trumps_activity() {
        let (h, f, _clk) = rig();

        f.set(
            "rc-fff666",
            "Sign in with ChatGPT",
            &managed_env("id-6", &RcKind::Codex),
        );
        h.reconcile();
        assert_eq!(
            activity_of(&h, "fff666"),
            None,
            "needs-auth session activity must be suppressed"
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

    // ---- pane-anchor approvals (hub_pane_approvals_test.go) ----

    /// The one codex session every pane-approval test drives
    /// (`paneApprovalHub`'s `"rc-pan001"`, `hub_pane_approvals_test.go:83`).
    const PAN_TMUX: &str = "rc-pan001";

    /// What [`pane_approval_hub`] hands back: the hub, the fake tmux, the
    /// pane-swapper, and the session's slug (Go's four named results).
    type PaneApprovalRig = (Hub, Arc<HubTmux>, Box<dyn Fn(&str)>, &'static str);

    /// A hub with one codex session showing `pane`, reconciled zero times
    /// (`paneApprovalHub`, `hub_pane_approvals_test.go:83`). `set_pane` is the
    /// common case (change what the whole pane shows for the next tick); `f` is
    /// returned for the tests that need the visible-frame seam or a
    /// lifecycle-state change; `slug` names it for the tracked-state helpers.
    fn pane_approval_hub(pane: &str) -> PaneApprovalRig {
        let (h, f, _clk) = rig();
        f.set(PAN_TMUX, pane, &managed_env("id-pan", &RcKind::Codex));
        let set_pane = {
            let f = Arc::clone(&f);
            Box::new(move |p: &str| f.set_pane(PAN_TMUX, p))
        };
        (h, f, set_pane, "pan001")
    }

    // Mirrors TestPaneApprovalDebounceDetectAndClear: TWO consecutive
    // matching ticks to detect, TWO non-matching ticks to clear — one
    // informational row per transition, the pending_approvals entry present
    // for exactly the open interval.
    #[test]
    fn pane_approval_debounce_detect_and_clear() {
        let approval = pane_fixture("codex-ready-approval-exec");
        let resolved = pane_fixture("codex-ready-approval-resolved");
        let (h, _f, set_pane, slug) = pane_approval_hub(&approval);

        // Tick 1: matched once — nothing on the wire.
        h.reconcile();
        assert_ne!(activity_of(&h, slug), Some(RcActivity::NeedsApproval));
        assert!(pane_approval_rows(&h, slug).is_empty());

        // Tick 2: debounced detection.
        h.reconcile();
        assert_eq!(activity_of(&h, slug), Some(RcActivity::NeedsApproval));
        let rows = pane_approval_rows(&h, slug);
        assert_eq!(rows.len(), 1, "exactly one row after detection: {rows:?}");
        let row = &rows[0];
        assert_eq!(row.role, FEED_ROLE_TOOL);
        let ap = row.approval.as_ref().expect("approval set");
        assert_eq!((ap.id.as_str(), ap.status.as_str()), ("pane-1", "pending"));
        // approvals stays "tui" for codex: NO decisions, no tool block, no
        // decision — a capability-driven client renders zero buttons.
        assert!(ap.decisions.is_empty() && ap.decision.is_empty() && row.tool.is_none());
        assert!(
            row.text.contains("Yes, proceed"),
            "row text = {:?}, want the first matching option row",
            row.text
        );
        let pend = pending_approvals_of(&h, slug);
        assert_eq!(pend.len(), 1);
        assert_eq!(
            (pend[0].id.as_str(), pend[0].status.as_str()),
            ("pane-1", "pending")
        );

        // Tick 3: dialog gone, but ONE non-matching tick must not clear it.
        set_pane(&resolved);
        h.reconcile();
        assert_eq!(
            activity_of(&h, slug),
            Some(RcActivity::NeedsApproval),
            "held through a single clearing tick"
        );
        assert_eq!(pane_approval_rows(&h, slug).len(), 1);

        // Tick 4: debounced clear.
        h.reconcile();
        assert_ne!(activity_of(&h, slug), Some(RcActivity::NeedsApproval));
        let rows = pane_approval_rows(&h, slug);
        assert_eq!(rows.len(), 2, "a resolved row after the clear: {rows:?}");
        let res = rows[1].approval.as_ref().expect("approval");
        assert_eq!(
            (res.id.as_str(), res.status.as_str()),
            ("pane-1", "resolved")
        );
        // The operator answered in the TUI; the hub cannot know which way.
        assert!(res.decision.is_empty());
        assert!(pending_approvals_of(&h, slug).is_empty());
    }

    // Mirrors TestPaneApprovalSingleTickBlipsIgnored — the anti-flap pin.
    #[test]
    fn pane_approval_single_tick_blips_ignored() {
        let approval = pane_fixture("codex-ready-approval-exec");
        let resolved = pane_fixture("codex-ready-approval-resolved");
        let (h, _f, set_pane, slug) = pane_approval_hub(&resolved);

        // Blip ON: quiet, one matching frame, quiet again.
        h.reconcile();
        set_pane(&approval);
        h.reconcile();
        set_pane(&resolved);
        h.reconcile();
        assert_ne!(activity_of(&h, slug), Some(RcActivity::NeedsApproval));
        assert!(pane_approval_rows(&h, slug).is_empty());

        // Open a real episode, then blip OFF for one tick.
        set_pane(&approval);
        h.reconcile();
        h.reconcile();
        assert_eq!(activity_of(&h, slug), Some(RcActivity::NeedsApproval));
        set_pane(&resolved);
        h.reconcile();
        set_pane(&approval);
        h.reconcile();
        assert_eq!(
            activity_of(&h, slug),
            Some(RcActivity::NeedsApproval),
            "a one-tick clearing blip must not close an open episode"
        );
        assert_eq!(pane_approval_rows(&h, slug).len(), 1);
    }

    // Mirrors TestPaneApprovalIDsMonotonicAcrossEpisodes.
    #[test]
    fn pane_approval_ids_monotonic_across_episodes() {
        let approval = pane_fixture("codex-ready-approval-exec");
        let resolved = pane_fixture("codex-ready-approval-resolved");
        let (h, _f, set_pane, slug) = pane_approval_hub(&approval);

        for pane in [
            &approval, &approval, &resolved, &resolved, &approval, &approval,
        ] {
            set_pane(pane);
            h.reconcile();
        }
        let rows = pane_approval_rows(&h, slug);
        assert_eq!(rows.len(), 3, "pending/resolved/pending: {rows:?}");
        let want = [
            ("pane-1", "pending"),
            ("pane-1", "resolved"),
            ("pane-2", "pending"),
        ];
        for (i, w) in want.iter().enumerate() {
            let ap = rows[i].approval.as_ref().expect("approval");
            assert_eq!((ap.id.as_str(), ap.status.as_str()), *w, "row {i}");
        }
        let pend = pending_approvals_of(&h, slug);
        assert_eq!(pend.len(), 1);
        assert_eq!(pend[0].id, "pane-2", "the second episode only");
    }

    // Mirrors TestPaneApprovalLongToolCallIsNotApproval — the negative that
    // matters most: only the pane distinguishes blocked-on-approval from a
    // slow tool, and a working pane must NEVER read needs_approval.
    #[test]
    fn pane_approval_long_tool_call_is_not_approval() {
        let base = pane_fixture("codex-ready-tool-running");
        let (h, _f, set_pane, slug) = pane_approval_hub(&base);

        for i in 0..6u8 {
            // The elapsed readout ticks every frame — genuinely working.
            let tick = base.replacen("2m14s", &format!("2m{}4s", (b'1' + i) as char), 1);
            set_pane(&tick);
            h.reconcile();
            assert_ne!(
                activity_of(&h, slug),
                Some(RcActivity::NeedsApproval),
                "tick {i}: a long tool call must not read needs_approval"
            );
        }
        assert!(pane_approval_rows(&h, slug).is_empty());
        assert!(pending_approvals_of(&h, slug).is_empty());
    }

    // Mirrors TestPaneApprovalQuotedProseIsNotApproval.
    #[test]
    fn pane_approval_quoted_prose_is_not_approval() {
        let (h, _f, _set_pane, slug) =
            pane_approval_hub(&pane_fixture("codex-ready-approval-quoted"));
        for i in 0..4 {
            h.reconcile();
            assert_ne!(
                activity_of(&h, slug),
                Some(RcActivity::NeedsApproval),
                "tick {i}: quoted prose must not read needs_approval"
            );
        }
        assert!(pane_approval_rows(&h, slug).is_empty());
    }

    // Mirrors TestPaneApprovalReachesSSEAndSessions' SSE half: needs_approval
    // reaches the activity.changed stream and the message.appended
    // notification through the normal machinery (the /v1/sessions HTTP half is
    // `sessions_pending_approvals_overlay` in hub_http_tests.rs; the overlay
    // source is asserted via approval_snapshot here).
    #[test]
    fn pane_approval_reaches_sse_and_snapshot() {
        let (h, _f, _set_pane, slug) =
            pane_approval_hub(&pane_fixture("codex-ready-approval-exec"));
        let sub = h.subscribe();
        h.reconcile();
        h.reconcile();

        let evs = drain_events(&sub);
        assert!(
            evs.iter().any(|e| e.name == "activity.changed"
                && e.raw.contains(r#""activity":"needs_approval""#)),
            "no activity.changed carrying needs_approval: {evs:?}"
        );
        assert!(
            count_events(&evs, "message.appended") > 0,
            "the informational approval row must announce itself as message.appended"
        );
        let pend = pending_approvals_of(&h, slug);
        assert_eq!(pend.len(), 1);
        assert_eq!(pend[0].id, "pane-1");
        assert!(
            pend[0].decisions.is_empty(),
            "codex approvals are answered in the TUI — no decisions advertised"
        );
    }

    // Mirrors TestPaneApprovalIgnoresScrollback: the anchor evaluates the
    // VISIBLE FRAME, never the scrollback.
    #[test]
    fn pane_approval_ignores_scrollback() {
        let approval = pane_fixture("codex-ready-approval-exec");
        let (h, f, _set_pane, slug) = pane_approval_hub(&approval);

        // Scrollback still carries the whole dialog; the visible frame moved on.
        f.set_visible(PAN_TMUX, &pane_fixture("codex-ready-approval-resolved"));
        for i in 0..4 {
            h.reconcile();
            assert_ne!(
                activity_of(&h, slug),
                Some(RcActivity::NeedsApproval),
                "tick {i}: chrome in the scrollback must not open an episode"
            );
        }
        assert!(pane_approval_rows(&h, slug).is_empty());

        // An OPEN episode clears when the dialog scrolls out of the visible
        // frame, even though the scrollback capture is unchanged.
        f.set_visible(PAN_TMUX, "");
        h.reconcile();
        h.reconcile();
        assert_eq!(activity_of(&h, slug), Some(RcActivity::NeedsApproval));
        f.set_visible(PAN_TMUX, &pane_fixture("codex-ready-approval-resolved"));
        h.reconcile();
        h.reconcile();
        assert_ne!(
            activity_of(&h, slug),
            Some(RcActivity::NeedsApproval),
            "an episode must clear once the dialog leaves the visible frame"
        );
    }

    // Mirrors TestPaneApprovalNotEvaluatedWhileLifecycleBlocks.
    #[test]
    fn pane_approval_not_evaluated_while_lifecycle_blocks() {
        // A codex pane showing the dialog AND the needs-auth signal —
        // classification wins. codexReadyRe wins over codexAuthRe on a pane
        // carrying both, so strip the banner to get a genuinely needs-auth
        // pane that still shows the dialog chrome.
        let pane = pane_fixture("codex-ready-approval-exec") + "\nSign in with ChatGPT\n";
        let stripped = pane
            .split_once('╰')
            .map(|(_, rest)| rest.to_string())
            .expect("fixture carries the banner box");
        let (h, f, _clk) = rig();
        f.set(
            "rc-pan002",
            &stripped,
            &managed_env("id-pan2", &RcKind::Codex),
        );

        for _ in 0..4 {
            h.reconcile();
        }
        assert_eq!(
            activity_of(&h, "pan002"),
            None,
            "want suppressed on a blocking lifecycle state"
        );
        assert!(pane_approval_rows(&h, "pan002").is_empty());
        assert!(pending_approvals_of(&h, "pan002").is_empty());
    }

    // Mirrors TestPaneApprovalDroppedSilentlyOnDeath: a death drops the
    // episode silently (no resolved row) and ids stay monotonic across it.
    #[test]
    fn pane_approval_dropped_silently_on_death() {
        let approval = pane_fixture("codex-ready-approval-exec");
        let (h, f, _set_pane, slug) = pane_approval_hub(&approval);

        h.reconcile();
        h.reconcile();
        assert_eq!(activity_of(&h, slug), Some(RcActivity::NeedsApproval));

        // codex exits with the dialog still on screen: the pane ends at a
        // shed shell prompt.
        f.set(
            PAN_TMUX,
            &format!("{approval}\n[shed:agent-fixtures] ~ $ "),
            &managed_env("id-pan", &RcKind::Codex),
        );
        h.reconcile();

        assert_eq!(
            activity_of(&h, slug),
            None,
            "want suppressed on a dead session"
        );
        let rows = pane_approval_rows(&h, slug);
        assert_eq!(rows.len(), 1, "a death must not append a resolved row");
        assert_eq!(
            rows[0].approval.as_ref().expect("approval").status,
            "pending"
        );
        assert!(
            pending_approvals_of(&h, slug).is_empty(),
            "the abandoned episode must leave the snapshot"
        );

        // Recovery: ids do not rewind — a new dialog is pane-2.
        f.set(PAN_TMUX, &approval, &managed_env("id-pan", &RcKind::Codex));
        h.reconcile();
        h.reconcile();
        let rows = pane_approval_rows(&h, slug);
        assert_eq!(rows.len(), 2);
        assert_eq!(
            rows[1].approval.as_ref().expect("approval").id,
            "pane-2",
            "a fresh pane-2 episode after recovery"
        );
    }

    // Mirrors TestPaneApprovalOverridesLastMessage.
    #[test]
    fn pane_approval_overrides_last_message() {
        let approval = pane_fixture("codex-ready-approval-exec");
        let resolved = pane_fixture("codex-ready-approval-resolved");
        let (h, _f, set_pane, slug) = pane_approval_hub(&approval);

        h.reconcile();
        h.reconcile();
        let msg = last_message_of(&h, slug);
        assert!(
            msg.contains("Yes, proceed"),
            "last_message = {msg:?}, want the approval's option-row summary"
        );
        let rows = pane_approval_rows(&h, slug);
        assert_eq!(rows.len(), 1);
        assert_eq!(
            rows[0].text, msg,
            "last_message must be the pending row's text"
        );

        set_pane(&resolved);
        h.reconcile();
        h.reconcile();
        assert!(
            !last_message_of(&h, slug).contains("Yes, proceed"),
            "the normal merge must be restored after the clear"
        );
    }

    // The firstAnchorLine slice of TestCodexApprovalAnchorFixtures (the
    // anchor match table itself is mirrored in rc_agents at H3): the row's
    // text is the single option row, not the whole match span.
    #[test]
    fn first_anchor_line_is_the_option_row() {
        let anchor = approval_anchor_for(&RcKind::Codex).expect("codex anchor");
        for fx in ["codex-ready-approval-exec", "codex-ready-approval-network"] {
            let pane = pane_fixture(fx);
            let line = first_anchor_line(anchor, &pane);
            assert!(
                !line.contains("Press enter") && line.contains(". "),
                "{fx}: firstAnchorLine = {line:?}, want the single option row"
            );
        }
        assert_eq!(first_anchor_line(anchor, "no match here"), "");
    }

    // Mirrors TestCursorPaneApprovalEpisode (cursor_approval_test.go:121) —
    // the cursor kind rides the same pane-anchor machinery.
    #[test]
    fn cursor_pane_approval_episode() {
        let (h, f, _clk) = rig();
        f.set(
            "rc-cap001",
            &pane_fixture("cursor-ready-approval-shell"),
            &managed_env("id-cap", &RcKind::Cursor),
        );

        h.reconcile(); // tick 1: matched once — not yet debounced
        assert_ne!(activity_of(&h, "cap001"), Some(RcActivity::NeedsApproval));
        h.reconcile(); // tick 2: debounced detection
        assert_eq!(activity_of(&h, "cap001"), Some(RcActivity::NeedsApproval));
        let pend = pending_approvals_of(&h, "cap001");
        assert_eq!(pend.len(), 1);
        assert_eq!(
            (pend[0].id.as_str(), pend[0].status.as_str()),
            ("pane-1", "pending")
        );
        assert!(
            pend[0].decisions.is_empty(),
            "a pane-derived approval must advertise no decisions"
        );

        f.set_pane("rc-cap001", &pane_fixture("cursor-ready-approval-resolved"));
        h.reconcile();
        h.reconcile(); // debounced clear
        assert_ne!(activity_of(&h, "cap001"), Some(RcActivity::NeedsApproval));
        assert!(pending_approvals_of(&h, "cap001").is_empty());
    }
}
