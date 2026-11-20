package backend

import (
	"context"
	"runtime"

	"github.com/charliek/shed/internal/config"
)

// This file holds the optional interfaces a backend may implement and the
// helpers that read them. Callers ask a question (IsAsync, CapabilitiesOf,
// SupportsNetworkProfile) rather than switching on Backend.Type, so a new
// backend needs no edit at the call sites.

// AsyncLifecycle is implemented by a backend whose create, start, stop and
// delete return as soon as the new status is saved and finish in a job the
// server owns. On such a backend:
//
//   - CreateShed returns a shed in config.StatusStarting, StartShed one in
//     StatusStarting, StopShed one in StatusStopping. The caller must pass a
//     context that outlives the request (context.WithoutCancel).
//   - GetShed and ListSheds read local records and never wait.
type AsyncLifecycle interface {
	// WaitSettled blocks until the named shed has no job in flight, and
	// returns the shed as it then is (running, stopped or error). While it
	// waits it replays the job's progress, from the job's start, into the
	// progress sink on ctx (EmitProgress and friends). A shed with no job
	// returns at once. Cancelling ctx ends the wait, never the job. A shed
	// that is deleted while waited on gives config.ErrShedNotFoundSentinel.
	WaitSettled(ctx context.Context, name string) (*config.Shed, error)
}

// IsAsync returns b as an AsyncLifecycle when it is one. Through a Guard it
// returns a guarded one (which also implements AsyncDeleter) when the backend
// the Guard wraps is asynchronous.
func IsAsync(b Backend) (AsyncLifecycle, bool) {
	if g, ok := b.(*Guard); ok {
		inner, ok := IsAsync(g.inner)
		if !ok {
			return nil, false
		}
		return guardedAsync{g: g, inner: inner}, true
	}
	a, ok := b.(AsyncLifecycle)
	return a, ok
}

// NetworkProfiles is implemented by a backend that decides for itself which
// network profiles it offers. A backend without it is asked through
// SupportsNetworkProfile's built-in rule.
type NetworkProfiles interface {
	// SupportsNetworkProfile reports whether a shed may be created with
	// profile (config.NetworkProfileOpen or NetworkProfileIsolated).
	SupportsNetworkProfile(profile string) bool
}

// SupportsNetworkProfile reports whether b can give a shed the network
// profile. A backend that implements NetworkProfiles answers for itself.
// Otherwise every backend offers "open" and only Firecracker offers more,
// which is the rule the API applied before this interface existed.
func SupportsNetworkProfile(b Backend, profile string) bool {
	if b == nil {
		return false
	}
	b = unwrap(b)
	if np, ok := b.(NetworkProfiles); ok {
		return np.SupportsNetworkProfile(profile)
	}
	return profile == config.NetworkProfileOpen || b.Type() == TypeFirecracker
}

// Isolation kinds (Capabilities.Isolation, /api/info "isolation", and a shed
// record's "isolation").
const (
	// IsolationNone: no isolation is in force for the shed. Only a shed
	// record carries it (IsolationOf); a backend's Capabilities.Isolation is
	// empty, not "none", when it offers only "open".
	IsolationNone = "none"
	// IsolationFirewall: the isolated profile is enforced by firewall rules
	// the server installs on its own host.
	IsolationFirewall = "firewall"
	// IsolationTopology: the isolated profile holds because the network
	// gives a shed no route to another shed or to the server host.
	IsolationTopology = "topology"
)

// IsolationOf is the isolation a shed with the stored network profile has on
// a backend with caps: IsolationNone for "open" (or a stored value written
// before profiles existed), and for "isolated" the backend's kind. It fails
// closed: an isolated shed on a backend with no isolation kind, an isolation
// kind this build does not know, or a profile it does not know is
// IsolationNone, never the profile's name, so a client that requires
// isolation refuses the shed.
func IsolationOf(caps Capabilities, profile string) string {
	if config.EffectiveNetworkProfile(profile) != config.NetworkProfileIsolated {
		return IsolationNone
	}
	switch caps.Isolation {
	case IsolationFirewall, IsolationTopology:
		return caps.Isolation
	}
	return IsolationNone
}

// Capabilities says which optional features a backend offers. The API reads
// it to refuse a request the backend cannot serve (422 or 501
// config.ErrFeatureUnsupported) before the backend is called.
type Capabilities struct {
	// Mounts: host directories in a shed (local_dir, add_dirs, and the
	// server's top-level mounts).
	Mounts bool
	// Snapshots: snapshot create, list, get, delete, and create's
	// from_snapshot.
	Snapshots bool
	// Reset: ResetShed.
	Reset bool
	// ImageManagement: image inspect, tag, pull, push, delete and prune.
	ImageManagement bool
	// EgressProfiles: egress profiles on create and the egress write
	// routes.
	EgressProfiles bool
	// AsyncLifecycle mirrors IsAsync, for callers that only report it.
	AsyncLifecycle bool
	// Isolation is how the isolated network profile is provided: one of the
	// Isolation* constants, or empty when the backend offers only "open".
	Isolation string
	// GuestArch is the architecture every guest of this backend runs
	// ("amd64" or "arm64"): the host's for a local VM backend, empty for a
	// backend whose sheds differ (remote, where it is per shed).
	GuestArch string
}

// CapabilityReporter is implemented by a backend that offers less than every
// feature. Firecracker and VZ do not implement it.
type CapabilityReporter interface {
	Capabilities() Capabilities
}

// ReportedCapabilities returns what b says it offers when it reports its own
// capabilities (CapabilityReporter), seeing through a Guard. ok is false for
// a backend that does not report (Firecracker, VZ).
func ReportedCapabilities(b Backend) (Capabilities, bool) {
	if b == nil {
		return Capabilities{}, false
	}
	r, ok := unwrap(b).(CapabilityReporter)
	if !ok {
		return Capabilities{}, false
	}
	return r.Capabilities(), true
}

// CapabilitiesOf returns what b offers. A backend that does not implement
// CapabilityReporter is a local VM backend and offers every feature (egress
// profiles still depend on the server's egress config and on the backend
// implementing the egress methods, which the API checks as before).
func CapabilitiesOf(b Backend) Capabilities {
	if b == nil {
		return Capabilities{}
	}
	b = unwrap(b)
	if r, ok := b.(CapabilityReporter); ok {
		return r.Capabilities()
	}
	c := Capabilities{
		Mounts:          true,
		Snapshots:       true,
		Reset:           true,
		ImageManagement: true,
		EgressProfiles:  true,
		// A local VM runs on the host's own CPU.
		GuestArch: runtime.GOARCH,
	}
	_, c.AsyncLifecycle = IsAsync(b)
	if SupportsNetworkProfile(b, config.NetworkProfileIsolated) {
		c.Isolation = IsolationFirewall
	}
	return c
}

// RemoteCapabilities is what the remote backend reports: no host mounts,
// snapshots, reset, image management or egress profiles; an asynchronous
// lifecycle; isolation by network topology. No GuestArch: each shed's size
// has its own, which its record carries.
func RemoteCapabilities() Capabilities {
	return Capabilities{AsyncLifecycle: true, Isolation: IsolationTopology}
}
