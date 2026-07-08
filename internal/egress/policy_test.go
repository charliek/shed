package egress

import (
	"net"
	"testing"
)

func TestCanonicalizeHost(t *testing.T) {
	tests := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"GitHub.com", "github.com", false},
		{"api.github.com.", "api.github.com", false}, // trailing dot stripped
		{"  Example.COM ", "example.com", false},
		{"xn--n3h.com", "xn--n3h.com", false}, // already punycode
		{"☃.com", "xn--n3h.com", false},       // unicode → punycode
		{"", "", true},
		{"a..b", "", true},    // empty label
		{"1.2.3.4", "", true}, // IP literal is not a host
		{"::1", "", true},     // IPv6 literal
	}
	for _, tt := range tests {
		got, err := CanonicalizeHost(tt.in)
		if (err != nil) != tt.wantErr {
			t.Errorf("CanonicalizeHost(%q) err=%v wantErr=%v", tt.in, err, tt.wantErr)
			continue
		}
		if err == nil && got != tt.want {
			t.Errorf("CanonicalizeHost(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestGlobMatch_SuffixTrap(t *testing.T) {
	tests := []struct {
		host, glob string
		want       bool
	}{
		{"api.github.com", "*.github.com", true},
		{"a.b.github.com", "*.github.com", true},
		{"github.com", "*.github.com", false},     // bare domain not matched by *.
		{"evilgithub.com", "*.github.com", false}, // suffix trap
		{"github.com.evil.com", "*.github.com", false},
		{"github.com", "github.com", true},
		{"api.github.com", "github.com", false},
	}
	for _, tt := range tests {
		if got := globMatch(tt.host, tt.glob); got != tt.want {
			t.Errorf("globMatch(%q,%q) = %v, want %v", tt.host, tt.glob, got, tt.want)
		}
	}
}

func TestGuards(t *testing.T) {
	p, err := CompilePolicy([]ProfileSpec{{Mode: "audit"}}, "192.168.64.1")
	if err != nil {
		t.Fatal(err)
	}
	denied := []string{"169.254.169.254", "169.254.170.2", "10.1.2.3", "172.16.0.5", "192.168.1.1", "127.0.0.1", "192.168.64.1", "::1", "fe80::1", "fd00::1"}
	for _, ip := range denied {
		if _, d := p.MatchGuard(net.ParseIP(ip)); !d {
			t.Errorf("MatchGuard(%s) = not denied, want denied", ip)
		}
	}
	allowed := []string{"1.1.1.1", "140.82.112.3", "8.8.8.8"}
	for _, ip := range allowed {
		if _, d := p.MatchGuard(net.ParseIP(ip)); d {
			t.Errorf("MatchGuard(%s) = denied, want allowed", ip)
		}
	}
}

func TestEvaluate_GuardDeniesEvenInAudit(t *testing.T) {
	// audit mode allows everything by fallthrough, but guards still deny.
	p, err := CompilePolicy([]ProfileSpec{{Mode: "audit"}}, "")
	if err != nil {
		t.Fatal(err)
	}
	if !p.IsAudit() {
		t.Fatal("expected audit mode")
	}
	// non-guard host: audit allows
	if v, _ := p.Evaluate(ConnContext{Host: "example.com", Port: 443, ResolvedIP: "93.184.216.34"}); v != VerdictAllow {
		t.Errorf("audit non-guard = %v, want allow", v)
	}
	// hostname resolving to IMDS: guard denies post-resolution, even in audit
	if v, r := p.Evaluate(ConnContext{Host: "metadata.example", Port: 80, ResolvedIP: "169.254.169.254"}); v != VerdictDeny {
		t.Errorf("audit guard hit = %v (%s), want deny", v, r)
	}
}

func TestEvaluate_EnforceAllowlist(t *testing.T) {
	p, err := CompilePolicy([]ProfileSpec{
		{Allow: []string{"*.ubuntu.com"}},               // base
		{Allow: []string{"*.github.com", "github.com"}}, // github
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	if p.IsAudit() {
		t.Fatal("expected enforce mode")
	}
	cases := []struct {
		host string
		want Verdict
	}{
		{"api.github.com", VerdictAllow},
		{"github.com", VerdictAllow},
		{"archive.ubuntu.com", VerdictAllow},
		{"pypi.org", VerdictDeny},       // not allowlisted → default-deny
		{"evilgithub.com", VerdictDeny}, // suffix trap → deny
	}
	for _, c := range cases {
		if v, r := p.Evaluate(ConnContext{Host: c.host, Port: 443}); v != c.want {
			t.Errorf("Evaluate(%s) = %v (%s), want %v", c.host, v, r, c.want)
		}
	}
}

func TestEvaluate_DenyBeforeAllow(t *testing.T) {
	// a deny glob in the same profile takes precedence (it's listed first).
	p, err := CompilePolicy([]ProfileSpec{
		{Deny: []string{"secrets.github.com"}, Allow: []string{"*.github.com"}},
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := p.Evaluate(ConnContext{Host: "secrets.github.com", Port: 443}); v != VerdictDeny {
		t.Errorf("deny-before-allow = %v, want deny", v)
	}
	if v, _ := p.Evaluate(ConnContext{Host: "api.github.com", Port: 443}); v != VerdictAllow {
		t.Errorf("allowed sibling = %v, want allow", v)
	}
}

func TestEvaluate_CELRule(t *testing.T) {
	p, err := CompilePolicy([]ProfileSpec{
		{Rule: `host.endsWith(".corp.internal") && port == 443`},
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := p.Evaluate(ConnContext{Host: "db.corp.internal", Port: 443}); v != VerdictAllow {
		t.Errorf("CEL allow = %v, want allow", v)
	}
	if v, _ := p.Evaluate(ConnContext{Host: "db.corp.internal", Port: 22}); v != VerdictDeny {
		t.Errorf("CEL wrong port = %v, want deny (fallthrough)", v)
	}
	if v, _ := p.Evaluate(ConnContext{Host: "db.other.com", Port: 443}); v != VerdictDeny {
		t.Errorf("CEL wrong host = %v, want deny", v)
	}
}

func TestCompilePolicy_InvalidRuleFailsLoad(t *testing.T) {
	if err := ValidateProfile(ProfileSpec{Rule: `this is not cel ++`}); err == nil {
		t.Error("expected invalid CEL to fail validation")
	}
	if err := ValidateProfile(ProfileSpec{Rule: `host`}); err == nil {
		t.Error("expected non-bool CEL to fail validation")
	}
	if err := ValidateProfile(ProfileSpec{Allow: []string{"*.github.com"}}); err != nil {
		t.Errorf("valid profile failed validation: %v", err)
	}
}
