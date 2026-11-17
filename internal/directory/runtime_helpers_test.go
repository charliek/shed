package directory

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/directory/refserver"
	"github.com/charliek/shed/internal/users"
)

// The runtime's tests run it against the reference server of the contract,
// in-process over loopback plain HTTP, on a fake clock that the runtime, its
// lookup and the reference server share: nothing waits for real time to
// pass.

// fakeClock is a wall clock a test moves, timers on it, and the elapsed time
// the runtime's waits are measured on (Elapsed). set and advance move the
// wall clock and, when it moves forward, elapsed time by as much (as a host
// whose clock is not stepped); stepWall moves the wall clock alone (a clock
// set) and elapse elapsed time alone. Timers follow set and advance only.
type fakeClock struct {
	mu     sync.Mutex
	t      time.Time
	mono   time.Time
	timers []*fakeTimer
}

type fakeTimer struct {
	at      time.Time
	ch      chan time.Time
	stopped bool
}

// fakeElapsedBase is where the fake elapsed clock starts: far from any wall
// time a test uses, so a wait measured by mixing the two clocks shows.
var fakeElapsedBase = time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)

func newFakeClock(t0 time.Time) *fakeClock { return &fakeClock{t: t0, mono: fakeElapsedBase} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

// Elapsed is the elapsed-time reading (RuntimeOptions.Elapsed): only the
// difference of two readings means anything.
func (c *fakeClock) Elapsed() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.mono
}

// stepWall moves the wall clock by d, forward or back, and nothing else: no
// time elapses and no timer fires.
func (c *fakeClock) stepWall(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// elapse lets d pass on the elapsed clock alone: the wall clock stays where
// it is and no timer fires.
func (c *fakeClock) elapse(d time.Duration) {
	c.mu.Lock()
	c.mono = c.mono.Add(d)
	c.mu.Unlock()
}

// Timer is a timer of d on the clock: it fires when the clock reaches now+d
// (at once when d is not positive).
func (c *fakeClock) Timer(d time.Duration) (<-chan time.Time, func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	tm := &fakeTimer{at: c.t.Add(d), ch: make(chan time.Time, 1)}
	if d <= 0 {
		tm.ch <- c.t
	} else {
		c.timers = append(c.timers, tm)
	}
	return tm.ch, func() {
		c.mu.Lock()
		tm.stopped = true
		c.mu.Unlock()
	}
}

// set moves the clock to t and fires every timer due by then.
func (c *fakeClock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if t.After(c.t) {
		c.mono = c.mono.Add(t.Sub(c.t))
	}
	c.t = t
	kept := c.timers[:0]
	for _, tm := range c.timers {
		switch {
		case tm.stopped:
		case !t.Before(tm.at):
			tm.ch <- t
		default:
			kept = append(kept, tm)
		}
	}
	c.timers = kept
}

func (c *fakeClock) advance(d time.Duration) { c.set(c.Now().Add(d)) }

// memDirectory is the reference server's inputs, in memory: the list, the
// codes, the disabled servers and an outage switch.
type memDirectory struct {
	mu       sync.Mutex
	users    []refserver.User
	codes    map[string]string // code -> server name
	disabled map[string]bool
	outage   bool
}

func (m *memDirectory) Users() ([]refserver.User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.outage {
		return nil, refserver.ErrOutage
	}
	return append([]refserver.User(nil), m.users...), nil
}

func (m *memDirectory) Code(code string) (refserver.Code, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.outage {
		return refserver.Code{}, false, refserver.ErrOutage
	}
	name, ok := m.codes[code]
	return refserver.Code{ServerName: name}, ok, nil
}

func (m *memDirectory) Disabled(id string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.outage {
		return false, refserver.ErrOutage
	}
	return m.disabled[id], nil
}

func (m *memDirectory) setUsers(us ...refserver.User) {
	m.mu.Lock()
	m.users = us
	m.mu.Unlock()
}

func (m *memDirectory) setOutage(on bool) {
	m.mu.Lock()
	m.outage = on
	m.mu.Unlock()
}

func (m *memDirectory) setDisabled(id string, on bool) {
	m.mu.Lock()
	m.disabled[id] = on
	m.mu.Unlock()
}

// person is a list user with one fresh key, labelled "laptop", expiring at
// expires when not zero.
type person struct {
	name, line, fp string
	admin          bool
	expires        time.Time
}

func newPerson(t *testing.T, name string, admin bool) *person {
	line, fp := newTestKey(t)
	return &person{name: name, line: line, fp: fp, admin: admin}
}

func (p *person) served() refserver.User {
	k := refserver.Key{Key: p.line, Label: "laptop"}
	if !p.expires.IsZero() {
		k.Expires = p.expires.UTC().Format(time.RFC3339)
	}
	return refserver.User{Name: p.name, Admin: p.admin, Keys: []refserver.Key{k}}
}

// faultMode is what the service in front of the reference server does to
// directory requests.
type faultMode string

const (
	faultNone faultMode = ""
	// fault401: every directory request is answered 401 invalid_token.
	fault401 faultMode = "401"
	// faultCut403: every directory request is answered 403 with a
	// SERVER_DISABLED body longer than the 4 KiB a client reads of one.
	faultCut403 faultMode = "403-cut"
	// faultHang: directory requests get no answer until the client gives up.
	faultHang faultMode = "hang"
	// faultRetryAfter: every directory request is answered 503 with the
	// Retry-After in retryAfter.
	faultRetryAfter faultMode = "503-retry-after"
	// fault429: every directory request is answered 429 with the
	// Retry-After in retryAfter.
	fault429 faultMode = "429"
	// faultContract2: every directory request is answered 200 with a body
	// naming contract 2.
	faultContract2 faultMode = "contract-2"
	// faultInvalidClient: every token request is answered 401
	// invalid_client, with a Date an hour ahead of the clock.
	faultInvalidClient faultMode = "invalid-client"
	// faultReflectAssertion: every token request is answered 401
	// invalid_client whose error_description quotes the assertion it
	// received.
	faultReflectAssertion faultMode = "reflect-assertion"
	// faultReflectBearer: every directory request is answered 400 whose
	// error message quotes the bearer token it received.
	faultReflectBearer faultMode = "reflect-bearer"
	// faultRedirectBearer: every directory request is answered 302 to a
	// Location carrying the bearer token it received.
	faultRedirectBearer faultMode = "redirect-bearer"
)

// rtService is the reference server behind a recorder that can also answer
// in its place (faultMode), hold the directory's answers (gate), and keep
// every access token issued and every assertion sent.
type rtService struct {
	t      *testing.T
	clock  *fakeClock
	dir    *memDirectory
	issuer string

	// arrived is told of each directory request the reference server has
	// answered (before the gate holds the answer).
	arrived chan struct{}
	// hung is told of each directory request faultHang holds.
	hung chan struct{}

	mu         sync.Mutex
	mode       faultMode
	retryAfter string
	// gate, when set, holds every directory answer (made by the reference
	// server when the request arrived) until it is closed.
	gate chan struct{}
	// gateMode and gateRetryAfter, when gateMode is set, replace each answer
	// the gate held (setGateAnswer).
	gateMode       faultMode
	gateRetryAfter string
	directory      int
	tokenReqs      int
	tokens         []string
	assertions     []string
	// inflight is how many directory requests are being answered now, and
	// maxInflight the most there have been at once.
	inflight, maxInflight int
	// conns are the service's connections not yet closed, in their state,
	// and connChanged when one last changed state (real time).
	conns       map[net.Conn]http.ConnState
	connChanged time.Time
}

// connState tracks the service's connections (http.Server.ConnState).
func (s *rtService) connState(c net.Conn, st http.ConnState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st == http.StateClosed || st == http.StateHijacked {
		delete(s.conns, c)
	} else {
		s.conns[c] = st
	}
	s.connChanged = time.Now()
}

// quiet waits until no connection of the service is new (accepted, its
// request not yet read) or active (a request being read or answered), and
// none has changed state for settle: every request a client wrote before the
// call has then arrived and been counted. It fails the test after a while.
func (s *rtService) quiet(t *testing.T, settle time.Duration) {
	t.Helper()
	end := time.Now().Add(10 * time.Second)
	for time.Now().Before(end) {
		s.mu.Lock()
		busy := time.Since(s.connChanged) < settle
		for _, st := range s.conns {
			if st == http.StateNew || st == http.StateActive {
				busy = true
			}
		}
		s.mu.Unlock()
		if !busy {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the service's connections did not go quiet")
}

func newRTService(t *testing.T, clock *fakeClock) *rtService {
	t.Helper()
	s := &rtService{t: t, clock: clock, arrived: make(chan struct{}, 1024), hung: make(chan struct{}, 1024),
		dir:   &memDirectory{codes: map[string]string{}, disabled: map[string]bool{}},
		conns: map[net.Conn]http.ConnState{}}
	ts := httptest.NewUnstartedServer(nil)
	ts.Config.ConnState = s.connState
	s.issuer = "http://" + ts.Listener.Addr().String()
	ref, err := refserver.New(refserver.Config{Issuer: s.issuer, Organization: worldOrg, Inputs: s.dir, Now: clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	ts.Config.Handler = s.wrap(ref)
	ts.Start()
	t.Cleanup(ts.Close)
	return s
}

func (s *rtService) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth2/token":
			// Counted at arrival, before any work: a request the client sent,
			// and then gave up on, is counted when it arrives, not after its
			// answer has been made (which may be after the client stopped).
			s.mu.Lock()
			s.tokenReqs++
			s.mu.Unlock()
			body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
			r.Body = io.NopCloser(bytes.NewReader(body))
			form, _ := url.ParseQuery(string(body))
			s.mu.Lock()
			mode := s.mode
			s.mu.Unlock()
			rec := httptest.NewRecorder()
			switch mode {
			case faultInvalidClient, faultReflectAssertion:
				desc := "the client is not known"
				if mode == faultReflectAssertion {
					desc = "refused the assertion " + form.Get("client_assertion") + " as sent"
				}
				out, _ := json.Marshal(map[string]string{"error": "invalid_client", "error_description": desc})
				rec.Header().Set("Content-Type", "application/json")
				rec.Header().Set("Date", s.clock.Now().Add(time.Hour).UTC().Format(http.TimeFormat))
				rec.WriteHeader(http.StatusUnauthorized)
				_, _ = rec.Write(out)
			default:
				next.ServeHTTP(rec, r)
			}
			s.mu.Lock()
			if a := form.Get("client_assertion"); a != "" {
				s.assertions = append(s.assertions, a)
			}
			if tok := tokenOf(rec.Body.Bytes()); tok != "" {
				s.tokens = append(s.tokens, tok)
			}
			s.mu.Unlock()
			replay(w, rec)
		case "/v1/directory":
			s.mu.Lock()
			s.directory++
			s.inflight++
			s.maxInflight = max(s.maxInflight, s.inflight)
			mode, retryAfter, gate := s.mode, s.retryAfter, s.gate
			s.mu.Unlock()
			defer func() {
				s.mu.Lock()
				s.inflight--
				s.mu.Unlock()
			}()
			bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			switch mode {
			case faultReflectBearer:
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				out, _ := json.Marshal(map[string]any{"error": map[string]string{"code": "BAD_REQUEST " + bearer, "message": "the token " + bearer + " is malformed"}})
				_, _ = w.Write(out)
				return
			case faultRedirectBearer:
				w.Header().Set("Location", "https://elsewhere.example.test/?t="+bearer)
				w.WriteHeader(http.StatusFound)
				return
			case fault401:
				w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = io.WriteString(w, `{"error":{"code":"invalid_token","message":"the token is not valid"}}`)
				return
			case faultCut403:
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusForbidden)
				_, _ = io.WriteString(w, `{"error":{"code":"SERVER_DISABLED","message":"`+strings.Repeat("x", 5000)+`"}}`)
				return
			case faultHang:
				select {
				case s.hung <- struct{}{}:
				default:
				}
				<-r.Context().Done()
				return
			case faultRetryAfter, fault429:
				writeTryLater(w, mode, retryAfter)
				return
			case faultContract2:
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"contract":2,"organization":"example","version":"`+strings.Repeat("a", 64)+`","users":[]}`)
				return
			}
			rec := httptest.NewRecorder()
			next.ServeHTTP(rec, r)
			select {
			case s.arrived <- struct{}{}:
			default:
			}
			if gate != nil {
				<-gate
				s.mu.Lock()
				gm, gra := s.gateMode, s.gateRetryAfter
				s.mu.Unlock()
				if gm != faultNone {
					writeTryLater(w, gm, gra)
					return
				}
			}
			replay(w, rec)
		default:
			next.ServeHTTP(w, r)
		}
	})
}

// writeTryLater answers 503 (faultRetryAfter) or 429 (fault429), with
// retryAfter as the Retry-After, or with none when it is "".
func writeTryLater(w http.ResponseWriter, mode faultMode, retryAfter string) {
	status := http.StatusServiceUnavailable
	if mode == fault429 {
		status = http.StatusTooManyRequests
	}
	if retryAfter != "" {
		w.Header().Set("Retry-After", retryAfter)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, `{"error":{"code":"UNAVAILABLE","message":"try later"}}`)
}

// setGateAnswer makes every directory answer the gate holds, once the gate
// opens, a 503 (faultRetryAfter) or 429 (fault429) with retryAfter ("": no
// header) in place of the reference server's; faultNone undoes it.
func (s *rtService) setGateAnswer(m faultMode, retryAfter string) {
	s.mu.Lock()
	s.gateMode, s.gateRetryAfter = m, retryAfter
	s.mu.Unlock()
}

func replay(w http.ResponseWriter, rec *httptest.ResponseRecorder) {
	for k, v := range rec.Header() {
		w.Header()[k] = v
	}
	w.WriteHeader(rec.Code)
	_, _ = w.Write(rec.Body.Bytes())
}

// tokenOf is the access_token of a token answer, if any.
func tokenOf(body []byte) string {
	obj, err := parseDocument(body)
	if err != nil {
		return ""
	}
	v, _ := obj.one("access_token")
	s, _ := rawString(v)
	return s
}

func (s *rtService) setMode(m faultMode, retryAfter string) {
	s.mu.Lock()
	s.mode, s.retryAfter = m, retryAfter
	s.mu.Unlock()
}

func (s *rtService) setGate(g chan struct{}) {
	s.mu.Lock()
	s.gate = g
	s.mu.Unlock()
}

func (s *rtService) directoryCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.directory
}

func (s *rtService) tokenCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tokenReqs
}

// mostAtOnce is the most directory requests the service has answered at
// once.
func (s *rtService) mostAtOnce() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.maxInflight
}

// secrets are every access token issued and every assertion sent so far.
func (s *rtService) secrets() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append(append([]string(nil), s.tokens...), s.assertions...)
}

// enrol enrols a server with the service and writes its enrolment.json in
// stateDir (made 0700), as `shed-server directory enroll` does. It returns
// the enrolment.
func (s *rtService) enrol(stateDir string) *Enrolment {
	s.t.Helper()
	code := fmt.Sprintf("code-for-%d", s.clock.Now().UnixNano())
	s.dir.mu.Lock()
	s.dir.codes[code] = "server-" + fmt.Sprint(len(s.dir.codes))
	s.dir.mu.Unlock()
	e, err := Enroll(context.Background(), EnrolRequest{
		Issuer: s.issuer, Code: code, Name: "srv.example.test", AllowPlainHTTP: true, Now: s.clock.Now,
	})
	if err != nil {
		s.t.Fatalf("enrol: %v", err)
	}
	d, err := MakeStateDir(stateDir)
	if err != nil {
		s.t.Fatal(err)
	}
	defer d.Close()
	data, err := e.MarshalFile()
	if err != nil {
		s.t.Fatal(err)
	}
	if _, err := d.CreateOnce(EnrolmentFileName, data); err != nil {
		s.t.Fatal(err)
	}
	return e
}

// rtLog is a runtime's log, safe to write and read at once.
type rtLog struct {
	mu    sync.Mutex
	lines []string
}

func (l *rtLog) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *rtLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

// count is how many lines hold sub.
func (l *rtLog) count(sub string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, line := range l.lines {
		if strings.Contains(line, sub) {
			n++
		}
	}
	return n
}

// rtServer is one run of a directory server's user source: its lookup and
// runtime over a state directory, on the shared clock. A restart is another
// rtServer over the same state directory.
type rtServer struct {
	rt     *Runtime
	log    *rtLog
	ticks  chan time.Time
	opsFP  string
	cancel context.CancelFunc
}

// rtTimes are the tests' refresh and trust.
const (
	rtRefresh = time.Hour
	rtTrust   = 4 * time.Hour
)

// startRT builds a server's lookup (local admin ops, with opsLine) and
// runtime over stateDir against svc, and runs Load. adjust, when set, edits
// the runtime before Load.
func startRT(t *testing.T, svc *rtService, stateDir, opsLine string, adjust func(*Runtime)) *rtServer {
	t.Helper()
	ticks := make(chan time.Time)
	l, err := NewLookup(LookupOptions{
		LocalAdmins: map[string]config.UserConfig{"ops": localAdmin(opsLine)},
		Now:         svc.clock.Now,
		TrustTicks:  func() (<-chan time.Time, func()) { return ticks, func() {} },
	})
	if err != nil {
		t.Fatal(err)
	}
	log := &rtLog{}
	// The pull's bound is the default (PullTimeout), on the fake clock.
	rt, err := NewRuntime(RuntimeOptions{
		Lookup: l, StateDir: stateDir, Issuer: svc.issuer, AllowPlainHTTP: true,
		Refresh: rtRefresh, Trust: rtTrust, Timer: svc.clock.Timer, Elapsed: svc.clock.Elapsed, Logf: log.logf,
	})
	if err != nil {
		t.Fatal(err)
	}
	if adjust != nil {
		adjust(rt)
	}
	rt.Load()
	s := &rtServer{rt: rt, log: log, ticks: ticks, opsFP: fpOf(t, opsLine), cancel: func() {}}
	t.Cleanup(func() { s.stop() })
	return s
}

// start starts the schedule and the trust watch.
func (s *rtServer) start() {
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.rt.StartRefresh(ctx)
}

// stop ends the schedule and the trust watch and waits for them.
func (s *rtServer) stop() {
	s.cancel()
	s.rt.cancel()
	s.rt.wait()
}

// refresh is an admin's refresh; it fails the test on an error.
func (s *rtServer) refresh(t *testing.T) users.DirectoryRefresh {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	rep, err := s.rt.Refresh(ctx)
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if rep.Directory == nil || rep.Logins == nil || len(rep.Logins) != 0 {
		t.Fatalf("Refresh report %+v: want a directory row and no logins", rep)
	}
	return *rep.Directory
}

// refreshTimingOut is an admin's refresh against a service that never
// answers the directory request (faultHang): it checks, on the fake clock,
// that the pull is still waiting just before PullTimeout has passed since it
// started, and that it ends, failed, as soon as it has: well before the
// transport's own response-header timeout (15 s of real time) could end it.
func (s *rtServer) refreshTimingOut(t *testing.T, svc *rtService) users.DirectoryRefresh {
	t.Helper()
	rows := make(chan users.RefreshReport, 1)
	errs := make(chan error, 1)
	go func() {
		rep, err := s.rt.Refresh(context.Background())
		if err != nil {
			errs <- err
			return
		}
		rows <- rep
	}()
	select {
	case <-svc.hung:
	case <-time.After(10 * time.Second):
		t.Fatal("the directory request did not reach the service")
	}
	svc.clock.advance(PullTimeout - time.Nanosecond)
	select {
	case rep := <-rows:
		t.Fatalf("the pull ended before its bound had passed: %+v", rep.Directory)
	case err := <-errs:
		t.Fatalf("Refresh: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	svc.clock.advance(time.Nanosecond)
	select {
	case rep := <-rows:
		return *rep.Directory
	case err := <-errs:
		t.Fatalf("Refresh: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("the pull did not end when its bound passed on the runtime's clock")
	}
	return users.DirectoryRefresh{}
}

// owner is the name of the user key fp resolves to, "" for nobody.
func (s *rtServer) owner(fp string) string {
	u, _, ok := s.rt.UserForKey(fp)
	if !ok {
		return ""
	}
	return u.Name
}

// listedNames are the users of the list the server answers for, sorted.
func (s *rtServer) listedNames() []string {
	var out []string
	for _, u := range s.rt.Status().Users {
		if u.Source == UserDirectory {
			out = append(out, u.Name)
		}
	}
	sort.Strings(out)
	return out
}

// copyBytes is users.json in stateDir, or nil when there is none.
func copyBytes(t *testing.T, stateDir string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(stateDir, CopyFileName))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// noSecretsIn fails the test when text holds any of secrets, an assertion's
// header, or a private key.
func noSecretsIn(t *testing.T, what, text string, secrets []string) {
	t.Helper()
	for _, s := range secrets {
		if s != "" && strings.Contains(text, s) {
			t.Errorf("%s holds a token or an assertion", what)
		}
	}
	header := base64.RawURLEncoding.EncodeToString([]byte(assertionHeader))
	for _, bad := range []string{"PRIVATE KEY", header, "client-authentication+jwt", "client_assertion"} {
		if strings.Contains(text, bad) {
			t.Errorf("%s holds %q", what, bad)
		}
	}
}
