package sshd

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gossh "golang.org/x/crypto/ssh"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/users"
	"github.com/charliek/shed/internal/users/userstest"
)

// nudgeLookup is a lookup that can fetch for a first login (refetchLookup)
// and nudge for a shed name, as a directory's list can. A nudge for the
// upstream key makes it known, as the background pull would; every nudge is
// recorded.
type nudgeLookup struct {
	*refetchLookup
	nmu    sync.Mutex
	nudges []string
}

var _ users.UnknownKeyNudger = (*nudgeLookup)(nil)

func (n *nudgeLookup) NudgeForUnknownKey(fp string) bool {
	n.nmu.Lock()
	n.nudges = append(n.nudges, fp)
	n.nmu.Unlock()
	n.mu.Lock()
	up := n.upstream
	n.mu.Unlock()
	if fp == up {
		n.AddKey(users.ImplicitName, users.Key{Fingerprint: fp, Source: users.SourceInline})
	}
	return true
}

func (n *nudgeLookup) nudged() []string {
	n.nmu.Lock()
	defer n.nmu.Unlock()
	return append([]string(nil), n.nudges...)
}

func newNudgeLookup(upstream string) *nudgeLookup {
	return &nudgeLookup{refetchLookup: &refetchLookup{Fake: userstest.Implicit(), upstream: upstream}}
}

// blockingBackend is identityBackend ("box" exists) whose shed lookups wait
// until release is closed, whatever their context says, as a metadata read
// stuck on a disk would. It counts them.
type blockingBackend struct {
	*identityBackend
	release chan struct{}
	once    sync.Once
	gets    atomic.Int64
}

func newBlockingBackend(t *testing.T) *blockingBackend {
	b := &blockingBackend{identityBackend: &identityBackend{}, release: make(chan struct{})}
	t.Cleanup(b.unblock)
	return b
}

// unblock lets every shed lookup, waiting or to come, return.
func (b *blockingBackend) unblock() { b.once.Do(func() { close(b.release) }) }

func (b *blockingBackend) GetShed(ctx context.Context, name string) (*config.Shed, error) {
	b.gets.Add(1)
	<-b.release
	return b.identityBackend.GetShed(ctx, name)
}

// dialWithin dials as user with signer and fails the test unless the
// handshake ends (admitted or refused) within five seconds of real time.
func dialWithin(t *testing.T, addr, user string, signer gossh.Signer) bool {
	t.Helper()
	got := make(chan bool, 1)
	go func() { got <- dialUser(t, addr, user, signer) }()
	select {
	case ok := <-got:
		return ok
	case <-time.After(5 * time.Second):
		t.Fatalf("the login as %s was not answered: it waited for the shed check", user)
	}
	return false
}

// waitFor polls cond for up to ten seconds of real time.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	end := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(end) {
			t.Fatalf("%s: not within ten seconds", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// checkIdle waits until no shed check is running.
func checkIdle(t *testing.T, s *Server) {
	t.Helper()
	waitFor(t, "the shed check ending", func() bool { return !s.checking.Load() })
}

// TestUnknownKeyByLoginName: on a lookup that can nudge (a directory's list),
// an unknown key offered as a shed name is refused at once; only a name that
// is a shed asks for a pull, in the background, and the person's next attempt
// is let in. A name that is no shed asks for nothing, `_bootstrap` asks the
// first-login fetch as before, and a name that cannot be a shed asks nothing.
func TestUnknownKeyByLoginName(t *testing.T) {
	for _, tc := range []struct {
		user                  string
		wantNudge, wantFetch  bool
		wantAdmitted, wantRun bool
	}{
		{"box", true, false, false, true},
		{"no-such-shed", false, false, false, true},
		{reservedBootstrapUser, false, true, true, false},
		{"Not_A_Shed", false, false, false, false},
		{"../../etc", false, false, false, false},
	} {
		t.Run(tc.user, func(t *testing.T) {
			newcomer := testSigner(t)
			fp := gossh.FingerprintSHA256(newcomer.PublicKey())
			lookup := newNudgeLookup(fp)
			s, addr := identityServer(t, &identityBackend{}, lookup)
			if got := dialWithin(t, addr, tc.user, newcomer); got != tc.wantAdmitted {
				t.Fatalf("admitted = %v, want %v", got, tc.wantAdmitted)
			}
			if tc.wantRun {
				waitFor(t, "the shed check counted", func() bool {
					c := s.unknownShedCounts()
					return c.noShed+c.existing+c.failed+c.skipped == 1
				})
			}
			checkIdle(t, s)
			if got := len(lookup.nudged()) == 1; got != tc.wantNudge {
				t.Fatalf("nudged = %v (%v), want %v", got, lookup.nudged(), tc.wantNudge)
			}
			if got := len(lookup.seen()) > 0; got != tc.wantFetch {
				t.Fatalf("asked for a first-login fetch = %v (%v), want %v", got, lookup.seen(), tc.wantFetch)
			}
			c := s.unknownShedCounts()
			switch tc.user {
			case "box":
				if c.existing != 1 || c.pulled != 1 || c.noShed != 0 {
					t.Fatalf("counts %+v, want one existing shed that pulled", c)
				}
				// The person's next attempt finds the key the pull brought.
				if !dialWithin(t, addr, tc.user, newcomer) {
					t.Fatal("the retry after the pull was refused")
				}
			case "no-such-shed":
				if c.noShed != 1 || c.existing != 0 {
					t.Fatalf("counts %+v, want one name that is no shed", c)
				}
			default:
				if !c.zero() {
					t.Fatalf("counts %+v, want none", c)
				}
			}
		})
	}
}

// TestUnknownKeyShedNamesNeverWaitForTheCheck (structural): with a backend
// whose shed lookups do not return until the test releases them, a login as
// an existing shed's name and one as a name that is no shed are both refused
// while the check is still blocked: neither answer waits for the check. (It
// does not pin whether the refusal is sent before the check begins: the
// scheduler may run either first.) The second offer, while the first check
// still runs, is refused with no check.
func TestUnknownKeyShedNamesNeverWaitForTheCheck(t *testing.T) {
	newcomer, other := testSigner(t), testSigner(t)
	fp := gossh.FingerprintSHA256(newcomer.PublicKey())
	lookup := newNudgeLookup(fp)
	be := newBlockingBackend(t)
	s, addr := identityServer(t, be, lookup)

	if dialWithin(t, addr, "box", newcomer) {
		t.Fatal("an unknown key as an existing shed's name was let in")
	}
	waitFor(t, "the check reaching the backend", func() bool { return be.gets.Load() == 1 })
	if dialWithin(t, addr, "no-such-shed", other) {
		t.Fatal("an unknown key as a name that is no shed was let in")
	}
	if n := be.gets.Load(); n != 1 {
		t.Fatalf("%d shed lookups while one was blocked, want 1 (one check at a time)", n)
	}
	if c := s.unknownShedCounts(); c.skipped != 1 {
		t.Fatalf("counts %+v, want one offer skipped while a check ran", c)
	}
	if len(lookup.nudged()) != 0 {
		t.Fatal("nudged before the check returned")
	}
	be.unblock()
	waitFor(t, "the nudge for the existing shed", func() bool { return len(lookup.nudged()) == 1 })
	checkIdle(t, s)
	if got := lookup.nudged(); len(got) != 1 || got[0] != fp {
		t.Fatalf("nudges %v, want exactly the existing shed's key", got)
	}
}

// TestUnknownKeyShedCheckOutlivesTheConnection: the check runs under the
// server's lifetime, not the connection's, so the pull is still asked for
// after the refused client has hung up.
func TestUnknownKeyShedCheckOutlivesTheConnection(t *testing.T) {
	newcomer := testSigner(t)
	fp := gossh.FingerprintSHA256(newcomer.PublicKey())
	lookup := newNudgeLookup(fp)
	be := newBlockingBackend(t)
	s, addr := identityServer(t, be, lookup)
	if dialWithin(t, addr, "box", newcomer) {
		t.Fatal("let in")
	}
	waitFor(t, "the check reaching the backend", func() bool { return be.gets.Load() == 1 })
	// The client has closed its connection (dialUser returned); give the
	// server a moment to see it close before the backend answers.
	waitFor(t, "the refused connection leaving the counts", func() bool {
		s.preMu.Lock()
		defer s.preMu.Unlock()
		return s.preTotal == 0
	})
	be.unblock()
	waitFor(t, "the nudge after the connection closed", func() bool { return len(lookup.nudged()) == 1 })
}

// TestUnknownKeyShedCheckDeadline: a check whose backend call outlasts its
// deadline asks for no pull when it does return, and is counted as failed;
// no second check starts while the backend is still blocked. Once it has
// returned, a new offer checks again.
func TestUnknownKeyShedCheckDeadline(t *testing.T) {
	newcomer := testSigner(t)
	fp := gossh.FingerprintSHA256(newcomer.PublicKey())
	lookup := newNudgeLookup(fp)
	be := newBlockingBackend(t)
	s, addr := identityServerWith(t, be, lookup, func(s *Server) { s.shedCheckTimeout = 50 * time.Millisecond })

	if dialWithin(t, addr, "box", newcomer) {
		t.Fatal("let in")
	}
	waitFor(t, "the check reaching the backend", func() bool { return be.gets.Load() == 1 })
	time.Sleep(4 * s.shedCheckTimeout)
	if dialWithin(t, addr, "box", newcomer) {
		t.Fatal("let in")
	}
	if n := be.gets.Load(); n != 1 {
		t.Fatalf("%d shed lookups: a second check started past the first one's deadline while its backend call was blocked", n)
	}
	be.unblock()
	checkIdle(t, s)
	if got := lookup.nudged(); len(got) != 0 {
		t.Fatalf("a check that answered past its deadline nudged: %v", got)
	}
	c := s.unknownShedCounts()
	if c.failed != 1 || c.skipped != 1 || c.existing != 0 || c.firstErr != "no answer within 50ms" {
		t.Fatalf("counts %+v, want one failed (past its deadline) and one skipped", c)
	}
	// The backend answers at once now: the next offer checks and nudges.
	if dialWithin(t, addr, "box", newcomer) {
		t.Fatal("let in before the pull")
	}
	waitFor(t, "the next check's nudge", func() bool { return len(lookup.nudged()) == 1 })
}

// TestUnknownKeyShedCheckShutdown: Shutdown during a check returns within its
// own context while the backend call is still blocked, and the check, when
// its backend call returns, asks for no pull. No check starts after it.
func TestUnknownKeyShedCheckShutdown(t *testing.T) {
	newcomer := testSigner(t)
	fp := gossh.FingerprintSHA256(newcomer.PublicKey())
	lookup := newNudgeLookup(fp)
	be := newBlockingBackend(t)
	s, addr := identityServer(t, be, lookup)
	if dialWithin(t, addr, "box", newcomer) {
		t.Fatal("let in")
	}
	waitFor(t, "the check reaching the backend", func() bool { return be.gets.Load() == 1 })

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.Shutdown(ctx) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Shutdown reported the blocked check ended")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown did not return within its context")
	}
	be.unblock()
	checkIdle(t, s)
	if got := lookup.nudged(); len(got) != 0 {
		t.Fatalf("a check that returned after Shutdown began nudged: %v", got)
	}
	// After Shutdown no check starts (the listener is closed; asked directly).
	s.nudgeIfShed(lookup, "box", fp)
	if n := be.gets.Load(); n != 1 {
		t.Fatalf("%d shed lookups: a check started after Shutdown", n)
	}
}

// TestUnknownKeyShedCheckShutdownWaits: Shutdown waits for a check that ends
// inside its context.
func TestUnknownKeyShedCheckShutdownWaits(t *testing.T) {
	newcomer := testSigner(t)
	fp := gossh.FingerprintSHA256(newcomer.PublicKey())
	lookup := newNudgeLookup(fp)
	be := newBlockingBackend(t)
	s, addr := identityServer(t, be, lookup)
	if dialWithin(t, addr, "box", newcomer) {
		t.Fatal("let in")
	}
	waitFor(t, "the check reaching the backend", func() bool { return be.gets.Load() == 1 })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.Shutdown(ctx) }()
	select {
	case err := <-done:
		t.Fatalf("Shutdown returned (%v) while the check ran", err)
	case <-time.After(200 * time.Millisecond):
	}
	be.unblock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown did not return once the check ended")
	}
	if s.checking.Load() {
		t.Fatal("Shutdown returned before the check ended")
	}
	if got := lookup.nudged(); len(got) != 0 {
		t.Fatalf("the check nudged after Shutdown began: %v", got)
	}
}

// TestUnknownKeyShedShutdownBetweenFoundAndNudge: the interleaving where the
// backend has found the shed and every earlier check of the lifetime has
// passed, and only then Shutdown ends the lifetime, before the nudge is made.
// No nudge is asked: the lifetime is checked again, under the lock Shutdown
// takes to end it, right before the nudge.
func TestUnknownKeyShedShutdownBetweenFoundAndNudge(t *testing.T) {
	newcomer := testSigner(t)
	fp := gossh.FingerprintSHA256(newcomer.PublicKey())
	lookup := newNudgeLookup(fp)
	shut := make(chan error, 1)
	s, addr := identityServerWith(t, &identityBackend{}, lookup, func(s *Server) {
		s.afterShedFound = func() {
			// Shutdown begins and ends the lifetime here, between the
			// backend's answer and the nudge.
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				shut <- s.Shutdown(ctx)
			}()
			<-s.life.Done()
		}
	})
	if dialWithin(t, addr, "box", newcomer) {
		t.Fatal("let in")
	}
	select {
	case err := <-shut:
		if err != nil {
			t.Fatalf("Shutdown: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Shutdown did not return")
	}
	checkIdle(t, s)
	if got := lookup.nudged(); len(got) != 0 {
		t.Fatalf("a nudge was asked after Shutdown ended the lifetime: %v", got)
	}
	if c := s.unknownShedCounts(); !c.zero() {
		t.Fatalf("counts %+v: a check that ended after Shutdown began counted", c)
	}
}

// TestUnknownKeyShedNoBackend: a server built with no backend treats every
// name as no shed.
func TestUnknownKeyShedNoBackend(t *testing.T) {
	newcomer := testSigner(t)
	fp := gossh.FingerprintSHA256(newcomer.PublicKey())
	lookup := newNudgeLookup(fp)
	s, addr := identityServer(t, nil, lookup)
	if dialWithin(t, addr, "box", newcomer) {
		t.Fatal("let in")
	}
	if got := lookup.nudged(); len(got) != 0 {
		t.Fatalf("nudged with no backend: %v", got)
	}
	if c := s.unknownShedCounts(); c.noShed != 1 {
		t.Fatalf("counts %+v, want one name that is no shed", c)
	}
}

// TestUnknownKeyShedStatsLine: the hourly line gives each count and, when a
// check failed, the first failure's text; nothing is logged when every count
// is zero, and the counts start again from zero after each line. The line
// does not start as the runtime's first-login line, which the integration
// suite counts.
func TestUnknownKeyShedStatsLine(t *testing.T) {
	s, _ := identityServer(t, &identityBackend{}, newNudgeLookup(""))
	out := captureLog(t)
	s.flushUnknownShedStats()
	if out.String() != "" {
		t.Fatalf("a line with every count zero: %q", out.String())
	}
	s.statsMu.Lock()
	s.stats = unknownShedStats{noShed: 7, existing: 2, pulled: 1, skipped: 3, failed: 2, firstErr: "shed metadata unreadable"}
	s.statsMu.Unlock()
	s.flushUnknownShedStats()
	const want = "directory: in the last hour 7 unknown keys were offered as a name that is no shed here and 2 as an existing shed's name (1 started a pull, 3 skipped while a check ran, 2 checks failed: shed metadata unreadable)\n"
	if out.String() != want {
		t.Fatalf("line\n got %q\nwant %q", out.String(), want)
	}
	if strings.HasPrefix(want, "directory: unknown key ") {
		t.Fatal("the line starts as the first-login line")
	}
	if c := s.unknownShedCounts(); !c.zero() {
		t.Fatalf("counts not reset: %+v", c)
	}
	s.statsMu.Lock()
	s.stats = unknownShedStats{noShed: 1}
	s.statsMu.Unlock()
	s.flushUnknownShedStats()
	if !strings.HasSuffix(out.String(), "(0 started a pull, 0 skipped while a check ran, 0 checks failed)\n") {
		t.Fatalf("a line with no failure: %q", out.String())
	}
}
