package tunnels

import (
	"context"
	"fmt"
	"log"
	"net"
	"sync"
	"time"

	"github.com/charliek/shed/internal/vmutil"
)

// Tunnel represents an active local TCP listener that bridges to a shed VM
// via the Connect API.
type Tunnel struct {
	client   *ConnectClient
	shedName string
	port     PortMapping
	listener net.Listener
	ctx      context.Context
	cancel   context.CancelFunc
	wg       sync.WaitGroup
}

// NewTunnel creates a new tunnel (does not start it).
func NewTunnel(client *ConnectClient, shedName string, port PortMapping) *Tunnel {
	ctx, cancel := context.WithCancel(context.Background())
	return &Tunnel{
		client:   client,
		shedName: shedName,
		port:     port,
		ctx:      ctx,
		cancel:   cancel,
	}
}

// Start opens the local listener and begins accepting connections.
func (t *Tunnel) Start() error {
	var err error
	t.listener, err = net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", t.port.Local))
	if err != nil {
		return fmt.Errorf("listen on port %d: %w", t.port.Local, err)
	}

	t.wg.Add(1)
	go t.acceptLoop()
	return nil
}

func (t *Tunnel) acceptLoop() {
	defer t.wg.Done()
	for {
		conn, err := t.listener.Accept()
		if err != nil {
			select {
			case <-t.ctx.Done():
				return
			default:
				log.Printf("tunnel %d: accept error: %v", t.port.Local, err)
				continue
			}
		}
		t.wg.Add(1)
		go t.handleConn(conn)
	}
}

func (t *Tunnel) handleConn(clientConn net.Conn) {
	defer t.wg.Done()
	defer clientConn.Close()

	vmConn, err := t.client.Dial(t.ctx, t.shedName, uint16(t.port.Remote))
	if err != nil {
		log.Printf("tunnel %d: connect failed: %v", t.port.Local, err)
		return
	}
	defer vmConn.Close()

	// If Stop() raced this dial and already cancelled, don't start copying.
	if t.ctx.Err() != nil {
		return
	}

	// Stop() only cancels the context; BidirectionalCopy blocks on io.Copy and
	// never watches it, so without help an idle-but-open connection would wedge
	// Stop()'s wg.Wait() forever. Close both conns when the tunnel is stopping
	// so both copies return. net.Conn.Close is idempotent, so the deferred
	// closes above are safe to run again.
	defer closeConnsOnCancel(t.ctx, clientConn, vmConn)()

	vmutil.BidirectionalCopy(clientConn, vmConn)
}

// closeConnsOnCancel closes conns if ctx is cancelled before the returned stop
// func runs, so a blocking read/write (which doesn't watch the context) is
// unblocked on teardown. The caller defers stop() to end the watcher once the
// operation it guards completes.
//
// stop is idempotent and synchronous: it returns only after the watcher has
// exited, and the watcher prefers the stop signal when both fire at once, so
// once stop() returns the conns are guaranteed safe from this watcher (e.g. a
// conn handed back by Dial can't be closed out from under the caller).
func closeConnsOnCancel(ctx context.Context, conns ...net.Conn) (stop func()) {
	done := make(chan struct{})
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		select {
		case <-done:
			return
		case <-ctx.Done():
			// stop() may have raced the cancellation; if so, don't close conns
			// the caller has already reclaimed.
			select {
			case <-done:
				return
			default:
			}
			for _, c := range conns {
				_ = c.Close()
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() { close(done) })
		<-exited
	}
}

// stopDrainTimeout bounds how long Stop() waits for in-flight connections to
// drain before giving up. Closing the conns (see handleConn) normally unblocks
// the copies immediately; this is a backstop so a latent edge case can never
// reintroduce the hang this fix removes.
const stopDrainTimeout = 5 * time.Second

// Stop stops the tunnel, closing the listener and waiting for connections to
// drain (bounded by stopDrainTimeout).
func (t *Tunnel) Stop() {
	t.cancel()
	if t.listener != nil {
		t.listener.Close()
	}

	done := make(chan struct{})
	go func() {
		t.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(stopDrainTimeout):
		log.Printf("tunnel %d: timed out waiting for connections to drain", t.port.Local)
	}
}

// LocalAddr returns the local listener address, or nil if not started.
func (t *Tunnel) LocalAddr() net.Addr {
	if t.listener != nil {
		return t.listener.Addr()
	}
	return nil
}

// Port returns the port mapping for this tunnel.
func (t *Tunnel) Port() PortMapping {
	return t.port
}
