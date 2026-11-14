package sshlink

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/charliek/shed/internal/agentproto"
	"github.com/charliek/shed/internal/remote/transport"
	"github.com/charliek/shed/internal/remote/transport/conformance"
	"github.com/charliek/shed/internal/remote/transport/locallink"
	"github.com/charliek/shed/internal/remote/transport/sshlink/sshtest"
	"github.com/charliek/shed/internal/vmutil"
)

// The agent is built once for the package, on first use.
var agentBuild struct {
	once sync.Once
	dir  string
	path string
	err  error
}

func TestMain(m *testing.M) {
	code := m.Run()
	if agentBuild.dir != "" {
		os.RemoveAll(agentBuild.dir)
	}
	os.Exit(code)
}

// localAgent is a real shed-agent on unix sockets on this host, the thing a
// guest's sshd forwards to.
type localAgent struct {
	link *locallink.Link
}

func (a *localAgent) socketDir() string { return a.link.SocketDir() }
func (a *localAgent) httpPort() uint16  { return uint16(a.link.HTTPPort()) }

// startLocalAgent runs a real agent for the test. Linux only.
func startLocalAgent(t *testing.T) *localAgent {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("needs a shed-agent on this host, which needs Linux")
	}
	agentBuild.once.Do(func() {
		agentBuild.dir, agentBuild.err = os.MkdirTemp("", "sshlink-agent-")
		if agentBuild.err == nil {
			agentBuild.path, agentBuild.err = conformance.BuildAgentInto(agentBuild.dir, "")
		}
	})
	if agentBuild.err != nil {
		t.Fatalf("build agent: %v", agentBuild.err)
	}
	f, err := locallink.New(locallink.Config{AgentPath: agentBuild.path, Logf: func(string, ...any) {}})
	if err != nil {
		t.Fatal(err)
	}
	link, err := f.Open(context.Background(), transport.Target{ShedName: "sshlink-test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { link.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := vmutil.NewAgentClient(link, transport.PortConsole, transport.PortNotify).CheckHealth(ctx); err != nil {
		t.Fatalf("local agent: %v", err)
	}
	return &localAgent{link: link.(*locallink.Link)}
}

// fakeSockets stands in for an agent where a test needs only sockets, so it
// can run on any system: the proxy port echoes, the notify port accepts and
// stays silent, and the console port does not exist.
func fakeSockets(t *testing.T) string {
	t.Helper()
	// Not t.TempDir(): on macOS it is too deep for a socket path.
	dir, err := os.MkdirTemp("/tmp", "sshlink-socks-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	serve := func(port uint32, handle func(net.Conn)) {
		ln, err := net.Listen("unix", transport.SocketPath(dir, port))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { ln.Close() })
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				go func() {
					defer c.Close()
					handle(c)
				}()
			}
		}()
	}
	serve(transport.PortTCPProxy, func(c net.Conn) { _, _ = io.Copy(c, c) })
	serve(transport.PortNotify, func(c net.Conn) { _, _ = io.Copy(io.Discard, c) })
	return dir
}

// fixture is an in-process SSH server, a client key it accepts, and a factory
// configured to reach sockets in socketDir through it.
type fixture struct {
	srv     *sshtest.Server
	key     ssh.Signer
	cfg     Config
	factory *Factory
	logs    *logSink
}

// logSink collects a link's log lines. Links log from goroutines that can
// outlive a test, so they must not log to testing.T.
type logSink struct {
	mu    sync.Mutex
	lines []string
}

func (s *logSink) logf(format string, args ...any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lines = append(s.lines, fmt.Sprintf(format, args...))
}

func (s *logSink) all() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.lines...)
}

func newFixture(t *testing.T, socketDir string, mutate func(*Config), srvOpts sshtest.Options) *fixture {
	t.Helper()
	key := sshtest.NewKey(t)
	if srvOpts.AuthorizedKeys == nil {
		srvOpts.AuthorizedKeys = []ssh.PublicKey{key.PublicKey()}
	}
	srv := sshtest.NewServer(t, srvOpts)

	logs := &logSink{}
	cfg := Config{
		Keys:      []ssh.Signer{key},
		SocketDir: socketDir,
		Logf:      logs.logf,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	factory, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		if t.Failed() {
			for _, line := range logs.all() {
				t.Log(line)
			}
		}
	})
	return &fixture{srv: srv, key: key, cfg: cfg, factory: factory, logs: logs}
}

// open returns a link to the fixture's server with its host key pinned.
func (f *fixture) open(t *testing.T) *Link {
	t.Helper()
	return f.openWith(t, PinnedHostKey(f.srv.HostKey()), f.addr)
}

func (f *fixture) addr(context.Context) (string, error) {
	if addr := f.srv.Addr(); addr != "" {
		return addr, nil
	}
	return "", errors.New("the server is not listening")
}

func (f *fixture) openWith(t *testing.T, keys transport.HostKeyStore, addr func(context.Context) (string, error)) *Link {
	t.Helper()
	link, err := f.factory.Open(context.Background(), transport.Target{
		ShedID:   "testshedid",
		ShedName: "testshed",
		Addr:     addr,
		HostKeys: keys,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { link.Close() })
	return link.(*Link)
}

// learnStore is a HostKeyStore with no pin at first. It learns a key only if
// it equals what the "guest published".
type learnStore struct {
	mu        sync.Mutex
	pin       string
	published string
	learns    int
	pinErr    error
}

func (s *learnStore) Pin(context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pin, s.pinErr
}

func (s *learnStore) Learn(_ context.Context, presented string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.learns++
	if s.pin != "" {
		return errors.New("a pin already exists")
	}
	if presented != s.published {
		return errors.New("the guest published a different key")
	}
	s.pin = presented
	return nil
}

func (s *learnStore) state() (pin string, learns int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pin, s.learns
}

func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// eventually polls cond until it holds or the time is up.
func eventually(t *testing.T, within time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("%s: not within %v", what, within)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// emptySocketDir is a socket directory with no agent in it: sshd is up, the
// agent is not.
func emptySocketDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "sshlink-empty-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Clean(dir)
}

// writeExecRequest starts a shell command on a console-port stream, speaking
// the agent's exec protocol by hand.
func writeExecRequest(w io.Writer, shell string) error {
	req, err := json.Marshal(agentproto.ExecRequest{Cmd: []string{"/bin/sh", "-c", shell}})
	if err != nil {
		return err
	}
	return agentproto.WriteMessage(w, agentproto.MsgTypeExecRequest, req)
}

// writeDataFrame sends one frame of stdin to a command started with
// writeExecRequest.
func writeDataFrame(w io.Writer, data []byte) error {
	return agentproto.WriteMessage(w, agentproto.MsgTypeData, data)
}
