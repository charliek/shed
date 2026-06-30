//go:build linux
// +build linux

package main

import (
	"encoding/json"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
)

// handleExecConnection handles a connection on the console port.
func handleExecConnection(conn net.Conn, user *userInfo) {
	defer conn.Close()

	// Read the exec request
	msgType, data, err := readMessage(conn)
	if err != nil {
		log.Printf("Failed to read exec request: %v", err)
		return
	}

	if msgType != MsgTypeExecRequest {
		log.Printf("Unexpected message type: %d", msgType)
		return
	}

	var req ExecRequest
	if err := json.Unmarshal(data, &req); err != nil {
		log.Printf("Failed to unmarshal exec request: %v", err)
		if err := writeExitCode(conn, 1); err != nil {
			log.Printf("Warning: failed to write exit code: %v", err)
		}
		return
	}

	// Validate command
	if len(req.Cmd) == 0 {
		req.Cmd = []string{"/bin/bash", "--login"}
	}

	log.Printf("Executing: %v (TTY: %v)", req.Cmd, req.TTY)

	// Create the command
	cmd := exec.Command(req.Cmd[0], req.Cmd[1:]...)

	// Run as non-root user if resolved at startup
	if user != nil {
		if cmd.SysProcAttr == nil {
			cmd.SysProcAttr = &syscall.SysProcAttr{}
		}
		cmd.SysProcAttr.Credential = user.cred
	}

	// Set working directory. An empty working dir defaults to the shed
	// user's home directory (the interactive landing default); fall back to
	// "/" if the home or requested directory doesn't exist.
	workDir := req.WorkingDir
	if workDir == "" && user != nil {
		workDir = user.homeDir
	}
	if workDir == "" {
		workDir = "/"
	}
	if _, err := os.Stat(workDir); err == nil {
		cmd.Dir = workDir
	} else {
		cmd.Dir = "/"
	}

	// Build environment: system env + environment.d + request-provided env
	env := append(os.Environ(), loadEnvironmentD("/etc/environment.d")...)
	env = append(env, req.Env...)

	// When running as a non-root user, force HOME and USER to match the
	// resolved user. The systemd service runs as root and sets USER=root
	// in the inherited environment, which must be overridden.
	if user != nil {
		env = setEnv(env, "HOME", user.homeDir)
		env = setEnv(env, "USER", user.name)
	}

	// Ensure essential variables have defaults if not set.
	// The systemd service environment may not include HOME, USER, etc.
	// Scripts with set -u fail when referencing unset variables.
	homeDir := "/root"
	userName := "root"
	if user != nil {
		homeDir = user.homeDir
		userName = user.name
	}
	essentialDefaults := [][2]string{
		{"HOME", homeDir},
		{"USER", userName},
		{"PATH", "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"},
		{"SHELL", "/bin/bash"},
		{"LANG", "C.UTF-8"},
	}
	for _, kv := range essentialDefaults {
		key := kv[0]
		found := false
		for _, e := range env {
			if strings.HasPrefix(e, key+"=") {
				found = true
				break
			}
		}
		if !found {
			env = append(env, key+"="+kv[1])
		}
	}
	cmd.Env = env

	// Only set TERM if not already provided by the caller
	hasTerm := false
	for _, e := range cmd.Env {
		if strings.HasPrefix(e, "TERM=") {
			hasTerm = true
			break
		}
	}
	if !hasTerm {
		cmd.Env = append(cmd.Env, "TERM=xterm-256color")
	}

	if req.TTY {
		runWithPTY(conn, cmd, req.Rows, req.Cols)
	} else {
		runWithoutPTY(conn, cmd)
	}
}

// runWithPTY runs a command with a PTY.
func runWithPTY(conn net.Conn, cmd *exec.Cmd, rows, cols uint16) {
	// Start command with PTY
	ptmx, err := pty.Start(cmd)
	if err != nil {
		log.Printf("Failed to start command with PTY: %v", err)
		if err := writeExitCode(conn, 1); err != nil {
			log.Printf("Warning: failed to write exit code: %v", err)
		}
		return
	}
	defer ptmx.Close()

	// Set initial size
	if rows > 0 && cols > 0 {
		if err := pty.Setsize(ptmx, &pty.Winsize{
			Rows: rows,
			Cols: cols,
		}); err != nil {
			log.Printf("Warning: failed to set initial PTY size: %v", err)
		}
	}

	// Channel to signal when command exits
	exitCh := make(chan error, 1)

	// WaitGroup to ensure output is flushed before sending exit code
	var outputWg sync.WaitGroup

	// Forward client→process messages (stdin data, resize, signals) using
	// blocking reads. The pump is unblocked and awaited at teardown via
	// pump.stop() before the exit code is sent.
	pump := startClientPump(conn, messageHandlers{
		onData: func(b []byte) {
			if _, err := ptmx.Write(b); err != nil {
				log.Printf("Warning: failed to write to PTY: %v", err)
			}
		},
		onResize: func(rows, cols uint16) {
			if err := pty.Setsize(ptmx, &pty.Winsize{Rows: rows, Cols: cols}); err != nil {
				log.Printf("Warning: failed to resize PTY: %v", err)
			}
		},
		onSignal:       signalProcess(cmd),
		stopOnStdinEOF: false, // a PTY has no separate stdin pipe
	}, nil)

	// Copy PTY output to connection
	outputWg.Add(1)
	go func() {
		defer outputWg.Done()
		buf := make([]byte, 4096)
		for {
			n, err := ptmx.Read(buf)
			if err != nil {
				if err != io.EOF {
					log.Printf("PTY read error: %v", err)
				}
				return
			}
			if n > 0 {
				if err := writeData(conn, buf[:n]); err != nil {
					log.Printf("Warning: failed to write data to connection: %v", err)
					return
				}
			}
		}
	}()

	// Wait for command to exit
	go func() {
		exitCh <- cmd.Wait()
	}()

	err = <-exitCh
	exitCode := 0
	if err != nil {
		if exitError, ok := err.(*exec.ExitError); ok {
			exitCode = exitError.ExitCode()
		} else {
			exitCode = 1
		}
	}

	// Close the PTY master to unblock the output goroutine. Without this,
	// ptmx.Read() can block indefinitely after the process exits because
	// the PTY master doesn't always deliver EOF promptly on Linux.
	ptmx.Close()

	// Wait for output to be flushed before sending exit code
	outputWg.Wait()

	// Unblock and await the input pump now that the process has exited, before
	// sending the exit code. (stop() sets a read deadline, which affects reads
	// only, so the writeExitCode below is unaffected.)
	pump.stop()

	if err := writeExitCode(conn, exitCode); err != nil {
		log.Printf("Warning: failed to write exit code: %v", err)
	}
	log.Printf("Command exited with code %d", exitCode)
}

// runWithoutPTY runs a command without a PTY.
func runWithoutPTY(conn net.Conn, cmd *exec.Cmd) {
	// Set up pipes
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		log.Printf("Failed to create stdout pipe: %v", err)
		if err := writeExitCode(conn, 1); err != nil {
			log.Printf("Warning: failed to write exit code: %v", err)
		}
		return
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		log.Printf("Failed to create stderr pipe: %v", err)
		if err := writeExitCode(conn, 1); err != nil {
			log.Printf("Warning: failed to write exit code: %v", err)
		}
		return
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		log.Printf("Failed to create stdin pipe: %v", err)
		if err := writeExitCode(conn, 1); err != nil {
			log.Printf("Warning: failed to write exit code: %v", err)
		}
		return
	}

	// Start command
	if err := cmd.Start(); err != nil {
		log.Printf("Failed to start command: %v", err)
		if err := writeExitCode(conn, 1); err != nil {
			log.Printf("Warning: failed to write exit code: %v", err)
		}
		return
	}

	// WaitGroup to ensure output is flushed before sending exit code
	var outputWg sync.WaitGroup

	// Forward client→process stdin using blocking reads. stdin is closed when the
	// pump returns (the cleanup arg) so the command sees EOF (e.g. `tar xzpf -`
	// finishes reading); the close is idempotent (cmd.Wait also closes the pipe).
	// Resize frames are ignored in non-PTY mode because onResize is left nil. The
	// pump is unblocked and awaited at teardown via pump.stop().
	pump := startClientPump(conn, messageHandlers{
		onData: func(b []byte) {
			if _, err := stdin.Write(b); err != nil {
				log.Printf("Warning: failed to write to stdin: %v", err)
			}
		},
		onSignal:       signalProcess(cmd),
		stopOnStdinEOF: true,
	}, func() { _ = stdin.Close() })

	// Mutex to protect concurrent writes to conn from stdout/stderr goroutines
	var connMu sync.Mutex

	// Copy stdout to connection
	outputWg.Add(1)
	go func() {
		defer outputWg.Done()
		buf := make([]byte, 4096)
		for {
			n, err := stdout.Read(buf)
			if err != nil {
				return
			}
			if n > 0 {
				connMu.Lock()
				err := writeData(conn, buf[:n])
				connMu.Unlock()
				if err != nil {
					log.Printf("Warning: failed to write stdout to connection: %v", err)
					return
				}
			}
		}
	}()

	// Copy stderr to connection (same stream for now)
	outputWg.Add(1)
	go func() {
		defer outputWg.Done()
		buf := make([]byte, 4096)
		for {
			n, err := stderr.Read(buf)
			if err != nil {
				return
			}
			if n > 0 {
				connMu.Lock()
				err := writeData(conn, buf[:n])
				connMu.Unlock()
				if err != nil {
					log.Printf("Warning: failed to write stderr to connection: %v", err)
					return
				}
			}
		}
	}()

	// Wait for command
	err = cmd.Wait()
	exitCode := 0
	if err != nil {
		if exitError, ok := err.(*exec.ExitError); ok {
			exitCode = exitError.ExitCode()
		} else {
			exitCode = 1
		}
	}

	// Wait for output to be flushed before sending exit code
	outputWg.Wait()

	// Unblock and await the input pump now that the process has exited, before
	// sending the exit code.
	pump.stop()

	connMu.Lock()
	err = writeExitCode(conn, exitCode)
	connMu.Unlock()
	if err != nil {
		log.Printf("Warning: failed to write exit code: %v", err)
	}
	log.Printf("Command exited with code %d", exitCode)
}

// messageHandlers customizes how pumpClientMessages dispatches each frame from
// the host. Any handler may be nil.
//
//   - onData receives stdin payload destined for the process (the stdin pipe or
//     the PTY master). It logs its own write errors and never stops the pump: a
//     process that has closed its own stdin may still be running and must keep
//     receiving forwarded signals. Pump termination is driven solely by
//     MsgTypeStdinEOF (when stopOnStdinEOF is set), a read error (the host closed
//     the conn), or the teardown read deadline.
//   - onResize handles a PTY window-resize request (left nil for the non-PTY path,
//     where resize frames are ignored).
//   - onSignal forwards an already-validated signal number to the process.
//
// stopOnStdinEOF is true for the non-PTY path (closing the stdin pipe lets the
// process see EOF) and false for the PTY path (a PTY has no separate stdin pipe,
// so MsgTypeStdinEOF is ignored).
type messageHandlers struct {
	onData         func([]byte)
	onResize       func(rows, cols uint16)
	onSignal       func(sig int)
	stopOnStdinEOF bool
}

// pumpClientMessages reads framed messages from the host and dispatches each to
// the supplied handlers until the host signals end-of-stdin (non-PTY), the
// connection errors, or the caller sets a read deadline at teardown.
//
// It uses blocking reads with NO per-read deadline. The previous implementation
// wrapped each read in a 500ms SetReadDeadline poll; when that deadline fired
// mid-frame, io.ReadFull had already consumed bytes from the stream that
// ReadMessage then discarded, permanently desynchronizing the framed binary
// protocol (issue #222: Zed Remote-SSH's raw binary pipe). Blocking reads cannot
// lose bytes that way.
//
// The caller runs this in a goroutine and, after the process exits, unblocks it
// with conn.SetReadDeadline(time.Now()) and then waits for it to return.
//
// INVARIANT: this function only reads from conn and writes to the process. It
// must never write to conn — otherwise the teardown read deadline racing a
// concurrent writeExitCode would become a data race on the connection.
func pumpClientMessages(conn net.Conn, h messageHandlers) {
	for {
		msgType, data, err := readMessage(conn)
		if err != nil {
			return
		}
		switch msgType {
		case MsgTypeData:
			if len(data) > 0 && h.onData != nil {
				h.onData(data)
			}
		case MsgTypeResize:
			var resize ResizeMessage
			if err := json.Unmarshal(data, &resize); err != nil {
				log.Printf("Warning: failed to unmarshal resize message: %v", err)
			} else if h.onResize != nil {
				h.onResize(resize.Rows, resize.Cols)
			}
		case MsgTypeSignal:
			var sig SignalMessage
			if err := json.Unmarshal(data, &sig); err != nil {
				log.Printf("Warning: failed to unmarshal signal message: %v", err)
			} else if sig.Signal < 1 || sig.Signal > 64 {
				log.Printf("Warning: invalid signal number %d", sig.Signal)
			} else if h.onSignal != nil {
				h.onSignal(sig.Signal)
			}
		case MsgTypeStdinEOF:
			// Client signaled end of stdin. In non-PTY mode this closes the
			// stdin pipe (via the caller's deferred stdin.Close) so the command
			// sees EOF; in PTY mode there is no separate stdin pipe, so ignore it.
			if h.stopOnStdinEOF {
				return
			}
		default:
			// Unknown/future message type: log and skip rather than writing it
			// into the child process.
			log.Printf("Ignoring unknown message type on exec channel: 0x%02x", msgType)
		}
	}
}

// signalProcess returns an onSignal handler that forwards a signal number to the
// process, logging any failure. Shared by the PTY and non-PTY input pumps.
func signalProcess(cmd *exec.Cmd) func(int) {
	return func(sig int) {
		if err := cmd.Process.Signal(syscall.Signal(sig)); err != nil {
			log.Printf("Warning: failed to send signal %d: %v", sig, err)
		}
	}
}

// clientPump runs pumpClientMessages in a goroutine and provides an ordered
// teardown. stop() sets a read deadline to unblock the pump's blocking read and
// then waits for the goroutine to exit. The deadline-before-wait ordering is the
// load-bearing invariant — waiting first would block forever on a pump still in a
// blocking read — so it is encapsulated here rather than open-coded at each call
// site.
type clientPump struct {
	conn net.Conn
	wg   sync.WaitGroup
}

// startClientPump spawns the input pump for conn. If cleanup is non-nil it runs
// once, after the pump returns and before the goroutine exits (e.g. closing the
// process stdin pipe so the command sees EOF).
func startClientPump(conn net.Conn, h messageHandlers, cleanup func()) *clientPump {
	p := &clientPump{conn: conn}
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		if cleanup != nil {
			defer cleanup()
		}
		pumpClientMessages(conn, h)
	}()
	return p
}

// stop unblocks the pump's blocking read and waits for the goroutine to exit. It
// should be called once, after the process has exited and the agent no longer
// needs to read client input.
func (p *clientPump) stop() {
	_ = p.conn.SetReadDeadline(time.Now())
	p.wg.Wait()
}

// setEnv sets or replaces an environment variable in the given slice.
func setEnv(env []string, key, value string) []string {
	prefix := key + "="
	for i, e := range env {
		if strings.HasPrefix(e, prefix) {
			env[i] = prefix + value
			return env
		}
	}
	return append(env, prefix+value)
}
