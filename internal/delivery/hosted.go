package delivery

import (
	"context"
	"errors"
	"log"
	"time"
)

// The hosted source (the contract's section 3b): the account service, asked
// over HTTPS with the enrolment the server holds for its directory. The
// HTTP exchange is internal/directory's (Runtime.Deliver); this side judges
// what it returns. Nothing the service sends but an accepted answer's items
// and problems is kept: its error texts, redirect targets and unparseable
// bodies are classified by the client and dropped there, and this side
// logs and records reasons and rules only.

// HostedStatus is what one hosted delivery came to, as the client classifies
// the service's replies (section 3b's two tables).
type HostedStatus int

const (
	// HostedAnswer: a 200 whose body (returned with it) was read whole,
	// within MaxBodyBytes, and holds no credential the server presented. It
	// is still to be judged under the answer rules.
	HostedAnswer HostedStatus = iota
	// HostedNotApplicable: NO_SOURCE, invalid_scope, INSUFFICIENT_SCOPE, or
	// the not-delivering memory: no source for this person here.
	HostedNotApplicable
	// HostedOffline: unreachable, 429, 5xx, or not enrolled.
	HostedOffline
	// HostedTimeout: the deadline passed, or 410 DEADLINE_PASSED.
	HostedTimeout
	// HostedDisconnected: the 200's body ended before it was complete.
	HostedDisconnected
	// HostedRefused: the service refused the server or its request.
	HostedRefused
	// HostedTooLarge: a 200 over MaxBodyBytes (answer_refused,
	// body_too_large).
	HostedTooLarge
	// HostedEchoed: a 200 that holds the bearer token or the assertion the
	// server presented (answer_refused, echoes_credential).
	HostedEchoed
)

func (st HostedStatus) String() string {
	switch st {
	case HostedAnswer:
		return "answer"
	case HostedNotApplicable:
		return "not applicable"
	case HostedOffline:
		return "offline"
	case HostedTimeout:
		return "timeout"
	case HostedDisconnected:
		return "disconnected"
	case HostedRefused:
		return "refused"
	case HostedTooLarge:
		return "too large"
	case HostedEchoed:
		return "echoed a credential"
	}
	return "unknown"
}

// Hosted is the account service as the hosted source reaches it:
// *directory.Runtime.
type Hosted interface {
	// Deliver sends request (the request document, compact) and returns
	// how the exchange ended and, for HostedAnswer, the body. ctx bounds the
	// whole exchange (its deadline is the request's): ending it aborts the
	// HTTP call. It never returns the service's text but in an answer's
	// body.
	Deliver(ctx context.Context, request []byte) (HostedStatus, []byte)
	// NotDelivering reports, with no network call, whether the service has
	// refused this server deliveries within the last five minutes
	// (invalid_scope, INSUFFICIENT_SCOPE): Deliver then answers
	// HostedNotApplicable with no request. It is a status line (the
	// sources document's "not_delivering"), never a record's reason.
	NotDelivering() bool
}

// hostedHolder holds the late-bound handle (SetHosted).
type hostedHolder struct{ h Hosted }

// SetHosted gives the Service the account service, once the directory's
// runtime exists (serve builds it after the Service). Until it is set, and
// on a server with no directory, a directory user's request is
// source_offline: never a machine's.
func (s *Service) SetHosted(h Hosted) {
	if s != nil && h != nil {
		s.hosted.Store(&hostedHolder{h: h})
	}
}

func (s *Service) hostedHandle() Hosted {
	if hh := s.hosted.Load(); hh != nil {
		return hh.h
	}
	return nil
}

// NotDelivering reports whether the account service is, as far as this
// server remembers, not delivering to it: the "not_delivering" state of a
// directory user's source document. False with no account service.
func (s *Service) NotDelivering() bool {
	if s == nil {
		return false
	}
	h := s.hostedHandle()
	return h != nil && h.NotDelivering()
}

// EndShed ends every hosted request and run in flight for the shed (name,
// and its permanent id: a stop or delete is matched to the incarnation it
// was checked against, never to another shed of the name), and every run of
// it admitted before (Admit) that has not started yet: a stop or a delete
// of it is under way. The backend calls it once it has checked the shed's
// identity. The call is aborted, its slot released, and the run ends
// (ErrShedEnded) with nothing recorded; a reply that comes later is
// discarded.
func (s *Service) EndShed(name, id string) {
	if s != nil {
		s.inflight.end(shedKey(name, id), ErrShedEnded)
	}
}

// Admit is a run's admission for the shed (name and permanent id), taken
// where the backend admits the run (a provision's slot, a create's pending
// delivery, a remote create's job), before anything is probed or asked. A
// stop or delete of that shed after it (EndShed) ends the run when it
// starts, or at once if it is running: a run ended so never asks the
// account service, and never writes a bundle.
func (s *Service) Admit(name, id string) Ticket {
	if s == nil {
		return Ticket{}
	}
	return s.inflight.ticket(shedKey(name, id))
}

// Close ends every hosted request in flight, and refuses every later one
// (ErrServiceClosed): the server is shutting down.
func (s *Service) Close() {
	if s != nil {
		s.inflight.close()
	}
}

// hostedSource is the account service.
type hostedSource struct{ s *Service }

// Applicable is true for every directory user. Whether the service is
// delivering to this server is Deliver's to say (its memory of a refusal
// answers not applicable with no request), under the enrolment it reads.
func (h hostedSource) Applicable(Params) bool { return true }

func (h hostedSource) Named(Params) *RecordSource { return ServiceSource() }

// Ask makes the one call of section 3b, under q's deadline, and judges what
// came of it. ctx is already the watched context Request derived (a stop or
// delete of the shed, and the shutdown, end it with their cause).
func (h hostedSource) Ask(ctx context.Context, q Asked) (Result, error) {
	p := q.Params
	hd := h.s.hostedHandle()
	if hd == nil {
		log.Printf("delivery: request for shed %q%s not sent: the account service is not reachable from this server yet",
			p.Shed.Name, forUser(p.User))
		return Result{Outcome: OutcomeNoAnswer, Reason: ReasonSourceOffline, Source: ServiceSource(), At: time.Now().UTC()}, nil
	}
	actx, cancel := context.WithDeadline(ctx, q.Deadline)
	defer cancel()
	log.Printf("delivery: request %s (%s) for shed %q%s: asking the account service, deadline %s",
		q.Request.ID, p.Reason, p.Shed.Name, forUser(p.User), q.Request.Deadline)
	if sentHook != nil {
		sentHook(q.Request.ID)
	}
	status, body := hd.Deliver(actx, q.Line)
	if err := ended(ctx); err != nil {
		// Its shed stopped or went, the server is stopping, or the caller
		// gave up: whatever came is dropped, unread.
		return Result{}, err
	}
	res := Result{RequestID: q.Request.ID, Source: ServiceSource(), At: time.Now().UTC()}
	if status != HostedNotApplicable && !res.At.Before(q.Deadline) {
		// Past the deadline whatever it was: anything later is discarded.
		status, body = HostedTimeout, nil
	}
	switch status {
	case HostedAnswer:
		a, refusal := ParseAnswer(body, q.Request)
		if refusal != nil {
			res.Outcome, res.Reason, res.Rule = OutcomeRefused, ReasonAnswerRefused, refusal.Rule
		} else {
			res.Outcome, res.Bundle = OutcomeAccepted, &Bundle{answer: a}
		}
	case HostedNotApplicable:
		return Result{Outcome: OutcomeNotRequested, Reason: ReasonNotApplicable, At: res.At}, nil
	case HostedTooLarge:
		res.Outcome, res.Reason, res.Rule = OutcomeRefused, ReasonAnswerRefused, RuleBodyTooLarge
	case HostedEchoed:
		res.Outcome, res.Reason, res.Rule = OutcomeRefused, ReasonAnswerRefused, RuleEchoesCredential
	case HostedTimeout:
		res.Outcome, res.Reason = OutcomeNoAnswer, ReasonSourceTimeout
	case HostedDisconnected:
		res.Outcome, res.Reason = OutcomeNoAnswer, ReasonSourceDisconnected
	case HostedRefused:
		res.Outcome, res.Reason = OutcomeNoAnswer, ReasonSourceRefused
	default:
		res.Outcome, res.Reason = OutcomeNoAnswer, ReasonSourceOffline
	}
	return res, nil
}

// ended is the error a hosted request whose context ended early ends with:
// the cause (ErrShedEnded, ErrServiceClosed), or the caller's own error.
// Nil while ctx lives.
func ended(ctx context.Context) error {
	if ctx.Err() == nil {
		return nil
	}
	if cause := context.Cause(ctx); errors.Is(cause, ErrShedEnded) || errors.Is(cause, ErrServiceClosed) {
		return cause
	}
	return ctx.Err()
}
