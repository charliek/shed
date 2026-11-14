package remote

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/remote/provider"
)

// The firewall check.
//
// Every shed's SSH port is open to whatever the environment's one cloud
// firewall rule admits, and the "isolated" network profile holds only while
// no other rule lets instances reach each other. The setup script makes the
// rule; people change clouds by hand. So the server reads the rule and the
// network's other ingress rules and refuses to create or start a VM while
// they are not what it expects.
//
// The check runs at start-up, in every periodic pass, and before a create or
// start whose last result is older than FirewallMaxAge. A failing check
// refuses creates and starts with 503 FIREWALL_CHECK_FAILED and nothing
// else: stop, delete, list and exec keep working, the server still starts,
// and no VM is ever stopped or deleted because of it.
const (
	// FirewallMaxAge is the oldest check result a create or start may rely
	// on.
	FirewallMaxAge = 60 * time.Second
	// FirewallPatchInterval is the least time between two patches of the
	// rule in auto mode.
	FirewallPatchInterval = 5 * time.Minute
	// PublicAddrMaxAge is how long auto mode keeps believing an address
	// after the echo services last agreed on it.
	PublicAddrMaxAge = 15 * time.Minute
	// firewallCheckTimeout bounds one check.
	firewallCheckTimeout = 30 * time.Second
	// firewallGateTimeout bounds a check a create or start waits for, so
	// the reply still fits in the time a client waits.
	firewallGateTimeout = 10 * time.Second
	// firewallRelog is how often a failure that has not changed is logged
	// again.
	firewallRelog = 10 * time.Minute
)

// FirewallReason says which clause of the firewall check failed.
type FirewallReason string

const (
	// FirewallUnreadable: the cloud's firewall could not be read.
	FirewallUnreadable FirewallReason = "unreadable"
	// FirewallRuleMissing: the environment's rule does not exist.
	FirewallRuleMissing FirewallReason = "rule_missing"
	// FirewallRuleNetwork: the rule is on another network.
	FirewallRuleNetwork FirewallReason = "rule_network"
	// FirewallRuleDirection: the rule is not an ingress rule.
	FirewallRuleDirection FirewallReason = "rule_direction"
	// FirewallRuleDisabled: the rule is disabled.
	FirewallRuleDisabled FirewallReason = "rule_disabled"
	// FirewallRuleAllow: the rule is not "allow tcp:22" and nothing else.
	FirewallRuleAllow FirewallReason = "rule_allow"
	// FirewallRuleTarget: the rule does not target exactly the
	// environment's network tag.
	FirewallRuleTarget FirewallReason = "rule_target"
	// FirewallRuleSourceKind: the rule admits by source tag or service
	// account, not only by address.
	FirewallRuleSourceKind FirewallReason = "rule_source_kind"
	// FirewallRuleSources: the rule's source ranges are not the expected
	// set.
	FirewallRuleSources FirewallReason = "rule_sources"
	// FirewallStrayRule: another enabled ingress allow rule on the network
	// reaches the sheds.
	FirewallStrayRule FirewallReason = "stray_rule"
	// FirewallDefaultNetwork: the sheds are on the project's default
	// network.
	FirewallDefaultNetwork FirewallReason = "default_network"
	// FirewallAddressUnknown: auto mode does not know the server's public
	// address.
	FirewallAddressUnknown FirewallReason = "address_unknown"
)

// FirewallFailure is why the firewall check failed. Its message is written
// for the operator who must fix the cloud: which rule, what was expected,
// what was found.
type FirewallFailure struct {
	Reason FirewallReason
	// Rule is the rule the failure is about, or "".
	Rule string
	// What says what is wrong, in plain words.
	What string
	// Expected and Found are set when the failure is a comparison.
	Expected string
	Found    string
	// Detail is anything more: why the rule was not repaired, the cloud's
	// own error.
	Detail string
}

func (f *FirewallFailure) Error() string {
	msg := f.What
	if f.Expected != "" || f.Found != "" {
		msg += fmt.Sprintf(": expected %s, found %s", f.Expected, f.Found)
	}
	if f.Detail != "" {
		msg += " (" + f.Detail + ")"
	}
	return msg
}

// FirewallStatus is the result of the last firewall check.
type FirewallStatus struct {
	// Mode is remote.firewall.mode.
	Mode string
	// CheckedAt is when the check finished; zero when none has run.
	CheckedAt time.Time
	// OK is true when the check passed.
	OK bool
	// Failure is why it failed; nil when OK or when none has run.
	Failure *FirewallFailure
	// Expected is the source set the rule was compared with: the
	// configured ranges in static mode, the believed public address in
	// auto mode, nothing in any mode.
	Expected []string
}

// firewallState is the backend's firewall check: its settings, the last
// result and, for auto mode, what it believes its public address to be.
type firewallState struct {
	mode   string
	static []netip.Prefix // static mode's expected sources
	finder *addrFinder    // auto mode only

	// run serialises checks: one reads the cloud (and may patch) at a time.
	run sync.Mutex
	// The fields below are written with run held.
	believed  netip.Addr // the address the echo services last agreed on
	agreedAt  time.Time  // when they last agreed
	lastPatch time.Time  // when the rule was last patched, or tried
	loggedAt  time.Time  // when the result was last logged
	loggedMsg string     // what was logged then ("" for a pass)

	mu     sync.Mutex
	status FirewallStatus
}

func newFirewallState(cfg *config.RemoteConfig, sources []string, echo *http.Client) (*firewallState, error) {
	fw := &firewallState{mode: cfg.Firewall.Mode}
	if fw.mode == "" {
		fw.mode = config.RemoteFirewallStatic
	}
	fw.status.Mode = fw.mode
	switch fw.mode {
	case config.RemoteFirewallStatic:
		// The list is checked again here, with nothing acknowledged or
		// refused for width: that was the config's decision. What matters
		// is that there is a set to compare the rule with.
		canonical, err := config.ParseFirewallSources("the firewall source ranges", sources, true)
		if err != nil {
			return nil, fmt.Errorf("remote: firewall.mode %s: %w", fw.mode, err)
		}
		for _, s := range canonical {
			fw.static = append(fw.static, netip.MustParsePrefix(s))
		}
	case config.RemoteFirewallAuto:
		if len(cfg.Firewall.EchoURLs) < echoQuorum {
			return nil, fmt.Errorf("remote: firewall.mode %s needs at least %d echo URLs", fw.mode, echoQuorum)
		}
		fw.finder = newAddrFinder(cfg.Firewall.EchoURLs, echo)
	case config.RemoteFirewallAny:
	default:
		return nil, fmt.Errorf("remote: unknown firewall.mode %q", fw.mode)
	}
	return fw, nil
}

// FirewallStatus returns the result of the last firewall check.
func (b *Backend) FirewallStatus() FirewallStatus {
	b.fw.mu.Lock()
	defer b.fw.mu.Unlock()
	s := b.fw.status
	s.Expected = append([]string(nil), s.Expected...)
	return s
}

// CheckFirewall runs the firewall check now, records and logs its result and
// returns it. In auto mode it may patch the rule's source ranges. Checks run
// one at a time.
func (b *Backend) CheckFirewall(ctx context.Context) FirewallStatus {
	b.fw.run.Lock()
	defer b.fw.run.Unlock()
	return b.checkFirewallLocked(ctx)
}

func (b *Backend) checkFirewallLocked(ctx context.Context) FirewallStatus {
	ctx, cancel := context.WithTimeout(ctx, firewallCheckTimeout)
	defer cancel()
	failure, expected := b.runFirewallCheck(ctx)
	if b.rootCtx.Err() != nil {
		// The server is shutting down: a check cut short says nothing.
		return b.FirewallStatus()
	}

	status := FirewallStatus{Mode: b.fw.mode, CheckedAt: b.now(), OK: failure == nil, Failure: failure}
	for _, p := range expected {
		status.Expected = append(status.Expected, p.String())
	}
	b.fw.mu.Lock()
	first := b.fw.status.CheckedAt.IsZero()
	b.fw.status = status
	b.fw.mu.Unlock()

	msg := ""
	if failure != nil {
		msg = failure.Error()
	}
	switch {
	case failure == nil && (first || b.fw.loggedMsg != ""):
		b.logf("remote: firewall check passed (mode %s, rule %s, sources %s)", b.fw.mode, provider.FirewallRuleName(b.cfg.Environment), describeExpected(b.fw.mode, status.Expected))
		b.fw.loggedMsg, b.fw.loggedAt = "", status.CheckedAt
	case failure != nil && (first || msg != b.fw.loggedMsg || status.CheckedAt.Sub(b.fw.loggedAt) >= firewallRelog):
		b.logf("remote: firewall check FAILED (mode %s, reason %s): %s. Creates and starts are refused until it passes; stop, delete, list and exec are not affected",
			b.fw.mode, failure.Reason, msg)
		b.fw.loggedMsg, b.fw.loggedAt = msg, status.CheckedAt
	}
	return status
}

func describeExpected(mode string, expected []string) string {
	if mode == config.RemoteFirewallAny {
		return "not compared"
	}
	return strings.Join(expected, ", ")
}

// firewallGate is called before a create or start. It returns nil when the
// firewall check passed no longer ago than FirewallMaxAge, running the check
// first when the last result is older, and otherwise an error that is
// config.ErrFirewallCheckFailedSentinel (503 FIREWALL_CHECK_FAILED) and
// carries the *FirewallFailure. Never call it with b.mu held: it may read
// the cloud.
func (b *Backend) firewallGate(ctx context.Context) error {
	status := b.FirewallStatus()
	if !b.firewallFresh(status) {
		b.fw.run.Lock()
		// Another request may have checked while this one waited.
		if status = b.FirewallStatus(); !b.firewallFresh(status) {
			// On the server's context: a client that hangs up must not
			// leave a half-made check behind as the next caller's result.
			cctx, cancel := context.WithTimeout(b.rootCtx, firewallGateTimeout)
			status = b.checkFirewallLocked(cctx)
			cancel()
		}
		b.fw.run.Unlock()
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if status.OK {
		return nil
	}
	if status.Failure == nil {
		return fmt.Errorf("%w: the check has not run yet", config.ErrFirewallCheckFailedSentinel)
	}
	return fmt.Errorf("%w: %w; creates and starts are refused until the check passes", config.ErrFirewallCheckFailedSentinel, status.Failure)
}

func (b *Backend) firewallFresh(s FirewallStatus) bool {
	if s.CheckedAt.IsZero() {
		return false
	}
	age := b.now().Sub(s.CheckedAt)
	return age >= 0 && age < FirewallMaxAge
}

// runFirewallCheck is one check. It returns nil when everything is as it
// should be, and the source set the rule was compared with.
func (b *Backend) runFirewallCheck(ctx context.Context) (*FirewallFailure, []netip.Prefix) {
	fw := b.fw
	env := b.cfg.Environment
	ruleName := provider.FirewallRuleName(env)

	var expected []netip.Prefix
	agreedNow := false
	switch fw.mode {
	case config.RemoteFirewallStatic:
		expected = fw.static
	case config.RemoteFirewallAuto:
		addr, ok, summary := fw.finder.agreed(ctx)
		now := b.now()
		switch {
		case ok:
			if fw.believed.IsValid() && fw.believed != addr {
				b.logf("remote: firewall: the server's public address changed from %s to %s", fw.believed, addr)
			}
			fw.believed, fw.agreedAt, agreedNow = addr, now, true
		case !fw.believed.IsValid():
			return &FirewallFailure{
				Reason: FirewallAddressUnknown,
				What:   fmt.Sprintf("the server's public address is not known: fewer than %d of the %d echo services agree on one", echoQuorum, len(fw.finder.urls)),
				Detail: summary + "; the firewall rule was not touched",
			}, nil
		case now.Sub(fw.agreedAt) >= PublicAddrMaxAge:
			return &FirewallFailure{
				Reason: FirewallAddressUnknown,
				What: fmt.Sprintf("the server's public address is not known: the echo services have not agreed on one for %s (they last agreed on %s)",
					now.Sub(fw.agreedAt).Round(time.Second), fw.believed),
				Detail: summary + "; the firewall rule was not touched",
			}, nil
		}
		expected = []netip.Prefix{netip.PrefixFrom(fw.believed, 32)}
	}

	rule, err := b.prov.EnvFirewallRule(ctx)
	if errors.Is(err, provider.ErrNotFound) {
		return &FirewallFailure{
			Reason: FirewallRuleMissing, Rule: ruleName,
			What:     "firewall rule " + ruleName + " does not exist",
			Expected: "an ingress rule allowing tcp:22 to network tag " + provider.NetworkTag(env),
			Found:    "no rule of that name",
			Detail:   "scripts/remote/gce-setup.sh prints the command that creates it",
		}, expected
	}
	if err != nil {
		return unreadable("firewall rule "+ruleName, err), expected
	}
	view, err := b.prov.NetworkIngress(ctx)
	if err != nil {
		return unreadable("the ingress rules of the shed network", err), expected
	}
	if f := checkRuleShape(env, view.Network, rule); f != nil {
		return f, expected
	}
	if f := checkNetwork(env, view); f != nil {
		return f, expected
	}
	if fw.mode == config.RemoteFirewallAny {
		return nil, nil
	}
	mismatch := checkRuleSources(rule, expected)
	if mismatch == nil || fw.mode != config.RemoteFirewallAuto {
		return mismatch, expected
	}

	// Auto mode, and the rule is right in every way but its sources.
	setter, canSet := b.prov.(provider.FirewallSourceSetter)
	now := b.now()
	switch {
	case !agreedNow:
		mismatch.Detail = "not repaired: the echo services did not agree in this check"
		return mismatch, expected
	case !publicIPv4(fw.believed):
		mismatch.Detail = "not repaired: " + fw.believed.String() + " is not a public address"
		return mismatch, expected
	case !canSet:
		mismatch.Detail = "not repaired: this provider cannot change the rule"
		return mismatch, expected
	case !fw.lastPatch.IsZero() && now.Sub(fw.lastPatch) < FirewallPatchInterval:
		mismatch.Detail = fmt.Sprintf("not repaired yet: the rule was last patched %s ago and is patched at most once every %s",
			now.Sub(fw.lastPatch).Round(time.Second), FirewallPatchInterval)
		return mismatch, expected
	}
	want := expected[0].String()
	fw.lastPatch = now
	if err := setter.SetEnvFirewallSources(ctx, []string{want}); err != nil {
		b.logf("remote: firewall: patching rule %s from sources [%s] to [%s] failed: %v", ruleName, strings.Join(rule.SourceRanges, ", "), want, err)
		mismatch.Detail = "the patch to " + want + " failed: " + err.Error()
		return mismatch, expected
	}
	patched, err := b.prov.EnvFirewallRule(ctx)
	if err != nil {
		return unreadable("firewall rule "+ruleName+" after patching it", err), expected
	}
	b.logf("remote: firewall: patched rule %s: sources were [%s], are now [%s]", ruleName, strings.Join(rule.SourceRanges, ", "), strings.Join(patched.SourceRanges, ", "))
	if f := checkRuleShape(env, view.Network, patched); f != nil {
		return f, expected
	}
	return checkRuleSources(patched, expected), expected
}

func unreadable(what string, err error) *FirewallFailure {
	return &FirewallFailure{Reason: FirewallUnreadable, What: "could not read " + what + " from the cloud", Detail: err.Error()}
}

// checkRuleShape checks everything about the environment's rule except its
// source ranges: that it is on the shed network, is an enabled ingress rule,
// allows tcp:22 and nothing else, targets exactly the environment's network
// tag, and admits by address only.
func checkRuleShape(env, network string, rule provider.FirewallRule) *FirewallFailure {
	name := provider.FirewallRuleName(env)
	tag := provider.NetworkTag(env)
	fail := func(reason FirewallReason, what, expected, found string) *FirewallFailure {
		return &FirewallFailure{Reason: reason, Rule: name, What: "firewall rule " + name + " " + what, Expected: expected, Found: found}
	}
	switch {
	case rule.Network != network:
		return fail(FirewallRuleNetwork, "is on the wrong network", "network "+network, "network "+rule.Network)
	case rule.Direction != provider.DirectionIngress:
		return fail(FirewallRuleDirection, "is not an ingress rule", "direction "+provider.DirectionIngress, "direction "+rule.Direction)
	case rule.Disabled:
		return fail(FirewallRuleDisabled, "is disabled", "an enabled rule", "a disabled one")
	case !allowsOnlySSH(rule):
		return fail(FirewallRuleAllow, "must allow tcp:22 and nothing else", "allow tcp:22", describeAction(rule))
	case len(rule.TargetTags) != 1 || rule.TargetTags[0] != tag || len(rule.TargetServiceAccounts) != 0:
		return fail(FirewallRuleTarget, "must target exactly the environment's network tag", "target tag "+tag, describeTargets(rule))
	case len(rule.SourceTags) != 0 || len(rule.SourceServiceAccounts) != 0:
		return fail(FirewallRuleSourceKind, "must admit by address only", "source ranges and nothing else",
			"source tags ["+strings.Join(rule.SourceTags, ", ")+"] and source service accounts ["+strings.Join(rule.SourceServiceAccounts, ", ")+"]")
	}
	return nil
}

// checkRuleSources compares the rule's source ranges with the expected set.
func checkRuleSources(rule provider.FirewallRule, expected []netip.Prefix) *FirewallFailure {
	fail := func(found string) *FirewallFailure {
		return &FirewallFailure{
			Reason: FirewallRuleSources, Rule: rule.Name,
			What:     "firewall rule " + rule.Name + " admits the wrong sources",
			Expected: "source ranges [" + joinPrefixes(expected) + "]",
			Found:    found,
		}
	}
	got, bad := normalizeSources(rule.SourceRanges)
	if bad != "" {
		return fail("a source range that is not an IPv4 CIDR: " + bad)
	}
	want := slices.Clone(expected)
	sortPrefixes(want)
	if !slices.Equal(got, want) {
		return fail("source ranges [" + strings.Join(rule.SourceRanges, ", ") + "]")
	}
	return nil
}

// checkNetwork is the VPC check: the sheds are not on the project's default
// network, and no enabled ingress allow rule other than the environment's
// own applies to the shed tag or to every instance. Such a rule would open
// the sheds to each other or to the world. Another environment's rule on the
// same network targets that environment's tag and is not a stray.
func checkNetwork(env string, view provider.NetworkIngress) *FirewallFailure {
	if view.Default {
		return &FirewallFailure{
			Reason: FirewallDefaultNetwork,
			What:   "the sheds are on the project's default network, " + view.Network,
			Detail: "its stock rules admit traffic between instances and SSH from anywhere; use a custom-mode network made for sheds",
		}
	}
	own := provider.FirewallRuleName(env)
	tag := provider.NetworkTag(env)
	for _, r := range view.Rules {
		if r.Name == own || r.Disabled || r.Direction != provider.DirectionIngress || len(r.Allowed) == 0 {
			continue
		}
		everyInstance := len(r.TargetTags) == 0 && len(r.TargetServiceAccounts) == 0
		if !everyInstance && !slices.Contains(r.TargetTags, tag) {
			continue
		}
		reach := "instances tagged " + tag
		if everyInstance {
			reach = "every instance on the network"
		}
		return &FirewallFailure{
			Reason: FirewallStrayRule, Rule: r.Name,
			What:     "firewall rule " + r.Name + " on network " + view.Network + " also admits traffic to the sheds",
			Expected: "no enabled ingress allow rule but " + own + " reaching " + reach,
			Found:    r.Name + " (" + describeAction(r) + " from " + describeSources(r) + " to " + reach + ")",
			Detail:   "delete or disable it, or give it a target tag the sheds do not carry",
		}
	}
	return nil
}

// allowsOnlySSH reports whether the rule is exactly "allow tcp:22".
func allowsOnlySSH(rule provider.FirewallRule) bool {
	if len(rule.Denied) != 0 || len(rule.Allowed) != 1 {
		return false
	}
	a := rule.Allowed[0]
	return strings.EqualFold(a.Protocol, "tcp") && len(a.Ports) == 1 && a.Ports[0] == "22"
}

func describeAction(rule provider.FirewallRule) string {
	part := func(verb string, ports []provider.FirewallPorts) string {
		items := make([]string, 0, len(ports))
		for _, p := range ports {
			if len(p.Ports) == 0 {
				items = append(items, p.Protocol)
				continue
			}
			items = append(items, p.Protocol+":"+strings.Join(p.Ports, ","))
		}
		return verb + " " + strings.Join(items, " ")
	}
	var parts []string
	if len(rule.Allowed) > 0 {
		parts = append(parts, part("allow", rule.Allowed))
	}
	if len(rule.Denied) > 0 {
		parts = append(parts, part("deny", rule.Denied))
	}
	if len(parts) == 0 {
		return "no allow entry"
	}
	return strings.Join(parts, ", ")
}

func describeTargets(rule provider.FirewallRule) string {
	if len(rule.TargetTags) == 0 && len(rule.TargetServiceAccounts) == 0 {
		return "no target (every instance on the network)"
	}
	out := "target tags [" + strings.Join(rule.TargetTags, ", ") + "]"
	if len(rule.TargetServiceAccounts) > 0 {
		out += " and target service accounts [" + strings.Join(rule.TargetServiceAccounts, ", ") + "]"
	}
	return out
}

func describeSources(rule provider.FirewallRule) string {
	var parts []string
	if len(rule.SourceRanges) > 0 {
		parts = append(parts, "["+strings.Join(rule.SourceRanges, ", ")+"]")
	}
	if len(rule.SourceTags) > 0 {
		parts = append(parts, "tags ["+strings.Join(rule.SourceTags, ", ")+"]")
	}
	if len(rule.SourceServiceAccounts) > 0 {
		parts = append(parts, "service accounts ["+strings.Join(rule.SourceServiceAccounts, ", ")+"]")
	}
	if len(parts) == 0 {
		return "any source"
	}
	return strings.Join(parts, " and ")
}

// normalizeSources parses a rule's source ranges into sorted, canonical IPv4
// prefixes without repeats. A bare address counts as its /32. bad is the
// first entry that is not IPv4.
func normalizeSources(ranges []string) (out []netip.Prefix, bad string) {
	seen := make(map[netip.Prefix]bool)
	for _, raw := range ranges {
		s := strings.TrimSpace(raw)
		p, err := netip.ParsePrefix(s)
		if err != nil {
			addr, aerr := netip.ParseAddr(s)
			if aerr != nil {
				return nil, raw
			}
			p = netip.PrefixFrom(addr, addr.BitLen())
		}
		if !p.Addr().Is4() {
			return nil, raw
		}
		p = p.Masked()
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	sortPrefixes(out)
	return out, ""
}

func sortPrefixes(ps []netip.Prefix) {
	sort.Slice(ps, func(i, k int) bool {
		if c := ps[i].Addr().Compare(ps[k].Addr()); c != 0 {
			return c < 0
		}
		return ps[i].Bits() < ps[k].Bits()
	})
}

func joinPrefixes(ps []netip.Prefix) string {
	items := make([]string, len(ps))
	for i, p := range ps {
		items[i] = p.String()
	}
	return strings.Join(items, ", ")
}
