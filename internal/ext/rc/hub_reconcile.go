package rc

import (
	"fmt"
	"regexp"
	"strings"
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

	// ring is the session's message feed (populated by the codex, opencode and cursor
	// watchers; every tracked session has one so /messages returns 200-empty for a
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
	// rows (see handleSessions). Reconcile republishes it every tick from the
	// session's watcher when that watcher knows its lane's approvals
	// (approvalPublisher — opencode today); kinds whose approvals are pane-derived
	// leave it empty. PENDING ONLY by wire contract: it answers "what is still open",
	// and resolution state stays in the watcher (approvalState) where the approvals
	// verb reads it. Rebuilding it from the native protocol after a hub restart is the
	// whole point of the snapshot: the feed ring's approval rows can be evicted or
	// lost, this cannot.
	pendingApprovals []FeedApproval

	// paneApproval is the PANE-derived approval state for kinds whose approvals never
	// reach a protocol (AgentSpec.ApprovalAnchor kinds — codex and cursor).
	// Deliberately a SEPARATE field from pendingApprovals, not a merge into that slice:
	// pendingApprovals is wholly owned by the publishing watcher (reconcile and the
	// approvals verb both REPLACE it from approvalPublisher), so a pane entry living
	// there would be blanked by the next republish. Reconcile is its sole writer; the
	// /v1/sessions overlay unions the two (see approvalSnapshot).
	paneApproval paneApprovalState
}

// paneApprovalDebounceTicks is how many CONSECUTIVE ticks the anchor must agree before
// the hub changes its mind — in BOTH directions. Two ticks (4s at the active cadence)
// is the smallest value that costs a single-tick blip nothing: a capture that catches
// the overlay mid-draw, or one that momentarily misses it because a redraw was in
// flight, is contradicted by the very next tick and never reaches the wire. The cost is
// one tick of latency on a genuine transition, which is far below human reaction time
// on the phone this signal exists for.
const paneApprovalDebounceTicks = 2

// paneApprovalState is one session's debounced pane-anchor approval episode. An EPISODE
// runs from a debounced detection to a debounced clear and owns exactly one id and
// exactly one pending feed row; ids are "pane-<n>" with n monotonic per session
// (per hub lifetime — the ring's seq has the same restart semantics).
//
// Reconcile-only: the streak counters are never read outside the reconcile goroutine,
// and the handler-visible part (pending/id) is committed under trackMu with the rest of
// the tick's output.
type paneApprovalState struct {
	matchTicks int  // consecutive ticks the anchor matched (reset by a non-match)
	clearTicks int  // consecutive ticks the anchor did not match (reset by a match)
	pending    bool // debounced verdict: an approval episode is open
	id         string
	// text is the open episode's one-line summary (the matched option row). It stands in
	// for the session's last_message while the episode runs: the watcher's own preview
	// describes the tool call the dialog SUSPENDED, which on a phone reads as "the agent
	// is busy doing this" at the exact moment the truth is "the agent is waiting on you".
	text     string
	episodes int
}

// abandon drops an open episode WITHOUT announcing a resolution — the exit for a session
// that stopped being observable (a blocking lifecycle state; most sharply, codex dying
// with its dialog on screen) rather than one whose dialog was answered. episodes is
// deliberately NOT reset: ids stay monotonic for the session's whole life, so a client
// folding by id can never see pane-1 mean two different asks.
func (p *paneApprovalState) abandon() {
	p.matchTicks, p.clearTicks = 0, 0
	p.pending = false
	p.id = ""
	p.text = ""
}

// observe folds one tick's anchor verdict into the debounce and reports a debounced
// TRANSITION as the feed status it should announce: approvalStatusPending when an
// episode just opened, approvalStatusResolved when the dialog is provably gone, and ""
// (the common case) when nothing changed. id is the transitioning episode's id,
// non-empty exactly when status is.
func (p *paneApprovalState) observe(matched bool) (status, id string) {
	if matched {
		p.matchTicks++
		p.clearTicks = 0
	} else {
		p.clearTicks++
		p.matchTicks = 0
	}
	switch {
	case !p.pending && matched && p.matchTicks >= paneApprovalDebounceTicks:
		p.pending = true
		p.episodes++
		p.id = fmt.Sprintf("pane-%d", p.episodes)
		return approvalStatusPending, p.id
	case p.pending && !matched && p.clearTicks >= paneApprovalDebounceTicks:
		id = p.id
		p.pending = false
		p.id = ""
		return approvalStatusResolved, id
	}
	return "", ""
}

// paneApprovalRow builds an INFORMATIONAL approval_request feed row for a pane-derived
// episode. `tool` is omitted (the hub never learns which call the dialog guards — the
// pane shows chrome, not a structured request) and `decisions` is omitted for the
// reason in approvalSnapshot. A resolved row additionally omits `decision`: the operator
// answered in the TUI and the hub cannot know which way they went — legal per the
// contract's loosening for out-of-hub resolutions.
func paneApprovalRow(id, status, text string) feedMessage {
	return feedMessage{
		Role:     feedRoleTool,
		Type:     feedTypeApprovalRequest,
		Text:     text,
		Approval: &FeedApproval{ID: id, Status: status},
	}
}

// firstAnchorLine returns the WHOLE pane line containing the anchor's first match, as a
// sanitized one-line preview — the row's human-readable text. Expanded to the enclosing
// line rather than reported as the match text so the row reads as what the operator
// sees on screen even for an anchor that matches a fragment (or, as codex's does, a
// multi-line span of chrome).
func firstAnchorLine(anchor *regexp.Regexp, pane string) string {
	loc := anchor.FindStringIndex(pane)
	if loc == nil {
		return ""
	}
	start := strings.LastIndexByte(pane[:loc[0]], '\n') + 1
	line, _, _ := strings.Cut(pane[start:], "\n")
	return SanitizeLastMessage(line)
}

// approvalSnapshot is the session's pending_approvals overlay: the lane-published
// entries (opencode) UNIONED with the open pane-derived episode (codex/cursor). The two
// sources are disjoint in practice — a kind's approvals are either lane-derived or
// pane-derived, never both — but they are unioned rather than switched so a kind that
// someday has both keeps every open ask visible. The pane entry carries no `decisions`:
// the kind's kind_features row says approvals:"tui", so there is NOTHING the hub can
// honor remotely and a capability-driven client must render zero decision buttons ("open
// the TUI"). Called under trackMu; the result is deep-copied by the caller before it
// reaches a response.
func (tr *trackedSession) approvalSnapshot() []FeedApproval {
	if !tr.paneApproval.pending {
		return tr.pendingApprovals
	}
	pane := FeedApproval{ID: tr.paneApproval.id, Status: approvalStatusPending}
	return append(append([]FeedApproval(nil), tr.pendingApprovals...), pane)
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
// tracked kind is what the verb handlers authorize against (verbTarget): a legacy
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
		var pendingApprovals []FeedApproval
		publishesApprovals := false
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
			// Republish the lane's OPEN approvals every tick (approvalPublisher — the same
			// narrow type-assert precedent as confirmedAgentIDDrainer above). The snapshot is
			// pending-only by wire contract: resolution state lives in the watcher's fold,
			// where the approvals verb reads it. Read unlocked (the watcher self-synchronizes)
			// and committed under trackMu below with the rest of the handler-visible fields.
			if ap, ok := watcher.(approvalPublisher); ok {
				pendingApprovals = ap.pendingApprovals()
				publishesApprovals = true
			}
			watcherActivity, watcherMessage, watcherFresh, watcherExpiredWorking = watcher.snapshot(now)
		}

		// Pane-anchor approvals (AgentSpec.ApprovalAnchor kinds: codex and cursor). For
		// both, the dialog reaches no protocol, so the pane is the only evidence that the
		// session is blocked on the operator.
		//
		// This takes its OWN capture, deliberately, instead of reusing the frame the
		// stability tracker just took: the tracker captures 200 lines of SCROLLBACK too
		// (the lifecycle classifiers need that history), and scrollback is exactly the
		// wrong thing to ask "is a modal on screen?" — an answered dialog, or one that was
		// up when the agent crashed out to a shell, stays in the history verbatim, so an
		// episode opened off scrollback could never clear. The cost is one extra tmux exec
		// per anchor-kind session per tick (codex and cursor), run unlocked like the rest
		// of the heavy per-session work. A capture failure means NO EVIDENCE, not
		// "cleared": the episode is held untouched and the next tick decides.
		paneApproval := tr.paneApproval
		if anchor := approvalAnchorFor(s.Kind); anchor != nil {
			if DisplayActivity(s.State, ActivityWorking) == "" {
				// Blocking lifecycle state (needs-trust / needs-auth / dead). The activity
				// dimension is suppressed anyway, and a session that DIED mid-dialog resolved
				// nothing — so an open episode is dropped SILENTLY, with no resolved row. The
				// state change is what tells the client (session.updated already carries it);
				// a resolved row would assert an answer that was never given.
				paneApproval.abandon()
			} else if vis := captureVisiblePane(h.cfg.runner, s.TmuxSession); vis.Code == 0 {
				// Exactly ONE pending row per episode (a debounced open transitions once) and
				// one resolved row on its debounced clear. The ring is self-synchronized, so
				// this appends while unlocked like the watcher's own drain above.
				if status, id := paneApproval.observe(anchor.MatchString(vis.Stdout)); status != "" {
					text := "" // a resolved row has nothing to preview: the dialog is gone
					if status == approvalStatusPending {
						text = firstAnchorLine(anchor, vis.Stdout)
					}
					// Retained for the duration of the episode: while it is open this REPLACES
					// the session's last_message (see the merge below), and on the resolved
					// transition the same assignment clears it.
					paneApproval.text = text
					seq := tr.ring.append(paneApprovalRow(id, status, text), now)
					msgEvents = append(msgEvents, messageAppendedEvent(s.Slug, seq))
				}
			}
		}

		// --- Re-acquire trackMu to commit handler-visible fields. ---
		h.trackMu.Lock()
		if newW != nil {
			tr.watcher = newW
			// SECOND drain of the cursor pre-watcher queue, and the load-bearing one.
			// ensureWatcher drained at CONSTRUCTION so this tick's refresh folds whatever was
			// already queued — but construction happens ~one full unlocked pass earlier (a
			// tracker capture, the watcher refresh, an anchor capture), and until the
			// assignment on the line above, the ingest handler still reads tr.watcher == nil
			// and keeps queueing. Those events would land in a FRESH queue that nothing ever
			// drains: ensureWatcher no-ops once tr.watcher is set, so the kickoff prompt would
			// sit there until the TTL dropped it. Draining HERE, immediately after the
			// publish, closes the window — the handler either sees the watcher and pushes, or
			// queued before this point and is drained now (folded next tick). Lock order is
			// trackMu → ingestMu → watcher.mu, and no path takes them the other way round (the
			// ingest handler releases trackMu before it pushes or queues).
			h.drainPreWatcher(s.Slug, newW)
		}
		// Only a publishing watcher owns this field: a kind whose approvals are not lane-
		// derived must keep whatever it holds (nothing — the pane-anchor kinds live in the
		// separate field below) rather than be blanked by an unrelated tick.
		if publishesApprovals {
			tr.pendingApprovals = pendingApprovals
		}
		tr.paneApproval = paneApproval
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
		// An open pane-anchor episode OVERRIDES the merge: the dialog owns the session,
		// and every other signal describes the suspended work underneath it. codex's
		// rollout in particular still reads `working` (the tool-call record is written
		// BEFORE the approval gate), and pane stability reads the frozen dialog as idle —
		// both would be wrong. Lifecycle still trumps this, via DisplayActivity below.
		//
		// last_message rides along: the merged preview describes the SUSPENDED tool call,
		// which pairs with needs_approval to read as "busy running this" precisely when the
		// session is waiting on the operator. The episode's own summary — the option row —
		// is what the client should show next to the badge, and it is restored to the
		// normal merge the moment the episode clears.
		if paneApproval.pending {
			mergedRaw = ActivityNeedsApproval
			mergedMsg = paneApproval.text
		}
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

	// Publish conversation ownership, after the trackMu release.
	//
	// Age alone cannot settle who owns a conversation in a shared opencode store: a
	// session that started FIRST and then sat idle will happily adopt the
	// conversation a later session is actively using, because that conversation is
	// newer than the adopter. Only the hub sees every session, so the hub is what
	// tells each watcher which ids are already spoken for — every tick, because a
	// neighbour's pin usually does not exist yet when the watcher is built.
	h.publishClaims()

	// Janitor for the cursor ingest queues: drop anything held for a slug that is gone or
	// that never grew a watcher within the TTL (hub_ingest.go). Runs after the trackMu
	// release — it takes its own lock and the two are never held together.
	h.prunePreWatcher(now, present)

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
		// When this RC session was created. opencode's store is shared per PROJECT,
		// so /session lists a neighbouring RC session's conversations too and the
		// directory alone cannot tell them apart — the watcher refuses to adopt one
		// older than the session itself. Unparseable/absent → zero, which disables
		// the check.
		notBefore, _ := time.Parse(time.RFC3339, s.CreatedAt)
		return newOpencodeWatcher(port, s.Workdir, agentID, notBefore, h.cfg.now, h.cfg.logf)
	}

	// cursor diverges from BOTH paths: its watcher is PUSH-fed by the agent's own hook
	// scripts (watch_cursor.go), so there is nothing to correlate and nothing to connect —
	// no file to find, no port to read, no retry budget to spend. It is built on the first
	// eligible tick and is immediately usable; the pin arrives later, inside the hook
	// payloads (drainConfirmedAgentID, like opencode). The one construction-time step is
	// draining the hub's PRE-WATCHER queue into it: hook events for this slug that landed
	// before this moment (the kickoff prompt above all) are pushed now, so they fold on
	// this very tick rather than being lost to the gap between create and first tick.
	if s.Kind == KindCursor {
		w := newCursorWatcher(agentSessionEnv(h.cfg.runner, s.TmuxSession), h.cfg.logf)
		h.drainPreWatcher(s.Slug, w)
		// Restart backfill (plan 008 §3.5 "Transcript tail = restart backfill only" / C5):
		// a VALIDATED prior pin (a hub restart mid-session — newCursorWatcher already
		// discarded a malformed one) means there is a transcript worth reading once, so a
		// restarted hub is not blank until the next hook fires. A fresh session (no prior
		// pin) attempts no read at all. Best-effort — see seedFromTranscript's doc.
		if w.priorID != "" {
			w.seedFromTranscript(h.cfg.getenv("HOME"), s.Workdir, w.priorID)
		}
		return w
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
