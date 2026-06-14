// Package egress implements the policy engine and host-side filtering proxy
// for shed's Level-1 (audit-first, cooperative) egress control. It is a leaf
// package: the shed-egress-proxy binary and internal/config both depend on it,
// so it must not import config (no import cycle).
//
// SECURITY POSTURE: Level 1 is cooperative/audit, NOT a security boundary.
// HTTP_PROXY is honored only by cooperating clients; the deny-CIDR guards here
// protect only traffic that actually reaches the proxy. A raw connect() that
// ignores HTTP_PROXY bypasses everything, including the IMDS guard.
package egress

import (
	"fmt"
	"net"
	"strings"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
	"golang.org/x/net/idna"
)

// Verdict is a policy decision for one connection.
type Verdict int

const (
	VerdictDeny  Verdict = iota // dropped + audited
	VerdictAllow                // spliced through
)

func (v Verdict) String() string {
	if v == VerdictAllow {
		return "allow"
	}
	return "deny"
}

// Mode is the policy fall-through behavior.
type Mode int

const (
	// ModeEnforce: a connection matching no allow rule (and no guard) is denied.
	ModeEnforce Mode = iota
	// ModeAudit: a connection matching no allow rule is allowed and logged.
	// Guards STILL deny in audit mode (you never want an audited shed to reach
	// IMDS) — they are simply recorded.
	ModeAudit
)

// ConnContext is the attribute set a rule evaluates against. ResolvedIP is set
// only at dial time (after the proxy's own DNS resolution) for CEL rules that
// reference it; the guard check against resolved IPs is done by MatchGuard.
type ConnContext struct {
	Host       string // canonicalized SNI / HTTP Host
	Port       int
	ResolvedIP string
	Protocol   string // "https" | "http"
	Shed       string
}

// ProfileSpec is the policy fragment a named profile contributes. config
// converts its YAML into these; the engine composes a list of them per shed.
type ProfileSpec struct {
	Mode  string   // "audit" | "" (enforce)
	Allow []string // domain globs, e.g. "*.github.com" (suffix) or "github.com" (exact)
	Deny  []string // domain globs (optional; checked before allows)
	Rule  string   // CEL expression (allow condition); the power path
}

// rule is one compiled matcher with a verdict.
type rule struct {
	verdict Verdict
	reason  string
	match   func(ConnContext) (bool, error)
}

// Policy is a compiled, per-shed effective policy: always-on guards (deny-CIDRs)
// + composed first-match rules + a fall-through mode.
type Policy struct {
	guards []*net.IPNet
	rules  []rule
	mode   Mode
}

// Mode reports the fall-through mode (audit logs everything, enforce denies).
func (p *Policy) IsAudit() bool { return p.mode == ModeAudit }

// MatchGuard reports whether ip falls in an always-on deny-CIDR. The proxy must
// call this for EVERY address a hostname resolves to (post-resolution), so a
// name that resolves to 169.254.169.254 is denied even if its host string was
// allowlisted.
func (p *Policy) MatchGuard(ip net.IP) (cidr string, denied bool) {
	for _, g := range p.guards {
		if g.Contains(ip) {
			return g.String(), true
		}
	}
	return "", false
}

// Evaluate applies the composed rules (first-match-wins) then the fall-through.
// It does NOT re-check guards (the proxy does that per resolved IP via
// MatchGuard); but if ctx.ResolvedIP is set it is also guard-checked here so a
// direct Evaluate call is safe. A rule whose CEL errors fails closed (deny).
func (p *Policy) Evaluate(ctx ConnContext) (Verdict, string) {
	if ctx.ResolvedIP != "" {
		if ip := net.ParseIP(ctx.ResolvedIP); ip != nil {
			if cidr, denied := p.MatchGuard(ip); denied {
				return VerdictDeny, "guard:" + cidr
			}
		}
	}
	for _, r := range p.rules {
		ok, err := r.match(ctx)
		if err != nil {
			return VerdictDeny, "cel-error:" + err.Error() // fail closed
		}
		if ok {
			return r.verdict, r.reason
		}
	}
	if p.mode == ModeAudit {
		return VerdictAllow, "audit-fallthrough"
	}
	return VerdictDeny, "default-deny"
}

// DefaultGuards returns the always-on deny-CIDRs (highest precedence). gatewayIP
// (the proxy host's VM-facing gateway) is added when non-empty. These protect
// only proxy-routed traffic.
func DefaultGuards(gatewayIP string) ([]*net.IPNet, error) {
	cidrs := []string{
		"169.254.0.0/16",                                // IMDS + link-local v4
		"169.254.170.2/32",                              // ECS task metadata
		"127.0.0.0/8",                                   // loopback
		"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", // RFC1918
		"100.64.0.0/10", // CGNAT
		"::1/128",       // loopback v6
		"fe80::/10",     // link-local v6
		"fc00::/7",      // ULA v6
		"ff00::/8",      // multicast v6
		"224.0.0.0/4",   // multicast v4
		"0.0.0.0/8",     // "this network" / unspecified v4
	}
	out := make([]*net.IPNet, 0, len(cidrs)+1)
	for _, c := range cidrs {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			return nil, fmt.Errorf("internal: bad guard cidr %q: %w", c, err)
		}
		out = append(out, n)
	}
	if gatewayIP != "" {
		ip := net.ParseIP(gatewayIP)
		if ip == nil {
			return nil, fmt.Errorf("invalid gateway ip %q", gatewayIP)
		}
		bits := 32
		if ip.To4() == nil {
			bits = 128
		}
		out = append(out, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
	}
	return out, nil
}

// CanonicalizeHost normalizes a hostname for safe suffix matching: IDNA/punycode
// to ASCII, lowercase, strip a trailing dot, reject empty labels. This closes
// the suffix-trap and homograph classes (e.g. "evilgithub.com" must not match a
// "*.github.com" rule, and unicode look-alikes are folded to punycode).
func CanonicalizeHost(host string) (string, error) {
	h := strings.TrimSuffix(strings.TrimSpace(host), ".")
	if h == "" {
		return "", fmt.Errorf("empty host")
	}
	// Reject an IP literal as a "host" — these have no name to allowlist.
	if net.ParseIP(h) != nil {
		return "", fmt.Errorf("host is an IP literal")
	}
	ascii, err := idna.Lookup.ToASCII(h)
	if err != nil {
		return "", fmt.Errorf("idna: %w", err)
	}
	ascii = strings.ToLower(ascii)
	for _, label := range strings.Split(ascii, ".") {
		if label == "" {
			return "", fmt.Errorf("empty label in %q", ascii)
		}
	}
	return ascii, nil
}

// globMatch reports whether a canonicalized host matches a domain glob.
// "*.example.com" matches any subdomain of example.com (not example.com itself);
// "example.com" matches exactly. The glob's literal part is canonicalized too.
func globMatch(host, glob string) bool {
	glob = strings.ToLower(strings.TrimSpace(glob))
	if strings.HasPrefix(glob, "*.") {
		suffix := glob[1:] // ".example.com"
		return strings.HasSuffix(host, suffix)
	}
	return host == glob
}

// CompilePolicy composes an ordered list of profile specs into a Policy. Order
// is: Deny globs, then Allow globs, then the CEL rule, per profile in order;
// guards are always highest-precedence (checked first in Evaluate). The mode is
// audit if any profile sets mode audit, else enforce.
func CompilePolicy(specs []ProfileSpec, gatewayIP string) (*Policy, error) {
	guards, err := DefaultGuards(gatewayIP)
	if err != nil {
		return nil, err
	}
	p := &Policy{guards: guards, mode: ModeEnforce}
	env, err := newCELEnv()
	if err != nil {
		return nil, err
	}
	for _, spec := range specs {
		if strings.EqualFold(spec.Mode, "audit") {
			p.mode = ModeAudit
		}
		for _, g := range spec.Deny {
			g := g
			p.rules = append(p.rules, rule{
				verdict: VerdictDeny, reason: "deny:" + g,
				match: func(c ConnContext) (bool, error) { return globMatch(c.Host, g), nil },
			})
		}
		for _, g := range spec.Allow {
			g := g
			p.rules = append(p.rules, rule{
				verdict: VerdictAllow, reason: "allow:" + g,
				match: func(c ConnContext) (bool, error) { return globMatch(c.Host, g), nil },
			})
		}
		if strings.TrimSpace(spec.Rule) != "" {
			prog, err := compileCEL(env, spec.Rule)
			if err != nil {
				return nil, fmt.Errorf("egress rule %q: %w", spec.Rule, err)
			}
			p.rules = append(p.rules, rule{
				verdict: VerdictAllow, reason: "rule",
				match: celMatch(prog),
			})
		}
	}
	return p, nil
}

// ValidateProfile compiles a profile's allow/deny globs and CEL rule so config
// load fails fast on a typo (mirrors the auth-config fail-on-load behavior).
func ValidateProfile(spec ProfileSpec) error {
	_, err := CompilePolicy([]ProfileSpec{spec}, "")
	return err
}

func newCELEnv() (*cel.Env, error) {
	return cel.NewEnv(
		cel.Variable("host", cel.StringType),
		cel.Variable("port", cel.IntType),
		cel.Variable("resolved_ip", cel.StringType),
		cel.Variable("protocol", cel.StringType),
		cel.Variable("shed", cel.StringType),
	)
}

func compileCEL(env *cel.Env, expr string) (cel.Program, error) {
	ast, iss := env.Compile(expr)
	if iss != nil && iss.Err() != nil {
		return nil, iss.Err()
	}
	if ast.OutputType() != cel.BoolType {
		return nil, fmt.Errorf("rule must evaluate to bool, got %s", ast.OutputType())
	}
	return env.Program(ast)
}

func celMatch(prog cel.Program) func(ConnContext) (bool, error) {
	return func(c ConnContext) (bool, error) {
		out, _, err := prog.Eval(map[string]any{
			"host":        c.Host,
			"port":        c.Port,
			"resolved_ip": c.ResolvedIP,
			"protocol":    c.Protocol,
			"shed":        c.Shed,
		})
		if err != nil {
			return false, err
		}
		return boolVal(out), nil
	}
}

func boolVal(v ref.Val) bool {
	b, ok := v.Value().(bool)
	return ok && b && v.Type() == types.BoolType
}
