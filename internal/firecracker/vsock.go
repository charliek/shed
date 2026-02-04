package firecracker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/charliek/shed/internal/agentproto"
	"github.com/charliek/shed/internal/backend"
	"github.com/mdlayher/vsock"
)

// VsockClient handles vsock communication with the guest agent.
type VsockClient struct {
	cid         uint32
	consolePort uint32
	healthPort  uint32
}

// NewVsockClient creates a new VsockClient.
func NewVsockClient(cid, consolePort, healthPort uint32) *VsockClient {
	return &VsockClient{
		cid:         cid,
		consolePort: consolePort,
		healthPort:  healthPort,
	}
}

// CheckHealth checks if the agent is healthy.
func (c *VsockClient) CheckHealth(ctx context.Context) error {
	conn, err := c.dialWithContext(ctx, c.healthPort)
	if err != nil {
		return fmt.Errorf("failed to connect to health port: %w", err)
	}
	defer conn.Close()

	// Send health request
	if err := agentproto.WriteMessage(conn, agentproto.MsgTypeHealthRequest, nil); err != nil {
		return fmt.Errorf("failed to send health request: %w", err)
	}

	// Read health response
	msgType, _, err := agentproto.ReadMessage(conn)
	if err != nil {
		return fmt.Errorf("failed to read health response: %w", err)
	}

	if msgType != agentproto.MsgTypeHealthResponse {
		return fmt.Errorf("unexpected response type: %d", msgType)
	}

	return nil
}

// WaitForHealth waits until the agent is healthy or the context is cancelled.
func (c *VsockClient) WaitForHealth(ctx context.Context, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := c.CheckHealth(ctx); err == nil {
				return nil
			}
		}
	}
}

// Exec executes a command in the VM via vsock.
func (c *VsockClient) Exec(ctx context.Context, opts backend.ExecOptions) error {
	conn, err := c.dialWithContext(ctx, c.consolePort)
	if err != nil {
		return fmt.Errorf("failed to connect to console port: %w", err)
	}
	defer conn.Close()

	// Build exec request
	req := agentproto.ExecRequest{
		Cmd:        opts.Cmd,
		Env:        opts.Env,
		TTY:        opts.TTY,
		WorkingDir: "/workspace",
	}

	if opts.InitialSize != nil {
		req.Rows = uint16(opts.InitialSize.Height)
		req.Cols = uint16(opts.InitialSize.Width)
	}

	// Send exec request
	reqData, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("failed to marshal exec request: %w", err)
	}

	if err := agentproto.WriteMessage(conn, agentproto.MsgTypeExecRequest, reqData); err != nil {
		return fmt.Errorf("failed to send exec request: %w", err)
	}

	// Handle resize events in a goroutine
	if opts.TTY && opts.ResizeChan != nil {
		go func() {
			for size := range opts.ResizeChan {
				msg := agentproto.ResizeMessage{
					Rows: uint16(size.Height),
					Cols: uint16(size.Width),
				}
				data, _ := json.Marshal(msg)
				agentproto.WriteMessage(conn, agentproto.MsgTypeResize, data)
			}
		}()
	}

	// Channel to signal when output is done
	done := make(chan error, 2)

	// Copy stdin to connection
	if opts.Stdin != nil {
		go func() {
			io.Copy(conn, opts.Stdin)
			// Signal EOF by closing write side if possible
			if cw, ok := conn.(interface{ CloseWrite() error }); ok {
				cw.CloseWrite()
			}
		}()
	}

	// Read output from connection
	go func() {
		defer close(done)
		for {
			msgType, data, err := agentproto.ReadMessage(conn)
			if err != nil {
				if err == io.EOF {
					done <- nil
					return
				}
				done <- err
				return
			}

			switch msgType {
			case agentproto.MsgTypeExitCode:
				var exitMsg agentproto.ExitCodeMessage
				if err := json.Unmarshal(data, &exitMsg); err != nil {
					done <- fmt.Errorf("failed to unmarshal exit code: %w", err)
					return
				}
				if exitMsg.Code != 0 {
					done <- fmt.Errorf("command exited with code %d", exitMsg.Code)
				} else {
					done <- nil
				}
				return
			default:
				// Data frame - write to stdout
				if opts.Stdout != nil {
					opts.Stdout.Write(data)
				}
			}
		}
	}()

	// Wait for completion or context cancellation
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// dialWithContext dials a vsock connection with context support.
func (c *VsockClient) dialWithContext(ctx context.Context, port uint32) (net.Conn, error) {
	// Create a channel to receive the connection result
	type dialResult struct {
		conn net.Conn
		err  error
	}
	result := make(chan dialResult, 1)

	go func() {
		conn, err := vsock.Dial(c.cid, port, nil)
		result <- dialResult{conn, err}
	}()

	select {
	case r := <-result:
		return r.conn, r.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
