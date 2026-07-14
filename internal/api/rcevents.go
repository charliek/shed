package api

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/ext/rc"
)

// GET /api/rc/events is the server-side aggregate SSE stream: a single stream a
// mobile/desktop client subscribes to for live rc activity across every shed on
// this host. It is DEMAND-DRIVEN — with zero connected clients it holds zero
// upstream hub connections; the first client spins up one upstream SSE reader per
// shed that is running AND has rc sessions, and the last client to leave tears
// them all down. Upstream envelope events have their (guest-blank) `shed` field
// filled in and are re-broadcast to every client; an upstream drop yields a
// synthetic hub.unavailable and exponential backoff (max 30s); a shed leaving
// candidacy (stopped/deleted) yields shed.stopped and stops that reader.
//
// SSE here is best-effort notification (the egress-stream / hub precedent): each
// client has a 256-buffered channel and a full queue DROPS frames rather than
// blocking the aggregator; clients refetch snapshots (overview / messages) on
// reconnect.

const (
	rcAggClientBuffer   = 256
	rcAggHeartbeat      = 25 * time.Second
	rcAggWriteTimeout   = 10 * time.Second
	rcAggRescanInterval = 30 * time.Second
	rcAggBackoffInitial = 500 * time.Millisecond
	rcAggBackoffMax     = 30 * time.Second
	// rcAggUpstreamLineLimit caps a single upstream SSE line the aggregator parses
	// (defense against a broken/hostile hub); the hub emits small frames.
	rcAggUpstreamLineLimit = 256 << 10
)

// aggClient is one connected /api/rc/events subscriber. Its lifetime is bounded
// by its HTTP request context (there is no aggregator-initiated close): the
// handler deregisters it on return, and the last client leaving tears down the
// upstream machinery. The closed channel is a never-firing select arm kept for
// symmetry with the hub's subscriber shape and as a seam for a future
// server-initiated close.
type aggClient struct {
	ch     chan []byte
	closed chan struct{}
}

// rcAggregator fans out per-shed upstream hub SSE into the set of connected
// clients, demand-driven. All maps are guarded by mu.
type rcAggregator struct {
	mu      sync.Mutex
	clients map[*aggClient]struct{}
	readers map[string]*shedReader
	// managerCancel cancels the manager + all reader contexts when the last
	// client leaves; nil when no manager is running.
	managerCancel context.CancelFunc
	// managerGen is the running manager's generation, bumped on every manager
	// start. A rescan captures its manager's generation and re-checks it under mu
	// before mutating readers: a just-canceled manager whose discover call was
	// in flight when a NEW manager was installed (last client left, next client
	// arrived) would otherwise win the lock afterwards and insert readers bound
	// to its dead context into the new manager's map — sheds that then silently
	// deliver no events. The stale-generation rescan is discarded instead.
	managerGen uint64

	// Injectable seams (production wired from the Server; tests substitute).
	discover     func(ctx context.Context) []string // candidate shed names (running + has rc sessions)
	openUpstream func(ctx context.Context, shed string) (io.ReadCloser, error)

	// Tunables (defaults applied by newRCAggregator; overridable in tests).
	rescanInterval time.Duration
	backoffInitial time.Duration
	backoffMax     time.Duration
}

// shedReader is one upstream SSE reader (one per candidate shed while at least
// one client is connected).
type shedReader struct {
	shed   string
	agg    *rcAggregator
	gone   chan struct{} // closed by the manager when the shed leaves candidacy
	cancel context.CancelFunc
}

// newRCAggregator builds the aggregator with production seams wired to s.
func (s *Server) newRCAggregator() *rcAggregator {
	a := &rcAggregator{
		clients:        map[*aggClient]struct{}{},
		readers:        map[string]*shedReader{},
		rescanInterval: rcAggRescanInterval,
		backoffInitial: rcAggBackoffInitial,
		backoffMax:     rcAggBackoffMax,
	}
	a.discover = s.discoverRCSheds
	a.openUpstream = s.openHubEvents
	return a
}

// discoverRCSheds returns the names of running sheds that currently have at least
// one rc-* tmux session — the aggregator's upstream candidate set.
//
// Discovery choice: this reuses the cheap tmux-session listing (ListSheds +
// ListSessions), NOT a guest exec of `shed-ext-rc` and NOT an ensure-start. A
// shed with live rc sessions already has a running hub (create/enrichment/proxy
// started it, and a hub with sessions never idle-exits), so a plain
// name-prefix scan of the tmux session list is a sufficient, exec-free signal.
// Sheds with no rc sessions are excluded so the aggregator never opens (or
// hub.unavailable-spams) an upstream for a shed that never used rc.
func (s *Server) discoverRCSheds(ctx context.Context) []string {
	sheds, err := s.backend.ListSheds(ctx)
	if err != nil {
		return nil
	}
	var out []string
	for i := range sheds {
		if sheds[i].Status != config.StatusRunning {
			continue
		}
		sess, err := s.backend.ListSessions(ctx, sheds[i].Name)
		if err != nil {
			continue
		}
		for j := range sess {
			if strings.HasPrefix(sess[j].Name, rc.TmuxPrefix) {
				out = append(out, sheds[i].Name)
				break
			}
		}
	}
	return out
}

// openHubEvents opens an upstream SSE connection to shed's rc hub GET /v1/events
// through the hub transport. It never ensure-starts (the aggregator is a passive
// observer): a shed whose hub is down surfaces as an error here, which the reader
// turns into a hub.unavailable + backoff.
func (s *Server) openHubEvents(ctx context.Context, shed string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+rc.HubAddr+"/v1/events", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/event-stream")
	client := &http.Client{Transport: s.newHubTransport(shed)} // no client timeout: SSE is long-lived
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, &httpStatusError{code: resp.StatusCode}
	}
	return resp.Body, nil
}

type httpStatusError struct{ code int }

func (e *httpStatusError) Error() string { return http.StatusText(e.code) }

// handleRCEvents serves GET /api/rc/events. Control scope, GET-only (registered
// as GET; the auth middleware's default branch requires a control token). It
// registers a client, lazily starting the demand-driven upstream machinery, and
// streams frames until the client disconnects.
func (s *Server) handleRCEvents(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	rcCtl := http.NewResponseController(w)
	if err := rcCtl.Flush(); err != nil {
		writeError(w, http.StatusInternalServerError, "NO_FLUSH", "streaming unsupported")
		return
	}

	client := &aggClient{ch: make(chan []byte, rcAggClientBuffer), closed: make(chan struct{})}
	s.rcAgg.addClient(client)
	defer s.rcAgg.removeClient(client)

	if writeAggSSE(w, rcCtl, []byte(": ok\n\n")) != nil {
		return
	}
	heartbeat := time.NewTicker(rcAggHeartbeat)
	defer heartbeat.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-client.closed:
			return
		case frame := <-client.ch:
			if writeAggSSE(w, rcCtl, frame) != nil {
				return
			}
		case <-heartbeat.C:
			if writeAggSSE(w, rcCtl, []byte(": heartbeat\n\n")) != nil {
				return
			}
		}
	}
}

// writeAggSSE writes AND flushes one SSE chunk under a single write deadline so a
// wedged client can't stall the writer (the hub's writeSSE precedent).
func writeAggSSE(w http.ResponseWriter, rcCtl *http.ResponseController, b []byte) error {
	_ = rcCtl.SetWriteDeadline(time.Now().Add(rcAggWriteTimeout))
	_, err := w.Write(b)
	if err == nil {
		err = rcCtl.Flush()
	}
	_ = rcCtl.SetWriteDeadline(time.Time{})
	return err
}

// addClient registers a client, starting the upstream manager on the transition
// from zero to one client.
func (a *rcAggregator) addClient(c *aggClient) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.clients[c] = struct{}{}
	if len(a.clients) == 1 {
		ctx, cancel := context.WithCancel(context.Background())
		a.managerCancel = cancel
		a.managerGen++
		go a.runManager(ctx, a.managerGen)
	}
}

// removeClient deregisters a client, tearing down ALL upstream readers on the
// transition back to zero clients (demand-driven: zero clients ⇒ zero upstreams).
func (a *rcAggregator) removeClient(c *aggClient) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.clients, c)
	if len(a.clients) == 0 && a.managerCancel != nil {
		a.managerCancel() // cancels the manager + every reader context
		a.managerCancel = nil
		a.readers = map[string]*shedReader{}
	}
}

// runManager discovers candidate sheds on start and every rescanInterval while
// clients remain, starting a reader for each new candidate and signaling
// (shed.stopped) + stopping a reader whose shed left candidacy. It exits when its
// context is canceled (last client left). gen is this manager's generation (see
// managerGen); every rescan carries it so a stale manager's rescan is discarded.
func (a *rcAggregator) runManager(ctx context.Context, gen uint64) {
	a.rescan(ctx, gen)
	ticker := time.NewTicker(a.rescanInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.rescan(ctx, gen)
		}
	}
}

// rescan reconciles the reader set against the current candidate sheds. The
// discover call runs unlocked; before mutating readers it re-checks, under mu,
// that this manager is still the CURRENT one (generation match) and not canceled
// — an old manager racing a new one must never touch the new manager's readers.
func (a *rcAggregator) rescan(ctx context.Context, gen uint64) {
	candidates := a.discover(ctx)
	if ctx.Err() != nil {
		return
	}
	want := make(map[string]struct{}, len(candidates))
	for _, name := range candidates {
		want[name] = struct{}{}
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	if a.managerCancel == nil || a.managerGen != gen || ctx.Err() != nil {
		return // torn down or superseded between discover and here
	}
	// Start readers for new candidates.
	for name := range want {
		if _, ok := a.readers[name]; ok {
			continue
		}
		rctx, cancel := context.WithCancel(ctx)
		sr := &shedReader{shed: name, agg: a, gone: make(chan struct{}), cancel: cancel}
		a.readers[name] = sr
		go sr.run(rctx)
	}
	// Signal + stop readers whose shed left candidacy.
	for name, sr := range a.readers {
		if _, ok := want[name]; ok {
			continue
		}
		delete(a.readers, name)
		close(sr.gone) // reader emits shed.stopped then returns
		sr.cancel()    // unblock any in-flight upstream read
	}
}

// run is the per-shed upstream reader loop: (re)open the upstream SSE, pump its
// events (filling in `shed` and re-broadcasting), and on any drop emit a
// synthetic hub.unavailable and back off (exponential, capped). It returns when
// its context is canceled — silently on a zero-client teardown, or emitting
// shed.stopped when the shed left candidacy (gone closed).
func (sr *shedReader) run(ctx context.Context) {
	backoff := sr.agg.backoffInitial
	for {
		if sr.stopped(ctx) {
			sr.finish(ctx)
			return
		}
		body, err := sr.agg.openUpstream(ctx, sr.shed)
		if err != nil {
			sr.agg.broadcast(hubUnavailableFrame(sr.shed))
			if !sr.waitBackoff(ctx, backoff) {
				sr.finish(ctx)
				return
			}
			backoff = nextBackoff(backoff, sr.agg.backoffMax)
			continue
		}
		backoff = sr.agg.backoffInitial
		sr.pump(ctx, body)
		body.Close()
		if sr.stopped(ctx) {
			sr.finish(ctx)
			return
		}
		// Upstream ended on its own (hub closed the stream / idle-exit): announce
		// it and back off before reconnecting.
		sr.agg.broadcast(hubUnavailableFrame(sr.shed))
		if !sr.waitBackoff(ctx, backoff) {
			sr.finish(ctx)
			return
		}
		backoff = nextBackoff(backoff, sr.agg.backoffMax)
	}
}

// stopped reports whether the reader should stop (context canceled or the shed
// left candidacy).
func (sr *shedReader) stopped(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return true
	case <-sr.gone:
		return true
	default:
		return false
	}
}

// finish emits shed.stopped when the reader is stopping because the shed left
// candidacy (gone). A plain teardown (last client left) is silent — there are no
// clients to notify.
func (sr *shedReader) finish(_ context.Context) {
	select {
	case <-sr.gone:
		sr.agg.broadcast(shedStoppedFrame(sr.shed))
	default:
	}
}

// waitBackoff sleeps for d, returning false (stop) if the context is canceled or
// the shed leaves candidacy first.
func (sr *shedReader) waitBackoff(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-sr.gone:
		return false
	case <-timer.C:
		return true
	}
}

// pump reads the upstream SSE stream, re-broadcasting each parsed envelope event
// (with `shed` filled in) to all clients. It returns when the stream ends/errors
// or the context is canceled.
func (sr *shedReader) pump(ctx context.Context, body io.ReadCloser) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 8<<10), rcAggUpstreamLineLimit)

	var eventName string
	var dataLines []string
	var dataSize int // cumulative len of the current event's data lines
	var dropped bool // this event exceeded the cap: discard it until the terminating blank
	dispatch := func() {
		defer func() { eventName = ""; dataLines = nil; dataSize = 0; dropped = false }()
		if dropped || eventName == "" || len(dataLines) == 0 {
			return
		}
		data := strings.Join(dataLines, "\n")
		if frame, ok := sr.agg.rewriteFrame(sr.shed, eventName, data); ok {
			sr.agg.broadcast(frame)
		}
	}

	for scanner.Scan() {
		if ctx.Err() != nil {
			return
		}
		line := scanner.Text()
		switch {
		case line == "":
			dispatch() // blank line terminates an event (and clears the drop flag)
		case strings.HasPrefix(line, ":"):
			// comment / heartbeat — ignore
		case strings.HasPrefix(line, "event:"):
			eventName = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			// scanner.Buffer caps a SINGLE line at rcAggUpstreamLineLimit, but a
			// broken/hostile hub could pile up many data lines before the terminating
			// blank and grow this event unboundedly. Cap the CUMULATIVE data size too:
			// once over, mark the event dropped and free the buffer, then keep skipping
			// its lines until the blank line resyncs to a fresh event.
			if dropped {
				continue
			}
			d := strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " ")
			dataSize += len(d)
			if dataSize > rcAggUpstreamLineLimit {
				dropped = true
				dataLines = nil // release the partial buffer immediately
				continue
			}
			dataLines = append(dataLines, d)
		default:
			// unknown SSE field — ignore
		}
	}
}

// rewriteFrame re-encodes an upstream event with `shed` injected into its JSON
// data object, returning the wire SSE frame. Only the known envelope events are
// forwarded; anything else is dropped (ok=false). A data payload that isn't a
// JSON object is dropped rather than forwarded raw.
//
// Trust boundary: the forwarded payload fields (slug/activity/last_message/seq,
// …) are guest-controlled and must be treated as untrusted by clients; `shed` is
// always server-corrected here, and the synthetic event types (hub.unavailable,
// shed.stopped) are not in the forwardable allowlist above, so a guest hub
// cannot spoof them.
func (a *rcAggregator) rewriteFrame(shed, event, data string) ([]byte, bool) {
	switch event {
	case "activity.changed", "session.updated", "message.appended":
	default:
		return nil, false
	}
	var obj map[string]json.RawMessage
	// json.Unmarshal of the literal `null` sets obj to nil WITHOUT an error
	// (golang/go#10411); assigning obj["shed"] below would then panic with
	// "assignment to entry in nil map" and crash the server. The data payload is
	// guest-controlled, so a `data: null` frame is a reachable crash vector —
	// reject a nil (or non-object) result before the assignment.
	if err := json.Unmarshal([]byte(data), &obj); err != nil || obj == nil {
		return nil, false
	}
	shedJSON, _ := json.Marshal(shed)
	obj["shed"] = shedJSON
	body, err := json.Marshal(obj)
	if err != nil {
		return nil, false
	}
	return encodeSSEFrame(event, body), true
}

// broadcast delivers frame to every client with a non-blocking send — a full
// client queue DROPS the frame rather than blocking the aggregator.
func (a *rcAggregator) broadcast(frame []byte) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for c := range a.clients {
		select {
		case c.ch <- frame:
		default: // drop on overflow
		}
	}
}

// hubUnavailableFrame is the synthetic event emitted when a shed's upstream hub
// connection drops. `shed` identifies which host degraded.
func hubUnavailableFrame(shed string) []byte {
	body, _ := json.Marshal(map[string]string{"shed": shed})
	return encodeSSEFrame("hub.unavailable", body)
}

// shedStoppedFrame is the synthetic event emitted when a shed leaves candidacy
// (stopped/deleted) and its reader tears down.
func shedStoppedFrame(shed string) []byte {
	body, _ := json.Marshal(map[string]string{"shed": shed})
	return encodeSSEFrame("shed.stopped", body)
}

// encodeSSEFrame builds the SSE wire form: event: <name>\ndata: <json>\n\n.
func encodeSSEFrame(event string, data []byte) []byte {
	var b strings.Builder
	b.WriteString("event: ")
	b.WriteString(event)
	b.WriteByte('\n')
	b.WriteString("data: ")
	b.Write(data)
	b.WriteString("\n\n")
	return []byte(b.String())
}

// nextBackoff doubles d, capped at max.
func nextBackoff(d, max time.Duration) time.Duration {
	d *= 2
	if d > max {
		return max
	}
	return d
}
