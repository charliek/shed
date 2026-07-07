package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// shortSocketPath returns a Unix-domain socket path under a short root.
// t.TempDir() lives under $TMPDIR, which on macOS CI runners is long enough
// (~49 bytes) that the longer test names push the full path past the macOS
// sun_path cap (104 bytes) — net.Listen("unix", …) then fails to bind and the
// socket never appears. A short, test-name-independent /tmp root keeps every
// case well under the cap on both macOS and Linux.
func shortSocketPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(shortDir(t), "a.sock")
}

// shortDir is a short, test-name-independent /tmp directory for binding Unix
// sockets in tests — same sun_path-cap rationale as shortSocketPath.
func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "shed-ds")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func startTestServer(t *testing.T, timeoutMS int) (*DesktopServer, *AuditLogger, context.CancelFunc, string) {
	t.Helper()
	sock := shortSocketPath(t)
	audit := NewAuditLogger(LogConfig{Enabled: false}, testLogger())
	timeout := time.Duration(timeoutMS) * time.Millisecond
	s := NewDesktopServer(sock, timeout, audit, "test", []string{"ssh-agent", "aws-credentials"}, testLogger())
	ctx, cancel := context.WithCancel(context.Background())
	go s.Listen(ctx)
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(sock); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("server socket never appeared")
		}
		time.Sleep(5 * time.Millisecond)
	}
	return s, audit, cancel, sock
}

// dialHello connects, sends hello, consumes the hello_ack, then waits until
// the server has promoted this connection to the active consumer. The wait
// matters because the server now sends hello_ack BEFORE promoting, so a test
// that drives an approval the instant dialHello returns could otherwise race
// ahead of promotion.
func dialHello(t *testing.T, s *DesktopServer, sock string) (net.Conn, *bufio.Reader) {
	t.Helper()
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if _, err := conn.Write([]byte(`{"type":"hello","client":{"name":"t","version":"1","pid":1},"capabilities":[],"replay_events":0}` + "\n")); err != nil {
		t.Fatalf("write hello: %v", err)
	}
	r := bufio.NewReader(conn)
	frame := readType(t, conn, r, "hello_ack")
	if accepted, _ := frame["accepted"].(bool); !accepted {
		t.Fatalf("hello_ack not accepted: %v", frame)
	}
	deadline := time.Now().Add(2 * time.Second)
	for !s.hasConsumer() {
		if time.Now().After(deadline) {
			t.Fatal("server never promoted the consumer")
		}
		time.Sleep(2 * time.Millisecond)
	}
	return conn, r
}

// readType reads frames until one with the wanted type (skipping ping etc).
// A read deadline keeps a missing frame from hanging the whole test run.
func readType(t *testing.T, conn net.Conn, r *bufio.Reader, want string) map[string]any {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	defer func() { _ = conn.SetReadDeadline(time.Time{}) }()
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			t.Fatalf("read %s: %v", want, err)
		}
		var m map[string]any
		if json.Unmarshal(line, &m) != nil {
			continue
		}
		if m["type"] == want {
			return m
		}
	}
}

func TestPrepareSocketPath(t *testing.T) {
	dir := shortDir(t) // bind under a short /tmp root (sun_path 104-byte cap)

	// Missing path → no-op.
	if err := prepareSocketPath(filepath.Join(dir, "nope.sock")); err != nil {
		t.Fatalf("missing path: %v", err)
	}

	// A non-socket file is refused and left intact (never delete an unrelated file).
	reg := filepath.Join(dir, "file")
	if err := os.WriteFile(reg, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := prepareSocketPath(reg); err == nil {
		t.Fatal("expected refusal for a non-socket file")
	}
	if _, err := os.Stat(reg); err != nil {
		t.Fatalf("non-socket file must be left intact: %v", err)
	}

	// A stale socket (file present, nothing accepting) is removed so we can bind.
	stale := filepath.Join(dir, "stale.sock")
	sln, err := net.ListenUnix("unix", &net.UnixAddr{Name: stale, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	sln.SetUnlinkOnClose(false) // leave the file behind → genuinely stale
	_ = sln.Close()
	if _, err := os.Stat(stale); err != nil {
		t.Fatalf("precondition: stale socket file should exist: %v", err)
	}
	if err := prepareSocketPath(stale); err != nil {
		t.Fatalf("stale socket should be removable: %v", err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("stale socket should have been removed")
	}

	// A LIVE socket (another agent accepting) is refused and left intact — the
	// regression: clobbering it would orphan the running agent's listener.
	live := filepath.Join(dir, "live.sock")
	lln, err := net.Listen("unix", live)
	if err != nil {
		t.Fatal(err)
	}
	defer lln.Close()
	if err := prepareSocketPath(live); err == nil {
		t.Fatal("expected refusal for a live socket")
	}
	if _, err := os.Stat(live); err != nil {
		t.Fatalf("live socket must be left intact: %v", err)
	}
}

func TestDesktopNoConsumerFailsClosed(t *testing.T) {
	s, _, cancel, _ := startTestServer(t, 1000)
	defer cancel()
	g := &desktopGate{server: s, namespace: "ssh-agent", op: "sign"}
	if _, err := g.Approve("srv", "stbot", "ssh-ed25519"); err == nil {
		t.Fatal("expected deny (error) with no consumer connected")
	}
}

func TestDesktopApprove(t *testing.T) {
	s, _, cancel, sock := startTestServer(t, 5000)
	defer cancel()
	conn, r := dialHello(t, s, sock)
	defer conn.Close()

	// The connected consumer's self-reported identity (from the hello) is what
	// `status` surfaces in the approval-channel line.
	if connected, client := s.ConsumerInfo(); !connected || client.Name != "t" || client.Version != "1" {
		t.Fatalf("ConsumerInfo() = %v, %+v; want connected with name=t version=1", connected, client)
	}

	done := make(chan error, 1)
	go func() {
		g := &desktopGate{server: s, namespace: "ssh-agent", op: "sign"}
		_, err := g.Approve("srv", "stbot", "ssh-ed25519")
		done <- err
	}()

	req := readType(t, conn, r, "approval_request")
	if req["namespace"] != "ssh-agent" || req["shed"] != "stbot" {
		t.Fatalf("unexpected request: %v", req)
	}
	respond(t, conn, req["id"].(string), "approve")
	if err := <-done; err != nil {
		t.Fatalf("approve should succeed, got %v", err)
	}
}

func TestDesktopDeny(t *testing.T) {
	s, _, cancel, sock := startTestServer(t, 5000)
	defer cancel()
	conn, r := dialHello(t, s, sock)
	defer conn.Close()

	type res struct {
		out ApprovalOutcome
		err error
	}
	done := make(chan res, 1)
	go func() {
		g := &desktopGate{server: s, namespace: "ssh-agent", op: "sign"}
		out, err := g.Approve("srv", "stbot", "x")
		done <- res{out, err}
	}()
	req := readType(t, conn, r, "approval_request")
	respond(t, conn, req["id"].(string), "deny")
	got := <-done
	if got.err == nil {
		t.Fatal("deny should return an error")
	}
	// A denied decision still carries decided_by so the denial is fully audited.
	if got.out.DecidedBy != "user" {
		t.Errorf("denied outcome decided_by = %q, want user", got.out.DecidedBy)
	}
}

func TestDesktopTimeoutFailsClosed(t *testing.T) {
	s, _, cancel, sock := startTestServer(t, 150)
	defer cancel()
	conn, r := dialHello(t, s, sock)
	defer conn.Close()

	done := make(chan error, 1)
	go func() {
		g := &desktopGate{server: s, namespace: "ssh-agent", op: "sign"}
		_, err := g.Approve("srv", "stbot", "x")
		done <- err
	}()
	readType(t, conn, r, "approval_request") // received, but we never respond
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("timeout should deny")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Approve did not time out")
	}
}

func TestDesktopAuditFanoutAllNamespaces(t *testing.T) {
	s, audit, cancel, sock := startTestServer(t, 1000)
	defer cancel()
	conn, r := dialHello(t, s, sock)
	defer conn.Close()

	// An aws event (a namespace that does NOT gate) must still reach the app.
	audit.Log("mini2", "roost-dev", "aws-credentials", "get_credentials", "ok", "role/dev", "none")
	ev := readType(t, conn, r, "event")
	if ev["ns"] != "aws-credentials" || ev["server"] != "mini2" || ev["shed"] != "roost-dev" || ev["result"] != "ok" {
		t.Fatalf("unexpected event: %v", ev)
	}
}

// TestDesktopAuditForwardsCodeReason pins that a failed entry's code+reason
// reach the app on the event frame — not just the durable log file. The desktop
// eventMsg is built by hand from the AuditEntry, so a new AuditEntry field is
// silently dropped from the app's feed unless the frame is updated too.
func TestDesktopAuditForwardsCodeReason(t *testing.T) {
	s, audit, cancel, sock := startTestServer(t, 1000)
	defer cancel()
	conn, r := dialHello(t, s, sock)
	defer conn.Close()

	audit.LogEntry(AuditEntry{
		Server: "localmac", Shed: "t1", Namespace: "docker-credentials", Operation: "get",
		Result: "error", Detail: "quay.io",
		Code: "REGISTRY_NOT_ALLOWED", Reason: `registry "quay.io" not in allowlist`,
		Approval: "approve-all",
	})
	ev := readType(t, conn, r, "event")
	if ev["code"] != "REGISTRY_NOT_ALLOWED" {
		t.Errorf("event code: got %v, want REGISTRY_NOT_ALLOWED", ev["code"])
	}
	if ev["reason"] != `registry "quay.io" not in allowlist` {
		t.Errorf("event reason: got %v, want the allowlist reason", ev["reason"])
	}
}

func TestDesktopLastWriterWins(t *testing.T) {
	s, _, cancel, sock := startTestServer(t, 5000)
	defer cancel()
	c1, r1 := dialHello(t, s, sock)
	defer c1.Close()
	// Second consumer supersedes the first.
	c2, r2 := dialHello(t, s, sock)
	defer c2.Close()
	// c1 must receive a superseded hello_ack (accepted:false).
	ack := readType(t, c1, r1, "hello_ack")
	if accepted, _ := ack["accepted"].(bool); accepted {
		t.Fatalf("first consumer should be superseded, got ack=%v", ack)
	}

	done := make(chan error, 1)
	go func() {
		g := &desktopGate{server: s, namespace: "ssh-agent", op: "sign"}
		_, err := g.Approve("srv", "stbot", "x")
		done <- err
	}()
	req := readType(t, c2, r2, "approval_request") // goes to the active (second) consumer
	respond(t, c2, req["id"].(string), "approve")
	if err := <-done; err != nil {
		t.Fatalf("active consumer approve should succeed, got %v", err)
	}
}

func respond(t *testing.T, conn net.Conn, requestID, decision string) {
	t.Helper()
	msg, _ := json.Marshal(map[string]any{"type": "approval_response", "request_id": requestID, "decision": decision, "decided_by": "user"})
	if _, err := conn.Write(append(msg, '\n')); err != nil {
		t.Fatalf("respond: %v", err)
	}
}

func TestApprovalTimeoutDefault(t *testing.T) {
	if got := DefaultConfig().ApprovalTimeout; got != "25s" {
		t.Errorf("default approval_timeout = %q, want 25s", got)
	}
}

func TestGateForMisconfigDenies(t *testing.T) {
	cfg := DefaultConfig()
	cfg.SSH.Approval.Policy = PolicyShedDesktop
	// nil approval channel → fail-closed deny gate (defensive; the daemon always
	// constructs one).
	g := gateFor("ssh-agent", "sign", cfg.SSH.Approval, nil)
	if _, err := g.Approve("srv", "stbot", "x"); err == nil {
		t.Fatal("shed-desktop policy with no channel should deny")
	}
}

// hello_ack advertises gate_namespaces from the per-provider policies so the app
// shows the matching approval UI.
func TestDesktopHelloAckGateNamespaces(t *testing.T) {
	_, _, cancel, sock := startTestServer(t, 1000)
	defer cancel()
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte(`{"type":"hello","client":{"name":"t","version":"1","pid":1},"capabilities":[],"replay_events":0}` + "\n")); err != nil {
		t.Fatalf("write hello: %v", err)
	}
	r := bufio.NewReader(conn)
	ack := readType(t, conn, r, "hello_ack")
	gn, _ := ack["gate_namespaces"].([]any)
	if len(gn) != 2 || gn[0] != "ssh-agent" || gn[1] != "aws-credentials" {
		t.Fatalf("gate_namespaces = %v, want [ssh-agent aws-credentials]", ack["gate_namespaces"])
	}
}

// A shed-desktop decision's scope/ttl/decided_by reach the durable audit log.
func TestDesktopDecisionDetailAudited(t *testing.T) {
	s, _, cancel, sock := startTestServer(t, 5000)
	defer cancel()
	conn, r := dialHello(t, s, sock)
	defer conn.Close()

	type res struct {
		out ApprovalOutcome
		err error
	}
	done := make(chan res, 1)
	go func() {
		g := &desktopGate{server: s, namespace: "ssh-agent", op: "sign"}
		out, err := g.Approve("srv", "stbot", "x")
		done <- res{out, err}
	}()
	req := readType(t, conn, r, "approval_request")
	msg, _ := json.Marshal(map[string]any{
		"type": "approval_response", "request_id": req["id"].(string),
		"decision": "approve", "decided_by": "user", "scope": "per-session", "ttl": "4h",
	})
	if _, err := conn.Write(append(msg, '\n')); err != nil {
		t.Fatalf("respond: %v", err)
	}
	got := <-done
	if got.err != nil {
		t.Fatalf("approve should succeed: %v", got.err)
	}
	if got.out.DecidedBy != "user" || got.out.Scope != "per-session" || got.out.TTL != "4h" {
		t.Fatalf("outcome = %+v, want decided_by=user scope=per-session ttl=4h", got.out)
	}
}

func TestDesktopTokenGet(t *testing.T) {
	cfg := writeShedConfig(t, "\nservers:\n  prod:\n    api_url: https://prod.example:8443\n    host: prod.example\n    ssh_port: 2222\n")
	far := time.Now().Add(24 * time.Hour)
	fm := &fakeMinter{results: []mintResult{{tok: "ctl-xyz", exp: far}}}

	sock := shortSocketPath(t)
	audit := NewAuditLogger(LogConfig{Enabled: false}, testLogger())
	s := NewDesktopServer(sock, 100*time.Millisecond, audit, "test", nil, testLogger())
	s.SetControlTokens(newControlTokenProvider(context.Background(), fm, cfg)) // before Listen
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Listen(ctx)
	waitForSocket(t, sock)

	conn, r := dialHello(t, s, sock)
	defer conn.Close()

	if _, err := conn.Write([]byte(`{"type":"token.get","id":"req-1","server":"prod"}` + "\n")); err != nil {
		t.Fatalf("write token.get: %v", err)
	}
	resp := readType(t, conn, r, "token.response")
	if resp["in_reply_to"] != "req-1" {
		t.Errorf("in_reply_to = %v, want req-1", resp["in_reply_to"])
	}
	if resp["server"] != "prod" {
		t.Errorf("server = %v, want prod", resp["server"])
	}
	if resp["token"] != "ctl-xyz" {
		t.Errorf("token = %v, want ctl-xyz", resp["token"])
	}
	if e, _ := resp["error"].(string); e != "" {
		t.Errorf("unexpected error: %q", e)
	}
}
