package vmutil

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// scriptConn is a deterministic in-memory net.Conn. Reads drain a preloaded
// byte script (the "guest" side of the tcpproxy handshake); when drained it
// either returns EOF immediately or, if block is set, blocks until Close so
// ctx-cancel unblocking can be exercised.
type scriptConn struct {
	mu        sync.Mutex
	remaining []byte
	writes    bytes.Buffer
	block     bool
	closed    chan struct{}
	closeOnce sync.Once
	cwCalled  bool
}

func newScriptConn(response string, block bool) *scriptConn {
	return &scriptConn{
		remaining: []byte(response),
		block:     block,
		closed:    make(chan struct{}),
	}
}

func (c *scriptConn) Read(p []byte) (int, error) {
	c.mu.Lock()
	if len(c.remaining) > 0 {
		n := copy(p, c.remaining)
		c.remaining = c.remaining[n:]
		c.mu.Unlock()
		return n, nil
	}
	block := c.block
	c.mu.Unlock()
	if block {
		<-c.closed
	}
	return 0, io.EOF
}

func (c *scriptConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	select {
	case <-c.closed:
		return 0, io.ErrClosedPipe
	default:
	}
	return c.writes.Write(p)
}

func (c *scriptConn) written() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.writes.String()
}

func (c *scriptConn) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return nil
}

func (c *scriptConn) CloseWrite() error {
	c.mu.Lock()
	c.cwCalled = true
	c.mu.Unlock()
	return nil
}

func (c *scriptConn) closeWriteCalled() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cwCalled
}

func (c *scriptConn) LocalAddr() net.Addr                { return fakeAddr{} }
func (c *scriptConn) RemoteAddr() net.Addr               { return fakeAddr{} }
func (c *scriptConn) SetDeadline(t time.Time) error      { return nil }
func (c *scriptConn) SetReadDeadline(t time.Time) error  { return nil }
func (c *scriptConn) SetWriteDeadline(t time.Time) error { return nil }

type fakeAddr struct{}

func (fakeAddr) Network() string { return "fake" }
func (fakeAddr) String() string  { return "fake" }

// fakeDialer returns a preconfigured conn (or error) from Dial, standing in for
// a real backend Dialer. When conn is a *BufferedConn it models the Firecracker
// mux path (Dial already consumed the mux "OK <n>" line into a bufio.Reader).
type fakeDialer struct {
	conn net.Conn
	err  error
}

func (d *fakeDialer) Dial(ctx context.Context, port uint32) (net.Conn, error) {
	if d.err != nil {
		return nil, d.err
	}
	return d.conn, nil
}

// vzDialer wraps a scriptConn as a plain net.Conn — the VZ single-handshake path.
func vzDialer(response string, block bool) (*fakeDialer, *scriptConn) {
	sc := newScriptConn(response, block)
	return &fakeDialer{conn: sc}, sc
}

// fcDialer wraps a scriptConn in a *BufferedConn, modeling the FC mux path. If
// preconsume is non-empty, that prefix is drained from the reader first (as the
// mux handshake in FirecrackerDialer.Dial does) — used for the coalesced case
// where the mux "OK <n>\n" and the tcpproxy "OK\n" arrived in one write and the
// tcpproxy line is left buffered in the mux reader.
func fcDialer(t *testing.T, script string, preconsume string) (*fakeDialer, *scriptConn) {
	t.Helper()
	sc := newScriptConn(script, false)
	br := bufio.NewReader(sc)
	if preconsume != "" {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("preconsume read: %v", err)
		}
		if line != preconsume {
			t.Fatalf("preconsume: got %q want %q", line, preconsume)
		}
	}
	bc := &BufferedConn{Conn: sc, Reader: br}
	return &fakeDialer{conn: bc}, sc
}

func TestDialService(t *testing.T) {
	t.Run("vz_plain_ok", func(t *testing.T) {
		d, sc := vzDialer("OK\n", false)
		conn, err := DialService(context.Background(), d, 1028, 1029)
		if err != nil {
			t.Fatalf("DialService: %v", err)
		}
		defer conn.Close()
		if got := sc.written(); got != "CONNECT 1029\n" {
			t.Errorf("CONNECT sent = %q, want %q", got, "CONNECT 1029\n")
		}
	})

	t.Run("err_propagates", func(t *testing.T) {
		d, _ := vzDialer("ERR dial 127.0.0.1:1029: connection refused\n", false)
		_, err := DialService(context.Background(), d, 1028, 1029)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "connection refused") {
			t.Errorf("error = %q, want it to contain %q", err.Error(), "connection refused")
		}
	})

	t.Run("short_read_eof", func(t *testing.T) {
		d, _ := vzDialer("", false) // no newline, immediate EOF
		_, err := DialService(context.Background(), d, 1028, 1029)
		if err == nil {
			t.Fatal("expected error on short read, got nil")
		}
		if !strings.Contains(err.Error(), "read CONNECT response") {
			t.Errorf("error = %q, want read CONNECT response", err.Error())
		}
	})

	t.Run("dial_error", func(t *testing.T) {
		// Models "UDS not present": the inner Dial fails.
		d := &fakeDialer{err: errors.New("no such file or directory")}
		_, err := DialService(context.Background(), d, 1028, 1029)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "dial tcp proxy") {
			t.Errorf("error = %q, want dial tcp proxy", err.Error())
		}
	})

	t.Run("ctx_cancel_unblocks", func(t *testing.T) {
		d, _ := vzDialer("", true) // Read blocks until Close
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			_, err := DialService(ctx, d, 1028, 1029)
			done <- err
		}()
		// Give the goroutine a moment to reach the blocking read, then cancel.
		time.Sleep(20 * time.Millisecond)
		cancel()
		select {
		case err := <-done:
			if err == nil {
				t.Fatal("expected error after ctx cancel, got nil")
			}
		case <-time.After(2 * time.Second):
			t.Fatal("DialService did not unblock after ctx cancel")
		}
	})

	t.Run("fc_stacked_ok", func(t *testing.T) {
		// FC path, non-coalesced: mux line already consumed in Dial, tcpproxy
		// "OK\n" served on the next read.
		d, sc := fcDialer(t, "OK 0\nOK\n", "OK 0\n")
		conn, err := DialService(context.Background(), d, 1028, 1029)
		if err != nil {
			t.Fatalf("DialService: %v", err)
		}
		defer conn.Close()
		if got := sc.written(); got != "CONNECT 1029\n" {
			t.Errorf("CONNECT sent = %q, want %q", got, "CONNECT 1029\n")
		}
	})

	t.Run("fc_coalesced_no_double_wrap", func(t *testing.T) {
		// The classic nested-buffered-reader bug: mux "OK 0\n" and tcpproxy
		// "OK\n" arrived in one write, so the tcpproxy line is buffered inside
		// the mux reader. The helper MUST reuse that reader. If it double-wraps
		// with a fresh bufio.Reader, the buffered "OK\n" is stranded and the
		// underlying read returns EOF → this test fails.
		d, _ := fcDialer(t, "OK 0\nOK\n", "OK 0\n")
		conn, err := DialService(context.Background(), d, 1028, 1029)
		if err != nil {
			t.Fatalf("DialService (coalesced): %v — helper likely double-wrapped the reader", err)
		}
		defer conn.Close()
	})

	t.Run("fc_close_write_chain", func(t *testing.T) {
		d, sc := fcDialer(t, "OK 0\nOK\n", "OK 0\n")
		conn, err := DialService(context.Background(), d, 1028, 1029)
		if err != nil {
			t.Fatalf("DialService: %v", err)
		}
		// conn is a *BufferedConn wrapping the inner *BufferedConn wrapping sc.
		cw, ok := conn.(interface{ CloseWrite() error })
		if !ok {
			t.Fatal("returned conn does not support CloseWrite")
		}
		if err := cw.CloseWrite(); err != nil { // must not panic and must chain
			t.Fatalf("CloseWrite: %v", err)
		}
		if !sc.closeWriteCalled() {
			t.Error("CloseWrite did not chain to the underlying conn")
		}
		conn.Close()
	})
}

// TestDialServiceDataFlow proves data flows after a successful handshake, using
// a real net.Pipe so blocking reads/writes behave like a live connection.
func TestDialServiceDataFlow(t *testing.T) {
	client, server := net.Pipe()
	d := &fakeDialer{conn: client}

	go func() {
		r := bufio.NewReader(server)
		if _, err := r.ReadString('\n'); err != nil { // CONNECT line
			return
		}
		if _, err := server.Write([]byte("OK\n")); err != nil {
			return
		}
		buf := make([]byte, 5)
		if _, err := io.ReadFull(r, buf); err != nil {
			return
		}
		server.Write(buf) // echo
	}()

	conn, err := DialService(context.Background(), d, 1028, 1029)
	if err != nil {
		t.Fatalf("DialService: %v", err)
	}
	defer conn.Close()

	if _, err := conn.Write([]byte("hello")); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := make([]byte, 5)
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "hello" {
		t.Errorf("echo = %q, want %q", got, "hello")
	}
}

// TestDialServiceConcurrent runs two independent dials in parallel to confirm
// there is no cross-connection serialization in the handshake path.
func TestDialServiceConcurrent(t *testing.T) {
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d, _ := vzDialer("OK\n", false)
			conn, err := DialService(context.Background(), d, 1028, 1029)
			if err != nil {
				t.Errorf("DialService: %v", err)
				return
			}
			conn.Close()
		}()
	}
	wg.Wait()
}
