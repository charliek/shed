package delivery

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"
)

// Who is asked (plan 040, section 3.2). The Service chooses one Source per
// request by where the shed's user comes from, read from the server's static
// configuration alone (SetOrigin): no lookup snapshot, no trust state, no
// caller. So the choice is the same for an HTTP caller, a delayed cloud job
// and a resumed one, and does not change when a directory's trust time ends.
//
//	not a directory server               anyone          the pinned machine
//	a directory server                   a local admin   the pinned machine
//	a directory server                   anyone else     the account service, and nothing else

// The origins of a user name, as SetOrigin's function answers them and as a
// shed's metadata records the origin of its user at create (user_origin).
const (
	// OriginLocal is a user of a server with no directory, or a name in a
	// directory server's local-admin set: their pinned machine answers.
	OriginLocal = "local"
	// OriginDirectory is every other name on a directory server: the
	// account service answers, and nothing else.
	OriginDirectory = "directory"
	// OriginUnknown is the recorded origin of a shed recovered with no
	// evidence of where its user came from (a cloud VM adopted, or a lost
	// user restored, from an instance with no origin label). Unlike an
	// empty origin (a record v0.11 wrote, which the start-up migration
	// fills from the decision the first successful migration recorded) it is never filled
	// by the migration or by a later read of the instance; only an explicit
	// assignment replaces it. A delivery to such a shed is refused
	// (ErrShedOriginUnknown).
	OriginUnknown = "unknown"
)

// ErrShedOriginChanged refuses a delivery to a shed whose recorded user
// origin (user_origin, stamped at create) is not the current origin of its
// user's name: a configuration change moved the name between the local
// admins and the directory, so the principal of that name now is not the
// one who made the shed. Nothing is requested and the record is unchanged.
var ErrShedOriginChanged = errors.New("the shed's user is no longer the one who created it (the name moved between this server's local admins and the account service); no delivery is made to it: create a new shed")

// ErrShedOriginUnknown refuses a delivery to a shed that has no recorded
// user origin: one made before user_origin whose one-time stamp at start-up
// has not been saved (durably) yet. It fails closed: such a shed gets
// nothing until a start stamps it, so no principal's credentials can reach
// a shed whose maker is not known. Nothing is requested and the record is
// unchanged.
var ErrShedOriginUnknown = errors.New("the shed has no record of where its user came from; no credentials are delivered to it; recreate the shed (a shed made before this version is recorded by this version's first successful origin migration, at a start; a failed one stamps nothing and is retried at the next start; one made after that by an older shed-server stays unknown)")

// Source is who answers a delivery request: the user's bound machine (the
// machine source, the contract's section 3a) or the account service (the
// hosted source, section 3b).
type Source interface {
	// Applicable reports whether a request for p would ask this source.
	// It has no side effect and makes no network call. Run.request calls it
	// (through Service.Applicable) before it probes the shed's agent; a
	// resumed run never does.
	Applicable(p Params) bool
	// Named is the source a failure before the ask (no slot by the
	// deadline) records: the bound machine, or the service. Nil when there
	// is none to name.
	Named(p Params) *RecordSource
	// Ask asks the source for the answer to q, holding the request's
	// admission (taken by the Service before it). It returns the result (an
	// accepted bundle, not_applicable, or a failure with its reason, and
	// the source to record), or an error for what is not a delivery
	// outcome: ctx ending first, and for the hosted source the shed's stop
	// or delete (ErrShedEnded) and the shutdown (ErrServiceClosed).
	Ask(ctx context.Context, q Asked) (Result, error)
}

// Asked is one request as a source is asked it.
type Asked struct {
	Params   Params
	Request  *Request
	Line     []byte
	Deadline time.Time
}

// SetOrigin sets how the Service tells where a user name comes from:
// OriginLocal or OriginDirectory, from the server's static configuration
// (config.ServerConfig.UserOrigin). Unset, every user is local. Called once,
// before the Service is used.
func (s *Service) SetOrigin(origin func(user string) string) {
	if s != nil {
		s.origin = origin
	}
}

// Origin is where user comes from: OriginDirectory for a directory user,
// OriginLocal otherwise.
func (s *Service) Origin(user string) string {
	if s == nil || s.origin == nil {
		return OriginLocal
	}
	if s.origin(user) == OriginDirectory {
		return OriginDirectory
	}
	return OriginLocal
}

// CheckOrigin refuses a delivery to a shed whose recorded origin is not
// user's current origin (ErrShedOriginChanged), and to a shed with no
// recorded origin (ErrShedOriginUnknown): it fails closed.
func (s *Service) CheckOrigin(user, recorded string) error {
	switch recorded {
	case "", OriginUnknown:
		return ErrShedOriginUnknown
	case s.Origin(user):
		return nil
	}
	return ErrShedOriginChanged
}

// source is the Source that answers p's user.
func (s *Service) source(p Params) Source {
	if s.Origin(p.User) == OriginDirectory {
		return hostedSource{s: s}
	}
	return machineSource{s: s}
}

// machineSource is the user's pinned machine, over the pin: today's
// delivery, moved behind Source with no change.
type machineSource struct{ s *Service }

func (m machineSource) Applicable(p Params) bool {
	_, ok := m.s.pin.DeliveryBound(p.User)
	return ok
}

func (m machineSource) Named(p Params) *RecordSource {
	if bound, ok := m.s.pin.DeliveryBound(p.User); ok {
		return bound.Record()
	}
	return nil
}

// Ask sends the request on the user's delivery stream and waits for its
// answer, up to the deadline: the accepted bundle, or source_offline (the
// bound machine does not hold its stream: at once), source_timeout,
// source_disconnected (the stream ended, or the binding moved), or
// answer_refused with the rule.
func (m machineSource) Ask(ctx context.Context, q Asked) (Result, error) {
	s, p, req := m.s, q.Params, q.Request
	// One read of the binding, whose machine an offline request names: an
	// unbind since Applicable leaves no binding, and no request.
	bound, ok := s.pin.DeliveryBound(p.User)
	if !ok {
		return Result{Outcome: OutcomeNotRequested, Reason: ReasonNotApplicable, At: time.Now().UTC()}, nil
	}
	st, connected := s.pin.DeliveryStream(p.User)
	if !connected {
		log.Printf("delivery: request for shed %q%s not sent: its source, machine %s (%q), is not connected",
			p.Shed.Name, forUser(p.User), bound.MachineID, bound.MachineName)
		return Result{Outcome: OutcomeNoAnswer, Reason: ReasonSourceOffline, Source: bound.Record(), At: time.Now().UTC()}, nil
	}
	e := &entry{
		id: req.ID, req: req, user: p.User, machine: st.Machine(), gen: st.Generation(),
		serial: st.Serial(), stream: st.ID(), done: st.Done(), deadline: q.Deadline,
	}
	if err := s.table.add(e); err != nil {
		return Result{}, fmt.Errorf("delivery request for shed %q: %w", p.Shed.Name, err)
	}

	src := st.Machine()
	res := Result{RequestID: req.ID, Source: src.Record()}
	id := req.ID
	if !st.Offer(Outgoing{ID: id, Line: q.Line, Live: func() bool { return s.table.live(id) }}) {
		s.table.endUnanswered(e, false)
		log.Printf("delivery: request %s for shed %q%s: the stream of machine %s (%q) is not taking requests",
			id, p.Shed.Name, forUser(p.User), src.MachineID, src.MachineName)
		res.Outcome, res.Reason, res.At = OutcomeNoAnswer, ReasonSourceDisconnected, time.Now().UTC()
		return res, nil
	}
	log.Printf("delivery: request %s (%s) for shed %q%s sent to machine %s (%q), deadline %s",
		id, p.Reason, p.Shed.Name, forUser(p.User), src.MachineID, src.MachineName, req.Deadline)
	if sentHook != nil {
		sentHook(id)
	}

	j, how, err := s.wait(ctx, e)
	res.At = time.Now().UTC()
	switch {
	case err != nil:
		return Result{}, err
	case how != "":
		res.Outcome, res.Reason = OutcomeNoAnswer, how
	case j.answer != nil:
		res.Outcome, res.Bundle = OutcomeAccepted, &Bundle{answer: j.answer}
	default:
		res.Outcome, res.Reason, res.Rule = OutcomeRefused, ReasonAnswerRefused, j.rule
	}
	return res, nil
}
