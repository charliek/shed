package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/tunnels"
)

// readyFDEnv names the inherited pipe fd the detached worker writes its startup
// result to. spawnTunnelDaemon passes the pipe as ExtraFiles[0], which the child
// sees as fd 3.
const readyFDEnv = "SHED_TUNNEL_READY_FD"

// daemonReadyTimeout bounds how long the parent waits for the worker to report
// it is listening. It must cover the worker's config load + findShedServer +
// ensureRunningShed + the startup Connect auth probe (tunnelProbeTimeout) +
// listener bind; the parent already started the shed, so those are fast, but be
// generous.
const daemonReadyTimeout = 15 * time.Second

// tunnelProbeTimeout bounds the startup Connect auth probe. It stays well under
// daemonReadyTimeout so a failing probe can report ERR: to the parent before the
// parent gives up and kills the worker with a generic readiness timeout.
const tunnelProbeTimeout = 5 * time.Second

// probeConnectAuth checks that the tunnel target can authenticate to the Connect
// API (see tunnels.ConnectClient.Probe), bounded by tunnelProbeTimeout. Both the
// foreground and detached-worker start paths run it before binding listeners, so
// an auth/transport failure fails `shed tunnels start` loudly instead of
// surfacing later as a per-connection reset.
func probeConnectAuth(target tunnels.ConnectTarget, shedName string) error {
	ctx, cancel := context.WithTimeout(context.Background(), tunnelProbeTimeout)
	defer cancel()
	return tunnels.NewConnectClient(target).Probe(ctx, shedName)
}

// spawnTunnelDaemon re-execs this binary as a detached background worker for the
// given shed, waits for it to report that the tunnels are listening, then
// returns — handing the terminal back while the worker keeps running. The
// worker re-resolves its connect target from config, so the bearer token never
// appears on its command line.
func spawnTunnelDaemon(shedName string, ports []tunnels.PortMapping) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate shed binary: %w", err)
	}

	if err := os.MkdirAll(config.GetTunnelLogDir(), 0700); err != nil {
		return fmt.Errorf("create tunnel log dir: %w", err)
	}
	logPath := config.GetTunnelLogPath(shedName)
	// O_TRUNC: each start resets the per-shed log. The log is error-only and not
	// rotated (see docs/reference/tunnels.md).
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return fmt.Errorf("open tunnel log %s: %w", logPath, err)
	}
	defer logFile.Close()

	devNull, err := os.OpenFile(os.DevNull, os.O_RDONLY, 0)
	if err != nil {
		return fmt.Errorf("open %s: %w", os.DevNull, err)
	}
	defer devNull.Close()

	readyR, readyW, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("create readiness pipe: %w", err)
	}
	defer readyR.Close()

	cmd := exec.Command(exe, daemonChildArgs(os.Args[1:])...)
	cmd.Stdin = devNull
	cmd.Stdout = logFile
	cmd.Stderr = logFile // same *os.File as Stdout: one fd, no interleaving
	cmd.ExtraFiles = []*os.File{readyW}
	cmd.Env = append(os.Environ(), readyFDEnv+"=3")
	// New session: detach from the controlling terminal so closing it (SIGHUP)
	// doesn't kill the tunnel. Deliberately no Pdeathsig — unlike the egress
	// proxy, this child must outlive its parent.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := cmd.Start(); err != nil {
		readyW.Close()
		return fmt.Errorf("start tunnel daemon: %w", err)
	}
	// The child holds its own copy of the write end; close ours so our read
	// unblocks with EOF if the child dies without reporting.
	readyW.Close()

	if err := awaitDaemonReady(readyR, daemonReadyTimeout); err != nil {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		return fmt.Errorf("%w (see %s)", err, logPath)
	}

	printSuccess("Tunnel started for %s (PID %d)", shedName, cmd.Process.Pid)
	fmt.Println("Forwarding:")
	for _, pm := range ports {
		fmt.Printf("  localhost:%d -> %s:%d\n", pm.Local, shedName, pm.Remote)
	}
	fmt.Printf("Logs: %s\n", logPath)
	return nil
}

// daemonChildArgs builds the argv for the detached worker: the parent's args
// with exactly one hidden --daemon flag. Any existing --daemon is dropped first
// (so a stray re-exec can't compound it), and the flag is inserted before a "--"
// terminator if present so cobra parses it as a flag, not a positional.
func daemonChildArgs(args []string) []string {
	filtered := make([]string, 0, len(args)+1)
	afterTerminator := false
	for _, a := range args {
		if a == "--" {
			afterTerminator = true
			filtered = append(filtered, a)
			continue
		}
		// Only dedupe the hidden flag among real flags; after "--" it's a
		// positional value Cobra must keep verbatim.
		if !afterTerminator && a == "--daemon" {
			continue
		}
		filtered = append(filtered, a)
	}
	for i, a := range filtered {
		if a == "--" {
			return append(filtered[:i:i], append([]string{"--daemon"}, filtered[i:]...)...)
		}
	}
	return append(filtered, "--daemon")
}

// awaitDaemonReady reads one readiness line from the worker, bounded by timeout.
// A bare EOF (no line) means the worker died during startup.
func awaitDaemonReady(r *os.File, timeout time.Duration) error {
	type result struct {
		line string
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		// *os.File deadlines aren't reliable on pipes, so read in a goroutine
		// and race it against the timeout instead.
		line, err := bufio.NewReader(r).ReadString('\n')
		ch <- result{line: line, err: err}
	}()

	select {
	case res := <-ch:
		if strings.TrimSpace(res.line) == "" {
			return fmt.Errorf("tunnel daemon exited before reporting readiness")
		}
		return parseReadyMessage(res.line)
	case <-time.After(timeout):
		return fmt.Errorf("tunnel daemon did not report readiness within %s", timeout)
	}
}

// parseReadyMessage interprets the worker's readiness line: "OK" means the
// tunnels are listening; "ERR:<msg>" carries a startup failure.
func parseReadyMessage(line string) error {
	line = strings.TrimSpace(line)
	switch {
	case line == "OK":
		return nil
	case strings.HasPrefix(line, "ERR:"):
		return fmt.Errorf("tunnel daemon failed: %s", strings.TrimSpace(strings.TrimPrefix(line, "ERR:")))
	default:
		return fmt.Errorf("tunnel daemon sent unexpected readiness message %q", line)
	}
}

// runDaemonWorker is the detached worker (the --daemon re-exec). It starts the
// tunnels, records state under its own detached PID, reports the outcome to the
// parent over the inherited pipe, then blocks until signalled and tears down.
// The ordering is load-bearing: listeners bound -> state saved -> "OK" written,
// so `shed tunnels list` immediately after the parent returns sees the entry.
func runDaemonWorker(mgr *tunnels.Manager, shedName, serverName string, target tunnels.ConnectTarget, ports []tunnels.PortMapping, profile string) error {
	ready, err := openReadyPipe()
	if err != nil {
		return err
	}

	// Validate Connect auth before binding listeners or reporting readiness, so a
	// broken tunnel fails `shed tunnels start` in the user's terminal (via the
	// ERR: channel) rather than looking healthy and resetting every connection.
	if probeErr := probeConnectAuth(target, shedName); probeErr != nil {
		fmt.Fprintf(ready, "ERR:%s\n", probeErr)
		_ = ready.Close()
		return probeErr
	}

	// Bind listeners, then record state under our own (detached) PID.
	activeTunnels, startErr := mgr.StartTunnels(target, shedName, ports)
	if startErr != nil {
		startErr = fmt.Errorf("failed to start tunnels: %w", startErr)
	} else if saveErr := mgr.SaveBackground(shedName, serverName, profile, os.Getpid(), ports); saveErr != nil {
		for _, t := range activeTunnels {
			t.Stop()
		}
		startErr = fmt.Errorf("failed to save tunnel state: %w", saveErr)
	}
	if startErr != nil {
		fmt.Fprintf(ready, "ERR:%s\n", startErr)
		_ = ready.Close()
		return startErr
	}

	if _, err := fmt.Fprintln(ready, "OK"); err != nil {
		// The parent is gone or the pipe broke; tear down rather than leak an
		// untracked daemon.
		stopDaemonTunnels(mgr, shedName, activeTunnels)
		_ = ready.Close()
		return fmt.Errorf("signal readiness: %w", err)
	}
	_ = ready.Close()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()

	stopDaemonTunnels(mgr, shedName, activeTunnels)
	return nil
}

// openReadyPipe returns the inherited readiness pipe, or an error if this
// process wasn't launched as a worker — so a manual `--daemon` invocation fails
// fast instead of hanging on or writing to a bogus fd.
func openReadyPipe() (*os.File, error) {
	v := os.Getenv(readyFDEnv)
	if v == "" {
		return nil, fmt.Errorf("--daemon is internal and must be launched by `shed tunnels start -d` (%s not set)", readyFDEnv)
	}
	fd, err := strconv.Atoi(v)
	if err != nil {
		return nil, fmt.Errorf("invalid %s=%q: %w", readyFDEnv, v, err)
	}
	return os.NewFile(uintptr(fd), "tunnel-ready"), nil
}

func stopDaemonTunnels(mgr *tunnels.Manager, shedName string, activeTunnels []*tunnels.Tunnel) {
	for _, t := range activeTunnels {
		t.Stop()
	}
	if err := mgr.RemoveOwnedTunnel(shedName, os.Getpid()); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to clear tunnel state: %v\n", err)
	}
}
