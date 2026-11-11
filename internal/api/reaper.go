package api

import (
	"context"
	"log"
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
// half torn down, and with the shed's owner required, so the backend refuses
// a shed that was deleted and recreated by someone else since the listing.
func (s *Server) reap(ctx context.Context, sh config.Shed, age time.Duration) {
	opCtx := backend.WithRequiredOwner(context.WithoutCancel(ctx), sh.Owner)
	log.Printf("reaper: shed %s (owner %s) is %s old, over its client's max_age; deleting it", sh.Name, sh.Owner, age.Round(time.Second))
	if sh.Status == config.StatusRunning {
		if _, err := s.backend.StopShed(opCtx, sh.Name); err != nil {
			// Delete still runs: it destroys a running shed too.
			log.Printf("reaper: stop %s: %v", sh.Name, err)
		}
	}
	ev := audit.ReapEvent{Event: audit.EventReap, Shed: sh.Name, Owner: sh.Owner, AgeSeconds: int64(age / time.Second)}
	if err := s.backend.DeleteShed(opCtx, sh.Name); err != nil {
		log.Printf("reaper: delete %s: %v", sh.Name, err)
		ev.Error = err.Error()
	}
	s.recordAudit(ev)
}
