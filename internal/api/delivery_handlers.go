package api

// The provision contract's transport for a machine source
// (tests/provision-contract/v1/CONTRACT.md, section 3): the delivery request
// stream and the answer route. The machine is admitted for its user by the
// pin (plugin.Registry.SubscribeDelivery, under the user's gate, as a bus
// subscription is); the requests, their binding and the judgement of an
// answer are internal/delivery's (Service, and its pending-request table).
//
// Who may: a person, as themselves, with a credentials certificate (the
// scope table, auth.go); never a service client (not on its allowlist,
// clientauth.go) and never a script key. Each refusal is answered in the
// contract's order:
//
//	403 FORBIDDEN                  a caller with no place here (a service
//	                               client) or a script key (both routes)
//	403 SOURCE_MACHINE_ID_REQUIRED no valid machine headers (both)
//	429 SOURCE_RATE_LIMITED        a new or changed machine past the user's
//	                               rate limit (stream)
//	403 SOURCE_NOT_BOUND           not the user's bound machine (both)
//	409 SOURCE_DUPLICATE_STREAM    the machine holds its stream already
//	404 unknown_request            (answer, and the rest below)
//	403 not_stream_owner
//	409 already_answered
//	410 deadline_passed
//	413 answer_refused             body_too_large, before it is read
//	400 answer_refused             not_json
//	422 answer_refused             any other rule
//
// Every response on both routes carries Cache-Control: no-store
// (deliveryNoStore, at the root, so the auth middleware's refusals carry it
// too).

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/delivery"
	"github.com/charliek/shed/internal/plugin"
)

// deliveryKeepaliveInterval is how often an idle delivery stream writes its
// keepalive comment (the contract's 15 seconds). A var so tests can shorten
// it.
var deliveryKeepaliveInterval = delivery.KeepaliveInterval

// errDeliveryDisabled is the code of a delivery route on a server that has
// no delivery service or no source bindings.
const errDeliveryDisabled = "DELIVERY_DISABLED"

// SetDelivery attaches the delivery service the delivery routes hand
// answers to. shed-server calls it once at start-up; without it the routes
// answer 503.
func (s *Server) SetDelivery(svc *delivery.Service) {
	s.delivery = svc
	if s.audit != nil {
		svc.SetAuditLog(s.audit)
	}
}

// isDeliveryPath reports whether path is under the delivery transport
// (/api/delivery*), which requires a credentials-scoped credential, as the
// credential bus does: a request carries the shape of a person's sheds, and
// an answer writes files and environment into them.
func isDeliveryPath(path string) bool {
	return path == "/api/delivery" || strings.HasPrefix(path, "/api/delivery/")
}

// deliveryNoStore sets Cache-Control: no-store on every response under
// /api/delivery, before anything else can answer.
func deliveryNoStore(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isDeliveryPath(r.URL.Path) {
			w.Header().Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

// deliveryCaller returns the user a delivery request acts as. It answers
// 403 FORBIDDEN, and reports false, for a caller with no place here: a
// service client, a request with no caller, and a script key (automation
// never holds a person's source, so it may neither receive their sheds'
// requests nor answer them; read from the live lookup on every request).
func deliveryCaller(w http.ResponseWriter, r *http.Request) (string, bool) {
	user, _, ok := busCaller(r)
	if !ok {
		writeError(w, http.StatusForbidden, config.ErrForbidden, "delivery requests are for users")
		return "", false
	}
	if callerFrom(r.Context()).Script {
		writeError(w, http.StatusForbidden, config.ErrForbidden, "a script key cannot hold or answer delivery requests")
		return "", false
	}
	return user, true
}

// deliveryReady answers 503 and reports false on a server with no delivery
// service or no source bindings (fail closed).
func (s *Server) deliveryReady(w http.ResponseWriter) bool {
	if s.delivery == nil || s.plugins == nil || s.plugins.Bindings() == nil {
		writeError(w, http.StatusServiceUnavailable, errDeliveryDisabled, "delivery is not initialized on this server")
		return false
	}
	return true
}

// handleDeliveryStream is GET /api/delivery/requests: the caller's machine
// is admitted for its user and holds the user's delivery stream until it
// disconnects. The 200 names the stream in X-Shed-Delivery-Stream; each
// request is one "request" event; a keepalive comment is written every 15
// seconds. Nothing is ever replayed: Last-Event-ID is ignored, and when the
// stream ends every request it carried fails.
func (s *Server) handleDeliveryStream(w http.ResponseWriter, r *http.Request) {
	user, ok := deliveryCaller(w, r)
	if !ok || !s.deliveryReady(w) {
		return
	}
	machine, ok := machineFromRequest(r)
	if !ok {
		s.missingID.logOnce(user, callerFrom(r.Context()).Name)
		writeMachineIDRequired(w)
		return
	}
	st, err := s.plugins.SubscribeDelivery(r.Context(), user, machine, callerFrom(r.Context()).Serial)
	if err != nil {
		var notBound *plugin.NotBoundError
		var throttled *plugin.ThrottledError
		switch {
		case errors.As(err, &notBound):
			writeSourceNotBound(w, notBound.Bound)
		case errors.As(err, &throttled):
			writeAdmissionThrottled(w, throttled.RetryAfter)
		case errors.Is(err, plugin.ErrDuplicateStream):
			writeDuplicateDeliveryStream(w)
		case errors.Is(err, plugin.ErrBindingStore):
			log.Printf("delivery: admitting machine %s (%q)%s: %v", machine.ID, machine.Name, forBusUser(user), err)
			writeError(w, http.StatusInternalServerError, config.ErrInternalError, "the credential source bindings could not be saved; nothing was registered, retry")
		case errors.Is(err, plugin.ErrBusClosed):
			writeError(w, http.StatusServiceUnavailable, errDeliveryDisabled, "the server is shutting down")
		case errors.Is(err, r.Context().Err()) && r.Context().Err() != nil:
			// The client went away while waiting for its user's gate.
		default:
			writeError(w, http.StatusInternalServerError, config.ErrInternalError, err.Error())
		}
		return
	}
	// The stream's end, whatever ends it, unregisters it, which fails every
	// request it carried.
	defer s.plugins.UnregisterDelivery(st)
	s.streamDelivery(w, r, st)
}

// writeDuplicateDeliveryStream answers the bound machine opening a second
// delivery stream: the bus's duplicate refusal, with its code.
func writeDuplicateDeliveryStream(w http.ResponseWriter) {
	msg := "this machine already holds its delivery stream on this server: another process may be using this machine's id " +
		"(a copied state directory), or the previous stream has not ended yet; it is retried"
	short := "this machine already holds its delivery stream on this server; it is retried"
	writeRefusal(w, http.StatusConflict, encodeRefusal(config.ErrSourceDuplicateStream, msg, short, nil))
}

// streamDelivery writes st's requests as SSE events until the request ends,
// the stream is unregistered (a move of the binding, shutdown), the caller
// no longer authenticates (a directory server), or a write fails or stalls.
// The bus stream's protections are kept: every write has the deadline
// sseWriteTimeout, the connection's unacknowledged data is bounded, and on
// a directory server the caller is checked again before each request.
func (s *Server) streamDelivery(w http.ResponseWriter, r *http.Request, st *plugin.DeliveryStream) {
	m := st.Machine()
	rc := http.NewResponseController(w)
	defer func() { _ = rc.SetWriteDeadline(time.Time{}) }()
	if err := limitUnackedData(r.Context(), sseUnackedTimeout); err != nil {
		log.Printf("delivery: cannot bound unacknowledged data on the stream of machine %s (%q): %v", m.MachineID, m.MachineName, err)
	}
	// One write and its flush within one deadline, cleared once done (see
	// streamListener for why it must not stay armed between writes).
	write := func(fn func() error) bool {
		if err := rc.SetWriteDeadline(time.Now().Add(sseWriteTimeout)); err != nil && !errors.Is(err, http.ErrNotSupported) {
			return false
		}
		err := fn()
		if err == nil {
			err = rc.Flush()
		}
		if err != nil {
			if errors.Is(err, os.ErrDeadlineExceeded) {
				log.Printf("delivery: closing the stream of machine %s (%q)%s: a write was not taken within %s; its requests fail",
					m.MachineID, m.MachineName, forBusUser(st.User()), sseWriteTimeout)
			}
			return false
		}
		if err := rc.SetWriteDeadline(time.Time{}); err != nil && !errors.Is(err, http.ErrNotSupported) {
			return false
		}
		return true
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set(delivery.HeaderStream, st.ID())
	if !write(func() error { w.WriteHeader(http.StatusOK); return nil }) {
		return
	}

	ctx := r.Context()
	keepalive := time.NewTicker(deliveryKeepaliveInterval)
	defer keepalive.Stop()
	stillTheCaller := s.busCallerCheck(r, st.User())
	for {
		select {
		case <-ctx.Done():
			return
		case <-st.Done():
			return
		case o := <-st.Requests():
			// Sent only while it still waits: a request that ended since it
			// was queued (its caller gave up, its deadline passed) is not.
			if o.Live == nil || !o.Live() {
				continue
			}
			if stillTheCaller != nil && !stillTheCaller() {
				log.Printf("delivery: closing the stream of machine %s (%q)%s: its certificate no longer authenticates its user; its requests fail",
					m.MachineID, m.MachineName, forBusUser(st.User()))
				return
			}
			if !write(func() error {
				_, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", delivery.EventRequest, o.Line)
				return err
			}) {
				return
			}
		case <-keepalive.C:
			if !write(func() error { _, err := io.WriteString(w, delivery.KeepaliveComment); return err }) {
				return
			}
		}
	}
}

// answerRefusal is the body of an answer_refused error: the API's error
// object, and the rule broken beside it.
type answerRefusal struct {
	Error config.APIErrorDetail `json:"error"`
	Rule  delivery.Rule         `json:"rule"`
}

// deliveryClaimedHook, when set (tests only), runs in handleDeliveryAnswer
// once the request is claimed and before its body is read.
var deliveryClaimedHook func()

// writeAnswerRefused answers a consumed answer refused under rule. The
// message names the rule and where, never a value.
func writeAnswerRefused(w http.ResponseWriter, rule delivery.Rule, where string) {
	msg := fmt.Sprintf("the answer breaks the rule %s", rule)
	if where != "" {
		msg += " at " + where
	}
	msg += "; the request is over"
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(rule.Status())
	_ = json.NewEncoder(w).Encode(answerRefusal{
		Error: config.APIErrorDetail{Code: delivery.CodeAnswerRefused, Message: msg},
		Rule:  rule,
	})
}

// handleDeliveryAnswer is POST /api/delivery/requests/{id}: a source's
// answer to one request it was sent. The binding is judged first, reading
// no body (so a caller that does not own the request cannot consume it);
// from the size check on, the request is consumed whatever the body is.
// The caller is authenticated afresh on this request by the middleware (on
// a directory server, against the user list as it is now).
func (s *Server) handleDeliveryAnswer(w http.ResponseWriter, r *http.Request) {
	user, ok := deliveryCaller(w, r)
	if !ok || !s.deliveryReady(w) {
		return
	}
	machine, ok := machineFromRequest(r)
	if !ok {
		s.missingID.logOnce(user, callerFrom(r.Context()).Name)
		writeMachineIDRequired(w)
		return
	}
	if err := s.plugins.DeliveryAnswerFrom(user, machine.ID); err != nil {
		var notBound *plugin.NotBoundError
		if errors.As(err, &notBound) {
			writeSourceNotBound(w, notBound.Bound)
			return
		}
		writeError(w, http.StatusServiceUnavailable, errDeliveryDisabled, "delivery is not initialized on this server")
		return
	}
	stream, _ := singleHeader(r, delivery.HeaderStream)
	claim, err := s.delivery.Claim(chi.URLParam(r, "id"), delivery.Answerer{
		User:    user,
		Machine: machine.ID,
		Serial:  callerFrom(r.Context()).Serial,
		Stream:  stream,
	})
	if err != nil {
		status, code, known := delivery.ClaimStatus(err)
		if !known {
			status, code = http.StatusInternalServerError, config.ErrInternalError
		}
		writeError(w, status, code, err.Error())
		return
	}
	// Consumed from here: whatever ends this handler ends the request.
	defer func() { _ = claim.Refuse(delivery.RuleNotJSON) }()
	if deliveryClaimedHook != nil {
		deliveryClaimedHook()
	}
	// refuse ends the request under rule and answers it; a request that
	// ended meanwhile (its stream ended, the binding moved) is
	// unknown_request, as one that ended before the claim is.
	refuse := func(rule delivery.Rule, where string) {
		if err := claim.Refuse(rule); err != nil {
			writeError(w, http.StatusNotFound, delivery.CodeUnknownRequest, err.Error())
			return
		}
		writeAnswerRefused(w, rule, where)
	}
	if r.ContentLength > delivery.MaxBodyBytes {
		refuse(delivery.RuleBodyTooLarge, "")
		return
	}
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Now().Add(delivery.AnswerReadTimeout))
	body, err := io.ReadAll(io.LimitReader(r.Body, delivery.MaxBodyBytes+1))
	switch {
	case err != nil:
		refuse(delivery.RuleNotJSON, "")
		return
	case len(body) > delivery.MaxBodyBytes:
		refuse(delivery.RuleBodyTooLarge, "")
		return
	}
	refusal, err := claim.Judge(body)
	clear(body)
	switch {
	case err != nil:
		writeError(w, http.StatusNotFound, delivery.CodeUnknownRequest, err.Error())
		return
	case refusal != nil:
		writeAnswerRefused(w, refusal.Rule, refusal.Where)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
