package locallink_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/charliek/shed/internal/remote/transport"
	"github.com/charliek/shed/internal/remote/transport/conformance"
	"github.com/charliek/shed/internal/remote/transport/locallink"
	"github.com/charliek/shed/internal/vmutil"
)

func requireLinux(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("the local link runs shed-agent on this host, which needs Linux")
	}
}

func newFactory(t *testing.T, agent string) *locallink.Factory {
	t.Helper()
	f, err := locallink.New(locallink.Config{AgentPath: agent, Logf: t.Logf})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return f
}

func open(t *testing.T, f *locallink.Factory, name string) *locallink.Link {
	t.Helper()
	link, err := f.Open(context.Background(), transport.Target{ShedID: "localtest", ShedName: name})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { link.Close() })
	return link.(*locallink.Link)
}

// TestConformance is leg 1 of the transport suite: a real agent on unix
// sockets, no SSH.
func TestConformance(t *testing.T) {
	requireLinux(t)
	f := newFactory(t, conformance.BuildAgent(t, ""))
	if f.Name() != transport.NameLocal {
		t.Errorf("Name = %q", f.Name())
	}

	conformance.Run(t, conformance.Harness{
		Open: func(t *testing.T) transport.Link { return open(t, f, "conformance") },
		HTTPPort: func(link transport.Link) uint16 {
			return uint16(link.(*locallink.Link).HTTPPort())
		},
		// The peer is the link's own agent. Kill it outright; the next
		// Dial must start another.
		Restart: func(t *testing.T, link transport.Link) {
			pid := link.(*locallink.Link).AgentPID()
			if pid == 0 {
				t.Fatal("no agent is running")
			}
			if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
				t.Fatalf("kill agent %d: %v", pid, err)
			}
		},
	})
}

func TestOpenStartsNothing(t *testing.T) {
	requireLinux(t)
	f := newFactory(t, conformance.BuildAgent(t, ""))
	link := open(t, f, "lazy")
	if pid := link.AgentPID(); pid != 0 {
		t.Fatalf("Open started an agent (pid %d); the first Dial should", pid)
	}
	if link.LogPath() != "" {
		t.Errorf("Open made the link's directory: %s", link.LogPath())
	}
}

func TestCloseStopsTheAgentAndRemovesItsDirectory(t *testing.T) {
	requireLinux(t)
	f := newFactory(t, conformance.BuildAgent(t, ""))
	link := open(t, f, "closing")
	client := vmutil.NewAgentClient(link, transport.PortConsole, transport.PortNotify)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := client.CheckHealth(ctx); err != nil {
		t.Fatalf("CheckHealth: %v", err)
	}
	pid := link.AgentPID()
	dir := filepath.Dir(link.LogPath())
	if pid == 0 || dir == "." {
		t.Fatalf("pid %d, dir %q after a successful probe", pid, dir)
	}
	if st, err := os.Stat(filepath.Join(dir, "s")); err != nil || st.Mode().Perm() != 0o700 {
		t.Errorf("socket directory: %v, mode %v; want 0700", err, st.Mode().Perm())
	}
	if st, err := os.Stat(transport.SocketPath(filepath.Join(dir, "s"), transport.PortConsole)); err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("console socket: %v, mode %v; want 0600", err, st.Mode().Perm())
	}

	if err := link.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Errorf("agent %d after Close: kill -0 = %v, want no such process", pid, err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the link's directory %s survived Close (%v)", dir, err)
	}
	if _, err := link.Dial(ctx, transport.PortNotify); !errors.Is(err, transport.ErrClosed) {
		t.Errorf("Dial after Close = %v, want ErrClosed", err)
	}
}

// A deep TMPDIR must not push the agent's socket paths past the kernel's
// 107-byte limit.
func TestDeepTempDir(t *testing.T) {
	requireLinux(t)
	deep := filepath.Join(t.TempDir(), strings.Repeat("d", 60), strings.Repeat("e", 60))
	if err := os.MkdirAll(deep, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", deep)

	f := newFactory(t, conformance.BuildAgent(t, ""))
	link := open(t, f, "deep")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client := vmutil.NewAgentClient(link, transport.PortConsole, transport.PortNotify)
	if err := client.CheckHealth(ctx); err != nil {
		t.Fatalf("CheckHealth with TMPDIR=%d bytes: %v", len(deep), err)
	}
	if got := link.LogPath(); strings.HasPrefix(got, deep) {
		t.Errorf("the link used the deep TMPDIR: %s", got)
	}
}

func TestStartFailures(t *testing.T) {
	requireLinux(t)
	dir := t.TempDir()
	script := func(name, body string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		return path
	}
	counter := filepath.Join(dir, "starts")

	tests := []struct {
		name    string
		agent   string
		wantMsg string
	}{
		{
			name:    "the agent exits at once",
			agent:   script("exits", "echo run >> "+counter+"; echo 'no vsock here' >&2; exit 1"),
			wantMsg: "no vsock here",
		},
		{
			name:    "the agent never listens",
			agent:   script("hangs", "exec sleep 30"),
			wantMsg: "did not listen",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, err := locallink.New(locallink.Config{
				AgentPath:        tt.agent,
				StartTimeout:     300 * time.Millisecond,
				MinStartInterval: 400 * time.Millisecond,
				Logf:             t.Logf,
			})
			if err != nil {
				t.Fatal(err)
			}
			link := open(t, f, "failing")
			ctx := context.Background()

			_, err = link.Dial(ctx, transport.PortNotify)
			if !errors.Is(err, transport.ErrUnreachable) {
				t.Fatalf("Dial = %v, want ErrUnreachable", err)
			}
			if !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("Dial error %q does not say %q", err, tt.wantMsg)
			}
			if link.AgentPID() != 0 {
				t.Error("a failed start left an agent recorded as running")
			}

			// Inside the interval the last error comes back at once,
			// with no new start.
			start := time.Now()
			_, again := link.Dial(ctx, transport.PortNotify)
			if again == nil || again.Error() != err.Error() {
				t.Errorf("second Dial = %v, want the first error again", again)
			}
			if d := time.Since(start); d > 100*time.Millisecond {
				t.Errorf("second Dial took %v; it should not have started an agent", d)
			}
		})
	}

	t.Run("failed starts are spaced", func(t *testing.T) {
		data, err := os.ReadFile(counter)
		if err != nil {
			t.Fatal(err)
		}
		if n := strings.Count(string(data), "run"); n != 1 {
			t.Errorf("the failing agent was started %d times for two Dials inside the interval, want 1", n)
		}
	})
}

func TestNewRejectsBadConfig(t *testing.T) {
	requireLinux(t)
	dir := t.TempDir()
	plain := filepath.Join(dir, "plain")
	if err := os.WriteFile(plain, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		path string
	}{
		{"no path", ""},
		{"missing file", filepath.Join(dir, "absent")},
		{"a directory", dir},
		{"not executable", plain},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := locallink.New(locallink.Config{AgentPath: tt.path}); err == nil {
				t.Errorf("New(%q) succeeded", tt.path)
			}
		})
	}
}
