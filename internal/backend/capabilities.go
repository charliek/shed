package backend

import (
	"context"

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

// IsAsync returns b as an AsyncLifecycle when it is one.
func IsAsync(b Backend) (AsyncLifecycle, bool) {
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
	if np, ok := b.(NetworkProfiles); ok {
		return np.SupportsNetworkProfile(profile)
	}
	return profile == config.NetworkProfileOpen || b.Type() == TypeFirecracker
}

// Isolation kinds (Capabilities.Isolation, /api/info "isolation").
const (
	// IsolationFirewall: the isolated profile is enforced by firewall rules
	// the server installs on its own host.
	IsolationFirewall = "firewall"
	// IsolationTopology: the isolated profile holds because the network
	// gives a shed no route to another shed or to the server host.
	IsolationTopology = "topology"
)

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
}

// CapabilityReporter is implemented by a backend that offers less than every
// feature. Firecracker and VZ do not implement it.
type CapabilityReporter interface {
	Capabilities() Capabilities
}

// CapabilitiesOf returns what b offers. A backend that does not implement
// CapabilityReporter is a local VM backend and offers every feature (egress
// profiles still depend on the server's egress config and on the backend
// implementing the egress methods, which the API checks as before).
func CapabilitiesOf(b Backend) Capabilities {
	if b == nil {
		return Capabilities{}
	}
	if r, ok := b.(CapabilityReporter); ok {
		return r.Capabilities()
	}
	c := Capabilities{
		Mounts:          true,
		Snapshots:       true,
		Reset:           true,
		ImageManagement: true,
		EgressProfiles:  true,
	}
	_, c.AsyncLifecycle = IsAsync(b)
	if SupportsNetworkProfile(b, config.NetworkProfileIsolated) {
		c.Isolation = IsolationFirewall
	}
	return c
}

// RemoteCapabilities is what the remote backend reports: no host mounts,
// snapshots, reset, image management or egress profiles; an asynchronous
// lifecycle; isolation by network topology.
func RemoteCapabilities() Capabilities {
	return Capabilities{AsyncLifecycle: true, Isolation: IsolationTopology}
}
