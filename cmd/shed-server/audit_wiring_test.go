package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	gossh "golang.org/x/crypto/ssh"

	"github.com/charliek/shed/internal/api"
	"github.com/charliek/shed/internal/audit"
	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/sshd"
)

// ownedShedBackend holds one running shed of the service client build-1. Its
// exec reaches the shed's agent and, when release is set, the first exec
// waits for it to be closed before it reports the command started
// (OnStarted); reached is closed when that exec arrives. Every later exec
// reports its start at once. execs counts them all.
type ownedShedBackend struct {
	backend.Backend
	reached, release chan struct{}
	done             chan struct{} // closed when the exec that waited returns
	execs            atomic.Int32
}

func (*ownedShedBackend) Type() backend.Type { return backend.TypeVZ }

func (*ownedShedBackend) GetShed(_ context.Context, name string) (*config.Shed, error) {
	if name != "ci-box" {
		return nil, config.ErrShedNotFoundSentinel
	}
	return &config.Shed{Name: "ci-box", Owner: "build-1", Status: config.StatusRunning, CreatedAt: time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)}, nil
}

// DialService fails: no test here opens a tunnel; a Connect that gets this
// far answers 502.
func (*ownedShedBackend) DialService(context.Context, string, uint16) (net.Conn, error) {
	return nil, errors.New("connection refused")
}

func (b *ownedShedBackend) Exec(_ context.Context, _ string, opts backend.ExecOptions) error {
	if n := b.execs.Add(1); b.release != nil && n == 1 {
		defer close(b.done)
		close(b.reached)
		<-b.release
	}
	if opts.OnStarted != nil {
		opts.OnStarted()
	}
	return nil
}

// frontDoorRig is serve's front doors (runFrontDoors, with serveDoors' doors)
// over be, with no user list (every key is the implicit user), an audit log,
// and before them an extra door that fails when the test says.
type frontDoorRig struct {
	apiServer *api.Server
	sshAddr   string
	httpAddr  string // set when the rig serves an HTTP door (rigOptions.httpHandler)
	auditLog  *audit.Log
	auditPath string
	signals   chan os.Signal
	fail      chan error
	returned  chan error
	// began is closed when the servers' shutdown begins: runFrontDoors runs
	// beforeShutdown, after the reaper has stopped.
	began chan struct{}
	// reaperStopping is closed when the shutdown waits on the reaper's stop
	// (rigOptions.reaperRelease).
	reaperStopping chan struct{}
	// sshShutdownEntered is closed when the SSH door's shutdown is called;
	// sshShutdownDone then gets what it returned.
	sshShutdownEntered chan struct{}
	sshShutdownDone    chan error
}

// rigOptions changes the rig: an HTTP door (a real loopback listener served
// by httpHandler, shut down with serve's other doors), the shutdown bound (10
// seconds when zero), and a max_age reaper whose stop, the shutdown's first
// wait, closes reaperStopping and then waits for reaperRelease to be closed
// (no reaper when reaperRelease is nil).
type rigOptions struct {
	httpHandler   http.Handler
	timeout       time.Duration
	reaperRelease chan struct{}
}

func startFrontDoors(t *testing.T, be backend.Backend) *frontDoorRig {
	t.Helper()
	return startFrontDoorsWith(t, be, rigOptions{})
}

// freeAddr returns a loopback address nothing listens on (yet).
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

func startFrontDoorsWith(t *testing.T, be backend.Backend, opts rigOptions) *frontDoorRig {
	t.Helper()
	r := &frontDoorRig{
		signals: make(chan os.Signal, 1), fail: make(chan error, 1), returned: make(chan error, 1),
		began: make(chan struct{}), reaperStopping: make(chan struct{}),
		sshShutdownEntered: make(chan struct{}), sshShutdownDone: make(chan error, 1),
	}
	r.auditPath = filepath.Join(t.TempDir(), audit.FileName)
	l, err := audit.Open(r.auditPath, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	r.auditLog = l
	t.Cleanup(func() { _ = l.Close() })

	guard := backend.NewGuard(be)
	r.sshAddr = freeAddr(t)
	sshServer, err := sshd.NewServer(guard, filepath.Join(t.TempDir(), "host_key"), r.sshAddr, nil, sshd.KeyAuth{})
	if err != nil {
		t.Fatal(err)
	}
	r.apiServer = api.NewServer(guard, &config.ServerConfig{Name: "test-server", HTTPPort: 8080}, sshServer.GetHostPublicKey(), nil, nil)

	var httpServer *http.Server
	if opts.httpHandler != nil {
		r.httpAddr = freeAddr(t)
		httpServer = &http.Server{Addr: r.httpAddr, Handler: opts.httpHandler, ReadHeaderTimeout: 10 * time.Second}
	}
	timeout := opts.timeout
	if timeout == 0 {
		timeout = 10 * time.Second
	}

	failing := frontDoor{
		name:     "test server",
		serve:    func() error { return <-r.fail },
		shutdown: func(context.Context) error { return nil },
	}
	doors := append([]frontDoor{failing}, serveDoors(httpServer, nil, r.apiServer, sshServer)...)
	for i := range doors {
		if doors[i].name != "SSH server" {
			continue
		}
		shutdown := doors[i].shutdown
		doors[i].shutdown = func(ctx context.Context) error {
			close(r.sshShutdownEntered)
			err := shutdown(ctx)
			r.sshShutdownDone <- err
			return err
		}
	}
	fd := frontDoors{
		api:            r.apiServer,
		ssh:            sshServer,
		auditLog:       l,
		doors:          doors,
		beforeShutdown: func() { close(r.began) },
		timeout:        timeout,
	}
	if release := opts.reaperRelease; release != nil {
		fd.startReaper = func() func() {
			return func() {
				close(r.reaperStopping)
				<-release
			}
		}
	}
	go func() { r.returned <- runFrontDoors(fd, r.signals) }()
	t.Cleanup(func() {
		select {
		case r.signals <- syscall.SIGTERM:
		default:
		}
		select {
		case <-r.returned:
		case <-time.After(15 * time.Second):
			t.Error("runFrontDoors did not return")
		}
	})
	return r
}

// sshClient dials the rig's SSH server as ci-box, once it serves.
func (r *frontDoorRig) sshClient(t *testing.T) *gossh.Client {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := gossh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		client, err := gossh.Dial("tcp", r.sshAddr, &gossh.ClientConfig{
			User:            "ci-box",
			Auth:            []gossh.AuthMethod{gossh.PublicKeys(signer)},
			HostKeyCallback: gossh.InsecureIgnoreHostKey(),
			Timeout:         2 * time.Second,
		})
		if err == nil {
			t.Cleanup(func() { _ = client.Close() })
			return client
		}
		if time.Now().After(deadline) {
			t.Fatalf("the SSH server did not start: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// wait returns what runFrontDoors returned.
func (r *frontDoorRig) wait(t *testing.T) error {
	t.Helper()
	select {
	case err := <-r.returned:
		r.returned <- err // for the cleanup
		return err
	case <-time.After(15 * time.Second):
		t.Fatal("runFrontDoors did not return")
		return nil
	}
}

// events counts the audit log's lines by event, and checks the ssh lines.
func (r *frontDoorRig) events(t *testing.T) map[string]int {
	t.Helper()
	raw, err := os.ReadFile(r.auditPath)
	if err != nil {
		t.Fatal(err)
	}
	events := map[string]int{}
	sc := bufio.NewScanner(bytes.NewReader(raw))
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("line %q: %v", sc.Text(), err)
		}
		ev, _ := m["event"].(string)
		events[ev]++
		if ev == audit.EventSSH && (m["shed"] != "ci-box" || m["owner"] != "build-1" || m["channel"] != audit.SSHChannelExec) {
			t.Errorf("ssh line = %v", m)
		}
	}
	return events
}

// TestAuditLogReachesBothFrontDoors: serve's own start-up (runFrontDoors)
// hands the one audit log to both front doors: an HTTPS exec writes its
// exec.end line, and an SSH exec into the same service client's shed its ssh
// line, to the same file.
func TestAuditLogReachesBothFrontDoors(t *testing.T) {
	r := startFrontDoors(t, &ownedShedBackend{})
	client := r.sshClient(t) // the doors serve: the log is attached

	// The HTTPS exec (open mode: the implicit user).
	req := httptest.NewRequest(http.MethodPost, "/api/sheds/ci-box/exec", bytes.NewReader([]byte(`{"argv":["true"]}`)))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	r.apiServer.Router().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("HTTPS exec: status %d, body %s", rr.Code, rr.Body.String())
	}

	// The SSH exec.
	sess, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.Run("true"); err != nil {
		t.Fatalf("SSH exec: %v", err)
	}
	_ = sess.Close()
	_ = client.Close() // so no connection holds the shutdown up

	r.signals <- syscall.SIGTERM
	if err := r.wait(t); err != nil {
		t.Fatalf("runFrontDoors = %v", err)
	}
	if ev := r.events(t); ev[audit.EventExecEnd] != 1 || ev[audit.EventSSH] != 1 {
		t.Errorf("the one log holds %v, want one exec.end and one ssh line", ev)
	}
}

// syncBuffer is a log destination safe to read while servers may still log.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestFrontDoorsShutdownLogsEachStepOnce: a graceful stop logs the entry
// gates closing once, before any door's shutdown line (they close first),
// and each door's shutdown line exactly once; the SSH server's own shutdown
// adds no second line. No line from the signal on appears twice.
func TestFrontDoorsShutdownLogsEachStepOnce(t *testing.T) {
	logs := &syncBuffer{}
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(logs)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(prevOut); log.SetFlags(prevFlags) })

	r := startFrontDoors(t, &ownedShedBackend{})
	_ = r.sshClient(t).Close() // the doors serve; no connection holds the shutdown up
	r.signals <- syscall.SIGTERM
	if err := r.wait(t); err != nil {
		t.Fatalf("runFrontDoors = %v", err)
	}

	all := strings.Split(strings.TrimSpace(logs.String()), "\n")
	start := -1
	for i, l := range all {
		if strings.HasPrefix(l, "Received signal ") {
			start = i
			break
		}
	}
	if start < 0 {
		t.Fatalf("no signal line in the log:\n%s", logs.String())
	}
	lines := all[start:]
	seen := map[string]int{}
	gate := -1
	for i, l := range lines {
		seen[l]++
		if l == "Closing entry gates..." && gate < 0 {
			gate = i
		}
	}
	for l, n := range seen {
		if n > 1 {
			t.Errorf("%q logged %d times in the shutdown", l, n)
		}
	}
	if gate < 0 {
		t.Fatalf("no entry-gate line in the shutdown:\n%s", strings.Join(lines, "\n"))
	}
	doors := 0
	for i, l := range lines {
		if !strings.HasPrefix(l, "Shutting down ") {
			continue
		}
		doors++
		if i < gate {
			t.Errorf("%q logged before the entry gates closed", l)
		}
	}
	for _, want := range []string{"Shutting down test server...", "Shutting down Connect tunnels...", "Shutting down SSH server..."} {
		if seen[want] != 1 {
			t.Errorf("%q logged %d times, want once", want, seen[want])
		}
	}
	if doors != 3 {
		t.Errorf("%d door shutdown lines, want 3:\n%s", doors, strings.Join(lines, "\n"))
	}
}

// TestFrontDoorsShutdownKeepsAnAdmittedEntrysLine: on both ways serve ends (a
// signal, or a server's error), an SSH exec into the client's shed that was
// handed to the shed's agent just before, and whose line is not written yet,
// keeps its line: the servers stop and drain before the audit log is closed,
// and runFrontDoors closes it. After the error, every server is stopped too
// (the SSH listener is gone).
func TestFrontDoorsShutdownKeepsAnAdmittedEntrysLine(t *testing.T) {
	for _, tc := range []struct {
		name string
		stop func(r *frontDoorRig)
		want error
	}{
		{"signal", func(r *frontDoorRig) { r.signals <- syscall.SIGTERM }, nil},
		{"server error", func(r *frontDoorRig) { r.fail <- errors.New("listen tcp: address already in use") }, errors.New("test server error: listen tcp: address already in use")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			be := &ownedShedBackend{reached: make(chan struct{}), release: make(chan struct{}), done: make(chan struct{})}
			r := startFrontDoors(t, be)
			client := r.sshClient(t)
			sess, err := client.NewSession()
			if err != nil {
				t.Fatal(err)
			}
			go func() { _ = sess.Run("true") }()
			select {
			case <-be.reached:
			case <-time.After(10 * time.Second):
				t.Fatal("the exec did not reach the backend")
			}
			// The client goes, so no connection holds the shutdown up.
			_ = client.Close()

			tc.stop(r)
			// The command reports its start only once the SSH server's
			// shutdown, which waits for its line, has begun (its own drain
			// waiting is TestSSHShutdownKeepsAnAdmittedEntrysLine's).
			select {
			case <-r.sshShutdownEntered:
			case <-time.After(10 * time.Second):
				t.Fatal("the SSH server's shutdown was never called")
			}
			close(be.release)
			err = r.wait(t)
			if (err == nil) != (tc.want == nil) || (err != nil && err.Error() != tc.want.Error()) {
				t.Fatalf("runFrontDoors = %v, want %v", err, tc.want)
			}
			select {
			case <-be.done:
			case <-time.After(10 * time.Second):
				t.Fatal("the exec did not finish")
			}
			if ev := r.events(t); ev[audit.EventSSH] != 1 {
				t.Errorf("the log holds %v, want the admitted exec's ssh line", ev)
			}
			if err := r.auditLog.Record(audit.SSHEvent{Event: audit.EventSSH}); err == nil {
				t.Error("the audit log is still open after runFrontDoors returned")
			}
			if c, err := net.DialTimeout("tcp", r.sshAddr, time.Second); err == nil {
				_ = c.Close()
				t.Error("the SSH server still accepts after runFrontDoors returned")
			}
			// The Connect tunnels were drained too: a person's Connect into
			// the client's shed is now refused before any dial.
			rr := httptest.NewRecorder()
			r.apiServer.Router().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/sheds/ci-box/connect/8080", nil))
			if rr.Code != http.StatusServiceUnavailable {
				t.Errorf("a Connect after runFrontDoors returned: status %d, want 503 (drained)", rr.Code)
			}
		})
	}
}

// holdingHandler holds every request open until its client goes, as the
// egress stream does: it answers 200, then waits on the request's context,
// which an HTTP server's graceful shutdown does not end. held gets a value
// for each request it holds.
func holdingHandler(held chan<- struct{}) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		held <- struct{}{}
		<-r.Context().Done()
	})
}

// holdRequest opens a request on the rig's HTTP door, once it serves, and
// returns its connection once the handler holds it. Closing the connection
// ends the request.
func (r *frontDoorRig) holdRequest(t *testing.T, held <-chan struct{}) net.Conn {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var conn net.Conn
	for {
		c, err := net.DialTimeout("tcp", r.httpAddr, time.Second)
		if err == nil {
			conn = c
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the HTTP server did not start: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if _, err := conn.Write([]byte("GET /api/egress/stream HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-held:
	case <-time.After(10 * time.Second):
		t.Fatal("the HTTP server did not hold the request")
	}
	return conn
}

// TestFrontDoorsShutdownClosesEveryGateFirst is the shutdown a request that
// never ends (the egress stream) used to stretch: the HTTP server's graceful
// shutdown waits the whole bound for it. Every entry gate closes when the
// shutdown begins, before its first wait (the reaper's stop, held here): an
// SSH entry into the client's shed attempted then, on a connection opened
// before, is refused at once and never reaches the shed; so is a Connect.
// Every listener's shutdown starts together, so the SSH listener closes
// while the request is still held. The entry admitted before the shutdown
// keeps its line, and its drain is not used up by the HTTP server's wait:
// the SSH server's shutdown is under way, and finishes cleanly, while the
// request is still held.
func TestFrontDoorsShutdownClosesEveryGateFirst(t *testing.T) {
	held := make(chan struct{}, 1)
	reaperRelease := make(chan struct{})
	var releaseOnce sync.Once
	releaseReaper := func() { releaseOnce.Do(func() { close(reaperRelease) }) }
	be := &ownedShedBackend{reached: make(chan struct{}), release: make(chan struct{}), done: make(chan struct{})}
	r := startFrontDoorsWith(t, be, rigOptions{httpHandler: holdingHandler(held), timeout: 10 * time.Second, reaperRelease: reaperRelease})
	t.Cleanup(releaseReaper) // runs before the rig's cleanup waits for runFrontDoors
	httpConn := r.holdRequest(t, held)

	// Admitted before the shutdown: an exec handed to the shed's agent,
	// whose start (and so its line) waits for release.
	client := r.sshClient(t)
	sess, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = sess.Run("true") }()
	select {
	case <-be.reached:
	case <-time.After(10 * time.Second):
		t.Fatal("the exec did not reach the backend")
	}

	r.signals <- syscall.SIGTERM
	select {
	case <-r.reaperStopping:
	case <-time.After(10 * time.Second):
		t.Fatal("the shutdown did not begin")
	}

	// An SSH entry attempted now, while the shutdown waits on the reaper,
	// on the connection opened before: refused at once, never reaching the
	// shed.
	sess2, err := client.NewSession()
	if err != nil {
		t.Fatalf("a new session on the open connection: %v", err)
	}
	var stderr bytes.Buffer
	sess2.Stderr = &stderr
	start := time.Now()
	err = sess2.Run("true")
	if took := time.Since(start); took > 3*time.Second {
		t.Errorf("the entry after the shutdown began took %s to be answered", took)
	}
	if err == nil {
		t.Error("an SSH entry into the client's shed after the shutdown began was let in")
	} else if !strings.Contains(stderr.String(), "shutting down") {
		t.Errorf("the entry after the shutdown began: %v, stderr %q, want the shutdown refusal", err, stderr.String())
	}
	if n := be.execs.Load(); n != 1 {
		t.Errorf("the backend ran %d execs, want only the one admitted before the shutdown", n)
	}

	// A Connect now: refused (503) before any dial.
	rr := httptest.NewRecorder()
	r.apiServer.Router().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/sheds/ci-box/connect/8080", nil))
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("a Connect after the shutdown began: status %d, want 503", rr.Code)
	}

	releaseReaper()
	select {
	case <-r.began:
	case <-time.After(10 * time.Second):
		t.Fatal("the servers' shutdown did not begin")
	}

	// The SSH listener closes although the request still holds the HTTP
	// server's shutdown.
	deadline := time.Now().Add(3 * time.Second)
	for {
		c, err := net.DialTimeout("tcp", r.sshAddr, time.Second)
		if err != nil {
			break
		}
		_ = c.Close()
		if time.Now().After(deadline) {
			t.Error("the SSH listener still accepts while the HTTP request is held")
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// The admitted entry: its drain runs while the request is still held.
	_ = client.Close() // so no SSH connection holds the shutdown up
	select {
	case <-r.sshShutdownEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("the SSH server's shutdown did not begin while the HTTP request was held")
	}
	close(be.release)
	select {
	case err := <-r.sshShutdownDone:
		if err != nil {
			t.Errorf("the SSH server's shutdown = %v, want nil (its drain had its own wait)", err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("the SSH server's shutdown did not finish while the HTTP request was held")
	}
	select {
	case err := <-r.returned:
		r.returned <- err
		t.Fatalf("runFrontDoors returned (%v) while the HTTP request was still held", err)
	default:
	}

	_ = httpConn.Close() // the request ends; the HTTP server's shutdown with it
	if err := r.wait(t); err != nil {
		t.Fatalf("runFrontDoors = %v", err)
	}
	select {
	case <-be.done:
	case <-time.After(10 * time.Second):
		t.Fatal("the exec did not finish")
	}
	if ev := r.events(t); ev[audit.EventSSH] != 1 {
		t.Errorf("the log holds %v, want the one ssh line of the entry admitted before the shutdown", ev)
	}
}
