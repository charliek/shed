// Package conformance is the one test suite every transport.Link must pass.
// It drives a real shed-agent through the link the way the server does: with
// vmutil.AgentClient, vmutil.DialService and vmutil.BidirectionalCopy.
//
// A transport's tests build a Harness and call Run. The same suite runs
// against the local link, against the SSH link through an in-process SSH
// server, against the SSH link through a real OpenSSH sshd in a container,
// and, by hand, against a cloud VM.
//
// The suite needs nothing in the guest but the agent, /bin/sh, cat and sleep:
// its "guest service" for the proxy checks is the agent's own plugin HTTP
// API, which listens on the guest's loopback.
package conformance

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charliek/shed/internal/agentproto"
	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/plugin"
	"github.com/charliek/shed/internal/remote/guest"
	"github.com/charliek/shed/internal/remote/transport"
	"github.com/charliek/shed/internal/vmutil"
)

// Defaults for the Harness's sizes and waits.
const (
	DefaultConcurrentDials = 64
	DefaultLargeStream     = 16 << 20
	DefaultDropDetect      = 10 * time.Second
	DefaultRecover         = 60 * time.Second

	// DefaultAgentHTTPPort is the agent's plugin HTTP port when it is
	// started without --http-port, as it is in a cloud image.
	DefaultAgentHTTPPort = 498

	// unblockWithin is how soon a blocked Read or Write must return after
	// Close. The plan's number.
	unblockWithin = time.Second
	// eofWithin is how soon an end-of-file must cross the link.
	eofWithin = 5 * time.Second
)

// Harness describes one peer and how to reach it.
type Harness struct {
	// Open returns a new link to the peer, not yet connected. Every call
	// returns an independent link to the same agent where the transport
	// allows it (the local link owns its agent, so each of its links has
	// its own). The suite closes what it opens.
	Open func(t *testing.T) transport.Link

	// HTTPPort returns the port of the agent's plugin HTTP API on the
	// guest's loopback, as of now, given the link under test. Nil means
	// DefaultAgentHTTPPort.
	HTTPPort func(link transport.Link) uint16

	// Restart restarts the peer: sshd, the agent, or both. The link must
	// notice and serve a later Dial. Nil skips that check.
	Restart func(t *testing.T, link transport.Link)

	// DropDetect is how long a stream blocked in Read may take to return
	// after Restart. Recover is how long the link may take to serve a
	// Dial again.
	DropDetect time.Duration
	Recover    time.Duration

	// ConcurrentDials is how many streams are held open at once.
	ConcurrentDials int
	// LargeStream is how many bytes the large exec round trip carries.
	LargeStream int
}

func (h Harness) withDefaults() Harness {
	if h.DropDetect <= 0 {
		h.DropDetect = DefaultDropDetect
	}
	if h.Recover <= 0 {
		h.Recover = DefaultRecover
	}
	if h.ConcurrentDials <= 0 {
		h.ConcurrentDials = DefaultConcurrentDials
	}
	if h.LargeStream <= 0 {
		h.LargeStream = DefaultLargeStream
	}
	return h
}

func (h Harness) httpPort(link transport.Link) uint16 {
	if h.HTTPPort == nil {
		return DefaultAgentHTTPPort
	}
	return h.HTTPPort(link)
}

// Run runs the suite. The checks share one link and run in order; the last
// two disturb the peer and the link.
func Run(t *testing.T, h Harness) {
	h = h.withDefaults()
	if h.Open == nil {
		t.Fatal("conformance: Harness.Open is nil")
	}

	link := h.Open(t)
	t.Cleanup(func() { link.Close() })
	client := vmutil.NewAgentClient(link, transport.PortConsole, transport.PortNotify)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	t.Cleanup(cancel)

	// Everything below needs a reachable agent; say so once, clearly.
	if _, err := client.WaitForProbe(ctx, h.Recover); err != nil {
		t.Fatalf("the agent never answered a probe through the link: %v (last dial error: %v)", err, dialError(link))
	}

	s := &suite{h: h, link: link, client: client, ctx: ctx}
	t.Run("probe", s.probe)
	t.Run("exec round trip", s.execRoundTrip)
	t.Run("exec large stream", s.execLargeStream)
	t.Run("exec cancel hangs up the command", s.execCancel)
	t.Run("guest EOF reaches the host", s.guestEOF)
	t.Run("host half-close reaches the guest and the reply returns", s.hostHalfClose)
	t.Run("half-close through BidirectionalCopy", s.halfCloseBridged)
	t.Run("Close unblocks a blocked Read", s.closeUnblocksRead)
	t.Run("Close unblocks a blocked Write", s.closeUnblocksWrite)
	t.Run("deadlines work or are refused", s.deadlines)
	t.Run("concurrent dials", s.concurrentDials)
	t.Run("dial after peer restart", s.dialAfterRestart)
	t.Run("link Close", s.linkClose)
}

type suite struct {
	h      Harness
	link   transport.Link
	client *vmutil.AgentClient
	ctx    context.Context
}

// dialError returns the error of one Dial, for a failure message.
func dialError(link transport.Link) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	conn, err := link.Dial(ctx, transport.PortNotify)
	if err == nil {
		conn.Close()
	}
	return err
}

func (s *suite) probe(t *testing.T) {
	info, err := s.client.Probe(s.ctx)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	t.Logf("agent: version %q protocol %d layout %d", info.Version, info.Protocol, info.Layout)
	if !guest.ProtocolCompatible(info.Protocol) {
		t.Errorf("agent protocol %d is not one this build can drive", info.Protocol)
	}
	if err := s.client.CheckHealth(s.ctx); err != nil {
		t.Errorf("CheckHealth: %v", err)
	}
}

func (s *suite) execRoundTrip(t *testing.T) {
	tests := []struct {
		name       string
		cmd        string
		stdin      string
		wantOut    string
		wantErr    string
		wantStatus int
	}{
		{name: "stdin to stdout and stderr", cmd: "read x; echo out:$x; echo err:$x >&2", stdin: "hi\n", wantOut: "out:hi\n", wantErr: "err:hi\n"},
		{name: "stdin end-of-file", cmd: "cat; echo done", stdin: "a\nb\n", wantOut: "a\nb\ndone\n"},
		{name: "no stdin", cmd: "echo plain", wantOut: "plain\n"},
		{name: "exit status", cmd: "echo before; exit 3", wantOut: "before\n", wantStatus: 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			opts := backend.ExecOptions{
				Cmd:    []string{"/bin/sh", "-c", tt.cmd},
				Stdout: vmutil.NopWriteCloser(&stdout),
				Stderr: vmutil.NopWriteCloser(&stderr),
			}
			if tt.stdin != "" {
				opts.Stdin = io.NopCloser(strings.NewReader(tt.stdin))
			}
			err := s.client.Exec(s.ctx, opts)
			status := 0
			var exitErr *vmutil.ExitError
			switch {
			case errors.As(err, &exitErr):
				status = exitErr.Code
			case err != nil:
				t.Fatalf("Exec: %v", err)
			}
			if status != tt.wantStatus {
				t.Errorf("exit status %d, want %d", status, tt.wantStatus)
			}
			if stdout.String() != tt.wantOut {
				t.Errorf("stdout %q, want %q", stdout.String(), tt.wantOut)
			}
			if stderr.String() != tt.wantErr {
				t.Errorf("stderr %q, want %q", stderr.String(), tt.wantErr)
			}
		})
	}
}

// execLargeStream sends LargeStream bytes through "cat" and back, and checks
// that what returns is what was sent.
func (s *suite) execLargeStream(t *testing.T) {
	size := s.h.LargeStream
	sent := sha256.New()
	got := sha256.New()
	counter := &countingWriter{w: got}

	start := time.Now()
	err := s.client.Exec(s.ctx, backend.ExecOptions{
		Cmd:    []string{"/bin/sh", "-c", "cat"},
		Stdin:  io.NopCloser(io.TeeReader(&patternReader{left: size}, sent)),
		Stdout: vmutil.NopWriteCloser(counter),
	})
	if err != nil {
		t.Fatalf("Exec after %d of %d bytes returned: %v", counter.n, size, err)
	}
	elapsed := time.Since(start)
	if counter.n != size {
		t.Fatalf("%d bytes came back, want %d", counter.n, size)
	}
	if a, b := hex.EncodeToString(sent.Sum(nil)), hex.EncodeToString(got.Sum(nil)); a != b {
		t.Fatalf("the stream was changed in transit: sent sha256 %s, received %s", a, b)
	}
	mib := float64(size) / (1 << 20)
	t.Logf("%.0f MiB each way in %v (%.1f MiB/s each way)", mib, elapsed.Round(time.Millisecond), mib/elapsed.Seconds())
}

// execCancel cancels a running command's context. Exec must return at once,
// and the close must reach the agent, which hangs up the command's process
// group: the command must be gone from the guest soon after.
func (s *suite) execCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(s.ctx)
	defer cancel()

	pidCh := make(chan string, 1)
	out := &lineWriter{first: pidCh}
	done := make(chan error, 1)
	go func() {
		done <- s.client.Exec(ctx, backend.ExecOptions{
			Cmd:    []string{"/bin/sh", "-c", "echo $$; exec sleep 300"},
			Stdout: out,
		})
	}()

	var pid string
	select {
	case pid = <-pidCh:
	case err := <-done:
		t.Fatalf("Exec returned before the command reported its pid: %v", err)
	case <-time.After(15 * time.Second):
		t.Fatal("the command never reported its pid")
	}
	if _, err := strconv.Atoi(pid); err != nil {
		t.Fatalf("pid line %q is not a number", pid)
	}

	cancelled := time.Now()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Exec after cancel: %v, want context.Canceled", err)
		}
		if d := time.Since(cancelled); d > unblockWithin {
			t.Errorf("Exec took %v to return after cancel, want under %v", d, unblockWithin)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Exec did not return within 10 s of its context being cancelled")
	}

	// The process must disappear without anyone asking the guest to kill
	// it: only the closed connection tells the agent.
	deadline := time.Now().Add(15 * time.Second)
	for {
		err := s.client.Exec(s.ctx, backend.ExecOptions{Cmd: []string{"/bin/sh", "-c", "kill -0 " + pid + " 2>/dev/null"}})
		var exitErr *vmutil.ExitError
		if errors.As(err, &exitErr) {
			t.Logf("the guest command was gone %v after the cancel", time.Since(cancelled).Round(10*time.Millisecond))
			return
		}
		if err != nil {
			t.Fatalf("checking for pid %s: %v", pid, err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("pid %s was still running in the guest 15 s after its exec was cancelled", pid)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// dialHTTP opens a proxied connection to the agent's plugin HTTP API.
func (s *suite) dialHTTP(t *testing.T) net.Conn {
	t.Helper()
	port := s.h.httpPort(s.link)
	conn, err := vmutil.DialService(s.ctx, s.link, transport.PortTCPProxy, port)
	if err != nil {
		t.Fatalf("DialService to the agent's HTTP API on guest port %d: %v", port, err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

// guestEOF is the HTTP/1.0 case: the guest service answers and closes, and
// the host, which never closes its own write side, must see end-of-file. It
// fails if the end-of-file arrives only when the host gives up.
func (s *suite) guestEOF(t *testing.T) {
	conn := s.dialHTTP(t)
	if _, err := io.WriteString(conn, "GET /conformance HTTP/1.0\r\n\r\n"); err != nil {
		t.Fatalf("write request: %v", err)
	}
	start := time.Now()
	reply, err := readAllWithin(conn, eofWithin)
	if err != nil {
		t.Fatalf("reading to end-of-file: %v (got %q)", err, reply)
	}
	if !strings.HasPrefix(reply, "HTTP/1.") {
		t.Fatalf("reply %q is not an HTTP response", reply)
	}
	t.Logf("reply %q, end-of-file %v after the request", firstLine(reply), time.Since(start).Round(time.Millisecond))
}

// truncatedPost is a request whose body ends before its Content-Length and
// before its JSON does. The agent's handler blocks reading the body until the
// connection's write side is closed, then answers 400. So a 400 that arrives
// promptly proves two things: the half-close reached the guest service, and
// the other direction stayed open for the reply.
const truncatedPost = "POST /v1/publish HTTP/1.0\r\n" +
	"Content-Type: application/json\r\n" +
	"Content-Length: 4096\r\n\r\n" +
	`{"namespace":"conformance","payload":{"half":`

func (s *suite) hostHalfClose(t *testing.T) {
	conn := s.dialHTTP(t)
	if _, err := io.WriteString(conn, truncatedPost); err != nil {
		t.Fatalf("write request: %v", err)
	}

	// First show the handler is really waiting: with the write side open,
	// no reply comes.
	early := make(chan string, 1)
	go func() {
		buf := make([]byte, 1)
		n, _ := conn.Read(buf)
		early <- string(buf[:n])
	}()
	select {
	case b := <-early:
		t.Fatalf("the guest answered (%q...) before the host closed its write side", b)
	case <-time.After(300 * time.Millisecond):
	}

	cw, ok := conn.(interface{ CloseWrite() error })
	if !ok {
		t.Fatalf("%T has no CloseWrite", conn)
	}
	start := time.Now()
	if err := cw.CloseWrite(); err != nil {
		t.Fatalf("CloseWrite: %v", err)
	}

	var first string
	select {
	case first = <-early:
	case <-time.After(eofWithin):
		t.Fatalf("no reply within %v of CloseWrite: the half-close did not reach the guest service", eofWithin)
	}
	rest, err := readAllWithin(conn, eofWithin)
	if err != nil {
		t.Fatalf("reading the reply to end-of-file: %v", err)
	}
	reply := first + rest
	if !strings.HasPrefix(reply, "HTTP/1.") || !strings.Contains(firstLine(reply), "400") {
		t.Fatalf("reply %q, want a 400", firstLine(reply))
	}
	t.Logf("reply %q, complete %v after CloseWrite", firstLine(reply), time.Since(start).Round(time.Millisecond))
}

// halfCloseBridged repeats the half-close check the way the server's proxy
// paths use a link: a local TCP client on one side, the link on the other and
// vmutil.BidirectionalCopy between them (internal/api/connect.go,
// internal/tunnels).
func (s *suite) halfCloseBridged(t *testing.T) {
	guestConn := s.dialHTTP(t)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	bridged := make(chan struct{})
	go func() {
		defer close(bridged)
		local, err := ln.Accept()
		if err != nil {
			return
		}
		defer local.Close()
		vmutil.BidirectionalCopy(local, guestConn)
	}()

	local, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	if _, err := io.WriteString(local, truncatedPost); err != nil {
		t.Fatal(err)
	}
	if err := local.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	reply, err := readAllWithin(local, eofWithin)
	if err != nil {
		t.Fatalf("reading the reply to end-of-file through the bridge: %v (got %q)", err, reply)
	}
	if !strings.Contains(firstLine(reply), "400") {
		t.Fatalf("reply %q, want a 400", firstLine(reply))
	}
	select {
	case <-bridged:
	case <-time.After(eofWithin):
		t.Fatal("BidirectionalCopy did not finish after both sides reached end-of-file")
	}
}

// closeUnblocksRead blocks a Read on a stream the agent will never write to,
// then closes the stream.
func (s *suite) closeUnblocksRead(t *testing.T) {
	// The notify port waits for a request and says nothing first.
	conn, err := s.link.Dial(s.ctx, transport.PortNotify)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()

	readDone := make(chan error, 1)
	go func() {
		_, err := conn.Read(make([]byte, 16))
		readDone <- err
	}()
	select {
	case err := <-readDone:
		t.Fatalf("Read returned before Close: %v", err)
	case <-time.After(200 * time.Millisecond):
	}

	closed := time.Now()
	if err := conn.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	select {
	case err := <-readDone:
		if err == nil {
			t.Error("Read returned no error after Close")
		}
		t.Logf("Read returned %v after Close: %v", time.Since(closed).Round(time.Microsecond), err)
	case <-time.After(unblockWithin):
		t.Fatalf("Read still blocked %v after Close", unblockWithin)
	}
}

// closeUnblocksWrite fills the path to a guest command that never reads its
// stdin, until Write blocks, then closes the stream. vmutil.AgentClient.Exec
// relies on this to abandon a command whose guest side has stopped reading.
func (s *suite) closeUnblocksWrite(t *testing.T) {
	conn, err := s.link.Dial(s.ctx, transport.PortConsole)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()

	req, err := json.Marshal(agentproto.ExecRequest{Cmd: []string{"/bin/sh", "-c", "exec sleep 20"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := agentproto.WriteMessage(conn, agentproto.MsgTypeExecRequest, req); err != nil {
		t.Fatalf("write exec request: %v", err)
	}

	const limit = 256 << 20
	progress := make(chan int, 1024)
	writeDone := make(chan error, 1)
	go func() {
		frame := make([]byte, 32*1024)
		for sent := 0; sent < limit; sent += len(frame) {
			if err := agentproto.WriteMessage(conn, agentproto.MsgTypeData, frame); err != nil {
				writeDone <- err
				return
			}
			select {
			case progress <- sent + len(frame):
			default:
			}
		}
		writeDone <- nil
	}()

	// Blocked means: no progress for a while.
	sent := 0
	for stalled := false; !stalled; {
		select {
		case n := <-progress:
			sent = n
		case err := <-writeDone:
			t.Fatalf("Write ended after %d bytes without ever blocking: %v", sent, err)
		case <-time.After(750 * time.Millisecond):
			stalled = true
		}
	}
	t.Logf("Write blocked after about %.1f MiB", float64(sent)/(1<<20))

	closed := time.Now()
	conn.Close()
	select {
	case err := <-writeDone:
		if err == nil {
			t.Error("Write returned no error after Close")
		}
		t.Logf("Write returned %v after Close: %v", time.Since(closed).Round(time.Microsecond), err)
	case <-time.After(unblockWithin):
		t.Fatalf("Write still blocked %v after Close", unblockWithin)
	}
}

// deadlines checks the contract's either-or: a stream may refuse deadlines,
// but if it accepts one it must honour it.
func (s *suite) deadlines(t *testing.T) {
	conn, err := s.link.Dial(s.ctx, transport.PortNotify)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()

	if err := conn.SetReadDeadline(time.Now().Add(150 * time.Millisecond)); err != nil {
		t.Logf("deadlines are refused: %v", err)
		return
	}
	start := time.Now()
	readDone := make(chan error, 1)
	go func() {
		_, err := conn.Read(make([]byte, 16))
		readDone <- err
	}()
	select {
	case err := <-readDone:
		var ne net.Error
		if !errors.As(err, &ne) || !ne.Timeout() {
			t.Errorf("Read past its deadline returned %v, want a timeout", err)
		}
		if d := time.Since(start); d < 100*time.Millisecond {
			t.Errorf("Read returned after %v, before its 150 ms deadline", d)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("SetReadDeadline returned nil but Read ignored the deadline")
	}

	// A timeout is not the end of the stream: with the deadline cleared, a
	// health probe on the same stream still gets its answer.
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		t.Fatalf("clearing the deadline: %v", err)
	}
	payload, err := json.Marshal(plugin.HealthRequestPayload{Probe: true})
	if err != nil {
		t.Fatal(err)
	}
	req := plugin.NewEnvelope(plugin.NamespaceHealth, plugin.MessageTypeRequest, payload)
	data, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	if err := agentproto.WriteMessage(conn, agentproto.MsgTypePluginMessage, data); err != nil {
		t.Fatalf("write after a read timeout: %v", err)
	}
	timer := time.AfterFunc(eofWithin, func() { conn.Close() })
	defer timer.Stop()
	_, reply, err := agentproto.ReadMessage(conn)
	if err != nil {
		t.Fatalf("read after a read timeout: %v", err)
	}
	var env plugin.Envelope
	if err := json.Unmarshal(reply, &env); err != nil || env.InReplyTo != req.ID {
		t.Errorf("reply %q does not answer the probe (%v)", reply, err)
	}
}

// concurrentDials holds ConcurrentDials exec streams open at the same moment,
// then lets them all finish. It answers whether the transport limits streams
// per connection (OpenSSH's MaxSessions defaults to 10).
func (s *suite) concurrentDials(t *testing.T) {
	n := s.h.ConcurrentDials
	ctx, cancel := context.WithTimeout(s.ctx, 2*time.Minute)
	defer cancel()

	var started, finished sync.WaitGroup
	release := make(chan struct{})
	outs := make([]bytes.Buffer, n)
	errs := make([]error, n)
	startedOnce := make([]sync.Once, n)

	started.Add(n)
	finished.Add(n)
	for i := range n {
		go func() {
			defer finished.Done()
			markStarted := func() { startedOnce[i].Do(started.Done) }
			defer markStarted() // a failed dial must not hang the test
			pr, pw := io.Pipe()
			go func() {
				<-release
				fmt.Fprintf(pw, "%d\n", i)
				pw.Close()
			}()
			errs[i] = s.client.Exec(ctx, backend.ExecOptions{
				Cmd:       []string{"/bin/sh", "-c", "read x; echo got:$x"},
				Stdin:     pr,
				Stdout:    vmutil.NopWriteCloser(&outs[i]),
				OnStarted: markStarted,
			})
		}()
	}

	allStarted := make(chan struct{})
	go func() { started.Wait(); close(allStarted) }()
	select {
	case <-allStarted:
	case <-time.After(time.Minute):
		close(release)
		t.Fatalf("not all %d streams opened within a minute", n)
	}
	// Every stream is open now, and every command is waiting on stdin.
	close(release)
	finished.Wait()

	failed := 0
	for i := range n {
		want := fmt.Sprintf("got:%d\n", i)
		if errs[i] != nil || outs[i].String() != want {
			failed++
			if failed <= 5 {
				t.Errorf("stream %d: output %q, error %v; want %q", i, outs[i].String(), errs[i], want)
			}
		}
	}
	if failed > 0 {
		t.Fatalf("%d of %d concurrent streams failed", failed, n)
	}
	t.Logf("%d streams were open at once and all completed", n)
}

// dialAfterRestart restarts the peer under an open stream. The stream must
// end, and the link must serve again without being reopened.
func (s *suite) dialAfterRestart(t *testing.T) {
	if s.h.Restart == nil {
		t.Skip("this harness cannot restart its peer")
	}

	conn, err := s.link.Dial(s.ctx, transport.PortNotify)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()
	readDone := make(chan error, 1)
	go func() {
		_, err := conn.Read(make([]byte, 16))
		readDone <- err
	}()

	s.h.Restart(t, s.link)
	restarted := time.Now()

	select {
	case err := <-readDone:
		t.Logf("the open stream ended %v after the restart: %v", time.Since(restarted).Round(time.Millisecond), err)
	case <-time.After(s.h.DropDetect):
		t.Errorf("a stream opened before the restart was still blocked %v after it", s.h.DropDetect)
	}

	// The server's own wait: a probe every 50 ms until one succeeds.
	info, err := s.client.WaitForProbe(s.ctx, s.h.Recover)
	if err != nil {
		t.Fatalf("no probe succeeded within %v of the restart: %v (last dial error: %v)", s.h.Recover, err, dialError(s.link))
	}
	t.Logf("the link served a probe again %v after the restart (protocol %d)", time.Since(restarted).Round(time.Millisecond), info.Protocol)

	var out bytes.Buffer
	if err := s.client.Exec(s.ctx, backend.ExecOptions{
		Cmd:    []string{"/bin/sh", "-c", "echo back"},
		Stdout: vmutil.NopWriteCloser(&out),
	}); err != nil || out.String() != "back\n" {
		t.Errorf("exec after the restart: output %q, error %v", out.String(), err)
	}
}

// linkClose closes a link of its own under an open stream.
func (s *suite) linkClose(t *testing.T) {
	link := s.h.Open(t)
	defer link.Close()
	client := vmutil.NewAgentClient(link, transport.PortConsole, transport.PortNotify)
	if _, err := client.WaitForProbe(s.ctx, s.h.Recover); err != nil {
		t.Fatalf("second link: no probe succeeded: %v", err)
	}

	conn, err := link.Dial(s.ctx, transport.PortNotify)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()
	readDone := make(chan error, 1)
	go func() {
		_, err := conn.Read(make([]byte, 16))
		readDone <- err
	}()
	select {
	case err := <-readDone:
		t.Fatalf("Read returned before the link was closed: %v", err)
	case <-time.After(200 * time.Millisecond):
	}

	if err := link.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	select {
	case err := <-readDone:
		if err == nil {
			t.Error("Read on a stream of a closed link returned no error")
		}
	case <-time.After(unblockWithin):
		t.Errorf("a stream was still blocked in Read %v after its link was closed", unblockWithin)
	}

	if _, err := link.Dial(s.ctx, transport.PortNotify); !errors.Is(err, transport.ErrClosed) {
		t.Errorf("Dial after Close = %v, want transport.ErrClosed", err)
	}
	if err := link.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}

	// The suite's own link is another link and must not have noticed,
	// unless the transport's links own their peer.
	if err := s.client.CheckHealth(s.ctx); err != nil {
		t.Errorf("the first link stopped working when a second link was closed: %v", err)
	}
}

// readAllWithin reads to end-of-file, failing if it does not come in time. It
// uses a timer and Close, not a deadline, so it works on any stream.
func readAllWithin(conn net.Conn, d time.Duration) (string, error) {
	timer := time.AfterFunc(d, func() { _ = conn.Close() })
	data, err := io.ReadAll(conn)
	if !timer.Stop() {
		return string(data), fmt.Errorf("no end-of-file within %v", d)
	}
	return string(data), err
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return strings.TrimSpace(line)
}

// patternReader yields a fixed, position-dependent byte pattern, so a dropped,
// repeated or reordered chunk changes the stream's hash.
type patternReader struct {
	left int
	pos  uint64
}

func (r *patternReader) Read(p []byte) (int, error) {
	if r.left == 0 {
		return 0, io.EOF
	}
	n := min(len(p), r.left)
	for i := range n {
		x := r.pos*0x9E3779B97F4A7C15 + 0x7F4A7C15
		p[i] = byte(x >> 56)
		r.pos++
	}
	r.left -= n
	return n, nil
}

type countingWriter struct {
	w io.Writer
	n int
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += n
	return n, err
}

// lineWriter delivers the first line written to it and discards the rest.
type lineWriter struct {
	mu    sync.Mutex
	buf   []byte
	first chan<- string
	sent  bool
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.sent {
		w.buf = append(w.buf, p...)
		if i := bytes.IndexByte(w.buf, '\n'); i >= 0 {
			w.first <- string(w.buf[:i])
			w.sent = true
		}
	}
	return len(p), nil
}

func (w *lineWriter) Close() error { return nil }
