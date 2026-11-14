package remote

import (
	"fmt"
	"time"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/remote/provider"
)

// Limits are counted from the records under Backend.mu. A create or start
// saves its record ("starting") inside the same critical section that checked
// the limit, so the count always includes every job in flight and two calls
// can never both take the last slot.

// usesRunSlot reports whether a shed in this status holds a running VM, or
// one on its way up or down.
func usesRunSlot(status string) bool {
	switch status {
	case config.StatusStarting, config.StatusRunning, config.StatusStopping:
		return true
	}
	return false
}

// countsLocked returns how many sheds exist and how many hold a run slot.
//
// A shed being deleted is not counted: its record is already gone, and its
// VM normally follows within a minute. But a VM whose delete was acknowledged
// longer ago than DeleteRetryAge and that still exists is a destroy that is
// not working, and it does hold a run slot: otherwise max_running would cap
// nothing while destroys fail, and every create-and-delete would leave one
// more VM running.
func (b *Backend) countsLocked() (sheds, running int) {
	for _, r := range b.records {
		sheds++
		if usesRunSlot(r.Shed.Status) {
			running++
		}
	}
	return sheds, running + b.stuckDeletesLocked()
}

// stuckDeletesLocked counts the delete markers older than DeleteRetryAge.
func (b *Backend) stuckDeletesLocked() int {
	n := 0
	now := b.now()
	for _, at := range b.pending {
		if now.Sub(at) >= b.opts.DeleteRetryAge {
			n++
		}
	}
	return n
}

// limitError is the refusal for a reached server limit. It matches
// provider.ErrLimit and the API's 503 SERVER_LIMIT_REACHED sentinel.
func limitError(what string, limit, stuck int) error {
	if stuck > 0 {
		return fmt.Errorf("%w: %w: %s (%d) reached; %d of them are VMs of deleted sheds that could not be destroyed yet (see the server log)",
			config.ErrServerLimitReachedSentinel, provider.ErrLimit, what, limit, stuck)
	}
	return fmt.Errorf("%w: %w: %s (%d) reached", config.ErrServerLimitReachedSentinel, provider.ErrLimit, what, limit)
}

// checkCreateLocked refuses a create that would pass max_sheds or
// max_running.
func (b *Backend) checkCreateLocked() error {
	sheds, running := b.countsLocked()
	if limit := b.cfg.Limits.MaxSheds; limit > 0 && sheds >= limit {
		return limitError("remote.limits.max_sheds", limit, 0)
	}
	if limit := b.cfg.Limits.MaxRunning; limit > 0 && running >= limit {
		return limitError("remote.limits.max_running", limit, b.stuckDeletesLocked())
	}
	return nil
}

// checkStartLocked refuses a start that would pass max_running.
func (b *Backend) checkStartLocked() error {
	_, running := b.countsLocked()
	if limit := b.cfg.Limits.MaxRunning; limit > 0 && running >= limit {
		return limitError("remote.limits.max_running", limit, b.stuckDeletesLocked())
	}
	return nil
}

// runLimit is the run limit a shed of this owner gets at each start: the
// larger of remote.max_run_duration and the owning client's max_age.
func (b *Backend) runLimit(owner string) time.Duration {
	var maxAge time.Duration
	if owner != "" && b.opts.Server != nil && b.opts.Server.Auth != nil {
		if client, ok := b.opts.Server.Auth.Clients[owner]; ok {
			maxAge = client.Policy.MaxAge.Duration()
		}
	}
	return b.cfg.RunLimit(maxAge)
}
