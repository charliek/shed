package delivery

import (
	"errors"
	"net/http"
	"sync"
	"time"
)

// The pending-request table: every request the server has sent and not yet
// seen end, and the outcome of each that ended answered or expired, kept a
// while so a repeated post is told what became of it.
//
// A request is bound, when it is sent, to its user, the binding generation,
// the machine, the certificate serial, the one stream it went out on, and
// the shed's id. An answer is judged against that binding in the contract's
// order, reading no body until the request is claimed:
//
//	404 unknown_request   no request with the id waits for this user: never
//	                      made, another user's, ended without an answer
//	                      (its stream ended, the binding moved, the caller
//	                      gave up), or forgotten
//	403 not_stream_owner  another certificate, machine or stream
//	409 already_answered  claimed or answered already, accepted or refused
//	410 deadline_passed   past its deadline
//
// A claim consumes the request: from then on it ends accepted or refused,
// whatever the body turns out to be.
//
// The limits (one request in flight per shed, maxActivePerUser per user)
// are not the table's: they are the Service's admission (admission.go),
// shared by both sources and taken before a source is asked.

// The answer refusals of the table, in the order they are checked.
var (
	ErrUnknownRequest  = errors.New("no such delivery request is waiting for your answer")
	ErrNotStreamOwner  = errors.New("the answer did not come from the certificate, machine and stream the request went out on")
	ErrAlreadyAnswered = errors.New("the delivery request was answered already")
	ErrDeadlinePassed  = errors.New("the delivery request's deadline has passed")
)

// ClaimStatus is the status and code the transport answers a claim refusal
// with; ok is false for an error that is not one.
func ClaimStatus(err error) (status int, code string, ok bool) {
	switch {
	case errors.Is(err, ErrUnknownRequest):
		return http.StatusNotFound, CodeUnknownRequest, true
	case errors.Is(err, ErrNotStreamOwner):
		return http.StatusForbidden, CodeNotStreamOwner, true
	case errors.Is(err, ErrAlreadyAnswered):
		return http.StatusConflict, CodeAlreadyAnswered, true
	case errors.Is(err, ErrDeadlinePassed):
		return http.StatusGone, CodeDeadlinePassed, true
	}
	return 0, "", false
}

// maxRemembered bounds how many ended outcomes are kept, whatever their
// age. A var so tests can change it.
var maxRemembered = 4096

type entryState int

const (
	stateWaiting  entryState = iota // sent, not answered
	stateClaimed                    // an answer is being judged
	stateAccepted                   // ended: an answer was accepted
	stateRefused                    // ended: an answer was refused
	stateExpired                    // ended: the deadline passed unanswered
	stateGone                       // ended, forgotten: its stream ended, the binding moved, or its caller gave up
)

// judged is the judgement of a claimed request, handed to its waiter: the
// accepted answer, or the rule it broke.
type judged struct {
	answer *Answer
	rule   Rule
	// gone is set when the claim lost to the request's end: its stream
	// ended or its binding moved before the judgement could be had.
	gone bool
}

// entry is one request in the table. Every field but state, endedAt and
// active is fixed when it is made; those are guarded by the table's lock.
type entry struct {
	id       string
	req      *Request
	user     string
	machine  SourceMachine
	gen      uint64
	serial   string
	stream   string
	done     <-chan struct{}
	deadline time.Time

	state   entryState
	active  bool // waiting or claimed: not yet ended
	endedAt time.Time
	// result receives the judgement of a claim, once.
	result chan judged
}

type table struct {
	mu      sync.Mutex
	entries map[string]*entry
	// ended is the ids whose outcomes are remembered, oldest first.
	ended   []string
	current func(user, machine string, gen uint64) bool
	now     func() time.Time
}

func newTable(current func(user, machine string, gen uint64) bool, now func() time.Time) *table {
	return &table{
		entries: map[string]*entry{},
		current: current,
		now:     now,
	}
}

// add makes e waiting and active. The caller holds e's admission (the
// shed's and the user's slots).
func (t *table) add(e *entry) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.sweepLocked()
	if _, taken := t.entries[e.id]; taken {
		return errors.New("delivery: request id collision")
	}
	e.state, e.active = stateWaiting, true
	e.result = make(chan judged, 1)
	t.entries[e.id] = e
	return nil
}

// releaseLocked marks e ended.
func (t *table) releaseLocked(e *entry) { e.active = false }

// rememberLocked ends e in a remembered state.
func (t *table) rememberLocked(e *entry, st entryState) {
	e.state, e.endedAt = st, t.now()
	t.releaseLocked(e)
	t.ended = append(t.ended, e.id)
	t.sweepLocked()
}

// forgetLocked ends e unanswered and forgets it: an answer to it is
// unknown_request.
func (t *table) forgetLocked(e *entry) {
	e.state = stateGone
	t.releaseLocked(e)
	if t.entries[e.id] == e {
		delete(t.entries, e.id)
	}
}

// sweepLocked forgets ended outcomes older than RememberAnswered, and the
// oldest beyond maxRemembered.
func (t *table) sweepLocked() {
	cutoff := t.now().Add(-RememberAnswered)
	n := 0
	for _, id := range t.ended {
		e := t.entries[id]
		if e == nil || e.active {
			n++
			continue
		}
		if len(t.ended)-n > maxRemembered || e.endedAt.Before(cutoff) {
			delete(t.entries, id)
			n++
			continue
		}
		break
	}
	t.ended = t.ended[n:]
}

// closed reports whether ch is closed.
func closed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// goneLocked reports whether waiting e has ended without an answer: its
// stream ended, or its stamp is no longer the user's binding (a move).
func (t *table) goneLocked(e *entry) bool {
	return closed(e.done) || !t.current(e.user, e.machine.MachineID, e.gen)
}

// live reports whether request id is still to be sent: waiting, its stream
// and binding current, before its deadline.
func (t *table) live(id string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	e := t.entries[id]
	return e != nil && e.state == stateWaiting && !t.goneLocked(e) && !t.now().After(e.deadline)
}

// claim judges an answer's binding to request id (see the note at the top)
// and, when every check passes, claims the request for a. No body has been
// read.
func (t *table) claim(id string, a Answerer) (*entry, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.sweepLocked()
	e := t.entries[id]
	if e == nil || e.user != a.User {
		return nil, ErrUnknownRequest
	}
	if e.state == stateWaiting && t.goneLocked(e) {
		t.forgetLocked(e)
		return nil, ErrUnknownRequest
	}
	if a.Machine != e.machine.MachineID || a.Serial != e.serial || a.Stream == "" || a.Stream != e.stream {
		return nil, ErrNotStreamOwner
	}
	switch e.state {
	case stateClaimed, stateAccepted, stateRefused:
		return nil, ErrAlreadyAnswered
	case stateExpired:
		return nil, ErrDeadlinePassed
	}
	if t.now().After(e.deadline) {
		t.rememberLocked(e, stateExpired)
		return nil, ErrDeadlinePassed
	}
	e.state = stateClaimed
	return e, nil
}

// judge ends claimed e with its judgement and hands it to the waiter,
// atomically with the request's end: under the table's lock it checks
// again that e's stream has not ended and its stamp is still the user's
// binding. When either changed since the claim (the stream ended, the
// binding moved), or the request has ended meanwhile (its stream ended
// while the body was read, its caller gave up), nothing is accepted: e is
// forgotten, its waiter told it is gone, and ErrUnknownRequest returned
// for the answer's poster.
func (t *table) judge(e *entry, j judged) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if e.state != stateClaimed {
		return ErrUnknownRequest
	}
	if t.goneLocked(e) {
		t.forgetLocked(e)
		e.result <- judged{gone: true}
		return ErrUnknownRequest
	}
	st := stateAccepted
	if j.answer == nil {
		st = stateRefused
	}
	t.rememberLocked(e, st)
	e.result <- j
	return nil
}

// endUnanswered ends e without an accepted judgement: at its deadline
// (expired true) or because its stream ended, its binding moved or its
// caller gave up (expired false). It reports false when e's judgement has
// come, or (at the deadline only) is coming: an answer claimed it in time.
// A stream's end or a move fails a claimed request too, whose body may
// still be on its way: its judge then finds it gone. Otherwise e has ended
// unanswered, now or before.
func (t *table) endUnanswered(e *entry, expired bool) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	switch {
	case e.state == stateAccepted || e.state == stateRefused:
		return false
	case e.state == stateClaimed && expired:
		return false
	case e.state == stateGone || e.state == stateExpired:
		return true
	case expired:
		t.rememberLocked(e, stateExpired)
	default:
		t.forgetLocked(e)
	}
	return true
}
