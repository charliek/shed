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

	// Set working directory
	workDir := req.WorkingDir
	if workDir == "" {
		workDir = "/workspace"
	}
	if _, err := os.Stat(workDir); err == nil {
		cmd.Dir = workDir
	} else {
		cmd.Dir = "/"
	}

	// Build environment: system env + request-provided env
	env := append(os.Environ(), req.Env...)

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

	stopCh := make(chan struct{})

	// Handle resize messages in background
	go func() {
		for {
			msgType, data, err := readMessageWithTimeout(conn, 500*time.Millisecond)
			if err != nil {
				if isTimeout(err) {
					select {
					case <-stopCh:
						return
					default:
						continue
					}
				}
				return
			}

			switch msgType {
			case MsgTypeResize:
				var resize ResizeMessage
				if err := json.Unmarshal(data, &resize); err != nil {
					log.Printf("Warning: failed to unmarshal resize message: %v", err)
				} else if err := pty.Setsize(ptmx, &pty.Winsize{
					Rows: resize.Rows,
					Cols: resize.Cols,
				}); err != nil {
					log.Printf("Warning: failed to resize PTY: %v", err)
				}
			case MsgTypeSignal:
				var sig SignalMessage
				if err := json.Unmarshal(data, &sig); err != nil {
					log.Printf("Warning: failed to unmarshal signal message: %v", err)
				} else if sig.Signal < 1 || sig.Signal > 64 {
					log.Printf("Warning: invalid signal number %d", sig.Signal)
				} else if err := cmd.Process.Signal(syscall.Signal(sig.Signal)); err != nil {
					log.Printf("Warning: failed to send signal %d: %v", sig.Signal, err)
				}
			case MsgTypeStdinEOF:
				// Ignored in PTY mode — PTY doesn't have a separate stdin pipe
			default:
				// Data from stdin
				if len(data) > 0 {
					if _, err := ptmx.Write(data); err != nil {
						log.Printf("Warning: failed to write to PTY: %v", err)
					}
				}
			}
		}
	}()

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

	// Wait for output to be flushed before sending exit code
	outputWg.Wait()

	close(stopCh)
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

	done := make(chan struct{})

	// Copy stdin from connection messages
	go func() {
		defer stdin.Close()
		for {
			msgType, data, err := readMessageWithTimeout(conn, 500*time.Millisecond)
			if err != nil {
				if isTimeout(err) {
					select {
					case <-done:
						return
					default:
						continue
					}
				}
				return
			}
			switch msgType {
			case MsgTypeSignal:
				var sig SignalMessage
				if err := json.Unmarshal(data, &sig); err != nil {
					log.Printf("Warning: failed to unmarshal signal message: %v", err)
				} else if sig.Signal < 1 || sig.Signal > 64 {
					log.Printf("Warning: invalid signal number %d", sig.Signal)
				} else if err := cmd.Process.Signal(syscall.Signal(sig.Signal)); err != nil {
					log.Printf("Warning: failed to send signal %d: %v", sig.Signal, err)
				}
			case MsgTypeStdinEOF:
				// Client signaled end of stdin; close the pipe so the
				// command sees EOF (e.g. tar xzpf - finishes reading).
				return
			case MsgTypeResize:
				// Resize is only meaningful for PTY sessions; ignore in non-PTY mode.
			default:
				if len(data) > 0 {
					if _, err := stdin.Write(data); err != nil {
						log.Printf("Warning: failed to write to stdin: %v", err)
					}
				}
			}
		}
	}()

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

	close(done)
	connMu.Lock()
	err = writeExitCode(conn, exitCode)
	connMu.Unlock()
	if err != nil {
		log.Printf("Warning: failed to write exit code: %v", err)
	}
	log.Printf("Command exited with code %d", exitCode)
}

func readMessageWithTimeout(conn net.Conn, timeout time.Duration) (byte, []byte, error) {
	if err := conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		return 0, nil, err
	}
	return readMessage(conn)
}

func isTimeout(err error) bool {
	if err == nil {
		return false
	}
	if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
		return true
	}
	return false
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
