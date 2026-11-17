package directory

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charliek/shed/internal/users"
)

// The schedule and the runtime's one outstanding Retry-After: no scheduled
// pull reaches the service before the Retry-After has elapsed, measured as
// elapsed time, whatever an admin's refresh in between got and whatever the
// wall clock was set to; only an admin's refresh pulls inside it, and a good
// one clears it.

// schedProbe is a runtime whose schedule runs, on the fake clock, with each
// attempt (afterAttempt) and each return of the schedule to waiting
// (beforeWait) seen by the test.
type schedProbe struct {
	t        *testing.T
	clock    *fakeClock
	svc      *rtService
	srv      *rtServer
	attempts chan users.DirectoryRefresh
	waits    chan schedWait
}

// schedWait is one return of the schedule to waiting: the clock it read and
// the next attempt it found.
type schedWait struct{ now, next time.Time }

// startScheduled starts a runtime enrolled with a service listing ada, and
// its schedule, whose first pull (at rtT0) is good.
func startScheduled(t *testing.T) *schedProbe {
	t.Helper()
	clock := newFakeClock(rtT0)
	svc := newRTService(t, clock)
	dir := filepath.Join(t.TempDir(), "directory")
	svc.enrol(dir)
	opsLine, _ := newTestKey(t)
	svc.dir.setUsers(newPerson(t, "ada", false).served())
	p := &schedProbe{t: t, clock: clock, svc: svc,
		attempts: make(chan users.DirectoryRefresh, 64), waits: make(chan schedWait, 1024)}
	p.srv = startRT(t, svc, dir, opsLine, func(rt *Runtime) {
		rt.afterAttempt = func(row users.DirectoryRefresh) { p.attempts <- row }
		rt.beforeWait = func(now, next time.Time) { p.waits <- schedWait{now, next} }
	})
	p.srv.start()
	if row := p.next(); row.Status != users.DirectoryRefreshGood {
		t.Fatalf("first pull: %+v", row)
	}
	p.idleAt(rtT0)
	for len(svc.arrived) > 0 {
		<-svc.arrived // the first pull's
	}
	return p
}

// idleAt waits until the schedule has read the clock at at and gone back to
// waiting, and returns the next attempt it found then.
func (p *schedProbe) idleAt(at time.Time) time.Time {
	p.t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case w := <-p.waits:
			if w.now.Equal(at) {
				return w.next
			}
		case <-deadline:
			p.t.Fatalf("the schedule did not wait at %s", stamp(at))
		}
	}
}

// setIdle moves the clock to at and waits for the schedule to look at it; it
// returns the next attempt the schedule found.
func (p *schedProbe) setIdle(at time.Time) time.Time {
	p.t.Helper()
	p.clock.set(at)
	return p.idleAt(at)
}

// next is the next attempt's row.
func (p *schedProbe) next() users.DirectoryRefresh {
	p.t.Helper()
	select {
	case row := <-p.attempts:
		return row
	case <-time.After(10 * time.Second):
		p.t.Fatal("no pull")
	}
	return users.DirectoryRefresh{}
}

// none fails the test on an attempt not yet seen.
func (p *schedProbe) none(what string) {
	p.t.Helper()
	select {
	case row := <-p.attempts:
		p.t.Fatalf("%s: an unexpected pull %+v", what, row)
	default:
	}
}

// wantNext checks the next attempt status shows.
func (p *schedProbe) wantNext(want time.Time) {
	p.t.Helper()
	if got := p.srv.rt.Status().NextAttempt; !got.Equal(want) {
		p.t.Fatalf("next attempt %s, want %s", stamp(got), stamp(want))
	}
}

// failHour has the scheduled pull at rtT0 + rtRefresh answered 429 with an
// hour's Retry-After, and returns when that was.
func (p *schedProbe) failHour() time.Time {
	p.t.Helper()
	p.svc.setMode(fault429, "3600")
	failAt := rtT0.Add(rtRefresh)
	p.clock.set(failAt)
	if row := p.next(); row.Status != users.DirectoryRefreshFailed || !strings.Contains(row.Error, "rate-limited") {
		p.t.Fatalf("the scheduled pull: %+v", row)
	}
	p.wantNext(failAt.Add(time.Hour))
	p.idleAt(failAt)
	return failAt
}

// notSentLines is how many times the schedule's pull was refused at its
// start for a pending Retry-After.
func (p *schedProbe) notSentLines() int {
	return p.srv.log.count("directory: the scheduled pull is not sent")
}

// TestScheduleKeepsTheOutstandingRetryAfter: after a 429 with an hour's
// Retry-After, an admin's refresh five minutes in pulls (it may) and fails
// with a 503 that carries no Retry-After; the next scheduled attempt stays at
// the hour's end, and no scheduled request reaches the service before it.
func TestScheduleKeepsTheOutstandingRetryAfter(t *testing.T) {
	p := startScheduled(t)
	failAt := p.failHour()

	p.svc.setMode(faultRetryAfter, "")
	p.setIdle(failAt.Add(5 * time.Minute))
	before := p.svc.directoryCount()
	if row := p.srv.refresh(t); row.Status != users.DirectoryRefreshFailed || !strings.Contains(row.Error, "503") {
		t.Fatalf("admin refresh: %+v", row)
	}
	p.next()
	if n := p.svc.directoryCount() - before; n != 1 {
		t.Fatalf("the admin's refresh inside the Retry-After sent %d directory requests, want 1", n)
	}
	p.wantNext(failAt.Add(time.Hour))

	sent := p.svc.sent()
	for _, at := range []time.Duration{20 * time.Minute, 30 * time.Minute, time.Hour - time.Second} {
		p.setIdle(failAt.Add(at))
		p.none(stamp(failAt.Add(at)) + ", inside the hour")
		p.wantNext(failAt.Add(time.Hour))
	}
	if n := p.svc.sent() - sent; n != 0 {
		t.Fatalf("%d scheduled requests reached the service inside the Retry-After", n)
	}
	if n := p.notSentLines(); n != 0 {
		t.Fatalf("the schedule started %d pulls inside the Retry-After (refused at their start):\n%s", n, p.srv.log)
	}
	p.svc.setMode(faultNone, "")
	p.clock.set(failAt.Add(time.Hour))
	if row := p.next(); row.Status != users.DirectoryRefreshGood {
		t.Fatalf("the scheduled pull at the hour's end: %+v", row)
	}
	p.wantNext(failAt.Add(time.Hour + rtRefresh))
}

// TestScheduleRetryAfterIsElapsedTime: a wall clock set forward while the
// Retry-After runs does not bring a scheduled pull inside it: the attempt
// that would start is put off until the Retry-After has elapsed, and status
// says when that is.
func TestScheduleRetryAfterIsElapsedTime(t *testing.T) {
	p := startScheduled(t)
	failAt := p.failHour()
	p.setIdle(failAt.Add(10 * time.Minute))

	// The wall clock is set 90 minutes forward, past the hour's end on the
	// wall clock; then the schedule's timer fires a minute on: 11 minutes
	// have elapsed, 49 remain.
	p.clock.stepWall(90 * time.Minute)
	at := failAt.Add(101 * time.Minute)
	p.clock.set(at)
	p.idleAt(at)
	p.none("the wall clock set forward past the hour, 11 minutes elapsed")
	p.wantNext(at.Add(49 * time.Minute))
	sent := p.svc.sent()
	p.setIdle(at.Add(49*time.Minute - time.Second))
	p.none("a second before the Retry-After has elapsed")
	if n := p.svc.sent() - sent; n != 0 {
		t.Fatalf("%d scheduled requests reached the service inside the Retry-After", n)
	}
	if n := p.notSentLines(); n != 0 {
		t.Fatalf("the schedule started %d pulls inside the Retry-After (refused at their start):\n%s", n, p.srv.log)
	}
	p.svc.setMode(faultNone, "")
	p.clock.set(at.Add(49 * time.Minute))
	if row := p.next(); row.Status != users.DirectoryRefreshGood {
		t.Fatalf("the scheduled pull once the hour has elapsed: %+v", row)
	}
}

// TestScheduleRetryAfterDoesNotRatchet: the wall clock set forward while an
// hour's Retry-After runs, the schedule looking at it, then the clock
// corrected back: the scheduled pull goes out when the hour has elapsed, not
// later. The schedule's own next attempt and the Retry-After are kept apart,
// and the next attempt computed from them is never written back; status
// shows the instant the schedule uses.
func TestScheduleRetryAfterDoesNotRatchet(t *testing.T) {
	p := startScheduled(t)
	failAt := p.failHour()
	p.setIdle(failAt.Add(10 * time.Minute))

	// Ten minutes in, the wall clock is set 30 minutes forward; the
	// schedule's timer fires a minute on: 11 minutes have elapsed, 49 remain.
	p.clock.stepWall(30 * time.Minute)
	at := failAt.Add(41 * time.Minute)
	if got := p.setIdle(at); !got.Equal(at.Add(49 * time.Minute)) {
		t.Fatalf("the schedule's next attempt with the clock forward: %s, want %s", stamp(got), stamp(at.Add(49*time.Minute)))
	}
	p.wantNext(at.Add(49 * time.Minute))

	// The clock is corrected back: the hour ends where it always did.
	p.clock.stepWall(-30 * time.Minute)
	p.wantNext(failAt.Add(time.Hour))
	look := failAt.Add(42 * time.Minute)
	if got := p.setIdle(look); !got.Equal(failAt.Add(time.Hour)) {
		t.Fatalf("the schedule's next attempt once the clock is corrected: %s, want %s", stamp(got), stamp(failAt.Add(time.Hour)))
	}
	p.wantNext(failAt.Add(time.Hour))
	sent := p.svc.sent()
	p.setIdle(failAt.Add(time.Hour - time.Second))
	p.none("a second before the Retry-After has elapsed")
	if n := p.svc.sent() - sent; n != 0 {
		t.Fatalf("%d scheduled requests reached the service inside the Retry-After", n)
	}
	p.svc.setMode(faultNone, "")
	p.clock.set(failAt.Add(time.Hour))
	if row := p.next(); row.Status != users.DirectoryRefreshGood {
		t.Fatalf("the scheduled pull once the hour has elapsed: %+v", row)
	}
}

// TestScheduleGoodAdminRefreshClearsRetryAfter: a good admin's refresh inside
// the Retry-After clears it, and the schedule goes back to the last good pull
// plus refresh.
func TestScheduleGoodAdminRefreshClearsRetryAfter(t *testing.T) {
	p := startScheduled(t)
	failAt := p.failHour()

	p.svc.setMode(faultNone, "")
	goodAt := failAt.Add(10 * time.Minute)
	p.setIdle(goodAt)
	if row := p.srv.refresh(t); row.Status != users.DirectoryRefreshGood {
		t.Fatalf("admin refresh: %+v", row)
	}
	p.next()
	p.wantNext(goodAt.Add(rtRefresh))
	p.srv.rt.mu.Lock()
	cleared := p.srv.rt.retryEnd.IsZero()
	p.srv.rt.mu.Unlock()
	if !cleared {
		t.Fatal("a good pull left the Retry-After pending")
	}
	p.setIdle(failAt.Add(time.Hour))
	p.none("at the end of the Retry-After the good refresh cleared")
	p.clock.set(goodAt.Add(rtRefresh))
	if row := p.next(); row.Status != users.DirectoryRefreshGood {
		t.Fatalf("the scheduled pull after the admin's: %+v", row)
	}
}

// TestScheduleQueuedPullCheckedAtStart: the schedule's pull, queued behind an
// admin's refresh that is then answered with a Retry-After, is not sent when
// its turn comes; the schedule waits for the Retry-After's end.
func TestScheduleQueuedPullCheckedAtStart(t *testing.T) {
	p := startScheduled(t)
	dueAt := rtT0.Add(rtRefresh)
	p.setIdle(dueAt.Add(-10 * time.Second))

	gate := make(chan struct{})
	var open sync.Once
	release := func() {
		open.Do(func() {
			p.svc.setGate(nil)
			close(gate)
		})
	}
	t.Cleanup(release)
	p.svc.setGate(gate)
	p.svc.setGateAnswer(fault429, "3600")
	rows := make(chan users.RefreshReport, 1)
	errs := make(chan error, 1)
	go func() {
		rep, err := p.srv.rt.Refresh(context.Background())
		if err != nil {
			errs <- err
			return
		}
		rows <- rep
	}()
	select {
	case <-p.svc.arrived:
	case <-time.After(10 * time.Second):
		t.Fatal("the admin's refresh did not reach the service")
	}
	// The schedule comes due while the admin's pull runs: its pull queues.
	p.clock.set(dueAt)
	eventually(t, "the schedule's pull queued", func() bool {
		_, q := p.srv.flights()
		return q != nil
	})
	sent := p.svc.sent()
	release()
	select {
	case rep := <-rows:
		if rep.Directory.Status != users.DirectoryRefreshFailed || !strings.Contains(rep.Directory.Error, "rate-limited") {
			t.Fatalf("admin refresh: %+v", rep.Directory)
		}
	case err := <-errs:
		t.Fatalf("Refresh: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("the admin's refresh was not answered")
	}
	p.next() // the admin's
	p.idleAt(dueAt)
	p.srv.idle(t)
	if n := p.svc.sent() - sent; n != 0 {
		t.Fatalf("the schedule's queued pull sent %d requests inside the Retry-After", n)
	}
	if n := p.srv.log.count("directory: the scheduled pull is not sent: the account service asked for no request"); n != 1 {
		t.Fatalf("the pull not sent was logged %d times:\n%s", n, p.srv.log)
	}
	p.wantNext(dueAt.Add(time.Hour))
	p.svc.setGateAnswer(faultNone, "")
	p.clock.set(dueAt.Add(time.Hour))
	if row := p.next(); row.Status != users.DirectoryRefreshGood {
		t.Fatalf("the scheduled pull at the Retry-After's end: %+v", row)
	}
}
