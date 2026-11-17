package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charliek/shed/internal/directory"
	"github.com/charliek/shed/internal/directory/refserver"
	"github.com/charliek/shed/internal/users"
)

// The tests of serve's directory steps at a graceful stop (stopUserSource),
// against the reference server over loopback plain HTTP, on a clock the test
// moves. The runtime is serve's (newDirectoryUsers), but its schedule and
// trust watch are never started: its pulls are an admin's refreshes, so the
// only work that can remove the kept copy is the stop's own check (and a
// good pull's, at its end), and nothing depends on when a goroutine runs.

// stopHarness is a directory server's user source, enrolled with a reference
// server, holding a good pull's list and its kept copy.
type stopHarness struct {
	t        *testing.T
	rt       *directory.Runtime
	offset   *atomic.Int64
	copyPath string
	control  string
	recorder *e2eRecorder
}

// newStopHarness enrols a server with a reference server (ada listed) and
// builds serve's runtime for it, then pulls once: the list is held and its
// copy written. The trust time is two minutes.
func newStopHarness(t *testing.T) *stopHarness {
	t.Helper()
	h := &stopHarness{t: t, offset: &atomic.Int64{}, control: t.TempDir(), recorder: &e2eRecorder{}}
	now := func() time.Time { return time.Now().Add(time.Duration(h.offset.Load())) }

	refLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	issuer := "http://" + refLn.Addr().String()
	cd, err := refserver.OpenControlDir(h.control)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cd.Close() })
	ref, err := refserver.New(refserver.Config{Issuer: issuer, Organization: "example", Inputs: cd, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	refTS := httptest.NewUnstartedServer(h.recorder.wrap(ref))
	refTS.Listener.Close()
	refTS.Listener = refLn
	refTS.Start()
	t.Cleanup(refTS.Close)

	_, opsLine := e2eKey(t)
	stateDir := filepath.Join(t.TempDir(), "state")
	writeServeConfig(t, "state_dir: "+stateDir+"\n"+
		"auth:\n  mode: mtls\n  users:\n    ops:\n      admin: true\n      keys:\n        - {key: \""+opsLine+"\", label: desk}\n"+
		"  directory:\n    url: "+issuer+"\n    allow_plain_http: true\n    refresh: 1m\n    trust: 2m\n")
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	code := "stop-" + base64.RawURLEncoding.EncodeToString([]byte(t.Name()))[:16]
	h.writeControl("codes", "stop-server "+code+"\n")
	h.listUsers("ada")
	var out, errOut bytes.Buffer
	if err := directoryEnroll(context.Background(), enrolStreams{in: strings.NewReader(code + "\n"), out: &out, errOut: &errOut}, "stop.example.test", true); err != nil {
		t.Fatalf("enrol: %v\n%s%s", err, out.String(), errOut.String())
	}

	prevNow, prevTimer := directoryNow, directoryTimer
	directoryNow = now
	directoryTimer = func(time.Duration) (<-chan time.Time, func()) { return make(chan time.Time), func() {} }
	t.Cleanup(func() { directoryNow, directoryTimer = prevNow, prevTimer })
	h.rt, err = newDirectoryUsers(cfg, serverStateDir(cfg))
	if err != nil {
		t.Fatal(err)
	}
	h.copyPath = filepath.Join(serverStateDir(cfg), directory.StateDirName, directory.CopyFileName)
	if row := h.refresh(); row.Status != users.DirectoryRefreshGood || !h.hasCopy() {
		t.Fatalf("set-up pull: %+v", row)
	}
	return h
}

func (h *stopHarness) writeControl(name, content string) {
	h.t.Helper()
	if err := os.WriteFile(filepath.Join(h.control, name), []byte(content), 0o600); err != nil {
		h.t.Fatal(err)
	}
}

// listUsers serves these people, none an admin, each with a fresh key.
func (h *stopHarness) listUsers(names ...string) {
	h.t.Helper()
	var b strings.Builder
	b.WriteString("[")
	for i, name := range names {
		_, line := e2eKey(h.t)
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"name":"` + name + `","admin":false,"keys":[{"key":"` + line + `","label":"laptop"}]}`)
	}
	b.WriteString("]")
	h.writeControl("users.json", b.String())
}

// refresh is an admin's refresh: the pull's row.
func (h *stopHarness) refresh() users.DirectoryRefresh {
	rep, err := h.rt.Refresh(context.Background())
	if err != nil || rep.Directory == nil {
		h.t.Errorf("Refresh: %+v %v", rep, err)
		return users.DirectoryRefresh{}
	}
	return *rep.Directory
}

func (h *stopHarness) hasCopy() bool {
	_, err := os.Stat(h.copyPath)
	return err == nil
}

// pastTrust moves the clock past the end of the trust time of the list
// pulled at the start.
func (h *stopHarness) pastTrust() { h.offset.Store(int64(2*time.Minute + time.Second)) }

// listed is whether the list held names name.
func (h *stopHarness) listed(name string) bool {
	_, ok := h.rt.User(name)
	return ok
}

// TestStopUserSource: serve's directory steps at a graceful stop.
func TestStopUserSource(t *testing.T) {
	quiet := func(string, ...any) {}

	t.Run("a stop after the end of the trust time removes the copy", func(t *testing.T) {
		h := newStopHarness(t)
		h.pastTrust()
		if !h.hasCopy() {
			t.Fatal("the copy went before the stop: the test checks nothing")
		}
		stopUserSource(h.rt.CheckTrustAtStop, directory.StopTrustBound, func() {}, func() {}, quiet)
		if h.hasCopy() {
			t.Fatal("the kept copy outlived a stop after the end of its trust time")
		}
	})

	t.Run("a stop inside the trust time keeps the copy", func(t *testing.T) {
		h := newStopHarness(t)
		stopUserSource(h.rt.CheckTrustAtStop, directory.StopTrustBound, func() {}, func() {}, quiet)
		if !h.hasCopy() {
			t.Fatal("a stop inside the trust time removed the copy")
		}
	})

	t.Run("a pull held open across the stop is discarded", func(t *testing.T) {
		h := newStopHarness(t)
		h.listUsers("ada", "bob")
		hold, held := make(chan struct{}), make(chan struct{}, 1)
		release := sync.OnceFunc(func() { close(hold) })
		t.Cleanup(release)
		h.recorder.setHold(hold, held)
		rows := make(chan users.DirectoryRefresh, 1)
		go func() { rows <- h.refresh() }()
		select {
		case <-held:
		case <-time.After(10 * time.Second):
			t.Fatal("the pull did not reach the service")
		}
		// The held list's trust time ends: the stop removes its copy. The
		// held pull goes on (cancelRefresh here ends nothing, standing for a
		// pull past its network work when the refresh is cancelled) and its
		// answer, a list trusted on its own clock, arrives during the
		// servers' shutdown.
		h.pastTrust()
		var late users.DirectoryRefresh
		stopUserSource(h.rt.CheckTrustAtStop, directory.StopTrustBound, func() {}, func() {
			release()
			select {
			case late = <-rows:
			case <-time.After(10 * time.Second):
				t.Error("the held pull did not end")
			}
		}, quiet)
		if late.Status == users.DirectoryRefreshGood {
			t.Errorf("the late pull's row: %+v", late)
		}
		if h.hasCopy() {
			t.Fatal("a pull that ended after the stop began wrote the kept copy")
		}
		if h.listed("bob") {
			t.Fatal("a pull that ended after the stop began published its list")
		}
	})

	t.Run("a trust time that ends during the shutdown is caught", func(t *testing.T) {
		h := newStopHarness(t)
		var shut bool
		stopUserSource(h.rt.CheckTrustAtStop, directory.StopTrustBound, func() {}, func() {
			if !h.hasCopy() {
				t.Error("the copy went at the start of the stop, inside its trust time")
			}
			h.pastTrust() // a slow shutdown, past the end of the trust time
			shut = true
		}, quiet)
		if !shut {
			t.Fatal("the servers were not shut down")
		}
		if h.hasCopy() {
			t.Fatal("the kept copy outlived a stop during which its trust time ended")
		}
	})

	t.Run("the order of the steps", func(t *testing.T) {
		var steps []string
		check := func(context.Context) error { steps = append(steps, "check"); return nil }
		stopUserSource(check, time.Second, func() { steps = append(steps, "cancel") }, func() { steps = append(steps, "shutdown") }, quiet)
		if got := strings.Join(steps, " "); got != "cancel check shutdown check" {
			t.Fatalf("steps %q, want the refresh cancelled, a check, the shutdown, then the last check", got)
		}
		steps = nil
		stopUserSource(nil, time.Second, func() { steps = append(steps, "cancel") }, func() { steps = append(steps, "shutdown") }, quiet)
		if got := strings.Join(steps, " "); got != "cancel shutdown" {
			t.Fatalf("without a directory: steps %q", got)
		}
	})

	t.Run("a stuck log writer does not hold the stop", func(t *testing.T) {
		stuck := make(chan struct{})
		t.Cleanup(func() { close(stuck) })
		var warned atomic.Int32
		logf := func(format string, _ ...any) {
			if strings.Contains(format, "did not finish within") {
				warned.Add(1)
			}
			<-stuck
		}
		hung := func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }
		done := make(chan struct{})
		go func() {
			defer close(done)
			stopUserSource(hung, 10*time.Millisecond, func() {}, func() {}, logf)
		}()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("the stop waited for a stuck log writer")
		}
		deadline := time.Now().Add(10 * time.Second)
		for warned.Load() != 2 {
			if time.Now().After(deadline) {
				t.Fatalf("%d warnings, want one per check that timed out", warned.Load())
			}
			time.Sleep(5 * time.Millisecond)
		}
	})
}
