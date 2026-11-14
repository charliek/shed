package remote

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/remote/provider"
	"github.com/charliek/shed/internal/remote/provider/fake"
)

// echoServer is an https server that answers like a "what is my address"
// service, or misbehaves as a test tells it to.
type echoServer struct {
	srv *httptest.Server

	mu       sync.Mutex
	body     string
	status   int
	redirect string
	hits     int
}

func newEcho(t *testing.T, body string) *echoServer {
	t.Helper()
	e := &echoServer{body: body, status: http.StatusOK}
	e.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.mu.Lock()
		e.hits++
		body, status, redirect := e.body, e.status, e.redirect
		e.mu.Unlock()
		if redirect != "" {
			http.Redirect(w, r, redirect, http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(e.srv.Close)
	return e
}

func (e *echoServer) set(body string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.body, e.status, e.redirect = body, http.StatusOK, ""
}

func (e *echoServer) answer(status int, body string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.body, e.status, e.redirect = body, status, ""
}

func (e *echoServer) redirectTo(url string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.redirect = url
}

func (e *echoServer) hitCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.hits
}

// echoes starts one echo server per body. Every httptest TLS server presents
// the same test certificate, so the first server's client trusts them all.
func echoes(t *testing.T, bodies ...string) ([]*echoServer, []string, *http.Client) {
	t.Helper()
	servers := make([]*echoServer, len(bodies))
	urls := make([]string, len(bodies))
	for i, b := range bodies {
		servers[i] = newEcho(t, b)
		urls[i] = servers[i].srv.URL
	}
	return servers, urls, servers[0].srv.Client()
}

func TestPublicAddrAgreement(t *testing.T) {
	const a, b = "203.0.113.7", "203.0.113.8"
	down := func(e *echoServer) { e.answer(http.StatusServiceUnavailable, "") }
	closed := func(e *echoServer) { e.srv.Close() }
	tests := []struct {
		name   string
		bodies []string
		// arrange changes server i before the question is asked.
		arrange map[int]func(*echoServer)
		want    string // the agreed address, or "" for none
		// wantNote is a phrase the summary must hold.
		wantNote string
	}{
		{name: "two agree", bodies: []string{a, a}, want: a},
		{name: "two agree, with a trailing newline", bodies: []string{a + "\n", " " + a + "\r\n"}, want: a},
		{name: "two disagree", bodies: []string{a, b}},
		{name: "one of two answers 503", bodies: []string{a, a}, arrange: map[int]func(*echoServer){1: down}, wantNote: "answered HTTP 503"},
		{name: "one of two is unreachable", bodies: []string{a, a}, arrange: map[int]func(*echoServer){1: closed}, wantNote: "no answer"},
		{name: "one of three is down and two agree", bodies: []string{a, a, a}, arrange: map[int]func(*echoServer){0: down}, want: a},
		{name: "three all differ", bodies: []string{a, b, "203.0.113.9"}},
		{name: "two against one", bodies: []string{a, b, a}, want: a},
		{name: "two against two", bodies: []string{a, b, a, b}},
		{name: "a single url is never enough", bodies: []string{a}},

		{name: "private 10/8", bodies: []string{"10.0.0.5", "10.0.0.5"}, wantNote: "not a public address"},
		{name: "private 172.16/12", bodies: []string{"172.16.4.4", "172.16.4.4"}, wantNote: "not a public address"},
		{name: "private 192.168/16", bodies: []string{"192.168.1.10", "192.168.1.10"}, wantNote: "not a public address"},
		{name: "loopback", bodies: []string{"127.0.0.1", "127.0.0.1"}, wantNote: "not a public address"},
		{name: "link-local", bodies: []string{"169.254.169.254", "169.254.169.254"}, wantNote: "not a public address"},
		{name: "unspecified", bodies: []string{"0.0.0.0", "0.0.0.0"}, wantNote: "not a public address"},
		{name: "carrier-grade nat", bodies: []string{"100.64.0.9", "100.64.0.9"}, wantNote: "not a public address"},
		{name: "multicast", bodies: []string{"224.0.0.1", "224.0.0.1"}, wantNote: "not a public address"},
		{name: "broadcast", bodies: []string{"255.255.255.255", "255.255.255.255"}, wantNote: "not a public address"},

		{name: "garbage words", bodies: []string{"hello", "hello"}, wantNote: "not one IPv4 address"},
		{name: "html around an address", bodies: []string{"<html>" + a + "</html>", "<html>" + a + "</html>"}, wantNote: "not one IPv4 address"},
		{name: "two addresses on a line", bodies: []string{a + " " + a, a + " " + a}, wantNote: "not one IPv4 address"},
		{name: "two addresses on two lines", bodies: []string{a + "\n" + b, a + "\n" + b}, wantNote: "not one IPv4 address"},
		{name: "a cidr", bodies: []string{a + "/32", a + "/32"}, wantNote: "not one IPv4 address"},
		{name: "json", bodies: []string{`{"ip":"` + a + `"}`, `{"ip":"` + a + `"}`}, wantNote: "not one IPv4 address"},
		{name: "ipv6", bodies: []string{"2001:db8::1", "2001:db8::1"}, wantNote: "not one IPv4 address"},
		{name: "ipv4 mapped in ipv6", bodies: []string{"::ffff:" + a, "::ffff:" + a}, wantNote: "not one IPv4 address"},
		{name: "empty", bodies: []string{"", ""}, wantNote: "not one IPv4 address"},

		// The address is right and the body is too long: it is not read as
		// an address, even though trimming it would leave one.
		{name: "oversized body", bodies: []string{a + strings.Repeat("\n", echoMaxBody), a + strings.Repeat("\n", echoMaxBody)}, wantNote: "more than 64 bytes"},
		{name: "one byte over", bodies: []string{a + strings.Repeat(" ", echoMaxBody+1-len(a)), a}, wantNote: "more than 64 bytes"},
		{name: "exactly the limit", bodies: []string{a + strings.Repeat(" ", echoMaxBody-len(a)), a}, want: a},

		{name: "a 404 that carries an address", bodies: []string{a, a}, arrange: map[int]func(*echoServer){
			0: func(e *echoServer) { e.answer(http.StatusNotFound, a) },
		}, wantNote: "answered HTTP 404"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			servers, urls, client := echoes(t, tt.bodies...)
			for i, fn := range tt.arrange {
				fn(servers[i])
			}
			addr, ok, summary := newAddrFinder(urls, client).agreed(context.Background())
			got := ""
			if ok {
				got = addr.String()
			}
			if got != tt.want {
				t.Errorf("agreed = %q (%v), want %q; summary: %s", got, ok, tt.want, summary)
			}
			if !ok && addr.IsValid() {
				t.Errorf("no agreement but an address was returned: %s", addr)
			}
			if !strings.Contains(summary, tt.wantNote) {
				t.Errorf("summary lacks %q: %s", tt.wantNote, summary)
			}
		})
	}
}

func TestPublicAddrRedirectNotFollowed(t *testing.T) {
	const a = "203.0.113.7"
	servers, urls, client := echoes(t, a, a)
	// The first service redirects to the second, which answers correctly.
	// Followed, that would be two "agreeing" answers from one service.
	servers[0].redirectTo(servers[1].srv.URL)
	addr, ok, summary := newAddrFinder(urls, client).agreed(context.Background())
	if ok {
		t.Errorf("believed %s on one real answer", addr)
	}
	if !strings.Contains(summary, "answered HTTP 302") {
		t.Errorf("summary = %s", summary)
	}
	if n := servers[1].hitCount(); n != 1 {
		t.Errorf("the redirect target was asked %d times, want 1 (its own question only)", n)
	}
}

func TestPublicAddrHTTPSOnly(t *testing.T) {
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("a plain-http echo URL was requested")
		_, _ = w.Write([]byte("203.0.113.7"))
	}))
	defer plain.Close()
	f := newAddrFinder([]string{plain.URL, plain.URL}, plain.Client())
	if _, err := f.ask(context.Background(), plain.URL); err == nil || !strings.Contains(err.Error(), "not an https URL") {
		t.Errorf("ask over http = %v", err)
	}
	if addr, ok, _ := f.agreed(context.Background()); ok {
		t.Errorf("believed %s from plain http", addr)
	}
}

func TestPublicIPv4(t *testing.T) {
	tests := []struct {
		addr string
		want bool
	}{
		{"203.0.113.7", true}, {"198.51.100.1", true}, {"192.0.2.1", true}, {"8.8.8.8", true},
		{"10.1.2.3", false}, {"172.31.255.255", false}, {"192.168.0.1", false}, {"127.0.0.53", false},
		{"169.254.0.1", false}, {"0.0.0.0", false}, {"100.64.0.1", false}, {"100.127.255.255", false},
		{"224.0.0.251", false}, {"255.255.255.255", false}, {"2001:db8::1", false}, {"::ffff:203.0.113.7", false},
	}
	for _, tt := range tests {
		t.Run(tt.addr, func(t *testing.T) {
			if got := publicIPv4(netip.MustParseAddr(tt.addr)); got != tt.want {
				t.Errorf("publicIPv4 = %v, want %v", got, tt.want)
			}
		})
	}
	if publicIPv4(netip.Addr{}) {
		t.Error("the zero address is public")
	}
}

// --- auto mode on the backend ---

type autoEnv struct {
	*env
	clock   *testClock
	servers []*echoServer
}

// newAutoEnv builds a backend in firewall mode auto over echo servers that
// answer the given bodies. The fake's rule starts with ruleSource. The
// backend's start-up check has already run when it returns.
func newAutoEnv(t *testing.T, ruleSource string, bodies []string, more ...envOpt) *autoEnv {
	t.Helper()
	servers, urls, client := echoes(t, bodies...)
	clock := newTestClock()
	opts := []envOpt{
		withClock(clock),
		func(e *env, o *fake.Options) {
			e.cfg.Firewall = config.RemoteFirewallConfig{Mode: config.RemoteFirewallAuto, EchoURLs: urls}
			o.FirewallSources = []string{ruleSource}
		},
		tune(func(o *Options) { o.EchoClient, o.FirewallSources = client, nil }),
	}
	return &autoEnv{env: newEnv(t, append(opts, more...)...), clock: clock, servers: servers}
}

func (a *autoEnv) setAll(body string) {
	for _, s := range a.servers {
		s.set(body)
	}
}

func (a *autoEnv) downAll() {
	for _, s := range a.servers {
		s.answer(http.StatusBadGateway, "")
	}
}

func (a *autoEnv) ruleSources() []string {
	a.t.Helper()
	rule, err := a.prov.EnvFirewallRule(context.Background())
	if err != nil {
		a.t.Fatalf("EnvFirewallRule: %v", err)
	}
	return rule.SourceRanges
}

// check moves the clock and runs one check.
func (a *autoEnv) check(after time.Duration) FirewallStatus {
	a.t.Helper()
	a.clock.Advance(after)
	return a.b.CheckFirewall(ctxT(a.t))
}

func TestFirewallAutoPatchesOnAgreement(t *testing.T) {
	const addr = "203.0.113.7"
	a := newAutoEnv(t, "192.0.2.1/32", []string{addr, addr})
	s := a.b.FirewallStatus()
	if !s.OK || !slices.Equal(s.Expected, []string{addr + "/32"}) {
		t.Fatalf("start-up status = %+v (%v)", s, s.Failure)
	}
	if got := a.prov.FirewallPatches(); len(got) != 1 || !slices.Equal(got[0], []string{addr + "/32"}) {
		t.Fatalf("patches = %v, want one to %s/32", got, addr)
	}
	if got := a.ruleSources(); !slices.Equal(got, []string{addr + "/32"}) {
		t.Errorf("rule sources = %v", got)
	}
	log := strings.Join(a.logLines(), "\n")
	if !strings.Contains(log, "patched rule shed-"+testEnv+"-ssh: sources were [192.0.2.1/32], are now ["+addr+"/32]") {
		t.Errorf("the patch's old and new sources are not in the log:\n%s", log)
	}
	// With the rule right, later checks patch nothing.
	for range 3 {
		if s := a.check(FirewallPatchInterval + time.Second); !s.OK {
			t.Fatalf("check failed: %v", s.Failure)
		}
	}
	if n := a.count(fake.OpSetFirewallSources); n != 1 {
		t.Errorf("%d patches, want 1", n)
	}
	if _, err := a.create("works"); err != nil {
		t.Errorf("create in auto mode: %v", err)
	}
	a.settled("works")
}

func TestFirewallAutoNoPatchWhenRuleIsRight(t *testing.T) {
	const addr = "203.0.113.7"
	a := newAutoEnv(t, addr+"/32", []string{addr, addr})
	if s := a.b.FirewallStatus(); !s.OK {
		t.Fatalf("start-up check failed: %v", s.Failure)
	}
	if n := a.count(fake.OpSetFirewallSources); n != 0 {
		t.Errorf("%d patches of a rule that was already right", n)
	}
}

// TestFirewallAutoNeverPatchesWithoutAgreement: every way the echo services
// can fail to agree leaves the rule alone and refuses new work.
func TestFirewallAutoNeverPatchesWithoutAgreement(t *testing.T) {
	const addr, other = "203.0.113.7", "203.0.113.8"
	tests := []struct {
		name    string
		bodies  []string
		arrange func(a []*echoServer)
		wantMsg string
	}{
		{name: "disagree", bodies: []string{addr, other}},
		{name: "one down", bodies: []string{addr, addr}, arrange: func(s []*echoServer) { s[1].answer(http.StatusInternalServerError, "") }, wantMsg: "answered HTTP 500"},
		{name: "one unreachable", bodies: []string{addr, addr}, arrange: func(s []*echoServer) { s[0].srv.Close() }, wantMsg: "no answer"},
		{name: "private address", bodies: []string{"10.20.30.40", "10.20.30.40"}, wantMsg: "not a public address"},
		{name: "loopback address", bodies: []string{"127.0.0.1", "127.0.0.1"}, wantMsg: "not a public address"},
		{name: "link-local address", bodies: []string{"169.254.10.10", "169.254.10.10"}, wantMsg: "not a public address"},
		{name: "unspecified address", bodies: []string{"0.0.0.0", "0.0.0.0"}, wantMsg: "not a public address"},
		{name: "garbage body", bodies: []string{"<!doctype html>", "<!doctype html>"}, wantMsg: "not one IPv4 address"},
		{name: "oversized body", bodies: []string{addr + strings.Repeat(" ", 100), addr + strings.Repeat(" ", 100)}, wantMsg: "more than 64 bytes"},
		{name: "redirect", bodies: []string{addr, addr}, arrange: func(s []*echoServer) { s[0].redirectTo(s[1].srv.URL) }, wantMsg: "answered HTTP 302"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			servers, urls, client := echoes(t, tt.bodies...)
			if tt.arrange != nil {
				tt.arrange(servers)
			}
			e := newEnv(t,
				func(e *env, _ *fake.Options) {
					e.cfg.Firewall = config.RemoteFirewallConfig{Mode: config.RemoteFirewallAuto, EchoURLs: urls}
				},
				tune(func(o *Options) { o.EchoClient, o.FirewallSources = client, nil }),
			)
			s := e.b.FirewallStatus()
			if s.OK || s.Failure == nil || s.Failure.Reason != FirewallAddressUnknown {
				t.Fatalf("status = %+v (%v), want reason %s", s, s.Failure, FirewallAddressUnknown)
			}
			for _, w := range []string{"public address is not known", "the firewall rule was not touched", tt.wantMsg} {
				if !strings.Contains(s.Failure.Error(), w) {
					t.Errorf("message lacks %q: %v", w, s.Failure)
				}
			}
			if n := e.count(fake.OpSetFirewallSources); n != 0 {
				t.Errorf("%d patches without agreement", n)
			}
			rule, err := e.prov.EnvFirewallRule(context.Background())
			if err != nil || !slices.Equal(rule.SourceRanges, []string{fake.DefaultFirewallSource}) {
				t.Errorf("the rule changed: %+v, %v", rule, err)
			}
			if _, err := e.create("refused"); !errors.Is(err, config.ErrFirewallCheckFailedSentinel) {
				t.Errorf("create = %v, want the firewall refusal", err)
			}
		})
	}
}

func TestFirewallAutoRateLimit(t *testing.T) {
	const first, second, third = "203.0.113.7", "203.0.113.8", "203.0.113.9"
	a := newAutoEnv(t, "192.0.2.1/32", []string{first, first})
	if n := a.count(fake.OpSetFirewallSources); n != 1 {
		t.Fatalf("%d patches at start-up, want 1", n)
	}

	// The address moves a minute later. The rule may not be patched again
	// yet, so the check fails and says when it will be repaired.
	a.setAll(second)
	s := a.check(time.Minute)
	if s.OK || s.Failure.Reason != FirewallRuleSources {
		t.Fatalf("status = %+v (%v)", s, s.Failure)
	}
	for _, w := range []string{"expected source ranges [" + second + "/32]", "found source ranges [" + first + "/32]", "not repaired yet", "at most once every 5m0s"} {
		if !strings.Contains(s.Failure.Error(), w) {
			t.Errorf("message lacks %q: %v", w, s.Failure)
		}
	}
	if _, err := a.create("waits"); !errors.Is(err, config.ErrFirewallCheckFailedSentinel) {
		t.Errorf("create inside the rate limit = %v", err)
	}
	// Still inside the five minutes, three checks later.
	for range 3 {
		a.check(time.Minute)
	}
	if n := a.count(fake.OpSetFirewallSources); n != 1 {
		t.Fatalf("%d patches inside the rate limit, want 1", n)
	}
	// Five minutes after the first patch it is repaired.
	s = a.check(time.Minute)
	if !s.OK {
		t.Fatalf("check after the rate limit: %v", s.Failure)
	}
	if got := a.prov.FirewallPatches(); len(got) != 2 || !slices.Equal(got[1], []string{second + "/32"}) {
		t.Fatalf("patches = %v", got)
	}

	// A patch that fails counts against the limit too: no hammering.
	a.setAll(third)
	a.prov.FailNext(fake.OpSetFirewallSources, provider.Errorf(nil, "set_firewall_sources", "HTTP 403: Required 'compute.firewalls.update' permission"))
	s = a.check(FirewallPatchInterval)
	if s.OK || !strings.Contains(s.Failure.Error(), "compute.firewalls.update") {
		t.Fatalf("status after a failed patch = %+v (%v)", s, s.Failure)
	}
	s = a.check(time.Minute)
	if s.OK || !strings.Contains(s.Failure.Error(), "not repaired yet") {
		t.Fatalf("status a minute after a failed patch = %+v (%v)", s, s.Failure)
	}
	if n := a.count(fake.OpSetFirewallSources); n != 3 {
		t.Errorf("%d patch attempts, want 3", n)
	}
	if got := a.ruleSources(); !slices.Equal(got, []string{second + "/32"}) {
		t.Errorf("rule sources = %v", got)
	}
}

func TestFirewallAutoFifteenMinuteRule(t *testing.T) {
	const addr = "203.0.113.7"
	a := newAutoEnv(t, addr+"/32", []string{addr, addr})
	if s := a.b.FirewallStatus(); !s.OK {
		t.Fatalf("start-up: %v", s.Failure)
	}

	// The echo services go quiet. The last agreed address is still
	// believed, the rule still matches it, and work carries on.
	a.downAll()
	if s := a.check(14 * time.Minute); !s.OK || !slices.Equal(s.Expected, []string{addr + "/32"}) {
		t.Fatalf("14 minutes without agreement: %+v (%v)", s, s.Failure)
	}
	if _, err := a.create("inside"); err != nil {
		t.Errorf("create at 14 minutes: %v", err)
	}
	a.settled("inside")

	// During that time a hand edit of the rule is reported and not undone:
	// nothing is patched on an old belief.
	a.prov.EditEnvRule(func(r *provider.FirewallRule) { r.SourceRanges = []string{"198.51.100.9/32"} })
	s := a.check(30 * time.Second)
	if s.OK || s.Failure.Reason != FirewallRuleSources || !strings.Contains(s.Failure.Error(), "did not agree in this check") {
		t.Fatalf("hand-edited rule without agreement: %+v (%v)", s, s.Failure)
	}
	a.prov.EditEnvRule(func(r *provider.FirewallRule) { r.SourceRanges = []string{addr + "/32"} })

	// At fifteen minutes the address is no longer believed.
	s = a.check(30 * time.Second)
	if s.OK || s.Failure.Reason != FirewallAddressUnknown {
		t.Fatalf("15 minutes without agreement: %+v (%v)", s, s.Failure)
	}
	for _, w := range []string{"have not agreed on one for 15m0s", "last agreed on " + addr, "the firewall rule was not touched"} {
		if !strings.Contains(s.Failure.Error(), w) {
			t.Errorf("message lacks %q: %v", w, s.Failure)
		}
	}
	if _, err := a.create("refused"); !errors.Is(err, config.ErrFirewallCheckFailedSentinel) {
		t.Errorf("create at 15 minutes = %v", err)
	}
	if n := a.count(fake.OpSetFirewallSources); n != 0 {
		t.Errorf("%d patches, want none", n)
	}
	if got := a.ruleSources(); !slices.Equal(got, []string{addr + "/32"}) {
		t.Errorf("the rule was touched: %v", got)
	}
	// "inside" is still running: nothing is stopped for a failing check.
	if got := a.status("inside"); got != config.StatusRunning {
		t.Errorf("inside is %q", got)
	}

	// The services come back and agree again.
	a.setAll(addr)
	if s := a.check(time.Minute); !s.OK {
		t.Fatalf("after the services returned: %v", s.Failure)
	}
	if _, err := a.create("again"); err != nil {
		t.Errorf("create after recovery: %v", err)
	}
	a.settled("again")
}

// TestFirewallAutoNeverPatchesABrokenRule: the echo services agree and the
// sources are wrong, but something else about the rule or the network is
// wrong too. The rule is reported and never patched.
func TestFirewallAutoNeverPatchesABrokenRule(t *testing.T) {
	const addr = "203.0.113.7"
	tag := provider.NetworkTag(testEnv)
	edit := func(fn func(*provider.FirewallRule)) func(*fake.Provider) {
		return func(p *fake.Provider) { p.EditEnvRule(fn) }
	}
	tests := []struct {
		name    string
		arrange func(p *fake.Provider)
		want    FirewallReason
	}{
		{"rule missing", func(p *fake.Provider) { p.RemoveFirewallRule(provider.FirewallRuleName(testEnv)) }, FirewallRuleMissing},
		{"rule on another network", edit(func(r *provider.FirewallRule) { r.Network = "elsewhere" }), FirewallRuleNetwork},
		{"rule is egress", edit(func(r *provider.FirewallRule) { r.Direction = provider.DirectionEgress }), FirewallRuleDirection},
		{"rule disabled", edit(func(r *provider.FirewallRule) { r.Disabled = true }), FirewallRuleDisabled},
		{"rule allows more", edit(func(r *provider.FirewallRule) { r.Allowed[0].Ports = []string{"22", "8080"} }), FirewallRuleAllow},
		{"rule has no target", edit(func(r *provider.FirewallRule) { r.TargetTags = nil }), FirewallRuleTarget},
		{"rule targets a second tag", edit(func(r *provider.FirewallRule) { r.TargetTags = []string{tag, "web"} }), FirewallRuleTarget},
		{"rule admits a source tag", edit(func(r *provider.FirewallRule) { r.SourceTags = []string{"bastion"} }), FirewallRuleSourceKind},
		{"stray rule", func(p *fake.Provider) {
			p.AddFirewallRule(provider.FirewallRule{Name: "allow-internal", SourceRanges: []string{"10.128.0.0/9"}, Allowed: []provider.FirewallPorts{{Protocol: "all"}}})
		}, FirewallStrayRule},
		{"default network", func(p *fake.Provider) { p.SetDefaultNetwork(true) }, FirewallDefaultNetwork},
		{"the network's rules cannot be read", func(p *fake.Provider) { p.FailNext(fake.OpNetworkIngress, errors.New("timeout")) }, FirewallUnreadable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := newAutoEnv(t, addr+"/32", []string{addr, addr})
			if s := a.b.FirewallStatus(); !s.OK || a.count(fake.OpSetFirewallSources) != 0 {
				t.Fatalf("start-up: %+v (%v)", s, s.Failure)
			}
			// Wrong sources (which alone would be patched) and one more
			// fault.
			a.prov.EditEnvRule(func(r *provider.FirewallRule) { r.SourceRanges = []string{"198.51.100.9/32"} })
			tt.arrange(a.prov)
			s := a.check(FirewallPatchInterval + time.Second)
			if s.OK || s.Failure == nil || s.Failure.Reason != tt.want {
				t.Fatalf("status = %+v (%v), want reason %s", s, s.Failure, tt.want)
			}
			if n := a.count(fake.OpSetFirewallSources); n != 0 {
				t.Errorf("%d patches of a rule that fails the check for another reason", n)
			}
		})
	}
}

// readOnly hides the fake's FirewallSourceSetter, as a provider that cannot
// change rules would be.
type readOnly struct{ provider.Provider }

func TestFirewallAutoProviderCannotPatch(t *testing.T) {
	const addr = "203.0.113.7"
	a := newAutoEnv(t, "192.0.2.1/32", []string{addr, addr}, func(e *env, _ *fake.Options) {
		e.wrap = func(p provider.Provider) provider.Provider { return readOnly{p} }
	})
	s := a.b.FirewallStatus()
	if s.OK || s.Failure.Reason != FirewallRuleSources || !strings.Contains(s.Failure.Error(), "this provider cannot change the rule") {
		t.Fatalf("status = %+v (%v)", s, s.Failure)
	}
	if n := a.count(fake.OpSetFirewallSources); n != 0 {
		t.Errorf("%d patches", n)
	}
}

// TestPublicAddrSummaryHidesCredentials: the summary is logged and sent to
// clients in the refusal, so an echo URL's userinfo never appears in it,
// whether the entry parses (the host alone is named) or not.
func TestPublicAddrSummaryHidesCredentials(t *testing.T) {
	const secret = "s3cr3t-token"
	tests := []struct {
		name string
		url  string
		want string
	}{
		{"parses", "https://user:" + secret + "@echo.invalid/ip", "echo.invalid: "},
		{"plain http", "http://" + secret + "@echo.invalid/ip", "echo.invalid: not an https URL"},
		{"does not parse", "https://user:" + secret + "@echo.invalid/%zz", "https://REDACTED@echo.invalid/%zz: not an https URL"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// A transport that fails every request: nothing leaves the test.
			client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return nil, errors.New("no network in this test")
			})}
			_, ok, summary := newAddrFinder([]string{tt.url, tt.url}, client).agreed(context.Background())
			if ok {
				t.Fatal("believed an address nobody reported")
			}
			if strings.Contains(summary, secret) {
				t.Errorf("summary carries the credential: %s", summary)
			}
			if !strings.Contains(summary, tt.want) {
				t.Errorf("summary lacks %q: %s", tt.want, summary)
			}
		})
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
