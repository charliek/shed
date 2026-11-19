package backend

import (
	"context"
	"runtime"
	"testing"

	"github.com/charliek/shed/internal/config"
)

// typedBackend is a Backend that answers only Type; calling anything else
// panics on the nil embedded interface.
type typedBackend struct {
	Backend
	typ Type
}

func (b typedBackend) Type() Type { return b.typ }

// remoteLike is what the remote backend looks like to a caller: it reports
// its own capabilities, has an asynchronous lifecycle and answers for its
// network profiles.
type remoteLike struct {
	typedBackend
	profiles map[string]bool
}

func (remoteLike) Capabilities() Capabilities { return RemoteCapabilities() }

func (remoteLike) WaitSettled(context.Context, string) (*config.Shed, error) {
	return &config.Shed{Status: config.StatusRunning}, nil
}

func (r remoteLike) SupportsNetworkProfile(profile string) bool { return r.profiles[profile] }

func newRemoteLike() remoteLike {
	return remoteLike{
		typedBackend: typedBackend{typ: TypeRemote},
		profiles:     map[string]bool{config.NetworkProfileOpen: true, config.NetworkProfileIsolated: true},
	}
}

func TestTypeRemoteMatchesConfig(t *testing.T) {
	for _, tt := range []struct {
		typ  Type
		want string
	}{
		{TypeRemote, config.BackendRemote},
		{TypeFirecracker, config.BackendFirecracker},
		{TypeVZ, config.BackendVZ},
	} {
		t.Run(tt.want, func(t *testing.T) {
			if string(tt.typ) != tt.want {
				t.Errorf("backend type %q, config name %q", tt.typ, tt.want)
			}
		})
	}
}

func TestSupportsNetworkProfile(t *testing.T) {
	openOnly := remoteLike{typedBackend: typedBackend{typ: TypeRemote}, profiles: map[string]bool{config.NetworkProfileOpen: true}}
	tests := []struct {
		name    string
		backend Backend
		profile string
		want    bool
	}{
		{"nil backend", nil, config.NetworkProfileOpen, false},
		{"firecracker open", typedBackend{typ: TypeFirecracker}, config.NetworkProfileOpen, true},
		{"firecracker isolated", typedBackend{typ: TypeFirecracker}, config.NetworkProfileIsolated, true},
		{"vz open", typedBackend{typ: TypeVZ}, config.NetworkProfileOpen, true},
		{"vz isolated", typedBackend{typ: TypeVZ}, config.NetworkProfileIsolated, false},
		{"remote open", newRemoteLike(), config.NetworkProfileOpen, true},
		{"remote isolated", newRemoteLike(), config.NetworkProfileIsolated, true},
		{"remote type without the interface", typedBackend{typ: TypeRemote}, config.NetworkProfileIsolated, false},
		{"the interface decides, not the type", openOnly, config.NetworkProfileIsolated, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := SupportsNetworkProfile(tt.backend, tt.profile); got != tt.want {
				t.Errorf("SupportsNetworkProfile = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCapabilitiesOf(t *testing.T) {
	local := Capabilities{Mounts: true, Snapshots: true, Reset: true, ImageManagement: true, EgressProfiles: true, GuestArch: runtime.GOARCH}
	fc := local
	fc.Isolation = IsolationFirewall
	tests := []struct {
		name    string
		backend Backend
		want    Capabilities
	}{
		{"nil backend", nil, Capabilities{}},
		{"firecracker", typedBackend{typ: TypeFirecracker}, fc},
		{"vz", typedBackend{typ: TypeVZ}, local},
		{"remote", newRemoteLike(), Capabilities{AsyncLifecycle: true, Isolation: IsolationTopology}},
		{"guarded remote", NewGuard(newRemoteLike()), Capabilities{AsyncLifecycle: true, Isolation: IsolationTopology}},
		// A Guard around a Guard is seen through all the way down: still the
		// remote's capabilities, never a local guest architecture.
		{"doubly guarded remote", NewGuard(NewGuard(newRemoteLike())), Capabilities{AsyncLifecycle: true, Isolation: IsolationTopology}},
		{"doubly guarded firecracker", NewGuard(NewGuard(typedBackend{typ: TypeFirecracker})), fc},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := CapabilitiesOf(tt.backend); got != tt.want {
				t.Errorf("CapabilitiesOf = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// TestReportedCapabilitiesSeesThroughGuards holds that the reporter is found
// under any number of Guards.
func TestReportedCapabilitiesSeesThroughGuards(t *testing.T) {
	for _, tt := range []struct {
		name    string
		backend Backend
		wantOK  bool
	}{
		{"remote", newRemoteLike(), true},
		{"guarded remote", NewGuard(newRemoteLike()), true},
		{"doubly guarded remote", NewGuard(NewGuard(newRemoteLike())), true},
		{"doubly guarded firecracker", NewGuard(NewGuard(typedBackend{typ: TypeFirecracker})), false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ReportedCapabilities(tt.backend)
			if ok != tt.wantOK {
				t.Fatalf("ReportedCapabilities ok = %v, want %v", ok, tt.wantOK)
			}
			if ok && got != RemoteCapabilities() {
				t.Errorf("ReportedCapabilities = %+v, want %+v", got, RemoteCapabilities())
			}
		})
	}
}

func TestIsAsync(t *testing.T) {
	tests := []struct {
		name    string
		backend Backend
		want    bool
	}{
		{"nil backend", nil, false},
		{"firecracker", typedBackend{typ: TypeFirecracker}, false},
		{"vz", typedBackend{typ: TypeVZ}, false},
		{"remote", newRemoteLike(), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, ok := IsAsync(tt.backend)
			if ok != tt.want {
				t.Fatalf("IsAsync = %v, want %v", ok, tt.want)
			}
			if !ok {
				return
			}
			shed, err := a.WaitSettled(context.Background(), "x")
			if err != nil || shed.Status != config.StatusRunning {
				t.Errorf("WaitSettled = %+v, %v", shed, err)
			}
		})
	}
}
