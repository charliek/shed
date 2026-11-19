package sshd

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	gossh "golang.org/x/crypto/ssh"

	"github.com/charliek/shed/internal/audit"
	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
)

// The ssh lines of the audit log: a person's entry into a service client's
// shed, written once the guarded call that enters it has passed its own
// check and started (audit.go).

var entryCreated = time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)

// entryBackend holds "ci-box", a running shed of the service client
// build-1, and "box", alice's own running shed. Its exec starts the command
// (OnStarted) and its dial connects, unless told otherwise.
type entryBackend struct {
	backend.Backend

	mu   sync.Mutex
	gets map[string]int
	// second, when set, is what every second GetShed of a shed returns (the
	// guarded call's load, after the channel's own check): the shed as it
	// is by then.
	second func(*config.Shed) *config.Shed
	// execErr fails an exec before the command starts (no OnStarted).
	execErr error
	// dialErr fails a dial.
	dialErr error
	// dialWait, when set, runs in a dial before it connects.
	dialWait func(ctx context.Context)
	// dialClosed is closed when a connection a dial returned is closed.
	dialClosed chan struct{}
	closeOnce  sync.Once

	execs, dials int
}

func newEntryBackend() *entryBackend {
	return &entryBackend{gets: map[string]int{}, dialClosed: make(chan struct{})}
}

func (b *entryBackend) GetShed(_ context.Context, name string) (*config.Shed, error) {
	var shed *config.Shed
	switch name {
	case "ci-box":
		shed = &config.Shed{Name: "ci-box", Owner: "build-1", Status: config.StatusRunning, CreatedAt: entryCreated}
	case "box":
		shed = &config.Shed{Name: "box", User: "alice", Status: config.StatusRunning, CreatedAt: entryCreated}
	default:
		return nil, config.ErrShedNotFoundSentinel
	}
	b.mu.Lock()
	b.gets[name]++
	n := b.gets[name]
	second := b.second
	b.mu.Unlock()
	if second != nil && n%2 == 0 {
		return second(shed), nil
	}
	return shed, nil
}

func (b *entryBackend) Exec(_ context.Context, _ string, opts backend.ExecOptions) error {
	b.mu.Lock()
	b.execs++
	err := b.execErr
	b.mu.Unlock()
	if err != nil {
		return err
	}
	if opts.OnStarted != nil {
		opts.OnStarted()
	}
	if opts.Stdout != nil {
		_, _ = opts.Stdout.Write([]byte("ran\n"))
	}
	return nil
}

func (b *entryBackend) DialService(ctx context.Context, _ string, _ uint16) (net.Conn, error) {
	b.mu.Lock()
	b.dials++
	err, wait := b.dialErr, b.dialWait
	b.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if wait != nil {
		wait(ctx)
	}
	server, client := net.Pipe()
	go func() { _, _ = io.Copy(server, server) }()
	return &notifyConn{Conn: client, closed: func() { b.closeOnce.Do(func() { close(b.dialClosed) }) }}, nil
}

func (b *entryBackend) reached() (execs, dials int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.execs, b.dials
}

// notifyConn is a net.Conn that says when it is closed.
type notifyConn struct {
	net.Conn
	closed func()
}

func (c *notifyConn) Close() error {
	c.closed()
	return c.Conn.Close()
}

// auditedServer is identityServer (enforce mode, who's lookup) with an audit
// log attached, and the log's path.
func auditedServer(t *testing.T, be backend.Backend, who people) (string, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), audit.FileName)
	l, err := audit.Open(path, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	_, addr := identityServerWith(t, be, who.lookup, func(s *Server) { s.SetAuditLog(l) })
	return addr, path
}

// sshLines returns the raw audit log at path and its ssh lines.
func sshLines(t *testing.T, path string) (string, []map[string]any) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	sc := bufio.NewScanner(bytes.NewReader(raw))
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("line %q is not JSON: %v", sc.Text(), err)
		}
		if m["event"] == audit.EventSSH {
			out = append(out, m)
		}
	}
	return string(raw), out
}

// shellOn opens an interactive shell (no command, no PTY) on client and
// returns its exit status.
func shellOn(t *testing.T, client *gossh.Client) int {
	t.Helper()
	sess, err := client.NewSession()
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	defer func() { _ = sess.Close() }()
	sess.Stdout, sess.Stderr = io.Discard, io.Discard
	if err := sess.Shell(); err != nil {
		t.Fatalf("shell: %v", err)
	}
	err = sess.Wait()
	var exit *gossh.ExitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &exit):
		return exit.ExitStatus()
	}
	t.Fatalf("shell: %v", err)
	return -1
}

// tunnelOn opens one forwarded connection to port 8080 on client and
// reports whether it was opened.
func tunnelOn(client *gossh.Client) bool {
	conn, err := client.Dial("tcp", "localhost:8080")
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// enterEveryWay opens a shell, runs a command, starts sftp and opens a
// forwarded connection on client, and returns how many of the four were let
// in. The command carries secret, which no ssh line may hold.
func enterEveryWay(t *testing.T, client *gossh.Client, secret string) int {
	t.Helper()
	in := 0
	if shellOn(t, client) == 0 {
		in++
	}
	if _, status := runOn(t, client, "echo "+secret, ""); status == 0 {
		in++
	}
	if _, status := runOn(t, client, "", "sftp"); status == 0 {
		in++
	}
	if tunnelOn(client) {
		in++
	}
	return in
}

// TestSSHEntryIntoAClientsShedIsAudited: an admin's shell, command, sftp and
// forwarded connection into a service client's shed each write one ssh line:
// the person (kind user, their name, admin true, script false, the key's
// fingerprint, no serial), the peer, the shed and its owner, the channel, and
// the port for the tunnel only. No line holds the command.
func TestSSHEntryIntoAClientsShedIsAudited(t *testing.T) {
	who := newPeople(t)
	addr, path := auditedServer(t, newEntryBackend(), who)
	client := dialWith(t, addr, "ci-box", who.root)
	if in := enterEveryWay(t, client, "s3cret-ssh-marker"); in != 4 {
		t.Fatalf("the admin was let in %d of 4 ways", in)
	}
	// The tunnel's line is written after the channel is accepted, which the
	// client may see first.
	waitFor(t, "four ssh lines", func() bool { _, lines := sshLines(t, path); return len(lines) >= 4 })

	raw, lines := sshLines(t, path)
	if len(lines) != 4 {
		t.Fatalf("want 4 ssh lines, got %d: %s", len(lines), raw)
	}
	if strings.Contains(raw, "s3cret") || strings.Contains(raw, "echo") {
		t.Errorf("the command reached the audit log: %s", raw)
	}
	fp := gossh.FingerprintSHA256(who.root.PublicKey())
	wantID := map[string]any{"kind": "user", "user": "root", "admin": true, "script": false, "subject": fp}
	for i, want := range []string{audit.SSHChannelShell, audit.SSHChannelExec, audit.SSHChannelSFTP, audit.SSHChannelTunnel} {
		l := lines[i]
		if l["channel"] != want || l["shed"] != "ci-box" || l["owner"] != "build-1" {
			t.Errorf("line %d = %v, want channel %s in ci-box of build-1", i, l, want)
		}
		if got, _ := json.Marshal(l["identity"]); string(got) != mustJSON(t, wantID) {
			t.Errorf("line %d identity = %s, want %s", i, got, mustJSON(t, wantID))
		}
		if r, _ := l["remote"].(string); !strings.HasPrefix(r, "127.0.0.1:") {
			t.Errorf("line %d remote = %v", i, l["remote"])
		}
		port, hasPort := l["port"]
		if want == audit.SSHChannelTunnel {
			if port != float64(8080) {
				t.Errorf("tunnel line port = %v, want 8080", port)
			}
		} else if hasPort {
			t.Errorf("line %d (%s) carries a port: %v", i, want, l)
		}
		for k := range l {
			switch k {
			case "ts", "event", "identity", "remote", "shed", "owner", "channel", "port":
			default:
				t.Errorf("line %d carries %q: %v", i, k, l)
			}
		}
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestSSHEntryIntoOwnShedIsNotAudited: a person entering their own shed
// writes no line, however they enter.
func TestSSHEntryIntoOwnShedIsNotAudited(t *testing.T) {
	who := newPeople(t)
	be := newEntryBackend()
	addr, path := auditedServer(t, be, who)
	client := dialWith(t, addr, "box", who.alice)
	if in := enterEveryWay(t, client, "x"); in != 4 {
		t.Fatalf("alice was let into her own shed %d of 4 ways", in)
	}
	// The forward's channel has closed, so its handler has long passed the
	// point where a line would be written.
	select {
	case <-be.dialClosed:
	case <-time.After(10 * time.Second):
		t.Fatal("the forwarded connection was not closed")
	}
	if raw, lines := sshLines(t, path); len(lines) != 0 {
		t.Errorf("own shed: %d ssh lines: %s", len(lines), raw)
	}
}

// TestSSHRefusedEntryIsNotAudited: a user who is not an admin is refused a
// service client's shed every way, and nothing is written.
func TestSSHRefusedEntryIsNotAudited(t *testing.T) {
	who := newPeople(t)
	be := newEntryBackend()
	addr, path := auditedServer(t, be, who)
	client := dialWith(t, addr, "ci-box", who.bob)
	if in := enterEveryWay(t, client, "x"); in != 0 {
		t.Fatalf("bob was let into the client's shed %d of 4 ways", in)
	}
	if execs, dials := be.reached(); execs != 0 || dials != 0 {
		t.Errorf("the backend was reached: %d execs, %d dials", execs, dials)
	}
	if raw, lines := sshLines(t, path); len(lines) != 0 {
		t.Errorf("refused entry: %d ssh lines: %s", len(lines), raw)
	}
}

// TestSSHEntryRefusedAtTheGuardedCallIsNotAudited: the channel's own check
// finds the client's shed, and by the guarded exec or dial it is not that
// shed any more: deleted and created again under its name (a new creation
// time), or reported with another owner. The guard refuses it as a shed that
// is not there, and nothing is written: the line means the entry happened.
func TestSSHEntryRefusedAtTheGuardedCallIsNotAudited(t *testing.T) {
	for _, tc := range []struct {
		name   string
		second func(*config.Shed) *config.Shed
	}{
		{"replaced", func(s *config.Shed) *config.Shed { s.CreatedAt = s.CreatedAt.Add(time.Second); return s }},
		{"another owner", func(s *config.Shed) *config.Shed { s.Owner = "build-2"; return s }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			who := newPeople(t)
			be := newEntryBackend()
			be.second = tc.second
			addr, path := auditedServer(t, be, who)
			client := dialWith(t, addr, "ci-box", who.root)
			if in := enterEveryWay(t, client, "x"); in != 0 {
				t.Fatalf("the admin was let in %d of 4 ways", in)
			}
			if execs, dials := be.reached(); execs != 0 || dials != 0 {
				t.Errorf("the backend was reached: %d execs, %d dials", execs, dials)
			}
			if raw, lines := sshLines(t, path); len(lines) != 0 {
				t.Errorf("%d ssh lines: %s", len(lines), raw)
			}
		})
	}
}

// wakingEntryBackend is entryBackend with "ci-box" not running when first
// checked: a start (or, async, the wait for its job) brings it up, and wake
// changes it then (the creation time stays).
type wakingEntryBackend struct {
	*entryBackend
	mu   sync.Mutex
	shed config.Shed
	wake func(*config.Shed)
}

func (b *wakingEntryBackend) GetShed(_ context.Context, name string) (*config.Shed, error) {
	if name != "ci-box" {
		return nil, config.ErrShedNotFoundSentinel
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	cp := b.shed
	return &cp, nil
}

func (b *wakingEntryBackend) StartShed(_ context.Context, _ string) (*config.Shed, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.shed.Status = config.StatusRunning
	b.wake(&b.shed)
	cp := b.shed
	return &cp, nil
}

// asyncWakingEntryBackend is wakingEntryBackend on an asynchronous backend
// (remote-shaped): the shed is starting, and the wait for its job settles
// it, changed by wake.
type asyncWakingEntryBackend struct{ *wakingEntryBackend }

func (b asyncWakingEntryBackend) WaitSettled(_ context.Context, _ string) (*config.Shed, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.shed.Status = config.StatusRunning
	b.wake(&b.shed)
	cp := b.shed
	return &cp, nil
}

// TestSSHEntryWhoseShedChangedDuringTheWakeIsRefused: the client's shed is
// checked, and while it is woken (a start; or, on an asynchronous backend,
// the wait for its job) its reported ownership changes with its creation
// time unchanged: reassigned to another client, or a client's shed a
// reconcile can no longer resolve (no owner, its user unresolved). The
// ownership the first check found is held through the wake, so the entry is
// refused as one into a shed that is not there: no exec, no line. The admin
// root and the implicit user (who may enter an unresolved shed) are both
// refused. Control: a wake that changes nothing lets the admin in, with one
// line naming build-1.
func TestSSHEntryWhoseShedChangedDuringTheWakeIsRefused(t *testing.T) {
	reassign := func(s *config.Shed) { s.Owner = "build-2" }
	unresolve := func(s *config.Shed) { s.Owner, s.User = "", config.UserUnresolved }
	keep := func(*config.Shed) {}
	stopped := config.Shed{Name: "ci-box", Owner: "build-1", Status: config.StatusStopped, CreatedAt: entryCreated}
	starting := stopped
	starting.Status = config.StatusStarting
	for _, tc := range []struct {
		name     string
		async    bool
		implicit bool
		wake     func(*config.Shed)
		wantIn   int
	}{
		{"sync, unchanged", false, false, keep, 3},
		{"sync, reassigned", false, false, reassign, 0},
		{"sync, unresolved, the implicit user", false, true, unresolve, 0},
		{"async, unchanged", true, false, keep, 3},
		{"async, reassigned", true, false, reassign, 0},
		{"async, unresolved, the implicit user", true, true, unresolve, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			eb := newEntryBackend()
			wb := &wakingEntryBackend{entryBackend: eb, shed: stopped, wake: tc.wake}
			var be backend.Backend = wb
			if tc.async {
				wb.shed = starting
				be = asyncWakingEntryBackend{wb}
			}
			path := filepath.Join(t.TempDir(), audit.FileName)
			l, err := audit.Open(path, 0, 0)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = l.Close() })
			var addr string
			var signer gossh.Signer
			if tc.implicit {
				s, err := NewServer(be, filepath.Join(t.TempDir(), "host_key"), "127.0.0.1:0", nil, KeyAuth{})
				if err != nil {
					t.Fatal(err)
				}
				s.SetAuditLog(l)
				addr, signer = serveForTest(t, s), testSigner(t)
			} else {
				who := newPeople(t)
				_, addr = identityServerWith(t, be, who.lookup, func(s *Server) { s.SetAuditLog(l) })
				signer = who.root
			}
			// Each way in starts from a shed that is not running yet.
			in := 0
			for _, way := range []func(*gossh.Client) bool{
				func(c *gossh.Client) bool { return shellOn(t, c) == 0 },
				func(c *gossh.Client) bool { _, st := runOn(t, c, "true", ""); return st == 0 },
				func(c *gossh.Client) bool { _, st := runOn(t, c, "", "sftp"); return st == 0 },
			} {
				wb.mu.Lock()
				if tc.async {
					wb.shed = starting
				} else {
					wb.shed = stopped
				}
				wb.mu.Unlock()
				if way(dialWith(t, addr, "ci-box", signer)) {
					in++
				}
			}
			if in != tc.wantIn {
				t.Fatalf("let in %d of 3 ways, want %d", in, tc.wantIn)
			}
			if execs, _ := eb.reached(); execs != tc.wantIn {
				t.Errorf("the backend ran %d execs, want %d", execs, tc.wantIn)
			}
			raw, lines := sshLines(t, path)
			if len(lines) != tc.wantIn {
				t.Fatalf("%d ssh lines, want %d: %s", len(lines), tc.wantIn, raw)
			}
			for _, l := range lines {
				if l["owner"] != "build-1" {
					t.Errorf("line names owner %v, want build-1: %v", l["owner"], l)
				}
			}
		})
	}
}

// slowStartBackend is entryBackend whose exec reaches the shed's agent
// (reached is closed) and then waits for release before it reports the
// command started (OnStarted) and returns (done is closed).
type slowStartBackend struct {
	*entryBackend
	reached, release, done chan struct{}
}

func (b *slowStartBackend) Exec(_ context.Context, _ string, opts backend.ExecOptions) error {
	defer close(b.done)
	close(b.reached)
	<-b.release
	if opts.OnStarted != nil {
		opts.OnStarted()
	}
	return nil
}

// TestSSHShutdownKeepsAnAdmittedEntrysLine: an exec into the client's shed
// has been handed to the shed's agent, and its line is not written yet, when
// the server shuts down (its client already gone, so no connection holds the
// shutdown up). Shutdown waits for that line; the audit log, closed after
// Shutdown returns as serve closes it, holds it.
func TestSSHShutdownKeepsAnAdmittedEntrysLine(t *testing.T) {
	who := newPeople(t)
	be := &slowStartBackend{entryBackend: newEntryBackend(), reached: make(chan struct{}), release: make(chan struct{}), done: make(chan struct{})}
	path := filepath.Join(t.TempDir(), audit.FileName)
	l, err := audit.Open(path, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	s, addr := identityServerWith(t, be, who.lookup, func(s *Server) { s.SetAuditLog(l) })

	client := dialWith(t, addr, "ci-box", who.root)
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
	_ = client.Close()

	shut := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		shut <- s.Shutdown(ctx)
	}()
	// The command reports its start only once the shutdown is waiting for
	// its line.
	select {
	case <-s.entries.Waiting():
	case err := <-shut:
		t.Fatalf("Shutdown returned (%v) without waiting for the admitted exec", err)
	case <-time.After(10 * time.Second):
		t.Fatal("Shutdown never waited for the admitted exec")
	}
	close(be.release)
	if err := <-shut; err != nil {
		t.Errorf("Shutdown = %v", err)
	}
	_ = l.Close() // as serve does, once its servers have shut down
	select {
	case <-be.done:
	case <-time.After(10 * time.Second):
		t.Fatal("the exec did not finish")
	}
	if raw, lines := sshLines(t, path); len(lines) != 1 || lines[0]["channel"] != audit.SSHChannelExec {
		t.Fatalf("want the admitted exec's one ssh line, got %s", raw)
	}
}

// TestSSHEntryAfterShutdownBeganIsRefused: once the server's shutdown has
// begun (its entry gate closed, CloseEntries, as shed-server does first
// thing), a new entry into a client's shed is refused before it reaches the
// shed, every way in, and nothing is written; an entry into a person's own
// shed writes no line and is not held back.
func TestSSHEntryAfterShutdownBeganIsRefused(t *testing.T) {
	who := newPeople(t)
	be := newEntryBackend()
	path := filepath.Join(t.TempDir(), audit.FileName)
	l, err := audit.Open(path, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	s, addr := identityServerWith(t, be, who.lookup, func(s *Server) { s.SetAuditLog(l) })
	s.CloseEntries()
	client := dialWith(t, addr, "ci-box", who.root)
	if in := enterEveryWay(t, client, "x"); in != 0 {
		t.Fatalf("let into the client's shed %d of 4 ways after the shutdown began", in)
	}
	if stderr, status := runOn(t, client, "true", ""); status == 0 || !strings.Contains(stderr, "shutting down") {
		t.Errorf("exec: status %d, stderr %q, want the shutdown refusal", status, stderr)
	}
	if execs, dials := be.reached(); execs != 0 || dials != 0 {
		t.Errorf("the backend was reached: %d execs, %d dials", execs, dials)
	}
	if in := enterEveryWay(t, dialWith(t, addr, "box", who.alice), "x"); in != 4 {
		t.Errorf("alice was let into her own shed %d of 4 ways", in)
	}
	if raw, lines := sshLines(t, path); len(lines) != 0 {
		t.Errorf("%d ssh lines: %s", len(lines), raw)
	}
}

// TestSSHFailedEntryIsNotAudited: an exec that fails before its command
// starts, and a dial that fails, write nothing.
func TestSSHFailedEntryIsNotAudited(t *testing.T) {
	who := newPeople(t)
	be := newEntryBackend()
	be.execErr = errors.New("the agent did not answer")
	be.dialErr = errors.New("connection refused")
	addr, path := auditedServer(t, be, who)
	client := dialWith(t, addr, "ci-box", who.root)
	if in := enterEveryWay(t, client, "x"); in != 0 {
		t.Fatalf("the admin was let in %d of 4 ways", in)
	}
	if execs, dials := be.reached(); execs != 3 || dials != 1 {
		t.Fatalf("the backend was reached %d times to exec and %d to dial, want 3 and 1", execs, dials)
	}
	if raw, lines := sshLines(t, path); len(lines) != 0 {
		t.Errorf("%d ssh lines: %s", len(lines), raw)
	}
}

// TestSSHTunnelWhoseChannelIsNotAcceptedIsNotAudited: the guarded dial
// connects, but the client has gone by then, so the forward's channel cannot
// be accepted. The connection is closed and nothing is written.
func TestSSHTunnelWhoseChannelIsNotAcceptedIsNotAudited(t *testing.T) {
	who := newPeople(t)
	be := newEntryBackend()
	dialing := make(chan struct{})
	// The dial connects only once the SSH connection is gone (its context
	// ends when the server has closed it, and every channel on it).
	be.dialWait = func(ctx context.Context) {
		close(dialing)
		<-ctx.Done()
	}
	addr, path := auditedServer(t, be, who)
	client := dialWith(t, addr, "ci-box", who.root)
	opened := make(chan bool, 1)
	go func() { opened <- tunnelOn(client) }()
	<-dialing
	_ = client.Close()
	if <-opened {
		t.Fatal("the forward was opened")
	}
	select {
	case <-be.dialClosed:
	case <-time.After(10 * time.Second):
		t.Fatal("the dialled connection was not closed")
	}
	if raw, lines := sshLines(t, path); len(lines) != 0 {
		t.Errorf("%d ssh lines: %s", len(lines), raw)
	}
}

// TestSSHBootstrapIsNotAudited: _bootstrap enters no shed and writes no line.
func TestSSHBootstrapIsNotAudited(t *testing.T) {
	who := newPeople(t)
	addr, path := auditedServer(t, newEntryBackend(), who)
	_, _, _ = runBootstrap(t, addr, who.root, "")
	if raw, lines := sshLines(t, path); len(lines) != 0 {
		t.Errorf("%d ssh lines: %s", len(lines), raw)
	}
}

// TestSSHEntryWithoutAuditLog: with no audit log attached every way in still
// works, and nothing panics.
func TestSSHEntryWithoutAuditLog(t *testing.T) {
	who := newPeople(t)
	_, addr := identityServer(t, newEntryBackend(), who.lookup)
	client := dialWith(t, addr, "ci-box", who.root)
	if in := enterEveryWay(t, client, "x"); in != 4 {
		t.Fatalf("the admin was let in %d of 4 ways", in)
	}
}

// TestSSHEntryAuditWriteErrorDoesNotFailTheChannel: a line that cannot be
// written (the log is closed) is logged, and the channel goes on.
func TestSSHEntryAuditWriteErrorDoesNotFailTheChannel(t *testing.T) {
	who := newPeople(t)
	l, err := audit.Open(filepath.Join(t.TempDir(), audit.FileName), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	_ = l.Close()
	_, addr := identityServerWith(t, newEntryBackend(), who.lookup, func(s *Server) { s.SetAuditLog(l) })
	client := dialWith(t, addr, "ci-box", who.root)
	if _, status := runOn(t, client, "true", ""); status != 0 {
		t.Errorf("exec status %d with an unwritable audit log", status)
	}
}

// TestSSHEntryByTheImplicitUserIsAudited: on a server with no user list the
// person is the implicit user: implicit true, admin true, no name, and the
// key's fingerprint.
func TestSSHEntryByTheImplicitUserIsAudited(t *testing.T) {
	path := filepath.Join(t.TempDir(), audit.FileName)
	l, err := audit.Open(path, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	s, err := NewServer(newEntryBackend(), filepath.Join(t.TempDir(), "host_key"), "127.0.0.1:0", nil, KeyAuth{})
	if err != nil {
		t.Fatal(err)
	}
	s.SetAuditLog(l)
	addr := serveForTest(t, s)
	signer := testSigner(t)
	client := dialWith(t, addr, "ci-box", signer)
	if _, status := runOn(t, client, "true", ""); status != 0 {
		t.Fatalf("exec status %d", status)
	}
	raw, lines := sshLines(t, path)
	if len(lines) != 1 {
		t.Fatalf("want one ssh line, got %d: %s", len(lines), raw)
	}
	want := map[string]any{"kind": "user", "implicit": true, "admin": true, "script": false, "subject": gossh.FingerprintSHA256(signer.PublicKey())}
	if got, _ := json.Marshal(lines[0]["identity"]); string(got) != mustJSON(t, want) {
		t.Errorf("identity = %s, want %s", got, mustJSON(t, want))
	}
	if lines[0]["channel"] != audit.SSHChannelExec || lines[0]["owner"] != "build-1" {
		t.Errorf("line = %v", lines[0])
	}
}

// TestSSHIdentity: the audit identity of the principal a channel was admitted
// for. A principal that is not a person is unknown, never a person.
func TestSSHIdentity(t *testing.T) {
	script := backend.UserPrincipal("ci", false)
	script.Script = true
	for _, tc := range []struct {
		name string
		p    backend.Principal
		want string
	}{
		{"admin", backend.UserPrincipal("pat", true), `{"kind":"user","user":"pat","admin":true,"script":false,"subject":"SHA256:abc"}`},
		{"user", backend.UserPrincipal("pat", false), `{"kind":"user","user":"pat","admin":false,"script":false,"subject":"SHA256:abc"}`},
		{"script key", script, `{"kind":"user","user":"ci","admin":false,"script":true,"subject":"SHA256:abc"}`},
		{"implicit", backend.ImplicitPrincipal(), `{"kind":"user","implicit":true,"admin":true,"script":false,"subject":"SHA256:abc"}`},
		{"client", backend.ClientPrincipal("build-1"), `{"kind":"unknown"}`},
		{"system", backend.SystemPrincipal(), `{"kind":"unknown"}`},
		{"none", backend.Principal{}, `{"kind":"unknown"}`},
	} {
		if got := mustJSON(t, sshIdentity(tc.p, "SHA256:abc")); got != tc.want {
			t.Errorf("%s: identity = %s, want %s", tc.name, got, tc.want)
		}
	}
}

// TestAuditEntryNeedsAnOwnerAndALog: auditEntry writes only for a shed of a
// service client, and only with a log attached.
func TestAuditEntryNeedsAnOwnerAndALog(t *testing.T) {
	path := filepath.Join(t.TempDir(), audit.FileName)
	l, err := audit.Open(path, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	p := backend.UserPrincipal("pat", true)
	owned := &config.Shed{Name: "ci-box", Owner: "build-1"}
	own := &config.Shed{Name: "box", User: "pat"}

	var s Server
	s.auditEntry(p, "SHA256:abc", nil, owned, audit.SSHChannelExec, 0) // no log: nothing, no panic
	s.SetAuditLog(l)
	s.auditEntry(p, "SHA256:abc", nil, own, audit.SSHChannelExec, 0)
	s.auditEntry(p, "SHA256:abc", nil, nil, audit.SSHChannelExec, 0)
	hook := s.onEntered(p, "SHA256:abc", nil, owned, audit.SSHChannelSFTP)
	hook()
	hook() // once only
	raw, lines := sshLines(t, path)
	if len(lines) != 1 || lines[0]["channel"] != audit.SSHChannelSFTP {
		t.Fatalf("want the one sftp line, got %s", raw)
	}
	if _, ok := lines[0]["remote"]; ok {
		t.Errorf("a line with no peer address carries remote: %v", lines[0])
	}
}
