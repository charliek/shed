package directory

import (
	"context"
	"crypto/subtle"
	"crypto/x509"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/users"
)

// The runtime's fixed values (plan section 3.6): none of them is a setting.
const (
	// RetryInterval is how long after a failed attempt the next one is due,
	// unless a Retry-After asks for longer (at most MaxRetryAfter).
	RetryInterval = 15 * time.Minute
	// PullTimeout bounds the network work of one pull, scheduled or asked
	// for by an admin: the token request, the directory request and the one
	// new login after a refused token, together. It is measured on the
	// runtime's timer (RuntimeOptions.Timer) and ends the pull's requests
	// when it passes. The local storage work around them (reading the
	// enrolment before, writing or removing the kept copy after) is not
	// interruptible and is outside this bound: a state directory on a disk
	// that blocks holds the pull, and every pull queued behind it, as long
	// as the disk does.
	PullTimeout = 30 * time.Second
	// skewWarning is how far an answer's Date may be from the server's clock
	// before the server warns (once per pull).
	skewWarning = 30 * time.Second
	// maxScheduleWait is the longest the schedule waits on one timer before
	// it reads the wall clock again: a timer does not count the time a host
	// spends asleep, the wall clock does.
	maxScheduleWait = time.Minute
	// UnknownKeyEvery is how often an unknown key may start a pull while the
	// last pull succeeded (RefetchForUnknownKey). While the server is in the
	// failed state, it is RetryInterval.
	UnknownKeyEvery = time.Minute
	// UnknownKeyWait is the one deadline the login that takes the unknown-key
	// slot waits under: queueing behind a pull already running, the token
	// request, the directory request and the one new login after a refused
	// token, together. Measured on the runtime's timer.
	UnknownKeyWait = 20 * time.Second
)

// SourceState is the state of a directory server's user source.
type SourceState string

// The states of the source.
const (
	// StateTrusted: the server answers for a list inside its trust time.
	StateTrusted SourceState = "trusted"
	// StateExpired: the trust time of the list the server holds (or of the
	// kept copy it found at start) has ended; only the local admins can log
	// in until a good pull.
	StateExpired SourceState = "expired"
	// StateNoCopy: enrolled, but no list yet (no usable kept copy and no
	// good pull); only the local admins can log in.
	StateNoCopy SourceState = "no-copy"
	// StateNotEnrolled: no enrolment, or one for another issuer or
	// organization than the config's; nothing is sent, and only the local
	// admins can log in.
	StateNotEnrolled SourceState = "not-enrolled"
	// StateEnrolmentUnusable: the enrolment, or its directory, failed a
	// check; nothing is sent, and only the local admins can log in.
	StateEnrolmentUnusable SourceState = "enrolment-unusable"
)

// CopyCondition is the condition of the kept copy (users.json).
type CopyCondition string

// The conditions of the kept copy.
const (
	// CopyNone: there is no kept copy. CopyError says so when one was
	// removed because the trust time of its list ended.
	CopyNone CopyCondition = "none"
	// CopyKept: the kept copy is the list the server answers for (written at
	// the last good pull, or the one found at start).
	CopyKept CopyCondition = "kept"
	// CopyUnused: the copy found at start was not used (CopyError says why);
	// the next good pull replaces it.
	CopyUnused CopyCondition = "unused"
	// CopyNotWritten: the last good pull's copy could not be kept (not
	// written, or written for a list that was not published), and the copy
	// on disk was removed: a restart starts with only the local admins until
	// a good pull.
	CopyNotWritten CopyCondition = "not-written"
	// CopyStale: a copy on disk that must not be used at the next start could
	// not be removed (CopyError says which, and why): the old copy after a
	// failed write, the new, unpublished copy after its list was not
	// published, or a copy whose trust time ended. A restart would bring
	// back its list for what remains of its trust time (for one whose trust
	// time ended: while the clock reads before that end).
	CopyStale CopyCondition = "stale-on-disk"
)

// RuntimeOptions are what a Runtime is built from.
type RuntimeOptions struct {
	// Lookup is the user lookup the runtime publishes lists to. Its clock
	// (LookupOptions.Now) is the runtime's.
	Lookup *Lookup
	// StateDir is <server state dir>/directory: the enrolment and the kept
	// copy.
	StateDir string
	// Issuer is the configured issuer (auth.directory's URL, normalized).
	Issuer string
	// Organization is the configured organization, "" for none.
	Organization string
	// AllowPlainHTTP is auth.directory.allow_plain_http.
	AllowPlainHTTP bool
	// RootCAs, when set, replace the system's roots for an https issuer:
	// for tests.
	RootCAs *x509.CertPool
	// Refresh is how long after a good pull the next is due, and Trust how
	// long a list is honoured after its answer arrived.
	Refresh, Trust time.Duration
	// Timer starts a timer of d on the runtime's clock and returns its
	// channel and how to stop it; nil uses time.NewTimer. The schedule and
	// each pull's bound (PullTimeout) run on it. For tests.
	Timer func(d time.Duration) (<-chan time.Time, func())
	// Elapsed reads the clock the runtime's waits are measured on (a pending
	// Retry-After, the unknown-key slot's intervals): the
	// difference of two readings is the time elapsed between them, whatever
	// the wall clock was set to meanwhile. nil is the lookup's clock as it
	// reads, unrounded: in production time.Now, whose monotonic reading makes
	// the difference elapsed time. For tests, which move the wall clock and
	// elapsed time apart.
	Elapsed func() time.Time
	// PullTimeout bounds one pull's network work; 0 is PullTimeout. For
	// tests.
	PullTimeout time.Duration
	// Logf is where the runtime logs; nil is log.Printf.
	Logf func(format string, args ...any)
}

// Runtime is a directory server's user source while it runs: the Lookup
// every front door asks (embedded), and the pulls that change it. It reads
// the enrolment before each pull, pulls one list at a time, keeps a copy of
// each good pull (written before the list is published), starts from that
// copy, and schedules the next pull.
//
// It is the users.Refresher of a directory server (`shed users refresh`), and
// its users.UnknownKeyRefetcher (a new person's first SSH login). Its Status
// is what an admin is shown.
type Runtime struct {
	*Lookup

	dir            string
	issuer         string
	organization   string
	allowPlainHTTP bool
	rootCAs        *x509.CertPool
	refresh, trust time.Duration
	pullTimeout    time.Duration
	timer          func(time.Duration) (<-chan time.Time, func())
	elapsed        func() time.Time
	logf           func(string, ...any)

	// ctx bounds every pull; cancelled when StartRefresh's context ends.
	ctx     context.Context
	cancel  context.CancelFunc
	started atomic.Bool
	wg      sync.WaitGroup
	// wake tells the schedule its next attempt may have moved.
	wake chan struct{}

	// fmu guards the flights: the pull running and the one waiting for it,
	// and stopped: set once the runtime's context has ended (stopping) or a
	// test waits for its goroutines (wait), after which no pull starts.
	fmu              sync.Mutex
	running, pending *flight
	stopped          bool
	// asked counts the pulls asked for (PullsAsked).
	asked uint64

	// copyMu orders the work on the kept copy on disk: a good pull holds it
	// from the write through the publication (so the copy is never removed
	// between the two), and the removal at the end of the trust time holds
	// it. No reader of the lookup and no status takes it.
	copyMu sync.Mutex

	// mu guards everything below. A publication made by a pull (Install in
	// Load and good, withdraw in notEnrolled) runs under it together with
	// the record of what it published, and Status and row read the lookup's
	// snapshot under it, so a status never pairs one list with another's
	// record. The publication hook runs inside it then: it must not take mu
	// (it does not: it closes SSH connections). The readers of the lookup
	// never take it.
	mu sync.Mutex
	// seq numbers the lists offered to the lookup (Install), in the order
	// their pulls started.
	seq uint64
	// client is the contract client of the enrolment it was made from
	// (clientOf), kept so its access token is reused.
	client   *Client
	clientOf *Enrolment
	// enrol is what the last read of the enrolment found.
	enrol enrolState
	// lastGood is when the answer of the list held (or of the copy refused
	// for its trust time) arrived, and version its version.
	lastGood time.Time
	version  string
	// pastTrust is the deadline of a kept copy refused at start for its
	// trust time: the source is expired until a good pull.
	pastTrust time.Time
	copyCond  CopyCondition
	copyErr   string
	// copyEnd is, when copyCond is about the end of a trust time (the copy
	// removed, or stale because its removal failed), the end it is about;
	// zero when copyCond is about a pull's write.
	copyEnd     time.Time
	lastAttempt time.Time
	lastErr     string
	next        time.Time
	// retryEnd is the end of the pending Retry-After (zero: none), on the
	// elapsed clock (RuntimeOptions.Elapsed): the runtime's one outstanding
	// Retry-After. Neither the schedule nor an unknown key starts a pull
	// before it; only an admin's refresh may. A later failure never moves it
	// earlier; a good pull, a read of the enrolment that finds the server not
	// enrolled, or its own end clears it.
	retryEnd time.Time
	// unknownAt is when an unknown key last took the unknown-key slot (zero:
	// never), on the elapsed clock: a wall clock set forward does not free
	// the slot early, nor one set back hold it.
	unknownAt time.Time

	// What has been logged, for the lines logged once per change of state.
	failClass   string
	enrolLogged string
	goodLogged  string
	skipsLogged string
	endLogged   string

	// Seams for tests in this package.
	writeCopy    func(d *StateDir, data []byte) error
	removeCopy   func(d *StateDir) error
	afterAttempt func(users.DirectoryRefresh)
	// beforeWait runs when the schedule, having read the clock (now) and
	// armed its timer, is about to wait.
	beforeWait func(now time.Time)
	// afterDone runs in the pull goroutine after a pull's callers were told
	// its result and before the next pull asked for is taken up.
	afterDone func()
}

var (
	_ users.Refresher           = (*Runtime)(nil)
	_ users.UnknownKeyRefetcher = (*Runtime)(nil)
)

// enrolState is what a read of the enrolment found: state "" when the
// server is enrolled with the configured service.
type enrolState struct {
	state  SourceState
	reason string
	// clientID and organization (the accepted one) are the enrolment's,
	// when enrolled.
	clientID     string
	organization string
}

// flight is one pull: callers that asked for it wait on done, then read row.
type flight struct {
	done chan struct{}
	row  users.DirectoryRefresh
	// unknownOnly: the pull exists only for an unknown key (made by
	// RefetchForUnknownKey, joined by no admin's refresh and not by the
	// schedule); prevUnknown is when the unknown-key slot had been taken
	// before the take that made it (zero: never). Such a pull is checked
	// again against the unknown-key policy when it would start
	// (unknownMayStart). Both are written under fmu, and read by the pull
	// goroutine once the pull is the running one, after which nobody joins
	// it.
	unknownOnly bool
	prevUnknown time.Time
	// admin: an admin's refresh asked for the pull, or joined it. A pull no
	// admin asked for (the schedule's) is not sent while a Retry-After is
	// pending when it would start (scheduleMayStart). Written under fmu,
	// read as unknownOnly is.
	admin bool
}

// asker is who asks for a pull (ask).
type asker int

const (
	// askAdmin is an admin's refresh (Refresh): its pull is sent whatever
	// Retry-After is pending.
	askAdmin asker = iota
	// askSchedule is the schedule: its pull is not sent while a Retry-After
	// is pending.
	askSchedule
	// askUnknown is an unknown key that took the unknown-key slot: its pull,
	// while nobody else joins it, is checked against the unknown-key policy
	// when it would start.
	askUnknown
)

// NewRuntime builds the runtime over opts.Lookup. Nothing is read and no
// pull starts until Load and StartRefresh.
func NewRuntime(opts RuntimeOptions) (*Runtime, error) {
	switch {
	case opts.Lookup == nil:
		return nil, errors.New("directory: the runtime needs a lookup")
	case opts.StateDir == "":
		return nil, errors.New("directory: the runtime needs its state directory")
	case opts.Refresh <= 0 || opts.Trust <= 0:
		return nil, fmt.Errorf("directory: refresh %s and trust %s must be positive", opts.Refresh, opts.Trust)
	}
	if err := config.CheckDirectoryIssuer(opts.Issuer); err != nil {
		return nil, fmt.Errorf("directory: %w", err)
	}
	if opts.Organization != "" && !ValidOrganization(opts.Organization) {
		return nil, fmt.Errorf("directory: organization %q is not one", opts.Organization)
	}
	r := &Runtime{
		Lookup:         opts.Lookup,
		dir:            opts.StateDir,
		issuer:         opts.Issuer,
		organization:   opts.Organization,
		allowPlainHTTP: opts.AllowPlainHTTP,
		rootCAs:        opts.RootCAs,
		refresh:        opts.Refresh,
		trust:          opts.Trust,
		pullTimeout:    opts.PullTimeout,
		timer:          opts.Timer,
		logf:           opts.Logf,
		wake:           make(chan struct{}, 1),
		copyCond:       CopyNone,
		writeCopy:      func(d *StateDir, data []byte) error { return d.WriteFile(CopyFileName, data) },
		removeCopy:     func(d *StateDir) error { return d.Remove(CopyFileName) },
	}
	if r.pullTimeout <= 0 {
		r.pullTimeout = PullTimeout
	}
	if r.timer == nil {
		r.timer = func(d time.Duration) (<-chan time.Time, func()) {
			t := time.NewTimer(d)
			return t.C, func() { t.Stop() }
		}
	}
	if r.logf == nil {
		r.logf = log.Printf
	}
	r.elapsed = opts.Elapsed
	if r.elapsed == nil {
		r.elapsed = r.clock
	}
	r.ctx, r.cancel = context.WithCancel(context.Background())
	context.AfterFunc(r.ctx, r.stopping)
	return r, nil
}

// stopping runs once the runtime's context has ended: no pull starts from
// then on, and the client, its access token and the enrolment's private key
// are forgotten (the key's bytes cleared) now when no pull is running, or
// else by the pull goroutine when the pull running ends.
func (r *Runtime) stopping() {
	r.fmu.Lock()
	r.stopped = true
	idle := r.running == nil
	r.fmu.Unlock()
	if idle {
		r.forgetClient()
	}
}

// forgetClient drops the client and clears the private key it was made
// with.
func (r *Runtime) forgetClient() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.dropClientLocked()
}

// dropClientLocked is forgetClient with r.mu held.
func (r *Runtime) dropClientLocked() {
	if r.clientOf != nil {
		clear(r.clientOf.PrivateKey)
	}
	r.client, r.clientOf = nil, nil
}

// Load is the start-up step, run before the listeners start: it reads the
// enrolment and, when the server is enrolled, the kept copy, and publishes
// the copy's list at once when it may be used: it passes the file checks,
// its issuer and client id are the enrolment's, its organization is the
// accepted one, its fetched_at is not after the wall clock, its body passes
// today's rules, and it is inside its trust time counted with today's trust.
// A copy past its trust time is removed (removeExpiredCopy). It sends
// nothing: the first pull starts with StartRefresh.
//
// A copy that passes every check is used even if its list was never
// published: a server that stopped between writing a good pull's copy and
// publishing its list (good) left the service's own, fully checked answer.
func (r *Runtime) Load() {
	seq := r.nextSeq()
	d, e, es := r.openEnrolment()
	if d != nil {
		defer d.Close()
	}
	r.mu.Lock()
	r.enrol = es
	if es.state != "" {
		// The start-up lines say it; the pulls log it again only when it
		// changes.
		r.enrolLogged = es.key()
	}
	r.mu.Unlock()
	if es.state != "" {
		return
	}
	defer clear(e.PrivateKey)

	data, err := d.ReadFile(CopyFileName, MaxCopyFileSize)
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	if err != nil {
		r.copyUnused(fileError(d, CopyFileName, err))
		return
	}
	c, err := parseCopy(data, e, es.organization, r.trust, r.now())
	switch {
	case errors.Is(err, errCopyPastTrust):
		// The trust time of the copy's list has ended: it is never used
		// again, whatever the clock says later, so it is removed now (the
		// last good pull and its version stay known until the server
		// stops).
		r.mu.Lock()
		r.pastTrust = c.fetchedAt.Add(r.trust)
		r.lastGood, r.version = c.fetchedAt, c.list.Version
		r.copyCond, r.copyErr = CopyUnused, "past its trust time"
		r.mu.Unlock()
		r.removeEndedCopy(d)
		return
	case err != nil:
		r.copyUnused(fmt.Sprintf("%s: %v", d.Join(CopyFileName), err))
		return
	}
	// The list and the runtime's record of it change together (see good).
	r.mu.Lock()
	ok, err := r.Install(c.list, c.fetchedAt, r.trust, seq)
	if err != nil || !ok {
		r.mu.Unlock()
		r.copyUnused(fmt.Sprintf("%s: not installed: %v", d.Join(CopyFileName), err))
		return
	}
	r.lastGood, r.version = c.fetchedAt, c.list.Version
	r.copyCond, r.copyErr = CopyKept, ""
	r.mu.Unlock()
	r.logSkips(c.list.Version)
	// Its trust time may have ended between the copy's check and its
	// publication: then the copy goes.
	r.removeEndedCopy(d)
}

// copyUnused records and logs that the copy found at start is not used.
func (r *Runtime) copyUnused(why string) {
	r.mu.Lock()
	r.copyCond, r.copyErr = CopyUnused, why
	r.mu.Unlock()
	r.logf("directory: kept copy not used: %s; only the local admins can log in until the first good pull", why)
}

// StartRefresh starts the schedule (the first pull at once) and the trust
// time's watch, until ctx ends; a pull under way then ends too. It starts
// them once.
func (r *Runtime) StartRefresh(ctx context.Context) {
	if !r.started.CompareAndSwap(false, true) {
		return
	}
	context.AfterFunc(ctx, r.cancel)
	r.fmu.Lock()
	defer r.fmu.Unlock()
	if r.stopped {
		return
	}
	r.wg.Add(2)
	go func() {
		defer r.wg.Done()
		// After each tick's check, outside the publication and its lock:
		// when the trust time has ended, the kept copy is removed.
		r.watchTrust(r.ctx, r.removeExpiredCopy)
	}()
	go func() {
		defer r.wg.Done()
		r.schedule()
	}()
}

// wait waits for the runtime's goroutines to end, once its context has
// ended (the caller cancels it first). For tests. It marks the runtime
// stopped under fmu before it waits, so every goroutine counted in wg was
// counted before the wait began, and no pull starts after it.
func (r *Runtime) wait() {
	r.fmu.Lock()
	r.stopped = true
	r.fmu.Unlock()
	r.wg.Wait()
}

// Refresh pulls the list now (`shed users refresh`), ignoring a pending
// Retry-After, and reports the pull as the report's Directory row: a failed
// pull is a row, not an error. It joins a pull asked for but not yet
// started, and otherwise waits behind the one running, so the row is never
// of a pull that started before the call. Only ctx's end is an error; the
// pull then carries on and its result applies when it ends.
func (r *Runtime) Refresh(ctx context.Context) (users.RefreshReport, error) {
	f := r.request()
	select {
	case <-f.done:
		row := f.row
		return users.RefreshReport{Logins: []users.LoginRefresh{}, Directory: &row}, nil
	case <-ctx.Done():
		return users.RefreshReport{}, ctx.Err()
	}
}

// RefetchForUnknownKey is a new person's first SSH login (asked by the SSH
// server, in enforce mode, for a key the lookup does not know): it pulls the
// list, at most at the unknown-key rate, and reports whether the key is
// known, and not expired, afterwards.
//
// It first takes the unknown-key slot (takeUnknownKeySlot), atomically and
// before anything else; every caller that does not get it returns false at
// once, having started nothing and waited for nothing. The caller that gets
// it joins the single flight (a pull asked for and not yet started, or one
// queued behind the pull running) and waits for it under one deadline,
// UnknownKeyWait on the runtime's timer, started before it joins, so it
// covers the queueing as well as the pull's own requests; or until ctx ends.
// It then answers from the newest snapshot. A pull it stops waiting for
// carries on under its own bound (PullTimeout), and its result applies when
// it ends, as an admin's refresh whose caller gave up.
//
// A pull that exists only for the unknown key (no admin's refresh and not
// the schedule joined it) is checked against the policy again when it would
// start (unknownMayStart): queued behind a pull that was then answered with
// a Retry-After, or that failed, it may end without sending anything, and
// the caller is answered from the snapshot it has.
//
// A local admin's login never comes here (their keys are always known), and
// no reader of the lookup waits for a pull: a login that does not take the
// slot, or whose key is known, is never held by one, hung or not. An admin's
// refresh (Refresh) is not limited by the slot and does not take it.
func (r *Runtime) RefetchForUnknownKey(ctx context.Context, fingerprint string) bool {
	if fingerprint == "" {
		return false
	}
	prev, ok := r.takeUnknownKeySlot()
	if !ok {
		return false
	}
	r.logf("directory: unknown key %s: pulling the list (at most once a minute while pulls succeed, once every %s while they fail)", fingerprint, RetryInterval)
	deadline, stop := r.timer(UnknownKeyWait)
	defer stop()
	f := r.ask(askUnknown, prev)
	select {
	case <-f.done:
	case <-deadline:
	case <-ctx.Done():
	}
	_, k, known := r.UserForKey(fingerprint)
	return known && !k.Expired(r.now())
}

// takeUnknownKeySlot takes the unknown-key slot, and reports whether it did,
// and when it had been taken before (prev; zero: never). It is taken only
// when the runtime is not stopping, the last read of the enrolment found the
// server enrolled with the configured service, no Retry-After is pending,
// and the slot was last taken at least UnknownKeyEvery ago while the last
// pull succeeded (or none has ended yet), or at least RetryInterval ago while
// the server is in the failed state. Every check and the taking are done
// under one lock, so of callers at once one at most gets it. The waits are
// measured as elapsed time (RuntimeOptions.Elapsed), not on the wall clock.
func (r *Runtime) takeUnknownKeySlot() (prev time.Time, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ctx.Err() != nil || r.enrol.state != "" || r.enrol.clientID == "" {
		return time.Time{}, false
	}
	now := r.elapsed()
	if now.Before(r.retryEnd) {
		return time.Time{}, false
	}
	every := UnknownKeyEvery
	if r.lastErr != "" {
		every = RetryInterval
	}
	if !r.unknownAt.IsZero() && now.Sub(r.unknownAt) < every {
		return time.Time{}, false
	}
	prev, r.unknownAt = r.unknownAt, now
	return prev, true
}

// unknownMayStart reports whether a pull that exists only for an unknown key
// (made when the slot was taken, prev the take before that one) may start
// now, and if not, why: not while a Retry-After is pending, and not while
// the last pull failed and the slot had been taken less than RetryInterval
// before (the failed state's rate, counted as when the slot is taken). It
// is the slot's own policy, read again at the start: the pull it was made
// for may have queued behind one that ended since.
func (r *Runtime) unknownMayStart(prev time.Time) (bool, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.elapsed()
	if now.Before(r.retryEnd) {
		return false, fmt.Sprintf("the account service asked for no request for another %s (Retry-After)", r.retryEnd.Sub(now).Round(time.Second))
	}
	if r.lastErr != "" && !prev.IsZero() && now.Sub(prev) < RetryInterval {
		return false, fmt.Sprintf("the last pull failed, and while pulls fail an unknown key pulls at most once every %s", RetryInterval)
	}
	return true, ""
}

// PullsAsked is how many pulls have been asked for since the runtime was
// made: each pull counted once, when it is made (started at once, or queued
// behind the one running); a caller that joins a pull already asked for
// adds nothing, and nothing is made once the runtime is stopping. An unknown
// key refused at once asks for none.
func (r *Runtime) PullsAsked() uint64 {
	r.fmu.Lock()
	defer r.fmu.Unlock()
	return r.asked
}

// request is the pull an admin's refresh waits for (ask).
func (r *Runtime) request() *flight { return r.ask(askAdmin, time.Time{}) }

// ask is the pull a caller waits for: the one asked for and not yet
// started, if there is one; otherwise a new one, started now when none is
// running, or else started when the running one ends. by says who asks;
// for an unknown key that took the slot, prev is the take before its own: a
// pull it makes exists only for it until another caller joins it.
//
// Once the runtime is stopping (its context ended), it starts nothing: the
// pull it returns has already ended, as "the server is stopping".
func (r *Runtime) ask(by asker, prev time.Time) *flight {
	r.fmu.Lock()
	defer r.fmu.Unlock()
	if r.stopped || r.ctx.Err() != nil {
		f := &flight{done: make(chan struct{}), row: r.row(false, "the server is stopping")}
		close(f.done)
		return f
	}
	if f := r.pending; f != nil {
		if by != askUnknown {
			f.unknownOnly = false
		}
		if by == askAdmin {
			f.admin = true
		}
		return f
	}
	r.asked++
	f := &flight{done: make(chan struct{}), unknownOnly: by == askUnknown, prevUnknown: prev, admin: by == askAdmin}
	if r.running == nil {
		r.running = f
		r.wg.Add(1)
		go r.run(f)
	} else {
		r.pending = f
	}
	return f
}

// run runs f, then each pull asked for while it ran, one at a time. When
// the last ends with the runtime stopping, it forgets the client. A pull
// that exists only for an unknown key is sent only when the unknown-key
// policy still allows it (unknownMayStart), and one no admin asked for only
// when no Retry-After is pending (scheduleMayStart); otherwise it ends at
// once, having sent nothing.
func (r *Runtime) run(f *flight) {
	defer r.wg.Done()
	// f is the running pull: nobody joins it, so what it was asked for is
	// settled.
	unknownOnly, admin, prev := f.unknownOnly, f.admin, f.prevUnknown
	for f != nil {
		f.row = r.startPull(unknownOnly, admin, prev)
		close(f.done)
		if r.afterDone != nil {
			r.afterDone()
		}
		r.fmu.Lock()
		f, r.pending = r.pending, nil
		r.running = f
		if f != nil {
			unknownOnly, admin, prev = f.unknownOnly, f.admin, f.prevUnknown
		}
		forget := f == nil && r.stopped
		r.fmu.Unlock()
		if forget {
			r.forgetClient()
		}
		select {
		case r.wake <- struct{}{}:
		default:
		}
	}
}

// startPull is the running pull's attempt, unless the pull exists only for
// an unknown key (prev: the slot's take before the one that made it) and the
// unknown-key policy no longer allows it, or no admin asked for it (the
// schedule's) and a Retry-After is pending: then it ends at once, failed,
// having sent nothing.
func (r *Runtime) startPull(unknownOnly, admin bool, prev time.Time) users.DirectoryRefresh {
	switch {
	case admin:
	case unknownOnly:
		if ok, why := r.unknownMayStart(prev); !ok {
			r.logf("directory: the pull asked for by an unknown key is not sent: %s", why)
			return r.row(false, "not sent: "+why)
		}
	default:
		if left := r.retryLeft(); left > 0 {
			why := fmt.Sprintf("the account service asked for no request for another %s (Retry-After)", left.Round(time.Second))
			r.logf("directory: the scheduled pull is not sent: %s", why)
			return r.row(false, "not sent: "+why)
		}
	}
	return r.attempt()
}

// retryLeft is how long the pending Retry-After has still to run, as
// elapsed time (0: none pending).
func (r *Runtime) retryLeft() time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.retryLeftLocked(r.elapsed())
}

// retryLeftLocked is retryLeft at el, a reading of the elapsed clock; r.mu
// is held.
func (r *Runtime) retryLeftLocked(el time.Time) time.Duration {
	if r.retryEnd.IsZero() || !el.Before(r.retryEnd) {
		return 0
	}
	return r.retryEnd.Sub(el)
}

// nextAttempt is when the schedule's next attempt is due, read at now on
// the wall clock: the next attempt recorded, put off (and recorded, so
// status shows it) to the end of the pending Retry-After when that is
// later. The Retry-After's end is measured as elapsed time, so a wall clock
// set forward while it runs does not bring a scheduled pull inside it.
func (r *Runtime) nextAttempt(now time.Time) time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.next = r.nextAttemptLocked(now)
	return r.next
}

// nextAttemptLocked is nextAttempt without recording it; r.mu is held.
func (r *Runtime) nextAttemptLocked(now time.Time) time.Time {
	next := r.next
	if left := r.retryLeftLocked(r.elapsed()); left > 0 {
		if at := now.Add(left); at.After(next) {
			next = at
		}
	}
	return next
}

// schedule starts a pull whenever the next attempt is due, until the
// runtime's context ends: the first at once, then the last good pull plus
// refresh, or after a failure 15 minutes, or the end of the pending
// Retry-After when later (nextAttempt).
func (r *Runtime) schedule() {
	for r.ctx.Err() == nil {
		// At each turn: when the trust time has ended and the kept copy is
		// not yet removed (or its removal failed), remove it.
		r.removeExpiredCopy()
		now := r.now()
		next := r.nextAttempt(now)
		wait := next.Sub(now)
		if wait <= 0 {
			f := r.ask(askSchedule, time.Time{})
			select {
			case <-f.done:
			case <-r.ctx.Done():
				return
			}
			continue
		}
		ch, stop := r.timer(min(wait, maxScheduleWait))
		if r.now().Sub(now).Abs() >= time.Second {
			// The wall clock jumped while the timer was armed (a host that
			// woke, a clock set): its wait was counted from the old reading,
			// so look again.
			stop()
			continue
		}
		if r.beforeWait != nil {
			r.beforeWait(now)
		}
		select {
		case <-r.ctx.Done():
			stop()
			return
		case <-r.wake:
			stop()
		case <-ch:
		}
	}
}

// nextSeq is the next pull sequence number.
func (r *Runtime) nextSeq() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seq++
	return r.seq
}

// attempt is one pull: the enrolment read, the token and the directory
// request, then the copy written and the list published, or the failure
// recorded. It returns the pull's row.
func (r *Runtime) attempt() (row users.DirectoryRefresh) {
	if r.ctx.Err() != nil {
		return r.row(false, "the server is stopping")
	}
	defer func() {
		if r.afterAttempt != nil {
			r.afterAttempt(row)
		}
	}()
	seq := r.nextSeq()
	start := r.now()
	d, e, es := r.openEnrolment()
	if d != nil {
		defer d.Close()
	}
	if es.state != "" {
		return r.notEnrolled(seq, es, start)
	}
	client, err := r.clientFor(e)
	if err != nil {
		clear(e.PrivateKey)
		return r.notEnrolled(seq, enrolState{state: StateEnrolmentUnusable, reason: fmt.Sprintf("%s: %v", d.Join(EnrolmentFileName), err)}, start)
	}
	r.enrolled(es)
	ctx, timedOut, end := r.pullContext()
	p, err := client.Pull(ctx)
	end()
	if err != nil && r.ctx.Err() != nil {
		// Ended by the server stopping: nothing to record.
		return r.row(false, "the server is stopping")
	}
	if err != nil {
		if timedOut() {
			err = r.timeoutFailure(err)
		}
		return r.failed(start, err)
	}
	return r.good(seq, start, d, e, p)
}

// pullContext is the context of one pull's network work: it ends with the
// runtime's, or when PullTimeout has passed on the runtime's timer
// (timedOut then reports true). end releases it; it is called once the
// network work is over.
func (r *Runtime) pullContext() (ctx context.Context, timedOut func() bool, end func()) {
	ctx, cancel := context.WithCancel(r.ctx)
	fire, stop := r.timer(r.pullTimeout)
	var fired atomic.Bool
	over := make(chan struct{})
	go func() {
		select {
		case <-fire:
			fired.Store(true)
			cancel()
		case <-over:
		}
	}()
	return ctx, fired.Load, func() {
		close(over)
		stop()
		cancel()
	}
}

// timeoutFailure is err, a pull's failure when its bound passed: a
// transport failure (the requests ended by the bound) says so, in place of
// the cancelled context's words.
func (r *Runtime) timeoutFailure(err error) error {
	var f *Failure
	if !errors.As(err, &f) || f.Kind != KindTransport {
		return err
	}
	return &Failure{Step: f.Step, Kind: KindTransport, Local: f.Local,
		Err: fmt.Errorf("no answer within the pull's bound of %s", r.pullTimeout)}
}

// openEnrolment opens the state directory and reads the enrolment: d is the
// directory opened (nil when it could not be), e the enrolment when the
// server is enrolled with the configured service (es.state ""). Every read
// goes through the directory opened and checked (StateDir).
func (r *Runtime) openEnrolment() (d *StateDir, e *Enrolment, es enrolState) {
	d, err := OpenStateDir(r.dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, enrolState{state: StateNotEnrolled, reason: "no enrolment: " + r.dir + " does not exist"}
	}
	if err != nil {
		return nil, nil, enrolState{state: StateEnrolmentUnusable, reason: fileError(nil, r.dir, err)}
	}
	path := d.Join(EnrolmentFileName)
	data, err := d.ReadFile(EnrolmentFileName, MaxEnrolmentFileSize)
	if errors.Is(err, fs.ErrNotExist) {
		return d, nil, enrolState{state: StateNotEnrolled, reason: "no enrolment: " + path + " does not exist"}
	}
	if err != nil {
		return d, nil, enrolState{state: StateEnrolmentUnusable, reason: fileError(d, EnrolmentFileName, err)}
	}
	e, err = ParseEnrolmentFile(data)
	clear(data)
	if err != nil {
		return d, nil, enrolState{state: StateEnrolmentUnusable, reason: fmt.Sprintf("%s: %v", path, err)}
	}
	if err := e.CheckConfig(r.issuer, r.organization); err != nil {
		clear(e.PrivateKey)
		return d, nil, enrolState{state: StateNotEnrolled, reason: fmt.Sprintf("%s: %v", path, err)}
	}
	return d, e, enrolState{clientID: e.ClientID, organization: e.AcceptedOrganization(r.organization)}
}

// fileError is err, from reading name in d (or opening the directory name,
// when d is nil), as "<path>: <what>".
func fileError(d *StateDir, name string, err error) string {
	var fc *FileCheckError
	if errors.As(err, &fc) {
		return fc.Error()
	}
	path := name
	if d != nil {
		path = d.Join(name)
	}
	var pe *fs.PathError
	if errors.As(err, &pe) {
		err = pe.Err
	}
	return fmt.Sprintf("%s: %v", path, err)
}

// key is what the once-per-change log of es compares.
func (es enrolState) key() string { return string(es.state) + ": " + es.reason }

// clientFor is the client of enrolment e: the one already made when e is the
// enrolment it was made from (so its access token is reused), otherwise a
// new one. e's key is the new client's, or cleared when it is not needed.
func (r *Runtime) clientFor(e *Enrolment) (*Client, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if old := r.clientOf; old != nil && old.ClientID == e.ClientID && old.Issuer == e.Issuer &&
		old.Organization == e.Organization && subtle.ConstantTimeCompare(old.PrivateKey, e.PrivateKey) == 1 {
		clear(e.PrivateKey)
		return r.client, nil
	}
	c, err := NewClient(ClientConfig{
		Enrolment:      e,
		Organization:   r.organization,
		AllowPlainHTTP: r.allowPlainHTTP,
		RootCAs:        r.rootCAs,
		Now:            r.clock,
	})
	if err != nil {
		return nil, err
	}
	if r.clientOf != nil {
		clear(r.clientOf.PrivateKey)
	}
	r.client, r.clientOf = c, e
	return c, nil
}

// enrolled records that the enrolment read is usable, and logs it when the
// last read was not.
func (r *Runtime) enrolled(es enrolState) {
	r.mu.Lock()
	was := r.enrol.state
	r.enrol = es
	logged := r.enrolLogged != ""
	r.enrolLogged = ""
	r.mu.Unlock()
	if was != "" || logged {
		r.logf("directory: enrolled as %s; pulling the list", es.clientID)
	}
}

// notEnrolled records an attempt that found no usable enrolment: nothing was
// sent. A list the lookup holds is withdrawn (only the local admins can log
// in, and the listed users' SSH connections are closed), and the client and
// its access token are forgotten.
func (r *Runtime) notEnrolled(seq uint64, es enrolState, start time.Time) users.DirectoryRefresh {
	next := r.now().Add(RetryInterval)
	// The withdrawal and the enrolment state it is for change together: a
	// status never shows the list withdrawn under the state before.
	r.mu.Lock()
	withdrawn := r.withdraw(seq)
	r.enrol = es
	r.dropClientLocked()
	r.lastAttempt, r.lastErr, r.next, r.retryEnd = start, es.describe(), next, time.Time{}
	logIt := r.enrolLogged != es.key()
	r.enrolLogged = es.key()
	r.failClass, r.goodLogged = "", ""
	r.mu.Unlock()
	if logIt {
		switch es.state {
		case StateEnrolmentUnusable:
			r.logf("directory: ENROLMENT UNUSABLE (%s); nothing is sent and only the local admins can log in until it is put right; next attempt at %s", es.reason, stamp(next))
		default:
			r.logf("directory: not enrolled (%s): run 'shed-server directory enroll'; nothing is sent and only the local admins can log in; next attempt at %s", es.reason, stamp(next))
		}
	}
	if withdrawn {
		r.logf("directory: the list is withdrawn: only the local admins can log in, and the listed users' SSH connections are closed")
	}
	r.logStale()
	row := r.row(false, es.describe())
	if withdrawn && row.List == users.DirectoryListNone {
		row.List = users.DirectoryListWithdrawn
	}
	return row
}

// describe is es in a status line.
func (es enrolState) describe() string {
	switch es.state {
	case StateEnrolmentUnusable:
		return "enrolment unusable: " + es.reason
	case StateNotEnrolled:
		return "not enrolled: " + es.reason
	}
	return ""
}

// good records a good pull: the copy is written durably, then the list is
// published. When the copy cannot be written the old one is removed, so a
// restart cannot bring back a person the new list removed, and the list is
// published in memory only.
//
// Write, then publish: the rule this order keeps is that the copy on disk is
// never OLDER than the list published, so no restart brings back a person a
// published list had removed. The other direction is allowed: when the
// process dies after the write and before the publication, the copy on disk
// holds a list that was never published. That list is the service's own
// answer to a good pull, fully checked, and loading it at the next start
// (under every start-up check) is correct; the crash window is not a bug.
func (r *Runtime) good(seq uint64, start time.Time, d *StateDir, e *Enrolment, p *Pull) users.DirectoryRefresh {
	r.copyMu.Lock()
	cond, why := CopyKept, ""
	data, err := marshalCopy(e, p)
	if err == nil {
		err = r.writeCopy(d, data)
	}
	if err != nil {
		cond, why = r.dropCopy(d, err, "the copy on disk remains")
	}
	before := r.State()
	prev, _ := r.held()
	// The list is published and the runtime's record of it (its version,
	// when it arrived, the attempt, the copy) set under one hold of r.mu,
	// which Status and row read under: no status pairs the new list with the
	// previous version or pull time. The readers of the lookup take no lock
	// and see the new list at its publication, as before.
	r.mu.Lock()
	ok, err := r.Install(p.List, p.FetchedAt, r.trust, seq)
	if err != nil || !ok {
		r.mu.Unlock()
		if err == nil {
			err = errors.New("a newer list was published first")
		}
		// A copy of a list this server did not publish is removed: the next
		// start must not prefer it to what this server holds.
		if cond == CopyKept {
			cond, why = r.dropCopy(d, fmt.Errorf("the list was not published: %w", err), "the new, unpublished copy remains on disk")
		}
		r.mu.Lock()
		r.copyCond, r.copyErr, r.copyEnd, r.endLogged = cond, why, time.Time{}, ""
		r.mu.Unlock()
		r.copyMu.Unlock()
		return r.failed(start, fmt.Errorf("installing the list: %w", err))
	}
	listed, skipped := r.held()
	pastTrust := !r.pastTrust.IsZero()
	prevCond := r.copyCond
	r.lastGood, r.version = p.FetchedAt.Round(0), p.List.Version
	r.pastTrust = time.Time{}
	r.copyCond, r.copyErr, r.copyEnd, r.endLogged = cond, why, time.Time{}, ""
	r.lastAttempt, r.lastErr, r.retryEnd = start, "", time.Time{}
	r.next = p.FetchedAt.Round(0).Add(r.refresh)
	r.failClass = ""
	logGood := r.goodLogged != p.List.Version
	r.goodLogged = p.List.Version
	next := r.next
	r.mu.Unlock()
	r.copyMu.Unlock()

	if p.HasSkew {
		r.warnSkew(p.Skew)
	}
	if before.Expired || pastTrust {
		r.logf("directory: trust restored: a good pull (version %s) replaces the list whose trust time had ended; its users can log in again", p.List.Version)
	}
	if logGood {
		r.logf("directory: good pull: version %s, %d users (%d skipped)%s; next pull at %s",
			p.List.Version, len(listed), len(skipped), changes(prev, listed), stamp(next))
	}
	r.logSkips(p.List.Version)
	switch {
	case cond == CopyNotWritten && prevCond != CopyNotWritten:
		r.logf("directory: kept copy not written: %s; a restart starts with only the local admins until a good pull", why)
	case cond == CopyKept && (prevCond == CopyNotWritten || prevCond == CopyStale):
		r.logf("directory: kept copy written again")
	}
	r.logStale()
	// A late answer whose trust time had already ended is published as that
	// end: its copy goes too.
	r.removeExpiredCopy()
	return r.row(true, "")
}

// dropCopy removes the copy on disk after a good pull's copy could not be
// kept (cause), and returns the copy's condition and why. which says what
// remains when the removal fails: the copy on disk after a failed write (the
// old copy, or the new one when the write got as far as its rename), the
// new, unpublished copy after its list was not published.
func (r *Runtime) dropCopy(d *StateDir, cause error, which string) (CopyCondition, string) {
	if err := r.removeCopy(d); err != nil {
		return CopyStale, fmt.Sprintf("%v; %s: removing it failed: %v", cause, which, err)
	}
	return CopyNotWritten, cause.Error()
}

// logStale logs, at every pull, that the copy on disk is stale after a
// pull's write or publication failed. (A copy left behind at the end of the
// trust time is logged once per change, by removeExpiredCopy.)
func (r *Runtime) logStale() {
	r.mu.Lock()
	cond, why, end := r.copyCond, r.copyErr, r.copyEnd
	r.mu.Unlock()
	if cond == CopyStale && end.IsZero() {
		r.logf("directory: WARNING: the copy on disk is stale (%s): a restart would bring back its list for what remains of its trust time", why)
	}
}

// trustEnd is the end of the trust time that has passed for the list the
// server holds, or for the kept copy refused at start for its trust time;
// ok false when neither has ended.
func (r *Runtime) trustEnd() (end time.Time, ok bool) {
	st := r.State()
	if st.Listed && st.Expired {
		return st.Deadline, true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !st.Listed && !r.pastTrust.IsZero() {
		return r.pastTrust, true
	}
	return time.Time{}, false
}

// removeExpiredCopy makes the end of the trust time durable: once the trust
// time of the list the server holds has ended (or that of the copy found at
// start), the kept copy is removed (unlinked, the directory synced), so a
// restart, whatever its clock then says, starts with only the local admins
// until a good pull. It runs outside the publication hook and outside every
// lock the readers take, after each check of the trust watch and at each
// turn of the schedule; a removal that failed is tried again then. The last
// good pull's time and version stay known (Status) until the server stops.
func (r *Runtime) removeExpiredCopy() {
	r.copyMu.Lock()
	defer r.copyMu.Unlock()
	end, ok := r.trustEnd()
	if !ok {
		return
	}
	r.mu.Lock()
	done := r.copyEnd.Equal(end) && r.copyCond != CopyStale
	r.mu.Unlock()
	if done {
		return
	}
	d, err := OpenStateDir(r.dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		err = nil // no state directory: no copy
	case err == nil:
		r.removeEndedCopy(d)
		d.Close()
		return
	}
	r.endedCopy(end, err)
}

// removeEndedCopy removes the kept copy in d when the trust time of its list
// has ended (trustEnd), and records the outcome; otherwise it does nothing.
func (r *Runtime) removeEndedCopy(d *StateDir) {
	end, ok := r.trustEnd()
	if !ok {
		return
	}
	r.endedCopy(end, r.removeCopy(d))
}

// endedCopy records the outcome of removing the copy of a list whose trust
// time ended at end (err nil: removed), and logs it once per change.
func (r *Runtime) endedCopy(end time.Time, err error) {
	var cond CopyCondition
	var why, line string
	if err == nil {
		cond = CopyNone
		why = "removed: the trust time of its list ended at " + stamp(end)
		line = fmt.Sprintf("directory: kept copy removed: the trust time of its list ended at %s; a restart starts with only the local admins until a good pull", stamp(end))
	} else {
		cond = CopyStale
		why = fmt.Sprintf("the trust time of its list ended at %s, and removing it failed: %v", stamp(end), err)
		line = fmt.Sprintf("directory: WARNING: the copy on disk is stale (%s): a restart while this server's clock reads before %s would bring back its list; the removal is tried again at each turn of the schedule", why, stamp(end))
	}
	r.mu.Lock()
	r.copyCond, r.copyErr, r.copyEnd = cond, why, end
	logIt := r.endLogged != why
	r.endLogged = why
	r.mu.Unlock()
	if logIt {
		r.logf("%s", line)
	}
}

// failed records a failed pull: the list and the copy stay as they were. Its
// Retry-After (at most MaxRetryAfter) is kept, as elapsed time, as the
// runtime's one outstanding Retry-After (retryEnd): a later failure never
// ends a pending one sooner, with no Retry-After or a shorter one; the later
// end is kept. The next attempt is 15 minutes away, or the end of that
// outstanding Retry-After when later, whatever this failure's own answer
// carried: an admin's refresh that fails inside it does not bring the
// schedule closer.
func (r *Runtime) failed(start time.Time, err error) users.DirectoryRefresh {
	now := r.now()
	var f *Failure
	errors.As(err, &f)
	var retryFor time.Duration
	if f != nil && f.HasRetryAfter {
		retryFor = min(f.RetryAfter, MaxRetryAfter)
	}
	r.mu.Lock()
	el := r.elapsed()
	if retryFor > 0 {
		if end := el.Add(retryFor); end.After(r.retryEnd) {
			r.retryEnd = end
		}
	}
	next := now.Add(max(RetryInterval, r.retryLeftLocked(el)))
	class, line, everyAttempt := describeFailure(f, err, next)
	r.lastAttempt, r.lastErr, r.next = start, err.Error(), next
	logIt := everyAttempt || r.failClass != class
	r.failClass, r.goodLogged = class, ""
	r.mu.Unlock()
	if logIt {
		r.logf("%s", line)
	}
	if f != nil && !f.Date.IsZero() && !f.Local.IsZero() {
		r.warnSkew(f.Date.Sub(f.Local))
	}
	r.logStale()
	return r.row(false, err.Error())
}

// warnSkew warns when an answer's Date was further than skewWarning from the
// server's clock.
func (r *Runtime) warnSkew(skew time.Duration) {
	if skew.Abs() > skewWarning {
		r.logf("directory: WARNING: the account service's clock is %s from this server's; the trust time is measured on this server's clock, which must be accurate", skew)
	}
}

// describeFailure is the log line of a failed pull, its class (for the
// lines logged once per change of state) and whether it is logged at every
// attempt. f is err as a *Failure, or nil.
func describeFailure(f *Failure, err error, next time.Time) (class, line string, everyAttempt bool) {
	at := "next attempt at " + stamp(next)
	if f == nil {
		return "other", fmt.Sprintf("directory: pull failed: %v; the list held stays; %s", err, at), false
	}
	switch {
	case f.Kind == KindInvalidClient:
		return "token-refused", fmt.Sprintf("directory: the account service refused this server's login: %v; the list held stays; %s", f, at), true
	case f.Kind == KindInvalidToken:
		return "token-refused", fmt.Sprintf("directory: the account service refused this server's access token after a new login: %v (the service's clock %s, this server's %s); the list held stays; %s",
			f, formatDate(f.Date), stamp(f.Local), at), true
	case f.Kind == KindServerDisabled:
		return "server-disabled", fmt.Sprintf("directory: SERVER DISABLED: the account service has disabled this server: %v; the list held stays until its trust time ends; %s", f, at), false
	case f.Step == StepDirectory && f.Status == 403 && f.Code == "":
		// A SERVER_DISABLED answer whose body was cut at the 4 KiB an error
		// body is read to arrives with no code: it is reported as possibly
		// that.
		return "server-disabled", fmt.Sprintf("directory: REFUSED: the account service answered HTTP 403 with no error this server could read (a disabled server's answer, cut short, looks like this): %v; the list held stays until its trust time ends; %s", f, at), false
	case f.Kind == KindRateLimited:
		return "rate-limited", fmt.Sprintf("directory: rate limited by the account service: %v; the list held stays; %s", f, at), false
	case f.Kind == KindUnavailable || f.Kind == KindTransport || f.Kind == KindServerError:
		return "unavailable", fmt.Sprintf("directory: the account service is unavailable or did not answer in time: %v; the list held stays; %s", f, at), true
	case errors.Is(err, ErrContract) || errors.Is(err, ErrOrganization):
		return "contract", fmt.Sprintf("directory: the account service's answer is refused: %v; the list held stays; %s", f, at), false
	}
	return "refused:" + string(f.Kind), fmt.Sprintf("directory: pull failed: %v; the list held stays; %s", f, at), false
}

// row is the Directory row of an attempt that was good or failed with
// errText: the version and counts are those of the list in force after it,
// read from one snapshot (view) with the version recorded for it (under
// r.mu, as good sets both). A list whose trust time has ended is not in
// force: the row then has no version and no users, and says why (List).
func (r *Runtime) row(good bool, errText string) users.DirectoryRefresh {
	r.mu.Lock()
	v := r.view()
	version, pastTrust := r.version, !r.pastTrust.IsZero()
	r.mu.Unlock()
	row := users.DirectoryRefresh{Status: users.DirectoryRefreshFailed, Error: errText}
	if good {
		row.Status = users.DirectoryRefreshGood
	}
	switch {
	case v.Listed && !v.Expired:
		row.List, row.Version, row.Users, row.Skipped = users.DirectoryListInForce, version, v.Admitted, len(v.Skipped)
	case v.Listed, pastTrust:
		// The list held, or the kept copy found at start, is past its trust
		// time.
		row.List = users.DirectoryListTrustEnded
	default:
		row.List = users.DirectoryListNone
	}
	return row
}

// logSkips logs the entries of the list held that the server does not
// answer for: every one once per version, and those with a local admin's
// name at every pull.
func (r *Runtime) logSkips(version string) {
	_, skipped := r.held()
	r.mu.Lock()
	all := r.skipsLogged != version
	r.skipsLogged = version
	r.mu.Unlock()
	for _, s := range skipped {
		if all || s.Reason == SkipLocalAdminName {
			r.logf("directory: list entry %q skipped: %s", s.Name, skipWords(s.Reason))
		}
	}
}

// skipWords is why an entry was skipped, in words.
func skipWords(reason SkipReason) string {
	switch reason {
	case SkipLocalAdminName:
		return "its name is a local admin's (the local admin is a separate, local-only identity)"
	case SkipLocalAdminKey:
		return "it lists a key a local admin holds"
	case SkipServiceClientName:
		return "its name is an auth.clients name"
	case SkipDuplicateMember:
		return "a member is named twice in it"
	case SkipMalformed:
		return "it is not a user as the contract describes one"
	case SkipInvalidName:
		return "its name is not a valid user name"
	case SkipDuplicateName:
		return "its name is listed twice"
	case SkipDuplicateKey:
		return "it lists a key listed twice"
	case SkipNoUsableKey:
		return "it has no key this server can use"
	}
	return string(reason)
}

// changes is a good pull's changes from prev to cur, the admitted users of
// the lists before and after: added and removed names, and admin flags that
// changed. Names are quoted.
func changes(prev, cur map[string]users.User) string {
	var added, removed, promoted, demoted []string
	for name, u := range cur {
		old, ok := prev[name]
		switch {
		case !ok:
			added = append(added, name)
		case u.Admin && !old.Admin:
			promoted = append(promoted, name)
		case !u.Admin && old.Admin:
			demoted = append(demoted, name)
		}
	}
	for name := range prev {
		if _, ok := cur[name]; !ok {
			removed = append(removed, name)
		}
	}
	var b strings.Builder
	for _, part := range []struct {
		what  string
		names []string
	}{{"added", added}, {"removed", removed}, {"now admin", promoted}, {"no longer admin", demoted}} {
		if len(part.names) == 0 {
			continue
		}
		sort.Strings(part.names)
		quoted := make([]string, len(part.names))
		for i, n := range part.names {
			quoted[i] = fmt.Sprintf("%q", n)
		}
		fmt.Fprintf(&b, "; %s %s", part.what, strings.Join(quoted, ", "))
	}
	return b.String()
}

// stamp is t for a log or status line.
func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// Status is the state of a directory server's user source, as an admin is
// shown it. It holds no key.
type Status struct {
	// Issuer is the configured issuer; Organization the accepted one (the
	// configured, or with none configured the enrolment's; "" when not
	// known); ClientID the enrolment's ("" when not enrolled).
	Issuer, Organization, ClientID string
	// State is the source's state, and Reason why it is not trusted, when it
	// is not.
	State  SourceState
	Reason string
	// LastGoodPull is when the answer of the last list held arrived (zero
	// for none), and Version its version.
	LastGoodPull time.Time
	Version      string
	// TrustedUntil is when the list held is (or was) trusted until; zero
	// with no list.
	TrustedUntil time.Time
	// LastAttempt is when the last pull started, and LastError why it
	// failed ("" when it was good).
	LastAttempt time.Time
	LastError   string
	// NextAttempt is when the next scheduled pull is due (zero: at once):
	// never inside the pending Retry-After, whatever the wall clock was set
	// to while it runs.
	NextAttempt time.Time
	// Copy is the kept copy's condition, and CopyError why it is not kept.
	Copy      CopyCondition
	CopyError string
	// Users are the users the server answers for now, sorted by name.
	Users []StatusUser
	// Skipped are the entries of the list held that the server does not
	// answer for.
	Skipped []Skip
}

// Status reports the source's state now, coherently: the lookup's part (the
// state, whether the list is trusted, its deadline, the users and the skip
// report) from one snapshot, loaded once, with one check of its trust time;
// the runtime's own fields read together under r.mu, the lock a good pull
// (good) and a withdrawal (notEnrolled) hold while they publish and record,
// so the snapshot is never paired with the version, pull time or enrolment
// of another.
func (r *Runtime) Status() Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	v := r.view()
	ls := v.State
	now := r.now()
	st := Status{
		Issuer:       r.issuer,
		Organization: r.organization,
		ClientID:     r.enrol.clientID,
		LastGoodPull: r.lastGood,
		Version:      r.version,
		TrustedUntil: ls.Deadline,
		LastAttempt:  r.lastAttempt,
		LastError:    r.lastErr,
		NextAttempt:  r.nextAttemptLocked(now),
		Copy:         r.copyCond,
		CopyError:    r.copyErr,
		Users:        v.Users,
		Skipped:      ls.Skipped,
	}
	if r.enrol.organization != "" {
		st.Organization = r.enrol.organization
	}
	switch {
	case r.enrol.state != "":
		st.State, st.Reason = r.enrol.state, r.enrol.reason
	case ls.Listed && !ls.Expired:
		st.State = StateTrusted
	case ls.Listed:
		st.State, st.Reason = StateExpired, "the list's trust time ended at "+stamp(ls.Deadline)
	case !r.pastTrust.IsZero():
		st.State, st.Reason = StateExpired, "the kept copy's trust time ended at "+stamp(r.pastTrust)
		st.TrustedUntil = r.pastTrust
	default:
		st.State, st.Reason = StateNoCopy, "no list yet: no usable kept copy and no good pull"
	}
	return st
}

// StartupLines are the start-up lines of the source (plan section 3.10):
// where the users come from, then the state Load found.
func (r *Runtime) StartupLines() []string {
	st := r.Status()
	org, id := st.Organization, st.ClientID
	if org == "" {
		org = "not known until enrolment"
	}
	if id == "" {
		id = "none"
	}
	lines := []string{fmt.Sprintf("Users: from the directory at %s (organization %s, client id %s)", st.Issuer, org, id)}
	switch st.State {
	case StateTrusted:
		listed := 0
		for _, u := range st.Users {
			if u.Source == UserDirectory {
				listed++
			}
		}
		lines = append(lines, fmt.Sprintf("Users: kept copy from %s, version %s, %d users (%d skipped), trusted until %s",
			stamp(st.LastGoodPull), st.Version, listed, len(st.Skipped), stamp(st.TrustedUntil)))
	case StateExpired:
		lines = append(lines, fmt.Sprintf("Users: kept copy is past its trust time (%s); only the local admins can log in until a good pull", stamp(st.TrustedUntil)))
	case StateNotEnrolled:
		lines = append(lines, fmt.Sprintf("Users: not enrolled (%s): run 'shed-server directory enroll'; only the local admins can log in", st.Reason))
	case StateEnrolmentUnusable:
		lines = append(lines, fmt.Sprintf("Users: enrolment unusable (%s); only the local admins can log in", st.Reason))
	default:
		lines = append(lines, "Users: no kept copy; only the local admins can log in until the first good pull")
	}
	return lines
}
