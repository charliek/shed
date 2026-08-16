package rc

import (
	"encoding/json"
	"errors"
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
// The three routes exist NOW so clients (mobile above all) can be written against a
// stable surface, but no lane implements them in this phase: every handler validates
// fully and then rejects with 409 not_supported, because no kind's kind_features row
// advertises the verb (input == "turn" / interrupt == true / approvals == "remote").
// The success shapes are pinned by the response types below and their schema test, so
// the lane implementations that land later cannot recontract them.
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
// R0 handlers deliberately take NO input mutex and capture NO pane: there is nothing
// to deliver, so the serialization the input path needs has no counterpart here. The
// tracked-lookup rule (404 for an unknown slug, no re-derivation from tmux) matches
// handleMessages.

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
	// "gated" describes. No kind carries it in this phase — it is the predicate the
	// turn handler tests, so a lane advertising it lights the verb up.
	inputModeTurn = "turn"
	// approvalsRemote is the kind_features.approvals value denoting a lane whose
	// approvals are answered THROUGH the hub (the POST /approvals/{id} verb) rather
	// than on the pane ("tui"). No kind carries it in this phase — it is the
	// predicate the approval handler tests.
	approvalsRemote = "remote"
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

// noLaneMsg is the DEFENSIVE trailing rejection every verb ends in: the capability
// check passed, so some kind_features row claims the verb, but no lane implements it.
// Unreachable in this phase (no row advertises any verb) — it exists so a row that
// lights a verb up before its lane lands fails closed with a retryable 409 instead of
// falling through to a success with no effect.
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

// The verb SUCCESS bodies. RESERVED FOR THE LANE IMPLEMENTATIONS: nothing in this
// phase emits them (every request is rejected before it could), but their shape is
// pinned now — by these types and by the round-trip schema test — so the lane that
// eventually answers 2xx ships the shape clients were already written against.
//
//	turn       → 202 {"turn_id": "<opaque>"}   ; busy      → 409 not_accepting
//	interrupt  → 202 {"interrupting": true}    ; no turn   → 409 not_accepting
//	approvals  → 200 {"resolved": true, "decision": "<decision>"}
//	                                            ; same decision replayed → 200 (idempotent)
//	                                            ; different decision     → 409 already_resolved
//	                                            ; unknown id             → 404 unknown_approval
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
// notes at the top of this file. Rejected with 409 not_supported for every kind in
// this phase (no kind advertises kind_features.input == "turn").
func (h *Hub) handleTurn(w http.ResponseWriter, r *http.Request) {
	var req turnRequest
	if !decodeHubBody(w, r, &req) {
		return
	}
	if trimFeedText(NormalizeNewlines(req.Text)) == "" {
		writeError(w, http.StatusBadRequest, "empty_text", "text is required")
		return
	}
	kf, ok := h.verbFeatures(w, r)
	if !ok {
		return
	}
	if kf.Input != inputModeTurn {
		writeError(w, http.StatusConflict, errNotSupported,
			"this session's kind does not accept turns")
		return
	}
	// A lane implementation replaces this with the real submit path (202 turnResponse).
	writeError(w, http.StatusConflict, errNotAccepting, noLaneMsg)
}

// handleInterrupt serves POST /v1/sessions/{slug}/interrupt. The body is IGNORED (the
// verb carries no parameters) but still size-capped, so a client that posts one is not
// treated differently from one that posts none. Rejected with 409 not_supported for
// every kind in this phase (no kind advertises kind_features.interrupt).
func (h *Hub) handleInterrupt(w http.ResponseWriter, r *http.Request) {
	if !discardHubBody(w, r) {
		return
	}
	kf, ok := h.verbFeatures(w, r)
	if !ok {
		return
	}
	if !kf.Interrupt {
		writeError(w, http.StatusConflict, errNotSupported,
			"this session's kind does not support interrupt")
		return
	}
	// A lane implementation replaces this with the real cancel path (202
	// interruptResponse).
	writeError(w, http.StatusConflict, errNotAccepting, noLaneMsg)
}

// handleApproval serves POST /v1/sessions/{slug}/approvals/{id}. Rejected with 409
// not_supported for every kind in this phase (every kind answers approvals on the pane
// — kind_features.approvals == "tui", not "remote").
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
	if !ApprovalIDRe.MatchString(r.PathValue("id")) {
		writeError(w, http.StatusBadRequest, "invalid_approval_id",
			"approval id must match ^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$")
		return
	}
	kf, ok := h.verbFeatures(w, r)
	if !ok {
		return
	}
	if kf.Approvals != approvalsRemote {
		writeError(w, http.StatusConflict, errNotSupported,
			"approvals for this session's kind are answered in the terminal")
		return
	}
	// A lane implementation replaces this with the real resolve path (200
	// approvalResponse, or 404 errUnknownApproval / 409 errAlreadyResolved).
	writeError(w, http.StatusConflict, errNotAccepting, noLaneMsg)
}

// verbFeatures is the shared middle of every verb: it resolves {slug} to its tracked
// kind's kind_features row, writing the 404 unknown_slug envelope and returning
// ok=false when the slug is not tracked (the same rule handleMessages applies — no
// re-derivation from tmux; R0 verbs deliver nothing, so there is no acceptance window
// to close and no reason to spend a capture-pane per request). The kind is read under
// trackMu, which reconcile also holds while mutating tracked.
//
// A tracked kind with NO row (an unregistered kind, or a newer client's) yields the
// ZERO row, which advertises no verb — a missing row must reject exactly as finally as
// an explicit false, so callers need no separate "unknown kind" branch.
func (h *Hub) verbFeatures(w http.ResponseWriter, r *http.Request) (KindFeatures, bool) {
	h.trackMu.Lock()
	tr, ok := h.tracked[r.PathValue("slug")]
	var kind Kind
	if ok {
		kind = tr.kind
	}
	h.trackMu.Unlock()
	if !ok {
		writeError(w, http.StatusNotFound, "unknown_slug", "no such rc session")
		return KindFeatures{}, false
	}
	return kindFeatures()[kind], true
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
