package config

import (
	"testing"

	"github.com/charliek/shed/internal/remote/provider"
)

// TestRemoteGuestArch: the architecture every configured size shares, or
// none when they mix or one names none.
func TestRemoteGuestArch(t *testing.T) {
	size := func(arch string) RemoteSizeConfig {
		return RemoteSizeConfig{MachineType: "m", CPUs: 2, MemoryMB: 4096, Arch: arch}
	}
	for _, tc := range []struct {
		name  string
		sizes map[string]RemoteSizeConfig
		want  string
	}{
		{"one arm64 size", map[string]RemoteSizeConfig{"a": size(provider.ArchARM64)}, provider.ArchARM64},
		{"all amd64", map[string]RemoteSizeConfig{"a": size(provider.ArchAMD64), "b": size(provider.ArchAMD64)}, provider.ArchAMD64},
		{"mixed", map[string]RemoteSizeConfig{"a": size(provider.ArchAMD64), "b": size(provider.ArchARM64)}, ""},
		{"a size naming none", map[string]RemoteSizeConfig{"a": size(provider.ArchAMD64), "b": size("")}, ""},
		{"no sizes", nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &RemoteConfig{Sizes: tc.sizes}
			if got := c.GuestArch(); got != tc.want {
				t.Errorf("GuestArch = %q, want %q", got, tc.want)
			}
		})
	}
	// A loaded config's defaults (the built-in sizes) are all amd64.
	if got := validRemote().GuestArch(); got != provider.ArchAMD64 {
		t.Errorf("the defaulted config's GuestArch = %q, want %q", got, provider.ArchAMD64)
	}
}
