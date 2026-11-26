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
	"sync/atomic"
	"time"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/remote/provider"
)

// The firewall check.
//
// Every machine's SSH port is open to the sources the service applies for
// this server (its ingress), and the "isolated" network profile holds only
// while the service can say nothing else reaches the machines (ingress
// exclusive). So the server reads the ingress and refuses to create or start
// a machine while its sources are not the ones the firewall mode expects,
// and an isolated shed while the service cannot say it is exclusive. In auto
// and static mode the server writes the sources itself when they differ
// (SetIngress, at most once every FirewallPatchInterval); a static server
// whose service manages the ingress elsewhere only compares.
//
// The check runs at start-up, in every periodic pass, and before a create or
// start whose last result is older than FirewallMaxAge. A failing check
// refuses creates and starts with 503 FIREWALL_CHECK_FAILED and nothing
// else: stop, delete, list and exec keep working, the server still starts,
// and no machine is ever stopped or deleted because of it.
const (
	// FirewallMaxAge is the oldest check result a create or start may rely
	// on.
	FirewallMaxAge = 60 * time.Second
	// FirewallPatchInterval is the least time between two writes of the
	// ingress (auto and static mode).
	FirewallPatchInterval = 5 * time.Minute
	// PublicAddrMaxAge is how long auto mode keeps believing an address
	// after the echo services last agreed on it.
	PublicAddrMaxAge = 15 * time.Minute
	// firewallCheckTimeout bounds one check.
	firewallCheckTimeout = 30 * time.Second
	// firewallGateTimeout bounds what a create or start waits for at the
	// gate, so the reply still fits in the time a client waits: the wait
	// for a check already in progress and the check it then runs itself,
	// together.
	firewallGateTimeout = 10 * time.Second
	// firewallRelog is how often a failure that has not changed is logged
	// again.
	firewallRelog = 10 * time.Minute
)

// FirewallReason says which clause of the firewall check failed.
type FirewallReason string

const (
	// FirewallUnreadable: the service's ingress could not be read.
	FirewallUnreadable FirewallReason = "unreadable"
	// FirewallSources: the ingress admits other sources than the mode
	// expects.
	FirewallSources FirewallReason = "sources"
	// FirewallNotWritable: auto mode, and the service's ingress is managed
	// elsewhere (INGRESS_NOT_WRITABLE) or refused the sources.
	FirewallNotWritable FirewallReason = "not_writable"
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

// logKey names what is wrong without the parts of the message that change
// from one check to the next while nothing else does (how long ago the rule
// was patched, how long the echo services have disagreed). Two failures with
// the same key are the same failure to the log.
func (f *FirewallFailure) logKey() string {
	return strings.Join([]string{string(f.Reason), f.Rule, f.Expected, f.Found}, "\x00")
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
	// Expected is the source set the ingress was compared with: the
	// configured ranges in static mode, the believed public address in
	// auto mode, nothing in any mode.
	Expected []string
	// Exclusive is the service's word that nothing but the sources reaches
	// the machines (ingress exclusive), as the last check read it. An
	// isolated shed is created and started only while it is true.
	Exclusive bool
}

// firewallState is the backend's firewall check: its settings, the last
// result and, for auto mode, what it believes its public address to be.
type firewallState struct {
	mode   string
	static []netip.Prefix // static mode's expected sources
	finder *addrFinder    // auto mode only

	// run serialises checks: one reads the cloud (and may patch) at a time.
	// It is a one-slot channel, not a mutex, so a caller can stop waiting
	// for its turn (acquire) while the check in progress carries on.
	run chan struct{}
	// gateTimeout is firewallGateTimeout; tests shorten it.
	gateTimeout time.Duration
	// readingIngress is true while a check waits on the service's answer
	// to its ingress read: a caller that gives up waiting for that check
	// was kept waiting by the service, not by a firewall found wrong.
	readingIngress atomic.Bool
	// The fields below are written with run held.
	believed  netip.Addr // the address the echo services last agreed on
	agreedAt  time.Time  // when they last agreed
	lastPatch time.Time  // when the ingress was last written, or tried
	exclusive bool       // the last ingress read's exclusive
	loggedAt  time.Time  // when the result was last logged
	loggedKey string     // the logKey of the failure logged then ("" for a pass)

	mu     sync.Mutex
	status FirewallStatus
}

func newFirewallState(cfg *config.RemoteConfig, sources []string, echo *http.Client) (*firewallState, error) {
	fw := &firewallState{mode: cfg.Firewall.Mode, run: make(chan struct{}, 1), gateTimeout: firewallGateTimeout}
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
	if err := b.fw.acquire(ctx, nil); err != nil {
		// ctx ended while another check ran: the last result stands.
		return b.FirewallStatus()
	}
	defer b.fw.release()
	return b.checkFirewallLocked(ctx)
}

// errCheckBusy is acquire's error when the turn did not come in time.
var errCheckBusy = errors.New("a firewall check is in progress")

// acquire waits for the caller's turn to run a check. It gives up when ctx
// ends (ctx's error) or when timeout fires (errCheckBusy); a nil timeout
// never fires. The check in progress is not disturbed either way.
func (fw *firewallState) acquire(ctx context.Context, timeout <-chan time.Time) error {
	select {
	case fw.run <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-timeout:
		return errCheckBusy
	}
}

func (fw *firewallState) release() { <-fw.run }

func (b *Backend) checkFirewallLocked(ctx context.Context) FirewallStatus {
	ctx, cancel := context.WithTimeout(ctx, firewallCheckTimeout)
	defer cancel()
	failure, expected := b.runFirewallCheck(ctx)
	if b.rootCtx.Err() != nil {
		// The server is shutting down: a check cut short says nothing.
		return b.FirewallStatus()
	}

	status := FirewallStatus{Mode: b.fw.mode, CheckedAt: b.now(), OK: failure == nil, Failure: failure, Exclusive: b.fw.exclusive}
	for _, p := range expected {
		status.Expected = append(status.Expected, p.String())
	}
	b.fw.mu.Lock()
	first := b.fw.status.CheckedAt.IsZero()
	b.fw.status = status
	b.fw.mu.Unlock()

	msg, key := "", ""
	if failure != nil {
		msg, key = failure.Error(), failure.logKey()
	}
	switch {
	case failure == nil && (first || b.fw.loggedKey != ""):
		b.logf("remote: firewall check passed (mode %s, sources %s, exclusive %v)", b.fw.mode, describeExpected(b.fw.mode, status.Expected), status.Exclusive)
		b.fw.loggedKey, b.fw.loggedAt = "", status.CheckedAt
	case failure != nil && (first || key != b.fw.loggedKey || status.CheckedAt.Sub(b.fw.loggedAt) >= firewallRelog):
		b.logf("remote: firewall check FAILED (mode %s, reason %s): %s. Creates and starts are refused until it passes; stop, delete, list and exec are not affected",
			b.fw.mode, failure.Reason, msg)
		b.fw.loggedKey, b.fw.loggedAt = key, status.CheckedAt
	}
	return status
}

func describeExpected(mode string, expected []string) string {
	if mode == config.RemoteFirewallAny {
		return "not compared"
	}
	return strings.Join(expected, ", ")
}

// firewallGate is called before a create or start of a shed with the given
// network profile. It returns nil when the firewall check passed no longer
// ago than FirewallMaxAge (and, for an isolated shed, found the ingress
// exclusive), running the check first when the last result is older, and
// otherwise an error that is config.ErrFirewallCheckFailedSentinel (503
// FIREWALL_CHECK_FAILED) and carries the *FirewallFailure, except when the
// check failed because the service's ingress could not be read
// (FirewallUnreadable), or the wait for a check in progress ran out while
// that check still waited on the service's ingress read: that is 503
// COMPUTE_UNAVAILABLE. Never call it
// with b.mu held: it may read the service.
//
// The caller waits firewallGateTimeout at most, counted from here: a check
// already in progress (the periodic one may take firewallCheckTimeout) uses
// up the same budget as the check this call then runs. A caller whose turn
// does not come in time is refused like any other; a caller whose ctx ends
// leaves at once. Neither stops the check in progress.
func (b *Backend) firewallGate(ctx context.Context, network string) error {
	status := b.FirewallStatus()
	if !b.firewallFresh(status) {
		deadline := time.Now().Add(b.fw.gateTimeout)
		budget := time.NewTimer(b.fw.gateTimeout)
		err := b.fw.acquire(ctx, budget.C)
		budget.Stop()
		switch {
		case errors.Is(err, errCheckBusy) && b.fw.readingIngress.Load():
			// The check in progress is waiting on the service's answer to
			// its ingress read: the service is slow or hanging, which is
			// the service being unavailable, as an unreadable ingress is.
			return computeUnavailable(fmt.Errorf("a firewall check in progress is still waiting for the account service to answer its ingress read after %s; creates and starts are refused until it answers",
				b.fw.gateTimeout))
		case errors.Is(err, errCheckBusy):
			return fmt.Errorf("%w: a firewall check in progress did not finish within %s; creates and starts are refused until the check passes",
				config.ErrFirewallCheckFailedSentinel, b.fw.gateTimeout)
		case err != nil:
			return err
		}
		// Another request may have checked while this one waited.
		if status = b.FirewallStatus(); !b.firewallFresh(status) {
			// On the server's context: a client that hangs up must not
			// leave a half-made check behind as the next caller's result.
			cctx, cancel := context.WithDeadline(b.rootCtx, deadline)
			status = b.checkFirewallLocked(cctx)
			cancel()
		}
		b.fw.release()
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if status.OK && network == config.NetworkProfileIsolated && !status.Exclusive {
		return fmt.Errorf("%w: the account service does not say the machines' ingress is exclusive (nothing but the sources reaches them, and no machine reaches another), which the isolated network profile needs; isolated sheds are not created or started until it does",
			config.ErrFirewallCheckFailedSentinel)
	}
	if status.OK {
		return nil
	}
	if status.Failure == nil {
		return fmt.Errorf("%w: the check has not run yet", config.ErrFirewallCheckFailedSentinel)
	}
	if status.Failure.Reason == FirewallUnreadable {
		// The service did not answer the ingress read (unreachable,
		// hanging, failing): that is the service being unavailable, not a
		// firewall found wrong. 503 COMPUTE_UNAVAILABLE, so a client waits
		// for the service rather than looks at the firewall.
		return computeUnavailable(fmt.Errorf("the account service could not be asked for the machines' ingress, which the firewall check reads before a create or start: %w; creates and starts are refused until it answers", status.Failure))
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
// should be, and the source set the ingress was compared with.
func (b *Backend) runFirewallCheck(ctx context.Context) (*FirewallFailure, []netip.Prefix) {
	fw := b.fw
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
				Detail: summary + "; the ingress was not touched",
			}, nil
		case now.Sub(fw.agreedAt) >= PublicAddrMaxAge:
			return &FirewallFailure{
				Reason: FirewallAddressUnknown,
				What: fmt.Sprintf("the server's public address is not known: the echo services have not agreed on one for %s (they last agreed on %s)",
					now.Sub(fw.agreedAt).Round(time.Second), fw.believed),
				Detail: summary + "; the ingress was not touched",
			}, nil
		}
		expected = []netip.Prefix{netip.PrefixFrom(fw.believed, 32)}
	}

	fw.readingIngress.Store(true)
	ing, err := b.prov.Ingress(ctx)
	fw.readingIngress.Store(false)
	if err != nil {
		return unreadable("the machines' ingress", err), expected
	}
	fw.exclusive = ing.Exclusive
	if fw.mode == config.RemoteFirewallAny {
		return nil, nil
	}
	mismatch := checkSources(ing.Sources, expected)
	if mismatch == nil {
		return nil, expected
	}

	// The sources are not the mode's: the server writes them (plan 041
	// R7), in auto mode its address, in static mode its configured
	// ranges. A static server whose service manages the ingress elsewhere
	// (the catalog says so, or a write answers INGRESS_NOT_WRITABLE) only
	// compares, and the mismatch refuses creates and starts.
	now := b.now()
	static := fw.mode == config.RemoteFirewallStatic
	cat, haveCat := b.Catalog()
	switch {
	case static && haveCat && !cat.IngressWritable:
		mismatch.Detail = "compared only: the account service's ingress is managed elsewhere; apply these sources there"
		return mismatch, expected
	case !static && !agreedNow:
		mismatch.Detail = "not repaired: the echo services did not agree in this check"
		return mismatch, expected
	case !static && !publicIPv4(fw.believed):
		mismatch.Detail = "not repaired: " + fw.believed.String() + " is not a public address"
		return mismatch, expected
	case !fw.lastPatch.IsZero() && now.Sub(fw.lastPatch) < FirewallPatchInterval:
		mismatch.Detail = fmt.Sprintf("not repaired yet: the ingress was last written %s ago and is written at most once every %s",
			now.Sub(fw.lastPatch).Round(time.Second), FirewallPatchInterval)
		return mismatch, expected
	}
	fw.lastPatch = now
	var written provider.Ingress
	err = b.retryUnknown(ctx, "", func(rid string) error {
		var werr error
		written, werr = b.prov.SetIngress(ctx, rid, expected)
		return werr
	})
	if errors.Is(err, provider.ErrIngress) && static {
		// Static mode against an ingress it may not write: it compares
		// only, as before, and the mismatch stands.
		b.logf("remote: firewall: the account service will not apply the configured sources [%s] (%s); remote.firewall.mode: %s compares only", joinPrefixes(expected), provider.CodeOf(err), config.RemoteFirewallStatic)
		mismatch.Detail = "compared only: the account service will not apply the sources (" + provider.CodeOf(err) + "); apply them there"
		return mismatch, expected
	}
	if errors.Is(err, provider.ErrIngress) {
		b.logf("remote: firewall: the account service will not apply the sources [%s] (%s); remote.firewall.mode: %s cannot keep the ingress", joinPrefixes(expected), provider.CodeOf(err), config.RemoteFirewallAuto)
		return &FirewallFailure{
			Reason:   FirewallNotWritable,
			What:     "the account service will not apply the ingress sources auto mode writes (" + provider.CodeOf(err) + ")",
			Expected: "sources [" + joinPrefixes(expected) + "]",
			Found:    "sources [" + joinPrefixes(ing.Sources) + "]",
			Detail:   "use remote.firewall.mode: static with the sources the service applies",
		}, expected
	}
	if err != nil {
		b.logf("remote: firewall: writing the ingress sources [%s] failed: %v", joinPrefixes(expected), err)
		mismatch.Detail = "the write of [" + joinPrefixes(expected) + "] failed: " + err.Error()
		return mismatch, expected
	}
	b.logf("remote: firewall: wrote the ingress: sources were [%s], are now [%s]", joinPrefixes(ing.Sources), joinPrefixes(written.Sources))
	fw.exclusive = written.Exclusive
	return checkSources(written.Sources, expected), expected
}

func unreadable(what string, err error) *FirewallFailure {
	return &FirewallFailure{Reason: FirewallUnreadable, What: "could not read " + what + " from the account service", Detail: err.Error()}
}

// checkSources compares the ingress's sources with the expected set.
func checkSources(got, expected []netip.Prefix) *FirewallFailure {
	have := make([]netip.Prefix, 0, len(got))
	seen := map[netip.Prefix]bool{}
	for _, p := range got {
		p = p.Masked()
		if !seen[p] {
			seen[p] = true
			have = append(have, p)
		}
	}
	sortPrefixes(have)
	want := slices.Clone(expected)
	sortPrefixes(want)
	if slices.Equal(have, want) {
		return nil
	}
	return &FirewallFailure{
		Reason:   FirewallSources,
		What:     "the machines' ingress admits the wrong sources",
		Expected: "sources [" + joinPrefixes(want) + "]",
		Found:    "sources [" + joinPrefixes(have) + "]",
	}
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
