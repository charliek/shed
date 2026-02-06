//go:build linux
// +build linux

package firecracker

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	"github.com/charliek/shed/internal/agentproto"
	"github.com/charliek/shed/internal/backend"
)

// VsockClient handles vsock communication with the guest agent via Firecracker's UDS.
type VsockClient struct {
	socketPath  string
	consolePort uint32
	healthPort  uint32
}

// NewVsockClient creates a new VsockClient.
// socketPath is the path to Firecracker's vsock Unix domain socket.
func NewVsockClient(socketPath string, consolePort, healthPort uint32) *VsockClient {
	return &VsockClient{
		socketPath:  socketPath,
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
	workingDir := opts.WorkingDir
	if workingDir == "" {
		workingDir = "/workspace"
	}
	req := agentproto.ExecRequest{
		Cmd:        opts.Cmd,
		Env:        opts.Env,
		TTY:        opts.TTY,
		WorkingDir: workingDir,
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
				data, err := json.Marshal(msg)
				if err != nil {
					// Log but continue - resize failures are non-fatal
					continue
				}
				if err := agentproto.WriteMessage(conn, agentproto.MsgTypeResize, data); err != nil {
					// Log but continue - resize failures are non-fatal, connection may be closing
					continue
				}
			}
		}()
	}

	// Channel to signal when output is done
	done := make(chan error, 2)

	// Copy stdin to connection
	if opts.Stdin != nil {
		go func() {
			if _, err := io.Copy(conn, opts.Stdin); err != nil {
				// Log stdin errors but don't fail - often expected on disconnect
				// Only log if it's not a closed connection error
				if !strings.Contains(err.Error(), "closed") && !strings.Contains(err.Error(), "EOF") {
					// Silently ignore - stdin errors are often expected when connection closes
				}
			}
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

// dialWithContext connects to the guest via Firecracker's vsock UDS.
// Firecracker's vsock protocol requires:
// 1. Connect to the Unix domain socket
// 2. Send "CONNECT <port>\n"
// 3. Read response "OK <port>\n"
// 4. Then the connection is bridged to the guest
func (c *VsockClient) dialWithContext(ctx context.Context, port uint32) (net.Conn, error) {
	// Create a channel to receive the connection result
	type dialResult struct {
		conn net.Conn
		err  error
	}
	result := make(chan dialResult, 1)

	go func() {
		// Connect to Firecracker's vsock UDS
		conn, err := net.Dial("unix", c.socketPath)
		if err != nil {
			result <- dialResult{nil, fmt.Errorf("failed to connect to vsock socket: %w", err)}
			return
		}

		// Send CONNECT command
		connectCmd := fmt.Sprintf("CONNECT %d\n", port)
		if _, err := conn.Write([]byte(connectCmd)); err != nil {
			conn.Close()
			result <- dialResult{nil, fmt.Errorf("failed to send CONNECT command: %w", err)}
			return
		}

		// Read response
		reader := bufio.NewReader(conn)
		response, err := reader.ReadString('\n')
		if err != nil {
			conn.Close()
			result <- dialResult{nil, fmt.Errorf("failed to read CONNECT response: %w", err)}
			return
		}

		response = strings.TrimSpace(response)
		if !strings.HasPrefix(response, "OK ") {
			conn.Close()
			result <- dialResult{nil, fmt.Errorf("vsock CONNECT failed: %s", response)}
			return
		}

		// Return a connection that wraps the buffered reader
		result <- dialResult{&vsockConn{Conn: conn, reader: reader}, nil}
	}()

	select {
	case r := <-result:
		return r.conn, r.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// vsockConn wraps a net.Conn with a buffered reader for the initial handshake.
type vsockConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *vsockConn) Read(p []byte) (int, error) {
	return c.reader.Read(p)
}
