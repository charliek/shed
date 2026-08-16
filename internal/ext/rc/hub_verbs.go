package rc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
)

// Contract-v2 hub verbs: turn, interrupt, and approval resolution.
//
//	POST /v1/sessions/{slug}/turn            {"text": string, "options": object?}
//	POST /v1/sessions/{slug}/interrupt       (body ignored)
//	POST /v1/sessions/{slug}/approvals/{id}  {"decision": "allow"|"allow_always"|"deny"}
//
// The three routes were specified (and fully validated) before any lane implemented
// them, so clients — mobile above all — could be written against a stable surface. The
// OPENCODE lane implements all three now; every other kind still validates fully and
// then rejects with 409 not_supported, because its kind_features row advertises no verb
// (input == "turn" / interrupt == true / approvals == "remote"). The success shapes are
// pinned by the response types below and their schema test, so no lane can recontract
// them.
//
// LANE DISPATCH: a verb whose capability check passes type-asserts the session's tracked
// watcher against the narrow interface for that verb (turnStarter / turnInterrupter /
// approvalResolver). The watcher pointer is copied out under trackMu (verbTarget) —
// reconcile commits that field under the same lock — and the lane call itself runs
// UNLOCKED, on the request's context. A watcher that does not implement the asserted
// interface (or a session that has none yet) falls through to the noLaneMsg 409.
//
// 409 VOCABULARY (the one place it is defined; mirrored in
// docs/extensions/rc-helper.md):
//
//   - not_supported — this session's kind/lane never supports the verb. Capabilities
//     said so; retrying, or waiting, changes nothing. Every kind returns this today.
//   - not_accepting — the verb IS supported but not right now (the existing POST
//     /input vocabulary: wrong activity, recreated identity; and later a turn while a
//     turn is running, or an interrupt with no active turn). Retryable in principle.
//
// There are deliberately no 501s: one envelope ({error, message}) and one vocabulary
// for every rejection. A client that must distinguish "this server is too old to have
// the route at all" reads the `contract-v2` capability feature token rather than
// interpreting the mux's bare 404.
//
// HANDLER PRECEDENCE (identical across the three verbs, and to handleInput's
// precedent): body size (413) → body validation (400) → path-value validation (400)
// → tracked-session lookup (404) → capability check (409). Validating before the
// lookup keeps a malformed request's answer independent of which sessions happen to
// exist, and keeps the 404 free of information about bodies the hub never accepted.
//
// The verb handlers deliberately take NO input mutex and capture NO pane: they deliver
// through the lane's own protocol, not the pane, so the serialization the typed-input
// path needs has no counterpart here. The tracked-lookup rule (404 for an unknown slug,
// no re-derivation from tmux) matches handleMessages.

const (
	// hubMaxBodyBytes caps every POST body the hub accepts (413 past it). 16 KiB is
	// the input handler's long-standing cap, reused here so one number governs the
	// whole surface — and mirrored by the server proxy's blanket cap on proxied POST
	// bodies (internal/api/rchub.go), so a body rejected here is usually rejected a
	// hop earlier.
	//
	// KNOWN LIMIT (revisit when a structured lane lands): 16 KiB may be small for a
	// structured-lane turn carrying pasted context. Raising it is a contract change
	// on both this handler and the proxy cap, so it is a deliberate decision rather
	// than a quiet bump.
	hubMaxBodyBytes = 16 << 10

	// inputModeTurn is the kind_features.input value denoting a lane that accepts
	// whole TURNS (the POST /turn verb) rather than the pane-gated line delivery
	// "gated" describes. It is the predicate the turn handler tests; opencode is the
	// first kind to carry it, which is exactly what lights the verb up for that kind.
	inputModeTurn = "turn"
	// inputModeGated is the kind_features.input value denoting pane-gated line
	// delivery (POST /input accepted only while the session is waiting). The single
	// input field is one-of, so a kind that moves to "turn" LEAVES "gated" behind —
	// the gated feed-input surface is derived from this value alone (see handleInput),
	// never from a second hardcoded kind list.
	inputModeGated = "gated"
	// approvalsRemote is the kind_features.approvals value denoting a lane whose
	// approvals are answered THROUGH the hub (the POST /approvals/{id} verb) rather
	// than on the pane ("tui"). It is the predicate the approval handler tests.
	approvalsRemote = "remote"
)

// The three LANE interfaces the verb handlers type-assert a session's watcher against
// (the confirmedAgentIDDrainer/approvalPublisher precedent: narrow, one capability
// each, so a watcher opts into exactly the verbs its protocol can serve). All three
// take the CALLER's context and are called with NO hub lock held; each bounds its own
// upstream call. Errors are opaque to the handler beyond being mapped to 409
// not_accepting — a verb never invents a new status code for an upstream failure.
type (
	// turnStarter delivers a whole turn and returns the lane's opaque turn handle.
	turnStarter interface {
		startTurn(ctx context.Context, text string) (turnID string, err error)
	}
	// turnInterrupter asks the lane to abort the running turn. Success means the
	// interrupt was DELIVERED, not that the turn has stopped.
	turnInterrupter interface {
		interruptTurn(ctx context.Context) error
	}
	// approvalResolver answers an approval through the lane's own protocol. The four
	// bookkeeping methods make the otherwise check-then-act flow atomic:
	// approvalState is the fold-backed oracle for the 404/replay/conflict decision (NOT
	// pending_approvals, which is pending-only by wire contract); claimApproval takes
	// exclusive ownership of a pending id so two concurrent requests cannot both POST;
	// releaseApproval hands it back when the upstream write fails; commitApproval
	// records the resolution synchronously — so a later replay sees it resolved without
	// a second POST — and reports the decision the lane ACTUALLY holds.
	approvalResolver interface {
		approvalState(id string) (status, decision string, ok bool)
		claimApproval(id, decision string) approvalClaim
		releaseApproval(id string)
		commitApproval(id, decision string) (recorded string)
		resolveApproval(ctx context.Context, id, decision string) error
	}
)

// Hub error codes carried in the {error, message} envelope. The rejection codes the
// handlers below emit today, plus the codes the pinned success semantics reserve for
// the lane implementations — declared here so the vocabulary lives in one place and
// a later implementation reuses the exact token this contract advertises.
const (
	// errNotSupported / errNotAccepting — 409, per the vocabulary above.
	errNotSupported = "not_supported"
	errNotAccepting = "not_accepting"
	// errAlreadyResolved — 409 (RESERVED): a DIFFERENT decision was posted for an
	// approval that is already resolved. Replaying the SAME decision is idempotent
	// and answers 200.
	errAlreadyResolved = "already_resolved"
	// errUnknownApproval — 404 (RESERVED): the slug is known but carries no approval
	// with this (syntactically valid) id.
	errUnknownApproval = "unknown_approval"
)

// noLaneMsg is the rejection a verb falls to when the capability check passed — some
// kind_features row claims the verb — but the session has no watcher implementing that
// verb's lane interface. Genuinely reachable now: an opencode session whose watcher has
// not been built yet (no recorded port, a blocking lifecycle state, or the tick before
// reconcile creates it) answers here. A retryable 409 rather than a success with no
// effect is the whole point: the row promises the verb, this session cannot serve it
// YET.
const noLaneMsg = "no lane is attached to this session"

// ApprovalIDRe is the CONTRACT grammar for an approval id — a deliberate design
// decision, not an inherited slug regex:
//
//	^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$
//
// It must start alphanumeric (so ".", ".." and "..." can never match — path traversal
// is excluded by the grammar itself, not by a downstream cleaner), allows the dot,
// colon, underscore and dash that appear in the native ids lanes will carry (codex
// call ids, ACP/opencode request ids), and caps the whole id at 128 characters (the
// same ceiling maxApprovalTokenBytes enforces on the ring side).
//
// Exported because it is shared, not package-private: the server-side proxy path
// classifier (classifyRCProxyPath in internal/api/rchub.go) mirrors this EXACT
// expression via this symbol so a malformed id is rejected before the proxy dials
// the guest — the two must be kept in lockstep; {slug} keeps its own rcSlugRe there.
var ApprovalIDRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

// turnRequest is the POST /v1/sessions/{slug}/turn body. Unknown fields are ignored
// (the input handler's precedent; Content-Type is not enforced either).
type turnRequest struct {
	// Text is the turn's prompt. Required — empty/whitespace is a 400.
	Text string `json:"text"`
	// Options is RESERVED: a lane-specific option bag (model, permission mode,
	// …). Accepted and IGNORED in this phase; it is decoded rather than dropped so
	// a client can start sending it and a lane can start reading it without a
	// wire change.
	Options map[string]any `json:"options,omitempty"`
}

// approvalRequest is the POST /v1/sessions/{slug}/approvals/{id} body.
type approvalRequest struct {
	// Decision is one of allow | allow_always | deny (the fixed approval
	// vocabulary; see hub_messages.go). Anything else is a 400.
	Decision string `json:"decision"`
}

// The verb SUCCESS bodies, pinned by these types and by the round-trip schema test so
// every lane answers the shape clients were written against:
//
//	turn       → 202 {"turn_id": "<opaque>"}
//	interrupt  → 202 {"interrupting": true}
//	approvals  → 200 {"resolved": true, "decision": "<decision>"}
//	                                            ; same decision replayed → 200 (idempotent)
//	                                            ; different decision     → 409 already_resolved
//	                                            ; unknown id             → 404 unknown_approval
//
// The "busy → 409 not_accepting" / "no active turn → 409 not_accepting" rejections R0
// sketched here are LANE-DEFINED, not contract-mandated: the codes stay reserved for a
// lane whose native surface refuses the verb in that state, and a lane whose surface
// accepts it simply never emits them. The OPENCODE lane defines NEITHER — opencode
// natively queues/steers typed input while a turn runs, and answers an abort on an idle
// session successfully — so it forwards both verbs regardless of the merged activity.
// A client must therefore not treat a 409 as the way it learns a session is busy;
// activity is the signal for that.
type turnResponse struct {
	// TurnID is an OPAQUE, lane-assigned handle for the accepted turn. Clients must
	// not parse it; it exists so a later status/cancel surface can address the turn.
	TurnID string `json:"turn_id"`
}

// interruptResponse acknowledges that an interrupt was DELIVERED, not that the turn
// has stopped — the stop itself surfaces on the feed/activity stream.
type interruptResponse struct {
	Interrupting bool `json:"interrupting"`
}

// approvalResponse reports the approval as resolved and echoes the decision that
// resolved it (which, on an idempotent replay, is the decision already recorded).
type approvalResponse struct {
	Resolved bool   `json:"resolved"`
	Decision string `json:"decision"`
}

// validApprovalDecision reports whether d is one of the three contract decisions.
func validApprovalDecision(d string) bool {
	switch d {
	case approvalDecisionAllow, approvalDecisionAllowAlways, approvalDecisionDeny:
		return true
	}
	return false
}

// handleTurn serves POST /v1/sessions/{slug}/turn. See the precedence + 409 vocabulary
// notes at the top of this file. A kind whose row does not advertise
// kind_features.input == "turn" is rejected 409 not_supported; a kind that does is
// dispatched to its lane's turnStarter.
func (h *Hub) handleTurn(w http.ResponseWriter, r *http.Request) {
	var req turnRequest
	if !decodeHubBody(w, r, &req) {
		return
	}
	// The lane receives the NORMALIZED text verbatim (CRLF folded), not a sanitized or
	// re-quoted copy: a turn travels as a JSON string over the agent's own protocol, so
	// there is no pane, no shell and no escaping layer that content could break out of —
	// unlike POST /input, which types into a terminal.
	text := NormalizeNewlines(req.Text)
	if trimFeedText(text) == "" {
		writeError(w, http.StatusBadRequest, "empty_text", "text is required")
		return
	}
	_, watcher, kf, ok := h.verbTarget(w, r)
	if !ok {
		return
	}
	if kf.Input != inputModeTurn {
		writeError(w, http.StatusConflict, errNotSupported,
			"this session's kind does not accept turns")
		return
	}
	starter, ok := watcher.(turnStarter)
	if !ok {
		writeError(w, http.StatusConflict, errNotAccepting, noLaneMsg)
		return
	}
	turnID, err := starter.startTurn(r.Context(), text)
	if err != nil {
		h.writeLaneError(w, "the agent did not accept the turn", err)
		return
	}
	writeJSON(w, http.StatusAccepted, turnResponse{TurnID: turnID})
}

// handleInterrupt serves POST /v1/sessions/{slug}/interrupt. The body is IGNORED (the
// verb carries no parameters) but still size-capped, so a client that posts one is not
// treated differently from one that posts none. A kind whose row does not advertise
// kind_features.interrupt is rejected 409 not_supported.
func (h *Hub) handleInterrupt(w http.ResponseWriter, r *http.Request) {
	if !discardHubBody(w, r) {
		return
	}
	_, watcher, kf, ok := h.verbTarget(w, r)
	if !ok {
		return
	}
	if !kf.Interrupt {
		writeError(w, http.StatusConflict, errNotSupported,
			"this session's kind does not support interrupt")
		return
	}
	interrupter, ok := watcher.(turnInterrupter)
	if !ok {
		writeError(w, http.StatusConflict, errNotAccepting, noLaneMsg)
		return
	}
	if err := interrupter.interruptTurn(r.Context()); err != nil {
		h.writeLaneError(w, "the interrupt was not delivered", err)
		return
	}
	writeJSON(w, http.StatusAccepted, interruptResponse{Interrupting: true})
}

// handleApproval serves POST /v1/sessions/{slug}/approvals/{id}. A kind that answers
// approvals on the pane (kind_features.approvals == "tui") is rejected 409
// not_supported before any lookup — including for the informational `pane-*` ids the
// pane-anchor kinds publish, which are deliberately not remotely resolvable.
func (h *Hub) handleApproval(w http.ResponseWriter, r *http.Request) {
	var req approvalRequest
	if !decodeHubBody(w, r, &req) {
		return
	}
	if !validApprovalDecision(req.Decision) {
		writeError(w, http.StatusBadRequest, "invalid_decision",
			"decision must be one of allow, allow_always, deny")
		return
	}
	// A syntactically invalid id is a malformed REQUEST, not a missing approval: 400,
	// never 404 — a 404 here would imply the id was well-formed but unknown, which is
	// the distinct (reserved) unknown_approval case.
	id := r.PathValue("id")
	if !ApprovalIDRe.MatchString(id) {
		writeError(w, http.StatusBadRequest, "invalid_approval_id",
			"approval id must match ^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$")
		return
	}
	tr, watcher, kf, ok := h.verbTarget(w, r)
	if !ok {
		return
	}
	if kf.Approvals != approvalsRemote {
		writeError(w, http.StatusConflict, errNotSupported,
			"approvals for this session's kind are answered in the terminal")
		return
	}
	resolver, ok := watcher.(approvalResolver)
	if !ok {
		writeError(w, http.StatusConflict, errNotAccepting, noLaneMsg)
		return
	}

	// Resolution state comes from the LANE's fold (approvalState), never from
	// tr.pendingApprovals: the snapshot is pending-only by wire contract, so it cannot
	// tell "already answered" from "never existed".
	if answeredFromApprovalState(w, resolver, id, req.Decision) {
		return
	}

	// CLAIM the id before writing. The three lines above are a check, and check-then-act
	// would let two concurrent requests for the same id both find it pending and both POST
	// upstream (double-answering the ask, and racing over which decision the fold records).
	// The claim makes that transition atomic: exactly one request owns an id's resolution.
	switch resolver.claimApproval(id, req.Decision) {
	case approvalClaimBusy:
		// Another request is mid-POST for this id — retryable, and deliberately reported
		// even for the SAME decision: the honest answer is "in flight", and the retry
		// will see the recorded resolution and answer idempotently.
		writeError(w, http.StatusConflict, errNotAccepting,
			"a resolution for this approval is already in progress")
		return
	case approvalClaimSettled:
		// It stopped being pending between the read above and the claim: opencode's own
		// permission.replied landed, or a racing request committed. Answer from the
		// recorded state rather than POSTing a second answer.
		if answeredFromApprovalState(w, resolver, id, req.Decision) {
			return
		}
		writeError(w, http.StatusConflict, errNotAccepting,
			"this approval changed state; retry")
		return
	}

	if err := resolver.resolveApproval(r.Context(), id, req.Decision); err != nil {
		// The write failed, so nothing was resolved: hand the claim back so a retry (or
		// the operator, in the TUI) can still answer the ask.
		resolver.releaseApproval(id)
		h.writeLaneError(w, "the agent did not accept the decision", err)
		return
	}
	// Bookkeeping runs SYNCHRONOUSLY on success, closing the ~1-tick window in which a
	// same-decision replay would re-POST: commitApproval consumes the claim and marks the
	// fold entry resolved (the feed's resolved row is still emitted once — the fold's
	// resolve is idempotent against the permission.replied event that follows). It reports
	// the decision the fold ACTUALLY holds, which is what the response echoes: if the
	// stream's own reply for this id won the race, its record is the truth, not ours.
	recorded := resolver.commitApproval(id, req.Decision)
	h.republishApprovals(r.PathValue("slug"), tr, watcher)
	writeJSON(w, http.StatusOK, approvalResponse{Resolved: true, Decision: recorded})
}

// answeredFromApprovalState answers the request from the lane's recorded approval state
// when that state is final — unknown id (404), the same decision replayed (200,
// idempotent, with NO upstream write), or a different decision against a resolved ask
// (409 already_resolved, which also covers "resolved with no known decision": answered in
// the TUI or retired by a reseed, and the hub cannot re-answer what is already closed).
// It reports whether it wrote a response; false means the ask is PENDING and the caller
// proceeds to claim it.
func answeredFromApprovalState(w http.ResponseWriter, resolver approvalResolver, id, want string) bool {
	status, decision, known := resolver.approvalState(id)
	switch {
	case !known:
		writeError(w, http.StatusNotFound, errUnknownApproval,
			"this session has no approval with that id")
	case status == approvalStatusResolved && decision == want:
		writeJSON(w, http.StatusOK, approvalResponse{Resolved: true, Decision: decision})
	case status == approvalStatusResolved:
		writeError(w, http.StatusConflict, errAlreadyResolved,
			"this approval was already resolved")
	default:
		return false
	}
	return true
}

// republishApprovals refreshes the session's pending_approvals snapshot right after a
// resolve, rather than leaving it stale until the next reconcile tick. It republishes
// through the SAME producer reconcile uses (approvalPublisher, hub_reconcile.go) rather
// than hand-filtering the snapshot, so the resolve path and the tick path cannot drift.
//
// tr may be an ORPHAN by now: reconcile can have replaced the tracked entry (a recreate)
// while the upstream POST was in flight, and writing into a detached struct would publish
// into nothing — or worse, resurrect a dead incarnation's state if the pointer were ever
// re-reachable. So the entry is re-looked-up under trackMu and written only when it is
// still THE tracked entry (pointer identity); otherwise the write is skipped and the next
// tick republishes from the live watcher. Read UNLOCKED, committed under trackMu — the
// reconcile lock order.
func (h *Hub) republishApprovals(slug string, tr *trackedSession, watcher sessionWatcher) {
	publisher, ok := watcher.(approvalPublisher)
	if !ok {
		return
	}
	pending := publisher.pendingApprovals()
	h.trackMu.Lock()
	defer h.trackMu.Unlock()
	if cur, ok := h.tracked[slug]; ok && cur == tr {
		tr.pendingApprovals = pending
	}
}

// writeLaneError maps a lane method's failure onto the 409 not_accepting envelope —
// never a new status code, and never a 5xx: an upstream that refused, timed out, or has
// gone away is a retryable "not right now" in this vocabulary.
//
// The WIRE message is deliberately coarse. A lane error's text names the upstream URL,
// which embeds the loopback port and the pinned agent session id — hub-internal
// addressing a caller has no business learning from an error string — so the detail goes
// to the hub log and the client gets the verb's prefix plus, at most, the upstream status
// code (safe, and the one detail that tells an operator "the agent said no" apart from
// "the agent never answered"). The two sentinels leak nothing and are surfaced verbatim:
// errNoAgentSession carries operator-facing remediation, errWatcherClosed is generic.
func (h *Hub) writeLaneError(w http.ResponseWriter, prefix string, err error) {
	switch {
	case errors.Is(err, errNoAgentSession):
		writeError(w, http.StatusConflict, errNotAccepting, errNoAgentSession.Error())
		return
	case errors.Is(err, errWatcherClosed):
		writeError(w, http.StatusConflict, errNotAccepting, prefix+": the agent session is gone")
		return
	}
	h.cfg.logf("rc hub: %s: %v", prefix, err)
	detail := "upstream request failed"
	var statusErr *ocStatusError
	if errors.As(err, &statusErr) {
		detail = fmt.Sprintf("upstream status %d", statusErr.Status())
	}
	writeError(w, http.StatusConflict, errNotAccepting, prefix+": "+detail)
}

// verbTarget is the shared middle of every verb: it resolves {slug} to its tracked
// entry, the entry's WATCHER, and the kind's kind_features row, writing the 404
// unknown_slug envelope and returning ok=false when the slug is not tracked (the same
// rule handleMessages applies — no re-derivation from tmux, so no capture-pane per
// request). Everything is read under trackMu, which reconcile also holds while mutating
// tracked; the watcher POINTER in particular must be copied out here, because reconcile
// commits that field under this lock and type-asserting it after the unlock would be a
// data race.
//
// The returned entry and watcher are used UNLOCKED (the lane call must never run under
// trackMu — the handleSessions precedent). KNOWN, ACCEPTED WINDOW: between this copy and
// the lane call, reconcile may replace the entry and close the old watcher; the in-flight
// call then targets a dead per-create port, fails, and maps to 409 not_accepting.
//
// A tracked kind with NO row (an unregistered kind, or a newer client's) yields the ZERO
// row, which advertises no verb — a missing row must reject exactly as finally as an
// explicit false, so callers need no separate "unknown kind" branch.
func (h *Hub) verbTarget(w http.ResponseWriter, r *http.Request) (*trackedSession, sessionWatcher, KindFeatures, bool) {
	h.trackMu.Lock()
	tr, ok := h.tracked[r.PathValue("slug")]
	if !ok {
		h.trackMu.Unlock()
		writeError(w, http.StatusNotFound, "unknown_slug", "no such rc session")
		return nil, nil, KindFeatures{}, false
	}
	kind, watcher := tr.kind, tr.watcher
	h.trackMu.Unlock()
	return tr, watcher, kindFeatureRow(kind), true
}

// decodeHubBody bounds and JSON-decodes a POST body (every hub POST, /input included),
// writing the 413/400 envelope and returning false when it could not be read. Unknown
// fields are IGNORED — an old server must not reject a newer client's additive field —
// and Content-Type is not enforced.
func decodeHubBody(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, hubMaxBodyBytes)
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(v); err != nil {
		if wroteTooLarge(w, err) {
			return false
		}
		writeError(w, http.StatusBadRequest, "invalid_json", "malformed request body")
		return false
	}
	// The body is exactly ONE JSON value. Without this second read, a request could
	// smuggle arbitrary trailing bytes after a small valid prefix: the size cap would
	// never trip (Decode stops at the first value) and trailing garbage would be
	// silently ignored — so the pinned precedence (413 before 400 before everything)
	// would be a lie for such bodies. Draining to EOF makes oversized trailers a 413
	// and non-empty trailers a 400.
	if err := drainToEOF(dec, r.Body); err != nil {
		if wroteTooLarge(w, err) {
			return false
		}
		writeError(w, http.StatusBadRequest, "invalid_json", "request body has trailing data after the JSON value")
		return false
	}
	return true
}

// drainToEOF verifies dec's underlying body is exhausted after the decoded value:
// any non-whitespace trailer is an error (json.Decoder tolerates leading whitespace
// of a "next value", so a whitespace-only tail reads as io.EOF), and reading the
// tail through the MaxBytesReader surfaces an oversized trailer as MaxBytesError.
func drainToEOF(dec *json.Decoder, body io.Reader) error {
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return errors.New("trailing JSON value")
		}
		return err
	}
	// dec buffered at most one read; make sure the raw body is fully consumed so a
	// huge whitespace tail still trips the size cap rather than being ignored.
	if _, err := io.Copy(io.Discard, body); err != nil {
		return err
	}
	return nil
}

// discardHubBody drains a body the verb does not read, purely to enforce the size cap
// (an oversized body is a 413 even when its content is irrelevant — the cap is about
// what the hub is willing to READ, and answering 202 to a 1 GiB body would invite
// exactly that). Any OTHER read error is ignored: the body is not part of this verb's
// contract, so a truncated one must not fail an otherwise valid request.
func discardHubBody(w http.ResponseWriter, r *http.Request) bool {
	r.Body = http.MaxBytesReader(w, r.Body, hubMaxBodyBytes)
	_, err := io.Copy(io.Discard, r.Body)
	return err == nil || !wroteTooLarge(w, err)
}

// wroteTooLarge writes the 413 envelope when err is the body-cap error, reporting
// whether it did — the one place the cap's rejection is spelled.
func wroteTooLarge(w http.ResponseWriter, err error) bool {
	var maxErr *http.MaxBytesError
	if !errors.As(err, &maxErr) {
		return false
	}
	writeError(w, http.StatusRequestEntityTooLarge, "too_large", "request body exceeds 16 KiB")
	return true
}
