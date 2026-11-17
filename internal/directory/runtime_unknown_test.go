package directory

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/directory/refserver"
	"github.com/charliek/shed/internal/users"
)

// The unknown-key path (RefetchForUnknownKey): a new person's first SSH login
// pulls the list, at most once a minute while pulls succeed, at most once per
// retry interval while they fail, never before a pending Retry-After, and
// waits under one deadline. These tests run on the fake clock: none sleeps,
// and the schedule is never started, so every pull is one the test caused.

// unknownRT is a runtime enrolled with svc, with ada listed and pulled once
// (good), its schedule not started.
func unknownRT(t *testing.T) (*fakeClock, *rtService, *rtServer, *person) {
	t.Helper()
	clock := newFakeClock(rtT0)
	svc := newRTService(t, clock)
	dir := filepath.Join(t.TempDir(), "directory")
	svc.enrol(dir)
	opsLine, _ := newTestKey(t)
	ada := newPerson(t, "ada", false)
	svc.dir.setUsers(ada.served())
	srv := startRT(t, svc, dir, opsLine, nil)
	if row := srv.refresh(t); row.Status != users.DirectoryRefreshGood {
		t.Fatalf("set-up pull: %+v", row)
	}
	for len(svc.arrived) > 0 {
		<-svc.arrived // the set-up pull's
	}
	return clock, svc, srv, ada
}

// sent is how many requests the service has had, token and directory.
func (s *rtService) sent() int { return s.directoryCount() + s.tokenCount() }

// unknownKey is RefetchForUnknownKey for fp, which must answer within ten
// seconds of real time (a pull against the in-process service, or nothing).
func (s *rtServer) unknownKey(t *testing.T, fp string) bool {
	t.Helper()
	got := make(chan bool, 1)
	go func() { got <- s.rt.RefetchForUnknownKey(context.Background(), fp) }()
	select {
	case ok := <-got:
		return ok
	case <-time.After(10 * time.Second):
		t.Fatal("RefetchForUnknownKey did not answer")
	}
	return false
}

// refusedAtOnce checks that RefetchForUnknownKey for fp returns false having
// asked for no pull, sent nothing and waited for nothing: the service holds
// every directory answer meanwhile, so a call that joined a pull would not
// return. "Asked for no pull" is read on the runtime's count of the pulls
// made (PullsAsked), which does not depend on when a pull would run.
func refusedAtOnce(t *testing.T, svc *rtService, srv *rtServer, fp, why string) {
	t.Helper()
	gate := make(chan struct{})
	svc.setGate(gate)
	defer func() {
		svc.setGate(nil)
		close(gate)
	}()
	before, asked := svc.sent(), srv.rt.PullsAsked()
	slots := srv.log.count("directory: unknown key ")
	got := make(chan bool, 1)
	go func() { got <- srv.rt.RefetchForUnknownKey(context.Background(), fp) }()
	select {
	case ok := <-got:
		if ok {
			t.Fatalf("%s: an unknown key was let in", why)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("%s: the call waited (it should have been refused at once)", why)
	}
	if n := srv.rt.PullsAsked() - asked; n != 0 {
		t.Fatalf("%s: %d pulls asked for a refused unknown key", why, n)
	}
	if n := srv.log.count("directory: unknown key ") - slots; n != 0 {
		t.Fatalf("%s: a refused unknown key took the slot", why)
	}
	if n := svc.sent() - before; n != 0 {
		t.Fatalf("%s: %d requests sent for a refused unknown key", why, n)
	}
}

// eventually waits, up to ten seconds of real time, for cond.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for !cond() {
		select {
		case <-deadline:
			t.Fatalf("%s: not within ten seconds", what)
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// flights are the runtime's running and pending pulls.
func (s *rtServer) flights() (running, pending *flight) {
	s.rt.fmu.Lock()
	defer s.rt.fmu.Unlock()
	return s.rt.running, s.rt.pending
}

// idle waits until no pull runs and none is queued.
func (s *rtServer) idle(t *testing.T) {
	t.Helper()
	eventually(t, "the runtime's pulls ending", func() bool {
		r, p := s.flights()
		return r == nil && p == nil
	})
}

// pulledFor checks that RefetchForUnknownKey for fp pulled once and reports
// want.
func pulledFor(t *testing.T, svc *rtService, srv *rtServer, fp string, want bool, why string) {
	t.Helper()
	before := svc.sent()
	if got := srv.unknownKey(t, fp); got != want {
		t.Fatalf("%s: RefetchForUnknownKey = %v, want %v", why, got, want)
	}
	if n := svc.sent() - before; n < 1 {
		t.Fatalf("%s: no pull for the unknown key", why)
	}
}

// TestUnknownKeyHealthyRate: while the last pull succeeded, an unknown key
// pulls at most once a minute. A person added to the list after the last pull
// logs in with no refresh run by anyone; a second unknown key inside the
// minute is refused at once, having sent nothing; at the minute it pulls. An
// admin's refresh is not limited by the slot and does not take it.
func TestUnknownKeyHealthyRate(t *testing.T) {
	clock, svc, srv, ada := unknownRT(t)
	bob, cy := newPerson(t, "bob", false), newPerson(t, "cy", false)
	svc.dir.setUsers(ada.served(), bob.served())

	t0 := clock.Now()
	pulledFor(t, svc, srv, bob.fp, true, "bob, listed since the last pull")
	if srv.owner(bob.fp) != "bob" {
		t.Fatal("bob's key does not resolve after the unknown-key pull")
	}
	// Cy is listed after it: her key is unknown to the server.
	svc.dir.setUsers(ada.served(), bob.served(), cy.served())
	if n := srv.log.count("unknown key " + bob.fp); n != 1 {
		t.Errorf("the unknown-key pull logged %d times, want 1:\n%s", n, srv.log)
	}
	refusedAtOnce(t, svc, srv, cy.fp, "a second unknown key at once")
	clock.set(t0.Add(UnknownKeyEvery - time.Second))
	refusedAtOnce(t, svc, srv, cy.fp, "a second unknown key a second before the minute")

	// An admin's refresh inside the minute and one at the minute: neither
	// is limited, and neither takes the slot (the unknown key after them, at
	// the minute, pulls).
	if row := srv.refresh(t); row.Status != users.DirectoryRefreshGood || srv.owner(cy.fp) != "cy" {
		t.Fatalf("an admin's refresh inside the minute: %+v", row)
	}
	clock.set(t0.Add(UnknownKeyEvery))
	if row := srv.refresh(t); row.Status != users.DirectoryRefreshGood {
		t.Fatalf("an admin's refresh at the minute: %+v", row)
	}
	eve := newPerson(t, "eve", false)
	svc.dir.setUsers(ada.served(), eve.served())
	pulledFor(t, svc, srv, eve.fp, true, "an unknown key at the minute, after two admin refreshes")
	if srv.owner(bob.fp) != "" {
		t.Fatal("the unknown-key pull did not publish the new list (bob removed)")
	}

	// A key the list does not name: the pull happens, the answer is no.
	clock.set(t0.Add(2 * UnknownKeyEvery))
	stranger := newPerson(t, "stranger", false)
	pulledFor(t, svc, srv, stranger.fp, false, "a key nobody lists")
	refusedAtOnce(t, svc, srv, "", "the empty fingerprint")
}

// TestUnknownKeyFailedRate: while the server is in the failed state (the last
// pull failed, with no Retry-After), an unknown key pulls at most once per
// retry interval (15 minutes). The rate follows the state at the time of the
// call: once a pull is good again, it is once a minute.
func TestUnknownKeyFailedRate(t *testing.T) {
	clock, svc, srv, ada := unknownRT(t)
	bob := newPerson(t, "bob", false)
	svc.dir.setOutage(true)
	if row := srv.refresh(t); row.Status != users.DirectoryRefreshFailed {
		t.Fatalf("the outage: %+v", row)
	}
	svc.dir.setUsers(ada.served(), bob.served())

	t0 := clock.Now()
	pulledFor(t, svc, srv, bob.fp, false, "the first unknown key in the failed state")
	for _, at := range []time.Duration{0, UnknownKeyEvery, 5 * time.Minute, RetryInterval - time.Second} {
		clock.set(t0.Add(at))
		refusedAtOnce(t, svc, srv, bob.fp, "inside the retry interval after "+at.String())
	}
	svc.dir.setOutage(false)
	clock.set(t0.Add(RetryInterval))
	pulledFor(t, svc, srv, bob.fp, true, "at the retry interval, the service back")

	// Good again: once a minute.
	cy := newPerson(t, "cy", false)
	svc.dir.setUsers(ada.served(), bob.served(), cy.served())
	clock.set(t0.Add(RetryInterval + UnknownKeyEvery))
	pulledFor(t, svc, srv, cy.fp, true, "a minute later, the last pull good")
}

// TestUnknownKeyRetryAfter: no unknown key pulls before a pending Retry-After
// ends, though the slot is free; an admin's refresh ignores it, and its own
// result decides what is pending next. A Retry-After over an hour holds the
// unknown key for an hour.
func TestUnknownKeyRetryAfter(t *testing.T) {
	clock, svc, srv, ada := unknownRT(t)
	bob := newPerson(t, "bob", false)
	svc.dir.setUsers(ada.served(), bob.served())

	svc.setMode(faultRetryAfter, "1800")
	if row := srv.refresh(t); row.Status != users.DirectoryRefreshFailed {
		t.Fatalf("a 503 with a Retry-After: %+v", row)
	}
	t0 := clock.Now()
	svc.setMode(faultNone, "")
	for _, at := range []time.Duration{0, RetryInterval, 30*time.Minute - time.Second} {
		clock.set(t0.Add(at))
		refusedAtOnce(t, svc, srv, bob.fp, "before the Retry-After's end, "+at.String()+" in")
	}
	clock.set(t0.Add(30 * time.Minute))
	pulledFor(t, svc, srv, bob.fp, true, "at the Retry-After's end")

	// Over an hour: held to one hour.
	cy := newPerson(t, "cy", false)
	svc.dir.setUsers(ada.served(), bob.served(), cy.served())
	svc.setMode(fault429, "7200")
	if row := srv.refresh(t); row.Status != users.DirectoryRefreshFailed {
		t.Fatalf("a 429 with a Retry-After of two hours: %+v", row)
	}
	t1 := clock.Now()
	svc.setMode(faultNone, "")
	clock.set(t1.Add(MaxRetryAfter - time.Second))
	refusedAtOnce(t, svc, srv, cy.fp, "a second before the hour")
	clock.set(t1.Add(MaxRetryAfter))
	pulledFor(t, svc, srv, cy.fp, true, "at the hour")

	// An admin's refresh ignores a pending Retry-After, and its good result
	// clears it.
	dee := newPerson(t, "dee", false)
	svc.setMode(faultRetryAfter, "1800")
	if row := srv.refresh(t); row.Status != users.DirectoryRefreshFailed {
		t.Fatalf("a 503 with a Retry-After: %+v", row)
	}
	svc.setMode(faultNone, "")
	svc.dir.setUsers(ada.served(), dee.served())
	if row := srv.refresh(t); row.Status != users.DirectoryRefreshGood {
		t.Fatalf("an admin's refresh during a pending Retry-After: %+v", row)
	}
	eve := newPerson(t, "eve", false)
	svc.dir.setUsers(ada.served(), eve.served())
	clock.set(t1.Add(MaxRetryAfter + UnknownKeyEvery))
	pulledFor(t, svc, srv, eve.fp, true, "after a good refresh cleared the Retry-After")
}

// TestUnknownKeyDeadline: the login that takes the slot waits under one
// deadline (UnknownKeyWait, on the runtime's clock) and is answered when it
// passes, from the newest snapshot, while the pull carries on; the deadline
// covers the time spent queued behind a pull already running.
func TestUnknownKeyDeadline(t *testing.T) {
	t.Run("a slow service", func(t *testing.T) {
		clock, svc, srv, ada := unknownRT(t)
		bob := newPerson(t, "bob", false)
		svc.dir.setUsers(ada.served(), bob.served())
		svc.setMode(faultHang, "")
		got := make(chan bool, 1)
		go func() { got <- srv.rt.RefetchForUnknownKey(context.Background(), bob.fp) }()
		select {
		case <-svc.hung:
		case <-time.After(10 * time.Second):
			t.Fatal("the unknown key's pull did not reach the service")
		}
		clock.advance(UnknownKeyWait - time.Nanosecond)
		select {
		case ok := <-got:
			t.Fatalf("answered (%v) before the deadline", ok)
		case <-time.After(200 * time.Millisecond):
		}
		clock.advance(time.Nanosecond)
		select {
		case ok := <-got:
			if ok {
				t.Fatal("a key the hung pull never delivered was let in")
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the unknown key was not answered at its deadline")
		}
		// The pull ends at its own bound, failed. (That it carries on after
		// its caller gives up is shown, without depending on timing, by "the
		// pull outlives its waiter".)
		svc.setMode(faultNone, "")
		clock.advance(PullTimeout - UnknownKeyWait)
		eventually(t, "the pull ending at its own bound", func() bool { return srv.rt.Status().LastError != "" })
	})

	t.Run("the pull outlives its waiter", func(t *testing.T) {
		clock, svc, srv, ada := unknownRT(t)
		bob := newPerson(t, "bob", false)
		svc.dir.setUsers(ada.served(), bob.served())
		gate := make(chan struct{})
		var open sync.Once
		release := func() {
			open.Do(func() {
				svc.setGate(nil)
				close(gate)
			})
		}
		t.Cleanup(release)
		svc.setGate(gate)
		asked := srv.rt.PullsAsked()
		got := make(chan bool, 1)
		go func() { got <- srv.rt.RefetchForUnknownKey(context.Background(), bob.fp) }()
		select {
		case <-svc.arrived:
		case <-time.After(10 * time.Second):
			t.Fatal("the unknown key's pull did not reach the service")
		}
		f, _ := srv.flights()
		if f == nil || !f.unknownOnly {
			t.Fatalf("the running pull is not the unknown key's: %+v", f)
		}
		clock.advance(UnknownKeyWait)
		select {
		case ok := <-got:
			if ok {
				t.Fatal("let in before the pull's answer")
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the unknown key was not answered at its deadline")
		}
		select {
		case <-f.done:
			t.Fatalf("the pull ended with its waiter: %+v", f.row)
		default:
		}
		// The service answers now; the pull its waiter gave up on is the one
		// that answers, good, and its list applies.
		release()
		select {
		case <-f.done:
		case <-time.After(10 * time.Second):
			t.Fatal("the pull did not end once answered")
		}
		if f.row.Status != users.DirectoryRefreshGood {
			t.Fatalf("the pull its waiter gave up on did not carry on to a good answer: %+v", f.row)
		}
		if srv.owner(bob.fp) != "bob" {
			t.Fatal("the pull's list was not published")
		}
		if n := srv.rt.PullsAsked() - asked; n != 1 {
			t.Fatalf("%d pulls asked for, want 1", n)
		}
	})

	t.Run("queued behind a running pull", func(t *testing.T) {
		clock, svc, srv, ada := unknownRT(t)
		bob := newPerson(t, "bob", false)
		gate := make(chan struct{})
		var open sync.Once
		release := func() { open.Do(func() { close(gate) }) }
		t.Cleanup(release) // before the runtime's and the service's own
		svc.setGate(gate)
		running := srv.rt.request()
		select {
		case <-svc.arrived:
		case <-time.After(10 * time.Second):
			t.Fatal("the running pull did not reach the service")
		}
		svc.setGate(nil)
		svc.dir.setUsers(ada.served(), bob.served())
		got := make(chan bool, 1)
		go func() { got <- srv.rt.RefetchForUnknownKey(context.Background(), bob.fp) }()
		// The unknown key's pull is queued behind the running one, and the
		// slot is taken: another unknown key is refused at once.
		deadline := time.After(10 * time.Second)
		for {
			srv.rt.fmu.Lock()
			queued := srv.rt.pending != nil
			srv.rt.fmu.Unlock()
			if queued {
				break
			}
			select {
			case <-deadline:
				t.Fatal("the unknown key's pull was not queued")
			case <-time.After(5 * time.Millisecond):
			}
		}
		refusedAtOnce(t, svc, srv, newPerson(t, "cy", false).fp, "a second unknown key while the first waits")
		clock.advance(UnknownKeyWait)
		select {
		case ok := <-got:
			if ok {
				t.Fatal("let in before any pull that could name the key had ended")
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the deadline did not cover the time queued")
		}
		release()
		<-running.done
		// The queued pull carries on and its list applies.
		deadline = time.After(10 * time.Second)
		for srv.owner(bob.fp) != "bob" {
			select {
			case <-deadline:
				t.Fatal("the queued pull's list was not published")
			case <-time.After(5 * time.Millisecond):
			}
		}
	})

	t.Run("the login ends", func(t *testing.T) {
		_, svc, srv, ada := unknownRT(t)
		bob := newPerson(t, "bob", false)
		svc.dir.setUsers(ada.served(), bob.served())
		svc.setMode(faultHang, "")
		ctx, cancel := context.WithCancel(context.Background())
		got := make(chan bool, 1)
		go func() { got <- srv.rt.RefetchForUnknownKey(ctx, bob.fp) }()
		select {
		case <-svc.hung:
		case <-time.After(10 * time.Second):
			t.Fatal("the unknown key's pull did not reach the service")
		}
		cancel()
		select {
		case ok := <-got:
			if ok {
				t.Fatal("let in")
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the unknown key's wait did not end with its login")
		}
	})
}

// TestUnknownKeyLocalAdminsNeverWait: while an unknown key's pull hangs, a
// local admin's key resolves at once, a listed user's too, and another
// unknown key is refused at once.
func TestUnknownKeyLocalAdminsNeverWait(t *testing.T) {
	clock, svc, srv, ada := unknownRT(t)
	bob, cy := newPerson(t, "bob", false), newPerson(t, "cy", false)
	svc.dir.setUsers(ada.served(), bob.served(), cy.served())
	svc.setMode(faultHang, "")
	got := make(chan bool, 1)
	go func() { got <- srv.rt.RefetchForUnknownKey(context.Background(), bob.fp) }()
	select {
	case <-svc.hung:
	case <-time.After(10 * time.Second):
		t.Fatal("the unknown key's pull did not reach the service")
	}

	resolved := make(chan string, 2)
	go func() {
		resolved <- srv.owner(srv.opsFP)
		resolved <- srv.owner(ada.fp)
	}()
	for _, want := range []string{"ops", "ada"} {
		select {
		case who := <-resolved:
			if who != want {
				t.Fatalf("resolved to %q, want %q", who, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s's key waited on the hung pull", want)
		}
	}
	before, asked := svc.sent(), srv.rt.PullsAsked()
	if srv.unknownKey(t, cy.fp) || svc.sent() != before || srv.rt.PullsAsked() != asked {
		t.Fatal("a second unknown key during the hung pull was not refused at once")
	}
	clock.advance(UnknownKeyWait)
	select {
	case ok := <-got:
		if ok {
			t.Fatal("the hung pull let bob in")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("bob's login was not answered at its deadline")
	}
}

// TestUnknownKeyOnlyWhenEnrolled: with no enrolment, or one that cannot be
// used, no unknown key starts anything, and none takes the slot: once the
// server is enrolled and has pulled, the first unknown key pulls at once. A
// runtime that is stopping starts nothing either.
func TestUnknownKeyOnlyWhenEnrolled(t *testing.T) {
	clock := newFakeClock(rtT0)
	svc := newRTService(t, clock)
	dir := filepath.Join(t.TempDir(), "directory")
	opsLine, _ := newTestKey(t)
	ada := newPerson(t, "ada", false)
	svc.dir.setUsers(ada.served())
	srv := startRT(t, svc, dir, opsLine, nil)

	refusedAtOnce(t, svc, srv, ada.fp, "not enrolled, at start")
	if row := srv.refresh(t); row.Status != users.DirectoryRefreshFailed {
		t.Fatalf("a pull with no enrolment: %+v", row)
	}
	refusedAtOnce(t, svc, srv, ada.fp, "not enrolled, after a pull")

	svc.enrol(dir)
	path := filepath.Join(dir, EnrolmentFileName)
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	srv.refresh(t)
	if st := srv.rt.Status(); st.State != StateEnrolmentUnusable {
		t.Fatalf("a group-readable enrolment: %s", st.State)
	}
	refusedAtOnce(t, svc, srv, ada.fp, "an unusable enrolment")

	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if row := srv.refresh(t); row.Status != users.DirectoryRefreshGood {
		t.Fatalf("enrolled: %+v", row)
	}
	bob := newPerson(t, "bob", false)
	svc.dir.setUsers(ada.served(), bob.served())
	pulledFor(t, svc, srv, bob.fp, true, "the first unknown key once enrolled")

	cy := newPerson(t, "cy", false)
	svc.dir.setUsers(ada.served(), bob.served(), cy.served())
	clock.advance(UnknownKeyEvery)
	srv.stop()
	refusedAtOnce(t, svc, srv, cy.fp, "a stopped runtime")
}

// TestUnknownKeyHealthyBoundary: while pulls succeed, the slot is free again
// exactly a minute after it was taken, and not a moment before, with nothing
// in between (no admin's refresh) to move anything.
func TestUnknownKeyHealthyBoundary(t *testing.T) {
	clock, svc, srv, ada := unknownRT(t)
	bob, cy, dee := newPerson(t, "bob", false), newPerson(t, "cy", false), newPerson(t, "dee", false)
	svc.dir.setUsers(ada.served(), bob.served())
	t0 := clock.Now()
	pulledFor(t, svc, srv, bob.fp, true, "the first unknown key")
	svc.dir.setUsers(ada.served(), bob.served(), cy.served())
	clock.set(t0.Add(UnknownKeyEvery - time.Nanosecond))
	refusedAtOnce(t, svc, srv, cy.fp, "a nanosecond before the minute")
	clock.set(t0.Add(UnknownKeyEvery))
	pulledFor(t, svc, srv, cy.fp, true, "at the minute")
	svc.dir.setUsers(ada.served(), bob.served(), cy.served(), dee.served())
	clock.set(t0.Add(2*UnknownKeyEvery - time.Nanosecond))
	refusedAtOnce(t, svc, srv, dee.fp, "a nanosecond before the next minute")
	clock.set(t0.Add(2 * UnknownKeyEvery))
	pulledFor(t, svc, srv, dee.fp, true, "at the next minute")
}

// TestUnknownKeySimultaneousCallers: of many unknown keys offered at once,
// exactly one takes the slot and one pull is asked for; every other call is
// refused at once while that one waits for the pull's answer.
func TestUnknownKeySimultaneousCallers(t *testing.T) {
	_, svc, srv, ada := unknownRT(t)
	const n = 16
	served := []refserver.User{ada.served()}
	people := make([]*person, n)
	for i := range people {
		people[i] = newPerson(t, fmt.Sprintf("p%d", i), false)
		served = append(served, people[i].served())
	}
	svc.dir.setUsers(served...)
	gate := make(chan struct{})
	var open sync.Once
	release := func() {
		open.Do(func() {
			svc.setGate(nil)
			close(gate)
		})
	}
	t.Cleanup(release)
	svc.setGate(gate)
	asked, directory := srv.rt.PullsAsked(), svc.directoryCount()

	start := make(chan struct{})
	results := make(chan bool, n)
	for _, p := range people {
		go func(fp string) {
			<-start
			results <- srv.rt.RefetchForUnknownKey(context.Background(), fp)
		}(p.fp)
	}
	close(start)
	for i := 0; i < n-1; i++ {
		select {
		case ok := <-results:
			if ok {
				t.Fatal("a key was let in before the pull's answer")
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("%d of %d calls were refused at once, want %d", i, n, n-1)
		}
	}
	// The call that took the slot asks for its pull after it took it, so
	// the refused calls may all have returned before it has.
	eventually(t, "the pull asked for by the call that took the slot", func() bool {
		return srv.rt.PullsAsked()-asked >= 1
	})
	if got := srv.rt.PullsAsked() - asked; got != 1 {
		t.Fatalf("%d pulls asked for by %d simultaneous unknown keys, want 1", got, n)
	}
	if got := srv.log.count("directory: unknown key "); got != 1 {
		t.Fatalf("the slot was taken %d times, want 1", got)
	}
	select {
	case ok := <-results:
		t.Fatalf("every call answered (%v) before the pull's answer", ok)
	default:
	}
	release()
	select {
	case ok := <-results:
		if !ok {
			t.Fatal("the key that took the slot, listed, was not let in")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the call that took the slot was not answered")
	}
	if got := svc.directoryCount() - directory; got != 1 {
		t.Fatalf("%d directory requests, want 1", got)
	}
}

// queuedUnknownKey has an unknown key (fp) take the slot and queue its pull
// behind a running one (an admin's refresh or the schedule's, asked through
// request) whose answer the service holds and then, released, replaces with
// mode and retryAfter. joinAdmin, when set, has an admin's refresh join the
// queued pull before the release. It returns the unknown key's answer, the
// requests sent after the running pull's, and the admin's row (when joined).
func queuedUnknownKey(t *testing.T, svc *rtService, srv *rtServer, fp string, mode faultMode, retryAfter string, joinAdmin bool) (bool, int, *users.DirectoryRefresh) {
	t.Helper()
	gate := make(chan struct{})
	var open sync.Once
	release := func() {
		open.Do(func() {
			svc.setGate(nil)
			close(gate)
		})
	}
	t.Cleanup(release)
	svc.setGate(gate)
	svc.setGateAnswer(mode, retryAfter)
	defer svc.setGateAnswer(faultNone, "")
	running := srv.rt.request()
	select {
	case <-svc.arrived:
	case <-time.After(10 * time.Second):
		t.Fatal("the running pull did not reach the service")
	}
	asked := srv.rt.PullsAsked()
	got := make(chan bool, 1)
	go func() { got <- srv.rt.RefetchForUnknownKey(context.Background(), fp) }()
	eventually(t, "the unknown key's pull queued", func() bool {
		_, p := srv.flights()
		return p != nil
	})
	if n := srv.rt.PullsAsked() - asked; n != 1 {
		t.Fatalf("%d pulls asked for by the unknown key, want 1", n)
	}
	// An admin's refresh joins through request, as Refresh does, before it
	// waits.
	var joined *flight
	if joinAdmin {
		_, queued := srv.flights()
		if joined = srv.rt.request(); joined != queued {
			t.Fatal("the admin's refresh did not join the queued pull")
		}
		if n := srv.rt.PullsAsked() - asked; n != 1 {
			t.Fatalf("the admin's refresh asked for a pull of its own (%d in all)", n)
		}
	}
	sent := svc.sent()
	release()
	select {
	case <-running.done:
	case <-time.After(10 * time.Second):
		t.Fatal("the running pull did not end")
	}
	if running.row.Status != users.DirectoryRefreshFailed {
		t.Fatalf("the running pull: %+v", running.row)
	}
	var ok bool
	select {
	case ok = <-got:
	case <-time.After(10 * time.Second):
		t.Fatal("the unknown key was not answered")
	}
	var row *users.DirectoryRefresh
	if joined != nil {
		select {
		case <-joined.done:
			row = &joined.row
		case <-time.After(10 * time.Second):
			t.Fatal("the admin's refresh was not answered")
		}
	}
	srv.idle(t)
	return ok, svc.sent() - sent, row
}

// TestUnknownKeyQueuedPullCheckedAtStart: a pull that exists only for an
// unknown key, queued behind a pull that is then answered with a Retry-After,
// or that fails while an unknown key pulled less than the retry interval
// before, ends without sending anything when its turn comes, and its caller
// is answered from the list the server holds. One an admin's refresh joined
// is sent all the same.
func TestUnknownKeyQueuedPullCheckedAtStart(t *testing.T) {
	t.Run("the running pull answered 429 with a Retry-After", func(t *testing.T) {
		_, svc, srv, ada := unknownRT(t)
		bob := newPerson(t, "bob", false)
		svc.dir.setUsers(ada.served(), bob.served())
		ok, sent, _ := queuedUnknownKey(t, svc, srv, bob.fp, fault429, "3600", false)
		if ok || sent != 0 {
			t.Fatalf("the queued pull went ahead inside the Retry-After: let in %v, %d requests sent", ok, sent)
		}
		if srv.log.count("the pull asked for by an unknown key is not sent: the account service asked for no request") != 1 {
			t.Errorf("the pull not sent was not logged:\n%s", srv.log)
		}
	})

	t.Run("the running pull answered 503 with no Retry-After", func(t *testing.T) {
		clock, svc, srv, ada := unknownRT(t)
		bob, cy := newPerson(t, "bob", false), newPerson(t, "cy", false)
		svc.dir.setUsers(ada.served(), cy.served())
		t0 := clock.Now()
		pulledFor(t, svc, srv, cy.fp, true, "an unknown key a minute before")
		svc.dir.setUsers(ada.served(), bob.served(), cy.served())
		clock.set(t0.Add(UnknownKeyEvery))
		ok, sent, _ := queuedUnknownKey(t, svc, srv, bob.fp, faultRetryAfter, "", false)
		if ok || sent != 0 {
			t.Fatalf("the queued pull went ahead in the failed state a minute after the last unknown key's: let in %v, %d requests sent", ok, sent)
		}
		if srv.log.count("the last pull failed, and while pulls fail an unknown key pulls at most once every") != 1 {
			t.Errorf("the pull not sent was not logged:\n%s", srv.log)
		}
	})

	t.Run("no unknown key inside the retry interval: sent after a 503", func(t *testing.T) {
		// The reading the failed state's rate is held to at the start: the
		// same as when the slot is taken, so a pull queued for the first
		// unknown key in the interval goes ahead, as one asked for just after
		// the failure would.
		_, svc, srv, ada := unknownRT(t)
		bob := newPerson(t, "bob", false)
		svc.dir.setUsers(ada.served(), bob.served())
		ok, sent, _ := queuedUnknownKey(t, svc, srv, bob.fp, faultRetryAfter, "", false)
		if !ok || sent == 0 {
			t.Fatalf("the first unknown key's queued pull: let in %v, %d requests sent", ok, sent)
		}
	})

	t.Run("an admin's refresh joined it", func(t *testing.T) {
		_, svc, srv, ada := unknownRT(t)
		bob := newPerson(t, "bob", false)
		svc.dir.setUsers(ada.served(), bob.served())
		ok, sent, row := queuedUnknownKey(t, svc, srv, bob.fp, fault429, "3600", true)
		if sent == 0 || row == nil || row.Status != users.DirectoryRefreshGood {
			t.Fatalf("the pull an admin joined was not sent: %d requests, row %+v", sent, row)
		}
		if !ok {
			t.Fatal("the unknown key the joined pull listed was not let in")
		}
	})
}

// TestUnknownKeyRetryAfterKeepsTheLaterEnd: a pending Retry-After is never
// ended sooner by a later failure, with no Retry-After or a shorter one; a
// longer one moves its end later.
func TestUnknownKeyRetryAfterKeepsTheLaterEnd(t *testing.T) {
	clock, svc, srv, ada := unknownRT(t)
	bob, cy := newPerson(t, "bob", false), newPerson(t, "cy", false)
	svc.dir.setUsers(ada.served(), bob.served())
	fail := func(mode faultMode, retryAfter, why string) {
		t.Helper()
		svc.setMode(mode, retryAfter)
		if row := srv.refresh(t); row.Status != users.DirectoryRefreshFailed {
			t.Fatalf("%s: %+v", why, row)
		}
		svc.setMode(faultNone, "")
	}

	fail(faultRetryAfter, "3600", "a 503 with an hour's Retry-After")
	t0 := clock.Now()
	clock.set(t0.Add(time.Minute))
	fail(faultRetryAfter, "", "a 503 with no Retry-After")
	clock.set(t0.Add(30 * time.Minute))
	refusedAtOnce(t, svc, srv, bob.fp, "half an hour in, after a failure with no Retry-After")
	fail(fault429, "60", "a 429 with a minute's Retry-After")
	clock.set(t0.Add(45 * time.Minute))
	refusedAtOnce(t, svc, srv, bob.fp, "after a shorter Retry-After ended")
	clock.set(t0.Add(time.Hour - time.Second))
	refusedAtOnce(t, svc, srv, bob.fp, "a second before the hour")
	clock.set(t0.Add(time.Hour))
	pulledFor(t, svc, srv, bob.fp, true, "at the hour")

	// A longer one later: its end is kept.
	svc.dir.setUsers(ada.served(), bob.served(), cy.served())
	t1 := clock.Now()
	fail(fault429, "600", "a 429 with ten minutes' Retry-After")
	clock.set(t1.Add(5 * time.Minute))
	fail(fault429, "3600", "a 429 with an hour's Retry-After")
	clock.set(t1.Add(30 * time.Minute))
	refusedAtOnce(t, svc, srv, cy.fp, "past the first, inside the longer")
	clock.set(t1.Add(65 * time.Minute))
	pulledFor(t, svc, srv, cy.fp, true, "at the longer one's end")
}

// TestUnknownKeyWaitsKeepTheMonotonicReading: with the real clock (no
// seam), the readings the unknown-key path's waits are measured on, the
// slot's taking and a Retry-After's end, keep Go's monotonic reading, so
// their differences are elapsed time and not the wall clock's.
func TestUnknownKeyWaitsKeepTheMonotonicReading(t *testing.T) {
	line, _ := newTestKey(t)
	l, err := NewLookup(LookupOptions{LocalAdmins: map[string]config.UserConfig{"ops": localAdmin(line)}})
	if err != nil {
		t.Fatal(err)
	}
	rt, err := NewRuntime(RuntimeOptions{
		Lookup: l, StateDir: filepath.Join(t.TempDir(), "directory"), Issuer: "https://accounts.example.test",
		Refresh: time.Hour, Trust: 2 * time.Hour, Logf: (&rtLog{}).logf,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.cancel()
	monotonic := func(tm time.Time) bool { return strings.Contains(tm.String(), " m=") }
	rt.mu.Lock()
	rt.enrol = enrolState{clientID: "srv_" + strings.Repeat("0", 32)}
	rt.mu.Unlock()
	if _, ok := rt.takeUnknownKeySlot(); !ok {
		t.Fatal("the slot was not taken")
	}
	rt.failed(time.Now(), &Failure{Step: StepDirectory, Kind: KindRateLimited, Status: 429, RetryAfter: time.Hour, HasRetryAfter: true})
	rt.mu.Lock()
	at, end := rt.unknownAt, rt.retryEnd
	rt.mu.Unlock()
	if !monotonic(at) {
		t.Errorf("the slot's taking %s has no monotonic reading", at)
	}
	if !monotonic(end) {
		t.Errorf("the Retry-After's end %s has no monotonic reading", end)
	}
}

// TestUnknownKeyWaitsAreElapsedTime: the Retry-After and the slot's minute
// are measured as time elapsed, not on the wall clock: a wall clock set
// forward does not end them early, and one set back does not prolong them.
func TestUnknownKeyWaitsAreElapsedTime(t *testing.T) {
	t.Run("a Retry-After", func(t *testing.T) {
		clock, svc, srv, ada := unknownRT(t)
		bob := newPerson(t, "bob", false)
		svc.dir.setUsers(ada.served(), bob.served())
		svc.setMode(faultRetryAfter, "3600")
		if row := srv.refresh(t); row.Status != users.DirectoryRefreshFailed {
			t.Fatalf("a 503 with an hour's Retry-After: %+v", row)
		}
		svc.setMode(faultNone, "")
		clock.elapse(time.Second)
		clock.stepWall(time.Hour)
		refusedAtOnce(t, svc, srv, bob.fp, "a second elapsed, the wall clock set an hour forward")
		clock.stepWall(-time.Hour)
		clock.elapse(time.Hour - time.Second)
		pulledFor(t, svc, srv, bob.fp, true, "an hour elapsed, the wall clock set back an hour")
	})

	t.Run("the slot's minute", func(t *testing.T) {
		clock, svc, srv, ada := unknownRT(t)
		bob, cy := newPerson(t, "bob", false), newPerson(t, "cy", false)
		svc.dir.setUsers(ada.served(), bob.served())
		pulledFor(t, svc, srv, bob.fp, true, "the first unknown key")
		svc.dir.setUsers(ada.served(), bob.served(), cy.served())
		clock.stepWall(UnknownKeyEvery)
		refusedAtOnce(t, svc, srv, cy.fp, "no time elapsed, the wall clock set a minute forward")
		clock.stepWall(-UnknownKeyEvery)
		clock.elapse(UnknownKeyEvery)
		pulledFor(t, svc, srv, cy.fp, true, "a minute elapsed, the wall clock set back")
	})
}
