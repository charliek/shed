package backend

import (
	"context"
	"errors"
	"runtime"
	"strings"
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

func TestComputeCatalogResolveSize(t *testing.T) {
	cat := ComputeCatalog{
		Sizes: []CatalogSize{
			{Name: "standard", CPUs: 2, MemoryMB: 8192, Arch: "amd64"},
			{Name: "large", CPUs: 8, MemoryMB: 32768, Arch: "amd64"},
		},
		DefaultSize: "standard",
	}
	tests := []struct {
		name     string
		cpus, mb int
		want     string
		wantErr  bool
	}{
		{"nothing asked gives the default", 0, 0, "standard", false},
		{"exact standard", 2, 8192, "standard", false},
		{"smaller rounds up to standard", 1, 1024, "standard", false},
		{"cpus only", 4, 0, "large", false},
		{"memory only", 0, 16384, "large", false},
		{"exact large", 8, 32768, "large", false},
		{"too many cpus", 16, 0, "", true},
		{"too much memory", 2, 65536, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := cat.ResolveSize(tt.cpus, tt.mb)
			if tt.wantErr {
				if !errors.Is(err, config.ErrSizeUnavailableSentinel) {
					t.Fatalf("err = %v, want ErrSizeUnavailableSentinel", err)
				}
				if !strings.Contains(err.Error(), "large") {
					t.Errorf("error should name the sizes: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveSize: %v", err)
			}
			if got.Name != tt.want {
				t.Errorf("size = %q, want %q", got.Name, tt.want)
			}
		})
	}
	t.Run("unknown default", func(t *testing.T) {
		c := cat
		c.DefaultSize = "huge"
		if _, err := c.ResolveSize(0, 0); !errors.Is(err, config.ErrSizeUnavailableSentinel) {
			t.Errorf("err = %v, want ErrSizeUnavailableSentinel", err)
		}
	})
	t.Run("negative", func(t *testing.T) {
		if _, err := cat.ResolveSize(-1, 0); !errors.Is(err, config.ErrInvalidShedRequestSentinel) {
			t.Errorf("err = %v, want ErrInvalidShedRequestSentinel", err)
		}
	})
}
