package api

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/charliek/shed/internal/audit"
	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
)

// DefaultReapInterval is how often shed-server's max_age reaper runs.
const DefaultReapInterval = 60 * time.Second

// RunReaper deletes service-client sheds older than their client's
// policy.max_age, the only server-side lifetime bound on an owned shed. It
// runs one pass at once (so a shed that expired while the server was down goes
// on the first pass) and then one every interval, until ctx ends. now is the
// clock (time.Now in shed-server; a fake in tests).
//
// A pass lists every shed and, for each one whose owner is a configured client
// and whose age (now - created_at) is over that client's max_age, stops it if
// it is running and then deletes it, writing a reap line to the audit log.
// Sheds with no owner are never touched. A shed whose owner is no longer a
// configured client is left alone (and logged once): nothing bounds it any
// more, and removing a client must not delete what it left behind unseen.
func (s *Server) RunReaper(ctx context.Context, interval time.Duration, now func() time.Time) {
	if interval <= 0 {
		interval = DefaultReapInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		s.reapOnce(ctx, now())
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// StartReaper runs RunReaper on its own goroutine and returns stop, which
// ends its scheduling and waits for the pass in progress, a stop or delete
// included, to finish. shed-server calls stop at shutdown before it closes
// the audit log and the backend, so a reap that has begun completes (and
// records its line) rather than dying with the process. stop is idempotent.
func (s *Server) StartReaper(interval time.Duration, now func() time.Time) (stop func()) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.RunReaper(ctx, interval, now)
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			cancel()
			<-done
		})
	}
}

// reapOnce is one reaper pass at time now.
func (s *Server) reapOnce(ctx context.Context, now time.Time) {
	if ctx.Err() != nil {
		return
	}
	sheds, err := s.backend.ListSheds(ctx)
	if err != nil {
		log.Printf("reaper: list sheds: %v", err)
		return
	}
	for i := range sheds {
		sh := sheds[i]
		if sh.Owner == "" {
			continue
		}
		cc, ok := s.cfg.ServiceClient(sh.Owner)
		if !ok {
			if s.reapSkipped == nil {
				s.reapSkipped = map[string]bool{}
			}
			if !s.reapSkipped[sh.Name] {
				s.reapSkipped[sh.Name] = true
				log.Printf("reaper: shed %s is owned by %q, which is not a configured client; leaving it alone", sh.Name, sh.Owner)
			}
			continue
		}
		maxAge := cc.Policy.MaxAge.Duration()
		age := now.Sub(sh.CreatedAt)
		if maxAge <= 0 || age <= maxAge {
			continue
		}
		if ctx.Err() != nil {
			return
		}
		s.reap(ctx, sh, age)
	}
}

// reap stops (when running) and deletes one expired owned shed. Both run
// detached from the reaper's cancellation, so a shutdown can't leave a shed
// half torn down (StartReaper's stop waits for them), and with the shed's
// owner and creation time required, checked by the backend under its per-shed
// lock before each one: a shed deleted and recreated under the name since the
// listing (by anyone, its own client included, before the stop or between
// the stop and the delete) is a different, young shed, and is left alone.
func (s *Server) reap(ctx context.Context, sh config.Shed, age time.Duration) {
	opCtx := backend.WithRequiredCreatedAt(backend.WithRequiredOwner(context.WithoutCancel(ctx), sh.Owner), sh.CreatedAt)
	log.Printf("reaper: shed %s (owner %s) is %s old, over its client's max_age; deleting it", sh.Name, sh.Owner, age.Round(time.Second))
	// Not found: the listed shed is gone, deleted by someone else or
	// replaced by a new one under the name. Nothing is reaped, and nothing
	// recorded.
	gone := func(op string, err error) bool {
		if !errors.Is(err, config.ErrShedNotFoundSentinel) {
			return false
		}
		log.Printf("reaper: %s %s: the listed shed is gone or was recreated; leaving it: %v", op, sh.Name, err)
		return true
	}
	if sh.Status == config.StatusRunning {
		if _, err := s.backend.StopShed(opCtx, sh.Name); err != nil {
			if gone("stop", err) {
				return
			}
			// Delete still runs: it destroys a running shed too.
			log.Printf("reaper: stop %s: %v", sh.Name, err)
		}
	}
	ev := audit.ReapEvent{Event: audit.EventReap, Shed: sh.Name, Owner: sh.Owner, AgeSeconds: int64(age / time.Second)}
	if err := s.backend.DeleteShed(opCtx, sh.Name); err != nil {
		if gone("delete", err) {
			return
		}
		log.Printf("reaper: delete %s: %v", sh.Name, err)
		ev.Error = err.Error()
	}
	s.recordAudit(ev)
}
