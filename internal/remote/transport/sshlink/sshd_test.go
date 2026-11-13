package sshlink

import (
	"context"
	"errors"
	"io"
	"net"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/remote/guest"
	"github.com/charliek/shed/internal/remote/transport"
	"github.com/charliek/shed/internal/remote/transport/conformance"
	"github.com/charliek/shed/internal/remote/transport/sshlink/sshtest"
	"github.com/charliek/shed/internal/vmutil"
)

// sshdFixture is a real OpenSSH sshd in a container laid out like a cloud
// guest, and a factory whose key it accepts.
type sshdFixture struct {
	sshd    *sshtest.SSHD
	key     ssh.Signer
	factory *Factory
	logs    *logSink
}

func newSSHDFixture(t *testing.T, extraConfig string, mutate func(*Config)) *sshdFixture {
	t.Helper()
	key := sshtest.NewKey(t)
	sshd := sshtest.StartSSHD(t, sshtest.SSHDOptions{
		AuthorizedKeys: []string{AuthorizedKey(key)},
		ExtraConfig:    extraConfig,
	})

	logs := &logSink{}
	cfg := Config{Keys: []ssh.Signer{key}, Logf: logs.logf}
	if mutate != nil {
		mutate(&cfg)
	}
	factory, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if t.Failed() {
			for _, line := range logs.all() {
				t.Log(line)
			}
		}
	})
	return &sshdFixture{sshd: sshd, key: key, factory: factory, logs: logs}
}

func (f *sshdFixture) open(t *testing.T) *Link {
	t.Helper()
	link, err := f.factory.Open(context.Background(), transport.Target{
		ShedID:   "sshdtestid",
		ShedName: "sshd-test",
		Addr:     f.sshd.Addr,
		HostKeys: PinnedHostKey(f.sshd.HostKey()),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { link.Close() })
	return link.(*Link)
}

// TestConformanceSSHD is leg 3 of the transport suite: the SSH link, with
// its defaults, against OpenSSH's sshd running guest.SSHDConfig, forwarding
// to an agent that runs as root with its sockets open to the link user's
// group. It is the leg that decides what the plan left open about sshd:
// whether forwarded channels count against MaxSessions (the suite holds 64
// open against the default limit of 10) and whether half-closes cross sshd in
// both directions. TestSSHDForwardingPolicy and TestSSHDFacts hold the rest.
func TestConformanceSSHD(t *testing.T) {
	f := newSSHDFixture(t, "", nil)

	conformance.Run(t, conformance.Harness{
		Open: func(t *testing.T) transport.Link { return f.open(t) },
		// The container restarts: sshd and the agent go down together
		// and come back, possibly on another host port.
		Restart: func(t *testing.T, _ transport.Link) { f.sshd.Restart(t) },
	})

	t.Run("guest layout", func(t *testing.T) {
		tests := []struct {
			name string
			args []string
			want string
		}{
			{"socket directory", []string{"stat", "-c", "%U:%G %a", guest.AgentSocketDir}, "root:shed-link 750"},
			{"console socket", []string{"stat", "-c", "%U:%G %a", transport.SocketPath(guest.AgentSocketDir, transport.PortConsole)}, "root:shed-link 660"},
			{"notify socket", []string{"stat", "-c", "%U:%G %a", transport.SocketPath(guest.AgentSocketDir, transport.PortNotify)}, "root:shed-link 660"},
			{"proxy socket", []string{"stat", "-c", "%U:%G %a", transport.SocketPath(guest.AgentSocketDir, transport.PortTCPProxy)}, "root:shed-link 660"},
			{"link user's password field", []string{"sh", "-c", "getent shadow shed-link | cut -d: -f2"}, "*"},
			{"sshd MaxSessions", []string{"sh", "-c", "sshd -T | grep -i '^maxsessions '"}, "maxsessions 10"},
			{"sshd TCP forwarding", []string{"sh", "-c", "sshd -T | grep -i '^allowtcpforwarding '"}, "allowtcpforwarding local"},
			{"sshd unix forwarding", []string{"sh", "-c", "sshd -T | grep -i '^allowstreamlocalforwarding '"}, "allowstreamlocalforwarding local"},
			{"sshd forwarding targets", []string{"sh", "-c", "sshd -T | grep -i '^permitopen '"},
				"permitopen [/run/shed-agent/1024.sock]:* [/run/shed-agent/1026.sock]:* [/run/shed-agent/1028.sock]:*"},
			{"sshd users", []string{"sh", "-c", "sshd -T | grep -i '^allowusers '"}, "allowusers shed-link"},
			{"sshd root login", []string{"sh", "-c", "sshd -T | grep -i '^permitrootlogin '"}, "permitrootlogin no"},
			{"sshd passwords", []string{"sh", "-c", "sshd -T | grep -i '^passwordauthentication '"}, "passwordauthentication no"},
			{"sshd host keys", []string{"sh", "-c", "sshd -T | grep -i '^hostkey '"}, "hostkey /etc/ssh/ssh_host_ed25519_key"},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				if got := f.sshd.Exec(t, tt.args...); got != tt.want {
					t.Errorf("%v = %q, want %q", tt.args, got, tt.want)
				}
			})
		}
		t.Logf("sshd: %s", f.sshd.Exec(t, "sh", "-c", "sshd -V 2>&1 || true"))
	})
}

// TestSSHDRefusesWhatTheGuestPolicyForbids checks the policy side of guest
// layout 1 from the link's point of view.
func TestSSHDRefusesWhatTheGuestPolicyForbids(t *testing.T) {
	f := newSSHDFixture(t, "", nil)
	ctx := testCtx(t)

	t.Run("a wrong pin is a mismatch", func(t *testing.T) {
		link, err := f.factory.Open(ctx, transport.Target{
			Addr:     f.sshd.Addr,
			HostKeys: PinnedHostKey(sshtest.PublicKeyLine(sshtest.NewKey(t).PublicKey())),
		})
		if err != nil {
			t.Fatal(err)
		}
		defer link.Close()
		if _, err := link.Dial(ctx, transport.PortNotify); !errors.Is(err, transport.ErrHostKeyMismatch) {
			t.Errorf("Dial = %v, want ErrHostKeyMismatch", err)
		}
	})

	for _, user := range []string{"root", "shed", "ubuntu"} {
		t.Run("login as "+user+" is refused", func(t *testing.T) {
			factory, err := New(Config{Keys: []ssh.Signer{f.key}, User: user, Logf: f.logs.logf})
			if err != nil {
				t.Fatal(err)
			}
			link, err := factory.Open(ctx, transport.Target{Addr: f.sshd.Addr, HostKeys: PinnedHostKey(f.sshd.HostKey())})
			if err != nil {
				t.Fatal(err)
			}
			defer link.Close()
			if _, err := link.Dial(ctx, transport.PortNotify); !errors.Is(err, transport.ErrAuth) {
				t.Errorf("Dial = %v, want ErrAuth", err)
			}
		})
	}

	t.Run("an unknown key is refused", func(t *testing.T) {
		factory, err := New(Config{Keys: []ssh.Signer{sshtest.NewKey(t)}, Logf: f.logs.logf})
		if err != nil {
			t.Fatal(err)
		}
		link, err := factory.Open(ctx, transport.Target{Addr: f.sshd.Addr, HostKeys: PinnedHostKey(f.sshd.HostKey())})
		if err != nil {
			t.Fatal(err)
		}
		defer link.Close()
		if _, err := link.Dial(ctx, transport.PortNotify); !errors.Is(err, transport.ErrAuth) {
			t.Errorf("Dial = %v, want ErrAuth", err)
		}
	})

	// The rest speaks to sshd directly, through the link's own
	// connection, to ask for things the link never asks for.
	link := f.open(t)
	c, err := link.connection(ctx)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}

	t.Run("TCP forwarding is refused", func(t *testing.T) {
		for _, target := range []string{"127.0.0.1:498", "127.0.0.1:22", "192.0.2.1:80"} {
			conn, err := c.client.Dial("tcp", target)
			if err == nil {
				conn.Close()
				t.Fatalf("sshd forwarded a TCP connection to %s", target)
			}
			var refused *ssh.OpenChannelError
			if !errors.As(err, &refused) || refused.Reason != ssh.Prohibited {
				t.Errorf("direct-tcpip to %s = %v, want administratively prohibited", target, err)
			}
		}
	})

	// A second path to an agent socket. The kernel would connect it (it
	// is the same socket, and the link user may open it), so only sshd's
	// list of forwarding targets stands in the way.
	t.Run("a socket that is not one of the agent's three is refused", func(t *testing.T) {
		const alias = "/tmp/alias.sock"
		f.sshd.Exec(t, "ln", transport.SocketPath(guest.AgentSocketDir, transport.PortNotify), alias)
		conn, err := c.client.Dial("unix", alias)
		if err == nil {
			conn.Close()
			t.Fatal("sshd forwarded to a socket outside its PermitOpen list")
		}
		if logs := f.sshd.Logs(); !strings.Contains(logs, "connect to path "+alias+", but the request was denied") {
			t.Errorf("sshd's log does not show a policy refusal for %s:\n%s", alias, logs)
		}
	})

	t.Run("a remote unix-socket listener is refused", func(t *testing.T) {
		ln, err := c.client.ListenUnix("/tmp/shed-link-test.sock")
		if err == nil {
			ln.Close()
			t.Fatal("sshd agreed to listen on a unix socket in the guest for the server")
		}
	})

	t.Run("a remote TCP listener is refused", func(t *testing.T) {
		ln, err := c.client.Listen("tcp", "127.0.0.1:0")
		if err == nil {
			ln.Close()
			t.Fatal("sshd agreed to listen on a TCP port in the guest for the server")
		}
	})

	t.Run("a terminal is refused", func(t *testing.T) {
		session, err := c.client.NewSession()
		if err != nil {
			t.Fatalf("NewSession: %v", err)
		}
		defer session.Close()
		if err := session.RequestPty("xterm", 24, 80, ssh.TerminalModes{}); err == nil {
			t.Error("sshd allocated a terminal for the link user")
		}
	})

	// The policy refuses things; it must not have refused the link itself.
	t.Run("the agent's sockets are still reachable", func(t *testing.T) {
		for _, port := range []uint32{transport.PortConsole, transport.PortNotify, transport.PortTCPProxy} {
			conn, err := link.Dial(ctx, port)
			if err != nil {
				t.Errorf("Dial port %d: %v", port, err)
				continue
			}
			conn.Close()
		}
	})
}

// TestSSHDForwardingPolicy shows why guest.SSHDConfig's forwarding lines are
// what they are, by running sshd with each simpler alternative. These are
// facts about OpenSSH 9.6 that the plan could not settle on paper; if a later
// OpenSSH changes one, this test says so and the policy can be simplified.
func TestSSHDForwardingPolicy(t *testing.T) {
	t.Run("AllowTcpForwarding no also refuses unix sockets", func(t *testing.T) {
		f := newSSHDFixture(t, "AllowTcpForwarding no", nil)
		ctx := testCtx(t)
		link := f.open(t)
		_, err := link.Dial(ctx, transport.PortNotify)
		if err == nil {
			t.Fatal("unix-socket forwarding works with AllowTcpForwarding no: this OpenSSH no longer ties the two, " +
				"and guest.SSHDConfig could say \"AllowTcpForwarding no\" and drop PermitOpen")
		}
		if !errors.Is(err, transport.ErrUnreachable) {
			t.Errorf("Dial = %v, want ErrUnreachable", err)
		}
		if got := f.sshd.Exec(t, "sh", "-c", "sshd -T | grep -i '^allowstreamlocalforwarding '"); got != "allowstreamlocalforwarding local" {
			t.Errorf("%q: the refusal must not be for this setting", got)
		}
		if logs := f.sshd.Logs(); !strings.Contains(logs, "but the request was denied") {
			t.Errorf("sshd's log does not show a policy refusal:\n%s", logs)
		}
		// The SSH connection itself was fine.
		if _, err := link.connection(ctx); err != nil {
			t.Errorf("the SSH connection: %v", err)
		}
	})

	t.Run("AllowTcpForwarding local without PermitOpen forwards TCP anywhere", func(t *testing.T) {
		f := newSSHDFixture(t, "PermitOpen any", nil)
		ctx := testCtx(t)
		link := f.open(t)
		c, err := link.connection(ctx)
		if err != nil {
			t.Fatalf("connect: %v", err)
		}
		conn, err := c.client.Dial("tcp", "127.0.0.1:498")
		if err != nil {
			t.Fatalf("direct-tcpip without PermitOpen = %v; if sshd now refuses this, PermitOpen is no longer needed", err)
		}
		conn.Close()
		// And any socket the link user can open, not just the agent's.
		const alias = "/tmp/alias.sock"
		f.sshd.Exec(t, "ln", transport.SocketPath(guest.AgentSocketDir, transport.PortNotify), alias)
		conn, err = c.client.Dial("unix", alias)
		if err != nil {
			t.Fatalf("a second path to an agent socket = %v, want it forwarded without PermitOpen", err)
		}
		conn.Close()
	})
}

// TestSSHDFacts records how OpenSSH behaves under a bare x/crypto client,
// without the link's stream wrapper. Nothing here is a requirement on sshd;
// the suite above holds the requirements. The log lines are the answers to
// the plan's open questions, and the few assertions are the facts the link's
// design leans on, so a new OpenSSH that changes one is noticed.
func TestSSHDFacts(t *testing.T) {
	f := newSSHDFixture(t, "", nil)
	ctx := testCtx(t)
	link := f.open(t)
	c, err := link.connection(ctx)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	client := c.client
	t.Logf("server version: %s", client.ServerVersion())
	notify := transport.SocketPath(guest.AgentSocketDir, transport.PortNotify)

	t.Run("MaxSessions counts sessions, not forwarded channels", func(t *testing.T) {
		// Forwarded channels first: far more than MaxSessions (10).
		var forwards []net.Conn
		defer func() {
			for _, c := range forwards {
				c.Close()
			}
		}()
		for i := range 40 {
			conn, err := client.Dial("unix", notify)
			if err != nil {
				t.Fatalf("forwarded channel %d of 40 was refused: %v", i+1, err)
			}
			forwards = append(forwards, conn)
		}

		// Then sessions, each kept open by a running command, until
		// sshd refuses one.
		var sessions []*ssh.Session
		defer func() {
			for _, s := range sessions {
				s.Close()
			}
		}()
		refusedAt, reason := 0, ""
		for i := 1; i <= 16; i++ {
			s, err := client.NewSession()
			if err != nil {
				refusedAt, reason = i, err.Error()
				break
			}
			sessions = append(sessions, s)
			if err := s.Start("sleep 30"); err != nil {
				refusedAt, reason = i, "start: "+err.Error()
				break
			}
		}
		t.Logf("with 40 forwarded channels open, session %d was refused: %s", refusedAt, reason)
		if refusedAt != 11 {
			t.Errorf("sshd refused session %d, want the 11th (MaxSessions 10, forwarded channels not counted)", refusedAt)
		}

		// And forwarded channels still open with every session slot
		// taken.
		conn, err := client.Dial("unix", notify)
		if err != nil {
			t.Errorf("a forwarded channel was refused with all sessions in use: %v", err)
		} else {
			conn.Close()
		}
	})

	t.Run("a channel close is answered at once when the guest side is idle", func(t *testing.T) {
		conn, err := client.Dial("unix", notify)
		if err != nil {
			t.Fatal(err)
		}
		readDone := make(chan error, 1)
		go func() { _, err := conn.Read(make([]byte, 1)); readDone <- err }()
		time.Sleep(100 * time.Millisecond)
		start := time.Now()
		conn.Close()
		select {
		case err := <-readDone:
			t.Logf("bare channel: Read returned %v after Close (%v)", time.Since(start).Round(time.Microsecond), err)
		case <-time.After(5 * time.Second):
			t.Error("bare channel: sshd did not answer a close within 5 s on an idle stream")
		}
	})

	t.Run("a channel close is not answered while the guest side is not reading", func(t *testing.T) {
		// The console port, with a command that never reads stdin: the
		// agent stops reading the socket once the command's pipe is
		// full, and sshd's buffer for the channel fills behind it.
		conn, err := client.Dial("unix", transport.SocketPath(guest.AgentSocketDir, transport.PortConsole))
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		if err := writeExecRequest(conn, "exec sleep 4"); err != nil {
			t.Fatal(err)
		}
		writeDone := make(chan error, 1)
		progress := make(chan struct{}, 1)
		go func() {
			frame := make([]byte, 32*1024)
			for {
				if err := writeDataFrame(conn, frame); err != nil {
					writeDone <- err
					return
				}
				select {
				case progress <- struct{}{}:
				default:
				}
			}
		}()
		for stalled := false; !stalled; {
			select {
			case <-progress:
			case err := <-writeDone:
				t.Fatalf("Write ended without blocking: %v", err)
			case <-time.After(750 * time.Millisecond):
				stalled = true
			}
		}

		start := time.Now()
		conn.Close()
		select {
		case err := <-writeDone:
			d := time.Since(start)
			t.Logf("bare channel: a blocked Write returned %v after Close (%v)", d.Round(time.Millisecond), err)
			if d < time.Second {
				t.Logf("NOTE: this sshd answers a close without draining; the stream wrapper is then belt and braces")
			}
		case <-time.After(20 * time.Second):
			t.Logf("bare channel: a blocked Write was still blocked 20 s after Close")
		}
	})

	t.Run("half-close crosses sshd both ways", func(t *testing.T) {
		proxy := transport.SocketPath(guest.AgentSocketDir, transport.PortTCPProxy)
		conn, err := client.Dial("unix", proxy)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		if _, err := io.WriteString(conn, "CONNECT 498\n"); err != nil {
			t.Fatal(err)
		}
		ok := make([]byte, 3)
		if _, err := io.ReadFull(conn, ok); err != nil || string(ok) != "OK\n" {
			t.Fatalf("CONNECT reply %q, %v", ok, err)
		}
		// A request cut short: the agent's HTTP handler answers only
		// once it sees end-of-file on the request body.
		if _, err := io.WriteString(conn, "POST /v1/publish HTTP/1.0\r\nContent-Length: 4096\r\n\r\n{\"namespace\":"); err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		if err := conn.(interface{ CloseWrite() error }).CloseWrite(); err != nil {
			t.Fatal(err)
		}
		timer := time.AfterFunc(10*time.Second, func() { client.Close() })
		defer timer.Stop()
		reply, err := io.ReadAll(conn)
		if err != nil {
			t.Fatalf("reading the reply: %v", err)
		}
		line, _, _ := strings.Cut(string(reply), "\r\n")
		t.Logf("bare channel: after CloseWrite the reply %q and end-of-file took %v", line, time.Since(start).Round(time.Millisecond))
		if !strings.Contains(line, "400") {
			t.Errorf("reply %q, want a 400", line)
		}
	})
}

// TestSSHDLatency guards the quick-ACK setting on the link's TCP connection
// (see quickAck). sshd keeps Nagle's algorithm on for a connection with no
// terminal, so against a client that delays its ACKs every small exchange
// costs a 40 ms timer. The check is a median over many probes with a wide
// margin: about 0.2 ms with the setting, 41 ms without.
func TestSSHDLatency(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("quick-ACK is a Linux socket option")
	}
	f := newSSHDFixture(t, "", nil)
	direct, ok := f.sshd.DirectAddr()
	if !ok {
		t.Skip("the container's own address is not reachable from this host; a published port hides the effect")
	}
	ctx := testCtx(t)
	link, err := f.factory.Open(ctx, transport.Target{
		ShedName: "sshd-latency",
		Addr:     func(context.Context) (string, error) { return direct, nil },
		HostKeys: PinnedHostKey(f.sshd.HostKey()),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer link.Close()
	client := vmutil.NewAgentClient(link, transport.PortConsole, transport.PortNotify)
	if _, err := client.WaitForProbe(ctx, 20*time.Second); err != nil {
		t.Fatalf("no probe succeeded: %v", err)
	}

	median := func(n int, fn func() error) time.Duration {
		t.Helper()
		times := make([]time.Duration, 0, n)
		for range n {
			start := time.Now()
			if err := fn(); err != nil {
				t.Fatal(err)
			}
			times = append(times, time.Since(start))
		}
		slices.Sort(times)
		return times[n/2]
	}
	probe := median(41, func() error { _, err := client.Probe(ctx); return err })
	exec := median(41, func() error {
		return client.Exec(ctx, backend.ExecOptions{Cmd: []string{"/bin/sh", "-c", "echo hi"}, Stdout: vmutil.NopWriteCloser(io.Discard)})
	})
	t.Logf("median over 41: probe %v, exec of \"echo hi\" %v", probe.Round(10*time.Microsecond), exec.Round(10*time.Microsecond))
	const limit = 20 * time.Millisecond
	if probe > limit || exec > limit {
		t.Errorf("median probe %v, median exec %v; want both under %v. "+
			"About 40 ms is the delayed-ACK timer: the link's quick-ACK is not taking effect", probe, exec, limit)
	}
}
