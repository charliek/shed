package main

import (
	"encoding/json"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"sync"
	"syscall"

	"github.com/creack/pty"
)

// handleExecConnection handles a connection on the console port.
func handleExecConnection(conn net.Conn) {
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
		writeExitCode(conn, 1)
		return
	}

	// Validate command
	if len(req.Cmd) == 0 {
		req.Cmd = []string{"/bin/bash", "--login"}
	}

	log.Printf("Executing: %v (TTY: %v)", req.Cmd, req.TTY)

	// Create the command
	cmd := exec.Command(req.Cmd[0], req.Cmd[1:]...)

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

	// Set environment
	cmd.Env = append(os.Environ(), req.Env...)
	cmd.Env = append(cmd.Env, "TERM=xterm-256color")

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
		writeExitCode(conn, 1)
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
	done := make(chan error, 1)

	// WaitGroup to ensure output is flushed before sending exit code
	var outputWg sync.WaitGroup

	// Handle resize messages in background
	go func() {
		for {
			msgType, data, err := readMessage(conn)
			if err != nil {
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
		done <- cmd.Wait()
	}()

	err = <-done
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

	writeExitCode(conn, exitCode)
	log.Printf("Command exited with code %d", exitCode)
}

// runWithoutPTY runs a command without a PTY.
func runWithoutPTY(conn net.Conn, cmd *exec.Cmd) {
	// Set up pipes
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		log.Printf("Failed to create stdout pipe: %v", err)
		writeExitCode(conn, 1)
		return
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		log.Printf("Failed to create stderr pipe: %v", err)
		writeExitCode(conn, 1)
		return
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		log.Printf("Failed to create stdin pipe: %v", err)
		writeExitCode(conn, 1)
		return
	}

	// Start command
	if err := cmd.Start(); err != nil {
		log.Printf("Failed to start command: %v", err)
		writeExitCode(conn, 1)
		return
	}

	// WaitGroup to ensure output is flushed before sending exit code
	var outputWg sync.WaitGroup

	// Copy stdin from connection messages
	go func() {
		defer stdin.Close()
		for {
			msgType, data, err := readMessage(conn)
			if err != nil {
				return
			}
			if msgType == MsgTypeSignal {
				var sig SignalMessage
				if err := json.Unmarshal(data, &sig); err != nil {
					log.Printf("Warning: failed to unmarshal signal message: %v", err)
				} else if sig.Signal < 1 || sig.Signal > 64 {
					log.Printf("Warning: invalid signal number %d", sig.Signal)
				} else if err := cmd.Process.Signal(syscall.Signal(sig.Signal)); err != nil {
					log.Printf("Warning: failed to send signal %d: %v", sig.Signal, err)
				}
			} else if len(data) > 0 {
				if _, err := stdin.Write(data); err != nil {
					log.Printf("Warning: failed to write to stdin: %v", err)
				}
			}
		}
	}()

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
				if err := writeData(conn, buf[:n]); err != nil {
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
				if err := writeData(conn, buf[:n]); err != nil {
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

	writeExitCode(conn, exitCode)
	log.Printf("Command exited with code %d", exitCode)
}
