package egress

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// Manager is shed-server's handle to the egress proxy child process. It owns
// the proxy's lifecycle (spawn/kill), the control/audit Client, and per-shed
// listener-port allocation from the configured range. It is tag-free and does
// NOT import config (config already imports this package for profile
// validation), so it traffics in primitives + the egress package's own types.
type Manager struct {
	mu     sync.Mutex
	client *Client
	proc   *exec.Cmd // the proxy child; nil when a Client is injected (tests)
	socket string

	portLo, portHi int
	used           map[int]string // port -> owning shed (ownership, not just membership)
	sheds          map[string]int // shed -> port (so Remove need not be told the port)

	tokenGen func() (string, error) // injectable for deterministic tests
}

// StartManager spawns the shed-egress-proxy child, waits for its control
// socket, dials it, and returns a ready Manager. Fails closed: if the binary
// is missing or the socket never comes up, the child is killed and an error is
// returned so shed-server treats egress-enabled-but-unstartable as a startup
// failure (never a silent disable).
func StartManager(socketPath, proxyBin string, portLo, portHi int, onAudit func(AuditRecord)) (*Manager, error) {
	if portLo <= 0 || portHi < portLo {
		return nil, fmt.Errorf("egress: invalid port range %d-%d", portLo, portHi)
	}
	cmd := exec.Command(proxyBin, "--control-socket", socketPath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	setPdeathsig(cmd) // Linux: die with shed-server; macOS relies on the proxy's getppid poll
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("egress: start proxy %s: %w", proxyBin, err)
	}
	client, err := dialControlRetry(socketPath, onAudit, 5*time.Second)
	if err != nil {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		return nil, fmt.Errorf("egress: dial proxy control socket %s: %w", socketPath, err)
	}
	return newManager(client, cmd, socketPath, portLo, portHi), nil
}

// newManager builds a Manager around an already-connected Client. proc may be
// nil (tests inject an in-process Client and supervise no child).
func newManager(client *Client, proc *exec.Cmd, socket string, portLo, portHi int) *Manager {
	return &Manager{
		client:   client,
		proc:     proc,
		socket:   socket,
		portLo:   portLo,
		portHi:   portHi,
		used:     map[int]string{},
		sheds:    map[string]int{},
		tokenGen: randToken,
	}
}

// Configure opens (or replaces) a shed's listener. A zero port allocates a free
// one from the range; a non-zero port reuses the caller's persisted assignment
// (restart re-push). An empty token mints a fresh one. On a proxy-side failure
// the freshly reserved port is released and the error is returned so the
// orchestrator hook aborts the create/start. Returns the effective port+token
// for the caller to persist in shed metadata.
func (m *Manager) Configure(shed string, port int, token, gateway string, specs []ProfileSpec) (int, string, error) {
	m.mu.Lock()
	if port == 0 {
		p, err := m.allocPortLocked(shed)
		if err != nil {
			m.mu.Unlock()
			return 0, "", err
		}
		port = p
	} else if owner, taken := m.used[port]; taken && owner != shed {
		// Reusing a persisted port that another shed already owns (e.g.
		// hand-edited metadata) would corrupt allocation — reject it.
		m.mu.Unlock()
		return 0, "", fmt.Errorf("egress: port %d already reserved by shed %q", port, owner)
	} else {
		m.used[port] = shed
		m.sheds[shed] = port
	}
	if token == "" {
		t, err := m.tokenGen()
		if err != nil {
			m.releaseLocked(shed, port)
			m.mu.Unlock()
			return 0, "", fmt.Errorf("egress: mint token for %s: %w", shed, err)
		}
		token = t
	}
	m.mu.Unlock()

	if err := m.client.Configure(shed, port, token, gateway, specs); err != nil {
		m.mu.Lock()
		m.releaseLocked(shed, port)
		m.mu.Unlock()
		return 0, "", err
	}
	return port, token, nil
}

// releaseLocked frees a port reservation iff this shed actually owns it (so a
// failed reuse never frees another shed's live reservation). Caller holds m.mu.
func (m *Manager) releaseLocked(shed string, port int) {
	if m.used[port] == shed {
		delete(m.used, port)
		delete(m.sheds, shed)
	}
}

// Remove closes a shed's listener but KEEPS its per-shed port reserved, so a
// stopped shed reopens on the same port at restart and no other shed can be
// allocated that port in the meantime. Idempotent. Used on shed STOP.
func (m *Manager) Remove(shed string) error {
	return m.client.Remove(shed)
}

// Release closes a shed's listener AND frees its port reservation. Idempotent
// (the proxy's Remove is a no-op for an unknown shed). Used on shed DELETE and
// on `egress off` — the shed no longer needs the port.
func (m *Manager) Release(shed string) error {
	m.mu.Lock()
	if port, ok := m.sheds[shed]; ok {
		delete(m.used, port)
		delete(m.sheds, shed)
	}
	m.mu.Unlock()
	return m.client.Remove(shed)
}

func (m *Manager) allocPortLocked(shed string) (int, error) {
	for p := m.portLo; p <= m.portHi; p++ {
		if _, taken := m.used[p]; !taken {
			m.used[p] = shed
			m.sheds[shed] = p
			return p, nil
		}
	}
	return 0, fmt.Errorf("egress: no free listener port in range %d-%d", m.portLo, m.portHi)
}

// Close shuts down the control channel and terminates the proxy child (SIGTERM,
// then SIGKILL if it does not exit promptly).
func (m *Manager) Close() error {
	if m.client != nil {
		_ = m.client.Close()
	}
	if m.proc != nil && m.proc.Process != nil {
		_ = m.proc.Process.Signal(syscall.SIGTERM)
		done := make(chan struct{})
		go func() { _, _ = m.proc.Process.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			_ = m.proc.Process.Kill()
		}
	}
	return nil
}

func dialControlRetry(path string, onAudit func(AuditRecord), timeout time.Duration) (*Client, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		c, err := DialControl(path, onAudit)
		if err == nil {
			return c, nil
		}
		lastErr = err
		if time.Now().After(deadline) {
			return nil, lastErr
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func randToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// Fail closed: a guessable (e.g. time-seeded) token would weaken the
		// only thing binding the listener port to this shed. crypto/rand
		// failing is near-impossible, so aborting the configure is acceptable.
		return "", err
	}
	return hex.EncodeToString(b), nil
}
