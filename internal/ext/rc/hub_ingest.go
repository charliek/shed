package rc

import (
	"errors"
	"io"
	"net/http"
	"regexp"
	"time"
)

// The cursor hook INGEST route (plan 008 §3.5):
//
//	POST /v1/ingest/cursor?slug=<slug>&event=<hookEvent>   body: cursor's raw hook payload
//
// It is the push half of the cursor watcher (watch_cursor.go): cursor-agent has no server
// to subscribe to, so the hub preseeds a hook script (preseed_cursor.go) that relays every
// interesting event here. This is the ONLY hub route whose caller is a process INSIDE the
// shed rather than the server's proxy, and it is deliberately NOT on the proxy allowlist
// (internal/api/rchub.go): nothing outside the shed has any business injecting a session's
// feed. A test in internal/api/rchub_test.go pins that /rc/v1/ingest/… is rejected before
// the proxy dials.
//
// It carries its OWN body cap, not the 16 KiB one every other POST shares
// (hubMaxBodyBytes): afterShellExecution.output is the feed's only source of tool output
// and routinely exceeds 16 KiB for build-style commands. The per-FIELD 8 KiB ring cap
// still applies on the fold, so the larger cap buys fidelity at the ingest hop only.
//
// EVERY failure mode is a plain HTTP status with a JSON envelope and nothing else: the
// hook script ignores the response entirely (it must — see cursorHookScript), so these
// codes exist for operators and tests, never to steer the agent.

const (
	// hubIngestMaxBodyBytes caps one hook payload (413 past it, event dropped — the feed
	// just misses that event; the session is otherwise unaffected).
	hubIngestMaxBodyBytes = 256 << 10 // 256 KiB

	// maxPreWatcherEvents / maxPreWatcherBytes bound the per-slug PRE-WATCHER inbox: hook
	// events that arrive before reconcile has built the session's watcher. The window is
	// real and matters — `shed attach --kind cursor --prompt …` delivers the kickoff
	// prompt within a second of create, well before the first reconcile tick, and that
	// beforeSubmitPrompt is the feed's first row.
	maxPreWatcherEvents = 32
	maxPreWatcherBytes  = 256 << 10 // 256 KiB total across the slug's queued events

	// preWatcherTTL is how long a queue is held for a slug that never grows a watcher (a
	// session that died at create, a slug that vanished, a kind whose watcher is never
	// built). Checked on reconcile ticks; the whole queue is dropped, not trimmed —
	// half a turn's events are worse than none.
	preWatcherTTL = 60 * time.Second
)

// cursorHookEventRe bounds the ?event= token: cursor's hook names are camelCase
// identifiers. It is validated because the value is stored, compared, and logged — and
// because an unbounded query parameter is exactly the kind of field that grows into a
// ring-filling payload.
var cursorHookEventRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,63}$`)

// preWatcherQueue is one slug's bounded queue of hook events awaiting its watcher.
type preWatcherQueue struct {
	events []cursorHookEvent
	bytes  int
	first  time.Time // when the queue was opened (the TTL clock)
}

// handleIngestCursor serves POST /v1/ingest/cursor. Precedence mirrors the verb handlers
// (body size → request validation → tracked lookup → capability), so a malformed request's
// answer never depends on which sessions happen to exist:
//
//	413 too_large      — the payload exceeds the ingest cap (event dropped)
//	400 invalid_slug / invalid_event — malformed query parameters
//	404 unknown_slug   — no such rc session
//	409 not_supported  — the slug is tracked but is not a cursor session
//	202                — accepted (pushed to the watcher, or queued for it)
func (h *Hub) handleIngestCursor(w http.ResponseWriter, r *http.Request) {
	// Its OWN MaxBytesReader — the 256 KiB ingest cap, not the shared 16 KiB POST cap.
	r.Body = http.MaxBytesReader(w, r.Body, hubIngestMaxBodyBytes)
	payload, err := io.ReadAll(r.Body)
	if err != nil {
		// NOT wroteTooLarge (hub_verbs.go): that helper spells the shared 16 KiB cap, which
		// would be a lie on this route.
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "too_large",
				"hook payload exceeds 256 KiB")
			return
		}
		writeError(w, http.StatusBadRequest, "invalid_body", "hook payload could not be read")
		return
	}

	slug := r.URL.Query().Get("slug")
	if !ValidCallerSlug(slug) {
		writeError(w, http.StatusBadRequest, "invalid_slug", "slug is missing or malformed")
		return
	}
	event := r.URL.Query().Get("event")
	if !cursorHookEventRe.MatchString(event) {
		writeError(w, http.StatusBadRequest, "invalid_event", "event is missing or malformed")
		return
	}

	// The tracked entry is the authorization: an untracked slug is a 404 (the handleMessages
	// rule — no re-derivation from tmux), and a tracked slug of another kind is a 409, because
	// this payload shape is cursor's and folding it anywhere else would be a category error.
	h.trackMu.Lock()
	tr, tracked := h.tracked[slug]
	var (
		kind    Kind
		watcher sessionWatcher
	)
	if tracked {
		kind, watcher = tr.kind, tr.watcher
	}
	h.trackMu.Unlock()
	if !tracked {
		writeError(w, http.StatusNotFound, "unknown_slug", "no such rc session")
		return
	}
	if kind != KindCursor {
		writeError(w, http.StatusConflict, errNotSupported,
			"this session's kind does not ingest cursor hook events")
		return
	}

	// The watcher pointer was copied out under trackMu (reconcile commits it under the same
	// lock); the push itself takes the WATCHER's mutex, never the hub's — so a hook event
	// arriving mid-tick can never block reconcile's tracked-state work.
	//
	// THE QUEUE IS FOR THE PRE-WATCHER WINDOW ONLY. Once a watcher exists the event is
	// either pushed or DROPPED, never queued. Two reasons, both real: nothing drains a queue
	// after construction (ensureWatcher no-ops once tr.watcher is set), so a queued event
	// would occupy the slug's budget until the TTL; and worse, an event queued against a
	// CLOSED watcher would be drained into the NEXT watcher built at that slug — folding a
	// dead incarnation's turn into a recreated session's feed. A refusal means the watcher is
	// closed (the session is going away) or its inbox is full (which already recorded a gap);
	// dropping is the honest answer to both.
	//
	// Either way the answer is 202: the hook script cannot act on anything else.
	ev := cursorHookEvent{event: event, payload: payload}
	switch ing, isIngester := watcher.(cursorIngester); {
	case watcher == nil:
		// The create → first-reconcile-tick window, where the kickoff prompt's
		// beforeSubmitPrompt lands. Hold it for the watcher to drain on construction.
		h.queuePreWatcher(slug, ev)
	case !isIngester:
		// Unreachable today (a tracked cursor session's watcher is always a cursorWatcher);
		// dropping rather than queueing keeps the invariant above true if that ever changes.
		h.cfg.logf("rc hub: cursor session %s has a non-ingesting watcher; dropping %s", slug, event)
	default:
		if !ing.pushHookEvent(ev) {
			h.cfg.logf("rc hub: cursor watcher for %s refused a %s event (closed or full); dropping", slug, event)
		}
	}
	writeJSON(w, http.StatusAccepted, map[string]bool{"accepted": true})
}

// queuePreWatcher appends an event to the slug's pre-watcher queue under ingestMu (its own
// lock — never trackMu, so an ingest burst cannot contend with reconcile). Overflow of
// either bound drops the event: the queue exists to preserve the FIRST events of a session
// (above all the kickoff prompt), and the watcher normally exists within one tick, so a
// queue that fills means something is wrong and dropping is better than growing.
func (h *Hub) queuePreWatcher(slug string, ev cursorHookEvent) {
	h.ingestMu.Lock()
	defer h.ingestMu.Unlock()
	q := h.preWatcher[slug]
	if q == nil {
		q = &preWatcherQueue{first: h.cfg.now()}
		h.preWatcher[slug] = q
	}
	if len(q.events) >= maxPreWatcherEvents || q.bytes+len(ev.payload) > maxPreWatcherBytes {
		h.cfg.logf("rc hub: cursor pre-watcher queue full for %s; dropping %s event", slug, ev.event)
		return
	}
	q.events = append(q.events, ev)
	q.bytes += len(ev.payload)
}

// drainPreWatcher hands a freshly built watcher every event queued for its slug, in
// arrival order, and clears the queue. Reconcile calls it TWICE per construction, and both
// calls matter: once at construction (before the watcher's first refresh, so everything
// already queued folds on the same tick the watcher appears — the kickoff prompt is in the
// feed immediately), and once right after tr.watcher is published under trackMu, to sweep
// up anything the ingest handler queued during the window in between (see hub_reconcile.go).
// Idempotent: the second call finds an empty map entry and does nothing.
func (h *Hub) drainPreWatcher(slug string, watcher sessionWatcher) {
	ing, ok := watcher.(cursorIngester)
	if !ok {
		return
	}
	h.ingestMu.Lock()
	q := h.preWatcher[slug]
	delete(h.preWatcher, slug)
	h.ingestMu.Unlock()
	if q == nil {
		return
	}
	for _, ev := range q.events {
		ing.pushHookEvent(ev)
	}
}

// prunePreWatcher drops queues whose slug still has no watcher after preWatcherTTL, and
// any queue for a slug that is no longer present at all. Run on every reconcile tick —
// without it, hook events for a session that died at create would be retained for the
// hub's whole life.
func (h *Hub) prunePreWatcher(now time.Time, present map[string]bool) {
	h.ingestMu.Lock()
	defer h.ingestMu.Unlock()
	for slug, q := range h.preWatcher {
		if !present[slug] || now.Sub(q.first) >= preWatcherTTL {
			delete(h.preWatcher, slug)
		}
	}
}
