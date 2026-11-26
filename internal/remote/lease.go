package remote

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/delivery"
	"github.com/charliek/shed/internal/remote/provider"
	"github.com/charliek/shed/internal/remote/state"
)

// A cloud shed's lease (plan 041 §3.4): the account service deletes the
// machine at its lease's end; a person moves the end later with an extend,
// never past the cap. The server keeps no timer of its own.

// usageTTL is how long a usage answer is kept for its user.
const usageTTL = 30 * time.Second

// usageEntry is a kept usage answer.
type usageEntry struct {
	at    time.Time
	usage config.CloudUsage
}

// ExtendShed moves the shed's lease later by hours (backend.Extender).
//
// requestID is the compute contract's request id to send: the caller's
// (derived by the access guard from the id the caller made, so a retried
// request moves the lease once), or, when empty, a new one: each such call
// is a new extend. Either way it is written to the record, durably, before
// the extend is sent (a write that cannot be flushed sends nothing: 503
// COMPUTE_UNAVAILABLE), and a lost reply is sent again with it. A refusal is
// the service's, mapped by its code: 403 EXTEND_NOT_ALLOWED, 409
// LEASE_AT_CAP, 409 LEASE_ENDED, 403 PERSON_INACTIVE, 409 COMPUTE_DELETING,
// 503 COMPUTE_UNAVAILABLE. The whole call (the wait for another extend of
// the shed, the sends, any wait a 429's Retry-After asks for, and the read
// after) is bounded by the fast-fail window (Options.FastFailWindow).
//
// A request id the record still holds from an earlier call (its outcome
// unknown: every reply was lost) keeps that unknown outcome when this call
// is refused before the service judged it (not acted on, refused at the
// scope): the extend may have landed, so the answer is 503 with the id kept
// for the caller to send again, never a refusal that clears it.
func (b *Backend) ExtendShed(ctx context.Context, name string, hours int, requestID string) (*config.Shed, error) {
	if err := b.checkReady(); err != nil {
		return nil, err
	}
	if hours < 1 || hours > config.MaxExtendHours {
		return nil, fmt.Errorf("%w: hours must be 1 to %d", config.ErrInvalidShedRequestSentinel, config.MaxExtendHours)
	}
	if requestID != "" && !config.ValidUUID(requestID) {
		return nil, fmt.Errorf("%w: request id %q is not a UUID", config.ErrInvalidShedRequestSentinel, requestID)
	}
	// The budget starts here, before the wait for another extend of the
	// shed: a Retry-After that would run past it is not waited for, and
	// the extend, which the service did not act on, is answered 503
	// COMPUTE_UNAVAILABLE at once.
	cctx, cancel := context.WithTimeout(ctx, b.opts.FastFailWindow)
	defer cancel()
	b.mu.Lock()
	r, err := b.lookupLocked(ctx, name)
	if err != nil {
		b.mu.Unlock()
		return nil, err
	}
	ref, id := r.Ref, r.Shed.ID
	b.mu.Unlock()
	if ref == "" {
		// The create has not been accepted yet: there is no machine and
		// no lease to move.
		return nil, fmt.Errorf("%w: shed %s has no machine yet; try again once it is starting", config.ErrProvisionBusySentinel, name)
	}

	release, err := b.extending.acquire(cctx, id)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, computeUnavailable(fmt.Errorf("another extend of shed %s is still waiting for the account service; try again", name))
	}
	defer release()
	rid := strings.ToLower(requestID)
	if rid == "" {
		rid = provider.NewRequestID()
	}
	var prev *state.ExtendRequest
	err = b.updateDurable(name, id, func(r *state.Record) {
		prev = r.Extend
		r.Extend = &state.ExtendRequest{Ref: ref, RequestID: rid, Hours: hours, At: b.now().UTC()}
	})
	switch {
	case errors.Is(err, errSuperseded):
		return nil, fmt.Errorf("%w: %s", config.ErrShedNotFoundSentinel, name)
	case errors.Is(err, state.ErrNotDurable):
		// In place but not flushed: a power cut could lose it. Nothing
		// is sent; the caller tries again.
		b.update(name, id, func(r *state.Record) { r.Extend = prev })
		return nil, computeUnavailable(fmt.Errorf("recording the extend's request id: %w", err))
	case err != nil:
		return nil, err
	}

	// The record held this id from an earlier call whose outcome is
	// unknown (the id is cleared otherwise).
	priorUnknown := prev != nil && prev.RequestID == rid && prev.Ref == ref
	var inst provider.Instance
	err = b.retryUnknown(cctx, rid, func(rid string) error {
		var cerr error
		inst, cerr = b.prov.Extend(cctx, ref, rid, time.Duration(hours)*time.Hour)
		return cerr
	})
	// The answer to a repeated request id is the first send's, as it was
	// then: an extend sent again after another extend landed answers with
	// a lease older than the machine's. So the machine is read afresh, and
	// only a view no older than the record's is kept (notOlder).
	if err == nil {
		if fresh, gerr := b.prov.Get(cctx, ref); gerr == nil {
			inst = fresh
		}
	}
	if err != nil && priorUnknown && notSent(err) {
		// Refused before the service judged it, which says nothing about
		// the earlier send of this id: its outcome stays unknown, as
		// retryUnknown keeps it across the attempts of one call.
		err = &provider.Error{Kind: provider.ErrUnavailable, Op: "extend",
			Detail: "an earlier send of this request id may have been acted on, and this one was refused before the service judged it (" + err.Error() + "); the outcome is unknown: send it again with the same request id"}
	}
	// The id stays on the record while its outcome is unknown (a lost
	// reply): the caller who sent its own id sends it again.
	keep := err != nil && unknownOutcome(err)
	var shed *config.Shed
	b.mu.Lock()
	if cur := b.currentLocked(name, id); cur != nil {
		changed := false
		if !keep && cur.Extend != nil && cur.Extend.RequestID == rid {
			cur.Extend = nil
			changed = true
		}
		if err == nil && inst.Ref == ref && notOlder(inst, cur.Machine) {
			// noteMachineLocked holds to notOlder too; asked here so the
			// record is saved only when it took the view.
			b.noteMachineLocked(cur, inst)
			changed = true
		}
		if changed {
			b.saveLocked(cur)
		}
		shed = b.shedOf(cur)
	}
	b.mu.Unlock()
	if err != nil {
		b.logf("remote: extend of shed %s by %dh: %v", name, hours, err)
		return nil, mapRefusal(err)
	}
	if shed == nil {
		return nil, fmt.Errorf("%w: %s", config.ErrShedNotFoundSentinel, name)
	}
	return shed, nil
}

// CloudUsage reports the machines held against the account service's
// limits (backend.CloudUsager): the organization's, and user's when user is
// a person of the service. Anyone else (a local admin) is never named to
// the service, and gets the organization's only. Answers are kept 30
// seconds per user.
func (b *Backend) CloudUsage(ctx context.Context, user string) (config.CloudUsage, error) {
	if user != "" && !b.ServicePerson(user) {
		user = ""
	}
	now := b.now()
	b.usageMu.Lock()
	if e, ok := b.usage[user]; ok && now.Sub(e.at) < usageTTL && !now.Before(e.at) {
		b.usageMu.Unlock()
		return e.usage, nil
	}
	b.usageMu.Unlock()

	u, err := b.prov.Usage(ctx, user)
	if err != nil {
		return config.CloudUsage{}, mapRefusal(err)
	}
	out := config.CloudUsage{Org: config.CloudCounts(u.Org)}
	if u.Person != nil {
		pc := config.CloudCounts(*u.Person)
		out.Person = &pc
	}
	b.usageMu.Lock()
	if b.usage == nil {
		b.usage = map[string]usageEntry{}
	}
	b.usage[user] = usageEntry{at: now, usage: out}
	b.usageMu.Unlock()
	return out, nil
}

// ServicePerson reports whether name is a person of the account service
// (backend.CloudUsager): a valid handle whose origin is the directory. On a
// test provider that does not require an owner every valid handle is one.
func (b *Backend) ServicePerson(name string) bool {
	if !provider.ValidHandle(name) {
		return false
	}
	return !b.ownerRequired() || b.deliverySvc.Origin(name) == delivery.OriginDirectory
}

// ownerRequired says a machine is made only for a person or a client: on a
// server whose provider is the account service, or where a test provider's
// catalog asks for it.
func (b *Backend) ownerRequired() bool {
	if b.cfg.Provider == config.RemoteProviderService {
		return true
	}
	cat, _ := b.Catalog()
	return cat.OwnerRequired
}

// notOlder reports whether a machine view is no older than the one the
// record holds (nil: none): its lease ends no earlier (a lease only moves
// later) and its last start or stop is no earlier (stopped_at is cleared by
// a start, so the later of the two is the machine's last transition). A
// replayed extend's answer, older than a later extend, fails it and is not
// kept.
func notOlder(inst provider.Instance, held *provider.Instance) bool {
	if held == nil {
		return true
	}
	return !inst.Lease.ExpiresAt.Before(held.Lease.ExpiresAt) && !lastTransition(inst).Before(lastTransition(*held))
}

// lastTransition is the later of a machine's last start and last stop.
func lastTransition(m provider.Instance) time.Time {
	if m.StoppedAt.After(m.StartedAt) {
		return m.StoppedAt
	}
	return m.StartedAt
}

// extendSlots holds one extend per shed (by shed id) at a time. Unlike a
// mutex, a caller waits for its turn only while its context allows: the
// wait is part of the extend's budget. The zero value is ready to use.
type extendSlots struct {
	mu    sync.Mutex
	slots map[string]chan struct{}
}

// acquire takes the shed's slot, or returns ctx's error when ctx ends
// first. The caller calls the returned release once done.
func (s *extendSlots) acquire(ctx context.Context, id string) (release func(), err error) {
	s.mu.Lock()
	if s.slots == nil {
		s.slots = make(map[string]chan struct{})
	}
	slot := s.slots[id]
	if slot == nil {
		slot = make(chan struct{}, 1)
		s.slots[id] = slot
	}
	s.mu.Unlock()
	select {
	case slot <- struct{}{}:
		return func() { <-slot }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
