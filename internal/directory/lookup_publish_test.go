package directory

import (
	"bytes"
	"context"
	"log"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/users"
)

// testClock is a wall clock a test sets.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) set(t time.Time) {
	c.mu.Lock()
	c.t = t
	c.mu.Unlock()
}

// trustT0 is when the tests' lists arrive.
var trustT0 = time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)

// trustLookup is a lookup with the local admin root (rootLine) on clock c,
// and no list yet.
func trustLookup(t *testing.T, c *testClock, rootLine string, ticks func() (<-chan time.Time, func())) *Lookup {
	t.Helper()
	return mustLookup(t, LookupOptions{
		LocalAdmins: map[string]config.UserConfig{"root": localAdmin(rootLine)},
		Now:         c.now,
		TrustTicks:  ticks,
	}, nil)
}

// adaAndBob is a list with ada (an admin) and bob.
func adaAndBob(t *testing.T) *List {
	return decoded(t,
		listEntry("ada", true, listKey(keyAda, "laptop", "")),
		listEntry("bob", false, listKey(keyBob, "desk", "")),
	)
}

// install installs list as pull seq, fetched at fetchedAt and trusted for
// trust, and fails the test unless it was published.
func install(t *testing.T, l *Lookup, list *List, fetchedAt time.Time, trust time.Duration, seq uint64) {
	t.Helper()
	ok, err := l.Install(list, fetchedAt, trust, seq)
	if err != nil || !ok {
		t.Fatalf("Install(seq %d) = %v, %v; want published", seq, ok, err)
	}
}

// resolves reports whether fp resolves, and to whom.
func resolves(l *Lookup, fp string) (string, bool) {
	u, _, ok := l.UserForKey(fp)
	return u.Name, ok
}

// hasMonotonic reports whether t carries a monotonic clock reading. Its
// String form shows one as "m=...", and == tells it apart from the same
// instant without one.
func hasMonotonic(t time.Time) bool {
	return strings.Contains(t.String(), " m=") || t != t.Round(0)
}

// TestLookupTrustDeadlineIsOnTheWallClock checks, on real time.Now()
// values, that the stored fetched-at instant and deadline, and the now the
// trust time is compared with, carry no monotonic reading: Go compares two
// times that both carry one on that reading, which does not count a sleeping
// host's time.
func TestLookupTrustDeadlineIsOnTheWallClock(t *testing.T) {
	if !hasMonotonic(time.Now()) {
		t.Skip("this platform's time.Now carries no monotonic reading; the test would prove nothing")
	}
	rootLine, _ := newTestKey(t)
	l := mustLookup(t, LookupOptions{LocalAdmins: map[string]config.UserConfig{"root": localAdmin(rootLine)}}, nil)
	fetched := time.Now()
	if !hasMonotonic(fetched) {
		t.Fatal("time.Now() carries no monotonic reading")
	}
	install(t, l, adaAndBob(t), fetched, time.Hour, 1)
	s := l.snap.Load()
	if hasMonotonic(s.fetchedAt) || hasMonotonic(s.deadline) {
		t.Errorf("stored fetched-at %q and deadline %q; want no monotonic reading", s.fetchedAt, s.deadline)
	}
	if !s.deadline.Equal(fetched.Add(time.Hour)) {
		t.Errorf("deadline %s, want %s", s.deadline, fetched.Add(time.Hour))
	}
	st := l.State()
	if hasMonotonic(st.FetchedAt) || hasMonotonic(st.Deadline) {
		t.Errorf("State: fetched-at %q deadline %q carry a monotonic reading", st.FetchedAt, st.Deadline)
	}
	if now := l.now(); hasMonotonic(now) {
		t.Errorf("the now the deadline is compared with, %q, carries a monotonic reading", now)
	}
	if _, ok := resolves(l, fpOf(t, keyAda)); !ok {
		t.Error("ada does not resolve inside the trust time")
	}
}

// TestLookupTrustBoundary checks the list is trusted until its deadline and
// not at it: now == deadline is expired. The local admin is never subject to
// it.
func TestLookupTrustBoundary(t *testing.T) {
	rootLine, rootFP := newTestKey(t)
	c := &testClock{t: trustT0}
	l := trustLookup(t, c, rootLine, nil)
	install(t, l, adaAndBob(t), trustT0, time.Hour, 1)

	c.set(trustT0.Add(time.Hour - time.Nanosecond))
	if who, ok := resolves(l, fpOf(t, keyAda)); !ok || who != "ada" {
		t.Fatalf("one nanosecond before the deadline ada's key resolves to %q %v", who, ok)
	}
	if l.State().Expired {
		t.Fatal("expired before the deadline")
	}
	c.set(trustT0.Add(time.Hour))
	if _, ok := resolves(l, fpOf(t, keyAda)); ok {
		t.Error("ada's key resolves at the deadline")
	}
	if _, ok := l.User("bob"); ok {
		t.Error("bob is a user at the deadline")
	}
	if got := names(l.Users()); got != "root" {
		t.Errorf("Users() at the deadline = %q, want root", got)
	}
	if who, ok := resolves(l, rootFP); !ok || who != "root" {
		t.Errorf("the local admin's key at the deadline resolves to %q %v", who, ok)
	}
	if st := l.State(); !st.Expired || !st.Listed || st.Seq != 1 {
		t.Errorf("State at the deadline = %+v; want expired, listed, seq 1", st)
	}
}

// TestLookupClockJumpedPastTheDeadline checks a clock that jumps forward past
// the deadline (a host that slept) ends the trust at once, and the next trust
// check publishes the end: listed users are no longer users of it, the local
// admin is.
func TestLookupClockJumpedPastTheDeadline(t *testing.T) {
	rootLine, _ := newTestKey(t)
	c := &testClock{t: trustT0}
	l := trustLookup(t, c, rootLine, nil)
	var pubs []Publication
	l.SetPublicationHook(func(p Publication) { pubs = append(pubs, p) })
	install(t, l, adaAndBob(t), trustT0, 72*time.Hour, 1)

	c.set(trustT0.Add(time.Minute))
	if _, ok := resolves(l, fpOf(t, keyBob)); !ok {
		t.Fatal("bob does not resolve inside the trust time")
	}
	c.set(trustT0.Add(30 * 24 * time.Hour))
	if _, ok := resolves(l, fpOf(t, keyBob)); ok {
		t.Error("bob resolves after the clock jumped past the deadline")
	}
	if !l.checkTrust() {
		t.Fatal("the trust check did not publish the end of the trust time")
	}
	if len(pubs) != 2 {
		t.Fatalf("%d publications, want the list's and the end's", len(pubs))
	}
	end := pubs[1]
	if !end.TrustEnded || end.Gen != 3 || end.Has("ada") || end.Has("bob") || !end.Has("root") {
		t.Errorf("end publication = gen %d ended %v ada %v bob %v root %v; want gen 3, ended, only root",
			end.Gen, end.TrustEnded, end.Has("ada"), end.Has("bob"), end.Has("root"))
	}
	if pubs[0].TrustEnded || !pubs[0].Has("ada") || !pubs[0].Has("bob") || pubs[0].Has("carol") {
		t.Errorf("the list's publication = ended %v ada %v bob %v carol %v", pubs[0].TrustEnded, pubs[0].Has("ada"), pubs[0].Has("bob"), pubs[0].Has("carol"))
	}
	// Published once: the next check finds it published.
	if l.checkTrust() || len(pubs) != 2 {
		t.Errorf("a second trust check published again (%d publications)", len(pubs))
	}
}

// TestLookupLatchHoldsWhenTheClockMovesBack checks a list whose deadline a
// reader found passed stays expired when the clock moves back before the
// deadline, the trust check still publishes the end, and only a newly
// installed list is trusted again.
func TestLookupLatchHoldsWhenTheClockMovesBack(t *testing.T) {
	rootLine, rootFP := newTestKey(t)
	c := &testClock{t: trustT0}
	l := trustLookup(t, c, rootLine, nil)
	install(t, l, adaAndBob(t), trustT0, time.Hour, 1)

	c.set(trustT0.Add(2 * time.Hour))
	if _, ok := resolves(l, fpOf(t, keyAda)); ok {
		t.Fatal("ada resolves past the deadline")
	}
	c.set(trustT0.Add(time.Minute))
	if _, ok := resolves(l, fpOf(t, keyAda)); ok {
		t.Error("ada resolves again after the clock moved back: the latch did not hold")
	}
	if _, ok := l.User("ada"); ok || names(l.Users()) != "root" || !l.State().Expired {
		t.Error("a reader other than UserForKey answers for the list after the clock moved back")
	}
	if !l.checkTrust() {
		t.Error("the trust check did not publish the end of a latched list")
	}
	if _, ok := resolves(l, fpOf(t, keyAda)); ok {
		t.Error("ada resolves after the end was published")
	}
	if _, ok := resolves(l, rootFP); !ok {
		t.Error("the local admin does not resolve")
	}

	// A new list, fetched now, is trusted again.
	install(t, l, adaAndBob(t), c.now(), time.Hour, 2)
	if who, ok := resolves(l, fpOf(t, keyAda)); !ok || who != "ada" {
		t.Errorf("after a new list ada's key resolves to %q %v", who, ok)
	}
	if st := l.State(); st.Expired || st.Seq != 2 || st.Gen != 4 {
		t.Errorf("State after the new list = %+v; want trusted, seq 2, gen 4", st)
	}
}

// TestLookupTrustEndsBetweenTwoReaderCalls checks two reader calls of one
// decision (as the SSH door makes: the key's check, then the connection's
// identity) that straddle the deadline: the first answers for the list, the
// second does not, and each answered from one state.
func TestLookupTrustEndsBetweenTwoReaderCalls(t *testing.T) {
	rootLine, _ := newTestKey(t)
	c := &testClock{t: trustT0}
	l := trustLookup(t, c, rootLine, nil)
	install(t, l, adaAndBob(t), trustT0, time.Hour, 1)
	fp := fpOf(t, keyAda)

	c.set(trustT0.Add(time.Hour - time.Second))
	if !users.HasKey(l, fp) {
		t.Fatal("ada's key is not accepted before the deadline")
	}
	c.set(trustT0.Add(time.Hour))
	if _, _, ok := users.ResolveBound(l, fp, users.Binding{Bound: true, Name: "ada"}, c.now()); ok {
		t.Error("ada's key resolves in the second call, made at the deadline")
	}
	if users.HasKey(l, fp) {
		t.Error("ada's key is accepted at the deadline")
	}
}

// TestLookupReaderDuringAPublish checks a reader that has loaded its state
// answers from it whole while a publication replaces it (forced by the
// test's load hook), the next call answers from the new state, and readers
// called from the publication hook already see the new state.
func TestLookupReaderDuringAPublish(t *testing.T) {
	rootLine, _ := newTestKey(t)
	c := &testClock{t: trustT0}
	l := trustLookup(t, c, rootLine, nil)
	install(t, l, adaAndBob(t), trustT0, time.Hour, 1)
	onlyBob := decoded(t, listEntry("bob", false, listKey(keyBob, "desk", "")))

	var inHook struct {
		ada bool
		gen uint64
	}
	l.SetPublicationHook(func(p Publication) {
		_, inHook.ada = resolves(l, fpOf(t, keyAda))
		inHook.gen = l.State().Gen
	})
	var published bool
	l.loaded = func() {
		// Once: the readers in the publication hook load too.
		l.loaded = nil
		ok, err := l.Install(onlyBob, trustT0, time.Hour, 2)
		published = ok && err == nil
	}

	u, k, ok := l.UserForKey(fpOf(t, keyAda))
	l.loaded = nil
	if !published {
		t.Fatal("the publication inside the reader did not happen")
	}
	if !ok || u.Name != "ada" || !u.Admin || k.Source != users.SourceDirectory {
		t.Errorf("the reader that loaded before the publication = %+v %+v %v; want ada's key, from its one state", u, k, ok)
	}
	if _, ok := resolves(l, fpOf(t, keyAda)); ok {
		t.Error("the next reader still answers from the replaced state")
	}
	if inHook.ada || inHook.gen != 3 {
		t.Errorf("readers in the publication hook saw ada %v gen %d; want the new state (no ada, gen 3)", inHook.ada, inHook.gen)
	}

	// Users during a publication: the whole list from the one state.
	l.loaded = func() {
		l.loaded = nil
		install(t, l, adaAndBob(t), trustT0, time.Hour, 3)
	}
	if got := names(l.Users()); got != "bob root" {
		t.Errorf("Users() loaded before the publication = %q, want the old state's bob root", got)
	}
	if got := names(l.Users()); got != "ada bob root" {
		t.Errorf("Users() after the publication = %q", got)
	}
}

// TestLookupStaleResultDiscarded checks a pull's result whose sequence
// number is not greater than the last one published is discarded with
// nothing changed (no publication, no hook), and an end of the trust time
// made from a state that has been replaced since is discarded too.
func TestLookupStaleResultDiscarded(t *testing.T) {
	rootLine, _ := newTestKey(t)
	c := &testClock{t: trustT0}
	l := trustLookup(t, c, rootLine, nil)
	hooks := 0
	l.SetPublicationHook(func(Publication) { hooks++ })
	install(t, l, adaAndBob(t), trustT0, time.Hour, 5)

	onlyBob := decoded(t, listEntry("bob", false, listKey(keyBob, "desk", "")))
	for _, seq := range []uint64{4, 5, 1} {
		ok, err := l.Install(onlyBob, trustT0, time.Hour, seq)
		if err != nil || ok {
			t.Errorf("Install(seq %d) after seq 5 = %v, %v; want discarded", seq, ok, err)
		}
	}
	if st := l.State(); st.Gen != 2 || st.Seq != 5 || hooks != 1 {
		t.Errorf("after stale results: gen %d seq %d hooks %d; want 2, 5, 1", st.Gen, st.Seq, hooks)
	}
	if _, ok := resolves(l, fpOf(t, keyAda)); !ok {
		t.Error("a stale result removed ada")
	}

	// The end of the trust time, made from the state of seq 5, loses to a
	// newer pull published before it.
	c.set(trustT0.Add(2 * time.Hour))
	end := trustEndOf(l.snap.Load())
	install(t, l, onlyBob, c.now(), time.Hour, 6)
	if _, ok := l.publish(end, noPull); ok {
		t.Fatal("an end of the trust time made from a replaced state was published")
	}
	if who, ok := resolves(l, fpOf(t, keyBob)); !ok || who != "bob" {
		t.Errorf("bob's key resolves to %q %v; the newer list was undone", who, ok)
	}
	if st := l.State(); st.Seq != 6 || st.Expired || hooks != 2 {
		t.Errorf("State = %+v hooks %d; want seq 6, trusted, 2 hooks", st, hooks)
	}
	// After the end is published it still holds seq 6: a result of that
	// pull or an older one is discarded, and the next pull publishes.
	c.set(trustT0.Add(4 * time.Hour))
	if !l.checkTrust() {
		t.Fatal("the end of seq 6's trust time was not published")
	}
	for _, seq := range []uint64{6, 5} {
		ok, err := l.Install(adaAndBob(t), c.now(), time.Hour, seq)
		if err != nil || ok {
			t.Errorf("Install(seq %d) after the end of seq 6's trust time = %v, %v; want discarded", seq, ok, err)
		}
	}
	if st := l.State(); st.Seq != 6 || !st.Expired || st.Gen != 4 || hooks != 3 {
		t.Errorf("State after stale results following the end = %+v hooks %d; want seq 6, expired, gen 4, 3 hooks", st, hooks)
	}
	if _, ok := resolves(l, fpOf(t, keyAda)); ok {
		t.Error("a stale result after the end of the trust time brought ada back")
	}
	install(t, l, adaAndBob(t), c.now(), time.Hour, 7)
	if st := l.State(); st.Seq != 7 || st.Expired {
		t.Errorf("State after a pull following the end = %+v", st)
	}
}

// TestLookupLatchSetByAnotherReader forces the interleaving of two readers
// of one snapshot (the test's latch-read hook): reader A reads the latch
// unset; reader B finds the deadline passed and sets it; the clock moves back
// before the deadline; A takes its clock reading. A must not answer from the
// list: it reads the latch again before accepting the listed users. The same
// for State.
func TestLookupLatchSetByAnotherReader(t *testing.T) {
	rootLine, rootFP := newTestKey(t)
	c := &testClock{t: trustT0.Add(30 * time.Minute)}
	l := trustLookup(t, c, rootLine, nil)
	install(t, l, adaAndBob(t), trustT0, time.Hour, 1)
	t.Cleanup(func() { l.latchRead = nil })

	// raced makes reader B end the trust time of the current state while
	// reader A is between its first latch read and its clock reading.
	raced := func(deadline time.Time) func() {
		return func() {
			l.latchRead = nil
			back := c.now()
			c.set(deadline.Add(time.Hour))
			if _, ok := resolves(l, fpOf(t, keyBob)); ok {
				t.Error("reader B: bob resolves past the deadline")
			}
			c.set(back)
		}
	}

	l.latchRead = raced(trustT0.Add(time.Hour))
	if who, ok := resolves(l, fpOf(t, keyAda)); ok {
		t.Errorf("reader A answered ada's key as %q from the list after reader B had ended its trust time", who)
	}
	if l.latchRead != nil {
		t.Fatal("the latch-read hook did not run: the interleaving was not forced")
	}
	if who, ok := resolves(l, rootFP); !ok || who != "root" {
		t.Errorf("the local admin's key resolves to %q %v", who, ok)
	}

	// State, on a new list (deadline an hour from the clock's reading).
	install(t, l, adaAndBob(t), c.now(), time.Hour, 2)
	l.latchRead = raced(c.now().Add(time.Hour))
	if st := l.State(); !st.Expired || st.Seq != 2 {
		t.Errorf("State read while reader B ended the trust time = %+v; want expired, seq 2", st)
	}
	if l.latchRead != nil {
		t.Fatal("the latch-read hook did not run for State")
	}
}

// TestLookupInstallAlreadyEnded checks a list whose trust time has already
// ended when it is installed (fetchedAt + trust <= now, a late result) is
// published at once as the end of the trust time: one publication, for the
// local admins only, logged once; the trust ticker publishes no second one.
// A second such list after it is published without a second log line; a list
// inside its trust time is trusted again.
func TestLookupInstallAlreadyEnded(t *testing.T) {
	rootLine, rootFP := newTestKey(t)
	c := &testClock{t: trustT0.Add(time.Hour)}
	l := trustLookup(t, c, rootLine, nil)
	var pubs []Publication
	l.SetPublicationHook(func(p Publication) { pubs = append(pubs, p) })
	logged := captureLog(t)

	// Exactly at the deadline: ended.
	install(t, l, adaAndBob(t), trustT0, time.Hour, 1)
	if len(pubs) != 1 {
		t.Fatalf("%d publications, want one", len(pubs))
	}
	if p := pubs[0]; !p.TrustEnded || p.Has("ada") || p.Has("bob") || !p.Has("root") || p.Gen != 2 {
		t.Errorf("publication = gen %d ended %v ada %v bob %v root %v; want gen 2, ended, only root",
			p.Gen, p.TrustEnded, p.Has("ada"), p.Has("bob"), p.Has("root"))
	}
	if _, ok := resolves(l, fpOf(t, keyAda)); ok {
		t.Error("ada resolves from a list installed past its trust time")
	}
	if who, ok := resolves(l, rootFP); !ok || who != "root" {
		t.Errorf("the local admin's key resolves to %q %v", who, ok)
	}
	if st := l.State(); !st.Expired || !st.Listed || st.Seq != 1 || st.Gen != 2 {
		t.Errorf("State = %+v; want expired, listed, seq 1, gen 2", st)
	}
	if l.checkTrust() || len(pubs) != 1 {
		t.Errorf("the trust check published a second end (%d publications)", len(pubs))
	}
	if n := strings.Count(logged.String(), "TRUST ENDED"); n != 1 {
		t.Errorf("TRUST ENDED logged %d times, want once: %q", n, logged.String())
	}

	// Another late list after the end: published, ended, not logged again.
	install(t, l, adaAndBob(t), trustT0.Add(-time.Hour), time.Hour, 2)
	if len(pubs) != 2 || !pubs[1].TrustEnded || pubs[1].Has("ada") {
		t.Errorf("the second late list's publication = %+v (%d publications)", pubs[len(pubs)-1], len(pubs))
	}
	if n := strings.Count(logged.String(), "TRUST ENDED"); n != 1 {
		t.Errorf("TRUST ENDED logged %d times after a second late list, want once", n)
	}

	// A list inside its trust time is trusted.
	install(t, l, adaAndBob(t), c.now(), time.Hour, 3)
	if who, ok := resolves(l, fpOf(t, keyAda)); !ok || who != "ada" || pubs[2].TrustEnded {
		t.Errorf("after a list inside its trust time ada resolves to %q %v (ended %v)", who, ok, pubs[2].TrustEnded)
	}
}

// TestLookupPublicationHooksNeverOverlap checks every hook runs under the
// lock that orders publications (so no two overlap), and that hooks of
// publications made at once from many goroutines never run together.
func TestLookupPublicationHooksNeverOverlap(t *testing.T) {
	rootLine, _ := newTestKey(t)
	c := &testClock{t: trustT0}
	l := trustLookup(t, c, rootLine, nil)
	var inHook, overlaps, unlocked, hooks atomic.Int32
	l.SetPublicationHook(func(Publication) {
		hooks.Add(1)
		if l.pubMu.TryLock() {
			l.pubMu.Unlock()
			unlocked.Add(1)
		}
		if inHook.Add(1) > 1 {
			overlaps.Add(1)
		}
		runtime.Gosched()
		inHook.Add(-1)
	})

	const n = 32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 1; i <= n; i++ {
		wg.Add(1)
		go func(seq uint64) {
			defer wg.Done()
			<-start
			if _, err := l.Install(adaAndBob(t), trustT0, time.Hour, seq); err != nil {
				t.Errorf("Install(seq %d): %v", seq, err)
			}
		}(uint64(i))
	}
	close(start)
	wg.Wait()
	if hooks.Load() == 0 {
		t.Fatal("no hook ran")
	}
	if u := unlocked.Load(); u != 0 {
		t.Errorf("%d hooks ran without the publication lock held", u)
	}
	if o := overlaps.Load(); o != 0 {
		t.Errorf("%d hooks ran while another was running", o)
	}
	if st := l.State(); st.Seq != n {
		t.Errorf("State after the publications = %+v; want seq %d", st, n)
	}
}

// syncBuffer is a bytes.Buffer safe for the log package's writer and a
// test's reads at once.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

// captureLog sends the log package's output to a buffer for the rest of the
// test, and restores the writer it had at the end.
func captureLog(t *testing.T) *syncBuffer {
	t.Helper()
	prev := log.Writer()
	buf := &syncBuffer{}
	log.SetOutput(buf)
	t.Cleanup(func() { log.SetOutput(prev) })
	return buf
}

// TestLookupLocalAdminsOutsideTheTrustTime checks the local admins are never
// subject to the trust time, while each key's own expiry still applies.
func TestLookupLocalAdminsOutsideTheTrustTime(t *testing.T) {
	liveLine, liveFP := newTestKey(t)
	soonLine, soonFP := newTestKey(t)
	local := localAdmin(liveLine, soonLine)
	soonAt := trustT0.Add(3 * time.Hour)
	local.Keys[1].Expires = soonAt.Format(time.RFC3339)
	c := &testClock{t: trustT0}
	l := mustLookup(t, LookupOptions{LocalAdmins: map[string]config.UserConfig{"root": local}, Now: c.now}, nil)

	// No list yet, and then a list whose trust has ended.
	if _, ok := resolves(l, liveFP); !ok {
		t.Error("the local admin does not resolve with no list")
	}
	install(t, l, adaAndBob(t), trustT0, time.Hour, 1)
	c.set(trustT0.Add(2 * time.Hour))
	l.checkTrust()
	for _, fp := range []string{liveFP, soonFP} {
		if _, _, ok := users.ResolveBound(l, fp, users.Binding{Bound: true, Name: "root"}, c.now()); !ok {
			t.Errorf("the local admin's key %s is refused after the trust time", fp)
		}
	}
	if _, _, ok := users.ResolveBound(l, soonFP, users.Binding{Bound: true, Name: "root"}, soonAt); ok {
		t.Error("the local admin's key is accepted at its own expiry")
	}
	if _, _, ok := users.ResolveBound(l, liveFP, users.Binding{Bound: true, Name: "root"}, soonAt.Add(time.Hour)); !ok {
		t.Error("the local admin's other key is refused")
	}
}

// TestLookupWatchTrust checks the ticker: ticks before the deadline publish
// nothing; the first tick past it publishes the end once, logged once and
// loudly; later ticks publish nothing more; the watch stops its ticker when
// its context ends.
func TestLookupWatchTrust(t *testing.T) {
	rootLine, _ := newTestKey(t)
	c := &testClock{t: trustT0}
	ticks := make(chan time.Time)
	stopped := make(chan struct{})
	l := trustLookup(t, c, rootLine, func() (<-chan time.Time, func()) {
		return ticks, func() { close(stopped) }
	})
	pubs := make(chan Publication, 4)
	l.SetPublicationHook(func(p Publication) { pubs <- p })
	install(t, l, adaAndBob(t), trustT0, time.Hour, 1)
	<-pubs

	logged := captureLog(t)

	// The watcher is cancelled and waited for on every path out of the test
	// (a cleanup registered after captureLog's runs before it, so the
	// watcher has stopped logging when the writer is restored).
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		l.WatchTrust(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	// tick sends one tick; the next send completes only once WatchTrust is
	// back at its select, so after tick(); tick() the first has been handled.
	// A watcher that stopped taking ticks fails the test rather than hanging
	// it.
	tick := func() {
		t.Helper()
		select {
		case ticks <- c.now():
		case <-time.After(5 * time.Second):
			t.Fatal("WatchTrust did not take a tick")
		}
	}

	c.set(trustT0.Add(time.Hour - time.Second))
	tick()
	tick()
	if len(pubs) != 0 {
		t.Fatalf("a tick before the deadline published: %+v", <-pubs)
	}
	c.set(trustT0.Add(time.Hour))
	tick()
	select {
	case p := <-pubs:
		if !p.TrustEnded || p.Has("ada") || !p.Has("root") {
			t.Errorf("the tick at the deadline published %+v; want the end of the trust time", p)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the tick at the deadline published nothing")
	}
	tick()
	tick()
	if len(pubs) != 0 {
		t.Errorf("the end was published again: %+v", <-pubs)
	}
	cancel()
	<-done
	select {
	case <-stopped:
	default:
		t.Error("WatchTrust did not stop its ticker")
	}
	if n := strings.Count(logged.String(), "TRUST ENDED"); n != 1 {
		t.Errorf("the end of the trust time was logged %d times, want once: %q", n, logged.String())
	}
}

// TestLookupInstallRefuses checks what Install will not install.
func TestLookupInstallRefuses(t *testing.T) {
	rootLine, _ := newTestKey(t)
	c := &testClock{t: trustT0}
	l := trustLookup(t, c, rootLine, nil)
	bad := &List{Users: []User{{Name: "ada"}}}
	for _, tc := range []struct {
		name      string
		list      *List
		fetchedAt time.Time
		trust     time.Duration
		seq       uint64
		want      string
	}{
		{"no list", nil, trustT0, time.Hour, 1, "no list"},
		{"no trust time", adaAndBob(t), trustT0, 0, 1, "not positive"},
		{"a negative trust time", adaAndBob(t), trustT0, -time.Hour, 1, "not positive"},
		{"no fetched-at", adaAndBob(t), time.Time{}, time.Hour, 1, "fetched-at"},
		{"a fetched-at after the clock", adaAndBob(t), trustT0.Add(time.Nanosecond), time.Hour, 1, "after this server's clock"},
		{"seq 0", adaAndBob(t), trustT0, time.Hour, 0, "sequence number"},
		{"a list DecodeList could not make", bad, trustT0, time.Hour, 1, "has no key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ok, err := l.Install(tc.list, tc.fetchedAt, tc.trust, tc.seq)
			if ok || err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Install = %v, %v; want refused with %q", ok, err, tc.want)
			}
		})
	}
	if st := l.State(); st.Gen != 1 || st.Listed {
		t.Errorf("State after refusals = %+v; want the first, with no list", st)
	}
}
