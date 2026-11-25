package delivery

import (
	"context"
	"errors"
	"sync"
)

// Admission: the limits every request is held to, whichever source answers
// it, taken by Service.Request before the source is asked and released when
// the ask ends. One request in flight per shed (a second for the same shed
// id is refused at once, ErrShedPending) and maxActivePerUser per user (a
// request beyond them waits for a slot, within its own deadline). The
// machine source's answer claims (machine, generation, serial, stream) are
// not admission: they stay in the machine source's pending table.

// ErrShedPending is returned by Service.Request when a request for the same
// shed id is already in flight.
var ErrShedPending = errors.New("a delivery request for this shed is already waiting for its answer")

// maxActivePerUser is how many requests one user may have in flight at once.
// A var so tests can change it.
var maxActivePerUser = 8

// admission counts the requests in flight, by shed id and by user.
type admission struct {
	mu    sync.Mutex
	sheds map[string]bool
	users map[string]int
	// freed is closed, and replaced, whenever a slot is released.
	freed chan struct{}
}

func newAdmission() *admission {
	return &admission{sheds: map[string]bool{}, users: map[string]int{}, freed: make(chan struct{})}
}

// errUserFull is take's refusal of a request beyond the user's limit; the
// caller waits on the freed channel it returns with it.
var errUserFull = errors.New("the user has as many delivery requests waiting as they may")

// take admits one request for shedID and user. On errUserFull it returns the
// channel that is closed when a slot may have freed; on ErrShedPending the
// shed already has one. release gives the slots back, once.
func (a *admission) take(shedID, user string) (release func(), freed <-chan struct{}, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.sheds[shedID] {
		return nil, nil, ErrShedPending
	}
	if a.users[user] >= maxActivePerUser {
		return nil, a.freed, errUserFull
	}
	a.sheds[shedID] = true
	a.users[user]++
	var once sync.Once
	return func() { once.Do(func() { a.release(shedID, user) }) }, nil, nil
}

func (a *admission) release(shedID, user string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.sheds, shedID)
	if a.users[user]--; a.users[user] <= 0 {
		delete(a.users, user)
	}
	close(a.freed)
	a.freed = make(chan struct{})
}

// inFlight reports the slots held, for tests: the sheds and the requests of
// user.
func (a *admission) inFlight(user string) (sheds, ofUser int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.sheds), a.users[user]
}

// The ends a hosted ask is cut short by, besides its deadline and its
// caller's context: a stop or delete of its shed (Service.EndShed) and the
// server's shutdown (Service.Close). Each is the cause of the ask's
// context, so the ask can tell them from its deadline.
var (
	// ErrShedEnded ends a hosted request whose shed is being stopped or
	// deleted: the call is aborted, its slot released, and a reply that
	// comes later is discarded.
	ErrShedEnded = errors.New("the shed is being stopped or deleted: its delivery request was abandoned")
	// ErrServiceClosed ends a hosted request at the server's shutdown.
	ErrServiceClosed = errors.New("the server is shutting down: the delivery request was abandoned")
)

// cancels holds the cancel of every hosted request in flight, by shed name,
// so a stop or delete of the shed, or the shutdown, can end it.
type cancels struct {
	mu     sync.Mutex
	closed bool
	byShed map[string]map[*context.CancelCauseFunc]struct{}
	// ends counts, per shed name, the stops and deletes of it (end): a run
	// admitted before one (its Ticket names the count it saw) is ended as
	// soon as it registers, whenever that is.
	ends map[string]uint64
}

// Ticket is a run's admission for a shed: the shed's stop-and-delete count
// when the run was admitted (Service.Admit). A run whose shed has been
// stopped or deleted since its ticket was taken ends at once (ErrShedEnded)
// and never asks a source or writes.
type Ticket struct {
	name  string
	epoch uint64
	taken bool
}

// shedKey is what a shed's runs and its stops and deletes are matched by:
// its permanent id, which no other incarnation of the name ever has (a shed
// with none, from before delivery, by its name). A stale stop or delete of
// an old incarnation never reaches a replacement's run.
func shedKey(name, id string) string {
	if id != "" {
		return "id:" + id
	}
	return "name:" + name
}

// ticket is shed's admission now (shed is a shedKey).
func (c *cancels) ticket(shed string) Ticket {
	c.mu.Lock()
	defer c.mu.Unlock()
	return Ticket{name: shed, epoch: c.ends[shed], taken: true}
}

// watch derives a context from ctx that a stop or delete of shed, or the
// shutdown, ends with its cause. stop unregisters it and releases the
// context.
func (c *cancels) watch(ctx context.Context, shed string) (context.Context, func()) {
	return c.watchSince(ctx, Ticket{name: shed})
}

// watchSince is watch for a run admitted with t: a stop or delete of the
// shed since t was taken ends the context at once.
func (c *cancels) watchSince(ctx context.Context, t Ticket) (context.Context, func()) {
	shed := t.name
	wctx, cancel := context.WithCancelCause(ctx)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		cancel(ErrServiceClosed)
		return wctx, func() {}
	}
	if t.taken && c.ends[shed] != t.epoch {
		c.mu.Unlock()
		cancel(ErrShedEnded)
		return wctx, func() {}
	}
	if c.byShed == nil {
		c.byShed = map[string]map[*context.CancelCauseFunc]struct{}{}
	}
	if c.byShed[shed] == nil {
		c.byShed[shed] = map[*context.CancelCauseFunc]struct{}{}
	}
	key := &cancel
	c.byShed[shed][key] = struct{}{}
	c.mu.Unlock()
	return wctx, func() {
		c.mu.Lock()
		delete(c.byShed[shed], key)
		if len(c.byShed[shed]) == 0 {
			delete(c.byShed, shed)
		}
		c.mu.Unlock()
		cancel(context.Canceled)
	}
}

// end ends every request in flight for shed with cause.
func (c *cancels) end(shed string, cause error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ends == nil {
		c.ends = map[string]uint64{}
	}
	c.ends[shed]++
	for k := range c.byShed[shed] {
		(*k)(cause)
	}
}

// close ends every request in flight, and every later one at once.
func (c *cancels) close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	for _, set := range c.byShed {
		for k := range set {
			(*k)(ErrServiceClosed)
		}
	}
}
