package vmutil

import (
	"strings"
	"testing"
)

func TestEgressProxyURL(t *testing.T) {
	withTok := EgressProxyEnv{Gateway: "192.168.64.1", Port: 20001, Token: "tok"}
	if got, want := withTok.ProxyURL(), "http://tok@192.168.64.1:20001"; got != want {
		t.Errorf("ProxyURL = %q, want %q", got, want)
	}
	noTok := EgressProxyEnv{Gateway: "10.0.0.1", Port: 8080}
	if got, want := noTok.ProxyURL(), "http://10.0.0.1:8080"; got != want {
		t.Errorf("ProxyURL (no token) = %q, want %q", got, want)
	}
}

func TestEgressEnvPairs(t *testing.T) {
	e := EgressProxyEnv{Gateway: "g", Port: 1, Token: "t", NoProxy: "localhost"}
	pairs := e.EnvPairs()
	want := []string{
		"HTTP_PROXY=http://t@g:1", "http_proxy=http://t@g:1",
		"HTTPS_PROXY=http://t@g:1", "https_proxy=http://t@g:1",
		"NO_PROXY=localhost", "no_proxy=localhost",
	}
	if len(pairs) != len(want) {
		t.Fatalf("EnvPairs len = %d, want %d (%v)", len(pairs), len(want), pairs)
	}
	for i := range want {
		if pairs[i] != want[i] {
			t.Errorf("EnvPairs[%d] = %q, want %q", i, pairs[i], want[i])
		}
	}
}

func TestBuildNoProxy(t *testing.T) {
	full := BuildNoProxy("192.168.64.1", "192.168.64.0/24")
	for _, sub := range []string{"localhost", "127.0.0.1", "::1", "192.168.64.0/24", "192.168.64.1", ".local"} {
		if !strings.Contains(full, sub) {
			t.Errorf("BuildNoProxy = %q, missing %q", full, sub)
		}
	}
	// Empty subnet/gateway are skipped, not rendered as empty entries.
	bare := BuildNoProxy("", "")
	if strings.Contains(bare, ",,") || strings.HasSuffix(bare, ",") {
		t.Errorf("BuildNoProxy with empties has malformed list: %q", bare)
	}
	if !strings.Contains(bare, "localhost") || !strings.Contains(bare, ".local") {
		t.Errorf("BuildNoProxy bare = %q, want loopback + .local", bare)
	}
}

func TestRenderEgressProfile(t *testing.T) {
	out := renderEgressProfile(EgressProxyEnv{Gateway: "g", Port: 1, Token: "t", NoProxy: "localhost"})
	if !strings.HasPrefix(out, egressManagedComment) {
		t.Errorf("profile missing managed-by header:\n%s", out)
	}
	for _, want := range []string{
		"export HTTP_PROXY=http://t@g:1",
		"export https_proxy=http://t@g:1",
		"export NO_PROXY=localhost",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("profile missing %q:\n%s", want, out)
		}
	}
}

func TestRenderDockerProxyDropIn(t *testing.T) {
	out := renderDockerProxyDropIn(EgressProxyEnv{Gateway: "g", Port: 2, Token: "t", NoProxy: "localhost,g"})
	for _, want := range []string{
		"[Service]",
		`Environment="HTTP_PROXY=http://t@g:2"`,
		`Environment="HTTPS_PROXY=http://t@g:2"`,
		`Environment="NO_PROXY=localhost,g"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("drop-in missing %q:\n%s", want, out)
		}
	}
}
