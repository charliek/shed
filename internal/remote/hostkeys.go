package remote

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/remote/provider"
	"github.com/charliek/shed/internal/remote/provider/service"
	"github.com/charliek/shed/internal/remote/state"
	"github.com/charliek/shed/internal/remote/transport"
)

// accessRetry is the least time between two reads of a machine whose access
// (or its attested host key) is not known yet, or whose last dial failed.
const accessRetry = 3 * time.Second

// errNoAccess is the link's error for a machine this server cannot or may
// not dial: it offers only access kinds this build does not know, or its
// host is outside remote.access.allow. The shed is marked unreachable with
// reason access.
var errNoAccess = errors.New("the machine offers no access this server can use")

// accessRule is remote.access.allow: where a machine's access host may be.
type accessRule struct {
	allow []netip.Prefix
	// testDrivers lets a host be in a refused class (the validation of the
	// provider's reply already refused those otherwise).
	testDrivers bool
}

func newAccessRule(cfg *config.RemoteConfig) (accessRule, error) {
	allow, err := cfg.Access.Prefixes()
	if err != nil {
		return accessRule{}, fmt.Errorf("remote: %w", err)
	}
	return accessRule{allow: allow, testDrivers: cfg.AllowTestDrivers}, nil
}

// allows reports whether the server may dial host. With no list, every
// unicast address outside the refused classes (loopback, link-local,
// unspecified, multicast) is allowed, and with test drivers those too.
func (a accessRule) allows(host string) bool {
	addr, err := netip.ParseAddr(host)
	if err != nil || addr.Zone() != "" {
		return false
	}
	addr = addr.Unmap()
	if len(a.allow) > 0 {
		for _, p := range a.allow {
			if p.Contains(addr) {
				return true
			}
		}
		return false
	}
	if a.testDrivers {
		return true
	}
	return service.RefusedClass(addr) == ""
}

// chooseAccess picks the option a link dials: the first the machine offers
// of a kind this build supports, checked against remote.access.allow and,
// once the shed has a pin, against it. It returns "" with a nil error when
// the machine offers none yet (it is booting, or its key is not known).
func (b *Backend) chooseAccess(m *provider.Instance, pin string) (addr, hostKey string, err error) {
	if m == nil {
		return "", "", nil
	}
	if len(m.Access) == 0 {
		if m.IgnoredAccess > 0 {
			return "", "", fmt.Errorf("%w: it offers %d option(s), none of a kind this server supports (%s)", errNoAccess, m.IgnoredAccess, provider.AccessSSH)
		}
		return "", "", nil
	}
	opt := m.Access[0]
	if !b.access.allows(opt.Host) {
		return "", "", fmt.Errorf("%w: its host %s is outside remote.access.allow; not dialled", errNoAccess, opt.Host)
	}
	if pin != "" && opt.HostKey != "" && !sameHostKey(pin, opt.HostKey) {
		// The service attests another key than the one pinned at first
		// contact: refused, never followed.
		return "", "", fmt.Errorf("the account service attests a host key other than the one pinned at first contact: %w", transport.ErrHostKeyMismatch)
	}
	return net.JoinHostPort(opt.Host, strconv.Itoa(opt.Port)), opt.HostKey, nil
}

// hostKeys is one shed's transport.HostKeyStore and address source. The
// address and the attested host key are the record's machine's first access
// option; the pin is the record's, written at first contact.
//
// A link asks for the address before every connection attempt, up to twice
// a second while a guest boots, so everything here answers from the record
// when it can. The address the record holds (Record.Machine's access) is
// tried first, whatever happened before: the first connection, and a
// reconnect after a connection that worked and dropped. Only after a dial
// to it failed (no Connected since the last answer) is the machine read
// again, at most every HostKeyPollInterval, so a service that is down does
// not stand between a reachable guest and its server.
type hostKeys struct {
	b    *Backend
	name string
	id   string

	mu sync.Mutex
	// tried is set when an address was given and cleared by connected: set
	// at the next request, the last address's dial failed.
	tried bool
	// readAt is the last read of the machine, readErr its error.
	readAt  time.Time
	readErr error
}

// connected is the link's Target.Connected: the last address worked.
func (h *hostKeys) connected() {
	h.mu.Lock()
	h.tried = false
	h.mu.Unlock()
}

var _ transport.HostKeyStore = (*hostKeys)(nil)

// pinOf is a key in authorized_keys form as a pin: its type and blob,
// without the comment or extra spacing. It is "" when the key has no blob.
func pinOf(key string) string {
	f := strings.Fields(key)
	if len(f) < 2 {
		return ""
	}
	return f[0] + " " + f[1]
}

// sameHostKey compares two keys in authorized_keys form, ignoring comments
// and spacing.
func sameHostKey(a, b string) bool {
	p := pinOf(a)
	return p != "" && p == pinOf(b)
}

func (h *hostKeys) record() (*state.Record, error) {
	rec := h.b.snapshot(h.name, h.id)
	if rec == nil {
		return nil, fmt.Errorf("shed %s no longer exists", h.name)
	}
	if rec.Ref == "" {
		return nil, fmt.Errorf("shed %s has no machine yet", h.name)
	}
	return rec, nil
}

// refresh reads the machine again, at most every HostKeyPollInterval, and
// keeps it in the record. It returns the record as it then is.
func (h *hostKeys) refresh(ctx context.Context, rec *state.Record) (*state.Record, error) {
	h.mu.Lock()
	if !h.readAt.IsZero() && time.Since(h.readAt) < h.b.opts.HostKeyPollInterval {
		err := h.readErr
		h.mu.Unlock()
		return rec, err
	}
	h.readAt = time.Now()
	h.mu.Unlock()

	inst, err := h.b.prov.Get(ctx, rec.Ref)
	if errors.Is(err, provider.ErrNotActed) {
		// Only this read never reached the service, or was not acted on
		// (a 429, a second 401): it says nothing about the machine, and
		// must not pass for a create or start the service did not act on
		// once it is the error of a dial.
		err = &provider.Error{Kind: provider.ErrUnavailable, Op: "get", Detail: "the service could not be reached to read the machine's access"}
	}
	h.mu.Lock()
	h.readErr = err
	h.mu.Unlock()
	if err != nil {
		return rec, err
	}
	h.b.update(h.name, h.id, func(r *state.Record) { h.b.noteMachineLocked(r, inst) })
	if cur := h.b.snapshot(h.name, h.id); cur != nil {
		return cur, nil
	}
	return rec, nil
}

// addr is the link's Target.Addr.
func (h *hostKeys) addr(ctx context.Context) (string, error) {
	rec, err := h.record()
	if err != nil {
		return "", err
	}
	h.mu.Lock()
	failed := h.tried
	h.tried = true
	h.mu.Unlock()
	addr, _, err := h.b.chooseAccess(rec.Machine, rec.HostKeyPin)
	if err != nil {
		return "", err
	}
	if !failed && addr != "" {
		return addr, nil
	}
	// The last dial failed, or there is nothing to dial yet: ask the
	// service where the machine is now.
	rec, rerr := h.refresh(ctx, rec)
	addr, _, err = h.b.chooseAccess(rec.Machine, rec.HostKeyPin)
	switch {
	case err != nil:
		return "", err
	case addr != "":
		return addr, nil
	case rerr != nil:
		return "", rerr
	}
	state := provider.StateUnknown
	if rec.Machine != nil {
		state = rec.Machine.State
	}
	return "", fmt.Errorf("the machine of %s has no access yet (state %s)", h.name, state)
}

// Pin returns the pinned key, or "" when the shed has none.
func (h *hostKeys) Pin(ctx context.Context) (string, error) {
	rec, err := h.record()
	if err != nil {
		return "", err
	}
	return rec.HostKeyPin, nil
}

// Learn accepts the key the guest's sshd presented only when it equals the
// key the account service attests for the machine (the first key its guest
// published, kept write-once), then pins it in the record. It is never
// trust on first use: with no attested key nothing is learned. So a shed
// with no pin learns one at its first contact whenever that is: a create's,
// or the first start or probe of a shed adopted from its machine (whose pin
// is not taken from the attestation alone).
func (h *hostKeys) Learn(ctx context.Context, presented string) error {
	rec, err := h.record()
	if err != nil {
		return err
	}
	if rec.HostKeyPin != "" {
		if sameHostKey(rec.HostKeyPin, presented) {
			return nil
		}
		return errors.New("the presented key differs from the pinned key")
	}
	_, attested, err := h.b.chooseAccess(rec.Machine, "")
	if err == nil && attested == "" {
		rec, _ = h.refresh(ctx, rec)
		_, attested, err = h.b.chooseAccess(rec.Machine, "")
	}
	if err != nil {
		return err
	}
	if attested == "" {
		return errors.New("the account service attests no host key for the machine yet")
	}
	if !sameHostKey(attested, presented) {
		// The service attests a key for this machine, and the guest
		// presents another: not a key still to come, a mismatch.
		return fmt.Errorf("the presented key is not the key the account service attests: %w", transport.ErrHostKeyMismatch)
	}
	pin := pinOf(presented)
	if !h.b.update(h.name, h.id, func(r *state.Record) { r.HostKeyPin = pin }) {
		return fmt.Errorf("shed %s no longer exists", h.name)
	}
	return nil
}
