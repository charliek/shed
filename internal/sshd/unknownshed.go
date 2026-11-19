package sshd

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/users"
)

// An unknown key offered for a login to a shed name, on a lookup that can
// nudge (users.UnknownKeyNudger: a directory's list). The login is refused at
// once, whether a shed of that name exists or not, and never waits for
// anything about the name: the check is started on a goroutine of its own and
// the refusal is returned without waiting for it (which of the two the
// scheduler runs first is not fixed). Off the login path, the server checks
// whether the shed exists and, only when it does, nudges the lookup to pull
// in the background on a slot of its own. The person's next attempt, seconds
// later, finds the key known. A name that is no shed starts no pull, so a
// scanner trying root, admin or ubuntu costs the account service nothing
// unless a shed has that name.
//
// The check runs on a goroutine owned by the server, under the server's
// lifetime context (never the connection's, which ends when the refused
// client hangs up), as the server itself (backend.SystemPrincipal), through
// the access guard, within shedCheckTimeout. At most one runs at a time: an
// offer that arrives while one runs is refused with no check. A result that
// arrives after its deadline asks for no pull, and no nudge is asked once
// Shutdown has begun: the nudge is made under nudgeMu, which Shutdown takes to
// end the lifetime, after a check of the lifetime under it.
const (
	// defaultShedCheckTimeout bounds one check of whether a shed exists.
	defaultShedCheckTimeout = 2 * time.Second
	// unknownShedStatsEvery is how often the counts of shed-name offers are
	// logged (when any is not zero).
	unknownShedStatsEvery = time.Hour
)

// unknownShedStats counts the unknown keys offered as a shed name since the
// counts were last logged.
type unknownShedStats struct {
	// noShed: the check found no shed of that name (or the server has no
	// backend). existing: it found one; pulled of those asked for a pull.
	noShed, existing, pulled int
	// skipped: refused with no check, while another check ran.
	skipped int
	// failed: the check failed or passed its deadline; firstErr is the first
	// such failure's text since the counts were last logged.
	failed   int
	firstErr string
}

// zero reports whether nothing was counted.
func (st unknownShedStats) zero() bool {
	return st.noShed == 0 && st.existing == 0 && st.skipped == 0 && st.failed == 0
}

// line is the hourly log line of st.
func (st unknownShedStats) line() string {
	failed := fmt.Sprintf("%d checks failed", st.failed)
	if st.failed > 0 {
		failed += ": " + st.firstErr
	}
	return fmt.Sprintf("directory: in the last hour %d unknown keys were offered as a name that is no shed here and %d as an existing shed's name (%d started a pull, %d skipped while a check ran, %s)",
		st.noShed, st.existing, st.pulled, st.skipped, failed)
}

// nudgeIfShed starts the off-path check for an unknown key (fingerprint)
// offered for the shed name name, and returns at once: the login is refused
// whatever the check finds.
func (s *Server) nudgeIfShed(n users.UnknownKeyNudger, name, fingerprint string) {
	if s.backend == nil {
		// A server built with no backend (tests): no name is a shed.
		s.countUnknownShed(func(st *unknownShedStats) { st.noShed++ })
		return
	}
	s.checkMu.Lock()
	if s.life.Err() != nil {
		s.checkMu.Unlock()
		return
	}
	// Taken here and released only when the backend call returns, even past
	// its deadline, so a backend that ignores cancellation never has a second
	// check started beside the first.
	if !s.checking.CompareAndSwap(false, true) {
		s.checkMu.Unlock()
		s.countUnknownShed(func(st *unknownShedStats) { st.skipped++ })
		return
	}
	done := make(chan struct{})
	s.checkDone = done
	s.checkMu.Unlock()
	go func() {
		defer close(done)
		defer s.checking.Store(false)
		s.checkShed(n, name, fingerprint)
	}()
}

// checkShed is the check itself: whether a shed named name exists, asked as
// the server, and the nudge when it does.
func (s *Server) checkShed(n users.UnknownKeyNudger, name, fingerprint string) {
	ctx, cancel := context.WithTimeout(backend.WithPrincipal(s.life, backend.SystemPrincipal()), s.shedCheckTimeout)
	defer cancel()
	_, err := s.guard().GetShed(ctx, name)
	switch {
	case s.life.Err() != nil:
		// Shutdown has begun: nothing more is asked, or counted.
	case ctx.Err() != nil:
		// The answer came after the deadline: no pull.
		s.countUnknownShed(func(st *unknownShedStats) {
			st.failed++
			if st.firstErr == "" {
				st.firstErr = fmt.Sprintf("no answer within %s", s.shedCheckTimeout)
			}
		})
	case errors.Is(err, config.ErrShedNotFoundSentinel):
		s.countUnknownShed(func(st *unknownShedStats) { st.noShed++ })
	case err != nil:
		s.countUnknownShed(func(st *unknownShedStats) {
			st.failed++
			if st.firstErr == "" {
				st.firstErr = err.Error()
			}
		})
	default:
		if s.afterShedFound != nil {
			s.afterShedFound()
		}
		pulled, alive := s.nudgeWhileAlive(n, fingerprint)
		if !alive {
			// Shutdown began after the backend answered: nothing is asked, or
			// counted.
			return
		}
		s.countUnknownShed(func(st *unknownShedStats) {
			st.existing++
			if pulled {
				st.pulled++
			}
		})
	}
}

// nudgeWhileAlive asks n for a pull for fingerprint unless the server's
// lifetime has ended, and reports whether the nudge asked for one (pulled)
// and whether it was made at all (alive). The check of the lifetime and the
// nudge are made together under nudgeMu, which endLife holds while it ends
// the lifetime: a nudge is either over before Shutdown ends the lifetime or
// never made. NudgeForUnknownKey does no network work and waits for no pull.
func (s *Server) nudgeWhileAlive(n users.UnknownKeyNudger, fingerprint string) (pulled, alive bool) {
	s.nudgeMu.Lock()
	defer s.nudgeMu.Unlock()
	if s.life.Err() != nil {
		return false, false
	}
	return n.NudgeForUnknownKey(fingerprint), true
}

// endLife ends the server's lifetime, once no nudge is being made (nudgeMu)
// and no check is being started (checkMu): from then on no check starts and
// no nudge is asked.
func (s *Server) endLife() {
	s.nudgeMu.Lock()
	defer s.nudgeMu.Unlock()
	s.checkMu.Lock()
	defer s.checkMu.Unlock()
	s.lifeCancel()
}

// countUnknownShed records one shed-name offer in the counts, and starts the
// hourly log of them the first time.
func (s *Server) countUnknownShed(add func(*unknownShedStats)) {
	s.statsMu.Lock()
	add(&s.stats)
	s.statsMu.Unlock()
	s.statsOnce.Do(func() {
		if s.life.Err() != nil {
			return
		}
		go s.logUnknownShedStats()
	})
}

// logUnknownShedStats logs the counts every unknownShedStatsEvery, until the
// server's lifetime ends.
func (s *Server) logUnknownShedStats() {
	t := time.NewTicker(s.statsEvery)
	defer t.Stop()
	for {
		select {
		case <-s.life.Done():
			return
		case <-t.C:
			s.flushUnknownShedStats()
		}
	}
}

// flushUnknownShedStats logs the counts, when any is not zero, and starts
// them again from zero.
func (s *Server) flushUnknownShedStats() {
	s.statsMu.Lock()
	st := s.stats
	s.stats = unknownShedStats{}
	s.statsMu.Unlock()
	if !st.zero() {
		log.Print(st.line())
	}
}

// unknownShedCounts is the counts as they are now (for tests).
func (s *Server) unknownShedCounts() unknownShedStats {
	s.statsMu.Lock()
	defer s.statsMu.Unlock()
	return s.stats
}

// stopShedCheck is Shutdown's part: it ends the server's lifetime, so no
// check starts and none asks for a pull from then on, and waits for a check
// still running until ctx ends. A check still blocked in the backend then is
// left to finish, and does nothing.
func (s *Server) stopShedCheck(ctx context.Context) error {
	s.endLife()
	s.checkMu.Lock()
	done := s.checkDone
	s.checkMu.Unlock()
	if done == nil {
		return nil
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
