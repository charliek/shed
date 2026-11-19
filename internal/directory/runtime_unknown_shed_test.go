package directory

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charliek/shed/internal/users"
)

// The unknown-key rule of a directory server (plan 037 §3.5): a first login
// (`_bootstrap`, RefetchForUnknownKey) whose key stays unknown is answered at
// its deadline, never earlier; a login to a shed that exists asks for a pull
// in the background on a slot of its own (NudgeForUnknownKey); a pull several
// unknown keys asked for is sent when any one of them alone would have had it.
// These tests run on the fake clock, with the schedule never started.

// The numbers the docs, the contract and the CHANGELOG state, written out
// here apart from the constants they pin: the tests' boundaries are these, so
// a changed constant fails a test.
const (
	// wantWait: a first login whose key stays unknown is answered 8 s on.
	wantWait = 8 * time.Second
	// wantFirstLoginEvery: the first-login slot, once a minute.
	wantFirstLoginEvery = time.Minute
	// wantShedEvery: the shed-name slot, once every 5 minutes.
	wantShedEvery = 5 * time.Minute
	// wantFailingEvery: either slot while pulls fail, once every 15 minutes.
	wantFailingEvery = 15 * time.Minute
)

// answered waits up to five seconds of real time for a RefetchForUnknownKey
// result.
func answered(t *testing.T, got <-chan bool, what string) bool {
	t.Helper()
	select {
	case ok := <-got:
		return ok
	case <-time.After(5 * time.Second):
		t.Fatalf("%s: not answered", what)
	}
	return false
}

// notAnswered checks that no RefetchForUnknownKey result arrives within 200 ms
// of real time.
func notAnswered(t *testing.T, got <-chan bool, what string) {
	t.Helper()
	select {
	case ok := <-got:
		t.Fatalf("%s: answered (%v) before its deadline", what, ok)
	case <-time.After(200 * time.Millisecond):
	}
}

// bootstrapTaker starts RefetchForUnknownKey for fp and waits until it has
// its deadline timer and has asked for (made or joined) its pull: the
// runtime's seam (unknownWaiting) says so, after both.
func bootstrapTaker(t *testing.T, srv *rtServer, fp string) <-chan bool {
	t.Helper()
	got := make(chan bool, 1)
	waiting := srv.waiting.Load()
	go func() { got <- srv.rt.RefetchForUnknownKey(context.Background(), fp) }()
	eventually(t, "the first login waiting, its timer set and its pull asked for", func() bool {
		return srv.waiting.Load() > waiting
	})
	return got
}

// TestUnknownKeyRefusedAtItsDeadline (J1): a first login whose key the pull
// it asked for did not bring is answered when its deadline timer fires, not
// when the pull ends, whether the pull was good or failed; one whose key the
// pull brings is let in as soon as the pull ends, with no time passing.
func TestUnknownKeyRefusedAtItsDeadline(t *testing.T) {
	for _, tc := range []struct {
		name   string
		outage bool
	}{
		{"a good pull that does not list the key", false},
		{"a failed pull", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock, svc, srv, _ := unknownRT(t)
			stranger := newPerson(t, "stranger", false)
			svc.dir.setOutage(tc.outage)
			asked := srv.rt.PullsAsked()
			got := bootstrapTaker(t, srv, stranger.fp)
			eventually(t, "the pull asked for", func() bool { return srv.rt.PullsAsked() > asked })
			srv.idle(t)
			notAnswered(t, got, "the pull ended, the key unknown")
			clock.advance(wantWait - time.Nanosecond)
			notAnswered(t, got, "a nanosecond before the deadline")
			clock.advance(time.Nanosecond)
			if answered(t, got, "at the deadline") {
				t.Fatal("a key the list does not hold was let in")
			}
		})
	}

	t.Run("a key the pull brings is let in at once", func(t *testing.T) {
		clock, svc, srv, ada := unknownRT(t)
		bob := newPerson(t, "bob", false)
		svc.dir.setUsers(ada.served(), bob.served())
		t0 := clock.Now()
		got := bootstrapTaker(t, srv, bob.fp)
		if !answered(t, got, "a listed key") {
			t.Fatal("a key the pull brought was refused")
		}
		if !clock.Now().Equal(t0) {
			t.Fatal("the clock moved")
		}
	})
}

// TestUnknownKeyLetInByAnotherPublication: a first login whose own pull ended
// without its key, waiting for its deadline, is let in as soon as any other
// publication brings the key (an admin's refresh, or a shed name's nudge), with
// no time passing, not at its deadline.
func TestUnknownKeyLetInByAnotherPublication(t *testing.T) {
	for _, by := range []string{"an admin's refresh", "a shed name's nudge"} {
		t.Run(by, func(t *testing.T) {
			clock, svc, srv, ada := unknownRT(t)
			bob, other := newPerson(t, "bob", false), newPerson(t, "other", false)
			t0 := clock.Now()
			asked := srv.rt.PullsAsked()
			got := bootstrapTaker(t, srv, bob.fp)
			eventually(t, "the first login's pull asked for", func() bool { return srv.rt.PullsAsked() > asked })
			srv.idle(t)
			notAnswered(t, got, "its own pull ended without the key")

			// Another pull, not the first login's, brings the key.
			svc.dir.setUsers(ada.served(), bob.served())
			switch by {
			case "an admin's refresh":
				if row := srv.refresh(t); row.Status != users.DirectoryRefreshGood {
					t.Fatalf("the admin's refresh: %+v", row)
				}
			default:
				nudgedOnce(t, svc, srv, other.fp, "the nudge that brings bob")
			}
			if !answered(t, got, "the key brought by another pull") {
				t.Fatal("a key another pull brought was refused")
			}
			if !clock.Now().Equal(t0) {
				t.Fatal("the clock moved: the login was answered at its deadline, not when its key became known")
			}
		})
	}
}

// TestUnknownKeyShedNudge: an unknown key offered for a login to a shed that
// exists asks for one pull in the background, and returns at once whatever
// the service does; the pull's result applies. Its log line has its own
// prefix, which the integration suite does not count as a first login's.
func TestUnknownKeyShedNudge(t *testing.T) {
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
	done := make(chan bool, 1)
	go func() { done <- srv.rt.NudgeForUnknownKey(bob.fp) }()
	select {
	case ok := <-done:
		if !ok {
			t.Fatal("the nudge asked for no pull")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the nudge waited for the pull")
	}
	if n := srv.rt.PullsAsked() - asked; n != 1 {
		t.Fatalf("%d pulls asked for, want 1", n)
	}
	select {
	case <-svc.arrived:
	case <-time.After(10 * time.Second):
		t.Fatal("the nudge's pull did not reach the service")
	}
	if srv.owner(bob.fp) != "" {
		t.Fatal("bob known before the pull's answer")
	}
	release()
	eventually(t, "the nudge's pull publishing bob", func() bool { return srv.owner(bob.fp) == "bob" })

	// One line, with its own prefix: none starts as a first login's line,
	// which the integration suite counts.
	if n := srv.log.count("directory: shed-name key " + bob.fp + " for an existing shed: pulling the list in the background"); n != 1 {
		t.Fatalf("the nudge's line logged %d times, want 1:\n%s", n, srv.log)
	}
	for _, l := range strings.Split(srv.log.String(), "\n") {
		if strings.HasPrefix(l, "directory: unknown key ") {
			t.Fatalf("the nudge logged a first login's line: %q", l)
		}
	}

	// A known key asks for nothing, the slot free again.
	clock.advance(wantShedEvery)
	asked = srv.rt.PullsAsked()
	if srv.rt.NudgeForUnknownKey(bob.fp) || srv.rt.NudgeForUnknownKey("") || srv.rt.PullsAsked() != asked {
		t.Fatal("a known key, or no key, asked for a pull")
	}
}

// nudgedOnce checks that NudgeForUnknownKey for fp asks for exactly one pull,
// and waits until the runtime has settled (settled).
func nudgedOnce(t *testing.T, svc *rtService, srv *rtServer, fp, why string) {
	t.Helper()
	asked := srv.rt.PullsAsked()
	if !srv.rt.NudgeForUnknownKey(fp) {
		t.Fatalf("%s: the nudge asked for no pull", why)
	}
	if n := srv.rt.PullsAsked() - asked; n != 1 {
		t.Fatalf("%s: %d pulls asked for, want 1", why, n)
	}
	srv.settled(t, svc)
}

// notNudged checks that NudgeForUnknownKey for fp asks for no pull and sends
// nothing.
func notNudged(t *testing.T, svc *rtService, srv *rtServer, fp, why string) {
	t.Helper()
	asked, before := srv.rt.PullsAsked(), svc.sent()
	if srv.rt.NudgeForUnknownKey(fp) {
		t.Fatalf("%s: the nudge asked for a pull", why)
	}
	if n := srv.rt.PullsAsked() - asked; n != 0 {
		t.Fatalf("%s: %d pulls asked for", why, n)
	}
	srv.idle(t)
	if n := svc.sent() - before; n != 0 {
		t.Fatalf("%s: %d requests sent", why, n)
	}
}

// TestUnknownKeyShedRates: the shed-name slot is taken at most once every 5
// minutes while pulls succeed, once every 15 minutes while they fail, never
// inside a pending Retry-After, and never when the server is not enrolled or
// is stopping.
func TestUnknownKeyShedRates(t *testing.T) {
	t.Run("healthy", func(t *testing.T) {
		clock, svc, srv, _ := unknownRT(t)
		x := newPerson(t, "x", false)
		t0 := clock.Now()
		nudgedOnce(t, svc, srv, x.fp, "the first nudge")
		for _, at := range []time.Duration{0, wantFirstLoginEvery, wantShedEvery - time.Nanosecond} {
			clock.set(t0.Add(at))
			notNudged(t, svc, srv, x.fp, "inside the five minutes, "+at.String()+" in")
		}
		clock.set(t0.Add(wantShedEvery))
		nudgedOnce(t, svc, srv, x.fp, "at the five minutes")
	})

	t.Run("failing", func(t *testing.T) {
		clock, svc, srv, _ := unknownRT(t)
		x := newPerson(t, "x", false)
		svc.dir.setOutage(true)
		if row := srv.refresh(t); row.Status != users.DirectoryRefreshFailed {
			t.Fatalf("the outage: %+v", row)
		}
		t0 := clock.Now()
		nudgedOnce(t, svc, srv, x.fp, "the first nudge in the failed state")
		for _, at := range []time.Duration{0, wantShedEvery, wantFailingEvery - time.Nanosecond} {
			clock.set(t0.Add(at))
			notNudged(t, svc, srv, x.fp, "inside the retry interval, "+at.String()+" in")
		}
		clock.set(t0.Add(wantFailingEvery))
		nudgedOnce(t, svc, srv, x.fp, "at the retry interval")
	})

	t.Run("Retry-After", func(t *testing.T) {
		clock, svc, srv, _ := unknownRT(t)
		x := newPerson(t, "x", false)
		svc.setMode(faultRetryAfter, "1800")
		if row := srv.refresh(t); row.Status != users.DirectoryRefreshFailed {
			t.Fatalf("a 503 with a Retry-After: %+v", row)
		}
		svc.setMode(faultNone, "")
		t0 := clock.Now()
		for _, at := range []time.Duration{0, wantFailingEvery, 30*time.Minute - time.Second} {
			clock.set(t0.Add(at))
			notNudged(t, svc, srv, x.fp, "inside the Retry-After, "+at.String()+" in")
		}
		clock.set(t0.Add(30 * time.Minute))
		nudgedOnce(t, svc, srv, x.fp, "at the Retry-After's end")
	})

	t.Run("not enrolled, then stopping", func(t *testing.T) {
		clock := newFakeClock(rtT0)
		svc := newRTService(t, clock)
		dir := filepath.Join(t.TempDir(), "directory")
		opsLine, _ := newTestKey(t)
		x := newPerson(t, "x", false)
		srv := startRT(t, svc, dir, opsLine, nil)
		notNudged(t, svc, srv, x.fp, "not enrolled")
		svc.enrol(dir)
		path := filepath.Join(dir, EnrolmentFileName)
		if err := os.Chmod(path, 0o644); err != nil {
			t.Fatal(err)
		}
		srv.refresh(t)
		notNudged(t, svc, srv, x.fp, "an unusable enrolment")
		if err := os.Chmod(path, 0o600); err != nil {
			t.Fatal(err)
		}
		if row := srv.refresh(t); row.Status != users.DirectoryRefreshGood {
			t.Fatalf("enrolled: %+v", row)
		}
		srv.stop()
		notNudged(t, svc, srv, x.fp, "a stopped runtime")
	})
}

// TestUnknownKeySlotsAreIndependent: a shed name's nudge does not take the
// first-login slot, and a first login does not take the shed-name slot: one of
// each at the same moment both pull.
func TestUnknownKeySlotsAreIndependent(t *testing.T) {
	_, svc, srv, ada := unknownRT(t)
	x, bob, cy, y := newPerson(t, "x", false), newPerson(t, "bob", false), newPerson(t, "cy", false), newPerson(t, "y", false)
	nudgedOnce(t, svc, srv, x.fp, "a shed name's nudge")
	// Listed after the nudge's pull, so the first login's own pull brings them.
	svc.dir.setUsers(ada.served(), bob.served(), cy.served())
	pulledFor(t, svc, srv, bob.fp, true, "a first login right after the nudge")
	// And the other way round: the first-login slot is taken now (bob), the
	// shed-name slot only five minutes later; neither blocks the other.
	refusedAtOnce(t, svc, srv, cy.fp, "a second first login in the same minute")
	notNudged(t, svc, srv, y.fp, "a second nudge in the same five minutes")
}

// TestUnknownKeyMergeRule: a pull that only unknown keys asked for is sent
// when any one asker's own policy allows it, and not when none does. The
// shed-name slot is taken at t0 (healthy) and again at t0+5m, while an
// admin's pull runs that then fails: by that nudge's own policy (the failed
// state, its slot taken five minutes before) its queued pull is not sent.
func TestUnknownKeyMergeRule(t *testing.T) {
	for _, tc := range []struct {
		name string
		// firstLoginBefore: the first-login slot was taken a minute before
		// the joiner's take, so the joiner's own policy forbids the pull too.
		firstLoginBefore bool
		wantSent         bool
	}{
		{"a first login whose own policy allows it joins: sent", false, true},
		{"every asker's own policy forbids it: not sent", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock, svc, srv, ada := unknownRT(t)
			x, y, z, w := newPerson(t, "x", false), newPerson(t, "y", false), newPerson(t, "z", false), newPerson(t, "w", false)
			t0 := clock.Now()
			nudgedOnce(t, svc, srv, x.fp, "the shed-name slot's first take")
			if tc.firstLoginBefore {
				clock.set(t0.Add(wantShedEvery - wantFirstLoginEvery))
				svc.dir.setUsers(ada.served(), w.served())
				pulledFor(t, svc, srv, w.fp, true, "a first login a minute before the joiner")
			}
			svc.dir.setUsers(ada.served(), z.served())
			clock.set(t0.Add(wantShedEvery))

			// An admin's pull runs, held, and will fail with no Retry-After.
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
			svc.setGateAnswer(faultRetryAfter, "")
			defer svc.setGateAnswer(faultNone, "")
			running := srv.rt.request()
			select {
			case <-svc.arrived:
			case <-time.After(10 * time.Second):
				t.Fatal("the running pull did not reach the service")
			}
			if r, p := srv.flights(); r != running || p != nil {
				t.Fatal("the admin's pull is not the one running alone: the nudge would join it, not queue a pull of its own")
			}

			asked := srv.rt.PullsAsked()
			if !srv.rt.NudgeForUnknownKey(y.fp) {
				t.Fatal("the second nudge, five minutes on, did not take its slot")
			}
			got := bootstrapTaker(t, srv, z.fp)
			if n := srv.rt.PullsAsked() - asked; n != 1 {
				t.Fatalf("%d pulls asked for: the first login did not join the nudge's queued pull", n)
			}
			_, queued := srv.flights()
			srv.rt.fmu.Lock()
			prevs := len(queued.unknownPrevs)
			srv.rt.fmu.Unlock()
			if !queued.unknownOnly || prevs != 2 {
				t.Fatalf("the queued pull: unknown-only %v with %d askers, want true with 2", queued.unknownOnly, prevs)
			}

			sent := svc.sent()
			release()
			<-running.done
			if running.row.Status != users.DirectoryRefreshFailed {
				t.Fatalf("the running pull: %+v", running.row)
			}
			srv.idle(t)
			if gotSent := svc.sent() > sent; gotSent != tc.wantSent {
				t.Fatalf("the queued pull sent = %v (%d requests), want %v:\n%s", gotSent, svc.sent()-sent, tc.wantSent, srv.log)
			}
			clock.fireWithin(wantWait)
			if ok := answered(t, got, "the first login"); ok != tc.wantSent {
				t.Fatalf("the first login let in = %v, want %v", ok, tc.wantSent)
			}
		})
	}
}

// TestUnknownKeyAdversarialDeadline: a peer guesses a shed name, then logs in
// as `_bootstrap` with a key that stays unknown (pulls succeeding, no
// Retry-After). When the name is a shed (the server nudged: a pull runs, held
// by the service, and the first login's queues behind it) and when it is not
// (no nudge: the first login's own pull runs, held), the first login is
// answered exactly at its deadline, though the service answers both pulls well
// before it: the refusal's time does not depend on a running shed-name pull.
// (It pins only that: not the failed state, a Retry-After, or a listed key's
// first login, which the security reference states as residuals.)
func TestUnknownKeyAdversarialDeadline(t *testing.T) {
	for _, existing := range []bool{true, false} {
		name := "a name that is no shed"
		if existing {
			name = "an existing shed's name"
		}
		t.Run(name, func(t *testing.T) {
			clock, svc, srv, _ := unknownRT(t)
			guess, taker := newPerson(t, "guess", false), newPerson(t, "taker", false)
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
			if existing {
				if !srv.rt.NudgeForUnknownKey(guess.fp) {
					t.Fatal("the guess's nudge asked for no pull")
				}
				select {
				case <-svc.arrived:
				case <-time.After(10 * time.Second):
					t.Fatal("the nudge's pull did not reach the service")
				}
			}
			got := bootstrapTaker(t, srv, taker.fp)
			if !existing {
				select {
				case <-svc.arrived:
				case <-time.After(10 * time.Second):
					t.Fatal("the first login's pull did not reach the service")
				}
			}
			clock.advance(wantWait / 4)
			release()
			srv.idle(t)
			notAnswered(t, got, "every pull answered")
			clock.advance(wantWait - wantWait/4 - time.Nanosecond)
			notAnswered(t, got, "a nanosecond before the deadline")
			clock.advance(time.Nanosecond)
			if answered(t, got, "at the deadline") {
				t.Fatal("an unknown key was let in")
			}
		})
	}
}
