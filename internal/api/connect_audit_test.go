package api

import (
	"bufio"
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charliek/shed/internal/audit"
	"github.com/charliek/shed/internal/config"
)

// The connect line: a person's tunnel into a service client's shed through
// the HTTPS Connect API, written once the tunnel is established.

var connectCreated = time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)

// connectFakeBackend holds "ci-box", a running shed of the service client
// build-1, and "root-box", root's own running shed. Its dial connects (to a
// loopback socket the test may read), unless told otherwise.
type connectFakeBackend struct {
	routesFakeBackend

	mu    sync.Mutex
	loads int
	// change, when set, changes "ci-box" from its second load on (the
	// guarded dial's, after the handler's own check).
	change func(*config.Shed)
	// dialErr fails a dial; dialWait, when set, runs in a dial before it
	// connects.
	dialErr  error
	dialWait func()
	dials    int
}

func (b *connectFakeBackend) GetShed(_ context.Context, name string) (*config.Shed, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch name {
	case "ci-box":
		b.loads++
		s := &config.Shed{Name: "ci-box", Owner: "build-1", Status: config.StatusRunning, CreatedAt: connectCreated}
		if b.change != nil && b.loads >= 2 {
			b.change(s)
		}
		return s, nil
	case "root-box":
		return &config.Shed{Name: "root-box", User: "root", Status: config.StatusRunning, CreatedAt: connectCreated}, nil
	}
	return nil, fmt.Errorf("%w: %s", config.ErrShedNotFoundSentinel, name)
}

func (b *connectFakeBackend) DialService(_ context.Context, _ string, _ uint16) (net.Conn, error) {
	b.mu.Lock()
	b.dials++
	err, wait := b.dialErr, b.dialWait
	b.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if wait != nil {
		wait()
	}
	return closedPeerConn()
}

// closedPeerConn returns one end of a loopback TCP connection whose other
// end is already closed: a service that hangs up at once, so the tunnel's
// copy ends as soon as the client goes too.
func closedPeerConn() (net.Conn, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	defer ln.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			accepted <- nil
			return
		}
		accepted <- c
	}()
	c, err := net.DialTimeout("tcp", ln.Addr().String(), 5*time.Second)
	if err != nil {
		return nil, err
	}
	if peer := <-accepted; peer != nil {
		_ = peer.Close()
	}
	return c, nil
}

func (b *connectFakeBackend) dialed() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.dials
}

// connectAs serves h on a loopback HTTP server that presents leaf as the
// caller's client certificate, opens GET path there as an upgrade, and
// returns the response status. The tunnel, when opened, is closed at once.
func connectAs(t *testing.T, h http.Handler, leaf *x509.Certificate, path string) int {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		withClientCert(r, leaf)
		withTestMachine(r, leaf.Subject.CommonName)
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	code, err := connectStatus(ts.Listener.Addr().String(), path)
	if err != nil {
		t.Fatal(err)
	}
	return code
}

// connectStatus opens GET path on the HTTP server at addr as an upgrade and
// returns the response status, closing the connection after.
func connectStatus(addr, path string) (int, error) {
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	req := "GET " + path + " HTTP/1.1\r\nHost: x\r\nConnection: Upgrade\r\nUpgrade: shed-tcp\r\n\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		return 0, err
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		return 0, fmt.Errorf("read response: %w", err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode, nil
}

// connectLines returns the connect lines of the audit log at path, and
// every line.
func connectLines(t *testing.T, path string) (connects, all []map[string]any) {
	t.Helper()
	lines, err := readAuditLines(path, auditTestMaxLine)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range lines {
		if l["event"] == audit.EventConnect {
			connects = append(connects, l)
		}
	}
	return connects, lines
}

func rootLeaf(serial int64) *x509.Certificate {
	c := testClientCert(rootFP, "control", time.Hour)
	c.SerialNumber = big.NewInt(serial)
	return c
}

// TestConnectIntoAClientsShedIsAudited: an admin's Connect tunnel into a
// service client's shed writes one connect line once it is established: the
// person as an HTTPS caller (user, admin, script, the certificate's subject
// and serial), the peer, the shed and its owner, and the port. Nothing else
// is written for it, and nothing the caller sent: the request carries an
// X-Request-Id header (which the server's request id takes up), and the line
// has no request_id member and no trace of the header's value.
func TestConnectIntoAClientsShedIsAudited(t *testing.T) {
	b := &connectFakeBackend{}
	s, h := usersTestServer(t, b)
	path := attachTestAudit(t, s)
	const marker = "caller-supplied-marker"
	withRequestID := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Header.Set("X-Request-Id", marker)
		h.ServeHTTP(w, r)
	})
	if code := connectAs(t, withRequestID, rootLeaf(0x1f), "/api/sheds/ci-box/connect/8080"); code != http.StatusSwitchingProtocols {
		t.Fatalf("connect = %d", code)
	}
	waitForLines(t, path, 1)
	connects, all := connectLines(t, path)
	if len(connects) != 1 || len(all) != 1 {
		t.Fatalf("want one line, the connect line; got %v", all)
	}
	l := connects[0]
	wantID := map[string]any{"kind": "user", "user": "root", "admin": true, "script": false, "subject": rootFP, "serial": "1f"}
	if id := l["identity"].(map[string]any); !reflect.DeepEqual(id, wantID) {
		t.Errorf("identity = %v, want %v", id, wantID)
	}
	if l["shed"] != "ci-box" || l["owner"] != "build-1" || l["port"] != float64(8080) {
		t.Errorf("line = %v", l)
	}
	if r, _ := l["remote"].(string); !strings.HasPrefix(r, "127.0.0.1:") {
		t.Errorf("remote = %v", l["remote"])
	}
	for k := range l {
		switch k {
		case "ts", "seq", "event", "identity", "remote", "shed", "owner", "port":
		default:
			t.Errorf("the line carries %q: %v", k, l)
		}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), marker) {
		t.Errorf("the caller's X-Request-Id reached the audit log: %s", raw)
	}
	assertNoAdminKind(t, path)
}

// waitForLines waits until the audit log at path has at least n lines: the
// connect line is written by the handler after it switched protocols, which
// the client may see first.
func waitForLines(t *testing.T, path string, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		lines, err := readAuditLines(path, auditTestMaxLine)
		if err == nil && len(lines) >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the audit log did not reach %d lines: %v %v", n, lines, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestConnectNotAudited: no connect line for a person's Connect into their
// own shed (established), a refused one (a user who is not an admin: not
// found, never dialled), one whose dial fails, or one whose shed changed
// between the check and the dial (another owner, or no owner: the guarded
// dial refuses it as not found, never dialling). A tunnel into the client's
// shed after them still writes its line, so the log was writable throughout.
func TestConnectNotAudited(t *testing.T) {
	for _, tc := range []struct {
		name     string
		leaf     *x509.Certificate
		shed     string
		setup    func(*connectFakeBackend)
		wantCode int
		wantDial bool
	}{
		{"own shed", rootLeaf(1), "root-box", nil, http.StatusSwitchingProtocols, true},
		{"a user who is not an admin", testClientCert(aliceFP, "control", time.Hour), "ci-box", nil, http.StatusNotFound, false},
		{"the dial fails", rootLeaf(1), "ci-box", func(b *connectFakeBackend) { b.dialErr = errors.New("connection refused") }, http.StatusBadGateway, true},
		{"another owner by the dial", rootLeaf(1), "ci-box", func(b *connectFakeBackend) { b.change = func(s *config.Shed) { s.Owner = "build-2" } }, http.StatusNotFound, false},
		{"no owner by the dial", rootLeaf(1), "ci-box", func(b *connectFakeBackend) {
			b.change = func(s *config.Shed) { s.Owner, s.User = "", config.UserUnresolved }
		}, http.StatusNotFound, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := &connectFakeBackend{}
			if tc.setup != nil {
				tc.setup(b)
			}
			s, h := usersTestServer(t, b)
			path := attachTestAudit(t, s)
			if code := connectAs(t, h, tc.leaf, "/api/sheds/"+tc.shed+"/connect/8080"); code != tc.wantCode {
				t.Fatalf("connect = %d, want %d", code, tc.wantCode)
			}
			if dialed := b.dialed() > 0; dialed != tc.wantDial {
				t.Errorf("dialled = %v, want %v", dialed, tc.wantDial)
			}
			// The control: the client's shed, unchanged, with a dial that
			// works, writes its line.
			b.mu.Lock()
			b.change, b.dialErr, b.loads = nil, nil, 0
			b.mu.Unlock()
			if code := connectAs(t, h, rootLeaf(2), "/api/sheds/ci-box/connect/8080"); code != http.StatusSwitchingProtocols {
				t.Fatalf("control connect = %d", code)
			}
			waitForLines(t, path, 1)
			connects, all := connectLines(t, path)
			if len(connects) != 1 || len(all) != 1 || connects[0]["identity"].(map[string]any)["serial"] != "2" {
				t.Errorf("want only the control's connect line, got %v", all)
			}
		})
	}
}

// TestConnectWithoutAuditLog: with no audit log a person's Connect into a
// client's shed works as before, and nothing panics.
func TestConnectWithoutAuditLog(t *testing.T) {
	b := &connectFakeBackend{}
	_, h := usersTestServer(t, b)
	if code := connectAs(t, h, rootLeaf(1), "/api/sheds/ci-box/connect/8080"); code != http.StatusSwitchingProtocols {
		t.Fatalf("connect = %d", code)
	}
}

// TestConnectDrainKeepsAnAdmittedTunnelsLine: a Connect into the client's
// shed is dialling when the server shuts down. DrainEntries waits until its
// tunnel is established and its line written, so the audit log, closed once
// DrainEntries returns, holds it; a Connect after that is refused (503) before
// anything is dialled, and writes nothing.
func TestConnectDrainKeepsAnAdmittedTunnelsLine(t *testing.T) {
	reached, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	b := &connectFakeBackend{dialWait: func() {
		once.Do(func() { close(reached) })
		<-release
	}}
	s, h := usersTestServer(t, b)
	path := attachTestAudit(t, s)
	leaf := rootLeaf(1)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		withClientCert(r, leaf)
		withTestMachine(r, leaf.Subject.CommonName)
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	// On a failure the dial is released too, before the test server's
	// Close waits for its connection (cleanups run last-in first-out).
	var releaseOnce sync.Once
	releaseDial := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseDial)

	opened := make(chan int, 1)
	go func() {
		code, err := connectStatus(ts.Listener.Addr().String(), "/api/sheds/ci-box/connect/8080")
		if err != nil {
			code = -1
		}
		opened <- code
	}()
	select {
	case <-reached:
	case <-time.After(10 * time.Second):
		t.Fatal("the dial was not reached")
	}
	drained := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		drained <- s.DrainEntries(ctx)
	}()
	// The tunnel is established only once the drain is waiting for it.
	select {
	case <-s.connects.Waiting():
	case err := <-drained:
		t.Fatalf("DrainEntries returned (%v) without waiting for the dialling Connect", err)
	case <-time.After(10 * time.Second):
		t.Fatal("DrainEntries never waited for the dialling Connect")
	}
	releaseDial()
	if err := <-drained; err != nil {
		t.Fatalf("DrainEntries = %v", err)
	}
	_ = s.audit.Close() // as serve does once its servers have drained
	if code := <-opened; code != http.StatusSwitchingProtocols {
		t.Fatalf("connect = %d", code)
	}
	connects, all := connectLines(t, path)
	if len(connects) != 1 || len(all) != 1 {
		t.Fatalf("want the admitted tunnel's one connect line, got %v", all)
	}

	dials := b.dialed()
	if code := connectAs(t, h, rootLeaf(2), "/api/sheds/ci-box/connect/8080"); code != http.StatusServiceUnavailable {
		t.Errorf("a Connect after the drain = %d, want 503", code)
	}
	if b.dialed() != dials {
		t.Error("a Connect after the drain was dialled")
	}
}
