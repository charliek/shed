package vmutil

import (
	"bufio"
	"io"
	"net"
	"sync"
)

// BufferedConn wraps a net.Conn with a bufio.Reader to ensure any bytes
// buffered during a handshake are returned on subsequent reads.
type BufferedConn struct {
	net.Conn
	Reader *bufio.Reader
}

// Read reads from the buffered reader, draining any handshake-buffered data first.
func (c *BufferedConn) Read(p []byte) (int, error) {
	return c.Reader.Read(p)
}

// CloseWrite calls CloseWrite on the underlying connection if supported.
func (c *BufferedConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}

// BidirectionalCopy bridges two connections bidirectionally, copying data
// in both directions until one side closes or errors. Blocks until both
// directions complete.
func BidirectionalCopy(a, b io.ReadWriter) {
	closeWrite := func(rw io.ReadWriter) {
		if cw, ok := rw.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(a, b)
		closeWrite(a) // signal EOF to reader on a
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(b, a)
		closeWrite(b) // signal EOF to reader on b
	}()
	wg.Wait()
}
