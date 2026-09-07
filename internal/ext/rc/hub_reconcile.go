package rc

import (
	"time"
)

// The reconcile loop is the hub's heartbeat: on each tick it enumerates the shed's
// rc-* tmux sessions (the same List machinery the one-shot subcommands use), refreshes
// each session's structured-signal watcher (opencode's SSE lane — the only one left),
// and emits SSE events on transitions:
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
// ticked from the single reconcile goroutine.
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
	kind Kind

	// activity is the DISPLAYED activity (DisplayActivity already applied): "" means
	// the activity dimension is suppressed (blocking lifecycle state). Used for both
	// change detection and the /v1/sessions overlay.
	activity    Activity
	activityAt  string // RFC3339 time the displayed activity last changed
	lastMessage string // sanitized preview from the watcher (opencode SSE); "" when it has none
	lastState   State

	// watcher is the session's structured-signal watcher: an opencode SSE client, lazily
	// created against its recorded port and correlating asynchronously in its own
	// goroutine (see ensureWatcher). nil for kinds with no structured signal (every kind
	// but opencode since A6, charliek/shed#322), or before correlation succeeds. When
	// present and FRESH, its verdict IS the session's activity (see reconcile's merge);
	// absent, closed or stale means the session simply has no activity dimension —
	// S2 (charliek/shed#324) deleted the pane-stability fallback that used to fill it
	// in. Closed when the session disappears/recreates.
	watcher sessionWatcher
	// pendingAgentID is an AMBIGUOUS correlation's agent session id, held back until
	// the watcher's first confirming event — only then is it back-written to
	// SHED_RC_AGENT_SESSION. Back-writing an unconfirmed ambiguous pick would make a
	// WRONG pin permanent across hub restarts (the exact-id path would trust it
	// forever). "" once written or when the match was unambiguous.
	pendingAgentID string

	// ring is the session's message feed (populated by the opencode watcher; every
	// tracked session has one so /messages returns 200-empty for a
	// known slug and 404 only for an unknown one). Its own mutex guards concurrent access.
	ring *messageRing
	// pendingApprovals is the session's currently-open approval requests — the
	// hub-layer source for Session.PendingApprovals, overlaid onto the /v1/sessions
	// rows (see handleSessions). Reconcile republishes it every tick from the
	// session's watcher when that watcher knows its lane's approvals
	// (approvalPublisher — opencode today); kinds whose approvals are pane-derived
	// leave it empty. PENDING ONLY by wire contract: it answers "what is still open",
	// and resolution state stays in the watcher (approvalState) where the approvals
	// verb reads it. Rebuilding it from the native protocol after a hub restart is the
	// whole point of the snapshot: the feed ring's approval rows can be evicted or
	// lost, this cannot.
	pendingApprovals []FeedApproval
}

// approvalSnapshot is the session's pending_approvals overlay: the entries its lane
// watcher published (opencode's — the only lane with approvals since A6). S2
// (charliek/shed#324) removed the pane-anchor episodes that used to be unioned in
// here, so this is now just the published slice. Called under trackMu; the result is
// deep-copied by the caller before it reaches a response.
func (tr *trackedSession) approvalSnapshot() []FeedApproval {
	return tr.pendingApprovals
}

// newTrackedSession builds tracker state for a freshly seen session. EVERY enumerated
// session gets an entry, watcher or not — that is what makes /messages answer 200 with
// an empty page for a tracked feedless kind and 404 only for an unknown slug.
func (h *Hub) newTrackedSession(s Session) *trackedSession {
	return &trackedSession{
		id:        s.ID,
		createdAt: s.CreatedAt,
		kind:      s.Kind,
		lastState: s.State,
		ring:      newMessageRing(),
	}
}

// sameIdentity reports whether s is still the session this tracker state was built
// for (see the id/createdAt doc on trackedSession). Kind participates because the
// tracked kind is what the verb handlers authorize against (verbTarget): a legacy
// session with empty id/created_at recreated at the same slug as a DIFFERENT kind
// would otherwise keep the old incarnation's kind — outcome-neutral while every verb
// rejects, an authorization bug the day one kind advertises a verb.
func (tr *trackedSession) sameIdentity(s Session) bool {
	return tr.id == s.ID && tr.createdAt == s.CreatedAt && tr.kind == s.Kind
}

// reconcile runs one enumeration+tick pass and broadcasts the resulting events. It
// holds trackMu only while READING/MUTATING handler-visible tracked fields; it RELEASES
// the lock around each session's tmux/network work (ensureWatcher's show/set-environment,
// the opencode watcher's HTTP+SSE calls) so a slow tmux call can never block the HTTP
// handlers that read tracked state. This is sound because reconcile is the SOLE writer
// of tracked state (no other goroutine mutates it, so tr and the map entry stay valid
// across the unlock) and the sub-objects touched unlocked are either self-synchronized
// (the watcher, messageRing) or reconcile-only (pendingAgentID). Events are collected
// into a slice and broadcast after the final unlock — so a broadcast can never block
// reconcile against an SSE handler.
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

		// --- Heavy tmux/network work runs WITHOUT trackMu held (see reconcile's doc). ---
		// tr and the map entry stay valid across the unlock (reconcile is the sole
		// writer); the sub-objects touched here are self-synchronized (the watcher, the
		// ring) or reconcile-only (pendingAgentID). The one handler-visible field
		// produced here — the watcher pointer — is returned and committed under the
		// lock below, never published unlocked.
		h.trackMu.Unlock()

		// Lazily correlate the session to its structured signal — an opencode session's
		// SSE stream (async, its own goroutine). Once correlated, the watcher
		// subscribes and — when FRESH — supplies the session's whole activity dimension
		// below. newW is any watcher freshly created this pass.
		newW := h.ensureWatcher(tr, s)
		watcher := tr.watcher // existing committed watcher (read: sole writer, safe unlocked)
		if newW != nil {
			watcher = newW
		}

		// Derive activity. Since S2 (charliek/shed#324) there is exactly ONE source: a
		// fresh, correlated watcher. No watcher, a closed/unhealthy transport, or a
		// stale verdict all mean the same thing — this session has no activity
		// dimension (mergedActivity returns "", which the DTO omits).
		var watcherActivity Activity
		var watcherMessage string
		watcherFresh := false
		var msgEvents []hubEvent
		var pendingApprovals []FeedApproval
		publishesApprovals := false
		if watcher != nil {
			watcher.refresh(now)
			// A deferred (ambiguous-correlation) back-write happens only once the first
			// confirming event settles the pick — see trackedSession.pendingAgentID.
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
			// Republish the lane's OPEN approvals every tick (approvalPublisher — the same
			// narrow type-assert precedent as confirmedAgentIDDrainer above). The snapshot is
			// pending-only by wire contract: resolution state lives in the watcher's fold,
			// where the approvals verb reads it. Read unlocked (the watcher self-synchronizes)
			// and committed under trackMu below with the rest of the handler-visible fields.
			if ap, ok := watcher.(approvalPublisher); ok {
				pendingApprovals = ap.pendingApprovals()
				publishesApprovals = true
			}
			watcherActivity, watcherMessage, watcherFresh, _ = watcher.snapshot(now)
		}

		// --- Re-acquire trackMu to commit handler-visible fields. ---
		h.trackMu.Lock()
		if newW != nil {
			tr.watcher = newW
		}
		// Only a publishing watcher owns this field: a kind whose approvals are not lane-
		// derived must keep whatever it holds (nothing — the pane-anchor kinds live in the
		// separate field below) rather than be blanked by an unrelated tick.
		if publishesApprovals {
			tr.pendingApprovals = pendingApprovals
		}
		events = append(events, msgEvents...)

		mergedRaw, mergedMsg := mergedActivity(watcherActivity, watcherMessage, watcherFresh)
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
	// opencode SSE connection is now pointed at a dead session).
	for slug, tr := range h.tracked {
		if !present[slug] {
			if tr.watcher != nil {
				tr.watcher.close()
			}
			delete(h.tracked, slug)
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

	// Publish conversation ownership, after the trackMu release.
	//
	// Age alone cannot settle who owns a conversation in a shared opencode store: a
	// session that started FIRST and then sat idle will happily adopt the
	// conversation a later session is actively using, because that conversation is
	// newer than the adopter. Only the hub sees every session, so the hub is what
	// tells each watcher which ids are already spoken for — every tick, because a
	// neighbour's pin usually does not exist yet when the watcher is built.
	h.publishClaims()

	for _, e := range events {
		h.broadcast(e)
	}
}

// publishClaims tells every claim-holding watcher which agent-session ids belong to
// the OTHER tracked sessions (see claimHolder).
//
// The (slug, watcher) pairs are snapshotted under trackMu and the pushing happens
// with it released: setClaimed takes the watcher's own mutex, and holding two locks
// in one order here and the other order anywhere else is how a deadlock is written.
func (h *Hub) publishClaims() {
	type holder struct {
		slug string
		c    claimHolder
	}
	var holders []holder
	h.trackMu.Lock()
	for slug, tr := range h.tracked {
		if c, ok := tr.watcher.(claimHolder); ok && tr.watcher != nil {
			holders = append(holders, holder{slug: slug, c: c})
		}
	}
	h.trackMu.Unlock()
	if len(holders) < 2 {
		return // nothing can be contested
	}
	type pin struct{ owner, id string }
	var pins []pin
	for _, hd := range holders {
		if id := hd.c.pinnedAgentID(); id != "" {
			pins = append(pins, pin{owner: hd.slug, id: id})
		}
	}
	for _, hd := range holders {
		others := make([]string, 0, len(pins))
		for _, p := range pins {
			if p.owner != hd.slug {
				others = append(others, p.id)
			}
		}
		hd.c.setClaimed(others)
	}
}

// ensureWatcher lazily builds a watchable session's structured-signal watcher, RETURNING
// it (nil when none was created this call: a watcher already exists, the kind is
// unwatchable, the session is in a blocking lifecycle state, or no valid opencode port
// was recorded). It runs UNLOCKED from reconcile (tmux show-environment), so it must NOT
// publish tr.watcher — handlers read that field under trackMu; the caller commits the
// returned watcher under the lock.
//
// opencode is the only watchable kind since A6 (charliek/shed#322) removed the codex
// JSONL tail and the cursor hook-ingest lane. Its watcher owns its OWN async correlation
// over SSE/REST (constructed NON-BLOCKING; it pins the session id from its own /event
// stream and surfaces it via drainConfirmedAgentID — see watch_opencode_transport.go), so
// none of the file-correlation machinery the other two needed survives here.
func (h *Hub) ensureWatcher(tr *trackedSession, s Session) sessionWatcher {
	if tr.watcher != nil || !watchableKind(s.Kind) {
		return nil
	}
	switch s.State {
	case StateNeedsTrust, StateNeedsAuth, StateDead:
		return nil // no live activity to watch; retry once the session becomes usable
	}

	// A session with no valid recorded port — a pre-upgrade session created before the
	// port plumbing shipped, or an out-of-range value — is unwatchable over this
	// transport, so it falls back to pane stability (see opencodePortEnv).
	port, ok := opencodePortEnv(h.cfg.runner, s.TmuxSession)
	if !ok {
		return nil
	}
	// A prior back-written SHED_RC_AGENT_SESSION (from an earlier hub lifetime) is the
	// trusted pin; "" means the watcher searches its SSE stream for the session id.
	agentID := agentSessionEnv(h.cfg.runner, s.TmuxSession)
	// When this RC session was created. opencode's store is shared per PROJECT, so
	// /session lists a neighbouring RC session's conversations too and the directory
	// alone cannot tell them apart — the watcher refuses to adopt one older than the
	// session itself. Unparseable/absent → zero, which disables the check.
	notBefore, _ := time.Parse(time.RFC3339, s.CreatedAt)
	return newOpencodeWatcher(port, s.Workdir, agentID, notBefore, h.cfg.now, h.cfg.logf)
}
