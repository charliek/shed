package rc

import (
	"bytes"
	"encoding/json"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// SSE fan-out for GET /v1/events. Each connected client gets a subscriber with a
// bounded buffered channel; the reconcile loop broadcasts pre-encoded event frames
// to every subscriber with a NON-BLOCKING send — a slow client whose queue is full
// has events DROPPED rather than stalling the broadcaster (the egress-stream
// precedent). SSE here is best-effort notification: a client that misses events
// refetches the /v1/sessions snapshot on reconnect (no Last-Event-ID replay).

// subscriber is one connected SSE client.
type subscriber struct {
	ch      chan []byte   // buffered queue of encoded frames
	closed  chan struct{} // closed by the hub on idle-exit to end the stream
	once    sync.Once     // guards a single close of closed
	dropped atomic.Int64  // frames dropped due to a full queue (debug/metric)
}

func (s *subscriber) close() {
	s.once.Do(func() { close(s.closed) })
}

// hubEvent is one SSE event: a name (the `event:` field) and a JSON-serializable
// data payload (the `data:` field).
type hubEvent struct {
	name string
	data any
}

// frame encodes the event into the SSE wire form:
//
//	event: <name>\n
//	data: <json>\n
//	\n
func (e hubEvent) frame() []byte {
	body, err := json.Marshal(e.data)
	if err != nil {
		body = []byte("{}")
	}
	var buf bytes.Buffer
	buf.WriteString("event: ")
	buf.WriteString(e.name)
	buf.WriteByte('\n')
	buf.WriteString("data: ")
	buf.Write(body)
	buf.WriteString("\n\n")
	return buf.Bytes()
}

// SSE envelope payloads. `shed` is part of the wire contract but is unknown to the
// guest-local hub (the binary does not know the orchestrator's shed alias) — it is
// left empty here and filled in by the server-side aggregator, which knows which
// shed each upstream belongs to.
//
// Contract: `activity` is always one of the advertised non-empty enum values
// (working|needs_input|idle|unknown) — an activity.changed is NEVER emitted for
// the suppressed ("") dimension. When a blocking lifecycle state (needs-trust/
// needs-auth/dead) suppresses activity, the transition is announced by a
// session.updated (which carries the state); clients derive "drop the activity
// badge" from that state, per the lifecycle-trumps-activity precedence.
type activityChangedData struct {
	Shed       string   `json:"shed"`
	Slug       string   `json:"slug"`
	Activity   Activity `json:"activity"`
	ActivityAt string   `json:"activity_at"`
	State      State    `json:"state"`
}

type sessionUpdatedData struct {
	Shed    string   `json:"shed"`
	Slug    string   `json:"slug"`
	Session *Session `json:"session"` // null on disappear (kill)
}

func activityChangedEvent(slug string, activity Activity, activityAt string, state State) hubEvent {
	return hubEvent{name: "activity.changed", data: activityChangedData{
		Slug: slug, Activity: activity, ActivityAt: activityAt, State: state,
	}}
}

// sessionUpdatedEvent fires on appear / recreate / lifecycle-state change; it
// carries the base session DTO (activity travels on activity.changed). A copy is
// taken so a later mutation of the caller's slice can't alias the event payload.
func sessionUpdatedEvent(s Session) hubEvent {
	cp := s
	return hubEvent{name: "session.updated", data: sessionUpdatedData{Slug: s.Slug, Session: &cp}}
}

// sessionGoneEvent fires when a tracked session disappears (killed). session is
// null — clients refetch the snapshot.
func sessionGoneEvent(slug string) hubEvent {
	return hubEvent{name: "session.updated", data: sessionUpdatedData{Slug: slug, Session: nil}}
}

// subscribe registers a new SSE subscriber.
func (h *Hub) subscribe() *subscriber {
	s := &subscriber{
		ch:     make(chan []byte, h.cfg.subBuffer),
		closed: make(chan struct{}),
	}
	h.subMu.Lock()
	h.subs[s] = struct{}{}
	h.subMu.Unlock()
	return s
}

// unsubscribe removes a subscriber (on client disconnect).
func (h *Hub) unsubscribe(s *subscriber) {
	h.subMu.Lock()
	delete(h.subs, s)
	h.subMu.Unlock()
}

// subscriberCount is the current number of SSE clients (drives the reconcile
// cadence).
func (h *Hub) subscriberCount() int {
	h.subMu.Lock()
	defer h.subMu.Unlock()
	return len(h.subs)
}

// broadcast delivers an event to every subscriber. The per-subscriber send is
// non-blocking: a full queue drops the frame (counted) instead of blocking the
// broadcaster. Encoding happens once, up front, so the same bytes are shared.
func (h *Hub) broadcast(e hubEvent) {
	frame := e.frame()
	h.subMu.Lock()
	defer h.subMu.Unlock()
	for s := range h.subs {
		select {
		case s.ch <- frame:
		default:
			s.dropped.Add(1)
		}
	}
}

// closeAllSubscribers ends every SSE stream (on idle-exit / shutdown). Idempotent
// per subscriber via the once guard.
func (h *Hub) closeAllSubscribers() {
	h.subMu.Lock()
	defer h.subMu.Unlock()
	for s := range h.subs {
		s.close()
	}
}

// handleEvents serves GET /v1/events as an SSE stream: it registers a subscriber,
// then writes queued event frames plus a periodic heartbeat comment until the
// client disconnects or the hub closes the stream (idle-exit). Write deadlines
// bound each frame so a wedged client can't stall the writer.
func (h *Hub) handleEvents(w http.ResponseWriter, r *http.Request) {
	// Headers must be set BEFORE the first flush/write, which commits the 200 status.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	rc := http.NewResponseController(w)
	// SSE clients need each frame flushed immediately; a ResponseWriter that can't
	// flush can't stream, so refuse rather than buffer the whole response. The flush
	// commits the (already-set) headers + 200 status.
	if err := rc.Flush(); err != nil {
		writeError(w, http.StatusInternalServerError, "no_flush", "streaming unsupported")
		return
	}

	sub := h.subscribe()
	defer h.unsubscribe(sub)

	// Open the stream with a comment so proxies see bytes immediately.
	if h.writeSSE(w, rc, []byte(": ok\n\n")) != nil {
		return
	}

	// Heartbeat comment every ~25s keeps the connection warm through idle periods
	// (the mobile client's pinned reader idles at 120s, comfortably above this).
	heartbeat := time.NewTicker(h.cfg.heartbeat)
	defer heartbeat.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return // client went away
		case <-sub.closed:
			return // hub idle-exit / shutdown closed the stream
		case frame := <-sub.ch:
			if h.writeSSE(w, rc, frame) != nil {
				return
			}
		case <-heartbeat.C:
			if h.writeSSE(w, rc, []byte(": heartbeat\n\n")) != nil {
				return
			}
		}
	}
}

// writeSSE writes AND flushes one SSE chunk under a single write deadline. The
// deadline must cover the Flush too, not just the Write: with HTTP chunked
// encoding the bytes can sit in the server's buffer until the flush pushes them
// onto the (possibly wedged) connection — an uncovered Flush would block forever
// on a client that stopped reading, parking this handler as a permanent
// subscriber (and pinning the reconcile loop at the fast cadence). Any error
// (deadline hit included) makes the caller return and unsubscribe.
func (h *Hub) writeSSE(w http.ResponseWriter, rc *http.ResponseController, b []byte) error {
	_ = rc.SetWriteDeadline(time.Now().Add(h.cfg.writeTimeout))
	_, err := w.Write(b)
	if err == nil {
		err = rc.Flush()
	}
	_ = rc.SetWriteDeadline(time.Time{})
	return err
}
