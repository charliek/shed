package gce

import (
	"context"
	"testing"

	"github.com/charliek/shed/internal/remote/provider"
)

// TestInstanceArch holds where an instance's architecture comes from: the
// boot disk the cloud reports, whatever the configured sizes say now; the
// configured sizes only when the disk names none, and nothing when they
// disagree. The record an adopted shed gets is built from this value.
func TestInstanceArch(t *testing.T) {
	const mt = "e2-standard-2"
	size := func(name, arch string) provider.Size {
		return provider.Size{Name: name, MachineType: mt, CPUs: 2, MemoryMB: 8192, Arch: arch}
	}
	cases := []struct {
		name     string
		diskArch string
		sizes    []provider.Size
		want     string
	}{
		{
			// Made as amd64; the operator has since said arm64 for its type.
			name:     "the disk wins over a changed configuration",
			diskArch: "X86_64",
			sizes:    []provider.Size{size("standard", provider.ArchARM64)},
			want:     provider.ArchAMD64,
		},
		{
			name:     "the disk wins over two disagreeing sizes",
			diskArch: "ARM64",
			sizes:    []provider.Size{size("a", provider.ArchAMD64), size("b", provider.ArchAMD64)},
			want:     provider.ArchARM64,
		},
		{
			name:     "no disk value: the one configured size",
			diskArch: diskArchNone,
			sizes:    []provider.Size{size("standard", provider.ArchARM64)},
			want:     provider.ArchARM64,
		},
		{
			name:     "no disk value: sizes that agree",
			diskArch: diskArchNone,
			sizes:    []provider.Size{size("a", provider.ArchARM64), size("b", provider.ArchARM64)},
			want:     provider.ArchARM64,
		},
		{
			// Either size could have made it; no guess is reported.
			name:     "no disk value: two sizes share the type with different arches",
			diskArch: diskArchNone,
			sizes:    []provider.Size{size("a", provider.ArchAMD64), size("b", provider.ArchARM64)},
			want:     "",
		},
		{
			name:     "no disk value and no matching size",
			diskArch: diskArchNone,
			sizes:    []provider.Size{{Name: "big", MachineType: "e2-standard-8", CPUs: 8, MemoryMB: 32768, Arch: provider.ArchARM64}},
			want:     "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeCompute(t)
			id := provider.Identity{Env: testEnv, ID: "idarch0000000000", Name: "arch"}
			f.seed(&fInst{name: id.InstanceName(), labels: id.Labels(provider.ShedStateReady), machineType: mt, diskArch: tc.diskArch})
			c := testClient(t, f, func(cfg *Config) { cfg.Sizes = tc.sizes })
			ctx := context.Background()

			got, err := c.Get(ctx, makeRef(f.zone, id.InstanceName()))
			if err != nil {
				t.Fatal(err)
			}
			if got.Arch != tc.want {
				t.Errorf("Get: Arch = %q, want %q", got.Arch, tc.want)
			}
			// The size's other facts still come from the first matching size.
			wantCPUs := 0
			for _, s := range tc.sizes {
				if s.MachineType == mt {
					wantCPUs = s.CPUs
					break
				}
			}
			if got.CPUs != wantCPUs {
				t.Errorf("Get: CPUs = %d, want %d", got.CPUs, wantCPUs)
			}

			list, err := c.List(ctx, provider.Selector{Env: testEnv})
			if err != nil {
				t.Fatal(err)
			}
			if len(list) != 1 || list[0].Arch != tc.want {
				t.Errorf("List: %+v, want one instance with Arch %q", list, tc.want)
			}
		})
	}
}
