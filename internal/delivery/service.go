package delivery

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"time"
)

// Service is the server's delivery: it asks a person's source for a shed's
// bundle (Request), judges the source's answer when the transport hands it
// over (Claim), and writes an accepted bundle into a shed one stage at a time
// (Write). The orchestrator and the remote job call Request and Write; the
// API's delivery routes call Claim. It keeps nothing durable: a bundle lives
// in memory, with its caller, until it is written or dropped, and is never
// logged, stored or put in an error.
type Service struct {
	pin     Pin
	timeout time.Duration
	table   *table
}

// NewService returns a Service over pin that waits up to timeout for a
// connected source's answer (delivery.timeout).
func NewService(pin Pin, timeout time.Duration) *Service {
	return &Service{
		pin:     pin,
		timeout: timeout,
		table:   newTable(pin.DeliveryCurrent, time.Now),
	}
}

// Timeout is how long a request waits for a connected source's answer.
func (s *Service) Timeout() time.Duration { return s.timeout }

// Params is one delivery as its caller asks for it.
type Params struct {
	// Reason is create or provision.
	Reason Reason
	// User is the shed's user, the one whose source is asked: "" for the
	// implicit user of a server with no user list.
	User string
	// Shed is the shed the request is for, as the request carries it
	// (name, permanent id, backend, and repo, local_dir and project_dir
	// when it has them).
	Shed RequestShed

	// What makes a delivery not applicable (section 3.2 of the plan): a
	// shed created by a service client, a create called with a script key,
	// a shed restored from a snapshot. No request is made for any of them.
	ServiceClient bool
	ScriptKey     bool
	FromSnapshot  bool
}

// Result is what became of one Request.
type Result struct {
	// RequestID is the request's id ("" when none was sent).
	RequestID string
	// Outcome is accepted, refused, no_answer or not_requested.
	Outcome Outcome
	// Reason is "" for an accepted answer; not_applicable when no request
	// was made and none was due (no warning); otherwise the failure:
	// source_offline, source_timeout, source_disconnected or
	// answer_refused.
	Reason StepReason
	// Rule is the rule an answer broke (answer_refused only).
	Rule Rule
	// Source is the machine the request went to, or the bound machine
	// that was not connected. Nil when no machine was asked.
	Source *SourceMachine
	// At is when the delivery ended.
	At time.Time
	// Bundle is the accepted answer (accepted only).
	Bundle *Bundle
}

// Failed reports whether the delivery failed: a reason other than
// not_applicable. A failed delivery is warned about; a not applicable one
// is not.
func (r Result) Failed() bool {
	return r.Reason != "" && r.Reason != ReasonNotApplicable
}

// Bundle is an accepted answer, held in memory for the stages that write it.
// It prints as its item count only, however it is formatted, so a stray log
// line or error cannot carry a delivered value.
type Bundle struct {
	answer *Answer
}

// NewBundle is the bundle of a, an answer ParseAnswer accepted (Request's
// own come from there; this is for a caller that judged one itself, and
// for tests).
func NewBundle(a *Answer) *Bundle { return &Bundle{answer: a} }

// Items is the answer's items, in its order.
func (b *Bundle) Items() []Item {
	if b == nil || b.answer == nil {
		return nil
	}
	return b.answer.Items
}

// Problems is what the source could not deliver.
func (b *Bundle) Problems() []Problem {
	if b == nil || b.answer == nil {
		return nil
	}
	return b.answer.Problems
}

// StageItems is the items written at stage: its file items, and for stage 1
// every env item, in the answer's order.
func (b *Bundle) StageItems(stage int) []Item {
	var out []Item
	for _, it := range b.Items() {
		if it.ItemStage() == stage {
			out = append(out, it)
		}
	}
	return out
}

func (b *Bundle) String() string {
	return fmt.Sprintf("delivery bundle (%d items, %d problems)", len(b.Items()), len(b.Problems()))
}

// GoString is String: %#v prints no value either.
func (b *Bundle) GoString() string { return b.String() }

// Format writes String for every verb.
func (b *Bundle) Format(f fmt.State, _ rune) { _, _ = fmt.Fprint(f, b.String()) }

// newRequestID is 32 lowercase hex digits, at random.
func newRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("delivery: no randomness for a request id: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

// deadlineAfter is now plus d, rounded up to a whole second: the deadline
// a request carries is the one the server holds it to.
func deadlineAfter(now time.Time, d time.Duration) time.Time {
	t := now.Add(d).UTC()
	if r := t.Truncate(time.Second); !r.Equal(t) {
		return r.Add(time.Second)
	}
	return t
}

// Test hooks (nil in production): sentHook runs once a request has been
// queued on its stream.
var sentHook func(id string)

// Request asks the source of the shed's user for its bundle, as section 3.2
// of the plan decides:
//
//   - a shed created by a service client, a create with a script key, a
//     snapshot restore, and a user with no bound machine: no request,
//     not_applicable (no warning);
//   - a bound machine that does not hold its delivery stream: no request,
//     source_offline, at once;
//   - otherwise the request is sent on the stream and waited for, up to
//     the timeout: the accepted bundle, or source_timeout,
//     source_disconnected (the stream ended, or the binding moved), or
//     answer_refused with the rule.
//
// A request beyond the user's limit of waiting requests waits for a slot
// within the same timeout (source_timeout when none frees). The error is
// for what is not a delivery outcome: params that make no valid request,
// a request for a shed that already has one waiting (ErrShedPending), and
// ctx ending first (ctx's error; the request is then dropped).
func (s *Service) Request(ctx context.Context, p Params) (Result, error) {
	if p.ServiceClient || p.ScriptKey || p.FromSnapshot {
		return Result{Outcome: OutcomeNotRequested, Reason: ReasonNotApplicable, At: time.Now().UTC()}, nil
	}
	bound, ok := s.pin.DeliveryBound(p.User)
	if !ok {
		return Result{Outcome: OutcomeNotRequested, Reason: ReasonNotApplicable, At: time.Now().UTC()}, nil
	}
	deadline := deadlineAfter(time.Now(), s.timeout)
	req := &Request{
		Version:  Version,
		ID:       newRequestID(),
		Reason:   p.Reason,
		Deadline: FormatTime(deadline),
		User:     p.User,
		Shed:     p.Shed,
	}
	line, err := req.Marshal()
	if err != nil {
		return Result{}, fmt.Errorf("delivery request for shed %q: %w", p.Shed.Name, err)
	}
	// The server's own request is held to the contract it sends: one that
	// would not parse is a bug in the caller, never sent.
	if _, refusal := ParseRequest(line); refusal != nil {
		return Result{}, fmt.Errorf("delivery request for shed %q is not a valid request: %w", p.Shed.Name, refusal)
	}

	var e *entry
	var st Stream
	for {
		var connected bool
		st, connected = s.pin.DeliveryStream(p.User)
		if !connected {
			src := bound
			log.Printf("delivery: request for shed %q%s not sent: its source, machine %s (%q), is not connected",
				p.Shed.Name, forUser(p.User), src.MachineID, src.MachineName)
			return Result{Outcome: OutcomeNoAnswer, Reason: ReasonSourceOffline, Source: &src, At: time.Now().UTC()}, nil
		}
		e = &entry{
			id: req.ID, req: req, user: p.User, machine: st.Machine(), gen: st.Generation(),
			serial: st.Serial(), stream: st.ID(), done: st.Done(), shedID: p.Shed.ID, deadline: deadline,
		}
		freed, err := s.table.add(e)
		if err == nil {
			break
		}
		if !errors.Is(err, errUserFull) {
			return Result{}, fmt.Errorf("delivery request for shed %q: %w", p.Shed.Name, err)
		}
		timer := time.NewTimer(time.Until(deadline))
		select {
		case <-freed:
			timer.Stop()
		case <-st.Done():
			// Looked up again: offline, or the stream that replaced it.
			timer.Stop()
		case <-timer.C:
			src := st.Machine()
			log.Printf("delivery: request for shed %q%s not sent: the user had %d requests waiting until its deadline",
				p.Shed.Name, forUser(p.User), maxActivePerUser)
			return Result{Outcome: OutcomeNoAnswer, Reason: ReasonSourceTimeout, Source: &src, At: time.Now().UTC()}, nil
		case <-ctx.Done():
			timer.Stop()
			return Result{}, ctx.Err()
		}
	}

	src := st.Machine()
	res := Result{RequestID: req.ID, Source: &src}
	id := req.ID
	if !st.Offer(Outgoing{ID: id, Line: line, Live: func() bool { return s.table.live(id) }}) {
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
	log.Printf("delivery: request %s for shed %q%s: %s", id, p.Shed.Name, forUser(p.User), res.describe())
	return res, nil
}

// describe is a log line's account of a result: no value, ever.
func (r Result) describe() string {
	switch r.Outcome {
	case OutcomeAccepted:
		return r.Bundle.String() + " accepted"
	case OutcomeRefused:
		return fmt.Sprintf("answer refused (%s)", r.Rule)
	}
	return fmt.Sprintf("no answer (%s)", r.Reason)
}

// claimGrace is how long past its deadline a request whose answer was
// claimed in time waits for the judgement. A var so tests can shorten it.
var claimGrace = AnswerReadTimeout + 5*time.Second

// wait waits for e's judgement until its deadline (and, for an answer
// claimed by then, claimGrace more), its stream's end or ctx's. how is the
// failure reason when it ended without an accepted or refused answer. A
// stream that ends, or a binding that moves, fails e even while an answer's
// body is being read: that answer is then refused (see table.judge), and
// the request is source_disconnected.
func (s *Service) wait(ctx context.Context, e *entry) (judged, StepReason, error) {
	timer := time.NewTimer(time.Until(e.deadline))
	defer timer.Stop()
	var grace <-chan time.Time
	done, deadline, ctxDone := e.done, timer.C, ctx.Done()
	for {
		select {
		case j := <-e.result:
			if j.gone {
				return judged{}, ReasonSourceDisconnected, nil
			}
			return j, "", nil
		case <-done:
			done = nil
			if s.table.endUnanswered(e, false) {
				return judged{}, ReasonSourceDisconnected, nil
			}
		case <-deadline:
			deadline = nil
			if s.table.endUnanswered(e, true) {
				return judged{}, ReasonSourceTimeout, nil
			}
			// An answer claimed the request in time: its judgement is coming.
			g := time.NewTimer(claimGrace)
			defer g.Stop()
			grace = g.C
		case <-grace:
			grace = nil
			if s.table.endUnanswered(e, false) {
				return judged{}, ReasonSourceTimeout, nil
			}
		case <-ctxDone:
			ctxDone = nil
			if s.table.endUnanswered(e, false) {
				return judged{}, "", ctx.Err()
			}
		}
	}
}

// Claim is a request an answer has claimed: the transport reads the body
// and hands it to Judge, or refuses it with Refuse. Either ends the request.
type Claim struct {
	s *Service
	e *entry
}

// Claim judges the binding of an answer from a to request id, reading no
// body, and claims the request when it holds: the request is then consumed,
// whatever its body. The errors are ErrUnknownRequest, ErrNotStreamOwner,
// ErrAlreadyAnswered and ErrDeadlinePassed (ClaimStatus maps them).
func (s *Service) Claim(id string, a Answerer) (*Claim, error) {
	if !ValidID(id) {
		return nil, ErrUnknownRequest
	}
	e, err := s.table.claim(id, a)
	if err != nil {
		return nil, err
	}
	return &Claim{s: s, e: e}, nil
}

// Judge judges body as the answer to the claimed request and ends it:
// accepted (nil, nil) or refused under the rule returned. The refusal names
// a rule and a place, never a value. err is ErrUnknownRequest when the
// request ended while its body was on its way (its stream ended, its
// binding moved, its caller gave up): nothing is accepted then.
func (c *Claim) Judge(body []byte) (*Refusal, error) {
	a, refusal := ParseAnswer(body, c.e.req)
	if refusal != nil {
		if err := c.s.table.judge(c.e, judged{rule: refusal.Rule}); err != nil {
			return nil, err
		}
		return refusal, nil
	}
	if err := c.s.table.judge(c.e, judged{answer: a}); err != nil {
		return nil, err
	}
	return nil, nil
}

// Refuse ends the claimed request refused under rule, unread (a body over
// the limit) or unreadable (rule not_json). It does nothing once judged;
// ErrUnknownRequest, as for Judge, when the request ended meanwhile.
func (c *Claim) Refuse(rule Rule) error {
	return c.s.table.judge(c.e, judged{rule: rule})
}

// forUser names a named user in a log line; the implicit user is not named.
func forUser(user string) string {
	if user == "" {
		return ""
	}
	return fmt.Sprintf(" for user %q", user)
}
