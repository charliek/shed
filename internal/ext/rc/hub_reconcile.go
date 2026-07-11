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
//     only advertises working|needs_input|idle|unknown — the suppressed value ("")
//     is never emitted as an activity.changed (a strict decoder would reject it);
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
	tracker   *StabilityTracker

	// activity is the DISPLAYED activity (DisplayActivity already applied): "" means
	// the activity dimension is suppressed (blocking lifecycle state). Used for both
	// change detection and the /v1/sessions overlay.
	activity    Activity
	activityAt  string // RFC3339 time the displayed activity last changed
	lastMessage string // populated by the watcher commit; "" here
	lastState   State
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
		tracker:   NewStabilityTracker(s.Kind, capture, h.cfg.now, h.cfg.quiet),
		lastState: s.State,
	}
}

// sameIdentity reports whether s is still the session this tracker state was built
// for (see the id/createdAt doc on trackedSession).
func (tr *trackedSession) sameIdentity(s Session) bool {
	return tr.id == s.ID && tr.createdAt == s.CreatedAt
}

// reconcile runs one enumeration+tick pass and broadcasts the resulting events. It
// holds trackMu only while mutating tracked state (events are collected into a
// slice), then broadcasts after unlocking — so a broadcast can never block reconcile
// against an SSE handler.
func (h *Hub) reconcile() {
	sessions := List(h.cfg.runner, nil).RCSessions
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
			// latter catches legacy sessions with no SHED_RC_ID) → start over.
			tr = h.newTrackedSession(s)
			h.tracked[s.Slug] = tr
			events = append(events, sessionUpdatedEvent(s))
		} else if tr.lastState != s.State {
			events = append(events, sessionUpdatedEvent(s))
		}
		tr.lastState = s.State

		// Derive activity from the pane-stability tracker. A capture error (e.g. a
		// transient tmux hiccup) leaves the prior activity untouched rather than
		// flapping the DTO to unknown.
		raw, err := tr.tracker.Tick()
		if err != nil {
			continue
		}
		eff := DisplayActivity(s.State, raw)
		if eff != tr.activity {
			tr.activity = eff
			tr.activityAt = now.UTC().Format(time.RFC3339)
			// Contract: activity.changed carries only valid non-empty activity values.
			// A transition INTO suppression (eff == "", i.e. the state just became
			// needs-trust/needs-auth/dead) is announced by the session.updated the
			// state change emitted above — clients drop the activity dimension from
			// the state, not from a hollow activity event.
			if eff != "" {
				events = append(events, activityChangedEvent(s.Slug, eff, tr.activityAt, s.State))
			}
		}
	}

	// Sessions that vanished since the last pass (killed).
	for slug := range h.tracked {
		if !present[slug] {
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

	for _, e := range events {
		h.broadcast(e)
	}
}
