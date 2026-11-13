// Package locallink is the transport that needs no network: each link runs a
// real shed-agent process on the server's own host, listening on unix sockets
// in a private directory, and dials those sockets directly.
//
// It exists for tests and for the development driver (remote.transport:
// local, which the config refuses unless remote.allow_test_drivers is set).
// It is not isolation of any kind: the agent runs commands on the server's
// host, as the server's user.
//
// The agent is started on the first Dial and again on the first Dial after it
// has exited, so a link survives its agent being killed. Close stops the
// agent and removes the directory.
//
// The agent is Linux-only, so a link works only on Linux; the package builds
// everywhere.
package locallink

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/charliek/shed/internal/remote/guest"
	"github.com/charliek/shed/internal/remote/transport"
)

const (
	// DefaultStartTimeout bounds how long a started agent may take to
	// listen.
	DefaultStartTimeout = 10 * time.Second

	// stopGrace is how long Close waits after SIGTERM before SIGKILL.
	stopGrace = 3 * time.Second

	// maxSocketPath is the longest unix socket path Linux binds.
	maxSocketPath = 107

	// socketSubdir is the agent's socket directory inside a link's
	// directory. The agent creates it, mode 0700.
	socketSubdir = "s"
)

// Config configures a Factory.
type Config struct {
	// AgentPath is a shed-agent binary built for this host. Required.
	AgentPath string
	// BaseDir is where each link makes its private directory. Default
	// os.TempDir(), or /tmp when that is too deep for a socket path.
	BaseDir string
	// StartTimeout bounds how long a started agent may take to listen.
	// Default DefaultStartTimeout.
	StartTimeout time.Duration
	// MinStartInterval is the spacing of failed starts. Default
	// transport.MinConnectInterval.
	MinStartInterval time.Duration
	// Logf receives agent lifecycle events. Default log.Printf.
	Logf func(format string, args ...any)
}

// Factory makes local links. It implements transport.Factory.
type Factory struct {
	cfg Config
}

var _ transport.Factory = (*Factory)(nil)

// New returns a Factory. It fails on a host that cannot run the agent.
func New(cfg Config) (*Factory, error) {
	if runtime.GOOS != "linux" {
		return nil, fmt.Errorf("locallink: the local transport runs shed-agent on this host, which needs Linux (this is %s)", runtime.GOOS)
	}
	if cfg.AgentPath == "" {
		return nil, errors.New("locallink: no agent binary path")
	}
	st, err := os.Stat(cfg.AgentPath)
	if err != nil {
		return nil, fmt.Errorf("locallink: agent binary: %w", err)
	}
	if st.IsDir() || st.Mode()&0o111 == 0 {
		return nil, fmt.Errorf("locallink: agent binary %s is not an executable file", cfg.AgentPath)
	}
	if cfg.StartTimeout <= 0 {
		cfg.StartTimeout = DefaultStartTimeout
	}
	if cfg.MinStartInterval <= 0 {
		cfg.MinStartInterval = transport.MinConnectInterval
	}
	if cfg.Logf == nil {
		cfg.Logf = log.Printf
	}
	return &Factory{cfg: cfg}, nil
}

// Name returns transport.NameLocal.
func (f *Factory) Name() string { return transport.NameLocal }

// Open returns a link for the target. It starts nothing: the agent is started
// by the first Dial. Target.Addr and Target.HostKeys are not used.
func (f *Factory) Open(_ context.Context, target transport.Target) (transport.Link, error) {
	return &Link{
		cfg:    f.cfg,
		target: target,
		conns:  make(map[*conn]struct{}),
	}, nil
}

// Link is one local agent. It implements transport.Link.
type Link struct {
	cfg    Config
	target transport.Target

	// startMu serialises starting the agent. It is held across the wait
	// for the agent to listen.
	startMu sync.Mutex

	mu       sync.Mutex
	closed   bool
	dir      string // the link's private directory; made on first start
	proc     *process
	lastFail time.Time
	lastErr  error
	conns    map[*conn]struct{}
}

var _ transport.Link = (*Link)(nil)

// process is one run of the agent.
type process struct {
	cmd      *exec.Cmd
	pid      int
	httpPort int
	exited   chan struct{} // closed when the process has been reaped
}

func (p *process) alive() bool {
	select {
	case <-p.exited:
		return false
	default:
		return true
	}
}

// conn is a stream to the agent: the unix socket conn itself (half-close and
// deadlines included), tracked so Close on the link can end it.
type conn struct {
	*net.UnixConn
	link *Link
	once sync.Once
}

func (c *conn) Close() error {
	c.once.Do(func() { c.link.forget(c) })
	return c.UnixConn.Close()
}

func (l *Link) name() string {
	if l.target.ShedName != "" {
		return l.target.ShedName
	}
	if l.target.ShedID != "" {
		return l.target.ShedID
	}
	return "local"
}

// Dial opens a stream to the agent socket for port, starting the agent if it
// is not running. See transport.Link.
func (l *Link) Dial(ctx context.Context, port uint32) (net.Conn, error) {
	sockDir, err := l.agent(ctx)
	if err != nil {
		return nil, err
	}
	var d net.Dialer
	raw, err := d.DialContext(ctx, "unix", transport.SocketPath(sockDir, port))
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if l.isClosed() {
			return nil, transport.ErrClosed
		}
		return nil, fmt.Errorf("locallink: %s: %w: %w", l.name(), err, transport.ErrUnreachable)
	}
	uc, ok := raw.(*net.UnixConn)
	if !ok {
		raw.Close()
		return nil, fmt.Errorf("locallink: unexpected conn type %T", raw)
	}

	c := &conn{UnixConn: uc, link: l}
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		uc.Close()
		return nil, transport.ErrClosed
	}
	l.conns[c] = struct{}{}
	l.mu.Unlock()
	return c, nil
}

func (l *Link) forget(c *conn) {
	l.mu.Lock()
	delete(l.conns, c)
	l.mu.Unlock()
}

func (l *Link) isClosed() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.closed
}

// Close stops the agent, ends every stream and removes the link's directory.
func (l *Link) Close() error {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return nil
	}
	l.closed = true
	conns := l.conns
	l.conns = map[*conn]struct{}{}
	l.mu.Unlock()

	for c := range conns {
		_ = c.UnixConn.Close()
	}

	// Wait for a start in progress, so its process is not missed.
	l.startMu.Lock()
	defer l.startMu.Unlock()
	l.mu.Lock()
	p, dir := l.proc, l.dir
	l.proc, l.dir = nil, ""
	l.mu.Unlock()

	if p != nil {
		stop(p)
	}
	if dir != "" {
		_ = os.RemoveAll(dir)
	}
	return nil
}

// stop ends an agent: SIGTERM, then SIGKILL after stopGrace.
func stop(p *process) {
	if !p.alive() {
		return
	}
	_ = p.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-p.exited:
	case <-time.After(stopGrace):
		_ = p.cmd.Process.Kill()
		<-p.exited
	}
}

// agent returns the socket directory of the running agent, starting one if
// none runs. Failed starts are spaced MinStartInterval apart.
func (l *Link) agent(ctx context.Context) (string, error) {
	if sockDir, ok, err := l.running(); ok || err != nil {
		return sockDir, err
	}

	l.startMu.Lock()
	defer l.startMu.Unlock()
	if sockDir, ok, err := l.running(); ok || err != nil {
		return sockDir, err
	}

	l.mu.Lock()
	if !l.lastFail.IsZero() && time.Since(l.lastFail) < l.cfg.MinStartInterval {
		err := l.lastErr
		l.mu.Unlock()
		return "", err
	}
	l.mu.Unlock()

	p, dir, err := l.start(ctx)

	l.mu.Lock()
	defer l.mu.Unlock()
	if err != nil {
		if ctx.Err() == nil {
			l.lastFail, l.lastErr = time.Now(), err
		}
		return "", err
	}
	l.lastFail, l.lastErr = time.Time{}, nil
	l.proc = p
	return filepath.Join(dir, socketSubdir), nil
}

// running returns the live agent's socket directory. ok is false, with a nil
// error, when an agent must be started.
func (l *Link) running() (sockDir string, ok bool, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return "", false, transport.ErrClosed
	}
	if l.proc != nil && l.proc.alive() {
		return filepath.Join(l.dir, socketSubdir), true, nil
	}
	return "", false, nil
}

// start runs the agent and waits until it listens. The caller holds startMu.
func (l *Link) start(ctx context.Context) (*process, string, error) {
	dir, err := l.ensureDir()
	if err != nil {
		return nil, "", fmt.Errorf("locallink: %s: %w: %w", l.name(), err, transport.ErrUnreachable)
	}
	sockDir := filepath.Join(dir, socketSubdir)
	layoutFile := filepath.Join(dir, "layout")
	logPath := filepath.Join(dir, "agent.log")

	// The agent reports the layout of the "image" it runs in. Here that is
	// this server's own layout.
	if err := os.WriteFile(layoutFile, []byte(strconv.Itoa(guest.Layout)+"\n"), 0o644); err != nil {
		return nil, "", fmt.Errorf("locallink: %s: %w: %w", l.name(), err, transport.ErrUnreachable)
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, "", fmt.Errorf("locallink: %s: %w: %w", l.name(), err, transport.ErrUnreachable)
	}
	defer logFile.Close()

	// The agent's plugin HTTP API defaults to a privileged port. Give it a
	// free one; a start that loses the race for it fails and is retried.
	httpPort, err := freePort()
	if err != nil {
		return nil, "", fmt.Errorf("locallink: %s: %w: %w", l.name(), err, transport.ErrUnreachable)
	}

	cmd := exec.Command(l.cfg.AgentPath,
		"--listen", "unix:"+sockDir,
		"--layout-file", layoutFile,
		"--http-port", strconv.Itoa(httpPort),
		"--shed-name", l.name(),
	)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Dir = dir
	cmd.SysProcAttr = agentProcAttr()

	p := &process{cmd: cmd, httpPort: httpPort, exited: make(chan struct{})}
	started := make(chan error, 1)
	go func() {
		// The parent-death signal is tied to the thread that forks, so
		// this goroutine keeps its thread for the agent's whole life.
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		if err := cmd.Start(); err != nil {
			started <- err
			return
		}
		started <- nil
		_ = cmd.Wait()
		close(p.exited)
	}()
	if err := <-started; err != nil {
		return nil, "", fmt.Errorf("locallink: %s: start %s: %w: %w", l.name(), l.cfg.AgentPath, err, transport.ErrUnreachable)
	}
	p.pid = cmd.Process.Pid

	if err := l.waitListening(ctx, p, sockDir); err != nil {
		stop(p)
		if ctx.Err() != nil {
			return nil, "", ctx.Err()
		}
		return nil, "", fmt.Errorf("locallink: %s: %w%s: %w", l.name(), err, logTail(logPath), transport.ErrUnreachable)
	}
	l.cfg.Logf("locallink: %s: agent running, pid %d, sockets in %s", l.name(), p.pid, sockDir)
	return p, dir, nil
}

// waitListening waits until the agent's three sockets exist and its HTTP API
// accepts a connection. The HTTP listener is the last thing the agent starts,
// so once it answers, the sockets are bound, listening and have their final
// modes.
func (l *Link) waitListening(ctx context.Context, p *process, sockDir string) error {
	deadline := time.Now().Add(l.cfg.StartTimeout)
	ports := []uint32{transport.PortConsole, transport.PortNotify, transport.PortTCPProxy}
	for {
		ready := true
		for _, port := range ports {
			if _, err := os.Stat(transport.SocketPath(sockDir, port)); err != nil {
				ready = false
				break
			}
		}
		if ready {
			c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(p.httpPort)), time.Second)
			if err == nil {
				c.Close()
				return nil
			}
		}
		if !p.alive() {
			return errors.New("the agent exited while starting")
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the agent did not listen within %v", l.cfg.StartTimeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-p.exited:
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// ensureDir makes the link's private directory once.
func (l *Link) ensureDir() (string, error) {
	l.mu.Lock()
	dir := l.dir
	l.mu.Unlock()
	if dir != "" {
		return dir, nil
	}

	base := l.cfg.BaseDir
	if base == "" {
		base = os.TempDir()
		// A deep TMPDIR would push the socket paths past the kernel's
		// limit. "shed-local-" plus the random suffix is under 32
		// bytes, the socket name under 16.
		if len(base)+48+len(socketSubdir) > maxSocketPath {
			base = "/tmp"
		}
	}
	dir, err := os.MkdirTemp(base, "shed-local-")
	if err != nil {
		return "", err
	}
	if longest := transport.SocketPath(filepath.Join(dir, socketSubdir), transport.PortTCPProxy); len(longest) > maxSocketPath {
		_ = os.RemoveAll(dir)
		return "", fmt.Errorf("socket path %s is %d bytes; the limit is %d (set a shorter base directory)", longest, len(longest), maxSocketPath)
	}

	l.mu.Lock()
	l.dir = dir
	l.mu.Unlock()
	return dir, nil
}

// freePort asks the kernel for a free loopback TCP port.
func freePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

// logTail returns the end of the agent's log, for a start error.
func logTail(path string) string {
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		return ""
	}
	const keep = 2048
	if len(data) > keep {
		data = data[len(data)-keep:]
	}
	return "; agent log: " + strings.TrimSpace(string(data))
}

// AgentPID returns the running agent's process ID, or 0 when none runs. Tests
// use it to kill the agent.
func (l *Link) AgentPID() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.proc == nil || !l.proc.alive() {
		return 0
	}
	return l.proc.pid
}

// HTTPPort returns the loopback port of the running agent's plugin HTTP API,
// or 0 when none runs. It changes when the agent is restarted.
func (l *Link) HTTPPort() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.proc == nil || !l.proc.alive() {
		return 0
	}
	return l.proc.httpPort
}

// SocketDir returns the directory of the agent's sockets, or "" before the
// first start. Tests point an SSH server's forwarding at it.
func (l *Link) SocketDir() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.dir == "" {
		return ""
	}
	return filepath.Join(l.dir, socketSubdir)
}

// LogPath returns the agent's log file, or "" before the first start.
func (l *Link) LogPath() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.dir == "" {
		return ""
	}
	return filepath.Join(l.dir, "agent.log")
}
