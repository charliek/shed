package tunnels

import (
	"context"
	"fmt"
	"log"
	"net"
	"sync"

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

	vmutil.BidirectionalCopy(clientConn, vmConn)
}

// Stop stops the tunnel, closing the listener and waiting for connections to drain.
func (t *Tunnel) Stop() {
	t.cancel()
	if t.listener != nil {
		t.listener.Close()
	}
	t.wg.Wait()
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
