package egress

import (
	"bufio"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

// shortSock returns a UDS path short enough for the platform sun_path limit
// (~104 bytes on macOS). t.TempDir() lives under a long /var/folders/... path
// on macOS, so bind would fail; a /tmp-rooted dir keeps the path tiny.
func shortSock(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "egx")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "s")
}

// freePort returns a currently-unused TCP port on the loopback interface. There
// is an inherent (small) race between close and re-listen; acceptable for tests.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// serveControl wires a ProxyServer behind a fresh control UDS and returns a
// connected Client plus a channel that receives every streamed audit record.
func serveControl(t *testing.T, ps *ProxyServer) (*Client, <-chan AuditRecord) {
	t.Helper()
	sock := shortSock(t)
	cln, err := ListenControl(sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cln.Close() })
	go func() {
		c, err := cln.Accept()
		if err != nil {
			return
		}
		_ = ps.Serve(c)
	}()

	audits := make(chan AuditRecord, 16)
	client, err := DialControl(sock, func(rec AuditRecord) { audits <- rec })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	return client, audits
}

// dialGuest connects to a per-shed proxy listener, retrying until it is up.
func dialGuest(t *testing.T, port int) net.Conn {
	t.Helper()
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	for i := 0; i < 200; i++ {
		c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			return c
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("listener on %s never came up", addr)
	return nil
}

func TestControlChannel_ConfigureDenyAudit(t *testing.T) {
	ps := NewProxyServer()
	ps.Resolve = fakeResolver(map[string][]string{"pypi.org": {"151.101.0.223"}})
	client, audits := serveControl(t, ps)

	port := freePort(t)
	// Empty spec ⇒ enforce mode, no allows ⇒ default-deny. Configure blocks for
	// the ack, so a nil return proves the listener actually opened.
	if err := client.Configure("web", port, "", "", []ProfileSpec{{}}); err != nil {
		t.Fatalf("Configure: %v", err)
	}

	guest := dialGuest(t, port)
	defer guest.Close()
	if _, err := guest.Write([]byte("CONNECT pypi.org:443 HTTP/1.1\r\nHost: pypi.org:443\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	_, _ = bufio.NewReader(guest).ReadString('\n') // drain the 403 refusal

	select {
	case rec := <-audits:
		if rec.Shed != "web" || rec.Host != "pypi.org" || rec.Verdict != "deny" {
			t.Errorf("audit = %+v, want shed=web host=pypi.org verdict=deny", rec)
		}
		if rec.ResolvedIP != "151.101.0.223" {
			t.Errorf("audit resolved_ip = %q, want 151.101.0.223", rec.ResolvedIP)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no audit record streamed back")
	}
}

func TestControlChannel_ConfigureFailsClosed(t *testing.T) {
	ps := NewProxyServer()
	client, _ := serveControl(t, ps)

	// Occupy a port, then configure a shed onto it: the proxy's listen fails and
	// the failure must surface as a non-nil Configure error (abort+unwind path).
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	port := occupied.Addr().(*net.TCPAddr).Port

	if err := client.Configure("web", port, "", "", []ProfileSpec{{}}); err == nil {
		t.Fatal("expected Configure to fail when the port is already in use")
	}
}

func TestControlChannel_RemoveFreesPort(t *testing.T) {
	ps := NewProxyServer()
	client, _ := serveControl(t, ps)

	port := freePort(t)
	if err := client.Configure("web", port, "", "", []ProfileSpec{{}}); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	dialGuest(t, port).Close() // listener is up

	if err := client.Remove("web"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	var reln net.Listener
	var err error
	for i := 0; i < 200; i++ {
		reln, err = net.Listen("tcp", addr)
		if err == nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if reln == nil {
		t.Fatalf("port %d not freed after Remove: %v", port, err)
	}
	reln.Close()
}

func TestControlChannel_ConcurrentConfigure(t *testing.T) {
	// Multiple sheds configured concurrently must each get their own ack (ID
	// correlation) and their own listener.
	ps := NewProxyServer()
	ps.Resolve = fakeResolver(nil)
	client, _ := serveControl(t, ps)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			port := freePort(t)
			if err := client.Configure("shed-"+strconv.Itoa(n), port, "", "", []ProfileSpec{{}}); err != nil {
				t.Errorf("Configure shed-%d: %v", n, err)
				return
			}
			dialGuest(t, port).Close()
		}(i)
	}
	wg.Wait()
}
