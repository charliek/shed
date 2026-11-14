package sshlink

import (
	"bytes"
	"errors"
	"io"
	"net"
	"os"
	"sync"
	"testing"
	"time"
)

// fakeChannel is an SSH channel as x/crypto presents one, with the properties
// stream exists to hide: Close does not unblock Read or Write, deadlines are
// refused, and concurrent Write and CloseWrite would be a data race (the
// race detector checks that, because the fake touches unguarded state the
// way the real channel does).
type fakeChannel struct {
	in       chan []byte   // data for Read; closing it is end-of-file
	gate     chan struct{} // Write blocks until it can receive from gate
	released chan struct{} // closed to let every blocked call return, as a dead connection does

	mu     sync.Mutex
	ops    []string // "write:<n>", "eof", "close"
	wrote  bytes.Buffer
	closed int

	unguarded int // written by Write and CloseWrite without a lock, on purpose
}

func newFakeChannel() *fakeChannel {
	return &fakeChannel{
		in:       make(chan []byte, 16),
		gate:     make(chan struct{}),
		released: make(chan struct{}),
	}
}

// open lets writes through without blocking.
func (f *fakeChannel) open() { close(f.gate) }

func (f *fakeChannel) Read(p []byte) (int, error) {
	select {
	case data, ok := <-f.in:
		if !ok {
			return 0, io.EOF
		}
		return copy(p, data), nil
	case <-f.released:
		return 0, io.EOF
	}
}

func (f *fakeChannel) Write(p []byte) (int, error) {
	f.unguarded++
	select {
	case <-f.gate:
	case <-f.released:
		return 0, io.EOF
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ops = append(f.ops, "write")
	f.wrote.Write(p)
	return len(p), nil
}

func (f *fakeChannel) CloseWrite() error {
	f.unguarded++
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ops = append(f.ops, "eof")
	return nil
}

func (f *fakeChannel) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed++
	return nil
}

func (f *fakeChannel) LocalAddr() net.Addr              { return &net.UnixAddr{Name: "@", Net: "unix"} }
func (f *fakeChannel) RemoteAddr() net.Addr             { return &net.UnixAddr{Name: "/sock", Net: "unix"} }
func (f *fakeChannel) SetDeadline(time.Time) error      { return errors.New("deadline not supported") }
func (f *fakeChannel) SetReadDeadline(time.Time) error  { return errors.New("deadline not supported") }
func (f *fakeChannel) SetWriteDeadline(time.Time) error { return errors.New("deadline not supported") }

func (f *fakeChannel) written() ([]byte, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]byte(nil), f.wrote.Bytes()...), append([]string(nil), f.ops...)
}

func newTestStream(t *testing.T) (*stream, *fakeChannel, *conn) {
	t.Helper()
	ch := newFakeChannel()
	c := &conn{tcp: &trackedConn{}, dead: make(chan struct{})}
	s := newStream(ch, c)
	t.Cleanup(func() {
		s.Close()
		close(ch.released)
	})
	return s, ch, c
}

// within fails the test if fn has not returned in time.
func within(t *testing.T, d time.Duration, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); fn() }()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("%s: still blocked after %v", what, d)
	}
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout() && errors.Is(err, os.ErrDeadlineExceeded)
}

func TestStreamCloseUnblocksReadAndWrite(t *testing.T) {
	s, ch, _ := newTestStream(t)

	readErr := make(chan error, 1)
	writeErr := make(chan error, 1)
	go func() { _, err := s.Read(make([]byte, 8)); readErr <- err }()
	go func() { _, err := s.Write([]byte("stuck")); writeErr <- err }()
	time.Sleep(50 * time.Millisecond) // both are inside the channel now

	within(t, time.Second, "Close", func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	for name, errCh := range map[string]chan error{"Read": readErr, "Write": writeErr} {
		select {
		case err := <-errCh:
			if !errors.Is(err, net.ErrClosed) {
				t.Errorf("%s after Close = %v, want net.ErrClosed", name, err)
			}
		case <-time.After(time.Second):
			t.Fatalf("%s still blocked after Close", name)
		}
	}

	// Closed means closed, however often it is said.
	if err := s.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
	if ch.closed != 1 {
		t.Errorf("the channel was closed %d times, want 1", ch.closed)
	}
	tests := []struct {
		name string
		err  error
	}{
		{"Read", func() error { _, err := s.Read(make([]byte, 1)); return err }()},
		{"Write", func() error { _, err := s.Write([]byte("x")); return err }()},
		{"CloseWrite", s.CloseWrite()},
		{"SetDeadline", s.SetDeadline(time.Now())},
		{"SetReadDeadline", s.SetReadDeadline(time.Now())},
		{"SetWriteDeadline", s.SetWriteDeadline(time.Now())},
	}
	for _, tt := range tests {
		t.Run(tt.name+" after Close", func(t *testing.T) {
			if !errors.Is(tt.err, net.ErrClosed) {
				t.Errorf("got %v, want net.ErrClosed", tt.err)
			}
		})
	}
}

func TestStreamReadDeadline(t *testing.T) {
	s, ch, _ := newTestStream(t)

	if err := s.SetReadDeadline(time.Now().Add(50 * time.Millisecond)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	start := time.Now()
	_, err := s.Read(make([]byte, 8))
	if !isTimeout(err) {
		t.Fatalf("Read = %v, want a timeout", err)
	}
	if d := time.Since(start); d < 40*time.Millisecond || d > time.Second {
		t.Errorf("Read timed out after %v, want about 50 ms", d)
	}

	// A deadline in the past fails at once, even with data waiting.
	ch.in <- []byte("late")
	time.Sleep(20 * time.Millisecond)
	if _, err := s.Read(make([]byte, 8)); !isTimeout(err) {
		t.Errorf("Read past its deadline = %v, want a timeout", err)
	}

	// Nothing was lost to the timeouts: clear the deadline and the bytes
	// that arrived meanwhile are read, in order.
	if err := s.SetReadDeadline(time.Time{}); err != nil {
		t.Fatal(err)
	}
	ch.in <- []byte("r")
	got := make([]byte, 5)
	if _, err := io.ReadFull(s, got); err != nil || string(got) != "later" {
		t.Errorf("after clearing the deadline read %q, %v; want \"later\"", got, err)
	}

	// A deadline set while a Read is blocked still ends it.
	readErr := make(chan error, 1)
	go func() { _, err := s.Read(make([]byte, 8)); readErr <- err }()
	time.Sleep(30 * time.Millisecond)
	if err := s.SetDeadline(time.Now().Add(30 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-readErr:
		if !isTimeout(err) {
			t.Errorf("blocked Read = %v, want a timeout", err)
		}
	case <-time.After(time.Second):
		t.Fatal("a deadline set during a Read did not end it")
	}
}

func TestStreamShortReadsKeepTheRest(t *testing.T) {
	s, ch, _ := newTestStream(t)
	ch.in <- []byte("abcdefgh")
	close(ch.in)

	var got []byte
	buf := make([]byte, 3)
	for {
		n, err := s.Read(buf)
		got = append(got, buf[:n]...)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
	}
	if string(got) != "abcdefgh" {
		t.Errorf("read %q through a 3-byte buffer", got)
	}
	// End-of-file is sticky.
	if _, err := s.Read(buf); err != io.EOF {
		t.Errorf("Read after end-of-file = %v", err)
	}
	if n, err := s.Read(nil); n != 0 || err != io.EOF {
		t.Errorf("zero-length Read after end-of-file = %d, %v", n, err)
	}
}

func TestStreamEOFOnALostConnection(t *testing.T) {
	tests := []struct {
		name string
		lose func(*conn)
	}{
		{"the TCP connection failed", func(c *conn) { c.tcp.broken.Store(true) }},
		{"the SSH connection ended", func(c *conn) { close(c.dead) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, ch, c := newTestStream(t)
			ch.in <- []byte("partial")
			tt.lose(c)
			close(ch.in)

			got, err := io.ReadAll(s)
			if string(got) != "partial" {
				t.Errorf("read %q before the error, want what had arrived", got)
			}
			if !errors.Is(err, ErrConnectionLost) {
				t.Errorf("ReadAll = %v, want ErrConnectionLost", err)
			}
		})
	}
}

func TestStreamWriteChunksAndOrder(t *testing.T) {
	s, ch, _ := newTestStream(t)
	ch.open()

	payload := bytes.Repeat([]byte("0123456789"), 10_000) // 100 kB: four chunks
	n, err := s.Write(payload)
	if err != nil || n != len(payload) {
		t.Fatalf("Write = %d, %v; want %d", n, err, len(payload))
	}
	if n, err := s.Write(nil); n != 0 || err != nil {
		t.Errorf("zero-length Write = %d, %v", n, err)
	}
	if err := s.CloseWrite(); err != nil {
		t.Fatalf("CloseWrite: %v", err)
	}
	if err := s.CloseWrite(); err != nil {
		t.Errorf("second CloseWrite: %v", err)
	}
	if _, err := s.Write([]byte("more")); !errors.Is(err, errWriteClosed) {
		t.Errorf("Write after CloseWrite = %v, want errWriteClosed", err)
	}

	got, ops := ch.written()
	if !bytes.Equal(got, payload) {
		t.Errorf("the channel received %d bytes, want the %d written", len(got), len(payload))
	}
	want := []string{"write", "write", "write", "write", "eof"}
	if len(ops) != len(want) {
		t.Fatalf("channel operations %v, want %v", ops, want)
	}
	for i := range want {
		if ops[i] != want[i] {
			t.Fatalf("channel operations %v, want %v", ops, want)
		}
	}
}

// Many goroutines may write and half-close a stream. The channel under it
// must still see one call at a time (the race detector checks the fake's
// unguarded counter).
func TestStreamSerialisesTheChannel(t *testing.T) {
	s, ch, _ := newTestStream(t)
	ch.open()

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 50 {
				if _, err := s.Write([]byte("abcd")); err != nil {
					t.Errorf("Write: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()
	wg.Add(2)
	for range 2 {
		go func() { defer wg.Done(); _ = s.CloseWrite() }()
	}
	wg.Wait()

	got, ops := ch.written()
	if len(got) != 8*50*4 {
		t.Errorf("%d bytes reached the channel, want %d", len(got), 8*50*4)
	}
	eofs := 0
	for _, op := range ops {
		if op == "eof" {
			eofs++
		}
	}
	if eofs != 1 {
		t.Errorf("the channel was half-closed %d times, want 1", eofs)
	}
}

func TestStreamWriteDeadline(t *testing.T) {
	s, ch, _ := newTestStream(t)

	if err := s.SetWriteDeadline(time.Now().Add(50 * time.Millisecond)); err != nil {
		t.Fatalf("SetWriteDeadline: %v", err)
	}
	start := time.Now()
	n, err := s.Write([]byte("never delivered"))
	if !isTimeout(err) || n != 0 {
		t.Fatalf("blocked Write = %d, %v; want 0 and a timeout", n, err)
	}
	if d := time.Since(start); d < 40*time.Millisecond || d > time.Second {
		t.Errorf("Write timed out after %v, want about 50 ms", d)
	}

	// The abandoned bytes may still go out, so the write side is failed
	// for good: clearing the deadline does not bring it back.
	if err := s.SetWriteDeadline(time.Time{}); err != nil {
		t.Fatal(err)
	}
	ch.open()
	if _, err := s.Write([]byte("after")); !isTimeout(err) {
		t.Errorf("Write after a timed-out Write = %v, want the same timeout", err)
	}
	if err := s.CloseWrite(); !isTimeout(err) {
		t.Errorf("CloseWrite after a timed-out Write = %v, want the same timeout", err)
	}

	// Reading is unaffected.
	ch.in <- []byte("still readable")
	buf := make([]byte, 32)
	if n, err := s.Read(buf); err != nil || string(buf[:n]) != "still readable" {
		t.Errorf("Read after a write timeout = %q, %v", buf[:n], err)
	}
}

func TestStreamWriteDeadlineAlreadyPassed(t *testing.T) {
	s, ch, _ := newTestStream(t)
	ch.open()
	if err := s.SetWriteDeadline(time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write([]byte("x")); !isTimeout(err) {
		t.Fatalf("Write past its deadline = %v, want a timeout", err)
	}
	// Nothing was handed over, so the stream is not failed.
	if err := s.SetWriteDeadline(time.Time{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write([]byte("ok")); err != nil {
		t.Errorf("Write after clearing a passed deadline: %v", err)
	}
	if got, _ := ch.written(); string(got) != "ok" {
		t.Errorf("the channel received %q, want \"ok\"", got)
	}
}

func TestStreamWriteOnAChannelThePeerClosed(t *testing.T) {
	tests := []struct {
		name string
		lost bool
		want error
	}{
		{"the guest closed the stream", false, io.ErrClosedPipe},
		{"the connection was lost", true, ErrConnectionLost},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, ch, c := newTestStream(t)
			if tt.lost {
				c.tcp.broken.Store(true)
			}
			// x/crypto answers a write on a closed channel with EOF.
			released := make(chan struct{})
			close(released)
			ch.released = released
			_, err := s.Write([]byte("x"))
			if !errors.Is(err, tt.want) {
				t.Errorf("Write = %v, want %v", err, tt.want)
			}
			if err == io.EOF {
				t.Error("Write returned a bare EOF")
			}
			ch.released = make(chan struct{}) // for the cleanup's close
		})
	}
}

func TestStreamAddrs(t *testing.T) {
	s, _, _ := newTestStream(t)
	if s.LocalAddr().Network() != "unix" || s.RemoteAddr().String() != "/sock" {
		t.Errorf("addrs %v -> %v", s.LocalAddr(), s.RemoteAddr())
	}
	var _ net.Conn = s
	var _ interface{ CloseWrite() error } = s
}
