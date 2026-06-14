package egress

import (
	"bufio"
	"context"
	"net"
	"strings"
	"testing"
)

func parse(t *testing.T, raw string) proxyTarget {
	t.Helper()
	pt, err := parseProxyRequest(bufio.NewReader(strings.NewReader(raw)))
	if err != nil {
		t.Fatalf("parseProxyRequest(%q): %v", raw, err)
	}
	return pt
}

func TestParseProxyRequest(t *testing.T) {
	c := parse(t, "CONNECT api.github.com:443 HTTP/1.1\r\nHost: api.github.com:443\r\n\r\n")
	if c.proto != "https" || c.host != "api.github.com" || c.port != 443 {
		t.Errorf("CONNECT parsed = %+v", c)
	}

	h := parse(t, "GET http://example.com/path?q=1 HTTP/1.1\r\nHost: example.com\r\n\r\n")
	if h.proto != "http" || h.host != "example.com" || h.port != 80 {
		t.Errorf("absolute-form parsed = %+v", h)
	}

	// Origin-form (no absolute URI) is not a valid forward-proxy request → error.
	if _, err := parseProxyRequest(bufio.NewReader(strings.NewReader("GET /path HTTP/1.1\r\nHost: evil.com\r\n\r\n"))); err == nil {
		t.Error("expected origin-form request to be rejected")
	}
}

func fakeResolver(m map[string][]string) Resolver {
	return func(_ context.Context, host string) ([]net.IP, error) {
		ss, ok := m[host]
		if !ok {
			return nil, &net.DNSError{Err: "no such host", Name: host}
		}
		var ips []net.IP
		for _, s := range ss {
			ips = append(ips, net.ParseIP(s))
		}
		return ips, nil
	}
}

func TestDecide(t *testing.T) {
	pol, err := CompilePolicy([]ProfileSpec{{Allow: []string{"*.github.com"}}}, "")
	if err != nil {
		t.Fatal(err)
	}
	h := &ConnHandler{
		Shed:   "web",
		Policy: pol,
		Resolve: fakeResolver(map[string][]string{
			"api.github.com": {"140.82.112.3"},
			"pypi.org":       {"151.101.0.223"},
			"metadata.evil":  {"169.254.169.254"},          // resolves to IMDS
			"rebind.evil":    {"140.82.112.3", "10.0.0.5"}, // one public, one private guard
		}),
	}

	cases := []struct {
		host        string
		wantVerdict string
		wantNilIP   bool
		reasonHas   string
	}{
		{"api.github.com", "allow", false, "allow:"},
		{"pypi.org", "deny", true, "default-deny"},
		{"metadata.evil", "deny", true, "guard:169.254.0.0/16"},
		{"rebind.evil", "deny", true, "guard:"}, // any resolved IP in a guard → deny
		{"nxdomain.example", "deny", true, "resolve-failed"},
		{"1.2.3.4", "deny", true, "bad-host"}, // IP literal not a host
	}
	for _, c := range cases {
		ip, rec := h.decide(context.Background(), c.host, 443, "https")
		if rec.Verdict != c.wantVerdict {
			t.Errorf("decide(%s) verdict = %s (%s), want %s", c.host, rec.Verdict, rec.Reason, c.wantVerdict)
		}
		if (ip == nil) != c.wantNilIP {
			t.Errorf("decide(%s) ip=%v, wantNil=%v", c.host, ip, c.wantNilIP)
		}
		if !strings.Contains(rec.Reason, c.reasonHas) {
			t.Errorf("decide(%s) reason=%q, want contains %q", c.host, rec.Reason, c.reasonHas)
		}
	}
}

func TestDecideAuditNoURL(t *testing.T) {
	// The audit record must never carry a path/query (token leakage).
	pol, _ := CompilePolicy([]ProfileSpec{{Mode: "audit"}}, "")
	h := &ConnHandler{Shed: "web", Policy: pol, Resolve: fakeResolver(map[string][]string{"x.com": {"1.1.1.1"}})}
	_, rec := h.decide(context.Background(), "x.com", 443, "https")
	if rec.Host != "x.com" || rec.ResolvedIP != "1.1.1.1" {
		t.Errorf("rec = %+v", rec)
	}
}
