package directory

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charliek/shed/internal/users"
)

// The runtime's lifecycle and single flight, its log lines, and what it
// never logs.

// TestRuntimeStopForgetsTheClient: when the runtime stops, the client and
// its access token are dropped and the enrolment's private key is cleared:
// at once when no pull runs, or when the pull running ends.
func TestRuntimeStopForgetsTheClient(t *testing.T) {
	for _, running := range []bool{false, true} {
		t.Run(fmt.Sprintf("a pull running %v", running), func(t *testing.T) {
			_, svc, dir, opsLine, _ := rtSetup(t)
			srv := startRT(t, svc, dir, opsLine, nil)
			srv.refresh(t)
			srv.rt.mu.Lock()
			key := srv.rt.clientOf.PrivateKey
			srv.rt.mu.Unlock()
			if allZero(key) {
				t.Fatal("no key held after a good pull")
			}
			var gate chan struct{}
			var f *flight
			if running {
				gate = make(chan struct{})
				svc.setGate(gate)
				f = srv.rt.request()
				<-svc.arrived
			}
			srv.rt.cancel()
			if running {
				close(gate)
				<-f.done
			}
			srv.rt.wait()
			deadline := time.Now().Add(10 * time.Second)
			for {
				srv.rt.mu.Lock()
				gone := srv.rt.client == nil && srv.rt.clientOf == nil
				srv.rt.mu.Unlock()
				if gone && allZero(key) {
					return
				}
				if time.Now().After(deadline) {
					t.Fatalf("after the stop: client dropped %v, key cleared %v", gone, allZero(key))
				}
				time.Sleep(10 * time.Millisecond)
			}
		})
	}
}

func allZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

// TestRuntimeRequestAfterStopStartsNothing: once the runtime's context has
// ended a request starts no pull: it returns at once, ended, as "the server
// is stopping". A pull running then ends, and the one queued behind it ends
// without a request. The test barrier (wait) returns once both have.
func TestRuntimeRequestAfterStopStartsNothing(t *testing.T) {
	_, svc, dir, opsLine, _ := rtSetup(t)
	srv := startRT(t, svc, dir, opsLine, nil)
	gate := make(chan struct{})
	var release sync.Once
	open := func() { release.Do(func() { close(gate) }) }
	t.Cleanup(open) // a failure below must not leave the service's handler held
	svc.setGate(gate)
	running := srv.rt.request()
	<-svc.arrived
	queued := srv.rt.request()
	if queued == running {
		t.Fatal("the second request did not queue")
	}
	srv.rt.cancel()
	after := srv.rt.request()
	select {
	case <-after.done:
	default:
		t.Fatal("a request after the stop did not return at once")
	}
	if after == queued || after.row.Error != "the server is stopping" {
		t.Fatalf("a request after the stop: %+v", after.row)
	}
	open()
	for _, f := range []*flight{running, queued} {
		select {
		case <-f.done:
		case <-time.After(10 * time.Second):
			t.Fatal("a pull did not end after the stop")
		}
	}
	if queued.row.Error != "the server is stopping" || queued.row.Status != users.DirectoryRefreshFailed {
		t.Fatalf("the queued pull's row: %+v", queued.row)
	}
	srv.rt.wait()
	if n := svc.directoryCount(); n != 1 {
		t.Fatalf("%d directory requests; want only the one running at the stop", n)
	}
}

// TestRuntimeRefreshRacingShutdown: admins' refreshes racing the stop each
// return (a row or their own deadline), the barrier returns, and no pull
// starts after it. Run under -race.
func TestRuntimeRefreshRacingShutdown(t *testing.T) {
	_, svc, dir, opsLine, _ := rtSetup(t)
	srv := startRT(t, svc, dir, opsLine, nil)
	srv.start()
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				rep, err := srv.rt.Refresh(ctx)
				cancel()
				if err != nil {
					t.Errorf("Refresh: %v", err)
					return
				}
				if rep.Directory == nil {
					t.Error("Refresh: no directory row")
					return
				}
			}
		}()
	}
	var halt sync.Once
	stopCallers := func() {
		halt.Do(func() { close(stop) })
		wg.Wait()
	}
	time.Sleep(50 * time.Millisecond)
	srv.cancel()
	srv.rt.cancel()
	waited := make(chan struct{})
	go func() {
		srv.rt.wait()
		close(waited)
	}()
	select {
	case <-waited:
	case <-time.After(10 * time.Second):
		stopCallers()
		t.Fatal("the barrier did not return while refreshes raced the stop")
	}
	n := svc.directoryCount() + svc.tokenCount()
	time.Sleep(50 * time.Millisecond)
	stopCallers()
	if m := svc.directoryCount() + svc.tokenCount(); m != n {
		t.Fatalf("%d requests sent after the barrier returned", m-n)
	}
}

// TestRuntimeSingleFlightHandoff: a request made after a pull's callers were
// told its result, and before the next pull asked for is taken up, gets a
// new pull, which starts after the call; and the service never sees two
// directory requests at once, whatever the number of callers.
func TestRuntimeSingleFlightHandoff(t *testing.T) {
	_, svc, dir, opsLine, ada := rtSetup(t)
	bob := newPerson(t, "bob", false)
	var once sync.Once
	asked := make(chan *flight, 1)
	srv := startRT(t, svc, dir, opsLine, func(rt *Runtime) {
		rt.afterDone = func() {
			once.Do(func() {
				svc.dir.setUsers(ada.served(), bob.served())
				asked <- rt.request()
			})
		}
	})
	done := srv.rt.request()
	<-done.done
	var inWindow *flight
	select {
	case inWindow = <-asked:
	case <-time.After(10 * time.Second):
		t.Fatal("the hand-off hook did not run")
	}
	if inWindow == done {
		t.Fatal("a request after a pull's result was given that pull")
	}
	select {
	case <-inWindow.done:
	case <-time.After(10 * time.Second):
		t.Fatal("the pull asked for in the hand-off never ran")
	}
	if done.row.Users != 1 || inWindow.row.Users != 2 {
		t.Fatalf("rows: the first %+v, the hand-off's %+v", done.row, inWindow.row)
	}

	// Many callers at once: one directory request at a time.
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := srv.rt.Refresh(context.Background()); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if n := svc.mostAtOnce(); n != 1 {
		t.Fatalf("the service answered %d directory requests at once", n)
	}
}

// TestRuntimeFailedCapsTheRetryAfter: the schedule waits after a failure at
// most MaxRetryAfter, whatever the failure carries.
func TestRuntimeFailedCapsTheRetryAfter(t *testing.T) {
	clock, svc, dir, opsLine, _ := rtSetup(t)
	srv := startRT(t, svc, dir, opsLine, nil)
	for _, ra := range []time.Duration{3 * time.Hour, time.Hour + time.Second} {
		srv.rt.failed(clock.Now(), &Failure{Step: StepDirectory, Kind: KindUnavailable, Status: 503, RetryAfter: ra, HasRetryAfter: true})
		if got := srv.rt.Status().NextAttempt; !got.Equal(clock.Now().Add(MaxRetryAfter)) {
			t.Fatalf("Retry-After %s: next attempt %s, want an hour on", ra, stamp(got))
		}
	}
}

// TestRuntimeInvalidClientLogged: a refused login is logged at every
// attempt, with the service's clock beside this server's.
func TestRuntimeInvalidClientLogged(t *testing.T) {
	clock, svc, dir, opsLine, _ := rtSetup(t)
	srv := startRT(t, svc, dir, opsLine, nil)
	svc.setMode(faultInvalidClient, "")
	for i := 0; i < 2; i++ {
		if row := srv.refresh(t); row.Status != users.DirectoryRefreshFailed || !strings.Contains(row.Error, "invalid-client") {
			t.Fatalf("pull %d: %+v", i, row)
		}
	}
	want := fmt.Sprintf("(the service's clock %s, this server's %s)", stamp(clock.Now().Add(time.Hour).Truncate(time.Second)), stamp(clock.Now()))
	if n := srv.log.count("refused this server's login"); n != 2 {
		t.Errorf("the refused login was logged %d times over two attempts, want 2:\n%s", n, srv.log)
	}
	if n := srv.log.count(want); n != 2 {
		t.Errorf("%q in %d lines, want 2:\n%s", want, n, srv.log)
	}
}

// TestRuntimeDisabledGoodDisabledLogsAgain: a line logged once per change of
// state is logged again when the state comes back after a good pull.
func TestRuntimeDisabledGoodDisabledLogsAgain(t *testing.T) {
	_, svc, dir, opsLine, _ := rtSetup(t)
	srv := startRT(t, svc, dir, opsLine, nil)
	e := readEnrolmentID(t, dir)
	srv.refresh(t)
	for round := 1; round <= 2; round++ {
		svc.dir.setDisabled(e, true)
		srv.refresh(t)
		srv.refresh(t)
		svc.dir.setDisabled(e, false)
		if row := srv.refresh(t); row.Status != users.DirectoryRefreshGood {
			t.Fatalf("round %d: the good pull %+v", round, row)
		}
		if n := srv.log.count("SERVER DISABLED"); n != round {
			t.Fatalf("after round %d the disabled server was logged %d times:\n%s", round, n, srv.log)
		}
	}
}

// readEnrolmentID is the client id of the enrolment in dir.
func readEnrolmentID(t *testing.T, dir string) string {
	t.Helper()
	d, err := OpenStateDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	data, err := d.ReadFile(EnrolmentFileName, MaxEnrolmentFileSize)
	if err != nil {
		t.Fatal(err)
	}
	e, err := ParseEnrolmentFile(data)
	if err != nil {
		t.Fatal(err)
	}
	clear(e.PrivateKey)
	return e.ClientID
}

// TestRuntimeReflectedCredentialsNotLogged: a service that quotes back the
// assertion in a token error, or the bearer token in a directory error (its
// code, its message, a redirect's Location), never gets either into the
// runtime's log, its rows or its status: each is replaced by
// CredentialMarker where the failure is built.
func TestRuntimeReflectedCredentialsNotLogged(t *testing.T) {
	global := captureLog(t)
	_, svc, dir, opsLine, _ := rtSetup(t)
	srv := startRT(t, svc, dir, opsLine, nil)
	var seen []string
	for _, m := range []faultMode{faultReflectAssertion, faultReflectBearer, faultRedirectBearer} {
		if m != faultReflectAssertion {
			// The bearer is the token a good login gave.
			svc.setMode(faultNone, "")
			if row := srv.refresh(t); row.Status != users.DirectoryRefreshGood {
				t.Fatalf("pull: %+v", row)
			}
		}
		svc.setMode(m, "")
		row := srv.refresh(t)
		if row.Status != users.DirectoryRefreshFailed || !strings.Contains(row.Error, CredentialMarker) {
			t.Fatalf("%s: row %+v; want a failure with the credential replaced", m, row)
		}
		seen = append(seen, row.Error, srv.rt.Status().LastError)
	}
	if n := srv.log.count(CredentialMarker); n < 3 {
		t.Errorf("the marker is in %d log lines, want the three failures':\n%s", n, srv.log)
	}
	noSecretsIn(t, "the log, the rows and the status", srv.log.String()+global.String()+strings.Join(seen, "\n"), svc.secrets())
}

// TestScrubKeepsTheClassification: a failure's error scrubbed of a
// credential still matches what it matched, and no longer unwraps to the
// text that held it.
func TestScrubKeepsTheClassification(t *testing.T) {
	inner := fmt.Errorf("%w: the organization %q", ErrOrganization, "tok-123")
	f := scrub(&Failure{Step: StepDirectory, Kind: KindBadAnswer, Code: "x tok-123", Message: "tok-123 tok-123", Err: inner}, "tok-123", "")
	if strings.Contains(f.Error(), "tok-123") || !errors.Is(f, ErrOrganization) {
		t.Fatalf("%v (is ErrOrganization: %v)", f, errors.Is(f, ErrOrganization))
	}
	if u := errors.Unwrap(f.Err); u != nil {
		t.Fatalf("the scrubbed error unwraps to %v", u)
	}
	if g := scrub(&Failure{Code: "c", Err: inner}, "absent"); g.Err != inner {
		t.Fatal("an error holding no credential was replaced")
	}
}
