package vmutil

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"sync"
	"time"

	"github.com/charliek/shed/internal/agentproto"
	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/plugin"
	"github.com/charliek/shed/internal/remote/guest"
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
	notifyPort  uint32
}

// NewAgentClient creates a new AgentClient.
func NewAgentClient(dialer Dialer, consolePort, notifyPort uint32) *AgentClient {
	return &AgentClient{
		dialer:      dialer,
		consolePort: consolePort,
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

// maxProbeFrames bounds how many frames Probe reads looking for its reply.
// An agent older than the probe field promotes the connection and may send a
// heartbeat event before the reply; those are skipped.
const maxProbeFrames = 4

// Probe checks the agent and learns its version, wire protocol and guest
// layout. It sends a system:health request with probe:true over a short-lived
// notify-port connection; the agent replies with a HealthResponsePayload and
// does not promote the connection to the message channel.
//
// An agent that predates the probe field ignores it and replies with no
// payload: Probe then succeeds with the zero AgentInfo (protocol 0). Callers
// decide what protocol 0 means (Firecracker and VZ accept it with a warning).
func (c *AgentClient) Probe(ctx context.Context) (guest.AgentInfo, error) {
	conn, err := c.dialer.Dial(ctx, c.notifyPort)
	if err != nil {
		return guest.AgentInfo{}, fmt.Errorf("failed to connect to message port: %w", err)
	}
	defer conn.Close()

	// Bound the exchange by ctx without deadlines (some links reject them):
	// a cancel closes the connection, which unblocks the read.
	stopCloseOnCancel := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopCloseOnCancel()

	payload, err := json.Marshal(plugin.HealthRequestPayload{Probe: true})
	if err != nil {
		return guest.AgentInfo{}, fmt.Errorf("failed to marshal health request: %w", err)
	}
	reqEnv := plugin.NewEnvelope(plugin.NamespaceHealth, plugin.MessageTypeRequest, payload)
	reqData, err := json.Marshal(reqEnv)
	if err != nil {
		return guest.AgentInfo{}, fmt.Errorf("failed to marshal health request: %w", err)
	}
	if err := agentproto.WriteMessage(conn, agentproto.MsgTypePluginMessage, reqData); err != nil {
		return guest.AgentInfo{}, c.probeErr(ctx, fmt.Errorf("failed to send health request: %w", err))
	}

	for range maxProbeFrames {
		msgType, respData, err := agentproto.ReadMessage(conn)
		if err != nil {
			return guest.AgentInfo{}, c.probeErr(ctx, fmt.Errorf("failed to read health response: %w", err))
		}
		if msgType != agentproto.MsgTypePluginMessage {
			return guest.AgentInfo{}, fmt.Errorf("unexpected message type: 0x%02x", msgType)
		}

		var respEnv plugin.Envelope
		if err := json.Unmarshal(respData, &respEnv); err != nil {
			return guest.AgentInfo{}, fmt.Errorf("invalid health response envelope: %w", err)
		}
		if respEnv.Namespace != plugin.NamespaceHealth {
			return guest.AgentInfo{}, fmt.Errorf("unexpected response namespace: %s", respEnv.Namespace)
		}
		if respEnv.Type == plugin.MessageTypeEvent {
			continue // an old agent's heartbeat on a promoted connection
		}
		if respEnv.Type != plugin.MessageTypeResponse {
			return guest.AgentInfo{}, fmt.Errorf("unexpected response type: %s", respEnv.Type)
		}
		if respEnv.InReplyTo != reqEnv.ID {
			return guest.AgentInfo{}, fmt.Errorf("response in_reply_to mismatch: got %s, want %s", respEnv.InReplyTo, reqEnv.ID)
		}
		info, err := guest.ParseAgentInfo(respEnv.Payload)
		if err != nil {
			return guest.AgentInfo{}, fmt.Errorf("invalid health response payload: %w", err)
		}
		return info, nil
	}
	return guest.AgentInfo{}, fmt.Errorf("no health response in %d messages", maxProbeFrames)
}

// probeErr reports ctx's error in place of an I/O error the cancel's close
// caused.
func (c *AgentClient) probeErr(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

// CheckHealth checks that the agent answers a probe. It sends probe:true on
// every backend (see Probe); an agent that predates the field still passes.
func (c *AgentClient) CheckHealth(ctx context.Context) error {
	_, err := c.Probe(ctx)
	return err
}

// healthPollInterval is how often WaitForHealth re-probes the agent. The
// floor of detection latency is one interval, so this bounds the dead
// time between the agent actually coming up and the host noticing.
//
// Tightened from 150 ms → 50 ms (§15 Phase 1 PR 1a): each probe is a
// low-overhead transient socket dial + a small JSON health request
// (one Envelope round trip — see Probe above), so the extra
// probes at higher frequency are essentially free; a 100-ms-tighter
// detection floor shows up directly as a 50–100 ms drop in the `agent`
// PhaseTimer phase on every create. Verified end-to-end via the
// integration suite (`make test-integration`); see
// docs/discovery/runtime-optimization-backlog.md §15a.
const healthPollInterval = 50 * time.Millisecond

// WaitForHealth waits until the agent is healthy or the context is cancelled.
// An agent that reports protocol 0 (it predates the probe reply) passes with
// a logged warning: Firecracker and VZ images built before the reply existed
// keep working.
func (c *AgentClient) WaitForHealth(ctx context.Context, timeout time.Duration) error {
	info, err := c.WaitForProbe(ctx, timeout)
	if err != nil {
		return err
	}
	if info.Protocol == 0 {
		log.Printf("Warning: guest agent did not report a version (protocol 0); its image predates the probe reply")
	}
	return nil
}

// WaitForProbe probes the agent every healthPollInterval until one probe
// succeeds, and returns what that agent reported. It does not judge the
// protocol or layout; the caller does.
func (c *AgentClient) WaitForProbe(ctx context.Context, timeout time.Duration) (guest.AgentInfo, error) {
	backend.Phase(ctx, "agent")
	backend.Status(ctx, "Waiting for agent to come up...")

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ticker := time.NewTicker(healthPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return guest.AgentInfo{}, ctx.Err()
		case <-ticker.C:
			if info, err := c.Probe(ctx); err == nil {
				return info, nil
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

	// The connection lives no longer than ctx: a cancel or a deadline closes
	// it, which unblocks any write to a guest that has stopped reading (the
	// request, stdin, the stdin EOF) as well as the reader. The guest agent
	// sees the close and SIGHUPs the command's process group.
	stopCloseOnCancel := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopCloseOnCancel()

	// execDone is closed when Exec returns, allowing spawned goroutines to
	// detect completion and exit even if ctx is still alive.
	execDone := make(chan struct{})
	defer close(execDone)

	// Build exec request. An empty WorkingDir is passed through; the guest
	// agent resolves it to the shed user's home directory (it knows the
	// real resolved home), so we don't impose a host-side default here.
	req := agentproto.ExecRequest{
		Cmd:        opts.Cmd,
		Env:        opts.Env,
		TTY:        opts.TTY,
		WorkingDir: opts.WorkingDir,
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
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("failed to send exec request: %w", err)
	}
	// The request is out: from here the command may be running. Tell the
	// caller once, before any output can be delivered to its writers.
	if opts.OnStarted != nil {
		opts.OnStarted()
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
			case agentproto.MsgTypeData:
				if opts.Stdout != nil {
					if _, err := opts.Stdout.Write(data); err != nil {
						done <- err
						return
					}
				}
			case agentproto.MsgTypeStderr:
				// Route process stderr to its own writer. When the caller didn't
				// set Stderr, fold into Stdout to preserve the combined-output
				// behavior internal callers (e.g. ListSessions) rely on.
				w := opts.Stderr
				if w == nil {
					w = opts.Stdout
				}
				if w != nil {
					if _, err := w.Write(data); err != nil {
						done <- err
						return
					}
				}
			default:
				// Don't write unknown frames to stdout — this channel may carry a
				// binary stdout protocol (Zed, SFTP), and injecting unexpected bytes
				// is the corruption MsgTypeStderr separation fixes. Log for parity
				// with the agent-side pump's unknown-frame handling.
				log.Printf("Ignoring unknown message type on exec output channel: 0x%02x", msgType)
			}
		}
	}()

	// Wait for completion or context cancellation
	select {
	case err := <-done:
		// An I/O error the cancel's close caused is reported as ctx's.
		var exitErr *ExitError
		if err != nil && !errors.As(err, &exitErr) && ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}
