//go:build linux
// +build linux

package main

import (
	"encoding/json"
	"errors"
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

// connKeepaliveInterval is how often the exec paths probe the host connection
// with a zero-length frame to detect a disconnect for commands that otherwise
// neither read stdin nor write output (see startKeepalive).
const connKeepaliveInterval = 15 * time.Second

// connKeepaliveWriteTimeout bounds a single keepalive probe write so a stalled
// connection can't wedge teardown (which waits for the keepalive goroutine).
const connKeepaliveWriteTimeout = 5 * time.Second

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

	// On host disconnect, SIGHUP the command's process group (pty.Start put it in
	// its own session) so it terminates instead of orphaning. Detected via the
	// output-write path below, the pump's read error, and the keepalive probe —
	// so even a perfectly idle PTY session is cleaned up on a lost host.
	disconnect := onHostDisconnect(cmd)

	// w serializes the output copier and the keepalive probe (both write conn)
	// and SIGHUPs the command on a write failure.
	w := &connWriter{conn: conn, disconnect: disconnect}

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
		onSignal:     signalProcess(cmd),
		onDisconnect: disconnect,
		// onStdinEOF left nil: a PTY has no separate stdin pipe.
	}, nil)

	// Copy PTY output to connection.
	outputWg.Add(1)
	go func() { defer outputWg.Done(); copyToConn(w, ptmx, "PTY") }()

	// Probe for a host disconnect even when the PTY session is idle (no output);
	// stopped after the command exits.
	keepaliveStop := startKeepalive(w)

	// Wait for command to exit
	go func() {
		exitCh <- cmd.Wait()
	}()

	err = <-exitCh
	exitCode := exitCodeFromWait(err)

	// Close the PTY master to unblock the output goroutine. Without this,
	// ptmx.Read() can block indefinitely after the process exits because
	// the PTY master doesn't always deliver EOF promptly on Linux. This may
	// discard a small unread tail still buffered in the line discipline — a
	// truncation-vs-hang tradeoff accepted for interactive PTY sessions. Do NOT
	// switch this to "drain before close" like the non-PTY path: the master's
	// lack of a prompt EOF would reintroduce the indefinite hang.
	ptmx.Close()

	// Wait for output to be flushed before sending exit code
	outputWg.Wait()

	// Stop the keepalive (it probed until the command exited) so it can't write
	// conn concurrently with writeExitCode below.
	keepaliveStop()

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

	// Run the command in its own process group so a host disconnect can SIGHUP
	// the whole tree (the command plus anything it spawned) instead of orphaning
	// it. Any Credential set by the caller (for the shed user) is preserved.
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true

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

	// On host disconnect, SIGHUP the command's process group so it terminates
	// instead of orphaning (and leaking this handler at cmd.Wait below).
	disconnect := onHostDisconnect(cmd)

	// Forward client→process stdin using blocking reads. On MsgTypeStdinEOF the
	// stdin pipe is closed (once) so the command sees EOF (e.g. `tar xzpf -`
	// finishes reading) — but the pump keeps running so it can still forward
	// signals and detect a host disconnect via its read error after stdin closes.
	// The same close runs as the pump's teardown cleanup; sync.Once makes it
	// idempotent (cmd.Wait also closes the pipe). Resize frames are ignored in
	// non-PTY mode because onResize is left nil. The pump is unblocked and awaited
	// at teardown via pump.stop().
	var closeStdinOnce sync.Once
	closeStdin := func() { closeStdinOnce.Do(func() { _ = stdin.Close() }) }
	pump := startClientPump(conn, messageHandlers{
		onData: func(b []byte) {
			if _, err := stdin.Write(b); err != nil {
				log.Printf("Warning: failed to write to stdin: %v", err)
			}
		},
		onSignal:     signalProcess(cmd),
		onDisconnect: disconnect,
		onStdinEOF:   closeStdin,
	}, closeStdin)

	// w serializes the stdout/stderr copiers and the keepalive probe (all write
	// conn) and SIGHUPs the command on a write failure.
	w := &connWriter{conn: conn, disconnect: disconnect}

	// Copy stdout and stderr to the connection (folded into one stream).
	outputWg.Add(2)
	go func() { defer outputWg.Done(); copyToConn(w, stdout, "stdout") }()
	go func() { defer outputWg.Done(); copyToConn(w, stderr, "stderr") }()

	// Probe for a host disconnect even when the command produces no output and
	// reads no stdin (see startKeepalive). Stopped after cmd.Wait below so it
	// keeps probing until the process actually exits.
	keepaliveStop := startKeepalive(w)

	// Drain stdout/stderr to EOF BEFORE reaping the process. cmd.Wait closes the
	// StdoutPipe/StderrPipe read ends, discarding any output still buffered in
	// the OS pipe; calling it before the copy goroutines finish truncates large
	// outputs under back-pressure (the pipes hit EOF on their own when the
	// process exits and closes its write ends). Per the os/exec StdoutPipe docs:
	// "It is thus incorrect to call Wait before all reads from the pipe have
	// completed."
	outputWg.Wait()

	// Reap the process. The keepalive keeps probing the conn until the process
	// actually EXITS — not merely until output drains — so a disconnect is still
	// caught for a command that closed its own stdout/stderr but kept running
	// (e.g. `exec >/dev/null 2>&1; sleep 600`), where outputWg.Wait returns early.
	// (For commands that hold their output pipes open, outputWg.Wait already
	// blocks until exit.)
	err = cmd.Wait()
	exitCode := exitCodeFromWait(err)

	// Now that the process has exited, stop the keepalive and the input pump,
	// waiting for the keepalive to return, so neither can write conn concurrently
	// with writeExitCode below.
	keepaliveStop()
	pump.stop()

	// writeExitCode is the sole remaining conn writer: outputWg.Wait +
	// keepaliveStop above guarantee every other writer (the copiers and the probe
	// on w) has returned, and the input pump only ever reads conn.
	if err := writeExitCode(conn, exitCode); err != nil {
		log.Printf("Warning: failed to write exit code: %v", err)
	}
	log.Printf("Command exited with code %d", exitCode)
}

// exitCodeFromWait maps a cmd.Wait() error to a process exit code: 0 for nil,
// the process's real code for an *exec.ExitError, and 1 for any other failure.
func exitCodeFromWait(err error) int {
	if err == nil {
		return 0
	}
	if exitError, ok := err.(*exec.ExitError); ok {
		return exitError.ExitCode()
	}
	return 1
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
//   - onDisconnect fires once when the read loop ends because the host closed the
//     connection (a non-timeout read error), so the caller can terminate the
//     command rather than orphaning it. It is NOT called for the teardown read
//     deadline (a timeout) the caller sets after the process has already exited.
//
// stopOnStdinEOF is true for the non-PTY path (closing the stdin pipe lets the
// process see EOF) and false for the PTY path (a PTY has no separate stdin pipe,
// so MsgTypeStdinEOF is ignored).
type messageHandlers struct {
	onData       func([]byte)
	onResize     func(rows, cols uint16)
	onSignal     func(sig int)
	onDisconnect func()
	// onStdinEOF, if set, is called when the host signals end-of-stdin
	// (MsgTypeStdinEOF) — the non-PTY path closes the stdin pipe here. It does
	// NOT stop the pump: a command can keep running after stdin closes and must
	// still receive forwarded signals and have a later host disconnect detected
	// via the read-error path. PTY mode leaves this nil (no separate stdin pipe).
	onStdinEOF func()
}

// pumpClientMessages reads framed messages from the host and dispatches each to
// the supplied handlers until the connection errors (host disconnect) or the
// caller sets a read deadline at teardown. End-of-stdin closes the stdin pipe
// (via onStdinEOF) but does NOT stop the pump.
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
			// A non-timeout read error means the host closed the connection
			// mid-session. The teardown path (clientPump.stop) instead sets a
			// read DEADLINE, which surfaces as a timeout and must NOT be treated
			// as a disconnect — the process has already exited in that case.
			if h.onDisconnect != nil && !isTimeoutError(err) {
				h.onDisconnect()
			}
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
			// Client signaled end of stdin. Non-PTY closes the stdin pipe (so the
			// command sees EOF) but the pump keeps running for later signals and
			// disconnect detection; PTY has no separate stdin pipe (onStdinEOF nil).
			if h.onStdinEOF != nil {
				h.onStdinEOF()
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

// isTimeoutError reports whether err is a deadline timeout (as opposed to a
// connection-closed or other read error). Used to distinguish the teardown read
// deadline from a host disconnect.
func isTimeoutError(err error) bool {
	var nerr net.Error
	return errors.As(err, &nerr) && nerr.Timeout()
}

// terminateGroup sends sig to the command's entire process group. The child runs
// in its own process group (Setpgid on the non-PTY path; Setsid via pty.Start on
// the PTY path), so the negative PID reaches the command and everything it
// spawned. No-op if the process never started.
func terminateGroup(cmd *exec.Cmd, sig syscall.Signal) {
	if cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, sig)
}

// onHostDisconnect returns a func that, once, SIGHUPs the command's process
// group so a host disconnect terminates the command (and its children) instead
// of orphaning it and leaking this handler's goroutine — matching standard SSH
// "connection hung up" semantics. Use tmux/nohup to intentionally survive a
// disconnect.
//
// Callers stop the disconnect sources (pump, keepalive) immediately after
// cmd.Wait, so a call after reap is a tiny race rather than fully safe:
// kill(-pid) on a freed process group is normally a no-op (ESRCH), but in the
// microscopic window before those sources stop, a reused PID that became a group
// leader could in theory be signaled. The window is negligible in practice.
func onHostDisconnect(cmd *exec.Cmd) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			if cmd.Process == nil {
				return
			}
			log.Printf("Host disconnected; sending SIGHUP to command group (pid %d)", cmd.Process.Pid)
			terminateGroup(cmd, syscall.SIGHUP)
		})
	}
}

// connWriter serializes all writes to the host connection (the output copiers
// and the keepalive probe on both the PTY and non-PTY paths) and fires
// disconnect() on the first write failure — the host is gone, so the command
// should be terminated rather than left orphaned.
type connWriter struct {
	mu         sync.Mutex
	conn       net.Conn
	disconnect func()
}

// write sends data to the host, firing disconnect on a write error.
func (w *connWriter) write(data []byte) error {
	w.mu.Lock()
	err := writeData(w.conn, data)
	w.mu.Unlock()
	if err != nil {
		w.disconnect()
	}
	return err
}

// writeProbe sends a bounded zero-length keepalive frame. A stalled conn that
// can't accept even 5 bytes within timeout is treated as a disconnect, so a
// wedged write can't hang teardown (which waits for the keepalive goroutine).
func (w *connWriter) writeProbe(timeout time.Duration) error {
	w.mu.Lock()
	err := w.conn.SetWriteDeadline(time.Now().Add(timeout))
	if err == nil {
		err = writeData(w.conn, nil)
	}
	_ = w.conn.SetWriteDeadline(time.Time{}) // clear for other writers
	w.mu.Unlock()
	if err != nil {
		w.disconnect()
	}
	return err
}

// copyToConn copies r to the host connection via w until EOF or a write failure,
// labeling read/write errors with name. Used for the process stdout/stderr pipes
// (non-PTY) and the PTY master.
func copyToConn(w *connWriter, r io.Reader, name string) {
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if werr := w.write(buf[:n]); werr != nil {
				log.Printf("Warning: failed to write %s to connection: %v", name, werr)
				return
			}
		}
		if err != nil {
			if err != io.EOF {
				log.Printf("%s read error: %v", name, err)
			}
			return
		}
	}
}

// startKeepalive periodically probes w with a zero-length frame so a host
// disconnect is detected even for a command that produces no output and reads no
// stdin: the guest's vsock read never delivers EOF on the host's close, and the
// output-write path only fires for commands that emit something. The host treats
// a zero-length frame as an empty stdout write — harmless to the byte stream.
// Returns stop(), which ends the probe and waits for it to exit so it can't
// write conn concurrently with a later writeExitCode. Used by both paths.
func startKeepalive(w *connWriter) (stop func()) {
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(connKeepaliveInterval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				if w.writeProbe(connKeepaliveWriteTimeout) != nil {
					return
				}
			}
		}
	}()
	return func() {
		close(done)
		wg.Wait()
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
