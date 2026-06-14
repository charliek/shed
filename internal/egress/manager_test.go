package egress

import (
	"net"
	"testing"
)

// managerWithProxy returns a Manager backed by an in-process ProxyServer (no
// child process), so Configure/Remove exercise the real control channel and
// real listeners on the given port range.
func managerWithProxy(t *testing.T, lo, hi int) *Manager {
	t.Helper()
	ps := NewProxyServer()
	ps.Resolve = fakeResolver(nil)
	client, _ := serveControl(t, ps)
	return newManager(client, nil, "", lo, hi)
}

func TestManager_AllocPortLocked(t *testing.T) {
	// Pure allocation math — no network, ports are just integers here.
	m := newManager(nil, nil, "", 20000, 20002)
	m.mu.Lock()
	defer m.mu.Unlock()

	seen := map[int]bool{}
	for _, shed := range []string{"a", "b", "c"} {
		p, err := m.allocPortLocked(shed)
		if err != nil {
			t.Fatalf("alloc %s: %v", shed, err)
		}
		if p < 20000 || p > 20002 {
			t.Errorf("alloc %s = %d, out of range", shed, p)
		}
		if seen[p] {
			t.Errorf("alloc %s = %d, duplicate", shed, p)
		}
		seen[p] = true
	}
	if _, err := m.allocPortLocked("d"); err == nil {
		t.Error("expected exhaustion error when range is full")
	}
}

func TestManager_ConfigureAllocatesAndOpens(t *testing.T) {
	lo := freePort(t)
	m := managerWithProxy(t, lo, lo) // single-port range ⇒ deterministic allocation

	port, token, err := m.Configure("web", 0, "", "", []ProfileSpec{{}})
	if err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if port != lo {
		t.Errorf("allocated port = %d, want %d", port, lo)
	}
	if token == "" {
		t.Error("expected a minted token, got empty")
	}
	dialGuest(t, port).Close() // listener is actually open
}

func TestManager_ConfigureReusesProvidedPortAndToken(t *testing.T) {
	m := managerWithProxy(t, 1, 65535)
	p := freePort(t)

	port, token, err := m.Configure("web", p, "tok-123", "", []ProfileSpec{{}})
	if err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if port != p {
		t.Errorf("port = %d, want provided %d", port, p)
	}
	if token != "tok-123" {
		t.Errorf("token = %q, want provided tok-123", token)
	}
}

func TestManager_RemoveFreesPort(t *testing.T) {
	lo := freePort(t)
	m := managerWithProxy(t, lo, lo)

	port, _, err := m.Configure("web", 0, "", "", []ProfileSpec{{}})
	if err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if err := m.Remove("web"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	m.mu.Lock()
	_, stillTracked := m.sheds["web"]
	m.mu.Unlock()
	if stillTracked {
		t.Error("shed still tracked after Remove")
	}
	// A second Configure (range is the single freed port) must reuse it.
	port2, _, err := m.Configure("web2", 0, "", "", []ProfileSpec{{}})
	if err != nil {
		t.Fatalf("re-Configure after Remove: %v", err)
	}
	if port2 != port {
		t.Errorf("re-allocated port = %d, want freed %d", port2, port)
	}
}

func TestManager_ConfigureFailureReleasesPort(t *testing.T) {
	m := managerWithProxy(t, 1, 65535)

	// Occupy a port so the proxy's listen fails; the Manager must not keep the
	// reservation for the failed shed.
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	p := occupied.Addr().(*net.TCPAddr).Port

	if _, _, err := m.Configure("web", p, "", "", []ProfileSpec{{}}); err == nil {
		t.Fatalf("expected Configure to fail on occupied port %d", p)
	}
	m.mu.Lock()
	_, tracked := m.sheds["web"]
	_, used := m.used[p]
	m.mu.Unlock()
	if tracked || used {
		t.Errorf("failed Configure leaked reservation: tracked=%v used=%v", tracked, used)
	}
}
