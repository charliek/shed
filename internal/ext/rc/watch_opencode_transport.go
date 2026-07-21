package rc

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// opencodeWatcher is the SSE/REST-backed sessionWatcher for an opencode session. Unlike
// the codex/claude fileWatchers — which tail a durable append-only JSONL file — opencode
// is client/server: the bare TUI runs an embedded HTTP+SSE server on a per-session port
// (SHED_RC_OPENCODE_PORT, stamped at create), and this watcher subscribes to that server's
// /event stream (plus a REST seed) as a SECOND client. It presents the same five
// sessionWatcher methods reconcile/input depend on, so reconcile stays transport-agnostic
// between a tailed file and a live SSE feed (see watch.go's sessionWatcher doc).
//
// CONCURRENCY MODEL (the load-bearing invariant — mirror watch.go's activityFold note):
//
//   - A single background goroutine (run) owns ALL HTTP I/O: correlation, the SSE read
//     loop, and the REST seed. It NEVER mutates the fold. Under the watcher mutex it only
//     (a) pushes routing-filtered raw envelope payloads (and marker records) onto the
//     bounded inbox, (b) updates the transport-health fields (connected/lastFrameAt), and
//     (c) sets the discovered-confirmedID slot. It NEVER holds the lock across a network
//     wait (Do / Body.Read / backoff sleep).
//   - refresh(now) — called by reconcile on the MAIN goroutine — is the ONLY place the
//     fold is mutated: under the mutex it drains the inbox → fold.applyLine, handles the
//     seedComplete / overflowGap markers, and reads the fold's verdict + feed. The
//     opencodeFold is NOT concurrency-safe (watch_opencode.go); this single-writer-under-
//     mutex discipline is exactly what makes it safe.
//
// See §3.3 (correlation state machine), §3.4 (transport), §3.6 (transport-health
// freshness) of the plan for the authoritative behavior this implements.
type opencodeWatcher struct {
	baseURL string // http://127.0.0.1:<port>
	workdir string // canonical (EvalSymlinks'd) session workdir, for the dir-match pin
	priorID string // a prior back-written SHED_RC_AGENT_SESSION ("" if none) — a trusted pin
	client  *http.Client
	nowFn   func() time.Time
	logf    func(string, ...any)

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{} // closed when run() returns (test leak-check / shutdown join point)

	mu   sync.Mutex
	fold *opencodeFold

	closed bool      // terminal: refresh/snapshot no-op, run exits (see close)
	body   io.Closer // the current in-flight SSE response body, for close() to unblock a read

	// gen is the connection-generation counter (incremented on each connect attempt). Every
	// marker (seedComplete/overflowGap) is tagged with the generation live when it is enqueued;
	// refresh honors a marker only while its generation is still current, so a superseded
	// connection's queued seedComplete can never make a newer connection authoritative (§3.6).
	gen uint64

	// backoff is the current reconnect backoff (run writes; a test reads via getBackoff). Reset
	// to the floor ONLY after a successful seed, never merely on server.connected (§3.4).
	backoff time.Duration

	// inbox: the goroutine pushes here; refresh drains. Bounded by BOTH count and bytes.
	inbox      []inboxItem
	inboxBytes int

	// correlation
	pinnedID      string // the session id we filter/seed on ("" while still searching)
	confirmedID   string // an SSE-discovered pin awaiting drainConfirmedAgentID ("" = none/drained)
	confirmedOnce bool   // a discovered pin was enqueued once (do not re-enqueue on reconnect)

	// transport health (goroutine writes; snapshot reads)
	connected   bool
	lastFrameAt time.Time // stamp of the most recent SSE frame (heartbeats count)
	seedApplied bool      // a seedComplete marker has been folded on the CURRENT connection

	// fold-derived verdict (refresh writes; snapshot reads)
	lastEventAt time.Time
	curActivity Activity
	curMessage  string
	curSettled  bool
	pending     []feedMessage
}

// Compile-time checks that opencodeWatcher satisfies the reconcile/input surface plus the
// small confirmed-id back-write hook C5 type-asserts (like messageProducer on the fold).
var (
	_ sessionWatcher          = (*opencodeWatcher)(nil)
	_ confirmedAgentIDDrainer = (*opencodeWatcher)(nil)
)

// confirmedAgentIDDrainer is the second, small interface reconcile type-asserts on a
// watcher (parallel to messageProducer on the fold): a stream-discovered agent session id
// the transport back-writes into SHED_RC_AGENT_SESSION so a hub restart re-correlates
// exactly. Only opencodeWatcher implements it — the fileWatchers correlate off-line.
type confirmedAgentIDDrainer interface {
	drainConfirmedAgentID() string
}

// inboxKind discriminates the records the goroutine pushes onto the inbox.
type inboxKind uint8

const (
	inboxPayload      inboxKind = iota // a raw {id,type,properties} envelope → fold.applyLine
	inboxSeedComplete                  // the seed + buffered replay is fully applied → authoritative
	inboxOverflowGap                   // the inbox overflowed → fold.noteGap() + forced resync
)

type inboxItem struct {
	kind     inboxKind
	payload  []byte
	gen      uint64       // the connection generation this item was enqueued under (marker authority)
	fallback seedFallback // seedComplete only: the REST-derived status fallback to apply if no live boundary
}

// seedFallback carries the REST /session/status result captured during a seed. It is applied by
// refresh AFTER the seed's payloads (and any buffered live events in the same batch) as a
// FALLBACK — only when no live session.status/session.idle boundary was folded (§3.4).
type seedFallback struct {
	set  bool // a status seed result is present (the seed established the boundary authoritatively)
	idle bool // true → the session was idle at connect; false → busy
}

const (
	// maxInboxItems / maxInboxBytes bound the inbox by BOTH element count AND total bytes.
	// Overflow drops the item and forces a full reconnect+reseed (drop-oldest+noteGap alone
	// can permanently miss permission.replied / idle / tool-completion).
	maxInboxItems = 1024
	maxInboxBytes = 4 << 20 // 4 MiB

	// maxSSELineBytes caps one SSE line (one data: field) and maxSSEFrameBytes caps a
	// whole frame's accumulated data. Oversized → the read errors → reconnect.
	maxSSELineBytes  = 1 << 20 // 1 MiB
	maxSSEFrameBytes = 4 << 20 // 4 MiB

	// maxRESTBytes caps a single REST response body (seed reads are bounded; the SSE GET
	// is deliberately NOT capped — it streams).
	maxRESTBytes = 8 << 20 // 8 MiB

	// dialTimeout / headerTimeout keep a dead port from hanging a connect; restTimeout
	// bounds each seed REST call. NONE of these is an overall client timeout on the SSE GET.
	dialTimeout   = 3 * time.Second
	headerTimeout = 5 * time.Second
	restTimeout   = 5 * time.Second

	// ocBackoffBase / ocBackoffMax bound the reconnect backoff (jittered).
	ocBackoffBase = 100 * time.Millisecond
	ocBackoffMax  = 5 * time.Second

	// ocFrameStaleWindow bounds how long a settled/working verdict stays authoritative
	// after the last SSE frame. opencode heartbeats ~every 10s, so ~3 missed heartbeats
	// means the stream is wedged even if the socket has not errored yet.
	ocFrameStaleWindow = 30 * time.Second
)

var (
	errWatcherClosed    = errors.New("opencode watcher closed")
	errInboxOverflow    = errors.New("opencode watcher inbox overflow")
	errSSEOversize      = errors.New("opencode watcher sse frame too large")
	errStatusSeedFailed = errors.New("opencode watcher status seed failed")
)

// newOpencodeWatcher builds a watcher for an opencode session's embedded server on
// 127.0.0.1:<port> and starts its background I/O goroutine. It is NON-BLOCKING: the
// constructor returns immediately and correlation/seed/subscribe happen on the goroutine
// (§3.3 is first-prompt-aware — a fresh idle TUI has no session yet, so the goroutine must
// not block the caller waiting for one). agentID is a prior back-written
// SHED_RC_AGENT_SESSION ("" if none): when set it is the trusted pin and no SSE discovery
// is needed. now/logf may be nil (defaulted). The watcher is runner-free — it does no tmux
// I/O; the discovered id is surfaced via drainConfirmedAgentID for reconcile to back-write.
func newOpencodeWatcher(port int, workdir, agentID string, now func() time.Time, logf func(string, ...any)) *opencodeWatcher {
	if now == nil {
		now = time.Now
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	ctx, cancel := context.WithCancel(context.Background())
	w := &opencodeWatcher{
		baseURL:  fmt.Sprintf("http://127.0.0.1:%d", port),
		workdir:  canonicalDir(workdir),
		priorID:  agentID,
		client:   newLoopbackClient(),
		nowFn:    now,
		logf:     logf,
		ctx:      ctx,
		cancel:   cancel,
		done:     make(chan struct{}),
		fold:     newOpencodeFold(),
		pinnedID: agentID, // a prior back-write is the pin; "" means "search the SSE stream"
	}
	go w.run()
	return w
}

// newLoopbackClient builds the HTTP client the watcher uses: loopback-only, env proxy
// DISABLED (a custom Transport whose Proxy stays nil, unlike http.DefaultTransport's
// ProxyFromEnvironment), short dial + response-header timeouts, and NO overall client
// timeout (the SSE GET streams; per-request context.WithTimeout bounds the REST seeds).
func newLoopbackClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy: nil, // disable environment proxy — loopback only
			DialContext: (&net.Dialer{
				Timeout: dialTimeout,
			}).DialContext,
			ResponseHeaderTimeout: headerTimeout,
			MaxIdleConns:          2,
			IdleConnTimeout:       30 * time.Second,
		},
	}
}

// ---- sessionWatcher surface ----

// refresh drains the inbox under the mutex and folds each payload into the (single-writer)
// fold, mirroring fileWatcher.refresh but sourced from the inbox rather than a lineTailer.
// A seedComplete marker flips the transport authoritative (seedApplied); an overflowGap
// marker drops record-exact state (noteGap). A CLOSED watcher no-ops.
func (w *opencodeWatcher) refresh(now time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return
	}
	items := w.inbox
	w.inbox = nil
	w.inboxBytes = 0
	var pending seedFallback // the current-generation seed's status fallback, applied AFTER the batch
	for _, it := range items {
		switch it.kind {
		case inboxPayload:
			if w.fold.applyLine(it.payload) {
				w.lastEventAt = now
			}
		case inboxSeedComplete:
			// The seed + buffered replay is now fully folded: the watcher becomes
			// authoritative (see the seed-complete barrier, §3.6). Reset to false by the
			// goroutine on every disconnect until that connection's own marker passes.
			// A marker from a SUPERSEDED generation is ignored (§3.6, fix #2): connection A's
			// queued seedComplete must never make connection B authoritative before B's own
			// seed completes.
			if it.gen != w.gen {
				break
			}
			w.seedApplied = true
			pending = it.fallback // the REST status fallback is applied LAST, below (barrier order)
		case inboxOverflowGap:
			// A record was LOST (inbox overflow): drop pending tool-call state so a
			// swallowed completion can't pin the verdict at working forever. The dedup set
			// survives (noteGap keeps it) so the forced reseed emits no duplicate rows. A gap
			// from a superseded generation is dropped — the new connection already reseeds and
			// must not have its authority revoked by a stale gap (fix #2 + #7).
			if it.gen != w.gen {
				break
			}
			w.fold.noteGap()
			w.seedApplied = false
		}
	}
	// The REST /session/status fallback is applied AFTER every payload in this batch (message
	// history AND any buffered live events), so a live boundary in the same batch suppresses it —
	// the barrier (seedComplete) is honored LAST and the live stream wins (§3.4, fix #3).
	if pending.set && w.fold.applyStatusFallback(pending.idle) {
		w.lastEventAt = now // the seed established the boundary: count it as an event for freshness
	}
	w.curActivity = w.fold.activity()
	w.curMessage = w.fold.lastMessage()
	w.curSettled = w.fold.settled()
	w.pending = append(w.pending, w.fold.drainMessages()...)
}

// snapshot reports the watcher's verdict + its authority at now. Unlike a durable file
// tail, a network watcher's settled verdict is authoritative ONLY while the transport is
// healthy: the seed is applied, the stream is connected, and a frame (or heartbeat) landed
// within ocFrameStaleWindow. When UNHEALTHY it returns BOTH fresh=false AND
// expiredWorking=false — returning only fresh=false would let mergedActivity keep a stale
// working verdict against a churning pane (watch.go:252-263); forcing expiredWorking=false
// routes to the stability-drives branch (§3.6).
func (w *opencodeWatcher) snapshot(now time.Time) (activity Activity, message string, fresh, expiredWorking bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		// A closed watcher has revoked its authority (close() cleared connected/seedApplied):
		// never report fresh, and force expiredWorking=false so pane-stability drives (fix #6).
		return w.curActivity, w.curMessage, false, false
	}
	if w.curActivity == "" || w.curActivity == ActivityUnknown {
		return w.curActivity, w.curMessage, false, false
	}
	healthy := w.seedApplied && w.connected
	if healthy && !w.lastFrameAt.IsZero() && now.Sub(w.lastFrameAt) >= ocFrameStaleWindow {
		healthy = false // heartbeat-stale: the stream is wedged even if the socket has not errored
	}
	if !healthy {
		// Disconnected / heartbeat-stale / seed-not-yet-applied: hand the verdict to
		// pane-stability (both flags false — see the doc above).
		return w.curActivity, w.curMessage, false, false
	}
	sinceEvent := time.Duration(-1)
	if !w.lastEventAt.IsZero() {
		sinceEvent = now.Sub(w.lastEventAt)
	}
	recent := sinceEvent >= 0 && sinceEvent < watcherFreshWindow
	workingGrace := w.curActivity == ActivityWorking && sinceEvent >= 0 && sinceEvent < watcherWorkingGrace
	fresh = w.curSettled || recent || workingGrace
	expiredWorking = w.curActivity == ActivityWorking && !fresh
	return w.curActivity, w.curMessage, fresh, expiredWorking
}

// drainPending returns and clears the feed messages folded since the last drain (stream
// order); reconcile appends them to the session ring. Mirrors fileWatcher.drainPending.
func (w *opencodeWatcher) drainPending() []feedMessage {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.pending) == 0 {
		return nil
	}
	out := w.pending
	w.pending = nil
	return out
}

// hadEvent reports whether the fold has consumed at least one activity-relevant event
// since attach (used to confirm an ambiguous correlation). Mirrors fileWatcher.hadEvent.
func (w *opencodeWatcher) hadEvent() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return !w.lastEventAt.IsZero()
}

// drainConfirmedAgentID returns and clears the SSE-discovered session id the transport
// pinned ("" when none, already drained, or the pin came from a prior back-write). The id
// is enqueued exactly once (see confirmedOnce), so reconnect+reseed does not re-back-write.
func (w *opencodeWatcher) drainConfirmedAgentID() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	id := w.confirmedID
	w.confirmedID = ""
	return id
}

// close marks the watcher terminally closed, cancels the I/O context, and closes the
// current SSE response body — cancelling the context alone does NOT unblock an in-flight
// Body.Read, so the body must be closed too. It is NON-BLOCKING (reconcile calls it under
// trackMu): it NEVER joins the goroutine. Idempotent; a later refresh/snapshot no-ops.
func (w *opencodeWatcher) close() {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return
	}
	w.closed = true
	// Revoke authority atomically with the close: snapshot must not report fresh after close(),
	// and run()'s deferred setConnected(false) is a belt-and-braces backstop (fix #6).
	w.connected = false
	w.seedApplied = false
	body := w.body
	w.body = nil
	w.mu.Unlock()

	w.cancel()
	if body != nil {
		_ = body.Close()
	}
}

// ---- inbox / health / body plumbing (all under the mutex, never across a network wait) ----

// pushPayload enqueues a raw envelope onto the inbox under the mutex. On overflow (either
// bound exceeded) it enqueues a single overflowGap marker instead and returns false — the
// caller must then force a reconnect+reseed. A closed watcher drops silently (returns false).
func (w *opencodeWatcher) pushPayload(payload []byte) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return false
	}
	if len(w.inbox) >= maxInboxItems || w.inboxBytes+len(payload) > maxInboxBytes {
		w.enqueueOverflowLocked()
		return false
	}
	w.inbox = append(w.inbox, inboxItem{kind: inboxPayload, payload: payload, gen: w.gen})
	w.inboxBytes += len(payload)
	return true
}

// enqueueOverflowLocked records an inbox overflow. It revokes authority IMMEDIATELY under the
// same lock (seedApplied=false, fix #7) so snapshot is non-authoritative before run() even
// observes the forced reconnect — waiting for run()'s later setConnected(false) would leave a
// window where a lossy connection still reports fresh. The overflowGap marker (tagged with the
// current generation, coalesced) drives fold.noteGap() in refresh; markers carry no bytes and
// bypass the byte bound (tiny and bounded in number).
func (w *opencodeWatcher) enqueueOverflowLocked() {
	w.seedApplied = false
	if len(w.inbox) > 0 && w.inbox[len(w.inbox)-1].kind == inboxOverflowGap {
		return // coalesce consecutive overflow markers
	}
	w.inbox = append(w.inbox, inboxItem{kind: inboxOverflowGap, gen: w.gen})
}

// pushSeedComplete enqueues the seed-complete barrier tagged with the current generation and
// carrying the REST status fallback. refresh flips the watcher authoritative only when it folds
// a seedComplete whose generation is still current (fix #2). A closed watcher drops it.
func (w *opencodeWatcher) pushSeedComplete(fb seedFallback) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return
	}
	w.inbox = append(w.inbox, inboxItem{kind: inboxSeedComplete, gen: w.gen, fallback: fb})
}

// beginGeneration advances the connection-generation counter at the start of each connect
// attempt. Any marker still queued from the prior generation is thereby invalidated (§3.6).
func (w *opencodeWatcher) beginGeneration() {
	w.mu.Lock()
	w.gen++
	w.mu.Unlock()
}

// setBackoff / getBackoff expose the reconnect-backoff state (run writes; a test reads it to
// assert exponential growth without depending on wall-clock timing, §3.4).
func (w *opencodeWatcher) setBackoff(d time.Duration) {
	w.mu.Lock()
	w.backoff = d
	w.mu.Unlock()
}

func (w *opencodeWatcher) getBackoff() time.Duration {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.backoff
}

// setConnected records the SSE stream as up/down. On disconnect it also resets seedApplied
// so the watcher is non-authoritative until its NEXT re-seed marker passes (§3.6).
func (w *opencodeWatcher) setConnected(up bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.connected = up
	if !up {
		w.seedApplied = false
	}
}

// markFrame stamps the last-frame time (heartbeats count toward transport freshness).
func (w *opencodeWatcher) markFrame() {
	w.mu.Lock()
	w.lastFrameAt = w.nowFn()
	w.mu.Unlock()
}

// setPinned records the correlated session id. When discovered (SSE-trusted, not a prior
// back-write) its id is enqueued ONCE for drainConfirmedAgentID.
func (w *opencodeWatcher) setPinned(id string, discovered bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.pinnedID = id
	if discovered && !w.confirmedOnce && id != "" && id != w.priorID {
		w.confirmedID = id
		w.confirmedOnce = true
	}
}

func (w *opencodeWatcher) getPinned() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.pinnedID
}

func (w *opencodeWatcher) isClosed() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.closed
}

// registerBody stores the in-flight SSE body so close() can unblock a read. It GUARDS the
// close/Do race: if the watcher was closed while Do was in flight, it returns false and the
// caller closes the body immediately (no post-close body registered, no leak).
func (w *opencodeWatcher) registerBody(body io.Closer) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return false
	}
	w.body = body
	return true
}

// clearBody drops body from the watcher (if it is still the registered one) and closes it.
func (w *opencodeWatcher) clearBody(body io.Closer) {
	w.mu.Lock()
	if w.body == body {
		w.body = nil
	}
	w.mu.Unlock()
	_ = body.Close()
}

// ---- the I/O goroutine: correlation state machine + SSE read + REST seed ----

// run is the single goroutine that owns all HTTP I/O. It loops connect→(pin)→seed→live and,
// on any disconnect/error/overflow, marks disconnected and reconnects with jittered backoff
// (uncorrelated → subscribed → pinned → seeded → live → disconnected → resyncing, §3.3).
// The fold is NEVER mutated here — only the inbox + health flags + confirmed-id slot.
func (w *opencodeWatcher) run() {
	defer close(w.done)
	defer w.setConnected(false) // fix #6: on any exit path, revoke authority (also clears seedApplied)
	backoff := ocBackoffBase
	w.setBackoff(backoff)
	for {
		if w.isClosed() {
			return
		}
		seededOK, err := w.connectAndStream()
		if w.isClosed() {
			return
		}
		if err != nil && !errors.Is(err, errWatcherClosed) {
			w.logf("rc hub: opencode watcher %s stream ended: %v", w.baseURL, err)
		}
		w.setConnected(false)
		backoff = nextReconnectBackoff(backoff, seededOK)
		w.setBackoff(backoff)
		if !w.sleepBackoff(backoff) {
			return // context cancelled (closed)
		}
	}
}

// nextReconnectBackoff advances the reconnect backoff. The floor is restored ONLY after a
// SUCCESSFUL seed (seededOK) — a connection that reached server.connected but then failed its
// REST seed or immediately EOF'd keeps GROWING the backoff, so a server that accepts /event but
// can't be seeded cannot drive 10–20 reconnects/sec (fix #8). Exponential, capped, jitter is
// applied separately in sleepBackoff.
func nextReconnectBackoff(cur time.Duration, seededOK bool) time.Duration {
	if seededOK {
		return ocBackoffBase
	}
	return min(cur*2, ocBackoffMax)
}

// connectAndStream opens /event, runs the correlation state machine, seeds, and folds the
// live stream until it ends/errors. seededOK reports whether a seed reached its barrier this
// connection (a SUCCESSFUL seed — the only thing that resets the reconnect backoff, fix #8);
// merely reaching server.connected does not. Returns errWatcherClosed on close.
func (w *opencodeWatcher) connectAndStream() (seededOK bool, err error) {
	w.beginGeneration() // advance the connection generation: any stale queued marker is invalidated (fix #2)
	req, err := http.NewRequestWithContext(w.ctx, http.MethodGet, w.baseURL+"/event", nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := w.client.Do(req)
	if err != nil {
		return false, err
	}
	// close/Do race: register the returned body, re-checking closed under the mutex. If
	// already closed, close the body immediately and stop (no leaked body, no post-close work).
	if !w.registerBody(resp.Body) {
		_ = resp.Body.Close()
		return false, errWatcherClosed
	}
	defer w.clearBody(resp.Body)

	if resp.StatusCode != http.StatusOK {
		// Non-2xx (401 when a password somehow reached opencode, 5xx, etc.): disconnect +
		// backoff (degrade to stability visibly, never a silent hot-loop).
		return false, fmt.Errorf("GET /event: status %d", resp.StatusCode)
	}

	// markFrame fires on EVERY scanned line (comment heartbeats + empty-data frames included, fix
	// #9) so a stream that only heartbeats still counts as fresh.
	sc := newSSEScanner(resp.Body, w.markFrame)
	pinned := w.getPinned() // priorID or a previously-discovered id survives across reconnects
	candidate := ""         // a REST follow-only candidate, unconfirmed until SSE evidence
	candidateTried := false
	var candidateFallback seedFallback // the follow-only candidate's captured REST status
	seeded := false

	for {
		payload, rerr := sc.next()
		if rerr != nil {
			return seededOK, rerr
		}
		pk := peekEnvelope(payload)

		switch {
		case pk.Type == "server.connected":
			w.setConnected(true)
			switch {
			case pinned != "":
				if serr := w.seedAndBarrier(pinned); serr != nil {
					return seededOK, serr
				}
				seeded = true
				seededOK = true
			case !candidateTried:
				candidateTried = true
				// A follow-only candidate seed failure must NOT be swallowed: incomplete history
				// could later be blindly declared authoritative by an SSE confirm (fix #4). On any
				// seed error, force a reconnect+reseed instead of keeping the candidate.
				cand, fb, cerr := w.establishCandidate()
				if cerr != nil {
					return seededOK, cerr
				}
				candidate = cand
				candidateFallback = fb
			}

		case pinned == "":
			// Searching: pin only from port-local SSE evidence (§3.3).
			if id, ok := w.rootPinFromCreated(pk); ok {
				pinned = id
				w.setPinned(id, true)
				if serr := w.seedAndBarrier(id); serr != nil {
					return seededOK, serr
				}
				seeded = true
				seededOK = true
				break
			}
			if candidate != "" && pk.sessionID() == candidate {
				// The follow-only candidate is now confirmed by a live event on OUR stream. Fold
				// the confirming event FIRST, then the barrier LAST (§3.4 order, fix #3).
				pinned = candidate
				w.setPinned(candidate, true)
				if foldRelevantType(pk.Type) {
					if !w.pushPayload(payload) {
						return seededOK, errInboxOverflow
					}
				}
				w.pushSeedComplete(candidateFallback) // history was already follow-only seeded
				seeded = true
				seededOK = true
			}

		default:
			// Live: filter to the pinned session (drop child/sibling ids) and fold.
			if !seeded {
				// A pinned-but-unseeded state can only happen if the seed failed earlier;
				// re-seed defensively before folding live frames.
				if serr := w.seedAndBarrier(pinned); serr != nil {
					return seededOK, serr
				}
				seeded = true
				seededOK = true
			}
			if sid := pk.sessionID(); sid != "" && sid != pinned {
				break
			}
			if !foldRelevantType(pk.Type) {
				break // server.*, session.created/updated, step-*, etc. — heartbeat/pin only
			}
			if !w.pushPayload(payload) {
				return seededOK, errInboxOverflow // overflow → forced reconnect+reseed
			}
		}

		if w.isClosed() {
			return seededOK, errWatcherClosed
		}
	}
}

// seedAndBarrier seeds the pinned session's history + status/permission/question via REST,
// then enqueues the seed-complete barrier (carrying the REST status FALLBACK) so refresh flips
// the watcher authoritative only once the whole seed is folded (§3.6). Reseed is idempotent
// (the fold's partID/callID dedup survives a reconnect), so it is safe to call on every
// (re)connection. A seed error (including a failed /session/status, fix #5) propagates so the
// caller forces a reconnect+reseed rather than declaring an unestablished boundary authoritative.
func (w *opencodeWatcher) seedAndBarrier(id string) error {
	fb, err := w.seedHistory(id)
	if err != nil {
		return err
	}
	w.pushSeedComplete(fb)
	return nil
}

// seedHistory reconstructs the pinned session's state from REST and pushes it as synthesized
// envelopes: GET /session/{id}/message → message.updated + message.part.updated; GET
// /permission + /question → filtered permission.asked/question.asked status rows. GET
// /session/status is read to establish the activity boundary, but returned as a FALLBACK
// (seedFallback) applied by refresh only if no live boundary was folded — it is NOT synthesized
// as a payload (§3.4, fix #3). Both the message read AND the status read are fatal on error: a
// failed status read means we could not establish the authoritative activity boundary, so the
// seed is declared FAILED (fix #5) → reconnect+reseed. Overflow during seed aborts to a reconnect.
func (w *opencodeWatcher) seedHistory(id string) (seedFallback, error) {
	msgs, err := w.restMessages(id)
	if err != nil {
		return seedFallback{}, err
	}
	for _, m := range msgs {
		if len(m.Info) > 0 {
			if !w.pushSynth(synthEnvelope{Type: "message.updated", Properties: synthProps{SessionID: id, Info: m.Info}}) {
				return seedFallback{}, errInboxOverflow
			}
		}
		for _, part := range m.Parts {
			if !w.pushSynth(synthEnvelope{Type: "message.part.updated", Properties: synthProps{SessionID: id, Part: part}}) {
				return seedFallback{}, errInboxOverflow
			}
		}
	}

	// Status seed: a present-and-complete status map decides idle-vs-busy. Absence of the id in a
	// 200 map == idle (opencode omits idle sessions). A FAILED read (timeout/401/non-2xx) means we
	// could not establish the boundary → the whole seed fails (fix #5). The result is carried as a
	// FALLBACK the barrier applies only if the live stream carried no status/idle (fix #3).
	statusType, present, ok := w.restStatus(id)
	if !ok {
		return seedFallback{}, errStatusSeedFailed
	}
	fb := seedFallback{set: true, idle: !present || statusType == "idle"}

	for _, p := range w.restPermissions(id) {
		if !w.pushSynth(synthEnvelope{Type: "permission.asked", Properties: synthProps{
			SessionID: id, ID: p.ID, Permission: p.Permission, Patterns: p.Patterns,
		}}) {
			return seedFallback{}, errInboxOverflow
		}
	}
	for _, q := range w.restQuestions(id) {
		if !w.pushSynth(synthEnvelope{Type: "question.asked", Properties: synthProps{
			SessionID: id, ID: q.ID, Questions: q.Questions,
		}}) {
			return seedFallback{}, errInboxOverflow
		}
	}
	return fb, nil
}

// establishCandidate consults GET /session for a follow-only candidate — the newest ROOT
// session whose canonical directory matches the workdir — and, if found, seeds its history
// follow-only (feed populated, but NO confirmedID and NO seed-complete barrier, so activity
// stays non-authoritative until a live SSE event on our own stream confirms it, §3.3). GET
// /session is the shared DB and is NOT a trusted pin on its own. It returns the id + the
// captured REST status fallback (applied only when the candidate is later confirmed). A
// seedHistory FAILURE is propagated (fix #4): the caller must force a reconnect+reseed rather
// than keep a partially-seeded candidate that a later SSE frame could blindly declare
// authoritative over incomplete history. Returns ("", {}, nil) when no candidate matches (an
// idle TUI with no session yet stays watchable indefinitely).
func (w *opencodeWatcher) establishCandidate() (string, seedFallback, error) {
	id, ok := w.restFindCandidate()
	if !ok {
		return "", seedFallback{}, nil
	}
	// Follow-only: fold the history (populate the feed ring) but do NOT barrier/confirm.
	fb, err := w.seedHistory(id)
	if err != nil {
		return "", seedFallback{}, err
	}
	return id, fb, nil
}

// rootPinFromCreated returns a trusted pin from a session.created/session.updated frame on
// our own (directory-scoped) stream: the session must be a ROOT (empty parentID) whose
// canonical directory matches (equals, or is an ancestor of) the workdir (§3.3).
func (w *opencodeWatcher) rootPinFromCreated(pk *ocPeek) (string, bool) {
	if pk.Type != "session.created" && pk.Type != "session.updated" {
		return "", false
	}
	info := pk.Properties.Info
	if info.ID == "" || info.ParentID != "" {
		return "", false
	}
	if !dirMatchCanon(info.Directory, w.workdir) {
		return "", false
	}
	return info.ID, true
}

// sleepBackoff waits a jittered duration (full jitter over [d/2, d]) or returns false if the
// context is cancelled (close). No foreground sleep is used elsewhere; this is the only wait.
func (w *opencodeWatcher) sleepBackoff(d time.Duration) bool {
	if d <= 0 {
		d = ocBackoffBase
	}
	half := d / 2
	jittered := half + time.Duration(rand.Int63n(int64(half)+1)) //nolint:gosec // jitter, not security
	t := time.NewTimer(jittered)
	defer t.Stop()
	select {
	case <-w.ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// ---- REST seed reads (all bounded, context-timed, tolerant) ----

// restMessage is one {info, parts} element of GET /session/{id}/message (raw pass-through so
// the exact opencode Message/Part shapes reach the fold unchanged).
type restMessage struct {
	Info  json.RawMessage   `json:"info"`
	Parts []json.RawMessage `json:"parts"`
}

func (w *opencodeWatcher) restMessages(id string) ([]restMessage, error) {
	var out []restMessage
	if err := w.getJSON("/session/"+id+"/message", &out); err != nil {
		return nil, err
	}
	return out, nil
}

// restStatus reads GET /session/status. present reports whether the id is in the map (busy);
// ok reports whether the call succeeded at all (a failed call → don't synthesize anything).
func (w *opencodeWatcher) restStatus(id string) (statusType string, present bool, ok bool) {
	m := map[string]ocStatus{}
	if err := w.getJSON("/session/status", &m); err != nil {
		return "", false, false
	}
	st, present := m[id]
	return st.Type, present, true
}

type restPermission struct {
	ID         string   `json:"id"`
	SessionID  string   `json:"sessionID"`
	Permission string   `json:"permission"`
	Patterns   []string `json:"patterns"`
}

func (w *opencodeWatcher) restPermissions(id string) []restPermission {
	var all []restPermission
	if err := w.getJSON("/permission", &all); err != nil {
		return nil
	}
	var out []restPermission
	for _, p := range all {
		if p.SessionID == id {
			out = append(out, p)
		}
	}
	return out
}

type restQuestion struct {
	ID        string          `json:"id"`
	SessionID string          `json:"sessionID"`
	Questions json.RawMessage `json:"questions"`
}

func (w *opencodeWatcher) restQuestions(id string) []restQuestion {
	var all []restQuestion
	if err := w.getJSON("/question", &all); err != nil {
		return nil
	}
	var out []restQuestion
	for _, q := range all {
		if q.SessionID == id {
			out = append(out, q)
		}
	}
	return out
}

// restFindCandidate picks the newest ROOT session (empty parentID) whose canonical directory
// matches the workdir from GET /session (sorted most-recently-updated first, so the first
// match is newest). ok=false when none matches.
func (w *opencodeWatcher) restFindCandidate() (string, bool) {
	var sessions []struct {
		ID        string `json:"id"`
		Directory string `json:"directory"`
		ParentID  string `json:"parentID"`
	}
	if err := w.getJSON("/session", &sessions); err != nil {
		return "", false
	}
	for _, s := range sessions {
		if s.ID == "" || s.ParentID != "" {
			continue
		}
		if dirMatchCanon(s.Directory, w.workdir) {
			return s.ID, true
		}
	}
	return "", false
}

// getJSON does a bounded, context-timed GET and decodes the body into out. Non-2xx → error.
func (w *opencodeWatcher) getJSON(path string, out any) error {
	ctx, cancel := context.WithTimeout(w.ctx, restTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, w.baseURL+path, nil)
	if err != nil {
		return err
	}
	resp, err := w.client.Do(req)
	if err != nil {
		return err
	}
	// close/Do guard (fix #10): mirror registerBody on the SSE path — if the watcher was closed
	// while this REST request was in flight, close the body and stop. The request context is
	// derived from w.ctx (cancelled by close), so a blocked Do already unblocks; this recheck
	// makes a raced-but-succeeded Do abandon its body immediately rather than fold post-close work.
	if w.isClosed() {
		_ = resp.Body.Close()
		return errWatcherClosed
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: status %d", path, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxRESTBytes))
	if err != nil {
		return err
	}
	return json.Unmarshal(body, out)
}

// ---- synthesized-envelope emission ----

// synthEnvelope / synthProps build a {id,type,properties} frame from REST data that folds
// through the exact same applyLine path as a live SSE frame (so seed history is deduped
// against live events by partID/callID).
type synthEnvelope struct {
	Type       string     `json:"type"`
	Properties synthProps `json:"properties"`
}

type synthProps struct {
	SessionID  string          `json:"sessionID"`
	Info       json.RawMessage `json:"info,omitempty"`
	Part       json.RawMessage `json:"part,omitempty"`
	Status     json.RawMessage `json:"status,omitempty"`
	ID         string          `json:"id,omitempty"`
	Permission string          `json:"permission,omitempty"`
	Patterns   []string        `json:"patterns,omitempty"`
	Questions  json.RawMessage `json:"questions,omitempty"`
}

// pushSynth marshals a synthesized envelope and pushes it onto the inbox. Returns false on
// overflow (the caller aborts the seed to a reconnect). A marshal failure is dropped (true).
func (w *opencodeWatcher) pushSynth(env synthEnvelope) bool {
	raw, err := json.Marshal(env)
	if err != nil {
		return true // an un-marshalable synth is dropped, not an overflow
	}
	return w.pushPayload(raw)
}

// ---- SSE parsing ----

// sseScanner parses an SSE byte stream per the spec: it accumulates data: lines (joined by
// \n), ignores comment lines (leading ':'), ignores the event:/id:/retry: field VALUES (the
// opencode event name is always "message"), and dispatches one frame on a blank line. Lines
// are split on \n with a trailing \r trimmed (CRLF-tolerant). One line and one accumulated
// frame are both size-capped (oversized → errSSEOversize → reconnect). onLine (if set) fires for
// EVERY scanned line — comment heartbeats and empty-data frames included — so transport freshness
// tracks any received traffic, not only frames that yield a payload (fix #9).
type sseScanner struct {
	sc     *bufio.Scanner
	onLine func()
}

func newSSEScanner(r io.Reader, onLine func()) *sseScanner {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 4096), maxSSELineBytes)
	sc.Split(bufio.ScanLines) // ScanLines strips a trailing \r, so CRLF is handled
	return &sseScanner{sc: sc, onLine: onLine}
}

// next returns the JSON payload of the next SSE frame (the accumulated data: fields), or an
// error (io.EOF on a clean end, bufio.ErrTooLong on an oversized line, errSSEOversize on an
// oversized frame, or the body's read error — e.g. after close()).
func (s *sseScanner) next() ([]byte, error) {
	var data []byte
	for {
		if !s.sc.Scan() {
			if err := s.sc.Err(); err != nil {
				return nil, err
			}
			return nil, io.EOF
		}
		if s.onLine != nil {
			s.onLine() // any received line (comment/blank/data) is transport activity (fix #9)
		}
		line := s.sc.Bytes()
		if len(line) == 0 {
			// Blank line → dispatch if we accumulated any data; otherwise keep reading
			// (leading blank lines / keep-alive newlines).
			if len(data) > 0 {
				out := make([]byte, len(data))
				copy(out, data)
				return out, nil
			}
			continue
		}
		if line[0] == ':' {
			continue // comment line
		}
		field, value := splitSSEField(line)
		if field != "data" {
			continue // event:/id:/retry:/unknown — value ignored
		}
		if len(data) > 0 {
			data = append(data, '\n') // multiple data: lines join with \n (SSE spec)
		}
		data = append(data, value...)
		if len(data) > maxSSEFrameBytes {
			return nil, errSSEOversize
		}
	}
}

// splitSSEField splits an SSE line into (field, value): everything before the first ':' is
// the field; the value is what follows, with a single leading space stripped (SSE spec). A
// line with no ':' is a field with an empty value.
func splitSSEField(line []byte) (field, value string) {
	i := bytes.IndexByte(line, ':')
	if i < 0 {
		return string(line), ""
	}
	field = string(line[:i])
	v := line[i+1:]
	if len(v) > 0 && v[0] == ' ' {
		v = v[1:]
	}
	return field, string(v)
}

// ---- envelope peeking + routing predicates ----

// ocPeek is the lightweight decode the transport uses for ROUTING only (pin/filter) — the
// fold does the real parse. It reads the type, the properties.sessionID, and the session
// info fields a pin needs (id/parentID/directory).
type ocPeek struct {
	Type       string `json:"type"`
	Properties struct {
		SessionID string `json:"sessionID"`
		Info      struct {
			ID        string `json:"id"`
			ParentID  string `json:"parentID"`
			Directory string `json:"directory"`
		} `json:"info"`
	} `json:"properties"`
}

func (p *ocPeek) sessionID() string { return p.Properties.SessionID }

// peekEnvelope decodes a payload for routing (never nil; an unparseable payload yields an
// empty-Type peek, which routes as "ignore").
func peekEnvelope(payload []byte) *ocPeek {
	var pk ocPeek
	_ = json.Unmarshal(payload, &pk)
	return &pk
}

// foldRelevantType reports whether an envelope type is one the fold consumes (so only those
// are pushed to the inbox; server.*/session.created/step-* etc. update lastFrameAt for the
// heartbeat/pin logic but never enter the fold).
func foldRelevantType(typ string) bool {
	switch typ {
	case "session.status", "session.idle", "message.updated", "message.part.updated",
		"permission.asked", "question.asked":
		return true
	default:
		return false
	}
}

// ---- path canonicalization for the directory-match pin ----

// canonicalDir resolves symlinks (falling back to a lexical Clean) so both sides of the
// directory match are compared canonically (§3.3: filepath.EvalSymlinks both sides).
func canonicalDir(p string) string {
	if p == "" {
		return ""
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return filepath.Clean(p)
}

// dirMatchCanon reports whether an event's directory matches the (already-canonical) workdir:
// equal, OR the workdir is under the event directory (opencode may report the project root).
func dirMatchCanon(eventDir, canonWorkdir string) bool {
	if eventDir == "" || canonWorkdir == "" {
		return false
	}
	ed := canonicalDir(eventDir)
	if ed == canonWorkdir {
		return true
	}
	rel, err := filepath.Rel(ed, canonWorkdir)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return false
	}
	return true
}
