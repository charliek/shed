package rc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/charliek/shed/internal/version"
)

// The rc hub is the resident, on-demand per-shed daemon that the `shed-ext-rc
// serve` subcommand runs. It exposes a small loopback HTTP API (session list +
// SSE activity stream in this commit; message feed + input in a later one) over
// which the server's rc proxy and the mobile client read live session activity.
// It drives the SAME tmux/pane-stability machinery the one-shot subcommands use
// (List + StabilityTracker), so it lives inside package rc rather than a nested
// package: the reconcile loop needs the unexported capture/list plumbing and the
// tracker, and keeping it here avoids exporting that surface just to feed a
// daemon that is conceptually another consumer of ops.go.
//
// Lifecycle overview (see RunHub / DetachHub / EnsureHub):
//   - `serve` (or `serve --foreground`) binds the port and runs in this process.
//   - `serve --detach` double-forks a `serve --foreground` child (setsid), waits
//     for the port to come up, then exits — so a guest exec that spawns it returns
//     promptly and the daemon survives the exec channel's SIGHUP (see DetachHub).
//   - Binding the port IS the lock: EADDRINUSE plus a verified /v1/health identity
//     handshake means a hub is already running, so the process exits 0 (a foreign
//     listener on the port is an error, not a hub). The pidfile under ~/.shed-rc-hub
//     is advisory/debug only.
//   - The hub self-exits after IdleTimeout with zero rc sessions (subscribers do
//     not block that exit — its SSE is closed and the aggregator re-demands later),
//     with a last-chance re-check + respawn so a create racing the exit is never
//     left unmonitored (see idleExitHandoff).

const (
	// HubPort is the fixed loopback TCP port the rc hub listens on inside a shed.
	// It is registered here beside the cmd/shed-agent vsock port constants
	// (DefaultConsolePort=1024, DefaultNotifyPort=1026, DefaultTCPProxyPort=1028,
	// DefaultHTTPPort=498 — see cmd/shed-agent/server.go) so the guest-side port
	// map lives in two documented places, never as a magic number in rc code.
	// 1029 sits just past the agent's 1028 TCP-proxy port.
	HubPort = 1029

	// HubAddr is the hub's bind/dial address. The bind is 127.0.0.1 ONLY — this is
	// a SECURITY invariant, not a default: the hub is unauthenticated and trusts
	// the loopback (it is reachable only via the server's DialService proxy or an
	// SSH forward). Binding 0.0.0.0 would expose an unauthenticated control surface
	// on a shed's shared bridge. Never widen this to a non-loopback interface.
	HubAddr = "127.0.0.1:1029"

	// hubDirName is the per-user hub state directory basename under $HOME.
	hubDirName = ".shed-rc-hub"
	// hubLogName is the detached daemon's stdout/stderr sink inside hubDirName.
	hubLogName = "hub.log"
	// hubPidName is the advisory/debug pidfile inside hubDirName (NOT the lock —
	// the port bind is the lock).
	hubPidName = "hub.pid"
)

// Hub tuning defaults. All are overridable via HubConfig for tests; production
// uses these.
const (
	// defaultActiveInterval is the reconcile cadence while >=1 SSE subscriber is
	// attached (fast, so activity transitions surface within a couple of seconds).
	defaultActiveInterval = 2 * time.Second
	// defaultIdleInterval is the reconcile cadence with no subscribers (slow — just
	// enough to keep the session list warm and drive idle-exit).
	defaultIdleInterval = 10 * time.Second
	// defaultIdleTimeout is how long the hub tolerates zero rc sessions before it
	// self-exits. Subscribers do not extend this window (see shouldIdleExit).
	defaultIdleTimeout = 15 * time.Minute
	// defaultHeartbeat is the SSE keep-alive comment cadence. Every subscriber gets
	// a `: heartbeat` comment this often so an idle stream stays warm through
	// proxies; the mobile client's pinned reader idles at 120s, well above this.
	defaultHeartbeat = 25 * time.Second
	// defaultWriteTimeout bounds a single SSE frame write so a wedged client can't
	// stall the writer goroutine.
	defaultWriteTimeout = 10 * time.Second
	// defaultSubscriberBuffer is the per-subscriber event queue depth. A slow
	// subscriber whose queue fills has events DROPPED (never blocks the broadcaster)
	// — the egress-stream precedent; SSE is best-effort notification and clients
	// refetch snapshots on reconnect.
	defaultSubscriberBuffer = 256
)

// HubConfig configures a hub. Runner and Getenv are required; everything else is
// optional and falls back to the defaults above (the zero-value durations/ints are
// the "use default" signal), so tests can pin a fast clock, tiny intervals, and a
// throwaway loopback address while production passes only Runner+Getenv.
type HubConfig struct {
	// Runner runs tmux (the same injectable seam as the one-shot subcommands).
	Runner Runner
	// Getenv reads the environment (for $HOME → the hub state dir). Injected for tests.
	Getenv Getenv
	// Now is the clock used for activity timestamps and the idle-exit decision.
	// nil → time.Now.
	Now func() time.Time
	// Logf logs hub diagnostics (to hub.log in production). nil → log.Printf.
	Logf func(format string, args ...any)
	// Addr overrides the bind/dial address. "" → HubAddr (the loopback invariant).
	// Tests set an ephemeral 127.0.0.1:0-style address; production must leave it
	// empty so the loopback-only bind holds.
	Addr string
	// Respawn relaunches a detached hub — the idle-exit handoff hook: after the
	// exiting hub releases its bind, a last-chance session re-check that finds
	// sessions (a create raced the exit) invokes this so the new session is not
	// left unmonitored (see serveOn). nil → the production detach path (DetachHub
	// with this same config); tests inject a recorder.
	Respawn func() error

	// Tuning overrides (zero → the matching default constant).
	ActiveInterval   time.Duration
	IdleInterval     time.Duration
	QuietPeriod      time.Duration
	IdleTimeout      time.Duration
	Heartbeat        time.Duration
	WriteTimeout     time.Duration
	SubscriberBuffer int
}

// hubResolved is HubConfig with every default applied — the form the hub and the
// detach path actually read.
type hubResolved struct {
	runner         Runner
	getenv         Getenv
	now            func() time.Time
	logf           func(format string, args ...any)
	addr           string
	respawn        func() error
	activeInterval time.Duration
	idleInterval   time.Duration
	quiet          time.Duration
	idleTimeout    time.Duration
	heartbeat      time.Duration
	writeTimeout   time.Duration
	subBuffer      int
}

func (c HubConfig) resolve() hubResolved {
	r := hubResolved{
		runner:         c.Runner,
		getenv:         c.Getenv,
		now:            c.Now,
		logf:           c.Logf,
		addr:           c.Addr,
		respawn:        c.Respawn,
		activeInterval: c.ActiveInterval,
		idleInterval:   c.IdleInterval,
		quiet:          c.QuietPeriod,
		idleTimeout:    c.IdleTimeout,
		heartbeat:      c.Heartbeat,
		writeTimeout:   c.WriteTimeout,
		subBuffer:      c.SubscriberBuffer,
	}
	if r.getenv == nil {
		r.getenv = os.Getenv
	}
	if r.now == nil {
		r.now = time.Now
	}
	if r.logf == nil {
		r.logf = log.Printf
	}
	if r.addr == "" {
		r.addr = HubAddr
	}
	if r.respawn == nil {
		// Production respawn = the detach path against this same config. Captured by
		// value so the closure re-resolves defaults consistently.
		cfg := c
		r.respawn = func() error { return DetachHub(cfg) }
	}
	if r.activeInterval <= 0 {
		r.activeInterval = defaultActiveInterval
	}
	if r.idleInterval <= 0 {
		r.idleInterval = defaultIdleInterval
	}
	if r.quiet <= 0 {
		r.quiet = DefaultQuietPeriod
	}
	if r.idleTimeout <= 0 {
		r.idleTimeout = defaultIdleTimeout
	}
	if r.heartbeat <= 0 {
		r.heartbeat = defaultHeartbeat
	}
	if r.writeTimeout <= 0 {
		r.writeTimeout = defaultWriteTimeout
	}
	if r.subBuffer <= 0 {
		r.subBuffer = defaultSubscriberBuffer
	}
	return r
}

// Hub is a running rc hub. Construct with newHub; drive with serveOn (via RunHub).
type Hub struct {
	cfg hubResolved

	// trackMu guards tracked + idleSince (the reconcile state).
	trackMu   sync.Mutex
	tracked   map[string]*trackedSession
	idleSince time.Time

	// subMu guards subs (the SSE fan-out set). Kept separate from trackMu so
	// broadcast (locks subMu) can never deadlock against reconcile (locks trackMu
	// then broadcasts after unlocking).
	subMu sync.Mutex
	subs  map[*subscriber]struct{}

	// inputLockMu guards inputLocks: per-SLUG input-delivery mutexes. Keyed on the
	// hub rather than the trackedSession so a tracked-entry replacement mid-request
	// (a recreate reconciled between a handler's lookup and its lock acquisition)
	// cannot yield two live locks for one pane. Entries are pruned when a session
	// disappears (see reconcile).
	inputLockMu sync.Mutex
	inputLocks  map[string]*sync.Mutex
}

func newHub(cfg HubConfig) *Hub {
	return &Hub{
		cfg:        cfg.resolve(),
		tracked:    map[string]*trackedSession{},
		subs:       map[*subscriber]struct{}{},
		inputLocks: map[string]*sync.Mutex{},
	}
}

// inputLock returns the slug's input-delivery mutex, creating it on first use. The
// same slug always yields the same mutex until the session disappears (pruned), so
// input serialization survives a tracked-entry replacement (kill+recreate keeps the
// slug present → keeps the lock).
func (h *Hub) inputLock(slug string) *sync.Mutex {
	h.inputLockMu.Lock()
	defer h.inputLockMu.Unlock()
	mu, ok := h.inputLocks[slug]
	if !ok {
		mu = &sync.Mutex{}
		h.inputLocks[slug] = mu
	}
	return mu
}

// pruneInputLock drops a disappeared slug's input mutex. A request still holding the
// old mutex finishes against a gone pane (its delivery 404s); a later recreate at the
// same slug gets a fresh lock.
func (h *Hub) pruneInputLock(slug string) {
	h.inputLockMu.Lock()
	defer h.inputLockMu.Unlock()
	delete(h.inputLocks, slug)
}

// handler builds the hub's HTTP routes. Go's method+wildcard ServeMux patterns
// give automatic 405s for a wrong method on a known path and 404s for unknown
// paths, so only the real handlers need explicit status codes.
//
// The last three are the contract-v2 verbs (turn / interrupt / approvals). They are
// routed and fully validated here but implemented by no lane yet — every request ends
// in 409 not_supported. See hub_verbs.go for the precedence rules, the 409 vocabulary,
// and the pinned success shapes.
func (h *Hub) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/health", h.handleHealth)
	mux.HandleFunc("GET /v1/sessions", h.handleSessions)
	mux.HandleFunc("GET /v1/events", h.handleEvents)
	mux.HandleFunc("GET /v1/sessions/{slug}/messages", h.handleMessages)
	mux.HandleFunc("POST /v1/sessions/{slug}/input", h.handleInput)
	mux.HandleFunc("POST /v1/sessions/{slug}/turn", h.handleTurn)
	mux.HandleFunc("POST /v1/sessions/{slug}/interrupt", h.handleInterrupt)
	mux.HandleFunc("POST /v1/sessions/{slug}/approvals/{id}", h.handleApproval)
	return mux
}

// handleSessions returns the live rc session list, reusing the one-shot List
// machinery and overlaying each session's derived activity from its tracker (with
// the lifecycle-trumps-activity precedence applied). last_message stays empty
// until the watcher commit populates it.
func (h *Hub) handleSessions(w http.ResponseWriter, _ *http.Request) {
	sessions := List(h.cfg.runner, nil).RCSessions
	h.trackMu.Lock()
	for i := range sessions {
		tr, ok := h.tracked[sessions[i].Slug]
		if !ok || !tr.sameIdentity(sessions[i]) {
			continue
		}
		// tr.activity already has DisplayActivity applied (see reconcile); an empty
		// value means "suppress the whole activity dimension" so the omitempty DTO
		// fields drop out — matching the wire contract for a gated/dead session.
		if tr.activity != "" {
			sessions[i].Activity = tr.activity
			sessions[i].ActivityAt = tr.activityAt
			sessions[i].LastMessage = tr.lastMessage
		}
		// pending_approvals is a HUB-LAYER overlay (the one-shot List above never
		// sets it): the open-approval snapshot that keeps a session actionable after
		// the feed ring evicted the rows announcing them. Nothing populates
		// tr.pendingApprovals in this phase, so this is always a no-op and the field
		// stays absent on the wire — the seam exists so a lane adapter has exactly
		// one place to publish from. Copied, never aliased: the response row must not
		// share a slice with live hub state. An empty snapshot copies to nil, which
		// omitempty drops — hence no guard.
		sessions[i].PendingApprovals = copyApprovals(tr.pendingApprovals)
	}
	h.trackMu.Unlock()
	writeJSON(w, http.StatusOK, hubSessionsResponse{Sessions: sessions})
}

// copyApprovals deep-copies an approval snapshot for serving. The per-element
// Decisions slice is copied too: a shallow copy of the outer slice would leave
// every served row aliasing the tracked entry's Decisions backing array, so a lane
// mutating its snapshot after this point could rewrite a response already built (or
// race the encoder). An empty snapshot copies to nil, which omitempty drops.
func copyApprovals(in []FeedApproval) []FeedApproval {
	if len(in) == 0 {
		return nil
	}
	out := make([]FeedApproval, len(in))
	for i, a := range in {
		a.Decisions = append([]string(nil), a.Decisions...)
		out[i] = a
	}
	return out
}

// hubSessionsResponse is the GET /v1/sessions body. Distinct from the one-shot
// `list` ListResponse (which also carries capabilities): the hub only serves the
// enriched session array; capability discovery stays on the one-shot path.
type hubSessionsResponse struct {
	Sessions []Session `json:"sessions"`
}

// HubAppID is the identity token GET /v1/health returns in `app`. Port 1029 being
// bound proves only that SOMETHING is listening — the bind-as-lock and detach-probe
// paths (and the server's rc proxy ensure-start) verify this token so a foreign
// process squatting the port is reported as an error instead of being mistaken for
// a running hub. Exported so the server-side probe (internal/api) verifies the
// same constant rather than a duplicate.
const HubAppID = "shed-rc-hub"

// hubHealth is the GET /v1/health payload — the hub's identity handshake.
type hubHealth struct {
	App     string `json:"app"`
	Version string `json:"version"`
	PID     int    `json:"pid"`
}

// handleHealth answers the identity probe (see HubAppID).
func (h *Hub) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, hubHealth{App: HubAppID, Version: version.Info(), PID: os.Getpid()})
}

// handleMessages serves GET /v1/sessions/{slug}/messages: a page of the session's feed
// ring after the exclusive `since` seq (default from the start), bounded by `limit`
// (≤200, default 100). truncated=true when `since` predates the ring (drop-oldest
// discarded messages the client hasn't seen) or points beyond the current tail (the
// ring restarted — refetch). 404 for an unknown slug, 400 for a malformed
// `since`/`limit`.
//
// POLICY (intended asymmetry with DisplayActivity): message history REMAINS readable
// while a blocking lifecycle state (needs-trust/needs-auth/dead) gates the activity
// dimension and input posting. The ring holds pre-gate content the operator already
// saw on the pane, this is a loopback-only surface, and the server-side proxy is the
// authorization boundary — suppressing history here would only hide context a client
// needs to render the "session died mid-conversation" view.
func (h *Hub) handleMessages(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")

	var since uint64
	if raw := r.URL.Query().Get("since"); raw != "" {
		v, err := strconv.ParseUint(raw, 10, 64)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_since", "since must be a non-negative integer")
			return
		}
		since = v
	}
	limit := defaultMessagesLimit
	if raw := r.URL.Query().Get("limit"); raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil || v < 0 {
			writeError(w, http.StatusBadRequest, "invalid_limit", "limit must be a non-negative integer")
			return
		}
		limit = v
	}

	h.trackMu.Lock()
	tr, ok := h.tracked[slug]
	var ring *messageRing
	if ok {
		ring = tr.ring
	}
	h.trackMu.Unlock()
	if !ok {
		writeError(w, http.StatusNotFound, "unknown_slug", "no such rc session")
		return
	}

	msgs, truncated := ring.since(since, limit)
	if msgs == nil {
		msgs = []feedMessage{} // encode [] not null for an empty page
	}
	writeJSON(w, http.StatusOK, hubMessagesResponse{Messages: msgs, Truncated: truncated})
}

// inputRequest is the POST /v1/sessions/{slug}/input body.
type inputRequest struct {
	Text string `json:"text"`
}

// handleInput serves POST /v1/sessions/{slug}/input: validate + re-derive live state
// under the per-session mutex, then deliver the text through the bracketed-paste path.
// Statuses: 400 invalid/unsafe text, 404 unknown/gone slug, 409 not accepting (wrong
// activity, recreated identity, or a non-input-gated kind), 413 body too large.
func (h *Hub) handleInput(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")

	// decodeHubBody (hub_verbs.go) bounds the body BEFORE decoding, so an oversized
	// payload is a 413 rather than an OOM, under the one cap every hub POST shares.
	var req inputRequest
	if !decodeHubBody(w, r, &req) {
		return
	}

	text := NormalizeNewlines(req.Text)
	if trimFeedText(text) == "" {
		writeError(w, http.StatusBadRequest, "empty_text", "text is required")
		return
	}
	if HasUnsafePromptChars(text) {
		writeError(w, http.StatusBadRequest, "unsafe_text", "text contains an unsupported control character")
		return
	}

	// Look up the tracked session and snapshot the identity the re-check pins against.
	// The read runs under trackMu — reconcile mutates tracked under the same lock.
	h.trackMu.Lock()
	tr, ok := h.tracked[slug]
	var wantID, wantCreatedAt string
	if ok {
		wantID, wantCreatedAt = tr.id, tr.createdAt
	}
	h.trackMu.Unlock()
	if !ok {
		writeError(w, http.StatusNotFound, "unknown_slug", "no such rc session")
		return
	}

	// Per-SLUG mutex (hub-keyed, not on the tracked entry): the acceptance re-check +
	// delivery are one critical section, and the same slug maps to the same mutex even
	// if reconcile replaces the tracked entry between our lookup and this lock — two
	// concurrent posts can never interleave keystrokes into one pane.
	mu := h.inputLock(slug)
	mu.Lock()
	defer mu.Unlock()

	name := TmuxName(slug)
	pane, err := capturePaneChecked(h.cfg.runner, name)
	if err != nil {
		if errors.Is(err, ErrSessionNotFound) {
			// The session vanished between the lookup and this re-capture.
			writeError(w, http.StatusNotFound, "unknown_slug", "rc session is gone")
			return
		}
		// A transient tmux failure is not evidence the session is gone — surface it as
		// a server error so the client retries rather than dropping the session.
		writeError(w, http.StatusInternalServerError, "capture_failed", "pane re-capture failed")
		return
	}
	fresh := ParseSession(name, showEnvironment(h.cfg.runner, name), pane, nil)

	// Identity guard: the slug must still be the same incarnation we looked up.
	if fresh.ID != wantID || fresh.CreatedAt != wantCreatedAt {
		writeError(w, http.StatusConflict, "not_accepting", "session was recreated")
		return
	}
	// Feed input is codex- and opencode-only in this phase (kind_features.input ==
	// "gated").
	if !inputGatedKind(fresh.Kind) {
		writeError(w, http.StatusConflict, "not_accepting", "this kind does not accept feed input")
		return
	}
	// A blocking lifecycle state suppresses the activity dimension entirely — nothing
	// is accepting typed input.
	if DisplayActivity(fresh.State, ActivityWorking) == "" {
		writeError(w, http.StatusConflict, "not_accepting", "session is not in an input-accepting state")
		return
	}

	// Re-read the CURRENT watcher + stability under trackMu (they may have been
	// replaced since the pre-lock lookup; identity was just re-verified above).
	h.trackMu.Lock()
	var (
		watcher   sessionWatcher
		stability Activity
	)
	if cur, ok := h.tracked[slug]; ok {
		watcher, stability = cur.watcher, cur.lastStability
	}
	h.trackMu.Unlock()

	// Acceptance→delivery gap: the per-slug mutex serializes concurrent POSTs, but
	// reconcile or the agent can still flip the session to working between the pane
	// capture above (used for identity/state) and delivery. Re-capture the pane HERE,
	// as late as possible, and run the acceptance merge on THAT fresh pane so the gate
	// reflects the pane immediately before sendLine — a session that resumed working
	// no longer shows the composer anchor and is rejected. Residual (accepted): the
	// few syscalls between this capture and sendLine below remain un-gated (tmux offers
	// no atomic capture-and-send), so a flip landing in that sliver can still deliver
	// mid-turn; the window is now a couple of calls rather than the whole handler body.
	deliverPane, err := capturePaneChecked(h.cfg.runner, name)
	if err != nil {
		if errors.Is(err, ErrSessionNotFound) {
			writeError(w, http.StatusNotFound, "unknown_slug", "rc session is gone")
			return
		}
		writeError(w, http.StatusInternalServerError, "capture_failed", "pane re-capture failed")
		return
	}
	if !h.inputAccepted(watcher, stability, fresh.Kind, deliverPane) {
		writeError(w, http.StatusConflict, "not_accepting", "session is not waiting for input")
		return
	}

	// Deliver via the shared bracketed-paste path (single line → send-keys -l + Enter;
	// multi-line → set-buffer + paste-buffer + Enter). AcceptsTypedInput holds for the
	// gated kinds.
	if res := sendLine(h.cfg.runner, name, text); res.Code != 0 {
		if isMissingSession(res.Stderr) {
			writeError(w, http.StatusNotFound, "unknown_slug", "rc session is gone")
			return
		}
		writeError(w, http.StatusInternalServerError, "delivery_failed", "input delivery failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"delivered": true})
}

// inputAccepted re-derives, from a FRESH pane capture, whether the session is waiting
// for typed input — running the SAME watcher+stability merge the reconcile loop uses
// (mergedActivity, working-grace included), so the handler can never be more
// permissive than the hub's own displayed activity:
//
//   - merged working → reject. This covers a fresh working verdict AND the
//     expired-working case (a >120s-quiet tool call whose stability is not settled) —
//     delivering mid-turn would interleave input into an active turn.
//   - a FRESH watcher needs_input → accept outright (the structured signal is settled
//     and authoritative; the pane may legitimately not match the anchor).
//   - anything else (merged idle/unknown, or a stability-derived needs_input whose
//     evidence is a previous tick's pane) → the degraded-path policy: accept only if
//     the kind's prompt anchor is visible on the FRESH pane. Requiring the fresh
//     anchor here is what closes the lookup→lock race — a pane that flipped back to
//     churning no longer shows the composer and is rejected.
func (h *Hub) inputAccepted(watcher sessionWatcher, stability Activity, kind Kind, pane string) bool {
	var (
		watcherAct                   Activity
		watcherFresh, expiredWorking bool
	)
	if watcher != nil {
		watcher.refresh(h.cfg.now())
		watcherAct, _, watcherFresh, expiredWorking = watcher.snapshot(h.cfg.now())
	}
	merged, _ := mergedActivity(watcherAct, "", watcherFresh, expiredWorking, stability)
	if merged == ActivityWorking {
		return false
	}
	if watcherFresh && watcherAct == ActivityNeedsInput {
		return true
	}
	anchor := promptAnchorFor(kind)
	return anchor != nil && anchor.MatchString(pane)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError writes the hub's JSON error envelope {error, message}. The wire
// contract's status codes (400 invalid, 404 unknown slug, 409 not accepting, 413
// too large) are produced by the specific handlers; this is the shared shape.
func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]string{"error": code, "message": msg})
}

// RunHub binds the hub port and serves in the foreground until it idle-exits or is
// signaled. Binding the port is the lock: if it is already in use AND the holder
// answers the /v1/health identity handshake as a hub, this returns nil (exit 0)
// without serving — a redundant ensure/start. A foreign process squatting the port
// is an error (non-zero exit), not a silent success. This is what `serve` /
// `serve --foreground` run, and what the detached child from DetachHub runs.
func RunHub(cfg HubConfig) error {
	h := newHub(cfg)
	ln, already, err := bindHubListener(h.cfg.addr)
	if err != nil {
		return fmt.Errorf("rc hub: binding %s: %w", h.cfg.addr, err)
	}
	if already {
		// Bind-as-lock, identity-verified: the port being held only proves SOMETHING
		// listens there. Confirm it is actually a hub before calling this a success.
		if isHub, herr := queryHubHealth(h.cfg.addr, time.Second); herr == nil && isHub {
			h.cfg.logf("rc hub: %s already in use; another hub is running", h.cfg.addr)
			return nil
		}
		return fmt.Errorf("rc hub: port %s is held by another process that is not a shed rc hub", h.cfg.addr)
	}
	defer ln.Close()

	// Advisory/debug pidfile — the daemon records its own pid. NOT the lock (a stale
	// pidfile is harmless: the port bind above already decided ownership).
	if dir := hubDir(h.cfg.getenv); dir != "" {
		if mkErr := os.MkdirAll(dir, 0o700); mkErr == nil {
			_ = writePidfile(dir)
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return h.serveOn(ctx, ln)
}

// serveOn runs the HTTP server on ln plus the reconcile loop, returning when the
// context is canceled (signal), the server errors, or the hub idle-exits. The loop
// cadence is fast (activeInterval) while any SSE subscriber is attached, slow
// (idleInterval) otherwise.
func (h *Hub) serveOn(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{
		Handler: h.handler(),
		// Bound how long a (possibly slow/hostile) client may hold header/body reads
		// open — MaxBytesReader only caps SIZE, not the time to dribble it in, so
		// without these a Slowloris-style client could pin connections. Deliberately
		// NO global WriteTimeout: the /v1/events SSE response is long-lived and paces
		// its own writes with a per-frame SetWriteDeadline (see writeSSE); a global
		// WriteTimeout would kill the stream mid-flight.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()

	// A best-effort fsnotify layer over the codex + claude JSONL trees nudges the loop
	// to reconcile sub-tick when a watched transcript is appended, so an activity
	// transition surfaces promptly instead of waiting for the next tick. If it cannot
	// start (fsnotify unavailable), the tick alone drives — correctness is unchanged.
	nudge := h.startFSNudger(ctx)

	h.reconcile() // seed the session list + fire appear events before the first tick

	for {
		interval := h.cfg.idleInterval
		if h.subscriberCount() > 0 {
			interval = h.cfg.activeInterval
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return h.shutdown(srv)
		case err := <-errCh:
			timer.Stop()
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				return err
			}
			return nil
		case <-nudge:
			timer.Stop()
			h.reconcile() // a watched file changed — surface it now, not next tick
		case <-timer.C:
			h.reconcile()
			if h.shouldIdleExit(h.cfg.now()) {
				// Idle exit: zero rc sessions for the idle window. Subscribers do NOT
				// block this — close their SSE streams and shut down; the aggregator
				// sees the hub go away and re-demands a start when sessions reappear.
				h.cfg.logf("rc hub: idle for %s with zero rc sessions; exiting", h.cfg.idleTimeout)
				h.idleExitHandoff(ln)
				return h.shutdown(srv)
			}
		}
	}
}

// idleExitHandoff closes the create-races-exit window: between the zero-session
// check above and process exit, a `create` can add a tmux session and its
// EnsureHub probe can hit THIS dying hub's health endpoint (the bind is still
// held), decide a hub is running, and skip its own spawn — leaving the new session
// unmonitored. The handoff releases the bind FIRST, then takes one final look at
// the session list; because create makes the tmux session BEFORE its ensure-hub
// probe, any create whose probe this hub answered is visible in that re-check, and
// any create probing after the close finds the port free and spawns its own hub.
// Sessions found → respawn a fresh hub via the detach path (worst case a harmless
// extra respawn) and log it.
func (h *Hub) idleExitHandoff(ln net.Listener) {
	_ = ln.Close() // release the bind so a respawned hub can take the port
	if names := listSessionNames(h.cfg.runner); len(names) > 0 {
		h.cfg.logf("rc hub: %d rc session(s) appeared during idle exit; respawning hub", len(names))
		if err := h.cfg.respawn(); err != nil {
			h.cfg.logf("rc hub: idle-exit respawn failed: %v", err)
		}
	}
}

// startFSNudger starts the best-effort fsnotify layer over the codex + claude JSONL
// roots and returns the channel it nudges on a watched-file change. A nil channel
// (fsnotify unavailable / HOME unset) is a valid select arm that simply never fires,
// leaving the reconcile tick as the sole driver. The nudger goroutine stops with ctx.
func (h *Hub) startFSNudger(ctx context.Context) <-chan struct{} {
	var roots []string
	if r := codexSessionsRoot(h.cfg.getenv); r != "" {
		roots = append(roots, r)
	}
	if r := claudeProjectsRoot(h.cfg.getenv); r != "" {
		roots = append(roots, r)
	}
	if len(roots) == 0 {
		return nil
	}
	n, err := newFSNudger(roots, h.cfg.logf)
	if err != nil {
		h.cfg.logf("rc hub: fsnotify unavailable (%v); tick-only activity", err)
		return nil
	}
	go n.run(ctx)
	return n.nudge
}

// shutdown closes all SSE subscribers + session watchers (codex/claude JSONL tails,
// opencode SSE clients) and gracefully stops the HTTP server.
func (h *Hub) shutdown(srv *http.Server) error {
	h.closeAllSubscribers()
	h.closeAllWatchers()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
	return nil
}

// closeAllWatchers releases every tracked session's watcher — a JSONL tail (codex/
// claude) or an opencode SSE client (hub shutdown).
func (h *Hub) closeAllWatchers() {
	h.trackMu.Lock()
	defer h.trackMu.Unlock()
	for _, tr := range h.tracked {
		if tr.watcher != nil {
			tr.watcher.close()
			tr.watcher = nil
		}
	}
}

// shouldIdleExit reports whether the hub has held zero rc sessions for at least the
// idle timeout. Subscribers deliberately do not extend the window (the contract:
// an all-sessions-killed hub exits even with the aggregator still attached).
func (h *Hub) shouldIdleExit(now time.Time) bool {
	h.trackMu.Lock()
	defer h.trackMu.Unlock()
	return !h.idleSince.IsZero() && now.Sub(h.idleSince) >= h.cfg.idleTimeout
}

// bindHubListener listens on addr, reporting already=true (not an error) when the
// address is already in use — the bind-as-lock signal that a hub is running.
func bindHubListener(addr string) (ln net.Listener, already bool, err error) {
	l, err := net.Listen("tcp", addr)
	if err != nil {
		if errors.Is(err, syscall.EADDRINUSE) {
			return nil, true, nil
		}
		return nil, false, err
	}
	return l, false, nil
}

// hubDir returns $HOME/.shed-rc-hub (or "" when HOME is unset).
func hubDir(getenv Getenv) string {
	home := getenv("HOME")
	if home == "" {
		return ""
	}
	return filepath.Join(home, hubDirName)
}

// writePidfile writes the current pid to dir/hub.pid (advisory/debug only).
func writePidfile(dir string) error {
	return os.WriteFile(filepath.Join(dir, hubPidName),
		[]byte(strconv.Itoa(os.Getpid())+"\n"), 0o600)
}

// queryHubHealth performs ONE identity check against addr's /v1/health:
//
//	(true, nil)  — a live hub answered with app == HubAppID;
//	(false, nil) — SOMETHING is listening but it is not a hub (foreign process);
//	(false, err) — nothing answered at all (connection refused / timeout).
//
// The raw TCP dial exists only to split "nothing listening" from "listening but
// not a hub" — a mere successful dial is never treated as a hub (the identity
// comes from the HTTP handshake, not the port being open).
func queryHubHealth(addr string, timeout time.Duration) (bool, error) {
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return false, err
	}
	_ = conn.Close()
	client := &http.Client{Timeout: timeout}
	resp, err := client.Get("http://" + addr + "/v1/health")
	if err != nil {
		return false, nil // listening, but not speaking our HTTP → not a hub
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, nil
	}
	var hh hubHealth
	if json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&hh) != nil {
		return false, nil
	}
	return hh.App == HubAppID, nil
}

// probeHubIdentity polls addr until a VERIFIED hub answers /v1/health or the budget
// elapses. Used by the detach parent to confirm the daemon (or a pre-existing hub)
// is up before it exits. A foreign listener fails fast with a clear error — it will
// never become a hub, and the spawned child is about to exit non-zero on the same
// identity check.
func probeHubIdentity(addr string, budget time.Duration) error {
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		isHub, err := queryHubHealth(addr, 500*time.Millisecond)
		if err == nil {
			if isHub {
				return nil
			}
			return fmt.Errorf("port %s is held by another process that is not a shed rc hub", addr)
		}
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("rc hub did not come up on %s within the probe budget", addr)
}

// EnsureHub best-effort ensures a hub is running for this host/shed, spawning the
// detached daemon if needed. It NEVER fails its caller (create) — a spawn failure
// is logged to stderr and swallowed. This is the single helper the create path
// and (later) the server proxy call.
func EnsureHub(cfg HubConfig, stderr io.Writer) {
	if err := DetachHub(cfg); err != nil && stderr != nil {
		fmt.Fprintf(stderr, "shed-ext-rc: rc hub ensure failed (best-effort): %v\n", err)
	}
}

// DetachHub double-forks a detached `serve --foreground` daemon and waits (bounded)
// for the port to come up, then returns. The child is put in its OWN session via
// SysProcAttr.Setsid, which is the load-bearing detail: cmd/shed-agent/exec.go
// SIGHUPs the exec channel's process GROUP on host disconnect (see onHostDisconnect
// / terminateGroup there), so a child left in that group would be killed the moment
// the spawning guest exec returns. Setsid moves the daemon out of that group so it
// survives — the same trick tmux/nohup use to intentionally outlive a disconnect.
// Stdio is redirected to ~/.shed-rc-hub/hub.log so the daemon has no live channel
// to write back to.
func DetachHub(cfg HubConfig) error {
	res := cfg.resolve()

	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolving own executable: %w", err)
	}

	dir := hubDir(res.getenv)
	if dir == "" {
		return errors.New("HOME unset; cannot place hub log")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}
	logFile, err := os.OpenFile(filepath.Join(dir, hubLogName),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("opening hub log: %w", err)
	}
	defer logFile.Close()

	cmd := exec.Command(exe, "serve", "--foreground")
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if devnull, derr := os.Open(os.DevNull); derr == nil {
		cmd.Stdin = devnull
		defer devnull.Close()
	}
	// New session (setsid) → detached from the exec channel's process group; see
	// the doc comment above.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting detached hub: %w", err)
	}
	// Don't Wait — release the child so it is reparented to init and this process
	// can exit without leaving a zombie. The daemon (RunHub) writes its own pidfile.
	_ = cmd.Process.Release()

	// Confirm a VERIFIED hub answers before returning, so the caller knows the hub
	// is reachable and really is a hub (not a foreign process squatting the port —
	// that case fails with a clear error). A hub already running (child hits
	// EADDRINUSE, verifies identity, exits 0) still satisfies the probe: the
	// existing hub answers the handshake.
	return probeHubIdentity(res.addr, 3*time.Second)
}
