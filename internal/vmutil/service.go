package vmutil

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
)

// DialService opens a connection to a TCP service inside the VM by routing
// through the guest agent's TCP proxy on vsock port tcpProxyPort. It dials the
// proxy, performs the tcpproxy CONNECT handshake ("CONNECT <servicePort>\n" →
// "OK\n"), and returns a raw connection bridged to the guest service.
//
// This is the single handshake path shared by both backends. It is layered on
// top of whatever the supplied Dialer speaks:
//
//   - VZ: Dialer.Dial returns a plain net.Conn (vfkit exposes each vsock port
//     as its own Unix socket), so this performs the ONLY handshake.
//   - Firecracker: Dialer.Dial itself speaks the Firecracker vsock-UDS mux
//     handshake ("CONNECT <port>\n" → "OK <hostport>\n") and returns a
//     *BufferedConn. So FC stacks TWO handshakes: the mux one (inside Dial)
//     then the tcpproxy one (here). Because the mux reader may have already
//     buffered bytes of the tcpproxy response (a coalesced write), this reuses
//     the inner *BufferedConn's Reader instead of wrapping a second bufio.Reader
//     around it — double-wrapping would strand those buffered bytes.
//
// There are no I/O deadlines: cancellation of ctx closes the connection to
// unblock the handshake read/write (mirroring the guest agent, which sets none).
func DialService(ctx context.Context, d Dialer, tcpProxyPort uint32, servicePort uint16) (net.Conn, error) {
	conn, err := d.Dial(ctx, tcpProxyPort)
	if err != nil {
		return nil, fmt.Errorf("dial tcp proxy: %w", err)
	}

	// Context cancellation closes the connection to unblock reads/writes.
	cancelDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			conn.Close()
		case <-cancelDone:
		}
	}()

	// handshakeFailed stops the cancel goroutine and closes the connection.
	// Only valid before the success-path close(cancelDone) below.
	handshakeFailed := func(msg string, err error) (net.Conn, error) {
		close(cancelDone)
		conn.Close()
		return nil, fmt.Errorf("%s: %w", msg, err)
	}

	connectCmd := fmt.Sprintf("CONNECT %d\n", servicePort)
	if _, err := conn.Write([]byte(connectCmd)); err != nil {
		return handshakeFailed("send CONNECT", err)
	}

	// If the inner Dial already returned a BufferedConn (Firecracker's mux
	// path), reuse its Reader — it may hold bytes of the tcpproxy response that
	// arrived coalesced with the mux "OK <n>\n" line. Wrapping a second reader
	// would leave those bytes unread. Otherwise (VZ) start a fresh reader.
	var reader *bufio.Reader
	if bc, ok := conn.(*BufferedConn); ok {
		reader = bc.Reader
	} else {
		reader = bufio.NewReader(conn)
	}

	response, err := reader.ReadString('\n')
	if err != nil {
		return handshakeFailed("read CONNECT response", err)
	}

	close(cancelDone)
	// The cancel goroutine may have already selected ctx.Done() and closed conn
	// before observing cancelDone. Check for that race.
	if ctx.Err() != nil {
		conn.Close()
		return nil, ctx.Err()
	}

	response = strings.TrimSpace(response)
	if response != "OK" {
		conn.Close()
		msg := strings.TrimPrefix(response, "ERR ")
		return nil, fmt.Errorf("CONNECT %d failed: %s", servicePort, msg)
	}

	// Wrap conn (which is itself a *BufferedConn on FC) so Read drains the
	// handshake-buffered reader first. Close/CloseWrite forward to conn, which
	// on FC chains through the inner BufferedConn to the underlying net.Conn.
	return &BufferedConn{Conn: conn, Reader: reader}, nil
}
