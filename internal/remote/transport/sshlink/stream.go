package sshlink

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"
)

// streamChunk is the most a stream reads or writes in one step. It equals the
// largest SSH channel data packet x/crypto sends.
const streamChunk = 32 * 1024

// Errors a stream returns.
var (
	// errWriteClosed: Write after CloseWrite.
	errWriteClosed = errors.New("sshlink: write on a stream whose write side is closed")
	// ErrConnectionLost is returned by a stream's Read when the SSH
	// connection under it ended before the guest closed the stream. Without
	// it a dropped connection would look like a clean end-of-file, and a
	// proxied download would be cut short with no error.
	ErrConnectionLost = fmt.Errorf("sshlink: connection to the guest was lost: %w", io.ErrUnexpectedEOF)
)

// channelConn is what x/crypto returns for a forwarded unix socket: a
// net.Conn with a half-close.
type channelConn interface {
	net.Conn
	CloseWrite() error
}

// stream is one agent connection: an SSH channel to one of the agent's unix
// sockets, wrapped so it behaves like the socket conns the rest of the server
// was written against.
//
// The wrapping exists because an x/crypto channel differs from a socket in
// three ways the server depends on:
//
//   - Close only sends a close message. A Read or Write blocked on the
//     channel returns when the peer answers, and sshd does not answer until
//     it has drained its buffer into the guest socket, which never happens if
//     the guest process has stopped reading. vmutil.AgentClient.Exec, Probe
//     and DialService all rely on Close unblocking their I/O at once.
//   - Deadlines are refused.
//   - Write and CloseWrite are not safe to call from different goroutines.
//
// A stream therefore moves channel I/O onto two goroutines of its own, a
// reader and a writer, and its Read, Write and CloseWrite hand work to them.
// Close, a deadline and a lost connection all end the wait locally. Write
// still returns only after the bytes were handed to the SSH transport, so
// "Write then Close" loses nothing.
//
// After Close, a goroutine that is still inside the channel stays there until
// the peer answers the close or the SSH connection ends. Against OpenSSH 9.6
// (TestSSHDFacts) an idle stream's close is answered in under a millisecond,
// and a stream whose guest side has stopped reading is answered only once the
// guest drains it. Until then an abandoned stream costs a goroutine, a 32 KiB
// buffer and its slot in the SSH connection, and no more: nothing waits on it.
type stream struct {
	ch   channelConn
	conn *conn // the SSH connection the channel lives on

	done      chan struct{} // closed by Close
	closeOnce sync.Once

	// Read side. rmu serialises Read; the fields below it belong to the
	// holder.
	rmu    sync.Mutex
	rstart sync.Once
	rch    chan readChunk // reader goroutine -> Read
	rack   chan struct{}  // Read -> reader goroutine: the chunk is consumed
	rbuf   []byte         // unread part of the last chunk
	rerr   error          // sticky
	rdl    deadline

	// Write side. wmu serialises Write and CloseWrite.
	wmu    sync.Mutex
	wstart sync.Once
	wreq   chan writeReq // Write -> writer goroutine
	wres   chan writeRes // writer goroutine -> Write
	werr   error         // sticky
	wdl    deadline
}

type readChunk struct {
	data []byte
	err  error
}

type writeReq struct {
	bufp *[]byte // a pooled copy of the caller's bytes; nil with eof
	n    int     // how much of *bufp to write
	eof  bool    // CloseWrite
}

type writeRes struct {
	n   int
	err error
}

var chunkPool = sync.Pool{New: func() any {
	b := make([]byte, streamChunk)
	return &b
}}

func newStream(ch channelConn, c *conn) *stream {
	return &stream{
		ch:   ch,
		conn: c,
		done: make(chan struct{}),
		rch:  make(chan readChunk),
		rack: make(chan struct{}, 1),
		rdl:  makeDeadline(),
		wreq: make(chan writeReq),
		wres: make(chan writeRes),
		wdl:  makeDeadline(),
	}
}

func (s *stream) isClosed() bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}

// Read reads from the guest. It returns net.ErrClosed once the stream is
// closed, os.ErrDeadlineExceeded when the read deadline passes, io.EOF when
// the guest closed its side, and ErrConnectionLost when the SSH connection
// ended first.
func (s *stream) Read(p []byte) (int, error) {
	s.rmu.Lock()
	defer s.rmu.Unlock()

	if s.isClosed() {
		return 0, net.ErrClosed
	}
	if s.rdl.passed() {
		return 0, os.ErrDeadlineExceeded
	}
	for len(s.rbuf) == 0 {
		if s.rerr != nil {
			return 0, s.rerr
		}
		if s.isClosed() {
			return 0, net.ErrClosed
		}
		if len(p) == 0 {
			return 0, nil
		}
		s.rstart.Do(func() { go s.readLoop() })
		select {
		case c := <-s.rch:
			s.rbuf, s.rerr = c.data, c.err
			if len(s.rbuf) == 0 && s.rerr == nil {
				s.rack <- struct{}{}
			}
		case <-s.done:
			return 0, net.ErrClosed
		case <-s.rdl.wait():
			return 0, os.ErrDeadlineExceeded
		}
	}

	n := copy(p, s.rbuf)
	s.rbuf = s.rbuf[n:]
	if len(s.rbuf) == 0 && s.rerr == nil {
		// Let the reader goroutine reuse its buffer and read on.
		s.rack <- struct{}{}
	}
	return n, nil
}

// readLoop is the reader goroutine. It reads one chunk ahead and no more: it
// waits for Read to consume a chunk before reading the next, so SSH flow
// control still reflects what the caller has taken.
func (s *stream) readLoop() {
	buf := make([]byte, streamChunk)
	for {
		n, err := s.ch.Read(buf)
		if err == io.EOF && s.conn.lost() {
			err = ErrConnectionLost
		}
		select {
		case s.rch <- readChunk{data: buf[:n], err: err}:
		case <-s.done:
			return
		}
		if err != nil {
			return
		}
		select {
		case <-s.rack:
		case <-s.done:
			return
		}
	}
}

// Write writes to the guest. It returns after the bytes were handed to the
// SSH transport, or early with net.ErrClosed or os.ErrDeadlineExceeded. A
// write that was cut short by its deadline fails the stream's write side:
// later writes return the same error, because the abandoned bytes may or may
// not still be delivered.
func (s *stream) Write(p []byte) (int, error) {
	s.wmu.Lock()
	defer s.wmu.Unlock()

	total := 0
	for len(p) > 0 {
		chunk := p[:min(len(p), streamChunk)]
		bufp := chunkPool.Get().(*[]byte)
		n := copy(*bufp, chunk)
		written, err := s.submit(writeReq{bufp: bufp, n: n})
		total += written
		if err != nil {
			return total, err
		}
		p = p[len(chunk):]
	}
	return total, nil
}

// CloseWrite closes the write side: the guest sees end-of-file on its read
// side while the other direction stays open. It is ordered after every Write
// that returned before it.
func (s *stream) CloseWrite() error {
	s.wmu.Lock()
	defer s.wmu.Unlock()

	if errors.Is(s.werr, errWriteClosed) {
		return nil
	}
	if _, err := s.submit(writeReq{eof: true}); err != nil {
		return err
	}
	s.werr = errWriteClosed
	return nil
}

// submit hands one request to the writer goroutine and waits for its result.
// The caller holds wmu. The writer goroutine returns the request's pooled
// buffer to the pool; submit does if the request was never handed over.
func (s *stream) submit(req writeReq) (int, error) {
	release := func() {
		if req.bufp != nil {
			chunkPool.Put(req.bufp)
		}
	}
	if s.werr != nil {
		release()
		return 0, s.werr
	}
	if s.isClosed() {
		release()
		return 0, net.ErrClosed
	}
	if s.wdl.passed() {
		release()
		return 0, os.ErrDeadlineExceeded
	}
	s.wstart.Do(func() { go s.writeLoop() })

	select {
	case s.wreq <- req:
	case <-s.done:
		release()
		return 0, net.ErrClosed
	case <-s.wdl.wait():
		release()
		return 0, os.ErrDeadlineExceeded
	}

	select {
	case res := <-s.wres:
		if res.err != nil {
			s.werr = res.err
		}
		return res.n, res.err
	case <-s.done:
		return 0, net.ErrClosed
	case <-s.wdl.wait():
		// The request is still with the writer goroutine; its result
		// would be mistaken for the next request's.
		s.werr = os.ErrDeadlineExceeded
		return 0, os.ErrDeadlineExceeded
	}
}

// writeLoop is the writer goroutine: the only caller of the channel's Write
// and CloseWrite.
func (s *stream) writeLoop() {
	for {
		var req writeReq
		select {
		case req = <-s.wreq:
		case <-s.done:
			return
		}

		var res writeRes
		if req.eof {
			res.err = s.ch.CloseWrite()
		} else {
			res.n, res.err = s.ch.Write((*req.bufp)[:req.n])
			chunkPool.Put(req.bufp)
		}
		if res.err == io.EOF {
			// x/crypto reports a write on a closed channel as EOF,
			// which is not an error a writer expects.
			if s.conn.lost() {
				res.err = ErrConnectionLost
			} else {
				res.err = io.ErrClosedPipe
			}
		}

		select {
		case s.wres <- res:
		case <-s.done:
			return
		}
	}
}

// Close closes the stream. Blocked Reads and Writes return at once. It is
// safe to call more than once and from any goroutine.
func (s *stream) Close() error {
	s.closeOnce.Do(func() {
		close(s.done)
		// Sends the channel close. The error is that of a channel or
		// connection that is already gone, which is what Close wants.
		_ = s.ch.Close()
	})
	return nil
}

func (s *stream) LocalAddr() net.Addr  { return s.ch.LocalAddr() }
func (s *stream) RemoteAddr() net.Addr { return s.ch.RemoteAddr() }

// SetDeadline sets both deadlines. Unlike a bare x/crypto channel, a stream
// supports them.
func (s *stream) SetDeadline(t time.Time) error {
	if s.isClosed() {
		return net.ErrClosed
	}
	s.rdl.set(t)
	s.wdl.set(t)
	return nil
}

// SetReadDeadline sets the deadline for Read. Zero means none.
func (s *stream) SetReadDeadline(t time.Time) error {
	if s.isClosed() {
		return net.ErrClosed
	}
	s.rdl.set(t)
	return nil
}

// SetWriteDeadline sets the deadline for Write and CloseWrite. Zero means
// none.
func (s *stream) SetWriteDeadline(t time.Time) error {
	if s.isClosed() {
		return net.ErrClosed
	}
	s.wdl.set(t)
	return nil
}

// deadline is a settable deadline that waiters select on. It follows the
// pipeDeadline of the standard library's net.Pipe.
type deadline struct {
	mu     sync.Mutex
	timer  *time.Timer
	cancel chan struct{} // closed when the deadline has passed
}

func makeDeadline() deadline {
	return deadline{cancel: make(chan struct{})}
}

// set sets the point in time when the deadline passes. A zero t clears it.
func (d *deadline) set(t time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.timer != nil && !d.timer.Stop() {
		<-d.cancel // the timer fired; wait for it to finish closing cancel
	}
	d.timer = nil

	closed := false
	select {
	case <-d.cancel:
		closed = true
	default:
	}

	if t.IsZero() {
		if closed {
			d.cancel = make(chan struct{})
		}
		return
	}
	if dur := time.Until(t); dur > 0 {
		if closed {
			d.cancel = make(chan struct{})
		}
		d.timer = time.AfterFunc(dur, func() { close(d.cancel) })
		return
	}
	if !closed {
		close(d.cancel)
	}
}

// passed reports whether the deadline has passed.
func (d *deadline) passed() bool {
	select {
	case <-d.wait():
		return true
	default:
		return false
	}
}

// wait returns a channel that is closed when the deadline has passed.
func (d *deadline) wait() chan struct{} {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.cancel
}
