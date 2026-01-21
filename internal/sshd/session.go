package sshd

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/gliderlabs/ssh"

	"github.com/charliek/shed/internal/config"
)

const (
	// reservedAPIUser is a special username reserved for API access.
	reservedAPIUser = "_api"

	// containerReadyTimeout is the maximum time to wait for a container to be ready.
	containerReadyTimeout = 10 * time.Second

	// containerReadyPollInterval is how often to check if the container is ready.
	containerReadyPollInterval = 250 * time.Millisecond
)

// handleSession is the main session handler for SSH connections.
func (s *Server) handleSession(sess ssh.Session) {
	user := sess.User()
	remoteAddr := sess.RemoteAddr()

	log.Printf("SSH session started: user=%s remote=%s", user, remoteAddr)
	defer log.Printf("SSH session ended: user=%s remote=%s", user, remoteAddr)

	// Check for reserved usernames.
	if user == reservedAPIUser {
		log.Printf("Rejected reserved username: %s", user)
		fmt.Fprintf(sess.Stderr(), "Error: username '%s' is reserved for API access\n", user)
		sess.Exit(1)
		return
	}

	// Extract shed name from username (username maps directly to shed name).
	shedName := user

	// Validate shed name.
	if shedName == "" {
		log.Printf("Empty shed name from user")
		fmt.Fprintf(sess.Stderr(), "Error: invalid username\n")
		sess.Exit(1)
		return
	}

	ctx := sess.Context()

	// Look up the shed.
	shed, err := s.docker.GetShed(ctx, shedName)
	if err != nil {
		log.Printf("Failed to get shed %s: %v", shedName, err)
		fmt.Fprintf(sess.Stderr(), "Error: shed '%s' not found\n", shedName)
		sess.Exit(1)
		return
	}

	// Auto-start if stopped.
	if shed.Status == config.StatusStopped {
		log.Printf("Auto-starting stopped shed: %s", shedName)
		fmt.Fprintf(sess.Stderr(), "Starting shed '%s'...\n", shedName)

		if err := s.docker.StartShed(ctx, shedName); err != nil {
			log.Printf("Failed to start shed %s: %v", shedName, err)
			fmt.Fprintf(sess.Stderr(), "Error: failed to start shed: %v\n", err)
			sess.Exit(1)
			return
		}

		// Wait for the container to be ready.
		if err := s.waitForReady(ctx, shedName); err != nil {
			log.Printf("Shed %s not ready: %v", shedName, err)
			fmt.Fprintf(sess.Stderr(), "Error: shed not ready: %v\n", err)
			sess.Exit(1)
			return
		}

		// Refresh shed info after starting.
		shed, err = s.docker.GetShed(ctx, shedName)
		if err != nil {
			log.Printf("Failed to get shed %s after start: %v", shedName, err)
			fmt.Fprintf(sess.Stderr(), "Error: failed to get shed after start: %v\n", err)
			sess.Exit(1)
			return
		}
	}

	// Verify the shed is running.
	if shed.Status != config.StatusRunning {
		log.Printf("Shed %s is not running (status: %s)", shedName, shed.Status)
		fmt.Fprintf(sess.Stderr(), "Error: shed '%s' is not running (status: %s)\n", shedName, shed.Status)
		sess.Exit(1)
		return
	}

	// Execute in the container.
	if err := s.execInContainer(ctx, sess, shed); err != nil {
		log.Printf("Exec failed for shed %s: %v", shedName, err)
		// Don't write error to stderr here as it may have already been closed.
		sess.Exit(1)
		return
	}

	sess.Exit(0)
}

// waitForReady polls until the container is ready or timeout.
func (s *Server) waitForReady(ctx context.Context, shedName string) error {
	deadline := time.Now().Add(containerReadyTimeout)
	ticker := time.NewTicker(containerReadyPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if time.Now().After(deadline) {
				return fmt.Errorf("timeout waiting for shed to be ready")
			}

			shed, err := s.docker.GetShed(ctx, shedName)
			if err != nil {
				continue // Keep trying.
			}

			if shed.Status == config.StatusRunning {
				return nil
			}

			if shed.Status == config.StatusError {
				return fmt.Errorf("shed entered error state")
			}
		}
	}
}

// execInContainer executes a command or shell in the container.
func (s *Server) execInContainer(ctx context.Context, sess ssh.Session, shed *ShedInfo) error {
	// Get the command to execute.
	cmd := sess.Command()

	// Check if we have a PTY request.
	ptyReq, winCh, isPTY := sess.Pty()

	// Build environment variables.
	var env []string
	if isPTY {
		// Normalize the TERM value using configured mappings
		term := s.termConfig.NormalizeTerm(ptyReq.Term)
		env = append(env, fmt.Sprintf("TERM=%s", term))
	}

	// Add shed name for shell prompt customization
	env = append(env, fmt.Sprintf("SHED_NAME=%s", shed.Name))

	// Create resize channel for window changes.
	resizeChan := make(chan TerminalSize, 10)
	defer close(resizeChan)

	// Handle window resize events in a goroutine.
	if isPTY && winCh != nil {
		go s.handleWindowResize(ctx, winCh, resizeChan)
	}

	// Build initial terminal size.
	var initialSize *TerminalSize
	if isPTY {
		initialSize = &TerminalSize{
			Width:  uint(ptyReq.Window.Width),
			Height: uint(ptyReq.Window.Height),
		}
	}

	// Create the exec options.
	opts := ExecOptions{
		Cmd:         cmd,
		Stdin:       &sessionReadCloser{sess},
		Stdout:      &sessionWriteCloser{sess},
		Stderr:      &sessionStderrWriteCloser{sess},
		TTY:         isPTY,
		Env:         env,
		InitialSize: initialSize,
		ResizeChan:  resizeChan,
	}

	log.Printf("Executing in container %s: tty=%v cmd=%v", shed.ContainerID, isPTY, cmd)

	return s.docker.ExecInContainer(ctx, shed.ContainerID, opts)
}

// handleWindowResize forwards window resize events from SSH to the resize channel.
func (s *Server) handleWindowResize(ctx context.Context, winCh <-chan ssh.Window, resizeChan chan<- TerminalSize) {
	for {
		select {
		case <-ctx.Done():
			return
		case win, ok := <-winCh:
			if !ok {
				return
			}
			// Non-blocking send to resize channel.
			select {
			case resizeChan <- TerminalSize{
				Width:  uint(win.Width),
				Height: uint(win.Height),
			}:
			default:
				// Channel full, skip this resize event.
			}
		}
	}
}

// sessionReadCloser wraps an ssh.Session to implement ReadCloser.
type sessionReadCloser struct {
	sess ssh.Session
}

func (r *sessionReadCloser) Read(p []byte) (n int, err error) {
	return r.sess.Read(p)
}

func (r *sessionReadCloser) Close() error {
	return r.sess.Close()
}

// sessionWriteCloser wraps an ssh.Session to implement WriteCloser for stdout.
type sessionWriteCloser struct {
	sess ssh.Session
}

func (w *sessionWriteCloser) Write(p []byte) (n int, err error) {
	return w.sess.Write(p)
}

func (w *sessionWriteCloser) Close() error {
	return nil // Don't close the session, just stop writing.
}

// sessionStderrWriteCloser wraps an ssh.Session to implement WriteCloser for stderr.
type sessionStderrWriteCloser struct {
	sess ssh.Session
}

func (w *sessionStderrWriteCloser) Write(p []byte) (n int, err error) {
	return w.sess.Stderr().Write(p)
}

func (w *sessionStderrWriteCloser) Close() error {
	return nil // Don't close the session, just stop writing.
}
