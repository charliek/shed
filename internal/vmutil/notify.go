package vmutil

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"time"

	"github.com/charliek/shed/internal/agentproto"
)

// NotifyHandler processes messages on a persistent notify connection.
type NotifyHandler interface {
	// OnConnect is called when a connection is established.
	// Send setup/registration messages here.
	OnConnect(conn net.Conn) error
	// OnMessage is called for each received agentproto message.
	OnMessage(msgType byte, data []byte) error
}

// NotifyConn maintains a persistent connection with auto-reconnect and backoff.
type NotifyConn struct {
	dialer Dialer
	port   uint32
	name   string // for logging

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// NewNotifyConn creates a new NotifyConn.
func NewNotifyConn(dialer Dialer, port uint32, name string) *NotifyConn {
	return &NotifyConn{
		dialer: dialer,
		port:   port,
		name:   name,
	}
}

// Start begins the persistent connection loop.
func (nc *NotifyConn) Start(ctx context.Context, handler NotifyHandler) {
	nc.ctx, nc.cancel = context.WithCancel(ctx)
	nc.wg.Add(1)
	go func() {
		defer nc.wg.Done()
		nc.run(handler)
	}()
}

// Stop stops the connection and waits for it to finish.
func (nc *NotifyConn) Stop() {
	if nc.cancel != nil {
		nc.cancel()
	}
	nc.wg.Wait()
}

// run is the main loop that connects and reconnects with exponential backoff.
func (nc *NotifyConn) run(handler NotifyHandler) {
	backoff := time.Second
	maxBackoff := 30 * time.Second

	for {
		select {
		case <-nc.ctx.Done():
			return
		default:
		}

		err := nc.connectAndListen(handler)
		if err != nil {
			select {
			case <-nc.ctx.Done():
				return
			default:
				log.Printf("[%s] Notify connection error: %v, reconnecting in %v", nc.name, err, backoff)
				select {
				case <-time.After(backoff):
				case <-nc.ctx.Done():
					return
				}
				backoff = backoff * 2
				if backoff > maxBackoff {
					backoff = maxBackoff
				}
			}
		} else {
			// Reset backoff after a successful connection so the next
			// retry starts fresh rather than at the previously escalated delay.
			backoff = time.Second
		}
	}
}

// connectAndListen connects, calls handler.OnConnect, then reads messages.
func (nc *NotifyConn) connectAndListen(handler NotifyHandler) error {
	conn, err := nc.dialer.Dial(nc.ctx, nc.port)
	if err != nil {
		return fmt.Errorf("failed to dial notify port: %w", err)
	}
	defer conn.Close()

	if err := handler.OnConnect(conn); err != nil {
		return fmt.Errorf("handler OnConnect failed: %w", err)
	}

	// Per-connection context — canceling this unblocks the close-helper
	// goroutine when connectAndListen returns.
	connCtx, connCancel := context.WithCancel(nc.ctx)
	defer connCancel()

	// Close the connection when context is canceled so ReadMessage unblocks.
	go func() {
		<-connCtx.Done()
		conn.Close()
	}()

	for {
		msgType, data, err := agentproto.ReadMessage(conn)
		if err != nil {
			if nc.ctx.Err() != nil {
				return nil // graceful shutdown
			}
			if errors.Is(err, io.EOF) {
				return fmt.Errorf("connection closed by agent")
			}
			return fmt.Errorf("read error: %w", err)
		}

		if err := handler.OnMessage(msgType, data); err != nil {
			log.Printf("[%s] Handler OnMessage error: %v", nc.name, err)
		}
	}
}
