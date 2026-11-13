package remote

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/charliek/shed/internal/remote/provider"
	"github.com/charliek/shed/internal/remote/state"
	"github.com/charliek/shed/internal/remote/transport"
)

const (
	// guestAttrInterval is the least time between two reads of a guest's
	// published host key. Google allows about ten guest-attribute reads a
	// minute per VM, and a link asks at every connection attempt.
	guestAttrInterval = 6 * time.Second
	// addressTTL is how long a cached address is handed to the link before
	// the cloud is asked again. A live connection never asks; this bounds
	// how stale the address can be when a link reconnects.
	addressTTL = 30 * time.Second
	// addressRetry is the least time between two cloud reads for a shed
	// whose address is not known yet.
	addressRetry = 3 * time.Second
)

// errNoRelearn is returned by Learn for a shed that was once ready: it had a
// pinned host key, so a missing pin is reported and no key is learned again.
var errNoRelearn = errors.New("the shed was ready and has no pinned host key; a host key is learned only once")

// hostKeys is one shed's transport.HostKeyStore and address source. The pin
// is instance metadata (provider.MetadataHostKeyPin), copied into the shed's
// record; learning compares the key sshd presents with the one the guest
// published through the provider.
//
// A link asks for the address and the pin before every connection attempt,
// up to twice a second while a guest boots, so everything here answers from
// memory when it can.
type hostKeys struct {
	b    *Backend
	name string
	id   string

	mu sync.Mutex
	// pinRead is true once the cloud has been asked for the pin.
	pinRead bool
	// published and publishedAt cache the guest attribute.
	published   string
	publishedAt time.Time
	publishErr  error
	// addrTried is the last cloud read for the address.
	addrTried time.Time
	addrErr   error
}

var _ transport.HostKeyStore = (*hostKeys)(nil)

// sameHostKey compares two keys in authorized_keys form, ignoring comments
// and spacing.
func sameHostKey(a, b string) bool {
	fa, fb := strings.Fields(a), strings.Fields(b)
	return len(fa) >= 2 && len(fb) >= 2 && fa[0] == fb[0] && fa[1] == fb[1]
}

func (h *hostKeys) record() (*state.Record, error) {
	rec := h.b.snapshot(h.name, h.id)
	if rec == nil {
		return nil, fmt.Errorf("shed %s no longer exists", h.name)
	}
	if rec.Ref == "" {
		return nil, fmt.Errorf("shed %s has no instance yet", h.name)
	}
	return rec, nil
}

// addr is the link's Target.Addr.
func (h *hostKeys) addr(ctx context.Context) (string, error) {
	rec, err := h.record()
	if err != nil {
		return "", err
	}
	now := time.Now()
	if rec.Address != "" && now.Sub(rec.AddressAt) < addressTTL {
		return rec.Address, nil
	}
	h.mu.Lock()
	if now.Sub(h.addrTried) < addressRetry {
		cached, cachedErr := rec.Address, h.addrErr
		h.mu.Unlock()
		if cached != "" {
			return cached, nil
		}
		if cachedErr == nil {
			cachedErr = errors.New("the instance has no address yet")
		}
		return "", cachedErr
	}
	h.addrTried = now
	h.mu.Unlock()

	inst, err := h.b.prov.Get(ctx, rec.Ref)
	h.mu.Lock()
	h.addrErr = err
	h.mu.Unlock()
	if err != nil {
		return "", err
	}
	h.b.update(h.name, h.id, func(r *state.Record) {
		r.Address = inst.Address
		r.AddressAt = now
	})
	if inst.Address == "" {
		return "", fmt.Errorf("instance of %s has no address (state %s)", h.name, inst.State)
	}
	return inst.Address, nil
}

// Pin returns the pinned key, or "" when the shed has none. The cloud is
// asked at most once per link; after that the record's copy answers.
func (h *hostKeys) Pin(ctx context.Context) (string, error) {
	rec, err := h.record()
	if err != nil {
		return "", err
	}
	if rec.HostKeyPin != "" {
		return rec.HostKeyPin, nil
	}
	h.mu.Lock()
	read := h.pinRead
	h.mu.Unlock()
	if read {
		return "", nil
	}
	inst, err := h.b.prov.Get(ctx, rec.Ref)
	if err != nil {
		return "", err
	}
	h.mu.Lock()
	h.pinRead = true
	h.mu.Unlock()
	pin := inst.HostKeyPin()
	if pin != "" {
		h.b.update(h.name, h.id, func(r *state.Record) { r.HostKeyPin = pin })
	}
	return pin, nil
}

// Learn accepts the key the guest's sshd presented only when it equals the
// key the guest published through the provider, then writes the pin and
// reads it back. A shed that has been ready is never learned again.
func (h *hostKeys) Learn(ctx context.Context, presented string) error {
	rec, err := h.record()
	if err != nil {
		return err
	}
	if rec.Ready {
		return errNoRelearn
	}
	if rec.HostKeyPin != "" {
		if sameHostKey(rec.HostKeyPin, presented) {
			return nil
		}
		return errors.New("the presented key differs from the pinned key")
	}
	reader, ok := h.b.prov.(provider.HostKeyReader)
	if !ok {
		return fmt.Errorf("provider %s cannot report a guest's published host key", h.b.prov.Name())
	}

	h.mu.Lock()
	if h.publishedAt.IsZero() || time.Since(h.publishedAt) >= guestAttrInterval {
		h.published, h.publishErr = reader.GuestHostKey(ctx, rec.Ref)
		h.publishedAt = time.Now()
	}
	published, publishErr := h.published, h.publishErr
	h.mu.Unlock()
	if publishErr != nil {
		return fmt.Errorf("reading the guest's published host key: %w", publishErr)
	}
	if !sameHostKey(published, presented) {
		return errors.New("the presented key is not the key the guest published")
	}

	fields := strings.Fields(presented)
	pin := fields[0] + " " + fields[1]
	if err := h.b.verify(ctx, rec.Ref, h.id); err != nil {
		return err
	}
	if err := h.b.prov.SetMetadata(ctx, rec.Ref, map[string]string{provider.MetadataHostKeyPin: pin}); err != nil {
		return fmt.Errorf("writing the host key pin: %w", err)
	}
	inst, err := h.b.prov.Get(ctx, rec.Ref)
	if err != nil {
		return fmt.Errorf("reading the host key pin back: %w", err)
	}
	if !sameHostKey(inst.HostKeyPin(), pin) {
		return errors.New("the host key pin read back differs from the one written")
	}
	if !h.b.update(h.name, h.id, func(r *state.Record) { r.HostKeyPin = pin }) {
		return fmt.Errorf("shed %s no longer exists", h.name)
	}
	return nil
}
