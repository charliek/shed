package vmutil

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/charliek/shed/internal/agentproto"
	"github.com/charliek/shed/internal/backend"
)

// ExitError is returned when a command executed via the agent exits with a non-zero code.
type ExitError struct {
	Code int
}

func (e *ExitError) Error() string {
	return fmt.Sprintf("command exited with code %d", e.Code)
}

// AgentClient handles communication with the guest agent via a Dialer.
type AgentClient struct {
	dialer      Dialer
	consolePort uint32
	healthPort  uint32
	notifyPort  uint32
}

// NewAgentClient creates a new AgentClient.
func NewAgentClient(dialer Dialer, consolePort, healthPort, notifyPort uint32) *AgentClient {
	return &AgentClient{
		dialer:      dialer,
		consolePort: consolePort,
		healthPort:  healthPort,
		notifyPort:  notifyPort,
	}
}

// NotifyPort returns the notify port number.
func (c *AgentClient) NotifyPort() uint32 {
	return c.notifyPort
}

// Dialer returns the underlying dialer.
func (c *AgentClient) Dialer() Dialer {
	return c.dialer
}

// CheckHealth checks if the agent is healthy.
func (c *AgentClient) CheckHealth(ctx context.Context) error {
	conn, err := c.dialer.Dial(ctx, c.healthPort)
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
func (c *AgentClient) WaitForHealth(ctx context.Context, timeout time.Duration) error {
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

// Exec executes a command in the VM via the agent.
func (c *AgentClient) Exec(ctx context.Context, opts backend.ExecOptions) error {
	conn, err := c.dialer.Dial(ctx, c.consolePort)
	if err != nil {
		return fmt.Errorf("failed to connect to console port: %w", err)
	}
	defer conn.Close()

	// execDone is closed when Exec returns, allowing spawned goroutines to
	// detect completion and exit even if ctx is still alive.
	execDone := make(chan struct{})
	defer close(execDone)

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

	writeMu := &sync.Mutex{}
	writeMessage := func(msgType byte, payload []byte) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return agentproto.WriteMessage(conn, msgType, payload)
	}

	if err := writeMessage(agentproto.MsgTypeExecRequest, reqData); err != nil {
		return fmt.Errorf("failed to send exec request: %w", err)
	}

	// Handle resize events in a goroutine
	if opts.TTY && opts.ResizeChan != nil {
		go func() {
			for {
				select {
				case size, ok := <-opts.ResizeChan:
					if !ok {
						return
					}
					msg := agentproto.ResizeMessage{
						Rows: uint16(size.Height),
						Cols: uint16(size.Width),
					}
					data, err := json.Marshal(msg)
					if err != nil {
						continue
					}
					if err := writeMessage(agentproto.MsgTypeResize, data); err != nil {
						return
					}
				case <-ctx.Done():
					return
				case <-execDone:
					return
				}
			}
		}()
	}

	// Channel to signal when output is done
	done := make(chan error, 2)

	// Copy stdin to connection as framed protocol messages.
	if opts.Stdin != nil {
		go func() {
			buf := make([]byte, 4096)
			for {
				n, err := opts.Stdin.Read(buf)
				if n > 0 {
					if writeErr := writeMessage(agentproto.MsgTypeData, buf[:n]); writeErr != nil {
						break
					}
				}
				if err != nil {
					break
				}
			}
			_ = writeMessage(agentproto.MsgTypeStdinEOF, nil)
		}()
	} else {
		_ = writeMessage(agentproto.MsgTypeStdinEOF, nil)
	}

	// Read output from connection
	go func() {
		defer close(done)
		for {
			msgType, data, err := agentproto.ReadMessage(conn)
			if err != nil {
				if err == io.EOF {
					done <- fmt.Errorf("connection closed before exit code received")
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
					done <- &ExitError{Code: exitMsg.Code}
				} else {
					done <- nil
				}
				return
			default:
				if opts.Stdout != nil {
					if _, err := opts.Stdout.Write(data); err != nil {
						done <- err
						return
					}
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
