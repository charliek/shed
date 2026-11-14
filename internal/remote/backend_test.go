package remote

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charliek/shed/internal/agentproto"
	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/plugin"
	"github.com/charliek/shed/internal/remote/guest"
	"github.com/charliek/shed/internal/remote/provider"
	"github.com/charliek/shed/internal/remote/provider/fake"
	"github.com/charliek/shed/internal/remote/transport"
	"github.com/charliek/shed/internal/remote/transport/conformance"
	"github.com/charliek/shed/internal/remote/transport/locallink"
	"github.com/charliek/shed/internal/vmutil"
)

const (
	testEnv     = "test"
	testKey     = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIserverkeyserverkeyserverkeyserverkeyserverke"
	testHostKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIhostkeyhostkeyhostkeyhostkeyhostkeyhostkey00"
	testTimeout = 20 * time.Second
)

// --- a link factory with an in-process agent that answers probes ---

type fakeLinks struct {
	mu sync.Mutex
	// sticky is the error every Dial for a shed returns, by shed name.
	sticky map[string]error
	// queue holds errors returned by the next Dials for a shed, in order.
	queue map[string][]error
	// info is what the agent reports.
	info guest.AgentInfo
	// presented, when set, makes each Dial run the host key check the SSH
	// link runs: Pin, then Learn when there is no pin.
	presented string
	dials     int
}

func newFakeLinks() *fakeLinks {
	return &fakeLinks{
		sticky: make(map[string]error),
		queue:  make(map[string][]error),
		info:   guest.AgentInfo{Version: "test", Protocol: guest.AgentProtocol, Layout: guest.Layout},
	}
}

func (f *fakeLinks) Name() string { return "fake" }

func (f *fakeLinks) Open(_ context.Context, target transport.Target) (transport.Link, error) {
	return &fakeLink{f: f, target: target}, nil
}

func (f *fakeLinks) fail(name string, errs ...error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queue[name] = append(f.queue[name], errs...)
}

func (f *fakeLinks) failAlways(name string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sticky[name] = err
}

type fakeLink struct {
	f      *fakeLinks
	target transport.Target
	mu     sync.Mutex
	closed bool
}

func (l *fakeLink) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.closed = true
	return nil
}

func (l *fakeLink) Dial(ctx context.Context, port uint32) (net.Conn, error) {
	l.mu.Lock()
	closed := l.closed
	l.mu.Unlock()
	if closed {
		return nil, transport.ErrClosed
	}
	f := l.f
	f.mu.Lock()
	f.dials++
	name := l.target.ShedName
	var err error
	if q := f.queue[name]; len(q) > 0 {
		err, f.queue[name] = q[0], q[1:]
	} else {
		err = f.sticky[name]
	}
	info, presented := f.info, f.presented
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if presented != "" {
		if _, err := l.target.Addr(ctx); err != nil {
			return nil, fmt.Errorf("%w: %w", err, transport.ErrUnreachable)
		}
		pin, err := l.target.HostKeys.Pin(ctx)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", err, transport.ErrUnreachable)
		}
		switch {
		case pin == "":
			if err := l.target.HostKeys.Learn(ctx, presented); err != nil {
				return nil, fmt.Errorf("%w: %w", err, transport.ErrNoHostKey)
			}
		case pin != presented:
			return nil, transport.ErrHostKeyMismatch
		}
	}
	if port != transport.PortNotify {
		return nil, errors.New("the fake agent has no console")
	}
	client, server := net.Pipe()
	go serveHealth(server, info)
	return client, nil
}

// serveHealth answers every health request on the connection, the way the
// agent's notify port does.
func serveHealth(conn net.Conn, info guest.AgentInfo) {
	defer conn.Close()
	for {
		msgType, data, err := agentproto.ReadMessage(conn)
		if err != nil {
			return
		}
		if msgType != agentproto.MsgTypePluginMessage {
			continue
		}
		var env plugin.Envelope
		if json.Unmarshal(data, &env) != nil || env.Namespace != plugin.NamespaceHealth {
			continue
		}
		payload, _ := json.Marshal(info)
		reply, _ := json.Marshal(plugin.NewResponse(env.ID, plugin.NamespaceHealth, payload))
		if agentproto.WriteMessage(conn, agentproto.MsgTypePluginMessage, reply) != nil {
			return
		}
	}
}

// --- test environment ---

type env struct {
	t     *testing.T
	prov  *fake.Provider
	links transport.Factory
	fl    *fakeLinks
	dir   string
	cfg   *config.RemoteConfig
	b     *Backend

	logMu sync.Mutex
	logs  []string

	// wrap, when set, wraps the provider the backend is given.
	wrap func(provider.Provider) provider.Provider
	// tune, when set, edits the backend's options.
	tune func(*Options)

	autoStop chan struct{}
	autoDone chan struct{}
}

type envOpt func(*env, *fake.Options)

func manual() envOpt { return func(_ *env, o *fake.Options) { o.Manual = true } }

func limits(running, sheds int) envOpt {
	return func(e *env, _ *fake.Options) {
		e.cfg.Limits = config.RemoteLimitsConfig{MaxRunning: running, MaxSheds: sheds}
	}
}

func newEnv(t *testing.T, opts ...envOpt) *env {
	t.Helper()
	e := &env{
		t:   t,
		fl:  newFakeLinks(),
		dir: t.TempDir(),
		cfg: &config.RemoteConfig{
			Provider:     config.RemoteProviderFake,
			Transport:    config.RemoteTransportSSH,
			Environment:  testEnv,
			DefaultImage: "shed-image-1",
			DefaultSize:  provider.SizeStandard,
			Sizes: map[string]config.RemoteSizeConfig{
				provider.SizeStandard: {MachineType: "e2-standard-2", CPUs: 2, MemoryMB: 8192, Arch: provider.ArchAMD64},
			},
			DiskGB:         50,
			Limits:         config.RemoteLimitsConfig{MaxRunning: 8, MaxSheds: 16},
			MaxRunDuration: config.Duration(24 * time.Hour),
		},
	}
	e.links = e.fl
	fo := fake.Options{Env: testEnv}
	for _, o := range opts {
		o(e, &fo)
	}
	e.prov = fake.New(fo)
	e.b = e.open()
	t.Cleanup(func() {
		e.stopAuto()
		_ = e.b.Close()
	})
	return e
}

func (e *env) logf(format string, args ...any) {
	e.logMu.Lock()
	defer e.logMu.Unlock()
	e.logs = append(e.logs, fmt.Sprintf(format, args...))
}

func (e *env) logLines() []string {
	e.logMu.Lock()
	defer e.logMu.Unlock()
	return append([]string(nil), e.logs...)
}

// open builds a backend over the environment's state directory and provider
// and runs its start-up pass.
func (e *env) open() *Backend {
	e.t.Helper()
	var prov provider.Provider = e.prov
	if e.wrap != nil {
		prov = e.wrap(e.prov)
	}
	o := Options{
		Config:              e.cfg,
		Provider:            prov,
		Links:               e.links,
		StateDir:            e.dir,
		AuthorizedKeys:      []string{testKey},
		FirewallSources:     []string{fake.DefaultFirewallSource},
		FastFailWindow:      60 * time.Millisecond,
		BootTimeout:         2 * time.Second,
		AuthGrace:           300 * time.Millisecond,
		ProbeInterval:       5 * time.Millisecond,
		HostKeyPollInterval: 20 * time.Millisecond,
		CloneRetryPause:     time.Millisecond,
		// The periodic pass runs only when a test calls Sweep, and a delete
		// marker with no job is retried whatever its age.
		SweepInterval:  -1,
		DeleteRetryAge: time.Nanosecond,
		Logf:           e.logf,
	}
	if e.tune != nil {
		e.tune(&o)
	}
	b, err := New(o)
	if err != nil {
		e.t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	go b.Reconcile(ctx)
	if err := b.WaitReady(ctx); err != nil {
		e.t.Fatalf("WaitReady: %v", err)
	}
	return b
}

// restart closes the backend and opens a new one over the same state
// directory and the same cloud.
func (e *env) restart() {
	e.t.Helper()
	if err := e.b.Close(); err != nil {
		e.t.Fatalf("Close: %v", err)
	}
	e.b = e.open()
}

func ctxT(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	t.Cleanup(cancel)
	return ctx
}

func (e *env) ref(name string) provider.Ref {
	e.t.Helper()
	insts, err := e.prov.List(context.Background(), provider.Selector{Env: testEnv})
	if err != nil {
		e.t.Fatalf("List: %v", err)
	}
	for _, inst := range insts {
		if inst.Labels[provider.LabelName] == name {
			return inst.Ref
		}
	}
	return ""
}

// finish completes the cloud operation in flight on a shed's instance,
// waiting for one to appear.
func (e *env) finish(name string) {
	e.t.Helper()
	deadline := time.Now().Add(testTimeout)
	for time.Now().Before(deadline) {
		if ref := e.ref(name); ref != "" && e.prov.Complete(ref) {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	e.t.Fatalf("no cloud operation appeared on shed %s", name)
}

// auto completes every cloud operation as it appears, until stopAuto.
func (e *env) auto() {
	if e.autoStop != nil {
		return
	}
	e.autoStop, e.autoDone = make(chan struct{}), make(chan struct{})
	go func() {
		defer close(e.autoDone)
		for {
			select {
			case <-e.autoStop:
				return
			case <-time.After(2 * time.Millisecond):
			}
			insts, _ := e.prov.List(context.Background(), provider.Selector{})
			for _, inst := range insts {
				e.prov.Complete(inst.Ref)
			}
		}
	}()
}

func (e *env) stopAuto() {
	if e.autoStop == nil {
		return
	}
	close(e.autoStop)
	<-e.autoDone
	e.autoStop = nil
}

func (e *env) create(name string) (*config.Shed, error) {
	return e.b.CreateShed(ctxT(e.t), config.CreateShedRequest{Name: name, NoProvision: true})
}

func (e *env) settled(name string) *config.Shed {
	e.t.Helper()
	shed, err := e.b.WaitSettled(ctxT(e.t), name)
	if err != nil {
		e.t.Fatalf("WaitSettled(%s): %v", name, err)
	}
	return shed
}

func (e *env) idle() {
	e.t.Helper()
	if err := e.b.WaitIdle(ctxT(e.t)); err != nil {
		e.t.Fatalf("WaitIdle: %v", err)
	}
}

func (e *env) status(name string) string {
	e.t.Helper()
	shed, err := e.b.GetShed(context.Background(), name)
	if err != nil {
		e.t.Fatalf("GetShed(%s): %v", name, err)
	}
	return shed.Status
}

// put brings a new shed into the given status on a manual provider and
// leaves it there.
func (e *env) put(name, status string) {
	e.t.Helper()
	if status == config.StatusError {
		e.prov.FailOperation(fake.OpCreate, errors.New("the insert operation failed"))
	}
	shed, err := e.create(name)
	if err != nil {
		e.t.Fatalf("create %s: %v", name, err)
	}
	if shed.Status != config.StatusStarting || shed.ID == "" {
		e.t.Fatalf("create %s returned status %q id %q, want starting with an id", name, shed.Status, shed.ID)
	}
	if status == config.StatusStarting {
		return
	}
	e.finish(name)
	if got := e.settled(name).Status; status == config.StatusError || status == config.StatusRunning {
		if got != status {
			e.t.Fatalf("shed %s settled %q, want %q", name, got, status)
		}
		return
	}
	if _, err := e.b.StopShed(ctxT(e.t), name); err != nil {
		e.t.Fatalf("stop %s: %v", name, err)
	}
	if status == config.StatusStopping {
		return
	}
	e.finish(name)
	if got := e.settled(name).Status; got != config.StatusStopped {
		e.t.Fatalf("shed %s settled %q, want stopped", name, got)
	}
}

func markerCount(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(dir, "deleting"))
	if err != nil {
		t.Fatalf("read markers: %v", err)
	}
	return len(entries)
}

// --- the call table ---

func TestCallTable(t *testing.T) {
	statuses := []string{config.StatusStarting, config.StatusRunning, config.StatusStopping, config.StatusStopped, config.StatusError}
	type cell struct {
		call    string
		from    string
		wantErr error
		// wantNow is the status the call's reply reports; wantEnd is where
		// the shed rests once every cloud operation has finished.
		wantNow string
		wantEnd string
	}
	cells := []cell{
		{"start", config.StatusStarting, nil, config.StatusStarting, config.StatusRunning},
		{"start", config.StatusRunning, config.ErrShedAlreadyRunningSentinel, "", config.StatusRunning},
		{"start", config.StatusStopping, nil, config.StatusStopping, config.StatusRunning},
		{"start", config.StatusStopped, nil, config.StatusStarting, config.StatusRunning},
		{"start", config.StatusError, config.ErrShedNotStoppedSentinel, "", config.StatusError},
		{"stop", config.StatusStarting, nil, config.StatusStarting, config.StatusStopped},
		{"stop", config.StatusRunning, nil, config.StatusStopping, config.StatusStopped},
		{"stop", config.StatusStopping, nil, config.StatusStopping, config.StatusStopped},
		{"stop", config.StatusStopped, config.ErrShedNotRunningSentinel, "", config.StatusStopped},
		{"stop", config.StatusError, config.ErrShedNotRunningSentinel, "", config.StatusError},
	}
	for _, from := range statuses {
		cells = append(cells, cell{call: "delete", from: from})
	}

	for _, c := range cells {
		t.Run(c.call+"_on_"+c.from, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t, manual())
			const name = "shed-a"
			e.put(name, c.from)
			if got := e.status(name); got != c.from {
				t.Fatalf("set-up left the shed %q, want %q", got, c.from)
			}
			ctx := ctxT(t)

			if c.call == "delete" {
				if err := e.b.DeleteShed(ctx, name); err != nil {
					t.Fatalf("delete on %s: %v", c.from, err)
				}
				if _, err := e.b.GetShed(ctx, name); !errors.Is(err, config.ErrShedNotFoundSentinel) {
					t.Fatalf("GetShed right after delete = %v, want not found", err)
				}
				if sheds, _ := e.b.ListSheds(ctx); len(sheds) != 0 {
					t.Fatalf("ListSheds right after delete = %d sheds, want 0", len(sheds))
				}
				// WaitSettled lasts until the VM is gone, then reports the
				// shed not found. The provider is manual, so while an
				// instance exists the delete cannot have finished.
				if insts, _ := e.prov.List(ctx, provider.Selector{}); len(insts) != 0 {
					short, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
					_, err := e.b.WaitSettled(short, name)
					cancel()
					if !errors.Is(err, context.DeadlineExceeded) {
						t.Fatalf("WaitSettled while the VM still exists = %v, want it to block", err)
					}
				}
				e.auto()
				if _, err := e.b.WaitSettled(ctx, name); !errors.Is(err, config.ErrShedNotFoundSentinel) {
					t.Fatalf("WaitSettled after delete = %v, want not found", err)
				}
				if insts, _ := e.prov.List(ctx, provider.Selector{}); len(insts) != 0 {
					t.Fatalf("%d instances left when WaitSettled reported the shed gone", len(insts))
				}
				e.idle()
				if n := markerCount(t, e.dir); n != 0 {
					t.Fatalf("%d delete markers left after the delete finished", n)
				}
				return
			}

			var shed *config.Shed
			var err error
			if c.call == "start" {
				shed, err = e.b.StartShed(ctx, name)
			} else {
				shed, err = e.b.StopShed(ctx, name)
			}
			if c.wantErr != nil {
				if !errors.Is(err, c.wantErr) {
					t.Fatalf("%s on %s = %v, want %v", c.call, c.from, err, c.wantErr)
				}
			} else {
				if err != nil {
					t.Fatalf("%s on %s: %v", c.call, c.from, err)
				}
				if shed.Status != c.wantNow {
					t.Fatalf("%s on %s replied status %q, want %q", c.call, c.from, shed.Status, c.wantNow)
				}
			}
			e.auto()
			if got := e.settled(name); got.Status != c.wantEnd {
				t.Fatalf("%s on %s came to rest %q (%s), want %q", c.call, c.from, got.Status, got.StatusReason, c.wantEnd)
			}
		})
	}
}

func TestCreateDuplicateName(t *testing.T) {
	e := newEnv(t)
	if _, err := e.create("dup"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.create("dup"); !errors.Is(err, config.ErrShedAlreadyExistsSentinel) {
		t.Fatalf("second create = %v, want already exists", err)
	}
}

// --- create failures ---

func TestCreateFastFailLeavesNoRecord(t *testing.T) {
	tests := []struct {
		name    string
		arrange func(p *fake.Provider)
		want    error
	}{
		{"capacity from the operation", func(p *fake.Provider) {
			p.FailOperation(fake.OpCreate, provider.Errorf(provider.ErrCapacity, "wait", "zone is full"))
		}, config.ErrZoneCapacityUnavailableSentinel},
		{"quota from the call", func(p *fake.Provider) {
			p.FailNext(fake.OpCreate, provider.Errorf(provider.ErrQuota, "create", "CPUS"))
		}, config.ErrCloudQuotaExceededSentinel},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			tt.arrange(e.prov)
			_, err := e.create("fast")
			if !errors.Is(err, tt.want) {
				t.Fatalf("create = %v, want %v", err, tt.want)
			}
			if sheds, _ := e.b.ListSheds(context.Background()); len(sheds) != 0 {
				t.Fatalf("a refused create left %d records", len(sheds))
			}
			if entries, _ := os.ReadDir(filepath.Join(e.dir, "sheds")); len(entries) != 0 {
				t.Fatalf("a refused create left %d record files", len(entries))
			}
			e.idle()
			if _, err := e.create("fast"); err != nil {
				t.Fatalf("create after the refusal: %v", err)
			}
		})
	}
}

func TestCreateLateFailureLeavesError(t *testing.T) {
	tests := []struct {
		name       string
		arrange    func(e *env)
		wantReason string
	}{
		{"capacity", func(e *env) {
			e.prov.FailOperation(fake.OpCreate, provider.Errorf(provider.ErrCapacity, "wait", "zone is full"))
		}, config.StatusReasonCapacity},
		{"operation error", func(e *env) {
			e.prov.FailOperation(fake.OpCreate, errors.New("boom"))
		}, config.StatusReasonCreateFailed},
		{"host key mismatch", func(e *env) {
			e.fl.failAlways("late", transport.ErrHostKeyMismatch)
		}, config.StatusReasonHostKey},
		{"incompatible agent", func(e *env) {
			e.fl.info = guest.AgentInfo{Protocol: 0, Layout: 1}
		}, config.StatusReasonLayout},
		{"key refused for too long", func(e *env) {
			e.fl.failAlways("late", transport.ErrAuth)
		}, config.StatusReasonCreateFailed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t, manual())
			tt.arrange(e)
			shed, err := e.create("late")
			if err != nil || shed.Status != config.StatusStarting {
				t.Fatalf("create = %+v, %v; want starting", shed, err)
			}
			e.auto()
			got := e.settled("late")
			if got.Status != config.StatusError || got.StatusReason != tt.wantReason {
				t.Fatalf("settled %q (%q), want error (%q)", got.Status, got.StatusReason, tt.wantReason)
			}
			e.idle()
			// The failed create's VM is destroyed; the record outlives it.
			if insts, _ := e.prov.List(context.Background(), provider.Selector{}); len(insts) != 0 {
				t.Fatalf("%d instances left after a failed create", len(insts))
			}
			if _, err := e.b.StartShed(ctxT(t), "late"); !errors.Is(err, config.ErrShedNotStoppedSentinel) {
				t.Fatalf("start on an error shed = %v", err)
			}
			if err := e.b.DeleteShed(ctxT(t), "late"); err != nil {
				t.Fatalf("delete of an error shed: %v", err)
			}
		})
	}
}

func TestCreateRetriesEarlyAuthAndUnreachable(t *testing.T) {
	e := newEnv(t)
	unreachable := fmt.Errorf("dial: %w", transport.ErrUnreachable)
	auth := fmt.Errorf("handshake: %w", transport.ErrAuth)
	e.fl.fail("boot", unreachable, unreachable, auth, auth, unreachable)
	if _, err := e.create("boot"); err != nil {
		t.Fatal(err)
	}
	if got := e.settled("boot"); got.Status != config.StatusRunning {
		t.Fatalf("settled %q (%q), want running", got.Status, got.StatusReason)
	}
}

// --- start ---

func TestStartRefused(t *testing.T) {
	t.Run("capacity", func(t *testing.T) {
		e := newEnv(t, manual())
		e.put("cap", config.StatusStopped)
		e.prov.FailOperation(fake.OpStart, provider.Errorf(provider.ErrCapacity, "wait", "zone is full"))
		shed, err := e.b.StartShed(ctxT(t), "cap")
		if err != nil || shed.Status != config.StatusStarting {
			t.Fatalf("start = %+v, %v", shed, err)
		}
		e.auto()
		got := e.settled("cap")
		if got.Status != config.StatusStopped || got.StatusReason != config.StatusReasonCapacity {
			t.Fatalf("settled %q (%q), want stopped (capacity)", got.Status, got.StatusReason)
		}
		// A later start succeeds and clears the reason.
		if _, err := e.b.StartShed(ctxT(t), "cap"); err != nil {
			t.Fatal(err)
		}
		if got := e.settled("cap"); got.Status != config.StatusRunning || got.StatusReason != "" {
			t.Fatalf("second start settled %q (%q)", got.Status, got.StatusReason)
		}
	})
	t.Run("server limit", func(t *testing.T) {
		e := newEnv(t, manual(), limits(1, 8))
		e.put("one", config.StatusStopped)
		e.put("two", config.StatusRunning)
		_, err := e.b.StartShed(ctxT(t), "one")
		if !errors.Is(err, config.ErrServerLimitReachedSentinel) || !errors.Is(err, provider.ErrLimit) {
			t.Fatalf("start over max_running = %v, want the server limit", err)
		}
		got := e.settled("one")
		if got.Status != config.StatusStopped || got.StatusReason != config.StatusReasonLimit {
			t.Fatalf("shed is %q (%q), want stopped (limit)", got.Status, got.StatusReason)
		}
	})
	t.Run("run limit is passed to the provider", func(t *testing.T) {
		e := newEnv(t)
		e.cfg.MaxRunDuration = config.Duration(3 * time.Hour)
		if _, err := e.create("lim"); err != nil {
			t.Fatal(err)
		}
		e.settled("lim")
		inst, err := e.prov.Get(context.Background(), e.ref("lim"))
		if err != nil || inst.MaxRun != 3*time.Hour {
			t.Fatalf("instance run limit = %v, %v; want 3h", inst.MaxRun, err)
		}
	})
}

// --- delete ---

func TestDeleteDuringCreate(t *testing.T) {
	e := newEnv(t, manual())
	e.put("gone", config.StatusStarting)
	if err := e.b.DeleteShed(ctxT(t), "gone"); err != nil {
		t.Fatal(err)
	}
	if n := markerCount(t, e.dir); n != 1 {
		t.Fatalf("%d delete markers right after the delete, want 1", n)
	}
	// The name is free at once.
	if _, err := e.create("gone"); err != nil {
		t.Fatalf("create under the deleted shed's name: %v", err)
	}
	e.auto()
	if got := e.settled("gone"); got.Status != config.StatusRunning {
		t.Fatalf("the new shed settled %q", got.Status)
	}
	e.idle()
	insts, _ := e.prov.List(context.Background(), provider.Selector{})
	if len(insts) != 1 {
		t.Fatalf("%d instances after delete and re-create, want 1", len(insts))
	}
	if n := markerCount(t, e.dir); n != 0 {
		t.Fatalf("%d delete markers left", n)
	}
}

func TestDeleteMarkerSurvivesRestart(t *testing.T) {
	e := newEnv(t, manual())
	e.put("crash", config.StatusRunning)
	if err := e.b.DeleteShed(ctxT(t), "crash"); err != nil {
		t.Fatal(err)
	}
	// The server dies right after the 204: the destroy never finishes.
	e.restart()
	if sheds, _ := e.b.ListSheds(context.Background()); len(sheds) != 0 {
		t.Fatalf("the deleted shed is listed after a restart")
	}
	e.auto()
	e.idle()
	if insts, _ := e.prov.List(context.Background(), provider.Selector{}); len(insts) != 0 {
		t.Fatalf("%d instances left; the restarted server should have finished the delete", len(insts))
	}
}

// --- restart ---

func TestRestartResumesJobs(t *testing.T) {
	tests := []struct {
		name    string
		from    string
		act     func(e *env, name string) error
		wantEnd string
	}{
		{"during create", config.StatusStarting, nil, config.StatusRunning},
		{"during start", config.StatusStopped, func(e *env, name string) error {
			_, err := e.b.StartShed(ctxT(e.t), name)
			return err
		}, config.StatusRunning},
		{"during stop", config.StatusRunning, func(e *env, name string) error {
			_, err := e.b.StopShed(ctxT(e.t), name)
			return err
		}, config.StatusStopped},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t, manual())
			const name = "resume"
			e.put(name, tt.from)
			if tt.act != nil {
				if err := tt.act(e, name); err != nil {
					t.Fatal(err)
				}
			}
			before := e.status(name)
			id := e.settledID(name)
			e.restart()
			if got := e.status(name); got != before {
				t.Fatalf("status after restart %q, want %q until the job finishes", got, before)
			}
			e.auto()
			got := e.settled(name)
			if got.Status != tt.wantEnd || got.ID != id {
				t.Fatalf("came to rest %q (%q) id %s, want %q id %s", got.Status, got.StatusReason, got.ID, tt.wantEnd, id)
			}
			if insts, _ := e.prov.List(context.Background(), provider.Selector{}); len(insts) != 1 {
				t.Fatalf("%d instances after the resumed job, want 1", len(insts))
			}
			if n := e.prov.Count(fake.OpDestroy); n != 0 {
				t.Fatalf("a restart destroyed something (%d destroy calls)", n)
			}
		})
	}
}

func (e *env) settledID(name string) string {
	e.t.Helper()
	shed, err := e.b.GetShed(context.Background(), name)
	if err != nil {
		e.t.Fatal(err)
	}
	return shed.ID
}

// --- adoption ---

func TestAdoptionAfterCacheLoss(t *testing.T) {
	e := newEnv(t)
	for _, name := range []string{"adopt-a", "adopt-b", "adopt-c"} {
		req := config.CreateShedRequest{Name: name, NoProvision: true, Owner: "nocturn", Network: config.NetworkProfileIsolated}
		if _, err := e.b.CreateShed(ctxT(t), req); err != nil {
			t.Fatal(err)
		}
		e.settled(name)
	}
	if _, err := e.b.StopShed(ctxT(t), "adopt-c"); err != nil {
		t.Fatal(err)
	}
	e.settled("adopt-c")
	before, err := e.b.ListSheds(context.Background())
	if err != nil || len(before) != 3 {
		t.Fatalf("ListSheds = %d, %v", len(before), err)
	}

	if err := e.b.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(e.dir); err != nil {
		t.Fatal(err)
	}
	e.b = e.open()
	// A running shed stays "starting" until a probe has succeeded.
	for _, name := range []string{"adopt-a", "adopt-b", "adopt-c"} {
		e.settled(name)
	}
	after, err := e.b.ListSheds(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	wantJSON, _ := json.MarshalIndent(before, "", " ")
	gotJSON, _ := json.MarshalIndent(after, "", " ")
	if !bytes.Equal(wantJSON, gotJSON) {
		t.Fatalf("adopted records differ from the lost ones\nbefore: %s\nafter:  %s", wantJSON, gotJSON)
	}
	if n := e.prov.Count(fake.OpDestroy) + e.prov.Count(fake.OpStart); n != 0 {
		t.Fatalf("adoption made %d destroy or start calls, want none", n)
	}
	if insts, _ := e.prov.List(context.Background(), provider.Selector{}); len(insts) != 3 {
		t.Fatalf("%d instances after adoption, want 3", len(insts))
	}
}

func TestReconcileLeavesTheRestAlone(t *testing.T) {
	e := newEnv(t)
	if err := e.b.Close(); err != nil {
		t.Fatal(err)
	}
	seed := func(name, id string, st provider.ShedState, env string) {
		ident := provider.Identity{Env: env, ID: id, Name: name}
		spec, _ := provider.ShedSpec{CreatedAt: time.Now(), Image: "img", Size: "standard", Network: "open"}.Encode()
		e.prov.Seed(provider.Instance{
			Name:     ident.InstanceName(),
			State:    provider.StateRunning,
			Labels:   ident.Labels(st),
			Metadata: map[string]string{provider.MetadataShedSpec: spec},
		})
	}
	seed("half-made", "aaaaaaaaaaaa", provider.ShedStateCreating, testEnv)
	seed("doomed", "bbbbbbbbbbbb", provider.ShedStateDeleting, testEnv)
	seed("theirs", "cccccccccccc", provider.ShedStateReady, "other")
	e.prov.Seed(provider.Instance{Name: "unlabelled", State: provider.StateRunning})

	e.b = e.open()
	e.idle()
	sheds, _ := e.b.ListSheds(context.Background())
	if len(sheds) != 0 {
		t.Fatalf("reconcile adopted %d sheds, want none", len(sheds))
	}
	insts, _ := e.prov.List(context.Background(), provider.Selector{})
	left := map[string]bool{}
	for _, inst := range insts {
		left[inst.Labels[provider.LabelName]] = true
	}
	if left["doomed"] || !left["half-made"] || !left["theirs"] || len(insts) != 3 {
		t.Fatalf("instances left: %v; want everything but the one labelled deleting", left)
	}
	if rep := strings.Join(e.b.Reported(), "\n"); !strings.Contains(rep, "half-made") {
		t.Fatalf("the creating instance was not reported: %q", rep)
	}
}

// --- limits ---

func TestLimitsUnderConcurrentCreates(t *testing.T) {
	e := newEnv(t, limits(4, 8))
	const n = 20
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = e.create(fmt.Sprintf("many-%02d", i))
		}()
	}
	wg.Wait()
	passed := 0
	for i, err := range errs {
		switch {
		case err == nil:
			passed++
		case !errors.Is(err, config.ErrServerLimitReachedSentinel):
			t.Errorf("create %d failed with %v, want the server limit", i, err)
		}
	}
	if passed != 4 {
		t.Fatalf("%d of %d concurrent creates passed at max_running 4, want exactly 4", passed, n)
	}
	e.idle()
	if insts, _ := e.prov.List(context.Background(), provider.Selector{}); len(insts) != 4 {
		t.Fatalf("%d instances exist, want 4", len(insts))
	}
}

func TestMaxSheds(t *testing.T) {
	e := newEnv(t, limits(8, 2))
	for _, name := range []string{"s1", "s2"} {
		if _, err := e.create(name); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.create("s3"); !errors.Is(err, config.ErrServerLimitReachedSentinel) {
		t.Fatalf("third create at max_sheds 2 = %v", err)
	}
}

// --- host keys ---

func TestHostKeyIsLearnedOnceAndPinned(t *testing.T) {
	e := newEnv(t, manual())
	e.fl.presented = testHostKey
	e.put("keys", config.StatusStarting)
	e.finish("keys")
	ref := e.ref("keys")
	// The guest has not published its key yet: the create keeps waiting.
	time.Sleep(50 * time.Millisecond)
	if got := e.status("keys"); got != config.StatusStarting {
		t.Fatalf("status before the key is published: %q", got)
	}
	e.prov.PublishHostKey(ref, testHostKey+" a-comment")
	if got := e.settled("keys"); got.Status != config.StatusRunning {
		t.Fatalf("settled %q (%q)", got.Status, got.StatusReason)
	}
	inst, _ := e.prov.Get(context.Background(), ref)
	if inst.HostKeyPin() != testHostKey {
		t.Fatalf("pin in metadata = %q", inst.HostKeyPin())
	}
	if inst.ShedState() != provider.ShedStateReady || inst.Layout() != guest.Layout {
		t.Fatalf("labels after create: state %q layout %d", inst.ShedState(), inst.Layout())
	}
	reads := e.prov.Count(fake.OpGuestHostKey)

	// A pinned shed is never learned again: a different key is a mismatch.
	e.auto()
	if _, err := e.b.StopShed(ctxT(t), "keys"); err != nil {
		t.Fatal(err)
	}
	e.settled("keys")
	e.fl.mu.Lock()
	e.fl.presented = strings.Replace(testHostKey, "hostkey00", "otherkey0", 1)
	e.fl.mu.Unlock()
	if _, err := e.b.StartShed(ctxT(t), "keys"); err != nil {
		t.Fatal(err)
	}
	got := e.settled("keys")
	if got.Status != config.StatusStopped || got.StatusReason != config.StatusReasonHostKey {
		t.Fatalf("start with a changed host key settled %q (%q), want stopped (host_key)", got.Status, got.StatusReason)
	}
	if n := e.prov.Count(fake.OpGuestHostKey); n != reads {
		t.Fatalf("the published key was read again for a pinned shed (%d reads, was %d)", n, reads)
	}
}

// --- exec and friends on a shed that is not running ---

func TestNotRunningAndUnsupported(t *testing.T) {
	e := newEnv(t, manual())
	e.put("idle", config.StatusStarting)
	ctx := ctxT(t)
	if err := e.b.Exec(ctx, "idle", backend.ExecOptions{Cmd: []string{"true"}}); !errors.Is(err, config.ErrShedNotRunningSentinel) {
		t.Errorf("Exec on a starting shed = %v", err)
	}
	if _, err := e.b.ListSessions(ctx, "idle"); !errors.Is(err, config.ErrShedNotRunningSentinel) {
		t.Errorf("ListSessions on a starting shed = %v", err)
	}
	if _, err := e.b.DialService(ctx, "idle", 80); !errors.Is(err, config.ErrShedNotRunningSentinel) {
		t.Errorf("DialService on a starting shed = %v", err)
	}
	if err := e.b.Exec(ctx, "nope", backend.ExecOptions{}); !errors.Is(err, config.ErrShedNotFoundSentinel) {
		t.Errorf("Exec on a missing shed = %v", err)
	}
	owned := backend.WithRequiredOwner(ctx, "someone-else")
	if _, err := e.b.GetShed(owned, "idle"); !errors.Is(err, config.ErrShedNotFoundSentinel) {
		t.Errorf("GetShed for another owner = %v", err)
	}
	if err := e.b.DeleteShed(owned, "idle"); !errors.Is(err, config.ErrShedNotFoundSentinel) {
		t.Errorf("DeleteShed for another owner = %v", err)
	}

	if _, err := e.b.ResetShed(ctx, "idle"); !errors.Is(err, config.ErrNotSupportedSentinel) || !errors.Is(err, config.ErrFeatureUnsupportedSentinel) {
		t.Errorf("ResetShed = %v", err)
	}
	if _, err := e.b.CreateSnapshot(ctx, config.SnapshotCreateRequest{}); !errors.Is(err, config.ErrFeatureUnsupportedSentinel) {
		t.Errorf("CreateSnapshot = %v", err)
	}
	if imgs, err := e.b.ListImages(ctx); err != nil || imgs == nil || len(imgs) != 0 {
		t.Errorf("ListImages = %v, %v", imgs, err)
	}
	if snaps, err := e.b.ListSnapshots(ctx); err != nil || len(snaps) != 0 {
		t.Errorf("ListSnapshots = %v, %v", snaps, err)
	}
	for _, req := range []config.CreateShedRequest{
		{Name: "x1", LocalDir: "/tmp"},
		{Name: "x2", FromSnapshot: "snap"},
		{Name: "x3", Egress: []string{"p"}},
	} {
		if _, err := e.b.CreateShed(ctx, req); !errors.Is(err, config.ErrFeatureUnsupportedSentinel) {
			t.Errorf("CreateShed(%+v) = %v, want feature unsupported", req, err)
		}
	}
	if caps := e.b.Capabilities(); !caps.AsyncLifecycle || caps.Isolation != backend.IsolationTopology || caps.Snapshots {
		t.Errorf("Capabilities = %+v", caps)
	}
	if !e.b.SupportsNetworkProfile(config.NetworkProfileIsolated) || e.b.SupportsNetworkProfile("nonsense") {
		t.Errorf("SupportsNetworkProfile is wrong")
	}
}

// --- timing lines and progress ---

func TestTimingLinesAndProgress(t *testing.T) {
	e := newEnv(t, manual())
	e.put("timed", config.StatusStarting)

	var mu sync.Mutex
	var phases []string
	ctx := backend.ContextWithProgress(ctxT(t), func(ev backend.ProgressEvent) {
		mu.Lock()
		defer mu.Unlock()
		if ev.Phase != "" {
			phases = append(phases, ev.Phase)
		}
	})
	e.auto()
	shed, err := e.b.WaitSettled(ctx, "timed")
	if err != nil || shed.Status != config.StatusRunning {
		t.Fatalf("WaitSettled = %+v, %v", shed, err)
	}
	mu.Lock()
	got := strings.Join(phases, ",")
	mu.Unlock()
	if !strings.HasPrefix(got, "provider,boot,agent") {
		t.Fatalf("progress phases = %q, want provider, boot, agent first", got)
	}

	for _, call := range []func() error{
		func() error { _, err := e.b.StopShed(ctxT(t), "timed"); return err },
		func() error { _, err := e.b.StartShed(ctxT(t), "timed"); return err },
	} {
		if err := call(); err != nil {
			t.Fatal(err)
		}
		e.settled("timed")
	}
	if err := e.b.DeleteShed(ctxT(t), "timed"); err != nil {
		t.Fatal(err)
	}
	e.idle()

	logs := strings.Join(e.logLines(), "\n")
	for _, want := range []string{
		"timing: create name=timed backend=remote total=",
		"timing: stop name=timed backend=remote total=",
		"timing: start name=timed backend=remote total=",
		"timing: delete name=timed backend=remote total=",
	} {
		if !strings.Contains(logs, want) {
			t.Errorf("no log line %q in:\n%s", want, logs)
		}
	}
	for _, line := range e.logLines() {
		if strings.HasPrefix(line, "timing: create") {
			for _, phase := range []string{" provider=", " boot=", " agent="} {
				if !strings.Contains(line, phase) {
					t.Errorf("create timing line lacks%s: %s", phase, line)
				}
			}
		}
	}
}

func TestHTTPSRepoURL(t *testing.T) {
	tests := []struct{ in, want string }{
		{"", ""},
		{"charliek/prox", "https://github.com/charliek/prox.git"},
		{"git@github.com:charliek/prox.git", "https://github.com/charliek/prox.git"},
		{"git@github.com:charliek/prox", "https://github.com/charliek/prox.git"},
		{"https://token@github.com/o/r.git", "https://token@github.com/o/r.git"},
		{"git@gitlab.com:o/r.git", "git@gitlab.com:o/r.git"},
	}
	for _, tt := range tests {
		if got := httpsRepoURL(tt.in); got != tt.want {
			t.Errorf("httpsRepoURL(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// --- end to end: the fake cloud and a real agent on unix sockets ---

func TestEndToEndLocalLink(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the local link runs shed-agent on this host, which needs Linux")
	}
	factory, err := locallink.New(locallink.Config{AgentPath: conformance.BuildAgent(t, ""), Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, fl: newFakeLinks(), dir: t.TempDir(), prov: fake.New(fake.Options{Env: testEnv})}
	e.cfg = newEnv(t).cfg
	e.links = factory
	e.b = e.open()
	t.Cleanup(func() { _ = e.b.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	run := func(want string) {
		t.Helper()
		var out bytes.Buffer
		err := e.b.Exec(ctx, "e2e", backend.ExecOptions{
			Cmd:        []string{"/bin/sh", "-c", "echo " + want},
			Stdout:     vmutil.NopWriteCloser(&out),
			Stderr:     vmutil.NopWriteCloser(&out),
			WorkingDir: "/",
		})
		if err != nil || strings.TrimSpace(out.String()) != want {
			t.Fatalf("exec = %q, %v; want %q", out.String(), err, want)
		}
	}
	wait := func(want string) {
		t.Helper()
		shed, err := e.b.WaitSettled(ctx, "e2e")
		if err != nil || shed.Status != want {
			t.Fatalf("WaitSettled = %+v, %v; want %s", shed, err, want)
		}
	}

	shed, err := e.b.CreateShed(ctx, config.CreateShedRequest{Name: "e2e", NoProvision: true})
	if err != nil || shed.Status == config.StatusError {
		t.Fatalf("create = %+v, %v", shed, err)
	}
	wait(config.StatusRunning)
	run("first")

	if _, err := e.b.StopShed(ctx, "e2e"); err != nil {
		t.Fatal(err)
	}
	wait(config.StatusStopped)
	if err := e.b.Exec(ctx, "e2e", backend.ExecOptions{Cmd: []string{"true"}}); !errors.Is(err, config.ErrShedNotRunningSentinel) {
		t.Fatalf("exec on a stopped shed = %v", err)
	}

	if _, err := e.b.StartShed(ctx, "e2e"); err != nil {
		t.Fatal(err)
	}
	wait(config.StatusRunning)
	run("second")
	if sessions, err := e.b.ListSessions(ctx, "e2e"); err != nil {
		t.Logf("ListSessions (needs tmux on this host): %v", err)
	} else if len(sessions) != 0 {
		t.Logf("ListSessions = %v", sessions)
	}

	if err := e.b.DeleteShed(ctx, "e2e"); err != nil {
		t.Fatal(err)
	}
	if err := e.b.WaitIdle(ctx); err != nil {
		t.Fatal(err)
	}
	if insts, _ := e.prov.List(ctx, provider.Selector{}); len(insts) != 0 {
		t.Fatalf("%d instances left after delete", len(insts))
	}
}
