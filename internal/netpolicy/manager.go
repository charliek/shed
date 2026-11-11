package netpolicy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/charliek/shed/internal/config"
)

// Runner runs nft with stdin and args and returns its combined output.
type Runner interface {
	Run(ctx context.Context, stdin string, args ...string) ([]byte, error)
}

// availability is implemented by a Runner that can tell, without running
// anything, whether its binary exists. The Manager uses it so a server that
// needs no rules makes no calls at all when nft is missing.
type availability interface {
	Available() bool
}

// ExecRunner runs Bin with Prefix followed by the nft arguments. The default
// (Bin "nft", no prefix) runs nft directly; the network-namespace test uses
// Bin "ip" with Prefix "netns exec <ns> nft".
type ExecRunner struct {
	Bin    string
	Prefix []string
}

// Run implements Runner.
func (r ExecRunner) Run(ctx context.Context, stdin string, args ...string) ([]byte, error) {
	bin := r.Bin
	if bin == "" {
		bin = "nft"
	}
	cmd := exec.CommandContext(ctx, bin, append(append([]string(nil), r.Prefix...), args...)...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	return out.Bytes(), err
}

// Available reports whether the binary is on PATH (or the path exists).
func (r ExecRunner) Available() bool {
	bin := r.Bin
	if bin == "" {
		bin = "nft"
	}
	_, err := exec.LookPath(bin)
	return err == nil
}

// applyTimeout bounds one nft invocation. nft -f of two small tables takes
// milliseconds; this only keeps a wedged nft from holding the Manager's lock
// (and so every create and start) forever.
const applyTimeout = 15 * time.Second

// Manager holds the desired per-shed state of one server's rule set and
// applies it. Every change re-renders and applies both tables in full, in one
// transaction, under the Manager's lock, so applies happen in the order of the
// changes and the kernel always ends up holding the latest state that applied.
//
// A failed apply leaves the desired state changed (the next successful apply,
// or the presence check, installs it); callers decide what a failure means
// (a create or start fails, a delete only logs). Errors wrap
// config.ErrNetworkEnforcementUnavailableSentinel.
type Manager struct {
	cfg            Config
	runner         Runner
	defaultProfile Profile

	mu    sync.Mutex
	sheds map[string]Shed // guarded sheds only
}

// NewManager returns a Manager for cfg that applies through runner (nil: nft
// on PATH). defaultProfile is the server's network.default_profile; a non-open
// default means the tables are needed even with no guarded shed yet.
func NewManager(cfg Config, runner Runner, defaultProfile Profile) *Manager {
	if runner == nil {
		runner = ExecRunner{}
	}
	if defaultProfile == "" {
		defaultProfile = Open
	}
	return &Manager{cfg: cfg, runner: runner, defaultProfile: defaultProfile, sheds: map[string]Shed{}}
}

// TableName is the name of this server's two tables.
func (m *Manager) TableName() string { return TableName(m.cfg.ServerName) }

// Needed reports whether this server needs its tables: a guarded shed exists,
// or the default profile is not open.
func (m *Manager) Needed() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.neededLocked()
}

func (m *Manager) neededLocked() bool {
	return len(m.sheds) > 0 || m.defaultProfile.Guarded()
}

// IsGuarded reports whether name is a guarded shed in the desired state.
func (m *Manager) IsGuarded(name string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.sheds[name]
	return ok
}

// Guarded returns the guarded sheds in the desired state, sorted by name.
func (m *Manager) Guarded() []Shed {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Shed, 0, len(m.sheds))
	for _, s := range m.sheds {
		s.P9Ports = slices.Clone(s.P9Ports)
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Upsert sets a shed's entry (all of it, ports included) and applies. An open
// shed is removed instead.
func (m *Manager) Upsert(ctx context.Context, s Shed) error {
	if !s.Profile.Guarded() {
		return m.Remove(ctx, s.Name)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	s.P9Ports = slices.Clone(s.P9Ports)
	m.sheds[s.Name] = s
	return m.applyLocked(ctx)
}

// Remove drops a shed's entry and applies. Removing a shed that is not guarded
// changes nothing and runs nothing.
func (m *Manager) Remove(ctx context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.sheds[name]; !ok {
		return nil
	}
	delete(m.sheds, name)
	return m.applyLocked(ctx)
}

// SetEgressPort sets a guarded shed's egress-proxy port (0: none) and applies
// when it changed. A shed that is not guarded is ignored.
func (m *Manager) SetEgressPort(ctx context.Context, name string, port uint16) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sheds[name]
	if !ok || s.EgressPort == port {
		return nil
	}
	s.EgressPort = port
	m.sheds[name] = s
	return m.applyLocked(ctx)
}

// SetP9Ports sets a guarded shed's 9P server ports and applies when they
// changed. A shed that is not guarded is ignored.
func (m *Manager) SetP9Ports(ctx context.Context, name string, ports []uint16) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sheds[name]
	if !ok {
		return nil
	}
	next := slices.Clone(ports)
	slices.Sort(next)
	cur := slices.Clone(s.P9Ports)
	slices.Sort(cur)
	if slices.Equal(cur, next) {
		return nil
	}
	s.P9Ports = next
	m.sheds[name] = s
	return m.applyLocked(ctx)
}

// Rebuild replaces the whole desired state with sheds (open ones are
// ignored) and brings the kernel in line: when the tables are needed they are
// applied; when they are not, stale tables of this server are deleted. With
// nft missing and nothing needed it runs nothing and succeeds.
func (m *Manager) Rebuild(ctx context.Context, sheds []Shed) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sheds = map[string]Shed{}
	for _, s := range sheds {
		if s.Profile.Guarded() {
			s.P9Ports = slices.Clone(s.P9Ports)
			m.sheds[s.Name] = s
		}
	}
	if m.neededLocked() {
		return m.applyLocked(ctx)
	}
	if !m.available() {
		return nil
	}
	present, err := m.anyPresentLocked(ctx)
	if err != nil {
		return err
	}
	if !present {
		return nil
	}
	return m.teardownLocked(ctx)
}

// Reapply re-renders and applies the current desired state (the presence
// check's repair). It does nothing when the tables are not needed.
func (m *Manager) Reapply(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.neededLocked() {
		return nil
	}
	return m.applyLocked(ctx)
}

// Present reports whether both of this server's tables exist.
func (m *Manager) Present(ctx context.Context) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, family := range []string{"bridge", "inet"} {
		ok, err := m.tableExistsLocked(ctx, family)
		if err != nil || !ok {
			return false, err
		}
	}
	return true, nil
}

// Teardown deletes this server's two tables, and no other. A missing table,
// or a missing nft binary, is success.
func (m *Manager) Teardown(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.available() {
		return nil
	}
	return m.teardownLocked(ctx)
}

func (m *Manager) available() bool {
	if a, ok := m.runner.(availability); ok {
		return a.Available()
	}
	return true
}

func (m *Manager) enforcementErr(format string, args ...any) error {
	return fmt.Errorf("%w: %s", config.ErrNetworkEnforcementUnavailableSentinel, fmt.Sprintf(format, args...))
}

func (m *Manager) applyLocked(ctx context.Context) error {
	if !m.neededLocked() {
		// Nothing guarded and an open default: the tables go (when nft is
		// there to have installed them at all).
		if !m.available() {
			return nil
		}
		return m.teardownLocked(ctx)
	}
	if !m.available() {
		return m.enforcementErr("nft is not installed; it is needed to enforce non-open network profiles")
	}
	sheds := make([]Shed, 0, len(m.sheds))
	for _, s := range m.sheds {
		sheds = append(sheds, s)
	}
	script, err := Render(m.cfg, sheds)
	if err != nil {
		return m.enforcementErr("%v", err)
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), applyTimeout)
	defer cancel()
	if out, err := m.runner.Run(ctx, script, "-f", "-"); err != nil {
		return m.enforcementErr("nft -f: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (m *Manager) teardownLocked(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), applyTimeout)
	defer cancel()
	if out, err := m.runner.Run(ctx, RenderTeardown(m.cfg.ServerName), "-f", "-"); err != nil {
		if isNotFound(err) {
			return nil
		}
		return m.enforcementErr("nft teardown: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (m *Manager) anyPresentLocked(ctx context.Context) (bool, error) {
	for _, family := range []string{"bridge", "inet"} {
		ok, err := m.tableExistsLocked(ctx, family)
		if err != nil {
			return false, err
		}
		if ok {
			return true, nil
		}
	}
	return false, nil
}

// tableExistsLocked lists one table. nft exits non-zero when it is missing;
// an nft that cannot run at all is an error.
func (m *Manager) tableExistsLocked(ctx context.Context, family string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), applyTimeout)
	defer cancel()
	_, err := m.runner.Run(ctx, "", "list", "table", family, m.TableName())
	if err == nil {
		return true, nil
	}
	// An nft that ran and exited non-zero (exec.ExitError has ExitCode) is
	// a missing table.
	var exited interface{ ExitCode() int }
	if errors.As(err, &exited) {
		return false, nil
	}
	return false, m.enforcementErr("nft list table %s %s: %v", family, m.TableName(), err)
}

func isNotFound(err error) bool {
	return errors.Is(err, exec.ErrNotFound)
}
