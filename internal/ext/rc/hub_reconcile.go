package rc

import (
	"fmt"
	"time"
)

// The reconcile loop is the hub's heartbeat: on each tick it enumerates the shed's
// rc-* tmux sessions (the same List machinery the one-shot subcommands use), runs a
// StabilityTracker per session to derive live activity, and emits SSE events on
// transitions:
//
//   - session.updated on appear (new slug or a recreated slug — different SHED_RC_ID
//     or created_at), on a lifecycle-state change, and on disappear (killed →
//     session:null);
//   - activity.changed when a session's DISPLAYED activity (lifecycle-trumps-activity
//     precedence applied) changes to a VALID, NON-EMPTY value. The wire contract
//     only advertises working|needs_input|needs_approval|idle|unknown — the
//     suppressed value ("") is never emitted as an activity.changed (a strict
//     decoder would reject it);
//     a transition INTO suppression always coincides with a blocking lifecycle
//     state change, which already emits session.updated (carrying the new state).
//
// Everything is driven off the injected Runner + clock, so tests script tmux output
// and time and assert the emitted events with no real tmux or wall-clock.

// trackedSession is the hub's per-session reconcile state. One per live rc session,
// ticked from the single reconcile goroutine (StabilityTracker is not concurrent).
type trackedSession struct {
	// id + createdAt are the session's identity pin: a change in EITHER means the
	// slug was killed and recreated, so the tracker state must reset. Both are
	// checked because legacy/partial sessions can lack SHED_RC_ID — created_at is
	// the fallback signal (two legacy sessions with neither are indistinguishable,
	// which is acceptable: there is no identity to pin).
	id        string // SHED_RC_ID
	createdAt string // SHED_RC_CREATED_AT (normalized RFC3339, "" when absent)
	// kind is the session's agent kind, captured when the entry was built. It is the
	// hub's capability key: the contract-v2 verb handlers (hub_verbs.go) look the
	// kind's kind_features row up from it rather than re-deriving the session from a
	// fresh pane capture. A kind is stamped at create time and never changes for a
	// given incarnation — a recreate replaces this whole entry.
	kind    Kind
	tracker *StabilityTracker

	// activity is the DISPLAYED activity (DisplayActivity already applied): "" means
	// the activity dimension is suppressed (blocking lifecycle state). Used for both
	// change detection and the /v1/sessions overlay.
	activity    Activity
	activityAt  string // RFC3339 time the displayed activity last changed
	lastMessage string // sanitized preview from the watcher (JSONL tail or opencode SSE; "" from stability)
	lastState   State

	// watcher is the session's structured-signal watcher: a JSONL tail (codex rollout /
	// claude transcript), lazily created once the session is pinned to a file, OR an
	// opencode SSE client, lazily created against its recorded port and correlating
	// asynchronously in its own goroutine (see ensureWatcher). nil for kinds with no
	// structured signal, or before correlation succeeds. When present and FRESH, its
	// activity overrides the pane-stability tracker (see reconcile's merge); when
	// absent/stale, stability drives. Closed when the session disappears/recreates.
	watcher sessionWatcher
	// pendingAgentID is an AMBIGUOUS correlation's agent session id, held back until
	// the watcher's first in-file event confirms the pick — only then is it back-
	// written to SHED_RC_AGENT_SESSION. Back-writing an unconfirmed ambiguous pick
	// would make a WRONG pin permanent across hub restarts (the exact-id path would
	// trust it forever). "" once written or when the match was unambiguous.
	pendingAgentID string
	// correlateTried counts correlation attempts so a session whose file never appears
	// does not re-scan the filesystem on every single tick forever.
	correlateTried int

	// ring is the session's message feed (populated by the codex and opencode watchers
	// in this phase; every tracked session has one so /messages returns 200-empty for a
	// known slug and 404 only for an unknown one). Its own mutex guards concurrent access.
	ring *messageRing
	// lastStability is the raw pane-stability verdict from the most recent successful
	// tracker Tick (before the watcher merge / DisplayActivity). The input handler
	// reads it so its acceptance re-check runs the SAME mergedActivity the reconcile
	// uses — without it, a long-quiet working session (>120s tool call) would fall
	// to the anchor path and deliver mid-turn.
	lastStability Activity

	// pendingApprovals is the session's currently-open approval requests — the
	// hub-layer source for Session.PendingApprovals, overlaid onto the /v1/sessions
	// rows (see handleSessions). NOTHING writes it in this phase: no lane produces
	// approvals yet, so reconcile never populates it and the overlay is always a
	// no-op. It exists so the field is plumbed end-to-end now (contract fixed, value
	// always absent on the wire) and a lane adapter has exactly one place to maintain
	// the per-session pending map — including rebuilding it from its native protocol
	// after a hub restart, which is the whole point of the snapshot: the feed ring's
	// approval rows can be evicted or lost, this cannot.
	pendingApprovals []FeedApproval
}

// newTrackedSession builds tracker state for a freshly seen session. The tracker
// captures the session's pane on demand via the hub's runner (independent of the
// pane List already captured for classification — a modest extra capture-pane per
// tick, acceptable at the 2s/10s cadence).
func (h *Hub) newTrackedSession(s Session) *trackedSession {
	name := s.TmuxSession
	capture := func() (string, error) {
		res := capturePane(h.cfg.runner, name)
		if res.Code != 0 {
			return "", fmt.Errorf("capture-pane %s failed: %s", name, res.Stderr)
		}
		return res.Stdout, nil
	}
	return &trackedSession{
		id:        s.ID,
		createdAt: s.CreatedAt,
		kind:      s.Kind,
		tracker:   NewStabilityTracker(s.Kind, capture, h.cfg.now, h.cfg.quiet),
		lastState: s.State,
		ring:      newMessageRing(),
	}
}

// sameIdentity reports whether s is still the session this tracker state was built
// for (see the id/createdAt doc on trackedSession). Kind participates because the
// tracked kind is what the verb handlers authorize against (verbFeatures): a legacy
// session with empty id/created_at recreated at the same slug as a DIFFERENT kind
// would otherwise keep the old incarnation's kind — outcome-neutral while every verb
// rejects, an authorization bug the day one kind advertises a verb.
func (tr *trackedSession) sameIdentity(s Session) bool {
	return tr.id == s.ID && tr.createdAt == s.CreatedAt && tr.kind == s.Kind
}

// reconcile runs one enumeration+tick pass and broadcasts the resulting events. It
// holds trackMu only while READING/MUTATING handler-visible tracked fields; it RELEASES
// the lock around each session's tmux/disk/network work (ensureWatcher's show/set-
// environment, tracker.Tick's capture-pane, the watcher's I/O — a JSONL tail's file
// reads or an opencode watcher's HTTP+SSE calls) so a slow tmux call can
// never block the HTTP handlers that read tracked state. This is sound because reconcile
// is the SOLE writer of tracked state (no other goroutine mutates it, so tr and the map
// entry stay valid across the unlock) and the sub-objects touched unlocked are either
// self-synchronized (fileWatcher, messageRing) or reconcile-only (tracker,
// correlateTried, pendingAgentID). Events are collected into a slice and broadcast after
// the final unlock — so a broadcast can never block reconcile against an SSE handler.
func (h *Hub) reconcile() {
	// A transient tmux listing failure must NOT read as "every session is gone" —
	// that would wipe the message rings, close the watchers, and broadcast a storm of
	// session-gone events over one hiccup. Skip the whole pass and keep state; the
	// next tick retries. (A genuine "no sessions/no server" answer returns an empty
	// list with no error and proceeds — that IS everything-gone.)
	names, err := listSessionNamesChecked(h.cfg.runner)
	if err != nil {
		h.cfg.logf("rc hub: session listing failed (%v); keeping state this tick", err)
		return
	}
	sessions := sessionsForNames(h.cfg.runner, names, nil)
	now := h.cfg.now()

	var events []hubEvent
	h.trackMu.Lock()

	present := make(map[string]bool, len(sessions))
	for i := range sessions {
		s := sessions[i]
		present[s.Slug] = true

		tr, ok := h.tracked[s.Slug]
		if !ok || !tr.sameIdentity(s) {
			// New session, or the slug was recreated (id OR created_at changed — the
			// latter catches legacy sessions with no SHED_RC_ID) → start over. A
			// recreate must drop the previous session's watcher (a new session gets a
			// new JSONL file or opencode port; keeping the old tail/SSE connection would
			// report the dead session).
			if ok && tr.watcher != nil {
				tr.watcher.close()
			}
			tr = h.newTrackedSession(s)
			h.tracked[s.Slug] = tr
			events = append(events, sessionUpdatedEvent(s))
		} else if tr.lastState != s.State {
			events = append(events, sessionUpdatedEvent(s))
		}
		tr.lastState = s.State

		// --- Heavy tmux/disk work runs WITHOUT trackMu held (see reconcile's doc). ---
		// tr and the map entry stay valid across the unlock (reconcile is the sole
		// writer); the sub-objects touched here are self-synchronized (fileWatcher,
		// ring) or reconcile-only (tracker, correlateTried, pendingAgentID). The one
		// handler-visible field produced here — the watcher pointer — is returned and
		// committed under the lock below, never published unlocked.
		h.trackMu.Unlock()

		// Lazily correlate the session to its structured signal — codex rollout / claude
		// transcript JSONL for those kinds, or an opencode session's SSE stream (async,
		// its own goroutine). Once correlated, the watcher tails/subscribes and — when
		// FRESH — overrides the pane-stability tracker below. newW is any watcher freshly
		// created this pass.
		newW := h.ensureWatcher(tr, s)
		watcher := tr.watcher // existing committed watcher (read: sole writer, safe unlocked)
		if newW != nil {
			watcher = newW
		}

		// Derive activity. The pane-stability tracker is the universal fallback; a fresh,
		// correlated watcher (JSONL- or SSE-backed) overrides it (mergedActivity). A stability capture
		// error (transient tmux hiccup) with no fresh watcher leaves the prior activity
		// untouched rather than flapping the DTO to unknown.
		raw, capErr := tr.tracker.Tick()

		var watcherActivity Activity
		var watcherMessage string
		watcherFresh, watcherExpiredWorking := false, false
		var msgEvents []hubEvent
		if watcher != nil {
			watcher.refresh(now)
			// A deferred (ambiguous-correlation) back-write happens only once the first
			// in-file event confirms the pick — see trackedSession.pendingAgentID.
			if tr.pendingAgentID != "" && watcher.hadEvent() {
				backWriteAgentSession(h.cfg.runner, s.TmuxSession, tr.pendingAgentID)
				tr.pendingAgentID = ""
			}
			// The opencode watcher correlates ASYNC in its own transport goroutine (unlike
			// the file watchers, which correlate off-line in ensureWatcher): once it pins the
			// session id from a port-local SSE event it surfaces it here for back-write into
			// SHED_RC_AGENT_SESSION, so a hub restart re-correlates exactly. drainConfirmedAgentID
			// returns "" once drained (and "" for a prior-back-write pin), so a non-empty id is
			// always a fresh one to stamp. Runs UNLOCKED like the rest of the heavy per-session
			// work — backWriteAgentSession is a tmux set-environment, kept off trackMu.
			if d, ok := watcher.(confirmedAgentIDDrainer); ok {
				if id := d.drainConfirmedAgentID(); id != "" {
					backWriteAgentSession(h.cfg.runner, s.TmuxSession, id)
				}
			}
			// Drain any normalized feed messages the watcher produced this poll into the
			// session ring (codex and opencode; other kinds' watchers produce none). A per-message
			// message.appended notification lets subscribers know to fetch /messages — the
			// body is deliberately not on the SSE frame (keeps fan-out tiny + drop-safe).
			// The ring is self-synchronized and events is reconcile-local, so this is
			// safe to do while unlocked.
			for _, m := range watcher.drainPending() {
				seq := tr.ring.append(m, now)
				msgEvents = append(msgEvents, messageAppendedEvent(s.Slug, seq))
			}
			watcherActivity, watcherMessage, watcherFresh, watcherExpiredWorking = watcher.snapshot(now)
		}

		// --- Re-acquire trackMu to commit handler-visible fields. ---
		h.trackMu.Lock()
		if newW != nil {
			tr.watcher = newW
		}
		if capErr == nil {
			// Remember the raw stability verdict for the input handler's acceptance
			// re-check (it re-runs the same watcher+stability merge as below).
			tr.lastStability = raw
		}
		events = append(events, msgEvents...)

		if capErr != nil && !watcherFresh {
			continue // no signal this tick; retain the prior verdict (lock stays held)
		}

		mergedRaw, mergedMsg := mergedActivity(watcherActivity, watcherMessage, watcherFresh, watcherExpiredWorking, raw)
		eff := DisplayActivity(s.State, mergedRaw)
		// last_message rides with the activity dimension: a suppressed (blocking
		// lifecycle) activity drops the message too, per DisplayActivity's contract.
		effMsg := mergedMsg
		if eff == "" {
			effMsg = ""
		}

		if eff != tr.activity {
			tr.activity = eff
			tr.activityAt = now.UTC().Format(time.RFC3339)
			// Contract: activity.changed carries only valid non-empty activity values.
			// A transition INTO suppression (eff == "", i.e. the state just became
			// needs-trust/needs-auth/dead) is announced by the session.updated the
			// state change emitted above — clients drop the activity dimension from
			// the state, not from a hollow activity event.
			if eff != "" {
				events = append(events, activityChangedEvent(s.Slug, eff, tr.activityAt, s.State, effMsg))
			}
		}
		// Keep the message preview current every tick (the /v1/sessions overlay reads
		// it) even when the activity value itself did not change.
		tr.lastMessage = effMsg
	}

	// Sessions that vanished since the last pass (killed). Release the watcher (its
	// JSONL tail or opencode SSE connection is now pointed at a dead session) and prune the slug's input
	// lock (a recreate at the same slug later gets a fresh one; an input request
	// already holding the old lock finishes against a gone pane harmlessly).
	for slug, tr := range h.tracked {
		if !present[slug] {
			if tr.watcher != nil {
				tr.watcher.close()
			}
			delete(h.tracked, slug)
			h.pruneInputLock(slug)
			events = append(events, sessionGoneEvent(slug))
		}
	}

	// Idle-exit bookkeeping: start the clock when the session count first hits zero,
	// clear it the moment any session exists. shouldIdleExit reads idleSince.
	if len(sessions) == 0 {
		if h.idleSince.IsZero() {
			h.idleSince = now
		}
	} else {
		h.idleSince = time.Time{}
	}

	h.trackMu.Unlock()

	for _, e := range events {
		h.broadcast(e)
	}
}

// maxCorrelateTries bounds how many reconcile ticks a session's correlation is
// re-attempted before giving up (the JSONL file never appeared — an old agent binary,
// a kind that does not log, or a create that failed to launch). ~40 ticks at the
// active cadence (2s) is >1min of retry, comfortably longer than the file-appears
// latency, while a never-appearing file stops re-scanning the filesystem forever.
const maxCorrelateTries = 40

// ensureWatcher lazily builds a watchable session's structured-signal watcher, RETURNING
// it (nil when none was created this call: a watcher already exists, the kind is
// unwatchable, the session is in a blocking lifecycle state, the retry budget is
// exhausted, correlation failed, or — opencode — no valid port was recorded). It runs
// UNLOCKED from reconcile (tmux show-environment / set-environment), so it must NOT
// publish tr.watcher — handlers read that field under trackMu; the caller commits the
// returned watcher under the lock. It DOES mutate tr.correlateTried / tr.pendingAgentID,
// which are reconcile-only (never read by handlers) and thus safe to touch unlocked.
//
// codex/claude correlate a JSONL file (rollout / transcript) and build a tailing
// fileWatcher here; on an unambiguous match it back-writes the discovered agent session
// id into the tmux env so a hub restart re-correlates exactly, while an ambiguous window
// match follows only new appends (activity stays unknown until an in-file event confirms).
// opencode instead returns a NON-BLOCKING SSE/REST watcher that correlates itself on its
// own goroutine (see the opencode arm below and drainConfirmedAgentID in reconcile).
func (h *Hub) ensureWatcher(tr *trackedSession, s Session) sessionWatcher {
	if tr.watcher != nil || !watchableKind(s.Kind) {
		return nil
	}
	switch s.State {
	case StateNeedsTrust, StateNeedsAuth, StateDead:
		return nil // no live activity to tail; retry once the session becomes usable
	}

	// opencode diverges from the codex/claude file-correlation path below: its watcher
	// owns its OWN async correlation over SSE/REST (constructed NON-BLOCKING; it pins the
	// session id from its own /event stream and surfaces it via drainConfirmedAgentID —
	// see watch_opencode_transport.go). So it needs none of the file-correlation +
	// maxCorrelateTries + pendingAgentID/back-write machinery here — it returns early with
	// its own watcher, built on the FIRST eligible tick (ensureWatcher no-ops thereafter,
	// tr.watcher being set), never spuriously blocked by the correlate-retry budget. A
	// session with no valid recorded port — a pre-upgrade session created before the port
	// plumbing shipped, or an out-of-range value — is unwatchable over this transport, so
	// it returns nil and falls back to pane stability (see opencodePortEnv).
	if s.Kind == KindOpencode {
		port, ok := opencodePortEnv(h.cfg.runner, s.TmuxSession)
		if !ok {
			return nil
		}
		// A prior back-written SHED_RC_AGENT_SESSION (from an earlier hub lifetime) is the
		// trusted pin; "" means the watcher searches its SSE stream for the session id.
		agentID := agentSessionEnv(h.cfg.runner, s.TmuxSession)
		return newOpencodeWatcher(port, s.Workdir, agentID, h.cfg.now, h.cfg.logf)
	}

	if tr.correlateTried >= maxCorrelateTries {
		return nil
	}
	tr.correlateTried++

	createdAt, hasCreatedAt := parseJSONLTime(s.CreatedAt)
	agentID := agentSessionEnv(h.cfg.runner, s.TmuxSession)

	var corr correlation
	var ok bool
	var fold activityFold
	switch {
	case s.Kind == KindCodex:
		corr, ok = correlateCodex(h.cfg.getenv, s.Workdir, agentID, createdAt, hasCreatedAt)
		fold = newCodexFold()
	case IsClaudeKind(s.Kind):
		corr, ok = correlateClaude(h.cfg.getenv, s.Workdir, agentID, createdAt, hasCreatedAt)
		fold = newClaudeFold()
	default:
		return nil
	}
	if !ok {
		return nil
	}

	// Unambiguous match → do a bounded catch-up read so the current activity is known
	// immediately. Ambiguous → follow only new appends (unknown until an event confirms
	// which file is really this session's).
	w := newFileWatcher(corr.path, !corr.ambiguous, fold)
	if agentID != "" || corr.sessionID == "" {
		return w
	}
	if corr.ambiguous {
		// NEVER back-write an ambiguous pick immediately: a wrong id stamped into the
		// tmux env would be trusted by the exact-id path on every future hub restart,
		// making the mistake permanent. Hold it until the watcher's first in-file event
		// confirms the pick (see reconcile).
		tr.pendingAgentID = corr.sessionID
		return w
	}
	backWriteAgentSession(h.cfg.runner, s.TmuxSession, corr.sessionID)
	return w
}
