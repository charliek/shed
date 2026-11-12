//go:build linux

package firecracker

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/netpolicy"
	"github.com/charliek/shed/internal/stallwatch"
	"github.com/vishvananda/netlink"
)

// networkPolicyCheckInterval is how often the presence check runs while the
// server's tables are needed. Something that flushes the ruleset (an
// `nft flush ruleset`, a firewall reload) leaves guarded sheds unguarded until
// the next check reapplies them.
var networkPolicyCheckInterval = 30 * time.Second

// netpolState is the Client's network-policy bookkeeping besides the
// Manager's own desired state.
type netpolState struct {
	mu sync.Mutex
	// tapsDown records that the guarded sheds' TAPs were set down because
	// the tables could not be installed; the next successful check sets
	// them up again.
	tapsDown bool
}

// newNetworkPolicy builds the server's network-policy Manager.
func newNetworkPolicy(cfg *config.FirecrackerConfig, serverCfg *config.ServerConfig, gateway string) *netpolicy.Manager {
	def, err := netpolicy.Parse(serverCfg.NetworkDefaultProfile())
	if err != nil {
		// Validated at load; fail closed if it somehow is not.
		def = netpolicy.Isolated
	}
	return netpolicy.NewManager(netpolicy.Config{
		ServerName: serverCfg.Name,
		Bridge:     cfg.BridgeName,
		Gateway:    gateway,
		AllowCIDRs: serverCfg.NetworkAllowCIDRs(),
	}, netpolicy.ExecRunner{Bin: "nft"}, def)
}

// resolveNetworkProfile turns a create request's network (the API always
// writes the effective profile) into a profile; "" (a non-API caller) is the
// server's network.default_profile, never silently open.
func (c *Client) resolveNetworkProfile(requested string) (netpolicy.Profile, error) {
	if requested == "" {
		requested = c.serverCfg.NetworkDefaultProfile()
	}
	p, err := netpolicy.Parse(requested)
	if err != nil {
		return "", fmt.Errorf("%w: %v", config.ErrInvalidShedRequestSentinel, err)
	}
	return p, nil
}

// networkPolicyShed is a shed's entry for the rule set (ports: its egress
// port; 9P ports are added as its servers open).
func networkPolicyShed(meta *Metadata, profile netpolicy.Profile) netpolicy.Shed {
	return netpolicy.Shed{
		Name:       meta.Name,
		TAP:        meta.TAPDevice,
		MAC:        generateMACAddress(meta.CID),
		IP:         meta.IPAddress,
		Profile:    profile,
		EgressPort: egressPort(meta.EgressPort),
	}
}

func egressPort(p int) uint16 {
	if p <= 0 || p > 65535 {
		return 0
	}
	return uint16(p)
}

// networkPolicySheds reads every shed's metadata and returns the guarded ones
// (running or stopped: a stopped shed keeps its TAP, so it keeps its entry).
func (c *Client) networkPolicySheds() []netpolicy.Shed {
	stop := stallwatch.Watch("listing sheds", c.cfg.InstanceDir)
	names, err := ListInstances(c.cfg.InstanceDir)
	stop()
	if err != nil {
		log.Printf("ERROR: network policy: cannot list sheds to rebuild the rules: %v", err)
		return nil
	}
	var out []netpolicy.Shed
	for _, name := range names {
		meta, err := LoadMetadata(c.cfg.InstanceDir, name)
		if err != nil {
			if !errors.Is(err, ErrInstanceNotFound) {
				// Its TAP and profile are unknown, so it gets no entry: if
				// it is a running isolated shed, it is unguarded until its
				// metadata is readable and the server restarts.
				log.Printf("ERROR: network policy: cannot read shed %q's metadata (%v); it has no network-policy entry", name, err)
			}
			continue
		}
		p, err := netpolicy.Parse(meta.NetworkProfile)
		if err != nil {
			// An unknown stored profile fails closed: guard it.
			log.Printf("Warning: network policy: shed %q has unknown network profile %q; treating it as isolated", name, meta.NetworkProfile)
			p = netpolicy.Isolated
		}
		if p.Guarded() {
			out = append(out, networkPolicyShed(meta, p))
		}
	}
	return out
}

// StartNetworkPolicy installs this server's network-policy tables from the
// sheds' metadata, before the listeners start and before the resume walk, and
// starts the presence check, which runs until ctx ends. A server that needs no
// tables (no guarded shed and an open default) deletes stale ones of its own
// when nft is there, and makes no nft call at all when it is not. When the
// tables cannot be installed, every guarded shed's TAP is set down (it is
// cut off rather than left unguarded) and the server keeps serving; a later
// successful check sets them up again. When the tables are installed, every
// guarded shed's TAP is set up: a previous process may have set them down and
// exited before its own check restored them.
func (c *Client) StartNetworkPolicy(ctx context.Context) {
	if c.netpol == nil {
		return
	}
	sheds := c.networkPolicySheds()
	if err := c.netpol.Rebuild(ctx, sheds); err != nil {
		log.Printf("ERROR: network policy: could not install tables %s for %d guarded shed(s): %v; setting their TAPs down until the rules apply", c.netpol.TableName(), len(sheds), err)
		c.setGuardedTAPs(false)
	} else if c.netpol.Needed() {
		log.Printf("Network policy: tables bridge/inet %s installed (%d guarded shed(s), default profile %s)", c.netpol.TableName(), len(sheds), c.serverCfg.NetworkDefaultProfile())
		c.setGuardedTAPs(true)
	}
	go c.networkPolicyLoop(ctx, networkPolicyCheckInterval)
}

func (c *Client) networkPolicyLoop(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.checkNetworkPolicy(ctx)
		}
	}
}

// checkNetworkPolicy is one presence check: while an earlier apply failed
// (the kernel may still hold an older, more permissive rule set even though
// both tables exist), or the tables are needed and either is missing, log
// loudly and reapply; when that fails, set the guarded TAPs down; once the
// desired tables are in place, set them back up.
func (c *Client) checkNetworkPolicy(ctx context.Context) {
	if c.netpol == nil {
		return
	}
	pending := c.netpol.Pending()
	if !pending && !c.netpol.Needed() {
		return
	}
	var err error
	if pending {
		log.Printf("ERROR: network policy: an earlier apply of tables %s failed; reapplying", c.netpol.TableName())
		if err = c.netpol.Reapply(ctx); err == nil {
			log.Printf("Network policy: tables %s reapplied", c.netpol.TableName())
		}
	} else {
		var present bool
		present, err = c.netpol.Present(ctx)
		if err == nil && !present {
			log.Printf("ERROR: network policy: table bridge/inet %s is missing (flushed by something else?); reapplying", c.netpol.TableName())
			err = c.netpol.Reapply(ctx)
			if err == nil {
				log.Printf("Network policy: tables %s reapplied", c.netpol.TableName())
			}
		}
	}
	if err != nil {
		log.Printf("ERROR: network policy: cannot ensure tables %s: %v; setting guarded sheds' TAPs down", c.netpol.TableName(), err)
		c.setGuardedTAPs(false)
		return
	}
	c.netpolSt.mu.Lock()
	down := c.netpolSt.tapsDown
	c.netpolSt.mu.Unlock()
	if down {
		log.Printf("Network policy: tables %s are in place again; setting guarded sheds' TAPs up", c.netpol.TableName())
		c.setGuardedTAPs(true)
	}
}

// setGuardedTAPs sets every guarded shed's TAP up or down. It walks the
// Manager's entries under its lock (EachGuarded), so a shed deleted meanwhile
// cannot have its TAP name reused by another shed before the link change.
// tapsDown stays set after a raise that failed for any TAP (other than one
// that does not exist), so the next check retries it.
func (c *Client) setGuardedTAPs(up bool) {
	failed := false
	c.netpol.EachGuarded(func(s netpolicy.Shed) {
		var notFound netlink.LinkNotFoundError
		if err := c.linkState(s.TAP, up); err != nil && !errors.As(err, &notFound) {
			log.Printf("Warning: network policy: set %s (shed %s) up=%v: %v", s.TAP, s.Name, up, err)
			failed = true
		}
	})
	c.netpolSt.mu.Lock()
	c.netpolSt.tapsDown = !up || failed
	c.netpolSt.mu.Unlock()
}

// linkState sets an interface up or down (netlink; replaced in tests).
func (c *Client) linkState(name string, up bool) error {
	if c.setLinkState != nil {
		return c.setLinkState(name, up)
	}
	link, err := netlink.LinkByName(name)
	if err != nil {
		return err
	}
	if up {
		return netlink.LinkSetUp(link)
	}
	return netlink.LinkSetDown(link)
}

// guardNewShed puts a new guarded shed's entry in place (create, right after
// its TAP exists and before its VM does) and registers its removal on the
// create's cleanup. A failure fails the create. Open sheds do nothing.
func (c *Client) guardNewShed(ctx context.Context, name string, profile netpolicy.Profile, tap string, cid uint32, ip string, cleanup *backend.Cleanup) error {
	if !profile.Guarded() || c.netpol == nil {
		return nil
	}
	cleanup.Register("remove network policy entry", func() error {
		return c.netpol.Remove(context.Background(), name)
	})
	entry := netpolicy.Shed{Name: name, TAP: tap, MAC: generateMACAddress(cid), IP: ip, Profile: profile}
	if err := c.netpol.Upsert(ctx, entry); err != nil {
		return fmt.Errorf("network profile %s for %s: %w", profile, name, err)
	}
	return nil
}

// ensureGuardedForStart puts a guarded shed's entry in place before its VM
// starts (refusing the start when the rules cannot be applied) and makes sure
// its TAP is up (a failed check may have set it down). Open sheds do nothing.
func (c *Client) ensureGuardedForStart(ctx context.Context, meta *Metadata) error {
	if c.netpol == nil {
		return nil
	}
	p, err := netpolicy.Parse(meta.NetworkProfile)
	if err != nil {
		p = netpolicy.Isolated // unknown stored profile: fail closed
	}
	if !p.Guarded() {
		return nil
	}
	if err := c.netpol.Upsert(ctx, networkPolicyShed(meta, p)); err != nil {
		return fmt.Errorf("start %s: %w", meta.Name, err)
	}
	if err := c.linkState(meta.TAPDevice, true); err != nil {
		log.Printf("Warning: network policy: set %s (shed %s) up: %v", meta.TAPDevice, meta.Name, err)
	}
	return nil
}

// p9Ports returns the gateway ports of a shed's registered 9P servers.
func (c *Client) p9Ports(name string) []uint16 {
	c.mu.Lock()
	defer c.mu.Unlock()
	var ports []uint16
	for _, srv := range c.p9Servers[name] {
		_, ps, err := net.SplitHostPort(srv.Addr())
		if err != nil {
			continue
		}
		if p, err := strconv.ParseUint(ps, 10, 16); err == nil {
			ports = append(ports, uint16(p))
		}
	}
	return ports
}

// syncP9Ports puts a guarded shed's current 9P ports in its host_allow entry.
// Open sheds (and a Client with no network policy) do nothing.
func (c *Client) syncP9Ports(name string) error {
	if c.netpol == nil || !c.netpol.IsGuarded(name) {
		return nil
	}
	return c.netpol.SetP9Ports(context.Background(), name, c.p9Ports(name))
}

// syncEgressPort puts a guarded shed's egress-proxy port in its host_allow
// entry (0: none). Open sheds do nothing; a failure is logged (the guest
// loses the proxy, which fails closed).
func (c *Client) syncEgressPort(name string, port int) {
	if c.netpol == nil {
		return
	}
	if err := c.netpol.SetEgressPort(context.Background(), name, egressPort(port)); err != nil {
		log.Printf("Warning: network policy: egress port for %s: %v", name, err)
	}
}

// removeNetworkPolicy drops a deleted shed's entry. A failure is logged and
// does not block the delete: the next successful apply drops the element.
func (c *Client) removeNetworkPolicy(ctx context.Context, name string) {
	if c.netpol == nil {
		return
	}
	if err := c.netpol.Remove(ctx, name); err != nil {
		log.Printf("Warning: network policy: remove %s: %v (the next successful apply drops it)", name, err)
	}
}
