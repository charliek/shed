// Package servicetest runs the reference compute service
// (internal/directory/refserver) in process, over loopback, with one server
// enrolled and granted compute, for tests of the compute service's provider
// and of the remote backend on it. It is imported by tests only.
package servicetest

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/directory"
	"github.com/charliek/shed/internal/directory/refserver"
	"github.com/charliek/shed/internal/remote/provider/service"
	"github.com/charliek/shed/internal/remote/transport/sshlink/sshtest"
)

// The world's names.
const (
	// Organization is the service's organization.
	Organization = "example"
	// DefaultServer is the server's enrolled name.
	DefaultServer = "srv-a"
)

// Options configures a World.
type Options struct {
	// Server is the enrolled name; empty means DefaultServer.
	Server string
	// People are the service's people; nil means ada (an admin), linus and
	// turing, the compute contract's world.
	People []refserver.User
	// Clients are the clients registered for the server.
	Clients []string
	// NoGrant leaves the server without the compute grant.
	NoGrant bool
	// AutoSettle settles every operation this long after it starts (zero:
	// only Settle does).
	AutoSettle time.Duration
	// HostKey is published for every machine when it first runs; empty
	// means a fixed test key, and NoHostKey publishes none.
	HostKey   string
	NoHostKey bool
	// Logf receives the service's log lines; nil logs nothing.
	Logf func(format string, args ...any)
}

// World is the reference compute service with one enrolled server.
type World struct {
	t        testing.TB
	Ref      *refserver.Server
	HTTP     *httptest.Server
	Issuer   string
	Server   string
	ClientID string
	// StateDir is the server's directory state directory, holding its
	// enrolment: what a server process's runtime reads.
	StateDir string
	// HostKey is the key the guests publish.
	HostKey string

	inputs *memInputs
	down   atomic.Bool
	// lose is the method whose requests the service acts on and whose
	// replies it then drops ("" for none).
	lose    atomic.Value
	handler http.Handler
	stop    chan struct{}
}

// Start runs a world for the test.
func Start(t testing.TB, opts Options) *World {
	t.Helper()
	if opts.Server == "" {
		opts.Server = DefaultServer
	}
	if opts.People == nil {
		opts.People = []refserver.User{
			{Name: "ada", Admin: true, Keys: []refserver.Key{{Key: NewKey(t), Label: "laptop"}}},
			{Name: "linus", Keys: []refserver.Key{{Key: NewKey(t), Label: "laptop"}}},
			{Name: "turing", Keys: []refserver.Key{{Key: NewKey(t), Label: "laptop"}}},
		}
	}
	w := &World{t: t, Server: opts.Server, inputs: &memInputs{users: opts.People, codes: map[string]string{}}, stop: make(chan struct{})}
	switch {
	case opts.NoHostKey:
	case opts.HostKey != "":
		w.HostKey = opts.HostKey
	default:
		w.HostKey = NewKey(t)
	}
	w.HTTP = httptest.NewUnstartedServer(nil)
	w.Issuer = "http://" + w.HTTP.Listener.Addr().String()
	logf := opts.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	ref, err := refserver.New(refserver.Config{Issuer: w.Issuer, Organization: Organization, Inputs: w.inputs, Logf: logf,
		Limits: refserver.Limits{Compute: 100000, Token: 100000, Directory: 100000}})
	if err != nil {
		t.Fatal(err)
	}
	w.Ref = ref
	w.lose.Store("")
	w.handler = http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if m, _ := w.lose.Load().(string); m != "" && r.Method == m && strings.HasPrefix(r.URL.Path, "/v1/compute/") {
			// The service acts on the request, then the connection drops
			// before the reply: the outcome is unknown to the server.
			ref.ServeHTTP(httptest.NewRecorder(), r)
			if hj, ok := rw.(http.Hijacker); ok {
				if c, _, err := hj.Hijack(); err == nil {
					_ = c.Close()
					return
				}
			}
			return
		}
		if w.down.Load() {
			// The service is down: the connection is dropped.
			if hj, ok := rw.(http.Hijacker); ok {
				if c, _, err := hj.Hijack(); err == nil {
					_ = c.Close()
					return
				}
			}
			rw.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		ref.ServeHTTP(rw, r)
	})
	w.HTTP.Config.Handler = w.handler
	w.HTTP.Start()
	t.Cleanup(func() {
		close(w.stop)
		w.HTTP.Close()
	})
	ref.ComputeSettings(opts.AutoSettle, w.HostKey, nil)
	if opts.AutoSettle > 0 {
		go func() {
			tick := time.NewTicker(10 * time.Millisecond)
			defer tick.Stop()
			for {
				select {
				case <-w.stop:
					return
				case <-tick.C:
					_ = ref.ComputeTick()
				}
			}
		}()
	}
	w.StateDir = filepath.Join(t.TempDir(), directory.StateDirName)
	w.enrol()
	if !opts.NoGrant {
		w.Step("compute.allow", w.Server)
	}
	for _, c := range opts.Clients {
		w.Step("client.register", []string{w.Server, c})
	}
	return w
}

// enrol enrols the server with a new code and writes its enrolment.
func (w *World) enrol() {
	w.t.Helper()
	code := fmt.Sprintf("code-%d", time.Now().UnixNano())
	w.inputs.mu.Lock()
	w.inputs.codes[code] = w.Server
	w.inputs.mu.Unlock()
	e, err := directory.Enroll(context.Background(), directory.EnrolRequest{
		Issuer: w.Issuer, Code: code, Name: w.Server, AllowPlainHTTP: true,
	})
	if err != nil {
		w.t.Fatal(err)
	}
	data, err := e.MarshalFile()
	if err != nil {
		w.t.Fatal(err)
	}
	if err := directory.EnsureStateDir(w.StateDir); err != nil {
		w.t.Fatal(err)
	}
	_ = directory.RemoveStateFile(w.StateDir, directory.EnrolmentFileName)
	if err := directory.WriteStateFile(w.StateDir, directory.EnrolmentFileName, data); err != nil {
		w.t.Fatal(err)
	}
	w.ClientID = e.ClientID
}

// Runtime returns a new directory runtime over the server's enrolment: what
// one server process holds.
func (w *World) Runtime() *directory.Runtime {
	w.t.Helper()
	lookup, err := directory.NewLookup(directory.LookupOptions{LocalAdmins: map[string]config.UserConfig{
		"ops": {Admin: true, Keys: []config.UserKeyConfig{{Key: NewKey(w.t), Label: "desk"}}},
	}})
	if err != nil {
		w.t.Fatal(err)
	}
	rt, err := directory.NewRuntime(directory.RuntimeOptions{
		Lookup: lookup, StateDir: w.StateDir, Issuer: w.Issuer, AllowPlainHTTP: true,
		Refresh: time.Hour, Trust: 2 * time.Hour,
	})
	if err != nil {
		w.t.Fatal(err)
	}
	return rt
}

// Client returns a compute client of a new server process (its own
// runtime), with test drivers and fast polling.
func (w *World) Client() *service.Client {
	w.t.Helper()
	c, err := service.New(service.Config{Session: w.Runtime(), TestDrivers: true,
		PollFirst: 5 * time.Millisecond, PollAfter: 10 * time.Millisecond})
	if err != nil {
		w.t.Fatal(err)
	}
	return c
}

// Step applies one of the compute contract's set-up steps.
func (w *World) Step(name string, value any) {
	w.t.Helper()
	if err := w.Ref.ComputeStep(name, value); err != nil {
		w.t.Fatalf("set-up step %s %v: %v", name, value, err)
	}
}

// Settle finishes the work in progress on a machine of the server; it
// reports false when there is no such machine.
func (w *World) Settle(shedID string) bool {
	return w.Ref.ComputeStep("cloud.settle", []string{w.Server, shedID}) == nil
}

// Down makes the service drop every connection (true) or answer again. A
// request is read before its connection is dropped: its outcome is unknown.
func (w *World) Down(down bool) { w.down.Store(down) }

// LoseReplies makes the service act on every compute request of method and
// drop the connection before replying ("" stops it). Token requests are
// answered.
func (w *World) LoseReplies(method string) { w.lose.Store(method) }

// Stop stops the service: nothing listens at its address, so a connection
// is refused and no request reaches it.
func (w *World) Stop() {
	w.HTTP.Close()
}

// Restart listens at the service's address again, with the same state.
func (w *World) Restart() {
	w.t.Helper()
	addr := strings.TrimPrefix(w.Issuer, "http://")
	var ln net.Listener
	var err error
	for i := 0; i < 50; i++ {
		if ln, err = net.Listen("tcp", addr); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		w.t.Fatalf("restarting the service at %s: %v", addr, err)
	}
	srv := httptest.NewUnstartedServer(w.handler)
	_ = srv.Listener.Close()
	srv.Listener = ln
	srv.Start()
	w.HTTP = srv
}

// SetPeople replaces the service's list of people.
func (w *World) SetPeople(people []refserver.User) {
	w.inputs.mu.Lock()
	w.inputs.users = people
	w.inputs.mu.Unlock()
}

// NewKey returns a new ssh-ed25519 public key line.
func NewKey(t testing.TB) string {
	t.Helper()
	return sshtest.PublicKeyLine(sshtest.NewKey(t).PublicKey())
}

// memInputs is the operator's inputs in memory.
type memInputs struct {
	mu    sync.Mutex
	users []refserver.User
	codes map[string]string // code -> server name
}

func (m *memInputs) Users() ([]refserver.User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]refserver.User(nil), m.users...), nil
}

func (m *memInputs) Code(code string) (refserver.Code, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	name, ok := m.codes[code]
	return refserver.Code{ServerName: name}, ok, nil
}

func (m *memInputs) Disabled(string) (bool, error) { return false, nil }
